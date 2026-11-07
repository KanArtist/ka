package kaspec

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The canonical YAML writer. Output is a strict subset of YAML 1.2 that any
// YAML parser reads, and that is byte-for-byte deterministic for a given
// value, which is what keeps diffs minimal and merges clean.
//
// Value model:
//   string  - a string; emitted plain when that is unambiguous, else double-quoted
//   Raw     - a typed scalar (integer, boolean, date, timestamp) emitted verbatim
//   []any   - a block sequence (items are string, Raw or []KV)
//   []KV    - a block mapping, keys in the given order
//   nil, empty []any, empty []KV - omitted entirely

// KV is one key/value pair of an ordered mapping.
type KV struct {
	K string
	V any
}

// Raw is a scalar emitted verbatim (the caller guarantees it is a valid typed literal).
type Raw string

// EmitYAML renders an ordered mapping as canonical YAML.
func EmitYAML(m []KV) string {
	var b strings.Builder
	emitMap(&b, m, 0)
	return b.String()
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case []KV:
		return len(t) == 0
	case []string:
		return len(t) == 0
	}
	return false
}

func emitMap(b *strings.Builder, m []KV, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, kv := range m {
		if isEmpty(kv.V) {
			continue
		}
		key := kv.K
		if !safePlain(key) {
			key = quote(key)
		}
		switch v := kv.V.(type) {
		case string:
			fmt.Fprintf(b, "%s%s: %s\n", pad, key, scalar(v))
		case Raw:
			fmt.Fprintf(b, "%s%s: %s\n", pad, key, string(v))
		case []KV:
			fmt.Fprintf(b, "%s%s:\n", pad, key)
			emitMap(b, v, indent+2)
		case []string:
			fmt.Fprintf(b, "%s%s:\n", pad, key)
			for _, s := range v {
				fmt.Fprintf(b, "%s  - %s\n", pad, scalar(s))
			}
		case []any:
			fmt.Fprintf(b, "%s%s:\n", pad, key)
			emitSeq(b, v, indent+2)
		default:
			panic(fmt.Sprintf("kaspec: cannot emit %T", kv.V))
		}
	}
}

func emitSeq(b *strings.Builder, items []any, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, it := range items {
		switch v := it.(type) {
		case string:
			fmt.Fprintf(b, "%s- %s\n", pad, scalar(v))
		case Raw:
			fmt.Fprintf(b, "%s- %s\n", pad, string(v))
		case []KV:
			var sub strings.Builder
			emitMap(&sub, v, indent+2)
			s := sub.String()
			// replace the first line's indentation with "- "
			s = pad + "- " + strings.TrimPrefix(s, pad+"  ")
			b.WriteString(s)
		default:
			panic(fmt.Sprintf("kaspec: cannot emit sequence item %T", it))
		}
	}
}

func scalar(s string) string {
	if safePlain(s) {
		return s
	}
	return quote(s)
}

var (
	reserved = map[string]bool{
		"true": true, "false": true, "yes": true, "no": true, "on": true, "off": true,
		"y": true, "n": true, "null": true, "~": true,
	}
	// numberLike matches every YAML 1.1 and 1.2 number form (integers with
	// any base or underscores, floats, sexagesimals, .inf, .nan), lower-cased.
	numberLike = regexp.MustCompile(`^[-+]?(0b[01_]+|0o?[0-7_]+|0x[0-9a-f_]+|[0-9][0-9_]*(:[0-5]?[0-9])+(\.[0-9_]*)?|(\.[0-9_]+|[0-9][0-9_]*(\.[0-9_]*)?)(e[-+]?[0-9]+)?|\.(inf|nan))$`)
	// dateLike matches the start of a YAML 1.1 timestamp.
	dateLike = regexp.MustCompile(`^[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}`)
)

// safePlain reports whether s can be written as a plain scalar and read back
// as the same string by any YAML 1.1 or 1.2 parser.
func safePlain(s string) bool {
	if s == "" || reserved[strings.ToLower(s)] {
		return false
	}
	if s != strings.TrimSpace(s) {
		return false
	}
	if strings.ContainsAny(s[:1], "-?:,[]{}#&*!|>'\"%@`") {
		return false
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return false
	}
	if numberLike.MatchString(strings.ToLower(s)) || dateLike.MatchString(s) {
		return false
	}
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' || unicode.IsControl(r) || r == '\uFEFF' || r == '\u2028' || r == '\u2029' || r == '\u0085' {
			return false
		}
	}
	return true
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if unicode.IsControl(r) || r == '\uFEFF' || r == '\u2028' || r == '\u2029' || r == '\u0085' {
				if r <= 0xFF {
					fmt.Fprintf(&b, `\x%02X`, r)
				} else {
					fmt.Fprintf(&b, `\u%04X`, r)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
