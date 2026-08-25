/// Client half of the Companion Sync clock (ADR-002, §7.4).
///
/// The server owns the truth; this file owns the correction. Its entire job is
/// to answer one question accurately: *what time does the server think it is,
/// right now?* Everything timed in a room — the countdown, trivia beats,
/// prediction windows — is scheduled against that answer and never against
/// `DateTime.now()`.
///
/// The property this buys, and the reason it exists (interview answer
/// §16.4.2): a device whose clock is five minutes wrong behaves identically to
/// one that is correct.
library;

import 'dart:math' as math;

/// §7.4 constants. Mirrored from the server's `internal/modules/realtime`
/// package. They are duplicated rather than fetched because a client that had
/// to ask the server for its tolerances could not schedule anything before the
/// first round trip completed.
class SyncConstants {
  const SyncConstants._();

  /// Rolling window of round-trip samples.
  static const int sampleWindow = 5;

  /// Beyond this, a sample's asymmetry can exceed the offset we are trying to
  /// measure, so it tells us nothing.
  static const Duration maxAcceptableRtt = Duration(seconds: 2);

  /// A timed event fires only within this window of its target (FR-27).
  static const Duration beatTolerance = Duration(milliseconds: 1500);

  /// How often to re-measure while connected (§7.4).
  static const Duration handshakeInterval = Duration(seconds: 60);
}

/// One completed PING/PONG round trip.
///
/// `t0`/`t3` are read from this device's clock; `t1`/`t2` from the server's.
/// The two clocks are never required to agree — we measure how far apart they
/// are, which is the whole trick.
class ClockSample {
  const ClockSample({
    required this.t0,
    required this.t1,
    required this.t2,
    required this.t3,
  });

  /// Device clock: PING sent.
  final DateTime t0;

  /// Server clock: PING received.
  final DateTime t1;

  /// Server clock: PONG sent.
  final DateTime t2;

  /// Device clock: PONG received.
  final DateTime t3;

  /// Round trip excluding server think-time.
  ///
  /// Subtracting `(t2 - t1)` matters: without it a slow handler looks like a
  /// slow network, which would both distort sample selection and inflate the
  /// RTT compensation applied to trivia timing.
  Duration get rtt => t3.difference(t0) - t2.difference(t1);

  /// How far this device's clock sits behind the server's. Add to a device
  /// instant to get server time.
  ///
  ///     offset = ((t1 - t0) + (t2 - t3)) / 2
  Duration get offset =>
      Duration(microseconds: (t1.difference(t0).inMicroseconds + t2.difference(t3).inMicroseconds) ~/ 2);

  /// A negative RTT means the timestamps are incoherent — a clock stepped
  /// mid-round-trip. Such a sample is meaningless, not merely poor.
  bool get isUsable => !rtt.isNegative && rtt <= SyncConstants.maxAcceptableRtt;
}

/// Maintains the rolling sample window and exposes the current correction.
///
/// Deliberately a plain object with no Flutter, no timers, and no I/O, so the
/// §13.1 domain coverage gate applies to it and every branch is unit-testable.
/// The timer that feeds it lives in the socket layer.
class SyncClock {
  SyncClock({DateTime Function()? now}) : _now = now ?? DateTime.now;

  final DateTime Function() _now;
  final List<ClockSample> _samples = <ClockSample>[];

  /// Records a completed round trip.
  ///
  /// Unusable samples are retained so that "all five were bad" stays
  /// distinguishable from "we have not measured yet". The first is a degraded
  /// connection; the second is a connection still handshaking, and treating
  /// them alike would suppress timed events during every normal connect.
  void observe(ClockSample sample) {
    _samples.add(sample);
    if (_samples.length > SyncConstants.sampleWindow) {
      _samples.removeRange(0, _samples.length - SyncConstants.sampleWindow);
    }
  }

  /// The lowest-RTT usable sample, or null.
  ///
  /// §7.4 requires the lowest-RTT sample and explicitly **not** the mean.
  /// Offset error is bounded by path asymmetry, and asymmetry tracks delay, so
  /// one 800ms sample among four 45ms samples is noise. Averaging would spread
  /// its error across every subsequent calculation instead of discarding it.
  ClockSample? get bestSample {
    ClockSample? best;
    for (final s in _samples) {
      if (!s.isUsable) continue;
      if (best == null || s.rtt < best.rtt) best = s;
    }
    return best;
  }

  /// Current best correction, or null if nothing usable has been measured.
  Duration? get offset => bestSample?.offset;

  /// Current best RTT estimate, used for trivia RTT compensation.
  Duration? get rtt => bestSample?.rtt;

  /// True when we have samples but none are usable (FR-24).
  ///
  /// A degraded connection still carries chat and reactions; it must not
  /// schedule timed events (EC-13). Firing a trivia beat at an unknown offset
  /// is worse than not firing it.
  bool get isDegraded => _samples.isNotEmpty && bestSample == null;

  /// True once at least one usable sample exists.
  bool get isSynchronised => bestSample != null;

  int get sampleCount => _samples.length;

  /// The server's current time, as best we can tell.
  ///
  /// Returns null rather than falling back to the device clock when
  /// unsynchronised. That refusal is the point: silently substituting an
  /// uncorrected clock is how a five-minute device error becomes a
  /// five-minute timeline error, and the user would never know.
  DateTime? get serverNow {
    final o = offset;
    if (o == null) return null;
    return _now().add(o);
  }

  /// Clears the window. Called on reconnect, because an offset measured over a
  /// previous connection may describe a different network path entirely.
  void reset() => _samples.clear();
}

/// The shared virtual timeline (ADR-002).
///
///     tRoom = (serverNow - anchorServerTime) + anchorOffset
///
/// `anchorOffset` lets a room start part-way into a programme, which is what a
/// host re-anchor produces (FR-26).
class Timeline {
  const Timeline({required this.anchorServerTime, this.anchorOffset = Duration.zero});

  /// A room that has not started. Position is unavailable, not zero — zero
  /// would read as "at the very beginning", which is a different claim.
  const Timeline.notStarted() : anchorServerTime = null, anchorOffset = Duration.zero;

  final DateTime? anchorServerTime;
  final Duration anchorOffset;

  bool get hasStarted => anchorServerTime != null;

  /// Room position at a given **server** instant.
  Duration? positionAt(DateTime serverNow) {
    final anchor = anchorServerTime;
    if (anchor == null) return null;
    return serverNow.difference(anchor) + anchorOffset;
  }

  /// Room position now, using [clock] for the correction.
  ///
  /// Returns null when the room has not started **or** the clock is not yet
  /// synchronised. Both are honest "we do not know" answers, and §3.2 forbids
  /// presenting an unknown as a value.
  Duration? currentPosition(SyncClock clock) {
    final serverNow = clock.serverNow;
    if (serverNow == null) return null;
    return positionAt(serverNow);
  }

  /// The server instant at which the room reaches [position]. Used to schedule
  /// a local timer for an upcoming beat.
  DateTime? serverTimeFor(Duration position) {
    final anchor = anchorServerTime;
    if (anchor == null) return null;
    return anchor.add(position - anchorOffset);
  }

  /// How long from now until the room reaches [position], corrected.
  /// Negative when it has already passed.
  Duration? timeUntil(Duration position, SyncClock clock) {
    final serverNow = clock.serverNow;
    final target = serverTimeFor(position);
    if (serverNow == null || target == null) return null;
    return target.difference(serverNow);
  }

  Timeline reanchor(DateTime at, Duration position) =>
      Timeline(anchorServerTime: at, anchorOffset: position);

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      (other is Timeline &&
          other.anchorServerTime == anchorServerTime &&
          other.anchorOffset == anchorOffset);

  @override
  int get hashCode => Object.hash(anchorServerTime, anchorOffset);
}

/// Outcome of testing whether a timed event may fire (FR-27).
enum BeatDecision {
  /// Not yet. Wait.
  pending,

  /// Within tolerance. Fire now.
  fire,

  /// Past tolerance. Skip and log as drift — **never fire late**.
  skip,
}

/// Decides whether an event targeted at [firesAt] may fire at [current].
///
/// The asymmetry is the product decision, not an oversight: early means wait,
/// late past tolerance means give up. A trivia question about a twist that
/// played twenty seconds ago actively spoils the room, and showing nothing
/// does not.
BeatDecision evaluateBeat(Duration current, Duration firesAt) {
  final delta = current - firesAt;
  if (delta < -SyncConstants.beatTolerance) return BeatDecision.pending;
  if (delta > SyncConstants.beatTolerance) return BeatDecision.skip;
  return BeatDecision.fire;
}

/// How far a user believes they are from the room, for the "I'm out of sync"
/// affordance (FR-25).
///
/// Positive means the user is ahead of the room.
class DriftNudge {
  const DriftNudge(this.amount);

  /// §3.3: a ±5s nudge adjusts only this user's local offset and tells nobody
  /// else. It is a personal correction, not a room event — one person's
  /// buffering must not move everyone.
  static const Duration step = Duration(seconds: 5);

  final Duration amount;

  DriftNudge forward() => DriftNudge(amount + step);
  DriftNudge backward() => DriftNudge(amount - step);

  /// Clamped so a user cannot nudge themselves somewhere absurd and then
  /// report the room as broken.
  static const Duration maxNudge = Duration(minutes: 5);

  DriftNudge clamped() {
    final micros = amount.inMicroseconds.clamp(
      -maxNudge.inMicroseconds,
      maxNudge.inMicroseconds,
    );
    return DriftNudge(Duration(microseconds: micros));
  }

  /// True once the drift is large enough to be worth reporting to the server
  /// for the §7.4 consensus calculation.
  bool get isReportable => amount.abs() >= const Duration(seconds: 8);

  @override
  bool operator ==(Object other) =>
      identical(this, other) || (other is DriftNudge && other.amount == amount);

  @override
  int get hashCode => amount.hashCode;
}

/// Formats a room position as `H:MM:SS` or `M:SS`.
///
/// Digits are Western here by design. Eastern Arabic numerals are a *user
/// setting* (§3.6), applied by the presentation layer via `intl`, not baked
/// into a domain helper — an Arabic speaker may well prefer Western digits.
String formatPosition(Duration position) {
  final total = position.inSeconds;
  final negative = total < 0;
  final abs = total.abs();

  final hours = abs ~/ 3600;
  final minutes = (abs % 3600) ~/ 60;
  final seconds = abs % 60;

  final sign = negative ? '-' : '';
  final ss = seconds.toString().padLeft(2, '0');

  if (hours > 0) {
    final mm = minutes.toString().padLeft(2, '0');
    return '$sign$hours:$mm:$ss';
  }
  return '$sign$minutes:$ss';
}

/// Clamps a value into a range. Small helper kept local so `dart:math` import
/// stays justified.
Duration clampDuration(Duration value, Duration min, Duration max) =>
    Duration(
      microseconds: math.max(
        min.inMicroseconds,
        math.min(max.inMicroseconds, value.inMicroseconds),
      ),
    );
