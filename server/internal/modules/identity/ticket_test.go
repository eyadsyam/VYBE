package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewWSTicketShapeAndTTL(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	ticket, err := NewWSTicket("user-1", "session-1", now)
	if err != nil {
		t.Fatalf("NewWSTicket: %v", err)
	}
	if ticket.UserID != "user-1" || ticket.SessionID != "session-1" {
		t.Errorf("ticket = %+v", ticket)
	}
	if !ticket.ExpiresAt.Equal(now.Add(60 * time.Second)) {
		t.Errorf("ExpiresAt = %v, want now+60s (FR-5)", ticket.ExpiresAt)
	}
	if len(ticket.Plaintext) != 43 {
		t.Errorf("plaintext length = %d, want 43 (32 bytes base64url)", len(ticket.Plaintext))
	}
	if len(ticket.Hash) != 32 {
		t.Errorf("hash length = %d, want 32", len(ticket.Hash))
	}
	if string(ticket.Hash) == ticket.Plaintext {
		t.Error("the hash equals the plaintext")
	}
	if !ConstantTimeHashEqual(HashWSTicket(ticket.Plaintext), ticket.Hash) {
		t.Error("HashWSTicket does not reproduce the ticket's stored hash")
	}
}

func TestWSTicketTTLIsSixtySeconds(t *testing.T) {
	if WSTicketTTL != 60*time.Second {
		t.Errorf("WSTicketTTL = %v, want 60s (FR-5, ADR-011)", WSTicketTTL)
	}
}

func TestWSTicketsAreUnique(t *testing.T) {
	now := time.Now()
	seen := map[string]bool{}
	for range 300 {
		ticket, err := NewWSTicket("u", "s", now)
		if err != nil {
			t.Fatalf("NewWSTicket: %v", err)
		}
		if seen[ticket.Plaintext] {
			t.Fatal("duplicate ticket generated")
		}
		seen[ticket.Plaintext] = true
	}
}

func TestTicketExpired(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	ticket, err := NewWSTicket("u", "s", now)
	if err != nil {
		t.Fatalf("NewWSTicket: %v", err)
	}

	if TicketExpired(ticket, now) {
		t.Error("a fresh ticket is expired")
	}
	if TicketExpired(ticket, now.Add(WSTicketTTL)) {
		t.Error("a ticket expired exactly at its deadline; the boundary should still be valid")
	}
	if !TicketExpired(ticket, now.Add(WSTicketTTL+time.Nanosecond)) {
		t.Error("a ticket past its deadline was not expired")
	}
	if !TicketExpired(nil, now) {
		t.Error("a nil ticket must count as expired, not as valid")
	}
}

func TestValidateWSUpgradeRefusesTokensInTheQueryString(t *testing.T) {
	// FR-5: the system MUST NOT accept an access token as a WebSocket query
	// parameter. A token in a URL is written to the server's access log, every
	// proxy log in between, and browser history — permanently, in plaintext.
	for _, param := range []string{"access_token", "token", "jwt", "bearer", "authorization"} {
		t.Run(param, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/ws?ticket=abc&"+param+"=eyJhbGciOi", nil)
			if _, err := ValidateWSUpgrade(r); !errors.Is(err, ErrAccessTokenInQuery) {
				t.Errorf("err = %v, want ErrAccessTokenInQuery", err)
			}
		})
	}
}

func TestValidateWSUpgradeRefusesABearerHeader(t *testing.T) {
	// Not forbidden by FR-5, but this endpoint accepts exactly one credential
	// so there is exactly one code path to audit.
	for _, header := range []string{"Bearer eyJhbGciOi", "bearer eyJhbGciOi", "BEARER eyJhbGciOi"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/ws?ticket=abc", nil)
		r.Header.Set("Authorization", header)
		if _, err := ValidateWSUpgrade(r); !errors.Is(err, ErrAccessTokenInQuery) {
			t.Errorf("Authorization %q: err = %v, want ErrAccessTokenInQuery", header, err)
		}
	}
}

func TestValidateWSUpgradeAcceptsATicket(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/ws?ticket=abc123", nil)
	got, err := ValidateWSUpgrade(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc123" {
		t.Errorf("ticket = %q, want abc123", got)
	}
}

func TestValidateWSUpgradeRequiresATicket(t *testing.T) {
	for _, url := range []string{"/v1/ws", "/v1/ws?ticket="} {
		r := httptest.NewRequest(http.MethodGet, url, nil)
		if _, err := ValidateWSUpgrade(r); !errors.Is(err, ErrTicketNotFound) {
			t.Errorf("%s: err = %v, want ErrTicketNotFound", url, err)
		}
	}
}

func TestRedeemedAndNeverIssuedAreIndistinguishable(t *testing.T) {
	// Telling a caller "that ticket existed but was used" confirms a valid
	// ticket to somebody who guessed one. Both cases are the same error value.
	r := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	_, missing := ValidateWSUpgrade(r)

	if !errors.Is(missing, ErrTicketNotFound) {
		t.Fatalf("err = %v", missing)
	}
	if got := ErrTicketNotFound.Error(); got != "identity: websocket ticket is not valid" {
		t.Errorf("message = %q; it must not distinguish 'used' from 'never existed'", got)
	}
}
