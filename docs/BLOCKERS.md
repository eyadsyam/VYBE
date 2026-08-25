# Blockers

Live register of things that stop a milestone exit criterion from being met.
§0.4 requires halting and reporting rather than continuing past them, and §0.5
requires an honest "what is NOT done" list at every work unit.

A blocker leaves this file only when the criterion it blocks is **verified**,
not when it is worked around.

| # | Blocks | Severity | Owner | Status |
|---|---|---|---|---|
| [BLOCKER-01](#blocker-01) | M0 — provider probe run against the real API | High | Operator | Open |
| [BLOCKER-02](#blocker-02) | M0 — schema v1 migrated + seed working | High | Operator | Open |
| [BLOCKER-03](#blocker-03) | M1 — vertical slice on two physical devices | Medium | Operator | Open |
| [BLOCKER-04](#blocker-04) | Pre-launch — trademark clearance | High | Operator | Open |

---

## BLOCKER-01

**No TMDB API key, so no provider probe has run.**

**Blocks:** M0 exit criterion *"provider integration probe run against the real
API"*. Also blocks `go run ./cmd/seed --full`, and therefore the 200-title
catalogue that §16.3 requires and that the §10.4 ranking harness needs to
produce a meaningful number.

**Why it is not worked around.** §0.3 rule 1 forbids inventing third-party API
behaviour. `cmd/probe` deliberately has no offline mode and `cmd/seed --full`
deliberately refuses: a canned fixture or 200 invented titles would let the
catalogue adapter be written against a schema nobody has observed, and the
ranker's nDCG@10 would then be computed against fiction and quoted as evidence.

`docs/INTEGRATIONS.md` therefore records **zero** TMDB response shapes.

**Resolution:**
1. Get a free key: <https://www.themoviedb.org/settings/api>
2. Put `TMDB_API_KEY=...` in `.env` (gitignored).
3. `cd server && go run ./cmd/probe tmdb`
4. Transcribe the observed shapes from `tools/probe/out/tmdb.json` into
   `docs/INTEGRATIONS.md`.
5. Read TMDB's current terms — see `docs/LEGAL.md` L2, which is the same
   dependency from the legal side.

**Note.** The probe deliberately includes three Arabic-script queries. TMDB's
MENA coverage is the single biggest unknown behind the §1.4 launch wedge, and
the probe is designed to answer it with evidence rather than optimism. If
coverage turns out to be poor, that is a **product finding**, and it should
change the plan rather than be quietly absorbed.

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

*(none yet)*
