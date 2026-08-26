/// The auth data source (§4.2).
///
/// It speaks HTTP and returns [Result] — never throws, and never leaks a
/// [DioException] upward. That boundary is the whole reason this layer exists:
/// a `catch (e)` in a widget is how an unhandled socket error becomes a red
/// screen, and a typed [Failure] is what §3.2's state widgets render from.
library;

import 'package:dio/dio.dart';

import '../../../core/auth/secure_token_store.dart';
import '../../../core/error/failure.dart';
import '../../../core/error/result.dart';
import '../../../core/network/problem_details.dart';
import '../domain/account.dart';

/// Result of a successful authentication.
class AuthOutcome {
  const AuthOutcome({required this.session, required this.account});

  final StoredSession session;
  final Account account;
}

class AuthApi {
  AuthApi(this._dio);

  final Dio _dio;

  /// FR-1. Creates an account and opens a session.
  Future<Result<AuthOutcome>> register({
    required String email,
    required String password,
    required String handle,
    required String displayName,
    required DateTime dateOfBirth,
    required String locale,
    String region = 'EG',
    String? deviceName,
    String? platform,
  }) async {
    return _authCall(
      () => _dio.post<dynamic>('/v1/auth/register', data: {
        'email': email.trim(),
        'password': password,
        'handle': HandleRules.normalise(handle),
        'displayName': displayName.trim(),
        // Date-only, deliberately. A full timestamp would make the age band
        // depend on the device's timezone, and a birth date does not have one.
        'dateOfBirth': _dateOnly(dateOfBirth),
        'locale': locale,
        'region': region,
        'deviceName': ?deviceName,
        'platform': ?platform,
      }),
      expectedStatus: 201,
    );
  }

  /// FR-2. Opens a session.
  Future<Result<AuthOutcome>> login({
    required String email,
    required String password,
    String? deviceName,
    String? platform,
  }) async {
    return _authCall(
      () => _dio.post<dynamic>('/v1/auth/login', data: {
        'email': email.trim(),
        'password': password,
        'deviceName': ?deviceName,
        'platform': ?platform,
      }),
      expectedStatus: 200,
    );
  }

  /// FR-5. Ends the session.
  ///
  /// A failure is reported but must NOT stop the caller clearing local
  /// credentials. If the network is down, the user still expects to be signed
  /// out of their own phone — and the server's session expires on its own.
  Future<Result<void>> logout() async {
    try {
      final response = await _dio.post<dynamic>('/v1/auth/logout');
      if (response.statusCode == 204 || response.statusCode == 401) {
        // 401 means the session was already gone. That is success, not failure.
        return const Result.ok(null);
      }
      return Result.err(_failureFrom(response));
    } on DioException catch (e) {
      return Result.err(_transportFailure(e));
    }
  }

  /// The current user (FR-6).
  Future<Result<Account>> me() async {
    try {
      final response = await _dio.get<dynamic>('/v1/auth/me');
      if (response.statusCode == 200 && response.data is Map) {
        return Result.ok(
          _accountFrom(Map<String, dynamic>.from(response.data as Map)),
        );
      }
      return Result.err(_failureFrom(response));
    } on DioException catch (e) {
      return Result.err(_transportFailure(e));
    }
  }

  /// Mints a single-use WebSocket ticket (ADR-011).
  ///
  /// Requested immediately before connecting, never cached: it expires in 60
  /// seconds and does not survive redemption, so a stored one is guaranteed
  /// stale by the time anything would reuse it.
  Future<Result<String>> webSocketTicket() async {
    try {
      final response = await _dio.post<dynamic>('/v1/auth/ws-ticket');
      if (response.statusCode == 201 && response.data is Map) {
        final ticket = (response.data as Map)['ticket'];
        if (ticket is String && ticket.isNotEmpty) {
          return Result.ok(ticket);
        }
      }
      return Result.err(_failureFrom(response));
    } on DioException catch (e) {
      return Result.err(_transportFailure(e));
    }
  }

  Future<Result<AuthOutcome>> _authCall(
    Future<Response<dynamic>> Function() send, {
    required int expectedStatus,
  }) async {
    try {
      final response = await send();
      if (response.statusCode == expectedStatus && response.data is Map) {
        final body = Map<String, dynamic>.from(response.data as Map);
        final user = body['user'];
        if (user is! Map) {
          return const Result.err(
            UnexpectedFailure('the session response carried no user'),
          );
        }
        return Result.ok(
          AuthOutcome(
            session: StoredSession(
              accessToken: body['accessToken'] as String? ?? '',
              refreshToken: body['refreshToken'] as String? ?? '',
              expiresAt:
                  DateTime.tryParse(body['expiresAt'] as String? ?? '')?.toUtc() ??
                      DateTime.now().toUtc().add(const Duration(minutes: 15)),
              sessionId: body['sessionId'] as String? ?? '',
              userId: user['id'] as String? ?? '',
            ),
            account: _accountFrom(Map<String, dynamic>.from(user)),
          ),
        );
      }
      return Result.err(_failureFrom(response));
    } on DioException catch (e) {
      return Result.err(_transportFailure(e));
    }
  }

  static Account _accountFrom(Map<String, dynamic> json) => Account(
        id: json['id'] as String? ?? '',
        handle: json['handle'] as String? ?? '',
        displayName: json['displayName'] as String? ?? '',
        locale: json['locale'] as String? ?? 'en',
        region: json['region'] as String? ?? 'EG',
        numeralSystem: NumeralSystem.fromWire(json['numeralSystem'] as String?),
        // An unknown band falls back to `adult` rather than a minor band.
        // Getting this backwards would apply §12.4's restrictions to an adult
        // on a server version this client does not recognise — a confusing
        // regression with no error to point at.
        ageBand: AgeBand.fromWire(json['ageBand'] as String?) ?? AgeBand.adult,
        entitlementTier:
            EntitlementTier.fromWire(json['entitlementTier'] as String?),
        isDiscoverable: json['isDiscoverable'] as bool? ?? false,
        avatarUrl: json['avatarUrl'] as String?,
      );

  static Failure _failureFrom(Response<dynamic> response) =>
      ProblemDetails.parse(response.data, response.statusCode ?? 0).toFailure();

  /// Maps a transport-level exception.
  ///
  /// Distinguishing "no network" from "the server said no" matters because the
  /// two get different §3.2 states and different actions: offline offers a
  /// retry when connectivity returns, a server error offers a retry now.
  static Failure _transportFailure(DioException e) => switch (e.type) {
        DioExceptionType.connectionError ||
        DioExceptionType.connectionTimeout =>
          NetworkFailure(isOffline: true, cause: e),
        DioExceptionType.sendTimeout || DioExceptionType.receiveTimeout =>
          NetworkFailure(isOffline: false, cause: e),
        DioExceptionType.cancel => NetworkFailure(isOffline: false, cause: e),
        _ => UnexpectedFailure(e, e.stackTrace),
      };

  static String _dateOnly(DateTime value) =>
      '${value.year.toString().padLeft(4, '0')}-'
      '${value.month.toString().padLeft(2, '0')}-'
      '${value.day.toString().padLeft(2, '0')}';
}
