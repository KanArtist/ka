package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets the test binary act as `ka merge-driver` when git invokes it.
func TestMain(m *testing.M) {
	if os.Getenv("KA_TEST_MERGE_DRIVER") == "1" {
		a := os.Args[len(os.Args)-4:]
		os.Exit(RunMergeDriver(ctx, a[0], a[1], a[2], a[3]))
	}
	os.Exit(m.Run())
}

func useTestDriver(t *testing.T) {
	exe, _ := os.Executable()
	old := MergeDriverCommand
	MergeDriverCommand = `KA_TEST_MERGE_DRIVER=1 "` + filepath.ToSlash(exe) + `" %O %A %B %P`
	t.Cleanup(func() { MergeDriverCommand = old })
}

// A human's `git pull --rebase` in a store goes through the semantic merge
// driver: edits to different fields of one ticket merge cleanly, and a
// same-field conflict keeps upstream and preserves the local value.
func TestMergeDriverDuringHumanRebase(t *testing.T) {
	e := newEnv(t, nil)
	useTestDriver(t)
	a, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "shared", Status: "ready", Priority: "P2"}})).Created[0].ID
	_, hdir := e.clone()
	if err := RegisterMergeDriver(ctx, a.WS.Stores[0].git); err != nil {
		t.Fatal(err)
	}
	run(t, hdir, "config", "merge.ka.driver", MergeDriverCommand)

	// Upstream: an agent changes priority and status.
	must[[]*WriteOut](t)(a.Update(ctx, agent("a"), []UpdateIn{{ID: id, Set: map[string]any{"priority": "P0", "kind": "bug"}}}))
	// The human edits the title and the kind by hand, on the adjacent lines.
	p := filepath.Join(hdir, "projects", "jk", "tickets", id, "ticket.md")
	data, _ := os.ReadFile(p)
	s := strings.Replace(string(data), "title: shared", "title: shared, renamed by hand", 1)
	s = strings.Replace(s, "priority: P2\n", "priority: P2\nkind: chore\n", 1)
	os.WriteFile(p, []byte(s), 0o644)
	run(t, hdir, "commit", "-qam", "hand edit")
	run(t, hdir, "pull", "-q", "--rebase")

	merged, _ := os.ReadFile(p)
	m := string(merged)
	if !strings.Contains(m, "title: shared, renamed by hand") || !strings.Contains(m, "priority: P0") || !strings.Contains(m, "kind: bug") {
		t.Fatalf("merged:\n%s", m)
	}
	// The lost value (kind: chore) waits in .git/ka-preserved until ka runs.
	h := e.open(hdir)
	out := h.Sync(ctx, true)
	if len(out.Preserved) != 1 {
		t.Fatalf("preserved: %+v", out)
	}
	d := must[*Detail](t)(h.Get(GetIn{ID: id, Include: []string{"comments"}}))
	if len(d.Comments) != 1 || !strings.Contains(d.Comments[0].Body, "chore") {
		t.Fatalf("comment: %+v", d.Comments)
	}
}

func TestManagedCloneFromWorkspaceFile(t *testing.T) {
	e := newEnv(t, nil)
	wsFile := filepath.Join(e.root, "workspace.yml")
	repo := filepath.Join(e.root, "code")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(wsFile, []byte("name: me\nstores:\n  - name: tickets\n    url: "+filepath.ToSlash(e.remote)+"\nidentity:\n  actor: tester\nrepos:\n  jk: ./code\n"), 0o644)
	ws, err := Open(ctx, OpenOptions{Dir: e.root, Workspace: wsFile})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Stores) != 1 || !ws.Stores[0].Managed || !strings.HasPrefix(ws.Stores[0].Root, os.Getenv("KA_DATA_DIR")) {
		t.Fatalf("store: %+v", ws.Stores[0])
	}
	s, _ := New(ctx, ws)
	out := must[*CreateOut](t)(s.Create(ctx, s.CallerFor("", ""), []CreateIn{{Title: "via managed clone"}}))
	if !out.Pushed || out.Local {
		t.Fatalf("create: %+v", out)
	}
	if s.Human() != "tester" || ws.Repos["jk"] != repo {
		t.Fatalf("identity %q repos %v", s.Human(), ws.Repos)
	}
	// Opening again reuses the clone.
	ws2, err := Open(ctx, OpenOptions{Dir: e.root, Workspace: wsFile})
	if err != nil || ws2.Stores[0].Root != ws.Stores[0].Root {
		t.Fatalf("reopen: %v", err)
	}
}

func TestPointerDiscovery(t *testing.T) {
	e := newEnv(t, nil)
	_, store := e.clone()
	code := filepath.Join(e.root, "code")
	os.MkdirAll(filepath.Join(code, "src", "deep"), 0o755)
	rel, _ := filepath.Rel(code, store)
	os.WriteFile(filepath.Join(code, ".ka.yml"), []byte("store: "+filepath.ToSlash(rel)+"\nproject: jk\n"), 0o644)
	ws, err := Open(ctx, OpenOptions{Dir: filepath.Join(code, "src", "deep")})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Project != "jk" || ws.Repos["jk"] != code || len(ws.Stores) != 1 {
		t.Fatalf("workspace: %+v", ws)
	}
	// An agent in the code repo can file and find work with no setup.
	s, _ := New(ctx, ws)
	must[*CreateOut](t)(s.Create(ctx, agent("a"), []CreateIn{{Title: "from the code repo", Status: "ready"}}))
	if n := must[*NextOut](t)(s.Next(ctx, agent("a"), NextIn{Peek: true})); n.Ticket.Title != "from the code repo" {
		t.Fatalf("next: %+v", n)
	}
	l := must[*LinkOut](t)(s.ResolveLink("jk://.ka.yml#L1"))
	if l.Path != filepath.Join(code, ".ka.yml") {
		t.Fatalf("link: %+v", l)
	}
}

func TestExtendsParentIsReadOnlyAndLayersConfig(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	org := filepath.Join(root, "org")
	if _, err := InitStore(ctx, InitOptions{Dir: org, Name: "org"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(org, ".ka", "config.yml"), []byte("kinds:\n  - feature\n  - bug\n  - spike\nlocked:\n  - kinds\n"), 0o644)
	run(t, org, "add", "-A")
	run(t, org, "commit", "-qm", "org")
	jk := filepath.Join(root, "jk")
	if _, err := InitStore(ctx, InitOptions{Dir: jk, Name: "jk", Project: "jk", Extends: "../org"}); err != nil {
		t.Fatal(err)
	}
	run(t, jk, "add", "-A")
	run(t, jk, "commit", "-qm", "jk")
	ws, err := Open(ctx, OpenOptions{Dir: jk})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Stores) != 2 || !ws.Stores[1].ReadOnly {
		t.Fatalf("stores: %+v", ws.Stores)
	}
	s, _ := New(ctx, ws)
	if _, err := s.Create(ctx, agent("a"), []CreateIn{{Title: "x", Kind: "spike"}}); err != nil {
		t.Fatalf("org kind not inherited: %v", err)
	}
	if _, err := s.Create(ctx, agent("a"), []CreateIn{{Title: "x", Kind: "chore"}}); !IsCode(err, CodeUnknownKind) {
		t.Fatalf("org replaced kinds; chore should be rejected: %v", err)
	}
}

// An extends parent known only by URL is cloned once online; offline opens
// (ka check) must use that clone rather than drop the parent's config.
func TestOfflineOpenUsesExistingParentClone(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	org := filepath.Join(root, "org")
	if _, err := InitStore(ctx, InitOptions{Dir: org, Name: "org"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(org, ".ka", "config.yml"), []byte("kinds:\n  - feature\n  - spike\n"), 0o644)
	run(t, org, "add", "-A")
	run(t, org, "commit", "-qm", "org")
	bare := filepath.Join(root, "org.git")
	run(t, root, "clone", "-q", "--bare", org, bare)
	jk := filepath.Join(root, "jk")
	if _, err := InitStore(ctx, InitOptions{Dir: jk, Name: "jk", Project: "jk", Extends: bare}); err != nil {
		t.Fatal(err)
	}
	run(t, jk, "add", "-A")
	run(t, jk, "commit", "-qm", "jk")
	if ws, err := Open(ctx, OpenOptions{Dir: jk}); err != nil || len(ws.Stores) != 2 {
		t.Fatalf("online open: %v %+v", err, ws)
	}
	ws, err := Open(ctx, OpenOptions{Dir: jk, Offline: true})
	if err != nil || len(ws.Stores) != 2 || !ws.Stores[1].ReadOnly {
		t.Fatalf("offline open dropped the parent: %v %+v", err, ws)
	}
}
