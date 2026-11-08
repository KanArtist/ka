package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// Caller identifies who is acting (PRD §7.6, §11.1).
type Caller struct {
	Actor      string // bsant or agent/claude-code
	OnBehalfOf string // the human an agent works for
	Session    string
}

// Service is the operation and query layer shared by the CLI, MCP and REST.
type Service struct {
	WS       *Workspace
	ix       *Index
	writeMu  sync.Mutex
	ops      OpStats
	Now      func() time.Time
	NoPush   bool          // commit locally only (ka --no-push)
	PollStep time.Duration // ka_ask wait polling interval
	human    string

	events   EventSink
	syncMu   sync.Mutex
	lastSync map[string]string // store -> last published sync state

	// Background marks a long-running process (the daemon, ka mcp): it may
	// debounce pushes and run the fetch loop. Call Flush before exiting.
	Background bool
	deb        debouncer
	kick       chan string
	kickOnce   sync.Once
}

// New builds the service and its index.
func New(ctx context.Context, ws *Workspace) (*Service, error) {
	s := &Service{WS: ws, Now: time.Now, PollStep: 2 * time.Second}
	s.ix = newIndex(ws.Stores)
	s.human = s.detectHuman(ctx)
	s.applyRuntime()
	return s, nil
}

// Index exposes the index (read-only use).
func (s *Service) Index() *Index { return s.ix }

func (s *Service) now() string { return s.Now().UTC().Format("2006-01-02T15:04:05Z") }

var actorClean = regexp.MustCompile(`[^a-z0-9._-]+`)

// NormalizeAgent turns an MCP client name into an agent actor.
func NormalizeAgent(name string) string {
	n := strings.Trim(actorClean.ReplaceAllString(strings.ToLower(name), "-"), "-._")
	if n == "" {
		n = "unknown"
	}
	if len(n) > 60 {
		n = n[:60]
	}
	return "agent/" + n
}

// detectHuman finds the principal: identity.actor, then an actor whose
// emails include git's user.email, then a handle derived from user.email.
func (s *Service) detectHuman(ctx context.Context) string {
	if a := s.WS.Actor; a != "" && kaspec.HumanRe.MatchString(a) {
		return a
	}
	if a := os.Getenv("KA_HUMAN"); a != "" {
		return a
	}
	var email, name string
	for _, st := range s.WS.Stores {
		// git var honors GIT_AUTHOR_* as well as user.name/user.email.
		if ident, err := st.git.Run(ctx, "var", "GIT_AUTHOR_IDENT"); err == nil {
			if i := strings.Index(ident, " <"); i >= 0 {
				name = ident[:i]
				if j := strings.Index(ident[i:], ">"); j > 2 {
					email = ident[i+2 : i+j]
				}
			}
		}
		if email != "" {
			break
		}
	}
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	for _, h := range sortedKeys(s.ix.actors.Humans) {
		hu := s.ix.actors.Humans[h]
		for _, e := range hu.Emails {
			if strings.EqualFold(e, email) {
				return h
			}
		}
		for _, a := range hu.Aliases {
			if a == name {
				return h
			}
		}
	}
	local, _, _ := strings.Cut(email, "@")
	if h := strings.Trim(actorClean.ReplaceAllString(strings.ToLower(local), "-"), "-._"); h != "" && kaspec.HumanRe.MatchString(h) {
		return h
	}
	return "unknown"
}

// Human returns the detected principal.
func (s *Service) Human() string { return s.human }

// CallerFor builds a caller for an actor (empty means the local human).
func (s *Service) CallerFor(actor, session string) Caller {
	if actor == "" {
		actor = s.human
	}
	c := Caller{Actor: actor, Session: session}
	if strings.HasPrefix(actor, "agent/") && s.human != "unknown" {
		c.OnBehalfOf = s.human
	}
	return c
}

// CallerAs builds a caller for an authenticated principal (remote mode,
// PRD §15): a person acts as themselves; an agent (actor agent/<name>)
// acts on the principal's behalf. principal may be a handle or an email.
func (s *Service) CallerAs(principal, actor, session string) Caller {
	p := s.ActorFor(principal)
	if strings.HasPrefix(actor, "agent/") {
		return Caller{Actor: actor, OnBehalfOf: p, Session: session}
	}
	return Caller{Actor: p, Session: session}
}

// ActorFor maps a name, alias or email to an actor handle through the actor
// registry; anything else is returned as given.
func (s *Service) ActorFor(nameOrEmail string) string {
	v := strings.TrimSpace(nameOrEmail)
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	if _, ok := s.ix.actors.Humans[v]; ok {
		return v
	}
	for _, h := range sortedKeys(s.ix.actors.Humans) {
		hu := s.ix.actors.Humans[h]
		for _, e := range hu.Emails {
			if strings.EqualFold(e, v) {
				return h
			}
		}
		for _, a := range hu.Aliases {
			if strings.EqualFold(a, v) {
				return h
			}
		}
	}
	if local, _, ok := strings.Cut(v, "@"); ok {
		if h := strings.Trim(actorClean.ReplaceAllString(strings.ToLower(local), "-"), "-._"); h != "" && kaspec.HumanRe.MatchString(h) {
			return h
		}
	}
	return v
}

// ---- reading -----------------------------------------------------------------

func (s *Service) view() view { return view{ix: s.ix, now: s.Now()} }

func (s *Service) rec(id string) (*Rec, error) {
	r, ok := s.ix.tickets[strings.ToUpper(strings.TrimSpace(id))]
	if !ok {
		e := Errf(CodeNotFound, "check the ID with ka_search", "ticket %s does not exist", id)
		if _, known := s.ix.projectFor(strings.ToUpper(id)); !known {
			e.Fix = "IDs look like JK-7QK3M; ka_schema lists project prefixes"
		}
		return nil, e
	}
	return r, nil
}

// Summary is the compact form of a ticket used in lists.
type Summary struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Status     string   `json:"status"`
	Priority   string   `json:"priority,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Assignee   string   `json:"assignee,omitempty"`
	Claim      *Claim   `json:"claim,omitempty"`
	Parent     string   `json:"parent,omitempty"`
	Blocked    bool     `json:"blocked,omitempty"`
	Acceptance string   `json:"acceptance,omitempty"` // "2/5"
	Project    string   `json:"project"`
	Updated    string   `json:"updated,omitempty"`
	// Filled for ka_get, events, and searches with include=fields (the
	// table view's columns); empty in agent-facing lists to save tokens.
	Owner   string         `json:"owner,omitempty"`
	Created string         `json:"created,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Claim is a claim as reported to callers.
type Claim struct {
	By    string `json:"by"`
	At    string `json:"at"`
	Stale bool   `json:"stale,omitempty"`
}

func (s *Service) summary(v view, r *Rec) Summary {
	t := r.T
	sm := Summary{ID: r.ID, Title: t.Title, Status: t.Status, Priority: t.Priority, Kind: t.Kind, Labels: t.Labels,
		Assignee: t.Assignee, Parent: t.Parent, Blocked: v.isBlocked(r), Project: r.Project.Key, Updated: t.Updated}
	if t.Claim != nil {
		sm.Claim = &Claim{By: t.Claim.By, At: t.Claim.At, Stale: v.stale(r)}
	}
	if n := len(t.Acceptance); n > 0 {
		d := 0
		for _, a := range t.Acceptance {
			if a.Done {
				d++
			}
		}
		sm.Acceptance = fmt.Sprintf("%d/%d", d, n)
	}
	if v.full {
		sm.Owner, sm.Created, sm.Fields = t.Owner, t.Created, fieldsOut(t)
	}
	return sm
}

func fieldsOut(t *kaspec.Ticket) map[string]any {
	if len(t.Fields) == 0 {
		return nil
	}
	m := make(map[string]any, len(t.Fields))
	for k, fv := range t.Fields {
		if fv.IsList {
			m[k] = fv.List
		} else {
			m[k] = fv.Scalar
		}
	}
	return m
}

// SearchIn is ka_search's input.
type SearchIn struct {
	Query string   `json:"query,omitempty"`
	Sort  []string `json:"sort,omitempty"`
	Limit int      `json:"limit,omitempty"` // 0: 20; negative: all
	All   bool     `json:"all,omitempty"`   // include closed tickets when the query does not say
	// Full adds owner, created and custom fields to each summary.
	Full bool `json:"-"`
}

// SearchOut is ka_search's output.
type SearchOut struct {
	Total   int       `json:"total"`
	Tickets []Summary `json:"tickets"`
}

// Search runs a query.
func (s *Service) Search(in SearchIn) (*SearchOut, error) {
	s.ix.catchUp()
	q, err := ParseQuery(in.Query)
	if err != nil {
		return nil, err
	}
	if !in.All && !q.Has("is") && !q.Has("status") && !q.Has("category") && !q.Has("id") {
		q.terms = append(q.terms, term{key: "is", values: []string{"open"}})
	}
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	v := s.view()
	v.full = in.Full
	var rs []*Rec
	for _, r := range s.ix.tickets {
		if q.match(v, r) {
			rs = append(rs, r)
		}
	}
	keys := in.Sort
	if len(keys) == 0 {
		keys = q.Sort
	}
	sortRecs(rs, keys)
	out := &SearchOut{Total: len(rs), Tickets: []Summary{}}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = 20
	case limit < 0: // everything
		limit = len(rs)
	}
	for i, r := range rs {
		if i >= limit {
			break
		}
		out.Tickets = append(out.Tickets, s.summary(v, r))
	}
	return out, nil
}

// Ref is a related ticket, briefly.
type Ref struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// CommentOut is a comment as reported to callers.
type CommentOut struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Author     string   `json:"author"`
	Created    string   `json:"created"`
	To         string   `json:"to,omitempty"`
	Options    []string `json:"options,omitempty"`
	Answers    string   `json:"answers,omitempty"`
	Choice     *int64   `json:"choice,omitempty"`
	ChoiceText string   `json:"choice_text,omitempty"` // the chosen option, from the question
	Approved   *bool    `json:"approved,omitempty"`
	Resolution string   `json:"resolution,omitempty"`
	Body       string   `json:"body,omitempty"`
}

func commentOut(c *kaspec.CommentEntry) CommentOut {
	if c.Comment == nil {
		return CommentOut{Name: c.Name, Type: "invalid"}
	}
	k := c.Comment
	return CommentOut{Name: c.Name, Type: k.Type, Author: k.Author, Created: k.Created, To: k.To, Options: k.Options,
		Answers: k.Answers, Choice: k.Choice, Approved: k.Approved, Resolution: k.Resolution, Body: k.Body}
}

// recCommentOut is commentOut plus, for an answer that picked an option,
// that option's text from the question it answers.
func recCommentOut(r *Rec, ce *kaspec.CommentEntry) CommentOut {
	co := commentOut(ce)
	if co.Choice == nil || co.Answers == "" {
		return co
	}
	for _, q := range r.Comments {
		if q.Name == co.Answers && q.Comment != nil {
			if i := int(*co.Choice); i >= 0 && i < len(q.Comment.Options) {
				co.ChoiceText = q.Comment.Options[i]
			}
			break
		}
	}
	return co
}

// Detail is a full ticket with its context digest.
type Detail struct {
	Summary
	ParentRef     *Ref                    `json:"parent_ref,omitempty"`
	BlockedBy     []Ref                   `json:"blocked_by,omitempty"`
	Blocks        []Ref                   `json:"blocks,omitempty"`
	Children      []Ref                   `json:"children,omitempty"`
	Related       []string                `json:"related,omitempty"`
	Links         []string                `json:"links,omitempty"`
	AcceptanceAll []kaspec.AcceptanceItem `json:"acceptance_items,omitempty"`
	Body          string                  `json:"body,omitempty"`
	OpenQuestions []CommentOut            `json:"open_questions,omitempty"`
	Decisions     []CommentOut            `json:"decisions,omitempty"`
	Recent        []CommentOut            `json:"recent_comments,omitempty"`
	Comments      []CommentOut            `json:"comments,omitempty"`
	CommentCount  int                     `json:"comment_count,omitempty"`
	CreatedBy     string                  `json:"created_by,omitempty"`
	Store         string                  `json:"store"`
	Path          string                  `json:"path"`
}

// GetIn is ka_get's input.
type GetIn struct {
	ID      string   `json:"id"`
	Include []string `json:"include,omitempty"` // comments, children, relations
}

// Get returns a ticket with a digest of its comments.
func (s *Service) Get(in GetIn) (*Detail, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	r, err := s.rec(in.ID)
	if err != nil {
		return nil, err
	}
	return s.detail(r, in.Include), nil
}

func (s *Service) ref(id string) Ref {
	if r, ok := s.ix.tickets[id]; ok {
		return Ref{ID: id, Title: r.T.Title, Status: r.T.Status}
	}
	return Ref{ID: id}
}

func (s *Service) detail(r *Rec, include []string) *Detail {
	v := s.view()
	v.full = true
	t := r.T
	d := &Detail{Summary: s.summary(v, r), Related: t.Related, Links: t.Links, AcceptanceAll: t.Acceptance,
		Body: t.Body, CreatedBy: t.CreatedBy, Store: r.Store.Name, Path: filepath.Join(r.Store.Root, filepath.FromSlash(r.Dir), "ticket.md")}
	d.Acceptance = ""
	if t.Parent != "" {
		p := s.ref(t.Parent)
		d.ParentRef = &p
		d.Parent = ""
	}
	open := slices.Contains(include, "relations") || true
	if open {
		for _, b := range t.BlockedBy {
			d.BlockedBy = append(d.BlockedBy, s.ref(b))
		}
		for _, b := range s.ix.blocks[r.ID] {
			d.Blocks = append(d.Blocks, s.ref(b))
		}
	}
	kids := s.ix.children[r.ID]
	if slices.Contains(include, "children") || len(kids) <= 10 {
		for _, c := range kids {
			d.Children = append(d.Children, s.ref(c))
		}
	}
	d.CommentCount = len(r.Comments)
	if slices.Contains(include, "comments") {
		for _, c := range r.Comments {
			d.Comments = append(d.Comments, recCommentOut(r, c))
		}
		return d
	}
	for _, q := range openQuestions(r) {
		d.OpenQuestions = append(d.OpenQuestions, commentOut(q))
	}
	for _, c := range r.Comments {
		if c.Comment != nil && c.Comment.Type == "decision" {
			d.Decisions = append(d.Decisions, commentOut(c))
		}
	}
	// The digest: the last few comments that are not already shown, with
	// progress notes limited to the newest one.
	var recent []CommentOut
	seenProgress := false
	for i := len(r.Comments) - 1; i >= 0 && len(recent) < 3; i-- {
		c := r.Comments[i]
		if c.Comment == nil || c.Comment.Type == "decision" || (c.Comment.Type == "question" && slices.ContainsFunc(d.OpenQuestions, func(q CommentOut) bool { return q.Name == c.Name })) {
			continue
		}
		if c.Comment.Type == "progress" {
			if seenProgress {
				continue
			}
			seenProgress = true
		}
		co := commentOut(c)
		co.Body = clip(co.Body, 600)
		recent = append(recent, co)
	}
	slices.Reverse(recent)
	d.Recent = recent
	return d
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + " …"
}

// ---- writing -----------------------------------------------------------------

func (s *Service) hooks(st *Store) hooks { return s.hooksBy(st, "") }

// hooksBy attributes reloaded tickets to actor. A full reload follows a
// fast-forward to upstream, whose changes are someone else's.
func (s *Service) hooksBy(st *Store, actor string) hooks {
	return hooks{
		env: func() applyEnv { return ixEnv{s.ix, st} },
		reloaded: func(paths []string) {
			if paths == nil {
				s.ix.refreshBy(st, nil, "")
				return
			}
			s.ix.refreshBy(st, paths, actor)
		},
	}
}

func (s *Service) do(ctx context.Context, st *Store, o *Op, push bool) (res *Result, err error) {
	start := time.Now()
	defer func() { s.ops.record(o.Op, err, time.Since(start)) }()
	deb := push && s.debounced(st)
	res, err = st.Do(ctx, s.hooksBy(st, o.Actor), o, push && !deb && !s.NoPush && st.PushMode != "manual")
	if err == nil && deb && res.HasRemote && res.Commit != "" {
		res.PushPending = true
		s.schedulePush(st)
	}
	if err == nil {
		s.syncEvents(st, res)
	}
	return res, err
}

func (s *Service) push(ctx context.Context, st *Store) (*Result, error) {
	res, err := s.pushStore(ctx, st)
	if err == nil {
		s.syncEvents(st, res)
	}
	return res, err
}

func (s *Service) storeOf(id string) (*Store, *ProjectInfo, error) {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	pi, ok := s.ix.projectFor(strings.ToUpper(id))
	if !ok {
		return nil, nil, Errf(CodeUnknownProject, "check the ID prefix; ka_schema lists projects", "no project has the prefix of %s", id)
	}
	return pi.Store, pi, nil
}

func stamp(o *Op, c Caller, at string) *Op {
	o.Actor, o.OnBehalfOf, o.Session, o.At = c.Actor, c.OnBehalfOf, c.Session, at
	return o
}

// WriteOut is the compact result of a write.
type WriteOut struct {
	ID        string   `json:"id"`
	Status    string   `json:"status,omitempty"`
	Pushed    bool     `json:"pushed"`
	Local     bool     `json:"local_only,omitempty"` // the store has no remote
	PushError string   `json:"push_error,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	Unblocked []string `json:"unblocked,omitempty"`
	Commit    string   `json:"commit,omitempty"`
	Preserved []string `json:"preserved,omitempty"`
	NoChange  bool     `json:"no_change,omitempty"`
	// PushPending: committed; a debounced push follows within seconds.
	PushPending bool `json:"push_pending,omitempty"`
}

func writeOut(id string, res *Result) *WriteOut {
	w := &WriteOut{ID: id}
	if res != nil {
		w.Pushed, w.Local, w.PushError, w.Commit, w.Preserved = res.Pushed, !res.HasRemote, res.PushError, short(res.Commit), res.Preserved
		w.NoChange = res.Commit == ""
		w.PushPending = res.PushPending
	}
	return w
}

// CreateIn is one ticket for ka_create.
type CreateIn struct {
	Project    string         `json:"project,omitempty"`
	Title      string         `json:"title"`
	Kind       string         `json:"kind,omitempty"`
	Priority   string         `json:"priority,omitempty"`
	Status     string         `json:"status,omitempty"`
	Labels     []string       `json:"labels,omitempty"`
	Assignee   string         `json:"assignee,omitempty"`
	Owner      string         `json:"owner,omitempty"`
	Parent     string         `json:"parent,omitempty"`
	BlockedBy  []string       `json:"blocked_by,omitempty"`
	Related    []string       `json:"related,omitempty"`
	Links      []string       `json:"links,omitempty"`
	Acceptance []string       `json:"acceptance,omitempty"`
	Fields     map[string]any `json:"fields,omitempty"`
	Problem    string         `json:"problem,omitempty"`
	Approach   string         `json:"approach,omitempty"`
	Body       string         `json:"body,omitempty"`
}

// CreateOut lists created tickets.
type CreateOut struct {
	Created []Ref `json:"created"`
	Pushed  bool  `json:"pushed"`
	Local   bool  `json:"local_only,omitempty"`
	// PushPending: committed; a debounced push follows within seconds.
	PushPending bool   `json:"push_pending,omitempty"`
	Error       *Error `json:"error,omitempty"` // first failure; earlier tickets were still created
}

func (s *Service) resolveProject(key string) (*ProjectInfo, error) {
	if key == "" {
		key = s.WS.Project
	}
	if key == "" {
		var writable []string
		for k, pi := range s.ix.projects {
			if !pi.Store.ReadOnly && !pi.File.Archived {
				writable = append(writable, k)
			}
		}
		if len(writable) == 1 {
			key = writable[0]
		} else {
			sort.Strings(writable)
			return nil, Errf(CodeInvalid, "pass project (one of: "+strings.Join(writable, ", ")+")", "no default project")
		}
	}
	pi, ok := s.ix.projects[strings.ToLower(key)]
	if !ok {
		keys := sortedKeys(s.ix.projects)
		e := Errf(CodeUnknownProject, "projects: "+strings.Join(keys, ", "), "no project %q", key)
		if to := s.ix.movedTo(strings.ToLower(key)); to != "" {
			e.Message = fmt.Sprintf("project %q moved to store %s", key, to)
			e.Fix = "add that store to your workspace: ka store add " + to
		} else if n := nearest(key, keys); n != "" {
			e.Fix = "did you mean " + n + "? projects: " + strings.Join(keys, ", ")
		}
		return nil, e
	}
	return pi, nil
}

func (s *Service) newID(pi *ProjectInfo) string {
	n := pi.Cfg.IDSuffixLength
	if n < 5 {
		n = 5
	}
	for {
		id := kaspec.NewID(pi.Prefix, n)
		_, sfx, _ := kaspec.ParseID(id)
		if kaspec.LowEntropy(sfx) {
			continue
		}
		if _, taken := s.ix.tickets[id]; taken {
			continue
		}
		if _, err := os.Stat(filepath.Join(pi.Store.Root, filepath.FromSlash(pi.Dir), "tickets", id)); err == nil {
			continue
		}
		return id
	}
}

func composeBody(in CreateIn) string {
	if in.Body != "" {
		return in.Body
	}
	var parts []string
	if in.Problem != "" {
		parts = append(parts, "## Problem\n\n"+strings.TrimSpace(in.Problem))
	}
	if in.Approach != "" {
		parts = append(parts, "## Approach\n\n"+strings.TrimSpace(in.Approach))
	}
	return strings.Join(parts, "\n\n")
}

// fieldValue converts a JSON value into a custom field value of the declared type.
func fieldValue(def kaspec.FieldDef, v any) (*kaspec.FieldValue, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		var l []string
		for _, it := range x {
			l = append(l, fmt.Sprint(it))
		}
		return &kaspec.FieldValue{List: l, IsList: true}, nil
	case []string:
		return &kaspec.FieldValue{List: x, IsList: true}, nil
	case bool:
		return &kaspec.FieldValue{Scalar: strconv.FormatBool(x), Typed: true}, nil
	case float64:
		return &kaspec.FieldValue{Scalar: strconv.FormatFloat(x, 'f', -1, 64), Typed: true}, nil
	case int:
		return &kaspec.FieldValue{Scalar: strconv.Itoa(x), Typed: true}, nil
	case string:
		switch def.Type {
		case "number", "integer", "boolean", "date", "timestamp":
			return &kaspec.FieldValue{Scalar: x, Typed: true}, nil
		case "list":
			var l []string
			for _, p := range strings.Split(x, ",") {
				if p = strings.TrimSpace(p); p != "" {
					l = append(l, p)
				}
			}
			return &kaspec.FieldValue{List: l, IsList: true}, nil
		}
		return &kaspec.FieldValue{Scalar: x}, nil
	}
	return &kaspec.FieldValue{Scalar: fmt.Sprint(v)}, nil
}

func (s *Service) buildFields(pi *ProjectInfo, in map[string]any) (map[string]kaspec.FieldValue, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]kaspec.FieldValue{}
	for k, v := range in {
		def, ok := pi.Cfg.Fields[k]
		if !ok {
			return nil, Errf(CodeUnknownField, "declared fields: "+strings.Join(pi.Cfg.FieldOrder, ", "), "custom field %q is not declared in %s", k, pi.Key)
		}
		fv, err := fieldValue(def, v)
		if err != nil {
			return nil, err
		}
		if fv != nil {
			out[k] = *fv
		}
	}
	return out, nil
}

func normalizeIDs(l []string) []string {
	out := make([]string, 0, len(l))
	for _, x := range l {
		if x = strings.ToUpper(strings.TrimSpace(x)); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Create files one or many tickets. Each ticket is its own commit; the batch
// is pushed once at the end.
func (s *Service) Create(ctx context.Context, c Caller, ins []CreateIn) (*CreateOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	out := &CreateOut{Created: []Ref{}}
	touched := map[*Store]bool{}
	var firstErr error
	for i, in := range ins {
		s.ix.mu.RLock()
		pi, err := s.resolveProject(in.Project)
		var o *Op
		if err == nil {
			o, err = s.createOp(c, pi, in)
		}
		s.ix.mu.RUnlock()
		if err != nil {
			firstErr = withIndex(err, i, len(ins))
			break
		}
		if _, err := s.do(ctx, pi.Store, o, false); err != nil {
			firstErr = withIndex(err, i, len(ins))
			break
		}
		touched[pi.Store] = true
		out.Created = append(out.Created, Ref{ID: o.ID, Title: o.New.Title, Status: o.New.Status})
	}
	out.Pushed, out.Local = true, true
	for st := range touched {
		res, err := s.push(ctx, st)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if res != nil {
			out.Pushed = out.Pushed && (res.Pushed || !res.HasRemote)
			out.Local = out.Local && !res.HasRemote
			out.PushPending = out.PushPending || res.PushPending
			for i := range out.Created {
				if n, ok := res.Renamed[out.Created[i].ID]; ok {
					out.Created[i].ID = n
				}
			}
		}
	}
	if len(touched) == 0 {
		out.Pushed, out.Local = false, false
	}
	if firstErr != nil {
		if len(out.Created) == 0 {
			return nil, firstErr
		}
		out.Error = AsError(firstErr)
	}
	return out, nil
}

func withIndex(err error, i, n int) error {
	if n <= 1 {
		return err
	}
	e := *AsError(err)
	e.Message = fmt.Sprintf("ticket %d: %s", i, e.Message)
	return &e
}

func (s *Service) createOp(c Caller, pi *ProjectInfo, in CreateIn) (*Op, error) {
	if pi.Store.ReadOnly {
		return nil, Errf(CodeReadOnly, "add the store to your workspace to write to it", "project %s lives in a store this workspace only reads", pi.Key)
	}
	if pi.File.Archived {
		return nil, Errf(CodeInvalid, "file it in an active project", "project %s is archived", pi.Key)
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, Errf(CodeInvalid, "", "title is required")
	}
	status := in.Status
	if status == "" {
		status = pi.Cfg.Initial
		if strings.HasPrefix(c.Actor, "agent/") && pi.Cfg.AgentCreatedInitial != "" {
			status = pi.Cfg.AgentCreatedInitial
		}
	}
	fields, err := s.buildFields(pi, in.Fields)
	if err != nil {
		return nil, err
	}
	owner := in.Owner
	if owner == "" {
		owner = c.OnBehalfOf
		if owner == "" && !strings.HasPrefix(c.Actor, "agent/") {
			owner = c.Actor
		}
		if owner == "unknown" {
			owner = ""
		}
	}
	now := s.now()
	t := &kaspec.Ticket{
		Title: strings.TrimSpace(in.Title), Status: status, Priority: strings.ToUpper(in.Priority), Kind: in.Kind,
		Labels: in.Labels, Assignee: in.Assignee, Owner: owner, Parent: strings.ToUpper(in.Parent),
		BlockedBy: normalizeIDs(in.BlockedBy), Related: normalizeIDs(in.Related), Links: in.Links,
		Fields: fields, Created: now, CreatedBy: c.Actor, Updated: now, Body: composeBody(in),
	}
	for _, a := range in.Acceptance {
		if a = strings.TrimSpace(a); a != "" {
			t.Acceptance = append(t.Acceptance, kaspec.AcceptanceItem{Text: a})
		}
	}
	id := s.newID(pi)
	return stamp(&Op{Op: "create", ID: id, New: t}, c, now), nil
}

// pushStore publishes a store's local commits (or schedules a debounced push).
func (s *Service) pushStore(ctx context.Context, st *Store) (*Result, error) {
	if s.NoPush || st.PushMode == "manual" || s.debounced(st) {
		remote, _ := st.upstream(ctx)
		res := &Result{HasRemote: remote != ""}
		if res.HasRemote && s.debounced(st) {
			res.PushPending = true
			s.schedulePush(st)
		}
		return res, nil
	}
	return st.Sync(ctx, s.hooks(st))
}

// UpdateIn patches one ticket.
type UpdateIn struct {
	ID     string              `json:"id"`
	Set    map[string]any      `json:"set,omitempty"`
	Add    map[string][]string `json:"add,omitempty"`
	Remove map[string][]string `json:"remove,omitempty"`
	Body   *string             `json:"body,omitempty"`
	// IfMatch makes the update conditional on ticket.md's blob SHA (REST If-Match).
	IfMatch string `json:"if_match,omitempty"`
}

// Update patches one or many tickets (one commit each, one push per store).
func (s *Service) Update(ctx context.Context, c Caller, ins []UpdateIn) ([]*WriteOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	var outs []*WriteOut
	touched := map[*Store]bool{}
	var firstErr error
	for i, in := range ins {
		st, pi, err := s.storeOf(in.ID)
		var o *Op
		if err == nil {
			o, err = s.updateOp(c, pi, in)
		}
		if err == nil {
			var res *Result
			res, err = s.do(ctx, st, o, false)
			if err == nil {
				touched[st] = true
				outs = append(outs, writeOut(o.ID, res))
			}
		}
		if err != nil {
			firstErr = withIndex(err, i, len(ins))
			break
		}
	}
	for st := range touched {
		res, err := s.push(ctx, st)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		for _, w := range outs {
			if res != nil {
				w.Pushed, w.Local, w.PushError, w.PushPending = res.Pushed, !res.HasRemote, res.PushError, res.PushPending
			}
		}
	}
	if firstErr != nil && len(outs) == 0 {
		return nil, firstErr
	}
	return outs, firstErr
}

func (s *Service) updateOp(c Caller, pi *ProjectInfo, in UpdateIn) (*Op, error) {
	o := &Op{Op: "update", ID: strings.ToUpper(in.ID), Set: map[string]*string{}, Add: in.Add, Remove: in.Remove, Body: in.Body, IfMatch: in.IfMatch}
	for k, v := range in.Set {
		if name, ok := strings.CutPrefix(k, "fields."); ok {
			if err := s.setField(pi, o, name, v); err != nil {
				return nil, err
			}
			continue
		}
		if k == "fields" {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, Errf(CodeInvalid, "", "set.fields must be an object")
			}
			for name, fv := range m {
				if err := s.setField(pi, o, name, fv); err != nil {
					return nil, err
				}
			}
			continue
		}
		if v == nil {
			o.Set[k] = nil
			continue
		}
		str := fmt.Sprint(v)
		switch k {
		case "priority":
			str = strings.ToUpper(str)
		case "parent", "duplicate_of":
			str = strings.ToUpper(str)
		}
		o.Set[k] = &str
	}
	for _, m := range []map[string][]string{o.Add, o.Remove} {
		for k, vs := range m {
			if k == "blocked_by" || k == "related" {
				m[k] = normalizeIDs(vs)
			}
		}
	}
	return stamp(o, c, s.now()), nil
}

func (s *Service) setField(pi *ProjectInfo, o *Op, name string, v any) error {
	def, ok := pi.Cfg.Fields[name]
	if !ok {
		return Errf(CodeUnknownField, "declared fields: "+strings.Join(pi.Cfg.FieldOrder, ", "), "custom field %q is not declared in %s", name, pi.Key)
	}
	fv, err := fieldValue(def, v)
	if err != nil {
		return err
	}
	if o.Fields == nil {
		o.Fields = map[string]*kaspec.FieldValue{}
	}
	o.Fields[name] = fv
	return nil
}

func startStatus(cfg *kaspec.Config, current string) string {
	if category(cfg, current) == "started" {
		return ""
	}
	for _, st := range cfg.Statuses {
		if st.Category == "started" && !st.Blocked {
			return st.Name
		}
	}
	return ""
}

func unstartedStatus(cfg *kaspec.Config) string {
	for _, st := range cfg.Statuses {
		if st.Category == "unstarted" {
			return st.Name
		}
	}
	return ""
}

// ClaimOut reports a claim.
type ClaimOut struct {
	WriteOut
	Confirmed bool `json:"confirmed"` // published (or the store has no remote)
}

// Claim claims a ticket for the caller and pushes synchronously.
func (s *Service) Claim(ctx context.Context, c Caller, id string, steal bool) (*ClaimOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	return s.claimLocked(ctx, c, id, steal)
}

func (s *Service) claimLocked(ctx context.Context, c Caller, id string, steal bool) (*ClaimOut, error) {
	st, pi, err := s.storeOf(id)
	if err != nil {
		return nil, err
	}
	s.ix.mu.RLock()
	cur := ""
	if r, ok := s.ix.tickets[strings.ToUpper(id)]; ok {
		cur = r.T.Status
	}
	s.ix.mu.RUnlock()
	o := stamp(&Op{Op: "claim", ID: strings.ToUpper(id), Steal: steal, Status: startStatus(pi.Cfg, cur)}, c, s.now())
	// Claims always push synchronously (PRD §10.1), even in manual mode.
	start := time.Now()
	res, err := st.Do(ctx, s.hooks(st), o, !s.NoPush)
	s.ops.record(o.Op, err, time.Since(start))
	if err != nil {
		return nil, err
	}
	w := writeOut(o.ID, res)
	s.ix.mu.RLock()
	if r, ok := s.ix.tickets[o.ID]; ok {
		w.Status = r.T.Status
	}
	s.ix.mu.RUnlock()
	return &ClaimOut{WriteOut: *w, Confirmed: res.Pushed || !res.HasRemote}, nil
}

// Release gives up a claim, optionally moving the ticket.
func (s *Service) Release(ctx context.Context, c Caller, id, status string, force bool) (*WriteOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	st, pi, err := s.storeOf(id)
	if err != nil {
		return nil, err
	}
	if status == "" {
		s.ix.mu.RLock()
		if r, ok := s.ix.tickets[strings.ToUpper(id)]; ok && category(pi.Cfg, r.T.Status) == "started" {
			status = unstartedStatus(pi.Cfg)
		}
		s.ix.mu.RUnlock()
	} else if _, ok := pi.Cfg.StatusByName(status); !ok {
		return nil, unknownStatus(pi.Cfg, status)
	}
	o := stamp(&Op{Op: "release", ID: strings.ToUpper(id), Status: status, Steal: force}, c, s.now())
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, err
	}
	w := writeOut(o.ID, res)
	w.Status = status
	return w, nil
}

// TransitionIn is ka_transition's input.
type TransitionIn struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Resolution string `json:"resolution,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// Transition changes status. Closing requires a resolution and summary,
// recorded as a resolution comment in the same commit.
func (s *Service) Transition(ctx context.Context, c Caller, in TransitionIn) (*WriteOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	st, pi, err := s.storeOf(in.ID)
	if err != nil {
		return nil, err
	}
	stt, ok := pi.Cfg.StatusByName(in.Status)
	if !ok {
		return nil, unknownStatus(pi.Cfg, in.Status)
	}
	now := s.now()
	o := stamp(&Op{Op: "transition", ID: strings.ToUpper(in.ID), Status: in.Status}, c, now)
	if closedCategory(stt.Category) {
		res := in.Resolution
		if res == "" {
			if stt.Category == "completed" {
				res = "done"
			} else {
				res = "canceled"
			}
		}
		if !slices.Contains(kaspec.Resolutions, res) {
			return nil, Errf(CodeInvalid, "resolutions: "+strings.Join(kaspec.Resolutions, ", "), "%q is not a resolution", res)
		}
		if strings.TrimSpace(in.Summary) == "" {
			return nil, Errf(CodeResolutionRequired, "pass summary: one or two sentences on what was done (or why it was canceled)", "closing %s needs a summary", o.ID)
		}
		o.CommentName = kaspec.CommentFilename(now, kaspec.RandomBase32(4))
		o.Comment = &kaspec.Comment{Type: "resolution", Author: c.Actor, OnBehalfOf: c.OnBehalfOf, Session: c.Session,
			Created: now, Resolution: res, Body: strings.TrimSpace(in.Summary)}
	} else if in.Summary != "" {
		o.CommentName = kaspec.CommentFilename(now, kaspec.RandomBase32(4))
		o.Comment = &kaspec.Comment{Type: "note", Author: c.Actor, OnBehalfOf: c.OnBehalfOf, Session: c.Session, Created: now, Body: strings.TrimSpace(in.Summary)}
	}
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, err
	}
	w := writeOut(o.ID, res)
	w.Status = in.Status
	if o.Comment != nil {
		w.Comment = o.CommentName
	}
	if closedCategory(stt.Category) {
		s.ix.mu.RLock()
		v := s.view()
		for _, b := range s.ix.blocks[o.ID] {
			if br, ok := s.ix.tickets[b]; ok && !closedCategory(category(br.Project.Cfg, br.T.Status)) && !v.isBlocked(br) {
				w.Unblocked = append(w.Unblocked, b)
			}
		}
		s.ix.mu.RUnlock()
	}
	return w, nil
}

// CommentIn is ka_comment's input.
type CommentIn struct {
	ID         string `json:"id"`
	Type       string `json:"type,omitempty"` // note (default), progress, decision
	Body       string `json:"body"`
	Supersedes string `json:"supersedes,omitempty"`
	Retracts   string `json:"retracts,omitempty"`
}

// Comment adds a note, progress or decision comment.
func (s *Service) Comment(ctx context.Context, c Caller, in CommentIn) (*WriteOut, error) {
	typ := in.Type
	if typ == "" {
		typ = "note"
	}
	if !slices.Contains([]string{"note", "progress", "decision"}, typ) {
		return nil, Errf(CodeInvalid, "use note, progress or decision; questions go through ka_ask", "comment type %q is not allowed here", typ)
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, Errf(CodeInvalid, "", "body is required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	st, pi, err := s.storeOf(in.ID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if typ == "progress" && pi.Cfg.ProgressMinInterval > 0 {
		s.ix.mu.RLock()
		r, ok := s.ix.tickets[strings.ToUpper(in.ID)]
		var last string
		if ok {
			for _, ce := range r.Comments {
				if ce.Comment != nil && ce.Comment.Type == "progress" && ce.Comment.Author == c.Actor && ce.Comment.Created > last {
					last = ce.Comment.Created
				}
			}
		}
		s.ix.mu.RUnlock()
		if lt, err := parseTS(last); err == nil {
			if wait := pi.Cfg.ProgressMinInterval - s.Now().Sub(lt); wait > 0 {
				return nil, Errf(CodeThrottled, fmt.Sprintf("fold this into your next progress note (allowed again in %ds)", int(wait.Seconds()+0.999)), "progress notes on %s are limited to one per %s", strings.ToUpper(in.ID), pi.Cfg.ProgressMinInterval).With("retry_after_s", int(wait.Seconds()+0.999))
			}
		}
	}
	o := stamp(&Op{Op: "comment", ID: strings.ToUpper(in.ID)}, c, now)
	o.CommentName = kaspec.CommentFilename(now, kaspec.RandomBase32(4))
	o.Comment = &kaspec.Comment{Type: typ, Author: c.Actor, OnBehalfOf: c.OnBehalfOf, Session: c.Session, Created: now,
		Supersedes: in.Supersedes, Retracts: in.Retracts, Body: strings.TrimSpace(in.Body)}
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, err
	}
	w := writeOut(o.ID, res)
	w.Comment = o.CommentName
	return w, nil
}

// CheckIn is ka_check's input.
type CheckIn struct {
	ID    string      `json:"id"`
	Items []CheckItem `json:"items"`
}

// CheckItems marks acceptance items done or not done.
func (s *Service) CheckItems(ctx context.Context, c Caller, in CheckIn) (*WriteOut, string, error) {
	s.ix.catchUp()
	if len(in.Items) == 0 {
		return nil, "", Errf(CodeInvalid, "pass items: [{index: 0, done: true}] or [{text: \"...\", done: true}]", "no items")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	st, _, err := s.storeOf(in.ID)
	if err != nil {
		return nil, "", err
	}
	o := stamp(&Op{Op: "check", ID: strings.ToUpper(in.ID), Check: in.Items}, c, s.now())
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, "", err
	}
	s.ix.mu.RLock()
	progress := ""
	if r, ok := s.ix.tickets[o.ID]; ok {
		progress = s.summary(s.view(), r).Acceptance
	}
	s.ix.mu.RUnlock()
	return writeOut(o.ID, res), progress, nil
}

// AskIn is ka_ask's input.
type AskIn struct {
	ID       string        `json:"id"`
	Question string        `json:"question"`
	Options  []string      `json:"options,omitempty"`
	To       string        `json:"to,omitempty"`
	Kind     string        `json:"kind,omitempty"` // question (default) or approval
	Wait     time.Duration `json:"-"`
}

// AskOut reports a question and, if waited for, its answer.
type AskOut struct {
	WriteOut
	Question string      `json:"question"`
	Answered bool        `json:"answered"`
	Answer   *CommentOut `json:"answer,omitempty"`
}

// Ask records a question for a human; with Wait it blocks until answered.
func (s *Service) Ask(ctx context.Context, c Caller, in AskIn) (*AskOut, error) {
	if strings.TrimSpace(in.Question) == "" {
		return nil, Errf(CodeInvalid, "", "question is required")
	}
	opts := in.Options
	if in.Kind == "approval" && len(opts) == 0 {
		opts = []string{"Approve", "Reject"}
	}
	to := in.To
	if to == "" && strings.HasPrefix(c.Actor, "agent/") {
		to = c.OnBehalfOf
	}
	s.writeMu.Lock()
	s.ix.catchUp()
	st, _, err := s.storeOf(in.ID)
	if err != nil {
		s.writeMu.Unlock()
		return nil, err
	}
	now := s.now()
	o := stamp(&Op{Op: "comment", ID: strings.ToUpper(in.ID)}, c, now)
	o.CommentName = kaspec.CommentFilename(now, kaspec.RandomBase32(4))
	o.Comment = &kaspec.Comment{Type: "question", Author: c.Actor, OnBehalfOf: c.OnBehalfOf, Session: c.Session, Created: now,
		To: to, Options: opts, Body: strings.TrimSpace(in.Question)}
	res, err := s.do(ctx, st, o, true)
	s.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &AskOut{WriteOut: *writeOut(o.ID, res), Question: o.CommentName}
	if in.Wait > 0 {
		ans, err := s.waitAnswer(ctx, st, o.ID, o.CommentName, in.Wait)
		if err != nil {
			return out, err
		}
		if ans != nil {
			out.Answered, out.Answer = true, ans
		}
	}
	return out, nil
}

func (s *Service) findAnswer(id, question string) *CommentOut {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	r, ok := s.ix.tickets[id]
	if !ok {
		return nil
	}
	for _, ce := range r.Comments {
		if ce.Comment != nil && ce.Comment.Answers == question {
			co := recCommentOut(r, ce)
			return &co
		}
	}
	return nil
}

func (s *Service) waitAnswer(ctx context.Context, st *Store, id, question string, d time.Duration) (*CommentOut, error) {
	deadline := s.Now().Add(d)
	for {
		if a := s.findAnswer(id, question); a != nil {
			return a, nil
		}
		if !s.Now().Before(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(s.PollStep, time.Until(deadline)+time.Millisecond)):
		}
		s.writeMu.Lock()
		if remote, _ := st.upstream(ctx); remote != "" && !s.NoPush {
			st.Sync(ctx, s.hooks(st))
		}
		s.writeMu.Unlock()
		// Answers may also arrive through another local process.
		s.ix.mu.RLock()
		dir := ""
		if r, ok := s.ix.tickets[id]; ok {
			dir = r.Dir
		}
		s.ix.mu.RUnlock()
		if dir != "" {
			s.ix.refresh(st, []string{dir + "/ticket.md"})
		}
	}
}

// AnswerIn answers (or approves/rejects) an open question.
type AnswerIn struct {
	ID       string `json:"id"`
	Question string `json:"question,omitempty"` // comment filename; default: the newest open question
	Choice   *int   `json:"choice,omitempty"`
	Text     string `json:"text,omitempty"`
	Approved *bool  `json:"approved,omitempty"` // set for ka approve / ka reject
}

// Answer records an answer or approval.
func (s *Service) Answer(ctx context.Context, c Caller, in AnswerIn) (*WriteOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	st, _, err := s.storeOf(in.ID)
	if err != nil {
		return nil, err
	}
	s.ix.mu.RLock()
	r, err := s.rec(in.ID)
	var q *kaspec.CommentEntry
	if err == nil {
		open := openQuestions(r)
		if in.Question != "" {
			for _, ce := range r.Comments {
				if ce.Name == in.Question || strings.HasPrefix(ce.Name, in.Question) {
					q = ce
				}
			}
			if q == nil {
				err = Errf(CodeNoQuestion, "", "%s has no comment %s", r.ID, in.Question)
			}
		} else if len(open) > 0 {
			q = open[len(open)-1]
		} else {
			err = Errf(CodeNoQuestion, "nothing to answer; add a note with ka comment instead", "%s has no open question", r.ID)
		}
	}
	s.ix.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	now := s.now()
	o := stamp(&Op{Op: "comment", ID: strings.ToUpper(in.ID)}, c, now)
	o.CommentName = kaspec.CommentFilename(now, kaspec.RandomBase32(4))
	cm := &kaspec.Comment{Author: c.Actor, OnBehalfOf: c.OnBehalfOf, Session: c.Session, Created: now, Answers: q.Name, Body: strings.TrimSpace(in.Text)}
	if in.Approved != nil {
		cm.Type, cm.Approved = "approval", in.Approved
	} else {
		cm.Type = "answer"
		if in.Choice != nil {
			if q.Comment == nil || *in.Choice < 0 || *in.Choice >= len(q.Comment.Options) {
				return nil, Errf(CodeInvalid, fmt.Sprintf("choose 0 to %d", max(0, len(q.Comment.Options)-1)), "choice %d is not an option of %s", *in.Choice, q.Name)
			}
			ch := int64(*in.Choice)
			cm.Choice = &ch
		} else if cm.Body == "" {
			return nil, Errf(CodeInvalid, "pass --choice N or a text answer", "an answer needs a choice or text")
		}
	}
	o.Comment = cm
	res, err := s.do(ctx, st, o, true)
	if err != nil {
		return nil, err
	}
	w := writeOut(o.ID, res)
	w.Comment = o.CommentName
	return w, nil
}

// InboxItem is one entry in an inbox.
type InboxItem struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Comment  CommentOut `json:"comment"`
	Question string     `json:"question,omitempty"` // for answers: the question's filename
	// Restorable: a preserved operation that POST /tickets/{id}/restore can re-apply.
	Restorable bool `json:"restorable,omitempty"`
}

// InboxOut is ka_inbox's output.
type InboxOut struct {
	Answers   []InboxItem `json:"answers"`   // answers and approvals to the caller's questions
	Questions []InboxItem `json:"questions"` // open questions addressed to the caller
}

// Inbox lists answers to the caller's questions since a time, and open
// questions addressed to the caller (or a role they hold).
func (s *Service) Inbox(c Caller, since string) *InboxOut {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	out := &InboxOut{Answers: []InboxItem{}, Questions: []InboxItem{}}
	roles := map[string]bool{c.Actor: true}
	for role, members := range s.ix.actors.Roles {
		if slices.Contains(members, c.Actor) {
			roles["role/"+role] = true
		}
	}
	for _, id := range sortedKeys(s.ix.tickets) {
		r := s.ix.tickets[id]
		mine := map[string]bool{}
		for _, ce := range r.Comments {
			if ce.Comment != nil && ce.Comment.Type == "question" && ce.Comment.Author == c.Actor {
				mine[ce.Name] = true
			}
		}
		for _, ce := range r.Comments {
			k := ce.Comment
			if k == nil || k.Answers == "" || !mine[k.Answers] || (since != "" && k.Created < since) {
				continue
			}
			out.Answers = append(out.Answers, InboxItem{ID: id, Title: r.T.Title, Comment: recCommentOut(r, ce), Question: k.Answers})
		}
		for _, q := range openQuestions(r) {
			if roles[q.Comment.To] || (q.Comment.To == "" && r.T.Owner == c.Actor) {
				out.Questions = append(out.Questions, InboxItem{ID: id, Title: r.T.Title, Comment: commentOut(q)})
			}
		}
	}
	return out
}

func parseTS(s string) (time.Time, error) { return time.Parse("2006-01-02T15:04:05Z", s) }
