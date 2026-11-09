package engine

import (
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// Query is a parsed filter in the shared query language (PRD §7.7):
//
//	project:jk,gyggly category:started -label:infra assignee:agent/* flaky cache
//
// Terms are ANDed; comma-separated values within a term are ORed; a leading
// "-" negates a term; "*" in a value is a wildcard; "none" matches an empty
// field; bare words search the title and description.
type Query struct {
	terms []term
	text  []textTerm
	Sort  []string
}

type term struct {
	key    string
	values []string
	neg    bool
}

type textTerm struct {
	s   string
	neg bool
}

// QueryKeys are the recognized filter keys.
var QueryKeys = []string{"id", "project", "store", "status", "category", "priority", "kind", "label",
	"assignee", "owner", "claimed", "parent", "blocked_by", "blocks", "created_by", "is", "has", "sort"}

var keyAliases = map[string]string{"cat": "category", "p": "priority", "labels": "label", "proj": "project", "state": "status"}

// ParseQuery parses a query string.
func ParseQuery(s string) (*Query, error) {
	q := &Query{}
	for _, tok := range tokenize(s) {
		neg := false
		raw := tok
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			neg, tok = true, tok[1:]
		}
		k, v, ok := strings.Cut(tok, ":")
		if ok && k != "" && !strings.Contains(k, " ") && !strings.HasPrefix(raw, "\"") {
			k = strings.ToLower(k)
			if a, ok := keyAliases[k]; ok {
				k = a
			}
			if !slices.Contains(QueryKeys, k) && !strings.HasPrefix(k, "field.") && !strings.HasPrefix(k, "fields.") {
				fix := "filter keys: " + strings.Join(QueryKeys, ", ") + ", field.<name>"
				if n := nearest(k, QueryKeys); n != "" {
					fix = "did you mean " + n + ":? " + fix
				}
				return nil, Errf(CodeInvalid, fix, "unknown filter key %q", k)
			}
			k = strings.Replace(k, "fields.", "field.", 1)
			vals := strings.Split(v, ",")
			if k == "sort" {
				q.Sort = append(q.Sort, vals...)
				continue
			}
			q.terms = append(q.terms, term{key: k, values: vals, neg: neg})
			continue
		}
		q.text = append(q.text, textTerm{s: strings.ToLower(strings.Trim(tok, "\"")), neg: neg})
	}
	return q, nil
}

func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// Has reports whether the query constrains a key.
func (q *Query) Has(key string) bool {
	for _, t := range q.terms {
		if t.key == key {
			return true
		}
	}
	return false
}

func valueMatch(pattern, v string) bool {
	if pattern == "none" {
		return v == ""
	}
	if strings.Contains(pattern, "*") {
		ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(v))
		return ok
	}
	return strings.EqualFold(pattern, v)
}

func anyMatch(patterns, vals []string) bool {
	if len(vals) == 0 {
		vals = []string{""}
	}
	for _, p := range patterns {
		for _, v := range vals {
			if valueMatch(p, v) {
				return true
			}
		}
	}
	return false
}

// view gives the matcher access to derived state.
type view struct {
	ix   *Index
	now  time.Time
	full bool // summaries carry owner, created and custom fields
}

func (v view) isBlocked(r *Rec) bool {
	if st, ok := r.Project.Cfg.StatusByName(r.T.Status); ok && st.Blocked {
		return true
	}
	return len(v.openBlockers(r)) > 0
}

// openBlockers lists blocked_by targets that are not finished. Targets
// outside the workspace cannot be seen and do not block.
func (v view) openBlockers(r *Rec) []string {
	var out []string
	for _, b := range r.T.BlockedBy {
		br, ok := v.ix.tickets[b]
		if !ok {
			continue
		}
		if !closedCategory(category(br.Project.Cfg, br.T.Status)) {
			out = append(out, b)
		}
	}
	return out
}

func closedCategory(c string) bool { return c == "completed" || c == "canceled" }

func (v view) stale(r *Rec) bool {
	return r.T.Claim != nil && claimStale(r.T.Claim, r.Project.Cfg, v.now.UTC().Format(time.RFC3339))
}

// Match reports whether a ticket satisfies the query.
func (q *Query) match(v view, r *Rec) bool {
	t := r.T
	for _, tm := range q.terms {
		var ok bool
		switch tm.key {
		case "id":
			ok = anyMatch(tm.values, []string{r.ID})
		case "project":
			ok = anyMatch(tm.values, []string{r.Project.Key})
		case "store":
			ok = anyMatch(tm.values, []string{r.Store.Name})
		case "status":
			ok = anyMatch(tm.values, []string{t.Status})
		case "category":
			ok = anyMatch(tm.values, []string{category(r.Project.Cfg, t.Status)})
		case "priority":
			ok = anyMatch(tm.values, []string{t.Priority})
		case "kind":
			ok = anyMatch(tm.values, []string{t.Kind})
		case "label":
			ok = anyMatch(tm.values, t.Labels)
		case "assignee":
			ok = anyMatch(tm.values, []string{t.Assignee})
		case "owner":
			ok = anyMatch(tm.values, []string{t.Owner})
		case "created_by":
			ok = anyMatch(tm.values, []string{t.CreatedBy})
		case "parent":
			ok = anyMatch(tm.values, []string{t.Parent})
		case "blocked_by":
			ok = anyMatch(tm.values, t.BlockedBy)
		case "blocks":
			ok = anyMatch(tm.values, v.ix.blocks[r.ID])
		case "claimed":
			by := ""
			if t.Claim != nil {
				by = t.Claim.By
			}
			for _, val := range tm.values {
				switch strings.ToLower(val) {
				case "true", "yes":
					ok = ok || by != ""
				case "false", "no":
					ok = ok || by == ""
				default:
					ok = ok || valueMatch(val, by)
				}
			}
		case "is":
			for _, val := range tm.values {
				ok = ok || v.is(r, strings.ToLower(val))
			}
		case "has":
			for _, val := range tm.values {
				ok = ok || has(r, strings.ToLower(val))
			}
		default:
			if name, isField := strings.CutPrefix(tm.key, "field."); isField {
				fv, present := t.Fields[name]
				switch {
				case !present:
					ok = anyMatch(tm.values, nil)
				case fv.IsList:
					ok = anyMatch(tm.values, fv.List)
				default:
					ok = anyMatch(tm.values, []string{fv.Scalar})
				}
			}
		}
		if ok == tm.neg {
			return false
		}
	}
	for _, tt := range q.text {
		found := strings.Contains(r.text, tt.s) || strings.EqualFold(r.ID, tt.s)
		if found == tt.neg {
			return false
		}
	}
	return true
}

func (v view) is(r *Rec, what string) bool {
	cat := category(r.Project.Cfg, r.T.Status)
	switch what {
	case "open":
		return !closedCategory(cat)
	case "closed":
		return closedCategory(cat)
	case "claimed":
		return r.T.Claim != nil
	case "unclaimed":
		return r.T.Claim == nil
	case "stale":
		return v.stale(r)
	case "blocked":
		return v.isBlocked(r)
	case "unblocked":
		return !v.isBlocked(r)
	case "ready":
		return cat == "unstarted" && r.T.Claim == nil && !v.isBlocked(r)
	case "invalid":
		return r.Invalid
	case "archived":
		return r.Project.File.Archived
	case "question", "asking":
		return len(openQuestions(r)) > 0
	}
	return false
}

func has(r *Rec, what string) bool {
	t := r.T
	switch what {
	case "parent":
		return t.Parent != ""
	case "assignee":
		return t.Assignee != ""
	case "priority":
		return t.Priority != ""
	case "labels", "label":
		return len(t.Labels) > 0
	case "acceptance":
		return len(t.Acceptance) > 0
	case "blocked_by", "blockers":
		return len(t.BlockedBy) > 0
	case "links":
		return len(t.Links) > 0
	case "comments":
		return len(r.Comments) > 0
	}
	_, ok := t.Fields[strings.TrimPrefix(what, "field.")]
	return ok
}

func priorityRank(p string) int {
	if i := slices.Index(kaspec.Priorities, p); i >= 0 {
		return i
	}
	return len(kaspec.Priorities)
}

// sortRecs orders tickets by sort keys (priority, created, updated, id,
// title, status; "-" prefix for descending).
func sortRecs(rs []*Rec, keys []string) {
	if len(keys) == 0 {
		keys = []string{"priority", "-updated"}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		for _, k := range keys {
			desc := strings.HasPrefix(k, "-")
			k = strings.TrimPrefix(k, "-")
			var c int
			switch k {
			case "priority":
				c = priorityRank(a.T.Priority) - priorityRank(b.T.Priority)
			case "created":
				c = strings.Compare(a.T.Created, b.T.Created)
			case "updated":
				c = strings.Compare(a.T.Updated, b.T.Updated)
			case "title":
				c = strings.Compare(strings.ToLower(a.T.Title), strings.ToLower(b.T.Title))
			case "status":
				c = strings.Compare(a.T.Status, b.T.Status)
			case "id":
				c = strings.Compare(a.ID, b.ID)
			}
			if desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return a.ID < b.ID
	})
}

// openQuestions lists question comments no answer or approval names yet.
func openQuestions(r *Rec) []*kaspec.CommentEntry {
	answered := map[string]bool{}
	retracted := map[string]bool{}
	for _, c := range r.Comments {
		if c.Comment == nil {
			continue
		}
		if c.Comment.Answers != "" {
			answered[c.Comment.Answers] = true
		}
		if c.Comment.Retracts != "" {
			retracted[c.Comment.Retracts] = true
		}
	}
	var out []*kaspec.CommentEntry
	for _, c := range r.Comments {
		if c.Comment != nil && c.Comment.Type == "question" && !answered[c.Name] && !retracted[c.Name] {
			out = append(out, c)
		}
	}
	return out
}
