package api

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/KanArtist/ka/internal/engine"
)

// Problem is an RFC 9457 problem document carrying the engine's error code
// and fix, so REST clients see the same errors as MCP and the CLI.
type Problem struct {
	Type   string              `json:"type,omitempty" doc:"Always about:blank; use code."`
	Status int                 `json:"status" doc:"The HTTP status."`
	Title  string              `json:"title" doc:"The HTTP status text."`
	Code   string              `json:"code" doc:"The ka error code, the same as in MCP and the CLI (not_found, claim_lost, unknown_status, ...)."`
	Detail string              `json:"detail,omitempty" doc:"What went wrong."`
	Fix    string              `json:"fix,omitempty" doc:"What to do about it."`
	Data   map[string]any      `json:"data,omitempty" doc:"Structured details, such as the index of the failing item in a batch."`
	Errors []*huma.ErrorDetail `json:"errors,omitempty" doc:"Request validation failures."`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Detail }

// GetStatus implements huma.StatusError.
func (p *Problem) GetStatus() int { return p.Status }

// ContentType implements huma.ContentTypeFilter.
func (p *Problem) ContentType(ct string) string {
	if ct == "application/json" {
		return "application/problem+json"
	}
	return ct
}

// statusFor maps engine error codes to HTTP statuses.
func statusFor(code string) int {
	switch code {
	case engine.CodeNotFound, engine.CodeUnknownProject, engine.CodeNoCandidates, engine.CodeNoQuestion:
		return http.StatusNotFound
	case engine.CodeClaimLost, engine.CodeNotClaimed, engine.CodeNotYourClaim, engine.CodeMoved, engine.CodeIDTaken:
		return http.StatusConflict
	case engine.CodeConflict:
		return http.StatusPreconditionFailed
	case engine.CodeThrottled:
		return http.StatusTooManyRequests
	case engine.CodeReadOnly, codeForbidden:
		return http.StatusForbidden
	case codeUnauthorized:
		return http.StatusUnauthorized
	case engine.CodeGitBusy:
		return http.StatusServiceUnavailable
	case engine.CodeGit:
		return http.StatusBadGateway
	case codePreconditionRequired:
		return http.StatusPreconditionRequired
	case "internal":
		return http.StatusInternalServerError
	}
	return http.StatusUnprocessableEntity
}

// codeFor maps HTTP statuses (from Huma's own validation and routing) to
// engine-style codes.
func codeFor(status int) string {
	switch status {
	case http.StatusNotFound:
		return engine.CodeNotFound
	case http.StatusUnauthorized:
		return codeUnauthorized
	case http.StatusForbidden:
		return codeForbidden
	case http.StatusPreconditionFailed:
		return engine.CodeConflict
	case http.StatusPreconditionRequired:
		return codePreconditionRequired
	case http.StatusInternalServerError:
		return "internal"
	}
	if status >= 400 && status < 500 {
		return engine.CodeInvalid
	}
	return "internal"
}

// REST-only codes.
const (
	codeUnauthorized         = "unauthorized"
	codeForbidden            = "forbidden"
	codePreconditionRequired = "if_match_required"
)

func init() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		p := &Problem{Status: status, Title: http.StatusText(status), Code: codeFor(status), Detail: msg}
		for _, e := range errs {
			if e == nil {
				continue
			}
			var d huma.ErrorDetailer
			if errors.As(e, &d) {
				p.Errors = append(p.Errors, d.ErrorDetail())
			} else {
				p.Errors = append(p.Errors, &huma.ErrorDetail{Message: e.Error()})
			}
		}
		return p
	}
}

// problem converts an engine error into a Problem.
func problem(err error) error {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	var se huma.StatusError
	if errors.As(err, &se) {
		return se
	}
	e := engine.AsError(err)
	st := statusFor(e.Code)
	return &Problem{Status: st, Title: http.StatusText(st), Code: e.Code, Detail: e.Message, Fix: e.Fix, Data: e.Data}
}

func newProblem(code, fix, detail string) *Problem {
	st := statusFor(code)
	return &Problem{Status: st, Title: http.StatusText(st), Code: code, Detail: detail, Fix: fix}
}
