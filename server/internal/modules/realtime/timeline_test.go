package realtime

import (
	"errors"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)

// --- AC-3: lowest RTT wins, the mean does not -------------------------------

// SPEC-001 AC-3, verbatim: samples with RTTs of 40, 45, 800, 60, 50 ms must
// select the 40ms sample, and the 800ms sample must not be averaged in.
func TestClockEstimator_PicksLowestRTTNotMean(t *testing.T) {
	var e ClockEstimator

	// Each sample carries a distinguishable offset so we can prove WHICH one
	// was selected, not merely that the number looks plausible.
	specs := []struct {
		rtt    time.Duration
		offset time.Duration
	}{
		{40 * time.Millisecond, 1000 * time.Millisecond},
		{45 * time.Millisecond, 1200 * time.Millisecond},
		{800 * time.Millisecond, 9000 * time.Millisecond}, // the outlier
		{60 * time.Millisecond, 1300 * time.Millisecond},
		{50 * time.Millisecond, 1100 * time.Millisecond},
	}
	for _, s := range specs {
		e.Observe(makeSample(s.rtt, s.offset))
	}

	best, err := e.Best()
	if err != nil {
		t.Fatalf("Best(): %v", err)
	}
	if got := best.RTT(); got != 40*time.Millisecond {
		t.Fatalf("selected RTT = %v, want 40ms", got)
	}

	got, err := e.Offset()
	if err != nil {
		t.Fatalf("Offset(): %v", err)
	}
	if !within(got, 1000*time.Millisecond, time.Millisecond) {
		t.Fatalf("offset = %v, want ~1s (the 40ms sample's offset)", got)
	}

	// Guard against a future "improvement" to averaging. The mean of those
	// five offsets is 2.72s; if this ever passes, the outlier got in.
	if within(got, 2720*time.Millisecond, 100*time.Millisecond) {
		t.Fatal("offset looks like the MEAN of all samples; §7.4 requires lowest-RTT selection")
	}
}

// --- AC-4: RTT > 2s is discarded and the connection is degraded -------------

func TestClockEstimator_DiscardsHighRTTAndMarksDegraded(t *testing.T) {
	var e ClockEstimator
	e.Observe(makeSample(2500*time.Millisecond, 3*time.Second))

	if _, err := e.Best(); !errors.Is(err, ErrNoUsableSamples) {
		t.Fatalf("Best() err = %v, want ErrNoUsableSamples", err)
	}
	if !e.Degraded() {
		t.Fatal("connection should be degraded when every sample exceeds MaxAcceptableRTT")
	}

	// One good sample must rescue it.
	e.Observe(makeSample(50*time.Millisecond, 250*time.Millisecond))
	if e.Degraded() {
		t.Fatal("a usable sample arrived; connection should no longer be degraded")
	}
	off, err := e.Offset()
	if err != nil {
		t.Fatal(err)
	}
	if !within(off, 250*time.Millisecond, time.Millisecond) {
		t.Fatalf("offset = %v, want ~250ms", off)
	}
}

// "No samples yet" is a different state from "every sample was bad". Treating
// a freshly-connected client as degraded would suppress timed events during
// the handshake.
func TestClockEstimator_NoSamplesIsNotDegraded(t *testing.T) {
	var e ClockEstimator
	if e.Degraded() {
		t.Fatal("an unmeasured connection must not report as degraded")
	}
	if _, err := e.Offset(); !errors.Is(err, ErrNoUsableSamples) {
		t.Fatalf("err = %v, want ErrNoUsableSamples", err)
	}
}

func TestClockEstimator_WindowIsBounded(t *testing.T) {
	var e ClockEstimator
	for i := 0; i < 50; i++ {
		e.Observe(makeSample(time.Duration(i+10)*time.Millisecond, time.Second))
	}
	if got := e.SampleCount(); got != OffsetSampleWindow {
		t.Fatalf("window holds %d samples, want %d", got, OffsetSampleWindow)
	}
}

// A negative RTT means the timestamps are incoherent — a clock stepped
// mid-round-trip, or a client is fabricating them. Either way, unusable.
func TestSample_NegativeRTTIsUnusable(t *testing.T) {
	s := Sample{
		T0: base,
		T1: base.Add(-5 * time.Second),
		T2: base.Add(5 * time.Second),
		T3: base.Add(10 * time.Millisecond),
	}
	if s.Usable() {
		t.Fatalf("sample with RTT %v must be unusable", s.RTT())
	}
}

// Server think-time must not be counted as network delay.
func TestSample_RTTExcludesServerProcessingTime(t *testing.T) {
	s := Sample{
		T0: base,
		T1: base.Add(20 * time.Millisecond),  // 20ms out
		T2: base.Add(120 * time.Millisecond), // 100ms of server work
		T3: base.Add(140 * time.Millisecond), // 20ms back
	}
	if got := s.RTT(); got != 40*time.Millisecond {
		t.Fatalf("RTT = %v, want 40ms (140ms wall clock minus 100ms server time)", got)
	}
}

// --- AC-2: the five-minutes-wrong device clock ------------------------------

// This is the headline claim of ADR-002 and interview answer §16.4.2. If this
// test ever fails, the product's central promise is broken.
func TestTimeline_DeviceClockFiveMinutesWrongStillReadsCorrectPosition(t *testing.T) {
	tl := Timeline{AnchorServerTime: base}

	const skew = -5 * time.Minute // device believes it is 5 minutes earlier

	// Real server instant: 90 seconds into the programme.
	serverNow := base.Add(90 * time.Second)

	// The device's own reading of that instant, and the offset a PING/PONG
	// against that same wrong clock would measure. Note they are the same
	// magnitude with opposite sign — that is the mechanism.
	clientNow := serverNow.Add(skew)
	measuredOffset := -skew

	got, err := tl.PositionForClient(clientNow, measuredOffset)
	if err != nil {
		t.Fatalf("PositionForClient: %v", err)
	}
	if got != 90*time.Second {
		t.Fatalf("position = %v, want 90s despite a 5-minute clock error", got)
	}

	// And a client that ignored the offset would be catastrophically wrong —
	// this asserts the correction is doing real work, not that the test is
	// trivially satisfiable.
	naive, err := tl.PositionAt(clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if naive == 90*time.Second {
		t.Fatal("uncorrected position matched; the test is not exercising the correction")
	}
}

// Two devices with wildly different clock errors must agree on the room
// position — this is AC-1's ±250ms convergence requirement, at the unit level.
func TestTimeline_TwoSkewedDevicesAgreeWithin250ms(t *testing.T) {
	tl := Timeline{AnchorServerTime: base}
	serverNow := base.Add(42 * time.Second)

	devices := []struct {
		name string
		skew time.Duration
	}{
		{"5 minutes slow", -5 * time.Minute},
		{"3 hours fast", 3 * time.Hour},
		{"37ms slow", -37 * time.Millisecond},
		{"exact", 0},
	}

	var first time.Duration
	for i, d := range devices {
		pos, err := tl.PositionForClient(serverNow.Add(d.skew), -d.skew)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		if i == 0 {
			first = pos
			continue
		}
		if !within(pos, first, 250*time.Millisecond) {
			t.Fatalf("%s: position %v diverges from %v by more than 250ms", d.name, pos, first)
		}
	}
}

func TestTimeline_UnanchoredRoomHasNoPosition(t *testing.T) {
	var tl Timeline
	if tl.Started() {
		t.Fatal("zero Timeline must not report as started")
	}
	// Returning 0 here would read as "at the very beginning", which is a
	// different and wrong claim.
	if _, err := tl.PositionAt(base); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
}

func TestTimeline_AnchorOffsetStartsMidProgramme(t *testing.T) {
	tl := Timeline{AnchorServerTime: base, AnchorOffset: 10 * time.Minute}
	pos, err := tl.PositionAt(base.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if want := 10*time.Minute + 30*time.Second; pos != want {
		t.Fatalf("position = %v, want %v", pos, want)
	}
}

func TestTimeline_ServerTimeForPositionInvertsPositionAt(t *testing.T) {
	tl := Timeline{AnchorServerTime: base, AnchorOffset: 2 * time.Minute}
	for _, pos := range []time.Duration{0, 2 * time.Minute, 17 * time.Minute, 90 * time.Minute} {
		at, err := tl.ServerTimeForPosition(pos)
		if err != nil {
			t.Fatal(err)
		}
		back, err := tl.PositionAt(at)
		if err != nil {
			t.Fatal(err)
		}
		if back != pos {
			t.Fatalf("round trip changed %v into %v", pos, back)
		}
	}
}

// --- AC-6: re-anchoring converges every client ------------------------------

func TestTimeline_ReanchorConvergesAllClients(t *testing.T) {
	tl := Timeline{AnchorServerTime: base}
	reanchorAt := base.Add(10 * time.Minute)
	const truePosition = 8 * time.Minute // host reports the room is 2 min ahead

	tl = tl.Reanchor(reanchorAt, truePosition)

	skews := []time.Duration{-5 * time.Minute, 2 * time.Hour, 0, -800 * time.Millisecond}
	serverNow := reanchorAt.Add(15 * time.Second)

	for _, skew := range skews {
		pos, err := tl.PositionForClient(serverNow.Add(skew), -skew)
		if err != nil {
			t.Fatal(err)
		}
		if want := truePosition + 15*time.Second; !within(pos, want, 250*time.Millisecond) {
			t.Fatalf("skew %v: position %v, want ~%v", skew, pos, want)
		}
	}
}

// --- AC-5: beat tolerance, and never firing late ----------------------------

func TestEvaluateBeat(t *testing.T) {
	const target = 60 * time.Second

	cases := []struct {
		name    string
		current time.Duration
		want    BeatDecision
	}{
		{"well before", 55 * time.Second, BeatPending},
		{"just before tolerance edge", target - BeatTolerance - time.Millisecond, BeatPending},
		{"at early tolerance edge", target - BeatTolerance, BeatFire},
		{"exactly on target", target, BeatFire},
		{"at late tolerance edge", target + BeatTolerance, BeatFire},
		// AC-5 verbatim: t_room = 62s against a 60s target must SKIP.
		{"AC-5: 2s late", 62 * time.Second, BeatSkip},
		{"well past", 90 * time.Second, BeatSkip},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EvaluateBeat(c.current, target); got != c.want {
				t.Fatalf("EvaluateBeat(%v, %v) = %v, want %v", c.current, target, got, c.want)
			}
		})
	}
}

// The asymmetry is the product decision: early means wait, late means give up.
// A symmetric rule would fire spoilers into a room that had moved on.
func TestEvaluateBeat_LateNeverFires(t *testing.T) {
	const target = 30 * time.Second
	for late := BeatTolerance + time.Millisecond; late < 5*time.Minute; late += 137 * time.Millisecond {
		if got := EvaluateBeat(target+late, target); got != BeatSkip {
			t.Fatalf("%v late: got %v, want skip", late, got)
		}
	}
}

// --- FR-26 / AC-6: drift consensus ------------------------------------------

func TestEvaluateDriftConsensus(t *testing.T) {
	now := base

	t.Run("AC-6: 3 of 5 agree beyond threshold prompts the host", func(t *testing.T) {
		got := EvaluateDriftConsensus([]DriftReport{
			{UserID: "a", DriftMS: 9000, Reported: now},
			{UserID: "b", DriftMS: 9500, Reported: now},
			{UserID: "c", DriftMS: 10000, Reported: now},
		}, 5)
		if !got.ShouldPromptHost {
			t.Fatal("3 of 5 (60% > 40%) beyond 8s should prompt a re-anchor")
		}
		if got.AgreeingUsers != 3 {
			t.Fatalf("AgreeingUsers = %d, want 3", got.AgreeingUsers)
		}
		if want := 9500 * time.Millisecond; got.SuggestedShift != want {
			t.Fatalf("SuggestedShift = %v, want the median %v", got.SuggestedShift, want)
		}
	})

	t.Run("40% exactly does not trigger; the rule is strictly greater", func(t *testing.T) {
		got := EvaluateDriftConsensus([]DriftReport{
			{UserID: "a", DriftMS: 9000, Reported: now},
			{UserID: "b", DriftMS: 9000, Reported: now},
		}, 5)
		if got.ShouldPromptHost {
			t.Fatal("2 of 5 is exactly 40%; §7.4 says MORE than 40%")
		}
	})

	t.Run("drift below 8s never counts", func(t *testing.T) {
		got := EvaluateDriftConsensus([]DriftReport{
			{UserID: "a", DriftMS: 7999, Reported: now},
			{UserID: "b", DriftMS: 7000, Reported: now},
			{UserID: "c", DriftMS: 5000, Reported: now},
		}, 4)
		if got.ShouldPromptHost {
			t.Fatal("sub-threshold drift must not trigger a re-anchor")
		}
	})

	// A room split half ahead and half behind is not consensus. Re-anchoring
	// to the average would put everybody in the wrong place.
	t.Run("opposite directions do not combine", func(t *testing.T) {
		got := EvaluateDriftConsensus([]DriftReport{
			{UserID: "a", DriftMS: 9000, Reported: now},
			{UserID: "b", DriftMS: 9000, Reported: now},
			{UserID: "c", DriftMS: -9000, Reported: now},
			{UserID: "d", DriftMS: -9000, Reported: now},
		}, 5)
		if got.ShouldPromptHost {
			t.Fatal("2 ahead and 2 behind is a split room, not consensus")
		}
	})

	// One frustrated participant tapping repeatedly must not be able to
	// re-anchor the room on their own.
	t.Run("only the latest report per user counts", func(t *testing.T) {
		got := EvaluateDriftConsensus([]DriftReport{
			{UserID: "a", DriftMS: 9000, Reported: now},
			{UserID: "a", DriftMS: 9100, Reported: now.Add(time.Second)},
			{UserID: "a", DriftMS: 9200, Reported: now.Add(2 * time.Second)},
			{UserID: "a", DriftMS: 9300, Reported: now.Add(3 * time.Second)},
			{UserID: "a", DriftMS: 9400, Reported: now.Add(4 * time.Second)},
		}, 5)
		if got.ShouldPromptHost {
			t.Fatal("five taps from one user must count as one opinion")
		}
		if got.TotalReporters != 1 {
			t.Fatalf("TotalReporters = %d, want 1", got.TotalReporters)
		}
	})

	t.Run("negative direction reaches consensus too", func(t *testing.T) {
		got := EvaluateDriftConsensus([]DriftReport{
			{UserID: "a", DriftMS: -9000, Reported: now},
			{UserID: "b", DriftMS: -11000, Reported: now},
			{UserID: "c", DriftMS: -10000, Reported: now},
		}, 4)
		if !got.ShouldPromptHost {
			t.Fatal("3 of 4 behind by >8s should prompt a re-anchor")
		}
		if want := -10 * time.Second; got.SuggestedShift != want {
			t.Fatalf("SuggestedShift = %v, want %v", got.SuggestedShift, want)
		}
	})

	t.Run("empty and degenerate inputs are safe", func(t *testing.T) {
		if EvaluateDriftConsensus(nil, 5).ShouldPromptHost {
			t.Fatal("no reports must not prompt")
		}
		if EvaluateDriftConsensus([]DriftReport{{UserID: "a", DriftMS: 20000}}, 0).ShouldPromptHost {
			t.Fatal("zero participants must not divide by zero or prompt")
		}
	})
}

// --- helpers -----------------------------------------------------------------

// makeSample builds a round trip with an exact RTT and an exact client→server
// offset, so tests can assert on which sample was chosen rather than on a
// plausible-looking number.
//
// Construction: the client sends at T0. The true server instant of that moment
// is T0+offset. One network leg is rtt/2.
func makeSample(rtt, offset time.Duration) Sample {
	t0 := base
	t1 := t0.Add(offset).Add(rtt / 2)
	t2 := t1 // no server think-time, so RTT() == rtt exactly
	t3 := t0.Add(rtt)
	return Sample{T0: t0, T1: t1, T2: t2, T3: t3}
}

func within(got, want, tolerance time.Duration) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}
