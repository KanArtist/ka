package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

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
