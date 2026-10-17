# Deploying Majo to one small server (pilot)

Pilot batch item 6. Everything runs with Docker Compose on one Linux server:
Caddy (TLS + routing), the Go backend, the AI service and the dashboard. The
production database is new and separate. The dev Neon database is never used
in production, and production is never used for tests.

```
            Internet (Meta webhooks, operators' browsers)
                         │ 443 / 80
                    ┌────▼────┐
                    │  Caddy  │  TLS for MAJO_DOMAIN (Let's Encrypt)
                    └┬───────┬┘
   /api/* /webhooks/*│       │ everything else
   /health /ready    │       │
              ┌──────▼──┐ ┌──▼────────┐
              │ backend │ │ dashboard │
              └─┬─────┬─┘ └───────────┘
                │     │
          ┌─────▼┐  ┌─▼──────────────────────────┐
          │  ai  │  │ production Postgres         │
          └──────┘  │ A: managed (new project)    │
                    │ B: on this server (localdb) │
                    └─────────────────────────────┘
```

Files:

| File | What it is |
|---|---|
| `deploy/compose.prod.yml` | The production stack. Only Caddy publishes ports. |
| `deploy/Caddyfile` | TLS, path routing, security headers, log scrubbing (tokens never logged). |
| `deploy/.env.production.example` | Every production setting, documented. Copy to `deploy/.env.production` on the server. |
| `deploy/scripts/majo.sh` | `check`, `deploy`, `status`, `logs`, `backup`, `restore-test`, `operator`, `pause`, `resume`. |

The dashboard and the API share one origin (`https://MAJO_DOMAIN`), so there
is one DNS record, one certificate and no CORS. Every `/api/*` route needs an
operator token; the dashboard page itself is only a shell that asks for one.

## 1. Choose the server

- **Size:** 2 vCPU, 4 GB RAM, 40 GB+ disk. 2 GB RAM works only with swap
  (the dashboard build needs about 1.5 GB); add 2 GB swap either way.
- **OS:** Ubuntu 24.04 LTS.
- **Region:** close to the database. Users never talk to the server
  directly (everything goes through WhatsApp and Meta), so the region is
  about latency to Meta and the database, not to Nigeria. A European region
  (Frankfurt, London, Amsterdam) is a sensible default.
- Any provider with a plain VPS is fine. Turn on the provider's snapshot or
  backup add-on if it is cheap: it is a second line of defence for the media
  files.

## 2. Choose the production database

Pick one and write the choice in the handoff doc.

**A. Managed Postgres, new project (recommended in the reuse audit).**
Create a *new* project (for Neon: a separate project, not a branch of the dev
project), in the same region as the server, named for example `majo-prod`.
Use its own role and password. Take the connection string with
`sslmode=require`. Check what restore window the plan includes. The nightly
dumps below cover you either way, but point-in-time restore is better.

**B. Postgres on the server** (`--profile localdb`). Cheapest and simplest;
the database never leaves the server's private Docker network. Backups are
then entirely ours: the off-server copy (`BACKUP_RCLONE_TARGET`) is not
optional.

Either way, put the dev database's host (for example the dev Neon endpoint
id, `ep-...`) in `FORBIDDEN_DATABASE_HOSTS`, and `majo.sh check` will refuse a
production config that points at it.

## 3. Prepare the server (once)

As root, or with sudo, on a fresh Ubuntu 24.04:

```bash
# Updates, firewall (SSH + web only), automatic security updates
apt-get update && apt-get -y upgrade
apt-get install -y ufw unattended-upgrades git curl
ufw allow OpenSSH && ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 443/udp && ufw --force enable

# Swap (2 GB)
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab

# Docker Engine + Compose plugin (Docker's official convenience script)
curl -fsSL https://get.docker.com | sh

# A deploy user that can run Docker; log in with an SSH key only
adduser --disabled-password --gecos "" majo && usermod -aG docker majo
mkdir -p /home/majo/.ssh && cp ~/.ssh/authorized_keys /home/majo/.ssh/ && chown -R majo:majo /home/majo/.ssh
# Then in /etc/ssh/sshd_config: PasswordAuthentication no, PermitRootLogin no; systemctl restart ssh
```

Docker publishes ports by writing its own iptables rules, which bypass ufw.
That is why only Caddy publishes ports in `compose.prod.yml`. Never add a
`ports:` entry to another service.

## 4. DNS

Create an `A` record (and `AAAA` if the server has IPv6) for `MAJO_DOMAIN`,
for example `app.yourdomain.com`, pointing at the server. Wait until
`dig +short app.yourdomain.com` returns the server's IP. Caddy cannot get a
certificate before that.

## 5. First deploy

As the `majo` user:

```bash
git clone https://github.com/GrinchXoXo/majo.git ~/majo && cd ~/majo
git checkout <release tag or commit>
cp deploy/.env.production.example deploy/.env.production
chmod 600 deploy/.env.production
nano deploy/.env.production          # fill in; see the comments in the file
deploy/scripts/majo.sh check         # must say "env OK"
deploy/scripts/majo.sh deploy        # builds, starts, waits for /ready
```

The repository is private, so the server needs read access: use a
**read-only deploy key** (GitHub → repo → Settings → Deploy keys), not a
personal token.

Secrets:

- Generate tokens with `openssl rand -hex 32` (operator token, verify
  token) and, for option B, `openssl rand -hex 24` for `POSTGRES_PASSWORD`.
  Hex values avoid two traps: URL-encoding in `DATABASE_URL`, and Docker
  Compose rewriting `$` in values.
- If a value from a provider contains `$`, wrap it in single quotes:
  `DATABASE_URL='postgres://...'`. `majo.sh check` catches this.
- `.env.production` stays on the server only (mode 600). It is gitignored.

What `deploy` does:

1. runs `check`;
2. refuses to run if the checkout has local changes;
3. takes a backup if the stack is already running (migrations are forward-only);
4. builds the images and starts everything; migrations run automatically on
   backend start;
5. waits until `/ready` reports Postgres and the AI service OK;
6. checks `https://MAJO_DOMAIN/health` from outside;
7. appends the release to `deploy/.deploy-history`.

## 6. Connect WhatsApp (Meta)

In the Meta App dashboard → WhatsApp → Configuration:

- Callback URL: `https://MAJO_DOMAIN/webhooks/whatsapp`
- Verify token: the value of `WHATSAPP_VERIFY_TOKEN`
- Subscribe to the `messages` field.

Meta verifies the URL immediately. Incoming POSTs must carry a valid
`X-Hub-Signature-256` (checked against `WHATSAPP_APP_SECRET`), otherwise the
backend answers 401. Then follow `docs/pilot/whatsapp-setup-checklist.md`, and
submit the vendor template from `docs/pilot/whatsapp-templates.md`.

## 7. Operators

The shared `OPERATOR_API_TOKEN` is the break-glass credential. Give each
person a named token:

```bash
deploy/scripts/majo.sh operator create -name "Godyes" -role OPERATOR
deploy/scripts/majo.sh operator create -name "Helper" -role VIEWER
deploy/scripts/majo.sh operator list
deploy/scripts/majo.sh operator revoke -name "Helper"
```

The token is printed once. Give it to the person directly, never in a group
chat. They paste it into the dashboard's token field.

## 8. Backups and the restore drill

```bash
deploy/scripts/majo.sh backup
```

This writes `deploy/backups/majo-db-<time>.dump` (pg_dump custom format) and
`majo-media-<time>.tar.gz` (vendor photos and videos). It keeps
`BACKUP_KEEP_DAYS` days locally and, when `BACKUP_RCLONE_TARGET` is set,
copies both files off the server with rclone. Install rclone and run
`rclone config` once; any S3-compatible bucket works.

Schedule it nightly (`crontab -e` as `majo`):

```
15 2 * * * /home/majo/majo/deploy/scripts/majo.sh backup >> /home/majo/majo-backup.log 2>&1
```

**Restore drill: do it once before the pilot, then monthly.**

```bash
deploy/scripts/majo.sh restore-test deploy/backups/majo-db-<time>.dump
```

This restores the dump into a throwaway Postgres container with no network,
prints row counts (migrations, buyer requests, vendors, events, deals), and
deletes the container. A backup that has never been restored is not a
backup.

**Real restore** (after data loss):

1. Pause automation: `majo.sh pause "restoring database"`.
2. Restore into a **new, empty** database, never over the damaged one:
   `pg_restore --no-owner --no-acl -d "<new DATABASE_URL>" majo-db-<time>.dump`
   (from the `dbtools` container:
   `majo.sh compose --profile tools run --rm dbtools pg_restore ...`).
3. Point `DATABASE_URL` at the new database and run `majo.sh deploy`.
4. Media: `tar -xzf majo-media-<time>.tar.gz` into the `majo_media_data`
   volume.
5. Check the dashboard, then `majo.sh resume "restore complete"`.

With option A, the provider's point-in-time restore (to a new branch or
database) is usually faster and loses less data than last night's dump. Use
it first if the plan has it.

## 9. Updating and rolling back

```bash
cd ~/majo
deploy/scripts/majo.sh deploy v0.2.0        # a tag, or origin/<branch>, or a commit
deploy/scripts/majo.sh status
deploy/scripts/majo.sh logs backend
```

Rollback means deploying the previous ref (see `deploy/.deploy-history`).
Migrations are forward-only: rolling the code back is safe only while the
newer migrations are additive (new tables or columns), which has been the
rule so far. If a release changed data, restore the pre-deploy backup
instead.

## 10. Incidents

- Stop everything Majo does on its own, from the server, even when the
  dashboard is broken:
  `deploy/scripts/majo.sh pause "what happened"`; later
  `deploy/scripts/majo.sh resume "why it is safe"`.
- Health: `majo.sh status`, or `https://MAJO_DOMAIN/ready`.
- Restart one service: `majo.sh compose restart backend`.
- Full runbook: `docs/pilot/incident-runbook-draft.md`.
- Optional uptime alerts: point any free uptime monitor at
  `https://MAJO_DOMAIN/ready` (it returns 503 when Postgres or the AI service
  is down).
- Optional error tracking: set `BACKEND_SENTRY_DSN` / `AI_SENTRY_DSN`
  (GlitchTip; see `docker-compose.yml`'s observability profile).

## What was verified, and what was not

Verified in the build sandbox (no Docker daemon available there):

- `docker compose config` is valid for both database options and the tools
  profile.
- Caddy 2.10 validated the Caddyfile, then ran it in front of the real
  backend (APP_ENV=production), AI service and production-built dashboard:
  - routing to the right service;
  - 401 without a token, 200 with one, 403 for a VIEWER trying to pause;
  - Meta verification challenge;
  - unsigned webhook rejected; bodies over 2 MB rejected;
  - security headers present;
  - tokens and the verify token absent from the logs.
- The dashboard bundle contains the production API URL. Before this
  change, the Dockerfile ignored it and the browser would have called
  `localhost:8080`.
- `majo.sh check` catches the error cases it is built for:
  - empty values;
  - a dev host;
  - localhost;
  - no TLS;
  - mismatched passwords;
  - unquoted `$`.
- `majo.sh pause` and `resume` work against the live API.
- The backup command was run with native Postgres tools, and the dump
  restored and counted with the same SQL that `restore-test` runs.

Not verified here: building the Docker images and running the stack under
Docker, real Let's Encrypt issuance, and the rclone copy. The first real
deploy (or `docker compose -f deploy/compose.prod.yml build` on a machine
with Docker) is the first time those run.
