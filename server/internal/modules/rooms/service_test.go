package rooms_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms/roomstest"
)

var svcNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

const (
	hostID    = "11111111-1111-7111-8111-111111111111"
	guestID   = "22222222-2222-7222-8222-222222222222"
	thirdID   = "33333333-3333-7333-8333-333333333333"
	fourthID  = "44444444-4444-7444-8444-444444444444"
	contentID = "aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"
)

// tiers is an EntitlementLookup backed by a map.
//
// A two-line fake rather than the real identity service, which is the point of
// the narrow interface: rooms must not import identity (§5.1), and this proves
// the dependency really is only "one string per user".
type tiers map[string]string

func (t tiers) EntitlementTier(_ context.Context, userID string) (string, error) {
	if tier, ok := t[userID]; ok {
		return tier, nil
	}
	return "free", nil
}

type failingTiers struct{ err error }

func (f failingTiers) EntitlementTier(context.Context, string) (string, error) {
	return "", f.err
}

func newRoomService(t *testing.T, tierMap tiers) (*rooms.Service, *roomstest.Store, func(time.Time)) {
	t.Helper()
	store := roomstest.New()
	store.AddContent(contentID)

	svc := rooms.NewService(store, tierMap)
	now := svcNow
	svc.SetClock(func() time.Time { return now })
	return svc, store, func(v time.Time) { now = v }
}

func mustCreate(t *testing.T, svc *rooms.Service) *rooms.Mutation {
	t.Helper()
	m, err := svc.Create(context.Background(), rooms.CreateInput{
		HostUserID: hostID, ContentID: contentID, Title: "ليلة أفلام",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreateOpensARoomInLobby(t *testing.T) {
	svc, store, _ := newRoomService(t, tiers{})
	m := mustCreate(t, svc)

	if m.Room.State != rooms.StateLobby {
		t.Errorf("state = %q, want LOBBY", m.Room.State)
	}
	if m.Room.HostUserID != hostID {
		t.Errorf("host = %q, want %q", m.Room.HostUserID, hostID)
	}
	if m.Room.Visibility != rooms.VisibilityPrivate {
		t.Errorf("visibility = %q; private is the default and the safe one", m.Room.Visibility)
	}
	if m.Room.SyncMode != rooms.SyncModeCompanion {
		t.Errorf("sync mode = %q, want COMPANION (ADR-002 is why the mode exists)", m.Room.SyncMode)
	}
	if len(m.Room.JoinCode) != rooms.JoinCodeLength {
		t.Errorf("join code %q is not %d characters", m.Room.JoinCode, rooms.JoinCodeLength)
	}
	if m.Room.MaxParticipants != 4 {
		t.Errorf("capacity = %d, want the free tier's 4", m.Room.MaxParticipants)
	}

	// The host must be a participant, or they cannot be found by any of the
	// membership checks that guard the rest of the API.
	ps, err := store.Participants(context.Background(), m.Room.ID)
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(ps) != 1 || ps[0].UserID != hostID || !ps[0].IsHost {
		t.Errorf("participants = %+v, want the host alone and flagged as host", ps)
	}
}

func TestCreateEmitsSeqOneAsTheFirstEvent(t *testing.T) {
	// AC-7 asserts 1..100 for a hundred events. Starting anywhere but 1 puts
	// every room permanently off by one.
	svc, store, _ := newRoomService(t, tiers{})
	m := mustCreate(t, svc)

	events := store.Events(m.Room.ID)
	if len(events) != 1 {
		t.Fatalf("%d events after Create, want 1", len(events))
	}
	if events[0].Seq != 1 {
		t.Errorf("first event seq = %d, want 1", events[0].Seq)
	}
	if events[0].Type != realtime.EventRoomStateChanged {
		t.Errorf("first event type = %q, want %q", events[0].Type, realtime.EventRoomStateChanged)
	}
	if m.Event.Seq != 1 {
		t.Errorf("returned event seq = %d, want 1", m.Event.Seq)
	}
	if err := m.Event.Validate(); err != nil {
		t.Errorf("the returned envelope does not satisfy §7.2: %v", err)
	}
}

func TestCreateRespectsEntitlementCapacity(t *testing.T) {
	for tier, want := range map[string]int{"free": 4, "plus": 8, "unknown-tier": 4} {
		svc, _, _ := newRoomService(t, tiers{hostID: tier})
		m := mustCreate(t, svc)
		if m.Room.MaxParticipants != want {
			t.Errorf("tier %q gave capacity %d, want %d", tier, m.Room.MaxParticipants, want)
		}
	}
}

func TestCreateRejectsUnknownContent(t *testing.T) {
	// Checked before the insert so a foreign-key violation becomes a 422 that
	// names the field rather than a 500 with a Postgres message in the log.
	svc, _, _ := newRoomService(t, tiers{})
	_, err := svc.Create(context.Background(), rooms.CreateInput{
		HostUserID: hostID, ContentID: "bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb",
	})
	if !errors.Is(err, rooms.ErrContentNotFound) {
		t.Errorf("Create with unknown content = %v, want ErrContentNotFound", err)
	}
}

func TestCreateValidatesTitleAndSyncMode(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{})
	ctx := context.Background()

	// The title is bounded even though the column is unconstrained text: it
	// rides in every room snapshot on every resync.
	_, err := svc.Create(ctx, rooms.CreateInput{
		HostUserID: hostID, ContentID: contentID,
		Title: strings.Repeat("ب", rooms.MaxTitleLength+1),
	})
	if !errors.Is(err, rooms.ErrInvalidTitle) {
		t.Errorf("an over-long title = %v, want ErrInvalidTitle", err)
	}

	// Exactly at the limit is fine — and counted in RUNES, not bytes, or an
	// Arabic title would be cut at a third of the length of an English one.
	if _, err := svc.Create(ctx, rooms.CreateInput{
		HostUserID: hostID, ContentID: contentID,
		Title: strings.Repeat("ب", rooms.MaxTitleLength),
	}); err != nil {
		t.Errorf("a title of exactly %d runes was rejected: %v", rooms.MaxTitleLength, err)
	}

	if _, err := svc.Create(ctx, rooms.CreateInput{
		HostUserID: hostID, ContentID: contentID, SyncMode: "TELEPATHY",
	}); !errors.Is(err, rooms.ErrInvalidSyncMode) {
		t.Errorf("an unknown sync mode = %v, want ErrInvalidSyncMode", err)
	}
}

func TestCreateRetriesOnAJoinCodeCollision(t *testing.T) {
	// A one-in-a-billion event, forced. What matters is that it terminates:
	// an unbounded retry loop against a database turns a rare collision into a
	// hung request.
	svc, store, _ := newRoomService(t, tiers{})
	ctx := context.Background()

	svc.SetCodeGenerator(func() (string, error) { return "AAAAAA", nil })
	if _, err := svc.Create(ctx, rooms.CreateInput{HostUserID: hostID, ContentID: contentID}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Every draw now collides.
	if _, err := svc.Create(ctx, rooms.CreateInput{HostUserID: guestID, ContentID: contentID}); !errors.Is(err, rooms.ErrJoinCodeConflict) {
		t.Fatalf("an always-colliding generator = %v, want ErrJoinCodeConflict", err)
	}

	// But a generator that collides once and then succeeds must go through.
	calls := 0
	svc.SetCodeGenerator(func() (string, error) {
		calls++
		if calls == 1 {
			return "AAAAAA", nil
		}
		return "BBBBBB", nil
	})
	m, err := svc.Create(ctx, rooms.CreateInput{HostUserID: guestID, ContentID: contentID})
	if err != nil {
		t.Fatalf("Create after one collision: %v", err)
	}
	if m.Room.JoinCode != "BBBBBB" {
		t.Errorf("join code = %q, want the second draw", m.Room.JoinCode)
	}
	_ = store
}

// ---------------------------------------------------------------------------
// Join
// ---------------------------------------------------------------------------

func TestJoinByCodeAdmitsAndEmits(t *testing.T) {
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)

	m, err := svc.JoinByCode(context.Background(), created.Room.JoinCode, guestID)
	if err != nil {
		t.Fatalf("JoinByCode: %v", err)
	}
	if m.Event.Type != realtime.EventParticipantJoined {
		t.Errorf("event type = %q, want PARTICIPANT_JOINED", m.Event.Type)
	}
	if m.Event.Seq != 2 {
		t.Errorf("event seq = %d, want 2 (the create event was 1)", m.Event.Seq)
	}

	var payload map[string]any
	if err := json.Unmarshal(m.Event.Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload["userId"] != guestID {
		t.Errorf("payload.userId = %v, want %q", payload["userId"], guestID)
	}

	ps, _ := store.Participants(context.Background(), created.Room.ID)
	if len(ps) != 2 {
		t.Errorf("%d participants, want 2", len(ps))
	}
}

func TestJoinAcceptsTheFormsUsersActuallyType(t *testing.T) {
	// The Crockford decode is only useful if the service applies it. A code is
	// read aloud over a call and typed by somebody else, so transcription is
	// the dominant failure mode.
	svc, _, _ := newRoomService(t, tiers{hostID: "plus"})
	created := mustCreate(t, svc)
	code := created.Room.JoinCode

	variants := []string{
		strings.ToLower(code),
		"  " + code + "  ",
		code[:3] + "-" + code[3:],
		code[:3] + " " + code[3:],
	}
	joiners := []string{guestID, thirdID, fourthID, "55555555-5555-7555-8555-555555555555"}

	for i, v := range variants {
		if _, err := svc.JoinByCode(context.Background(), v, joiners[i]); err != nil {
			t.Errorf("JoinByCode(%q): %v", v, err)
		}
	}
}

func TestJoinTreatsMalformedAndUnknownCodesIdentically(t *testing.T) {
	// Distinguishing them tells somebody enumerating codes that their alphabet
	// is right, which is the expensive half of the search.
	svc, _, _ := newRoomService(t, tiers{})
	mustCreate(t, svc)

	for _, code := range []string{
		"ZZZZZZ",    // well-formed, no such room
		"TOO",       // too short
		"TOOLONGXX", // too long
		"K7X2QU",    // contains U, which the alphabet excludes
		"",          // empty
		"!!!!!!",    // punctuation
	} {
		if _, err := svc.JoinByCode(context.Background(), code, guestID); !errors.Is(err, rooms.ErrRoomNotFound) {
			t.Errorf("JoinByCode(%q) = %v, want ErrRoomNotFound for every unusable code", code, err)
		}
	}
}

func TestJoinRefusesASecondTime(t *testing.T) {
	// A second JOINED event would put a duplicate in the participant list of
	// every client that applied both.
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("first join: %v", err)
	}
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); !errors.Is(err, rooms.ErrAlreadyJoined) {
		t.Errorf("second join = %v, want ErrAlreadyJoined", err)
	}
	// The host rejoining their own room is the same case.
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, hostID); !errors.Is(err, rooms.ErrAlreadyJoined) {
		t.Errorf("host rejoining = %v, want ErrAlreadyJoined", err)
	}

	if got := len(store.Events(created.Room.ID)); got != 2 {
		t.Errorf("%d events after two refused joins, want 2", got)
	}
}

func TestJoinEnforcesCapacityAgainstTheHostsTier(t *testing.T) {
	// FR-16: capacity is a property of the ROOM, and the room belongs to
	// whoever opened it. Measuring the joiner's tier instead would refuse a
	// free user entry to a premium host's room that has space.
	svc, _, _ := newRoomService(t, tiers{hostID: "free"})
	created := mustCreate(t, svc)
	ctx := context.Background()

	// Free tier is 4, and the host occupies one.
	for _, id := range []string{guestID, thirdID, fourthID} {
		if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, id); err != nil {
			t.Fatalf("join by %s: %v", id, err)
		}
	}
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, "55555555-5555-7555-8555-555555555555"); !errors.Is(err, rooms.ErrRoomFull) {
		t.Errorf("the fifth join = %v, want ErrRoomFull", err)
	}
}

func TestAPlusHostGetsTheLargerRoom(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{hostID: "plus", guestID: "free"})
	created := mustCreate(t, svc)
	ctx := context.Background()

	// A free-tier joiner must still get in, because the capacity is the
	// host's.
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("a free user joining a plus host's room: %v", err)
	}
	// Host + guest is 2; the plus cap is 8, so six more fit.
	for i := range 6 {
		id := "6666666" + string(rune('a'+i)) + "-6666-7666-8666-666666666666"
		if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, id); err != nil {
			t.Fatalf("join %d of an 8-seat room: %v", i+3, err)
		}
	}
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, "77777777-7777-7777-8777-777777777777"); !errors.Is(err, rooms.ErrRoomFull) {
		t.Errorf("the ninth join = %v, want ErrRoomFull", err)
	}
}

func TestJoinRefusesAnEndedRoom(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	if _, err := svc.End(ctx, created.Room.ID, hostID); err != nil {
		t.Fatalf("End: %v", err)
	}
	// The code no longer resolves at all — the unique index is partial on
	// `state <> 'ENDED'` precisely so codes can be reused.
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); !errors.Is(err, rooms.ErrRoomNotFound) {
		t.Errorf("joining an ended room = %v, want ErrRoomNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Leave and succession
// ---------------------------------------------------------------------------

func TestNonHostLeaveJustEmitsLeft(t *testing.T) {
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("join: %v", err)
	}

	m, err := svc.Leave(ctx, created.Room.ID, guestID)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if m.Event.Type != realtime.EventParticipantLeft {
		t.Errorf("event = %q, want PARTICIPANT_LEFT", m.Event.Type)
	}
	if m.Room.HostUserID != hostID {
		t.Errorf("the host changed when a guest left: %q", m.Room.HostUserID)
	}
	if types := store.EventTypes(created.Room.ID); len(types) != 3 {
		t.Errorf("events = %v, want three", types)
	}
}

func TestHostLeavingPromotesTheLongestTenuredParticipant(t *testing.T) {
	// FR-17. Somebody must own the room, or nobody can start playback and the
	// party is silently dead while everyone waits.
	svc, store, setNow := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	setNow(svcNow.Add(1 * time.Minute))
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("join guest: %v", err)
	}
	setNow(svcNow.Add(2 * time.Minute))
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, thirdID); err != nil {
		t.Fatalf("join third: %v", err)
	}

	setNow(svcNow.Add(3 * time.Minute))
	m, err := svc.Leave(ctx, created.Room.ID, hostID)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}

	if m.Event.Type != realtime.EventHostChanged {
		t.Fatalf("event = %q, want HOST_CHANGED — it is the one that changes what clients may do", m.Event.Type)
	}
	if m.Room.HostUserID != guestID {
		t.Errorf("new host = %q, want the longest-tenured %q", m.Room.HostUserID, guestID)
	}

	// Both events are durable, in order. The client learns of the departure
	// and the succession through the same log.
	types := store.EventTypes(created.Room.ID)
	want := []string{
		realtime.EventRoomStateChanged,
		realtime.EventParticipantJoined,
		realtime.EventParticipantJoined,
		realtime.EventParticipantLeft,
		realtime.EventHostChanged,
	}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, types[i], want[i])
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(m.Event.Payload, &payload); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if payload["previousHostId"] != hostID || payload["newHostId"] != guestID {
		t.Errorf("HOST_CHANGED payload = %v; it must name both sides", payload)
	}
}

func TestTheLastParticipantLeavingEndsTheRoom(t *testing.T) {
	// An empty LOBBY that still resolves by join code for another ten minutes
	// is a room a straggler can walk into alone.
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	m, err := svc.Leave(ctx, created.Room.ID, hostID)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if m.Event.Type != realtime.EventRoomEnded {
		t.Errorf("event = %q, want ROOM_ENDED", m.Event.Type)
	}
	if m.Room.State != rooms.StateEnded {
		t.Errorf("state = %q, want ENDED", m.Room.State)
	}
	if m.Room.EndReason != "reaper_abandoned" {
		t.Errorf("end reason = %q, want reaper_abandoned", m.Room.EndReason)
	}

	// And the code stops resolving immediately.
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); !errors.Is(err, rooms.ErrRoomNotFound) {
		t.Errorf("the code still resolved after the room emptied: %v", err)
	}
	_ = store
}

func TestLeaveRefusesANonParticipant(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	if _, err := svc.Leave(context.Background(), created.Room.ID, guestID); !errors.Is(err, rooms.ErrNotAParticipant) {
		t.Errorf("Leave by a stranger = %v, want ErrNotAParticipant", err)
	}
}

func TestLeaveOnAnUnknownRoom(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{})
	if _, err := svc.Leave(context.Background(), "99999999-9999-7999-8999-999999999999", hostID); !errors.Is(err, rooms.ErrRoomNotFound) {
		t.Errorf("Leave on an unknown room = %v, want ErrRoomNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

func TestTransitionFollowsTheStateMachine(t *testing.T) {
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	m, err := svc.Transition(ctx, created.Room.ID, hostID, rooms.EventArm)
	if err != nil {
		t.Fatalf("ARM: %v", err)
	}
	if m.Room.State != rooms.StateReady {
		t.Errorf("state = %q, want READY", m.Room.State)
	}

	m, err = svc.Transition(ctx, created.Room.ID, hostID, rooms.EventStart)
	if err != nil {
		t.Fatalf("START: %v", err)
	}
	if m.Room.State != rooms.StatePlaying {
		t.Errorf("state = %q, want PLAYING", m.Room.State)
	}

	var payload map[string]any
	if err := json.Unmarshal(m.Event.Payload, &payload); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if payload["from"] != "READY" || payload["to"] != "PLAYING" {
		t.Errorf("payload = %v; a state change must name both ends", payload)
	}
	_ = store
}

func TestTransitionRejectsAnIllegalMove(t *testing.T) {
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	// LOBBY has no START.
	var illegal rooms.ErrIllegalTransition
	if _, err := svc.Transition(ctx, created.Room.ID, hostID, rooms.EventStart); !errors.As(err, &illegal) {
		t.Fatalf("START from LOBBY = %v, want ErrIllegalTransition", err)
	}
	// And it must not have written an event for something that did not happen.
	if got := len(store.Events(created.Room.ID)); got != 1 {
		t.Errorf("%d events after a refused transition, want 1", got)
	}
}

func TestOnlyTheHostMayTransitionOrEnd(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("join: %v", err)
	}

	if _, err := svc.Transition(ctx, created.Room.ID, guestID, rooms.EventArm); !errors.Is(err, rooms.ErrNotTheHost) {
		t.Errorf("a guest arming = %v, want ErrNotTheHost", err)
	}
	if _, err := svc.End(ctx, created.Room.ID, guestID); !errors.Is(err, rooms.ErrNotTheHost) {
		t.Errorf("a guest ending = %v, want ErrNotTheHost", err)
	}
}

// ---------------------------------------------------------------------------
// End
// ---------------------------------------------------------------------------

func TestEndIsIdempotent(t *testing.T) {
	// A host double-tapping "end party" on a slow connection must not see a
	// failure for something that worked.
	svc, store, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	if _, err := svc.End(ctx, created.Room.ID, hostID); err != nil {
		t.Fatalf("first End: %v", err)
	}
	m, err := svc.End(ctx, created.Room.ID, hostID)
	if err != nil {
		t.Fatalf("second End: %v", err)
	}
	if m.Room.State != rooms.StateEnded {
		t.Errorf("state = %q, want ENDED", m.Room.State)
	}
	// Crucially: no second ROOM_ENDED. Clients that handled the first would
	// otherwise process the end twice.
	types := store.EventTypes(created.Room.ID)
	ended := 0
	for _, ty := range types {
		if ty == realtime.EventRoomEnded {
			ended++
		}
	}
	if ended != 1 {
		t.Errorf("%d ROOM_ENDED events, want exactly 1: %v", ended, types)
	}
}

// ---------------------------------------------------------------------------
// Get and list
// ---------------------------------------------------------------------------

func TestGetRequiresMembership(t *testing.T) {
	// FR-14: possession of a code is not access. A code shared into a group
	// chat months ago must not remain a permanent key.
	svc, _, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()

	if _, _, err := svc.Get(ctx, created.Room.ID, hostID); err != nil {
		t.Fatalf("the host cannot read their own room: %v", err)
	}

	// A stranger gets NOT_FOUND, not FORBIDDEN. Telling them "that room exists
	// but you are not in it" confirms the id, which is what an id-prober wants.
	if _, _, err := svc.Get(ctx, created.Room.ID, guestID); !errors.Is(err, rooms.ErrRoomNotFound) {
		t.Errorf("a stranger reading a room = %v, want ErrRoomNotFound (not Forbidden)", err)
	}

	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, _, err := svc.Get(ctx, created.Room.ID, guestID); err != nil {
		t.Errorf("a participant cannot read the room they joined: %v", err)
	}
}

func TestListForUserIsNewestFirstAndPaginates(t *testing.T) {
	svc, _, setNow := newRoomService(t, tiers{})
	ctx := context.Background()

	var created []*rooms.Mutation
	for i := range 5 {
		setNow(svcNow.Add(time.Duration(i) * time.Minute))
		m, err := svc.Create(ctx, rooms.CreateInput{HostUserID: hostID, ContentID: contentID})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		created = append(created, m)
	}

	page1, err := svc.ListForUser(ctx, hostID, nil, "", 2)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1 has %d rooms, want 2", len(page1))
	}
	// Newest first: the last created is first.
	if page1[0].ID != created[4].Room.ID {
		t.Errorf("page 1 starts with %q, want the newest %q", page1[0].ID, created[4].Room.ID)
	}

	last := page1[1]
	page2, err := svc.ListForUser(ctx, hostID, &last.CreatedAt, last.ID, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2 has %d rooms, want 2", len(page2))
	}
	// No overlap: keyset pagination's whole point.
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Errorf("room %q appears on both pages", a.ID)
			}
		}
	}
}

func TestListIncludesRoomsJoinedNotOnlyHosted(t *testing.T) {
	svc, _, _ := newRoomService(t, tiers{})
	created := mustCreate(t, svc)
	ctx := context.Background()
	if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, guestID); err != nil {
		t.Fatalf("join: %v", err)
	}

	got, err := svc.ListForUser(ctx, guestID, nil, "", 10)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(got) != 1 || got[0].ID != created.Room.ID {
		t.Errorf("a joined room did not appear in the participant's list: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Failure propagation
// ---------------------------------------------------------------------------

func TestStorageFailuresPropagate(t *testing.T) {
	boom := errors.New("connection refused")

	t.Run("create", func(t *testing.T) {
		for _, method := range []string{"ContentExists", "JoinCodeTaken", "CreateRoom"} {
			svc, store, _ := newRoomService(t, tiers{})
			store.FailNext[method] = boom
			if _, err := svc.Create(context.Background(), rooms.CreateInput{
				HostUserID: hostID, ContentID: contentID,
			}); !errors.Is(err, boom) {
				t.Errorf("Create with %s failing = %v, want the underlying error", method, err)
			}
		}
	})

	t.Run("join", func(t *testing.T) {
		for _, method := range []string{"RoomByJoinCode", "Participants", "NextSeq", "AddParticipant"} {
			svc, store, _ := newRoomService(t, tiers{})
			created := mustCreate(t, svc)
			store.FailNext[method] = boom
			if _, err := svc.JoinByCode(context.Background(), created.Room.JoinCode, guestID); !errors.Is(err, boom) {
				t.Errorf("Join with %s failing = %v, want the underlying error", method, err)
			}
		}
	})

	t.Run("leave", func(t *testing.T) {
		for _, method := range []string{"RoomByID", "Participants", "NextSeq", "RemoveParticipant"} {
			svc, store, _ := newRoomService(t, tiers{})
			created := mustCreate(t, svc)
			store.FailNext[method] = boom
			if _, err := svc.Leave(context.Background(), created.Room.ID, hostID); !errors.Is(err, boom) {
				t.Errorf("Leave with %s failing = %v, want the underlying error", method, err)
			}
		}
	})
}

func TestEntitlementLookupFailurePropagates(t *testing.T) {
	// A failing entitlement lookup must not silently fall back to a tier. The
	// permissive direction would hand a free account a premium-sized room; the
	// restrictive one would refuse a paying customer. Neither is acceptable
	// as a silent default, so it is an error.
	boom := errors.New("identity unavailable")
	store := roomstest.New()
	store.AddContent(contentID)
	svc := rooms.NewService(store, failingTiers{err: boom})

	if _, err := svc.Create(context.Background(), rooms.CreateInput{
		HostUserID: hostID, ContentID: contentID,
	}); !errors.Is(err, boom) {
		t.Errorf("Create with a failing entitlement lookup = %v, want the underlying error", err)
	}
}

func TestCodeGeneratorFailurePropagates(t *testing.T) {
	boom := errors.New("crypto/rand unavailable")
	svc, _, _ := newRoomService(t, tiers{})
	svc.SetCodeGenerator(func() (string, error) { return "", boom })
	if _, err := svc.Create(context.Background(), rooms.CreateInput{
		HostUserID: hostID, ContentID: contentID,
	}); !errors.Is(err, boom) {
		t.Errorf("Create with a broken code generator = %v, want the underlying error", err)
	}
}

func TestEverySequenceIsGapFreeAcrossAMixedWorkload(t *testing.T) {
	// The property AC-7 actually cares about, exercised through the real API
	// rather than by calling NextSeq directly. The fake panics on a gap, so
	// this also proves the service never skips an allocation.
	svc, store, setNow := newRoomService(t, tiers{hostID: "plus"})
	ctx := context.Background()
	created := mustCreate(t, svc)

	for i, id := range []string{guestID, thirdID, fourthID} {
		setNow(svcNow.Add(time.Duration(i+1) * time.Minute))
		if _, err := svc.JoinByCode(ctx, created.Room.JoinCode, id); err != nil {
			t.Fatalf("join %s: %v", id, err)
		}
	}
	if _, err := svc.Transition(ctx, created.Room.ID, hostID, rooms.EventArm); err != nil {
		t.Fatalf("ARM: %v", err)
	}
	if _, err := svc.Transition(ctx, created.Room.ID, hostID, rooms.EventStart); err != nil {
		t.Fatalf("START: %v", err)
	}
	if _, err := svc.Leave(ctx, created.Room.ID, thirdID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := svc.Leave(ctx, created.Room.ID, hostID); err != nil {
		t.Fatalf("host leave: %v", err)
	}
	if _, err := svc.End(ctx, created.Room.ID, guestID); err != nil {
		t.Fatalf("End by the successor: %v", err)
	}

	events := store.Events(created.Room.ID)
	if err := realtime.VerifyContiguous(events, 1); err != nil {
		t.Fatalf("the log has a gap: %v", err)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
		if err := e.Validate(); err != nil {
			t.Errorf("event %d (%s) is not a valid §7.2 envelope: %v", i, e.Type, err)
		}
		if !realtime.IsKnownEventType(e.Type) {
			t.Errorf("event %d has type %q, which no client will recognise", i, e.Type)
		}
	}
	if len(events) < 9 {
		t.Errorf("only %d events for a full room lifecycle; something is not being recorded", len(events))
	}
}
