# Architecture Decision Records

One file per decision. Format is fixed (Master Prompt v2 §16.2). A decision is
not "made" until it is here with a status of **Accepted**.

| ADR | Decision | Status |
|---|---|---|
| [001](ADR-001-state-management.md) | Riverpod for Flutter state management and DI | Accepted |
| [002](ADR-002-companion-sync.md) | Companion Sync (shared clock) over playback sync | Accepted |
| [003](ADR-003-event-log-resync.md) | Per-room monotonic event log with snapshot/delta resync | Accepted |
| [004](ADR-004-server-authoritative-games.md) | Server-authoritative game logic; the client has zero authority | Accepted |
| [005](ADR-005-modular-monolith.md) | Modular monolith in Go with CI-enforced boundaries | Accepted |
| [006](ADR-006-postgres-native-search.md) | Postgres-native search (tsvector + pg_trgm) with Arabic normalisation | Accepted |
| [007](ADR-007-heuristic-ranker.md) | Two-stage heuristic ranker behind a `Ranker` seam | Accepted |
| [008](ADR-008-offline-conflict.md) | Per-entity offline conflict resolution, not one global rule | Accepted |
| [009](ADR-009-redis-scope.md) | Redis holds only reconstructible ephemeral state | Accepted |
| [010](ADR-010-uuidv7.md) | UUIDv7 primary keys | Accepted |
| [011](ADR-011-auth-tokens.md) | Ed25519 JWT access + rotating opaque refresh with reuse detection | Accepted |
| [012](ADR-012-metadata-provider.md) | TMDB behind a `CatalogProvider` port, backend-proxied, locally cached | Accepted |
