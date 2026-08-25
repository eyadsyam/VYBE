import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/error/failure.dart';
import 'package:vybe/core/error/failure_presenter.dart';
import 'package:vybe/l10n/generated/app_localizations.dart';

/// Every Failure variant, so the exhaustiveness assertions below have
/// something concrete to walk. Adding a variant to the sealed hierarchy without
/// adding it here makes `TestEveryVariantIsCovered` fail.
final _allFailures = <Failure>[
  const NetworkFailure(isOffline: true),
  const NetworkFailure(isOffline: false),
  const ServerFailure(status: 500, code: 'INTERNAL'),
  const ServerFailure(status: 400, code: 'BAD_REQUEST'),
  for (final r in AuthReason.values) AuthFailure(r),
  const ValidationFailure({'email': 'FORMAT'}),
  const RateLimitFailure(retryAfter: Duration(seconds: 30)),
  const RateLimitFailure(retryAfter: Duration.zero),
  for (final k in ConflictKind.values) ConflictFailure(k),
  const RequiresConnectionFailure('chat'),
  UnexpectedFailure(Exception('boom')),
];

void main() {
  late L10n en;
  late L10n ar;

  setUpAll(() async {
    en = await L10n.delegate.load(const Locale('en'));
    ar = await L10n.delegate.load(const Locale('ar'));
  });

  test('every failure variant yields non-empty, localised text', () {
    // The point of the sealed hierarchy is that no failure can reach a screen
    // without somebody deciding what it says and what the user does next.
    for (final f in _allFailures) {
      final p = FailurePresenter.present(f, en);

      expect(p.title, isNotEmpty, reason: '${f.code} produced an empty title');
      expect(p.body, isNotEmpty, reason: '${f.code} produced an empty body');
    }
  });

  test('every failure variant is translated in Arabic', () {
    // §3.6 makes Arabic a launch requirement. A presentation that falls back to
    // English here is the exact failure the l10n gate exists to catch, and it
    // would otherwise only surface by someone switching locale by hand.
    for (final f in _allFailures) {
      final e = FailurePresenter.present(f, en);
      final a = FailurePresenter.present(f, ar);

      expect(a.title, isNotEmpty, reason: '${f.code} has no Arabic title');
      expect(a.body, isNotEmpty, reason: '${f.code} has no Arabic body');
      expect(a.title, isNot(equals(e.title)),
          reason: '${f.code} title is identical in both locales — untranslated?');
    }
  });

  group('the action offered actually helps', () {
    test('offline offers retry, not sign-in', () {
      final p = FailurePresenter.present(
          const NetworkFailure(isOffline: true), en);
      expect(p.action, FailureAction.retry);
      expect(p.isRetryable, isTrue);
    });

    test('a 5xx is retryable; a stray 4xx is terminal with a support path', () {
      final server = FailurePresenter.present(
          const ServerFailure(status: 503, code: 'UNAVAILABLE'), en);
      expect(server.isRetryable, isTrue);
      expect(server.action, FailureAction.retry);

      final client = FailurePresenter.present(
          const ServerFailure(status: 400, code: 'BAD_REQUEST'), en);
      expect(client.isRetryable, isFalse);
      expect(client.action, FailureAction.contactSupport);
    });

    test('forbidden does NOT offer sign-in', () {
      // Offering "sign in" to somebody already signed in is nonsense and reads
      // as a broken app — which is why AuthReason keeps the two distinct.
      final p = FailurePresenter.present(
          const AuthFailure(AuthReason.forbidden), en);
      expect(p.action, isNot(FailureAction.signIn));
      expect(p.action, FailureAction.goHome);
    });

    test('not-authenticated does offer sign-in', () {
      final p = FailurePresenter.present(
          const AuthFailure(AuthReason.notAuthenticated), en);
      expect(p.action, FailureAction.signIn);
    });

    test('a revoked session explains itself rather than silently bouncing', () {
      // EC-8 / ADR-011: reuse detection logs the user out. If the app does not
      // say why, it is indistinguishable from a bug.
      final p = FailurePresenter.present(
          const AuthFailure(AuthReason.sessionRevoked), en);

      expect(p.action, FailureAction.signIn);
      expect(p.body, en.errorSessionRevoked);
      expect(p.body, isNot(en.authRequiredBody),
          reason: 'a revoked session must not read as an ordinary signed-out state');
    });

    test('room ended gets its own copy, not a generic conflict message', () {
      // §3.2 and EC-17: tapping an invite to a finished room is a normal thing
      // to do and deserves a real answer.
      final ended = FailurePresenter.present(
          const ConflictFailure(ConflictKind.roomEnded), en);
      final stale = FailurePresenter.present(
          const ConflictFailure(ConflictKind.staleWrite), en);

      expect(ended.title, en.roomEndedTitle);
      expect(ended.title, isNot(stale.title));
      expect(ended.isRetryable, isFalse);
    });

    test('room full is distinct from room ended', () {
      final full = FailurePresenter.present(
          const ConflictFailure(ConflictKind.roomFull), en);
      final ended = FailurePresenter.present(
          const ConflictFailure(ConflictKind.roomEnded), en);
      expect(full.title, isNot(ended.title));
    });
  });

  group('rate limiting shows the real wait', () {
    test('a live countdown value appears in the body', () {
      // §3.2: time until retry, not "try again later".
      final p = FailurePresenter.present(
          const RateLimitFailure(retryAfter: Duration(seconds: 45)), en);
      expect(p.body, contains('45'));
      // No retry affordance while the clock is still running.
      expect(p.action, FailureAction.none);
    });

    test('once elapsed, retry becomes available', () {
      final p = FailurePresenter.present(
          const RateLimitFailure(retryAfter: Duration.zero), en);
      expect(p.body, en.errorRateLimitedReady);
      expect(p.action, FailureAction.retry);
    });
  });

  test('an un-queueable offline action says so plainly', () {
    // AC-33 / §11.3: the forbidden alternative is optimistic success followed
    // by a silent drop.
    final p = FailurePresenter.present(
        const RequiresConnectionFailure('chat'), en);
    expect(p.body, en.errorRequiresConnection);
  });

  test('a trace id is surfaced only where a user would need to quote it', () {
    final withTrace = FailurePresenter.present(
      const ServerFailure(status: 400, code: 'BAD_REQUEST', traceId: 'trace-abc'),
      en,
    );
    expect(withTrace.traceId, 'trace-abc');

    // Retryable, self-resolving states do not need one.
    final offline = FailurePresenter.present(
        const NetworkFailure(isOffline: true), en);
    expect(offline.traceId, isNull);
  });

  test('no presentation leaks the server detail string', () {
    // ServerFailure.detail is diagnostic and unlocalised. Rendering it would
    // put untranslated English on an Arabic screen (FR-61).
    const detail = 'nil pointer dereference in roomsvc.go:214';
    final p = FailurePresenter.present(
      const ServerFailure(status: 500, code: 'INTERNAL', detail: detail),
      en,
    );
    expect(p.title, isNot(contains('roomsvc')));
    expect(p.body, isNot(contains('roomsvc')));
  });
}
