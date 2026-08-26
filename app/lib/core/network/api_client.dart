/// The HTTP client factory (§4.3).
///
/// One place builds the client, so timeouts, the trace header, and the retry
/// policy cannot drift between call sites. A feature that constructs its own
/// [Dio] gets none of this, which is why nothing else in the app is allowed to.
library;

import 'dart:async';
import 'dart:math';

import 'package:dio/dio.dart';

import 'auth_interceptor.dart';
import 'problem_details.dart';

/// Where the API lives.
///
/// A class rather than a bare string so the WebSocket URL is derived from the
/// same value. Deriving it is not fussiness: a base URL and a socket URL
/// configured independently WILL diverge between environments, and the failure
/// is a client that talks to staging over HTTP and production over WebSocket.
class ApiEndpoint {
  const ApiEndpoint(this.baseUrl);

  /// The HTTP origin, with no trailing slash.
  final String baseUrl;

  /// The WebSocket origin for the same host.
  ///
  /// `https` becomes `wss` and `http` becomes `ws`. Downgrading a secure origin
  /// to an insecure socket would put the ticket on the wire in plaintext, so
  /// the mapping is one-directional by construction.
  String get socketUrl {
    if (baseUrl.startsWith('https://')) {
      return baseUrl.replaceFirst('https://', 'wss://');
    }
    if (baseUrl.startsWith('http://')) {
      return baseUrl.replaceFirst('http://', 'ws://');
    }
    return baseUrl;
  }

  /// The local stack from docker-compose.
  ///
  /// 10.0.2.2 rather than localhost: on the Android emulator, localhost is the
  /// emulated device itself, so a developer following the README on Android
  /// would otherwise get a connection refused with nothing to explain it.
  static const androidEmulator = ApiEndpoint('http://10.0.2.2:8080');

  /// The local stack from a simulator or desktop build.
  static const localhost = ApiEndpoint('http://127.0.0.1:8080');
}

/// Builds the app's Dio client.
///
/// [tokens] attaches the bearer token and performs single-flight refresh.
/// [now] and [delay] are injected so the retry policy is testable without
/// waiting real seconds.
Dio createApiClient({
  required ApiEndpoint endpoint,
  TokenStore? tokens,
  String Function()? traceIdFactory,
  Future<void> Function(Duration)? delay,
  HttpClientAdapter? adapter,
  Random? random,
}) {
  final dio = Dio(
    BaseOptions(
      baseUrl: endpoint.baseUrl,

      // Three separate timeouts, because they fail differently and a single
      // number cannot express all three. `connect` is a dead host — fail fast.
      // `receive` must be generous enough for a slow mobile network but short
      // enough that a hung request does not look like a frozen app.
      connectTimeout: const Duration(seconds: 10),
      sendTimeout: const Duration(seconds: 20),
      receiveTimeout: const Duration(seconds: 30),

      // Never throw on a status code. Dio's default turns every 4xx into an
      // exception, which would make the RFC 9457 body — the part that says
      // WHAT went wrong — something you have to dig out of an error object.
      // Handling status explicitly is what makes §4.4's mapping possible.
      validateStatus: (_) => true,

      contentType: 'application/json',
      responseType: ResponseType.json,
      headers: const {'Accept': 'application/json, application/problem+json'},
    ),
  );

  if (adapter != null) {
    dio.httpClientAdapter = adapter;
  }

  dio.interceptors.add(TraceInterceptor(traceIdFactory ?? newTraceId));

  if (tokens != null) {
    // A separate client for replays, WITHOUT the auth interceptor, so a replay
    // that 401s again cannot recurse into another refresh.
    final retryClient = Dio(dio.options)
      ..httpClientAdapter = dio.httpClientAdapter
      ..interceptors.add(TraceInterceptor(traceIdFactory ?? newTraceId));
    dio.interceptors.add(AuthInterceptor(tokens, retryClient));
  }

  dio.interceptors.add(
    RetryInterceptor(delay: delay ?? Future.delayed, random: random),
  );

  return dio;
}

/// Attaches `X-Trace-Id` to every request (§14.2).
///
/// The client generates the id, not the server. That is the whole point: a user
/// reporting "it failed at 3pm" can be joined to server logs by an id their app
/// already recorded, which is impossible if the server invents one the client
/// never sees.
class TraceInterceptor extends Interceptor {
  TraceInterceptor(this._newId);

  final String Function() _newId;

  static const header = 'X-Trace-Id';

  @override
  void onRequest(RequestOptions options, RequestInterceptorHandler handler) {
    options.headers.putIfAbsent(header, _newId);
    handler.next(options);
  }
}

final _traceRandom = Random.secure();

/// A 128-bit hex trace id.
///
/// Random rather than sequential: a counter would leak how many requests the
/// app has made, and would collide across reinstalls.
String newTraceId() {
  final buffer = StringBuffer();
  for (var i = 0; i < 16; i++) {
    buffer.write(_traceRandom.nextInt(256).toRadixString(16).padLeft(2, '0'));
  }
  return buffer.toString();
}

/// Retries requests that are safe to retry (§4.3).
///
/// Two rules decide whether a request may be retried, and both must hold:
///
///   * **The method must be idempotent, OR the request must carry an
///     Idempotency-Key.** Retrying a POST without one can create two rooms.
///     This is not theoretical — it is the exact failure FR-57 exists to
///     prevent, and the client half of that contract is refusing to retry when
///     the key is absent.
///   * **The failure must be transient.** A 500 might succeed next time; a 422
///     will fail identically forever, and retrying it is just noise.
///
/// A 429 is honoured rather than retried on the normal schedule: the server
/// said how long to wait, and ignoring that is how a rate limit becomes a
/// thundering herd.
class RetryInterceptor extends Interceptor {
  RetryInterceptor({
    required Future<void> Function(Duration) delay,
    Random? random,
    this.maxAttempts = 3,
  })  :
        // Dart forbids a named parameter starting with an underscore, so
        // `this._delay` is not available here and the lint's suggestion does
        // not compile.
        // ignore: prefer_initializing_formals
        _delay = delay,
        _random = random ?? Random();

  final Future<void> Function(Duration) _delay;
  final Random _random;

  /// Total attempts including the first. Three means at most two retries.
  ///
  /// Small on purpose: a mobile client retrying five times against a struggling
  /// server is part of the problem, not a workaround for it.
  final int maxAttempts;

  static const _attemptKey = 'vybe.attempt';

  /// Methods that are safe to repeat by definition.
  static const _idempotentMethods = {'GET', 'HEAD', 'OPTIONS', 'PUT', 'DELETE'};

  /// The header that makes an unsafe method safe to repeat (FR-57).
  static const idempotencyHeader = 'Idempotency-Key';

  @override
  Future<void> onResponse(
    Response<dynamic> response,
    ResponseInterceptorHandler handler,
  ) async {
    // validateStatus lets everything through as a response, so retryable
    // failures arrive here rather than in onError.
    if (!_shouldRetryStatus(response.statusCode)) {
      handler.next(response);
      return;
    }
    if (!_mayRetry(response.requestOptions)) {
      handler.next(response);
      return;
    }

    final attempt = (response.requestOptions.extra[_attemptKey] as int?) ?? 1;
    if (attempt >= maxAttempts) {
      handler.next(response);
      return;
    }

    final wait = _backoffFor(attempt, response);
    await _delay(wait);

    final options = response.requestOptions;
    options.extra[_attemptKey] = attempt + 1;

    try {
      final retried = await Dio(
        BaseOptions(
          baseUrl: options.baseUrl,
          validateStatus: (_) => true,
        ),
      ).fetch<dynamic>(options);
      handler.resolve(retried);
    } on DioException catch (_) {
      // The retry itself failed at the transport layer. Surface the ORIGINAL
      // response rather than the retry's error: the caller's mapping already
      // handles a 503, and swapping it for a socket exception would lose the
      // problem body that says what happened.
      handler.next(response);
    }
  }

  @override
  Future<void> onError(
    DioException err,
    ErrorInterceptorHandler handler,
  ) async {
    // Transport failures — no response at all. A timeout or a dropped
    // connection is exactly the transient case worth retrying.
    if (!_isTransientTransportError(err) || !_mayRetry(err.requestOptions)) {
      handler.next(err);
      return;
    }

    final attempt = (err.requestOptions.extra[_attemptKey] as int?) ?? 1;
    if (attempt >= maxAttempts) {
      handler.next(err);
      return;
    }

    await _delay(_backoffFor(attempt, null));

    final options = err.requestOptions;
    options.extra[_attemptKey] = attempt + 1;

    try {
      final retried = await Dio(
        BaseOptions(baseUrl: options.baseUrl, validateStatus: (_) => true),
      ).fetch<dynamic>(options);
      handler.resolve(retried);
    } on DioException catch (retryError) {
      handler.next(retryError);
    }
  }

  bool _shouldRetryStatus(int? status) {
    if (status == null) return false;
    // 408 request timeout, 429 rate limited, and 5xx except 501.
    if (status == 408 || status == 429) return true;
    if (status == 501) return false; // not implemented will never succeed
    return status >= 500 && status < 600;
  }

  bool _isTransientTransportError(DioException err) => switch (err.type) {
        DioExceptionType.connectionTimeout ||
        DioExceptionType.sendTimeout ||
        DioExceptionType.receiveTimeout ||
        DioExceptionType.connectionError =>
          true,
        // A cancellation is the user's decision. Retrying it would resurrect a
        // request they explicitly abandoned.
        DioExceptionType.cancel => false,
        _ => false,
      };

  /// Whether repeating this request is safe.
  bool _mayRetry(RequestOptions options) {
    final method = options.method.toUpperCase();
    if (_idempotentMethods.contains(method)) return true;

    // An unsafe method is retryable ONLY with an idempotency key. Without one,
    // a retried POST /v1/rooms creates a second room and the user cannot tell
    // which is theirs.
    return options.headers.keys.any(
      (k) => k.toLowerCase() == idempotencyHeader.toLowerCase(),
    );
  }

  /// Exponential backoff with full jitter, capped.
  ///
  /// Jitter is not decoration. Without it every client that failed during the
  /// same outage retries at the same instant, and the server is hit by a
  /// synchronised wave precisely as it recovers — turning a brief blip into a
  /// sustained outage.
  Duration _backoffFor(int attempt, Response<dynamic>? response) {
    final retryAfter = _retryAfterFrom(response);
    if (retryAfter != null) {
      // The server named a delay. Honouring it is the whole point of the
      // header; a client that backs off less than instructed is the reason
      // rate limits get tightened.
      return retryAfter;
    }

    const base = Duration(milliseconds: 300);
    const cap = Duration(seconds: 8);
    final exponential = base * pow(2, attempt - 1).toDouble();
    final bounded = exponential > cap ? cap : exponential;
    return Duration(
      milliseconds: _random.nextInt(bounded.inMilliseconds + 1),
    );
  }

  /// Reads the delay a 429 asked for, from the problem body or the header.
  Duration? _retryAfterFrom(Response<dynamic>? response) {
    if (response == null) return null;

    // The RFC 9457 extension member first, because it is ours and is always
    // seconds. Retry-After may be an HTTP date, which nobody gets right.
    final problem = ProblemDetails.parse(
      response.data,
      response.statusCode ?? 0,
    );
    final seconds = problem.retryAfterSeconds;
    if (seconds != null && seconds > 0) {
      return Duration(seconds: seconds);
    }

    final header = response.headers.value('retry-after');
    final parsed = header == null ? null : int.tryParse(header.trim());
    if (parsed != null && parsed > 0) {
      return Duration(seconds: parsed);
    }
    return null;
  }
}
