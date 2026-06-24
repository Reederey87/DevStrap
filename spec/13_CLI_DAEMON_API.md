# CLI and Daemon API

## CLI principles

- dry-run available for mutating commands;
- explain what will happen before destructive changes;
- never hide dirty-state warnings;
- keep commands composable;
- JSON output for automation;
- human-friendly rich output by default.

## Command groups

```text
devstrap init
devstrap scan
devstrap status
devstrap sync
devstrap open
devstrap hydrate
devstrap add
devstrap env
devstrap worktree
devstrap agent
devstrap devices
devstrap doctor
devstrap ignore
devstrap daemon
devstrap hub
devstrap export
```

## Initial commands

Current repository status as of `2026-06-24`:

```text
Implemented: devstrap init, devstrap status, devstrap doctor, devstrap version
Planned: scan, sync, open, hydrate, add, env, worktree, agent, devices, daemon, hub, export
```

### init

```bash
devstrap init ~/Code
```

Creates:

```text
~/Code
~/.devstrap/state.db
~/.devstrap/config.yaml
~/.devstrap/logs
```

Options:

```bash
--workspace-name artem-main
--no-daemon
--dry-run
```

### scan

```bash
devstrap scan ~/Code --adopt
```

Detects:

- Git repos;
- draft folders;
- duplicate remotes;
- secret-looking files;
- dependency folders;
- env templates;
- toolchains.

### status

```bash
devstrap status
devstrap status --devices
devstrap status --json
```

Example:

```text
Project                         Device     Code       Env      Tools    Status
work/acme/api                   this       current    ready    ready    ready
work/acme/web                   this       dirty      ready    ready    local changes
experiments/fs2                 this       draft      ready    n/a      synced
work/acme/data                  this       skeleton   mapped   unknown  not hydrated
```

### sync

```bash
devstrap sync
```

Does:

- push local events;
- pull remote events;
- reconcile skeletons;
- fetch repos if policy allows;
- never overwrite dirty worktrees.

Options:

```bash
--namespace-only
--fetch
--hydrate-eager
--dry-run
```

### open

```bash
devstrap open work/acme/api --cursor
devstrap open work/acme/api --vscode
```

Does:

- hydrate if skeleton;
- validate env/tooling;
- open editor.

### hydrate

```bash
devstrap hydrate work/acme/api
```

Options:

```bash
--partial
--full
--lfs
--no-bootstrap
```

### add

```bash
devstrap add git@github.com:acme/api.git --path work/acme/api
```

## Env commands

```bash
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local
devstrap env check work/acme/api
devstrap env bind work/acme/api --provider 1password --profile acme-dev
devstrap run work/acme/api -- uv run pytest
```

## Worktree commands

```bash
devstrap worktree new work/acme/api --fresh-main --name fix-tests
devstrap worktree list work/acme/api
devstrap worktree remove wt_01jz...
devstrap worktree cleanup --merged
```

## Agent commands

```bash
devstrap agent run work/acme/api --engine cursor --task "fix failing tests"
devstrap agent list
devstrap agent show arun_01jz...
devstrap agent pr arun_01jz...
devstrap agent cleanup --merged
```

## Device commands

```bash
devstrap devices list
devstrap devices approve dev_01jz...
devstrap devices revoke dev_01jz...
devstrap devices rename dev_01jz... gmk-ubuntu
```

## Doctor command

```bash
devstrap doctor
```

Checks:

- daemon running;
- database accessible;
- managed root exists;
- Git installed;
- SSH auth works;
- GitHub CLI optional;
- secret providers installed/authenticated;
- ignored generated folders;
- stale conflicts;
- service health.

## Local daemon API

Transport:

```text
Unix domain socket at ~/.devstrap/devstrapd.sock
```

Protocol options:

- HTTP over Unix socket: easiest for CLI/debugging;
- gRPC: stronger typed API but heavier;
- JSON-RPC: simple and portable.

Recommendation:

```text
MVP: HTTP+JSON over Unix socket.
```

## API endpoints

```text
GET  /v1/status
POST /v1/sync
POST /v1/hydrate
POST /v1/open
POST /v1/worktrees
POST /v1/agent-runs
GET  /v1/events
GET  /v1/projects
POST /v1/projects
GET  /v1/jobs
```

Example hydrate request:

```json
{
  "path": "work/acme/api",
  "mode": "partial",
  "open_editor": "cursor"
}
```

Example status response:

```json
{
  "workspace": "artem-main",
  "device": "mac-mini-upstairs",
  "projects": [
    {
      "path": "work/acme/api",
      "type": "git_repo",
      "state": "ready",
      "dirty": false,
      "env_ready": true
    }
  ]
}
```

## Daemon job types

```text
reconcile_root
scan_project
create_skeleton
hydrate_git_repo
fetch_git_repo
check_dirty_state
capture_env
hydrate_env
sync_push
sync_pull
create_worktree
run_agent
cleanup_worktree
```

## Logging

Log files:

```text
~/.devstrap/logs/devstrapd.log
~/.devstrap/logs/jobs.log
~/.devstrap/logs/agent-runs/<id>.log
```

Rules:

- redact secrets;
- include job id;
- include project path;
- use structured JSON logs internally;
- human summaries in CLI.

## Exit codes

```text
0 success
1 generic error
2 invalid config
3 daemon unavailable
4 conflict exists
5 dirty worktree blocks operation
6 auth/secrets error
7 Git error
8 network/sync error
9 policy violation
```
