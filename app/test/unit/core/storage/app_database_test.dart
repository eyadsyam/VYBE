import 'dart:convert';

import 'package:drift/drift.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/storage/app_database.dart';
import 'package:vybe/core/storage/cache_policy.dart';

void main() {
  late AppDatabase db;
  final now = DateTime.utc(2026, 8, 26, 12);

  setUp(() => db = AppDatabase.memory());
  tearDown(() => db.close());

  CachedRoomsCompanion room(
    String id, {
    String state = 'LOBBY',
    DateTime? cachedAt,
    DateTime? createdAt,
  }) =>
      CachedRoomsCompanion.insert(
        id: id,
        state: state,
        hostUserId: 'host-1',
        createdAt: createdAt ?? now,
        payload: jsonEncode({'id': id, 'state': state, 'currentSeq': 1}),
        cachedAt: cachedAt ?? now,
      );

  group('schema', () {
    test('opens and creates every table', () async {
      // A migration that does not run is a database that fails on first write,
      // in production, on somebody's phone.
      await db.into(db.cachedRooms).insert(room('r1'));
      await db.into(db.cachedProfile).insert(
            CachedProfileCompanion.insert(
              id: 'me',
              handle: 'sara_q',
              displayName: 'سارة',
              locale: 'ar',
              entitlementTier: 'free',
              numeralSystem: 'western',
              cachedAt: now,
            ),
          );
      await db.into(db.cachedContent).insert(
            CachedContentCompanion.insert(
              id: 'c1',
              contentType: 'movie',
              title: 'The Choice',
              titleAr: const Value('الاختيار'),
              cachedAt: now,
            ),
          );

      expect(await db.select(db.cachedRooms).get(), hasLength(1));
      expect(await db.select(db.cachedProfile).get(), hasLength(1));
      expect(await db.select(db.cachedContent).get(), hasLength(1));
    });

    test('round-trips Arabic without mangling it', () async {
      // sqlite is UTF-8, but an encoding mistake anywhere in the chain shows up
      // here rather than on a user's screen.
      await db.into(db.cachedContent).insert(
            CachedContentCompanion.insert(
              id: 'c1',
              contentType: 'series',
              title: 'Ma Waraa Al Tabiaa',
              titleAr: const Value('ما وراء الطبيعة'),
              synopsisAr: const Value('مسلسل رعب نفسي'),
              cachedAt: now,
            ),
          );

      final row = await db.select(db.cachedContent).getSingle();
      expect(row.titleAr, 'ما وراء الطبيعة');
      expect(row.synopsisAr, 'مسلسل رعب نفسي');
    });
  });

  group('replaceRooms', () {
    test('replaces rather than merges', () async {
      // The list is a SNAPSHOT: a room the server no longer returns is one the
      // user left or that ended. Upserting would only ever add, leaving a
      // ghost the user can tap.
      await db.replaceRooms([room('r1'), room('r2')], now: now);
      expect(await db.select(db.cachedRooms).get(), hasLength(2));

      await db.replaceRooms([room('r2'), room('r3')], now: now);
      final ids = (await db.select(db.cachedRooms).get()).map((r) => r.id);
      expect(ids, containsAll(['r2', 'r3']));
      expect(ids, isNot(contains('r1')));
    });

    test('is atomic', () async {
      // A delete that committed without its inserts would leave the user with
      // an empty list where they had rooms a moment ago.
      await db.replaceRooms([room('r1')], now: now);
      expect(await db.select(db.cachedRooms).get(), hasLength(1));

      await expectLater(
        db.replaceRooms([room('r2'), room('r2')], now: now),
        throwsA(anything),
        reason: 'a duplicate primary key must fail the whole batch',
      );

      // The original row survives, because the transaction rolled back.
      final rows = await db.select(db.cachedRooms).get();
      expect(rows, hasLength(1));
      expect(rows.single.id, 'r1');
    });
  });

  group('cachedRoomList', () {
    test('returns fresh rooms newest first', () async {
      await db.replaceRooms([
        room('older', createdAt: now.subtract(const Duration(hours: 2))),
        room('newer', createdAt: now.subtract(const Duration(minutes: 5))),
      ], now: now);

      final cached = await db.cachedRoomList(now: now);
      expect(cached.map((c) => c.value['id']), ['newer', 'older']);
      expect(cached.every((c) => c.verdict == CacheVerdict.fresh), isTrue);
    });

    test('omits rooms past the usable window', () async {
      await db.replaceRooms([
        room('current'),
        room('ancient', cachedAt: now.subtract(const Duration(hours: 3))),
      ], now: now);

      final cached = await db.cachedRoomList(now: now);
      expect(cached.map((c) => c.value['id']), ['current']);
    });

    test('marks stale rooms rather than hiding them', () async {
      // 45s is past the room tier's 30s freshness and inside its 5m usable
      // window: the user sees the list immediately, marked as stale, while a
      // refresh runs behind it.
      await db.replaceRooms(
        [room('r1', cachedAt: now.subtract(const Duration(seconds: 45)))],
        now: now,
      );

      final cached = await db.cachedRoomList(now: now);
      expect(cached, hasLength(1));
      expect(cached.single.isStale, isTrue);
      expect(cached.single.ageAt(now), const Duration(seconds: 45));
    });

    test('serves everything when offline', () async {
      // Offline the choice is old data or an empty screen.
      await db.replaceRooms(
        [room('r1', cachedAt: now.subtract(const Duration(days: 2)))],
        now: now,
      );

      expect(await db.cachedRoomList(now: now), isEmpty);
      final offline = await db.cachedRoomList(now: now, isOffline: true);
      expect(offline, hasLength(1));
      expect(offline.single.isStale, isTrue);
    });

    test('skips a row whose payload is corrupt instead of throwing', () async {
      // A cache is not worth crashing over. A corrupt row costs one refetch;
      // an exception on launch costs the whole app.
      await db.into(db.cachedRooms).insert(
            CachedRoomsCompanion.insert(
              id: 'broken',
              state: 'LOBBY',
              hostUserId: 'h',
              createdAt: now,
              payload: 'not json at all',
              cachedAt: now,
            ),
          );
      await db.into(db.cachedRooms).insert(room('good'));

      final cached = await db.cachedRoomList(now: now);
      expect(cached.map((c) => c.value['id']), ['good']);
    });
  });

  group('evictExpired', () {
    test('removes what is past its usable window and keeps the rest', () async {
      // Without eviction the catalogue grows without bound: every title a user
      // has ever scrolled past stays forever, and the cache becomes a slow
      // leak that shows up as disk pressure months later.
      await db.into(db.cachedContent).insert(
            CachedContentCompanion.insert(
              id: 'stale',
              contentType: 'movie',
              title: 'Old',
              cachedAt: now.subtract(const Duration(days: 30)),
            ),
          );
      await db.into(db.cachedContent).insert(
            CachedContentCompanion.insert(
              id: 'current',
              contentType: 'movie',
              title: 'New',
              cachedAt: now,
            ),
          );
      await db.replaceRooms([
        room('live'),
        room('dead', cachedAt: now.subtract(const Duration(hours: 1))),
      ], now: now);

      final removed = await db.evictExpired(now);
      expect(removed, 2);

      final content = await db.select(db.cachedContent).get();
      expect(content.map((c) => c.id), ['current']);
      final rooms = await db.select(db.cachedRooms).get();
      expect(rooms.map((r) => r.id), ['live']);
    });
  });

  group('clearAll', () {
    test('leaves nothing behind on sign-out', () async {
      // The next user of the device must not see the previous user's watch
      // parties. That is a privacy failure, not an inconvenience.
      await db.replaceRooms([room('r1')], now: now);
      await db.into(db.cachedProfile).insert(
            CachedProfileCompanion.insert(
              id: 'me',
              handle: 'sara_q',
              displayName: 'Sara',
              locale: 'en',
              entitlementTier: 'free',
              numeralSystem: 'western',
              cachedAt: now,
            ),
          );
      await db.into(db.cachedContent).insert(
            CachedContentCompanion.insert(
              id: 'c1',
              contentType: 'movie',
              title: 'X',
              cachedAt: now,
            ),
          );

      await db.clearAll();

      expect(await db.select(db.cachedRooms).get(), isEmpty);
      expect(await db.select(db.cachedProfile).get(), isEmpty);
      expect(await db.select(db.cachedContent).get(), isEmpty);
    });
  });

  group('what is deliberately absent', () {
    test('there is no table that could hold a credential', () async {
      // §12.2 puts tokens in the platform keystore, never here: a sqlite file
      // is readable by anything with root and is included in device backups.
      // This asserts the SHAPE of the schema, so adding a token column becomes
      // a deliberate act with a failing test attached.
      final tables = db.allTables.map((t) => t.actualTableName).toList();
      expect(tables, hasLength(3));

      for (final table in db.allTables) {
        for (final column in table.$columns) {
          final name = column.name.toLowerCase();
          for (final forbidden in [
            'token',
            'password',
            'secret',
            'refresh',
            'credential',
          ]) {
            expect(
              name.contains(forbidden),
              isFalse,
              reason: '${table.actualTableName}.${column.name} looks like a '
                  'credential; §12.2 keeps those in the keystore',
            );
          }
        }
      }
    });
  });
}
