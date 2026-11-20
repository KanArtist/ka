package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/testkit"
)

func TestCLIInitiatives(t *testing.T) {
	testkit.GitEnv(t)
	dir := filepath.Join(t.TempDir(), "tickets")
	if r := ka(t, dir, "init", "--project", "demo", dir); r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "init")

	// Without requires_ka the first container is refused, with the fix.
	if r := ka(t, dir, "new", "-t", "GDPR", "--kind", "initiative"); r.code != 1 || !strings.Contains(r.err, "containers_unguarded") || !strings.Contains(r.err, "ka store require 1.3.0") {
		t.Fatalf("unguarded: %+v", r)
	}
	if r := ka(t, dir, "store", "require", "1.3.0"); r.code != 0 || !strings.Contains(r.out, "now requires ka 1.3.0") {
		t.Fatalf("store require: %+v", r)
	}
	if r := ka(t, dir, "store", "require", "1.2.0"); r.code != 1 || !strings.Contains(r.err, "never lowered") {
		t.Fatalf("store require lower: %+v", r)
	}
	if r := ka(t, dir, "store", "require", "1.3.0"); r.code != 0 || !strings.Contains(r.out, "(already)") {
		t.Fatalf("store require again: %+v", r)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, ".ka", "store.yml")); !strings.Contains(string(raw), "requires_ka: 1.3.0\n") {
		t.Fatalf("store.yml:\n%s", raw)
	}
	if r := ka(t, dir, "check"); r.code != 0 {
		t.Fatalf("check after require: %+v", r)
	}

	newID := func(args ...string) string {
		t.Helper()
		r := ka(t, dir, append([]string{"new", "--json"}, args...)...)
		var out struct{ Created []struct{ ID string } }
		if r.code != 0 || json.Unmarshal([]byte(r.out), &out) != nil {
			t.Fatalf("new %v: %+v", args, r)
		}
		return out.Created[0].ID
	}
	ini := newID("-t", "Windows support", "--kind", "initiative")
	epic := newID("-t", "Installer", "--kind", "epic", "--parent", ini)
	work := newID("-t", "Sign the MSI", "--parent", epic, "--status", "ready")
	newID("-t", "Port the path handling", "--label", "win", "--status", "ready")

	if r := ka(t, dir, "new", "-t", "Bad", "--kind", "epic", "--parent", work); r.code != 1 || !strings.Contains(r.err, "hierarchy_invalid") {
		t.Fatalf("epic under work: %+v", r)
	}
	if r := ka(t, dir, "initiative", "from-label", "win", "--title", "Windows support", "--dry-run"); r.code != 0 || !strings.Contains(r.out, "(reused)") || !strings.Contains(r.out, "dry run") {
		t.Fatalf("from-label dry run: %+v", r)
	}
	if r := ka(t, dir, "initiative", "from-label", "win", "--title", "Windows support"); r.code != 0 || !strings.Contains(r.out, "parented") {
		t.Fatalf("from-label: %+v", r)
	}
	r := ka(t, dir, "tree", ini)
	if r.code != 0 || !strings.Contains(r.out, "initiative backlog Windows support · 0/2 done, 2 ready") || !strings.Contains(r.out, "    "+work) {
		t.Fatalf("tree: %+v", r)
	}
	if r := ka(t, dir, "next", "--within", epic); r.code != 0 || !strings.Contains(r.out, work) {
		t.Fatalf("next --within: %+v", r)
	}
	if r := ka(t, dir, "ls", "epic:"+epic); r.code != 0 || !strings.Contains(r.out, work) {
		t.Fatalf("ls epic: %+v", r)
	}
}
