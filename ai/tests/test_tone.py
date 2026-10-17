"""Pilot batch item 7: the tone endpoint, with the network faked.

The backend does the real fact checks (Go, internal/tone); these tests cover
the service contract: no key -> 503, a broken answer -> 422, a model failure
-> 502, a good answer -> 200, the configured model and timeout are used, and
the message never appears in the logs.
"""
import json
import logging

import pytest
from fastapi.testclient import TestClient

from app import tone
from app.main import app

client = TestClient(app)

MASKED = "[[F1]] Thanks for waiting. Here are the sellers who replied:\n[[F2]]\n[[F3]]\nReply with the number of the one you want."


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
            calls.append({"json": json, "timeout": timeout})
            if isinstance(output, Exception):
                raise output
            return FakeResponse(output)

        import httpx
        monkeypatch.setattr(httpx, "post", post)
        return calls

    return install


def answer(text):
    return json.dumps({"text": text})


def test_no_key_is_503(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    r = client.post("/tone/rewrite", json={"text": MASKED})
    assert r.status_code == 503


def test_good_rewrite(monkeypatch, fake_model):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "k")
    monkeypatch.delenv("TONE_MODEL", raising=False)
    monkeypatch.delenv("ANTHROPIC_MODEL", raising=False)
    good = "[[F1]] Thanks for your patience o. See the sellers wey reply:\n[[F2]]\n[[F3]]\nAbeg reply with the number of the one you want."
    calls = fake_model(answer(good))
    r = client.post("/tone/rewrite", json={"text": MASKED})
    assert r.status_code == 200, r.text
    assert r.json() == {"text": good, "model": "claude-haiku-5-5"}
    assert calls[-1]["json"]["model"] == "claude-haiku-5-5"
    assert calls[-1]["timeout"] == 4.0
    assert "[[F1]]" in calls[-1]["json"]["system"]  # the rules travel with every call


def test_model_and_timeout_are_configuration(monkeypatch, fake_model):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "k")
    monkeypatch.setenv("TONE_MODEL", "claude-sonnet-5-5")
    monkeypatch.setenv("TONE_TIMEOUT_SECONDS", "2.5")
    calls = fake_model(answer(MASKED))
    assert client.post("/tone/rewrite", json={"text": MASKED}).status_code == 200
    assert calls[-1]["json"]["model"] == "claude-sonnet-5-5"
    assert calls[-1]["timeout"] == 2.5


@pytest.mark.parametrize("bad,reason", [
    ("[[F1]] Here are the sellers:\n[[F3]]\n[[F2]]\nReply with a number.", "tokens_changed"),  # reordered
    ("[[F1]] Here are the sellers:\n[[F2]]\nReply with a number.", "tokens_changed"),  # dropped
    ("[[F1]] [[F1]] Here:\n[[F2]]\n[[F3]]", "tokens_changed"),  # duplicated
    ("[[F1]] Here are the 2 sellers:\n[[F2]]\n[[F3]]\nReply.", "digits_added"),
    ("", "empty"),
])
def test_broken_rewrites_are_422(monkeypatch, fake_model, bad, reason):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "k")
    fake_model(answer(bad))
    r = client.post("/tone/rewrite", json={"text": MASKED})
    assert r.status_code == 422
    assert r.json()["detail"] == reason


def test_not_json_is_502(monkeypatch, fake_model):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "k")
    fake_model("Sure! Here you go: no json")
    assert client.post("/tone/rewrite", json={"text": MASKED}).status_code == 502


def test_network_error_is_502_and_message_not_logged(monkeypatch, fake_model, caplog):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "k")
    fake_model(RuntimeError("boom"))
    secret = "Thanks for waiting, Adaeze"
    with caplog.at_level(logging.DEBUG):
        r = client.post("/tone/rewrite", json={"text": secret})
    assert r.status_code == 502
    assert secret not in caplog.text and "Adaeze" not in caplog.text


def test_too_long_is_422():
    assert client.post("/tone/rewrite", json={"text": "x" * 1501}).status_code == 422


def test_check_rewrite_length_cap():
    assert tone.check_rewrite("Hi [[F1]]", "Hi [[F1]] " + "o" * 200) == "too_long"
    assert tone.check_rewrite("Hi [[F1]]", "How far [[F1]]") is None
