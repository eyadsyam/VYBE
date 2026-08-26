/// Tests for the sign-in and registration controller.
///
/// The controller owns every decision the form makes, which is what lets the
/// interesting cases be tested here rather than through a widget tree: an
/// under-13 date of birth, a duplicate handle, a breached password.
library;

import 'package:dio/dio.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/app/providers.dart';
import 'package:vybe/core/auth/secure_token_store.dart';
import 'package:vybe/core/error/failure.dart';
import 'package:vybe/core/error/result.dart';
import 'package:vybe/core/network/api_client.dart';
import 'package:vybe/features/auth/data/auth_api.dart';
import 'package:vybe/features/auth/domain/account.dart';
import 'package:vybe/features/auth/presentation/auth_controller.dart';

/// An AuthApi that answers from a script instead of a network.
class FakeAuthApi implements AuthApi {
  FakeAuthApi();

  Result<AuthOutcome>? loginResult;
  Result<AuthOutcome>? registerResult;

  int loginCalls = 0;
  int registerCalls = 0;
  Map<String, Object?> lastRegisterArgs = const {};

  @override
  Future<Result<AuthOutcome>> login({
    required String email,
    required String password,
    String? deviceName,
    String? platform,
  }) async {
    loginCalls++;
    return loginResult ?? Result.ok(_outcome());
  }

  @override
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
    registerCalls++;
    lastRegisterArgs = {
      'email': email,
      'handle': handle,
      'displayName': displayName,
      'dateOfBirth': dateOfBirth,
      'locale': locale,
    };
    return registerResult ?? Result.ok(_outcome());
  }

  @override
  Future<Result<void>> logout() async => const Result.ok(null);

  @override
  Future<Result<Account>> me() async => Result.ok(_account());

  @override
  Future<Result<String>> webSocketTicket() async => const Result.ok('ticket');

  static AuthOutcome _outcome() => AuthOutcome(
        session: StoredSession(
          accessToken: 'access',
          refreshToken: 'refresh',
          expiresAt: DateTime.utc(2026, 8, 26, 12, 15),
          sessionId: 'session-1',
          userId: 'user-1',
        ),
        account: _account(),
      );

  static Account _account() => const Account(
        id: 'user-1',
        handle: 'sara_q',
        displayName: 'سارة',
        locale: 'ar',
        region: 'EG',
        numeralSystem: NumeralSystem.western,
        ageBand: AgeBand.adult,
        entitlementTier: EntitlementTier.free,
        isDiscoverable: true,
      );
}

void main() {
  late ProviderContainer container;
  late FakeAuthApi api;
  late MemorySecretStore secrets;

  // Local functions, not getters: Dart does not allow a getter declaration
  // inside a function body, and both must be re-read after every action
  // because the notifier replaces its state rather than mutating it.
  AuthController controller() =>
      container.read(authControllerProvider.notifier);
  AuthState state() => container.read(authControllerProvider);

  setUp(() {
    api = FakeAuthApi();
    secrets = MemorySecretStore();
    container = ProviderContainer(
      overrides: [
        apiEndpointProvider.overrideWithValue(ApiEndpoint.localhost),
        secretStoreProvider.overrideWithValue(secrets),
        publicAuthApiProvider.overrideWithValue(api),
        authApiProvider.overrideWithValue(api),
        unauthenticatedClientProvider.overrideWithValue(Dio()),
      ],
    );
    addTearDown(container.dispose);
  });

  const goodPassword = 'a sufficiently long passphrase';
  final adultDob = DateTime.utc(2000, 1, 1);

  // `omitDateOfBirth` is a separate flag rather than passing null, because
  // `dateOfBirth ?? adultDob` would silently substitute a valid date for the
  // one case that is specifically testing its absence.
  Future<bool> registerWith({
    String email = 'sara@example.com',
    String password = goodPassword,
    String handle = 'sara_q',
    String displayName = 'سارة',
    DateTime? dateOfBirth,
    bool omitDateOfBirth = false,
    String locale = 'ar',
  }) =>
      controller().register(
        email: email,
        password: password,
        handle: handle,
        displayName: displayName,
        dateOfBirth: omitDateOfBirth ? null : (dateOfBirth ?? adultDob),
        locale: locale,
      );

  group('sign in', () {
    test('stores the session on success', () async {
      expect(await controller().signIn(email: 'a@b.co', password: goodPassword),
          isTrue);

      final stored = await container.read(tokenStoreProvider).session();
      expect(stored?.accessToken, 'access');
      expect(stored?.refreshToken, 'refresh');
      expect(state().submitting, isFalse);
    });

    test('does NOT validate the email format locally', () async {
      // Deliberate. The server returns one identical failure for every login
      // problem, so a local format check would be the one place the client
      // distinguishes "that is not an address" from "no such account" — and an
      // attacker could use exactly that to probe for registered emails.
      await controller().signIn(email: 'definitely-not-an-email', password: goodPassword);

      expect(
        api.loginCalls,
        1,
        reason: 'a malformed address must still reach the server, so the '
            'response is indistinguishable from a wrong password',
      );
    });

    test('refuses an empty field without a round trip', () async {
      // An empty field is not an enumeration risk — the user has not entered
      // anything to probe with — so catching it locally is a courtesy rather
      // than a leak.
      expect(await controller().signIn(email: '', password: goodPassword), isFalse);
      expect(api.loginCalls, 0);
      expect(state().fieldErrors[AuthField.email], AuthFieldProblem.required);
    });

    test('surfaces a server failure without inventing a field error', () async {
      // The server says only "invalid credentials". Pinning that to the
      // password field would tell the user their EMAIL was right.
      api.loginResult = const Result.err(
        ServerFailure(status: 401, code: 'INVALID_CREDENTIALS'),
      );

      expect(await controller().signIn(email: 'a@b.co', password: 'wrong'), isFalse);
      expect(state().failure, isA<ServerFailure>());
      expect(state().fieldErrors, isEmpty);
      expect(state().submitting, isFalse);
    });
  });

  group('registration validation', () {
    test('refuses an under-13 date of birth before any request', () async {
      // §12.4. A twelve-year-old does not get an account, and asking the
      // server first would send a child's date of birth over the network only
      // to be told the same thing.
      final twelve = DateTime.now().subtract(const Duration(days: 365 * 12));

      expect(await registerWith(dateOfBirth: twelve), isFalse);
      expect(api.registerCalls, 0);
      expect(
        state().fieldErrors[AuthField.dateOfBirth],
        AuthFieldProblem.underMinimumAge,
      );
    });

    test('accepts a thirteen-year-old', () async {
      // The boundary in the other direction. An off-by-one here would refuse
      // accounts the product is explicitly for.
      final thirteen = DateTime.now().subtract(const Duration(days: 365 * 14));
      expect(await registerWith(dateOfBirth: thirteen), isTrue);
      expect(api.registerCalls, 1);
    });

    test('refuses a short password locally', () async {
      expect(await registerWith(password: 'short'), isFalse);
      expect(api.registerCalls, 0);
      expect(
        state().fieldErrors[AuthField.password],
        AuthFieldProblem.passwordTooShort,
      );
    });

    test('refuses an invalid handle locally', () async {
      for (final handle in ['ab', 'no spaces', '.leading', 'trailing.', 'a..b']) {
        expect(await registerWith(handle: handle), isFalse,
            reason: 'handle was "$handle"');
        expect(
          state().fieldErrors[AuthField.handle],
          AuthFieldProblem.handleInvalid,
          reason: 'handle was "$handle"',
        );
      }
      expect(api.registerCalls, 0);
    });

    test('DOES validate the email format on register', () async {
      // Unlike sign-in. The server distinguishes here too — a signup form
      // cannot function without telling the user which field to fix — so
      // catching a typo locally saves a round trip and leaks nothing.
      expect(await registerWith(email: 'no-at-sign'), isFalse);
      expect(api.registerCalls, 0);
      expect(state().fieldErrors[AuthField.email], AuthFieldProblem.emailInvalid);
    });

    test('reports every local problem at once', () async {
      // One field at a time is the pattern that makes a user submit five
      // times to learn five things.
      await registerWith(
        email: 'bad',
        password: 'short',
        handle: '!!',
        omitDateOfBirth: true,
      );

      expect(state().fieldErrors.keys, containsAll([
        AuthField.email,
        AuthField.password,
        AuthField.handle,
        AuthField.dateOfBirth,
      ]));
    });

    test('normalises the handle before sending', () async {
      // The client and server must normalise identically, or a handle that
      // looks valid here is rejected there.
      await registerWith(handle: '  Sara_Q  ');
      expect(api.lastRegisterArgs['handle'], 'sara_q');
    });

    test('falls back to the handle when no display name is given', () async {
      // An empty display name would render as a blank row in every
      // participant list.
      await registerWith(displayName: '   ');
      expect(api.lastRegisterArgs['displayName'], 'sara_q');
    });
  });

  group('registration failures from the server', () {
    test('pins EMAIL_TAKEN to the email field', () async {
      api.registerResult = const Result.err(
        ServerFailure(status: 409, code: 'EMAIL_TAKEN'),
      );
      expect(await registerWith(), isFalse);
      expect(state().fieldErrors[AuthField.email], AuthFieldProblem.emailTaken);
      expect(state().fieldErrors.containsKey(AuthField.handle), isFalse);
    });

    test('pins HANDLE_TAKEN to the handle field', () async {
      api.registerResult = const Result.err(
        ServerFailure(status: 409, code: 'HANDLE_TAKEN'),
      );
      expect(await registerWith(), isFalse);
      expect(state().fieldErrors[AuthField.handle], AuthFieldProblem.handleTaken);
      expect(state().fieldErrors.containsKey(AuthField.email), isFalse);
    });

    test('pins PASSWORD_BREACHED to the password field', () async {
      // The server has a breach corpus the client does not, so this can only
      // arrive from there — and it must land under the password field rather
      // than in a banner, or the user has to guess what to change.
      api.registerResult = const Result.err(
        ServerFailure(status: 422, code: 'PASSWORD_BREACHED'),
      );
      expect(await registerWith(), isFalse);
      expect(
        state().fieldErrors[AuthField.password],
        AuthFieldProblem.passwordBreached,
      );
    });

    test('maps a validation failure field by field', () async {
      api.registerResult = const Result.err(
        ValidationFailure({'handle': 'INVALID', 'email': 'INVALID'}),
      );
      expect(await registerWith(), isFalse);
      expect(state().fieldErrors[AuthField.handle], AuthFieldProblem.handleInvalid);
      expect(state().fieldErrors[AuthField.email], AuthFieldProblem.emailInvalid);
    });

    test('leaves an unmappable failure in the banner', () async {
      // A 500 has no field to blame. Inventing one would tell the user to
      // change something that was never the problem.
      api.registerResult = const Result.err(
        ServerFailure(status: 500, code: 'INTERNAL'),
      );
      expect(await registerWith(), isFalse);
      expect(state().failure, isA<ServerFailure>());
      expect(state().fieldErrors, isEmpty);
    });
  });

  group('mode switching', () {
    test('clears errors when switching form', () async {
      // Not cosmetic. "That email is already registered" is correct advice on
      // the register form and actively misleading on the sign-in one, where it
      // is the whole point of being there.
      api.registerResult = const Result.err(
        ServerFailure(status: 409, code: 'EMAIL_TAKEN'),
      );
      await registerWith();
      expect(state().fieldErrors, isNotEmpty);

      controller().switchMode(AuthMode.signIn);
      expect(state().fieldErrors, isEmpty);
      expect(state().failure, isNull);
      expect(state().isRegistering, isFalse);
    });
  });

  group('sign out', () {
    test('clears local credentials', () async {
      await controller().signIn(email: 'a@b.co', password: goodPassword);
      expect(secrets.isEmpty, isFalse);

      await controller().signOut();
      expect(secrets.isEmpty, isTrue);
      expect(await container.read(tokenStoreProvider).session(), isNull);
    });

    test('clears them even when the server call fails', () async {
      // If the network is down the user still expects to be signed out of
      // their own phone — and the server session expires on its own anyway.
      await controller().signIn(email: 'a@b.co', password: goodPassword);

      final failing = _FailingLogoutApi(api);
      final failingContainer = ProviderContainer(
        overrides: [
          apiEndpointProvider.overrideWithValue(ApiEndpoint.localhost),
          secretStoreProvider.overrideWithValue(secrets),
          publicAuthApiProvider.overrideWithValue(failing),
          authApiProvider.overrideWithValue(failing),
          unauthenticatedClientProvider.overrideWithValue(Dio()),
        ],
      );
      addTearDown(failingContainer.dispose);

      await failingContainer.read(authControllerProvider.notifier).signOut();
      expect(secrets.isEmpty, isTrue);
    });
  });
}

class _FailingLogoutApi implements AuthApi {
  _FailingLogoutApi(this._inner);
  final FakeAuthApi _inner;

  @override
  Future<Result<void>> logout() async =>
      const Result.err(NetworkFailure(isOffline: true));

  @override
  Future<Result<AuthOutcome>> login({
    required String email,
    required String password,
    String? deviceName,
    String? platform,
  }) =>
      _inner.login(email: email, password: password);

  @override
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
  }) =>
      _inner.register(
        email: email,
        password: password,
        handle: handle,
        displayName: displayName,
        dateOfBirth: dateOfBirth,
        locale: locale,
      );

  @override
  Future<Result<Account>> me() => _inner.me();

  @override
  Future<Result<String>> webSocketTicket() => _inner.webSocketTicket();
}
