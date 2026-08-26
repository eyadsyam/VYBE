package rooms

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eyadsyam/vybe/server/internal/modules/realtime"
	"github.com/eyadsyam/vybe/server/internal/platform/db"
)

// The Postgres implementation of Repository, against migration 0004.
//
// This is where FR-28's gap-free sequence is actually enforced, and it is
// worth being explicit about how, because it is the one property in the
// system that cannot be recovered from once broken.
//
// Every mutating method runs ONE transaction that:
//
//	UPDATE rooms SET current_seq = current_seq + 1
//	  WHERE id = $1 RETURNING current_seq
//
// then writes the state change and inserts the event carrying that number.
// The UPDATE takes a row lock on the room, so two concurrent mutations
// serialise on it: the second waits, then reads the first's value. That row
// lock is what makes the sequence gap-free — an allocation from a sequence
// object or from a SELECT MAX(seq) would not hold it, and two concurrent
// joins would both take the same number or skip one.
//
// The Service also has a NextSeq method on this interface, used to build an
// envelope before calling the mutation. That allocation is NOT the one that
// counts: the mutating methods re-allocate inside their own transaction and
// overwrite the envelope's seq, so the service's value is only a placeholder.
// Doing it any other way would mean the number was chosen outside the lock.

// PostgresRepository implements Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a Repository backed by pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// allocSeqLocked increments and returns the room's sequence, holding the row
// lock for the rest of the transaction.
func allocSeqLocked(ctx context.Context, tx pgx.Tx, roomID string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx,
		`UPDATE rooms SET current_seq = current_seq + 1 WHERE id = $1 RETURNING current_seq`,
		roomID).Scan(&seq)
	if db.IsNoRows(err) {
		return 0, ErrRoomNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("allocating seq: %w", err)
	}
	return seq, nil
}

// insertEventLocked appends an event, using the seq allocated in this
// transaction rather than whatever the caller put in the envelope.
func insertEventLocked(ctx context.Context, tx pgx.Tx, e *realtime.Envelope, seq int64, actorRole string) error {
	e.Seq = seq
	if err := e.Validate(); err != nil {
		// Checked after the seq is stamped, so "seq must be >= 1" catches a
		// genuinely broken allocation rather than the placeholder.
		return fmt.Errorf("refusing to append an invalid envelope: %w", err)
	}

	var actor *string
	if e.Actor != "" {
		actor = &e.Actor
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO room_events (id, room_id, seq, type, actor_user_id, actor_role, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, e.Room, e.Seq, e.Type, actor, actorRole, []byte(e.Payload), e.TS)
	if err != nil {
		return fmt.Errorf("inserting event: %w", err)
	}
	return nil
}

func roleFor(actor string) string {
	if actor == "" {
		return "system"
	}
	return "participant"
}

// CreateRoom inserts the room, its host participant, and the opening event.
func (r *PostgresRepository) CreateRoom(ctx context.Context, room *Room, event *realtime.Envelope) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO rooms (
				id, content_id, host_user_id, join_code, visibility, state,
				sync_mode, title, max_participants, current_seq, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5::room_visibility,$6::room_state,$7::sync_mode,
			          NULLIF($8,''),$9,$10,$11,$11)`,
			room.ID, room.ContentID, room.HostUserID, room.JoinCode, room.Visibility,
			string(room.State), room.SyncMode, room.Title, room.MaxParticipants,
			room.CurrentSeq, room.CreatedAt)
		if err != nil {
			// The unique index is PARTIAL — `WHERE state <> 'ENDED'` — so a
			// code may repeat across finished rooms. A violation here means a
			// live room already holds it, which the service's pre-check races.
			if db.IsUniqueViolation(err) {
				return ErrJoinCodeConflict
			}
			if db.IsForeignKeyViolation(err) {
				return ErrContentNotFound
			}
			return fmt.Errorf("inserting room: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO room_participants (room_id, user_id, role, joined_at)
			VALUES ($1,$2,'host'::participant_role,$3)`,
			room.ID, room.HostUserID, room.CreatedAt); err != nil {
			return fmt.Errorf("inserting host participant: %w", err)
		}

		// current_seq is already 1 from the insert, so the opening event takes
		// that number rather than allocating another. Allocating here would
		// make the first event seq 2 and put every room permanently off by one
		// against AC-7.
		return insertEventLocked(ctx, tx, event, room.CurrentSeq, "host")
	})
}

const roomColumns = `
	id, content_id, host_user_id, join_code, visibility::text, state::text,
	sync_mode::text, COALESCE(title,''), anchor_server_time, anchor_offset_ms,
	reanchor_count, max_participants, current_seq, created_at, updated_at,
	started_at, ended_at, COALESCE(end_reason,'')`

func scanRoom(row pgx.Row) (*Room, error) {
	var rm Room
	var state string
	err := row.Scan(&rm.ID, &rm.ContentID, &rm.HostUserID, &rm.JoinCode,
		&rm.Visibility, &state, &rm.SyncMode, &rm.Title, &rm.AnchorServerTime,
		&rm.AnchorOffsetMS, &rm.ReanchorCount, &rm.MaxParticipants, &rm.CurrentSeq,
		&rm.CreatedAt, &rm.UpdatedAt, &rm.StartedAt, &rm.EndedAt, &rm.EndReason)
	if err != nil {
		return nil, err
	}
	rm.State = State(state)
	return &rm, nil
}

// RoomByID returns a room, or (nil, nil) when absent.
func (r *PostgresRepository) RoomByID(ctx context.Context, id string) (*Room, error) {
	room, err := scanRoom(r.pool.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = $1`, id))
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("selecting room: %w", err)
	}
	return room, nil
}

// RoomByJoinCode resolves a LIVE room by code.
func (r *PostgresRepository) RoomByJoinCode(ctx context.Context, code string) (*Room, error) {
	// `state <> 'ENDED'` matches the partial unique index exactly, so this
	// query uses it and can never return two rows.
	room, err := scanRoom(r.pool.QueryRow(ctx,
		`SELECT `+roomColumns+` FROM rooms WHERE join_code = $1 AND state <> 'ENDED'`, code))
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("selecting room by join code: %w", err)
	}
	return room, nil
}

// JoinCodeTaken reports whether a live room holds the code.
func (r *PostgresRepository) JoinCodeTaken(ctx context.Context, code string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms WHERE join_code = $1 AND state <> 'ENDED')`,
		code).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking join code: %w", err)
	}
	return exists, nil
}

// Participants returns the ACTIVE participants, oldest join first.
//
// Connected is reported as true for every active row. Live socket presence
// lives in the hub, not in Postgres — writing a row on every connect and
// disconnect would mean a database write per lift and tunnel, and ADR-009
// puts reconstructible state in Redis rather than in the source of truth. The
// caller overlays hub presence where it matters.
func (r *PostgresRepository) Participants(ctx context.Context, roomID string) ([]Participant, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.user_id, p.joined_at, (p.role = 'host') AS is_host
		FROM room_participants p
		WHERE p.room_id = $1 AND p.left_at IS NULL
		ORDER BY p.joined_at ASC, p.user_id ASC`, roomID)
	if err != nil {
		return nil, fmt.Errorf("selecting participants: %w", err)
	}
	defer rows.Close()

	var out []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.UserID, &p.JoinedAt, &p.IsHost); err != nil {
			return nil, fmt.Errorf("scanning participant: %w", err)
		}
		p.Connected = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddParticipant inserts or reactivates a participant and appends the event.
func (r *PostgresRepository) AddParticipant(ctx context.Context, roomID, userID, role string, at time.Time, event *realtime.Envelope) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		seq, err := allocSeqLocked(ctx, tx, roomID)
		if err != nil {
			return err
		}

		// ON CONFLICT because the primary key is (room_id, user_id) and a
		// rejoin after leaving must reactivate the existing row. A plain
		// INSERT would fail for anybody who has ever left, which is a bug
		// nobody sees until the first person rejoins.
		if _, err := tx.Exec(ctx, `
			INSERT INTO room_participants (room_id, user_id, role, joined_at)
			VALUES ($1,$2,$3::participant_role,$4)
			ON CONFLICT (room_id, user_id)
			DO UPDATE SET left_at = NULL, joined_at = $4, kicked_by = NULL`,
			roomID, userID, role, at); err != nil {
			return fmt.Errorf("inserting participant: %w", err)
		}

		return insertEventLocked(ctx, tx, event, seq, roleFor(event.Actor))
	})
}

// RemoveParticipant stamps departure and appends the event.
func (r *PostgresRepository) RemoveParticipant(ctx context.Context, roomID, userID string, at time.Time, event *realtime.Envelope) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		seq, err := allocSeqLocked(ctx, tx, roomID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE room_participants SET left_at = $3
			WHERE room_id = $1 AND user_id = $2 AND left_at IS NULL`,
			roomID, userID, at); err != nil {
			return fmt.Errorf("removing participant: %w", err)
		}
		return insertEventLocked(ctx, tx, event, seq, roleFor(event.Actor))
	})
}

// TransferHost reassigns the host and appends the event.
func (r *PostgresRepository) TransferHost(ctx context.Context, roomID, newHostID string, _ time.Time, event *realtime.Envelope) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		seq, err := allocSeqLocked(ctx, tx, roomID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE rooms SET host_user_id = $2 WHERE id = $1`, roomID, newHostID); err != nil {
			return fmt.Errorf("reassigning host: %w", err)
		}
		// Demote everyone, then promote one. Two statements rather than a CASE
		// because the old host's row must lose the role even if the new host's
		// row does not exist yet.
		if _, err := tx.Exec(ctx,
			`UPDATE room_participants SET role = 'participant'::participant_role
			 WHERE room_id = $1 AND role = 'host'::participant_role`, roomID); err != nil {
			return fmt.Errorf("demoting the previous host: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE room_participants SET role = 'host'::participant_role
			 WHERE room_id = $1 AND user_id = $2`, roomID, newHostID); err != nil {
			return fmt.Errorf("promoting the successor: %w", err)
		}
		return insertEventLocked(ctx, tx, event, seq, "system")
	})
}

// SetState writes a transition and appends the event.
func (r *PostgresRepository) SetState(ctx context.Context, roomID string, to State, at time.Time, event *realtime.Envelope) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		seq, err := allocSeqLocked(ctx, tx, roomID)
		if err != nil {
			return err
		}

		// started_at is set once, on the first entry to PLAYING. COALESCE
		// keeps a re-anchor from rewriting when the party actually began.
		if _, err := tx.Exec(ctx, `
			UPDATE rooms
			SET state = $2::room_state,
			    started_at = CASE WHEN $2 = 'PLAYING' THEN COALESCE(started_at, $3) ELSE started_at END
			WHERE id = $1`, roomID, string(to), at); err != nil {
			return fmt.Errorf("setting state: %w", err)
		}
		return insertEventLocked(ctx, tx, event, seq, roleFor(event.Actor))
	})
}

// EndRoom marks the room ended and appends the event.
func (r *PostgresRepository) EndRoom(ctx context.Context, roomID, reason string, at time.Time, event *realtime.Envelope) error {
	return db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		seq, err := allocSeqLocked(ctx, tx, roomID)
		if err != nil {
			return err
		}
		// The rooms_ended_has_reason CHECK requires state = 'ENDED' and
		// ended_at to agree, so both are set in one statement.
		if _, err := tx.Exec(ctx, `
			UPDATE rooms
			SET state = 'ENDED'::room_state, ended_at = COALESCE(ended_at, $2), end_reason = COALESCE(end_reason, $3)
			WHERE id = $1`, roomID, at, reason); err != nil {
			return fmt.Errorf("ending room: %w", err)
		}
		// Everybody is out. Without this the participant rows stay active
		// forever and every "rooms I am in" query returns finished parties.
		if _, err := tx.Exec(ctx,
			`UPDATE room_participants SET left_at = $2 WHERE room_id = $1 AND left_at IS NULL`,
			roomID, at); err != nil {
			return fmt.Errorf("clearing participants: %w", err)
		}
		return insertEventLocked(ctx, tx, event, seq, roleFor(event.Actor))
	})
}

// NextSeq returns what the next sequence number will be.
//
// A read, not an allocation: the real allocation happens inside each mutating
// method's transaction, under the room's row lock. This value only fills in a
// placeholder on the envelope the service builds, and is overwritten there.
// Allocating for real here would put the number outside the lock, which is
// precisely how a gap appears.
func (r *PostgresRepository) NextSeq(ctx context.Context, roomID string) (int64, error) {
	var current int64
	err := r.pool.QueryRow(ctx, `SELECT current_seq FROM rooms WHERE id = $1`, roomID).Scan(&current)
	if db.IsNoRows(err) {
		return 0, ErrRoomNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading current seq: %w", err)
	}
	return realtime.NextSeq(current), nil
}

// RoomsForUser lists a user's rooms, newest first, keyset-paginated.
func (r *PostgresRepository) RoomsForUser(ctx context.Context, userID string, before *time.Time, beforeID string, limit int) ([]Room, error) {
	// The (created_at, id) tuple comparison is the keyset. Comparing only on
	// created_at would drop or repeat rows whenever a page boundary lands
	// inside a group of equal timestamps — which happens constantly, because
	// rooms created by a script share a millisecond.
	//
	// EXISTS rather than a LEFT JOIN with DISTINCT: the join would produce one
	// row per participant row and then deduplicate them, and DISTINCT over
	// eighteen columns is a sort the query does not otherwise need.
	rows, err := r.pool.Query(ctx, `
		SELECT `+roomColumns+`
		FROM rooms
		WHERE (host_user_id = $1 OR EXISTS(
		         SELECT 1 FROM room_participants p
		         WHERE p.room_id = rooms.id AND p.user_id = $1))
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, userID, before, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("selecting rooms for user: %w", err)
	}
	defer rows.Close()

	var out []Room
	for rows.Next() {
		room, err := scanRoom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning room: %w", err)
		}
		out = append(out, *room)
	}
	return out, rows.Err()
}

// ContentExists reports whether a content id is in the catalogue.
func (r *PostgresRepository) ContentExists(ctx context.Context, contentID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM content WHERE id = $1)`, contentID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking content: %w", err)
	}
	return exists, nil
}

// ---------------------------------------------------------------------------
// realtime.RoomReader
// ---------------------------------------------------------------------------

// Membership reports the room's current seq if the user is a member.
func (r *PostgresRepository) Membership(ctx context.Context, roomID, userID string) (int64, error) {
	var seq int64
	err := r.pool.QueryRow(ctx, `
		SELECT r.current_seq
		FROM rooms r
		WHERE r.id = $1
		  AND r.state <> 'ENDED'
		  AND (r.host_user_id = $2 OR EXISTS(
		        SELECT 1 FROM room_participants p
		        WHERE p.room_id = r.id AND p.user_id = $2 AND p.left_at IS NULL))`,
		roomID, userID).Scan(&seq)
	if db.IsNoRows(err) {
		// "No such room" and "not a member" are one answer, as they are on the
		// HTTP path: distinguishing them confirms the id to somebody probing.
		return 0, realtime.ErrRoomNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("checking membership: %w", err)
	}
	return seq, nil
}

// EventsSince returns events in (fromSeq, toSeq], oldest first.
func (r *PostgresRepository) EventsSince(ctx context.Context, roomID string, fromSeq, toSeq int64) ([]realtime.Envelope, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, room_id, seq, type, COALESCE(actor_user_id::text,''), payload, created_at
		FROM room_events
		WHERE room_id = $1 AND seq > $2 AND seq <= $3
		ORDER BY seq ASC`, roomID, fromSeq, toSeq)
	if err != nil {
		return nil, fmt.Errorf("selecting events: %w", err)
	}
	defer rows.Close()

	out := make([]realtime.Envelope, 0, toSeq-fromSeq)
	for rows.Next() {
		e := realtime.Envelope{V: realtime.EnvelopeVersion}
		var payload []byte
		if err := rows.Scan(&e.ID, &e.Room, &e.Seq, &e.Type, &e.Actor, &payload, &e.TS); err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// OldestRetainedSeq is the lowest seq still in the log.
func (r *PostgresRepository) OldestRetainedSeq(ctx context.Context, roomID string) (int64, error) {
	var oldest *int64
	err := r.pool.QueryRow(ctx,
		`SELECT MIN(seq) FROM room_events WHERE room_id = $1`, roomID).Scan(&oldest)
	if err != nil {
		return 0, fmt.Errorf("reading the retention floor: %w", err)
	}
	if oldest == nil {
		// No events at all. 1 rather than 0, so DecideResync's
		// `lastSeq+1 < oldestRetained` comparison does not treat an empty log
		// as "everything has aged out" and force a snapshot on a brand-new
		// room.
		return 1, nil
	}
	return *oldest, nil
}

// Snapshot renders the room's whole state for a client that cannot be served a
// delta.
func (r *PostgresRepository) Snapshot(ctx context.Context, roomID string) (json.RawMessage, error) {
	room, err := r.RoomByID(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, ErrRoomNotFound
	}
	participants, err := r.Participants(ctx, roomID)
	if err != nil {
		return nil, err
	}

	type snapParticipant struct {
		UserID   string    `json:"userId"`
		IsHost   bool      `json:"isHost"`
		JoinedAt time.Time `json:"joinedAt"`
	}
	snap := struct {
		RoomID           string            `json:"roomId"`
		State            string            `json:"state"`
		HostUserID       string            `json:"hostUserId"`
		SyncMode         string            `json:"syncMode"`
		CurrentSeq       int64             `json:"currentSeq"`
		MaxParticipants  int               `json:"maxParticipants"`
		AnchorServerTime *time.Time        `json:"anchorServerTime,omitempty"`
		AnchorOffsetMS   int64             `json:"anchorOffsetMs"`
		Participants     []snapParticipant `json:"participants"`
	}{
		RoomID: room.ID, State: string(room.State), HostUserID: room.HostUserID,
		SyncMode: room.SyncMode, CurrentSeq: room.CurrentSeq,
		MaxParticipants:  room.MaxParticipants,
		AnchorServerTime: room.AnchorServerTime, AnchorOffsetMS: room.AnchorOffsetMS,
		// Non-nil so the field serialises as [] rather than null: a Dart
		// List<T> decode throws on null where it accepts an empty list.
		Participants: make([]snapParticipant, 0, len(participants)),
	}
	for _, p := range participants {
		snap.Participants = append(snap.Participants, snapParticipant{
			UserID: p.UserID, IsHost: p.IsHost, JoinedAt: p.JoinedAt,
		})
	}

	encoded, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("encoding snapshot: %w", err)
	}
	return encoded, nil
}

var (
	_ Repository          = (*PostgresRepository)(nil)
	_ realtime.RoomReader = (*PostgresRepository)(nil)
)
