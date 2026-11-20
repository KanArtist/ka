package kaspec

import (
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// History summaries (spec §2, §10.1): when a store's git history is
// compacted, ka writes history/<yyyymmddThhmmssZ>-<XXXX>.md at the store
// root. Its frontmatter records machine facts about the history that was
// dropped; its body is an optional narrative. Summary files are never
// rewritten or removed, so every compaction's record survives later ones.

// CompactionMinKa is the first ka release that understands compacted
// stores: a compaction raises requires_ka to at least this, so older clients
// stop with "upgrade" instead of "shares no history".
const CompactionMinKa = "1.3.0"

// HistoryDir is the store-relative directory of history summaries.
const HistoryDir = "history"

var (
	// HistoryFileRe matches a summary file name: <yyyymmddThhmmssZ>-<4 base32>.md.
	HistoryFileRe = regexp.MustCompile(`^(\d{8}T\d{6}Z)-([0-9A-HJKMNP-TV-Z]{4})\.md$`)
	// CommitRe matches a full commit or tree hash (SHA-1 or SHA-256).
	CommitRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// HistoryKeys is the canonical frontmatter key order of a summary.
var HistoryKeys = []string{
	"compacted_at", "by", "on_behalf_of", "from", "tree", "commits", "first_commit", "last_commit",
	"actors", "tickets_created", "tickets_closed", "previous",
}

// HistorySummary is one history/<name>.md file.
type HistorySummary struct {
	CompactedAt string `json:"compacted_at"`
	By          string `json:"by"`
	OnBehalfOf  string `json:"on_behalf_of,omitempty"`
	// From is the last commit of the history that was compacted, and Tree
	// its tree: clients that still have From recognize the new history.
	From        string           `json:"from"`
	Tree        string           `json:"tree"`
	Commits     int64            `json:"commits"`
	FirstCommit string           `json:"first_commit,omitempty"`
	LastCommit  string           `json:"last_commit,omitempty"`
	Actors      map[string]int64 `json:"actors,omitempty" doc:"Commits per actor in the compacted history."`
	Created     int64            `json:"tickets_created,omitempty"`
	Closed      int64            `json:"tickets_closed,omitempty"`
	Previous    string           `json:"previous,omitempty" doc:"The summary file of the compaction before this one."`
	Body        string           `json:"body,omitempty"`
}

// HistoryFilename names a summary from a canonical timestamp and a
// 4-character suffix (the comment naming rule, spec §7.1).
func HistoryFilename(at, suffix string) string { return CommentFilename(at, suffix) }

// ParseHistorySummary parses a summary file.
func ParseHistorySummary(path string, data []byte) (*HistorySummary, []Diagnostic) {
	d := splitDocument(path, data)
	if d.fm == nil {
		return nil, d.diags
	}
	r := newFieldReader(path, d.fm)
	h := &HistorySummary{Body: d.body}
	h.CompactedAt = r.strRe("compacted_at", TimestampRe, "timestamps are RFC 3339 UTC with seconds, e.g. 2026-11-07T13:07:22Z")
	h.By = r.strRe("by", ActorRe, "actors are `handle` or `agent/<name>`, lower-case")
	h.OnBehalfOf = r.strRe("on_behalf_of", HumanRe, "on_behalf_of names a human actor")
	h.From = r.strRe("from", CommitRe, "a full commit hash")
	h.Tree = r.strRe("tree", CommitRe, "a full tree hash")
	if n, ok := r.integer("commits"); ok {
		if n < 0 {
			r.bad("commits", "", "must not be negative")
		}
		h.Commits = n
	}
	h.FirstCommit = r.strRe("first_commit", TimestampRe, "")
	h.LastCommit = r.strRe("last_commit", TimestampRe, "")
	if a := r.get("actors"); a != nil {
		if a.Kind != MapNode {
			r.bad("actors", "", "must be a mapping of actors to commit counts")
		} else {
			h.Actors = map[string]int64{}
			ar := newFieldReader(path, a)
			for _, k := range a.Keys {
				if n, ok := ar.integer(k); ok {
					h.Actors[k] = n
				}
			}
			r.diags = append(r.diags, ar.diags...)
		}
	}
	h.Created, _ = r.integer("tickets_created")
	h.Closed, _ = r.integer("tickets_closed")
	h.Previous = r.strRe("previous", HistoryFileRe, "previous names another file in history/")
	r.unknown(HistoryKeys)
	for _, key := range []string{"compacted_at", "by", "from", "tree", "commits"} {
		if v := d.fm.Map[key]; v == nil || v.Kind == NullNode || (v.Kind == ScalarNode && v.Value == "") {
			r.diags = append(r.diags, diag("missing_field", path, d.fmLine, "", "%q is required", key))
		}
	}
	diags := append(d.diags, r.diags...)
	if !HasErrors(diags, false) && d.encoded {
		if string(FormatHistorySummary(h)) != string(data) {
			diags = append(diags, diag("non_canonical", path, 0, "ka writes summaries canonically; restore the file from the commit that added it", "file is not in canonical form"))
		}
	}
	return h, diags
}

// FormatHistorySummary renders a summary in canonical form.
func FormatHistorySummary(h *HistorySummary) []byte {
	var actors []KV
	names := make([]string, 0, len(h.Actors))
	for k := range h.Actors {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		actors = append(actors, KV{k, Raw(strconv.FormatInt(h.Actors[k], 10))})
	}
	count := func(n int64) any {
		if n == 0 {
			return nil
		}
		return Raw(strconv.FormatInt(n, 10))
	}
	fm := []KV{
		{"compacted_at", rawOrEmpty(h.CompactedAt)},
		{"by", h.By},
		{"on_behalf_of", h.OnBehalfOf},
		{"from", h.From},
		{"tree", h.Tree},
		{"commits", Raw(strconv.FormatInt(h.Commits, 10))},
		{"first_commit", rawOrEmpty(h.FirstCommit)},
		{"last_commit", rawOrEmpty(h.LastCommit)},
		{"actors", actors},
		{"tickets_created", count(h.Created)},
		{"tickets_closed", count(h.Closed)},
		{"previous", h.Previous},
	}
	return renderDocument(dropEmptyStrings(fm), h.Body)
}

// loadHistory reads history/: every summary is parsed (its diagnostics go
// to the store); anything else there is a stray file.
func (s *Store) loadHistory(root []fs.DirEntry) {
	for _, e := range s.listEntry(HistoryDir, entry(root, HistoryDir), s.add) {
		rel := HistoryDir + "/" + e.Name()
		if e.IsDir() || !HistoryFileRe.MatchString(e.Name()) {
			s.add(diag("stray_file", s.p(rel), 0, "history/ holds only summaries named <yyyymmddThhmmssZ>-<XXXX>.md", "unexpected entry in history/"))
			continue
		}
		data, err := s.readEntry(rel, e)
		if err != nil {
			s.add(s.unreadable(rel, err))
			continue
		}
		h, ds := ParseHistorySummary(s.p(rel), data)
		s.add(ds...)
		if h != nil {
			if s.History == nil {
				s.History = map[string]*HistorySummary{}
			}
			s.History[e.Name()] = h
		}
	}
}
