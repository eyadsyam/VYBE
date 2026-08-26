/// Cache tiers and freshness (§11.1–11.2).
///
/// Pure Dart, no Drift. The rules about what may be served stale and for how
/// long are the part worth reasoning about, and keeping them out of the
/// database layer means they can be tested without opening a file.
///
/// The governing idea is that "cached" is not one thing. A poster URL and a
/// room's participant list are both cached data, and serving a five-minute-old
/// poster is fine while serving a five-minute-old participant list is a lie.
/// A single TTL for everything has to be tuned to the strictest case, which
/// throws away the whole benefit for the loosest.
library;

/// How long a kind of data may be served from cache.
enum CacheTier {
  /// Catalogue metadata: titles, posters, synopses.
  ///
  /// Long-lived because it genuinely is: a film's synopsis does not change
  /// between Tuesday and Thursday. This is what makes the app usable on a
  /// train — the browse experience survives with no network at all.
  catalogue,

  /// The user's own profile and entitlements.
  ///
  /// Medium-lived. A tier change matters, but not within a minute, and the
  /// server refuses over-capacity actions regardless of what the client
  /// believes.
  profile,

  /// Room membership and state.
  ///
  /// Short-lived, and only ever a starting point. The socket is the source of
  /// truth once connected; this exists so a room screen has something to
  /// render in the moment between opening it and the socket's first frame.
  room,

  /// Never cached.
  ///
  /// Anything whose staleness would be actively misleading: a live scoreboard,
  /// a countdown, a clock offset. Serving these stale is worse than serving
  /// nothing, because the user cannot tell the difference and acts on them.
  ephemeral;

  /// How long data of this tier stays fresh.
  Duration get freshFor => switch (this) {
        CacheTier.catalogue => const Duration(hours: 24),
        CacheTier.profile => const Duration(minutes: 15),
        CacheTier.room => const Duration(seconds: 30),
        CacheTier.ephemeral => Duration.zero,
      };

  /// How long data of this tier may still be SHOWN while refreshing behind it.
  ///
  /// The gap between [freshFor] and this is the stale-while-revalidate window:
  /// the user sees something immediately, marked as stale, while a refresh runs.
  /// An app that showed a spinner instead would be blank for the same duration
  /// and less useful.
  Duration get usableFor => switch (this) {
        CacheTier.catalogue => const Duration(days: 7),
        CacheTier.profile => const Duration(days: 1),
        CacheTier.room => const Duration(minutes: 5),
        CacheTier.ephemeral => Duration.zero,
      };

  /// Whether this tier is cached at all.
  bool get isCacheable => this != CacheTier.ephemeral;
}

/// What to do with a cache entry.
enum CacheVerdict {
  /// Serve it, no refresh needed.
  fresh,

  /// Serve it AND refresh in the background. §11.2's stale-while-revalidate.
  staleButUsable,

  /// Do not serve it. Fetch first.
  expired,

  /// Nothing cached.
  missing,
}

/// Decides what to do with a cached entry.
///
/// [isOffline] widens the usable window to infinity, deliberately. A
/// three-week-old catalogue entry is normally expired, but when there is no
/// network the choice is not "old data or fresh data" — it is "old data or an
/// empty screen", and old data clearly labelled as old wins. FR-60's freshness
/// indicator is what makes that honest rather than deceptive.
CacheVerdict evaluateCache({
  required DateTime? cachedAt,
  required CacheTier tier,
  required DateTime now,
  bool isOffline = false,
}) {
  if (cachedAt == null) return CacheVerdict.missing;
  if (!tier.isCacheable) return CacheVerdict.expired;

  final age = now.difference(cachedAt);

  // A negative age means the device clock moved backwards — a timezone change,
  // an NTP correction, or a user setting the date by hand. Treating the entry
  // as fresh would pin it forever; treating it as expired refetches once and
  // corrects itself.
  if (age.isNegative) return CacheVerdict.expired;

  if (age < tier.freshFor) return CacheVerdict.fresh;
  if (isOffline) return CacheVerdict.staleButUsable;
  if (age < tier.usableFor) return CacheVerdict.staleButUsable;
  return CacheVerdict.expired;
}

/// A cached value and how old it is.
///
/// The age travels WITH the value rather than being looked up separately,
/// because FR-60 requires the UI to show it — and a freshness indicator fed by
/// a second query is a freshness indicator that will eventually disagree with
/// the data beside it.
class Cached<T> {
  const Cached({
    required this.value,
    required this.cachedAt,
    required this.verdict,
  });

  final T value;
  final DateTime cachedAt;
  final CacheVerdict verdict;

  bool get isStale => verdict == CacheVerdict.staleButUsable;

  /// How old the data is, against [now].
  Duration ageAt(DateTime now) {
    final age = now.difference(cachedAt);
    return age.isNegative ? Duration.zero : age;
  }
}
