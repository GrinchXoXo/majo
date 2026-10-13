# Operator access

Reuse-audit R1. This change implements decision J-3 of the Phase 4C hardening
report: dashboard read endpoints are now authenticated.

## Rules

- **Every `/api` route requires a credential.**
  - Read routes (`GET`) accept a VIEWER or an OPERATOR.
  - Every change (`POST`, `PATCH`) needs an OPERATOR.
  - A missing, wrong or revoked credential gets `401` with `WWW-Authenticate: Bearer`.
  - A VIEWER attempting a change gets `403 FORBIDDEN`.
- **Outside the guard:**
  - `/health` and `/ready`, for monitoring;
  - `/webhooks/whatsapp`, which Meta signs (HMAC with `WHATSAPP_APP_SECRET`).
- **Credentials:**
  - **Named operator tokens** (table `operators`, migration 0016). Each person gets their own token and a role. Only the token's SHA-256 hash is stored. The person's name and ID are recorded as the actor on:
    - corrections, confirmations, associations and curation;
    - vendor create and update;
    - exception resolution;
    - holds (`held_by`);
    - discovery batches (`created_by`).
  - **The shared `OPERATOR_API_TOKEN`.** It is still required outside development, and acts as the bootstrap / break-glass credential, recorded as actor "operator".
- **Development mode.** With no shared token and no active named operator, the guard is open. Config allows that only with `APP_ENV=development` set explicitly. As soon as one named operator exists, the guard closes.
- **CORS.** Only the origins in `CORS_ALLOWED_ORIGINS` (default `DASHBOARD_URL`) get an `Access-Control-Allow-Origin` header; preflights from other origins get `403`. CORS does not authorize anything: it only stops other websites using a signed-in operator's browser.

## Managing operators

```
DATABASE_URL=... go run ./cmd/operatorctl create -name "Ada" -role OPERATOR
DATABASE_URL=... go run ./cmd/operatorctl create -name "Bola" -role VIEWER
DATABASE_URL=... go run ./cmd/operatorctl list
DATABASE_URL=... go run ./cmd/operatorctl revoke -name "Bola"
```

- `create` prints the token once.
- Revocation takes effect on the next request.

## Dashboard

- The token is entered once, in the navigation bar, and kept in `sessionStorage` for that browser tab only. Setting or clearing it reloads the page.
- Every API call sends it.
- Pages that used to render on the server (dashboard funnel, requests list) now render in the browser, so the token never reaches the Next.js server.

## Control-plane read views

These are new, all paginated with `limit` (1–200, default 50), `offset` and the `X-Total-Count` header.

| Route | Purpose |
|---|---|
| `GET /api/events?event_type=&entity_type=&entity_id=&actor_type=` | Event timeline, newest first, with the named operator when there is one |
| `GET /api/events/types` | Distinct event types (for the filter) |
| `GET /api/jobs?status=&job_type=` | Durable jobs, without payloads |
| `GET /api/jobs/summary` | Per job type: pending, running, succeeded, failed, oldest overdue |
| `GET /api/requests?limit=&offset=&status=` | Paginated request list. Without `limit`, the full array is returned as before. |

## Known limits

- Routing, offer delivery, selection and clarification events still record actor type OPERATOR without the operator's ID. Those services do not take an operator ID yet.
- There is no login UI or session: a token is a bearer secret. A person-facing identity provider stays deferred (component registry: authentik / Keycloak).
