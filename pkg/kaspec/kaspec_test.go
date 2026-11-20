package kaspec

import (
	"errors"
	"io/fs"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
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

// ---- review fixes: config values, ids, store loading -------------------------------

func TestConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		src   string
		check func(*Config) bool
	}{
		// invalid integers fall back to the built-in default, never to 0 ("no limit")
		{"id_suffix_length: 9\n", func(c *Config) bool { return c.IDSuffixLength == 5 }},
		{"id_suffix_length: 99999999999999999999\n", func(c *Config) bool { return c.IDSuffixLength == 5 }},
		{"comments: {max_bytes: -1}\n", func(c *Config) bool { return c.CommentsMaxBytes == 65536 }},
		{"comments: {max_bytes: lots}\n", func(c *Config) bool { return c.CommentsMaxBytes == 65536 }},
		{"attachments: {max_bytes: 99999999999999999999}\n", func(c *Config) bool { return c.AttachmentsMaxBytes == 5242880 }},
		{"labels: {closed: yes}\n", func(c *Config) bool { return !c.LabelsClosed }},
		{"labels: {closed: \"true\"}\n", func(c *Config) bool { return !c.LabelsClosed }},
		{"workflow:\n  statuses:\n    - {name: todo, category: unstarted, blocked: yes}\n  initial: todo\n", func(c *Config) bool { return !c.Statuses[0].Blocked }},
		{"claims: {stale_after: 200000d}\n", func(c *Config) bool { return c.ClaimsStaleAfter == 0 }},
		{"claims: {stale_after: 99999999999999999999d}\n", func(c *Config) bool { return c.ClaimsStaleAfter == 0 }},
	}
	for _, c := range cases {
		cfg, diags := ResolveConfig([]ConfigLayer{layer(t, "org", c.src)})
		if !slices.Contains(codesOf(diags), "config_invalid") || !c.check(cfg) {
			t.Errorf("%q: want config_invalid and a safe value, got %v", c.src, diags)
		}
	}
	cfg, diags := ResolveConfig([]ConfigLayer{layer(t, "org", "id_suffix_length: 8\nlabels: {closed: true}\n")})
	if len(diags) != 0 || cfg.IDSuffixLength != 8 || !cfg.LabelsClosed {
		t.Errorf("in-range values must load: %v %+v", diags, cfg)
	}
}

func TestParseDurationDays(t *testing.T) {
	if d, err := ParseDuration("7d"); err != nil || d != 7*24*time.Hour {
		t.Errorf("7d = %v, %v", d, err)
	}
	for _, s := range []string{"200000d", "99999999999999999999d"} {
		if d, err := ParseDuration(s); err == nil {
			t.Errorf("%s: want an error, got %v", s, d)
		}
	}
}

func TestNewIDClampsLength(t *testing.T) {
	for _, n := range []int{-1, 0, 4, 9, 100} {
		if id := NewID("JK", n); !IDRe.MatchString(id) {
			t.Errorf("NewID(JK, %d) = %q is not a valid ID", n, id)
		}
	}
}

func TestStoreFileSpec(t *testing.T) {
	cases := map[string]string{
		"spec: 1\nname: s\n":                    "",
		"spec: 0\nname: s\n":                    "spec_unsupported",
		"spec: 2\nname: s\n":                    "spec_unsupported",
		"spec: 99999999999999999999\nname: s\n": "invalid_value",
		"name: s\n":                             "missing_field",
	}
	for src, want := range cases {
		_, diags := ParseStoreFile("store.yml", []byte(src))
		got := codesOf(diags)
		if (want == "" && len(got) != 0) || (want != "" && !slices.Equal(got, []string{want})) {
			t.Errorf("%q: got %v, want %q", src, got, want)
		}
	}
}

func TestActorsRejectWrongShapes(t *testing.T) {
	for _, src := range []string{
		"humans:\n  bob: hello\n",
		"humans:\n  bob:\n    - x\n",
		"agents:\n  agent/x: hello\n",
	} {
		a, diags := ParseActors("actors.yml", []byte(src))
		if !slices.Equal(codesOf(diags), []string{"invalid_value"}) || len(a.Humans)+len(a.Agents) != 0 {
			t.Errorf("%q: got %v %+v", src, diags, a)
		}
	}
}

// failFS fails Open and ReadDir for the named paths, like EACCES or EIO would.
type failFS struct {
	fstest.MapFS
	fail map[string]bool
}

func (f failFS) Open(name string) (fs.File, error) {
	if f.fail[name] {
		return nil, &fs.PathError{Op: "open", Path: "/abs/" + name, Err: errors.New("input/output error")}
	}
	return f.MapFS.Open(name)
}

func (f failFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.fail[name] {
		return nil, &fs.PathError{Op: "readdir", Path: "/abs/" + name, Err: errors.New("input/output error")}
	}
	return f.MapFS.ReadDir(name)
}

func TestLoadStoreUnreadable(t *testing.T) {
	const tdir = "projects/jk/tickets/JK-7QK3M"
	base := func() fstest.MapFS {
		return fstest.MapFS{
			".ka/store.yml":             {Data: []byte("spec: 1\nname: s\n")},
			"projects/jk/project.yml":   {Data: []byte("key: jk\n")},
			tdir + "/ticket.md":         {Data: []byte(canonicalTicket)},
			tdir + "/comments/c.md.tmp": {Data: []byte("x")},
		}
	}
	link := &fstest.MapFile{Mode: fs.ModeSymlink, Data: []byte("/dev/zero")}
	cases := []struct {
		name string
		edit func(fstest.MapFS) fs.FS
		path string // the unreadable path
	}{
		{"symlinked ticket", func(m fstest.MapFS) fs.FS { m[tdir+"/ticket.md"] = link; return m }, tdir + "/ticket.md"},
		{"symlinked config", func(m fstest.MapFS) fs.FS { m[".ka/config.yml"] = link; return m }, ".ka/config.yml"},
		{"symlinked tickets dir", func(m fstest.MapFS) fs.FS {
			for k := range m {
				if strings.HasPrefix(k, "projects/jk/tickets/") {
					delete(m, k)
				}
			}
			m["projects/jk/tickets"] = link
			return m
		}, "projects/jk/tickets"},
		{"oversized comment", func(m fstest.MapFS) fs.FS {
			m[tdir+"/comments/20261107T120001Z-4F2A.md"] = &fstest.MapFile{Data: make([]byte, MaxFileBytes+1)}
			return m
		}, tdir + "/comments/20261107T120001Z-4F2A.md"},
		{"read error on ticket", func(m fstest.MapFS) fs.FS { return failFS{m, map[string]bool{tdir + "/ticket.md": true}} }, tdir + "/ticket.md"},
		{"read error on config", func(m fstest.MapFS) fs.FS {
			m[".ka/config.yml"] = &fstest.MapFile{Data: []byte("kinds: [x]\n")}
			return failFS{m, map[string]bool{".ka/config.yml": true}}
		}, ".ka/config.yml"},
		{"list error on tickets", func(m fstest.MapFS) fs.FS { return failFS{m, map[string]bool{"projects/jk/tickets": true}} }, "projects/jk/tickets"},
		{"symlinked .ka", func(m fstest.MapFS) fs.FS { delete(m, ".ka/store.yml"); m[".ka"] = link; return m }, ".ka"},
		{"symlinked project dir", func(m fstest.MapFS) fs.FS { m["projects/other"] = link; return m }, "projects/other"},
		{"symlinked ticket dir", func(m fstest.MapFS) fs.FS { m["projects/jk/tickets/JK-K2M7P"] = link; return m }, "projects/jk/tickets/JK-K2M7P"},
		{"symlinked comments dir", func(m fstest.MapFS) fs.FS { delete(m, tdir+"/comments/c.md.tmp"); m[tdir+"/comments"] = link; return m }, tdir + "/comments"},
		{"ticket.md is a directory", func(m fstest.MapFS) fs.FS {
			delete(m, tdir+"/ticket.md")
			m[tdir+"/ticket.md/x"] = &fstest.MapFile{Data: []byte("x")}
			return m
		}, tdir + "/ticket.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := LoadStore(c.edit(base()), "")
			var found bool
			for _, d := range s.Diags {
				switch {
				case d.Code == "unreadable" && d.Path == c.path:
					found = true
					if strings.Contains(d.Message, "/abs/") {
						t.Errorf("message leaks the absolute path: %s", d.Message)
					}
				case d.Code == "ticket_missing_file":
					t.Errorf("an unreadable file is not a missing one: %v", d)
				}
			}
			if !found {
				t.Errorf("want unreadable %s, got %v", c.path, s.Diags)
			}
		})
	}
	// a stray file is still just stray
	s := LoadStore(base(), "")
	if got := codesOf(s.Diags); !slices.Equal(got, []string{"stray_file"}) || len(s.Projects["jk"].IDs) != 1 {
		t.Errorf("baseline store: %v", s.Diags)
	}
}

// countFS counts path-based operations, to keep loading free of redundant I/O.
type countFS struct {
	fstest.MapFS
	ops map[string]int // "op path" -> count
}

func (c countFS) Open(name string) (fs.File, error) {
	c.ops["open "+name]++
	return c.MapFS.Open(name)
}

func (c countFS) ReadDir(name string) ([]fs.DirEntry, error) {
	c.ops["readdir "+name]++
	return c.MapFS.ReadDir(name)
}

func (c countFS) Lstat(name string) (fs.FileInfo, error) {
	c.ops["lstat "+name]++
	return c.MapFS.Lstat(name)
}

func (c countFS) Stat(name string) (fs.FileInfo, error) {
	c.ops["stat "+name]++
	return c.MapFS.Stat(name)
}

func TestLoadStoreIOPattern(t *testing.T) {
	const tdir = "projects/jk/tickets/JK-7QK3M"
	cfs := countFS{fstest.MapFS{
		".ka/store.yml":                             {Data: []byte("spec: 1\nname: s\n")},
		"projects/jk/project.yml":                   {Data: []byte("key: jk\n")},
		tdir + "/ticket.md":                         {Data: []byte(canonicalTicket)},
		tdir + "/comments/20261107T120001Z-4F2A.md": {Data: []byte("---\ntype: note\nauthor: bsant\ncreated: 2026-11-07T12:00:01Z\n---\n\nhi\n")},
		tdir + "/files/a/b.txt":                     {Data: []byte("x")},
	}, map[string]int{}}
	s := LoadStore(cfs, "")
	if len(s.Diags) != 0 || len(s.Projects["jk"].Tickets[tdir[len("projects/jk/tickets/"):]].Files) != 1 {
		t.Fatalf("store did not load cleanly: %v", s.Diags)
	}
	want := map[string]int{
		"readdir .": 1, "readdir .ka": 1, "open .ka/store.yml": 1,
		"readdir projects": 1, "readdir projects/jk": 1, "open projects/jk/project.yml": 1,
		"readdir projects/jk/tickets": 1, "readdir " + tdir: 1, "open " + tdir + "/ticket.md": 1,
		"readdir " + tdir + "/comments": 1, "open " + tdir + "/comments/20261107T120001Z-4F2A.md": 1,
		"readdir " + tdir + "/files": 1, "readdir " + tdir + "/files/a": 1,
	}
	if !maps.Equal(cfs.ops, want) {
		t.Errorf("I/O pattern:\n got %v\nwant %v", cfs.ops, want)
	}
}

func TestParentProblemsReportedOnEveryCheck(t *testing.T) {
	child := func(name string) fstest.MapFS {
		return fstest.MapFS{
			".ka/store.yml":                     {Data: []byte("spec: 1\nname: " + name + "\nextends:\n  - url: org\n")},
			"projects/" + name + "/project.yml": {Data: []byte("key: " + name + "\n")},
		}
	}
	for _, c := range []struct {
		name   string
		parent fstest.MapFS
		want   []string
	}{
		{"not a store", fstest.MapFS{"README": {Data: []byte("x")}}, []string{"config_parent_unavailable a/.ka/store.yml", "config_parent_unavailable b/.ka/store.yml", "invalid_store org/.ka/store.yml"}},
		{"bad parent config", fstest.MapFS{".ka/store.yml": {Data: []byte("spec: 1\nname: org\n")}, ".ka/config.yml": {Data: []byte("kinds: [x\n")}}, []string{"yaml_syntax org/.ka/config.yml"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := NewWorkspace(func(from *Store, p Parent) (fs.FS, string, string, error) { return c.parent, "org", "org", nil })
			ws.Add(child("ka"), "a", "a")
			ws.Add(child("kb"), "b", "b")
			for i := range 2 {
				var got []string
				for _, d := range Check(ws, CheckOptions{}) {
					got = append(got, d.Code+" "+d.Path)
				}
				if !slices.Equal(got, c.want) {
					t.Errorf("Check #%d: got %v, want %v", i+1, got, c.want)
				}
			}
		})
	}
}

func TestCycles(t *testing.T) {
	ticket := func(id, blockedBy string) *fstest.MapFile {
		src := "---\nid: " + id + "\ntitle: t\nstatus: ready\n"
		if blockedBy != "" {
			src += "blocked_by:\n"
			for _, b := range strings.Split(blockedBy, ",") {
				src += "  - " + b + "\n"
			}
		}
		return &fstest.MapFile{Data: []byte(src + "created: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n")}
	}
	cases := []struct {
		name    string
		tickets map[string]string // id -> blocked_by
		want    []string          // cycle messages
	}{
		{"two cycles through one node", map[string]string{"JK-AAAA1": "JK-BBBB2,JK-CCCC3", "JK-BBBB2": "JK-AAAA1", "JK-CCCC3": "JK-AAAA1"},
			[]string{"blocked_by cycle: JK-AAAA1 -> JK-BBBB2 -> JK-AAAA1", "blocked_by cycle: JK-AAAA1 -> JK-CCCC3 -> JK-AAAA1"}},
		{"edge with a former prefix", map[string]string{"JK-AAAA1": "OLD-BBBB2", "JK-BBBB2": "JK-AAAA1"},
			[]string{"blocked_by cycle: JK-AAAA1 -> JK-BBBB2 -> JK-AAAA1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := fstest.MapFS{
				".ka/store.yml":           {Data: []byte("spec: 1\nname: s\n")},
				"projects/jk/project.yml": {Data: []byte("key: jk\nprefix: JK\nformer_prefixes:\n  - OLD\n")},
			}
			for id, b := range c.tickets {
				m["projects/jk/tickets/"+id+"/ticket.md"] = ticket(id, b)
			}
			ws := NewWorkspace(nil)
			ws.Add(m, "s", "")
			var got []string
			for _, d := range Check(ws, CheckOptions{}) {
				if d.Code == "cycle" {
					got = append(got, d.Message)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestEmitEmptyMappingItem(t *testing.T) {
	got := EmitYAML([]KV{{"a", []any{[]KV{{"b", ""}}, "x"}}})
	if want := "a:\n  - {}\n  - x\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if n, err := ParseYAML([]byte(got)); err != nil || len(n.Map["a"].Items) != 2 {
		t.Errorf("does not read back: %v", err)
	}
}

func TestBodyLine(t *testing.T) {
	cases := map[string]int{
		"---\n---\n\n- [ ]\n":               3,
		"---\na: 1\n---\nbody\n---\nmore\n": 4,
		"---\r\na: 1\r\nb: 2\r\n---\r\nx":   5,
		"no frontmatter":                    1,
	}
	for src, want := range cases {
		if got := bodyLine([]byte(src)); got != want {
			t.Errorf("bodyLine(%q) = %d, want %d", src, got, want)
		}
	}
}

func TestFindSecret(t *testing.T) {
	cases := map[string]string{
		"key AKIAABCDEFGHIJKLMNOP here":                                 "AWS access key",
		"t=ghs_" + strings.Repeat("a", 36):                              "GitHub token",
		"gho_" + strings.Repeat("Z", 40):                                "GitHub token",
		"github_pat_" + strings.Repeat("a_", 25):                        "GitHub fine-grained token",
		"xoxr-1234567890":                                               "Slack token",
		"-----BEGIN OPENSSH PRIVATE KEY-----":                           "private key",
		"-----BEGIN PRIVATE KEY-----":                                   "private key",
		"sk-ant-" + strings.Repeat("x", 20):                             "Anthropic API key",
		"AIza" + strings.Repeat("0", 35):                                "Google API key",
		"AKIA too short":                                                "",
		"ghx_" + strings.Repeat("a", 36):                                "",
		"xoxq-1234567890":                                               "",
		"-----BEGIN CERTIFICATE-----":                                   "",
		"plain text with no credentials, {{ braces }} and sk-ant- tail": "",
	}
	for src, want := range cases {
		if got := findSecret([]byte(src)); got != want {
			t.Errorf("findSecret(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestNearest(t *testing.T) {
	cands := []string{"status", "statuses", "kind", "labels"}
	for s, want := range map[string]string{"statu": "status", "kinds": "kind", "lables": "labels", "x": "", "averylongkey": ""} {
		if got := nearest(s, cands); got != want {
			t.Errorf("nearest(%q) = %q, want %q", s, got, want)
		}
	}
	if levenshtein("kitten", "sitting") != 3 {
		t.Error("levenshtein")
	}
}
