---
last_reviewed: 2026-06-28
tracks_code: [internal/git/**, internal/cli/add.go, internal/cli/hydrate.go, internal/cli/open.go, internal/cli/repo_lock.go, internal/cli/worktree.go]
---
# Git Materialization and Worktree Design

## Principle

Git is the source of truth for code content. DevStrap orchestrates Git safely.

## Clone strategies

### Standard clone

```bash
git clone git@github.com:org/repo.git ~/Code/work/org/repo
```

Use for small repos.

### Partial clone

```bash
git clone --filter=blob:none git@github.com:org/repo.git ~/Code/work/org/repo
```

Use for larger repos. Missing blobs are fetched on demand by Git when needed.

### No checkout clone

```bash
git clone --filter=blob:none --no-checkout git@github.com:org/repo.git ~/Code/work/org/repo
```

Useful when combined with sparse checkout.

### Sparse checkout

```bash
git sparse-checkout init --cone
git sparse-checkout set src tests pyproject.toml
```

Use for monorepos or large repos where the user only needs a subset.

## Materialization policy

Each repo can specify:

```yaml
materialization:
  mode: lazy
  clone_filter: blob:none
  sparse: false
  lfs: true
  bootstrap_on_open: ask
```

Modes:

```text
eager      clone during sync
lazy       create skeleton; clone on open
manual     only hydrate when explicitly requested
ephemeral  hydrate for agent/cloud task, then cleanup
```

Current hydrate implementation uses lazy skeleton directories and clones into a hidden sibling temp directory named like `.repo.devstrap-tmp-*` on the same filesystem as the final target. It validates the target before staging and revalidates it immediately before promotion, so a late local file blocks promotion without removing the dirty target. Clone failures leave the original skeleton in place and the caller cleans staged temp directories before returning.

## Safe update behavior

`devstrap sync` must be conservative.

Rules:

1. Always `git fetch` first.
2. Do not pull dirty worktrees.
3. Do not overwrite local branches.
4. Show ahead/behind/diverged state.
5. Use `--rebase` only when configured and safe.
6. Do not mutate agent worktrees unless running agent lifecycle command.

Status states:

```text
up_to_date
behind
ahead
diverged
dirty
conflicted
unknown_remote
```

## Fresh worktree creation

This is a killer feature.

Command:

```bash
devstrap worktree new work/org/repo --fresh-upstream --name fix-tests
```

Algorithm:

```text
0. Preflight: the project MUST have a non-empty remote_key. A remote-less repo (local_git) cannot create a fresh-upstream worktree; return a typed, actionable error ("fresh-upstream worktrees require a git remote; add origin or use --base local:<branch>") before touching git, instead of failing deep in plumbing (NOVCS-04).
1. Resolve namespace entry.
2. Ensure repo object cache/local clone exists.
3. Determine upstream default branch from refs/remotes/origin/HEAD.
4. Use `git_repos.default_branch` only as an explicit fallback and verify `origin/<fallback>` exists; do not silently fall back to `main` on git errors.
5. Persist the resolved default branch back to git_repos.default_branch.
6. git fetch origin <default_branch> --prune.
7. Resolve base SHA: git rev-parse origin/<default_branch>.
8. Create branch name: agent/fix-tests-YYYYMMDD-HHMMSS-<random suffix>.
9. Create worktree from base SHA.
10. Record worktree metadata.
11. Hydrate env/tooling.
12. Return path/open editor/launch agent.
```

Shell equivalent:

```bash
DEFAULT=$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's@^origin/@@')
DEFAULT=${DEFAULT:-$(devstrap config get default_branch)}
git fetch origin "$DEFAULT" --prune
BASE_SHA=$(git rev-parse "origin/$DEFAULT")
git worktree add ~/.devstrap/worktrees/repo/fix-tests -b agent/fix-tests "$BASE_SHA"
```

Important:

```text
Never use a local default branch as the base.
Never resolve a base from refs/devstrap/wip/* (the working-state plane).
```

The base resolver reads **only** `origin/<default_branch>` (or an explicitly configured upstream). The working-state plane's WIP refs (`refs/devstrap/wip/<device>/<path_key>`, see `07_NAMESPACE_AND_SYNC_MODEL.md`) are human-convenience recovery and must never become a worktree/agent base; add a test asserting this exclusion (an agent worktree created after a WIP push still bases from `origin/<default_branch>` and does not see the WIP content).

Current implementation fetches `origin <default_branch>` before resolving `origin/<default_branch>` and records `base_ref`, `base_sha`, branch, path, creator, and dirty state in SQLite. It rejects unsupported/option-like remotes, disables interactive git prompts, applies a sanitized git environment with protocol policy, redacts URL credentials in git errors, classifies network/auth/branch/remote Git failures into typed sentinels, and retries transient network clone/fetch failures only. Worktree branches include UTC date/time plus a long random suffix, and branch-name collisions from `git worktree add -b` trigger bounded suffix regeneration before surfacing an error. `devstrap worktree status <id>` re-fetches the recorded base ref and reports `fresh` or `stale (behind N)`. Integration coverage proves the worktree base equals the advanced remote SHA while the hydrated local default branch is stale, then advances the remote again and proves stale-base detection reports the drift.

## Worktree layout

```text
~/.devstrap/worktrees/
  repo-id/
    agent-fix-tests-20260623-120405-a13f92c0b31d/
    human-refactor-auth-20260623-130000-c11a8134fd44/
```

Metadata:

```yaml
id: wt_01jz...
repo_id: repo_01jz...
path: ~/.devstrap/worktrees/repo/agent-fix-tests-20260623-120405-a13f92c0b31d
branch: agent/fix-tests-20260623-120405-a13f92c0b31d
base_ref: origin/<default_branch>
base_sha: abc123
created_by: agent
agent_run_id: arun_01jz...
status: active
```

## Rebase freshness check

Before PR or finalization:

```text
1. git fetch origin <default_branch>
2. compare stored base_sha to current origin/<default_branch>
3. if changed:
     - warn
     - offer rebase
     - rerun tests
```

Current implementation provides this as `devstrap worktree finalize <id>`. It re-fetches the recorded `base_ref`, compares it with the stored `base_sha`, and exits with a conflict if the base moved. `--allow-stale-base` permits an explicit override and prints a warning. Future `agent pr`/GitHub integration must call the same gate before pushing or creating a PR.

## Branch naming

Recommended:

```text
agent/<short-task>-<date>-<time>-<random-suffix>
human/<short-task>-<date>-<time>-<random-suffix>
```

Examples:

```text
agent/fix-gss-tests-20260623-120405-a13f92c0b31d
agent/add-snowflake-env-check-20260623-120406-b92c4818df20
human/refactor-devstrap-sync-20260623-130000-c11a8134fd44
```

Rules:

- branch is always based on fetched upstream default branch;
- branch name includes task slug;
- branch name includes UTC date, UTC time, and a long random suffix;
- if `git worktree add -b` reports that the generated branch already exists, DevStrap regenerates the suffix and retries a bounded number of times;
- branch is recorded in SQLite;
- no shared branch between concurrent agents.

## Git LFS

If repo uses LFS:

```bash
git lfs install
git lfs pull
```

Policy:

```yaml
git:
  lfs: auto
  lfs_pull_on_open: ask
  lfs_pull_for_agent: false
```

For agents, avoid pulling all LFS objects unless needed.

Current implementation stores `git_repos.lfs_policy` from `devstrap add --lfs-policy` and reads it during `worktree new`. After creating an agent worktree, DevStrap scans checked-out `.gitattributes` files for `filter=lfs`. If LFS is used and the policy is `agent` or `always`, it runs `git lfs pull` in the worktree and fails clearly if the pull fails. If the policy is `auto` or `never`, it leaves the worktree lightweight and prints a warning that LFS pointer files may remain.

## Dirty worktree handling

Dirty primary repo:

```text
sync: fetch only, no pull
open: allow, show warning
worktree new: allowed because it uses remote base
rename/delete: conflict or quarantine
```

Dirty DevStrap worktree:

```text
remove: block dirty worktrees unless --force
cleanup: remove clean merged worktrees; prune missing stale paths
rebase: ask
agent rerun: ask
```

## Duplicate clone detection

During scan:

```text
remote URL normalized
compare remotes
compare repo root paths
compare default branch
```

Normalize examples:

```text
git@github.com:org/repo.git
https://github.com/org/repo.git
ssh://git@github.com/org/repo.git
```

All should map to canonical:

```text
github.com/org/repo
```

Current implementation also strips explicit SSH ports from `ssh://` and scp-like remotes for duplicate detection, so `ssh://git@github.com:2222/org/repo.git` and `git@github.com:2222:org/repo.git` normalize to the same host/path key.

## Bare cache option

Later optimization:

```text
~/.devstrap/cache/git/github.com/org/repo.git   # bare mirror/cache
~/Code/work/org/repo                            # worktree
~/.devstrap/worktrees/...                       # agent worktrees
```

Pros:

- shared object storage;
- faster worktree creation;
- less disk duplication.

Cons:

- more complex;
- harder for users to understand;
- avoid in MVP unless needed.

MVP should use normal clones first.

## Git operation locks

Use per-repo locks to avoid simultaneous Git commands.

Lock file:

```text
~/.devstrap/locks/repo-id.lock
```

Current implementation uses this lock for `hydrate` and `worktree new`. `worktree new` holds the same project lock through hydration, fetch, default-branch update, and worktree creation so the repo cannot be cloned or mutated concurrently.

Lock timeout behavior:

- lock files are created atomically with `O_CREATE|O_EXCL`;
- lock files include PID, hostname, and acquisition time;
- an active same-host owner blocks the operation;
- a dead same-host owner or an over-age lock is reclaimed;
- stale removal double-reads the file before deleting so a refreshed lock is not removed accidentally.

## Git provider integration (forge-agnostic)

Clone/fetch/push are forge-neutral and work against any `origin` (GitHub, GitLab, Bitbucket, Gitea/Forgejo, self-hosted, Azure DevOps, SourceHut). Only **PR/MR creation** is forge-specific.

MVP:

- shell out to `git` for all materialization;
- detect the forge from the `origin` host (`DetectForge`); for PR/MR creation shell out to the matching CLI — `gh` (GitHub), `glab` (GitLab), `tea` (Gitea/Forgejo);
- on an unknown/unsupported forge, **fail gracefully**: the branch is already pushed, so print the branch + a constructed compare/MR URL and exit cleanly — never run `gh` unconditionally (`FORGE-01`).

Status: today `agent pr` is hardcoded to `gh pr create` and fails post-push on any non-GitHub remote, and its child-env allowlist passes only GitHub tokens (`FORGE-01/02`). Introduce a small `Forge` interface (`CreatePR(ctx, dir, base, head, title, body)`) with `gh`/`glab`/`tea` implementations and a forge-aware token allowlist (`GITLAB_TOKEN`/`GLAB_*`, `GITEA_TOKEN`/`TEA_*`, `BITBUCKET_*`); add a `--forge` / `git_repos.forge_kind` override for self-hosted instances and SSH host-aliases. Remote-key normalization is generic but should add Azure DevOps SSH-vs-HTTPS folding (`FORGE-03`); `doctor` should mark `gh` optional and probe the relevant forge CLI per adopted remote (`FORGE-04`).

Later:

- native GitHub / GitLab / Bitbucket REST clients behind the same `Forge` interface;
- enterprise auth.

## Audit implementation notes (2026-06-28)

- **FORGE-01**: New `internal/cli/forge.go` with `DetectForge(remoteURL)`, `createForgePR` routing to `gh`/`glab`/`tea` based on detected forge; unknown forges get graceful degradation (branch pushed + compare URL).
- **FORGE-02**: PR env allowlist is now forge-aware (GH_*/GITLAB_TOKEN/GLAB_*/GITEA_TOKEN/TEA_*/BITBUCKET_*/AZURE_DEVOPS_EXT_PAT).
- **FORGE-03**: `normalizeHostPath` unifies Azure DevOps SSH (`ssh.dev.azure.com/v3/`) and HTTPS (`dev.azure.com/_git/`) forms to `dev.azure.com/org/proj/repo`.
- **GIT-01**: `repoLockIsStale` treats same-host liveness as authoritative over age; a live PID is never declared stale regardless of `acquired_at`.
- **NOVCS-01**: Scanner classifies no-remote/unvalidated-remote repos as `local_git` instead of `git_repo`.
- **NOVCS-04**: `createFreshWorktree` preflights `project.RemoteKey == ""` with an actionable error.
- **M2 (review fix)**: Agent run cleans up the just-created worktree when `enforceAgentFilePolicy` denies the command, preventing orphan git worktrees and DB rows.
