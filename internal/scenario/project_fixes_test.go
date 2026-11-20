package scenario

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
	"github.com/KanArtist/ka/pkg/kaspec"
)

func storeNamed(svc *engine.Service, name string) *engine.Store {
	for _, st := range svc.WS.Stores {
		if st.Name == name {
			return st
		}
	}
	return nil
}

// shHook installs an executable shell hook.
func shHook(t *testing.T, p, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// handTicket commits (without pushing) a ticket written by hand in a clone
// and returns its ID.
func handTicket(t *testing.T, clone, project, prefix string) string {
	t.Helper()
	id := prefix + "-" + kaspec.RandomBase32(5)
	p := filepath.Join(clone, "projects", project, "tickets", id, "ticket.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("---\nid: "+id+"\ntitle: Arrived meanwhile\nstatus: ready\ncreated: 2026-11-07T10:00:00Z\nupdated: 2026-11-07T10:00:00Z\n---\n"), 0o644)
	testkit.Git(t, clone, "add", "-A")
	testkit.Git(t, clone, "commit", "-qm", "a ticket from elsewhere")
	return id
}

// pushFromElsewhere makes the next push from store dir lose a race: a
// pre-push hook first publishes clone's commits to the same remote.
func pushFromElsewhere(t *testing.T, dir, clone string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "done")
	shHook(t, filepath.Join(dir, ".git", "hooks", "pre-push"),
		"unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX\n"+
			"[ -e '"+marker+"' ] && exit 0\ntouch '"+marker+"'\n"+
			"git -C '"+clone+"' push -q origin HEAD:main")
}

// B2: when the target cannot be pushed, the source's tombstone is not
// pushed either, and the result says so.
func TestProjectMoveTargetNotPushed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, svc *engine.Service, remotes map[string]*testkit.Remote)
		want  string
	}{
		{"manual", func(t *testing.T, svc *engine.Service, _ map[string]*testkit.Remote) {
			storeNamed(svc, "org").PushMode = "manual"
		}, "push: manual"},
		{"refused", func(t *testing.T, _ *engine.Service, remotes map[string]*testkit.Remote) {
			shHook(t, filepath.Join(remotes["org"].Path, "hooks", "pre-receive"), "echo protected >&2; exit 1")
		}, "org: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, remotes := layered(t)
			me := svc.CallerFor("bsant", "")
			jkt := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Cache layer", Status: "ready"})[0]
			tc.setup(t, svc, remotes)
			out, err := svc.MoveProject(ctx, me, "jk", "org", false)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Pushed) != 0 || !strings.Contains(out.PushError, tc.want) || !strings.Contains(out.PushError, "jk held back") {
				t.Fatalf("move: %+v", out)
			}
			if _, err := os.Stat(filepath.Join(remotes["jk"].Clone(), "projects", "jk", "tickets", jkt, "ticket.md")); err != nil {
				t.Fatal("the source's tombstone was published without the copy")
			}
		})
	}
}

// B16, B18: a ticket that reaches the source while the move is pushed is
// moved too: the move is redone on top of upstream, not replayed stale.
func TestProjectMoveRedoneOnRace(t *testing.T) {
	svc, remotes := layered(t)
	me := svc.CallerFor("bsant", "")
	seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Cache layer", Status: "ready"})
	other := remotes["jk"].Clone()
	late := handTicket(t, other, "jk", "JK")
	pushFromElsewhere(t, storeNamed(svc, "jk").Root, other)

	out, err := svc.MoveProject(ctx, me, "jk", "org", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.PushError != "" || len(out.Pushed) != 2 {
		t.Fatalf("move: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(remotes["org"].Clone(), "projects", "jk", "tickets", late, "ticket.md")); err != nil {
		t.Fatalf("the ticket that arrived meanwhile was not moved: %v", err)
	}
	src := remotes["jk"].Clone()
	if _, err := os.Stat(filepath.Join(src, "projects", "jk", "tickets")); err == nil {
		t.Fatal("tickets left behind in the source")
	}
	if _, err := svc.Get(engine.GetIn{ID: late}); err != nil {
		t.Fatal(err)
	}
}

// B18: the same for a rename: a ticket that reaches the home store while
// the rename is pushed is renamed too.
func TestProjectRenameRedoneOnRace(t *testing.T) {
	svc, remotes := layered(t)
	me := svc.CallerFor("bsant", "")
	seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Parent", Status: "ready"})
	other := remotes["jk"].Clone()
	late := handTicket(t, other, "jk", "JK")
	pushFromElsewhere(t, storeNamed(svc, "jk").Root, other)

	out, err := svc.RenameProject(ctx, me, "jk", "jkx", "JKX", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.PushError != "" || out.IDs[late] != "JKX"+late[2:] {
		t.Fatalf("rename: %+v", out)
	}
	clone := remotes["jk"].Clone()
	if _, err := os.Stat(filepath.Join(clone, "projects", "jkx", "tickets", "JKX"+late[2:], "ticket.md")); err != nil {
		t.Fatalf("the ticket that arrived meanwhile was not renamed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "projects", "jk")); err == nil {
		t.Fatal("the old project directory is still published")
	}
}

// B4, B17: a rename that fails partway puts every store back, leaves the
// user's own staged and untracked files alone, and never commits them.
func TestProjectRenameRollsBack(t *testing.T) {
	svc, _ := layered(t)
	me := svc.CallerFor("bsant", "")
	jkt := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Parent", Status: "ready"})[0]
	seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "Needs it", Status: "ready", BlockedBy: []string{jkt}})
	jk, gy := storeNamed(svc, "jk"), storeNamed(svc, "gyggly")
	os.WriteFile(filepath.Join(jk.Root, "notes.txt"), []byte("mine\n"), 0o644)
	os.WriteFile(filepath.Join(jk.Root, "staged.txt"), []byte("mine\n"), 0o644)
	testkit.Git(t, jk.Root, "add", "staged.txt")
	jkHead, gyHead := testkit.Git(t, jk.Root, "rev-parse", "HEAD"), testkit.Git(t, gy.Root, "rev-parse", "HEAD")
	hook := filepath.Join(gy.Root, ".git", "hooks", "prepare-commit-msg")
	shHook(t, hook, "exit 1")

	if _, err := svc.RenameProject(ctx, me, "jk", "jkx", "JKX", false, false); err == nil {
		t.Fatal("the rename succeeded despite the failing commit")
	}
	if h := testkit.Git(t, jk.Root, "rev-parse", "HEAD"); h != jkHead {
		t.Fatal("the home store kept the rename commit")
	}
	if h := testkit.Git(t, gy.Root, "rev-parse", "HEAD"); h != gyHead {
		t.Fatal("gyggly kept a commit")
	}
	for _, st := range []*engine.Store{jk, gy} {
		want := ""
		if st == jk {
			want = "A  staged.txt\n?? notes.txt"
		}
		if s := testkit.Git(t, st.Root, "status", "--porcelain", "--untracked-files=all"); s != want {
			t.Fatalf("%s after the rollback: %q", st.Name, s)
		}
	}
	if _, err := svc.Get(engine.GetIn{ID: jkt}); err != nil {
		t.Fatalf("index after the rollback: %v", err)
	}

	os.Remove(hook)
	if _, err := svc.RenameProject(ctx, me, "jk", "jkx", "JKX", false, false); err != nil {
		t.Fatal(err)
	}
	files := testkit.Git(t, jk.Root, "show", "--name-only", "--format=", "HEAD")
	if strings.Contains(files, "notes.txt") || strings.Contains(files, "staged.txt") {
		t.Fatalf("the rename commit took the user's files:\n%s", files)
	}
	if s := testkit.Git(t, jk.Root, "status", "--porcelain"); s != "A  staged.txt\n?? notes.txt" {
		t.Fatalf("status after the rename: %q", s)
	}
}

// B17: a move whose second commit fails takes the first one back.
func TestProjectMoveRollsBack(t *testing.T) {
	svc, _ := layered(t)
	me := svc.CallerFor("bsant", "")
	jkt := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "Cache layer", Status: "ready"})[0]
	jk, org := storeNamed(svc, "jk"), storeNamed(svc, "org")
	orgHead := testkit.Git(t, org.Root, "rev-parse", "HEAD")
	hook := filepath.Join(jk.Root, ".git", "hooks", "prepare-commit-msg")
	shHook(t, hook, "exit 1")

	if _, err := svc.MoveProject(ctx, me, "jk", "org", false); err == nil {
		t.Fatal("the move succeeded despite the failing commit")
	}
	if h := testkit.Git(t, org.Root, "rev-parse", "HEAD"); h != orgHead {
		t.Fatal("the target kept the copy")
	}
	for _, st := range []*engine.Store{jk, org} {
		if s := testkit.Git(t, st.Root, "status", "--porcelain", "--untracked-files=all"); s != "" {
			t.Fatalf("%s after the rollback: %q", st.Name, s)
		}
	}
	if sch, _ := svc.Schema(me, "jk"); len(sch.Projects) != 1 || sch.Projects[0].Store != "jk" {
		t.Fatalf("jk's home after the rollback: %+v", sch.Projects)
	}
	os.Remove(hook)
	if _, err := svc.MoveProject(ctx, me, "jk", "org", false); err != nil {
		t.Fatal(err)
	}
	if d, err := svc.Get(engine.GetIn{ID: jkt}); err != nil || d.Store != "org" {
		t.Fatalf("after the move: %+v %v", d, err)
	}
}

// B20: a batch update across stores reports each store's own push.
func TestBatchUpdatePerStore(t *testing.T) {
	svc, _ := layered(t)
	me := svc.CallerFor("bsant", "")
	jkt := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "a", Status: "ready"})[0]
	gyt := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "b", Status: "ready"})[0]
	storeNamed(svc, "gyggly").PushMode = "manual"
	outs, err := svc.Update(ctx, me, []engine.UpdateIn{
		{ID: jkt, Set: map[string]any{"priority": "P1"}},
		{ID: gyt, Set: map[string]any{"priority": "P1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outs[0].Pushed || outs[1].Pushed {
		t.Fatalf("results: %+v %+v", outs[0], outs[1])
	}
}
