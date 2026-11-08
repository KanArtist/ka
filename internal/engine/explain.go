package engine

import (
	"sort"
	"strings"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// ConfigEntry is one effective config value and where it came from
// (PRD §9.3, "Seeing where a value came from").
type ConfigEntry struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Class    string `json:"class" doc:"data (validation; resolved through extends) or runtime (this ka instance; the user wins)."`
	Source   string `json:"source" doc:"The layer that set the value: a file, built-in defaults, or the workspace."`
	Line     int    `json:"line,omitempty"`
	LockedBy string `json:"locked_by,omitempty" doc:"The layer whose lock fixes this value."`
	Note     string `json:"note,omitempty"`
}

// ConfigExplainOut is a project's effective config.
type ConfigExplainOut struct {
	Project string        `json:"project"`
	Store   string        `json:"store"`
	Layers  []string      `json:"layers" doc:"The data config chain, lowest first."`
	Entries []ConfigEntry `json:"entries"`
	Locked  []string      `json:"locked,omitempty"`
}

// ConfigExplain reports each effective config value of a project and the
// layer it came from, plus locks. key limits the output to that key and
// the keys under it.
func (s *Service) ConfigExplain(project, key string) (*ConfigExplainOut, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	if project == "" {
		project = s.WS.Project
	}
	if project == "" && len(s.ix.projects) == 1 {
		for k := range s.ix.projects {
			project = k
		}
	}
	pi := s.ix.projects[project]
	if pi == nil {
		return nil, Errf(CodeUnknownProject, "pass --project; projects: "+strings.Join(sortedKeys(s.ix.projects), ", "), "no project %q", project)
	}
	cfg := pi.Cfg
	out := &ConfigExplainOut{Project: project, Store: pi.Store.Name, Locked: cfg.Locked, Entries: []ConfigEntry{}}
	out.Layers = cfg.Layers

	match := func(k string) bool {
		return key == "" || k == key || strings.HasPrefix(k, key+".") || strings.HasPrefix(key, k+".")
	}
	var walk func(n *kaspec.Node, prefix string)
	walk = func(n *kaspec.Node, prefix string) {
		if n == nil || n.Kind != kaspec.MapNode {
			return
		}
		for _, k := range n.Keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if p == "locked" || p == "runtime" || !match(p) {
				continue
			}
			v := n.Map[k]
			if v.Kind == kaspec.MapNode && len(v.Keys) > 0 && prefix != "fields" {
				walk(v, p)
				continue
			}
			out.Entries = append(out.Entries, entry(cfg, p, render(v)))
		}
	}
	walk(cfg.Effective, "")
	// Keys a layer deleted no longer appear in the tree; show them anyway.
	for p, src := range cfg.Sources {
		if strings.HasSuffix(src, " (deleted)") && match(p) {
			e := entry(cfg, p, "")
			e.Source, e.Note = strings.TrimSuffix(src, " (deleted)"), "deleted"
			out.Entries = append(out.Entries, e)
		}
	}
	sort.SliceStable(out.Entries, func(i, j int) bool { return out.Entries[i].Key < out.Entries[j].Key })

	// Runtime (class 3) for the project's store.
	st := pi.Store
	rt := []ConfigEntry{
		s.runtimeEntry(cfg, "sync.push", st.PushSetting(), st.pushSet),
		s.runtimeEntry(cfg, "sync.fetch_interval", st.fetchEvery().String(), st.fetchSet),
	}
	for _, e := range rt {
		if match(e.Key) {
			out.Entries = append(out.Entries, e)
		}
	}
	if key != "" && len(out.Entries) == 0 {
		return nil, Errf(CodeInvalid, "run ka config explain with no key to list every key", "no config key %q", key)
	}
	return out, nil
}

func (s *Service) runtimeEntry(cfg *kaspec.Config, k, val string, userSet bool) ConfigEntry {
	e := ConfigEntry{Key: k, Value: val, Class: "runtime", Source: "built-in defaults"}
	rk := "runtime." + k
	rec := ""
	if n := lookup(cfg.Effective, rk); n != nil && n.Value != "" {
		rec = n.Value
	}
	switch {
	case userSet:
		e.Source = s.WS.Source
		if rec != "" {
			e.Note = "store recommends " + rec
		}
	case rec != "" && !strings.HasPrefix(cfg.Sources[rk], "built-in"):
		e.Source, e.Line = cfg.Sources[rk], cfg.SourceLines[rk]
		e.Note = "store recommendation"
	}
	return e
}

func entry(cfg *kaspec.Config, p, val string) ConfigEntry {
	e := ConfigEntry{Key: p, Value: val, Class: "data"}
	// The most specific recorded source for p or an ancestor.
	for q := p; q != ""; {
		if src, ok := cfg.Sources[q]; ok {
			e.Source, e.Line, e.Note = src, cfg.SourceLines[q], cfg.SourceNotes[q]
			break
		}
		i := strings.LastIndexByte(q, '.')
		if i < 0 {
			break
		}
		q = q[:i]
	}
	for l, by := range cfg.LockedBy {
		if p == l || strings.HasPrefix(p, l+".") {
			e.LockedBy = by
		}
	}
	return e
}

func lookup(n *kaspec.Node, path string) *kaspec.Node {
	for _, k := range strings.Split(path, ".") {
		if n == nil || n.Kind != kaspec.MapNode {
			return nil
		}
		n = n.Map[k]
	}
	return n
}

// render prints a config value on one line.
func render(n *kaspec.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case kaspec.ScalarNode:
		return n.Value
	case kaspec.NullNode:
		return "null"
	case kaspec.SeqNode:
		parts := make([]string, 0, len(n.Items))
		for _, it := range n.Items {
			if it.Kind == kaspec.MapNode {
				// statuses: name (category)
				name := render(it.Map["name"])
				if c := it.Map["category"]; c != nil {
					name += " (" + c.Value + ")"
				}
				parts = append(parts, name)
				continue
			}
			parts = append(parts, render(it))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case kaspec.MapNode:
		if len(n.Keys) == 0 {
			return "{}"
		}
		parts := make([]string, 0, len(n.Keys))
		for _, k := range n.Keys {
			parts = append(parts, k+": "+render(n.Map[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}
