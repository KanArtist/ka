package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

const idSeedVersion = "ka-migrate-v0/1"

// ---- ID assignment -----------------------------------------------------------
//
// A v0 ticket's identity is its file basename (v0 never renamed ticket files).
// Its v1 ID is the first usable candidate from a deterministic sequence seeded
// by (project, basename). On a re-run, a candidate that already exists in the
// output with the same legacy_id is reused, so IDs are stable across runs even
// when a collision pushed a ticket past its first candidate. New tickets are
// assigned only after every existing one is matched, so they never take an ID
// that belongs to a ticket from an earlier run.

func candidate(prefix, project, base string, k int) string {
	return prefix + "-" + kaspec.HashBase32(fmt.Sprintf("%s|%s|%s|%d", idSeedVersion, project, base, k), kaspec.DefaultSuffixLength)
}

type existing struct {
	legacy map[string]string // v1 ID -> legacy_id, for every ticket already in the output
}

func assignIDs(c *v0Corpus, ex existing) map[*v0Ticket]string {
	out := map[*v0Ticket]string{}
	taken := map[string]bool{}
	for id := range ex.legacy {
		taken[id] = true
	}
	claimed := map[string]bool{}
	const maxK = 32
	// pass 1: re-find tickets from earlier runs
	for _, t := range c.Tickets {
		prefix := c.Projects[t.Project].Prefix
		for k := range maxK {
			id := candidate(prefix, t.Project, t.Base, k)
			if ex.legacy[id] == t.LegacyID && !claimed[id] {
				out[t] = id
				claimed[id] = true
				break
			}
		}
	}
	// pass 2: new tickets take the first free candidate
	for _, t := range c.Tickets {
		if out[t] != "" {
			continue
		}
		prefix := c.Projects[t.Project].Prefix
		for k := range maxK {
			id := candidate(prefix, t.Project, t.Base, k)
			_, suffix, _ := kaspec.ParseID(id)
			if taken[id] || claimed[id] || kaspec.LowEntropy(suffix) {
				continue
			}
			out[t] = id
			claimed[id] = true
			break
		}
	}
	return out
}

// ---- reference rewriting --------------------------------------------------------

type refMap struct {
	byLegacy map[string][]string // legacy id -> new ids
	re       *regexp.Regexp
}

func newRefMap(c *v0Corpus, ids map[*v0Ticket]string) *refMap {
	m := &refMap{byLegacy: map[string][]string{}}
	prefixes := map[string]bool{}
	for _, t := range c.Tickets {
		m.byLegacy[t.LegacyID] = append(m.byLegacy[t.LegacyID], ids[t])
		if p, _, ok := strings.Cut(t.LegacyID, "-"); ok {
			prefixes[p] = true
		}
	}
	var ps []string
	for p := range prefixes {
		ps = append(ps, regexp.QuoteMeta(p))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ps)))
	m.re = regexp.MustCompile(`\b(?:` + strings.Join(ps, "|") + `)-[0-9]+\b`)
	return m
}

// resolve maps a legacy id to its new id when unambiguous.
func (m *refMap) resolve(legacy string) (string, bool) {
	ids := m.byLegacy[legacy]
	if len(ids) != 1 {
		return "", false
	}
	return ids[0], true
}

// rewrite replaces unambiguous legacy ids in text.
func (m *refMap) rewrite(text string) string {
	return m.re.ReplaceAllStringFunc(text, func(s string) string {
		if id, ok := m.resolve(s); ok {
			return id
		}
		return s
	})
}

// ---- ticket transform ------------------------------------------------------------

type plannedComment struct {
	Name    string
	Comment *kaspec.Comment
}

type transformed struct {
	Ticket   *kaspec.Ticket
	Comments []plannedComment
}

type stats struct {
	assigneeMapped   map[string]int // "from -> to"
	kindsMapped      map[string]int
	droppedDeps      []string
	droppedLinks     []string
	droppedLabels    []string
	priorityDropped  []string
	statusFixed      []string
	extractedByHead  map[string]int
	commentsByType   map[string]int
	acceptanceItems  int
	acceptanceTicket int
	placeholderDrops int
	titlesShortened  []string
	crlfFixed        int
	unknownKeys      map[string]int
	claims           int
}

func newStats() *stats {
	return &stats{assigneeMapped: map[string]int{}, kindsMapped: map[string]int{}, extractedByHead: map[string]int{}, commentsByType: map[string]int{}, unknownKeys: map[string]int{}}
}

var v1Kinds = []string{"feature", "bug", "chore", "research", "design", "refactor", "perf"}
var v0Statuses = []string{"backlog", "ready", "in_progress", "blocked", "review", "done"}

func dateTS(d string) string {
	if kaspec.DateRe.MatchString(d) {
		return d + "T00:00:00Z"
	}
	if kaspec.TimestampRe.MatchString(d) {
		return d
	}
	return ""
}

func normAssignee(raw string, st *stats) string {
	k := strings.ToLower(strings.Trim(strings.TrimSpace(raw), `"`))
	if v, ok := assigneeAliases[k]; ok {
		if raw != v && raw != "" {
			st.assigneeMapped[fmt.Sprintf("%q -> %q", raw, v)]++
		}
		return v
	}
	h := strings.ReplaceAll(k, " ", ".")
	if kaspec.HumanRe.MatchString(h) {
		if h != raw {
			st.assigneeMapped[fmt.Sprintf("%q -> %q", raw, h)]++
		}
		return h
	}
	st.assigneeMapped[fmt.Sprintf("%q -> (dropped)", raw)]++
	return ""
}

func transform(t *v0Ticket, id string, refs *refMap, st *stats) transformed {
	nt := &kaspec.Ticket{ID: id, Fields: map[string]kaspec.FieldValue{}}
	for _, k := range t.Meta.Keys {
		if !v0Keys[k] {
			st.unknownKeys[k]++
		}
	}
	if t.HadCRLF {
		st.crlfFixed++
	}

	// title
	title := strings.Join(strings.Fields(refs.rewrite(t.str("title"))), " ")
	fullTitle := ""
	if utf8.RuneCountInString(title) > 300 {
		fullTitle = title
		title = shorten(title, 299) + "…"
		st.titlesShortened = append(st.titlesShortened, t.LegacyID)
	}
	nt.Title = title

	// status, priority, kind
	nt.Status = t.str("status")
	if !slices.Contains(v0Statuses, nt.Status) {
		st.statusFixed = append(st.statusFixed, fmt.Sprintf("%s: %q -> backlog", t.LegacyID, nt.Status))
		nt.Status = "backlog"
	}
	if p := t.str("priority"); slices.Contains(kaspec.Priorities, p) {
		nt.Priority = p
	} else if p != "" {
		st.priorityDropped = append(st.priorityDropped, fmt.Sprintf("%s: %q", t.LegacyID, p))
	}
	kind := t.str("kind")
	if m, ok := kindMap[kind]; ok {
		st.kindsMapped[fmt.Sprintf("%s -> %s", kind, m)]++
		kind = m
	} else if kind != "" && !slices.Contains(v1Kinds, kind) {
		st.kindsMapped[fmt.Sprintf("%s (%s) -> feature", kind, t.LegacyID)]++
		kind = "feature"
	}
	nt.Kind = kind

	// labels
	var relLabels []relLabel
	for _, raw := range t.list("labels") {
		// some v0 labels are two labels joined by a comma ("windows,cache")
		for _, l := range strings.Split(raw, ",") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			// v0 tooling sometimes wrote relations as labels:
			// "parent:JK-4398" and "--depends-on JK-4360 JK-4361".
			if rest, ok := strings.CutPrefix(l, "parent:"); ok {
				relLabels = append(relLabels, relLabel{parent: strings.TrimSpace(rest), raw: l})
				continue
			}
			if rest, ok := strings.CutPrefix(l, "--depends-on"); ok {
				relLabels = append(relLabels, relLabel{deps: strings.Fields(rest), raw: l})
				continue
			}
			if strings.ContainsRune(l, '\n') || utf8.RuneCountInString(l) > 64 {
				st.droppedLabels = append(st.droppedLabels, fmt.Sprintf("%s: %q", t.LegacyID, l))
				continue
			}
			nt.Labels = append(nt.Labels, l)
		}
	}

	// people
	nt.Assignee = normAssignee(t.str("assignee"), st)
	updated := dateTS(t.str("updated"))
	created := dateTS(t.str("created"))
	if created == "" {
		created = updated
	}
	if updated == "" || updated < created {
		updated = created
	}
	nt.Created, nt.Updated = created, updated
	if nt.Status == "in_progress" && nt.Assignee != "" {
		at := dateTS(t.str("claimed_at"))
		if at == "" {
			at = updated
		}
		nt.Claim = &kaspec.Claim{By: nt.Assignee, At: at}
		st.claims++
	}

	// relations
	for _, dep := range t.list("depends_on") {
		nid, ok := refs.resolve(dep)
		switch {
		case !ok:
			st.droppedDeps = append(st.droppedDeps, fmt.Sprintf("%s -> %s", t.LegacyID, dep))
		case nid == id:
		default:
			nt.BlockedBy = append(nt.BlockedBy, nid)
		}
	}
	for _, rl := range relLabels {
		if rl.parent != "" {
			if nid, ok := refs.resolve(rl.parent); ok && nid != id && nt.Parent == "" {
				nt.Parent = nid
			} else {
				nt.Labels = append(nt.Labels, rl.raw)
			}
		}
		for _, dep := range rl.deps {
			nid, ok := refs.resolve(dep)
			switch {
			case !ok:
				st.droppedDeps = append(st.droppedDeps, fmt.Sprintf("%s -> %s", t.LegacyID, dep))
			case nid == id:
			default:
				nt.BlockedBy = append(nt.BlockedBy, nid)
			}
		}
	}
	for _, l := range t.list("links") {
		if kaspec.LinkRe.MatchString(l) || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
			nt.Links = append(nt.Links, l)
		} else {
			st.droppedLinks = append(st.droppedLinks, fmt.Sprintf("%s: %q", t.LegacyID, l))
		}
	}

	// custom fields
	nt.Fields["legacy_id"] = kaspec.FieldValue{Scalar: t.LegacyID}
	for _, k := range extensionFields {
		if v := t.str(k); v != "" {
			nt.Fields[k] = kaspec.FieldValue{Scalar: strings.Join(strings.Fields(refs.rewrite(v)), " ")}
		}
	}

	// body
	author := nt.Assignee
	if author == "" {
		author = "agent/migrate-v0"
	}
	b := restructure(t, nt, refs, st)
	if fullTitle != "" {
		b.body = "**Full title:** " + fullTitle + "\n\n" + b.body
	}
	nt.Body = b.body
	nt.Acceptance = b.acceptance

	var comments []plannedComment
	addComment := func(typ, created, heading, body, resolution string) {
		idx := len(comments)
		ts := offsetTS(created, idx)
		c := &kaspec.Comment{Type: typ, Author: author, Created: ts, Resolution: resolution, Body: body}
		suffix := kaspec.HashBase32(fmt.Sprintf("%s|comment|%s|%d|%s", idSeedVersion, t.Base, idx, heading), 4)
		comments = append(comments, plannedComment{Name: kaspec.CommentFilename(ts, suffix), Comment: c})
		st.commentsByType[typ]++
	}
	for _, h := range b.history {
		created := updated
		if d := dateInRe.FindString(h.heading); d != "" {
			created = d + "T00:00:00Z"
		}
		res := ""
		if h.typ == "resolution" {
			res = "done"
		}
		addComment(h.typ, created, h.heading, "### "+h.heading+"\n\n"+refs.rewrite(h.content), res)
	}
	if sn := t.str("status_note"); sn != "" {
		addComment("note", updated, "status_note", "**Status note (v0):** "+refs.rewrite(sn), "")
	}
	return transformed{Ticket: nt, Comments: comments}
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:—-")
}

// offsetTS adds idx seconds to a midnight timestamp so comments that share a
// date keep their document order and get distinct filenames.
func offsetTS(ts string, idx int) string {
	if idx == 0 || len(ts) != 20 {
		return ts
	}
	return fmt.Sprintf("%sT%02d:%02d:%02dZ", ts[:10], idx/3600%24, idx/60%60, idx%60)
}

// ---- body restructuring -------------------------------------------------------------

var (
	dateInRe      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	h12Re         = regexp.MustCompile(`^(#{1,2})\s+(.*?)\s*$`)
	fenceLineRe   = regexp.MustCompile("^\\s*(```|~~~)")
	resolutionRe  = regexp.MustCompile(`(?i)^(outcome|resolution|done notes|done|landed|what landed|closing note|shipped|closed by audit|closed|resolved)(\s*$|\s*[(:,—–-])`)
	noteRe        = regexp.MustCompile(`(?i)^(audit note|amendment|correction|note\s*\(\d{4}|status update)`)
	progressRe    = regexp.MustCompile(`(?i)^progress(\s*$|\s*[(:,—–-])`)
	acceptanceRe  = regexp.MustCompile(`(?i)^acceptance(\s*$|\s*[(:])`)
	checkItemRe   = regexp.MustCompile(`^[-*]\s+\[([ xX])\]\s+(.*\S)\s*$`)
	contLineRe    = regexp.MustCompile(`^\s{2,}\S`)
	templateHints = []string{"_What's wrong, or what do we want? Keep it concrete._"}
)

type historySection struct {
	typ, heading, content string
}

type restructured struct {
	body       string
	acceptance []kaspec.AcceptanceItem
	history    []historySection
}

type section struct {
	level   int    // 1 or 2; 0 for the preamble
	heading string // "" for the preamble
	lines   []string
}

func splitSections(body string) []section {
	secs := []section{{}}
	inFence := false
	for _, l := range strings.Split(body, "\n") {
		if fenceLineRe.MatchString(l) {
			inFence = !inFence
		}
		if !inFence {
			if m := h12Re.FindStringSubmatch(l); m != nil {
				secs = append(secs, section{level: len(m[1]), heading: m[2]})
				continue
			}
		}
		secs[len(secs)-1].lines = append(secs[len(secs)-1].lines, l)
	}
	return secs
}

func placeholderOnly(lines []string) bool {
	for _, l := range lines {
		s := strings.TrimSpace(l)
		switch {
		case s == "", s == "-", s == "*", s == "- [ ]", s == "* [ ]", s == "- [x]", s == "- [X]":
		case strings.HasPrefix(s, "<!--") && strings.HasSuffix(s, "-->"):
		case slices.Contains(templateHints, s):
		default:
			return false
		}
	}
	return true
}

// parseChecklist returns the items when the lines are only a checklist
// (with optional indented continuation lines and blank lines).
func parseChecklist(lines []string) ([]kaspec.AcceptanceItem, bool) {
	var items []kaspec.AcceptanceItem
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if m := checkItemRe.FindStringSubmatch(l); m != nil {
			items = append(items, kaspec.AcceptanceItem{Text: m[2], Done: m[1] != " "})
			continue
		}
		if contLineRe.MatchString(l) && len(items) > 0 && !strings.HasPrefix(strings.TrimSpace(l), "-") && !strings.HasPrefix(strings.TrimSpace(l), "*") {
			items[len(items)-1].Text += " " + strings.TrimSpace(l)
			continue
		}
		return nil, false
	}
	return items, len(items) > 0
}

func restructure(t *v0Ticket, nt *kaspec.Ticket, refs *refMap, st *stats) restructured {
	var r restructured
	secs := splitSections(t.Body)

	var kept []string
	appendLines := func(ls []string) { kept = append(kept, ls...) }
	appendLines(secs[0].lines)
	for _, s := range secs[1:] {
		content := strings.Trim(strings.Join(s.lines, "\n"), "\n")
		if s.level == 1 {
			// "# JK-1234 — Title" repeats the frontmatter: drop it, or keep a differing title without the id
			if h, ok := h1Title(s.heading, t); ok {
				if h != "" {
					appendLines([]string{"# " + h})
				}
			} else {
				appendLines([]string{"# " + s.heading})
			}
			appendLines(s.lines)
			continue
		}
		switch {
		case resolutionRe.MatchString(s.heading):
			typ := "note"
			if nt.Status == "done" {
				typ = "resolution"
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			r.history = append(r.history, historySection{typ: typ, heading: s.heading, content: content})
			st.extractedByHead[headingKey(s.heading)]++
		case noteRe.MatchString(s.heading) || progressRe.MatchString(s.heading):
			typ := "note"
			if progressRe.MatchString(s.heading) {
				typ = "progress"
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			r.history = append(r.history, historySection{typ: typ, heading: s.heading, content: content})
			st.extractedByHead[headingKey(s.heading)]++
		case placeholderOnly(s.lines):
			st.placeholderDrops++
		case acceptanceRe.MatchString(s.heading):
			if items, ok := parseChecklist(s.lines); ok {
				for i := range items {
					items[i].Text = refs.rewrite(items[i].Text)
				}
				r.acceptance = append(r.acceptance, items...)
				st.acceptanceItems += len(items)
				continue
			}
			appendLines([]string{"## " + s.heading})
			appendLines(s.lines)
		default:
			appendLines([]string{"## " + s.heading})
			appendLines(s.lines)
		}
	}
	if len(r.acceptance) > 0 {
		st.acceptanceTicket++
	}
	r.body = refs.rewrite(strings.Join(dropEmptyH1(kept), "\n"))
	return r
}

// dropEmptyH1 removes level-1 headings left with no content once template
// sections were dropped (the next non-blank line is another H1, or nothing).
func dropEmptyH1(lines []string) []string {
	var out []string
	for i, l := range lines {
		if strings.HasPrefix(l, "# ") {
			next := ""
			for _, n := range lines[i+1:] {
				if strings.TrimSpace(n) != "" {
					next = n
					break
				}
			}
			if next == "" || strings.HasPrefix(next, "# ") {
				continue
			}
		}
		out = append(out, l)
	}
	return out
}

// h1Title reports whether an H1 heading names the ticket's legacy id. It
// returns "" when the rest repeats the title, else the rest without the id.
func h1Title(heading string, t *v0Ticket) (string, bool) {
	if !strings.HasPrefix(heading, t.LegacyID) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(heading, t.LegacyID))
	rest = strings.TrimSpace(strings.TrimLeft(rest, "—–-:"))
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	if rest == "" || norm(rest) == norm(t.str("title")) {
		return "", true
	}
	return rest, true
}

func headingKey(h string) string {
	h = dateInRe.ReplaceAllString(h, "")
	h = strings.Trim(h, " ()—–-:,")
	if i := strings.IndexAny(h, "(:—–,"); i > 0 {
		h = strings.TrimSpace(h[:i])
	}
	return h
}

// relLabel is a relation v0 recorded as a label.
type relLabel struct {
	parent string   // legacy id from "parent:<id>"
	deps   []string // legacy ids from "--depends-on <id> <id>"
	raw    string
}
