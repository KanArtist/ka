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
	if r := ka(t, dir, "daemon", "--listen", "0.0.0.0:0"); r.code != 1 || !strings.Contains(r.err, "loopback") {
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
