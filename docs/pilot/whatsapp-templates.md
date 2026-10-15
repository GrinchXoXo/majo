# WhatsApp templates to submit to Meta

WhatsApp only lets Majo send free text to someone who wrote to Majo in the
last 24 hours. Anything else must be a template that Meta has approved.
Submit these in WhatsApp Manager → Message templates → Create template.
Approval usually takes minutes to a day; a rejection gives a reason you can
fix and resubmit.

## 1. Vendor request (needed for the pilot)

Without it, a vendor who has not messaged Majo in the last 24 hours never
receives the request (Meta refuses it with error 131047), and the buyer
waits for nothing.

| Field | Value |
|---|---|
| Category | **Utility** (it is about a request the vendor signed up for, not marketing) |
| Name | `majo_vendor_request` |
| Language | English (`en`) |
| Header | none |
| Footer | `Majo` |
| Buttons | none (vendors reply in their own words, as today) |

**Body** (copy exactly; `{{1}}`–`{{4}}` are the variables):

```
Hello, a buyer on Majo is looking for {{1}}.
Budget: {{2}}
Location: {{3}}
Reference: {{4}}

Can you supply this? Reply to this message with Available and your price, Can source, or Not available.
```

**Sample values** (Meta asks for one per variable):

| Variable | What Majo fills in | Sample |
|---|---|---|
| `{{1}}` | the item, with details and headcount | `small chops, for 20 people` |
| `{{2}}` | budget | `Up to ₦45,000` |
| `{{3}}` | location | `Benin City` |
| `{{4}}` | request reference | `MM-482913` |

When a vendor is asked to re-confirm their own listing, the same template is
used: `{{1}}` reads `your listing L-ABC123: iphone 13` and `{{2}}` reads
`Your listed price ₦620,000`.

**After approval**, add to the server's environment (and `.env.local` for a
local run):

```
VENDOR_REQUEST_TEMPLATE_NAME=majo_vendor_request
VENDOR_REQUEST_TEMPLATE_LANGUAGE=en
```

What Majo then does:

- Vendor wrote in the last ~23½ hours → free text, as today.
- Otherwise, or if Meta refuses the free text for the 24-hour rule → the template.
- The vendor's reply (quoting the message, or plain text while it is their
  only open request) attaches to the request exactly as before.
- Every send records whether it went as text or template (`sent_as`).

If the name or language in the environment does not match an approved
template, Meta refuses the send; the vendor request shows SEND_FAILED on the
dashboard with Meta's error (132001 = template does not exist).

## 2. Buyer update (optional for the pilot)

Not built yet. Today, when a buyer's 24-hour window has closed, updates such
as "no vendor has replied yet" wait until the buyer writes again. That is
acceptable for a small pilot. A template for it can be added later.

## 3. Discovery (not for the pilot)

The discovery feature (Phase 4B) has its own template setting
(`DISCOVERY_TEMPLATE_NAME`). Discovery stays off for the pilot.
