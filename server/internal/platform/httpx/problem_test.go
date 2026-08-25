package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeProblem(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, rr.Body.String())
	}
	return got
}

func TestWriteProblemEmitsEveryRequiredMember(t *testing.T) {
	// FR-58 names seven members. A missing one is not cosmetic: the client's
	// §4.4 mapping switches on `code`, and the support path needs `traceId`.
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req = req.WithContext(ContextWithTraceID(req.Context(), "trace-abc"))
	rr := httptest.NewRecorder()

	WriteProblem(rr, req, ErrConflict.WithDetail("Room already ended."))

	if got, want := rr.Code, http.StatusConflict; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := rr.Header().Get("Content-Type"), "application/problem+json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	got := decodeProblem(t, rr)
	for _, member := range []string{"type", "title", "status", "detail", "code", "traceId"} {
		if _, ok := got[member]; !ok {
			t.Errorf("FR-58 member %q missing from %v", member, got)
		}
	}
	if got["code"] != "CONFLICT" {
		t.Errorf("code = %v, want CONFLICT", got["code"])
	}
	if got["traceId"] != "trace-abc" {
		t.Errorf("traceId = %v, want trace-abc; the context value must be used", got["traceId"])
	}
	if got["detail"] != "Room already ended." {
		t.Errorf("detail = %v", got["detail"])
	}
}

func TestProblemTypeURIDerivesFromCode(t *testing.T) {
	// §7's example is "https://vybe.app/problems/duplicate-answer".
	tests := []struct {
		code string
		want string
	}{
		{"DUPLICATE_ANSWER", "https://vybe.app/problems/duplicate-answer"},
		{"NOT_FOUND", "https://vybe.app/problems/not-found"},
		{"ROOM_FULL", "https://vybe.app/problems/room-full"},
		{"INTERNAL", "https://vybe.app/problems/internal"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rr := httptest.NewRecorder()
			WriteProblem(rr, req, NewProblem(http.StatusConflict, tt.code, "t", "d"))
			if got := decodeProblem(t, rr)["type"]; got != tt.want {
				t.Errorf("type = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnknownErrorBecomes500AndDoesNotLeakItsCause(t *testing.T) {
	// The important half is the negative assertion. A handler returning a raw
	// pgx error must not put the schema into the response body.
	secret := errors.New(`pq: relation "users" does not exist (host=db-prod-1)`)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	WriteProblem(rr, req, fmt.Errorf("loading room: %w", secret))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if body := rr.Body.String(); contains(body, "relation") || contains(body, "db-prod-1") {
		t.Errorf("internal cause leaked into the response body: %s", body)
	}
	if got := decodeProblem(t, rr)["code"]; got != "INTERNAL" {
		t.Errorf("code = %v, want INTERNAL", got)
	}
}

func TestAsProblemUnwrapsThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("joining room: %w", fmt.Errorf("checking cap: %w", ErrConflict))
	if got := AsProblem(wrapped); got.Code != "CONFLICT" {
		t.Errorf("code = %q, want CONFLICT — errors.As must see through wrapping", got.Code)
	}
	if got := AsProblem(nil); got.Code != "INTERNAL" {
		t.Errorf("nil error should be INTERNAL, got %q", got.Code)
	}
}

func TestFieldErrorsAreRendered(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rr := httptest.NewRecorder()

	WriteProblem(rr, req, ErrValidation.WithFieldErrors(
		FieldError{Field: "contentId", Code: "REQUIRED", Detail: "must be present"},
		FieldError{Field: "syncMode", Code: "ENUM", Detail: "must be COMPANION, CLIP or ASYNC"},
	))

	got := decodeProblem(t, rr)
	errs, ok := got["errors"].([]any)
	if !ok || len(errs) != 2 {
		t.Fatalf("errors[] = %v, want 2 entries", got["errors"])
	}
	first := errs[0].(map[string]any)
	if first["field"] != "contentId" || first["code"] != "REQUIRED" {
		t.Errorf("first field error = %v", first)
	}
}

func TestExtraMembersAreMergedButCannotShadowRequiredOnes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rr := httptest.NewRecorder()

	p := ErrRateLimited.
		WithExtra("retryAfterSeconds", 30).
		// A caller trying to override a required member must be ignored: FR-58's
		// contract is that those seven always mean the same thing.
		WithExtra("code", "PRETEND_SOMETHING_ELSE").
		WithExtra("status", 200)

	WriteProblem(rr, req, p)

	got := decodeProblem(t, rr)
	if got["retryAfterSeconds"] != float64(30) {
		t.Errorf("retryAfterSeconds = %v, want 30", got["retryAfterSeconds"])
	}
	if got["code"] != "RATE_LIMITED" {
		t.Errorf("code was shadowed by an extension: %v", got["code"])
	}
	if got["status"] != float64(http.StatusTooManyRequests) {
		t.Errorf("status was shadowed by an extension: %v", got["status"])
	}
	// AC-23: the client can only honour a backoff it is told about.
	if got, want := rr.Header().Get("Retry-After"), "30"; got != want {
		t.Errorf("Retry-After header = %q, want %q", got, want)
	}
}

func TestWithersDoNotMutateTheSharedSentinels(t *testing.T) {
	// The vocabulary is package-level state. If WithDetail mutated in place,
	// one handler's message would leak into every later use of that code —
	// across goroutines, which is also a data race.
	before := *ErrNotFound

	_ = ErrNotFound.WithDetail("no such room %q", "ABC123")
	_ = ErrNotFound.WithCause(errors.New("boom"))
	_ = ErrNotFound.WithExtra("k", "v")
	_ = ErrNotFound.WithFieldErrors(FieldError{Field: "f"})

	if ErrNotFound.Detail != before.Detail {
		t.Errorf("WithDetail mutated the sentinel: %q -> %q", before.Detail, ErrNotFound.Detail)
	}
	if ErrNotFound.Extra != nil {
		t.Errorf("WithExtra mutated the sentinel: %v", ErrNotFound.Extra)
	}
	if len(ErrNotFound.Errors) != 0 {
		t.Errorf("WithFieldErrors mutated the sentinel: %v", ErrNotFound.Errors)
	}
	if ErrNotFound.Unwrap() != nil {
		t.Errorf("WithCause mutated the sentinel")
	}
}

func TestProblemErrorStringIncludesCause(t *testing.T) {
	p := ErrInternal.WithCause(errors.New("disk on fire"))
	if !contains(p.Error(), "disk on fire") {
		t.Errorf("Error() should carry the cause for logs, got %q", p.Error())
	}
	if contains(ErrNotFound.Error(), "<nil>") {
		t.Errorf("Error() without a cause should not print <nil>, got %q", ErrNotFound.Error())
	}
}

func TestVocabularyCodesAreUniqueAndWellFormed(t *testing.T) {
	// A duplicated code would make two different failures indistinguishable to
	// the client, which is the one thing `code` exists to prevent.
	all := []*Problem{
		ErrBadRequest, ErrValidation, ErrUnauthorized, ErrTokenExpired, ErrForbidden,
		ErrNotFound, ErrConflict, ErrRateLimited, ErrInternal, ErrUnavailable,
		ErrIdempotencyKeyRequired, ErrIdempotencyKeyInvalid, ErrIdempotencyKeyReused,
		ErrIdempotencyInFlight, ErrOffsetPagination, ErrCursorInvalid, ErrLimitInvalid,
	}
	seen := map[string]bool{}
	for _, p := range all {
		if seen[p.Code] {
			t.Errorf("duplicate problem code %q", p.Code)
		}
		seen[p.Code] = true

		if p.Status < 400 || p.Status > 599 {
			t.Errorf("%s: status %d is not an error status", p.Code, p.Status)
		}
		if p.Title == "" || p.Detail == "" {
			t.Errorf("%s: title and detail must both be set", p.Code)
		}
		for i := 0; i < len(p.Code); i++ {
			c := p.Code[i]
			if !((c >= 'A' && c <= 'Z') || c == '_') {
				t.Errorf("%s: codes must be SCREAMING_SNAKE_CASE", p.Code)
				break
			}
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
