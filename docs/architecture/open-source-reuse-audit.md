# Majo open-source reuse audit

| | |
|---|---|
| Date | 13 October 2026 |
| Repository state | sandbox branch `phase-4a.1-media-ingestion`, inspected at `73ffb0b` |
| Scope | Evaluation and architecture only. **Nothing was installed, deployed or integrated.** No production service, commerce-loop, payment or autonomy change. |
| Registry | `docs/registry/COMPONENT_REGISTRY.md`, generated from `docs/registry/components.json` (71 entries: 9 installed — dependencies, 2 external APIs and the native metrics — plus 60 evaluated products and 2 planned native builds) |

## Implementation status (updated 13 Oct 2026)

Phases R0 and R1 were implemented in the sandbox the same day; see
`docs/implementation/reuse-audit-r0-r1-report.md`. The registry's **Status**
column is authoritative for what is installed.

| Item | Status |
|---|---|
| Gitleaks, govulncheck/npm audit/pip-audit, Trivy (config) | Installed as developer tools (`scripts/security/run_security_checks.sh`, pre-commit hook); all findings fixed |
| Next.js 14.2.15 → 16.4.0, React 19.3; Python pins; Go 1.22 → 1.26 | Done; all scanners report 0 |
| Native route protection, named operators, CORS allow-list | Done |
| TanStack Table + event timeline + jobs pages + paginated requests | Done |
| Bruno collection, Mermaid diagrams, Playwright smoke tests | Done |
| GlitchTip pilot | SDK side done and tested against a fake endpoint; GlitchTip server **not run** (sandbox has no Docker daemon) |
| Testcontainers pilot | **Not run** (no Docker daemon in the sandbox); left for the local session |
| Everything deferred or rejected | Unchanged |

## 0. The five most valuable decisions

1. **Keep the native Next.js dashboard as the only control plane. Do not adopt a low-code admin platform.**
   - Appsmith needs about 8 GB RAM.
   - ToolJet is AGPL and similarly heavy.
   - Both invite direct database writes that bypass Majo's authorization, audit and state transitions.
   - Instead, reuse **TanStack Table** inside the dashboard and build the two missing screens natively: the event timeline (a placeholder today) and the jobs/failures view.
2. **Close the security gaps first. They are real and cheap to fix.**
   - Add **Gitleaks** (secret scanning) and language-native dependency scanners (**govulncheck, npm audit, pip-audit**).
   - `npm audit` on 13 Oct 2026 already reports **1 critical and 2 high** advisories in the dashboard (Next.js 14.2.15).
   - The Go module targets **Go 1.22, unsupported since 11 Feb 2025**.
3. **Protect every `/api` route before any external tool touches Majo.** 16 data-returning GET routes are unauthenticated and return buyer data, and CORS is `*`. This is **built natively**: per-operator tokens with roles, plus a CORS allow-list. It is small, and it is a prerequisite for everything in §8.
4. **Fill the observability gap with GlitchTip (PILOT), not a monitoring stack.**
   - It gives error tracking and uptime checks through standard Sentry SDKs.
   - It needs about 512 MB RAM and can run on Postgres only.
   - Grafana, Prometheus, Loki and OpenTelemetry are deferred. Self-hosted Sentry (16 GB minimum) is rejected.
5. **Reject every commerce platform, notification platform and support inbox that would own state or the WhatsApp channel.** That covers Medusa, Saleor, Vendure, Directus, Novu, Chatwoot (for WhatsApp) and Temporal.
   - Majo's existing **durable jobs**, **deterministic matching**, **messaging layer** and **metrics** stay.
   - None of them has a missing capability that justifies replacing it.

## 1. Ranked recommendations

Effort figures are engineering estimates for one developer, not measurements.

| Rank | Capability | Recommended project | Decision | Why | Integration effort | Cost / maintenance | Main risk |
|---|---|---|---|---|---|---|---|
| 1 | Secret detection | Gitleaks | ADOPT NOW | `.env` with a Neon password exists locally; there is no CI or hook; a move to a new git repo is planned | 1–2 h (pre-commit + one history scan) | None (CLI) | Findings contain secret fragments: handle privately |
| 2 | Dependency vulnerabilities | govulncheck + npm audit + pip-audit | ADOPT NOW | Already finds a critical; govulncheck reports only reachable Go vulnerabilities | 2–4 h to script; acting on findings is separate (Next 14.2.35 patch: ~1 h; supported major: 1–3 days; Go toolchain bump: ~½ day) | None | Some Next.js fixes exist only in 15.x/16.x, so a major upgrade is unavoidable |
| 3 | Operator access control | Native: operator accounts + route protection | BUILD NATIVELY | 16 unauthenticated data reads; audit has no operator identity | 1–2 days | None | Dashboard pages that read without a token need updating together |
| 4 | Error tracking + uptime | GlitchTip | PILOT | Panics and terminal job failures are logged only; nobody is alerted | ~1 day for the pilot (E3) | One ~512 MB container + its own database | PII in error payloads; must scrub |
| 5 | Admin lists (sort, filter, page) | TanStack Table v8 | REUSE COMPONENTS | Lists have no sort, filter or paging, and the API has no paging; events/jobs screens are missing | 2–4 days including paginated read APIs and the native timeline/jobs pages | None (library) | Scope creep into a dashboard rewrite |
| 6 | Portable integration tests | Testcontainers for Go | PILOT | Postgres integration tests are NOT RUN on the owner's Windows machine | ½–1 day (E4) | Docker on dev machines | Slower test start-up |
| 7 | API exploration / onboarding | Bruno | ADOPT NOW | 58 routes, no collection or spec | ½–1 day | None (files in git) | Tokens committed by mistake: use git-ignored env files |
| 8 | Architecture diagrams | Mermaid | ADOPT NOW | State machines and failure paths are prose-only | 2–3 h | None (renders in GitHub markdown) | None |
| 9 | Container image scanning | Trivy | PILOT | OS-package vulnerabilities in images are unscanned | 2 h | None (CLI) | **March 2026 supply-chain compromise**: install pinned, verified versions only |
| 10 | Dashboard smoke tests | Playwright | PILOT | No browser tests protect the control plane through the Next.js upgrade | ~1 day (3 tests) | Browser binaries in CI/dev | Flaky tests if over-scoped |
| 11 | Ad-hoc business questions | Metabase OSS | DEFER (first to pilot later) | Useful only once real traffic exists | ½ day once triggered (E5) | One JVM container: ≥1 core / 1 GB + app database | Exposes PII unless restricted to views |
| 12 | Typo-tolerant listing search | pg_trgm | DEFER | No measured search misses yet; it is a Postgres extension, not a service | ½ day once triggered | None | Fuzzy matches must stay explainable in match reasons |

## 2. Repository audit by capability

| Capability | Existing Majo code | What works | Missing capability | Right answer |
|---|---|---|---|---|
| Admin UI | `dashboard/app/*` (12 pages: requests, offers, vendors, unmatched responses, discovery, autonomy, funnel), `lib/api.ts` (≈40 API calls), operator-token component | All operator workflows, decision logs, pause/resume, curation, discovery batches | Events page is a placeholder; no jobs/failure view; no sort, filter or pagination; inline styles (~330) | Keep it native; TanStack Table; build the missing screens natively |
| Business analytics | `GET /api/autonomy/metrics` (layered, defined denominators), `GET /api/metrics/funnel`, `internal/funnel` | Defined product metrics, served with definitions | Ad-hoc questions (vendor performance, demand by area) once real data exists | Native for defined metrics; Metabase later for ad-hoc (DEFER) |
| Error tracking / observability | `internal/logging` (slog JSON + request_id), `/health`, `/ready`; panics in job handlers recovered and logged | Structured logs, readiness checks | No alerting, no error aggregation, no uptime check; no HTTP panic middleware (net/http default) | GlitchTip (PILOT); defer metrics/log/trace stacks |
| Human exception handling | `autonomy_exceptions`, holds, message trail, `POST /resolve`, pause/resume | Exception-only operator model (0 operator steps in synthetic runs) | Email/web support channels (none exist) | Keep native; no support suite now |
| Background jobs | `internal/jobs/durable.go`, `infrastructure/postgres/job_repository.go`: `FOR UPDATE SKIP LOCKED`, leases, idempotency keys, backoff, terminal failure with event in the same transaction; 12 job types including `VENDOR_REQUEST_TIMEOUT` | Retries, deadlines, expiry and recovery, with integration tests | Job visibility (no UI); alert on terminal failure | Keep; native jobs view; GlitchTip alert on terminal failure |
| Search / matching | `internal/matching` (rule-based weights, explainable), `normalized_product LIKE` in repositories | Deterministic eligibility and scoring | Typo tolerance (not yet shown to matter) | Keep; pg_trgm when triggered |
| Media | `internal/media` (magic-byte validation: JPEG/PNG/MP4/3GP; size limits), `storage.Provider` with local disk on a Docker volume | Ingest, validate and store via durable `MEDIA_DOWNLOAD` jobs | Durable multi-host storage and backups; audio not ingested | Keep; managed S3-compatible bucket behind `storage.Provider` when multi-host; FFmpeg when voice notes are ingested |
| Authentication | `OperatorAuth`: one shared bearer token, constant-time compare | Guards all mutating routes | 16 unauthenticated data GETs; `CORS: *`; no operator identity or roles | Build natively now; IdP deferred |
| Configuration / flags | `internal/config` env flags (`AUTO_*`, discovery gates), per-request holds | Safe defaults; per-request runtime control | Global switches need a restart | Keep; a native settings table before any flag service |
| API documentation | Route list in `cmd/server/main.go`; docs in markdown | — | No OpenAPI spec, no request collection | Bruno now; Scalar after a spec exists |
| Testing / security | Go unit + Postgres integration tests (skip without `MAJO_TEST_DATABASE_URL`), race tests, pytest, `tsc`/`next build`, 15 E2E suites with a fake Graph API | Strong regression for the commerce loop | No secret scanning, no dependency scanning, no CI, no browser tests, integration tests not portable | Gitleaks, scanners (now); Testcontainers, Trivy, Playwright (pilot); ZAP once staging exists |
| Workflow visualisation | Request page shows the decision log, questions and messages | Per-request trail | A global event timeline | Native timeline; Mermaid for docs |
| Commerce | BuyerRequest → … → Transaction; `payment.Provider` interface only | The commerce loop | Payments (out of scope) | Native only |
| Notifications | `channel.Sender`, WhatsApp adapter, failure taxonomy, `pending_notices` | Classified retries, 24h-window handling | Email (no need yet) | Native; managed email provider behind an interface when needed |

**Deployment facts used:**
- `docker-compose.yml` runs four services: `postgres:16-alpine`, `ai`, `backend` (built on `golang:1.22`) and `dashboard` (Node 22). There is no CI configuration and no deployment manifest.
- The owner's development database is Neon.
- The production host is not documented. This audit assumes one small VM-class host (see Assumptions).

## 3. Existing functionality to retain (do not rebuild or replace)

- **The durable job engine and the deadline/recovery jobs.** They already provide what River or Temporal would add for Majo's needs.
- **The WhatsApp channel layer:** adapter, failure classification, pending notices, idempotent webhook handling. No notification platform or support inbox may send on the Majo number.
- **Deterministic matching, routing and eligibility.** No search engine or vector index replaces them.
- **Autonomy and funnel metrics with explicit denominators.** A BI tool may *query* them; it must not *redefine* them.
- **The operator dashboard pages,** the exception queue and pause/resume.
- **`storage.Provider` and media validation.** A future object store plugs in behind the interface.
- **Structured logging** with request IDs, `/health` and `/ready`.
- **Env-based configuration** with safe defaults.
- **The E2E harness** with the fake Graph API, and the Postgres integration tests.

## 4. Recommended dashboard architecture

The dashboard has four distinct problems, and each gets a different home:

| Problem | Home | Notes |
|---|---|---|
| Administrative interface (control plane) | **Native Next.js dashboard** | Add TanStack Table; build the event timeline and jobs/failure pages; paginated read APIs |
| Business intelligence | **Metabase, later** (separate, read-only) | Read-only role on PII-minimised views; not embedded; defined metrics stay native |
| System monitoring | **GlitchTip** (pilot) | Errors + uptime monitor on `/ready`; link from the dashboard header; Grafana only when time-series alerts are needed |
| Customer support | **Existing exception queue** | No support suite while support happens on WhatsApp through Majo |

**Answers to the brief's dashboard questions:**

1. **Which pages stay native?** All 12. None is a generic CRUD screen that a low-code tool would build better.
2. **Which missing screens are cheaper in Appsmith, ToolJet, Refine or React Admin?** None. The missing screens (event timeline, jobs/failures) are views of Majo's own state; a low-code tool would need the same paginated API plus its own auth.
3. **Which UI libraries can be reused without replacing the app?** TanStack Table now. shadcn/ui and charts are deferred: shadcn/ui needs Tailwind, which the dashboard doesn't use, and there are no time-series metrics yet. Tremor is rejected; net-new development moved to shadcn/ui.
4. **Native or Metabase for business analytics?** Defined metrics stay native. Exploratory BI goes to Metabase once real data exists.
5. **Grafana for monitoring?** Not yet. GlitchTip covers errors and uptime. Grafana (reading Postgres directly, no Prometheus) is the next step if time-series ops alerts are needed.
6. **Would Chatwoot help?** Not for WhatsApp. Its WhatsApp Cloud setup "sets up the webhook in Meta for you" and sends its own messages, which would create a second owner of the channel. Reassess only for email or web channels.
7. **Auth work before external interfaces:**
   - guard every `/api` route;
   - per-operator tokens with roles (viewer / operator / admin);
   - a CORS allow-list;
   - a read-only database role plus PII-free reporting views (before any BI);
   - DSNs and secrets kept in env, never in the repo.
8. **What fits the current deployment and cost?** Everything ranked 1–10. Only GlitchTip adds a running service (~512 MB plus its own database). The rest are libraries or dev-time CLIs.
9. **Too heavy or duplicative:**
   - too heavy: Appsmith (8 GB), self-hosted Sentry (16 GB), Chatwoot (4 GB + Redis), Temporal, OpenSearch, Superset;
   - duplicative: Novu, Medusa, Saleor, Vendure, Directus, Windmill, Asynq.
10. **The smallest useful set:** Gitleaks + govulncheck/npm audit/pip-audit + TanStack Table + Bruno + Mermaid, then a GlitchTip pilot. Plus the native auth hardening.

## 5. Missing capabilities best supplied by external projects

- **Secret detection:** Gitleaks.
- **Dependency vulnerability scanning:** govulncheck, npm audit, pip-audit.
- **Container image scanning:** Trivy, pinned.
- **Application error aggregation and uptime alerting:** GlitchTip.
- **Portable real-database integration tests:** Testcontainers for Go.
- **Browser smoke tests:** Playwright.
- **API request collection:** Bruno.
- **Table behaviour (sort, filter, paginate):** TanStack Table.
- **Diagrams in docs:** Mermaid.
- **Later, on triggers:**
  - ad-hoc BI: Metabase;
  - typo tolerance: pg_trgm;
  - object storage: a managed S3-compatible bucket (or SeaweedFS if self-hosting is required);
  - identity provider: authentik or Keycloak;
  - API reference UI: Scalar, after an OpenAPI spec exists;
  - dynamic security scan: OWASP ZAP, against staging only.

## 6. Capabilities that remain native

- **The commerce loop and its state machine:** BuyerRequest, VendorRequest, VendorResponse, Offer, selection, Transaction.
- **Request understanding, matching, routing, eligibility, recovery.**
- **WhatsApp messaging and buyer/vendor notifications.**
- **Product metrics and their definitions.**
- **Exception handling, holds, the operator decision log.**
- **Operator accounts and route protection, for now.**
- **Event timeline and jobs/failure views.**
- **A runtime settings table,** if global switches ever need changing without a restart. Consider it before any flag service.

## 7. Licensing and deployment risk assessment

This is not legal advice. Check commercial terms with counsel before any redistribution or hosted offering.

**Licences**

| Licence situation | Products | What it means for Majo |
|---|---|---|
| Permissive (MIT / Apache / BSD / PostgreSQL / MPL) | TanStack Table, Gitleaks, govulncheck, pip-audit, Trivy, Testcontainers, Playwright, Bruno, Mermaid, pg_trgm, pgvector, Keycloak, Flagsmith, SeaweedFS, ECharts, Superset, Prometheus, OTel, Temporal, Saleor; River (MPL-2.0, file-level copyleft) | No obligations beyond notices (MPL: share changes to River's own files) |
| AGPL-3.0, run as a separate unmodified service | Grafana, Loki, Metabase (core), Zammad, FreeScout, ToolJet, listmonk, Zitadel (since v3.0, March 2025), MinIO, Unleash (server source since v8; official images stay Apache-2.0) | Internal use of unmodified software creates no obligation on Majo's own code. Modifying it **and** offering it to users over a network requires publishing those modifications. |
| Open-core (paid features in enterprise directories) | Metabase, Chatwoot, authentik, GrowthBook, Novu, Medusa, React Admin, Windmill, Meilisearch (EE under BUSL-1.1), Appsmith | Check that the needed feature is in the free edition. Example: Metabase SAML/JWT/LDAP sign-in, row- and column-level permissions and full-app embedding are paid. |
| Source-available (not open source) | Sentry (FSL-1.1, Apache-2.0 after 2 years), Directus (Monospace Sustainable Core License: "Competing Use" restriction, licence-key-protected features, GPL-3.0 after 4 years) | Usable internally, but restrictions apply; one more reason both are rejected |
| GPL / LGPL | Typesense (GPL-3.0), Vendure CE (GPL-3.0), FFmpeg (LGPL/GPL by build), Paperless-ngx (GPL-3.0) | Fine as separate services or CLIs; avoid linking GPL code into Majo binaries |

**Supply chain and maintenance**

| Product | Risk |
|---|---|
| Trivy | Compromised in March 2026: the `v0.69.4` binary, Docker Hub `0.69.5`/`0.69.6` and 75 of 76 `trivy-action` tags were malicious. Install only verified versions by digest or full SHA, never by moving tag. |
| MinIO | Community edition: admin features removed (2025); no official Docker images since October 2025; maintenance-only mode since late 2025. Rejected. |
| Tremor | Last commit 2025-10-10; net-new development moved to shadcn/ui. Rejected. |
| Papercups | Maintenance mode; last commit 2022-05-10. Rejected. |
| lib/pq (installed) | Upstream is active (v1.12.3, August 2026), but Majo pins v1.10.9 (2023). River and much newer Go Postgres tooling assume pgx. |

**Data protection.** Any tool that reads Majo's database sees buyer phone numbers and message text: Metabase, Grafana, and GlitchTip through error payloads.
- Give each one a read-only role on views that exclude or mask those columns.
- Scrub error events before sending (`BeforeSend`).
- Personal data of Nigerian users falls under the Nigeria Data Protection Act 2023. Confirm the obligations (retention, processor terms) with counsel before sending data to any new processor.

**Deployment.**
- Recommended now: one new runtime service at most (GlitchTip).
- Each later service must ship with:
  - an owner;
  - a backup plan (if it stores data);
  - an update cadence;
  - a removal path (§8).

## 8. Integration rules for every external service

| Service | Purpose | Data owner | Boundary | Auth | Deploy / update | Failure mode | Backup | Removal path |
|---|---|---|---|---|---|---|---|---|
| GlitchTip (pilot) | Errors + uptime | GlitchTip owns error events; Majo state untouched | Sentry SDK, outbound only; DSN via env; empty DSN = off | GlitchTip users; DSN treated as a secret | Compose profile `observability` (not default); pinned image; monthly update | If down or unreachable, SDK send fails silently and Majo continues | Optional (error history only) | Unset the DSN; remove the SDK init (≈20 lines) |
| Metabase (deferred) | Ad-hoc BI | Majo owns the data; Metabase stores saved questions | Read-only role on a reporting schema of PII-free views | Metabase users; never the write role | Pinned image; quarterly update | If down, BI is unavailable; Majo is unaffected | Back up its app database (saved questions) | Drop the role and views; stop the container |
| Dev-time CLIs (Gitleaks, scanners, Trivy, Bruno, Playwright, Testcontainers) | Quality gates | n/a | Run on developer machines or CI only; no runtime path | n/a | Pinned versions in a script | A failing gate blocks a merge, not production | n/a | Delete the script or hook |

## 9. Proposed experiments (not run — each needs your go-ahead)

| # | Experiment | What it would establish | Pass criteria |
|---|---|---|---|
| E1 | Run Gitleaks over the sandbox repo history, and over your local repo once linked (read-only) | Whether any secret ever entered git history before the move to a new repo | Zero findings, or findings rotated and history plan agreed |
| E2 | Run `govulncheck ./...`, `npm audit`, `pip-audit`; then bump `next` to 14.2.35 on a branch and re-audit | Which vulnerabilities are reachable, and what remains after the patch-level fix (sizes the major upgrade) | A written list; patch branch builds and passes `tsc`/`next build`/E2E |
| E3 | GlitchTip in a non-default compose profile (Postgres-only mode). `sentry-go` init guarded by `SENTRY_DSN`; capture on job-handler panic, terminal job failure and an HTTP recover middleware; `BeforeSend` scrubber. Force one failing job. | SDK compatibility, memory footprint, PII scrubbing, no impact when disabled | Event visible within 1 min with job type/id and request_id; no phone numbers or message text; idle RSS ≤ 512 MB; full regression green with the DSN unset |
| E4 | Testcontainers helper: when `MAJO_TEST_DATABASE_URL` is empty and Docker is available, start `postgres:16-alpine` for the integration packages | Whether the integration tests can run on your Windows machine with Docker Desktop, and how long they take | The 28 hardening integration tests pass locally without a hand-made database |
| E5 (later) | Metabase against a `reporting` schema of PII-free views through a read-only role; answer "confirmed requests by category per week" | The PII boundary holds, and the real memory cost | No access to phone/message columns; Metabase stable at ≤ 1.5 GB |
| E6 | Three Playwright smoke tests (set token → open request → pause/resume) against the compose stack with the fake Graph API | The control plane survives the Next.js upgrade | Green before and after the upgrade |

## 10. Deferred tools and adoption triggers

| Tool | Trigger |
|---|---|
| Metabase | Real buyer/vendor traffic, and recurring ad-hoc questions (more than weekly) |
| Grafana (Postgres datasource) | Alerts needed on rates over time (queue backlog, delivery failures), or a second person monitors operations |
| Prometheus, Loki, OpenTelemetry | More than one host/instance, latency SLOs, or log search becomes a daily need |
| pg_trgm | Real data shows listings missed because of spelling variants |
| pgvector | Measured misses that synonyms and trigram matching cannot fix. Even then, it only proposes candidates that rules score. |
| Meilisearch / Typesense | An approved web storefront with search-as-you-type |
| ECharts | A time-series metric enters the dashboard |
| shadcn/ui | A dashboard styling overhaul (Tailwind adoption) is approved |
| River | Go toolchain upgraded **and** a need for a job UI, periodic schedules or rate-limited queues |
| authentik / Keycloak (Zitadel ranked lower: AGPL) | More than ~3 operators, SSO, or customer web accounts |
| Unleash / Flagsmith / GrowthBook | Several environments or percentage rollouts. First try a native settings table. |
| Scalar / Swagger UI | An OpenAPI spec is written |
| OWASP ZAP | A staging environment exists, before public launch |
| SeaweedFS (or a managed S3 bucket, preferred) | More than one host, or media backups required |
| FFmpeg | Voice notes ingested, or thumbnails needed |
| Zammad / FreeScout | Disputes or support move to email (after payments) |
| Umami | A public website launches |
| listmonk | Email marketing is approved |
| EventCatalog | Several services or teams publish and consume events |

## 11. Rejected candidates and why

| Candidate | Reason |
|---|---|
| Appsmith, ToolJet | Duplicate the dashboard; 8 GB-class footprint (Appsmith); invite DB-level writes that bypass controls |
| Refine, React Admin (as replacements) | Would mean rewriting working, domain-specific pages |
| Tremor | Development moved to shadcn/ui; stale since October 2025 |
| Superset | Heavier than Metabase for a one-operator team |
| Self-hosted Sentry | 4 cores / 16 GB RAM minimum; GlitchTip uses the same SDKs |
| Chatwoot (WhatsApp) | Would take over the Meta webhook and send on Majo's number |
| Papercups | Unmaintained |
| Temporal | Moves business state outside Majo; separate cluster |
| Asynq | Requires Redis |
| Windmill | A side door around the state machine |
| OpenSearch | Far beyond current scale |
| MinIO | Community edition in maintenance mode |
| Paperless-ngx | No document-management need |
| React Flow | Operators need a timeline, not a graph editor |
| Hoppscotch | Bruno covers it without a server |
| Medusa, Saleor, Vendure | A second commerce engine and source of truth |
| Directus | Source-available licence, and DB-level admin |
| Novu | Duplicates Majo's messaging layer and adds a second WhatsApp sender |
| Postal | Running a mail server is false economy versus a managed provider |

Self-hosted Sentry scores 82/100 on the weighted criteria but is rejected: its hosting cost is a hard constraint that the weighting averages away. The scores are decision aids, not decisions.

## 12. Phased adoption plan

| Phase | When | Work |
|---|---|---|
| **R0: hygiene** | Now, ≈1–2 days, no new services | E1 Gitleaks (plus a pre-commit hook); E2 scanners; patch `next` to 14.2.35; plan the supported Next.js major and Go toolchain (1.26+) upgrades; Bruno collection; Mermaid diagrams of the request/vendor-request state machines |
| **R1: control-plane hardening** | Before any external interface or pilot | Native operator accounts and route protection plus CORS allow-list; paginated read APIs; TanStack Table; native event-timeline and jobs/failure pages; E3 GlitchTip pilot; E4 Testcontainers pilot |
| **R2: with real data** | When the pilot resumes | E6 Playwright smoke tests; Trivy (pinned) on images; Metabase pilot (E5) once its trigger is met; ZAP baseline once staging exists |
| **R3: on triggers only** | §10 | Object storage, pg_trgm, IdP, flags, River, search engine, Grafana |

## 13. Next actions in priority order

1. Approve E1 and E2 (read-only scans) and the `next@14.2.35` patch branch.
2. Decide the Next.js major and Go toolchain upgrade window. This also unblocks River later.
3. Approve the native route-protection work (it also resolves decision J-3 from the Phase 4C hardening report).
4. Approve the E3 GlitchTip pilot and E4 Testcontainers pilot.
5. Add TanStack Table and build the event-timeline and jobs pages.
6. Commit the Bruno collection and Mermaid diagrams.
7. Revisit the deferred list at each phase boundary against §10.

## Assumptions

- Production will run on one small VM-class host plus a managed Postgres (Neon). Nothing in the repository documents hosting.
- There is one operator today; buyers and vendors stay WhatsApp-only, with no web accounts.
- Maintenance status uses the last commit on the default branch (fetched 13 Oct 2026, read-only shallow clones) plus vendor release notes. GitHub API metadata (archived flag, issue counts) was not available from the sandbox.
- Licences were read from each repository's licence files on 13 Oct 2026. Edition limits and resource figures come from vendor documentation on the same date and may change.
- The effort estimates and the weighted scores are judgements, documented so they can be challenged.
- `npm audit` was run read-only against the committed `package-lock.json`. No other scanner was run, and no tool was installed.
