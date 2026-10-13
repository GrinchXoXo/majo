"""
Error tracking for the AI service (reuse-audit R1 pilot).

Off unless SENTRY_DSN is set. When on, unhandled exceptions and ERROR log
records are sent to a Sentry-protocol endpoint (GlitchTip self-hosted, or
Sentry). Reports are scrubbed before they leave the process: buyer message
text must never be sent, so request bodies and breadcrumbs are dropped, and
phone-number-like digit runs, bearer tokens and e-mail addresses are masked
in every string. The service never depends on the tracker being reachable.
"""
import os
import re
from typing import Any

_UUID = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b")
_DIGITS = re.compile(r"\+?\d[\d\s-]{6,}\d")
_BEARER = re.compile(r"(?i)bearer\s+[A-Za-z0-9._~+/=-]+")
_EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")


def scrub(s: str) -> str:
    """Mask phone numbers / long digit runs, bearer tokens and e-mails. UUIDs are kept."""
    s = _BEARER.sub("Bearer [redacted]", s)
    s = _EMAIL.sub("[email]", s)
    ids = _UUID.findall(s)
    s = _UUID.sub("\x00", s)
    s = _DIGITS.sub("[number]", s)
    for i in ids:
        s = s.replace("\x00", i, 1)
    return s


def _walk(v: Any) -> Any:
    if isinstance(v, str):
        return scrub(v)
    if isinstance(v, dict):
        return {k: _walk(x) for k, x in v.items()}
    if isinstance(v, list):
        return [_walk(x) for x in v]
    return v


def before_send(event: dict, hint: dict) -> dict:
    event.pop("request", None)  # URL, headers and body of the call (buyer text)
    event.pop("breadcrumbs", None)
    event.pop("user", None)
    for exc in (event.get("exception") or {}).get("values", []):
        for frame in (exc.get("stacktrace") or {}).get("frames", []):
            for k in ("vars", "pre_context", "context_line", "post_context"):
                frame.pop(k, None)  # locals and source lines can hold message text
    for key in ("message", "logentry", "exception", "extra", "tags", "contexts", "logger", "transaction"):
        if key in event:
            event[key] = _walk(event[key])
    return event


def init_error_tracking() -> bool:
    dsn = os.environ.get("SENTRY_DSN", "").strip()
    if not dsn:
        return False
    import sentry_sdk

    sentry_sdk.init(
        dsn=dsn,
        environment=os.environ.get("SENTRY_ENVIRONMENT", os.environ.get("APP_ENV", "development")),
        release=os.environ.get("SENTRY_RELEASE", "majo-ai@unknown"),
        server_name="majo-ai",
        include_local_variables=False,
        max_request_body_size="never",
        max_breadcrumbs=0,
        traces_sample_rate=0,
        before_send=before_send,
    )
    return True
