---
last_reviewed: 2026-06-29
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
Implemented:
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
devstrap run
devstrap worktree
devstrap agent
devstrap devices
devstrap conflicts
devstrap doctor

Planned:
devstrap ignore
devstrap daemon
devstrap hub
devstrap export
devstrap materialize
devstrap run-loop
devstrap promote
devstrap gitstate
devstrap wip
```

## Initial commands

Current repository status as of `2026-06-28`:

```text
Implemented: devstrap init, version, scan, add, clone, hydrate, open, sync --hub-file, materialize, draft snapshot create, run-loop, status, doctor, conflicts list/show/resolve, db migrate/status/backup/down, env capture/hydrate/bind, run, worktree new/status/finalize/list/remove/cleanup/unlock, agent run/list/show/pr, devices enroll/list/approve/revoke/lost/rename
Planned: production R2/S3 SDK wiring, env check, OS-enforced agent sandboxing, automatic remote device enrollment/fingerprint confirmation, daemon/socket API, export, promote, gitstate, wip
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

`init` normalizes the root to an absolute clean path, creates `~/.devstrap/config.yaml` with mode `0600` if missing, and does not overwrite an existing config file. `--scan` (`PROD-03`) runs the existing `scan --adopt` path inline after workspace creation so a populated root is adopted on the first command, prints the adopted count, and always prints a short next-steps hint (`devstrap status • devstrap scan --adopt • devstrap sync --hub-file <path>`).

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

`doctor` (`PROD-02`) is a severity-graded health report: each check returns `{name, status: ok|warning|error, detail, remedy}`, rendered as a graded table with a summary line and a non-zero exit code when any check is error (so it can gate CI). Checks cover git/gh/go tools (git required, gh/go optional), state home + permissions, schema version, SQLite `quick_check`/`foreign_key_check`, secrets needing rotation, local age + Ed25519 device-key health, and held repo locks (stale = warning). `--json` emits the check array; `--fix` applies safe remediations (create the missing state home, run pending migrations, clear stale repo locks) and re-runs the checks.

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
devstrap sync --hub-s3 devstrap-hub                  # planned: one bucket, tenants separated by key prefix
```

Current implementation does:

- push local events;
- pull remote events;
- apply namespace events idempotently;
- support `--namespace-only` and `--dry-run`;
- leave filesystem materialization/fetch reconciliation unimplemented.

Planned `EAGER-*` behavior adds:

- reconcile skeletons;
- fetch or blobless-clone repos if policy allows;
- hydrate env/draft blobs;
- never overwrite dirty worktrees.

Options:

```bash
--hub-file <path>     # file-backed test hub (current)
--hub-s3 <bucket>     # planned: Cloudflare R2 / S3 zero-knowledge hub backend
--namespace-only
--fetch               # planned
--hydrate-eager       # planned default: eager blobless clone of the whole tree
--dry-run
```

Current implementation supports the file-backed test hub only. It requires `--hub-file`, pushes all local events, pulls hub events from the beginning, applies namespace events idempotently, supports `--namespace-only` and `--dry-run`, and reports that hydration/fetch reconciliation is not implemented yet.

Shipped (`EAGER-*`/`HUB-*`, audit `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`): `sync` is the materialization entrypoint. A single `devstrap sync` eagerly blobless/partial-clones every mapped repo (`git clone --filter=blob:none`) from its existing remote, hydrates env profiles, extracts draft bundles, and (opt-in via `DEVSTRAP_REBUILD_DEPS`) rebuilds `node_modules`/build artifacts on hydrate rather than syncing them. The hub pull is cursor-based (HLC cursor via `hub_cursors`, low-water-mark safe cursor `SYNC-01`, inclusive boundary `HUB-13`; `410 -> snapshot`), and the command prints a real materialize summary. `materialize` returns non-zero when any project fails (`ErrPartialMaterialize`, `QUAL-03`) while still completing the batch, so CI/cron gates and `&&` chains detect partial failure. Repo content always rides git's own transport and never traverses the hub; only the signed namespace map (event log) and ciphertext blobs do. `--namespace-only` opts out of materialization. The hub is resolved through one selection seam (`hubFromOptions`, `P5-HUB-01`/`ARCH-03`): `--hub-file` (or a `hub: file:<path>` config value) selects the file-backed test backend, and the `Hub` interface is ready for the Cloudflare R2 / S3 zero-knowledge backend (`internal/hub`, whose keying/retry/conditional-put/`ListBlobs`/retention-floor logic is unit-tested; the production `aws-sdk-go-v2` S3 adapter + MinIO integration test are the remaining `P5-HUB-01` step). No FUSE/placeholder/lazy-VFS layer is part of this design — StrapFS stays deferred.

### hub

```bash
devstrap hub gc --hub-file <path> [--dry-run] [--keep N]
```

`hub gc` (`P5-HUB-02`) is the hub-side reclamation counterpart to the per-sync local-cache GC (`gcUnreferencedBlobs`). It prunes superseded `draft_snapshots` rows (keeping the latest `--keep` per project, default 1, so the current snapshot is always retained), then lists every blob on the hub (`Hub.ListBlobs`) and deletes those no current secret binding or draft snapshot references. `--dry-run` prunes nothing and reports what would be deleted. Progress/warnings go to stderr; the summary to stdout.

### open

```bash
devstrap open work/acme/api --cursor
devstrap open work/acme/api --vscode
```

Does:

- hydrate if skeleton;
- validate env/tooling;
- open editor.

Current implementation hydrates if needed, refuses unknown namespace paths, checks that `cursor` or `code` exists, starts the editor without tying it to the CLI context, and releases the child process handle. Env/tooling validation is still future work. Planned (`DRAFT-*`): `open` (and `hydrate`) extend beyond `git_repo` projects to materialize `local_git`/`plain_folder`/draft types from decrypted `age_blob:<sha256>` bundles.

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

Current implementation uses partial clone by default, supports `--full` and `--lfs`, refuses to clone into non-empty non-skeleton directories, stages clones in hidden sibling temp directories, promotes only after clone success plus a second target validation, preserves the original skeleton on clone failure, and updates local materialization/dirty state. Planned (`DRAFT-*`): `hydrate` extends beyond `git_repo` projects to materialize `local_git`/`plain_folder`/draft content from decrypted `age_blob:<sha256>` bundles, while `node_modules`/build artifacts are rebuilt (npm/pnpm/uv install) rather than synced.

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

### clone

```bash
devstrap clone git@github.com:acme/api.git
devstrap clone git@github.com:acme/api.git work/acme/api --open
```

`clone` (`PROD-01`) is the one-shot quick path that collapses onboarding to a single command: it derives a namespace path from the remote (`work/<org>/<repo>`, overridable via a positional arg), runs the existing `add` + eager `materialize` (blobless clone + env hydrate), and optionally `--open`/`--vscode` the result. It reuses `addProject` + `materializeOne` internals — a thin orchestrator, not new core logic.

Options:

```bash
--open                # open in Cursor after materialization
--vscode              # open in VS Code after materialization
--default-branch
--lfs-policy auto|never|agent|always
```

### conflicts

```bash
devstrap conflicts                              # list open conflicts (default)
devstrap conflicts list
devstrap conflicts show <id>
devstrap conflicts resolve <id> --keep-local|--keep-remote|--keep-both
```

`conflicts` (`PROD-06`) is a command group that turns the detect-don't-merge model from a read-only count into an actionable resolution surface. `list` (the default when `conflicts` is run with no subcommand) shows open conflict rows; `show <id>` prints one conflict's details and status; `resolve <id>` accepts exactly one of `--keep-local` (keep the local version, discard the remote variant), `--keep-remote` (keep the remote version, discard the local), or `--keep-both` (dual-copy: the local entry stays and the remote variant is re-added under a sibling path). Resolving marks the row `resolved` (so the `status` open-conflict count converges), records the decision in `resolution_json`, and emits a signed `conflict.resolved` HLC event so every device sees the same outcome. Namespace files are never byte-merged; the dual-copy safe default mirrors the draft-bundle conflict behavior.

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

Current implementation supports `agent run/list/show/pr`. `agent run` creates a fresh upstream worktree, runs an explicit generic command with a sanitized no-secret default environment, applies wrapper-level command and file path policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`), records the run in SQLite, captures a `0600` log, and stores a Git status/diff summary. The file path policy denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs; it is a preflight wrapper policy, not an OS sandbox. `agent pr` refuses stale recorded bases unless `--allow-stale-base` is passed, pushes the agent branch, and creates a PR/MR through the detected forge CLI (`gh`/`glab`/`tea`) when available; unsupported forges get the pushed branch and compare URL instead of a failed hardcoded GitHub path. `--dry-run` reports the planned PR without pushing. Non-generic engines, project-env allowlists, OS-enforced sandboxing, `agent cleanup`, self-hosted forge overrides, and forge-specific `doctor` probes remain future work.

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

## Audit follow-ups (2026-06-27)

### CLI consistency (`CLI-01..04`)
- `--json` is silently ignored by most commands and never applies to error output (`CLI-01`); route all machine-readable output (and errors) through one JSON path.
- `scan --json --quarantine` interleaves human progress into the JSON stream, producing invalid JSON (`CLI-02`); send progress to stderr.
- `run`/`agent run` collapse subprocess exit codes to a generic 1 (`CLI-03`); propagate the child's exit code.
- Exit-code taxonomy is overloaded (`CLI-04`): usage errors and overwrite-conflicts both map to `exitInvalidConfig`, and Cobra arg errors map to 1. Disambiguate.

### Daemon socket API (reserved for M5)
The local Unix-socket API and job model are **design intent, not shipped** (`ARCH2-04`); `exitDaemonUnavailable=3` is reserved but never returned. The planned API still needs peer-credential checks / root rejection, message framing, and version negotiation (`CLI-05`).

### Planned commands (not yet registered)
Referenced by the new workstreams; intentionally absent from the live command tree the drift test checks until implemented (`devstrap conflicts` has since shipped — see the 2026-06-28 implementation notes — and is documented under `status`):

```text
devstrap promote <path> --draft|--git-remote <url>     # plain -> draft -> git (NOVCS-03 / DRAFT-*)
devstrap gitstate capture [--fetch]                    # working-state validation plane (Section 5)
devstrap status --all-devices                          # cross-device git-state view
devstrap wip push|status|fetch|show|apply|drop <proj>  # WIP recovery (Phase 1)
devstrap sync --hub-s3 <bucket>                        # Cloudflare R2 / S3 zero-knowledge hub backend (HUB-*)
```

PR creation becomes forge-agnostic (`gh`/`glab`/`tea`) with a `--forge` override (`FORGE-01`).

## Audit implementation notes (2026-06-28)

- **CLI-02**: `scan --quarantine` progress lines now go to stderr, preserving valid JSON on stdout.
- **CLI-03**: `run` and `agent run` propagate child exit codes as `100+N` (new `childExitBase`).
- **CLI-04**: Added `exitUsage = 10` for bad-flag/missing-flag/arg-count errors; `childExitBase = 100` for child process exit codes.
- **PROD-01**: `deriveDisplayStatus` maps materialization+dirty states to user-facing labels; `status` output uses it.
- **PROD-02/PROD-06**: `devstrap conflicts` is a command group (`list`/`show`/`resolve --keep-local|--keep-remote|--keep-both`) that surfaces and resolves open conflicts; `status` shows the open-conflict count and it converges as rows are resolved.
- **ARCH2-04**: Reserved `exitDaemonUnavailable` code for M5 daemon.

## Cloud-sync CLI (2026-06-28)

The cloud-sync architecture (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`) shapes the sync/materialization commands. None of the following is shipped yet; all are planned and marked as such above:

- **Eager materialization (`EAGER-*`)**: `devstrap sync --hydrate-eager` (planned default) clones the whole `~/Code` tree up front via blobless/partial clone — no FUSE, placeholder, or lazy-VFS layer (StrapFS stays deferred). After sync the full tree is present on disk.
- **Two-plane zero-knowledge hub (`HUB-*`)**: the hub carries only (a) the signed, HLC-ordered namespace map (event log) and (b) content-addressed `age_blob:<sha256>` ciphertext for env and non-git/draft content. Repo content never traverses the hub — it rides git transport from the existing remote. `--hub-s3 <bucket>` selects the Cloudflare R2 / S3 backend behind one pluggable Hub interface; `--hub-file` stays for tests only.
- **Content-type split (`DRAFT-*`)**: env plus non-git/draft folders sync as age-encrypted blobs; `node_modules`/build artifacts are never synced and are rebuilt on hydrate. `hydrate`/`open` extend to `local_git`/`plain_folder`/draft project types; `devstrap promote` walks a folder from plain -> draft -> git (`NOVCS-03`).
- **Conflicts stay detect-don't-merge**: HLC ordering plus tombstones; `devstrap conflicts` (shipped) surfaces them. Files are never byte-merged.
- **Device trust**: revocation re-encrypts affected blobs to the reduced recipient set and flags secrets for rotation; once device enrollment exists, event verification must fail closed (`SECU-03`).
