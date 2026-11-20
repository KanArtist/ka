package scenario

import (
	"fmt"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
)

// containersStore lets a store hold initiatives and epics (spec §4.5).
var containersStore = map[string]string{".ka/store.yml": "spec: 1\nname: test\nrequires_ka: 1.3.0\n"}

func createdIDs(t *testing.T, sc map[string]any) []string {
	t.Helper()
	var ids []string
	for _, c := range sc["created"].([]any) {
		ids = append(ids, digS(c.(map[string]any), "id"))
	}
	return ids
}

// Initiatives and epics: an orchestrator plans an initiative with epics and
// tickets, reads the tree, dispatches workers with ka_next within an epic,
// watches the rollup, and closes containers by hand. Nesting rules hold.
func TestInitiativesAndEpics(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, containersStore)
	orch := connect(t, testkit.Service(t, r.Clone()), "orchestrator")
	worker := connect(t, testkit.Service(t, r.Clone()), "worker")

	_, sc := orch.ok("ka_create", map[string]any{"tickets": []map[string]any{{"title": "GDPR compliance", "kind": "initiative", "status": "in_progress"}}})
	ini := createdIDs(t, sc)[0]
	_, sc = orch.ok("ka_create", map[string]any{"tickets": []map[string]any{
		{"title": "Consent banner", "kind": "epic", "parent": ini, "status": "in_progress"},
		{"title": "Data export", "kind": "epic", "parent": ini},
	}})
	epics := createdIDs(t, sc)
	_, sc = orch.ok("ka_create", map[string]any{"tickets": []map[string]any{
		{"title": "Show the banner on first visit", "parent": epics[0], "status": "ready", "priority": "P1"},
		{"title": "Store the consent choice", "parent": epics[0], "status": "ready", "priority": "P2"},
		{"title": "Export a user's data as JSON", "parent": epics[1], "status": "ready", "priority": "P0"},
		{"title": "Write the retention policy", "parent": ini, "status": "backlog"},
	}})
	work := createdIDs(t, sc)

	// Nesting rules are enforced, with a fix.
	for _, bad := range []map[string]any{
		{"title": "Nested initiative", "kind": "initiative", "parent": ini},
		{"title": "Epic in an epic", "kind": "epic", "parent": epics[0]},
		{"title": "Epic under a ticket", "kind": "epic", "parent": work[0]},
	} {
		text, _, isErr := orch.call("ka_create", map[string]any{"tickets": []map[string]any{bad}})
		if !isErr || !strings.Contains(text, "hierarchy_invalid") || !strings.Contains(text, "fix:") {
			t.Fatalf("%v: %s", bad["title"], text)
		}
	}

	// The tree shows every level with a rollup on containers.
	text, sc := orch.ok("ka_tree", map[string]any{"id": ini})
	if !strings.Contains(text, "0/4 done, 3 ready") || strings.Count(text, "\n") != 6 {
		t.Fatalf("tree:\n%s", text)
	}
	if n := dig(sc, "nodes").([]any)[0].(map[string]any); dig(n, "rollup", "total").(float64) != 4 {
		t.Fatalf("tree json: %v", n)
	}

	// A worker inside the first epic only gets its tickets, never a container.
	worker.ok("ka_sync", map[string]any{"force": true})
	_, sc = worker.ok("ka_next", map[string]any{"within": epics[0]})
	if got := digS(sc, "ticket", "id"); got != work[0] {
		t.Fatalf("next within epic: %s, want %s", got, work[0])
	}
	worker.ok("ka_transition", map[string]any{"id": work[0], "status": "done", "summary": "Banner shown on first visit."})
	_, sc = worker.ok("ka_next", map[string]any{"within": epics[0]})
	if got := digS(sc, "ticket", "id"); got != work[1] {
		t.Fatalf("second next within epic: %s", got)
	}
	worker.ok("ka_transition", map[string]any{"id": work[1], "status": "done", "summary": "Choice stored in a cookie."})
	if text, _, isErr := worker.call("ka_next", map[string]any{"within": epics[0]}); !isErr || !strings.Contains(text, "no_candidates") {
		t.Fatalf("epic exhausted: %s", text)
	}

	// The orchestrator sees the epic complete and closes it by hand.
	orch.ok("ka_sync", map[string]any{"force": true})
	text, _ = orch.ok("ka_tree", map[string]any{"id": epics[0]})
	if !strings.HasPrefix(text, epics[0]) || !strings.Contains(text, "2/2 done") {
		t.Fatalf("epic tree:\n%s", text)
	}
	if !hasDiag(orch.svc, "container_status") {
		t.Fatal("ka check does not flag an open epic whose work is done")
	}
	orch.ok("ka_transition", map[string]any{"id": epics[0], "status": "done", "summary": "Consent banner shipped."})
	text, _ = orch.ok("ka_tree", map[string]any{"id": ini, "hide_closed": true})
	if strings.Contains(text, "Consent banner") || !strings.Contains(text, "… 1 more") || !strings.Contains(text, "2/4 done") {
		t.Fatalf("tree without closed:\n%s", text)
	}
	text, _ = orch.ok("ka_search", map[string]any{"query": "initiative:" + ini + " is:ready"})
	if !strings.Contains(text, work[2]) || strings.Count(text, "\n") != 0 {
		t.Fatalf("ready work in the initiative:\n%s", text)
	}
	if hasDiag(orch.svc, "container_status") || hasDiag(orch.svc, "hierarchy_invalid") {
		t.Fatal("unexpected findings after closing the epic")
	}
}

func hasDiag(svc *engine.Service, code string) bool {
	for _, d := range svc.Validate(svc.Now()) {
		if d.Code == code {
			return true
		}
	}
	return false
}

// A label becomes an initiative. Two clones: one converts while the other
// edits a labelled ticket and files a new one; the conversion replays onto
// the other's changes, and running it again picks up the new ticket only.
func TestInitiativeFromLabelReplay(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, containersStore)
	a := testkit.Service(t, r.Clone())
	b := testkit.Service(t, r.Clone())
	ca, cb := a.CallerFor("alice", ""), b.CallerFor("bob", "")

	ids := seedTickets(t, a,
		engine.CreateIn{Title: "Consent", Kind: "epic"},
		engine.CreateIn{Title: "Cookie scan", Labels: []string{"gdpr"}, Status: "ready"},
		engine.CreateIn{Title: "Erase on request", Labels: []string{"gdpr", "api"}, Status: "ready"},
	)
	epic := ids[0]
	inEpic := seedTickets(t, a, engine.CreateIn{Title: "Banner copy", Labels: []string{"gdpr"}, Parent: epic})[0]
	b.Sync(ctx, true)

	// Bob edits a labelled ticket and files a new one, and pushes first.
	if _, err := b.Update(ctx, cb, []engine.UpdateIn{{ID: ids[1], Set: map[string]any{"priority": "P0"}}}); err != nil {
		t.Fatal(err)
	}
	late := seedTickets(t, b, engine.CreateIn{Title: "Records of processing", Labels: []string{"gdpr"}})[0]

	// Alice converts without having seen that: her operations replay.
	out, err := a.InitiativeFromLabel(ctx, ca, engine.FromLabelIn{Label: "gdpr", Title: "GDPR"})
	if err != nil || out.Error != nil || !out.Pushed {
		t.Fatalf("from-label: %v %+v", err, out)
	}
	ini := out.Initiative.ID
	if n := len(out.Tickets); n != 4 { // 2 free, 1 via the epic, the epic
		t.Fatalf("report: %+v", out.Tickets)
	}

	b.Sync(ctx, true)
	d, err := b.Get(engine.GetIn{ID: ids[1]})
	if err != nil || d.Priority != "P0" || d.ParentRef == nil || d.ParentRef.ID != ini || len(d.Labels) != 0 {
		t.Fatalf("replayed onto bob's edit: %v %+v", err, d)
	}
	if d, _ := b.Get(engine.GetIn{ID: epic}); d.ParentRef == nil || d.ParentRef.ID != ini {
		t.Fatalf("epic: %+v", d.ParentRef)
	}
	if d, _ := b.Get(engine.GetIn{ID: inEpic}); d.ParentRef.ID != epic || len(d.Labels) != 0 {
		t.Fatalf("ticket in the epic: %+v", d.Summary)
	}

	// Bob runs it again: the initiative is reused and only his ticket moves.
	again, err := b.InitiativeFromLabel(ctx, cb, engine.FromLabelIn{Label: "gdpr", Title: "GDPR"})
	if err != nil || again.Created || again.Initiative.ID != ini || len(again.Tickets) != 1 || again.Tickets[0].ID != late || again.Tickets[0].Action != engine.LabelParented {
		t.Fatalf("rerun: %v %+v", err, again)
	}
	third, _ := b.InitiativeFromLabel(ctx, cb, engine.FromLabelIn{Label: "gdpr", Title: "GDPR"})
	if third.Created || len(third.Tickets) != 0 {
		t.Fatalf("third run: %+v", third)
	}
	tree, _ := b.Tree(engine.TreeIn{ID: ini})
	if ru := tree.Nodes[0].Rollup; ru.Total != 4 || ru.Children != 4 {
		t.Fatalf("rollup after conversion: %+v", ru)
	}
	if hasDiag(b, "hierarchy_invalid") || hasDiag(b, "containers_unguarded") {
		t.Fatal("check findings after conversion")
	}
	// History is linear and every change is a ka operation.
	log := testkit.Git(t, b.WS.Stores[0].Root, "log", "--merges", "--format=%H")
	if log != "" {
		t.Fatalf("merge commits: %s", log)
	}
}

// ka_tree stays cheap: a realistic initiative with three epics.
func TestTreeTokenBudget(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, containersStore)
	seed := testkit.Service(t, r.Clone())
	ini := seedTickets(t, seed, engine.CreateIn{Title: "GDPR compliance for the hosted service", Kind: "initiative", Status: "in_progress", Priority: "P1"})[0]
	epics := seedTickets(t, seed,
		engine.CreateIn{Title: "Consent banner and preferences", Kind: "epic", Parent: ini, Status: "in_progress"},
		engine.CreateIn{Title: "Data export and erasure requests", Kind: "epic", Parent: ini, Status: "backlog"},
		engine.CreateIn{Title: "Records of processing activities", Kind: "epic", Parent: ini, Status: "backlog"},
	)
	statuses := []string{"done", "in_progress", "ready", "ready", "backlog", "review"}
	var ins []engine.CreateIn
	for i, e := range epics {
		for j := range 6 {
			ins = append(ins, engine.CreateIn{Title: fmt.Sprintf("Handle the consent edge case number %d for regional cookie rules", i*6+j),
				Parent: e, Priority: []string{"P1", "P2"}[j%2], Status: statuses[j]})
		}
	}
	for j := range 3 {
		ins = append(ins, engine.CreateIn{Title: fmt.Sprintf("Document the retention policy for data class %d", j), Parent: ini, Status: "ready"})
	}
	seedTickets(t, seed, ins...)

	a := connect(t, testkit.Service(t, r.Clone()), "claude-code")
	for _, tg := range []struct {
		name  string
		limit int
		args  map[string]any
	}{
		{"ka_tree of an initiative (25 tickets)", 1000, map[string]any{"id": ini}},
		{"ka_tree depth 1", 320, map[string]any{"id": ini, "depth": 1}},
		{"ka_tree of the project", 320, map[string]any{"depth": 1, "hide_closed": true, "project": "jk"}},
	} {
		text, _ := a.ok("ka_tree", tg.args)
		got := budget(tg.args, text)
		t.Logf("%-40s %4d / %4d tokens", tg.name, got, tg.limit)
		if got > tg.limit {
			t.Errorf("%s: %d tokens, budget %d\n%s", tg.name, got, tg.limit, text)
		}
	}
}
