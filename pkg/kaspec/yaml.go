package kaspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// NodeKind classifies a parsed YAML value.
type NodeKind int

const (
	NullNode NodeKind = iota
	ScalarNode
	MapNode
	SeqNode
)

// Node is a YAML value parsed under the KanArtist YAML profile: no anchors,
// aliases, tags, merge keys or duplicate keys, and NO implicit typing. A
// scalar is kept as its source text; the reader of a field decides whether
// it is a string, a boolean, an integer or a date. That is what keeps
// `title: no` a string and `version: 1.10` from becoming 1.1.
type Node struct {
	Kind   NodeKind
	Value  string // scalar text
	Quoted bool   // scalar was quoted or block-style (always a string)
	Line   int

	Keys     []string // mapping keys in source order
	Map      map[string]*Node
	KeyLines map[string]int

	Items []*Node
}

// YAMLError is a syntax or profile violation.
type YAMLError struct {
	Code string // yaml_syntax | yaml_feature | duplicate_key
	Line int
	Msg  string
}

func (e *YAMLError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// ParseYAML parses one YAML document under the KanArtist profile. An empty
// document yields an empty mapping.
func ParseYAML(src []byte) (*Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return &Node{Kind: MapNode, Map: map[string]*Node{}, KeyLines: map[string]int{}}, nil
		}
		return nil, &YAMLError{Code: "yaml_syntax", Line: yamlErrLine(err), Msg: cleanYAMLErr(err)}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &YAMLError{Code: "yaml_feature", Line: extra.Line, Msg: "multiple YAML documents are not allowed"}
	} else if !errors.Is(err, io.EOF) {
		return nil, &YAMLError{Code: "yaml_syntax", Line: yamlErrLine(err), Msg: cleanYAMLErr(err)}
	}
	if len(doc.Content) == 0 {
		return &Node{Kind: MapNode, Map: map[string]*Node{}, KeyLines: map[string]int{}}, nil
	}
	return convert(doc.Content[0])
}

var yamlLineRe = regexp.MustCompile(`line (\d+):? ?`)

func yamlErrLine(err error) int {
	m := yamlLineRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	n := 0
	fmt.Sscanf(m[1], "%d", &n)
	return n
}

func cleanYAMLErr(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "yaml: ")
	return strings.TrimSpace(yamlLineRe.ReplaceAllString(s, ""))
}

func convert(n *yaml.Node) (*Node, error) {
	if n.Anchor != "" {
		return nil, &YAMLError{Code: "yaml_feature", Line: n.Line, Msg: "anchors (&" + n.Anchor + ") are not allowed"}
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return nil, &YAMLError{Code: "yaml_feature", Line: n.Line, Msg: "explicit tags (" + n.Tag + ") are not allowed"}
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nil, &YAMLError{Code: "yaml_feature", Line: n.Line, Msg: "aliases (*" + n.Value + ") are not allowed"}
	case yaml.ScalarNode:
		quoted := n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0
		if !quoted && (n.Value == "" || n.Value == "~" || strings.EqualFold(n.Value, "null")) {
			return &Node{Kind: NullNode, Line: n.Line}, nil
		}
		return &Node{Kind: ScalarNode, Value: n.Value, Quoted: quoted, Line: n.Line}, nil
	case yaml.SequenceNode:
		out := &Node{Kind: SeqNode, Line: n.Line}
		for _, c := range n.Content {
			cn, err := convert(c)
			if err != nil {
				return nil, err
			}
			out.Items = append(out.Items, cn)
		}
		return out, nil
	case yaml.MappingNode:
		out := &Node{Kind: MapNode, Line: n.Line, Map: map[string]*Node{}, KeyLines: map[string]int{}}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				return nil, &YAMLError{Code: "yaml_feature", Line: k.Line, Msg: "mapping keys must be plain scalars"}
			}
			if k.Value == "<<" && k.Style == 0 {
				return nil, &YAMLError{Code: "yaml_feature", Line: k.Line, Msg: "merge keys (<<) are not allowed"}
			}
			if k.Anchor != "" || k.Style&yaml.TaggedStyle != 0 {
				return nil, &YAMLError{Code: "yaml_feature", Line: k.Line, Msg: "anchors and tags are not allowed on keys"}
			}
			if _, dup := out.Map[k.Value]; dup {
				return nil, &YAMLError{Code: "duplicate_key", Line: k.Line, Msg: fmt.Sprintf("duplicate key %q", k.Value)}
			}
			cv, err := convert(v)
			if err != nil {
				return nil, err
			}
			out.Keys = append(out.Keys, k.Value)
			out.Map[k.Value] = cv
			out.KeyLines[k.Value] = k.Line
		}
		return out, nil
	}
	return nil, &YAMLError{Code: "yaml_syntax", Line: n.Line, Msg: "unsupported YAML node"}
}

// ---- typed readers ---------------------------------------------------------

var (
	intRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
)

// fieldReader decodes a mapping's fields and collects diagnostics.
type fieldReader struct {
	path  string
	node  *Node
	diags []Diagnostic
	seen  map[string]bool
}

func newFieldReader(path string, n *Node) *fieldReader {
	return &fieldReader{path: path, node: n, seen: map[string]bool{}}
}

func (r *fieldReader) line(key string) int {
	if r.node == nil || r.node.KeyLines == nil {
		return 0
	}
	return r.node.KeyLines[key]
}

func (r *fieldReader) get(key string) *Node {
	r.seen[key] = true
	if r.node == nil || r.node.Map == nil {
		return nil
	}
	v := r.node.Map[key]
	if v == nil || v.Kind == NullNode {
		return nil
	}
	return v
}

func (r *fieldReader) bad(key, fix, format string, args ...any) {
	r.diags = append(r.diags, diag("invalid_value", r.path, r.line(key), fix, "%s: "+format, append([]any{key}, args...)...))
}

func (r *fieldReader) str(key string) string {
	v := r.get(key)
	if v == nil {
		return ""
	}
	if v.Kind != ScalarNode {
		r.bad(key, "", "must be a string")
		return ""
	}
	return v.Value
}

func (r *fieldReader) strRe(key string, re *regexp.Regexp, fix string) string {
	s := r.str(key)
	if s != "" && !re.MatchString(s) {
		r.bad(key, fix, "%q does not match %s", s, re.String())
		return ""
	}
	return s
}

func (r *fieldReader) boolean(key string) (bool, bool) {
	v := r.get(key)
	if v == nil {
		return false, false
	}
	if v.Kind != ScalarNode || v.Quoted || (v.Value != "true" && v.Value != "false") {
		r.bad(key, "use true or false", "must be a boolean")
		return false, false
	}
	return v.Value == "true", true
}

func (r *fieldReader) integer(key string) (int64, bool) {
	v := r.get(key)
	if v == nil {
		return 0, false
	}
	if v.Kind != ScalarNode || v.Quoted || !intRe.MatchString(v.Value) {
		r.bad(key, "", "must be an integer")
		return 0, false
	}
	var n int64
	fmt.Sscanf(v.Value, "%d", &n)
	return n, true
}

// strList reads a list of strings; a single scalar is not accepted.
func (r *fieldReader) strList(key string, re *regexp.Regexp, fix string) []string {
	v := r.get(key)
	if v == nil {
		return nil
	}
	if v.Kind != SeqNode {
		r.bad(key, "write it as a block list (one `- item` per line)", "must be a list")
		return nil
	}
	var out []string
	for _, it := range v.Items {
		if it.Kind != ScalarNode {
			r.bad(key, "", "list items must be strings")
			continue
		}
		if re != nil && !re.MatchString(it.Value) {
			r.bad(key, fix, "%q does not match %s", it.Value, re.String())
			continue
		}
		out = append(out, it.Value)
	}
	return out
}

// unknown reports keys present in the mapping but never read.
func (r *fieldReader) unknown(known []string) {
	if r.node == nil {
		return
	}
	for _, k := range r.node.Keys {
		if r.seen[k] {
			continue
		}
		fix := ""
		if s := nearest(k, known); s != "" {
			fix = "did you mean " + s + "?"
		}
		r.diags = append(r.diags, diag("unknown_key", r.path, r.line(k), fix, "unknown key %q", k))
	}
}

// nearest returns the closest candidate within edit distance 2, or "".
func nearest(s string, cands []string) string {
	best, bestD := "", 3
	for _, c := range cands {
		if d := levenshtein(s, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// ToAny converts a Node into plain Go values for JSON-schema validation:
// unquoted true/false become booleans, unquoted integers become int64,
// everything else is a string.
func (n *Node) ToAny() any {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case NullNode:
		return nil
	case ScalarNode:
		if !n.Quoted {
			if n.Value == "true" || n.Value == "false" {
				return n.Value == "true"
			}
			if intRe.MatchString(n.Value) {
				var i int64
				fmt.Sscanf(n.Value, "%d", &i)
				return i
			}
		}
		return n.Value
	case SeqNode:
		out := make([]any, 0, len(n.Items))
		for _, it := range n.Items {
			out = append(out, it.ToAny())
		}
		return out
	case MapNode:
		out := make(map[string]any, len(n.Keys))
		for _, k := range n.Keys {
			out[k] = n.Map[k].ToAny()
		}
		return out
	}
	return nil
}
