-- Phase 4A.2: vendor media + caption -> grouped SupplySignal.
-- Additive only. No supply_submissions entity: a "submission" is simply the
-- set of messages recorded as one signal's sources.
--
--   supply_candidate_messages  the deterministic classification record: which
--                              inbound vendor messages were accepted as supply
--                              content, why, when the vendor sent them (Meta's
--                              timestamp), and whether their text names a
--                              product / a price. Grouping reads only this
--                              (plus breaks from messages), never re-guesses.
--   supply_signal_sources      a SupplySignal's evidence: its source messages in
--                              order, each with the stored MediaAsset (if any).
--                              message_id is UNIQUE, so one message can be the
--                              evidence of at most one signal: the database-level
--                              idempotency guarantee for grouping.

CREATE TABLE supply_candidate_messages (
    message_id       UUID PRIMARY KEY REFERENCES messages(id),
    vendor_id        UUID NOT NULL REFERENCES vendors(id),
    conversation_id  UUID NOT NULL REFERENCES conversations(id),
    kind             TEXT NOT NULL CHECK (kind IN ('text', 'media')),
    -- When the vendor sent it: Meta's message timestamp (falls back to
    -- receipt time if absent). Grouping orders by this, not insert order.
    message_at       TIMESTAMPTZ NOT NULL,
    -- Deterministic heuristic flags from the text/caption (never media).
    has_product      BOOLEAN NOT NULL DEFAULT false,
    has_price        BOOLEAN NOT NULL DEFAULT false,
    reason           TEXT NOT NULL,          -- why it was accepted as supply content
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Grouping reads one vendor conversation over a short time range.
CREATE INDEX idx_supply_candidates_vendor_conv_time
    ON supply_candidate_messages (vendor_id, conversation_id, message_at);

CREATE TABLE supply_signal_sources (
    supply_signal_id  UUID NOT NULL REFERENCES supply_signals(id) ON DELETE CASCADE,
    position          INTEGER NOT NULL CHECK (position >= 0),   -- order of the evidence
    message_id        UUID NOT NULL UNIQUE REFERENCES messages(id),
    -- The stored asset this message's media resolved to. NULL for text, and
    -- for media whose download failed (the message is still evidence, but
    -- there is no durable media to reference).
    media_asset_id    UUID REFERENCES media_assets(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (supply_signal_id, position)
);

CREATE INDEX idx_supply_signal_sources_media_asset
    ON supply_signal_sources (media_asset_id) WHERE media_asset_id IS NOT NULL;
