package kaspec

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// ---- canonical writer ----------------------------------------------------------

// Every string the writer emits must read back as the identical string, both
// under our profile and under a stock YAML parser WITH implicit typing.
func TestEmitRoundTripProperty(t *testing.T) {
	seeds := []string{
		"", " ", "a", "no", "No", "NULL", "~", "y", "on", "true", "1", "1.0", "1_000", "0x1F", "0o17", "0777",
		"1e5", ".5", "-.5", "+1", ".inf", ".NaN", "12:30", "2026-11-07", "2026-1-7 10:00", "2026-11-07T13:07:22Z",
		"01JC4N7Q2V8X", "20261107T120001Z-4F2A.md", "JK-7QK3M", "agent/claude", "https://x.y/z#L1",
		"jk://a/b@c#L4", "a: b", "a #b", "a#b", "trailing:", "- dash", "? q", "[x]", "{x}", "'q'", `"q"`,
		"%p", "@a", "`b`", "!t", "&a", "*a", "|", ">", "tab\there", "line\nbreak", "\x00nul", "café", "日本",
		"emoji 🚀", " ", "back\\slash", "1.0.3", "P1", "v1.2", "3 modules", "-", "--", "---", "...",
	}
	r := rand.New(rand.NewPCG(1, 2))
	alphabet := []rune("ab0129 :#-.,[]{}'\"\\\t\n_xXeE+@!&*|>?%`~é🚀")
	for range 5000 {
		n := r.IntN(12)
		var b strings.Builder
		for range n {
			b.WriteRune(alphabet[r.IntN(len(alphabet))])
		}
		seeds = append(seeds, b.String())
	}
	for _, s := range seeds {
		out := EmitYAML([]KV{{"k", s}})
		if s == "" {
			if out != "" {
				t.Errorf("empty string should be omitted, got %q", out)
			}
			continue
		}
		n, err := ParseYAML([]byte(out))
		if err != nil {
			t.Errorf("%q emitted as %q does not parse: %v", s, out, err)
			continue
		}
		if v := n.Map["k"]; v == nil || v.Kind != ScalarNode || v.Value != s {
			t.Errorf("%q emitted as %q reads back as %#v", s, out, v)
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(out), &m); err != nil {
			t.Errorf("%q emitted as %q: stock parser error %v", s, out, err)
			continue
		}
		if got, ok := m["k"].(string); !ok || got != s {
			t.Errorf("%q emitted as %q: stock parser reads %#v (%T)", s, out, m["k"], m["k"])
		}
	}
}

func TestEmitShapes(t *testing.T) {
	got := EmitYAML([]KV{
		{"a", "x"},
		{"skip", ""},
		{"list", []any{"p", "q"}},
		{"empty", []any{}},
		{"maps", []any{[]KV{{"text", "t1"}, {"done", Raw("true")}}, []KV{{"text", "t2"}}}},
		{"nested", []KV{{"k", Raw("1")}, {"deeper", []KV{{"z", "w"}}}}},
	})
	want := `a: x
list:
  - p
  - q
maps:
  - text: t1
    done: true
  - text: t2
nested:
  k: 1
  deeper:
    z: w
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// ---- YAML profile ---------------------------------------------------------------

func TestYAMLProfile(t *testing.T) {
	cases := []struct {
		src, code string
	}{
		{"a: &x 1\nb: *x\n", "yaml_feature"},
		{"a: !!str 1\n", "yaml_feature"},
		{"base: {a: 1}\nm:\n  <<: {a: 2}\n", "yaml_feature"},
		{"a: 1\na: 2\n", "duplicate_key"},
		{"a: [1\n", "yaml_syntax"},
		{"---\na: 1\n---\nb: 2\n", "yaml_feature"},
	}
	for _, c := range cases {
		_, err := ParseYAML([]byte(c.src))
		ye, ok := err.(*YAMLError)
		if !ok || ye.Code != c.code {
			t.Errorf("%q: got %v, want %s", c.src, err, c.code)
		}
	}
	n, err := ParseYAML([]byte("a: no\nb: \"1\"\nc:\nd: ~\ne: 1.10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Map["a"].Value != "no" || n.Map["a"].Kind != ScalarNode {
		t.Error("plain `no` must stay the string no")
	}
	if !n.Map["b"].Quoted {
		t.Error("quoted scalar must be marked Quoted")
	}
	if n.Map["c"].Kind != NullNode || n.Map["d"].Kind != NullNode {
		t.Error("empty and ~ are null")
	}
	if n.Map["e"].Value != "1.10" {
		t.Error("no implicit typing: 1.10 stays 1.10")
	}
}

// ---- tickets and comments ----------------------------------------------------------

const canonicalTicket = `---
id: JK-7QK3M
title: "Fix: the cache"
status: in_progress
priority: P1
kind: bug
labels:
  - a
  - b
assignee: agent/claude
claim:
  by: agent/claude
  session: 01JC4N7Q2V8X
  at: 2026-11-07T13:07:22Z
blocked_by:
  - GYG-K2M7P
acceptance:
  - text: it works
    done: true
fields:
  points: 3
  team: build
created: 2026-11-07T12:00:00Z
updated: 2026-11-07T13:07:22Z
---

## Problem

Body.
`

func TestTicketCanonicalRoundTrip(t *testing.T) {
	tk, diags := ParseTicket("t.md", []byte(canonicalTicket))
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got := string(FormatTicket(tk)); got != canonicalTicket {
		t.Errorf("round trip changed the file:\n%s", got)
	}
	if !tk.Acceptance[0].Done || tk.Claim.Session != "01JC4N7Q2V8X" || !tk.Fields["points"].Typed {
		t.Errorf("fields not decoded: %+v", tk)
	}
}

func TestTicketFormatNormalizes(t *testing.T) {
	messy := "---\r\ntitle: x\r\nid: JK-7QK3M\r\nstatus: ready\r\nlabels: [b, a, b]\r\nassignee: \"\"\r\ncreated: 2026-11-07T12:00:00Z\r\nupdated: 2026-11-07T12:00:00Z\r\n---\r\n\r\n\r\nBody   \r\n\r\n"
	tk, diags := ParseTicket("t.md", []byte(messy))
	codes := codesOf(diags)
	if !slices.Contains(codes, "crlf") || slices.Contains(codes, "non_canonical") {
		t.Errorf("want crlf (and non_canonical suppressed), got %v", codes)
	}
	want := "---\nid: JK-7QK3M\ntitle: x\nstatus: ready\nlabels:\n  - a\n  - b\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n\nBody\n"
	if got := string(FormatTicket(tk)); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestUnknownKeySuggestsFix(t *testing.T) {
	_, diags := ParseTicket("t.md", []byte("---\nid: JK-7QK3M\ntitle: x\nstatu: ready\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n"))
	var found bool
	for _, d := range diags {
		if d.Code == "unknown_key" && strings.Contains(d.Fix, "status") && d.Line == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("want unknown_key on line 4 suggesting status, got %v", diags)
	}
}

func TestCommentRoundTrip(t *testing.T) {
	src := "---\ntype: answer\nauthor: bsant\ncreated: 2026-11-07T12:05:02Z\nanswers: 20261107T120001Z-4F2A.md\nchoice: 1\n---\n\nSnapshot them.\n"
	c, diags := ParseComment("c.md", []byte(src))
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got := string(FormatComment(c)); got != src {
		t.Errorf("round trip changed the file:\n%s", got)
	}
	if CommentFilename(c.Created, "4F2A") != "20261107T120502Z-4F2A.md" {
		t.Error("CommentFilename")
	}
}

func codesOf(ds []Diagnostic) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

// ---- config layering -------------------------------------------------------------

func layer(t *testing.T, name, src string) ConfigLayer {
	t.Helper()
	n, err := ParseYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return ConfigLayer{Name: name, Path: name, Node: n}
}

func TestConfigMergeRules(t *testing.T) {
	cfg, diags := ResolveConfig([]ConfigLayer{
		layer(t, "org", "kinds: [feature, bug, chore]\nfields:\n  estimate: {type: string}\n  team: {type: string}\nclaims: {stale_after: 7d}\n"),
		layer(t, "jk", "kinds: {$append: [spike], $remove: [chore]}\nfields:\n  team: null\n  points: {type: integer}\n"),
		layer(t, "project", "labels: {closed: true, known: [infra]}\nid_suffix_length: 6\n"),
	})
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if !slices.Equal(cfg.Kinds, []string{"feature", "bug", "spike"}) {
		t.Errorf("kinds = %v", cfg.Kinds)
	}
	if _, ok := cfg.Fields["team"]; ok {
		t.Error("team should be deleted by null")
	}
	if cfg.Fields["estimate"].Type != "string" || cfg.Fields["points"].Type != "integer" {
		t.Errorf("fields = %v", cfg.Fields)
	}
	if cfg.ClaimsStaleAfter != 7*24*time.Hour || cfg.IDSuffixLength != 6 || !cfg.LabelsClosed {
		t.Errorf("scalars: %+v", cfg)
	}
	if len(cfg.Statuses) != 8 || cfg.Statuses[3].Name != "in_progress" {
		t.Errorf("default workflow not inherited: %v", cfg.Statuses)
	}
	if cfg.Sources["kinds"] != "jk" || cfg.Sources["id_suffix_length"] != "project" || cfg.Sources["workflow"] != "built-in defaults" {
		t.Errorf("sources: %v", cfg.Sources)
	}
}

func TestConfigListsReplaceWholesale(t *testing.T) {
	cfg, _ := ResolveConfig([]ConfigLayer{layer(t, "org", "workflow:\n  statuses:\n    - {name: todo, category: unstarted}\n    - {name: done, category: completed}\n  initial: todo\n")})
	if !slices.Equal(cfg.StatusNames(), []string{"todo", "done"}) {
		t.Errorf("statuses = %v", cfg.StatusNames())
	}
}

func TestConfigLocks(t *testing.T) {
	cfg, diags := ResolveConfig([]ConfigLayer{
		layer(t, "org", "locked: [workflow, fields.security]\nfields:\n  security: {type: boolean}\n  team: {type: string}\n"),
		layer(t, "jk", "workflow: {initial: ready}\nfields:\n  team: null\n  security: {type: string}\n"),
		layer(t, "project", "fields: null\n"),
	})
	locked := 0
	for _, d := range diags {
		if d.Code == "config_locked" {
			locked++
		}
	}
	if locked != 3 { // workflow, fields.security, fields (contains a locked key)
		t.Errorf("want 3 config_locked, got %v", diags)
	}
	if cfg.Initial != "backlog" || cfg.Fields["security"].Type != "boolean" {
		t.Errorf("locked values must win: initial=%s security=%v", cfg.Initial, cfg.Fields["security"])
	}
	if _, ok := cfg.Fields["team"]; ok {
		t.Error("unlocked sibling may still be deleted")
	}
	if cfg.LockedBy["workflow"] != "org" {
		t.Errorf("LockedBy = %v", cfg.LockedBy)
	}
}

func TestConfigLayerCanSetItsOwnLock(t *testing.T) {
	cfg, diags := ResolveConfig([]ConfigLayer{layer(t, "org", "locked: [kinds]\nkinds: [feature]\n")})
	if len(diags) != 0 || !slices.Equal(cfg.Kinds, []string{"feature"}) {
		t.Errorf("a layer may set the keys it locks: %v %v", cfg.Kinds, diags)
	}
}

// ---- ids --------------------------------------------------------------------------

func TestIDs(t *testing.T) {
	for range 1000 {
		id := NewID("JK", 5)
		if _, _, ok := ParseID(id); !ok {
			t.Fatalf("NewID produced invalid %q", id)
		}
	}
	for _, bad := range []string{"JK-1234", "JK-ILOU0", "jk-7QK3M", "JK-7QK3M9AB1", "J_K-7QK3M"} {
		if _, _, ok := ParseID(bad); ok {
			t.Errorf("%q should be invalid", bad)
		}
	}
	for s, want := range map[string]bool{"AAAAA": true, "AABAB": true, "ABCDE": true, "EDCBA": true, "12345": true, "7QK3M": false, "0WN70": false} {
		if LowEntropy(s) != want {
			t.Errorf("LowEntropy(%s) = %v", s, !want)
		}
	}
	if HashBase32("x", 5) != HashBase32("x", 5) || HashBase32("x", 5) == HashBase32("y", 5) {
		t.Error("HashBase32 must be deterministic and seed-dependent")
	}
	if DefaultPrefix("gyggly-java") != "GYGGLYJAVA" {
		t.Error(DefaultPrefix("gyggly-java"))
	}
}

func TestPlaceholders(t *testing.T) {
	cases := map[string]bool{
		"## Problem\n\n-\n\n## Goals\n\nx":            true,
		"## Acceptance\n\n- [ ]":                      true,
		"Version {{ v }} here":                        true,
		"Use `{{ v }}` in code":                       false,
		"```\n## h\n- [ ]\n```\n":                     false,
		"## Phase 1\n\n### Step\n\nDo it.":            false,
		"## Refs\n\n<!-- hint -->\n-\n\n# Title\n\nx": true,
		"Plain body.":                                 false,
	}
	for body, want := range cases {
		_, what := findPlaceholder(body)
		if (what != "") != want {
			t.Errorf("%q: placeholder=%q, want %v", body, what, want)
		}
	}
}
