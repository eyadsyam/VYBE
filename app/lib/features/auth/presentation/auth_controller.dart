/// Sign-in and registration state (§4.5).
///
/// The controller owns every decision the form makes, so the widget owns none.
/// That split is what lets the interesting cases — a breached password, an
/// under-13 date of birth, a duplicate handle — be tested without pumping a
/// widget tree.
library;

import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../app/providers.dart';
import '../../../core/error/failure.dart';
import '../domain/account.dart';

/// Which form is showing.
enum AuthMode { signIn, register }

/// What the auth screen is doing.
class AuthState {
  const AuthState({
    this.mode = AuthMode.signIn,
    this.submitting = false,
    this.failure,
    this.fieldErrors = const {},
  });

  final AuthMode mode;
  final bool submitting;

  /// The last failure, for the §3.2 error surface.
  final Failure? failure;

  /// Per-field problems, keyed by field name.
  ///
  /// Separate from [failure] because they render differently: a field error
  /// belongs under its input, while a general failure belongs in a banner. A
  /// single error slot would force one of the two to be rendered in the wrong
  /// place.
  final Map<String, AuthFieldProblem> fieldErrors;

  bool get isRegistering => mode == AuthMode.register;

  AuthState copyWith({
    AuthMode? mode,
    bool? submitting,
    Failure? failure,
    Map<String, AuthFieldProblem>? fieldErrors,
    bool clearFailure = false,
  }) =>
      AuthState(
        mode: mode ?? this.mode,
        submitting: submitting ?? this.submitting,
        failure: clearFailure ? null : (failure ?? this.failure),
        fieldErrors: fieldErrors ?? this.fieldErrors,
      );
}

/// The form's field keys.
///
/// Constants rather than string literals, so a typo at one of the two ends —
/// the controller writing the key, the widget reading it — is a compile error
/// rather than an error message that silently never appears.
abstract final class AuthField {
  static const email = 'email';
  static const password = 'password';
  static const handle = 'handle';
  static const dateOfBirth = 'dateOfBirth';
}

/// A per-field problem, as a code rather than a message.
///
/// FR-61 puts every user-facing string in an .arb file, so the controller must
/// never produce display text. The widget maps these to localised strings.
enum AuthFieldProblem {
  required,
  emailInvalid,
  handleInvalid,
  handleTaken,
  emailTaken,
  passwordTooShort,
  passwordBreached,
  underMinimumAge,
}

class AuthController extends Notifier<AuthState> {
  @override
  AuthState build() => const AuthState();

  void switchMode(AuthMode mode) {
    // Clearing errors on a mode switch is not cosmetic: "that email is already
    // registered" is correct advice on the register form and actively
    // misleading on the sign-in one, where it is the whole point.
    state = AuthState(mode: mode);
  }

  /// Signs in. Returns true on success.
  Future<bool> signIn({required String email, required String password}) async {
    final local = <String, AuthFieldProblem>{};
    if (email.trim().isEmpty) local[AuthField.email] = AuthFieldProblem.required;
    if (password.isEmpty) local[AuthField.password] = AuthFieldProblem.required;
    if (local.isNotEmpty) {
      state = state.copyWith(fieldErrors: local, clearFailure: true);
      return false;
    }

    state = state.copyWith(
      submitting: true,
      fieldErrors: const {},
      clearFailure: true,
    );

    // Deliberately NO local email-format check on sign-in.
    //
    // The server returns one identical failure for every login problem, so
    // rejecting a malformed address here would be the one place the client
    // distinguishes "that is not an address" from "no such account" — and an
    // attacker could use exactly that to probe. On the REGISTER form the check
    // is fine and useful, because the server distinguishes there too.
    final result = await ref.read(publicAuthApiProvider).login(
          email: email,
          password: password,
        );

    return result.fold(
      ok: (outcome) async {
        await ref.read(tokenStoreProvider).save(outcome.session);
        ref.invalidate(sessionProvider);
        state = state.copyWith(submitting: false);
        return true;
      },
      err: (failure) {
        state = state.copyWith(submitting: false, failure: failure);
        return Future.value(false);
      },
    );
  }

  /// Registers. Returns true on success.
  Future<bool> register({
    required String email,
    required String password,
    required String handle,
    required String displayName,
    required DateTime? dateOfBirth,
    required String locale,
  }) async {
    final local = <String, AuthFieldProblem>{};

    if (email.trim().isEmpty) {
      local[AuthField.email] = AuthFieldProblem.required;
    } else if (!_looksLikeEmail(email)) {
      local[AuthField.email] = AuthFieldProblem.emailInvalid;
    }

    if (handle.trim().isEmpty) {
      local[AuthField.handle] = AuthFieldProblem.required;
    } else if (!HandleRules.isValid(handle)) {
      local[AuthField.handle] = AuthFieldProblem.handleInvalid;
    }

    switch (PasswordRules.check(password)) {
      case PasswordProblem.tooShort:
        local[AuthField.password] = AuthFieldProblem.passwordTooShort;
      case PasswordProblem.tooLong:
        local[AuthField.password] = AuthFieldProblem.passwordTooShort;
      case PasswordProblem.breached || null:
        break;
    }

    if (dateOfBirth == null) {
      local[AuthField.dateOfBirth] = AuthFieldProblem.required;
    } else if (deriveAgeBand(dateOfBirth, DateTime.now()) == AgeBand.under13) {
      // Refused HERE, before any request. §12.4 means a twelve-year-old does
      // not get an account, and asking the server first would send a child's
      // date of birth over the network to be told the same thing.
      local[AuthField.dateOfBirth] = AuthFieldProblem.underMinimumAge;
    }

    if (local.isNotEmpty) {
      state = state.copyWith(fieldErrors: local, clearFailure: true);
      return false;
    }

    state = state.copyWith(
      submitting: true,
      fieldErrors: const {},
      clearFailure: true,
    );

    final result = await ref.read(publicAuthApiProvider).register(
          email: email,
          password: password,
          handle: handle,
          displayName: displayName.trim().isEmpty
              ? HandleRules.normalise(handle)
              : displayName,
          dateOfBirth: dateOfBirth!,
          locale: locale,
        );

    return result.fold(
      ok: (outcome) async {
        await ref.read(tokenStoreProvider).save(outcome.session);
        ref.invalidate(sessionProvider);
        state = state.copyWith(submitting: false);
        return true;
      },
      err: (failure) {
        state = state.copyWith(
          submitting: false,
          failure: failure,
          fieldErrors: _fieldErrorsFrom(failure),
        );
        return Future.value(false);
      },
    );
  }

  /// Signs out.
  ///
  /// Local credentials are cleared even when the server call fails. If the
  /// network is down, the user still expects to be signed out of their own
  /// phone — and the server session expires on its own regardless.
  Future<void> signOut() async {
    await ref.read(authApiProvider).logout();
    await ref.read(tokenStoreProvider).clear();
    ref.invalidate(sessionProvider);
    ref.invalidate(accountProvider);
  }

  /// Maps a server failure onto the field it belongs under.
  ///
  /// Registration is the one place the server distinguishes which field
  /// collided, because a signup form cannot function otherwise.
  static Map<String, AuthFieldProblem> _fieldErrorsFrom(Failure failure) =>
      switch (failure) {
        ServerFailure(code: 'EMAIL_TAKEN') => {
            AuthField.email: AuthFieldProblem.emailTaken,
          },
        ServerFailure(code: 'HANDLE_TAKEN') => {
            AuthField.handle: AuthFieldProblem.handleTaken,
          },
        ServerFailure(code: 'PASSWORD_BREACHED') => {
            AuthField.password: AuthFieldProblem.passwordBreached,
          },
        ServerFailure(code: 'PASSWORD_WEAK') => {
            AuthField.password: AuthFieldProblem.passwordTooShort,
          },
        ServerFailure(code: 'UNDER_MINIMUM_AGE') => {
            AuthField.dateOfBirth: AuthFieldProblem.underMinimumAge,
          },
        ValidationFailure(:final fields) => {
            for (final entry in fields.entries)
              entry.key: switch (entry.key) {
                'email' => AuthFieldProblem.emailInvalid,
                'handle' => AuthFieldProblem.handleInvalid,
                'password' => AuthFieldProblem.passwordTooShort,
                _ => AuthFieldProblem.required,
              },
          },
        _ => const {},
      };

  /// A deliberately loose email check.
  ///
  /// Full RFC 5322 validation accepts addresses no provider will deliver to and
  /// rejects nothing an attacker cares about. The only validation that means
  /// anything is sending a confirmation mail; this exists to catch a typo like
  /// a missing @, and nothing more.
  static bool _looksLikeEmail(String value) {
    final trimmed = value.trim();
    final at = trimmed.indexOf('@');
    if (at <= 0 || at == trimmed.length - 1) return false;
    final domain = trimmed.substring(at + 1);
    return !domain.contains('@') &&
        domain.contains('.') &&
        !trimmed.contains(' ');
  }
}

final authControllerProvider =
    NotifierProvider<AuthController, AuthState>(AuthController.new);
