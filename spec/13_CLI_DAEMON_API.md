---
last_reviewed: 2026-07-03
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
devstrap init [--join] [--workspace-id <id>]
devstrap version
devstrap scan
devstrap status
devstrap db
devstrap sync
devstrap open
devstrap hydrate
devstrap add
devstrap clone
devstrap env
devstrap run
devstrap worktree
devstrap agent
devstrap devices
devstrap conflicts
devstrap doctor
devstrap hub
devstrap materialize
devstrap run-loop
devstrap draft
devstrap completion

Planned:
devstrap ignore
devstrap daemon
devstrap export
devstrap promote
devstrap gitstate
devstrap wip
```

## Initial commands

Current repository status as of `2026-07-01`:

```text
Implemented: devstrap init, version, scan, add, clone, hydrate, open, sync --hub-file, sync (hub: r2://<bucket> production R2/S3 SDK wiring), hub gc, hub login/logout, materialize, draft snapshot create, run-loop, status, doctor, conflicts list/show/resolve, db migrate/status/backup/down, env capture/hydrate/bind/rotate, run, worktree new/status/finalize/list/remove/cleanup/unlock, agent run/list/show/pr, devices enroll/list/approve/revoke/lost/rename/recipient
Planned: env check, OS-enforced agent sandboxing, automatic remote device enrollment/fingerprint confirmation, daemon/socket API, export, promote, gitstate, wip
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
--workspace-name my-workspace
--dry-run
--join                 # join an existing workspace: do not found one; wait to be approved (P6-SEC-02)
--workspace-id ws_...  # adopt the founding device's workspace id; implies --join (P4-SEC-07 pairing)
```

`init` normalizes the root to an absolute clean path, creates `~/.devstrap/config.yaml` with mode `0600` if missing, and does not overwrite an existing config file (re-running with `--join` against a pre-existing founder config warns that the config was not modified). It records `role: founder` (default) or `role: joiner` (`--join`) in the config and, per `P6-SEC-02`, **no longer mints a workspace key** — founding is deferred to the first `devstrap sync` and happens only against an empty hub (see `sync` below). `--workspace-id <id>` (P4-SEC-07 pairing) adopts the founder's `ws_<32 hex>` id instead of minting one so both devices read the same r2/s3 hub prefix; the shape is validated before anything is written, supplying an id implies `--join`, `--dry-run` prints the would-adopted id, and a store already initialized under a different id is **refused** (exit 2) with a remove-the-state-home-and-re-init remedy — there is no post-hoc id rewrite. Bare `--join` without `--workspace-id` warns non-fatally that r2/s3 hubs key events by workspace id (flat file hubs are unaffected). `--join` prints approval-first next steps (copy the founder's Workspace ID from `devstrap status` when it was not supplied, share `devices recipient` / `--signing`, get approved on an existing device, then sync); default init prints the `devstrap status • devstrap scan --adopt • set 'hub: r2://<bucket>' in ~/.devstrap/config.yaml then devstrap sync` hint. `--scan` (`PROD-03`) runs the existing `scan --adopt` path inline after workspace creation so a populated root is adopted on the first command and prints the adopted count. Per `P6-CLI-05` (resolved), both hint forms teach the shipped production path (`hub: r2://<bucket>` in `~/.devstrap/config.yaml`) rather than the file-backed `--hub-file` test hub — see the P6-CLI-05 section below.

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

`doctor` (`PROD-02`) is a severity-graded health report: each check returns `{name, status: ok|warning|error, detail, remedy}`, rendered as a graded table with a summary line and a non-zero exit code when any check is error (so it can gate CI). Checks cover git/gh/go tools (git required, gh/go optional), state home + permissions, schema version, SQLite `quick_check`/`foreign_key_check`, secrets needing rotation, local age + Ed25519 device-key health, and held repo locks (stale = warning). `--json` emits the check array; `--fix` applies safe remediations (create the missing state home, run pending migrations, clear stale repo locks) and re-runs the checks. `--remote` (`P5-PROD-05`) additionally probes the configured sync hub (reachability, pending push, queued deletes, device trust) and always reports a `workspace id` row (a warning row when the id is unreadable) so two devices can be compared directly; `--hub-file` selects the file-backed hub for that probe. For R2/S3 hubs, `--remote` also warns `workspace id match` when the local role is `joiner`, the pull cursor is still `0`, and the raw hub backend reports no events under this device's workspace-id prefix — the signature of a joiner reading its own empty `workspaces/<workspace_id>/...` prefix instead of the founder's populated prefix. The remedy text points operators to confirm the founder's workspace id with `devstrap doctor` on the founding device and re-init the joiner with `devstrap init --join --workspace-id <founder workspace id>` (the adoption flag shipped with the P4-SEC-07 pairing wave — see the `init` section above; this change ships the detection and regression-test side).

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
- `--adopt` writes namespace, git repo, draft project, and device project state rows, and is gated on the scanned root matching the workspace root (`P6-CLI-02`, shipped): `scan <other-dir> --adopt` refuses with `exitUsage` ("--adopt only adopts from the workspace root ..."), because adoption emits signed fleet-wide `project.added` events; the comparison resolves symlinks (a symlink alias of the real root is accepted, and adoption then uses the canonical root spelling) but deliberately does not case-fold — over-refusal is the safe direction; read-only scans of arbitrary directories keep working, and `devstrap add` remains the single-repo path;
- escaping symlinks are hard-excluded (never adopted) and surfaced as conflict rows; dangling/IO symlink errors are advisory warnings only;
- `--quarantine` moves secret-looking files out of the managed tree into a dated `~/.devstrap/quarantine/<YYYYMMDD>/` directory (mode `0600`) instead of leaving them in place.

### status

```bash
devstrap status
devstrap status --json
devstrap status --watch [--interval 2s]
```

`status --watch` re-renders the snapshot on an interval until interrupted.

Current Phase-0 status shows workspace name, workspace ID (`Workspace ID:` row / JSON `workspace_id` — the value a founder copies into `init --join --workspace-id <id>` on a joining device, P4-SEC-07 pairing), root path, project count, local device ID, and adopted project rows. Future daemon-backed status adds:

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
devstrap sync   # hub: r2://devstrap-hub (shipped; one bucket, tenants separated by key prefix)
```

Current implementation pushes local events, pulls hub events from the stored cursor, applies namespace events idempotently, then eagerly materializes the tree (blobless clone, draft-bundle extract, env hydrate) unless `--namespace-only` is set; dirty worktrees are never overwritten. `--dry-run` reports the plan without writing.

Options:

```bash
--hub-file <path>     # file-backed test hub (tests only)
hub: r2://<bucket>    # shipped: Cloudflare R2 / S3 zero-knowledge hub backend (creds via DEVSTRAP_HUB_S3_*)
--namespace-only      # opt out of eager whole-tree materialization (the shipped default)
--fetch               # planned: fetch-only reconciliation mode, distinct from the shipped default
--dry-run
```

The file-backed test hub uses `--hub-file` (or `hub: file:<path>`); the R2/S3 production backend is selected via `hub: r2://<bucket>` (or `s3://`). Credentials resolve most-explicit-first (`P6-HUB-02`): `DEVSTRAP_HUB_S3_ACCESS_KEY_ID`/`DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY` env/config — where either value may be a 1Password `op://` reference resolved via `op read` at sync time — then `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` literals, then the per-workspace OS-keychain slot written by `devstrap hub login` (0600 file fallback under `DEVSTRAP_NO_KEYCHAIN`); `hub_s3_endpoint` and `hub_s3_region` (default `auto`) stay env/config. Plaintext env remains the CI/override fallback; the keychain/op:// path is the recommended custody on developer machines. Both backends push local events past the push cursor, pull hub events from the pull cursor, apply namespace events idempotently, and support `--namespace-only` and `--dry-run`.

Shipped (`EAGER-*`/`HUB-*`, audit `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`): `sync` is the materialization entrypoint. A single `devstrap sync` eagerly blobless/partial-clones every mapped repo (`git clone --filter=blob:none`) from its existing remote, hydrates env profiles, extracts draft bundles, and (opt-in via `DEVSTRAP_REBUILD_DEPS`) rebuilds `node_modules`/build artifacts on hydrate rather than syncing them. The hub pull is cursor-based (HLC cursor via `hub_cursors`, low-water-mark safe cursor `SYNC-01`, inclusive boundary `HUB-13`; `410 -> snapshot`), and the command prints a real materialize summary. `materialize` returns non-zero when any project fails (`ErrPartialMaterialize`, `QUAL-03`) while still completing the batch, so CI/cron gates and `&&` chains detect partial failure. Repo content always rides git's own transport and never traverses the hub; only the signed namespace map (event log) and ciphertext blobs do. `--namespace-only` opts out of materialization. Per `P6-SEC-02`, `sync` now **pulls before it pushes** and runs the push behind a founder/join gate: a founder's first sync to an empty hub mints the workspace key (epoch 1) then pushes; a device that has no key and sees a non-empty hub (a joiner awaiting approval) DEFERS the push and prints `Awaiting workspace key grant: N local event(s) queued …`, leaving its events queued (push cursor unadvanced) until it is approved and ingests the fleet key on a later cycle. `--namespace-only` output reports the deferred count when the push is held. The hub is resolved through one selection seam (`hubFromOptions`, `P5-HUB-01`/`ARCH-03`): `--hub-file` (or a `hub: file:<path>` config value) selects the file-backed test backend, and `hub: r2://<bucket>` (or `s3://`) selects the Cloudflare R2 / S3 zero-knowledge backend — the production `aws-sdk-go-v2` S3 adapter (`internal/hub`, with `NopRetryer` so `R2Hub.Retry` is the single retry layer) is wired in, its keying/retry/conditional-put/`ListBlobs`/retention-floor logic is unit-tested, and the same conformance contract is proven against MinIO via an env-gated integration test. No FUSE/placeholder/lazy-VFS layer is part of this design — StrapFS stays deferred.

### hub

```bash
devstrap hub gc --hub-file <path> [--dry-run] [--keep N] [--grace-window 24h]
```

`hub gc` (`P5-HUB-02`, hardened by `P6-HUB-01`) is the hub-side reclamation counterpart to the per-sync local-cache GC (`gcUnreferencedBlobs`). It first pulls and applies the hub event log (the same pull half `sync` runs, including caching referenced blobs — the cursor advances past those events, so gc is the only chance to fetch them) so the mark set includes every device's latest snapshots, and **refuses to sweep** — non-zero exit, nothing deleted — when its view is incomplete: the pull deferred (awaiting a key grant) or skipped events, the apply quarantined events or held the cursor back, or any quarantine-class conflict is still open (a transiently-held event gets a distinct message, since `conflicts resolve` cannot clear a cursor hold; a skew-quarantined event auto-resolves its conflict once it later applies). It then prunes superseded `draft_snapshots` rows (keeping the latest `--keep` per project, default 1, so the current snapshot is always retained), lists every blob on the hub (`Hub.ListBlobs`, which reports each blob's `LastModified`), and deletes those no current secret binding or draft snapshot references — except blobs younger than `--grace-window` (default 24h), which are kept even when unreferenced because a device pushes its blob before its referencing event. The window **bounds** that race rather than closing it: a device offline past the window is not protected (it re-pushes on recovery since its push cursor never advanced), and a dedup'd re-upload does not refresh `LastModified` (tracked with `P4-HUB-12`). `--dry-run` prunes nothing and reports what would be deleted (it still runs the pull, which is the same converging apply `sync` performs — dry run is not read-only). Run `gc` from one designated device; concurrent sweeps are not coordinated (the S3 conditional-write lock is a `P6-HUB-04`-adjacent follow-up). Progress/warnings go to stderr; the summary to stdout.

### hub login / hub logout

```bash
devstrap hub login [--access-key-id <id>]
devstrap hub logout
```

`hub login` (`P6-HUB-02`) stores the hub S3/R2 credential pair in the OS keychain under the per-workspace account `hub-s3.<workspace_id>` (0600 file fallback `hub-s3-<workspace_id>.json` when the keychain is genuinely unavailable, e.g. `DEVSTRAP_NO_KEYCHAIN=1`; a present-but-failing keychain fails closed). The secret is read from a hidden terminal prompt, or from stdin when piped — never from argv (process listings and shell history). `op://` references are refused here: they belong in `DEVSTRAP_HUB_S3_*` env/config, where they resolve at sync time. The command reports whether the pair landed in the keychain or the file fallback. `hub logout` removes the stored pair from both custody backends. Explicit `DEVSTRAP_HUB_S3_*`/`AWS_*` env values always override the stored pair. Auth failures against the hub surface as `ErrS3Auth` with a remediation hint (`mapS3Error`), not a raw `SignatureDoesNotMatch`.

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
devstrap env rotate work/acme/api
devstrap run work/acme/api -- uv run pytest
```

`env rotate` re-encrypts a project's captured env blobs to the current set of approved-device age recipients (dropping any device that was revoked/marked lost) and clears the `needs_rotation` flag on the affected `secret_bindings` rows once the fresh ciphertext is written, so `doctor`'s rotation warnings converge. Provider-ref bindings (`op://`) hold no local plaintext and are marked rotated without re-encryption.

Current implementation supports `env capture`, `env hydrate`, `env bind`, `env rotate`, and top-level `run`. Capture parses a local env file with a non-interpolating grammar, refuses dangerous names, rejects interpolation-looking values unless `--literal` is passed, encrypts the bundle to the local device age recipient, writes a `0600` age blob under `~/.devstrap/blobs`, stores only `age_blob:<sha256>` references in `secret_bindings`, and appends the captured file path to project `.gitignore` when possible. Hydrate decrypts the local age blob with the local device identity or resolves 1Password provider refs through `op inject`, writes only to an explicit `--write` target, creates the file atomically with mode `0600`, refuses to overwrite unless `--force` is passed, and appends the hydrated target to project `.gitignore` when possible. Bind stores 1Password `op://` provider refs without resolving plaintext. `run` injects encrypted profiles directly into the subprocess environment or delegates provider refs to `op run --env-file <temp-refs-file> -- <command>`.

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
devstrap devices enroll dev_01jz... --name linux-desktop --os linux --arch arm64 --age-recipient age1...
devstrap devices approve dev_01jz...
devstrap devices revoke dev_01jz...
devstrap devices lost dev_01jz...
devstrap devices rename dev_01jz... linux-desktop
devstrap devices recipient                 # print local device's age recipient (for out-of-band enrollment)
devstrap devices recipient --signing       # print local device's Ed25519 signing public key
devstrap devices recipient --workspace-id  # print the workspace id (for init --join --workspace-id on a joining device)
```

Current implementation manually enrolls remote device records with age recipients, lists and renames device records, and updates non-local device trust state to `approved`, `revoked`, or `lost`. `devices recipient` is a read-only helper that prints the local device's age recipient (or Ed25519 signing public key with `--signing`, or the workspace id with `--workspace-id` for `init --join --workspace-id` pairing; `--signing` and `--workspace-id` are mutually exclusive, and the bare default output stays frozen because scripts consume it unadorned) so it can be shared out-of-band for enrollment on another device. Env capture encrypts local bundles to the local recipient plus approved remote recipients. `devices approve` and `enroll --approve` grant every held WCK epoch to the newly-approved device (`P4-SEC-07`); on a keyless **joiner** the approve path grants nothing (it is founder-gated — a joiner never self-mints) but still pins the enrolled device's keys and flips verification fail-closed, which is the documented founder-pinning ceremony a joiner runs BEFORE its first sync (`P4-SEC-04` joiner half; in a multi-device fleet the joiner pins every existing device this way — an unpinned signer's events quarantine and replay once that device is approved); `devices approve` and `enroll --approve` also replay open `verification`-kind `event_verification_failure` conflicts from that device using the stored full event JSON and resolve conflicts whose events now apply (`divergent`-kind rows are never auto-resolved). `devices revoke`/`lost` rotate the WCK to a new epoch (go-forward forward secrecy) before the existing blob re-encryption pass. It refuses to change the current local device trust state so a user cannot revoke the only active local root by accident. Automatic remote enrollment, out-of-band fingerprint confirmation UX, a synced `device.revoked` event path, and bundle re-encryption hooks remain future work.

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
  "workspace": "my-workspace",
  "device": "laptop",
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
10 usage error (partially wired: only hand-mapped sites; Cobra flag/arg/unknown-command errors still exit 1 — see P6-CLI-03)
100+N child process exit code
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
devstrap sync   # hub: r2://<bucket> — shipped R2/S3 zero-knowledge hub backend (HUB-*); the --hub-s3 flag was superseded by the hub: config value
```

PR creation becomes forge-agnostic (`gh`/`glab`/`tea`) with a `--forge` override (`FORGE-01`).

## Audit implementation notes (2026-06-28)

- **CLI-02**: `scan --quarantine` progress lines now go to stderr, preserving valid JSON on stdout.
- **CLI-03**: `run` and `agent run` propagate child exit codes as `100+N` (new `childExitBase`).
- **CLI-04**: Added `exitUsage = 10` and `childExitBase = 100` (child process exit codes). Note (`P6-CLI-03`): `exitUsage` is only wired at hand-mapped sites; Cobra flag-parse, Args-validation, and unknown-command errors still bypass `appError` and exit 1 — the `SetFlagErrorFunc`/Args-validator wiring is not yet in place.
- **PROD-01**: `deriveDisplayStatus` maps materialization+dirty states to user-facing labels; `status` output uses it.
- **PROD-02/PROD-06**: `devstrap conflicts` is a command group (`list`/`show`/`resolve --keep-local|--keep-remote|--keep-both`) that surfaces and resolves open conflicts; `status` shows the open-conflict count and it converges as rows are resolved.
- **ARCH2-04**: Reserved `exitDaemonUnavailable` code for M5 daemon.

## Cloud-sync CLI (2026-06-28)

The cloud-sync architecture (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`) shapes the sync/materialization commands. The eager-clone materialization and R2/S3 hub items below are shipped; the rest remain planned:

- **Eager materialization (`EAGER-*`)**: `devstrap sync` (shipped) clones the whole `~/Code` tree up front via blobless/partial clone as the default behavior (`--namespace-only` opts out) — no FUSE, placeholder, or lazy-VFS layer (StrapFS stays deferred). After a **successful** sync the full tree is present on disk; per-project materialization failures are isolated and leave those entries as `failed`, and the command finishes non-zero (see the `sync`/`materialize` exit codes above), so the "whole tree present" guarantee is scoped to a clean exit.
- **Two-plane zero-knowledge hub (`HUB-*`)**: the hub carries only (a) the signed, HLC-ordered namespace map (event log) and (b) content-addressed `age_blob:<sha256>` ciphertext for env and non-git/draft content. Repo content never traverses the hub — it rides git transport from the existing remote. `hub: r2://<bucket>` (or `s3://`) selects the shipped Cloudflare R2 / S3 backend (`aws-sdk-go-v2` adapter behind `hubFromOptions`) behind one pluggable Hub interface; `--hub-file` stays for tests only. Credentials resolve via env/config (values may be `op://` refs), then `AWS_*` literals, then the `hub login` keychain slot / 0600 file fallback — never the URI (`P6-HUB-02`).
- **Content-type split (`DRAFT-*`)**: env plus non-git/draft folders sync as age-encrypted blobs; `node_modules`/build artifacts are never synced and are rebuilt on hydrate. `hydrate`/`open` extend to `local_git`/`plain_folder`/draft project types; `devstrap promote` walks a folder from plain -> draft -> git (`NOVCS-03`).
- **Conflicts stay detect-don't-merge**: HLC ordering plus tombstones; `devstrap conflicts` (shipped) surfaces them. Files are never byte-merged.
- **Device trust**: revocation re-encrypts affected blobs to the reduced recipient set and flags secrets for rotation; once device enrollment exists, event verification must fail closed (`SECU-03`).

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-CLI-01 — Re-running `init` with a new root splits DB root vs config.yaml

**Problem.** `writeDefaultConfig` early-returns without writing when config.yaml exists (`internal/cli/init.go:182-183`), while `state.EnsureWorkspace` unconditionally updates `root_path` (`internal/state/store.go:473-480`), so `init root2` after `init root1` makes `status` (DB) report root2 while config-driven `scan`/`materialize`/`sync` keep using root1.

**Actionable steps.**
1. Before `EnsureWorkspace`, read the existing workspace root and compare against the *effective resolved* requested root (`DEVSTRAP_ROOT`/`--root`/positional all resolved via viper).
2. On a mismatch, refuse with `exitConflict` unless `--move-root`; when accepted (or by default), rewrite config.yaml atomically (temp + rename, `0600`) instead of early-returning; leave `--dry-run` touching neither.
3. Longer term make the DB workspace row the single source of truth for root; add a testscript asserting `scan` and `status` agree after `init A; init B`.

```go
if oldRoot != "" && oldRoot != effectiveRoot && !moveRoot {
    return appError{code: exitConflict,
        err: fmt.Errorf("workspace already rooted at %s; re-run with --move-root to relocate", oldRoot)}
}
```

### P6-CLI-02 — `scan <dir> --adopt` adopts out-of-tree repos into the shared namespace — **shipped (2026-07-03)**

**Was.** `scan` accepted any positional root and `adoptFindings` emitted signed `project.added` events with no check that the scanned root was the workspace root, so `devstrap scan ~/Downloads --adopt` turned every repo there into a fleet-wide namespace event that other devices eagerly blobless-clone into `~/Code`.

**Shipped fix.** After resolving `rootAbs`, `--adopt` is gated on the scanned root naming the same directory as the workspace root (`sameResolvedDir`: byte-exact after `EvalSymlinks`; no case-folding, so over-refusal stays the safe direction) and refuses otherwise with `exitUsage`; on success adoption proceeds under the canonical root spelling. Read-only scans of arbitrary directories keep working. Pinned by `TestScanAdoptRefusesNonWorkspaceRoot` (refusal + zero projects adopted), `TestScanAdoptExplicitWorkspaceRootSucceeds`, `TestScanAdoptAcceptsSymlinkedWorkspaceRoot`, and `TestScanReadOnlyAllowsNonWorkspaceRoot`. If subtree adoption is wanted later, rebase `finding.Path` against `wsRoot`.

```go
if adopt && rootAbs != wsRoot {
    return appError{code: exitUsage, err: fmt.Errorf(
        "--adopt only adopts from the workspace root %s (scanned %s); scan without --adopt to inspect, or use 'devstrap add' for a single repo", wsRoot, rootAbs)}
}
```

### P6-CLI-03 — Usage errors exit 1, not the documented `exitUsage=10`

**Problem.** `root.go:30` declares `exitUsage = 10` but only two hand-mapped sites use it; Cobra flag-parse, Args-validation, and unknown-command errors bypass `appError` and exit `1`, so the exit-code table (this file, "Exit codes") and the `CLI-04` note that claims 10 covers these are false.

**Actionable steps.**
1. Wire `cmd.SetFlagErrorFunc(func(c, err) error { return appError{code: exitUsage, err: err} })`, wrap positional validators once (`usageArgs(cobra.ExactArgs(1))`), and map unknown-command errors to `exitUsage`.
2. Extend the Exit codes table to include `10 usage error` and `100+N child process exit` (the shipped `childExitBase`).
3. Add a `root_test` asserting `devstrap --frobnicate` exits 10.

```go
cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
    return appError{code: exitUsage, err: err}
})
```

### P6-CLI-04 — `--quiet` only lowers slog verbosity; stdout chatter ignores it

**Problem.** `--quiet` (help: "only print errors") is consumed solely by `logging.Configure` (`internal/cli/root.go:69` → `logging/logging.go:19`); `sync.go:144`, `materialize.go:81`, `init.go:126`, and `run_loop.go:71` print progress/summary lines unconditionally, so `run-loop --once --quiet` from cron still emits "pushed 0, pulled 0; materialized 0/0" every tick.

**Actionable steps.**
1. Add a render-seam helper `progressf` that no-ops when `o.quiet`, and route sync/materialize/init/hub-gc summary and progress lines through it; keep errors and explicitly-requested data (`--json`, `status`/`list`/`show` tables) printing.
2. Zero-cost stopgap: reword the flag help to "suppress log output (command results still print)" so it matches the verbosity-only behavior documented under Logging.

```go
func (o *options) progressf(w io.Writer, format string, a ...any) {
    if o.quiet { return }
    fmt.Fprintf(w, format, a...)
}
```

### P6-CLI-05 — README/init hint steer users to the test-only file hub; shipped `r2://` undocumented — RESOLVED (`fix/p6-cli-05`, 2026-07-03)

**Problem.** README (project-status/roadmap/quickstart) still called the R2 backend "wired but not switched on" and showed only `sync --hub-file`, `init.go` hardcoded the `--hub-file` next-steps hint, and `sync.go`'s dry-run printed an empty target when the hub came from config — even though PR #24 shipped `hub: r2://<bucket>` with `DEVSTRAP_HUB_S3_*` credentials.

**Resolution.**
1. README project-status/features/roadmap now describe the R2/S3 backend as shipped (`hub: r2://<bucket>` + `DEVSTRAP_HUB_S3_*`), the quickstart step 6 shows the config line + credential env vars and links `spec/19`, and the command-reference `sync` row names the config hub with `--hub-file` as the local-test override.
2. The `init` next-steps hint (both the default and `--join` forms) now points at configuring `hub: r2://<bucket>` in `~/.devstrap/config.yaml` then plain `devstrap sync` (`--hub-file <path>` still noted for local tests), and the `sync --dry-run` line prints the resolved hub ID (`file:<path>` / `r2:<ws…>`) instead of the raw `--hub-file` flag.

Explicit non-goal: no `devstrap init --hub <uri>` flag was added — the hub is configured in `config.yaml`, keeping one source of truth.

```yaml
# ~/.devstrap/config.yaml
hub: r2://my-devstrap-bucket
# env: DEVSTRAP_HUB_S3_ACCESS_KEY_ID, DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY, DEVSTRAP_HUB_S3_ENDPOINT
```
