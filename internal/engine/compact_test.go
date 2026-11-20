package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KanArtist/ka/pkg/kaspec"
)

func TestCompactDryRunAndConfirm(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("claude")
	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "one"}, {Title: "two"}}))
	must[*WriteOut](t)(a.Transition(ctx, c, TransitionIn{ID: out.Created[0].ID, Status: "done", Summary: "did it"}))
	before := run(t, dir, "rev-parse", "HEAD")

	dry := must[*CompactOut](t)(a.Compact(ctx, c, CompactIn{Store: "test", DryRun: true}))
	if dry.Commits != 4 || dry.TicketsCreated != 2 || dry.TicketsClosed != 1 || dry.Actors["agent/claude"] != 3 ||
		!dry.RaisesKa || dry.RequiresKa != kaspec.CompactionMinKa || !strings.HasPrefix(dry.SummaryFile, "history/") || dry.SizeBefore == 0 {
		t.Fatalf("dry run: %+v", dry)
	}
	if run(t, dir, "rev-parse", "HEAD") != before {
		t.Fatal("the dry run changed the clone")
	}
	// The real run needs the store's name as confirmation.
	for _, conf := range []string{"", "other"} {
		if _, err := a.Compact(ctx, c, CompactIn{Store: "test", Confirm: conf}); !IsCode(err, CodeInvalid) {
			t.Fatalf("confirm %q: %v", conf, err)
		}
	}
	if run(t, dir, "rev-parse", "HEAD") != before {
		t.Fatal("an unconfirmed compaction changed the clone")
	}
}

func TestCompactRefusals(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("claude")
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "one"}}))

	// Uncommitted edits in the store.
	p := filepath.Join(dir, ".ka", "store.yml")
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(orig, []byte("description: edited\n")...), 0o644)
	if _, err := a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"}); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty checkout: %v", err)
	}
	os.WriteFile(p, orig, 0o644)

	// A commit that cannot be pushed (the remote refuses every push).
	a.NoPush = true
	if _, err := a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"}); err == nil {
		t.Fatal("compacted with --no-push")
	}
	a.NoPush = false

	// No remote.
	local := t.TempDir()
	if _, err := InitStore(ctx, InitOptions{Dir: local, Name: "solo", Project: "so", Prefix: "SO"}); err != nil {
		t.Fatal(err)
	}
	run(t, local, "add", "-A")
	run(t, local, "commit", "-qm", "init")
	l := e.open(local)
	if _, err := l.Compact(ctx, c, CompactIn{Store: "solo", Confirm: "solo"}); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("no remote: %v", err)
	}
}

// A push that lands between the summary push and the force push makes the
// lease fail: the branch goes back to the summary commit, nothing else changes.
func TestCompactLeaseFailure(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	b, _ := e.clone()
	c := agent("claude")
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "one"}}))
	b.Sync(ctx, true)
	beforeForcePush = func() {
		must[*CreateOut](t)(b.Create(ctx, agent("other"), []CreateIn{{Title: "sneaked in"}}))
	}
	t.Cleanup(func() { beforeForcePush = nil })

	_, err := a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test", Summary: "first try"})
	if !IsCode(err, CodeGitBusy) || !strings.Contains(err.Error(), "someone pushed") {
		t.Fatalf("lease: %v", err)
	}
	head := run(t, dir, "rev-parse", "HEAD")
	if subj := run(t, dir, "log", "-1", "--format=%s", "HEAD"); !strings.HasPrefix(subj, "Compaction summary:") {
		t.Fatalf("HEAD is %q", subj)
	}
	if n := run(t, dir, "rev-list", "--count", "HEAD"); n != "3" {
		t.Fatalf("history rewritten locally: %s commits", n)
	}
	if st := run(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("status: %q", st)
	}
	if roots := run(t, e.root, "--git-dir", e.remote, "rev-list", "--max-parents=0", "main"); strings.Contains(roots, "\n") || run(t, e.root, "--git-dir", e.remote, "rev-list", "--count", "main") != "4" {
		t.Fatalf("remote rewritten")
	}
	// The retry takes in the new commit, so it writes a new summary that
	// names the first one as previous.
	beforeForcePush = nil
	out := must[*CompactOut](t)(a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"}))
	if names, _ := os.ReadDir(filepath.Join(dir, "history")); len(names) != 2 || out.Previous == "" || "history/"+out.Previous == out.SummaryFile {
		t.Fatalf("summaries after the retry: %v, previous %q", names, out.Previous)
	}
	if out.Root == "" || run(t, e.root, "--git-dir", e.remote, "rev-parse", "main") != out.Root || head == out.From {
		t.Fatalf("retry: %+v", out)
	}
}

// A force push the host refuses leaves the branch at the published summary
// commit; a retry with nothing new reuses that summary.
func TestCompactRefusedForcePush(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("claude")
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "one"}}))
	run(t, e.root, "--git-dir", e.remote, "config", "receive.denyNonFastForwards", "true")

	_, err := a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"})
	if !IsCode(err, CodeGit) || !strings.Contains(AsError(err).Fix, "allow force pushes to main") {
		t.Fatalf("refused force push: %v", err)
	}
	head := run(t, dir, "rev-parse", "HEAD")
	if head != run(t, e.root, "--git-dir", e.remote, "rev-parse", "main") || run(t, dir, "rev-list", "--count", "HEAD") != "3" {
		t.Fatal("clone and remote disagree after the refused force push")
	}
	run(t, e.root, "--git-dir", e.remote, "config", "receive.denyNonFastForwards", "false")
	out := must[*CompactOut](t)(a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"}))
	if names, _ := os.ReadDir(filepath.Join(dir, "history")); len(names) != 1 || out.From != head {
		t.Fatalf("retry wrote another summary: %v %+v", names, out)
	}
}

// A clone that never saw the compacted tip, whose tracking ref an unlocked
// fetch (the daemon's fetchLevel) already moved to the new history, finds
// what it last saw upstream in the reflog and replays its unpushed work.
func TestAdoptBehindAfterUnlockedFetch(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, bdir := e.clone()
	c := agent("claude")
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "known"}}))
	b.Sync(ctx, true)
	b.NoPush = true
	local := must[*CreateOut](t)(b.Create(ctx, c, []CreateIn{{Title: "offline"}})).Created[0].ID
	b.NoPush = false
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "unseen"}}))
	out := must[*CompactOut](t)(a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"}))

	st := b.WS.Stores[0]
	if st.fetchLevel(ctx, "origin", "main") {
		t.Fatal("level after fetching a compaction")
	}
	if run(t, bdir, "rev-parse", "origin/main") != out.Root {
		t.Fatal("the unlocked fetch did not move the tracking ref")
	}
	res := b.Sync(ctx, true)
	if len(res.Errors) > 0 || len(res.Compacted) != 1 {
		t.Fatalf("sync: %+v", res)
	}
	if run(t, bdir, "rev-list", "--max-parents=0", "HEAD") != out.Root || run(t, bdir, "rev-list", "--count", "HEAD") != "2" {
		t.Fatal("b did not adopt and replay")
	}
	if _, err := b.Get(GetIn{ID: local}); err != nil {
		t.Fatal(err)
	}
}

// Uncommitted edits that the move to the compacted history would overwrite
// stop the adoption; nothing changes.
func TestAdoptBlockedByLocalEdits(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, bdir := e.clone()
	c := agent("claude")
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "one"}}))
	head := run(t, bdir, "rev-parse", "HEAD") // b is behind: the tree differs
	must[*CompactOut](t)(a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test"}))
	p := filepath.Join(bdir, ".ka", "store.yml") // the compaction changes it upstream
	edited := "spec: 1\nname: test\ndescription: mine\n"
	os.WriteFile(p, []byte(edited), 0o644)

	res := b.Sync(ctx, true)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Fix, "commit or stash") {
		t.Fatalf("sync: %+v", res.Errors)
	}
	if run(t, bdir, "rev-parse", "HEAD") != head {
		t.Fatal("b moved")
	}
	if got, _ := os.ReadFile(p); string(got) != edited {
		t.Fatal("the local edit is gone")
	}
	if refs := run(t, bdir, "for-each-ref", "refs/ka/"); refs != "" {
		t.Fatalf("backup ref left: %s", refs)
	}
}

func TestCompactThenAdopt(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	b, bdir := e.clone()
	c := agent("claude")
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "one"}, {Title: "two"}}))
	b.Sync(ctx, true)
	oldHead := run(t, bdir, "rev-parse", "HEAD")

	out := must[*CompactOut](t)(a.Compact(ctx, c, CompactIn{Store: "test", Confirm: "test", Summary: "Two tickets."}))
	if n := run(t, dir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("compactor has %s commits", n)
	}
	info := run(t, dir, "log", "-1", "--format=%B", "HEAD")
	for _, want := range []string{"Ka-Compacted-From: " + out.From, "Ka-Compacted-Tree: " + out.Tree, "Ka-Actor: agent/claude"} {
		if !strings.Contains(info, want) {
			t.Fatalf("root commit lacks %q:\n%s", want, info)
		}
	}
	if exec.Command("git", "-C", dir, "cat-file", "-e", oldHead+"^{commit}").Run() == nil {
		t.Fatal("old history still in the compactor's clone")
	}
	if !strings.Contains(run(t, dir, "show", "HEAD:.ka/store.yml"), "requires_ka: 1.3.0") {
		t.Fatal("requires_ka not raised")
	}
	h := must[*HistoryOut](t)(a.History(ctx, "test"))
	if !h.Compacted || len(h.Summaries) != 1 || h.Summaries[0].Commits != 3 {
		t.Fatalf("history: %+v", h)
	}
	one := must[*HistoryEntry](t)(a.HistorySummary("test", h.Summaries[0].Name))
	if one.Body != "Two tickets." {
		t.Fatalf("summary body %q", one.Body)
	}
	// The activity feed ends with the compaction.
	act := must[*ActivityOut](t)(a.Activity(ctx, ActivityIn{}))
	if last := act.Events[len(act.Events)-1]; last.Type != EvStoreCompacted {
		t.Fatalf("activity ends with %+v", last)
	}

	res := b.Sync(ctx, true)
	if len(res.Errors) > 0 {
		t.Fatalf("adopting: %v", res.Errors[0])
	}
	if run(t, bdir, "rev-parse", "HEAD") != out.Root {
		t.Fatal("b did not adopt the new root")
	}
	// b is an attached checkout (not a clone ka manages): adopting must not
	// expire its reflogs or prune, so the old history stays until git's
	// own expiry, with nothing of ka's pointing at it.
	if exec.Command("git", "-C", bdir, "cat-file", "-e", oldHead+"^{commit}").Run() != nil {
		t.Fatal("adopting pruned an attached checkout")
	}
	if refs := run(t, bdir, "for-each-ref", "--contains", oldHead); refs != "" {
		t.Fatalf("refs still hold the old history in b: %s", refs)
	}
	if st := run(t, bdir, "status", "--porcelain"); st != "" {
		t.Fatalf("b status: %q", st)
	}
}
