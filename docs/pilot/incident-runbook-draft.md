# DRAFT — incident runbook (pilot)

Based only on what the repo's docs and code describe (see sources at the
bottom). Anything the docs don't cover is marked TODO(owner) — do not
guess at those steps.

## 1. Backend or AI service down

The backend exposes two public checks (no operator token needed):

- `GET http://localhost:8080/health` — liveness only; answers `{"status":"ok"}`
  while the Go process is up.
- `GET http://localhost:8080/ready` — checks Postgres and the AI service.

Steps:

1. Open `http://<host>:8080/ready` in a browser (or `curl`).
2. Read the `checks` object: each entry is `ok` or `unreachable: <error>`.
   - `"status":"ok"` with both checks ok — both services are fine.
   - `"status":"degraded"` — HTTP 503; one of the checks is down.
   - If the page does not load at all, the backend process itself is down.
3. If the backend is down: on the server, `deploy/scripts/majo.sh status`,
   then `deploy/scripts/majo.sh logs backend` and
   `deploy/scripts/majo.sh compose restart backend`
   (see `docs/operations/deployment.md`). Locally, see README "Local run".
4. If `ai_service` is unreachable: `deploy/scripts/majo.sh compose restart ai`
   (locally: uvicorn on port 8000). The core request flow needs it for
   extraction; without an Anthropic key it uses the heuristic extractor.

## 2. Database unreachable

1. Check `GET http://<host>:8080/ready`.
2. If `"postgres": "unreachable: ..."` — the backend cannot reach the
   database. Production uses its own database (managed, or Postgres on the
   server with `--profile localdb`; see `docs/operations/deployment.md`);
   the dev Neon database is never production.
3. Managed: check `DATABASE_URL` in `deploy/.env.production` still matches
   the production project's connection string, and the project is not
   suspended. On-server: `deploy/scripts/majo.sh compose ps postgres` and
   `deploy/scripts/majo.sh logs postgres` (disk full is the usual cause:
   `df -h`). TODO(owner): exact console steps for the provider in use.
4. After the database is reachable again, `/ready` should return
   `"postgres": "ok"`. Migrations run automatically on server start —
   never run them by hand against the production database.

## 3. WhatsApp messages not arriving or not sending

Facts from `docs/architecture/whatsapp.md`:

1. **Incoming messages stop arriving** — check the webhook: Meta must be
   able to `POST https://<your-domain>/webhooks/whatsapp` and must be
   subscribed to the `messages` field. Verify `WHATSAPP_VERIFY_TOKEN`
   matches Meta's dashboard. Incoming POSTs are rejected (401) if the
   `X-Hub-Signature-256` signature doesn't match `WHATSAPP_APP_SECRET`.
2. **Outgoing messages fail** — Majo classifies Meta's error codes.
   Failures appear on the **Autonomy** dashboard page as exceptions
   (reasons include `provider_policy_rejection`, `invalid_recipient`,
   `provider_auth_or_config`).
   - **24-hour window (error 131047 etc.)**: free-form text can only be
     sent within 24h of the user's last message. Majo queues the buyer's
     update (`pending_notices`) and sends it when the buyer writes again.
     No message template is configured yet (`template_configured=false`).
   - **Auth/config errors**: check `WHATSAPP_ACCESS_TOKEN` and
     `WHATSAPP_APP_SECRET` are still valid.
   - **Invalid recipient**: fix the vendor's number (exception
     `invalid_recipient`).
3. Work through the open exceptions on the **Autonomy** page
   (dashboard → Autonomy → "Open exceptions"); each one can be resolved
   with a note.

## 4. Pausing a request, and pausing automation

**Pause one request** (Majo takes no automatic step for that request
until it is resumed):

1. Open the dashboard → Requests → the request.
2. In the autonomy panel, use **Pause automation** with a reason
   (operator token required).
   Or via API/Bruno: `POST /api/requests/{id}/pause` with body
   `{"reason": "..."}` (`tools/bruno/05-operator-actions/pause-request.bru`).
3. To undo: **Resume automation** with a reason, or
   `POST /api/requests/{id}/resume`.

**Pause ALL automation (incident switch):**

1. Dashboard → **Autonomy** → **Global pause**: type a reason, click
   **Pause all automation**, then **Confirm pause** (operator token
   required). Or `POST /api/autonomy/pause` with `{"reason": "..."}`.
   From the server, even if the dashboard is down:
   `deploy/scripts/majo.sh pause "what happened"`.
2. Effect, immediately and without a restart: Majo confirms nothing,
   routes nothing, asks buyers and vendors nothing, delivers no offers and
   sends no queued notices. A red banner shows on every dashboard page.
   Inbound messages are still recorded, and every operator override
   (confirm, route, deliver, select) still works.
3. To undo: same panel, **Resume all automation** with a reason (or
   `POST /api/autonomy/resume`). Majo then re-runs whatever was left
   mid-flow: requests waiting to be confirmed or routed, vendor deadlines
   that passed, offers that became deliverable. A request with its own
   pause stays paused.
4. Both actions are audited (`AUTOMATION_PAUSED` / `AUTOMATION_RESUMED`
   events, the decision log, and the history on the panel).

The autonomy flags (AUTO_CONFIRM..., AUTO_ROUTE... chips on the Autonomy
page) are still environment settings read at startup; changing one needs
a restart. Use the global pause for anything urgent.
- Resolving exceptions on the Autonomy page stops the same work being
  raised again.

## 5. A leaked operator token

Tokens are named; only their SHA-256 hash is stored (per
`docs/architecture/operator-access.md`).

1. Revoke the leaked token immediately:
   `go run ./cmd/operatorctl revoke -name "Name"`
   (revocation takes effect on the next request).
2. Check who has tokens: `go run ./cmd/operatorctl list`.
3. Issue a replacement:
   `go run ./cmd/operatorctl create -name "Name" -role OPERATOR`
   — the new token is printed once; give it to the person directly.
4. In the person's browser, the old token lives in that tab's
   `sessionStorage`; setting the new token in the nav bar replaces it.
5. If the shared `OPERATOR_API_TOKEN` leaked: put a new value
   (`openssl rand -hex 32`) in `deploy/.env.production` on the server and
   run `deploy/scripts/majo.sh compose up -d backend`.
   On the server, operatorctl is `deploy/scripts/majo.sh operator list|create|revoke ...`.

## 6. A leaked database password or other secret

1. **Contain first**: rotate the leaked secret at its source
   - Database password: change it at the production database's provider
     (or, on-server, `ALTER ROLE majo PASSWORD ...` plus `POSTGRES_PASSWORD`),
     update `DATABASE_URL` in `deploy/.env.production`, then
     `deploy/scripts/majo.sh compose up -d backend`.
   - WhatsApp access token: regenerate in Meta Business Settings
     (System User), update `WHATSAPP_ACCESS_TOKEN`, restart.
   - App secret / verify token: regenerate in the Meta App dashboard and
     update the server environment.
2. **Check how it leaked**: run the security checks from the repo root:
   `scripts/security/run_security_checks.sh` (see
   `docs/security/security-checks.md`). This runs Gitleaks over git
   history, govulncheck, npm audit, pip-audit and Trivy config.
3. **Stop it happening again**: the pre-commit hook
   (`git config core.hooksPath .githooks`) blocks commits containing
   secrets. Never paste connection strings or tokens into chats; reports
   can contain secret fragments.

## 7. Restoring the database from a backup

Nightly dumps (database and media) come from `deploy/scripts/majo.sh backup`;
test them with `majo.sh restore-test <dump>`. The full restore procedure
is in `docs/operations/deployment.md` §8. Its rules:

- pause automation first;
- restore into a new, empty database, never over the damaged one;
- repoint `DATABASE_URL` and redeploy.

TODO(owner): the managed provider's point-in-time restore steps, if you use
option A.

## Sources

- `docs/architecture/whatsapp.md` — webhook, 24h window, failure classes
- `docs/architecture/operator-access.md` — tokens, operatorctl, /health, /ready
- `docs/security/security-checks.md` — gitleaks, run_security_checks.sh
- `docs/implementation/phase-4c-autonomy-hardening-report.md`,
  `docs/architecture/autonomous-runtime.md` — autonomy flags, exceptions,
  pending notices
- Code: `backend/internal/httpserver/health.go` (readiness checks),
  `backend/internal/httpserver/autonomy_handlers.go` (pause/resume API)
