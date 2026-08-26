/// The room WebSocket client (FR-28–FR-42).
///
/// A client is a POSITION IN A LOG, not a listener on a stream (ADR-003). That
/// single idea is what makes reconnection solvable: this class remembers its
/// `lastSeq`, and on every reconnect it asks the server what it missed. Without
/// it, a 30-second tunnel would leave the UI silently and permanently wrong,
/// which is exactly the scenario AC-13 makes mandatory.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:web_socket_channel/web_socket_channel.dart';

/// Frame types, server to client. Mirrors realtime/protocol.go.
class ServerFrame {
  const ServerFrame._();

  static const hello = 'HELLO';
  static const event = 'EVENT';
  static const delta = 'DELTA';
  static const snapshot = 'SNAPSHOT';
  static const pong = 'PONG';
  static const error = 'ERROR';
}

/// Frame types, client to server.
class ClientFrame {
  const ClientFrame._();

  static const ping = 'PING';
  static const resync = 'RESYNC';
}

/// One durable room event (§7.2).
class RoomEnvelope {
  const RoomEnvelope({
    required this.id,
    required this.room,
    required this.seq,
    required this.type,
    required this.timestamp,
    required this.payload,
    this.actor,
  });

  final String id;
  final String room;
  final int seq;
  final String type;
  final DateTime timestamp;
  final String? actor;
  final Map<String, dynamic> payload;

  static RoomEnvelope? tryParse(Object? raw) {
    if (raw is! Map) return null;
    final json = Map<String, dynamic>.from(raw);

    final id = json['id'];
    final seq = json['seq'];
    final type = json['type'];
    if (id is! String || seq is! num || type is! String) return null;

    final payload = json['payload'];
    return RoomEnvelope(
      id: id,
      room: json['room'] as String? ?? '',
      seq: seq.toInt(),
      type: type,
      timestamp: DateTime.tryParse(json['ts'] as String? ?? '')?.toUtc() ??
          DateTime.now().toUtc(),
      actor: json['actor'] as String?,
      payload: payload is Map ? Map<String, dynamic>.from(payload) : const {},
    );
  }
}

/// What the socket reports upward.
sealed class SocketMessage {
  const SocketMessage();
}

/// A durable event to apply.
final class SocketEvent extends SocketMessage {
  const SocketEvent(this.envelope);
  final RoomEnvelope envelope;
}

/// The whole state, replacing whatever the client had.
final class SocketSnapshot extends SocketMessage {
  const SocketSnapshot(this.state, this.currentSeq, this.reason);
  final Map<String, dynamic> state;
  final int currentSeq;
  final String reason;
}

/// Connection status changed.
final class SocketStatusChanged extends SocketMessage {
  const SocketStatusChanged(this.status);
  final SocketStatus status;
}

/// A clock sample for ADR-002.
final class SocketClockSample extends SocketMessage {
  const SocketClockSample({
    required this.offset,
    required this.roundTrip,
  });

  /// How far the device's clock is from the server's. Positive means the
  /// device is BEHIND.
  final Duration offset;
  final Duration roundTrip;
}

enum SocketStatus { idle, connecting, connected, reconnecting, closed }

/// Opens a socket. Injected so tests can drive a fake channel.
typedef ChannelFactory = WebSocketChannel Function(Uri url);

/// A connection to one room.
///
/// Owns reconnection, backoff, the sequence position, and dedupe. The UI
/// subscribes to [messages] and never sees a raw frame.
class RoomSocket {
  RoomSocket({
    required String socketBaseUrl,
    required String roomId,
    required Future<String?> Function() ticketProvider,
    ChannelFactory? channelFactory,
    Future<void> Function(Duration)? delay,
    DateTime Function()? now,
    Random? random,
  })  : _base = socketBaseUrl,
        // Dart forbids named parameters that start with an underscore, so
        // `this._roomId` does not compile and the lint's suggestion cannot be
        // taken. Same for every private field initialised from a named
        // parameter below.
        // ignore: prefer_initializing_formals
        _roomId = roomId,
        // ignore: prefer_initializing_formals
        _ticketProvider = ticketProvider,
        _connect = channelFactory ?? WebSocketChannel.connect,
        _delay = delay ?? Future.delayed,
        _now = now ?? DateTime.now,
        _random = random ?? Random();

  final String _base;
  final String _roomId;

  /// Mints a FRESH ticket per connection attempt.
  ///
  /// A function rather than a value because tickets are single-use and expire
  /// in 60 seconds: a stored one is guaranteed dead by the second reconnect,
  /// and reusing it would turn every reconnection into a 401.
  final Future<String?> Function() _ticketProvider;

  final ChannelFactory _connect;
  final Future<void> Function(Duration) _delay;
  final DateTime Function() _now;
  final Random _random;

  final _controller = StreamController<SocketMessage>.broadcast();
  Stream<SocketMessage> get messages => _controller.stream;

  WebSocketChannel? _channel;
  StreamSubscription<dynamic>? _subscription;
  Timer? _pingTimer;
  bool _disposed = false;
  int _attempt = 0;

  SocketStatus _status = SocketStatus.idle;
  SocketStatus get status => _status;

  /// The last sequence number applied.
  ///
  /// The client's entire position in the log. Persisted only in memory: on a
  /// cold start there is nothing to resync FROM, so the room is fetched over
  /// HTTP and the socket starts from that snapshot's seq.
  int _lastSeq = 0;
  int get lastSeq => _lastSeq;

  /// FR-34's dedupe window: the last 500 envelope ids.
  ///
  /// Bounded on purpose. An unbounded set grows for the lifetime of a room and
  /// is the kind of leak that only shows up in the 30-minute session NFR-7
  /// measures — by which point it is a memory graph nobody can explain.
  final _seen = <String>{};
  final _seenOrder = <String>[];
  static const dedupeCapacity = 500;

  /// Outstanding pings, by the client timestamp that identifies them.
  final _pending = <int, DateTime>{};

  /// Connects, resuming from [fromSeq].
  Future<void> connect({int fromSeq = 0}) async {
    if (_disposed) return;
    _lastSeq = fromSeq > _lastSeq ? fromSeq : _lastSeq;
    await _open();
  }

  Future<void> _open() async {
    if (_disposed) return;
    _setStatus(_attempt == 0 ? SocketStatus.connecting : SocketStatus.reconnecting);

    final ticket = await _ticketProvider();
    if (ticket == null || ticket.isEmpty) {
      // No ticket means not authenticated. Retrying would hammer the ticket
      // endpoint with a credential that is not going to appear, so this is
      // terminal until something calls connect() again.
      _setStatus(SocketStatus.closed);
      return;
    }

    // The ticket goes in the query string because that is the only place a
    // WebSocket handshake can carry it — and it is safe to do so precisely
    // because it is single-use and expires in 60 seconds. An ACCESS token here
    // would be a long-lived credential in every proxy log on the path, which
    // is why the server refuses one outright.
    final url = Uri.parse('$_base/v1/ws').replace(
      queryParameters: {'room': _roomId, 'ticket': ticket},
    );

    try {
      final channel = _connect(url);
      _channel = channel;
      _subscription = channel.stream.listen(
        _onFrame,
        onError: (Object _) => _scheduleReconnect(),
        onDone: _scheduleReconnect,
        cancelOnError: false,
      );
      _setStatus(SocketStatus.connected);
      _attempt = 0;
      _startPinging();
    } on Object {
      _scheduleReconnect();
    }
  }

  void _onFrame(dynamic raw) {
    if (raw is! String) return;

    Map<String, dynamic> frame;
    try {
      final decoded = jsonDecode(raw);
      if (decoded is! Map) return;
      frame = Map<String, dynamic>.from(decoded);
    } on FormatException {
      // A malformed frame is dropped, not fatal. Tearing the connection down
      // over one bad message would turn a server-side encoding bug into a
      // reconnect storm.
      return;
    }

    switch (frame['type']) {
      case ServerFrame.hello:
        _onHello(frame);
      case ServerFrame.event:
        _onEvent(frame['event']);
      case ServerFrame.delta:
        _onDelta(frame);
      case ServerFrame.snapshot:
        _onSnapshot(frame);
      case ServerFrame.pong:
        _onPong(frame);
      case ServerFrame.error:
        // Reported by the server and deliberately not fatal.
        break;
      default:
        // FR-33 in the client's direction: an unknown frame type means this
        // client is older than the server. Ignoring it is REQUIRED, so that a
        // server can ship a new frame before every client understands it.
        break;
    }
  }

  void _onHello(Map<String, dynamic> frame) {
    final currentSeq = (frame['currentSeq'] as num?)?.toInt() ?? 0;

    // Ask for a resync only when there is actually a gap. HELLO carries the
    // room's position precisely so the overwhelmingly common case — a clean
    // reconnect with nothing missed — costs no round trip at all.
    if (currentSeq > _lastSeq) {
      _send({'type': ClientFrame.resync, 'lastSeq': _lastSeq});
    }
    _ping();
  }

  void _onEvent(Object? raw) {
    final envelope = RoomEnvelope.tryParse(raw);
    if (envelope == null) return;
    _apply(envelope);
  }

  void _onDelta(Map<String, dynamic> frame) {
    final events = frame['events'];
    if (events is! List) return;

    final envelopes = events
        .map(RoomEnvelope.tryParse)
        .whereType<RoomEnvelope>()
        .toList()
      ..sort((a, b) => a.seq.compareTo(b.seq));

    // A delta with a hole in it is worse than no delta: applying it would
    // leave the client believing it is caught up while silently missing an
    // event. The server verifies contiguity before sending, so this is defence
    // in depth — and if it ever fires, a snapshot is the only safe answer.
    for (var i = 0; i < envelopes.length; i++) {
      final expected = _lastSeq + i + 1;
      if (envelopes[i].seq != expected) {
        _send({'type': ClientFrame.resync, 'lastSeq': 0});
        return;
      }
    }

    for (final envelope in envelopes) {
      _apply(envelope);
    }
  }

  void _onSnapshot(Map<String, dynamic> frame) {
    final state = frame['state'];
    if (state is! Map) return;

    final currentSeq = (frame['currentSeq'] as num?)?.toInt() ?? _lastSeq;
    _lastSeq = currentSeq;

    // The dedupe window is cleared because a snapshot REPLACES history. Ids
    // from before it can never legitimately arrive again, and keeping them
    // would waste the bounded window on events that no longer matter.
    _seen.clear();
    _seenOrder.clear();

    _emit(
      SocketSnapshot(
        Map<String, dynamic>.from(state),
        currentSeq,
        frame['reason'] as String? ?? '',
      ),
    );
  }

  void _onPong(Map<String, dynamic> frame) {
    final clientTime = (frame['clientTime'] as num?)?.toInt();
    final serverTime = (frame['serverTime'] as num?)?.toInt();
    if (clientTime == null || serverTime == null) return;

    final sentAt = _pending.remove(clientTime);
    if (sentAt == null) return;

    final receivedAt = _now();
    final roundTrip = receivedAt.difference(sentAt);

    // ADR-002's estimator: offset = ((t1 − t0) + (t2 − t3)) / 2, where t1 and
    // t2 are the server's receive and send times. The server processes a PING
    // in microseconds, so both collapse to its single `serverTime` and the
    // formula reduces to "server time minus the midpoint of the round trip".
    final midpoint = sentAt.add(roundTrip ~/ 2);
    final offset = DateTime.fromMillisecondsSinceEpoch(serverTime, isUtc: true)
        .difference(midpoint.toUtc());

    _emit(SocketClockSample(offset: offset, roundTrip: roundTrip));
  }

  /// Applies an envelope exactly once (FR-34, AC-12).
  void _apply(RoomEnvelope envelope) {
    if (_seen.contains(envelope.id)) {
      // A duplicate. The state change must happen exactly once, and a
      // reconnect that overlaps a delivery makes duplicates ordinary rather
      // than exceptional.
      return;
    }

    _remember(envelope.id);
    if (envelope.seq > _lastSeq) {
      _lastSeq = envelope.seq;
    }
    _emit(SocketEvent(envelope));
  }

  void _remember(String id) {
    _seen.add(id);
    _seenOrder.add(id);
    if (_seenOrder.length > dedupeCapacity) {
      _seen.remove(_seenOrder.removeAt(0));
    }
  }

  void _startPinging() {
    _pingTimer?.cancel();

    // Ping IMMEDIATELY, then on a timer. Waiting for the first tick would
    // leave the room with no clock offset for its first twenty seconds — and
    // the first twenty seconds are when the host is arming a countdown, which
    // is precisely when ADR-002's offset has to be right.
    _ping();

    // Every 20 seconds thereafter. Frequent enough that the estimate stays
    // fresh through the 30-minute session NFR-7 measures, rare enough not to
    // be a meaningful battery or data cost.
    _pingTimer = Timer.periodic(const Duration(seconds: 20), (_) => _ping());
  }

  void _ping() {
    final sentAt = _now();
    final stamp = sentAt.toUtc().millisecondsSinceEpoch;
    _pending[stamp] = sentAt;

    // Drop stale entries rather than letting the map grow. A pong that never
    // arrives is a lost frame, not a reason to remember a timestamp forever.
    if (_pending.length > 8) {
      final oldest = _pending.keys.reduce(min);
      _pending.remove(oldest);
    }

    _send({'type': ClientFrame.ping, 'clientTime': stamp});
  }

  void _send(Map<String, dynamic> frame) {
    final channel = _channel;
    if (channel == null) return;
    try {
      channel.sink.add(jsonEncode(frame));
    } on Object {
      // The socket died between the check and the write. The reconnect path
      // already handles it; throwing here would surface a transport detail to
      // a caller that cannot act on it.
    }
  }

  void _scheduleReconnect() {
    if (_disposed) return;
    _teardownChannel();
    _setStatus(SocketStatus.reconnecting);

    _attempt += 1;
    unawaited(_delay(_backoff(_attempt)).then((_) => _open()));
  }

  /// Exponential backoff with full jitter, capped at 30 seconds.
  ///
  /// Jitter is load-bearing, not decoration. Every client disconnected by the
  /// same server restart would otherwise reconnect at the same instant and hit
  /// it with a synchronised wave precisely as it comes back — turning a
  /// ten-second restart into a sustained outage.
  Duration _backoff(int attempt) {
    const base = Duration(milliseconds: 500);
    const cap = Duration(seconds: 30);
    final exponential = base * pow(2, min(attempt - 1, 6)).toDouble();
    final bounded = exponential > cap ? cap : exponential;
    return Duration(milliseconds: _random.nextInt(bounded.inMilliseconds + 1));
  }

  void _setStatus(SocketStatus next) {
    if (_status == next) return;
    _status = next;
    _emit(SocketStatusChanged(next));
  }

  void _emit(SocketMessage message) {
    if (_controller.isClosed) return;
    _controller.add(message);
  }

  void _teardownChannel() {
    _pingTimer?.cancel();
    _pingTimer = null;
    unawaited(_subscription?.cancel());
    _subscription = null;
    // The sink is closed without awaiting: a dead socket's close can hang, and
    // reconnection must not wait on the corpse of the previous connection.
    unawaited(_channel?.sink.close());
    _channel = null;
    _pending.clear();
  }

  /// Closes permanently. Safe to call twice.
  Future<void> dispose() async {
    if (_disposed) return;
    _disposed = true;
    _teardownChannel();
    _setStatus(SocketStatus.closed);
    await _controller.close();
  }
}
