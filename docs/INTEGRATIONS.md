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
| **Probe** | `server/cmd/probe` → `cd server && go run ./cmd/probe tmdb` |
| **Output** | `server/tools/probe/out/tmdb.json` (gitignored; the key is redacted in every recorded URL) |
| **Status** | 🟢 **PROBED** — 10/10 calls answered, observed `2026-08-25T07:00:29Z` |

Everything below was read off live responses. Where something was **not**
observed it says so, and no adapter depends on it.

### Auth

v3 endpoints authenticate with `api_key=<key>` as a **query parameter**, and
that is what `cmd/probe` and the adapter use. TMDB also issues a v4 "API Read
Access Token" (a JWT, sent as `Authorization: Bearer`). Both were issued to this
account; the v4 token is stored in `.env` as `TMDB_READ_ACCESS_TOKEN` and is
currently **unused** — no v3 endpoint we need requires it.

### Envelopes

Paginated list endpoints (`/search/*`, `/discover/*`):

```
{ page: int, results: [ … ], total_pages: int, total_results: int }
```

Movie search result item, observed in full:

```
adult:boolean  backdrop_path:string  genre_ids:[int]  id:int
original_language:string  original_title:string  overview:string
popularity:float  poster_path:string  release_date:string
softcore:boolean  title:string  video:boolean
vote_average:float  vote_count:int
```

> **`softcore` is a real field and is absent from most TMDB documentation.**
> It is recorded here precisely because a mapping written from memory would not
> have contained it. This is §0.3 rule 1 earning its place.

`/movie/{id}` adds `belongs_to_collection` (**nullable object** — observed
`null`), `budget`, `genres:[{id,name}]`, `homepage`, `imdb_id`,
`origin_country:[string]`, `production_companies`, `production_countries`,
`revenue`, `runtime`, `spoken_languages`, `status`, `tagline`.

`/tv/{id}/season/{n}` returns `{_id, air_date, episodes:[…], id, name,
overview, poster_path, season_number, vote_average}`. Each episode carries
`episode_number`, `season_number`, `show_id`, `still_path`, `runtime`,
`episode_type`, and inline `crew` / `guest_stars`. This gives the
`content_type` discriminator and the `parent_id`/season/episode columns
directly.

### Images are paths, not URLs

`poster_path`, `backdrop_path`, `still_path`, `profile_path` and provider
`logo_path` are **relative paths** (`/8ZTVqvKDQ8emSGUEMjsS4yHAwrp.jpg`). They
are unusable without `/configuration`, observed as:

| | |
|---|---|
| `secure_base_url` | `https://image.tmdb.org/t/p/` |
| `poster_sizes` | `w92 w154 w185 w342 w500 w780 original` |
| `backdrop_sizes` | `w300 w780 w1280 original` |
| `logo_sizes` | `w45 w92 w154 w185 w300 w500 original` |
| `still_sizes` | `w92 w185 w300 original` |
| `profile_sizes` | `w45 w185 h632 original` |

A URL is `secure_base_url + size + path`. The size list is **server-supplied and
must not be hardcoded in the client** — the backend composes URLs, which also
keeps the key-free contract of NFR-19 intact.

### Rate limits — the answer is "no headers at all"

**Observed: TMDB returned none of `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
`X-RateLimit-Reset` or `Retry-After` on any of the 10 probe calls, nor on
`/configuration`.** The probe explicitly requests those four header names
(`cmd/probe/main.go`), so this is an observed absence, not a gap in the probe.

Consequence, and it is a real one: **our limiter cannot be adaptive.** There is
no ceiling to read and no reset to honour, so §2.4 R3 is answered by making the
limit a conservative static config value, not by tracking headers.

**Not observed:** we never triggered a `429`, so whether a throttled response
carries `Retry-After` is **unknown**. The adapter must therefore treat a missing
`Retry-After` on 429 as the expected case and back off on its own schedule. Do
not write code that assumes the header exists.

### Caching — observed `Cache-Control`

Every 200 carried `public, max-age=…` and a weak `ETag`. Observed values:

| Endpoint | `max-age` |
|---|---|
| `/search/movie`, `/search/tv` | 24 533 – 26 575 s (~7 h) |
| `/movie/{id}` `language=en-US` | 12 604 s (~3.5 h) |
| `/movie/{id}` `language=ar` | 5 470 s (~1.5 h) |
| `/tv/{id}/season/{n}` | 14 008 s (~3.9 h) |
| `/movie/{id}/credits` | 3 274 s (~55 min) |
| `/movie/{id}/watch/providers` | 3 120 s (~52 min) |
| `/configuration` | 3 340 s (~56 min) |

These are the real inputs to the §11.1 cache tiers, and the weak `ETag` means
conditional revalidation is available. Note the shortest windows belong to
`watch/providers` and `credits` — the two most volatile payloads — which is
consistent with treating offers as a **Volatile** tier rather than caching them
with title metadata.

### Errors

A miss returns HTTP **404** with a body that is *not* the list envelope:

```
{ success: false, status_code: 34, status_message: "The resource you requested could not be found." }
```

`status_code` is TMDB's own code (**34**), independent of the HTTP status, and
must not be confused with it. This maps cleanly onto the §4.4 `Failure`
hierarchy: switch on HTTP status, carry `status_message` as diagnostic detail
only, and never surface TMDB's English prose to a user.

---

### The Arabic coverage question — answered, with a caveat that changes the plan

Three probes were Arabic because §1.4 stakes the launch on Ramadan musalsalat.
The headline: **Arabic works, and named musalsalat are present.**

Search on `الفيل الأزرق` returned exactly 4 results — the film (2014), both
sequels, and the collection — all with Arabic-script titles and
`original_language: "ar"`. Live follow-up queries found `الاختيار` (2020),
`ما وراء الطبيعة` (2020), `لعبة نيوتن` (2021), and the whole `رامز` Ramadan
franchise through 2025. These are exactly the titles the wedge targets.

**But metadata completeness is uneven, and one gap is severe.** Sampling the 60
most popular Arabic-original titles of each kind (`/discover`, `language=ar`,
`with_original_language=ar`):

| | Arabic **TV** | Arabic **film** |
|---|---|---|
| **empty `overview`** | **23 %** | **80 %** |
| missing air/release date | 0 % | 5 % |
| missing poster | 3 % | 10 % |
| missing genres | 8 % | 22 % |

Identity, artwork and dates are good enough to build on. **Arabic synopsis is
not** — four out of five Arabic films have none.

**And TMDB does not fall back.** Requesting `language=ar` on a title with no
Arabic translation returns `overview: ""` — an empty string, not the English
text. Verified against 5 such films: all 5 had a non-empty English `overview`
(130–868 chars) sitting behind the empty Arabic one.

Three consequences, all load-bearing:

1. **The fallback is ours to write.** The adapter fetches `ar`, and on an empty
   field falls back to `en-US`, recording **which locale each field came from**
   so the UI can honestly mark a synopsis as shown in English rather than
   silently presenting it as Arabic.
2. **ADR-012's curated-fields path is promoted.** For Arabic film synopses it is
   not a supporting feature, it is the primary pipeline. That is the editorial
   cost §1.4 has to budget for, and per this file's own standing instruction it
   is surfaced rather than quietly absorbed.
3. **`title` under `language=ar` is not guaranteed Arabic script.** Observed
   `"Arwah Saghirah"` (transliteration) and
   `"Mémoires anachroniques ou le couscous du vendredi midi"` (French) returned
   for `language=ar`. So `vybe_normalize()` (ADR-006) must handle Latin,
   transliterated and Arabic forms in the same column — it cannot assume the
   `ar` field is Arabic.

Generic Arabic queries rank poorly: searching the bare word `مسلسل` ("series")
returned Hotel Transylvania and Korean dramas, because TMDB matches localised
`name` fields and Arabic names of foreign shows begin with that word. Provider
search alone is therefore **not** sufficient for the MENA wedge; §6.4's local
ranking tiers carry real weight rather than being a refinement.

### Where-to-watch (FR-9) — and what it means for L1

`/movie/{id}/watch/providers` returned **112 regions**, `EG` among them, keyed
by ISO-3166-1. The `EG` block for the probed title:

```
link     : https://www.themoviedb.org/movie/27205-inception/watch?locale=EG
flatrate : Shahid VIP            (provider_id 1715)
rent/buy : Apple TV Store (2), Google Play Movies (3)
```

Each offer is `{provider_id, provider_name, logo_path, display_priority}`.
**Shahid VIP being present matters** — the dominant MENA streamer is covered for
the launch region.

**The legal point, now evidence-backed rather than asserted:** the only URL in
the entire payload is a link to *themoviedb.org's own* watch page. TMDB supplies
**no per-provider deeplink and no stream URL of any kind.** There is nothing in
this response that could facilitate unauthorised viewing even if VYBE wanted it
to. `LEGAL.md` L1 claims that boundary is closed by architecture; this probe
shows the upstream data cannot breach it either. `content_offers` therefore
stores provider identity and display priority only.

### Compliance notes (design; terms still unread — LEGAL.md L2 stays open)

- Required attribution renders on every surface showing their data.
- Our limiter sits under their ceiling — **static, because no header exposes one**.
- Responses cached within the observed `max-age` windows above.
- No bulk redistribution of the catalogue.
- Provider **logos** are TMDB-hosted assets with their own usage terms; using
  them in `content_offers` UI is **not yet checked** against those terms.

> L2 remains open. Probing an API tells you how it behaves, not what you are
> permitted to do with it. That still needs a human to read the current terms.

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

- [x] Probe written, covering success, error, rate-limit, and the edge cases
      specific to our domain (for TMDB: Arabic)
- [x] Probe run against the **live** API, output recorded as evidence
- [x] Observed shapes transcribed here, with the observation date
- [x] Rate limits observed from real response headers, not from documentation
      — observed result: **no such headers exist**
- [x] Error envelope mapped to our `Failure` hierarchy (§4.4)
- [ ] Terms of use read against the current published version, and dated
      (LEGAL.md L2 — needs a human)
- [ ] Attribution requirements implemented
- [x] Key held server-side; CI asserts it is absent from the client (NFR-19)
- [ ] Outage behaviour decided and tested — for TMDB: browse and search continue
      from local `content`, only refresh degrades (EC-18)
