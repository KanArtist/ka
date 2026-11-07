package kaspec

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ParentResolver locates a parent store named in `extends`. It returns the
// parent's filesystem, a stable identity key (two references to the same
// store must return the same key) and a display label.
type ParentResolver func(from *Store, p Parent) (fsys fs.FS, key, label string, err error)

// Workspace is a set of stores checked together.
type Workspace struct {
	Stores  []*Store
	Resolve ParentResolver

	keyOf  map[*Store]string
	loaded map[string]*Store
}

// NewWorkspace returns an empty workspace.
func NewWorkspace(resolve ParentResolver) *Workspace {
	return &Workspace{Resolve: resolve, keyOf: map[*Store]string{}, loaded: map[string]*Store{}}
}

// Add loads a store into the workspace. key identifies the store's location
// (e.g. its absolute path); label prefixes its diagnostic paths.
func (w *Workspace) Add(fsys fs.FS, key, label string) *Store {
	if s, ok := w.loaded[key]; ok {
		if !slices.Contains(w.Stores, s) {
			w.Stores = append(w.Stores, s)
		}
		return s
	}
	s := LoadStore(fsys, label)
	w.loaded[key] = s
	w.keyOf[s] = key
	w.Stores = append(w.Stores, s)
	return s
}

// KeyOf returns the identity key of any store the workspace has loaded,
// including parents pulled in through extends.
func (w *Workspace) KeyOf(s *Store) string { return w.keyOf[s] }

// CheckOptions tunes Check.
type CheckOptions struct {
	Now time.Time // for stale-claim detection; zero disables it
}

type projRef struct {
	store *Store
	proj  *Project
}

type checker struct {
	w        *Workspace
	opt      CheckOptions
	diags    []Diagnostic
	byKey    map[string][]projRef
	byPrefix map[string]projRef
	repos    map[string]bool
	chains   map[*Store][]*Store
}

// Check validates every store in the workspace, including cross-store rules.
func Check(w *Workspace, opt CheckOptions) []Diagnostic {
	c := &checker{w: w, opt: opt, byKey: map[string][]projRef{}, byPrefix: map[string]projRef{}, repos: map[string]bool{}, chains: map[*Store][]*Store{}}
	for _, s := range w.Stores {
		c.diags = append(c.diags, s.Diags...)
	}
	c.index()
	for _, s := range w.Stores {
		for _, k := range s.Keys {
			c.checkProject(s, s.Projects[k])
		}
	}
	c.cycles()
	out := dedupe(c.diags)
	SortDiagnostics(out)
	return out
}

func dedupe(ds []Diagnostic) []Diagnostic {
	seen := map[Diagnostic]bool{}
	out := ds[:0:0]
	for _, d := range ds {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func (c *checker) index() {
	for _, s := range c.w.Stores {
		for _, k := range s.Keys {
			pr := s.Projects[k]
			c.byKey[k] = append(c.byKey[k], projRef{s, pr})
			for name := range pr.File.Repos {
				c.repos[name] = true
			}
		}
	}
	for k, refs := range c.byKey {
		if len(refs) > 1 {
			for _, r := range refs[1:] {
				c.diags = append(c.diags, diag("duplicate_project", r.store.p(r.proj.Dir+"/project.yml"), 1,
					"a project has exactly one home store; remove or rename one copy",
					"project %q is also defined in %s", k, labelOr(refs[0].store)))
			}
		}
	}
	for _, s := range c.w.Stores {
		for _, k := range s.Keys {
			pr := s.Projects[k]
			pfx := pr.File.Prefix
			if prev, ok := c.byPrefix[pfx]; ok && prev.proj != pr {
				if prev.proj.Key != pr.Key {
					c.diags = append(c.diags, diag("duplicate_prefix", s.p(pr.Dir+"/project.yml"), 1,
						"give each project a unique prefix", "prefix %q is also used by project %q", pfx, prev.proj.Key))
				}
				continue
			}
			c.byPrefix[pfx] = projRef{s, pr}
		}
	}
}

func labelOr(s *Store) string {
	if s.Label == "" {
		return "this store"
	}
	return s.Label
}

// chain returns the store's config chain: parents depth-first, left to right,
// each store once at its first position, the store itself last.
func (c *checker) chain(s *Store) []*Store {
	if ch, ok := c.chains[s]; ok {
		return ch
	}
	var out []*Store
	seen := map[*Store]bool{}
	var visit func(st *Store, stack []*Store, depth int)
	visit = func(st *Store, stack []*Store, depth int) {
		if st.File != nil {
			for _, par := range st.File.Extends {
				ps, err := c.parent(st, par)
				if err != nil {
					c.diags = append(c.diags, diag("config_parent_unavailable", st.p(".ka/store.yml"), 0,
						"run `ka sync`, check access to the parent, or map its URL with --map",
						"parent store %s is unavailable: %v", par.URL, err))
					continue
				}
				if slices.Contains(stack, ps) || ps == st {
					c.diags = append(c.diags, diag("config_cycle", st.p(".ka/store.yml"), 0, "remove the circular extends", "extends cycle through %s", par.URL))
					continue
				}
				if depth >= 4 {
					c.diags = append(c.diags, diag("config_invalid", st.p(".ka/store.yml"), 0, "", "extends chain is deeper than 4"))
					continue
				}
				visit(ps, append(stack, st), depth+1)
			}
		}
		if !seen[st] {
			seen[st] = true
			out = append(out, st)
		}
	}
	visit(s, nil, 0)
	c.chains[s] = out
	return out
}

func (c *checker) parent(from *Store, p Parent) (*Store, error) {
	if c.w.Resolve == nil {
		return nil, fmt.Errorf("no resolver configured")
	}
	fsys, key, label, err := c.w.Resolve(from, p)
	if err != nil {
		return nil, err
	}
	if s, ok := c.w.loaded[key]; ok {
		return s, nil
	}
	s := LoadStore(fsys, label)
	c.w.loaded[key] = s
	c.w.keyOf[s] = key
	// a parent's store/config file problems affect its children
	for _, d := range s.Diags {
		if strings.Contains(d.Path, ".ka/") {
			c.diags = append(c.diags, d)
		}
	}
	if s.File == nil {
		return nil, fmt.Errorf("%s is not a store", p.URL)
	}
	return s, nil
}

// EffectiveConfig computes a project's data config through its store's chain.
func (c *checker) effectiveConfig(s *Store, pr *Project) *Config {
	var layers []ConfigLayer
	for _, st := range c.chain(s) {
		layers = append(layers, ConfigLayer{Name: st.p(".ka/config.yml"), Path: st.p(".ka/config.yml"), Node: st.Config})
	}
	pp := s.p(pr.Dir + "/project.yml")
	layers = append(layers, ConfigLayer{Name: pp, Path: pp, Node: pr.File.Config})
	cfg, ds := ResolveConfig(layers)
	c.diags = append(c.diags, ds...)
	return cfg
}

// ProjectConfig exposes effective config resolution for callers (e.g. `ka config explain`).
func ProjectConfig(w *Workspace, s *Store, pr *Project) (*Config, []Diagnostic) {
	c := &checker{w: w, chains: map[*Store][]*Store{}}
	cfg := c.effectiveConfig(s, pr)
	return cfg, c.diags
}

func (c *checker) checkProject(s *Store, pr *Project) {
	cfg := c.effectiveConfig(s, pr)
	pr.Config = cfg
	for _, id := range pr.IDs {
		te := pr.Tickets[id]
		if te.Ticket == nil {
			continue
		}
		c.checkTicket(s, pr, cfg, te)
	}
}

func (c *checker) checkTicket(s *Store, pr *Project, cfg *Config, te *TicketEntry) {
	t := te.Ticket
	tp := s.p(te.Dir + "/ticket.md")
	ln := func(k string) int { return lineOf(t, k) }
	add := func(d Diagnostic) { c.diags = append(c.diags, d) }

	if pfx, suffix, ok := ParseID(t.ID); ok {
		if pfx != pr.File.Prefix {
			add(diag("id_prefix_mismatch", tp, ln("id"), "tickets in project "+pr.Key+" use prefix "+pr.File.Prefix, "id %q does not use the project prefix %q", t.ID, pr.File.Prefix))
		}
		if LowEntropy(suffix) {
			add(diag("low_entropy_id", tp, ln("id"), "generate the suffix randomly from 0-9A-HJKMNP-TV-Z", "id suffix %q does not look random", suffix))
		}
	}
	if t.Status != "" {
		if _, ok := cfg.StatusByName(t.Status); !ok {
			add(diag("unknown_status", tp, ln("status"), suggest(t.Status, cfg.StatusNames()), "%q is not a status in project %s (statuses: %s)", t.Status, pr.Key, strings.Join(cfg.StatusNames(), ", ")))
		}
	}
	if t.Kind != "" && len(cfg.Kinds) > 0 && !slices.Contains(cfg.Kinds, t.Kind) {
		add(diag("unknown_kind", tp, ln("kind"), suggest(t.Kind, cfg.Kinds), "%q is not a kind in project %s (kinds: %s)", t.Kind, pr.Key, strings.Join(cfg.Kinds, ", ")))
	}
	if cfg.LabelsClosed {
		for _, l := range t.Labels {
			if !slices.Contains(cfg.LabelsKnown, l) {
				add(diag("unknown_label", tp, ln("labels"), suggest(l, cfg.LabelsKnown), "label %q is not in project %s's closed label set", l, pr.Key))
			}
		}
	}
	for name, v := range t.Fields {
		def, ok := cfg.Fields[name]
		if !ok {
			add(diag("unknown_field", tp, v.Line, suggest(name, cfg.FieldOrder)+" (declare it under fields: in config)", "custom field %q is not declared for project %s", name, pr.Key))
			continue
		}
		if err := ValidateFieldValue(def, v); err != nil {
			add(diag("invalid_field_value", tp, v.Line, "", "fields.%s: %v", name, err))
		}
	}
	if t.Claim != nil && !c.opt.Now.IsZero() && cfg.ClaimsStaleAfter > 0 {
		if at, err := time.Parse(time.RFC3339, t.Claim.At); err == nil && c.opt.Now.Sub(at) > cfg.ClaimsStaleAfter {
			add(diag("stale_claim", tp, ln("claim"), "release it, or re-claim to refresh", "claim by %s is older than %s", t.Claim.By, cfg.ClaimsStaleAfter))
		}
	}
	if pr.File.Archived {
		if st, ok := cfg.StatusByName(t.Status); ok && st.Category == "started" {
			add(diag("archived_project", tp, ln("status"), "", "ticket is %s in an archived project", t.Status))
		}
	}
	if l, what := findPlaceholder(t.Body); what != "" {
		add(diag("placeholder_text", tp, bodyLine(te.Raw)+l-1, "fill it in or delete it", "body contains template placeholder text: %s", what))
	}
	refs := []struct {
		key string
		ids []string
	}{
		{"parent", nonEmpty(t.Parent)}, {"blocked_by", t.BlockedBy}, {"related", t.Related},
		{"duplicate_of", nonEmpty(t.DuplicateOf)}, {"moved_to", nonEmpty(t.MovedTo)},
	}
	for _, r := range refs {
		for _, id := range r.ids {
			if id == t.ID {
				add(diag("self_ref", tp, ln(r.key), "", "%s refers to the ticket itself", r.key))
				continue
			}
			pfx, _, _ := ParseID(id)
			target, ok := c.byPrefix[pfx]
			if !ok {
				add(diag("external_ref", tp, ln(r.key), "", "%s %s is in a project outside this workspace", r.key, id))
				continue
			}
			if _, ok := target.proj.Tickets[id]; !ok {
				add(diag("dangling_ref", tp, ln(r.key), "", "%s %s does not exist in project %s", r.key, id, target.proj.Key))
			}
		}
	}
	for _, l := range t.Links {
		if urlRe.MatchString(l) {
			continue
		}
		if m := LinkRe.FindStringSubmatch(l); m != nil && !c.repos[m[1]] {
			add(diag("unknown_repo", tp, ln("links"), "declare the repo under repos: in a project.yml", "link %q names repo %q, which no project declares", l, m[1]))
		}
	}
	if what := findSecret(te.Raw); what != "" {
		add(diag("secret_suspect", tp, 0, "remove it, rotate the credential, and rewrite history if it was pushed", "possible secret (%s)", what))
	}

	names := map[string]bool{}
	for _, ce := range te.Comments {
		names[ce.Name] = true
	}
	for _, ce := range te.Comments {
		cp := s.p(ce.Path)
		if cfg.CommentsMaxBytes > 0 && int64(len(ce.Raw)) > cfg.CommentsMaxBytes {
			add(diag("comment_too_large", cp, 0, "move bulky output into files/ and link it", "comment is %d bytes; the limit is %d", len(ce.Raw), cfg.CommentsMaxBytes))
		}
		if what := findSecret(ce.Raw); what != "" {
			add(diag("secret_suspect", cp, 0, "remove it, rotate the credential, and rewrite history if it was pushed", "possible secret (%s)", what))
		}
		cm := ce.Comment
		if cm == nil {
			continue
		}
		if m := CommentFileRe.FindStringSubmatch(ce.Name); m != nil && cm.Created != "" {
			if CommentFilename(cm.Created, m[2]) != ce.Name {
				add(diag("comment_time_mismatch", cp, cm.Lines["created"], "the filename timestamp should equal created", "created %s does not match the filename timestamp %s", cm.Created, m[1]))
			}
		}
		for _, r := range []struct{ key, val string }{{"answers", cm.Answers}, {"supersedes", cm.Supersedes}, {"retracts", cm.Retracts}} {
			if r.val != "" && !names[r.val] {
				add(diag("answer_dangling", cp, cm.Lines[r.key], "", "%s names %s, which is not a comment on this ticket", r.key, r.val))
			}
		}
	}
	for _, f := range te.Files {
		if cfg.AttachmentsMaxBytes > 0 && f.Size > cfg.AttachmentsMaxBytes {
			add(diag("file_too_large", s.p(f.Path), 0, "store large artifacts elsewhere (or in Git LFS) and link them", "attachment is %d bytes; the limit is %d", f.Size, cfg.AttachmentsMaxBytes))
		}
	}
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func suggest(s string, cands []string) string {
	if n := nearest(s, cands); n != "" {
		return "did you mean " + n + "?"
	}
	return ""
}

// cycles reports cycles in the parent and blocked_by graphs, once per cycle.
func (c *checker) cycles() {
	type node struct {
		s  *Store
		te *TicketEntry
	}
	all := map[string]node{}
	var ids []string
	for _, s := range c.w.Stores {
		for _, k := range s.Keys {
			pr := s.Projects[k]
			for _, id := range pr.IDs {
				te := pr.Tickets[id]
				if te.Ticket != nil && te.Ticket.ID == id {
					all[id] = node{s, te}
					ids = append(ids, id)
				}
			}
		}
	}
	slices.Sort(ids)
	for _, rel := range []string{"parent", "blocked_by"} {
		edges := func(id string) []string {
			t := all[id].te.Ticket
			if rel == "parent" {
				return nonEmpty(t.Parent)
			}
			return t.BlockedBy
		}
		const (
			white = iota
			grey
			black
		)
		color := map[string]int{}
		reported := map[string]bool{}
		var stack []string
		var visit func(id string)
		visit = func(id string) {
			color[id] = grey
			stack = append(stack, id)
			for _, nx := range edges(id) {
				if _, ok := all[nx]; !ok || nx == id {
					continue
				}
				switch color[nx] {
				case white:
					visit(nx)
				case grey:
					i := slices.Index(stack, nx)
					cyc := slices.Clone(stack[i:])
					start := slices.Min(cyc)
					if !reported[start] {
						reported[start] = true
						k := slices.Index(cyc, start)
						cyc = append(cyc[k:], cyc[:k]...)
						n := all[start]
						c.diags = append(c.diags, diag("cycle", n.s.p(n.te.Dir+"/ticket.md"), lineOf(n.te.Ticket, rel), "break the cycle by removing one edge",
							"%s cycle: %s -> %s", rel, strings.Join(cyc, " -> "), start))
					}
				}
			}
			stack = stack[:len(stack)-1]
			color[id] = black
		}
		for _, id := range ids {
			if color[id] == white {
				visit(id)
			}
		}
	}
}

// ---- body heuristics ---------------------------------------------------------

var (
	headingRe    = regexp.MustCompile(`^(#{1,6})\s+\S`)
	fenceRe      = regexp.MustCompile("^\\s*(```|~~~)")
	emptyBoxRe   = regexp.MustCompile(`^\s*[-*]\s+\[ \]\s*$`)
	bareDashRe   = regexp.MustCompile(`^\s*[-*]\s*$`)
	moustache    = regexp.MustCompile(`\{\{\s*[a-zA-Z_.]+\s*\}\}`)
	inlineCodeRe = regexp.MustCompile("`[^`]*`")
)

// findPlaceholder returns the 1-based body line and a description of the first
// template placeholder: an empty checkbox, a {{moustache}}, or a heading whose
// section is empty or holds only a bare "-".
func findPlaceholder(body string) (int, string) {
	lines := strings.Split(body, "\n")
	inFence := false
	type sec struct {
		line, level int
		title       string
		empty       bool
	}
	var open []sec
	closeSecs := func(level int) (int, string) {
		for len(open) > 0 && open[len(open)-1].level >= level {
			s := open[len(open)-1]
			open = open[:len(open)-1]
			if s.empty {
				return s.line, fmt.Sprintf("empty section %q", s.title)
			}
		}
		return 0, ""
	}
	for i, l := range lines {
		if fenceRe.MatchString(l) {
			inFence = !inFence
			for j := range open {
				open[j].empty = false
			}
			continue
		}
		if inFence {
			continue
		}
		if m := headingRe.FindStringSubmatch(l); m != nil {
			if ln, what := closeSecs(len(m[1])); what != "" {
				return ln, what
			}
			for j := range open {
				open[j].empty = false
			}
			open = append(open, sec{line: i + 1, level: len(m[1]), title: strings.TrimSpace(strings.TrimLeft(l, "#")), empty: true})
			continue
		}
		if emptyBoxRe.MatchString(l) {
			return i + 1, "empty checklist item `- [ ]`"
		}
		if code := inlineCodeRe.ReplaceAllString(l, ""); moustache.MatchString(code) {
			return i + 1, fmt.Sprintf("template variable %q", moustache.FindString(code))
		}
		if strings.TrimSpace(l) == "" || bareDashRe.MatchString(l) || strings.HasPrefix(strings.TrimSpace(l), "<!--") {
			continue
		}
		for j := range open {
			open[j].empty = false
		}
	}
	if ln, what := closeSecs(0); what != "" {
		return ln, what
	}
	return 0, ""
}

// bodyLine returns the 1-based line where the body starts in a raw document.
func bodyLine(raw []byte) int {
	text := string(NormalizeEncoding(raw))
	if !strings.HasPrefix(text, "---\n") {
		return 1
	}
	i := strings.Index(text[4:], "\n---\n")
	if i < 0 {
		return 1
	}
	return strings.Count(text[:4+i+5], "\n") + 1
}

var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"GitHub fine-grained token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{50,}`)},
	{"Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
}

func findSecret(b []byte) string {
	for _, p := range secretPatterns {
		if p.re.Match(b) {
			return p.name
		}
	}
	return ""
}
