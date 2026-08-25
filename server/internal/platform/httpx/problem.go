// Package httpx is the HTTP edge every module facade shares: RFC 9457 problem
// responses (FR-58), opaque cursor pagination (FR-59), and Idempotency-Key
// replay (FR-57).
//
// It exists so those three requirements are satisfied in exactly one place. A
// per-module error writer drifts — one handler forgets traceId, another invents
// a code spelling — and the client's error mapping degrades into a pile of
// special cases. §4.4 requires the client to switch on a stable machine-readable
// code, which is only possible if the server emits one consistently.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// problemBase is the URI namespace for the `type` member. RFC 9457 wants a URI
// that identifies the problem type; ours are documentation anchors rather than
// dereferenceable endpoints, which the RFC explicitly permits.
const problemBase = "https://vybe.app/problems/"

// ProblemDetails is the RFC 9457 `application/problem+json` body required by
// FR-58 on every non-2xx response.
//
// The member set is fixed by the spec's §7 contract. `Code` is the one clients
// are expected to branch on: `Type` is a URI (verbose, and tempting to compare
// with string prefixes), `Title` is human-readable (and therefore free to be
// reworded), and `Status` is too coarse — five different 409s are all "409".
type ProblemDetails struct {
	Type    string       `json:"type"`
	Title   string       `json:"title"`
	Status  int          `json:"status"`
	Detail  string       `json:"detail,omitempty"`
	Code    string       `json:"code"`
	TraceID string       `json:"traceId"`
	Errors  []FieldError `json:"errors,omitempty"`
}

// FieldError is one entry in FR-58's `errors[]`, used for field validation.
//
// `Detail` is a diagnostic for the developer reading logs, NOT display copy:
// FR-61 requires every user-facing string to come from an .arb file, so the
// client renders from `Code` and never prints this text.
type FieldError struct {
	Field  string `json:"field"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Problem is an error that knows how it should be rendered over HTTP.
//
// Handlers return it as a plain `error`, so a problem can travel up through
// call sites that know nothing about HTTP without being unwrapped and rebuilt.
type Problem struct {
	Status int
	Code   string
	Title  string
	Detail string
	Errors []FieldError

	// Extra carries problem-specific members. RFC 9457 §3.2 allows extensions,
	// and some of ours are load-bearing: a 429 carries retryAfterSeconds, and
	// AC-23 asserts the client can honour it.
	Extra map[string]any

	// cause is preserved for logs and never serialised. Leaking an internal
	// error string to a client is how database schema and file paths escape.
	cause error
}

func (p *Problem) Error() string {
	if p.cause != nil {
		return fmt.Sprintf("%s (%d): %s: %v", p.Code, p.Status, p.Detail, p.cause)
	}
	return fmt.Sprintf("%s (%d): %s", p.Code, p.Status, p.Detail)
}

// Unwrap exposes the cause to errors.Is/As without exposing it to clients.
func (p *Problem) Unwrap() error { return p.cause }

// WithCause attaches an internal error for logging. The returned Problem
// serialises identically — the cause never reaches the response body.
func (p *Problem) WithCause(err error) *Problem {
	clone := *p
	clone.cause = err
	return &clone
}

// WithDetail replaces the instance-specific detail string.
func (p *Problem) WithDetail(format string, args ...any) *Problem {
	clone := *p
	clone.Detail = fmt.Sprintf(format, args...)
	return &clone
}

// WithExtra adds an RFC 9457 extension member.
func (p *Problem) WithExtra(key string, value any) *Problem {
	clone := *p
	clone.Extra = make(map[string]any, len(p.Extra)+1)
	for k, v := range p.Extra {
		clone.Extra[k] = v
	}
	clone.Extra[key] = value
	return &clone
}

// WithFieldErrors attaches FR-58's `errors[]` for field-level validation.
func (p *Problem) WithFieldErrors(errs ...FieldError) *Problem {
	clone := *p
	clone.Errors = append(append([]FieldError(nil), p.Errors...), errs...)
	return &clone
}

// NewProblem builds a problem with a stable code.
//
// `code` is SCREAMING_SNAKE_CASE and is part of the client contract: renaming
// one is a breaking API change, which is why the vocabulary below is declared
// in one list rather than being spelled inline at call sites.
func NewProblem(status int, code, title, detail string) *Problem {
	return &Problem{Status: status, Code: code, Title: title, Detail: detail}
}

// The stable problem vocabulary. Every code the M1 surface can emit is here so
// the set can be diffed in review and mirrored in the client's Failure mapping
// (§4.4) and in api/openapi.yaml.
var (
	ErrBadRequest   = NewProblem(http.StatusBadRequest, "BAD_REQUEST", "Bad Request", "The request could not be understood.")
	ErrValidation   = NewProblem(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "One or more fields are invalid.")
	ErrUnauthorized = NewProblem(http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized", "Authentication is required.")
	ErrTokenExpired = NewProblem(http.StatusUnauthorized, "TOKEN_EXPIRED", "Token Expired", "The access token has expired.")
	ErrForbidden    = NewProblem(http.StatusForbidden, "FORBIDDEN", "Forbidden", "You do not have access to this resource.")
	ErrNotFound     = NewProblem(http.StatusNotFound, "NOT_FOUND", "Not Found", "No such resource.")
	ErrConflict     = NewProblem(http.StatusConflict, "CONFLICT", "Conflict", "The request conflicts with the current state.")
	ErrRateLimited  = NewProblem(http.StatusTooManyRequests, "RATE_LIMITED", "Too Many Requests", "Slow down.")
	ErrInternal     = NewProblem(http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "Something went wrong on our side.")
	ErrUnavailable  = NewProblem(http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable", "The service is temporarily unavailable.")

	// FR-57 — Idempotency-Key.
	ErrIdempotencyKeyRequired = NewProblem(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key Required",
		"This endpoint mutates state and requires an Idempotency-Key header.")
	ErrIdempotencyKeyInvalid = NewProblem(http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key Invalid",
		"The Idempotency-Key must be 8 to 255 printable ASCII characters.")
	ErrIdempotencyKeyReused = NewProblem(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key Reused",
		"This Idempotency-Key was already used with a different request body.")
	ErrIdempotencyInFlight = NewProblem(http.StatusConflict, "IDEMPOTENCY_IN_FLIGHT", "Request In Flight",
		"A request with this Idempotency-Key is still being processed. Retry shortly.")

	// FR-59 — pagination.
	ErrOffsetPagination = NewProblem(http.StatusBadRequest, "OFFSET_PAGINATION_UNSUPPORTED", "Offset Pagination Unsupported",
		"This API uses opaque cursor pagination. Use ?cursor= with the value from the previous page.")
	ErrCursorInvalid = NewProblem(http.StatusBadRequest, "CURSOR_INVALID", "Cursor Invalid",
		"The cursor is malformed. Cursors are opaque and must be echoed back unmodified.")
	ErrLimitInvalid = NewProblem(http.StatusBadRequest, "LIMIT_INVALID", "Limit Invalid",
		"The limit must be a positive integer within the documented maximum.")
)

// WriteProblem renders a problem as RFC 9457 `application/problem+json`.
//
// It takes the trace id from the request context so FR-58's traceId is never
// forgotten at a call site, and logs 5xx with the internal cause attached —
// the one place where the cause is allowed to be seen.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	p := AsProblem(err)
	traceID := TraceIDFromContext(r.Context())

	if p.Status >= http.StatusInternalServerError {
		// The client gets a generic detail; the log gets the truth.
		slog.ErrorContext(r.Context(), "request failed",
			"code", p.Code, "status", p.Status, "trace_id", traceID,
			"method", r.Method, "path", r.URL.Path, "err", p.Error())
	}

	body := ProblemDetails{
		Type:    problemBase + slugForCode(p.Code),
		Title:   p.Title,
		Status:  p.Status,
		Detail:  p.Detail,
		Code:    p.Code,
		TraceID: traceID,
		Errors:  p.Errors,
	}

	// Extension members are merged at the top level, per RFC 9457 §3.2. This
	// needs a map rather than the struct, so encode the struct first and fold
	// the extras in.
	payload := mergeExtras(body, p.Extra)

	w.Header().Set("Content-Type", "application/problem+json")
	// RFC 9110: a 429 or 503 should say when to come back. AC-23 depends on it.
	if secs, ok := p.Extra["retryAfterSeconds"].(int); ok && secs > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
	}
	w.WriteHeader(p.Status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already written, so this cannot be turned into a
		// different response. Log it and move on.
		slog.ErrorContext(r.Context(), "encoding problem response failed",
			"trace_id", traceID, "err", err)
	}
}

// AsProblem coerces any error into a *Problem.
//
// An unrecognised error becomes a generic 500 with its cause preserved for the
// log and stripped from the body. That default matters: it means a handler
// that returns a raw database error cannot accidentally leak it.
func AsProblem(err error) *Problem {
	if err == nil {
		return ErrInternal
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return ErrInternal.WithCause(err)
}

func mergeExtras(body ProblemDetails, extra map[string]any) any {
	if len(extra) == 0 {
		return body
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return body
	}
	merged := map[string]any{}
	if err := json.Unmarshal(raw, &merged); err != nil {
		return body
	}
	for k, v := range extra {
		// Never let an extension shadow a required member: FR-58's contract
		// is that these seven always mean the same thing.
		switch k {
		case "type", "title", "status", "detail", "code", "traceId", "errors":
			continue
		}
		merged[k] = v
	}
	return merged
}

// slugForCode turns SCREAMING_SNAKE_CASE into the kebab-case tail of the type
// URI, e.g. DUPLICATE_ANSWER -> duplicate-answer, matching the §7 example.
func slugForCode(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c == '_':
			out = append(out, '-')
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
