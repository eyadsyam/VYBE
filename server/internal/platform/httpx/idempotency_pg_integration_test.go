package httpx_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

// Integration tests for the Postgres idempotency store.
//
// These SKIP without VYBE_DB_DSN and RUN in CI against a real Postgres 17.
//
// The whole contract rests on Reserve being ATOMIC — one statement that either
// inserts or fetches the conflicting row. A read-then-write has a window
// between the two in which two concurrent retries both see nothing and both
// proceed, which is precisely the double-charge FR-57 exists to prevent. Only
// a real database can demonstrate that the CTE actually closes it.

func idemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VYBE_DB_DSN")
	if dsn == "" {
		t.Skip("VYBE_DB_DSN is not set; skipping the Postgres integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// idemUser inserts a throwaway user, because idempotency_keys.user_id is a
// foreign key. Returns its id.
func idemUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (id, handle, display_name, age_band, date_of_birth)
		VALUES (uuid_generate_v7(), $1, 'idem fixture', 'adult'::age_band, '2000-01-01')
		RETURNING id::text`, idemHandle(t)).Scan(&id)
	if err != nil {
		t.Fatalf("inserting a fixture user: %v", err)
	}

	t.Cleanup(func() {
		// ON DELETE CASCADE takes the idempotency rows with it.
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func idemHandle(t *testing.T) string {
	t.Helper()
	sum := 0
	for _, r := range t.Name() {
		sum = sum*31 + int(r)
		sum %= 1 << 28
	}
	const digits = "abcdefghijklmnopqrstuvwxyz"
	out := []byte("idem")
	for sum > 0 {
		out = append(out, digits[sum%26])
		sum /= 26
	}
	return string(out)
}

func TestPGIdempotencyReserveThenComplete(t *testing.T) {
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "reserve-complete-01"
	fingerprint := []byte("fp-1")

	// First reservation: nothing existed, so no prior record is reported.
	existing, err := store.Reserve(ctx, user, key, "/v1/rooms", fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if existing != nil {
		t.Fatalf("a fresh key reported an existing record: %+v", existing)
	}

	// A second reservation before completion sees the in-flight record. That
	// is what produces a 409 rather than running the handler twice.
	existing, err = store.Reserve(ctx, user, key, "/v1/rooms", fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("second Reserve: %v", err)
	}
	if existing == nil {
		t.Fatal("the second reservation did not see the in-flight record")
	}
	if existing.Status != httpx.IdemInFlight {
		t.Errorf("status = %q, want in_flight", existing.Status)
	}

	body, _ := json.Marshal(map[string]string{"id": "room-1"})
	if err := store.Complete(ctx, user, key, 201, body); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Now it replays.
	existing, err = store.Reserve(ctx, user, key, "/v1/rooms", fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("Reserve after Complete: %v", err)
	}
	if existing == nil || existing.Status != httpx.IdemCompleted {
		t.Fatalf("record = %+v, want a completed one", existing)
	}
	if existing.ResponseStatus != 201 {
		t.Errorf("response status = %d, want 201", existing.ResponseStatus)
	}
	if string(existing.ResponseBody) != string(body) {
		t.Errorf("replayed body = %s, want %s", existing.ResponseBody, body)
	}
}

func TestPGIdempotencyIsScopedPerActor(t *testing.T) {
	// Two users, the SAME key. Without the user_id in the primary key, one
	// user's retry would replay the other's response — handing them a room id
	// they have no business seeing.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()

	alice := idemUser(t, pool)

	var bob string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (id, handle, display_name, age_band, date_of_birth)
		VALUES (uuid_generate_v7(), $1, 'bob', 'adult'::age_band, '2000-01-01')
		RETURNING id::text`, idemHandle(t)+"b").Scan(&bob)
	if err != nil {
		t.Fatalf("inserting bob: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, bob)
	})

	const key = "the-very-same-key-01"

	if _, err := store.Reserve(ctx, alice, key, "/v1/rooms", []byte("a"), time.Hour); err != nil {
		t.Fatalf("alice Reserve: %v", err)
	}
	if err := store.Complete(ctx, alice, key, 201, []byte(`{"id":"alice-room"}`)); err != nil {
		t.Fatalf("alice Complete: %v", err)
	}

	existing, err := store.Reserve(ctx, bob, key, "/v1/rooms", []byte("b"), time.Hour)
	if err != nil {
		t.Fatalf("bob Reserve: %v", err)
	}
	if existing != nil {
		t.Fatalf("bob's reservation saw alice's record: %+v", existing)
	}
}

func TestPGIdempotencyReserveIsAtomicUnderConcurrency(t *testing.T) {
	// The property that justifies the CTE. Eight goroutines reserve the same
	// key at once; exactly ONE must be told it has a fresh reservation, and the
	// other seven must see the existing record.
	//
	// A read-then-write implementation lets several see nothing and all proceed
	// — which is the double-charge FR-57 exists to prevent.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "concurrent-reserve-1"
	const racers = 8

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	sawExisting := 0
	failures := 0

	start := make(chan struct{})
	for range racers {
		wg.Go(func() {
			<-start
			existing, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), time.Hour)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures++
			case existing == nil:
				claimed++
			default:
				sawExisting++
			}
		})
	}
	close(start)
	wg.Wait()

	if failures != 0 {
		t.Errorf("%d reservations errored", failures)
	}
	if claimed != 1 {
		t.Errorf("%d of %d concurrent reservations claimed the key, want exactly 1; "+
			"the handler would run %d times for one logical request",
			claimed, racers, claimed)
	}
	if sawExisting != racers-1 {
		t.Errorf("%d saw the existing record, want %d", sawExisting, racers-1)
	}
}

func TestPGIdempotencyReleaseAllowsARetry(t *testing.T) {
	// Called when the handler produced a 5xx, which is by definition not a
	// terminal answer. Leaving the reservation would make a transient database
	// blip permanently poison that key: every retry would see an in_flight
	// record and get a 409 forever.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "release-then-retry-1"

	if _, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := store.Release(ctx, user, key); err != nil {
		t.Fatalf("Release: %v", err)
	}

	existing, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), time.Hour)
	if err != nil {
		t.Fatalf("Reserve after Release: %v", err)
	}
	if existing != nil {
		t.Fatalf("the released key still reported a record: %+v", existing)
	}
}

func TestPGIdempotencyReleaseDoesNotDiscardACompletedRecord(t *testing.T) {
	// Release is `... AND status = 'in_flight'` on purpose. Deleting a
	// COMPLETED record would throw away the response a client is entitled to
	// replay, turning a successful request into one that runs a second time.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "release-completed-01"

	if _, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := store.Complete(ctx, user, key, 201, []byte(`{"id":"x"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := store.Release(ctx, user, key); err != nil {
		t.Fatalf("Release: %v", err)
	}

	existing, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), time.Hour)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if existing == nil || existing.Status != httpx.IdemCompleted {
		t.Fatalf("the completed record did not survive Release: %+v", existing)
	}
}

func TestPGIdempotencyReportsAChangedFingerprint(t *testing.T) {
	// Same key, different body. That is a client bug, and returning the FIRST
	// response would silently answer a question the client did not ask.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "changed-fingerprint-1"

	if _, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("original"), time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := store.Complete(ctx, user, key, 201, []byte(`{"id":"x"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	existing, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("different"), time.Hour)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if existing == nil {
		t.Fatal("no record reported")
	}
	// The STORED fingerprint comes back, so the middleware can compare it
	// against what this request actually carried and answer 422.
	if string(existing.Fingerprint) != "original" {
		t.Errorf("fingerprint = %q, want the originally stored one", existing.Fingerprint)
	}
}

func TestPGIdempotencyExpiredKeyIsReclaimable(t *testing.T) {
	// §5.2's 24-hour window is the point after which a replay is no longer
	// owed. Without the `expires_at <= now()` clause the key would be blocked
	// forever, and a client reusing a key from last week would get a 409 it
	// cannot resolve.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "already-expired-001"

	// A zero TTL means it is expired the instant it is written.
	if _, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	existing, err := store.Reserve(ctx, user, key, "/v1/rooms", []byte("fp"), time.Hour)
	if err != nil {
		t.Fatalf("Reserve of an expired key: %v", err)
	}
	if existing != nil {
		t.Errorf("an expired key still reported a record: %+v", existing)
	}
}

func TestPGIdempotencyPurgeExpired(t *testing.T) {
	// A silently-growing idempotency table is a slow leak that only shows up
	// as query latency months later.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	if _, err := store.Reserve(ctx, user, "purge-me-000000001", "/v1/rooms", []byte("fp"), 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := store.Reserve(ctx, user, "keep-me-0000000001", "/v1/rooms", []byte("fp"), time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	removed, err := store.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if removed < 1 {
		t.Errorf("purged %d rows, want at least the expired one", removed)
	}

	// The live one survives.
	existing, err := store.Reserve(ctx, user, "keep-me-0000000001", "/v1/rooms", []byte("fp"), time.Hour)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if existing == nil {
		t.Error("the purge removed a live reservation")
	}
}

func TestPGIdempotencyCompleteWithNoBody(t *testing.T) {
	// A 204 legitimately has no body, and jsonb rejects an empty string. This
	// is the path that would 500 at runtime on the logout endpoint.
	pool := idemPool(t)
	store := httpx.NewPostgresIdemStore(pool)
	ctx := context.Background()
	user := idemUser(t, pool)

	const key = "no-body-000000001"

	if _, err := store.Reserve(ctx, user, key, "/v1/auth/logout", []byte("fp"), time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := store.Complete(ctx, user, key, 204, nil); err != nil {
		t.Fatalf("Complete with no body: %v", err)
	}

	existing, err := store.Reserve(ctx, user, key, "/v1/auth/logout", []byte("fp"), time.Hour)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if existing == nil || existing.ResponseStatus != 204 {
		t.Fatalf("record = %+v, want a completed 204", existing)
	}
	if len(existing.ResponseBody) != 0 {
		t.Errorf("body = %q, want none; a literal \"null\" would decode to a null object",
			existing.ResponseBody)
	}
}
