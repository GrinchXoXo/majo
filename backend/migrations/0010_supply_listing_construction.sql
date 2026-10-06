-- Phase 4A.3: SupplySignal -> SupplyListing construction.
-- Reuses the Phase 3 supply_listings table (no second listing model). Adds
-- only what the table lacked: a public reference, database-enforced
-- one-listing-per-signal, a status check, and one search normalization.

-- 1. Public reference: "L-" + 6 characters from the Crockford base32
--    alphabet (0-9 A-Z without I L O U): 32^6 ~ 1.07 billion values.
--    Uniqueness is enforced here; the application retries on collision.
ALTER TABLE supply_listings ADD COLUMN public_reference TEXT;

-- Backfill any pre-existing (Phase 3) rows so the column can be NOT NULL.
DO $$
DECLARE
    r RECORD;
    alphabet CONSTANT TEXT := '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
    candidate TEXT;
BEGIN
    FOR r IN SELECT id FROM supply_listings WHERE public_reference IS NULL ORDER BY created_at, id LOOP
        LOOP
            candidate := 'L-';
            FOR i IN 1..6 LOOP
                candidate := candidate || substr(alphabet, 1 + floor(random() * 32)::int, 1);
            END LOOP;
            EXIT WHEN NOT EXISTS (SELECT 1 FROM supply_listings WHERE public_reference = candidate);
        END LOOP;
        UPDATE supply_listings SET public_reference = candidate WHERE id = r.id;
    END LOOP;
END $$;

ALTER TABLE supply_listings ALTER COLUMN public_reference SET NOT NULL;
ALTER TABLE supply_listings ADD CONSTRAINT supply_listings_public_reference_key UNIQUE (public_reference);
ALTER TABLE supply_listings ADD CONSTRAINT supply_listings_public_reference_format
    CHECK (public_reference ~ '^L-[0-9A-HJKMNP-TV-Z]{6}$');

-- 2. Valid statuses (the Phase 3 vocabulary, unchanged).
ALTER TABLE supply_listings ADD CONSTRAINT supply_listings_status_check
    CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'SOLD', 'UNAVAILABLE', 'EXPIRED', 'ARCHIVED'));

-- 3. One live listing per signal. Before 4A.3 the "find, then create" check
--    was unprotected, so concurrent requests could create two listings from
--    one signal. Keep the oldest; archive any later twin (provenance kept,
--    removed from the marketplace). Then let the database enforce it.
UPDATE supply_listings l SET status = 'ARCHIVED', updated_at = now()
WHERE l.supply_signal_id IS NOT NULL AND l.status <> 'ARCHIVED'
  AND EXISTS (SELECT 1 FROM supply_listings o
              WHERE o.supply_signal_id = l.supply_signal_id AND o.status <> 'ARCHIVED'
                AND (o.created_at, o.id) < (l.created_at, l.id));
CREATE UNIQUE INDEX supply_listings_one_live_per_signal
    ON supply_listings (supply_signal_id) WHERE supply_signal_id IS NOT NULL AND status <> 'ARCHIVED';

-- 4. Search normalization: normalized_product is the compact search form of
--    product (lowercase, letters and digits only: "iPhone 13" -> "iphone13").
--    Phase 3 only removed spaces; recompute existing rows the same way the
--    application now does. The display value (product) is untouched.
UPDATE supply_listings SET normalized_product = regexp_replace(lower(product), '[^[:alnum:]]+', '', 'g')
WHERE product IS NOT NULL;
