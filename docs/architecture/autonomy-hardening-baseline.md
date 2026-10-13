# Autonomy hardening: baseline (before changes)

Inspected on 13 Oct 2026 in the sandbox. This is not the user's local repository.

| Item | Value |
|---|---|
| Repository | `/home/claude/mm/middleman` |
| Branch | `phase-4a.1-media-ingestion` |
| HEAD | `3aae7c8` (Phase 4C correction) |
| Parent | `6f11ab1` (Phase 4B) → `bd7a670` → `5d7208b` (4A.5) → … → `d8daece` (4A step 2) |
| Working tree | Clean (nothing uncommitted before this work) |

## Baseline test run (unchanged code, exit codes captured)

| Suite | Command | Exit | Result |
|---|---|---|---|
| go vet | `go vet ./...` | 0 | clean |
| Go unit + Postgres integration | `MAJO_TEST_DATABASE_URL=… go test -count=1 -json ./...` | 0 | 409 pass (325 top-level), 0 fail, 0 skip |
| Go race | `go test -race -count=1 ./...` | 0 | 22 packages ok |
| Python AI | `python3 -m pytest -q` | 0 | 36 passed |
| Dashboard types | `npx tsc --noEmit` | 0 | clean |
| Dashboard build | `npx next build` | 0 | built |
| E2E, 14 suites | `scripts/e2e/run_regression.sh` (fake Graph API) | 0 | every suite passed: phase31 76, media 14+26, grouping 36, listing 28, curation 52, matching 28, lifecycle 44, disc_commerce 74, disc_geo 66, disc_stale 27, disc_expired 47, disc_dashboard 17, autonomy 147 (288 s) |

## Verified implementation (read in code)

- **Buyer side:**
  - Auto-confirmation (`workflow.Assess` → `ConfirmAs(SYSTEM)`).
  - One question at a time per conversation (DB partial unique index plus `OpenClarificationIfFree`).
  - References to shown items.
  - Broaden question.
  - Honest no-match message.
- **Routing:** automatic, through `RoutingPolicy` (score floor, canonical eligibility, busy vendors, fan-out 3).
- **Vendor side:**
  - Association precedence, and the vendor "which request?" / detail questions.
  - The unrelated-product-is-supply guard.
- **Durable jobs:** `WORKFLOW_ADVANCE`, `BUYER_ANSWER`, `VENDOR_REQUEST_RESEND`, `OFFER_DELIVERY_RETRY`, with startup `Recover()` for requests left mid-flow.
- **Decision log, clarifications and exception queue:** all persisted (migration 0014).
- **Operator overrides:** behind `operatorAuth`.

## Test-covered behaviour

- **Autonomy E2E (147 checks, webhook path + fake provider):**
  - scenarios A–H;
  - the synchronous 131047 rejection;
  - a transient failure recovered by retry;
  - retry exhaustion.
- **Unit tests:** policy, references, `Assess`, `SpecInText`, `StrongYes`, classifier strings.
- **Integration tests:** clarification store, locks, exceptions.

## Reported but not verified, and discrepancies found

| # | Claim | What the code actually does |
|---|---|---|
| D1 | "Vendor requests expire after 72 hours" | Expiry is **lazy**. A SENT request becomes EXPIRED only when the *same vendor* sends another message (`expireStale` in the vendor path). Nothing runs at the deadline, the buyer is never told, and nobody else is contacted. A quiet vendor leaves the request in a silent dead end. Migration 0014's comment ("expiry is now real") overstates this. |
| D2 | "Policy rejections (24h window) are not retried and go to the operator" | True only for **synchronous** API errors. Meta documents that errors also arrive **asynchronously** in webhooks. Those only stamp `messages.failed_at` and write `MESSAGE_FAILED`: the vendor request stays SENT, offers stay "delivered", and nothing is recovered or raised. The fake Graph API only simulates the synchronous case. |
| D3 | Failure classification | A text match with three classes. Meta returns most errors with HTTP 400, so throttling (130429, 131056, 80007, 4) was classified **permanent** and never retried. 131026 (undeliverable / not a WhatsApp number) was classified **policy**. Auth errors (401 / 190) were reported as `vendor_unreachable`. There was no "unknown" class: every unrecognised 4xx was "permanent". |
| D4 | Report: "Automatic recovery 1/2 — the other was a policy rejection" | Wrong. The denominator counts `retry_scheduled` decisions, which only exist for transient failures. The policy rejection never enters it. The "other" one was the transient failure whose retries ran out. |
| D5 | "Clarification resolution 14/19" | The numerator is clarification **rows** with status ANSWERED. The denominator is rows not CANCELLED, which includes OPEN, SUPERSEDED, EXPIRED and EXHAUSTED. It measures *questions answered*, not *ambiguity resolved*. Re-asks (`attempts`) are folded into one row, so repeated questions are invisible. Workflow, question and message levels are not separated. |
| D6 | "Clear at confidence ≥ 0.45" | With the default heuristic extractor, confidence 0.45 is exactly the weight given to "a product word was found", so the threshold is equivalent to "product present". It is not a calibrated probability. |
| D7 | Request understanding | The extractor matches product words as **substrings**: "fan" in "fancy", "bag" in "baggage", "table" in "vegetable", "watch" in "watching", "bed" in "bedroom". A message naming two products silently keeps the first. "hello", "ok" and "thanks" each create a BuyerRequest (2 of the 17 synthetic workflows were greetings). |
| D8 | Operator can pause a workflow | Only globally, through env flags (restart required). There is no per-request pause or resume. |
| D9 | Operator view | Decisions, questions and exceptions per request exist. The request's messages and their delivery state (sent / delivered / failed / blocked) are not shown. |

## Known defects (before this work)

1. A quiet vendor is a silent dead end for the buyer (D1).
2. Provider-reported delivery failures leave business state wrong (D2).
3. Throttling is never retried, and wrong numbers are mislabelled (D3).
4. Junk requests are created from greetings and acknowledgements, and products are matched as substrings (D7).
5. A process crash between creating a vendor request and sending it leaves a PENDING request forever. `decideRouting` sees it as "already routed".
6. Phone numbers (`msg.From`) are written unmasked to error logs.

## Missing tests (before this work)

- Vendor-request timeout (all 15 required cases).
- Asynchronous delivery failure.
- Throttling and invalid-recipient classification.
- Notice re-delivery.
- Pause and resume.
- A realistic buyer message corpus.

## Decisions requiring the owner

These are listed in the hardening report (§J), each with its safe default.
