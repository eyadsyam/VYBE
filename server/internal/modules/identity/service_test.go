package identity_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
	"github.com/eyadsyam/vybe/server/internal/modules/identity/identitytest"
	"github.com/eyadsyam/vybe/server/internal/platform/passwords"
)

// A fixed instant, so age-band arithmetic is not a function of when the suite
// runs. A test that passes in August and fails on a leap day is worse than no
// test.
var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T) (*identity.Service, *identitytest.Store, func(time.Time)) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	issuer, err := identity.NewTokenIssuer(priv, "vybe", "vybe-app")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	store := identitytest.New()
	breaches, err := identity.LoadStaticBreachSet(strings.NewReader(
		"password123456\ncorrect horse battery staple\n"))
	if err != nil {
		t.Fatalf("LoadStaticBreachSet: %v", err)
	}
	policy := identity.PasswordPolicy{Breaches: breaches}
	svc := identity.NewService(store, issuer, policy, passwords.TestParams)

	now := testNow
	svc.SetClock(func() time.Time { return now })
	return svc, store, func(t time.Time) { now = t }
}

func validRegistration() identity.RegisterInput {
	return identity.RegisterInput{
		Email:       "Sara@Example.COM",
		Password:    "a sufficiently long passphrase",
		Handle:      "sara_q",
		DisplayName: "سارة",
		DateOfBirth: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		Locale:      "ar",
		Region:      "EG",
		DeviceName:  "Pixel 8",
		Platform:    "android",
	}
}

func TestRegisterCreatesAUsableAccount(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("Register returned an empty token")
	}
	if pair.User.Handle != "sara_q" {
		t.Errorf("handle = %q, want the lowercased form", pair.User.Handle)
	}
	if pair.User.AgeBand != identity.AgeAdult {
		t.Errorf("age band = %q, want adult for a 2000 birth date", pair.User.AgeBand)
	}
	if pair.User.EntitlementTier != "free" {
		t.Errorf("entitlement = %q, want free", pair.User.EntitlementTier)
	}
	if pair.ExpiresAt != testNow.Add(identity.AccessTokenTTL) {
		t.Errorf("expiry = %v, want now + %v", pair.ExpiresAt, identity.AccessTokenTTL)
	}

	// The credentials must be findable by the NORMALISED email, not the mixed
	// case that was submitted, or the user can never log in again.
	if taken, _ := store.EmailTaken(ctx, "sara@example.com"); !taken {
		t.Error("the email was not stored in its normalised form")
	}

	// And the account must authenticate immediately.
	claims, err := svc.Authenticate(ctx, pair.AccessToken)
	if err != nil {
		t.Fatalf("Authenticate with a fresh token: %v", err)
	}
	if claims.Subject != pair.User.ID {
		t.Errorf("sub = %q, want %q", claims.Subject, pair.User.ID)
	}
}

func TestRegisterStoresNoPlaintextPassword(t *testing.T) {
	svc, store, _ := newService(t)
	in := validRegistration()

	if _, err := svc.Register(context.Background(), in); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, creds, err := store.UserByEmail(context.Background(), "sara@example.com")
	if err != nil || creds == nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	if strings.Contains(creds.PasswordHash, in.Password) {
		t.Fatal("the stored hash contains the plaintext password")
	}
	if !strings.HasPrefix(creds.PasswordHash, "$argon2id$") {
		t.Errorf("stored credential is %q, want an argon2id hash", creds.PasswordHash)
	}
}

func TestRegisterRefusesUnderThirteen(t *testing.T) {
	// §12.4. A twelve-year-old does not get a restricted account; they get no
	// account. A degraded account still processes a child's data.
	svc, _, _ := newService(t)
	in := validRegistration()
	in.DateOfBirth = testNow.AddDate(-12, 0, 0)

	if _, err := svc.Register(context.Background(), in); !errors.Is(err, identity.ErrUnderMinimumAge) {
		t.Fatalf("Register for a 12-year-old = %v, want ErrUnderMinimumAge", err)
	}
}

func TestRegisterAppliesMinorPrivacyDefaults(t *testing.T) {
	// A 14-year-old is old enough for an account and too young to be
	// discoverable by default (§12.4).
	svc, _, _ := newService(t)
	in := validRegistration()
	in.DateOfBirth = testNow.AddDate(-14, 0, 0)

	pair, err := svc.Register(context.Background(), in)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if pair.User.AgeBand != identity.AgeTeen1315 {
		t.Fatalf("age band = %q, want teen_13_15", pair.User.AgeBand)
	}
	if pair.User.IsDiscoverable {
		t.Error("a 14-year-old was discoverable by default; §12.4 requires the opposite")
	}
}

func TestRegisterRejectsDuplicateEmailAndHandle(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, validRegistration()); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Same email, different handle — and in a different case, to prove the
	// duplicate check runs on the normalised value.
	dup := validRegistration()
	dup.Handle = "different"
	dup.Email = "SARA@EXAMPLE.COM"
	if _, err := svc.Register(ctx, dup); !errors.Is(err, identity.ErrEmailTaken) {
		t.Errorf("duplicate email = %v, want ErrEmailTaken", err)
	}

	// Same handle, different email.
	dup2 := validRegistration()
	dup2.Email = "other@example.com"
	if _, err := svc.Register(ctx, dup2); !errors.Is(err, identity.ErrHandleTaken) {
		t.Errorf("duplicate handle = %v, want ErrHandleTaken", err)
	}
}

func TestRegisterEnforcesThePasswordPolicy(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	short := validRegistration()
	short.Password = "short"
	if _, err := svc.Register(ctx, short); !errors.Is(err, identity.ErrPasswordTooShort) {
		t.Errorf("short password = %v, want ErrPasswordTooShort", err)
	}

	breached := validRegistration()
	breached.Email = "b@example.com"
	breached.Handle = "breached"
	breached.Password = "correct horse battery staple" // seeded into the breach set
	if _, err := svc.Register(ctx, breached); !errors.Is(err, identity.ErrPasswordBreached) {
		t.Errorf("breached password = %v, want ErrPasswordBreached", err)
	}
}

func TestLoginSucceedsAndIsCaseInsensitiveOnEmail(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	in := validRegistration()
	if _, err := svc.Register(ctx, in); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, email := range []string{"sara@example.com", "Sara@Example.com", "  SARA@EXAMPLE.COM  "} {
		pair, err := svc.Login(ctx, identity.LoginInput{
			Email: email, Password: in.Password, Platform: "ios",
		})
		if err != nil {
			t.Fatalf("Login(%q): %v", email, err)
		}
		if pair.AccessToken == "" {
			t.Errorf("Login(%q) returned no access token", email)
		}
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	// The single most important property in this file. An attacker must not be
	// able to learn which emails have accounts, and the error value is the
	// most obvious channel for that leak.
	svc, _, _ := newService(t)
	ctx := context.Background()
	in := validRegistration()
	if _, err := svc.Register(ctx, in); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cases := []struct {
		name  string
		email string
		pw    string
	}{
		{"no such account", "nobody@example.com", in.Password},
		{"right account, wrong password", "sara@example.com", "the wrong passphrase entirely"},
		{"right account, empty password", "sara@example.com", ""},
		{"malformed email", "not-an-email", in.Password},
		{"empty email", "", in.Password},
		{"both empty", "", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Login(ctx, identity.LoginInput{Email: tt.email, Password: tt.pw})
			if !errors.Is(err, identity.ErrInvalidCredentials) {
				t.Fatalf("Login = %v, want ErrInvalidCredentials for every failure mode", err)
			}
		})
	}
}

func TestLoginOnAnUnknownEmailStillPaysTheHashingCost(t *testing.T) {
	// The timing side of the same leak. A miss that returns instantly answers
	// "does this email exist?" precisely, regardless of what the error says.
	//
	// Asserted as a ratio against a known-good login rather than an absolute
	// duration, because absolute timings on a shared CI runner are flaky. The
	// bound is deliberately loose: this catches "returns in nanoseconds
	// because it never hashed", not a subtle few-percent difference.
	svc, _, _ := newService(t)
	ctx := context.Background()
	in := validRegistration()
	if _, err := svc.Register(ctx, in); err != nil {
		t.Fatalf("Register: %v", err)
	}

	measure := func(email, pw string) time.Duration {
		best := time.Hour
		for range 5 {
			start := time.Now()
			_, _ = svc.Login(ctx, identity.LoginInput{Email: email, Password: pw})
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	hit := measure("sara@example.com", "the wrong passphrase")
	miss := measure("nobody@example.com", "the wrong passphrase")

	if miss*10 < hit {
		t.Errorf("an unknown email returned in %v against %v for a known one; "+
			"the dummy-hash verification is not running and the user table is enumerable by timing",
			miss, hit)
	}
}

func TestLoginUpgradesAWeakStoredHash(t *testing.T) {
	// The rehash-on-login path. Without it, a cost increase never reaches the
	// accounts that already exist.
	svc, store, _ := newService(t)
	ctx := context.Background()
	in := validRegistration()
	if _, err := svc.Register(ctx, in); err != nil {
		t.Fatalf("Register: %v", err)
	}

	weak := passwords.Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 16}
	weakHash, err := passwords.Hash(in.Password, weak)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	_, creds, _ := store.UserByEmail(ctx, "sara@example.com")
	if err := store.UpdatePasswordHash(ctx, creds.UserID, weakHash); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}

	if _, err := svc.Login(ctx, identity.LoginInput{Email: "sara@example.com", Password: in.Password}); err != nil {
		t.Fatalf("Login with the weak hash: %v", err)
	}

	_, after, _ := store.UserByEmail(ctx, "sara@example.com")
	if after.PasswordHash == weakHash {
		t.Error("the weak hash survived a successful login; the rehash path did not run")
	}
	if passwords.NeedsRehash(after.PasswordHash, passwords.TestParams) {
		t.Error("the rehashed credential is still below policy")
	}
}

func TestRefreshRotatesAndInvalidatesThePreviousToken(t *testing.T) {
	svc, _, setNow := newService(t)
	ctx := context.Background()
	first, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Past the overlap window, so this is a clean rotation rather than a replay.
	setNow(testNow.Add(time.Hour))

	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.RefreshToken == "" {
		t.Fatal("a rotation returned no new refresh token")
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("Refresh returned the same token; it did not rotate")
	}
	if second.AccessToken == first.AccessToken {
		t.Error("Refresh returned the same access token")
	}

	// The new one works.
	setNow(testNow.Add(2 * time.Hour))
	if _, err := svc.Refresh(ctx, second.RefreshToken); err != nil {
		t.Fatalf("the rotated token was rejected: %v", err)
	}
}

func TestRefreshWithinTheOverlapWindowReplaysInsteadOfRevoking(t *testing.T) {
	// ADR-011's 10 seconds. The client sent a refresh, the response was lost,
	// and it retried. That is a flaky network, not a stolen token — and
	// treating it as theft logs a user out every time their train enters a
	// tunnel at the wrong moment.
	svc, store, setNow := newService(t)
	ctx := context.Background()
	first, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	setNow(testNow.Add(time.Hour))
	if _, err := svc.Refresh(ctx, first.RefreshToken); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// Same token again, 5 seconds later — inside the window.
	setNow(testNow.Add(time.Hour).Add(5 * time.Second))
	replay, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("a replay inside the overlap window was rejected: %v", err)
	}
	if replay.AccessToken == "" {
		t.Error("the replay returned no access token")
	}
	if replay.RefreshToken != "" {
		t.Error("the replay minted a second refresh token; that forks the family")
	}
	if reason := store.FamilyRevokedReason(store.OnlyFamilyID()); reason != "" {
		t.Errorf("the family was revoked (%q) for an in-window retry", reason)
	}
}

func TestRefreshAfterTheOverlapWindowRevokesTheWholeFamily(t *testing.T) {
	// The other side of the same rule. A rotated token presented late is the
	// signature of theft: either the thief has it or the legitimate client
	// does, and we cannot tell which, so both re-authenticate.
	svc, store, setNow := newService(t)
	ctx := context.Background()
	first, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	setNow(testNow.Add(time.Hour))
	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// 11 seconds > the 10-second window.
	setNow(testNow.Add(time.Hour).Add(identity.OverlapWindow + time.Second))
	if _, err := svc.Refresh(ctx, first.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Fatalf("reuse = %v, want ErrRefreshRejected", err)
	}

	if reason := store.FamilyRevokedReason(store.OnlyFamilyID()); reason != "reuse_detected" {
		t.Errorf("family revocation reason = %q, want reuse_detected", reason)
	}
	if !store.SessionRevoked(first.SessionID) {
		t.Error("the session survived reuse detection")
	}

	// And crucially: the token the THIEF might hold — the legitimate successor
	// — must also be dead. Revoking only the presented token would leave
	// whoever stole the successor with a working credential.
	if _, err := svc.Refresh(ctx, second.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("the successor token still worked after family revocation: %v", err)
	}
}

func TestRefreshRejectsUnknownAndExpiredTokens(t *testing.T) {
	svc, _, setNow := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.Refresh(ctx, "a token that was never issued"); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("unknown token = %v, want ErrRefreshRejected", err)
	}
	if _, err := svc.Refresh(ctx, ""); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("empty token = %v, want ErrRefreshRejected", err)
	}

	setNow(testNow.Add(identity.RefreshTokenTTL + time.Hour))
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("expired token = %v, want ErrRefreshRejected", err)
	}
}

func TestConcurrentRefreshMintsExactlyOneSuccessor(t *testing.T) {
	// Two goroutines present the same valid token at the same instant. The
	// conditional update in MarkRotated is what makes exactly one of them the
	// rotator; the other must fall back to the replay path.
	//
	// If both rotated, the family would fork into two live branches and the
	// next reuse check would revoke a legitimate session at random.
	svc, store, setNow := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	before := store.TokenCount()

	setNow(testNow.Add(time.Hour))

	const racers = 8

	// A barrier inside MarkRotated, so every goroutine has already READ the
	// token — and seen rotated_at as NULL — before any of them writes.
	//
	// This is the entire point of the test. Without the barrier the goroutines
	// serialise: the first completes its whole rotation, and the rest see an
	// already-rotated token and take the overlap-replay path, so the double
	// rotation never occurs and the test passes even with the conditional
	// update removed. Verified by deleting the guard and watching this fail.
	arrived := make(chan struct{}, racers)
	release := make(chan struct{})
	store.BeforeMarkRotated = func() {
		arrived <- struct{}{}
		<-release
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	newTokens := 0
	failures := 0

	start := make(chan struct{})
	for range racers {
		wg.Go(func() {
			<-start
			got, err := svc.Refresh(ctx, pair.RefreshToken)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures++
			case got.RefreshToken != "":
				newTokens++
			}
		})
	}
	close(start)

	// Wait until all eight are inside MarkRotated holding a stale read, then
	// let them all through at once.
	for range racers {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the racers to reach MarkRotated")
		}
	}
	close(release)
	wg.Wait()

	if failures != 0 {
		t.Errorf("%d of %d concurrent refreshes failed; an honest client racing itself must not be punished", failures, racers)
	}
	if newTokens != 1 {
		t.Errorf("%d successors were minted, want exactly 1; the family has forked", newTokens)
	}
	if got := store.TokenCount() - before; got != 1 {
		t.Errorf("%d tokens were stored, want exactly 1", got)
	}
	if reason := store.FamilyRevokedReason(store.OnlyFamilyID()); reason != "" {
		t.Errorf("the family was revoked (%q) by an honest concurrent refresh", reason)
	}
}

func TestLogoutRevokesTheSessionAndItsRefreshTokens(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := svc.Logout(ctx, pair.SessionID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if !store.SessionRevoked(pair.SessionID) {
		t.Fatal("the session was not revoked")
	}

	// The access token is still cryptographically valid — that is exactly the
	// point. Authenticate must reject it anyway, or "log out" leaves a
	// working credential for up to 15 minutes.
	if _, err := svc.Authenticate(ctx, pair.AccessToken); !errors.Is(err, identity.ErrSessionRevoked) {
		t.Errorf("Authenticate after logout = %v, want ErrSessionRevoked", err)
	}
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("Refresh after logout = %v, want ErrRefreshRejected", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := range 3 {
		if err := svc.Logout(ctx, pair.SessionID); err != nil {
			t.Fatalf("Logout #%d: %v", i+1, err)
		}
	}
	// An unknown session too — a client retrying after a timeout must not be
	// told its successful request failed.
	if err := svc.Logout(ctx, "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Errorf("Logout of an unknown session: %v", err)
	}
}

func TestAuthenticateRejectsAForgedToken(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	// Signed by a different key entirely.
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	other, err := identity.NewTokenIssuer(otherPriv, "vybe", "vybe-app")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	forged, err := other.Mint("some-user", "some-session", "premium", "jti")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := svc.Authenticate(ctx, forged); err == nil {
		t.Fatal("a token signed by a foreign key was accepted")
	}
}

func TestNormaliseEmail(t *testing.T) {
	ok := []struct{ in, want string }{
		{"a@b.co", "a@b.co"},
		{"  A@B.CO  ", "a@b.co"},
		{"first.last+tag@sub.example.com", "first.last+tag@sub.example.com"},
		// Surrounding whitespace is trimmed, newlines included — a pasted
		// address commonly carries one. Interior whitespace is a different
		// matter and is rejected below.
		{"a@b.co\n", "a@b.co"},
		{"\t a@b.co \r\n", "a@b.co"},
	}
	for _, tt := range ok {
		got, err := identity.NormaliseEmail(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("NormaliseEmail(%q) = %q, %v; want %q, nil", tt.in, got, err, tt.want)
		}
	}

	bad := []string{
		"", "   ", "no-at-sign", "@example.com", "user@", "a@b@c.com",
		"user@nodot", "user name@example.com", "user@exa mple.com",
		"a\t@b.co",
		strings.Repeat("a", 250) + "@example.com", // over 254
	}
	for _, in := range bad {
		if got, err := identity.NormaliseEmail(in); !errors.Is(err, identity.ErrInvalidEmail) {
			t.Errorf("NormaliseEmail(%q) = %q, %v; want ErrInvalidEmail", in, got, err)
		}
	}
}

func TestNormaliseHandle(t *testing.T) {
	ok := []struct{ in, want string }{
		{"sara", "sara"},
		{"  SARA  ", "sara"},
		{"sara_q", "sara_q"},
		{"sara.q", "sara.q"},
		{"a1b2c3", "a1b2c3"},
		{strings.Repeat("a", 30), strings.Repeat("a", 30)},
	}
	for _, tt := range ok {
		got, err := identity.NormaliseHandle(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("NormaliseHandle(%q) = %q, %v; want %q, nil", tt.in, got, err, tt.want)
		}
	}

	bad := []struct{ name, in string }{
		{"empty", ""},
		{"too short", "ab"},
		{"too long", strings.Repeat("a", 31)},
		{"space", "sara q"},
		{"leading dot", ".sara"},
		{"trailing dot", "sara."},
		{"leading underscore", "_sara"},
		{"trailing underscore", "sara_"},
		{"doubled dot", "sa..ra"},
		{"dot then underscore", "sa._ra"},
		// The homograph case. Cyrillic а is indistinguishable from Latin a in
		// most fonts, so allowing it would let anyone impersonate any handle.
		{"cyrillic lookalike", "sаra"},
		{"arabic script", "سارة"},
		{"emoji", "sara🎬"},
		{"at sign", "sara@q"},
		{"hyphen", "sara-q"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := identity.NormaliseHandle(tt.in); !errors.Is(err, identity.ErrInvalidHandle) {
				t.Errorf("NormaliseHandle(%q) = %q, %v; want ErrInvalidHandle", tt.in, got, err)
			}
		})
	}
}

func TestPlatformIsCoercedToTheEnum(t *testing.T) {
	// The column has a CHECK constraint. An unrecognised platform must become
	// "unknown" here rather than a 500 at insert time.
	svc, store, _ := newService(t)
	ctx := context.Background()

	in := validRegistration()
	in.Platform = "SymbianOS"
	pair, err := svc.Register(ctx, in)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess, err := store.SessionByID(ctx, pair.SessionID)
	if err != nil || sess == nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess.Platform != "unknown" {
		t.Errorf("platform = %q, want unknown", sess.Platform)
	}
	if sess.DeviceName != "Pixel 8" {
		t.Errorf("device name = %q, want the submitted value", sess.DeviceName)
	}
}

func TestRegisterPropagatesStorageFailures(t *testing.T) {
	// A broken database must surface as an error the handler can map to a 500,
	// not as a half-created account.
	boom := errors.New("connection refused")
	for _, method := range []string{
		"EmailTaken", "HandleTaken", "CreateUser", "CreateSession",
		"CreateFamily", "InsertRefreshToken",
	} {
		t.Run(method, func(t *testing.T) {
			svc, store, _ := newService(t)
			store.FailNext[method] = boom
			if _, err := svc.Register(context.Background(), validRegistration()); !errors.Is(err, boom) {
				t.Errorf("Register with %s failing = %v, want the underlying error", method, err)
			}
		})
	}
}

func TestRefreshPropagatesStorageFailures(t *testing.T) {
	boom := errors.New("connection reset by peer")
	for _, method := range []string{
		"RefreshTokenByHash", "MarkRotated", "InsertRefreshToken", "SessionByID", "UserByID",
	} {
		t.Run(method, func(t *testing.T) {
			svc, store, setNow := newService(t)
			ctx := context.Background()
			pair, err := svc.Register(ctx, validRegistration())
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			setNow(testNow.Add(time.Hour))

			store.FailNext[method] = boom
			if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, boom) {
				t.Errorf("Refresh with %s failing = %v, want the underlying error", method, err)
			}
		})
	}
}

func TestRefreshFailsWhenTheUserIsGone(t *testing.T) {
	// A deleted account whose refresh token has not yet expired. §6.5's hard
	// delete removes the user row; the token must stop working rather than
	// minting an access token for a subject that no longer exists.
	svc, store, setNow := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	setNow(testNow.Add(time.Hour))
	store.DeleteUser(pair.User.ID)

	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("Refresh for a deleted user = %v, want ErrRefreshRejected", err)
	}
}

func TestAuthenticatePropagatesStorageFailures(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	boom := errors.New("database is starting up")
	store.FailNext["SessionByID"] = boom
	if _, err := svc.Authenticate(ctx, pair.AccessToken); !errors.Is(err, boom) {
		t.Errorf("Authenticate with SessionByID failing = %v, want the underlying error", err)
	}
}

func TestLogoutPropagatesStorageFailures(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	pair, err := svc.Register(ctx, validRegistration())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	boom := errors.New("deadlock detected")
	store.FailNext["RevokeSession"] = boom
	if err := svc.Logout(ctx, pair.SessionID); !errors.Is(err, boom) {
		t.Errorf("Logout with RevokeSession failing = %v, want the underlying error", err)
	}
}

func TestLoginPropagatesStorageFailures(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, validRegistration()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	boom := errors.New("too many connections")
	store.FailNext["UserByEmail"] = boom
	_, err := svc.Login(ctx, identity.LoginInput{Email: "sara@example.com", Password: "a sufficiently long passphrase"})
	if !errors.Is(err, boom) {
		t.Errorf("Login with UserByEmail failing = %v, want the underlying error", err)
	}
	// Specifically NOT ErrInvalidCredentials: a database outage that presents
	// as "wrong password" sends every user to the password-reset flow during
	// an incident, which is the worst possible time for that traffic.
	if errors.Is(err, identity.ErrInvalidCredentials) {
		t.Error("a storage failure was reported as invalid credentials")
	}
}

func TestNormalisePlatformCoversEveryEnumMember(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	for i, p := range []string{"android", "IOS", " web ", "", "plan9"} {
		in := validRegistration()
		in.Email = "u" + string(rune('a'+i)) + "@example.com"
		in.Handle = "user" + string(rune('a'+i))
		in.Platform = p
		pair, err := svc.Register(ctx, in)
		if err != nil {
			t.Fatalf("Register(platform=%q): %v", p, err)
		}
		sess, _ := store.SessionByID(ctx, pair.SessionID)
		want := map[string]string{"android": "android", "IOS": "ios", " web ": "web"}[p]
		if want == "" {
			want = "unknown"
		}
		if sess.Platform != want {
			t.Errorf("platform %q stored as %q, want %q", p, sess.Platform, want)
		}
	}
}
