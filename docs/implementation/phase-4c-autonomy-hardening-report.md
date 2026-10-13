# Phase 4C autonomy hardening: report

**Where:** Claude's cloud sandbox only (`/home/claude/mm/middleman`, branch `phase-4a.1-media-ingestion`).
**Nothing has been applied to the owner's local repository.**
**Ancestry:**

- Base: `6f11ab1` (Phase 4B).
- Autonomy correction: `3aae7c8`.
- Hardening: the commits after `3aae7c8` (exact hashes in the transfer package, `ANCESTRY.txt`).

**Test conditions:**

- WhatsApp was the fake Graph API (`scripts/e2e/fake_graph_api.py`).
- No live WhatsApp call was made, no real buyer or vendor was contacted, and no payment was made.
- The Neon database was not used.
- Every number below is from synthetic test runs, not production.
- **No feature here is production-ready on this evidence alone.**

---

## A. Outcome

### Inspected

- The full autonomy implementation at `3aae7c8`.
- Migrations 0001–0014.
- The WhatsApp adapter and webhook.
- The durable jobs, metrics SQL and configuration.
- The tests and documentation.

The baseline is in `docs/architecture/autonomy-hardening-baseline.md`. All suites were green before any change.

### Discrepancies found against the previous report

| # | Previous claim | What the code actually did |
|---|---|---|
| D1 | Vendor requests expire after 72 hours. | Expiry was **lazy**: a request expired only when the *same* vendor messaged again. Nothing ran at the deadline, and the buyer was never told. |
| D2 | Policy rejections go to the operator. | Only for **synchronous** errors. Failures that Meta reports by webhook only stamped `failed_at`: vendor requests stayed SENT and offers stayed "delivered". |
| D3 | Failures are classified. | Throttling (HTTP 400 + 130429/131056) was "permanent" and never retried. 131026 was treated as "policy". Auth errors were reported as `vendor_unreachable`. There was no "unknown" class. |
| D4 | "Automatic recovery 1/2, the other was a policy rejection." | Wrong. The other one was the transient failure whose retries ran out; policy refusals are never in that denominator. |
| D5 | "Clarification resolution 14/19." | It counted *questions answered*, over rows that included open, superseded and expired questions. It did not measure *ambiguity resolved*. |
| D6 | "Clear at confidence ≥ 0.45." | With the heuristic extractor, 0.45 is exactly the weight of "a product word was found". |
| D7 | Request understanding. | Products were matched as substrings ("fan" in "fancy"). A second product was dropped. "hello" and "thanks" created BuyerRequests. "iPhone 13 and a laptop" was read as **13 laptops** (quantity bug). |
| D8 | The operator can pause a workflow. | Only globally, through env flags. |

### Changed

These are described in detail in §§B–F:

- Vendor-request deadlines and request recovery.
- One failure taxonomy, applied to both synchronous and webhook failures.
- Buyer updates kept for a compliant later route.
- Request-understanding rules plus a measured corpus.
- Layered metrics.
- Per-request pause and resume.
- An operator message trail.
- Phone numbers masked in error logs.

**Commits:** three code/doc commits plus this report.
**Size:** 55 files changed, about 8.6k lines; ~1.6k of those are the corpus JSON.

### Left untouched

- The commerce loop and the canonical models.
- The offer-selection rule: an explicit number, or a strong "yes" to a single confirm question.
- Discovery and distribution (batch sending is still operator-triggered).
- Payments, fulfilment, the pilot, Phase 5 and WA Shopa.
- The infrastructure: no new components; Postgres jobs only.
- Pre-existing `gofmt` deviations in untouched files. They were deliberately not reformatted.

---

## B. Verified behaviour (sandbox, fake provider)

| Capability | Status | Tests | Result | Remaining limitation |
|---|---|---|---|---|
| Request confirmation | Implemented, verified | autonomy E2E A–H; corpus | pass | Heuristic interpreter; misspellings are asked about, not read |
| Context-aware clarification | Implemented, verified | E2E B, H, M, Q; `understanding_test.go`; corpus answers 14/14 | pass | — |
| Database-aware resolution | Implemented, verified | E2E C; `FirstReading` | pass | — |
| Automatic routing | Implemented, verified | E2E A, I | pass | — |
| Vendor-response association, including late replies | Implemented, verified | E2E E, I (late unquoted reply) | pass | A late *unnamed* reply is not attached (by design) |
| Vendor clarification | Implemented, verified | E2E E | pass | — |
| Offer creation and delivery | Implemented, verified | E2E A, D, I | pass | — |
| **Vendor-request deadline and recovery** | **New**, verified | 18 integration tests; E2E I, J, P | pass | Live timing is unverified (E2E uses a 12 s TTL) |
| **Delivery failure handling** | **New**, verified against the fake provider | 5 classifier unit tests (~35 cases); 9 integration tests; E2E K, L; autonomy G | pass | **Requires live-Meta verification**: real codes, and sync vs. webhook timing |
| Discovery → commerce | Implemented, verified | E2E D; `disc_commerce` | pass | — |
| Operator as exception-only | Implemented, verified | Metrics: 0 operator steps; E2E P (pause/resume, 401/400) | pass | Read endpoints are not authenticated (decision J-3) |

---

## C. Vendor expiry and recovery

1. **Scheduling the deadline.** When a vendor request is sent, a durable job `VENDOR_REQUEST_TIMEOUT` is scheduled at `sent_at + VENDOR_REQUEST_TTL` (default 72h).
   - On startup, a sweep schedules missing deadlines and recovers overdue or undelivered requests.
   - A request that has been PENDING for more than 10 minutes was never sent; it becomes SEND_FAILED.
2. **The job is idempotent and race-safe.**
   - The conditional update `SENT → EXPIRED` has exactly one winner.
   - A reply that arrived first wins; it is recorded as `already_responded`.
   - CANCELLED is left alone.
   - If the deadline has moved (the request was re-sent), the job is re-scheduled.
3. **Recovery** runs under the request's workflow lock. It is also triggered when a vendor is unreachable, when retries run out, or when a vendor declines. It checks, in order:
   1. The request is on hold.
   2. The request is no longer CONFIRMED.
   3. The request is fulfilled.
   4. The buyer already has a usable reply or offer.
   5. Other vendors are still awaited, or a reply is still being clarified.
   6. A newer request for the same product exists.
   7. **The same state was already handled.** This is recorded as `duplicate_recovery_prevented`.
   8. Recovery is switched off (recorded only).
   9. Rounds, contact cap or routing are switched off; the buyer is told.
   10. Otherwise: **re-run matching without the vendors already asked**, through the canonical routing policy. That policy covers score, listing eligibility (lifecycle, freshness, expansion stage), busy vendors and the remaining cap.
       - If alternatives exist, they are contacted and the buyer is told.
       - If only out-of-area vendors fit, the buyer is asked (never after a "no", never for excluded categories), and on "yes" those vendors are contacted.
4. **No alternative.** The buyer is told:

   > I haven't got a confirmed option for X yet 😔 The seller(s) I contacted haven't replied with one. I'll pass on any reply that still comes in. If you'd like, change the budget, condition or area…

   The item is never called "unavailable". The request stays CONFIRMED, and a late reply still becomes an offer. If that message is blocked by the 24h window, it is kept and sent when the buyer writes. If progress overtakes it, it is cancelled.
5. **Caps:**

   | Parameter | Default | Meaning |
   |---|---|---|
   | `AUTO_ROUTE_MAX_VENDORS` | 3 | Vendors per round |
   | `AUTO_RECOVERY_MAX_ROUNDS` | 2 | Recovery rounds |
   | `AUTO_ROUTE_MAX_VENDORS_PER_REQUEST` | 6 | Vendors in total |

   The unique (vendor, request) pair is unchanged.
6. **Audit trail:**
   - Decisions: `VENDOR_REQUEST_TIMEOUT`, `RECOVER_REQUEST`.
   - Events: `VENDOR_REQUEST_EXPIRED` (with `detected_by`), `REQUEST_RECOVERY_DECIDED`.
   - A recovery step that keeps failing is retried by the job, then raises `request_recovery_failed`.

**Tests.** The 15 required cases are covered by `infrastructure/postgres/autonomy_hardening_integration_test.go` against real Postgres and the real repositories:

- reply just before the deadline;
- reply after the deadline;
- processed twice, and by four concurrent workers;
- cancelled;
- fulfilled;
- alternative exists;
- no alternative;
- broader allowed (already broadened, or asked);
- broader not allowed (excluded category; buyer declined);
- contact limit;
- request no longer valid (reopened; superseded);
- notification blocked;
- recovery step fails then raises;
- maximum rounds.

There are also tests for hold/resume, a request that was never sent, the startup sweep, recovery switched off, and a stale notice.

---

## D. Messaging failure handling

There is one classifier, `channel.Classify`, which reads Meta's error code. Codes were taken from Meta's official error-code page, read 13 Oct 2026. The full table is in `docs/architecture/whatsapp.md` → Delivery failures.

| Class | Retry | Then |
|---|---|---|
| TRANSIENT (network, 5xx, 131000) | Bounded; first after 5s, then backoff | Retries exhausted: vendor → operator exception plus recovery to other vendors; buyer update → kept for the next message |
| RATE_LIMITED (4, 80007, 130429, 131056, 429) | Bounded; 60s (131056: 5 min) | As above |
| PROVIDER_UNAVAILABLE (131016, 133004, 131057, 503) | Bounded; 60s | As above |
| INVALID_RECIPIENT (131026, 131021, 130403) | Never | Exception `invalid_recipient`; vendor → other vendors |
| INVALID_REQUEST (100, 1310xx parameter errors, 1320xx) | Never | Exception `invalid_outbound_message` |
| POLICY, window/recipient (131047, 131049, 131050, …) | Never as free text | Buyer update or offers kept until the buyer writes (no exception). Vendor: exception plus other vendors. `template_required` and `template_configured=false` are recorded. |
| POLICY, account (368, 131031, 131048, …) | Never | One channel exception |
| AUTH_OR_CONFIG (0, 3, 10, 190, 200–299, 132001, 401/403, …) | Never | One channel exception; buyer updates kept |
| UNKNOWN (unrecognised 4xx, or a webhook failure without a code) | Never | Exception `unclassified_provider_error` |

**Webhook-reported failures.** These are classified the same way.

- A vendor request moves to SEND_FAILED and the buyer's request is recovered.
- The offers it carried are returned to deliverable and kept for the buyer's next message.
- An undelivered question is closed (not counted as asked) and asked again when the buyer writes.
- A duplicate receipt is ignored.

**Templates.** No template is assumed. Majo has no configured buyer-update template, and `/api/autonomy/settings` says so.

---

## E. Request understanding

**The policy.** `workflow.Assess` is deterministic. The extractor only reports the product, fields, `other_products` and `ambiguities`. The outcome classes are `clear`, `missing_detail`, `ambiguous`, `conflicting`, `unanswerable` and `policy_exception`.

**Order of checks:**

1. A restricted item is held (SAFETY).
2. No product: ask.
3. Several products: ask which first, once. The others become their own requests.
4. A contradictory condition: ask.
5. Two places offered: ask.
6. Location: taken from context first.
7. A generic product: ask one detail. A model stated in the message is used.
8. Budget: asked only after a vague start.

**No junk requests.**

- Greetings get a prompt.
- Courtesy messages get nothing.
- A question about nothing shown gets "I haven't sent you any options yet".
- Affirmatives and ordinal words ("the first one") stay with the offer resolver. They never select by themselves.

**No loops.**

- One question per conversation.
- A field is asked at most twice.
- Answers merge into the same request.
- A different product named in an answer replaces the product; it does not merge into the model field.
- A long sentence is never stored as a model.

**The 0.45 threshold.** It is kept as a product-present floor. It is **not** calibrated and no calibration evidence exists; the decisions are made by the checks above. An LLM extractor's confidence would need calibration on real messages before it means anything more.

**Synthetic corpus.** 68 first messages and 14 follow-up answers, run through the real heuristic extractor (`scripts/eval/`). Results:

| Measure | Result |
|---|---|
| Correct outcome | 64 / 68 |
| Known gaps | 4 (misspellings and spelled-out numbers: Majo asks instead of reading them) |
| False confirmations | 0 |
| Junk requests | 0 |
| Restricted-item misses | 0 |
| Restricted-item false positives | 0 ("human hair wig" passes) |
| Unnecessary questions | 4 (the same 4 known gaps) |
| Answers parsed correctly | 14 / 14 |
| Long sentences stored as a field | 0 |

**Extractor fixes made along the way:**

- Products matched as whole words.
- Brand + product read as one product ("samsung tv").
- `X and Y k` read as a budget range.
- A model number is not a quantity.
- Common household products and Nigerian phone brands added to the vocabulary.

---

## F. Metrics (`GET /api/autonomy/metrics`)

Definitions are served with the numbers and documented in `docs/analytics/funnel.md`.

| Group | Denominator |
|---|---|
| `workflow.*` | Buyer requests created |
| `questions.*` | Questions delivered (`messages_sent` is message-level) |
| `recovery.*` | Its own denominators: retries scheduled; re-routed requests |
| `delivery.*` | Counts |

All values come from persisted rows. The legacy fields are kept for compatibility and their meanings corrected (D4, D5).

**Synthetic run of the hardening E2E** (9 workflows):

| Metric | Value |
|---|---|
| Completed | 1 / 9 |
| Needed a question | 4 / 9 |
| Escalated to operator | 2 / 9 (a wrong vendor number; a restricted item) |
| Operator performed a step | 0 / 9 |
| Ended without an offer | 3 / 9 |
| Pending | 5 / 9 |
| Questions answered | 3 / 4 |
| Questions resolved | 3 / 4 |
| Questions re-asked | 0 / 4 |

**Synthetic run of the autonomy E2E:** 15 workflows. This was 17 before: the 2 greetings no longer create requests. Manual routing 0/18, manual confirmation 0/15. The legacy "questions answered" figure is still 14/19.

---

## G. Test results (exit codes captured; details in `TEST_EVIDENCE.md`)

| Suite | Before (`3aae7c8`) | After |
|---|---|---|
| `go vet ./...` | 0, clean | 0, clean |
| Go unit + Postgres integration (`go test -count=1 -json ./...`) | 0: 409 pass (325 top-level), 0 fail, 0 skip | 0: 460 pass (365 top-level), 0 fail, 0 skip |
| Go race (`go test -race ./...`) | 0, 22 packages | 0, 23 packages |
| pytest | 0, 36 passed | 0, 48 passed |
| `tsc --noEmit` / `next build` | 0 / 0 | 0 / 0 |
| E2E (`scripts/e2e/run_regression.sh`) | 14 suites, all pass | 15 suites, all pass |

**After-change E2E detail:**

| Suite | Checks |
|---|---|
| phase31 | 76 |
| media | 14 + 26 |
| grouping | 36 |
| listing | 28 |
| curation | 52 |
| matching | 28 |
| lifecycle | 44 |
| disc_commerce | 74 |
| disc_geo | 66 |
| disc_stale | 27 |
| disc_expired | 47 |
| disc_dashboard | 17 |
| autonomy | 147 |
| **hardening (new)** | **88** |

**Expectations changed (none weakened):**

1. `SpecInText` now keeps every model suffix: "15 pro max", not "15 pro".
2. The E2E runner passes `AUTO_RETRY_FIRST_DELAY=2s` to the autonomy suite, because class delays are now 60s for 503s.
3. Legacy suites run with the new flag `AUTO_RECOVER_STALLED_REQUESTS=false` as well.

**NOT RUN:** live WhatsApp, the Anthropic LLM extractor path, and load tests.

---

## H. Documentation updated

**Repository:**

- `docs/architecture/autonomous-runtime.md`: the 3 new lock statements, §6, and new §§11–13; known limits.
- `docs/architecture/whatsapp.md`: delivery failures and the 24h window.
- `docs/analytics/funnel.md`: the metric correction and layered metrics.
- `docs/architecture/supply-lifecycle.md`: §12.
- `docs/architecture/discovery-distribution.md`: §13.
- `docs/architecture/autonomy-hardening-baseline.md` (new).
- `.env.example`.
- This report.
- `docs/implementation/LOCAL_REPOSITORY_TRANSFER_GUIDE.md`.

**Project:** `architecture/architecture-lock-v2.md` (amended) and `implementation/phase-4c-autonomy-hardening-report.md`.

---

## I. Transfer readiness

The transfer package is `majo-4c-hardening-transfer.zip`. It contains:

- patches (per commit and cumulative);
- Git bundles (incremental and full branch);
- `MANIFEST.json` (changed files and their purpose);
- `TEST_EVIDENCE.md`, `CONFIG_AND_MIGRATIONS.md` and `ANCESTRY.txt`;
- `SHA256SUMS`;
- the transfer guide.

Applicability was tested independently (results in `ANCESTRY.txt`): on a clean worktree of `6f11ab1`, on a clean worktree of `3aae7c8`, and on a non-git copy rebuilt from the earlier `majo-repo-6f11ab1.zip`.

---

## J. Outstanding decisions (owner)

1. **Restricted-items list** (`AUTO_RESTRICTED_ITEM_TERMS`).
   - Safe default: the shipped list (weapons, hard drugs incl. codeine/tramadol, forged documents, explosives); matches are held for review.
   - Consequence: false holds delay legitimate buyers; misses route restricted demand automatically.
2. **Approved utility templates** (buyer updates, vendor requests).
   - Safe default: none. Updates wait for the buyer's next message; vendors outside their window are skipped.
   - Consequence without templates: the buyer may learn of "no reply yet" late, and quiet vendors are lost.
3. **Authenticate dashboard read endpoints** (`GET /api/requests`, `/vendors`, `/vendor-responses`, `/offers`, `/transactions`, `/metrics/funnel`).
   - Today: unauthenticated (pre-existing). They expose buyer messages and phone-derived data if the API is reachable.
   - Safe default if the API is ever public: require the operator token, with a dashboard change.
   - Not changed here, because it affects every dashboard page.
4. **Out-of-area searching per category** (`AUTO_BROADEN_EXCLUDED_CATEGORIES`).
   - Default: none; the buyer decides.
   - Consequence: buyers may agree to a seller in a far city for bulky goods.
5. **Recovery caps:** 2 rounds, 6 vendors per request, 72h deadline.
   - These are hypotheses. Higher values contact more vendors (vendor fatigue); lower values tell the buyer sooner.
6. **"No reply yet" wording and timing.**
   - Today: sent once, when all contacted vendors have gone quiet and nobody else can be contacted.

---

## K. Next session — handoff checklist

1. **Wait** until the owner connects the local repository. Do not assume it.
2. Follow `docs/implementation/LOCAL_REPOSITORY_TRANSFER_GUIDE.md` step by step. **Inspect before editing.**
3. Verify `SHA256SUMS`. Then decide: bundle (shared history) or patch (no shared history).
4. Run `--check` / dry-run first. **Stop on any conflict or unexplained divergence**, and report it.
5. Apply on a new branch. Never reset, force-push or discard local changes.
6. Run migrations and tests **only** against a local disposable Postgres. **Never run them against Neon.** Never print or commit `.env`.
7. Report exactly what was applied (commits/files), what was not, and the test results.

## Assumptions

- The owner's local repository may be at `6f11ab1`, at `3aae7c8`, or a copy without shared history. All three are handled.
- 72h / 2 rounds / 6 vendors / 24h question TTL are starting hypotheses.
- Meta error codes are as published on 13 Oct 2026. Whether a code arrives synchronously or by webhook is not assumed: both paths are handled.
- No buyer-update template exists or is approved.
- A greeting deserves a prompt, and a courtesy message deserves no reply.
- When several products are named, the first named is offered first, and "both" means "start with the first".
