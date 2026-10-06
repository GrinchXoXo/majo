# WhatsApp Integration Notes

Majo uses the **Meta WhatsApp Cloud API directly** (no BSP) per
Middleman_Technical_Spec_v1 §1.

## What you need from Meta Business Manager

1. **Meta Business Account** — verified business, owned by Middleman
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
  same way; the offer stays deliverable and the failure is shown on the
  dashboard (retry with "Deliver pending offers" after the buyer writes again).

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
