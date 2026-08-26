package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The event log and resync decision (ADR-003, FR-28–35).
//
// The room's shared state is a gap-free append-only sequence, and a client is
// nothing more than a position in it. That is what makes reconnection a
// solvable problem rather than a guess: a client that knows its `last_seq` can
// always be told, exactly, what it missed — or told that the answer is too
// large and here is the whole state instead.
//
// The alternative, broadcasting state diffs with no ordering, cannot survive a
// 30-second tunnel, which is precisely the scenario AC-13 makes mandatory.

// EnvelopeVersion is the `v` member of §7.2. It is 1 and changing it is a
// protocol break, so it is a named constant rather than a literal at each
// construction site.
const EnvelopeVersion = 1

// Envelope is §7.2's event envelope (FR-29).
//
// Every field is required except Actor, which is absent for server-originated
// events such as the reaper ending a room. Making that the only optional member
// keeps "which fields can I trust?" answerable.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"` // uuidv7 — the client's dedupe key (FR-34)
	Room    string          `json:"room"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	TS      time.Time       `json:"ts"`
	Actor   string          `json:"actor,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// ErrInvalidEnvelope means an envelope failed §7.2 validation.
var ErrInvalidEnvelope = errors.New("realtime: envelope does not satisfy §7.2")

// Validate enforces the §7.2 schema (FR-29, AC-7).
//
// Checked on the way OUT, not just on the way in. An event that fails here is a
// bug in our own emitter, and finding it at the boundary is far cheaper than
// finding it in a client's crash report — especially since FR-33 requires
// clients to silently ignore what they cannot parse, which means a malformed
// event would otherwise vanish without a trace.
func (e Envelope) Validate() error {
	switch {
	case e.V != EnvelopeVersion:
		return fmt.Errorf("%w: v = %d, want %d", ErrInvalidEnvelope, e.V, EnvelopeVersion)
	case e.ID == "":
		return fmt.Errorf("%w: id is empty, so clients cannot dedupe (FR-34)", ErrInvalidEnvelope)
	case e.Room == "":
		return fmt.Errorf("%w: room is empty", ErrInvalidEnvelope)
	case e.Seq < 1:
		return fmt.Errorf("%w: seq = %d, must be >= 1 (FR-28)", ErrInvalidEnvelope, e.Seq)
	case e.Type == "":
		return fmt.Errorf("%w: type is empty", ErrInvalidEnvelope)
	case e.TS.IsZero():
		return fmt.Errorf("%w: ts is zero", ErrInvalidEnvelope)
	case len(e.Payload) == 0:
		// An absent payload and an empty object are different things, and the
		// client's decoder will treat them differently. Require the explicit
		// `{}` rather than accepting null.
		return fmt.Errorf("%w: payload is absent; use {} for no data", ErrInvalidEnvelope)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sequence allocation (FR-28)
// ---------------------------------------------------------------------------

// ErrSeqGap reports a break in the sequence.
type ErrSeqGap struct {
	Expected int64
	Got      int64
}

func (e ErrSeqGap) Error() string {
	return fmt.Sprintf("realtime: seq gap — expected %d, got %d", e.Expected, e.Got)
}

// NextSeq returns the sequence number that must follow current.
//
// Sequences start at 1, so a room that has emitted nothing is at 0 and its
// first event is 1. AC-7 asserts exactly 1..100 for a hundred events, and
// starting at 0 would make that off by one for the whole life of the room.
//
// The real allocation is `UPDATE rooms SET current_seq = current_seq + 1
// RETURNING current_seq` inside the same transaction as the event insert —
// that is what makes it gap-free under concurrency. This function is the rule
// the transaction implements, kept here so it can be reasoned about and tested
// without a database.
func NextSeq(current int64) int64 { return current + 1 }

// VerifyContiguous checks that a run of events has no gaps (FR-28, AC-7).
//
// Used by the resync path before sending a delta. Shipping a delta with a hole
// in it is worse than shipping a snapshot: the client applies it, believes it
// is caught up, and stays silently wrong until something else forces a resync.
func VerifyContiguous(events []Envelope, expectedFirst int64) error {
	want := expectedFirst
	for _, e := range events {
		if e.Seq != want {
			return ErrSeqGap{Expected: want, Got: e.Seq}
		}
		want++
	}
	return nil
}

// ---------------------------------------------------------------------------
// Resync (FR-30, FR-31)
// ---------------------------------------------------------------------------

// ResyncMode is what the server owes a reconnecting client.
type ResyncMode int

const (
	// ResyncDelta — send events (lastSeq, currentSeq].
	ResyncDelta ResyncMode = iota
	// ResyncSnapshot — the gap is too large, or the events have aged out of
	// retention. Send the whole state instead.
	ResyncSnapshot
	// ResyncUpToDate — nothing to send.
	ResyncUpToDate
	// ResyncInvalid — the client claims a position ahead of the server's.
	ResyncInvalid
)

func (m ResyncMode) String() string {
	switch m {
	case ResyncDelta:
		return "delta"
	case ResyncSnapshot:
		return "snapshot"
	case ResyncUpToDate:
		return "up_to_date"
	default:
		return "invalid"
	}
}

// DefaultDeltaThreshold is FR-31's 200.
//
// FR-31 requires this be **configuration, not a constant** — hence
// DecideResync taking it as a parameter. This value is only the default, and
// the reason the requirement insists is that the right threshold is a tradeoff
// between bandwidth and database work that cannot be known before the load test
// (§13.4). Hard-coding it would mean a production tuning change needs a deploy.
const DefaultDeltaThreshold = 200

// ResyncDecision is the outcome of a RESYNC request.
type ResyncDecision struct {
	Mode ResyncMode

	// FromSeq and ToSeq bound the delta, exclusive of From and inclusive of To.
	// AC-8: last_seq 1400, current 1450 → events 1401..1450.
	FromSeq int64
	ToSeq   int64

	// Reason is recorded in logs so a spike in snapshots is diagnosable rather
	// than mysterious — snapshots are far more expensive than deltas, and a
	// sudden shift to them is an incident signal.
	Reason string
}

// DecideResync implements FR-31.
//
// `oldestRetained` is the lowest seq still in `room_events`. A client whose
// position predates it cannot be served a delta at any threshold, because the
// events it missed no longer exist — retention, not size, is the binding
// constraint in that case, and conflating the two would send a delta with a
// hole in it.
func DecideResync(lastSeq, currentSeq, oldestRetained int64, threshold int) ResyncDecision {
	switch {
	case lastSeq > currentSeq:
		// The client is ahead of the server. Either it is talking to a stale
		// replica, or its state is corrupt. A snapshot is the only safe answer,
		// but it is flagged separately because it is never normal.
		return ResyncDecision{
			Mode:   ResyncInvalid,
			Reason: "client last_seq is ahead of the room's current_seq",
		}

	case lastSeq == currentSeq:
		return ResyncDecision{Mode: ResyncUpToDate, Reason: "already current"}

	case currentSeq-lastSeq > int64(threshold):
		return ResyncDecision{
			Mode:   ResyncSnapshot,
			Reason: fmt.Sprintf("gap of %d exceeds the threshold of %d", currentSeq-lastSeq, threshold),
		}

	case lastSeq+1 < oldestRetained:
		// The gap is small enough, but the events are gone.
		return ResyncDecision{
			Mode:   ResyncSnapshot,
			Reason: fmt.Sprintf("events from %d have aged out; oldest retained is %d", lastSeq+1, oldestRetained),
		}

	default:
		return ResyncDecision{
			Mode:    ResyncDelta,
			FromSeq: lastSeq + 1,
			ToSeq:   currentSeq,
			Reason:  fmt.Sprintf("gap of %d is within the threshold of %d", currentSeq-lastSeq, threshold),
		}
	}
}

// ---------------------------------------------------------------------------
// Client-side dedupe (FR-34) — server-side too, for the outbox
// ---------------------------------------------------------------------------

// DedupeCapacity is FR-34's 500.
const DedupeCapacity = 500

// DedupeLRU remembers recently seen envelope ids (FR-34, AC-12).
//
// Bounded on purpose. An unbounded set would grow for the lifetime of a room
// and is the kind of leak that only shows up in the 30-minute session NFR-7
// measures — by which point it is a memory graph nobody can explain.
//
// Not safe for concurrent use; each connection owns one.
type DedupeLRU struct {
	capacity int
	seen     map[string]int
	order    []string
	next     int
}

// NewDedupeLRU returns an LRU with the given capacity, or FR-34's 500 when
// capacity is not positive.
func NewDedupeLRU(capacity int) *DedupeLRU {
	if capacity <= 0 {
		capacity = DedupeCapacity
	}
	return &DedupeLRU{
		capacity: capacity,
		seen:     make(map[string]int, capacity),
		order:    make([]string, 0, capacity),
	}
}

// Seen records an id and reports whether it had already been seen.
//
// Returns true when the event is a duplicate and must NOT be applied — AC-12
// requires the state change to happen exactly once.
func (d *DedupeLRU) Seen(id string) bool {
	if _, dup := d.seen[id]; dup {
		return true
	}

	if len(d.order) < d.capacity {
		d.order = append(d.order, id)
		d.seen[id] = len(d.order) - 1
		return false
	}

	// Full: evict the oldest slot and reuse it. A ring buffer rather than a
	// slice shuffle, so eviction is O(1) and a busy room does not spend its
	// time copying a 500-element slice on every event.
	evicted := d.order[d.next]
	delete(d.seen, evicted)
	d.order[d.next] = id
	d.seen[id] = d.next
	d.next = (d.next + 1) % d.capacity
	return false
}

// Len reports how many ids are currently remembered.
func (d *DedupeLRU) Len() int { return len(d.seen) }

// ---------------------------------------------------------------------------
// Unknown event types (FR-33)
// ---------------------------------------------------------------------------

// KnownEventTypes is the M1 vocabulary.
//
// Its existence is not a whitelist for *receiving* — FR-33 requires an unknown
// type to be ignored and logged, never rejected, so that a server can ship a
// new event before every client understands it. This set is what the emitter
// validates against, so a typo in an emitted type is caught here rather than
// silently ignored by every client in the field.
// The event vocabulary as constants.
//
// Emitters reference these rather than string literals, so a typo is a compile
// error instead of an event that every client silently ignores under FR-33.
// That failure mode is the reason to bother: an unknown type is *by design*
// dropped without complaint, which means a misspelled emitted type produces no
// error anywhere — not in the server, not in the client, not in the logs.
const (
	EventRoomStateChanged  = "ROOM_STATE_CHANGED"
	EventParticipantJoined = "PARTICIPANT_JOINED"
	EventParticipantLeft   = "PARTICIPANT_LEFT"
	EventHostChanged       = "HOST_CHANGED"
	EventSyncArm           = "SYNC_ARM"
	EventTimelineAnchored  = "TIMELINE_ANCHORED"
	EventTimelineReanchor  = "TIMELINE_REANCHOR"
	EventChatMessage       = "CHAT_MESSAGE"
	EventReactionBucket    = "REACTION_BUCKET"
	EventTriviaStart       = "TRIVIA_START"
	EventQuestionOpen      = "QUESTION_OPEN"
	EventQuestionClose     = "QUESTION_CLOSE"
	EventScoreboardUpdate  = "SCOREBOARD_UPDATE"
	EventRoomEnded         = "ROOM_ENDED"
	EventPresenceChanged   = "PRESENCE_CHANGED"
)

var KnownEventTypes = map[string]bool{
	EventRoomStateChanged:  true,
	EventParticipantJoined: true,
	EventParticipantLeft:   true,
	EventHostChanged:       true,
	EventSyncArm:           true,
	EventTimelineAnchored:  true,
	EventTimelineReanchor:  true,
	EventChatMessage:       true,
	EventReactionBucket:    true,
	EventTriviaStart:       true,
	EventQuestionOpen:      true,
	EventQuestionClose:     true,
	EventScoreboardUpdate:  true,
	EventRoomEnded:         true,
	EventPresenceChanged:   true,
}

// IsKnownEventType reports whether the emitter recognises a type.
func IsKnownEventType(t string) bool { return KnownEventTypes[t] }
