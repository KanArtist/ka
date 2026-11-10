package kaspec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Slug limits (spec §4.3).
const (
	MaxSlugLen   = 24
	MaxSlugWords = 4
)

// SlugRule describes a valid slug, for error messages.
const SlugRule = "a slug is 1-4 lower-case words joined by hyphens, at most 24 characters, e.g. compiler-warnings"

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// CheckSlug reports why s is not a valid slug, or nil.
func CheckSlug(s string) error {
	switch {
	case !slugRe.MatchString(s):
		return fmt.Errorf("%q is not lower-case words joined by single hyphens", s)
	case len(s) > MaxSlugLen:
		return fmt.Errorf("%q is %d characters; the limit is %d", s, len(s), MaxSlugLen)
	case strings.Count(s, "-")+1 > MaxSlugWords:
		return fmt.Errorf("%q has %d words; the limit is %d", s, strings.Count(s, "-")+1, MaxSlugWords)
	}
	return nil
}

// ErrEmptySlug is returned by NormalizeSlug for input with no letters or digits.
var ErrEmptySlug = errors.New("the slug has no letters or digits")

// NormalizeSlug turns what a person or agent typed ("Compiler warnings",
// "compiler_warnings") into slug form, then checks the limits. It never
// shortens: an over-long slug is an error, so the caller picks a better one.
func NormalizeSlug(s string) (string, error) {
	w := slugWords(s)
	if len(w) == 0 {
		return "", ErrEmptySlug
	}
	out := strings.Join(w, "-")
	return out, CheckSlug(out)
}

// slugStop are words a derived slug leaves out.
var slugStop = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "with": true, "from": true, "by": true, "at": true,
	"is": true, "are": true, "be": true, "as": true, "it": true, "its": true, "into": true,
	"via": true, "when": true, "that": true, "this": true, "should": true, "must": true,
	"epic": true, // epics say so in their kind or labels
}

// DeriveSlug builds a slug from a title: up to three significant words,
// within the length limit. It returns "" when the title has none.
func DeriveSlug(title string) string {
	all := slugWords(title)
	var words []string
	for _, w := range all {
		if !slugStop[w] {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		words = all
	}
	var out string
	for i, w := range words {
		if i == 3 {
			break
		}
		next := w
		if out != "" {
			next = out + "-" + w
		}
		if len(next) > MaxSlugLen {
			if out == "" {
				out = strings.TrimRight(w[:MaxSlugLen], "-")
			}
			break
		}
		out = next
	}
	return out
}

// fold maps common accented Latin letters to ASCII.
var fold = func() map[rune]string {
	m := map[rune]string{'ß': "ss", 'æ': "ae", 'œ': "oe", 'ø': "o", 'đ': "d", 'ł': "l", 'þ': "th"}
	for base, accented := range map[string]string{
		"a": "àáâãäåāăą", "c": "çćĉċč", "e": "èéêëēĕėęě", "g": "ĝğġģ", "i": "ìíîïĩīĭįı",
		"n": "ñńņňŉ", "o": "òóôõöōŏő", "s": "śŝşšș", "t": "ţťț", "u": "ùúûüũūŭůűų",
		"y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range accented {
			m[r] = base
		}
	}
	return m
}()

// slugWords splits s into lower-case ASCII words, folding common accents
// (café → cafe) and dropping other characters.
func slugWords(s string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		r = unicode.ToLower(r)
		switch {
		case fold[r] != "":
			cur.WriteString(fold[r])
		case r == '\'' || r == '’':
			// don't → dont
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return words
}

// DisplaySlug is the slug shown for a ticket: the stored one, else one
// derived from the title.
func DisplaySlug(t *Ticket) string {
	if t == nil {
		return ""
	}
	if t.Slug != "" {
		return t.Slug
	}
	return DeriveSlug(t.Title)
}

// Ref is a ticket's reference form: ID-slug (JK-7QK3M-compiler-warnings),
// or the bare ID when there is no slug.
func Ref(id, slug string) string {
	if slug == "" {
		return id
	}
	return id + "-" + slug
}

var refRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{0,9})-([0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{5,8})(-[A-Za-z0-9-]*)?$`)

// RefID returns the ticket ID a reference names, upper-cased, with any slug
// removed: "jk-7qk3m-compiler-warnings" is JK-7QK3M. Lookups ignore the
// slug, so a stale one still resolves. Input that is not a reference comes
// back trimmed and upper-cased.
func RefID(s string) string {
	s = strings.TrimSpace(s)
	if m := refRe.FindStringSubmatch(s); m != nil {
		return strings.ToUpper(m[1] + "-" + m[2])
	}
	return strings.ToUpper(s)
}
