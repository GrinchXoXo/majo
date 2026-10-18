# Majo session handoff, 17–18 October 2026

Read this first in any new session. It continues
`session-handoff-2026-10-14.md`.

## Where the code is

- **Branch `pilot-batch-1`** in the cloud sandbox, 12 commits on top of
  `phase-4c-hardening` (`8c53958`). Head: see `git log`.
- **Not on GitHub.** The sandbox has no push access to `GrinchXoXo/majo`.
  The branch is delivered as a git bundle (`pilot-batch-1.bundle`) and
  prerequisites `8c53958`. To bring it in (only when Godyes asks), Mimo
  runs from the repo root:

  ```
  git fetch <path-to>/pilot-batch-1.bundle pilot-batch-1:pilot-batch-1
  git log --oneline -3 pilot-batch-1
  ```

  This creates the branch without touching `phase-4c-hardening`. After
  review: `git push origin pilot-batch-1`. A new cloud session can then
  clone it.

## What this session did

| # | Item | Commit | Notes |
|---|---|---|---|
| 1 | Headcount ≠ price; the 60 samples as tests; 40 held-out samples | `63e140b` | Heuristic: 60/60 on the tuned set, 30/40 on the held-out set, 0 invented budgets |
| 2 | AI extractor configurable (`ANTHROPIC_MODEL`, default `claude-haiku-5-5`); eval script | `63e140b` | `ai/eval_extractor.py`; prints scores only |
| 3 | Vendor request template outside the 24-hour window | `d61dc68` | Wording for Meta: `docs/pilot/whatsapp-templates.md` |
| 4 | Global automation pause | `e219b1d` | Dashboard → Autonomy; also `majo.sh pause` |
| 5 | Introduction, deal follow-up, ratings, free-deal counter | `6353dcb` | Dashboard → Deals |
| 6 | Production deploy for one small server | `c9a35f6` | `deploy/`, `docs/operations/deployment.md` |
| 7 | Tone layer, off by default | `2aa6317`, `a2501ba` | `docs/architecture/tone-layer.md`; run shadow mode first |
| 8 | Monetization scaffold, all off | `bf9dbd6`, `a2501ba` | `docs/architecture/ledger.md` |
| 01a | Code audit (12 blocks), plus pilot fixes | `0dc7084`, `5ad80d9` | `docs/audits/01a-code-audit.md` |
| Review | Independent review and fixes | `a2501ba`, `470e849` | Addendum in the audit |

The most important audit fixes for the pilot:

- **Vendor numbers** are normalised. Before, a vendor typed as `0803…` or
  `+234…` was never recognised, so their replies became buyer requests.
  Migration `0020` fixes existing rows.
- **New vendors get requests by area.** Before, only an identical location
  string worked, so service areas never routed. Place names now match more
  loosely, and the route floor is 45.
- **Vendor STOP/START**, as the invite promises.
- **Fair rotation** among equally good vendors.
- **A Vendors edit form** (categories, location, service areas, status).

## Verification (18 October, sandbox)

- Go: `go vet` clean; **440 tests in 31 packages pass**, including the
  Postgres integration tests. Fuzzing (tone masking round trip, 2.5 min
  total) passed.
- Python: **159 pytest pass.**
- End to end: **16 runs in the regression harness, 804 checks, all pass**
  (fake Graph API, real binary, fresh database per suite). One earlier run
  of `disc_dashboard` failed once on a UI timing check; it passed on the
  re-run. Watch for it.
- Dashboard: `tsc` clean; **Playwright 7/7**, including the new vendor
  onboarding test.
- Deploy: `docker compose config` is valid; Caddy validated and ran in
  front of the real services; `majo.sh check/pause/resume` and the backup
  commands were exercised.
- **Not run:** Docker image builds (no Docker daemon in the sandbox), real
  Let's Encrypt, real WhatsApp and real Anthropic calls (no keys in the
  sandbox), Gitleaks/Trivy (scanner databases not reachable).

Rebuilding the regression harness in a new sandbox:

- Postgres 16 on port 5433 with role `middleman`/`middleman`;
- `scripts/e2e/fake_graph_api.py` on port 9090;
- the AI service on port 8000;
- the dashboard on port 3000, built with
  `NEXT_PUBLIC_API_URL=http://localhost:8080`;
- `$E2E/start_backend.sh <db>`, which starts the backend against that
  database (env as in the script header) and writes `$E2E/backend.pid`;
- then `E2E=… scripts/e2e/run_regression.sh`.

## Assumptions made

1. Production database: either option. A new managed project, as the
   reuse audit suggested, or Postgres on the server; both are scripted, and
   Godyes chooses. The dev Neon is never used: `FORBIDDEN_DATABASE_HOSTS`
   enforces it.
2. Server: one Ubuntu 24.04 VPS, 2 vCPU / 4 GB, in Europe; Caddy for TLS;
   dashboard and API on one domain.
3. The tone layer may only *season* templates: add Pidgin particles, drop
   fillers, keep every other word in order. Whether that reads naturally
   enough is to be judged in shadow mode.
4. The route floor is 45 (was 50), so that a vendor in the right category
   who serves the buyer's area is contacted before posting any stock.
5. A vendor's START never re-activates them by itself. The owner re-vets
   and re-activates.
6. Amounts stay whole naira in the app; only the ledger and payments use
   kobo. Nothing converts between them yet.
7. The illustrative 4% fee on transactions is kept but labelled "not
   charged".
8. Nigerian numbers are assumed when normalising (`0…` and 10-digit
   `7/8/9…` get 234).

## Waiting on Godyes

Unchanged from the 14 October handoff, plus:

- **Before 2 November:**
  - the WhatsApp Cloud API number;
  - submit the vendor template (`docs/pilot/whatsapp-templates.md`);
  - the Anthropic key with a spend limit;
  - choose the production database option and the server, then follow
    `docs/operations/deployment.md`.
- **For vendor entry:** the campus name (it becomes
  `PILOT_DEFAULT_LOCATION`), its area names (vendor service areas), and the
  3–4 launch categories, chosen from the vocabulary in
  `docs/pilot/vendor-onboarding.md`.
- **01b decisions from the audit:**
  - how vendors are verified;
  - the data retention period (NDPA);
  - buyer blocking and a request cap before any public launch.

## Next session

1. If Godyes asks, Mimo brings `pilot-batch-1` into the local repo (prompt
   above), runs `go test ./...` and `pytest` on Windows, and builds the
   images: `docker compose -f deploy/compose.prod.yml build` with a test
   env file.
2. Pilot launch support: first deploy, Meta webhook, vendor entry, shadow
   tone review.
3. 01b → 02 once the answers above are in.
