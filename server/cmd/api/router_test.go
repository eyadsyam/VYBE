package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
	"github.com/eyadsyam/vybe/server/internal/modules/identity/identitytest"
	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms/roomstest"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
	"github.com/eyadsyam/vybe/server/internal/platform/passwords"
)

// The end-to-end test for the composition root.
//
// It builds the SAME module graph newRouter is given in production — the same
// handlers, the same middleware order, the same narrow interfaces between
// modules — over in-memory stores rather than Postgres and Redis. What it
// exercises is the wiring: that auth runs before idempotency, that the rooms
// handler can see the identity claims, that the ticket issued by one module is
// redeemable by another, and that a room mutation made over HTTP arrives on a
// WebSocket opened by a different client.
//
// Those are exactly the joins that unit tests cannot reach, and exactly where
// a modular monolith goes wrong.

const contentID = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"

// stubPool satisfies poolPinger without a database.
type stubPool struct{ err error }

func (s stubPool) Ping(context.Context) error { return s.err }

// stubRedis satisfies redisPinger without a Redis.
//
// It exists so the DEGRADED path is assertable: ADR-009 says an unavailable
// Redis must leave the instance READY, and there is no way to test that
// without being able to make Redis unavailable.
type stubRedis struct{ err error }

func (s stubRedis) Ping(context.Context) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(context.Background())
	if s.err != nil {
		cmd.SetErr(s.err)
	} else {
		cmd.SetVal("PONG")
	}
	return cmd
}

type e2e struct {
	t       *testing.T
	server  *httptest.Server
	rooms   *roomstest.Store
	tickets *identitytest.TicketStore
	hub     *realtime.Hub
	setNow  func(time.Time)
}

var e2eNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func newE2E(t *testing.T) *e2e {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tokens, err := identity.NewTokenIssuer(priv, jwtIssuer, jwtAudience)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	breaches, err := identity.EmbeddedBreachSet()
	if err != nil {
		t.Fatalf("EmbeddedBreachSet: %v", err)
	}

	now := e2eNow
	clock := func() time.Time { return now }

	identityStore := identitytest.New()
	ticketStore := identitytest.NewTicketStore()
	identitySvc := identity.NewService(identityStore, tokens,
		identity.PasswordPolicy{Breaches: breaches}, passwords.TestParams)
	identitySvc.SetClock(clock)
	identityHandler := identity.NewHandler(identitySvc, ticketStore)
	identityHandler.SetClock(clock)

	roomStore := roomstest.New()
	roomStore.AddContent(contentID)
	hub := realtime.NewHub(logger)

	roomsSvc := rooms.NewService(roomStore, freeTier{})
	roomsSvc.SetClock(clock)
	roomsHandler := rooms.NewHandler(roomsSvc, hub, actorFromContext, logger)

	realtimeHandler := realtime.NewHandler(hub, memTicketRedeemer{ticketStore},
		roomStore, realtime.DefaultDeltaThreshold, logger)
	realtimeHandler.SetClock(clock)

	mods := &modules{
		identity: identityHandler,
		rooms:    roomsHandler,
		realtime: realtimeHandler,
		hub:      hub,
	}

	router := newRouter(nil, stubPool{}, stubRedis{err: errors.New("redis: connection refused")}, logger, mods, httpx.NewMemoryIdemStore())
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	return &e2e{
		t: t, server: srv, rooms: roomStore, tickets: ticketStore, hub: hub,
		setNow: func(v time.Time) { now = v },
	}
}

// freeTier is the EntitlementLookup. Everybody is free tier, which keeps
// capacity at 4 and makes the limit reachable in a test.
type freeTier struct{}

func (freeTier) EntitlementTier(context.Context, string) (string, error) { return "free", nil }

// memTicketRedeemer adapts the in-memory ticket store to realtime's interface,
// mirroring the production ticketRedeemer exactly.
type memTicketRedeemer struct{ store *identitytest.TicketStore }

func (m memTicketRedeemer) Redeem(ctx context.Context, plaintext string, now time.Time) (string, string, error) {
	t, err := m.store.Redeem(ctx, plaintext, now)
	if err != nil {
		return "", "", err
	}
	return t.UserID, t.SessionID, nil
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

type session struct {
	AccessToken string `json:"accessToken"`
	UserID      string
}

func (e *e2e) do(method, path, token string, body any, headers ...string) *http.Response {
	e.t.Helper()

	var rdr io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshalling: %v", err)
		}
		rdr = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
}

// register creates an account and returns its session.
func (e *e2e) register(handle, email string) session {
	e.t.Helper()
	resp := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": email, "password": "a sufficiently long passphrase",
		"handle": handle, "displayName": handle,
		"dateOfBirth": "2000-01-01", "locale": "ar", "region": "EG",
		"deviceName": "Pixel 8", "platform": "android",
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("register %s: %d %s", handle, resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	decodeBody(e.t, resp, &out)
	return session{AccessToken: out.AccessToken, UserID: out.User.ID}
}

// ---------------------------------------------------------------------------
// The full journey
// ---------------------------------------------------------------------------

func TestTheWholeJourneyFromSignupToARoomEventOnASocket(t *testing.T) {
	// Two accounts, a room, a join over HTTP, and a socket that receives the
	// event the join produced. Every module boundary in the system is crossed
	// at least once.
	e := newE2E(t)

	host := e.register("host_user", "host@example.com")
	guest := e.register("guest_user", "guest@example.com")

	// 1. The host opens a room.
	resp := e.do(http.MethodPost, "/v1/rooms", host.AccessToken,
		map[string]any{"contentId": contentID, "title": "ليلة أفلام"},
		httpx.IdempotencyHeader, "create-room-key-0001")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create room: %d %s", resp.StatusCode, body)
	}
	var room struct {
		ID       string `json:"id"`
		JoinCode string `json:"joinCode"`
		State    string `json:"state"`
	}
	decodeBody(t, resp, &room)
	if room.State != "LOBBY" || room.JoinCode == "" {
		t.Fatalf("room = %+v", room)
	}

	// 2. The guest asks for a WebSocket ticket, then connects.
	resp = e.do(http.MethodPost, "/v1/auth/ws-ticket", guest.AccessToken, nil)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("ws-ticket: %d %s", resp.StatusCode, body)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	decodeBody(t, resp, &ticket)

	// The guest must join before the socket will accept them — membership is
	// checked at upgrade, so this ordering is part of the contract.
	resp = e.do(http.MethodPost, "/v1/rooms/join", guest.AccessToken,
		map[string]any{"joinCode": room.JoinCode},
		httpx.IdempotencyHeader, "join-room-key-0001")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("join: %d %s", resp.StatusCode, body)
	}

	wsURL := "ws" + strings.TrimPrefix(e.server.URL, "http") +
		"/v1/ws?room=" + room.ID + "&ticket=" + ticket.Ticket
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dialling the socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var hello realtime.HelloFrame
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&hello); err != nil {
		t.Fatalf("reading HELLO: %v", err)
	}
	if hello.Type != realtime.FrameHello || hello.Room != room.ID {
		t.Fatalf("hello = %+v", hello)
	}
	// Two events so far: the room opening, and the guest joining.
	if hello.CurrentSeq != 2 {
		t.Errorf("hello.currentSeq = %d, want 2", hello.CurrentSeq)
	}

	waitFor(t, "the socket to register", func() bool { return e.hub.ConnCount(room.ID) == 1 })

	// 3. The host arms the room over HTTP. The guest must see it on the socket.
	//    This is the join that matters: an HTTP mutation in one module
	//    reaching a WebSocket owned by another.
	resp = e.do(http.MethodPost, "/v1/rooms/"+room.ID+"/transition", host.AccessToken,
		map[string]any{"event": "ARM"},
		httpx.IdempotencyHeader, "arm-room-key-0001")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("arm: %d %s", resp.StatusCode, body)
	}

	var event realtime.EventFrame
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("the guest never received the ARM event: %v", err)
	}
	if event.Type != realtime.FrameEvent {
		t.Fatalf("frame = %q, want EVENT", event.Type)
	}
	if event.Envelope.Type != realtime.EventRoomStateChanged {
		t.Errorf("event type = %q, want ROOM_STATE_CHANGED", event.Envelope.Type)
	}
	if event.Envelope.Seq != 3 {
		t.Errorf("event seq = %d, want 3", event.Envelope.Seq)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Envelope.Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload["from"] != "LOBBY" || payload["to"] != "READY" {
		t.Errorf("payload = %v, want LOBBY -> READY", payload)
	}
}

// ---------------------------------------------------------------------------
// Middleware ordering
// ---------------------------------------------------------------------------

func TestIdempotencyReplaysTheSameResponse(t *testing.T) {
	// FR-57 through the real middleware stack. A retried room creation must
	// return the FIRST response, not make a second room.
	e := newE2E(t)
	host := e.register("host_user", "host@example.com")

	const key = "retry-me-please-0001"
	body := map[string]any{"contentId": contentID, "title": "once"}

	first := e.do(http.MethodPost, "/v1/rooms", host.AccessToken, body, httpx.IdempotencyHeader, key)
	if first.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(first.Body)
		t.Fatalf("first create: %d %s", first.StatusCode, raw)
	}
	var firstRoom struct {
		ID string `json:"id"`
	}
	decodeBody(t, first, &firstRoom)

	second := e.do(http.MethodPost, "/v1/rooms", host.AccessToken, body, httpx.IdempotencyHeader, key)
	if second.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("replay: %d %s", second.StatusCode, raw)
	}
	if second.Header.Get(httpx.ReplayHeader) != "true" {
		t.Errorf("%s = %q, want true", httpx.ReplayHeader, second.Header.Get(httpx.ReplayHeader))
	}
	var secondRoom struct {
		ID string `json:"id"`
	}
	decodeBody(t, second, &secondRoom)
	if secondRoom.ID != firstRoom.ID {
		t.Errorf("the replay created a SECOND room (%s vs %s); FR-57 is not holding",
			secondRoom.ID, firstRoom.ID)
	}
}

func TestIdempotencyKeysAreScopedPerActor(t *testing.T) {
	// The reason Idempotency is mounted INSIDE RequireAuth. If it ran first,
	// the actor would be unknown, keys would share one namespace, and one
	// user's retry would replay another user's response — handing them a room
	// id they have no business seeing.
	e := newE2E(t)
	alice := e.register("alice_q", "alice@example.com")
	bob := e.register("bob_q", "bob@example.com")

	const sharedKey = "the-very-same-key-01"

	resp := e.do(http.MethodPost, "/v1/rooms", alice.AccessToken,
		map[string]any{"contentId": contentID, "title": "alice"},
		httpx.IdempotencyHeader, sharedKey)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("alice create: %d %s", resp.StatusCode, raw)
	}
	var aliceRoom struct {
		ID         string `json:"id"`
		HostUserID string `json:"hostUserId"`
	}
	decodeBody(t, resp, &aliceRoom)

	resp = e.do(http.MethodPost, "/v1/rooms", bob.AccessToken,
		map[string]any{"contentId": contentID, "title": "bob"},
		httpx.IdempotencyHeader, sharedKey)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("bob create: %d %s", resp.StatusCode, raw)
	}
	if resp.Header.Get(httpx.ReplayHeader) == "true" {
		t.Fatal("bob's request replayed alice's response; the key is not actor-scoped")
	}
	var bobRoom struct {
		ID         string `json:"id"`
		HostUserID string `json:"hostUserId"`
	}
	decodeBody(t, resp, &bobRoom)

	if bobRoom.ID == aliceRoom.ID {
		t.Error("bob received alice's room")
	}
	if bobRoom.HostUserID != bob.UserID {
		t.Errorf("bob's room is hosted by %q, want %q", bobRoom.HostUserID, bob.UserID)
	}
}

func TestUnauthenticatedRoomRequestsNeverReachIdempotency(t *testing.T) {
	// Auth first means an anonymous request is refused before it can occupy a
	// key at all.
	e := newE2E(t)
	resp := e.do(http.MethodPost, "/v1/rooms", "",
		map[string]any{"contentId": contentID}, httpx.IdempotencyHeader, "anon-key-00000001")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var p httpx.ProblemDetails
	decodeBody(t, resp, &p)
	if p.Code != "UNAUTHORIZED" {
		t.Errorf("code = %q, want UNAUTHORIZED", p.Code)
	}
}

func TestMutatingRoomRoutesRequireAnIdempotencyKey(t *testing.T) {
	e := newE2E(t)
	host := e.register("host_user", "host@example.com")

	resp := e.do(http.MethodPost, "/v1/rooms", host.AccessToken,
		map[string]any{"contentId": contentID})
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body = %s", resp.StatusCode, raw)
	}
	var p httpx.ProblemDetails
	decodeBody(t, resp, &p)
	if p.Code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Errorf("code = %q, want IDEMPOTENCY_KEY_REQUIRED", p.Code)
	}
}

// ---------------------------------------------------------------------------
// Router-level responses
// ---------------------------------------------------------------------------

func TestUnknownRoutesAndMethodsAreProblems(t *testing.T) {
	// A client must never have to parse chi's plain-text default.
	e := newE2E(t)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"unknown path", http.MethodGet, "/v1/nope", http.StatusNotFound, "NOT_FOUND"},
		{"unversioned path", http.MethodGet, "/rooms", http.StatusNotFound, "NOT_FOUND"},
		{"wrong method", http.MethodDelete, "/v1/auth/login", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := e.do(tt.method, tt.path, "", nil)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
			var p httpx.ProblemDetails
			decodeBody(t, resp, &p)
			if p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
			if p.TraceID == "" {
				t.Error("no traceId (FR-58)")
			}
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	e := newE2E(t)

	resp := e.do(http.MethodGet, "/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}

func TestReadinessFailsWhenPostgresIsDownButNotWhenRedisIs(t *testing.T) {
	// ADR-009 in one assertion. Postgres is the source of truth, so losing it
	// means serving nothing; Redis holds only reconstructible state, so losing
	// it is DEGRADED. Treating a cache outage as unready would take every
	// instance out of the load balancer and turn it into a total outage.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Redis is nil here, so rdb.Ping panics unless the handler is careful —
	// which is itself worth knowing, so the check uses a real client pointed
	// at a closed port instead.
	e := newE2E(t)
	resp := e.do(http.MethodGet, "/readyz", "", nil)
	// The stub pool answers Ping successfully and the redis client is absent,
	// so readiness must still report 200 with redis degraded.
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("readyz with redis unavailable = %d, want 200 (degraded, not unready); body = %s",
			resp.StatusCode, raw)
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	decodeBody(t, resp, &body)
	if !body.Ready {
		t.Error("ready = false with only Redis down; ADR-009 makes that degraded, not unready")
	}
	if !strings.Contains(body.Checks["redis"], "degraded") {
		t.Errorf("redis check = %q, want it to say degraded", body.Checks["redis"])
	}

	// Now break Postgres.
	mods := e.buildModulesForTest(t, logger)
	broken := newRouter(nil, stubPool{err: errors.New("connection refused")}, stubRedis{}, logger, mods, httpx.NewMemoryIdemStore())
	srv := httptest.NewServer(broken)
	t.Cleanup(srv.Close)

	r, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz with Postgres down = %d, want 503", r.StatusCode)
	}

	// Liveness must still pass: a liveness probe that fails on a database blip
	// gets the container killed, which does not fix the database and does lose
	// every WebSocket it was holding.
	live, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = live.Body.Close() }()
	if live.StatusCode != http.StatusOK {
		t.Errorf("healthz with Postgres down = %d, want 200 — liveness must not depend on the database", live.StatusCode)
	}
}

// buildModulesForTest returns a minimal module graph, for routers that only
// need the health endpoints.
func (e *e2e) buildModulesForTest(t *testing.T, logger *slog.Logger) *modules {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	tokens, err := identity.NewTokenIssuer(priv, jwtIssuer, jwtAudience)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	breaches, err := identity.EmbeddedBreachSet()
	if err != nil {
		t.Fatalf("EmbeddedBreachSet: %v", err)
	}
	store := identitytest.New()
	tickets := identitytest.NewTicketStore()
	svc := identity.NewService(store, tokens, identity.PasswordPolicy{Breaches: breaches}, passwords.TestParams)
	roomStore := roomstest.New()
	hub := realtime.NewHub(logger)
	return &modules{
		identity: identity.NewHandler(svc, tickets),
		rooms:    rooms.NewHandler(rooms.NewService(roomStore, freeTier{}), hub, actorFromContext, logger),
		realtime: realtime.NewHandler(hub, memTicketRedeemer{tickets}, roomStore, 0, logger),
		hub:      hub,
	}
}

func TestSignupThroughTheRealPolicyRejectsABreachedPassword(t *testing.T) {
	// Proves the EMBEDDED breach list is actually wired, not just loadable.
	// PasswordPolicy fails closed, so this also proves the set is non-empty at
	// the composition root.
	e := newE2E(t)
	resp := e.do(http.MethodPost, "/v1/auth/register", "", map[string]any{
		"email": "x@example.com", "password": "password1234",
		"handle": "xx_user", "displayName": "x", "dateOfBirth": "2000-01-01",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 422; body = %s", resp.StatusCode, raw)
	}
	var p httpx.ProblemDetails
	decodeBody(t, resp, &p)
	if p.Code != "PASSWORD_BREACHED" {
		t.Errorf("code = %q, want PASSWORD_BREACHED", p.Code)
	}
}

func TestClosingTheHubDisconnectsSockets(t *testing.T) {
	// The shutdown path. http.Server.Shutdown does not touch hijacked
	// connections, so without CloseAll a graceful restart waits the full
	// termination grace and every client sees an abrupt 1006.
	e := newE2E(t)
	host := e.register("host_user", "host@example.com")

	resp := e.do(http.MethodPost, "/v1/rooms", host.AccessToken,
		map[string]any{"contentId": contentID}, httpx.IdempotencyHeader, "create-key-00000001")
	var room struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &room)

	resp = e.do(http.MethodPost, "/v1/auth/ws-ticket", host.AccessToken, nil)
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	decodeBody(t, resp, &ticket)

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(e.server.URL, "http")+"/v1/ws?room="+room.ID+"&ticket="+ticket.Ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	waitFor(t, "the socket to register", func() bool { return e.hub.ConnCount(room.ID) == 1 })

	e.hub.CloseAll("the server is restarting; reconnect and resync")

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return // closed, as expected
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
