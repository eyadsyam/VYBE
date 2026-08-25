package ids

import (
	"bytes"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// ADR-010 states the generator is "unit-tested for monotonicity within the
// same millisecond". This is that test. It is not a formality: room_events
// ordering and cursor pagination both assume ID order equals creation order.
func TestNew_MonotonicWithinSameMillisecond(t *testing.T) {
	frozen := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	g := &generator{
		now:      func() time.Time { return frozen }, // clock never advances
		randRead: fixedRand(0xAA),
	}

	const n = 3000 // comfortably inside the 4096-per-ms counter range
	got := make([]UUID, n)
	for i := range got {
		u, err := g.next()
		if err != nil {
			t.Fatalf("next() error at %d: %v", i, err)
		}
		got[i] = u
	}

	for i := 1; i < n; i++ {
		if bytes.Compare(got[i-1][:], got[i][:]) >= 0 {
			t.Fatalf("IDs not strictly increasing at index %d:\n  %s\n  %s",
				i, got[i-1], got[i])
		}
	}
}

// The counter is 12 bits. Exceeding it must not wrap and produce a smaller ID;
// it borrows from the next millisecond instead.
func TestNew_CounterOverflowBorrowsNextMillisecond(t *testing.T) {
	frozen := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	g := &generator{
		now:      func() time.Time { return frozen },
		randRead: fixedRand(0x00), // counter starts at 0
	}

	const n = 5000 // > 4096, so it must roll into the next millisecond
	prev, err := g.next()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		cur, err := g.next()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Compare(prev[:], cur[:]) >= 0 {
			t.Fatalf("ordering broke across counter rollover at %d:\n  %s\n  %s",
				i, prev, cur)
		}
		prev = cur
	}

	// The borrowed milliseconds must be a small, bounded overshoot, not a jump.
	drift := prev.Timestamp().Sub(frozen)
	if drift < 0 || drift > 5*time.Millisecond {
		t.Fatalf("timestamp drifted %v after rollover; want 0..5ms", drift)
	}
}

// A backwards clock step (NTP correction, VM migration, a laptop resuming) must
// never produce an ID that sorts before one already issued.
func TestNew_BackwardClockStepDoesNotBreakOrdering(t *testing.T) {
	current := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	g := &generator{
		now:      func() time.Time { return current },
		randRead: fixedRand(0x11),
	}

	first, err := g.next()
	if err != nil {
		t.Fatal(err)
	}

	// Clock jumps 10 seconds backwards.
	current = current.Add(-10 * time.Second)

	second, err := g.next()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Compare(first[:], second[:]) >= 0 {
		t.Fatalf("backwards clock produced a non-increasing ID:\n  %s\n  %s", first, second)
	}
}

func TestNew_VersionAndVariantAreRFC9562(t *testing.T) {
	for i := 0; i < 200; i++ {
		u := New()
		if got := u.Version(); got != 7 {
			t.Fatalf("version = %d, want 7 (%s)", got, u)
		}
		if variant := u[8] & 0xC0; variant != 0x80 {
			t.Fatalf("variant bits = %#x, want 0x80 (%s)", variant, u)
		}
	}
}

func TestTimestamp_RoundTripsWithinAMillisecond(t *testing.T) {
	before := time.Now().UTC()
	u := New()
	after := time.Now().UTC()

	ts := u.Timestamp()
	// Truncate the bounds, since the encoded value has millisecond resolution.
	if ts.Before(before.Truncate(time.Millisecond)) || ts.After(after) {
		t.Fatalf("Timestamp() = %v, want within [%v, %v]", ts, before, after)
	}
}

// Sorting a batch of IDs must reproduce creation order — this is what lets
// cursor pagination use the ID as an opaque, stable sort key (§5.2).
func TestNew_SortOrderMatchesCreationOrder(t *testing.T) {
	const n = 500
	created := make([]UUID, n)
	for i := range created {
		created[i] = New()
		if i%50 == 0 {
			time.Sleep(time.Millisecond) // span several milliseconds
		}
	}

	shuffled := make([]UUID, n)
	copy(shuffled, created)
	// Reverse, so a no-op sort cannot pass by accident.
	for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	sort.Slice(shuffled, func(i, j int) bool {
		return bytes.Compare(shuffled[i][:], shuffled[j][:]) < 0
	})

	for i := range created {
		if created[i] != shuffled[i] {
			t.Fatalf("sorted order differs from creation order at %d:\n  created  %s\n  sorted   %s",
				i, created[i], shuffled[i])
		}
	}
}

func TestNew_ConcurrentCallersStayUnique(t *testing.T) {
	const goroutines, perGoroutine = 16, 500

	var wg sync.WaitGroup
	out := make(chan UUID, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				out <- New()
			}
		}()
	}
	wg.Wait()
	close(out)

	seen := make(map[UUID]struct{}, goroutines*perGoroutine)
	for u := range out {
		if _, dup := seen[u]; dup {
			t.Fatalf("duplicate ID under concurrency: %s", u)
		}
		seen[u] = struct{}{}
	}
	if len(seen) != goroutines*perGoroutine {
		t.Fatalf("got %d unique IDs, want %d", len(seen), goroutines*perGoroutine)
	}
}

func TestParse_RoundTrip(t *testing.T) {
	orig := New()
	parsed, err := Parse(orig.String())
	if err != nil {
		t.Fatalf("Parse(%q): %v", orig, err)
	}
	if parsed != orig {
		t.Fatalf("round trip changed the value: %s -> %s", orig, parsed)
	}
}

// Being liberal about input spellings would let two renderings of one ID reach
// the database and defeat a unique constraint. Each of these must be rejected.
func TestParse_RejectsNonCanonicalForms(t *testing.T) {
	cases := map[string]string{
		"no hyphens":     "0192f1e4c8d97a3b8f2e1d0c9b8a7654",
		"braces":         "{0192f1e4-c8d9-7a3b-8f2e-1d0c9b8a7654}",
		"urn prefix":     "urn:uuid:0192f1e4-c8d9-7a3b-8f2e-1d0c9b8a7654",
		"too short":      "0192f1e4-c8d9-7a3b-8f2e-1d0c9b8a765",
		"too long":       "0192f1e4-c8d9-7a3b-8f2e-1d0c9b8a76543",
		"non-hex":        "0192f1e4-c8d9-7a3b-8f2e-1d0c9b8a765z",
		"empty":          "",
		"hyphen misplaced": "0192f1e4c-8d9-7a3b-8f2e-1d0c9b8a7654",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Fatalf("Parse(%q) accepted a non-canonical form", in)
			}
		})
	}
}

func TestScan_AcceptsBinaryAndText(t *testing.T) {
	orig := New()

	t.Run("binary 16 bytes", func(t *testing.T) {
		var u UUID
		if err := u.Scan(orig[:]); err != nil {
			t.Fatal(err)
		}
		if u != orig {
			t.Fatalf("got %s, want %s", u, orig)
		}
	})

	t.Run("text", func(t *testing.T) {
		var u UUID
		if err := u.Scan(orig.String()); err != nil {
			t.Fatal(err)
		}
		if u != orig {
			t.Fatalf("got %s, want %s", u, orig)
		}
	})

	t.Run("nil yields Nil", func(t *testing.T) {
		u := orig
		if err := u.Scan(nil); err != nil {
			t.Fatal(err)
		}
		if !u.IsNil() {
			t.Fatalf("got %s, want Nil", u)
		}
	})

	t.Run("wrong type errors", func(t *testing.T) {
		var u UUID
		if err := u.Scan(42); err == nil {
			t.Fatal("Scan(int) should fail")
		}
	})
}

func TestNext_PropagatesRandFailure(t *testing.T) {
	sentinel := errors.New("entropy pool drained")
	g := &generator{
		now:      time.Now,
		randRead: func([]byte) (int, error) { return 0, sentinel },
	}
	if _, err := g.next(); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// fixedRand fills the buffer with a repeating byte so tests are deterministic.
func fixedRand(b byte) func([]byte) (int, error) {
	return func(p []byte) (int, error) {
		for i := range p {
			p[i] = b
		}
		return len(p), nil
	}
}

func BenchmarkNew(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = New()
	}
}
