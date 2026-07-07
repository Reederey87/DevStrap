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

Current implementation has the shared `internal/childenv` environment sanitizer used by Git/editor/agent subprocesses. `devstrap agent run` supports the `generic` engine: it creates a fresh upstream worktree, runs explicit argv commands in that isolated cwd with a sanitized no-secret default environment, applies a wrapper-level command policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`) plus a wrapper-level file path policy that denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs, records an `agent_runs` row with `status='running'` and the recorder PID, captures a `0600` log under `~/.devstrap/logs/agent-runs`, and stores a labeled Git diff summary split into `Committed since base:` (`BaseSHA..HEAD`) and `Uncommitted:` (`git status --short`) sections. If the post-create file policy denies the command, the just-created worktree is removed, its branch is deleted, and the DB row is marked removed. `devstrap agent list/show/pr` and `doctor` sweep `running` rows whose recorded PID is dead to `interrupted`; `devstrap agent pr` now refuses non-`complete` runs unless `--allow-incomplete` is explicit, then reuses the stale-base gate before pushing and creating a forge-aware PR/MR via `gh`/`glab`/`tea` when available, or a compare URL fallback for unsupported forges. **OS-enforced sandboxing slices shipped (2026-07-05, `P4-GIT-03`):** on macOS, `agent run` wraps the child argv in `/usr/bin/sandbox-exec` with a generated Seatbelt (SBPL) profile — allow-default with targeted denies: writes confined to the worktree + temp dir (plus a few device nodes; the log dir stays parent-only so the child cannot tamper with its own 0600 log), reads of credential paths denied (real `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`, `~/.kube`, `~/.docker`, `~/.devstrap/keys` — anchored on the REAL user home since the child env repoints `$HOME`; the darwin adapter resolves each anchor's leaf symlinks and denies both the literal alias and its resolved target, so a `~/.ssh -> /elsewhere` symlink is denied at the kernel-real path, mirroring the bwrap masks) — and the run fails closed when that real home cannot be resolved, since an empty anchor would silently drop every home-anchored credential deny while still reporting the run as sandboxed (`--sandbox off` is the explicit escape hatch), and all network denied for `readonly`/`cautious` policies. On Linux, `agent run` lazily chooses `bubblewrap`, then Landlock, then unsupported. The bubblewrap backend uses `--ro-bind / /` for allow-default reads and deny-default writes, read-write binds for only the worktree and per-run temp dir, tmpfs and `/dev/null` mounts for credential masks, `--unshare-net` for deny-network policies, `--unshare-user` plus probe-feature-gated `--disable-userns`, `--unshare-pid` plus `--die-with-parent`, and `--new-session` to block the TIOCSTI terminal-injection escape. The Landlock fallback wraps the child argv through a hidden `devstrap sandbox-helper` re-exec shim because Landlock must restrict its own process before `execve()`: it pins a strict ABI v3 floor so raw `truncate(2)` outside the worktree is denied, grants read+execute everywhere, confines writes to the worktree and per-run tmp with `REFER` for Git's cross-directory object renames, allows the needed `/dev` nodes and `/dev/shm`, and deliberately leaves the log dir unwritable. It is additive-allow, so credential reads are NOT denied; network denial is TCP bind/connect only and only on Landlock ABI >= 4; there are no mount or pid namespaces. `agent run` surfaces that degrade with one `notice: OS sandbox landlock active with reduced guarantees: ...` stderr line. Linux availability is probe-based, not stat-based, so userns-restricted hosts can fall through to Landlock instead of pretending bubblewrap is usable. `DEVSTRAP_SANDBOX_BACKEND=bwrap|landlock` forces one Linux backend and never silently falls back. `--sandbox auto|off|require` (env `DEVSTRAP_SANDBOX`, default `auto`): `auto` sandboxes when the host adapter is available and otherwise prints one warning and runs with today's advisory behavior; `require` refuses to run unsandboxed (policy exit class, checked BEFORE worktree creation); `yolo-local` stays unconfined and conflicts with `require`. **Both Linux backends additionally install a seccomp syscall denylist (2026-07-05, `P4-GIT-03` slice 4):** the mount, kernel-module/boot, ptrace/tracing, keyring, io_uring, and legacy-escape syscalls return `EPERM` (default action stays Allow — a targeted denylist, not an allowlist); `clone`/`clone3`/`unshare`/`setns` and `execve`/`fork` stay allowed so nested sandboxes and the agent's own launches keep working. bubblewrap receives the compiled cBPF filter over an inherited fd (`--seccomp`); the Landlock shim loads it in-process after the ruleset and before `execve`. The denylist is compiled for the running arch (x86-only names like `vm86`/`modify_ldt`/`_sysctl` are dropped on arm64 so assembly never fails). It is unconditional hardening for every sandboxed policy; `DEVSTRAP_SANDBOX_SECCOMP=off` disables it (a mistyped value fails closed with the invalid-config exit class), and a kernel without seccomp-filter support degrades to a `Limitations()` line rather than failing `require` (the fs/network boundary is intact). **`sandbox.violation` telemetry shipped as unsigned local visibility (P4-GIT-03 slice 5):** each run records sandbox backend/mode/limitations on `agent_runs`, macOS Seatbelt deny rules embed a per-run tag, post-run log collection records scrubbed denial rows in `sandbox_violations`, and `agent show`/`doctor` surface them. Linux runtime denial detection is still future; Linux runs populate only backend/mode/limitations. Tighter read confinement is the remaining named sandbox follow-up. Project-env allowlists for agents, `agent cleanup`, and non-generic engine adapters remain future work; `doctor` now probes the matching forge CLI per adopted remote (`FORGE-04`/`GIT-05`).

## Enforcement reality (audit `AGEN-01..06`, `SECU-02`)

The current wrapper-level enforcement oversells its safety and must not be presented as a sandbox:

- **Command/file policy is argv-substring matching, trivially bypassed by any interpreter** (`AGEN-01`). `bash -c "…"`, `python -c`, base64-decode, `rm -fr /` (variant spacing), variable indirection, or a script file all evade the deny list, so the default `guarded` profile actually gives an agent full filesystem **read** and **network exfil**. Treat substring matching as a guardrail against accidents, not a security boundary.
- **Credential-env inheritance is fixed, but the wrapper is still not a sandbox** (`AGEN-02`/`SECU-02`): `AgentAllowlist` excludes `SSH_AUTH_SOCK`, avoids inheriting the user's `HOME`, and repoints `HOME` to the worktree. The remaining risk is broader: the subprocess still has normal filesystem and network capability unless an OS sandbox (Seatbelt / bubblewrap-landlock-seccomp) constrains it.
- **Profile semantics are partially misleading** (`AGEN-04`, partly fixed): `ephemeral-ci` is now accepted and `readonly` denies redirection and known mutating commands (argv-aware, wrapper-level only); but `cautious` is still behaviorally identical to `guarded`, and `ephemeral-ci` has no cloud/auto-cleanup semantics yet — implement the distinctions or fold the profiles.
- **OS-enforced sandbox: macOS Seatbelt, Linux bubblewrap, and Linux Landlock fallback shipped** (`AGEN-03`/`P4-GIT-03`, 2026-07-05). The wraps above are kernel-enforced for the child and everything it spawns — the interpreter bypasses in the first bullet can no longer write outside the worktree; full-fidelity backends also read-deny/mask the denied credential paths and (under `readonly`/`cautious`) deny ordinary network sockets. **Because an agent worktree is a git _linked_ worktree, all three backends additionally grant write to the linked worktree's git storage — the shared object store, refs, and reflogs in the clone's git-common-dir, plus the per-worktree admin dir — so the agent's `git add`/`git commit` are not kernel-EPERM'd (`P7-SANDBOX-01`, resolved by `git.Runner.WorktreeSandboxWriteDirs`). The common dir's `hooks/` and `config` are deliberately NOT granted: granting them would let the child plant a hook or config that executes UNSANDBOXED on a later git operation. These git dirs also join the `--read-confine` allow-list so git reads work under the `readonly` policy; a live Seatbelt e2e (`TestSeatbeltAllowsLinkedWorktreeCommit`) proves the commit fails without the grant and succeeds with it.** **Both Linux backends install a seccomp syscall denylist** (mount/kexec/ptrace/keyring/io_uring/legacy-escape syscalls → `EPERM`; `DEVSTRAP_SANDBOX_SECCOMP=off` opt-out). **`sandbox.violation` telemetry is live as unsigned local visibility**: backend/mode/limitations are recorded per run, macOS Seatbelt denials are collected from tagged unified-log rows after the run, scrubbed, and shown by `agent show`/`doctor`; Linux runtime denial detection remains future. **Tighter read confinement is shipped** (`--read-confine auto|on|off`, env `DEVSTRAP_SANDBOX_READ_CONFINE`, default-on for the `readonly` policy; `--read-allow <abs>` adds roots): all three backends restrict the child's reads to the worktree/tmp, the OS toolchain/system roots, and the `$HOME` build caches instead of the whole disk — the Seatbelt profile denies all reads then re-allows the roots (credential denies stay last so they out-rank the allows; a global `file-read-metadata` allow keeps stat/traversal working), bubblewrap exposes only the roots via `--ro-bind-try`, and Landlock restricts its `RODirs` grant to the roots (which finally gives the Landlock fallback a credential-read boundary). `--sandbox require` refuses to launch if a requested read confinement cannot be enforced. The remaining OS-sandbox direction is containerization (spec/14). Linux caveats: bubblewrap credential masks hide paths (ENOENT/empty) rather than returning EPERM, nested-sandbox tools (Chrome headless, flatpak, bwrap-in-bwrap) can break under `--disable-userns`, and Landlock is a reduced fallback (credential reads still allowed; network deny TCP-only at ABI >= 4; no mount/pid namespace; and — because Landlock has no `--new-session` analogue — the `TIOCSTI` terminal-injection escape stays open on the Landlock path, since seccomp does not arg-filter `ioctl`). A kernel without seccomp-filter support degrades to a reduced-guarantees notice, not a hard failure. `--sandbox require` accepts Landlock as a real write-confinement boundary except when the policy demands a network deny that the backend cannot enforce at all (ABI < 4), in which case it fails with the policy exit class before worktree creation; a TCP-only deny (ABI >= 4) passes `require` with a warning that UDP, QUIC, and unix-domain sockets stay open; `--sandbox off` is the explicit escape hatch.
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

Current implementation strips credential-bearing inherited env (`SSH_AUTH_SOCK`) and does not expose the user's `HOME`; `HOME` is repointed to the worktree so common dotfile secret locations are not reachable through `~`. This is necessary but not sufficient: project-env allowlists are still future work, and if the OS sandbox is unavailable, disabled, or bypassed via `yolo-local`, an agent can still read any path its process user can access if it discovers or constructs that path. Treat "no secrets by default" as the wrapper contract, with the Seatbelt/bubblewrap sandbox as the kernel-enforced layer where available.

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
