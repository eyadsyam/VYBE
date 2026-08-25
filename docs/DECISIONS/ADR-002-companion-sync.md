# ADR-002: Companion Sync — synchronise the clock, not the stream

Status: Accepted
Date: 2026-08-25

## Context

Master Prompt v1 asked for two mutually exclusive things: never serve
copyrighted video, and synchronise playback of that video. §1.2 of v2 identifies
this as the project's central contradiction. It must be resolved before any room
code is written, because the answer determines the entire realtime model.

The hard constraint: **VYBE does not control the video pipeline.** Netflix,
Shahid, Disney+, and YouTube expose no sanctioned API for a third party to read
or drive playback position on mobile. There is no legal mechanism to know where
the user is in an episode, and no legal mechanism to move them.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **A. Host / proxy the video** | True frame sync, full control | Illegal without licences we do not have; guaranteed store rejection (§1.9 R2); the project ends | Rejected outright |
| **B. Accept user-supplied stream URLs** | Trivial to build; "works" in a demo | This is the architecture of a piracy app, and store review reads it that way regardless of intent. Also a raw SSRF and abuse surface. | Rejected outright |
| **C. Screen-capture / accessibility scraping of the other app** | Would yield a real position | Prohibited by both platforms' policies and every provider's ToS; breaks on every provider update; requires invasive permissions | Rejected |
| **D. Provider "watch together" SDKs** | Sanctioned where they exist | Only a couple of providers offer anything, only inside their own apps, none to a third-party mobile client. Not a general solution. | Rejected — no viable API |
| **E. Companion Sync — a server-authoritative virtual clock** | Legal everywhere; provider-agnostic; works with services that will never have an API; the genuinely interesting distributed-systems problem | Accuracy depends on user cooperation at the start ritual, and drifts if someone pauses. Requires drift measurement and a resync affordance. | **Chosen** |
| **F. Async rooms only** — everyone watches, then discusses | Zero sync problem | Removes the live, competitive product entirely | Kept as mode C, not as the primary |

## Decision

**Companion Sync is the primary mode.** The room maintains a *shared virtual
timeline*, not a video stream:

```
t_room = (server_now - anchor_server_time) + anchor_offset_ms
```

`server_now` is the client's local clock corrected by a measured offset
(ADR-003, §7.4). The user starts playback in their own app during a
server-driven countdown ritual (§3.3). Every timed feature — trivia beats,
prediction windows, spoiler-gated chat, reaction aggregation — fires against
`t_room`, never against a video element.

Two secondary modes are retained: **Clip Sync** (true position sync, but only
over trailers and clips VYBE is licensed to serve or embeds via a provider's own
sanctioned player) and **Async Room** (no live sync at all).

## Consequences

**Becomes easy**

- Legality. VYBE never touches a frame of licensed video, so §1.9's critical
  store-rejection risk is designed out rather than mitigated.
- Provider coverage is universal. A service with no API works exactly as well as
  one with an API, because we do not use the API.
- The entire feature set becomes testable without any video at all: the timeline
  is a pure function of two timestamps.

**Becomes hard**

- **Drift is now a first-class product problem, not an edge case.** A user who
  pauses to answer the door is silently wrong until they say so. Mitigations
  (§7.4): per-client clock offset via an NTP-style handshake, a visible "I'm out
  of sync" affordance with a ±5s nudge, a host re-anchor event, and a ±1.5s
  tolerance window on timed events, with anything outside it skipped and logged.
- The UI must be honest that VYBE cannot see the user's player. The countdown
  ritual is the contract; the product must not imply more than it knows.

**Revisit when** a provider ships a genuine third-party co-watching API. That
provider can then be promoted to Clip Sync fidelity without changing the
timeline abstraction — `t_room` simply gains a more accurate anchor source.

This ADR is the answer to interview question §16.4.1, and the reason the answer
to §16.4.2 ("a client's clock is wrong by five minutes") is "nothing breaks."
