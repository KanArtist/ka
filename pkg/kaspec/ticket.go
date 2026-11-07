package kaspec

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Priorities is the fixed priority vocabulary, highest first.
var Priorities = []string{"P0", "P1", "P2", "P3"}

// TicketKeys is the canonical frontmatter key order of ticket.md.
var TicketKeys = []string{
	"id", "title", "status", "priority", "kind", "labels",
	"assignee", "owner", "claim",
	"parent", "blocked_by", "related", "duplicate_of",
	"links", "acceptance", "milestone", "due", "fields", "moved_to",
	"created", "created_by", "updated",
}

// Claim records who holds a ticket.
type Claim struct {
	By      string
	Session string
	At      string
}

// AcceptanceItem is one acceptance criterion.
type AcceptanceItem struct {
	Text string
	Done bool
}

// FieldValue is the value of a custom field: a scalar or a list of strings.
type FieldValue struct {
	Scalar string
	Typed  bool // unquoted integer, boolean or date literal; emitted verbatim
	List   []string
	IsList bool
	Line   int
}

// Ticket is the parsed content of ticket.md.
type Ticket struct {
	ID, Title, Status, Priority, Kind string
	Labels                            []string
	Assignee, Owner                   string
	Claim                             *Claim
	Parent                            string
	BlockedBy, Related                []string
	DuplicateOf                       string
	Links                             []string
	Acceptance                        []AcceptanceItem
	Milestone, Due                    string
	Fields                            map[string]FieldValue
	MovedTo                           string
	Created, CreatedBy, Updated       string
	Body                              string

	// Lines maps frontmatter keys to their line numbers (for diagnostics).
	Lines map[string]int
}

var (
	labelRe     = regexp.MustCompile(`^[^\s,][^,\n]{0,63}$`)
	fieldNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	sessionRe   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	nameRe      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`) // status, kind, milestone names
)

// ParseTicket parses ticket.md. Diagnostics cover encoding, syntax and the
// config-independent rules; config-dependent rules run in Check.
func ParseTicket(path string, data []byte) (*Ticket, []Diagnostic) {
	d := splitDocument(path, data)
	if d.fm == nil {
		return nil, d.diags
	}
	r := newFieldReader(path, d.fm)
	t := &Ticket{Lines: d.fm.KeyLines, Body: d.body}

	t.ID = r.strRe("id", IDRe, "IDs are PREFIX-SUFFIX with a 5-8 character Crockford base32 suffix, e.g. JK-7QK3M")
	t.Title = r.str("title")
	if strings.ContainsAny(t.Title, "\n\r") {
		r.bad("title", "", "must be a single line")
	} else if len([]rune(t.Title)) > 300 {
		r.bad("title", "shorten it and move detail into the body", "is longer than 300 characters")
	}
	t.Status = r.strRe("status", nameRe, "")
	if p := r.str("priority"); p != "" {
		if !slices.Contains(Priorities, p) {
			r.bad("priority", "use one of P0, P1, P2, P3 (or omit it)", "%q is not a priority", p)
		} else {
			t.Priority = p
		}
	}
	t.Kind = r.strRe("kind", nameRe, "")
	t.Labels = r.strList("labels", labelRe, "labels are single-line, at most 64 characters, without commas")
	t.Assignee = r.strRe("assignee", ActorRe, "actors are `handle`, `agent/<name>` or `role/<name>`, lower-case")
	t.Owner = r.strRe("owner", ActorRe, "")
	if c := r.get("claim"); c != nil {
		if c.Kind != MapNode {
			r.bad("claim", "", "must be a mapping with by, at and optionally session")
		} else {
			cr := newFieldReader(path, c)
			t.Claim = &Claim{
				By:      cr.strRe("by", ActorRe, ""),
				Session: cr.strRe("session", sessionRe, ""),
				At:      cr.strRe("at", TimestampRe, "timestamps are RFC 3339 UTC with seconds, e.g. 2026-11-07T13:07:22Z"),
			}
			if t.Claim.By == "" && cr.get("by") == nil {
				cr.diags = append(cr.diags, diag("missing_field", path, c.Line, "", "claim.by is required"))
			}
			if t.Claim.At == "" && cr.get("at") == nil {
				cr.diags = append(cr.diags, diag("missing_field", path, c.Line, "", "claim.at is required"))
			}
			cr.unknown([]string{"by", "session", "at"})
			r.diags = append(r.diags, prefixKey("claim.", cr.diags)...)
		}
	}
	idFix := "references are ticket IDs such as JK-7QK3M"
	t.Parent = r.strRe("parent", IDRe, idFix)
	t.BlockedBy = r.strList("blocked_by", IDRe, idFix)
	t.Related = r.strList("related", IDRe, idFix)
	t.DuplicateOf = r.strRe("duplicate_of", IDRe, idFix)
	t.Links = r.strList("links", nil, "")
	for _, l := range t.Links {
		if !LinkRe.MatchString(l) && !urlRe.MatchString(l) {
			r.bad("links", "use repo://path[@ref][#Lline] or an http(s) URL", "%q is neither a symbolic code link nor a URL", l)
		}
	}
	if a := r.get("acceptance"); a != nil {
		if a.Kind != SeqNode {
			r.bad("acceptance", "", "must be a list of {text, done} items")
		} else {
			for _, it := range a.Items {
				if it.Kind != MapNode {
					r.bad("acceptance", "", "each item must be a mapping with text and done")
					continue
				}
				ar := newFieldReader(path, it)
				item := AcceptanceItem{Text: ar.str("text")}
				item.Done, _ = ar.boolean("done")
				if item.Text == "" {
					ar.diags = append(ar.diags, diag("missing_field", path, it.Line, "", "acceptance item text is required"))
				}
				ar.unknown([]string{"text", "done"})
				r.diags = append(r.diags, prefixKey("acceptance.", ar.diags)...)
				t.Acceptance = append(t.Acceptance, item)
			}
		}
	}
	t.Milestone = r.strRe("milestone", nameRe, "")
	t.Due = r.strRe("due", DateRe, "dates are YYYY-MM-DD")
	if f := r.get("fields"); f != nil {
		if f.Kind != MapNode {
			r.bad("fields", "", "must be a mapping of custom field names to values")
		} else {
			t.Fields = map[string]FieldValue{}
			for _, k := range f.Keys {
				v := f.Map[k]
				line := f.KeyLines[k]
				if !fieldNameRe.MatchString(k) {
					r.diags = append(r.diags, diag("invalid_value", path, line, "field names are lower_snake_case", "fields: invalid field name %q", k))
					continue
				}
				switch v.Kind {
				case NullNode:
					continue
				case ScalarNode:
					t.Fields[k] = FieldValue{Scalar: v.Value, Typed: !v.Quoted && isTypedLiteral(v.Value), Line: line}
				case SeqNode:
					fv := FieldValue{IsList: true, Line: line}
					for _, it := range v.Items {
						if it.Kind != ScalarNode {
							r.diags = append(r.diags, diag("invalid_value", path, line, "", "fields.%s: list items must be strings", k))
							continue
						}
						fv.List = append(fv.List, it.Value)
					}
					t.Fields[k] = fv
				default:
					r.diags = append(r.diags, diag("invalid_value", path, line, "", "fields.%s: must be a scalar or a list of strings", k))
				}
			}
		}
	}
	t.MovedTo = r.strRe("moved_to", IDRe, idFix)
	tsFix := "timestamps are RFC 3339 UTC with seconds, e.g. 2026-11-07T13:07:22Z"
	t.Created = r.strRe("created", TimestampRe, tsFix)
	t.CreatedBy = r.strRe("created_by", ActorRe, "")
	t.Updated = r.strRe("updated", TimestampRe, tsFix)
	r.unknown(TicketKeys)

	for _, k := range []string{"id", "title", "status", "created", "updated"} {
		if r.node.Map[k] == nil || r.node.Map[k].Kind == NullNode || (r.node.Map[k].Kind == ScalarNode && r.node.Map[k].Value == "") {
			r.diags = append(r.diags, diag("missing_field", path, d.fmLine, "", "required field %q is missing", k))
		}
	}
	diags := append(d.diags, r.diags...)
	if !HasErrors(diags, false) && d.encoded {
		if string(FormatTicket(t)) != string(data) {
			diags = append(diags, diag("non_canonical", path, 0, "run `ka fmt`", "file is not in canonical form"))
		}
	}
	return t, diags
}

var typedLiteralRe = regexp.MustCompile(`^(true|false|-?(0|[1-9][0-9]*)(\.[0-9]+)?|\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}:\d{2}Z)?)$`)

func isTypedLiteral(s string) bool { return typedLiteralRe.MatchString(s) }

func prefixKey(prefix string, ds []Diagnostic) []Diagnostic {
	for i := range ds {
		ds[i].Message = prefix + ds[i].Message
	}
	return ds
}

// FormatTicket renders a ticket in canonical form. Set-valued lists
// (labels, blocked_by, related) are sorted and de-duplicated.
func FormatTicket(t *Ticket) []byte {
	fm := []KV{
		{"id", t.ID},
		{"title", t.Title},
		{"status", t.Status},
		{"priority", t.Priority},
		{"kind", t.Kind},
		{"labels", toAny(sortedSet(t.Labels))},
		{"assignee", t.Assignee},
		{"owner", t.Owner},
	}
	if t.Claim != nil {
		fm = append(fm, KV{"claim", []KV{{"by", t.Claim.By}, {"session", t.Claim.Session}, {"at", rawOrEmpty(t.Claim.At)}}})
	}
	fm = append(fm,
		KV{"parent", t.Parent},
		KV{"blocked_by", toAny(sortedSet(t.BlockedBy))},
		KV{"related", toAny(sortedSet(t.Related))},
		KV{"duplicate_of", t.DuplicateOf},
		KV{"links", toAny(t.Links)},
	)
	if len(t.Acceptance) > 0 {
		items := make([]any, 0, len(t.Acceptance))
		for _, a := range t.Acceptance {
			done := Raw("false")
			if a.Done {
				done = Raw("true")
			}
			items = append(items, []KV{{"text", a.Text}, {"done", done}})
		}
		fm = append(fm, KV{"acceptance", items})
	}
	fm = append(fm, KV{"milestone", t.Milestone}, KV{"due", rawOrEmpty(t.Due)})
	if len(t.Fields) > 0 {
		names := make([]string, 0, len(t.Fields))
		for k := range t.Fields {
			names = append(names, k)
		}
		sort.Strings(names)
		var fkv []KV
		for _, k := range names {
			v := t.Fields[k]
			switch {
			case v.IsList:
				fkv = append(fkv, KV{k, toAny(v.List)})
			case v.Typed:
				fkv = append(fkv, KV{k, Raw(v.Scalar)})
			default:
				fkv = append(fkv, KV{k, v.Scalar})
			}
		}
		fm = append(fm, KV{"fields", fkv})
	}
	fm = append(fm,
		KV{"moved_to", t.MovedTo},
		KV{"created", rawOrEmpty(t.Created)},
		KV{"created_by", t.CreatedBy},
		KV{"updated", rawOrEmpty(t.Updated)},
	)
	return renderDocument(dropEmptyStrings(fm), t.Body)
}

func rawOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return Raw(s)
}

func dropEmptyStrings(fm []KV) []KV {
	out := fm[:0:0]
	for _, kv := range fm {
		if s, ok := kv.V.(string); ok && s == "" {
			continue
		}
		if sub, ok := kv.V.([]KV); ok {
			kv.V = dropEmptyStrings(sub)
		}
		out = append(out, kv)
	}
	return out
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func sortedSet(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := slices.Clone(ss)
	sort.Strings(out)
	return slices.Compact(out)
}
