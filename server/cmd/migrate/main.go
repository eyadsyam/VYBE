// Command migrate applies, rolls back, or reports the schema.
//
//	migrate up       apply every pending migration
//	migrate down     roll back exactly one migration
//	migrate status   show applied and pending
//
// Run by docker compose before the API starts, and by CI against a scratch
// database to prove the migrations actually execute.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eyadsyam/vybe/server/internal/platform/db"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("migrate: ")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]

	dsn := os.Getenv("VYBE_DB_DSN")
	if dsn == "" {
		log.Fatal("VYBE_DB_DSN is not set (see .env.example)")
	}

	// Interruptible: a migration that is taking too long should be stoppable
	// without a kill -9, and the per-migration transaction means an
	// interrupted run rolls back cleanly rather than half-applying.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	logf := func(format string, args ...any) { log.Printf(format, args...) }

	switch command {
	case "up":
		err = db.MigrateUp(ctx, pool, logf)
	case "down":
		// Guard rail: rolling back in production is almost always the wrong
		// move (§5.4 prefers expand -> migrate -> contract), so it must be
		// asked for twice.
		if os.Getenv("VYBE_ENV") == "production" && os.Getenv("VYBE_CONFIRM_DOWN") != "yes" {
			log.Fatal("refusing to roll back in production without VYBE_CONFIRM_DOWN=yes")
		}
		err = db.MigrateDown(ctx, pool, logf)
	case "status":
		err = db.Status(ctx, pool, logf)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		log.Fatalf("%v", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: migrate <up|down|status>

  up      apply every pending migration, each in its own transaction
  down    roll back exactly one migration (never in bulk — see §5.4)
  status  list applied and pending migrations

Environment:
  VYBE_DB_DSN   required
  VYBE_ENV      when "production", `+"`down`"+` also requires VYBE_CONFIRM_DOWN=yes
`)
}
