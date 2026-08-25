package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// TraceHeader is the inbound header §14.2 requires the mobile client to send so
// one user-visible failure can be followed across client and server logs.
const TraceHeader = "X-Trace-Id"

type contextKey int

const (
	traceIDKey contextKey = iota
	actorIDKey
)

// Trace establishes a trace id for the request.
//
// It prefers the client's value, because the whole point of §14.2 is that a
// user reporting "it failed" can be joined to server logs via the id their app
// already recorded. A generated id would be correct-looking and useless for
// that.
//
// The client's value is length-capped and sanitised: it lands in structured
// logs, and an unbounded attacker-controlled string in a log line is how log
// injection and 10MB log entries happen.
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitiseTraceID(r.Header.Get(TraceHeader))
		if id == "" {
			id = newTraceID()
		}
		ctx := context.WithValue(r.Context(), traceIDKey, id)
		// Echo it so a client that did not send one can still record what the
		// server used, and so a proxy can correlate without parsing the body.
		w.Header().Set(TraceHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TraceIDFromContext returns the request's trace id, or "" outside a request.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

// ContextWithTraceID is for tests and for background workers that continue a
// request's work after the response (the outbox drain, FR-56).
func ContextWithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// ActorID is the authenticated user id, or "" for an anonymous request.
//
// It lives here rather than in the identity module because httpx needs it:
// FR-57's idempotency keys MUST be scoped per actor, or one user's key collides
// with another's and replays somebody else's response.
func ActorID(ctx context.Context) string {
	if v, ok := ctx.Value(actorIDKey).(string); ok {
		return v
	}
	return ""
}

// ContextWithActorID is called by the identity middleware once a token is
// verified.
func ContextWithActorID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, actorIDKey, id)
}

const maxTraceIDLen = 64

// sanitiseTraceID keeps printable ASCII minus whitespace and control
// characters, truncated. Anything else is dropped entirely rather than
// partially cleaned, because a half-scrubbed id is not worth correlating.
func sanitiseTraceID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxTraceIDLen {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return ""
		}
	}
	return raw
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not survivable in any meaningful sense, but a
		// trace id is diagnostic metadata — degrading it must never fail the
		// user's request.
		return "trace-unavailable"
	}
	return hex.EncodeToString(b[:])
}
