package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecorderIgnoresASecondWriteHeader(t *testing.T) {
	// net/http logs "superfluous WriteHeader" and keeps the first status. The
	// recorder must agree with it, or a stored replay would carry a status the
	// client never actually received.
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr, status: http.StatusOK}

	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusTeapot)

	if rec.status != http.StatusCreated {
		t.Errorf("recorded status = %d, want the first one (201)", rec.status)
	}
	if rr.Code != http.StatusCreated {
		t.Errorf("underlying status = %d, want 201", rr.Code)
	}
}

func TestRecorderDefaultsToOKOnBareWrite(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr, status: http.StatusOK}

	if _, err := rec.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.status)
	}
	if rec.body.String() != "hello" {
		t.Errorf("mirrored body = %q", rec.body.String())
	}
}

func TestRecorderCapsTheMirrorButNotTheResponse(t *testing.T) {
	// A body over the cap must still reach the client in full — it just becomes
	// ineligible for replay. Truncating the real response would corrupt it.
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr, status: http.StatusOK}

	big := strings.Repeat("x", maxIdemBodyBytes+500)
	n, err := rec.Write([]byte(big))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(big) {
		t.Errorf("Write reported %d bytes, want %d — the client must get all of it", n, len(big))
	}
	if rr.Body.Len() != len(big) {
		t.Errorf("client received %d bytes, want %d", rr.Body.Len(), len(big))
	}
	if rec.body.Len() > maxIdemBodyBytes {
		t.Errorf("mirror grew to %d bytes, want it capped at %d", rec.body.Len(), maxIdemBodyBytes)
	}

	// A further write once the cap is reached must not panic or grow it.
	if _, err := rec.Write([]byte("more")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if rec.body.Len() > maxIdemBodyBytes {
		t.Errorf("mirror exceeded the cap after a second write: %d", rec.body.Len())
	}
}

func TestRecorderUnwrapExposesTheUnderlyingWriter(t *testing.T) {
	// http.ResponseController walks Unwrap to find Flush/Hijack. Without it,
	// wrapping silently disables streaming further down the chain.
	rr := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: rr}
	if rec.Unwrap() != http.ResponseWriter(rr) {
		t.Error("Unwrap did not return the underlying ResponseWriter")
	}
}

func TestReplayDefaultsAMissingStatusTo200(t *testing.T) {
	// A record written by an older version, or truncated, must not produce a
	// literal "HTTP 0".
	rr := httptest.NewRecorder()
	replay(rr, &IdemRecord{ResponseBody: []byte(`{"ok":true}`)})

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if rr.Header().Get(ReplayHeader) != "true" {
		t.Error("replay must be marked")
	}
}

// completeFailingStore reserves cleanly but fails to persist, exercising the
// "logged, not surfaced" path: the client already holds a correct response.
type completeFailingStore struct{ released bool }

func (s *completeFailingStore) Reserve(context.Context, string, string, string, []byte, time.Duration) (*IdemRecord, error) {
	return nil, nil
}
func (s *completeFailingStore) Complete(context.Context, string, string, int, []byte) error {
	return errors.New("postgres write failed")
}
func (s *completeFailingStore) Release(context.Context, string, string) error {
	s.released = true
	return errors.New("release also failed")
}

func TestPersistFailureDoesNotBreakACorrectResponse(t *testing.T) {
	store := &completeFailingStore{}
	h, calls := newIdemHandler(t, store, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusCreated, map[string]string{"id": "room-1"})
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post(`{"contentId":"c1"}`))

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 — a storage failure must not corrupt a correct response", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "room-1") {
		t.Errorf("body = %s", rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times", calls.Load())
	}
}

func TestReleaseFailureAfter5xxIsAlsoTolerated(t *testing.T) {
	store := &completeFailingStore{}
	h, _ := newIdemHandler(t, store, func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, ErrInternal)
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post(`{"contentId":"c1"}`))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	if !store.released {
		t.Error("Release was not attempted after a 5xx")
	}
}

func TestSlugForCodeHandlesDigitsAndLowercase(t *testing.T) {
	tests := map[string]string{
		"ROOM_FULL":   "room-full",
		"HTTP2_ONLY":  "http2-only",
		"alreadyLow":  "alreadylow",
		"MIXED_Case1": "mixed-case1",
	}
	for in, want := range tests {
		if got := slugForCode(in); got != want {
			t.Errorf("slugForCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeExtrasWithNoExtrasReturnsTheStruct(t *testing.T) {
	body := ProblemDetails{Code: "X", Status: 400}
	got := mergeExtras(body, nil)
	if _, isMap := got.(map[string]any); isMap {
		t.Error("with no extras the struct should be returned unchanged, not converted to a map")
	}
}

func TestMergeExtrasSurvivesAnUnmarshalableValue(t *testing.T) {
	// json.Marshal fails on a channel. The response must still be written
	// rather than the request dying inside the error path.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	WriteProblem(rr, req, ErrBadRequest.WithExtra("bad", make(chan int)))

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// errReader fails mid-body, simulating a client that disconnects during upload.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestBodyReadFailureIsA400NotA500(t *testing.T) {
	// A truncated upload is the client's transport failing, not the server
	// breaking. Returning 500 would page somebody for a flaky phone.
	h, calls := newIdemHandler(t, NewMemoryIdemStore(), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/rooms", errReader{})
	r.Header.Set(IdempotencyHeader, testKey)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if calls.Load() != 0 {
		t.Error("the handler ran despite an unreadable body")
	}
}
