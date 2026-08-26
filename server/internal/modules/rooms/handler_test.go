package rooms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms/roomstest"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

// recordingPublisher captures what the handler fans out.
type recordingPublisher struct {
	mu     sync.Mutex
	events []realtime.Envelope
	err    error
}

func (p *recordingPublisher) Publish(_ context.Context, e realtime.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, e)
	return nil
}

func (p *recordingPublisher) all() []realtime.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]realtime.Envelope(nil), p.events...)
}

type actorKey struct{}

type roomHarness struct {
	t         *testing.T
	router    chi.Router
	store     *roomstest.Store
	publisher *recordingPublisher
	setNow    func(time.Time)
}

func newRoomHarness(t *testing.T, tierMap tiers) *roomHarness {
	t.Helper()
	store := roomstest.New()
	store.AddContent(contentID)

	svc := rooms.NewService(store, tierMap)
	now := svcNow
	svc.SetClock(func() time.Time { return now })

	pub := &recordingPublisher{}
	actor := func(ctx context.Context) (string, bool) {
		id, ok := ctx.Value(actorKey{}).(string)
		return id, ok && id != ""
	}

	h := rooms.NewHandler(svc, pub, actor, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r := chi.NewRouter()
	r.Use(httpx.Trace)
	r.Mount("/v1/rooms", h.Routes())

	return &roomHarness{t: t, router: r, store: store, publisher: pub, setNow: func(v time.Time) { now = v }}
}

func (h *roomHarness) as(userID, method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()

	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			h.t.Fatalf("marshalling: %v", err)
		}
		rdr = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if userID != "" {
		req = req.WithContext(context.WithValue(req.Context(), actorKey{}, userID))
	}

	rr := httptest.NewRecorder()
	h.router.ServeHTTP(rr, req)
	return rr
}

func decodeInto(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("decoding %q: %v", rr.Body.String(), err)
	}
}

func roomProblemOf(t *testing.T, rr *httptest.ResponseRecorder) httpx.ProblemDetails {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p httpx.ProblemDetails
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding problem %q: %v", rr.Body.String(), err)
	}
	if p.TraceID == "" {
		t.Error("problem has no traceId (FR-58)")
	}
	if p.Status != rr.Code {
		t.Errorf("problem.status = %d, HTTP status = %d", p.Status, rr.Code)
	}
	return p
}

func (h *roomHarness) createRoom(userID string) map[string]any {
	h.t.Helper()
	rr := h.as(userID, http.MethodPost, "/v1/rooms", map[string]any{
		"contentId": contentID, "title": "ليلة أفلام",
	})
	if rr.Code != http.StatusCreated {
		h.t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	var out map[string]any
	decodeInto(h.t, rr, &out)
	return out
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreateEndpointReturns201AndPublishes(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	room := h.createRoom(hostID)

	if room["state"] != "LOBBY" {
		t.Errorf("state = %v, want LOBBY", room["state"])
	}
	if room["joinCode"] == nil || room["joinCode"] == "" {
		t.Error("the creator was not given the join code")
	}
	if room["shareUrl"] == nil || !strings.HasPrefix(room["shareUrl"].(string), "https://vybe.app/r/") {
		t.Errorf("shareUrl = %v, want a https universal link (FR-13)", room["shareUrl"])
	}
	if room["serverTime"] == nil {
		t.Error("no serverTime; ADR-002 needs a clock reference on every response")
	}

	published := h.publisher.all()
	if len(published) != 1 {
		t.Fatalf("%d events published, want 1", len(published))
	}
	if published[0].Type != realtime.EventRoomStateChanged {
		t.Errorf("published %q, want ROOM_STATE_CHANGED", published[0].Type)
	}
}

func TestCreateRequiresAuthentication(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	rr := h.as("", http.MethodPost, "/v1/rooms", map[string]any{"contentId": contentID})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(h.publisher.all()) != 0 {
		t.Error("an unauthenticated request published an event")
	}
}

func TestCreateMapsErrorsToProblems(t *testing.T) {
	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{
			"unknown content",
			map[string]any{"contentId": "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb"},
			http.StatusUnprocessableEntity, "CONTENT_NOT_FOUND",
		},
		{
			"over-long title",
			map[string]any{"contentId": contentID, "title": strings.Repeat("x", rooms.MaxTitleLength+1)},
			http.StatusUnprocessableEntity, "VALIDATION_FAILED",
		},
		{
			"unknown sync mode",
			map[string]any{"contentId": contentID, "syncMode": "TELEPATHY"},
			http.StatusUnprocessableEntity, "VALIDATION_FAILED",
		},
		{
			// A typo'd field must be reported rather than silently dropped —
			// otherwise the user's title never arrives and nothing says why.
			"unknown field",
			map[string]any{"contentId": contentID, "titel": "typo"},
			http.StatusUnprocessableEntity, "VALIDATION_FAILED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRoomHarness(t, tiers{})
			rr := h.as(hostID, http.MethodPost, "/v1/rooms", tt.body)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tt.wantStatus, rr.Body)
			}
			if p := roomProblemOf(t, rr); p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Join
// ---------------------------------------------------------------------------

func TestJoinEndpointReturnsTheParticipantList(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)

	rr := h.as(guestID, http.MethodPost, "/v1/rooms/join", map[string]any{
		"joinCode": created["joinCode"],
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rr.Code, rr.Body)
	}

	var joined struct {
		ID           string `json:"id"`
		Participants []struct {
			UserID string `json:"userId"`
			IsHost bool   `json:"isHost"`
		} `json:"participants"`
	}
	decodeInto(t, rr, &joined)

	// Returned inline so the first thing every client does after joining is
	// not a second request for exactly this.
	if len(joined.Participants) != 2 {
		t.Fatalf("%d participants in the join response, want 2", len(joined.Participants))
	}
	if !joined.Participants[0].IsHost || joined.Participants[0].UserID != hostID {
		t.Errorf("participants[0] = %+v, want the host first", joined.Participants[0])
	}
}

func TestJoinMapsErrorsToProblems(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)

	// Unknown code.
	rr := h.as(guestID, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": "ZZZZZZ"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown code = %d, want 404", rr.Code)
	}
	if p := roomProblemOf(t, rr); p.Code != "ROOM_NOT_FOUND" {
		t.Errorf("code = %q, want ROOM_NOT_FOUND", p.Code)
	}

	// Already joined.
	if rr := h.as(hostID, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": created["joinCode"]}); rr.Code != http.StatusConflict {
		t.Errorf("rejoin = %d, want 409", rr.Code)
	}

	// Full: free tier is 4, host occupies one.
	for _, id := range []string{guestID, thirdID, fourthID} {
		if rr := h.as(id, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": created["joinCode"]}); rr.Code != http.StatusOK {
			t.Fatalf("join by %s: %d %s", id, rr.Code, rr.Body)
		}
	}
	rr = h.as("55555555-5555-7555-8555-555555555555", http.MethodPost, "/v1/rooms/join",
		map[string]any{"joinCode": created["joinCode"]})
	if rr.Code != http.StatusConflict {
		t.Fatalf("the fifth join = %d, want 409", rr.Code)
	}
	if p := roomProblemOf(t, rr); p.Code != "ROOM_FULL" {
		t.Errorf("code = %q, want ROOM_FULL", p.Code)
	}
}

// ---------------------------------------------------------------------------
// Authorisation
// ---------------------------------------------------------------------------

func TestGetHidesTheRoomFromNonMembers(t *testing.T) {
	// FR-14 at the HTTP layer. A stranger must get 404, not 403: telling them
	// "that room exists but you are not in it" confirms the id.
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)

	if rr := h.as(hostID, http.MethodGet, "/v1/rooms/"+id, nil); rr.Code != http.StatusOK {
		t.Fatalf("the host cannot read their own room: %d %s", rr.Code, rr.Body)
	}

	rr := h.as(guestID, http.MethodGet, "/v1/rooms/"+id, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("a stranger = %d, want 404 (not 403)", rr.Code)
	}
	p := roomProblemOf(t, rr)
	if p.Code != "ROOM_NOT_FOUND" {
		t.Errorf("code = %q, want ROOM_NOT_FOUND", p.Code)
	}
	// And the response must not leak the join code, which is the credential.
	if strings.Contains(rr.Body.String(), created["joinCode"].(string)) {
		t.Error("the 404 body contained the room's join code")
	}
}

func TestOnlyTheHostMayEndOrTransitionOverHTTP(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)
	if rr := h.as(guestID, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": created["joinCode"]}); rr.Code != http.StatusOK {
		t.Fatalf("join: %d", rr.Code)
	}

	rr := h.as(guestID, http.MethodPost, "/v1/rooms/"+id+"/end", nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a guest ending = %d, want 403", rr.Code)
	}
	if p := roomProblemOf(t, rr); p.Code != "NOT_THE_HOST" {
		t.Errorf("code = %q, want NOT_THE_HOST", p.Code)
	}

	rr = h.as(guestID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": "ARM"})
	if rr.Code != http.StatusForbidden {
		t.Errorf("a guest arming = %d, want 403", rr.Code)
	}
}

func TestLeaveWithholdsTheJoinCode(t *testing.T) {
	// Echoing the credential back to somebody on their way out is exactly the
	// leak FR-14 is about.
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)
	if rr := h.as(guestID, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": created["joinCode"]}); rr.Code != http.StatusOK {
		t.Fatalf("join: %d", rr.Code)
	}

	rr := h.as(guestID, http.MethodPost, "/v1/rooms/"+id+"/leave", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), created["joinCode"].(string)) {
		t.Error("the leave response echoed the join code back to a departing member")
	}
}

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

func TestTransitionEndpointDrivesTheStateMachine(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)

	for _, step := range []struct{ event, want string }{
		{"ARM", "READY"},
		{"START", "PLAYING"},
	} {
		rr := h.as(hostID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": step.event})
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.event, rr.Code, rr.Body)
		}
		var out map[string]any
		decodeInto(t, rr, &out)
		if out["state"] != step.want {
			t.Errorf("after %s, state = %v, want %q", step.event, out["state"], step.want)
		}
	}
}

func TestIllegalTransitionExplainsItself(t *testing.T) {
	// "Illegal transition" alone leaves a client author guessing what the
	// room's state actually is.
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)

	rr := h.as(hostID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": "START"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("START from LOBBY = %d, want 409; body = %s", rr.Code, rr.Body)
	}

	var raw map[string]any
	decodeInto(t, rr, &raw)
	if raw["code"] != "ILLEGAL_TRANSITION" {
		t.Errorf("code = %v, want ILLEGAL_TRANSITION", raw["code"])
	}
	if raw["fromState"] != "LOBBY" {
		t.Errorf("fromState = %v, want LOBBY", raw["fromState"])
	}
	if raw["attemptedEvent"] != "START" {
		t.Errorf("attemptedEvent = %v, want START", raw["attemptedEvent"])
	}
}

func TestUnknownTransitionEventIsAValidationError(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)

	rr := h.as(hostID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": "EXPLODE"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rr.Code, rr.Body)
	}
	p := roomProblemOf(t, rr)
	if p.Code != "VALIDATION_FAILED" {
		t.Errorf("code = %q, want VALIDATION_FAILED", p.Code)
	}
	if len(p.Errors) == 0 || p.Errors[0].Field != "event" {
		t.Errorf("errors[] does not name the event field: %+v", p.Errors)
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func TestListPaginatesWithOpaqueCursors(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	for i := range 5 {
		h.setNow(svcNow.Add(time.Duration(i) * time.Minute))
		h.createRoom(hostID)
	}
	h.setNow(svcNow.Add(time.Hour))

	rr := h.as(hostID, http.MethodGet, "/v1/rooms?limit=2", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"nextCursor"`
	}
	decodeInto(t, rr, &page)
	if len(page.Items) != 2 {
		t.Fatalf("%d items, want 2", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Fatal("no nextCursor with 5 rooms and a limit of 2")
	}

	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it["id"].(string)] = true
	}

	cursor := page.NextCursor
	for pages := 0; cursor != "" && pages < 5; pages++ {
		rr := h.as(hostID, http.MethodGet, "/v1/rooms?limit=2&cursor="+cursor, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("page: %d %s", rr.Code, rr.Body)
		}
		var next struct {
			Items      []map[string]any `json:"items"`
			NextCursor string           `json:"nextCursor"`
		}
		decodeInto(t, rr, &next)
		for _, it := range next.Items {
			id := it["id"].(string)
			if seen[id] {
				t.Errorf("room %s appeared on two pages; the keyset is not stable", id)
			}
			seen[id] = true
		}
		cursor = next.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("paged through %d rooms, want all 5", len(seen))
	}
}

func TestListRefusesOffsetPagination(t *testing.T) {
	// FR-59. A requirement that is only documented gets violated by the next
	// client written against a half-remembered convention.
	h := newRoomHarness(t, tiers{})
	h.createRoom(hostID)

	for _, q := range []string{"?offset=10", "?page=2", "?skip=5"} {
		rr := h.as(hostID, http.MethodGet, "/v1/rooms"+q, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rr.Code)
			continue
		}
		if p := roomProblemOf(t, rr); p.Code != "OFFSET_PAGINATION_UNSUPPORTED" {
			t.Errorf("%s gave code %q, want OFFSET_PAGINATION_UNSUPPORTED", q, p.Code)
		}
	}
}

func TestListRejectsAMalformedCursor(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	h.createRoom(hostID)

	rr := h.as(hostID, http.MethodGet, "/v1/rooms?cursor=not-a-cursor!!", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body)
	}
	if p := roomProblemOf(t, rr); p.Code != "CURSOR_INVALID" {
		t.Errorf("code = %q, want CURSOR_INVALID", p.Code)
	}
}

func TestEmptyListIsAnEmptyArrayNotNull(t *testing.T) {
	// Dart's List<T> decode throws on null where it accepts an empty list, so
	// a null here would crash the app instead of rendering §3.2's empty state.
	h := newRoomHarness(t, tiers{})
	rr := h.as(guestID, http.MethodGet, "/v1/rooms", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Errorf("an empty page rendered as %s, want \"items\":[]", rr.Body)
	}
}

// ---------------------------------------------------------------------------
// Publishing
// ---------------------------------------------------------------------------

func TestAFailedPublishDoesNotFailTheRequest(t *testing.T) {
	// The event is already committed to the log, so a delivery failure costs a
	// client one extra resync. Failing the request would report a mutation
	// that DID happen as if it had not — and the caller would retry into a
	// conflict.
	h := newRoomHarness(t, tiers{})
	h.publisher.err = errors.New("hub is down")

	rr := h.as(hostID, http.MethodPost, "/v1/rooms", map[string]any{"contentId": contentID})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 despite the publish failure; body = %s", rr.Code, rr.Body)
	}

	var created map[string]any
	decodeInto(t, rr, &created)
	// And the event must still be durable, which is what makes the resync
	// recovery possible in the first place.
	if got := len(h.store.Events(created["id"].(string))); got != 1 {
		t.Errorf("%d events in the log, want 1", got)
	}
}

func TestEveryMutationPublishesExactlyOnce(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)

	if rr := h.as(guestID, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": created["joinCode"]}); rr.Code != http.StatusOK {
		t.Fatalf("join: %d", rr.Code)
	}
	if rr := h.as(hostID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": "ARM"}); rr.Code != http.StatusOK {
		t.Fatalf("arm: %d", rr.Code)
	}
	if rr := h.as(hostID, http.MethodPost, "/v1/rooms/"+id+"/end", nil); rr.Code != http.StatusOK {
		t.Fatalf("end: %d", rr.Code)
	}

	published := h.publisher.all()
	stored := h.store.Events(id)
	if len(published) != len(stored) {
		t.Fatalf("%d events published but %d stored; the two must not diverge", len(published), len(stored))
	}
	for i := range stored {
		if published[i].Seq != stored[i].Seq || published[i].Type != stored[i].Type {
			t.Errorf("event %d: published %s/%d, stored %s/%d",
				i, published[i].Type, published[i].Seq, stored[i].Type, stored[i].Seq)
		}
		if published[i].ID != stored[i].ID {
			t.Errorf("event %d has a different id published than stored; FR-34 dedupe would break", i)
		}
	}
}

func TestARefusedMutationPublishesNothing(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)
	before := len(h.publisher.all())

	// Every one of these must be refused.
	h.as(guestID, http.MethodPost, "/v1/rooms/"+id+"/end", nil)
	h.as(guestID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": "ARM"})
	h.as(hostID, http.MethodPost, "/v1/rooms/"+id+"/transition", map[string]any{"event": "START"})
	h.as(guestID, http.MethodPost, "/v1/rooms/"+id+"/leave", nil)
	h.as(guestID, http.MethodPost, "/v1/rooms/join", map[string]any{"joinCode": "ZZZZZZ"})

	if got := len(h.publisher.all()); got != before {
		t.Errorf("%d events published by refused requests, want 0", got-before)
	}
}

func TestStorageFailureIsAFiveHundredWithoutLeakingDetail(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	h.store.FailNext["ContentExists"] = errors.New(`pq: relation "content" does not exist`)

	rr := h.as(hostID, http.MethodPost, "/v1/rooms", map[string]any{"contentId": contentID})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rr.Code, rr.Body)
	}
	for _, leak := range []string{"pq:", "relation", `"content"`} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Errorf("the 500 leaked %q:\n%s", leak, rr.Body)
		}
	}
}

func TestEveryRouteRequiresAnActor(t *testing.T) {
	h := newRoomHarness(t, tiers{})
	created := h.createRoom(hostID)
	id := created["id"].(string)

	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/rooms"},
		{http.MethodGet, "/v1/rooms"},
		{http.MethodPost, "/v1/rooms/join"},
		{http.MethodGet, "/v1/rooms/" + id},
		{http.MethodPost, "/v1/rooms/" + id + "/leave"},
		{http.MethodPost, "/v1/rooms/" + id + "/end"},
		{http.MethodPost, "/v1/rooms/" + id + "/transition"},
	} {
		var body any
		if tt.method == http.MethodPost {
			body = map[string]any{}
		}
		rr := h.as("", tt.method, tt.path, body)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without an actor = %d, want 401", tt.method, tt.path, rr.Code)
		}
	}
}

func TestFieldNameMatchingIsCaseInsensitive(t *testing.T) {
	// Documenting a real encoding/json behaviour that is easy to be surprised
	// by: DisallowUnknownFields does NOT catch a case-typo, because Go matches
	// JSON keys to struct tags case-insensitively. `contentID` is therefore a
	// KNOWN field, not an unknown one.
	//
	// Worth pinning down, because the natural assumption is the opposite, and
	// a client author debugging why `contentID` "works" here but not against a
	// stricter server deserves an answer in the test suite rather than in a
	// support thread.
	h := newRoomHarness(t, tiers{})
	rr := h.as(hostID, http.MethodPost, "/v1/rooms", map[string]any{"contentID": contentID})
	if rr.Code != http.StatusCreated {
		t.Errorf("contentID (wrong case) = %d, want 201; Go matches tags case-insensitively", rr.Code)
	}
}
