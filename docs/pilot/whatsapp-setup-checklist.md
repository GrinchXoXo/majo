# WhatsApp setup checklist (before launch)

Taken from `docs/architecture/whatsapp.md` and the backend config
(`backend/internal/config/config.go`). Names only — never put real values
in this file.

## Meta Business Manager / Developers

- [ ] **Meta Business Account** — verified; owned by the business (not an
      individual's personal account). Business verification can take
      several days — start early, in parallel with everything else.
- [ ] **WhatsApp Business Account (WABA)** with a dedicated phone number.
- [ ] **App** created in Meta for Developers, with the WhatsApp product
      added to it.
- [ ] **Permanent access token** — generated via a System User in
      Business Settings (not a temporary token from API Setup).
- [ ] **Phone number ID** — WhatsApp > API Setup in the app dashboard.
- [ ] **Verify token** — a string you choose yourself; Meta echoes it
      back during webhook verification to prove ownership.
- [ ] **App secret** — App Dashboard > App settings > Basic. Meta signs
      every webhook POST with it (`X-Hub-Signature-256`); the backend
      rejects unsigned/invalid POSTs with 401. Required unless
      `APP_ENV=development`.
- [ ] **Webhook** pointed at `POST https://<your-domain>/webhooks/whatsapp`,
      with the **`messages`** field subscribed, and the verify token
      entered in Meta's dashboard so the `GET` handshake succeeds.
- [ ] **Message template(s) submitted and approved** in Meta (see below).

## Backend environment variables (WhatsApp-related, names only)

Set on the backend before start (values stay in the server environment,
never in git):

- `WHATSAPP_API_URL` — Graph API base URL (default points at
  graph.facebook.com v19.0)
- `WHATSAPP_ACCESS_TOKEN` — the permanent System User token
- `WHATSAPP_PHONE_NUMBER_ID` — the business number's ID
- `WHATSAPP_VERIFY_TOKEN` — your chosen webhook verify string
- `WHATSAPP_APP_SECRET` — for webhook signature checks
- `DISCOVERY_TEMPLATE_NAME` — approved template for discovery messages
  outside the 24h window (empty = none configured)
- `DISCOVERY_TEMPLATE_LANGUAGE` — template language code (default `en`)

## Message templates used by the code

- **Core buyer/vendor messaging: no template.** The code sends free-form
  text only (`docs/architecture/whatsapp.md`: "Majo does not send
  templates yet"). Outside the 24-hour customer-service window those
  sends fail, and Majo queues buyer updates until the buyer writes
  again.
- **Discovery (Phase 4B): one approved template**, configured through
  `DISCOVERY_TEMPLATE_NAME`. With no name set, discovery outside the
  session window fails closed. Template delivery has not been tested
  against real Meta.
  - Template name `majo_finds` appears in the codebase only in test
    fixtures (`infrastructure/whatsapp/rich_message_test.go`,
    `internal/distribution/config_test.go`,
    `internal/presentation/discovery_test.go`).
  - TODO(owner): submit the **vendor request** template
    (`majo_vendor_request`, wording in `docs/pilot/whatsapp-templates.md`)
    and set `VENDOR_REQUEST_TEMPLATE_NAME` once approved. Needed for the
    pilot: without it vendors who have not written in 24h are not reached.
  - The discovery template (`DISCOVERY_TEMPLATE_NAME`) is not needed for
    the pilot (discovery stays off).
  - A buyer-update template is optional for the pilot (see the templates doc).
