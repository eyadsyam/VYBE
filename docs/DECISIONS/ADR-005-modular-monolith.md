# ADR-005: Modular monolith in Go, with CI-enforced module boundaries

Status: Accepted
Date: 2026-08-25

## Context

§5.1 mandates a modular monolith with thirteen modules and states the rule that
makes it real: *an unenforced boundary is a comment.* The language was left open
by the master prompt and is decided here alongside the shape, because the two
interact — the enforcement mechanism differs per ecosystem.

The dominant technical constraint is §13.4: **5,000 concurrent WebSocket
connections, 500 active rooms, 20 events/sec/room during a trivia round, p95
fan-out under 150ms**, on a single instance before horizontal scaling.

## Options considered

### Shape

| Option | Pros | Cons |
|---|---|---|
| Single-package monolith | Fastest to write | No extraction path; every "module" is a naming convention; §5.1 unsatisfiable |
| **Modular monolith** | One deploy, one transaction boundary, real internal seams, genuine extraction path | Boundaries must be mechanically enforced or they rot |
| Microservices | Independent scaling | Distributed transactions, network hops inside a request, tracing overhead — the §2.2 "Never" list names this explicitly |

### Language

| Criterion | Go 1.26 | Node 24 + TS | Dart 3.12 |
|---|---|---|---|
| Memory per idle WS connection | ~4–8 KB (goroutine + buffers) | ~30–60 KB | ~20–40 KB |
| 5k-connection fan-out on one box | Comfortable | Needs clustering + `uWebSockets.js` | Unproven at this scale |
| Concurrency model fit | Goroutine per connection reads naturally | Event loop; CPU-bound scoring blocks everything | Isolates, but ecosystem is thin |
| Postgres tooling | `pgx` — excellent, native | Very good | Weak |
| OpenAPI 3.1 codegen | `oapi-codegen` | Best in class | Poor |
| Shared models with Flutter | No | No | **Yes** |
| Boundary enforcement in CI | `go-arch-lint`, internal packages | `eslint-plugin-boundaries` | Ad hoc |

## Decision

**A modular monolith written in Go 1.26**, with thirteen modules under
`server/internal/modules/`: `identity`, `users`, `social`, `catalog`,
`discovery`, `rooms`, `realtime`, `games`, `progression`, `notifications`,
`moderation`, `analytics`, `entitlements`.

Deciding factor for the language: **per-connection cost.** The load target is
5,000 sockets on one instance. At Node's ~40 KB/connection that is ~200 MB of
socket overhead before any room state; at Go's ~6 KB it is ~30 MB. Go's
goroutine-per-connection model also lets the fan-out path be written as
straight-line blocking code, which is far easier to reason about — and to defend
in an interview — than callback or isolate choreography.

Cost accepted: a second language alongside Dart, and no shared domain models
between app and server. The OpenAPI 3.1 contract (§5.2) is what bridges them,
and it is the source of truth for both sides.

**Boundary rules, enforced not asserted:**

1. Each module owns its tables. **No cross-module table reads** — verified by a
   CI check that greps each module's SQL for table names owned by other modules.
2. Cross-module data flows through a published facade interface or a domain
   event. Never a direct struct reference into another module's internals.
3. Each module exposes exactly three things: a public facade, the events it
   publishes, and the events it consumes.
4. Import direction is checked in CI. A module importing another module's
   non-facade package fails the build.

Go's `internal/` visibility gives part of this for free; the import-direction
check covers the rest.

## Consequences

**Becomes easy**

- One deploy, one database transaction boundary. "Write the domain change and
  the outbox row in one transaction" (§5.4) is a plain `BEGIN`, which is the
  entire reason exactly-once achievement grants are achievable.
- Extraction later is mechanical: a module already talks only through a facade,
  so replacing the facade's implementation with an RPC client is a contained
  change.

**Becomes hard**

- Two languages means two toolchains, two CI lanes, two dependency audits.
- The boundary checks must be written and maintained. If they are ever disabled
  "temporarily", the architecture reverts to a comment within a month.

**Extraction triggers** (§7.9 — state the trigger, not the date): extract the
`realtime` tier when p95 fan-out exceeds 200ms, or when realtime CPU exceeds 60%
of total process CPU. This is the answer to interview question §16.4.10.
