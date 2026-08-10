---
last_reviewed: 2026-08-09
tracks_code: [internal/git/**, internal/cli/add.go, internal/cli/clone.go, internal/cli/forge.go, internal/cli/hydrate.go, internal/cli/materialize.go, internal/cli/open.go, internal/cli/repo_lock.go, internal/cli/worktree.go, internal/cli/status.go, internal/cli/doctor.go, internal/cli/project.go]
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

### Sparse checkout (shipped — cone-mode profiles, `W12-02`)

```bash
git sparse-checkout init --cone
git sparse-checkout set src tests pyproject.toml
```

Use for monorepos or large repos where the user only needs a subset. `devstrap` supports this as a per-project, opt-in **cone-mode-only** profile — cone mode is the only supported mode: git's own documentation deprecates non-cone/pattern mode (O(N×M) pattern-matching cost, and it is incompatible with `--sparse-index`), so `internal/git.ValidSparsePath` rejects anything that is not a plain, repo-relative directory path (glob metacharacters, absolute paths, `..` segments, and gitignore-style `!`/`#` prefixes are all refused before a path ever reaches a git subprocess).

**Sparse-checkout is not `.devstrapignore`, and the two solve different problems at different times** (see `11_IGNORE_AND_LOCAL_GARBAGE.md` for the ignore compiler itself). `.devstrapignore` controls sync/materialization **inclusion universally**, **before** anything is cloned — it decides which files are ever adopted, event-logged, or bundled into an `age_blob:<sha256>` draft snapshot in the first place, and it applies the same way on every device. Sparse-checkout is git's own **working-tree cone**, applied **after** a repo is already materialized — the repo's full history and every commit remain fully present and clonable; only which paths are checked out into the working tree on *this device* is narrowed. A project can (and typically would) use both together: `.devstrapignore` keeps `node_modules`/build output out of sync entirely, while a per-device sparse profile additionally narrows which of the *remaining*, synced directories are checked out locally. Configuring one has no effect on the other, and neither is a substitute for the other.

**Shipped:** `internal/git.Runner.SparseCheckoutInit`/`SparseCheckoutSet`/`SparseCheckoutList`/`SparseCheckoutDisable` are the primitives, and `Runner.ApplyConvergedSparseCheckout` composes them. It validates every requested path **before** any git mutation runs — `SparseCheckoutInit` narrows a never-sparse repo to cone mode's top-level-only files as a *side effect* of enabling it, so validating only deep inside `SparseCheckoutSet` (the first-cut design; closed by Codex review) left a window where a single invalid path narrowed the tree before the caller ever learned the whole request would fail. It then reads the currently active cone via `SparseCheckoutList` and no-ops when it already matches the desired set (an idle `devstrap sync` on a project with a configured profile therefore stays a zero-subprocess-churn no-op). On a genuine `SparseCheckoutSet` failure (a validation-passing path git itself rejects) after a successful `SparseCheckoutInit`, it restores the **prior active cone** — the snapshot `SparseCheckoutList` already read — rather than unconditionally disabling sparse-checkout: if the repo already had a working profile before the call, falling back to a full checkout would be a worse outcome than the state the caller started with; `SparseCheckoutDisable` is used only when there was no prior profile to restore (or the restore attempt itself fails). Convergence is also **bidirectional**: `applyProjectSparseProfile` (`internal/cli/hydrate.go`) treats an *empty* configured set as more than a no-op — it also disables an on-disk cone left over from a `project sparse clear` whose own immediate `SparseCheckoutDisable` call failed, so a project that failed to clear self-heals on the next sync/hydrate rather than staying narrowed forever. The profile itself (`project_sparse_paths`, migration `00033`, see `12_DATA_MODEL_SQLITE.md`) is deliberately **local-only and never synced** through the signed event log, mirroring the documented intent of the pre-existing (and still unused) `git_repos.sparse_config` column — each device chooses its own profile. A known consequence of local-only config: a **fresh device**'s first `devstrap sync` fully materializes every project (including its blobs, since a normal checkout is what triggers blob fetch on a blobless/partial clone) *before* a human has any chance to configure a sparse profile on that device — the expensive full materialization the feature exists to help avoid has already happened by the time a profile could narrow it. This is an accepted limitation of the local-only design (not yet worth a synced "suggested default" profile). Note what `W14-01` did and did not change here: once a profile IS configured, the *first clone on any subsequent device* now narrows before its checkout rather than after, so the transfer saving is real from that point on — but a profile that does not exist yet on a brand-new device still cannot narrow that device's very first sync, and only a synced default would fix that.

**CLI lock ordering (`project sparse set`/`clear`, Codex review).** Every mutating git operation in this codebase runs under the project's repo operation lock, held across both its own state write and its git work (`hydrate`, `worktree new`). The first cut of `project sparse set` violated this — it wrote the DB profile, then acquired the lock only inside a later helper for the git apply — which let a concurrent `set`/`clear`/sync interleave DB writes and git applies out of order and leave the DB's desired state and the on-disk cone permanently mismatched. `set` now acquires the lock once, before the DB write, and holds it across both the write and the immediate apply. `clear` disables the on-disk cone under the lock **first**, releases it, and then always clears the DB regardless of whether the disable succeeded — the DB is the desired-state source of truth, and a failed disable self-heals via the bidirectional convergence above rather than needing the CLI command itself to retry.

Application is wired into `hydrateProjectUnlocked` — the single function `devstrap sync`, `devstrap hydrate`, and `devstrap worktree new` all call to get a cloned/checked-out repo onto disk — on BOTH its "already materialized" and "freshly cloned" success paths, so re-running sync/hydrate after a profile is configured or changed converges the tree. On an **already-materialized** repo it is applied immediately after the checkout completes. On a project's **first clone**, the narrowing is integrated into the clone sequence instead (`W14-01`): a project with a configured profile clones with `--no-checkout`, has its cone configured **while still in the staging directory**, and is only then checked out, so the FIRST checkout is already narrow and git never lazily fetches blobs for paths that would be pruned moments later. Staging is load-bearing — `hydrate` clones into a sibling temp dir and promotes by rename, so a half-narrowed repo is never promoted into the managed namespace.

Three properties of that sequence are deliberate and each is pinned by a test:

- **A project with no configured profile is byte-identical to before** — same clone argv, no `--no-checkout`, no extra subprocess. The overwhelming majority of projects have no profile and must not pay for this.
- **A sparse failure never fails materialization — but a failed *rollback* does.** If `SparseCheckoutInit`/`SparseCheckoutSet` fail, DevStrap disables sparse-checkout and checks out the FULL tree, degrading to exactly the pre-`W14-01` outcome rather than abandoning the clone. If that `SparseCheckoutDisable` **also** fails, materialization is refused outright.

  That asymmetry is deliberate and was added on review. `SparseCheckoutInit` narrows a never-sparse repo to cone mode's top-level-only files as a *side effect of merely enabling it*, so a failed `set` plus a failed `disable` leaves the cone active, and the following checkout populates almost nothing — for a repo whose root has no top-level tracked files, literally nothing. **Nothing downstream can detect that state:** measured, `git checkout` exits 0, `headResolvable` passes, and `git status --porcelain=v2` — precisely what `DirtyState` parses — emits only branch headers, so the tree would be recorded `available`/`clean`. Git reports *"0% of tracked files present"* only in its human output, which DevStrap never reads. An empty tree promoted as healthy breaks the product's central promise that the tree is really present on disk, so this trades an invisible-incomplete-tree class for a visible failed-materialization class: staging is discarded, the project records `failed`, and the next sync retries.
- **Submodules are re-initialized explicitly.** `--no-checkout` **silently** skips submodule initialization — `git clone --no-checkout --recurse-submodules` exits 0 with empty submodule directories, and a later `git checkout` does not repair them — so the sequence runs `git submodule update --init --recursive` after checking out. Without it, any project with both a sparse profile and submodules would materialize structurally incomplete and be recorded `available`/`clean`, breaking the GIT-06 guarantee invisibly.

Both the checkout and the submodule step run in the **long transfer timeout class** (`P6-GIT-01`), not the default 2m cap. On a blobless clone the checkout *is* the network transfer — git lazily fetches a blob for every file it writes — and that cost previously sat inside `git clone`'s own `LongTimeout` budget; splitting it into a separate command under the short cap would time out materialization of precisely the large monorepos this path exists to make cheaper. Failure to apply is **always best-effort**: a git error warns and records via `RecordProjectWarning` rather than failing materialization, since narrowing the working tree is a disk/index-cost optimization, never a correctness requirement — the full checkout underneath remains perfectly usable.

`devstrap worktree new --fresh-upstream` applies the SAME project's sparse profile to the fresh worktree it creates, immediately after `git worktree add` succeeds — this is the step most likely to be silently missed, since skipping it would defeat the whole feature for agent worktrees on a large monorepo. `devstrap worktree adopt` (registering a worktree an external harness such as Codex/Cursor/Devin already created) deliberately does **not** apply a configured profile to the adopted checkout — adopt never mutates a checkout it did not create — and instead surfaces a warning naming the manual `git sparse-checkout init --cone && git sparse-checkout set` remedy, so a configured-but-unapplied profile on an adopted worktree is an explained inconsistency, not a silent one.

CLI: `devstrap add --sparse dir1,dir2` persists the initial profile in the same SQLite transaction as the project's `project.added` event; `devstrap project sparse set <path> <dir1> [dir2...]` changes the profile on an already-adopted project and immediately re-applies it against an already-materialized checkout (under the project's repo operation lock, the same lock every other mutating git operation on the project holds) rather than waiting for the next sync; `devstrap project sparse list <path>` reads it back; `devstrap project sparse clear <path>` removes the profile and restores a full working tree.

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

### Persisted materialize failure record (P4-GIT-07, shipped)

Per-project materialize failures still **never abort the batch** (EAGER-04 isolation). What changed is visibility: writers persist the scrubbed failure text into the existing `device_project_state.last_error` column (no migration — the column was already in `00001_initial.sql`).

- **Writers.** Clone failure, promote failure, LFS-policy failure (`always`/`agent`), draft-extract failure, and the empty/broken-HEAD (`materialized-empty`) path all call `UpdateProjectLocalState` with a scrubbed `last_error` (prefixed cause). Success and interim-skeleton writes pass `""`, which clears any stale error. Env hydrate is still best-effort (does not fail the project): on failure it keeps the Warn log and also calls `RecordProjectWarning` so the non-fatal warning is visible without flipping `materialization_state`.
- **Resume.** Unchanged: `SkeletonProjects` still returns `skeleton` **and** `failed` projects, so the next `sync`/`materialize` retries automatically. There is no `--only-failed` flag and no new retry mechanism.
- **Surfacing.** `status` (human) prints a "Failed materializations:" section for every project with non-empty `LastError`; `status --json` includes `last_error` on each project via the existing `Summary` payload. `doctor` emits one `warning`-tier check per `materialization_state=failed` project (`Name: materialize: <path>`, `Detail: last_error` or a legacy fallback, remedy points at re-running `materialize`/`sync`); when none failed it reports a single OK check so the suite always shows the check ran.

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

**Default-branch resolution is split by phase (`P6-XP-05`).** The authoritative resolver `Runner.ResolveDefaultBranch` reads `refs/remotes/origin/HEAD` and, when it is missing/stale, repairs it with `git remote set-head origin --auto` — a **network** round-trip — before verifying the stored fallback. That authoritative path runs only at **materialization** (hydrate/worktree creation), which is already a network phase. At **scan** time it would be wrong: the scanner walks the whole tree and must stay filesystem-only, so `Runner.LocalDefaultBranch` resolves from local refs only (`symbolic-ref refs/remotes/origin/HEAD`, then a local `origin/<fallback>` `rev-parse`) and never runs `set-head --auto`/`ls-remote`/`fetch`. A scan whose default branch could not be confirmed locally records `main` with a non-authoritative warning and lets materialization resolve it authoritatively at use time.

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

**The eager materialize/hydrate path honors `lfs_policy` too (`P6-GIT-04`).** `materializeGitRepo` (the whole-tree `devstrap sync`/`materialize` clone) applies the policy after `hydrateProjectUnlocked` returns, mirroring the worktree path: `always`/`agent` runs `git lfs install --local` (required because the sanitized git env sets `GIT_CONFIG_GLOBAL=/dev/null`, hiding any global smudge filter) then `git lfs pull`, recording the project **failed** on error rather than silently landing pointer files as available/clean; `auto`/`never` warns. It is applied in the caller (`materializeGitRepo`), not inside `hydrateProjectUnlocked` (which the worktree path also calls), so it covers both the fresh clone and a `SkeletonProjects` retry of a repo previously recorded failed — an always-policy repo can never be flipped back to available/clean with pointers on a later sync. `LFSPull` carries the `P6-GIT-01` long-transfer timeout.

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

`devstrap worktree cleanup --merged` takes no positional arguments (`cobra.NoArgs`, P7-CLI-02) so a stray id cannot be silently discarded while sweeping the whole fleet. Before the sweep it reconciles stale `agent_runs` (dead recorder PIDs → `interrupted`); for each path-present worktree it then refuses to reap when any `agent_runs` row is still `status='running'` for that worktree id (P7-GIT-01 — including runs without a recorded PID). Path-missing rows stay a metadata-only prune outside the lock. For an existing path the project repo lock is held across the full dirty-check → base-refresh → merge-check → dirty re-check → `WorktreeRemove` → `branch -D` → `MarkWorktreeRemoved` sequence (P7-GIT-02); a lock conflict skips that worktree with a warning rather than failing the whole sweep. Dirty state is re-checked immediately before removal under that lock (P7-GIT-01 TOCTOU).

`devstrap worktree cleanup --merged` treats a worktree as merged when either the worktree branch is an ancestor of its recorded `base_ref` or the branch's patch content is already present on that base. Before each mergedness check it best-effort fetches the recorded base ref (for example `origin/main`) while the project repo lock is already held (same lock as fresh worktree creation), deduped per (project, base ref) per run; if that fetch fails, cleanup warns and continues against the locally-known base ref. That offline fallback is deliberate, not an oversight: cleanup must work without network (the same reason forge cross-checks are a non-goal), the ancestry check has always evaluated against the local ref (the fetch is a freshness improvement this feature ADDED, not a guarantee it weakened), and merged-ness is by definition evaluated against the base as this machine knows it — a branch reaped offline is one whose content is already part of the locally-known base history, and the printed tip SHA restores it if the remote has since diverged.

The content-equivalence path is intentionally offline and forge-agnostic: `git.Runner.IsSquashMerged` simulates the merge with `git merge-tree --write-tree <base> <branch>` (git >= 2.38) and reports merged only when the resulting tree is IDENTICAL to the current base tree — merging the branch would contribute nothing, which is exactly the effect of a squash-, rebase-, or cherry-pick-merge. Comparing against the CURRENT base tree (not patch-id history — the dual-review finding that killed the first draft) means a change that was merged and then reverted on base correctly reads as NOT merged. The conservative rule governs the content test itself: a conflicting simulated merge, any git error (including an older git without `--write-tree`), or unproven equivalence means "not merged", so the equivalence probe never affirms on doubt (staleness of the base is bounded separately, by the best-effort fetch above). One limitation is inherent to any content-equivalence test and accepted deliberately: a branch whose net change also landed via an unrelated, coincidentally identical commit is indistinguishable from a squash-merge and is reaped (pinned by `TestIsSquashMergedMatchesCoincidentallyIdenticalDiff`); as the recovery breadcrumb, every reap prints the deleted branch's tip SHA so `git branch <name> <sha>` restores it until git gc. Explicit forge cross-checks such as `gh pr list --state merged` are a non-goal for the materialization layer.

## Working-state capture (P7-GITSTATE-01, shipped)

`git.Runner.CaptureGitstate` (`internal/git/gitstate.go`) captures a read-only snapshot of one repo's working state for the working-state validation plane's Layer A (see `07_NAMESPACE_AND_SYNC_MODEL.md` § *Working-state plane*): current branch, HEAD sha, upstream branch/sha, and dirty/untracked/unmerged/ahead/behind/stash counts. It runs `git --no-optional-locks status --porcelain=v2 --branch`, which never writes `.git/index`, so capture is safe to run against a repo another process (an editor, an agent, a concurrent `devstrap` command) is actively using — the same safety property `DirtyState` already relies on for its own porcelain-v2 read. Porcelain v2 does not report the upstream SHA or the stash count, so those are two cheap follow-up calls (`git rev-parse --verify -q <upstream>`, `git stash list`); either failing (no upstream configured, no stash) leaves the corresponding field at its zero value instead of failing the whole capture. This routine only captures — the `repo.gitstate.observed` event constructor/apply handler live in `internal/sync`. The CLI surfacing is now SHIPPED: `status --all-devices` (`internal/cli/status.go`, `renderAllDevicesStatus`) renders each device's last-observed working-state per project, newest-first, with an explicit "never synced" row for a project no device has reported on — never a silent omission; `doctor` (`internal/cli/doctor.go`, `checkGitstateFreshness`) grades the same data per project (warning when no device has reported, warning past `gitstateStaleAfter` = 7 days, ok otherwise). Calling capture during `devstrap sync` is **SHIPPED** (`internal/cli/sync.go` invokes `CaptureGitstate` per materialized `git_repo` project once per cycle, mirroring into this device's own `device_gitstate` row immediately and skipping an unchanged capture so an idle sync stays a zero-push no-op). A "never synced" row therefore now means what it says — no device has reported — rather than "no producer is wired". (Corrected 2026-07-31, `P9-WIP-04`: this paragraph still described the producer as an unlanded follow-up long after it landed, which is the class of stale status claim `AGENTS.md`'s post-wave review exists to catch.)

## WIP-ref git plumbing (P7-WIP-01/P7-WIP-02, push/fetch shipped)

`git.Runner.StashCreate` (`internal/git/git.go`) wraps `git stash create`, which produces a commit object without touching the worktree or index — empty stdout means a clean tree, reported as `ok=false` rather than an error. `git.Runner.PushRef` wraps a raw refspec push (`git push <remote> <sha>:<ref>`), distinct from `PushBranch`'s tracking-branch push, and is the primitive that will back `devstrap wip push`'s push to `refs/devstrap/wip/<device_id>/<path_key>` (working-state validation plane Layer B, see `07_NAMESPACE_AND_SYNC_MODEL.md` § *Working-state plane*). `PushRef` validates its ref with a new `safeRefPath`, deliberately narrower than the generic `safeBranchName`: it requires the literal `refs/devstrap/wip/` prefix plus at least two more segments, reuses `safeBranchName`'s character-class checks for the ref as a whole, and adds an explicit per-segment leading-`-` check — `device_id`/`path_key` are peer-influenced (unlike a locally-typed branch name), so a dash-prefixed inner segment needs its own guard rather than relying on `safeBranchName`'s whole-string-only check. `safeBranchName` itself is untouched (it is still used to validate real branch names elsewhere). The `repo.wip.pushed` event constructor/apply handler and the mirror-only `device_wip` storage (migration `00030`, see `12_DATA_MODEL_SQLITE.md`) are also shipped. `git.Runner.FetchRef` (`P7-WIP-02`) is the pull-side twin: `git fetch <remote> +<ref>:<ref>` mirrors a peer's WIP ref into the identical local ref path under `runWithNetworkRetry`, validating the ref with the same `safeRefPath` — the one place a mirror-derived ref string is handed to a git subprocess, making this validation the fetch path's actual trust boundary. The `+` force prefix on BOTH `PushRef`'s refspec and `FetchRef`'s is load-bearing, not stylistic: the owning device force-pushes a fresh, never-fast-forward `stash create` commit on every `wip push`, so both the remote update and a repeat local fetch of the same ref are non-fast-forward by design. Callers: `devstrap wip push`/`devstrap wip fetch` (see `13_CLI_DAEMON_API.md`). The delete-side plumbing (`P7-WIP-05`): `git.Runner.DeleteRef` deletes a WIP ref via an empty-source refspec push (`git push <remote> :<ref>`) and, when given an expected sha, becomes a COMPARE-AND-DELETE — the explicit-value `--force-with-lease=<ref>:<sha>` form makes the server refuse unless the ref still points exactly there, so a `wip drop` driven by a stale `device_wip` mirror can never destroy a newer snapshot whose event has not synced yet (a lease rejection classifies as `ErrNonFastForward`, including against an already-absent ref); `git.Runner.LsRemoteRef` reads the remote's current advertisement for exactly one ref (or `ErrBranchNotFound`) to tell those two rejection causes apart. `PushRef` additionally requires a full-length hex object id for its source (`isHexObjectID`) — an empty sha would silently turn the refspec into git's delete syntax.

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

### `RemoteTrackingContains` — a pin must be reachable *from the exported remote* (`W13-02`, 2026-08-01; scoped by `P11-MANIFEST-01`, 2026-08-05)

`git.Runner.RemoteTrackingContains` reports whether a SHA is reachable from a
remote-tracking ref of one of the **named** remotes it is given (`git branch -r
--contains <sha> <name>/*` …). It exists for `export --pinned`, where the
distinction is the whole point: `git rev-parse HEAD` resolves an **unpushed**
commit perfectly well, and a manifest that pins it is worthless in the disaster
the pin is written for, because after total local loss that commit exists
nowhere and `vcs import` fails its checkout.

**The remote scoping is the second half of the same claim, and it is not
optional.** A manifest entry pairs its SHA with exactly ONE url — the registered
remote. `git branch -r` with no pattern lists remote-tracking branches for
*every* configured remote, which answers "this SHA is on some remote I have
fetched", not "this SHA is on the url this entry records". In a fork workflow —
`origin` an empty fork, `upstream` the canonical repo, HEAD on `upstream/main` —
the unscoped question answers yes and the manifest pins the SHA against the
fork's url, which cannot serve it. `vcs import` then clones the fork and fails
its checkout during the actual recovery: exactly the failure the reachability
gate exists to prevent (`P11-MANIFEST-01`). `internal/cli`'s
`remoteNamesForURL` therefore resolves which configured remotes serve
`project.RemoteURL` — comparing `CanonicalRemoteKey`s, so URL-shape differences
between the registered url and the checkout's config do not cause a spurious
miss — and passes only those names. All matching names are passed, not one:
two names for the same url address the same upstream refs but their local
caches were fetched at different times. No matching remote is an error that
takes the same omit-the-version path, and an empty name list is refused rather
than degraded back into the unscoped query. Each name is validated with
`safeRemoteName` before it becomes a `<name>/*` pattern, keeping the invocation
free of option injection and fnmatch metacharacters.

**Two ways the scoping could still be evaded, both closed (Codex review).**

- **A nested remote name.** A remote *named* `origin/vendor` writes its refs
  under `refs/remotes/origin/vendor/*`. git's `branch --list` wildmatch runs
  without `WM_PATHNAME`, so `*` spans slashes — which is what makes `origin/*`
  match the nested branch `origin/feat/x` as intended, and also makes it match
  `origin/vendor/main`. A commit present only on the nested remote would vouch
  for `origin`'s url. The namespaces are not separable by pattern, so
  `remoteNamesForURL` refuses when any configured remote name is nested inside a
  selected one, and the pin is omitted rather than guessed.

  **How reachable this is depends on the git that wrote the config**, which is
  worth stating precisely rather than implying it is an everyday shape. Newer
  git refuses `git remote add origin/vendor` while `origin` exists (*"remote
  name 'origin/vendor' is a subset of existing remote 'origin'"*); git 2.50.1
  creates it without complaint, in either order. Writing
  `remote.origin/vendor.url` with `git config` bypasses the porcelain guard on
  every version. So the state arrives from an older git, a repo created by one,
  a tool that writes config directly, or a hand edit — not from current
  porcelain. The guard stays because DevStrap reads whatever config it is
  handed and the failure mode is a false pin; it is cheap and fails safe. The
  regression test builds the state with `git config` for exactly this reason:
  constructing it with `git remote add` passes on the maintainer's machine and
  fails on CI, which is how this version dependency was found.
- **A multi-url remote.** `git config --get-regexp` prints every `remote.<n>.url`
  value; git fetches from the **first** and treats the rest as push-only, so the
  first is the one whose refs are in `refs/remotes`. `Runner.Remotes` therefore
  keeps the first occurrence. Last-wins would let a remote whose push mirror
  happens to be the registered url claim to serve refs it fetched from
  somewhere else — a false pin, not a missed one.

**Two known limits, accepted and stated rather than guarded.** A hand-written
`remote.<other>.fetch` refspec whose destination is `refs/remotes/origin/*`
aliases another remote into origin's namespace, and a repo-local
`url.<base>.insteadOf` rewrites a configured url before git fetches it. Both
would let refs from one endpoint answer for another. Detecting them means
parsing every remote's refspecs and resolving git's url expansion, and both
require deliberately hand-edited config that already breaks git's own
remote-tracking bookkeeping. A pin is best-effort local evidence (see the
staleness note below), so this is a documented ceiling on the guarantee, not an
oversight.

It reads refs already fetched into `refs/remotes` and makes **no network call**,
so the answer is only as good as the last fetch — and that cache is stale in
**both** directions. It answers "not reachable" for a commit that *is* on the
remote but has not been fetched (the safe direction: the caller degrades to
omitting the version). It can also answer "reachable" for an object the remote
**no longer serves**, after an upstream force-push or branch deletion with no
intervening fetch. A "yes" is therefore the best available local evidence, not
a guarantee about the remote's current state — the fork case above does not even
need staleness to break, which is why the scoping, not a freshness heuristic, is
the fix.


## Promotion git operations (`W13-03` / `NOVCS-03`, 2026-08-01)

`devstrap promote` graduates a remote-less project into a real `git_repo`, and
the git primitives it needs live in `internal/git/promote.go`: `RemoteIsEmpty`,
`InitRepo`, `StageAll`, `StagedFiles`, `CommitStaged`, `AddRemote`,
`RemoveRemote`, `CurrentBranch`, `HasCommits`. They run under the project's
repo operation lock like every other mutating path.

**The ordering is the safety property, not a preference: validate → push →
record.** A `git_repo` row whose remote has no commits is precisely the "broken
clonable repo" `00_START_HERE.md` promises never to create — the next device
would blobless-clone it and get nothing. So the type is recorded and the
namespace event emitted **only after the push succeeds**; a failed push leaves
the project at its original type, and `promoteInitRepo`'s rollback removes an
`init`/`remote add` the promotion itself created. **That rollback is armed
before `InitRepo`, not after** (`P11-PROMOTE-02`): `git init` creates and
populates `.git` incrementally, so a failure partway through returns non-zero
with a partial `.git` on disk, and leaving it there wedges every later attempt —
`IsRepo`/`HasCommits` all refuse once a `.git` exists — with no message naming
the fix. Arming it earlier is safe only because of the `Lstat` gate below,
which is what makes "the only `.git` this can remove is one this call created"
true. A rollback that itself fails is reported to stderr rather than swallowed —
the same reasoning as the `local_git` arm's `origin` removal: telling the user
the folder is back at its pre-command state while a partial `.git` still sits in
it is worse than the failure it follows.

**`IsRepo` resolves symlinks, so `promoteInitRepo` re-checks with `Lstat`.**
`IsRepo`'s existence checks are `os.Stat`-based, so a **dangling** `.git`
symlink reads as "not a repository" and reaches the init. That is not merely an
odd input: `git init` FOLLOWS the link and initializes the repository at its
target, so the managed namespace would gain a project whose `.git` lives at an
arbitrary path `pathkey.VerifyWithinRoot` never validated — and the rollback
would then delete a symlink the user created rather than anything this command
made. Any existing `.git` node (`Lstat` succeeding, including a dangling link)
is refused with a message naming the path; a stat error other than
`os.ErrNotExist` is refused too rather than assumed absent.

**`StagedFiles` reads `git ls-files -z --stage`, not `--cached`, because the
mode is load-bearing.** `StageAll`'s `git add -A` records a nested repository as
a gitlink (mode `160000`) instead of descending into it, and git's
"adding embedded git repository" warning goes to stderr where `Runner.Run` drops
it. Returning `(mode, path)` pairs is what lets the caller refuse rather than
push a commit referencing objects the remote will never receive
(`P11-PROMOTE-03`). Records are split on `\x00` first, then on the FIRST `\t` —
a path may contain tabs, the metadata prefix before the first one never does.

**`local_git` never goes through `InitRepo`.** It is a real repository whose
remote is absent *or failed validation* (`NOVCS-01`), so the promotion pushes its
existing history. Running `init` over it would destroy that history, which is the
single most likely way this path ships broken. Two refusals follow from the same
fact: a `local_git` that already has an `origin` is refused rather than rewritten
(the classification may be due to a *rejected* remote, not a missing one), and a
repository with no commits is refused rather than given an invented one.

`RemoteIsEmpty` gates `--git-remote` on the target being empty; a non-empty
remote means the user wants `scan --adopt` (not `add`, which refuses a populated
directory — `P11-PROMOTE-01`), and the error says so.


## Git operation locks

Use per-repo locks to avoid simultaneous Git commands.

Lock file:

```text
~/.devstrap/locks/repo-id.lock
```

Current implementation uses this lock for `hydrate` and `worktree new`. `worktree new` holds the same project lock through hydration, fetch, default-branch update, and worktree creation so the repo cannot be cloned or mutated concurrently.

Lock timeout behavior:

- lock files are created atomically with `O_CREATE|O_EXCL`;
- lock files include PID, opaque process start-time identity (when the platform exposes it), hostname, and acquisition time;
- an active same-host owner blocks the operation while its PID is alive and, when a start-time identity was recorded, that identity still matches; a live PID with a mismatched identity is a recycled PID and therefore stale, while a missing identity or a failed lookup conservatively keeps the lock (`P7-GIT-03`);
- a dead same-host owner or an over-age lock is reclaimed;
- stale removal double-reads the file before deleting so a refreshed lock is not removed accidentally.

## Git provider integration (forge-agnostic)

Clone/fetch/push are forge-neutral and work against any `origin` (GitHub, GitLab, Bitbucket, Gitea/Forgejo, self-hosted, Azure DevOps, SourceHut). Only **PR/MR creation** is forge-specific.

MVP:

- shell out to `git` for all materialization;
- detect the forge from the `origin` host (`DetectForge`); for PR/MR creation shell out to the matching CLI — `gh` (GitHub), `glab` (GitLab), `tea` (Gitea/Forgejo);
- on an unknown/unsupported forge, **fail gracefully**: the branch is already pushed, so print the branch + a constructed compare/MR URL and exit cleanly — never run `gh` unconditionally (`FORGE-01`).

Status: `agent pr` is forge-aware as of the 2026-06-28 implementation pass: it detects GitHub/GitLab/Gitea/Forgejo/Bitbucket/Azure remotes, routes through `gh`/`glab`/`tea` when supported, allows forge-specific token env names, and gracefully prints a compare/MR URL for unsupported forges instead of running `gh` unconditionally (`FORGE-01/02`). Azure DevOps SSH-vs-HTTPS remote-key folding is also implemented (`FORGE-03`). `FORGE-04` is now implemented (GIT-05): a `--forge` flag, a per-project `git_repos.forge_kind` column, and a `[forge] host = kind` config map resolve self-hosted GitLab/Gitea/Forgejo instances with precedence flag > project column > host map > `DetectForge` heuristic; SSH host aliases (`~/.ssh/config` `Host`->`HostName`) are resolved before detection so `git@work-gitlab:org/repo` maps to the real host; `doctor` probes the matching forge CLI per adopted remote and warns on a missing `glab`/`tea` or unknown forge. The `gh`/`glab`/`tea` invocation itself, self-hosted-override precedence, missing-CLI degradation, and failure wrapping are now exercised hermetically via PATH-shimmed forge-CLI stubs (`FORGE-05`), closing the last forge-hardening gap. Remaining work: native Bitbucket/Azure clients where useful.

Later:

- native GitHub / GitLab / Bitbucket REST clients behind the same `Forge` interface;
- enterprise auth.

## Audit implementation notes (2026-06-28)

- **FORGE-01**: New `internal/cli/forge.go` with `DetectForge(remoteURL)`, `createForgePR` routing to `gh`/`glab`/`tea` based on detected forge; unknown forges get graceful degradation (branch pushed + compare URL).
- **FORGE-02**: PR env allowlist is now forge-aware (GH_*/GITLAB_TOKEN/GLAB_*/GITEA_TOKEN/TEA_*/BITBUCKET_*/AZURE_DEVOPS_EXT_PAT).
- **FORGE-03**: `normalizeHostPath` unifies Azure DevOps SSH (`ssh.dev.azure.com/v3/`) and HTTPS (`dev.azure.com/_git/`) forms to `dev.azure.com/org/proj/repo`.
- **GIT-01 / P4-QUAL-04**: `repoLockIsStale` treats same-host liveness as authoritative over age; both repo locks and folder-hub locks use the shared build-tagged `platform.ProcessAlive` adapter. Only a PID positively confirmed absent is dead; permission-denied or otherwise indeterminate checks stay alive, so an inaccessible live holder's lock is never stolen. A live PID is never declared stale regardless of `acquired_at`.
- **NOVCS-01**: Scanner classifies no-remote/unvalidated-remote repos as `local_git` instead of `git_repo`.
- **NOVCS-04**: `createFreshWorktree` preflights `project.RemoteKey == ""` with an actionable error.
- **M2 (review fix)**: Agent run cleans up the just-created worktree when `enforceAgentFilePolicy` denies the command, preventing orphan git worktrees and DB rows.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-GIT-01 — Universal 2-minute git timeout makes large-repo materialization impossible and triple-downloads — **shipped (2026-07-02)**

**Was.** `NewRunner()` applied `Timeout: 2*time.Minute` to every command including clone; a `DeadlineExceeded` was classified retryable `ErrNetwork` and `CloneWithOptions` retried up to 3× while wiping the staging dir each time, so any blobless clone taking > 2:00 could never materialize and burned ~6 min / 3× bandwidth. `LFSPull` hit the same cap once.

**Shipped fix.** The timeout is split by command class: `Runner.LongTimeout` (default **30m**, config `materialization.clone_timeout`, resolved by the `gitRunner(opts)` helper every CLI call site now uses) is applied **per attempt** to the network-transfer class — `CloneWithOptions`, `Fetch`/`runWithNetworkRetry`, `PushBranch` (the `agent pr` branch push), `LFSPull` — via `longTransferContext`, which also tags the context so (a) the "raise materialization.clone_timeout" hint appears only on transfer-class timeouts, and (b) an explicit `clone_timeout: 0` makes the class **unbounded** rather than silently falling back to the 2m cap. A caller-supplied deadline always wins; everything else keeps the 2m `Timeout`. Any `DeadlineExceeded` (the runner's own or a caller's) is the distinct terminal `ErrTimeout` (never `ErrNetwork`), so the retry loops stop the wipe-and-retry after one attempt; caller cancellation still classifies normally. `ErrTimeout` maps to the network exit code. Local-only helpers (e.g. `agentDiffSummary`) intentionally keep the bare runner. Pinned by `TestRunTimesOutAndReportsTimeoutError` (kind + no hint on a short-class timeout), `TestCloneTimeoutIsTerminalAndDoesNotRetryOrWipe` (at most one attempt, destination not wiped — the fakes sleep 5s against sub-second deadlines and a pre-log kill counts as zero attempts, so the pins hold under full-suite `-race` load, 2026-07-05), `TestCloneUsesLongTimeoutInsteadOfShortTimeout`, `TestFetchTimeoutIsTerminalAndDoesNotRetry`, `TestLFSPullTimeoutIsTerminalAndDoesNotRetry`, `TestPushBranchTimeoutIsTerminalWithHint`, `TestZeroLongTimeoutMeansUnboundedTransfer`, and the `gitRunner` config round-trip tests.

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

> **`git stash create` and untracked files (`P9-WIP-02`, 2026-07-31).** The working-state
> plane's Layer B captures with `git stash create`, chosen precisely because it writes a
> commit object **without touching the worktree or index**. That choice carries a limit
> worth stating at the primitive rather than rediscovering at each call site: `stash
> create` does **not** include untracked files, and unlike `git stash push` it has no `-u`
> form. Capturing them would require mutating the working tree, which is the one thing
> this plane must never do.
>
> The consequence is not academic. A tree holding **only** new files produces no stash
> object at all, so before this was handled `wip push` reported "working tree is clean" —
> the most misleading thing a recovery feature can say, since a brand-new uncommitted file
> is the most common thing anyone forgets. `git.Runner.UntrackedCount` now supplies the
> count so callers never conflate "nothing to stash" with "nothing at risk": an
> untracked-only tree is refused with the `git add` remedy, and a mixed tree pushes with an
> explicit warning that the snapshot omits them.
>
> The general rule: when a primitive's blind spot is invisible in its return value —
> `stash create` returns empty for *both* a clean tree and an untracked-only one — the
> caller must distinguish the two, and the primitive's documentation must say so.
