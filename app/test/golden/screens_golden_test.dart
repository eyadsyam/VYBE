/// Golden coverage for the V1 screens (§13.2).
///
/// These are LAYOUT tests, not pixel tests. `matchesGoldenFile` is deliberately
/// not used: golden images are platform-specific — font rasterisation differs
/// between a Windows dev machine and a Linux CI runner — so a pixel comparison
/// here would fail on every push for reasons that have nothing to do with the
/// code. Chasing that produces a suite people learn to ignore.
///
/// What these assert instead is the thing a pixel diff would only tell you
/// about indirectly and unreliably: that nothing overflows, that the direction
/// actually flips, and that the localised strings really are localised. Those
/// hold identically on every platform.
library;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/misc.dart' show Override;
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/app/providers.dart';
import 'package:vybe/core/auth/secure_token_store.dart';
import 'package:vybe/core/error/failure.dart';
import 'package:vybe/core/network/api_client.dart';
import 'package:vybe/core/realtime/room_socket.dart';
import 'package:vybe/core/ui/states/state_views.dart';
import 'package:vybe/features/auth/presentation/sign_in_screen.dart';
import 'package:vybe/features/rooms/domain/room.dart';
import 'package:vybe/features/rooms/presentation/join_room_screen.dart';
import 'package:vybe/features/rooms/presentation/rooms_controller.dart';
import 'package:vybe/features/rooms/presentation/rooms_list_screen.dart';
import 'package:vybe/l10n/generated/app_localizations.dart';

import 'golden_harness.dart';

/// A room with enough content that a cramped layout shows.
Room sampleRoom({
  String id = 'room-1',
  RoomState state = RoomState.lobby,
  String? title = 'ليلة أفلام مع الأصدقاء',
  int participants = 3,
}) {
  final now = DateTime.utc(2026, 8, 26, 12);
  return Room(
    id: id,
    contentId: 'content-1',
    hostUserId: 'user-1',
    state: state,
    syncMode: 'COMPANION',
    visibility: 'private',
    maxParticipants: 4,
    currentSeq: 12,
    createdAt: now,
    serverTime: now,
    joinCode: 'K7X2QP',
    shareUrl: 'https://vybe.app/r/K7X2QP',
    title: title,
    participants: [
      for (var i = 0; i < participants; i++)
        Participant(
          userId: 'user-${i + 1}',
          isHost: i == 0,
          connected: i != 2,
          joinedAt: now,
        ),
    ],
  );
}

List<Override> baseOverrides({RoomsListState? rooms}) => [
      apiEndpointProvider.overrideWithValue(ApiEndpoint.localhost),
      secretStoreProvider.overrideWithValue(MemorySecretStore()),
      if (rooms != null)
        roomsListProvider.overrideWith(() => _StubRoomsList(rooms)),
    ];

class _StubRoomsList extends RoomsListController {
  _StubRoomsList(this._value);
  final RoomsListState _value;

  @override
  Future<RoomsListState> build() async => _value;
}

void main() {
  group('sign-in screen', () {
    for (final variant in goldenVariants) {
      testWidgets('lays out at $variant', (tester) async {
        await withSurface(tester, goldenTallSurface, () async {
          await tester.pumpWidget(
            goldenApp(
              variant: variant,
              overrides: baseOverrides(),
              child: const SignInScreen(),
            ),
          );
          await tester.pumpAndSettle();

          expectNoOverflow(tester, variant);
          expect(find.byType(TextField), findsAtLeast(2));

          // The direction must actually flip. A layout using left/right
          // instead of start/end renders identically in both and this is the
          // only thing that catches it.
          final direction = Directionality.of(
            tester.element(find.byType(SignInScreen)),
          );
          expect(
            direction,
            variant.locale.languageCode == 'ar'
                ? TextDirection.rtl
                : TextDirection.ltr,
          );
        });
      });
    }
  });

  group('rooms list — empty state', () {
    for (final variant in goldenVariants) {
      testWidgets('lays out at $variant', (tester) async {
        await withSurface(tester, goldenTallSurface, () async {
          await tester.pumpWidget(
            goldenApp(
              variant: variant,
              overrides: baseOverrides(rooms: const RoomsListState()),
              child: const RoomsListScreen(),
            ),
          );
          await tester.pumpAndSettle();

          expectNoOverflow(tester, variant);
          expect(find.byType(EmptyStateView), findsOneWidget);

          // The Arabic copy must actually render — an English fallback here
          // would mean the .arb lookup silently missed.
          final l10n = L10n.of(tester.element(find.byType(EmptyStateView)));
          expect(find.text(l10n.roomsEmptyTitle), findsOneWidget);
          if (variant.locale.languageCode == 'ar') {
            expect(
              l10n.roomsEmptyTitle,
              isNot('No rooms yet'),
              reason: 'Arabic fell back to English copy',
            );
          }
        });
      });
    }
  });

  group('rooms list — populated', () {
    for (final variant in goldenVariants) {
      testWidgets('lays out at $variant', (tester) async {
        final state = RoomsListState(
          rooms: [
            sampleRoom(),
            sampleRoom(id: 'room-2', state: RoomState.playing, title: null),
            sampleRoom(
              id: 'room-3',
              state: RoomState.ready,
              title: 'A deliberately long room name that will wrap',
            ),
          ],
        );

        await withSurface(tester, goldenTallSurface, () async {
          await tester.pumpWidget(
            goldenApp(
              variant: variant,
              overrides: baseOverrides(rooms: state),
              child: const RoomsListScreen(),
            ),
          );
          await tester.pumpAndSettle();

          expectNoOverflow(tester, variant);
          expect(find.byType(Card), findsNWidgets(3));
        });
      });
    }
  });

  group('join screen', () {
    for (final variant in goldenVariants) {
      testWidgets('lays out at $variant', (tester) async {
        await withSurface(tester, goldenTallSurface, () async {
          await tester.pumpWidget(
            goldenApp(
              variant: variant,
              overrides: baseOverrides(),
              child: const JoinRoomScreen(initialCode: 'K7X2QP'),
            ),
          );
          await tester.pumpAndSettle();

          expectNoOverflow(tester, variant);

          // The code field is LTR even in an Arabic UI. A join code rendered
          // RTL is a code somebody then reads aloud backwards.
          final field = tester.widget<TextField>(find.byType(TextField));
          expect(
            field.textDirection,
            TextDirection.ltr,
            reason: 'the join code must render LTR in every locale',
          );
        });
      });
    }
  });

  group('state views', () {
    // Every §3.2 state, in every variant. These are the screens a user sees on
    // a bad day, which makes them the ones most likely to be tested by hand
    // once and never again.
    final states = <String, Widget Function(L10n)>{
      'loading': (_) => const LoadingStateView(),
      'offline': (_) => const OfflineStateView(),
      'unauthorised': (_) => const UnauthorisedStateView(),
      'notFound': (_) => const NotFoundStateView(),
      'rateLimited': (_) =>
          const RateLimitedStateView(retryAfter: Duration(seconds: 42)),
      'error': (_) => const ErrorStateView(
            failure: ServerFailure(status: 500, code: 'INTERNAL'),
          ),
      'empty': (l10n) => EmptyStateView(
            title: l10n.stateEmptyTitle,
            body: l10n.stateEmptyBody,
          ),
    };

    for (final variant in goldenVariants) {
      for (final entry in states.entries) {
        testWidgets('${entry.key} at $variant', (tester) async {
          await withSurface(tester, goldenTallSurface, () async {
            await tester.pumpWidget(
              goldenApp(
                variant: variant,
                overrides: baseOverrides(),
                child: Scaffold(
                  body: Builder(
                    builder: (context) => entry.value(L10n.of(context)),
                  ),
                ),
              ),
            );
            await tester.pump();

            expectNoOverflow(tester, variant);
          });
        });
      }
    }
  });

  group('the matrix itself', () {
    test('covers every axis', () {
      // The matrix is the contract. A variant quietly dropped would silently
      // stop testing a whole class of bug, so its shape is asserted.
      expect(goldenVariants, hasLength(8));
      expect(
        goldenVariants.map((v) => v.locale.languageCode).toSet(),
        {'en', 'ar'},
      );
      expect(
        goldenVariants.map((v) => v.brightness).toSet(),
        {Brightness.light, Brightness.dark},
      );
      expect(goldenVariants.map((v) => v.textScale).toSet(), {1.0, 2.0});

      // Names must be unique, or two variants would write the same golden.
      expect(
        goldenVariants.map((v) => v.name).toSet(),
        hasLength(goldenVariants.length),
      );
    });

    test('uses a small surface on purpose', () {
      // A layout tuned on a large device overflows on the cheap phone most of
      // the audience actually owns.
      expect(goldenSurface.width, lessThanOrEqualTo(400));
    });
  });

  group('socket status rendering', () {
    // The connection indicator is the one thing on the room screen that must
    // never be silently wrong: a stale participant list looks exactly like an
    // accurate one.
    test('every SocketStatus has a rendering', () {
      // A `switch` over SocketStatus in the widget is exhaustive at compile
      // time, so this asserts the enum has not grown a member that some other
      // non-exhaustive switch would silently drop.
      expect(SocketStatus.values, hasLength(5));
    });
  });
}
