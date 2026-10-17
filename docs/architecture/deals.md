# Deals: introduction, follow-up, ratings, free-deal counter

Pilot batch item 5 (monetization Stage A). Code: `internal/deal`,
`internal/deal/answer`, `infrastructure/postgres/deal_repository.go`,
migration `0018_deals.sql`.

## Why

A transaction records that a buyer **chose** an offer. In the pilot buyers
pay sellers directly, so Majo never sees the sale. Two things were missing:

1. **Nobody was introduced.** The buyer was told "payment and delivery will
   be handled in the next step" and the seller was told nothing.
2. **Nothing said whether a sale happened**, which the monetization plan
   needs ("a deal is counted when both answer yes"; the first 100 are free).

## Flow

```
buyer replies "1" ─▶ transaction (UNPAID) ─▶ deal PENDING
   │                                           │
   │ confirmation to the buyer:                │ to the seller: "A Majo buyer chose
   │ seller name, wa.me link, reference,       │ your offer ... they'll message you"
   │ safety advice (pay on delivery, ...)      │
   ▼                                           ▼
            DEAL_FOLLOWUP_DELAY later (default 6h, inside the 24h window)
   buyer: "did you buy ...?"          seller: "did you sell ...?"   (YES / NO / NOT YET)
            │                                   │
            ▼                                   ▼
   YES + YES ─▶ CONFIRMED, deal #N (free while N ≤ FREE_DEALS) ─▶ buyer rates the seller 1-5
   YES + NO  ─▶ DISPUTED ─▶ operator exception (SAFETY)
   NO  + NO  ─▶ DECLINED
   NOT YET   ─▶ asked once more DEAL_FOLLOWUP_DELAY later
   rating 1-2 ─▶ operator exception "low_rating"
   after DEAL_ANSWER_TTL (72h): BUYER_ONLY / VENDOR_ONLY / DECLINED / NO_ANSWER
```

## Rules

- **Introductions.** The buyer always gets the seller's link. The seller gets
  the buyer's link only with `INTRO_SHARE_BUYER_CONTACT=true` (owner decision;
  default off: the buyer makes contact). A choice made by an operator or the
  API also introduces the seller to the buyer.
- **Questions use the existing clarification system** (one open question per
  conversation). A deal question never replaces a question the buyer or seller
  is answering: it waits (bounded). The seller is not asked while they owe a
  reply on another buyer request (a bare "yes" would be ambiguous).
- **Answers are read deterministically** (`deal/answer`: English and Pidgin
  phrase lists; never the AI). A message that is not an answer is asked about
  once more; a new request or a seller's price reply takes the normal path.
- **Paused automation** (global or the request's hold) postpones the follow-up.
- **Numbering** happens in the same statement that confirms, under an advisory
  lock: no gaps, no duplicates; the table rejects CONFIRMED without a number.
- **Metrics.** Deal questions and deal exceptions are excluded from the
  request-understanding metrics (they happen after the sale).

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `DEAL_FOLLOWUP` | `true` | introductions and follow-ups on/off |
| `DEAL_FOLLOWUP_DELAY` | `6h` | wait after the choice; keep under 24h |
| `DEAL_ANSWER_TTL` | `72h` | how long answers are awaited |
| `FREE_DEALS` | `100` | confirmed deals marked free (Stage A) |
| `INTRO_SHARE_BUYER_CONTACT` | `false` | also give the seller the buyer's link |

Dashboard: **Deals** page (counter, outcomes, ratings) and
`GET /api/deals/summary`, `GET /api/deals` (operator/viewer token).

## Known limits

- A follow-up that misses a party's 24h window is not delivered (no template
  for it); that side is marked unreachable and the deal settles on the other
  side's answer.
- Ratings are stored, not yet used in ranking.
- Sellers do not rate buyers.
