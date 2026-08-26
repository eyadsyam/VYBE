/// Rooms list and room-detail state (§4.5).
library;

import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../app/providers.dart';
import '../../../core/error/failure.dart';
import '../../../core/error/result.dart';
import '../../../core/realtime/room_socket.dart';
import '../domain/room.dart';

/// Rethrows a [Failure] into Riverpod's error channel.
///
/// Riverpod models failure as a thrown object, while the data layer models it
/// as a value. This is the one place the two conventions meet, and it is a
/// function rather than a scattered `throw` so the translation happens
/// identically everywhere.
Never _raise(Failure failure) =>
    Error.throwWithStackTrace(failure, StackTrace.current);

/// Unwraps a [Result] into Riverpod's convention.
T _require<T>(Result<T> result) => result.fold(ok: (v) => v, err: _raise);

// ---------------------------------------------------------------------------
// Rooms list
// ---------------------------------------------------------------------------

/// The paginated rooms list (FR-20).
class RoomsListState {
  const RoomsListState({
    this.rooms = const [],
    this.nextCursor,
    this.loadingMore = false,
  });

  final List<Room> rooms;
  final String? nextCursor;
  final bool loadingMore;

  bool get hasMore => nextCursor != null && nextCursor!.isNotEmpty;
}

class RoomsListController extends AsyncNotifier<RoomsListState> {
  @override
  Future<RoomsListState> build() async {
    final page = _require(await ref.watch(roomsApiProvider).list());
    return RoomsListState(rooms: page.rooms, nextCursor: page.nextCursor);
  }

  /// Fetches the next page and APPENDS it.
  ///
  /// Appending rather than replacing is the whole point of cursor pagination:
  /// the user keeps what they were looking at, and the list does not jump under
  /// their thumb.
  Future<void> loadMore() async {
    final current = state.value;
    if (current == null || !current.hasMore || current.loadingMore) return;

    state = AsyncData(
      RoomsListState(
        rooms: current.rooms,
        nextCursor: current.nextCursor,
        loadingMore: true,
      ),
    );

    final result =
        await ref.read(roomsApiProvider).list(cursor: current.nextCursor);

    state = AsyncData(
      result.fold(
        ok: (page) => RoomsListState(
          rooms: [...current.rooms, ...page.rooms],
          nextCursor: page.nextCursor,
        ),
        // A failed page-load KEEPS what is already on screen. Replacing the
        // list with an error state would throw away rows the user is reading
        // because the next page failed — a far worse outcome than a silently
        // absent "load more".
        err: (_) => RoomsListState(
          rooms: current.rooms,
          nextCursor: current.nextCursor,
        ),
      ),
    );
  }

  Future<void> refresh() async {
    ref.invalidateSelf();
    await future;
  }
}

final roomsListProvider =
    AsyncNotifierProvider<RoomsListController, RoomsListState>(
  RoomsListController.new,
);

// ---------------------------------------------------------------------------
// Room detail
// ---------------------------------------------------------------------------

/// One room, kept live by a socket.
class RoomDetailState {
  const RoomDetailState({
    required this.room,
    required this.connection,
    this.acting = false,
    this.actionFailure,
  });

  final Room room;
  final SocketStatus connection;

  /// True while a host action is in flight, so the buttons can be disabled.
  ///
  /// Without it, a double-tap on "start playback" sends two transitions and the
  /// second gets a 409 the user did nothing to deserve.
  final bool acting;

  final Failure? actionFailure;

  RoomDetailState copyWith({
    Room? room,
    SocketStatus? connection,
    bool? acting,
    Failure? actionFailure,
    bool clearFailure = false,
  }) =>
      RoomDetailState(
        room: room ?? this.room,
        connection: connection ?? this.connection,
        acting: acting ?? this.acting,
        actionFailure:
            clearFailure ? null : (actionFailure ?? this.actionFailure),
      );
}

/// Drives one room screen.
///
/// It fetches the room over HTTP FIRST, then opens a socket seeded with that
/// room's `currentSeq`. The ordering matters: the socket has no history of its
/// own on a cold start, so without the fetch it would resync from zero and be
/// handed a full snapshot of a room it already has.
class RoomDetailController extends AsyncNotifier<RoomDetailState> {
  RoomDetailController(this.roomId);

  /// The family argument. Riverpod 3 passes it to the constructor rather than
  /// to `build`.
  final String roomId;

  @override
  Future<RoomDetailState> build() async {
    final room = _require(await ref.watch(roomsApiProvider).get(roomId));

    final socket = RoomSocket(
      socketBaseUrl: ref.watch(apiEndpointProvider).socketUrl,
      roomId: roomId,
      ticketProvider: () async {
        final ticket = await ref.read(authApiProvider).webSocketTicket();
        return ticket.fold(ok: (t) => t, err: (_) => null);
      },
    );

    final subscription = socket.messages.listen(_onSocketMessage);
    ref.onDispose(() {
      subscription.cancel();
      socket.dispose();
    });

    // Seeded with the room's position, so a reconnect asks only for what it
    // actually missed.
    await socket.connect(fromSeq: room.currentSeq);

    return RoomDetailState(room: room, connection: socket.status);
  }

  void _onSocketMessage(SocketMessage message) {
    final current = state.value;
    if (current == null) return;

    switch (message) {
      case SocketStatusChanged(:final status):
        state = AsyncData(current.copyWith(connection: status));

      case SocketEvent(:final envelope):
        state = AsyncData(
          current.copyWith(room: applyEvent(current.room, envelope)),
        );

      case SocketSnapshot(state: final snapshot, :final currentSeq):
        // A snapshot REPLACES everything. Merging it with what we had would
        // defeat its purpose: the server sent it precisely because our
        // position could not be reconciled incrementally.
        state = AsyncData(
          current.copyWith(
            room: current.room.copyWith(
              state: RoomState.fromWire(snapshot['state'] as String?),
              hostUserId: snapshot['hostUserId'] as String?,
              currentSeq: currentSeq,
              participants: participantsFrom(snapshot['participants']),
            ),
          ),
        );

      case SocketClockSample():
        // Consumed by the sync clock, not by this screen. Rebuilding the room
        // UI on every sample would repaint it three times a minute for no
        // visible change.
        break;
    }
  }

  /// Runs a host transition (FR-15).
  Future<bool> transition(RoomEvent event) =>
      _act(() => ref.read(roomsApiProvider).transition(roomId, event));

  /// Ends the room (FR-19).
  Future<bool> end() => _act(() => ref.read(roomsApiProvider).end(roomId));

  /// Leaves the room (FR-17).
  Future<bool> leave() => _act(() => ref.read(roomsApiProvider).leave(roomId));

  Future<bool> _act(Future<Result<Room>> Function() call) async {
    final current = state.value;
    if (current == null || current.acting) return false;

    state = AsyncData(current.copyWith(acting: true, clearFailure: true));
    final result = await call();

    return result.fold(
      ok: (room) {
        // The HTTP response is authoritative and arrives before the socket
        // event, so applying it here makes the button's effect visible
        // immediately rather than after a round trip the user cannot see.
        state = AsyncData(
          current.copyWith(room: room, acting: false, clearFailure: true),
        );
        return true;
      },
      err: (failure) {
        state = AsyncData(
          current.copyWith(acting: false, actionFailure: failure),
        );
        return false;
      },
    );
  }
}

/// Folds one event into the room.
///
/// Public so it can be tested without a provider container. Only the events
/// that change what the screen renders are handled; everything else advances
/// `currentSeq` and nothing more — which is correct rather than lazy, because
/// FR-33 requires an unknown event to be ignored, and an event this screen does
/// not display is exactly that case.
Room applyEvent(Room room, RoomEnvelope envelope) {
  final updated = room.copyWith(currentSeq: envelope.seq);

  return switch (envelope.type) {
    'ROOM_STATE_CHANGED' => updated.copyWith(
        state: RoomState.fromWire(envelope.payload['to'] as String?),
      ),
    'ROOM_ENDED' => updated.copyWith(
        state: RoomState.ended,
        endedAt: envelope.timestamp,
        endReason: envelope.payload['reason'] as String?,
      ),
    'HOST_CHANGED' => updated.copyWith(
        hostUserId: envelope.payload['newHostId'] as String?,
        participants: [
          for (final p in updated.participants)
            Participant(
              userId: p.userId,
              isHost: p.userId == envelope.payload['newHostId'],
              connected: p.connected,
              joinedAt: p.joinedAt,
            ),
        ],
      ),
    // The `where` filter is not redundant: a rejoin after leaving re-emits
    // JOINED for somebody already in the list, and appending blindly would
    // show them twice.
    'PARTICIPANT_JOINED' => updated.copyWith(
        participants: [
          ...updated.participants
              .where((p) => p.userId != envelope.payload['userId']),
          Participant(
            userId: envelope.payload['userId'] as String? ?? '',
            isHost: false,
            connected: true,
            joinedAt: envelope.timestamp,
          ),
        ],
      ),
    'PARTICIPANT_LEFT' => updated.copyWith(
        participants: updated.participants
            .where((p) => p.userId != envelope.payload['userId'])
            .toList(),
      ),
    _ => updated,
  };
}

/// Decodes a snapshot's participant array.
List<Participant> participantsFrom(Object? raw) {
  if (raw is! List) return const [];
  return raw.whereType<Map>().map((p) {
    return Participant(
      userId: p['userId'] as String? ?? '',
      isHost: p['isHost'] as bool? ?? false,
      connected: p['connected'] as bool? ?? true,
      joinedAt: DateTime.tryParse(p['joinedAt'] as String? ?? '')?.toUtc() ??
          DateTime.now().toUtc(),
    );
  }).toList();
}

final roomDetailProvider = AsyncNotifierProvider.family<RoomDetailController,
    RoomDetailState, String>(RoomDetailController.new);
