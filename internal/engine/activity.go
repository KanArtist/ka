package engine

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/internal/gitx"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// EvCommit is a history entry for a commit made outside ka.
const EvCommit = "commit"

// ActivityIn selects a page of history.
type ActivityIn struct {
	Limit  int    `json:"limit,omitempty"`  // default 100, at most 500
	Before string `json:"before,omitempty"` // RFC 3339: only older entries
	Ticket string `json:"ticket,omitempty"` // only this ticket's history
}

// ActivityOut is a page of history, newest first.
type ActivityOut struct {
	Events []Event `json:"events"`
	// More is true when older entries exist; pass the last event's at as before.
	More bool `json:"more"`
}

// Activity reads recent history from git across the workspace's stores and
// expresses it as the same events the live stream sends (without sequence
// numbers), so a client renders both alike. ka's own commits are decoded
// from their Ka-Op trailers; other commits appear as "commit" entries.
func (s *Service) Activity(ctx context.Context, in ActivityIn) (*ActivityOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	var all []Event
	stores := s.WS.Stores
	var paths []string
	if in.Ticket != "" {
		s.ix.mu.RLock()
		r, err := s.rec(in.Ticket)
		s.ix.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		stores = []*Store{r.Store}
		paths = []string{r.Store.repoPath(r.Dir)}
	}
	for _, st := range stores {
		cs, err := st.git.Recent(ctx, limit+1, in.Before, paths...)
		if err != nil {
			continue // an empty repository has no HEAD yet
		}
		for _, c := range cs {
			all = append(all, s.commitEvents(st, c)...)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At > all[j].At })
	out := &ActivityOut{Events: []Event{}}
	if len(all) > limit {
		all, out.More = all[:limit], true
	}
	out.Events = append(out.Events, all...)
	return out, nil
}

func (s *Service) commitEvents(st *Store, c gitx.Commit) []Event {
	at := c.Date
	if t, err := time.Parse(time.RFC3339, c.Date); err == nil {
		at = t.UTC().Format("2006-01-02T15:04:05Z")
	}
	author, _, _ := strings.Cut(c.Author, " <")
	base := Event{At: at, Store: st.Name, Commit: short(c.SHA), Actor: author}
	if a := c.Trailers["Ka-Actor"]; a != "" {
		base.Actor = a
	}
	raw := c.Trailers["Ka-Op"]
	if raw == "" {
		e := base
		e.Type, e.Text = EvCommit, c.Subject
		return []Event{e}
	}
	o, err := DecodeOp(raw)
	if err != nil {
		e := base
		e.Type, e.Text = EvCommit, c.Subject
		return []Event{e}
	}
	base.Ticket = o.ID
	_, text, _ := strings.Cut(c.Subject, ": ")
	base.Text = text

	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	v := view{ix: s.ix, now: s.Now()}
	var sum *Summary
	ref := Ref{ID: o.ID}
	if r := s.ix.tickets[o.ID]; r != nil {
		x := s.summary(v, r)
		sum = &x
		base.Project = r.Project.Key
		ref.Title, ref.Status = r.T.Title, r.T.Status
	} else if pi, ok := s.ix.projectFor(o.ID); ok {
		base.Project = pi.Key
	}
	if sum == nil {
		sum = &Summary{ID: o.ID, Project: base.Project}
		if o.New != nil {
			sum.Title, sum.Status = o.New.Title, o.New.Status
		}
	}

	e := base
	switch o.Op {
	case "create":
		e.Type = EvTicketCreated
		if o.New != nil {
			// Show the ticket as it was created.
			cs := *sum
			cs.Title, cs.Status, cs.Priority, cs.Kind = o.New.Title, o.New.Status, o.New.Priority, o.New.Kind
			sum = &cs
		}
		e.Data = TicketEvent{Ticket: *sum}
	case "transition":
		e.Type = EvTicketMoved
		from, _, _ := strings.Cut(text, " → ")
		e.Data = TicketEvent{Ticket: *sum, From: from, To: o.Status}
		// The same order as the live stream: the status change, then its
		// resolution comment (newest-first lists put the comment first).
		if o.Comment != nil {
			return []Event{commentEvent(base, ref, o), e}
		}
		return []Event{e}
	case "claim":
		e.Type = EvClaimChanged
		e.Data = ClaimEvent{Ticket: *sum, To: &Claim{By: base.Actor, At: at}}
	case "release":
		e.Type = EvClaimChanged
		e.Data = ClaimEvent{Ticket: *sum, From: &Claim{By: base.Actor}}
	case "comment":
		if o.Comment == nil {
			e.Type = EvCommit
			break
		}
		return []Event{commentEvent(base, ref, o)}
	default: // update, check
		e.Type = EvTicketUpdated
		te := TicketEvent{Ticket: *sum, BodyChanged: o.Body != nil, Changes: map[string]FieldChange{}}
		for k, val := range o.Set {
			var to any
			if val != nil {
				to = *val
			}
			te.Changes[k] = FieldChange{To: to}
		}
		for k := range o.Fields {
			te.Changes["fields."+k] = FieldChange{}
		}
		for k, vs := range o.Add {
			te.Changes[k] = FieldChange{To: map[string]any{"add": vs}}
		}
		for k, vs := range o.Remove {
			c := te.Changes[k]
			if m, ok := c.To.(map[string]any); ok {
				m["remove"] = vs
			} else {
				c.To = map[string]any{"remove": vs}
			}
			te.Changes[k] = c
		}
		if len(o.Check) > 0 {
			// The count at the time, from the subject ("acceptance 1/2").
			te.Changes["acceptance"] = FieldChange{To: strings.TrimPrefix(text, "acceptance ")}
		}
		if len(te.Changes) == 0 {
			te.Changes = nil
		}
		e.Data = te
	}
	return []Event{e}
}

func commentEvent(base Event, ref Ref, o *Op) Event {
	e := base
	e.Type = EvCommentCreated
	c := o.Comment
	ce := &kaspec.CommentEntry{Name: o.CommentName, Comment: c}
	co := commentOut(ce)
	if co.Author == "" {
		co.Author = base.Actor
	}
	e.Data = CommentEvent{Ticket: ref, Comment: co}
	return e
}

// CommitOut is one commit and its diff, limited to the store.
type CommitOut struct {
	SHA       string            `json:"sha"`
	Store     string            `json:"store"`
	Author    string            `json:"author"`
	Date      string            `json:"date"`
	Subject   string            `json:"subject"`
	Trailers  map[string]string `json:"trailers,omitempty"`
	Diff      string            `json:"diff"`
	Truncated bool              `json:"truncated,omitempty"`
}

// maxDiff caps the diff a client is sent.
const maxDiff = 256 << 10

// Commit returns a commit's message and its diff within the store (the
// change behind an activity entry).
func (s *Service) Commit(ctx context.Context, store, sha string) (*CommitOut, error) {
	if !isHexSHA(sha) {
		return nil, Errf(CodeInvalid, "pass a commit hash from an event's commit field", "%q is not a commit hash", sha)
	}
	for _, st := range s.WS.Stores {
		if store != "" && st.Name != store {
			continue
		}
		c, err := st.git.CommitInfo(ctx, sha)
		if err != nil {
			continue
		}
		args := []string{"show", "--format=", "--no-color", "--no-ext-diff", "-M", c.SHA}
		if st.sub != "" {
			args = append(args, "--", st.sub)
		}
		diff, err := st.git.Run(ctx, args...)
		if err != nil {
			return nil, Errf(CodeGit, "", "git show %s: %v", short(c.SHA), err)
		}
		out := &CommitOut{SHA: c.SHA, Store: st.Name, Author: c.Author, Date: c.Date, Subject: c.Subject, Trailers: c.Trailers}
		if len(diff) > maxDiff {
			diff, out.Truncated = diff[:maxDiff], true
		}
		out.Diff = diff
		return out, nil
	}
	return nil, Errf(CodeNotFound, "check the store and hash", "no commit %s in the workspace", sha)
}

func isHexSHA(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
