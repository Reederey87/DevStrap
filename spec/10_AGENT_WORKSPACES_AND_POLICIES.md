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

Current implementation has the shared `internal/childenv` environment sanitizer used by Git/editor/agent subprocesses. `devstrap agent run` supports the `generic` engine: it creates a fresh upstream worktree, runs explicit argv commands in that isolated cwd with a sanitized no-secret default environment, applies a wrapper-level command policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`) plus a wrapper-level file path policy that denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs, records an `agent_runs` row with `status='running'` and the recorder PID, captures a `0600` log under `~/.devstrap/logs/agent-runs`, and stores a labeled Git diff summary split into `Committed since base:` (`BaseSHA..HEAD`) and `Uncommitted:` (`git status --short`) sections. If the post-create file policy denies the command, the just-created worktree is removed, its branch is deleted, and the DB row is marked removed. `devstrap agent list/show/pr` and `doctor` sweep `running` rows whose recorded PID is dead to `interrupted`; `devstrap agent pr` now refuses non-`complete` runs unless `--allow-incomplete` is explicit, then reuses the stale-base gate before pushing and creating a forge-aware PR/MR via `gh`/`glab`/`tea` when available, or a compare URL fallback for unsupported forges. **OS-enforced sandboxing first slice shipped (2026-07-05, `P4-GIT-03`):** on macOS, `agent run` wraps the child argv in `/usr/bin/sandbox-exec` with a generated Seatbelt (SBPL) profile — allow-default with targeted denies: writes confined to the worktree + temp dir (plus a few device nodes; the log dir stays parent-only so the child cannot tamper with its own 0600 log), reads of credential paths denied (real `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`, `~/.kube`, `~/.docker`, `~/.devstrap/keys` — anchored on the REAL user home since the child env repoints `$HOME`) — and the run fails closed when that real home cannot be resolved, since an empty anchor would silently drop every home-anchored credential deny while still reporting the run as sandboxed (`--sandbox off` is the explicit escape hatch), and all network denied for `readonly`/`cautious` policies. `--sandbox auto|off|require` (env `DEVSTRAP_SANDBOX`, default `auto`): `auto` sandboxes when the host adapter is available and otherwise prints one warning and runs with today's advisory behavior; `require` refuses to run unsandboxed (policy exit class, checked BEFORE worktree creation); `yolo-local` stays unconfined and conflicts with `require`. Linux (landlock/bubblewrap) and `sandbox.violation` telemetry are named follow-up slices. Project-env allowlists for agents, `agent cleanup`, and non-generic engine adapters remain future work; `doctor` now probes the matching forge CLI per adopted remote (`FORGE-04`/`GIT-05`).

## Enforcement reality (audit `AGEN-01..06`, `SECU-02`)

The current wrapper-level enforcement oversells its safety and must not be presented as a sandbox:

- **Command/file policy is argv-substring matching, trivially bypassed by any interpreter** (`AGEN-01`). `bash -c "…"`, `python -c`, base64-decode, `rm -fr /` (variant spacing), variable indirection, or a script file all evade the deny list, so the default `guarded` profile actually gives an agent full filesystem **read** and **network exfil**. Treat substring matching as a guardrail against accidents, not a security boundary.
- **Credential-env inheritance is fixed, but the wrapper is still not a sandbox** (`AGEN-02`/`SECU-02`): `AgentAllowlist` excludes `SSH_AUTH_SOCK`, avoids inheriting the user's `HOME`, and repoints `HOME` to the worktree. The remaining risk is broader: the subprocess still has normal filesystem and network capability unless an OS sandbox (Seatbelt / bubblewrap-landlock-seccomp) constrains it.
- **Profile semantics are partially misleading** (`AGEN-04`, partly fixed): `ephemeral-ci` is now accepted and `readonly` denies redirection and known mutating commands (argv-aware, wrapper-level only); but `cautious` is still behaviorally identical to `guarded`, and `ephemeral-ci` has no cloud/auto-cleanup semantics yet — implement the distinctions or fold the profiles.
- **OS-enforced sandbox: macOS shipped, Linux pending** (`AGEN-03`/`P4-GIT-03` slice 1, 2026-07-05). On macOS the Seatbelt wrap above is kernel-enforced for the child and everything it spawns — the interpreter bypasses in the first bullet can no longer write outside the worktree, read the denied credential paths, or (under `readonly`/`cautious`) touch the network. On Linux the wrapper remains advisory until the bubblewrap/landlock/seccomp slice lands; `--sandbox auto` says so loudly at run start.
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
1. sweep dead recorder PIDs to `interrupted` and require agent run status `complete` unless `--allow-incomplete` is explicit
2. show diff summary (`Committed since base:` plus `Uncommitted:` sections)
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
- diff summary is available for both committed work since the recorded base SHA and uncommitted residue;
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

### P6-GIT-02 — diff summary misses committed agent work — shipped 2026-07-03

**Problem.** `agentDiffSummary` previously only ran revision-less `git status --short` + `git diff --stat`, so an agent that committed its work recorded "(no changes)"; `agent show` and the PR body gate then omitted the real diff, violating the "diff summary is available" acceptance criterion. The recorded `BaseSHA` was never diffed against.

**Shipped behavior.**
1. `agentDiffSummary` takes the recorded base SHA and returns labeled `Committed since base:` and `Uncommitted:` sections.
2. The committed section diffs `BaseSHA..HEAD`; the uncommitted section keeps `git status --short` output.
3. If `rev-parse --verify HEAD` fails for an unborn repository, the helper falls back to the old working-tree-only summary.
4. Tests cover committed work, uncommitted residue, and the unborn-HEAD fallback.

**Example.**
```go
committed, _ := r.Run(ctx, wt.Path, "diff", "--stat", wt.BaseSHA+"..HEAD")
uncommitted, _ := r.Run(ctx, wt.Path, "status", "--short")
// join with labeled "committed:" / "uncommitted:" sections
```

### P6-GIT-06 — `agent pr` never checks run status — shipped 2026-07-04

**Problem.** `newAgentPRCommand` (`internal/cli/agent.go:203`) proceeds through drift check, push, and PR creation (`:220-246`) without ever reading `run.Status`, so a failed run (`status='failed'`) opens a PR of broken work; a SIGKILL/crash also leaves the row stuck at `running` (`:95-99`, corrected only at `:112`) with no reconciliation, and that phantom run is also PR-able.

**Shipped behavior.**
1. `agent pr` rejects unless `Status == "complete"` with `exitConflict`, matching the stale-base refusal class; `--allow-incomplete` warns and proceeds.
2. Migration `00021_agent_run_runner_pid.sql` records `agent_runs.runner_pid` for new runs. `agent list`, `agent show`, `agent pr`, and `doctor` sweep `running` rows whose recorder PID is dead to `interrupted`; rows with no recorded PID are left `running`.
3. Tests cover failed-run refusal/override, dead-PID reconciliation before PR gating, live-PID preservation, and NULL-PID preservation.

**Example.**
```sql
UPDATE agent_runs SET status='interrupted' WHERE status='running'; -- for dead runner PIDs
```
