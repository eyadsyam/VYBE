package identity

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// The Redis TicketStore.
//
// Redis is the RIGHT home for this under ADR-009, which is worth stating
// because the neighbouring idempotency store deliberately is not. The rule is
// "Redis holds only reconstructible state", and a WS ticket qualifies: losing
// the store logs people out of sockets they can re-establish in one round trip
// by asking for a new ticket. An idempotency record, by contrast, is the sole
// evidence a request already happened, and losing it duplicates work with
// nothing anywhere to detect it.
//
// Single-use redemption is the whole contract, and it is delegated to Redis's
// GETDEL rather than implemented as GET-then-DEL. Two upgrades presenting the
// same ticket at the same moment must not both succeed, and a read followed by
// a delete has a window between them where both do.

const ticketKeyPrefix = "vybe:ws-ticket:"

// RedisTicketStore implements TicketStore.
type RedisTicketStore struct {
	rdb redis.Cmdable
}

// NewRedisTicketStore returns a TicketStore backed by rdb.
func NewRedisTicketStore(rdb redis.Cmdable) *RedisTicketStore {
	return &RedisTicketStore{rdb: rdb}
}

// storedTicket is the value side. The plaintext is deliberately absent: the
// key is derived from it, so storing it again would put the credential itself
// in the value where a MONITOR or a memory dump would show it next to the
// user it belongs to.
type storedTicket struct {
	UserID    string    `json:"u"`
	SessionID string    `json:"s"`
	ExpiresAt time.Time `json:"e"`
}

func ticketKey(hash []byte) string { return ticketKeyPrefix + hex.EncodeToString(hash) }

// Put stores a ticket under its hash with a TTL.
func (s *RedisTicketStore) Put(ctx context.Context, t *WSTicket) error {
	encoded, err := json.Marshal(storedTicket{
		UserID: t.UserID, SessionID: t.SessionID, ExpiresAt: t.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("encoding ticket: %w", err)
	}

	// The TTL is belt and braces alongside the ExpiresAt check in Redeem. The
	// TTL bounds memory even if nobody ever redeems; the explicit check means
	// correctness does not depend on Redis's expiry being prompt, which it is
	// not obliged to be.
	ttl := time.Until(t.ExpiresAt)
	if ttl <= 0 {
		ttl = WSTicketTTL
	}
	if err := s.rdb.Set(ctx, ticketKey(t.Hash), encoded, ttl).Err(); err != nil {
		return fmt.Errorf("storing ticket: %w", err)
	}
	return nil
}

// Redeem returns the ticket and deletes it in one atomic operation.
func (s *RedisTicketStore) Redeem(ctx context.Context, plaintext string, now time.Time) (*WSTicket, error) {
	hash := HashWSTicket(plaintext)

	// GETDEL, not GET then DEL. Two upgrades presenting the same ticket at the
	// same instant must not both succeed, and the two-command version has a
	// window in which they do.
	raw, err := s.rdb.GetDel(ctx, ticketKey(hash)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrTicketNotFound
		}
		return nil, fmt.Errorf("redeeming ticket: %w", err)
	}

	var stored storedTicket
	if err := json.Unmarshal(raw, &stored); err != nil {
		// A corrupt value is unusable and now deleted, which is the right
		// outcome. Report it as not-found rather than an internal error: the
		// caller's correct response is identical, and a 500 here would tell an
		// attacker that their ticket key existed.
		return nil, ErrTicketNotFound
	}

	ticket := &WSTicket{
		Hash:      hash,
		UserID:    stored.UserID,
		SessionID: stored.SessionID,
		ExpiresAt: stored.ExpiresAt,
	}
	if TicketExpired(ticket, now) {
		return nil, ErrTicketNotFound
	}
	return ticket, nil
}

// RedeemForRealtime adapts Redeem to the realtime module's narrow interface.
//
// realtime must not import identity (§5.1), so it declares a TicketRedeemer
// that returns two strings. This method is the adapter, and it lives here
// rather than in realtime so the dependency stays one-directional.
func (s *RedisTicketStore) RedeemForRealtime(ctx context.Context, plaintext string, now time.Time) (userID, sessionID string, err error) {
	t, err := s.Redeem(ctx, plaintext, now)
	if err != nil {
		return "", "", err
	}
	return t.UserID, t.SessionID, nil
}

var _ TicketStore = (*RedisTicketStore)(nil)
