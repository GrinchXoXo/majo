-- Phase 4C hardening: recovery state the autonomous runtime needs.
--
-- 1. pending_notices: a buyer-facing message that could not be delivered
--    (e.g. the 24-hour customer-service window was closed, or a transient
--    provider failure). The business state is unchanged; the notice is sent
--    when a compliant route exists again (the buyer's next message) or by a
--    bounded retry. Never a second message model: the sent notice is an
--    ordinary recorded outbound message.
-- 2. workflow_holds: an operator pause of one request's automation. While
--    a hold is active Majo takes no automatic step for that request
--    (routing, re-routing, questions, offer delivery, notices); operator
--    overrides still work. Releasing the hold hands the request back.
-- 3. An index for the vendor-request timeout sweep.

CREATE TABLE pending_notices (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id   UUID NOT NULL REFERENCES conversations(id),
    buyer_request_id  UUID REFERENCES buyer_requests(id),
    kind              TEXT NOT NULL,              -- outbound purpose kind (no_match, routing_update, vendor_timeout, offer_delivery, ...)
    body              TEXT NOT NULL DEFAULT '',   -- '' for offer_delivery (offers are rebuilt and delivered by the offer loop)
    status            TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'CANCELLED')),
    failure_class     TEXT,                       -- channel.FailureClass of the failure that queued it
    failure_code      INTEGER,
    attempts          INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT,
    source_message_id UUID REFERENCES messages(id), -- the failed outbound message (async failures), for dedupe
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at           TIMESTAMPTZ
);
-- One queued notice per failed outbound message (a webhook reported twice).
CREATE UNIQUE INDEX uq_pending_notices_source ON pending_notices (source_message_id) WHERE source_message_id IS NOT NULL;
-- One pending offer re-delivery per request.
CREATE UNIQUE INDEX uq_pending_notices_offer ON pending_notices (buyer_request_id) WHERE kind = 'offer_delivery' AND status = 'PENDING';
CREATE INDEX idx_pending_notices_conversation ON pending_notices (conversation_id, status, created_at);

CREATE TABLE workflow_holds (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_request_id UUID NOT NULL REFERENCES buyer_requests(id),
    held_by          TEXT NOT NULL,
    reason           TEXT NOT NULL,
    held_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_by      TEXT,
    release_reason   TEXT,
    released_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_workflow_holds_active ON workflow_holds (buyer_request_id) WHERE released_at IS NULL;

CREATE INDEX idx_vendor_requests_status_sent ON vendor_requests (status, sent_at);
