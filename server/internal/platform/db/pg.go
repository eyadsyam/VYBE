package db

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Shared helpers for the Postgres repositories.

// Querier is the subset of pgxpool.Pool and pgx.Tx that repositories use.
//
// Having both satisfy one interface is what lets a repository method run
// either standalone or inside a caller's transaction without a second
// implementation. The alternative — a `WithTx` variant of every method — is
// where the atomicity bugs live, because the two copies drift.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
//
// The panic case is the one that is easy to omit and expensive to omit: a
// panic mid-transaction without this leaves the connection holding an open
// transaction, which the pool then hands to the next request. That request
// fails with a bewildering error about the transaction being aborted, far from
// the code that caused it.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p) // re-panic after cleaning up, so the stack is preserved
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Postgres error codes worth naming. Comparing against these rather than
// matching on message text is the difference between a check that survives a
// Postgres upgrade and one that does not.
const (
	// CodeUniqueViolation — 23505. A duplicate handle, email, or join code.
	CodeUniqueViolation = "23505"
	// CodeForeignKeyViolation — 23503. A referenced row does not exist.
	CodeForeignKeyViolation = "23503"
	// CodeCheckViolation — 23514. A CHECK constraint refused the row.
	CodeCheckViolation = "23514"
	// CodeSerializationFailure — 40001. Retryable at the caller's discretion.
	CodeSerializationFailure = "40001"
	// CodeDeadlockDetected — 40P01. Also retryable.
	CodeDeadlockDetected = "40P01"
)

// IsUniqueViolation reports whether err is a 23505, optionally on a specific
// constraint.
//
// The constraint name matters: a signup can violate the users_handle_key OR
// the user_credentials_email_key, and mapping both to "handle taken" tells the
// user to change the wrong field.
func IsUniqueViolation(err error, constraint ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != CodeUniqueViolation {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	return slices.Contains(constraint, pgErr.ConstraintName)
}

// IsForeignKeyViolation reports whether err is a 23503.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == CodeForeignKeyViolation
}

// IsRetryable reports whether err is a serialization failure or deadlock.
//
// Both mean "this transaction lost a race and would succeed if run again", as
// opposed to every other error, which means "this will fail the same way
// forever". Retrying the second kind is how a transient blip becomes a
// hammering loop.
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == CodeSerializationFailure || pgErr.Code == CodeDeadlockDetected
}

// IsNoRows reports whether a query returned nothing.
//
// Wrapped rather than exposing pgx.ErrNoRows to callers, so a repository can
// translate "no rows" into its own domain answer — usually (nil, nil) rather
// than an error, because "that user does not exist" is an ordinary outcome and
// not a failure.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
