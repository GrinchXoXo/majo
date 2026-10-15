"""Pilot batch: the Anthropic extractor, with the network faked.

No test here calls the real API or needs a key. They check what Majo does
with whatever the model says: the model is configuration, the model's
output is only a proposal, and a failure never loses the buyer's message.
"""
import json
import logging

import pytest
from fastapi.testclient import TestClient

import app.extraction as ex
from app.main import app


class FakeResponse:
    def __init__(self, text):
        self._text = text

    def raise_for_status(self):
        pass

    def json(self):
        return {"content": [{"type": "text", "text": self._text}]}


@pytest.fixture
def fake_model(monkeypatch):
    calls = []

    def install(output):
        def post(url, headers=None, json=None, timeout=None):
            calls.append({"url": url, "headers": headers, "json": json, "timeout": timeout})
            if isinstance(output, Exception):
                raise output
            return FakeResponse(output if isinstance(output, str) else __import__("json").dumps(output))

        import httpx
        monkeypatch.setattr(httpx, "post", post)
        return calls

    return install


def base(**kw):
    out = {"category": None, "product": None, "variant": None, "budget_min": None, "budget_max": None,
           "location": None, "condition": None, "quantity": None, "headcount": None, "urgency": None,
           "preferences": [], "confidence": 0.8, "other_products": [], "ambiguities": []}
    out.update(kw)
    return out


def test_model_is_configuration(monkeypatch, fake_model):
    calls = fake_model(base(product="laptop"))
    monkeypatch.delenv("ANTHROPIC_MODEL", raising=False)
    ex.AnthropicExtractor("k").extract("I need a laptop")
    assert calls[-1]["json"]["model"] == "claude-haiku-5-5"
    monkeypatch.setenv("ANTHROPIC_MODEL", "claude-sonnet-5-5")
    ex.AnthropicExtractor("k").extract("I need a laptop")
    assert calls[-1]["json"]["model"] == "claude-sonnet-5-5"
    monkeypatch.setenv("ANTHROPIC_TIMEOUT_SECONDS", "4")
    ex.AnthropicExtractor("k").extract("I need a laptop")
    assert calls[-1]["timeout"] == 4.0


def test_no_hardcoded_model_left():
    src = open(ex.__file__).read()
    assert "claude-haiku-4-5" not in src


def test_headcount_read_as_budget_by_the_model_is_dropped(fake_model):
    fake_model(base(product="small chops", category="food", budget_min=20000, budget_max=20000))
    r = ex.AnthropicExtractor("k").extract("abeg I need small chops for 20 people")
    assert (r.budget_min, r.budget_max) == (None, None)
    assert r.headcount == 20 and "for 20 people" in r.preferences


def test_unclear_shorthand_budget_is_dropped(fake_model):
    fake_model(base(product="iphone 14 pro", budget_max=650000))
    r = ex.AnthropicExtractor("k").extract("person dey sell original iphone 14 pro? my budget 6.5, abeg")
    assert r.budget_max is None


@pytest.mark.parametrize("text,lo,hi", [
    ("tv budget #85,000", 85000, 85000),
    ("sneaker size 43, 35k", 35000, 35000),
    ("generator 35-40k", 35000, 40000),
    ("budget 250,000 naira", 250000, 250000),
])
def test_supported_budgets_are_kept(fake_model, text, lo, hi):
    fake_model(base(product="x", budget_min=lo, budget_max=hi))
    r = ex.AnthropicExtractor("k").extract(text)
    assert (r.budget_min, r.budget_max) == (lo, hi)


def test_unit_numbers_cannot_support_a_budget(fake_model):
    fake_model(base(product="power bank", budget_max=20000))
    assert ex.AnthropicExtractor("k").extract("power bank 20,000mah").budget_max is None


def test_quantity_equal_to_headcount_is_cleared(fake_model):
    fake_model(base(product="fried rice", quantity=50, headcount=50))
    r = ex.AnthropicExtractor("k").extract("fried rice for 50 people")
    assert r.quantity is None and r.headcount == 50


def test_output_is_cleaned(fake_model):
    fake_model("```json\n" + json.dumps(base(product="  Laptop ", condition="tokunbo", confidence=7,
                                             ambiguities=["made_up", "condition_conflict"], unknown_key=1)) + "\n```")
    r = ex.AnthropicExtractor("k").extract("I need laptop")
    assert r.product == "laptop" and r.condition is None and r.confidence == 1.0
    assert r.ambiguities == ["condition_conflict"]


def test_no_product_means_zero_confidence(fake_model):
    fake_model(base(confidence=0.9))
    assert ex.AnthropicExtractor("k").extract("good morning").confidence == 0.0


def test_endpoint_reports_method_and_model(monkeypatch, fake_model):
    fake_model(base(product="laptop", category="electronics"))
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test-key")
    monkeypatch.delenv("EXTRACTOR", raising=False)
    monkeypatch.delenv("ANTHROPIC_MODEL", raising=False)
    body = TestClient(app).post("/extract", json={"text": "I need a laptop"}).json()
    assert body["extraction_method"] == "anthropic" and body["model"] == "claude-haiku-5-5"
    assert body["product"] == "laptop"


@pytest.mark.parametrize("failure", [RuntimeError("network down"), "not json at all", "[1, 2]"])
def test_failure_falls_back_to_heuristic_and_never_logs_the_message(monkeypatch, fake_model, caplog, failure):
    fake_model(failure)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "secret-test-key")
    monkeypatch.delenv("EXTRACTOR", raising=False)
    text = "abeg I need small chops for 20 people, budget 45,000, my number 08012345678"
    with caplog.at_level(logging.WARNING):
        body = TestClient(app).post("/extract", json={"text": text}).json()
    assert body["extraction_method"] == "heuristic_fallback"
    assert body["product"] == "small chops" and body["budget_max"] == 45000 and body["headcount"] == 20
    assert "08012345678" not in caplog.text and "small chops" not in caplog.text and "secret-test-key" not in caplog.text


def test_extractor_switch_forces_heuristic(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "k")
    monkeypatch.setenv("EXTRACTOR", "heuristic")
    assert isinstance(ex.get_extractor(), ex.HeuristicExtractor)
    monkeypatch.delenv("EXTRACTOR")
    assert isinstance(ex.get_extractor(), ex.AnthropicExtractor)


def test_without_a_key_the_heuristic_runs(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    body = TestClient(app).post("/extract", json={"text": "I need a laptop"}).json()
    assert body["extraction_method"] == "heuristic" and body["model"] is None


def test_vendor_reply_model_output_is_checked(fake_model):
    fake_model({"status": "DEFINITELY", "price": 999999, "confidence": 0.9})
    r = ex.AnthropicVendorResponseExtractor("k").extract("yes I get am, 620k")
    assert r.status == "UNCLASSIFIED" and r.review_state == "NEEDS_REVIEW" and r.price is None
    fake_model({"status": "AVAILABLE", "price": 620000, "confidence": 0.9})
    r = ex.AnthropicVendorResponseExtractor("k").extract("yes I get am, 620k")
    assert r.status == "AVAILABLE" and r.price == 620000
