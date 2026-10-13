# Autonomous runtime (Phase 4C correction)

> **Majo is autonomous by default. Human operators are control-plane
> participants and exception handlers, not normal transaction processors.**
>
> **Ambiguity is resolved through contextual interpretation and participant
> clarification before operator escalation.**
>
> **Operator intervention is an exception path, not a normal workflow state.**
>
> **MAJO is autonomous by default, observable by admin, interruptible by admin.**
>
> **Humans operate the control plane, not individual transactions.**
>
> **The operator is an exception handler, not the hidden state machine.**

These statements are part of the Architecture Lock (the last three were
added by the Phase 4C hardening pass). They supersede the
earlier assumptions that every BuyerRequest needs operator confirmation and
that vendor routing is "never called automatically". The correct rule is:
*automatically routed when deterministic eligibility and routing policy permit;
operator intervention remains available for exceptions.*

Baseline audit (every operator dependency before this change):
`autonomy-audit.md`.

## 1. Normal path (no human gate)

```
Buyer → inbound message → context resolution → interpretation
  → completeness / confidence / policy → [clarification if necessary]
  → BuyerRequest → SupplyEligibility / Matching → Routing Policy → VendorRequest
  → vendor conversation → context resolution → vendor reply interpretation
  → [clarification if necessary] → Offer → buyer presentation → buyer selection → Transaction
```

Discovery: Distribution → buyer interaction (tap or typed reference) →
BuyerRequest(origin=discovery) → automatic confirmation → the listing's vendor
→ VendorRequest → reply → Offer → selection → Transaction.

## 2. AI interprets; policy decides; services mutate

```
message → context builder → interpreter → typed proposal
        → deterministic policy validation → application service → DB → event → outbound
```

* **Context builder** (`postgres.WorkflowRepository.BuyerContext`): targeted
  queries — offers presented in this conversation, listings in discovery
  deliveries sent to this buyer (and the one last tapped), earlier requests,
  the buyer's known location. Never a table dump.
* **Interpreter**: deterministic rules (`workflow.ParseReference`,
  `ResolveReference`, `Assess`, `SpecInText`, `routing.MatchVendorOption`,
  `pickByContext`) plus the existing AI extraction service, which only
  proposes field values.
* **Typed proposals** (decision log kinds): `CONFIRM_REQUEST`,
  `ASK_CLARIFICATION`, `CONTINUE_REQUEST`, `ROUTE_REQUEST`, `NO_MATCH`,
  `REFERENCE_ITEM`, `ANSWER_FROM_CONTEXT`, `ASSOCIATE_VENDOR_RESPONSE`,
  `INTERPRET_VENDOR_RESPONSE`, `ASK_VENDOR_CLARIFICATION`, `RETRY_DELIVERY`,
  `AUTO_PUBLISH_LISTING`.
* **Policy**: `workflow.Assess` (confirmation), `workflow.RoutingPolicy`
  (routing), the canonical `supply.EligibilityPolicy`, `EnsureConfirmed`.
* **Services**: `request.Service` (Create, ConfirmAs, ApplyClarification,
  ClarifyAs), `routing.Service` (RouteAs, ResendVendorRequest), offer delivery,
  `transaction.Service`, `distribution.Service.EnquireFromContext`.

Every decision is appended to `workflow_decisions` (outcome, fallback class,
reason, confidence, inputs, result) and shown on the request page.

## 3. Buyer side

* **Clear request** (product known; location known or known from context —
  the consent record or an earlier request; for generic products like "tv" or
  "phone" one detail) → confirmed by SYSTEM → routed.
* **Missing information**: Majo asks the *one* question the next action needs
  (product → location → model/size for generic products → budget only when the
  request started vague). Never more than `AUTO_CLARIFY_MAX_QUESTIONS`. A
  stated model ("Samsung A54", "55 inch") is used, not asked again.
* **The answer continues the same request** (`ApplyClarification`, event
  `REQUEST_CLARIFIED`). "any / doesn't matter" skips the field. A clearly new
  request supersedes the question.
* **One open question per conversation.** A request whose question cannot be
  asked yet waits (`another_question_open`) and continues when the slot frees,
  so an answer can never be merged into the wrong request. An interrupted
  question is asked again.
* **References to what was shown**: "how much is the black one?" → answer from
  the listing/offer; two matches → "Do you mean the ₦120k one or the ₦145k
  one?"; "I want that one" → listing enquiry, or "Do you want option 2? Reply 2
  or YES" for an offer (selection needs an explicit yes, valid 2 hours);
  "something cheaper" → a new request below that price. Every identifying word
  must describe the item, otherwise the message is an ordinary new request.
* **No match**: an honest reply, or — when only sellers outside the buyer's
  area fit and the canonical geographic policy does not already reach the buyer
  — "I can check sellers in other areas too. Want me to?". No-match is not an
  operator event.
* Buyer answers are processed as durable jobs (`BUYER_ANSWER`), serialised per
  conversation; a message that arrives after its question was answered is not
  dropped — it takes the reference or ordinary path.

## 4. Routing policy

Discovery enquiry → the listing's own vendor (if ACTIVE). Typed demand →
matching candidates, then: score ≥ `AUTO_ROUTE_MIN_SCORE`; listing evidence
eligible under the canonical policy; vendor not already holding
`AUTO_ROUTE_VENDOR_MAX_OPEN` open requests; candidates priced over the budget
last; at most `AUTO_ROUTE_MAX_VENDORS`; duplicates impossible (unique pair).
Vendors outside the buyer's area are held back unless their listing is already
reachable at its expansion stage. `VENDORS_ROUTED` records actor SYSTEM (or
OPERATOR for an override) so manual routing is measurable.

## 5. Vendor side — association precedence

1. structured: the quoted WhatsApp message (`context.id`) or an MM- reference
2. the vendor's open clarification question
3. the single unexpired open request
4. the reply names exactly one open request (product, variant, listing ref)
5. ask the vendor: "Which request is that for? 1. … 2. … Reply with the number."
6. genuine exception — only if the answer stays ambiguous

Guards: a reply naming a product none of the open requests is about (with a
price) is **supply**, never a reply; an answer to a detail question about a
different product or another open request is not taken as that answer; a full
reply quoting a request while "which request?" is open answers afresh;
superseding an open "which request?" with a reply still waiting raises an
exception (nothing orphaned). Requests older than `VENDOR_REQUEST_TTL` expire
(event `VENDOR_REQUEST_EXPIRED`) instead of keeping the vendor ambiguous forever.

Unclear reply ("Which one?", a bare "620k", "maybe") → a targeted question:
"The buyer is asking for samsung A54. Do you have that exact item? Reply YES
with your price, or NO." / "Is the iphone 13 available at ₦620,000? Reply YES
or NO." A "yes" carries the price asked about.

## 6. Recovery

Rejected sends are classified by one taxonomy (`channel.Classify`, from Meta's
structured error code; see `whatsapp.md` → Delivery failures): transient /
rate-limited / provider-unavailable → durable bounded retry jobs
(`VENDOR_REQUEST_RESEND`, `OFFER_DELIVERY_RETRY`, `BUYER_NOTICE_RESEND`);
everything else is never retried as-is. A vendor that cannot be reached
(policy, wrong number, retries exhausted) does not stall the buyer: the
request is recovered (§11). Buyer updates and offers blocked by the 24h
window are kept and delivered when the buyer next writes. Provider-reported
(webhook) failures are handled the same way (before the hardening pass they
only stamped `failed_at`). Workflow steps run as durable
`WORKFLOW_ADVANCE` jobs; a step that keeps failing raises an exception; on
startup, requests left mid-flow continue (`Engine.Recover`). Locks are
`pg_try_advisory_lock` with backoff and a 30 s bound, so a burst cannot pin the
connection pool.

## 7. Fallback taxonomy

| Class | Who resolves |
|---|---|
| CLARIFICATION_REQUIRED | Majo asks the participant |
| RETRY_REQUIRED / TEMPORARY_PROVIDER_FAILURE | Majo retries |
| POLICY_REJECTION | deterministic refusal (logged; flag off, not publishable…) |
| GENUINE_AMBIGUITY | Majo stops asking and tells the participant |
| SAFETY_EXCEPTION | operator (e.g. a vendor reply that stayed ambiguous) |
| SYSTEM_EXCEPTION | operator (delivery exhausted, provider policy, workflow failure) |

Only the last two land in `workflow_exceptions`, the operator's queue
(`/autonomy`). Resolving one hands the request back to Majo.

## 8. Control plane

Operators can observe (decision log, questions, exceptions per request),
override any step (confirm, ask, correct, route, associate, deliver, select —
all labelled *Override* and behind the operator token, as are vendor create and
activate), pause a path (flags), and resolve exceptions. They are not required
for a healthy request. Metrics: `docs/analytics/funnel.md` → Autonomy.

## 9. Flags ([PARAM], env)

`AUTO_CONFIRM_CLEAR_REQUESTS`, `AUTO_ROUTE_DISCOVERY_REQUESTS`,
`AUTO_ROUTE_MATCHED_REQUESTS`, `AUTO_RESOLVE_VENDOR_CONTEXT`,
`AUTO_CLARIFY_REQUESTS`, `AUTO_CLARIFY_VENDOR_RESPONSES` (requires
`AUTO_RESOLVE_VENDOR_CONTEXT`), `AUTO_RETRY_DELIVERY` — default **ON**;
`AUTO_PUBLISH_TRUSTED_VENDOR_LISTINGS` — default **OFF** (Lock gate: ≥ 95% of a
vendor's last 20 operator-curated drafts approved unchanged). OFF disables a
path; the implementation stays and is tested (earlier-phase E2E suites run
with every flag off).

## 11. Vendor-request deadlines and request recovery (Phase 4C hardening)

Before this pass expiry was **lazy**: a SENT request became EXPIRED only when
the same vendor sent another message; nothing ran at the deadline and the
buyer was never told. Now:

1. **Deadline.** When a vendor request is sent, a durable
   `VENDOR_REQUEST_TIMEOUT` job is scheduled for `sent_at + VENDOR_REQUEST_TTL`
   (re-scheduled if it was re-sent). On startup a sweep schedules any missing
   deadline (requests sent before this version, or jobs lost) and the recovery
   of overdue / undelivered requests. A PENDING request older than 10 minutes
   (the process died between creating and sending it) becomes SEND_FAILED.
2. **Expiry** is a conditional update (`SENT → EXPIRED` only if still SENT and
   past the deadline): two workers, a retried job or a racing vendor reply —
   exactly one wins; a reply that arrived first wins and is recorded as
   `already_responded`. CANCELLED is left alone; EXPIRED stays distinct from
   RESPONDED / CANCELLED.
3. **Recovery** (`RecoverRequest`, under the request's workflow lock, also
   triggered when a vendor could not be reached or declined), in order:
   on hold → deferred; request not CONFIRMED → nothing; selection /
   transaction → nothing; a usable reply or an active offer → nothing; another
   vendor still awaited (incl. a send retry pending) or a reply still being
   clarified → wait; superseded by a newer confirmed request for the same
   product → nothing; **this state already handled** (same number of contacted
   vendors) → `duplicate_recovery_prevented`; `AUTO_RECOVER_STALLED_REQUESTS=false`
   → recorded only; `AUTO_ROUTE_MATCHED_REQUESTS=false`,
   `AUTO_RECOVERY_MAX_ROUNDS` reached or `AUTO_ROUTE_MAX_VENDORS_PER_REQUEST`
   reached → tell the buyer. Otherwise matching runs again **minus every vendor
   already contacted**, through the canonical routing policy (score floor,
   listing eligibility incl. lifecycle / freshness / geographic expansion
   stage, busy vendors, fan-out ≤ remaining cap) → contact the alternatives and
   tell the buyer. If only vendors outside the buyer's area fit (and the
   expansion policy does not already reach the buyer) → ask the buyer
   (one-question slot permitting; not for `AUTO_BROADEN_EXCLUDED_CATEGORIES`;
   never after a "no"); on "yes" those vendors are contacted (still minus those
   already asked, within the cap). Else → **tell the buyer the real status**:
   "I haven't got a confirmed option for X yet … I'll pass on any reply that
   still comes in … change the budget, condition or area …". Never
   "unavailable"; the request stays CONFIRMED.
4. **Late replies** still count: a quoted / MM-reference reply attaches to the
   expired request (as before); an unquoted reply that clearly names one
   request expired within `AUTO_LATE_REPLY_WINDOW` (buyer request still open)
   is associated as `late_reply_after_expiry` and becomes an offer.
5. Every step is in the decision log (`VENDOR_REQUEST_TIMEOUT`,
   `RECOVER_REQUEST`) and as events (`VENDOR_REQUEST_EXPIRED` with
   `detected_by`, `REQUEST_RECOVERY_DECIDED`). A recovery step that keeps
   failing (matching / routing errors) is retried by the job, then raises
   `request_recovery_failed`; nothing is sent twice.

## 12. Request understanding (Phase 4C hardening)

`Assess` is the deterministic policy; the extractor only reports.

* **The 0.45 floor is not the policy.** With the heuristic extractor 0.45 is
  exactly the weight of "a product word was found", so
  `AUTO_CONFIRM_MIN_CONFIDENCE` acts as a product-present check. No calibration
  data exists; it is kept as a floor, and the decisions below are made by
  deterministic checks. (An LLM extractor's confidence would need its own
  calibration before the floor means anything else.)
* **Order:** restricted item → held (SAFETY, never confirmed); no product →
  ask; several products → "which first?" (asked once; the others become their
  own requests — one BuyerRequest per product, never dropped, never
  duplicated); contradictory condition → ask; two places offered → ask;
  location (from context first); generic product → one detail (a model stated
  in the message is used); budget only after a vague start; otherwise clear.
* **Outcomes** (`Assessment.Outcome`, recorded as `understanding` on the
  decision): `clear`, `missing_detail`, `ambiguous`, `conflicting`,
  `unanswerable`, `policy_exception`.
* **No junk requests:** a greeting gets a prompt, a courtesy message gets
  nothing, a question about something never shown gets "I haven't sent you any
  options yet" — none creates a BuyerRequest. Affirmatives and ordinal words
  ("ok", "that one", "the first one") stay with the offer resolver: they never
  select by themselves (the explicit selection rule is unchanged).
* **No loops / no duplicates:** one open question per conversation; a field
  is asked at most twice, then Majo proceeds or stops; an answer naming a
  different product changes the product of the same request; a long sentence
  is never stored as a model.
* Measured on a synthetic corpus (`scripts/eval/`, test
  `TestRequestUnderstandingCorpus`): see the hardening report.

## 13. Operator holds (Phase 4C hardening)

`POST /api/requests/{id}/pause|resume {"reason"}` (operator token, reason
required, audited as `WORKFLOW_HELD` / `WORKFLOW_RESUMED` and in the decision
log). While held Majo takes no automatic step for the request (advance,
recovery, automatic offer delivery, redelivery, notices); buyer messages are
still recorded and operator overrides still work. Resume re-advances the
request, re-runs recovery and delivers deliverable offers. The request view
(`GET /api/requests/{id}/autonomy`) now also returns the message trail
(sent / delivered / read / failed), holds and pending notices.

## 10. Known limits

* The interpreter is deterministic plus the heuristic extractor (an LLM
  extractor is used only if configured); vague phrasing it cannot read becomes
  a question, not a guess.
* ~~Vendor requests that expire unanswered do not yet tell the buyer.~~ Fixed by
  the hardening pass (§11).
* No approved WhatsApp template is configured: updates outside the 24h window
  wait for the buyer's next message; vendors outside their window cannot be
  reached at all (recovery moves on to other vendors).
* The interpreter misses misspellings and spelled-out numbers (it asks; it
  never guesses). Restricted-items matching is literal whole words.
* Fulfilment and payment after the transaction are unchanged (UNPAID).
* Discovery deliveries are not auto-retried (operator-triggered by design).
* Tested against the fake Graph API, not live WhatsApp.
