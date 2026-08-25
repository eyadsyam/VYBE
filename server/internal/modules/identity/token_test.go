package identity

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testIssuer(t *testing.T) (*TokenIssuer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	iss, err := NewTokenIssuer(priv, "https://vybe.app", "vybe-api")
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	return iss, priv
}

func TestMintVerifyRoundTrip(t *testing.T) {
	iss, _ := testIssuer(t)

	token, err := iss.Mint("user-1", "session-1", "plus", "jti-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	claims, err := iss.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "user-1" || claims.SessionID != "session-1" {
		t.Errorf("claims = %+v", claims)
	}
	if claims.Entitlement != "plus" || claims.JWTID != "jti-1" {
		t.Errorf("claims = %+v", claims)
	}
	if got := claims.ExpiresAt - claims.IssuedAt; got != int64(AccessTokenTTL.Seconds()) {
		t.Errorf("TTL = %ds, want %ds (FR-3 requires 15 minutes)", got, int64(AccessTokenTTL.Seconds()))
	}
}

func TestAccessTokenTTLIsFifteenMinutes(t *testing.T) {
	// FR-3 states the number. Asserting the constant means changing it is a
	// deliberate act with a failing test, not a quiet edit.
	if AccessTokenTTL != 15*time.Minute {
		t.Errorf("AccessTokenTTL = %v, want 15m (FR-3, ADR-011)", AccessTokenTTL)
	}
}

func TestClaimsCarryNoPersonalData(t *testing.T) {
	// ADR-011: minimal claims only. A bearer credential ends up in a log or a
	// crash report, so it must be boring. This test fails the moment somebody
	// adds `email` "just for convenience".
	iss, _ := testIssuer(t)
	token, err := iss.Mint("user-1", "session-1", "free", "jti-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	allowed := map[string]bool{
		"sub": true, "sid": true, "entitlement_tier": true,
		"iat": true, "exp": true, "jti": true, "iss": true, "aud": true,
	}
	for k := range raw {
		if !allowed[k] {
			t.Errorf("claim %q is not in ADR-011's minimal set", k)
		}
	}
	for _, forbidden := range []string{"email", "display_name", "handle", "roles", "date_of_birth"} {
		if _, present := raw[forbidden]; present {
			t.Errorf("claim %q must never appear in an access token", forbidden)
		}
	}
}

// This is the test the hand-rolled implementation exists for.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	iss, _ := testIssuer(t)
	pub := iss.PublicKey()

	mkToken := func(hdr header, claims Claims, sig []byte) string {
		h, _ := json.Marshal(hdr)
		c, _ := json.Marshal(claims)
		return b64(h) + "." + b64(c) + "." + b64(sig)
	}
	valid := Claims{
		Subject: "attacker", SessionID: "s", Entitlement: "plus",
		IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Issuer: "https://vybe.app", Audience: "vybe-api",
	}

	// The canonical alg=none token has an EMPTY third segment, so it is
	// refused by the structural check before the algorithm is even read. Both
	// paths matter, so both are asserted: here the property is simply that it
	// never verifies, whatever the reason.
	t.Run("alg none with an empty signature segment", func(t *testing.T) {
		for _, alg := range []string{"none", "NONE", "None"} {
			token := mkToken(header{Alg: alg, Typ: "JWT"}, valid, nil)
			claims, err := iss.Verify(token)
			if err == nil {
				t.Errorf("alg=%q verified and returned %+v; it must never be honoured", alg, claims)
			}
		}
	})

	// With a non-empty (junk) signature the token is structurally well formed,
	// so verification reaches the algorithm check. This is the case that proves
	// the alg is pinned rather than merely unreachable.
	t.Run("alg none with a junk signature", func(t *testing.T) {
		for _, alg := range []string{"none", "NONE", "None"} {
			token := mkToken(header{Alg: alg, Typ: "JWT"}, valid, make([]byte, ed25519.SignatureSize))
			if _, err := iss.Verify(token); !errors.Is(err, ErrBadAlgorithm) {
				t.Errorf("alg=%q: err = %v, want ErrBadAlgorithm", alg, err)
			}
		}
	})

	t.Run("HS256 using the public key as the HMAC secret", func(t *testing.T) {
		// The classic confusion attack: the public key is public, so if the
		// verifier honours `alg: HS256` the attacker can forge freely.
		hdr := header{Alg: "HS256", Typ: "JWT"}
		h, _ := json.Marshal(hdr)
		c, _ := json.Marshal(valid)
		signingInput := b64(h) + "." + b64(c)

		mac := hmac.New(sha256.New, pub)
		mac.Write([]byte(signingInput))
		token := signingInput + "." + b64(mac.Sum(nil))

		if _, err := iss.Verify(token); !errors.Is(err, ErrBadAlgorithm) {
			t.Errorf("err = %v, want ErrBadAlgorithm — HS256 confusion must be refused", err)
		}
	})

	t.Run("empty alg", func(t *testing.T) {
		token := mkToken(header{Typ: "JWT"}, valid, make([]byte, ed25519.SignatureSize))
		if _, err := iss.Verify(token); !errors.Is(err, ErrBadAlgorithm) {
			t.Errorf("err = %v, want ErrBadAlgorithm", err)
		}
	})
}

func TestVerifyRejectsTamperedClaims(t *testing.T) {
	iss, _ := testIssuer(t)
	token, err := iss.Mint("user-1", "session-1", "free", "jti-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(token, ".")

	// Escalate `free` to `plus` and re-encode, leaving the signature alone.
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	tampered := strings.Replace(string(payload), `"entitlement_tier":"free"`, `"entitlement_tier":"plus"`, 1)
	if tampered == string(payload) {
		t.Fatal("test setup failed to modify the payload")
	}
	forged := parts[0] + "." + b64([]byte(tampered)) + "." + parts[2]

	if _, err := iss.Verify(forged); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature — an edited payload must not verify", err)
	}
}

func TestVerifyRejectsAnotherKeysToken(t *testing.T) {
	issA, _ := testIssuer(t)
	issB, _ := testIssuer(t)

	token, err := issA.Mint("user-1", "s", "free", "j")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := issB.Verify(token); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyTimeClaims(t *testing.T) {
	iss, _ := testIssuer(t)
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	iss.SetClock(func() time.Time { return base })

	token, err := iss.Mint("user-1", "s", "free", "j")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	tests := []struct {
		name    string
		at      time.Time
		wantErr error
	}{
		{"immediately", base, nil},
		{"one second before expiry", base.Add(AccessTokenTTL - time.Second), nil},
		{"just past expiry but inside skew", base.Add(AccessTokenTTL + 10*time.Second), nil},
		{"well past expiry", base.Add(AccessTokenTTL + 2*time.Minute), ErrTokenExpired},
		{"before issuance but inside skew", base.Add(-10 * time.Second), nil},
		{"long before issuance", base.Add(-2 * time.Minute), ErrTokenNotYetValid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := NewVerifier(iss.PublicKey(), "https://vybe.app", "vybe-api")
			v.SetClock(func() time.Time { return tt.at })

			_, err := v.Verify(token)
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

func TestClockSkewToleranceExists(t *testing.T) {
	// This project is unusually clock-aware (ADR-002). A phone three seconds
	// fast must not be locked out of its own freshly minted token.
	if clockSkew < 5*time.Second {
		t.Errorf("clockSkew = %v; too tight for real device clocks", clockSkew)
	}
	if clockSkew > 5*time.Minute {
		t.Errorf("clockSkew = %v; so loose that expiry stops meaning much", clockSkew)
	}
}

func TestVerifyRejectsIssuerAndAudienceMismatch(t *testing.T) {
	iss, priv := testIssuer(t)
	token, err := iss.Mint("user-1", "s", "free", "j")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	t.Run("wrong issuer", func(t *testing.T) {
		v, _ := NewVerifier(pub, "https://evil.example", "vybe-api")
		if _, err := v.Verify(token); !errors.Is(err, ErrWrongIssuerAudience) {
			t.Errorf("err = %v, want ErrWrongIssuerAudience", err)
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		v, _ := NewVerifier(pub, "https://vybe.app", "some-other-service")
		if _, err := v.Verify(token); !errors.Is(err, ErrWrongIssuerAudience) {
			t.Errorf("err = %v, want ErrWrongIssuerAudience", err)
		}
	})
	t.Run("unset issuer and audience skip the check", func(t *testing.T) {
		v, _ := NewVerifier(pub, "", "")
		if _, err := v.Verify(token); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	iss, _ := testIssuer(t)
	tests := []struct{ name, token string }{
		{"empty", ""},
		{"one segment", "abc"},
		{"two segments", "abc.def"},
		{"four segments", "a.b.c.d"},
		{"leading dot", ".b.c"},
		{"trailing dot", "a.b."},
		{"empty middle", "a..c"},
		{"header not base64", "!!!.eyJ9.c2ln"},
		{"header not json", b64([]byte("not json")) + ".e30." + b64(make([]byte, 64))},
		{"only dots", "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := iss.Verify(tt.token); err == nil {
				t.Errorf("Verify(%q) succeeded; want an error", tt.token)
			}
		})
	}
}

func TestPayloadNotJSONIsRejectedAfterSignatureCheck(t *testing.T) {
	// A validly signed but non-JSON payload must fail cleanly rather than
	// panicking or yielding zero-valued claims that look like a real user.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	iss, _ := NewTokenIssuer(priv, "", "")

	h, _ := json.Marshal(header{Alg: algEdDSA, Typ: "JWT"})
	signingInput := b64(h) + "." + b64([]byte("this is not json"))
	sig := ed25519.Sign(priv, []byte(signingInput))

	if _, err := iss.Verify(signingInput + "." + b64(sig)); !errors.Is(err, ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken", err)
	}
}

func TestWrongSignatureLengthIsRejected(t *testing.T) {
	iss, priv := testIssuer(t)
	h, _ := json.Marshal(header{Alg: algEdDSA, Typ: "JWT"})
	c, _ := json.Marshal(Claims{Subject: "u", Issuer: "https://vybe.app", Audience: "vybe-api"})
	signingInput := b64(h) + "." + b64(c)
	full := ed25519.Sign(priv, []byte(signingInput))

	// Truncated: ed25519.Verify would panic on a short signature in some
	// implementations, so the length is checked first.
	if _, err := iss.Verify(signingInput + "." + b64(full[:32])); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifierCannotMint(t *testing.T) {
	// The point of choosing an asymmetric algorithm: a future realtime tier
	// gets a key that verifies and can never mint (§7.9).
	iss, _ := testIssuer(t)
	v, err := NewVerifier(iss.PublicKey(), "https://vybe.app", "vybe-api")
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	if _, err := v.Mint("u", "s", "free", "j"); err == nil {
		t.Fatal("a verify-only issuer minted a token; it must refuse")
	}
}

func TestConstructorsRejectWrongSizedKeys(t *testing.T) {
	if _, err := NewTokenIssuer(ed25519.PrivateKey("too short"), "", ""); err == nil {
		t.Error("NewTokenIssuer accepted an undersized private key")
	}
	if _, err := NewVerifier(ed25519.PublicKey("too short"), "", ""); err == nil {
		t.Error("NewVerifier accepted an undersized public key")
	}
}

func TestMintedTokensAreDistinct(t *testing.T) {
	iss, _ := testIssuer(t)
	a, _ := iss.Mint("u", "s", "free", "jti-a")
	b, _ := iss.Mint("u", "s", "free", "jti-b")
	if a == b {
		t.Error("two tokens with different jti encoded identically")
	}
}
