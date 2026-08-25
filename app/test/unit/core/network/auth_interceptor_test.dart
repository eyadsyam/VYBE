import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/error/failure.dart';
import 'package:vybe/core/network/auth_interceptor.dart';

/// A TokenStore that counts refreshes and can be held open, so concurrency is
/// controlled rather than hoped for.
class FakeTokenStore implements TokenStore {
  FakeTokenStore({this.refreshSucceeds = true});

  int refreshCalls = 0;
  int revokedCalls = 0;
  bool refreshSucceeds;
  String? token = 'access-token-1';

  /// When set, refresh blocks until it completes, letting a test create a real
  /// overlap instead of relying on scheduling luck.
  Completer<void>? gate;

  @override
  Future<String?> accessToken() async => token;

  @override
  Future<bool> refresh() async {
    refreshCalls++;
    if (gate != null) await gate!.future;
    if (!refreshSucceeds) return false;
    token = 'access-token-${refreshCalls + 1}';
    return true;
  }

  @override
  Future<void> onSessionRevoked() async {
    revokedCalls++;
    token = null;
  }
}

/// Returns a 401 with a problem document the given number of times, then 200.
class _Handler {
  _Handler({required this.failFirst, this.code = 'TOKEN_EXPIRED'});

  int failFirst;
  final String code;
  int calls = 0;
  final List<String?> seenAuthHeaders = [];

  Future<Response<dynamic>> handle(RequestOptions options) async {
    calls++;
    seenAuthHeaders.add(options.headers['Authorization'] as String?);
    if (calls <= failFirst) {
      throw DioException(
        requestOptions: options,
        response: Response<dynamic>(
          requestOptions: options,
          statusCode: 401,
          data: {'code': code, 'status': 401, 'title': 'Unauthorized'},
        ),
        type: DioExceptionType.badResponse,
      );
    }
    return Response<dynamic>(
      requestOptions: options,
      statusCode: 200,
      data: {'ok': true},
    );
  }
}

/// Builds a Dio whose transport is [handler], with the interceptor installed.
(Dio, Dio) _buildClient(FakeTokenStore store, _Handler handler) {
  final adapter = _FakeAdapter(handler);

  // The retry client deliberately does NOT carry the interceptor, so a replay
  // that 401s again cannot recurse.
  final retry = Dio(BaseOptions(baseUrl: 'https://api.vybe.test'))
    ..httpClientAdapter = adapter;

  final dio = Dio(BaseOptions(baseUrl: 'https://api.vybe.test'))
    ..httpClientAdapter = adapter
    ..interceptors.add(AuthInterceptor(store, retry));

  return (dio, retry);
}

class _FakeAdapter implements HttpClientAdapter {
  _FakeAdapter(this.handler);
  final _Handler handler;

  @override
  Future<ResponseBody> fetch(
    RequestOptions options,
    Stream<Uint8List>? requestStream,
    Future<void>? cancelFuture,
  ) async {
    try {
      final response = await handler.handle(options);
      return ResponseBody.fromString(
        _encode(response.data),
        response.statusCode ?? 200,
        headers: {
          Headers.contentTypeHeader: [Headers.jsonContentType],
        },
      );
    } on DioException catch (e) {
      return ResponseBody.fromString(
        _encode(e.response?.data),
        e.response?.statusCode ?? 500,
        headers: {
          Headers.contentTypeHeader: [Headers.jsonContentType],
        },
      );
    }
  }

  static String _encode(Object? data) =>
      data == null ? '{}' : const JsonEncoder().convert(data);

  @override
  void close({bool force = false}) {}
}

void main() {
  group('single-flight refresh (ADR-011)', () {
    test('concurrent 401s trigger exactly ONE refresh', () async {
      // This is the property ADR-011 names by hand:
      //
      //   "the client serialises refresh attempts through a single-flight lock
      //    so two concurrent 401s cannot trigger two refreshes"
      //
      // Without it, the first refresh rotates the token and the others present
      // the one they already had — which is now rotated. The server cannot
      // distinguish that from theft, so FR-4 fires and the user is signed out.
      // The bug presents as "VYBE randomly logs me out under load".
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 6);
      final (dio, _) = _buildClient(store, handler);

      final gate = Completer<void>();
      store.gate = gate;

      // Six requests in flight, as a room screen genuinely does.
      final inFlight = [
        for (var i = 0; i < 6; i++) dio.get<dynamic>('/v1/rooms/$i'),
      ];

      // Getting this synchronisation right is the whole test, so it is spelled
      // out rather than approximated with a sleep.
      //
      // Gating on `handler.calls == 6` is WRONG: that counter increments in the
      // transport, which finishes before onError runs. Opening the gate there
      // lets each refresh complete before the next 401 is even seen, so the six
      // refreshes are sequential — correct behaviour for sequential 401s, and a
      // test that silently stops testing concurrency.
      //
      // The signal that actually matters is the first refresh having STARTED.
      for (var turn = 0; turn < 1000 && store.refreshCalls < 1; turn++) {
        await Future<void>.delayed(Duration.zero);
      }
      expect(store.refreshCalls, 1, reason: 'no refresh was ever started');

      // The first refresh is now parked on the gate. Give the other five all
      // the turns they need to reach the lock and join it.
      for (var turn = 0; turn < 200; turn++) {
        await Future<void>.delayed(Duration.zero);
      }

      gate.complete();
      for (final f in inFlight) {
        try {
          await f;
        } on DioException {
          // Not what this test is about.
        }
      }

      expect(
        store.refreshCalls,
        1,
        reason: 'six concurrent 401s caused ${store.refreshCalls} refreshes; '
            'each extra one presents an already-rotated token and trips FR-4',
      );
    });

    test('a later 401 refreshes again once the first flight has finished',
        () async {
      // The lock must not be permanent. Clearing the slot only when it still
      // holds the same future is what makes a second, genuinely later refresh
      // possible without reopening the concurrency window.
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 1);
      final (dio, _) = _buildClient(store, handler);

      await dio.get<dynamic>('/v1/rooms');
      expect(store.refreshCalls, 1);

      handler.failFirst = handler.calls + 1;
      await dio.get<dynamic>('/v1/rooms');
      expect(store.refreshCalls, 2,
          reason: 'the single-flight slot was never released');
    });
  });

  group('replay', () {
    test('a refreshed request is replayed and succeeds', () async {
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 1);
      final (dio, _) = _buildClient(store, handler);

      final response = await dio.get<dynamic>('/v1/rooms');

      expect(response.statusCode, 200);
      expect(handler.calls, 2, reason: 'the request was not replayed');
    });

    test('the replay carries the NEW token, not the expired one', () async {
      // Replaying with the stale token would 401 again and burn the retry.
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 1);
      final (dio, _) = _buildClient(store, handler);

      await dio.get<dynamic>('/v1/rooms');

      expect(handler.seenAuthHeaders.first, 'Bearer access-token-1');
      expect(handler.seenAuthHeaders.last, 'Bearer access-token-2');
    });

    test('a replay that 401s again gives up instead of looping', () async {
      // Two independent guards protect this: the retry client has no
      // interceptor, and the request is marked as replayed.
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 99);
      final (dio, _) = _buildClient(store, handler);

      await expectLater(
        dio.get<dynamic>('/v1/rooms'),
        throwsA(isA<DioException>()),
      );
      expect(store.refreshCalls, 1, reason: 'refresh looped');
      expect(handler.calls, lessThanOrEqualTo(3),
          reason: 'the request looped ${handler.calls} times');
    });
  });

  group('when refresh fails terminally', () {
    test('the session is cleared and the failure says so', () async {
      // EC-8 / ADR-011: reuse was detected, or the refresh token expired. The
      // user must sign in again — and §3.2 requires them to be told why rather
      // than silently bounced.
      final store = FakeTokenStore(refreshSucceeds: false);
      final handler = _Handler(failFirst: 1);
      final (dio, _) = _buildClient(store, handler);

      try {
        await dio.get<dynamic>('/v1/rooms');
        fail('expected the request to fail');
      } on DioException catch (e) {
        expect(e.error, const AuthFailure(AuthReason.sessionRevoked));
      }

      expect(store.revokedCalls, 1, reason: 'local credentials were not cleared');
    });
  });

  group('what is NOT refreshed', () {
    test('a 401 that is not TOKEN_EXPIRED does not refresh', () async {
      // Presenting a token the server has already rejected is a second reuse
      // signal on a family that is probably already being torn down.
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 1, code: 'SESSION_REVOKED');
      final (dio, _) = _buildClient(store, handler);

      await expectLater(
        dio.get<dynamic>('/v1/rooms'),
        throwsA(isA<DioException>()),
      );
      expect(store.refreshCalls, 0);
    });

    test('non-401 statuses pass straight through', () async {
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 0);
      final (dio, _) = _buildClient(store, handler);

      await dio.get<dynamic>('/v1/rooms');
      expect(store.refreshCalls, 0);
    });

    test('auth endpoints are exempt, or refresh would deadlock on itself',
        () async {
      final store = FakeTokenStore();
      final handler = _Handler(failFirst: 1);
      final (dio, _) = _buildClient(store, handler);

      await expectLater(
        dio.get<dynamic>(
          '/v1/auth/token',
          options: Options(extra: {AuthInterceptor.skipAuthExtra: true}),
        ),
        throwsA(isA<DioException>()),
      );
      expect(store.refreshCalls, 0);
      expect(handler.seenAuthHeaders.single, isNull,
          reason: 'an exempt request must carry no bearer token');
    });
  });

  test('the bearer token is attached when present, omitted when signed out',
      () async {
    final store = FakeTokenStore();
    final handler = _Handler(failFirst: 0);
    final (dio, _) = _buildClient(store, handler);

    await dio.get<dynamic>('/v1/rooms');
    expect(handler.seenAuthHeaders.last, 'Bearer access-token-1');

    store.token = null;
    await dio.get<dynamic>('/v1/rooms');
    expect(handler.seenAuthHeaders.last, isNull);
  });
}
