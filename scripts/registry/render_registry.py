#!/usr/bin/env python3
"""Render docs/registry/components.json into docs/registry/COMPONENT_REGISTRY.md.

components.json is the source of truth for Majo's component registry: every
third-party product that is installed in the repository or has been
evaluated. Edit the JSON, then re-run:

    python3 scripts/registry/render_registry.py

Weighted score (decision aid only, out of 100) = sum(weight% x score) / 5,
with each criterion scored 1-5.
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SRC = ROOT / "docs/registry/components.json"
OUT = ROOT / "docs/registry/COMPONENT_REGISTRY.md"

WEIGHTS = {  # percent; must sum to 100
    "need": 20, "compat": 15, "maint": 10, "licence": 10,
    "deploy": 15, "security": 10, "effort": 15, "arch": 5,
}
LABELS = {
    "need": "Need", "compat": "Compat", "maint": "Maint", "licence": "Licence",
    "deploy": "Deploy/cost", "security": "Security", "effort": "Effort saved", "arch": "Architecture",
}
DECISIONS = ["ADOPT NOW", "PILOT", "REUSE COMPONENTS", "BUILD NATIVELY", "DEFER", "REJECT"]


def weighted(scores):
    if not scores:
        return None
    assert sum(WEIGHTS.values()) == 100
    for k, v in scores.items():
        if not 1 <= v <= 5:
            raise ValueError(f"score {k}={v} out of range")
    return round(sum(WEIGHTS[k] * scores[k] for k in WEIGHTS) / 5, 1)


def cell(s):
    return str(s).replace("|", "\\|").replace("\n", " ")


def main():
    entries = json.loads(SRC.read_text())
    for e in entries:
        e["weighted"] = weighted(e.get("scores"))
    installed = [e for e in entries if e["status"] == "INSTALLED"]
    evaluated = [e for e in entries if e["status"] != "INSTALLED"]

    out = []
    w = out.append
    w("# Majo component registry")
    w("")
    w("Generated from `docs/registry/components.json` by `scripts/registry/render_registry.py`. "
      "**Edit the JSON, not this file.** Consult this registry before building a new capability or adding a dependency.")
    w("")
    w("**Status** distinguishes what is actually in the repository (`INSTALLED`) from what was only assessed (`EVALUATED`). "
      "An `EVALUATED` entry — whatever its decision — is **not integrated**.")
    w("")
    w("Weighted score: " + ", ".join(f"{LABELS[k]} {v}%" for k, v in WEIGHTS.items()) +
      ". Each criterion is scored 1–5; the total is out of 100. Scores are decision aids, not decisions.")
    w("")
    w("## 1. Installed in the repository today")
    w("")
    w("| Component | Licence | Where | Decision | Note |")
    w("|---|---|---|---|---|")
    for e in installed:
        w(f"| {cell(e['product'])} | {cell(e['licence'])} | {cell(e['hosting'])} | {e['decision']} | {cell(e['reason'])} {cell(e['security_implications']) if e['security_implications'] not in ('None', '-') else ''} |")
    w("")
    w("No other third-party product, service or library is integrated. Everything below is **evaluated or planned only — not integrated**.")
    w("")
    w("## 2. Evaluated: decisions at a glance")
    w("")
    w("| Decision | Products |")
    w("|---|---|")
    for d in DECISIONS:
        names = [f"{e['product']} ({e['weighted']})" if e["weighted"] is not None else e["product"]
                 for e in sorted(evaluated, key=lambda x: -(x["weighted"] or 0)) if e["decision"].startswith(d)]
        if names:
            w(f"| **{d}** | {', '.join(names)} |")
    w("")
    w("## 3. Scores")
    w("")
    w("| Product | Category | " + " | ".join(LABELS[k] for k in WEIGHTS) + " | **Weighted** | Decision |")
    w("|---|---|" + "---|" * len(WEIGHTS) + "---|---|")
    for e in sorted(evaluated, key=lambda x: -(x["weighted"] or 0)):
        if not e.get("scores"):
            continue
        s = e["scores"]
        w(f"| {cell(e['product'])} | {cell(e['category'])} | " + " | ".join(str(s[k]) for k in WEIGHTS) +
          f" | **{e['weighted']}** | {e['decision']} |")
    w("")
    w("## 4. Entries")
    w("")
    cat = None
    for e in sorted(evaluated, key=lambda x: (x["category"], -(x["weighted"] or 0))):
        if e["category"] != cat:
            cat = e["category"]
            w(f"### {cat}")
            w("")
        m = e["maintenance"]
        maint = f"last commit {m['last_commit']}" if m.get("last_commit") else "n/a"
        if m.get("note"):
            maint += f"; {m['note']}"
        w(f"#### {e['product']} — {e['decision']}  `{e['status']}`")
        w("")
        rows = [
            ("Capability", e["capability"]),
            ("Repository", e["repository"]),
            ("Licence", e["licence"]),
            ("Open-source edition limits", e["oss_edition_limits"]),
            ("Maintenance (checked " + m["checked"] + ")", maint),
            ("Overlaps with Majo", e["overlaps_with_majo"]),
            ("Integration model", e["integration_model"]),
            ("Data owner", e["data_owner"]),
            ("Auth requirements", e["auth_requirements"]),
            ("Hosting", e["hosting"]),
            ("Resource/maintenance burden", e["resource_and_maintenance_burden"]),
            ("Integration complexity", e["integration_complexity"]),
            ("Security", e["security_implications"]),
            ("Reason", e["reason"]),
            ("Reconsider when", e["reconsider_when"]),
            ("Weighted score", e["weighted"] if e["weighted"] is not None else "not scored"),
        ]
        w("| Field | Value |")
        w("|---|---|")
        for k, v in rows:
            w(f"| {k} | {cell(v)} |")
        w("")
    OUT.write_text("\n".join(out) + "\n")
    print(f"wrote {OUT.relative_to(ROOT)}: {len(installed)} installed, {len(evaluated)} evaluated")
    return 0


if __name__ == "__main__":
    sys.exit(main())
