// Package kaspec implements the KanArtist Store Spec v1: parsing, canonical
// formatting, config layering and validation of store contents.
//
// The spec itself lives in spec/v1/README.md. This package is one
// implementation of it; the conformance corpus in spec/v1/conformance is the
// arbiter when the two disagree.
package kaspec

import (
	"fmt"
	"sort"
)

// SpecVersion is the major Store Spec version this package reads and writes.
const SpecVersion = 1

// Severity of a diagnostic.
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
	SevInfo    Severity = "info"
)

// Diagnostic is one finding from parsing or checking a store.
type Diagnostic struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`           // slash-separated, relative to the store root (or workspace label)
	Line     int      `json:"line,omitempty"` // 1-based; 0 when not applicable
	Message  string   `json:"message"`
	Fix      string   `json:"fix,omitempty"` // a one-line hint on how to fix it
}

func (d Diagnostic) String() string {
	loc := d.Path
	if d.Line > 0 {
		loc = fmt.Sprintf("%s:%d", d.Path, d.Line)
	}
	s := fmt.Sprintf("%s: %s %s: %s", loc, d.Severity, d.Code, d.Message)
	if d.Fix != "" {
		s += " (fix: " + d.Fix + ")"
	}
	return s
}

// Codes is the registry of diagnostic codes and their severities. It is the
// single source of truth; spec/v1/README.md restates it and a test fails if
// the two disagree.
var Codes = map[string]Severity{
	// file encoding and syntax
	"crlf":                SevError,
	"bom":                 SevError,
	"frontmatter_missing": SevError,
	"yaml_syntax":         SevError,
	"yaml_feature":        SevError,
	"duplicate_key":       SevError,
	"non_canonical":       SevWarning,
	// fields
	"unknown_key":         SevError,
	"missing_field":       SevError,
	"invalid_value":       SevError,
	"unknown_status":      SevError,
	"unknown_kind":        SevError,
	"unknown_label":       SevError,
	"unknown_field":       SevError,
	"invalid_field_value": SevError,
	"placeholder_text":    SevWarning,
	"low_entropy_id":      SevWarning,
	"unknown_repo":        SevWarning,
	// identity and layout
	"invalid_store":         SevError,
	"spec_unsupported":      SevError,
	"project_key_mismatch":  SevError,
	"project_missing_file":  SevError,
	"ticket_missing_file":   SevError,
	"id_dir_mismatch":       SevError,
	"id_prefix_mismatch":    SevError,
	"duplicate_project":     SevError,
	"duplicate_prefix":      SevError,
	"duplicate_id":          SevError,
	"stray_file":            SevWarning,
	"comment_filename":      SevError,
	"comment_time_mismatch": SevWarning,
	// relations
	"dangling_ref":    SevError,
	"external_ref":    SevInfo,
	"self_ref":        SevError,
	"cycle":           SevError,
	"answer_dangling": SevError,
	// operational
	"stale_claim":       SevInfo,
	"archived_project":  SevInfo,
	"secret_suspect":    SevWarning,
	"comment_too_large": SevWarning,
	"file_too_large":    SevWarning,
	// config layering
	"config_invalid":            SevError,
	"config_locked":             SevError,
	"config_cycle":              SevError,
	"config_parent_unavailable": SevError,
}

func diag(code, path string, line int, fix, format string, args ...any) Diagnostic {
	sev, ok := Codes[code]
	if !ok {
		panic("kaspec: unregistered diagnostic code " + code)
	}
	return Diagnostic{Code: code, Severity: sev, Path: path, Line: line, Message: fmt.Sprintf(format, args...), Fix: fix}
}

// SortDiagnostics orders diagnostics deterministically (path, line, code, message).
func SortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
}

// HasErrors reports whether any diagnostic is an error (or, when strict, a warning).
func HasErrors(ds []Diagnostic, strict bool) bool {
	for _, d := range ds {
		if d.Severity == SevError || (strict && d.Severity == SevWarning) {
			return true
		}
	}
	return false
}
