/// Credential storage (§12.2).
///
/// Tokens live in the platform keystore — Keychain on iOS, Android Keystore on
/// Android — and never in SharedPreferences, a plain file, or Drift. The
/// distinction matters on a rooted or jailbroken device and in a
/// device-to-device backup: SharedPreferences is readable by anything with
/// root and is included in backups, so a refresh token stored there is a
/// 60-day credential that leaves the device.
library;

import 'dart:async';
import 'dart:convert';

import 'package:dio/dio.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';

import '../network/auth_interceptor.dart';

/// A place to keep secrets. Abstracted so tests do not need a platform channel.
abstract interface class SecretStore {
  Future<String?> read(String key);
  Future<void> write(String key, String value);
  Future<void> delete(String key);
  Future<void> deleteAll();
}

/// [SecretStore] backed by the platform keystore.
class PlatformSecretStore implements SecretStore {
  PlatformSecretStore([FlutterSecureStorage? storage])
      : _storage = storage ??
            const FlutterSecureStorage(
              // AndroidOptions is left at its defaults deliberately.
              //
              // flutter_secure_storage 11 REMOVED `encryptedSharedPreferences`
              // and made the strong path the default: AES-GCM for the data
              // with RSA-OAEP key wrapping in the Keystore. Passing the old
              // flag no longer compiles, and reaching for a custom cipher here
              // would replace a reviewed default with an unreviewed one.
              aOptions: AndroidOptions(),
              iOptions: IOSOptions(
                // Not `first_unlock`: the refresh token is only needed while
                // the user is actually using the app. `_this_device` keeps it
                // out of an iCloud Keychain sync, because a credential that
                // syncs to another device is a credential ON another device.
                accessibility: KeychainAccessibility.unlocked_this_device,
              ),
            );

  final FlutterSecureStorage _storage;

  @override
  Future<String?> read(String key) => _storage.read(key: key);

  @override
  Future<void> write(String key, String value) =>
      _storage.write(key: key, value: value);

  @override
  Future<void> delete(String key) => _storage.delete(key: key);

  @override
  Future<void> deleteAll() => _storage.deleteAll();
}

/// An in-memory [SecretStore] for tests and desktop builds with no keystore.
class MemorySecretStore implements SecretStore {
  final Map<String, String> _values = {};

  @override
  Future<String?> read(String key) async => _values[key];

  @override
  Future<void> write(String key, String value) async => _values[key] = value;

  @override
  Future<void> delete(String key) async => _values.remove(key);

  @override
  Future<void> deleteAll() async => _values.clear();

  /// Whether anything is stored, for assertions about sign-out.
  bool get isEmpty => _values.isEmpty;
}

/// The stored session.
class StoredSession {
  const StoredSession({
    required this.accessToken,
    required this.refreshToken,
    required this.expiresAt,
    required this.sessionId,
    required this.userId,
  });

  final String accessToken;
  final String refreshToken;
  final DateTime expiresAt;
  final String sessionId;
  final String userId;

  Map<String, dynamic> toJson() => {
        'accessToken': accessToken,
        'refreshToken': refreshToken,
        'expiresAt': expiresAt.toUtc().toIso8601String(),
        'sessionId': sessionId,
        'userId': userId,
      };

  static StoredSession? fromJson(Map<String, dynamic> json) {
    final access = json['accessToken'];
    final refresh = json['refreshToken'];
    final expires = DateTime.tryParse(json['expiresAt'] as String? ?? '');
    if (access is! String || refresh is! String || expires == null) {
      // A partially-written record is unusable. Returning null sends the user
      // to sign-in, which is recoverable; returning a half-session would fail
      // later with something far harder to diagnose.
      return null;
    }
    return StoredSession(
      accessToken: access,
      refreshToken: refresh,
      expiresAt: expires,
      sessionId: json['sessionId'] as String? ?? '',
      userId: json['userId'] as String? ?? '',
    );
  }
}

/// The concrete [TokenStore] the auth interceptor drives.
///
/// It owns the refresh CALL; the interceptor owns the single-flight guard. The
/// split matters: this class must be safe to call from the interceptor's lock
/// and must never itself go through the interceptor, or a refresh would trigger
/// a refresh.
class SessionTokenStore implements TokenStore {
  SessionTokenStore({
    required SecretStore secrets,
    required Dio refreshClient,
    void Function()? onSignedOut,
    DateTime Function()? now,
  })  :
        // Dart forbids named parameters that start with an underscore, so
        // `this._secrets` is not available and the analyzer's suggestion does
        // not compile.
        // ignore: prefer_initializing_formals
        _secrets = secrets,
        // ignore: prefer_initializing_formals
        _refreshClient = refreshClient,
        // ignore: prefer_initializing_formals
        _onSignedOut = onSignedOut,
        _now = now ?? DateTime.now;

  final SecretStore _secrets;

  /// A client WITHOUT the auth interceptor. Refreshing through the interceptor
  /// would deadlock: the refresh request would wait on the refresh it is.
  final Dio _refreshClient;

  final void Function()? _onSignedOut;
  final DateTime Function() _now;

  static const _sessionKey = 'vybe.session.v1';

  /// Cached so the common path — attaching a bearer token to every request —
  /// does not hit a platform channel each time. A keystore read is a few
  /// milliseconds, which is nothing once and noticeable on every request.
  StoredSession? _cached;
  bool _loaded = false;

  /// The current session, or null when signed out.
  Future<StoredSession?> session() async {
    if (_loaded) return _cached;
    final raw = await _secrets.read(_sessionKey);
    _loaded = true;
    if (raw == null) return null;
    try {
      final decoded = jsonDecode(raw);
      _cached = decoded is Map<String, dynamic>
          ? StoredSession.fromJson(decoded)
          : null;
    } on FormatException {
      // Corrupt storage. Treat it as signed out rather than crashing on
      // launch — the user can sign in again, and a crash loop cannot be
      // recovered from without reinstalling.
      _cached = null;
    }
    return _cached;
  }

  /// Persists a session after sign-in or registration.
  Future<void> save(StoredSession session) async {
    _cached = session;
    _loaded = true;
    await _secrets.write(_sessionKey, jsonEncode(session.toJson()));
  }

  @override
  Future<String?> accessToken() async => (await session())?.accessToken;

  /// Whether the access token is close enough to expiry to refresh proactively.
  ///
  /// A 30-second margin, because a token that expires while the request is in
  /// flight produces a 401 the user pays a round trip for. Refreshing slightly
  /// early costs nothing — rotation is cheap and the old token dies anyway.
  Future<bool> shouldRefreshSoon() async {
    final current = await session();
    if (current == null) return false;
    return current.expiresAt
        .subtract(const Duration(seconds: 30))
        .isBefore(_now());
  }

  @override
  Future<bool> refresh() async {
    final current = await session();
    if (current == null) return false;

    final response = await _refreshClient.post<dynamic>(
      '/v1/auth/refresh',
      data: {'refreshToken': current.refreshToken},
    );

    if (response.statusCode != 200) {
      // Any non-200 here is terminal. The server collapses expired, unknown,
      // revoked, and reuse-detected into one REFRESH_REJECTED precisely
      // because the client's correct response to all four is identical.
      return false;
    }

    final body = response.data;
    if (body is! Map) return false;

    final access = body['accessToken'];
    if (access is! String || access.isEmpty) return false;

    // The refresh token is ABSENT on the server's overlap-replay path, where
    // the caller already holds a usable one. Keeping the existing token in that
    // case is not a fallback — it is the correct behaviour, and overwriting it
    // with an empty string would sign the user out on a successful refresh.
    final rotated = body['refreshToken'];
    final nextRefresh =
        rotated is String && rotated.isNotEmpty ? rotated : current.refreshToken;

    await save(
      StoredSession(
        accessToken: access,
        refreshToken: nextRefresh,
        expiresAt: DateTime.tryParse(body['expiresAt'] as String? ?? '') ??
            _now().add(const Duration(minutes: 15)),
        sessionId: body['sessionId'] as String? ?? current.sessionId,
        userId: (body['user'] as Map?)?['id'] as String? ?? current.userId,
      ),
    );
    return true;
  }

  @override
  Future<void> onSessionRevoked() async {
    await clear();
    _onSignedOut?.call();
  }

  /// Removes every stored credential.
  Future<void> clear() async {
    _cached = null;
    _loaded = true;
    await _secrets.delete(_sessionKey);
  }
}
