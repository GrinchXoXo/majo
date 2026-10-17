# 01a code audit: 12 blocks

17 October 2026 · branch `pilot-batch-1` · roadmap step 01a.

## Method

- I read each block's code, schema and tests, then checked the riskiest
  claims with small probes. For example, the routing probe below ran
  `matching.Match` and `RoutingPolicy` on real inputs.
- Severity:
  - **High:** can break the 2 November pilot, or harm a user.
  - **Medium:** wrong or missing, with a workaround or no impact yet.
  - **Low:** cleanliness or clarity.
- **P** marks findings that affect the pilot.
- **Fixed** marks findings fixed in this session, in commits `0dc7084` and
  after, each with a test.
- Everything else is a recommendation, tagged with the roadmap phase it
  belongs to.

## Summary

| # | Block | State today | Pilot risk after fixes | Fixed now | Next |
|---|---|---|---|---|---|
| 1 | Finance | Scaffold only: transactions record amounts, nothing moves money; payments boundary, flags and ledger built, all off | None (Stage A takes no money) | Fee shown as illustrative | 06–08: money type (naira vs kobo), order state machine, provider |
| 2 | Vendors | Operator-onboarded; status, categories, areas, activity counters | Low | Number normalisation, category vocabulary, STOP/START, edit form, status validation | 03: verification record, rating and response-rate profile |
| 3 | Ranking | Deterministic matching, routing policy, offer ranking | Low | New vendors routed by area; fair tie-break | 05: price-relative ranking, reliability signals |
| 4 | Orders | Request → offer → one active selection → transaction → deal | Low | — | 06: one order state machine fed by deal outcomes |
| 5 | Fulfilment | Status fields only; buyer and seller arrange it themselves | Accepted for the pilot | — | 09 |
| 6 | Catalogue | Signals, grouping, listings, curation, lifecycle, media | Low | Restricted items never auto-published | 04: canonical products once there is volume |
| 7 | Discovery | Built, OFF; INTERNAL staff mode only | None (off) | — | after the pilot |
| 8 | Trust & safety | Signatures, roles, restricted terms, safety exceptions, consent | **Medium** (buyer abuse) | Vendor STOP, restricted auto-publish | 10: buyer block and rate cap, report-scam, retention |
| 9 | Economics | Free-deal counter, deal value, revenue-line scaffold | Low | — | 13: cost per request and per deal |
| 10 | Geography / campus | Free-text places, small resolver, pilot default location | Low (after setup) | Place-name matching | 14: campus alias table |
| 11 | Control plane | 10 pages, named tokens and roles, pauses, exceptions, trails | Low | Vendor editing | 12: buyer view, tone-review view |
| 12 | Analytics | Funnel, supply, autonomy, discovery and deal metrics | Low | `deal_confirmed` funnel stage | 13: time series, export, per-vendor view |

**Pilot verdict:** no known code blocker is left for 2 November once the
fixes below are in. The remaining pilot risks are setup:

- the WhatsApp number;
- the vendor template approval;
- the campus name and vendor areas;
- categories that match the vocabulary;
- the production database.

The one open safety gap is buyer abuse (§8). It is acceptable for an
invited pilot and must close before public launch.

---

## 1. Finance (roadmap 06–08)

**What exists:**

- `transactions` (migration 0005): gross, platform fee, payment fee,
  vendor amount, refund amount, currency, and provider/reference columns;
  statuses for the transaction, payment and fulfilment.
- Pilot batch 8 adds:
  - `internal/payments` (provider boundary; only `none` exists);
  - `internal/monetization` (flags per revenue line, gated by stage);
  - the ledger (migration 0019, double-entry, enforced in the database).

**Findings:**

1. **Medium:** amounts are whole naira everywhere except the ledger and
   payments, which use kobo. That is fine today, because nothing converts
   between them. *06:* introduce a money type, or explicit `…Naira` and
   `…Kobo` names, and convert ×100 at one place only: where a payment or
   ledger entry is made.
2. **Low · P · Fixed (label):** every transaction computes an
   "illustrative" 4% `platform_fee` (`DefaultTakeRateBps`), and the request
   page showed it as if charged. It is now labelled "illustrative, not
   charged". *06:* set it from the approved price list, or drop it.
3. **Medium:** the transaction status columns have no CHECK constraints, and
   no code ever changes them (a transaction stays `CREATED`/`UNPAID`
   forever). The real outcome lives on the deal (pilot batch 5). *06:* one
   order state machine. A deal `CONFIRMED` completes the order, and
   `DECLINED`/`NO_ANSWER` cancels it.
4. No refunds, reconciliation or settlement flows yet. That is expected:
   they come with a real provider (rollout Stage B/C). The ledger journal
   already defines the entries.

## 2. Vendors (roadmap 03)

**What exists:**

- `vendors` with status (PENDING/ACTIVE/INACTIVE/BLOCKED), categories,
  service areas, primary location and activity counters.
- Operator onboarding on the dashboard.
- A per-vendor open-request limit (`AUTO_ROUTE_VENDOR_MAX_OPEN`).
- A trust window for auto-publishing listings.
- Deal ratings.

**Findings:**

1. **High · P · Fixed:** the WhatsApp number was stored exactly as typed.
   A vendor saved as `0803 123 4567` or `+234 803…` never matched the
   incoming `wa_id` (`234803…`). Their replies were then treated as a
   *buyer's* messages, which created buyer requests.
   - Now normalised (`vendor.NormalizeWhatsAppID`).
   - Unreadable numbers are refused (400), duplicates are refused (409),
     and the dashboard shows the server's reason.
2. **Medium · P · Fixed:** categories were free text. A vendor tagged
   "Small chops" or "Food " never matched a request categorised `food`.
   - Now lower-cased and de-duplicated.
   - Words outside the request vocabulary come back as a warning on the
     page.
   - The vocabulary is written down in `docs/pilot/vendor-onboarding.md`.
3. **Medium · P · Fixed:** the vendor invite promises "reply STOP to
   leave", but nothing handled it. A STOP was read as a reply or as supply.
   - STOP now makes the vendor INACTIVE and confirms.
   - START records `VENDOR_OPT_IN_REQUESTED` for the owner to re-activate.
   - BLOCKED is never changed by texting.
   - Failed requests are not re-sent to an inactive vendor.
4. **Medium · P · Fixed:** categories, location and service areas, which
   decide who gets requests, could only be changed through the API. The
   Vendors page now has an Edit form (and shows service areas).
5. **Low · Fixed:** PATCH accepted any status string (for example
   `ACTIV`), silently taking a vendor out of matching. It is now
   validated.
6. **Open (03):**
   - the "verified vendor" of rollout Stage A has no record: who checked,
     when, how;
   - deal ratings are not aggregated per vendor;
   - `requests_received` / `responses_received` are not shown as a
     response rate.

## 3. Ranking (roadmap 05)

**What exists:**

- `matching.Match` (category 30, product 30, location 20, condition 10,
  availability 10; floor 40, top 5).
- `workflow.RoutingPolicy` (route floor, open-request limit, fan-out 3,
  local first, over-budget last).
- `offer.rankOffer` for the buyer's list.
- Discovery's own weights.

**Findings:**

1. **High · P · Fixed:** a new pilot vendor (categories and area set, no
   stock posted yet) was contacted **only** when the buyer's location
   string was identical to the vendor's primary location. Probe, before the
   fix, for a vendor "Unilag" with service areas "Akoka" and "Yaba":

   | Buyer location | Score | Routed? |
   |---|---|---|
   | Unilag | 50 | yes |
   | Akoka (a service area) | 47 | **no**: below the 50 route floor |
   | Moremi hall | — | **no**: not even a candidate |
   | Unilag main gate | — | **no**: not even a candidate |

   Two fixes:
   - **place names match when one names the other more precisely**: every
     word of the shorter is in the longer (`matching.LocationMatches`);
   - **`AUTO_ROUTE_MIN_SCORE` default 45**: category plus a service-area
     match (47) is enough to contact a vendor; category alone (30) is not.

   The regression test is `TestRoutingNewVendorByArea`.
2. **Medium · P · Fixed:** ties were broken by vendor UUID, so equally good
   vendors always lost to the same ones and could go the whole pilot
   without a lead. The vendor sent fewer requests so far now goes first
   (`TestEqualVendorsRotateByRequestsReceived`).
3. **Medium (05):** offer ranking.
   - Without a buyer budget, every price counts as "within budget".
   - Among offers within budget, a cheaper price earns nothing.
   - There is no reliability signal (response rate, confirmed deals,
     ratings).
   - Ties go to the first responder.
4. **Low · P:** a request with no category, or the AI's `other`, can't
   reach any vendor without product evidence (category is the eligibility
   gate). The 3–4 pilot launch categories must come from the vocabulary.
   That is a 01b decision.

## 4. Orders (roadmap 06)

**What exists:**

- Request lifecycle.
- Offers that expire once sent (the Phase 3.1 issue is fixed).
- One ACTIVE selection per request (database constraint, race handled).
- A transaction per selection.
- The deal follow-up (pilot batch 5).

**Findings:**

1. **Medium (06):** see Finance 3: the order outcome lives on the deal, and
   the transaction never moves.
2. **Low (06):** the agreed price and quantity are the offer's. There is no
   record of a renegotiated price, though the deal amount could carry one.

## 5. Fulfilment (roadmap 09)

There is no fulfilment logic, only status fields. In the pilot, buyer and
seller arrange delivery or pickup themselves. The introduction message says
so and carries the safety tip, and the deal follow-up records the outcome.
Nothing to fix before 09.

## 6. Catalogue (roadmap 04)

**What exists:**

- supply signals with grouping;
- listings with draft, curation and publish steps, and an audit trail;
- lifecycle, freshness and geographic expansion;
- media ingestion.

The Phase 3.1 search bugs are fixed in code: space-insensitive product
search, location normalised, and the vendor's primary location used when a
listing has none.

**Findings:**

1. **Medium · Fixed:** trusted-vendor auto-publishing (off by default) did
   not check the restricted-item list that buyer requests are held for. A
   trusted vendor's "codeine" listing would have gone live with no operator.
   It now stays a draft, with a `restricted_item` decision recorded
   (`TestIntegration_AutoPublishRefusesRestrictedItems`).
2. **Low (04):** there is no canonical product entity. That is correct
   until there is real listing volume, as the 3.1 audit concluded. The
   addition path is unchanged.

## 7. Discovery (Phase 4B)

**What exists:**

- `DISCOVERY_MODE` OFF by default, with INTERNAL for staff numbers only.
  `CONTROLLED_PILOT` and `ON` are refused at startup.
- Opt-in consent, frequency caps, the delivery window, a STOP keyword and
  button, and a funnel.

**Findings:** none for the pilot; it stays OFF. Turning it on for buyers is
an owner decision after the pilot.

## 8. Trust & safety (roadmap 10)

**What exists:**

- signed webhooks;
- named operator tokens with roles;
- restricted terms on buyer requests;
- SAFETY exceptions for disputed deals and 1–2-star ratings;
- the safety tip in every introduction;
- the buyer's number not shared by default;
- discovery consent;
- scrubbed error reports;
- the global pause.

**Findings:**

1. **Fixed:** vendor STOP (Vendors 3) and restricted auto-publish
   (Catalogue 1).
2. **Medium · P (High before public launch):** a buyer cannot be blocked
   (`buyers` has no status), and there is no per-buyer request cap.
   - One abusive number can keep sending requests.
   - Each confirmed request reaches up to 3 vendors.
   - Vendors stop receiving requests at 5 open ones, so a spammer can
     crowd out real buyers.
   - Acceptable for an invited single-campus pilot, given the global and
     per-request pauses. *10:* add `buyers.status` (BLOCKED), a daily
     request cap per buyer, and a "Block buyer" action.
3. **Medium (10):** no "report scam" path for buyers beyond answering NO
   to the follow-up, or a low rating. Rollout Stage A lists "report scam
   and a ban list".
4. **Medium (10, legal):** no retention policy. Messages and phone numbers
   are kept indefinitely, and the privacy notice is still a draft. NDPA
   2023 needs a stated purpose and retention period. *Owner + legal.*
5. **Low:** vendor verification is not modelled (Vendors 6).

## 9. Economics (roadmap 13)

**What exists:**

- the free-deal counter and confirmed sales value (Deals page);
- the funnel;
- the revenue-line scaffold;
- revenue projections (analytics doc).

**Findings:**

1. **Medium (13):** costs are not counted. You can't yet say what a
   request or a deal costs. Missing:
   - LLM calls: extraction (`extraction_method = anthropic`) and the tone
     layer;
   - paid WhatsApp template sends: vendor requests outside the 24-hour
     window, recorded as `sent_as = template` on events.

   *13:* add these two counts per period next to the funnel, and divide
   by confirmed deals.
2. **Low:** the transaction "platform fee" is not revenue (Finance 2).
   Revenue starts with the ledger.

## 10. Geography / campus (roadmap 14)

**What exists:**

- free-text locations;
- `internal/geo` (levels, a small in-memory Nigerian resolver used for
  listing expansion);
- `PILOT_DEFAULT_LOCATION` for buyers who don't say where they are;
- place-name matching (Ranking 1).

**Findings:**

1. **Low · P:** the pilot works if vendors are entered as the onboarding
   doc now says:
   - primary location equal to `PILOT_DEFAULT_LOCATION`;
   - halls, gates and nearby areas as service areas.
2. **Medium (14):** there is no campus model. Areas live per vendor, and
   the resolver knows cities, not campuses. *14:* once the campus is named,
   add a configurable alias table (for example "Moremi", "Akoka" → the
   campus) used by both matching and the resolver.

## 11. Admin control plane (roadmap 12)

**What exists:**

- pages: requests (with decision log, questions and message trail),
  offers, vendors, autonomy (exceptions, global pause), deals (with
  revenue lines), discovery, events timeline, jobs, unmatched responses;
- named tokens with VIEWER/OPERATOR roles;
- an operator name on actions;
- per-request and global pause.

**Findings:**

1. **Medium · P · Fixed:** vendor editing (Vendors 4).
2. **Medium (12):** there is no buyer view, so a buyer's history (all
   requests, deals and messages) can't be looked up in one place. It is
   needed for support and for blocking (Trust 2).
3. **Low (12):** tone-layer shadow review is a SQL query for now
   (`docs/architecture/tone-layer.md`); a small page would help before
   switching `TONE_LAYER=on`.
4. **Low:** tokens live in `sessionStorage`, and there is no 2FA. This is
   the known, accepted trade-off for an internal tool (reuse audit).

## 12. Analytics (roadmap 13)

**What exists:**

- the funnel, now 16 stages;
- 15 supply metrics;
- autonomy metrics with denominators;
- the discovery funnel;
- the deal summary;
- the event timeline.

**Findings:**

1. **Low · P · Fixed:** the funnel ended at "transaction completed",
   which is always 0. It now includes **deal confirmed**, the pilot's
   measure of a sale.
2. **Medium (13):**
   - no time series or cohort view (the funnel counts activity per window);
   - no CSV export;
   - no per-vendor performance view (response rate, confirmed deals,
     rating).

   Metabase on PII-free views stays the plan for exploratory work (reuse
   audit).

---

## Fixed in this session (with tests)

| Fix | Files | Test |
|---|---|---|
| Vendor number normalised; duplicates and unreadable numbers refused | `internal/vendor/normalize.go`, `httpserver/vendor_handlers.go` | `TestNormalizeWhatsAppID`, Playwright vendor test |
| Category vocabulary and warnings; status validation | same | `TestNormalizeCategories` |
| Vendor STOP/START; no re-send to inactive vendors | `internal/routing/optout.go`, `routing/autonomy.go` | `TestVendorStopAndStart`, `TestVendorKeyword` |
| Place-name matching; route floor 45 | `internal/matching`, `internal/autonomy` | `TestLocationMatches`, `TestRoutingNewVendorByArea` |
| Fair tie-break | `internal/matching` | `TestEqualVendorsRotateByRequestsReceived` |
| Restricted items never auto-published | `internal/workflow/recovery.go` | `TestIntegration_AutoPublishRefusesRestrictedItems` |
| `deal_confirmed` funnel stage | `postgres/funnel_repository.go` | funnel integration tests |
| Vendor edit form; error text; fee label | `dashboard/app/vendors`, `requests/[id]` | Playwright vendor test, `tsc` |

## Open items by roadmap phase

- **01b (owner):**
  - launch categories from the vocabulary;
  - the campus name and its area names;
  - how vendors are verified;
  - the retention period.
- **03:** a vendor verification record; a rating, response-rate and
  confirmed-deal profile.
- **05:** price-relative and reliability signals in offer ranking.
- **06:** money type; one order state machine fed by deals.
- **10:** buyer block and daily cap; a report-scam path; retention job.
- **12:** a buyer view; a tone-review view.
- **13:** cost counts (LLM, templates) per period and per deal; time
  series; export; per-vendor view.
- **14:** a campus alias table.
