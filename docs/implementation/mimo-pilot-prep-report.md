# Majo pilot-prep report (mimo/pilot-prep)

Branch: `mimo/pilot-prep` (created from `phase-4c-hardening` after the
`mimo/housekeeping` merge). Not merged, not pushed.

## Part A — finish and merge mimo/housekeeping

| Step | Status | Detail |
|------|--------|--------|
| A1. Bruno vendorId fix | done | Added `script:post-response` to `tools/bruno/04-autonomy/vendors.bru`, same `bru.setVar` style as `list-requests-page.bru`: stores `res.getBody()[0].id` as `vendorId` only when the list is a non-empty array. Commit `2a78cf8 "Bruno: set vendorId from vendor list"`. |
| A2. AI venv packaging upgrade | done | `pip 24.0 → 26.2.1`, `setuptools 65.5.0 → 84.0.0`. `pip_audit -r requirements.txt`: **`No known vulnerabilities found`** (exit 0). `pytest -q`: **`51 passed, 1 warning`** (exit 0). Local venv only — nothing committed. |
| A3. Merge | done | `git merge --ff-only mimo/housekeeping` succeeded (fast-forward `2cf4b3b..2a78cf8`, 39 files). Not pushed. |
| A4. Stop services | done | Killed the listeners for :3000 (dashboard), :8080 (backend), :8000 (AI). Confirmation below. |

### A3: `git log --oneline -8` (after merge)

```
2a78cf8 Bruno: set vendorId from vendor list
f815509 Housekeeping report: mimo/housekeeping branch
607ebb8 Bruno: cover more read routes
7c499be Dashboard: visual polish (no behaviour change)
14bcec3 README: local run, operator tokens, tests, security checks
6609b92 Rename remaining Middleman references to Majo (text only)
9530944 Repo: normalise line endings (.gitattributes)
2cf4b3b Reuse audit R0+R1: docs, registry, report and evidence
```

`git status --short` after the merge: clean (no output).

### A4: netstat confirmation (after stopping services)

```
no listeners on 8000/8080/3000
```

## Part B — pilot preparation documents

All files under `docs/pilot/` only; nothing outside `docs/pilot/` was
changed. Committed together as `d9fb3dd "Pilot prep: onboarding, buyer
guide, privacy draft, runbook draft, WhatsApp checklist"`.

Files created:

- `docs/pilot/vendor-onboarding.md` — vendor invite message, how it works,
  rules, details the owner needs
- `docs/pilot/buyer-guide.md` — buyer invite message, 10-question FAQ
  (payment goes directly to the vendor during the pilot)
- `docs/pilot/privacy-notice-draft.md` — draft privacy notice (NDPA 2023
  mentioned, no legal advice)
- `docs/pilot/incident-runbook-draft.md` — 7 incident sections, based only
  on repo docs/code (`/health` `/ready`, Autonomy page, operatorctl,
  security checks, pause/resume API)
- `docs/pilot/whatsapp-setup-checklist.md` — Meta setup checklist, WhatsApp
  env var names from `config.go` (names only), template findings

### Every TODO(owner) left, as one list

1. `buyer-guide.md` — contact details for buyer questions/complaints.
2. `privacy-notice-draft.md` — review the notice before use (NDPA
   mention; not legal advice).
3. `privacy-notice-draft.md` — retention period (how long requests and
   messages are kept).
4. `privacy-notice-draft.md` — how deletion requests are handled and how
   long they take.
5. `privacy-notice-draft.md` — contact name/number/email for privacy
   questions.
6. `incident-runbook-draft.md` §1 — production start/restart command and
   where the backend runs.
7. `incident-runbook-draft.md` §2 — exact Neon console steps for checking
  /waking the database.
8. `incident-runbook-draft.md` §4 — how autonomy flags are changed in
   production (and whether a global pause is needed for the pilot).
9. `incident-runbook-draft.md` §5 — where the server environment is
   managed (for rotating `OPERATOR_API_TOKEN`).
10. `incident-runbook-draft.md` §6 — exact Neon console steps to rotate a
    leaked database password.
11. `incident-runbook-draft.md` §7 — Neon restore steps (whole section).
12. `whatsapp-setup-checklist.md` — submit the discovery template for
    approval in Meta and confirm its final name for
    `DISCOVERY_TEMPLATE_NAME`.
13. `whatsapp-setup-checklist.md` — decide whether core buyer/vendor
    messaging also needs an approved template (none is wired today).

## Uncertainties / notes

- The runbook deliberately invents no steps where the docs are silent
  (production process management, Neon console operations, restore) —
  each is a TODO(owner) instead.
- Template research: the only concrete template name in the repo is
  `majo_finds`, and it appears only in test fixtures. Production template
  names come from `DISCOVERY_TEMPLATE_NAME` (empty by default); core
  messaging sends no templates at all (`docs/architecture/whatsapp.md`).
- Autonomy flags are shown on the dashboard but cannot be toggled there;
  they are environment-driven, so "pausing automation" in the pilot means
  per-request pause/resume plus exception handling (TODO(owner) for any
  global switch).
- Services were stopped before Part B; nothing is left running.
