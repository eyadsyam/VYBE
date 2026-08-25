# ADR-008: Per-entity offline conflict resolution, not one global rule

Status: Accepted
Date: 2026-08-25

## Context

The app queues mutations while offline (§11.3). When connectivity returns, the
queued mutation may conflict with a change made elsewhere — a second device, or
the server itself. A single global strategy is tempting and wrong: the correct
resolution for *watch progress* is provably different from the correct
resolution for *XP*.

A worked case from §16.4.8: a user favourites a title offline on their phone,
then favourites the same title on a tablet. What should happen?

## Options considered

| Option | Why it fails |
|---|---|
| **Global last-write-wins** | Applied to watch progress, it lets a stale device *rewind* your position. You cannot un-watch something; LWW here destroys real information. |
| **Global server-wins** | Applied to favourites, the user's offline action silently vanishes. The app lied when it showed the optimistic tick. |
| **Global client-wins** | Applied to XP or scores, the client is now authoritative over its own score. This directly contradicts ADR-004. |
| **Ask the user every time** | Correct but unusable. Nobody wants a merge conflict dialog for a favourite. |
| **Per-entity strategy** | More code, more tests, and each rule must be justified — but it is the only option that is correct for every entity |

## Decision

**Per-entity strategies, declared in a table that is the specification** (§11.4):

| Entity | Strategy | Justification |
|---|---|---|
| Watch progress | **Max position wins** | Monotonic. You cannot un-watch. Max is the only lossless merge. |
| Favourites / lists | Last-write-wins by client timestamp, server breaks ties | Set membership; the user's most recent intent is the right answer. Both devices favouriting converges to "favourited" — no conflict exists in practice. |
| Profile fields | Server wins, **and the user is shown the diff** | Prevents silent data loss without a modal; the user can re-apply. |
| Follows | Idempotent set semantics | Conflict is not representable. Following twice equals following once. |
| Reports | Append-only | Never conflicts. Two reports are two reports. |
| Anything the server computes — XP, scores, achievements, leaderboard | **Server wins, unconditionally** | ADR-004. Not negotiable, not overridable, no exception. |

Supporting rule, and the one that actually prevents double-application: **the
idempotency key is generated when the action is enqueued, not when it is sent**
(§11.3). A request that times out but in fact succeeded, then retries, replays
the stored response instead of applying twice.

**What is never queued** is as much a part of this decision as what is: room
join, chat message, reaction, trivia answer, prediction. A trivia answer
submitted three hours late is not an answer. These show "requires connection"
rather than faking success — §53 of the Global Master Prompt forbids the silent
fallback.

## Consequences

**Becomes easy**

- Each rule is a pure function, unit-testable with no I/O, and lands directly in
  the §13.1 90%-coverage domain layer.
- The answer to interview question §16.4.8 is a table entry with a reason, not
  an improvisation.

**Becomes hard**

- Six strategies mean six sets of tests, including the awkward ones (clock skew
  between two client devices for the LWW entities).
- Adding a new synced entity is not free: it must pick a strategy explicitly.
  The outbox handler is written so that an entity without a declared strategy
  **fails to compile**, rather than silently defaulting.

**Revisit when** an entity appears whose correct merge is genuinely a set union
or counter increment — at that point a CRDT for that one entity may be
justified. Not before, and not globally.
