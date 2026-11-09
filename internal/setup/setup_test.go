package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeOpts(t *testing.T, bins ...string) (Options, *[]string) {
	t.Helper()
	var calls []string
	have := map[string]bool{}
	for _, b := range bins {
		have[b] = true
	}
	return Options{
		Ka:   "/usr/local/bin/ka",
		Home: t.TempDir(),
		LookPath: func(n string) (string, error) {
			if have[n] {
				return "/bin/" + n, nil
			}
			return "", errors.New("not found")
		},
		Run: func(_ context.Context, name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return nil
		},
	}, &calls
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOnlyDetectedAgentsAndSkills(t *testing.T) {
	o, calls := fakeOpts(t, "claude", "agy")
	res, err := Run(context.Background(), o, "SKILL")
	if err != nil {
		t.Fatal(err)
	}
	var agents []string
	for _, r := range res {
		if r.Status != "configured" {
			t.Fatalf("%+v", r)
		}
		if r.Agent != "skill" {
			agents = append(agents, r.Agent)
		}
	}
	if strings.Join(agents, ",") != "claude,agy" {
		t.Fatalf("agents: %v", agents)
	}
	want := "claude mcp add -s user -e KA_ACTOR=agent/claude --transport stdio ka -- /usr/local/bin/ka mcp"
	if len(*calls) != 2 || (*calls)[1] != want {
		t.Fatalf("claude calls: %q", *calls)
	}
	m := readJSON(t, filepath.Join(o.Home, ".gemini", "config", "mcp_config.json"))
	ka := m["mcpServers"].(map[string]any)["ka"].(map[string]any)
	if ka["command"] != "/usr/local/bin/ka" || ka["env"].(map[string]any)["KA_ACTOR"] != "agent/agy" {
		t.Fatalf("agy config: %v", m)
	}
	for _, d := range []string{".agents/skills", ".claude/skills", ".gemini/config/skills"} {
		if b, err := os.ReadFile(filepath.Join(o.Home, d, "ka", "SKILL.md")); err != nil || string(b) != "SKILL" {
			t.Fatalf("skill in %s: %q %v", d, b, err)
		}
	}
}

func TestMergeKeepsOtherServersAndIsIdempotent(t *testing.T) {
	o, _ := fakeOpts(t)
	p := filepath.Join(o.Home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"mcpServers":{"other":{"command":"x"}},"theme":"dark"}`), 0o600)
	o.Only = []string{"cursor"}
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), o, "S"); err != nil {
			t.Fatal(err)
		}
	}
	m := readJSON(t, p)
	srv := m["mcpServers"].(map[string]any)
	if m["theme"] != "dark" || srv["other"] == nil || srv["ka"] == nil || len(srv) != 2 {
		t.Fatalf("merged: %v", m)
	}
	// Windows has no Unix permission bits to keep.
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
}

func TestMuseDefaultsAndNonJSON(t *testing.T) {
	o, _ := fakeOpts(t, "muse", "amp")
	if _, err := Run(context.Background(), o, "S"); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, filepath.Join(o.Home, ".config", "muse", "settings.json"))
	if m["schema_version"] != float64(1) || m["mcp_servers"].(map[string]any)["ka"].(map[string]any)["mode"] != "optional" {
		t.Fatalf("muse: %v", m)
	}
	if readJSON(t, filepath.Join(o.Home, ".config", "amp", "settings.json"))["amp.mcpServers"] == nil {
		t.Fatal("amp key")
	}
	// A config with comments is left alone and reported.
	p := filepath.Join(o.Home, ".config", "muse", "settings.json")
	os.WriteFile(p, []byte("// mine\n{}"), 0o644)
	res, _ := Run(context.Background(), Options{Ka: o.Ka, Home: o.Home, Only: []string{"muse"}, LookPath: o.LookPath}, "S")
	if res[0].Status != "failed" || !strings.Contains(res[0].Detail, "by hand") {
		t.Fatalf("non-JSON: %+v", res[0])
	}
	if b, _ := os.ReadFile(p); string(b) != "// mine\n{}" {
		t.Fatal("rewrote a file it could not parse")
	}
}

func TestDryRunAndUnknown(t *testing.T) {
	o, calls := fakeOpts(t, "codex")
	o.DryRun = true
	res, err := Run(context.Background(), o, "S")
	if err != nil || len(*calls) != 0 || res[0].Status != "planned" {
		t.Fatalf("dry run: %+v %v %q", res, err, *calls)
	}
	if _, err := os.Stat(filepath.Join(o.Home, ".agents")); err == nil {
		t.Fatal("dry run wrote files")
	}
	if _, err := Run(context.Background(), Options{Only: []string{"nope"}}, "S"); err == nil {
		t.Fatal("unknown agent accepted")
	}
}
