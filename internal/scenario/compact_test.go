package scenario

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// History compaction: one clone compacts, the others adopt on their own.

var compactor = engine.Caller{Actor: "bsant"}

func create(t *testing.T, svc *engine.Service, titles ...string) []string {
	t.Helper()
	var ins []engine.CreateIn
	for _, ti := range titles {
		ins = append(ins, engine.CreateIn{Title: ti})
	}
	out, err := svc.Create(ctx, engine.Caller{Actor: "agent/claude", OnBehalfOf: "bsant"}, ins)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range out.Created {
		ids = append(ids, c.ID)
	}
	return ids
}

func compact(t *testing.T, svc *engine.Service, summary string) *engine.CompactOut {
	t.Helper()
	out, err := svc.Compact(ctx, compactor, engine.CompactIn{Store: "test", Summary: summary, Confirm: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func syncOK(t *testing.T, svc *engine.Service) *engine.SyncOut {
	t.Helper()
	out := svc.Sync(ctx, true)
	if len(out.Errors) > 0 {
		t.Fatalf("sync: %v", out.Errors[0])
	}
	return out
}

func hasObject(dir, sha string) bool {
	return exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

func remoteMain(t *testing.T, r *testkit.Remote) string {
	return testkit.Git(t, r.Root, "--git-dir", r.Path, "rev-parse", "main")
}

// clean asserts a clone's working tree has nothing uncommitted.
func clean(t *testing.T, dir string) {
	t.Helper()
	if st := testkit.Git(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("%s is not clean:\n%s", dir, st)
	}
}

// (1) A level clone adopts the compacted history: the same tree, the old
// commits gone after gc, tickets and their timelines still readable.
func TestCompactLevelCloneAdopts(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, bdir := r.Clone(), r.Clone()
	a, b := testkit.Service(t, adir), testkit.Service(t, bdir)
	ids := create(t, a, "Cache eviction", "Flaky test")
	syncOK(t, b)
	oldHead := testkit.Git(t, bdir, "rev-parse", "HEAD")

	out := compact(t, a, "The cache work.")
	if out.Commits != 3 || out.Root != remoteMain(t, r) {
		t.Fatalf("compact: %+v", out)
	}
	sync := syncOK(t, b)
	if len(sync.Compacted) != 1 {
		t.Fatalf("sync did not report the adoption: %+v", sync)
	}
	if testkit.Git(t, bdir, "rev-parse", "HEAD") != out.Root || testkit.Git(t, bdir, "rev-parse", "HEAD^{tree}") != testkit.Git(t, adir, "rev-parse", "HEAD^{tree}") {
		t.Fatal("b's history or tree differs from the compactor's")
	}
	if hasObject(bdir, oldHead) || hasObject(adir, oldHead) {
		t.Fatal("old commits are still in a clone after gc")
	}
	if refs := testkit.Git(t, bdir, "for-each-ref", "refs/ka/"); refs != "" {
		t.Fatalf("backup refs left: %s", refs)
	}
	clean(t, bdir)
	if _, err := b.Get(engine.GetIn{ID: ids[0]}); err != nil {
		t.Fatal(err)
	}
	act, err := b.Activity(ctx, engine.ActivityIn{Ticket: ids[0]})
	if err != nil || len(act.Events) == 0 {
		t.Fatalf("timeline: %v %+v", err, act)
	}
	last := act.Events[len(act.Events)-1]
	if last.Type != engine.EvStoreCompacted || len(last.Data.(engine.CompactedEvent).Summaries) != 1 {
		t.Fatalf("timeline ends with %+v", last)
	}
	// Writes work on the new history, from either clone.
	create(t, b, "After the compaction")
	syncOK(t, a)
	if testkit.Git(t, adir, "rev-list", "--count", "HEAD") != "2" {
		t.Fatal("a did not fast-forward on the compacted history")
	}
}

// (2) A clone with unpushed operations adopts and replays them.
func TestCompactUnpushedOpsReplay(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, bdir := r.Clone(), r.Clone()
	a, b := testkit.Service(t, adir), testkit.Service(t, bdir)
	ids := create(t, a, "Shared")
	syncOK(t, b)
	b.NoPush = true
	local := create(t, b, "Written offline")
	if _, err := b.Comment(ctx, engine.Caller{Actor: "agent/claude"}, engine.CommentIn{ID: ids[0], Type: "note", Body: "offline note"}); err != nil {
		t.Fatal(err)
	}
	out := compact(t, a, "")
	b.NoPush = false
	syncOK(t, b)
	if roots := testkit.Git(t, bdir, "rev-list", "--max-parents=0", "HEAD"); roots != out.Root {
		t.Fatalf("b's root is %s", roots)
	}
	if testkit.Git(t, bdir, "rev-parse", "HEAD") != remoteMain(t, r) {
		t.Fatal("the replayed commits were not pushed")
	}
	fresh := testkit.Service(t, r.Clone())
	if _, err := fresh.Get(engine.GetIn{ID: local[0]}); err != nil {
		t.Fatalf("the offline ticket is lost: %v", err)
	}
	d, err := fresh.Get(engine.GetIn{ID: ids[0], Include: []string{"comments"}})
	if err != nil || !strings.Contains(strings.Join(commentBodies(d), "\n"), "offline note") {
		t.Fatalf("the offline comment is lost: %v", err)
	}
	clean(t, bdir)
}

func commentBodies(d *engine.Detail) []string {
	var out []string
	for _, c := range d.Comments {
		out = append(out, c.Body)
	}
	return out
}

// (3) A clone that never saw where the compaction started adopts it when it
// holds the same store and (nearly) all the tickets it knew.
func TestCompactBehindCloneAdopts(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, bdir := r.Clone(), r.Clone()
	a, b := testkit.Service(t, adir), testkit.Service(t, bdir)
	known := create(t, a, "Known to b")
	syncOK(t, b)
	b.NoPush = true
	local := create(t, b, "Unpushed in b")
	create(t, a, "Never seen by b") // b never fetches this before the compaction
	out := compact(t, a, "")
	b.NoPush = false
	syncOK(t, b)
	if testkit.Git(t, bdir, "rev-list", "--max-parents=0", "HEAD") != out.Root {
		t.Fatal("b did not adopt")
	}
	for _, id := range append(known, local...) {
		if _, err := b.Get(engine.GetIn{ID: id}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if testkit.Git(t, bdir, "rev-parse", "HEAD") != remoteMain(t, r) {
		t.Fatal("b's unpushed ticket was not published")
	}
}

// (4) A remote replaced by an unrelated repository is still refused, and so
// is a "compaction" that lacks the store's tickets; nothing moves.
func TestCompactUnrelatedRemoteRefused(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	bdir := r.Clone()
	b := testkit.Service(t, bdir)
	create(t, b, "One", "Two")
	head := testkit.Git(t, bdir, "rev-parse", "HEAD")

	// The same store name, other content (and so another first commit).
	other := testkit.NewRemote(t, map[string]string{"README.md": "another team's store\n"})
	testkit.Git(t, bdir, "remote", "set-url", "origin", other.Path)
	out := b.Sync(ctx, true)
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0].Message, "not this store's repository") || !strings.Contains(out.Errors[0].Message, "not a ka history compaction") {
		t.Fatalf("unrelated remote: %+v", out.Errors)
	}
	if testkit.Git(t, bdir, "rev-parse", "HEAD") != head {
		t.Fatal("the clone moved onto an unrelated repository")
	}

	// A root commit that claims to be a compaction, but of other tickets.
	seed := filepath.Join(other.Root, "seed")
	tree := testkit.Git(t, seed, "rev-parse", "HEAD^{tree}")
	cmd := exec.Command("git", "commit-tree", tree, "-m", "Compact history\n\nKa-Compacted-From: "+strings.Repeat("a", 40)+"\nKa-Compacted-Tree: "+tree)
	cmd.Dir = seed
	root, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	testkit.Git(t, seed, "push", "-q", "-f", "origin", strings.TrimSpace(string(root))+":refs/heads/main")
	out = b.Sync(ctx, true)
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0].Message, "only 0 of the 2 tickets") {
		t.Fatalf("fake compaction: %+v", out.Errors)
	}
	if testkit.Git(t, bdir, "rev-parse", "HEAD") != head {
		t.Fatal("the clone adopted a compaction of other tickets")
	}
}

// (5) A push that wins the race for the branch (the lease fails) and a host
// that refuses force pushes both leave the compactor's clone as it was after
// its summary was published, and the remote untouched.
func TestCompactFailuresLeaveCloneAsItWas(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, odir := r.Clone(), r.Clone()
	a := testkit.Service(t, adir)
	create(t, a, "One")
	testkit.Git(t, odir, "pull", "-q")
	handTicket(t, odir, "jk", "JK")

	// Lease: a pre-push hook publishes another clone's commit just before
	// the force push (the second push of the compaction).
	count := filepath.Join(t.TempDir(), "pushes")
	shHook(t, filepath.Join(adir, ".git", "hooks", "pre-push"),
		"unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX\n"+
			"echo x >> '"+count+"'\n"+
			"[ \"$(wc -l < '"+count+"')\" -eq 2 ] || exit 0\n"+
			"git -C '"+odir+"' pull -q --rebase origin main && git -C '"+odir+"' push -q origin HEAD:main")
	_, err := a.Compact(ctx, compactor, engine.CompactIn{Store: "test", Confirm: "test"})
	if !engine.IsCode(err, engine.CodeGitBusy) {
		t.Fatalf("lease: %v", err)
	}
	assertSummaryTip(t, adir, 3)
	if testkit.Git(t, r.Root, "--git-dir", r.Path, "rev-list", "--count", "main") != "4" {
		t.Fatal("the remote was rewritten")
	}
	os.Remove(filepath.Join(adir, ".git", "hooks", "pre-push"))

	// The host refuses force pushes (a protected branch).
	syncOK(t, a)
	shHook(t, filepath.Join(r.Path, "hooks", "pre-receive"),
		`while read old new ref; do
  if [ "$old" != 0000000000000000000000000000000000000000 ] && ! git merge-base --is-ancestor "$old" "$new"; then
    echo "protected branch: force push denied" >&2; exit 1
  fi
done`)
	before := testkit.Git(t, adir, "rev-parse", "HEAD")
	_, err = a.Compact(ctx, compactor, engine.CompactIn{Store: "test", Confirm: "test"})
	if !engine.IsCode(err, engine.CodeGit) || !strings.Contains(engine.AsError(err).Fix, "allow force pushes to main") {
		t.Fatalf("protected branch: %v", err)
	}
	n := testkit.Git(t, adir, "rev-list", "--count", "HEAD")
	assertSummaryTip(t, adir, 0)
	if testkit.Git(t, adir, "rev-parse", "HEAD^") != before || testkit.Git(t, adir, "rev-parse", "HEAD") != remoteMain(t, r) || n == "1" {
		t.Fatal("the clone or remote changed beyond the summary commit")
	}
}

// assertSummaryTip checks a clone sits on a published summary commit, with
// its full history (commits > 0: exactly that many), nothing uncommitted and
// no stray ka refs.
func assertSummaryTip(t *testing.T, dir string, commits int) {
	t.Helper()
	if s := testkit.Git(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(s, "Compaction summary:") {
		t.Fatalf("HEAD is %q", s)
	}
	if commits > 0 {
		if n := testkit.Git(t, dir, "rev-list", "--count", "HEAD"); n != strconv.Itoa(commits) {
			t.Fatalf("%s commits, want %d", n, commits)
		}
	}
	if testkit.Git(t, dir, "rev-parse", "HEAD") != testkit.Git(t, dir, "rev-parse", "origin/main") {
		t.Fatal("the tracking ref moved")
	}
	clean(t, dir)
	if refs := testkit.Git(t, dir, "for-each-ref", "refs/ka/"); refs != "" {
		t.Fatalf("ka refs: %s", refs)
	}
}

// (6) Two compactions in a row: a clone that slept through both adopts, its
// unpushed work is replayed, and both summaries are there.
func TestCompactTwiceSleeperAdopts(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, cdir := r.Clone(), r.Clone()
	a, c := testkit.Service(t, adir), testkit.Service(t, cdir)
	create(t, a, "Before")
	syncOK(t, c)
	c.NoPush = true
	local := create(t, c, "Slept on it")

	compact(t, a, "First.")
	create(t, a, "Between")
	out := compact(t, a, "Second.")
	if out.Previous == "" {
		t.Fatal("the second summary does not name the first")
	}
	c.NoPush = false
	syncOK(t, c)
	if testkit.Git(t, cdir, "rev-list", "--max-parents=0", "HEAD") != out.Root {
		t.Fatal("c did not adopt the second compaction")
	}
	h, err := c.History(ctx, "test")
	if err != nil || len(h.Summaries) != 2 || h.Summaries[1].Previous != h.Summaries[0].Name {
		t.Fatalf("history: %v %+v", err, h)
	}
	if first, err := c.HistorySummary("test", h.Summaries[0].Name); err != nil || first.Body != "First." {
		t.Fatalf("first summary: %v %+v", err, first)
	}
	if _, err := testkit.Service(t, r.Clone()).Get(engine.GetIn{ID: local[0]}); err != nil {
		t.Fatalf("c's offline ticket: %v", err)
	}
}

// (7) The daemon's fetch loop adopts a compaction without a manual sync.
func TestCompactFetchLoopAdopts(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, bdir := r.Clone(), r.Clone()
	a, b := testkit.Service(t, adir), testkit.Service(t, bdir)
	create(t, a, "One")
	b.Background = true
	st := storeNamed(b, "test")
	st.FetchInterval = 50 * time.Millisecond
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.FetchLoop(lctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitHead := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for testkit.Git(t, bdir, "rev-parse", "HEAD") != want {
			if time.Now().After(deadline) {
				t.Fatalf("b never reached %s; state %+v", want, st.State(ctx))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitHead(testkit.Git(t, adir, "rev-parse", "HEAD"))
	out := compact(t, a, "")
	waitHead(out.Root)
	time.Sleep(200 * time.Millisecond) // a few more ticks: no error, no loop
	if s := st.State(ctx); s.State != "synced" {
		t.Fatalf("after adopting: %+v", s)
	}
}

// (8) A ka older than the compaction's requires_ka says to upgrade instead
// of "shares no history", and does not move.
func TestCompactOldClientMustUpgrade(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir, bdir := r.Clone(), r.Clone()
	a, b := testkit.Service(t, adir), testkit.Service(t, bdir)
	create(t, a, "One")
	syncOK(t, b)
	head := testkit.Git(t, bdir, "rev-parse", "HEAD")
	compact(t, a, "")

	old := kaspec.KaVersion
	kaspec.KaVersion = "1.2.0"
	t.Cleanup(func() { kaspec.KaVersion = old })
	out := b.Sync(ctx, true)
	if len(out.Errors) != 1 || out.Errors[0].Code != engine.CodeKaOutdated || !strings.Contains(out.Errors[0].Message, "requires ka 1.3.0") ||
		strings.Contains(out.Errors[0].Message, "shares no history") {
		t.Fatalf("old client: %+v", out.Errors)
	}
	if testkit.Git(t, bdir, "rev-parse", "HEAD") != head {
		t.Fatal("an old client moved onto the compacted history")
	}
}
