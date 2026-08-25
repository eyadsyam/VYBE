# ADR-009: Redis holds only reconstructible, ephemeral state

Status: Accepted
Date: 2026-08-25

## Context

§14.3 makes a claim that must be true or the backup strategy is wrong: *"Redis:
no backup needed by design — it holds only reconstructible ephemeral state."*
And then, crucially: *"Verify this claim by killing Redis in staging and
confirming rooms survive."*

A claim like that decays the moment someone adds one convenient cache of
something that matters. The decision here is the boundary, plus the mechanism
that keeps it honest.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| No Redis; Postgres for everything | One datastore, trivially correct | Presence heartbeats every 15s x 3,000 users is ~200 writes/sec of pure churn against a durable store; rate limiting becomes a hot-row contention problem |
| **Redis for ephemeral only** | Right tool for TTL, counters, and pub/sub; Postgres stays the source of truth | Two stores to run; a real discipline problem to keep the boundary |
| Redis as a general cache, including durable-ish data | Fast everything | The §14.3 claim becomes false, and "Redis died" turns into an incident instead of a degradation |
| Redis as the room event log | Very fast fan-out | Rooms would not survive a Redis restart, contradicting ADR-003 |

## Decision

**Redis stores only state that can be rebuilt from Postgres or from client
reconnection.** Exhaustively, for V1:

| Key | Contents | TTL | Rebuilt by |
|---|---|---|---|
| `presence:{room}:{user}` | Heartbeat marker | 45s | The client's next heartbeat, 15s later |
| `rl:{scope}:{key}` | Token-bucket counters (§12.3) | Window length | Naturally — worst case a user briefly gets a fresh allowance |
| `ws:ticket:{id}` | Single-use WebSocket ticket | 60s | The client requests another over HTTPS |
| `idem:{key}` | Idempotency response cache | 24h | Also persisted in Postgres; Redis is the fast path only |
| `q:{question}:{user}` | Trivia nonce and open-time | Session | Session row in Postgres |
| `lb:snapshot:{period}` | Leaderboard read cache | 60s | Recomputed from `leaderboard_snapshot` |
| `pubsub:room:{id}` | Cross-instance fan-out channel | n/a | Not storage; transport |

**Never in Redis:** room membership, the event log, `seq` counters, chat
messages, XP, scores, achievements, sessions, or refresh tokens.

Two enforcement mechanisms, because a rule this easy to break needs both:

1. **A chaos test in CI-adjacent staging** (§14.3): `docker compose stop redis`,
   then assert that an existing room still accepts a chat message, still
   advances its timeline, and still serves a resync. This is the executable form
   of the claim.
2. **Graceful degradation, not failure.** Every Redis call site has a documented
   fallback: presence degrades to "unknown", rate limiting fails **closed** for
   auth endpoints and **open** for read endpoints, and the idempotency cache
   falls through to its Postgres row.

## Consequences

**Becomes easy**

- Backup strategy is genuinely simple: Postgres has PITR with RPO 5 min, Redis
  has nothing, and that is correct rather than negligent.
- Redis can be restarted, resized, or failed over during business hours.

**Becomes hard**

- Every future Redis use must be justified against this table, and the table
  must be updated. The temptation to cache "just this one query" is constant.
- The fail-closed/fail-open split must be deliberate per endpoint. Getting it
  backwards on the login endpoint would turn a Redis outage into an open door.

This is the answer to interview question §16.4.9 — and the answer includes "and
we tested it", because the chaos test exists.
