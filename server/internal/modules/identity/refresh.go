package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// RefreshTokenTTL is ADR-011's 60 days.
const RefreshTokenTTL = 60 * 24 * time.Hour

// OverlapWindow keeps a rotated token usable for a short period after it is
// replaced (ADR-011).
//
// Without it, the legitimate-retry case produces real logouts: a client whose
// response was lost retries with the token it still holds, that token has
// already been rotated, and reuse detection revokes the family. The client did
// nothing wrong and the user is signed out.
//
// Ten seconds covers an in-flight retry on a bad mobile connection without
// meaningfully widening the theft window — an attacker who has the token has
// 60 days of opportunity, so ten seconds changes nothing for them and
// everything for a commuter in a tunnel.
const OverlapWindow = 10 * time.Second

// refreshTokenBytes is 32 bytes of CSPRNG output (ADR-011): 256 bits, which is
// not guessable and not worth trying to shorten.
const refreshTokenBytes = 32

// ErrWeakRandomness means the CSPRNG failed. There is no safe fallback: a
// predictable refresh token is a total authentication bypass, so the operation
// fails rather than degrading.
var ErrWeakRandomness = errors.New("identity: secure random source unavailable")

// NewRefreshToken returns a fresh opaque token and the hash to store.
//
// The plaintext is returned once, to be handed to the client and then
// forgotten. Only the hash is persisted, so a database leak yields nothing
// usable — the same reason passwords are not stored either.
func NewRefreshToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrWeakRandomness, err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashRefreshToken(plaintext), nil
}

// HashRefreshToken is the storage transform for refresh tokens.
//
// Plain SHA-256, deliberately, where a password would use Argon2id. The
// difference is the input: a password is low-entropy and human-chosen, so it
// needs a slow hash to survive an offline guessing attack. This token is 256
// bits of CSPRNG output — brute-forcing it is not a matter of making each
// guess expensive. A slow KDF here would only add latency to every refresh.
func HashRefreshToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// RefreshTokenState is the stored row that a presented token maps to.
//
// It is the subset of `refresh_tokens` (migration 0002) that the decision
// below needs, so the state machine is testable without a database.
type RefreshTokenState struct {
	TokenHash         []byte
	FamilyID          string
	SessionID         string
	UserID            string
	ExpiresAt         time.Time
	RotatedAt         *time.Time // nil while current
	ValidUntilOverlap *time.Time // set when rotated
	FamilyRevokedAt   *time.Time
	FamilyRevokedWhy  string
}

// RefreshOutcome is the decision for a presented refresh token.
type RefreshOutcome int

const (
	// RefreshUnknown — no such token. Could be a typo, a token from a wiped
	// family, or an attacker probing. Indistinguishable, so: reject, no side
	// effects.
	RefreshUnknown RefreshOutcome = iota

	// RefreshRotate — the token is current and valid. Issue a new access token
	// and a new refresh token, and mark this one rotated.
	RefreshRotate

	// RefreshOverlapReplay — already rotated, but still inside the overlap
	// window. A legitimate retry. Re-issue against the SAME successor rather
	// than rotating again, so a retried request does not consume a second
	// generation.
	RefreshOverlapReplay

	// RefreshReuseDetected — FR-4. Already rotated and past the overlap. Revoke
	// the entire family, invalidate every session in it, alert.
	RefreshReuseDetected

	// RefreshExpired — past its 60 days.
	RefreshExpired

	// RefreshFamilyRevoked — the family is already dead, typically because
	// reuse was detected on a sibling token.
	RefreshFamilyRevoked
)

func (o RefreshOutcome) String() string {
	switch o {
	case RefreshRotate:
		return "rotate"
	case RefreshOverlapReplay:
		return "overlap_replay"
	case RefreshReuseDetected:
		return "reuse_detected"
	case RefreshExpired:
		return "expired"
	case RefreshFamilyRevoked:
		return "family_revoked"
	default:
		return "unknown"
	}
}

// EvaluateRefresh decides what to do with a presented refresh token (FR-4,
// ADR-011).
//
// This is the security-critical decision in the whole auth design, so it is a
// pure function of (state, now): no database, no clock, no I/O. Every branch
// is reachable in a table test, which is the only way anybody can be confident
// the reuse case actually fires.
//
// The ordering is deliberate. A revoked family wins over everything, because
// once reuse has been detected on any sibling the whole family is dead and no
// later presentation should be treated as ordinary. Expiry is checked before
// reuse so a token that merely aged out does not raise a theft alert and page
// somebody at 3am.
func EvaluateRefresh(state *RefreshTokenState, now time.Time) RefreshOutcome {
	if state == nil {
		return RefreshUnknown
	}
	if state.FamilyRevokedAt != nil && !now.Before(*state.FamilyRevokedAt) {
		return RefreshFamilyRevoked
	}
	if !state.ExpiresAt.IsZero() && now.After(state.ExpiresAt) {
		return RefreshExpired
	}
	if state.RotatedAt == nil {
		return RefreshRotate
	}
	// Rotated. Legitimate retry, or theft? Indistinguishable in principle, so
	// the overlap window is the only thing separating them.
	if state.ValidUntilOverlap != nil && !now.After(*state.ValidUntilOverlap) {
		return RefreshOverlapReplay
	}
	return RefreshReuseDetected
}

// ConstantTimeHashEqual compares two token hashes without leaking, via timing,
// how many leading bytes matched.
func ConstantTimeHashEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// RotationResult is what a successful rotation produces.
type RotationResult struct {
	Plaintext      string
	Hash           []byte
	ExpiresAt      time.Time
	PrevValidUntil time.Time
	FamilyID       string
	SessionID      string
}

// Rotate mints the successor token and computes both deadlines.
//
// The successor inherits the family, which is what makes reuse detectable at
// all: without a shared family id, a stolen token would simply look like
// another valid session.
func Rotate(prev *RefreshTokenState, now time.Time) (*RotationResult, error) {
	if prev == nil {
		return nil, errors.New("identity: cannot rotate a nil token state")
	}
	plaintext, hash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}
	return &RotationResult{
		Plaintext:      plaintext,
		Hash:           hash,
		ExpiresAt:      now.Add(RefreshTokenTTL),
		PrevValidUntil: now.Add(OverlapWindow),
		FamilyID:       prev.FamilyID,
		SessionID:      prev.SessionID,
	}, nil
}

// FamilyRevocationReason maps an outcome to the `revoked_reason` CHECK values
// in migration 0002, so the Go and SQL vocabularies cannot drift.
func FamilyRevocationReason(o RefreshOutcome) string {
	if o == RefreshReuseDetected {
		return "reuse_detected"
	}
	return ""
}
