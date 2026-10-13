#!/bin/bash
# usage: run_regression.sh [suite...]
# Runs every end-to-end suite on a fresh database each. Earlier-phase suites
# exercise the OPERATOR control-plane path, so they run with the autonomous
# runtime switched off (AUTO_*=false: the paths still exist, they are just
# not taken); the autonomy suite runs with the defaults (all ON).
# Needs: PostgreSQL (:5433, role middleman), AI service (:8000), fake Graph
# API (:9090), and for UI suites the dashboard (:3000).
E2E=${E2E:-/tmp/claude-0/e2e4}; export PGPASSWORD=middleman
HERE=$(cd $(dirname $0) && pwd)
(cd $HERE/../../backend && go build -o $E2E/majo-server ./cmd/server) || exit 1
LEGACY="AUTO_CONFIRM_CLEAR_REQUESTS=false AUTO_ROUTE_DISCOVERY_REQUESTS=false AUTO_ROUTE_MATCHED_REQUESTS=false AUTO_RESOLVE_VENDOR_CONTEXT=false AUTO_CLARIFY_REQUESTS=false AUTO_CLARIFY_VENDOR_RESPONSES=false AUTO_RETRY_DELIVERY=false"
run() { # name script mode(extra env...)
  local NAME=$1 SCRIPT=$2; shift 2
  [ -f $E2E/backend.pid ] && kill $(cat $E2E/backend.pid) 2>/dev/null; sleep 1
  local DBN=majo_reg_$NAME
  psql "postgres://middleman:middleman@localhost:5433/postgres" -qc "DROP DATABASE IF EXISTS $DBN" -c "CREATE DATABASE $DBN" 2>&1 | grep -v NOTICE
  : > $E2E/outbound.jsonl; : > $E2E/uploads.jsonl; echo '{}' > $E2E/fail_plan.json
  ( env "$@" $E2E/start_backend.sh $DBN >/dev/null ) || { echo "$NAME: backend failed"; return 1; }
  ( cd $HERE/../.. && env "$@" BASE=http://localhost:8080 APP_SECRET=e2esecret OPERATOR_TOKEN=e2e-operator-token-4a4 \
      DATABASE_URL="postgres://middleman:middleman@localhost:5433/$DBN?sslmode=disable" MEDIA_DIR=$E2E/media STORAGE_DIR=$E2E/store \
      OUTBOUND_LOG=$E2E/outbound.jsonl UPLOAD_LOG=$E2E/uploads.jsonl FAIL_PLAN=$E2E/fail_plan.json FAIL_VENDOR_PHONE=2347099999999 \
      DASH=http://localhost:3000 python3 scripts/e2e/$SCRIPT > $E2E/reg-$NAME.txt 2>&1 )  # SCRIPT may carry args
  echo "$NAME exit=$? $(tail -1 $E2E/reg-$NAME.txt)"
}
STAFF="DISCOVERY_MODE=INTERNAL DISCOVERY_INTERNAL_RECIPIENTS=2347300000001,2347300000002,2347300000003"
ALL="phase31 media grouping listing curation matching lifecycle disc_commerce disc_geo disc_stale disc_expired disc_dashboard autonomy"
for s in ${@:-$ALL}; do
  case $s in
    phase31) run phase31 offer_selection_loop.py $LEGACY ;;
    media) # send, kill -9 the backend while a job waits for its retry, restart, verify
      run media "media_ingestion.py send" $LEGACY JOBS_BACKOFF_BASE=20s
      sleep 4; kill -9 $(cat $E2E/backend.pid) 2>/dev/null; sleep 1
      ( env $LEGACY JOBS_BACKOFF_BASE=20s $E2E/start_backend.sh majo_reg_media >/dev/null ) || echo "media: restart failed"
      ( cd $HERE/../.. && env BASE=http://localhost:8080 APP_SECRET=e2esecret DATABASE_URL="postgres://middleman:middleman@localhost:5433/majo_reg_media?sslmode=disable" \
          MEDIA_DIR=$E2E/media STORAGE_DIR=$E2E/store python3 scripts/e2e/media_ingestion.py verify > $E2E/reg-media-verify.txt 2>&1 )
      echo "media(verify) exit=$? $(tail -1 $E2E/reg-media-verify.txt)" ;;
    grouping) run grouping supply_grouping.py $LEGACY ;;
    listing) run listing supply_listing.py $LEGACY ;;
    curation) run curation listing_curation.py $LEGACY ;;
    matching) run matching matching_listing.py $LEGACY ;;
    lifecycle) run lifecycle supply_lifecycle.py $LEGACY ;;
    disc_commerce) run disc_commerce discovery_staff_commerce.py $LEGACY $STAFF ;;
    disc_geo) run disc_geo discovery_geo_expansion.py $LEGACY $STAFF ;;
    disc_stale) run disc_stale discovery_stale_not_expired.py $LEGACY $STAFF ;;
    disc_expired) run disc_expired discovery_expired_supply.py $LEGACY $STAFF ;;
    disc_dashboard) run disc_dashboard discovery_dashboard.py $LEGACY $STAFF ;;
    autonomy) run autonomy autonomy_scenarios.py DISCOVERY_MODE=INTERNAL DISCOVERY_INTERNAL_RECIPIENTS=2347300000001,2347300000002 JOBS_BACKOFF_BASE=1s AUTO_RETRY_MAX_ATTEMPTS=3 ;;
  esac
done
