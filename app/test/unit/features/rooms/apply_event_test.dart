/// Tests for folding socket events into the room (FR-29–FR-35).
///
/// `applyEvent` is where the server's event log becomes what the user sees. It
/// is a pure function on purpose: this is the logic most likely to be subtly
/// wrong, and testing it through a widget would mean discovering that through
/// a screenshot.
library;

import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/realtime/room_socket.dart';
import 'package:vybe/features/rooms/domain/room.dart';
import 'package:vybe/features/rooms/presentation/rooms_controller.dart';

void main() {
  final now = DateTime.utc(2026, 8, 26, 12);

  Participant person(
    String id, {
    bool host = false,
    bool connected = true,
  }) =>
      Participant(
        userId: id,
        isHost: host,
        connected: connected,
        joinedAt: now,
      );

  Room roomWith({
    RoomState state = RoomState.lobby,
    String hostUserId = 'u1',
    int currentSeq = 1,
    List<Participant>? participants,
  }) =>
      Room(
        id: 'room-1',
        contentId: 'content-1',
        hostUserId: hostUserId,
        state: state,
        syncMode: 'COMPANION',
        visibility: 'private',
        maxParticipants: 4,
        currentSeq: currentSeq,
        createdAt: now,
        serverTime: now,
        participants: participants ?? [person('u1', host: true)],
      );

  RoomEnvelope event(
    String type, {
    int seq = 2,
    Map<String, dynamic> payload = const {},
  }) =>
      RoomEnvelope(
        id: 'evt-$seq',
        room: 'room-1',
        seq: seq,
        type: type,
        timestamp: now,
        payload: payload,
      );

  group('sequence', () {
    test('every event advances the position', () {
      // The position is how the client knows what to ask for on reconnect. An
      // event that did not advance it would make the client re-request work it
      // has already done, forever.
      final room = roomWith(currentSeq: 5);
      final updated = applyEvent(room, event('CHAT_MESSAGE', seq: 6));
      expect(updated.currentSeq, 6);
    });

    test('an unknown event type advances the position and nothing else', () {
      // FR-33: an unknown type means the server is newer than this client.
      // Ignoring it is REQUIRED — but the SEQUENCE must still advance, or the
      // client would resync from before it on every reconnect and be handed
      // the same unknown event again in a loop.
      final room = roomWith(state: RoomState.playing, currentSeq: 9);
      final updated = applyEvent(room, event('QUANTUM_ENTANGLEMENT', seq: 10));

      expect(updated.currentSeq, 10);
      expect(updated.state, RoomState.playing);
      expect(updated.hostUserId, room.hostUserId);
      expect(updated.participants, room.participants);
    });
  });

  group('ROOM_STATE_CHANGED', () {
    test('moves the room to the named state', () {
      final updated = applyEvent(
        roomWith(),
        event('ROOM_STATE_CHANGED', payload: {'from': 'LOBBY', 'to': 'READY'}),
      );
      expect(updated.state, RoomState.ready);
    });

    test('reads `to`, not `from`', () {
      // Trivially easy to get backwards, and the result is a UI that shows the
      // state the room just LEFT — permanently one step behind.
      final updated = applyEvent(
        roomWith(state: RoomState.ready),
        event(
          'ROOM_STATE_CHANGED',
          payload: {'from': 'READY', 'to': 'PLAYING'},
        ),
      );
      expect(updated.state, RoomState.playing);
    });

    test('falls back to lobby for a state this client does not know', () {
      // Lobby offers the fewest actions, so guessing wrong cannot drive the
      // room somewhere unexpected.
      final updated = applyEvent(
        roomWith(state: RoomState.playing),
        event('ROOM_STATE_CHANGED', payload: {'to': 'HYPERSPACE'}),
      );
      expect(updated.state, RoomState.lobby);
    });
  });

  group('ROOM_ENDED', () {
    test('marks the room terminal and records why', () {
      final updated = applyEvent(
        roomWith(state: RoomState.playing),
        event('ROOM_ENDED', payload: {'reason': 'host_ended'}),
      );
      expect(updated.state, RoomState.ended);
      expect(updated.endReason, 'host_ended');
      expect(updated.endedAt, now);
      expect(updated.state.isTerminal, isTrue);
    });

    test('leaves no action available afterwards', () {
      final updated = applyEvent(
        roomWith(state: RoomState.playing),
        event('ROOM_ENDED', payload: {'reason': 'reaper_abandoned'}),
      );
      expect(availableEvents(updated.state), isEmpty);
    });
  });

  group('HOST_CHANGED', () {
    test('moves the host flag as well as the id', () {
      // Both must move together. Updating only hostUserId would leave the
      // participant list showing a star beside somebody who is no longer host,
      // and the screen's isHost check disagreeing with what it renders.
      final room = roomWith(
        hostUserId: 'u1',
        participants: [person('u1', host: true), person('u2')],
      );

      final updated = applyEvent(
        room,
        event(
          'HOST_CHANGED',
          payload: {'previousHostId': 'u1', 'newHostId': 'u2'},
        ),
      );

      expect(updated.hostUserId, 'u2');
      expect(updated.isHost('u2'), isTrue);
      expect(updated.isHost('u1'), isFalse);

      final flags = {for (final p in updated.participants) p.userId: p.isHost};
      expect(flags, {'u1': false, 'u2': true});
    });

    test('does not disturb connection state', () {
      // A succession is not a reconnection. Resetting `connected` would make
      // everybody in a tunnel appear to come back online.
      final room = roomWith(
        participants: [person('u1', host: true), person('u2', connected: false)],
      );
      final updated = applyEvent(
        room,
        event('HOST_CHANGED', payload: {'newHostId': 'u2'}),
      );

      final u2 = updated.participants.firstWhere((p) => p.userId == 'u2');
      expect(u2.connected, isFalse);
      expect(u2.isHost, isTrue);
    });
  });

  group('PARTICIPANT_JOINED', () {
    test('adds somebody new', () {
      final updated = applyEvent(
        roomWith(),
        event('PARTICIPANT_JOINED', payload: {'userId': 'u2'}),
      );
      expect(updated.participants.map((p) => p.userId), ['u1', 'u2']);
    });

    test('does not duplicate somebody already present', () {
      // A rejoin after leaving re-emits JOINED for a user who may still be in
      // the local list — the LEFT event can arrive after the JOINED on a
      // reconnect, or be missed entirely. Appending blindly shows them twice,
      // and the seat count then reads wrong.
      final room = roomWith(
        participants: [person('u1', host: true), person('u2')],
      );
      final updated = applyEvent(
        room,
        event('PARTICIPANT_JOINED', payload: {'userId': 'u2'}),
      );

      expect(updated.participants.where((p) => p.userId == 'u2'), hasLength(1));
      expect(updated.participants, hasLength(2));
    });

    test('a joiner is never marked host', () {
      // Only HOST_CHANGED grants that. A joiner arriving as host would let
      // them see host controls they cannot use.
      final updated = applyEvent(
        roomWith(),
        event('PARTICIPANT_JOINED', payload: {'userId': 'u2'}),
      );
      final u2 = updated.participants.firstWhere((p) => p.userId == 'u2');
      expect(u2.isHost, isFalse);
    });
  });

  group('PARTICIPANT_LEFT', () {
    test('removes them', () {
      final room = roomWith(
        participants: [person('u1', host: true), person('u2')],
      );
      final updated = applyEvent(
        room,
        event('PARTICIPANT_LEFT', payload: {'userId': 'u2'}),
      );
      expect(updated.participants.map((p) => p.userId), ['u1']);
    });

    test('is harmless for somebody who is not there', () {
      // Happens routinely: a LEFT arrives in a delta for somebody the client
      // never saw join, because it resynced from after their JOINED.
      final room = roomWith(participants: [person('u1', host: true)]);
      final updated = applyEvent(
        room,
        event('PARTICIPANT_LEFT', payload: {'userId': 'nobody'}),
      );
      expect(updated.participants, hasLength(1));
    });
  });

  group('a full sequence', () {
    test('replays into the state the server would report', () {
      // The property that matters: applying the log in order reaches the same
      // answer the server has. If it did not, a reconnecting client would
      // diverge silently and the user would act on a room that does not exist.
      var room = roomWith(participants: [person('u1', host: true)]);

      final log = [
        event('PARTICIPANT_JOINED', seq: 2, payload: {'userId': 'u2'}),
        event('PARTICIPANT_JOINED', seq: 3, payload: {'userId': 'u3'}),
        event('ROOM_STATE_CHANGED', seq: 4, payload: {'to': 'READY'}),
        event('ROOM_STATE_CHANGED', seq: 5, payload: {'to': 'PLAYING'}),
        event('PARTICIPANT_LEFT', seq: 6, payload: {'userId': 'u3'}),
        event('PARTICIPANT_LEFT', seq: 7, payload: {'userId': 'u1'}),
        event('HOST_CHANGED', seq: 8, payload: {'newHostId': 'u2'}),
      ];

      for (final envelope in log) {
        room = applyEvent(room, envelope);
      }

      expect(room.currentSeq, 8);
      expect(room.state, RoomState.playing);
      expect(room.hostUserId, 'u2');
      expect(room.participants.map((p) => p.userId), ['u2']);
      expect(room.isHost('u2'), isTrue);
    });

    test('is idempotent per event, so a duplicate changes nothing', () {
      // The socket dedupes by envelope id, but applying the same event twice
      // must still be harmless — belt and braces on AC-12, since a delta that
      // overlaps a live delivery is ordinary rather than exceptional.
      final room = roomWith();
      final joined = event('PARTICIPANT_JOINED', seq: 2, payload: {'userId': 'u2'});

      final once = applyEvent(room, joined);
      final twice = applyEvent(once, joined);

      expect(twice.participants.map((p) => p.userId), ['u1', 'u2']);
      expect(twice.currentSeq, once.currentSeq);
    });
  });

  group('participantsFrom', () {
    test('decodes a snapshot array', () {
      final decoded = participantsFrom([
        {'userId': 'u1', 'isHost': true, 'joinedAt': now.toIso8601String()},
        {'userId': 'u2', 'isHost': false, 'connected': false},
      ]);

      expect(decoded, hasLength(2));
      expect(decoded[0].userId, 'u1');
      expect(decoded[0].isHost, isTrue);
      expect(decoded[1].connected, isFalse);
    });

    test('returns empty rather than throwing on a bad shape', () {
      // A snapshot arrives during a resync, which is already a recovery path.
      // Throwing there would turn a recoverable state into a crash.
      expect(participantsFrom(null), isEmpty);
      expect(participantsFrom('not a list'), isEmpty);
      expect(participantsFrom([1, 2, 3]), isEmpty);
    });

    test('defaults an absent `connected` to true', () {
      // Absent means "not stated". Defaulting to false would render everybody
      // as offline on a screen that has not opened a socket yet.
      final decoded = participantsFrom([
        {'userId': 'u1', 'isHost': false},
      ]);
      expect(decoded.single.connected, isTrue);
    });
  });
}
