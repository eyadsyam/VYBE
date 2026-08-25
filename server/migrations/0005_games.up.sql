-- 0005: games module — trivia.
-- Owns: trivia_questions, trivia_options, trivia_sessions, trivia_session_questions,
--       trivia_answers.
-- ADR-004.  The defining property of this schema: THE CORRECT ANSWER LIVES IN A
-- TABLE THE CLIENT NEVER READS FROM, and the anti-cheat guarantees are database
-- constraints rather than application if-statements.

CREATE TABLE trivia_questions (
  id            uuid PRIMARY KEY,
  content_id    uuid NOT NULL REFERENCES content(id) ON DELETE CASCADE,
  locale        text NOT NULL DEFAULT 'en',
  text          text NOT NULL,
  -- Integer points (§6.1 rule 4).
  points        int  NOT NULL DEFAULT 100 CHECK (points > 0),
  time_limit_ms int  NOT NULL DEFAULT 20000 CHECK (time_limit_ms BETWEEN 5000 AND 60000),
  difficulty    int  NOT NULL DEFAULT 2 CHECK (difficulty BETWEEN 1 AND 5),
  -- Spoiler safety: a question about the ending must not fire in act one.
  -- NULL means "safe at any point".
  min_timeline_ms bigint,
  -- §2.4 R5: the content pipeline is the real bottleneck.  Curated questions
  -- are reviewed before they can be served.
  is_approved   boolean NOT NULL DEFAULT false,
  author_note   text,
  created_at    timestamptz(3) NOT NULL DEFAULT now(),
  updated_at    timestamptz(3) NOT NULL DEFAULT now()
);

CREATE TRIGGER trivia_questions_touch BEFORE UPDATE ON trivia_questions
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query: "draw N approved questions for this content and locale."
CREATE INDEX idx_tq_content_locale ON trivia_questions (content_id, locale, difficulty)
  WHERE is_approved = true;

-- ---------------------------------------------------------------------------
-- Options.  `is_correct` lives HERE.
--
-- ADR-004 / FR-44: the QUESTION_OPEN payload is built by an explicit projection
-- that selects (id, text) only.  There is a test (AC-20) that serialises the
-- outgoing payload and asserts the string "is_correct" appears nowhere in it,
-- because "we remembered not to include it" is not a guarantee.
-- ---------------------------------------------------------------------------
CREATE TABLE trivia_options (
  id          uuid PRIMARY KEY,
  question_id uuid NOT NULL REFERENCES trivia_questions(id) ON DELETE CASCADE,
  text        text NOT NULL,
  is_correct  boolean NOT NULL DEFAULT false,
  ordinal     int NOT NULL CHECK (ordinal BETWEEN 0 AND 5),
  UNIQUE (question_id, ordinal)
);

COMMENT ON COLUMN trivia_options.is_correct IS
  'ADR-004. NEVER projected into any client-bound payload before QUESTION_CLOSE. Guarded by AC-20.';

-- Query: "load the options for an open question" (server-side only).
CREATE INDEX idx_to_question ON trivia_options (question_id, ordinal);

-- Exactly one correct option per question.  A question with zero or two
-- correct answers is a content bug that would silently score everyone wrong;
-- the database refuses it rather than serving it.
CREATE UNIQUE INDEX idx_to_single_correct ON trivia_options (question_id)
  WHERE is_correct = true;

-- ---------------------------------------------------------------------------
-- A round.
-- ---------------------------------------------------------------------------
CREATE TABLE trivia_sessions (
  id            uuid PRIMARY KEY,
  room_id       uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  started_by    uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  state         text NOT NULL DEFAULT 'ACTIVE'
                  CHECK (state IN ('ACTIVE', 'COMPLETED', 'ABANDONED')),
  question_count int NOT NULL DEFAULT 5 CHECK (question_count BETWEEN 1 AND 20),
  started_at    timestamptz(3) NOT NULL DEFAULT now(),
  completed_at  timestamptz(3),
  created_at    timestamptz(3) NOT NULL DEFAULT now()
);

-- Query: "is there an active round in this room?" (resync snapshot, FR-32).
CREATE UNIQUE INDEX idx_ts_room_active ON trivia_sessions (room_id)
  WHERE state = 'ACTIVE';

-- The questions drawn for this round, in order, with their authoritative
-- open time.  `opened_at` is the ONLY timing source for scoring (FR-50).
CREATE TABLE trivia_session_questions (
  session_id  uuid NOT NULL REFERENCES trivia_sessions(id) ON DELETE CASCADE,
  question_id uuid NOT NULL REFERENCES trivia_questions(id) ON DELETE RESTRICT,
  ordinal     int  NOT NULL,
  timeline_ms bigint,
  opened_at   timestamptz(3),          -- server clock at broadcast
  deadline_at timestamptz(3),          -- opened_at + time_limit_ms
  closed_at   timestamptz(3),
  PRIMARY KEY (session_id, ordinal),
  UNIQUE (session_id, question_id)     -- no question twice in one round
);

-- Query: "find the currently open question for this session."
CREATE INDEX idx_tsq_open ON trivia_session_questions (session_id)
  WHERE opened_at IS NOT NULL AND closed_at IS NULL;

-- ---------------------------------------------------------------------------
-- Answers.  This table IS the anti-cheat mechanism.
--
-- FR-48 / AC-19: the UNIQUE constraint below is what makes "100 concurrent
-- submissions yield exactly one answer" true.  An application-level
-- "if not exists then insert" races and loses under concurrency; a unique
-- index does not.
-- ---------------------------------------------------------------------------
CREATE TABLE trivia_answers (
  id                uuid PRIMARY KEY,
  session_id        uuid NOT NULL REFERENCES trivia_sessions(id) ON DELETE CASCADE,
  user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  question_id       uuid NOT NULL REFERENCES trivia_questions(id) ON DELETE RESTRICT,
  option_id         uuid NOT NULL REFERENCES trivia_options(id) ON DELETE RESTRICT,

  -- Computed server-side from trivia_options. Never accepted from a client.
  is_correct        boolean NOT NULL,
  points_awarded    int NOT NULL DEFAULT 0 CHECK (points_awarded >= 0),

  -- FR-50: the authoritative timing source.  A device clock cannot influence it.
  server_received_at timestamptz(3) NOT NULL DEFAULT now(),
  -- Telemetry only.  Recorded so device-clock skew is observable (and so the
  -- gap between this and server_received_at becomes an abuse signal, §8.4),
  -- but NEVER read by the scoring path.
  client_ts         timestamptz(3),
  rtt_ms            int CHECK (rtt_ms IS NULL OR rtt_ms >= 0),
  elapsed_ms        int NOT NULL CHECK (elapsed_ms >= 0),  -- post-clamp, post-RTT-compensation

  created_at        timestamptz(3) NOT NULL DEFAULT now(),

  -- FR-48. The belt-and-braces guarantee of §8.1 step 5e.
  CONSTRAINT trivia_answers_one_per_user_question UNIQUE (session_id, user_id, question_id)
);

COMMENT ON CONSTRAINT trivia_answers_one_per_user_question ON trivia_answers IS
  'FR-48 / AC-19. The database enforces the anti-cheat rule, not the application. Answers interview question §16.4.6.';

COMMENT ON COLUMN trivia_answers.client_ts IS
  'FR-50. Telemetry only. Divergence from server_received_at is an abuse signal (§8.4). NEVER used for scoring.';

-- Query: "per-user results for QUESTION_CLOSE" (FR-51).
CREATE INDEX idx_ta_session_question ON trivia_answers (session_id, question_id);

-- Query: "this user's round total, for the XP grant at round completion."
CREATE INDEX idx_ta_session_user ON trivia_answers (session_id, user_id);

-- Query: "abuse detection — answer latency below the human floor (<250ms),
-- consistently, across sessions" (§8.4 detection signals).
CREATE INDEX idx_ta_user_latency ON trivia_answers (user_id, created_at DESC)
  WHERE elapsed_ms < 250;
