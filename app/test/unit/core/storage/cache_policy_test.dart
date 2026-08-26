import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/storage/cache_policy.dart';

void main() {
  final now = DateTime.utc(2026, 8, 26, 12);

  CacheVerdict verdictFor(
    Duration age,
    CacheTier tier, {
    bool offline = false,
  }) =>
      evaluateCache(
        cachedAt: now.subtract(age),
        tier: tier,
        now: now,
        isOffline: offline,
      );

  group('tiers', () {
    test('are ordered from longest-lived to shortest', () {
      // The ordering is the whole design. A single TTL has to be tuned to the
      // strictest case, which throws away the benefit for the loosest — a
      // poster and a participant list are both "cached data" and only one of
      // them can be five minutes old without lying.
      expect(
        CacheTier.catalogue.freshFor,
        greaterThan(CacheTier.profile.freshFor),
      );
      expect(
        CacheTier.profile.freshFor,
        greaterThan(CacheTier.room.freshFor),
      );
      expect(CacheTier.ephemeral.freshFor, Duration.zero);
    });

    test('always allow a stale-while-revalidate window', () {
      // usableFor must exceed freshFor, or there is no window in which
      // something can be shown while it refreshes — and the user gets a
      // spinner where they could have had content.
      for (final tier in CacheTier.values) {
        if (!tier.isCacheable) continue;
        expect(
          tier.usableFor,
          greaterThan(tier.freshFor),
          reason: '$tier has no stale-while-revalidate window',
        );
      }
    });

    test('ephemeral is never cacheable', () {
      // Serving a stale scoreboard or countdown is worse than serving nothing,
      // because the user cannot tell the difference and acts on it.
      expect(CacheTier.ephemeral.isCacheable, isFalse);
      expect(CacheTier.ephemeral.usableFor, Duration.zero);
      expect(
        verdictFor(Duration.zero, CacheTier.ephemeral),
        CacheVerdict.expired,
        reason: 'even a zero-age ephemeral entry must not be served',
      );
    });
  });

  group('evaluateCache', () {
    test('reports missing when nothing is cached', () {
      expect(
        evaluateCache(cachedAt: null, tier: CacheTier.room, now: now),
        CacheVerdict.missing,
      );
    });

    test('is fresh inside the window', () {
      expect(
        verdictFor(const Duration(seconds: 10), CacheTier.room),
        CacheVerdict.fresh,
      );
      expect(
        verdictFor(const Duration(hours: 1), CacheTier.catalogue),
        CacheVerdict.fresh,
      );
    });

    test('is stale-but-usable between the two windows', () {
      // 45s is past the room tier's 30s freshness and inside its 5m usable
      // window: show it, marked stale, and refresh behind it.
      expect(
        verdictFor(const Duration(seconds: 45), CacheTier.room),
        CacheVerdict.staleButUsable,
      );
    });

    test('is expired past the usable window', () {
      expect(
        verdictFor(const Duration(minutes: 10), CacheTier.room),
        CacheVerdict.expired,
      );
      expect(
        verdictFor(const Duration(days: 8), CacheTier.catalogue),
        CacheVerdict.expired,
      );
    });

    test('offline widens the usable window to everything', () {
      // Normally expired. Offline, the choice is not "old data or fresh data",
      // it is "old data or an empty screen" — and old data clearly labelled as
      // old wins. FR-60's freshness indicator is what keeps that honest.
      expect(
        verdictFor(const Duration(days: 30), CacheTier.catalogue, offline: true),
        CacheVerdict.staleButUsable,
      );
      expect(
        verdictFor(const Duration(hours: 3), CacheTier.room, offline: true),
        CacheVerdict.staleButUsable,
      );
    });

    test('offline still does not resurrect ephemeral data', () {
      // The one thing offline must NOT do. A stale countdown offline is just
      // as misleading as a stale countdown online.
      expect(
        verdictFor(const Duration(seconds: 1), CacheTier.ephemeral, offline: true),
        CacheVerdict.expired,
      );
    });

    test('treats a backwards clock as expired rather than eternally fresh', () {
      // A device clock can move backwards: a timezone change, an NTP
      // correction, or a user setting the date by hand. Treating a
      // future-dated entry as fresh would pin it forever; expiring it costs
      // one refetch and corrects itself.
      expect(
        evaluateCache(
          cachedAt: now.add(const Duration(days: 1)),
          tier: CacheTier.catalogue,
          now: now,
        ),
        CacheVerdict.expired,
      );
    });

    test('the boundary is inclusive of fresh', () {
      // Exactly at freshFor is stale, not fresh. The direction matters less
      // than it being decided: an ambiguous boundary is where an
      // off-by-one-second flake lives.
      expect(
        verdictFor(CacheTier.room.freshFor, CacheTier.room),
        CacheVerdict.staleButUsable,
      );
      expect(
        verdictFor(
          CacheTier.room.freshFor - const Duration(milliseconds: 1),
          CacheTier.room,
        ),
        CacheVerdict.fresh,
      );
    });
  });

  group('Cached', () {
    test('carries its own age so the indicator cannot disagree', () {
      // FR-60 requires the UI to show how old the data is. A freshness
      // indicator fed by a SEPARATE query is one that will eventually disagree
      // with the data beside it.
      final cached = Cached<String>(
        value: 'x',
        cachedAt: now.subtract(const Duration(minutes: 3)),
        verdict: CacheVerdict.staleButUsable,
      );
      expect(cached.ageAt(now), const Duration(minutes: 3));
      expect(cached.isStale, isTrue);
    });

    test('never reports a negative age', () {
      // A backwards clock must not render as "updated in -4 minutes".
      final cached = Cached<String>(
        value: 'x',
        cachedAt: now.add(const Duration(minutes: 4)),
        verdict: CacheVerdict.fresh,
      );
      expect(cached.ageAt(now), Duration.zero);
    });
  });
}
