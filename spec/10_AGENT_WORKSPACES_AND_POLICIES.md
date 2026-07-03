---
last_reviewed: 2026-07-03
tracks_code: [internal/cli/agent.go, internal/cli/forge.go, internal/cli/worktree.go, internal/childenv/**, internal/git/**]
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

This keeps agents reproducible and deterministic even while the surrounding `~/Code` tree is being synced across the owner's fleet (audit `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`, `EAGER-*`/`DRAFT-*`/`HUB-*`).

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
engine: generic  # cursor/codex/copilot adapters are planned; only `generic` ships today
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
    - ~/.aws/**
    - ~/.kube/**
    - ~/.config/gcloud/**
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
    - "cat ~/.aws/credentials"
    - "curl * | sh"
    - "chmod -R 777"
```

MVP enforcement options:

1. prompt/approval wrapper;
2. command allowlist for DevStrap-invoked commands;
3. agent-specific policy config;
4. terminal/session recording;
5. later: sandbox/container.

Current implementation has the shared `internal/childenv` environment sanitizer used by Git/editor/agent subprocesses. `devstrap agent run` supports the `generic` engine: it creates a fresh upstream worktree, runs explicit argv commands in that isolated cwd with a sanitized no-secret default environment, applies a wrapper-level command policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`) plus a wrapper-level file path policy that denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs, records an `agent_runs` row, captures a `0600` log under `~/.devstrap/logs/agent-runs`, and stores a Git status/diff summary. If the post-create file policy denies the command, the just-created worktree is removed, its branch is deleted, and the DB row is marked removed. `devstrap agent pr` reuses the stale-base gate before pushing and creating a forge-aware PR/MR via `gh`/`glab`/`tea` when available, or a compare URL fallback for unsupported forges. OS-enforced sandboxing, project-env allowlists for agents, `agent cleanup`, and non-generic engine adapters remain future work; `doctor` now probes the matching forge CLI per adopted remote (`FORGE-04`/`GIT-05`).

## Enforcement reality (audit `AGEN-01..06`, `SECU-02`)

The current wrapper-level enforcement oversells its safety and must not be presented as a sandbox:

- **Command/file policy is argv-substring matching, trivially bypassed by any interpreter** (`AGEN-01`). `bash -c "…"`, `python -c`, base64-decode, `rm -fr /` (variant spacing), variable indirection, or a script file all evade the deny list, so the default `guarded` profile actually gives an agent full filesystem **read** and **network exfil**. Treat substring matching as a guardrail against accidents, not a security boundary.
- **Credential-env inheritance is fixed, but the wrapper is still not a sandbox** (`AGEN-02`/`SECU-02`): `AgentAllowlist` excludes `SSH_AUTH_SOCK`, avoids inheriting the user's `HOME`, and repoints `HOME` to the worktree. The remaining risk is broader: the subprocess still has normal filesystem and network capability unless an OS sandbox (Seatbelt / bubblewrap-landlock-seccomp) constrains it.
- **Profile semantics are partially misleading** (`AGEN-04`, partly fixed): `ephemeral-ci` is now accepted and `readonly` denies redirection and known mutating commands (argv-aware, wrapper-level only); but `cautious` is still behaviorally identical to `guarded`, and `ephemeral-ci` has no cloud/auto-cleanup semantics yet — implement the distinctions or fold the profiles.
- **There is no OS-enforced sandbox** under a profile literally named `guarded` (`AGEN-03`). Real isolation needs `sandbox-exec`/Seatbelt (macOS) or bubblewrap/landlock/seccomp (Linux); until then, say so plainly.
- **The file-path deny list is narrower than this spec** and ignores the project's stronger sensitive-file detector (`AGEN-05`); unify on the single `spec/11` ignore/deny compiler.

Direction: move to an **allowlist + OS sandbox** model, strip credential-bearing env by default, and make the default profile's true capability legible.

## Direction: DevStrap as the substrate agents run on (AD-5)

> Forward direction, not shipped. From the sixth-pass viability review; see `docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`.

Modern agent harnesses (Claude Code, Cursor, Codex, Copilot) increasingly manage their own worktrees and OS-level sandboxes, and the generic wrapper runner here cannot authenticate a real harness (it strips API keys and repoints `$HOME`). DevStrap's durable value is therefore the **substrate** agents run on — cross-machine workspace consistency plus fresh-base provenance (fetched `origin/<default_branch>`, recorded base SHA), a queryable run/worktree registry, and the stale-base gate — not the wrapper itself. Planned direction:

- expose `devstrap worktree new --fresh-upstream --json` as a **provisioning primitive** that harnesses call to obtain an isolated, fresh-based worktree;
- add `devstrap worktree adopt` / `devstrap agent adopt` to register externally-created worktrees so the registry and stale-base gate keep their value regardless of who runs the agent;
- ship one reference integration — a harness hook/plugin or a small MCP server over the namespace — rather than growing the bespoke wrapper;
- reframe the wrapper command/file policy honestly as **guardrails, not a sandbox**, and delegate real isolation to harness-native sandboxes composed *inside* a DevStrap worktree;
- the shipped engine set is `generic` only; the `cursor`/`codex`/`copilot` adapters listed under "Agent engines" are planned, and this substrate framing is the preferred path over per-harness wrapper adapters.

## Secret policy

Default:

```text
Agents receive no secrets.
```

Current implementation strips credential-bearing inherited env (`SSH_AUTH_SOCK`) and does not expose the user's `HOME`; `HOME` is repointed to the worktree so common dotfile secret locations are not reachable through `~`. This is necessary but not sufficient: project-env allowlists are still future work, and without an OS sandbox an agent can still read any path its process user can access if it discovers or constructs that path. Treat "no secrets by default" as the wrapper contract, not as a hard isolation boundary until sandboxing lands.

Project opt-in:

```yaml
agent_secrets:
  allow:
    - GITHUB_TOKEN_READONLY
    - API_BASE_URL
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
devstrap agent cleanup --merged          # planned — use `devstrap worktree cleanup` today
devstrap agent cleanup --older-than 14d  # planned — use `devstrap worktree cleanup` today
devstrap worktree cleanup --merged
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

ID        Repo        Branch                    Base      Status    Tests
arun_a13  api         agent/fix-tests-a13       abc123    complete  passed
arun_b92  api-worker  agent/add-check-b92       def456    running   pending
arun_c11  ui          agent/refactor-c11        old       stale     failed
```

The `Tests` column is a **planned** surface — no test-result tracking is wired today (only the `generic` engine ships), so shipped output omits it.

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

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-GIT-02 — diff summary misses committed agent work

**Problem.** `agentDiffSummary` (`internal/cli/agent.go:479-480`) only runs revision-less `git status --short` + `git diff --stat`, so an agent that commits its work records "(no changes)"; `agent show` (`:181`) and the PR body gate (`:504`) then omit the real diff, violating the "diff summary is available" acceptance criterion. The recorded `BaseSHA` (`:97`) is never diffed against.

**Actionable steps.**
1. Change the signature to take the worktree; diff base-vs-HEAD plus uncommitted residue in labeled sections.
2. Guard the unborn-HEAD case by falling back when `rev-parse --verify HEAD` fails.
3. Test: agent command runs `git commit -am x`; assert the summary contains the committed file stat.

**Example.**
```go
committed, _ := r.Run(ctx, wt.Path, "diff", "--stat", wt.BaseSHA+"..HEAD")
uncommitted, _ := r.Run(ctx, wt.Path, "status", "--short")
// join with labeled "committed:" / "uncommitted:" sections
```

### P6-GIT-06 — `agent pr` never checks run status

**Problem.** `newAgentPRCommand` (`internal/cli/agent.go:203`) proceeds through drift check, push, and PR creation (`:220-246`) without ever reading `run.Status`, so a failed run (`status='failed'`) opens a PR of broken work; a SIGKILL/crash also leaves the row stuck at `running` (`:95-99`, corrected only at `:112`) with no reconciliation, and that phantom run is also PR-able.

**Actionable steps.**
1. Reject unless `Status == "complete"` with a `--allow-incomplete` override that warns.
2. Add an `agent_runs` runner-PID column migration (update spec/12) and have `doctor`/`agent list` sweep dead PIDs to `interrupted`.
3. Testscript: failed run → `agent pr` exits invalid-config; `--allow-incomplete` proceeds to dry-run.

**Example.**
```sql
UPDATE agent_runs SET status='interrupted' WHERE status='running'; -- for dead runner PIDs
```
