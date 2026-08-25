// Package identity issues and verifies the two credentials VYBE runs on
// (ADR-011, FR-1–6): a short-lived Ed25519 JWT access token, and an opaque,
// hashed, rotating refresh token with reuse detection.
//
// The two are deliberately different in kind. The access token is stateless so
// no request pays a database round trip to be authorised — including every
// message on a socket. The refresh token is opaque and stored because a 60-day
// credential must be revocable, and rotating it makes theft *detectable*, which
// a static opaque token never is.
package identity

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AccessTokenTTL is ADR-011's 15 minutes.
//
// Short because an Ed25519 JWT cannot be revoked before it expires. Fifteen
// minutes is the accepted blast radius: for an immediate ban the moderation
// path also kills the socket and the session row, so what survives is API-only
// and brief.
const AccessTokenTTL = 15 * time.Minute

// algEdDSA is the ONLY algorithm this package will verify.
//
// Accepting whatever the header names is the classic JWT vulnerability: an
// attacker sets `alg` to "none" and drops the signature, or sets it to "HS256"
// so the verifier treats the Ed25519 *public* key as an HMAC secret — a key
// the attacker has, because public keys are public. Pinning the algorithm at
// the verifier removes both. See TestVerifyRejectsAlgorithmConfusion.
const algEdDSA = "EdDSA"

var (
	// ErrMalformedToken means the token is not three base64url segments.
	ErrMalformedToken = errors.New("identity: malformed token")
	// ErrBadAlgorithm means the header asked for anything other than EdDSA.
	ErrBadAlgorithm = errors.New("identity: unsupported token algorithm")
	// ErrBadSignature means the signature did not verify against the public key.
	ErrBadSignature = errors.New("identity: token signature is not valid")
	// ErrTokenExpired means exp is in the past.
	ErrTokenExpired = errors.New("identity: token has expired")
	// ErrTokenNotYetValid means iat is in the future beyond the allowed skew.
	ErrTokenNotYetValid = errors.New("identity: token is not yet valid")
	// ErrWrongIssuerAudience means iss or aud did not match.
	ErrWrongIssuerAudience = errors.New("identity: token issuer or audience mismatch")
)

// clockSkew is the tolerance applied to time-based claims.
//
// Client and server clocks disagree by seconds in practice, and this project
// is unusually clock-aware (ADR-002). Without tolerance, a user whose phone is
// 3 seconds fast gets a token the server thinks is from the future.
const clockSkew = 30 * time.Second

// Claims is the access token payload.
//
// ADR-011: minimal claims only — no email, no display name, no roles. A bearer
// credential ends up in a log or a crash report somewhere, so it must be
// boring. Anything the server can look up does not belong here.
type Claims struct {
	Subject     string `json:"sub"` // user id
	SessionID   string `json:"sid"` // FR-6: which session, so one can be revoked
	Entitlement string `json:"entitlement_tier"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
	JWTID       string `json:"jti"`
	Issuer      string `json:"iss,omitempty"`
	Audience    string `json:"aud,omitempty"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// TokenIssuer mints and verifies access tokens.
//
// It holds the private key; a verifier that only needs to check tokens can be
// built with NewVerifier and the public key alone. That split is the point of
// choosing an asymmetric algorithm: extracting the realtime tier later (§7.9)
// means shipping it a key that can verify and can never mint.
type TokenIssuer struct {
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	issuer   string
	audience string
	now      func() time.Time
}

// NewTokenIssuer builds an issuer from an Ed25519 private key.
func NewTokenIssuer(priv ed25519.PrivateKey, issuer, audience string) (*TokenIssuer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("identity: private key does not yield an ed25519 public key")
	}
	return &TokenIssuer{priv: priv, pub: pub, issuer: issuer, audience: audience, now: time.Now}, nil
}

// NewVerifier builds a verify-only issuer from a public key.
//
// Mint on such an instance fails rather than producing an unsigned token: a
// verifier that silently mints is worse than one that refuses.
func NewVerifier(pub ed25519.PublicKey, issuer, audience string) (*TokenIssuer, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return &TokenIssuer{pub: pub, issuer: issuer, audience: audience, now: time.Now}, nil
}

// SetClock injects a clock so expiry is testable without sleeping.
func (i *TokenIssuer) SetClock(now func() time.Time) { i.now = now }

// PublicKey returns the verification key, for distribution to a future
// realtime tier or a JWKS endpoint.
func (i *TokenIssuer) PublicKey() ed25519.PublicKey { return i.pub }

// Mint issues an access token for a session (FR-3).
func (i *TokenIssuer) Mint(userID, sessionID, entitlement, jti string) (string, error) {
	if i.priv == nil {
		return "", errors.New("identity: this issuer holds no private key and cannot mint")
	}
	now := i.now().UTC()
	claims := Claims{
		Subject:     userID,
		SessionID:   sessionID,
		Entitlement: entitlement,
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(AccessTokenTTL).Unix(),
		JWTID:       jti,
		Issuer:      i.issuer,
		Audience:    i.audience,
	}

	headerJSON, err := json.Marshal(header{Alg: algEdDSA, Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("identity: encoding header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("identity: encoding claims: %w", err)
	}

	signingInput := b64(headerJSON) + "." + b64(claimsJSON)
	sig := ed25519.Sign(i.priv, []byte(signingInput))
	return signingInput + "." + b64(sig), nil
}

// Verify checks a token and returns its claims.
//
// Order matters: signature first, then the time and identity claims. Reading
// claims from an unverified token — even to decide whether to bother verifying
// — is how a forged `exp` gets trusted.
func (i *TokenIssuer) Verify(token string) (*Claims, error) {
	h, p, sig, signingInput, err := split(token)
	if err != nil {
		return nil, err
	}

	var hdr header
	if err := json.Unmarshal(h, &hdr); err != nil {
		return nil, ErrMalformedToken
	}
	// Pinned, not consulted. See algEdDSA.
	if hdr.Alg != algEdDSA {
		return nil, fmt.Errorf("%w: %q", ErrBadAlgorithm, hdr.Alg)
	}

	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(i.pub, []byte(signingInput), sig) {
		return nil, ErrBadSignature
	}

	var claims Claims
	if err := json.Unmarshal(p, &claims); err != nil {
		return nil, ErrMalformedToken
	}

	now := i.now().UTC()
	if claims.ExpiresAt != 0 && now.After(time.Unix(claims.ExpiresAt, 0).Add(clockSkew)) {
		return nil, ErrTokenExpired
	}
	if claims.IssuedAt != 0 && now.Before(time.Unix(claims.IssuedAt, 0).Add(-clockSkew)) {
		return nil, ErrTokenNotYetValid
	}

	// Constant-time because these are attacker-influenced comparisons against
	// fixed strings. The timing signal is tiny, but so is the cost of removing it.
	if i.issuer != "" && subtle.ConstantTimeCompare([]byte(claims.Issuer), []byte(i.issuer)) != 1 {
		return nil, ErrWrongIssuerAudience
	}
	if i.audience != "" && subtle.ConstantTimeCompare([]byte(claims.Audience), []byte(i.audience)) != 1 {
		return nil, ErrWrongIssuerAudience
	}

	return &claims, nil
}

// split breaks a compact JWS into its decoded parts plus the signing input.
func split(token string) (hdr, payload, sig []byte, signingInput string, err error) {
	first := -1
	second := -1
	for idx := 0; idx < len(token); idx++ {
		if token[idx] != '.' {
			continue
		}
		if first < 0 {
			first = idx
			continue
		}
		if second < 0 {
			second = idx
			continue
		}
		// A fourth segment means JWE or a malformed token; either way this
		// package does not handle it.
		return nil, nil, nil, "", ErrMalformedToken
	}
	if first < 0 || second < 0 || first == 0 || second == first+1 || second == len(token)-1 {
		return nil, nil, nil, "", ErrMalformedToken
	}

	if hdr, err = unb64(token[:first]); err != nil {
		return nil, nil, nil, "", ErrMalformedToken
	}
	if payload, err = unb64(token[first+1 : second]); err != nil {
		return nil, nil, nil, "", ErrMalformedToken
	}
	if sig, err = unb64(token[second+1:]); err != nil {
		return nil, nil, nil, "", ErrMalformedToken
	}
	return hdr, payload, sig, token[:second], nil
}

// JWT uses base64url without padding (RFC 7515 §2).
func b64(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
