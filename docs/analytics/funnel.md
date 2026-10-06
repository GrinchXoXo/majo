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
