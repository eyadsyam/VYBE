// Package roomstest provides an in-memory rooms.Repository.
//
// It lives outside the production package for the same reason identitytest
// does (§13.3): a store that can be reached in a release build is not a
// convenience. Here the stake is lower than an auth bypass, but a room store
// with no durability would still silently discard every event in the log,
// which is the one thing ADR-003 cannot tolerate.
//
// The mutating methods honour the interface's atomicity contract: the row
// change and the event append happen under one lock, and either both land or
// neither does. A fake that appended the event first and then failed the row
// change would let a test pass against a repository that produces exactly the
// divergence FR-28 exists to prevent.
package roomstest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms"
)

// Store is an in-memory rooms.Repository.
type Store struct {
	mu sync.Mutex

	rooms        map[string]*rooms.Room
	participants map[string][]rooms.Participant // by room id, active and departed
	events       map[string][]realtime.Envelope // by room id, in seq order
	content      map[string]bool

	// FailNext makes the next call to the named method return this error.
	FailNext map[string]error
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		rooms:        map[string]*rooms.Room{},
		participants: map[string][]rooms.Participant{},
		events:       map[string][]realtime.Envelope{},
		content:      map[string]bool{},
		FailNext:     map[string]error{},
	}
}

// AddContent registers a content id so ContentExists returns true for it.
func (s *Store) AddContent(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.content[id] = true
	}
}

func (s *Store) fail(method string) error {
	if err, ok := s.FailNext[method]; ok {
		delete(s.FailNext, method)
		return err
	}
	return nil
}

// appendLocked records an event, enforcing the gap-free rule.
//
// A fake that accepted any seq would hide the bug this whole design exists to
// prevent, so it panics on a gap: that is a defect in the caller, surfaced
// where it happened rather than as a mysterious client resync later.
func (s *Store) appendLocked(roomID string, e *realtime.Envelope) {
	if e == nil {
		return
	}
	existing := s.events[roomID]
	want := int64(len(existing) + 1)
	if e.Seq != want {
		panic("roomstest: seq gap — room " + roomID +
			" expected " + itoa(want) + ", got " + itoa(e.Seq) +
			" (FR-28: the sequence must be gap-free)")
	}
	if err := e.Validate(); err != nil {
		panic("roomstest: invalid envelope appended: " + err.Error())
	}
	s.events[roomID] = append(existing, *e)
	if r, ok := s.rooms[roomID]; ok {
		r.CurrentSeq = e.Seq
		r.UpdatedAt = e.TS
	}
}

// CreateRoom inserts the room, its host participant, and the opening event.
func (s *Store) CreateRoom(_ context.Context, r *rooms.Room, event *realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("CreateRoom"); err != nil {
		return err
	}
	// The partial unique index: a code may repeat only across ENDED rooms.
	for _, existing := range s.rooms {
		if existing.JoinCode == r.JoinCode && existing.State != rooms.StateEnded {
			return rooms.ErrJoinCodeConflict
		}
	}

	copied := *r
	s.rooms[r.ID] = &copied
	s.participants[r.ID] = []rooms.Participant{{
		UserID:    r.HostUserID,
		JoinedAt:  r.CreatedAt,
		Connected: true,
		IsHost:    true,
	}}
	s.appendLocked(r.ID, event)
	return nil
}

// RoomByID returns a room, or (nil, nil) when absent.
func (s *Store) RoomByID(_ context.Context, id string) (*rooms.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RoomByID"); err != nil {
		return nil, err
	}
	r, ok := s.rooms[id]
	if !ok {
		return nil, nil
	}
	c := *r
	return &c, nil
}

// RoomByJoinCode resolves a LIVE room by code.
func (s *Store) RoomByJoinCode(_ context.Context, code string) (*rooms.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RoomByJoinCode"); err != nil {
		return nil, err
	}
	for _, r := range s.rooms {
		// Ended rooms must not resolve. The index is partial for exactly this
		// reason, and a fake that ignored state would let a joiner walk into a
		// party that finished last week.
		if r.JoinCode == code && r.State != rooms.StateEnded {
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}

// JoinCodeTaken reports whether a live room holds the code.
func (s *Store) JoinCodeTaken(_ context.Context, code string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("JoinCodeTaken"); err != nil {
		return false, err
	}
	for _, r := range s.rooms {
		if r.JoinCode == code && r.State != rooms.StateEnded {
			return true, nil
		}
	}
	return false, nil
}

// Participants returns the ACTIVE participants, oldest join first.
func (s *Store) Participants(_ context.Context, roomID string) ([]rooms.Participant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("Participants"); err != nil {
		return nil, err
	}
	return s.activeLocked(roomID), nil
}

func (s *Store) activeLocked(roomID string) []rooms.Participant {
	all := s.participants[roomID]
	out := make([]rooms.Participant, 0, len(all))
	for _, p := range all {
		if p.DisconnectedAt == nil || p.Connected {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].JoinedAt.Equal(out[j].JoinedAt) {
			return out[i].UserID < out[j].UserID
		}
		return out[i].JoinedAt.Before(out[j].JoinedAt)
	})
	return out
}

// AddParticipant inserts a participant and appends the event atomically.
func (s *Store) AddParticipant(_ context.Context, roomID, userID, role string, at time.Time, event *realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("AddParticipant"); err != nil {
		// Deliberately BEFORE the append. A failure must leave no event
		// behind, or clients learn about a join that did not happen.
		return err
	}
	s.participants[roomID] = append(s.participants[roomID], rooms.Participant{
		UserID:    userID,
		JoinedAt:  at,
		Connected: true,
		IsHost:    role == rooms.RoleHost,
	})
	s.appendLocked(roomID, event)
	return nil
}

// RemoveParticipant stamps departure and appends the event atomically.
func (s *Store) RemoveParticipant(_ context.Context, roomID, userID string, at time.Time, event *realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RemoveParticipant"); err != nil {
		return err
	}
	list := s.participants[roomID]
	for i := range list {
		if list[i].UserID == userID {
			t := at
			list[i].DisconnectedAt = &t
			list[i].Connected = false
		}
	}
	s.appendLocked(roomID, event)
	return nil
}

// TransferHost reassigns the host and appends the event atomically.
func (s *Store) TransferHost(_ context.Context, roomID, newHostID string, _ time.Time, event *realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("TransferHost"); err != nil {
		return err
	}
	r, ok := s.rooms[roomID]
	if !ok {
		return rooms.ErrRoomNotFound
	}
	r.HostUserID = newHostID
	list := s.participants[roomID]
	for i := range list {
		list[i].IsHost = list[i].UserID == newHostID
	}
	s.appendLocked(roomID, event)
	return nil
}

// SetState writes a transition and appends the event atomically.
func (s *Store) SetState(_ context.Context, roomID string, to rooms.State, at time.Time, event *realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("SetState"); err != nil {
		return err
	}
	r, ok := s.rooms[roomID]
	if !ok {
		return rooms.ErrRoomNotFound
	}
	r.State = to
	if to == rooms.StatePlaying && r.StartedAt == nil {
		t := at
		r.StartedAt = &t
	}
	s.appendLocked(roomID, event)
	return nil
}

// EndRoom marks the room ended and appends the event atomically.
func (s *Store) EndRoom(_ context.Context, roomID, reason string, at time.Time, event *realtime.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("EndRoom"); err != nil {
		return err
	}
	r, ok := s.rooms[roomID]
	if !ok {
		return rooms.ErrRoomNotFound
	}
	t := at
	r.State = rooms.StateEnded
	r.EndedAt = &t
	r.EndReason = reason
	s.appendLocked(roomID, event)
	return nil
}

// NextSeq allocates the next sequence number.
func (s *Store) NextSeq(_ context.Context, roomID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("NextSeq"); err != nil {
		return 0, err
	}
	return int64(len(s.events[roomID]) + 1), nil
}

// RoomsForUser lists a user's rooms, newest first, keyset-paginated.
func (s *Store) RoomsForUser(_ context.Context, userID string, before *time.Time, beforeID string, limit int) ([]rooms.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("RoomsForUser"); err != nil {
		return nil, err
	}

	var out []rooms.Room
	for id, r := range s.rooms {
		member := r.HostUserID == userID
		for _, p := range s.participants[id] {
			if p.UserID == userID {
				member = true
			}
		}
		if member {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			// The id tiebreak matters: without it, two rooms created in the
			// same millisecond can swap places between pages and one is
			// returned twice while the other is never seen.
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})

	if before != nil {
		filtered := out[:0]
		for _, r := range out {
			if r.CreatedAt.Before(*before) ||
				(r.CreatedAt.Equal(*before) && r.ID < beforeID) {
				filtered = append(filtered, r)
			}
		}
		out = filtered
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ContentExists reports whether a content id is registered.
func (s *Store) ContentExists(_ context.Context, contentID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail("ContentExists"); err != nil {
		return false, err
	}
	return s.content[contentID], nil
}

// ---------------------------------------------------------------------------
// Inspection
// ---------------------------------------------------------------------------

// Events returns the room's event log in seq order.
func (s *Store) Events(roomID string) []realtime.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]realtime.Envelope(nil), s.events[roomID]...)
}

// EventTypes returns just the types, which is what most assertions care about.
func (s *Store) EventTypes(roomID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events[roomID]))
	for _, e := range s.events[roomID] {
		out = append(out, e.Type)
	}
	return out
}

// Disconnect marks a participant as disconnected at a time, without removing
// them — the state FR-17's grace window is measured against.
func (s *Store) Disconnect(roomID, userID string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.participants[roomID]
	for i := range list {
		if list[i].UserID == userID {
			t := at
			list[i].Connected = false
			list[i].DisconnectedAt = &t
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

var _ rooms.Repository = (*Store)(nil)
