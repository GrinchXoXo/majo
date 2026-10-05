# WhatsApp Integration Notes

Middleman uses the **Meta WhatsApp Cloud API directly** (no BSP) per
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

## Verification timeline

Business verification can take several days. Start this process in
parallel with development — it should never become a blocker at the end
(Middleman_Technical_Spec_v1 §1).
