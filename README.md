# Middleman — Phase 0 + Phase 1 + Phase 2

WhatsApp-native demand aggregation and procurement platform.
This repository implements **Phase 0** (foundation), **Phase 1** (core
request engine), and **Phase 2** (`PRD 2.md`, vendor/supply engine): a
confirmed buyer request can now be matched to relevant vendors, an
operator can approve routing, vendors receive and respond to requests
over WhatsApp, and responses are structured, persisted, and auditable.

## What's actually implemented

**Phase 0 (foundation)** — Go backend, Postgres, Python AI service
boundary, WhatsApp webhook boundary, provider interfaces. See git history
or prior README revisions for full detail.

**Phase 1 (core request engine)** — WhatsApp message → structured,
operator-reviewed `BuyerRequest`, with AI extraction (heuristic + optional
Anthropic), correction/audit trail, confirmation, and clarification.

**Phase 2 (vendor & supply engine):**
- **Vendor management** — operator-onboarded (no public registration,
  PRD 2 §6), with categories, service areas, and a WhatsApp identity used
  to route incoming messages.
- **SupplySignal** — vendor-submitted "I have X" claims, extracted via a
  heuristic Python extractor, time-sensitive (7-day default expiry),
  never a permanent catalog.
- **Deterministic matching engine** (`internal/matching`) — scores
  category (30) + product (30) + location (20) + condition (10) +
  availability (10) against a confirmed `BuyerRequest`. Fully
  configurable weights, explainable output (score breakdown + reasons per
  candidate), no ML/embeddings. Ineligible vendors (wrong category,
  inactive) are excluded outright rather than scored low; results are
  **never padded** to reach a target count.
- **Operator-approved routing** — matching only ever *suggests*; the
  operator selects which vendors actually receive the request
  (`POST /api/requests/:id/route`), which creates `VendorRequest` records
  with a human-readable public reference (`MM-1234`, never an internal
  UUID) and sends the WhatsApp message.
- **Vendor message routing** — the webhook now distinguishes a known,
  active vendor's WhatsApp number from a buyer's: if the vendor has an
  active (`SENT`) `VendorRequest`, the message is parsed as a
  **VendorResponse**; otherwise it's parsed as a new **SupplySignal**.
  Unknown numbers always take the buyer path — nobody silently becomes a
  vendor.
- **Vendor response extraction** — classifies `AVAILABLE` /
  `UNAVAILABLE` / `CAN_SOURCE` / `ALTERNATIVE`, deliberately refusing to
  guess on ambiguous replies ("Maybe.", "Which one?") — these come back
  confidence `0.0` for operator review rather than being hallucinated
  into a confident classification.
- **Full dashboard integration**: `/vendors` (list + operator onboarding
  form + activate), and the request detail page now has a **"Suggested
  Vendors"** section (run matching, see score/breakdown/reasons, select,
  route) and a **"Vendor Requests & Responses"** section showing live
  status and structured responses per vendor.

## What was tested (not just written)

Every Phase 2 claim was exercised against real Postgres + the AI service,
using the **exact scenario from PRD 2 §66**:

- Three vendors created and activated: **Mike Gadgets** (electronics/phones,
  Benin City), **OJ Phones** (phones, Benin City), **Furniture World**
  (furniture, Benin City).
- Mike Gadgets submitted supply via WhatsApp ("I get iPhone 13 128gb,
  clean one, 620k.") → correctly extracted and persisted as a
  `SupplySignal` (product, variant, price all correct).
- A buyer sent "Abeg I need used iPhone 13 128gb around 650k in Benin." →
  extracted and confirmed as a `BuyerRequest`.
- **`GET /api/requests/:id/matches` on the live matching engine returned
  exactly the PRD's expected result**: Mike Gadgets scored 90
  (`SUPPLY_CONFIRMED`, within budget), OJ Phones scored 50
  (`CATEGORY_ONLY`, no current supply), and **Furniture World did not
  appear at all** (no relevant category).
- The operator routed the request to both vendors → `VendorRequest`
  records created with public references (`MM-7466`, `MM-1115`),
  correctly `PENDING` (not `SENT`) since the WhatsApp send predictably
  failed against fake test credentials — proving failed sends are never
  silently marked as delivered.
- Mike Gadgets replied "Yes, 620k." → correctly parsed as
  `AVAILABLE, ₦620,000`.
- OJ Phones replied "128 no dey but 256gb dey 700k." → correctly parsed
  as `ALTERNATIVE, 256GB, ₦700,000` — **exactly matching the PRD 2 §66
  worked example's expected dashboard output.**
- A duplicate routing attempt to an already-routed vendor correctly
  returned an empty result (DB unique constraint on
  `(buyer_request_id, vendor_id)` prevents duplicate active requests,
  PRD 2 §47).
- **13 Go tests** (`go test ./...`) including a matching-engine test suite
  that reproduces the PRD 2 §66 scenario in isolation, no-padding
  behavior, inactive-vendor exclusion, and deterministic tie-breaking.
- **16 Python tests** (`pytest`) covering supply and vendor-response
  extraction against the PRD 2 §60 test dataset.
- Dashboard (`/vendors`, `/requests/[id]` matching UI) built cleanly with
  zero type errors across 9 routes.

**Real bugs caught and fixed during this testing (not hypothetical):**
1. **Category taxonomy mismatch** — Phase 1's broad extraction category
   (`"electronics"`) didn't match Phase 2's finer-grained vendor tags
   (`"phones"`, `"laptops"`), which would have silently excluded
   relevant vendors like OJ Phones from every match. Fixed with a
   `categoryFamilies` compatibility mapping in the matching engine.
2. **SQL-level category pre-filter bug** — `SuggestVendors` was querying
   vendors with an exact-match SQL filter (`category = 'electronics'`)
   *before* the matching engine's category-family logic ever ran,
   silently excluding OJ Phones from the candidate pool entirely
   regardless of the Go-level fix. Fixed by loading all active vendors
   and letting the matching engine's logic be the single source of truth
   for eligibility.
3. **Missing bare-number price parsing** — "Can get it tomorrow for 640."
   (Nigerian shorthand for ₦640,000, no "k" suffix) wasn't extracted at
   all. Added a fallback pattern for this common phrasing.
4. **ALTERNATIVE vs UNAVAILABLE ordering bug** — "128 no dey but 256gb
   dey 700k." was being classified as `UNAVAILABLE` because the
   substring "no dey" matched before the more specific "no dey but"
   alternative-offer pattern was checked. Fixed by reordering the checks.

## Requirements

- Docker and Docker Compose
- Git

For local (non-Docker) development: Go 1.22+, Python 3.12+, Node.js 22+.

## Setup

```bash
git clone <repository>
cd middleman
cp .env.example .env
# optionally set ANTHROPIC_API_KEY for LLM-backed extraction (falls back
# to the free heuristic extractor if unset)
docker compose up --build
```

## Services & ports

| Service   | Port | Purpose                              |
|-----------|------|---------------------------------------|
| postgres  | 5432 | System of record                      |
| ai        | 8000 | Python extraction service (FastAPI)   |
| backend   | 8080 | Go core API + WhatsApp webhook        |
| dashboard | 3000 | Next.js operations dashboard          |

## API

**Phase 1 — Requests:**
```
GET    /api/requests                      list requests (optional ?status=)
GET    /api/requests/:id                  single request detail
PATCH  /api/requests/:id                  apply operator corrections
POST   /api/requests/:id/confirm          mark request CONFIRMED
POST   /api/requests/:id/clarify          send a clarifying question to the buyer
GET    /api/requests/:id/events           event history
```

**Phase 2 — Vendors, matching, routing:**
```
POST   /api/vendors                       onboard a vendor (operator-driven)
GET    /api/vendors                       list vendors (optional ?status=&category=)
GET    /api/vendors/:id                   vendor detail
PATCH  /api/vendors/:id                   edit / activate / deactivate / block
GET    /api/vendors/:id/supply            list a vendor's supply signals

GET    /api/requests/:id/matches          run the matching engine, get ranked candidates
POST   /api/requests/:id/route            operator-approve: create + send VendorRequests
GET    /api/requests/:id/vendor-requests  full routing + response status for a request
PATCH  /api/vendor-responses/:id          correct an extracted vendor response
```

## Environment variables

See `.env.example`. Notably `ANTHROPIC_API_KEY` is optional — the system
runs at zero AI cost by default via the heuristic extractor.

## Migrations

- `0001_foundational_tables.sql` — conversations, messages, events, audit_logs (Phase 0)
- `0002_buyers_and_requests.sql` — buyers, buyer_requests (Phase 1)
- `0003_vendor_supply_engine.sql` — vendors, vendor_categories,
  vendor_service_areas, supply_signals, vendor_requests, vendor_responses (Phase 2)

All run automatically on backend startup, tracked in `schema_migrations`.

## Running services individually (outside Docker)

**Backend:**
```bash
cd backend
export DATABASE_URL=postgres://middleman:middleman@localhost:5432/middleman?sslmode=disable
export AI_SERVICE_URL=http://localhost:8000
go run ./cmd/server
```

**AI service:**
```bash
cd ai
pip install -r requirements.txt
uvicorn app.main:app --reload --port 8000
```

**Dashboard:**
```bash
cd dashboard
npm install
npm run dev
```

## Repository structure

```
middleman/
├── backend/
│   ├── cmd/server/     Composition root (main.go)
│   ├── internal/
│   │   ├── buyer, conversation, request   Phase 1 core
│   │   ├── vendor, supply, routing        Phase 2 core:
│   │   │                                    vendor.Repository (CRUD + categories/areas)
│   │   │                                    supply.Repository (SupplySignal)
│   │   │                                    routing.Service (matching orchestration,
│   │   │                                      VendorRequest/VendorResponse lifecycle,
│   │   │                                      inbound vendor message routing)
│   │   ├── matching                       Deterministic scoring engine (PRD 2 §16-28)
│   │   ├── event, audit, jobs             Shared infrastructure
│   │   └── offer, notification            Untouched Phase 3+ stubs
│   ├── infrastructure/  postgres, ai, whatsapp, payment, storage
│   ├── migrations/      0001, 0002, 0003
│   └── tests/, internal/*/​*_test.go
├── ai/
│   └── app/extraction.py   Buyer intent + supply + vendor response heuristics,
│                           optional Anthropic-backed extraction
├── dashboard/
│   └── app/
│       ├── requests/[id]/   Correction UI + Suggested Vendors + Vendor Requests
│       └── vendors/         Vendor onboarding + list
├── docker-compose.yml
└── .env.example
```

## What's deliberately NOT here yet

Per PRD 2 §3 and the broader roadmap, still out of scope:

- Payments, escrow, payouts, orders, checkout
- Formal vendor ratings/reputation (basic activity counters exist;
  no scoring model)
- Offer aggregation / buyer-facing comparison UI (vendor responses are
  visible to the operator; presenting them back to the buyer is Phase 3+)
- WA Shopper, ML ranking, vector search
- Redis, Kafka/NATS/RabbitMQ, Kubernetes, MinIO
- Native Vendor App
- Full operator authentication (all endpoints remain open on the internal
  network, as in Phase 1)

## Phase 3 handoff

Phase 3 (per the broader roadmap) picks up from here: aggregating
multiple vendor responses into a ranked, buyer-facing offer comparison,
and closing the loop back to the buyer via WhatsApp ("I found 3 options:
...").  The `routing.Repository.ListVendorRequestsByBuyerRequest` +
`FindResponseByVendorRequest` methods already assemble exactly the data
Phase 3's offer-aggregation logic will need — no schema changes required
to start.


