// Package realtime owns the shared timeline, the event log, and resync.
//
// This file implements the clock discipline that ADR-002 rests on. VYBE cannot
// see the user's video player, so the only thing every participant can agree on
// is a server-authoritative clock. Everything timed — trivia beats, prediction
// windows, spoiler-gated chat — fires against that clock.
//
// The consequence worth stating plainly, because it is the answer to interview
// question §16.4.2: a participant whose device clock is five minutes wrong
// experiences nothing unusual. The offset absorbs it.
package realtime

import (
	"errors"
	"sort"
	"time"
)

// §7.4 constants. These are product decisions, not tuning knobs, so they live
// here next to the logic that gives them meaning.
const (
	// OffsetSampleWindow is how many PING/PONG round trips we keep. Five is
	// enough to survive a couple of bad samples without making the window so
	// long that a genuine network change takes a minute to show up.
	OffsetSampleWindow = 5

	// MaxAcceptableRTT — beyond this a sample tells us nothing useful about
	// the offset, because the asymmetry between the two legs could be larger
	// than the value we are trying to measure.
	MaxAcceptableRTT = 2 * time.Second

	// BeatTolerance — a timed event is valid only within this window of its
	// target. Outside it the event is skipped and logged as drift, never fired
	// late: a trivia question about a scene that played twenty seconds ago is
	// worse than no question at all.
	BeatTolerance = 1500 * time.Millisecond
)

var (
	// ErrNoUsableSamples means every sample was discarded. The connection is
	// degraded and timed events must be suppressed (EC-13) — firing them at an
	// unknown offset is worse than not firing them.
	ErrNoUsableSamples = errors.New("realtime: no usable clock samples")
)

// Sample is one completed PING/PONG round trip, §7.4:
//
//	client → PING { t0 }
//	server → PONG { t0, t1, t2 }
//	client   t3
//
// t0 and t3 are read from the client clock; t1 and t2 from the server clock.
// The whole point is that we never need those two clocks to agree — we measure
// the difference.
type Sample struct {
	T0 time.Time // client: PING sent
	T1 time.Time // server: PING received
	T2 time.Time // server: PONG sent
	T3 time.Time // client: PONG received
}

// RTT is the round trip excluding the time the server spent thinking.
//
// Subtracting (t2 - t1) matters: without it, a slow server looks like a slow
// network and inflates the RTT we use to pick the best sample and to
// compensate trivia timing.
func (s Sample) RTT() time.Duration {
	return s.T3.Sub(s.T0) - s.T2.Sub(s.T1)
}

// Offset is how far the client clock sits behind the server clock. Add it to a
// client instant to get server time.
//
//	offset = ((t1 - t0) + (t2 - t3)) / 2
//
// This is NTP's estimator. It is exact when the two network legs are
// symmetric, and wrong by half the asymmetry when they are not — which is
// precisely why we prefer the lowest-RTT sample rather than the mean.
func (s Sample) Offset() time.Duration {
	return (s.T1.Sub(s.T0) + s.T2.Sub(s.T3)) / 2
}

// Usable reports whether the sample is trustworthy enough to act on.
//
// A negative RTT means the timestamps are incoherent (a clock stepped mid
// round trip, or a client is lying). Either way the sample is meaningless.
func (s Sample) Usable() bool {
	rtt := s.RTT()
	return rtt >= 0 && rtt <= MaxAcceptableRTT
}

// ClockEstimator maintains the rolling sample window for one connection.
//
// The zero value is ready to use.
type ClockEstimator struct {
	samples []Sample // most recent last; capped at OffsetSampleWindow
}

// Observe records a completed round trip.
//
// Unusable samples are still stored, so that "all five samples were bad" is
// distinguishable from "we have no samples yet" — the first is a degraded
// connection, the second is a connection that has not finished handshaking.
func (e *ClockEstimator) Observe(s Sample) {
	e.samples = append(e.samples, s)
	if len(e.samples) > OffsetSampleWindow {
		e.samples = e.samples[len(e.samples)-OffsetSampleWindow:]
	}
}

// Best returns the sample with the lowest RTT among usable samples.
//
// §7.4 is explicit that this is the lowest-RTT sample and NOT the mean. The
// reason: offset error is bounded by the path asymmetry, and asymmetry
// correlates with delay. One 800ms sample among four 45ms samples is not a
// datum to average in — it is noise, and averaging spreads its error across
// every subsequent calculation.
func (e *ClockEstimator) Best() (Sample, error) {
	var best Sample
	found := false
	for _, s := range e.samples {
		if !s.Usable() {
			continue
		}
		if !found || s.RTT() < best.RTT() {
			best, found = s, true
		}
	}
	if !found {
		return Sample{}, ErrNoUsableSamples
	}
	return best, nil
}

// Offset returns the current best estimate of client-to-server clock offset.
func (e *ClockEstimator) Offset() (time.Duration, error) {
	best, err := e.Best()
	if err != nil {
		return 0, err
	}
	return best.Offset(), nil
}

// Degraded reports whether the connection should be treated as unable to keep
// time (FR-24). True when we have samples but none are usable.
//
// A degraded connection still carries chat and reactions; it just must not
// schedule timed events (EC-13).
func (e *ClockEstimator) Degraded() bool {
	if len(e.samples) == 0 {
		return false // not degraded — simply not measured yet
	}
	for _, s := range e.samples {
		if s.Usable() {
			return false
		}
	}
	return true
}

// SampleCount reports how many samples are in the window.
func (e *ClockEstimator) SampleCount() int { return len(e.samples) }

// Stats summarises the window for the §14.2 room-lifecycle dashboard. Drift
// distribution across real users is the only evidence that will ever tell us
// whether Companion Sync actually works in the field (risk R1).
type Stats struct {
	Samples   int
	Usable    int
	BestRTT   time.Duration
	MedianRTT time.Duration
	Offset    time.Duration
	Degraded  bool
}

func (e *ClockEstimator) Stats() Stats {
	st := Stats{Samples: len(e.samples), Degraded: e.Degraded()}

	rtts := make([]time.Duration, 0, len(e.samples))
	for _, s := range e.samples {
		if s.Usable() {
			rtts = append(rtts, s.RTT())
			st.Usable++
		}
	}
	if len(rtts) == 0 {
		return st
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	st.BestRTT = rtts[0]
	st.MedianRTT = rtts[len(rtts)/2]
	if off, err := e.Offset(); err == nil {
		st.Offset = off
	}
	return st
}
