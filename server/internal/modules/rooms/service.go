package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/platform/ids"
)

// The rooms application service (FR-11–FR-20).
//
// Every mutation here has two halves that must land together: the row change
// and the event that tells connected clients about it. ADR-003 makes the
// second half load-bearing rather than a notification — a client's whole model
// of the room is its position in that sequence, so a state change with no
// event is a room that some clients will never see change, and an event with
// no state change is a room whose clients disagree with the database.
//
// That is why the Repository methods below are coarse: JoinRoom inserts a
// participant AND appends the event, because splitting them into two calls
// makes the atomicity a caller's responsibility, and one caller will forget.

// Room is a watch party.
type Room struct {
	ID               string
	ContentID        string
	HostUserID       string
	JoinCode         string
	Visibility       string
	State            State
	SyncMode         string
	Title            string
	AnchorServerTime *time.Time
	AnchorOffsetMS   int64
	ReanchorCount    int
	MaxParticipants  int
	CurrentSeq       int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        *time.Time
	EndedAt          *time.Time
	EndReason        string
}

// Visibility values, matching the room_visibility enum.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
)

// Sync modes, matching the sync_mode enum.
const (
	SyncModeCompanion = "COMPANION"
	SyncModeManual    = "MANUAL"
)

// Mutation is the result of a state change: the room as it now stands, plus
// the event that must be fanned out to tell everyone.
//
// Returned as one value so a handler cannot respond 200 having forgotten to
// publish. The two are produced in one transaction and travel together.
type Mutation struct {
	Room  *Room
	Event realtime.Envelope
}

// Repository is the storage contract.
//
// Several methods bundle a row change with an event append. That is not
// convenience — it is the atomicity requirement stated where an implementer
// will see it. Each such method MUST be a single transaction that:
//
//  1. takes a row lock on the room,
//  2. allocates the next seq with UPDATE rooms SET current_seq = current_seq + 1
//     RETURNING current_seq,
//  3. writes the state change,
//  4. inserts the event.
//
// Doing (2) outside the transaction is what produces a gap, and a gap is
// indistinguishable from data loss to every connected client (FR-28).
type Repository interface {
	// CreateRoom inserts the room, its host participant row, and the opening
	// ROOM_STATE_CHANGED event.
	CreateRoom(ctx context.Context, r *Room, event *realtime.Envelope) error

	RoomByID(ctx context.Context, id string) (*Room, error)

	// RoomByJoinCode resolves a LIVE room by code. Ended rooms must not
	// resolve: the unique index is partial on `state <> 'ENDED'` precisely so
	// a code can be reused, and returning an ended room would send a joiner to
	// a party that finished last week.
	RoomByJoinCode(ctx context.Context, code string) (*Room, error)

	// JoinCodeTaken reports whether a code is in use by a live room.
	JoinCodeTaken(ctx context.Context, code string) (bool, error)

	// Participants returns the ACTIVE participants, oldest join first.
	Participants(ctx context.Context, roomID string) ([]Participant, error)

	// AddParticipant inserts or reactivates a participant and appends
	// PARTICIPANT_JOINED, in one transaction.
	AddParticipant(ctx context.Context, roomID, userID, role string, at time.Time, event *realtime.Envelope) error

	// RemoveParticipant stamps left_at and appends PARTICIPANT_LEFT, in one
	// transaction.
	RemoveParticipant(ctx context.Context, roomID, userID string, at time.Time, event *realtime.Envelope) error

	// TransferHost reassigns the host and appends HOST_CHANGED, in one
	// transaction.
	TransferHost(ctx context.Context, roomID, newHostID string, at time.Time, event *realtime.Envelope) error

	// SetState writes a state transition and appends ROOM_STATE_CHANGED, in
	// one transaction.
	SetState(ctx context.Context, roomID string, to State, at time.Time, event *realtime.Envelope) error

	// EndRoom marks the room ended and appends ROOM_ENDED, in one transaction.
	EndRoom(ctx context.Context, roomID, reason string, at time.Time, event *realtime.Envelope) error

	// NextSeq allocates the next sequence number for a room.
	//
	// Called by the service to fill the envelope BEFORE handing it to a
	// mutating method, which then uses that same number. An implementation
	// must allocate inside the same transaction the mutation runs in.
	NextSeq(ctx context.Context, roomID string) (int64, error)

	// RoomsForUser lists rooms a user hosts or participates in, newest first,
	// keyset-paginated (FR-59).
	RoomsForUser(ctx context.Context, userID string, before *time.Time, beforeID string, limit int) ([]Room, error)

	// ContentExists guards the foreign key. Checking first turns a 500 from a
	// constraint violation into a 422 that names the field.
	ContentExists(ctx context.Context, contentID string) (bool, error)
}

// EntitlementLookup reports a user's tier.
//
// Rooms needs it for capacity (FR-16) but must not import identity — §5.1's
// module boundary. A narrow interface satisfied by the identity facade keeps
// the dependency one-directional and the test a two-line fake.
type EntitlementLookup interface {
	EntitlementTier(ctx context.Context, userID string) (string, error)
}

// Errors a client is allowed to distinguish.
var (
	ErrRoomNotFound     = errors.New("rooms: no such room")
	ErrNotAParticipant  = errors.New("rooms: not a participant in this room")
	ErrNotTheHost       = errors.New("rooms: only the host can do that")
	ErrAlreadyJoined    = errors.New("rooms: already in this room")
	ErrContentNotFound  = errors.New("rooms: no such content")
	ErrJoinCodeConflict = errors.New("rooms: could not allocate a unique join code")
	ErrInvalidTitle     = errors.New("rooms: title is not valid")
	ErrInvalidSyncMode  = errors.New("rooms: sync mode is not recognised")
	ErrPublicRoomDenied = errors.New("rooms: this account may not create public rooms")
)

// MaxTitleLength bounds the room title.
//
// The column is unconstrained `text`, so this is the only limit. Without it a
// title is an unbounded attacker-controlled string that appears in every
// participant's UI and in every event payload — which is both a rendering
// problem and a bandwidth one, since the title rides along in the room
// snapshot sent on every resync.
const MaxTitleLength = 120

// joinCodeAttempts bounds the retry loop when a generated code collides.
//
// 32^6 is about 1.07e9, so a collision needs an extraordinary number of live
// rooms — but "extraordinary" is not "impossible", and an unbounded loop
// against a database is how a rare collision becomes a hung request. Five
// attempts makes the failure a clean 503 instead.
const joinCodeAttempts = 5

// Service composes the room rules with storage.
type Service struct {
	repo         Repository
	entitlements EntitlementLookup
	now          func() time.Time
	newCode      func() (string, error)
}

// NewService returns a Service.
func NewService(repo Repository, entitlements EntitlementLookup) *Service {
	return &Service{
		repo:         repo,
		entitlements: entitlements,
		now:          time.Now,
		newCode:      NewJoinCode,
	}
}

// SetClock replaces the time source. Tests only.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetCodeGenerator replaces the join-code source, so a test can force the
// collision path that is otherwise a one-in-a-billion event.
func (s *Service) SetCodeGenerator(gen func() (string, error)) { s.newCode = gen }

// CreateInput is what FR-11 requires to open a room.
type CreateInput struct {
	HostUserID string
	ContentID  string
	Title      string
	Visibility string
	SyncMode   string
}

// Create opens a room in LOBBY (FR-11, FR-12).
func (s *Service) Create(ctx context.Context, in CreateInput) (*Mutation, error) {
	now := s.now()

	title := strings.TrimSpace(in.Title)
	if len([]rune(title)) > MaxTitleLength {
		return nil, ErrInvalidTitle
	}

	syncMode, err := normaliseSyncMode(in.SyncMode)
	if err != nil {
		return nil, err
	}

	visibility := VisibilityPrivate
	if strings.EqualFold(strings.TrimSpace(in.Visibility), VisibilityPublic) {
		visibility = VisibilityPublic
	}

	if ok, err := s.repo.ContentExists(ctx, in.ContentID); err != nil {
		return nil, fmt.Errorf("checking content: %w", err)
	} else if !ok {
		// A foreign-key violation would be a 500 with a Postgres message in
		// the log and nothing useful for the client. Checking first makes it a
		// 422 that names the field.
		return nil, ErrContentNotFound
	}

	tier, err := s.entitlements.EntitlementTier(ctx, in.HostUserID)
	if err != nil {
		return nil, fmt.Errorf("resolving entitlement: %w", err)
	}

	code, err := s.allocateJoinCode(ctx)
	if err != nil {
		return nil, err
	}

	room := &Room{
		ID:              ids.New().String(),
		ContentID:       in.ContentID,
		HostUserID:      in.HostUserID,
		JoinCode:        code,
		Visibility:      visibility,
		State:           StateLobby,
		SyncMode:        syncMode,
		Title:           title,
		MaxParticipants: MaxParticipants(tier),
		// The first event is seq 1, so the room starts at 0. AC-7 asserts
		// 1..100 for a hundred events and starting at 1 here would put every
		// room permanently off by one.
		CurrentSeq: 1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	event := envelope(room.ID, 1, realtime.EventRoomStateChanged, in.HostUserID, now, map[string]any{
		"from":   "",
		"to":     string(StateLobby),
		"roomId": room.ID,
	})
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("building the opening event: %w", err)
	}

	if err := s.repo.CreateRoom(ctx, room, &event); err != nil {
		return nil, err
	}
	return &Mutation{Room: room, Event: event}, nil
}

// allocateJoinCode draws codes until one is free.
func (s *Service) allocateJoinCode(ctx context.Context) (string, error) {
	for range joinCodeAttempts {
		code, err := s.newCode()
		if err != nil {
			return "", fmt.Errorf("generating join code: %w", err)
		}
		taken, err := s.repo.JoinCodeTaken(ctx, code)
		if err != nil {
			return "", fmt.Errorf("checking join code: %w", err)
		}
		if !taken {
			return code, nil
		}
	}
	// The insert still carries the unique index, so this check racing is
	// survivable; what it cannot survive is looping forever.
	return "", ErrJoinCodeConflict
}

// JoinByCode admits a user to a room (FR-13, FR-14, FR-16).
func (s *Service) JoinByCode(ctx context.Context, rawCode, userID string) (*Mutation, error) {
	now := s.now()

	code, err := ParseJoinCode(rawCode)
	if err != nil {
		// A malformed code and an unknown code are the same answer on purpose.
		// Distinguishing them tells someone enumerating codes that their
		// alphabet is right, which is the expensive half of the search.
		return nil, ErrRoomNotFound
	}

	room, err := s.repo.RoomByJoinCode(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("resolving join code: %w", err)
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}

	participants, err := s.repo.Participants(ctx, room.ID)
	if err != nil {
		return nil, fmt.Errorf("loading participants: %w", err)
	}
	for _, p := range participants {
		if p.UserID == userID {
			// Idempotent-ish: rejoining is not an error the client should show
			// as a failure, but it must not append a second JOINED event
			// either — the participant list would then contain a duplicate on
			// every client that applied both.
			return nil, ErrAlreadyJoined
		}
	}

	// Capacity is measured against the HOST's tier, not the joiner's. FR-16
	// says the room's capacity is a property of the room, and the room belongs
	// to whoever opened it. Checking the joiner's tier would let a free user
	// be refused entry to a premium host's room with space in it.
	hostTier, err := s.entitlements.EntitlementTier(ctx, room.HostUserID)
	if err != nil {
		return nil, fmt.Errorf("resolving host entitlement: %w", err)
	}
	if err := CanJoin(room.State, hostTier, len(participants)); err != nil {
		return nil, err
	}

	seq, err := s.repo.NextSeq(ctx, room.ID)
	if err != nil {
		return nil, fmt.Errorf("allocating seq: %w", err)
	}
	event := envelope(room.ID, seq, realtime.EventParticipantJoined, userID, now, map[string]any{
		"userId": userID,
		"role":   RoleParticipant,
	})
	if err := s.repo.AddParticipant(ctx, room.ID, userID, RoleParticipant, now, &event); err != nil {
		return nil, err
	}

	room.CurrentSeq = seq
	return &Mutation{Room: room, Event: event}, nil
}

// Leave removes a participant, promoting a successor if the host left
// (FR-17, FR-18).
func (s *Service) Leave(ctx context.Context, roomID, userID string) (*Mutation, error) {
	now := s.now()

	room, participants, err := s.loadWithParticipants(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if !contains(participants, userID) {
		return nil, ErrNotAParticipant
	}

	seq, err := s.repo.NextSeq(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("allocating seq: %w", err)
	}
	leaveEvent := envelope(roomID, seq, realtime.EventParticipantLeft, userID, now, map[string]any{
		"userId": userID,
	})
	if err := s.repo.RemoveParticipant(ctx, roomID, userID, now, &leaveEvent); err != nil {
		return nil, err
	}
	room.CurrentSeq = seq

	// A non-host leaving is the whole story.
	if userID != room.HostUserID {
		return &Mutation{Room: room, Event: leaveEvent}, nil
	}

	// The host left. Somebody has to own the room, or nobody can start
	// playback and the party is silently dead while everyone waits.
	remaining := without(participants, userID)
	successor := SuccessorFor(remaining, now)
	if successor == "" {
		// Nobody is left. End it now rather than leaving an empty room to the
		// reaper — an empty LOBBY that resolves by join code for another ten
		// minutes is a room a straggler can walk into alone.
		return s.endRoom(ctx, room, "reaper_abandoned", now)
	}

	hostSeq, err := s.repo.NextSeq(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("allocating seq for host change: %w", err)
	}
	hostEvent := envelope(roomID, hostSeq, realtime.EventHostChanged, "", now, map[string]any{
		"previousHostId": userID,
		"newHostId":      successor,
		"reason":         "host_left",
	})
	if err := s.repo.TransferHost(ctx, roomID, successor, now, &hostEvent); err != nil {
		return nil, err
	}

	room.HostUserID = successor
	room.CurrentSeq = hostSeq
	// The HOST_CHANGED event is the one returned, because it is the one that
	// changes what clients may do. PARTICIPANT_LEFT is already durable and
	// will reach them through the same log.
	return &Mutation{Room: room, Event: hostEvent}, nil
}

// End closes a room (FR-19). Host only.
func (s *Service) End(ctx context.Context, roomID, userID string) (*Mutation, error) {
	now := s.now()

	room, err := s.repo.RoomByID(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("loading room: %w", err)
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}
	if room.HostUserID != userID {
		return nil, ErrNotTheHost
	}
	if room.State == StateEnded {
		// Already ended. Not an error: a host double-tapping "end party" on a
		// slow connection must not see a failure for something that worked.
		return &Mutation{Room: room}, nil
	}
	return s.endRoom(ctx, room, "host_ended", now)
}

func (s *Service) endRoom(ctx context.Context, room *Room, reason string, now time.Time) (*Mutation, error) {
	seq, err := s.repo.NextSeq(ctx, room.ID)
	if err != nil {
		return nil, fmt.Errorf("allocating seq: %w", err)
	}
	event := envelope(room.ID, seq, realtime.EventRoomEnded, "", now, map[string]any{
		"reason": reason,
		"from":   string(room.State),
	})
	if err := s.repo.EndRoom(ctx, room.ID, reason, now, &event); err != nil {
		return nil, err
	}

	ended := now
	room.State = StateEnded
	room.EndedAt = &ended
	room.EndReason = reason
	room.CurrentSeq = seq
	return &Mutation{Room: room, Event: event}, nil
}

// Transition applies a lifecycle event (FR-15). Host only.
func (s *Service) Transition(ctx context.Context, roomID, userID string, ev Event) (*Mutation, error) {
	now := s.now()

	room, err := s.repo.RoomByID(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("loading room: %w", err)
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}
	if room.HostUserID != userID {
		return nil, ErrNotTheHost
	}

	to, err := Next(room.State, ev)
	if err != nil {
		return nil, err // ErrIllegalTransition, which the handler maps to 409
	}

	seq, err := s.repo.NextSeq(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("allocating seq: %w", err)
	}
	event := envelope(roomID, seq, realtime.EventRoomStateChanged, userID, now, map[string]any{
		"from":  string(room.State),
		"to":    string(to),
		"event": string(ev),
	})
	if err := s.repo.SetState(ctx, roomID, to, now, &event); err != nil {
		return nil, err
	}

	room.State = to
	room.CurrentSeq = seq
	return &Mutation{Room: room, Event: event}, nil
}

// Get returns a room and its participants, for a caller who is in it.
//
// Membership is required even though the join code is unguessable. FR-14 is
// explicit that possession of a code is not access — a code shared into a
// group chat months ago must not remain a permanent key, and an unguessable
// identifier is a weak authorisation check that fails silently once leaked.
func (s *Service) Get(ctx context.Context, roomID, viewerID string) (*Room, []Participant, error) {
	room, participants, err := s.loadWithParticipants(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	if !contains(participants, viewerID) && room.HostUserID != viewerID {
		// ErrRoomNotFound, not ErrForbidden. Telling a stranger "that room
		// exists but you are not in it" confirms the id, which is exactly what
		// somebody probing ids wants to learn.
		return nil, nil, ErrRoomNotFound
	}
	return room, participants, nil
}

// ListForUser returns the caller's rooms, newest first (FR-20, FR-59).
func (s *Service) ListForUser(ctx context.Context, userID string, before *time.Time, beforeID string, limit int) ([]Room, error) {
	rooms, err := s.repo.RoomsForUser(ctx, userID, before, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing rooms: %w", err)
	}
	return rooms, nil
}

func (s *Service) loadWithParticipants(ctx context.Context, roomID string) (*Room, []Participant, error) {
	room, err := s.repo.RoomByID(ctx, roomID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading room: %w", err)
	}
	if room == nil {
		return nil, nil, ErrRoomNotFound
	}
	participants, err := s.repo.Participants(ctx, roomID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading participants: %w", err)
	}
	return room, participants, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// Participant roles, matching the participant_role enum.
const (
	RoleHost        = "host"
	RoleParticipant = "participant"
)

func envelope(roomID string, seq int64, eventType, actor string, ts time.Time, payload map[string]any) realtime.Envelope {
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Unreachable: every payload built here is a map of strings and
		// numbers. Falling back to `{}` rather than panicking keeps a
		// hypothetical encoding bug from taking the process down, and
		// Validate rejects an empty payload so it would still be caught.
		encoded = []byte(`{}`)
	}
	return realtime.Envelope{
		V:       realtime.EnvelopeVersion,
		ID:      ids.New().String(),
		Room:    roomID,
		Seq:     seq,
		Type:    eventType,
		TS:      ts,
		Actor:   actor,
		Payload: encoded,
	}
}

func contains(ps []Participant, userID string) bool {
	for _, p := range ps {
		if p.UserID == userID {
			return true
		}
	}
	return false
}

func without(ps []Participant, userID string) []Participant {
	out := make([]Participant, 0, len(ps))
	for _, p := range ps {
		if p.UserID != userID {
			out = append(out, p)
		}
	}
	return out
}

func normaliseSyncMode(raw string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", SyncModeCompanion:
		// Companion is the default because it is the mode ADR-002 exists for.
		return SyncModeCompanion, nil
	case SyncModeManual:
		return SyncModeManual, nil
	default:
		return "", ErrInvalidSyncMode
	}
}
