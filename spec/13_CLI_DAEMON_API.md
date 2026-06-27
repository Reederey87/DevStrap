---
last_reviewed: 2026-06-26
tracks_code: [cmd/**, internal/cli/**, internal/platform/**]
---
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
devstrap version
devstrap scan
devstrap status
devstrap db
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

Current repository status as of `2026-06-25`:

```text
Implemented: devstrap init, scan, add, hydrate, open, sync --hub-file, worktree new/status/finalize/list/remove/cleanup, env capture/hydrate/bind, run, agent run/list/show/pr, devices list/approve/revoke/lost/rename, status, doctor, version, db migrate/status/backup/down
Planned: production sync hub, env check, OS-enforced agent sandboxing, automatic remote device enrollment/fingerprint confirmation, daemon, export
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
--dry-run
```

`init` normalizes the root to an absolute clean path, creates `~/.devstrap/config.yaml` with mode `0600` if missing, and does not overwrite an existing config file.

### db

```bash
devstrap db migrate
devstrap db status
devstrap db backup ~/.devstrap/backups/state-20260624.db
devstrap db down
```

Rules:

- `migrate` applies all embedded Goose migrations;
- `status` prints schema version, SQLite `quick_check`, and SQLite `foreign_key_check`;
- `backup` uses `VACUUM INTO`, not file copy;
- state DB and backups are mode `0600`.

`doctor` reports schema version, SQLite `quick_check`, SQLite `foreign_key_check`, local age device-key health, and local Ed25519 signing-key health when the state database exists.

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

Current implementation:

- prunes generated folders before descent;
- records secret-looking filename warnings but never file values;
- only persists a discovered git remote after it passes validation, so an unvalidated/dangerous origin (e.g. `ext::`) is never stored for a later materialization step;
- normalizes SSH, HTTPS, `ssh://`, absolute, and `file://` remotes;
- `--adopt` writes namespace, git repo, draft project, and device project state rows;
- escaping symlinks are hard-excluded (never adopted) and surfaced as conflict rows; dangling/IO symlink errors are advisory warnings only;
- `--quarantine` moves secret-looking files out of the managed tree into a dated `~/.devstrap/quarantine/<YYYYMMDD>/` directory (mode `0600`) instead of leaving them in place.

### status

```bash
devstrap status
devstrap status --json
```

Current Phase-0 status shows workspace name, root path, project count, local device ID, and adopted project rows. Future daemon-backed status adds:

```bash
devstrap status --devices
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
devstrap sync --hub-file ~/.devstrap/test-hub/events.json
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

Current implementation supports the file-backed test hub only. It requires `--hub-file`, pushes all local events, pulls hub events from the beginning, applies namespace events idempotently, supports `--namespace-only` and `--dry-run`, and reports that hydration/fetch reconciliation is not implemented yet.

### open

```bash
devstrap open work/acme/api --cursor
devstrap open work/acme/api --vscode
```

Does:

- hydrate if skeleton;
- validate env/tooling;
- open editor.

Current implementation hydrates if needed, refuses unknown namespace paths, checks that `cursor` or `code` exists, starts the editor without tying it to the CLI context, and releases the child process handle. Env/tooling validation is still future work.

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

Current implementation uses partial clone by default, supports `--full` and `--lfs`, refuses to clone into non-empty non-skeleton directories, stages clones in hidden sibling temp directories, promotes only after clone success plus a second target validation, preserves the original skeleton on clone failure, and updates local materialization/dirty state.

### add

```bash
devstrap add git@github.com:acme/api.git --path work/acme/api --lfs-policy auto
```

Options:

```bash
--path
--default-branch
--lfs-policy auto|never|agent|always
```

## Env commands

```bash
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local
devstrap env check work/acme/api
devstrap env bind work/acme/api .env.refs --provider 1password --profile acme-dev
devstrap run work/acme/api -- uv run pytest
```

Current implementation supports `env capture`, `env hydrate`, `env bind`, and top-level `run`. Capture parses a local env file with a non-interpolating grammar, refuses dangerous names, rejects interpolation-looking values unless `--literal` is passed, encrypts the bundle to the local device age recipient, writes a `0600` age blob under `~/.devstrap/blobs`, stores only `age_blob:<sha256>` references in `secret_bindings`, and appends the captured file path to project `.gitignore` when possible. Hydrate decrypts the local age blob with the local device identity or resolves 1Password provider refs through `op inject`, writes only to an explicit `--write` target, creates the file atomically with mode `0600`, refuses to overwrite unless `--force` is passed, and appends the hydrated target to project `.gitignore` when possible. Bind stores 1Password `op://` provider refs without resolving plaintext. `run` injects encrypted profiles directly into the subprocess environment or delegates provider refs to `op run --env-file <temp-refs-file> -- <command>`.

## Worktree commands

```bash
devstrap worktree new work/acme/api --fresh-upstream --name fix-tests
devstrap worktree status wt_01jz...
devstrap worktree finalize wt_01jz... [--allow-stale-base]
devstrap worktree list
devstrap worktree remove wt_01jz... [--force]
devstrap worktree cleanup --merged [--force]
devstrap worktree unlock work/acme/api [--force]
```

Current implementation requires `--fresh-upstream` for `worktree new`, fetches `origin/<default_branch>` before resolving the base SHA, writes a per-repo lock under `~/.devstrap/locks`, records worktree metadata, honors the stored LFS policy by either running `git lfs pull` or warning about pointer files, and refuses dirty worktree removal unless `--force` is explicit. `worktree remove --force` handles manually deleted worktree paths by running `git worktree prune` from the main checkout and marking the DB row removed. `worktree status <id>` re-fetches the recorded base ref and reports whether the worktree is fresh or stale. `worktree finalize <id>` reuses the same stale-base check and exits non-zero if the base moved unless `--allow-stale-base` is set. `cleanup --merged` removes clean, merged worktrees, prunes stale missing paths, reports a skipped count for unreadable or dirty worktrees, and only removes a merged-but-dirty worktree when `--force` is set. `worktree unlock <path>` reports the holder of a project's repo operation lock and clears it when the holder is dead/stale (or when `--force` is set), providing a recovery path after a crash; `doctor` also lists held locks. The default-branch resolution for `worktree new` confirms the remote default authoritatively via `git ls-remote --symref origin HEAD`, repairing a missing `origin/HEAD` with `git remote set-head origin --auto` and warning if the result is not authoritative.

## Agent commands

```bash
devstrap agent run work/acme/api --engine generic --task "fix failing tests" -- npm test
devstrap agent list
devstrap agent show arun_01jz...
devstrap agent pr arun_01jz...
devstrap agent cleanup --merged
```

Current implementation supports `agent run/list/show/pr`. `agent run` creates a fresh upstream worktree, runs an explicit generic command with a sanitized no-secret default environment, applies wrapper-level command and file path policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`), records the run in SQLite, captures a `0600` log, and stores a Git status/diff summary. The file path policy denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs; it is a preflight wrapper policy, not an OS sandbox. `agent pr` refuses stale recorded bases unless `--allow-stale-base` is passed, pushes the agent branch, and calls `gh pr create`; `--dry-run` reports the planned PR without pushing. Non-generic engines, project-env allowlists, OS-enforced sandboxing, and `agent cleanup` remain future work.

## Device commands

```bash
devstrap devices list
devstrap devices enroll dev_01jz... --name gmk-ubuntu --os linux --arch arm64 --age-recipient age1...
devstrap devices approve dev_01jz...
devstrap devices revoke dev_01jz...
devstrap devices lost dev_01jz...
devstrap devices rename dev_01jz... gmk-ubuntu
```

Current implementation manually enrolls remote device records with age recipients, lists and renames device records, and updates non-local device trust state to `approved`, `revoked`, or `lost`. Env capture encrypts local bundles to the local recipient plus approved remote recipients. It refuses to change the current local device trust state so a user cannot revoke the only active local root by accident. Automatic remote enrollment, out-of-band fingerprint confirmation UX, and bundle re-encryption hooks remain future work.

## Doctor command

```bash
devstrap doctor
```

Checks:

- database existence and migration status;
- SQLite `quick_check`;
- local device age public/private identity match;
- state-home permissions;
- managed root exists;
- Git installed;
- Go installed;
- GitHub CLI optional;
- daemon running;            # future daemon phase
- SSH auth works;            # future Git materialization phase
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

Current Phase-0 JSON status response. It includes the workspace name, root path, project count, the local `device_id`, and a `projects` array of adopted rows (each with id, path, path_key, type, materialization_policy, status, and the optional git/local fields remote_url, default_branch, materialization_state, dirty_state):

```json
{
  "workspace_name": "personal",
  "root_path": "/Users/me/Code",
  "project_count": 1,
  "device_id": "dev_01jz...",
  "projects": [
    {
      "id": "prj_01jz...", "path": "work/acme/api", "path_key": "work/acme/api",
      "type": "git_repo", "materialization_policy": "lazy", "status": "active",
      "remote_url": "git@github.com:acme/api.git", "default_branch": "main",
      "materialization_state": "available", "dirty_state": "clean"
    }
  ]
}
```

Future project-level status response:

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
- use `log/slog` with a single configured handler;
- use a `ReplaceAttr` redaction choke point for secret-like attributes;
- emit text logs for interactive TTY output and JSON logs for daemon/service files;
- bind verbosity to `DEVSTRAP_LOG_LEVEL`, `--quiet`, and `--verbose`;
- rotate and retain logs under `~/.devstrap/logs`;
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
