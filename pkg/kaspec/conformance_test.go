package kaspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const specDir = "../../spec/v1"

// TestConformance runs every case in spec/v1/conformance. Each case is a
// workspace of store directories plus expect.yml listing the exact multiset
// of "<code> <path>" findings (none for valid cases).
func TestConformance(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join(specDir, "conformance", "*", "*", "expect.yml"))
	if len(cases) == 0 {
		t.Fatal("no conformance cases found")
	}
	for _, exp := range cases {
		dir := filepath.Dir(exp)
		name, _ := filepath.Rel(filepath.Join(specDir, "conformance"), dir)
		t.Run(filepath.ToSlash(name), func(t *testing.T) {
			data, err := os.ReadFile(exp)
			if err != nil {
				t.Fatal(err)
			}
			n, err := ParseYAML(data)
			if err != nil {
				t.Fatalf("expect.yml: %v", err)
			}
			var stores, want []string
			for _, it := range n.Map["stores"].Items {
				stores = append(stores, it.Value)
			}
			if e := n.Map["expect"]; e != nil {
				for _, it := range e.Items {
					want = append(want, it.Value)
				}
			}
			var now time.Time
			if v := n.Map["now"]; v != nil {
				now, _ = time.Parse(time.RFC3339, v.Value)
			}
			KaVersion = ""
			if v := n.Map["ka_version"]; v != nil {
				KaVersion = v.Value
			}
			ws := testWorkspace(t, dir, stores)
			diags := Check(ws, CheckOptions{Now: now})
			KaVersion = ""
			var got []string
			for _, d := range diags {
				got = append(got, d.Code+" "+d.Path)
			}
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				var full []string
				for _, d := range diags {
					full = append(full, "  "+d.String())
				}
				t.Errorf("findings differ\nwant:\n  %s\ngot:\n  %s\ndetail:\n%s",
					strings.Join(want, "\n  "), strings.Join(got, "\n  "), strings.Join(full, "\n"))
			}
			if strings.HasPrefix(name, "valid") {
				checkSchemas(t, ws, dir, stores)
			}
		})
	}
}

// testWorkspace loads stores from a case directory; extends URLs are paths
// relative to the extending store.
func testWorkspace(t *testing.T, caseDir string, stores []string) *Workspace {
	t.Helper()
	ws := NewWorkspace(nil)
	ws.Resolve = func(from *Store, p Parent) (fs.FS, string, string, error) {
		if !strings.HasPrefix(p.URL, ".") {
			return nil, "", "", fmt.Errorf("no local copy of %s", p.URL)
		}
		dir := filepath.Clean(filepath.Join(ws.KeyOf(from), p.URL))
		if _, err := os.Stat(filepath.Join(dir, ".ka", "store.yml")); err != nil {
			return nil, "", "", fmt.Errorf("%s is not a store", dir)
		}
		return os.DirFS(dir), dir, filepath.Base(dir), nil
	}
	for _, s := range stores {
		dir := filepath.Join(caseDir, s)
		ws.Add(os.DirFS(dir), dir, s)
	}
	return ws
}

// checkSchemas validates every file of a valid case against the published
// JSON Schemas, so the schemas cannot drift from the Go validator.
func checkSchemas(t *testing.T, ws *Workspace, caseDir string, stores []string) {
	t.Helper()
	c := jsonschema.NewCompiler()
	schemas := map[string]*jsonschema.Schema{}
	for _, name := range []string{"ticket", "comment", "store", "project", "config", "actors", "board"} {
		path, _ := filepath.Abs(filepath.Join(specDir, "schema", name+".schema.json"))
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		id := "https://kanartist.dev/spec/v1/schema/" + name + ".schema.json"
		if err := c.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ticket", "comment", "store", "project", "config", "actors", "board"} {
		s, err := c.Compile("https://kanartist.dev/spec/v1/schema/" + name + ".schema.json")
		if err != nil {
			t.Fatalf("compile %s schema: %v", name, err)
		}
		schemas[name] = s
	}
	for _, st := range stores {
		root := filepath.Join(caseDir, st)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			var kind string
			doc := false
			switch {
			case rel == ".ka/store.yml":
				kind = "store"
			case rel == ".ka/config.yml":
				kind = "config"
			case rel == ".ka/actors.yml":
				kind = "actors"
			case strings.HasPrefix(rel, "boards/"):
				kind = "board"
			case strings.HasSuffix(rel, "/project.yml"):
				kind = "project"
			case strings.HasSuffix(rel, "/ticket.md"):
				kind, doc = "ticket", true
			case strings.Contains(rel, "/comments/"):
				kind, doc = "comment", true
			default:
				return nil
			}
			data, _ := os.ReadFile(p)
			if doc {
				d := splitDocument(rel, data)
				if d.fm == nil {
					t.Errorf("%s: no frontmatter", rel)
					return nil
				}
				data = []byte(nil)
				validate(t, schemas[kind], st+"/"+rel, d.fm)
				return nil
			}
			n, err := ParseYAML(data)
			if err != nil {
				t.Errorf("%s: %v", rel, err)
				return nil
			}
			validate(t, schemas[kind], st+"/"+rel, n)
			return nil
		})
	}
}

func validate(t *testing.T, s *jsonschema.Schema, name string, n *Node) {
	t.Helper()
	// round-trip through JSON so numbers have the types the validator expects
	raw, _ := json.Marshal(n.ToAny())
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Errorf("%s does not match its JSON Schema: %v", name, err)
	}
}

// TestSpecCodeTable keeps spec §9.2 and the Codes registry identical.
func TestSpecCodeTable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(specDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| (error|warning|info) \\|")
	spec := map[string]Severity{}
	for _, m := range row.FindAllStringSubmatch(string(data), -1) {
		spec[m[1]] = Severity(m[2])
	}
	for code, sev := range Codes {
		if spec[code] != sev {
			t.Errorf("code %s: registry says %s, spec says %q", code, sev, spec[code])
		}
	}
	for code := range spec {
		if _, ok := Codes[code]; !ok {
			t.Errorf("code %s is in the spec but not the registry", code)
		}
	}
}

// TestSpecDefaults keeps spec/v1/defaults.yml identical to the embedded copy.
func TestSpecDefaults(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(specDir, "defaults.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, defaultsYAML) {
		t.Error("spec/v1/defaults.yml and pkg/kaspec/defaults.yml differ")
	}
}

// TestConformanceCoversCodes makes sure every diagnostic code is exercised by
// at least one conformance case.
func TestConformanceCoversCodes(t *testing.T) {
	seen := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(specDir, "conformance", "*", "*", "expect.yml"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(data), "\n") {
			if s, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
				seen[strings.Fields(s)[0]] = true
			}
		}
	}
	var missing []string
	for code := range Codes {
		if !seen[code] {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	// duplicate_id cannot arise from a filesystem (directory names are unique);
	// it is reserved for engines that index several sources.
	missing = slices.DeleteFunc(missing, func(s string) bool { return s == "duplicate_id" })
	if len(missing) > 0 {
		t.Errorf("codes without a conformance case: %s", strings.Join(missing, ", "))
	}
}

// TestSchemasReject guards against vacuous schemas: documents the Go validator
// rejects for config-independent reasons must fail schema validation too.
func TestSchemasReject(t *testing.T) {
	c := jsonschema.NewCompiler()
	path, _ := filepath.Abs(filepath.Join(specDir, "schema", "ticket.schema.json"))
	sch, err := c.Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	cpath, _ := filepath.Abs(filepath.Join(specDir, "schema", "comment.schema.json"))
	csch, err := c.Compile(cpath)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]*jsonschema.Schema{
		"id: JK-7QK3M\ntitle: x\nstatu: ready\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n":                sch,
		"id: JK-1234\ntitle: x\nstatus: ready\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n":                sch,
		"id: JK-7QK3M\ntitle: x\nstatus: ready\npriority: P5\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n": sch,
		"id: JK-7QK3M\ntitle: x\nstatus: ready\ncreated: 2026-11-07\nupdated: 2026-11-07T12:00:00Z\n":                         sch,
		"type: answer\nauthor: bsant\ncreated: 2026-11-07T12:00:00Z\n":                                                        csch,
		"type: note\nauthor: bsant\ncreated: 2026-11-07T12:00:00Z\noptions:\n  - a\n":                                         csch,
	}
	for src, s := range bad {
		n, err := ParseYAML([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(n.ToAny())
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if s.Validate(v) == nil {
			t.Errorf("schema accepted invalid document:\n%s", src)
		}
	}
}
