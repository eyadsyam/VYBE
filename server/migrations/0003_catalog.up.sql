-- 0003: catalog module — content, provider mirror, where-to-watch, search.
-- Owns: content, content_offers, content_people.
-- ADR-006 (search), ADR-012 (provider port).

-- §6.2: one table + a discriminator, because movies/series/episodes share ~80%
-- of their fields, and provider payloads vary and change.  Hot fields are
-- promoted to generated columns; everything else stays in JSONB.
CREATE TABLE content (
  id              uuid PRIMARY KEY,
  content_type    content_type NOT NULL,

  -- Parent for episodes; NULL for movies and series.
  parent_id       uuid REFERENCES content(id) ON DELETE CASCADE,
  season_number   int,
  episode_number  int,

  title           text NOT NULL,
  -- Arabic title held separately rather than relying on a locale row, because
  -- MENA titles are frequently BOTH — a series has an Arabic name and a Latin
  -- transliteration, and users search with either (§1.4).
  title_ar        text,
  title_original  text,
  synopsis        text,
  synopsis_ar     text,

  release_date    date,
  runtime_minutes int CHECK (runtime_minutes IS NULL OR runtime_minutes > 0),
  poster_path     text,
  backdrop_path   text,

  -- Integer, never float (§6.1 rule 4).  Basis points: 8750 = 87.50%.
  popularity_bp   int NOT NULL DEFAULT 0,
  vote_count      int NOT NULL DEFAULT 0,

  genres          text[] NOT NULL DEFAULT '{}',
  origin_country  text[] NOT NULL DEFAULT '{}',
  spoken_languages text[] NOT NULL DEFAULT '{}',

  -- ADR-012: the provider's raw payload, kept whole.  Their shape changes on
  -- their schedule; a rigid column mapping would break on it.
  provider        text NOT NULL DEFAULT 'tmdb',
  provider_ref    text,
  provider_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  provider_synced_at timestamptz(3),

  -- ADR-012: columns a human curator owns.  A provider refresh MUST NOT
  -- overwrite anything named here.  This is what protects hand-written Arabic
  -- titles from being clobbered by a nightly job.
  curated_fields  text[] NOT NULL DEFAULT '{}',

  -- Editorial boost window for new content (§10.3 cold start).
  editorial_boost_until timestamptz(3),

  is_retired      boolean NOT NULL DEFAULT false,
  created_at      timestamptz(3) NOT NULL DEFAULT now(),
  updated_at      timestamptz(3) NOT NULL DEFAULT now(),

  CONSTRAINT content_episode_shape CHECK (
    (content_type = 'episode' AND parent_id IS NOT NULL AND season_number IS NOT NULL AND episode_number IS NOT NULL)
    OR (content_type <> 'episode' AND parent_id IS NULL)
  )
);

CREATE TRIGGER content_touch BEFORE UPDATE ON content
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- ---------------------------------------------------------------------------
-- Search — ADR-006.
--
-- search_text is generated, so it can never drift from the source columns, and
-- vybe_normalize is applied HERE at index time.  The query path applies the
-- same function; the regression test that proves it asserts that a search for
-- 'احمد' matches a row titled 'أحمد'.
--
-- Configuration is 'simple', deliberately: Postgres ships no Arabic stemmer,
-- and pretending otherwise by using 'english' on Arabic text would produce
-- confidently wrong tokens.  We do not stem Arabic and we say so.
-- ---------------------------------------------------------------------------
ALTER TABLE content ADD COLUMN search_text text
  GENERATED ALWAYS AS (
    vybe_normalize(
      coalesce(title, '') || ' ' ||
      coalesce(title_ar, '') || ' ' ||
      coalesce(title_original, '')
    )
  ) STORED;

ALTER TABLE content ADD COLUMN search_vector tsvector
  GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', vybe_normalize(coalesce(title, ''))),          'A') ||
    setweight(to_tsvector('simple', vybe_normalize(coalesce(title_ar, ''))),       'A') ||
    setweight(to_tsvector('simple', vybe_normalize(coalesce(title_original, ''))), 'B') ||
    setweight(to_tsvector('simple', vybe_normalize(coalesce(synopsis, ''))),       'D') ||
    setweight(to_tsvector('simple', vybe_normalize(coalesce(synopsis_ar, ''))),    'D')
  ) STORED;

-- Query: "full-text search over titles and synopses, weighted" (§6.4).
CREATE INDEX idx_content_fts ON content USING gin (search_vector);

-- Query: "fuzzy / typo-tolerant / prefix title match as the user types" (§6.3).
CREATE INDEX idx_content_search_trgm ON content USING gin (search_text gin_trgm_ops);

-- Query: "trending shelf — most popular non-retired titles."
CREATE INDEX idx_content_popularity ON content (popularity_bp DESC, id)
  WHERE is_retired = false;

-- Query: "episodes of a series, in order."
CREATE INDEX idx_content_parent ON content (parent_id, season_number, episode_number)
  WHERE parent_id IS NOT NULL;

-- Query: "refresh job picks the stalest provider-backed rows first."
CREATE INDEX idx_content_sync_staleness ON content (provider_synced_at NULLS FIRST)
  WHERE is_retired = false;

-- Query: "resolve a provider reference to a local row during sync."
CREATE UNIQUE INDEX idx_content_provider_ref ON content (provider, provider_ref)
  WHERE provider_ref IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Where to watch.  §1.9: VYBE deep-links to official apps and NEVER stores or
-- accepts a stream URL.  `deeplink_url` is the provider's own app link,
-- supplied by the metadata provider — never user-supplied.
-- ---------------------------------------------------------------------------
CREATE TABLE content_offers (
  id            uuid PRIMARY KEY,
  content_id    uuid NOT NULL REFERENCES content(id) ON DELETE CASCADE,
  region        text NOT NULL,
  service_name  text NOT NULL,
  service_logo  text,
  offer_type    text NOT NULL CHECK (offer_type IN ('subscription', 'rent', 'buy', 'free', 'ads')),
  deeplink_url  text,
  created_at    timestamptz(3) NOT NULL DEFAULT now(),
  updated_at    timestamptz(3) NOT NULL DEFAULT now(),
  UNIQUE (content_id, region, service_name, offer_type)
);

CREATE TRIGGER content_offers_touch BEFORE UPDATE ON content_offers
  FOR EACH ROW EXECUTE FUNCTION vybe_touch_updated_at();

-- Query: "where can I watch this, in my region" (content detail screen).
CREATE INDEX idx_offers_content_region ON content_offers (content_id, region);

-- Cast and crew, for the §6.4 ranking tier below title and above description.
CREATE TABLE content_people (
  id            uuid PRIMARY KEY,
  content_id    uuid NOT NULL REFERENCES content(id) ON DELETE CASCADE,
  person_name   text NOT NULL,
  person_name_ar text,
  role          text NOT NULL CHECK (role IN ('cast', 'director', 'writer', 'creator')),
  character_name text,
  billing_order int,
  created_at    timestamptz(3) NOT NULL DEFAULT now()
);

-- Query: "search by actor or director name" (§6.4 ranking tier 3).
CREATE INDEX idx_people_name_trgm ON content_people
  USING gin (vybe_normalize(person_name) gin_trgm_ops);

-- Query: "top-billed cast for the detail screen."
CREATE INDEX idx_people_content ON content_people (content_id, billing_order NULLS LAST);
