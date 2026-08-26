// Command api is the VYBE modular monolith (ADR-005).
//
// At M0 it boots, validates configuration, connects to Postgres and Redis, and
// serves health and readiness. The thirteen module facades mount here in M1.
//
// It does NOT serve stub endpoints that return plausible-looking data.
// §0.3 rule 2 forbids real-looking surfaces over absent functionality, so an
// unimplemented route is absent, not fake.
//
//	api                 run the server
//	api -healthcheck    probe a running instance (used by Docker HEALTHCHECK)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/eyadsyam/vybe/server/internal/platform/config"
	"github.com/eyadsyam/vybe/server/internal/platform/db"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe a running instance and exit")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// Configuration errors are joined, so this prints every problem at
		// once rather than making the operator fix them one restart at a time.
		return fmt.Errorf("configuration:\n%w", err)
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	// Safe because String() redacts every secret, and a test asserts it
	// (§12.6). Logging the effective config is worth a great deal when
	// diagnosing "it works locally".
	logger.Info("starting", "config", cfg.String())

	signingKey, err := resolveSigningKey(cfg, logger)
	if err != nil {
		return err
	}

	// Signals first, so a Ctrl-C during a slow database connect still exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := db.Connect(connectCtx, cfg.DatabaseDSN)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres connected")

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer func() { _ = rdb.Close() }()

	// ADR-009: Redis holds only reconstructible state, so an unavailable Redis
	// is a DEGRADED start, not a failed one. Refusing to boot here would turn
	// a cache outage into a total outage and contradict the chaos test that
	// says rooms survive Redis dying.
	if err := rdb.Ping(connectCtx).Err(); err != nil {
		logger.Warn("redis unavailable at boot; starting degraded",
			"addr", cfg.RedisAddr, "err", err,
			"impact", "presence unknown, rate limits fail open on reads (ADR-009)")
	} else {
		logger.Info("redis connected")
	}

	mods, err := buildModules(cfg, pool, rdb, signingKey, logger)
	if err != nil {
		return fmt.Errorf("wiring modules: %w", err)
	}

	// Idempotency records live in Postgres, not Redis. ADR-009 puts only
	// RECONSTRUCTIBLE state in Redis, and an idempotency record is the exact
	// opposite: it is the sole evidence a request already happened, so losing
	// it duplicates work with nothing anywhere able to detect that.
	idemStore := httpx.NewPostgresIdemStore(pool)

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: newRouter(cfg, pool, rdb, logger, mods, idemStore),

		// Without these a slow-loris client holds a connection open forever.
		// ReadHeaderTimeout in particular is the one that actually stops it.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr, "env", string(cfg.Env))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Graceful shutdown. 20s is chosen to sit inside a typical orchestrator's
	// 30s termination grace, so in-flight requests finish rather than being
	// severed — and, once the realtime tier exists, so sockets get a close
	// frame and clients reconnect deliberately instead of timing out.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()

	// Close the sockets BEFORE Shutdown. http.Server.Shutdown does not touch
	// hijacked connections, so without this it waits the full 20 seconds for
	// WebSockets that will never close on their own, and every client then
	// sees an abrupt 1006 instead of a close frame explaining the restart.
	mods.hub.CloseAll("the server is restarting; reconnect and resync")

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	logger.Info("stopped cleanly")
	return nil
}

// poolPinger is the slice of *pgxpool.Pool the router actually needs, so the
// router stays testable without a live database.
type poolPinger interface {
	Ping(ctx context.Context) error
}

// redisPinger is the same seam for Redis.
//
// It exists for the same reason poolPinger does, and specifically so the
// DEGRADED path is testable: ADR-009 says an unavailable Redis must keep the
// instance ready, and asserting that without a way to make Redis unavailable
// means never asserting it at all. Returning *redis.StatusCmd rather than an
// error keeps *redis.Client satisfying this without an adapter.
type redisPinger interface {
	Ping(ctx context.Context) *redis.StatusCmd
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	// RFC 9457 (§5.2) for problems; plain JSON for the health endpoints, which
	// are not part of the versioned API surface.
	if status >= 400 {
		w.Header().Set("Content-Type", "application/problem+json")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	// §14.2: structured JSON logs everywhere a machine reads them. Local gets
	// text because a human reads them there.
	if cfg.Env == config.EnvLocal {
		return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// resolveSigningKey returns the configured Ed25519 key, or generates an
// ephemeral one in local/dev.
//
// config.Load has already refused an absent key outside local/dev, so the
// generated branch is unreachable in a deployed environment. It still logs a
// warning, because a developer should see that restarting invalidates their
// session rather than being confused by it.
func resolveSigningKey(cfg *config.Config, logger *slog.Logger) (ed25519.PrivateKey, error) {
	if !cfg.HasEphemeralSigningKey() {
		switch len(cfg.JWTPrivateKey) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(cfg.JWTPrivateKey), nil
		case ed25519.PrivateKeySize:
			return ed25519.PrivateKey(cfg.JWTPrivateKey), nil
		default:
			return nil, fmt.Errorf("signing key is %d bytes; want %d or %d",
				len(cfg.JWTPrivateKey), ed25519.SeedSize, ed25519.PrivateKeySize)
		}
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral signing key: %w", err)
	}
	logger.Warn("generated an EPHEMERAL signing key",
		"consequence", "every session is invalidated when this process restarts",
		"fix", "set VYBE_JWT_PRIVATE_KEY_B64 (see .env.example)")
	return priv, nil
}

// runHealthcheck is the Docker HEALTHCHECK entry point. Using the binary
// itself means the image needs no curl, which keeps the attack surface and
// the image size down.
func runHealthcheck() int {
	addr := os.Getenv("VYBE_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad VYBE_HTTP_ADDR %q: %v\n", addr, err)
		return 2
	}
	if host == "" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
