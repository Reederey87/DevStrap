---
last_reviewed: 2026-06-28
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

### Independence from the cross-machine sync plane (2026-06-28)

The cloud-sync architecture (eager blobless clone of repo content, age-encrypted env/draft blobs, the signed HLC-ordered namespace map, and the cross-machine working-state plane — git-state validation + `refs/devstrap/wip/*`) is purely a *human* device-mirroring plane. It must **never** feed agent base resolution. Regardless of what the sync plane has materialized or observed on this device:

- agents always fetch and base fresh from `origin/<default_branch>` (the authoritative remote ref), never from synced working-state, WIP refs, encrypted draft bundles, or any local branch;
- repo content reaches the worktree over git's own transport (blobless clone/fetch from the existing remote), not through the DevStrap hub;
- a stale, dirty, or mid-sync device state has no effect on the SHA an agent worktree is created from.

This keeps agents reproducible and deterministic even while the surrounding `~/Code` tree is being synced across the owner's fleet (audit `AUDIT_RECOMMENDATIONS_2026-06-28.md`, `EAGER-*`/`DRAFT-*`/`HUB-*`).

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

Current implementation has the shared `internal/childenv` environment sanitizer used by Git/editor/agent subprocesses. `devstrap agent run` supports the `generic` engine: it creates a fresh upstream worktree, runs explicit argv commands in that isolated cwd with a sanitized no-secret default environment, applies a wrapper-level command policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`) plus a wrapper-level file path policy that denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs, records an `agent_runs` row, captures a `0600` log under `~/.devstrap/logs/agent-runs`, and stores a Git status/diff summary. `devstrap agent pr` reuses the stale-base gate before pushing and creating the PR — currently hardcoded to `gh pr create`, which fails post-push on non-GitHub remotes; it should route through a forge-agnostic `Forge` interface (`gh`/`glab`/`tea`) with graceful degradation (`FORGE-01`, see `08_GIT_MATERIALIZATION_AND_WORKTREES.md`). OS-enforced sandboxing, project-env allowlists for agents, `agent cleanup`, and non-generic engine adapters remain future work.

## Enforcement reality (audit `AGEN-01..06`, `SECU-02`)

The current wrapper-level enforcement oversells its safety and must not be presented as a sandbox:

- **Command/file policy is argv-substring matching, trivially bypassed by any interpreter** (`AGEN-01`). `bash -c "…"`, `python -c`, base64-decode, `rm -fr /` (variant spacing), variable indirection, or a script file all evade the deny list, so the default `guarded` profile actually gives an agent full filesystem **read** and **network exfil**. Treat substring matching as a guardrail against accidents, not a security boundary.
- **The agent subprocess forwards `HOME` and `SSH_AUTH_SOCK`** (`AGEN-02`/`SECU-02`), handing a live Git/SSH credential capability to semi-trusted code — contradicting "agents receive no secrets" and the `~/.ssh/**` deny. Strip both from the agent env unless an explicit, scoped opt-in is given.
- **Profile semantics are misleading** (`AGEN-04`): `cautious` is currently identical to `guarded`, `readonly` is not actually read-only, and `ephemeral-ci` (listed above) is rejected by the code. Either implement the distinctions or rename/remove the profiles so names match behavior.
- **There is no OS-enforced sandbox** under a profile literally named `guarded` (`AGEN-03`). Real isolation needs `sandbox-exec`/Seatbelt (macOS) or bubblewrap/landlock/seccomp (Linux); until then, say so plainly.
- **The file-path deny list is narrower than this spec** and ignores the project's stronger sensitive-file detector (`AGEN-05`); unify on the single `spec/11` ignore/deny compiler.

Direction: move to an **allowlist + OS sandbox** model, strip credential-bearing env by default, and make the default profile's true capability legible.

## Secret policy

Default:

```text
Agents receive no secrets.
```

In practice this is **not yet true**: the agent subprocess inherits `HOME` and `SSH_AUTH_SOCK` (`AGEN-02`/`SECU-02`), so an SSH-agent-backed Git credential is reachable. Strip both (and re-audit `internal/childenv`) so the default genuinely passes no secret capability.

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
7. create PR/MR via the forge-agnostic Forge interface (gh/glab/tea); on an unknown forge, print the pushed branch + a compare/MR URL (FORGE-01)
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

## Audit implementation notes (2026-06-28)

- **AGEN-01**: `enforceAgentCommandPolicy` blocks known interpreters/shells/downloaders (sh, bash, python*, node, curl, etc.) under non-yolo policies; `--policy` help text disclaims advisory-only scope.
- **AGEN-02**: `runAgentProcess` uses `childenv.AgentAllowlist()` which excludes `SSH_AUTH_SOCK` and `HOME` from inheritance; HOME is repointed to the worktree path so agent tooling cannot reach user dotfiles (`~/.ssh`, `~/.aws`, `~/.config/gh`).
- **AGEN-04**: Added `ephemeral-ci` to accepted policy profiles; replaced `>` substring check with argv-aware redirection detection.
- **AGEN-05**: `agentTokenLooksSensitive` now includes `credentials.json`, `service-account*.json`, `*.pem`, `*.key`; deny list expanded with `/.kube`, `/.docker`.
- **AGEN-06**: Agent PR body scrubbed through `redact.Scrub` before forge submission.
- **CLI-03**: `agent run` now propagates child exit codes as `100+N`.
