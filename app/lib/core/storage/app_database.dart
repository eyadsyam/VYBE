/// The local database (§11.1).
///
/// Drift over sqlite. What goes in here is the deliberate part: **cached
/// server state and nothing else**. No credentials — those live in the
/// keystore (§12.2), because a sqlite file is readable by anything with root
/// and is included in device backups. No derived state that could be
/// recomputed, because a stale derivation is harder to spot than stale source
/// data.
library;

import 'dart:convert';
import 'dart:io';

import 'package:drift/drift.dart';
import 'package:drift/native.dart';
import 'package:path/path.dart' as p;
import 'package:path_provider/path_provider.dart';

import 'cache_policy.dart';

part 'app_database.g.dart';

/// Cached catalogue entries (§11.1).
///
/// Arabic and English titles are separate COLUMNS rather than separate rows,
/// because a MENA title frequently has both — a series with an Arabic name and
/// a Latin transliteration — and users search with either. Modelling it as one
/// localised row per language would make "find this title in any language" a
/// join instead of a column read.
class CachedContent extends Table {
  TextColumn get id => text()();
  TextColumn get contentType => text()();
  TextColumn get title => text()();
  TextColumn get titleAr => text().nullable()();
  TextColumn get synopsis => text().nullable()();
  TextColumn get synopsisAr => text().nullable()();
  TextColumn get posterPath => text().nullable()();
  IntColumn get runtimeMinutes => integer().nullable()();
  DateTimeColumn get releaseDate => dateTime().nullable()();

  /// When this row was written. Everything freshness-related reads from here.
  DateTimeColumn get cachedAt => dateTime()();

  @override
  Set<Column<Object>> get primaryKey => {id};
}

/// Cached rooms, so the list renders instantly on launch.
///
/// The full payload is kept as JSON alongside the queryable columns. That is
/// not laziness: the room shape changes with the API, and a schema migration
/// for every added field would mean the cache is unusable on the first launch
/// after an update — exactly when a user is most likely to be on a bad
/// network. The columns exist for sorting and filtering; the JSON is the
/// truth.
class CachedRooms extends Table {
  TextColumn get id => text()();
  TextColumn get state => text()();
  TextColumn get hostUserId => text()();
  TextColumn get title => text().nullable()();
  DateTimeColumn get createdAt => dateTime()();
  TextColumn get payload => text()();
  DateTimeColumn get cachedAt => dateTime()();

  @override
  Set<Column<Object>> get primaryKey => {id};
}

/// The user's own profile.
///
/// One row, always id 'me'. A table rather than a key-value blob so the
/// columns are typed, and singular because caching somebody ELSE's profile
/// here would quietly turn a private field into shared state.
class CachedProfile extends Table {
  TextColumn get id => text()();
  TextColumn get handle => text()();
  TextColumn get displayName => text()();
  TextColumn get locale => text()();
  TextColumn get entitlementTier => text()();
  TextColumn get numeralSystem => text()();
  DateTimeColumn get cachedAt => dateTime()();

  @override
  Set<Column<Object>> get primaryKey => {id};
}

@DriftDatabase(tables: [CachedContent, CachedRooms, CachedProfile])
class AppDatabase extends _$AppDatabase {
  AppDatabase([QueryExecutor? executor]) : super(executor ?? _open());

  /// An in-memory database, for tests.
  AppDatabase.memory() : super(NativeDatabase.memory());

  @override
  int get schemaVersion => 1;

  @override
  MigrationStrategy get migration => MigrationStrategy(
        onCreate: (m) => m.createAll(),
        onUpgrade: (m, from, to) async {
          // Deliberately empty at v1, and deliberately not a silent no-op
          // forever: every future version needs a case here. The safe fallback
          // for a CACHE is to drop and rebuild — unlike the server's data,
          // nothing here is irreplaceable, and a wrong migration on a cache is
          // a bug that costs one refetch rather than a user's history.
          await m.createAll();
        },
        beforeOpen: (details) async {
          // Foreign keys are OFF by default in sqlite and have to be enabled
          // per connection. There are no FKs in this schema yet, but enabling
          // it now means the first one added actually works.
          await customStatement('PRAGMA foreign_keys = ON');
        },
      );

  // -------------------------------------------------------------------------
  // Rooms
  // -------------------------------------------------------------------------

  /// Replaces the cached room list.
  ///
  /// A transaction with a delete-then-insert rather than an upsert, because
  /// the list is a SNAPSHOT: a room the server no longer returns is a room the
  /// user has left or that has ended, and leaving it behind would show a
  /// ghost. Upserting would only ever add.
  Future<void> replaceRooms(
    List<CachedRoomsCompanion> rooms, {
    required DateTime now,
  }) {
    return transaction(() async {
      await delete(cachedRooms).go();
      await batch((b) => b.insertAll(cachedRooms, rooms));
    });
  }

  /// The cached rooms, newest first, with a freshness verdict.
  Future<List<Cached<Map<String, dynamic>>>> cachedRoomList({
    required DateTime now,
    bool isOffline = false,
  }) async {
    final rows = await (select(cachedRooms)
          ..orderBy([(t) => OrderingTerm.desc(t.createdAt)]))
        .get();

    return rows
        .map((row) {
          final verdict = evaluateCache(
            cachedAt: row.cachedAt,
            tier: CacheTier.room,
            now: now,
            isOffline: isOffline,
          );
          if (verdict == CacheVerdict.expired ||
              verdict == CacheVerdict.missing) {
            return null;
          }
          // A corrupt row is SKIPPED, not thrown on. jsonDecode throws on
          // malformed input, and a cache is not worth crashing over: a bad row
          // costs one refetch, while an exception here happens on launch and
          // costs the whole app until the user reinstalls.
          //
          // Rows can genuinely go bad — a truncated write during a low-battery
          // shutdown, or a payload written by a newer version of the app that
          // was then downgraded.
          try {
            final decoded = jsonDecode(row.payload);
            if (decoded is! Map) return null;
            return Cached<Map<String, dynamic>>(
              value: Map<String, dynamic>.from(decoded),
              cachedAt: row.cachedAt,
              verdict: verdict,
            );
          } on FormatException {
            return null;
          }
        })
        .whereType<Cached<Map<String, dynamic>>>()
        .toList();
  }

  // -------------------------------------------------------------------------
  // Maintenance
  // -------------------------------------------------------------------------

  /// Deletes everything past its usable window.
  ///
  /// Run on launch. Without it the catalogue table grows without bound: every
  /// title a user has ever scrolled past stays forever, and the cache becomes
  /// a slow leak that only shows up as disk pressure months later.
  Future<int> evictExpired(DateTime now) async {
    var removed = 0;

    removed += await (delete(cachedContent)
          ..where((t) =>
              t.cachedAt.isSmallerThanValue(
                now.subtract(CacheTier.catalogue.usableFor),
              )))
        .go();

    removed += await (delete(cachedRooms)
          ..where((t) => t.cachedAt.isSmallerThanValue(
                now.subtract(CacheTier.room.usableFor),
              )))
        .go();

    return removed;
  }

  /// Clears everything.
  ///
  /// Called on sign-out. Leaving cached rooms behind would show the NEXT user
  /// of the device the previous user's watch parties — which is a privacy
  /// failure, not an inconvenience.
  Future<void> clearAll() {
    return transaction(() async {
      await delete(cachedContent).go();
      await delete(cachedRooms).go();
      await delete(cachedProfile).go();
    });
  }
}

/// Opens the on-disk database.
LazyDatabase _open() {
  return LazyDatabase(() async {
    final dir = await getApplicationDocumentsDirectory();
    final file = File(p.join(dir.path, 'vybe_cache.sqlite'));
    return NativeDatabase.createInBackground(file);
  });
}
