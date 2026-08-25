/// The failure hierarchy of Master Prompt v2 §4.4.
///
/// Two rules govern everything in this file:
///
/// 1. **No exception crosses a repository boundary.** Repositories return
///    `Result<T>`; data sources may throw, and the repository translates.
/// 2. **A `Failure` carries no user-facing text.** It carries a *cause*.
///    Turning a cause into a sentence is the sole job of `FailurePresenter`,
///    which is also the only place that text gets localised. Putting a message
///    here would put an untranslatable English string in the domain layer.
///
/// Sealed, so `switch` over a Failure is exhaustive and adding a new variant
/// becomes a compile error at every handling site rather than a silent
/// fall-through to "something went wrong".
library;

sealed class Failure {
  const Failure();

  /// A stable, machine-readable identifier for logs and analytics.
  /// Never shown to a user.
  String get code;
}

/// The request never reached the server, or the response never came back.
final class NetworkFailure extends Failure {
  const NetworkFailure({required this.isOffline, this.cause});

  /// True when the device knows it has no connectivity, false when the request
  /// failed for another transport reason (timeout, DNS, TLS).
  ///
  /// The distinction is not cosmetic: offline means "queue it if the entity is
  /// queueable" (§11.3), while a timeout on a connected device means "retry".
  final bool isOffline;
  final Object? cause;

  @override
  String get code => isOffline ? 'OFFLINE' : 'NETWORK';
}

/// The server responded, and it was not a success.
///
/// [code] is the stable `code` field from the RFC 9457 problem document
/// (§5.2) — not the HTTP status, and not a message. Clients branch on it, so
/// it is part of the API contract.
final class ServerFailure extends Failure {
  const ServerFailure({
    required this.status,
    required this.code,
    this.traceId,
    this.detail,
  });

  final int status;

  @override
  final String code;

  /// Propagated from the problem document so a user-reported issue can be
  /// found in the server logs (§14.2).
  final String? traceId;

  /// The server's own `detail`. Diagnostic only — it is not localised, so it
  /// must never be rendered to a user. `FailurePresenter` ignores it.
  final String? detail;
}

/// Why an authentication-related failure happened. The reasons differ in what
/// the app must *do*, which is why this is not one flat "auth failed".
enum AuthReason {
  /// Access token expired. The interceptor refreshes silently; the user sees
  /// nothing (§13.2.6 / EC-7).
  tokenExpired,

  /// Refresh failed, or reuse was detected and the family was revoked
  /// (ADR-011 / EC-8). The user must sign in again, and must be told why —
  /// silently bouncing them to a login screen is the §3.2 "no silent
  /// redirect" violation.
  sessionRevoked,

  /// Credentials were wrong at sign-in.
  invalidCredentials,

  /// The endpoint requires a signed-in user and there is none.
  notAuthenticated,

  /// Signed in, but not permitted. Kept distinct from [notAuthenticated]
  /// because offering "sign in" to someone already signed in is nonsense.
  forbidden,
}

final class AuthFailure extends Failure {
  const AuthFailure(this.reason);

  final AuthReason reason;

  @override
  String get code => 'AUTH_${reason.name.toUpperCase()}';
}

/// Field-level validation, straight from the problem document's `errors[]`.
///
/// The map is field name to a stable error code — again not a message, so the
/// form can render a localised string per field.
final class ValidationFailure extends Failure {
  const ValidationFailure(this.fields);

  final Map<String, String> fields;

  @override
  String get code => 'VALIDATION';
}

/// §12.3. Carries the real wait, because §3.2 requires the UI to show time
/// until retry rather than "try again later".
final class RateLimitFailure extends Failure {
  const RateLimitFailure({required this.retryAfter, this.scope});

  final Duration retryAfter;

  /// Which limit was hit (`chat`, `search`, `room_create`), so the message can
  /// be specific.
  final String? scope;

  @override
  String get code => 'RATE_LIMITED';
}

/// What kind of conflict occurred. Each maps to a different recovery, which is
/// exactly why ADR-008 refuses a single global resolution rule.
enum ConflictKind {
  /// Another writer changed the resource first.
  staleWrite,

  /// The action was already performed — an idempotent replay (§5.2).
  /// Usually not an error the user should ever see.
  duplicate,

  /// The entity is in a state that forbids the transition (FR-15).
  invalidState,

  /// The room reached its participant cap (FR-16).
  roomFull,

  /// The room has ended. Gets a dedicated screen, not a generic error (§3.2).
  roomEnded,
}

final class ConflictFailure extends Failure {
  const ConflictFailure(this.kind);

  final ConflictKind kind;

  @override
  String get code => 'CONFLICT_${kind.name.toUpperCase()}';
}

/// The action cannot be queued while offline (§11.3).
///
/// A chat message, reaction, room join, trivia answer, or prediction submitted
/// three hours late is not the action the user meant. Global Master Prompt §53
/// forbids the alternative: faking success and dropping it.
final class RequiresConnectionFailure extends Failure {
  const RequiresConnectionFailure(this.action);

  final String action;

  @override
  String get code => 'REQUIRES_CONNECTION';
}

/// Something we did not anticipate.
///
/// This exists so that no code path is tempted to swallow an unknown error to
/// keep a build green (Global Master Prompt §51). Every construction site
/// reports to crash reporting.
final class UnexpectedFailure extends Failure {
  const UnexpectedFailure(this.cause, [this.stackTrace]);

  final Object cause;
  final StackTrace? stackTrace;

  @override
  String get code => 'UNEXPECTED';
}
