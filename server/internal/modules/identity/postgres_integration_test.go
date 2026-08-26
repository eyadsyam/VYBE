package identity_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
	"github.com/eyadsyam/vybe/server/internal/platform/passwords"
)

// Integration tests for the Postgres repository.
//
// These SKIP without VYBE_DB_DSN, so a local run stays fast and offline, and
// they RUN in CI, which provides a real Postgres 17. That split is deliberate:
// the queries in postgres.go are the least testable and most consequential
// code in the module — a fake agrees with whatever we assumed, and the two
// properties below cannot be observed anywhere else:
//
//   - CreateUser's transaction. A partial write leaves an account that can
//     never log in and whose handle is permanently taken.
//   - MarkRotated's conditional update. It is what makes exactly one of two
//     racing refreshes the winner, and the in-memory fake can only model that,
//     not prove it.
//
// Everything here creates its own rows with unique identifiers and cleans up,
// so the tests can run against a shared database in any order.

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VYBE_DB_DSN")
	if dsn == "" {
		t.Skip("VYBE_DB_DSN is not set; skipping the Postgres integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to %s: %v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging Postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// uniqueSuffix keeps parallel and repeated runs from colliding on the unique
// constraints, without needing a truncate between tests.
func uniqueSuffix(t *testing.T) string {
	t.Helper()
	name := strings.ToLower(t.Name())
	name = strings.NewReplacer("/", "", "_", "", "-", "").Replace(name)
	if len(name) > 16 {
		name = name[len(name)-16:]
	}
	return name
}

func newPGService(t *testing.T) (*identity.Service, *identity.PostgresRepository) {
	t.Helper()
	pool := integrationPool(t)

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tokens, err := identity.NewTokenIssuer(priv, "vybe", "vybe-app")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	breaches, err := identity.EmbeddedBreachSet()
	if err != nil {
		t.Fatalf("EmbeddedBreachSet: %v", err)
	}

	repo := identity.NewPostgresRepository(pool)
	svc := identity.NewService(repo, tokens, identity.PasswordPolicy{Breaches: breaches}, passwords.TestParams)
	return svc, repo
}

func pgRegistration(t *testing.T) identity.RegisterInput {
	t.Helper()
	s := uniqueSuffix(t)
	return identity.RegisterInput{
		Email:       "pg" + s + "@example.com",
		Password:    "a sufficiently long passphrase",
		Handle:      "pg" + s,
		DisplayName: "سارة",
		DateOfBirth: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		Locale:      "ar",
		Region:      "EG",
		DeviceName:  "CI runner",
		Platform:    "android",
	}
}

func cleanupUser(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	t.Cleanup(func() {
		// ON DELETE CASCADE takes credentials, sessions, families, and tokens
		// with it, which is itself worth exercising: a missing cascade would
		// leave orphans that break the next run's unique constraints.
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
}

func TestPGRegisterAndLoginRoundTrip(t *testing.T) {
	svc, _ := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()
	in := pgRegistration(t)

	pair, err := svc.Register(ctx, in)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, pair.User.ID)

	// The age_band and entitlement_tier casts must match the real enums, which
	// is something only a live database can confirm — an invalid label is a
	// 22P02 at insert time, not a compile error.
	if pair.User.AgeBand != identity.AgeAdult {
		t.Errorf("age band = %q", pair.User.AgeBand)
	}
	if pair.User.EntitlementTier != "free" {
		t.Errorf("entitlement = %q", pair.User.EntitlementTier)
	}

	back, err := svc.Login(ctx, identity.LoginInput{
		Email: in.Email, Password: in.Password, Platform: "ios",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if back.User.ID != pair.User.ID {
		t.Errorf("login returned user %q, want %q", back.User.ID, pair.User.ID)
	}

	// Arabic must survive Postgres, not just Go.
	if back.User.DisplayName != "سارة" {
		t.Errorf("display name = %q; Arabic did not round-trip through the database", back.User.DisplayName)
	}
}

func TestPGDuplicateHandleAndEmailAreMappedNotFiveHundreds(t *testing.T) {
	// The service pre-checks both, but the check races. This proves the
	// constraint violation is translated back onto the same sentinel, so the
	// loser of a race gets a 409 rather than a 500 with a Postgres message.
	svc, _ := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()
	in := pgRegistration(t)

	first, err := svc.Register(ctx, in)
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	cleanupUser(t, pool, first.User.ID)

	dupEmail := in
	dupEmail.Handle = in.Handle + "b"
	if _, err := svc.Register(ctx, dupEmail); !errors.Is(err, identity.ErrEmailTaken) {
		t.Errorf("duplicate email = %v, want ErrEmailTaken", err)
	}

	dupHandle := in
	dupHandle.Email = "other" + uniqueSuffix(t) + "@example.com"
	if _, err := svc.Register(ctx, dupHandle); !errors.Is(err, identity.ErrHandleTaken) {
		t.Errorf("duplicate handle = %v, want ErrHandleTaken", err)
	}
}

func TestPGCreateUserIsAtomic(t *testing.T) {
	// The property the transaction exists for. A user row with no credentials
	// row is an account that can never log in and whose handle is permanently
	// taken — unrecoverable without manual surgery.
	//
	// Forced by taking the email first, so the credentials insert fails while
	// the users insert has already succeeded inside the same transaction.
	svc, repo := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()

	occupant := pgRegistration(t)
	first, err := svc.Register(ctx, occupant)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, first.User.ID)

	// A DIFFERENT handle with the SAME email. The service's pre-check would
	// catch this, so the repository is called directly to reach the SQL path.
	orphanHandle := occupant.Handle + "x"
	orphan := &identity.User{
		ID: newTestUUID(t), Handle: orphanHandle, DisplayName: "orphan",
		Locale: "en", Region: "EG", NumeralSystem: "western",
		AgeBand: identity.AgeAdult, DateOfBirth: occupant.DateOfBirth,
		EntitlementTier: "free", IsDiscoverable: true, CreatedAt: time.Now().UTC(),
	}
	cleanupUser(t, pool, orphan.ID)

	err = repo.CreateUser(ctx, orphan, occupant.Email, "$argon2id$fake")
	if !errors.Is(err, identity.ErrEmailTaken) {
		t.Fatalf("CreateUser with a taken email = %v, want ErrEmailTaken", err)
	}

	// The users row must NOT survive.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE handle = $1)`, orphanHandle).Scan(&exists); err != nil {
		t.Fatalf("checking for an orphan: %v", err)
	}
	if exists {
		t.Error("the users row survived a failed credentials insert; the transaction did not roll back, " +
			"and that handle is now permanently taken by an account that cannot log in")
	}
}

func TestPGRefreshRotationUnderRealConcurrency(t *testing.T) {
	// The in-memory fake needed a barrier to make this race happen. Postgres
	// makes it happen on its own, because `WHERE rotated_at IS NULL` is
	// evaluated under a real row lock — which is the thing being tested.
	svc, _ := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, pgRegistration(t))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, pair.User.ID)

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	successors := 0
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
				successors++
			}
		})
	}
	close(start)
	wg.Wait()

	if failures != 0 {
		t.Errorf("%d of %d concurrent refreshes failed; an honest client racing itself must not be punished",
			failures, racers)
	}
	if successors != 1 {
		t.Errorf("%d successors minted, want exactly 1 — the token family has forked", successors)
	}

	// And the family must be intact: a fork would look like reuse on the next
	// refresh and revoke a real user's session.
	var revoked *time.Time
	err = pool.QueryRow(ctx, `
		SELECT f.revoked_at FROM refresh_token_families f
		JOIN sessions s ON s.id = f.session_id
		WHERE s.id = $1`, pair.SessionID).Scan(&revoked)
	if err != nil {
		t.Fatalf("reading the family: %v", err)
	}
	if revoked != nil {
		t.Errorf("the family was revoked at %v by an honest concurrent refresh", revoked)
	}
}

func TestPGReuseDetectionRevokesTheFamilyAndSession(t *testing.T) {
	svc, _ := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()

	now := time.Now().UTC()
	svc.SetClock(func() time.Time { return now })

	pair, err := svc.Register(ctx, pgRegistration(t))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, pair.User.ID)

	now = now.Add(time.Hour)
	second, err := svc.Refresh(ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// Past the overlap window: this is theft, not a retry.
	now = now.Add(identity.OverlapWindow + time.Second)
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Fatalf("reuse = %v, want ErrRefreshRejected", err)
	}

	var reason *string
	err = pool.QueryRow(ctx, `
		SELECT f.revoked_reason FROM refresh_token_families f
		WHERE f.session_id = $1`, pair.SessionID).Scan(&reason)
	if err != nil {
		t.Fatalf("reading the family: %v", err)
	}
	if reason == nil || *reason != "reuse_detected" {
		t.Errorf("family revoked_reason = %v, want reuse_detected", reason)
	}

	// The successor — which is what a thief would actually hold — must be dead
	// too. Revoking only the presented token leaves them a working credential.
	if _, err := svc.Refresh(ctx, second.RefreshToken); !errors.Is(err, identity.ErrRefreshRejected) {
		t.Errorf("the successor token still worked after family revocation: %v", err)
	}
}

func TestPGLogoutRevokesSessionAndFamilyTogether(t *testing.T) {
	// Two UPDATEs in one transaction. Revoking the session alone leaves a live
	// refresh token that outlives the logout it was supposed to end.
	svc, _ := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, pgRegistration(t))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, pair.User.ID)

	if err := svc.Logout(ctx, pair.SessionID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	var sessionRevoked, familyRevoked *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID).Scan(&sessionRevoked); err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at FROM refresh_token_families WHERE session_id = $1`, pair.SessionID).Scan(&familyRevoked); err != nil {
		t.Fatalf("reading the family: %v", err)
	}
	if sessionRevoked == nil {
		t.Error("the session was not revoked")
	}
	if familyRevoked == nil {
		t.Error("the refresh family survived the logout; the token keeps renewing for 60 days")
	}

	if _, err := svc.Authenticate(ctx, pair.AccessToken); !errors.Is(err, identity.ErrSessionRevoked) {
		t.Errorf("Authenticate after logout = %v, want ErrSessionRevoked", err)
	}
}

func TestPGLogoutIsIdempotentAndKeepsTheFirstTimestamp(t *testing.T) {
	// COALESCE on the revocation timestamp: a retry must not rewrite the audit
	// trail to say the logout happened later than it did.
	svc, _ := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, pgRegistration(t))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, pair.User.ID)

	if err := svc.Logout(ctx, pair.SessionID); err != nil {
		t.Fatalf("first Logout: %v", err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID).Scan(&first); err != nil {
		t.Fatalf("reading revoked_at: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := svc.Logout(ctx, pair.SessionID); err != nil {
		t.Fatalf("second Logout: %v", err)
	}
	var second time.Time
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID).Scan(&second); err != nil {
		t.Fatalf("reading revoked_at: %v", err)
	}
	if !first.Equal(second) {
		t.Errorf("revoked_at moved from %v to %v on a retry; the audit trail was rewritten", first, second)
	}
}

func TestPGEntitlementTierFailsClosed(t *testing.T) {
	// An unknown user gets the FREE cap rather than an error or a generous
	// default. The caller is about to fail an authorisation check anyway, and
	// failing closed is the right direction.
	_, repo := newPGService(t)
	ctx := context.Background()

	tier, err := repo.EntitlementTier(ctx, "00000000-0000-7000-8000-000000000000")
	if err != nil {
		t.Fatalf("EntitlementTier for an unknown user: %v", err)
	}
	if tier != "free" {
		t.Errorf("tier = %q for an unknown user, want free", tier)
	}
}

func TestPGSoftDeletedUsersCannotLogIn(t *testing.T) {
	// §6.5's 30-day grace sets deleted_at rather than removing the row. Every
	// read path filters on it; a query that forgot would let a deleted account
	// keep working.
	svc, repo := newPGService(t)
	pool := integrationPool(t)
	ctx := context.Background()
	in := pgRegistration(t)

	pair, err := svc.Register(ctx, in)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cleanupUser(t, pool, pair.User.ID)

	if _, err := pool.Exec(ctx,
		`UPDATE users SET deleted_at = now() WHERE id = $1`, pair.User.ID); err != nil {
		t.Fatalf("soft-deleting: %v", err)
	}

	if _, err := svc.Login(ctx, identity.LoginInput{Email: in.Email, Password: in.Password}); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("login for a soft-deleted account = %v, want ErrInvalidCredentials", err)
	}
	got, err := repo.UserByID(ctx, pair.User.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got != nil {
		t.Error("UserByID returned a soft-deleted user")
	}
}

// newTestUUID returns a v7 UUID string for rows created outside the service.
func newTestUUID(t *testing.T) string {
	t.Helper()
	pool := integrationPool(t)
	var id string
	// Uses the database's own generator, which also exercises that migration
	// 0001's uuid_generate_v7() exists and produces something uuid-shaped.
	if err := pool.QueryRow(context.Background(), `SELECT uuid_generate_v7()::text`).Scan(&id); err != nil {
		t.Fatalf("generating a uuid: %v", err)
	}
	return id
}
