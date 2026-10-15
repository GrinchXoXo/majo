"""
Scoring for the buyer-message corpus (ai/tests/data/buyer_samples.json).

The corpus is the 60 pilot sample messages from docs/pilot/buyer-message-samples.md,
labelled by hand. One message counts as correct when every labelled field is
right:

- request:     the product contains one of `product_any`; the budget matches
               (or is absent when none is labelled); quantity and headcount
               match when labelled.
- non_request: no product (Majo must not open a request).
- vague:       no product (Majo must ask, not guess).
- vague_soft:  no product, or a generic product from `product_any`.

The rules are deliberately strict about money: a wrong budget is worse than a
missing one, so an invented budget always fails the message.
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from typing import Callable, List, Optional

CORPUS_PATH = os.path.join(os.path.dirname(__file__), "..", "tests", "data", "buyer_samples.json")


def load_corpus(path: str = CORPUS_PATH) -> list[dict]:
    with open(path, encoding="utf-8") as f:
        return json.load(f)


@dataclass
class RowResult:
    id: int
    kind: str
    category: str
    correct: bool
    problems: List[str] = field(default_factory=list)


def _budget_ok(expected: Optional[list], bmin: Optional[int], bmax: Optional[int]) -> tuple[bool, str]:
    if expected is None:
        if bmin is None and bmax is None:
            return True, ""
        return False, f"invented budget {bmin}-{bmax}"
    lo, hi = expected
    if bmax != hi:
        return False, f"budget max {bmax} != {hi}"
    if bmin not in (None, lo, hi):
        return False, f"budget min {bmin} != {lo}"
    return True, ""


def score_row(row: dict, res) -> RowResult:
    product = (getattr(res, "product", None) or "").lower()
    problems: list[str] = []
    kind = row["kind"]
    if kind in ("non_request", "vague"):
        if product:
            problems.append(f"opened a request for {product!r}")
    elif kind == "vague_soft":
        if product and not any(p in product for p in row["product_any"]):
            problems.append(f"guessed {product!r}")
    else:
        if not product:
            problems.append("no product")
        elif not any(p in product for p in row["product_any"]):
            problems.append(f"product {product!r} not in {row['product_any']}")
        ok, why = _budget_ok(row.get("budget"), getattr(res, "budget_min", None), getattr(res, "budget_max", None))
        if not ok:
            problems.append(why)
        if row.get("quantity") is not None and getattr(res, "quantity", None) != row["quantity"]:
            problems.append(f"quantity {getattr(res, 'quantity', None)} != {row['quantity']}")
        if row.get("headcount") is not None and getattr(res, "headcount", None) != row["headcount"]:
            problems.append(f"headcount {getattr(res, 'headcount', None)} != {row['headcount']}")
    return RowResult(row["id"], kind, row["category"], not problems, problems)


@dataclass
class Score:
    correct: int
    total: int
    by_category: dict
    invented_budgets: int
    false_requests: int
    rows: List[RowResult]

    @property
    def pct(self) -> float:
        return round(100.0 * self.correct / self.total, 1) if self.total else 0.0


def score(extract: Callable[[str], object], corpus: Optional[list[dict]] = None) -> Score:
    corpus = corpus if corpus is not None else load_corpus()
    rows = [score_row(r, extract(r["message"])) for r in corpus]
    by_cat: dict = {}
    for r in rows:
        c = by_cat.setdefault(r.category, [0, 0])
        c[0] += r.correct
        c[1] += 1
    invented = sum(any(p.startswith("invented budget") for p in r.problems) for r in rows)
    false_req = sum(any(p.startswith("opened a request") or p.startswith("guessed") for p in r.problems) for r in rows)
    return Score(sum(r.correct for r in rows), len(rows), by_cat, invented, false_req, rows)


def summary(s: Score) -> str:
    lines = [f"Correct: {s.correct}/{s.total} ({s.pct}%)",
             f"Invented budgets: {s.invented_budgets}",
             f"Requests opened from non-requests/vague messages: {s.false_requests}",
             "By category:"]
    for cat, (ok, n) in sorted(s.by_category.items()):
        lines.append(f"  {cat:20s} {ok}/{n}")
    return "\n".join(lines)
