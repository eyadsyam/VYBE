package rooms

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var roomNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

// The exhaustive transition matrix. FR-15 names four states; with five events
// that is twenty pairs, and every one is either allowed here or asserted to be
// rejected. A table like this is the only way to be sure "what else can happen
// from READY?" has a complete answer.
func TestEveryStateEventPair(t *testing.T) {
	allStates := []State{StateLobby, StateReady, StatePlaying, StateEnded}
	allEvents := []Event{EventArm, EventStart, EventReanchor, EventEnd, EventCancel}

	allowed := map[State]map[Event]State{
		StateLobby:   {EventArm: StateReady, EventEnd: StateEnded},
		StateReady:   {EventStart: StatePlaying, EventCancel: StateLobby, EventEnd: StateEnded},
		StatePlaying: {EventReanchor: StateReady, EventEnd: StateEnded},
		StateEnded:   {},
	}

	for _, from := range allStates {
		for _, event := range allEvents {
			t.Run(string(from)+"/"+string(event), func(t *testing.T) {
				want, isAllowed := allowed[from][event]
				got, err := Next(from, event)

				if !isAllowed {
					if err == nil {
						t.Fatalf("Next(%s, %s) = %s; FR-15 requires this to be rejected 409", from, event, got)
					}
					var illegal ErrIllegalTransition
					if !errors.As(err, &illegal) {
						t.Fatalf("err = %v, want ErrIllegalTransition so the handler can map it to 409", err)
					}
					if illegal.From != from || illegal.Event != event {
						t.Errorf("error names %s/%s, want %s/%s", illegal.From, illegal.Event, from, event)
					}
					return
				}
				if err != nil {
					t.Fatalf("Next(%s, %s) failed: %v", from, event, err)
				}
				if got != want {
					t.Errorf("Next(%s, %s) = %s, want %s", from, event, got, want)
				}
			})
		}
	}
}

func TestEndedIsTerminal(t *testing.T) {
	// FR-15 says ENDED is terminal. Even END must not be accepted from it: a
	// second end would emit a second ROOM_ENDED to clients that already
	// handled the first.
	for _, event := range []Event{EventArm, EventStart, EventReanchor, EventEnd, EventCancel} {
		if _, err := Next(StateEnded, event); err == nil {
			t.Errorf("%s was accepted from ENDED", event)
		}
	}
	if !IsTerminal(StateEnded) {
		t.Error("IsTerminal(ENDED) = false")
	}
	for _, s := range []State{StateLobby, StateReady, StatePlaying} {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = true", s)
		}
	}
}

func TestArmingTwiceIsRejected(t *testing.T) {
	// The concrete reason FR-15 exists. A room armed twice broadcasts two
	// countdowns, and clients holding the first are left with an anchor the
	// server has forgotten.
	ready, err := Next(StateLobby, EventArm)
	if err != nil {
		t.Fatalf("first arm: %v", err)
	}
	if _, err := Next(ready, EventArm); err == nil {
		t.Fatal("a second ARM was accepted")
	}
}

func TestReanchorReturnsToReadySoTheRitualReplays(t *testing.T) {
	// AC-6: re-anchoring must converge every client. Jumping straight back to
	// PLAYING would resync the server clock while leaving every human out of
	// step — the countdown is the mechanism by which people actually align.
	got, err := Next(StatePlaying, EventReanchor)
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	if got != StateReady {
		t.Errorf("REANCHOR from PLAYING = %s, want READY", got)
	}
}

func TestCanTransitionAgreesWithNext(t *testing.T) {
	for _, from := range []State{StateLobby, StateReady, StatePlaying, StateEnded} {
		for _, e := range []Event{EventArm, EventStart, EventReanchor, EventEnd, EventCancel} {
			_, err := Next(from, e)
			if got, want := CanTransition(from, e), err == nil; got != want {
				t.Errorf("CanTransition(%s,%s) = %v but Next err = %v", from, e, got, err)
			}
		}
	}
}

func TestUnknownStateIsRejected(t *testing.T) {
	// A row carrying a state this build does not know about must not be
	// transitioned on a guess.
	if _, err := Next(State("SOMETHING_ELSE"), EventEnd); err == nil {
		t.Error("an unknown state was transitioned")
	}
}

// ---------------------------------------------------------------------------
// Capacity
// ---------------------------------------------------------------------------

func TestMaxParticipantsByTier(t *testing.T) {
	if got := MaxParticipants("free"); got != 4 {
		t.Errorf("free cap = %d, want 4 (FR-16)", got)
	}
	if got := MaxParticipants("plus"); got != 8 {
		t.Errorf("plus cap = %d, want 8", got)
	}
}

func TestUnknownTierGetsTheFreeCapNotTheGenerousOne(t *testing.T) {
	// Failing closed: a typo should cost a paying user two seats, not hand
	// every user the paid limit.
	for _, tier := range []string{"", "PLUS", "Plus", "premium", "gold", "free "} {
		if got := MaxParticipants(tier); got != FreeTierMaxParticipants {
			t.Errorf("MaxParticipants(%q) = %d, want the free cap %d", tier, got, FreeTierMaxParticipants)
		}
	}
}

func TestCanJoin(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		tier    string
		current int
		wantErr error
	}{
		{"empty free room", StateLobby, "free", 0, nil},
		{"free room with a seat left", StateLobby, "free", 3, nil},
		{"free room at the cap", StateLobby, "free", 4, ErrRoomFull},
		{"free room over the cap", StateLobby, "free", 9, ErrRoomFull},
		{"plus room at the free cap still has room", StateLobby, "plus", 4, nil},
		{"plus room at its own cap", StateLobby, "plus", 8, ErrRoomFull},
		{"joining mid-playing is allowed (EC-14)", StatePlaying, "free", 1, nil},
		{"joining a ready room is allowed", StateReady, "free", 1, nil},
		{"ended beats full", StateEnded, "free", 0, ErrRoomEnded},
		{"ended beats full, even when full", StateEnded, "free", 99, ErrRoomEnded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CanJoin(tt.state, tt.tier, tt.current)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CanJoin = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCapIsTheHostsTierNotTheJoiners(t *testing.T) {
	// If the cap depended on who was arriving, it would depend on join order —
	// which is indefensible to explain to either user.
	if err := CanJoin(StateLobby, "free", 4); !errors.Is(err, ErrRoomFull) {
		t.Error("a free host's room admitted a fifth participant")
	}
	if err := CanJoin(StateLobby, "plus", 4); err != nil {
		t.Errorf("a plus host's room rejected a fifth participant: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Host succession (FR-17)
// ---------------------------------------------------------------------------

func TestSuccessorForRespectsTheGraceWindow(t *testing.T) {
	base := roomNow.Add(-time.Hour)
	mk := func(disconnectedAgo time.Duration) []Participant {
		gone := roomNow.Add(-disconnectedAgo)
		return []Participant{
			{UserID: "host", JoinedAt: base, IsHost: true, Connected: false, DisconnectedAt: &gone},
			{UserID: "early", JoinedAt: base.Add(time.Minute), Connected: true},
			{UserID: "late", JoinedAt: base.Add(2 * time.Minute), Connected: true},
		}
	}

	tests := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"just disconnected", time.Second, ""},
		{"a tunnel, still inside the window", 59 * time.Second, ""},
		{"exactly at the boundary", HostGraceWindow, "early"},
		{"well past the window", 5 * time.Minute, "early"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SuccessorFor(mk(tt.ago), roomNow); got != tt.want {
				t.Errorf("SuccessorFor = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGraceWindowIsSixtySeconds(t *testing.T) {
	// FR-17 states the number, and it is chosen for a reason: a five-second
	// window would make the host badge flicker every time somebody walked past
	// a lift shaft.
	if HostGraceWindow != 60*time.Second {
		t.Errorf("HostGraceWindow = %v, want 60s (FR-17)", HostGraceWindow)
	}
}

func TestConnectedHostIsNeverReplaced(t *testing.T) {
	base := roomNow.Add(-time.Hour)
	got := SuccessorFor([]Participant{
		{UserID: "host", JoinedAt: base, IsHost: true, Connected: true},
		{UserID: "other", JoinedAt: base.Add(time.Minute), Connected: true},
	}, roomNow)
	if got != "" {
		t.Errorf("SuccessorFor promoted %q over a connected host", got)
	}
}

func TestSuccessionPicksLongestTenuredConnected(t *testing.T) {
	base := roomNow.Add(-time.Hour)
	gone := roomNow.Add(-5 * time.Minute)

	got := SuccessorFor([]Participant{
		{UserID: "host", JoinedAt: base, IsHost: true, Connected: false, DisconnectedAt: &gone},
		// Longest-tenured, but disconnected — must be skipped.
		{UserID: "oldest-but-offline", JoinedAt: base.Add(time.Second), Connected: false},
		{UserID: "second", JoinedAt: base.Add(2 * time.Minute), Connected: true},
		{UserID: "third", JoinedAt: base.Add(3 * time.Minute), Connected: true},
	}, roomNow)

	if got != "second" {
		t.Errorf("SuccessorFor = %q, want %q — the longest-tenured CONNECTED participant", got, "second")
	}
}

func TestSuccessionIsDeterministicOnATie(t *testing.T) {
	// Two instances evaluating the same room must agree, or the room gets two
	// hosts and every later authority check is a coin toss.
	base := roomNow.Add(-time.Hour)
	gone := roomNow.Add(-5 * time.Minute)
	sameInstant := base.Add(time.Minute)

	participants := []Participant{
		{UserID: "host", JoinedAt: base, IsHost: true, Connected: false, DisconnectedAt: &gone},
		{UserID: "zzz", JoinedAt: sameInstant, Connected: true},
		{UserID: "aaa", JoinedAt: sameInstant, Connected: true},
		{UserID: "mmm", JoinedAt: sameInstant, Connected: true},
	}

	first := SuccessorFor(participants, roomNow)
	if first != "aaa" {
		t.Errorf("tie broken to %q, want the lowest user id %q", first, "aaa")
	}
	// Order of the slice must not change the answer.
	reordered := []Participant{participants[0], participants[3], participants[1], participants[2]}
	if second := SuccessorFor(reordered, roomNow); second != first {
		t.Errorf("re-ordering the slice changed the successor: %q then %q", first, second)
	}
}

func TestSuccessionWithNobodyToPromote(t *testing.T) {
	base := roomNow.Add(-time.Hour)
	gone := roomNow.Add(-5 * time.Minute)

	tests := []struct {
		name         string
		participants []Participant
	}{
		{"empty room", nil},
		{"host only", []Participant{
			{UserID: "host", JoinedAt: base, IsHost: true, Connected: false, DisconnectedAt: &gone},
		}},
		{"everyone else is offline too", []Participant{
			{UserID: "host", JoinedAt: base, IsHost: true, Connected: false, DisconnectedAt: &gone},
			{UserID: "b", JoinedAt: base.Add(time.Minute), Connected: false},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SuccessorFor(tt.participants, roomNow); got != "" {
				t.Errorf("SuccessorFor = %q, want no promotion", got)
			}
		})
	}
}

func TestDisconnectedHostWithNoTimestampIsNotPromotedOver(t *testing.T) {
	// Incomplete information is not grounds for a promotion: without a
	// timestamp there is no way to know the grace window has elapsed.
	base := roomNow.Add(-time.Hour)
	got := SuccessorFor([]Participant{
		{UserID: "host", JoinedAt: base, IsHost: true, Connected: false},
		{UserID: "other", JoinedAt: base.Add(time.Minute), Connected: true},
	}, roomNow)
	if got != "" {
		t.Errorf("SuccessorFor = %q; a host with no disconnect timestamp must not be replaced", got)
	}
}

func TestSuccessionWithNoHostRowAtAll(t *testing.T) {
	// The host row can legitimately be gone. Somebody still has to be host.
	base := roomNow.Add(-time.Hour)
	got := SuccessorFor([]Participant{
		{UserID: "b", JoinedAt: base.Add(2 * time.Minute), Connected: true},
		{UserID: "a", JoinedAt: base.Add(time.Minute), Connected: true},
	}, roomNow)
	if got != "a" {
		t.Errorf("SuccessorFor = %q, want %q", got, "a")
	}
}

// ---------------------------------------------------------------------------
// Reaping (FR-18)
// ---------------------------------------------------------------------------

func TestShouldReap(t *testing.T) {
	tests := []struct {
		name       string
		state      State
		emptySince *time.Time
		want       bool
	}{
		{"not empty", StatePlaying, nil, false},
		{"empty for a moment", StatePlaying, ptr(roomNow.Add(-time.Second)), false},
		{"empty for nine minutes", StatePlaying, ptr(roomNow.Add(-9 * time.Minute)), false},
		{"empty for exactly ten", StatePlaying, ptr(roomNow.Add(-EmptyRoomReapAfter)), true},
		{"empty for an hour", StateLobby, ptr(roomNow.Add(-time.Hour)), true},
		{"already ended is never reaped twice", StateEnded, ptr(roomNow.Add(-time.Hour)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldReap(tt.state, tt.emptySince, roomNow); got != tt.want {
				t.Errorf("ShouldReap = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReapWindowIsTenMinutes(t *testing.T) {
	if EmptyRoomReapAfter != 10*time.Minute {
		t.Errorf("EmptyRoomReapAfter = %v, want 10m (FR-18)", EmptyRoomReapAfter)
	}
}

func TestIllegalTransitionErrorNamesBothSides(t *testing.T) {
	// The message goes into the problem document's `detail` and into logs. It
	// has to say which transition was refused, or a 409 in production is a
	// puzzle rather than a diagnosis.
	_, err := Next(StatePlaying, EventArm)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, string(StatePlaying)) || !strings.Contains(msg, string(EventArm)) {
		t.Errorf("error %q names neither the state nor the event", msg)
	}
}
