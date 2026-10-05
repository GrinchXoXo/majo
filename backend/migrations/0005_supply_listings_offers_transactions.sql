-- Phase 3: supply listings (durable marketplace supply, distinct from the
-- ephemeral SupplySignal ingestion event), offers (buyer-facing, built
-- from vendor responses and/or listings), and the transaction boundary
-- (buyer selection -> transaction shell, no payment execution).

-- SupplyListing: "Middleman currently considers this available for
-- discovery/matching" — distinct from SupplySignal's "the vendor told us
-- this" (PRD 3 §5). Always traceable back to the signal(s) that produced
-- it for provenance/auditability.
CREATE TABLE supply_listings (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor_id          UUID NOT NULL REFERENCES vendors(id),
    supply_signal_id   UUID REFERENCES supply_signals(id),

    category           TEXT,
    product            TEXT,
    normalized_product TEXT,
    variant            TEXT,
    condition          TEXT,
    price              BIGINT,
    currency           TEXT NOT NULL DEFAULT 'NGN',
    quantity           INTEGER,
    location           TEXT,

    description        TEXT,
    media_references   JSONB NOT NULL DEFAULT '[]'::jsonb, -- [{type, storage_key/url}], provider-agnostic

    -- DRAFT | ACTIVE | PAUSED | SOLD | UNAVAILABLE | EXPIRED | ARCHIVED
    status             TEXT NOT NULL DEFAULT 'DRAFT',

    source_channel     TEXT NOT NULL DEFAULT 'whatsapp',
    source_message_id  UUID REFERENCES messages(id),
    extraction_method  TEXT NOT NULL DEFAULT 'heuristic',
    confidence         DOUBLE PRECISION NOT NULL DEFAULT 0,

    published_at       TIMESTAMPTZ,
    last_verified_at   TIMESTAMPTZ,
    expires_at         TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '7 days'),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_supply_listings_vendor ON supply_listings(vendor_id);
CREATE INDEX idx_supply_listings_signal ON supply_listings(supply_signal_id);
CREATE INDEX idx_supply_listings_status ON supply_listings(status);
CREATE INDEX idx_supply_listings_normalized_product ON supply_listings(normalized_product);

-- Offer: a buyer-facing, ranked representation of something that can
-- fulfil a BuyerRequest. May originate from a VendorResponse (request-
-- specific) and/or point at a pre-existing SupplyListing (discovery-
-- specific) — PRD 3 §17 explicitly requires both origins to be
-- representable, so both foreign keys are nullable.
CREATE TABLE offers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_request_id  UUID NOT NULL REFERENCES buyer_requests(id),
    vendor_request_id UUID REFERENCES vendor_requests(id),
    vendor_response_id UUID REFERENCES vendor_responses(id),
    supply_listing_id UUID REFERENCES supply_listings(id),
    vendor_id         UUID NOT NULL REFERENCES vendors(id),

    type              TEXT NOT NULL, -- EXACT | ALTERNATIVE | CAN_SOURCE | PARTIAL
    status            TEXT NOT NULL DEFAULT 'DRAFT', -- DRAFT|REVIEWED|SHORTLISTED|SENT|ACCEPTED|REJECTED|EXPIRED

    product           TEXT,
    variant           TEXT,
    condition         TEXT,
    price             BIGINT,
    currency          TEXT NOT NULL DEFAULT 'NGN',
    quantity          INTEGER,
    location          TEXT,
    notes             TEXT,

    ranking_score     INTEGER NOT NULL DEFAULT 0,
    ranking_reasons   JSONB NOT NULL DEFAULT '[]'::jsonb,
    confidence        DOUBLE PRECISION NOT NULL DEFAULT 0,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ,

    -- An offer must originate from at least one traceable source.
    CONSTRAINT offers_has_origin CHECK (vendor_response_id IS NOT NULL OR supply_listing_id IS NOT NULL)
);

CREATE INDEX idx_offers_buyer_request ON offers(buyer_request_id);
CREATE INDEX idx_offers_vendor ON offers(vendor_id);
CREATE INDEX idx_offers_status ON offers(status);

-- BuyerSelection: the buyer chose an offer. Deliberately NOT a
-- transaction — "Offer Accepted" and "Transaction Created" are distinct
-- steps (PRD 3 §20-21).
CREATE TABLE buyer_selections (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_request_id UUID NOT NULL REFERENCES buyer_requests(id),
    offer_id         UUID NOT NULL REFERENCES offers(id),
    buyer_id         UUID NOT NULL REFERENCES buyers(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_buyer_selections_buyer_request ON buyer_selections(buyer_request_id);

-- Transaction: the boundary for a future real payment/fulfilment engine.
-- No payment execution happens here — this table exists so the Offer
-- model doesn't dead-end (PRD 3 §21), and so gross/fee/net amounts are
-- structurally distinguished from day one (PRD 3 §24) rather than
-- retrofitted once commission logic is needed.
CREATE TABLE transactions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_selection_id UUID NOT NULL REFERENCES buyer_selections(id),
    buyer_request_id  UUID NOT NULL REFERENCES buyer_requests(id),
    offer_id          UUID NOT NULL REFERENCES offers(id),
    buyer_id          UUID NOT NULL REFERENCES buyers(id),
    vendor_id         UUID NOT NULL REFERENCES vendors(id),
    supply_listing_id UUID REFERENCES supply_listings(id),

    -- CREATED | PAYMENT_PENDING | PAID | PAYMENT_FAILED | FULFILLING |
    -- COMPLETED | CANCELLED | REFUNDED | PARTIALLY_REFUNDED
    status            TEXT NOT NULL DEFAULT 'CREATED',
    payment_status    TEXT NOT NULL DEFAULT 'UNPAID', -- UNPAID|PAYMENT_PENDING|PAID|PAYMENT_FAILED|REFUNDED|PARTIALLY_REFUNDED
    fulfilment_status TEXT NOT NULL DEFAULT 'PENDING',

    gross_amount      BIGINT NOT NULL,
    platform_fee      BIGINT NOT NULL DEFAULT 0,
    payment_fee       BIGINT NOT NULL DEFAULT 0,
    vendor_amount     BIGINT NOT NULL DEFAULT 0,
    refund_amount     BIGINT NOT NULL DEFAULT 0,
    currency          TEXT NOT NULL DEFAULT 'NGN',

    payment_provider  TEXT,
    payment_reference TEXT,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_transactions_buyer_selection ON transactions(buyer_selection_id);
CREATE INDEX idx_transactions_vendor ON transactions(vendor_id);
CREATE INDEX idx_transactions_status ON transactions(status);
