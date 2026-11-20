package scenario

import (
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/testkit"
)

// Over MCP, ka_compact without confirm only reports; a wrong confirm is an
// error; the store name compacts. ka_history reads the result.
func TestCompactOverMCP(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	adir := r.Clone()
	a := testkit.Service(t, adir)
	create(t, a, "One")
	head := testkit.Git(t, adir, "rev-parse", "HEAD")
	ag := connect(t, a, "claude")

	text, sc := ag.ok("ka_compact", map[string]any{"store": "test"})
	if sc["dry_run"] != true || !strings.Contains(text, `confirm: "test"`) || testkit.Git(t, adir, "rev-parse", "HEAD") != head {
		t.Fatalf("without confirm: %s", text)
	}
	if _, _, isErr := ag.call("ka_compact", map[string]any{"store": "test", "confirm": "yes"}); !isErr {
		t.Fatal("a confirm other than the store name was accepted")
	}
	_, sc = ag.ok("ka_compact", map[string]any{"store": "test", "confirm": "test", "summary": "Early days."})
	if sc["root"] == nil || sc["root"] != remoteMain(t, r) {
		t.Fatalf("compact: %v", sc)
	}
	text, _ = ag.ok("ka_history", map[string]any{"store": "test"})
	name := strings.TrimSuffix(strings.Fields(strings.TrimPrefix(text, "test history/"))[0], ":")
	if text, _ = ag.ok("ka_history", map[string]any{"file": name}); !strings.Contains(text, "Early days.") {
		t.Fatalf("ka_history file: %s", text)
	}
}
