# Handoff — where the build stands

Written 2026-08-25. Update this at the end of every working session, before
stopping. It exists so that picking the project back up costs minutes rather
than an hour of rereading.

---

## One-paragraph catch-up

VYBE synchronises a **clock**, not a stream (ADR-002) — you cannot sync
playback of a service you do not control, so the room runs a server-authoritative
virtual timeline and every timed feature fires against it. **M0 foundations are
built and verified.** **M1 domain logic has begun**: the HTTP edge, identity,
rooms, and the client's error/network/state layers are written and heavily
tested — but **nothing is wired together yet**. There are no handlers, no
repositories, no screens. M0 still has one exit criterion blocked on the
operator (a reboot, for Docker), and the vertical slice needs a second physical
device.

---

## Current state

**Repo:** https://github.com/eyadsyam/VYBE (private) · branch `main`

### Verified, with evidence

| Area | Evidence |
|---|---|
| Trivia scoring + anti-cheat (Go) | `go test` — **100%** coverage |
| Companion Sync clock + timeline (Go) | `go test` — **100%** coverage |
| UUIDv7 generator | 83.9%, incl. clock-step and counter-rollover cases |
| Config load + secret redaction | 88.0% |
| Migration loader, checksum guard, table-ownership check | all pass |
| Flutter core (error model, `Result`, sync clock) | 92.7% line coverage |
| Flutter app total | **91/91 tests**; hand-written code **96.3%** (445/462 lines) |
| Static analysis | `go vet` · `gofmt` · `flutter analyze` all clean |
| Spec traceability | 64 FR · 20 NFR · 35 AC — all trace |
| l10n en + ar | `untranslated.json` empty |
| HTTP edge — problems, cursors, idempotency (FR-57–59) | `go test` — **98.8%** coverage |
| TMDB provider behaviour | probe run live, 10/10 calls; shapes in [INTEGRATIONS.md](INTEGRATIONS.md) |
| Identity — JWT, refresh rotation, WS tickets (FR-1–5) | `go test` — **95.3%** coverage |
| Rooms — state machine, join codes, capacity, succession (FR-11–18) | `go test` — **98.6%** coverage |
| Client problem mapping + single-flight refresh | 100% / 97.8%; single-flight verified by removing the lock and watching the test fail |
| §3.2 state widgets | tested in en + ar RTL at 100% and 200% scale |

### Built but unverified

The **8 migrations have never executed against a live Postgres.** They are
authored, checksummed, and their loader is tested — the SQL itself is not. CI
asserts `vybe_normalize()`'s Arabic behaviour and `uuid_generate_v7()`'s version
nibble against a real Postgres 17 service, so the first CI run resolves this
either way.

### Not started

Still absent, and not claimed:

- **No HTTP handlers.** The identity and rooms *domain logic* exists and is
  tested; nothing mounts it. `/healthz` and `/readyz` remain the only routes.
- **No repositories.** Every module is pure domain plus an interface. Nothing
  talks to Postgres yet, so nothing has been exercised against real SQL.
- **No WebSocket hub** (FR-28–42), **no OpenAPI spec** (§5.2), **no V1 screens**,
  **no Drift storage layer**, **no router**, **no golden tests**.
- **`go test -race` has never run on this machine** — cgo needs a C compiler
  that is not installed. CI on ubuntu is what actually covers it. See
  [ENVIRONMENT.md](ENVIRONMENT.md).
- **CI has never run.** Every coverage figure above is from a local run.

---

## Blocked on the operator

These cannot be closed by engineering. Full detail in
[BLOCKERS.md](BLOCKERS.md).

1. **Reboot Windows.** WSL2's features are staged but inactive, so the Docker
   engine will not start. Then `docker compose up -d db redis` unblocks the
   schema and BLOCKER-02 closes.
2. ~~**TMDB API key.**~~ ✅ **Done 2026-08-25.** Key is in `.env`, the probe ran
   against the live API (10/10 calls), and observed shapes are transcribed in
   [INTEGRATIONS.md](INTEGRATIONS.md). BLOCKER-01 is closed. Two findings change
   plans rather than merely unblocking: Arabic **film** synopses are missing 80%
   of the time and TMDB does not fall back to English, so the fallback is ours
   and ADR-012's curated path is promoted; and TMDB exposes **no rate-limit
   headers at all**, so our limiter must be statically configured.
3. **A second physical device.** `flutter devices` currently shows only
   Windows/Chrome/Edge. Two clients on one machine share a clock, a network
   path, and a scheduler, so they cannot demonstrate Companion Sync — AC-1's
   ±250ms convergence assertion is meaningless when both read the same
   `DateTime.now()`.

*(The force push that was pending here is done — see "Authorship" below.)*

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
force-pushed on 2026-08-25. The rewrite is complete on both sides: `refs/original/*`
backups were deleted, the reflog expired, and `git gc --prune=now` run, so the
old commits no longer exist locally.

**One caveat, stated because it is easy to assume otherwise.** GitHub keeps
force-pushed commits as unreferenced objects for a while, so the pre-rewrite
SHAs can still be fetched by exact hash even though no branch points at them.
They are not on `main`, and the Contributors graph is computed from the default
branch, so it reflects the clean history. If they must be destroyed rather than
merely orphaned, that needs GitHub Support to run `git gc` server-side, or the
repository deleted and re-pushed.

---

## Resuming

Read in this order —
1. This file
2. [BLOCKERS.md](BLOCKERS.md) — what is stopping what
3. [DECISIONS/README.md](DECISIONS/README.md) — the 12 ADRs, one line each
4. [specs/001-vertical-slice/spec.md](../specs/001-vertical-slice/spec.md) — the
   M1 contract

---

## Verification commands

Note the Flutter junction — the SDK path contains a space, which breaks
`objective_c`'s build hook. See [ENVIRONMENT.md](ENVIRONMENT.md).

```bash
# Server
cd server && go vet ./... && gofmt -l . && go test ./... -race -cover

# App  (via the space-free junction)
cd app && /d/flutter-sdk/bin/flutter.bat analyze
cd app && /d/flutter-sdk/bin/flutter.bat test test/unit/

# Gates
python tools/spec/check_traceability.py specs/001-vertical-slice/spec.md
/d/flutter-sdk/bin/dart.bat tools/lint/no_literal_strings.dart app/lib
/d/flutter-sdk/bin/dart.bat tools/lint/feature_boundaries.dart app/lib
```

---

## Next work, in dependency order

Parallelisable into two independent tracks — they touch disjoint directories.

### Track A — backend (`server/` only)

1. ✅ `internal/platform/httpx` — RFC 9457, cursor pagination, `Idempotency-Key`
2. ✅ `internal/modules/identity` — Ed25519 JWT, rotating refresh, WS tickets
3. ✅ `internal/modules/rooms` — state machine, join codes, capacity, succession
4. **Repositories + handlers.** Everything above is pure domain behind an
   interface; nothing is mounted. This is now the critical path — the domain
   rules are meaningless until a request can reach them. Needs Postgres, so it
   is gated on BLOCKER-02.
5. `internal/modules/realtime` — WebSocket hub, `seq` allocator, resync
   delta/snapshot, per-recipient fan-out filtering (FR-28–42)
6. `api/openapi.yaml` — the contract; client models generate from it (§5.2)

### Track B — app (`app/` only)

1. ✅ `core/ui/states/` — the §3.2 state widgets and the FR-60 freshness banner
2. ✅ `core/network/` — RFC 9457 → `Failure`, auth interceptor with single-flight
   refresh. **Not yet done:** the Dio client factory itself (base options,
   timeouts, trace-id header, retry policy)
3. `core/storage/` — Drift schema, cache tiers, freshness (§11.1–11.2)
4. `app/router` — go_router with deep-link resolution (FR-13)
5. `features/rooms/` — the room screen against a fake data source until Track A
   lands
6. `test/golden/` — en LTR + ar RTL, light + dark, 100% and 200% scale

### Rules that apply to both

- No `Co-Authored-By` or AI-attribution trailers in commits. Sole author is
  eyadsyam.
- No mocked data path reachable in a release build (§13.3).
- Never invent third-party API behaviour (§0.3 rule 1) — this is why
  `cmd/probe` has no offline mode and `cmd/seed --full` refuses.
- Domain layer imports no Flutter, no Dio, no Drift; features never import
  another feature's `data/` or `presentation/`. Both enforced by
  `tools/lint/feature_boundaries.dart`.
