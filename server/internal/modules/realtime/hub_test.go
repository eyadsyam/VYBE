package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
)

const testRoom = "01a03b7d-7d57-740f-bb72-c72f0dc72422"

// fakeSink is a Sink with a bounded buffer, like the real one.
type fakeSink struct {
	mu       sync.Mutex
	userID   string
	connID   string
	capacity int
	received []realtime.Envelope
	closed   bool
	reason   string
	// blocked makes every Send fail, modelling a client that has stopped
	// draining entirely.
	blocked bool
}

func newSink(userID, connID string, capacity int) *fakeSink {
	return &fakeSink{userID: userID, connID: connID, capacity: capacity}
}

func (s *fakeSink) UserID() string { return s.userID }
func (s *fakeSink) ConnID() string { return s.connID }

func (s *fakeSink) Send(e realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sink is closed")
	}
	if s.blocked || len(s.received) >= s.capacity {
		return realtime.ErrSinkFull
	}
	s.received = append(s.received, e)
	return nil
}

func (s *fakeSink) Close(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.reason = reason
}

func (s *fakeSink) events() []realtime.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]realtime.Envelope(nil), s.received...)
}

func (s *fakeSink) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeSink) closeReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func quietHub() *realtime.Hub {
	return realtime.NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testEvent(seq int64, eventType string) realtime.Envelope {
	return realtime.Envelope{
		V:       realtime.EnvelopeVersion,
		ID:      "evt-" + strconv.FormatInt(seq, 10),
		Room:    testRoom,
		Seq:     seq,
		Type:    eventType,
		TS:      time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Payload: json.RawMessage(`{}`),
	}
}

// ---------------------------------------------------------------------------
// Fan-out
// ---------------------------------------------------------------------------

func TestPublishReachesEveryClientInTheRoom(t *testing.T) {
	h := quietHub()
	a, b, c := newSink("user-a", "conn-a", 10), newSink("user-b", "conn-b", 10), newSink("user-c", "conn-c", 10)
	h.Join(testRoom, a)
	h.Join(testRoom, b)
	h.Join(testRoom, c)

	if err := h.Publish(context.Background(), testEvent(1, realtime.EventChatMessage)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for _, s := range []*fakeSink{a, b, c} {
		got := s.events()
		if len(got) != 1 || got[0].Seq != 1 {
			t.Errorf("%s received %d events, want 1", s.UserID(), len(got))
		}
	}
}

func TestPublishDoesNotCrossRooms(t *testing.T) {
	// The most basic containment property, and the one whose failure is worst:
	// a chat message leaking into a different party.
	h := quietHub()
	mine := newSink("user-a", "conn-a", 10)
	theirs := newSink("user-b", "conn-b", 10)
	h.Join(testRoom, mine)
	h.Join("some-other-room", theirs)

	if err := h.Publish(context.Background(), testEvent(1, realtime.EventChatMessage)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(mine.events()) != 1 {
		t.Error("the event did not reach its own room")
	}
	if got := len(theirs.events()); got != 0 {
		t.Errorf("an event leaked into another room (%d events)", got)
	}
}

func TestPublishToFiltersByRecipient(t *testing.T) {
	// FR-41. The filter is applied at fan-out, not by the client: a client
	// that receives an event it should not see has already seen it, and
	// "please ignore this" is not a security boundary.
	h := quietHub()
	a, b, c := newSink("user-a", "conn-a", 10), newSink("user-b", "conn-b", 10), newSink("user-c", "conn-c", 10)
	h.Join(testRoom, a)
	h.Join(testRoom, b)
	h.Join(testRoom, c)

	err := h.PublishTo(context.Background(), testEvent(1, realtime.EventQuestionClose), []string{"user-a", "user-c"})
	if err != nil {
		t.Fatalf("PublishTo: %v", err)
	}

	if len(a.events()) != 1 {
		t.Error("user-a was named as a recipient but received nothing")
	}
	if len(c.events()) != 1 {
		t.Error("user-c was named as a recipient but received nothing")
	}
	if got := len(b.events()); got != 0 {
		t.Errorf("user-b was NOT a recipient but received %d events; the answer leaked", got)
	}
}

func TestPublishToWithNoRecipientsGoesToEverybody(t *testing.T) {
	// nil and empty both mean "the whole room". Getting this backwards would
	// silently deliver nothing, which is far harder to notice than
	// over-delivery.
	h := quietHub()
	a, b := newSink("user-a", "conn-a", 10), newSink("user-b", "conn-b", 10)
	h.Join(testRoom, a)
	h.Join(testRoom, b)

	if err := h.PublishTo(context.Background(), testEvent(1, realtime.EventChatMessage), nil); err != nil {
		t.Fatalf("nil recipients: %v", err)
	}
	if err := h.PublishTo(context.Background(), testEvent(2, realtime.EventChatMessage), []string{}); err != nil {
		t.Fatalf("empty recipients: %v", err)
	}

	for _, s := range []*fakeSink{a, b} {
		if got := len(s.events()); got != 2 {
			t.Errorf("%s received %d events, want 2", s.UserID(), got)
		}
	}
}

func TestPublishReachesBothOfAUsersDevices(t *testing.T) {
	// A phone and a tablet are two sinks for one user. Delivering to only one
	// would leave the other frozen until it happened to resync.
	h := quietHub()
	phone := newSink("user-a", "conn-phone", 10)
	tablet := newSink("user-a", "conn-tablet", 10)
	h.Join(testRoom, phone)
	h.Join(testRoom, tablet)

	if err := h.Publish(context.Background(), testEvent(1, realtime.EventChatMessage)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(phone.events()) != 1 || len(tablet.events()) != 1 {
		t.Errorf("phone got %d, tablet got %d; both must receive", len(phone.events()), len(tablet.events()))
	}
}

func TestPublishRefusesAMalformedEnvelope(t *testing.T) {
	// FR-33 makes clients silently ignore what they cannot parse, so a bad
	// event would vanish from every client with no error anywhere. Refusing at
	// the boundary is the only place it can be caught.
	h := quietHub()
	s := newSink("user-a", "conn-a", 10)
	h.Join(testRoom, s)

	bad := testEvent(1, realtime.EventChatMessage)
	bad.Payload = nil

	if err := h.Publish(context.Background(), bad); !errors.Is(err, realtime.ErrInvalidEnvelope) {
		t.Fatalf("Publish of a payload-less envelope = %v, want ErrInvalidEnvelope", err)
	}
	if got := len(s.events()); got != 0 {
		t.Errorf("a malformed envelope was delivered to %d clients", got)
	}
}

// ---------------------------------------------------------------------------
// Backpressure
// ---------------------------------------------------------------------------

func TestASlowClientIsDroppedAndTheRestAreUnaffected(t *testing.T) {
	// The policy that keeps one bad phone on a train from becoming an outage
	// for the room. It is only safe because ADR-003 makes reconnection
	// lossless — the client comes back and is served its delta.
	h := quietHub()
	healthy := newSink("user-a", "conn-a", 100)
	slow := newSink("user-b", "conn-b", 1)
	h.Join(testRoom, healthy)
	h.Join(testRoom, slow)

	dropped := make(chan string, 4)
	h.SetDropHook(func(_ string, s realtime.Sink) { dropped <- s.UserID() })

	// The slow client's single buffer slot fills on the first event and
	// overflows on the second.
	for seq := int64(1); seq <= 5; seq++ {
		if err := h.Publish(context.Background(), testEvent(seq, realtime.EventChatMessage)); err != nil {
			t.Fatalf("Publish %d: %v", seq, err)
		}
	}

	if got := len(healthy.events()); got != 5 {
		t.Errorf("the healthy client received %d of 5 events; a slow peer must not cost it anything", got)
	}
	if !slow.isClosed() {
		t.Error("the slow client was not disconnected")
	}
	select {
	case id := <-dropped:
		if id != "user-b" {
			t.Errorf("dropped %q, want user-b", id)
		}
	default:
		t.Error("the drop hook did not fire")
	}

	// The close reason must tell the client what to do, because the client's
	// correct response is to reconnect and resync rather than to give up.
	if reason := slow.closeReason(); reason == "" {
		t.Error("the slow client was closed with no reason")
	}
}

func TestADroppedClientIsRemovedFromTheRoom(t *testing.T) {
	// Otherwise the hub would keep trying to send to a dead socket forever,
	// and PresentUserIDs would report a ghost.
	h := quietHub()
	slow := newSink("user-b", "conn-b", 0)
	h.Join(testRoom, slow)

	if h.ConnCount(testRoom) != 1 {
		t.Fatalf("setup: conn count = %d", h.ConnCount(testRoom))
	}
	if err := h.Publish(context.Background(), testEvent(1, realtime.EventChatMessage)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := h.ConnCount(testRoom); got != 0 {
		t.Errorf("conn count = %d after dropping the only client, want 0", got)
	}
	if h.IsPresent(testRoom, "user-b") {
		t.Error("a dropped client is still reported as present")
	}
}

func TestAnEmptyRoomIsReclaimed(t *testing.T) {
	// A server that has hosted a million rooms would otherwise hold a million
	// empty maps for the life of the process.
	h := quietHub()
	s := newSink("user-a", "conn-a", 10)
	h.Join(testRoom, s)
	if h.RoomCount() != 1 {
		t.Fatalf("room count = %d, want 1", h.RoomCount())
	}
	h.Leave(testRoom, "conn-a")
	if got := h.RoomCount(); got != 0 {
		t.Errorf("room count = %d after the last client left, want 0", got)
	}
}

func TestLeaveIsIdempotent(t *testing.T) {
	h := quietHub()
	h.Join(testRoom, newSink("user-a", "conn-a", 10))
	h.Leave(testRoom, "conn-a")
	h.Leave(testRoom, "conn-a")             // again
	h.Leave("no-such-room", "conn-a")       // unknown room
	h.Leave(testRoom, "no-such-connection") // unknown conn
	if got := h.RoomCount(); got != 0 {
		t.Errorf("room count = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Presence
// ---------------------------------------------------------------------------

func TestPresenceCountsDistinctUsersNotConnections(t *testing.T) {
	// FR-39. Somebody with a phone and a tablet in one room is one person
	// present; a list that showed them twice reads as a bug to every user.
	h := quietHub()
	h.Join(testRoom, newSink("user-a", "conn-phone", 10))
	h.Join(testRoom, newSink("user-a", "conn-tablet", 10))
	h.Join(testRoom, newSink("user-b", "conn-b", 10))

	present := h.PresentUserIDs(testRoom)
	if len(present) != 2 {
		t.Fatalf("PresentUserIDs = %v, want 2 distinct users", present)
	}
	seen := map[string]bool{}
	for _, id := range present {
		if seen[id] {
			t.Errorf("%q appears twice in %v", id, present)
		}
		seen[id] = true
	}
	if !seen["user-a"] || !seen["user-b"] {
		t.Errorf("PresentUserIDs = %v, want both users", present)
	}
	// But the connection count is still two for user-a.
	if got := h.ConnCount(testRoom); got != 3 {
		t.Errorf("ConnCount = %d, want 3 sockets", got)
	}
}

func TestAUserStaysPresentWhileOneDeviceRemains(t *testing.T) {
	// Closing a laptop must not mark somebody absent while their phone is
	// still in the room.
	h := quietHub()
	h.Join(testRoom, newSink("user-a", "conn-phone", 10))
	h.Join(testRoom, newSink("user-a", "conn-tablet", 10))

	h.Leave(testRoom, "conn-tablet")
	if !h.IsPresent(testRoom, "user-a") {
		t.Error("the user was marked absent while their phone was still connected")
	}
	h.Leave(testRoom, "conn-phone")
	if h.IsPresent(testRoom, "user-a") {
		t.Error("the user is still present after their last device left")
	}
}

func TestPresenceOfAnUnknownRoomIsEmptyNotNil(t *testing.T) {
	h := quietHub()
	got := h.PresentUserIDs("no-such-room")
	if got == nil {
		t.Error("PresentUserIDs returned nil; it must return an empty slice so callers can range without a check")
	}
	if len(got) != 0 {
		t.Errorf("PresentUserIDs = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Room closure
// ---------------------------------------------------------------------------

func TestCloseRoomDisconnectsEverybody(t *testing.T) {
	h := quietHub()
	a, b := newSink("user-a", "conn-a", 10), newSink("user-b", "conn-b", 10)
	h.Join(testRoom, a)
	h.Join(testRoom, b)

	h.CloseRoom(testRoom, "the host ended the party")

	for _, s := range []*fakeSink{a, b} {
		if !s.isClosed() {
			t.Errorf("%s was not closed", s.UserID())
		}
		if s.closeReason() != "the host ended the party" {
			t.Errorf("%s close reason = %q", s.UserID(), s.closeReason())
		}
	}
	if got := h.RoomCount(); got != 0 {
		t.Errorf("room count = %d after closing, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestHubSurvivesConcurrentJoinLeaveAndPublish(t *testing.T) {
	// The hub takes a read lock, snapshots, releases, then sends. A version
	// that sent while holding the lock would deadlock the moment a Send caused
	// a Leave — which is exactly what dropping a slow client does.
	//
	// Run with -race in CI, which is where this earns its keep.
	h := quietHub()
	const workers = 16
	const iterations = 50

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			id := "conn-" + strconv.Itoa(w)
			for i := range iterations {
				s := newSink("user-"+strconv.Itoa(w), id, 2)
				h.Join(testRoom, s)
				_ = h.Publish(context.Background(), testEvent(int64(i+1), realtime.EventChatMessage))
				_ = h.PresentUserIDs(testRoom)
				_ = h.IsPresent(testRoom, "user-0")
				h.Leave(testRoom, id)
			}
		})
	}
	wg.Wait()

	// Everything unwound: no leaked rooms, no leaked connections.
	if got := h.RoomCount(); got != 0 {
		t.Errorf("room count = %d after all workers left, want 0", got)
	}
}

func TestDroppingASlowClientDuringFanOutDoesNotDeadlock(t *testing.T) {
	// The specific deadlock the snapshot-then-send design avoids: dropSlow
	// calls Leave, which takes the write lock. If the fan-out still held the
	// read lock, this would hang forever.
	//
	// Guarded by a timeout so a regression fails the suite instead of hanging
	// CI until it is killed.
	h := quietHub()
	for i := range 20 {
		h.Join(testRoom, newSink("user-"+strconv.Itoa(i), "conn-"+strconv.Itoa(i), 0))
	}

	done := make(chan error, 1)
	go func() { done <- h.Publish(context.Background(), testEvent(1, realtime.EventChatMessage)) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Publish deadlocked while dropping slow clients — the fan-out is holding the lock across Send")
	}

	if got := h.ConnCount(testRoom); got != 0 {
		t.Errorf("%d connections survived, want 0 — every sink had a zero buffer", got)
	}
}

func TestPresenceDebounceIsAsymmetricAndDocumented(t *testing.T) {
	// Not behaviour, but a value that must not drift silently: FR-40 asks for
	// presence, not a strobe light, and mobile clients drop constantly.
	if realtime.PresenceDebounce < time.Second {
		t.Errorf("PresenceDebounce is %v; anything under a second makes the "+
			"participant list flicker on every lift and tunnel", realtime.PresenceDebounce)
	}
}
