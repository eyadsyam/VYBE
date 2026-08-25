// Package db owns the Postgres connection and schema migration.
//
// The migrator is written here rather than pulled in as a dependency for two
// reasons. It is about 150 lines, so the dependency would not be paying for
// itself; and §5.4 requires specific behaviour — forward-only, one transaction
// per migration, an advisory lock so two instances starting together cannot
// race — which is easier to guarantee in code we own than to verify in code we
// do not.
package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/migrations"
)

// migrationLockID is an arbitrary but fixed key for pg_advisory_lock. Two API
// instances booting simultaneously must not both try to apply migration 0004;
// the second blocks here and then finds nothing to do.
const migrationLockID int64 = 0x5659_4245 // "VYBE"

// Migration is one versioned schema change.
type Migration struct {
	Version int
	Name    string
	UpSQL   string
	DownSQL string

	// Checksum of UpSQL. Recorded when applied and re-verified on every
	// subsequent boot: editing a migration that has already run in production
	// is a silent divergence between what the database contains and what the
	// repository claims, and it is far better to refuse to start.
	Checksum string
}

// LoadMigrations reads and pairs the embedded .up.sql / .down.sql files.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("globbing migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("db: no migrations embedded — check the go:embed path")
	}

	out := make([]Migration, 0, len(entries))
	for _, upPath := range entries {
		fileName := upPath[strings.LastIndex(upPath, "/")+1:]
		version, name, err := parseMigrationName(fileName)
		if err != nil {
			return nil, err
		}

		upSQL, err := fs.ReadFile(migrations.FS, upPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", upPath, err)
		}

		downPath := strings.TrimSuffix(upPath, ".up.sql") + ".down.sql"
		downSQL, err := fs.ReadFile(migrations.FS, downPath)
		if err != nil {
			// §5.4: "reversible where possible". A missing down migration is a
			// deliberate choice that must be made deliberately, so we refuse
			// rather than silently accepting an irreversible change.
			return nil, fmt.Errorf(
				"migration %04d_%s has no .down.sql; every migration must be reversible "+
					"or explicitly documented as not (see §5.4): %w", version, name, err)
		}

		sum := sha256.Sum256(upSQL)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			UpSQL:    string(upSQL),
			DownSQL:  string(downSQL),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	// Duplicate or missing versions mean two people numbered a migration the
	// same, which produces a different schema depending on filesystem order.
	for i, m := range out {
		if want := i + 1; m.Version != want {
			return nil, fmt.Errorf(
				"migration versions must be contiguous from 1: expected %04d, found %04d (%s)",
				want, m.Version, m.Name)
		}
	}
	return out, nil
}

func parseMigrationName(fileName string) (int, string, error) {
	base := strings.TrimSuffix(fileName, ".up.sql")
	idx := strings.Index(base, "_")
	if idx <= 0 {
		return 0, "", fmt.Errorf("db: migration %q must be named NNNN_name.up.sql", fileName)
	}
	version, err := strconv.Atoi(base[:idx])
	if err != nil {
		return 0, "", fmt.Errorf("db: migration %q has a non-numeric version: %w", fileName, err)
	}
	return version, base[idx+1:], nil
}

// EnsureMigrationTable creates the bookkeeping table. Deliberately not itself a
// migration — it is the thing that records migrations.
func EnsureMigrationTable(ctx context.Context, conn *pgxpool.Pool) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     int PRIMARY KEY,
			name        text NOT NULL,
			checksum    text NOT NULL,
			applied_at  timestamptz(3) NOT NULL DEFAULT now(),
			duration_ms int NOT NULL
		)`)
	return err
}

// AppliedMigration is a row of schema_migrations.
type AppliedMigration struct {
	Version  int
	Name     string
	Checksum string
}

// Applied returns what the database believes it has run, keyed by version.
func Applied(ctx context.Context, conn *pgxpool.Pool) (map[int]AppliedMigration, error) {
	rows, err := conn.Query(ctx,
		`SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int]AppliedMigration)
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			return nil, err
		}
		out[a.Version] = a
	}
	return out, rows.Err()
}

// VerifyChecksums refuses to proceed when a previously-applied migration's file
// has changed.
//
// This is the check that catches the genuinely dangerous mistake: editing an
// old migration so the repository and the live schema quietly disagree, and
// every fresh environment then gets a different database from production.
func VerifyChecksums(list []Migration, applied map[int]AppliedMigration) error {
	for _, m := range list {
		a, ok := applied[m.Version]
		if !ok {
			continue
		}
		if a.Checksum != m.Checksum {
			return fmt.Errorf(
				"migration %04d_%s has changed since it was applied\n"+
					"  applied checksum: %s\n"+
					"  current checksum: %s\n"+
					"Editing an applied migration makes the repository and the live schema diverge.\n"+
					"Write a NEW migration instead (§5.4: expand -> migrate -> contract).",
				m.Version, m.Name, a.Checksum, m.Checksum)
		}
	}
	return nil
}

// MigrateUp applies every pending migration in order.
//
// Each migration runs in its OWN transaction. A single transaction around all
// of them would be tidier to reason about but would roll the whole set back on
// one failure, so a partial success could never be inspected — and some DDL
// (CREATE INDEX CONCURRENTLY, later) cannot run inside a transaction at all.
func MigrateUp(ctx context.Context, pool *pgxpool.Pool, logf func(string, ...any)) error {
	loaded, err := LoadMigrations()
	if err != nil {
		return err
	}

	// Serialise concurrent starters. Released when this connection returns to
	// the pool at the end of the function.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquiring migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if err := EnsureMigrationTable(ctx, pool); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied, err := Applied(ctx, pool)
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}
	if err := VerifyChecksums(loaded, applied); err != nil {
		return err
	}

	pending := 0
	for _, m := range loaded {
		if _, done := applied[m.Version]; done {
			continue
		}
		pending++

		start := nowMillis()
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("beginning tx for %04d_%s: %w", m.Version, m.Name, err)
		}

		if _, err := tx.Exec(ctx, m.UpSQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("applying %04d_%s: %w", m.Version, m.Name, err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, duration_ms)
			 VALUES ($1, $2, $3, $4)`,
			m.Version, m.Name, m.Checksum, nowMillis()-start,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("recording %04d_%s: %w", m.Version, m.Name, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing %04d_%s: %w", m.Version, m.Name, err)
		}
		logf("applied %04d_%s (%dms)", m.Version, m.Name, nowMillis()-start)
	}

	if pending == 0 {
		logf("schema up to date at version %04d", loaded[len(loaded)-1].Version)
	}
	return nil
}

// MigrateDown reverses the single most recent migration.
//
// One at a time, and never in production. Bulk rollback is how you discover
// that migration 0003's down script was never tested.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, logf func(string, ...any)) error {
	loaded, err := LoadMigrations()
	if err != nil {
		return err
	}
	applied, err := Applied(ctx, pool)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		logf("nothing to roll back")
		return nil
	}

	latest := 0
	for v := range applied {
		if v > latest {
			latest = v
		}
	}

	var target Migration
	for _, m := range loaded {
		if m.Version == latest {
			target = m
			break
		}
	}
	if target.Version == 0 {
		return fmt.Errorf("db: version %04d is recorded as applied but is not in the repository", latest)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, target.DownSQL); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("rolling back %04d_%s: %w", target.Version, target.Name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, target.Version); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	logf("rolled back %04d_%s", target.Version, target.Name)
	return nil
}

// Status prints what has been applied and what is pending.
func Status(ctx context.Context, pool *pgxpool.Pool, logf func(string, ...any)) error {
	loaded, err := LoadMigrations()
	if err != nil {
		return err
	}
	if err := EnsureMigrationTable(ctx, pool); err != nil {
		return err
	}
	applied, err := Applied(ctx, pool)
	if err != nil {
		return err
	}

	for _, m := range loaded {
		mark := "pending"
		if a, ok := applied[m.Version]; ok {
			mark = "applied"
			if a.Checksum != m.Checksum {
				mark = "APPLIED BUT MODIFIED"
			}
		}
		logf("%04d  %-28s  %s", m.Version, m.Name, mark)
	}
	return nil
}

// Connect opens a pool and verifies it before returning.
//
// Pinging here rather than lazily means a bad DSN fails at boot with a clear
// message, instead of surfacing as a confusing error on the first request.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing VYBE_DB_DSN: %w", err)
	}

	// A modest ceiling: §13.4 alerts on the pool exceeding 80%, and an
	// unbounded pool would simply move the contention into Postgres.
	cfg.MaxConns = 25
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return pool, nil
}

// ErrNoRows re-exports pgx's sentinel so modules do not import pgx directly
// just to check for a missing row (ADR-005 boundary hygiene).
var ErrNoRows = pgx.ErrNoRows

// nowMillis is wall-clock milliseconds, used only to record how long a
// migration took. Not a monotonic source and not used for anything that
// matters — §8.1 timing goes nowhere near this.
func nowMillis() int64 { return time.Now().UnixMilli() }
