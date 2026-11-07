package kaspec

import (
	"crypto/rand"
	"crypto/sha256"
	"regexp"
	"strings"
)

// Alphabet is Crockford base32: digits and upper-case letters without I, L, O, U.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// DefaultSuffixLength is the default length of the random part of a ticket ID.
const DefaultSuffixLength = 5

var (
	// IDRe matches a ticket ID: PREFIX-SUFFIX.
	IDRe = regexp.MustCompile(`^([A-Z][A-Z0-9]{0,9})-([0-9A-HJKMNP-TV-Z]{5,8})$`)
	// PrefixRe matches a project ID prefix.
	PrefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)
	// ProjectKeyRe matches a project key (also its directory name).
	ProjectKeyRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// ActorRe matches an actor handle: a human, agent/<name> or role/<name>.
	ActorRe = regexp.MustCompile(`^((agent|role)/)?[a-z0-9][a-z0-9._-]{0,63}$`)
	// HumanRe matches a human actor handle.
	HumanRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// AgentRe matches an agent actor handle.
	AgentRe = regexp.MustCompile(`^agent/[a-z0-9][a-z0-9._-]{0,63}$`)
	// TimestampRe matches the canonical timestamp form: RFC 3339, UTC, whole seconds.
	TimestampRe = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])T([01]\d|2[0-3]):[0-5]\d:[0-5]\dZ$`)
	// DateRe matches a calendar date.
	DateRe = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$`)
	// CommentFileRe matches a comment filename: <yyyymmddThhmmssZ>-<4 base32>.md
	CommentFileRe = regexp.MustCompile(`^(\d{8}T\d{6}Z)-([0-9A-HJKMNP-TV-Z]{4})\.md$`)
	// LinkRe matches a symbolic code link: repo://path[@ref][#Lstart[-end]].
	LinkRe = regexp.MustCompile(`^([a-z][a-z0-9-]{0,31})://([^@#\s]+)(@[^#\s]+)?(#L[0-9]+(-L?[0-9]+)?)?$`)
	urlRe  = regexp.MustCompile(`^https?://\S+$`)
)

// ParseID splits a ticket ID into prefix and suffix.
func ParseID(id string) (prefix, suffix string, ok bool) {
	m := IDRe.FindStringSubmatch(id)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// NewID returns a fresh random ID with the given prefix and suffix length.
func NewID(prefix string, n int) string {
	return prefix + "-" + RandomBase32(n)
}

// RandomBase32 returns n cryptographically random Crockford base32 characters.
func RandomBase32(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	out := make([]byte, n)
	for i, c := range buf {
		out[i] = Alphabet[int(c)%32] // 256 is a multiple of 32: no modulo bias
	}
	return string(out)
}

// HashBase32 derives n base32 characters deterministically from a seed.
// Used by importers that must assign the same ID on every run.
func HashBase32(seed string, n int) string {
	sum := sha256.Sum256([]byte(seed))
	out := make([]byte, n)
	for i := range n {
		out[i] = Alphabet[int(sum[i])%32]
	}
	return string(out)
}

// LowEntropy flags suffixes that look hand-made rather than random: fewer
// than three distinct characters, or a straight run through the alphabet.
func LowEntropy(suffix string) bool {
	distinct := map[rune]bool{}
	for _, r := range suffix {
		distinct[r] = true
	}
	if len(distinct) < 3 {
		return true
	}
	asc, desc := true, true
	for i := 1; i < len(suffix); i++ {
		d := strings.IndexByte(Alphabet, suffix[i]) - strings.IndexByte(Alphabet, suffix[i-1])
		asc = asc && d == 1
		desc = desc && d == -1
	}
	return asc || desc
}

// DefaultPrefix derives a project's ID prefix from its key when project.yml omits one.
func DefaultPrefix(key string) string {
	p := strings.ToUpper(strings.ReplaceAll(key, "-", ""))
	if len(p) > 10 {
		p = p[:10]
	}
	return p
}
