/// Join by code (FR-13).
///
/// The field validates on every keystroke against the same parser the server
/// uses, so a user typing a correct code is never told it is wrong — which is
/// the entire failure mode Crockford's lenient decoding exists to prevent.
library;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/router.dart';
import '../../../core/error/failure.dart';
import '../../../core/error/failure_presenter.dart';
import '../../../core/ui/tokens.dart';
import '../../../l10n/generated/app_localizations.dart';
import '../domain/room.dart';
import 'rooms_controller.dart';

class JoinRoomScreen extends ConsumerStatefulWidget {
  const JoinRoomScreen({super.key, this.initialCode});

  /// A code from a share link, already normalised by the router.
  final String? initialCode;

  @override
  ConsumerState<JoinRoomScreen> createState() => _JoinRoomScreenState();
}

class _JoinRoomScreenState extends ConsumerState<JoinRoomScreen> {
  late final TextEditingController _code =
      TextEditingController(text: widget.initialCode ?? '');

  bool _submitting = false;
  Failure? _failure;

  /// One key for the whole screen's lifetime, NOT one per attempt.
  ///
  /// This is the client half of FR-57. If the first attempt times out and the
  /// user taps join again, the same key reaches the server and it replays the
  /// original response instead of processing a second join. A key regenerated
  /// per attempt is not an idempotency key at all.
  late final String _idempotencyKey =
      ref.read(roomsApiProvider).newIdempotencyKey();

  @override
  void dispose() {
    _code.dispose();
    super.dispose();
  }

  Future<void> _join() async {
    final parsed = JoinCode.parse(_code.text);
    if (parsed == null) return;

    setState(() {
      _submitting = true;
      _failure = null;
    });

    final result = await ref.read(roomsApiProvider).join(
          joinCode: parsed,
          idempotencyKey: _idempotencyKey,
        );

    if (!mounted) return;

    result.fold(
      ok: (room) {
        // The list is now stale — the user is in a room that was not there
        // before. Invalidating rather than mutating means the next visit shows
        // the truth rather than an optimistic guess.
        ref.invalidate(roomsListProvider);
        context.go(Routes.roomPath(room.id));
      },
      err: (failure) => setState(() {
        _submitting = false;
        _failure = failure;
      }),
    );
  }

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);
    final complete = JoinCode.isComplete(_code.text);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.joinTitle)),
      body: SafeArea(
        child: SingleChildScrollView(
          padding: Space.screen,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              TextField(
                controller: _code,
                autofocus: widget.initialCode == null,
                autocorrect: false,
                enableSuggestions: false,
                textCapitalization: TextCapitalization.characters,
                // Always LTR. A join code is Latin-and-digits even in an
                // Arabic UI, and rendering it RTL would show it reversed —
                // which somebody would then read aloud, backwards.
                textDirection: TextDirection.ltr,
                textAlign: TextAlign.center,
                style: theme.textTheme.headlineMedium?.copyWith(
                  letterSpacing: Space.sm,
                ),
                inputFormatters: [
                  // Seven, not six: the separator in "K7X-2QP" is stripped by
                  // the parser but occupies a character while typing.
                  LengthLimitingTextInputFormatter(JoinCode.length + 1),
                ],
                onChanged: (_) => setState(() => _failure = null),
                onSubmitted: (_) => complete && !_submitting ? _join() : null,
                decoration: InputDecoration(
                  labelText: l10n.joinCodeLabel,
                  helperText: l10n.joinCodeHint,
                  errorText: _errorText(l10n, complete),
                ),
              ),
              const SizedBox(height: Space.xl),
              FilledButton(
                onPressed: complete && !_submitting ? _join : null,
                child: _submitting
                    ? const SizedBox(
                        height: Space.lg,
                        width: Space.lg,
                        child: CircularProgressIndicator(strokeWidth: 2),
                      )
                    : Text(l10n.joinAction),
              ),
            ],
          ),
        ),
      ),
    );
  }

  /// The field's error, if any.
  ///
  /// An incomplete code is NOT an error while the user is still typing —
  /// showing "a join code is six characters" after the first keystroke is
  /// scolding somebody for not having finished.
  String? _errorText(L10n l10n, bool complete) {
    final failure = _failure;
    if (failure != null) {
      return switch (failure) {
        ServerFailure(code: 'ROOM_NOT_FOUND') => l10n.joinNotFound,
        ServerFailure(code: 'ALREADY_JOINED') => l10n.joinAlreadyIn,
        ServerFailure(code: 'ROOM_FULL') => l10n.roomFullTitle,
        ServerFailure(code: 'ROOM_ENDED') => l10n.roomEndedTitle,
        _ => FailurePresenter.present(failure, l10n).body,
      };
    }
    if (_code.text.isNotEmpty && !complete && _code.text.length > JoinCode.length) {
      return l10n.joinCodeIncomplete;
    }
    return null;
  }
}
