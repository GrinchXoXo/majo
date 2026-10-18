"""
Which LLM the AI service calls, and how (pilot: Anthropic or Google Gemini).

    LLM_PROVIDER   anthropic | gemini | none   (unset: Anthropic if
                   ANTHROPIC_API_KEY is set, else Gemini if GEMINI_API_KEY
                   is set, else none = the free heuristic only)
    ANTHROPIC_API_KEY, ANTHROPIC_MODEL (default claude-haiku-5-5)
    GEMINI_API_KEY,    GEMINI_MODEL    (default gemini-3.1-flash-lite)
    ANTHROPIC_TIMEOUT_SECONDS          (any provider; default 10)

Every caller (buyer extraction, vendor replies, tone layer) treats the
model's answer as a proposal: it is checked before use, and on any failure
the heuristic result or the template text is used instead.

Phone numbers and e-mail addresses are replaced before any text leaves
the service (scrub_personal). Google's terms for the unpaid Gemini tier
say not to submit personal information, and prompts there may be read by
human reviewers and used to improve Google's products: see
docs/architecture/llm-providers.md.
"""
import os
import re
from typing import Optional

DEFAULT_MODELS = {"anthropic": "claude-haiku-5-5", "gemini": "gemini-3.1-flash-lite"}
PROVIDERS = tuple(DEFAULT_MODELS)

GEMINI_URL = "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent"
ANTHROPIC_URL = "https://api.anthropic.com/v1/messages"


def provider() -> Optional[str]:
    p = (os.getenv("LLM_PROVIDER") or "").strip().lower()
    if p == "none":
        return None
    if p in PROVIDERS:
        return p if api_key(p) else None
    if os.getenv("ANTHROPIC_API_KEY"):
        return "anthropic"
    if os.getenv("GEMINI_API_KEY"):
        return "gemini"
    return None


def api_key(p: str) -> str:
    return os.getenv("ANTHROPIC_API_KEY" if p == "anthropic" else "GEMINI_API_KEY", "") or ""


def model(p: Optional[str] = None) -> Optional[str]:
    p = p or provider()
    if p is None:
        return None
    env = "ANTHROPIC_MODEL" if p == "anthropic" else "GEMINI_MODEL"
    return (os.getenv(env) or "").strip() or DEFAULT_MODELS[p]


def timeout() -> float:
    try:
        return float(os.getenv("ANTHROPIC_TIMEOUT_SECONDS", "10"))
    except ValueError:
        return 10.0


# Nigerian and international phone numbers (7+ digits with optional + and
# separators) and e-mail addresses. Prices ("620k", "₦650,000", "350000")
# are not touched: a phone number has at least 10 digits, starts with 0 or
# + / 234, and has no thousands commas.
_PHONE = re.compile(r"(?<![\d₦#])(?:\+?234|0)[\s\-.]?\d{3}[\s\-.]?\d{3}[\s\-.]?\d{4}(?!\d)")
_EMAIL = re.compile(r"[\w.+-]+@[\w-]+\.[\w.-]+")


def scrub_personal(text: str) -> str:
    return _EMAIL.sub("[email]", _PHONE.sub("[phone]", text))


def call(system: str, text: str, max_tokens: int, model_name: Optional[str] = None,
         timeout_s: Optional[float] = None, p: Optional[str] = None, key: Optional[str] = None) -> str:
    """One completion; returns the model's text. Raises on any failure."""
    p = p or provider()
    if p is None:
        raise RuntimeError("no LLM provider configured")
    key = key or api_key(p)
    m = model_name or model(p)
    t = timeout_s if timeout_s is not None else timeout()
    text = scrub_personal(text)
    if p == "gemini":
        return _call_gemini(key, system, text, max_tokens, m, t)
    return _call_anthropic(key, system, text, max_tokens, m, t)


def _call_anthropic(key, system, text, max_tokens, model_name, timeout_s) -> str:
    import httpx

    resp = httpx.post(
        ANTHROPIC_URL,
        headers={"x-api-key": key, "anthropic-version": "2023-06-01", "content-type": "application/json"},
        json={"model": model_name, "max_tokens": max_tokens, "system": system,
              "messages": [{"role": "user", "content": text}]},
        timeout=timeout_s,
    )
    resp.raise_for_status()
    data = resp.json()
    return "".join(block["text"] for block in data["content"] if block["type"] == "text")


def _call_gemini(key, system, text, max_tokens, model_name, timeout_s) -> str:
    import httpx

    resp = httpx.post(
        GEMINI_URL.format(model=model_name),
        headers={"x-goog-api-key": key, "content-type": "application/json"},
        json={
            "systemInstruction": {"parts": [{"text": system}]},
            "contents": [{"role": "user", "parts": [{"text": text}]}],
            # Room for models that think before answering; JSON only.
            "generationConfig": {"temperature": 0, "maxOutputTokens": max(max_tokens * 4, 2048),
                                 "responseMimeType": "application/json"},
        },
        timeout=timeout_s,
    )
    resp.raise_for_status()
    data = resp.json()
    candidates = data.get("candidates") or []
    if not candidates:
        raise RuntimeError("gemini returned no candidates (blocked or empty)")
    parts = (candidates[0].get("content") or {}).get("parts") or []
    out = "".join(part.get("text", "") for part in parts if not part.get("thought"))
    if not out.strip():
        raise RuntimeError("gemini returned no text")
    return out
