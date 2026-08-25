-- 0004: rooms + realtime modules.
-- Owns: rooms, room_participants, room_events, chat_messages, reaction_aggregates,
--       room_drift_reports.
-- ADR-002 (Companion Sync), ADR-003 (event log + resync).

CREATE TABLE rooms (
  id              uuid PRIMARY KEY,
  content_id      uuid NOT NULL REFERENCES content(id) ON DELETE RESTRICT,
  host_user_id    uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

  -- ADR-010: Crockford base32, 6 chars, I/L/O/U excluded — avoids both visual
  -- confusion and accidental profanity.  A code authorises NOTHING on its own;
  -- the server authorises on resolve (FR-14).
  join_code       text NOT NULL CHECK (join_code ~ '^[0-9A-HJKMNP-TV-Z]{6}$'),

  visibility      room_visibility NOT NULL DEFAULT 'private',
  state           room_state      NOT NULL DEFAULT 'LOBBY',
  sync_mode       sync_mode       NOT NULL DEFAULT 'COMPANION',
  title           text,

  -- ---------------------------------------------------------------------
  -- The shared timeline (ADR-002):
  --   t_room = (server_now - anchor_server_time) + anchor_offset_ms
  -- Both NULL/0 until the room reaches PLAYING.
  -- ---------------------------------------------------------------------
  anchor_server_time timestamptz(3),
  anchor_offset_ms   bigint NOT NULL DEFAULT 0,
  reanchor_count     int    NOT NULL DEFAULT 0,

  -- §1.8 / FR-16: free tier caps at 4.  Enforced server-side against the
  -- host's entitlement_tier, never against a client-supplied value.
  max_participants int NOT NULL DEFAULT 4 CHECK (max_participants BETWEEN 2 AND 50),

  -- ADR-003: monotonic, gap-free, per room.  Incremented under per-room
  -- serialisation; the UNIQUE index on room_events(room_id, seq) is the real
  -- guarantee, this column is the allocator.
  current_seq     bigint NOT NULL DEFAULT 0,

  created_at      timestamptz(3) NOT NULL DEFAULT now(),
  updated_at      timestamptz(3) NOT NULL DEFAULT now(),
  started_at      timestamptz(3),
  ended_at        timestamptz(3),
  end_reason      text CHECK (end_reason IN ('host_ended', 'reaper_abandoned', 'moderation', 'error')),

  -- A PLAYING room must have an anchor, or the timeline is undefined.
  CONSTRAINT rooms_playing_requires_anchor CHECK (
    state <> 'PLAYING' OR anchor_server_time IS NOT NULL
  ),
  CONSTRAINT rooms_ended_has_reason CHECK (
    (state = 'ENDED') = (ended_at IS NOT NULL)
  )
);

CREATE TRIGGER rooms_touch BEFORE UPDATE ON rooms
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query: "resolve a join code to a live room."  Partial, so an ENDED room's
-- code is released for reuse while live codes stay unique (EC-11).
CREATE UNIQUE INDEX idx_rooms_join_code_live ON rooms (join_code)
  WHERE state <> 'ENDED';

-- Query: "public rooms shelf on Discover, newest first."
CREATE INDEX idx_rooms_public_live ON rooms (created_at DESC)
  WHERE visibility = 'public' AND state <> 'ENDED';

-- Query: "this user's recent rooms" (Rooms tab).
CREATE INDEX idx_rooms_host_recent ON rooms (host_user_id, created_at DESC);

-- Query: "reaper finds rooms to end" (FR-18).
CREATE INDEX idx_rooms_reaper ON rooms (updated_at)
  WHERE state <> 'ENDED';

-- ---------------------------------------------------------------------------
-- Membership.  §7.5: presence is derived state in Redis; MEMBERSHIP LIVES
-- HERE, in Postgres, and survives a Redis flush (EC-4).
-- ---------------------------------------------------------------------------
CREATE TABLE room_participants (
  room_id     uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role        participant_role NOT NULL DEFAULT 'participant',
  joined_at   timestamptz(3) NOT NULL DEFAULT now(),
  left_at     timestamptz(3),
  -- FR-17: host promotion picks the longest-tenured connected participant.
  kicked_by   uuid REFERENCES users(id) ON DELETE SET NULL,
  PRIMARY KEY (room_id, user_id)
);

-- Query: "current members of a room, for fan-out and the participant list."
CREATE INDEX idx_participants_room_active ON room_participants (room_id, joined_at)
  WHERE left_at IS NULL;

-- Query: "rooms this user is currently in" (mini-bar, deep-link resolution).
CREATE INDEX idx_participants_user_active ON room_participants (user_id)
  WHERE left_at IS NULL;

-- ---------------------------------------------------------------------------
-- The event log — ADR-003.  This is the backbone of resync.
--
-- Partitioned by month (§6.2) so that the §6.5 retention rule (30 days, then
-- aggregate-only) is a DETACH, not a mass DELETE.  Deleting hundreds of
-- millions of rows on a live table is how you take an outage.
-- ---------------------------------------------------------------------------
CREATE TABLE room_events (
  id            uuid NOT NULL,           -- envelope `id`; client dedupe key (FR-34)
  room_id       uuid NOT NULL,
  seq           bigint NOT NULL,         -- gap-free, monotonic per room (FR-28)
  type          text NOT NULL,
  actor_user_id uuid,                    -- NULL for system events
  actor_role    text NOT NULL DEFAULT 'system'
                  CHECK (actor_role IN ('host', 'participant', 'system')),
  payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
  timeline_ms   bigint,                  -- set for timed events (FR-27)
  created_at    timestamptz(3) NOT NULL DEFAULT now(),  -- envelope `ts`, server clock
  PRIMARY KEY (room_id, seq, created_at)
) PARTITION BY RANGE (created_at);

COMMENT ON TABLE room_events IS
  'ADR-003. Append-only. seq is gap-free per room — a gap is the client''s signal to resync.';

-- Initial partitions.  A scheduled job creates the next month ahead of time;
-- the default partition exists so a missed job degrades to "slower" rather
-- than "inserts fail and every room breaks".
CREATE TABLE room_events_2026_08 PARTITION OF room_events
  FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE room_events_2026_09 PARTITION OF room_events
  FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE room_events_2026_10 PARTITION OF room_events
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE room_events_default PARTITION OF room_events DEFAULT;

-- Query: "replay events (last_seq, current] for a resyncing client" (FR-31).
-- This is THE hot read path of the realtime tier.
CREATE INDEX idx_room_events_seq ON room_events (room_id, seq);

-- Query: "dedupe check / lookup an event by its envelope id."
CREATE INDEX idx_room_events_id ON room_events (id);

-- ---------------------------------------------------------------------------
-- Chat.  §6.2: persisted with deleted_at + deleted_by, because moderation
-- needs an audit trail and §1.9 makes that a legal requirement, not a feature.
-- ---------------------------------------------------------------------------
CREATE TABLE chat_messages (
  id          uuid PRIMARY KEY,
  room_id     uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  body        text NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
  timeline_ms bigint,
  seq         bigint NOT NULL,          -- the room_events seq that carried it
  created_at  timestamptz(3) NOT NULL DEFAULT now(),
  deleted_at  timestamptz(3),
  deleted_by  uuid REFERENCES users(id) ON DELETE SET NULL,
  delete_reason text
);

-- Query: "last 50 messages for a resync snapshot" (FR-32).
CREATE INDEX idx_chat_room_recent ON chat_messages (room_id, created_at DESC)
  WHERE deleted_at IS NULL;

-- Query: "moderation review of a reported user's messages."
CREATE INDEX idx_chat_user ON chat_messages (user_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Reactions.  §6.2 is explicit: NOT one row per tap.
--
-- Clients batch at 250ms, the server aggregates into 1-second timeline
-- buckets, and only counts are broadcast (§7.6).  A 6-person room reacting
-- enthusiastically for 45 minutes produces thousands of taps and a few hundred
-- rows.
-- ---------------------------------------------------------------------------
CREATE TABLE reaction_aggregates (
  room_id         uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  timeline_bucket bigint NOT NULL,       -- floor(timeline_ms / 1000)
  emoji           text NOT NULL,
  count           int NOT NULL DEFAULT 0 CHECK (count >= 0),
  updated_at      timestamptz(3) NOT NULL DEFAULT now(),
  PRIMARY KEY (room_id, timeline_bucket, emoji)
);

-- Query: "reaction aggregates for a resync snapshot, and the timeline heatmap."
CREATE INDEX idx_reactions_room ON reaction_aggregates (room_id, timeline_bucket);

-- ---------------------------------------------------------------------------
-- Drift reports (FR-25, FR-26).  Stored rather than held in memory because
-- the >40%-in-the-same-direction consensus rule needs to survive a server
-- restart mid-room, and because drift distribution is the single most
-- important signal for whether Companion Sync actually works in the field
-- (§2.4 R1 — the highest combined risk in the register).
-- ---------------------------------------------------------------------------
CREATE TABLE room_drift_reports (
  id                uuid PRIMARY KEY,
  room_id           uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  observed_drift_ms int  NOT NULL,       -- signed: negative = behind
  resolution        text NOT NULL DEFAULT 'nudge'
                      CHECK (resolution IN ('nudge', 'requested_reanchor', 'auto_consensus')),
  created_at        timestamptz(3) NOT NULL DEFAULT now()
);

-- Query: "evaluate drift consensus over the last 2 minutes for this room."
CREATE INDEX idx_drift_room_recent ON room_drift_reports (room_id, created_at DESC);
