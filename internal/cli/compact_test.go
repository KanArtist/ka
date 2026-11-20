package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/testkit"
)

// ka compact without --yes is a dry run; with it, the store is compacted and
// ka history shows the summary.
func TestCompactCommand(t *testing.T) {
	testkit.GitEnv(t)
	dir := testkit.NewRemote(t, nil).Clone()
	if r := ka(t, dir, "new", "-t", "Something to compact"); r.code != 0 {
		t.Fatalf("new: %+v", r)
	}
	head := testkit.Git(t, dir, "rev-parse", "HEAD")
	if r := ka(t, dir, "compact"); r.code != 2 {
		t.Fatalf("compact without --store: %+v", r)
	}
	r := ka(t, dir, "compact", "--store", "test")
	if r.code != 0 || !strings.Contains(r.out, "dry run: compacting store test") || !strings.Contains(r.out, "--yes") ||
		!strings.Contains(r.out, "raise requires_ka from none to 1.3.0") {
		t.Fatalf("compact without --yes: %+v", r)
	}
	if testkit.Git(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("compact without --yes changed the store")
	}
	notes := filepath.Join(t.TempDir(), "notes.md")
	os.WriteFile(notes, []byte("We fixed the cache.\n"), 0o644)
	r = ka(t, dir, "compact", "--store", "test", "--summary-file", notes, "--yes")
	if r.code != 0 || !strings.Contains(r.out, "compacted store test: 2 commits") {
		t.Fatalf("compact --yes: %+v", r)
	}
	if n := testkit.Git(t, dir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("%s commits after compacting", n)
	}
	r = ka(t, dir, "history")
	if r.code != 0 || !strings.Contains(r.out, "test history/") || !strings.Contains(r.out, "2 commits") {
		t.Fatalf("history: %+v", r)
	}
	name := strings.Fields(strings.TrimPrefix(r.out, "test history/"))[0]
	name = strings.TrimSuffix(name, ":")
	if r := ka(t, dir, "history", name); r.code != 0 || !strings.Contains(r.out, "We fixed the cache.") {
		t.Fatalf("history %s: %+v", name, r)
	}
}
