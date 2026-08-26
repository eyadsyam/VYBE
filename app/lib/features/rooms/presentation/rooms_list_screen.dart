/// The rooms list (FR-20, §3.2).
///
/// Every §3.2 state is reachable here: loading, empty, error, offline. That is
/// the point of the state widgets in core/ui — a screen that only handles
/// "loaded" is a screen that shows a blank rectangle on a bad network.
library;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/router.dart';
import '../../../core/error/failure.dart';
import '../../../core/ui/states/state_views.dart';
import '../../../core/ui/tokens.dart';
import '../../../l10n/generated/app_localizations.dart';
import '../../auth/presentation/auth_controller.dart';
import '../domain/room.dart';
import 'rooms_controller.dart';

class RoomsListScreen extends ConsumerWidget {
  const RoomsListScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = L10n.of(context);
    final rooms = ref.watch(roomsListProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.roomsTitle),
        actions: [
          IconButton(
            onPressed: () => ref.read(authControllerProvider.notifier).signOut(),
            icon: const Icon(Icons.logout),
            tooltip: l10n.roomsSignOutAction,
          ),
        ],
      ),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: () => context.go(Routes.join),
        icon: const Icon(Icons.meeting_room_outlined),
        label: Text(l10n.roomsJoinAction),
      ),
      body: SafeArea(
        child: switch (rooms) {
          AsyncLoading() => const LoadingStateView(),
          AsyncError(:final error) => _errorView(context, ref, error),
          AsyncData(:final value) when value.rooms.isEmpty => EmptyStateView(
              title: l10n.roomsEmptyTitle,
              body: l10n.roomsEmptyBody,
              actionLabel: l10n.roomsJoinAction,
              onAction: () => context.go(Routes.join),
            ),
          AsyncData(:final value) => _RoomsList(state: value),
        },
      ),
    );
  }

  Widget _errorView(BuildContext context, WidgetRef ref, Object error) {
    final retry = ref.read(roomsListProvider.notifier).refresh;

    // A Failure gets the full §3.2 treatment; anything else is a bug rather
    // than a condition, and UnexpectedFailure is how the presenter says so.
    final failure = error is Failure ? error : UnexpectedFailure(error);

    return switch (failure) {
      NetworkFailure(isOffline: true) => OfflineStateView(onRetry: retry),
      AuthFailure() => UnauthorisedStateView(
          onSignIn: () => context.go(Routes.signIn),
        ),
      RateLimitFailure(:final retryAfter) => RateLimitedStateView(
          retryAfter: retryAfter,
          onRetry: retry,
        ),
      _ => ErrorStateView(failure: failure, onRetry: retry),
    };
  }
}

class _RoomsList extends ConsumerWidget {
  const _RoomsList({required this.state});

  final RoomsListState state;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = L10n.of(context);

    return RefreshIndicator(
      onRefresh: ref.read(roomsListProvider.notifier).refresh,
      child: ListView.separated(
        padding: Space.screen,
        // One extra row for the "load more" control, when there is a next page.
        itemCount: state.rooms.length + (state.hasMore ? 1 : 0),
        separatorBuilder: (_, _) => const SizedBox(height: Space.sm),
        itemBuilder: (context, index) {
          if (index >= state.rooms.length) {
            return Padding(
              padding: const EdgeInsets.symmetric(vertical: Space.lg),
              child: Center(
                child: state.loadingMore
                    ? const CircularProgressIndicator()
                    : TextButton(
                        onPressed:
                            ref.read(roomsListProvider.notifier).loadMore,
                        child: Text(l10n.roomsLoadMore),
                      ),
              ),
            );
          }
          return _RoomCard(room: state.rooms[index]);
        },
      ),
    );
  }
}

class _RoomCard extends StatelessWidget {
  const _RoomCard({required this.room});

  final Room room;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    final label = room.title?.isNotEmpty == true
        // A room without a name falls back to its code rather than an empty
        // row. The code is what the user recognises anyway.
        ? room.title!
        : JoinCode.format(room.joinCode ?? '');
    final seats =
        l10n.roomSeatsLabel(room.participants.length, room.maxParticipants);

    return Card(
      margin: EdgeInsets.zero,
      child: InkWell(
        onTap: room.state.isTerminal
            ? null
            : () => context.go(Routes.roomPath(room.id)),
        borderRadius: Radii.cardBorder,
        // MergeSemantics so a screen reader announces the row once, rather
        // than reading the title, the seat count, and the state as three
        // unrelated items.
        child: MergeSemantics(
          child: Padding(
            padding: const EdgeInsets.all(Space.lg),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(label, style: theme.textTheme.titleMedium),
                const SizedBox(height: Space.xs),
                // A Wrap, NOT a Row.
                //
                // At 200% text scale the seat count and the state chip do not
                // fit side by side in 360dp, and a Row overflows — which is
                // exactly what NFR-16 forbids and what the golden matrix
                // caught. Wrap reflows them onto a second line instead, and
                // does it correctly in RTL without a manual mirror.
                Wrap(
                  spacing: Space.sm,
                  runSpacing: Space.xs,
                  crossAxisAlignment: WrapCrossAlignment.center,
                  children: [
                    Text(seats, style: theme.textTheme.bodySmall),
                    _StateChip(state: room.state),
                  ],
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}

class _StateChip extends StatelessWidget {
  const _StateChip({required this.state});

  final RoomState state;

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    final (label, colour) = switch (state) {
      RoomState.lobby => (l10n.roomStateLobby, theme.colorScheme.secondary),
      RoomState.ready => (l10n.roomStateReady, theme.colorScheme.tertiary),
      RoomState.playing => (l10n.roomStatePlaying, theme.colorScheme.primary),
      RoomState.ended => (l10n.roomStateEnded, theme.colorScheme.outline),
    };

    return Container(
      padding: const EdgeInsets.symmetric(
        horizontal: Space.sm,
        vertical: Space.xs,
      ),
      decoration: BoxDecoration(
        color: colour.withValues(alpha: 0.12),
        borderRadius: const BorderRadius.all(Radii.pill),
      ),
      child: Text(
        label,
        style: theme.textTheme.labelSmall?.copyWith(color: colour),
      ),
    );
  }
}
