# Majo component registry

Generated from `docs/registry/components.json` by `scripts/registry/render_registry.py`. **Edit the JSON, not this file.** Consult this registry before building a new capability or adding a dependency.

**Status** distinguishes what is actually in the repository (`INSTALLED`) from what was only assessed (`EVALUATED`). An `EVALUATED` entry — whatever its decision — is **not integrated**.

Weighted score: Need 20%, Compat 15%, Maint 10%, Licence 10%, Deploy/cost 15%, Security 10%, Effort saved 15%, Architecture 5%. Each criterion is scored 1–5; the total is out of 100. Scores are decision aids, not decisions.

## 1. Installed in the repository today

| Component | Licence | Where | Decision | Note |
|---|---|---|---|---|
| google/uuid | BSD-3-Clause | Go module (backend/go.mod v1.6.0) | KEEP | In use across the backend.  |
| lib/pq | MIT | Go module (backend/go.mod v1.10.9) | KEEP | In use. Upstream is active (v1.12.3, last commit 2026-08-20) but Majo pins v1.10.9 (2023): include in dependency updates. pgx would be a separate decision (River needs it).  |
| FastAPI + Uvicorn + Pydantic + httpx | MIT / BSD-3-Clause | ai/requirements.txt | KEEP | In use (ai/). Pinned 2024 versions; include in vulnerability scans. |
| Next.js 14.2.15 + React 18 | MIT | dashboard/package.json | KEEP, UPGRADE | The dashboard is the control plane and should stay native. The version needs patching. npm audit on 2026-10-13: 1 critical (next), 2 high (postcss, source-map-js). The latest 14.x patch is 14.2.35; some advisories are only fixed in 15.x/16.x. |
| PostgreSQL 16 (local) / Neon (owner's dev DB) | PostgreSQL Licence | docker-compose postgres:16-alpine; Neon | KEEP | Canonical database. Holds buyer phone numbers and messages. |
| pytest | MIT | ai/requirements.txt | KEEP | In use.  |
| Meta WhatsApp Cloud API (external API) | Meta platform terms | External API behind channel.Sender / infrastructure/whatsapp | KEEP | Core channel, accessed only through Majo's provider abstraction. The only outbound channel: no other tool may send on the Majo number. |
| Anthropic API (external, optional) | Anthropic commercial terms | Called from ai/app/extraction.py over HTTP | KEEP | Optional; the deterministic policy decides, the extractor only proposes. Sends buyer message text to a processor when enabled. |
| Majo metrics | Majo | Existing | BUILD NATIVELY (keep) | The definitions are part of the control plane and the Architecture Lock; a BI tool must not redefine them.  |

No other third-party product, service or library is integrated. Everything below is **evaluated or planned only — not integrated**.

## 2. Evaluated: decisions at a glance

| Decision | Products |
|---|---|
| **ADOPT NOW** | govulncheck + npm audit + pip-audit (97.0), Gitleaks (95.0), Mermaid (89.0), Bruno (84.0) |
| **PILOT** | GlitchTip (93.0), Trivy (89.0), Testcontainers for Go (88.0), Playwright (83.0) |
| **REUSE COMPONENTS** | TanStack Table v8 (90.0) |
| **BUILD NATIVELY** | Majo operator accounts, Majo dashboard pages |
| **DEFER** | pg_trgm (Postgres contrib) (86.0), Apache ECharts (79.0), OWASP ZAP (76.0), Grafana OSS (74.0), shadcn/ui (73.0), Metabase (OSS) (73.0), pgvector (72.0), Scalar (71.0), Swagger UI (71.0), SeaweedFS (69.0), Umami (68.0), OpenTelemetry Go SDK (68.0), River (67.0), Prometheus (65.0), authentik (65.0), Keycloak (64.0), FFmpeg (63.0), Zitadel (63.0), EventCatalog (62.0), listmonk (62.0), Flagsmith (61.0), Unleash (59.0), GrowthBook (59.0), Grafana Loki (58.0), Meilisearch (57.0), Zammad (53.0), Typesense (52.0), FreeScout (51.0) |
| **REJECT** | Sentry (self-hosted) (82.0), React Flow (xyflow) (71.0), Apache Superset (69.0), Hoppscotch (67.0), Refine (64.0), React Admin (64.0), Tremor (62.0), ToolJet (51.0), Postal (51.0), Appsmith (50.0), MinIO (community) (50.0), Chatwoot (49.0), Temporal (48.0), OpenSearch (47.0), Novu (47.0), Asynq (46.0), Windmill (46.0), Paperless-ngx (46.0), Saleor (46.0), Medusa (44.0), Directus (44.0), Papercups (43.0), Vendure (42.0) |

## 3. Scores

| Product | Category | Need | Compat | Maint | Licence | Deploy/cost | Security | Effort saved | Architecture | **Weighted** | Decision |
|---|---|---|---|---|---|---|---|---|---|---|---|
| govulncheck + npm audit + pip-audit | K. Testing and security | 5 | 5 | 5 | 5 | 5 | 5 | 4 | 5 | **97.0** | ADOPT NOW |
| Gitleaks | K. Testing and security | 5 | 5 | 4 | 5 | 5 | 5 | 4 | 5 | **95.0** | ADOPT NOW |
| GlitchTip | C. Observability | 5 | 5 | 4 | 5 | 4 | 4 | 5 | 5 | **93.0** | PILOT |
| TanStack Table v8 | A. Admin interfaces | 4 | 5 | 5 | 5 | 5 | 5 | 3 | 5 | **90.0** | REUSE COMPONENTS |
| Trivy | K. Testing and security | 4 | 5 | 5 | 5 | 5 | 3 | 4 | 5 | **89.0** | PILOT |
| Mermaid | L. Workflow visualisation | 3 | 5 | 5 | 5 | 5 | 5 | 4 | 5 | **89.0** | ADOPT NOW |
| Testcontainers for Go | K. Testing and security | 4 | 4 | 5 | 5 | 5 | 4 | 4 | 5 | **88.0** | PILOT |
| pg_trgm (Postgres contrib) | F. Search | 3 | 5 | 5 | 5 | 5 | 5 | 3 | 5 | **86.0** | DEFER |
| Bruno | J. API tooling | 3 | 5 | 5 | 5 | 5 | 4 | 3 | 5 | **84.0** | ADOPT NOW |
| Playwright | K. Testing and security | 3 | 5 | 5 | 5 | 4 | 5 | 3 | 5 | **83.0** | PILOT |
| Sentry (self-hosted) | C. Observability | 5 | 5 | 5 | 3 | 1 | 4 | 5 | 5 | **82.0** | REJECT |
| Apache ECharts | B. Business analytics | 2 | 4 | 5 | 5 | 5 | 5 | 3 | 5 | **79.0** | DEFER |
| OWASP ZAP | K. Testing and security | 3 | 4 | 5 | 5 | 4 | 3 | 3 | 5 | **76.0** | DEFER |
| Grafana OSS | C. Observability | 3 | 4 | 5 | 4 | 4 | 3 | 3 | 5 | **74.0** | DEFER |
| shadcn/ui | A. Admin interfaces | 2 | 2 | 5 | 5 | 5 | 5 | 3 | 5 | **73.0** | DEFER |
| Metabase (OSS) | B. Business analytics | 3 | 4 | 5 | 4 | 3 | 3 | 4 | 4 | **73.0** | DEFER |
| pgvector | F. Search | 1 | 5 | 5 | 5 | 5 | 5 | 2 | 2 | **72.0** | DEFER |
| Scalar | J. API tooling | 2 | 3 | 5 | 5 | 5 | 4 | 2 | 5 | **71.0** | DEFER |
| Swagger UI | J. API tooling | 2 | 3 | 5 | 5 | 5 | 4 | 2 | 5 | **71.0** | DEFER |
| React Flow (xyflow) | L. Workflow visualisation | 1 | 4 | 5 | 5 | 5 | 5 | 2 | 4 | **71.0** | REJECT |
| Apache Superset | B. Business analytics | 3 | 4 | 5 | 5 | 2 | 3 | 3 | 4 | **69.0** | REJECT |
| SeaweedFS | G. Media and files | 2 | 4 | 5 | 5 | 3 | 3 | 3 | 5 | **69.0** | DEFER |
| Umami | B. Business analytics | 1 | 4 | 4 | 5 | 4 | 4 | 3 | 5 | **68.0** | DEFER |
| OpenTelemetry Go SDK | C. Observability | 2 | 4 | 5 | 5 | 3 | 4 | 2 | 5 | **68.0** | DEFER |
| River | E. Jobs and workflows | 2 | 2 | 5 | 4 | 5 | 5 | 2 | 4 | **67.0** | DEFER |
| Hoppscotch | J. API tooling | 2 | 4 | 4 | 5 | 3 | 3 | 3 | 5 | **67.0** | REJECT |
| Prometheus | C. Observability | 2 | 3 | 5 | 5 | 3 | 4 | 2 | 5 | **65.0** | DEFER |
| authentik | H. Identity and access | 2 | 3 | 5 | 4 | 3 | 4 | 3 | 4 | **65.0** | DEFER |
| Refine | A. Admin interfaces | 2 | 2 | 4 | 5 | 5 | 4 | 2 | 3 | **64.0** | REJECT (as replacement) |
| React Admin | A. Admin interfaces | 2 | 2 | 5 | 4 | 5 | 4 | 2 | 3 | **64.0** | REJECT (as replacement) |
| Keycloak | H. Identity and access | 2 | 3 | 5 | 5 | 2 | 4 | 3 | 4 | **64.0** | DEFER |
| FFmpeg | G. Media and files | 1 | 3 | 5 | 4 | 4 | 3 | 3 | 5 | **63.0** | DEFER |
| Zitadel | H. Identity and access | 2 | 3 | 5 | 3 | 3 | 4 | 3 | 4 | **63.0** | DEFER |
| Tremor | A. Admin interfaces | 2 | 2 | 1 | 5 | 5 | 5 | 2 | 5 | **62.0** | REJECT |
| EventCatalog | L. Workflow visualisation | 1 | 3 | 4 | 4 | 4 | 5 | 2 | 5 | **62.0** | DEFER |
| listmonk | N. Notifications | 1 | 3 | 5 | 4 | 4 | 3 | 3 | 4 | **62.0** | DEFER |
| Flagsmith | I. Feature flags | 1 | 4 | 5 | 5 | 3 | 3 | 2 | 4 | **61.0** | DEFER |
| Unleash | I. Feature flags | 1 | 4 | 5 | 4 | 3 | 3 | 2 | 4 | **59.0** | DEFER |
| GrowthBook | I. Feature flags | 1 | 4 | 5 | 4 | 3 | 3 | 2 | 4 | **59.0** | DEFER |
| Grafana Loki | C. Observability | 2 | 3 | 5 | 4 | 2 | 3 | 2 | 5 | **58.0** | DEFER |
| Meilisearch | F. Search | 1 | 3 | 5 | 4 | 3 | 3 | 3 | 2 | **57.0** | DEFER |
| Zammad | D. Support | 2 | 2 | 5 | 3 | 2 | 3 | 3 | 2 | **53.0** | DEFER |
| Typesense | F. Search | 1 | 3 | 5 | 3 | 2 | 3 | 3 | 2 | **52.0** | DEFER |
| ToolJet | A. Admin interfaces | 2 | 2 | 5 | 3 | 2 | 2 | 3 | 2 | **51.0** | REJECT |
| FreeScout | D. Support | 1 | 2 | 4 | 3 | 4 | 3 | 2 | 3 | **51.0** | DEFER |
| Postal | N. Notifications | 1 | 3 | 4 | 5 | 2 | 2 | 2 | 4 | **51.0** | REJECT |
| Appsmith | A. Admin interfaces | 2 | 2 | 5 | 4 | 1 | 2 | 3 | 2 | **50.0** | REJECT |
| MinIO (community) | G. Media and files | 2 | 4 | 1 | 2 | 2 | 2 | 3 | 5 | **50.0** | REJECT |
| Chatwoot | D. Support | 2 | 1 | 5 | 4 | 2 | 2 | 3 | 1 | **49.0** | REJECT (for WhatsApp) |
| Temporal | E. Jobs and workflows | 1 | 2 | 5 | 5 | 1 | 4 | 2 | 1 | **48.0** | REJECT |
| OpenSearch | F. Search | 1 | 2 | 5 | 5 | 1 | 3 | 2 | 2 | **47.0** | REJECT |
| Novu | N. Notifications | 1 | 2 | 5 | 4 | 2 | 3 | 2 | 1 | **47.0** | REJECT |
| Asynq | E. Jobs and workflows | 1 | 2 | 3 | 5 | 2 | 4 | 1 | 3 | **46.0** | REJECT |
| Windmill | E. Jobs and workflows | 1 | 2 | 5 | 3 | 3 | 2 | 2 | 1 | **46.0** | REJECT |
| Paperless-ngx | G. Media and files | 1 | 2 | 5 | 4 | 2 | 3 | 1 | 3 | **46.0** | REJECT |
| Saleor | M. Commerce platforms | 1 | 1 | 5 | 5 | 2 | 3 | 2 | 1 | **46.0** | REJECT |
| Medusa | M. Commerce platforms | 1 | 1 | 5 | 4 | 2 | 3 | 2 | 1 | **44.0** | REJECT |
| Directus | M. Commerce platforms | 1 | 2 | 5 | 2 | 3 | 2 | 2 | 1 | **44.0** | REJECT |
| Papercups | D. Support | 1 | 2 | 1 | 5 | 3 | 2 | 2 | 2 | **43.0** | REJECT |
| Vendure | M. Commerce platforms | 1 | 1 | 5 | 3 | 2 | 3 | 2 | 1 | **42.0** | REJECT |

## 4. Entries

### A. Admin interfaces

#### TanStack Table v8 — REUSE COMPONENTS  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Headless tables: sort, filter, paginate |
| Repository | https://github.com/TanStack/table |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | Hand-written tables in requests/vendors/offers pages (no sort, filter or pagination; the API has no paging either). |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | npm library, no service |
| Resource/maintenance burden | Low |
| Integration complexity | Low |
| Security | None (client-side only). |
| Reason | Headless, so it fits the inline-style dashboard; React 18 compatible. It saves real work on the events, jobs and exceptions lists that are missing. |
| Reconsider when | - |
| Weighted score | 90.0 |

#### shadcn/ui — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | UI component recipes |
| Repository | https://github.com/shadcn-ui/ui |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | None (the dashboard has 3 shared components and ~330 inline styles). |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | Copied source; requires Tailwind CSS |
| Resource/maintenance burden | Low |
| Integration complexity | Medium (Tailwind must be introduced first) |
| Security | Low |
| Reason | Useful accessible primitives, but it presupposes Tailwind, which the dashboard does not use. Adopting it is a styling decision, not a capability gap. |
| Reconsider when | The dashboard gets a styling overhaul, or dialogs/forms become hard to build accessibly by hand. |
| Weighted score | 73.0 |

#### Refine — REJECT (as replacement)  `EVALUATED`

| Field | Value |
|---|---|
| Capability | React admin framework |
| Repository | https://github.com/refinedev/refine |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-09-10 |
| Overlaps with Majo | Would replace the existing Next.js pages. |
| Integration model | A |
| Data owner | Majo |
| Auth requirements | Uses Majo's API |
| Hosting | npm library |
| Resource/maintenance burden | Medium |
| Integration complexity | High (rewrite) |
| Security | Low |
| Reason | Adopting it means rewriting working pages. Majo's screens are domain-specific (decision logs, holds, curation), not generic CRUD. |
| Reconsider when | A large new back-office area of generic CRUD resources is planned. |
| Weighted score | 64.0 |

#### React Admin — REJECT (as replacement)  `EVALUATED`

| Field | Value |
|---|---|
| Capability | React admin framework |
| Repository | https://github.com/marmelab/react-admin |
| Licence | MIT (Enterprise Edition modules are paid) |
| Open-source edition limits | Enterprise modules (audit log, RBAC, etc.) are paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-11 |
| Overlaps with Majo | Would replace the existing pages. |
| Integration model | A |
| Data owner | Majo |
| Auth requirements | Uses Majo's API |
| Hosting | npm library |
| Resource/maintenance burden | Medium |
| Integration complexity | High (rewrite) |
| Security | Low |
| Reason | Same as Refine. |
| Reconsider when | Same as Refine. |
| Weighted score | 64.0 |

#### Tremor — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Dashboard/chart components |
| Repository | https://github.com/tremorlabs/tremor |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2025-10-10 |
| Overlaps with Majo | - |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | npm library (Tailwind-based) |
| Resource/maintenance burden | Low |
| Integration complexity | Medium |
| Security | Low |
| Reason | Last commit 2025-10-10. After the Vercel acquisition, net-new development moved to shadcn/ui (Vercel community post). It also needs Tailwind. |
| Reconsider when | Upstream resumes active releases. |
| Weighted score | 62.0 |

#### ToolJet — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Low-code internal tools |
| Repository | https://github.com/ToolJet/ToolJet |
| Licence | AGPL-3.0 (plus paid editions) |
| Open-source edition limits | Enterprise features are separate. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | Overlaps the native dashboard. |
| Integration model | B |
| Data owner | Same as Appsmith |
| Auth requirements | Same as Appsmith |
| Hosting | Docker plus its own Postgres; multi-GB RAM in practice |
| Resource/maintenance burden | High |
| Integration complexity | Medium |
| Security | Same DB-bypass risk. |
| Reason | Same reasons as Appsmith, plus AGPL. |
| Reconsider when | Same as Appsmith. |
| Weighted score | 51.0 |

#### Appsmith — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Low-code internal tools |
| Repository | https://github.com/appsmithorg/appsmith |
| Licence | Apache-2.0 (some features in paid editions) |
| Open-source edition limits | SSO, granular access control, audit logs and branding are in paid tiers (per vendor pricing). |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | Overlaps the native dashboard (requests, vendors, offers, discovery, autonomy). |
| Integration model | B |
| Data owner | Majo (if pointed at the API) / bypass risk (if pointed at the DB) |
| Auth requirements | Needs its own users; must call Majo's API with an operator token, never the DB directly. |
| Hosting | Docker; vendor guidance: at least 8 GB RAM for the all-in-one container (bundled MongoDB, Redis and Postgres). |
| Resource/maintenance burden | High |
| Integration complexity | Medium |
| Security | A direct DB connection would bypass Majo authorization, audit and state transitions. |
| Reason | The native dashboard already covers the operator workflows. 8 GB RAM is out of budget, and the obvious shortcut (DB writes) breaks the architecture. |
| Reconsider when | Many non-engineer staff need bespoke CRUD screens weekly, AND Majo exposes a complete authenticated API. |
| Weighted score | 50.0 |

### B. Business analytics

#### Apache ECharts — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Charts inside the dashboard |
| Repository | https://github.com/apache/echarts |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-04 |
| Overlaps with Majo | None: the dashboard shows aggregate numbers and tables, with no charts. |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | npm library |
| Resource/maintenance burden | Low |
| Integration complexity | Low |
| Security | None |
| Reason | Current metrics are point-in-time ratios with explicit denominators. Tables are clearer than charts until time series exist. |
| Reconsider when | A time-series metric (per-day completion, failures over time) is added to the dashboard. |
| Weighted score | 79.0 |

#### Metabase (OSS) — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Ad-hoc business questions and reports |
| Repository | https://github.com/metabase/metabase |
| Licence | AGPL-3.0 outside enterprise/; commercial licence for enterprise/ and for app-embed.js |
| Open-source edition limits | Paid only: SAML/JWT/OIDC/LDAP sign-in, row- and column-level permissions, full-app/modular embedding, usage analytics, white-labelling (metabase.com/docs/latest/paid-features). Use it standalone, not embedded. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | The native autonomy and funnel metrics (defined denominators) must stay native. Metabase overlaps nothing for ad-hoc questions. |
| Integration model | B |
| Data owner | Majo owns the data; Metabase only stores saved questions in its own app DB |
| Auth requirements | Metabase users (operators only); a read-only Postgres role restricted to PII-minimised views; never the write role. |
| Hosting | Docker (JVM). Vendor guidance: ≥1 core and 1 GB RAM baseline (+1 CPU/2 GB per 20 concurrent users), plus a Postgres app DB. |
| Resource/maintenance burden | Medium (one JVM container, upgrades) |
| Integration complexity | Low |
| Security | Sees whatever the role sees: phone numbers and message text must be excluded by views. |
| Reason | There is no real marketplace data yet (the pilot is paused). Until there is, ad-hoc BI answers nothing. |
| Reconsider when | Real buyer/vendor traffic is flowing, AND the owner asks the same ad-hoc question more than weekly. Then run PILOT experiment E5. |
| Weighted score | 73.0 |

#### Apache Superset — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | BI platform |
| Repository | https://github.com/apache/superset |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Same as Metabase. |
| Integration model | B |
| Data owner | Majo |
| Auth requirements | Superset users + read-only role |
| Hosting | Python app + metadata DB; caching/async queries need Redis/Celery |
| Resource/maintenance burden | High |
| Integration complexity | Medium |
| Security | Same as Metabase. |
| Reason | Built for data teams; heavier to run than Metabase for a one-person operator team. |
| Reconsider when | A dedicated data analyst joins and Metabase proves limiting. |
| Weighted score | 69.0 |

#### Umami — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Website traffic analytics |
| Repository | https://github.com/umami-software/umami |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-09-16 |
| Overlaps with Majo | None (Majo has no public website). |
| Integration model | B |
| Data owner | Umami |
| Auth requirements | Umami admin login |
| Hosting | Node + Postgres |
| Resource/maintenance burden | Low |
| Integration complexity | Low |
| Security | Cookieless. |
| Reason | There is no public website to measure. |
| Reconsider when | A public marketing site or web storefront launches. |
| Weighted score | 68.0 |

### C. Observability

#### GlitchTip — PILOT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Application error tracking + uptime checks |
| Repository | https://gitlab.com/glitchtip (GitHub mirror is stale) |
| Licence | MIT |
| Open-source edition limits | No enterprise split found. |
| Maintenance (checked 2026-10-13) | last commit see note; Releases: 6.2 (2026-06-22), 6.1 (2026-03-23), 6.0 (2026-02-03) per glitchtip.com/blog |
| Overlaps with Majo | None: panics in job handlers are recovered and logged only; HTTP panics fall to net/http's default; nobody is alerted. |
| Integration model | B + C (Sentry SDKs: sentry-go, sentry-python, @sentry/nextjs) |
| Data owner | GlitchTip (error events). Majo state is untouched. |
| Auth requirements | GlitchTip users; DSN per service as an env var (empty = off). |
| Hosting | Docker; documented 512 MB RAM recommended (256 MB all-in-one). PostgreSQL 14+; Valkey/Redis optional ('PostgreSQL-only mode', 5.2). |
| Resource/maintenance burden | Low |
| Integration complexity | Low (SDK init + recover hooks) |
| Security | Error payloads can carry phone numbers or message text: scrub before send (SDK BeforeSend). |
| Reason | Closes the biggest observability gap (silent failures) at the smallest cost, and is Sentry-SDK compatible so it can be swapped later. |
| Reconsider when | - |
| Weighted score | 93.0 |

#### Sentry (self-hosted) — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Application error tracking |
| Repository | https://github.com/getsentry/sentry |
| Licence | FSL-1.1-Apache-2.0 (Fair Source; converts to Apache-2.0 after 2 years) |
| Open-source edition limits | Cannot be offered as a competing service. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Same as GlitchTip. |
| Integration model | B |
| Data owner | Sentry |
| Auth requirements | Sentry users |
| Hosting | Docs minimum: 4 cores, 16 GB RAM + 16 GB swap, 20 GB disk (32 GB recommended). |
| Resource/maintenance burden | Very high |
| Integration complexity | Low (same SDKs) |
| Security | Same as GlitchTip. |
| Reason | Its footprint is several times Majo's entire stack. GlitchTip uses the same SDKs. |
| Reconsider when | Majo outgrows GlitchTip; then consider paid hosted Sentry rather than self-hosting. |
| Weighted score | 82.0 |

#### Grafana OSS — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Operational dashboards and alert rules |
| Repository | https://github.com/grafana/grafana |
| Licence | AGPL-3.0 |
| Open-source edition limits | Enterprise plugins are paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Partly overlaps the native autonomy/funnel pages. Can read Postgres directly (no Prometheus needed). |
| Integration model | B |
| Data owner | Majo (Grafana reads it) |
| Auth requirements | Grafana users; read-only Postgres role on views |
| Hosting | Single Go binary/container, small footprint |
| Resource/maintenance burden | Low–Medium |
| Integration complexity | Low |
| Security | DB read access; keep PII out of views. |
| Reason | GlitchTip uptime and error alerts plus the native metrics pages cover current needs. Grafana adds value once time-series ops dashboards matter. |
| Reconsider when | Alerts are needed on queue backlog / delivery-failure rates over time, or a second operator monitors the system. |
| Weighted score | 74.0 |

#### OpenTelemetry Go SDK — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Tracing/metrics instrumentation |
| Repository | https://github.com/open-telemetry/opentelemetry-go |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | request_id propagation already exists for logs. |
| Integration model | A (+ needs a collector/backend) |
| Data owner | Backend of choice |
| Auth requirements | - |
| Hosting | Library + collector + trace store |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | Trace attributes may carry PII. |
| Reason | Majo is one Go process plus one Python service; traces add little over request IDs today. |
| Reconsider when | Cross-service latency problems appear, or more services are added. |
| Weighted score | 68.0 |

#### Prometheus — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Metrics collection |
| Repository | https://github.com/prometheus/prometheus |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Majo's metrics are business metrics computed from Postgres, not scraped counters. |
| Integration model | B |
| Data owner | Prometheus TSDB |
| Auth requirements | Network isolation |
| Hosting | Separate service + instrumentation |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium (instrument code) |
| Security | Low |
| Reason | No scrape-style metrics exist yet; Postgres already holds the events. |
| Reconsider when | Multiple backend instances, or latency SLOs are defined. |
| Weighted score | 65.0 |

#### Grafana Loki — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Log aggregation |
| Repository | https://github.com/grafana/loki |
| Licence | AGPL-3.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | slog JSON logs to stdout with request_id. |
| Integration model | B |
| Data owner | Loki |
| Auth requirements | Network isolation |
| Hosting | Separate service + object storage for scale |
| Resource/maintenance burden | Medium–High |
| Integration complexity | Medium |
| Security | Logs may contain PII. |
| Reason | A single host's container logs are enough at current volume. |
| Reconsider when | More than one host, or log retention/search becomes a daily need. |
| Weighted score | 58.0 |

### D. Support

#### Zammad — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Ticketing/case management |
| Repository | https://github.com/zammad/zammad |
| Licence | AGPL-3.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Exception queue (autonomy_exceptions). |
| Integration model | B |
| Data owner | Zammad |
| Auth requirements | Zammad users |
| Hosting | Rails application with Postgres and several companion services |
| Resource/maintenance burden | High |
| Integration complexity | Medium |
| Security | Holds customer PII. |
| Reason | No email support channel exists yet. |
| Reconsider when | Formal disputes/refunds start (after payments), handled by email. |
| Weighted score | 53.0 |

#### FreeScout — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Email helpdesk |
| Repository | https://github.com/freescout-helpdesk/freescout |
| Licence | AGPL-3.0 |
| Open-source edition limits | Many features are paid modules. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-10 |
| Overlaps with Majo | Exception queue. |
| Integration model | B |
| Data owner | FreeScout |
| Auth requirements | FreeScout users |
| Hosting | PHP + MySQL/Postgres; light |
| Resource/maintenance burden | Low–Medium |
| Integration complexity | Low |
| Security | Holds customer PII. |
| Reason | As Zammad. |
| Reconsider when | As Zammad, when a lightweight shared email inbox is enough. |
| Weighted score | 51.0 |

#### Chatwoot — REJECT (for WhatsApp)  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Shared inbox for human support |
| Repository | https://github.com/chatwoot/chatwoot |
| Licence | MIT outside enterprise/ (enterprise/ separately licensed) |
| Open-source edition limits | Some features live in enterprise/. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Conflicts with Majo's WhatsApp ownership: Chatwoot's WhatsApp Cloud setup registers Meta's webhook itself and sends its own outbound messages. |
| Integration model | B (if ever: email/web channels only) |
| Data owner | Chatwoot would hold conversation copies |
| Auth requirements | Chatwoot agents; must not hold the Majo WhatsApp number |
| Hosting | Rails + Sidekiq + Postgres + Redis; docs: 4 GB RAM required, 4 cores recommended. |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | A second outbound sender on the Majo number breaks idempotency, the 24h-window logic, audit and the failure taxonomy. |
| Reason | The exception queue, message trail and pause/resume already give operators what they need, and Majo must stay the only owner of the WhatsApp channel. |
| Reconsider when | Disputes/support move to email or web chat, with volume beyond what the exception queue handles; then reassess for non-WhatsApp channels only. |
| Weighted score | 49.0 |

#### Papercups — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Customer messaging |
| Repository | https://github.com/papercups-io/papercups |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2022-05-10 |
| Overlaps with Majo | - |
| Integration model | B |
| Data owner | - |
| Auth requirements | - |
| Hosting | Elixir + Postgres |
| Resource/maintenance burden | - |
| Integration complexity | - |
| Security | Unmaintained software exposed to customers. |
| Reason | README: 'in maintenance mode'; no commits since 2022-05-10. |
| Reconsider when | Never. |
| Weighted score | 43.0 |

### E. Jobs and workflows

#### River — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Postgres-backed job queue (Go) |
| Repository | https://github.com/riverqueue/river |
| Licence | MPL-2.0 (River Pro is commercial) |
| Open-source edition limits | Workflows, sequences and some Pro features are paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | Duplicates internal/jobs: SKIP LOCKED claims, leases, idempotency keys, retries with backoff, terminal failure with an event in the same transaction, 12 job types. |
| Integration model | A |
| Data owner | Majo DB (river_* tables) |
| Auth requirements | n/a |
| Hosting | Library; uses pgx (go.mod requires Go 1.26) |
| Resource/maintenance burden | Low once migrated |
| Integration complexity | Medium–High (Go upgrade, pgx, 12 handlers, tests) |
| Security | Low |
| Reason | The existing durable job system already meets the retry, deadline, expiry and recovery requirements and has integration tests. Migrating adds risk without a missing capability. |
| Reconsider when | The job system needs features that are expensive to build (a job UI, periodic schedules, rate-limited queues), AFTER the Go toolchain is upgraded. |
| Weighted score | 67.0 |

#### Temporal — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Durable workflow engine |
| Repository | https://github.com/temporalio/temporal |
| Licence | MIT |
| Open-source edition limits | Cloud is paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | Would duplicate the domain state machine (BuyerRequest/VendorRequest/Offer states). |
| Integration model | B |
| Data owner | Temporal (workflow history) |
| Auth requirements | mTLS/namespace auth |
| Hosting | Multi-service cluster + persistence DB |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | Moves business state outside Majo. |
| Reason | Conflicts with 'deterministic application services own state'. No evidence of a need. |
| Reconsider when | Never for the commerce loop. |
| Weighted score | 48.0 |

#### Asynq — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Redis job queue (Go) |
| Repository | https://github.com/hibiken/asynq |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-06-12 |
| Overlaps with Majo | Duplicates internal/jobs. |
| Integration model | A |
| Data owner | Redis |
| Auth requirements | - |
| Hosting | Needs Redis |
| Resource/maintenance burden | Medium |
| Integration complexity | High |
| Security | New datastore. |
| Reason | Introduces Redis for a capability Postgres already covers. |
| Reconsider when | Never. |
| Weighted score | 46.0 |

#### Windmill — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Script/workflow automation |
| Repository | https://github.com/windmill-labs/windmill |
| Licence | AGPLv3/Apache-2.0 parts + proprietary EE |
| Open-source edition limits | Enterprise features are proprietary. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Overlaps jobs and the operator dashboard. |
| Integration model | B |
| Data owner | Windmill |
| Auth requirements | Windmill users |
| Hosting | Rust server + Postgres + workers |
| Resource/maintenance burden | Medium–High |
| Integration complexity | Medium |
| Security | Scripts with DB credentials bypass Majo controls. |
| Reason | No need for user-authored automation; it would be a side door around the state machine. |
| Reconsider when | Never for commerce state. |
| Weighted score | 46.0 |

### F. Search

#### pg_trgm (Postgres contrib) — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Typo-tolerant text matching in Postgres |
| Repository | https://www.postgresql.org/docs/current/pgtrgm.html |
| Licence | PostgreSQL Licence |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit ships with PostgreSQL |
| Overlaps with Majo | Extends the LIKE-based normalized_product matching; no new service. |
| Integration model | A |
| Data owner | Majo |
| Auth requirements | n/a |
| Hosting | CREATE EXTENSION; supported on Neon (v1.6) |
| Resource/maintenance burden | None |
| Integration complexity | Low |
| Security | None |
| Reason | Matching is deterministic and explainable today, and misses have not been measured on real data. The 4 corpus misses are extraction gaps, not search gaps. |
| Reconsider when | Real data shows listings missed because of spelling variants. Add a trigram index plus a similarity threshold, logged in the match reasons. |
| Weighted score | 86.0 |

#### pgvector — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Vector similarity in Postgres |
| Repository | https://github.com/pgvector/pgvector |
| Licence | PostgreSQL Licence |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-01 |
| Overlaps with Majo | Would compete with explainable rule-based matching. |
| Integration model | A |
| Data owner | Majo |
| Auth requirements | n/a |
| Hosting | Extension; on Neon (0.8.x) |
| Resource/maintenance burden | Low |
| Integration complexity | Medium (embeddings pipeline) |
| Security | Embeddings of buyer text are PII-derived. |
| Reason | Matching must stay explainable; no demonstrated semantic gap. |
| Reconsider when | Measured, repeated misses that synonyms and pg_trgm cannot fix. Even then, use it only to propose candidates that rules then score. |
| Weighted score | 72.0 |

#### Meilisearch — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Search-as-you-type engine |
| Repository | https://github.com/meilisearch/meilisearch |
| Licence | MIT core; Enterprise Edition parts under BUSL-1.1 |
| Open-source edition limits | EE features are not free for production. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Would duplicate listing search; needs a second index kept in sync. |
| Integration model | B |
| Data owner | Majo (index derived) |
| Auth requirements | API keys |
| Hosting | Rust binary; RAM-heavy indexing |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | Index holds listing data. |
| Reason | No customer-facing search UI exists; buyers ask in WhatsApp. |
| Reconsider when | A web storefront with search-as-you-type is approved. |
| Weighted score | 57.0 |

#### Typesense — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Search engine |
| Repository | https://github.com/typesense/typesense |
| Licence | GPL-3.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | As Meilisearch. |
| Integration model | B |
| Data owner | Majo (index derived) |
| Auth requirements | API keys |
| Hosting | C++ binary; index held in RAM |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | As Meilisearch. |
| Reason | As Meilisearch. |
| Reconsider when | As Meilisearch. |
| Weighted score | 52.0 |

#### OpenSearch — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Search/analytics cluster |
| Repository | https://github.com/opensearch-project/OpenSearch |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | - |
| Integration model | B |
| Data owner | - |
| Auth requirements | - |
| Hosting | JVM cluster, multi-GB |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | - |
| Reason | Far beyond Majo's scale. |
| Reconsider when | Never at current scale. |
| Weighted score | 47.0 |

### G. Media and files

#### SeaweedFS — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Self-hosted S3-compatible storage |
| Repository | https://github.com/seaweedfs/seaweedfs |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | storage.Provider (local disk, Docker volume media_data). |
| Integration model | B + C (S3 adapter behind storage.Provider) |
| Data owner | Majo |
| Auth requirements | S3 keys |
| Hosting | Master + volume + filer services |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | Holds product images. |
| Reason | Local disk behind storage.Provider works for one host. When it stops being enough, a managed S3-compatible bucket is cheaper to operate than self-hosting. |
| Reconsider when | Majo runs on more than one host, or media backups become a requirement; compare a managed bucket first. |
| Weighted score | 69.0 |

#### FFmpeg — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Media transcoding/thumbnails |
| Repository | https://github.com/FFmpeg/FFmpeg |
| Licence | LGPL-2.1+ (GPL if built with GPL parts) |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Media pipeline validates JPEG/PNG/MP4/3GP by magic bytes and stores as-is; audio is not ingested. |
| Integration model | A (CLI in container) |
| Data owner | Majo |
| Auth requirements | n/a |
| Hosting | Binary in the backend image |
| Resource/maintenance burden | Low |
| Integration complexity | Low–Medium |
| Security | Parsing untrusted media is attack surface: sandbox it and keep it updated. |
| Reason | WhatsApp already delivers playable formats; nothing is transcoded today. |
| Reconsider when | Voice notes are ingested (Pidgin speech-to-text), or thumbnails/size reduction are needed for offers. |
| Weighted score | 63.0 |

#### MinIO (community) — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | S3-compatible object storage |
| Repository | https://github.com/minio/minio |
| Licence | AGPL-3.0 |
| Open-source edition limits | Admin console features removed (2025); no official Docker images since Oct 2025; maintenance-only mode since late 2025. |
| Maintenance (checked 2026-10-13) | last commit 2026-02-12; Last commit 2026-02-12 |
| Overlaps with Majo | storage.Provider with a local-disk implementation. |
| Integration model | B |
| Data owner | - |
| Auth requirements | - |
| Hosting | Build from source |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | Only critical security fixes, case by case. |
| Reason | The upstream community edition is no longer actively developed. |
| Reconsider when | Never; choose another S3-compatible store. |
| Weighted score | 50.0 |

#### Paperless-ngx — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Document management |
| Repository | https://github.com/paperless-ngx/paperless-ngx |
| Licence | GPL-3.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | - |
| Integration model | B |
| Data owner | - |
| Auth requirements | - |
| Hosting | Django + Redis + OCR |
| Resource/maintenance burden | Medium |
| Integration complexity | - |
| Security | - |
| Reason | No document-management requirement exists. |
| Reconsider when | A genuine archival requirement (KYC, invoices) appears. |
| Weighted score | 46.0 |

### H. Identity and access

#### authentik — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Identity provider (OIDC/SAML) |
| Repository | https://github.com/goauthentik/authentik |
| Licence | MIT outside authentik/enterprise/ |
| Open-source edition limits | Enterprise features are separately licensed. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | OperatorAuth: one shared bearer token; no operator identity. |
| Integration model | B |
| Data owner | authentik (identities) |
| Auth requirements | OIDC to the dashboard; the backend validates tokens |
| Hosting | Python/Go + Postgres |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | Becomes the root of operator access. |
| Reason | There is one operator. Buyers and vendors are identified by WhatsApp number and need no accounts. |
| Reconsider when | More than ~3 operators, SSO is required, OR customer web accounts launch. Prefer authentik or Keycloak at that point. |
| Weighted score | 65.0 |

#### Keycloak — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Identity provider (OIDC/SAML) |
| Repository | https://github.com/keycloak/keycloak |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | As authentik. |
| Integration model | B |
| Data owner | Keycloak |
| Auth requirements | As authentik |
| Hosting | JVM + Postgres |
| Resource/maintenance burden | Medium–High |
| Integration complexity | Medium |
| Security | As authentik. |
| Reason | As authentik. |
| Reconsider when | As authentik. |
| Weighted score | 64.0 |

#### Zitadel — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Identity provider |
| Repository | https://github.com/zitadel/zitadel |
| Licence | AGPL-3.0 since v3.0 (March 2025); APIs/SDKs stay Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | As authentik. |
| Integration model | B |
| Data owner | Zitadel |
| Auth requirements | As authentik |
| Hosting | Go + Postgres |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | As authentik. |
| Reason | As authentik; ranked below it because of AGPL. |
| Reconsider when | As authentik. |
| Weighted score | 63.0 |

### I. Feature flags

#### Flagsmith — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Flags + remote config |
| Repository | https://github.com/Flagsmith/flagsmith |
| Licence | BSD-3-Clause |
| Open-source edition limits | Some features are paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | As Unleash. |
| Integration model | B |
| Data owner | Flagsmith |
| Auth requirements | API keys |
| Hosting | Django + Postgres |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | As Unleash. |
| Reason | As Unleash. |
| Reconsider when | As Unleash. |
| Weighted score | 61.0 |

#### Unleash — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Feature flags / rollout |
| Repository | https://github.com/Unleash/unleash |
| Licence | AGPL-3.0-or-later from v8.0.0 (official Docker images and SDKs stay Apache-2.0) |
| Open-source edition limits | Many governance features are Enterprise. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Env-based AUTO_* flags + per-request pause/resume. |
| Integration model | B + C |
| Data owner | Unleash |
| Auth requirements | API tokens |
| Hosting | Node + Postgres |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | A flag service becomes a control-plane dependency. |
| Reason | Env flags are enough with one environment. Runtime control of single requests already exists (holds). |
| Reconsider when | Several environments, or the need to flip a global AUTO_* switch without a restart. Even then, first consider a native settings table. |
| Weighted score | 59.0 |

#### GrowthBook — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Flags + experimentation |
| Repository | https://github.com/growthbook/growthbook |
| Licence | MIT outside enterprise directories |
| Open-source edition limits | Enterprise features are paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | As Unleash. |
| Integration model | B |
| Data owner | GrowthBook |
| Auth requirements | API keys |
| Hosting | Node service + its own datastore |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | As Unleash. |
| Reason | No experiments planned. |
| Reconsider when | A/B tests on buyer messaging are planned. |
| Weighted score | 59.0 |

### J. API tooling

#### Bruno — ADOPT NOW  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Version-controlled API request collection |
| Repository | https://github.com/usebruno/bruno |
| Licence | MIT (paid editions exist) |
| Open-source edition limits | Some extras are paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | None: there is no API collection or OpenAPI spec. The E2E scripts are not for exploration. |
| Integration model | A (files in repo + desktop app) |
| Data owner | n/a |
| Auth requirements | Collections reference {{operatorToken}} from a git-ignored env file |
| Hosting | Desktop app / CLI; no server |
| Resource/maintenance burden | None |
| Integration complexity | Low |
| Security | Never commit tokens: use Bruno env files that are git-ignored. |
| Reason | Cheap onboarding and manual testing for 58 routes, with no infrastructure. Plain files diff well in git. |
| Reconsider when | - |
| Weighted score | 84.0 |

#### Scalar — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | API reference UI |
| Repository | https://github.com/scalar/scalar |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | None |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | Serve only behind operator auth |
| Hosting | Static bundle or Go handler |
| Resource/maintenance burden | Low |
| Integration complexity | Low once a spec exists |
| Security | Exposes the API surface: keep it internal. |
| Reason | It needs an OpenAPI spec, which Majo doesn't have; writing one for 58 routes is the real cost. |
| Reconsider when | An OpenAPI spec is written (needed anyway for external partners). |
| Weighted score | 71.0 |

#### Swagger UI — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | API reference UI |
| Repository | https://github.com/swagger-api/swagger-ui |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | None |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | As Scalar |
| Hosting | Static bundle |
| Resource/maintenance burden | Low |
| Integration complexity | Low once a spec exists |
| Security | As Scalar. |
| Reason | As Scalar; Scalar is preferred for the reading experience. |
| Reconsider when | As Scalar. |
| Weighted score | 71.0 |

#### Hoppscotch — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | API client (web) |
| Repository | https://github.com/hoppscotch/hoppscotch |
| Licence | MIT |
| Open-source edition limits | Some features are enterprise. |
| Maintenance (checked 2026-10-13) | last commit 2026-09-30 |
| Overlaps with Majo | Overlaps Bruno. |
| Integration model | B |
| Data owner | - |
| Auth requirements | - |
| Hosting | Self-hosting needs a server + Postgres |
| Resource/maintenance burden | Medium |
| Integration complexity | Low |
| Security | - |
| Reason | Bruno covers it without a server. |
| Reconsider when | Never while Bruno fits. |
| Weighted score | 67.0 |

### K. Testing and security

#### govulncheck + npm audit + pip-audit — ADOPT NOW  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Dependency vulnerability scanning (language-native) |
| Repository | https://github.com/golang/vuln |
| Licence | BSD-3-Clause (govulncheck); Apache-2.0 (pip-audit); npm built-in |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | n/a |
| Overlaps with Majo | None: dependencies have never been scanned. npm audit (2026-10-13): 1 critical, 2 high in the dashboard. |
| Integration model | A (CLI) |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | CLIs; no service |
| Resource/maintenance burden | None |
| Integration complexity | Low |
| Security | - |
| Reason | It has already found real issues. govulncheck reports only vulnerabilities reachable from Majo's code, which cuts noise. |
| Reconsider when | - |
| Weighted score | 97.0 |

#### Gitleaks — ADOPT NOW  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Secret detection (repo + history) |
| Repository | https://github.com/gitleaks/gitleaks |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-07-22; Last commit 2026-07-22; v8.30.1 |
| Overlaps with Majo | None. The owner's local repo has a .env with a Neon password; there is no CI and no pre-commit hook. |
| Integration model | A (CLI / pre-commit) |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | Single binary |
| Resource/maintenance burden | None |
| Integration complexity | Low |
| Security | Reports contain secret fragments: don't paste them in chats. |
| Reason | Highest value per hour: prevents committing the Neon credential, and the planned move to a new git repo is the moment to scan history. |
| Reconsider when | - |
| Weighted score | 95.0 |

#### Trivy — PILOT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Container image / config scanning |
| Repository | https://github.com/aquasecurity/trivy |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Complements the language-native scanners for OS packages in images. |
| Integration model | A (CLI) |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | Single binary |
| Resource/maintenance burden | Low |
| Integration complexity | Low |
| Security | Supply-chain incident: v0.69.4 binary, Docker Hub 0.69.5/0.69.6 and trivy-action tags were malicious (March 2026). Pin verified versions by digest/SHA. |
| Reason | Valuable for images, but after the March 2026 compromise it must be installed by verified digest only. |
| Reconsider when | - |
| Weighted score | 89.0 |

#### Testcontainers for Go — PILOT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Real-Postgres integration tests anywhere |
| Repository | https://github.com/testcontainers/testcontainers-go |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-05 |
| Overlaps with Majo | Integration tests skip without MAJO_TEST_DATABASE_URL (so they are NOT RUN on the owner's Windows machine). |
| Integration model | A (test-only dependency) |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | Requires Docker |
| Resource/maintenance burden | Low |
| Integration complexity | Low–Medium |
| Security | Test-only. |
| Reason | Would let 28+ Postgres integration tests run wherever Docker exists, without a hand-made DB, and keeps Neon out of tests by construction. |
| Reconsider when | - |
| Weighted score | 88.0 |

#### Playwright — PILOT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Browser end-to-end tests |
| Repository | https://github.com/microsoft/playwright |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | None in the repo (used ad hoc in the sandbox for screenshots only). |
| Integration model | A (test-only) |
| Data owner | n/a |
| Auth requirements | Test operator token |
| Hosting | npm dev dependency + browsers |
| Resource/maintenance burden | Low |
| Integration complexity | Low |
| Security | Test-only. |
| Reason | A few smoke tests on the operator pages (token, pause/resume, curation) protect the control plane during the Next.js upgrade. |
| Reconsider when | - |
| Weighted score | 83.0 |

#### OWASP ZAP — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Dynamic web security testing |
| Repository | https://github.com/zaproxy/zaproxy |
| Licence | Apache-2.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-08 |
| Overlaps with Majo | None |
| Integration model | A (CLI/Docker) |
| Data owner | n/a |
| Auth requirements | Authorized targets only |
| Hosting | Docker image |
| Resource/maintenance burden | Low |
| Integration complexity | Medium |
| Security | Must only target an authorized staging environment, never live services. |
| Reason | There is no staging environment to scan yet; known gaps (unauthenticated reads, CORS *) are already identified without it. |
| Reconsider when | A staging deployment exists before public launch; then run a baseline scan. |
| Weighted score | 76.0 |

### L. Workflow visualisation

#### Mermaid — ADOPT NOW  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Diagrams in docs |
| Repository | https://github.com/mermaid-js/mermaid |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Docs use prose and tables. |
| Integration model | A (markdown) |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | None (GitHub renders ```mermaid blocks natively) |
| Resource/maintenance burden | None |
| Integration complexity | Low |
| Security | None |
| Reason | Zero-install way to document state machines and failure paths. |
| Reconsider when | - |
| Weighted score | 89.0 |

#### React Flow (xyflow) — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Node-graph UI |
| Repository | https://github.com/xyflow/xyflow |
| Licence | MIT (Pro examples paid) |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-09-24 |
| Overlaps with Majo | The request page already shows the decision log, questions and message trail. |
| Integration model | A |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | npm library |
| Resource/maintenance burden | Low |
| Integration complexity | Medium |
| Security | None |
| Reason | Operators need a time-ordered timeline, not a graph editor. |
| Reconsider when | A visual workflow designer becomes a requirement. |
| Weighted score | 71.0 |

#### EventCatalog — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Event documentation site |
| Repository | https://github.com/event-catalog/eventcatalog |
| Licence | MIT |
| Open-source edition limits | Some features paid. |
| Maintenance (checked 2026-10-13) | last commit 2026-09-29 |
| Overlaps with Majo | Event types are documented in architecture docs. |
| Integration model | B (static site) |
| Data owner | n/a |
| Auth requirements | n/a |
| Hosting | Static site generator |
| Resource/maintenance burden | Low |
| Integration complexity | Medium |
| Security | None |
| Reason | One service emits the events; markdown plus Mermaid is enough. |
| Reconsider when | Several teams/services publish and consume events. |
| Weighted score | 62.0 |

### M. Commerce platforms

#### Saleor — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Commerce engine |
| Repository | https://github.com/saleor/saleor |
| Licence | BSD-3-Clause |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-12 |
| Overlaps with Majo | Duplicates BuyerRequest/Offer/Transaction, matching and listings: a second commerce engine. |
| Integration model | - |
| Data owner | Would compete with Majo |
| Auth requirements | - |
| Hosting | Separate service + DB |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | Two sources of truth for orders and prices. |
| Reason | Catalogue-first carts don't fit Majo's request-first model, and adopting one creates a second source of truth. |
| Reconsider when | Never as an engine. For a future storefront, build a Next.js front end on Majo's API. |
| Weighted score | 46.0 |

#### Medusa — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Commerce engine |
| Repository | https://github.com/medusajs/medusa |
| Licence | MIT (Enterprise Edition materials excluded) |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Duplicates BuyerRequest/Offer/Transaction, matching and listings: a second commerce engine. |
| Integration model | - |
| Data owner | Would compete with Majo |
| Auth requirements | - |
| Hosting | Separate service + DB |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | Two sources of truth for orders and prices. |
| Reason | Catalogue-first carts don't fit Majo's request-first model, and adopting one creates a second source of truth. |
| Reconsider when | Never as an engine. For a future storefront, build a Next.js front end on Majo's API. |
| Weighted score | 44.0 |

#### Directus — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Data platform/admin over a DB |
| Repository | https://github.com/directus/directus |
| Licence | Monospace Sustainable Core License 1.0 (source-available; GPL-3.0 after 4 years) |
| Open-source edition limits | Not OSI open source; 'Competing Use' restriction; licence-key-protected features. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-09 |
| Overlaps with Majo | Generic admin over Majo's DB would bypass the domain services. |
| Integration model | - |
| Data owner | - |
| Auth requirements | - |
| Hosting | Node + DB |
| Resource/maintenance burden | Medium |
| Integration complexity | Medium |
| Security | Writes would bypass audit/state transitions. |
| Reason | Source-available licence, and DB-level admin conflicts with the architecture. |
| Reconsider when | Never. |
| Weighted score | 44.0 |

#### Vendure — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Commerce engine |
| Repository | https://github.com/vendure-ecommerce/vendure |
| Licence | GPL-3.0 Community Edition or commercial licence |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Duplicates BuyerRequest/Offer/Transaction, matching and listings: a second commerce engine. |
| Integration model | - |
| Data owner | Would compete with Majo |
| Auth requirements | - |
| Hosting | Separate service + DB |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | Two sources of truth for orders and prices. |
| Reason | Catalogue-first carts don't fit Majo's request-first model, and adopting one creates a second source of truth. |
| Reconsider when | Never as an engine. For a future storefront, build a Next.js front end on Majo's API. |
| Weighted score | 42.0 |

### N. Notifications

#### listmonk — DEFER  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Newsletters/email campaigns |
| Repository | https://github.com/knadh/listmonk |
| Licence | AGPL-3.0 |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-11 |
| Overlaps with Majo | None |
| Integration model | B |
| Data owner | listmonk |
| Auth requirements | Admin login |
| Hosting | Go binary + Postgres |
| Resource/maintenance burden | Low |
| Integration complexity | Low |
| Security | Holds email lists. |
| Reason | Majo has no email audience. |
| Reconsider when | Email marketing to vendors/buyers is approved. |
| Weighted score | 62.0 |

#### Postal — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Self-hosted mail server |
| Repository | https://github.com/postalserver/postal |
| Licence | MIT |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | last commit 2026-10-01 |
| Overlaps with Majo | None |
| Integration model | B |
| Data owner | Postal |
| Auth requirements | - |
| Hosting | Mail server + MariaDB + IP reputation work |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | Deliverability and abuse handling become Majo's job. |
| Reason | Running a mail server to avoid a provider fee is false economy. Use a managed transactional email provider behind a provider interface when email is needed. |
| Reconsider when | Never at current scale. |
| Weighted score | 51.0 |

#### Novu — REJECT  `EVALUATED`

| Field | Value |
|---|---|
| Capability | Notification orchestration |
| Repository | https://github.com/novuhq/novu |
| Licence | MIT + separate enterprise licence |
| Open-source edition limits | Enterprise features are separate. |
| Maintenance (checked 2026-10-13) | last commit 2026-10-13 |
| Overlaps with Majo | Duplicates channel.Sender, pending_notices, dedup, retries and the failure taxonomy. |
| Integration model | B |
| Data owner | Novu |
| Auth requirements | API keys |
| Hosting | Community compose (v3.19.0): api, worker, ws, dashboard + MongoDB + Redis |
| Resource/maintenance burden | High |
| Integration complexity | High |
| Security | A second outbound path for WhatsApp. |
| Reason | Messaging must stay inside Majo's provider abstraction (24h window, idempotency, failure classes). |
| Reconsider when | Never for WhatsApp. |
| Weighted score | 47.0 |

### Native (Majo)

#### Majo operator accounts — BUILD NATIVELY  `NATIVE (planned, not built)`

| Field | Value |
|---|---|
| Capability | Operator identity, roles and endpoint protection |
| Repository | backend/internal/httpserver/operator_auth.go |
| Licence | Majo |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | n/a |
| Overlaps with Majo | Extends OperatorAuth (one shared token). |
| Integration model | E |
| Data owner | Majo |
| Auth requirements | Per-operator tokens (hashed) with a role (viewer / operator / admin); all /api routes guarded; CORS allow-list from config. |
| Hosting | In the backend; one migration |
| Resource/maintenance burden | Low |
| Integration complexity | Low–Medium (dashboard already sends a bearer token) |
| Security | Closes 16 unauthenticated data-returning GET routes (buyer requests, vendors, offers, transactions), and names the actor in audit records. |
| Reason | Small, Majo-specific, and a prerequisite before any external tool (BI, error tracking, API docs) touches Majo. |
| Reconsider when | Customer web accounts or SSO become requirements (then an IdP). |
| Weighted score | not scored |

#### Majo dashboard pages — BUILD NATIVELY  `NATIVE (planned, not built)`

| Field | Value |
|---|---|
| Capability | Event/audit timeline and job/failure views |
| Repository | dashboard/app/events/page.tsx (placeholder today) |
| Licence | Majo |
| Open-source edition limits | - |
| Maintenance (checked 2026-10-13) | n/a |
| Overlaps with Majo | Builds on events, decisions, durable jobs and autonomy_exceptions tables. |
| Integration model | E |
| Data owner | Majo |
| Auth requirements | Operator auth |
| Hosting | In the dashboard (TanStack Table for lists) |
| Resource/maintenance burden | Low |
| Integration complexity | Low–Medium (needs paginated read APIs) |
| Security | Read-only views. |
| Reason | Domain-specific: shows Majo's own state transitions. No external product understands them without a second copy of the data. |
| Reconsider when | - |
| Weighted score | not scored |

