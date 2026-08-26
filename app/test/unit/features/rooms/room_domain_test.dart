import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/features/rooms/domain/room.dart';

void main() {
  group('JoinCode.parse', () {
    test('accepts a canonical code unchanged', () {
      expect(JoinCode.parse('K7X2QP'), 'K7X2QP');
    });

    test('accepts what users actually type', () {
      // Crockford's decoding rules exist for exactly this moment: somebody is
      // typing what they heard read aloud over a call.
      const cases = <String, String>{
        'k7x2qp': 'K7X2QP',
        'K7x2Qp': 'K7X2QP',
        '  K7X2QP  ': 'K7X2QP',
        'K7X-2QP': 'K7X2QP',
        'K7X 2QP': 'K7X2QP',
        'K7X_2QP': 'K7X2QP',
        '-K7X2QP-': 'K7X2QP',
      };
      cases.forEach((input, expected) {
        expect(JoinCode.parse(input), expected, reason: 'input was "$input"');
      });
    });

    test('maps the letters that are misread as digits', () {
      // I and L look like 1; O looks like 0. Mapping them is what makes a
      // transcribed code work.
      expect(JoinCode.parse('I7X2QP'), '17X2QP');
      expect(JoinCode.parse('l7X2QP'), '17X2QP');
      expect(JoinCode.parse('L7X2QP'), '17X2QP');
      expect(JoinCode.parse('O7X2QP'), '07X2QP');
      expect(JoinCode.parse('o7X2QP'), '07X2QP');

      // Three different substitutions in one code.
      expect(JoinCode.parse('IOl-2QP'), '1012QP');
    });

    test('refuses U rather than mapping it', () {
      // U is excluded from the alphabet, and unlike I/L/O there is no digit it
      // is plausibly a misreading of. Silently mapping it would resolve to a
      // DIFFERENT room, which is worse than refusing.
      expect(JoinCode.parse('U7X2QP'), isNull);
      expect(JoinCode.parse('u7X2QP'), isNull);
      expect(JoinCode.parse('K7X2QU'), isNull);
    });

    test('refuses malformed input', () {
      const bad = [
        '',
        '   ',
        'K7X2Q', // too short
        'K7X2QPX', // too long
        'K7X2Q!', // punctuation
        'K7X2Qم', // non-ASCII
        '------', // separators only
        'K7X2Q🎬', // emoji
      ];
      for (final input in bad) {
        expect(JoinCode.parse(input), isNull, reason: 'input was "$input"');
      }
    });

    test('agrees with the server on the excluded alphabet', () {
      // The client and server alphabets must match exactly, or a code the
      // server generated is rejected here — a user typing a correct code and
      // being told it is wrong.
      expect(JoinCode.alphabet, '0123456789ABCDEFGHJKMNPQRSTVWXYZ');
      expect(JoinCode.alphabet.length, 32);
      for (final excluded in ['I', 'L', 'O', 'U']) {
        expect(
          JoinCode.alphabet.contains(excluded),
          isFalse,
          reason: '$excluded must not be in the alphabet',
        );
      }
    });

    test('round-trips every character the generator can produce', () {
      // Anything the server can generate must survive a parse unchanged, or a
      // user reading back a correct code would be refused.
      for (var i = 0; i < JoinCode.alphabet.length; i++) {
        final code = (JoinCode.alphabet[i] * 6);
        expect(JoinCode.parse(code), code, reason: 'code was "$code"');
      }
    });

    test('formats for reading aloud', () {
      expect(JoinCode.format('K7X2QP'), 'K7X-2QP');
      // A partial code is returned untouched rather than mangled.
      expect(JoinCode.format('K7X'), 'K7X');
    });

    test('isComplete tracks parse', () {
      expect(JoinCode.isComplete('k7x-2qp'), isTrue);
      expect(JoinCode.isComplete('k7x'), isFalse);
    });
  });

  group('RoomState', () {
    test('decodes the wire vocabulary', () {
      expect(RoomState.fromWire('LOBBY'), RoomState.lobby);
      expect(RoomState.fromWire('READY'), RoomState.ready);
      expect(RoomState.fromWire('PLAYING'), RoomState.playing);
      expect(RoomState.fromWire('ENDED'), RoomState.ended);
    });

    test('falls back to lobby for an unknown state', () {
      // An unrecognised state means this client is older than the server.
      // Lobby offers the fewest actions, so guessing wrong cannot drive the
      // room somewhere unexpected.
      expect(RoomState.fromWire('TIME_TRAVELLING'), RoomState.lobby);
      expect(RoomState.fromWire(null), RoomState.lobby);
      expect(availableEvents(RoomState.lobby), isNot(contains(RoomEvent.start)));
    });

    test('round-trips through the wire form', () {
      for (final state in RoomState.values) {
        expect(RoomState.fromWire(state.wire), state);
      }
    });
  });

  group('availableEvents', () {
    test('offers only what the server would accept', () {
      // The server is the authority and refuses an illegal transition with a
      // 409. This exists so the UI does not render a button guaranteed to
      // fail, which is worse than not rendering it.
      expect(availableEvents(RoomState.lobby), {RoomEvent.arm, RoomEvent.end});
      expect(availableEvents(RoomState.ready),
          {RoomEvent.start, RoomEvent.cancel, RoomEvent.end});
      expect(availableEvents(RoomState.playing),
          {RoomEvent.reanchor, RoomEvent.end});
    });

    test('offers nothing once ended', () {
      // A terminal state with an available action would let a host act on a
      // room that no longer exists.
      expect(availableEvents(RoomState.ended), isEmpty);
      expect(RoomState.ended.isTerminal, isTrue);
    });

    test('never offers START from LOBBY', () {
      // The exact illegal transition the server's own test asserts. Offering
      // it would produce a 409 the user cannot act on.
      expect(availableEvents(RoomState.lobby), isNot(contains(RoomEvent.start)));
    });
  });

  group('Room', () {
    Room roomWith({
      int maxParticipants = 4,
      List<Participant> participants = const [],
    }) =>
        Room(
          id: 'r1',
          contentId: 'c1',
          hostUserId: 'u1',
          state: RoomState.lobby,
          syncMode: 'COMPANION',
          visibility: 'private',
          maxParticipants: maxParticipants,
          currentSeq: 1,
          createdAt: DateTime.utc(2026, 8, 26),
          serverTime: DateTime.utc(2026, 8, 26),
          participants: participants,
        );

    Participant person(String id, {bool connected = true, bool host = false}) =>
        Participant(
          userId: id,
          isHost: host,
          connected: connected,
          joinedAt: DateTime.utc(2026, 8, 26),
        );

    test('counts a seat as occupied even when the person is disconnected', () {
      // Somebody in a tunnel still occupies a seat. Freeing it because they
      // lost signal would let a stranger take their place while they
      // reconnect.
      final room = roomWith(
        maxParticipants: 2,
        participants: [person('u1', host: true), person('u2', connected: false)],
      );
      expect(room.hasSpace, isFalse);
      expect(room.connectedCount, 1);
      expect(room.participants.length, 2);
    });

    test('reports space when under capacity', () {
      final room = roomWith(participants: [person('u1', host: true)]);
      expect(room.hasSpace, isTrue);
    });

    test('identifies the host', () {
      final room = roomWith();
      expect(room.isHost('u1'), isTrue);
      expect(room.isHost('u2'), isFalse);
    });
  });
}
