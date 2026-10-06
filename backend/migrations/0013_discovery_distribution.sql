-- Phase 4B: Discovery & Distribution foundation (Architecture Lock V2 §E-§H, §K.1).
--
-- Discovery itself stores nothing (it is a pure capability over the canonical
-- eligibility policy). Everything here belongs to Distribution (consent,
-- batches, deliveries, exposure, interactions) or to the existing commerce
-- loop (request origin and listing provenance). No second inventory, request
-- or transaction model is introduced.

-- 1. Consent: its own auditable record, never a field on Buyer or BuyerRequest.
CREATE TABLE discovery_preferences (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id          UUID NOT NULL REFERENCES buyers(id),
    scope             TEXT NOT NULL DEFAULT 'discovery' CHECK (scope = 'discovery'),
    status            TEXT NOT NULL CHECK (status IN ('OPTED_IN', 'OPTED_OUT')),
    -- Viewer context the buyer gave (or staff set for test records).
    location          TEXT,
    interests         TEXT[] NOT NULL DEFAULT '{}',
    wording_version   TEXT NOT NULL,
    -- whatsapp_button | whatsapp_keyword | operator_staff
    source            TEXT NOT NULL,
    source_message_id UUID REFERENCES messages(id),
    channel           TEXT NOT NULL DEFAULT 'whatsapp',
    captured_at       TIMESTAMPTZ NOT NULL,           -- when the current status was given
    opted_out_at      TIMESTAMPTZ,
    opt_out_reason    TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (buyer_id, scope)
);

-- Append-only consent history: every change, who/what made it, from which message.
CREATE TABLE discovery_preference_history (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    preference_id     UUID NOT NULL REFERENCES discovery_preferences(id),
    buyer_id          UUID NOT NULL REFERENCES buyers(id),
    from_status       TEXT,
    to_status         TEXT NOT NULL,
    source            TEXT NOT NULL,
    source_message_id UUID REFERENCES messages(id),
    wording_version   TEXT NOT NULL,
    actor_type        TEXT NOT NULL,     -- BUYER | OPERATOR
    actor_id          TEXT,
    reason            TEXT,
    location          TEXT,
    interests         TEXT[] NOT NULL DEFAULT '{}',
    at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_discovery_pref_history_buyer ON discovery_preference_history (buyer_id, at);

-- 2. Batches: one operator-triggered discovery run (no scheduler, no campaigns).
CREATE TABLE discovery_batches (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_key           TEXT NOT NULL UNIQUE,  -- idempotent creation
    status               TEXT NOT NULL CHECK (status IN ('PLANNED', 'SENDING', 'SENT', 'PARTIAL', 'FAILED', 'CANCELLED')),
    mode                 TEXT NOT NULL,         -- feature-gate mode at creation (INTERNAL)
    ranking_version      TEXT NOT NULL,         -- Discovery ranking + eligibility config
    presentation_version TEXT NOT NULL,
    arms                 TEXT[] NOT NULL,
    scope                JSONB NOT NULL DEFAULT '{}'::jsonb,
    plan                 JSONB NOT NULL DEFAULT '{}'::jsonb, -- dry-run report (audience, exclusions, counts)
    created_by           TEXT,
    note                 TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    send_started_at      TIMESTAMPTZ,
    sent_at              TIMESTAMPTZ
);

-- 3. Deliveries: what one buyer was (to be) shown in one batch.
CREATE TABLE discovery_deliveries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id            UUID NOT NULL REFERENCES discovery_batches(id),
    buyer_id            UUID NOT NULL REFERENCES buyers(id),
    recipient           TEXT NOT NULL,          -- channel address (wa_id)
    arm                 TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('PLANNED', 'SENDING', 'SENT', 'FAILED', 'SKIPPED')),
    -- Stable, deterministic: one delivery per (batch, buyer).
    dedupe_key          TEXT NOT NULL UNIQUE,
    consent_status      TEXT NOT NULL,          -- consent at planning, re-checked at send
    preference_id       UUID REFERENCES discovery_preferences(id),
    listing_ids         UUID[] NOT NULL,
    candidates          JSONB NOT NULL,         -- score, reasons, eligibility + reach snapshot per listing
    presentation        TEXT,                   -- session | template
    skip_reason         TEXT,
    error               TEXT,
    message_id          UUID REFERENCES messages(id),   -- first outbound message
    provider_message_id TEXT,
    planned_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at          TIMESTAMPTZ,
    sent_at             TIMESTAMPTZ,
    first_interaction_at TIMESTAMPTZ,
    last_interaction_at TIMESTAMPTZ,
    interaction_count   INTEGER NOT NULL DEFAULT 0,
    last_action         TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_discovery_deliveries_batch ON discovery_deliveries (batch_id);
-- Frequency caps: deliveries per buyer per period.
CREATE INDEX idx_discovery_deliveries_buyer_sent ON discovery_deliveries (buyer_id, status, claimed_at);

-- 4. Exposure: which listing reached which buyer when (per-listing dedupe).
CREATE TABLE discovery_exposures (
    delivery_id UUID NOT NULL REFERENCES discovery_deliveries(id),
    buyer_id    UUID NOT NULL REFERENCES buyers(id),
    listing_id  UUID NOT NULL REFERENCES supply_listings(id),
    exposed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (delivery_id, listing_id)
);
CREATE INDEX idx_discovery_exposures_buyer_listing ON discovery_exposures (buyer_id, listing_id, exposed_at);

-- 5. Interactions: every structured tap, resolved or not (diagnostics).
CREATE TABLE discovery_interactions (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    inbound_message_id UUID NOT NULL UNIQUE REFERENCES messages(id), -- idempotent per inbound message
    buyer_id           UUID NOT NULL REFERENCES buyers(id),
    delivery_id        UUID REFERENCES discovery_deliveries(id),     -- NULL when unknown/foreign/malformed
    raw_id             TEXT NOT NULL,
    action             TEXT,
    listing_ref        TEXT,
    listing_id         UUID REFERENCES supply_listings(id),
    category           TEXT,
    outcome            TEXT NOT NULL CHECK (outcome IN ('APPLIED', 'UNRESOLVED')),
    reason             TEXT,
    buyer_request_id   UUID REFERENCES buyer_requests(id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_discovery_interactions_delivery ON discovery_interactions (delivery_id);
CREATE INDEX idx_discovery_interactions_buyer ON discovery_interactions (buyer_id, created_at);

-- 6. Demand bridge (Lock §E): one BuyerRequest model with an origin.
ALTER TABLE buyer_requests
    ADD COLUMN origin TEXT NOT NULL DEFAULT 'message' CHECK (origin IN ('message', 'discovery', 'listing_link')),
    ADD COLUMN listing_id UUID REFERENCES supply_listings(id),
    ADD COLUMN discovery_delivery_id UUID REFERENCES discovery_deliveries(id);
-- "Ask about it" on the same listing of the same delivery is one request,
-- however many times the tap is delivered.
CREATE UNIQUE INDEX uq_buyer_requests_discovery_ask ON buyer_requests (discovery_delivery_id, listing_id)
    WHERE origin = 'discovery';

-- 7. Listing provenance on the vendor request (re-confirmation of a listing).
ALTER TABLE vendor_requests ADD COLUMN supply_listing_id UUID REFERENCES supply_listings(id);

-- 8. Outbound media reuse: a stored asset is uploaded to the channel once and
-- the provider's outbound media ID is reused until it expires. No copy of the
-- bytes is made per delivery.
ALTER TABLE media_assets
    ADD COLUMN outbound_provider_ref TEXT,
    ADD COLUMN outbound_ref_expires_at TIMESTAMPTZ;
