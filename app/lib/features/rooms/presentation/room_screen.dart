/// The room screen (FR-14–FR-19, §3.2).
///
/// The one screen where the socket's state is visible. A room whose connection
/// has dropped must SAY so — a silently-stale participant list is worse than an
/// honest "reconnecting", because the user acts on what they can see.
library;

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/router.dart';
import '../../../core/error/failure.dart';
import '../../../core/realtime/room_socket.dart';
import '../../../core/ui/states/state_views.dart';
import '../../../core/ui/tokens.dart';
import '../../../l10n/generated/app_localizations.dart';
import '../domain/room.dart';
import 'rooms_controller.dart';

class RoomScreen extends ConsumerWidget {
  const RoomScreen({super.key, required this.roomId});

  final String roomId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = L10n.of(context);
    final detail = ref.watch(roomDetailProvider(roomId));

    return Scaffold(
      appBar: AppBar(
        title: Text(detail.value?.room.title ?? l10n.roomsTitle),
        actions: [
          if (detail.hasValue)
            _ConnectionIndicator(status: detail.value!.connection),
        ],
      ),
      body: SafeArea(
        child: switch (detail) {
          AsyncLoading() => const LoadingStateView(itemCount: 3),
          AsyncError(:final error) => _error(context, ref, error),
          AsyncData(:final value) => _RoomBody(roomId: roomId, state: value),
        },
      ),
    );
  }

  Widget _error(BuildContext context, WidgetRef ref, Object error) {
    final failure = error is Failure ? error : UnexpectedFailure(error);
    void retry() => ref.invalidate(roomDetailProvider(roomId));

    return switch (failure) {
      NetworkFailure(isOffline: true) => OfflineStateView(onRetry: retry),
      AuthFailure() =>
        UnauthorisedStateView(onSignIn: () => context.go(Routes.signIn)),
      // A 404 here means the room is gone OR the caller is not a member — the
      // server deliberately does not say which. "Not found" is the honest
      // rendering of both.
      ServerFailure(code: 'ROOM_NOT_FOUND') =>
        NotFoundStateView(onGoHome: () => context.go(Routes.rooms)),
      _ => ErrorStateView(failure: failure, onRetry: retry),
    };
  }
}

class _RoomBody extends ConsumerWidget {
  const _RoomBody({required this.roomId, required this.state});

  final String roomId;
  final RoomDetailState state;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = L10n.of(context);
    final room = state.room;
    final me = ref.watch(sessionProvider).value?.userId ?? '';
    final isHost = room.isHost(me);

    if (room.state.isTerminal) {
      // A terminal room has nothing to act on. Rendering the controls disabled
      // would invite a user to keep tapping something that will never work.
      return EmptyStateView(
        title: l10n.roomEndedTitle,
        body: l10n.roomEndedBody,
        actionLabel: l10n.actionGoHome,
        onAction: () => context.go(Routes.rooms),
      );
    }

    return ListView(
      padding: Space.screen,
      children: [
        if (state.actionFailure != null) ...[
          _ActionFailureBanner(failure: state.actionFailure!),
          const SizedBox(height: Space.lg),
        ],
        _JoinCodeCard(room: room),
        const SizedBox(height: Space.lg),
        _ParticipantsSection(room: room, currentUserId: me),
        const SizedBox(height: Space.xl),
        if (isHost)
          _HostControls(roomId: roomId, state: state)
        else
          _GuestControls(roomId: roomId, state: state),
      ],
    );
  }
}

/// The shareable code (FR-13).
class _JoinCodeCard extends StatelessWidget {
  const _JoinCodeCard({required this.room});

  final Room room;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);
    final code = room.joinCode;

    // Absent for a non-member. The card disappears rather than rendering an
    // empty box, because there is genuinely nothing to show.
    if (code == null || code.isEmpty) return const SizedBox.shrink();

    return Card(
      margin: EdgeInsets.zero,
      child: Padding(
        padding: const EdgeInsets.all(Space.lg),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(l10n.roomCodeLabel, style: theme.textTheme.labelMedium),
            const SizedBox(height: Space.sm),
            Row(
              children: [
                Expanded(
                  child: Text(
                    JoinCode.format(code),
                    // LTR and monospace-spaced: the code is read aloud
                    // character by character, and RTL would reverse it.
                    textDirection: TextDirection.ltr,
                    style: theme.textTheme.headlineSmall
                        ?.copyWith(letterSpacing: Space.xs),
                  ),
                ),
                IconButton(
                  tooltip: l10n.roomCopyCode,
                  icon: const Icon(Icons.copy_outlined),
                  onPressed: () async {
                    // The share URL, not the bare code: a link works for
                    // somebody who does not have the app yet, and a code does
                    // not.
                    await Clipboard.setData(
                      ClipboardData(text: room.shareUrl ?? code),
                    );
                    if (!context.mounted) return;
                    ScaffoldMessenger.of(context).showSnackBar(
                      SnackBar(content: Text(l10n.roomCodeCopied)),
                    );
                  },
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}

class _ParticipantsSection extends StatelessWidget {
  const _ParticipantsSection({required this.room, required this.currentUserId});

  final Room room;
  final String currentUserId;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          l10n.roomSeatsLabel(room.participants.length, room.maxParticipants),
          style: theme.textTheme.labelMedium,
        ),
        const SizedBox(height: Space.sm),
        for (final participant in room.participants)
          ListTile(
            contentPadding: EdgeInsets.zero,
            minVerticalPadding: Space.sm,
            leading: CircleAvatar(
              // Dimmed rather than hidden when disconnected. Somebody in a
              // tunnel is still in the room, and removing them from the list
              // would make the seat count look wrong.
              backgroundColor: participant.connected
                  ? theme.colorScheme.primaryContainer
                  : theme.colorScheme.surfaceContainerHighest,
              child: Icon(
                participant.isHost ? Icons.star : Icons.person_outline,
                size: TypeScale.body,
              ),
            ),
            title: Text(
              participant.userId == currentUserId
                  ? l10n.roomYouLabel
                  : participant.userId,
            ),
            subtitle: participant.isHost ? Text(l10n.roomHostLabel) : null,
            trailing: participant.connected
                ? null
                : Icon(
                    Icons.cloud_off_outlined,
                    size: TypeScale.body,
                    color: theme.colorScheme.outline,
                  ),
          ),
      ],
    );
  }
}

/// Host-only actions (FR-15, FR-19).
class _HostControls extends ConsumerWidget {
  const _HostControls({required this.roomId, required this.state});

  final String roomId;
  final RoomDetailState state;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = L10n.of(context);
    final controller = ref.read(roomDetailProvider(roomId).notifier);
    final available = availableEvents(state.room.state);
    final busy = state.acting;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        // Only the transitions the server would actually accept are rendered.
        // A button that is guaranteed to return 409 is worse than no button.
        if (available.contains(RoomEvent.arm))
          FilledButton(
            onPressed: busy ? null : () => controller.transition(RoomEvent.arm),
            child: Text(l10n.roomArmAction),
          ),
        if (available.contains(RoomEvent.start))
          FilledButton(
            onPressed:
                busy ? null : () => controller.transition(RoomEvent.start),
            child: Text(l10n.roomStartAction),
          ),
        if (available.contains(RoomEvent.reanchor))
          OutlinedButton(
            onPressed:
                busy ? null : () => controller.transition(RoomEvent.reanchor),
            child: Text(l10n.roomReanchorAction),
          ),
        if (available.contains(RoomEvent.cancel)) ...[
          const SizedBox(height: Space.sm),
          OutlinedButton(
            onPressed:
                busy ? null : () => controller.transition(RoomEvent.cancel),
            child: Text(l10n.roomCancelAction),
          ),
        ],
        const SizedBox(height: Space.xl),
        TextButton(
          onPressed: busy ? null : () => _confirmEnd(context, ref),
          style: TextButton.styleFrom(
            foregroundColor: Theme.of(context).colorScheme.error,
          ),
          child: Text(l10n.roomEndAction),
        ),
      ],
    );
  }

  /// Ending a room disconnects everybody and cannot be undone, so it is
  /// confirmed. Destructive-and-irreversible is the bar for a dialog; anything
  /// less makes users dismiss dialogs reflexively.
  Future<void> _confirmEnd(BuildContext context, WidgetRef ref) async {
    final l10n = L10n.of(context);

    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: Text(l10n.roomConfirmEndTitle),
        content: Text(l10n.roomConfirmEndBody),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: Text(l10n.commonCancel),
          ),
          FilledButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: Text(l10n.commonConfirm),
          ),
        ],
      ),
    );

    if (confirmed != true || !context.mounted) return;

    final ok = await ref.read(roomDetailProvider(roomId).notifier).end();
    if (ok && context.mounted) {
      ref.invalidate(roomsListProvider);
      context.go(Routes.rooms);
    }
  }
}

/// What a non-host can do.
class _GuestControls extends ConsumerWidget {
  const _GuestControls({required this.roomId, required this.state});

  final String roomId;
  final RoomDetailState state;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = L10n.of(context);

    return OutlinedButton(
      onPressed: state.acting
          ? null
          : () async {
              final ok =
                  await ref.read(roomDetailProvider(roomId).notifier).leave();
              if (ok && context.mounted) {
                ref.invalidate(roomsListProvider);
                context.go(Routes.rooms);
              }
            },
      child: Text(l10n.roomLeaveAction),
    );
  }
}

/// Shows the socket's health (FR-38).
///
/// Visible rather than hidden, because a stale participant list looks exactly
/// like an accurate one. The user needs to know which they are looking at.
class _ConnectionIndicator extends StatelessWidget {
  const _ConnectionIndicator({required this.status});

  final SocketStatus status;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    final (label, colour, showSpinner) = switch (status) {
      SocketStatus.connected => (l10n.roomLive, theme.colorScheme.primary, false),
      SocketStatus.connecting =>
        (l10n.roomConnecting, theme.colorScheme.outline, true),
      SocketStatus.reconnecting =>
        (l10n.roomReconnecting, theme.colorScheme.error, true),
      SocketStatus.idle || SocketStatus.closed =>
        (l10n.errorOffline, theme.colorScheme.outline, false),
    };

    return Padding(
      padding: const EdgeInsetsDirectional.only(end: Space.lg),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          if (showSpinner)
            SizedBox(
              height: Space.md,
              width: Space.md,
              child: CircularProgressIndicator(strokeWidth: 2, color: colour),
            )
          else
            Icon(Icons.circle, size: Space.sm, color: colour),
          const SizedBox(width: Space.sm),
          Text(label, style: theme.textTheme.labelSmall?.copyWith(color: colour)),
        ],
      ),
    );
  }
}

class _ActionFailureBanner extends StatelessWidget {
  const _ActionFailureBanner({required this.failure});

  final Failure failure;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    // NOT_THE_HOST gets its own message rather than the generic forbidden
    // copy, because it is a race the user did not cause: the host changed
    // between the screen rendering and the tap landing.
    final message = switch (failure) {
      ServerFailure(code: 'NOT_THE_HOST') => l10n.roomOnlyHostCanDo,
      ServerFailure(code: 'ROOM_ENDED') => l10n.roomEndedBody,
      ServerFailure(code: 'ILLEGAL_TRANSITION') => l10n.errorConflictInvalidState,
      _ => l10n.errorServerBody,
    };

    return Container(
      padding: const EdgeInsets.all(Space.md),
      decoration: BoxDecoration(
        color: theme.colorScheme.errorContainer,
        borderRadius: Radii.cardBorder,
      ),
      child: Text(
        message,
        style: theme.textTheme.bodyMedium
            ?.copyWith(color: theme.colorScheme.onErrorContainer),
      ),
    );
  }
}
