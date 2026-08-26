# Blockers

Live register of things that stop a milestone exit criterion from being met.
§0.4 requires halting and reporting rather than continuing past them, and §0.5
requires an honest "what is NOT done" list at every work unit.

A blocker leaves this file only when the criterion it blocks is **verified**,
not when it is worked around.

| # | Blocks | Severity | Owner | Status |
|---|---|---|---|---|
| [BLOCKER-01](#blocker-01) | M0 — provider probe run against the real API | High | Operator | ✅ **Resolved 2026-08-25** |
| [BLOCKER-02](#blocker-02) | M0 — schema v1 migrated + seed working | High | Operator | Open |
| [BLOCKER-03](#blocker-03) | M1 — vertical slice on two physical devices | Medium | Operator | Open |
| [BLOCKER-04](#blocker-04) | Pre-launch — trademark clearance | High | Operator | Open |

---

## BLOCKER-01

✅ **RESOLVED 2026-08-25.** A free TMDB Developer key was issued to the operator's
account, stored in `.env` (gitignored, verified), and `cd server && go run
./cmd/probe tmdb` ran against the live API: **10/10 calls answered**. Observed
shapes are transcribed in `docs/INTEGRATIONS.md`.

**What the probe actually found** — recorded here because two results change
engineering plans rather than merely unblocking them:

- **Arabic works.** Named Ramadan musalsalat are present with Arabic-script
  titles (`الاختيار`, `ما وراء الطبيعة`, `لعبة نيوتن`, the `رامز` franchise).
  The §1.4 wedge is not built on sand.
- **But Arabic synopses are largely missing** — empty `overview` on **23 % of
  Arabic TV and 80 % of Arabic film** (sample: 60 most popular of each). TMDB
  does **not** fall back to English; it returns `""`. So ADR-012's curated-fields
  path is promoted from a supporting feature to the primary pipeline for Arabic
  film synopsis, with a real editorial cost that §1.4 must budget for.
- **TMDB exposes no rate-limit headers at all** — no `X-RateLimit-*`, no
  `Retry-After`, on any call. Our limiter therefore cannot be adaptive and must
  be a conservative static value (§2.4 R3).

**Still open, and tracked elsewhere:** TMDB's *terms* remain unread — see
`docs/LEGAL.md` **L2**. Probing an API establishes how it behaves, not what you
are permitted to do with it, and that still needs a human.

**Also note:** `cmd/seed --full` and the §10.4 ranking harness are now
*unblocked* but **not yet run**. The 200-title catalogue §16.3 requires depends
additionally on BLOCKER-02 (no live Postgres), so it stays undone.

---

## BLOCKER-02

**Docker engine will not start; WSL2 is missing a reboot.**

**Blocks:** M0 exit criterion *"schema v1 migrated + seed data script working"*.

**Current state.** Docker Desktop 4.88.0 is installed. `wsl --install
--no-launch` has been run elevated, but the Windows optional features it enables
do not take effect until the machine reboots. Until then:

```
Error response from daemon: Docker Desktop is unable to start
The Windows Subsystem for Linux is not installed.
```

**Consequence, stated plainly.** The eight migrations in `server/migrations/`
have **never been executed against a live Postgres**. They are authored,
reviewed, checksummed, and their loader is unit-tested — but the SQL itself is
unverified. Two things in particular need to actually run before anyone should
believe them:

- `vybe_normalize()`'s regular expression over the Arabic tashkeel range, and
  its `translate()` mapping of hamza carriers. A regex that compiles is not a
  regex that is correct.
- `uuid_generate_v7()`'s bit manipulation, which must produce a genuine
  version-7 UUID.

CI runs both of those as assertions against a real Postgres 17 service
(`.github/workflows/ci.yml`), so the first CI run will either confirm them or
fail loudly. **Neither has happened yet.**

**What this now also blocks (updated 2026-08-26).** The scope has grown since
this was written. There are now **36 integration tests** — `TestPG*` across
identity, rooms, and the idempotency store, plus `TestRedisTicket*` — and they
are the only cover the SQL and Redis paths get anywhere. They skip locally
without `VYBE_DB_DSN` / `VYBE_REDIS_ADDR`, which means a local `go test ./...`
is green while three whole files are untested. Three properties in particular
exist only inside them and cannot be demonstrated any other way:

- **`UPDATE rooms SET current_seq = current_seq + 1 RETURNING current_seq`
  actually serialises concurrent writers.** Eight goroutines join one room at
  once and must produce no gap and no duplicate seq. Whether Postgres's row
  lock delivers that is a question about Postgres, not about Go, and FR-28 is
  the one property in the system that cannot be recovered from once broken.
- **`Reserve`'s CTE is atomic.** Eight concurrent reservations of one
  idempotency key; exactly one must claim it. A read-then-write has a window
  where several see nothing and all proceed — the double-charge FR-57 exists
  to prevent.
- **Redis `GETDEL` is atomic.** Eight redemptions of one WS ticket; exactly one
  must succeed, or the ticket is a replayable credential sitting in a URL.

Because a run where all 36 silently skipped would still be *green*, CI now
asserts they ran and that none skipped. That check is what would catch a DSN
that quietly stopped reaching them — but it, too, has never run.

**Unblocking is one reboot and two commands:**

```bash
docker compose up -d db redis
cd server && go run ./cmd/migrate up
VYBE_DB_DSN='postgres://vybe:vybe@localhost:5432/vybe?sslmode=disable' \
VYBE_REDIS_ADDR=localhost:6379 go test ./internal/... -v -run 'TestPG|TestRedisTicket'
```

**Resolution:**
1. Reboot Windows.
2. `wsl --status` — confirm it reports a version. If a distribution is missing,
   `wsl --install -d Ubuntu`.
3. Launch Docker Desktop and accept the licence prompt.
4. `docker info --format '{{.ServerVersion}}'` should print a version.
5. `docker compose up -d db redis`
6. `cd server && go run ./cmd/migrate up && go run ./cmd/seed`

---

## BLOCKER-03

**No second physical device, and no Android emulator running.**

**Blocks:** M1 exit criterion — §2.1 demonstrated **on two physical devices** on
a throttled network, and E2E scenario §13.2.2 (realtime reconnect).

`flutter devices` currently reports Windows, Chrome, and Edge only.

**Why this is not negotiable.** §17 names *"a real-time demo that has only ever
run on one device"* as a way the project fails. Two clients on one machine share
a clock, share a network path, and share a process scheduler — which means they
cannot demonstrate the one thing Companion Sync claims. AC-1's ±250ms
convergence is not a meaningful assertion when both clients read the same
`DateTime.now()`.

**Resolution.** Any two of:
- Two physical Android phones over USB or wireless debugging (best — real
  clocks, real radios, real drift).
- One physical phone plus one Android emulator (acceptable; the emulator's
  clock is host-derived, so note it in the evidence).
- Two emulators (weakest — records as "not yet demonstrated on real hardware").

The 90-second walkthrough §16.3 asks for needs both screens visible side by
side with the network throttled. The reconnect moment is the money shot.

**Status update, 2026-08-26.** This has gone from "eventually" to "the only
thing left". The reconnect path it would demonstrate is now fully built on
both sides and tested in isolation:

- The server serves `DELTA` and `SNAPSHOT` correctly for all four resync
  decisions, including the case that is easy to conflate — a *small* gap whose
  events have aged out of retention cannot be served a delta at any threshold.
- The Dart client keeps its `lastSeq` across a transport death, mints a fresh
  ticket per attempt, dedupes the last 500 envelope ids, and **refuses a delta
  with a hole** rather than applying it and believing itself caught up.
- The hub drops a slow client rather than blocking the room, which is only
  safe *because* reconnection is lossless.

Every piece of that is tested. What is untested is the claim the project is
built on: that two devices, on real radios, with genuinely different clocks,
converge within ±250ms. No amount of single-machine testing substitutes,
because the failure mode being guarded against is precisely the one a shared
`DateTime.now()` hides.

---

## BLOCKER-04

**Trademark on "VYBE" has not been checked.**

**Blocks:** any public build, store submission, or further investment in
branding.

§1.9 requires this **before any design work**, because the cost of a rename
rises with every asset and listing that carries the name. "Vybe" and "Vibe" are
common word marks; a conflict is plausible.

See `docs/LEGAL.md` L3 for the specific checks required.

**Engineering mitigation already in place:** the name lives in three places —
`app/lib/l10n/*.arb` (`appTitle`), the bundle/application id, and the Universal
Link domain. A rename is hours, not days. It is deliberately not scattered.

---

## Resolved

| # | What unblocked it | Date | Evidence |
|---|---|---|---|
| [BLOCKER-01](#blocker-01) | Free TMDB Developer key issued; probe run against the live API | 2026-08-25 | 10/10 calls answered; shapes in [INTEGRATIONS.md](INTEGRATIONS.md) |
