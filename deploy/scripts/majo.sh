#!/usr/bin/env bash
# Majo production helper for one small server (pilot batch item 6).
#
#   deploy/scripts/majo.sh check                 validate deploy/.env.production
#   deploy/scripts/majo.sh deploy [git-ref]      backup, build, start, wait until ready
#   deploy/scripts/majo.sh status                containers + readiness
#   deploy/scripts/majo.sh logs [service]        follow logs (default: backend)
#   deploy/scripts/majo.sh backup                database dump + media archive
#   deploy/scripts/majo.sh restore-test FILE     restore a dump into a scratch DB and count rows
#   deploy/scripts/majo.sh operator ARGS...      operatorctl (create/list/revoke named tokens)
#   deploy/scripts/majo.sh pause "reason"        pause ALL automation (incident switch)
#   deploy/scripts/majo.sh resume "reason"       resume automation
#   deploy/scripts/majo.sh compose ARGS...       any docker compose command with the right flags
#
# Never prints secret values.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEPLOY="$ROOT/deploy"
ENV_FILE="${MAJO_ENV_FILE:-$DEPLOY/.env.production}"
ENV_FILE="$(cd "$(dirname "$ENV_FILE")" 2>/dev/null && pwd)/$(basename "$ENV_FILE")"
export MAJO_ENV_FILE="$ENV_FILE"
COMPOSE_FILE="$DEPLOY/compose.prod.yml"
BACKUP_DIR="$DEPLOY/backups"

die() { echo "majo: $*" >&2; exit 1; }
note() { echo "majo: $*"; }

# envget KEY: the value of KEY in the env file ("" if absent). Handles
# KEY=value, KEY="value" and KEY='value'; ignores comments. Does not evaluate
# anything, so values may contain $, spaces or quotes.
envget() {
	local key="$1" line val=""
	[[ -f "$ENV_FILE" ]] || { echo ""; return; }
	while IFS= read -r line || [[ -n "$line" ]]; do
		line="${line%$'\r'}"
		[[ "$line" =~ ^[[:space:]]*# ]] && continue
		if [[ "$line" == "$key="* ]]; then
			val="${line#*=}"
		fi
	done <"$ENV_FILE"
	if [[ ${#val} -ge 2 && ( ( "${val:0:1}" == '"' && "${val: -1}" == '"' ) || ( "${val:0:1}" == "'" && "${val: -1}" == "'" ) ) ]]; then
		val="${val:1:${#val}-2}"
	fi
	printf '%s' "$val"
}

# db_host: the host part of DATABASE_URL (no credentials).
db_host() {
	local url rest
	url="$(envget DATABASE_URL)"
	rest="${url#*://}"
	rest="${rest##*@}"
	rest="${rest%%/*}"
	rest="${rest%%\?*}"
	printf '%s' "${rest%%:*}"
}

uses_local_db() { [[ "$(db_host)" == "postgres" ]]; }

compose() {
	local profiles=()
	if uses_local_db; then
		profiles=(--profile localdb)
		export DBTOOLS_PG_MAJOR="${DBTOOLS_PG_MAJOR:-16}"
	fi
	docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" "${profiles[@]}" "$@"
}

# --- check ---------------------------------------------------------------

check() {
	local errors=0 warnings=0
	err() { echo "  ERROR: $*" >&2; errors=$((errors + 1)); }
	warn() { echo "  warning: $*" >&2; warnings=$((warnings + 1)); }

	[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE (copy deploy/.env.production.example and fill it in)"
	local mode
	mode="$(stat -c %a "$ENV_FILE" 2>/dev/null || echo "?")"
	[[ "$mode" == "600" || "$mode" == "400" ]] || warn "$ENV_FILE is mode $mode; run: chmod 600 $ENV_FILE"

	# Docker Compose expands $NAME inside env-file values (even in double
	# quotes), which silently corrupts a password containing "$". Only single
	# quotes keep a value literal.
	local line lineno=0 raw
	while IFS= read -r line || [[ -n "$line" ]]; do
		lineno=$((lineno + 1))
		line="${line%$'\r'}"
		[[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
		[[ "$line" == *=* ]] || { err "line $lineno is not KEY=value"; continue; }
		raw="${line#*=}"
		if [[ "$raw" == *'$'* && ! ( "${raw:0:1}" == "'" && "${raw: -1}" == "'" ) ]]; then
			err "line $lineno (${line%%=*}) contains \$: wrap the value in single quotes, or Docker Compose will rewrite it"
		fi
		if [[ "$raw" == *" #"* && "${raw:0:1}" != "'" && "${raw:0:1}" != '"' ]]; then
			warn "line $lineno (${line%%=*}): ' #' starts a comment in an unquoted value; quote the value if the # is part of it"
		fi
	done <"$ENV_FILE"

	local domain
	domain="$(envget MAJO_DOMAIN)"
	if [[ -z "$domain" || "$domain" == *example.com ]]; then err "MAJO_DOMAIN is not set"; fi
	[[ "$domain" == http* || "$domain" == */* ]] && err "MAJO_DOMAIN is a bare host name (no https://, no path)"
	local email
	email="$(envget ACME_EMAIL)"
	if [[ -z "$email" || "$email" == *example.com ]]; then err "ACME_EMAIL is not set"; fi

	local url host
	url="$(envget DATABASE_URL)"
	host="$(db_host)"
	if [[ -z "$url" || "$url" == *USER:PASSWORD@HOST* ]]; then
		err "DATABASE_URL is not set"
	elif [[ "$url" != postgres://* && "$url" != postgresql://* ]]; then
		err "DATABASE_URL must start with postgres://"
	elif [[ "$host" == "localhost" || "$host" == 127.* || "$host" == "::1" ]]; then
		err "DATABASE_URL points at localhost, which inside a container is the container itself; use host 'postgres' (--profile localdb) or the managed database's host"
	elif [[ "$host" == "postgres" ]]; then
		local pw
		pw="$(envget POSTGRES_PASSWORD)"
		[[ ${#pw} -ge 20 ]] || err "POSTGRES_PASSWORD must be at least 20 characters for the on-server database"
		[[ "$url" == *":$pw@postgres"* ]] || err "DATABASE_URL's password does not match POSTGRES_PASSWORD"
	else
		[[ "$url" == *sslmode=require* || "$url" == *sslmode=verify-full* || "$url" == *sslmode=verify-ca* ]] ||
			err "DATABASE_URL to a remote host must use sslmode=require (or verify-full)"
	fi
	local forbidden f
	forbidden="$(envget FORBIDDEN_DATABASE_HOSTS)"
	IFS=',' read -r -a fs <<<"$forbidden"
	for f in "${fs[@]}"; do
		f="${f// /}"
		[[ -n "$f" && "$url" == *"$f"* ]] && err "DATABASE_URL matches FORBIDDEN_DATABASE_HOSTS entry '$f' (the dev database must never be used in production)"
	done

	local k
	for k in WHATSAPP_ACCESS_TOKEN WHATSAPP_PHONE_NUMBER_ID WHATSAPP_VERIFY_TOKEN WHATSAPP_APP_SECRET; do
		[[ -n "$(envget "$k")" ]] || err "$k is not set"
	done
	local tok
	tok="$(envget OPERATOR_API_TOKEN)"
	[[ ${#tok} -ge 32 ]] || err "OPERATOR_API_TOKEN must be at least 32 characters (openssl rand -hex 32)"
	[[ "$(envget APP_ENV)" == "" || "$(envget APP_ENV)" == "production" ]] || warn "APP_ENV in the env file is ignored; production always runs APP_ENV=production"
	[[ -n "$(envget ANTHROPIC_API_KEY)" ]] || warn "ANTHROPIC_API_KEY is empty: the heuristic extractor will run"
	[[ -n "$(envget VENDOR_REQUEST_TEMPLATE_NAME)" ]] || warn "VENDOR_REQUEST_TEMPLATE_NAME is empty: vendors outside the 24-hour window cannot be reached"
	local disc
	disc="$(envget DISCOVERY_MODE)"
	[[ -z "$disc" || "$disc" == "OFF" ]] || warn "DISCOVERY_MODE=$disc"
	[[ -n "$(envget BACKUP_RCLONE_TARGET)" ]] || warn "BACKUP_RCLONE_TARGET is empty: backups stay on this server only"

	if ((errors > 0)); then
		echo "majo: $errors error(s), $warnings warning(s) in $ENV_FILE" >&2
		return 1
	fi
	note "env OK ($warnings warning(s)); database host: $host"
}

# --- readiness -----------------------------------------------------------

wait_ready() {
	local i
	for i in $(seq 1 60); do
		if compose exec -T caddy wget -qO- http://backend:8080/ready >/dev/null 2>&1; then
			note "backend ready (database and AI service reachable)"
			return 0
		fi
		sleep 5
		((i % 6 == 0)) && note "still waiting for /ready ($((i * 5))s)"
	done
	compose ps
	die "backend not ready after 5 minutes; see: deploy/scripts/majo.sh logs backend"
}

public_check() {
	local domain
	domain="$(envget MAJO_DOMAIN)"
	if curl -fsS --max-time 15 "https://$domain/health" >/dev/null 2>&1; then
		note "https://$domain/health OK (TLS certificate in place)"
	else
		note "https://$domain/health not reachable yet; on a first deploy the certificate can take a minute. Check DNS and that ports 80/443 are open."
	fi
}

# --- backup / restore ----------------------------------------------------

backup() {
	mkdir -p "$BACKUP_DIR"
	chmod 700 "$BACKUP_DIR"
	local ts db media keep target
	ts="$(date -u +%Y%m%dT%H%M%SZ)"
	db="majo-db-$ts.dump"
	media="majo-media-$ts.tar.gz"
	note "dumping database to deploy/backups/$db"
	# shellcheck disable=SC2016 # expanded inside the container, on purpose
	# The password goes to pg_dump through PGPASSWORD, not on its command
	# line (visible to ps). A percent-encoded password (rare with the
	# recommended hex passwords) stays in the URL.
	compose --profile tools run --rm -T dbtools sh -c '
		url="$DATABASE_URL"
		pw=$(printf "%s" "$url" | sed -nE "s#^[a-z]+://[^:/@]*:([^@]*)@.*#\1#p")
		case "$pw" in
		"" | *%*) ;;
		*)
			export PGPASSWORD="$pw"
			url=$(printf "%s" "$url" | sed -E "s#^([a-z]+://[^:/@]*):[^@]*@#\1@#")
			;;
		esac
		pg_dump --format=custom --no-owner --no-acl --dbname="$url" --file="/backups/$1.partial" && mv "/backups/$1.partial" "/backups/$1"' \
		sh "$db"
	note "archiving media to deploy/backups/$media"
	# shellcheck disable=SC2016 # expanded inside the container, on purpose
	compose --profile tools run --rm -T dbtools sh -c \
		'tar -czf "/backups/$1.partial" -C /media . && mv "/backups/$1.partial" "/backups/$1"' sh "$media"
	chmod 600 "$BACKUP_DIR/$db" "$BACKUP_DIR/$media" 2>/dev/null || true

	keep="$(envget BACKUP_KEEP_DAYS)"
	keep="${keep:-14}"
	find "$BACKUP_DIR" -maxdepth 1 -name 'majo-*' -type f -mtime +"$keep" -print -delete | sed 's/^/majo: removed old backup /'

	target="$(envget BACKUP_RCLONE_TARGET)"
	if [[ -n "$target" ]]; then
		command -v rclone >/dev/null || die "BACKUP_RCLONE_TARGET is set but rclone is not installed"
		rclone copy "$BACKUP_DIR/$db" "$target" && rclone copy "$BACKUP_DIR/$media" "$target"
		note "copied off-server to $target"
	else
		note "no off-server copy (BACKUP_RCLONE_TARGET is empty)"
	fi
	note "backup done: $db, $media"
}

restore_test() {
	local file="${1:-}" major name
	[[ -n "$file" && -f "$file" ]] || die "usage: majo.sh restore-test deploy/backups/majo-db-<time>.dump"
	file="$(cd "$(dirname "$file")" && pwd)/$(basename "$file")"
	major="${DBTOOLS_PG_MAJOR:-17}"
	name="majo-restore-test-$$"
	note "restoring $(basename "$file") into a scratch postgres:$major (no network, removed afterwards)"
	docker run -d --rm --name "$name" --network none -e POSTGRES_PASSWORD=scratch \
		-v "$file:/restore.dump:ro" "postgres:$major-alpine" >/dev/null
	trap 'docker rm -f "$name" >/dev/null 2>&1 || true' RETURN
	local i
	for i in $(seq 1 30); do
		docker exec "$name" pg_isready -U postgres >/dev/null 2>&1 && break
		sleep 1
	done
	docker exec "$name" createdb -U postgres majo
	docker exec "$name" pg_restore --no-owner --no-acl -U postgres -d majo /restore.dump ||
		note "pg_restore reported errors (see above); version-specific SET lines are harmless, anything else is not"
	docker exec "$name" psql -U postgres -d majo -At -c "
		SELECT 'migrations applied: ' || count(*) FROM schema_migrations
		UNION ALL SELECT 'buyer requests: ' || count(*) FROM buyer_requests
		UNION ALL SELECT 'vendors: ' || count(*) FROM vendors
		UNION ALL SELECT 'events: ' || count(*) FROM events
		UNION ALL SELECT 'deals: ' || count(*) FROM deals;"
	note "restore test finished"
}

# --- deploy --------------------------------------------------------------

deploy() {
	local ref="${1:-}" prev
	check
	cd "$ROOT"
	if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
		die "the checkout has local changes; the server must run committed code only (git status)"
	fi
	prev="$(git rev-parse HEAD)"
	if [[ -n "$ref" ]]; then
		git fetch --tags origin
		git checkout --detach "$ref"
	fi
	export MAJO_RELEASE
	MAJO_RELEASE="$(git rev-parse --short HEAD)"
	note "deploying $MAJO_RELEASE (previous $(git rev-parse --short "$prev"))"

	if compose ps --status running --services 2>/dev/null | grep -qx backend; then
		note "backup before deploy (migrations are forward-only)"
		backup
	else
		note "backend not running: skipping the pre-deploy backup (first deploy?)"
		if uses_local_db; then compose up -d postgres; fi
	fi

	compose build --pull
	compose up -d --remove-orphans
	wait_ready
	public_check
	echo "$MAJO_RELEASE $(date -u +%FT%TZ) previous=$(git rev-parse --short "$prev")" >>"$DEPLOY/.deploy-history"
	note "deployed $MAJO_RELEASE"
}

status() {
	compose ps
	if compose exec -T caddy wget -qO- http://backend:8080/ready 2>/dev/null; then echo; else note "backend NOT ready"; fi
	[[ -f "$DEPLOY/.deploy-history" ]] && { note "last deploys:"; tail -n 5 "$DEPLOY/.deploy-history"; }
}

# --- incident switch -----------------------------------------------------

automation() {
	local action="$1" reason="${2:-}" domain tok body
	[[ -n "$reason" ]] || die "usage: majo.sh $action \"reason\""
	domain="$(envget MAJO_DOMAIN)"
	tok="$(envget OPERATOR_API_TOKEN)"
	reason="${reason//\\/\\\\}"
	reason="${reason//\"/\\\"}"
	reason="${reason//$'\n'/ }"
	reason="${reason//$'\r'/ }"
	reason="${reason//$'\t'/ }"
	body="{\"reason\":\"$reason\"}"
	# The token goes in through a file descriptor, never on the command line.
	curl -fsS --max-time 20 -X POST "https://$domain/api/autonomy/$action" \
		-H @<(printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$tok") \
		--data "$body"
}

cmd="${1:-}"
shift || true
case "$cmd" in
check) check ;;
deploy) deploy "${1:-}" ;;
status) status ;;
logs) compose logs -f --tail=200 "${1:-backend}" ;;
backup) backup ;;
restore-test) restore_test "${1:-}" ;;
operator) compose exec -T backend /app/operatorctl "$@" ;;
pause) automation pause "${1:-}" ;;
resume) automation resume "${1:-}" ;;
compose) compose "$@" ;;
*) sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
