package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/internal/platform/db"
)

// The Postgres implementation of Repository, against migration 0002.
//
// Every query is written out rather than generated. The schema is small
// enough that the cost is one file, and the benefit is that the exact SQL
// executing in production is readable next to the rules it implements —
// including the two places where the SQL, not the Go, is what makes the
// behaviour correct:
//
//   - CreateUser is one transaction across users and user_credentials. A
//     partial write leaves an account that can never log in and whose handle
//     is permanently taken.
//   - MarkRotated is `UPDATE ... WHERE rotated_at IS NULL`, which is what
//     makes exactly one of two racing refreshes the winner. A read-then-write
//     here would fork the token family under load.

// PostgresRepository implements Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a Repository backed by pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// Constraint names from migration 0002. Named here so a unique violation maps
// to the right field — mapping every 23505 to "handle taken" would tell a user
// whose EMAIL collided to change the wrong thing.
const (
	constraintHandle = "users_handle_key"
	constraintEmail  = "user_credentials_email_key"
)

// CreateUser inserts the user and its credentials in one transaction.
func (r *PostgresRepository) CreateUser(ctx context.Context, u *User, email, passwordHash string) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO users (
				id, handle, display_name, avatar_url, locale, region,
				numeral_system, age_band, date_of_birth, entitlement_tier,
				is_discoverable, created_at, updated_at
			) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8::age_band,$9,$10::entitlement_tier,$11,$12,$12)`,
			u.ID, u.Handle, u.DisplayName, u.AvatarURL, u.Locale, u.Region,
			u.NumeralSystem, string(u.AgeBand), u.DateOfBirth, u.EntitlementTier,
			u.IsDiscoverable, u.CreatedAt,
		)
		if err != nil {
			// The service pre-checks both of these, but that check races: two
			// simultaneous signups for the same handle both pass it and only
			// the database can settle the outcome. Mapping the violation back
			// onto the same sentinel means the loser gets a 409 rather than a
			// 500.
			if db.IsUniqueViolation(err, constraintHandle) {
				return ErrHandleTaken
			}
			return fmt.Errorf("inserting user: %w", err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO user_credentials (user_id, email, password_hash, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$4)`,
			u.ID, email, passwordHash, u.CreatedAt,
		)
		if err != nil {
			if db.IsUniqueViolation(err, constraintEmail) {
				return ErrEmailTaken
			}
			return fmt.Errorf("inserting credentials: %w", err)
		}
		return nil
	})
}

const userColumns = `
	id, handle, display_name, COALESCE(avatar_url,''), locale, region,
	numeral_system, age_band::text, date_of_birth, entitlement_tier::text,
	is_discoverable, created_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	var band string
	err := row.Scan(&u.ID, &u.Handle, &u.DisplayName, &u.AvatarURL, &u.Locale,
		&u.Region, &u.NumeralSystem, &band, &u.DateOfBirth, &u.EntitlementTier,
		&u.IsDiscoverable, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.AgeBand = AgeBand(band)
	return &u, nil
}

// UserByEmail returns the user and credentials, or (nil, nil, nil) when absent.
func (r *PostgresRepository) UserByEmail(ctx context.Context, email string) (*User, *Credentials, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, c.email, c.password_hash
		FROM users u
		JOIN user_credentials c ON c.user_id = u.id
		WHERE c.email = $1 AND u.deleted_at IS NULL`, email)

	var u User
	var band string
	var creds Credentials
	err := row.Scan(&u.ID, &u.Handle, &u.DisplayName, &u.AvatarURL, &u.Locale,
		&u.Region, &u.NumeralSystem, &band, &u.DateOfBirth, &u.EntitlementTier,
		&u.IsDiscoverable, &u.CreatedAt, &creds.Email, &creds.PasswordHash)
	if db.IsNoRows(err) {
		// Not an error. "No such account" is an ordinary outcome, and the
		// service must handle it without distinguishing it to the caller.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("selecting user by email: %w", err)
	}
	u.AgeBand = AgeBand(band)
	creds.UserID = u.ID
	return &u, &creds, nil
}

// UserByID returns the user, or (nil, nil) when absent.
func (r *PostgresRepository) UserByID(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users u WHERE id = $1 AND deleted_at IS NULL`, id))
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("selecting user: %w", err)
	}
	return u, nil
}

// HandleTaken reports whether a handle is in use.
func (r *PostgresRepository) HandleTaken(ctx context.Context, handle string) (bool, error) {
	var exists bool
	// The column is citext, so the comparison is already case-insensitive; the
	// service lowercases anyway so the two layers cannot disagree.
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE handle = $1 AND deleted_at IS NULL)`,
		handle).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking handle: %w", err)
	}
	return exists, nil
}

// EmailTaken reports whether an email is registered.
func (r *PostgresRepository) EmailTaken(ctx context.Context, email string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_credentials WHERE email = $1)`, email).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking email: %w", err)
	}
	return exists, nil
}

// UpdatePasswordHash rewrites a stored hash.
func (r *PostgresRepository) UpdatePasswordHash(ctx context.Context, userID, hash string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE user_credentials SET password_hash = $2 WHERE user_id = $1`, userID, hash)
	if err != nil {
		return fmt.Errorf("updating password hash: %w", err)
	}
	return nil
}

// CreateSession inserts a session.
func (r *PostgresRepository) CreateSession(ctx context.Context, s *Session) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, device_name, platform, created_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$5)`,
		s.ID, s.UserID, s.DeviceName, s.Platform, s.CreatedAt)
	if err != nil {
		return fmt.Errorf("inserting session: %w", err)
	}
	return nil
}

// SessionByID returns a session, or (nil, nil) when absent.
func (r *PostgresRepository) SessionByID(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, device_name, platform, created_at, last_seen_at, revoked_at
		FROM sessions WHERE id = $1`, id).
		Scan(&s.ID, &s.UserID, &s.DeviceName, &s.Platform, &s.CreatedAt, &s.LastSeenAt, &s.RevokedAt)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("selecting session: %w", err)
	}
	return &s, nil
}

// RevokeSession revokes a session and every refresh family bound to it.
//
// Both halves in one transaction. Revoking the session alone would leave a
// live refresh token that outlives the logout it was supposed to end — the
// user sees "signed out" and the credential keeps renewing for 60 days.
func (r *PostgresRepository) RevokeSession(ctx context.Context, sessionID, reason string, at time.Time) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		// COALESCE keeps the FIRST revocation's timestamp, so a retry does not
		// move the clock forward. FR-5 requires logout to be idempotent, and
		// "idempotent" includes not rewriting the audit trail.
		if _, err := tx.Exec(ctx, `
			UPDATE sessions
			SET revoked_at = COALESCE(revoked_at, $2), revoked_reason = COALESCE(revoked_reason, $3)
			WHERE id = $1`, sessionID, at, reason); err != nil {
			return fmt.Errorf("revoking session: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE refresh_token_families
			SET revoked_at = $2, revoked_reason = $3
			WHERE session_id = $1 AND revoked_at IS NULL`,
			sessionID, at, familyReason(reason)); err != nil {
			return fmt.Errorf("revoking families: %w", err)
		}
		return nil
	})
}

// familyReason maps a session revocation reason onto the family CHECK
// constraint's vocabulary.
//
// The two columns have different allowed values, and inserting a session
// reason into the family column is a 23514 at runtime — a constraint violation
// that only fires on the logout path, which is exactly the kind of bug that
// reaches production.
func familyReason(reason string) string {
	switch reason {
	case "logout", "reuse_detected", "password_reset", "admin", "expired":
		return reason
	default:
		return "admin"
	}
}

// CreateFamily opens a refresh-token family.
func (r *PostgresRepository) CreateFamily(ctx context.Context, familyID, userID, sessionID string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO refresh_token_families (id, user_id, session_id, created_at)
		VALUES ($1,$2,$3,$4)`, familyID, userID, sessionID, at)
	if err != nil {
		return fmt.Errorf("inserting refresh family: %w", err)
	}
	return nil
}

// RevokeFamily revokes every token in a family.
func (r *PostgresRepository) RevokeFamily(ctx context.Context, familyID, reason string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE refresh_token_families
		SET revoked_at = COALESCE(revoked_at, $2), revoked_reason = COALESCE(revoked_reason, $3)
		WHERE id = $1`, familyID, at, familyReason(reason))
	if err != nil {
		return fmt.Errorf("revoking family: %w", err)
	}
	return nil
}

// InsertRefreshToken stores a new token.
func (r *PostgresRepository) InsertRefreshToken(ctx context.Context, id, familyID string, hash []byte, issuedAt, expiresAt time.Time) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO refresh_tokens (id, family_id, token_hash, issued_at, expires_at)
		VALUES ($1,$2,$3,$4,$5)`, id, familyID, hash, issuedAt, expiresAt)
	if err != nil {
		return fmt.Errorf("inserting refresh token: %w", err)
	}
	return nil
}

// RefreshTokenByHash returns a token's state, or (nil, nil) when unknown.
//
// The family's revocation is joined in rather than looked up separately: a
// second query would let the family be revoked between the two reads, which is
// precisely the window a token thief is racing for.
func (r *PostgresRepository) RefreshTokenByHash(ctx context.Context, hash []byte) (*RefreshTokenState, error) {
	var s RefreshTokenState
	var revokedWhy *string
	err := r.pool.QueryRow(ctx, `
		SELECT t.token_hash, t.family_id, f.session_id, f.user_id,
		       t.expires_at, t.rotated_at, t.valid_until_overlap,
		       f.revoked_at, f.revoked_reason
		FROM refresh_tokens t
		JOIN refresh_token_families f ON f.id = t.family_id
		WHERE t.token_hash = $1`, hash).
		Scan(&s.TokenHash, &s.FamilyID, &s.SessionID, &s.UserID,
			&s.ExpiresAt, &s.RotatedAt, &s.ValidUntilOverlap,
			&s.FamilyRevokedAt, &revokedWhy)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("selecting refresh token: %w", err)
	}
	if revokedWhy != nil {
		s.FamilyRevokedWhy = *revokedWhy
	}
	return &s, nil
}

// MarkRotated stamps rotation and reports whether it changed a row.
//
// `WHERE rotated_at IS NULL` is the entire concurrency contract. Two requests
// presenting the same token race here, exactly one UPDATE matches a row, and
// the loser sees rotated=false and falls back to the overlap-replay path
// rather than minting a second successor. A SELECT-then-UPDATE would let both
// win and fork the family, after which the next legitimate refresh looks like
// token reuse and revokes a real user's session.
func (r *PostgresRepository) MarkRotated(ctx context.Context, hash []byte, rotatedAt, validUntilOverlap time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET rotated_at = $2, valid_until_overlap = $3
		WHERE token_hash = $1 AND rotated_at IS NULL`, hash, rotatedAt, validUntilOverlap)
	if err != nil {
		return false, fmt.Errorf("rotating refresh token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EntitlementTier implements the rooms module's EntitlementLookup.
//
// It lives here so rooms never imports identity (§5.1): the dependency is one
// narrow interface, satisfied by a method that returns one string.
func (r *PostgresRepository) EntitlementTier(ctx context.Context, userID string) (string, error) {
	var tier string
	err := r.pool.QueryRow(ctx,
		`SELECT entitlement_tier::text FROM users WHERE id = $1 AND deleted_at IS NULL`,
		userID).Scan(&tier)
	if db.IsNoRows(err) {
		// A deleted or unknown user gets the free cap rather than an error.
		// The caller is about to fail an authorisation check anyway, and
		// failing closed is the right direction.
		return "free", nil
	}
	if err != nil {
		return "", fmt.Errorf("selecting entitlement: %w", err)
	}
	return tier, nil
}

var _ Repository = (*PostgresRepository)(nil)
