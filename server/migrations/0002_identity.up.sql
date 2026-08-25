-- 0002: identity module — users, sessions, refresh token families.
-- Owns: users, user_credentials, sessions, refresh_token_families, refresh_tokens.
-- ADR-011.  No other module may read these tables (ADR-005 boundary rule 1).

CREATE TABLE users (
  id                uuid PRIMARY KEY,
  handle            citext      NOT NULL UNIQUE,
  display_name      text        NOT NULL,
  avatar_url        text,
  locale            text        NOT NULL DEFAULT 'en',
  region            text        NOT NULL DEFAULT 'EG',
  -- Numeral preference is a user setting, not a locale consequence (§3.6):
  -- an Arabic speaker may well prefer Western digits.
  numeral_system    text        NOT NULL DEFAULT 'western'
                                CHECK (numeral_system IN ('western', 'eastern')),
  -- §12.4: the age band drives capability, so it is stored explicitly rather
  -- than recomputed from date_of_birth at each call site.  A nightly job
  -- promotes bands on birthdays.
  age_band          age_band    NOT NULL,
  date_of_birth     date        NOT NULL,
  entitlement_tier  entitlement_tier NOT NULL DEFAULT 'free',
  -- Derived from age_band at signup, then user-editable only upward in privacy.
  is_discoverable   boolean     NOT NULL DEFAULT true,
  shadow_scored     boolean     NOT NULL DEFAULT false,  -- §8.4: score, do not punish
  created_at        timestamptz(3) NOT NULL DEFAULT now(),
  updated_at        timestamptz(3) NOT NULL DEFAULT now(),
  -- §6.5: 30-day grace, then hard delete.  Moderation records survive
  -- pseudonymously.
  deleted_at        timestamptz(3)
);

COMMENT ON COLUMN users.shadow_scored IS
  '§8.4 — XP is recorded but withheld from public leaderboards pending review. Never an auto-ban.';

CREATE TRIGGER users_touch BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query: "resolve a handle for profile lookup and mention autocomplete."
CREATE INDEX idx_users_handle_trgm ON users USING gin (handle gin_trgm_ops)
  WHERE deleted_at IS NULL;

-- Query: "the nightly age-band promotion job scans birthdays due today."
CREATE INDEX idx_users_dob ON users (date_of_birth)
  WHERE deleted_at IS NULL;

-- Credentials live apart from the profile so that a JOIN is required to reach
-- a hash, and so `SELECT * FROM users` can never leak one.
CREATE TABLE user_credentials (
  user_id           uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  email             citext      NOT NULL UNIQUE,
  email_verified_at timestamptz(3),
  -- Argon2id. Encoded string carries its own params, so a cost increase does
  -- not require a schema change — rehash happens on next successful login.
  password_hash     text        NOT NULL,
  created_at        timestamptz(3) NOT NULL DEFAULT now(),
  updated_at        timestamptz(3) NOT NULL DEFAULT now()
);

CREATE TRIGGER user_credentials_touch BEFORE UPDATE ON user_credentials
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- §5.3: sessions are first-class rows the user can list and revoke.
CREATE TABLE sessions (
  id            uuid PRIMARY KEY,
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_name   text NOT NULL,
  platform      text NOT NULL CHECK (platform IN ('android', 'ios', 'web', 'unknown')),
  -- §12.6: never store a full IP.  /24 for v4, /48 for v6, applied before insert.
  ip_truncated  inet,
  user_agent    text,
  created_at    timestamptz(3) NOT NULL DEFAULT now(),
  last_seen_at  timestamptz(3) NOT NULL DEFAULT now(),
  revoked_at    timestamptz(3),
  revoked_reason text
);

-- Query: "list this user's active sessions, newest first" (settings screen).
CREATE INDEX idx_sessions_user_active ON sessions (user_id, created_at DESC)
  WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Refresh token families — ADR-011.
--
-- A family is one login.  Rotation replaces the token but keeps the family.
-- Presenting an already-rotated token is indistinguishable from theft, so the
-- WHOLE FAMILY dies.  That is the entire point of modelling a family at all.
-- ---------------------------------------------------------------------------
CREATE TABLE refresh_token_families (
  id             uuid PRIMARY KEY,
  user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id     uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  created_at     timestamptz(3) NOT NULL DEFAULT now(),
  revoked_at     timestamptz(3),
  revoked_reason text CHECK (revoked_reason IN ('logout', 'reuse_detected', 'password_reset', 'admin', 'expired'))
);

CREATE INDEX idx_rtf_user ON refresh_token_families (user_id) WHERE revoked_at IS NULL;

CREATE TABLE refresh_tokens (
  id          uuid PRIMARY KEY,
  family_id   uuid NOT NULL REFERENCES refresh_token_families(id) ON DELETE CASCADE,
  -- SHA-256 of the 32 random bytes.  A database leak yields no usable token.
  token_hash  bytea NOT NULL UNIQUE,
  issued_at   timestamptz(3) NOT NULL DEFAULT now(),
  expires_at  timestamptz(3) NOT NULL,
  -- Set when this token is exchanged.  A second presentation AFTER this is set
  -- is what triggers family-wide revocation.
  rotated_at  timestamptz(3),
  -- ADR-011: 10s overlap absorbs a legitimate in-flight retry without
  -- meaningfully widening the theft window.
  valid_until_overlap timestamptz(3)
);

-- Query: "look up a presented refresh token by its hash."  The UNIQUE
-- constraint above already provides this index; declared here as documentation
-- of the query, not as a second index.

-- Query: "revoke every token in a family on reuse detection."
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens (family_id);

-- Query: "nightly reaper deletes expired tokens."
CREATE INDEX idx_refresh_tokens_expiry ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- Password reset — random, NOT UUIDv7 (ADR-010: a UUIDv7 leaks its creation
-- time, which is exactly wrong for a security token).
-- ---------------------------------------------------------------------------
CREATE TABLE password_reset_tokens (
  id          uuid PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash  bytea NOT NULL UNIQUE,
  created_at  timestamptz(3) NOT NULL DEFAULT now(),
  expires_at  timestamptz(3) NOT NULL,
  consumed_at timestamptz(3)
);

CREATE INDEX idx_prt_user ON password_reset_tokens (user_id) WHERE consumed_at IS NULL;
