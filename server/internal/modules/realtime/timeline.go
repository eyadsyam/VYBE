package realtime

import (
	"errors"
	"time"
)

// Timeline is the shared virtual clock that Companion Sync synchronises
// instead of a video stream (ADR-002).
//
//	t_room = (server_now - AnchorServerTime) + AnchorOffset
//
// AnchorOffset exists so a room can start part-way into a programme — the host
// re-anchors at a known position and everyone converges there (FR-26).
//
// Nothing here knows about video, because VYBE never touches video. That is
// the whole design.
type Timeline struct {
	AnchorServerTime time.Time
	AnchorOffset     time.Duration
}

// ErrNotStarted is returned for a room that has no anchor yet — a room in
// LOBBY or READY has no meaningful position, and returning zero would be a lie
// that reads as "at the very beginning".
var ErrNotStarted = errors.New("realtime: timeline has no anchor")

// Started reports whether the timeline has been anchored.
func (t Timeline) Started() bool { return !t.AnchorServerTime.IsZero() }

// PositionAt returns the room position for a given SERVER instant.
//
// Callers on the client side must pass a corrected instant — see
// PositionForClient — never a raw device clock reading.
func (t Timeline) PositionAt(serverNow time.Time) (time.Duration, error) {
	if !t.Started() {
		return 0, ErrNotStarted
	}
	return serverNow.Sub(t.AnchorServerTime) + t.AnchorOffset, nil
}

// PositionForClient converts a raw client-clock reading into a room position
// by applying the measured offset first.
//
// This is the function that makes interview answer §16.4.2 true: pass a
// clientNow that is five minutes wrong together with the offset measured for
// that same wrong clock, and the result is correct. The two errors are the
// same error, and they cancel.
func (t Timeline) PositionForClient(clientNow time.Time, offset time.Duration) (time.Duration, error) {
	return t.PositionAt(clientNow.Add(offset))
}

// ServerTimeForPosition is the inverse: when will (or did) the room reach this
// position? Used to schedule timed events locally against the corrected clock.
func (t Timeline) ServerTimeForPosition(pos time.Duration) (time.Time, error) {
	if !t.Started() {
		return time.Time{}, ErrNotStarted
	}
	return t.AnchorServerTime.Add(pos - t.AnchorOffset), nil
}

// Reanchor returns a new Timeline pinned so that the given server instant
// corresponds to the given room position (FR-26).
//
// Returning a new value rather than mutating in place keeps the type safe to
// share across goroutines and makes the transition auditable — the old anchor
// is still in the caller's hands for the TIMELINE_REANCHOR event payload.
func (t Timeline) Reanchor(at time.Time, pos time.Duration) Timeline {
	return Timeline{AnchorServerTime: at, AnchorOffset: pos}
}

// --- Timed events ------------------------------------------------------------

// BeatDecision is the outcome of testing whether a timed event may fire.
type BeatDecision int

const (
	// BeatPending — the target has not arrived yet. Wait.
	BeatPending BeatDecision = iota
	// BeatFire — within tolerance. Fire now.
	BeatFire
	// BeatSkip — the target has passed by more than the tolerance. Skip and
	// log as drift (FR-27). Never fire late.
	BeatSkip
)

func (d BeatDecision) String() string {
	switch d {
	case BeatPending:
		return "pending"
	case BeatFire:
		return "fire"
	case BeatSkip:
		return "skip"
	}
	return "unknown"
}

// EvaluateBeat decides whether an event targeted at firesAt may fire, given
// the current room position.
//
// The asymmetry is deliberate and is the product decision, not an oversight:
// arriving early means waiting, arriving late past the tolerance means giving
// up. A trivia question about a twist that played twenty seconds ago actively
// spoils the room; showing nothing does not.
func EvaluateBeat(current, firesAt time.Duration) BeatDecision {
	delta := current - firesAt
	switch {
	case delta < -BeatTolerance:
		return BeatPending
	case delta > BeatTolerance:
		return BeatSkip
	default:
		return BeatFire
	}
}

// --- Drift consensus ----------------------------------------------------------

// §7.4: if more than 40% of participants report drift in the same direction
// exceeding 8s, prompt the host to re-anchor.
const (
	DriftConsensusFraction  = 0.40
	DriftConsensusThreshold = 8 * time.Second
)

// DriftReport is one participant's self-reported error. Negative means the
// participant believes they are behind the room.
type DriftReport struct {
	UserID   string
	DriftMS  int64
	Reported time.Time
}

// DriftConsensus is the result of evaluating recent reports.
type DriftConsensus struct {
	ShouldPromptHost bool
	// SuggestedShift is the median drift among the agreeing reports. The
	// median, not the mean, because a single wildly wrong self-report from a
	// user who wandered off should not drag the correction with it.
	SuggestedShift time.Duration
	AgreeingUsers  int
	TotalReporters int
}

// EvaluateDriftConsensus implements FR-26.
//
// Only the most recent report per user counts: a user who taps the drift
// button five times is one opinion, not five, and counting taps would let one
// frustrated participant re-anchor the whole room.
func EvaluateDriftConsensus(reports []DriftReport, participantCount int) DriftConsensus {
	res := DriftConsensus{}
	if participantCount <= 0 {
		return res
	}

	latest := make(map[string]DriftReport, len(reports))
	for _, r := range reports {
		if prev, ok := latest[r.UserID]; !ok || r.Reported.After(prev.Reported) {
			latest[r.UserID] = r
		}
	}
	res.TotalReporters = len(latest)

	var ahead, behind []time.Duration
	for _, r := range latest {
		d := time.Duration(r.DriftMS) * time.Millisecond
		switch {
		case d >= DriftConsensusThreshold:
			ahead = append(ahead, d)
		case d <= -DriftConsensusThreshold:
			behind = append(behind, d)
		}
	}

	// Directions are evaluated separately. Half the room ahead and half behind
	// is not consensus — it is a room that has come apart, and re-anchoring to
	// the average would put everybody in the wrong place.
	group := ahead
	if len(behind) > len(ahead) {
		group = behind
	}
	if len(group) == 0 {
		return res
	}

	res.AgreeingUsers = len(group)
	if float64(len(group))/float64(participantCount) > DriftConsensusFraction {
		res.ShouldPromptHost = true
		res.SuggestedShift = medianDuration(group)
	}
	return res
}

func medianDuration(in []time.Duration) time.Duration {
	if len(in) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(in))
	copy(sorted, in)
	for i := 1; i < len(sorted); i++ { // insertion sort; these slices are tiny
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
