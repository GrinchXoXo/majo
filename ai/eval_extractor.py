#!/usr/bin/env python3
"""Score the buyer-message extractors on the pilot corpora.

Run from the ai/ folder, with the AI service's venv:

    Windows:  .venv\\Scripts\\python.exe eval_extractor.py
    Linux:    .venv/bin/python eval_extractor.py

It scores the free heuristic extractor and, when ANTHROPIC_API_KEY is set
(in the environment, or in ../.env.local), the Anthropic extractor on:

  - the 60 pilot samples (docs/pilot/buyer-message-samples.md), and
  - the 40-message held-out set (never used to tune the heuristic).

Output is scores only: no message text, no model output, never the key.
Failing messages are listed by number so they can be looked up in
ai/tests/data/. About 100 short API calls (a few cents with Haiku).

Options:
    --heuristic-only    do not call the API
    --set samples|holdout|both   (default: both)
"""
import argparse
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from app.corpus_eval import load_corpus, score  # noqa: E402
from app.extraction import AnthropicExtractor, HeuristicExtractor, anthropic_model  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
SETS = {
    "samples": os.path.join(HERE, "tests", "data", "buyer_samples.json"),
    "holdout": os.path.join(HERE, "tests", "data", "buyer_holdout.json"),
}


def key_from_env_local() -> str:
    """Read ANTHROPIC_API_KEY from ../.env.local without printing anything."""
    path = os.path.join(HERE, "..", ".env.local")
    try:
        with open(path, encoding="utf-8") as f:
            for line in f:
                if line.strip().startswith("ANTHROPIC_API_KEY="):
                    return line.split("=", 1)[1].strip().strip('"').strip("'")
    except OSError:
        pass
    return ""


class Counting:
    """Wraps the Anthropic extractor: on any API failure, falls back to the
    heuristic exactly as the service does, and counts it."""

    def __init__(self, key):
        self.ai, self.h = AnthropicExtractor(key), HeuristicExtractor()
        self.fallbacks, self.calls, self.seconds = 0, 0, 0.0

    def __call__(self, text):
        self.calls += 1
        t0 = time.monotonic()
        try:
            return self.ai.extract(text)
        except Exception:  # noqa: BLE001 - mirrors main.py: never lose the message
            self.fallbacks += 1
            return self.h.extract(text)
        finally:
            self.seconds += time.monotonic() - t0


def report(name, s):
    print(f"  {name:10s} {s.correct:3d}/{s.total} ({s.pct}%)   invented budgets: {s.invented_budgets}   "
          f"requests from small talk/vague: {s.false_requests}")
    cats = "  ".join(f"{c} {ok}/{n}" for c, (ok, n) in sorted(s.by_category.items()))
    print(f"             by category: {cats}")
    failing = [str(r.id) for r in s.rows if not r.correct]
    print(f"             failing #: {', '.join(failing) if failing else 'none'}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--heuristic-only", action="store_true")
    ap.add_argument("--set", choices=["samples", "holdout", "both"], default="both")
    args = ap.parse_args()

    key = "" if args.heuristic_only else (os.getenv("ANTHROPIC_API_KEY") or key_from_env_local())
    sets = list(SETS) if args.set == "both" else [args.set]
    ai = Counting(key) if key else None

    for name in sets:
        corpus = load_corpus(SETS[name])
        print(f"\n== {name} ({len(corpus)} messages)")
        report("heuristic", score(HeuristicExtractor().extract, corpus))
        if ai:
            report("anthropic", score(ai, corpus))

    if ai:
        avg = ai.seconds / ai.calls if ai.calls else 0
        print(f"\nModel: {anthropic_model()}   calls: {ai.calls}   API failures (fell back): {ai.fallbacks}   "
              f"avg {avg:.1f}s per message")
        if ai.fallbacks:
            print("Some calls failed: check the key, the model name (ANTHROPIC_MODEL) and the network.")
    elif not args.heuristic_only:
        print("\nNo ANTHROPIC_API_KEY found (environment or ../.env.local): heuristic only.")


if __name__ == "__main__":
    main()
