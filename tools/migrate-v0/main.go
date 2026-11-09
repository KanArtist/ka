// Command migrate-v0 imports a KanArtist v0 repository (Python-era layout)
// into Store Spec v1 stores. It is a one-time tool kept out of the ka binary,
// built to be re-run safely until cut-over:
//
//   - IDs are deterministic and stable across runs (see transform.go).
//   - Every file it writes is recorded in <out>/.migrate-v0-state.json. On a
//     re-run it overwrites a file only if the file still has the content it
//     last wrote; files edited since (for example by dogfooding v1) are left
//     alone and reported as conflicts unless --force is given.
//   - Files it wrote earlier that the v0 source no longer produces are removed
//     (again, only if unmodified). Anything it did not write is never touched.
//
// Usage:
//
//	go run ./tools/migrate-v0 --v0 ../kanartist --out ../ka-stores [--layout split|single] [--commit]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

type options struct {
	V0, Out, Layout, OrgURL string
	Commit, Force, DryRun   bool
	Now                     time.Time
}

func main() {
	var o options
	flag.StringVar(&o.V0, "v0", "", "path to the KanArtist v0 repository (required)")
	flag.StringVar(&o.Out, "out", "", "directory to hold the v1 store directories (required)")
	flag.StringVar(&o.Layout, "layout", "split", "split: one store per project plus org; single: everything in org")
	flag.StringVar(&o.OrgURL, "org-url", "../org", "the org store's URL, written into project stores' extends (split layout)")
	flag.BoolVar(&o.Commit, "commit", false, "commit the changes in each store directory that is a git repository")
	flag.BoolVar(&o.Force, "force", false, "overwrite or delete files even if they were modified since the last run")
	flag.BoolVar(&o.DryRun, "dry-run", false, "report what would change without writing anything")
	flag.Parse()
	if o.V0 == "" || o.Out == "" || (o.Layout != "split" && o.Layout != "single") {
		flag.Usage()
		os.Exit(2)
	}
	o.Now = time.Now().UTC()
	rep, err := run(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-v0:", err)
		os.Exit(1)
	}
	fmt.Print(rep.summary())
	if rep.checkErrors > 0 || len(rep.conflicts) > 0 {
		os.Exit(1)
	}
}

// plan is the full desired output: store name -> relative path -> bytes.
type plan map[string]map[string][]byte

func (p plan) put(store, rel string, data []byte) {
	if p[store] == nil {
		p[store] = map[string][]byte{}
	}
	p[store][rel] = data
}

type report struct {
	o           options
	v0Head      string
	corpus      *v0Corpus
	st          *stats
	before      map[string]map[string]int // project -> "status:x"/"kind:x" -> n
	after       map[string]map[string]int
	stores      map[string][]string // store -> projects
	created     int
	updated     int
	deleted     int
	unchanged   int
	conflicts   []string
	commits     []string
	diags       []kaspec.Diagnostic
	checkErrors int
	checkWarns  int
	checkInfos  int
	newIDs      int
	reusedIDs   int
}

func run(o options) (*report, error) {
	c, err := loadV0(o.V0)
	if err != nil {
		return nil, err
	}
	rep := &report{o: o, corpus: c, st: newStats(), before: map[string]map[string]int{}, after: map[string]map[string]int{}, stores: map[string][]string{}}
	rep.v0Head = gitOut(o.V0, "rev-parse", "--short", "HEAD")

	// stores
	storeOf := func(project string) string {
		if o.Layout == "single" {
			return "org"
		}
		if s, ok := splitStore[project]; ok {
			return s
		}
		return project
	}
	for _, k := range c.Keys {
		s := storeOf(k)
		rep.stores[s] = append(rep.stores[s], k)
	}
	if rep.stores["org"] == nil {
		rep.stores["org"] = nil
	}

	// ids
	ex := existing{legacy: scanExisting(o.Out)}
	ids := assignIDs(c, ex)
	for _, t := range c.Tickets {
		if _, ok := ex.legacy[ids[t]]; ok {
			rep.reusedIDs++
		} else {
			rep.newIDs++
		}
		if ids[t] == "" {
			return nil, fmt.Errorf("could not assign an id to %s", t.Rel)
		}
	}
	refs := newRefMap(c, ids)

	// plan
	p := plan{}
	for store, projects := range rep.stores {
		writeStoreFiles(p, o, store, projects, c)
	}
	for _, t := range c.Tickets {
		count(rep.before, t.Project, "status:"+t.str("status"))
		count(rep.before, t.Project, "kind:"+t.str("kind"))
		tr := transform(t, ids[t], refs, rep.st)
		store := storeOf(t.Project)
		dir := "projects/" + t.Project + "/tickets/" + tr.Ticket.ID
		p.put(store, dir+"/ticket.md", kaspec.FormatTicket(tr.Ticket))
		for _, pc := range tr.Comments {
			p.put(store, dir+"/comments/"+pc.Name, kaspec.FormatComment(pc.Comment))
		}
		count(rep.after, t.Project, "status:"+tr.Ticket.Status)
		count(rep.after, t.Project, "kind:"+tr.Ticket.Kind)
	}
	for _, k := range c.Keys {
		copyDocs(p, storeOf(k), o.V0, k)
	}

	// write
	if err := apply(p, o, rep); err != nil {
		return nil, err
	}
	if o.DryRun {
		return rep, nil
	}

	// verify
	verify(o, rep)

	// commit
	if o.Commit {
		names := sortedKeys(rep.stores)
		for _, s := range names {
			dir := filepath.Join(o.Out, s)
			if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
				continue
			}
			gitOut(dir, "add", "-A")
			if exec.Command("git", "-C", dir, "diff", "--cached", "--quiet").Run() == nil {
				continue
			}
			msg := "Import from KanArtist v0"
			if rep.v0Head != "" {
				msg += " at " + rep.v0Head
			}
			msg += "\n\nGenerated by tools/migrate-v0. Re-running it updates tickets that changed in v0.\n"
			cmd := exec.Command("git", "-C", dir, "commit", "-q", "-m", msg)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("commit in %s: %v: %s", dir, err, out)
			}
			rep.commits = append(rep.commits, s)
		}
	}
	return rep, nil
}

func count(m map[string]map[string]int, project, key string) {
	if m[project] == nil {
		m[project] = map[string]int{}
	}
	m[project][key]++
}

func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// scanExisting maps v1 ticket IDs already in the output to their legacy_id.
func scanExisting(out string) map[string]string {
	m := map[string]string{}
	stores, _ := os.ReadDir(out)
	for _, s := range stores {
		if !s.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(out, s.Name(), "projects", "*", "tickets", "*", "ticket.md"))
		for _, f := range matches {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			t, _ := kaspec.ParseTicket(f, data)
			if t == nil || t.ID == "" {
				continue
			}
			m[t.ID] = t.Fields["legacy_id"].Scalar
		}
	}
	return m
}

// ---- generated store files -------------------------------------------------------

const gitattributes = `# KanArtist store: canonical LF line endings everywhere (spec §3.4).
* text=auto eol=lf
# Field-wise merges for tickets once ka registers its merge driver; plain text merge until then.
ticket.md merge=ka
`

const orgConfig = `# Org-wide data config. Project stores inherit it through extends (spec §6).
# The workflow, kinds and claim policy are the built-in defaults, so they are not restated here.
fields:
  legacy_id:
    type: string
    description: The ticket's id in KanArtist v0 (for example JK-4661)
  anti_goal:
    type: string
    description: Imported from v0, an explicit non-goal
  branch:
    type: string
    description: Imported from v0, the git branch that carried the work
  deliverable:
    type: string
    description: Imported from v0, the artifact the ticket produced
  estimate:
    type: string
    description: Imported from v0, a size guess recorded at import
  order:
    type: string
    description: Imported from v0, hand-set ordering within a batch
  shipped:
    type: string
    description: Imported from v0, the release that carried it
  source:
    type: string
    description: Imported from v0, where the ticket came from
`

const orgActors = `# Actor registry (spec §5.4). Aliases map historical spellings to one handle.
humans:
  bsant:
    name: Bryan Sant
    emails:
      - BryanSant@users.noreply.github.com
    aliases:
      - bryan.sant
      - Bryan Sant
agents:
  agent/claude:
    name: Claude Code
  agent/migrate-v0:
    name: KanArtist v0 importer
roles:
  maintainers:
    - bsant
`

const orgBoard = `name: In flight
description: Everything being worked on right now, across every project
filter: "category:started"
group_by: status
swimlanes: project
card_fields:
  - assignee
  - priority
  - labels
`

func writeStoreFiles(p plan, o options, store string, projects []string, c *v0Corpus) {
	sf := []kaspec.KV{{K: "spec", V: kaspec.Raw("1")}, {K: "name", V: store}}
	if store == "org" {
		desc := "Org-wide KanArtist config, actors and boards"
		if len(projects) > 0 {
			desc = "KanArtist store for " + strings.Join(projects, ", ") + ", plus org-wide config, actors and boards"
		}
		sf = append(sf, kaspec.KV{K: "description", V: desc})
		p.put(store, ".ka/config.yml", []byte(orgConfig))
		p.put(store, ".ka/actors.yml", []byte(orgActors))
		p.put(store, "boards/in-flight.yml", []byte(orgBoard))
	} else {
		sf = append(sf, kaspec.KV{K: "description", V: "KanArtist store for " + strings.Join(projects, ", ")})
		sf = append(sf, kaspec.KV{K: "extends", V: []any{[]kaspec.KV{{K: "url", V: o.OrgURL}}}})
	}
	p.put(store, ".ka/store.yml", []byte(kaspec.EmitYAML(sf)))
	p.put(store, ".gitattributes", []byte(gitattributes))
	readme := "# " + store + "\n\nA [KanArtist](https://github.com/KanArtist/ka) store (Store Spec v1).\n"
	if len(projects) > 0 {
		readme += "\nProjects: " + strings.Join(projects, ", ") + ".\n"
	}
	readme += "\nImported from KanArtist v0 by `tools/migrate-v0`; v0 ids are kept in each ticket's `fields.legacy_id`.\n"
	p.put(store, "README.md", []byte(readme))

	for _, k := range projects {
		vp := c.Projects[k]
		pf := []kaspec.KV{{K: "key", V: k}, {K: "prefix", V: vp.Prefix}, {K: "name", V: vp.Name}, {K: "description", V: vp.Description}}
		if archivedProjects[k] {
			pf = append(pf, kaspec.KV{K: "archived", V: kaspec.Raw("true")})
		}
		if vp.Forge != "" {
			pf = append(pf, kaspec.KV{K: "repos", V: []kaspec.KV{{K: k, V: []kaspec.KV{{K: "forge", V: vp.Forge}}}}})
		}
		var out []kaspec.KV
		for _, kv := range pf {
			if s, ok := kv.V.(string); ok && s == "" {
				continue
			}
			out = append(out, kv)
		}
		p.put(store, "projects/"+k+"/project.yml", []byte(kaspec.EmitYAML(out)))
	}
}

func copyDocs(p plan, store, v0, key string) {
	base := filepath.Join(v0, "projects", key, "docs")
	filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		data, err := os.ReadFile(path)
		if err == nil {
			p.put(store, "projects/"+key+"/docs/"+filepath.ToSlash(rel), data)
		}
		return nil
	})
}

// ---- applying the plan with ownership tracking ---------------------------------------

type state struct {
	Version int               `json:"version"`
	V0Head  string            `json:"v0_head,omitempty"`
	Files   map[string]string `json:"files"` // "<store>/<rel>" -> sha256 of the content last written
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func apply(p plan, o options, rep *report) error {
	statePath := filepath.Join(o.Out, ".migrate-v0-state.json")
	st := state{Version: 1, Files: map[string]string{}}
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("%s: %w", statePath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	next := state{Version: 1, V0Head: rep.v0Head, Files: map[string]string{}}

	var keys []string
	for store, files := range p {
		for rel := range files {
			keys = append(keys, store+"/"+rel)
		}
	}
	sort.Strings(keys)
	planned := map[string]bool{}
	for _, key := range keys {
		planned[key] = true
		store, rel, _ := strings.Cut(key, "/")
		data := p[store][rel]
		want := sha(data)
		path := filepath.Join(o.Out, store, filepath.FromSlash(rel))
		cur, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			rep.created++
		case err != nil:
			return err
		case sha(cur) == want:
			rep.unchanged++
			next.Files[key] = want
			continue
		case st.Files[key] == sha(cur) || o.Force:
			rep.updated++
		default:
			rep.conflicts = append(rep.conflicts, key+": modified since the last migration run; left as is (use --force to overwrite)")
			if prev, ok := st.Files[key]; ok {
				next.Files[key] = prev // stay owned, stay in conflict until resolved
			}
			continue
		}
		next.Files[key] = want
		if o.DryRun {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	// files written by an earlier run that the source no longer produces
	var stale []string
	for key := range st.Files {
		if !planned[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		store, rel, _ := strings.Cut(key, "/")
		path := filepath.Join(o.Out, store, filepath.FromSlash(rel))
		cur, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if sha(cur) != st.Files[key] && !o.Force {
			rep.conflicts = append(rep.conflicts, key+": no longer produced by v0 but modified since the last run; left as is")
			continue
		}
		rep.deleted++
		if !o.DryRun {
			os.Remove(path)
			removeEmptyParents(filepath.Dir(path), filepath.Join(o.Out, store))
		}
	}
	if o.DryRun {
		return nil
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(next, "", "  ")
	return os.WriteFile(statePath, append(data, '\n'), 0o644)
}

func removeEmptyParents(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// ---- verification and report ---------------------------------------------------------

func verify(o options, rep *report) {
	ws := kaspec.NewWorkspace(nil)
	ws.Resolve = func(from *kaspec.Store, par kaspec.Parent) (fs.FS, string, string, error) {
		dir := par.URL
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(ws.KeyOf(from), dir)
		}
		if _, err := os.Stat(filepath.Join(dir, ".ka", "store.yml")); err != nil {
			return nil, "", "", fmt.Errorf("%s is not a store", dir)
		}
		abs, _ := filepath.Abs(dir)
		return os.DirFS(abs), abs, filepath.Base(abs), nil
	}
	for _, s := range sortedKeys(rep.stores) {
		dir, _ := filepath.Abs(filepath.Join(o.Out, s))
		ws.Add(os.DirFS(dir), dir, s)
	}
	rep.diags = kaspec.Check(ws, kaspec.CheckOptions{Now: o.Now})
	for _, d := range rep.diags {
		switch d.Severity {
		case kaspec.SevError:
			rep.checkErrors++
		case kaspec.SevWarning:
			rep.checkWarns++
		default:
			rep.checkInfos++
		}
	}
	md := rep.markdown()
	os.WriteFile(filepath.Join(o.Out, "migrate-v0-report.md"), []byte(md), 0o644)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r *report) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "migrated %d v0 tickets from %s", len(r.corpus.Tickets), r.o.V0)
	if r.v0Head != "" {
		fmt.Fprintf(&b, " @ %s", r.v0Head)
	}
	fmt.Fprintf(&b, " into %d store(s) under %s (layout %s)\n", len(r.stores), r.o.Out, r.o.Layout)
	fmt.Fprintf(&b, "  ids: %d reused from earlier runs, %d new\n", r.reusedIDs, r.newIDs)
	verb := ""
	if r.o.DryRun {
		verb = " (dry run: nothing written)"
	}
	fmt.Fprintf(&b, "  files: %d created, %d updated, %d deleted, %d unchanged%s\n", r.created, r.updated, r.deleted, r.unchanged, verb)
	if len(r.corpus.Problems) > 0 {
		fmt.Fprintf(&b, "  unreadable v0 tickets: %d (see report)\n", len(r.corpus.Problems))
	}
	if len(r.conflicts) > 0 {
		fmt.Fprintf(&b, "  CONFLICTS: %d\n", len(r.conflicts))
		for _, c := range r.conflicts {
			fmt.Fprintf(&b, "    %s\n", c)
		}
	}
	if !r.o.DryRun {
		fmt.Fprintf(&b, "  ka check: %d error(s), %d warning(s), %d info\n", r.checkErrors, r.checkWarns, r.checkInfos)
		for _, d := range r.diags {
			if d.Severity == kaspec.SevError {
				fmt.Fprintf(&b, "    %s\n", d)
			}
		}
		fmt.Fprintf(&b, "  report: %s\n", filepath.Join(r.o.Out, "migrate-v0-report.md"))
	}
	for _, s := range r.commits {
		fmt.Fprintf(&b, "  committed in %s\n", s)
	}
	return b.String()
}

func (r *report) markdown() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# KanArtist v0 → v1 migration report\n\n")
	w("- Source: `%s`", r.o.V0)
	if r.v0Head != "" {
		w(" at `%s`", r.v0Head)
	}
	w("\n- Run: %s\n- Layout: %s\n- Tickets: %d (ids reused: %d, new: %d)\n", r.o.Now.Format(time.RFC3339), r.o.Layout, len(r.corpus.Tickets), r.reusedIDs, r.newIDs)
	w("- Files: %d created, %d updated, %d deleted, %d unchanged\n", r.created, r.updated, r.deleted, r.unchanged)
	w("- `ka check`: %d errors, %d warnings, %d info\n\n", r.checkErrors, r.checkWarns, r.checkInfos)

	w("## Stores\n\n| Store | Projects |\n|---|---|\n")
	for _, s := range sortedKeys(r.stores) {
		w("| %s | %s |\n", s, strings.Join(r.stores[s], ", "))
	}
	w("\n## Counts before and after\n\n| Project | Key | v0 | v1 |\n|---|---|---|---|\n")
	for _, p := range sortedKeys(r.before) {
		keys := map[string]bool{}
		for k := range r.before[p] {
			keys[k] = true
		}
		for k := range r.after[p] {
			keys[k] = true
		}
		for _, k := range sortedKeys(keys) {
			w("| %s | %s | %d | %d |\n", p, k, r.before[p][k], r.after[p][k])
		}
	}
	st := r.st
	w("\n## Normalizations\n\n")
	w("- CRLF files converted: %d\n- Claims created for in_progress tickets: %d\n", st.crlfFixed, st.claims)
	w("- Acceptance checklists moved to frontmatter: %d tickets, %d items\n", st.acceptanceTicket, st.acceptanceItems)
	w("- Empty template sections removed: %d\n", st.placeholderDrops)
	section := func(title string, m map[string]int) {
		if len(m) == 0 {
			return
		}
		w("\n### %s\n\n| Value | Tickets |\n|---|---|\n", title)
		for _, k := range sortedKeys(m) {
			w("| %s | %d |\n", k, m[k])
		}
	}
	section("Assignees", st.assigneeMapped)
	section("Kinds", st.kindsMapped)
	section("History sections extracted to comments (by heading)", st.extractedByHead)
	section("Comments created (by type)", st.commentsByType)
	section("Unrecognized v0 frontmatter keys (dropped)", st.unknownKeys)
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		w("\n### %s (%d)\n\n", title, len(items))
		for _, it := range items {
			w("- %s\n", it)
		}
	}
	list("Titles over 300 characters shortened (full title kept in the body)", st.titlesShortened)
	list("depends_on entries dropped (target missing or ambiguous)", st.droppedDeps)
	list("Links dropped (not a symbolic link or URL)", st.droppedLinks)
	list("Labels dropped", st.droppedLabels)
	list("Priorities dropped", st.priorityDropped)
	list("Statuses fixed", st.statusFixed)
	list("Unreadable v0 tickets", r.corpus.Problems)
	list("Conflicts", r.conflicts)
	if len(r.diags) > 0 {
		w("\n## ka check findings\n\n")
		byCode := map[string]int{}
		for _, d := range r.diags {
			byCode[string(d.Severity)+" "+d.Code]++
		}
		w("| Severity and code | Count |\n|---|---|\n")
		for _, k := range sortedKeys(byCode) {
			w("| %s | %d |\n", k, byCode[k])
		}
		w("\n```\n")
		for i, d := range r.diags {
			if i == 500 {
				w("... %d more\n", len(r.diags)-500)
				break
			}
			w("%s\n", d)
		}
		w("```\n")
	}
	return b.String()
}
