package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var evtNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func validEnvelope(seq int64) Envelope {
	return Envelope{
		V:       EnvelopeVersion,
		ID:      fmt.Sprintf("0192f0c1-8a3e-7c4d-9b2a-%012d", seq),
		Room:    "room-1",
		Seq:     seq,
		Type:    "CHAT_MESSAGE",
		TS:      evtNow,
		Actor:   "user-1",
		Payload: json.RawMessage(`{"text":"hi"}`),
	}
}

func TestValidEnvelopePasses(t *testing.T) {
	if err := validEnvelope(1).Validate(); err != nil {
		t.Fatalf("a well-formed envelope was rejected: %v", err)
	}
}

func TestEnvelopeValidationRejectsEveryMissingMember(t *testing.T) {
	// FR-29 names eight members. Validating on the way OUT matters because
	// FR-33 requires clients to silently ignore what they cannot parse — so a
	// malformed event we emit would otherwise vanish without a trace.
	tests := []struct {
		name   string
		mutate func(*Envelope)
	}{
		{"wrong version", func(e *Envelope) { e.V = 2 }},
		{"zero version", func(e *Envelope) { e.V = 0 }},
		{"no id", func(e *Envelope) { e.ID = "" }},
		{"no room", func(e *Envelope) { e.Room = "" }},
		{"zero seq", func(e *Envelope) { e.Seq = 0 }},
		{"negative seq", func(e *Envelope) { e.Seq = -1 }},
		{"no type", func(e *Envelope) { e.Type = "" }},
		{"zero timestamp", func(e *Envelope) { e.TS = time.Time{} }},
		{"absent payload", func(e *Envelope) { e.Payload = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEnvelope(1)
			tt.mutate(&e)
			if err := e.Validate(); !errors.Is(err, ErrInvalidEnvelope) {
				t.Errorf("Validate() = %v, want ErrInvalidEnvelope", err)
			}
		})
	}
}

func TestAbsentActorIsAllowed(t *testing.T) {
	// Server-originated events — the reaper ending a room — have no actor.
	e := validEnvelope(1)
	e.Actor = ""
	if err := e.Validate(); err != nil {
		t.Errorf("a server-originated event was rejected: %v", err)
	}
}

func TestEmptyPayloadObjectIsAllowedButNullIsNot(t *testing.T) {
	// An absent payload and an empty object decode differently on the client.
	e := validEnvelope(1)
	e.Payload = json.RawMessage(`{}`)
	if err := e.Validate(); err != nil {
		t.Errorf("an empty payload object was rejected: %v", err)
	}

	e.Payload = nil
	if err := e.Validate(); err == nil {
		t.Error("an absent payload was accepted; it must be an explicit {}")
	}
}

// ---------------------------------------------------------------------------
// Sequences
// ---------------------------------------------------------------------------

func TestSequencesStartAtOne(t *testing.T) {
	// AC-7 asserts exactly 1..100 for a hundred events. Starting at 0 would be
	// off by one for the entire life of every room.
	if got := NextSeq(0); got != 1 {
		t.Errorf("the first event of a room got seq %d, want 1", got)
	}
}

func TestAHundredEventsAreExactlyOneToAHundred(t *testing.T) {
	// AC-7, stated literally.
	var seq int64
	events := make([]Envelope, 0, 100)
	for range 100 {
		seq = NextSeq(seq)
		e := validEnvelope(seq)
		if err := e.Validate(); err != nil {
			t.Fatalf("event %d failed §7.2 validation: %v", seq, err)
		}
		events = append(events, e)
	}

	if len(events) != 100 {
		t.Fatalf("emitted %d events, want 100", len(events))
	}
	if events[0].Seq != 1 || events[99].Seq != 100 {
		t.Errorf("sequence runs %d..%d, want 1..100", events[0].Seq, events[99].Seq)
	}
	if err := VerifyContiguous(events, 1); err != nil {
		t.Errorf("gap detected in a contiguous run: %v", err)
	}
}

func TestVerifyContiguousDetectsGaps(t *testing.T) {
	// Shipping a delta with a hole is worse than shipping a snapshot: the
	// client applies it, believes it is current, and stays silently wrong.
	tests := []struct {
		name      string
		seqs      []int64
		first     int64
		wantErr   bool
		wantExpct int64
	}{
		{"contiguous", []int64{5, 6, 7}, 5, false, 0},
		{"empty run", nil, 5, false, 0},
		{"single", []int64{5}, 5, false, 0},
		{"missing one in the middle", []int64{5, 7, 8}, 5, true, 6},
		{"wrong start", []int64{6, 7}, 5, true, 5},
		{"duplicate", []int64{5, 5, 6}, 5, true, 6},
		{"out of order", []int64{6, 5, 7}, 5, true, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := make([]Envelope, len(tt.seqs))
			for i, s := range tt.seqs {
				events[i] = validEnvelope(s)
			}
			err := VerifyContiguous(events, tt.first)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected gap: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a gap went undetected")
			}
			var gap ErrSeqGap
			if !errors.As(err, &gap) {
				t.Fatalf("err = %v, want ErrSeqGap", err)
			}
			if gap.Expected != tt.wantExpct {
				t.Errorf("gap.Expected = %d, want %d", gap.Expected, tt.wantExpct)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Resync
// ---------------------------------------------------------------------------

func TestAC8DeltaContainsExactlyTheMissedEvents(t *testing.T) {
	// AC-8, verbatim: last_seq 1400, current 1450 → events 1401 through 1450.
	got := DecideResync(1400, 1450, 1, DefaultDeltaThreshold)

	if got.Mode != ResyncDelta {
		t.Fatalf("mode = %v, want delta", got.Mode)
	}
	if got.FromSeq != 1401 {
		t.Errorf("FromSeq = %d, want 1401 — the delta is exclusive of last_seq", got.FromSeq)
	}
	if got.ToSeq != 1450 {
		t.Errorf("ToSeq = %d, want 1450 — inclusive of current_seq", got.ToSeq)
	}
	if n := got.ToSeq - got.FromSeq + 1; n != 50 {
		t.Errorf("delta spans %d events, want 50", n)
	}
}

func TestAC9OversizedGapYieldsASnapshot(t *testing.T) {
	// AC-9, verbatim: last_seq 1000, current 1500 → snapshot, because 500 > 200.
	got := DecideResync(1000, 1500, 1, DefaultDeltaThreshold)

	if got.Mode != ResyncSnapshot {
		t.Fatalf("mode = %v, want snapshot for a gap of 500", got.Mode)
	}
	if got.Reason == "" {
		t.Error("the reason is empty; a spike in snapshots must be diagnosable")
	}
}

func TestResyncThresholdBoundary(t *testing.T) {
	// Exactly at the threshold is a delta; one more is a snapshot. FR-31 says
	// "≤ 200", so 200 must not tip over.
	if got := DecideResync(0, 200, 1, 200); got.Mode != ResyncDelta {
		t.Errorf("a gap of exactly 200 gave %v, want delta (FR-31 says <=)", got.Mode)
	}
	if got := DecideResync(0, 201, 1, 200); got.Mode != ResyncSnapshot {
		t.Errorf("a gap of 201 gave %v, want snapshot", got.Mode)
	}
}

func TestThresholdIsConfigurationNotAConstant(t *testing.T) {
	// FR-31 requires this explicitly. The right value is a bandwidth/database
	// tradeoff that cannot be known before the §13.4 load test, and hard-coding
	// it would make a production tuning change need a deploy.
	if got := DecideResync(0, 50, 1, 10); got.Mode != ResyncSnapshot {
		t.Errorf("with threshold 10, a gap of 50 gave %v, want snapshot", got.Mode)
	}
	if got := DecideResync(0, 50, 1, 1000); got.Mode != ResyncDelta {
		t.Errorf("with threshold 1000, a gap of 50 gave %v, want delta", got.Mode)
	}
}

func TestRetentionForcesASnapshotEvenWhenTheGapIsSmall(t *testing.T) {
	// The gap is well within the threshold, but the events no longer exist.
	// Conflating size with retention would send a delta with a hole in it.
	got := DecideResync(100, 110, 105, DefaultDeltaThreshold)

	if got.Mode != ResyncSnapshot {
		t.Fatalf("mode = %v, want snapshot — events 101..104 have aged out", got.Mode)
	}
	if got.Reason == "" {
		t.Error("no reason recorded")
	}
}

func TestRetentionBoundaryIsInclusive(t *testing.T) {
	// The client needs lastSeq+1 onwards. If that is exactly the oldest
	// retained event, a delta is still correct.
	if got := DecideResync(100, 110, 101, DefaultDeltaThreshold); got.Mode != ResyncDelta {
		t.Errorf("mode = %v, want delta when oldestRetained == lastSeq+1", got.Mode)
	}
	if got := DecideResync(100, 110, 102, DefaultDeltaThreshold); got.Mode != ResyncSnapshot {
		t.Errorf("mode = %v, want snapshot when event 101 is gone", got.Mode)
	}
}

func TestResyncUpToDateAndAhead(t *testing.T) {
	if got := DecideResync(500, 500, 1, DefaultDeltaThreshold); got.Mode != ResyncUpToDate {
		t.Errorf("mode = %v, want up_to_date", got.Mode)
	}
	// A client ahead of the server is never normal — a stale replica, or
	// corrupt local state.
	got := DecideResync(600, 500, 1, DefaultDeltaThreshold)
	if got.Mode != ResyncInvalid {
		t.Errorf("mode = %v, want invalid when the client is ahead", got.Mode)
	}
}

func TestFirstEverResyncFromZero(t *testing.T) {
	// A brand-new client at last_seq 0 in a young room takes the delta path
	// and must receive event 1 onwards, not 0.
	got := DecideResync(0, 5, 1, DefaultDeltaThreshold)
	if got.Mode != ResyncDelta || got.FromSeq != 1 || got.ToSeq != 5 {
		t.Errorf("got %+v, want a delta of 1..5", got)
	}
}

func TestResyncModeStringsAreStable(t *testing.T) {
	// These land in logs and dashboards.
	want := map[ResyncMode]string{
		ResyncDelta:    "delta",
		ResyncSnapshot: "snapshot",
		ResyncUpToDate: "up_to_date",
		ResyncInvalid:  "invalid",
	}
	for m, s := range want {
		if got := m.String(); got != s {
			t.Errorf("%d.String() = %q, want %q", int(m), got, s)
		}
	}
}

// ---------------------------------------------------------------------------
// Dedupe
// ---------------------------------------------------------------------------

func TestAC12SameIdAppliedExactlyOnce(t *testing.T) {
	d := NewDedupeLRU(DedupeCapacity)

	if d.Seen("evt-1") {
		t.Fatal("the first delivery was reported as a duplicate")
	}
	if !d.Seen("evt-1") {
		t.Error("the second delivery was not detected; AC-12 requires exactly-once application")
	}
	if !d.Seen("evt-1") {
		t.Error("a third delivery slipped through")
	}
}

func TestDedupeIsBounded(t *testing.T) {
	// An unbounded set grows for the life of the room — the kind of leak that
	// only surfaces in the 30-minute session NFR-7 measures.
	d := NewDedupeLRU(10)
	for i := range 1000 {
		d.Seen(fmt.Sprintf("evt-%d", i))
	}
	if d.Len() > 10 {
		t.Errorf("LRU holds %d ids, want at most 10", d.Len())
	}
}

func TestDedupeEvictsOldestFirst(t *testing.T) {
	d := NewDedupeLRU(3)
	d.Seen("a")
	d.Seen("b")
	d.Seen("c")

	// Full. Adding "d" must evict "a".
	d.Seen("d")

	if d.Seen("a") {
		t.Error("the oldest id was still remembered after eviction")
	}
	// "a" was just re-inserted by that call, which evicted "b".
	if d.Seen("b") {
		t.Error("eviction order is not FIFO")
	}
}

func TestDedupeCapacityDefaultsToFR34(t *testing.T) {
	for _, capacity := range []int{0, -1, -500} {
		d := NewDedupeLRU(capacity)
		for i := range DedupeCapacity + 50 {
			d.Seen(fmt.Sprintf("evt-%d", i))
		}
		if d.Len() != DedupeCapacity {
			t.Errorf("NewDedupeLRU(%d) held %d ids, want FR-34's %d", capacity, d.Len(), DedupeCapacity)
		}
	}
}

func TestDedupeCapacityIsFiveHundred(t *testing.T) {
	if DedupeCapacity != 500 {
		t.Errorf("DedupeCapacity = %d, want 500 (FR-34)", DedupeCapacity)
	}
}

func TestDedupeKeepsRecentIdsAcrossManyEvents(t *testing.T) {
	// The property that actually matters: a duplicate arriving shortly after
	// the original is caught, even in a busy room.
	d := NewDedupeLRU(500)
	for i := range 400 {
		d.Seen(fmt.Sprintf("evt-%d", i))
	}
	if !d.Seen("evt-399") {
		t.Error("the most recent id was forgotten")
	}
	if !d.Seen("evt-0") {
		t.Error("an id well within capacity was forgotten")
	}
}

// ---------------------------------------------------------------------------
// Unknown types (FR-33)
// ---------------------------------------------------------------------------

func TestUnknownEventTypeIsNotRecognisedButIsStillAValidEnvelope(t *testing.T) {
	// AC-11: a client receiving FUTURE_FEATURE_XYZ logs it, ignores it, and
	// keeps the socket open. So the envelope itself must still validate — the
	// type vocabulary is checked by the EMITTER, not used to reject inbound.
	e := validEnvelope(1)
	e.Type = "FUTURE_FEATURE_XYZ"

	if err := e.Validate(); err != nil {
		t.Errorf("an unknown type failed envelope validation: %v", err)
	}
	if IsKnownEventType("FUTURE_FEATURE_XYZ") {
		t.Error("FUTURE_FEATURE_XYZ was reported as known")
	}
}

func TestKnownEventTypesCoverTheM1Vocabulary(t *testing.T) {
	// A typo in an emitted type would otherwise be silently ignored by every
	// client in the field, per FR-33.
	for _, want := range []string{
		"ROOM_STATE_CHANGED", "PARTICIPANT_JOINED", "PARTICIPANT_LEFT",
		"HOST_CHANGED", "SYNC_ARM", "TIMELINE_ANCHORED", "CHAT_MESSAGE",
		"REACTION_BUCKET", "TRIVIA_START", "QUESTION_OPEN", "QUESTION_CLOSE",
		"ROOM_ENDED",
	} {
		if !IsKnownEventType(want) {
			t.Errorf("%q is not in the known vocabulary", want)
		}
	}
	if IsKnownEventType("") || IsKnownEventType("chat_message") {
		t.Error("the vocabulary is case-insensitive or accepts empty types")
	}
}

func TestSeqGapErrorNamesBothNumbers(t *testing.T) {
	// This message is what a "delta had a hole" incident is diagnosed from.
	err := VerifyContiguous([]Envelope{validEnvelope(7)}, 5)
	if err == nil {
		t.Fatal("expected a gap")
	}
	msg := err.Error()
	if !strings.Contains(msg, "5") || !strings.Contains(msg, "7") {
		t.Errorf("error %q names neither the expected nor the received seq", msg)
	}
}
