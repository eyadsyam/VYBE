/// The sign-in and registration screen.
///
/// One screen with two modes rather than two screens, because the fields
/// overlap and the user switches between them constantly when they cannot
/// remember whether they already have an account.
library;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/router.dart';
import '../../../core/error/failure.dart';
import '../../../core/error/failure_presenter.dart';
import '../../../core/ui/tokens.dart';
import '../../../l10n/generated/app_localizations.dart';
import 'auth_controller.dart';

class SignInScreen extends ConsumerStatefulWidget {
  const SignInScreen({super.key, this.next});

  /// Where to go after signing in, from a deep link that arrived signed-out.
  final String? next;

  @override
  ConsumerState<SignInScreen> createState() => _SignInScreenState();
}

class _SignInScreenState extends ConsumerState<SignInScreen> {
  final _email = TextEditingController();
  final _password = TextEditingController();
  final _handle = TextEditingController();
  final _displayName = TextEditingController();
  DateTime? _dateOfBirth;

  @override
  void dispose() {
    _email.dispose();
    _password.dispose();
    _handle.dispose();
    _displayName.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final controller = ref.read(authControllerProvider.notifier);
    final state = ref.read(authControllerProvider);

    final ok = state.isRegistering
        ? await controller.register(
            email: _email.text,
            password: _password.text,
            handle: _handle.text,
            displayName: _displayName.text,
            dateOfBirth: _dateOfBirth,
            locale: Localizations.localeOf(context).languageCode,
          )
        : await controller.signIn(
            email: _email.text,
            password: _password.text,
          );

    if (!ok || !mounted) return;

    // The router's redirect handles this too, but going explicitly means the
    // deep link that brought us here is honoured immediately rather than after
    // the session provider settles.
    final next = widget.next;
    if (next != null && next.isNotEmpty) {
      context.go(Uri.decodeComponent(next));
    } else {
      context.go(Routes.rooms);
    }
  }

  Future<void> _pickDateOfBirth() async {
    final now = DateTime.now();
    final picked = await showDatePicker(
      context: context,
      // Opens on a plausible adult birth year rather than today. Defaulting to
      // today means every user scrolls back two hundred and forty months.
      initialDate: _dateOfBirth ?? DateTime(now.year - 20, now.month, now.day),
      firstDate: DateTime(now.year - 120),
      lastDate: now,
    );
    if (picked != null) setState(() => _dateOfBirth = picked);
  }

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final state = ref.watch(authControllerProvider);
    final controller = ref.read(authControllerProvider.notifier);

    return Scaffold(
      appBar: AppBar(
        title: Text(
          state.isRegistering ? l10n.authCreateAccountTitle : l10n.authSignInTitle,
        ),
      ),
      body: SafeArea(
        // Scrollable unconditionally, not just when it overflows. NFR-16
        // requires the UI to work at 200% text scale, and at that size this
        // form is taller than any phone — a Column that only scrolls sometimes
        // is a Column that overflows on somebody's device.
        child: SingleChildScrollView(
          padding: Space.screen,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              if (state.failure != null) ...[
                _FailureBanner(failure: state.failure!),
                const SizedBox(height: Space.lg),
              ],
              TextField(
                controller: _email,
                keyboardType: TextInputType.emailAddress,
                autocorrect: false,
                enableSuggestions: false,
                autofillHints: const [AutofillHints.email],
                // Always LTR: an email address is Latin script even in an
                // Arabic UI, and rendering it RTL puts the domain first.
                textDirection: TextDirection.ltr,
                decoration: InputDecoration(
                  labelText: l10n.authEmailLabel,
                  errorText: _errorFor(l10n, state, AuthField.email),
                ),
              ),
              const SizedBox(height: Space.lg),
              TextField(
                controller: _password,
                obscureText: true,
                autocorrect: false,
                enableSuggestions: false,
                autofillHints: [
                  state.isRegistering
                      ? AutofillHints.newPassword
                      : AutofillHints.password,
                ],
                textDirection: TextDirection.ltr,
                decoration: InputDecoration(
                  labelText: l10n.authPasswordLabel,
                  errorText: _errorFor(l10n, state, AuthField.password),
                ),
              ),
              if (state.isRegistering) ...[
                const SizedBox(height: Space.lg),
                TextField(
                  controller: _handle,
                  autocorrect: false,
                  enableSuggestions: false,
                  // ASCII-only by policy, so LTR regardless of UI direction.
                  textDirection: TextDirection.ltr,
                  decoration: InputDecoration(
                    labelText: l10n.authHandleLabel,
                    helperText: l10n.authHandleHint,
                    errorText: _errorFor(l10n, state, AuthField.handle),
                  ),
                ),
                const SizedBox(height: Space.lg),
                TextField(
                  controller: _displayName,
                  // NO textDirection override. The display name is the field
                  // that carries somebody's actual name, in any script, so it
                  // must follow the text the user types rather than the UI.
                  decoration: InputDecoration(
                    labelText: l10n.authDisplayNameLabel,
                  ),
                ),
                const SizedBox(height: Space.lg),
                _DateOfBirthField(
                  value: _dateOfBirth,
                  errorText: _errorFor(l10n, state, AuthField.dateOfBirth),
                  onPressed: _pickDateOfBirth,
                ),
              ],
              const SizedBox(height: Space.xl),
              FilledButton(
                onPressed: state.submitting ? null : _submit,
                child: state.submitting
                    ? const SizedBox(
                        height: Space.lg,
                        width: Space.lg,
                        child: CircularProgressIndicator(strokeWidth: 2),
                      )
                    : Text(
                        state.isRegistering
                            ? l10n.createRoomAction
                            : l10n.authSignInTitle,
                      ),
              ),
              const SizedBox(height: Space.md),
              TextButton(
                onPressed: state.submitting
                    ? null
                    : () => controller.switchMode(
                          state.isRegistering ? AuthMode.signIn : AuthMode.register,
                        ),
                child: Text(
                  state.isRegistering
                      ? l10n.authSwitchToSignIn
                      : l10n.authSwitchToRegister,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }

  /// Maps a field problem code to a localised string (FR-61).
  String? _errorFor(L10n l10n, AuthState state, String field) {
    final problem = state.fieldErrors[field];
    if (problem == null) return null;
    return switch (problem) {
      AuthFieldProblem.required => l10n.authRequiredField,
      AuthFieldProblem.emailInvalid => l10n.authEmailInvalid,
      AuthFieldProblem.handleInvalid => l10n.authHandleInvalid,
      AuthFieldProblem.handleTaken => l10n.errorConflictStaleWrite,
      AuthFieldProblem.emailTaken => l10n.errorConflictStaleWrite,
      AuthFieldProblem.passwordTooShort => l10n.authPasswordTooShort,
      AuthFieldProblem.passwordBreached => l10n.authPasswordTooShort,
      AuthFieldProblem.underMinimumAge => l10n.authUnderMinimumAge,
    };
  }
}

class _DateOfBirthField extends StatelessWidget {
  const _DateOfBirthField({
    required this.value,
    required this.onPressed,
    this.errorText,
  });

  final DateTime? value;
  final VoidCallback onPressed;
  final String? errorText;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    return InputDecorator(
      decoration: InputDecoration(
        labelText: l10n.authDateOfBirthLabel,
        errorText: errorText,
        border: const OutlineInputBorder(),
      ),
      child: Row(
        children: [
          Expanded(
            child: Text(
              // A date, formatted by the platform for the active locale rather
              // than hand-formatted. Hand-formatting is how an Arabic UI ends
              // up showing an American month/day order.
              value == null
                  ? l10n.authSelectDate
                  : MaterialLocalizations.of(context).formatFullDate(value!),
            ),
          ),
          IconButton(
            onPressed: onPressed,
            icon: const Icon(Icons.calendar_today_outlined),
            tooltip: l10n.authDateOfBirthLabel,
          ),
        ],
      ),
    );
  }
}

/// A general failure, above the form.
class _FailureBanner extends StatelessWidget {
  const _FailureBanner({required this.failure});

  final Failure failure;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final presentation =
        FailurePresenter.present(failure, L10n.of(context));

    return Container(
      padding: const EdgeInsets.all(Space.lg),
      decoration: BoxDecoration(
        color: theme.colorScheme.errorContainer,
        borderRadius: Radii.cardBorder,
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            presentation.title,
            style: theme.textTheme.titleMedium?.copyWith(
              color: theme.colorScheme.onErrorContainer,
            ),
          ),
          const SizedBox(height: Space.xs),
          Text(
            presentation.body,
            style: theme.textTheme.bodyMedium?.copyWith(
              color: theme.colorScheme.onErrorContainer,
            ),
          ),
        ],
      ),
    );
  }
}
