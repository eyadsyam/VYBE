package identity_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
)

// Integration tests for the Redis ticket store.
//
// These SKIP without VYBE_REDIS_ADDR and RUN in CI, which provides a real
// Redis. The property that justifies them cannot be tested any other way:
// single-use redemption is delegated to Redis's GETDEL, and whether GETDEL is
// actually atomic under concurrency is a question about Redis, not about Go.
//
// A ticket that survived redemption would be a replayable credential sitting
// in a URL and in every proxy log that saw it.

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("VYBE_REDIS_ADDR")
	if addr == "" {
		t.Skip("VYBE_REDIS_ADDR is not set; skipping the Redis integration tests")
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("pinging Redis at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newTicket(t *testing.T, userID string, now time.Time) *identity.WSTicket {
	t.Helper()
	ticket, err := identity.NewWSTicket(userID, "session-"+userID, now)
	if err != nil {
		t.Fatalf("NewWSTicket: %v", err)
	}
	return ticket
}

func TestRedisTicketRoundTrip(t *testing.T) {
	store := identity.NewRedisTicketStore(redisClient(t))
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-roundtrip", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}

	redeemed, err := store.Redeem(ctx, ticket.Plaintext, now)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if redeemed.UserID != "user-roundtrip" {
		t.Errorf("user = %q, want user-roundtrip", redeemed.UserID)
	}
	if redeemed.SessionID != "session-user-roundtrip" {
		t.Errorf("session = %q", redeemed.SessionID)
	}
}

func TestRedisTicketIsSingleUse(t *testing.T) {
	store := identity.NewRedisTicketStore(redisClient(t))
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-single", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := store.Redeem(ctx, ticket.Plaintext, now); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := store.Redeem(ctx, ticket.Plaintext, now); !errors.Is(err, identity.ErrTicketNotFound) {
		t.Errorf("second Redeem = %v, want ErrTicketNotFound", err)
	}
}

func TestRedisTicketRedemptionIsAtomicUnderConcurrency(t *testing.T) {
	// The test that only a real Redis can run. Eight goroutines present the
	// same ticket simultaneously; GETDEL is what makes exactly one of them the
	// winner.
	//
	// A GET-then-DEL implementation has a window between the two commands in
	// which several callers all read the value and all succeed — and each of
	// them opens a socket on a credential that was meant to be used once.
	store := identity.NewRedisTicketStore(redisClient(t))
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-race", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	otherErrors := 0

	start := make(chan struct{})
	for range racers {
		wg.Go(func() {
			<-start
			_, err := store.Redeem(ctx, ticket.Plaintext, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, identity.ErrTicketNotFound):
				// Expected for the losers.
			default:
				otherErrors++
			}
		})
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Errorf("%d of %d concurrent redemptions succeeded, want exactly 1; "+
			"the ticket is replayable", successes, racers)
	}
	if otherErrors != 0 {
		t.Errorf("%d redemptions failed with an unexpected error", otherErrors)
	}
}

func TestRedisTicketExpires(t *testing.T) {
	store := identity.NewRedisTicketStore(redisClient(t))
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-expiry", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The explicit expiry check, not Redis's TTL. Correctness must not depend
	// on Redis expiring a key promptly — it is not obliged to, and a ticket
	// that outlives its window by even a second is a wider replay window than
	// ADR-011 allows.
	late := now.Add(identity.WSTicketTTL + time.Second)
	if _, err := store.Redeem(ctx, ticket.Plaintext, late); !errors.Is(err, identity.ErrTicketNotFound) {
		t.Errorf("redeeming %v after issue = %v, want ErrTicketNotFound",
			identity.WSTicketTTL+time.Second, err)
	}
}

func TestRedisTicketSetsATTL(t *testing.T) {
	// Belt to the expiry check's braces. Without a TTL, an unredeemed ticket
	// occupies memory forever — and most tickets ARE unredeemed, because a
	// client that fails to connect simply asks for another.
	client := redisClient(t)
	store := identity.NewRedisTicketStore(client)
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-ttl", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}

	keys, err := client.Keys(ctx, "vybe:ws-ticket:*").Result()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}

	found := false
	for _, key := range keys {
		ttl, err := client.TTL(ctx, key).Result()
		if err != nil {
			continue
		}
		if ttl > 0 && ttl <= identity.WSTicketTTL {
			found = true
		}
		if ttl < 0 {
			t.Errorf("key %s has no TTL (%v); unredeemed tickets would leak", key, ttl)
		}
	}
	if !found {
		t.Error("no ticket key carried a TTL within the 60-second window")
	}

	// Clean up so a re-run does not accumulate keys.
	if len(keys) > 0 {
		_ = client.Del(ctx, keys...).Err()
	}
}

func TestRedisTicketRejectsAnUnknownPlaintext(t *testing.T) {
	store := identity.NewRedisTicketStore(redisClient(t))
	ctx := context.Background()

	if _, err := store.Redeem(ctx, "never-issued", time.Now()); !errors.Is(err, identity.ErrTicketNotFound) {
		t.Errorf("an unknown ticket = %v, want ErrTicketNotFound", err)
	}
}

func TestRedisTicketStoresNoPlaintext(t *testing.T) {
	// The key is DERIVED from the plaintext, so storing the plaintext in the
	// value too would put the credential itself where a MONITOR or a memory
	// dump shows it next to the user it belongs to.
	client := redisClient(t)
	store := identity.NewRedisTicketStore(client)
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-noplaintext", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Redeem(context.Background(), ticket.Plaintext, now)
	})

	keys, err := client.Keys(ctx, "vybe:ws-ticket:*").Result()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	for _, key := range keys {
		value, err := client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		if strings.Contains(value, ticket.Plaintext) {
			t.Fatalf("the stored value contains the ticket plaintext: %s", value)
		}
		// And the key must not be the plaintext either.
		if strings.Contains(key, ticket.Plaintext) {
			t.Fatalf("the key contains the ticket plaintext: %s", key)
		}
	}
}

func TestRedisTicketAdaptsToTheRealtimeInterface(t *testing.T) {
	// realtime declares its own narrow TicketRedeemer so it never imports
	// identity (§5.1). This is the adapter that satisfies it.
	store := identity.NewRedisTicketStore(redisClient(t))
	ctx := context.Background()
	now := time.Now().UTC()

	ticket := newTicket(t, "user-adapter", now)
	if err := store.Put(ctx, ticket); err != nil {
		t.Fatalf("Put: %v", err)
	}

	userID, sessionID, err := store.RedeemForRealtime(ctx, ticket.Plaintext, now)
	if err != nil {
		t.Fatalf("RedeemForRealtime: %v", err)
	}
	if userID != "user-adapter" || sessionID != "session-user-adapter" {
		t.Errorf("got (%q, %q)", userID, sessionID)
	}

	// And it is single-use through the adapter too.
	if _, _, err := store.RedeemForRealtime(ctx, ticket.Plaintext, now); err == nil {
		t.Error("the adapter allowed a second redemption")
	}
}
