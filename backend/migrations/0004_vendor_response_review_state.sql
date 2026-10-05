-- Phase 2.1: separates business status from review state on vendor
-- responses, and allows a response to be persisted even when it cannot
-- be safely associated with a specific VendorRequest (PRD 2.1 §4, §13).

-- vendor_request_id becomes nullable: an "unmatched" response (ambiguous
-- association, or a known vendor with no active request) must still be
-- persisted, never discarded.
ALTER TABLE vendor_responses ALTER COLUMN vendor_request_id DROP NOT NULL;

-- vendor_id is needed directly on the response so an unmatched response
-- (vendor_request_id IS NULL) can still be attributed to a vendor and
-- surfaced to the operator for manual association.
ALTER TABLE vendor_responses ADD COLUMN vendor_id UUID REFERENCES vendors(id);

-- Backfill vendor_id for existing rows from their (currently mandatory)
-- vendor_request_id, before any future row is allowed to have a null one.
UPDATE vendor_responses vr
SET vendor_id = vreq.vendor_id
FROM vendor_requests vreq
WHERE vr.vendor_request_id = vreq.id
  AND vr.vendor_id IS NULL;

CREATE INDEX idx_vendor_responses_vendor_id ON vendor_responses(vendor_id);

-- ResponseStatus gains UNCLASSIFIED (PRD 2.1 §1-2): ambiguous vendor
-- text must never be forced into AVAILABLE/UNAVAILABLE.
-- ReviewState is new and orthogonal to status (PRD 2.1 §1):
--   NEEDS_REVIEW | REVIEWED | CORRECTED
ALTER TABLE vendor_responses ADD COLUMN review_state TEXT NOT NULL DEFAULT 'NEEDS_REVIEW';

-- Tracks how the response was produced, so the dashboard can be honest
-- about provenance rather than fabricating a value (PRD 2.1 §8).
--   heuristic | ai | hybrid | manual
ALTER TABLE vendor_responses ADD COLUMN extraction_method TEXT NOT NULL DEFAULT 'heuristic';

CREATE INDEX idx_vendor_responses_review_state ON vendor_responses(review_state);
