# Reuse audit R0 + R1: implementation report

| | |
|---|---|
| Date | 13 October 2026 |
| Where | Claude's cloud sandbox, branch `phase-4a.1-media-ingestion`, on top of `930b1eb` |
| Local repository | **Untouched.** Nothing has been applied to `C:\Users\HomePC\Downloads\majo\middleman`. |
| Scope | Phases R0 (hygiene) and R1 (control-plane hardening) of `docs/architecture/open-source-reuse-audit.md` §12. No commerce-loop, payment, autonomy-policy or infrastructure change beyond what is listed here. |

## 1. What was done

### R0: security hygiene and dependency fixes

| Item | Result |
|---|---|
| **Secret scanning (Gitleaks 8.30.1)** | Git history (32 commits, all branches): no leaks. The default rules do **not** catch a PostgreSQL URL with a password (e.g. a Neon connection string), so `.gitleaks.toml` adds a rule for it. `.githooks/pre-commit` blocks commits with secrets; it was verified on a throwaway clone to refuse a Neon-style URL. |
| **Dashboard: Next.js 14.2.15 → 16.4.0, React 18 → 19.3** | `npm audit`: 1 critical + 2 high → **0**. Next 14 had advisories fixed only in 15.x/16.x. Next 16 passes route params as a Promise; the discovery batch page was fixed to use `useParams()`. `next-env.d.ts` is now generated (git-ignored), and the `lint` script (removed in Next 16) was dropped. |
| **AI service pins** | FastAPI 0.143 / Starlette 1.8 / Uvicorn 0.54 / Pydantic 2.14 / httpx 0.28 / pytest 9.1. `pip-audit`: 16 known vulnerabilities → **0**. |
| **Go 1.22 → 1.26** | `go.mod` declares `go 1.26.0` and `toolchain go1.26.9`; the Docker build uses `golang:1.26`. Go 1.22 has been unsupported since 11 Feb 2025. `govulncheck`: 36 reachable standard-library vulnerabilities (measured with go1.24.7; the old go1.22 image was worse) → **0**. |
| **Dockerfiles (Trivy 0.75.0, `trivy config`)** | 7 findings → **0**. All three images now run as non-root, have a HEALTHCHECK, and the backend's distroless base is pinned to `:nonroot` (it was implicit `:latest`). The backend binary gains a `server healthcheck` subcommand, because distroless has no shell or curl. The dashboard image uses `npm ci`. `dashboard/public/.gitkeep` fixes a pre-existing build break: a fresh clone had no `public/`, which the Dockerfile copies. |
| **One security gate** | `scripts/security/run_security_checks.sh` runs every check above. A tool that is not installed is reported as SKIPPED. Pinned versions and install notes: `docs/security/security-checks.md`. |
| **Bruno collection** | `tools/bruno`: 20 requests, 22 tests. `bru run -r` passes against the sandbox backend. The token is a secret variable, never stored in the files. |
| **Mermaid diagrams** | `docs/architecture/diagrams.md` has five diagrams: request states, vendor-request lifecycle, recovery decision, failure handling, operator access. All five render with mermaid-cli. While drawing them, the request diagram was corrected against the code: correcting a CONFIRMED request keeps it CONFIRMED. |

### R1: control-plane hardening

| Item | Result |
|---|---|
| **Every `/api` route behind the guard** | 31 existing GET routes plus 4 new ones need a credential: VIEWER or OPERATOR for reads, OPERATOR for every change. This resolves decision J-3 of the hardening report. `/health`, `/ready` and the signed webhook stay outside the guard. |
| **Named operators** | Migration `0016_operator_access.sql` (table `operators`, hash-only tokens) and `cmd/operatorctl` (`create`, `list`, `revoke`). The operator's name and ID are now recorded on corrections, confirmations, associations, curation, vendor changes, exception resolution, holds and discovery batches. The shared `OPERATOR_API_TOKEN` stays as the break-glass credential. Revocation is immediate. |
| **CORS allow-list** | `CORS_ALLOWED_ORIGINS` (default `DASHBOARD_URL`) replaces `Access-Control-Allow-Origin: *`. Preflights from other origins get `403`. |
| **Read views** | `GET /api/events` (timeline, filters, operator names), `/api/events/types`, `/api/jobs` (no payloads), `/api/jobs/summary` (backlog, failures, oldest overdue), and `?limit=&offset=` on `/api/requests`. All return `X-Total-Count`. |
| **Dashboard** | TanStack Table v9 (`components/DataTable.tsx`). New **Events** page (it was a placeholder) and new **Jobs** page. The requests list is paginated and filterable. One token box in the navigation bar replaces the separate copies on 8 pages. The dashboard funnel and requests list now render in the browser, so the token never reaches the Next.js server. |
| **Error-tracking pilot (GlitchTip-compatible)** | `backend/internal/errortrack` (sentry-go v0.50.0) and `ai/app/errortrack.py` (sentry-sdk 2.72.0). Off unless `SENTRY_DSN` is set. When on: ERROR log records and handler panics are reported. Reports are scrubbed: phone-like numbers, bearer and operator tokens and e-mails are masked; request bodies, locals and source lines are removed. HTTP handler panics are now recovered with a JSON 500 (previously net/http dropped the connection). `docker-compose.yml` has an `observability` profile running GlitchTip 6 on PostgreSQL only; it is **not** started by default. |
| **Playwright smoke tests** | `dashboard/e2e`: 5 tests (token gate, paginated requests and sort, timeline actor filter, jobs summary, pause/resume). All pass against Next 16. |
| **New E2E suite `access`** | 34 checks: all 35 GET `/api` routes refuse a missing or wrong token; VIEWER read/403; OPERATOR named in the event log; revocation; CORS; pagination; timeline and job views. |

## 2. Test results (exit codes)

| Check | Before (`930b1eb`) | After |
|---|---|---|
| `go vet ./...` | 0 | 0 |
| `go test -count=1 ./...` (with local Postgres) | 0: 460 pass, 0 fail, 0 skip | 0: 470 pass, 0 fail, 0 skip (+10 new tests) |
| `go test -race ./...` | 0: 23 packages | 0: 24 packages |
| pytest | 0: 48 passed | 0: 51 passed (+3) |
| `tsc --noEmit` / `next build` | 0 / 0 | 0 / 0 (Next 16.4.0) |
| E2E `run_regression.sh` | 0: 15 suites, all pass (389 s) | 0: 16 suites, all pass (376 s): the 15 previous suites with unchanged check counts, plus `access` (34 checks) |
| Playwright smoke (`dashboard/e2e`) | n/a | 0: 5 passed |
| Bruno collection (`bru run -r`) | n/a | 0: 20 requests, 22/22 tests |
| Security gate | n/a (findings above) | 0: gitleaks, govulncheck, npm audit, pip-audit and trivy config all PASS |

Go numbers for "after" are with go1.26.9. The baseline used go1.24.7.

**Expectations changed (none weakened):**

1. `listing_curation.py`: the UI check "publish without a token shows an error" became "without a token the detail shows an authorization error and no Publish button". The page can no longer be read at all without a token, which is stricter. The API-level 401 on publish is still checked.
2. `run_regression.sh` builds `operatorctl` and runs the new `access` suite.

## 3. NOT RUN, and why

| Item | Reason |
|---|---|
| GlitchTip server itself | The sandbox has no Docker daemon or registry access. The SDK side is tested against a fake Sentry-protocol endpoint, and `docker compose config` validates the profile. |
| Testcontainers pilot | Needs Docker; it could not be verified here, so it was not added. Do it in the local session. |
| Docker image builds | No Docker daemon. The Dockerfiles were changed and checked with `trivy config`, but not built. |
| Trivy image and filesystem CVE scans | The Trivy vulnerability database host (ghcr.io) is unreachable. Only `trivy config` was run. |
| Live WhatsApp, LLM extractor, load tests | Out of scope, as in earlier phases. |

## 4. How the sandbox obtained Go modules

`proxy.golang.org`, `sum.golang.org` and `vuln.go.dev` are blocked from the sandbox; GitHub is not.

- **Modules.** New modules (sentry-go and its graph) were built into a local `GOPROXY=file://` from the projects' GitHub mirrors, using `golang.org/x/mod/zip`.
- **Hash check.** The tool reproduced the existing `go.sum` hashes for `google/uuid` and `lib/pq` exactly. It also reproduced the well-known hashes of `gopkg.in/yaml.v3 v3.0.1` and `pkg/errors v0.9.1`.
- **Hash check on your machine.** The first `go mod download` there fetches from `proxy.golang.org` and compares against `go.sum`. Any mismatch is reported as a security error, so the hashes are checked independently there.
- **Vulnerability database.** govulncheck used a database built from the `golang/vulndb` repository (2026-10-10), because vuln.go.dev is unreachable.
- **Go 1.26.9.** It was built from the official source tag, because the binary download host is unreachable.

## 5. What you need to do differently

- **Every dashboard page now needs a token.**
  - Create one per person: `DATABASE_URL=... go run ./cmd/operatorctl create -name "<name>" -role OPERATOR`.
  - Or use `OPERATOR_API_TOKEN`. Enter it in the token box at the top right.
- **Go 1.26 or later locally.** With `GOTOOLCHAIN=auto` (the default), Go downloads 1.26.9 automatically.
- **Docker.** If you already have a `media_data` volume created by the old root image, run `chown -R 65532:65532` on its contents once, because the backend now runs as uid 65532.
- **Dashboard on another origin.** Set `CORS_ALLOWED_ORIGINS` to the dashboard URL(s).
- **Secret hook.** Optional but recommended: run `git config core.hooksPath .githooks` once per clone, with gitleaks installed.
- **Migration 0016 runs automatically** on server start. As always, use a local disposable database, never Neon, for testing.

## 6. Configuration and migrations

- **Migrations:** `0016_operator_access.sql` (table `operators`; index `idx_events_created_at`).
- **New environment variables** (all optional, safe defaults):
  - `CORS_ALLOWED_ORIGINS`
  - `SENTRY_DSN`, `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE` (backend and AI service)
  - Compose: `BACKEND_SENTRY_DSN`, `AI_SENTRY_DSN`, `GLITCHTIP_*`
- **New dependencies:**
  - Go: `github.com/getsentry/sentry-go v0.50.0` (plus `golang.org/x/sys`, `golang.org/x/text`)
  - Python: `sentry-sdk==2.72.0`
  - npm: `@tanstack/react-table 9.3.1`; `@playwright/test 1.64.0` (dev)

## 7. Assumptions

- One small server plus managed Postgres is still the deployment model. The GlitchTip profile is optional and off by default.
- Operators are trusted staff. Two roles (VIEWER, OPERATOR) are enough until there are more than about three operators.
- Bearer tokens in `sessionStorage` are acceptable for an internal control plane. A login/SSO flow stays deferred.
- Routing, offer delivery, selection and clarification events keep actor type OPERATOR without an operator ID: those services do not take one yet. Changing them was outside this phase.
- React 19 and Next 16 are acceptable. All dashboard pages are client components or simple server components, and they were exercised by the UI suites and the Playwright smoke tests.
- Python 3.12 (Docker image) and Python 3.13 (sandbox tests) both work with the new pins.
- Error reports are acceptable to send to a self-hosted GlitchTip once scrubbed. They must not go to a third-party processor until data-protection obligations (NDPA 2023) are checked.
