# Majo housekeeping report (mimo/housekeeping)

Branch: `mimo/housekeeping` (created from `phase-4c-hardening`).
Commits (`git log --oneline phase-4c-hardening..HEAD`):

```
607ebb8 Bruno: cover more read routes
7c499be Dashboard: visual polish (no behaviour change)
14bcec3 README: local run, operator tokens, tests, security checks
6609b92 Rename remaining Middleman references to Majo (text only)
9530944 Repo: normalise line endings (.gitattributes)
```

## Task status

| Task | Status | Notes |
|------|--------|-------|
| 1. Repo hygiene | done | `.gitattributes` added; `core.filemode false` set; renormalize produced no content diffs (repo was already LF-normalized; earlier status noise was only the exec bit on Windows) |
| 2. First local run | done | AI service, backend, dashboard all ran against the throwaway DB from `.env.local`; see results below |
| 3. Checks | done (report only) | One pre-existing Windows-specific Go test failure and pip-audit findings recorded; nothing fixed per instructions |
| 4. Middleman → Majo rename | done | Text-only rename of 12 files; protected identifiers (module path, log name, DB creds, test expectations, spec-doc names, protected paths) deliberately left unchanged |
| 5. README | done | Rewritten concisely: overview, prerequisites, local run via `.env.local`, operator tokens, tests, security checks, doc links |
| 6. Dashboard polish | done | Visual only under `dashboard/app` + `dashboard/components`; `lib/api.ts`, auth handling and testid/aria attributes untouched |
| 7. Bruno gaps | done | Six read-only GET requests added; no tokens in any `.bru` file |
| Report | done | This file |

## Task 2 results (first local run)

Environment loaded per instructions from `.env.local` (throwaway DB) in each
window; the main `.env` was never opened or used.

| Step | Command | Result |
|------|---------|--------|
| AI venv | `py -3.11 -m venv .venv`; `pip install -r requirements.txt` | PASS |
| AI service | `uvicorn app.main:app --port 8000` | PASS (running; reported `ai_service: ok` in `/ready`) |
| Backend | `go run ./cmd/server` (APP_ENV=development, AI_SERVICE_URL set) | PASS — log: `migrations applied successfully`; no SNI/endpoint error, so no `options=endpoint` change was needed |
| Readiness | `GET http://localhost:8080/ready` | PASS — `{"status":"ok","checks":{"ai_service":"ok","postgres":"ok"}}` |
| Operator token | `go run ./cmd/operatorctl create -name "Godyes" -role OPERATOR` | PASS — token printed once to the owner on screen only; not written to any file, log or this report |
| Dashboard | `npm install`; `npm run dev` with NEXT_PUBLIC_API_URL | PASS — /, /dashboard, /requests, /events, /jobs, /autonomy all HTTP 200 |

## Task 3 results (checks — report only)

### Go (`cd backend`)

- `go vet ./...` — **PASS** (exit 0, no output).
- `go test ./...` — **FAIL** (exit 1), one pre-existing failure, not touched
  (rule: never change a test to make it pass):

```
--- FAIL: TestLocalProvider_PutGetDeleteRoundTrip (0.02s)
    local_test.go:40: expected 0600 file mode, got -rw-rw-rw-
FAIL    github.com/middleman/backend/infrastructure/storage
```

All other packages `ok` or `[no test files]`; DB integration tests skipped as
expected (MAJO_TEST_DATABASE_URL never set). Likely cause: Windows does not
enforce Unix 0600 file modes the way the test expects.

### AI (`cd ai`)

- `pytest -q` — **PASS**: `51 passed, 1 warning in 2.18s` (warning is a
  starlette deprecation about httpx in the installed testclient).
- `pip-audit` — **FAIL** (exit 1): 20 known vulnerabilities in 2 packages,
  all in the venv's own packaging tools, not app dependencies:

```
pip 24.0        — PYSEC-2026-* (fix in 25.3–26.2)
setuptools 65.5.0 — PYSEC-2022/2025/2026-* (fix up to 83.0.0)
```

(`ai/requirements.txt` is not modifiable per the hard rules; upgrading the
venv's pip/setuptools is a local action left to the owner.)

### Dashboard (`cd dashboard`)

- `npx tsc --noEmit` — **PASS** (exit 0).
- `npm run build` — **PASS**: Next.js 16.4.0, 15 routes, no type errors.
  Cosmetic warning: Next.js noted a `package-lock.json` outside the
  dashboard directory (in the parent Downloads folder) being ignored.
- `npm audit --omit=dev` — **PASS**: `found 0 vulnerabilities`.

### Gitleaks (optional, owner-approved)

- Installed via `winget install Gitleaks.Gitleaks`.
- `gitleaks git --redact --config .gitleaks.toml .` — **PASS**:
  `26 commits scanned … no leaks found`.
- `git config core.hooksPath .githooks` — set (the pre-commit hook then
  reports gitleaks skipped because the winget PATH is not visible in
  already-open shells; the scan itself was run manually above).

## Task 4 notes (rename)

Changed 12 files (comments, docstrings, AI prompt persona text, docs title).
Deliberately unchanged, per instructions:

- Go module path `github.com/middleman/backend` (go.mod + all imports);
- log name `middleman-backend`;
- docker-compose / `.env.example` / E2E-script Postgres user/password/db
  values `middleman`;
- test expectations (`backend/tests/config_test.go` DB URLs) and the
  integration-test comment with example DB creds;
- references to the document name `Middleman_Technical_Spec_v1`;
- protected paths not modified at all: `internal/workflow`, `internal/routing`,
  `internal/autonomy`, `internal/channel`, `internal/access`,
  `httpserver/operator_auth.go`, `httpserver/cors.go`, `migrations/`,
  payments, `autonomy_metrics.go`, `go.mod`/`go.sum`;
- historical filesystem paths in older docs (e.g.
  `C:\Users\HomePC\Downloads\majo\middleman`, `/home/claude/mm/middleman`)
  left as factual records.

Verified after: `go build ./...` PASS, `npx tsc --noEmit` PASS, `pytest` 51
passed.

## Task 6 notes (dashboard)

- Per-page `<title>` added to all 13 pages; the root layout's
  `metadata.title` was removed because Next.js metadata overrides hoisted
  `<title>` elements (verified live: each page now serves its own title).
- New favicon `dashboard/app/icon.svg` (served as `/icon.svg`).
- Shared base styles in `globals.css`; DataTable empty rows render as a
  friendly dashed empty-state box; vendors page got a matching empty state.
- Events "Details" column now renders payload `key: value` lines, truncated
  (max 6 keys, 72-char values, "+N more…") — same data as before.
- Verified: `tsc --noEmit` and `npm run build` PASS; all list pages reload
  HTTP 200 with correct titles in the served HTML.

## Uncertainties / stopped items

- The pre-existing `TestLocalProvider_PutGetDeleteRoundTrip` failure
  (Windows file-mode) was reported, not fixed (rule 6).
- Detail-page titles (`/requests/[id]`, `/offers/[id]`, `/discovery/[id]`)
  are static strings; they render client-side after data loads, so they were
  not visible in plain HTTP fetches of those routes.
- The optional Playwright smoke tests (`dashboard/e2e`) were not executed
  (not part of the requested checks); the dashboard pages were verified via
  HTTP against the live dev server instead.
- Services (backend :8080, AI :8000, dashboard :3000) were left running in
  the background for the owner; stop them with Task Manager or by closing
  their windows when done.
