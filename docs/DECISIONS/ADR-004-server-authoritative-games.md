# ADR-004: Server-authoritative game logic — the client has zero authority

Status: Accepted
Date: 2026-08-25

## Context

VYBE has a scoreboard, and a scoreboard is a target. §12.1 states the working
assumption plainly: **assume the client binary is fully compromised.** A Flutter
release APK can be decompiled; a rooted device can set its clock to any value;
an HTTP client can replay any captured request; a modified client can send any
payload it likes.

The question is not whether to validate on the server — of course we do. The
question is *where the truth lives* while a trivia question is open, and what is
allowed to travel to the device.

## Options considered

| Option | Description | Why it fails / holds |
|---|---|---|
| **A. Client scores, server records** | Client knows the answer key, computes its own score, posts the result | Fails immediately. The answer key is in the payload; a trivial patch prints it. Every score is a claim. |
| **B. Client scores, server re-validates** | Client computes, server recomputes and compares, rejects mismatches | Still sends the answer key to the device. A cheat does not need to lie about the score — it just needs to *know the answer*, which we handed over. |
| **C. Server scores, client timing** | Answer key stays server-side, but elapsed time is taken from `client_ts` | Fails on device clock. A user who sets their clock back 60s claims maximum speed bonus on every question. |
| **D. Server scores, server timing, no key on the wire** | The device receives only the question and options; timing is measured at server receipt; the key is revealed only at close | **Chosen.** Nothing valuable ever reaches a compromised surface. |

## Decision

**Option D, applied to every game surface: trivia, predictions, XP, and
achievements.** The rules, verbatim from §8.1:

1. `QUESTION_OPEN` carries `{ question_id, text, options[{id,text}], deadline_ts,
   timeline_ms, nonce }`. It carries **no correct-answer field and no points
   formula**. Not encrypted, not obfuscated — *absent*.
2. Timing is `server_receive_time`, never `client_ts`. `client_ts` is accepted
   and logged as telemetry only, never used for scoring.
3. Elapsed time is RTT-compensated:
   `elapsed = server_receive_time - question_open_time - (rtt/2)`, clamped >= 0.
4. A per-user, per-question `nonce` prevents replaying another user's captured
   request.
5. Uniqueness on `(session_id, user_id, question_id)` is enforced **by a
   database constraint**, not by an application `if`. Two concurrent submissions
   race in the database, and exactly one wins.
6. A 400ms grace window past the deadline compensates network transit — trivia
   only. Predictions get **no grace** (§8.3), because there the late arrival
   *is* the cheat: late information is worth something.

The trust boundary is memorised as a list (§12.2). Never trusted from the
client: score, XP, room role, trivia correctness, timing, prediction state,
entitlement tier, achievement eligibility, leaderboard position, membership,
block state. Trusted with validation: UI preferences, locale, theme, and
non-authoritative telemetry.

## Consequences

**Becomes easy**

- Every one of the ten §8.2 anti-cheat cases becomes testable as a plain API
  test, because each maps to a specific server-side rule.
- Fairness on slow networks is a *property*, not a patch: RTT compensation means
  a user in Aswan on 3G is not penalised against a user on fibre.
- Client releases can ship independently. The client renders; it decides nothing.

**Becomes hard**

- Every scoring interaction is a network round trip. There is no optimistic
  local scoring, so the UI must show "submitted" and then "result", not an
  instant score. This is a deliberate UX cost paid for integrity.
- The server must hold per-question open-time and per-user nonces for the life
  of a session. That is Redis state (ADR-009) with the session row in Postgres
  as the durable record.

**Revisit** — do not. This is the load-bearing decision of the game layer; the
answer to interview questions §16.4.4 and §16.4.6 depends on it. If a future
feature seems to require client authority, the feature is wrong.
