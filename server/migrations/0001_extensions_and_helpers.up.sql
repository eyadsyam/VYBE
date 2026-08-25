-- 0001: extensions, shared helper functions, shared enums.
--
-- Forward-only, reversible (see .down.sql).  Nothing destructive.

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- unaccent() is declared STABLE, not IMMUTABLE, because with an implicit
-- dictionary its result depends on the search_path.  Naming the dictionary
-- explicitly makes it deterministic, which lets us mark this wrapper IMMUTABLE
-- and therefore use it inside a generated column and an index.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION vybe_unaccent(input text)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
AS $$ SELECT public.unaccent('public.unaccent', input) $$;

-- ---------------------------------------------------------------------------
-- vybe_normalize() — ADR-006.
--
-- THIS FUNCTION MUST BE APPLIED TO BOTH THE INDEXED TEXT AND THE QUERY.
-- If the two paths ever diverge, Arabic search silently returns nothing and no
-- English-language test notices.  That is the failure mode this exists to
-- prevent; see the regression tests in server/internal/modules/catalog.
--
-- Transforms, in order:
--   1. NFKC — collapse compatibility forms (Arabic presentation forms, full-width Latin)
--   2. Hamza carriers  أ إ آ ٱ  ->  ا
--      Alif maqsura    ى        ->  ي
--      Ta marbuta      ة        ->  ه
--   3. Strip tashkeel U+064B..U+0652, tatweel U+0640, superscript alef U+0670
--   4. unaccent — Latin diacritics (Pokémon -> Pokemon)
--   5. lower — case folding
--
-- Known limitation, recorded rather than hidden: this does NOT stem Arabic.
-- كتاب and كتب remain distinct tokens.  Acceptable for a title-matched
-- catalogue of ~200 curated items; revisit at the ADR-006 migration triggers.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION vybe_normalize(input text)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
  SELECT CASE
    WHEN input IS NULL THEN NULL
    ELSE lower(
      vybe_unaccent(
        regexp_replace(
          translate(
            normalize(input, NFKC),
            'أإآٱىة',
            'اااايه'
          ),
          '[ً-ْـٰ]', '', 'g'
        )
      )
    )
  END
$$;

COMMENT ON FUNCTION vybe_normalize(text) IS
  'ADR-006. Arabic + Latin search normalisation. MUST be applied identically at index time and query time.';

-- ---------------------------------------------------------------------------
-- uuid_generate_v7() — ADR-010.
--
-- Provided for SQL-side fixtures and seeding ONLY.  Application rows get their
-- id from Go, because the transactional outbox needs the id in hand before the
-- INSERT so it can reference the row in the same transaction.  Deliberately not
-- used as a column DEFAULT anywhere.
--
-- Layout per RFC 9562: 48-bit big-endian millisecond timestamp, 4-bit version
-- (7), 12 bits random, 2-bit variant (0b10), 62 bits random.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION uuid_generate_v7()
RETURNS uuid
LANGUAGE sql VOLATILE PARALLEL SAFE
AS $$
  SELECT encode(
    set_bit(
      set_bit(
        overlay(
          uuid_send(gen_random_uuid())
          PLACING substring(int8send((extract(epoch FROM clock_timestamp()) * 1000)::bigint) FROM 3)
          FROM 1 FOR 6
        ),
        52, 1                       -- version nibble -> 0111 (7)
      ),
      53, 1
    ),
    'hex'
  )::uuid
$$;

COMMENT ON FUNCTION uuid_generate_v7() IS
  'ADR-010. Fixtures and seeding only — application IDs are minted in Go.';

-- ---------------------------------------------------------------------------
-- updated_at maintenance.  §6.1 rule 2 requires created_at/updated_at on every
-- table; a trigger is the only way that stays true when someone writes an
-- UPDATE by hand.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION vybe_touch_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- Shared enums.  §6.1 rule 6: Postgres enums for closed sets that rarely
-- change; lookup tables for sets product will edit.
-- ---------------------------------------------------------------------------
CREATE TYPE age_band        AS ENUM ('under_13', 'teen_13_15', 'teen_16_17', 'adult');
CREATE TYPE entitlement_tier AS ENUM ('free', 'plus');
CREATE TYPE content_type    AS ENUM ('movie', 'series', 'episode', 'clip');
CREATE TYPE room_visibility AS ENUM ('private', 'public');
CREATE TYPE room_state      AS ENUM ('LOBBY', 'READY', 'PLAYING', 'ENDED');
CREATE TYPE sync_mode       AS ENUM ('COMPANION', 'CLIP', 'ASYNC');
CREATE TYPE participant_role AS ENUM ('host', 'participant');
