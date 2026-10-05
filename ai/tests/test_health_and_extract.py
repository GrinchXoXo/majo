"""
Phase 0 contract tests for the AI service.
Verifies /health responds and /extract returns the documented
ExtractionResult shape, matching what the Go AIProvider expects.
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from fastapi.testclient import TestClient
from app.main import app

client = TestClient(app)


def test_health():
    resp = client.get("/health")
    assert resp.status_code == 200
    assert resp.json() == {"status": "ok"}


def test_extract_returns_contract_shape():
    resp = client.post("/extract", json={"text": "need a mattress, budget 60k, benin"})
    assert resp.status_code == 200
    body = resp.json()

    expected_fields = {
        "category", "product", "variant", "budget_min", "budget_max",
        "location", "condition", "quantity", "urgency", "preferences", "confidence",
    }
    assert expected_fields.issubset(body.keys())
    assert isinstance(body["preferences"], list)
    assert isinstance(body["confidence"], float)
