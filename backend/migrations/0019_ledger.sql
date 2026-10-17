-- Pilot batch item 8: double-entry ledger (monetization scaffold).
--
-- Designed now so every revenue line of Stages B and C fits without a
-- schema rewrite; nothing writes to it while every revenue line is off
-- (docs/architecture/ledger.md). Amounts are integer kobo.
--
-- Rules the database enforces, whatever the application does:
--   * every entry has at least two postings and balances to zero per currency
--     (checked at COMMIT, so all postings of an entry go in one transaction);
--   * a posting's currency is its account's currency;
--   * entries and postings are never updated or deleted: a mistake is
--     corrected by a reversing entry (reverses_entry_id);
--   * an entry is written once per idempotency key.
-- Sign convention: debit = positive, credit = negative.

CREATE TABLE ledger_accounts (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code                  TEXT NOT NULL UNIQUE,  -- e.g. majo:revenue:lead_credits
    name                  TEXT NOT NULL,
    type                  TEXT NOT NULL CHECK (type IN ('ASSET', 'LIABILITY', 'EQUITY', 'REVENUE', 'EXPENSE')),
    owner_type            TEXT NOT NULL CHECK (owner_type IN ('MAJO', 'VENDOR', 'BUYER', 'PROVIDER')),
    owner_id              UUID,
    currency              TEXT NOT NULL DEFAULT 'NGN' CHECK (currency ~ '^[A-Z]{3}$'),
    -- Money that belongs to buyers or vendors (escrow deposits, split
    -- pass-through). Only usable once ESCROW / SPLIT_PAYMENTS are allowed
    -- (Stage C / B with a licensed partner); the application refuses earlier.
    holds_customer_funds  BOOLEAN NOT NULL DEFAULT false,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((owner_type IN ('VENDOR', 'BUYER')) = (owner_id IS NOT NULL) OR owner_type IN ('MAJO', 'PROVIDER'))
);

CREATE TABLE ledger_entries (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key    TEXT NOT NULL UNIQUE,
    kind               TEXT NOT NULL,           -- e.g. lead_credit_purchase
    revenue_line       TEXT CHECK (revenue_line IN ('LEAD_CREDITS', 'VENDOR_PRO', 'VERIFIED_BADGE', 'RUSH_REQUEST', 'SPONSORED', 'SPLIT_PAYMENTS', 'ESCROW')),
    description        TEXT NOT NULL,
    deal_id            UUID REFERENCES deals(id),
    vendor_id          UUID REFERENCES vendors(id),
    buyer_id           UUID REFERENCES buyers(id),
    payment_ref        TEXT,                    -- the payment provider's reference
    reverses_entry_id  UUID UNIQUE REFERENCES ledger_entries(id),
    occurred_at        TIMESTAMPTZ NOT NULL,
    created_by         TEXT NOT NULL,           -- system component or operator name
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_postings (
    id           BIGSERIAL PRIMARY KEY,
    entry_id     UUID NOT NULL REFERENCES ledger_entries(id),
    account_id   UUID NOT NULL REFERENCES ledger_accounts(id),
    amount_kobo  BIGINT NOT NULL CHECK (amount_kobo <> 0),
    currency     TEXT NOT NULL
);

CREATE INDEX ledger_postings_account_idx ON ledger_postings (account_id);
CREATE INDEX ledger_postings_entry_idx ON ledger_postings (entry_id);
CREATE INDEX ledger_entries_deal_idx ON ledger_entries (deal_id) WHERE deal_id IS NOT NULL;
CREATE INDEX ledger_entries_vendor_idx ON ledger_entries (vendor_id) WHERE vendor_id IS NOT NULL;

-- A posting's currency must be its account's currency.
CREATE FUNCTION ledger_posting_currency() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.currency <> (SELECT currency FROM ledger_accounts WHERE id = NEW.account_id) THEN
        RAISE EXCEPTION 'ledger: posting currency % does not match the account currency', NEW.currency;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER ledger_posting_currency BEFORE INSERT ON ledger_postings
    FOR EACH ROW EXECUTE FUNCTION ledger_posting_currency();

-- Every entry balances (per currency) and has at least two postings, checked
-- when the transaction commits.
CREATE FUNCTION ledger_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    eid UUID := NEW.entry_id;
    n INT;
    unbalanced TEXT;
BEGIN
    SELECT count(*) INTO n FROM ledger_postings WHERE entry_id = eid;
    IF n < 2 THEN
        RAISE EXCEPTION 'ledger: entry % has % posting(s); at least 2 are required', eid, n;
    END IF;
    SELECT currency INTO unbalanced FROM ledger_postings WHERE entry_id = eid
        GROUP BY currency HAVING sum(amount_kobo) <> 0 LIMIT 1;
    IF unbalanced IS NOT NULL THEN
        RAISE EXCEPTION 'ledger: entry % does not balance in %', eid, unbalanced;
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER ledger_postings_balanced AFTER INSERT ON ledger_postings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_entry_balanced();
-- An entry with no postings at all is caught too.
CREATE FUNCTION ledger_entry_has_postings() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM ledger_postings WHERE entry_id = NEW.id) THEN
        RAISE EXCEPTION 'ledger: entry % has no postings', NEW.id;
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER ledger_entries_have_postings AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_entry_has_postings();

-- Append-only.
CREATE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: % is append-only; post a reversing entry instead', TG_TABLE_NAME;
END $$;
CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_postings_append_only BEFORE UPDATE OR DELETE ON ledger_postings
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_postings_no_truncate BEFORE TRUNCATE ON ledger_postings
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();

-- Balances per account (debit-positive; for LIABILITY, EQUITY and REVENUE
-- accounts the natural balance is the negative of this).
CREATE VIEW ledger_balances AS
SELECT a.id AS account_id, a.code, a.type, a.currency,
       COALESCE(sum(p.amount_kobo), 0)::BIGINT AS debit_minus_credit_kobo
FROM ledger_accounts a
LEFT JOIN ledger_postings p ON p.account_id = a.id
GROUP BY a.id, a.code, a.type, a.currency;
