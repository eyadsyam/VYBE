# ADR-012: TMDB behind a `CatalogProvider` port, backend-proxied and locally cached

Status: Accepted
Date: 2026-08-25

## Context

Risk R3 in §2.4: *"Metadata provider terms change or rate-limit."* High impact,
medium likelihood, and outside our control. §1.4 adds a second constraint that
rules out a naive integration: **TMDB alone is insufficient for MENA content.**
Arabic titles, transliterations, and regional providers are unevenly covered,
and Ramadan musalsalat — the entire launch wedge — are exactly the long tail
where coverage is weakest.

§0.3 rule 1 forbids inventing provider response shapes. This ADR therefore
records the *design*; the actual observed shapes go in `docs/INTEGRATIONS.md`
only after a probe has run against the real API.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| Call TMDB directly from the Flutter app | Fewest moving parts | Ships an API key in a decompilable binary (§12.5 forbids it); no shared cache, so every device burns quota independently; terms change breaks shipped clients |
| **Backend proxy behind a `CatalogProvider` port, mirrored into local `content`** | One key, server-side; one shared cache; swappable provider; works offline; Arabic curation possible | We own a sync job and a staleness policy |
| Bulk-import the whole TMDB catalogue | Fast queries, no runtime dependency | Almost certainly breaches redistribution terms (§1.9); enormous; most of it irrelevant to 200 curated titles |
| Multiple providers behind one facade from day one | Best coverage | Premature. Build the port; add the second provider when coverage data proves it is needed. |

## Decision

**A `CatalogProvider` port with a TMDB adapter, called only from the backend,
with results mirrored into the local `content` table as the queryable source of
truth.**

```go
type CatalogProvider interface {
    Search(ctx context.Context, q Query) ([]ProviderItem, error)
    Details(ctx context.Context, ref ProviderRef) (ProviderItem, error)
    WhereToWatch(ctx context.Context, ref ProviderRef, region string) ([]Offer, error)
}
```

Consequences of that shape:

1. **The app never talks to TMDB.** It talks to `/v1/content/...`. The key lives
   in server configuration and is never in the binary (§12.5).
2. **`content` is the source of truth for queries**, not TMDB. Search (ADR-006),
   ranking (ADR-007), and offline cache (§11.1) all read local rows. A TMDB
   outage degrades *refresh*, not *browse* — the honest failure mode.
3. **Provider payloads land in JSONB** `provider_metadata` with generated columns
   for hot fields (§6.2). Provider response shapes change; a rigid column
   mapping would break on their schedule, not ours.
4. **A manual curation path is first-class, not a fallback.** Arabic titles,
   transliterations, and MENA availability are editable by an operator and are
   **never overwritten by a provider refresh** — a `curated_fields` mask records
   which columns a human owns. Without this, the launch wedge is at the mercy of
   someone else's coverage.
5. **Attribution and terms compliance are code, not intent.** The required TMDB
   attribution renders on every screen showing their data; rate limits are
   enforced client-side by our own limiter, well under their ceiling; responses
   are cached within their permitted window; nothing is bulk-redistributed.

### Open item, honestly recorded

**The TMDB probe has not been run.** It requires a free API key
(https://www.themoviedb.org/settings/api) that is not present in this
environment. Per §0.3 rule 1, `docs/INTEGRATIONS.md` records **no response
shapes** until `go run ./cmd/probe tmdb` has executed against the live API and
written its observed output. The probe is implemented and ready; the M0 exit
criterion "provider integration probe run against the real API" is **not met**
until then. This is tracked as **BLOCKER-01**.

## Consequences

**Becomes easy**

- Swapping or adding a provider is one adapter. R3's mitigation is structural.
- The catalogue works fully offline and under provider outage.
- Quota is one shared server-side budget, not N devices.

**Becomes hard**

- We own a refresh job, a staleness policy, and the curated-field merge rule.
  A bad merge silently overwrites a human's Arabic title — so that merge is
  unit-tested with the curated mask as its central case.
- Legal review of the current TMDB terms is required before launch, and they do
  change. Recorded as a recurring pre-launch checklist item in `docs/LEGAL.md`.
