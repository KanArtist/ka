package kaspec

import (
	"slices"
	"strings"
)

// CommentTypes is the fixed comment type vocabulary.
var CommentTypes = []string{"note", "progress", "question", "answer", "decision", "resolution", "approval", "system"}

// Resolutions is the fixed resolution vocabulary.
var Resolutions = []string{"done", "canceled", "duplicate", "wontfix"}

// CommentKeys is the canonical frontmatter key order of a comment.
var CommentKeys = []string{
	"type", "author", "on_behalf_of", "session", "created", "to",
	"answers", "choice", "approved", "resolution", "supersedes", "retracts", "options",
}

// Comment is one append-only comment file.
type Comment struct {
	Type, Author, OnBehalfOf, Session, Created, To string
	Answers                                        string
	Choice                                         *int64
	Approved                                       *bool
	Resolution, Supersedes, Retracts               string
	Options                                        []string
	Body                                           string
	Lines                                          map[string]int
}

// ParseComment parses a comment file.
func ParseComment(path string, data []byte) (*Comment, []Diagnostic) {
	d := splitDocument(path, data)
	if d.fm == nil {
		return nil, d.diags
	}
	r := newFieldReader(path, d.fm)
	c := &Comment{Lines: d.fm.KeyLines, Body: d.body}
	c.Type = r.str("type")
	if c.Type != "" && !slices.Contains(CommentTypes, c.Type) {
		r.bad("type", "use one of "+strings.Join(CommentTypes, ", "), "%q is not a comment type", c.Type)
	}
	c.Author = r.strRe("author", ActorRe, "actors are `handle` or `agent/<name>`, lower-case")
	c.OnBehalfOf = r.strRe("on_behalf_of", HumanRe, "on_behalf_of names a human actor")
	c.Session = r.strRe("session", sessionRe, "")
	c.Created = r.strRe("created", TimestampRe, "timestamps are RFC 3339 UTC with seconds, e.g. 2026-11-07T13:07:22Z")
	c.To = r.strRe("to", ActorRe, "")
	c.Answers = r.strRe("answers", CommentFileRe, "answers names another comment's filename in this ticket")
	if n, ok := r.integer("choice"); ok {
		c.Choice = &n
	}
	if b, ok := r.boolean("approved"); ok {
		c.Approved = &b
	}
	c.Resolution = r.str("resolution")
	if c.Resolution != "" && !slices.Contains(Resolutions, c.Resolution) {
		r.bad("resolution", "use one of "+strings.Join(Resolutions, ", "), "%q is not a resolution", c.Resolution)
	}
	c.Supersedes = r.strRe("supersedes", CommentFileRe, "")
	c.Retracts = r.strRe("retracts", CommentFileRe, "")
	c.Options = r.strList("options", nil, "")
	r.unknown(CommentKeys)

	line := d.fmLine
	// presence is judged on the raw node, so an invalid value is reported once
	// (as invalid_value), not twice
	req := func(key, why string) {
		if v := d.fm.Map[key]; v == nil || v.Kind == NullNode || (v.Kind == ScalarNode && v.Value == "") {
			r.diags = append(r.diags, diag("missing_field", path, line, "", "%q is required%s", key, why))
		}
	}
	req("type", "")
	req("author", "")
	req("created", "")
	switch c.Type {
	case "answer":
		req("answers", " on an answer")
	case "approval":
		req("answers", " on an approval")
		req("approved", " on an approval")
	case "resolution":
		req("resolution", " on a resolution")
	}
	misplaced := func(key string, present bool, allowed ...string) {
		if present && !slices.Contains(allowed, c.Type) && c.Type != "" {
			r.diags = append(r.diags, diag("invalid_value", path, c.Lines[key], "", "%s: only allowed on %s comments", key, strings.Join(allowed, "/")))
		}
	}
	misplaced("options", len(c.Options) > 0, "question")
	misplaced("choice", c.Choice != nil, "answer")
	misplaced("approved", c.Approved != nil, "approval")
	misplaced("resolution", c.Resolution != "", "resolution")
	misplaced("answers", c.Answers != "", "answer", "approval")
	if c.Choice != nil && (*c.Choice < 0) {
		r.bad("choice", "", "must be a zero-based option index")
	}

	diags := append(d.diags, r.diags...)
	if !HasErrors(diags, false) && d.encoded {
		if string(FormatComment(c)) != string(data) {
			diags = append(diags, diag("non_canonical", path, 0, "run `ka fmt`", "file is not in canonical form"))
		}
	}
	return c, diags
}

// FormatComment renders a comment in canonical form.
func FormatComment(c *Comment) []byte {
	fm := []KV{
		{"type", c.Type},
		{"author", c.Author},
		{"on_behalf_of", c.OnBehalfOf},
		{"session", c.Session},
		{"created", rawOrEmpty(c.Created)},
		{"to", c.To},
		{"answers", c.Answers},
	}
	if c.Choice != nil {
		fm = append(fm, KV{"choice", Raw(itoa(*c.Choice))})
	}
	if c.Approved != nil {
		fm = append(fm, KV{"approved", Raw(map[bool]string{true: "true", false: "false"}[*c.Approved])})
	}
	fm = append(fm,
		KV{"resolution", c.Resolution},
		KV{"supersedes", c.Supersedes},
		KV{"retracts", c.Retracts},
		KV{"options", toAny(c.Options)},
	)
	return renderDocument(dropEmptyStrings(fm), c.Body)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// CommentFilename builds a comment filename from a canonical timestamp and a 4-character suffix.
func CommentFilename(created, suffix string) string {
	// 2026-11-07T13:07:22Z -> 20261107T130722Z
	s := strings.NewReplacer("-", "", ":", "").Replace(created)
	return s + "-" + suffix + ".md"
}
