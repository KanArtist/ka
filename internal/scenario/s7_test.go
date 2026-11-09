package scenario

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// layered builds the S7 workspace: an org store (shared config, actors,
// cross-team boards, and a project of its own) and two project stores that
// extend it. It returns a service over a workspace naming all three.
func layered(t *testing.T) (*engine.Service, map[string]*testkit.Remote) {
	t.Helper()
	testkit.GitEnv(t)
	org := testkit.NewStoreRemote(t, engine.InitOptions{Name: "org", Project: "ops", Prefix: "OPS"}, map[string]string{
		".ka/config.yml":       "kinds: [feature, bug, chore]\nfields:\n  estimate:\n    type: number\n  team:\n    type: string\nlocked: [workflow]\n",
		".ka/actors.yml":       "humans:\n  bsant:\n    name: Bryan\nagents:\n  claude:\n    name: Claude\n",
		"boards/in-flight.yml": "name: In flight\nfilter: \"status:in_progress\"\ngroup_by: status\nswimlanes: project\n",
	})
	jk := testkit.NewStoreRemote(t, engine.InitOptions{Name: "jk-tickets", Project: "jk", Prefix: "JK", Extends: org.Path}, map[string]string{
		".ka/config.yml": "kinds:\n  $append: [spike]\nfields:\n  team: null\n",
	})
	gy := testkit.NewStoreRemote(t, engine.InitOptions{Name: "gyggly", Project: "gyggly", Prefix: "GYG", Extends: org.Path}, map[string]string{
		".ka/config.yml": "workflow:\n  statuses:\n    - {name: open, category: unstarted}\n    - {name: closed, category: completed}\n  initial: open\n",
	})

	ws := filepath.Join(t.TempDir(), "workspace.yml")
	body := "name: dev\nstores:\n" +
		"  - name: org\n    url: " + org.Path + "\n" +
		"  - name: jk\n    url: " + jk.Path + "\n" +
		"  - name: gyggly\n    url: " + gy.Path + "\n" +
		"identity:\n  actor: bsant\n"
	if err := os.WriteFile(ws, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := engine.Open(ctx, engine.OpenOptions{Workspace: ws, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := engine.New(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	return svc, map[string]*testkit.Remote{"org": org, "jk": jk, "gyggly": gy}
}

// S7: a workspace combines org-planning (shared config, actors and
// cross-team boards), jk-tickets and gyggly. One board shows in_progress
// work across all three, and a jk ticket can be blocked by a gyggly ticket.
func TestS7LayeredWorkspace(t *testing.T) {
	svc, _ := layered(t)
	if got := svc.Projects(); !slices.Equal(got, []string{"gyggly", "jk", "ops"}) {
		t.Fatalf("projects = %v", got)
	}
	me := svc.CallerFor("bsant", "")

	// Config layering: jk inherits org's kinds and appends spike; org's
	// workflow lock beats gyggly's override (reported, and ignored).
	sch, err := svc.Schema(me, "")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{}
	statuses := map[string][]string{}
	for _, p := range sch.Projects {
		kinds[p.Key] = p.Kinds
		for _, s := range p.Statuses {
			statuses[p.Key] = append(statuses[p.Key], s.Name)
		}
	}
	if !slices.Equal(kinds["jk"], []string{"feature", "bug", "chore", "spike"}) || !slices.Equal(kinds["gyggly"], []string{"feature", "bug", "chore"}) {
		t.Fatalf("kinds = %v", kinds)
	}
	if slices.Contains(statuses["gyggly"], "open") || !slices.Contains(statuses["gyggly"], "in_progress") {
		t.Fatalf("org's workflow lock did not hold: gyggly statuses %v", statuses["gyggly"])
	}
	var locked bool
	for _, d := range svc.Validate(svc.Now()) {
		if d.Code == "config_locked" && strings.Contains(d.Path, "gyggly") {
			locked = true
		}
	}
	if !locked {
		t.Fatal("ka check does not report gyggly's override of a locked key")
	}

	// ka config explain shows where each value came from.
	ex, err := svc.ConfigExplain("jk", "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]engine.ConfigEntry{}
	for _, e := range ex.Entries {
		got[e.Key] = e
	}
	if e := got["kinds"]; !strings.HasPrefix(e.Source, "jk") || e.Note == "" || e.Line == 0 {
		t.Fatalf("kinds: %+v", e)
	}
	if e := got["fields.estimate"]; !strings.HasPrefix(e.Source, "org") {
		t.Fatalf("fields.estimate: %+v", e)
	}
	if e := got["fields.team"]; e.Note != "deleted" || !strings.HasPrefix(e.Source, "jk") {
		t.Fatalf("fields.team: %+v", e)
	}
	if e := got["workflow.statuses"]; !strings.HasPrefix(e.LockedBy, "org") {
		t.Fatalf("workflow.statuses: %+v", e)
	}
	if len(ex.Layers) != 4 || ex.Layers[0] != "built-in defaults" || !strings.HasPrefix(ex.Layers[1], "org") {
		t.Fatalf("layers = %v", ex.Layers)
	}

	// Cross-store blocker: a jk ticket blocked by a gyggly ticket.
	gyg := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "Gyggly API", Status: "ready", Priority: "P1"})[0]
	jkb := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Use the Gyggly API", Status: "ready", Priority: "P0", BlockedBy: []string{gyg}})[0]
	ops := seedTickets(t, svc, engine.CreateIn{Project: "ops", Title: "Rotate keys", Status: "ready", Priority: "P2"})[0]
	d, err := svc.Get(engine.GetIn{ID: jkb})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked {
		t.Fatalf("%s is not blocked by %s", jkb, gyg)
	}
	agent := svc.CallerFor("agent/claude", "S-1")
	n, err := svc.Next(ctx, agent, engine.NextIn{Peek: true})
	if err != nil {
		t.Fatal(err)
	}
	if n.Ticket == nil || n.Ticket.ID != gyg {
		t.Fatalf("next should be the gyggly blocker, got %+v", n.Ticket)
	}

	// Work in progress in all three stores.
	for _, id := range []string{gyg, ops} {
		if _, err := svc.Claim(ctx, agent, id, false); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Transition(ctx, agent, engine.TransitionIn{ID: id, Status: "in_progress"}); err != nil {
			t.Fatal(err)
		}
	}
	// Closing the gyggly ticket unblocks the jk one.
	if _, err := svc.Transition(ctx, agent, engine.TransitionIn{ID: gyg, Status: "done", Resolution: "done", Summary: "API shipped"}); err != nil {
		t.Fatal(err)
	}
	if d, _ = svc.Get(engine.GetIn{ID: jkb}); d.Blocked {
		t.Fatalf("%s still blocked after %s closed", jkb, gyg)
	}
	if _, err := svc.Transition(ctx, agent, engine.TransitionIn{ID: jkb, Status: "in_progress"}); err != nil {
		t.Fatal(err)
	}
	gy2 := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "Docs", Status: "in_progress"})[0]

	// The org board shows in_progress work across all three stores.
	f, ok := svc.BoardFilter("in-flight")
	if !ok {
		t.Fatal("org board in-flight not found")
	}
	res, err := svc.Search(engine.SearchIn{Query: f, Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range res.Tickets {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	want := []string{gy2, jkb, ops}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("board in-flight = %v, want %v", ids, want)
	}
	stores := map[string]bool{}
	for _, s := range res.Tickets {
		stores[s.Project] = true
	}
	if len(stores) != 3 {
		t.Fatalf("board spans %v", stores)
	}
}

// TestS7MigratedStores runs S7 over real migration output: set
// KA_S7_STORES to a migrate-v0 --out directory (split or single layout).
// The stores are copied, so the originals are never written.
func TestS7MigratedStores(t *testing.T) {
	src := os.Getenv("KA_S7_STORES")
	if src == "" {
		t.Skip("set KA_S7_STORES to a migrate-v0 output directory")
	}
	testkit.GitEnv(t)
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	body := "name: s7\nstores:\n"
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if !e.IsDir() || !fileExists(filepath.Join(dir, ".ka", "store.yml")) {
			continue
		}
		testkit.Git(t, dir, "init", "-q", "-b", "main")
		testkit.Git(t, dir, "add", "-A")
		testkit.Git(t, dir, "commit", "-q", "-m", "migrated")
		body += "  - name: " + e.Name() + "\n    path: " + dir + "\n"
	}
	ws := filepath.Join(root, "workspace.yml")
	os.WriteFile(ws, []byte(body+"identity:\n  actor: bsant\n"), 0o644)
	w, err := engine.Open(ctx, engine.OpenOptions{Workspace: ws, Dir: root, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := engine.New(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	svc.NoPush = true
	agent := svc.CallerFor("agent/claude", "S-1")

	gyg := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "S7 gyggly blocker", Status: "in_progress"})[0]
	jkb := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "S7 jk work", Status: "ready", BlockedBy: []string{gyg}})[0]
	kn := seedTickets(t, svc, engine.CreateIn{Project: "knest", Title: "S7 knest work", Status: "in_progress"})[0]
	if d, _ := svc.Get(engine.GetIn{ID: jkb}); d == nil || !d.Blocked {
		t.Fatalf("%s is not blocked by %s", jkb, gyg)
	}
	if _, err := svc.Transition(ctx, agent, engine.TransitionIn{ID: gyg, Status: "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Transition(ctx, agent, engine.TransitionIn{ID: gyg, Status: "done", Resolution: "done", Summary: "shipped"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Get(engine.GetIn{ID: jkb}); d.Blocked {
		t.Fatalf("%s still blocked", jkb)
	}
	if _, err := svc.Transition(ctx, agent, engine.TransitionIn{ID: jkb, Status: "in_progress"}); err != nil {
		t.Fatal(err)
	}

	f, ok := svc.BoardFilter("in-flight")
	if !ok {
		t.Fatal("the org board in-flight is missing")
	}
	res, err := svc.Search(engine.SearchIn{Query: f, Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, s := range res.Tickets {
		found[s.ID] = s.Project
	}
	if found[jkb] != "jk" || found[kn] != "knest" {
		t.Fatalf("in-flight board (%s) misses the new work: %d tickets", f, len(res.Tickets))
	}
	projects := map[string]bool{}
	for _, p := range found {
		projects[p] = true
	}
	t.Logf("%d stores, in-flight board: %d tickets across %d projects", len(w.Stores), len(res.Tickets), len(projects))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ka project move: jk moves from its own store into org. IDs stay, a gyggly
// ticket blocked by a jk ticket still resolves, the source keeps a
// tombstone, and a fresh clone of the workspace sees one home for jk.
func TestProjectMove(t *testing.T) {
	svc, remotes := layered(t)
	me := svc.CallerFor("bsant", "")
	jkt := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Cache layer", Status: "ready"})[0]
	if _, err := svc.Comment(ctx, me, engine.CommentIn{ID: jkt, Body: "history stays with the ticket"}); err != nil {
		t.Fatal(err)
	}
	gyt := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "Needs the cache", Status: "ready", BlockedBy: []string{jkt}})[0]

	dry, err := svc.MoveProject(ctx, me, "jk", "org", true)
	if err != nil || dry.Tickets != 1 || dry.Files < 3 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	out, err := svc.MoveProject(ctx, me, "jk", "org", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.PushError != "" || len(out.Pushed) != 2 || out.Pushed[0] != "org" {
		t.Fatalf("move: %+v", out)
	}
	if _, err := svc.MoveProject(ctx, me, "jk", "org", false); err == nil {
		t.Fatal("moving into the store that already holds the project should fail")
	}

	check := func(svc *engine.Service) {
		t.Helper()
		sch, _ := svc.Schema(me, "jk")
		if len(sch.Projects) != 1 || sch.Projects[0].Store != "org" {
			t.Fatalf("jk's home: %+v", sch.Projects)
		}
		d, err := svc.Get(engine.GetIn{ID: jkt, Include: []string{"comments"}})
		if err != nil || len(d.Comments) != 1 {
			t.Fatalf("%s after the move: %+v %v", jkt, d, err)
		}
		g, _ := svc.Get(engine.GetIn{ID: gyt})
		if !g.Blocked || len(g.BlockedBy) != 1 || g.BlockedBy[0].Title != "Cache layer" {
			t.Fatalf("cross-store reference broke: %+v", g.BlockedBy)
		}
		for _, d := range svc.Validate(svc.Now()) {
			if d.Code == "duplicate_project" || d.Code == "moved_project_has_tickets" {
				t.Fatalf("after the move: %v", d)
			}
		}
	}
	check(svc)

	// A fresh clone of the jk store has only the tombstone.
	jkClone := remotes["jk"].Clone()
	tomb, err := os.ReadFile(filepath.Join(jkClone, "projects", "jk", "project.yml"))
	pf, _ := kaspec.ParseProjectFile("project.yml", tomb)
	if err != nil || pf == nil || pf.MovedToStore != remotes["org"].Path {
		t.Fatalf("tombstone: %s %v", tomb, err)
	}
	if _, err := os.Stat(filepath.Join(jkClone, "projects", "jk", "tickets")); err == nil {
		t.Fatal("tickets left behind in the source store")
	}
	// Writes keep working, now in org.
	if _, err := svc.Update(ctx, me, []engine.UpdateIn{{ID: jkt, Set: map[string]any{"priority": "P1"}}}); err != nil {
		t.Fatal(err)
	}
	orgClone := remotes["org"].Clone()
	if b, err := os.ReadFile(filepath.Join(orgClone, "projects", "jk", "tickets", jkt, "ticket.md")); err != nil || !strings.Contains(string(b), "priority: P1") {
		t.Fatalf("write after the move did not land in org: %v", err)
	}
}
