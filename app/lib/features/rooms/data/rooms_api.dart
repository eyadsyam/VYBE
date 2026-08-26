/// The rooms data source (§4.2).
///
/// Every mutating call carries an `Idempotency-Key`, and the key is generated
/// ONCE per logical operation rather than per attempt. That distinction is the
/// entire contract: a key regenerated on retry is not an idempotency key, it is
/// a new request, and the user gets two rooms.
library;

import 'dart:math';

import 'package:dio/dio.dart';

import '../../../core/error/failure.dart';
import '../../../core/error/result.dart';
import '../../../core/network/problem_details.dart';
import '../domain/room.dart';

class RoomsApi {
  RoomsApi(this._dio, {Random? random}) : _random = random ?? Random.secure();

  final Dio _dio;
  final Random _random;

  static const _idempotencyHeader = 'Idempotency-Key';

  /// FR-11. Opens a room.
  ///
  /// [idempotencyKey] is a parameter rather than generated inside, so a caller
  /// retrying after a timeout can pass the SAME key and get the original room
  /// back instead of a second one. Generating it here would make that
  /// impossible and would quietly defeat FR-57.
  Future<Result<Room>> create({
    required String contentId,
    String? title,
    String? visibility,
    String? syncMode,
    String? idempotencyKey,
  }) async {
    return _call(
      () => _dio.post<dynamic>(
        '/v1/rooms',
        data: {
          'contentId': contentId,
          'title': ?title,
          'visibility': ?visibility,
          'syncMode': ?syncMode,
        },
        options: Options(
          headers: {_idempotencyHeader: idempotencyKey ?? newIdempotencyKey()},
        ),
      ),
      expectedStatus: 201,
    );
  }

  /// FR-13. Joins by code.
  Future<Result<Room>> join({
    required String joinCode,
    String? idempotencyKey,
  }) async {
    final parsed = JoinCode.parse(joinCode);
    if (parsed == null) {
      // Refused locally, without a round trip. The server would answer
      // ROOM_NOT_FOUND for a malformed code anyway — deliberately
      // indistinguishable from an unknown one — so asking it would cost a
      // request and tell the user nothing more.
      return const Result.err(
        ServerFailure(status: 404, code: 'ROOM_NOT_FOUND'),
      );
    }

    return _call(
      () => _dio.post<dynamic>(
        '/v1/rooms/join',
        data: {'joinCode': parsed},
        options: Options(
          headers: {_idempotencyHeader: idempotencyKey ?? newIdempotencyKey()},
        ),
      ),
      expectedStatus: 200,
    );
  }

  /// Reads a room. Membership is required (FR-14).
  Future<Result<Room>> get(String roomId) =>
      _call(() => _dio.get<dynamic>('/v1/rooms/$roomId'), expectedStatus: 200);

  /// FR-17. Leaves.
  Future<Result<Room>> leave(String roomId, {String? idempotencyKey}) => _call(
        () => _dio.post<dynamic>(
          '/v1/rooms/$roomId/leave',
          options: Options(
            headers: {_idempotencyHeader: idempotencyKey ?? newIdempotencyKey()},
          ),
        ),
        expectedStatus: 200,
      );

  /// FR-19. Ends the room. Host only.
  Future<Result<Room>> end(String roomId, {String? idempotencyKey}) => _call(
        () => _dio.post<dynamic>(
          '/v1/rooms/$roomId/end',
          options: Options(
            headers: {_idempotencyHeader: idempotencyKey ?? newIdempotencyKey()},
          ),
        ),
        expectedStatus: 200,
      );

  /// FR-15. Drives the state machine. Host only.
  Future<Result<Room>> transition(
    String roomId,
    RoomEvent event, {
    String? idempotencyKey,
  }) =>
      _call(
        () => _dio.post<dynamic>(
          '/v1/rooms/$roomId/transition',
          data: {'event': event.wire},
          options: Options(
            headers: {_idempotencyHeader: idempotencyKey ?? newIdempotencyKey()},
          ),
        ),
        expectedStatus: 200,
      );

  /// FR-20, FR-59. The caller's rooms, newest first.
  Future<Result<RoomPage>> list({String? cursor, int limit = 20}) async {
    try {
      final response = await _dio.get<dynamic>(
        '/v1/rooms',
        queryParameters: {'limit': limit, 'cursor': ?cursor},
      );
      if (response.statusCode == 200 && response.data is Map) {
        final body = Map<String, dynamic>.from(response.data as Map);
        final items = body['items'];
        return Result.ok(
          RoomPage(
            // The server guarantees `items` is `[]` rather than null, but a
            // client that trusted that and was wrong would crash rather than
            // render §3.2's empty state. Defending costs one `is List`.
            rooms: items is List
                ? items
                    .whereType<Map>()
                    .map((r) => roomFromJson(Map<String, dynamic>.from(r)))
                    .toList()
                : const [],
            nextCursor: body['nextCursor'] as String?,
          ),
        );
      }
      return Result.err(_failureFrom(response));
    } on DioException catch (e) {
      return Result.err(_transportFailure(e));
    }
  }

  Future<Result<Room>> _call(
    Future<Response<dynamic>> Function() send, {
    required int expectedStatus,
  }) async {
    try {
      final response = await send();
      if (response.statusCode == expectedStatus && response.data is Map) {
        return Result.ok(
          roomFromJson(Map<String, dynamic>.from(response.data as Map)),
        );
      }
      return Result.err(_failureFrom(response));
    } on DioException catch (e) {
      return Result.err(_transportFailure(e));
    }
  }

  /// A fresh idempotency key.
  ///
  /// 128 bits of randomness, hex-encoded — 32 characters, inside the server's
  /// 8–255 bound. Random rather than derived from the request body: two
  /// genuinely separate room creations with identical bodies must NOT collide,
  /// and a content hash would make them do exactly that.
  String newIdempotencyKey() {
    final buffer = StringBuffer();
    for (var i = 0; i < 16; i++) {
      buffer.write(_random.nextInt(256).toRadixString(16).padLeft(2, '0'));
    }
    return buffer.toString();
  }

  static Failure _failureFrom(Response<dynamic> response) =>
      ProblemDetails.parse(response.data, response.statusCode ?? 0).toFailure();

  static Failure _transportFailure(DioException e) => switch (e.type) {
        DioExceptionType.connectionError ||
        DioExceptionType.connectionTimeout =>
          NetworkFailure(isOffline: true, cause: e),
        DioExceptionType.sendTimeout || DioExceptionType.receiveTimeout =>
          NetworkFailure(isOffline: false, cause: e),
        _ => UnexpectedFailure(e, e.stackTrace),
      };
}

/// One page of rooms.
class RoomPage {
  const RoomPage({required this.rooms, this.nextCursor});

  final List<Room> rooms;

  /// Null on the last page. Opaque — echo it back, never construct one.
  final String? nextCursor;

  bool get hasMore => nextCursor != null && nextCursor!.isNotEmpty;
}

/// Decodes a room payload.
///
/// Public because the socket's SNAPSHOT frame carries the same shape, and two
/// decoders for one wire format is how they drift.
Room roomFromJson(Map<String, dynamic> json) {
  final participants = json['participants'];

  return Room(
    id: json['id'] as String? ?? '',
    contentId: json['contentId'] as String? ?? '',
    hostUserId: json['hostUserId'] as String? ?? '',
    state: RoomState.fromWire(json['state'] as String?),
    syncMode: json['syncMode'] as String? ?? 'COMPANION',
    visibility: json['visibility'] as String? ?? 'private',
    maxParticipants: (json['maxParticipants'] as num?)?.toInt() ?? 4,
    currentSeq: (json['currentSeq'] as num?)?.toInt() ?? 0,
    createdAt: _time(json['createdAt']) ?? DateTime.now().toUtc(),
    // Falls back to the device clock only when the server omitted it, which it
    // never does. ADR-002 depends on this being the SERVER's time; silently
    // substituting the device's would make every offset calculation measure
    // nothing.
    serverTime: _time(json['serverTime']) ?? DateTime.now().toUtc(),
    joinCode: json['joinCode'] as String?,
    shareUrl: json['shareUrl'] as String?,
    title: json['title'] as String?,
    anchorServerTime: _time(json['anchorServerTime']),
    anchorOffsetMs: (json['anchorOffsetMs'] as num?)?.toInt() ?? 0,
    reanchorCount: (json['reanchorCount'] as num?)?.toInt() ?? 0,
    startedAt: _time(json['startedAt']),
    endedAt: _time(json['endedAt']),
    endReason: json['endReason'] as String?,
    participants: participants is List
        ? participants
            .whereType<Map>()
            .map((p) => _participantFrom(Map<String, dynamic>.from(p)))
            .toList()
        : const [],
  );
}

Participant _participantFrom(Map<String, dynamic> json) => Participant(
      userId: json['userId'] as String? ?? '',
      isHost: json['isHost'] as bool? ?? false,
      // Absent means "we do not know", and the honest default is connected:
      // the participants list from an HTTP response carries membership, not
      // live presence, and rendering everybody as offline would be wrong on
      // every screen that has not opened a socket yet.
      connected: json['connected'] as bool? ?? true,
      joinedAt: _time(json['joinedAt']) ?? DateTime.now().toUtc(),
    );

DateTime? _time(Object? value) =>
    value is String ? DateTime.tryParse(value)?.toUtc() : null;
