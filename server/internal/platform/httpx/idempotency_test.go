package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testActor = "0192f0c1-8a3e-7c4d-9b2a-1f5e6d7c8b9a"
const testKey = "idem-key-0000000001"

// newIdemHandler wires the middleware over a handler that counts executions,
// which is the property FR-57 is actually about.
func newIdemHandler(t *testing.T, store IdemStore, h http.HandlerFunc) (http.Handler, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	})
	// The actor normally arrives from the identity middleware.
	withActor := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := ContextWithActorID(r.Context(), testActor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return withActor(Idempotency(store)(inner)), &calls
}

func post(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/rooms", strings.NewReader(body))
	r.Header.Set(IdempotencyHeader, testKey)
	return r
}

func TestIdempotencyReplaysInsteadOfReExecuting(t *testing.T) {
	// The core of FR-57: a retry after a lost response must return the ORIGINAL
	// response and must not run the handler a second time.
	store := NewMemoryIdemStore()
	h, calls := newIdemHandler(t, store, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusCreated, map[string]string{"joinCode": "K7X2QP"})
	})

	first := httptest.NewRecorder()
	h.ServeHTTP(first, post(`{"contentId":"c1"}`))

	second := httptest.NewRecorder()
	h.ServeHTTP(second, post(`{"contentId":"c1"}`))

	if got := calls.Load(); got != 1 {
		t.Errorf("handler ran %d times, want 1 — the retry must not re-execute", got)
	}
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Errorf("statuses = %d and %d, want both 201", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replay body differs:\n first: %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
	if got := second.Header().Get(ReplayHeader); got != "true" {
		t.Errorf("%s = %q on the replay, want \"true\"", ReplayHeader, got)
	}
	if got := first.Header().Get(ReplayHeader); got != "" {
		t.Errorf("%s should be absent on the first execution, got %q", ReplayHeader, got)
	}
}

func TestIdempotencyRequiresAndValidatesTheKey(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		wantCode string
	}{
		{"absent", "", "IDEMPOTENCY_KEY_REQUIRED"},
		{"too short", "short", "IDEMPOTENCY_KEY_INVALID"},
		{"too long", strings.Repeat("k", 256), "IDEMPOTENCY_KEY_INVALID"},
		{"contains a space", "has a space here", "IDEMPOTENCY_KEY_INVALID"},
		{"contains a control character", "abcdefgh\x00ij", "IDEMPOTENCY_KEY_INVALID"},
		{"at the minimum length", "12345678", ""},
		{"at the maximum length", strings.Repeat("k", 255), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, calls := newIdemHandler(t, NewMemoryIdemStore(), func(w http.ResponseWriter, r *http.Request) {
				WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/rooms", strings.NewReader("{}"))
			if tt.key != "" {
				r.Header.Set(IdempotencyHeader, tt.key)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if tt.wantCode == "" {
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body %s", rr.Code, rr.Body.String())
				}
				return
			}
			if calls.Load() != 0 {
				t.Error("the handler ran despite an invalid key")
			}
			got := decodeProblem(t, rr)
			if got["code"] != tt.wantCode {
				t.Errorf("code = %v, want %v", got["code"], tt.wantCode)
			}
		})
	}
}

func TestSafeMethodsBypassIdempotency(t *testing.T) {
	// FR-57 covers mutations. Requiring a key on GET would break every read.
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(m, func(t *testing.T) {
			h, calls := newIdemHandler(t, NewMemoryIdemStore(), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(m, "/v1/rooms", nil))
			if rr.Code != http.StatusOK || calls.Load() != 1 {
				t.Errorf("%s was blocked: status %d, calls %d", m, rr.Code, calls.Load())
			}
		})
	}
}

func TestSameKeyWithDifferentBodyIsRejected(t *testing.T) {
	// Replaying the first response for a genuinely different request would be
	// silent data loss. 422 tells the client it has a bug.
	store := NewMemoryIdemStore()
	h, calls := newIdemHandler(t, store, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusCreated, map[string]string{"id": "room-1"})
	})

	h.ServeHTTP(httptest.NewRecorder(), post(`{"contentId":"c1"}`))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post(`{"contentId":"DIFFERENT"}`))

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if got := decodeProblem(t, rr)["code"]; got != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("code = %v, want IDEMPOTENCY_KEY_REUSED", got)
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times; the mismatched retry must not execute", calls.Load())
	}
}

func TestKeysAreScopedPerActor(t *testing.T) {
	// An unscoped key namespace lets one user replay another's response. That
	// is an authorisation bug, which is why the schema's PK is (user_id, key).
	store := NewMemoryIdemStore()
	var served []string
	var mu sync.Mutex

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := ActorID(r.Context())
		mu.Lock()
		served = append(served, actor)
		mu.Unlock()
		WriteJSON(w, http.StatusCreated, map[string]string{"servedTo": actor})
	})
	wrapped := Idempotency(store)(inner)

	call := func(actor string) *httptest.ResponseRecorder {
		r := post(`{"contentId":"c1"}`)
		r = r.WithContext(ContextWithActorID(r.Context(), actor))
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, r)
		return rr
	}

	alice := call("alice")
	bob := call("bob")

	if len(served) != 2 {
		t.Fatalf("handler ran %d times, want 2 — Bob must not replay Alice's response", len(served))
	}
	if !strings.Contains(alice.Body.String(), "alice") {
		t.Errorf("alice got %s", alice.Body.String())
	}
	if !strings.Contains(bob.Body.String(), "bob") {
		t.Errorf("bob got %s — cross-actor replay", bob.Body.String())
	}
	if bob.Header().Get(ReplayHeader) == "true" {
		t.Error("Bob's request was served as a replay of Alice's")
	}
}

func TestAnonymousCallerIsRefused(t *testing.T) {
	// No actor means no scope, and a global key namespace is cross-user replay.
	wrapped := Idempotency(NewMemoryIdemStore())(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, post(`{}`))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestConcurrentIdenticalRequestsExecuteOnce(t *testing.T) {
	// The reservation is written BEFORE the handler runs precisely so two
	// concurrent retries serialise. Exactly one must execute; the other gets
	// 409 in-flight rather than double-joining the room.
	store := NewMemoryIdemStore()
	release := make(chan struct{})
	var calls atomic.Int32

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release // hold the first request open so the second overlaps it
		WriteJSON(w, http.StatusCreated, map[string]string{"id": "room-1"})
	})
	wrapped := Idempotency(store)(inner)

	do := func() *httptest.ResponseRecorder {
		r := post(`{"contentId":"c1"}`)
		r = r.WithContext(ContextWithActorID(r.Context(), testActor))
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, r)
		return rr
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- do() }()

	// Wait until the first request is definitely inside the handler.
	deadline := time.After(2 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the first request never reached the handler")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	second := do() // overlaps the in-flight first
	close(release)
	first := <-firstDone

	if got := calls.Load(); got != 1 {
		t.Errorf("handler executed %d times concurrently, want exactly 1", got)
	}
	if first.Code != http.StatusCreated {
		t.Errorf("first status = %d, want 201", first.Code)
	}
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409 in-flight; body %s", second.Code, second.Body.String())
	}
	if got := decodeProblem(t, second)["code"]; got != "IDEMPOTENCY_IN_FLIGHT" {
		t.Errorf("code = %v, want IDEMPOTENCY_IN_FLIGHT", got)
	}
}

func TestServerErrorsAreNotStoredSoRetryCanSucceed(t *testing.T) {
	// A 5xx means "unknown", not "no". Storing it would make every future retry
	// replay the failure forever, turning a blip into a permanent outage for
	// that key.
	store := NewMemoryIdemStore()
	var calls atomic.Int32

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			WriteProblem(w, r, ErrInternal)
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]string{"id": "room-1"})
	})
	wrapped := Idempotency(store)(inner)

	do := func() *httptest.ResponseRecorder {
		r := post(`{"contentId":"c1"}`)
		r = r.WithContext(ContextWithActorID(r.Context(), testActor))
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, r)
		return rr
	}

	if got := do().Code; got != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", got)
	}
	retry := do()
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201 — a 5xx must be retryable, not replayed", retry.Code)
	}
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2", calls.Load())
	}
}

func TestClientErrorsAreStoredAndReplayed(t *testing.T) {
	// A 4xx IS a terminal answer: the same request will always be rejected the
	// same way, so replaying it is correct and saves the work.
	store := NewMemoryIdemStore()
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		WriteProblem(w, r, ErrConflict.WithDetail("Room already ended."))
	})
	wrapped := Idempotency(store)(inner)

	do := func() *httptest.ResponseRecorder {
		r := post(`{"contentId":"c1"}`)
		r = r.WithContext(ContextWithActorID(r.Context(), testActor))
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, r)
		return rr
	}

	first, second := do(), do()
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", calls.Load())
	}
	if first.Code != http.StatusConflict || second.Code != http.StatusConflict {
		t.Errorf("statuses = %d, %d; want both 409", first.Code, second.Code)
	}
	if second.Header().Get(ReplayHeader) != "true" {
		t.Error("the replayed 4xx is not marked as a replay")
	}
}

func TestHandlerStillReceivesTheBody(t *testing.T) {
	// The middleware consumes the body to fingerprint it; forgetting to restore
	// it would make every handler see an empty request.
	var got string
	h, _ := newIdemHandler(t, NewMemoryIdemStore(), func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		got = string(b[:n])
		w.WriteHeader(http.StatusOK)
	})
	h.ServeHTTP(httptest.NewRecorder(), post(`{"contentId":"c1"}`))

	if got != `{"contentId":"c1"}` {
		t.Errorf("handler saw body %q, want the original", got)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	h, calls := newIdemHandler(t, NewMemoryIdemStore(), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/rooms", strings.NewReader(strings.Repeat("x", maxIdemBodyBytes+10)))
	r.Header.Set(IdempotencyHeader, testKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if calls.Load() != 0 {
		t.Error("the handler ran on an oversized body")
	}
}

func TestStoreFailureSurfacesAs500(t *testing.T) {
	h, calls := newIdemHandler(t, failingStore{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post(`{}`))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if calls.Load() != 0 {
		t.Error("the handler ran despite the reservation failing")
	}
}

type failingStore struct{}

func (failingStore) Reserve(context.Context, string, string, string, []byte, time.Duration) (*IdemRecord, error) {
	return nil, errors.New("postgres unavailable")
}
func (failingStore) Complete(context.Context, string, string, int, []byte) error { return nil }
func (failingStore) Release(context.Context, string, string) error               { return nil }

func TestMemoryStoreExpiresRecords(t *testing.T) {
	// After the 24h TTL the key is free again, which is what a bounded TTL
	// means. Tested with an injected clock: nobody runs a test that sleeps 24h.
	store := NewMemoryIdemStore()
	now := time.Date(2026, 8, 25, 7, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })

	fp := fingerprint(http.MethodPost, "/v1/rooms", []byte("{}"))
	if existing, err := store.Reserve(t.Context(), testActor, testKey, "POST /v1/rooms", fp, IdemTTL); err != nil || existing != nil {
		t.Fatalf("first reserve: existing=%v err=%v", existing, err)
	}
	if err := store.Complete(t.Context(), testActor, testKey, 201, []byte(`{"id":"r"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	existing, err := store.Reserve(t.Context(), testActor, testKey, "POST /v1/rooms", fp, IdemTTL)
	if err != nil || existing == nil || existing.Status != IdemCompleted {
		t.Fatalf("within TTL the record should still be there, got %+v err=%v", existing, err)
	}

	now = now.Add(IdemTTL + time.Second)
	existing, err = store.Reserve(t.Context(), testActor, testKey, "POST /v1/rooms", fp, IdemTTL)
	if err != nil {
		t.Fatalf("reserve after expiry: %v", err)
	}
	if existing != nil {
		t.Errorf("record survived its TTL: %+v", existing)
	}
}

func TestMemoryStoreCompleteOnMissingRecordIsNotAnError(t *testing.T) {
	// The record can legitimately have been reaped between reserve and
	// complete. That must not fail a request whose response is already correct.
	store := NewMemoryIdemStore()
	if err := store.Complete(t.Context(), testActor, "never-reserved-key", 200, nil); err != nil {
		t.Errorf("Complete on a missing record returned %v, want nil", err)
	}
}

func TestFingerprintSeparatesFieldsUnambiguously(t *testing.T) {
	// Length-prefixing prevents a crafted path from colliding with a different
	// method/path/body split.
	a := fingerprint("POST", "/v1/rooms", []byte("x"))
	b := fingerprint("POST", "/v1/room", []byte("sx"))
	if string(a) == string(b) {
		t.Error("fingerprint collision across a shifted method/path/body boundary")
	}
	if string(fingerprint("POST", "/a", []byte("b"))) == string(fingerprint("GET", "/a", []byte("b"))) {
		t.Error("method is not part of the fingerprint")
	}
}
