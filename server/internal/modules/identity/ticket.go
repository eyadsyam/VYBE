package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WSTicketTTL is ADR-011's 60 seconds.
//
// A ticket only has to survive the gap between "client asked for one over
// HTTPS" and "client opened the socket". Sixty seconds is generous for that and
// short enough that a captured ticket is near-worthless even before single-use
// redemption is considered.
const WSTicketTTL = 60 * time.Second

const wsTicketBytes = 32

var (
	// ErrTicketNotFound means the ticket was never issued, or — far more
	// likely — was already redeemed. The two are deliberately not
	// distinguished in the response: telling a caller "that ticket existed but
	// was used" confirms a valid ticket to somebody who guessed one.
	ErrTicketNotFound = errors.New("identity: websocket ticket is not valid")

	// ErrAccessTokenInQuery is FR-5's explicit prohibition.
	ErrAccessTokenInQuery = errors.New("identity: access tokens must not be passed as query parameters")
)

// WSTicket is the single-use credential redeemed during the WS handshake.
type WSTicket struct {
	Plaintext string
	Hash      []byte
	UserID    string
	SessionID string
	ExpiresAt time.Time
}

// NewWSTicket mints a ticket (FR-5).
//
// Only the hash should reach storage, for the same reason as refresh tokens: a
// dump of the ticket store must not be a set of usable credentials, even for
// the sixty seconds they live.
func NewWSTicket(userID, sessionID string, now time.Time) (*WSTicket, error) {
	buf := make([]byte, wsTicketBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeakRandomness, err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	return &WSTicket{
		Plaintext: plaintext,
		Hash:      sum[:],
		UserID:    userID,
		SessionID: sessionID,
		ExpiresAt: now.Add(WSTicketTTL),
	}, nil
}

// HashWSTicket is the storage transform. Same reasoning as HashRefreshToken:
// high-entropy input, so a fast hash is the correct choice.
func HashWSTicket(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// TicketStore consumes tickets atomically.
//
// Redeem MUST be atomic — a read-then-delete has a window in which two
// concurrent handshakes both see the ticket and both succeed, which is exactly
// the single-use property FR-5 requires. ADR-011 names Redis GETDEL for this;
// the interface exists so the guarantee is stated at the boundary rather than
// assumed at the call site.
//
// A ticket is legitimately reconstructible state — losing the store logs
// people out of sockets they can immediately re-establish — so Redis is the
// right home for it under ADR-009, unlike idempotency records.
type TicketStore interface {
	Put(ctx context.Context, t *WSTicket) error
	// Redeem returns the ticket and deletes it in one atomic operation.
	Redeem(ctx context.Context, plaintext string, now time.Time) (*WSTicket, error)
}

// ValidateWSUpgrade enforces FR-5 on an incoming handshake.
//
// It refuses an access token in the query string rather than merely preferring
// the ticket. A token in a URL is written to the server's access log, every
// proxy log in between, and the browser's history — permanently, in plaintext,
// somewhere nobody is watching. Accepting it "just in case a client does it"
// is how that becomes permanent.
func ValidateWSUpgrade(r *http.Request) (ticket string, err error) {
	q := r.URL.Query()

	for _, banned := range []string{"access_token", "token", "jwt", "bearer", "authorization"} {
		if q.Has(banned) {
			return "", fmt.Errorf("%w: %q", ErrAccessTokenInQuery, banned)
		}
	}
	// A bearer token in the Authorization header of an upgrade request is not
	// forbidden by FR-5, but this endpoint does not accept one either: the
	// ticket is the only credential, so there is exactly one code path to audit.
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return "", ErrAccessTokenInQuery
	}

	ticket = q.Get("ticket")
	if ticket == "" {
		return "", ErrTicketNotFound
	}
	return ticket, nil
}

// TicketExpired reports whether a ticket is past its TTL.
func TicketExpired(t *WSTicket, now time.Time) bool {
	return t == nil || now.After(t.ExpiresAt)
}
