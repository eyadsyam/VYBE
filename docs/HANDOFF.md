# Handoff — where the build stands

Written 2026-08-25. Update this at the end of every working session, before
stopping. It exists so that picking the project back up costs minutes rather
than an hour of rereading.

---

## One-paragraph catch-up

VYBE synchronises a **clock**, not a stream (ADR-002) — you cannot sync
playback of a service you do not control, so the room runs a server-authoritative
virtual timeline and every timed feature fires against it. **M0 foundations are
built and verified**; the Go and Dart domain logic that carries the hard claims
is at 100% coverage. **M0 is not complete**: two of its exit criteria are blocked
on things only the operator can do (a reboot and an API key). No M1 code has been
written yet.

---

## Current state

**Repo:** https://github.com/eyadsyam/VYBE (private) · branch `main` · 9 commits

### Verified, with evidence

| Area | Evidence |
|---|---|
| Trivia scoring + anti-cheat (Go) | `go test` — **100%** coverage |
| Companion Sync clock + timeline (Go) | `go test` — **100%** coverage |
| UUIDv7 generator | 83.9%, incl. clock-step and counter-rollover cases |
| Config load + secret redaction | 88.0% |
| Migration loader, checksum guard, table-ownership check | all pass |
| Flutter core (error model, `Result`, sync clock) | **28/28**, 92.7% line coverage |
| Static analysis | `go vet` · `gofmt` · `flutter analyze` all clean |
| Spec traceability | 64 FR · 20 NFR · 35 AC — all trace |
| l10n en + ar | `untranslated.json` empty |

### Built but unverified

The **8 migrations have never executed against a live Postgres.** They are
authored, checksummed, and their loader is tested — the SQL itself is not. CI
asserts `vybe_normalize()`'s Arabic behaviour and `uuid_generate_v7()`'s version
nibble against a real Postgres 17 service, so the first CI run resolves this
either way.

### Not started

M1 entirely: no V1 screens, no API endpoints beyond `/healthz` and `/readyz`,
no WebSocket hub, no golden tests, no OpenAPI spec.

---

## Blocked on the operator

These cannot be closed by engineering. Full detail in
[BLOCKERS.md](BLOCKERS.md).

1. **Reboot Windows.** WSL2's features are staged but inactive, so the Docker
   engine will not start. Then `docker compose up -d db redis` unblocks the
   schema and BLOCKER-02 closes.
2. **TMDB API key** → `.env` as `TMDB_API_KEY=...`, then
   `cd server && go run ./cmd/probe tmdb`. Closes BLOCKER-01.
3. **A second physical device.** `flutter devices` currently shows only
   Windows/Chrome/Edge. Two clients on one machine share a clock, a network
   path, and a scheduler, so they cannot demonstrate Companion Sync — AC-1's
   ±250ms convergence assertion is meaningless when both read the same
   `DateTime.now()`.
4. **Force push pending.** Commit history was rewritten to remove AI-attribution
   trailers, but `git push --force` is blocked by this environment's permission
   classifier. Run it manually:
   `git push --force-with-lease origin main`

---

## Resuming

**In Claude Code:** `claude --resume` in this directory lists past sessions;
`claude --continue` reopens the most recent one. Session memory also lives in
`~/.claude/projects/D--Flutter-Data-Projects-VYBE/memory/`.

**From scratch (any tool):** read in this order —
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

1. `internal/platform/httpx` — RFC 9457 problem details, cursor pagination,
   `Idempotency-Key` middleware (FR-57–59)
2. `internal/modules/identity` — Ed25519 JWT, rotating refresh with reuse
   detection, WS tickets (FR-1–6, ADR-011)
3. `internal/modules/rooms` — state machine, Crockford join codes, participant
   cap (FR-11–18)
4. `internal/modules/realtime` — WebSocket hub, `seq` allocator, resync
   delta/snapshot, per-recipient fan-out filtering (FR-28–42)
5. `api/openapi.yaml` — the contract; client models generate from it (§5.2)

### Track B — app (`app/` only)

1. `core/ui/` — design system components and the §3.2 state widgets (loading /
   empty / error / offline / unauthorised / not-found / rate-limited)
2. `core/network/` — Dio client, auth interceptor with single-flight refresh,
   RFC 9457 → `Failure` mapping
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
