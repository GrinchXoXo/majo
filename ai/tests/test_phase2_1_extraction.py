"""
Phase 2.1 regression tests: status/review-state separation, conservative
classification, Nigerian pidgin extraction, and heuristic/AI reconciliation.

Every test case here corresponds to a specific bug found in the PRD 2.1
code-level audit — see the audit report for the empirical "before" output
each of these guards against.
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app.extraction import extract_vendor_response, classify_vendor_response


# --- Conservative classification (PRD 2.1 §2) ---

def test_hedge_maybe_is_unclassified_not_unavailable():
    r = extract_vendor_response("Maybe.")
    assert r.status == "UNCLASSIFIED"
    assert r.review_state == "NEEDS_REVIEW"


def test_question_which_one_is_unclassified():
    r = extract_vendor_response("Which one?")
    assert r.status == "UNCLASSIFIED"
    assert r.review_state == "NEEDS_REVIEW"


def test_question_how_much_is_unclassified():
    r = extract_vendor_response("How much?")
    assert r.status == "UNCLASSIFIED"
    assert r.review_state == "NEEDS_REVIEW"


def test_bare_price_alone_is_unclassified_not_available():
    """The critical audit finding: a price alone must NOT imply availability."""
    r = extract_vendor_response("620k")
    assert r.status == "UNCLASSIFIED"
    assert r.review_state == "NEEDS_REVIEW"
    assert r.price == 620000  # still extracted for operator visibility


def test_bare_time_reference_is_unclassified():
    r = extract_vendor_response("Tomorrow.")
    assert r.status == "UNCLASSIFIED"
    assert r.review_state == "NEEDS_REVIEW"


def test_yes_with_price_is_available():
    r = extract_vendor_response("Yes, 620k")
    assert r.status == "AVAILABLE"
    assert r.review_state == "REVIEWED"
    assert r.price == 620000


def test_i_have_it_is_available():
    r = extract_vendor_response("I have it for 620k")
    assert r.status == "AVAILABLE"
    assert r.price == 620000


def test_no_stock_is_unavailable():
    r = extract_vendor_response("No stock")
    assert r.status == "UNAVAILABLE"
    assert r.review_state == "REVIEWED"
    # A confidently-matched negative should not be forced to zero
    # confidence just because it isn't a positive result.
    assert r.confidence > 0.0


def test_can_get_tomorrow_bare_number_is_can_source():
    r = extract_vendor_response("Can get tomorrow for 640")
    assert r.status == "CAN_SOURCE"
    assert r.price == 640000


# --- Nigerian pidgin (PRD 2.1 §3) ---

def test_pidgin_i_get_am_is_available():
    r = extract_vendor_response("I get am")
    assert r.status == "AVAILABLE"


def test_pidgin_e_dey_is_available():
    r = extract_vendor_response("e dey")
    assert r.status == "AVAILABLE"


def test_pidgin_i_fit_source_am_is_can_source():
    r = extract_vendor_response("I fit source am")
    assert r.status == "CAN_SOURCE"


def test_pidgin_i_go_get_am_is_can_source():
    r = extract_vendor_response("I go get am")
    assert r.status == "CAN_SOURCE"


def test_no_dey_still_unavailable_not_confused_with_e_dey():
    """Regression guard: adding 'e dey' -> AVAILABLE must not break the
    existing 'no dey' -> UNAVAILABLE classification."""
    r = extract_vendor_response("no dey")
    assert r.status == "UNAVAILABLE"


def test_alternative_still_correct_after_pidgin_additions():
    r = extract_vendor_response("128 no dey but 256gb dey 700k")
    assert r.status == "ALTERNATIVE"
    assert r.variant == "256GB"
    assert r.price == 700000


# --- Price format coverage (PRD 2.1 §3) ---

def test_naira_symbol_price_format():
    r = extract_vendor_response("Yes, ₦620,000")
    assert r.status == "AVAILABLE"
    assert r.price == 620000


def test_comma_formatted_price_no_symbol():
    r = extract_vendor_response("Yes, 620,000")
    assert r.price == 620000


def test_bare_six_digit_price():
    r = extract_vendor_response("Yes, 620000")
    assert r.price == 620000


# --- AI/heuristic reconciliation (PRD 2.1 §9) — no API key configured ---

def test_classify_without_ai_key_uses_heuristic_only():
    result, method = classify_vendor_response("Yes, 620k")
    assert method == "heuristic"
    assert result.status == "AVAILABLE"


def test_classify_ambiguous_without_ai_key_stays_unclassified():
    result, method = classify_vendor_response("Maybe.")
    assert method == "heuristic"
    assert result.status == "UNCLASSIFIED"
    assert result.review_state == "NEEDS_REVIEW"
