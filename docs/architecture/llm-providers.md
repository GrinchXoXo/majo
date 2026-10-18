# LLM providers: Gemini or Anthropic

The AI service can call Google Gemini (the owner's choice for the pilot) or
Anthropic. It is used for three things, and each has a fallback:

| Use | Without a key, or when a call fails |
|---|---|
| Reading buyer requests (`/extract`) | the free heuristic extractor |
| Reading vendor replies (`/extract-vendor-response`) | the heuristic classifier |
| Tone layer (`/tone/rewrite`, off by default) | the template text |

## Settings (AI service)

| Variable | Meaning |
|---|---|
| `LLM_PROVIDER` | `gemini`, `anthropic` or `none`. Empty means whichever key is set, Anthropic first |
| `GEMINI_API_KEY` | from Google AI Studio |
| `GEMINI_MODEL` | default `gemini-3.1-flash-lite` (the cheapest Flash-Lite with a free tier, October 2026) |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL` | as before (default `claude-haiku-5-5`) |
| `ANTHROPIC_TIMEOUT_SECONDS` | per-call timeout for either provider (default 10) |
| `TONE_MODEL` | model for the tone layer (default: the provider's model) |

For the pilot: `LLM_PROVIDER=gemini` and `GEMINI_API_KEY=...`. Keep the key
in `.env.local` (development) or `deploy/.env.production` (server) only.

Scoring with your key: `ai/eval_extractor.py` reads `GEMINI_API_KEY` from the
environment or `../.env.local` and prints scores only. Its 100 messages are
made-up samples, so running it on the free tier is fine.

## The free tier and real buyers

Google's Gemini API terms for **unpaid** use (checked 18 October 2026)
say three things:

- Google uses the prompts and responses "to provide, improve, and develop"
  its products.
- "Human reviewers may read, annotate, and process your API input and
  output."
- "Do not submit sensitive, confidential, or personal information to the
  Unpaid Services."

On the **paid** tier, Google does not use prompts or responses to improve
its products; they are kept for a limited time for abuse detection only.

Buyer messages are personal data under the NDPA (who wants what, where).
So:

- **Testing and the eval script:** the free tier is fine.
- **Real buyers:** turn on billing for the Gemini project before the pilot
  starts. Flash-Lite is the cheapest paid model: about $0.25 per million
  input tokens and $1.50 per million output tokens. A request is a few
  hundred tokens, so a thousand requests cost cents.

Majo also limits what leaves the service on every provider:

- Phone numbers and e-mail addresses in a message are replaced with
  `[phone]` / `[email]` before any LLM call (`app/llm.py`, `scrub_personal`).
  Prices and references are kept.
- The tone layer only ever sends the template with its facts masked
  (`docs/architecture/tone-layer.md`).

This is not legal advice. The pilot privacy notice
(`docs/pilot/privacy-notice-draft.md`) should name the AI provider used to
read messages.

## How a Gemini call is made

`POST https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`
with the `x-goog-api-key` header:

- the system prompt goes in `systemInstruction`;
- the message goes in `contents`;
- `generationConfig` sets `temperature` 0, `responseMimeType`
  `application/json`, and a token allowance with room for models that
  think first.

Thought parts are ignored. An empty or blocked answer is a failure, and
the fallback applies.

Not tested against Google from the build sandbox (no network access to it,
no key). The request shape is unit-tested with a faked network
(`ai/tests/test_llm_providers.py`). Run `eval_extractor.py` with the real key
before the pilot; if the model name has changed, set `GEMINI_MODEL`.

Sources:

- [Gemini API Additional Terms of Service](https://ai.google.dev/gemini-api/terms)
- [Gemini Developer API pricing](https://ai.google.dev/gemini-api/docs/pricing)
