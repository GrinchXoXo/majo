# Security checks

Reuse-audit R0. All checks are read-only and run on a developer machine (or CI
later); none of them is a runtime service.

## One command

```
scripts/security/run_security_checks.sh          # exit code = number of failed checks
SKIP=trivy scripts/security/run_security_checks.sh
```

A tool that is not installed is reported as SKIPPED, not failed.

| Check | Tool (pinned) | What it catches |
|---|---|---|
| Secrets in git history | Gitleaks 8.30.1 + `.gitleaks.toml` | Committed credentials, including PostgreSQL URLs with a password (e.g. a Neon connection string). The default rules do not catch those; `.gitleaks.toml` adds a rule. |
| Go vulnerabilities | govulncheck v1.9.0 | Known vulnerabilities that Majo's code actually reaches (stdlib and modules). |
| Dashboard dependencies | `npm audit --omit=dev` (npm built-in) | Advisories in production npm packages. |
| AI service dependencies | pip-audit 2.10.1 | Advisories in `ai/requirements.txt`. |
| Dockerfiles / config | Trivy 0.75.0 (`trivy config`) | Root containers, missing health checks, unpinned base images, etc. |

## Install (pinned versions)

- Gitleaks: download `gitleaks_8.30.1_<os>_<arch>` from
  github.com/gitleaks/gitleaks/releases/tag/v8.30.1 and check it against
  `gitleaks_8.30.1_checksums.txt` from the same release.
- govulncheck: `go install golang.org/x/vuln/cmd/govulncheck@v1.9.0`
- pip-audit: `pip install pip-audit==2.10.1` (in a separate virtualenv)
- Trivy: download v0.75.0 from github.com/aquasecurity/trivy/releases and
  verify it against that release's checksums file. **Never use Trivy
  v0.69.4, or Docker Hub images 0.69.5 / 0.69.6.** Those were the malicious
  releases in the March 2026 supply-chain compromise. Never reference
  `aquasecurity/trivy-action` or `setup-trivy` by tag: pin a full commit SHA.

## Pre-commit secret scan

Enable it once per clone:

```
git config core.hooksPath .githooks
```

`.githooks/pre-commit` runs `gitleaks git --staged` and blocks the commit when
it finds a secret. If gitleaks is not installed, the hook warns and lets the
commit through.

## Offline vulnerability database

`GOVULNDB=file:///path/to/db` points govulncheck at an offline copy of the Go
vulnerability database. Use it only where vuln.go.dev is unreachable; the
online database is the authoritative source.

## Results on 13 Oct 2026 (sandbox)

Before is at commit `930b1eb`; after is with this change.

| Check | Before | After |
|---|---|---|
| Gitleaks (git history, 32 commits on all branches) | no leaks | no leaks |
| govulncheck | 36 reachable stdlib vulnerabilities (Go 1.24.7 toolchain; the Docker image used EOL Go 1.22, which is worse); 0 in modules | **No vulnerabilities found** (Go 1.26.9) |
| npm audit (dashboard) | 1 critical (next 14.2.15), 2 high (postcss, source-map-js) | **0** (next 16.4.0, react 19.3.0) |
| pip-audit (ai) | 16 known vulnerabilities (starlette 0.38.6 via fastapi 0.115.0; pytest 8.3.3) | **0** |
| trivy config | 7 misconfigurations: 3 root containers (HIGH), 1 `:latest` base (MEDIUM), 3 missing health checks (LOW) | **0** |

In the sandbox, govulncheck used a database built from the
github.com/golang/vulndb repository (commit of 2026-10-10), because vuln.go.dev
is unreachable there.

Trivy's vulnerability database (ghcr.io) was also unreachable, so only
`trivy config` was run. Image and filesystem CVE scanning with Trivy is **NOT
RUN**; the language-native scanners above cover the application dependencies.
