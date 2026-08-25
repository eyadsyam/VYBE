package db

import (
	"strings"
	"testing"
)

// These tests need no database. They exercise the parts of the migrator that
// would otherwise only be discovered to be wrong during a deploy: version
// contiguity, checksum drift, and the requirement that every migration is
// reversible.

func TestLoadMigrations_EmbedsTheRealSchema(t *testing.T) {
	got, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations(): %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no migrations loaded — the go:embed pattern is not matching")
	}

	for i, m := range got {
		if m.Version != i+1 {
			t.Fatalf("migration %d has version %d; versions must be contiguous from 1", i, m.Version)
		}
		if m.Name == "" {
			t.Fatalf("migration %04d has an empty name", m.Version)
		}
		if strings.TrimSpace(m.UpSQL) == "" {
			t.Fatalf("migration %04d_%s has an empty up script", m.Version, m.Name)
		}
		// §5.4: reversible. LoadMigrations errors on a missing .down.sql, so
		// reaching here already proves one exists; assert it is not a stub.
		if strings.TrimSpace(m.DownSQL) == "" {
			t.Fatalf("migration %04d_%s has an empty down script", m.Version, m.Name)
		}
		if len(m.Checksum) != 64 {
			t.Fatalf("migration %04d_%s checksum is not a SHA-256 hex digest: %q",
				m.Version, m.Name, m.Checksum)
		}
	}
}

// Every migration is loaded twice and must hash identically. If this is ever
// flaky, the checksum guard is worthless and would reject valid deploys.
func TestLoadMigrations_ChecksumIsStable(t *testing.T) {
	first, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("load count differs between calls: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Checksum != second[i].Checksum {
			t.Fatalf("migration %04d_%s hashed differently across two loads",
				first[i].Version, first[i].Name)
		}
	}
}

// The guard that matters: editing an already-applied migration must stop the
// deploy, because it silently diverges the repository from the live schema.
func TestVerifyChecksums(t *testing.T) {
	list := []Migration{
		{Version: 1, Name: "one", Checksum: "aaa"},
		{Version: 2, Name: "two", Checksum: "bbb"},
	}

	t.Run("matching checksums pass", func(t *testing.T) {
		applied := map[int]AppliedMigration{
			1: {Version: 1, Name: "one", Checksum: "aaa"},
			2: {Version: 2, Name: "two", Checksum: "bbb"},
		}
		if err := VerifyChecksums(list, applied); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("an edited applied migration is refused", func(t *testing.T) {
		applied := map[int]AppliedMigration{
			1: {Version: 1, Name: "one", Checksum: "aaa"},
			2: {Version: 2, Name: "two", Checksum: "DIFFERENT"},
		}
		err := VerifyChecksums(list, applied)
		if err == nil {
			t.Fatal("editing an applied migration must refuse to proceed")
		}
		// The message has to tell the operator what to do instead, or they
		// will simply delete the row and carry on.
		if !strings.Contains(err.Error(), "NEW migration") {
			t.Fatalf("error must direct the operator to write a new migration, got: %v", err)
		}
	})

	t.Run("not-yet-applied migrations are ignored", func(t *testing.T) {
		applied := map[int]AppliedMigration{
			1: {Version: 1, Name: "one", Checksum: "aaa"},
		}
		if err := VerifyChecksums(list, applied); err != nil {
			t.Fatalf("a pending migration must not trip the checksum guard: %v", err)
		}
	})

	t.Run("an empty database passes", func(t *testing.T) {
		if err := VerifyChecksums(list, map[int]AppliedMigration{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestParseMigrationName(t *testing.T) {
	t.Run("valid names", func(t *testing.T) {
		cases := map[string]struct {
			version int
			name    string
		}{
			"0001_extensions_and_helpers.up.sql": {1, "extensions_and_helpers"},
			"0012_rooms.up.sql":                  {12, "rooms"},
			"0100_a.up.sql":                      {100, "a"},
		}
		for in, want := range cases {
			v, n, err := parseMigrationName(in)
			if err != nil {
				t.Fatalf("parseMigrationName(%q): %v", in, err)
			}
			if v != want.version || n != want.name {
				t.Fatalf("parseMigrationName(%q) = (%d, %q), want (%d, %q)",
					in, v, n, want.version, want.name)
			}
		}
	})

	t.Run("malformed names are rejected", func(t *testing.T) {
		for _, in := range []string{
			"no_version.up.sql",
			"_leading_underscore.up.sql",
			"0001.up.sql",
			"",
		} {
			if _, _, err := parseMigrationName(in); err == nil {
				t.Fatalf("parseMigrationName(%q) should have failed", in)
			}
		}
	})
}

// Guards against a class of mistake the SQL itself cannot catch: a migration
// that mentions a table another module owns (ADR-005 boundary rule 1). This is
// a coarse check — it looks for CREATE TABLE only — but it makes the ownership
// map explicit and fails loudly when a new table appears with no declared home.
func TestMigrations_EveryTableHasADeclaredOwner(t *testing.T) {
	owners := map[string]string{
		// identity
		"users": "identity", "user_credentials": "identity", "sessions": "identity",
		"refresh_token_families": "identity", "refresh_tokens": "identity",
		"password_reset_tokens": "identity",
		// catalog
		"content": "catalog", "content_offers": "catalog", "content_people": "catalog",
		// rooms + realtime
		"rooms": "rooms", "room_participants": "rooms", "room_events": "realtime",
		"chat_messages": "rooms", "reaction_aggregates": "rooms",
		"room_drift_reports": "realtime",
		// games
		"trivia_questions": "games", "trivia_options": "games",
		"trivia_sessions": "games", "trivia_session_questions": "games",
		"trivia_answers": "games",
		// progression
		"xp_ledger": "progression", "xp_totals": "progression",
		"achievements": "progression", "user_achievements": "progression",
		"achievement_progress": "progression",
		// platform
		"outbox": "platform", "idempotency_keys": "platform", "feature_flags": "platform",
		// social + moderation
		"follows": "social", "blocks": "social", "mutes": "social",
		"reports": "moderation", "moderation_actions": "moderation",
		"watch_progress": "social", "favourites": "social",
	}

	loaded, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range loaded {
		for _, line := range strings.Split(m.UpSQL, "\n") {
			trimmed := strings.TrimSpace(line)
			const prefix = "CREATE TABLE "
			if !strings.HasPrefix(trimmed, prefix) {
				continue
			}
			rest := strings.TrimPrefix(trimmed, prefix)
			rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
			table := rest
			if i := strings.IndexAny(table, " ("); i > 0 {
				table = table[:i]
			}
			// Declarative partitions inherit their parent's owner.
			if strings.HasPrefix(table, "room_events_") {
				continue
			}
			if _, ok := owners[table]; !ok {
				t.Fatalf("migration %04d_%s creates table %q with no declared module owner.\n"+
					"ADR-005 boundary rule 1: each module owns its tables. Add it to the map "+
					"in this test, and confirm no other module reads it directly.",
					m.Version, m.Name, table)
			}
		}
	}
}
