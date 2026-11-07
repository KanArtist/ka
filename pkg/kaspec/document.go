package kaspec

import (
	"bytes"
	"strings"
)

// rawDoc is a markdown document split into frontmatter and body.
type rawDoc struct {
	fm      *Node
	fmLine  int // line number of the first frontmatter line
	body    string
	diags   []Diagnostic
	encoded bool // false when crlf/bom were found (canonical comparison is skipped)
}

// splitDocument checks encoding, splits frontmatter and parses it.
// It returns nil fm when the document is unusable.
func splitDocument(path string, data []byte) rawDoc {
	d := rawDoc{encoded: true}
	if bytes.HasPrefix(data, []byte("\xEF\xBB\xBF")) {
		d.diags = append(d.diags, diag("bom", path, 1, "save the file as UTF-8 without a byte-order mark (ka fmt fixes this)", "file starts with a UTF-8 byte-order mark"))
		data = data[3:]
		d.encoded = false
	}
	if bytes.IndexByte(data, '\r') >= 0 {
		d.diags = append(d.diags, diag("crlf", path, 0, "use LF line endings; set `* text=auto eol=lf` in .gitattributes (ka fmt fixes this)", "file contains CR characters (CRLF line endings)"))
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
		d.encoded = false
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		d.diags = append(d.diags, diag("frontmatter_missing", path, 1, "start the file with a `---` line, the YAML frontmatter, and a closing `---` line", "file has no YAML frontmatter"))
		return d
	}
	rest := text[4:]
	var fmText string
	switch {
	case strings.HasPrefix(rest, "---\n") || rest == "---":
		fmText, rest = "", strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n")
	default:
		i := strings.Index(rest, "\n---\n")
		if i < 0 {
			if strings.HasSuffix(rest, "\n---") {
				i = len(rest) - 4
			} else {
				d.diags = append(d.diags, diag("frontmatter_missing", path, 1, "close the frontmatter with a `---` line", "frontmatter is not closed"))
				return d
			}
		}
		fmText = rest[:i+1]
		rest = rest[i+1:]
		rest = strings.TrimPrefix(rest, "---")
		rest = strings.TrimPrefix(rest, "\n")
	}
	fm, err := ParseYAML([]byte(fmText))
	if err != nil {
		ye := err.(*YAMLError)
		line := ye.Line
		if line > 0 {
			line++ // account for the opening fence
		}
		d.diags = append(d.diags, diag(ye.Code, path, line, "", "%s", ye.Msg))
		return d
	}
	if fm.Kind != MapNode {
		d.diags = append(d.diags, diag("yaml_syntax", path, 2, "", "frontmatter must be a YAML mapping"))
		return d
	}
	shiftLines(fm, 1)
	d.fm = fm
	d.fmLine = 2
	d.body = rest
	return d
}

// shiftLines offsets every line number in a node tree (frontmatter starts after the fence).
func shiftLines(n *Node, by int) {
	if n == nil {
		return
	}
	if n.Line > 0 {
		n.Line += by
	}
	for k, l := range n.KeyLines {
		n.KeyLines[k] = l + by
	}
	for _, v := range n.Map {
		shiftLines(v, by)
	}
	for _, v := range n.Items {
		shiftLines(v, by)
	}
}

// canonicalBody trims leading blank lines and trailing whitespace.
func canonicalBody(body string) string {
	body = strings.TrimLeft(body, "\n")
	return strings.TrimRight(body, " \t\n")
}

// renderDocument joins frontmatter and body canonically.
func renderDocument(fm []KV, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(EmitYAML(fm))
	b.WriteString("---\n")
	if body = canonicalBody(body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// NormalizeEncoding strips a BOM and converts CRLF/CR to LF.
func NormalizeEncoding(data []byte) []byte {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
}
