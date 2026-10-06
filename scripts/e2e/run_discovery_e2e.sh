#!/bin/bash
# usage: run_discovery_e2e.sh <script.py> [dbname]
# Fresh database + backend in DISCOVERY_MODE=INTERNAL (staff allowlist only), then the E2E.
# Needs the AI service (:8000) and fake_graph_api.py (:9090, MEDIA_DIR + UPLOAD_LOG) already running.
set -e
E2E=${E2E:-/tmp/claude-0/e2e4}
SCRIPT=$1; DBN=${2:-majo_disc_$(basename $SCRIPT .py)}
export PGPASSWORD=middleman
[ -f $E2E/backend.pid ] && kill $(cat $E2E/backend.pid) 2>/dev/null || true; sleep 1
psql "postgres://middleman:middleman@localhost:5433/postgres" -qc "DROP DATABASE IF EXISTS $DBN" -c "CREATE DATABASE $DBN"
: > $E2E/outbound.jsonl; : > $E2E/uploads.jsonl
export DISCOVERY_MODE=INTERNAL DISCOVERY_INTERNAL_RECIPIENTS=${STAFF:-2347300000001,2347300000002,2347300000003}
$E2E/start_backend.sh $DBN
export BASE=http://localhost:8080 APP_SECRET=e2esecret OPERATOR_TOKEN=e2e-operator-token-4a4 \
  DATABASE_URL="postgres://middleman:middleman@localhost:5433/$DBN?sslmode=disable" MEDIA_DIR=$E2E/media \
  OUTBOUND_LOG=$E2E/outbound.jsonl UPLOAD_LOG=$E2E/uploads.jsonl
python3 $(dirname $0)/$SCRIPT
