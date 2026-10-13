# Discovery & Distribution foundation (Phase 4B)

Architecture Lock V2 §F (Discovery) and §G (Distribution). This phase is a
**controlled internal proof**: the machinery exists and is exercised end to
end against staff test numbers only. **No real buyer is messaged.**

Tags: **[ARCH]** structural decision · **[PARAM]** configurable default ·
**[PLATFORM]** WhatsApp/Meta limit · **[FUTURE]** designed for, not built.

## 1. The separation [ARCH]

| Capability | Question it answers | Package | Never does |
|---|---|---|---|
| Supply eligibility (4A.5) | Is this listing live, how fresh, who can it reach now? | `internal/supply` | rank, send |
| **Discovery** | Which eligible supply is relevant to this viewer, in what order, why? | `internal/discovery` | send, read consent, cap, dedupe, window |
| **Distribution** | Who receives what, when, how, how often? | `internal/distribution` | rank, own lifecycle/freshness/geography |
| Presentation | How is it shown on a channel? | `internal/presentation`, `internal/channel` | decide relevance or eligibility |
| Demand loop | Request → vendor → offer → selection → transaction | existing | change because discovery exists |

`Discovery.Candidates(viewer, scope, params)` is a pure pipeline:
**candidate supply → eligibility → retrieval → filtering → scoring → sorting → result.**
Distribution calls it; a future web/app client would call it with the same
contract. Nothing in Discovery knows WhatsApp.

## 2. Discovery

* **Eligibility is not re-implemented.** Every listing goes through
  `supply.EligibilityPolicy.EvaluateForViewer` (lifecycle ceiling 60 days,
  freshness 72h, geographic stage). The storage query pre-filters
  (ACTIVE, `expires_at` in the future) as an optimisation only; the policy is
  authoritative.
* **Geography** is the canonical `GeographicExpansionPolicy.Reach`: the
  narrowest level relating the listing origin and the viewer (`geo.Within`)
  must be inside the listing's current stage (LOCAL 0d, NEARBY 20d, STATE 30d,
  REGION 45d). A viewer described less precisely than the origin is never
  assumed to be inside the narrower area. Discovery has no geography logic of
  its own (a mutation test proves the gate is exercised).
* **Stale is not expired.** A stale listing stays a candidate with the note
  `stale_needs_reconfirmation` (unless `ExcludeStale`). It earns no freshness
  points and presentation must never promise availability.
* **Ranking** is a deterministic weighted sum. Every point is a `Reason`
  (`+30 category match …`); `score == Σ reason.points`.

| Dimension | Default | Meaning |
|---|---|---|
| category / product | 30 / 25 | matches declared interests, recent categories/products, scope |
| location local / nearby / state / region | 20 / 14 / 8 / 4 | viewer's level relative to the listing origin |
| freshness | 10 | fresh verification only |
| media / video | 10 / 5 | complete images; a stored video |
| budget / over_budget | 5 / −5 | within / above the viewer's stated budget |
| quality | 5 | listing quality signals |
| exposure | −15 | already shown recently (down-ranked here, hard-excluded in Distribution) |

  Tie-break: score, then `last_verified_at`, then `published_at`, then public
  reference (fully deterministic). Override with `DISCOVERY_RANKING_WEIGHTS`;
  `RankingParams.Version` hashes weights + policy into
  `discovery-det-v1+xxxxxxxx`, stored on every batch.
* **Explainable exclusions.** Each dropped listing has a stage
  (`eligibility|retrieval|filter|limit`) and a code (policy reason codes plus
  `geo_out_of_scope`, `category_mismatch`, `price_out_of_range`,
  `negative_interest`, …). Listings past the 60-day ceiling are removed by the
  storage pre-filter and produce no row; the policy still reports
  `lifecycle_expired` for them.
* **No ML, embeddings, vector search or personalisation** in 4B.

## 3. Distribution

### 3.1 Feature gate and the staff-only boundary [ARCH]
`DISCOVERY_MODE` is `OFF` (default) or `INTERNAL`. `CONTROLLED_PILOT` and `ON`
are rejected at startup. In INTERNAL, `Permits(recipient)` allows only numbers
in `DISCOVERY_INTERNAL_RECIPIENTS`. It is enforced at planning, again inside
`sendOne` immediately before sending, and for consent writes and invites. A
non-allowlisted number can never be planned for, invited, opted in by an
operator, or sent to (E2E 1 proves each).

### 3.2 Consent [ARCH]
Consent is its own concept: `discovery_preferences` (current state, one row per
buyer) plus append-only `discovery_preference_history`, written in one
transaction with an event. It is **separate from `Buyer` and `BuyerRequest`**:
making a request never opts anyone in, and recording consent never creates a
request. States: `OPTED_IN`, `OPTED_OUT`. Records carry source
(`button`, `operator_staff`, …), wording version, captured/opted-out times,
location and interests. Opt-in via WhatsApp requires a real button tap on a
prior prompt for the current wording version; text such as "yes" is not
consent. A buyer who never messaged Majo cannot be given a consent record.

**STOP** (case/whitespace-insensitive keyword, or the "Stop these messages"
row/button) sets `OPTED_OUT`, acknowledges, and is honoured at planning *and*
at send time (a delivery planned before STOP is skipped as `opted_out`). STOP
does not block ordinary commerce; the buyer can still make requests.

### 3.3 Batches and deliveries
* `POST /api/discovery/preview` is a **dry run**: nothing persisted, nothing sent.
  It reports supply considered, eligible listings, audience, exclusions with
  reasons (`opted_out`, `frequency_cap`, `geographic`, `no_suitable_candidates`,
  `unknown_buyer`, `not_permitted`, …) and per-buyer candidates with reasons.
* `POST /api/discovery/batches` creates a `PLANNED` batch (idempotent per
  `client_key`) recording ranking version, presentation version, arms, scope,
  and the plan. Creating a batch sends nothing.
* `POST /api/discovery/batches/{id}/send` is an explicit operator action.
  There is **no scheduler**. Re-sending a sent batch is a no-op.
* One `DiscoveryDelivery` per buyer per batch; deterministic dedupe key
  `batch:<batchID>:buyer:<buyerID>`.
* **Frequency cap** (`DISCOVERY_MAX_DELIVERIES_PER_BUYER` per
  `DISCOVERY_CAP_PERIOD`), **listing exposure window**
  (`DISCOVERY_EXPOSURE_WINDOW`) and an optional **delivery window**
  (`DISCOVERY_DELIVERY_WINDOW`, with timezone, may cross midnight).
* **Atomic enforcement.** `ClaimDelivery` takes a per-buyer advisory lock,
  re-checks consent, cap and per-listing exposure, records
  `discovery_exposures`, and moves `PLANNED → SENDING`. Two concurrent batches
  can never both pass the cap or expose the same listing. A failed send releases
  the exposures. Skipped deliveries (`outside_session_no_template`,
  `opted_out`, …) do not consume the cap.
* **Send-time refresh.** Before presenting, `sendOne` re-runs Discovery for the
  planned listings: a listing that expired or left the buyer's reach between
  planning and sending is dropped (E2E 4). If none remain the delivery is
  skipped rather than sent empty.
* **Partial-failure isolation.** One delivery failing, one listing's media
  failing to upload (card falls back to text), or one listing expiring never
  blocks the rest of the batch.

### 3.4 Presentation (channel-neutral model, WhatsApp renderer)
`internal/channel` is a channel-neutral message model (text, image, video,
buttons, list, template) validated against WhatsApp limits [PLATFORM]. The
WhatsApp provider is only a renderer. Arms (`ARM_A`, `ARM_B`) are assigned by
a deterministic hash and stored on the delivery; `presentation-v1` differs by
arm only in the buttons (ARM_B adds "Watch video" when a stored video exists).

* Inside the 24-hour session window: one card (image header, caption, buttons)
  or a list of up to 8 finds plus "Not interested" and "Stop these messages".
* Outside it: only the configured approved template (one quick reply opens the
  delivery). No template configured ⇒ **fail closed** (skipped, nothing sent).
* Cards show the **public L-reference** only, never vendor identity. A stale
  listing's caption says availability is to be confirmed.
* **Media reuse.** A stored `MediaAsset` is uploaded to WhatsApp once; the
  provider media id is cached (`outbound_provider_ref`, re-uploaded after
  `DISCOVERY_OUTBOUND_MEDIA_TTL`). Majo never re-downloads inbound media for
  sending.
* All outbound rich messages are recorded (`SendRecorded`) with purpose,
  entity and provider message id so delivery/read receipts attribute correctly.

## 4. Interactions: structured id > ordinal > free text [ARCH]
Taps arrive as WhatsApp `interactive.button_reply`, `list_reply` or template
`button` quick-reply and carry a structured id:

```
dd:<delivery>:show | cat:<c> | l:<ref>[:video|:ask] | not_interested | stop
dc:in:<wording> | dc:no          (consent prompt)
```

Parsing is strict. `structid.Looks()` stops anything shaped like a Majo id
from falling through to free text. A structured id always wins over the
offer-selection ordinal ("1") and over free-text extraction.

Every tap is persisted in `discovery_interactions` (idempotent per inbound
message) as `APPLIED` or `UNRESOLVED` with a reason: `malformed`, `unknown`,
`foreign` (another buyer's delivery; never revealed or acted on),
`not_sent`, `expired` (older than `DISCOVERY_INTERACTION_TTL`),
`listing_not_shown`, `listing_no_longer_available`, `consent_*`.
**Unresolved taps mutate nothing** (no request, no consent change) but are
recorded and counted for the operator.

## 5. "Ask about it" → the existing demand loop [ARCH]
The tap calls `request.Service.Create(NewRequest)`, the single entry point for
BuyerRequests (WhatsApp free text uses it too). The new request has
`origin=discovery`, `listing_id`, `discovery_delivery_id`, and a unique partial
index makes the tap idempotent per (delivery, listing).

* **Autonomous from here (Phase 4C correction).** The request names one
  curated listing, so the workflow confirms it (actor SYSTEM) and routes it to
  the listing's own vendor; matching, routing and offers still refuse a
  non-confirmed request (`EnsureConfirmed`). See `autonomous-runtime.md`.
  Typed references ("I want that one", "is it still available?") reach the
  same path through `EnquireFromContext`, with the same ownership checks.
* The buyer gets an honest acknowledgement: nothing is reserved or paid.
* **Listing provenance** flows forward without becoming a second source of
  truth: `VendorRequest.supply_listing_id` → `Offer.supply_listing_id` (inherited)
  → Selection → Transaction. `origin` stays derived.
* **Reconfirmation hook.** Routing a listing-origin request tells the listing's
  vendor which listing is asked about and requests reconfirmation. A reviewed
  AVAILABLE reply calls `ListingService.ReconfirmFromVendor`, which moves
  `last_verified_at` **only**. It never touches the 60-day ceiling and is
  audited/evented as a verification.

## 6. Attribution and metrics
Every outbound message, tap, request, offer, selection and transaction links
back to the delivery. `GET /api/discovery/deliveries/{id}` returns the chain and
whether the delivery would be permitted now; `GET /api/discovery/funnel`
(`?batch_id=`) returns: opted in/out, eligible audience and listings, deliveries
planned/attempted/sent/failed/skipped, delivered, read, interacted, listing
viewed, **asked (distinct delivery+listing, not taps)**, requests created and
confirmed, offers, selections, transactions, STOPs from discovery, unresolved
interactions, with per-arm and per-exclusion breakdowns. See
`docs/analytics/funnel.md`.

## 7. Operator surface
All 12 `/api/discovery/*` routes are operator-token only; there is no consumer
endpoint. The dashboard `/discovery` page: gate/caps/window/arms/ranking
version, staff consent records, dry run (policy result + score reasons per
listing), create batch, batch page with operator-confirmed send, per-delivery
attribution and the funnel.

## 8. Events
`DISCOVERY_OPTED_IN`, `_OPTED_OUT`, `_PREFERENCE_UPDATED`, `_BATCH_CREATED`,
`_BATCH_SENT`, `_DELIVERY_SENT|FAILED|SKIPPED`, `_INTERACTION`,
`_INTERACTION_UNRESOLVED`, `LISTING_ENQUIRY`.

## 9. Configuration [PARAM]
See `.env.example` (`DISCOVERY_*`). Durations are Go durations (`72h`, not
`3d`). Invalid combinations stop startup.

## 10. Known limits (honest list)
* The pilot gazetteer is tiny (Benin City, Port Harcourt, Warri, Lagos); there
  is no LGA adjacency, so "nearby" means same city/LGA only.
* Template sending is built to Meta's payload schema but **untested against
  real Meta**; a template must be approved before any out-of-window send.
* The consent prompt needs a prior inbound within the 24h window.
* No delivery-window scheduler by design: windows gate sends that an operator
  triggers.
* "Watch video" appears only on ARM_B and only when a stored video exists.
* Ranking weights are a hypothesis, not tuned on data.
* Staff E2Es run against a fake Graph API, not live WhatsApp.

## 11. Future / deferred [FUTURE — none of this is current]
Roadmap items, each behind its own capability gate (Lock §L): a scheduler for
delivery windows; `CONTROLLED_PILOT` with real, consented buyers (Phase 4C);
WA Shopa and Spotlight as consumer surfaces; a web marketplace/consumer feed
calling the same `Discovery.Candidates`; personalisation and learned ranking;
vendor-initiated broadcast; richer geography (LGA adjacency, coordinates).
Explicitly not built: scheduler, consumer feed, ML, WA Shopa, Spotlight, web
marketplace, Redis/Kafka/RabbitMQ/NATS/Elasticsearch/vector DB.

## 12. Phase 4C handoff
Prerequisites before any real buyer is messaged: approved Meta template(s) and
verified sending number; written consent wording reviewed (version bump);
decision on cap/window values with data from INTERNAL runs; a gate change from
INTERNAL to a controlled pilot with an explicit buyer allowlist and a kill
switch; reconfirmation SLA with vendors; monitoring of the funnel above.
The code paths 4C needs already exist (consent, caps, atomic claim, send-time
re-check, attribution); 4C should add the gate mode and the pilot audience, not
new ranking or new consent rules.
