# Monetization scaffold: flags, payment provider, ledger (pilot batch item 8)

Status: built and switched off. Majo takes no money in Stage A. This page
covers the code side of `monetization-and-payments-rollout.md` (project
docs). The business decisions, prices and legal gates stay with the owner.

## 1. Revenue-line flags (`internal/monetization`)

| Line | Env var | Who pays | Needs stage | Needs provider |
|---|---|---|---|---|
| Lead credits | `FEATURE_LEAD_CREDITS` | vendor | B | `payment_link` or `split` |
| Vendor Pro | `FEATURE_VENDOR_PRO` | vendor | B | `payment_link` or `split` |
| Verified badge | `FEATURE_VERIFIED_BADGE` | vendor | B | `payment_link` or `split` |
| Rush request | `FEATURE_RUSH_REQUEST` | buyer | B | `payment_link` or `split` |
| Sponsored slot | `FEATURE_SPONSORED` | vendor | B | `payment_link` or `split` |
| Split payments | `FEATURE_SPLIT_PAYMENTS` | buyer (Majo keeps its fee) | B | `split` |
| Escrow | `FEATURE_ESCROW` | buyer deposit | C | `escrow_partner` |

`MONETIZATION_STAGE` is `A` by default. `PAYMENTS_PROVIDER` is `none` by
default.

Startup fails if:

- a line is on before its stage or its provider;
- an unknown value is used;
- `PAYMENTS_PROVIDER` is anything but `none`, because no other provider is
  built yet.

So a configuration change alone can never start charging anyone. Each
gate's condition, in words, is in `monetization.Requirements` and on the
dashboard's **Deals** page under "Revenue lines". The JSON is at
`GET /api/monetization` (viewer).

## 2. Payment provider boundary (`internal/payments`)

This replaces the Phase 0 placeholder in `infrastructure/payment`, which
nothing used.

- `Provider` covers:
  - checkout links for Majo's own income;
  - `VerifyCharge`: confirm with the provider and never trust a redirect;
  - `ParseWebhook`: the signature is verified before parsing;
  - `Refund`.
- `SplitProvider` adds a checkout whose vendor share settles straight to the
  vendor's sub-account. Majo never holds the vendor's share, so this is not
  escrow.
- `EscrowPartner` adds `Hold`, `Release` and `Return` with a licensed
  partner (Stage C).
- `None` is the only implementation. Every call returns `ErrDisabled`.

Building a real provider (for example Bachs or Paystack) is its own
reviewed change. It must cover:

- webhook signature verification;
- reconciliation through `VerifyCharge`;
- idempotent references;
- tests against the provider's sandbox.

## 3. Ledger (`internal/ledger`, migration `0019_ledger.sql`)

Double-entry, integer kobo, debit positive and credit negative.

**The database enforces:**

- every entry has at least two postings and sums to zero per currency
  (deferred constraint trigger, checked at COMMIT);
- a posting's currency equals its account's currency;
- entries and postings are append-only: no UPDATE, DELETE or TRUNCATE.
  Corrections are reversing entries (`reverses_entry_id`, at most one
  reversal per entry);
- one entry per `idempotency_key` (a payment reference), so a webhook
  delivered twice is booked once.

**The service enforces:**

- an entry for a revenue line that is off is refused (`ErrLineOff`);
- an entry touching an account with `holds_customer_funds` (escrow, buyer
  deposits, vendor payables) is refused until split payments or escrow are
  on (`ErrCustomerFunds`). Before that, Majo books only its own income.

### Chart of accounts

| Code | Type | Customer funds | Meaning |
|---|---|---|---|
| `majo:cash:<provider>` | asset | no | collected by the provider, not yet settled |
| `majo:bank` | asset | no | Majo's bank account |
| `majo:revenue:<line>` | revenue | no | income per revenue line |
| `majo:expense:payment_fees` | expense | no | provider fees |
| `majo:deferred:lead_credits:<vendor>` | liability | no | prepaid, unused credits (Majo owes a service, not money) |
| `customer:escrow:<partner>` | asset | **yes** | deposits held by the licensed partner |
| `customer:buyer_deposits:<buyer>` | liability | **yes** | owed back to the buyer |
| `customer:vendor_payable:<vendor>` | liability | **yes** | owed to the vendor from a deposit |

### Journal (functions in `journal.go`; all tested to balance)

| Event | Debit | Credit |
|---|---|---|
| Vendor buys credits (`LeadCreditPurchase`) | cash:provider | deferred:lead_credits:vendor |
| A credit is used on an introduction (`LeadCreditSpend`) | deferred:lead_credits:vendor | revenue:lead_credits |
| Pro / badge / rush / sponsored payment (`ServicePayment`) | cash:provider | revenue:line |
| Provider settles to the bank (`ProviderSettlement`) | bank (net) + payment_fees (fee) | cash:provider (gross) |
| Split payment, Majo's fee only (`SplitFee`) | cash:provider | revenue:split_payments |
| Escrow deposit (`EscrowDeposit`) | escrow:partner | buyer_deposits:buyer |
| Deal confirmed, release (`EscrowRelease`) | buyer_deposits:buyer | vendor_payable:vendor (net) + revenue:escrow (commission) |
| Partner pays out (`EscrowPayout`) | vendor_payable:vendor + cash:partner (commission) | escrow:partner |
| Deposit refunded (`EscrowRefund`) | buyer_deposits:buyer | escrow:partner |

Over a full escrow cycle, every customer-funds account returns to zero and
only Majo's commission remains, as revenue and as cash at the partner. The
unit test checks exactly that.

### When a line is switched on later

The change that switches a line on also:

1. builds the provider and its webhook endpoint;
2. writes the matching journal call at the business event (for example
   `LeadCreditSpend` when a deal introduction happens);
3. adds a daily reconciliation between the provider's records and
   `majo:cash:<provider>`.

The open business questions (prices, who bears the provider fee, and the
five escrow decisions) decide amounts and timing only. The ledger shape
does not change.

## Tests

- `internal/monetization`: defaults are all off; every gate.
- `internal/payments`: only `none` is built; validation.
- `internal/ledger`: validation, every journal entry balances, policy
  (line off, customer funds, idempotency), the full escrow cycle, reversal.
- `infrastructure/postgres/ledger_integration_test.go` (real Postgres):
  - post, read back, idempotency and reversal;
  - the database refuses unbalanced entries, single postings, empty
    entries, currency mismatches, zero amounts, UPDATE, DELETE and
    TRUNCATE;
  - account redefinition is refused;
  - the whole ledger sums to zero.
- `internal/config`: a line switched on in Stage A, or without a provider,
  stops startup.
