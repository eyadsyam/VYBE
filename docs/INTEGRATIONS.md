# Integrations

> §0.3 rule 1: **"Never invent third-party API behaviour. If you do not know how
> a provider responds, write a small integration probe, run it, record the real
> response shape in `docs/INTEGRATIONS.md`, then build against that."**

This file records **observed** provider behaviour. Nothing is written here from
memory, from documentation, or from a model's recollection of an API. If a
section says "not yet observed", that is the honest state and the corresponding
adapter has not been written.

---

## TMDB — content metadata

| | |
|---|---|
| **Role** | Primary catalogue source (ADR-012) |
| **Access** | Backend only. The key never reaches the client (NFR-19, §12.5). |
| **Probe** | `server/cmd/probe` → `go run ./cmd/probe tmdb` |
| **Output** | `tools/probe/out/tmdb.json` |
| **Status** | 🔴 **NOT YET PROBED** |

### Response shapes

**None recorded.**

The probe has not been run, because `TMDB_API_KEY` is not set in this
environment (BLOCKER-01). Per §0.3 rule 1, no shapes are documented until the
probe has executed against the live API and written its observed output.

**This is deliberate and load-bearing.** The catalogue adapter, the
`content.provider_metadata` mapping, the `curated_fields` merge rule, and the
`content_offers` shape all depend on what TMDB actually returns — not on what
its documentation says it returns, and not on what anyone remembers. Writing a
mapping now would produce code that compiles, looks correct in review, and
breaks on first contact.

### What the probe will answer

Each call exists because the adapter cannot be written without the answer:

| Probe | Question it answers |
|---|---|
| `search_movie_latin` | Field names, pagination envelope, date and poster path formats |
| `search_movie_arabic_query` | **Does an Arabic-script query return anything at all?** Determines whether provider search is usable for the MENA wedge, or whether local search must carry it entirely |
| `search_tv_arabic_musalsal` | Do Ramadan musalsalat exist in TMDB, and how are titles transliterated? This is the §1.4 launch wedge |
| `movie_details_ar_locale` | Which fields localise under `language=ar` and which stay English — drives `title_ar` / `synopsis_ar` and the curated mask |
| `movie_details_en_locale` | The same record in English, to diff against the `ar` response |
| `watch_providers_eg` | Where-to-watch offers for region EG (FR-9); whether deeplinks are supplied |
| `tv_season_episodes` | Episode shape for the `content_type` discriminator and `parent_id`/season/episode |
| `credits_for_search_ranking` | Cast/crew payload for `content_people` and §6.4's third ranking tier |
| `rate_limit_headers` | **What rate-limit headers does TMDB return today?** Our limiter must sit under their real ceiling, and their documented limits have changed before (§2.4 R3) |
| `not_found_error_shape` | Error envelope, so provider errors map to our `Failure` hierarchy rather than being guessed |

### The Arabic coverage question

Three of the ten probes are Arabic. That is not thoroughness for its own sake:
§1.4 stakes the entire launch strategy on Ramadan musalsalat, and TMDB's MENA
coverage is the largest unvalidated assumption in the product.

If coverage turns out to be poor, that is a **product finding that should change
the plan** — ADR-012's manual curation path stops being a supporting feature and
becomes the primary content pipeline, with a corresponding cost in editorial
effort. It must not be quietly absorbed.

### Compliance notes (design, not yet verified against current terms)

Designed in ADR-012, **not yet checked against TMDB's live terms** — see
`docs/LEGAL.md` L2:

- Required attribution renders on every surface showing their data.
- Our own rate limiter sits under their ceiling (value TBD from the probe).
- Responses cached within their permitted window.
- No bulk redistribution of the catalogue.

---

## Firebase Cloud Messaging — push notifications

| | |
|---|---|
| **Role** | Push delivery (M5) |
| **Status** | 🔴 Not started — M5, out of scope for M0/M1 (OS-14) |

The vertical slice is validated with both apps foregrounded, so push is not on
the M1 critical path. No probe written.

---

## Provider integration checklist

Applied to every provider before its adapter is written:

- [ ] Probe written, covering success, error, rate-limit, and the edge cases
      specific to our domain (for TMDB: Arabic)
- [ ] Probe run against the **live** API, output committed as evidence
- [ ] Observed shapes transcribed here, with the observation date
- [ ] Rate limits observed from real response headers, not from documentation
- [ ] Error envelope mapped to our `Failure` hierarchy (§4.4)
- [ ] Terms of use read against the current published version, and dated
- [ ] Attribution requirements implemented
- [ ] Key held server-side; CI asserts it is absent from the client (NFR-19)
- [ ] Outage behaviour decided and tested — for TMDB: browse and search continue
      from local `content`, only refresh degrades (EC-18)
