import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/realtime/sync_clock.dart';

/// Client-side mirror of the server's timeline tests. The same acceptance
/// criteria are asserted on both sides on purpose: a correction applied on one
/// side only would still let the two disagree, and the whole product is that
/// they agree.
void main() {
  final base = DateTime.utc(2026, 8, 25, 20, 0, 0);

  /// Builds a sample with an exact RTT and an exact device-to-server offset,
  /// so tests can assert *which* sample was selected rather than merely that
  /// the number looks plausible.
  ClockSample sample({required Duration rtt, required Duration offset}) {
    final t0 = base;
    final t1 = t0.add(offset).add(rtt ~/ 2);
    return ClockSample(t0: t0, t1: t1, t2: t1, t3: t0.add(rtt));
  }

  group('ClockSample', () {
    test('RTT excludes server processing time', () {
      final s = ClockSample(
        t0: base,
        t1: base.add(const Duration(milliseconds: 20)),
        t2: base.add(const Duration(milliseconds: 120)), // 100ms of server work
        t3: base.add(const Duration(milliseconds: 140)),
      );
      expect(
        s.rtt,
        const Duration(milliseconds: 40),
        reason: '140ms wall clock minus 100ms server time; otherwise a slow '
            'handler masquerades as a slow network',
      );
    });

    test('negative RTT is unusable', () {
      final s = ClockSample(
        t0: base,
        t1: base.subtract(const Duration(seconds: 5)),
        t2: base.add(const Duration(seconds: 5)),
        t3: base.add(const Duration(milliseconds: 10)),
      );
      expect(s.isUsable, isFalse);
    });

    test('RTT above the ceiling is unusable', () {
      expect(
        sample(rtt: const Duration(milliseconds: 2001), offset: Duration.zero).isUsable,
        isFalse,
      );
      expect(
        sample(rtt: const Duration(seconds: 2), offset: Duration.zero).isUsable,
        isTrue,
        reason: 'exactly at the ceiling is still acceptable',
      );
    });
  });

  group('SyncClock', () {
    // AC-3, verbatim: RTTs of 40, 45, 800, 60, 50 must select the 40ms sample.
    test('AC-3: selects the lowest-RTT sample, never the mean', () {
      final clock = SyncClock(now: () => base);

      // Distinguishable offsets, so we can prove which sample won.
      clock.observe(sample(rtt: const Duration(milliseconds: 40), offset: const Duration(seconds: 1)));
      clock.observe(sample(rtt: const Duration(milliseconds: 45), offset: const Duration(milliseconds: 1200)));
      clock.observe(sample(rtt: const Duration(milliseconds: 800), offset: const Duration(seconds: 9)));
      clock.observe(sample(rtt: const Duration(milliseconds: 60), offset: const Duration(milliseconds: 1300)));
      clock.observe(sample(rtt: const Duration(milliseconds: 50), offset: const Duration(milliseconds: 1100)));

      expect(clock.bestSample!.rtt, const Duration(milliseconds: 40));
      expect(clock.offset!.inMilliseconds, closeTo(1000, 1));

      // Guard against a future "improvement" to averaging: the mean of those
      // five offsets is ~2.72s.
      expect(
        clock.offset!.inMilliseconds,
        isNot(closeTo(2720, 100)),
        reason: 'offset looks like the mean; §7.4 requires lowest-RTT selection',
      );
    });

    // AC-4.
    test('AC-4: a 2.5s RTT sample is discarded and marks the connection degraded', () {
      final clock = SyncClock(now: () => base);
      clock.observe(sample(rtt: const Duration(milliseconds: 2500), offset: const Duration(seconds: 3)));

      expect(clock.bestSample, isNull);
      expect(clock.isDegraded, isTrue);
      expect(clock.isSynchronised, isFalse);
      expect(clock.serverNow, isNull);

      clock.observe(sample(rtt: const Duration(milliseconds: 50), offset: const Duration(milliseconds: 250)));
      expect(clock.isDegraded, isFalse);
      expect(clock.offset!.inMilliseconds, closeTo(250, 1));
    });

    test('an unmeasured clock is not degraded — it is simply not measured yet', () {
      final clock = SyncClock(now: () => base);
      expect(clock.isDegraded, isFalse);
      expect(clock.isSynchronised, isFalse);
    });

    test('refuses to fall back to the device clock when unsynchronised', () {
      final clock = SyncClock(now: () => base);
      expect(
        clock.serverNow,
        isNull,
        reason: 'silently substituting an uncorrected clock is how a 5-minute '
            'device error becomes a 5-minute timeline error, unnoticed',
      );
    });

    test('the sample window is bounded', () {
      final clock = SyncClock(now: () => base);
      for (var i = 0; i < 50; i++) {
        clock.observe(sample(rtt: Duration(milliseconds: 10 + i), offset: const Duration(seconds: 1)));
      }
      expect(clock.sampleCount, SyncConstants.sampleWindow);
    });

    test('reset clears the window on reconnect', () {
      final clock = SyncClock(now: () => base);
      clock.observe(sample(rtt: const Duration(milliseconds: 40), offset: const Duration(seconds: 1)));
      expect(clock.isSynchronised, isTrue);

      clock.reset();
      expect(clock.sampleCount, 0);
      expect(clock.isSynchronised, isFalse);
      expect(clock.isDegraded, isFalse);
    });
  });

  group('Timeline', () {
    // AC-2 — the headline claim of ADR-002 and interview answer §16.4.2.
    test('AC-2: a device clock 5 minutes wrong still reads the correct position', () {
      const skew = Duration(minutes: -5);

      // The true server instant: 90 seconds into the programme.
      final trueServerNow = base.add(const Duration(seconds: 90));

      // The device's own (wrong) reading of that same instant.
      final deviceNow = trueServerNow.add(skew);

      // A PING/PONG against that same wrong clock measures exactly -skew.
      final clock = SyncClock(now: () => deviceNow);
      clock.observe(ClockSample(
        t0: deviceNow,
        t1: trueServerNow,
        t2: trueServerNow,
        t3: deviceNow,
      ));

      const timeline = Timeline(anchorServerTime: null);
      expect(timeline.hasStarted, isFalse);

      final started = Timeline(anchorServerTime: base);
      final position = started.currentPosition(clock);

      expect(
        position,
        const Duration(seconds: 90),
        reason: 'the offset absorbs the device clock error entirely',
      );

      // And prove the correction is doing real work: without it, the position
      // would be catastrophically wrong.
      final uncorrected = started.positionAt(deviceNow);
      expect(
        uncorrected,
        isNot(const Duration(seconds: 90)),
        reason: 'if this matched, the test would not be exercising the correction',
      );
    });

    // AC-1's convergence requirement, at the unit level.
    test('AC-1: devices with wildly different clock errors agree within 250ms', () {
      final trueServerNow = base.add(const Duration(seconds: 42));
      final timeline = Timeline(anchorServerTime: base);

      const skews = <Duration>[
        Duration(minutes: -5),
        Duration(hours: 3),
        Duration(milliseconds: -37),
        Duration.zero,
      ];

      Duration? first;
      for (final skew in skews) {
        final deviceNow = trueServerNow.add(skew);
        final clock = SyncClock(now: () => deviceNow)
          ..observe(ClockSample(
            t0: deviceNow,
            t1: trueServerNow,
            t2: trueServerNow,
            t3: deviceNow,
          ));

        final pos = timeline.currentPosition(clock)!;
        first ??= pos;
        expect(
          (pos - first).abs().inMilliseconds,
          lessThanOrEqualTo(250),
          reason: 'device skewed by $skew diverged from the first device',
        );
      }
    });

    test('an unstarted room has no position — not zero', () {
      const timeline = Timeline.notStarted();
      expect(timeline.hasStarted, isFalse);
      expect(
        timeline.positionAt(base),
        isNull,
        reason: 'returning 0 would read as "at the very beginning", a different claim',
      );
      expect(timeline.serverTimeFor(const Duration(seconds: 30)), isNull);
    });

    test('anchorOffset starts the room part-way into a programme', () {
      final timeline = Timeline(
        anchorServerTime: base,
        anchorOffset: const Duration(minutes: 10),
      );
      expect(
        timeline.positionAt(base.add(const Duration(seconds: 30))),
        const Duration(minutes: 10, seconds: 30),
      );
    });

    test('serverTimeFor inverts positionAt', () {
      final timeline = Timeline(
        anchorServerTime: base,
        anchorOffset: const Duration(minutes: 2),
      );
      for (final pos in const [
        Duration.zero,
        Duration(minutes: 2),
        Duration(minutes: 17),
        Duration(minutes: 90),
      ]) {
        final at = timeline.serverTimeFor(pos)!;
        expect(timeline.positionAt(at), pos);
      }
    });

    // AC-6.
    test('AC-6: re-anchoring converges every client regardless of clock error', () {
      final reanchorAt = base.add(const Duration(minutes: 10));
      const truePosition = Duration(minutes: 8);

      final timeline = Timeline(anchorServerTime: base).reanchor(reanchorAt, truePosition);
      final trueServerNow = reanchorAt.add(const Duration(seconds: 15));

      for (final skew in const [
        Duration(minutes: -5),
        Duration(hours: 2),
        Duration.zero,
        Duration(milliseconds: -800),
      ]) {
        final deviceNow = trueServerNow.add(skew);
        final clock = SyncClock(now: () => deviceNow)
          ..observe(ClockSample(t0: deviceNow, t1: trueServerNow, t2: trueServerNow, t3: deviceNow));

        expect(
          timeline.currentPosition(clock),
          truePosition + const Duration(seconds: 15),
          reason: 'skew $skew failed to converge after re-anchor',
        );
      }
    });

    test('timeUntil is negative once the position has passed', () {
      final trueServerNow = base.add(const Duration(seconds: 60));
      final clock = SyncClock(now: () => trueServerNow)
        ..observe(ClockSample(t0: trueServerNow, t1: trueServerNow, t2: trueServerNow, t3: trueServerNow));

      final timeline = Timeline(anchorServerTime: base);

      expect(timeline.timeUntil(const Duration(seconds: 90), clock), const Duration(seconds: 30));
      expect(timeline.timeUntil(const Duration(seconds: 30), clock), const Duration(seconds: -30));
    });

    test('position is unavailable while the clock is unsynchronised', () {
      final clock = SyncClock(now: () => base);
      final timeline = Timeline(anchorServerTime: base);
      expect(timeline.currentPosition(clock), isNull);
      expect(timeline.timeUntil(const Duration(seconds: 10), clock), isNull);
    });

    test('equality is by value', () {
      expect(Timeline(anchorServerTime: base), equals(Timeline(anchorServerTime: base)));
      expect(
        Timeline(anchorServerTime: base).hashCode,
        Timeline(anchorServerTime: base).hashCode,
      );
    });
  });

  group('evaluateBeat', () {
    const target = Duration(seconds: 60);

    test('AC-5: 2 seconds late is skipped, never fired', () {
      expect(evaluateBeat(const Duration(seconds: 62), target), BeatDecision.skip);
    });

    test('tolerance boundaries are exact', () {
      expect(evaluateBeat(target - SyncConstants.beatTolerance, target), BeatDecision.fire);
      expect(evaluateBeat(target, target), BeatDecision.fire);
      expect(evaluateBeat(target + SyncConstants.beatTolerance, target), BeatDecision.fire);

      expect(
        evaluateBeat(target - SyncConstants.beatTolerance - const Duration(milliseconds: 1), target),
        BeatDecision.pending,
      );
      expect(
        evaluateBeat(target + SyncConstants.beatTolerance + const Duration(milliseconds: 1), target),
        BeatDecision.skip,
      );
    });

    test('a late beat never fires, at any lateness', () {
      for (var ms = 1501; ms < 300000; ms += 977) {
        expect(
          evaluateBeat(target + Duration(milliseconds: ms), target),
          BeatDecision.skip,
          reason: '${ms}ms late must skip; firing spoils a room that has moved on',
        );
      }
    });
  });

  group('DriftNudge', () {
    test('nudges move in 5-second steps', () {
      const n = DriftNudge(Duration.zero);
      expect(n.forward().amount, const Duration(seconds: 5));
      expect(n.backward().amount, const Duration(seconds: -5));
      expect(n.forward().forward().amount, const Duration(seconds: 10));
    });

    test('clamping prevents an absurd self-inflicted offset', () {
      expect(const DriftNudge(Duration(hours: 1)).clamped().amount, DriftNudge.maxNudge);
      expect(const DriftNudge(Duration(hours: -1)).clamped().amount, -DriftNudge.maxNudge);
      expect(const DriftNudge(Duration(seconds: 5)).clamped().amount, const Duration(seconds: 5));
    });

    test('only drift of 8s or more is worth reporting for consensus', () {
      expect(const DriftNudge(Duration(seconds: 7, milliseconds: 999)).isReportable, isFalse);
      expect(const DriftNudge(Duration(seconds: 8)).isReportable, isTrue);
      expect(const DriftNudge(Duration(seconds: -8)).isReportable, isTrue);
      expect(const DriftNudge(Duration(seconds: -20)).isReportable, isTrue);
    });
  });

  group('formatPosition', () {
    test('formats under an hour as M:SS', () {
      expect(formatPosition(Duration.zero), '0:00');
      expect(formatPosition(const Duration(seconds: 7)), '0:07');
      expect(formatPosition(const Duration(minutes: 12, seconds: 5)), '12:05');
    });

    test('formats an hour or more as H:MM:SS', () {
      expect(formatPosition(const Duration(hours: 1, minutes: 2, seconds: 3)), '1:02:03');
      expect(formatPosition(const Duration(hours: 2)), '2:00:00');
    });

    test('negative positions are signed, for pre-roll countdowns', () {
      expect(formatPosition(const Duration(seconds: -5)), '-0:05');
      expect(formatPosition(const Duration(minutes: -3, seconds: -20)), '-3:20');
    });
  });

  group('clampDuration', () {
    test('clamps to the range', () {
      const min = Duration(seconds: 1);
      const max = Duration(seconds: 10);
      expect(clampDuration(Duration.zero, min, max), min);
      expect(clampDuration(const Duration(seconds: 5), min, max), const Duration(seconds: 5));
      expect(clampDuration(const Duration(minutes: 1), min, max), max);
    });
  });
}
