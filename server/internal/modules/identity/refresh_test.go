package identity

import (
	"testing"
	"time"
)

var refNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func TestEvaluateRefreshCoversEveryOutcome(t *testing.T) {
	// FR-4 is the security-critical decision in the auth design, so every
	// branch is exercised explicitly. A reuse case that never fires in a test
	// is a reuse case nobody should believe in.
	tests := []struct {
		name  string
		state *RefreshTokenState
		now   time.Time
		want  RefreshOutcome
	}{
		{
			name:  "unknown token",
			state: nil,
			now:   refNow,
			want:  RefreshUnknown,
		},
		{
			name:  "current and valid rotates",
			state: &RefreshTokenState{ExpiresAt: refNow.Add(RefreshTokenTTL)},
			now:   refNow,
			want:  RefreshRotate,
		},
		{
			name: "rotated, inside the overlap window, is a legitimate retry",
			state: &RefreshTokenState{
				ExpiresAt:         refNow.Add(RefreshTokenTTL),
				RotatedAt:         ptr(refNow.Add(-3 * time.Second)),
				ValidUntilOverlap: ptr(refNow.Add(7 * time.Second)),
			},
			now:  refNow,
			want: RefreshOverlapReplay,
		},
		{
			name: "rotated, exactly at the overlap boundary, still a retry",
			state: &RefreshTokenState{
				ExpiresAt:         refNow.Add(RefreshTokenTTL),
				RotatedAt:         ptr(refNow.Add(-OverlapWindow)),
				ValidUntilOverlap: ptr(refNow),
			},
			now:  refNow,
			want: RefreshOverlapReplay,
		},
		{
			name: "rotated, one instant past the overlap, is theft",
			state: &RefreshTokenState{
				ExpiresAt:         refNow.Add(RefreshTokenTTL),
				RotatedAt:         ptr(refNow.Add(-time.Minute)),
				ValidUntilOverlap: ptr(refNow.Add(-time.Nanosecond)),
			},
			now:  refNow,
			want: RefreshReuseDetected,
		},
		{
			name: "rotated with no overlap recorded is theft",
			state: &RefreshTokenState{
				ExpiresAt: refNow.Add(RefreshTokenTTL),
				RotatedAt: ptr(refNow.Add(-time.Hour)),
			},
			now:  refNow,
			want: RefreshReuseDetected,
		},
		{
			name:  "expired",
			state: &RefreshTokenState{ExpiresAt: refNow.Add(-time.Second)},
			now:   refNow,
			want:  RefreshExpired,
		},
		{
			name: "expiry is checked before reuse, so ageing out does not raise a theft alert",
			state: &RefreshTokenState{
				ExpiresAt:         refNow.Add(-time.Hour),
				RotatedAt:         ptr(refNow.Add(-2 * time.Hour)),
				ValidUntilOverlap: ptr(refNow.Add(-2 * time.Hour)),
			},
			now:  refNow,
			want: RefreshExpired,
		},
		{
			name: "a revoked family beats everything else",
			state: &RefreshTokenState{
				ExpiresAt:        refNow.Add(RefreshTokenTTL),
				FamilyRevokedAt:  ptr(refNow.Add(-time.Minute)),
				FamilyRevokedWhy: "reuse_detected",
			},
			now:  refNow,
			want: RefreshFamilyRevoked,
		},
		{
			name: "a revocation timestamped in the future has not taken effect yet",
			state: &RefreshTokenState{
				ExpiresAt:       refNow.Add(RefreshTokenTTL),
				FamilyRevokedAt: ptr(refNow.Add(time.Minute)),
			},
			now:  refNow,
			want: RefreshRotate,
		},
		{
			name:  "a zero ExpiresAt is not treated as already expired",
			state: &RefreshTokenState{},
			now:   refNow,
			want:  RefreshRotate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateRefresh(tt.state, tt.now); got != tt.want {
				t.Errorf("EvaluateRefresh = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReuseDetectionRevokesTheWholeFamily(t *testing.T) {
	// FR-4 names the reason string, and migration 0002 CHECKs it. If the two
	// drift, the write fails at runtime in the middle of a security response.
	if got := FamilyRevocationReason(RefreshReuseDetected); got != "reuse_detected" {
		t.Errorf("reason = %q, want %q (migration 0002 CHECK)", got, "reuse_detected")
	}
	for _, o := range []RefreshOutcome{RefreshRotate, RefreshExpired, RefreshUnknown, RefreshOverlapReplay} {
		if got := FamilyRevocationReason(o); got != "" {
			t.Errorf("%v should not revoke a family, got reason %q", o, got)
		}
	}
}

func TestRefreshOutcomeStringsAreStable(t *testing.T) {
	// These land in logs and alerts; renaming one silently breaks a dashboard.
	want := map[RefreshOutcome]string{
		RefreshUnknown:       "unknown",
		RefreshRotate:        "rotate",
		RefreshOverlapReplay: "overlap_replay",
		RefreshReuseDetected: "reuse_detected",
		RefreshExpired:       "expired",
		RefreshFamilyRevoked: "family_revoked",
	}
	for o, s := range want {
		if got := o.String(); got != s {
			t.Errorf("%d.String() = %q, want %q", int(o), got, s)
		}
	}
}

func TestTTLsMatchADR011(t *testing.T) {
	if RefreshTokenTTL != 60*24*time.Hour {
		t.Errorf("RefreshTokenTTL = %v, want 60 days (FR-3)", RefreshTokenTTL)
	}
	if OverlapWindow != 10*time.Second {
		t.Errorf("OverlapWindow = %v, want 10s (ADR-011)", OverlapWindow)
	}
	if OverlapWindow >= RefreshTokenTTL {
		t.Error("the overlap window must be a small fraction of the token lifetime")
	}
}

func TestNewRefreshTokenIsHighEntropyAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := range 500 {
		plaintext, hash, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("NewRefreshToken: %v", err)
		}
		if seen[plaintext] {
			t.Fatalf("duplicate refresh token generated at iteration %d", i)
		}
		seen[plaintext] = true

		// 32 raw bytes base64url-encodes to 43 characters without padding.
		if len(plaintext) != 43 {
			t.Fatalf("token length = %d, want 43 (32 bytes base64url)", len(plaintext))
		}
		if len(hash) != 32 {
			t.Fatalf("hash length = %d, want 32 (SHA-256)", len(hash))
		}
	}
}

func TestHashRefreshTokenIsDeterministicAndNotTheIdentity(t *testing.T) {
	plaintext, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken: %v", err)
	}
	if !ConstantTimeHashEqual(HashRefreshToken(plaintext), hash) {
		t.Error("HashRefreshToken is not deterministic for the same input")
	}
	if string(hash) == plaintext {
		t.Error("the stored value equals the plaintext; a database leak would yield usable tokens")
	}
	if ConstantTimeHashEqual(HashRefreshToken("a"), HashRefreshToken("b")) {
		t.Error("different inputs hashed equal")
	}
}

func TestConstantTimeHashEqual(t *testing.T) {
	a := HashRefreshToken("x")
	if !ConstantTimeHashEqual(a, a) {
		t.Error("a hash did not equal itself")
	}
	if ConstantTimeHashEqual(a, a[:16]) {
		t.Error("different-length hashes compared equal")
	}
	if ConstantTimeHashEqual(nil, a) {
		t.Error("nil compared equal to a hash")
	}
}

func TestRotateInheritsTheFamilyAndSetsBothDeadlines(t *testing.T) {
	prev := &RefreshTokenState{
		FamilyID:  "fam-1",
		SessionID: "sess-1",
		UserID:    "user-1",
		ExpiresAt: refNow.Add(RefreshTokenTTL),
	}

	got, err := Rotate(prev, refNow)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// Without a shared family id a stolen token just looks like another valid
	// session, and reuse can never be detected.
	if got.FamilyID != "fam-1" {
		t.Errorf("FamilyID = %q, want the inherited fam-1", got.FamilyID)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}
	if !got.ExpiresAt.Equal(refNow.Add(RefreshTokenTTL)) {
		t.Errorf("ExpiresAt = %v, want now+60d", got.ExpiresAt)
	}
	if !got.PrevValidUntil.Equal(refNow.Add(OverlapWindow)) {
		t.Errorf("PrevValidUntil = %v, want now+10s", got.PrevValidUntil)
	}
	if got.Plaintext == "" || len(got.Hash) != 32 {
		t.Errorf("rotation produced no usable token: %+v", got)
	}
}

func TestRotateRefusesNil(t *testing.T) {
	if _, err := Rotate(nil, refNow); err == nil {
		t.Error("Rotate(nil) succeeded; want an error")
	}
}

// The full theft sequence, end to end, because the individual branches passing
// does not prove they compose into the behaviour FR-4 describes.
func TestStolenTokenSequenceRevokesTheFamily(t *testing.T) {
	// 1. Victim holds a current token.
	victim := &RefreshTokenState{
		FamilyID:  "fam-1",
		SessionID: "sess-1",
		ExpiresAt: refNow.Add(RefreshTokenTTL),
	}
	if got := EvaluateRefresh(victim, refNow); got != RefreshRotate {
		t.Fatalf("step 1: got %v, want rotate", got)
	}

	// 2. Victim refreshes. The old token is now rotated with an overlap.
	rotation, err := Rotate(victim, refNow)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rotated := *victim
	rotated.RotatedAt = ptr(refNow)
	rotated.ValidUntilOverlap = ptr(rotation.PrevValidUntil)

	// 3. An attacker who captured the OLD token uses it a minute later —
	//    well past the overlap.
	attackTime := refNow.Add(time.Minute)
	outcome := EvaluateRefresh(&rotated, attackTime)
	if outcome != RefreshReuseDetected {
		t.Fatalf("step 3: got %v, want reuse_detected", outcome)
	}

	// 4. The family is revoked with the reason the schema permits.
	reason := FamilyRevocationReason(outcome)
	if reason != "reuse_detected" {
		t.Fatalf("step 4: reason = %q", reason)
	}

	// 5. Every sibling token in the family is now dead — including the
	//    successor the victim legitimately holds. That is the accepted cost:
	//    the victim re-authenticates once, instead of the attacker holding a
	//    60-day credential.
	successor := &RefreshTokenState{
		FamilyID:         "fam-1",
		ExpiresAt:        rotation.ExpiresAt,
		FamilyRevokedAt:  ptr(attackTime),
		FamilyRevokedWhy: reason,
	}
	if got := EvaluateRefresh(successor, attackTime.Add(time.Second)); got != RefreshFamilyRevoked {
		t.Errorf("step 5: successor got %v, want family_revoked", got)
	}
}
