---
last_reviewed: 2026-07-03
tracks_code: [internal/git/**, internal/cli/add.go, internal/cli/clone.go, internal/cli/forge.go, internal/cli/hydrate.go, internal/cli/materialize.go, internal/cli/open.go, internal/cli/repo_lock.go, internal/cli/worktree.go]
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

### Submodules and maintenance (GIT-06)

The materialize/hydrate clone path initializes submodules so the working tree is structurally complete: `git clone --filter=blob:none --also-filter-submodules --recurse-submodules` (submodules stay blobless too) unless `materialization.submodules` is set to `never`. The policy is `auto` by default (recurse if present, a no-op when the repo has no submodules). An opt-in `materialization.maintenance` config runs a one-time `git maintenance run --auto` (commit-graph + prefetch) after clone so `git blame`/`log -p` on a blobless clone do not trigger per-object lazy-fetch storms on first use. `doctor` surfaces the offline caveat: historical blobs on a blobless clone need the promisor remote online for the first lazy fetch.

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

Each repo can specify (illustrative, planned config surface):

```yaml
materialization:
  mode: lazy
  clone_filter: blob:none
  sparse: false
  lfs: true
  bootstrap_on_open: ask
```

Shipped config keys today: `materialization.submodules` (auto|never), `materialization.maintenance` (bool), and `materialization.clone_timeout` (duration, default 30m — the per-attempt deadline for clone/fetch/LFS transfers, `P6-GIT-01`); the `mode`/`clone_filter`/`sparse`/`bootstrap_on_open` knobs above are not yet implemented.

Modes:

```text
eager      clone during sync
lazy       create skeleton; clone on open
manual     only hydrate when explicitly requested
ephemeral  hydrate for agent/cloud task, then cleanup
```

The hydrate implementation stages clones in a hidden sibling temp directory named like `.repo.devstrap-tmp-*` on the same filesystem as the final target, with atomic promotion after a successful clone. It validates the target before staging and revalidates it immediately before promotion, so a late local file blocks promotion without removing the dirty target; clone failures leave the original skeleton in place and the caller cleans staged temp directories before returning. `devstrap sync` drives this same code path eagerly for every namespace entry (`internal/cli/materialize.go`), while explicit `devstrap hydrate` remains for lazy/manual use.

### Eager clone-everything on sync (EAGER-*, shipped)

Eager whole-tree materialization is shipped (`EAGER-01/02`, `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`): the "Dropbox experience for code" is the materialization default, so after `devstrap sync` the whole `~/Code` tree is present, not skeletons. This is deliberately **eager clone-everything**, not a FUSE/placeholder/lazy-VFS scheme — StrapFS stays explicitly deferred (Phase 4, see `00_START_HERE.md`).

Decisions:

- Repo content is materialized by **git's own transport**, not the DevStrap hub. Each project is a blobless/partial clone (`git clone --filter=blob:none`) from its **existing remote**; repo content never traverses the hub. Missing blobs are still fetched on demand by git when an editor or build touches them.
- Eager sync reuses the shipped partial-clone machinery: `--filter=blob:none`, sibling temp-dir staging, and atomic promotion after a successful clone. It is the same code path as lazy hydrate, driven for every namespace entry instead of on first open.
- The materialization layer stays forge-agnostic; only PR/MR creation is forge-specific (see "Git provider integration").
- Content type dictates transport (cross-spec invariant, see `07_NAMESPACE_AND_SYNC_MODEL.md`): repo content rides git; env vars and non-git/draft folders ride age-encrypted, content-addressed `age_blob:<sha256>` blobs through the hub; the project map rides the signed, HLC-ordered event log. DevStrap **never** blanket file-syncs, and **never** file-syncs `.git` (file-syncing a `.git` directory corrupts the repo).
- `node_modules`, build artifacts, and other derived trees are **never synced**; they are rebuilt on hydrate (see "Post-hydrate dependency rebuild").

This makes the materialization `mode` effectively `eager` for synced projects. The `lazy`/`manual`/`ephemeral` modes above remain available for opt-out and for agent/cloud-task workflows.

## Post-hydrate dependency rebuild (DRAFT-*, partially shipped)

Derived dependency trees (`node_modules`, virtualenvs, `target/`, `dist/`, build caches) are **never synced** — they are large, OS/arch-specific, and reproducible from manifests. Instead, hydrate runs a **post-hydrate dependency rebuild hook** that regenerates them locally per-OS, keeping cross-platform devices (e.g. a macOS laptop and a Linux workstation or headless runner) consistent without shipping platform-specific binaries.

Detection and rebuild are manifest-driven:

```text
package-lock.json / npm-shrinkwrap.json  -> npm install / npm ci
pnpm-lock.yaml                           -> pnpm install --frozen-lockfile
yarn.lock                                -> yarn install --immutable
uv.lock / pyproject.toml                 -> uv sync
poetry.lock                              -> poetry install
requirements.txt                         -> python -m venv + pip install -r
```

Rules:

- the rebuild is **opt-in/gated** — today only globally, via the `DEVSTRAP_REBUILD_DEPS` env var (`internal/cli/materialize.go`); the per-project `materialization.rebuild_on_hydrate: ask|always|never` policy and the "ask before running" default are **target design, not yet implemented**;
- the hook runs **after** atomic promotion of the cloned/hydrated tree but **before** any env hydrate, in the project root, with the shared sanitized child-process environment; this ordering keeps untrusted lifecycle/postinstall scripts from running after the project's live `.env` has already been decrypted into the same directory at `$HOME/.env`;
- the rebuild-before-hydrate ordering is defense in depth, not a sandbox boundary: without OS enforcement, a script may still resolve the real user home via platform APIs such as `getpwuid`/`dscl`, or read another known project's `.env` by absolute path;
- the package-manager binary is resolved from the chosen tool adapter; a missing tool produces a typed, actionable warning rather than a hard failure, and the tree is left dependency-less;
- rebuild stdout/stderr is captured to `~/.devstrap/logs/rebuilds/<sanitized-project-path>.log` with mode `0600`; rebuilds are best-effort and never block the rest of `devstrap sync`;
- the rebuild map is OS-aware so the same project resolves to the correct toolchain on macOS and Linux, keeping Mac-specific behavior behind adapters (see `00_START_HERE.md`).

`node_modules` and equivalents stay gitignored and `.devstrapignore`-excluded so they are never adopted, never event-logged, and never bundled into `age_blob:<sha256>` content (cross-reference `11_IGNORE_AND_LOCAL_GARBAGE.md`).

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

Current implementation fetches `origin <default_branch>` before resolving `origin/<default_branch>` and records `base_ref`, `base_sha`, branch, path, creator, and dirty state in SQLite. It rejects unsupported/option-like remotes, disables interactive git prompts, applies a sanitized git environment with protocol policy, redacts URL credentials in git errors, classifies network/auth/branch/remote Git failures into typed sentinels, and retries transient network clone/fetch failures only. Worktree branches include UTC date/time plus a long random suffix, and branch-name collisions from `git worktree add -b` trigger bounded suffix regeneration before surfacing an error. After a successful `git worktree add`, failures in LFS policy handling, current-device lookup, or SQLite worktree insertion remove the just-created checkout and delete its `agent/...` branch so DB-invisible worktrees do not leak; the cleanup (`removeOrphanWorktree`, shared with the agent-run policy-denial path) runs under a detached, bounded context (`context.WithoutCancel` + 2m cap) so a Ctrl-C/deadline that caused the failure cannot also no-op the cleanup, and surfaces removal failures as warnings rather than swallowing them. `devstrap worktree status <id>` re-fetches the recorded base ref and reports `fresh` or `stale (behind N)`. Integration coverage proves the worktree base equals the advanced remote SHA while the hydrated local default branch is stale, then advances the remote again and proves stale-base detection reports the drift.

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

Current implementation provides this as `devstrap worktree finalize <id>`. It re-fetches the recorded `base_ref`, compares it with the stored `base_sha`, and exits with a conflict if the base moved. `--allow-stale-base` permits an explicit override and prints a warning. `devstrap agent pr` (shipped) calls this same gate before pushing and creating a PR/MR; see `10_AGENT_WORKSPACES_AND_POLICIES.md`.

## Branch naming

Recommended:

```text
agent/<short-task>-<date>-<time>-<random-suffix>
human/<short-task>-<date>-<time>-<random-suffix>
```

Examples:

```text
agent/fix-flaky-tests-20260623-120405-a13f92c0b31d
agent/add-ci-env-check-20260623-120406-b92c4818df20
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

Current implementation stores `git_repos.lfs_policy` from `devstrap add --lfs-policy` and reads it during `worktree new`. After creating an agent worktree, DevStrap scans checked-out `.gitattributes` files for `filter=lfs`. If LFS is used and the policy is `agent` or `always`, it runs `git lfs pull` in the worktree and fails clearly with the worktree path if the pull fails, then removes the orphan checkout and branch. If the policy is `auto` or `never`, it leaves the worktree lightweight and prints a warning that LFS pointer files may remain.

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

Status: `agent pr` is forge-aware as of the 2026-06-28 implementation pass: it detects GitHub/GitLab/Gitea/Forgejo/Bitbucket/Azure remotes, routes through `gh`/`glab`/`tea` when supported, allows forge-specific token env names, and gracefully prints a compare/MR URL for unsupported forges instead of running `gh` unconditionally (`FORGE-01/02`). Azure DevOps SSH-vs-HTTPS remote-key folding is also implemented (`FORGE-03`). `FORGE-04` is now implemented (GIT-05): a `--forge` flag, a per-project `git_repos.forge_kind` column, and a `[forge] host = kind` config map resolve self-hosted GitLab/Gitea/Forgejo instances with precedence flag > project column > host map > `DetectForge` heuristic; SSH host aliases (`~/.ssh/config` `Host`->`HostName`) are resolved before detection so `git@work-gitlab:org/repo` maps to the real host; `doctor` probes the matching forge CLI per adopted remote and warns on a missing `glab`/`tea` or unknown forge. Remaining work: native Bitbucket/Azure clients where useful, and broader hermetic fake-CLI tests (`FORGE-05`; a `--forge gitlab` override test exists).

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

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-GIT-01 — Universal 2-minute git timeout makes large-repo materialization impossible and triple-downloads — **shipped (2026-07-02)**

**Was.** `NewRunner()` applied `Timeout: 2*time.Minute` to every command including clone; a `DeadlineExceeded` was classified retryable `ErrNetwork` and `CloneWithOptions` retried up to 3× while wiping the staging dir each time, so any blobless clone taking > 2:00 could never materialize and burned ~6 min / 3× bandwidth. `LFSPull` hit the same cap once.

**Shipped fix.** The timeout is split by command class: `Runner.LongTimeout` (default **30m**, config `materialization.clone_timeout`, resolved by the `gitRunner(opts)` helper every CLI call site now uses) is applied **per attempt** to the network-transfer class — `CloneWithOptions`, `Fetch`/`runWithNetworkRetry`, `PushBranch` (the `agent pr` branch push), `LFSPull` — via `longTransferContext`, which also tags the context so (a) the "raise materialization.clone_timeout" hint appears only on transfer-class timeouts, and (b) an explicit `clone_timeout: 0` makes the class **unbounded** rather than silently falling back to the 2m cap. A caller-supplied deadline always wins; everything else keeps the 2m `Timeout`. Any `DeadlineExceeded` (the runner's own or a caller's) is the distinct terminal `ErrTimeout` (never `ErrNetwork`), so the retry loops stop the wipe-and-retry after one attempt; caller cancellation still classifies normally. `ErrTimeout` maps to the network exit code. Local-only helpers (e.g. `agentDiffSummary`) intentionally keep the bare runner. Pinned by `TestRunTimesOutAndReportsTimeoutError` (kind + no hint on a short-class timeout), `TestCloneTimeoutIsTerminalAndDoesNotRetryOrWipe` (one attempt, destination not wiped), `TestCloneUsesLongTimeoutInsteadOfShortTimeout`, `TestFetchTimeoutIsTerminalAndDoesNotRetry`, `TestLFSPullTimeoutIsTerminalAndDoesNotRetry`, `TestPushBranchTimeoutIsTerminalWithHint`, `TestZeroLongTimeoutMeansUnboundedTransfer`, and the `gitRunner` config round-trip tests.

**Accepted trade-off (review sign-off).** A hard-hung (not fast-failing) transfer is now detected at `LongTimeout` (30m) instead of the old 2m×3 (~6m) — one stuck clone can occupy a materialize worker slot (concurrency cap 4) for up to 30 minutes during a bulk sync. This is the deliberate cost of letting slow-but-progressing large-repo transfers finish, and it is operator-tunable (`materialization.clone_timeout`). Follow-up idea for hang-vs-slow discrimination without shrinking the ceiling: pass `-c http.lowSpeedLimit=1000 -c http.lowSpeedTime=60` on transfer commands so a genuinely stalled HTTP transfer dies in ~60s while a progressing one continues.

### P6-GIT-03 — Dependency rebuild runs after env hydrate, discards output, and is gated by one global env var — **shipped (2026-07-03)**

**Was.** The rebuild path gated all rebuilds on the single global `DEVSTRAP_REBUILD_DEPS` env var, ran `npm ci`/`uv sync`/etc. **after** the project's `.env` had been decrypted into the working tree with `$HOME` pointed at it, and discarded rebuild stdout/stderr with no log — so lifecycle/postinstall scripts could read freshly-decrypted secrets, and failures left no trace. This contradicted the "secrets-free, `0600`-logged" rules in "Post-hydrate dependency rebuild" above.

**Shipped fix.** `materializeGitRepo` now keeps the same global opt-in gate but runs `rebuildDependencies` before `hydrateProjectEnv`, so any untrusted lifecycle scripts execute before DevStrap writes the project's live `.env` into that directory. `runRebuildCommand` captures stdout/stderr to `~/.devstrap/logs/rebuilds/<sanitized-project-path>.log` with mode `0600`, overwriting the prior per-project log on re-run, and rebuild failures name the log path. Pinned by `TestMaterializeRebuildsBeforeHydrate` and `TestMaterializeRebuildLogIsWritten0600`.

**Remaining design gap.** The per-project `materialization.rebuild_on_hydrate: ask|always|never` policy is still target design; the shipped gate remains the single global `DEVSTRAP_REBUILD_DEPS` env var.

### P6-GIT-04 — Eager materialize/hydrate ignore stored `lfs_policy`; `always` repos land as silent pointer files

**Problem.** `materializeGitRepo` and `hydrateProjectUnlocked` never read `project.LFSPolicy` or call `UsesLFS`/`LFSPull` (`internal/cli/materialize.go:182-211`, `internal/cli/hydrate.go:93-190`); only the worktree path applies policy (`worktree.go:217-240`). Because `gitEnv` forces `GIT_CONFIG_GLOBAL=/dev/null` (`internal/git/git.go:704-712`), the user's global LFS smudge filter is invisible, so an `lfs-policy=always` repo materializes as pointer files that match the index and are recorded available/clean with no warning.

**Actionable steps.**
1. On the materialize/hydrate path, after hydration `install --local` + `LFSPull` for `always` (fail the project on error), warn otherwise; give `LFSPull` the P6-GIT-01 large-operation timeout.
2. Record available/clean only after the LFS decision.
3. Testscript: a fake-LFS repo with `always` pulls; with `auto` warns.

```go
if used, _ := dsgit.UsesLFS(ctx, localPath); used {
    switch policy {
    case "always":
        r.Run(ctx, localPath, "lfs", "install", "--local")
        if err := r.LFSPull(ctx, localPath); err != nil { /* fail project */ }
    default:
        log.Warn("LFS pointer files remain", "path", localPath)
    }
}
```

### P6-GIT-05 — post-`worktree add` failure cleanup (shipped 2026-07-03, `fix/p6-git-05`)

**Was.** `addWorktreeWithFreshBranch` created the branch and worktree, but later `applyWorktreeLFSPolicy`, `store.CurrentDevice`, and `store.InsertWorktree` failures returned without removing them, leaking a full checkout under `~/.devstrap/worktrees/<project>/` plus an `agent/...` branch untracked by SQLite — invisible to `worktree list`/`cleanup`.

**Shipped fix.** All three post-`worktree add` failure paths (and the `agent run` file-policy-denial path) now run `removeOrphanWorktree` (`internal/cli/worktree.go`), which removes the just-created checkout and deletes its `agent/...` branch under a detached, bounded context (`context.WithoutCancel` + 2m cap) so the Ctrl-C/deadline that caused the failure cannot also no-op the cleanup; removal failures surface as warnings with a manual-remedy hint, and the LFS error names the worktree path. Pinned by `TestCreateFreshWorktreeCleansUpAfterLFSPullFailure` / `...AfterInsertWorktreeFailure`. The `doctor` orphan-worktree check (on-disk worktrees with no `worktrees` row) was deliberately left out of scope and remains a candidate follow-up.
