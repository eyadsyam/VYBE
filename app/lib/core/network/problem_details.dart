/// RFC 9457 `application/problem+json` → [Failure] (§4.4, §5.2, FR-58).
///
/// This is the client half of a contract whose server half is
/// `server/internal/platform/httpx`. The two must agree on the `code`
/// vocabulary, and they are kept in step by both being enumerated in one place:
/// there, `ErrBadRequest` and friends; here, [ProblemCode].
///
/// The mapping branches on **`code`**, never on the HTTP status alone. Five
/// different 409s are all "409", and §3.2 requires the room-ended case to get a
/// dedicated screen rather than a generic conflict message — which is only
/// possible if the client can tell them apart.
library;

import '../error/failure.dart';

/// The stable machine-readable codes the server emits.
///
/// Mirrors the vocabulary block in `httpx/problem.go`. Anything not listed here
/// still maps to a usable [Failure] via the status fallback — an unknown code
/// must degrade, never crash, because a server deploy can legitimately add one
/// before the app that understands it has shipped.
abstract final class ProblemCode {
  static const badRequest = 'BAD_REQUEST';
  static const validationFailed = 'VALIDATION_FAILED';
  static const unauthorized = 'UNAUTHORIZED';
  static const tokenExpired = 'TOKEN_EXPIRED';
  static const forbidden = 'FORBIDDEN';
  static const notFound = 'NOT_FOUND';
  static const conflict = 'CONFLICT';
  static const rateLimited = 'RATE_LIMITED';
  static const internal = 'INTERNAL';
  static const unavailable = 'UNAVAILABLE';

  // Idempotency (FR-57).
  static const idempotencyKeyRequired = 'IDEMPOTENCY_KEY_REQUIRED';
  static const idempotencyKeyInvalid = 'IDEMPOTENCY_KEY_INVALID';
  static const idempotencyKeyReused = 'IDEMPOTENCY_KEY_REUSED';
  static const idempotencyInFlight = 'IDEMPOTENCY_IN_FLIGHT';

  // Pagination (FR-59).
  static const offsetPaginationUnsupported = 'OFFSET_PAGINATION_UNSUPPORTED';
  static const cursorInvalid = 'CURSOR_INVALID';
  static const limitInvalid = 'LIMIT_INVALID';

  // Rooms (FR-11–18).
  static const roomEnded = 'ROOM_ENDED';
  static const roomFull = 'ROOM_FULL';
  static const notParticipant = 'NOT_PARTICIPANT';

  // Trivia (§8.4).
  static const duplicateAnswer = 'DUPLICATE_ANSWER';
  static const questionClosed = 'QUESTION_CLOSED';
  static const invalidNonce = 'INVALID_NONCE';

  // Auth session lifecycle (ADR-011).
  static const sessionRevoked = 'SESSION_REVOKED';
  static const invalidCredentials = 'INVALID_CREDENTIALS';
}

/// A parsed problem document.
///
/// Deliberately tolerant: every field is optional in the parser even though the
/// server always sends the required seven. A proxy returning its own HTML 502,
/// or a captive portal injecting a login page, must produce a [Failure] rather
/// than a `TypeError` from a cast on a missing key.
class ProblemDetails {
  const ProblemDetails({
    required this.status,
    required this.code,
    this.type,
    this.title,
    this.detail,
    this.traceId,
    this.errors = const [],
    this.retryAfterSeconds,
  });

  final int status;
  final String code;
  final String? type;
  final String? title;
  final String? detail;
  final String? traceId;
  final List<ProblemFieldError> errors;

  /// From the server's `retryAfterSeconds` extension member, which §3.2 needs
  /// so the UI can show time until retry rather than "try again later".
  final int? retryAfterSeconds;

  /// Parses a decoded JSON body.
  ///
  /// [status] is passed separately and wins over any `status` in the body: the
  /// transport's status is the one that actually happened, and a body claiming
  /// otherwise is either a bug or a hostile response.
  static ProblemDetails parse(Object? body, int status) {
    if (body is! Map) {
      // Not a problem document at all — an HTML error page, a plain string, or
      // an empty body. Still has to become a Failure.
      return ProblemDetails(status: status, code: _codeForStatus(status));
    }

    final map = body;
    final code = _asString(map['code']);

    return ProblemDetails(
      status: status,
      code: code == null || code.isEmpty ? _codeForStatus(status) : code,
      type: _asString(map['type']),
      title: _asString(map['title']),
      detail: _asString(map['detail']),
      traceId: _asString(map['traceId']),
      errors: _parseErrors(map['errors']),
      retryAfterSeconds: _asInt(map['retryAfterSeconds']),
    );
  }

  /// Converts to the domain failure type (§4.4).
  ///
  /// [isOffline] is supplied by the caller because the network layer knows it
  /// and this parser does not.
  Failure toFailure() {
    switch (code) {
      case ProblemCode.tokenExpired:
        // Not surfaced to the user: the interceptor refreshes and replays
        // (§13.2.6 / EC-7).
        return const AuthFailure(AuthReason.tokenExpired);

      case ProblemCode.sessionRevoked:
        return const AuthFailure(AuthReason.sessionRevoked);

      case ProblemCode.invalidCredentials:
        return const AuthFailure(AuthReason.invalidCredentials);

      case ProblemCode.unauthorized:
        return const AuthFailure(AuthReason.notAuthenticated);

      case ProblemCode.forbidden:
      case ProblemCode.notParticipant:
      case ProblemCode.invalidNonce:
        return const AuthFailure(AuthReason.forbidden);

      case ProblemCode.validationFailed:
        return ValidationFailure({
          for (final e in errors) e.field: e.code,
        });

      case ProblemCode.rateLimited:
        return RateLimitFailure(
          // A missing hint is not an excuse to retry immediately. TMDB's probe
          // showed a real provider returning no Retry-After at all
          // (docs/INTEGRATIONS.md), so a sane default is not hypothetical.
          retryAfter: Duration(seconds: retryAfterSeconds ?? _defaultRetrySeconds),
          scope: _asScope(type),
        );

      case ProblemCode.roomEnded:
        return const ConflictFailure(ConflictKind.roomEnded);
      case ProblemCode.roomFull:
        return const ConflictFailure(ConflictKind.roomFull);
      case ProblemCode.duplicateAnswer:
      case ProblemCode.idempotencyInFlight:
        return const ConflictFailure(ConflictKind.duplicate);
      case ProblemCode.questionClosed:
        return const ConflictFailure(ConflictKind.invalidState);
      case ProblemCode.conflict:
        return const ConflictFailure(ConflictKind.staleWrite);

      default:
        // Unknown code. Fall back on the status class so a newly deployed
        // server code degrades to something sensible instead of crashing an
        // app that predates it.
        return _fallback();
    }
  }

  Failure _fallback() {
    if (status == 401) return const AuthFailure(AuthReason.notAuthenticated);
    if (status == 403) return const AuthFailure(AuthReason.forbidden);
    if (status == 409) return const ConflictFailure(ConflictKind.staleWrite);
    if (status == 422) {
      return ValidationFailure({for (final e in errors) e.field: e.code});
    }
    if (status == 429) {
      return RateLimitFailure(
        retryAfter: Duration(seconds: retryAfterSeconds ?? _defaultRetrySeconds),
      );
    }
    return ServerFailure(
      status: status,
      code: code,
      traceId: traceId,
      detail: detail,
    );
  }

  /// Used when the server sends no code at all.
  static String _codeForStatus(int status) => switch (status) {
        400 => ProblemCode.badRequest,
        401 => ProblemCode.unauthorized,
        403 => ProblemCode.forbidden,
        404 => ProblemCode.notFound,
        409 => ProblemCode.conflict,
        422 => ProblemCode.validationFailed,
        429 => ProblemCode.rateLimited,
        503 => ProblemCode.unavailable,
        _ => ProblemCode.internal,
      };

  /// §12.3 wants the message to name which limit was hit. The server encodes
  /// that in the type URI's last segment, e.g. `.../problems/rate-limited-chat`.
  static String? _asScope(String? type) {
    if (type == null) return null;
    final slash = type.lastIndexOf('/');
    if (slash < 0 || slash == type.length - 1) return null;
    final tail = type.substring(slash + 1);
    return tail.isEmpty ? null : tail;
  }

  static List<ProblemFieldError> _parseErrors(Object? raw) {
    if (raw is! List) return const [];
    final out = <ProblemFieldError>[];
    for (final entry in raw) {
      if (entry is! Map) continue;
      final field = _asString(entry['field']);
      if (field == null || field.isEmpty) continue;
      out.add(ProblemFieldError(
        field: field,
        code: _asString(entry['code']) ?? 'INVALID',
        detail: _asString(entry['detail']),
      ));
    }
    return out;
  }

  static String? _asString(Object? v) => v is String ? v : null;

  static int? _asInt(Object? v) => switch (v) {
        int() => v,
        // JSON numbers decode as double when they carry a fraction, and some
        // proxies re-serialise integers that way.
        double() => v.round(),
        String() => int.tryParse(v),
        _ => null,
      };

  /// The fallback backoff when the server gives no hint.
  static const _defaultRetrySeconds = 30;
}

/// One entry of FR-58's `errors[]`.
class ProblemFieldError {
  const ProblemFieldError({
    required this.field,
    required this.code,
    this.detail,
  });

  final String field;
  final String code;

  /// Server-side diagnostic. Never rendered: it is not localised, and FR-61
  /// requires every user-facing string to come from an .arb file.
  final String? detail;
}
