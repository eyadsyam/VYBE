package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTracePrefersTheClientsID(t *testing.T) {
	// §14.2's whole purpose: a user reporting "it failed" can be joined to
	// server logs through the id their app already recorded. Generating a new
	// one would look correct and be useless for that.
	var seen string
	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceIDFromContext(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/v1/rooms", nil)
	r.Header.Set(TraceHeader, "client-abc-123")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if seen != "client-abc-123" {
		t.Errorf("context trace id = %q, want the client's value", seen)
	}
	if got := rr.Header().Get(TraceHeader); got != "client-abc-123" {
		t.Errorf("response %s = %q, want it echoed", TraceHeader, got)
	}
}

func TestTraceGeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceIDFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no trace id was generated")
	}
	if len(seen) != 32 {
		t.Errorf("generated id %q has length %d, want 32 hex chars", seen, len(seen))
	}
	if rr.Header().Get(TraceHeader) != seen {
		t.Error("the generated id was not echoed to the client")
	}
}

func TestTraceGeneratesDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[TraceIDFromContext(r.Context())] = true
	}))
	for i := 0; i < 100; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if len(seen) != 100 {
		t.Errorf("got %d distinct ids from 100 requests; collisions make traces useless", len(seen))
	}
}

func TestHostileTraceIDsAreRejectedNotSanitised(t *testing.T) {
	// This value lands in structured logs. A newline forges log entries, and an
	// unbounded string writes multi-megabyte ones. A half-cleaned id is not
	// worth correlating, so anything suspect is dropped and replaced.
	tests := []struct {
		name string
		raw  string
	}{
		{"newline injection", "abc\nlevel=ERROR msg=\"forged\""},
		{"carriage return", "abc\rdef"},
		{"null byte", "abc\x00def"},
		{"tab", "abc\tdef"},
		{"space", "abc def"},
		{"json breakout", `abc","evil":"yes`},
		{"too long", strings.Repeat("a", maxTraceIDLen+1)},
		{"unicode", "abc‮en"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = TraceIDFromContext(r.Context())
			}))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set(TraceHeader, tt.raw)
			h.ServeHTTP(httptest.NewRecorder(), r)

			if seen == tt.raw {
				t.Fatalf("hostile trace id %q was accepted verbatim", tt.raw)
			}
			if seen == "" {
				t.Fatal("a replacement id must still be generated")
			}
			for _, bad := range []string{"\n", "\r", "\x00", "\t", " ", `"`} {
				if strings.Contains(seen, bad) {
					t.Errorf("resulting id %q still contains %q", seen, bad)
				}
			}
		})
	}
}

func TestAcceptableTraceIDCharacters(t *testing.T) {
	for _, ok := range []string{
		"abc123",
		"550e8400-e29b-41d4-a716-446655440000",
		"trace_id.with.dots",
		strings.Repeat("a", maxTraceIDLen),
	} {
		t.Run(ok, func(t *testing.T) {
			if got := sanitiseTraceID(ok); got != ok {
				t.Errorf("sanitiseTraceID(%q) = %q, want it preserved", ok, got)
			}
		})
	}
}

func TestTraceIDSurroundingWhitespaceIsTrimmed(t *testing.T) {
	if got := sanitiseTraceID("  abc123  "); got != "abc123" {
		t.Errorf("got %q, want %q", got, "abc123")
	}
	if got := sanitiseTraceID("   "); got != "" {
		t.Errorf("whitespace-only should be rejected, got %q", got)
	}
}

func TestTraceIDFromContextOutsideARequest(t *testing.T) {
	if got := TraceIDFromContext(t.Context()); got != "" {
		t.Errorf("got %q, want empty outside a request", got)
	}
}

func TestActorIDRoundTrip(t *testing.T) {
	if got := ActorID(t.Context()); got != "" {
		t.Errorf("anonymous context should yield %q, got %q", "", got)
	}
	ctx := ContextWithActorID(t.Context(), "user-1")
	if got := ActorID(ctx); got != "user-1" {
		t.Errorf("ActorID = %q, want user-1", got)
	}
}

func TestTraceAndActorDoNotCollideInContext(t *testing.T) {
	// Both use unexported int keys off the same type. If the constants were
	// ever given the same value, one would silently read the other's string —
	// which would scope idempotency keys by trace id. That is worth a test.
	ctx := ContextWithTraceID(t.Context(), "the-trace")
	ctx = ContextWithActorID(ctx, "the-actor")

	if got := TraceIDFromContext(ctx); got != "the-trace" {
		t.Errorf("trace id = %q", got)
	}
	if got := ActorID(ctx); got != "the-actor" {
		t.Errorf("actor id = %q", got)
	}
}
