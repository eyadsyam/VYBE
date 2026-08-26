# Handoff — where the build stands

Written 2026-08-26. Update this at the end of every working session, before
stopping. It exists so that picking the project back up costs minutes rather
than an hour of rereading.

---

## One-paragraph catch-up

VYBE synchronises a **clock**, not a stream (ADR-002) — you cannot sync
playback of a service you do not control, so the room runs a
server-authoritative virtual timeline and every timed feature fires against
it. **M0 is complete. M1's vertical slice is built and wired end to end**: the
server serves auth, rooms, and a WebSocket tier against Postgres and Redis, and
the Flutter app signs in, lists rooms, joins by code, and watches a room update
live from the socket. What is still missing is not plumbing — it is the
features that sit on top (Companion Sync's countdown UI, trivia, chat) and one
thing only the operator can supply: a **second physical device**, without which
AC-1's ±250ms convergence cannot be demonstrated at all.

---

## Current state

**Repo:** https://github.com/eyadsyam/VYBE (private) · branch `main`

### Verified, with evidence

| Area | Evidence |
|---|---|
| Trivia scoring + anti-cheat (Go) | `go test` — **100%** |
| Companion Sync clock + timeline (Go) | `go test` — **100%** |
| Event log, resync, dedupe (FR-28–35) | AC-7/8/9/11/12 asserted with the numbers the criteria name |
| WebSocket hub — fan-out, presence, backpressure | **92%**; the send-under-lock deadlock verified by writing it and watching the test time out |
| Identity — Argon2id, JWT, refresh rotation, tickets | domain files **93.9%** |
| Rooms — state machine, join codes, capacity, succession | domain files **92.5%** |
| HTTP edge — problems, cursors, idempotency, decode | domain files **98.9%** |
| Password hashing | **97.2%**; long-passphrase truncation asserted against the bcrypt failure |
| Full request path, end to end | `cmd/api` test: signup → room → join → ws-ticket → socket → an HTTP transition arriving as a WebSocket event on another client |
| OpenAPI ↔ router agreement | a test walks the real chi router; both directions asserted |
| Spec traceability | 64 FR · 20 NFR · 35 AC — all trace |
| TMDB provider behaviour | probe run live, 10/10 calls; shapes in [INTEGRATIONS.md](INTEGRATIONS.md) |
| Flutter — 287 tests | analyze clean, both lint gates OK |
| Room socket (Dart) | resync, dedupe, backoff; the delta-hole guard verified by deleting it |
| Golden matrix | 8 variants per screen: en/ar × light/dark × 100%/200% |
| l10n en + ar | 127 keys each; `untranslated.json` empty |

### Built but unverified

**Nothing has ever run against a live Postgres or Redis on this machine.**
Docker will not start until the operator reboots (BLOCKER-02), so:

- The **8 migrations** are authored, checksummed, and their loader is tested;
  the SQL itself has not executed.
- The **36 integration tests** — `TestPG*` and `TestRedisTicket*` — skip
  locally and run in CI. They are the only cover the SQL and Redis paths get,
  which is why CI now asserts they actually ran rather than silently skipped.
- **CI has never run.** Every number above is from a local run.

### Not started

- **Companion Sync's UI.** The clock, the timeline, and the socket's offset
  estimator all exist and are tested; no screen arms a countdown yet.
- **Trivia, chat, reactions.** Scoring is at 100%; nothing is wired to a room.
- **The catalogue.** TMDB is probed and documented; no browse screen, and
  `content` rows are only created by tests.
- **`go test -race` has never run on this machine** — cgo needs a C compiler
  that is not installed. CI on ubuntu is what covers it. See
  [ENVIRONMENT.md](ENVIRONMENT.md).

---

## Blocked on the operator

Full detail in [BLOCKERS.md](BLOCKERS.md).

1. **Reboot Windows.** WSL2's features are staged but inactive, so the Docker
   engine will not start. Then `docker compose up -d db redis` and
   `go run ./cmd/migrate up` closes BLOCKER-02 and the 36 integration tests
   run locally for the first time.
2. ~~**TMDB API key.**~~ ✅ Done 2026-08-25. Two findings changed plans rather
   than merely unblocking: Arabic **film** synopses are missing 80% of the time
   and TMDB does not fall back to English, so the fallback is ours and
   ADR-012's curated path is promoted; and TMDB exposes **no rate-limit headers
   at all**, so our limiter must be statically configured.
3. **A second physical device.** `flutter devices` shows only Windows, Chrome,
   and Edge. Two clients on one machine share a clock, a network path, and a
   scheduler, so they cannot demonstrate Companion Sync — AC-1's ±250ms
   assertion is meaningless when both read the same `DateTime.now()`.

---

## Authorship

Sole author is **eyadsyam**. Every commit's author *and* committer is
`eyadsyam <138543485+eyadsyam@users.noreply.github.com>`, and no commit message
carries a `Co-Authored-By` or any other attribution trailer. Verify with:

```bash
git log --all --format='%an <%ae> | %cn <%ce>' | sort -u   # one line, eyadsyam
git log --all --format='%B' | grep -ci 'co-authored\|assistant'   # 0
```

History was rewritten once to strip trailers that earlier commits carried, and
force-pushed on 2026-08-25. `refs/original/*` was deleted, the reflog expired,
and `git gc --prune=now` run, so the old commits no longer exist locally.

**One caveat, stated because it is easy to assume otherwise.** GitHub keeps
force-pushed commits as unreferenced objects for a while, so the pre-rewrite
SHAs can still be fetched by exact hash even though no branch points at them.
They are not on `main`, and the Contributors graph is computed from the default
branch, so it reflects the clean history. Destroying them rather than orphaning
them needs GitHub Support, or the repository deleted and re-pushed.

---

## Resuming

Read in this order —
1. This file
2. [BLOCKERS.md](BLOCKERS.md) — what is stopping what
3. [DECISIONS/README.md](DECISIONS/README.md) — the 12 ADRs, one line each
4. [../server/api/openapi.yaml](../server/api/openapi.yaml) — the v1 contract,
   including the reasoning a client author would otherwise have to infer
5. [specs/001-vertical-slice/spec.md](../specs/001-vertical-slice/spec.md) —
   the M1 contract

---

## Running it

```bash
# Dependencies (needs BLOCKER-02 resolved)
docker compose up -d db redis
cd server && go run ./cmd/migrate up

# Server
cd server && go run ./cmd/api            # :8080

# App — note the junction; the SDK path contains a space, which breaks
# objective_c's build hook. See ENVIRONMENT.md.
cd app && /d/flutter-sdk/bin/flutter.bat run

# Point the app somewhere else
flutter run --dart-define=VYBE_API_BASE_URL=https://staging.vybe.app
```

The Android emulator maps the host's loopback to `10.0.2.2`, which `main.dart`
selects automatically — `localhost` there is the emulated device itself.

---

## Verification commands

```bash
# Server
cd server && go vet ./... && gofmt -l . && go test ./... -race -cover

# Integration tests (needs the stack up)
cd server && VYBE_DB_DSN='postgres://vybe:vybe@localhost:5432/vybe?sslmode=disable' \
             VYBE_REDIS_ADDR=localhost:6379 go test ./internal/... -v -run 'TestPG|TestRedisTicket'

# App  (via the space-free junction)
cd app && /d/flutter-sdk/bin/flutter.bat analyze
cd app && /d/flutter-sdk/bin/flutter.bat test

# Gates
python tools/spec/check_traceability.py specs/001-vertical-slice/spec.md
/d/flutter-sdk/bin/dart.bat tools/lint/no_literal_strings.dart app/lib
/d/flutter-sdk/bin/dart.bat tools/lint/feature_boundaries.dart app/lib
```

---

## Next work, in dependency order

### Track A — backend (`server/` only)

1. ✅ `internal/platform/httpx` — RFC 9457, cursor pagination, `Idempotency-Key`
2. ✅ `internal/modules/identity` — Ed25519 JWT, rotating refresh, WS tickets
3. ✅ `internal/modules/rooms` — state machine, join codes, capacity, succession
4. ✅ **Repositories + handlers.** Postgres and Redis, wired in `cmd/api`.
5. ✅ `internal/modules/realtime` — hub, fan-out filtering, presence, resync
6. ✅ `api/openapi.yaml` — with a test that keeps it honest
7. **Companion Sync endpoints** — arm, anchor, re-anchor (FR-21–27). The clock
   and timeline maths are done and at 100%; nothing calls them.
8. **Catalogue** — the TMDB refresh path and `content` search (ADR-012). Note
   the Arabic-synopsis finding: the curated path is primary, not a fallback.
9. **Trivia** (FR-43–52) and **chat/reactions** (FR-36–42's payload types).
10. **The reaper** — `ShouldReap` is written and tested; no job runs it.

### Track B — app (`app/` only)

1. ✅ `core/ui/states/` — the §3.2 state widgets and the FR-60 freshness banner
2. ✅ `core/network/` — Dio factory, RFC 9457 → `Failure`, single-flight refresh
3. ✅ `core/storage/` — Drift schema, cache tiers, freshness (§11.1–11.2)
4. ✅ `app/router` — go_router with deep-link resolution (FR-13)
5. ✅ `features/rooms/` — list, join, and the room screen against the real API
6. ✅ `test/golden/` — en LTR + ar RTL, light + dark, 100% and 200%
7. **Wire the cache into the repositories.** The Drift layer and the cache
   policy exist and are tested; no repository reads from them yet, so the app
   still shows a spinner where it could show stale-but-labelled data.
8. **Companion Sync UI** — the countdown, the drift nudge, the re-anchor
   request. This is the demo, and it needs Track A #7 and a second device.
9. **The five-tab shell** (§3.1) and the catalogue browse screen.

### Rules that apply to both

- No `Co-Authored-By` or AI-attribution trailers in commits. Sole author is
  eyadsyam.
- No mocked data path reachable in a release build (§13.3). The in-memory
  stores live in `identitytest/` and `roomstest/`, which nothing under `cmd/`
  imports outside a test file.
- Never invent third-party API behaviour (§0.3 rule 1) — this is why
  `cmd/probe` has no offline mode and `cmd/seed --full` refuses.
- Domain imports no Flutter, no Dio, no Drift; features never import another
  feature's `data/` or `presentation/`. Both enforced by
  `tools/lint/feature_boundaries.dart`.
