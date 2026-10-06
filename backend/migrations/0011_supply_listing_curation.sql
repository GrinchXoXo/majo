-- Phase 4A Step 4: operator curation of SupplyListings.
-- Additive only; no existing row or column is changed.
--
-- Rejection reuses the existing ARCHIVED status (Architecture Lock V2:
-- "reject (-> ARCHIVED + reason)"). These two nullable columns record that
-- an ARCHIVED listing was archived by an operator rejection, and why.
-- No REJECTED status is introduced.
ALTER TABLE supply_listings ADD COLUMN rejected_at TIMESTAMPTZ;
ALTER TABLE supply_listings ADD COLUMN rejection_reason TEXT;

-- A rejection is always an archival.
ALTER TABLE supply_listings ADD CONSTRAINT supply_listings_rejected_is_archived
    CHECK (rejected_at IS NULL OR status = 'ARCHIVED');

-- The operator draft queue: status filter + created_at DESC.
CREATE INDEX IF NOT EXISTS idx_supply_listings_status_created
    ON supply_listings (status, created_at DESC);
