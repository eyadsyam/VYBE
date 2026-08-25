# Legal, compliance, and store-review review

> §1.9: **"This section can kill the project. Address it in Milestone 0, not at
> the end."**

This is the M0 review. Status is honest: some items are **designed out** (the
architecture makes the risk impossible), some are **mitigated** (a control
exists and is tested), and some are **open** (they need a human with authority,
and no amount of engineering closes them).

**None of this is legal advice.** It is an engineering record of which risks the
architecture addresses and which ones a lawyer still has to answer.

| Status | Meaning |
|---|---|
| ✅ **Designed out** | The system cannot do the risky thing, by construction |
| 🟡 **Mitigated** | A control exists, and a test proves it |
| 🔴 **Open** | Needs a decision or check outside engineering |

---

## L1 — App Store / Play rejection for facilitating unauthorised viewing

**Severity: Critical. Status: ✅ Designed out.**

This is the risk that ends the project, and it is the reason ADR-002 exists.

VYBE never hosts, proxies, embeds, transcodes, or links to a video stream. The
architecture makes this structural rather than a policy someone must remember:

| Control | Where it is enforced |
|---|---|
| No stream URL is accepted anywhere in the API | FR-10. There is no field in any request body, in `server/api/openapi.yaml`, or in any table that holds one. |
| No video is stored | The schema has no media table. `content.poster_path` and `backdrop_path` are image references from the metadata provider. |
| What is synchronised is a **clock**, not a stream | ADR-002. `t_room` is a function of two timestamps and knows nothing about video. |
| Where-to-watch links go to official apps only | `content_offers.deeplink_url` is populated from the metadata provider, never from user input. |

**Store review notes** (to be submitted with the first build — drafted now so it
is not written under deadline pressure):

> VYBE is a companion app. It does not stream, host, or link to unauthorised
> video. Users watch content in their own licensed streaming apps; VYBE
> synchronises a shared timer so friends can watch simultaneously and play
> along with trivia. Where-to-watch information links only to official
> provider applications. The app accepts no user-supplied media URLs.

**Residual risk.** A reviewer may still misread the category. Mitigation: the
review notes above, a demo account pre-seeded with a room, and the fact that no
build contains a video player at all.

---

## L2 — Metadata provider terms and attribution (TMDB)

**Severity: High. Status: 🔴 Open — blocked on BLOCKER-01.**

ADR-012 designs the compliance controls:

- Attribution renders on every surface showing provider data.
- Our own rate limiter sits under the provider's ceiling.
- Responses are cached within the permitted window; nothing is bulk
  redistributed.
- The key lives server-side only and never ships in the binary (NFR-19, and CI
  greps the client source for it).

**What is genuinely open:** the terms have not been read against the current
published version, because the integration has not been built yet — and reading
them now against an unbuilt integration would produce a false sense of closure.

**Action required before any public build:**
1. Read TMDB's current Terms of Use and API attribution requirements in full.
2. Confirm the attribution wording and logo placement they mandate today.
3. Confirm the caching window they permit.
4. Record the date they were read. **They change** — §2.4 R3 rates this High
   impact / Medium likelihood, and a term read eighteen months ago is not a
   term you have read.

---

## L3 — Trademark on the name "VYBE"

**Severity: High. Status: 🔴 Open — needs a human, and needs it early.**

§1.9 is explicit that this must happen **before any design work**, because the
cost of a rename rises with every asset, store listing, and domain that carries
the name.

"Vybe" and "Vibe" are common word marks. A conflict is not unlikely.

**Action required — cannot be done by engineering:**
1. Search the relevant trademark registries for the classes that matter
   (software, entertainment services) in the launch jurisdictions: Egypt, the
   GCC, plus EUIPO and USPTO if expansion is intended.
2. Check app-store name availability on both stores.
3. Check domain availability for `vybe.app` — note that §1.9 already assumes
   this domain for Universal Links, and that assumption is load-bearing for
   FR-13.
4. **Have a fallback name chosen before the first store submission.**

**Engineering mitigation already in place:** the name appears in exactly three
places that would need changing — `app/lib/l10n/*.arb` (`appTitle`), the
bundle/application id, and the Universal Link domain. It is not scattered
through the codebase, so a rename is hours rather than days.

---

## L4 — Deep link scheme collision

**Severity: Medium. Status: ✅ Designed out.**

`vybe://` is not unique and is trivially hijacked by another app registering
the same scheme. FR-13 therefore makes **Universal Links / App Links** the
primary mechanism (`https://vybe.app/r/{code}`), with a custom scheme permitted
only as a fallback.

The security property that matters is separate from the scheme, and it holds
either way: **FR-14 requires the server to authorise on resolve.** Possession of
a link grants nothing. A hijacked scheme yields an attacker a room code and no
access, and AC-30 tests exactly that.

---

## L5 — GDPR and Egypt PDPL (Law 151/2020)

**Severity: High. Status: 🟡 Mitigated in design, 🔴 open on process.**

Launching MENA-first (§1.4) means Egypt's PDPL applies alongside GDPR for any
EU user.

**In place by design:**

| Requirement | Where |
|---|---|
| Retention schedule per data category | §6.5, encoded in the schema comments and the reaper jobs |
| Data minimisation | §5.3 — the access token carries `sub`, `sid`, `entitlement_tier` and nothing else |
| IP truncation | `sessions.ip_truncated` is `inet`, truncated before insert (§12.6) |
| Chat retention with a moderation exception | 90 days, longer under active report (§6.5) |
| Analytics pseudonymisation after 90 days | §6.5 |
| Deletion cascade | 30-day grace, then hard delete; moderation records retained pseudonymously |

**Open, and needs a human:**
- A field-level PII inventory in `docs/PRIVACY.md`: what, why, lawful basis,
  retention, who can access. The schema is the input to this; the document does
  not exist yet.
- Lawful basis per category — consent for analytics, contract for account data.
- Export and delete endpoints (data subject requests). Designed for; not built.
- Breach notification process and owner.
- Whether PDPL registration with Egypt's Data Protection Centre is required at
  this scale.

---

## L6 — Minors in social rooms

**Severity: Critical. Status: 🟡 Mitigated — enforced in schema and spec, not yet built.**

§12.4 is unambiguous: *"Do not ship the social layer without this."*

Encoded in FR-2 and AC-31, and in the schema:

- `users.age_band` is an enum, stored explicitly rather than recomputed at each
  call site — so a missed recomputation cannot silently grant an adult
  capability to a minor.
- `users.is_discoverable` defaults from the age band.
- Under-16 accounts: no public rooms, no discoverability, no public
  leaderboards, no DMs (which do not exist at all — §2.2 "Never").

**Open:**
- Age assurance beyond a self-declared date of birth. A date picker is a weak
  control and should be described as one, not as compliance.
- Whether the launch jurisdictions require verifiable parental consent below a
  given age, and at what age.
- Stricter chat filtering for minors is specified but not implemented (M6).

---

## L7 — UGC liability (chat, room names, reports)

**Severity: High. Status: 🟡 Mitigated in design.**

§12.4's ladder is specified, and the schema supports the parts that must be
durable:

- `chat_messages.deleted_at` / `deleted_by` — moderation is an audit trail, not
  a delete.
- `reports.evidence` is a JSONB snapshot captured **at report time**, because
  the content may be deleted before review, and §1.9 makes preservation a legal
  requirement rather than a nicety.
- `moderation_actions` retains actor, reason, and appeal outcome for 2 years
  (§6.5), surviving account deletion pseudonymously.
- SLA due dates are a stored column, so the §14.2 breach alert is a query
  rather than a hope.

**Open:** a published notice-and-action policy, a named human escalation path,
and a decision on which jurisdiction's takedown regime applies.

---

## Summary — what engineering cannot close

| # | Item | Needs |
|---|---|---|
| **L3** | Trademark and name availability | A search, in the launch jurisdictions, **before** more assets carry the name |
| **L2** | TMDB terms read against the current version | A read, dated, before any public build |
| **L5** | PII inventory, lawful basis, DSR endpoints, breach process | A privacy owner |
| **L6** | Age assurance strategy; parental consent thresholds | A policy decision |
| **L7** | Notice-and-action policy; human escalation path | A policy decision |

L1 and L4 are closed by architecture. The rest are open, and listing them as
open is the point — §1.9 exists because these are the items a portfolio project
silently skips and a real product cannot.
