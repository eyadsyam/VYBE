package identity

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// AgeBand mirrors the `age_band` enum in migration 0001. The Go and SQL
// vocabularies are kept identical so a value can round-trip without mapping.
type AgeBand string

const (
	AgeUnder13  AgeBand = "under_13"
	AgeTeen1315 AgeBand = "teen_13_15"
	AgeTeen1617 AgeBand = "teen_16_17"
	AgeAdult    AgeBand = "adult"
)

// DeriveAgeBand computes the band from a date of birth (FR-2).
//
// Only the band is meant to be used for policy decisions; the exact date is
// collected because the band changes over time and cannot be recomputed
// without it. §12.4's protections hang off the band, so getting the boundary
// arithmetic right is not cosmetic — an off-by-one here puts a 15-year-old in
// public rooms.
//
// The boundary is "has the birthday occurred yet this year", which is the
// distinction a naive year subtraction gets wrong for the majority of the year.
func DeriveAgeBand(dob, now time.Time) AgeBand {
	switch years := completedYears(dob, now); {
	case years < 13:
		return AgeUnder13
	case years < 16:
		return AgeTeen1315
	case years < 18:
		return AgeTeen1617
	default:
		return AgeAdult
	}
}

// completedYears counts whole years elapsed, in UTC.
func completedYears(dob, now time.Time) int {
	dob, now = dob.UTC(), now.UTC()
	years := now.Year() - dob.Year()
	// Subtract one if this year's birthday has not arrived yet.
	if now.Month() < dob.Month() || (now.Month() == dob.Month() && now.Day() < dob.Day()) {
		years--
	}
	if years < 0 {
		return 0
	}
	return years
}

// MinorDefaults are the §12.4 protections FR-2 requires for under-16s.
//
// Expressed as a struct rather than three scattered booleans so a new surface
// cannot forget one: adding a field here makes every construction site fail to
// compile until it is considered.
type MinorDefaults struct {
	AllowPublicRooms       bool
	Discoverable           bool
	PublicLeaderboardEntry bool
}

// DefaultsForBand returns the account defaults for an age band (FR-2, §12.4).
//
// These are *defaults at registration*, and they are restrictive for under-16s:
// no public rooms, no discoverability, no public leaderboard entry. Note that
// FR-2 draws its line at 16, which spans two bands — under_13 and teen_13_15 —
// so the check is on the derived age, not on a single band value. Writing it
// as `band == AgeTeen1315` would silently exempt under-13s, which is the
// opposite of the intent.
func DefaultsForBand(band AgeBand) MinorDefaults {
	if band == AgeUnder13 || band == AgeTeen1315 {
		return MinorDefaults{
			AllowPublicRooms:       false,
			Discoverable:           false,
			PublicLeaderboardEntry: false,
		}
	}
	return MinorDefaults{
		AllowPublicRooms:       true,
		Discoverable:           true,
		PublicLeaderboardEntry: true,
	}
}

// IsMinorUnder16 is the FR-2 predicate, stated once.
func IsMinorUnder16(band AgeBand) bool {
	return band == AgeUnder13 || band == AgeTeen1315
}

// ---------------------------------------------------------------------------
// Password policy (FR-1)
// ---------------------------------------------------------------------------

var (
	// ErrPasswordTooShort is the length floor.
	ErrPasswordTooShort = errors.New("identity: password is too short")
	// ErrPasswordTooLong guards the KDF against a denial-of-service via a
	// megabyte password: Argon2id happily hashes one and burns CPU doing it.
	ErrPasswordTooLong = errors.New("identity: password is too long")
	// ErrPasswordBreached is FR-1's requirement.
	ErrPasswordBreached = errors.New("identity: password appears in a known-breached set")
	// ErrBreachSetUnavailable means no breach set is configured.
	ErrBreachSetUnavailable = errors.New("identity: breached-password set is not configured")
)

const (
	// MinPasswordLength is 12.
	//
	// Length beats composition rules. Mandatory symbol-and-digit requirements
	// push people toward "Password1!", which is in every breach corpus, while
	// a longer minimum plus a breach check rejects exactly the passwords that
	// are actually known to attackers. FR-1 asks for the breach check, which is
	// the half that carries the weight.
	MinPasswordLength = 12

	// MaxPasswordLength bounds the KDF input.
	MaxPasswordLength = 256
)

// BreachSet reports whether a password is known to be breached (FR-1).
//
// This is an interface, and there is deliberately no default implementation
// that returns "not breached". A policy that silently passes when its data is
// missing satisfies the letter of FR-1 while providing none of the protection,
// and nothing in a test or a log would reveal it.
type BreachSet interface {
	// Contains reports whether the password is present in the set. An error
	// means the answer is unknown, which callers must treat as fatal — see
	// PasswordPolicy.Validate.
	Contains(password string) (bool, error)
}

// PasswordPolicy validates a candidate password (FR-1).
type PasswordPolicy struct {
	Breaches BreachSet
}

// Validate applies the policy.
//
// If the breach set is missing or errors, registration FAILS rather than
// proceeding. That is the deliberate choice: FR-1 says the system must reject
// breached passwords, and a system that cannot tell has not met that
// requirement. Failing closed turns a data-loading bug into a loud outage
// instead of a silent, permanent weakening of every account created meanwhile.
func (p PasswordPolicy) Validate(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	if p.Breaches == nil {
		return ErrBreachSetUnavailable
	}
	breached, err := p.Breaches.Contains(password)
	if err != nil {
		return errors.Join(ErrBreachSetUnavailable, err)
	}
	if breached {
		return ErrPasswordBreached
	}
	return nil
}

// StaticBreachSet is an in-memory breached-password set loaded from a list.
//
// It is the M1 implementation. A network-backed range query against a service
// such as HIBP is the obvious upgrade, and is deliberately NOT written yet:
// §0.3 rule 1 forbids coding against a third-party API whose behaviour has not
// been probed and recorded in docs/INTEGRATIONS.md. Guessing its response shape
// here would produce code that compiles, reviews cleanly, and fails on first
// contact — the exact failure the TMDB probe was written to avoid.
type StaticBreachSet struct {
	set map[string]struct{}
}

// LoadStaticBreachSet reads one password per line.
//
// Comparison is case-sensitive and exact. Breach corpora contain the literal
// strings people actually used, so normalising them would both miss real
// entries and reject passwords that were never breached.
func LoadStaticBreachSet(r io.Reader) (*StaticBreachSet, error) {
	s := &StaticBreachSet{set: map[string]struct{}{}}
	scanner := bufio.NewScanner(r)
	// Breach lists contain long lines; the default 64KB token limit is ample
	// but the buffer is set explicitly so a surprise is a clear error.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s.set[line] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(s.set) == 0 {
		// An empty set would pass every password while looking configured.
		return nil, errors.New("identity: breach list contained no entries")
	}
	return s, nil
}

// Contains implements BreachSet.
func (s *StaticBreachSet) Contains(password string) (bool, error) {
	if s == nil || s.set == nil {
		return false, ErrBreachSetUnavailable
	}
	_, found := s.set[password]
	return found, nil
}

// Size reports how many entries were loaded, for a startup log line. An
// operator seeing "breach set loaded: 12 entries" knows something is wrong;
// seeing nothing at all, they do not.
func (s *StaticBreachSet) Size() int {
	if s == nil {
		return 0
	}
	return len(s.set)
}
