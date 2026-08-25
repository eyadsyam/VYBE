/// Access-token refresh with a **single-flight** lock (ADR-011, EC-7).
///
/// ADR-011 does not merely suggest this; it names it as the mitigation that
/// makes reuse detection survivable:
///
/// > the client serialises refresh attempts through a single-flight lock so two
/// > concurrent 401s cannot trigger two refreshes
///
/// The failure it prevents is specific and severe. A room screen issues several
/// requests at once. The access token expires. Every one of them comes back
/// 401. Without a lock, each spawns its own refresh; the first rotates the
/// refresh token and the rest present the one they already had — which is now
/// rotated. The server cannot distinguish that from theft, so it does exactly
/// what FR-4 requires: revokes the family and signs the user out.
///
/// So the bug does not look like a race. It looks like "VYBE randomly logs me
/// out", it happens under load, and it is caused by the security feature
/// working correctly against a client that asked for it.
library;

import 'dart:async';

import 'package:dio/dio.dart';

import '../error/failure.dart';
import 'problem_details.dart';

/// Supplies and refreshes credentials. Implemented by the auth repository;
/// declared here so this file depends on no feature.
abstract interface class TokenStore {
  /// The current access token, or null when signed out.
  Future<String?> accessToken();

  /// Exchanges the refresh token for a new pair.
  ///
  /// Returns false when refresh failed terminally — the family was revoked, or
  /// the refresh token expired — meaning the user must sign in again.
  Future<bool> refresh();

  /// Clears local credentials after a terminal refresh failure.
  Future<void> onSessionRevoked();
}

/// Adds the bearer token, and refreshes exactly once when it has expired.
class AuthInterceptor extends Interceptor {
  AuthInterceptor(this._tokens, [this._retryClient]);

  final TokenStore _tokens;

  /// The client used to replay a request after a successful refresh.
  ///
  /// It must NOT carry this interceptor, or a replay that 401s again would
  /// recurse. [_replayed] guards that too, but two independent guards on an
  /// infinite loop is the right number.
  final Dio? _retryClient;

  /// The in-flight refresh, if one is running.
  ///
  /// This single field is the entire single-flight mechanism: the first caller
  /// creates the Future, every concurrent caller awaits the same one, and all
  /// of them observe one refresh and one rotation.
  Future<bool>? _inFlight;

  /// Marks a request as already replayed, so a second 401 gives up instead of
  /// looping.
  static const _replayed = 'vybe.replayed';

  /// Marks a request as exempt from the bearer token — the auth endpoints
  /// themselves, which would otherwise deadlock waiting for a refresh that is
  /// waiting for them.
  static const skipAuthExtra = 'vybe.skipAuth';

  @override
  Future<void> onRequest(
    RequestOptions options,
    RequestInterceptorHandler handler,
  ) async {
    if (options.extra[skipAuthExtra] == true) {
      return handler.next(options);
    }
    final token = await _tokens.accessToken();
    if (token != null && token.isNotEmpty) {
      options.headers['Authorization'] = 'Bearer $token';
    }
    handler.next(options);
  }

  @override
  Future<void> onError(
    DioException err,
    ErrorInterceptorHandler handler,
  ) async {
    if (!_shouldAttemptRefresh(err)) {
      return handler.next(err);
    }

    final refreshed = await _refreshOnce();

    if (!refreshed) {
      // Terminal. The user must sign in again, and must be TOLD why — §3.2
      // forbids the silent bounce to a login screen, and EC-8 says a revoked
      // session is explained rather than merely enforced.
      await _tokens.onSessionRevoked();
      return handler.next(
        DioException(
          requestOptions: err.requestOptions,
          response: err.response,
          type: err.type,
          error: const AuthFailure(AuthReason.sessionRevoked),
        ),
      );
    }

    try {
      final replayed = await _replay(err.requestOptions);
      return handler.resolve(replayed);
    } on DioException catch (e) {
      return handler.next(e);
    }
  }

  /// Runs at most one refresh at a time (ADR-011).
  ///
  /// The `whenComplete` clears the slot only if it still holds *this* future.
  /// Clearing unconditionally would let a refresh that finished late wipe a
  /// newer one that had already started, and the next caller would then begin a
  /// third — reintroducing exactly the concurrency this exists to remove.
  Future<bool> _refreshOnce() {
    final existing = _inFlight;
    if (existing != null) return existing;

    final future = _tokens.refresh();
    _inFlight = future;
    return future.whenComplete(() {
      if (identical(_inFlight, future)) {
        _inFlight = null;
      }
    });
  }

  Future<Response<dynamic>> _replay(RequestOptions options) async {
    final client = _retryClient;
    if (client == null) {
      throw DioException(
        requestOptions: options,
        error: const UnexpectedFailure('no retry client configured'),
      );
    }

    final token = await _tokens.accessToken();
    final headers = Map<String, dynamic>.from(options.headers);
    if (token != null && token.isNotEmpty) {
      headers['Authorization'] = 'Bearer $token';
    }

    return client.fetch<dynamic>(
      options.copyWith(
        headers: headers,
        extra: {...options.extra, _replayed: true},
      ),
    );
  }

  bool _shouldAttemptRefresh(DioException err) {
    if (err.response?.statusCode != 401) return false;
    if (err.requestOptions.extra[_replayed] == true) return false;
    if (err.requestOptions.extra[skipAuthExtra] == true) return false;

    // Only TOKEN_EXPIRED is refreshable. A 401 for any other reason —
    // SESSION_REVOKED above all — means refreshing would present a token the
    // server has already rejected, which is a second reuse signal on a family
    // that is likely already being torn down.
    final problem = ProblemDetails.parse(err.response?.data, 401);
    return problem.code == ProblemCode.tokenExpired ||
        problem.code == ProblemCode.unauthorized;
  }
}
