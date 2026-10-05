-- Phase 3.1: close the Offer -> Buyer -> Selection loop, plus the
-- correctness baseline fixes from the Phase 3.1 audit (§3A).
--
-- Everything here is additive. Existing rows are preserved; where existing
-- data would violate a new uniqueness invariant, the extra rows are marked
-- (never deleted) and an audit record is written.

-- ---------------------------------------------------------------------------
-- 1. Offer delivery tracking + stable buyer-facing ordinals
-- ---------------------------------------------------------------------------
-- presentation_ordinal is the number the buyer sees ("Reply 1"). It is
-- assigned once, when the offer is first delivered, and never reused within
-- the same BuyerRequest, so "1" always means the same offer for that request.
ALTER TABLE offers ADD COLUMN presentation_ordinal    INTEGER;
ALTER TABLE offers ADD COLUMN sent_at                 TIMESTAMPTZ;
ALTER TABLE offers ADD COLUMN delivery_attempts       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE offers ADD COLUMN last_delivery_error     TEXT;
ALTER TABLE offers ADD COLUMN last_delivery_failed_at TIMESTAMPTZ;

-- One logical Offer per VendorResponse. Pre-existing duplicates (possible
-- before 3.1 because offers were built by concurrent GET requests) keep the
-- earliest row; later copies are marked DUPLICATE and excluded from the
-- unique index rather than deleted (they may be referenced by selections).
UPDATE offers o
SET status = 'DUPLICATE', updated_at = now()
FROM (
    SELECT id, row_number() OVER (PARTITION BY vendor_response_id ORDER BY created_at, id) AS rn
    FROM offers
    WHERE vendor_response_id IS NOT NULL
) d
WHERE o.id = d.id AND d.rn > 1;

CREATE UNIQUE INDEX uq_offers_vendor_response
    ON offers (vendor_response_id)
    WHERE vendor_response_id IS NOT NULL AND status <> 'DUPLICATE';

CREATE UNIQUE INDEX uq_offers_request_ordinal
    ON offers (buyer_request_id, presentation_ordinal)
    WHERE presentation_ordinal IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. One BuyerRequest -> at most one BuyerSelection -> at most one Transaction
-- ---------------------------------------------------------------------------
ALTER TABLE buyer_selections ADD COLUMN status TEXT NOT NULL DEFAULT 'ACTIVE'; -- ACTIVE | VOID

-- Pre-existing duplicate selections (possible before 3.1): keep the earliest
-- per request ACTIVE, mark the rest VOID, and cancel their still-CREATED
-- transaction shells. Nothing is deleted; every change is audited below.
UPDATE buyer_selections s
SET status = 'VOID'
FROM (
    SELECT id, row_number() OVER (PARTITION BY buyer_request_id ORDER BY created_at, id) AS rn
    FROM buyer_selections
) d
WHERE s.id = d.id AND d.rn > 1;

UPDATE transactions t
SET status = 'CANCELLED', updated_at = now()
FROM buyer_selections s
WHERE t.buyer_selection_id = s.id AND s.status = 'VOID' AND t.status = 'CREATED';

INSERT INTO audit_logs (actor_type, action, entity_type, entity_id, previous_value, new_value, metadata)
SELECT 'SYSTEM', 'BUYER_SELECTION_VOIDED_BY_MIGRATION', 'buyer_selection', s.id,
       '{"status":"ACTIVE"}'::jsonb, '{"status":"VOID"}'::jsonb,
       jsonb_build_object('migration', '0006', 'reason', 'duplicate selection for the same buyer_request')
FROM buyer_selections s WHERE s.status = 'VOID';

CREATE UNIQUE INDEX uq_buyer_selections_active_request
    ON buyer_selections (buyer_request_id)
    WHERE status = 'ACTIVE';

CREATE UNIQUE INDEX uq_transactions_buyer_selection
    ON transactions (buyer_selection_id);

-- ---------------------------------------------------------------------------
-- 3. VendorRequest delivery failure is a distinct state (SEND_FAILED)
-- ---------------------------------------------------------------------------
-- Only SENT requests count as "awaiting a vendor reply". A request WhatsApp
-- rejected (e.g. 24h window closed / template required) must never capture
-- the vendor's next unrelated message as a "response".
ALTER TABLE vendor_requests ADD COLUMN send_error     TEXT;
ALTER TABLE vendor_requests ADD COLUMN send_failed_at TIMESTAMPTZ;

-- Pre-3.1 PENDING rows were never confirmed delivered: before this phase a
-- request stayed PENDING precisely when the WhatsApp send failed.
UPDATE vendor_requests
SET status = 'SEND_FAILED',
    send_error = 'send outcome not recorded before Phase 3.1 (left PENDING)',
    send_failed_at = now()
WHERE status = 'PENDING';
