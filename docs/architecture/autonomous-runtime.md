# Autonomous runtime (Phase 4C correction)

> **Majo is autonomous by default. Human operators are control-plane
> participants and exception handlers, not normal transaction processors.**
>
> **Ambiguity is resolved through contextual interpretation and participant
> clarification before operator escalation.**
>
> **Operator intervention is an exception path, not a normal workflow state.**

These three statements are part of the Architecture Lock. They supersede the
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

Rejected sends are classified (policy / transient / permanent). Transient →
durable retry jobs (`VENDOR_REQUEST_RESEND`, `OFFER_DELIVERY_RETRY`) with
backoff up to `AUTO_RETRY_MAX_ATTEMPTS`; policy (24h window closed) → exception
immediately; exhausted → exception. Workflow steps run as durable
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

## 10. Known limits

* The interpreter is deterministic plus the heuristic extractor (an LLM
  extractor is used only if configured); vague phrasing it cannot read becomes
  a question, not a guess.
* Vendor requests that expire unanswered do not yet tell the buyer.
* Fulfilment and payment after the transaction are unchanged (UNPAID).
* Discovery deliveries are not auto-retried (operator-triggered by design).
* Tested against the fake Graph API, not live WhatsApp.
