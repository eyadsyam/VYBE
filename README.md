# VYBE

**The multiplayer layer over any streaming service.**

VYBE turns solitary streaming into a synchronised, competitive, shared event —
without ever hosting a frame of copyrighted video.

Flutter mobile client · Go modular-monolith backend · Postgres · Redis

---

## The problem, and the interesting bit

You cannot synchronise playback of a stream you do not control. Netflix,
Shahid, Disney+, and YouTube expose no sanctioned API for a third party to read
or drive playback position on mobile. Any product that claims otherwise is
either faking the feature or breaking the law.

**So VYBE synchronises the clock, not the stream.**

```
t_room = (server_now − anchor_server_time) + anchor_offset
```

The room runs a server-authoritative virtual timeline. You press play in *your*
app, during a countdown driven by *our* clock. Every timed thing — trivia beats,
prediction windows, spoiler-safe chat, reaction bursts — fires against that
timeline. Clients measure their own clock offset with an NTP-style handshake and
correct for it.

The consequence worth stating: **a device whose clock is five minutes wrong
behaves identically to one that is correct.** There is a test that proves it,
and a second assertion in the same test proving the correction is doing real
work rather than being trivially satisfiable.

This is genuinely harder than naive playback sync, is legal on every provider,
and works with services that will never have an API.

→ [ADR-002](docs/DECISIONS/ADR-002-companion-sync.md) ·
[ADR-003](docs/DECISIONS/ADR-003-event-log-resync.md)

---

## Current status — M0, in progress

Honest state. §0.5 requires a "what is NOT done" list, so here it is up front.

### Verified working

| | Evidence |
|---|---|
| Go domain logic: trivia scoring + anti-cheat | `go test` — **100%** statement coverage |
| Go domain logic: Companion Sync clock + timeline | `go test` — **100%** statement coverage |
| UUIDv7 generator, monotonic within a millisecond | `go test` — 83.9%, incl. clock-step and rollover cases |
| Config loading + secret redaction | `go test` — 88.0% |
| Migration loader, checksum guard, module-ownership check | `go test` — all 35 tables have a declared owner |
| Flutter core: error model, `Result`, sync clock | `flutter test` — **28/28**, 92.7% line coverage |
| Static analysis | `go vet` clean · `gofmt` clean · `flutter analyze` **no issues** |
| Spec traceability | 64 FR · 20 NFR · 35 AC — every FR covered, every AC traces back |
| l10n `en` + `ar` | `untranslated.json` empty — zero gaps |
| Import boundary + literal-string lints | Both pass |

### Not done, and not claimed

| | Why |
|---|---|
| **The schema has never run against a live Postgres** | Docker engine blocked on a WSL2 reboot — [BLOCKER-02](docs/BLOCKERS.md#blocker-02). The eight migrations are authored and checksummed but **unverified**. |
| **No TMDB response shapes recorded** | No API key, so the probe has not run — [BLOCKER-01](docs/BLOCKERS.md#blocker-01). `docs/INTEGRATIONS.md` therefore documents nothing, deliberately. |
| **No 200-title catalogue** | Same blocker. `cmd/seed --full` refuses rather than inventing plausible titles. |
| **No V1 screens** | M1. The app currently boots to a placeholder that exercises the l10n pipeline and says so. |
| **No API endpoints** | M1. `/healthz` and `/readyz` only; everything else returns an honest 404. |
| **Vertical slice not demonstrated** | Needs two physical devices — [BLOCKER-03](docs/BLOCKERS.md#blocker-03). |
| **Trademark unchecked** | [BLOCKER-04](docs/BLOCKERS.md#blocker-04) · [LEGAL.md L3](docs/LEGAL.md) |

---

## Run it

### Prerequisites

Flutter 3.44+ · Go 1.26+ · Docker (with WSL2 on Windows)

> **Windows note:** if your Flutter SDK path contains a space, `flutter test`
> fails in `objective_c`'s build hook. See
> [ENVIRONMENT.md](docs/ENVIRONMENT.md) for the one-line junction workaround.

### Backend

```bash
cp .env.example .env          # optional: add TMDB_API_KEY to enable provider refresh

docker compose up -d db redis
cd server
go run ./cmd/migrate up       # apply schema
go run ./cmd/seed             # achievement rules + feature flags
go run ./cmd/api              # listens on :8080

curl localhost:8080/readyz
```

Or the whole stack:

```bash
docker compose --profile app up
```

### App

```bash
cd app
flutter pub get
flutter gen-l10n
flutter run
```

### Tests

```bash
cd server && go test ./... -race -cover
cd app    && flutter test --coverage

python tools/spec/check_traceability.py specs/001-vertical-slice/spec.md
dart tools/lint/no_literal_strings.dart app/lib
dart tools/lint/feature_boundaries.dart app/lib
```

---

## Architecture at a glance

```
Flutter (Riverpod)                Go modular monolith
─────────────────                 ───────────────────
widgets      (dumb)               identity · users · social · catalog
   ↕                              discovery · rooms · realtime · games
view models  (no I/O)             progression · notifications · moderation
   ↕                              analytics · entitlements
use cases    (business rules)          ↕
   ↕                              Postgres 17 (source of truth)
repositories (cache policy)       Redis 7 (ephemeral only — ADR-009)
   ↕
data sources (api · db · socket)
```

**Boundaries are enforced, not asserted.** `tools/lint/feature_boundaries.dart`
fails the build when a feature imports another feature's `data/` or
`presentation/`, or when the domain layer imports Flutter, Dio, or Drift. A test
fails the build when a table has no declared module owner. §5.1: *an unenforced
boundary is a comment.*

---

## The parts worth reading first

If you have fifteen minutes and want to see whether this is real:

1. **[ADR-002](docs/DECISIONS/ADR-002-companion-sync.md)** — why the product's
   central contradiction had to be resolved before any code, and how.
2. **[`server/internal/modules/realtime/timeline_test.go`](server/internal/modules/realtime/timeline_test.go)**
   — `TestTimeline_DeviceClockFiveMinutesWrongStillReadsCorrectPosition`.
3. **[`server/internal/modules/games/scoring.go`](server/internal/modules/games/scoring.go)**
   — the answer key is *absent* from the wire, not obfuscated. Timing is server
   receipt, RTT-compensated so a user on 3G is not penalised for their network.
4. **[`server/internal/modules/games/scoring_test.go`](server/internal/modules/games/scoring_test.go)**
   — `TestScore_ExactHalfRoundsUp`. A float implementation silently loses a
   point at base 250 / elapsed 10.96s. Scores are integers for a reason.
5. **[`specs/001-vertical-slice/spec.md`](specs/001-vertical-slice/spec.md)** —
   AC-10 and AC-20 assert on the *full serialised payload*, because "we don't
   show the answer in the UI" is a different claim from "the answer never
   reaches the device".

---

## Documentation

| | |
|---|---|
| [Master prompts](docs/spec/) | The governing specification (this repo is the implementation) |
| [SPEC-001](specs/001-vertical-slice/spec.md) | The V1 vertical slice — 64 FR, 20 NFR, 35 AC, 18 edge cases |
| [ADRs](docs/DECISIONS/) | 12 decisions with scoring matrices and honest consequences |
| [LEGAL.md](docs/LEGAL.md) | §1.9 review — what is designed out, mitigated, and still open |
| [BLOCKERS.md](docs/BLOCKERS.md) | Live register of what is stopping which exit criterion |
| [INTEGRATIONS.md](docs/INTEGRATIONS.md) | Observed provider behaviour (currently: none, deliberately) |
| [ENVIRONMENT.md](docs/ENVIRONMENT.md) | Verified toolchain, and the two workarounds in use |

---

## What this project is not

Named explicitly, because §2.2 says the cut list matters as much as the feature
list:

- **Not a video host.** No stream is ever served, proxied, or accepted as input.
- **Not a Netflix clone.** The catalogue exists to give a room something to be
  about.
- **Not microservices.** A modular monolith with documented extraction triggers
  (ADR-005) — extract `realtime` when p95 fan-out exceeds 200ms, not on a date.
- **Not "AI-powered recommendations".** A documented heuristic ranker behind a
  model-shaped seam, which must beat popularity on nDCG@10 in an offline harness
  — and if it does not, [ADR-007](docs/DECISIONS/ADR-007-heuristic-ranker.md)
  commits to saying so in those words.
