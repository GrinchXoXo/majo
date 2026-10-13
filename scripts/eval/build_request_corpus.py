#!/usr/bin/env python3
"""Attach the REAL heuristic extractor's output to every corpus message, so
the Go policy is evaluated on what the interpreter actually produces (not a
hand-written mock). Deterministic. Usage (from the repo root):

    python3 scripts/eval/build_request_corpus.py

Writes backend/internal/marketplace/testdata/request_corpus.json.
"""
import dataclasses
import json
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "ai"))
from app.extraction import HeuristicExtractor  # noqa: E402

src = json.load(open(os.path.join(ROOT, "scripts/eval/request_corpus.source.json")))
h = HeuristicExtractor()
for case in src["first_messages"] + src["answers"]:
    case["extraction"] = dataclasses.asdict(h.extract(case["text"]))
src["extractor"] = "HeuristicExtractor (ai/app/extraction.py)"
out = os.path.join(ROOT, "backend/internal/marketplace/testdata/request_corpus.json")
with open(out, "w") as f:
    json.dump(src, f, indent=1, ensure_ascii=False)
    f.write("\n")
print("wrote", out, len(src["first_messages"]), "messages,", len(src["answers"]), "answers")
