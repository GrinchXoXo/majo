"""Gemini as the LLM provider (owner's choice), with the network faked.

No test calls Google. They check the request Majo sends, that the answer is
read, that failures fall back, and that phone numbers and e-mails never
leave the service.
"""
import json

import pytest
from fastapi.testclient import TestClient

from app import llm, tone
import app.extraction as ex
from app.main import app


class FakeResponse:
    def __init__(self, body):
        self._body = body

    def raise_for_status(self):
        pass

    def json(self):
        return self._body


def gemini_body(text):
    return {"candidates": [{"content": {"parts": [{"text": text}], "role": "model"}}]}


@pytest.fixture
def gemini(monkeypatch):
    for k in ("ANTHROPIC_API_KEY", "LLM_PROVIDER", "GEMINI_MODEL", "TONE_MODEL", "EXTRACTOR"):
        monkeypatch.delenv(k, raising=False)
    monkeypatch.setenv("GEMINI_API_KEY", "g-key")
    calls = []

    def install(body):
        def post(url, headers=None, json=None, timeout=None):
            calls.append({"url": url, "headers": headers, "json": json, "timeout": timeout})
            if isinstance(body, Exception):
                raise body
            return FakeResponse(body)

        import httpx
        monkeypatch.setattr(httpx, "post", post)
        return calls

    return install


def answer(**kw):
    out = {"category": None, "product": None, "variant": None, "budget_min": None, "budget_max": None,
           "location": None, "condition": None, "quantity": None, "headcount": None, "urgency": None,
           "preferences": [], "confidence": 0.8, "other_products": [], "ambiguities": []}
    out.update(kw)
    return json.dumps(out)


def test_provider_choice(monkeypatch):
    for k in ("ANTHROPIC_API_KEY", "GEMINI_API_KEY", "LLM_PROVIDER"):
        monkeypatch.delenv(k, raising=False)
    assert llm.provider() is None
    monkeypatch.setenv("GEMINI_API_KEY", "g")
    assert llm.provider() == "gemini" and llm.model() == "gemini-3.1-flash-lite"
    monkeypatch.setenv("ANTHROPIC_API_KEY", "a")
    assert llm.provider() == "anthropic"  # both keys: Anthropic unless told otherwise
    monkeypatch.setenv("LLM_PROVIDER", "gemini")
    assert llm.provider() == "gemini"
    monkeypatch.setenv("LLM_PROVIDER", "none")
    assert llm.provider() is None
    monkeypatch.setenv("LLM_PROVIDER", "gemini")
    monkeypatch.delenv("GEMINI_API_KEY")
    assert llm.provider() is None  # chosen but no key: heuristic, never a broken call


def test_extract_with_gemini(gemini, monkeypatch):
    calls = gemini(gemini_body(answer(product="laptop", category="electronics", budget_max=300000)))
    monkeypatch.setenv("GEMINI_MODEL", "gemini-3.8-flash")
    body = TestClient(app).post("/extract", json={"text": "I need a laptop under 300k, call me on 0803 123 4567"}).json()
    assert body["extraction_method"] == "gemini" and body["model"] == "gemini-3.8-flash"
    assert body["product"] == "laptop" and body["budget_max"] == 300000
    c = calls[-1]
    assert c["url"].endswith("/models/gemini-3.8-flash:generateContent")
    assert c["headers"]["x-goog-api-key"] == "g-key"
    sent = c["json"]["contents"][0]["parts"][0]["text"]
    assert "0803" not in sent and "[phone]" in sent and "300k" in sent
    assert c["json"]["generationConfig"]["responseMimeType"] == "application/json"


def test_gemini_guard_still_applies(gemini):
    gemini(gemini_body(answer(product="small chops", budget_max=20000)))
    body = TestClient(app).post("/extract", json={"text": "small chops for 20 people"}).json()
    assert body["budget_max"] is None  # a headcount is never a budget, whatever the model says


@pytest.mark.parametrize("bad", [RuntimeError("network"), {"candidates": []}, gemini_body("   "), gemini_body("not json")])
def test_gemini_failure_falls_back(gemini, bad):
    gemini(bad)
    body = TestClient(app).post("/extract", json={"text": "I need a laptop"}).json()
    assert body["extraction_method"] == "heuristic_fallback" and body["product"] == "laptop"


def test_thought_parts_are_ignored(gemini):
    gemini({"candidates": [{"content": {"parts": [{"text": "thinking...", "thought": True},
                                                   {"text": answer(product="ps5")}]}}]})
    assert TestClient(app).post("/extract", json={"text": "ps5 dey?"}).json()["product"] == "ps5"


def test_vendor_reply_and_tone_use_gemini(gemini):
    calls = gemini(gemini_body(json.dumps({"status": "AVAILABLE", "price": 620000})))
    res, method = ex.classify_vendor_response("Yes I get am, 620k")
    assert calls and calls[-1]["url"].endswith(":generateContent") and res.status == "AVAILABLE"
    gemini(gemini_body(json.dumps({"text": "How far [[F1]] o"})))
    assert tone.rewrite("Hello [[F1]]") == {"text": "How far [[F1]] o", "model": "gemini-3.1-flash-lite"}


@pytest.mark.parametrize("text,kept", [
    ("budget 650,000 for iphone", "650,000"), ("₦620000", "₦620000"), ("350000 naira", "350000"),
    ("between 35-40k", "35-40k"), ("MM-123456", "MM-123456"),
])
def test_scrub_keeps_prices(text, kept):
    assert kept in llm.scrub_personal(text)


@pytest.mark.parametrize("text", ["call 08031234567", "+234 803 123 4567 is my line", "2348031234567", "mail ada@example.com"])
def test_scrub_removes_contacts(text):
    out = llm.scrub_personal(text)
    assert "803" not in out and "example.com" not in out
