# ADR-006: Postgres-native search for V1, with Arabic normalisation

Status: Accepted
Date: 2026-08-25

## Context

§1.4 makes Arabic a launch requirement, not a later phase. That turns search
from a commodity feature into a correctness problem, because **Arabic search
fails silently**: a user types `احمد`, the catalogue stores `أحمد`, zero results
come back, and nothing in an English test suite notices.

The catalogue at V1 is ~200 curated titles (§16.3), not 200,000.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| `ILIKE '%term%'` | Zero setup | No ranking, no typo tolerance, sequential scan, no Arabic handling |
| **Postgres `tsvector` + `pg_trgm`** | Already in the stack, transactional with the data, no sync lag, ships Arabic support if configured deliberately | Ranking is less sophisticated than a dedicated engine; scaling ceiling exists |
| Elasticsearch / OpenSearch | Best-in-class relevance, analyzers for Arabic | A second datastore, a sync pipeline, its own failure modes and ops burden — for 200 titles. §2.2 warns against exactly this. |
| Meilisearch / Typesense | Lightweight, typo-tolerant by default | Still a second datastore and a sync pipeline; still premature at this size |

## Decision

**Postgres-native search: `tsvector` for full text, `pg_trgm` for fuzzy and
prefix matching**, with an explicit Arabic normalisation step applied **at index
time and at query time by the same function**.

Normalisation (§6.4), implemented once as `vybe_normalize(text)` in SQL so the
two paths can never diverge:

| Transform | Example |
|---|---|
| Hamza forms to bare alif: `أ إ آ ٱ` to `ا` | `أحمد` to `احمد` |
| Ta marbuta to ha: `ة` to `ه` | `مدرسة` to `مدرسه` |
| Alif maqsura to ya: `ى` to `ي` | `مصطفى` to `مصطفي` |
| Strip tashkeel (U+064B–U+0652) and tatweel (U+0640) | `مُحَمَّد` to `محمد` |
| Unicode NFKC, then `unaccent` for Latin | `Pokémon` to `Pokemon` |

Ranking order, per §6.4: exact prefix, then title trigram similarity, then
cast/crew, then description, with a popularity tiebreak.

**Migration trigger, stated now so it is a decision and not a surprise:** move to
a dedicated engine when the catalogue exceeds ~100,000 titles, *or* when p95
search latency exceeds 300ms, *or* when relevance requires per-language
stemming beyond what a Postgres configuration provides. Not before.

## Consequences

**Becomes easy**

- Search results are transactionally consistent with the catalogue. A title
  added in a transaction is searchable in that same transaction — no index lag,
  no reconciliation job, no "why isn't my new title showing up".
- One datastore to back up, monitor, and restore (§14.3).

**Becomes hard**

- Postgres has no built-in Arabic text search configuration. We use `'simple'`
  over pre-normalised text rather than pretending a stemmer exists. This is a
  known, documented limitation: **VYBE does not stem Arabic**, so `كتاب` and
  `كتب` are distinct tokens. For 200 curated titles matched largely by title,
  this is acceptable; it is recorded in `docs/TESTING.md` as a known gap rather
  than hidden.
- The normalisation function must be applied at query time too. A test asserting
  that `احمد` finds `أحمد` is mandatory and guards this exact regression.

**Revisit at** the migration triggers above.
