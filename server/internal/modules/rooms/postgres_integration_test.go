package rooms_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/modules/rooms"
)

// Integration tests for the rooms Postgres repository.
//
// These SKIP without VYBE_DB_DSN and RUN in CI against a real Postgres 17.
//
// The property that justifies them is FR-28's gap-free sequence, and it is the
// one property in the system that cannot be recovered from once broken: a gap
// is indistinguishable from data loss to every connected client, forever. The
// in-memory fake can only assert that the service ASKS for contiguous numbers.
// Whether `UPDATE rooms SET current_seq = current_seq + 1 RETURNING
// current_seq` actually serialises concurrent writers is a question about
// Postgres row locks, and only Postgres can answer it.

func roomsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("VYBE_DB_DSN")
	if dsn == "" {
		t.Skip("VYBE_DB_DSN is not set; skipping the Postgres integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgFixture creates the users and content a room needs, and cleans them up.
type pgFixture struct {
	pool      *pgxpool.Pool
	svc       *rooms.Service
	repo      *rooms.PostgresRepository
	contentID string
	users     []string
}

func newPGFixture(t *testing.T, userCount int) *pgFixture {
	t.Helper()
	pool := roomsPool(t)
	ctx := context.Background()

	f := &pgFixture{pool: pool}
	f.repo = rooms.NewPostgresRepository(pool)
	f.svc = rooms.NewService(f.repo, tiers{})

	// Content first: rooms.content_id is ON DELETE RESTRICT, so the room must
	// go before the content does.
	if err := pool.QueryRow(ctx, `
		INSERT INTO content (id, content_type, title)
		VALUES (uuid_generate_v7(), 'movie'::content_type, $1)
		RETURNING id::text`, "pg fixture "+t.Name()).Scan(&f.contentID); err != nil {
		t.Fatalf("inserting content: %v", err)
	}

	for i := range userCount {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO users (id, handle, display_name, age_band, date_of_birth)
			VALUES (uuid_generate_v7(), $1, $2, 'adult'::age_band, '2000-01-01')
			RETURNING id::text`,
			handleFor(t, i), "fixture user").Scan(&id); err != nil {
			t.Fatalf("inserting user %d: %v", i, err)
		}
		f.users = append(f.users, id)
	}

	t.Cleanup(func() {
		bg := context.Background()
		// Rooms before content and users: RESTRICT on content_id and
		// host_user_id means the parents cannot go first.
		for _, u := range f.users {
			_, _ = pool.Exec(bg, `DELETE FROM room_events WHERE room_id IN
				(SELECT id FROM rooms WHERE host_user_id = $1)`, u)
			_, _ = pool.Exec(bg, `DELETE FROM rooms WHERE host_user_id = $1`, u)
		}
		for _, u := range f.users {
			_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u)
		}
		_, _ = pool.Exec(bg, `DELETE FROM content WHERE id = $1`, f.contentID)
	})

	return f
}

func handleFor(t *testing.T, i int) string {
	t.Helper()
	// Lowercase ASCII only, to satisfy NormaliseHandle's rules and stay unique
	// across a shared database.
	sum := 0
	for _, r := range t.Name() {
		sum = sum*31 + int(r)
		sum %= 1 << 28
	}
	return "fx" + itoaLower(sum) + itoaLower(i)
}

func itoaLower(n int) string {
	const digits = "abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "a"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%26]}, b...)
		n /= 26
	}
	return string(b)
}

func (f *pgFixture) create(t *testing.T) *rooms.Mutation {
	t.Helper()
	m, err := f.svc.Create(context.Background(), rooms.CreateInput{
		HostUserID: f.users[0], ContentID: f.contentID, Title: "ليلة أفلام",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m
}

func TestPGRoomLifecycleWritesAContiguousLog(t *testing.T) {
	f := newPGFixture(t, 3)
	ctx := context.Background()
	m := f.create(t)

	if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[1]); err != nil {
		t.Fatalf("join 1: %v", err)
	}
	if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[2]); err != nil {
		t.Fatalf("join 2: %v", err)
	}
	if _, err := f.svc.Transition(ctx, m.Room.ID, f.users[0], rooms.EventArm); err != nil {
		t.Fatalf("ARM: %v", err)
	}
	if _, err := f.svc.Transition(ctx, m.Room.ID, f.users[0], rooms.EventStart); err != nil {
		t.Fatalf("START: %v", err)
	}
	if _, err := f.svc.Leave(ctx, m.Room.ID, f.users[2]); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := f.svc.End(ctx, m.Room.ID, f.users[0]); err != nil {
		t.Fatalf("End: %v", err)
	}

	// 1 create + 2 joins + 2 transitions + 1 leave + 1 end = 7.
	events, err := f.repo.EventsSince(ctx, m.Room.ID, 0, 1000)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(events) != 7 {
		types := make([]string, len(events))
		for i, e := range events {
			types[i] = e.Type
		}
		t.Fatalf("%d events, want 7: %v", len(events), types)
	}
	if err := realtime.VerifyContiguous(events, 1); err != nil {
		t.Fatalf("the log has a gap: %v", err)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
		if err := e.Validate(); err != nil {
			t.Errorf("event %d (%s) is not a valid §7.2 envelope: %v", i, e.Type, err)
		}
	}

	// The room's own counter must agree with the log, or a HELLO frame tells
	// a reconnecting client the wrong position.
	room, err := f.repo.RoomByID(ctx, m.Room.ID)
	if err != nil {
		t.Fatalf("RoomByID: %v", err)
	}
	if room.CurrentSeq != 7 {
		t.Errorf("room.current_seq = %d, want 7", room.CurrentSeq)
	}
	if room.State != rooms.StateEnded {
		t.Errorf("state = %q, want ENDED", room.State)
	}
}

func TestPGConcurrentJoinsProduceNoGapAndNoDuplicateSeq(t *testing.T) {
	// The test that only Postgres can run. Eight goroutines join the same room
	// at once; the row lock taken by `UPDATE ... RETURNING current_seq` is what
	// makes their sequence numbers distinct and contiguous.
	//
	// Without that lock — an allocation from a sequence object, or a
	// SELECT MAX(seq)+1 — two joiners take the same number (violating the
	// primary key) or skip one (a gap every client reads as data loss).
	f := newPGFixture(t, 9)
	ctx := context.Background()
	m := f.create(t)

	// A plus-tier host would cap at 8; the capacity check runs before the
	// allocation, so what matters here is that whoever gets in gets a distinct
	// number. Raise the cap directly so all eight are admitted.
	if _, err := f.pool.Exec(ctx,
		`UPDATE rooms SET max_participants = 50 WHERE id = $1`, m.Room.ID); err != nil {
		t.Fatalf("raising the cap: %v", err)
	}

	const joiners = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error

	start := make(chan struct{})
	for i := range joiners {
		wg.Go(func() {
			<-start
			if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[i+1]); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	for _, err := range failures {
		// ErrRoomFull would mean the capacity check raced, which is a
		// different (and acceptable) outcome; anything else is a real failure.
		if !errors.Is(err, rooms.ErrRoomFull) {
			t.Errorf("concurrent join failed: %v", err)
		}
	}

	events, err := f.repo.EventsSince(ctx, m.Room.ID, 0, 1000)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if err := realtime.VerifyContiguous(events, 1); err != nil {
		t.Fatalf("concurrent joins produced a gap: %v", err)
	}

	seen := map[int64]bool{}
	for _, e := range events {
		if seen[e.Seq] {
			t.Errorf("seq %d was allocated twice", e.Seq)
		}
		seen[e.Seq] = true
	}
	if len(events) != joiners+1 {
		t.Errorf("%d events, want %d (one create plus %d joins)", len(events), joiners+1, joiners)
	}
}

func TestPGJoinCodeUniquenessIsPartialOnEndedRooms(t *testing.T) {
	// The index is `UNIQUE (join_code) WHERE state <> 'ENDED'`, so a code
	// becomes reusable once a room finishes. If the index were total, the
	// code space would shrink permanently with every party ever held.
	f := newPGFixture(t, 2)
	ctx := context.Background()

	first := f.create(t)
	code := first.Room.JoinCode

	// While it is live, the code must not be reusable.
	f.svc.SetCodeGenerator(func() (string, error) { return code, nil })
	if _, err := f.svc.Create(ctx, rooms.CreateInput{
		HostUserID: f.users[1], ContentID: f.contentID,
	}); !errors.Is(err, rooms.ErrJoinCodeConflict) {
		t.Fatalf("reusing a live code = %v, want ErrJoinCodeConflict", err)
	}

	// End it, and the same code must now be accepted.
	if _, err := f.svc.End(ctx, first.Room.ID, f.users[0]); err != nil {
		t.Fatalf("End: %v", err)
	}
	second, err := f.svc.Create(ctx, rooms.CreateInput{
		HostUserID: f.users[1], ContentID: f.contentID,
	})
	if err != nil {
		t.Fatalf("reusing a code from an ENDED room: %v", err)
	}
	if second.Room.JoinCode != code {
		t.Errorf("join code = %q, want the reused %q", second.Room.JoinCode, code)
	}

	// And resolving it must find the LIVE room, not the finished one.
	got, err := f.repo.RoomByJoinCode(ctx, code)
	if err != nil {
		t.Fatalf("RoomByJoinCode: %v", err)
	}
	if got == nil || got.ID != second.Room.ID {
		t.Errorf("the code resolved to %v, want the live room %q", got, second.Room.ID)
	}
}

func TestPGRoomStateCheckConstraintsHold(t *testing.T) {
	// Two CHECK constraints in migration 0004 encode rules the Go code also
	// believes. If they ever disagree, the database wins — at insert time, in
	// production. Better to find out here.
	f := newPGFixture(t, 1)
	ctx := context.Background()
	m := f.create(t)

	// rooms_playing_requires_anchor: PLAYING without an anchor must be refused.
	_, err := f.pool.Exec(ctx,
		`UPDATE rooms SET state = 'PLAYING'::room_state, anchor_server_time = NULL WHERE id = $1`,
		m.Room.ID)
	if err == nil {
		t.Error("Postgres accepted PLAYING with no anchor; rooms_playing_requires_anchor is not doing its job")
	}

	// rooms_ended_has_reason: ENDED and ended_at must agree in both directions.
	_, err = f.pool.Exec(ctx,
		`UPDATE rooms SET state = 'ENDED'::room_state, ended_at = NULL WHERE id = $1`, m.Room.ID)
	if err == nil {
		t.Error("Postgres accepted ENDED with a null ended_at")
	}
}

func TestPGEventPayloadRoundTripsThroughJSONB(t *testing.T) {
	// jsonb normalises: it reorders keys, collapses whitespace, and rejects
	// invalid UTF-8. Arabic in a payload is exactly the case where an encoding
	// mistake would surface, and it surfaces on the client rather than here.
	f := newPGFixture(t, 2)
	ctx := context.Background()
	m := f.create(t)

	if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[1]); err != nil {
		t.Fatalf("join: %v", err)
	}

	events, err := f.repo.EventsSince(ctx, m.Room.ID, 0, 10)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("%d events, want at least 2", len(events))
	}

	var payload map[string]any
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil {
		t.Fatalf("the payload did not survive jsonb: %v", err)
	}
	if payload["userId"] != f.users[1] {
		t.Errorf("payload.userId = %v, want %q", payload["userId"], f.users[1])
	}
	if events[1].Actor != f.users[1] {
		t.Errorf("actor = %q, want %q", events[1].Actor, f.users[1])
	}
}

func TestPGSnapshotIsValidJSONWithAnArrayNotNull(t *testing.T) {
	// A Dart List<T> decode throws on null where it accepts an empty list, so
	// a null participants array would crash the client on resync.
	f := newPGFixture(t, 1)
	ctx := context.Background()
	m := f.create(t)

	raw, err := f.repo.Snapshot(ctx, m.Room.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var snap struct {
		RoomID       string `json:"roomId"`
		State        string `json:"state"`
		CurrentSeq   int64  `json:"currentSeq"`
		Participants []struct {
			UserID string `json:"userId"`
			IsHost bool   `json:"isHost"`
		} `json:"participants"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("the snapshot is not valid JSON: %v", err)
	}
	if snap.RoomID != m.Room.ID || snap.State != "LOBBY" {
		t.Errorf("snapshot = %+v", snap)
	}
	if len(snap.Participants) != 1 || !snap.Participants[0].IsHost {
		t.Errorf("participants = %+v, want the host alone", snap.Participants)
	}
	if snap.Participants == nil {
		t.Error("participants serialised as null rather than []")
	}
}

func TestPGMembershipConflatesUnknownRoomAndNonMember(t *testing.T) {
	f := newPGFixture(t, 2)
	ctx := context.Background()
	m := f.create(t)

	if _, err := f.repo.Membership(ctx, m.Room.ID, f.users[0]); err != nil {
		t.Fatalf("the host is not a member of their own room: %v", err)
	}
	if _, err := f.repo.Membership(ctx, m.Room.ID, f.users[1]); !errors.Is(err, realtime.ErrRoomNotFound) {
		t.Errorf("a non-member = %v, want ErrRoomNotFound", err)
	}
	if _, err := f.repo.Membership(ctx, "00000000-0000-7000-8000-000000000000", f.users[0]); !errors.Is(err, realtime.ErrRoomNotFound) {
		t.Errorf("an unknown room = %v, want ErrRoomNotFound", err)
	}

	// An ENDED room must also refuse: the socket has nothing to deliver.
	if _, err := f.svc.End(ctx, m.Room.ID, f.users[0]); err != nil {
		t.Fatalf("End: %v", err)
	}
	if _, err := f.repo.Membership(ctx, m.Room.ID, f.users[0]); !errors.Is(err, realtime.ErrRoomNotFound) {
		t.Errorf("an ended room = %v, want ErrRoomNotFound", err)
	}
}

func TestPGRejoinReactivatesRatherThanFailing(t *testing.T) {
	// The primary key is (room_id, user_id), so a rejoin after leaving must
	// UPDATE rather than INSERT. A plain insert would fail for anybody who has
	// ever left — a bug nobody sees until the first person rejoins.
	f := newPGFixture(t, 2)
	ctx := context.Background()
	m := f.create(t)

	if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[1]); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := f.svc.Leave(ctx, m.Room.ID, f.users[1]); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[1]); err != nil {
		t.Fatalf("rejoin: %v", err)
	}

	participants, err := f.repo.Participants(ctx, m.Room.ID)
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(participants) != 2 {
		t.Errorf("%d active participants after a rejoin, want 2", len(participants))
	}
}

func TestPGListForUserPaginatesWithoutOverlap(t *testing.T) {
	f := newPGFixture(t, 1)
	ctx := context.Background()

	const total = 5
	for range total {
		f.create(t)
		// Distinct created_at values, so the keyset has something to order by.
		time.Sleep(2 * time.Millisecond)
	}

	seen := map[string]bool{}
	var before *time.Time
	var beforeID string

	for page := range total {
		got, err := f.svc.ListForUser(ctx, f.users[0], before, beforeID, 2)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		for _, r := range got {
			if seen[r.ID] {
				t.Errorf("room %s appeared on two pages", r.ID)
			}
			seen[r.ID] = true
		}
		last := got[len(got)-1]
		before, beforeID = &last.CreatedAt, last.ID
	}

	if len(seen) != total {
		t.Errorf("paged through %d rooms, want %d", len(seen), total)
	}
}

func TestPGEndRoomClearsParticipants(t *testing.T) {
	// Without this, participant rows stay active forever and every
	// "rooms I am in" query returns finished parties.
	f := newPGFixture(t, 2)
	ctx := context.Background()
	m := f.create(t)

	if _, err := f.svc.JoinByCode(ctx, m.Room.JoinCode, f.users[1]); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := f.svc.End(ctx, m.Room.ID, f.users[0]); err != nil {
		t.Fatalf("End: %v", err)
	}

	participants, err := f.repo.Participants(ctx, m.Room.ID)
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(participants) != 0 {
		t.Errorf("%d participants still active after the room ended", len(participants))
	}
}
