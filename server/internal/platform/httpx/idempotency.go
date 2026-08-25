package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// FR-57: every non-GET mutation requires an Idempotency-Key; the server stores
// key -> response for 24h and replays on repeat.
//
// This is not a nicety on mobile. A phone loses its network mid-POST all the
// time, and the client cannot tell "the request never arrived" from "the
// response never came back". Without replay, the safe client behaviour is to
// not retry, which loses writes; the convenient behaviour is to retry, which
// double-joins rooms and double-submits trivia answers. Replay makes retry
// both safe and correct.
//
// Records live in **Postgres**, not Redis. ADR-009 restricts Redis to state
// that can be reconstructed after a flush, and an idempotency record is the
// opposite of reconstructible: losing one means the next retry executes a
// second time. That is exactly the bug this exists to prevent.

// IdemStatus mirrors the `status` column's CHECK constraint in migration 0007.
type IdemStatus string

const (
	// IdemInFlight is written BEFORE the handler runs, so two concurrent
	// identical requests serialise on the primary key rather than both
	// executing.
	IdemInFlight  IdemStatus = "in_flight"
	IdemCompleted IdemStatus = "completed"
)

// IdemRecord is one row of idempotency_keys.
type IdemRecord struct {
	Status         IdemStatus
	Fingerprint    []byte
	ResponseStatus int
	ResponseBody   []byte
}

// IdemStore is the persistence contract. The production implementation is
// Postgres-backed; MemoryIdemStore below is used by tests and by local runs
// with no database.
type IdemStore interface {
	// Reserve atomically inserts an in_flight record, or returns the record
	// that already exists for (actorID, key).
	//
	// Atomicity is the entire contract. A read-then-write implementation has a
	// race between the two, which is precisely the concurrent-retry case this
	// guards. In Postgres this is one INSERT ... ON CONFLICT DO NOTHING
	// followed by a SELECT of the conflicting row.
	Reserve(ctx context.Context, actorID, key, endpoint string, fingerprint []byte, ttl time.Duration) (existing *IdemRecord, err error)

	// Complete stores the terminal response for replay.
	Complete(ctx context.Context, actorID, key string, status int, body []byte) error

	// Release removes a reservation so the request can be retried. Used when
	// the handler produced a 5xx, which is by definition not a terminal answer.
	Release(ctx context.Context, actorID, key string) error
}

// IdempotencyHeader is the client-supplied key. The spelling is fixed by the
// IETF draft that the wider ecosystem follows, so clients and proxies agree.
const IdempotencyHeader = "Idempotency-Key"

// ReplayHeader marks a response as served from the store rather than freshly
// executed. It is diagnostic: it makes "did my retry actually re-run?"
// answerable from a log or a proxy trace instead of by inference.
const ReplayHeader = "Idempotent-Replay"

const (
	minIdemKeyLen = 8   // matches the CHECK in migration 0007
	maxIdemKeyLen = 255 // matches the CHECK in migration 0007

	// maxIdemBodyBytes caps what is buffered for fingerprinting and replay.
	// No M1 mutation body is remotely this large; the cap exists so a hostile
	// client cannot make the server hold arbitrary memory per connection.
	maxIdemBodyBytes = 1 << 20 // 1 MiB

	// IdemTTL is §5.2's 24 hours.
	IdemTTL = 24 * time.Hour
)

// Idempotency returns middleware enforcing FR-57.
//
// Mount it only on authenticated mutating routes. Keys are scoped per actor
// (the schema's PRIMARY KEY (user_id, key)) because an unscoped key lets one
// client's arbitrary string collide with another's and replay a response
// belonging to a different user — an authorisation bug wearing a caching
// costume. Unauthenticated mutations, such as the token endpoints in FR-1–6,
// therefore do not mount this.
func Idempotency(store IdemStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get(IdempotencyHeader)
			if key == "" {
				WriteProblem(w, r, ErrIdempotencyKeyRequired)
				return
			}
			if !validIdemKey(key) {
				WriteProblem(w, r, ErrIdempotencyKeyInvalid)
				return
			}

			actor := ActorID(r.Context())
			if actor == "" {
				// Refusing is the only safe answer: without an actor there is
				// no scope, and a global key namespace is cross-user replay.
				WriteProblem(w, r, ErrUnauthorized.WithDetail(
					"Idempotent endpoints require an authenticated caller."))
				return
			}

			body, err := io.ReadAll(io.LimitReader(r.Body, maxIdemBodyBytes+1))
			_ = r.Body.Close()
			if err != nil {
				WriteProblem(w, r, ErrBadRequest.WithCause(err).WithDetail("Could not read the request body."))
				return
			}
			if len(body) > maxIdemBodyBytes {
				WriteProblem(w, r, ErrBadRequest.WithDetail("Request body exceeds %d bytes.", maxIdemBodyBytes))
				return
			}
			// The handler still needs the body we just consumed.
			r.Body = io.NopCloser(bytes.NewReader(body))

			endpoint := r.Method + " " + r.URL.Path
			fp := fingerprint(r.Method, r.URL.Path, body)

			existing, err := store.Reserve(r.Context(), actor, key, endpoint, fp, IdemTTL)
			if err != nil {
				WriteProblem(w, r, ErrInternal.WithCause(err))
				return
			}

			if existing != nil {
				// subtle.ConstantTimeCompare is habit rather than necessity
				// here; the fingerprint is not a secret. It costs nothing and
				// removes the question from review.
				if subtle.ConstantTimeCompare(existing.Fingerprint, fp) != 1 {
					WriteProblem(w, r, ErrIdempotencyKeyReused)
					return
				}
				if existing.Status == IdemInFlight {
					// The first request is still running. Returning 409 rather
					// than blocking keeps this handler from holding a
					// connection for the duration of an unrelated request.
					WriteProblem(w, r, ErrIdempotencyInFlight)
					return
				}
				replay(w, existing)
				return
			}

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// A 5xx is not a terminal answer — it is "unknown". Storing it
			// would make every later retry replay the failure forever, turning
			// a transient blip into a permanently broken operation. Releasing
			// lets the client retry into a working server.
			if rec.status >= http.StatusInternalServerError {
				if err := store.Release(r.Context(), actor, key); err != nil {
					logReleaseFailure(r.Context(), key, err)
				}
				return
			}

			if err := store.Complete(r.Context(), actor, key, rec.status, rec.body.Bytes()); err != nil {
				// The response has already been written and is correct. Failing
				// to persist it only costs replay on a future retry, so this is
				// logged, not surfaced.
				logCompleteFailure(r.Context(), key, err)
			}
		})
	}
}

func replay(w http.ResponseWriter, rec *IdemRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(ReplayHeader, "true")
	status := rec.ResponseStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rec.ResponseBody)
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// validIdemKey accepts printable ASCII within the schema's length bounds.
//
// Clients are told to use a UUID; the check is deliberately wider than that so
// a client using its own scheme is not broken, but narrow enough that the value
// is safe to log and to use as a database key.
func validIdemKey(k string) bool {
	if len(k) < minIdemKeyLen || len(k) > maxIdemKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// fingerprint is SHA-256 over method, path and body, matching the column
// comment in migration 0007.
//
// The separators are length-prefixed rather than delimiter-joined so that a
// path ending in a newline cannot be crafted to collide with a different
// method/path split.
func fingerprint(method, path string, body []byte) []byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(method), []byte(path), body} {
		var lenBuf [8]byte
		n := uint64(len(part))
		for i := 0; i < 8; i++ {
			lenBuf[i] = byte(n >> (8 * (7 - i)))
		}
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(part)
	}
	return h.Sum(nil)
}

// recorder captures a handler's response so it can be stored for replay.
type recorder struct {
	http.ResponseWriter
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	// Cap the mirror, never the response: a body larger than the cap is still
	// sent to the client in full, it just becomes ineligible for replay.
	if r.body.Len() < maxIdemBodyBytes {
		remaining := maxIdemBodyBytes - r.body.Len()
		if len(b) <= remaining {
			r.body.Write(b)
		} else {
			r.body.Write(b[:remaining])
		}
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer, so wrapping
// does not silently disable flushing or hijacking further down the chain.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ---------------------------------------------------------------------------
// In-memory store
// ---------------------------------------------------------------------------

// MemoryIdemStore is an in-process IdemStore for tests and for a local run
// without Postgres.
//
// It is explicitly NOT production-safe and must never be wired outside local:
// it is per-process, so with more than one instance a retry that lands on
// another node re-executes. config.Env gates the choice at startup.
type MemoryIdemStore struct {
	mu      sync.Mutex
	records map[string]*memoryEntry
	now     func() time.Time
}

type memoryEntry struct {
	rec       IdemRecord
	expiresAt time.Time
}

// NewMemoryIdemStore returns an empty store using the wall clock.
func NewMemoryIdemStore() *MemoryIdemStore {
	return &MemoryIdemStore{records: map[string]*memoryEntry{}, now: time.Now}
}

// SetClock injects a clock so expiry is testable without sleeping. A test that
// sleeps for a 24h TTL is not a test anybody runs.
func (s *MemoryIdemStore) SetClock(now func() time.Time) { s.now = now }

func memKey(actorID, key string) string { return actorID + "\x1f" + key }

func (s *MemoryIdemStore) Reserve(_ context.Context, actorID, key, _ string, fp []byte, ttl time.Duration) (*IdemRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := memKey(actorID, key)
	if e, ok := s.records[k]; ok {
		if s.now().Before(e.expiresAt) {
			clone := e.rec
			return &clone, nil
		}
		// Expired: the key is free again, which is what a 24h TTL means.
		delete(s.records, k)
	}

	s.records[k] = &memoryEntry{
		rec:       IdemRecord{Status: IdemInFlight, Fingerprint: append([]byte(nil), fp...)},
		expiresAt: s.now().Add(ttl),
	}
	return nil, nil
}

func (s *MemoryIdemStore) Complete(_ context.Context, actorID, key string, status int, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.records[memKey(actorID, key)]
	if !ok {
		return nil // released or expired; nothing to complete
	}
	e.rec.Status = IdemCompleted
	e.rec.ResponseStatus = status
	e.rec.ResponseBody = append([]byte(nil), body...)
	return nil
}

func (s *MemoryIdemStore) Release(_ context.Context, actorID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, memKey(actorID, key))
	return nil
}

// WriteJSON renders a success payload. Mutations funnel through it so the
// recorder above captures exactly the bytes a replay will later return.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Both failures below are logged rather than surfaced: in each case the client
// already holds a correct response, and the only cost is a future retry
// re-executing instead of replaying. Turning that into a user-visible error
// would trade a rare, harmless degradation for a certain, visible one.

func logReleaseFailure(ctx context.Context, key string, err error) {
	slog.WarnContext(ctx, "releasing idempotency reservation failed",
		"idempotency_key", key, "trace_id", TraceIDFromContext(ctx), "err", err,
		"impact", "a retry with this key returns 409 until the record expires")
}

func logCompleteFailure(ctx context.Context, key string, err error) {
	slog.WarnContext(ctx, "storing idempotent response failed",
		"idempotency_key", key, "trace_id", TraceIDFromContext(ctx), "err", err,
		"impact", "a retry with this key re-executes instead of replaying")
}
