package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eyadsyam/vybe/server/internal/platform/ids"
	"github.com/eyadsyam/vybe/server/internal/platform/passwords"
)

// The identity application service (FR-1–FR-10).
//
// Everything above this file is a pure rule: EvaluateRefresh decides an
// outcome, PasswordPolicy decides validity, DeriveAgeBand decides a band. This
// is where those rules meet storage, and it is deliberately the only place
// that knows both — a handler talks to Service, and Service talks to
// Repository. That is what keeps the rules testable without a database and the
// SQL testable without HTTP.
//
// The service never returns a raw storage error to its caller. Errors that a
// client should see are the exported sentinels below; everything else is
// wrapped so the handler can map it to a 500 without having to guess.

// User is the account as the rest of the system sees it.
type User struct {
	ID              string
	Handle          string
	DisplayName     string
	AvatarURL       string
	Locale          string
	Region          string
	NumeralSystem   string
	AgeBand         AgeBand
	DateOfBirth     time.Time
	EntitlementTier string
	IsDiscoverable  bool
	CreatedAt       time.Time
}

// Credentials is the login secret, kept separate from User so that a query for
// profile data cannot accidentally select a password hash into a response.
// The split is a schema decision (migration 0002) worth preserving in Go.
type Credentials struct {
	UserID       string
	Email        string
	PasswordHash string
}

// Session is one device's login (FR-6).
type Session struct {
	ID         string
	UserID     string
	DeviceName string
	Platform   string
	CreatedAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

// Repository is the storage contract.
//
// Two methods carry more weight than the rest and are called out because an
// implementation that gets them wrong is not obviously broken:
//
//   - CreateUser must be atomic across users + user_credentials. A partial
//     write leaves an account that can never log in and whose handle is taken.
//   - MarkRotated must be a conditional update, not a read-then-write. The
//     reuse detection in EvaluateRefresh is only sound if exactly one
//     concurrent presentation of a token can win the rotation.
type Repository interface {
	CreateUser(ctx context.Context, u *User, email, passwordHash string) error
	UserByEmail(ctx context.Context, email string) (*User, *Credentials, error)
	UserByID(ctx context.Context, id string) (*User, error)
	HandleTaken(ctx context.Context, handle string) (bool, error)
	EmailTaken(ctx context.Context, email string) (bool, error)
	UpdatePasswordHash(ctx context.Context, userID, hash string) error

	CreateSession(ctx context.Context, s *Session) error
	SessionByID(ctx context.Context, id string) (*Session, error)
	RevokeSession(ctx context.Context, sessionID, reason string, at time.Time) error

	CreateFamily(ctx context.Context, familyID, userID, sessionID string, at time.Time) error
	RevokeFamily(ctx context.Context, familyID, reason string, at time.Time) error
	InsertRefreshToken(ctx context.Context, id, familyID string, hash []byte, issuedAt, expiresAt time.Time) error
	RefreshTokenByHash(ctx context.Context, hash []byte) (*RefreshTokenState, error)

	// MarkRotated stamps rotated_at and valid_until_overlap on the token
	// identified by hash, and reports whether it actually changed a row.
	//
	// The boolean is the concurrency guard: two simultaneous refreshes with
	// the same token must not both mint a new family member. The loser sees
	// false and falls back to the overlap replay path.
	MarkRotated(ctx context.Context, hash []byte, rotatedAt, validUntilOverlap time.Time) (rotated bool, err error)
}

// Errors a client is allowed to distinguish.
//
// ErrInvalidCredentials deliberately covers "no such email" AND "wrong
// password". Splitting them hands an attacker a user-enumeration oracle for
// free, and the login handler pairs this with a dummy hash verification so the
// two cases cost the same wall-clock time as well.
var (
	ErrInvalidCredentials = errors.New("identity: email or password is incorrect")
	ErrHandleTaken        = errors.New("identity: handle is already taken")
	ErrEmailTaken         = errors.New("identity: email is already registered")
	ErrUnderMinimumAge    = errors.New("identity: below the minimum age")
	ErrSessionRevoked     = errors.New("identity: session has been revoked")
	ErrInvalidHandle      = errors.New("identity: handle is not valid")
	ErrInvalidEmail       = errors.New("identity: email is not valid")
)

// MinimumAge is 13, matching §12.4 and the age_band enum in migration 0002.
//
// Below this we do not create a degraded account — we refuse. A "13+" product
// that quietly accepts a nine-year-old with restricted settings is still
// processing a nine-year-old's data.
const MinimumAge = 13

// Service composes the identity rules with storage.
type Service struct {
	repo     Repository
	tokens   *TokenIssuer
	policy   PasswordPolicy
	hashCost passwords.Params
	now      func() time.Time
}

// NewService returns a Service.
//
// hashCost is a parameter rather than a constant so tests can use
// passwords.TestParams; production passes passwords.DefaultParams. It is the
// kind of knob that gets quietly lowered to speed a test suite up, so the
// production value lives at the call site in main, where it is visible.
func NewService(repo Repository, tokens *TokenIssuer, policy PasswordPolicy, hashCost passwords.Params) *Service {
	return &Service{
		repo:     repo,
		tokens:   tokens,
		policy:   policy,
		hashCost: hashCost,
		now:      time.Now,
	}
}

// SetClock replaces the time source. Tests only.
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
	s.tokens.SetClock(now)
}

// RegisterInput is what FR-1 requires at signup.
type RegisterInput struct {
	Email       string
	Password    string
	Handle      string
	DisplayName string
	DateOfBirth time.Time
	Locale      string
	Region      string
	DeviceName  string
	Platform    string
}

// TokenPair is what a successful authentication returns.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	SessionID    string
	User         *User
}

// Register creates an account and logs it in (FR-1, FR-3, §12.4).
func (s *Service) Register(ctx context.Context, in RegisterInput) (*TokenPair, error) {
	now := s.now()

	email, err := NormaliseEmail(in.Email)
	if err != nil {
		return nil, err
	}
	handle, err := NormaliseHandle(in.Handle)
	if err != nil {
		return nil, err
	}
	if err := s.policy.Validate(in.Password); err != nil {
		return nil, err
	}

	band := DeriveAgeBand(in.DateOfBirth, now)
	if band == AgeUnder13 {
		return nil, ErrUnderMinimumAge
	}

	// Checked before the insert so the caller gets a precise 409 rather than a
	// unique-violation surfaced as a 500. The insert still carries the unique
	// constraint, because this check races: two simultaneous signups for the
	// same handle both pass here and only the database can settle it. The
	// repository maps that violation back onto these same sentinels.
	if taken, err := s.repo.EmailTaken(ctx, email); err != nil {
		return nil, fmt.Errorf("checking email: %w", err)
	} else if taken {
		return nil, ErrEmailTaken
	}
	if taken, err := s.repo.HandleTaken(ctx, handle); err != nil {
		return nil, fmt.Errorf("checking handle: %w", err)
	} else if taken {
		return nil, ErrHandleTaken
	}

	hash, err := passwords.Hash(in.Password, s.hashCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	defaults := DefaultsForBand(band)
	user := &User{
		ID:              ids.New().String(),
		Handle:          handle,
		DisplayName:     strings.TrimSpace(in.DisplayName),
		Locale:          orDefault(in.Locale, "en"),
		Region:          orDefault(in.Region, "EG"),
		NumeralSystem:   "western", // §3.6: a preference, not a locale consequence
		AgeBand:         band,
		DateOfBirth:     in.DateOfBirth,
		EntitlementTier: "free",
		// §12.4 again: a minor's account starts closed and can only be opened
		// deliberately. Defaulting to discoverable and letting them opt out is
		// the same policy with the failure mode reversed.
		IsDiscoverable: defaults.Discoverable,
		CreatedAt:      now,
	}
	if user.DisplayName == "" {
		user.DisplayName = handle
	}

	if err := s.repo.CreateUser(ctx, user, email, hash); err != nil {
		return nil, err // already mapped to a sentinel where it matters
	}

	return s.startSession(ctx, user, in.DeviceName, in.Platform, now)
}

// LoginInput is a credential presentation.
type LoginInput struct {
	Email      string
	Password   string
	DeviceName string
	Platform   string
}

// Login authenticates and opens a session (FR-2).
func (s *Service) Login(ctx context.Context, in LoginInput) (*TokenPair, error) {
	now := s.now()

	email, err := NormaliseEmail(in.Email)
	if err != nil {
		// Even a malformed email must not short-circuit: "that is not a valid
		// address" and "no such account" are different answers, and the
		// difference is enumerable. Fall through to the same generic failure,
		// paying the same dummy-hash cost.
		verifyDummy(in.Password)
		return nil, ErrInvalidCredentials
	}

	user, creds, err := s.repo.UserByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrInvalidCredentials) {
		return nil, fmt.Errorf("loading user: %w", err)
	}

	if user == nil || creds == nil {
		// No account. Burn the same Argon2id work anyway — otherwise the
		// response time answers "does this email have an account?" precisely.
		verifyDummy(in.Password)
		return nil, ErrInvalidCredentials
	}

	if err := passwords.Verify(in.Password, creds.PasswordHash); err != nil {
		// Includes a corrupt stored hash. The user cannot act on that
		// distinction and an attacker can, so it collapses here.
		return nil, ErrInvalidCredentials
	}

	// The only moment the plaintext exists to rehash with. A failure here is
	// not worth failing the login over — the user authenticated correctly, and
	// the worst case is that the upgrade happens at the next login instead.
	if passwords.NeedsRehash(creds.PasswordHash, s.hashCost) {
		if upgraded, err := passwords.Hash(in.Password, s.hashCost); err == nil {
			_ = s.repo.UpdatePasswordHash(ctx, user.ID, upgraded)
		}
	}

	return s.startSession(ctx, user, in.DeviceName, in.Platform, now)
}

// verifyDummy pays the cost of a real verification against a throwaway hash.
//
// The result is discarded on purpose; the point is the elapsed time, not the
// answer. Keep the call — a compiler or a well-meaning refactor removing it
// reopens the enumeration channel silently.
func verifyDummy(password string) {
	_ = passwords.Verify(password, passwords.DummyHash)
}

// startSession mints a session, a refresh family, and the first token pair.
func (s *Service) startSession(ctx context.Context, user *User, deviceName, platform string, now time.Time) (*TokenPair, error) {
	session := &Session{
		ID:         ids.New().String(),
		UserID:     user.ID,
		DeviceName: orDefault(strings.TrimSpace(deviceName), "Unknown device"),
		Platform:   normalisePlatform(platform),
		CreatedAt:  now,
		LastSeenAt: now,
	}
	if err := s.repo.CreateSession(ctx, session); err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}

	familyID := ids.New().String()
	if err := s.repo.CreateFamily(ctx, familyID, user.ID, session.ID, now); err != nil {
		return nil, fmt.Errorf("creating refresh family: %w", err)
	}

	plaintext, hash, err := NewRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("minting refresh token: %w", err)
	}
	expires := now.Add(RefreshTokenTTL)
	if err := s.repo.InsertRefreshToken(ctx, ids.New().String(), familyID, hash, now, expires); err != nil {
		return nil, fmt.Errorf("storing refresh token: %w", err)
	}

	access, err := s.tokens.Mint(user.ID, session.ID, user.EntitlementTier, ids.New().String())
	if err != nil {
		return nil, fmt.Errorf("minting access token: %w", err)
	}

	return &TokenPair{
		AccessToken:  access,
		RefreshToken: plaintext,
		ExpiresAt:    now.Add(AccessTokenTTL),
		SessionID:    session.ID,
		User:         user,
	}, nil
}

// ErrRefreshRejected is returned for every unsuccessful refresh.
//
// One error for expired, unknown, revoked, and reuse-detected. The client's
// only correct response to any of them is identical — discard the tokens and
// send the user to login — and distinguishing them tells a token thief which
// of their guesses was a real token that had already been rotated.
var ErrRefreshRejected = errors.New("identity: refresh token is not usable")

// Refresh exchanges a refresh token for a new pair (FR-4, ADR-011).
func (s *Service) Refresh(ctx context.Context, plaintext string) (*TokenPair, error) {
	now := s.now()

	state, err := s.repo.RefreshTokenByHash(ctx, HashRefreshToken(plaintext))
	if err != nil && !errors.Is(err, ErrRefreshRejected) {
		return nil, fmt.Errorf("loading refresh token: %w", err)
	}
	if state == nil {
		return nil, ErrRefreshRejected
	}

	switch outcome := EvaluateRefresh(state, now); outcome {
	case RefreshReuseDetected:
		// A token that was already rotated has been presented again outside
		// the overlap window. Either it was stolen and the thief is using it,
		// or it was stolen and the legitimate client is. We cannot tell which,
		// so the entire family dies and both parties re-authenticate. Losing a
		// session is recoverable; leaving a thief with a renewing credential
		// is not.
		if err := s.repo.RevokeFamily(ctx, state.FamilyID, FamilyRevocationReason(outcome), now); err != nil {
			return nil, fmt.Errorf("revoking compromised family: %w", err)
		}
		if err := s.repo.RevokeSession(ctx, state.SessionID, "reuse_detected", now); err != nil {
			return nil, fmt.Errorf("revoking session after reuse: %w", err)
		}
		return nil, ErrRefreshRejected

	case RefreshOverlapReplay:
		// ADR-011's 10s window. The client retried an in-flight refresh — a
		// dropped response, not an attack — so it is served rather than
		// punished. Without this, every flaky network logs the user out.
		return s.issueForExistingFamily(ctx, state, now)

	case RefreshRotate:
		rotated, err := s.repo.MarkRotated(ctx, state.TokenHash, now, now.Add(OverlapWindow))
		if err != nil {
			return nil, fmt.Errorf("rotating refresh token: %w", err)
		}
		if !rotated {
			// Another request rotated it between our read and our write. That
			// request is now the legitimate holder; this one replays under the
			// overlap rule rather than triggering reuse detection, because it
			// is the same honest client racing itself.
			return s.issueForExistingFamily(ctx, state, now)
		}

		next, err := Rotate(state, now)
		if err != nil {
			return nil, fmt.Errorf("deriving successor token: %w", err)
		}
		if err := s.repo.InsertRefreshToken(ctx, ids.New().String(), state.FamilyID, next.Hash, now, next.ExpiresAt); err != nil {
			return nil, fmt.Errorf("storing successor token: %w", err)
		}
		pair, err := s.issueForExistingFamily(ctx, state, now)
		if err != nil {
			return nil, err
		}
		pair.RefreshToken = next.Plaintext
		return pair, nil

	default: // RefreshExpired, RefreshFamilyRevoked, RefreshUnknown
		return nil, ErrRefreshRejected
	}
}

// issueForExistingFamily mints an access token for an already-valid session.
func (s *Service) issueForExistingFamily(ctx context.Context, state *RefreshTokenState, now time.Time) (*TokenPair, error) {
	session, err := s.repo.SessionByID(ctx, state.SessionID)
	if err != nil {
		return nil, fmt.Errorf("loading session: %w", err)
	}
	if session == nil || session.RevokedAt != nil {
		// The session was revoked (a logout elsewhere, an admin action) but
		// the refresh token had not yet expired. Revocation must win.
		return nil, ErrRefreshRejected
	}

	user, err := s.repo.UserByID(ctx, state.UserID)
	if err != nil {
		return nil, fmt.Errorf("loading user: %w", err)
	}
	if user == nil {
		return nil, ErrRefreshRejected
	}

	access, err := s.tokens.Mint(user.ID, state.SessionID, user.EntitlementTier, ids.New().String())
	if err != nil {
		return nil, fmt.Errorf("minting access token: %w", err)
	}

	return &TokenPair{
		AccessToken: access,
		// Deliberately empty on the replay path: the caller already holds a
		// usable refresh token, and handing out a second one would fork the
		// family. Rotate overwrites this.
		RefreshToken: "",
		ExpiresAt:    now.Add(AccessTokenTTL),
		SessionID:    state.SessionID,
		User:         user,
	}, nil
}

// Logout revokes one session and its refresh family (FR-5).
//
// Idempotent: logging out an already-revoked session succeeds. A client
// retrying a logout after a timeout must not receive an error for having
// succeeded the first time.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	now := s.now()
	if err := s.repo.RevokeSession(ctx, sessionID, "logout", now); err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

// Authenticate verifies an access token and confirms its session is still live
// (FR-6).
//
// The session check is the part that is easy to skip and expensive to omit. A
// signed JWT stays cryptographically valid for its full 15 minutes, so without
// this a "log out all devices" leaves a stolen token working for a quarter of
// an hour after the user believed they had killed it.
func (s *Service) Authenticate(ctx context.Context, token string) (*Claims, error) {
	claims, err := s.tokens.Verify(token)
	if err != nil {
		return nil, err
	}

	session, err := s.repo.SessionByID(ctx, claims.SessionID)
	if err != nil {
		return nil, fmt.Errorf("loading session: %w", err)
	}
	if session == nil || session.RevokedAt != nil {
		return nil, ErrSessionRevoked
	}
	return claims, nil
}

// ---------------------------------------------------------------------------
// Normalisation
// ---------------------------------------------------------------------------

// NormaliseEmail lowercases and trims an address.
//
// Not full RFC 5322 validation, on purpose. That grammar accepts addresses no
// mail provider will deliver to and rejects nothing an attacker cares about;
// the only validation that means anything is sending a confirmation mail. What
// this does guarantee is that "A@B.com" and "a@b.com" cannot become two
// accounts — which the citext column also enforces, but a mismatch between the
// two layers is how duplicate-account bugs happen.
func NormaliseEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" {
		return "", ErrInvalidEmail
	}
	at := strings.IndexByte(e, '@')
	if at <= 0 || at == len(e)-1 {
		return "", ErrInvalidEmail
	}
	if strings.Contains(e[at+1:], "@") {
		return "", ErrInvalidEmail
	}
	if !strings.Contains(e[at+1:], ".") {
		return "", ErrInvalidEmail
	}
	if len(e) > 254 { // RFC 5321 §4.5.3.1.3
		return "", ErrInvalidEmail
	}
	if strings.ContainsAny(e, " \t\r\n") {
		return "", ErrInvalidEmail
	}
	return e, nil
}

const (
	// MinHandleLength and MaxHandleLength bound the public identifier.
	MinHandleLength = 3
	MaxHandleLength = 30
)

// NormaliseHandle lowercases and validates a public handle.
//
// ASCII-only, deliberately, even though the product launches in Arabic. A
// handle is an identifier that appears in URLs and is read aloud; allowing
// mixed scripts opens homograph impersonation (Cyrillic а for Latin a is the
// classic), and the display name — which is unrestricted and fully
// Unicode — is the field that actually carries a user's name.
func NormaliseHandle(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if len(h) < MinHandleLength || len(h) > MaxHandleLength {
		return "", ErrInvalidHandle
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '_' || c == '.':
			// Not at either end, and never doubled: "a..b" and "a._b" read as
			// one separator to a human and are a distinct handle to a machine.
			if i == 0 || i == len(h)-1 {
				return "", ErrInvalidHandle
			}
			if prev := h[i-1]; prev == '_' || prev == '.' {
				return "", ErrInvalidHandle
			}
		default:
			return "", ErrInvalidHandle
		}
	}
	return h, nil
}

func normalisePlatform(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "android":
		return "android"
	case "ios":
		return "ios"
	case "web":
		return "web"
	default:
		// The column has a CHECK constraint, so an unrecognised value would be
		// a 500 at insert time. "unknown" is a real enum member for exactly
		// this reason.
		return "unknown"
	}
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
