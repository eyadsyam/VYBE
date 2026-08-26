/// The dependency graph (§4.5).
///
/// Riverpod providers are the app's composition root, and they live in one
/// file for the same reason the server's does: "what depends on what" should be
/// a question you answer by reading, not by tracing constructors through twenty
/// files.
///
/// Providers here are *wiring only*. Anything with a decision in it belongs in
/// a service or a controller, because a provider body is the one place in the
/// app that is awkward to test directly.
library;

import 'package:dio/dio.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../core/auth/secure_token_store.dart';
import '../core/network/api_client.dart';
import '../features/auth/data/auth_api.dart';
import '../features/auth/domain/account.dart';
import '../features/rooms/data/rooms_api.dart';

/// Where the API lives.
///
/// Overridden in `main` per build flavour and in tests with a fake server. A
/// provider rather than a constant so a test never has to reach a real host to
/// find out it is not there.
final apiEndpointProvider = Provider<ApiEndpoint>(
  (ref) => throw UnimplementedError(
    'apiEndpointProvider must be overridden at the root of the app',
  ),
);

/// Where secrets go.
///
/// Overridden with [MemorySecretStore] in tests and on desktop, where there is
/// no platform keystore to talk to.
final secretStoreProvider = Provider<SecretStore>(
  (ref) => PlatformSecretStore(),
);

/// A Dio with NO auth interceptor.
///
/// Used for the auth endpoints themselves. Refreshing through the authenticated
/// client would deadlock: the refresh request would wait on the refresh it is.
final unauthenticatedClientProvider = Provider<Dio>((ref) {
  return createApiClient(endpoint: ref.watch(apiEndpointProvider));
});

/// The credential store, and the thing that performs a refresh.
final Provider<SessionTokenStore> tokenStoreProvider =
    Provider<SessionTokenStore>((ref) {
  return SessionTokenStore(
    secrets: ref.watch(secretStoreProvider),
    refreshClient: ref.watch(unauthenticatedClientProvider),
    onSignedOut: () {
      // A terminal refresh failure means the session is gone for good — the
      // family was revoked or the token expired. Invalidating the session
      // provider is what makes the router redirect to sign-in, so the user is
      // never left staring at a screen whose data will never load.
      ref.invalidate(sessionProvider);
    },
  );
});

/// The authenticated client. Everything that is not an auth endpoint uses this.
final apiClientProvider = Provider<Dio>((ref) {
  return createApiClient(
    endpoint: ref.watch(apiEndpointProvider),
    tokens: ref.watch(tokenStoreProvider),
  );
});

final authApiProvider = Provider<AuthApi>(
  (ref) => AuthApi(ref.watch(apiClientProvider)),
);

/// The auth API without a bearer token, for register and login.
final publicAuthApiProvider = Provider<AuthApi>(
  (ref) => AuthApi(ref.watch(unauthenticatedClientProvider)),
);

final roomsApiProvider = Provider<RoomsApi>(
  (ref) => RoomsApi(ref.watch(apiClientProvider)),
);

/// Whether anybody is signed in, and who.
///
/// The router watches this to decide between the sign-in screen and the app.
/// It is a FutureProvider because answering requires a keystore read, and
/// pretending that is synchronous would mean showing the sign-in screen for a
/// frame to somebody who is already signed in.
final FutureProvider<StoredSession?> sessionProvider =
    FutureProvider<StoredSession?>((ref) async {
  return ref.watch(tokenStoreProvider).session();
});

/// The signed-in user's profile.
///
/// Separate from [sessionProvider] because a session is a credential and an
/// account is data: the first is needed to make any request at all, the second
/// only to render a name.
final accountProvider = FutureProvider<Account?>((ref) async {
  final session = await ref.watch(sessionProvider.future);
  if (session == null) return null;

  final result = await ref.watch(authApiProvider).me();
  return result.fold(ok: (account) => account, err: (_) => null);
});
