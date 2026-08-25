# ADR-010: UUIDv7 primary keys

Status: Accepted
Date: 2026-08-25

## Context

Every table needs a primary key (§6.1). The choice interacts with three things
that matter here: index write locality on the hot append-only tables
(`room_events`, `xp_ledger`, `chat_messages`), whether IDs can be enumerated by
an attacker, and whether a client can generate an ID before the server sees it —
which the offline outbox (§11.3) would like.

## Options considered

| Option | Index locality | Enumerable | Client-generatable | Notes |
|---|---|---|---|---|
| `bigserial` | Excellent | **Yes** — `/rooms/41` implies `/rooms/40` exists | No | Leaks volume; a sequence is a business metric an attacker reads for free |
| UUIDv4 | **Poor** — random inserts scatter across the B-tree, causing page splits and WAL bloat | No | Yes | The classic mistake at write-heavy scale |
| **UUIDv7** | Good — time-ordered prefix, near-append inserts | No | Yes | Millisecond timestamp prefix plus randomness |
| ULID | Good | No | Yes | Same idea, but not a UUID; weaker native Postgres/driver support |
| Snowflake | Excellent | Partially | Needs a node ID | Requires coordination infrastructure we do not want |

## Decision

**UUIDv7 everywhere**, stored in Postgres as native `uuid` (16 bytes, not a
36-byte string).

The deciding factor is the combination the other options cannot offer together:

- **Write locality.** `room_events` is the hottest insert path in the system —
  20 events/sec/room across 500 rooms at the §13.4 target. UUIDv4's random
  distribution turns that into scattered B-tree page splits and inflated WAL.
  UUIDv7's time prefix keeps inserts near the right edge of the index, which is
  the same property that made `bigserial` attractive.
- **No enumeration.** Room codes, user IDs, and session IDs appear in URLs and
  deep links. A sequential ID tells an attacker how many rooms exist and lets
  them walk the space.
- **Client generation.** The outbox generates an idempotency key at enqueue time
  (ADR-008); being able to mint a valid, sortable ID offline without a server
  round trip falls out for free.

Generation is server-side by default via `uuidv7()` in Go, not a Postgres
default, so the ID is available to application code before the insert — which
the transactional outbox needs in order to reference the row it is about to
write in the same transaction.

**Room join codes are a separate concern and deliberately not UUIDs.** A UUID is
unusable as something a person reads aloud. Join codes are short, human-typable,
Crockford base32 (no `I`, `L`, `O`, `U` — avoids both confusion and accidental
profanity), rate-limited on resolve, and single-purpose. A join code authorises
nothing on its own; the server authorises on resolve (§12.1).

## Consequences

**Becomes easy**

- `ORDER BY id` is approximately chronological, so cursor pagination (§5.2) has
  a natural, opaque, stable sort key with no extra column.
- Merging data from multiple sources or environments never collides.

**Becomes hard**

- 16 bytes per key instead of 8. Across `room_events` at millions of rows this
  is real storage, accepted for the properties above and bounded by the §6.5
  30-day retention.
- UUIDv7 is RFC 9562, but tooling maturity varies. The Go generator is pinned
  and unit-tested for monotonicity within the same millisecond — two IDs minted
  in the same millisecond must still sort in creation order, and that is
  precisely the property `room_events` ordering leans on.
- IDs leak their creation time. This is acceptable for our entities; it would
  not be for something like a password-reset token, which is therefore random
  and not a UUIDv7.

**Revisit** if a table's insert rate makes 16-byte keys a measured bottleneck.
Not on suspicion — on a measurement.
