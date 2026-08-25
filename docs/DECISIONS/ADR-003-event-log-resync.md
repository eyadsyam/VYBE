# ADR-003: Per-room monotonic event log with snapshot/delta resync

Status: Accepted
Date: 2026-08-25

## Context

Mobile clients disconnect constantly — tunnels, lifts, WiFi to LTE handover, the
OS suspending the app after 30s in background. §7.3 states the governing rule:
*the client never assumes its pre-disconnect state is still valid.* We need a
recovery mechanism that is correct regardless of how long the client was away,
and cheap in the common case (away for four seconds).

## Options considered

| Option | Pros | Cons |
|---|---|---|
| **A. Full snapshot on every reconnect** | Trivially correct; one code path | A three-second tunnel costs a full room state transfer. At 500 rooms x 6 users on a flaky network this becomes the dominant bandwidth cost. |
| **B. Delta only, from a client-held cursor** | Minimal bytes | Unbounded server retention; a client away for an hour cannot be served; no bound on replay cost |
| **C. Last-writer-wins state broadcast, no log** | Simplest server | Cannot reconstruct ordering; concurrent chat and trivia interleave wrongly; makes exactly-once achievement grants impossible |
| **D. CRDTs** | Converges without a server referee | Enormous complexity in a domain where the server *is* authoritative by design (ADR-004). Solves a problem we do not have. |
| **E. Monotonic per-room `seq`, delta if cheap else snapshot** | Cheap common case, correct worst case, bounded retention | Two code paths; `seq` assignment must be serialised per room |

## Decision

**Option E.** Every room state change is appended to `room_events` with a
gap-free, server-assigned, monotonic `seq`, unique on `(room_id, seq)`. The
envelope is §7.2 verbatim:

```jsonc
{ "v": 1, "id": "<uuidv7>", "room": "<uuidv7>", "seq": 1482,
  "type": "TRIVIA_QUESTION_OPEN", "ts": "<rfc3339 ms, server clock>",
  "actor": { "id": "<uuidv7>", "role": "host" }, "payload": { } }
```

Resync decision, server-side:

```
gap = current_seq - client_last_seq

gap <= 200  AND  events still within retention  ->  DELTA    (events in (last_seq, current])
otherwise                                       ->  SNAPSHOT (full state + seq)
```

The threshold of 200 is a **starting hypothesis, not a measured value**. It is
configuration, not a constant, and §14.2 instrumentation records the
delta/snapshot ratio so it can be tuned against real data.

Three invariants make this work:

1. **A client that observes a `seq` gap must resync.** Gaps are not tolerated;
   they are the detection mechanism.
2. **Unknown `type` is ignored and logged, never fatal.** This is what lets the
   server ship new event types before clients update.
3. **Payloads are additive-only.** A breaking change bumps `v`, and both
   versions are served in parallel for one release cycle.

## Consequences

**Becomes easy**

- Reconnect correctness stops being a special case — it is one RESYNC message.
- Duplicate delivery is harmless: clients dedupe on envelope `id` with a
  500-entry LRU, so at-least-once transport delivery is sufficient.
- A room survives a server restart, because the log is in Postgres, not memory.

**Becomes hard**

- `seq` assignment must be serialised per room. In the single-instance topology
  this is an in-process per-room mutex, with the `(room_id, seq)` unique index
  as the real guarantee. At the ~100K tier it becomes room-affinity routing
  (§7.9); the unique index remains the backstop either way.
- `room_events` grows fast. Mitigated by monthly partitioning and the §6.5
  retention rule: 30 days, then aggregate-only.

**Snapshot contents** are fixed by §7.3 and deliberately exclude answer keys:
state-machine position, timeline anchor plus server time, participants, last 50
chat messages, reaction aggregates, active trivia and prediction state *without
correct answers*, and host identity.

**Revisit when** instrumentation shows the 200 threshold producing more than 20%
snapshots, or when p95 fan-out exceeds 200ms (the §7.9 extraction trigger).
