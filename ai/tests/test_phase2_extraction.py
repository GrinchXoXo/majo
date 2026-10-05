"""
Phase 2 extraction tests using the PRD 2 §60 test dataset for supply
submissions and vendor responses.
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app.extraction import extract_supply, extract_vendor_response


def test_supply_basic_available():
    r = extract_supply("I get iPhone 13 128gb, clean one, 620k.")
    assert r.product == "iphone 13"
    assert r.variant == "128GB"
    assert r.price == 620000
    assert r.availability_status == "AVAILABLE"


def test_supply_can_source():
    r = extract_supply("I can source MacBook Air M2 around 950k.")
    assert r.availability_status == "CAN_SOURCE"
    assert r.price == 950000


def test_vendor_response_available():
    r = extract_vendor_response("Yes boss, I have one for 620k.")
    assert r.status == "AVAILABLE"
    assert r.price == 620000


def test_vendor_response_unavailable():
    r = extract_vendor_response("No stock currently.")
    assert r.status == "UNAVAILABLE"


def test_vendor_response_can_source_bare_number():
    r = extract_vendor_response("Can get it tomorrow for 640.")
    assert r.status == "CAN_SOURCE"
    assert r.price == 640000


def test_vendor_response_alternative():
    r = extract_vendor_response("128 no dey but 256gb dey 700k.")
    assert r.status == "ALTERNATIVE"
    assert r.variant == "256GB"
    assert r.price == 700000


def test_vendor_response_ambiguous_not_hallucinated():
    """PRD 2 §37: ambiguous replies must never be classified as
    AVAILABLE/UNAVAILABLE confidently — must come back low-confidence."""
    for msg in ["Maybe.", "Which one?"]:
        r = extract_vendor_response(msg)
        assert r.confidence == 0.0
