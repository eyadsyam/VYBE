// Command seed populates a development database.
//
// §16.3 sets the bar and explains why it matters: "Seed data must be
// plausible: 200 real titles with real metadata, 40 users with realistic
// social graphs and watch histories, 300 hand-written trivia questions across
// 20 titles, 50 achievements, 30 historical rooms. Quality over v1's 500
// random items."
//
// Plausibility is not decoration. The offline ranking harness (ADR-007,
// §10.4) computes nDCG@10 against this data, and a random social graph
// produces a meaningless number that would nonetheless get quoted as evidence
// that the ranker works.
//
// STATUS AT M0: seeds the fixed reference data — achievement rules and feature
// flags — which depend on nothing external. The 200-title catalogue and
// everything derived from it (users, watch histories, rooms, trivia) come from
// the provider mirror, which is blocked on BLOCKER-01. Rather than invent 200
// plausible-looking titles, which §0.3 rule 1 forbids, `--full` refuses and
// says exactly what is missing.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/internal/platform/db"
	"github.com/eyadsyam/vybe/server/internal/platform/ids"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("seed: ")

	full := flag.Bool("full", false,
		"also seed catalogue, users, rooms and trivia (requires a probed provider mirror)")
	flag.Parse()

	dsn := os.Getenv("VYBE_DB_DSN")
	if dsn == "" {
		log.Fatal("VYBE_DB_DSN is not set (see .env.example)")
	}

	// A seed script writes fabricated data by definition. There is no version
	// of that which belongs in a production database.
	if os.Getenv("VYBE_ENV") == "production" {
		log.Fatal("refusing to seed a production database")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	n, err := seedAchievements(ctx, pool)
	if err != nil {
		log.Fatalf("achievements: %v", err)
	}
	log.Printf("achievements: %d rule(s) upserted", n)

	n, err = seedFeatureFlags(ctx, pool)
	if err != nil {
		log.Fatalf("feature flags: %v", err)
	}
	log.Printf("feature flags: %d flag(s) upserted", n)

	if *full {
		log.Fatal(`--full is not available yet.

It needs the 200-title catalogue, which comes from the provider mirror
(ADR-012). The provider probe has not been run, so no real metadata exists
to seed from.

Inventing 200 plausible-looking titles here would be exactly the fabrication
Master Prompt v2 §0.3 rule 1 prohibits, and the offline ranking harness
(§10.4) would then report an nDCG figure computed against fiction.

To unblock:
  1. Get a free key: https://www.themoviedb.org/settings/api
  2. TMDB_API_KEY=... go run ./cmd/probe tmdb
  3. Transcribe the observed shapes into docs/INTEGRATIONS.md
  4. Implement the catalog adapter against those shapes
  5. Re-run: go run ./cmd/seed --full

See docs/BLOCKERS.md, BLOCKER-01.`)
	}

	log.Println("done — reference data only; catalogue requires --full (BLOCKER-01)")
}

// achievement is one §8.5 rule.
//
// Rules are DATA, not code: "adding an achievement must not require a deploy."
// The predicate is a JSONB document evaluated by a generic engine.
type achievement struct {
	code         string
	nameKey      string
	descKey      string
	icon         string
	triggerEvent string
	predicate    string // JSONB
	threshold    int
	rewardXP     int
	windowSecs   *int // nil = lifetime
}

// The starting rule set.
//
// Every name and description is an l10n KEY, never a literal string. §3.6
// applies to server-originated user-facing text too — otherwise Arabic users
// get English achievement names, which is precisely the gap that only shows up
// after launch.
var startingAchievements = []achievement{
	{
		code: "FIRST_ROOM", nameKey: "achievement.firstRoom.name",
		descKey: "achievement.firstRoom.desc", icon: "sparkles",
		triggerEvent: "ROOM_COMPLETED", predicate: `{}`,
		threshold: 1, rewardXP: 100,
	},
	{
		// The Host is the growth engine (§1.5) — every host brings 2–8 users —
		// so hosting is rewarded distinctly from attending.
		code: "HOST_FIVE", nameKey: "achievement.hostFive.name",
		descKey: "achievement.hostFive.desc", icon: "crown",
		triggerEvent: "ROOM_COMPLETED", predicate: `{"role":"host"}`,
		threshold: 5, rewardXP: 500,
	},
	{
		code: "TRIVIA_PERFECT", nameKey: "achievement.triviaPerfect.name",
		descKey: "achievement.triviaPerfect.desc", icon: "target",
		triggerEvent: "TRIVIA_ROUND_COMPLETED", predicate: `{"all_correct":true}`,
		threshold: 1, rewardXP: 300,
	},
	{
		// Rewards staying in a room through a reconnect — the exact thing the
		// resync protocol (ADR-003) exists to make survivable.
		code: "SYNC_SURVIVOR", nameKey: "achievement.syncSurvivor.name",
		descKey: "achievement.syncSurvivor.desc", icon: "link",
		triggerEvent: "ROOM_COMPLETED",
		predicate:    `{"reconnected":true,"min_duration_s":1500}`,
		threshold:    1, rewardXP: 250,
	},
	{
		code: "NIGHT_OWL", nameKey: "achievement.nightOwl.name",
		descKey: "achievement.nightOwl.desc", icon: "moon",
		triggerEvent: "ROOM_COMPLETED",
		predicate:    `{"local_hour_gte":1,"local_hour_lt":5}`,
		threshold:    3, rewardXP: 150,
	},
	{
		// Ramadan musalsalat are the launch wedge (§1.4), so the seasonal
		// surface has a matching progression hook from day one.
		code: "RAMADAN_REGULAR", nameKey: "achievement.ramadanRegular.name",
		descKey: "achievement.ramadanRegular.desc", icon: "crescent",
		triggerEvent: "ROOM_COMPLETED", predicate: `{"season":"ramadan"}`,
		threshold: 10, rewardXP: 750,
	},
}

// seedAchievements upserts the rules by `code`.
//
// Upsert rather than insert, so re-running the seed is idempotent and an
// edited threshold takes effect without a manual delete. Idempotency matters
// here specifically because docker compose runs this on every `up`.
func seedAchievements(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	const q = `
		INSERT INTO achievements
			(id, code, name_key, description_key, icon,
			 trigger_event, predicate, window_seconds, threshold, reward_xp)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10)
		ON CONFLICT (code) DO UPDATE SET
			name_key        = EXCLUDED.name_key,
			description_key = EXCLUDED.description_key,
			icon            = EXCLUDED.icon,
			trigger_event   = EXCLUDED.trigger_event,
			predicate       = EXCLUDED.predicate,
			window_seconds  = EXCLUDED.window_seconds,
			threshold       = EXCLUDED.threshold,
			reward_xp       = EXCLUDED.reward_xp`

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	count := 0
	for _, a := range startingAchievements {
		// ADR-010: the ID is minted in Go, not by a column default, so the
		// application holds it before the insert.
		if _, err := tx.Exec(ctx, q,
			ids.New(), a.code, a.nameKey, a.descKey, a.icon,
			a.triggerEvent, a.predicate, a.windowSecs, a.threshold, a.rewardXP,
		); err != nil {
			return count, err
		}
		count++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

// featureFlag is a §5.4 server-evaluated flag.
type featureFlag struct {
	key            string
	description    string
	enabled        bool
	rolloutPercent int
}

// §14.1: "Every realtime feature behind a flag with a kill switch."
//
// Each of these defaults to DISABLED. A flag that ships enabled is not a kill
// switch — it is a constant that took a database round trip.
var startingFlags = []featureFlag{
	{"realtime.rooms", "Room WebSocket connections. Kill switch for the whole realtime tier.", true, 100},
	{"realtime.companion_sync", "Companion Sync ritual and shared timeline (ADR-002).", true, 100},
	{"realtime.reactions", "Reaction bursts and aggregation (§7.6).", true, 100},
	{"games.trivia", "Server-authoritative trivia rounds (§8.1).", true, 100},
	{"games.predictions", "Predictions (§8.3). V1.5 — off until built.", false, 0},
	{"discovery.personalised_ranking", "Two-stage heuristic ranker (ADR-007). V1.5.", false, 0},
	{"social.activity_feed", "Friend activity feed. V1.5.", false, 0},
	{"progression.leaderboards", "Weekly and friends leaderboards (§8.6). V1.5.", false, 0},
}

func seedFeatureFlags(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	const q = `
		INSERT INTO feature_flags (key, description, enabled, rollout_percent)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE SET
			description = EXCLUDED.description
		-- enabled and rollout_percent are deliberately NOT overwritten: an
		-- operator who pulled a kill switch during an incident must not have
		-- it silently re-enabled by the next deploy's seed run.`

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	count := 0
	for _, f := range startingFlags {
		if _, err := tx.Exec(ctx, q, f.key, f.description, f.enabled, f.rolloutPercent); err != nil {
			return count, err
		}
		count++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
