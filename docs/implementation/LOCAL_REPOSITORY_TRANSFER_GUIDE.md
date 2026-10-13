# Local repository transfer guide: Phase 4C correction + hardening

This guide is for the **next session**, once the owner connects their local repository:

- Windows path: `C:\Users\HomePC\Downloads\majo\middleman`.
- It is reached through the Claude desktop app bridge (`mcp__remote-devices__*`).

**Nothing in this guide has been executed against the local repository yet.**

The sandbox source is branch `phase-4a.1-media-ingestion`. `ANCESTRY.txt` has the exact hashes and these commits:

| Commit | Content |
|---|---|
| `6f11ab1` | Phase 4B (base) |
| `3aae7c8` | Phase 4C autonomy correction |
| `7b5b5dc`, `7b69963`, `111ed1f` | Phase 4C hardening |
| final commit | This report and guide |

## 0. Ground rules

- **Inspect before editing.** Do not modify the working tree until steps 1–6 pass.
- **Never** reset, clean, stash-drop, rebase, force-push, or discard local changes. Never `git checkout -- .`.
- Apply on a **new branch**. Leave the owner's current branch untouched.
- **Never run migrations or integration tests against the Neon database** in `.env`. Never print or commit `.env`.
- Use a local disposable Postgres (`MAJO_TEST_DATABASE_URL`). If none exists, mark DB-dependent tests **NOT RUN**.
- **Stop and report** on any conflict, unexplained divergence, or failing check. Do not "fix forward" silently.

## 1. Verify the transfer package

Copy `majo-4c-hardening-transfer.zip` to a folder **outside** the repository (for example `Downloads\majo-transfer`), unzip it, and run:

```
cd <transfer folder>
sha256sum -c SHA256SUMS          # PowerShell: Get-FileHash -Algorithm SHA256 <file>, compare by hand
git bundle verify majo-4c-full-branch.bundle
```

Every line must report OK.

## 2. Inspect the local repository (read-only)

```
cd C:\Users\HomePC\Downloads\majo\middleman
git rev-parse --is-inside-work-tree   # is it a git repo at all?
git status --porcelain=v1             # uncommitted changes: RECORD them, do not touch
git branch --show-current
git log --oneline -15
git stash list
git remote -v
```

Record the branch, HEAD and the full `status` output in the session notes.

## 3. Compare history with the sandbox

```
git cat-file -t 6f11ab1     # Phase 4B base present?
git cat-file -t 3aae7c8     # correction already present?
git cat-file -t 111ed1f     # hardening already present?
git merge-base --is-ancestor 6f11ab1 HEAD && echo "HEAD contains Phase 4B"
```

Decide the case:

| Case | Condition | Method |
|---|---|---|
| A | `6f11ab1` present, `3aae7c8` absent | Bundle (§5A), or `git am` all patches |
| B | `3aae7c8` present, hardening absent | Bundle (§5A), or `git am` the hardening patches only |
| C | Hardening commits already present | Nothing to apply; go to §7 and verify |
| D | No shared history (repo built from the zip `majo-repo-6f11ab1.zip`, a different `git init`, or not a git repo) | Patch with `git apply --3way` (§5B) |
| E | Local history diverged in other ways (local commits on top of something else) | Stop. Report the divergence and ask the owner. |

## 4. Check whether equivalent changes already exist

Even without the commits, the code may already contain the work:

```
git grep -n "VENDOR_REQUEST_TIMEOUT" -- backend || echo "deadline job absent"
git grep -n "func Classify(err error) Failure" -- backend/internal/channel || echo "classifier absent"
ls backend/migrations/0014_autonomous_runtime.sql backend/migrations/0015_autonomy_hardening.sql
```

If some of the work is present under different commits, compare file by file:

```
git diff --no-index <sandbox file from the full-tree zip> <local file>
```

Report the overlap before applying anything.

## 5. Dry run, then apply (only on a clean slate for the touched files)

First check that the local uncommitted changes do not touch the same files:

```
git status --porcelain | cut -c4- > local_changed.txt
# compare with the "path" list in MANIFEST.json; ANY overlap -> stop and report
```

### 5A. Shared history (cases A and B): Git bundle (preferred)

```
git fetch <transfer>/majo-4c-full-branch.bundle phase-4a.1-media-ingestion:refs/remotes/sandbox/phase-4c
git log --oneline HEAD..sandbox/phase-4c          # must list ONLY the expected Phase 4C commits
git switch -c phase-4c-hardening sandbox/phase-4c  # new branch; the owner's branch is untouched
```

Merge into the owner's working branch **only if the owner asks**. Use `git merge --ff-only` if possible; otherwise stop and report.

Alternative: `git am`.

```
git switch -c phase-4c-hardening
git am --3way <transfer>/patches/*.patch   # case B: only patches 0002+
```

On any conflict, run `git am --abort` and report.

### 5B. No shared history (case D): cumulative patch

```
git switch -c phase-4c-hardening        # if it is a git repo; otherwise copy the folder first
git apply --check --3way <transfer>/majo-4c-cumulative-from-6f11ab1.patch
# or, if the local code already has 3aae7c8's content:
git apply --check --3way <transfer>/majo-4c-hardening-only-from-3aae7c8.patch
```

`--check` must print nothing. Then apply:

```
git apply --3way --index <same patch>
git commit -m "Apply Phase 4C correction + hardening from sandbox (<source commit>)"
```

### Bundle vs. patch

| | Bundle | Patch |
|---|---|---|
| Carries | The exact commits, authorship and history | Only the text changes |
| Needs | `6f11ab1` (or `3aae7c8`) in the local history | Files whose content matches the base |
| Verification | `git log` shows identical hashes | Rebuilds the content, not the history |
| Use when | History is shared | History differs (case D), or for review |

## 6. Verify the resulting diff

```
git diff --stat <base>..phase-4c-hardening                     # compare with MANIFEST.json (paths and counts)
git diff <base>..phase-4c-hardening -- backend/migrations      # must show only 0014 and 0015 added
```

- The tree must equal the sandbox tree. Compare `git rev-parse phase-4c-hardening^{tree}` with the `tree` line in `ANCESTRY.txt` (bundle, `git am` and patch paths alike).
- No file outside the manifest may change.
- The local uncommitted changes must still be present: run `git status`. If they were on another branch, `git stash list` must be unchanged.

## 7. Migrations and tests (local safeguards only)

- **Migrations:** 0014 (autonomous runtime) and 0015 (autonomy hardening). They run automatically when the server starts. Start the server **only** with a local disposable `DATABASE_URL`, never Neon.
- **Go:**

  ```
  cd backend
  go build ./...
  go vet ./...
  go test ./...                                            # unit; integration tests skip without MAJO_TEST_DATABASE_URL
  MAJO_TEST_DATABASE_URL=postgres://...local.../postgres?sslmode=disable go test ./...   # local Postgres only
  go test -race ./...
  ```

- **AI:** `cd ai && python -m pytest -q`
- **Dashboard:** `cd dashboard && npx tsc --noEmit && npx next build`
- **E2E** (`scripts/e2e/run_regression.sh`) needs Linux tooling (bash, psql, a fake Graph API on :9090). On Windows, mark it **NOT RUN** unless a local equivalent is available.
- **Environment:** add the new variables (see `CONFIG_AND_MIGRATIONS.md`). All have safe defaults. Two need the owner's decision: `AUTO_RESTRICTED_ITEM_TERMS` and `AUTO_BROADEN_EXCLUDED_CATEGORIES`.

## 8. Report

Report the following:

- the case (A–E);
- the method used;
- the new branch and its HEAD;
- the commits or files applied;
- anything skipped and why;
- the local uncommitted changes, confirmed preserved;
- the test commands with exit codes;
- what remains unresolved (the hardening report §J).

Do not claim success for anything not verified.
