package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// ---- corpus rules ------------------------------------------------------------
//
// The v0 corpus is one specific repository; these tables encode what we know
// about it. They are data, so a re-run with new tickets applies the same rules.

// assigneeAliases normalizes v0 assignee spellings (lower-cased, trimmed) to
// v1 actor handles. An empty value means "no assignee".
var assigneeAliases = map[string]string{
	"":           "",
	"unknown":    "",
	"bsant":      "bsant",
	"bryan.sant": "bsant",
	"bryan sant": "bsant",
	"claude":     "agent/claude",
}

// archiveProject routes tickets under projects/<key>/archive/ to another
// project. Gyggly's archive holds the abandoned Java build (GYG-1..GYG-100),
// which belongs to the archived gyggly-java project.
var archiveProject = map[string]string{"gyggly": "gyggly-java"}

// archivedProjects are migrated with `archived: true`.
var archivedProjects = map[string]bool{"gyggly-java": true}

// splitStore is the home store of each project in the split layout; projects
// not listed get a store named after their key.
var splitStore = map[string]string{"gyggly-java": "gyggly"}

// kindMap renames v0 kinds; any other value outside the v1 kinds becomes feature.
var kindMap = map[string]string{"go-do": "feature"}

// extensionFields are v0 extension keys carried as declared custom fields.
var extensionFields = []string{"anti_goal", "branch", "deliverable", "estimate", "order", "shipped", "source"}

// v0Keys are the frontmatter keys the migrator understands.
var v0Keys = map[string]bool{
	"id": true, "project": true, "title": true, "status": true, "priority": true, "kind": true,
	"assignee": true, "claimed_at": true, "depends_on": true, "labels": true, "links": true,
	"created": true, "updated": true, "status_note": true,
	"anti_goal": true, "branch": true, "deliverable": true, "estimate": true, "order": true, "shipped": true, "source": true,
}

// ---- v0 model ------------------------------------------------------------------

type v0Project struct {
	Key, Name, Forge, Prefix, Description string
}

type v0Ticket struct {
	Base     string // file basename: the stable identity of a v0 ticket
	Rel      string // path relative to the v0 root
	Project  string // target v1 project key
	LegacyID string
	Meta     *kaspec.Node
	Body     string
	HadCRLF  bool
}

func (t *v0Ticket) str(key string) string {
	if t.Meta == nil || t.Meta.Map[key] == nil {
		return ""
	}
	n := t.Meta.Map[key]
	switch n.Kind {
	case kaspec.ScalarNode:
		return strings.TrimSpace(n.Value)
	case kaspec.SeqNode:
		var parts []string
		for _, it := range n.Items {
			parts = append(parts, strings.TrimSpace(it.Value))
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func (t *v0Ticket) list(key string) []string {
	if t.Meta == nil || t.Meta.Map[key] == nil {
		return nil
	}
	n := t.Meta.Map[key]
	var out []string
	switch n.Kind {
	case kaspec.SeqNode:
		for _, it := range n.Items {
			if it.Kind == kaspec.ScalarNode && strings.TrimSpace(it.Value) != "" {
				out = append(out, strings.TrimSpace(it.Value))
			} else if it.Kind == kaspec.MapNode {
				out = append(out, linkFromMap(it))
			}
		}
	case kaspec.ScalarNode:
		if s := strings.TrimSpace(n.Value); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// linkFromMap turns a v0 {repo, path, ref, line} link object into compact form.
func linkFromMap(n *kaspec.Node) string {
	get := func(k string) string {
		if v := n.Map[k]; v != nil && v.Kind == kaspec.ScalarNode {
			return strings.TrimSpace(v.Value)
		}
		return ""
	}
	s := get("repo") + "://" + get("path")
	if r := get("ref"); r != "" {
		s += "@" + r
	}
	if l := get("line"); l != "" {
		s += "#L" + l
	}
	return s
}

type v0Corpus struct {
	Root     string
	Projects map[string]*v0Project
	Keys     []string // project keys (sorted), including archive targets
	Tickets  []*v0Ticket
	Problems []string // tickets that could not be read
}

func loadV0(root string) (*v0Corpus, error) {
	c := &v0Corpus{Root: root, Projects: map[string]*v0Project{}}
	pdir := filepath.Join(root, "projects")
	ents, err := os.ReadDir(pdir)
	if err != nil {
		return nil, fmt.Errorf("%s does not look like a KanArtist v0 repo: %w", root, err)
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		key := e.Name()
		p, err := loadV0Project(filepath.Join(pdir, key, "project.yml"), key)
		if err != nil {
			return nil, err
		}
		c.Projects[key] = p
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		key := e.Name()
		for _, sub := range []string{"tickets", "archive"} {
			base := filepath.Join(pdir, key, sub)
			filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
					return nil
				}
				target := key
				if sub == "archive" && archiveProject[key] != "" {
					target = archiveProject[key]
				}
				t, err := loadV0Ticket(root, p, target)
				if err != nil {
					rel, _ := filepath.Rel(root, p)
					c.Problems = append(c.Problems, fmt.Sprintf("%s: %v", filepath.ToSlash(rel), err))
					return nil
				}
				c.Tickets = append(c.Tickets, t)
				return nil
			})
		}
	}
	for k := range c.Projects {
		c.Keys = append(c.Keys, k)
	}
	sort.Strings(c.Keys)
	sort.Slice(c.Tickets, func(i, j int) bool { return c.Tickets[i].Rel < c.Tickets[j].Rel })
	return c, nil
}

func loadV0Project(path, key string) (*v0Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("project %s: %w", key, err)
	}
	data = kaspec.NormalizeEncoding(data)
	n, err := kaspec.ParseYAML(data)
	if err != nil {
		return nil, fmt.Errorf("project %s: %w", key, err)
	}
	get := func(k string) string {
		if v := n.Map[k]; v != nil && v.Kind == kaspec.ScalarNode {
			return strings.TrimSpace(v.Value)
		}
		return ""
	}
	p := &v0Project{Key: key, Name: get("name"), Forge: get("forge"), Prefix: get("id_prefix")}
	if p.Prefix == "" {
		p.Prefix = kaspec.DefaultPrefix(key)
	}
	// leading comment lines become the description
	var desc []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		desc = append(desc, strings.TrimSpace(strings.TrimPrefix(line, "#")))
	}
	p.Description = strings.Join(desc, " ")
	return p, nil
}

func loadV0Ticket(root, path, project string) (*v0Ticket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hadCR := strings.ContainsRune(string(data), '\r')
	text := string(kaspec.NormalizeEncoding(data))
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("no frontmatter")
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("frontmatter not closed")
	}
	fm := text[4 : 4+end+1]
	body := text[4+end+5:]
	n, err := kaspec.ParseYAML([]byte(v0Requote(fm)))
	if err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	rel, _ := filepath.Rel(root, path)
	t := &v0Ticket{Base: filepath.Base(path), Rel: filepath.ToSlash(rel), Project: project, Meta: n, Body: body, HadCRLF: hadCR}
	t.LegacyID = t.str("id")
	if t.LegacyID == "" {
		return nil, fmt.Errorf("no id")
	}
	return t, nil
}

var quotedLineRe = regexp.MustCompile(`^(\s*(?:- )?(?:[A-Za-z0-9_]+: )?)"(.*)"\s*$`)

// v0Requote re-encodes double-quoted scalars with v0's semantics. v0's reader
// treated only \" and \\ as escapes and kept every other backslash literally,
// so "\usr\bin" meant a backslash, "usr", a backslash, "bin". Read as YAML,
// "\b" would silently become a backspace. Each quoted value is decoded the v0
// way and re-escaped for YAML so it means exactly what v0 read.
func v0Requote(fm string) string {
	lines := strings.Split(fm, "\n")
	for i, l := range lines {
		m := quotedLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		inner := strings.ReplaceAll(strings.ReplaceAll(m[2], `\"`, `"`), `\\`, `\`)
		lines[i] = m[1] + `"` + strings.ReplaceAll(strings.ReplaceAll(inner, `\`, `\\`), `"`, `\"`) + `"`
	}
	return strings.Join(lines, "\n")
}
