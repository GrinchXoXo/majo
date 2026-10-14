# Majo

Majo is a WhatsApp-native commerce platform: buyers describe what they need
in ordinary WhatsApp messages, an operator (with AI assistance) turns that
into a structured request, and matching vendors are reached over WhatsApp
with auditable, operator-approved routing. The Go backend, Python AI
extraction service and Next.js operations dashboard run as three local
processes against PostgreSQL.

## Prerequisites

- Go 1.26+
- Node.js 22+
- Python 3.11+
- PostgreSQL (or use the throwaway cloud database provided in `.env.local`)

## Local run

Configuration comes from `.env.local` in the repo root, which holds a
`DATABASE_URL` pointing at a **throwaway** database. Never point local
development at the main database, and never commit `.env.local`.

Load the environment in every PowerShell window you use (run from the repo root):

```powershell
Get-Content .env.local | Where-Object { $_ -match '^\s*[A-Za-z_]+=' } | ForEach-Object { $k,$v = $_ -split '=',2; Set-Item -Path "env:$($k.Trim())" -Value $v.Trim() }
```

**AI service:**

```powershell
cd ai
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m uvicorn app.main:app --port 8000
```

**Backend** (after loading `.env.local`):

```powershell
cd backend
$env:APP_ENV = "development"
$env:AI_SERVICE_URL = "http://localhost:8000"
go run ./cmd/server
```

Migrations apply automatically on startup ("migrations applied
successfully"). `http://localhost:8080/ready` reports `postgres` and
`ai_service` health.

**Dashboard:**

```powershell
cd dashboard
npm install
$env:NEXT_PUBLIC_API_URL = "http://localhost:8080"
npm run dev
```

Open http://localhost:3000 and paste your operator token (top right).

## Operator tokens

Every `/api` route requires an operator token. Manage operators with:

```powershell
cd backend
go run ./cmd/operatorctl create -name "Name" -role OPERATOR   # prints the token ONCE
go run ./cmd/operatorctl list
go run ./cmd/operatorctl revoke -name "Name"
```

Save the token when it is printed — it is not shown again.

## Tests

```powershell
cd backend; go test ./...              # DB integration tests skip unless MAJO_TEST_DATABASE_URL is set
cd ai; .\.venv\Scripts\python -m pytest -q
cd dashboard; npx tsc --noEmit; npm run build
```

- Playwright smoke tests: `dashboard/e2e`
- Bruno API collection: `tools/bruno` (auth via environment token, never committed)

## Security checks

```bash
scripts/security/run_security_checks.sh
```

See [docs/security/security-checks.md](docs/security/security-checks.md).

## Documentation

- [docs/architecture/](docs/architecture/) — architecture notes (autonomy
  hardening baseline, WhatsApp integration)
- [docs/implementation/](docs/implementation/) — phase reports, transfer
  guide, housekeeping notes
