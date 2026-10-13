#!/bin/bash
# Majo security gate (reuse-audit R0): secrets, dependency vulnerabilities and
# Dockerfile misconfiguration. Read-only; run from anywhere in the repo.
#   scripts/security/run_security_checks.sh          # all checks
#   SKIP=trivy scripts/security/run_security_checks.sh
# Exit code: number of checks that FAILED (a missing tool is SKIPPED, not failed).
# GOVULNDB=file://... points govulncheck at an offline database copy.
# Pinned tool versions are listed in docs/security/security-checks.md.
ROOT=$(git rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "$0")/../.." && pwd))
cd "$ROOT" || exit 1
FAILED=0
have() { command -v "$1" >/dev/null 2>&1; }
skip() { [[ ",$SKIP," == *",$1,"* ]]; }
step() { printf '\n== %s\n' "$1"; }
result() { if [ "$1" -eq 0 ]; then echo "-- $2: PASS"; else echo "-- $2: FAIL (exit $1)"; FAILED=$((FAILED+1)); fi; }

step "Secrets in git history (gitleaks)"
if skip gitleaks; then echo "-- skipped"; elif have gitleaks; then
  gitleaks git --redact --no-banner --config .gitleaks.toml .; result $? gitleaks
else echo "-- gitleaks not installed: SKIPPED"; fi

step "Go: reachable vulnerabilities (govulncheck)"
if skip govulncheck; then echo "-- skipped"; elif have govulncheck; then
  (cd backend && govulncheck ${GOVULNDB:+-db "$GOVULNDB"} ./...); result $? govulncheck
else echo "-- govulncheck not installed: SKIPPED"; fi

step "Dashboard: npm audit (production dependencies)"
if skip npm; then echo "-- skipped"; elif have npm; then
  (cd dashboard && npm audit --omit=dev); result $? "npm audit"
else echo "-- npm not installed: SKIPPED"; fi

step "AI service: pip-audit"
if skip pip-audit; then echo "-- skipped"; elif have pip-audit; then
  (cd ai && pip-audit -r requirements.txt --desc off); result $? pip-audit
else echo "-- pip-audit not installed: SKIPPED"; fi

step "Dockerfiles and config (trivy config)"
if skip trivy; then echo "-- skipped"; elif have trivy; then
  trivy config --quiet --exit-code 1 .; result $? "trivy config"
else echo "-- trivy not installed: SKIPPED"; fi

printf '\n%s check(s) failed\n' "$FAILED"
exit $FAILED
