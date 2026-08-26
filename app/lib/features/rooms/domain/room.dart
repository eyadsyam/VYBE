/// Room domain entities (§4.1).
///
/// Pure Dart. The join-code parsing below is the reason this file is worth
/// having separately from the API layer: it runs on every keystroke in the
/// join field, and it must agree with the server exactly or a code that looks
/// valid here is rejected there.
library;

/// FR-15's lifecycle.
enum RoomState {
  lobby,
  ready,
  playing,
  ended;

  static RoomState fromWire(String? value) => switch (value) {
        'LOBBY' => RoomState.lobby,
        'READY' => RoomState.ready,
        'PLAYING' => RoomState.playing,
        'ENDED' => RoomState.ended,
        // An unrecognised state means this client is older than the server.
        // `lobby` is the safe landing place: it offers the fewest actions, so a
        // client that guesses wrong cannot drive the room somewhere unexpected.
        _ => RoomState.lobby,
      };

  String get wire => switch (this) {
        RoomState.lobby => 'LOBBY',
        RoomState.ready => 'READY',
        RoomState.playing => 'PLAYING',
        RoomState.ended => 'ENDED',
      };

  bool get isTerminal => this == RoomState.ended;

  /// Whether the room is running its shared timeline.
  bool get isLive => this == RoomState.playing;
}

/// The transitions FR-15 allows, as the client understands them.
enum RoomEvent {
  arm,
  start,
  reanchor,
  cancel,
  end;

  String get wire => switch (this) {
        RoomEvent.arm => 'ARM',
        RoomEvent.start => 'START',
        RoomEvent.reanchor => 'REANCHOR',
        RoomEvent.cancel => 'CANCEL',
        RoomEvent.end => 'END',
      };
}

/// Which transitions a host may offer from a given state.
///
/// Duplicated from the server for ONE purpose: deciding which buttons to
/// render. The server is still the authority and will refuse an illegal
/// transition with 409; this only stops the UI offering a button that is
/// guaranteed to fail, which is worse than not offering it.
Set<RoomEvent> availableEvents(RoomState state) => switch (state) {
      RoomState.lobby => {RoomEvent.arm, RoomEvent.end},
      RoomState.ready => {RoomEvent.start, RoomEvent.cancel, RoomEvent.end},
      RoomState.playing => {RoomEvent.reanchor, RoomEvent.end},
      RoomState.ended => const {},
    };

/// Someone in the room.
class Participant {
  const Participant({
    required this.userId,
    required this.isHost,
    required this.connected,
    required this.joinedAt,
  });

  final String userId;
  final bool isHost;

  /// Live socket presence (FR-39), not membership. Somebody in a tunnel is a
  /// participant who is not connected — showing them as gone would be wrong.
  final bool connected;

  final DateTime joinedAt;

  @override
  bool operator ==(Object other) =>
      other is Participant &&
      other.userId == userId &&
      other.isHost == isHost &&
      other.connected == connected &&
      other.joinedAt == joinedAt;

  @override
  int get hashCode => Object.hash(userId, isHost, connected, joinedAt);
}

/// A watch party.
class Room {
  const Room({
    required this.id,
    required this.contentId,
    required this.hostUserId,
    required this.state,
    required this.syncMode,
    required this.visibility,
    required this.maxParticipants,
    required this.currentSeq,
    required this.createdAt,
    required this.serverTime,
    this.joinCode,
    this.shareUrl,
    this.title,
    this.anchorServerTime,
    this.anchorOffsetMs = 0,
    this.reanchorCount = 0,
    this.startedAt,
    this.endedAt,
    this.endReason,
    this.participants = const [],
  });

  final String id;
  final String contentId;
  final String hostUserId;
  final RoomState state;
  final String syncMode;
  final String visibility;
  final int maxParticipants;

  /// The room's position in its event log (FR-30).
  ///
  /// A reconnecting client compares its own last applied seq against this to
  /// decide whether it needs a resync at all — which is why the server sends it
  /// on every room payload and in the socket's HELLO.
  final int currentSeq;

  final DateTime createdAt;

  /// The server's clock when this payload was produced (ADR-002).
  ///
  /// Carried on every room response so the client always has a fresh reference
  /// without a second round trip.
  final DateTime serverTime;

  /// Present only for members. The credential that admits somebody, so a UI
  /// that renders it must be sure of who is looking.
  final String? joinCode;
  final String? shareUrl;

  final String? title;
  final DateTime? anchorServerTime;
  final int anchorOffsetMs;
  final int reanchorCount;
  final DateTime? startedAt;
  final DateTime? endedAt;
  final String? endReason;
  final List<Participant> participants;

  bool isHost(String userId) => hostUserId == userId;

  /// Participants currently holding a socket.
  int get connectedCount => participants.where((p) => p.connected).length;

  /// Whether another person can be admitted, for display only.
  ///
  /// Counts MEMBERS, not connections: somebody in a tunnel still occupies a
  /// seat, and freeing it because they lost signal would let a stranger take
  /// their place while they reconnect.
  bool get hasSpace => participants.length < maxParticipants;

  Room copyWith({
    RoomState? state,
    int? currentSeq,
    String? hostUserId,
    List<Participant>? participants,
    DateTime? serverTime,
    DateTime? anchorServerTime,
    DateTime? endedAt,
    String? endReason,
  }) =>
      Room(
        id: id,
        contentId: contentId,
        hostUserId: hostUserId ?? this.hostUserId,
        state: state ?? this.state,
        syncMode: syncMode,
        visibility: visibility,
        maxParticipants: maxParticipants,
        currentSeq: currentSeq ?? this.currentSeq,
        createdAt: createdAt,
        serverTime: serverTime ?? this.serverTime,
        joinCode: joinCode,
        shareUrl: shareUrl,
        title: title,
        anchorServerTime: anchorServerTime ?? this.anchorServerTime,
        anchorOffsetMs: anchorOffsetMs,
        reanchorCount: reanchorCount,
        startedAt: startedAt,
        endedAt: endedAt ?? this.endedAt,
        endReason: endReason ?? this.endReason,
        participants: participants ?? this.participants,
      );
}

/// Join code handling (FR-12), matching the server's Crockford rules.
///
/// This runs on every keystroke in the join field, so it has to be both fast
/// and exactly right. "Exactly right" means agreeing with the server: a code
/// this accepts and the server rejects is a user typing a correct code and
/// being told it is wrong.
class JoinCode {
  const JoinCode._();

  static const length = 6;

  /// Crockford base32, with I, L, O and U excluded.
  static const alphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

  /// Normalises what a user actually types into a canonical code.
  ///
  /// Returns null when the input cannot be a code. The lenient decoding is the
  /// point: a code is read aloud over a call and typed by somebody else, so
  /// transcription is the dominant failure mode.
  ///
  ///   * case is ignored
  ///   * hyphens and spaces are stripped, because people group digits
  ///   * `I`, `i`, `L`, `l` become `1`; `O`, `o` become `0`
  ///   * `U` is REFUSED, not mapped — unlike the others there is no digit it is
  ///     plausibly a misreading of, and silently mapping it would resolve to a
  ///     DIFFERENT room, which is worse than refusing
  static String? parse(String raw) {
    final buffer = StringBuffer();

    for (final rune in raw.trim().toUpperCase().runes) {
      final char = String.fromCharCode(rune);
      if (char == '-' || char == ' ' || char == '_') continue;

      final decoded = switch (char) {
        'I' || 'L' => '1',
        'O' => '0',
        _ => char,
      };

      if (!alphabet.contains(decoded)) return null;
      buffer.write(decoded);
      if (buffer.length > length) return null;
    }

    final code = buffer.toString();
    return code.length == length ? code : null;
  }

  /// Whether the input is a complete, valid code.
  static bool isComplete(String raw) => parse(raw) != null;

  /// Formats a code for display, grouped for readability when read aloud.
  static String format(String code) =>
      code.length == length ? '${code.substring(0, 3)}-${code.substring(3)}' : code;
}
