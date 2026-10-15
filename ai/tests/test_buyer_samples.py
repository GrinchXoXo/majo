"""Pilot batch: the 60 pilot buyer messages (docs/pilot/buyer-message-samples.md)
as permanent tests, plus a 40-message held-out set that was NOT used to tune
the heuristic extractor.

- Every one of the 60 samples is its own test case.
- Money and request-opening are hard rules on BOTH sets: never invent a
  budget, never open a request from small talk or a vague message.
- The held-out score has a floor so the extractor cannot quietly regress.
"""
import os

import pytest

from app.corpus_eval import load_corpus, score, score_row
from app.extraction import HeuristicExtractor, extract_supply

h = HeuristicExtractor()
SAMPLES = load_corpus()
HOLDOUT = load_corpus(os.path.join(os.path.dirname(os.path.abspath(__file__)), "data", "buyer_holdout.json"))

# Held-out score when this suite was written (2026-10-15): 30/40 (29/40 on first
# measurement; +1 after fixing "rice cooker", which adding "rice" had broken). Raise it
# when the extractor genuinely improves; never lower it.
HOLDOUT_FLOOR = 30


@pytest.mark.parametrize("row", SAMPLES, ids=[f"sample{r['id']:02d}" for r in SAMPLES])
def test_pilot_sample(row):
    res = score_row(row, h.extract(row["message"]))
    assert res.correct, f"#{row['id']} {row['message']!r}: {res.problems}"


def test_no_invented_budgets_or_false_requests_on_either_set():
    for corpus in (SAMPLES, HOLDOUT):
        s = score(h.extract, corpus)
        assert s.invented_budgets == 0, [r for r in s.rows if r.problems]
        assert s.false_requests == 0, [r for r in s.rows if r.problems]


def test_holdout_floor():
    s = score(h.extract, HOLDOUT)
    assert s.correct >= HOLDOUT_FLOOR, f"held-out {s.correct}/{s.total} fell below {HOLDOUT_FLOOR}"


# --- The headcount-as-price bug, directly -----------------------------------

@pytest.mark.parametrize("text,count", [
    ("small chops for 20 people", 20),
    ("fried rice for 50 people?", 50),
    ("catering for 30 guests", 30),
    ("cake for 15 persons", 15),
    ("we need food for 120 students", 120),
])
def test_headcount_is_never_a_budget(text, count):
    r = h.extract(text)
    assert (r.budget_min, r.budget_max) == (None, None)
    assert r.headcount == count
    assert f"for {count} people" in r.preferences
    assert r.quantity is None


def test_headcount_and_budget_together():
    r = h.extract("abeg I need small chops for 20 people on Saturday, budget 45,000")
    assert (r.headcount, r.budget_max) == (20, 45000)


@pytest.mark.parametrize("text", [
    "power bank 20,000mah",
    "43 inch tv",
    "3kg cake",
    "size 43 sneakers",
    "619 size mattress",
    "2 pieces bedsheet",
    "5 reams of paper",
])
def test_sizes_and_units_are_never_a_budget(text):
    r = h.extract(text)
    assert (r.budget_min, r.budget_max) == (None, None), text


def test_vendor_shorthand_price_still_works():
    # The bare "for 640" vendor shorthand (₦640,000) is kept for vendors.
    assert extract_supply("iphone 13 for 640").price == 640000
    assert extract_supply("2 pieces left, 15k").price == 15000


def test_hash_sign_is_naira():
    assert h.extract("tv budget #85,000").budget_max == 85000
