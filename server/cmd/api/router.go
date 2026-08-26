package main

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/eyadsyam/vybe/server/internal/modules/identity"
	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms"
	"github.com/eyadsyam/vybe/server/internal/platform/config"
	"github.com/eyadsyam/vybe/server/internal/platform/httpx"
	"github.com/eyadsyam/vybe/server/internal/platform/passwords"
)

// The composition root (ADR-005).
//
// Every module is constructed here and nowhere else. That is the whole point
// of the modular monolith: the wiring is one readable file, so "what talks to
// what" is a question you answer by reading rather than by tracing imports
// through thirteen packages.
//
// Note what does NOT appear below: rooms never receives an *identity.Service,
// and realtime never receives either. They get narrow interfaces —
// EntitlementLookup, Actor, TicketRedeemer — satisfied by methods that return
// one or two values. §5.1's module boundary is enforceable by looking at these
// constructor arguments.

// modules holds the constructed application, so main can wire it and tests can
// build the same graph over fakes.
type modules struct {
	identity *identity.Handler
	rooms    *rooms.Handler
	realtime *realtime.Handler
	hub      *realtime.Hub
}

func buildModules(
	cfg *config.Config,
	pool *pgxpool.Pool,
	rdb *redis.Client,
	signingKey ed25519.PrivateKey,
	logger *slog.Logger,
) (*modules, error) {
	// --- identity -----------------------------------------------------------

	tokens, err := identity.NewTokenIssuer(signingKey, jwtIssuer, jwtAudience)
	if err != nil {
		return nil, err
	}

	breaches, err := identity.EmbeddedBreachSet()
	if err != nil {
		// Fail at boot rather than at first signup. PasswordPolicy fails
		// closed, so a broken breach set means every registration returns 503
		// — much better discovered here, where the operator is watching, than
		// by a user at 2am.
		return nil, err
	}
	logger.Info("breach set loaded", "entries", breaches.Size(),
		"note", "embedded fallback, not the full HIBP corpus — see breachlist.txt")

	identityRepo := identity.NewPostgresRepository(pool)
	tickets := identity.NewRedisTicketStore(rdb)
	identitySvc := identity.NewService(
		identityRepo, tokens,
		identity.PasswordPolicy{Breaches: breaches},
		passwords.DefaultParams, // the production cost, visible at the call site
	)
	identityHandler := identity.NewHandler(identitySvc, tickets)

	// --- rooms --------------------------------------------------------------

	roomsRepo := rooms.NewPostgresRepository(pool)
	hub := realtime.NewHub(logger)

	// identityRepo satisfies EntitlementLookup with one method. rooms does not
	// import identity; it imports an interface that happens to be satisfied
	// here.
	roomsSvc := rooms.NewService(roomsRepo, identityRepo)

	roomsHandler := rooms.NewHandler(roomsSvc, hub, actorFromContext, logger)

	// --- realtime -----------------------------------------------------------

	realtimeHandler := realtime.NewHandler(
		hub,
		ticketRedeemer{tickets},
		roomsRepo, // satisfies RoomReader
		cfg.ResyncDeltaThreshold,
		logger,
	)

	return &modules{
		identity: identityHandler,
		rooms:    roomsHandler,
		realtime: realtimeHandler,
		hub:      hub,
	}, nil
}

// jwtIssuer and jwtAudience bind a token to this deployment.
//
// Constants rather than configuration because changing either invalidates
// every live token: it is a deliberate protocol decision, not a knob.
const (
	jwtIssuer   = "https://vybe.app"
	jwtAudience = "vybe-mobile"
)

// actorFromContext bridges identity's claims to the rooms module.
//
// A function value rather than an import: rooms needs to know who is calling,
// not what an identity.Claims is.
func actorFromContext(ctx context.Context) (string, bool) {
	claims, ok := identity.ClaimsFromContext(ctx)
	if !ok || claims.Subject == "" {
		return "", false
	}
	return claims.Subject, true
}

// ticketRedeemer adapts the identity ticket store to realtime's interface.
type ticketRedeemer struct{ store *identity.RedisTicketStore }

func (t ticketRedeemer) Redeem(ctx context.Context, plaintext string, now time.Time) (string, string, error) {
	return t.store.RedeemForRealtime(ctx, plaintext, now)
}

// newRouter mounts the v1 API.
func newRouter(
	cfg *config.Config,
	pool poolPinger,
	rdb redisPinger,
	logger *slog.Logger,
	mods *modules,
	idemStore httpx.IdemStore,
) http.Handler {
	r := chi.NewRouter()

	// middleware.RealIP is deliberately NOT used. It is deprecated as
	// IP-spoofable (GHSA-3fxj-6jh8-hvhx and friends): it rewrites
	// r.RemoteAddr from the leftmost X-Forwarded-For, or from True-Client-IP
	// or X-Real-IP, whether or not our infrastructure actually sets them. Any
	// client can therefore choose its own apparent address.
	//
	// That matters here specifically because rate limiting keys on the client
	// address, and a spoofable key is not a rate limit -- it is a formality
	// that a single attacker bypasses by incrementing a header. When there IS
	// a trusted proxy in front of this, the right fix is a middleware that
	// trusts X-Forwarded-For only from that proxy's address, not one that
	// trusts it from everybody.
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	// Trace before everything that can produce a problem, so FR-58's traceId is
	// never missing from one.
	r.Use(httpx.Trace)
	r.Use(middleware.Timeout(30 * time.Second))

	mountHealth(r, pool, rdb)

	// The WebSocket endpoint sits OUTSIDE the timeout middleware's intent — a
	// socket is meant to be long-lived, and chi's Timeout wraps the
	// ResponseWriter in a way that a hijack must not inherit. Mounting it on
	// its own sub-router with no timeout keeps that explicit rather than
	// relying on the upgrade to work by accident.
	ws := chi.NewRouter()
	ws.Use(middleware.RequestID, httpx.Trace)
	ws.Handle("/", mods.realtime)
	r.Mount("/v1/ws", ws)

	r.Route("/v1", func(r chi.Router) {
		r.Mount("/auth", mods.identity.Routes())

		r.Group(func(r chi.Router) {
			r.Use(mods.identity.RequireAuth)
			// Idempotency runs INSIDE auth, because FR-57 scopes keys per
			// actor and the actor is not known until the token is verified.
			// Mounting it outside would let one user's key collide with
			// another's and replay somebody else's response.
			r.Use(httpx.Idempotency(idemStore))
			r.Mount("/rooms", mods.rooms.Routes())
		})
	})

	// Method-not-allowed and not-found as RFC 9457, so a client never has to
	// parse chi's plain-text default.
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.ErrMethodNotAllowed.
			WithDetail("%s is not supported for this resource.", r.Method))
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, httpx.ErrNotFound.
			WithDetail("No route for %s %s.", r.Method, r.URL.Path))
	})

	return r
}

func mountHealth(r chi.Router, pool poolPinger, rdb redisPinger) {
	// Liveness: is this process alive? Deliberately does NOT touch the
	// database. A liveness probe that fails on a database blip gets the
	// container killed and restarted, which does not fix the database and does
	// lose every WebSocket it was holding.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// Readiness: should this instance receive traffic? This one DOES check
	// dependencies, because an instance that cannot reach Postgres should be
	// taken out of the load balancer without being killed.
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()

		checks := map[string]string{}
		ready := true

		if err := pool.Ping(ctx); err != nil {
			checks["postgres"] = "unavailable"
			ready = false // the source of truth; without it we serve nothing
		} else {
			checks["postgres"] = "ok"
		}

		if err := rdb.Ping(ctx).Err(); err != nil {
			// ADR-009: Redis holds only reconstructible state, so this is
			// DEGRADED, not unready. Refusing traffic here would turn a cache
			// outage into a total one.
			checks["redis"] = "unavailable (degraded, not fatal — ADR-009)"
		} else {
			checks["redis"] = "ok"
		}

		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
	})
}
