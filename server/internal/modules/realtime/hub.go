package realtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The fan-out hub (FR-36–FR-42).
//
// The hub is deliberately transport-agnostic: it knows about Sinks, not about
// WebSockets. That is what lets the fan-out rules — which are the part with
// the interesting failure modes — be tested with a fake sink and no network,
// no goroutine timing, and no flakiness. The gorilla adapter in wsconn.go is
// the only file that knows what a WebSocket is.
//
// The load-bearing decision here is what happens to a SLOW client. A hub that
// blocks on a slow sink stops delivering to everybody, which turns one bad
// phone on a train into an outage for the whole room. So a sink that cannot
// keep up is disconnected, not waited for. That is only safe because ADR-003
// makes reconnection lossless: the client comes back, presents its last_seq,
// and is told exactly what it missed. Without the event log this policy would
// be data loss; with it, it is a hiccup.

// Sink is one connected client.
//
// Send must not block indefinitely. An implementation that cannot accept the
// event promptly must return ErrSinkFull rather than waiting, because the hub
// holds a lock across the fan-out and a blocking sink stalls the room.
type Sink interface {
	// UserID is who is on the other end, for FR-41's per-recipient filtering.
	UserID() string
	// ConnID distinguishes two sockets belonging to the same user — a phone
	// and a tablet, or a reconnect that raced its own teardown.
	ConnID() string
	// Send delivers one event, or returns ErrSinkFull.
	Send(e Envelope) error
	// Close terminates the connection with a reason, for the client's log.
	Close(reason string)
}

// ErrSinkFull means a client is not draining fast enough.
var ErrSinkFull = errors.New("realtime: sink buffer is full")

// Hub routes events to the clients in a room.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]map[string]Sink // room id -> conn id -> sink

	logger *slog.Logger

	// onDrop is called when a slow sink is evicted. Exists so tests can
	// observe the policy firing without inspecting logs.
	onDrop func(roomID string, s Sink)
}

// NewHub returns an empty Hub.
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		rooms:  map[string]map[string]Sink{},
		logger: logger,
	}
}

// SetDropHook installs a callback for evicted sinks. Tests only.
func (h *Hub) SetDropHook(fn func(roomID string, s Sink)) { h.onDrop = fn }

// Join registers a sink in a room.
//
// A second connection from the same user is kept, not replaced: FR-39 counts
// presence per user, but a user genuinely may have two devices in one room,
// and silently killing the older one makes "open on my tablet" log you out on
// your phone.
func (h *Hub) Join(roomID string, s Sink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[roomID]
	if !ok {
		room = map[string]Sink{}
		h.rooms[roomID] = room
	}
	room[s.ConnID()] = s
}

// Leave removes a sink. Safe to call twice.
func (h *Hub) Leave(roomID string, connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[roomID]
	if !ok {
		return
	}
	delete(room, connID)
	if len(room) == 0 {
		// Reclaim the map rather than leaving an empty one behind. A server
		// that has hosted a million rooms would otherwise hold a million empty
		// maps for the life of the process.
		delete(h.rooms, roomID)
	}
}

// Publish delivers an event to every client in its room (FR-36).
func (h *Hub) Publish(ctx context.Context, e Envelope) error {
	return h.PublishTo(ctx, e, nil)
}

// PublishTo delivers an event to a subset of a room (FR-41).
//
// A nil or empty `recipients` means everybody. When it is non-empty, only
// those user ids receive the event — which is how a private answer reveal
// reaches one player without leaking the answer to the rest of the room.
//
// The filter is applied HERE, at fan-out, rather than by the client. A client
// that receives an event it should not see has already seen it; "please
// ignore this" is not a security boundary.
func (h *Hub) PublishTo(_ context.Context, e Envelope, recipients []string) error {
	if err := e.Validate(); err != nil {
		// Refuse to fan out a malformed envelope. FR-33 requires clients to
		// silently ignore what they cannot parse, so a bad event would vanish
		// from every client with no error anywhere — the hardest possible bug
		// to find later.
		return err
	}

	var allowed map[string]struct{}
	if len(recipients) > 0 {
		allowed = make(map[string]struct{}, len(recipients))
		for _, id := range recipients {
			allowed[id] = struct{}{}
		}
	}

	// Snapshot under a read lock, then send outside it. Sending while holding
	// the lock would let one slow sink block every other room's fan-out, and
	// Join/Leave from a reconnecting client would deadlock behind it.
	h.mu.RLock()
	targets := make([]Sink, 0, len(h.rooms[e.Room]))
	for _, s := range h.rooms[e.Room] {
		if allowed != nil {
			if _, ok := allowed[s.UserID()]; !ok {
				continue
			}
		}
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	var slow []Sink
	for _, s := range targets {
		if err := s.Send(e); err != nil {
			slow = append(slow, s)
		}
	}

	// Evict outside the send loop, so a drop does not perturb the iteration
	// and every healthy client still gets the event.
	for _, s := range slow {
		h.dropSlow(e.Room, s)
	}
	return nil
}

// dropSlow disconnects a client that cannot keep up.
//
// Safe precisely because ADR-003 exists: the client reconnects, presents its
// last_seq, and is served the delta. The alternative — an unbounded buffer —
// converts a slow client into unbounded server memory, which is how one
// device on a bad connection takes down a process.
func (h *Hub) dropSlow(roomID string, s Sink) {
	h.Leave(roomID, s.ConnID())
	s.Close("slow consumer: reconnect and resync")
	h.logger.Warn("dropped a slow client",
		"room", roomID, "user", s.UserID(), "conn", s.ConnID(),
		"recovery", "the client reconnects and resyncs from its last_seq (ADR-003)")
	if h.onDrop != nil {
		h.onDrop(roomID, s)
	}
}

// PresentUserIDs returns the distinct users connected to a room (FR-39).
//
// Distinct users, not connections: somebody with a phone and a tablet in the
// same room is one person present, and a participant list that showed them
// twice would be read as a bug by every user who saw it.
func (h *Hub) PresentUserIDs(roomID string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	seen := map[string]struct{}{}
	out := make([]string, 0, len(h.rooms[roomID]))
	for _, s := range h.rooms[roomID] {
		id := s.UserID()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// IsPresent reports whether a user has at least one live connection.
func (h *Hub) IsPresent(roomID, userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.rooms[roomID] {
		if s.UserID() == userID {
			return true
		}
	}
	return false
}

// ConnCount reports the number of sockets in a room, phones and tablets alike.
func (h *Hub) ConnCount(roomID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[roomID])
}

// RoomCount reports how many rooms have at least one connection.
func (h *Hub) RoomCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

// CloseRoom disconnects everybody in a room, used when it ends (FR-19).
func (h *Hub) CloseRoom(roomID, reason string) {
	h.mu.Lock()
	sinks := make([]Sink, 0, len(h.rooms[roomID]))
	for _, s := range h.rooms[roomID] {
		sinks = append(sinks, s)
	}
	delete(h.rooms, roomID)
	h.mu.Unlock()

	// Outside the lock: Close may block on a network write, and holding the
	// hub lock across it would stall every other room.
	for _, s := range sinks {
		s.Close(reason)
	}
}

// ---------------------------------------------------------------------------
// Presence events
// ---------------------------------------------------------------------------

// PresenceChange is FR-40's payload: who changed and how.
type PresenceChange struct {
	UserID    string    `json:"userId"`
	Connected bool      `json:"connected"`
	At        time.Time `json:"at"`
}

// PresenceDebounce is how long a disconnect is held before it is announced.
//
// FR-40 asks for presence, not for a strobe light. Mobile clients drop and
// reconnect constantly — a lift, a tunnel, an app backgrounded for two
// seconds — and announcing each transition would fill the room's event log
// with noise and make the participant list flicker for everybody else.
//
// The debounce is asymmetric on purpose: a CONNECT is announced immediately
// because seeing someone arrive late is worse than seeing them arrive
// twice, while a DISCONNECT waits, because most disconnects resolve
// themselves before anybody needed to know.
const PresenceDebounce = 5 * time.Second
