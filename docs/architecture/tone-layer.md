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

   The model never sees prices, phone numbers or references. A
   one-word name at the start of a sentence ("Tunde replied") is not
   masked; the model sees it, but the check keeps it exactly.
2. **Rewrite (Python, `ai/app/tone.py`).** The model is told to keep every
   token once and in order, add nothing, and answer with JSON. The service
   does a first check, then the backend checks again.
3. **Check (Go).** The rule is that the template's words survive, in order.
   - The tokens must be identical and in the same order.
   - Every word of the template must still be there, in the same order.
     Words are compared after expanding contractions, removing plain
     plural and tense endings, and mapping a few Pidgin equivalents: "no" =
     not, "fit" = can, "wetin" = what, "una" = you; "I no fit find" reads
     as "I can not find". Two exceptions: filler words ("the", "please",
     "did", "will", "still", "yet", "and"...) may be left out, and the
     fixed expressions "no wahala", "no problem" and "how far" are ignored.
   - Added words must come from a small register list: Pidgin particles
     ("o", "abeg", "dey", "don", "go", "na", "wey", "make", "sef", "sha",
     "abi"...), greetings, and grammatical glue.
   - The rewrite may not add a digit of any script, `₦`, a link (including a
     bare domain such as `bit.ly/x`) or `@`.
   - Negation must stay the same, questions must stay questions, and the
     length must stay within about ⅓× to 2× the original.

   So a rewrite cannot swap "bad" and "great" on a rating scale, turn
   "after" into "before", give YES and NO new meanings, add "free",
   "without" or "brand new", or drop an item word. An independent review
   tried each of these against the first version, which passed them; all
   are now regression tests (`TestCheckRejectsMeaningFlips`). The price of
   this is that the model may only *season* the template, not rephrase it.
   Shadow mode shows how often that still reads naturally.
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
