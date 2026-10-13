-- Reuse-audit R1: operator access control and control-plane read views.
--
-- 1. operators: named operator credentials. Each person gets their own token
--    (only its SHA-256 hash is stored) and a role:
--      VIEWER   - read routes only (GET /api/...)
--      OPERATOR - every control-plane route
--    The shared OPERATOR_API_TOKEN remains the bootstrap/break-glass
--    credential. Managed with `go run ./cmd/operatorctl`.
-- 2. An index for the dashboard's event timeline (newest first).

CREATE TABLE operators (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL UNIQUE CHECK (length(trim(name)) BETWEEN 1 AND 80),
    role          TEXT NOT NULL CHECK (role IN ('VIEWER', 'OPERATOR')),
    token_hash    BYTEA NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    active        BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ
);

CREATE INDEX idx_events_created_at ON events (created_at DESC);
