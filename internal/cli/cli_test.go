package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JumpKickOSS/ka/internal/testkit"
)

type res struct {
	code     int
	out, err string
}

func ka(t *testing.T, dir string, args ...string) res {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), &IO{Out: &out, Err: &errb, Dir: dir, In: strings.NewReader("")}, args)
	return res{code, out.String(), errb.String()}
}

func TestCLIWorkflow(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	if r := ka(t, dir, "init", "--project", "demo", dir); r.code != 0 || !strings.Contains(r.out, "created .ka/store.yml") {
		t.Fatalf("init: %+v", r)
	}
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")

	r := ka(t, dir, "new", "-t", "Fix the flaky cache test", "--kind", "bug", "--priority", "P1", "--status", "ready", "--accept", "passes 100 runs", "--json")
	var created struct {
		Created []struct{ ID string }
	}
	if r.code != 0 || json.Unmarshal([]byte(r.out), &created) != nil || len(created.Created) != 1 {
		t.Fatalf("new: %+v", r)
	}
	id := created.Created[0].ID

	if r := ka(t, dir, "ls", "kind:bug"); r.code != 0 || !strings.Contains(r.out, id+" P1 bug ready") {
		t.Fatalf("ls: %+v", r)
	}
	if r := ka(t, dir, "next", "--claim", "--as", "agent/claude"); r.code != 0 || !strings.Contains(r.out, "claimed") {
		t.Fatalf("next: %+v", r)
	}
	if r := ka(t, dir, "edit", id, "--set", "priority=P0", "--add", "labels=cache"); r.code != 0 || !strings.Contains(r.out, id+" updated") {
		t.Fatalf("edit: %+v", r)
	}
	if r := ka(t, dir, "check-item", id, "0", "--as", "agent/claude"); r.code != 0 || !strings.Contains(r.out, "acceptance 1/1") {
		t.Fatalf("check-item: %+v", r)
	}
	// Closing without a summary fails with an actionable message.
	if r := ka(t, dir, "move", id, "done"); r.code != 1 || !strings.Contains(r.err, "resolution_required") || !strings.Contains(r.err, "fix:") {
		t.Fatalf("move without -m: %+v", r)
	}
	if r := ka(t, dir, "move", id, "doing", "-m", "x"); r.code != 1 || !strings.Contains(r.err, "did you mean") {
		t.Fatalf("move to a typo: %+v", r)
	}
	if r := ka(t, dir, "move", id, "done", "-m", "Fixed the race.", "--as", "agent/claude"); r.code != 0 || !strings.Contains(r.out, "→ done") {
		t.Fatalf("move: %+v", r)
	}
	r = ka(t, dir, "show", id, "--comments")
	if r.code != 0 || !strings.Contains(r.out, "resolution (done)") || !strings.Contains(r.out, "[cache]") || !strings.Contains(r.out, "P0") {
		t.Fatalf("show: %+v", r)
	}
	if r := ka(t, dir, "check"); r.code != 0 || !strings.Contains(r.out, "0 error(s), 0 warning(s)") {
		t.Fatalf("check: %+v", r)
	}
	if r := ka(t, dir, "doctor"); !strings.Contains(r.out, "git") {
		t.Fatalf("doctor: %+v", r)
	}
	if r := ka(t, dir, "show", "JK-ZZZZZ"); r.code != 1 || !strings.Contains(r.err, "not_found") {
		t.Fatalf("show missing: %+v", r)
	}
	if r := ka(t, dir, "bogus"); r.code != 2 {
		t.Fatalf("unknown command: %+v", r)
	}
	if r := ka(t, dir, "template"); r.code != 0 || !strings.Contains(r.out, "id: DEMO-") {
		t.Fatalf("template: %+v", r)
	}
	if r := ka(t, dir, "daemon", "openapi"); r.code != 0 || !strings.Contains(r.out, `"openapi": "3.1.0"`) {
		t.Fatalf("daemon openapi: %+v", r)
	}
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	if r := ka(t, dir, "daemon", "status"); r.code != 1 || !strings.Contains(r.out, "no daemon") {
		t.Fatalf("daemon status: %+v", r)
	}
	if r := ka(t, dir, "daemon", "--listen", "0.0.0.0:0"); r.code != 1 || !strings.Contains(r.err, "without authentication") {
		t.Fatalf("daemon on 0.0.0.0: %+v", r)
	}
	// Commits carry the actor and op; the author is the git identity.
	log := testkit.Git(t, dir, "log", "-1", "--format=%an|%B")
	if !strings.HasPrefix(log, "Test User|"+id+": ready → done") && !strings.Contains(log, "→ done") {
		t.Fatalf("log: %s", log)
	}
	if !strings.Contains(log, "Ka-Actor: agent/claude") || !strings.Contains(log, "Ka-On-Behalf-Of: tester") {
		t.Fatalf("trailers: %s", log)
	}
}

// ka move without -m must not wait on stdin, even when stdin is a pipe
// that stays open (as in agents' shells).
func TestMoveDoesNotReadOpenStdin(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")
	r := ka(t, dir, "new", "-t", "Something", "--status", "ready", "--json")
	var created struct{ Created []struct{ ID string } }
	json.Unmarshal([]byte(r.out), &created)
	pr, pw := io.Pipe()
	defer pw.Close()
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		done <- Run(context.Background(), &IO{Out: &out, Err: &errb, Dir: dir, In: pr}, []string{"move", created.Created[0].ID, "in_progress"})
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("move: %d %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ka move blocked reading stdin")
	}
}

func TestConfigExplain(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	os.WriteFile(filepath.Join(dir, ".ka", "config.yml"), []byte("kinds: [feature, bug]\nlocked: [kinds]\n"), 0o644)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")
	r := ka(t, dir, "config", "explain")
	if r.code != 0 || !regexp.MustCompile(`kinds\s+\[feature, bug\]\s+\S*\.ka/config\.yml:1`).MatchString(r.out) || !strings.Contains(r.out, "claims.stale_after") {
		t.Fatalf("config explain: %+v", r)
	}
	r = ka(t, dir, "config", "explain", "--json", "claims")
	if r.code != 0 || !strings.Contains(r.out, `"key": "claims.stale_after"`) || strings.Contains(r.out, `"key": "kinds"`) {
		t.Fatalf("config explain claims: %+v", r)
	}
	if r = ka(t, dir, "config", "explain", "nope"); r.code == 0 {
		t.Fatalf("unknown key accepted: %+v", r)
	}
}

func TestCheckCIFormats(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	bad := "---\nid: DEMO-7QK3M\ntitle: Bad kind\nstatus: ready\nkind: nonsense\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n"
	p := filepath.Join(dir, "projects", "demo", "tickets", "DEMO-7QK3M", "ticket.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(bad), 0o644)

	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_WORKSPACE", filepath.Dir(dir))
	r := ka(t, dir, "check", "--ci", dir)
	if r.code != 1 || !strings.Contains(r.out, "::error file=tickets/projects/demo/tickets/DEMO-7QK3M/ticket.md,line=5,title=ka check%3A unknown_kind::") {
		t.Fatalf("github annotations: %+v", r)
	}
	sarif := filepath.Join(t.TempDir(), "ka.sarif")
	r = ka(t, dir, "check", "--format", "sarif", "-o", sarif, dir)
	if r.code != 1 || !strings.Contains(r.out, "1 error(s)") {
		t.Fatalf("sarif run: %+v", r)
	}
	var log struct {
		Version string `json:"version"`
		Runs    []struct {
			Results []struct {
				RuleID    string `json:"ruleId"`
				Level     string `json:"level"`
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct{ URI string }    `json:"artifactLocation"`
						Region           struct{ StartLine int } `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	data, _ := os.ReadFile(sarif)
	if err := json.Unmarshal(data, &log); err != nil || log.Version != "2.1.0" || len(log.Runs[0].Results) != 1 {
		t.Fatalf("sarif: %v\n%s", err, data)
	}
	res := log.Runs[0].Results[0]
	if res.RuleID != "unknown_kind" || res.Level != "error" || res.Locations[0].PhysicalLocation.ArtifactLocation.URI != "tickets/projects/demo/tickets/DEMO-7QK3M/ticket.md" || res.Locations[0].PhysicalLocation.Region.StartLine != 5 {
		t.Fatalf("sarif result: %+v", res)
	}
	if r = ka(t, dir, "check", "--format", "xml", dir); r.code != 2 {
		t.Fatalf("bad format accepted: %+v", r)
	}
}

func TestHooksAndStaged(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")

	if r := ka(t, dir, "hooks", "install"); r.code != 0 {
		t.Fatalf("install: %+v", r)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if b, err := os.ReadFile(hook); err != nil || !strings.Contains(string(b), "ka check --staged") {
		t.Fatalf("hook: %s %v", b, err)
	}
	if r := ka(t, dir, "hooks", "status"); !strings.Contains(r.out, "installed") {
		t.Fatalf("status: %+v", r)
	}

	// A hand-written ticket with keys out of order: fmt --staged fixes and restages it.
	p := filepath.Join(dir, "projects", "demo", "tickets", "DEMO-7QK3M", "ticket.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("---\ntitle: Hand written\nid: DEMO-7QK3M\nstatus: ready\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n"), 0o644)
	testkit.Git(t, dir, "add", "-A")
	if r := ka(t, dir, "fmt", "--staged"); r.code != 0 || !strings.Contains(r.out, "formatted projects/demo/tickets/DEMO-7QK3M/ticket.md") {
		t.Fatalf("fmt --staged: %+v", r)
	}
	if staged := testkit.Git(t, dir, "show", ":projects/demo/tickets/DEMO-7QK3M/ticket.md"); !strings.HasPrefix(staged, "---\nid: DEMO-7QK3M\ntitle: Hand written") {
		t.Fatalf("staged after fmt:\n%s", staged)
	}
	if r := ka(t, dir, "check", "--staged"); r.code != 0 {
		t.Fatalf("check --staged (clean): %+v", r)
	}

	// The index decides: a bad kind staged fails even if the worktree is fixed.
	os.WriteFile(p, []byte("---\nid: DEMO-7QK3M\ntitle: Hand written\nstatus: ready\nkind: nonsense\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n"), 0o644)
	testkit.Git(t, dir, "add", "-A")
	os.WriteFile(p, []byte("---\nid: DEMO-7QK3M\ntitle: Hand written\nstatus: ready\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n"), 0o644)
	if r := ka(t, dir, "check", "--staged"); r.code != 1 || !strings.Contains(r.out, "unknown_kind") || !strings.Contains(r.out, "projects/demo/tickets/DEMO-7QK3M/ticket.md") {
		t.Fatalf("check --staged (bad): %+v", r)
	}

	// An unrelated hook is not replaced without --force; ours uninstalls.
	if r := ka(t, dir, "hooks", "uninstall"); r.code != 0 {
		t.Fatalf("uninstall: %+v", r)
	}
	os.WriteFile(hook, []byte("#!/bin/sh\necho mine\n"), 0o755)
	if r := ka(t, dir, "hooks", "install"); r.code != 1 || !strings.Contains(r.err, "--force") {
		t.Fatalf("install over a foreign hook: %+v", r)
	}
}

func TestCompletion(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")
	r := ka(t, dir, "new", "-t", "Complete me", "--no-push")
	id := strings.Fields(r.out)[0]
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"__complete", "cl"}, "claim\n"},
		{[]string{"__complete", "new", "--pri"}, "--priority\n"},
		{[]string{"__complete", "show", strings.ToLower(id[:6])}, id + "\n"},
		{[]string{"__complete", "move", id, "in_"}, "in_progress\n"},
	} {
		if r := ka(t, dir, c.args...); r.code != 0 || r.out != c.want {
			t.Errorf("%v: %q (want %q) %s", c.args, r.out, c.want, r.err)
		}
	}
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		if r := ka(t, dir, "completion", sh); r.code != 0 || !strings.Contains(r.out, "__complete") {
			t.Errorf("completion %s: %+v", sh, r)
		}
	}
}

func TestDescribeEvent(t *testing.T) {
	for _, c := range []struct {
		typ, data, want string
	}{
		{"ticket.moved", `{"ticket":{"title":"T"},"from":"ready","to":"review"}`, "moved it ready → review"},
		{"ticket.updated", `{"changes":{"labels":{"from":["a"],"to":["a","b"]},"priority":{"to":"P1"}}}`, "labels +b, set priority to P1"},
		{"ticket.updated", `{"changes":{"labels":{"to":{"add":["x"],"remove":["y"]}}},"body_changed":true}`, "labels +x −y, edited the description"},
		{"claim.changed", `{"to":{"by":"agent/a"}}`, "claimed it"},
		{"claim.changed", `{"from":{"by":"agent/a"}}`, "released it"},
		{"comment.created", `{"comment":{"type":"answer","choice_text":"Yes"}}`, "answered “Yes”"},
	} {
		if v, _, _ := describeEvent(watchEvent{Type: c.typ, Data: json.RawMessage(c.data)}); v != c.want {
			t.Errorf("%s %s: %q, want %q", c.typ, c.data, v, c.want)
		}
	}
}

// kaTTY runs ka as if a person were typing input at a terminal.
func kaTTY(t *testing.T, dir, input string, args ...string) res {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), &IO{Out: &out, Err: &errb, Dir: dir, In: strings.NewReader(input), Interactive: true}, args)
	return res{code, out.String(), errb.String()}
}

func TestInteractivePrompts(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")

	// ka new with no title: title, kind 2 (bug), priority 3 (P1), a description.
	r := kaTTY(t, dir, "Fix the flaky test\n3\n3\nIt fails one run in ten.\n\n", "new", "--no-push")
	if r.code != 0 || !strings.Contains(r.err, "Title: ") || !strings.Contains(r.err, "Priority") {
		t.Fatalf("new: %+v", r)
	}
	id := strings.Fields(r.out)[0]
	sh := ka(t, dir, "show", id, "--json")
	if !strings.Contains(sh.out, `"kind": "bug"`) || !strings.Contains(sh.out, `"priority": "P1"`) || !strings.Contains(sh.out, "one run in ten") {
		t.Fatalf("created: %s", sh.out)
	}

	// ka move with no status: pick by name, then the close asks for a summary.
	r = kaTTY(t, dir, "done\nFixed the race.\n\n", "move", id, "--no-push")
	if r.code != 0 || !strings.Contains(r.err, "Move it to:") || !strings.Contains(r.err, "Summary") {
		t.Fatalf("move: %+v", r)
	}
	if sh = ka(t, dir, "show", id, "--json", "--comments"); !strings.Contains(sh.out, `"status": "done"`) || !strings.Contains(sh.out, "Fixed the race.") {
		t.Fatalf("after move: %s", sh.out)
	}

	// ka comment with no text.
	if r = kaTTY(t, dir, "Line one\nline two\n\n", "comment", id, "--no-push"); r.code != 0 {
		t.Fatalf("comment: %+v", r)
	}

	// ka answer with no choice: the question and its options are shown.
	q := ka(t, dir, "new", "-t", "Pick a cache", "--no-push")
	qid := strings.Fields(q.out)[0]
	if a := ka(t, dir, "ask", qid, "-m", "Which cache?", "--option", "LRU", "--option", "LFU", "--no-push", "--as", "agent/claude"); a.code != 0 {
		t.Fatalf("ask: %+v", a)
	}
	r = kaTTY(t, dir, "2\n", "answer", qid, "--no-push")
	if r.code != 0 || !strings.Contains(r.err, "Which cache?") || !strings.Contains(r.err, "2) LFU") {
		t.Fatalf("answer: %+v", r)
	}
	if sh = ka(t, dir, "show", qid, "--comments"); !strings.Contains(sh.out, "LFU") {
		t.Fatalf("answer not recorded: %s", sh.out)
	}

	// Without a terminal nothing prompts: the command says what is missing.
	if r = ka(t, dir, "new"); r.code != 2 || !strings.Contains(r.err, "title is required") {
		t.Fatalf("non-interactive new: %+v", r)
	}
}

func TestTokensAndRemoteRefusal(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	ka(t, dir, "init", "--project", "demo", dir)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")
	// Remote mode without authentication is refused.
	if r := ka(t, dir, "daemon", "run", "--listen", "0.0.0.0:0"); r.code == 0 || !strings.Contains(r.err, "without authentication") || !strings.Contains(r.err, "ka token create") {
		t.Fatalf("remote without auth: %+v", r)
	}
	r := ka(t, dir, "token", "create", "--actor", "bob", "--note", "ci")
	tok := strings.TrimSpace(r.out)
	if r.code != 0 || !strings.HasPrefix(tok, "ka_") || !strings.Contains(r.err, "shown only now") {
		t.Fatalf("create: %+v", r)
	}
	ls := ka(t, dir, "token", "ls")
	if !strings.Contains(ls.out, "bob") || strings.Contains(ls.out, tok) {
		t.Fatalf("ls must list the token without revealing it: %+v", ls)
	}
	id := strings.Fields(ls.out)[0]
	if r := ka(t, dir, "token", "rm", id); r.code != 0 {
		t.Fatalf("rm: %+v", r)
	}
	if r := ka(t, dir, "token", "ls"); !strings.Contains(r.out, "no API tokens") {
		t.Fatalf("after rm: %+v", r)
	}
}

func TestStoreAddBareRepo(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	home := t.TempDir()
	if res := ka(t, home, "store", "add", r.Path, "--name", "team"); res.code != 0 {
		t.Fatalf("store add: %+v", res)
	}
	if res := ka(t, home, "new", "-t", "From a teammate"); res.code != 0 || !strings.Contains(res.out, "pushed") {
		t.Fatalf("new via managed clone: %+v", res)
	}
}
