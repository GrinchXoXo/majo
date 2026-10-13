-- Phase 4C correction: autonomous runtime.
--
-- Majo is autonomous by default; operators are control-plane participants and
-- exception handlers. This migration adds the state the autonomous loop needs
-- to be observable and auditable: clarification questions (buyer and vendor),
-- the decision log ("why did Majo do that?") and the operator exception queue.
-- No second request, offer or transaction model is introduced.

-- 1. Clarifications: every question Majo put to a participant and its outcome.
CREATE TABLE clarifications (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    party               TEXT NOT NULL CHECK (party IN ('BUYER', 'VENDOR')),
    conversation_id     UUID REFERENCES conversations(id),
    buyer_id            UUID REFERENCES buyers(id),
    vendor_id           UUID REFERENCES vendors(id),
    buyer_request_id    UUID REFERENCES buyer_requests(id),
    vendor_request_id   UUID REFERENCES vendor_requests(id),
    vendor_response_id  UUID REFERENCES vendor_responses(id),
    kind                TEXT NOT NULL,
    field               TEXT,
    question            TEXT NOT NULL,
    options             JSONB NOT NULL DEFAULT '[]'::jsonb,
    context             JSONB NOT NULL DEFAULT '{}'::jsonb,
    status              TEXT NOT NULL CHECK (status IN ('OPEN', 'ANSWERED', 'SUPERSEDED', 'EXPIRED', 'EXHAUSTED', 'CANCELLED')),
    attempts            INTEGER NOT NULL DEFAULT 1,
    question_message_id UUID REFERENCES messages(id),
    answer_message_id   UUID REFERENCES messages(id),
    answer_text         TEXT,
    resolution          JSONB,
    asked_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL,
    resolved_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((party = 'BUYER' AND conversation_id IS NOT NULL) OR (party = 'VENDOR' AND vendor_id IS NOT NULL))
);
-- A conversation has at most one open question per party: the newest one is
-- what the participant is answering (older ones are SUPERSEDED).
CREATE UNIQUE INDEX uq_clarifications_open_buyer ON clarifications (conversation_id) WHERE status = 'OPEN' AND party = 'BUYER';
CREATE UNIQUE INDEX uq_clarifications_open_vendor ON clarifications (vendor_id) WHERE status = 'OPEN' AND party = 'VENDOR';
CREATE INDEX idx_clarifications_request ON clarifications (buyer_request_id);
CREATE INDEX idx_clarifications_vendor_request ON clarifications (vendor_request_id);

-- 2. Decision log: append-only record of each autonomous decision (typed
-- proposal -> deterministic policy -> outcome), with confidence and inputs.
CREATE TABLE workflow_decisions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_type    TEXT NOT NULL,
    subject_type     TEXT NOT NULL,
    subject_id       UUID NOT NULL,
    buyer_request_id UUID REFERENCES buyer_requests(id),
    decision         TEXT NOT NULL,
    outcome          TEXT NOT NULL CHECK (outcome IN ('APPLIED', 'REJECTED', 'DEFERRED', 'FAILED')),
    fallback         TEXT,
    reason           TEXT,
    confidence       NUMERIC(4, 3),
    policy_version   TEXT NOT NULL,
    inputs           JSONB NOT NULL DEFAULT '{}'::jsonb,
    result           JSONB NOT NULL DEFAULT '{}'::jsonb,
    actor            TEXT NOT NULL DEFAULT 'SYSTEM',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_workflow_decisions_request ON workflow_decisions (buyer_request_id, created_at);
CREATE INDEX idx_workflow_decisions_subject ON workflow_decisions (subject_type, subject_id);
CREATE INDEX idx_workflow_decisions_kind ON workflow_decisions (decision, outcome);

-- 3. Operator exceptions: only what genuinely needs the control plane.
CREATE TABLE workflow_exceptions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    class            TEXT NOT NULL CHECK (class IN ('SAFETY_EXCEPTION', 'SYSTEM_EXCEPTION')),
    reason           TEXT NOT NULL,
    subject_type     TEXT NOT NULL,
    subject_id       UUID NOT NULL,
    buyer_request_id UUID REFERENCES buyer_requests(id),
    detail           JSONB NOT NULL DEFAULT '{}'::jsonb,
    status           TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'RESOLVED')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at      TIMESTAMPTZ,
    resolved_by      TEXT,
    resolution       TEXT
);
-- One open exception per subject and reason (a retry storm raises it once).
CREATE UNIQUE INDEX uq_workflow_exceptions_open ON workflow_exceptions (subject_type, subject_id, reason) WHERE status = 'OPEN';
CREATE INDEX idx_workflow_exceptions_status ON workflow_exceptions (status, created_at);

-- 4. Vendor request expiry is now real: when it happened.
ALTER TABLE vendor_requests ADD COLUMN expired_at TIMESTAMPTZ;
