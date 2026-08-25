package realtime

import (
	"errors"
	"testing"
	"time"
)

// Stats feeds the §14.2 room-lifecycle dashboard. Drift distribution across
// real users is the only evidence that will ever tell us whether Companion
// Sync works in the field (risk R1), so the numbers have to be right.
func TestClockEstimator_Stats(t *testing.T) {
	t.Run("empty estimator", func(t *testing.T) {
		var e ClockEstimator
		got := e.Stats()
		if got.Samples != 0 || got.Usable != 0 || got.Degraded {
			t.Fatalf("empty Stats() = %+v, want zero and not degraded", got)
		}
		if got.BestRTT != 0 || got.MedianRTT != 0 || got.Offset != 0 {
			t.Fatalf("empty Stats() should report zero durations, got %+v", got)
		}
	})

	t.Run("mixed usable and unusable", func(t *testing.T) {
		var e ClockEstimator
		e.Observe(makeSample(40*time.Millisecond, time.Second))
		e.Observe(makeSample(60*time.Millisecond, time.Second))
		e.Observe(makeSample(3*time.Second, 9*time.Second)) // discarded, RTT > 2s
		e.Observe(makeSample(80*time.Millisecond, time.Second))

		got := e.Stats()
		if got.Samples != 4 {
			t.Fatalf("Samples = %d, want 4 (unusable samples are still counted)", got.Samples)
		}
		if got.Usable != 3 {
			t.Fatalf("Usable = %d, want 3", got.Usable)
		}
		if got.BestRTT != 40*time.Millisecond {
			t.Fatalf("BestRTT = %v, want 40ms", got.BestRTT)
		}
		if got.MedianRTT != 60*time.Millisecond {
			t.Fatalf("MedianRTT = %v, want 60ms", got.MedianRTT)
		}
		if !within(got.Offset, time.Second, time.Millisecond) {
			t.Fatalf("Offset = %v, want ~1s", got.Offset)
		}
		if got.Degraded {
			t.Fatal("three usable samples: must not be degraded")
		}
	})

	t.Run("all unusable reports degraded with zero RTTs", func(t *testing.T) {
		var e ClockEstimator
		e.Observe(makeSample(3*time.Second, time.Second))
		e.Observe(makeSample(4*time.Second, time.Second))

		got := e.Stats()
		if !got.Degraded {
			t.Fatal("every sample exceeded MaxAcceptableRTT: must be degraded")
		}
		if got.Usable != 0 {
			t.Fatalf("Usable = %d, want 0", got.Usable)
		}
		if got.BestRTT != 0 || got.Offset != 0 {
			t.Fatalf("degraded Stats() must not report a fabricated RTT/offset, got %+v", got)
		}
	})
}

func TestServerTimeForPosition_UnanchoredIsAnError(t *testing.T) {
	var tl Timeline
	if _, err := tl.ServerTimeForPosition(30 * time.Second); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
}

func TestBeatDecision_String(t *testing.T) {
	cases := map[BeatDecision]string{
		BeatPending:      "pending",
		BeatFire:         "fire",
		BeatSkip:         "skip",
		BeatDecision(99): "unknown",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Fatalf("BeatDecision(%d).String() = %q, want %q", int(d), got, want)
		}
	}
}

// The median of an even-length set must interpolate, not pick a side —
// otherwise a two-person drift consensus would systematically favour one
// reporter over the other.
func TestMedianDuration_EvenLengthInterpolates(t *testing.T) {
	got := medianDuration([]time.Duration{10 * time.Second, 20 * time.Second})
	if want := 15 * time.Second; got != want {
		t.Fatalf("median = %v, want %v", got, want)
	}
}

func TestMedianDuration_EmptyIsZero(t *testing.T) {
	if got := medianDuration(nil); got != 0 {
		t.Fatalf("median of empty = %v, want 0", got)
	}
}
