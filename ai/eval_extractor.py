#!/usr/bin/env python3
"""Score the buyer-message extractors on the pilot corpora.

Run from the ai/ folder, with the AI service's venv:

    Windows:  .venv\\Scripts\\python.exe eval_extractor.py
    Linux:    .venv/bin/python eval_extractor.py

It scores the free heuristic extractor and, when GEMINI_API_KEY or
ANTHROPIC_API_KEY is set (in the environment, or in ../.env.local), the LLM
extractor of that provider (LLM_PROVIDER picks one if both are set) on:

  - the 60 pilot samples (docs/pilot/buyer-message-samples.md), and
  - the 40-message held-out set (never used to tune the heuristic).

Output is scores only: no message text, no model output, never the key.
Failing messages are listed by number so they can be looked up in
ai/tests/data/. About 100 short API calls (free on Gemini's free tier, a few
cents on a paid one). The 100 messages are made-up samples, so sending them
to a free tier is fine.

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
from app import llm  # noqa: E402
from app.extraction import HeuristicExtractor, get_extractor  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
SETS = {
    "samples": os.path.join(HERE, "tests", "data", "buyer_samples.json"),
    "holdout": os.path.join(HERE, "tests", "data", "buyer_holdout.json"),
}


KEYS = ("ANTHROPIC_API_KEY", "GEMINI_API_KEY", "LLM_PROVIDER", "ANTHROPIC_MODEL", "GEMINI_MODEL")


def load_env_local() -> None:
    """Copy the LLM settings from ../.env.local into the environment (only
    those not already set), without printing anything."""
    path = os.path.join(HERE, "..", ".env.local")
    try:
        with open(path, encoding="utf-8") as f:
            for line in f:
                name, _, value = line.strip().partition("=")
                if name in KEYS and value and not os.getenv(name):
                    os.environ[name] = value.strip().strip('"').strip("'")
    except OSError:
        pass


class Counting:
    """Wraps the LLM extractor: on any API failure, falls back to the
    heuristic exactly as the service does, and counts it."""

    def __init__(self):
        self.ai, self.h = get_extractor(), HeuristicExtractor()
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

    if not args.heuristic_only:
        load_env_local()
    os.environ.pop("EXTRACTOR", None)
    provider = None if args.heuristic_only else llm.provider()
    sets = list(SETS) if args.set == "both" else [args.set]
    ai = Counting() if provider else None

    for name in sets:
        corpus = load_corpus(SETS[name])
        print(f"\n== {name} ({len(corpus)} messages)")
        report("heuristic", score(HeuristicExtractor().extract, corpus))
        if ai:
            report(provider, score(ai, corpus))

    if ai:
        avg = ai.seconds / ai.calls if ai.calls else 0
        print(f"\nProvider: {provider}   model: {llm.model(provider)}   calls: {ai.calls}   API failures (fell back): {ai.fallbacks}   "
              f"avg {avg:.1f}s per message")
        if ai.fallbacks:
            print("Some calls failed: check the key, the model name (GEMINI_MODEL / ANTHROPIC_MODEL), "
                  "the free-tier rate limit and the network.")
    elif not args.heuristic_only:
        print("\nNo GEMINI_API_KEY or ANTHROPIC_API_KEY found (environment or ../.env.local): heuristic only.")


if __name__ == "__main__":
    main()
