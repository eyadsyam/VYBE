package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

var wsNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// fakeTickets is a TicketRedeemer backed by a map, single-use.
type fakeTickets struct {
	mu      sync.Mutex
	tickets map[string]string // plaintext -> user id
	err     error
}

func newTickets() *fakeTickets {
	return &fakeTickets{tickets: map[string]string{}}
}

func (f *fakeTickets) issue(plaintext, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickets[plaintext] = userID
}

func (f *fakeTickets) Redeem(_ context.Context, plaintext string, _ time.Time) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", "", f.err
	}
	userID, ok := f.tickets[plaintext]
	if !ok {
		return "", "", errors.New("no such ticket")
	}
	// Single use: a ticket that survives redemption is a replayable
	// credential sitting in a URL.
	delete(f.tickets, plaintext)
	return userID, "session-" + userID, nil
}

// fakeRooms is a RoomReader over an in-memory log.
type fakeRooms struct {
	mu             sync.Mutex
	members        map[string]map[string]bool // room -> user -> in
	events         map[string][]realtime.Envelope
	oldestRetained map[string]int64
	snapshot       json.RawMessage
	snapshotErr    error
	eventsErr      error
	oldestErr      error
}

func newRooms() *fakeRooms {
	return &fakeRooms{
		members:        map[string]map[string]bool{},
		events:         map[string][]realtime.Envelope{},
		oldestRetained: map[string]int64{},
		snapshot:       json.RawMessage(`{"state":"LOBBY","participants":[]}`),
	}
}

func (f *fakeRooms) addMember(roomID, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members[roomID] == nil {
		f.members[roomID] = map[string]bool{}
	}
	f.members[roomID][userID] = true
}

func (f *fakeRooms) appendEvents(roomID string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := int64(len(f.events[roomID]))
	for i := int64(1); i <= int64(n); i++ {
		f.events[roomID] = append(f.events[roomID], realtime.Envelope{
			V: realtime.EnvelopeVersion, ID: fmt.Sprintf("evt-%d", start+i),
			Room: roomID, Seq: start + i, Type: realtime.EventChatMessage,
			TS: wsNow, Payload: json.RawMessage(`{}`),
		})
	}
}

func (f *fakeRooms) Membership(_ context.Context, roomID, userID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.members[roomID][userID] {
		return 0, realtime.ErrRoomNotFound
	}
	return int64(len(f.events[roomID])), nil
}

func (f *fakeRooms) EventsSince(_ context.Context, roomID string, fromSeq, toSeq int64) ([]realtime.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	var out []realtime.Envelope
	for _, e := range f.events[roomID] {
		if e.Seq > fromSeq && e.Seq <= toSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeRooms) OldestRetainedSeq(_ context.Context, roomID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.oldestErr != nil {
		return 0, f.oldestErr
	}
	if v, ok := f.oldestRetained[roomID]; ok {
		return v, nil
	}
	return 1, nil
}

func (f *fakeRooms) Snapshot(_ context.Context, _ string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	return f.snapshot, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type wsHarness struct {
	t       *testing.T
	server  *httptest.Server
	hub     *realtime.Hub
	tickets *fakeTickets
	rooms   *fakeRooms
}

func newWSHarness(t *testing.T, threshold int) *wsHarness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := realtime.NewHub(logger)
	tickets := newTickets()
	rooms := newRooms()

	h := realtime.NewHandler(hub, tickets, rooms, threshold, logger)
	h.SetClock(func() time.Time { return wsNow })

	mux := http.NewServeMux()
	mux.Handle("/v1/ws", httpx.Trace(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &wsHarness{t: t, server: srv, hub: hub, tickets: tickets, rooms: rooms}
}

func (h *wsHarness) wsURL(query string) string {
	return "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/ws" + query
}

// dial opens a socket and returns it plus the HELLO frame.
func (h *wsHarness) dial(ticket, roomID string) (*websocket.Conn, realtime.HelloFrame) {
	h.t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(
		h.wsURL("?room="+roomID+"&ticket="+ticket), nil)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		h.t.Fatalf("dial: %v (%s)", err, body)
	}
	h.t.Cleanup(func() { _ = conn.Close() })

	var hello realtime.HelloFrame
	if err := readFrameInto(conn, &hello); err != nil {
		h.t.Fatalf("reading HELLO: %v", err)
	}
	if hello.Type != realtime.FrameHello {
		h.t.Fatalf("first frame was %q, want HELLO", hello.Type)
	}
	return conn, hello
}

func readFrameInto(conn *websocket.Conn, dst any) error {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return conn.ReadJSON(dst)
}

// frameType peeks at a frame's type without consuming its body.
func readRaw(conn *websocket.Conn) (map[string]json.RawMessage, string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, "", err
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, "", err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", err
	}
	var typ string
	if v, ok := raw["type"]; ok {
		_ = json.Unmarshal(v, &typ)
	}
	return raw, typ, nil
}

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

func TestUpgradeRequiresAValidTicket(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantCode   string
	}{
		{"no ticket", "?room=room-1", http.StatusUnauthorized, "WS_TICKET_INVALID"},
		{"unknown ticket", "?room=room-1&ticket=never-issued", http.StatusUnauthorized, "WS_TICKET_INVALID"},
		{"no room", "?ticket=t", http.StatusBadRequest, "ROOM_REQUIRED"},
		// FR-5: the whole reason tickets exist.
		{"access token in query", "?room=room-1&access_token=abc", http.StatusBadRequest, "TOKEN_IN_QUERY"},
		{"token in query", "?room=room-1&token=abc", http.StatusBadRequest, "TOKEN_IN_QUERY"},
		{"jwt in query", "?room=room-1&jwt=abc", http.StatusBadRequest, "TOKEN_IN_QUERY"},
		{"authorization in query", "?room=room-1&authorization=abc", http.StatusBadRequest, "TOKEN_IN_QUERY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(h.server.URL + "/v1/ws" + tt.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			// The failure must arrive as a normal problem response, BEFORE the
			// upgrade. A client forced to parse a close frame to learn it was
			// unauthorised has a far worse time.
			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
			var p httpx.ProblemDetails
			if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
				t.Fatalf("decoding problem: %v", err)
			}
			if p.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", p.Code, tt.wantCode)
			}
			if p.TraceID == "" {
				t.Error("no traceId on the problem (FR-58)")
			}
		})
	}
}

func TestUpgradeRefusesABearerHeader(t *testing.T) {
	// The ticket is the only credential on this path, so there is exactly one
	// code path to audit.
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("good-ticket", "user-a")

	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/v1/ws?room=room-1&ticket=good-ticket", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer some.access.token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestATicketIsSingleUse(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("one-shot", "user-a")

	conn, _ := h.dial("one-shot", "room-1")
	_ = conn

	// The same ticket again must fail. A ticket that survives redemption is a
	// replayable credential sitting in a URL and in every log that saw it.
	resp, err := http.Get(h.server.URL + "/v1/ws?room=room-1&ticket=one-shot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("reusing a ticket = %d, want 401", resp.StatusCode)
	}
}

func TestUpgradeRefusesANonMember(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t-b", "user-b") // valid ticket, wrong room

	resp, err := http.Get(h.server.URL + "/v1/ws?room=room-1&ticket=t-b")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHelloCarriesTheRoomsPosition(t *testing.T) {
	// So a client knows, without asking, whether it missed anything — saving a
	// RESYNC round trip on the common case of a clean reconnect.
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 42)
	h.tickets.issue("t", "user-a")

	_, hello := h.dial("t", "room-1")

	if hello.Room != "room-1" {
		t.Errorf("hello.room = %q", hello.Room)
	}
	if hello.CurrentSeq != 42 {
		t.Errorf("hello.currentSeq = %d, want 42", hello.CurrentSeq)
	}
	if hello.ServerTime != wsNow.UnixMilli() {
		t.Errorf("hello.serverTime = %d, want %d", hello.ServerTime, wsNow.UnixMilli())
	}
	if hello.HeartbeatSeconds <= 0 {
		t.Error("hello carries no heartbeat interval; the client must guess its read deadline")
	}
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

func TestAConnectedClientReceivesPublishedEvents(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	want := realtime.Envelope{
		V: realtime.EnvelopeVersion, ID: "evt-1", Room: "room-1", Seq: 1,
		Type: realtime.EventChatMessage, TS: wsNow, Payload: json.RawMessage(`{"body":"مرحبا"}`),
	}
	if err := h.hub.Publish(context.Background(), want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var frame realtime.EventFrame
	if err := readFrameInto(conn, &frame); err != nil {
		t.Fatalf("reading event: %v", err)
	}
	if frame.Type != realtime.FrameEvent {
		t.Errorf("type = %q, want EVENT", frame.Type)
	}
	if frame.Envelope.Seq != 1 || frame.Envelope.ID != "evt-1" {
		t.Errorf("envelope = %+v", frame.Envelope)
	}
	// Arabic must survive the socket, not just the HTTP layer.
	if !strings.Contains(string(frame.Envelope.Payload), "مرحبا") {
		t.Errorf("payload = %s; Arabic did not survive the round trip", frame.Envelope.Payload)
	}
}

func TestPongEchoesBothClocks(t *testing.T) {
	// ADR-002: the client computes offset = ((t1-t0) + (t2-t3)) / 2, which is
	// only possible if its own timestamp comes back unmodified. A pong that
	// carried only the server's time would let the client measure latency but
	// not correct its clock — and correcting the clock is the entire point.
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	const clientTime = 1756209600123
	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FramePing, ClientTime: clientTime}); err != nil {
		t.Fatalf("writing PING: %v", err)
	}

	var pong realtime.PongFrame
	if err := readFrameInto(conn, &pong); err != nil {
		t.Fatalf("reading PONG: %v", err)
	}
	if pong.Type != realtime.FramePong {
		t.Errorf("type = %q, want PONG", pong.Type)
	}
	if pong.ClientTime != clientTime {
		t.Errorf("clientTime = %d, want it echoed unmodified as %d", pong.ClientTime, clientTime)
	}
	if pong.ServerTime != wsNow.UnixMilli() {
		t.Errorf("serverTime = %d, want %d", pong.ServerTime, wsNow.UnixMilli())
	}
}

// ---------------------------------------------------------------------------
// Resync
// ---------------------------------------------------------------------------

func TestResyncSendsADeltaWhenTheGapIsSmall(t *testing.T) {
	// AC-8's shape: last_seq 1400, current 1450 → events 1401..1450.
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 1450)
	h.tickets.issue("t", "user-a")
	conn, hello := h.dial("t", "room-1")

	if hello.CurrentSeq != 1450 {
		t.Fatalf("hello.currentSeq = %d, want 1450", hello.CurrentSeq)
	}

	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 1400}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}

	var delta realtime.DeltaFrame
	if err := readFrameInto(conn, &delta); err != nil {
		t.Fatalf("reading DELTA: %v", err)
	}
	if delta.Type != realtime.FrameDelta {
		t.Fatalf("type = %q, want DELTA", delta.Type)
	}
	if delta.From != 1401 || delta.To != 1450 {
		t.Errorf("delta covers %d..%d, want 1401..1450", delta.From, delta.To)
	}
	if len(delta.Events) != 50 {
		t.Fatalf("%d events, want 50", len(delta.Events))
	}
	// Contiguous and in order, or the client applies a hole and believes it is
	// caught up.
	for i, e := range delta.Events {
		if e.Seq != int64(1401+i) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, 1401+i)
		}
	}
}

func TestResyncSendsASnapshotWhenTheGapIsLarge(t *testing.T) {
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 1000)
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	// A gap of 999, far over the threshold of 200.
	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 1}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}

	var snap realtime.SnapshotFrame
	if err := readFrameInto(conn, &snap); err != nil {
		t.Fatalf("reading SNAPSHOT: %v", err)
	}
	if snap.Type != realtime.FrameSnapshot {
		t.Fatalf("type = %q, want SNAPSHOT", snap.Type)
	}
	if snap.CurrentSeq != 1000 {
		t.Errorf("currentSeq = %d, want 1000", snap.CurrentSeq)
	}
	if snap.Reason == "" {
		t.Error("no reason on the snapshot; a spike in snapshots must be diagnosable")
	}
	if !strings.Contains(string(snap.State), "LOBBY") {
		t.Errorf("state = %s, want the room's state", snap.State)
	}
}

func TestResyncSendsASnapshotWhenEventsHaveAgedOut(t *testing.T) {
	// Retention, not size, is the binding constraint here. A small gap whose
	// events no longer exist cannot be served a delta at ANY threshold, and
	// conflating the two cases would ship a delta with a hole in it.
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 100)
	h.rooms.oldestRetained["room-1"] = 90 // 1..89 have been pruned
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	// A gap of only 90 — well under the threshold — but the events are gone.
	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 10}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}

	_, typ, err := readRaw(conn)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if typ != realtime.FrameSnapshot {
		t.Errorf("frame = %q, want SNAPSHOT — the events it missed no longer exist", typ)
	}
}

func TestResyncOnAnUpToDateClientSendsAnEmptyDelta(t *testing.T) {
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 10)
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 10}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}

	var delta realtime.DeltaFrame
	if err := readFrameInto(conn, &delta); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if delta.Type != realtime.FrameDelta {
		t.Fatalf("type = %q, want DELTA", delta.Type)
	}
	if len(delta.Events) != 0 {
		t.Errorf("%d events for an up-to-date client, want 0", len(delta.Events))
	}
	if delta.Applied {
		t.Error("applied = true with nothing to apply")
	}
	// And it must be `[]`, not null — the client decodes into a List.
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(raw), `"events":null`) {
		t.Error("events serialised as null; a Dart List<T> decode throws on that")
	}
}

func TestResyncWithASeqAheadOfTheServerSendsASnapshot(t *testing.T) {
	// The client's state is corrupt, or it is talking to a stale replica.
	// Either way a snapshot is the only safe answer.
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 10)
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 999}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}
	_, typ, err := readRaw(conn)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if typ != realtime.FrameSnapshot {
		t.Errorf("frame = %q, want SNAPSHOT", typ)
	}
}

func TestResyncFallsBackToASnapshotWhenTheDeltaReadFails(t *testing.T) {
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 10)
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	h.rooms.mu.Lock()
	h.rooms.eventsErr = errors.New("query timeout")
	h.rooms.mu.Unlock()

	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 5}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}
	_, typ, err := readRaw(conn)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if typ != realtime.FrameSnapshot {
		t.Errorf("frame = %q, want a SNAPSHOT fallback rather than an error", typ)
	}
}

func TestResyncReportsAnErrorWhenEvenTheSnapshotFails(t *testing.T) {
	h := newWSHarness(t, realtime.DefaultDeltaThreshold)
	h.rooms.addMember("room-1", "user-a")
	h.rooms.appendEvents("room-1", 1000)
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	h.rooms.mu.Lock()
	h.rooms.snapshotErr = errors.New("database down")
	h.rooms.mu.Unlock()

	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FrameResync, LastSeq: 1}); err != nil {
		t.Fatalf("writing RESYNC: %v", err)
	}

	var frame realtime.ErrorFrame
	if err := readFrameInto(conn, &frame); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if frame.Type != realtime.FrameError || frame.Code != realtime.ErrCodeResyncFailed {
		t.Errorf("frame = %+v, want an ERROR/RESYNC_FAILED", frame)
	}
	// The socket must stay open: the client can retry, and dropping it would
	// cause a reconnect storm during exactly the outage that caused this.
	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FramePing, ClientTime: 1}); err != nil {
		t.Errorf("the socket was closed after a resync failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Protocol tolerance
// ---------------------------------------------------------------------------

func TestAnUnknownFrameIsAnsweredNotFatal(t *testing.T) {
	// FR-33's tolerance runs both ways: a newer client speaking a frame this
	// server does not know should not lose its connection over it.
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	if err := conn.WriteJSON(map[string]any{"type": "TELEPORT", "destination": "mars"}); err != nil {
		t.Fatalf("writing: %v", err)
	}

	var frame realtime.ErrorFrame
	if err := readFrameInto(conn, &frame); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if frame.Code != realtime.ErrCodeUnknownFrame {
		t.Errorf("code = %q, want UNKNOWN_FRAME", frame.Code)
	}

	// Still usable.
	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FramePing, ClientTime: 7}); err != nil {
		t.Fatalf("the socket died over an unknown frame: %v", err)
	}
	var pong realtime.PongFrame
	if err := readFrameInto(conn, &pong); err != nil {
		t.Fatalf("reading PONG after an unknown frame: %v", err)
	}
	if pong.ClientTime != 7 {
		t.Errorf("clientTime = %d, want 7", pong.ClientTime)
	}
}

func TestMalformedJSONIsAnsweredNotFatal(t *testing.T) {
	// Disconnecting over one bad frame turns a client-side encoding bug into a
	// reconnect storm.
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type": broken`)); err != nil {
		t.Fatalf("writing: %v", err)
	}

	var frame realtime.ErrorFrame
	if err := readFrameInto(conn, &frame); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if frame.Code != realtime.ErrCodeBadFrame {
		t.Errorf("code = %q, want BAD_FRAME", frame.Code)
	}

	if err := conn.WriteJSON(realtime.ClientFrame{Type: realtime.FramePing}); err != nil {
		t.Errorf("the socket died over malformed JSON: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestDisconnectingRemovesTheClientFromTheHub(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")

	waitFor(t, "the client to register", func() bool { return h.hub.ConnCount("room-1") == 1 })

	_ = conn.Close()

	// Otherwise the hub keeps trying to send to a dead socket and presence
	// reports a ghost forever.
	waitFor(t, "the client to be reaped", func() bool { return h.hub.ConnCount("room-1") == 0 })
	if h.hub.IsPresent("room-1", "user-a") {
		t.Error("a disconnected user is still reported as present")
	}
}

func TestTwoDevicesForOneUserBothReceive(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t1", "user-a")
	h.tickets.issue("t2", "user-a")

	phone, _ := h.dial("t1", "room-1")
	tablet, _ := h.dial("t2", "room-1")

	waitFor(t, "both sockets to register", func() bool { return h.hub.ConnCount("room-1") == 2 })

	// One person present, two sockets.
	if present := h.hub.PresentUserIDs("room-1"); len(present) != 1 {
		t.Errorf("PresentUserIDs = %v, want one distinct user", present)
	}

	e := realtime.Envelope{
		V: realtime.EnvelopeVersion, ID: "evt-1", Room: "room-1", Seq: 1,
		Type: realtime.EventChatMessage, TS: wsNow, Payload: json.RawMessage(`{}`),
	}
	if err := h.hub.Publish(context.Background(), e); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for name, c := range map[string]*websocket.Conn{"phone": phone, "tablet": tablet} {
		var frame realtime.EventFrame
		if err := readFrameInto(c, &frame); err != nil {
			t.Errorf("%s did not receive the event: %v", name, err)
			continue
		}
		if frame.Envelope.ID != "evt-1" {
			t.Errorf("%s got %+v", name, frame.Envelope)
		}
	}
}

func TestCloseRoomDisconnectsLiveSockets(t *testing.T) {
	h := newWSHarness(t, 0)
	h.rooms.addMember("room-1", "user-a")
	h.tickets.issue("t", "user-a")
	conn, _ := h.dial("t", "room-1")
	waitFor(t, "the client to register", func() bool { return h.hub.ConnCount("room-1") == 1 })

	h.hub.CloseRoom("room-1", "the host ended the party")

	// The client must see the socket close rather than hang.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
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
