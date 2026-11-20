package engine

import (
	"strings"
	"testing"
)

const containersStore = "spec: 1\nname: test\nrequires_ka: 1.3.0\n"

func TestContainersNeedRequiresKa(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	c := agent("claude")
	_, err := a.Create(ctx, c, []CreateIn{{Title: "GDPR", Kind: "initiative"}})
	if !IsCode(err, CodeContainersUnguarded) || !strings.Contains(AsError(err).Fix, "ka store require 1.3.0") {
		t.Fatalf("initiative in an unguarded store: %v", err)
	}
	id := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Work"}})).Created[0].ID
	if _, err := a.Update(ctx, c, []UpdateIn{{ID: id, Set: map[string]any{"kind": "epic"}}}); !IsCode(err, CodeContainersUnguarded) {
		t.Fatalf("kind epic in an unguarded store: %v", err)
	}
	if _, err := a.InitiativeFromLabel(ctx, c, FromLabelIn{Label: "gdpr", DryRun: true}); !IsCode(err, CodeContainersUnguarded) {
		t.Fatalf("from-label in an unguarded store: %v", err)
	}
}

func TestHierarchyRules(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	c := agent("claude")
	create := func(in CreateIn) string {
		t.Helper()
		return must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{in})).Created[0].ID
	}
	ini := create(CreateIn{Title: "GDPR", Kind: "initiative"})
	epic := create(CreateIn{Title: "Consent", Kind: "epic", Parent: ini})
	work := create(CreateIn{Title: "Banner", Kind: "feature", Parent: epic})
	create(CreateIn{Title: "Sub-issue", Parent: work})
	create(CreateIn{Title: "Directly under the initiative", Parent: ini})
	lone := create(CreateIn{Title: "Top-level epic", Kind: "epic"})

	bad := []CreateIn{
		{Title: "Nested initiative", Kind: "initiative", Parent: ini},
		{Title: "Epic under an epic", Kind: "epic", Parent: epic},
		{Title: "Epic under work", Kind: "epic", Parent: work},
		{Title: "Initiative under work", Kind: "initiative", Parent: work},
	}
	for _, in := range bad {
		if _, err := a.Create(ctx, c, []CreateIn{in}); !IsCode(err, CodeHierarchy) || AsError(err).Fix == "" {
			t.Errorf("%s: %v", in.Title, err)
		}
	}
	for _, u := range []UpdateIn{
		{ID: ini, Set: map[string]any{"parent": lone}},
		{ID: lone, Set: map[string]any{"parent": work}},
		{ID: ini, Set: map[string]any{"kind": "feature"}},  // its child epic would sit under work
		{ID: epic, Set: map[string]any{"kind": "feature"}}, // fine: work may sit under an initiative, and hold work
	} {
		_, err := a.Update(ctx, c, []UpdateIn{u})
		if u.ID == epic {
			if err != nil {
				t.Errorf("epic to feature: %v", err)
			}
			continue
		}
		if !IsCode(err, CodeHierarchy) {
			t.Errorf("update %v: %v", u.Set, err)
		}
	}
	// A work ticket may move under an epic, and an epic under an initiative.
	must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: lone, Set: map[string]any{"parent": ini}}}))
	// References with a slug resolve.
	must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: work, Set: map[string]any{"parent": lone + "-top-level-epic"}}}))
	if diags := a.Validate(a.Now()); hasCode(diags, "hierarchy_invalid") || hasCode(diags, "containers_unguarded") {
		t.Fatalf("check: %v", diags)
	}
}

func TestContainersAreNotWork(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	c := agent("claude")
	ids := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{
		{Title: "Empty initiative", Kind: "initiative", Status: "ready", Priority: "P0"},
		{Title: "Empty epic", Kind: "epic", Status: "ready", Priority: "P0"},
	})).Created
	if _, err := a.Next(ctx, c, NextIn{Peek: true}); !IsCode(err, CodeNoCandidates) {
		t.Fatalf("next handed out a container: %v", err)
	}
	if out := must[*SearchOut](t)(a.Search(SearchIn{Query: "is:ready"})); out.Total != 0 {
		t.Fatalf("is:ready lists containers: %+v", out.Tickets)
	}
	if out := must[*SearchOut](t)(a.Search(SearchIn{Query: "is:container"})); out.Total != 2 {
		t.Fatalf("is:container: %+v", out.Tickets)
	}
	// An orchestrator may still claim one explicitly.
	if co := must[*ClaimOut](t)(a.Claim(ctx, c, ids[0].ID, false)); co.Status != "in_progress" {
		t.Fatalf("claim a container: %+v", co)
	}
}

func TestScopesTreeAndNextWithin(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	c := agent("claude")
	create := func(in CreateIn) string {
		t.Helper()
		return must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{in})).Created[0].ID
	}
	ini := create(CreateIn{Title: "GDPR", Kind: "initiative", Status: "in_progress"})
	e1 := create(CreateIn{Title: "Consent banner", Kind: "epic", Parent: ini})
	e2 := create(CreateIn{Title: "Data export", Kind: "epic", Parent: ini})
	t1 := create(CreateIn{Title: "Show the banner", Parent: e1, Status: "ready", Priority: "P1"})
	t2 := create(CreateIn{Title: "Store the choice", Parent: e1, Status: "ready", Priority: "P0"})
	t3 := create(CreateIn{Title: "Export as JSON", Parent: e2, Status: "ready", Priority: "P0"})
	t4 := create(CreateIn{Title: "Retention policy", Parent: ini, Status: "backlog"})
	t5 := create(CreateIn{Title: "Blocked export", Parent: e2, Status: "ready", BlockedBy: []string{t3}})
	create(CreateIn{Title: "Unrelated", Status: "ready", Priority: "P0"})
	must[*WriteOut](t)(a.Transition(ctx, c, TransitionIn{ID: t2, Status: "done", Summary: "Stored."}))

	search := func(q string) map[string]bool {
		t.Helper()
		out := must[*SearchOut](t)(a.Search(SearchIn{Query: q, All: true, Limit: -1}))
		m := map[string]bool{}
		for _, s := range out.Tickets {
			m[s.ID] = true
		}
		return m
	}
	if got := search("initiative:" + strings.ToLower(ini) + "-gdpr"); len(got) != 7 || got[ini] {
		t.Fatalf("initiative: %v", got)
	}
	if got := search("epic:" + e1); len(got) != 2 || !got[t1] || !got[t2] {
		t.Fatalf("epic: %v", got)
	}
	if got := search("initiative:" + ini + " -epic:" + e1 + " -is:container"); len(got) != 3 || !got[t3] || !got[t4] || !got[t5] {
		t.Fatalf("initiative minus an epic: %v", got)
	}
	if got := search("is:epic"); len(got) != 2 || !got[e1] || !got[e2] {
		t.Fatalf("is:epic: %v", got)
	}
	if got := search("parent:" + ini); len(got) != 3 || !got[t4] {
		t.Fatalf("parent: stays direct children: %v", got)
	}

	// next within an epic only hands out its tickets.
	n := must[*NextOut](t)(a.Next(ctx, c, NextIn{Within: e2, Peek: true}))
	if n.Ticket.ID != t3 || n.Remaining != 0 {
		t.Fatalf("next within e2: %s, %d more", n.Ticket.ID, n.Remaining)
	}
	n = must[*NextOut](t)(a.Next(ctx, c, NextIn{Within: ini}))
	if n.Ticket.ID != t3 || !n.Claimed {
		t.Fatalf("next within the initiative: %+v", n)
	}
	if _, err := a.Next(ctx, c, NextIn{Within: "JK-ZZZZZ"}); !IsCode(err, CodeNotFound) {
		t.Fatalf("next within a missing ticket: %v", err)
	}

	tree := must[*TreeOut](t)(a.Tree(TreeIn{ID: ini}))
	root := tree.Nodes[0]
	want := Rollup{Total: 5, Done: 1, InProgress: 1, Blocked: 1, Ready: 1, Children: 3, ChildrenOpen: 3}
	if root.Rollup == nil || *root.Rollup != want {
		t.Fatalf("initiative rollup %+v, want %+v", root.Rollup, want)
	}
	if len(root.Children) != 3 || root.Children[0].Kind != "epic" || root.Children[1].Kind != "epic" || root.Children[2].ID != t4 {
		t.Fatalf("children order: %+v", root.Children)
	}
	ex := root.Children[0]
	if ex.ID != e2 {
		ex = root.Children[1]
	}
	if ex.Rollup.Total != 2 || ex.Rollup.InProgress != 1 || ex.Rollup.Blocked != 1 {
		t.Fatalf("epic rollup: %+v", ex.Rollup)
	}
	for _, k := range ex.Children {
		if k.ID == t3 && k.Claim != "agent/claude" {
			t.Fatalf("claim holder missing: %+v", k)
		}
		if k.ID == t5 && !k.Blocked {
			t.Fatalf("blocked flag missing: %+v", k)
		}
	}
	tree = must[*TreeOut](t)(a.Tree(TreeIn{ID: ini, Depth: 1, HideClosed: true}))
	if r := tree.Nodes[0]; len(r.Children) != 3 || r.Children[0].Children != nil || r.Children[0].More != 2 {
		t.Fatalf("depth 1: %+v", r.Children[0])
	}
	tree = must[*TreeOut](t)(a.Tree(TreeIn{ID: e1, HideClosed: true}))
	if r := tree.Nodes[0]; len(r.Children) != 1 || r.More != 1 || r.Rollup.Done != 1 {
		t.Fatalf("hide closed: %+v", r)
	}
	tree = must[*TreeOut](t)(a.Tree(TreeIn{Depth: 1}))
	if len(tree.Nodes) != 1 || tree.Nodes[0].ID != ini {
		t.Fatalf("project tree: %+v", tree.Nodes)
	}

	// A parent cycle (made by hand-style edits) ends every walk.
	x := create(CreateIn{Title: "Cycle A", Status: "ready"})
	y := create(CreateIn{Title: "Cycle B", Status: "ready", Parent: x})
	must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: x, Set: map[string]any{"parent": y}}}))
	if got := search("epic:" + x); len(got) != 1 || !got[y] {
		t.Fatalf("descendants with a cycle: %v", got)
	}
	must[*TreeOut](t)(a.Tree(TreeIn{ID: x}))
}

func TestContainerStatusWarnings(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	c := agent("claude")
	ini := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "GDPR", Kind: "initiative", Status: "in_progress"}})).Created[0].ID
	w := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Only ticket", Parent: ini, Status: "ready"}})).Created[0].ID
	if hasCode(a.Validate(a.Now()), "container_status") {
		t.Fatal("container_status with open work")
	}
	must[*WriteOut](t)(a.Transition(ctx, c, TransitionIn{ID: w, Status: "done", Summary: "Done."}))
	if !hasCode(a.Validate(a.Now()), "container_status") {
		t.Fatal("no container_status for an open initiative whose work is done")
	}
	must[*WriteOut](t)(a.Transition(ctx, c, TransitionIn{ID: ini, Status: "done", Summary: "All done."}))
	if hasCode(a.Validate(a.Now()), "container_status") {
		t.Fatal("container_status after closing it")
	}
}

func TestInitiativeFromLabel(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	c := agent("claude")
	create := func(in CreateIn) string {
		t.Helper()
		return must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{in})).Created[0].ID
	}
	other := create(CreateIn{Title: "Other initiative", Kind: "initiative"})
	epic := create(CreateIn{Title: "Consent", Kind: "epic"})
	busyEpic := create(CreateIn{Title: "Elsewhere", Kind: "epic", Parent: other})
	host := create(CreateIn{Title: "Host ticket"})
	free := create(CreateIn{Title: "Free", Labels: []string{"gdpr", "ui"}})
	viaEpic := create(CreateIn{Title: "In the epic", Labels: []string{"gdpr"}, Parent: epic})
	viaEpic2 := create(CreateIn{Title: "Also in the epic", Labels: []string{"GDPR"}, Parent: epic})
	sub := create(CreateIn{Title: "Sub-issue", Labels: []string{"gdpr"}, Parent: host})
	blocked := create(CreateIn{Title: "Under a busy epic", Labels: []string{"gdpr"}, Parent: busyEpic})

	dry := must[*FromLabelOut](t)(a.InitiativeFromLabel(ctx, c, FromLabelIn{Label: "gdpr", Title: "GDPR", DryRun: true}))
	if !dry.Created || dry.Initiative.ID != "" || len(dry.Tickets) != 6 {
		t.Fatalf("dry run: %+v", dry)
	}
	if out := must[*SearchOut](t)(a.Search(SearchIn{Query: "is:initiative"})); out.Total != 1 {
		t.Fatal("dry run wrote")
	}

	out := must[*FromLabelOut](t)(a.InitiativeFromLabel(ctx, c, FromLabelIn{Label: "gdpr", Title: "GDPR"}))
	ini := out.Initiative.ID
	if !out.Created || ini == "" || out.Error != nil {
		t.Fatalf("from-label: %+v", out)
	}
	act := map[string]FromLabelTicket{}
	for _, x := range out.Tickets {
		act[x.ID] = x
	}
	for id, want := range map[string]string{free: LabelParented, viaEpic: LabelViaEpic, viaEpic2: LabelViaEpic, sub: LabelSkipped, blocked: LabelSkipped, epic: LabelEpic} {
		if act[id].Action != want {
			t.Errorf("%s: %+v, want %s", id, act[id], want)
		}
	}
	get := func(id string) *Detail { return must[*Detail](t)(a.Get(GetIn{ID: id})) }
	if d := get(free); d.ParentRef == nil || d.ParentRef.ID != ini || len(d.Labels) != 1 || d.Labels[0] != "ui" {
		t.Fatalf("free ticket: %+v", d.Summary)
	}
	if d := get(epic); d.ParentRef == nil || d.ParentRef.ID != ini {
		t.Fatalf("epic not under the initiative: %+v", d.ParentRef)
	}
	if d := get(viaEpic2); d.ParentRef.ID != epic || len(d.Labels) != 0 {
		t.Fatalf("ticket in the epic: %+v", d.Summary)
	}
	if d := get(sub); d.ParentRef.ID != host || len(d.Labels) != 1 {
		t.Fatalf("sub-issue touched: %+v", d.Summary)
	}
	if got := must[*SearchOut](t)(a.Search(SearchIn{Query: "initiative:" + ini})); got.Total != 4 {
		t.Fatalf("initiative scope after from-label: %+v", got.Tickets)
	}

	// Running it again finds the initiative by title and changes nothing.
	head := run(t, a.WS.Stores[0].Root, "rev-parse", "HEAD")
	again := must[*FromLabelOut](t)(a.InitiativeFromLabel(ctx, c, FromLabelIn{Label: "gdpr", Title: "gdpr"}))
	if again.Created || again.Initiative.ID != ini || len(again.Tickets) != 2 {
		t.Fatalf("rerun: %+v", again)
	}
	if run(t, a.WS.Stores[0].Root, "rev-parse", "HEAD") != head {
		t.Fatal("rerun committed")
	}
}

func TestInitiativeFromLabelKeepLabel(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	c := agent("claude")
	id := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Port the installer", Labels: []string{"windows"}}})).Created[0].ID
	out := must[*FromLabelOut](t)(a.InitiativeFromLabel(ctx, c, FromLabelIn{Label: "windows", KeepLabel: true}))
	if d := must[*Detail](t)(a.Get(GetIn{ID: id})); d.ParentRef == nil || d.ParentRef.ID != out.Initiative.ID || len(d.Labels) != 1 {
		t.Fatalf("keep label: %+v", d.Summary)
	}
	again := must[*FromLabelOut](t)(a.InitiativeFromLabel(ctx, c, FromLabelIn{Label: "windows", KeepLabel: true}))
	if again.Created || len(again.Tickets) != 1 || again.Tickets[0].Action != LabelAlready {
		t.Fatalf("rerun with the label kept: %+v", again)
	}
}
