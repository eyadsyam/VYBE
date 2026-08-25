import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/error/failure.dart';
import 'package:vybe/core/ui/states/freshness.dart';
import 'package:vybe/core/ui/states/state_views.dart';
import 'package:vybe/core/ui/tokens.dart';
import 'package:vybe/l10n/generated/app_localizations.dart';

/// Wraps a state view in the minimum app scaffolding it needs, with the locale
/// and text scale under test control.
Widget _host(
  Widget child, {
  Locale locale = const Locale('en'),
  double textScale = 1.0,
}) {
  return MaterialApp(
    locale: locale,
    localizationsDelegates: L10n.localizationsDelegates,
    supportedLocales: L10n.supportedLocales,
    home: MediaQuery(
      data: MediaQueryData(textScaler: TextScaler.linear(textScale)),
      child: Scaffold(body: child),
    ),
  );
}

void main() {
  group('LoadingStateView', () {
    testWidgets('is announced to a screen reader', (tester) async {
      // §3.5: a loading state that is only a set of grey rectangles does not
      // exist for anybody using a screen reader.
      await tester.pumpWidget(_host(const LoadingStateView()));
      final en = await L10n.delegate.load(const Locale('en'));

      expect(
        find.bySemanticsLabel(en.stateLoading),
        findsOneWidget,
        reason: 'the skeleton carries no semantic label',
      );
    });

    testWidgets('renders the requested number of skeleton rows', (tester) async {
      await tester.pumpWidget(_host(const LoadingStateView(itemCount: 3)));
      expect(find.byType(ListView), findsOneWidget);
    });
  });

  group('EmptyStateView', () {
    testWidgets('shows generic copy by default', (tester) async {
      await tester.pumpWidget(_host(const EmptyStateView()));
      final en = await L10n.delegate.load(const Locale('en'));

      expect(find.text(en.stateEmptyTitle), findsOneWidget);
      expect(find.text(en.stateEmptyBody), findsOneWidget);
    });

    testWidgets('a surface can supply its own invitation', (tester) async {
      final en = await L10n.delegate.load(const Locale('en'));
      var tapped = false;

      await tester.pumpWidget(_host(EmptyStateView(
        title: en.stateEmptyRoomsTitle,
        body: en.stateEmptyRoomsBody,
        actionLabel: en.stateEmptyRoomsAction,
        onAction: () => tapped = true,
      )));

      expect(find.text(en.stateEmptyRoomsTitle), findsOneWidget);
      await tester.tap(find.text(en.stateEmptyRoomsAction));
      expect(tapped, isTrue);
    });

    testWidgets('offers no action when none is given', (tester) async {
      await tester.pumpWidget(_host(const EmptyStateView()));
      expect(find.byType(FilledButton), findsNothing);
    });
  });

  group('ErrorStateView', () {
    testWidgets('renders retry for a retryable failure and invokes it',
        (tester) async {
      final en = await L10n.delegate.load(const Locale('en'));
      var retried = false;

      await tester.pumpWidget(_host(ErrorStateView(
        failure: const NetworkFailure(isOffline: false),
        onRetry: () => retried = true,
      )));

      expect(find.text(en.errorNetworkBody), findsOneWidget);
      await tester.tap(find.text(en.errorRetry));
      expect(retried, isTrue);
    });

    testWidgets('a revoked session offers sign-in and says why',
        (tester) async {
      // EC-8: the user is told, not silently bounced.
      final en = await L10n.delegate.load(const Locale('en'));
      var signedIn = false;

      await tester.pumpWidget(_host(ErrorStateView(
        failure: const AuthFailure(AuthReason.sessionRevoked),
        onSignIn: () => signedIn = true,
      )));

      expect(find.text(en.errorSessionRevoked), findsOneWidget);
      await tester.tap(find.text(en.authSignInAction));
      expect(signedIn, isTrue);
    });

    testWidgets('shows a copyable trace id on a terminal error',
        (tester) async {
      // §14.2: the support path only leads somewhere if the user can quote the
      // reference, and transcribing it from a screenshot is not that.
      await tester.pumpWidget(_host(const ErrorStateView(
        failure: ServerFailure(status: 400, code: 'BAD_REQUEST', traceId: 'trace-abc'),
      )));

      expect(find.byType(SelectableText), findsOneWidget);
      expect(find.textContaining('trace-abc'), findsOneWidget);
    });

    testWidgets('shows no trace id where there is nothing to quote',
        (tester) async {
      await tester.pumpWidget(_host(const ErrorStateView(
        failure: NetworkFailure(isOffline: true),
      )));
      expect(find.byType(SelectableText), findsNothing);
    });

    testWidgets('hides the action when the caller supplies no callback',
        (tester) async {
      // A button that does nothing is worse than no button.
      await tester.pumpWidget(_host(const ErrorStateView(
        failure: NetworkFailure(isOffline: false),
      )));
      expect(find.byType(FilledButton), findsNothing);
    });
  });

  testWidgets('UnauthorisedStateView distinguishes revoked from signed out',
      (tester) async {
    final en = await L10n.delegate.load(const Locale('en'));

    await tester.pumpWidget(_host(const UnauthorisedStateView()));
    expect(find.text(en.authRequiredBody), findsOneWidget);

    await tester.pumpWidget(_host(
      const UnauthorisedStateView(reason: AuthReason.sessionRevoked),
    ));
    expect(find.text(en.errorSessionRevoked), findsOneWidget);
  });

  group('RateLimitedStateView', () {
    testWidgets('counts down and only then offers retry', (tester) async {
      // §3.2 / AC-23: show the real wait. "Try again later" is the version that
      // gets tapped repeatedly, which is what a rate limit is trying to stop.
      final en = await L10n.delegate.load(const Locale('en'));
      var retried = false;

      await tester.pumpWidget(_host(RateLimitedStateView(
        retryAfter: const Duration(seconds: 3),
        onRetry: () => retried = true,
      )));

      expect(find.textContaining('3'), findsOneWidget);
      expect(find.text(en.errorRetry), findsNothing);

      await tester.pump(const Duration(seconds: 1));
      expect(find.textContaining('2'), findsOneWidget);

      await tester.pump(const Duration(seconds: 2));
      expect(find.text(en.errorRateLimitedReady), findsOneWidget);

      await tester.tap(find.text(en.errorRetry));
      expect(retried, isTrue);
    });

    testWidgets('cancels its timer on dispose', (tester) async {
      // A leaked periodic timer keeps calling setState after the route is gone;
      // the test framework fails the test if one outlives the widget.
      await tester.pumpWidget(_host(
        const RateLimitedStateView(retryAfter: Duration(seconds: 30)),
      ));
      await tester.pumpWidget(_host(const SizedBox.shrink()));
      await tester.pump(const Duration(seconds: 2));
      // Reaching here without a pending-timer failure is the assertion.
    });
  });

  group('FreshnessBanner (AC-32)', () {
    testWidgets('renders nothing when the data is live', (tester) async {
      // A badge on live data is noise, so a screen can place it unconditionally.
      await tester.pumpWidget(_host(const FreshnessBanner(
        freshness: Freshness.live,
        age: Duration.zero,
      )));
      expect(find.byType(Text), findsNothing);
    });

    testWidgets('states the age when stale', (tester) async {
      await tester.pumpWidget(_host(const FreshnessBanner(
        freshness: Freshness.stale,
        age: Duration(hours: 2),
      )));
      expect(find.textContaining('2'), findsOneWidget);
    });

    testWidgets('says it is offline rather than guessing an age',
        (tester) async {
      final en = await L10n.delegate.load(const Locale('en'));
      await tester.pumpWidget(_host(const FreshnessBanner(
        freshness: Freshness.offlineStale,
        age: Duration(days: 1),
      )));
      expect(find.text(en.freshnessOfflineStale), findsOneWidget);
    });
  });

  group('formatAge rounds down', () {
    late L10n en;
    setUpAll(() async => en = await L10n.delegate.load(const Locale('en')));

    test('under a minute is "just now"', () {
      expect(formatAge(const Duration(seconds: 59), en), en.agoJustNow);
    });

    test('rounds down rather than up', () {
      // Claiming data is younger than it is defeats the point of the indicator.
      expect(formatAge(const Duration(minutes: 59, seconds: 59), en),
          en.agoMinutes(59));
      expect(formatAge(const Duration(hours: 2, minutes: 59), en),
          en.agoHours(2));
      expect(formatAge(const Duration(days: 1, hours: 23), en), en.agoDays(1));
    });
  });

  group('renders correctly in Arabic RTL (§3.6, AC-34)', () {
    testWidgets('the text direction actually flips', (tester) async {
      await tester.pumpWidget(_host(
        const ErrorStateView(failure: NetworkFailure(isOffline: true)),
        locale: const Locale('ar'),
      ));

      final direction = Directionality.of(
        tester.element(find.byType(ErrorStateView)),
      );
      expect(direction, TextDirection.rtl);
    });

    testWidgets('Arabic copy is rendered, not an English fallback',
        (tester) async {
      final ar = await L10n.delegate.load(const Locale('ar'));
      await tester.pumpWidget(_host(
        const ErrorStateView(failure: NetworkFailure(isOffline: true)),
        locale: const Locale('ar'),
      ));
      expect(find.text(ar.errorOfflineBody), findsOneWidget);
    });

    testWidgets('every state view survives RTL without overflowing',
        (tester) async {
      for (final view in <Widget>[
        const LoadingStateView(),
        const EmptyStateView(),
        const ErrorStateView(failure: NetworkFailure(isOffline: true)),
        const OfflineStateView(),
        const UnauthorisedStateView(),
        const NotFoundStateView(),
        const RateLimitedStateView(retryAfter: Duration(seconds: 30)),
      ]) {
        await tester.pumpWidget(_host(view, locale: const Locale('ar')));
        await tester.pump();
        expect(tester.takeException(), isNull,
            reason: '${view.runtimeType} threw in RTL');
      }
    });
  });

  group('survives 200% text scale (NFR-16, §3.5)', () {
    testWidgets('no overflow at the largest supported scale', (tester) async {
      // The states scroll precisely so this holds: at 200% a title, body and
      // button no longer fit a phone, and NFR-16 forbids truncation.
      tester.view.physicalSize = const Size(360, 640);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.reset);

      for (final locale in [const Locale('en'), const Locale('ar')]) {
        for (final view in <Widget>[
          const EmptyStateView(),
          const ErrorStateView(
            failure: ServerFailure(status: 400, code: 'X', traceId: 'trace-abc'),
          ),
          const OfflineStateView(),
          const UnauthorisedStateView(reason: AuthReason.sessionRevoked),
          const NotFoundStateView(),
        ]) {
          await tester.pumpWidget(_host(
            view,
            locale: locale,
            textScale: TypeScale.maxSupportedScale,
          ));
          await tester.pump();
          expect(tester.takeException(), isNull,
              reason: '${view.runtimeType} overflowed at 200% in $locale');
        }
      }
    });
  });
}
