#!/bin/bash
# usage: run_autonomy_e2e.sh [dbname]
# Fresh database + backend with the autonomous runtime at its defaults (all
# flags ON except trusted auto-publish), discovery INTERNAL for two staff
# numbers, fast job backoff, then scenarios A-G. Needs the AI service (:8000)
# and fake_graph_api.py (:9090, MEDIA_DIR, UPLOAD_LOG, FAIL_PLAN,
# FAIL_RECIPIENTS=2347099999999) already running.
set -e
(cd $(dirname $0)/../../backend && go build -o ${E2E:-/tmp/claude-0/e2e4}/majo-server ./cmd/server)
E2E=${E2E:-/tmp/claude-0/e2e4}
DBN=${1:-majo_autonomy}
export PGPASSWORD=middleman
[ -f $E2E/backend.pid ] && kill $(cat $E2E/backend.pid) 2>/dev/null || true; sleep 1
psql "postgres://middleman:middleman@localhost:5433/postgres" -qc "DROP DATABASE IF EXISTS $DBN" -c "CREATE DATABASE $DBN"
: > $E2E/outbound.jsonl; : > $E2E/uploads.jsonl; echo '{}' > $E2E/fail_plan.json
export DISCOVERY_MODE=INTERNAL DISCOVERY_INTERNAL_RECIPIENTS=2347300000001,2347300000002
export JOBS_BACKOFF_BASE=1s AUTO_RETRY_MAX_ATTEMPTS=3
$E2E/start_backend.sh $DBN
export BASE=http://localhost:8080 APP_SECRET=e2esecret OPERATOR_TOKEN=e2e-operator-token-4a4 \
  DATABASE_URL="postgres://middleman:middleman@localhost:5433/$DBN?sslmode=disable" MEDIA_DIR=$E2E/media \
  OUTBOUND_LOG=$E2E/outbound.jsonl UPLOAD_LOG=$E2E/uploads.jsonl FAIL_PLAN=$E2E/fail_plan.json
python3 $(dirname $0)/autonomy_scenarios.py
