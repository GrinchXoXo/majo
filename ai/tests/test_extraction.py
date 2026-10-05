"""
Extraction quality tests using the PRD 1 §36 test dataset.
These are not exhaustive benchmarks (per PRD 1 §37, that's not required
for Phase 1) — they verify the heuristic extractor gets the obviously
correct answer on realistic Nigerian WhatsApp messages, and critically,
that it never invents information it wasn't given (PRD 1 §17).
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app.extraction import HeuristicExtractor

extractor = HeuristicExtractor()


def test_full_request_extracts_all_fields():
    result = extractor.extract(
        "I need an iPhone 13, preferably 128gb, used is fine, around 400k, in Benin."
    )
    assert result.product == "iphone 13"
    assert result.variant == "128GB"
    assert result.budget_max == 400000
    assert result.location == "Benin City"
    assert result.condition == "used_or_new"


def test_budget_range_extraction():
    result = extractor.extract("Need iphone 13 128gb, around 350-400k. Im in Benin.")
    assert result.budget_min == 350000
    assert result.budget_max == 400000


def test_ambiguous_laptop_does_not_invent_fields():
    """PRD 1 §17: 'I need a laptop' must extract product=laptop and
    nothing else — no invented brand, budget, or condition."""
    result = extractor.extract("I need a laptop.")
    assert result.product == "laptop"
    assert result.budget_min is None
    assert result.budget_max is None
    assert result.condition is None
    assert result.location is None


def test_vague_request_extracts_nothing_confidently():
    result = extractor.extract("find me something cheap")
    assert result.product is None
    assert result.confidence == 0.0


def test_quantity_word_form():
    result = extractor.extract("I need two office chairs in Benin")
    assert result.quantity == 2
    assert result.product == "chair"
    assert result.location == "Benin City"


def test_under_budget_pattern():
    result = extractor.extract("looking for AirPods under 150k")
    assert result.product == "airpods"
    assert result.budget_max == 150000
    assert result.budget_min is None


def test_urgency_detection():
    result = extractor.extract("I need one asap")
    assert result.urgency == "urgent"
