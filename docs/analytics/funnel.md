# Majo core funnel — what is recorded and how to measure it

Phase 3.1 §23A. **Purpose:** make sure Majo collects the behavioural data needed to decide
*later* — on evidence — when to build discovery, personalization, recommendations, proactive
messaging, Spotlight or WA Shopa. This is **not** an analytics platform: no third-party
provider, no warehouse, no event bus. Everything lives in the existing PostgreSQL tables.

## Two sources, one rule

| Source | Role | Why |
|---|---|---|
| **Entity tables** (`buyer_requests`, `vendor_requests`, `vendor_responses`, `offers`, `buyer_selections`, `transactions`, `messages`) | **Authoritative counts and timestamps** | Written in the same statement as the business state change — cannot drift from reality. |
| **`events` table** | Rich context per step (who/what/where/how) for slicing | Append-only, but written *after* the state change and best-effort (a failed write is logged, not retried). Use for dimensions, not for "did it happen". |

`GET /api/metrics/funnel?days=N` (and the table on `/dashboard`) counts from the entity tables.

## Stages

| # | Stage | Authoritative source | Event (context) |
|---|---|---|---|
| 1 | Buyer request created | `buyer_requests.created_at` | `REQUEST_CREATED` |
| 2 | Buyer request confirmed | `buyer_requests.confirmed_at` | `REQUEST_CONFIRMED` (once; idempotent re-confirms write nothing) |
| 3 | Vendor request created | `vendor_requests.created_at` | `VENDORS_ROUTED` (one per vendor request; `outcome` = `sent` \| `send_failed`) |
| 4 | Vendor request accepted by WhatsApp | `vendor_requests.sent_at` | `VENDORS_ROUTED` outcome=`sent` |
| 4b | Vendor request rejected | `vendor_requests.send_failed_at`, `send_error` | `VENDOR_REQUEST_SEND_FAILED` |
| 5 | Vendor request **delivered** | `messages.delivered_at` where `metadata.kind='vendor_request'` | `MESSAGE_DELIVERED` on entity `vendor_request` |
| 6 | Vendor response received | `vendor_responses.created_at` (with `vendor_request_id`) | `VENDOR_RESPONSE_RECEIVED` (+ `seconds_since_request_sent`) |
| 7 | Offer created | `offers.created_at` (status ≠ `DUPLICATE`) | `OFFER_CREATED` |
| 8 | Offer sent to buyer | `offers.sent_at`, `presentation_ordinal` | `OFFER_SENT` (`OFFER_SEND_FAILED` on rejection) |
| 9 | Offer message **delivered** | `messages.delivered_at` where `kind='offer_delivery'` | `MESSAGE_DELIVERED` on entity `buyer_request` |
| 10 | Offer message **read** *(where observable)* | `messages.read_at` where `kind='offer_delivery'` | `MESSAGE_READ` |
| 11 | Offer selected | `buyer_selections` (ACTIVE), `source` | `BUYER_SELECTION_CREATED`, `OFFER_ACCEPTED` (+ `seconds_since_offer_sent`) |
| 12 | Transaction created | `transactions.created_at` | `TRANSACTION_CREATED` |
| 13 | Transaction completed | `transactions.status = 'COMPLETED'` | `TRANSACTION_COMPLETED` — **reserved**; nothing completes a transaction until fulfilment exists |

Also recorded: `OFFER_WITHDRAWN`, `BUYER_REPLY_UNRESOLVED` (ambiguous / out-of-range replies —
a direct measure of conversational friction), `MESSAGE_FAILED` (asynchronous delivery failure),
`SUPPLY_SIGNAL_CREATED` (supply side: vendor, category, product, price, location).

### Delivery and read receipts — what is and isn't observable
- Every outbound message is stored in `messages` (direction `OUTBOUND`) with Meta's message ID
  and its purpose in `metadata` (`kind`, `entity_type`, `entity_id`, plus e.g. `offer_ids`,
  `reference`). Meta's status callbacks then stamp `delivered_at` / `read_at` / `failed_at`
  (first arrival only; repeats are ignored).
- **`read_at` exists only if the recipient has WhatsApp read receipts on** (platform-dependent).
  Read counts are a *lower bound*. Never treat "not read" as "not seen".
- Messages sent before migration 0007 have no stored ID and will never get receipts.

## Dimension vocabulary (event payload keys)

`buyer_id`, `buyer_request_id`, `conversation_id`, `vendor_id`, `vendor_request_id`,
`vendor_response_id`, `offer_id`, `supply_listing_id`, `selection_id`, `transaction_id`,
`category`, `product`, `location`, `budget_max`, `price`, `offer_type`, `channel`, `source`,
`outcome`, `request_status`.

- Only **known** values are written; unknown ones are omitted (never `null`, never guessed).
  `category`/`product`/`location` appear once the buyer stated them (extraction or operator
  correction) — `location` is only what the buyer/vendor said, never inferred.
- `source` on selections: `whatsapp_reply` (organic) · `dashboard_operator` (override) · `api`.
- Timestamps: `events.created_at` and the entity columns above (UTC).

## Example queries

```sql
-- Request-level conversion (cohort: requests created in the last 30 days)
SELECT
  count(*)                                                   AS requests,
  count(*) FILTER (WHERE br.confirmed_at IS NOT NULL)        AS confirmed,
  count(*) FILTER (WHERE EXISTS (SELECT 1 FROM vendor_requests v WHERE v.buyer_request_id = br.id AND v.sent_at IS NOT NULL)) AS routed,
  count(*) FILTER (WHERE EXISTS (SELECT 1 FROM offers o WHERE o.buyer_request_id = br.id AND o.sent_at IS NOT NULL))         AS got_offer,
  count(*) FILTER (WHERE EXISTS (SELECT 1 FROM buyer_selections s WHERE s.buyer_request_id = br.id AND s.status = 'ACTIVE')) AS selected
FROM buyer_requests br
WHERE br.created_at >= now() - interval '30 days';

-- Time to first offer (median minutes) by category
SELECT br.category,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (o.first_sent - br.created_at)) / 60) AS median_minutes
FROM buyer_requests br
JOIN (SELECT buyer_request_id, min(sent_at) AS first_sent FROM offers WHERE sent_at IS NOT NULL GROUP BY 1) o
  ON o.buyer_request_id = br.id
GROUP BY br.category;

-- Vendor responsiveness: reply rate and median reply time per vendor
SELECT v.business_name,
       count(*) AS sent,
       count(r.id) AS replied,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (r.created_at - vr.sent_at)) / 60) AS median_reply_minutes
FROM vendor_requests vr
JOIN vendors v ON v.id = vr.vendor_id
LEFT JOIN vendor_responses r ON r.vendor_request_id = vr.id
WHERE vr.sent_at IS NOT NULL
GROUP BY v.business_name;

-- Offer message delivery/read rate
SELECT count(*) AS offer_messages,
       count(delivered_at) AS delivered,
       count(read_at) AS read_lower_bound
FROM messages
WHERE direction = 'OUTBOUND' AND metadata->>'kind' = 'offer_delivery';

-- Demand by location / category from funnel events (context, not counts)
SELECT payload->>'category' AS category, payload->>'location' AS location, count(*)
FROM events WHERE event_type = 'REQUEST_CONFIRMED'
GROUP BY 1, 2 ORDER BY 3 DESC;

-- Conversational friction: unresolved replies per selection
SELECT (SELECT count(*) FROM events WHERE event_type = 'BUYER_REPLY_UNRESOLVED') AS unresolved,
       (SELECT count(*) FROM buyer_selections WHERE source = 'whatsapp_reply')   AS selections;
```

## Which signals inform which future capability

Thresholds are **product decisions, not set here**. This maps each future capability to the
recorded signals that would justify it.

| Future capability | Signals already recorded |
|---|---|
| Marketplace discovery / search | Repeat requests for the same `product`/`category`; share of requests answered by existing supply (`SUPPLY_SIGNAL_CREATED` vs requests); time to first offer |
| Personalization | Repeat buyers (`buyer_id` across requests), per-buyer `category`/`location`/`budget_max` stability |
| Recommendations | Selection rate by `offer_type` / price vs budget; alternatives chosen (`offer_type=ALTERNATIVE`) |
| Proactive messaging / WA Shopa | Requests with no offer (stage 7 = 0 per request); offers rejected/ignored; delivery & read rates; 24h-window failures (`send_error` 131047) |
| Spotlight (vendor promotion) | Vendor reply rate & latency; per-vendor selection share; supply volume by vendor |

## Known limits
- Receipts start at migration 0007; earlier sends have no message ID.
- Events are best-effort; use entity tables for counts.
- "Transaction completed" is always 0 until fulfilment is built.
- `raw_message` (buyer/vendor free text) is stored in events and entities — treat event
  exports as personal data.


## Supply side (Phase 4A, Architecture Lock V2 §K.2)

The same endpoint (`GET /api/metrics/funnel?days=N`) also returns `supply`: one row per metric, counted from the
`supply_signals`, `supply_listings`, `vendors`, `vendor_requests` and `vendor_responses` tables. The dashboard shows them under
"Supply". Each row has a `kind`:
- `count`: a number;
- `rate`: `value / of` as a share;
- `per`: `value / of` as an average.

`scope` is `window` (activity inside the period) or `now` (a snapshot of the current state).

If the supply rows cannot be computed, the report still returns the stages, with `supply_error` set.

| Key | Meaning | Source |
|---|---|---|
| `vendor_submissions` | supply signals created | `supply_signals.created_at` |
| `submissions_per_submitting_vendor` | signals / vendors with ≥ 1 signal | `supply_signals` |
| `valid_supply_rate` | signals with product **and** price / all signals | `supply_signals` |
| `vendor_supply_repeat_rate` | vendors submitting on ≥ 2 distinct UTC days / submitting vendors | `supply_signals` |
| `listings_drafted` | listings created | `supply_listings.created_at` |
| `media_complete_rate` | drafted listings with ≥ 1 image reference / drafted listings | `supply_listings.media_references` |
| `listings_published` | operator publications | `supply_listings.published_at` |
| `listings_rejected` | drafts rejected (never published) | `supply_listings.rejected_at`, `published_at IS NULL` |
| `listing_approval_rate` / `listing_rejection_rate` | published (or rejected) / (published + drafts rejected), decisions made in the period | `supply_listings` |
| `listings_withdrawn` | published listings later rejected | `rejected_at` with `published_at` set |
| `stale_listing_rate` (now) | ACTIVE listings inside their lifecycle whose last verification is older than `SUPPLY_FRESHNESS_WINDOW` [PARAM, default 72h] / ACTIVE inside their lifecycle. Stale is not expired. | `last_verified_at`, `expires_at` |
| `lifecycle_expired_listings` (now) | ACTIVE listings past their lifecycle ceiling: logically expired, not eligible, kept as history | `expires_at` |
| `usable_listings_per_active_vendor` (now) | ACTIVE unexpired listings of ACTIVE vendors / ACTIVE vendors | `supply_listings`, `vendors` |
| `vendor_response_rate` | vendor requests sent in the period that got ≥ 1 matched response / vendor requests sent in the period | `vendor_requests`, `vendor_responses` |

Rejection reasons are in `supply_listings.rejection_reason`. Two §K.2 rows wait for Phase 4B: vendor re-confirmation rate,
and discovery-eligible supply volume.

## Discovery side (Phase 4B, Architecture Lock V2 §K.1)

Source: `GET /api/discovery/funnel[?batch_id=]` (operator-only) and the
`/discovery` dashboard page. Counts are computed from the delivery chain, not
from raw taps. Staff/INTERNAL traffic only in Phase 4B.

| Stage | Meaning |
|---|---|
| opted_in / opted_out | current consent records |
| eligible_audience / eligible_listings | what a plan found (permitted + consented + under cap; policy-eligible and reachable) |
| deliveries planned → attempted → sent / failed / skipped | `skipped` carries a reason (`opted_out`, `outside_session_no_template`, `frequency_cap`, …) |
| delivered / read | provider receipts for the delivery's messages |
| interacted / listing_viewed | any tap / a card or video opened |
| **asked** | distinct (delivery, listing) "Ask about it" — a repeated tap is not a second ask |
| requests_created / requests_confirmed | discovery-origin BuyerRequests, and those an operator confirmed |
| offers / selections / transactions | attributed through `supply_listing_id` and the delivery id |
| opt_outs_from_discovery | STOPs from a discovery delivery |
| unresolved_interactions | malformed/unknown/foreign/expired/gone taps (persisted, mutated nothing) |

Breakdowns: by presentation arm (ARM_A/ARM_B), by exclusion reason. Attribution
chain: delivery → message(s) → interaction → BuyerRequest(`origin=discovery`,
`listing_id`, `discovery_delivery_id`) → VendorRequest/Offer(`supply_listing_id`)
→ BuyerSelection → Transaction.

Known limits: delivered/read depend on Meta status callbacks; with the fake
Graph API they are not produced. Arm comparisons need far more volume than a
staff proof can give; Phase 4B proves the measurement exists, not that an arm wins.

## Autonomy (Phase 4C correction)

`GET /api/autonomy/metrics[?since=RFC3339]` (operator-only), counted from
system-of-record tables: events **with their actor**, clarifications, the
decision log, exceptions, vendor requests and selections. Every rate is
reported as `{n, of, pct}` — raw counts always travel with the percentage.

| Metric | Definition |
|---|---|
| Total workflows | BuyerRequests created in the period (one workflow per request) |
| Workflow class (exactly one each) | `system_failure` (a SYSTEM_EXCEPTION was raised) > `operator_intervention` (an operator confirmed, routed, asked a question or associated a reply) > `operator_override` (an operator corrected the request or a vendor reply, or selected for the buyer) > `automatic_retry` (a failed delivery recovered automatically) > `clarification_assisted` (a buyer or vendor question was answered) > `autonomous` |
| Autonomous request handling | requests in an autonomous class (`autonomous`, `clarification_assisted`, `automatic_retry`) that progressed (routed, told "no match", or asked to broaden) / all requests |
| Autonomous completion | transactions whose workflow is in an autonomous class / all transactions |
| Clarification resolution | clarifications ANSWERED / clarifications issued (not CANCELLED), also per party |
| Operator intervention | workflows classed `operator_intervention` / all workflows; by reason: manual_confirmation, manual_routing, ambiguous_intent, vendor_association, operator_correction, operator_selection, system_failure |
| Manual routing | `VENDORS_ROUTED` events with actor OPERATOR / all `VENDORS_ROUTED` (never hidden inside the generic intervention rate; target 0% in normal operation) |
| Manual confirmation | `REQUEST_CONFIRMED` with actor OPERATOR / all `REQUEST_CONFIRMED` |
| Automatic recovery | delivery retries that recovered / retries scheduled |
| Failures by category | decision-log fallbacks other than clarification/retry (POLICY_REJECTION, TEMPORARY_PROVIDER_FAILURE, GENUINE_AMBIGUITY, SAFETY_EXCEPTION, SYSTEM_EXCEPTION) |

A transaction where an operator routed the vendor is **never** counted as
autonomous, whatever happened afterwards.

> **Correction (Phase 4C hardening).** "Clarification resolution" above counts
> clarification *rows* ANSWERED / rows not CANCELLED (OPEN, SUPERSEDED, EXPIRED
> and EXHAUSTED are in the denominator): it measures **questions answered**, not
> ambiguity resolved, and re-asks are folded into one row. "Automatic
> recovery" counts retries scheduled — only transient failures; a policy
> refusal is never retried and never in that denominator. (The correction
> report's sentence "the other one was a policy rejection" was wrong: it was
> the transient failure whose retries ran out.) Both are kept for
> compatibility; use the layered metrics below.

### Layered autonomy metrics (Phase 4C hardening)

Same endpoint, fields `workflow`, `questions`, `recovery`, `delivery`,
`definitions` (the definitions are served with the numbers). One level per
group; levels are never mixed. Everything is counted from persisted rows
(`buyer_requests`, `transactions`, `clarifications`, `workflow_decisions`,
`workflow_exceptions`, `workflow_holds`, `pending_notices`, `events`).

| Metric | Numerator | Denominator | Level |
|---|---|---|---|
| workflow.started | — | buyer requests created since `since` (small talk creates none) | request |
| workflow.completed | requests with a transaction (UNPAID) | started | request |
| workflow.completed_autonomously | completed, no operator step, no system exception, no operator selection | started | request |
| workflow.requiring_clarification | requests with ≥ 1 delivered question (buyer or vendor) | started | request |
| workflow.escalated_to_operator | requests with ≥ 1 operator exception | started | request |
| workflow.operator_performed_step | an operator confirmed / routed / asked / re-associated | started | request |
| workflow.held | ever paused by an operator | started | request |
| workflow.ended_without_offer | told "no match" / "no reply yet" (no transaction) | started | request |
| workflow.abandoned | waiting on the buyer, last question EXPIRED / EXHAUSTED | started | request |
| workflow.pending | started − completed − ended − abandoned | started | request |
| questions.delivered | — | clarification rows except CANCELLED and undelivered | question |
| questions.messages_sent | question messages incl. re-asks (sum of attempts) | — | message |
| questions.answered | ANSWERED | delivered | question |
| questions.resolved | ANSWERED and the same request+field (or vendor request+kind) not asked again | delivered | question |
| questions.re_asked | attempts > 1, or asked again later | delivered | question |
| questions.expired_or_gave_up | EXPIRED + EXHAUSTED | delivered | question |
| recovery.delivery_retries | recovered | retries scheduled (transient only) | delivery attempt |
| recovery.delivery_retries_failed | retries exhausted | — | delivery attempt |
| recovery.vendor_requests_expired | deadline passed / never sent | — | vendor request |
| recovery.recovery_decisions | `RECOVER_REQUEST` outcome:reason counts | — | request decision |
| recovery.reroute_led_to_offer | re-routed requests that later got an offer | re-routed requests | request |
| recovery.duplicate_actions_prevented | duplicate recovery / duplicate notice decisions | — | decision |
| delivery.policy_blocked | sends refused by a messaging policy | — | message |
| delivery.notices_* | queued / delivered late / pending / cancelled | — | message |
| delivery.provider_failures_reported_async | classified webhook failures | — | message |
| delivery.system_exceptions / safety_exceptions | exceptions raised | — | exception |

Test runs against the fake Graph API produce **synthetic** numbers; label them
as such wherever they are quoted.

## Controlled discovery pilot — locked metric definitions (Phase 4C)

Two funnels, never one denominator. Someone can opt in and receive zero
discovery (no eligible supply): they count toward opt-in, not toward tap rate.

**Consent funnel:** eligible buyer → opt-in invitation delivered → explicit opt-in → opted-in audience.
**Discovery performance funnel:** opted-in buyer → discovery delivered → interaction → listing enquiry → offer → selection → transaction.

| Metric | Definition |
|---|---|
| Eligible buyers | buyers who meet the pilot's eligibility criteria and are permitted to receive an opt-in invitation |
| Opt-in invitation delivered | unique eligible buyers who successfully received the opt-in invitation |
| Opted-in buyers | unique buyers who explicitly accepted proactive discovery |
| **Opt-in rate** | opted_in_buyers / opt_in_invitation_delivered — not all users, not all contacts, not deliveries, not buyers who merely received an invitation in the numerator, not taps |
| Opt-out rate | buyers who opted out / buyers who received proactive discovery |
| Tap rate | unique buyers with ≥ 1 discovery interaction / unique delivered discovery recipients |
| Enquiry rate | unique discovery-origin BuyerRequests / unique delivered discovery recipients |
| Transaction rate | unique discovery-attributed Transactions / unique delivered discovery recipients |

Gate wording: **Opt-in gate: ≥ 25% of unique eligible buyers who receive an
opt-in invitation explicitly opt in, measured per pilot round and
cumulatively.** "Tap ≥ 20% delivered" means ≥ 20% of unique opted-in buyers who
received a discovery delivery interact at least once. Every report shows the
numerator and denominator (e.g. `opt_in_invited=40, opted_in=12, rate=30%`)
with sample-size context; a gate is never declared passed on a percentage
alone when the sample is too small to mean anything. The thresholds are
targets/hypotheses, not automatic expansion triggers: expansion weighs opt-in,
tap, opt-out, enquiry, transaction and operational burden together.
