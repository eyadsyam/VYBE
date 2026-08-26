/// Navigation and deep links (FR-13, §4.5).
///
/// go_router rather than imperative navigation, because a share link has to
/// resolve to a screen from a cold start — and `Navigator.push` from an app
/// that has not finished deciding whether the user is signed in is how deep
/// links end up landing on the wrong screen or nowhere at all.
library;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../core/ui/states/state_views.dart';
import '../features/auth/presentation/sign_in_screen.dart';
import '../features/rooms/domain/room.dart';
import '../features/rooms/presentation/join_room_screen.dart';
import '../features/rooms/presentation/room_screen.dart';
import '../features/rooms/presentation/rooms_list_screen.dart';
import 'providers.dart';

/// Route names, referenced rather than spelled at call sites.
///
/// A typo in a path literal is a runtime 404 that only fires on the one screen
/// nobody opened during testing; a typo in a constant does not compile.
abstract final class Routes {
  static const rooms = '/rooms';
  static const room = '/rooms/:roomId';
  static const join = '/join';
  static const signIn = '/sign-in';

  /// The path for a specific room.
  static String roomPath(String roomId) => '/rooms/$roomId';

  /// The path a share link resolves to.
  ///
  /// The universal link is `https://vybe.app/r/CODE`, and the app maps it here
  /// rather than to `/rooms/:id`, because a CODE is not an ID: it must be
  /// exchanged for membership first, and treating them as interchangeable is
  /// how somebody who has never joined lands on a room screen that 404s.
  static String joinPath(String code) => '/join?code=$code';
}

/// Builds the router.
///
/// [refreshListenable] is what makes an auth change re-run the redirect — a
/// router that only evaluated redirects on navigation would leave a user
/// looking at a room screen after their session was revoked.
GoRouter createRouter(Ref ref) {
  return GoRouter(
    initialLocation: Routes.rooms,
    refreshListenable: _SessionRefresh(ref),
    redirect: (context, state) {
      final session = ref.read(sessionProvider);

      // While the keystore read is in flight, do not redirect at all.
      // Redirecting to sign-in here would flash it for one frame to somebody
      // who IS signed in, and a deep link would be lost in the process.
      if (session.isLoading) return null;

      final signedIn = session.value != null;
      final atSignIn = state.matchedLocation == Routes.signIn;

      if (!signedIn && !atSignIn) {
        // Carry the destination through, so a share link opened by a
        // signed-out user still lands where it was pointing once they sign in.
        // Dropping it is the difference between "sign in and you are there"
        // and "sign in and find it yourself".
        final target = state.uri.toString();
        return '${Routes.signIn}?next=${Uri.encodeComponent(target)}';
      }

      if (signedIn && atSignIn) {
        final next = state.uri.queryParameters['next'];
        return (next == null || next.isEmpty) ? Routes.rooms : Uri.decodeComponent(next);
      }

      return null;
    },
    routes: [
      GoRoute(
        path: Routes.signIn,
        builder: (context, state) => SignInScreen(
          next: state.uri.queryParameters['next'],
        ),
      ),
      GoRoute(
        path: Routes.rooms,
        builder: (context, state) => const RoomsListScreen(),
        routes: [
          GoRoute(
            path: ':roomId',
            builder: (context, state) => RoomScreen(
              roomId: state.pathParameters['roomId'] ?? '',
            ),
          ),
        ],
      ),
      GoRoute(
        path: Routes.join,
        builder: (context, state) {
          // A code arriving from a share link is normalised through the same
          // parser the join field uses. A link with a lowercase or hyphenated
          // code is common — people copy them out of chat messages — and
          // rejecting it because it is not canonical would be absurd.
          final raw = state.uri.queryParameters['code'] ?? '';
          return JoinRoomScreen(initialCode: JoinCode.parse(raw) ?? raw);
        },
      ),
    ],
    errorBuilder: (context, state) => _RouteNotFoundScreen(location: state.uri.toString()),
  );
}

/// Notifies the router when the session changes.
///
/// A [ChangeNotifier] bridging Riverpod to go_router, which predates it and
/// speaks Listenable. Without this, signing out would leave the user on
/// whatever screen they were on until they navigated.
class _SessionRefresh extends ChangeNotifier {
  _SessionRefresh(Ref ref) {
    _subscription = ref.listen(
      sessionProvider,
      (_, _) => notifyListeners(),
      fireImmediately: false,
    );
  }

  late final ProviderSubscription<dynamic> _subscription;

  @override
  void dispose() {
    _subscription.close();
    super.dispose();
  }
}

/// The router-level 404.
///
/// A real screen rather than go_router's default red text, because a bad share
/// link is a thing users see and the default is a stack trace.
class _RouteNotFoundScreen extends StatelessWidget {
  const _RouteNotFoundScreen({required this.location});

  final String location;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: SafeArea(
        child: NotFoundStateView(onGoHome: () => context.go(Routes.rooms)),
      ),
    );
  }
}
