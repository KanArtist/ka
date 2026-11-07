package engine

import (
	"errors"
	"fmt"
)

// Error is a structured, actionable error. The same codes are used by the CLI,
// MCP and (from M2) REST, so an agent learns them once.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Fix     string         `json:"fix,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Message
	if e.Fix != "" {
		s += " (fix: " + e.Fix + ")"
	}
	return s
}

// Errf builds an Error.
func Errf(code, fix, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Fix: fix}
}

// With attaches data to an error.
func (e *Error) With(k string, v any) *Error {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data[k] = v
	return e
}

// AsError converts any error into an *Error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: "internal", Message: err.Error()}
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Error codes.
const (
	CodeNotFound           = "not_found"
	CodeInvalid            = "invalid_argument"
	CodeUnknownStatus      = "unknown_status"
	CodeUnknownKind        = "unknown_kind"
	CodeUnknownLabel       = "unknown_label"
	CodeUnknownField       = "unknown_field"
	CodeInvalidField       = "invalid_field"
	CodeUnknownProject     = "unknown_project"
	CodeClaimLost          = "claim_lost"
	CodeNotClaimed         = "not_claimed"
	CodeNotYourClaim       = "not_your_claim"
	CodeResolutionRequired = "resolution_required"
	CodeThrottled          = "progress_throttled"
	CodeMoved              = "ticket_moved"
	CodeIDTaken            = "id_taken"
	CodeReadOnly           = "read_only_store"
	CodeDanglingRef        = "dangling_ref"
	CodeCycle              = "cycle"
	CodeNoCandidates       = "no_candidates"
	CodeGitBusy            = "git_busy"
	CodeGit                = "git_error"
	CodeNoQuestion         = "no_open_question"
	CodeConflict           = "edit_conflict"
)
