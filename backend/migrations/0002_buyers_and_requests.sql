-- Phase 1 schema: Buyer identity + BuyerRequest.
-- Per PRD 1 §5-8: BuyerRequest represents the buyer's current procurement
-- need, not merely a WhatsApp message. Buyer identity is minimal —
-- no full customer account system.

CREATE TABLE buyers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel           TEXT NOT NULL DEFAULT 'whatsapp',
    external_user_id  TEXT NOT NULL,          -- WhatsApp wa_id
    display_name      TEXT,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (channel, external_user_id)
);

-- Now that buyers exist, wire the conversations table's buyer_id
-- foreign key (it was left nullable/unconstrained in Phase 0).
ALTER TABLE conversations
    ADD CONSTRAINT fk_conversations_buyer
    FOREIGN KEY (buyer_id) REFERENCES buyers(id);

-- BuyerRequest per PRD 1 §5-7.
-- raw_message and extracted_data are preserved separately from the
-- current (possibly operator-corrected) structured fields — PRD 1 §19/§48:
-- "AI extracted something != Middleman confirmed the request".
CREATE TABLE buyer_requests (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id   UUID NOT NULL REFERENCES conversations(id),
    buyer_id          UUID NOT NULL REFERENCES buyers(id),
    origin_message_id UUID REFERENCES messages(id),

    status            TEXT NOT NULL DEFAULT 'EXTRACTING',
    -- EXTRACTING -> NEEDS_REVIEW -> CONFIRMED (CORRECTED tracked via audit/events, not a separate status)

    category          TEXT,
    product           TEXT,
    variant           TEXT,

    budget_min        BIGINT,
    budget_max        BIGINT,
    currency          TEXT NOT NULL DEFAULT 'NGN',

    location          TEXT,
    condition         TEXT,
    quantity          INTEGER,
    urgency           TEXT,
    preferences       JSONB NOT NULL DEFAULT '[]'::jsonb,

    raw_message       TEXT NOT NULL,
    extracted_data    JSONB NOT NULL DEFAULT '{}'::jsonb,  -- original AI output, never overwritten
    confidence        DOUBLE PRECISION NOT NULL DEFAULT 0,

    completeness      TEXT NOT NULL DEFAULT 'insufficient', -- complete | partially_complete | insufficient

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at      TIMESTAMPTZ
);

CREATE INDEX idx_buyer_requests_buyer_id ON buyer_requests(buyer_id);
CREATE INDEX idx_buyer_requests_conversation_id ON buyer_requests(conversation_id);
CREATE INDEX idx_buyer_requests_status ON buyer_requests(status);
