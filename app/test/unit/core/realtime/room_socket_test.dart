import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:flutter_test/flutter_test.dart';
import 'package:stream_channel/stream_channel.dart';
import 'package:vybe/core/realtime/room_socket.dart';
import 'package:web_socket_channel/web_socket_channel.dart';

/// A WebSocketChannel a test can drive from both ends.
class FakeChannel extends StreamChannelMixin<dynamic>
    implements WebSocketChannel {
  FakeChannel(this.url);

  final Uri url;

  final _incoming = StreamController<dynamic>.broadcast();
  final _outgoing = <String>[];
  var _closed = false;

  /// What the client sent, decoded.
  List<Map<String, dynamic>> get sent => _outgoing
      .map((raw) => Map<String, dynamic>.from(jsonDecode(raw) as Map))
      .toList();

  bool get isClosed => _closed;

  /// Pushes a frame from the "server".
  void emit(Map<String, dynamic> frame) => _incoming.add(jsonEncode(frame));

  /// Drops the connection, as a network failure would.
  void drop() => _incoming.close();

  @override
  Stream<dynamic> get stream => _incoming.stream;

  @override
  WebSocketSink get sink => _FakeSink(this);

  @override
  Future<void> get ready => Future.value();

  @override
  int? get closeCode => _closed ? 1000 : null;

  @override
  String? get closeReason => null;

  @override
  String? get protocol => null;
}

class _FakeSink implements WebSocketSink {
  _FakeSink(this._channel);
  final FakeChannel _channel;

  @override
  void add(dynamic data) {
    if (_channel._closed) return;
    _channel._outgoing.add(data as String);
  }

  @override
  Future<void> close([int? closeCode, String? closeReason]) async {
    _channel._closed = true;
    if (!_channel._incoming.isClosed) await _channel._incoming.close();
  }

  @override
  void addError(Object error, [StackTrace? stackTrace]) {}

  @override
  Future<void> addStream(Stream<dynamic> stream) async {}

  @override
  Future<void> get done => Future.value();
}

/// Builds an envelope frame the way the server does.
Map<String, dynamic> eventFrame(
  int seq, {
  String? id,
  String type = 'CHAT_MESSAGE',
  Map<String, dynamic> payload = const {},
}) =>
    {
      'type': 'EVENT',
      'event': {
        'v': 1,
        'id': id ?? 'evt-$seq',
        'room': 'room-1',
        'seq': seq,
        'type': type,
        'ts': '2026-08-26T12:00:00Z',
        'payload': payload,
      },
    };

void main() {
  late FakeChannel channel;
  late RoomSocket socket;
  late List<SocketMessage> received;
  late DateTime clock;

  /// Builds a socket wired to a fresh fake channel.
  Future<void> openSocket({
    int fromSeq = 0,
    Future<String?> Function()? ticketProvider,
  }) async {
    clock = DateTime.utc(2026, 8, 26, 12);
    received = [];

    socket = RoomSocket(
      socketBaseUrl: 'ws://test',
      roomId: 'room-1',
      ticketProvider: ticketProvider ?? () async => 'a-ticket',
      channelFactory: (url) => channel = FakeChannel(url),
      // No real waiting: the backoff is exercised by counting attempts, not by
      // sleeping through them.
      delay: (_) async {},
      now: () => clock,
      random: Random(1),
    );
    socket.messages.listen(received.add);
    await socket.connect(fromSeq: fromSeq);
    await pumpEventQueue();
  }

  tearDown(() async => socket.dispose());

  group('handshake', () {
    test('puts the ticket and room in the query string, not a token', () async {
      await openSocket();

      expect(channel.url.queryParameters['room'], 'room-1');
      expect(channel.url.queryParameters['ticket'], 'a-ticket');

      // FR-5: an access token must never reach a URL. The ticket is safe there
      // only because it is single-use and expires in 60 seconds.
      for (final banned in ['access_token', 'token', 'jwt', 'authorization']) {
        expect(
          channel.url.queryParameters.containsKey(banned),
          isFalse,
          reason: '$banned must never appear in a socket URL',
        );
      }
    });

    test('mints a fresh ticket per connection attempt', () async {
      // Tickets do not survive redemption, so a cached one would turn every
      // reconnect into a 401.
      var minted = 0;
      await openSocket(ticketProvider: () async => 'ticket-${++minted}');
      expect(minted, 1);

      channel.drop();
      await pumpEventQueue();

      expect(minted, 2, reason: 'the reconnect reused the redeemed ticket');
      expect(channel.url.queryParameters['ticket'], 'ticket-2');
    });

    test('stops rather than looping when no ticket is available', () async {
      // No ticket means not signed in. Retrying would hammer the endpoint with
      // a credential that is not going to appear.
      var calls = 0;
      await openSocket(ticketProvider: () async {
        calls++;
        return null;
      });

      expect(calls, 1);
      expect(socket.status, SocketStatus.closed);
    });
  });

  group('HELLO', () {
    test('does not resync when nothing was missed', () async {
      // The common case: a clean reconnect with nothing to catch up on. HELLO
      // carries the room's position precisely so this costs no round trip.
      await openSocket(fromSeq: 42);
      channel.emit({
        'type': 'HELLO',
        'room': 'room-1',
        'currentSeq': 42,
        'serverTime': clock.millisecondsSinceEpoch,
        'heartbeatSeconds': 54,
      });
      await pumpEventQueue();

      final resyncs = channel.sent.where((f) => f['type'] == 'RESYNC');
      expect(resyncs, isEmpty,
          reason: 'an up-to-date client asked for a resync it did not need');
    });

    test('resyncs from its own position when it is behind', () async {
      await openSocket(fromSeq: 40);
      channel.emit({
        'type': 'HELLO',
        'room': 'room-1',
        'currentSeq': 90,
        'serverTime': clock.millisecondsSinceEpoch,
        'heartbeatSeconds': 54,
      });
      await pumpEventQueue();

      final resync =
          channel.sent.firstWhere((f) => f['type'] == 'RESYNC');
      expect(resync['lastSeq'], 40);
    });
  });

  group('events', () {
    test('applies an event and advances the position', () async {
      await openSocket(fromSeq: 5);
      channel.emit(eventFrame(6));
      await pumpEventQueue();

      final events = received.whereType<SocketEvent>().toList();
      expect(events, hasLength(1));
      expect(events.single.envelope.seq, 6);
      expect(socket.lastSeq, 6);
    });

    test('applies a duplicate exactly once', () async {
      // AC-12. A reconnect that overlaps a delivery makes duplicates ordinary
      // rather than exceptional, so this is the common path, not the edge.
      await openSocket(fromSeq: 5);
      channel.emit(eventFrame(6, id: 'the-same-id'));
      channel.emit(eventFrame(6, id: 'the-same-id'));
      channel.emit(eventFrame(6, id: 'the-same-id'));
      await pumpEventQueue();

      expect(received.whereType<SocketEvent>(), hasLength(1),
          reason: 'the same envelope id was applied more than once');
    });

    test('bounds the dedupe window at 500', () async {
      // An unbounded set grows for the life of the room. The leak only shows
      // up in the 30-minute session NFR-7 measures.
      await openSocket();
      for (var seq = 1; seq <= 600; seq++) {
        channel.emit(eventFrame(seq));
      }
      await pumpEventQueue();

      expect(received.whereType<SocketEvent>(), hasLength(600));

      // The oldest id has been evicted, so it is no longer recognised as a
      // duplicate. That is the correct trade: the window is bounded, and an
      // event 600 places old is not going to be redelivered.
      channel.emit(eventFrame(1));
      await pumpEventQueue();
      expect(received.whereType<SocketEvent>(), hasLength(601));

      // A recent one is still deduped.
      channel.emit(eventFrame(600));
      await pumpEventQueue();
      expect(received.whereType<SocketEvent>(), hasLength(601));
    });

    test('ignores a malformed envelope without dropping the connection', () async {
      await openSocket();
      channel.emit({'type': 'EVENT', 'event': {'not': 'an envelope'}});
      channel.emit({'type': 'EVENT', 'event': 'a string'});
      await pumpEventQueue();

      expect(received.whereType<SocketEvent>(), isEmpty);
      expect(channel.isClosed, isFalse);

      // And the socket still works afterwards.
      channel.emit(eventFrame(1));
      await pumpEventQueue();
      expect(received.whereType<SocketEvent>(), hasLength(1));
    });

    test('ignores an unknown frame type', () async {
      // FR-33 in the client's direction: a frame this client does not know
      // means the server is newer. Ignoring it is REQUIRED so a server can
      // ship a new frame before every client understands it.
      await openSocket();
      channel.emit({'type': 'TELEPORT', 'destination': 'mars'});
      await pumpEventQueue();

      expect(channel.isClosed, isFalse);
      channel.emit(eventFrame(1));
      await pumpEventQueue();
      expect(received.whereType<SocketEvent>(), hasLength(1));
    });
  });

  group('DELTA', () {
    test('applies a contiguous run in order', () async {
      await openSocket(fromSeq: 1400);
      channel.emit({
        'type': 'DELTA',
        'fromSeq': 1401,
        'toSeq': 1405,
        'applied': true,
        'events': [
          for (var seq = 1401; seq <= 1405; seq++)
            eventFrame(seq)['event'],
        ],
      });
      await pumpEventQueue();

      final seqs =
          received.whereType<SocketEvent>().map((e) => e.envelope.seq).toList();
      expect(seqs, [1401, 1402, 1403, 1404, 1405]);
      expect(socket.lastSeq, 1405);
    });

    test('refuses a delta with a hole and asks for a full resync', () async {
      // Applying a delta with a gap would leave the client believing it is
      // caught up while silently missing an event — the worst outcome
      // available, because nothing later would reveal it.
      await openSocket(fromSeq: 10);
      channel.emit({
        'type': 'DELTA',
        'fromSeq': 11,
        'toSeq': 14,
        'applied': true,
        'events': [
          eventFrame(11)['event'],
          eventFrame(12)['event'],
          eventFrame(14)['event'], // 13 is missing
        ],
      });
      await pumpEventQueue();

      expect(received.whereType<SocketEvent>(), isEmpty,
          reason: 'a delta with a hole was applied');
      expect(socket.lastSeq, 10, reason: 'the position advanced past a gap');

      final resync = channel.sent.lastWhere((f) => f['type'] == 'RESYNC');
      expect(resync['lastSeq'], 0,
          reason: 'a holed delta must force a full resync, not another delta');
    });

    test('handles an empty delta', () async {
      await openSocket(fromSeq: 10);
      channel.emit({
        'type': 'DELTA',
        'fromSeq': 10,
        'toSeq': 10,
        'applied': false,
        'events': <dynamic>[],
      });
      await pumpEventQueue();

      expect(received.whereType<SocketEvent>(), isEmpty);
      expect(socket.lastSeq, 10);
    });
  });

  group('SNAPSHOT', () {
    test('replaces state and adopts the server position', () async {
      await openSocket(fromSeq: 1);
      channel.emit({
        'type': 'SNAPSHOT',
        'currentSeq': 900,
        'reason': 'gap of 899 exceeds the threshold of 200',
        'state': {'roomId': 'room-1', 'state': 'PLAYING', 'participants': []},
      });
      await pumpEventQueue();

      final snapshot = received.whereType<SocketSnapshot>().single;
      expect(snapshot.currentSeq, 900);
      expect(snapshot.state['state'], 'PLAYING');
      expect(snapshot.reason, contains('threshold'));
      expect(socket.lastSeq, 900);
    });

    test('clears the dedupe window', () async {
      // A snapshot REPLACES history, so ids from before it can never
      // legitimately arrive again. Keeping them would waste the bounded window
      // on events that no longer matter.
      await openSocket();
      channel.emit(eventFrame(1, id: 'old-event'));
      await pumpEventQueue();
      expect(received.whereType<SocketEvent>(), hasLength(1));

      channel.emit({
        'type': 'SNAPSHOT',
        'currentSeq': 50,
        'reason': 'aged out',
        'state': {'roomId': 'room-1'},
      });
      await pumpEventQueue();

      // The same id is no longer remembered, so it applies again. That is
      // correct: after a snapshot, anything the server sends is new.
      channel.emit(eventFrame(51, id: 'old-event'));
      await pumpEventQueue();
      expect(received.whereType<SocketEvent>(), hasLength(2));
    });
  });

  group('clock sync', () {
    test('computes an offset from the round trip (ADR-002)', () async {
      await openSocket();

      // The ping is sent at t0. Advance the clock to model a 100ms round trip,
      // and have the "server" answer with a clock 5 seconds ahead.
      final ping = channel.sent.firstWhere((f) => f['type'] == 'PING');
      final clientTime = ping['clientTime'] as int;

      clock = clock.add(const Duration(milliseconds: 100));
      final serverTime =
          DateTime.utc(2026, 8, 26, 12, 0, 5).millisecondsSinceEpoch;

      channel.emit({
        'type': 'PONG',
        'clientTime': clientTime,
        'serverTime': serverTime,
      });
      await pumpEventQueue();

      final sample = received.whereType<SocketClockSample>().single;
      expect(sample.roundTrip, const Duration(milliseconds: 100));
      // The device is ~5 seconds behind: the midpoint of the round trip was
      // 12:00:00.050, and the server said 12:00:05.
      expect(sample.offset.inMilliseconds, closeTo(4950, 5));
    });

    test('ignores a pong for a ping it never sent', () async {
      // An unmatched pong cannot be timed, and treating it as a sample would
      // feed a garbage offset into the clock.
      await openSocket();
      channel.emit({
        'type': 'PONG',
        'clientTime': 999999,
        'serverTime': clock.millisecondsSinceEpoch,
      });
      await pumpEventQueue();

      expect(received.whereType<SocketClockSample>(), isEmpty);
    });

    test('echoes its own timestamp so the offset is computable', () async {
      // ADR-002's formula needs the client's own send time back. A pong
      // carrying only the server's clock would let the client measure latency
      // but not correct itself, which is the entire point.
      await openSocket();
      final ping = channel.sent.firstWhere((f) => f['type'] == 'PING');
      expect(ping['clientTime'], isA<int>());
      expect(ping['clientTime'], clock.millisecondsSinceEpoch);
    });
  });

  group('reconnection', () {
    test('reconnects after the connection drops', () async {
      await openSocket(fromSeq: 7);
      final first = channel;

      first.drop();
      await pumpEventQueue();

      expect(identical(channel, first), isFalse,
          reason: 'a new channel was not opened');
      expect(socket.status, SocketStatus.connected);
    });

    test('keeps its position across a reconnect', () async {
      // The entire point of ADR-003: a client is a position in a log, and the
      // position must survive the transport dying.
      await openSocket();
      channel.emit(eventFrame(1));
      channel.emit(eventFrame(2));
      channel.emit(eventFrame(3));
      await pumpEventQueue();
      expect(socket.lastSeq, 3);

      channel.drop();
      await pumpEventQueue();
      expect(socket.lastSeq, 3, reason: 'the position was lost on reconnect');

      // And it resyncs from there, not from zero — which would re-fetch the
      // whole room for a two-second blip.
      channel.emit({
        'type': 'HELLO',
        'room': 'room-1',
        'currentSeq': 10,
        'serverTime': clock.millisecondsSinceEpoch,
        'heartbeatSeconds': 54,
      });
      await pumpEventQueue();

      final resync = channel.sent.firstWhere((f) => f['type'] == 'RESYNC');
      expect(resync['lastSeq'], 3);
    });

    test('stops reconnecting once disposed', () async {
      await openSocket();
      await socket.dispose();

      final before = channel;
      before.drop();
      await pumpEventQueue();

      expect(identical(channel, before), isTrue,
          reason: 'a disposed socket reconnected');
      expect(socket.status, SocketStatus.closed);
    });

    test('dispose is safe to call twice', () async {
      await openSocket();
      await socket.dispose();
      await socket.dispose();
      expect(socket.status, SocketStatus.closed);
    });
  });

  group('status', () {
    test('reports the connection lifecycle', () async {
      await openSocket();

      final statuses = received
          .whereType<SocketStatusChanged>()
          .map((m) => m.status)
          .toList();
      expect(statuses, contains(SocketStatus.connecting));
      expect(statuses.last, SocketStatus.connected);
    });
  });
}
