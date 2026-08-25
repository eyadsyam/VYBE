import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/error/failure.dart';
import 'package:vybe/core/network/problem_details.dart';

void main() {
  group('ProblemDetails.parse', () {
    test('reads every FR-58 member', () {
      final p = ProblemDetails.parse({
        'type': 'https://vybe.app/problems/room-full',
        'title': 'Room Full',
        'status': 409,
        'detail': 'This room has 8 of 8 participants.',
        'code': 'ROOM_FULL',
        'traceId': 'trace-abc',
      }, 409);

      expect(p.code, 'ROOM_FULL');
      expect(p.status, 409);
      expect(p.title, 'Room Full');
      expect(p.traceId, 'trace-abc');
      expect(p.detail, 'This room has 8 of 8 participants.');
    });

    test('the transport status wins over a status claimed in the body', () {
      // The status that actually happened is the one the transport reports. A
      // body claiming otherwise is a bug or a hostile response.
      final p = ProblemDetails.parse({'code': 'X', 'status': 200}, 500);
      expect(p.status, 500);
    });

    test('survives a body that is not a problem document at all', () {
      // A proxy's HTML 502, a captive portal's login page, an empty body. Each
      // must become a Failure rather than a TypeError from a cast.
      for (final body in <Object?>[
        null,
        '',
        '<html><body>502 Bad Gateway</body></html>',
        <String>['unexpected', 'list'],
        42,
      ]) {
        final p = ProblemDetails.parse(body, 502);
        expect(p.status, 502);
        expect(p.code, ProblemCode.internal);
        expect(p.toFailure(), isA<ServerFailure>());
      }
    });

    test('derives a code from the status when the body omits one', () {
      final cases = {
        400: ProblemCode.badRequest,
        401: ProblemCode.unauthorized,
        403: ProblemCode.forbidden,
        404: ProblemCode.notFound,
        409: ProblemCode.conflict,
        422: ProblemCode.validationFailed,
        429: ProblemCode.rateLimited,
        503: ProblemCode.unavailable,
        418: ProblemCode.internal,
      };
      cases.forEach((status, code) {
        expect(ProblemDetails.parse(<String, Object?>{}, status).code, code,
            reason: 'status $status');
      });
    });

    test('an empty-string code is treated as absent', () {
      expect(ProblemDetails.parse({'code': ''}, 404).code, ProblemCode.notFound);
    });

    test('non-string fields are ignored rather than crashing', () {
      final p = ProblemDetails.parse({
        'code': 123,
        'title': <String, Object>{},
        'traceId': true,
      }, 500);
      expect(p.code, ProblemCode.internal);
      expect(p.title, isNull);
      expect(p.traceId, isNull);
    });

    test('parses errors[] and skips malformed entries', () {
      final p = ProblemDetails.parse({
        'code': 'VALIDATION_FAILED',
        'errors': [
          {'field': 'contentId', 'code': 'REQUIRED', 'detail': 'must be present'},
          {'field': 'syncMode', 'code': 'ENUM'},
          {'code': 'NO_FIELD_NAME'},
          {'field': ''},
          'not an object',
        ],
      }, 422);

      expect(p.errors, hasLength(2));
      expect(p.errors[0].field, 'contentId');
      expect(p.errors[0].code, 'REQUIRED');
      // A missing code still yields a usable default rather than dropping the
      // field, because the user still needs to know which input is wrong.
      expect(p.errors[1].code, 'ENUM');
    });

    test('retryAfterSeconds accepts the shapes proxies actually produce', () {
      expect(ProblemDetails.parse({'retryAfterSeconds': 30}, 429).retryAfterSeconds, 30);
      expect(ProblemDetails.parse({'retryAfterSeconds': 30.0}, 429).retryAfterSeconds, 30);
      expect(ProblemDetails.parse({'retryAfterSeconds': '30'}, 429).retryAfterSeconds, 30);
      expect(ProblemDetails.parse({'retryAfterSeconds': 'soon'}, 429).retryAfterSeconds, isNull);
      expect(ProblemDetails.parse(<String, Object?>{}, 429).retryAfterSeconds, isNull);
    });
  });

  group('toFailure maps on code, not status', () {
    test('the five 409s are distinguishable', () {
      // This is the whole reason the client branches on `code`. §3.2 requires
      // room-ended to get a dedicated screen, which is impossible if every 409
      // collapses to one failure.
      Failure at(String code) =>
          ProblemDetails.parse({'code': code}, 409).toFailure();

      expect(at('ROOM_ENDED'), const ConflictFailure(ConflictKind.roomEnded));
      expect(at('ROOM_FULL'), const ConflictFailure(ConflictKind.roomFull));
      expect(at('DUPLICATE_ANSWER'), const ConflictFailure(ConflictKind.duplicate));
      expect(at('IDEMPOTENCY_IN_FLIGHT'), const ConflictFailure(ConflictKind.duplicate));
      expect(at('CONFLICT'), const ConflictFailure(ConflictKind.staleWrite));
    });

    test('auth codes map to the reason that changes what the app does', () {
      Failure at(String code, int status) =>
          ProblemDetails.parse({'code': code}, status).toFailure();

      expect(at('TOKEN_EXPIRED', 401), const AuthFailure(AuthReason.tokenExpired));
      expect(at('SESSION_REVOKED', 401), const AuthFailure(AuthReason.sessionRevoked));
      expect(at('INVALID_CREDENTIALS', 401), const AuthFailure(AuthReason.invalidCredentials));
      expect(at('UNAUTHORIZED', 401), const AuthFailure(AuthReason.notAuthenticated));
      expect(at('FORBIDDEN', 403), const AuthFailure(AuthReason.forbidden));
      expect(at('NOT_PARTICIPANT', 403), const AuthFailure(AuthReason.forbidden));
      expect(at('INVALID_NONCE', 403), const AuthFailure(AuthReason.forbidden));
    });

    test('validation carries field to code, never a message', () {
      final f = ProblemDetails.parse({
        'code': 'VALIDATION_FAILED',
        'errors': [
          {'field': 'email', 'code': 'FORMAT', 'detail': 'not an email'},
          {'field': 'password', 'code': 'BREACHED', 'detail': 'found in a breach corpus'},
        ],
      }, 422).toFailure();

      expect(f, isA<ValidationFailure>());
      final v = f as ValidationFailure;
      expect(v.fields, {'email': 'FORMAT', 'password': 'BREACHED'});
      // The server's English `detail` must not leak into the domain layer:
      // FR-61 requires the form to render a localised string per field.
      expect(v.fields.values, isNot(contains('not an email')));
    });

    test('rate limit uses the server hint when present', () {
      final f = ProblemDetails.parse({
        'code': 'RATE_LIMITED',
        'retryAfterSeconds': 45,
      }, 429).toFailure();

      expect(f, isA<RateLimitFailure>());
      expect((f as RateLimitFailure).retryAfter, const Duration(seconds: 45));
    });

    test('rate limit falls back to a real backoff when no hint is sent', () {
      // Not hypothetical: the TMDB probe found a live provider returning no
      // rate-limit headers at all (docs/INTEGRATIONS.md). A missing hint must
      // not become "retry immediately".
      final f = ProblemDetails.parse({'code': 'RATE_LIMITED'}, 429).toFailure();

      expect(f, isA<RateLimitFailure>());
      expect((f as RateLimitFailure).retryAfter.inSeconds, greaterThan(0));
    });

    test('rate limit scope comes from the type URI, for a specific message', () {
      final f = ProblemDetails.parse({
        'code': 'RATE_LIMITED',
        'type': 'https://vybe.app/problems/rate-limited-chat',
      }, 429).toFailure() as RateLimitFailure;

      expect(f.scope, 'rate-limited-chat');
    });

    test('a malformed type URI yields a null scope rather than throwing', () {
      for (final type in ['', 'no-slashes', 'https://vybe.app/']) {
        final f = ProblemDetails.parse({
          'code': 'RATE_LIMITED',
          'type': type,
        }, 429).toFailure() as RateLimitFailure;
        expect(f.scope, isNull, reason: 'type "$type"');
      }
    });

    test('an unknown code degrades by status instead of crashing', () {
      // A server deploy can add a code before the app understanding it ships.
      // That must degrade, never throw.
      expect(
        ProblemDetails.parse({'code': 'BRAND_NEW_CODE'}, 401).toFailure(),
        const AuthFailure(AuthReason.notAuthenticated),
      );
      expect(
        ProblemDetails.parse({'code': 'BRAND_NEW_CODE'}, 403).toFailure(),
        const AuthFailure(AuthReason.forbidden),
      );
      expect(
        ProblemDetails.parse({'code': 'BRAND_NEW_CODE'}, 409).toFailure(),
        const ConflictFailure(ConflictKind.staleWrite),
      );
      expect(
        ProblemDetails.parse({'code': 'BRAND_NEW_CODE'}, 422).toFailure(),
        isA<ValidationFailure>(),
      );
      expect(
        ProblemDetails.parse({'code': 'BRAND_NEW_CODE'}, 429).toFailure(),
        isA<RateLimitFailure>(),
      );
      expect(
        ProblemDetails.parse({'code': 'BRAND_NEW_CODE'}, 500).toFailure(),
        isA<ServerFailure>(),
      );
    });

    test('a server failure keeps the trace id so support can find it', () {
      final f = ProblemDetails.parse({
        'code': 'INTERNAL',
        'traceId': 'trace-xyz',
        'detail': 'nil pointer dereference in roomsvc.go:214',
      }, 500).toFailure() as ServerFailure;

      expect(f.traceId, 'trace-xyz');
      expect(f.status, 500);
      // Retained for logs. FailurePresenter never renders it.
      expect(f.detail, isNotNull);
    });
  });
}
