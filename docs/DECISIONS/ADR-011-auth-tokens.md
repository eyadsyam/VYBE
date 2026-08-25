# ADR-011: Ed25519 JWT access tokens, rotating opaque refresh with reuse detection

Status: Accepted
Date: 2026-08-25

## Context

Not in the master prompt's required list, but §5.3 and §12.1 specify the
behaviour precisely enough that the choices behind it deserve recording — in
particular *why* the two token types are deliberately different in kind.

Mobile-specific forces: the app is offline-tolerant, so a token expiring mid-room
must not interrupt anything (§13.2.6); the binary is assumed compromised
(§12.1); and the WebSocket handshake needs its own credential because a token in
a query string lands in access logs and proxy logs forever.

## Options considered

### Access token

| Option | Pros | Cons |
|---|---|---|
| Opaque, validated against the DB | Instantly revocable | A database round trip on every request, including every socket message |
| **JWT, asymmetric (Ed25519)** | Stateless verification; the public key can be distributed; small signatures (64 bytes) | Not revocable before expiry — mitigated by a 15-minute TTL |
| JWT, symmetric (HS256) | Simplest | Every verifier needs the signing secret. Extracting the realtime tier later (§7.9) would mean sharing the ability to *mint* tokens, not just verify them. |

Ed25519 over RS256: 64-byte signatures instead of 256, faster verification, no
key-size footguns, and no risk of the RSA `alg` confusion class of bugs.

### Refresh token

| Option | Pros | Cons |
|---|---|---|
| Long-lived JWT | Stateless | Cannot revoke; theft is permanent until expiry. Unacceptable for a 60-day credential. |
| Opaque, static | Revocable | Theft is undetectable — attacker and victim use the same value indefinitely |
| **Opaque, rotating, with reuse detection** | Revocable *and* theft is detectable | Requires a token-family table and careful handling of the legitimate-race case |

## Decision

- **Access token:** JWT signed Ed25519 (`EdDSA`), **15 minutes**, minimal claims
  only — `sub`, `sid`, `entitlement_tier`, `iat`, `exp`, `jti`. No email, no
  display name, no roles. It is a bearer credential that will end up in logs and
  crash reports somewhere; it must be boring.
- **Refresh token:** opaque, 32 bytes from a CSPRNG, **60 days**, stored
  **hashed** (SHA-256) so a database leak does not yield usable tokens.
  **Rotating**: every refresh issues a new token and invalidates the old one.
- **Reuse detection:** tokens form a *family* sharing a `family_id`. Presenting
  an already-rotated token means one of two things — a legitimate client retried
  after a network failure, or the token was stolen. We cannot distinguish them,
  so we assume theft: **revoke the entire family, invalidate every session in
  it, and alert.** The user re-authenticates. Annoying once, versus an attacker
  holding a 60-day credential.
- **Sessions are first-class rows** (§5.3): device name, platform, truncated IP,
  created-at, last-seen. The user can list and revoke them individually.
- **WebSocket auth:** a **single-use, 60-second ticket** issued over HTTPS and
  redeemed in the WS handshake. The access token is **never** sent as a query
  parameter. The ticket is consumed atomically on redemption (Redis `GETDEL`),
  so a captured ticket is worthless.
- **Password reset:** single-use, 15-minute, random (not UUIDv7 — see ADR-010),
  stored hashed, and **invalidates all sessions on success**.

## Consequences

**Becomes easy**

- Mid-room token expiry is invisible: the client refreshes silently, gets a new
  WS ticket, and the socket never drops (§13.2.6).
- Extracting the realtime tier later means shipping it the Ed25519 *public* key.
  It can verify; it can never mint.

**Becomes hard**

- The legitimate-retry case produces real logouts. Mitigation: the client
  serialises refresh attempts through a single-flight lock so two concurrent
  401s cannot trigger two refreshes; and the previous token stays valid for a
  10-second overlap window after rotation, which covers in-flight retries
  without meaningfully widening the theft window.
- Revocation within the 15-minute access window is not possible. Accepted: for
  an immediate ban, the moderation path also kills the socket and the session
  row, so the blast radius is API-only and short.

**Key rotation** is a documented runbook, not an aspiration: the JWKS endpoint
serves both old and new public keys during an overlap of one access-token TTL,
then the old key is dropped. See `docs/RUNBOOKS/key-rotation.md`.
