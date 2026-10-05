-- Phase 2 schema: vendor network, supply signals, vendor requests/responses.
-- Per PRD 2 §6-11 and §11-13: vendors are operator-onboarded (no public
-- registration), categories/service areas are simple join tables (no
-- geospatial infra), and SupplySignal is the first-class, time-sensitive
-- representation of vendor-claimed availability.

CREATE TABLE vendors (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    business_name       TEXT NOT NULL,
    whatsapp_external_id TEXT NOT NULL UNIQUE,  -- canonical identity for incoming message matching (PRD 2 §9)
    phone               TEXT,
    status              TEXT NOT NULL DEFAULT 'PENDING', -- PENDING | ACTIVE | INACTIVE | BLOCKED
    primary_location    TEXT,
    notes               TEXT,
    requests_received    INTEGER NOT NULL DEFAULT 0,
    responses_received    INTEGER NOT NULL DEFAULT 0,
    last_response_at     TIMESTAMPTZ,
    last_message_at      TIMESTAMPTZ,
    last_supply_submission_at TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_vendors_whatsapp_id ON vendors(whatsapp_external_id);
CREATE INDEX idx_vendors_status ON vendors(status);

-- Simple join table; a vendor may belong to multiple categories (PRD 2 §7).
CREATE TABLE vendor_categories (
    vendor_id  UUID NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    category   TEXT NOT NULL,
    PRIMARY KEY (vendor_id, category)
);
CREATE INDEX idx_vendor_categories_category ON vendor_categories(category);

-- Service areas beyond the primary location (PRD 2 §8).
CREATE TABLE vendor_service_areas (
    vendor_id  UUID NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    location   TEXT NOT NULL,
    PRIMARY KEY (vendor_id, location)
);
CREATE INDEX idx_vendor_service_areas_location ON vendor_service_areas(location);

-- SupplySignal: what a vendor has indicated they currently have, can
-- source, or can otherwise supply. Time-sensitive (PRD 2 §12): default
-- 7-day expiry, configurable, not a permanent catalog entry.
CREATE TABLE supply_signals (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor_id          UUID NOT NULL REFERENCES vendors(id) ON DELETE CASCADE,
    category           TEXT,
    product            TEXT,
    normalized_product TEXT,          -- lowercased/trimmed for matching (PRD 2 §19)
    variant            TEXT,
    condition          TEXT,
    price              BIGINT,
    currency           TEXT NOT NULL DEFAULT 'NGN',
    quantity           INTEGER,
    location           TEXT,
    availability_status TEXT NOT NULL DEFAULT 'AVAILABLE', -- AVAILABLE | CAN_SOURCE | UNAVAILABLE | EXPIRED
    raw_message        TEXT NOT NULL,
    extracted_data     JSONB NOT NULL DEFAULT '{}'::jsonb,
    confidence         DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '7 days')
);

CREATE INDEX idx_supply_signals_vendor_id ON supply_signals(vendor_id);
CREATE INDEX idx_supply_signals_normalized_product ON supply_signals(normalized_product);
CREATE INDEX idx_supply_signals_status ON supply_signals(availability_status);

-- VendorRequest: Middleman asking a specific vendor whether they can
-- fulfil a specific BuyerRequest (PRD 2 §31-32). public_reference is the
-- human-readable ID vendors see (e.g. "MM-1042") — internal UUIDs are
-- never exposed to vendors.
CREATE TABLE vendor_requests (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_request_id UUID NOT NULL REFERENCES buyer_requests(id),
    vendor_id        UUID NOT NULL REFERENCES vendors(id),
    public_reference TEXT NOT NULL UNIQUE,
    status           TEXT NOT NULL DEFAULT 'PENDING', -- PENDING | SENT | RESPONDED | EXPIRED | CANCELLED
    match_score      INTEGER,
    match_breakdown  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at          TIMESTAMPTZ,

    -- A vendor should not receive two simultaneously-active requests for
    -- the same buyer request (PRD 2 §47: "should not accidentally create
    -- duplicate active VendorRequests for the same vendor").
    UNIQUE (buyer_request_id, vendor_id)
);

CREATE INDEX idx_vendor_requests_buyer_request ON vendor_requests(buyer_request_id);
CREATE INDEX idx_vendor_requests_vendor ON vendor_requests(vendor_id);
CREATE INDEX idx_vendor_requests_status ON vendor_requests(status);

-- VendorResponse: the vendor's structured reply to a VendorRequest
-- (PRD 2 §35-38). raw_message always preserved; corrections never
-- overwrite it.
CREATE TABLE vendor_responses (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor_request_id UUID NOT NULL REFERENCES vendor_requests(id),
    status           TEXT NOT NULL, -- AVAILABLE | UNAVAILABLE | CAN_SOURCE | ALTERNATIVE
    product          TEXT,
    variant          TEXT,
    condition        TEXT,
    price            BIGINT,
    currency         TEXT NOT NULL DEFAULT 'NGN',
    quantity         INTEGER,
    location         TEXT,
    notes            TEXT,
    raw_message      TEXT NOT NULL,
    extracted_data   JSONB NOT NULL DEFAULT '{}'::jsonb,
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_vendor_responses_vendor_request ON vendor_responses(vendor_request_id);
