-- Phase 4A.5: supply lifecycle, freshness and geographic expansion.
--
-- supply_listings.expires_at previously meant "evidence time + 7 days"
-- (the Phase 3 default, which coincided with WhatsApp's ~7-day media
-- retrieval window) and every duplicate refresh pushed it forward. From
-- this migration it means ONE thing: the listing's lifecycle ceiling.
--   * A published listing's lifecycle starts at published_at; the ceiling
--     is published_at + the max lifecycle [PARAM, initial 60 days], fixed
--     when the lifecycle starts and never extended by later evidence.
--   * A draft carries a provisional ceiling from its evidence time; the
--     real one is set when it is published.
-- Freshness (last_verified_at vs the freshness window) is separate and
-- derived; geographic scope is derived from the lifecycle age. Neither is
-- stored. Nothing is ever deleted when a listing expires.
--
-- Backfill uses the initial 60-day default (the configured value is not
-- visible to SQL). Rows that already closed (SOLD, UNAVAILABLE, EXPIRED,
-- ARCHIVED) are history and are left exactly as they are. Rows whose old
-- ceiling has already passed are history too: they are NOT backfilled, so
-- no expired listing is resurrected by this migration.

UPDATE supply_listings
   SET expires_at = published_at + interval '60 days'
 WHERE published_at IS NOT NULL AND status IN ('ACTIVE', 'PAUSED')
   AND expires_at > now();

UPDATE supply_listings
   SET expires_at = created_at + interval '60 days'
 WHERE status = 'DRAFT'
   AND expires_at > now();

-- The column default no longer encodes the 7-day assumption. The
-- application always sets expires_at explicitly from the policy.
ALTER TABLE supply_listings ALTER COLUMN expires_at SET DEFAULT (now() + interval '60 days');

COMMENT ON COLUMN supply_listings.expires_at IS
  'Lifecycle ceiling (Phase 4A.5): published_at + max lifecycle [PARAM, default 60d], set at publication, never extended. Drafts: provisional. Past it the listing is ineligible but kept as history.';
COMMENT ON COLUMN supply_listings.last_verified_at IS
  'Freshness reference (Phase 4A.5): last verification of availability (vendor evidence, vendor resubmission, operator verification). Never moved by reads, searches or matches.';

-- Lifecycle queries ("live" vs "expired" listings) filter on expires_at.
CREATE INDEX IF NOT EXISTS idx_supply_listings_status_expires ON supply_listings (status, expires_at);
