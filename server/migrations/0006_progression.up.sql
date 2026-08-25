-- 0006: progression module — XP ledger, achievements.
-- Owns: xp_ledger, xp_totals, achievements, user_achievements.
-- §8.4, §8.5.  ADR-004.

-- ---------------------------------------------------------------------------
-- XP is a LEDGER, not a counter (§6.2, §8.4).
--
-- Why (interview question §16.4.5): a counter cannot answer "where did these
-- 4,200 XP come from?", cannot be reversed when abuse is detected without
-- guessing, and cannot be reconciled after a bug.  A ledger can be replayed,
-- audited, and partially reversed.  The cost is a sum on read, which is what
-- xp_totals below exists to amortise.
-- ---------------------------------------------------------------------------
CREATE TABLE xp_ledger (
  id            uuid PRIMARY KEY,
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_type   text NOT NULL,     -- e.g. 'TRIVIA_ROUND_COMPLETED', 'ROOM_COMPLETED'
  source_id     uuid NOT NULL,     -- the originating entity
  amount        int  NOT NULL,     -- signed; negative rows are reversals, never deletions
  -- §8.4 diminishing returns: the nth same-source action is worth
  -- round(base * 0.75^(n-1)), floor 1.  Recorded so the maths is auditable
  -- rather than inferred.
  base_amount   int  NOT NULL,
  repeat_index  int  NOT NULL DEFAULT 1 CHECK (repeat_index >= 1),
  -- §8.4: suspicious patterns are SHADOW-SCORED — recorded, withheld from
  -- public boards, pending review.  Never an auto-ban.
  withheld      boolean NOT NULL DEFAULT false,
  created_at    timestamptz(3) NOT NULL DEFAULT now(),

  -- FR-54.  Exactly-once by construction, not by the worker being careful.
  CONSTRAINT xp_ledger_idempotent UNIQUE (user_id, source_type, source_id)
);

COMMENT ON TABLE xp_ledger IS
  'Append-only. §8.4. Reversals are negative rows; nothing is ever UPDATEd or DELETEd. Answers §16.4.5.';

COMMENT ON CONSTRAINT xp_ledger_idempotent ON xp_ledger IS
  'FR-54 / AC-26. Makes the outbox worker safe to run twice, which it will be.';

-- Query: "a user's XP history, newest first" (profile screen).
CREATE INDEX idx_xp_user_recent ON xp_ledger (user_id, created_at DESC);

-- Query: "daily cap check — XP from this source for this user today" (§8.4).
CREATE INDEX idx_xp_user_source_day ON xp_ledger (user_id, source_type, created_at DESC);

-- Query: "weekly leaderboard rollup — XP earned within the period" (§8.6).
-- Note the WHERE: shadow-scored XP never reaches a public board.
CREATE INDEX idx_xp_period_rollup ON xp_ledger (created_at, user_id)
  WHERE withheld = false;

-- Materialised sum.  §8.4: "the user's total is a materialised sum,
-- refreshable from the ledger."  If this ever disagrees with the ledger, the
-- LEDGER IS RIGHT and a reconciliation job rewrites this row.
CREATE TABLE xp_totals (
  user_id     uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  total_xp    bigint NOT NULL DEFAULT 0,
  level       int    NOT NULL DEFAULT 1,
  reconciled_at timestamptz(3) NOT NULL DEFAULT now(),
  updated_at  timestamptz(3) NOT NULL DEFAULT now()
);

CREATE TRIGGER xp_totals_touch BEFORE UPDATE ON xp_totals
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- ---------------------------------------------------------------------------
-- Achievements as DATA, not code (§8.5).
--
-- "Adding an achievement must not require a deploy."  The predicate is a JSONB
-- rule document evaluated by a generic engine; the engine is unit-tested
-- against the rule grammar, and rules are validated on insert.
-- ---------------------------------------------------------------------------
CREATE TABLE achievements (
  id            uuid PRIMARY KEY,
  code          text NOT NULL UNIQUE,   -- stable identifier used in clients and analytics
  name_key      text NOT NULL,          -- l10n key, never a literal string (§3.6)
  description_key text NOT NULL,
  icon          text NOT NULL,
  -- The rule (§8.5): { trigger_event, predicate, window, threshold }
  trigger_event text NOT NULL,
  predicate     jsonb NOT NULL DEFAULT '{}'::jsonb,
  window_seconds int,                   -- NULL = lifetime
  threshold     int NOT NULL DEFAULT 1 CHECK (threshold >= 1),
  reward_xp     int NOT NULL DEFAULT 0 CHECK (reward_xp >= 0),
  is_active     boolean NOT NULL DEFAULT true,
  -- §8.5: a new achievement can be granted retroactively by replaying history.
  backfillable  boolean NOT NULL DEFAULT true,
  created_at    timestamptz(3) NOT NULL DEFAULT now(),
  updated_at    timestamptz(3) NOT NULL DEFAULT now()
);

CREATE TRIGGER achievements_touch BEFORE UPDATE ON achievements
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query: "which achievement rules subscribe to this domain event?"
-- This is the evaluator's hot path.
CREATE INDEX idx_achievements_trigger ON achievements (trigger_event)
  WHERE is_active = true;

-- ---------------------------------------------------------------------------
-- Grants.  §6.2: unique on (user_id, achievement_id), granted via outbox.
-- "Exactly-once by construction."
-- ---------------------------------------------------------------------------
CREATE TABLE user_achievements (
  user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  achievement_id uuid NOT NULL REFERENCES achievements(id) ON DELETE CASCADE,
  granted_at     timestamptz(3) NOT NULL DEFAULT now(),
  -- The event that caused the grant, so a grant can be explained and audited.
  source_event_id uuid,
  progress       int NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, achievement_id)
);

COMMENT ON TABLE user_achievements IS
  '§8.5. The composite PK is the final guard against double-grant. The evaluator is idempotent; this makes it provably so.';

-- Query: "this user's achievements, most recent first" (profile, activity).
CREATE INDEX idx_ua_user_recent ON user_achievements (user_id, granted_at DESC);

-- Progress toward multi-step achievements, kept separate so that an
-- in-progress counter never looks like a grant.
CREATE TABLE achievement_progress (
  user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  achievement_id uuid NOT NULL REFERENCES achievements(id) ON DELETE CASCADE,
  progress       int NOT NULL DEFAULT 0 CHECK (progress >= 0),
  window_started_at timestamptz(3),
  updated_at     timestamptz(3) NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, achievement_id)
);

CREATE TRIGGER achievement_progress_touch BEFORE UPDATE ON achievement_progress
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();
