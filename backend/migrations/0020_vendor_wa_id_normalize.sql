-- 01a audit: vendors saved before WhatsApp numbers were normalised
-- ("0803 123 4567", "+234 803...") never matched the incoming wa_id
-- ("2348031234567"), so their replies were treated as a buyer's. Rewrite
-- them to the wa_id form (same rules as vendor.NormalizeWhatsAppID).
-- A number that is unreadable, or whose normal form already belongs to
-- another vendor, is left as it is for the operator to fix.
WITH digits AS (
    SELECT id, regexp_replace(whatsapp_external_id, '[^0-9]', '', 'g') AS d
    FROM vendors
    WHERE whatsapp_external_id ~ '^[0-9 +()\-.]+$'
), stripped AS (
    SELECT id, CASE WHEN d LIKE '00%' THEN substr(d, 3) ELSE d END AS d FROM digits
), normal AS (
    SELECT id,
        CASE
            WHEN length(d) = 11 AND d LIKE '0%' THEN '234' || substr(d, 2)
            WHEN length(d) = 10 AND substr(d, 1, 1) IN ('7', '8', '9') THEN '234' || d
            WHEN d LIKE '2340%' THEN '234' || substr(d, 5)
            ELSE d
        END AS wa
    FROM stripped
)
UPDATE vendors v SET whatsapp_external_id = n.wa, updated_at = now()
FROM normal n
WHERE v.id = n.id
  AND v.whatsapp_external_id <> n.wa
  AND length(n.wa) BETWEEN 10 AND 15
  AND n.wa NOT LIKE '0%'
  AND NOT EXISTS (SELECT 1 FROM vendors o WHERE o.whatsapp_external_id = n.wa AND o.id <> v.id);
