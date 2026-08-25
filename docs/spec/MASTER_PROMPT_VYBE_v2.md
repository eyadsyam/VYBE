# VYBE — MASTER PROMPT v2.0
### Social Interactive Entertainment Platform · Flutter Mobile + Modular Monolith Backend

> **Document type:** Executable specification + AI agent operating contract.
> **Supersedes:** VYBE Master Prompt v1 (85-section feature list).
> **Read PART 0 first.** It defines how this document is to be used and what "done" means.

---

# PART 0 — AGENT OPERATING CONTRACT

## 0.1 Role

You are the **Lead Engineer and Architect** for VYBE. You own architecture, implementation, testing, and documentation. You are not a code generator that produces plausible-looking output; you are accountable for a system that a senior engineer could review line by line without finding fabricated functionality.

## 0.2 Decision authority

| Situation | Your action |
|---|---|
| One clearly correct technical option | Decide. Implement. Note it in `DECISIONS.md`. |
| 2+ defensible options (state mgmt, sync strategy, schema shape) | Write an ADR with a decision matrix, **pick one**, move on. Do not stall for approval. |
| Legal, cost, credentials, or brand ambiguity | **Stop and ask.** Batch questions, max 3 at a time. |
| Product-value ambiguity that changes scope by >1 week | Stop and ask, with a recommendation attached. |
| A requirement in this document is technically impossible or self-contradictory | Say so immediately, explain why, propose the closest achievable alternative. Do not silently reinterpret. |

## 0.3 Anti-fabrication rules (non-negotiable)

1. **Never invent third-party API behaviour.** If you do not know how a provider responds, write a small integration probe, run it, record the real response shape in `docs/INTEGRATIONS.md`, then build against that.
2. **Never mark a feature complete when mocked data sits behind real-looking UI.** Mocks are allowed only behind an explicit `DemoDataSource` that is visibly labelled in the app.
3. **Never claim a performance number you have not measured.** Write "target" until a benchmark exists, then write "measured on <device>, <date>".
4. **Never write a test that asserts the implementation rather than the behaviour.** A test that cannot fail is a lie.
5. **Never pin a library version from memory.** Resolve the current stable version at build time and record it.
6. **Never invent citations, benchmarks, or "industry standard" claims** in the documentation.

## 0.4 Stop conditions

Halt and report — do not continue to the next milestone — if any of these is true:

- A milestone's exit criteria cannot be met with real functionality.
- A legal/licensing blocker appears (see §1.9).
- Test coverage gates or static analysis gates fail and the fix is not mechanical.
- An architectural decision made earlier turns out to be wrong. Write a superseding ADR before continuing.

## 0.5 Output contract per work unit

Every milestone ends with exactly this, in this order:

1. **What was built** — feature list mapped to requirement IDs.
2. **Design decisions** — new/changed ADRs.
3. **Evidence** — test results, coverage delta, benchmark numbers, screenshots or a recording.
4. **What is NOT done** — honest gap list.
5. **Risks discovered** — added to the risk register.
6. **Next milestone's entry conditions.**

## 0.6 Anti-patterns that will fail review

These are the specific tells of a generated portfolio project. Treat each as a defect:

- God files (`home_screen.dart`, 1,400 lines).
- Business logic inside widgets.
- A README that claims capabilities no test exercises.
- "AI-powered recommendations" with no model, no evaluation, no offline metric.
- Real-time features demonstrated on one device.
- 40 screens, none of which survive a bad network.
- Copy-pasted architecture diagrams that do not match the code.
- Comments explaining *what* instead of *why*.

---

# PART 1 — PRODUCT

## 1.1 The thesis (one sentence)

> **VYBE is the social layer you run *alongside* whatever you're already watching** — it turns solitary streaming into a synchronised, competitive, shared event, without ever hosting a frame of copyrighted video.

## 1.2 The contradiction in v1, and how v2 resolves it

v1 said two incompatible things:

- §8: *"Do NOT illegally stream copyrighted movies or series."*
- §11–13: *"Synchronised playback watch parties with server-authoritative playback state."*

**You cannot synchronise playback of a stream you do not control.** v1 would have produced either a fake feature or a legal problem.

**v2's resolution — "sync the clock, not the stream":**

| Mode | What is synchronised | Content source | Fidelity |
|---|---|---|---|
| **A. Clip Sync** | Actual playback position | Trailers, clips, and micro-dramas VYBE is licensed to serve or embeds via a provider's sanctioned player | True frame sync |
| **B. Companion Sync** ⭐ *primary* | A **shared virtual timeline** — a server-authoritative clock, not a video stream | The user's own Netflix / Shahid / Disney+ / YouTube app, on the same or another screen | Sync ritual + drift correction |
| **C. Async Room** | Nothing live; room opens after everyone marks "watched" | Anything | No live sync needed |

**Companion Sync is the interesting engineering problem and the honest product.** The user presses play in their own app; VYBE runs a countdown ritual ("3… 2… 1… PLAY"), then maintains a shared timeline that every timed event — trivia beats, prediction windows, spoiler-safe chat, reaction bursts — fires against. Participants who drift report their offset and get a one-tap "you're 6s behind — resync" affordance.

This is genuinely harder than naive playback sync, is fully legal, works with every service, and is the single strongest talking point in the whole project.

## 1.3 Positioning

| Product | What it does | What VYBE does differently |
|---|---|---|
| Teleparty | Browser extension, syncs one service, desktop-only, chat only | Mobile-native, service-agnostic, game layer on top |
| Discord watch-together | Voice + screenshare, general purpose | Purpose-built for entertainment; trivia/predictions tied to the timeline |
| Letterboxd | Logging + reviews, async, no live layer | Live, social, competitive |
| Kahoot | Quiz, no content context | Trivia anchored to what you're watching, at the moment it's relevant |
| Amazon X-Ray | Metadata overlay, single-service, solo | Multi-service, multiplayer |

**Nobody owns "the multiplayer layer over any streaming service, on mobile, for MENA."** That is the wedge.

## 1.4 Market wedge (do not skip this)

Launch **Arabic-first, MENA-first**, then generalise.

Rationale: Ramadan musalsalat are the highest-synchronised-viewing event on earth per capita — millions of people watch the same episode the same night, and the social conversation is already happening on WhatsApp and X in a fragmented way. Egyptian, Gulf, and Levantine series drop on a known schedule. That is a ready-made shared timeline with a ready-made audience, and it is under-served by every product in the table above.

Consequences for the build:
- Arabic + RTL is a **launch requirement**, not a phase-20 nice-to-have (§3.6).
- Content metadata must handle Arabic titles, transliterations, and MENA providers — TMDB alone is insufficient; plan a supplementary catalogue source and a manual curation path.
- Ramadan schedule = a seasonal "Live Tonight" surface.

## 1.5 Users and jobs-to-be-done

| Persona | JTBD | Primary surface |
|---|---|---|
| **The Co-Watcher** (22, watches with 2–3 friends across cities) | "Make watching alone feel like watching together" | Rooms |
| **The Competitor** (19, trivia/quiz native, wants a scoreboard) | "Prove I know this show better than you" | Trivia, Leaderboards |
| **The Explorer** (27, decision fatigue, 20 min of browsing before giving up) | "Tell me what to watch tonight and why" | Home, Discover |
| **The Host** (24, organises the group) | "Get five people watching the same thing at the same time with zero friction" | Room creation, invites, deep links |

The Host is the **growth engine**. Every host acquisition brings 2–8 users. Optimise the host flow above everything else.

## 1.6 Core loop

```
DISCOVER  →  INVITE (host creates room, shares link)
              ↓
          SYNC RITUAL (countdown, everyone presses play in their own app)
              ↓
          SHARED TIMELINE (chat · reactions · timed trivia · predictions)
              ↓
          RESOLVE (scores settle, XP awarded, achievements fire)
              ↓
          ACTIVITY FEED (friends see it) → DISCOVER
```

The loop must close **without manipulative pressure**. Explicitly forbidden: streaks that punish, artificial scarcity timers, guilt notifications, infinite autoplay feeds, variable-ratio reward schedules on core actions.

## 1.7 Success metrics

**North Star:** *Synchronised Social Minutes* — minutes spent by ≥2 users in the same live room with the timeline running.

| Metric | Target (V1, 6 weeks post-launch) | Guardrail |
|---|---|---|
| Host conversion (room created / active user / week) | ≥ 12% | — |
| Room fill rate (rooms reaching ≥2 participants) | ≥ 60% | — |
| Median room duration | ≥ 25 min | — |
| D7 retention | ≥ 25% | — |
| Trivia completion (started → finished) | ≥ 70% | — |
| Notification opt-out rate | — | ≤ 15% |
| Reports per 1,000 room-hours | — | ≤ 3 |
| Crash-free sessions | ≥ 99.5% | — |

If a feature does not move a metric in this table, it does not ship in V1.

## 1.8 Monetisation (absent from v1 — required for credibility)

Not implemented in V1, but **designed for**, because it constrains the data model:

- **VYBE+** (subscription): unlimited room size (free tier caps at 4), custom trivia packs, room themes, ad-free activity feed.
- **Sponsored rooms / studio partnerships**: a studio hosts an official premiere room. This is the real business.
- **Never:** selling behavioural data, dark-pattern loot boxes, pay-to-win leaderboards.

Design implication: `Entitlement` is a first-class concept from day one, checked server-side. Do not bolt it on later.

## 1.9 Legal, compliance, and store-review risk

**This section can kill the project. Address it in Milestone 0, not at the end.**

| Risk | Severity | Mitigation |
|---|---|---|
| App Store / Play rejection for facilitating unauthorised viewing | **Critical** | Never embed, proxy, or hint at pirate sources. Never accept user-supplied video URLs. Deep-link only to official apps. Document this in the review notes. |
| TMDB (or equivalent) attribution + terms breach | High | Display required attribution, respect rate limits and caching terms, no bulk redistribution of their catalogue. Re-read the current terms before launch — they change. |
| Trademark on "VYBE" | High | Check trademark registries and app-store name availability **before** any design work. Have a fallback name. |
| URL scheme collision (`vybe://` is not unique) | Medium | Use **Universal Links / App Links** (`https://vybe.app/r/{code}`) as primary; custom scheme only as fallback. |
| GDPR + Egypt PDPL (Law 151/2020) | High | Lawful basis per data category, consent for analytics, export + delete endpoints, data map in `docs/PRIVACY.md`, breach process. |
| Minors in social rooms | **Critical** | Age gate at signup. Under-16 accounts: no public rooms, no DMs, no discoverability, restricted profile. Do not treat this as optional. |
| UGC liability (chat, room names, reports) | High | Notice-and-action process, retention of moderation records, human escalation path (§12.4). |

---

# PART 2 — SCOPE

## 2.1 V1 vertical slice — the one path that must be perfect

> A user signs up → finds a show → creates a room → shares a link → a friend joins on a second physical device → they run the sync ritual → they chat and react on the shared timeline → a 5-question trivia round runs with server-authoritative scoring → XP is awarded → one of them loses network for 30 seconds and rejoins with correct state.

**Nothing else ships until this path works end to end, on two real devices, on a bad network.** This slice alone demonstrates real-time systems, mobile architecture, offline resilience, server-authoritative game logic, and social features — which is the entire engineering signal.

## 2.2 Scope ladder

| Tier | Contents |
|---|---|
| **V1 (must)** | Auth + sessions · Content catalogue (metadata, trailers, where-to-watch) · Discover + Search · Content detail · Rooms (create/join/leave, Companion Sync, chat, reactions) · Real-time infra (envelope, resync, presence) · Trivia engine (server-authoritative) · XP + basic achievements · Friends (follow) · Push + deep links · Offline cache + outbox · Moderation primitives (report/block/mute) · Arabic + RTL · Observability |
| **V1.5 (should)** | Predictions · Leaderboards (weekly/friends) · Activity feed · Personalised ranking (two-stage) · Clip Sync mode · Room voice-note reactions |
| **V2 (could)** | Interactive stories engine · Async rooms · Collections/lists · Sponsored rooms · VYBE+ entitlements · Learning-to-rank recommender |
| **Never (in this project)** | Hosting or proxying licensed video · General-purpose DMs · A public web app · A desktop admin app · Microservices · A custom ML training platform |

## 2.3 Cut list — features from v1 deliberately removed or deferred, with reasons

| v1 feature | Verdict | Reason |
|---|---|---|
| Synchronised playback of licensed content | **Redesigned** → Companion Sync | Legally impossible; see §1.2 |
| Interactive stories (§23–24) | Deferred to V2 | An entire second product. Excellent, but it competes with the real-time slice for attention. Build it only after V1 is genuinely done. |
| 26 development phases | **Replaced** by 8 milestones | Phases with no exit criteria are not a plan |
| "Micro-dramas" as a first-class content type | Deferred | Requires licensing or original production |
| Full ML recommendation platform | Deferred | V1 ships a documented, evaluated heuristic ranker with a clean seam for a model |
| Genre-specific + global + monthly leaderboards | Reduced to weekly + friends | Every leaderboard is an anti-abuse surface |

## 2.4 Risk register (maintain this live)

| # | Risk | Impact | Likelihood | Mitigation | Owner |
|---|---|---|---|---|---|
| R1 | Companion Sync drift makes trivia beats fire at wrong moments | High | High | Drift measurement + one-tap resync + beat tolerance window (§7.4) | Eng |
| R2 | App store rejection | Critical | Medium | §1.9; pre-submission review notes; no pirate-adjacent surface | Lead |
| R3 | Metadata provider terms change or rate-limit | High | Medium | Adapter interface + local catalogue cache + second provider spike | Eng |
| R4 | Rooms are empty (cold-start social problem) | Critical | High | Host-first growth, invite links that work without an account install-gate, seeded public rooms during Ramadan schedule | Product |
| R5 | Trivia content pipeline (100+ good questions per title) | High | High | Start with 20 curated titles, not 500. Quality > volume. | Product |
| R6 | WebSocket costs at scale | Medium | Low | Documented scaling path (§7.9); not pre-optimised | Eng |

---

# PART 3 — EXPERIENCE

## 3.1 Information architecture

```
Tab 1  HOME       Continue · Live Tonight · Because you liked · Friends are watching · Quick trivia
Tab 2  DISCOVER   Search · Genres · Collections · Public rooms · Trending
Tab 3  ROOMS ⭐    Active room (if any) · Invites · Create · Recent rooms
Tab 4  ACTIVITY   Friend activity · Notifications · Achievements earned
Tab 5  PROFILE    Stats · XP · Achievements · Lists · Friends · Settings
```

Five tabs is the ceiling. **Rooms sits centre** and is visually emphasised — it is the product.

## 3.2 Screen state matrix (mandatory for every data-backed screen)

Every screen implements all applicable states. A screen missing a state is not done.

| State | Requirement |
|---|---|
| Loading (first) | Skeleton matching final layout, not a spinner |
| Loading (refresh) | Existing content stays visible; subtle indicator |
| Empty | Illustration + one-sentence explanation + one primary action |
| Error (retryable) | Cause in plain language + retry button |
| Error (terminal) | What happened + what to do + support path |
| Offline | Banner + cached content clearly marked stale with age ("updated 2h ago") |
| Unauthorised | No silent redirect; explain, then offer sign-in |
| Not found / Room ended / Invite expired | Dedicated screens, not a generic error |
| Rate limited | Time until retry, not "try again later" |

**Never present stale data as live.** Every cached surface carries a freshness indicator (§11.2).

## 3.3 The sync ritual (design this carefully — it is the product's first impression)

```
Host taps "Start"
  → Server sets room state READY, broadcasts SYNC_ARM with server_start_at (T+7s)
  → Every client shows: "Open Netflix. Queue the episode. Don't press play."
  → 3-second full-screen countdown, driven by the server clock, not local time
  → "PLAY" — haptic + audio cue on every device simultaneously
  → Room enters PLAYING; shared timeline begins at 00:00
  → Persistent mini-bar: timeline position · participant count · "I'm out of sync" button
```

Drift affordance: tapping "I'm out of sync" offers **±5s nudge** (adjusts only that user's local offset) or **request host resync** (host announces their true position, server re-anchors the timeline for everyone).

## 3.4 Motion budget

| Allowed | Forbidden |
|---|---|
| Hero transitions on poster → detail (≤300ms) | Any blocking animation >400ms |
| Reaction bursts (GPU-cheap, capped at 20 concurrent particles) | Continuous background animation |
| Trivia countdown ring, achievement unlock (≤600ms, skippable) | Animation that gates input |
| List item stagger ≤ 150ms total | Parallax on scroll-heavy feeds |

All motion respects `MediaQuery.disableAnimations` / reduced-motion.

## 3.5 Accessibility contract

- Every interactive element has a semantic label; icon-only buttons always.
- Minimum touch target 48×48dp.
- Text scales to 200% without truncation or overlap on all V1 screens.
- Contrast ≥ 4.5:1 body, ≥ 3:1 large text and meaningful icons.
- **Never colour-only:** trivia correct/incorrect uses icon + text + colour; sync status uses label + icon.
- Countdowns announce via `SemanticsService.announce` at 10s, 5s, and 0.
- Live regions for incoming chat, throttled to avoid screen-reader flooding.
- Automated: `flutter test` + accessibility guideline assertions in widget tests. Manual: one TalkBack and one VoiceOver pass per milestone, logged.

## 3.6 Internationalisation and RTL contract

- No hardcoded user-facing strings. `.arb` files, `flutter gen-l10n`. CI fails on a literal string in a widget file (lint rule).
- Arabic (`ar`) and English (`en`) both ship in V1.
- Every layout uses directional properties (`EdgeInsetsDirectional`, `start`/`end`), never `left`/`right`.
- Icons with direction meaning (back, next, seek) mirror.
- Numerals: Eastern Arabic vs Western Arabic numeral preference is a user setting.
- Dates, durations, and relative times via `intl`, with Hijri calendar display option.
- **CI gate:** golden tests render every V1 screen in `ar` RTL. A layout regression in RTL fails the build.

---

# PART 4 — FLUTTER ARCHITECTURE

## 4.1 Layers

```
Widgets (dumb, declarative)
      ↕  watch state / call intents
ViewModel  (presentation state, no I/O, no business rules)
      ↕
Use Case   (business rules; only where behaviour is non-trivial)
      ↕
Repository (the source of truth for a domain concept; owns cache policy)
      ↕
Data Source (Remote API · Local DB · Secure Storage · Realtime · Device)
```

Rules:
- Widgets contain **zero** business logic and zero direct repository access.
- ViewModels expose an immutable, exhaustive state object — not a bag of nullable fields. Use sealed classes / `freezed` unions: `Loading | Data | Empty | Failure(reason)`.
- Repositories are the only place cache/network arbitration happens.
- Data sources know nothing about domain models; mapping happens at the repository boundary.
- **Dependencies point inward.** The domain layer imports no Flutter, no Dio, no Drift.

## 4.2 Folder structure

```
lib/
  app/                    bootstrap, DI wiring, router, theme, l10n
  core/
    error/                Failure hierarchy, Result<T>
    network/              client, interceptors, retry, idempotency
    realtime/             socket client, envelope, resync engine
    storage/              db, secure storage, cache policy
    ui/                   design system: tokens, components, state widgets
    utils/
  features/
    <feature>/
      data/               dto, mappers, remote_ds, local_ds, repo impl
      domain/             entities, repo interfaces, use cases
      presentation/       view models, screens, widgets
  l10n/
test/  unit/ · widget/ · golden/ · integration/
```

A feature never imports another feature's `data/` or `presentation/`. Cross-feature needs go through `domain/` interfaces or a shared `core/` contract. **Enforce with an import lint rule in CI** — this is the difference between a real modular codebase and a claim of one.

## 4.3 State management — decide, don't default

Score the candidates against these weighted criteria and record the result as ADR-001:

| Criterion | Weight | Why it matters here |
|---|---|---|
| Async/stream state ergonomics | 25% | Realtime rooms are stream-heavy |
| Testability without a widget tree | 20% | Domain logic must be unit-testable |
| Compile-time safety of DI | 15% | Large feature count |
| Disposal / lifecycle correctness | 15% | Sockets and timers must not leak |
| Boilerplate per feature | 15% | Solo/small team velocity |
| Ecosystem + hiring familiarity | 10% | This is also a portfolio artefact |

Riverpod and Bloc both clear the bar. **Pick one, justify it against the table, and never mix paradigms in the same layer.** The wrong answer is "we used Provider here and Bloc there."

## 4.4 Error model

```dart
sealed class Failure {
  const Failure();
}
final class NetworkFailure   extends Failure { final bool isOffline; }
final class ServerFailure    extends Failure { final int status; final String code; }
final class AuthFailure      extends Failure { final AuthReason reason; }
final class ValidationFailure extends Failure { final Map<String, String> fields; }
final class RateLimitFailure extends Failure { final Duration retryAfter; }
final class ConflictFailure  extends Failure { final ConflictKind kind; }
final class UnexpectedFailure extends Failure { final Object cause; }
```

- Repositories return `Result<T, Failure>`. No exceptions cross the repository boundary.
- Every `Failure` maps to a user-facing message via a single `FailurePresenter` — **one place** where technical errors become human sentences, and the only place they get localised.

## 4.5 Performance budgets (targets until measured)

| Metric | Target | Measured on |
|---|---|---|
| Cold start to first frame | < 2.5s | Mid-tier Android, 4GB RAM |
| Home feed scroll | p95 frame build < 16ms, zero jank frames over 5s scroll | Same |
| Room event → UI update | < 100ms after socket receipt | Same |
| Memory, 30-min room session | < 250MB steady | Same |
| App size (Android, split ABI) | < 30MB | — |

Enforcement: a performance smoke test in CI (integration test + `flutter driver` timeline capture) on the Home feed and the Room screen. Regressions fail the build.

## 4.6 Flutter engineering rules

- `const` constructors everywhere possible; `const` lint enabled as an error.
- No `setState` in a widget that owns business state.
- Lists: `ListView.builder` + `itemExtent`/`prototypeItem` where uniform; `AutomaticKeepAlive` only where justified.
- Images: single caching image widget wrapper with explicit memory cache dimensions, disk cache cap, and a shimmer placeholder. Never load a 2000px poster into a 120px slot.
- Selective rebuilds: watch the narrowest slice of state; measure with the rebuild counter in debug.
- Isolates for JSON >100KB and for any local ranking computation.
- Timers, subscriptions, controllers: disposed in every ViewModel, verified by a leak test.

---

# PART 5 — BACKEND ARCHITECTURE

## 5.1 Shape: modular monolith with enforced boundaries

Modules: `identity` · `users` · `social` · `catalog` · `discovery` · `rooms` · `realtime` · `games` (trivia + predictions) · `progression` (XP, achievements, leaderboards) · `notifications` · `moderation` · `analytics` · `entitlements`

Boundary rules:
- Each module owns its tables. **No cross-module table reads.** Cross-module data flows through a published interface or a domain event.
- Each module exposes: a public API (facade), domain events it publishes, and events it consumes.
- Enforce with a package/module dependency check in CI. An unenforced boundary is a comment.

This gives a genuine extraction path to services later without paying distributed-systems tax now (ADR-005).

## 5.2 API conventions

| Concern | Decision |
|---|---|
| Versioning | URL prefix `/v1`. Additive changes only within a version. |
| Pagination | **Cursor-based** (opaque, encodes sort key + id). Never offset — feeds mutate underneath. |
| Errors | RFC 9457 `application/problem+json`: `type`, `title`, `status`, `detail`, `code`, `traceId`, plus `errors[]` for field validation. Stable machine-readable `code` values. |
| Idempotency | `Idempotency-Key` header **required** on all non-GET mutations. Server stores key → response for 24h and replays. |
| Caching | `ETag` + `If-None-Match` on catalogue reads; `Cache-Control` with `stale-while-revalidate`. |
| Time | All timestamps RFC 3339 UTC with milliseconds. Never local time. |
| IDs | UUIDv7 (time-ordered — better index locality than v4, no PK enumeration). |
| Contract | OpenAPI 3.1 is the **source of truth**; client models generated from it. CI fails if spec and handlers diverge. |

## 5.3 Authentication and session model

- Access token: JWT, 15 min, asymmetric signing, minimal claims (`sub`, `sid`, `entitlement_tier`).
- Refresh token: opaque, 60 days, **rotating with reuse detection** — a replayed refresh token revokes the entire family and alerts.
- Sessions are first-class rows: device name, platform, last-seen IP (truncated), created-at. User can list and revoke.
- Realtime auth: a **short-lived (60s), single-use WebSocket ticket** issued over HTTPS. Never send the access token as a query parameter — it lands in logs.
- Logout revokes the session; account deletion cascades per the retention policy (§6.5).
- Password reset: single-use, 15-min token, invalidates all sessions on success.

## 5.4 Cross-cutting infrastructure

- **Transactional outbox** for every side effect (push notification, analytics event, achievement evaluation). Write the domain change and the outbox row in one transaction; a worker publishes. This is what makes "achievement unlocked exactly once" true.
- **Background jobs**: scheduled (leaderboard rollups, cache warming, stale room reaping) and queued (push fan-out, moderation scans). Retries with exponential backoff + jitter, dead-letter queue with alerting.
- **Migrations**: forward-only, reversible where possible, never destructive in the same release as the code change. Expand → migrate → contract, across three deploys.
- **Feature flags**: server-evaluated, user-bucketed, with a kill switch for every realtime feature.

---

# PART 6 — DATA

## 6.1 Schema principles

1. Normalise transactional data; denormalise **only** where a measured query demands it, with a documented refresh path.
2. Every table: `id` (UUIDv7), `created_at`, `updated_at`. Soft-delete only where the domain requires recovery.
3. Foreign keys enforced. `ON DELETE` behaviour chosen deliberately per relationship, not defaulted.
4. Money and scores: integers, never floats.
5. **Every index exists because of a named query.** Document the query in a comment on the migration. No speculative indexes.
6. Enum-like values: Postgres enums for closed sets that rarely change; lookup tables for sets that product will edit.

## 6.2 Notable modelling decisions

| Entity | Decision | Why |
|---|---|---|
| `content` | Single table + `content_type` discriminator + JSONB `provider_metadata`, with generated columns for hot fields | Movies/series/episodes share 80% of fields; provider payloads vary and change |
| `watch_progress` | One row per (user, content), updated in place. Position written on pause/background/exit and at most every 30s | v1's "don't update every frame" made concrete |
| `room_events` | Append-only, partitioned monthly, `(room_id, seq)` unique | This is the event log that makes resync possible (§7.3) |
| `reactions` | **Not** one row per tap. Aggregated: `(room_id, timeline_bucket, emoji, count)` | v1 §18 required this; here is the shape |
| `chat_messages` | Persisted, with `deleted_at` + `deleted_by` for moderation audit | Legal requirement (§1.9) |
| `trivia_answers` | Unique on `(session_id, user_id, question_id)` | The database enforces the anti-cheat rule, not just the code |
| `xp_ledger` | **Append-only ledger**, never a mutable `users.xp` counter | Auditable, reversible on abuse detection, reconcilable |
| `user_achievements` | Unique on `(user_id, achievement_id)`, granted via outbox | Exactly-once by construction |

## 6.3 Indexing plan (illustrative — derive the rest from real queries)

```sql
-- Feed: a user's continue-watching, newest first
CREATE INDEX idx_wp_user_recent ON watch_progress (user_id, last_watched_at DESC)
  WHERE completed = false;

-- Room replay after reconnect
CREATE INDEX idx_room_events_seq ON room_events (room_id, seq);

-- Friend activity feed
CREATE INDEX idx_activity_actor_time ON activity (actor_id, created_at DESC);

-- Search: trigram for fuzzy, plus tsvector for full text (multilingual)
CREATE INDEX idx_content_title_trgm ON content USING gin (title gin_trgm_ops);
CREATE INDEX idx_content_fts ON content USING gin (search_vector);
```

## 6.4 Search

- Postgres-native for V1: `tsvector` (with an Arabic-aware configuration) + `pg_trgm` for fuzzy/typo tolerance + prefix matching for as-you-type.
- Ranking: exact prefix > title trigram similarity > cast/crew > description, with a popularity tiebreak.
- Arabic requires deliberate handling: normalise hamza forms (أ إ آ → ا), tāʾ marbūṭa (ة → ه), alif maqṣūra (ى → ي), and strip diacritics before indexing **and** before querying. Without this, Arabic search silently fails and you will not notice in English testing.
- Document the migration trigger to a dedicated search engine (§14.3) — do not adopt one in V1.

## 6.5 Data retention and PII map

| Category | Retention | Notes |
|---|---|---|
| Account + profile | Life of account + 30d grace | Export on request |
| Chat messages | 90 days, or life of room if shorter | Longer for messages under active report |
| Room events | 30 days, then aggregate-only | Storage control |
| Reaction aggregates | 12 months | |
| Moderation records | 2 years | Legal defensibility |
| Analytics events | 13 months, pseudonymised after 90 days | |
| Auth logs | 12 months, IP truncated | |
| Deleted accounts | Hard-deleted at 30 days; moderation records retained pseudonymously | |

Maintain `docs/PRIVACY.md` with a field-level PII inventory: what, why, lawful basis, retention, who can access.

---

# PART 7 — REALTIME (the crown jewel)

## 7.1 Principles

1. **The server is authoritative for all shared state.** The client renders; it does not decide.
2. **Every shared-state change is an event in an ordered, replayable log.**
3. **The client can always recover** by requesting a snapshot and replaying deltas.
4. **Degrade, don't die.** Realtime failure falls back to polling, not a broken screen.

## 7.2 Event envelope

```jsonc
{
  "v": 1,                       // envelope version — evolve without breaking old clients
  "id": "01J8...",              // UUIDv7, for dedupe
  "room": "01J7...",
  "seq": 1482,                  // monotonic per room — the backbone of resync
  "type": "TRIVIA_QUESTION_OPEN",
  "ts": "2026-08-25T19:04:11.230Z",   // server time, authoritative
  "actor": { "id": "01J6...", "role": "host" },
  "payload": { /* type-specific, additive-only */ }
}
```

Rules:
- `seq` is assigned by the server, gap-free per room. A client that sees a gap **must** resync.
- Unknown `type` → ignore and log, never crash. This is how you ship client updates independently.
- Payloads evolve by adding optional fields only. Breaking changes bump `v` and are served in parallel for one release cycle.

## 7.3 Resync protocol

```
Client detects: gap in seq · reconnect · resume from background · >30s silence
        ↓
  Send RESYNC { room, last_seq }
        ↓
  Server decides:
     gap ≤ 200 events and within retention  →  send delta: events (last_seq, current]
     otherwise                              →  send SNAPSHOT: full room state + seq
        ↓
  Client applies, discards local optimistic state that conflicts, resumes
```

Snapshot contains: room state machine position, timeline anchor + server time, participants, last 50 chat messages, reaction aggregates, active trivia/prediction state (**without** answer keys), host identity.

**The client never assumes its pre-disconnect state is still valid.** That assumption is the single most common bug in real-time apps.

## 7.4 The shared timeline (Companion Sync)

The timeline is `t = (server_now − anchor_server_time) + anchor_offset`, adjusted per-client by a measured clock offset.

Clock offset measurement (simplified NTP handshake) on connect and every 60s:
```
client → PING  { t0: client_clock }
server → PONG  { t0, t1: server_recv, t2: server_send }
client         t3 = client_clock
  rtt    = (t3 − t0) − (t2 − t1)
  offset = ((t1 − t0) + (t2 − t3)) / 2
```
Keep a rolling window of 5 samples; use the sample with the **lowest RTT** (not the mean — high-RTT samples are noise). Discard if RTT > 2s and mark the connection degraded.

Timeline events (trivia beats, prediction windows, spoiler-gated chat) carry a `fires_at_timeline_ms`. Clients schedule locally against their corrected clock. Tolerance window: an event is valid if it fires within ±1.5s of target; outside that, it is skipped and logged as drift.

Drift handling:
- Client self-reports observed drift when the user nudges.
- If >40% of participants report drift in the same direction >8s, prompt the host to re-anchor.
- Re-anchor is an event (`TIMELINE_REANCHOR`) — everyone converges.

## 7.5 Presence

- Heartbeat every 15s. Redis key `presence:{room}:{user}` with 45s TTL.
- Disconnect → **30s grace** before broadcasting `PARTICIPANT_LEFT`. Mobile users go through tunnels; do not eject them.
- Presence is derived state, never the source of truth for membership — membership lives in Postgres.
- On server restart, presence rebuilds from reconnections; rooms survive because their state is in Postgres.

## 7.6 Backpressure and coalescing

| Event class | Policy |
|---|---|
| Reactions | Client batches 250ms → server aggregates 1s buckets → broadcast counts, not individual events |
| Presence | Coalesce; max 1 broadcast per room per 2s |
| Chat | Per-user rate limit 5/10s, burst 3; server-side |
| Playback / timeline / trivia | **Never dropped, never coalesced** |

Per-connection outbound queue cap (e.g., 256 messages). On overflow: drop coalescable classes first; if a critical event would be dropped, **force the client to resync** rather than deliver an inconsistent stream.

## 7.7 Failure behaviour matrix

| Failure | Client behaviour | Server behaviour |
|---|---|---|
| Socket drop | Exponential backoff reconnect (1s → 30s, ±30% jitter), UI shows "reconnecting", timeline keeps running locally | Grace period, then presence expiry |
| App backgrounded | Socket closed after 30s; timeline persists; resync on resume | Normal presence expiry |
| Network switch (WiFi→LTE) | Immediate reconnect attempt, ticket refresh | New connection, same session |
| Server restart | Reconnect + RESYNC | State rehydrates from Postgres |
| Host disconnects >60s | UI shows "waiting for host" | Auto-promote longest-tenured participant, broadcast `HOST_CHANGED` |
| Room abandoned | — | Reaper job ends rooms with 0 participants for 10 min |
| Duplicate event received | Dedupe by envelope `id` (LRU of last 500) | — |

## 7.8 Realtime authorisation

Every event is filtered per-recipient. Never broadcast then filter client-side.
- Verify on connect: identity, then per-subscribe: room membership, room visibility, ban/block status.
- **Blocked users:** a user who has blocked another must not receive their chat, reactions, or presence — filtered server-side at fan-out.
- Host-only actions (`PLAY`, `REANCHOR`, `TRIVIA_START`, `KICK`) are re-verified server-side on every request. Client role is a UI hint with zero authority.

## 7.9 Scaling path (document; do not implement early)

| Scale | Architecture |
|---|---|
| ~1K users | Single app instance, in-process WebSocket hub, Postgres, Redis for presence |
| ~100K | Horizontal instances behind an LB, **Redis Pub/Sub** for cross-instance fan-out, read replica for catalogue, CDN for images |
| ~1M | Room-affinity routing (consistent hash on room_id) to cut fan-out chatter, dedicated realtime tier, partitioned event log, materialised feeds, async analytics pipeline |
| ~10M | Multi-region with regional room pinning, extract `realtime` and `games` modules to services, event streaming backbone, sharded Postgres by user_id, edge-terminated WebSockets |

**Migration triggers**, not timelines: extract the realtime tier when p95 fan-out latency exceeds 200ms or realtime CPU exceeds 60% of total. State the trigger, not the date.

---

# PART 8 — GAME SYSTEMS

## 8.1 Trivia protocol (server-authoritative)

```
1. Host starts round        → server loads questions, creates session
2. Server broadcasts QUESTION_OPEN:
     { question_id, text, options:[{id,text}], deadline_ts, timeline_ms, nonce }
     ⚠ NO correct answer. NO points formula. Not present in the payload at all.
3. Client renders countdown against corrected server clock
4. Client submits ANSWER { session_id, question_id, option_id, nonce, client_ts }
     with an Idempotency-Key
5. Server validates:
     a. session active, user is a participant
     b. question is currently open
     c. server_receive_time ≤ deadline + grace(400ms)
     d. nonce matches the one issued to THIS user for THIS question
     e. no prior answer exists (DB unique constraint — belt and braces)
6. Server scores:
     base = question.points
     speed_bonus = round(base * 0.5 * max(0, 1 − elapsed/time_limit))
     elapsed = server_receive_time − question_open_time − (rtt/2), clamped ≥ 0
7. Server broadcasts QUESTION_CLOSE with correct answer + per-user results
8. Server updates leaderboard, writes to xp_ledger via outbox
```

**Why each rule exists** — say this in the interview:
- The answer key never reaches the client, so a modified client cannot read it.
- Timing is measured server-side, so a manipulated device clock changes nothing.
- RTT compensation means a user on a slower network is not penalised — fair, and it is the detail that shows you thought about real users.
- The nonce prevents replaying another user's captured request.
- The unique constraint means a race between two concurrent submissions still yields one answer.

## 8.2 Anti-cheat test suite (mandatory, all must pass)

| # | Attack | Expected |
|---|---|---|
| 1 | Submit the same answer twice | Second rejected `409 DUPLICATE_ANSWER`, score unchanged |
| 2 | Submit after deadline + grace | `422 QUESTION_CLOSED` |
| 3 | Submit for another session's question | `403` |
| 4 | Submit another user's nonce | `403 INVALID_NONCE` |
| 5 | Device clock set 60s back | Score identical (server timing) |
| 6 | 100 concurrent submissions, same user/question | Exactly one accepted |
| 7 | Inspect `QUESTION_OPEN` payload | Contains no correct-answer field |
| 8 | Submit for a question never opened | `404` |
| 9 | Non-participant submits | `403` |
| 10 | Answer flood (50 req/s) | Rate limited after threshold, session flagged |

## 8.3 Predictions

- Lifecycle: `DRAFT → OPEN → LOCKED → SETTLED | VOIDED`.
- `locks_at` is server time. Submissions after lock are rejected — no grace (unlike trivia, where grace compensates network; here, late information is the cheat).
- Settlement: manual (host/moderator) for V1.5, with an audit record of who settled and when. Automatic settlement is a V2 integration.
- `VOIDED` refunds all stakes — needed when a prediction becomes unresolvable.
- Participant counts and option splits are hidden until lock, to prevent herding.

## 8.4 XP economy and anti-farming

XP is an **append-only ledger** (`xp_ledger`: user, source_type, source_id, amount, created_at). The user's total is a materialised sum, refreshable from the ledger.

| Rule | Value |
|---|---|
| Only server-verified terminal events award XP | e.g. `TRIVIA_ROUND_COMPLETED`, not `ANSWER_SUBMITTED` |
| Idempotent by `(source_type, source_id)` | Unique constraint |
| Daily cap per source | Trivia 500 · Rooms 300 · Social 150 · Content 400 |
| Diminishing returns on repeats | nth same-source action worth `round(base × 0.75^(n−1))`, floor 1 |
| Room XP requires ≥2 distinct participants and ≥5 min duration | Kills solo-room farming |
| Cooldown on repeated identical actions | 60s |
| Suspicious pattern → **shadow-scored** | XP recorded but withheld from leaderboards pending review — never auto-ban |

Detection signals (score, don't punish, in V1): rooms with participants sharing a device fingerprint, trivia answer latency below human floor (<250ms consistently), perfect scores at maximum speed across sessions, XP velocity >3σ above cohort.

## 8.5 Achievements as a rules engine

```
Domain event → Outbox → Achievement Evaluator → rule match → grant (idempotent) → notification
```

- Rules are **data**, not code: `{ id, trigger_event, predicate, window, threshold, reward_xp }`. Adding an achievement must not require a deploy.
- Evaluation is idempotent; the unique constraint on `(user_id, achievement_id)` is the final guard.
- **Never** evaluate achievements inside a UI screen or a request handler (v1 §32 was right; this is the mechanism).
- Backfill capability: a new achievement can be granted retroactively by replaying historical events.

## 8.6 Leaderboards

- V1: **weekly global** + **friends**, ranking metric = XP earned within the period (not lifetime — lifetime leaderboards freeze and demotivate).
- Ties broken by earliest achievement of the score, then by user id (deterministic).
- Computed by a scheduled rollup into a `leaderboard_snapshot` table; served from cache with a 60s TTL. Never computed live on read.
- Reset: new period starts Monday 00:00 in the user's timezone-anchored region; the prior period is archived and viewable.
- Shadow-scored users are excluded from public boards but still see their own rank.

---

# PART 9 — INTERACTIVE STORIES (V2 — spec now, build later)

## 9.1 Story as validated data, never as code

Stories are authored as JSON conforming to a published schema, stored in the DB, and executed by a generic runtime. **No story-specific logic in any widget.**

```jsonc
{
  "story_id": "...", "version": 3, "locale": "ar",
  "start_scene": "s1",
  "variables": { "trust": { "type": "int", "initial": 0 } },
  "scenes": [{
    "id": "s1",
    "media": { "type": "video", "ref": "..." },
    "text": "...",
    "choices": [
      { "id": "c1", "label": "...", "next": "s2", "effects": [{ "var": "trust", "op": "+", "value": 1 }] },
      { "id": "c2", "label": "...", "next": "s3", "condition": "trust >= 2" }
    ]
  }],
  "endings": [{ "scene": "s9", "achievement": "..." }]
}
```

## 9.2 Graph validation (CI gate — a story that fails does not publish)

1. Every `next` resolves to an existing scene.
2. Every scene is reachable from `start_scene`.
3. Every terminal scene is declared as an ending, or has choices.
4. No unintended cycles (cycles must be explicitly flagged `"loop": true`).
5. Every variable referenced in a condition is declared.
6. Every media ref resolves.
7. Every locale variant has identical graph topology — translations cannot change structure.

## 9.3 Runtime

- Engine holds an immutable `StoryState { scene_id, variables, path[] }`. Choices produce a new state — pure function, trivially unit-testable.
- Progress persisted per (user, story, version). A story version bump either migrates the path or restarts with the user's consent — never silently corrupts.
- Analytics: choice distribution per decision point, ending distribution, drop-off scene. This is what makes an interactive story product interesting rather than a gimmick.

---

# PART 10 — RECOMMENDATIONS

## 10.1 Two-stage architecture (with a seam for a model)

```
Stage 1 — CANDIDATE GENERATION (fast, recall-oriented, ~500 items)
   trending pool · genre-affinity pool · friends-watching pool ·
   continue-watching pool · similar-to-recent pool · editorial pool
        ↓
Stage 2 — RANKING (precise, ~50 items)
   weighted linear score (V1)  →  replaceable by a learned model (V2)
        ↓
Stage 3 — RE-RANK for diversity, freshness, and business rules
```

The V1 ranker is a documented, evaluated heuristic. **The seam matters more than the algorithm** — a `Ranker` interface with `rank(candidates, userFeatures, context)` means swapping in a model later touches one class.

## 10.2 The V1 ranking function

```
score = 0.30 · genre_affinity          // normalised watch time per genre, 90d decay
      + 0.20 · popularity_z            // z-score within cohort, prevents blockbuster domination
      + 0.15 · social_signal           // friends who watched/rated, log-scaled
      + 0.15 · recency_boost           // exp(−days_since_release / 45)
      + 0.10 · context_fit             // time of day, session length, device
      + 0.10 · completion_prior        // historical completion rate of similar items
      − 0.40 · already_seen_penalty    // hard suppression unless "continue watching"
      − 0.15 · fatigue_penalty         // impressions without click, 7d window
```

All features normalised to [0,1]. **Weights are a hypothesis, not a truth** — they must be recorded in config (not code) and tuned against the offline harness (§10.4).

## 10.3 Diversity, exploration, and guardrails

- **MMR re-rank**: `λ=0.7` relevance vs 0.3 dissimilarity, so the feed is not eight action films.
- **Genre cap**: max 3 consecutive items from one genre; max 40% of a shelf from one genre.
- **Exploration**: ε-greedy at 10% — one in ten slots goes to a high-uncertainty item. Without exploration the system only ever learns what it already believes.
- **Cold start**: new user → onboarding genre picks (3–5) + regional trending + editorial. Never an empty feed. New content → editorial boost for 14 days, then earn its place.
- **Filter-bubble guardrail**: track genre entropy per user over 30 days. If it falls below a threshold, increase exploration for that user.

## 10.4 Evaluation (this is what separates a real recommender from a claim)

- **Offline harness**: replay historical interactions, hold out the last 20%, compute **Precision@10, Recall@50, nDCG@10, catalogue coverage, intra-list diversity**. Any weight change must be run through this before shipping.
- **Baselines to beat**: (a) random, (b) global popularity, (c) genre-only. If the ranker does not beat popularity on nDCG@10, it is not earning its complexity — say so honestly in the docs.
- **Online**: CTR on recommended shelves, completion rate of recommended items, "not interested" rate. Instrument slot position so you can compute position-debiased CTR.
- **Explainability**: every recommended item carries a `reason` (`because_you_watched:{id}`, `friends_watching`, `trending_in_region`, `editorial`). Surface it in the UI. Users trust recommendations they understand, and it makes the system debuggable.

---

# PART 11 — OFFLINE AND SYNC

## 11.1 Cache tiers

| Tier | Contents | TTL | Eviction |
|---|---|---|---|
| Hot (memory) | Current screen state, active room | Session | On dispose |
| Warm (SQLite/Drift) | Catalogue metadata, feeds, watch progress, profile | 24h soft | LRU, 200MB cap |
| Cold (disk) | Images, avatars, posters | 7d | LRU, 300MB cap |
| Secure | Tokens, session id | — | Cleared on logout |

## 11.2 Freshness contract

Every cached entity carries `fetched_at` and a `Freshness` value:

- `Fresh` — within TTL, render normally.
- `Stale` — past TTL, render **with a visible age indicator**, revalidate in background (stale-while-revalidate).
- `Expired` — past hard limit, render only with an explicit "offline — showing old data" banner.

**The UI must never present stale data as live.** This is the concrete implementation of v1 §38.

## 11.3 Outbox (offline actions)

Queued while offline: watch progress, favourites, list changes, follows, profile edits, reports, read receipts.

**Never queued** (they are meaningless later): room join, chat message, reaction, trivia answer, prediction. A trivia answer submitted three hours late is not an answer. Show "action requires connection" — do not fake success.

```
Local mutation → optimistic UI + outbox row (idempotency key generated at enqueue)
       ↓ on connectivity
   Authenticate → submit in causal order → server validates
       ↓
   200/201 → mark synced        409 conflict → resolve (§11.4)
   4xx     → drop + notify user  5xx → backoff retry (max 5, then dead-letter)
```

The idempotency key is generated **when the action is queued**, not when it is sent — so a retry after a timeout that actually succeeded does not double-apply.

## 11.4 Conflict resolution — per entity, not one global rule

| Entity | Strategy |
|---|---|
| Watch progress | **Max position wins** (monotonic — you cannot un-watch) |
| Favourites / lists | Last-write-wins by client timestamp, server tie-break |
| Profile fields | Server wins on conflict; user is shown the diff and can re-apply |
| Follows | Idempotent set semantics — no conflict possible |
| Reports | Append-only — never conflicts |
| Anything the server computes (XP, scores) | **Server always wins, unconditionally** |

## 11.5 Storage discipline

- Hard cap on total app storage (~500MB); LRU eviction when exceeded, with a "manage storage" screen.
- No unbounded log tables on device.
- Cache is invalidated on: logout, account switch, schema version bump, explicit user clear.

---

# PART 12 — SECURITY, PRIVACY, TRUST & SAFETY

## 12.1 Threat model (STRIDE, per surface)

| Surface | Primary threat | Control |
|---|---|---|
| Auth endpoints | Credential stuffing, brute force | Rate limit by IP+account, exponential lockout, breached-password check, CAPTCHA after N failures |
| Refresh tokens | Theft, replay | Rotation with reuse detection → revoke family + alert |
| WebSocket | Unauthorised room access, event injection | Single-use ticket, per-subscribe authorisation, server-side fan-out filtering |
| Trivia / predictions | Score manipulation | §8.1, §8.2 |
| XP / achievements | Farming, duplication | §8.4, idempotent grants, ledger |
| Chat / room names | Injection, abuse, spam | Server-side sanitisation, rate limits, moderation pipeline |
| Deep links | Unauthorised access via crafted link | Server authorises on resolve, never on the client |
| Client binary | Reverse engineering | **Assume it is fully compromised.** No secrets in the app, ever. All authority server-side. |
| Media uploads (avatars) | Malicious file, EXIF leakage | Type + magic-byte validation, size cap, re-encode server-side, strip EXIF, serve from a separate origin |
| Analytics | Over-collection | Consent, pseudonymisation, no sensitive categories |

## 12.2 The trust boundary — memorise this list

Never trust from the client: score · XP · room role · trivia correctness · timing · prediction state · entitlement tier · achievement eligibility · leaderboard position · membership · block state.

Safe to trust (with validation): UI preferences, locale, theme, non-authoritative telemetry.

## 12.3 Rate limiting

Token-bucket in Redis, keyed by user and IP, with sensible burst allowances:

| Endpoint | Limit |
|---|---|
| Login / register | 5 / 15 min per IP+account |
| Password reset | 3 / hour per account |
| Chat message | 5 / 10s, burst 3 |
| Reaction | 10 / s (client-batched) |
| Room creation | 10 / hour |
| Trivia answer | 1 per question (enforced by uniqueness) + 30 / min ceiling |
| Search | 30 / min |
| Report | 20 / day |
| Global authenticated | 300 / min |

Responses include `Retry-After`; the client surfaces the actual wait time.

## 12.4 Moderation ladder

```
Report / automated signal
   ↓
Auto-triage (severity classifier + reporter reputation + accused history)
   ↓
Immediate action for severe categories (CSAM, credible threats, doxxing) → hard block + preserve evidence + escalate
   ↓
Queue for human review (SLA: severe <1h, high <24h, standard <72h)
   ↓
Action ladder: warn → mute (24h) → room ban → shadow-limit → account suspension
   ↓
Appeal path — every action is appealable, every action is logged with actor + reason
```

V1 builds: report, block, mute, host-kick, an admin review queue (internal tool, not a public web app), and the audit log. That is a defensible foundation, not a moderation platform.

**Minors:** age gate at signup; under-16 defaults to private profile, friends-only rooms, no discoverability, no public leaderboards, stricter chat filtering. Do not ship the social layer without this.

## 12.5 Mobile hardening

- Tokens in Keychain / EncryptedSharedPreferences — never `SharedPreferences`, never SQLite.
- No API keys in the binary. Provider calls that need a secret are **proxied through the backend**.
- Certificate pinning: implement with a documented rotation plan and a remote kill switch, or do not implement it. Unrotatable pinning bricks your app.
- Screenshot protection on any screen with sensitive data (there should be none in V1 — say so).
- Root/jailbreak detection as a **risk signal** feeding abuse scoring, never as a hard block.
- `flutter_secure_storage` + obfuscation (`--obfuscate --split-debug-info`) on release builds.

## 12.6 Logging prohibitions

Never log: passwords, tokens, refresh tokens, WS tickets, full IPs (truncate), chat content in application logs (it lives in the DB with retention), precise location, device identifiers linkable to a person.

Always log: request id, trace id, user id (pseudonymous), route, latency, status, error code.

---

# PART 13 — QUALITY

## 13.1 Test matrix

| Layer | Coverage gate | Scope |
|---|---|---|
| Domain / use cases | **≥ 90%** | Pure logic: scoring, XP rules, ranking, story engine, conflict resolution |
| Repositories | ≥ 80% | Cache arbitration, offline paths, mapping, error translation |
| ViewModels | ≥ 80% | State transitions, all failure branches |
| Widgets | Key components | Every state in §3.2 renders |
| Golden | All V1 screens | `en` LTR + `ar` RTL, light + dark, 100% and 200% text scale |
| API integration | All endpoints | Auth, validation, authorisation, idempotency, error shapes |
| Realtime integration | All event types | Two simulated clients minimum |
| E2E | Critical journeys | §13.2 |
| Load | Realtime + feed | §13.4 |
| Security | §12 controls | §8.2 plus authorisation matrix |

Overall coverage is a weak signal. **The gate that matters is domain-layer coverage** — that is where the thinking lives.

## 13.2 Mandatory E2E scenarios

1. **The vertical slice** (§2.1), on two real devices.
2. **Realtime reconnect** — A creates → B joins → A starts → B receives → A pauses → B receives → B loses network 30s → B reconnects → B requests state → server returns authoritative snapshot → B reconciles and shows the correct timeline position. *(v1 §71 — retained verbatim as a requirement.)*
3. **Anti-cheat suite** — all 10 cases in §8.2.
4. **Offline round trip** — go offline → change favourites and watch progress → force-quit → relaunch → reconnect → outbox drains → server state matches, exactly once.
5. **Deep link cold start** — link tapped with app killed and user logged out → install/open → auth → resolve → land on the correct room → handle "room ended" gracefully.
6. **Session expiry mid-room** — access token expires during a live room → silent refresh → socket ticket renewed → no visible interruption.
7. **Block enforcement** — A blocks B → both in the same public room → A receives none of B's chat, reactions, or presence, verified at the socket level.
8. **RTL sweep** — full journey in Arabic; no layout break, no clipped text at 200% scale.

## 13.3 Definition of Done (CI-enforced gates)

A feature is done only when every box is machine-verifiable:

- [ ] Requirement ID implemented and linked in the PR
- [ ] `flutter analyze` / linter: **zero warnings** (warnings are errors)
- [ ] All §3.2 states implemented for affected screens
- [ ] Server-side authorisation implemented **and tested negatively** (a test proves an unauthorised call fails)
- [ ] Tests at the required coverage gate; no test disabled or skipped
- [ ] Golden tests updated, including RTL
- [ ] OpenAPI spec updated; generated client regenerated
- [ ] Migration is reversible and tested against a seeded database
- [ ] No `TODO`, `FIXME`, or commented-out code in the diff
- [ ] Observability: the feature emits at least one meaningful metric or event
- [ ] Docs updated; ADR written if a decision was made
- [ ] No performance regression on the CI benchmark
- [ ] **No mocked data path reachable in a release build**

## 13.4 Load and chaos

Load targets for V1 (single instance, then verify horizontal scaling):
- 5,000 concurrent WebSocket connections
- 500 concurrent active rooms, average 6 participants
- Sustained 20 events/sec/room during a trivia round
- p95 fan-out latency < 150ms in-region
- Feed API p95 < 300ms at 200 rps

Chaos scenarios to run at least once: kill a backend instance mid-room · sever Redis · add 500ms latency + 5% packet loss · saturate the DB connection pool · clock-skew a client by 5 minutes. Document observed behaviour and recovery time for each.

---

# PART 14 — OPERATIONS

## 14.1 Environments and delivery

`local` → `dev` → `staging` (production-shaped, seeded) → `production`.

- Trunk-based, short-lived branches, PRs required.
- CI: analyze → unit → widget → golden → integration → build → security scan (dependency audit + secret scan) → deploy to dev.
- Staging deploy on merge to main; production on tag with manual approval.
- Mobile: Firebase App Distribution / TestFlight for internal, staged Play rollout (5% → 25% → 100%) gated on crash-free rate.
- Every realtime feature behind a flag with a kill switch.

## 14.2 Observability

**SLIs / SLOs:**

| SLI | SLO |
|---|---|
| API availability | 99.5% monthly |
| API latency | p95 < 300ms, p99 < 800ms |
| WS event fan-out | p95 < 150ms |
| WS connection success | > 99% |
| Push delivery | > 95% within 30s |
| Crash-free sessions | > 99.5% |
| Crash-free users | > 99.8% |

**Instrumentation:** structured JSON logs with `trace_id` propagated from the mobile client; RED metrics per endpoint; distributed traces on room join and trivia submission (the two highest-complexity paths); mobile crash + ANR reporting; a room-lifecycle dashboard (created / filled / duration / abandoned).

**Alerts** (page vs ticket, explicitly): error rate >2% for 5min → page · WS reconnect storm (>3× baseline) → page · DB pool >80% → ticket · outbox lag >5min → ticket · moderation queue SLA breach → ticket.

**Runbooks** in `docs/RUNBOOKS/`: realtime tier down · Redis unavailable · provider API outage · push token mass-invalidation · abuse spike. Each with detection, immediate mitigation, root-cause checklist, and rollback.

## 14.3 Backup and recovery

- Postgres: PITR, daily full + continuous WAL. **RPO 5 min, RTO 1 hour.**
- Restore drill: quarterly, timed, documented. *An untested backup is not a backup.*
- Redis: no backup needed by design — it holds only reconstructible ephemeral state. **Verify this claim by killing Redis in staging and confirming rooms survive.**
- Object storage: versioning + lifecycle rules.

## 14.4 Cost model (document, even at V1 scale)

Estimate and track: compute · database (storage + IOPS) · Redis · egress · object storage · push · metadata provider quota · crash/analytics tooling. Define the cost-per-1,000-MAU figure and the top-3 cost drivers at each scale tier in §7.9. Engineers who can talk about cost stand out.

---

# PART 15 — DELIVERY PLAN

Eight milestones with **exit criteria**, replacing v1's 26 phases.

| # | Milestone | Duration | Exit criteria |
|---|---|---|---|
| **M0** | Foundations & decisions | 1–2 wk | Legal checks done (§1.9) · provider integration probe run against the real API · ADRs 001–008 written · repo, CI, DI, design tokens, l10n scaffolding, error model in place · schema v1 migrated · seed data script working |
| **M1** | **Vertical slice** ⭐ | 3–4 wk | §2.1 demonstrated on two physical devices, recorded, on a throttled network. Realtime reconnect E2E (§13.2.2) passes. |
| **M2** | Trivia + progression | 2–3 wk | All 10 anti-cheat tests pass · XP ledger reconciles · achievements grant exactly once under concurrent load |
| **M3** | Offline + search | 2 wk | Offline round-trip E2E passes · Arabic search returns correct results for hamza/tāʾ-marbūṭa variants · storage caps enforced |
| **M4** | Discovery + social | 2–3 wk | Ranker beats the popularity baseline on nDCG@10 in the offline harness (or the gap is documented honestly) · activity feed respects privacy settings · block enforcement E2E passes |
| **M5** | Notifications + deep links | 1–2 wk | Cold-start deep link E2E passes · all §36 edge cases handled · opt-out honoured end to end |
| **M6** | Hardening | 3 wk | Load targets met · chaos scenarios documented · a11y audit passed · full RTL sweep passed · security checklist signed off · SLO dashboards live |
| **M7** | Demo + documentation | 1 wk | Demo environment reproducible from scratch by a stranger following the README · full doc set · 90-second walkthrough recorded · interview pack complete |

**Do not start M(n+1) until M(n) exit criteria are met.** Cut scope within a milestone rather than carrying debt forward.

---

# PART 16 — DOCUMENTATION & PORTFOLIO

## 16.1 Document set

```
README.md                  What, why, how to run — in under 5 minutes
docs/
  ARCHITECTURE.md          System overview, C4 context + container diagrams
  FLUTTER_ARCHITECTURE.md  Layers, state mgmt, DI, error model, folder rules
  BACKEND_ARCHITECTURE.md  Modules, boundaries, outbox, jobs
  DATABASE.md              ERD, schema decisions, indexing rationale, retention
  REALTIME.md              ⭐ Envelope, resync, timeline, presence, backpressure, failure matrix
  GAME_SYSTEMS.md          ⭐ Trivia protocol, anti-cheat, XP economy, achievements
  RECOMMENDATIONS.md       Two-stage design, features, weights, evaluation results
  OFFLINE_SYNC.md          Cache tiers, freshness, outbox, conflict resolution
  SECURITY.md              Threat model, trust boundary, controls
  MODERATION.md            Policy, ladder, SLAs, minors
  PRIVACY.md               PII map, lawful basis, retention, subject-request handling
  TESTING.md               Strategy, matrix, how to run everything
  API.md                   Generated from OpenAPI + conventions
  INTEGRATIONS.md          Real provider response shapes, quotas, terms notes
  DEPLOYMENT.md            Environments, pipeline, rollback
  RUNBOOKS/                One file per incident class
  DECISIONS/               ADR-001.md … (see 16.2)
  SCALING.md               §7.9 with migration triggers
  INTERVIEW.md             §16.4
```

Every diagram is generated from a text source (Mermaid/PlantUML) committed alongside it, so it cannot silently drift from reality.

## 16.2 ADR format

```markdown
# ADR-00X: <Decision>
Status: Proposed | Accepted | Superseded by ADR-00Y
Date: YYYY-MM-DD

## Context
The forces at play. What constraint made this a real decision?

## Options considered
| Option | Pros | Cons | Score |

## Decision
What was chosen and the deciding factor.

## Consequences
What becomes easy. What becomes hard. What must be revisited, and at what trigger.
```

**Required ADRs:** 001 state management · 002 Companion Sync over playback sync · 003 event-log + resync protocol · 004 server-authoritative game logic · 005 modular monolith · 006 Postgres-native search for V1 · 007 heuristic ranker with a model seam · 008 offline conflict strategy per entity · 009 Redis scope (ephemeral only) · 010 UUIDv7.

## 16.3 Demo requirements

- Reproducible from a clean machine: `docker compose up` + a seed command + a documented test account. No private credentials required.
- Seed data must be **plausible**: 200 real titles with real metadata, 40 users with realistic social graphs and watch histories, 300 hand-written trivia questions across 20 titles, 50 achievements, 30 historical rooms. *Quality over v1's 500 random items.*
- A 90-second recorded walkthrough of the vertical slice, with both device screens visible side by side and the network throttled — the reconnect moment is the money shot.

## 16.4 Interview pack — the hard questions, answered

Prepare a written answer to each. These are what a senior engineer will actually ask:

1. You can't stream the content — so what exactly is synchronised, and how do you keep it accurate? *(§1.2, §7.4)*
2. A client's clock is wrong by five minutes. What breaks? *(Nothing. §7.4, §8.1)*
3. A user reconnects after 90 seconds. Walk me through every message. *(§7.3)*
4. How do you know a trivia answer arrived before the deadline? *(§8.1 — server receipt time, RTT-compensated)*
5. Why is XP a ledger instead of a column? *(§6.2, §8.4)*
6. Two devices submit the same answer simultaneously. What happens? *(§8.1 step 5e — DB uniqueness, not application logic)*
7. Your recommendation weights — where did they come from, and how do you know they help? *(§10.2, §10.4 — and be honest if it barely beats popularity)*
8. A user is offline and favourites a title, then favourites it on another device. Reconcile. *(§11.4)*
9. Redis dies. What happens to live rooms? *(§7.5, §14.3 — and say that you tested it)*
10. Why a modular monolith and not services? What would make you split? *(§5.1, §7.9 triggers)*
11. Where is the biggest weakness in this system? *(Answer honestly. Suggested: Companion Sync accuracy depends on user cooperation; trivia content supply is the real bottleneck; the ranker has thin data at V1 scale.)*
12. What would you do differently with three more months? *(Have a real answer.)*

**Prepare a two-minute and a ten-minute version of the whole project.** The two-minute version leads with the sync problem, not the feature list.

---

# PART 17 — FINAL QUALITY BAR

VYBE succeeds if a senior engineer, after 30 minutes in the repository, concludes:

> "This person understands distributed state, doesn't trust the client, tests the failure paths, and knows what they chose not to build."

VYBE fails if it looks like: a Netflix clone · a CRUD app with a chat bolted on · a feature checklist with no working end-to-end path · a real-time demo that has only ever run on one device · an "AI recommender" with no evaluation · 40 beautiful screens that all break offline.

**One perfect vertical slice beats forty shallow features. Build the slice.**

---

## Appendix A — Changes from v1 (summary)

| # | Change | Type |
|---|---|---|
| 1 | Resolved the streaming/sync contradiction via Companion Sync | **Critical fix** |
| 2 | Added a product thesis, positioning, and an Arabic-first market wedge | Addition |
| 3 | Added measurable success metrics and engineering SLOs | Addition |
| 4 | Added legal, compliance, minors, and store-review risk | **Critical gap** |
| 5 | Replaced 26 phases with 8 milestones with exit criteria | Restructure |
| 6 | Added a ruthless V1 vertical slice and an explicit cut list | Restructure |
| 7 | Specified the event envelope, seq ordering, and snapshot/delta resync | Deepening |
| 8 | Specified clock-offset measurement with RTT compensation | Deepening |
| 9 | Specified the full trivia protocol and 10 anti-cheat test cases | Deepening |
| 10 | Made XP a ledger with diminishing returns and shadow-scoring | Deepening |
| 11 | Replaced "score = a+b+c" with a two-stage ranker plus an offline evaluation harness | Deepening |
| 12 | Added idempotency keys, transactional outbox, cursor pagination, RFC 9457 errors | Addition |
| 13 | Added per-entity conflict resolution instead of one global rule | Deepening |
| 14 | Made Arabic/RTL a launch requirement with CI golden gates | Elevation |
| 15 | Turned the Definition of Done into machine-checkable gates | Elevation |
| 16 | Added ops: environments, SLOs, alerts, runbooks, RPO/RTO, cost model | Addition |
| 17 | Added monetisation and the `Entitlement` concept | Addition |
| 18 | Added the agent operating contract, anti-fabrication rules, and stop conditions | **Structural** |
| 19 | Deferred interactive stories to V2, with the full spec preserved | Scope discipline |
| 20 | Removed the stray `:contentReference[oaicite:2]{index=2}` artefact from v1 §47 | Housekeeping |

---

*End of VYBE Master Prompt v2.0*
