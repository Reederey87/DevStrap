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
devstrap worktree new work/org/repo --fresh-main --name fix-tests
```

Algorithm:

```text
1. Resolve namespace entry.
2. Ensure repo object cache/local clone exists.
3. Determine upstream: origin/main or configured default.
4. git fetch origin main --prune.
5. Resolve base SHA: git rev-parse origin/main.
6. Create branch name: agent/fix-tests-YYYYMMDD-HHMM.
7. Create worktree from base SHA.
8. Record worktree metadata.
9. Hydrate env/tooling.
10. Return path/open editor/launch agent.
```

Shell equivalent:

```bash
git fetch origin main --prune
BASE_SHA=$(git rev-parse origin/main)
git worktree add ~/.devstrap/worktrees/repo/fix-tests -b agent/fix-tests "$BASE_SHA"
```

Important:

```text
Never use local main as base.
```

## Worktree layout

```text
~/.devstrap/worktrees/
  repo-id/
    agent-fix-tests-20260623-1200/
    human-refactor-auth-20260623-1300/
```

Metadata:

```yaml
id: wt_01jz...
repo_id: repo_01jz...
path: ~/.devstrap/worktrees/repo/agent-fix-tests-20260623-1200
branch: agent/fix-tests-20260623-1200
base_ref: origin/main
base_sha: abc123
created_by: agent
agent_run_id: arun_01jz...
status: active
```

## Rebase freshness check

Before PR or finalization:

```text
1. git fetch origin main
2. compare stored base_sha to current origin/main
3. if changed:
     - warn
     - offer rebase
     - rerun tests
```

## Branch naming

Recommended:

```text
agent/<short-task>-<date>-<shortid>
human/<short-task>-<date>-<shortid>
```

Examples:

```text
agent/fix-gss-tests-20260623-a13f
agent/add-snowflake-env-check-20260623-b92c
human/refactor-devstrap-sync-20260623-c11a
```

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
cleanup: block unless --force or branch merged
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

Lock timeout behavior:

- wait briefly;
- show owning job;
- allow manual unlock only if process dead.

## Git provider integration

MVP:

- shell out to `git`;
- shell out to `gh` for GitHub PRs if installed.

Later:

- GitHub API;
- GitLab API;
- Bitbucket API;
- enterprise auth.

