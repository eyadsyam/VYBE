# ADR-001: Riverpod for Flutter state management and dependency injection

Status: Accepted
Date: 2026-08-25

## Context

Master Prompt v2 §4.3 requires this decision be scored, not defaulted, and
forbids mixing paradigms within a layer. The forces specific to VYBE:

- **The app is stream-heavy.** A live room holds a WebSocket event stream, a
  presence stream, a corrected-clock ticker, and a trivia countdown — all
  concurrently, all needing cancellation the instant the user leaves the room.
- **Leaks are the top failure mode.** §4.6 requires "timers, subscriptions,
  controllers disposed in every ViewModel, verified by a leak test." A room the
  user left must not keep a socket subscription alive.
- **Domain logic must be unit-testable without a widget tree** (§13.1 gates the
  domain layer at ≥90% coverage).
- **Feature count is high** (13 backend modules, ~5 tabs), so per-feature
  boilerplate compounds.

## Options considered

Scored against §4.3's weighted criteria. Scores are 1–10, argued below the table.

| Criterion | Weight | Riverpod 3 (+ codegen) | Bloc 9 (+ get_it) |
|---|---|---|---|
| Async/stream state ergonomics | 25% | **9** | 7 |
| Testability without a widget tree | 20% | 9 | **9** |
| Compile-time safety of DI | 15% | **9** | 5 |
| Disposal / lifecycle correctness | 15% | **9** | 7 |
| Boilerplate per feature | 15% | **8** | 5 |
| Ecosystem + hiring familiarity | 10% | 7 | **9** |
| **Weighted total** | | **8.65** | **7.00** |

**Async/stream ergonomics.** `StreamProvider` + `AsyncValue` model
loading/data/error as one sealed value, which is exactly the §4.1 requirement
for an exhaustive state object. Bloc reaches the same place but the developer
manages `StreamSubscription` lifetimes by hand inside each bloc — the precise
thing we are trying to make impossible.

**Compile-time DI.** `@riverpod` code generation produces typed provider
references; a missing dependency is a compile error. `get_it` resolves by type
at runtime, so a missing registration is a crash in a room at 9pm. With 13
modules this difference is not academic.

**Disposal.** `autoDispose` + `ref.onDispose` tie a socket subscription's
lifetime to the provider's last listener. Under Bloc the equivalent discipline
is `close()` plus correct `BlocProvider` scoping, which is correct-by-review
rather than correct-by-construction.

**Where Bloc genuinely wins.** `bloc_test` is a better-documented testing story
than `ProviderContainer`, and Bloc is more commonly seen in Flutter job
descriptions. Neither outweighs lifecycle correctness for a realtime app.

## Decision

**Riverpod 3 with `riverpod_generator`, used for both state management and
dependency injection.** Presentation state lives in `@riverpod` Notifier classes
(the "ViewModel" of §4.1). Repositories, data sources, and clients are also
providers, so the whole graph is overridable in tests via
`ProviderContainer(overrides: [...])`.

Deciding factor: **`autoDispose` makes the room-teardown path correct by
construction rather than by review.** That is the single highest-risk path in
the product.

Consequences of the "never mix paradigms" rule (§4.3): `package:provider`,
`get_it`, `BlocProvider`, and raw `setState`-for-business-state are **banned**.
`setState` remains legal for purely local widget affordances (an expansion
toggle, a text field's obscure flag) that never leave the widget.

## Consequences

**Becomes easy**
- Overriding any dependency in a widget or unit test with one line.
- Cancelling every room subscription on navigation away — no manual bookkeeping.
- Deriving state (`ref.watch(a.select((s) => s.field))`) for narrow rebuilds, which §4.6 requires.

**Becomes hard**
- Code generation is now on the critical path: `build_runner` must run in CI and
  generated files are gitignored. A stale build produces confusing errors.
- Provider dependency cycles are a real failure mode and the error message is
  poor. Mitigation: `core/` never depends on `features/`.

**Revisit when**
- A leak test demonstrates `autoDispose` is not firing for the room graph, or
- Riverpod's generator becomes a build-time bottleneck (>60s incremental).

---

## Amendment, 2026-08-25 — code generation dropped; the decision stands

**Observed, not assumed.** On resolving dependencies at the versions current
today, `riverpod_generator` >= 4.0.6 requires `analyzer` ^13.0.0, while
`drift_dev` 2.34 caps `analyzer` at 12.x. There is no version pair that
satisfies both. The full resolver output is in the commit that introduced
`app/pubspec.yaml`.

Given a forced choice between the two generators:

- **`drift_dev` is kept.** It generates the entire typed DAO and query layer;
  hand-writing it would be thousands of lines of exactly the mapping code that
  §4.1 wants generated.
- **`riverpod_generator` is dropped.** Providers are written by hand as
  `NotifierProvider` / `StreamProvider` / `Provider` declarations.

**Why this does not weaken the decision.** Re-reading the scoring table above,
the generator was contributing convenience, not any of the deciding properties:

| Property that decided ADR-001 | Still true without codegen? |
|---|---|
| `autoDispose` ties socket/timer lifetime to listeners | **Yes** — it is a provider modifier, not a generator feature |
| Compile-time DI safety | **Yes** — a hand-declared `final fooProvider = ...` is still a typed reference; a missing dependency is still a compile error |
| `ProviderContainer(overrides:)` testability | **Yes** — unchanged |
| `AsyncValue` sealed loading/data/error | **Yes** — a runtime type, not generated |
| Boilerplate per feature | **Slightly worse** — roughly three extra lines per provider |

Only the last row moves, and it moves the smallest-weighted criterion (15%) by a
small amount. Re-scored, Riverpod drops from 8.65 to about 8.50 and still wins
comfortably.

**`freezed` is also dropped**, for the same analyzer conflict. §4.1 asks for
"sealed classes / `freezed` unions" — Dart 3.12 has native `sealed` classes with
exhaustive `switch` pattern matching, which satisfies the requirement directly.
The cost is hand-written `==`, `hashCode`, and `copyWith` on state classes; the
benefit is one less generator on the critical path.

**Revisit when** `drift_dev` gains analyzer 13 support, at which point
`riverpod_generator` can be reintroduced without changing any provider's
semantics — the hand-written declarations and the generated ones produce the
same provider types.
