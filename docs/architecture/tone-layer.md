# Tone layer (pilot batch item 7)

Majo's buyer messages are written as plain templates. The tone layer lets an
LLM rewrite them in a natural Nigerian WhatsApp register, for example "No
wahala 👍 I go check back later", **without being able to change a fact**.
It is off by default.

## Where it sits

`conversation.RecordingSender.SendText` → `tone.Layer.Tone` → AI service
`POST /tone/rewrite` → back to the sender, which sends and records the
result.

Only messages tagged `Audience: buyer` in their `OutboundPurpose` go through
it. These are:

- request clarifications;
- workflow notices (including late notices);
- the replies of the buyer inbound runtime;
- offer delivery;
- buyer-side deal messages.

Vendor messages, templates, rich (button or list) messages, discovery, and
anything untagged are sent exactly as written.

## How facts are protected

1. **Mask (Go, `internal/tone`).** Before anything leaves the backend, every
   fact is replaced by a token `[[F1]]`, `[[F2]]`, … The following are masked:
   - whole data lines: numbered offers and their indented details, short
     `Label: value` lines such as `Seller: Mike Gadgets`, and lines containing
     a link;
   - amounts, numbers and ranges;
   - links;
   - references (`MM-123456`);
   - quoted text;
   - reply keywords (`YES`, `NOT YET`);
   - capitalised names;
   - the terms the sender marks as fixed: product, variant, location, item,
     seller name.

   The model never sees prices, phone numbers or references.
2. **Rewrite (Python, `ai/app/tone.py`).** The model is told to keep every
   token once and in order, add nothing, and answer with JSON. The service
   does a first check, then the backend checks again.
3. **Check (Go).** The rewrite is refused if any of these fail:
   - the tokens are not identical and in the same order;
   - it adds a digit, `₦`, a link, or `@`;
   - it adds a number word, a time word ("tomorrow", "soon"), or a claim
     ("free", "delivery", "discount", "guaranteed", "price");
   - it drops an uncommon word (item words such as "rechargeable");
   - it changes negation ("No wahala" does not count as negation);
   - it turns a question into a statement;
   - its length is outside about ⅓× to 2× the original.
4. **Unmask or fall back.** If the check passes, the facts go back in. If
   the check fails, or the call fails, times out (`TONE_TIMEOUT`, 5s), or
   there is no API key, **the original template text is sent**.

A message is also skipped when it is longer than 1,200 bytes, has nothing
but data lines, contains `[[F`, or is not valid UTF-8.

## Modes (`TONE_LAYER`)

| Mode | What the buyer gets | Stored on the message (`metadata.tone`) |
|---|---|---|
| `off` (default) | the template | nothing; no AI call |
| `shadow` | the template | `result: shadow`, `candidate` (what would have been sent), `model`, `ms` |
| `on` | the rewrite if it passed, else the template | `result: applied`, `original`; or `rejected` with `reason` and `candidate`; or `rewrite_failed` with `error` |

`content.text` is always exactly what was sent.

## Switching it on

1. Run the preview with the real key (AI service running locally):
   `cd backend && go run ./cmd/tonepreview`. It prints each kind of buyer
   message (`internal/tone/samples.go`), what the model saw, and what the
   buyer would get, followed by a summary. Nothing is sent.
2. Run `TONE_LAYER=shadow` for a few days of the pilot and read the
   candidates:

   ```sql
   SELECT metadata->'tone'->>'result', content->>'text', metadata->'tone'->>'candidate'
   FROM messages
   WHERE metadata ? 'tone'
   ORDER BY created_at DESC
   LIMIT 50;
   ```
3. Switch to `on` when the candidates read well. Switch back to `off`
   any time (restart the backend).

## Cost and latency

There is one model call per buyer message, with short input and output.
On `ANTHROPIC_MODEL` (Haiku by default) it adds roughly a second before each
buyer message. The timeout caps the wait.

## Tests

- `internal/tone/tone_test.go`:
  - masks for every real template;
  - accepted rewrites and every rejection rule;
  - all three modes, timeout and failure fallbacks;
  - a 3,000-case adversarial test: whatever the "model" returns, every fact
    reaches the buyer unchanged and in order;
  - a fuzz test showing that masking always round-trips exactly.
- `internal/conversation/tone_sender_test.go`: only buyer-tagged text is
  toned, and it is stored as sent.
- `infrastructure/postgres/tone_integration_test.go`: a real workflow notice
  in `on` and `shadow` modes, stored metadata, and that vendor text is never
  toned.
- `ai/tests/test_tone.py`: the endpoint contract (503, 422, 502 and 200;
  model and timeout configuration; the message is never logged).

YarnGPT (text-to-speech) is not used here. It could later voice the rewritten
text for voice-note replies.
