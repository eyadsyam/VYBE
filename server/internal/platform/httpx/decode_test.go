package httpx_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

// DecodeJSON is exercised indirectly by every handler test, but those live in
// other packages — so without these it shows 0% in its own coverage report and,
// more importantly, its edge cases are only tested where somebody happened to
// think of them.

type target struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// decode runs DecodeJSON against a request built from body and contentType.
func decode(t *testing.T, body, contentType string) (target, error) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	var dst target
	err := httpx.DecodeJSON(httptest.NewRecorder(), req, &dst)
	return dst, err
}

func TestDecodeJSONAcceptsAWellFormedBody(t *testing.T) {
	got, err := decode(t, `{"name":"سارة","count":3}`, "application/json")
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if got.Name != "سارة" || got.Count != 3 {
		t.Errorf("decoded %+v", got)
	}
}

func TestDecodeJSONAcceptsAJSONSuffixMediaType(t *testing.T) {
	// RFC 6839's +json convention. A client sending
	// `application/vnd.vybe.v1+json` is doing something reasonable and should
	// not be refused for it.
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/problem+json",
		"application/vnd.vybe.v1+json",
	} {
		if _, err := decode(t, `{"name":"x"}`, ct); err != nil {
			t.Errorf("Content-Type %q was refused: %v", ct, err)
		}
	}
}

func TestDecodeJSONRejectsANonJSONContentType(t *testing.T) {
	for _, ct := range []string{
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"application/xml",
	} {
		_, err := decode(t, `{"name":"x"}`, ct)
		problem := httpx.AsProblem(err)
		if problem == nil || problem.Status != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q gave %v, want a 415 problem", ct, err)
		}
	}
}

func TestDecodeJSONRequiresAContentType(t *testing.T) {
	// An absent Content-Type on a body-carrying request is almost always a
	// misconfigured client. Guessing JSON would hide that until something
	// subtler broke.
	_, err := decode(t, `{"name":"x"}`, "")
	problem := httpx.AsProblem(err)
	if problem == nil || problem.Status != http.StatusUnsupportedMediaType {
		t.Errorf("no Content-Type gave %v, want a 415 problem", err)
	}
}

func TestDecodeJSONRejectsAnUnparseableContentType(t *testing.T) {
	_, err := decode(t, `{"name":"x"}`, "application/json; charset=")
	if httpx.AsProblem(err) == nil {
		t.Errorf("a malformed Content-Type gave %v, want a problem", err)
	}
}

func TestDecodeJSONNamesAnUnknownField(t *testing.T) {
	// The whole reason DisallowUnknownFields is on. A client sending `nmae`
	// otherwise gets a confusing complaint about a missing `name` — for a
	// field they can see they typed.
	_, err := decode(t, `{"nmae":"x"}`, "application/json")

	problem := httpx.AsProblem(err)
	if problem == nil || problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field gave %v, want a 422 problem", err)
	}
	if !strings.Contains(problem.Detail, "nmae") {
		t.Errorf("detail %q does not name the offending field", problem.Detail)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "nmae" {
		t.Errorf("errors[] = %+v, want it to name nmae", problem.Errors)
	}
	if problem.Errors[0].Code != "UNKNOWN_FIELD" {
		t.Errorf("code = %q, want UNKNOWN_FIELD", problem.Errors[0].Code)
	}
}

func TestDecodeJSONNamesATypeMismatch(t *testing.T) {
	_, err := decode(t, `{"count":"three"}`, "application/json")

	problem := httpx.AsProblem(err)
	if problem == nil || problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("type mismatch gave %v, want a 422 problem", err)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "count" {
		t.Errorf("errors[] = %+v, want it to name count", problem.Errors)
	}
	if problem.Errors[0].Code != "TYPE_MISMATCH" {
		t.Errorf("code = %q, want TYPE_MISMATCH", problem.Errors[0].Code)
	}
}

func TestDecodeJSONRejectsMalformedBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty", "", http.StatusBadRequest},
		{"not json", "hello", http.StatusBadRequest},
		{"truncated", `{"name":"x"`, http.StatusBadRequest},
		{"bare array", `[]`, http.StatusUnprocessableEntity},
		{"bare string", `"x"`, http.StatusUnprocessableEntity},
		{"bare number", `42`, http.StatusUnprocessableEntity},
		// Without the trailing-value check, this decodes the FIRST object and
		// silently discards the rest — request-smuggling territory, and at
		// best a client bug nobody notices.
		{"two objects", `{"name":"a"}{"name":"b"}`, http.StatusBadRequest},
		{"object then junk", `{"name":"a"} garbage`, http.StatusBadRequest},
		{"trailing array", `{"name":"a"}[1]`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decode(t, tt.body, "application/json")
			problem := httpx.AsProblem(err)
			if problem == nil {
				t.Fatalf("body %q gave %v, want a problem", tt.body, err)
			}
			if problem.Status != tt.want {
				t.Errorf("body %q gave status %d, want %d", tt.body, problem.Status, tt.want)
			}
		})
	}
}

func TestDecodeJSONAcceptsTrailingWhitespace(t *testing.T) {
	// Whitespace after the object is legal JSON framing, not a second value.
	// Rejecting it would break every client that pretty-prints its bodies.
	for _, body := range []string{
		`{"name":"x"}` + "\n",
		`{"name":"x"}` + "  \t\r\n ",
	} {
		if _, err := decode(t, body, "application/json"); err != nil {
			t.Errorf("body %q was refused: %v", body, err)
		}
	}
}

func TestDecodeJSONRejectsAnOversizedBody(t *testing.T) {
	// An unbounded decode lets one request exhaust the process's memory, and
	// the limit has to be applied to the READER — checking afterwards means
	// the memory is already gone.
	huge := `{"name":"` + strings.Repeat("a", httpx.MaxBodyBytes+1024) + `"}`

	_, err := decode(t, huge, "application/json")
	problem := httpx.AsProblem(err)
	if problem == nil || problem.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body gave %v, want a 413 problem", err)
	}
	if problem.Code != "PAYLOAD_TOO_LARGE" {
		t.Errorf("code = %q, want PAYLOAD_TOO_LARGE", problem.Code)
	}
}

func TestDecodeJSONAcceptsABodyAtExactlyTheLimit(t *testing.T) {
	// The boundary in the other direction. An off-by-one here refuses a
	// legitimate request for being exactly as large as the documented maximum.
	const overhead = len(`{"name":""}`)
	body := `{"name":"` + strings.Repeat("a", httpx.MaxBodyBytes-overhead) + `"}`
	if len(body) != httpx.MaxBodyBytes {
		t.Fatalf("test setup: body is %d bytes, want exactly %d", len(body), httpx.MaxBodyBytes)
	}

	if _, err := decode(t, body, "application/json"); err != nil {
		t.Errorf("a body at exactly the limit was refused: %v", err)
	}
}

func TestDecodeJSONErrorsAreProblemsAllTheWayDown(t *testing.T) {
	// Every failure must be a *Problem so a handler can pass it straight to
	// WriteProblem. One that is not would render as a bare 500 with no code
	// for the client to branch on.
	bodies := []string{"", "hello", `{"nmae":"x"}`, `{"count":"three"}`, `{}{}`}

	for _, body := range bodies {
		_, err := decode(t, body, "application/json")
		if err == nil {
			continue
		}
		var problem *httpx.Problem
		if !errors.As(err, &problem) {
			t.Errorf("body %q produced %T, want a *Problem", body, err)
			continue
		}
		if problem.Code == "" {
			t.Errorf("body %q produced a problem with no code", body)
		}
	}
}
