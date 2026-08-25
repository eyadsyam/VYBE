-- Extensions must exist before any migration runs.  Executed once by the
-- postgres image's entrypoint on an empty data directory.
--
-- Each extension is here because a named query needs it (Master Prompt v2 §6.1
-- rule 5: every index exists because of a named query — the same discipline
-- applies to extensions).

-- pg_trgm: fuzzy and typo-tolerant title matching, and prefix search for
-- as-you-type.  Used by idx_content_title_trgm (ADR-006, §6.3).
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- unaccent: strips Latin diacritics (Pokémon -> Pokemon) inside
-- vybe_normalize().  Arabic diacritics are handled separately because
-- unaccent's default rules do not cover the Arabic block (ADR-006).
CREATE EXTENSION IF NOT EXISTS unaccent;

-- citext: case-insensitive email uniqueness without a functional index that
-- every query has to remember to match.
CREATE EXTENSION IF NOT EXISTS citext;

-- pgcrypto: gen_random_bytes() for join-code and token generation paths that
-- run inside SQL (seeding, fixtures).  Application code uses Go's crypto/rand.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
