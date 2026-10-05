-- Phase 0 foundational schema.
-- Per PRD 0 FINAL §12: only foundational tables are created now.
-- Business tables (buyers, vendors, requests, supply_signals, offers, orders,
-- transactions, missions) are introduced in their respective phases.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Conversations represent a channel-agnostic thread with a user.
-- WhatsApp is the first channel, but this table is not WhatsApp-specific.
CREATE TABLE conversations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel      TEXT NOT NULL DEFAULT 'whatsapp',
    external_id  TEXT NOT NULL,            -- e.g. WhatsApp wa_id / phone number
    buyer_id     UUID,                     -- nullable until buyer domain exists (Phase 1+)
    status       TEXT NOT NULL DEFAULT 'ACTIVE',
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (channel, external_id)
);

-- Messages belong to a conversation. external_message_id lets us detect
-- and ignore duplicate webhook deliveries (idempotency, per PRD 0.2 §35).
CREATE TABLE messages (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id      UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    direction            TEXT NOT NULL CHECK (direction IN ('INBOUND', 'OUTBOUND')),
    external_message_id  TEXT,             -- WhatsApp message ID, unique per channel message
    message_type         TEXT NOT NULL DEFAULT 'text',
    content              JSONB NOT NULL DEFAULT '{}'::jsonb,
    metadata             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (conversation_id, external_message_id)
);

CREATE INDEX idx_messages_conversation_id ON messages(conversation_id);

-- Events: append-only record of meaningful business occurrences.
-- Persisted directly to PostgreSQL for now; no broker required (PRD 0.2 §17).
CREATE TABLE events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type   TEXT NOT NULL,
    entity_type  TEXT NOT NULL,
    entity_id    UUID,
    actor_type   TEXT NOT NULL DEFAULT 'SYSTEM',  -- SYSTEM | OPERATOR | BUYER | VENDOR
    actor_id     UUID,
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_entity ON events(entity_type, entity_id);
CREATE INDEX idx_events_event_type ON events(event_type);

-- Audit logs: operator/system actions with before/after values.
-- Important from day one because Middleman is operator-assisted at launch.
CREATE TABLE audit_logs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_type      TEXT NOT NULL DEFAULT 'OPERATOR',
    actor_id        UUID,
    action          TEXT NOT NULL,          -- e.g. REQUEST_CORRECTED, VENDOR_SHORTLIST_MODIFIED
    entity_type     TEXT NOT NULL,
    entity_id       UUID,
    previous_value  JSONB,
    new_value       JSONB,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_logs_entity ON audit_logs(entity_type, entity_id);
