-- 0008: social + moderation modules.
-- Owns: follows, blocks, mutes, reports, moderation_actions, watch_progress.
-- §12.4.  ADR-008 (conflict strategies).

-- ---------------------------------------------------------------------------
-- Follows.  ADR-008: idempotent set semantics — conflict is not representable.
-- Following twice equals following once, so the offline outbox needs no
-- reconciliation rule for this entity at all.
-- ---------------------------------------------------------------------------
CREATE TABLE follows (
  follower_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  followee_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  timestamptz(3) NOT NULL DEFAULT now(),
  PRIMARY KEY (follower_id, followee_id),
  CONSTRAINT follows_no_self CHECK (follower_id <> followee_id)
);

-- Query: "who does this user follow?" (friends-watching candidate pool, §10.1).
CREATE INDEX idx_follows_follower ON follows (follower_id, created_at DESC);
-- Query: "who follows this user?" (follower count, activity fan-out).
CREATE INDEX idx_follows_followee ON follows (followee_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Blocks.  §7.8 / FR-41: "a user who has blocked another must not receive
-- their chat, reactions, or presence — filtered SERVER-SIDE AT FAN-OUT."
--
-- This table is read on the realtime hot path, so it is small, indexed both
-- ways, and cached per-connection at subscribe time with invalidation on
-- change.  Client-side filtering would pass a UI test and still leak every
-- message (AC-29 asserts on raw socket frames for exactly this reason).
-- ---------------------------------------------------------------------------
CREATE TABLE blocks (
  blocker_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  blocked_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  timestamptz(3) NOT NULL DEFAULT now(),
  PRIMARY KEY (blocker_id, blocked_id),
  CONSTRAINT blocks_no_self CHECK (blocker_id <> blocked_id)
);

-- Query: "who has this user blocked?" — loaded at socket subscribe.
CREATE INDEX idx_blocks_blocker ON blocks (blocker_id);
-- Query: "who has blocked this user?" — needed because blocking is
-- BIDIRECTIONAL in effect: B must not see A's messages either, or B can infer
-- the block and work around it.
CREATE INDEX idx_blocks_blocked ON blocks (blocked_id);

-- Room-scoped mute: quieter than a block, reversible, no relationship signal.
CREATE TABLE mutes (
  muter_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  muted_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  room_id     uuid REFERENCES rooms(id) ON DELETE CASCADE,  -- NULL = global mute
  expires_at  timestamptz(3),
  created_at  timestamptz(3) NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_mutes_unique ON mutes (muter_id, muted_id, coalesce(room_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE INDEX idx_mutes_muter ON mutes (muter_id) WHERE expires_at IS NULL OR expires_at > now();

-- ---------------------------------------------------------------------------
-- Reports.  ADR-008: append-only, never conflicts.
-- §12.4 requires a notice-and-action process with retained records.
-- ---------------------------------------------------------------------------
CREATE TABLE reports (
  id            uuid PRIMARY KEY,
  reporter_id   uuid NOT NULL REFERENCES users(id) ON DELETE SET NULL,
  -- What is being reported.  Polymorphic by design: one queue, many surfaces.
  subject_type  text NOT NULL CHECK (subject_type IN ('user', 'chat_message', 'room', 'room_name')),
  subject_id    uuid NOT NULL,
  reported_user_id uuid REFERENCES users(id) ON DELETE SET NULL,

  category      text NOT NULL CHECK (category IN
                  ('spam', 'harassment', 'hate', 'sexual', 'violence',
                   'self_harm', 'csam', 'threat', 'doxxing', 'other')),
  detail        text,

  -- §12.4 auto-triage: severity + reporter reputation + accused history.
  severity      text NOT NULL DEFAULT 'standard'
                  CHECK (severity IN ('severe', 'high', 'standard')),
  status        text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'triaged', 'actioned', 'dismissed')),
  -- SLA: severe <1h, high <24h, standard <72h.  Stored so the breach alert
  -- (§14.2, ticket) is a query rather than a guess.
  sla_due_at    timestamptz(3) NOT NULL,

  -- Evidence snapshot, captured at report time because the content may be
  -- deleted before review.  §1.9 makes preservation a legal requirement.
  evidence      jsonb NOT NULL DEFAULT '{}'::jsonb,

  created_at    timestamptz(3) NOT NULL DEFAULT now(),
  resolved_at   timestamptz(3),
  resolved_by   uuid REFERENCES users(id) ON DELETE SET NULL
);

-- Query: "the moderation queue, most urgent first" (internal review tool).
CREATE INDEX idx_reports_queue ON reports (severity, sla_due_at)
  WHERE status IN ('open', 'triaged');

-- Query: "SLA breach alert" (§14.2).
CREATE INDEX idx_reports_sla ON reports (sla_due_at)
  WHERE status IN ('open', 'triaged');

-- Query: "this user's report history" — feeds auto-triage severity.
CREATE INDEX idx_reports_accused ON reports (reported_user_id, created_at DESC);

-- Query: "rate limit — 20 reports per day per user" (§12.3).
CREATE INDEX idx_reports_reporter_day ON reports (reporter_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Moderation actions.  §12.4: "every action is appealable, every action is
-- logged with actor + reason."  Retention 2 years (§6.5) for legal
-- defensibility — these survive account deletion, pseudonymised.
-- ---------------------------------------------------------------------------
CREATE TABLE moderation_actions (
  id            uuid PRIMARY KEY,
  report_id     uuid REFERENCES reports(id) ON DELETE SET NULL,
  target_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  -- The §12.4 ladder, in escalation order.
  action        text NOT NULL CHECK (action IN
                  ('warn', 'mute_24h', 'room_ban', 'shadow_limit', 'suspend', 'reinstate')),
  reason        text NOT NULL,
  -- Who did it.  NULL means an automated action, which is itself a fact worth
  -- recording distinctly from a human decision.
  actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  is_automated  boolean NOT NULL DEFAULT false,
  expires_at    timestamptz(3),
  created_at    timestamptz(3) NOT NULL DEFAULT now(),

  appealed_at   timestamptz(3),
  appeal_outcome text CHECK (appeal_outcome IN ('upheld', 'overturned', 'pending'))
);

-- Query: "active sanctions against this user" — checked at room join and at
-- socket subscribe.
CREATE INDEX idx_modactions_target_active ON moderation_actions (target_user_id, created_at DESC)
  WHERE expires_at IS NULL OR expires_at > now();

-- Query: "appeals awaiting review."
CREATE INDEX idx_modactions_appeals ON moderation_actions (appealed_at)
  WHERE appeal_outcome = 'pending';

-- ---------------------------------------------------------------------------
-- Watch progress.  §6.2: ONE ROW PER (user, content), updated in place.
-- Position written on pause/background/exit and at most every 30s — never per
-- frame.
--
-- ADR-008: conflict strategy is MAX POSITION WINS.  Monotonic; you cannot
-- un-watch.  A stale device must never be able to rewind you.  That rule is
-- enforced in the UPDATE itself (see the repository), not only in app logic.
-- ---------------------------------------------------------------------------
CREATE TABLE watch_progress (
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  content_id    uuid NOT NULL REFERENCES content(id) ON DELETE CASCADE,
  position_ms   bigint NOT NULL DEFAULT 0 CHECK (position_ms >= 0),
  duration_ms   bigint CHECK (duration_ms IS NULL OR duration_ms > 0),
  completed     boolean NOT NULL DEFAULT false,
  last_watched_at timestamptz(3) NOT NULL DEFAULT now(),
  created_at    timestamptz(3) NOT NULL DEFAULT now(),
  updated_at    timestamptz(3) NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, content_id)
);

COMMENT ON TABLE watch_progress IS
  'ADR-008: max-position-wins on conflict. Enforced in SQL (GREATEST) so a stale offline device cannot rewind. Answers §16.4.8.';

CREATE TRIGGER watch_progress_touch BEFORE UPDATE ON watch_progress
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query (§6.3, verbatim): "a user's continue-watching, newest first."
CREATE INDEX idx_wp_user_recent ON watch_progress (user_id, last_watched_at DESC)
  WHERE completed = false;

-- Query: "genre affinity — normalised watch time per genre, 90d decay"
-- (§10.2 ranking feature).
CREATE INDEX idx_wp_user_window ON watch_progress (user_id, last_watched_at DESC);

-- ---------------------------------------------------------------------------
-- Favourites.  ADR-008: last-write-wins by client timestamp, server tie-break.
-- client_updated_at is stored precisely so the LWW rule has something to
-- compare; without it, "last write" would mean "last to arrive", which is a
-- network property, not a user intent.
-- ---------------------------------------------------------------------------
CREATE TABLE favourites (
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  content_id  uuid NOT NULL REFERENCES content(id) ON DELETE CASCADE,
  is_favourite boolean NOT NULL DEFAULT true,
  client_updated_at timestamptz(3) NOT NULL,
  created_at  timestamptz(3) NOT NULL DEFAULT now(),
  updated_at  timestamptz(3) NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, content_id)
);

CREATE TRIGGER favourites_touch BEFORE UPDATE ON favourites
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query: "this user's favourites, newest first."
CREATE INDEX idx_fav_user ON favourites (user_id, updated_at DESC)
  WHERE is_favourite = true;
