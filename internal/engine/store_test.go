package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/gitx"
	"github.com/KanArtist/ka/pkg/kaspec"
)

func createOp(s *Service, c Caller, title string) *Op {
	now := s.now()
	return stamp(&Op{Op: "create", ID: kaspec.NewID("JK", 5), New: &kaspec.Ticket{Title: title, Status: "ready", Created: now, Updated: now}}, c, now)
}

func writeFile(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hook installs a git hook script in a clone.
func hook(t *testing.T, dir, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	p := filepath.Join(dir, ".git", "hooks", name)
	writeFile(t, p, "#!/bin/sh\n"+script+"\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func commitFiles(t *testing.T, dir, rev string) []string {
	t.Helper()
	return strings.Fields(run(t, dir, "show", "--name-only", "--format=", rev))
}

// B4: an operation commits its own paths only; whatever the user staged in
// the checkout stays staged and out of ka's commit.
func TestCommitOpTakesOnlyItsPaths(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	writeFile(t, filepath.Join(dir, "staged.txt"), "mine\n")
	run(t, dir, "add", "staged.txt")

	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "t", Status: "ready"}})).Created[0].ID
	files := commitFiles(t, dir, "HEAD")
	if len(files) != 1 || !strings.Contains(files[0], id+"/ticket.md") {
		t.Fatalf("commit took %v", files)
	}
	if st := run(t, dir, "status", "--porcelain"); st != "A  staged.txt" {
		t.Fatalf("status after the write: %q", st)
	}
	if strings.Contains(run(t, e.root, "--git-dir", e.remote, "ls-tree", "-r", "--name-only", "main"), "staged.txt") {
		t.Fatal("the staged file was pushed")
	}
}

// B7: a commit that fails leaves neither the working tree nor the index
// changed, so the next operation does not take the failed one with it.
func TestFailedCommitRollsBack(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "before", Status: "ready"}})).Created[0].ID
	hook(t, dir, "prepare-commit-msg", "exit 1")

	if _, err := a.Create(ctx, agent("a"), []CreateIn{{Title: "new ticket", Status: "ready"}}); err == nil {
		t.Fatal("create succeeded despite the failing hook")
	}
	if _, err := a.Update(ctx, agent("a"), []UpdateIn{{ID: id, Set: map[string]any{"title": "after"}}}); err == nil {
		t.Fatal("update succeeded despite the failing hook")
	}
	if st := run(t, dir, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("left behind:\n%s", st)
	}

	os.Remove(filepath.Join(dir, ".git", "hooks", "prepare-commit-msg"))
	must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "other", Status: "ready"}}))
	if files := commitFiles(t, dir, "HEAD"); len(files) != 1 {
		t.Fatalf("next commit took %v", files)
	}
	if d := must[*Detail](t)(a.Get(GetIn{ID: id})); d.Title != "before" {
		t.Fatalf("title %q", d.Title)
	}
}

func TestCommitPaths(t *testing.T) {
	e := newEnv(t, map[string]string{"boards/old.yml": "name: old\n"})
	a, dir := e.clone()
	st := a.WS.Stores[0]
	writeFile(t, filepath.Join(dir, "unrelated.txt"), "x\n")
	run(t, dir, "add", "unrelated.txt")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "x\n")

	unlock, err := st.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := st.guard(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "boards", "new.yml"), "name: new\n")
	os.Remove(filepath.Join(dir, "boards", "old.yml"))
	c := Caller{Actor: "agent/a", OnBehalfOf: "tester", Session: "S1"}
	sha, err := st.commitPaths(ctx, c, "ka: boards", []string{"Ka-Extra: yes"}, []string{"boards/new.yml", "boards/old.yml"})
	if err != nil || sha == "" {
		t.Fatalf("commitPaths: %q %v", sha, err)
	}
	if files := commitFiles(t, dir, "HEAD"); strings.Join(files, " ") != "boards/new.yml boards/old.yml" {
		t.Fatalf("committed %v", files)
	}
	msg := run(t, dir, "log", "-1", "--format=%B")
	tr := gitx.ParseTrailers(msg)
	if tr["Ka-Actor"] != "agent/a" || tr["Ka-On-Behalf-Of"] != "tester" || tr["Ka-Session"] != "S1" || tr["Ka-Spec"] != "1" || tr["Ka-Extra"] != "yes" || !strings.HasPrefix(msg, "ka: boards\n\n") {
		t.Fatalf("message:\n%s", msg)
	}
	if s := run(t, dir, "status", "--porcelain"); s != "A  unrelated.txt\n?? untracked.txt" {
		t.Fatalf("status: %q", s)
	}

	// A directory, and nothing to commit.
	if sha, err := st.commitPaths(ctx, c, "again", nil, []string{"boards"}); sha != "" || err != nil {
		t.Fatalf("unchanged: %q %v", sha, err)
	}

	// The guards Do runs.
	writeFile(t, filepath.Join(st.gitDir, "MERGE_HEAD"), run(t, dir, "rev-parse", "HEAD")+"\n")
	if _, err := st.commitPaths(ctx, c, "busy", nil, []string{"boards"}); !IsCode(err, CodeGitBusy) {
		t.Fatalf("mid-merge: %v", err)
	}
	os.Remove(filepath.Join(st.gitDir, "MERGE_HEAD"))
	st.ReadOnly = true
	if _, err := st.commitPaths(ctx, c, "ro", nil, []string{"boards"}); !IsCode(err, CodeReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
}

// B1: a replay that fails partway puts the branch back where it was, so no
// local commit is lost and nothing half-replayed is pushed.
func TestFailedReplayRestoresBranch(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, bdir := e.clone()
	b.NoPush = true
	sb := b.WS.Stores[0]
	one := createOp(b, agent("b"), "first")
	two := createOp(b, agent("b"), "second FAILME")
	for _, o := range []*Op{one, two} {
		if _, err := b.do(ctx, sb, o, true); err != nil {
			t.Fatal(err)
		}
	}
	up := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "upstream", Status: "ready"}})).Created[0].ID
	head := run(t, bdir, "rev-parse", "HEAD")
	remote := run(t, e.root, "--git-dir", e.remote, "rev-parse", "main")

	hook(t, bdir, "prepare-commit-msg", `grep -q FAILME "$1" && exit 1; exit 0`)
	if _, err := sb.Sync(ctx, b.hooks(sb)); err == nil {
		t.Fatal("sync succeeded despite the failing replay")
	}
	if h := run(t, bdir, "rev-parse", "HEAD"); h != head {
		t.Fatalf("HEAD moved to %s", h)
	}
	if s := run(t, bdir, "status", "--porcelain", "--untracked-files=all"); s != "" {
		t.Fatalf("left behind:\n%s", s)
	}
	if r := run(t, e.root, "--git-dir", e.remote, "rev-parse", "main"); r != remote {
		t.Fatal("pushed a partial replay")
	}
	for _, id := range []string{one.ID, two.ID} {
		if _, err := b.Get(GetIn{ID: id}); err != nil {
			t.Fatalf("index lost %s: %v", id, err)
		}
	}
	if _, err := b.Get(GetIn{ID: up}); err == nil {
		t.Fatal("index kept the upstream ticket after the abort")
	}

	os.Remove(filepath.Join(bdir, ".git", "hooks", "prepare-commit-msg"))
	b.NoPush = false
	if _, err := sb.Sync(ctx, b.hooks(sb)); err != nil {
		t.Fatal(err)
	}
	c, _ := e.clone()
	for _, id := range []string{one.ID, two.ID, up} {
		if _, err := c.Get(GetIn{ID: id}); err != nil {
			t.Fatalf("%s not published: %v", id, err)
		}
	}
}

// B1: local merge commits are linearized, not cherry-picked (which fails
// without -m); a clean replay leaves no backup ref behind.
func TestReplayLinearizesMerges(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, bdir := e.clone()
	b.NoPush = true
	sb := b.WS.Stores[0]
	mine := createOp(b, agent("b"), "local")
	if _, err := b.do(ctx, sb, mine, true); err != nil {
		t.Fatal(err)
	}
	run(t, bdir, "checkout", "-q", "-b", "side")
	writeFile(t, filepath.Join(bdir, "docs", "note.md"), "hello\n")
	run(t, bdir, "add", "docs/note.md")
	run(t, bdir, "commit", "-qm", "note")
	run(t, bdir, "checkout", "-q", "main")
	run(t, bdir, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	up := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "upstream", Status: "ready"}})).Created[0].ID

	b.NoPush = false
	res, err := sb.Sync(ctx, b.hooks(sb))
	if err != nil || !res.Pushed {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if m := run(t, bdir, "rev-list", "--merges", "HEAD"); m != "" {
		t.Fatalf("merge commits remain: %s", m)
	}
	if _, err := os.Stat(filepath.Join(bdir, "docs", "note.md")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{mine.ID, up} {
		if _, err := b.Get(GetIn{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if refs := run(t, bdir, "for-each-ref", "refs/ka/backup/"); refs != "" {
		t.Fatalf("backup kept after a clean replay: %s", refs)
	}
}

// B9: an operation that can neither be replayed nor preserved (its ticket
// was deleted upstream) is reported, and its commit kept in a backup ref.
func TestUnpreservableOpIsReported(t *testing.T) {
	e := newEnv(t, nil)
	a, adir := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "doomed", Status: "ready"}})).Created[0].ID
	b, bdir := e.clone()
	b.NoPush = true
	must[[]*WriteOut](t)(b.Update(ctx, agent("b"), []UpdateIn{{ID: id, Set: map[string]any{"title": "edited"}}}))
	run(t, adir, "rm", "-rq", "projects/jk/tickets/"+id)
	run(t, adir, "commit", "-qm", "delete by hand")
	run(t, adir, "push", "-q")

	sb := b.WS.Stores[0]
	res, err := sb.Sync(ctx, b.hooks(sb))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unpreserved) != 1 || !strings.Contains(res.Unpreserved[0], id) {
		t.Fatalf("unpreserved: %+v", res)
	}
	if run(t, bdir, "for-each-ref", "refs/ka/backup/") == "" {
		t.Fatal("no backup ref kept")
	}
}

// B9: preserved merge values whose comment cannot be written stay on disk.
func TestFlushPreservedKeepsUnwritten(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	st := a.WS.Stores[0]
	dir := filepath.Join(st.gitDir, "ka-preserved")
	writeFile(t, filepath.Join(dir, "1-x.json"), `[{"path":"projects/jk/tickets/JK-NOPE1/ticket.md","field":"title","lost":"a","kept":"b"}]`)
	unlock, _ := st.lock(ctx)
	out := st.flushPreserved(ctx, a.hooks(st))
	unlock()
	if len(out) != 0 {
		t.Fatalf("wrote %v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "1-x.json")); err != nil {
		t.Fatal("preserved file deleted although its comment was not written")
	}
}

// B8, B10: a rejected push whose fetch then fails is an offline push: the
// operation succeeded, and the health view says the push did not happen.
func TestRejectedPushWithFailedFetchIsOffline(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, bdir := e.clone()
	must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "first", Status: "ready"}}))
	run(t, bdir, "config", "remote.origin.uploadpack", "false")

	sb := b.WS.Stores[0]
	res, err := b.do(ctx, sb, createOp(b, agent("b"), "second"), true)
	if err != nil {
		t.Fatalf("committed op reported as failed: %v", err)
	}
	if res.Commit == "" || res.Pushed || !strings.Contains(res.PushError, "fetch failed") {
		t.Fatalf("result: %+v", res)
	}
	if st := sb.State(ctx); st.State != "degraded" {
		t.Fatalf("state: %+v", st)
	}
}

// B10: an offline push is not recorded as a successful one.
func TestOfflinePushIsDegraded(t *testing.T) {
	e := newEnv(t, nil)
	b, bdir := e.clone()
	run(t, bdir, "remote", "set-url", "origin", filepath.Join(e.root, "gone.git"))
	sb := b.WS.Stores[0]
	res, err := b.do(ctx, sb, createOp(b, agent("b"), "offline"), true)
	if err != nil || res.PushError == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if last, _, lastErr := sb.Activity(); !last.IsZero() || lastErr == "" {
		t.Fatalf("activity: fetch %v err %q", last, lastErr)
	}
	if st := sb.State(ctx); st.State != "degraded" {
		t.Fatalf("state: %+v", st)
	}
}

// I2: catchUp does not reload the process's own commits again, but still
// picks up commits made by another process in the same clone.
func TestCatchUpSkipsOwnCommits(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "mine", Status: "ready"}})).Created[0].ID
	// An uncommitted edit on disk: a reload would pick it up.
	p := filepath.Join(dir, "projects", "jk", "tickets", id, "ticket.md")
	data, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(data), "title: mine", "title: disk", 1)), 0o644)
	a.ix.catchUp()
	if d := must[*Detail](t)(a.Get(GetIn{ID: id})); d.Title != "mine" {
		t.Fatalf("own commit reloaded again: %q", d.Title)
	}
	os.WriteFile(p, data, 0o644)

	other := e.open(dir)
	id2 := must[*CreateOut](t)(other.Create(ctx, agent("o"), []CreateIn{{Title: "theirs", Status: "ready"}})).Created[0].ID
	a.ix.catchUp()
	if _, err := a.Get(GetIn{ID: id2}); err != nil {
		t.Fatalf("missed another process's commit: %v", err)
	}
}

// B11: an incremental reload of a deleted ticket drops it from the project's
// ID list too, so validation does not dereference it.
func TestIncrementalDeleteDropsID(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	ids := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "one", Status: "ready"}, {Title: "two", Status: "ready"}}))
	id := ids.Created[0].ID
	st := a.WS.Stores[0]
	rel := "projects/jk/tickets/" + id
	os.RemoveAll(filepath.Join(dir, filepath.FromSlash(rel)))
	a.ix.refresh(st, []string{rel + "/ticket.md"})
	kp := a.ix.ks[st].Projects["jk"]
	if slices.Contains(kp.IDs, id) || !slices.IsSorted(kp.IDs) || !slices.Contains(kp.IDs, ids.Created[1].ID) {
		t.Fatalf("IDs: %v", kp.IDs)
	}
	a.Validate(time.Now()) // panicked before
}

// fetchLevel lets the fetch loop skip the write locks when nothing changed.
func TestFetchLevel(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, _ := e.clone()
	sb := b.WS.Stores[0]
	if !sb.fetchLevel(ctx, "origin", "main") {
		t.Fatal("level clone not reported level")
	}
	must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "new", Status: "ready"}}))
	if sb.fetchLevel(ctx, "origin", "main") {
		t.Fatal("behind clone reported level")
	}
}

func TestEnsureCloneSafety(t *testing.T) {
	gitEnv(t)
	if p := ClonePath(StoreSpec{Name: "../../evil", URL: "x"}); filepath.Dir(p) != filepath.Join(DataDir(), "stores") {
		t.Fatalf("clone path escapes: %s", p)
	}
	if p := ClonePath(StoreSpec{Name: "tickets", URL: "x"}); !strings.HasPrefix(filepath.Base(p), "tickets-") {
		t.Fatalf("valid name changed: %s", p)
	}
	if _, err := ensureClone(ctx, StoreSpec{URL: "--upload-pack=touch /nonexistent"}, false); err == nil {
		t.Fatal("cloned an option")
	}
	if _, err := ensureClone(ctx, StoreSpec{URL: "x", Branch: "--evil"}, false); err == nil {
		t.Fatal("cloned an option branch")
	}

	e := newEnv(t, nil)
	sp := StoreSpec{URL: e.remote}
	dir, err := ensureClone(ctx, sp, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".ka", "store.yml")); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(dir + ".tmp-*"); len(left) > 0 {
		t.Fatalf("temporary clones left: %v", left)
	}

	// A failed clone never deletes a directory ka did not create.
	bad := StoreSpec{URL: filepath.Join(e.root, "missing.git")}
	keep := filepath.Join(ClonePath(bad), "keep.txt")
	writeFile(t, keep, "x")
	if _, err := ensureClone(ctx, bad, false); err == nil {
		t.Fatal("clone of a missing remote succeeded")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("deleted a directory it did not create")
	}
}

func TestEventHubSince(t *testing.T) {
	h := NewEventHub(10)
	start := h.Seq()
	for i := 0; i < 25; i++ {
		h.Publish(Event{Type: EvSyncState})
	}
	last := h.Seq()
	evs, ok, _ := h.Since(last - 3)
	if !ok || len(evs) != 3 || evs[0].Seq != last-2 || evs[2].Seq != last {
		t.Fatalf("since: %v %v", ok, evs)
	}
	if evs, ok, _ := h.Since(last); !ok || len(evs) != 0 {
		t.Fatalf("since last: %v %v", ok, evs)
	}
	if evs, ok, _ := h.Since(last - 10); !ok || len(evs) != 10 || evs[0].Seq != last-9 {
		t.Fatalf("whole ring: %v %d", ok, len(evs))
	}
	if _, ok, _ := h.Since(last - 11); ok {
		t.Fatal("evicted events not reported")
	}
	if _, ok, _ := h.Since(start); ok {
		t.Fatal("evicted events not reported")
	}
}

func TestQueryQuotesAndGlobs(t *testing.T) {
	q, err := ParseQuery(`label:"needs review",ui assignee:*claude* -"a:b"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.terms) != 2 || strings.Join(q.terms[0].values, "|") != "needs review|ui" {
		t.Fatalf("terms: %+v", q.terms)
	}
	if len(q.text) != 1 || q.text[0].s != "a:b" || !q.text[0].neg {
		t.Fatalf("text: %+v", q.text)
	}
	for _, c := range []struct {
		p, v string
		ok   bool
	}{
		{"*claude*", "agent/claude-code", true},
		{"area*", "area/ui", true},
		{"a*b*c", "aXbYc", true},
		{"a*a", "a", false},
		{"*x", "agent/y", false},
		{"ab*", "a", false},
	} {
		if globMatch(c.p, c.v) != c.ok {
			t.Errorf("globMatch(%q, %q) != %v", c.p, c.v, c.ok)
		}
	}
}

func TestMergeKeepsLocalSlug(t *testing.T) {
	base := "---\nid: JK-7QK3M\ntitle: t\nstatus: ready\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n"
	local := strings.Replace(base, "title: t\n", "title: t\nslug: my-slug\n", 1)
	up := strings.Replace(base, "status: ready", "status: review", 1)
	merged, _, ok := MergeTickets(ctx, []byte(base), []byte(up), []byte(local))
	m, _ := kaspec.ParseTicket("m", merged)
	if !ok || m.Slug != "my-slug" || m.Status != "review" {
		t.Fatalf("merged:\n%s", merged)
	}
}

func TestFileURLPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		if p, ok := fileURLPath("file:///C:/stores/org"); !ok || p != `C:\stores\org` {
			t.Fatalf("%q %v", p, ok)
		}
		return
	}
	for in, want := range map[string]string{
		"file:///srv/org":          "/srv/org",
		"file://localhost/srv/org": "/srv/org",
	} {
		if p, ok := fileURLPath(in); !ok || p != want {
			t.Errorf("%s: %q %v", in, p, ok)
		}
	}
	if _, ok := fileURLPath("file://server/share"); ok {
		t.Error("remote host taken as local")
	}
}

// B9: a hand edit to a non-ticket file that loses to upstream has no ticket
// to be preserved on; it is reported, and the backup ref kept.
func TestLostNonTicketEditIsReported(t *testing.T) {
	e := newEnv(t, map[string]string{"docs/x.md": "base\n"})
	_, adir := e.clone()
	b, bdir := e.clone()
	writeFile(t, filepath.Join(adir, "docs", "x.md"), "upstream\n")
	run(t, adir, "commit", "-qam", "up edit")
	run(t, adir, "push", "-q")
	writeFile(t, filepath.Join(bdir, "docs", "x.md"), "local\n")
	run(t, bdir, "commit", "-qam", "local edit")

	sb := b.WS.Stores[0]
	res, err := sb.Sync(ctx, b.hooks(sb))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unpreserved) != 1 || !strings.Contains(res.Unpreserved[0], "docs/x.md") {
		t.Fatalf("result: %+v", res)
	}
	if data, _ := os.ReadFile(filepath.Join(bdir, "docs", "x.md")); string(data) != "upstream\n" {
		t.Fatalf("docs/x.md: %q", data)
	}
	if run(t, bdir, "for-each-ref", "refs/ka/backup/") == "" {
		t.Fatal("no backup ref kept")
	}
}
