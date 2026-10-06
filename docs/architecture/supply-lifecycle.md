# Supply lifecycle, freshness and geographic expansion (Phase 4A.5)

**Status:** authoritative for how long Majo treats supply as live, and how widely it may be searched. It supersedes the "listing expiry default 7 days" wording in Architecture Lock V2. The Lock (project doc `architecture/architecture-lock-v2.md`) is updated to match.

**Principle:** Majo never confuses "we have not found a buyer" with "the item no longer exists".

**Labels:**
- **[ARCH]**: architecture; changing it needs a redesign.
- **[PARAM]**: configuration, a hypothesis to tune.

## 1. Four separate concepts [ARCH]

| Concept | Question | Where | Lifetime |
|---|---|---|---|
| **Media retrieval window** | Can Majo still download a vendor's photo from WhatsApp? | Media ingestion job (4A.1) | Provider limit, about 7 days [PLATFORM]. Irrelevant once the MediaAsset is stored. |
| **Freshness** | How recently was this supply verified? | `last_verified_at` + freshness window | Derived on every evaluation; never stored as a state |
| **Lifecycle** | How long is this published listing usable marketplace supply? | `published_at` → `expires_at` (the ceiling) | 60 days [PARAM], fixed when the lifecycle starts |
| **Geographic expansion** | How widely may this listing be searched against demand right now? | Derived from lifecycle age + policy | Changes with age; never stored |

**Separate from all four:** a raw SupplySignal's own evidence window (`supply_signals.expires_at`, DB default 7 days) only governs *uncurated* signals as matching evidence. It is not the listing lifecycle.

## 2. Freshness: what `last_verified_at` means

`last_verified_at` is the last time Majo had evidence that the supply exists.

**What sets it:**
- The supply evidence that created the listing (the signal's arrival time).
- The vendor posting the same supply again (duplicate refresh). `SUPPLY_LISTING_DUPLICATE_REFRESHED` carries `"verification": "vendor_resubmission"`.
- An operator's explicit verification (`POST /api/supply-listings/{id}/verify`, `SUPPLY_LISTING_VERIFIED` and an audit row).
- *Later (4B):* a vendor reconfirming in response to a buyer enquiry.

**What never sets it:**
- reads, searches, matches;
- opening the dashboard;
- publication, editing, rejection;
- a buyer viewing or enquiring (until a vendor actually reconfirms).

**States** (`WindowFreshness`, window [PARAM] default 72h):
- **FRESH** while the verification is younger than the window: age < window.
- **STALE** from exactly the window onwards.
- **UNVERIFIED** when there is no verification time.

**Stale is not expired.** A stale listing stays eligible and carries the note `stale_needs_reconfirmation`. It never disappears because of age alone.

## 3. Lifecycle

**When it starts and ends:**
- The lifecycle starts at **publication** (`published_at`). The persisted ceiling is `expires_at = published_at + max lifecycle` (default **60 days** [PARAM]), written in the publishing transaction.
- Drafts have only a provisional ceiling (evidence time + max lifecycle). A draft older than that cannot be published; the vendor must post again.

**Boundary [ARCH]:** a listing is live for lifecycle ages in `[0, 60 days)`. At exactly 60×24h after publication it is **expired**. Day 59 is eligible; day 60 is not.

**What never moves the ceiling:**
- verification, duplicate refresh, edits, a change of configuration, reads.
- Each listing keeps the ceiling it got when its lifecycle started. If the config changes, only new lifecycles get the new ceiling.

**Expiry is derived, not swept:**
- No job flips the status. A listing past `expires_at` may still say `ACTIVE` in the table.
- The eligibility policy is authoritative and treats it as expired. Matching and Discovery never use the status alone.
- The supply metric "ACTIVE listings past their lifecycle" shows how many rows a future reconciliation would mark.

**Expired is not deleted [ARCH]:**
- Expiry is an eligibility boundary, not a data-retention instruction.
- The listing, its vendor, source SupplySignal, source messages, MediaAssets, events, audit history and the publication decision all stay. Detail, search (`lifecycle=expired`) and media previews keep working.
- Nothing deletes rows or media because of expiry.

**No silent resurrection [ARCH]:**
- An expired listing cannot be verified, edited or refreshed back to life (409 `LIFECYCLE_EXPIRED`).
- New vendor evidence after expiry creates a **new DRAFT**: a new lifecycle with a new L-reference. The old listing stays as history.
- *Future:* linking "the same TV, reposted" across lifecycles (reincarnation/dedupe) is deliberately not built.

## 4. Geographic expansion (initial deterministic policy)

| Stage | Begins at lifecycle day [PARAM] | Scope | Eligible levels |
|---|---|---|---|
| LOCAL | 0 | ORIGIN_AREA | EXACT_AREA, COMMUNITY, LGA |
| NEARBY | 20 | NEARBY_AREAS | + CITY |
| STATE | 30 | STATE | + STATE |
| REGION | 45 | REGION | + REGION |
| EXPIRED | the ceiling (60) | NONE | — |

**Rules:**
- **Stage boundaries:** a stage begins at exactly its day. Day 20 is NEARBY, day 19 is LOCAL.
- **Equal starts:** these skip the earlier stage.
- **Local start after day 0:** if `SUPPLY_GEO_LOCAL_DAYS` > 0, new listings are PENDING (not searchable) until then.
- **The result also gives** the next stage and when it begins. The last change before the ceiling is EXPIRED.

**Origin:**
- The listing's location (which already falls back to the vendor's primary location), resolved by a `geo.Resolver` into country > region > state > city > LGA > community.
- Today: a small deterministic pilot gazetteer (`geo.NigeriaPilot`: Benin City, Port Harcourt, Warri, Lagos, Abuja and their states/zones).
- Anything else is "unresolved". It is still eligible, flagged `origin_unresolved`, and the scope applies to the raw text.

**Not built:** no GIS, no PostGIS, no coordinates, no LGA adjacency data (so NEARBY widens to the origin city), and no national LGA database.

**Nigeria-first, not Nigeria-only:** levels are generic (state = state/province, LGA = district), and Nigeria's mapping is data.

**Geographic expansion never sends anything.** It answers "what geography may this listing be searched against?". Contacting buyers belongs to Distribution (4B).

## 5. Eligibility: the one canonical policy [ARCH]

```
SupplyListing → ListingFacts → EligibilityPolicy.Evaluate(facts, now)
                                   ├── LifecyclePolicy          (CeilingLifecycle)
                                   ├── FreshnessPolicy          (WindowFreshness)
                                   └── GeographicExpansionPolicy (StagedExpansion + geo.Resolver)
                               → Eligibility{eligible, reasons, notes, lifecycle, freshness, geography}
```

**Properties of `Evaluate` (`internal/supply/eligibility.go`):**
- **Pure:** no database, no network, no clock (`now` is an argument), no randomness. The same listing, configuration and instant always give the same result (tested).

**Eligible means all of:**
- status ACTIVE;
- inside the lifecycle;
- vendor ACTIVE (when known);
- has a product and a price;
- geographic stage started.

**Blocking reason codes:** `not_published`, `rejected`, `status_<STATUS>`, `lifecycle_expired`, `vendor_inactive`, `no_product`, `no_price`, `geo_scope_not_started`.

**Non-blocking notes:** `stale_needs_reconfirmation`, `origin_unresolved`.

**Consumers:**
- **Matching (today).** `routing.SuggestVendors` loads ACTIVE, unexpired listings (a SQL pre-filter, only an optimisation) and keeps the ones `Evaluate` says are eligible. Matching itself has no lifecycle rules. The matches API shows each listing's freshness, geo stage and `needs_reconfirmation`. Geographic scope is evaluated and shown, but **not yet used to filter candidates**.
- **Discovery (4B) contract.** Discovery must call the same `EligibilityPolicy.Evaluate` and use `eligible`, `freshness`, `lifecycle`, `geography` and `reasons`. It must not define its own lifecycle, freshness or expiry rules, so matching and discovery can never disagree about whether a listing is alive. Discovery-specific ranking (freshness boost, prior exposure) consumes these outputs; it does not recompute them.
- **Operators:** listing detail, queue, and supply metrics.

## 6. Reconfirmation (future)

A stale listing becomes *less eligible*, not deleted. Before a buyer is told something is currently available, a stale listing needs vendor reconfirmation.

**Example:** a buyer asks about a 35-day-old TV last verified 30 days ago. The listing exists, is ACTIVE and inside its lifecycle, but it is STALE. Majo asks the vendor ("Is the TV still available?") before presenting it as available. The vendor's "yes" is a verification (moves `last_verified_at`). "Sold" closes the listing (SOLD), and it is never deleted.

That workflow (VendorRequest re-confirmation, Lock V2 §M 4B.7) is 4B. The data and policy already support it: freshness state, `needs_reconfirmation`, and verification that never touches the lifecycle.

**"Published" is not "in stock":** publication means the operator approved the vendor's supply. It does not prove the item is physically available today. Only verification evidence does, and freshness says how old that evidence is. Matching still awards published listings the same availability points as in 4A. It now also reports freshness so stale evidence is visible, and scoring by freshness is a future policy decision.

## 7. Future intelligence (extension points)

Policies are interfaces. A smarter policy replaces one component (`EligibilityPolicy.WithParts`) without changing SupplyListing, Matching or the Discovery contract. `ListingFacts` already carries category, price, condition, location, vendor, created/published/verified times.

**Intended future policies (not built):**
- **Category-aware expansion:** low-value apparel stays local longer; large appliances go broader earlier; industrial equipment goes regional almost at once; scarce specialist equipment goes national.
- **Value-aware lifecycle:** high-value items may live longer.
- **Transportability:** size, weight, fragility and delivery feasibility limit how far an item can reasonably travel. No such fields exist yet. They are added only when they can be populated reliably, never as placeholders.
- **Demand-aware expansion:** widen sooner when there is no local demand, using historical conversion. This must stay explainable: a deterministic floor with intelligence enriching it, never an opaque replacement.
- **Real geography:** an LGA adjacency table or a gazetteer/geocoding provider behind `geo.Resolver`.

## 8. Configuration [PARAM]

| Variable | Default | Meaning |
|---|---|---|
| `SUPPLY_MAX_LIFECYCLE_DAYS` | 60 | Lifecycle ceiling after publication |
| `SUPPLY_FRESHNESS_WINDOW` | 72h | Verification age at which a listing becomes STALE (Go duration) |
| `SUPPLY_GEO_LOCAL_DAYS` | 0 | Day LOCAL begins |
| `SUPPLY_GEO_NEARBY_DAYS` | 20 | Day NEARBY begins |
| `SUPPLY_GEO_STATE_DAYS` | 30 | Day STATE begins |
| `SUPPLY_GEO_REGION_DAYS` | 45 | Day REGION begins |

**Validation at startup** (the process exits with a clear message otherwise):
- `0 ≤ local ≤ nearby ≤ state ≤ region ≤ max`;
- `max > 0`;
- `freshness > 0`;
- days are whole numbers from 0 to 3650.

`SUPPLY_FRESHNESS_WINDOW` replaces the Phase 4A `LISTING_FRESHNESS_WINDOW`, which was introduced in the same week and never deployed.

## 9. Lifecycle / eligibility model

```
DRAFT ──publish──▶ ACTIVE ─────────────────────────────▶ (age ≥ ceiling) EXPIRED*   *derived, row kept
  │                  │  derived while live: FRESH ⇄ STALE (verification)
  │                  │  derived stages: LOCAL → NEARBY → STATE → REGION
  │                  ├──▶ PAUSED        (not eligible)
  │                  ├──▶ SOLD          (not eligible, closed)
  │                  ├──▶ UNAVAILABLE   (not eligible, closed)
  │                  └──reject──▶ ARCHIVED (+rejected_at)  (not eligible, closed)
  └──reject──▶ ARCHIVED (+rejected_at)
```

**Equivalences that must never hold [ARCH]:**
- EXPIRED ≠ DELETED;
- STALE ≠ EXPIRED;
- GEOGRAPHICALLY EXPANDED ≠ EXPIRED;
- ACTIVE ≠ "the item exists today".

**Statuses:** no FRESH, STALE or stage status is stored (derived states, no state explosion). The persisted `EXPIRED` status exists in the schema but nothing writes it. A future reconciliation may write it for reporting; eligibility does not depend on it.

## 10. Events

| Event | Written when |
|---|---|
| `SUPPLY_LISTING_VERIFIED` | An operator verifies (real action, with audit) |
| `SUPPLY_LISTING_DUPLICATE_REFRESHED` | A vendor resubmits the same supply (`verification: vendor_resubmission`) |
| `SUPPLY_LISTING_PUBLISHED` | Payload now includes `lifecycle_expires_at` |

**Reserved, NOT emitted** (no state-changing process detects these transitions; reads never write events): `SUPPLY_LISTING_BECAME_STALE`, `SUPPLY_LISTING_GEO_SCOPE_EXPANDED`, `SUPPLY_LISTING_EXPIRED`. That last one is defined since Phase 3 and still has no writer. They will be emitted only by a future process that actually changes state, such as a reconciliation that writes the EXPIRED status.

## 11. Explicit non-goals of 4A.5

- **Not built:**
  - Discovery, Distribution, consent, batches or deliveries;
  - proactive or WhatsApp messages of any kind, templates, buttons;
  - consumer feed, pages, maps;
  - scheduler, cron, sweeps, background expansion jobs;
  - Redis, Kafka, RabbitMQ, NATS;
  - ML, embeddings, vector search, AI or learned expansion, demand prediction, recommendation ranking;
  - GIS or PostGIS, a national geography database;
  - transportability fields, category rules;
  - automatic reconfirmation, a reconfirmation workflow;
  - expired-listing reincarnation or dedupe across lifecycles;
  - a dynamic policy editor or policy tables.
- **Geography is not yet a matching filter.**
