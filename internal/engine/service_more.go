package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// NextIn is ka_next's input.
type NextIn struct {
	Query   string `json:"query,omitempty"` // extra filters: project:jk kind:bug label:cache
	Project string `json:"project,omitempty"`
	Peek    bool   `json:"peek,omitempty"` // do not claim (claim: false)
}

// NextOut is ka_next's output.
type NextOut struct {
	Ticket    *Detail `json:"ticket,omitempty"`
	Claimed   bool    `json:"claimed"`
	Confirmed bool    `json:"confirmed"`
	PushError string  `json:"push_error,omitempty"`
	Remaining int     `json:"remaining"` // other ready tickets matching the filters
}

// Next picks the best ready ticket and, unless peeking, claims it. A claim
// lost to a concurrent agent moves on to the next candidate.
func (s *Service) Next(ctx context.Context, c Caller, in NextIn) (*NextOut, error) {
	q := in.Query
	if in.Project != "" {
		q += " project:" + in.Project
	} else if s.WS.Project != "" && !strings.Contains(q, "project:") {
		q += " project:" + s.WS.Project
	}
	query, err := ParseQuery(q)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tried := map[string]bool{}
	for attempt := 0; attempt < 8; attempt++ {
		s.ix.catchUp()
		cands := s.candidates(c, query, tried)
		if len(cands) == 0 {
			if attempt == 0 {
				return &NextOut{}, Errf(CodeNoCandidates, "nothing is ready: widen the filters, check ka_search is:blocked, or file work with ka_create", "no ready, unclaimed, unblocked ticket matches")
			}
			return &NextOut{}, Errf(CodeNoCandidates, "every candidate was claimed concurrently; retry shortly", "no ticket could be claimed")
		}
		r := cands[0]
		if in.Peek || r.Store.ReadOnly {
			s.ix.mu.RLock()
			out := &NextOut{Ticket: s.detail(r, nil), Remaining: len(cands) - 1}
			s.ix.mu.RUnlock()
			return out, nil
		}
		co, err := s.claimLocked(ctx, c, r.ID, false)
		if IsCode(err, CodeClaimLost) || IsCode(err, "ticket_closed") {
			tried[r.ID] = true
			continue
		}
		if err != nil {
			return nil, err
		}
		s.ix.mu.RLock()
		rr := s.ix.tickets[r.ID]
		out := &NextOut{Ticket: s.detail(rr, nil), Claimed: true, Confirmed: co.Confirmed, PushError: co.PushError, Remaining: len(cands) - 1}
		s.ix.mu.RUnlock()
		return out, nil
	}
	return nil, Errf(CodeNoCandidates, "retry shortly", "could not claim a ticket after 8 attempts")
}

func (s *Service) candidates(c Caller, q *Query, skip map[string]bool) []*Rec {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	v := s.view()
	var out []*Rec
	for _, r := range s.ix.tickets {
		if skip[r.ID] || r.Invalid || r.Project.File.Archived || r.T.MovedTo != "" {
			continue
		}
		if category(r.Project.Cfg, r.T.Status) != "unstarted" || r.T.Claim != nil || v.isBlocked(r) {
			continue
		}
		if r.T.Assignee != "" && r.T.Assignee != c.Actor {
			continue
		}
		if s.hasOpenChildren(r.ID) {
			continue // the work is in the children
		}
		if !q.match(v, r) {
			continue
		}
		out = append(out, r)
	}
	keys := q.Sort
	if len(keys) == 0 {
		keys = []string{"priority", "created"}
	}
	sortRecs(out, keys)
	// Work assigned to the caller comes first.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].T.Assignee == c.Actor && out[j].T.Assignee != c.Actor
	})
	return out
}

func (s *Service) hasOpenChildren(id string) bool {
	for _, k := range s.ix.children[id] {
		if kr, ok := s.ix.tickets[k]; ok && !closedCategory(category(kr.Project.Cfg, kr.T.Status)) {
			return true
		}
	}
	return false
}

// ---- schema ------------------------------------------------------------------

// ProjectSchema describes one project's vocabulary.
type ProjectSchema struct {
	Key        string                     `json:"key"`
	Prefix     string                     `json:"prefix"`
	Name       string                     `json:"name,omitempty"`
	Store      string                     `json:"store"`
	Archived   bool                       `json:"archived,omitempty"`
	ReadOnly   bool                       `json:"read_only,omitempty"`
	Statuses   []kaspec.Status            `json:"statuses"`
	Initial    string                     `json:"initial"`
	Kinds      []string                   `json:"kinds"`
	Labels     []string                   `json:"labels,omitempty"`
	LabelsOpen bool                       `json:"labels_open"`
	Fields     map[string]kaspec.FieldDef `json:"fields,omitempty"`
	Tickets    int                        `json:"tickets"`
}

// SchemaOut is ka_schema's output.
type SchemaOut struct {
	Projects       []ProjectSchema `json:"projects"`
	DefaultProject string          `json:"default_project,omitempty"`
	Priorities     []string        `json:"priorities"`
	Resolutions    []string        `json:"resolutions"`
	Humans         []string        `json:"humans,omitempty"`
	Agents         []string        `json:"agents,omitempty"`
	Roles          []string        `json:"roles,omitempty"`
	You            string          `json:"you"`
}

// Schema describes the workspace (optionally one project).
func (s *Service) Schema(c Caller, project string) (*SchemaOut, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	out := &SchemaOut{Priorities: kaspec.Priorities, Resolutions: kaspec.Resolutions, DefaultProject: s.WS.Project, You: c.Actor}
	counts := map[string]int{}
	for _, r := range s.ix.tickets {
		counts[r.Project.Key]++
	}
	for _, k := range sortedKeys(s.ix.projects) {
		if project != "" && k != project {
			continue
		}
		pi := s.ix.projects[k]
		cfg := pi.Cfg
		out.Projects = append(out.Projects, ProjectSchema{Key: k, Prefix: pi.Prefix, Name: pi.Name, Store: pi.Store.Name,
			Archived: pi.File.Archived, ReadOnly: pi.Store.ReadOnly, Statuses: cfg.Statuses, Initial: cfg.Initial, Kinds: cfg.Kinds,
			Labels: cfg.LabelsKnown, LabelsOpen: !cfg.LabelsClosed, Fields: cfg.Fields, Tickets: counts[k]})
	}
	if project != "" && len(out.Projects) == 0 {
		return nil, Errf(CodeUnknownProject, "projects: "+strings.Join(sortedKeys(s.ix.projects), ", "), "no project %q", project)
	}
	out.Humans = sortedKeys(s.ix.actors.Humans)
	out.Agents = sortedKeys(s.ix.actors.Agents)
	for r := range s.ix.actors.Roles {
		out.Roles = append(out.Roles, "role/"+r)
	}
	sort.Strings(out.Roles)
	return out, nil
}

// ---- sync --------------------------------------------------------------------

// SyncOut is ka_sync's output.
type SyncOut struct {
	Stores    []SyncState       `json:"stores"`
	Preserved []string          `json:"preserved,omitempty"` // written during this sync
	Conflicts []InboxItem       `json:"conflicts,omitempty"` // recent preserved-conflict comments
	Errors    []*Error          `json:"errors,omitempty"`
	Renamed   map[string]string `json:"renamed,omitempty"`
}

// Sync syncs every store (when force is set) and reports state.
func (s *Service) Sync(ctx context.Context, force bool) *SyncOut {
	out := &SyncOut{}
	if force {
		s.writeMu.Lock()
		for _, st := range s.WS.Stores {
			res, err := st.Sync(ctx, s.hooks(st))
			if err != nil {
				e := AsError(err)
				e.Message = st.Name + ": " + e.Message
				out.Errors = append(out.Errors, e)
			}
			if res != nil {
				out.Preserved = append(out.Preserved, res.Preserved...)
				for k, v := range res.Renamed {
					if out.Renamed == nil {
						out.Renamed = map[string]string{}
					}
					out.Renamed[k] = v
				}
			}
		}
		s.writeMu.Unlock()
	}
	for _, st := range s.WS.Stores {
		out.Stores = append(out.Stores, st.State(ctx))
	}
	s.syncStateEvents(out.Stores)
	if len(out.Preserved) > 0 {
		s.publish(Event{Type: EvConflictPreserved, At: s.now(), Data: map[string]any{"preserved": out.Preserved}})
	}
	week := s.Now().Add(-7 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	s.ix.mu.RLock()
	for _, id := range sortedKeys(s.ix.tickets) {
		r := s.ix.tickets[id]
		for _, ce := range r.Comments {
			if ce.Comment != nil && ce.Comment.Type == "system" && ce.Comment.Author == SystemActor && ce.Comment.Created >= week {
				co := commentOut(ce)
				co.Body = clip(co.Body, 200)
				out.Conflicts = append(out.Conflicts, InboxItem{ID: id, Title: r.T.Title, Comment: co})
			}
		}
	}
	s.ix.mu.RUnlock()
	return out
}

// ---- links -------------------------------------------------------------------

// LinkOut is a resolved code link.
type LinkOut struct {
	Link string `json:"link"`
	Path string `json:"path,omitempty"` // local file
	URL  string `json:"url,omitempty"`  // forge URL
	Line string `json:"line,omitempty"`
}

// ResolveLink turns repo://path[@ref][#Lline] into a local path or forge URL.
func (s *Service) ResolveLink(link string) (*LinkOut, error) {
	out := &LinkOut{Link: link}
	m := kaspec.LinkRe.FindStringSubmatch(link)
	if m == nil {
		if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
			out.URL = link
			return out, nil
		}
		return nil, Errf(CodeInvalid, "links look like jk://path/File.java@ref#L42", "%q is not a code link", link)
	}
	repo, p, ref, lines := m[1], m[2], strings.TrimPrefix(m[3], "@"), strings.TrimPrefix(m[4], "#")
	out.Line = lines
	if dir, ok := s.WS.Repos[repo]; ok {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if _, err := os.Stat(fp); err == nil {
			out.Path = fp
		}
	}
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	for _, k := range sortedKeys(s.ix.projects) {
		pi := s.ix.projects[k]
		if r, ok := pi.File.Repos[repo]; ok && r.Forge != "" {
			b := ref
			if b == "" {
				b = r.DefaultBranch
			}
			if b == "" {
				b = "HEAD"
			}
			out.URL = strings.TrimSuffix(r.Forge, "/") + "/blob/" + b + "/" + p
			if lines != "" {
				l := strings.Replace(lines, "-", "-L", 1)
				l = strings.Replace(l, "-LL", "-L", 1)
				out.URL += "#" + l
			}
			break
		}
	}
	if out.Path == "" && out.URL == "" {
		return nil, Errf("unknown_repo", "declare the repo under repos: in project.yml, or map a local checkout in the workspace's repos:", "no project declares repo %q", repo)
	}
	return out, nil
}

// ---- validation ----------------------------------------------------------------

// Validate runs ka check over the workspace.
func (s *Service) Validate(now time.Time) []kaspec.Diagnostic {
	s.ix.mu.Lock()
	defer s.ix.mu.Unlock()
	return kaspec.Check(s.ix.kws, kaspec.CheckOptions{Now: now})
}

// Projects lists project keys.
func (s *Service) Projects() []string {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	return sortedKeys(s.ix.projects)
}

// Reload rebuilds the index from disk.
func (s *Service) Reload() { s.ix.refresh(nil, nil) }

// TicketPath returns the absolute path of a ticket's ticket.md.
func (s *Service) TicketPath(id string) (string, *Store, error) {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	r, err := s.rec(id)
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(r.Store.Root, filepath.FromSlash(r.Dir), "ticket.md"), r.Store, nil
}

// ReplaceTicket commits a hand-edited ticket.md (ka edit with $EDITOR). The
// edit is validated first and recorded as an update of the changed fields,
// so it replays like any other operation. A non-empty ifMatch requires the
// file on disk to still have that blob SHA.
func (s *Service) ReplaceTicket(ctx context.Context, c Caller, id string, data []byte, ifMatch string) (*WriteOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	st, pi, err := s.storeOf(id)
	if err != nil {
		return nil, err
	}
	s.ix.mu.RLock()
	r, err := s.rec(id)
	var old kaspec.Ticket
	if err == nil {
		old = *r.T
	}
	s.ix.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	nt, ds := kaspec.ParseTicket("ticket.md", kaspec.NormalizeEncoding(data))
	if nt == nil || kaspec.HasErrors(ds, false) {
		return nil, invalidFrom(ds)
	}
	if nt.ID != old.ID {
		return nil, Errf(CodeInvalid, "IDs never change; file a new ticket instead", "id changed from %s to %s", old.ID, nt.ID)
	}
	o := diffOp(&old, nt)
	o.IfMatch = ifMatch
	if nt.Status != old.Status {
		if _, ok := pi.Cfg.StatusByName(nt.Status); !ok {
			return nil, unknownStatus(pi.Cfg, nt.Status)
		}
		if closedCategory(category(pi.Cfg, nt.Status)) {
			return nil, Errf(CodeResolutionRequired, "close it with `ka move "+old.ID+" "+nt.Status+" -m \"summary\"`", "closing needs a resolution summary")
		}
	}
	stamp(o, c, s.now())
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, err
	}
	if nt.Status != old.Status {
		t := stamp(&Op{Op: "transition", ID: old.ID, Status: nt.Status}, c, s.now())
		if r2, err := s.do(ctx, st, t, true); err == nil {
			res = r2
		}
	}
	return writeOut(old.ID, res), nil
}

// diffOp expresses the difference between two versions of a ticket as an
// update operation.
func diffOp(a, b *kaspec.Ticket) *Op {
	o := &Op{Op: "update", ID: a.ID, Set: map[string]*string{}, Add: map[string][]string{}, Remove: map[string][]string{}}
	set := func(k, x, y string) {
		if x != y {
			v := y
			o.Set[k] = &v
		}
	}
	set("title", a.Title, b.Title)
	set("priority", a.Priority, b.Priority)
	set("kind", a.Kind, b.Kind)
	set("assignee", a.Assignee, b.Assignee)
	set("owner", a.Owner, b.Owner)
	set("parent", a.Parent, b.Parent)
	set("duplicate_of", a.DuplicateOf, b.DuplicateOf)
	set("milestone", a.Milestone, b.Milestone)
	set("due", a.Due, b.Due)
	lists := func(k string, x, y []string) {
		for _, v := range y {
			if !slices.Contains(x, v) {
				o.Add[k] = append(o.Add[k], v)
			}
		}
		for _, v := range x {
			if !slices.Contains(y, v) {
				o.Remove[k] = append(o.Remove[k], v)
			}
		}
	}
	lists("labels", a.Labels, b.Labels)
	lists("blocked_by", a.BlockedBy, b.BlockedBy)
	lists("related", a.Related, b.Related)
	lists("links", a.Links, b.Links)
	var at, bt []string
	for _, it := range a.Acceptance {
		at = append(at, it.Text)
	}
	for _, it := range b.Acceptance {
		bt = append(bt, it.Text)
	}
	lists("acceptance", at, bt)
	for i, it := range b.Acceptance {
		j := slices.IndexFunc(a.Acceptance, func(x kaspec.AcceptanceItem) bool { return x.Text == it.Text })
		if (j >= 0 && a.Acceptance[j].Done != it.Done) || (j < 0 && it.Done) {
			_ = i
			o.Check = append(o.Check, CheckItem{Text: it.Text, Done: it.Done})
		}
	}
	for k, v := range b.Fields {
		if av, ok := a.Fields[k]; !ok || fmt.Sprint(av) != fmt.Sprint(v) {
			vv := v
			if o.Fields == nil {
				o.Fields = map[string]*kaspec.FieldValue{}
			}
			o.Fields[k] = &vv
		}
	}
	for k := range a.Fields {
		if _, ok := b.Fields[k]; !ok {
			if o.Fields == nil {
				o.Fields = map[string]*kaspec.FieldValue{}
			}
			o.Fields[k] = nil
		}
	}
	if a.Body != b.Body {
		body := b.Body
		o.Body = &body
	}
	return o
}

// BoardFilter returns a saved board's filter.
func (s *Service) BoardFilter(name string) (string, bool) {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	for _, st := range s.ix.stores {
		if b, ok := s.ix.ks[st].Boards[name]; ok && b != nil {
			return b.Filter, true
		}
	}
	return "", false
}
