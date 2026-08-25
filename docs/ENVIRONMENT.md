# Development environment

Observed on the machine this project was set up on, 2026-08-25. Anything marked
**blocker** stops a specific M0 exit criterion from being met and is tracked in
`docs/BLOCKERS.md`.

## Verified working

| Tool | Version | Notes |
|---|---|---|
| Flutter | 3.44.4 stable | `flutter doctor` reports no issues |
| Dart | 3.12.2 | |
| Android SDK | 36.0.0 | Toolchain complete |
| Go | 1.26.0 windows/amd64 | |
| Node | 24.13.0 | Tooling only; not a runtime dependency |
| Python | 3.14.5 | Spec traceability checks |
| Git | 2.49.0 | |
| Docker Desktop | 4.88.0 | Installed; engine **not** starting — see below |

## Known environment issue: spaces in the Flutter SDK path

**Symptom.** `flutter test` fails before running a single test:

```
'D:\Flutter' is not recognized as an internal or external command
Building native assets for package:objective_c failed.
```

**Cause.** The SDK is installed at `D:\Flutter Data\Futter\flutter`. The
`objective_c` package's native-assets build hook interpolates the SDK path into
a command without quoting it, so the path splits at the space. `objective_c`
arrives transitively via `path_provider_foundation`, and `sqlite3` needs the
same feature, so `flutter config --no-enable-native-assets` is not an escape —
both packages then refuse to build at all.

**Workaround in use.** A directory junction with no spaces. Junctions need no
administrator rights and change nothing about the original install:

```powershell
New-Item -ItemType Junction -Path "D:\flutter-sdk" -Target "D:\Flutter Data\Futter\flutter"
```

Then invoke Flutter through the junction:

```bash
/d/flutter-sdk/bin/flutter.bat test test/unit/
/d/flutter-sdk/bin/flutter.bat analyze
```

Verified: 28/28 unit tests pass and `flutter analyze` reports no issues through
the junction, while the same commands through the original path fail.

**Proper fix.** Reinstall or move the SDK to a path with no spaces (for example
`D:\flutter`) and delete the junction. Until then, CI is unaffected — GitHub's
runners install the SDK at a space-free path — so this is a local-only
annoyance, not a project defect.

## Blocker: Docker engine will not start

**Symptom.**

```
Error response from daemon: Docker Desktop is unable to start
failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine
```

**Cause.** WSL2 is not installed:

```
The Windows Subsystem for Linux is not installed.
```

Docker Desktop on Windows requires the WSL2 backend (or Hyper-V). `wsl --install
--no-launch` was run elevated; the optional Windows features it enables **do not
take effect until the machine is rebooted**.

**Action required from the operator:**

1. Reboot Windows.
2. Run `wsl --status` and confirm it reports a version rather than "not
   installed". If a distribution is still missing, run `wsl --install -d Ubuntu`.
3. Launch Docker Desktop and accept its licence prompt on first run.
4. Verify: `docker info --format '{{.ServerVersion}}'` prints a version.
5. Then: `docker compose up -d db redis`.

**What this blocks.** The M0 exit criterion *"schema v1 migrated + seed data
script working"*. The migrations in `server/migrations/` have **not been
executed against a live Postgres**, so the schema is authored but unverified —
in particular `vybe_normalize()`'s regular expression over the Arabic tashkeel
range, and the `uuid_generate_v7()` bit manipulation, are exactly the kind of
SQL that needs to run before anyone believes it. Nothing in this repository
claims otherwise.

**What it does not block.** Everything verified above: the Go domain logic and
its tests, the Flutter core and its tests, static analysis on both, and all
documentation.

## No TMDB API key present

`TMDB_API_KEY` is unset. Per Master Prompt v2 §0.3 rule 1, `docs/INTEGRATIONS.md`
records **no provider response shapes** until the probe has run against the live
API. Tracked as BLOCKER-01. A free key is available from
<https://www.themoviedb.org/settings/api>; put it in `.env` and run
`go run ./cmd/probe tmdb`.

## Note on an unrelated key found in the shell environment

A `GEMINI_API_KEY` is exported in the operator's shell. VYBE does not use it and
must not: it is unrelated to this project, and `.gitignore` excludes `.env` so
it cannot be committed by accident. It is mentioned here only so that its
presence in a terminal transcript is not mistaken for a project credential.
