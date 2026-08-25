package identity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeriveAgeBandBoundaries(t *testing.T) {
	// FR-2's protections hang off the band, so an off-by-one here puts a
	// 15-year-old into public rooms. The birthday-not-yet-reached case is the
	// one a naive year subtraction gets wrong for most of the year.
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		dob  time.Time
		want AgeBand
	}{
		{"newborn", now, AgeUnder13},
		{"12 years and 364 days", time.Date(2013, 8, 26, 0, 0, 0, 0, time.UTC), AgeUnder13},
		{"13 exactly today", time.Date(2013, 8, 25, 0, 0, 0, 0, time.UTC), AgeTeen1315},
		{"15 years and 364 days", time.Date(2010, 8, 26, 0, 0, 0, 0, time.UTC), AgeTeen1315},
		{"16 exactly today", time.Date(2010, 8, 25, 0, 0, 0, 0, time.UTC), AgeTeen1617},
		{"17 years and 364 days", time.Date(2008, 8, 26, 0, 0, 0, 0, time.UTC), AgeTeen1617},
		{"18 exactly today", time.Date(2008, 8, 25, 0, 0, 0, 0, time.UTC), AgeAdult},
		{"comfortably adult", time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), AgeAdult},
		{"birthday later this year keeps the younger band", time.Date(2010, 12, 31, 0, 0, 0, 0, time.UTC), AgeTeen1315},
		{"birthday earlier this year has already advanced", time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC), AgeTeen1617},
		{"birthday tomorrow", time.Date(2010, 8, 26, 0, 0, 0, 0, time.UTC), AgeTeen1315},
		{"birthday yesterday", time.Date(2010, 8, 24, 0, 0, 0, 0, time.UTC), AgeTeen1617},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveAgeBand(tt.dob, now); got != tt.want {
				t.Errorf("DeriveAgeBand(%s) = %q, want %q", tt.dob.Format("2006-01-02"), got, tt.want)
			}
		})
	}
}

func TestDeriveAgeBandHandlesLeapDayBirthdays(t *testing.T) {
	// 29 February exists only every fourth year. Someone born on it turns 16
	// during a non-leap year, and the arithmetic must not push them a year late.
	dob := time.Date(2008, 2, 29, 0, 0, 0, 0, time.UTC)

	if got := DeriveAgeBand(dob, time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)); got != AgeTeen1315 {
		t.Errorf("day before the notional 16th birthday = %q, want teen_13_15", got)
	}
	if got := DeriveAgeBand(dob, time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)); got != AgeTeen1617 {
		t.Errorf("day after = %q, want teen_16_17", got)
	}
}

func TestDeriveAgeBandWithFutureDOB(t *testing.T) {
	// Not reachable through validated input, but a negative age must not wrap
	// into `adult`.
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	if got := DeriveAgeBand(now.AddDate(5, 0, 0), now); got != AgeUnder13 {
		t.Errorf("a future date of birth yielded %q, want the most protective band", got)
	}
}

func TestDeriveAgeBandIsTimezoneStable(t *testing.T) {
	// The same instant expressed in another zone must not change the band, or
	// a user's protections would depend on which server answered.
	dob := time.Date(2010, 8, 25, 0, 0, 0, 0, time.UTC)
	utcNow := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	east := time.FixedZone("UTC+9", 9*3600)
	if a, b := DeriveAgeBand(dob, utcNow), DeriveAgeBand(dob, utcNow.In(east)); a != b {
		t.Errorf("band differs by timezone: %q vs %q", a, b)
	}
}

func TestUnder16DefaultsAreRestrictive(t *testing.T) {
	// FR-2: under 16 defaults to no public rooms, no discoverability, no public
	// leaderboard entry (§12.4). The line is at 16, which spans TWO bands —
	// writing the check as `band == AgeTeen1315` would exempt under-13s.
	for _, band := range []AgeBand{AgeUnder13, AgeTeen1315} {
		t.Run(string(band), func(t *testing.T) {
			d := DefaultsForBand(band)
			if d.AllowPublicRooms {
				t.Error("public rooms must be off by default")
			}
			if d.Discoverable {
				t.Error("discoverability must be off by default")
			}
			if d.PublicLeaderboardEntry {
				t.Error("public leaderboard entry must be off by default")
			}
			if !IsMinorUnder16(band) {
				t.Error("IsMinorUnder16 disagrees with DefaultsForBand")
			}
		})
	}
}

func TestSixteenAndOverGetsNormalDefaults(t *testing.T) {
	for _, band := range []AgeBand{AgeTeen1617, AgeAdult} {
		t.Run(string(band), func(t *testing.T) {
			d := DefaultsForBand(band)
			if !d.AllowPublicRooms || !d.Discoverable || !d.PublicLeaderboardEntry {
				t.Errorf("%q should get normal defaults, got %+v", band, d)
			}
			if IsMinorUnder16(band) {
				t.Errorf("%q was classified as under 16", band)
			}
		})
	}
}

func TestAgeBandValuesMatchTheSchemaEnum(t *testing.T) {
	// migration 0001: CREATE TYPE age_band AS ENUM ('under_13','teen_13_15','teen_16_17','adult')
	want := []string{"under_13", "teen_13_15", "teen_16_17", "adult"}
	got := []string{string(AgeUnder13), string(AgeTeen1315), string(AgeTeen1617), string(AgeAdult)}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("band %d = %q, want %q — Go and SQL vocabularies must match", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Password policy
// ---------------------------------------------------------------------------

func testBreachSet(t *testing.T, entries ...string) *StaticBreachSet {
	t.Helper()
	s, err := LoadStaticBreachSet(strings.NewReader(strings.Join(entries, "\n")))
	if err != nil {
		t.Fatalf("LoadStaticBreachSet: %v", err)
	}
	return s
}

func TestPasswordPolicyRejectsBreachedPasswords(t *testing.T) {
	// FR-1's actual requirement.
	policy := PasswordPolicy{Breaches: testBreachSet(t, "correct horse battery staple", "Password1234")}

	if err := policy.Validate("correct horse battery staple"); !errors.Is(err, ErrPasswordBreached) {
		t.Errorf("err = %v, want ErrPasswordBreached", err)
	}
	if err := policy.Validate("Password1234"); !errors.Is(err, ErrPasswordBreached) {
		t.Errorf("err = %v, want ErrPasswordBreached", err)
	}
	if err := policy.Validate("a password not in the corpus"); err != nil {
		t.Errorf("unexpected error for a clean password: %v", err)
	}
}

func TestPasswordPolicyFailsClosedWithoutABreachSet(t *testing.T) {
	// The important one. A policy that silently passes when its data is missing
	// satisfies the letter of FR-1 while providing none of the protection, and
	// nothing in a log would reveal it.
	var policy PasswordPolicy
	if err := policy.Validate("a perfectly long password"); !errors.Is(err, ErrBreachSetUnavailable) {
		t.Errorf("err = %v, want ErrBreachSetUnavailable — the policy must fail closed", err)
	}
}

func TestPasswordPolicyFailsClosedWhenTheSetErrors(t *testing.T) {
	policy := PasswordPolicy{Breaches: erroringBreachSet{}}
	err := policy.Validate("a perfectly long password")
	if !errors.Is(err, ErrBreachSetUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrBreachSetUnavailable", err)
	}
	if !strings.Contains(err.Error(), "corpus unavailable") {
		t.Errorf("the underlying cause should be preserved for logs, got %v", err)
	}
}

type erroringBreachSet struct{}

func (erroringBreachSet) Contains(string) (bool, error) {
	return false, errors.New("corpus unavailable")
}

func TestPasswordLengthBounds(t *testing.T) {
	policy := PasswordPolicy{Breaches: testBreachSet(t, "irrelevant")}

	tests := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"empty", "", ErrPasswordTooShort},
		{"one under the minimum", strings.Repeat("a", MinPasswordLength-1), ErrPasswordTooShort},
		{"exactly the minimum", strings.Repeat("a", MinPasswordLength), nil},
		{"exactly the maximum", strings.Repeat("a", MaxPasswordLength), nil},
		{"one over the maximum", strings.Repeat("a", MaxPasswordLength+1), ErrPasswordTooLong},
		{"a megabyte, which would burn CPU in the KDF", strings.Repeat("a", 1<<20), ErrPasswordTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy.Validate(tt.password)
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPasswordLengthCountsRunesNotBytes(t *testing.T) {
	// A 12-character Arabic passphrase is 24 bytes in UTF-8. Counting bytes
	// would accept a 6-character one, which is exactly the wrong direction for
	// a project whose launch market types in Arabic.
	policy := PasswordPolicy{Breaches: testBreachSet(t, "irrelevant")}

	short := strings.Repeat("ك", MinPasswordLength-1) // 11 runes, 22 bytes
	if err := policy.Validate(short); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("an 11-rune Arabic password was accepted: %v", err)
	}
	ok := strings.Repeat("ك", MinPasswordLength)
	if err := policy.Validate(ok); err != nil {
		t.Errorf("a 12-rune Arabic password was rejected: %v", err)
	}
}

func TestLoadStaticBreachSet(t *testing.T) {
	s, err := LoadStaticBreachSet(strings.NewReader("# comment\n\npassword123\n\nhunter2\n# another\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Size() != 2 {
		t.Errorf("Size = %d, want 2 — comments and blanks must be skipped", s.Size())
	}
	for _, want := range []string{"password123", "hunter2"} {
		found, err := s.Contains(want)
		if err != nil || !found {
			t.Errorf("Contains(%q) = %v, %v; want true", want, found, err)
		}
	}
	found, err := s.Contains("# comment")
	if err != nil || found {
		t.Error("a comment line was loaded as a password")
	}
}

func TestLoadStaticBreachSetRejectsAnEmptyList(t *testing.T) {
	// An empty set would pass every password while looking configured.
	for _, input := range []string{"", "\n\n\n", "# only comments\n"} {
		if _, err := LoadStaticBreachSet(strings.NewReader(input)); err == nil {
			t.Errorf("an empty breach list (%q) loaded successfully; it must refuse", input)
		}
	}
}

func TestBreachComparisonIsCaseSensitive(t *testing.T) {
	// Breach corpora contain the literal strings people used. Normalising would
	// both miss real entries and reject passwords that were never breached.
	s := testBreachSet(t, "Password1234")
	if found, _ := s.Contains("password1234"); found {
		t.Error("comparison is case-insensitive; it must be exact")
	}
	if found, _ := s.Contains("Password1234"); !found {
		t.Error("exact match failed")
	}
}

func TestNilBreachSetContainsFailsRatherThanPassing(t *testing.T) {
	var s *StaticBreachSet
	if _, err := s.Contains("anything"); !errors.Is(err, ErrBreachSetUnavailable) {
		t.Errorf("err = %v, want ErrBreachSetUnavailable", err)
	}
	if s.Size() != 0 {
		t.Errorf("Size on nil = %d, want 0", s.Size())
	}
}

// failingReader errors partway through, standing in for an unreadable breach
// list on disk.
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n > 0 {
		f.n--
		p[0] = 'a'
		return 1, nil
	}
	return 0, errors.New("disk read error")
}

func TestLoadStaticBreachSetPropagatesAReadError(t *testing.T) {
	// Silently returning a partial set would be worse than failing: the policy
	// would look configured while missing most of its data.
	if _, err := LoadStaticBreachSet(&failingReader{n: 3}); err == nil {
		t.Fatal("a read error was swallowed; LoadStaticBreachSet must propagate it")
	}
}
