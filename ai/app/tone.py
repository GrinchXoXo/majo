"""
Tone layer (pilot batch item 7): rewrite an outgoing buyer message in a
natural Nigerian WhatsApp register.

The Go backend owns the safety: before calling this it replaces every fact
(prices, numbers, names, links, references, reply keywords, list lines) with
a token [[F1]], [[F2]], ..., and afterwards it checks the rewrite and puts the
facts back, sending the original template text if anything is off. This
service never sees the facts themselves, so phone numbers and prices do not
reach the model. The checks here are a first filter only.
"""
import os
import re
from typing import Optional

from app.extraction import _call_anthropic, _parse_json_object, anthropic_model

TOKEN = re.compile(r"\[\[F\d+\]\]")

TONE_SYSTEM_PROMPT = """You rewrite short WhatsApp messages that Majo, a Nigerian \
shopping helper, sends to a buyer. Make each one sound like a friendly, sharp \
Nigerian person texting: natural Nigerian English with a light touch of Pidgin \
where it fits ("no wahala", "abeg", "o", "how far"). Not heavy Pidgin, \
not slang a student in any Nigerian city would not understand, never rude, \
never over-familiar.

Rules:
- Tokens like [[F1]] stand for fixed facts (prices, numbers, names, products, \
links, references, reply keywords, whole list lines). Copy every token exactly, \
each exactly once, in the same order. Never change, split, translate or drop one.
- Keep the meaning: every question, instruction and piece of information stays. \
If the buyer is asked to reply with something, they must still be asked to. \
Keep every item, product and place word exactly as written, and keep \
questions as questions.
- Add nothing: no numbers, prices, dates, times, places, names, promises, \
discounts, delivery claims or advice that the original does not contain.
- Keep the original's words in the same order. You may only: add Nigerian \
particles and greetings ("o", "oh", "abeg", "dey", "don", "go", "na", "wey", \
"make", "sef", "sha", "abi", "e", "am", "boss", "how far", "no wahala"); \
leave out filler words ("the", "please", "just", "did", "will", "is", "still", \
"yet"); use "wetin" for "what", "fit" for "can", "no" for "not". Any other \
new word, or any word moved, makes Majo send the original instead.
- Keep it about as short as the original. Keep a token that stands on its own \
line on its own line. Keep the original's emojis; add at most one.
- Do not use digits anywhere outside the tokens.

Answer with JSON only: {"text": "<the rewritten message>"}"""


def tone_model() -> str:
    return (os.getenv("TONE_MODEL") or "").strip() or anthropic_model()


def tone_timeout() -> float:
    try:
        return float(os.getenv("TONE_TIMEOUT_SECONDS", "4"))
    except ValueError:
        return 4.0


class ToneUnavailable(Exception):
    """No API key: the caller sends the original text."""


class ToneRejected(Exception):
    """The model's answer broke a rule; the caller sends the original text."""


def tokens_of(text: str) -> list:
    return TOKEN.findall(text)


def check_rewrite(original: str, rewritten: str) -> Optional[str]:
    """First-pass checks (the Go side checks again, more strictly).
    Returns the reason the rewrite is unusable, or None."""
    if not rewritten.strip():
        return "empty"
    if tokens_of(original) != tokens_of(rewritten):
        return "tokens_changed"
    if re.search(r"\d", TOKEN.sub("", rewritten)):
        return "digits_added"
    if len(rewritten) > max(2 * len(original), len(original) + 120):
        return "too_long"
    return None


def rewrite(text: str, api_key: Optional[str] = None) -> dict:
    key = api_key if api_key is not None else os.getenv("ANTHROPIC_API_KEY", "")
    if not key:
        raise ToneUnavailable("no ANTHROPIC_API_KEY")
    raw = _call_anthropic(key, TONE_SYSTEM_PROMPT, text, max_tokens=600,
                          model=tone_model(), timeout=tone_timeout())
    obj = _parse_json_object(raw)
    out = obj.get("text")
    if not isinstance(out, str):
        raise ToneRejected("no text in model output")
    out = out.strip()
    reason = check_rewrite(text, out)
    if reason:
        raise ToneRejected(reason)
    return {"text": out, "model": tone_model()}
