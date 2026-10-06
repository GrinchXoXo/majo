-- Phase 3.1 §23A: data readiness for the core marketplace funnel.
-- Additive only. No analytics platform: this keeps the facts needed to
-- measure the funnel later (delivered / read receipts, selection source)
-- in the existing system-of-record tables. See docs/analytics/funnel.md.

-- How a selection was made: whatsapp_reply | dashboard_operator | api.
-- Lets organic buyer selections be distinguished from operator overrides.
ALTER TABLE buyer_selections ADD COLUMN source TEXT;

-- Outbound messages are now persisted with Meta's message ID (wamid) and
-- the business purpose they served (metadata.kind / entity_type /
-- entity_id). Meta status callbacks then stamp delivery receipts here.
-- NULL delivery_status on an OUTBOUND row = accepted by the API, no
-- receipt yet. read_at is only ever set if the recipient has read receipts
-- enabled (platform-dependent), so "not read" must never be read as
-- "not seen".
ALTER TABLE messages ADD COLUMN delivery_status TEXT; -- delivered | read | failed
ALTER TABLE messages ADD COLUMN delivered_at    TIMESTAMPTZ;
ALTER TABLE messages ADD COLUMN read_at         TIMESTAMPTZ;
ALTER TABLE messages ADD COLUMN failed_at       TIMESTAMPTZ;
ALTER TABLE messages ADD COLUMN delivery_error  TEXT;

-- Status callbacks identify a message only by wamid (no conversation).
CREATE INDEX idx_messages_external_message_id
    ON messages (external_message_id) WHERE external_message_id IS NOT NULL;

-- Funnel queries filter events by type over a time window.
CREATE INDEX idx_events_type_created_at ON events (event_type, created_at);
