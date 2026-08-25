# SPEC-001: V1 Vertical Slice — Companion Sync Room with Server-Authoritative Trivia

| | |
|---|---|
| **Author** | Lead Engineer (AI agent, per Master Prompt v2 PART 0) |
| **Date** | 2026-08-25 |
| **Status** | Draft — awaiting approval |
| **Reviewers** | @eyadsyam |
| **Implements** | Master Prompt v2 §2.1, §3.3, §7.2–7.7, §8.1–8.2 |
| **Milestone** | M1 (entry conditions: M0 exit criteria met) |
| **Related ADRs** | 001, 002, 003, 004, 005, 009, 010, 011 |

---

## 1. Context

Master Prompt v2 §2.1 defines exactly one path that must be perfect before
anything else ships:

> A user signs up → finds a show → creates a room → shares a link → a friend
> joins on a second physical device → they run the sync ritual → they chat and
> react on the shared timeline → a 5-question trivia round runs with
> server-authoritative scoring → XP is awarded → one of them loses network for
> 30 seconds and rejoins with correct state.

This slice is chosen deliberately over breadth. §17 states the failure mode it
avoids: *"a real-time demo that has only ever run on one device"* and *"40
beautiful screens that all break offline."* The slice exercises, end to end,
every hard property the system claims: distributed state, clock synchronisation
without a shared media clock, server authority under a hostile client, ordered
event replay after disconnection, and offline-tolerant UI.

**Why now.** Nothing downstream can be validated without it. The ranker
(ADR-007) needs watch data the room generates; the leaderboard (§8.6) needs the
XP ledger this slice writes; moderation (§12.4) needs the chat this slice
creates. The slice is the load-bearing wall.

**Evidence of the risk being addressed.** §2.4 R1 rates Companion Sync drift as
High impact / High likelihood — the highest combined risk in the register. This
spec's §5 acceptance criteria for drift and resync are the mitigation, and they
are the reason AC-20 through AC-27 are non-negotiable.

---

## 2. Functional Requirements

### 2.1 Identity (`identity` module)

| ID | Requirement |
|---|---|
| **FR-1** | The system MUST allow a user to register with email and password, and MUST reject a password present in a known-breached-password set. |
| **FR-2** | The system MUST collect date of birth at registration and MUST derive an age band. Accounts under 16 MUST default to: no public rooms, no discoverability, no public leaderboard entry (§12.4). |
| **FR-3** | The system MUST issue an Ed25519-signed JWT access token with a 15-minute TTL and an opaque, hashed, rotating refresh token with a 60-day TTL (ADR-011). |
| **FR-4** | On presentation of an already-rotated refresh token, the system MUST revoke the entire token family, invalidate all sessions in it, and emit an alert (ADR-011). |
| **FR-5** | The system MUST issue single-use WebSocket tickets with a 60-second TTL over HTTPS. It MUST NOT accept an access token as a WebSocket query parameter. |
| **FR-6** | The system MUST record each session as a row with device name, platform, truncated IP, created-at, and last-seen; and MUST allow a user to list and revoke sessions. |

### 2.2 Catalogue (`catalog` module)

| ID | Requirement |
|---|---|
| **FR-7** | The system MUST allow search over the local `content` table by title, returning results ranked exact-prefix > trigram similarity > cast/crew > description, with a popularity tiebreak (ADR-006). |
| **FR-8** | Search MUST apply `vybe_normalize()` to both the indexed text and the query, such that a query of `احمد` returns content titled `أحمد` (ADR-006). |
| **FR-9** | The system MUST return content detail including title, year, synopsis, poster reference, runtime, and where-to-watch offers for the user's region. |
| **FR-10** | The system MUST NOT expose any provider API key to the client, and MUST NOT accept a user-supplied video URL anywhere in the API (§1.9). |

### 2.3 Rooms (`rooms` module)

| ID | Requirement |
|---|---|
| **FR-11** | An authenticated user MUST be able to create a room bound to one `content` item, choosing visibility `private` or `public`. The creator becomes `host`. |
| **FR-12** | Each room MUST have a join code: Crockford base32, 6 characters, excluding `I`, `L`, `O`, `U`, unique among non-ended rooms (ADR-010). |
| **FR-13** | The system MUST expose the room as a Universal/App Link at `https://vybe.app/r/{code}`. A custom scheme MAY exist only as a fallback (§1.9). |
| **FR-14** | The server MUST authorise membership on resolve of a join code. Possession of a code MUST NOT itself grant access to a private room. |
| **FR-15** | A room MUST follow the state machine `LOBBY → READY → PLAYING → ENDED`, with `ENDED` terminal. Illegal transitions MUST be rejected with `409`. |
| **FR-16** | Free-tier rooms MUST cap at 4 participants, enforced server-side against the caller's `entitlement_tier` (§1.8). |
| **FR-17** | If the host disconnects for more than 60 seconds, the system MUST promote the longest-tenured connected participant and broadcast `HOST_CHANGED` (§7.7). |
| **FR-18** | A reaper job MUST end any room with zero connected participants for 10 minutes (§7.7). |

### 2.4 Sync ritual and shared timeline (`realtime` module)

| ID | Requirement |
|---|---|
| **FR-19** | The host MUST be able to arm the ritual. The server MUST broadcast `SYNC_ARM` carrying `server_start_at` set to T+7s, and transition the room to `READY`. |
| **FR-20** | Clients MUST render a 3-second countdown driven by the corrected server clock, never by local device time. |
| **FR-21** | At `server_start_at` the room MUST transition to `PLAYING`, and the shared timeline MUST begin at `t_room = 0`. |
| **FR-22** | The timeline MUST be computed as `t_room = (server_now − anchor_server_time) + anchor_offset_ms`, where `server_now` is the device clock corrected by the measured offset (ADR-002). |
| **FR-23** | The system MUST measure clock offset via a PING/PONG handshake on connect and every 60s, keeping a rolling window of 5 samples and selecting the sample with the **lowest RTT**, not the mean (§7.4). |
| **FR-24** | A sample with RTT > 2s MUST be discarded and the connection marked `degraded`. |
| **FR-25** | A user MUST be able to self-report drift, receiving a ±5s local nudge (adjusting only their own offset) or requesting a host re-anchor. |
| **FR-26** | If more than 40% of participants report drift in the same direction exceeding 8s, the system MUST prompt the host to re-anchor. A re-anchor MUST be broadcast as `TIMELINE_REANCHOR` so all clients converge. |
| **FR-27** | A timed event MUST fire only if the local corrected clock is within ±1.5s of its `fires_at_timeline_ms`. Outside that window it MUST be skipped and logged as drift, never fired late. |

### 2.5 Event log and resync (`realtime` module)

| ID | Requirement |
|---|---|
| **FR-28** | Every shared-state change MUST be appended to `room_events` with a gap-free, server-assigned, monotonic `seq`, unique on `(room_id, seq)` (ADR-003). |
| **FR-29** | Every event MUST use the §7.2 envelope: `v`, `id`, `room`, `seq`, `type`, `ts`, `actor`, `payload`. |
| **FR-30** | A client MUST send `RESYNC { room, last_seq }` on any of: a `seq` gap, reconnection, resume from background, or 30s of socket silence. |
| **FR-31** | On `RESYNC`, the server MUST return a **delta** when `current_seq − last_seq ≤ 200` and the events remain within retention; otherwise a **snapshot**. The threshold MUST be configuration, not a constant. |
| **FR-32** | A snapshot MUST contain: state-machine position, timeline anchor and server time, participants, last 50 chat messages, reaction aggregates, active trivia/prediction state, and host identity. It MUST NOT contain any trivia correct-answer field. |
| **FR-33** | A client receiving an unknown event `type` MUST ignore it and log it. It MUST NOT crash or drop the connection. |
| **FR-34** | A client MUST dedupe by envelope `id` using an LRU of the last 500 ids. |
| **FR-35** | On applying a snapshot, the client MUST discard conflicting local optimistic state. |

### 2.6 Chat, reactions, presence

| ID | Requirement |
|---|---|
| **FR-36** | A participant MUST be able to post a chat message, persisted with `deleted_at`/`deleted_by` columns for moderation audit (§6.2). |
| **FR-37** | Chat MUST be rate-limited server-side to 5 per 10s with a burst of 3, returning `429` with `Retry-After` (§12.3). |
| **FR-38** | Reactions MUST NOT be stored one row per tap. Clients MUST batch at 250ms; the server MUST aggregate into 1-second timeline buckets and broadcast counts as `(room_id, timeline_bucket, emoji, count)` (§6.2, §7.6). |
| **FR-39** | Presence MUST use a 15s heartbeat with a 45s Redis TTL. A disconnect MUST NOT broadcast `PARTICIPANT_LEFT` until a 30-second grace period has elapsed (§7.5). |
| **FR-40** | Presence MUST be derived state only. Membership MUST live in Postgres and MUST survive a Redis flush (ADR-009). |
| **FR-41** | The server MUST filter fan-out per recipient. A user who has blocked another MUST NOT receive that user's chat, reactions, or presence. Filtering MUST occur server-side before transmission, never client-side (§7.8). |
| **FR-42** | Host-only actions (`SYNC_ARM`, `TIMELINE_REANCHOR`, `TRIVIA_START`, `KICK`) MUST be re-verified server-side on every request. Client-held role MUST carry zero authority. |

### 2.7 Trivia (`games` module)

| ID | Requirement |
|---|---|
| **FR-43** | The host MUST be able to start a 5-question round bound to the room's content. |
| **FR-44** | `QUESTION_OPEN` MUST carry `{ question_id, text, options[{id,text}], deadline_ts, timeline_ms, nonce }` and MUST NOT contain the correct answer or the points formula in any form (ADR-004). |
| **FR-45** | The `nonce` MUST be unique per `(user, question)`. Submitting another user's nonce MUST be rejected `403 INVALID_NONCE`. |
| **FR-46** | Answer submission MUST require an `Idempotency-Key` header. |
| **FR-47** | The server MUST accept an answer only if: the session is active; the user is a participant; the question is open; `server_receive_time ≤ deadline + 400ms`; the nonce matches; and no prior answer exists. |
| **FR-48** | Uniqueness MUST be enforced by a database constraint on `(session_id, user_id, question_id)`, not by an application-level check alone. |
| **FR-49** | Scoring MUST be `base + round(base × 0.5 × max(0, 1 − elapsed/time_limit))` where `elapsed = server_receive_time − question_open_time − (rtt/2)`, clamped ≥ 0. |
| **FR-50** | Elapsed time MUST be derived from server receipt time. `client_ts` MAY be recorded as telemetry but MUST NOT affect scoring. |
| **FR-51** | `QUESTION_CLOSE` MUST reveal the correct answer and per-user results only after the deadline has passed. |

### 2.8 Progression (`progression` module)

| ID | Requirement |
|---|---|
| **FR-52** | XP MUST be an append-only ledger (`xp_ledger`), never a mutable counter on `users` (§6.2). |
| **FR-53** | XP MUST be awarded only on server-verified terminal events (`TRIVIA_ROUND_COMPLETED`), never on `ANSWER_SUBMITTED` (§8.4). |
| **FR-54** | XP grants MUST be idempotent on `(source_type, source_id)`, enforced by a unique constraint. |
| **FR-55** | Room XP MUST require ≥2 distinct participants and ≥5 minutes duration, to prevent solo-room farming (§8.4). |
| **FR-56** | XP MUST be written through the transactional outbox: the domain change and the outbox row MUST be committed in a single transaction (§5.4). |

### 2.9 Cross-cutting

| ID | Requirement |
|---|---|
| **FR-57** | All non-GET mutations MUST require an `Idempotency-Key`. The server MUST store key→response for 24h and replay on repeat (§5.2). |
| **FR-58** | All errors MUST use RFC 9457 `application/problem+json` with `type`, `title`, `status`, `detail`, `code`, `traceId`, and `errors[]` for field validation. |
| **FR-59** | All list endpoints MUST use opaque cursor pagination. Offset pagination MUST NOT be used (§5.2). |
| **FR-60** | Every screen backed by data MUST implement every applicable state in §3.2, including a freshness indicator on cached content. Stale data MUST NOT be presented as live. |
| **FR-61** | All user-facing strings MUST come from `.arb` files. A literal string in a widget file MUST fail CI lint (§3.6). |
| **FR-62** | Every layout MUST use directional properties (`EdgeInsetsDirectional`, `start`/`end`). `left`/`right` MUST fail review. |

---

## 3. Non-Functional Requirements

| ID | Requirement | Threshold | Verification |
|---|---|---|---|
| **NFR-1** | Cold start to first frame | < 2.5s on mid-tier Android, 4GB RAM | CI integration test with timeline capture |
| **NFR-2** | Room event → UI update | < 100ms after socket receipt | Instrumented trace, p95 |
| **NFR-3** | WebSocket fan-out latency | p95 < 150ms in-region | Load test, §13.4 |
| **NFR-4** | Concurrent WebSocket connections | ≥ 5,000 on one instance | Load test |
| **NFR-5** | Concurrent active rooms | ≥ 500, mean 6 participants | Load test |
| **NFR-6** | Sustained event rate during trivia | 20 events/sec/room | Load test |
| **NFR-7** | Memory, 30-minute room session | < 250MB steady | Profiler, mid-tier Android |
| **NFR-8** | Home feed scroll | p95 frame build < 16ms, zero jank frames over a 5s scroll | `flutter driver` timeline |
| **NFR-9** | API latency | p95 < 300ms, p99 < 800ms | RED metrics |
| **NFR-10** | Crash-free sessions | > 99.5% | Crash reporting |
| **NFR-11** | Domain-layer test coverage | ≥ 90% | CI gate, build fails below |
| **NFR-12** | Repository / ViewModel coverage | ≥ 80% | CI gate |
| **NFR-13** | Static analysis | Zero warnings (`flutter analyze`, `go vet`, `golangci-lint`) | CI gate |
| **NFR-14** | Touch targets | ≥ 48×48dp on every interactive element | Widget test assertion |
| **NFR-15** | Contrast | ≥ 4.5:1 body text, ≥ 3:1 large text and meaningful icons | Golden + audit |
| **NFR-16** | Text scaling | 200% without truncation or overlap on all V1 screens | Golden tests at 2.0 scale |
| **NFR-17** | RTL | Every V1 screen renders correctly in `ar` | Golden tests, CI gate |
| **NFR-18** | Countdown announcements | `SemanticsService.announce` at 10s, 5s, 0 | Widget test |
| **NFR-19** | No secret in the client binary | Zero | Secret scan in CI on release artefact |
| **NFR-20** | Motion | No blocking animation > 400ms; all motion respects reduced-motion | Review + widget test |

---

## 4. Data Models

Full DDL in `server/migrations/`. Key entities for this slice:

### `rooms`
| Field | Type | Constraints |
|---|---|---|
| `id` | `uuid` | PK, UUIDv7 |
| `content_id` | `uuid` | FK → `content.id`, `ON DELETE RESTRICT` |
| `host_user_id` | `uuid` | FK → `users.id`, `ON DELETE RESTRICT` |
| `join_code` | `text` | Unique among non-ended rooms (partial unique index), 6 chars Crockford base32 |
| `visibility` | `room_visibility` | enum: `private`, `public` |
| `state` | `room_state` | enum: `LOBBY`, `READY`, `PLAYING`, `ENDED` |
| `sync_mode` | `sync_mode` | enum: `COMPANION`, `CLIP`, `ASYNC` |
| `anchor_server_time` | `timestamptz(3)` | NULL until `PLAYING` |
| `anchor_offset_ms` | `bigint` | Default 0 |
| `max_participants` | `int` | Default 4; server-enforced against entitlement |
| `current_seq` | `bigint` | Default 0; monotonic, incremented under per-room serialisation |
| `created_at` / `updated_at` / `ended_at` | `timestamptz(3)` | |

### `room_events`
| Field | Type | Constraints |
|---|---|---|
| `id` | `uuid` | PK, UUIDv7 — also the envelope `id` used for client dedupe |
| `room_id` | `uuid` | FK → `rooms.id`, `ON DELETE CASCADE` |
| `seq` | `bigint` | **Unique on `(room_id, seq)`** — the backbone of resync |
| `type` | `text` | Event type |
| `actor_user_id` | `uuid` | Nullable — system events have no actor |
| `actor_role` | `text` | `host` / `participant` / `system` |
| `payload` | `jsonb` | Additive-only |
| `timeline_ms` | `bigint` | Nullable — set for timed events |
| `created_at` | `timestamptz(3)` | Server clock; the envelope `ts` |

Partitioned monthly. Retention 30 days, then aggregate-only (§6.5).

### `trivia_answers`
| Field | Type | Constraints |
|---|---|---|
| `id` | `uuid` | PK |
| `session_id` | `uuid` | FK |
| `user_id` | `uuid` | FK |
| `question_id` | `uuid` | FK |
| `option_id` | `uuid` | The submitted answer |
| `is_correct` | `boolean` | **Computed server-side only** |
| `points_awarded` | `int` | Integer, never float (§6.1) |
| `server_received_at` | `timestamptz(3)` | The authoritative timing source |
| `client_ts` | `timestamptz(3)` | Telemetry only; never used for scoring |
| `rtt_ms` | `int` | For compensation |
| | | **UNIQUE `(session_id, user_id, question_id)`** — FR-48 |

### `xp_ledger`
| Field | Type | Constraints |
|---|---|---|
| `id` | `uuid` | PK |
| `user_id` | `uuid` | FK |
| `source_type` | `text` | e.g. `TRIVIA_ROUND_COMPLETED` |
| `source_id` | `uuid` | The originating entity |
| `amount` | `int` | Integer; may be negative for reversal |
| `created_at` | `timestamptz(3)` | |
| | | **UNIQUE `(user_id, source_type, source_id)`** — FR-54 |

Append-only. The user's total is a materialised sum, reconcilable from the
ledger at any time.

---

## 5. Acceptance Criteria

### Sync ritual

**AC-1** *(FR-19, FR-20, FR-21)*
Given a room in `LOBBY` with 2 connected participants,
When the host arms the ritual,
Then both clients receive `SYNC_ARM` with the same `server_start_at`, both render a countdown, and at `server_start_at` both transition to `PLAYING` with `t_room` within 250ms of each other.

**AC-2** *(FR-20, FR-22)*
Given a participant whose device clock is set 5 minutes **behind** real time,
When the ritual runs,
Then their countdown completes simultaneously with the other participant's, and their `t_room` remains within 250ms — because the corrected offset absorbs the skew.

**AC-3** *(FR-23)*
Given 5 PING/PONG samples with RTTs of 40, 45, 800, 60, and 50 ms,
When the offset is computed,
Then the sample with RTT 40ms is used and the 800ms sample is not averaged in.

**AC-4** *(FR-24)*
Given a PING/PONG sample with RTT of 2,500ms,
When the sample is processed,
Then it is discarded and the connection is marked `degraded`.

**AC-5** *(FR-27)*
Given a trivia beat scheduled at `timeline_ms = 60000` and a client whose corrected clock reads `t_room = 62000`,
When the beat's scheduled moment arrives,
Then the beat is **skipped and logged as drift**, and is not fired late.

**AC-6** *(FR-26)*
Given a room of 5 participants where 3 report drift of +9s in the same direction,
When the third report arrives,
Then the host is prompted to re-anchor, and on re-anchor all 5 clients receive `TIMELINE_REANCHOR` and converge to the same `t_room` within 250ms.

### Event log and resync

**AC-7** *(FR-28, FR-29)*
Given a room with 100 events emitted,
When `room_events` is read,
Then `seq` values are exactly 1..100 with no gaps, and every row's envelope validates against the §7.2 schema.

**AC-8** *(FR-30, FR-31)*
Given a client at `last_seq = 1400` and a room at `current_seq = 1450`,
When the client sends `RESYNC`,
Then it receives a **delta** containing exactly events 1401 through 1450.

**AC-9** *(FR-31)*
Given a client at `last_seq = 1000` and a room at `current_seq = 1500`,
When the client sends `RESYNC`,
Then it receives a **snapshot**, not a delta, because the gap of 500 exceeds the threshold of 200.

**AC-10** *(FR-32)*
Given an active trivia question,
When any client requests a snapshot,
Then the returned payload contains the active question but contains **no field carrying the correct answer**, verified by asserting on the full serialised payload.

**AC-11** *(FR-33)*
Given a client receiving an event with `type = "FUTURE_FEATURE_XYZ"`,
When the event is processed,
Then the client logs it, ignores it, keeps the socket open, and continues processing subsequent events.

**AC-12** *(FR-34)*
Given the same envelope `id` delivered twice,
When both are processed,
Then the state change is applied exactly once.

**AC-13** *(FR-35, §13.2.2 — the mandatory E2E)*
Given devices A and B in a `PLAYING` room, A has emitted 20 events, and B loses network for 30 seconds,
When B reconnects and sends `RESYNC`,
Then B receives the authoritative state, reconciles, shows the correct `t_room` within 250ms of A's, shows all 20 missed events, and displays no duplicates.

### Trivia and anti-cheat — all ten §8.2 cases

**AC-14** *(FR-48)* Given a submitted answer, When the same answer is submitted again, Then the second is rejected `409 DUPLICATE_ANSWER` and the score is unchanged.

**AC-15** *(FR-47)* Given a question whose deadline passed 500ms ago, When an answer arrives, Then it is rejected `422 QUESTION_CLOSED` — 500ms exceeds the 400ms grace.

**AC-16** *(FR-47)* Given a question belonging to session X, When a user submits it against session Y, Then it is rejected `403`.

**AC-17** *(FR-45)* Given user A's nonce for question Q, When user B submits with A's nonce, Then it is rejected `403 INVALID_NONCE`.

**AC-18** *(FR-50)* Given a device with its clock set 60 seconds behind, When the user answers, Then `points_awarded` is byte-identical to the same submission from a correctly-clocked device.

**AC-19** *(FR-48)* Given 100 concurrent submissions from one user for one question, When all are processed, Then exactly one row exists in `trivia_answers` and 99 requests receive `409`.

**AC-20** *(FR-44)* Given a `QUESTION_OPEN` event, When its full serialised payload is inspected, Then no field, nested field, or encoded value contains the correct answer.

**AC-21** *(FR-47)* Given a `question_id` that was never opened in this session, When it is submitted, Then it is rejected `404`.

**AC-22** *(FR-47)* Given a user who is not a participant in the room, When they submit an answer, Then it is rejected `403`.

**AC-23** *(§12.3)* Given a client submitting 50 requests per second, When the rate limit is evaluated, Then requests are rejected `429` with `Retry-After` after the threshold and the session is flagged for review — **not auto-banned** (§8.4).

### Scoring and XP

**AC-24** *(FR-49)* Given `base = 100`, `time_limit = 20s`, `elapsed = 5s`, When scored, Then `points_awarded = 138` (100 + round(100 × 0.5 × 0.75)).

**AC-25** *(FR-49)* Given `elapsed` computed as negative after RTT compensation, When scored, Then `elapsed` is clamped to 0 and the full speed bonus is awarded.

**AC-26** *(FR-53, FR-54)* Given a completed trivia round, When the outbox worker runs twice on the same event, Then exactly one `xp_ledger` row exists.

**AC-27** *(FR-55)* Given a room with 1 participant lasting 20 minutes, When XP is evaluated, Then no room XP is granted.

### Authorisation and safety

**AC-28** *(FR-42)* Given a non-host participant, When they send `TRIVIA_START`, Then it is rejected `403` regardless of any client-side role claim.

**AC-29** *(FR-41, §13.2.7)* Given A has blocked B and both are in the same public room, When B posts a chat message and a reaction, Then A's socket receives **neither**, verified by asserting on raw socket frames, not on rendered UI.

**AC-30** *(FR-14)* Given a valid join code for a **private** room, When a non-invited user resolves it, Then access is refused — the code alone grants nothing.

**AC-31** *(FR-2)* Given a user whose age band is under 16, When they attempt to create a public room, Then it is refused, and they do not appear in discovery or public leaderboards.

### Offline, i18n, accessibility

**AC-32** *(FR-60)* Given cached content older than its TTL, When the screen renders, Then a visible age indicator is shown ("updated 2h ago") and the data is not presented as live.

**AC-33** *(§11.3)* Given the device is offline, When the user attempts to send a chat message, Then the UI states that the action requires a connection. It MUST NOT show an optimistic success.

**AC-34** *(FR-61, NFR-17)* Given locale `ar`, When every V1 screen renders, Then the golden test matches, layout is RTL, directional icons are mirrored, and no text is clipped at 200% scale.

**AC-35** *(NFR-18)* Given a trivia countdown, When it reaches 10s, 5s, and 0, Then `SemanticsService.announce` fires at each.

---

## 6. Edge Cases

| ID | Case | Required behaviour |
|---|---|---|
| **EC-1** | Host disconnects mid-countdown | Countdown continues on the server clock; if the host is absent > 60s, promote and broadcast `HOST_CHANGED` (FR-17) |
| **EC-2** | Every participant disconnects | Room persists; reaper ends it after 10 minutes of zero connections (FR-18) |
| **EC-3** | Server restarts mid-room | Clients reconnect, `RESYNC`, and state rehydrates from Postgres. Rooms survive. (§7.7) |
| **EC-4** | **Redis is killed mid-room** | Rooms survive. Presence degrades to "unknown"; membership, timeline, and chat are unaffected (ADR-009). Chaos-tested. |
| **EC-5** | Network switches WiFi → LTE | Immediate reconnect, new WS ticket, same session, `RESYNC` |
| **EC-6** | App backgrounded > 30s | Socket closed; timeline continues locally; `RESYNC` on resume |
| **EC-7** | Access token expires mid-room | Silent refresh, WS ticket renewed, **no visible interruption** (§13.2.6) |
| **EC-8** | Refresh token reuse detected mid-room | Family revoked, socket closed, user returned to sign-in with a clear explanation — not a silent failure |
| **EC-9** | Outbound queue overflows (256 cap) | Drop coalescable classes first (reactions, presence). If a critical event would be dropped, **force the client to resync** rather than deliver an inconsistent stream (§7.6) |
| **EC-10** | Two answers race at the exact deadline | DB uniqueness decides; the loser gets `409`, not a 500 |
| **EC-11** | Join code collides on generation | Retry generation; after 5 failures return `503`, never reuse a live code |
| **EC-12** | Room's content is deleted from catalogue | `ON DELETE RESTRICT` prevents it; content is soft-retired instead |
| **EC-13** | Clock offset cannot be measured (all samples > 2s RTT) | Connection marked `degraded`; timed events are suppressed rather than fired at unknown times; user is told sync is unavailable |
| **EC-14** | Participant joins mid-`PLAYING` | Receives a snapshot with the current anchor and joins the timeline in progress |
| **EC-15** | Trivia starts while a participant is disconnected | On resync they receive the active question with time remaining, and no answer key |
| **EC-16** | Same user joins from two devices | Both connections allowed; presence dedupes by user; trivia uniqueness is per `(session, user, question)`, so the second device cannot double-answer |
| **EC-17** | Deep link tapped with app killed and user logged out | Install/open → auth → resolve → land on the room; "room ended" gets a dedicated screen, not a generic error (§13.2.5) |
| **EC-18** | Provider (TMDB) is down | Browse and search work from local `content`; only refresh degrades (ADR-012) |

---

## 7. API Contracts

Authoritative source is `server/api/openapi.yaml` (OpenAPI 3.1). CI fails if the
spec and handlers diverge (§5.2). Shapes shown here in TypeScript notation.

```ts
// ---- Errors: RFC 9457 on every non-2xx ----
interface ProblemDetails {
  type: string;        // URI, e.g. "https://vybe.app/problems/duplicate-answer"
  title: string;       // Short, stable, human-readable
  status: number;
  detail: string;      // Instance-specific
  code: string;        // Stable machine-readable, e.g. "DUPLICATE_ANSWER"
  traceId: string;
  errors?: Array<{ field: string; code: string; detail: string }>;
}

// ---- POST /v1/rooms   (Idempotency-Key required) ----
interface CreateRoomRequest {
  contentId: string;                       // uuid
  visibility: "private" | "public";
  syncMode: "COMPANION" | "CLIP" | "ASYNC";
}
interface RoomResponse {
  id: string;
  joinCode: string;                        // 6-char Crockford base32
  shareUrl: string;                        // https://vybe.app/r/{code}
  state: "LOBBY" | "READY" | "PLAYING" | "ENDED";
  syncMode: "COMPANION" | "CLIP" | "ASYNC";
  content: ContentSummary;
  host: UserSummary;
  participants: UserSummary[];
  maxParticipants: number;
  currentSeq: number;
  anchor: { serverTime: string; offsetMs: number } | null;  // null until PLAYING
  createdAt: string;                       // RFC 3339 UTC, ms
}

// ---- POST /v1/rooms/{id}/join   (Idempotency-Key required) ----
// 200 RoomResponse · 403 not authorised · 404 no such code
// 409 ROOM_ENDED · 409 ROOM_FULL · 429 rate limited

// ---- POST /v1/realtime/ticket ----
interface WsTicketResponse {
  ticket: string;      // single-use, opaque
  expiresAt: string;   // now + 60s
  wsUrl: string;       // wss://…/v1/ws
}

// ---- POST /v1/trivia/{sessionId}/answers   (Idempotency-Key required) ----
interface SubmitAnswerRequest {
  questionId: string;
  optionId: string;
  nonce: string;       // issued to THIS user for THIS question
  clientTs: string;    // telemetry only — never used for scoring
}
interface SubmitAnswerAccepted {
  accepted: true;
  // Deliberately NO isCorrect and NO points here.
  // Both are revealed only in QUESTION_CLOSE, after the deadline.
}
// 409 DUPLICATE_ANSWER · 422 QUESTION_CLOSED · 403 INVALID_NONCE
// 403 NOT_PARTICIPANT · 404 QUESTION_NOT_FOUND · 429 RATE_LIMITED

// ---- WebSocket envelope (both directions) ----
interface Envelope<T = unknown> {
  v: 1;
  id: string;          // uuidv7 — client dedupe key
  room: string;
  seq: number;         // server-assigned, gap-free, monotonic per room
  type: string;
  ts: string;          // server clock, RFC 3339 UTC with ms
  actor: { id: string; role: "host" | "participant" | "system" } | null;
  payload: T;
}

// ---- Client → server ----
type ClientMessage =
  | { type: "PING";   payload: { t0: number } }
  | { type: "RESYNC"; payload: { room: string; lastSeq: number } }
  | { type: "HEARTBEAT"; payload: { room: string } }
  | { type: "CHAT_SEND";  payload: { room: string; body: string; clientId: string } }
  | { type: "REACTION_BATCH"; payload: { room: string; items: Array<{ emoji: string; timelineMs: number; count: number }> } }
  | { type: "DRIFT_REPORT"; payload: { room: string; observedDriftMs: number } };

// ---- Server → client (payloads, wrapped in Envelope) ----
interface PongPayload      { t0: number; t1: number; t2: number }
interface SyncArmPayload   { serverStartAt: string; contentId: string }
interface ReanchorPayload  { anchorServerTime: string; anchorOffsetMs: number; reason: "host_manual" | "drift_consensus" }
interface QuestionOpenPayload {
  questionId: string;
  text: string;
  options: Array<{ id: string; text: string }>;   // NO correct flag
  deadlineTs: string;
  timelineMs: number;
  nonce: string;                                   // unique per (user, question)
  // NOTE: no correctOptionId, no pointsFormula, no answerHash. Absent, not hidden.
}
interface QuestionClosePayload {
  questionId: string;
  correctOptionId: string;                         // revealed only now
  results: Array<{ userId: string; optionId: string | null; isCorrect: boolean; points: number }>;
}
interface SnapshotPayload {
  seq: number;
  state: "LOBBY" | "READY" | "PLAYING" | "ENDED";
  anchor: { serverTime: string; offsetMs: number } | null;
  serverNow: string;
  participants: UserSummary[];
  hostId: string;
  recentChat: ChatMessage[];                       // last 50
  reactionAggregates: Array<{ timelineBucket: number; emoji: string; count: number }>;
  activeTrivia: Omit<QuestionOpenPayload, "nonce"> & { nonce: string } | null;  // still no answer key
}
```

---

## 8. Out of Scope

| ID | Excluded | Reason |
|---|---|---|
| **OS-1** | Predictions | V1.5 (§2.2). Trivia proves the server-authoritative pattern; predictions reuse it. |
| **OS-2** | Leaderboards | V1.5. Every leaderboard is an anti-abuse surface (§2.3); the XP ledger must be trusted first. |
| **OS-3** | Activity feed | V1.5. Depends on the social graph, which this slice only seeds. |
| **OS-4** | Personalised ranking | V1.5 / ADR-007. Needs interaction data this slice generates. |
| **OS-5** | Clip Sync mode | V1.5. Companion Sync is the honest primary (ADR-002); Clip Sync needs licensed assets. |
| **OS-6** | Interactive stories | V2 (§2.3). An entire second product. |
| **OS-7** | Voice-note reactions | V1.5. Media pipeline plus moderation surface. |
| **OS-8** | VYBE+ entitlements enforcement beyond the participant cap | V2. `Entitlement` exists as a concept from day one (§1.8) and is checked for FR-16 only. |
| **OS-9** | Automatic prediction settlement | V2 integration. |
| **OS-10** | Any hosting, proxying, or embedding of licensed video | **Never** (§2.2). Non-negotiable. |
| **OS-11** | General-purpose DMs | **Never** (§2.2). Unbounded moderation surface. |
| **OS-12** | Public web app / desktop admin app | **Never** (§2.2). The moderation queue is an internal tool. |
| **OS-13** | Microservices | **Never** for this project (ADR-005). Extraction triggers are documented instead. |
| **OS-14** | Push notifications | M5. The slice is validated with both apps foregrounded. |
| **OS-15** | Multi-region | §7.9's ~10M tier. Documented, not built. |

---

## 9. Traceability

Every FR maps to at least one AC; every AC references at least one FR or NFR.
`tools/spec/check_traceability.py` enforces this in CI: an orphaned AC or an FR
with no AC fails the build.

| Requirement block | Acceptance criteria |
|---|---|
| FR-19 – FR-27 (sync ritual, timeline) | AC-1 – AC-6 |
| FR-28 – FR-35 (event log, resync) | AC-7 – AC-13 |
| FR-43 – FR-51 (trivia, anti-cheat) | AC-14 – AC-25 |
| FR-52 – FR-56 (XP) | AC-26, AC-27 |
| FR-41, FR-42, FR-14, FR-2 (authz, safety) | AC-28 – AC-31 |
| FR-60 – FR-62 (offline, i18n, a11y) | AC-32 – AC-35 |
| FR-1 – FR-18, FR-36 – FR-40, FR-57 – FR-59 | Covered by API integration suite; see `docs/TESTING.md` |
