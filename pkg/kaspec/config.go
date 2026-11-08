package kaspec

import (
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed defaults.yml
var defaultsYAML []byte

// DefaultsYAML returns the built-in data-config defaults (spec §6.2).
func DefaultsYAML() []byte { return slices.Clone(defaultsYAML) }

// Categories is the fixed set of workflow categories.
var Categories = []string{"triage", "backlog", "unstarted", "started", "completed", "canceled"}

// FieldTypes is the set of custom field types.
var FieldTypes = []string{"string", "text", "number", "integer", "boolean", "enum", "date", "timestamp", "actor", "url", "id", "list"}

// ConfigKeys is the set of top-level keys allowed in .ka/config.yml and a project's config block.
var ConfigKeys = []string{"id_suffix_length", "workflow", "kinds", "labels", "fields", "claims", "comments", "attachments", "template", "locked", "runtime"}

// Status is one workflow status.
type Status struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Blocked  bool   `json:"blocked,omitempty"`
}

// FieldDef declares a custom field.
type FieldDef struct {
	Type        string   `json:"type"`
	Values      []string `json:"values,omitempty"` // enum values
	Description string   `json:"description,omitempty"`
}

// Config is the effective data config of a project.
type Config struct {
	IDSuffixLength      int
	Statuses            []Status
	Initial             string
	AgentCreatedInitial string
	Kinds               []string
	LabelsClosed        bool
	LabelsKnown         []string
	FieldOrder          []string
	Fields              map[string]FieldDef
	ClaimsStaleAfter    time.Duration
	CommentsMaxBytes    int64
	ProgressMinInterval time.Duration
	AttachmentsMaxBytes int64
	TemplateSections    []string
	Locked              []string
	Runtime             *Node // class 3; not validated here

	// Sources maps a dotted key path to the layer that last set it.
	Sources map[string]string
	// LockedBy maps a locked path to the layer that locked it.
	LockedBy map[string]string
}

// StatusByName returns the status with the given name.
func (c *Config) StatusByName(name string) (Status, bool) {
	for _, s := range c.Statuses {
		if s.Name == name {
			return s, true
		}
	}
	return Status{}, false
}

// StatusNames lists the workflow's status names.
func (c *Config) StatusNames() []string {
	out := make([]string, len(c.Statuses))
	for i, s := range c.Statuses {
		out[i] = s.Name
	}
	return out
}

// ConfigLayer is one layer of a config chain.
type ConfigLayer struct {
	Name string // shown in diagnostics and `ka config explain`, e.g. "org/.ka/config.yml"
	Path string // diagnostic path
	Node *Node  // a mapping; nil for an absent file
}

// ResolveConfig merges layers bottom-up (built-in defaults first, then each
// layer in order; later layers win) and decodes the result. Locks set by a
// layer stop every later layer from changing the locked path.
func ResolveConfig(layers []ConfigLayer) (*Config, []Diagnostic) {
	var diags []Diagnostic
	def, err := ParseYAML(defaultsYAML)
	if err != nil {
		panic("kaspec: built-in defaults do not parse: " + err.Error())
	}
	m := &merger{lockedBy: map[string]string{}, sources: map[string]string{}}
	merged := cloneNode(def)
	m.recordSources(merged, "", "built-in defaults")
	for _, l := range layers {
		if l.Node == nil {
			continue
		}
		if l.Node.Kind != MapNode {
			diags = append(diags, diag("config_invalid", l.Path, l.Node.Line, "", "config must be a YAML mapping"))
			continue
		}
		for _, k := range l.Node.Keys {
			if !slices.Contains(ConfigKeys, k) {
				fix := ""
				if s := nearest(k, ConfigKeys); s != "" {
					fix = "did you mean " + s + "?"
				}
				diags = append(diags, diag("unknown_key", l.Path, l.Node.KeyLines[k], fix, "unknown config key %q", k))
			}
		}
		m.layer, m.path = l.Name, l.Path
		merged = m.merge(merged, l.Node, "")
		// locks declared by this layer apply to later layers
		if ln := l.Node.Map["locked"]; ln != nil && ln.Kind == SeqNode {
			for _, it := range ln.Items {
				if it.Kind == ScalarNode {
					if _, ok := m.lockedBy[it.Value]; !ok {
						m.lockedBy[it.Value] = l.Name
					}
				}
			}
		}
	}
	diags = append(diags, m.diags...)
	cfg, dd := decodeConfig(merged, layers)
	diags = append(diags, dd...)
	cfg.Sources = m.sources
	cfg.LockedBy = m.lockedBy
	locked := make([]string, 0, len(m.lockedBy))
	for p := range m.lockedBy {
		locked = append(locked, p)
	}
	slices.Sort(locked)
	cfg.Locked = locked
	return cfg, diags
}

type merger struct {
	layer, path string
	lockedBy    map[string]string
	sources     map[string]string
	diags       []Diagnostic
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// lockFor returns the layer locking p, if p is locked or inside a locked path.
func (m *merger) lockFor(p string) (string, string) {
	for l, by := range m.lockedBy {
		if p == l || strings.HasPrefix(p, l+".") {
			return l, by
		}
	}
	return "", ""
}

// locksBelow reports whether some locked path lies strictly inside p.
func (m *merger) locksBelow(p string) (string, string) {
	for l, by := range m.lockedBy {
		if strings.HasPrefix(l, p+".") {
			return l, by
		}
	}
	return "", ""
}

func (m *merger) lockedErr(p string, line int, l, by string) {
	m.diags = append(m.diags, diag("config_locked", m.path, line,
		"remove the override, or ask the owner of "+by+" to unlock it",
		"%s is locked by %s (lock: %s); this override is ignored", p, by, l))
}

func (m *merger) recordSources(n *Node, prefix, layer string) {
	if prefix != "" {
		m.sources[prefix] = layer
	}
	if n != nil && n.Kind == MapNode {
		for _, k := range n.Keys {
			m.recordSources(n.Map[k], join(prefix, k), layer)
		}
	}
}

func (m *merger) forgetSources(prefix string) {
	for k := range m.sources {
		if k == prefix || strings.HasPrefix(k, prefix+".") {
			delete(m.sources, k)
		}
	}
}

// merge applies layer onto base. Rules (spec §6.3): maps merge key by key;
// null deletes; other values replace; {$append: [...]} / {$remove: [...]}
// patch a list of strings.
func (m *merger) merge(base, layer *Node, prefix string) *Node {
	out := cloneNode(base)
	if out == nil || out.Kind != MapNode {
		out = &Node{Kind: MapNode, Map: map[string]*Node{}, KeyLines: map[string]int{}}
	}
	for _, k := range layer.Keys {
		if prefix == "" && k == "locked" {
			continue // locks accumulate separately; they are never overridden
		}
		p := join(prefix, k)
		lv := layer.Map[k]
		line := layer.KeyLines[k]
		if l, by := m.lockFor(p); l != "" {
			m.lockedErr(p, line, l, by)
			continue
		}
		bv := out.Map[k]
		switch {
		case lv.Kind == NullNode:
			if l, by := m.locksBelow(p); l != "" {
				m.lockedErr(p, line, l, by)
				continue
			}
			deleteKey(out, k)
			m.forgetSources(p)
			m.sources[p] = m.layer + " (deleted)"
		case isPatch(lv):
			out.Map[k] = m.applyPatch(bv, lv, p, line)
			if _, ok := out.KeyLines[k]; !ok {
				out.Keys = append(out.Keys, k)
			}
			out.KeyLines[k] = line
			m.sources[p] = m.layer
		case lv.Kind == MapNode && bv != nil && bv.Kind == MapNode:
			out.Map[k] = m.merge(bv, lv, p)
			m.sources[p] = m.layer
		default:
			if l, by := m.locksBelow(p); l != "" {
				m.lockedErr(p, line, l, by)
				continue
			}
			if bv == nil {
				out.Keys = append(out.Keys, k)
			}
			out.Map[k] = cloneNode(lv)
			out.KeyLines[k] = line
			m.forgetSources(p)
			m.recordSources(lv, p, m.layer)
		}
	}
	return out
}

func isPatch(n *Node) bool {
	if n == nil || n.Kind != MapNode || len(n.Keys) == 0 {
		return false
	}
	for _, k := range n.Keys {
		if k != "$append" && k != "$remove" {
			return false
		}
	}
	return true
}

func (m *merger) applyPatch(base, patch *Node, p string, line int) *Node {
	out := &Node{Kind: SeqNode, Line: line}
	if base != nil {
		if base.Kind != SeqNode {
			m.diags = append(m.diags, diag("config_invalid", m.path, line, "", "%s: $append/$remove only apply to lists", p))
			return cloneNode(base)
		}
		out.Items = append(out.Items, cloneNode(base).Items...)
	}
	if rm := patch.Map["$remove"]; rm != nil {
		if rm.Kind != SeqNode {
			m.diags = append(m.diags, diag("config_invalid", m.path, line, "", "%s.$remove must be a list", p))
		} else {
			drop := map[string]bool{}
			for _, it := range rm.Items {
				drop[it.Value] = true
			}
			kept := out.Items[:0]
			for _, it := range out.Items {
				if !(it.Kind == ScalarNode && drop[it.Value]) {
					kept = append(kept, it)
				}
			}
			out.Items = kept
		}
	}
	if ap := patch.Map["$append"]; ap != nil {
		if ap.Kind != SeqNode {
			m.diags = append(m.diags, diag("config_invalid", m.path, line, "", "%s.$append must be a list", p))
		} else {
			have := map[string]bool{}
			for _, it := range out.Items {
				have[it.Value] = true
			}
			for _, it := range ap.Items {
				if !have[it.Value] {
					out.Items = append(out.Items, cloneNode(it))
					have[it.Value] = true
				}
			}
		}
	}
	return out
}

func deleteKey(n *Node, k string) {
	delete(n.Map, k)
	delete(n.KeyLines, k)
	n.Keys = slices.DeleteFunc(n.Keys, func(s string) bool { return s == k })
}

func cloneNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Map != nil {
		c.Map = make(map[string]*Node, len(n.Map))
		for k, v := range n.Map {
			c.Map[k] = cloneNode(v)
		}
		c.KeyLines = make(map[string]int, len(n.KeyLines))
		for k, v := range n.KeyLines {
			c.KeyLines[k] = v
		}
		c.Keys = slices.Clone(n.Keys)
	}
	if n.Items != nil {
		c.Items = make([]*Node, len(n.Items))
		for i, v := range n.Items {
			c.Items[i] = cloneNode(v)
		}
	}
	return &c
}

var durationRe = regexp.MustCompile(`^([0-9]+)d$`)

// ParseDuration accepts Go durations plus a whole-day form such as "7d".
func ParseDuration(s string) (time.Duration, error) {
	if m := durationRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// decodeConfig validates and decodes a merged config tree. Diagnostics are
// attributed to the layer that last set the offending key.
func decodeConfig(n *Node, layers []ConfigLayer) (*Config, []Diagnostic) {
	cfg := &Config{Fields: map[string]FieldDef{}}
	var diags []Diagnostic
	where := func(key string) (string, int) {
		// attribute to the last layer mentioning the top-level key
		for i := len(layers) - 1; i >= 0; i-- {
			if l := layers[i].Node; l != nil && l.Kind == MapNode && l.Map[key] != nil {
				return layers[i].Path, l.KeyLines[key]
			}
		}
		return "(built-in defaults)", 0
	}
	bad := func(key, format string, args ...any) {
		p, line := where(strings.SplitN(key, ".", 2)[0])
		diags = append(diags, diag("config_invalid", p, line, "", "%s: "+format, append([]any{key}, args...)...))
	}
	get := func(parent *Node, k string) *Node {
		if parent == nil || parent.Kind != MapNode {
			return nil
		}
		v := parent.Map[k]
		if v == nil || v.Kind == NullNode {
			return nil
		}
		return v
	}
	intOf := func(key string, v *Node, lo, hi int64) int64 {
		if v == nil {
			return 0
		}
		if v.Kind != ScalarNode || v.Quoted || !intRe.MatchString(v.Value) {
			bad(key, "must be an integer")
			return 0
		}
		x, _ := strconv.ParseInt(v.Value, 10, 64)
		if x < lo || x > hi {
			bad(key, "must be between %d and %d", lo, hi)
		}
		return x
	}
	durOf := func(key string, v *Node) time.Duration {
		if v == nil {
			return 0
		}
		d, err := ParseDuration(v.Value)
		if v.Kind != ScalarNode || err != nil || d < 0 {
			bad(key, "must be a duration such as 30s, 15m, 24h or 7d")
			return 0
		}
		return d
	}
	strList := func(key string, v *Node) []string {
		if v == nil {
			return nil
		}
		if v.Kind != SeqNode {
			bad(key, "must be a list")
			return nil
		}
		var out []string
		for _, it := range v.Items {
			if it.Kind != ScalarNode {
				bad(key, "items must be strings")
				continue
			}
			out = append(out, it.Value)
		}
		return out
	}

	cfg.IDSuffixLength = int(intOf("id_suffix_length", get(n, "id_suffix_length"), 5, 8))

	wf := get(n, "workflow")
	if st := get(wf, "statuses"); st == nil || st.Kind != SeqNode || len(st.Items) == 0 {
		bad("workflow.statuses", "must be a non-empty list")
	} else {
		seen := map[string]bool{}
		for _, it := range st.Items {
			if it.Kind != MapNode {
				bad("workflow.statuses", "items must be mappings with name and category")
				continue
			}
			s := Status{}
			if v := get(it, "name"); v != nil {
				s.Name = v.Value
			}
			if v := get(it, "category"); v != nil {
				s.Category = v.Value
			}
			if v := get(it, "blocked"); v != nil {
				s.Blocked = v.Value == "true" && !v.Quoted
			}
			for _, k := range it.Keys {
				if !slices.Contains([]string{"name", "category", "blocked", "description"}, k) {
					bad("workflow.statuses", "unknown key %q", k)
				}
			}
			if !nameRe.MatchString(s.Name) {
				bad("workflow.statuses", "status name %q must be lower_snake_case", s.Name)
				continue
			}
			if !slices.Contains(Categories, s.Category) {
				bad("workflow.statuses", "status %q has category %q; use one of %s", s.Name, s.Category, strings.Join(Categories, ", "))
			}
			if seen[s.Name] {
				bad("workflow.statuses", "duplicate status %q", s.Name)
			}
			seen[s.Name] = true
			cfg.Statuses = append(cfg.Statuses, s)
		}
	}
	if v := get(wf, "initial"); v != nil {
		cfg.Initial = v.Value
	}
	if v := get(wf, "agent_created_initial"); v != nil {
		cfg.AgentCreatedInitial = v.Value
	}
	for _, k := range []struct{ key, val string }{{"workflow.initial", cfg.Initial}, {"workflow.agent_created_initial", cfg.AgentCreatedInitial}} {
		if k.val != "" {
			if _, ok := cfg.StatusByName(k.val); !ok {
				bad(k.key, "%q is not a status in the workflow", k.val)
			}
		}
	}
	if wf != nil {
		for _, k := range wf.Keys {
			if !slices.Contains([]string{"statuses", "initial", "agent_created_initial"}, k) {
				bad("workflow", "unknown key %q", k)
			}
		}
	}

	cfg.Kinds = strList("kinds", get(n, "kinds"))
	for _, k := range cfg.Kinds {
		if !nameRe.MatchString(k) {
			bad("kinds", "kind %q must be lower-case (letters, digits, - and _)", k)
		}
	}
	lb := get(n, "labels")
	if v := get(lb, "closed"); v != nil {
		cfg.LabelsClosed = v.Value == "true" && !v.Quoted
	}
	cfg.LabelsKnown = strList("labels.known", get(lb, "known"))

	if fs := get(n, "fields"); fs != nil {
		if fs.Kind != MapNode {
			bad("fields", "must be a mapping of field names to definitions")
		} else {
			for _, name := range fs.Keys {
				def := fs.Map[name]
				if def == nil || def.Kind == NullNode {
					continue
				}
				if !fieldNameRe.MatchString(name) {
					bad("fields", "field name %q must be lower_snake_case", name)
					continue
				}
				if def.Kind != MapNode {
					bad("fields."+name, "must be a mapping with a type")
					continue
				}
				fd := FieldDef{}
				if v := get(def, "type"); v != nil {
					fd.Type = v.Value
				}
				if !slices.Contains(FieldTypes, fd.Type) {
					bad("fields."+name, "type %q is not one of %s", fd.Type, strings.Join(FieldTypes, ", "))
				}
				fd.Values = strList("fields."+name+".values", get(def, "values"))
				if fd.Type == "enum" && len(fd.Values) == 0 {
					bad("fields."+name, "enum fields need values")
				}
				if v := get(def, "description"); v != nil {
					fd.Description = v.Value
				}
				cfg.Fields[name] = fd
				cfg.FieldOrder = append(cfg.FieldOrder, name)
			}
		}
	}
	cfg.ClaimsStaleAfter = durOf("claims.stale_after", get(get(n, "claims"), "stale_after"))
	cfg.CommentsMaxBytes = intOf("comments.max_bytes", get(get(n, "comments"), "max_bytes"), 0, 1<<30)
	cfg.ProgressMinInterval = durOf("comments.progress_min_interval", get(get(n, "comments"), "progress_min_interval"))
	cfg.AttachmentsMaxBytes = intOf("attachments.max_bytes", get(get(n, "attachments"), "max_bytes"), 0, 1<<34)
	cfg.TemplateSections = strList("template.sections", get(get(n, "template"), "sections"))
	cfg.Runtime = get(n, "runtime")
	return cfg, diags
}

// ValidateFieldValue checks a custom field value against its declaration.
func ValidateFieldValue(def FieldDef, v FieldValue) error {
	if def.Type == "list" {
		if !v.IsList {
			return fmt.Errorf("must be a list of strings")
		}
		return nil
	}
	if v.IsList {
		return fmt.Errorf("must be a single value, not a list")
	}
	s := v.Scalar
	switch def.Type {
	case "string":
		if strings.ContainsAny(s, "\n\r") {
			return fmt.Errorf("must be a single line (use type text for multi-line values)")
		}
	case "text":
	case "number":
		if _, err := strconv.ParseFloat(s, 64); err != nil || !v.Typed {
			return fmt.Errorf("must be an unquoted number")
		}
	case "integer":
		if !intRe.MatchString(s) || !v.Typed {
			return fmt.Errorf("must be an unquoted integer")
		}
	case "boolean":
		if (s != "true" && s != "false") || !v.Typed {
			return fmt.Errorf("must be true or false")
		}
	case "enum":
		if !slices.Contains(def.Values, s) {
			return fmt.Errorf("%q is not one of %s", s, strings.Join(def.Values, ", "))
		}
	case "date":
		if !DateRe.MatchString(s) {
			return fmt.Errorf("must be a date (YYYY-MM-DD)")
		}
	case "timestamp":
		if !TimestampRe.MatchString(s) {
			return fmt.Errorf("must be an RFC 3339 UTC timestamp")
		}
	case "actor":
		if !ActorRe.MatchString(s) {
			return fmt.Errorf("must be an actor handle")
		}
	case "url":
		if !urlRe.MatchString(s) {
			return fmt.Errorf("must be an http(s) URL")
		}
	case "id":
		if !IDRe.MatchString(s) {
			return fmt.Errorf("must be a ticket ID")
		}
	}
	return nil
}
