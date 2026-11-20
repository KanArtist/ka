package engine

import (
	"sort"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// Initiatives and epics (spec §4.5): container tickets that group work.

const (
	// CodeHierarchy: a parent breaks the nesting rules of initiatives and epics.
	CodeHierarchy = "hierarchy_invalid"
	// CodeContainersUnguarded: the store does not require a ka that knows
	// the container kinds, so creating one would break older clients.
	CodeContainersUnguarded = "containers_unguarded"
)

func isContainer(r *Rec) bool { return kaspec.IsContainerKind(r.T.Kind) }

// kind returns the kind of a ticket in the index (ok false when unknown).
func (e ixEnv) kind(id string) (string, bool) {
	e.ix.mu.RLock()
	defer e.ix.mu.RUnlock()
	if r, ok := e.ix.tickets[e.ix.canon(id)]; ok {
		return r.T.Kind, true
	}
	return "", false
}

// childKinds returns the children of a ticket and their kinds.
func (e ixEnv) childKinds(id string) map[string]string {
	e.ix.mu.RLock()
	defer e.ix.mu.RUnlock()
	out := map[string]string{}
	for _, c := range e.ix.children[id] {
		if r, ok := e.ix.tickets[c]; ok {
			out[c] = r.T.Kind
		}
	}
	return out
}

// containersGuard refuses a store that does not require a ka that knows
// the container kinds (requires_ka >= 1.3.0), like slugs (spec §4.3).
func (e ixEnv) containersGuard() error {
	e.ix.mu.RLock()
	ks := e.ix.ks[e.s]
	ok := ks != nil && kaspec.ContainersGuarded(ks.File)
	e.ix.mu.RUnlock()
	if ok {
		return nil
	}
	return unguardedErr(e.s.Name)
}

func unguardedErr(store string) *Error {
	return Errf(CodeContainersUnguarded, "once every client of the store runs ka "+kaspec.ContainersMinKa+" or newer, add `requires_ka: "+kaspec.ContainersMinKa+"` to its .ka/store.yml, commit and push, then try again",
		"store %s does not require ka %s, so older clients would fail to read initiatives and epics", store, kaspec.ContainersMinKa)
}

// checkContainers enforces the container rules on a ticket an operation
// creates, or whose parent or kind it changes (kindChanged: the kind is new
// or changed, so the store guard and the ticket's children are checked too).
func checkContainers(env applyEnv, t *kaspec.Ticket, kindChanged bool) error {
	if kindChanged && kaspec.IsContainerKind(t.Kind) {
		if err := env.containersGuard(); err != nil {
			return err
		}
	}
	if t.Parent != "" {
		if pk, ok := env.kind(t.Parent); ok {
			if why := kaspec.HierarchyProblem(t.Kind, pk); why != "" {
				return Errf(CodeHierarchy, kaspec.HierarchyFix(t.Kind), "%s: %s is %s", why, t.Parent, kaspec.KindPhrase(pk))
			}
		}
	}
	if kindChanged {
		kids := env.childKinds(t.ID)
		ids := sortedKeys(kids)
		for _, c := range ids {
			if why := kaspec.HierarchyProblem(kids[c], t.Kind); why != "" {
				return Errf(CodeHierarchy, "move "+c+" first (ka edit "+c+" --set parent=...)", "%s cannot become %s: its child %s would break the rule (%s)", t.ID, kaspec.KindPhrase(t.Kind), c, why)
			}
		}
	}
	return nil
}

// ---- descendants ---------------------------------------------------------------

// descendants returns every ticket below id (transitively, without id
// itself). The walk visits each ticket once, so a parent cycle ends it.
// Results are memoized per view, so a query term costs one walk per call.
func (v view) descendants(id string) map[string]bool {
	id = v.ix.canon(id)
	if v.memo != nil {
		if d, ok := v.memo.desc[id]; ok {
			return d
		}
	}
	out := map[string]bool{}
	stack := append([]string(nil), v.ix.children[id]...)
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[c] || c == id {
			continue
		}
		out[c] = true
		stack = append(stack, v.ix.children[c]...)
	}
	if v.memo != nil {
		if v.memo.desc == nil {
			v.memo.desc = map[string]map[string]bool{}
		}
		v.memo.desc[id] = out
	}
	return out
}

// viewMemo caches derived sets for one query evaluation.
type viewMemo struct {
	desc map[string]map[string]bool
}

// under reports whether r is a descendant of any of the given tickets.
func (v view) under(r *Rec, ids []string) bool {
	for _, id := range ids {
		if v.descendants(id)[r.ID] {
			return true
		}
	}
	return false
}

// ---- rollups and trees ---------------------------------------------------------

// Rollup summarizes a container's descendants that are work (not
// containers), through nested epics. Done, Canceled, InProgress, Blocked
// and Ready count disjoint groups; tickets in triage or backlog, or claimed
// but not started, are only in Total.
type Rollup struct {
	Total      int `json:"total" doc:"Work tickets under the container, at any depth."`
	Done       int `json:"done" doc:"Of those, in a completed status."`
	Canceled   int `json:"canceled" doc:"In a canceled status."`
	InProgress int `json:"in_progress" doc:"In a started status and not blocked."`
	Blocked    int `json:"blocked" doc:"Open and blocked (a blocked status or an open blocker)."`
	Ready      int `json:"ready" doc:"Claimable now (is:ready)."`
	// Direct children, of any kind.
	Children     int `json:"children" doc:"Direct children of any kind."`
	ChildrenOpen int `json:"children_open" doc:"Direct children that are open."`
}

func (a *Rollup) add(b *Rollup) {
	a.Total += b.Total
	a.Done += b.Done
	a.Canceled += b.Canceled
	a.InProgress += b.InProgress
	a.Blocked += b.Blocked
	a.Ready += b.Ready
}

// count adds one work ticket to the rollup.
func (a *Rollup) count(v view, r *Rec) {
	a.Total++
	switch cat := category(r.Project.Cfg, r.T.Status); {
	case cat == "completed":
		a.Done++
	case cat == "canceled":
		a.Canceled++
	case v.isBlocked(r):
		a.Blocked++
	case cat == "started":
		a.InProgress++
	case v.is(r, "ready"):
		a.Ready++
	}
}

// TreeIn is ka_tree's input.
type TreeIn struct {
	ID         string `json:"id,omitempty" doc:"An initiative, epic or any parent ticket; empty: every initiative and top-level epic of the project."`
	Project    string `json:"project,omitempty" doc:"Without id: the project (default: the workspace's; else every project)."`
	Depth      int    `json:"depth,omitempty" doc:"Levels of children to show below each top node; 0 means all."`
	HideClosed bool   `json:"hide_closed,omitempty" doc:"Leave out closed tickets (rollups still count them)."`
}

// TreeNode is one ticket in a tree.
type TreeNode struct {
	ID       string      `json:"id"`
	Ref      string      `json:"ref"`
	Title    string      `json:"title"`
	Kind     string      `json:"kind,omitempty"`
	Status   string      `json:"status"`
	Category string      `json:"category,omitempty" doc:"The status category: triage, backlog, unstarted, started, completed or canceled."`
	Priority string      `json:"priority,omitempty"`
	Project  string      `json:"project"`
	Claim    string      `json:"claimed_by,omitempty" doc:"Who holds the claim."`
	Blocked  bool        `json:"blocked,omitempty"`
	Rollup   *Rollup     `json:"rollup,omitempty" doc:"Containers only: counts over every work ticket below."`
	Children []*TreeNode `json:"children,omitempty"`
	// More counts direct children left out (by depth or hide_closed).
	More int `json:"more,omitempty" doc:"Direct children not shown (depth limit or hide_closed)."`
}

// TreeOut is ka_tree's output.
type TreeOut struct {
	Nodes []*TreeNode `json:"nodes" doc:"The ticket asked for, or every initiative and top-level epic."`
}

// Tree returns the subtree under a ticket, or the initiatives and top-level
// epics of a project, with a rollup on every container.
func (s *Service) Tree(in TreeIn) (*TreeOut, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	v := s.view()
	var roots []*Rec
	if in.ID != "" {
		r, err := s.rec(in.ID)
		if err != nil {
			return nil, err
		}
		roots = []*Rec{r}
	} else {
		key := in.Project
		if key == "" {
			key = s.WS.Project
		}
		if key != "" {
			pi, err := s.resolveProject(key)
			if err != nil {
				return nil, err
			}
			key = pi.Key
		}
		for _, r := range s.ix.tickets {
			if key != "" && r.Project.Key != key {
				continue
			}
			if r.T.Kind == kaspec.KindInitiative || (r.T.Kind == kaspec.KindEpic && r.T.Parent == "") {
				roots = append(roots, r)
			}
		}
		sortTree(roots)
	}
	b := treeBuilder{s: s, v: v, in: in, rollups: map[string]*Rollup{}, seen: map[string]bool{}}
	out := &TreeOut{Nodes: []*TreeNode{}}
	for _, r := range roots {
		if in.ID == "" && in.HideClosed && closedCategory(category(r.Project.Cfg, r.T.Status)) {
			continue
		}
		out.Nodes = append(out.Nodes, b.node(r, 0))
	}
	return out, nil
}

type treeBuilder struct {
	s       *Service
	v       view
	in      TreeIn
	rollups map[string]*Rollup // by ticket: work below it
	seen    map[string]bool    // on the current path (cycle guard)
}

// below computes the rollup of the work under r, once per ticket.
func (b *treeBuilder) below(r *Rec) *Rollup {
	if ru, ok := b.rollups[r.ID]; ok {
		return ru
	}
	ru := &Rollup{}
	b.rollups[r.ID] = ru // a cycle back to r sees a partial, finite rollup
	for _, c := range b.s.ix.children[r.ID] {
		cr, ok := b.s.ix.tickets[c]
		if !ok {
			continue
		}
		ru.Children++
		if !closedCategory(category(cr.Project.Cfg, cr.T.Status)) {
			ru.ChildrenOpen++
		}
		if !isContainer(cr) {
			ru.count(b.v, cr)
		}
		ru.add(b.below(cr))
	}
	return ru
}

func (b *treeBuilder) node(r *Rec, depth int) *TreeNode {
	t := r.T
	n := &TreeNode{ID: r.ID, Ref: r.Ref(), Title: t.Title, Kind: t.Kind, Status: t.Status, Category: category(r.Project.Cfg, t.Status),
		Priority: t.Priority, Project: r.Project.Key, Blocked: !closedCategory(category(r.Project.Cfg, t.Status)) && b.v.isBlocked(r)}
	if t.Claim != nil {
		n.Claim = t.Claim.By
	}
	ru := b.below(r)
	if isContainer(r) {
		cp := *ru
		n.Rollup = &cp
	}
	if b.seen[r.ID] {
		return n
	}
	b.seen[r.ID] = true
	defer delete(b.seen, r.ID)
	var kids []*Rec
	for _, c := range b.s.ix.children[r.ID] {
		if cr, ok := b.s.ix.tickets[c]; ok {
			kids = append(kids, cr)
		}
	}
	sortTree(kids)
	for _, cr := range kids {
		if (b.in.Depth > 0 && depth+1 > b.in.Depth) || (b.in.HideClosed && closedCategory(category(cr.Project.Cfg, cr.T.Status))) || b.seen[cr.ID] {
			n.More++
			continue
		}
		n.Children = append(n.Children, b.node(cr, depth+1))
	}
	return n
}

// sortTree orders siblings: initiatives, then epics, then other tickets;
// within each, open before closed, then by priority and age.
func sortTree(rs []*Rec) {
	rank := func(r *Rec) int {
		k := 2
		switch r.T.Kind {
		case kaspec.KindInitiative:
			k = 0
		case kaspec.KindEpic:
			k = 1
		}
		if closedCategory(category(r.Project.Cfg, r.T.Status)) {
			k += 3
		}
		return k
	}
	sortRecs(rs, []string{"priority", "created"})
	sort.SliceStable(rs, func(i, j int) bool { return rank(rs[i]) < rank(rs[j]) })
}
