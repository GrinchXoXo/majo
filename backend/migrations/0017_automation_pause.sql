-- Pilot batch item 4: global automation pause.
--
-- One operator switch that stops every automatic step at once (confirming,
-- routing, re-routing, questions to buyers and vendors, automatic offer
-- delivery, queued notices) without a restart. Per-request holds
-- (workflow_holds) are unchanged. History is kept: a pause is never
-- updated except to record its release.
CREATE TABLE automation_pauses (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope          TEXT NOT NULL DEFAULT 'global' CHECK (scope = 'global'),
    paused_by      TEXT NOT NULL,
    reason         TEXT NOT NULL CHECK (btrim(reason) <> ''),
    paused_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_by    TEXT,
    release_reason TEXT,
    released_at    TIMESTAMPTZ,
    CHECK ((released_at IS NULL) = (released_by IS NULL))
);

-- At most one active pause.
CREATE UNIQUE INDEX uq_automation_pauses_active ON automation_pauses (scope) WHERE released_at IS NULL;
