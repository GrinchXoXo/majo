# WhatsApp Integration Notes

Majo uses the **Meta WhatsApp Cloud API directly** (no BSP) per
Middleman_Technical_Spec_v1 §1.

## What you need from Meta Business Manager

1. **Meta Business Account** — verified business, owned by Majo
   (not an individual's personal account).
2. **WhatsApp Business Account (WABA)** with a dedicated phone number.
3. **App** created in Meta for Developers, with the WhatsApp product added.
4. From the app dashboard, you need:
   - `WHATSAPP_ACCESS_TOKEN` — a permanent access token (generate via a
     System User in Business Settings, not a temporary token).
   - `WHATSAPP_PHONE_NUMBER_ID` — found under WhatsApp > API Setup.
   - `WHATSAPP_VERIFY_TOKEN` — a string you choose yourself; Meta echoes
     it back during webhook verification to prove ownership.
   - `WHATSAPP_APP_SECRET` — App Dashboard > App settings > Basic > App
     Secret. Meta signs every webhook POST with it (`X-Hub-Signature-256`);
     the backend rejects any POST whose signature doesn't verify (401).
     **Required unless `APP_ENV=development`** — without it the backend
     refuses to start, because anyone who can reach the webhook URL could
     otherwise impersonate buyers and vendors.

## Webhook configuration

Point Meta's webhook at:

```
POST https://<your-domain>/webhooks/whatsapp
```

During setup, Meta calls this URL with `GET` and query params
`hub.mode`, `hub.verify_token`, `hub.challenge` — the backend's
`WhatsAppWebhookHandler.handleVerification` responds to this automatically
as long as `WHATSAPP_VERIFY_TOKEN` matches what you entered in Meta's
dashboard.

Subscribe to the `messages` webhook field so incoming buyer/vendor
messages are delivered as `POST` requests.

## The 24-hour customer-service window

Free-form text can only be sent to a user within 24 hours of their last
message to the business number. Outside that window Meta rejects the send
(error 131047, "Re-engagement message") and an approved **template** is
required. Majo does not send templates yet. Consequences today:

- A vendor who hasn't messaged Majo in the last 24h cannot receive a
  VendorRequest. The request is recorded as `SEND_FAILED` (with Meta's error)
  and is never treated as awaiting a reply.
- An offer delivered more than 24h after the buyer's last message fails the
  same way; the offer stays deliverable and (Phase 4C hardening) is delivered
  automatically when the buyer next writes (`pending_notices`, kind
  `offer_delivery`). The operator's "Deliver pending offers" still works.
- (Phase 4C hardening) Majo's own buyer updates refused for the same reason
  are kept and sent when the buyer next writes, unless they are older than
  `AUTO_PENDING_NOTICE_TTL` or were overtaken by progress (cancelled).

## Delivery failures (Phase 4C hardening)

One classifier, `internal/channel/failure.go` (`channel.Classify`), reads
Meta's **structured error code** (the adapter returns a
`channel.ProviderError`; Meta answers most errors with HTTP 400, so the HTTP
status alone is not enough). Meta documents that errors arrive synchronously,
asynchronously (webhooks) or both, so the same classes apply to status-webhook
failures (`ClassifyCode(code, 0)`). Source: Meta, *WhatsApp Cloud API error
codes* (developers.facebook.com/documentation/business-messaging/whatsapp/support/error-codes),
read 13 Oct 2026. **Exercised only against the fake Graph API** — not
verified against live WhatsApp.

| Class | Codes (examples) | Retry | Compliant later route | Template | Stays pending | Operator |
|---|---|---|---|---|---|---|
| TRANSIENT | network error / timeout, 5xx, 131000 | yes, bounded (`AUTO_RETRY_MAX_ATTEMPTS`), first delay 5 s then backoff | — | no | yes, until retried | only if retries run out |
| RATE_LIMITED | 4, 80007, 130429, 131056, HTTP 429 | yes, bounded, first delay 60 s (131056: 5 min) | — | no | yes | only if retries run out |
| PROVIDER_UNAVAILABLE | 131016, 133004, 131057, HTTP 503 | yes, bounded, first delay 60 s | — | no | yes | only if retries run out |
| INVALID_RECIPIENT | 131026, 131021, 130403 | never | vendor request: other vendors | — | no | yes (`invalid_recipient`: fix the number) |
| INVALID_REQUEST | 100, 131008, 131009, 131051–131053, 132000/132005/132012/132018, 135000 | never | — | — | no | yes (`invalid_outbound_message`: a defect) |
| POLICY_RESTRICTED (window / recipient) | 131047, 131049, 131050, 130472, 131063 | never as free text | buyer: send when the buyer next writes; vendor: other vendors | 131047 needs an approved template — **none is configured**, recorded as `template_configured=false` | buyer updates: yes (`pending_notices`) | vendor 131047: yes (`provider_policy_rejection`); buyer: no |
| POLICY_RESTRICTED (account) | 368, 130497, 131031, 131048, 131064, 132007, 132015, 132016 | never | — | — | no | yes, one channel-level exception (`provider_account_restricted`) |
| AUTH_OR_CONFIG | 0, 3, 10, 190, 200–299, 131005, 131037, 131042, 131045, 133010, 132001, 134011, HTTP 401/403 | never | buyer updates kept for later | 132001 = template missing / not approved | buyer updates: yes | yes, one channel-level exception (`provider_auth_or_config`) |
| UNKNOWN | any other code on a 4xx, or a webhook failure without a code | never | buyer updates kept for later | — | buyer updates: yes | yes (`unclassified_provider_error`) |

An unrecognised code on a 5xx / 429 is classified by the status (temporary).
Every failure is recorded in the decision log (`RETRY_DELIVERY` /
`NOTIFY_BUYER`, with class, code and template facts); a failed notification
never changes the commerce state.

**Provider-reported failures.** A `failed` delivery status for an outbound
message (first report only; duplicates are ignored) is handed to the runtime:
a vendor request moves `SENT → SEND_FAILED` and the buyer's request is
recovered; offers it carried go back to deliverable (`RevertUndelivered`) and
wait for the buyer's next message; an undelivered question is closed (not
counted as asked) and asked again when the buyer next writes; other
buyer-facing updates are kept in `pending_notices`.

## Verification timeline

Business verification can take several days. Start this process in
parallel with development — it should never become a blocker at the end
(Middleman_Technical_Spec_v1 §1).

## Discovery messages (Phase 4B)

Discovery uses outbound interactive messages (reply buttons with an optional
image header, list messages), video, and one approved template. The
channel-neutral model in `internal/channel` is validated against WhatsApp
limits (button title 20, list row title 24, ≤10 rows, ≤3 buttons) and rendered
by `infrastructure/whatsapp`. Media is uploaded once via `POST /{phone-id}/media`
and the returned id reused (re-uploaded after `DISCOVERY_OUTBOUND_MEDIA_TTL`).

Inbound taps are parsed from `interactive.button_reply`, `interactive.list_reply`
and template `button` (quick reply) payloads. A structured id always takes
precedence over offer ordinals and free text. Outside the 24-hour window only an
approved template may be sent; with no template configured, discovery fails
closed. **Template delivery has not been tested against real Meta.** Staff-only
(`DISCOVERY_MODE=INTERNAL`): see `docs/architecture/discovery-distribution.md`.

## Reply context (Phase 4C correction)
When someone uses WhatsApp's **Reply** on one of Majo's messages, Meta sends
`messages[].context.id` (the wamid of the quoted message). The webhook stores
it on the inbound message (`metadata.quoted_message_id`) and the vendor
runtime uses it as a structured association signal: a vendor who replies to
"Majo Request #MM-123456" is answering that request, even with several open.
Outbound wamids are already recorded with their business entity by the
recording sender, so the lookup needs no extra state.

Send failures are classified from Meta's error: 131047/131026/131050 (window
closed, undeliverable, user stopped) are policy rejections — never retried,
raised to the operator; 429/5xx/network errors are retried on the durable job
table with backoff; other 4xx are permanent.
