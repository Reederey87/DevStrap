---
last_reviewed: 2026-06-26
tracks_code: [internal/cli/agent.go, internal/cli/worktree.go, internal/git/**]
---
# Agent Workspaces and Policies

## Goal

Make AI-agent development safe, reproducible, and fresh by default.

## Core rule

```text
Agents never work in the user's primary working tree.
Agents never branch from stale local default branch.
```

## Agent run lifecycle

```text
1. Resolve project path.
2. Fetch configured upstream branch.
3. Resolve remote SHA.
4. Create isolated worktree.
5. Create task branch.
6. Hydrate env with minimal scope.
7. Apply file/command policy.
8. Start agent.
9. Capture logs/diff/test result.
10. Summarize and optionally create PR.
11. Mark worktree active/complete/failed.
```

## Command

```bash
devstrap agent run work/acme/api \
  --engine generic \
  --task "fix failing tests" \
  -- npm test
```

## Agent worktree metadata

```yaml
agent_run_id: arun_01jz...
repo_id: repo_01jz...
project_path: work/acme/api
engine: cursor
task: fix failing tests
base_ref: origin/<default_branch>
base_sha: abc123
branch: agent/fix-failing-tests-20260623-120405-a13f92c0b31d
worktree_path: ~/.devstrap/worktrees/acme-api/agent-fix-failing-tests-20260623-120405-a13f92c0b31d
status: running
created_at: 2026-06-23T12:00:00Z
```

## Agent policies

Policy profiles:

```text
readonly       inspect only
cautious       edit repo, run tests, no network except package/Git providers
guarded        edit repo, limited commands, scoped secrets
yolo-local     broad local access, personal-only, explicit opt-in
ephemeral-ci   clean cloud task, runtime secrets only, auto-cleanup
```

## File access policy

Example:

```yaml
filesystem:
  allow:
    - repo/**
    - ~/.devstrap/worktrees/current/**
  deny:
    - repo/.env
    - repo/.env.*
    - ~/.ssh/**
    - ~/.snowflake/**
    - ~/.aws/**
    - ~/.config/gh/hosts.yml
    - "**/*service-account*.json"
```

MVP enforcement is wrapper-level, not perfect sandboxing:

- don't pass denied env;
- scan diffs and logs;
- block DevStrap-provided file reads;
- deny non-`yolo-local` command arguments that explicitly reference sensitive paths or paths outside the fresh worktree;
- launch agents in isolated cwd;
- later add container or OS sandbox.

## Command policy

Example:

```yaml
commands:
  allow:
    - git status
    - git diff
    - git add
    - git commit
    - uv run pytest
    - uv run ruff check
    - npm test
    - npm run lint
  deny_patterns:
    - "rm -rf /"
    - "cat .env"
    - "cat ~/.snowflake/config.toml"
    - "curl * | sh"
    - "chmod -R 777"
```

MVP enforcement options:

1. prompt/approval wrapper;
2. command allowlist for DevStrap-invoked commands;
3. agent-specific policy config;
4. terminal/session recording;
5. later: sandbox/container.

Current implementation has the shared `internal/childenv` environment sanitizer used by Git/editor/agent subprocesses. `devstrap agent run` supports the `generic` engine: it creates a fresh upstream worktree, runs explicit argv commands in that isolated cwd with a sanitized no-secret default environment, applies a wrapper-level command policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`) plus a wrapper-level file path policy that denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs, records an `agent_runs` row, captures a `0600` log under `~/.devstrap/logs/agent-runs`, and stores a Git status/diff summary. `devstrap agent pr` reuses the stale-base gate before pushing and calling `gh pr create`. OS-enforced sandboxing, project-env allowlists for agents, `agent cleanup`, and non-generic engine adapters remain future work.

## Secret policy

Default:

```text
Agents receive no secrets.
```

Project opt-in:

```yaml
agent_secrets:
  allow:
    - GITHUB_TOKEN_READONLY
    - SNOWFLAKE_ACCOUNT
  deny:
    - OPENAI_ADMIN_KEY
    - AWS_SECRET_ACCESS_KEY
```

## Network policy

MVP cannot fully enforce network restrictions without sandboxing, but it can document and wrap known commands.

Future:

- container network policy;
- local proxy;
- macOS Network Extension only for enterprise version;
- Linux namespace/firewall for isolated runner.

## Agent engines

Initial adapters:

```text
cursor-cli
codex-cli
copilot-cli
generic-command
```

Adapter interface:

```go
type AgentRunner interface {
    Name() string
    Prepare(ctx AgentContext) error
    Run(ctx AgentContext, task string) (*AgentResult, error)
    SupportsStreaming() bool
}
```

## PR flow

Command:

```bash
devstrap agent pr arun_01jz...
```

Algorithm:

```text
1. ensure agent run status is complete or reviewable
2. show diff summary
3. run configured validation
4. fetch origin/<default_branch>
5. rebase if needed/approved
6. push branch
7. create PR using gh/GitHub API
```

## Cleanup

Commands:

```bash
devstrap agent list
devstrap agent cleanup --merged
devstrap agent cleanup --older-than 14d
devstrap worktree remove <id> [--force]
```

Safety:

- never remove dirty worktree without explicit force;
- missing manually deleted worktrees require explicit force and run `git worktree prune`;
- if branch unpushed, warn;
- quarantine before destructive delete.

## Agent status view

Example:

```text
Agent runs

ID        Repo       Branch                    Base      Status    Tests
arun_a13  api        agent/fix-tests-a13       abc123    complete  passed
arun_b92  gss-agent  agent/add-check-b92       def456    running   pending
arun_c11  ui         agent/refactor-c11        old       stale     failed
```

## MVP acceptance criteria

- agent run creates worktree from fetched remote SHA;
- worktree metadata is recorded;
- agent env is scoped;
- logs are captured;
- diff summary is available;
- cleanup avoids deleting dirty/unpushed work;
- stale base is detected before PR.
