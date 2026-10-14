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
3. If the backend is down: restart it (see README "Local run";
   TODO(owner): document the production start/restart command and where
   it runs).
4. If `ai_service` is unreachable: start/restart the AI service
   (uvicorn on port 8000; see README). The core request flow needs it for
   extraction.

## 2. Database unreachable

1. Check `GET http://<host>:8080/ready`.
2. If `"postgres": "unreachable: ..."` — the backend cannot reach the
   database. The owner's database is Neon (per repo docs).
3. Check the DATABASE_URL still matches the current Neon connection
   string, and that the Neon project is awake/not suspended.
   TODO(owner): exact Neon console steps for the account in use.
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

**Pause automation more widely:**

- The Autonomy page shows the autonomy flags (AUTO_CONFIRM...,
  AUTO_ROUTE... etc.) as ON/OFF chips, but the dashboard cannot switch
  them; they come from backend environment settings at startup.
  TODO(owner): document how flags are changed in production (and whether
  a global pause is needed for the pilot).
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
5. If the shared `OPERATOR_API_TOKEN` leaked: rotate that value in the
   server environment and restart the backend.
   TODO(owner): confirm where the server environment is managed.

## 6. A leaked database password or other secret

1. **Contain first**: rotate the leaked secret at its source
   - Database password: change it in the Neon console (TODO(owner):
     exact steps), then update `DATABASE_URL` and restart the backend.
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

TODO(owner): Neon restore steps (point-in-time restore / branch restore
for the account in use). The repo docs do not describe a restore
procedure. Until that is written down here: do not improvise restores
against the live pilot database.

## Sources

- `docs/architecture/whatsapp.md` — webhook, 24h window, failure classes
- `docs/architecture/operator-access.md` — tokens, operatorctl, /health, /ready
- `docs/security/security-checks.md` — gitleaks, run_security_checks.sh
- `docs/implementation/phase-4c-autonomy-hardening-report.md`,
  `docs/architecture/autonomous-runtime.md` — autonomy flags, exceptions,
  pending notices
- Code: `backend/internal/httpserver/health.go` (readiness checks),
  `backend/internal/httpserver/autonomy_handlers.go` (pause/resume API)
