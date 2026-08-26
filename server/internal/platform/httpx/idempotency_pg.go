package httpx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Postgres IdemStore, against migration 0007.
//
// Postgres and not Redis, deliberately, and this is the one place ADR-009's
// rule bites hardest. Redis holds only RECONSTRUCTIBLE state; an idempotency
// record is the opposite of reconstructible — it is the sole evidence that a
// request already happened. Losing it means a retried room creation makes a
// second room, and there is nothing anywhere to detect that from.
//
// The whole contract rests on one statement doing insert-or-fetch atomically.
// A read-then-write has a race between the two, which is precisely the
// concurrent-retry case this exists to guard.

// PostgresIdemStore implements IdemStore.
type PostgresIdemStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresIdemStore returns an IdemStore backed by pool.
func NewPostgresIdemStore(pool *pgxpool.Pool) *PostgresIdemStore {
	return &PostgresIdemStore{pool: pool, now: time.Now}
}

// SetClock replaces the time source. Tests only.
func (s *PostgresIdemStore) SetClock(now func() time.Time) { s.now = now }

// Reserve atomically inserts an in_flight record, or returns the existing one.
//
// One statement, not two. The CTE inserts and, on conflict, does nothing; the
// outer SELECT then reads whichever row is now there — ours or the winner's.
// Splitting this into "SELECT, then INSERT if absent" opens a window in which
// two concurrent retries both see nothing and both proceed, which is the exact
// double-charge FR-57 exists to prevent.
//
// The `expires_at <= now()` clause lets an expired record be overwritten
// rather than blocking the key forever. §5.2's 24-hour window is the point
// after which a replay is no longer owed.
func (s *PostgresIdemStore) Reserve(
	ctx context.Context, actorID, key, endpoint string, fingerprint []byte, ttl time.Duration,
) (*IdemRecord, error) {
	now := s.now()

	rows, err := s.pool.Query(ctx, `
		WITH attempted AS (
			INSERT INTO idempotency_keys
				(user_id, key, request_fingerprint, endpoint, status, created_at, expires_at)
			VALUES ($1,$2,$3,$4,'in_flight',$5,$6)
			ON CONFLICT (user_id, key) DO UPDATE
				SET request_fingerprint = EXCLUDED.request_fingerprint,
				    endpoint            = EXCLUDED.endpoint,
				    status              = 'in_flight',
				    response_status     = NULL,
				    response_body       = NULL,
				    completed_at        = NULL,
				    created_at          = EXCLUDED.created_at,
				    expires_at          = EXCLUDED.expires_at
				WHERE idempotency_keys.expires_at <= $5
			RETURNING 1
		)
		SELECT status, request_fingerprint, COALESCE(response_status,0), COALESCE(response_body,'null'::jsonb),
		       (SELECT count(*) FROM attempted) AS claimed
		FROM idempotency_keys WHERE user_id = $1 AND key = $2`,
		actorID, key, fingerprint, endpoint, now, now.Add(ttl))
	if err != nil {
		return nil, fmt.Errorf("reserving idempotency key: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("reserving idempotency key: %w", err)
		}
		// Unreachable: the INSERT either created the row or a conflicting one
		// exists, so the SELECT always finds one.
		return nil, errors.New("httpx: idempotency reservation vanished")
	}

	var status string
	var storedFingerprint, body []byte
	var responseStatus, claimed int
	if err := rows.Scan(&status, &storedFingerprint, &responseStatus, &body, &claimed); err != nil {
		return nil, fmt.Errorf("scanning idempotency record: %w", err)
	}

	if claimed > 0 {
		// We inserted (or reclaimed an expired row). No prior record to report.
		return nil, nil
	}

	return &IdemRecord{
		Status:         IdemStatus(status),
		Fingerprint:    storedFingerprint,
		ResponseStatus: responseStatus,
		ResponseBody:   normaliseBody(body),
	}, nil
}

// normaliseBody turns the `'null'::jsonb` placeholder back into no body.
//
// The COALESCE in the query exists so the scan target is never nil, but a
// literal four-byte "null" replayed as a response body is not the same thing
// as an absent one — a client would decode it and get a null object.
func normaliseBody(b []byte) []byte {
	if len(b) == 4 && string(b) == "null" {
		return nil
	}
	return b
}

// Complete stores the terminal response for replay.
func (s *PostgresIdemStore) Complete(ctx context.Context, actorID, key string, status int, body []byte) error {
	if len(body) == 0 {
		// jsonb rejects an empty string. A 204 legitimately has no body, so
		// store SQL NULL and let the replay send nothing.
		body = nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_status = $3, response_body = $4::jsonb, completed_at = $5
		WHERE user_id = $1 AND key = $2`,
		actorID, key, status, body, s.now())
	if err != nil {
		return fmt.Errorf("completing idempotency key: %w", err)
	}
	return nil
}

// Release removes a reservation so the request can be retried.
//
// Called when the handler produced a 5xx, which is by definition not a
// terminal answer. Leaving the reservation in place would make a transient
// database blip permanently poison that key: every retry would see an
// in_flight record and get a 409 forever.
func (s *PostgresIdemStore) Release(ctx context.Context, actorID, key string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2 AND status = 'in_flight'`,
		actorID, key)
	if err != nil {
		return fmt.Errorf("releasing idempotency key: %w", err)
	}
	return nil
}

// PurgeExpired deletes records past their window, for a periodic job.
//
// Returns how many rows went, so the operator's log says whether the job is
// keeping up. A silently-growing idempotency table is a slow leak that only
// shows up as query latency months later.
func (s *PostgresIdemStore) PurgeExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= $1`, s.now())
	if err != nil {
		return 0, fmt.Errorf("purging idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

var (
	_ IdemStore = (*PostgresIdemStore)(nil)
	_ pgx.Rows  = (pgx.Rows)(nil)
)
