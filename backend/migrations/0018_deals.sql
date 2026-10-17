-- Pilot batch item 5: deal confirmation, ratings and the free-deal counter
-- (monetization Stage A).
--
-- A transaction records that a buyer CHOSE an offer; nothing says the sale
-- happened (buyers pay vendors directly in the pilot). A deal is Majo's
-- follow-up on that choice: some hours later both sides are asked "did you
-- buy / sell?". Both YES = a CONFIRMED deal, numbered in order of
-- confirmation; the first FREE_DEALS (default 100) are marked free. The
-- buyer then rates the seller 1-5.
CREATE TABLE deals (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id      UUID NOT NULL UNIQUE REFERENCES transactions(id),
    buyer_request_id    UUID NOT NULL REFERENCES buyer_requests(id),
    buyer_id            UUID NOT NULL REFERENCES buyers(id),
    vendor_id           UUID NOT NULL REFERENCES vendors(id),
    conversation_id     UUID NOT NULL REFERENCES conversations(id), -- the buyer's conversation
    vendor_request_id   UUID REFERENCES vendor_requests(id),
    item                TEXT NOT NULL,
    reference           TEXT NOT NULL DEFAULT '',
    amount              BIGINT,
    status              TEXT NOT NULL DEFAULT 'PENDING'
                        CHECK (status IN ('PENDING', 'ASKED', 'CONFIRMED', 'BUYER_ONLY', 'VENDOR_ONLY', 'DECLINED', 'DISPUTED', 'NO_ANSWER')),
    ask_at              TIMESTAMPTZ NOT NULL,
    ask_attempts        INT NOT NULL DEFAULT 0,
    buyer_asked_at      TIMESTAMPTZ,
    vendor_asked_at     TIMESTAMPTZ,
    buyer_unreachable   BOOLEAN NOT NULL DEFAULT false,
    vendor_unreachable  BOOLEAN NOT NULL DEFAULT false,
    buyer_answer        TEXT CHECK (buyer_answer IN ('YES', 'NO', 'NOT_YET')),
    buyer_answered_at   TIMESTAMPTZ,
    vendor_answer       TEXT CHECK (vendor_answer IN ('YES', 'NO', 'NOT_YET')),
    vendor_answered_at  TIMESTAMPTZ,
    confirmed_at        TIMESTAMPTZ,
    deal_number         INT UNIQUE CHECK (deal_number > 0),
    free                BOOLEAN NOT NULL DEFAULT false,
    buyer_rating        SMALLINT CHECK (buyer_rating BETWEEN 1 AND 5),
    buyer_rated_at      TIMESTAMPTZ,
    closed_at           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'CONFIRMED') = (deal_number IS NOT NULL)),
    CHECK (NOT free OR deal_number IS NOT NULL)
);

CREATE INDEX idx_deals_status ON deals (status);
CREATE INDEX idx_deals_vendor ON deals (vendor_id) WHERE buyer_rating IS NOT NULL;
