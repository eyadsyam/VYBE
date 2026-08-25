package rooms

import (
	"fmt"
	"time"
)

// State mirrors the `room_state` enum in migration 0001.
type State string

const (
	StateLobby   State = "LOBBY"
	StateReady   State = "READY"
	StatePlaying State = "PLAYING"
	StateEnded   State = "ENDED"
)

// Event is something that may change a room's state.
type Event string

const (
	// EventArm is the host arming the sync ritual (FR-19).
	EventArm Event = "ARM"
	// EventStart is the countdown reaching zero: the room begins PLAYING.
	EventStart Event = "START"
	// EventReanchor restarts the ritual from PLAYING (FR-24 / AC-6).
	EventReanchor Event = "REANCHOR"
	// EventEnd is the host ending the room, or the reaper (FR-18).
	EventEnd Event = "END"
	// EventCancel is the host abandoning an armed ritual before it fires.
	EventCancel Event = "CANCEL"
)

// transitions is FR-15's state machine, written as data.
//
// A table rather than a switch because the legal set is the specification: it
// can be read in one glance, diffed in review, and — in TestEveryTransition —
// enumerated exhaustively so that every pair of (state, event) is either
// explicitly allowed or explicitly proven to be rejected. A nest of ifs makes
// "what else can happen from READY?" a question you answer by reading code.
var transitions = map[State]map[Event]State{
	StateLobby: {
		EventArm: StateReady,
		EventEnd: StateEnded,
	},
	StateReady: {
		EventStart:  StatePlaying,
		EventCancel: StateLobby,
		EventEnd:    StateEnded,
	},
	StatePlaying: {
		// Re-anchoring returns to READY so the whole ritual replays: the
		// countdown is the mechanism by which humans actually align, and
		// skipping it would resync the server's clock while leaving every
		// person out of step (AC-6).
		EventReanchor: StateReady,
		EventEnd:      StateEnded,
	},
	// ENDED is terminal. FR-15 says so, and the empty map is how that is
	// enforced — there is no event, including END, that leaves this state.
	StateEnded: {},
}

// ErrIllegalTransition is what FR-15 requires be surfaced as a 409.
type ErrIllegalTransition struct {
	From  State
	Event Event
}

func (e ErrIllegalTransition) Error() string {
	return fmt.Sprintf("rooms: cannot %s a room in state %s", e.Event, e.From)
}

// Next applies an event to a state (FR-15).
//
// Rejecting an illegal transition is not defensive programming; it is the
// requirement. A room that can be armed twice broadcasts two countdowns, and
// clients that received the first are then holding an anchor the server has
// forgotten.
func Next(from State, event Event) (State, error) {
	allowed, known := transitions[from]
	if !known {
		return "", ErrIllegalTransition{From: from, Event: event}
	}
	to, ok := allowed[event]
	if !ok {
		return "", ErrIllegalTransition{From: from, Event: event}
	}
	return to, nil
}

// CanTransition reports whether an event is legal, without producing an error.
func CanTransition(from State, event Event) bool {
	_, err := Next(from, event)
	return err == nil
}

// IsTerminal reports whether a room can never change state again.
func IsTerminal(s State) bool { return s == StateEnded }

// ---------------------------------------------------------------------------
// Capacity (FR-16)
// ---------------------------------------------------------------------------

// Participant caps by entitlement tier (§1.8).
const (
	FreeTierMaxParticipants = 4
	PlusTierMaxParticipants = 8
)

// MaxParticipants returns the cap for a tier (FR-16).
//
// An unknown tier gets the FREE cap, not the generous one. Failing closed
// matters here: a typo in a tier string should cost a paying user two seats,
// not hand every user the paid limit.
func MaxParticipants(entitlementTier string) int {
	if entitlementTier == "plus" {
		return PlusTierMaxParticipants
	}
	return FreeTierMaxParticipants
}

// ErrRoomFull is FR-16's rejection, surfaced as 409 ROOM_FULL.
var ErrRoomFull = fmt.Errorf("rooms: room is full")

// ErrRoomEnded is the 409 ROOM_ENDED case.
var ErrRoomEnded = fmt.Errorf("rooms: room has ended")

// CanJoin decides whether a user may be admitted (FR-15, FR-16).
//
// The cap is evaluated against **the host's** tier, because the host is who
// created the room and whose entitlement it is. Evaluating it against the
// joiner would let a free-tier user be admitted to a paid room's fifth seat, or
// a Plus user raise a free host's ceiling by arriving — the cap would depend on
// join order, which is indefensible to explain to either of them.
func CanJoin(state State, hostTier string, currentParticipants int) error {
	if state == StateEnded {
		return ErrRoomEnded
	}
	if currentParticipants >= MaxParticipants(hostTier) {
		return ErrRoomFull
	}
	return nil
}

// ---------------------------------------------------------------------------
// Host succession (FR-17) and reaping (FR-18)
// ---------------------------------------------------------------------------

// HostGraceWindow is FR-17's 60 seconds.
//
// Long enough to cover a tunnel, a lift, or an app backgrounded to answer a
// message — all of which happen constantly during exactly the activity VYBE
// exists for. Promoting after five seconds would make the host badge flicker
// between people every time somebody walked past a lift shaft.
const HostGraceWindow = 60 * time.Second

// EmptyRoomReapAfter is FR-18's 10 minutes.
const EmptyRoomReapAfter = 10 * time.Minute

// Participant is the subset of a room member the succession rules need.
type Participant struct {
	UserID         string
	JoinedAt       time.Time
	Connected      bool
	IsHost         bool
	DisconnectedAt *time.Time
}

// SuccessorFor picks the new host when the current one has been gone too long
// (FR-17).
//
// Returns "" when no promotion is due — the host is connected, still inside the
// grace window, or there is nobody to promote.
//
// "Longest-tenured connected participant" is the rule, and the tiebreak is the
// user id so the choice is deterministic. Determinism is not pedantry: two
// server instances evaluating the same room must reach the same answer, or the
// room gets two hosts and every subsequent authority check is a coin toss.
func SuccessorFor(participants []Participant, now time.Time) string {
	var host *Participant
	for i := range participants {
		if participants[i].IsHost {
			host = &participants[i]
			break
		}
	}
	// No host at all is a legitimate state — the host row may already have been
	// removed — and still calls for a promotion.
	if host != nil {
		if host.Connected {
			return ""
		}
		if host.DisconnectedAt == nil {
			// Marked disconnected without a timestamp: treat as still inside
			// the window rather than promoting on incomplete information.
			return ""
		}
		if now.Sub(*host.DisconnectedAt) < HostGraceWindow {
			return ""
		}
	}

	var best *Participant
	for i := range participants {
		p := &participants[i]
		if p.IsHost || !p.Connected {
			continue
		}
		if best == nil ||
			p.JoinedAt.Before(best.JoinedAt) ||
			(p.JoinedAt.Equal(best.JoinedAt) && p.UserID < best.UserID) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.UserID
}

// ShouldReap reports whether a room must be ended (FR-18).
//
// `emptySince` is when the last participant disconnected; a nil value means the
// room is not empty. An already-ENDED room is never reaped again, because
// re-ending it would emit a second ROOM_ENDED event to clients that already
// handled the first.
func ShouldReap(state State, emptySince *time.Time, now time.Time) bool {
	if state == StateEnded || emptySince == nil {
		return false
	}
	return now.Sub(*emptySince) >= EmptyRoomReapAfter
}
