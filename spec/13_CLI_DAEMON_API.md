---
last_reviewed: 2026-07-14
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

## `--json` output conventions (P5-CLI-01)

Every command's terminal output should route through the `Renderer` seam (`internal/cli/render.go`): `opts.render(w, humanFunc, typedValue)` encodes `typedValue` as indented JSON when `--json` is set, otherwise it invokes `humanFunc`. This is the single seam — never an ad hoc `if opts.v.GetBool("json") { ... }` block inside a command's business logic — so `--json` is a uniform contract instead of a flag a minority of commands honor. The seam originally backed only `db backup --full`, `db restore`, and `materialize`; twelve more call sites across eight commands (`agent list`/`agent show`, `conflicts list`/`conflicts show`, `devices list`, `doctor`, `scan`, `worktree unlock`/`worktree status`/`worktree list`, `status`, `service status`) were migrated to it in the same change that added this section (`P5-CLI-01`, part A — see "Migration/compat rule" below). Roughly 25 leaf commands still have no `--json` support at all; wiring those is separate future work (part B) and does not close the `P5-CLI-01` finding. The conventions below are derived from the precedent already established across all fifteen current call sites and the typed values they encode (`state.Device`, `state.ProjectStatus`, `state.Conflict`, `state.AgentRun`, `state.Summary`, `fullBackupResult`, `checkResult`, `repoLockReport`, `worktreeStatusOutput`, `serviceStatusJSON`, `scan.Result`), not invented fresh.

**Field naming.** Every `json:` tag in the codebase is `snake_case` for multi-word fields (`project_id`, `base_ref`, `dirty_state`, `workspace_id`, `remote_key`, `sandbox_backend`, `runner_started_at`, `exec_path_missing`, `warnings`, `secrets`, `entries`, ...). There is no camelCase precedent anywhere in `internal/state`, `internal/scan`, or `internal/cli`. New JSON-emitting types must use `snake_case` tags.

**Named vs. anonymous inline result types.** Existing call sites follow one of three shapes, and new commands should pick the same way:
- If the JSON payload *is* an existing exported type from its owning package (e.g. a list of `state.Device`, `state.ProjectStatus`, `state.Worktree`, or `state.Conflict` rows), encode that type directly — bare array or bare object — rather than introducing a synthetic wrapper. Don't add a wrapper struct purely to "give it a name."
- If the payload assembles fields from multiple sources, needs derived/computed fields not on any store type, or is rendered by more than one code path, define a named struct at file scope in `internal/cli` (the `repoLockReport`, `worktreeStatusOutput`, `checkResult`, `serviceStatusJSON`, `fullBackupResult` pattern). Prefer this default for anything beyond a one-off.
- Reserve an anonymous inline struct literal (the `materialize.go` pattern) for a single trivial summary that exists only to be passed once to `opts.render` and is never referenced elsewhere. When a payload needs to combine an existing typed value with one extra field, anonymous struct embedding is acceptable (`agent show`'s `struct { state.AgentRun; Violations []state.SandboxViolation \`json:"violations"\` }`).

**Optional fields: value + `omitempty`, not pointers.** No type in the codebase uses a pointer field for an optional JSON value (e.g. `*string`); every optional field is a plain value type tagged `,omitempty` (`PID int \`json:"pid,omitempty"\``, `Hostname string \`json:"hostname,omitempty"\``, `ExecPath string \`json:"exec_path,omitempty"\``). Follow this default: a pointer is only justified if the field's zero value (`0`, `""`, `false`) is itself a meaningful, distinct-from-absent observation that must round-trip — no shipped command currently needs that, so don't introduce a pointer field without a concrete case for it.

**Warnings / partial-failure shape.** `P7-CLI-01` set the standard: a result struct carries a `Warnings []string \`json:"warnings,omitempty"\`` field (see `fullBackupResult`, `scan.Result`, `restoreResult`). Non-fatal warnings are appended to that slice instead of being `Fprintf`'d to stdout ahead of the JSON payload. The human-render callback passed to `opts.render` prints each warning line (`"warning: %s\n"`) before its summary; the JSON branch carries the same warnings inside the payload. This keeps `--json` stdout a single parseable document in both the success and partial-warning cases. New commands that can produce non-fatal warnings should follow this shape rather than writing warning text directly to stdout.

**Migration/compat rule for this PR.** The twelve call sites across eight commands (`agent list`/`agent show`, `conflicts list`/`conflicts show`, `devices list`, `doctor`, `scan`, `worktree unlock`/`worktree status`/`worktree list`, `status`, `service status`) previously emitted `--json` output through an older inline `json.NewEncoder(stdout)` pattern rather than the `Renderer` seam. This change migrated all twelve to `opts.render` while **preserving each command's exact prior JSON output shape byte-for-byte** — only the internal call moved from a raw `json.NewEncoder`/`enc.Encode` block to `opts.render`; no field was renamed, added, removed, reordered, or reshaped as part of that move (see `internal/cli/render_migration_test.go`, which pins the shape for the ten call sites that had no prior `--json` test coverage). The conventions in this section govern *new* commands and any deliberate future reshaping of these commands' output — they do not retroactively authorize a breaking change to `--json` consumers of these commands going forward.

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
devstrap service
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
Implemented: devstrap init, version, scan, add, clone, hydrate, open, sync --hub-file, sync (hub: git+ssh://…/git@host:path.git zero-infrastructure git carrier — the documented default — and hub: r2://<bucket> production R2/S3 SDK wiring), hub init, hub compact, hub gc, hub login, hub logout, hub migrate-events, keys rotate, materialize, draft snapshot create, run-loop, service install, service uninstall, service status, status, doctor, conflicts list, conflicts show, conflicts resolve, db migrate, db status, db backup, db backup --full, db restore, db down, env capture, env hydrate, env bind, env rotate, run, worktree new, worktree status, worktree finalize, worktree list, worktree remove, worktree cleanup, worktree unlock, agent run, agent list, agent show, agent pr, devices enroll, devices list, devices approve, devices revoke, devices lost, devices rename, devices recipient, devices pairing-code
Planned: env check, automatic remote device enrollment/fingerprint confirmation, daemon/socket API, export, promote, gitstate, wip
```

`TestEveryCommandIsDocumented` path-anchors this inventory against the live Cobra tree: every visible command path must appear as a contiguous substring here and in `spec/00_START_HERE.md`.

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
--code devstrap-pair1:...  # adopt the founding device's one-paste pairing code; implies --join
--fingerprint XXXX-...     # with --code: founder fingerprint confirmed out-of-band
--move-root            # explicitly relocate an already-initialized workspace root
```

`init` normalizes the effective root (`DEVSTRAP_ROOT`, `--root`, or positional `[root]`) to an absolute clean path, creates `~/.devstrap/config.yaml` with mode `0600` if missing, and does not overwrite an existing config file on same-root re-runs (re-running with `--join` against a pre-existing founder config warns that the config was not modified). If the store is already initialized under a different root, `init` refuses before `EnsureWorkspace` with `exitConflict`, names both the existing and requested roots, and points to `--move-root`; with `--move-root` it proceeds and rewrites `config.yaml` through a same-directory temp file plus rename (`0600`) so the config root and DB root agree afterward. It records `role: founder` (default) or `role: joiner` (`--join`) in the config and, per `P6-SEC-02`, **no longer mints a workspace key** — founding is deferred to the first `devstrap sync` and happens only against an empty hub (see `sync` below). `--workspace-id <id>` (P4-SEC-07 pairing) adopts the founder's `ws_<32 hex>` id instead of minting one so both devices read the same r2/s3 hub prefix; the shape is validated before anything is written, supplying an id implies `--join`, `--dry-run` prints the would-adopted id, and a store already initialized under a different id is **refused** (exit 2) with a remove-the-state-home-and-re-init remedy — there is no post-hoc id rewrite. `--code <devstrap-pair1:...>` is mutually exclusive with `--workspace-id`, decodes before filesystem writes, implies `--join`, adopts the carried workspace id, and enrolls the carried founder row after initialization: with `--fingerprint <fp>` it approves the founder only if the normalized value matches the fingerprint derived from the carried keys (mismatch fails before any write); with a TTY it prints the derived fingerprint and requires `yes`; with no TTY and no flag it keeps init scriptable by enrolling the founder as `pending` and printing the exact `devices approve <dev-id> --fingerprint <derived-fp>` follow-up. Bare `--join` without `--workspace-id`/`--code` warns non-fatally that remote hubs (git carrier, r2/s3) key events by workspace id (flat file hubs are unaffected). `--join --code` prints the two-paste ceremony next steps first (`devices pairing-code` on each side, then `devices enroll --code ... --approve --fingerprint ...`); manual flags remain the documented fallback when `--code` was not passed. Default init prints the `Next: devstrap status • devstrap scan --adopt • set 'hub: git@github.com:<you>/<hub-repo>.git' (any private repo; or r2://<bucket>) in ~/.devstrap/config.yaml then devstrap sync` hint. `--scan` (`PROD-03`) runs the existing `scan --adopt` path inline after workspace creation so a populated root is adopted on the first command and prints the adopted count. Per `P6-CLI-05` (resolved) and the `AD-1` quickstart-default swap (2026-07-04), both hint forms teach the zero-infrastructure git carrier first (`hub: git@github.com:<you>/<hub-repo>.git`, any private repo) with `r2://<bucket>` as the scale-up alternative, rather than the file-backed `--hub-file` test hub — see the P6-CLI-05 section below.

### db

```bash
devstrap db migrate
devstrap db status
devstrap db backup ~/.devstrap/backups/state-20260624.db
devstrap db backup --full ~/.devstrap/backups/workspace-20260704.tar
devstrap db restore ~/.devstrap/backups/workspace-20260704.tar
devstrap db restore --recover
devstrap db down
```

Rules:

- `migrate` applies all embedded Goose migrations;
- `status` prints schema version (currently **26** after `00025_blob_ref_indexes.sql` added the `NOCASE` blob-reference enumeration indexes and `00026_blob_ref_composite_indexes.sql` added the `BINARY` composite indexes for the exact-match revoke/rewrap lookups, `P7-DATA-06`), SQLite `quick_check`, and SQLite `foreign_key_check`;
- `backup` uses `VACUUM INTO`, not file copy;
- `backup --full` (`P6-DATA-04`, hardened by `P7-DATA-03/04`) writes a single mode-`0600` tar. The accepted `VACUUM INTO` snapshot is opened read-only and supplies blob refs, custody, device/workspace identity, and held WCK epochs; the existing bounded retry handles concurrent blob rotation/GC. Required key bytes resolve from the live custody backend using that snapshot inventory. Missing/unreadable/content-address-mismatched blobs or required keys are fatal and the partial archive is removed. A final `manifest.json` v1 records size/SHA-256 and required-set membership for every earlier entry. Under `--json` (`P7-CLI-01`), non-fatal warnings stay in the payload's `warnings` array;
- `restore [--force] [--allow-legacy] <archive>` (`P7-DATA-04/05`) stages extraction, rejects unsafe/non-regular entries, verifies manifest format/version, every entry hash/size, required-set membership, and absence of extra files, then read-only validates SQLite and cross-checks content-addressed blobs, current-device keys, and held-WCK files before any live swap. Pre-manifest archives require `--allow-legacy`, which warns and skips only manifest integrity. Promotion uses an atomically rewritten `.restore-journal.json`, one shared aside suffix, and durable per-target `done` markers: recovery rolls forward only when all targets are done and otherwise rolls back in reverse. `restore --recover` takes no archive; plain restore auto-recovers first. State opens fail closed and `doctor` reports the recovery remedy while a journal exists. A keychain-custody restore prints file-custody reconciliation guidance, and every `--json` path emits one result document;
- full backup, restore, `db down`, and each run-loop tick serialize through the state-home maintenance lock;
- state DB and backups are mode `0600`.

`doctor` (`PROD-02`) is a severity-graded health report: each check returns `{name, status: ok|warning|error, detail, remedy}`, rendered as a graded table with a summary line and a non-zero exit code when any check is error (so it can gate CI). Checks cover git/gh/go tools (git required, gh/go optional), state home + permissions, schema version, SQLite `quick_check`/`foreign_key_check`, dangling blob refs (`P6-DATA-04`: every `age_blob:` ref the DB holds must have its ciphertext present under `blobs/`; a missing one is an error whose remedy points at a `db backup --full` restore), secrets needing rotation, the recorded key-custody backend (`P6-XP-04`: a `key custody` row reporting `keychain` or `file`, warning when the recorded backend is currently unreachable, is being overridden by `DEVSTRAP_NO_KEYCHAIN`, or has not been recorded yet on a pre-`P6-XP-04` store), local age + Ed25519 device-key health, workspace keys awaiting grants (`P6-SEC-03`: each open `key_grant_waits` row with epoch/kid/first-seen and the re-approve remedy), the active workspace key's age against `keys.rotate_max_age` (`P4-SEC-07`: ok at epoch 0, warn past the deadline with the `keys rotate` remedy), an owed post-revoke workspace-key rotation (issue #134: warn while the machine-local `wck_rotation_pending` marker is set — a `devices revoke`/`lost` could not rotate the epoch, so events stay readable by the revoked device; remedy names sync's automatic retry and `keys rotate`; silent when nothing is owed), agent-run dead-PID reconciliation (`P6-GIT-06`: `agent run sweep` reports rows flipped from `running` to `interrupted` and the remaining running count), and held repo locks (stale = warning). `--json` emits the check array; `--fix` applies safe remediations (create the missing state home, run pending migrations, clear stale repo locks) and re-runs the checks. `--remote` (`P5-PROD-05`) additionally probes the configured sync hub (reachability, pending push, queued deletes, device trust) and always reports a `workspace id` row (a warning row when the id is unreadable) so two devices can be compared directly; `--hub-file` selects the file-backed hub for that probe. For workspace-id-keyed remote hubs (R2/S3 and the git carrier), `--remote` also warns `workspace id match` when the local role is `joiner`, the pull cursor is still `0`, and the raw hub backend reports no events under this device's workspace-id prefix — the signature of a joiner reading its own empty `workspaces/<workspace_id>/...` prefix instead of the founder's populated prefix. The remedy text points operators to confirm the founder's workspace id with `devstrap doctor` on the founding device and re-init the joiner with `devstrap init --join --workspace-id <founder workspace id>` (the adoption flag shipped with the P4-SEC-07 pairing wave — see the `init` section above; this change ships the detection and regression-test side).

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

- prunes generated folders before descent — the prune matcher is compiled per walk from the workspace root's `.devstrapignore` plus the built-in defaults (`P6-XP-06`; compile failures warn and fall back to defaults), so a root-level negation like `!bin/` re-includes a default-pruned directory; the pruned-dir count is surfaced as one informational `Pruned N directories …` line through the quiet-aware `progressf` seam (deliberately not a warning: `run-loop` echoes scan warnings every tick);
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

### keys

```bash
devstrap keys rotate     # mint epoch+1, grant to all approved devices; sync publishes
```

`keys rotate` (P4-SEC-07 periodic rotation) calls `Keyring.Rotate` directly: it mints a fresh WCK at epoch+1, grants it to every approved device the local registry knows (one `device.key.granted` event per recipient), and queues the grants for the next `sync`. It is deliberately NOT the revoke path — no secret-rotation flags, no blob re-encryption, no queued hub deletes — because a periodic rotation has no excluded device; it bounds FORWARD exposure only (see `15_SECURITY_THREAT_MODEL.md`). It refuses at epoch 0 (the key is founded on the first sync). `sync` performs the same rotation automatically when the active epoch is older than `keys.rotate_max_age` (default **2160h** = 90 days; `0` disables; malformed values warn and fall back to the default), checked AFTER the pull (a freshly ingested grant resets the local age, so fleets don't rotation-storm — whichever device syncs first past the deadline rotates and everyone else stands down) and BEFORE the push (the grants and any events sealed under the new epoch ride the same cycle); `sync --key-max-age <duration>` overrides the config for one run. Any device may rotate; concurrent mints at the same epoch coexist under `(epoch, kid)` keying. Known residual (spec/07/15): the rotator grants only to approved devices it knows locally, so a fleet device unknown to it rides the `P6-SEC-03` grace→quarantine→replay path until re-approved.

### sync

```bash
devstrap sync --hub-file ~/.devstrap/test-hub/events.json
devstrap sync   # hub: git@github.com:you/devstrap-hub.git (zero-infra git carrier — the documented default)
                # or hub: r2://devstrap-hub (R2/S3 scale-up; one bucket, tenants separated by key prefix)
```

Current implementation pushes local events past the Seq push watermark, pulls hub events from the stored per-origin-device Seq cursors (`P5-SYNC-01`), applies namespace events idempotently, then eagerly materializes the tree (blobless clone, draft-bundle extract, env hydrate) unless `--namespace-only` is set; dirty worktrees are never overwritten. `--dry-run` reports the plan without writing.

Options:

```bash
--hub-file <path>     # file-backed test hub (tests only)
hub: git+ssh://…      # shipped (AD-1): zero-infra private-git-repo carrier — the documented quickstart
                      # default. Also git+https://…, git+file:///path (tests), scp-like git@host:path.git;
                      # optional ?branch= (default main); auth = the user's existing ssh agent / git
                      # credential helpers, non-interactive (load the key: ssh-add ~/.ssh/<key>);
                      # embedded URI passwords are rejected
hub: r2://<bucket>    # shipped: Cloudflare R2 / S3 zero-knowledge hub backend — the scale-up path
                      # (creds via DEVSTRAP_HUB_S3_*)
--namespace-only      # opt out of eager whole-tree materialization (the shipped default)
--fetch               # planned: fetch-only reconciliation mode, distinct from the shipped default
--dry-run
--key-max-age <dur>   # override keys.rotate_max_age for this run (0 disables auto-rotation)
sync.key_grant_grace  # config: how long a not-yet-granted workspace key defers the pull tail before its
                      # events quarantine recoverably (P6-SEC-03). Default 72h; 0 = quarantine immediately.
                      # Parsed strictly: a malformed value warns and falls back to the default (never 0).
keys.rotate_max_age   # config: age-triggered periodic WCK rotation deadline (P4-SEC-07). Default 2160h
                      # (90d); 0 disables. Strictly parsed like sync.key_grant_grace.
```

The file-backed test hub uses `--hub-file` (or `hub: file:<path>`); the zero-infrastructure git carrier — the documented quickstart default since the `AD-1` swap (2026-07-04) — is selected via `hub: git+ssh://…` / `git+https://…` / `git+file://…` / scp-like `git@host:path.git` with optional `?branch=` (`GitCarrierHub` in `internal/hub`, local clone cache under `~/.devstrap/hub-git/`, hub id `git:<workspace_id>`; the carrier design is canonical in `03_SYSTEM_ARCHITECTURE.md`); the local-folder / cloud-drive-folder carrier (`AD-1` final slice, 2026-07-05) is selected via `hub: folder:<abs-path>` (a Dropbox/iCloud/Drive folder or network mount; the path must be absolute and carries no `?`-parameters — `FolderHub` in `internal/hub`, hub id `folder:<workspace_id>`, per-device lock + observation cache under `~/.devstrap/hub-folder/<hash>/` while only ciphertext objects live in the shared folder; `hub init` remains git-only, so the folder scheme is set in `config.yaml`/`DEVSTRAP_HUB` directly); the R2/S3 scale-up backend is selected via `hub: r2://<bucket>` (or `s3://`). Git-carrier auth is the user's existing git credentials, running non-interactively (a missing/denied key fails fast with the auth exit class and git's own stderr instead of prompting, followed by a second stderr line `hint: git authentication failed — check ssh key / repo access (load your key: ssh-add ~/.ssh/<key>)` — the single error sink prints it for every auth-class failure, including ones wrapped in an app exit code, shipped 2026-07-05); R2/S3 credentials resolve most-explicit-first (`P6-HUB-02`): `DEVSTRAP_HUB_S3_ACCESS_KEY_ID`/`DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY` env/config — where either value may be a 1Password `op://` reference resolved via `op read` at sync time — then `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` literals, then the per-workspace OS-keychain slot written by `devstrap hub login` (0600 file fallback under `DEVSTRAP_NO_KEYCHAIN`); `hub_s3_endpoint` and `hub_s3_region` (default `auto`) stay env/config. Plaintext env remains the CI/override fallback; the keychain/op:// path is the recommended custody on developer machines. Both backends push local events past the push cursor, pull hub events from the pull cursor, apply namespace events idempotently, and support `--namespace-only` and `--dry-run`.

Shipped (`EAGER-*`/`HUB-*`, audit `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`): `sync` is the materialization entrypoint. A single `devstrap sync` eagerly blobless/partial-clones every mapped repo (`git clone --filter=blob:none`) from its existing remote, hydrates env profiles, extracts draft bundles, and (opt-in via `DEVSTRAP_REBUILD_DEPS`) rebuilds `node_modules`/build artifacts rather than syncing them — the rebuild runs BEFORE env hydrate (`P6-GIT-03`: lifecycle scripts are arbitrary repo-controlled code and must never execute with a freshly decrypted `.env` on disk) and tees its output to a `0600` log under `~/.devstrap/logs/rebuilds/<project>.log`, named in the failure message. The hub pull is cursor-based (per-origin-device Seq cursors via `hub_device_cursors`, per-device contiguous-run safe cursor `SYNC-01`/`P5-SYNC-01` — an exact boundary, the `HUB-13` overlap is retired; per-device retention floor `410 -> snapshot`), and the command prints a real materialize summary. `materialize` returns non-zero when any project fails (`ErrPartialMaterialize`, `QUAL-03`) while still completing the batch, so CI/cron gates and `&&` chains detect partial failure. Repo content always rides git's own transport and never traverses the hub; only the signed namespace map (event log) and ciphertext blobs do. `--namespace-only` opts out of materialization. Per `P6-SEC-02`, `sync` now **pulls before it pushes** and runs the push behind a founder/join gate: a founder's first sync to an empty hub mints the workspace key (epoch 1) then pushes; a device that has no key and sees a non-empty hub (a joiner awaiting approval) DEFERS the push and prints `Awaiting workspace key grant: N local event(s) queued …`, leaving its events queued (push cursor unadvanced) until it is approved and ingests the fleet key on a later cycle. `--namespace-only` output reports the deferred count when the push is held. Per `P6-SEC-03`, the pull-side wait for a missing workspace key is **grace-bounded**: within `sync.key_grant_grace` the pull defers (truncates) at the first event it cannot decrypt, and past it those events are quarantined as recoverable `undecryptable` conflicts so the cursor advances and sync is never wedged forever; the quarantined carriers replay automatically once the grant arrives (see `07_NAMESPACE_AND_SYNC_MODEL.md`). Per `P6-SYNC-02`, the same window grace-bounds an **unknown envelope version** (a newer client's events defer per origin device until this binary upgrades, then quarantine), malformed envelopes forward straight to that quarantine, and retired-v1/anti-downgrade drops leave durable `sync_skipped_events` records — surfaced as `status` "Skipped hub events: N" and a graded `doctor` "skipped hub events" check with per-reason remedies, and a `hub gc` sweep refusal while any record is open. Records clear automatically when their event finally applies; there is deliberately no `sync --replay-skipped` flag (held classes retry at the per-device seq gap; quarantined classes ride the existing replay). After every pull apply, `sync` also replays open **pending-project pointer** quarantines (`env_pending_project`/`draft_pending_project` — a verified `env.profile.updated` or `draft.snapshot.created` that arrived before its project, issue #133) via `ReplayPendingProjectConflicts`, so a pointer recovered in the same cycle its `project.added` applies; `devices approve` runs the same replay after re-applying a newly-approved device's quarantined events. Per `P4-SYNC-06`, after a **fully-clean** cycle (push not deferred; no truncated/skipped/undecryptable pull; no quarantined/cursor-held apply; no open `sync_skipped_events`) `sync` best-effort publishes a signed **ack marker** to `meta/acks/<device_id>.json` recording the consumed transport cursor, push watermark, and current HLC — the tombstone-safety clock a compactor mins over. Writing never fails the sync (a `PutAck` error only delays a compactor's tombstone GC), and an unchanged cycle skips the redundant write. The hub is resolved through one selection seam (`hubFromOptions`, `P5-HUB-01`/`ARCH-03`): `--hub-file` (or a `hub: file:<path>` config value) selects the file-backed test backend, and `hub: r2://<bucket>` (or `s3://`) selects the Cloudflare R2 / S3 zero-knowledge backend — the production `aws-sdk-go-v2` S3 adapter (`internal/hub`, with `NopRetryer` so `R2Hub.Retry` is the single retry layer) is wired in, its keying/retry/conditional-put/`ListBlobs`/retention-floor logic is unit-tested, and the same conformance contract is proven against MinIO via an env-gated integration test. No FUSE/placeholder/lazy-VFS layer is part of this design — StrapFS stays deferred. Per `P4-SYNC-02`, when a device's pull cursor has fallen below the hub's retention floor the pull returns `ErrSnapshotRequired`; `sync` no longer dead-ends on it — it prints `Recovering from hub snapshot (retention floor passed our cursor)…` and runs one full-state snapshot exchange (get + fail-closed-verify the signed retention manifest → pull the tail so an in-batch grant is ingested → fetch + sha-check + unseal → import → advance cursors → pull imported draft blobs), then re-runs the incremental pull, which now succeeds. A **trust refusal** (the snapshot producer is not a locally approved device, a bad signature, an object sha256 mismatch, or an AEAD failure on every held key) exits `invalid-config` (2) with a pin/enroll remedy, distinct from a hub/fetch failure, which exits `network` (8). A **keyless joiner** (the snapshot is sealed under an epoch this device does not hold yet) prints the awaiting-grant defer and exits 0 — the next sync retries once the grant lands, importing nothing in the meantime. The hub is resolved through one selection seam (`hubFromOptions`, `P5-HUB-01`/`ARCH-03`): `--hub-file` (or a `hub: file:<path>` config value) selects the file-backed test backend, and `hub: r2://<bucket>` (or `s3://`) selects the Cloudflare R2 / S3 zero-knowledge backend — the production `aws-sdk-go-v2` S3 adapter (`internal/hub`, with `NopRetryer` so `R2Hub.Retry` is the single retry layer) is wired in, its keying/retry/conditional-put/`ListBlobs`/retention-floor logic is unit-tested, and the same conformance contract is proven against MinIO via an env-gated integration test. No FUSE/placeholder/lazy-VFS layer is part of this design — StrapFS stays deferred.

### hub

```bash
devstrap hub init <git-url> [--force] [--no-probe]
```

`hub init` (`AD-1` bootstrap convenience) writes a git-carrier hub URI into the initialized home's `config.yaml`. It refuses when the resolved home has no `config.yaml` and points to `devstrap init` first. Accepted values are the git-carrier forms parsed by the shared `parseGitCarrierURI` helper: `git+ssh://`, `git+https://`, `git+file://`, scp-like `git@host:path.git`, and optional `?branch=`. Embedded credentials are rejected without echoing the URI. Non-git hub URIs such as `r2://...` are intentionally out of scope for this convenience command; set `hub:` manually for R2/S3.

The config rewrite is surgical: it replaces the existing top-level `hub:` line or appends one, preserving every other line/comment, and writes through the same `0600` temp+rename path as `init`. If a different top-level `hub:` value already exists, the command refuses with `exit-conflict` and names both values (the existing value is userinfo-stripped before echoing) unless `--force` is passed; the same value is a no-op success that skips the probe. After a write, unless `--no-probe` is set, it runs a best-effort non-interactive `git ls-remote` through the shared sanitized git runner. Probe failure is only a warning with the ssh-key/repo-access (`ssh-add`) remedy; the config write remains committed. An empty carrier repo is valid because the first `sync` seeds the marker and event objects. Output ends with the founder next step (`devstrap sync`) and the joiner-ceremony pointer.

```bash
devstrap hub gc --hub-file <path> [--dry-run] [--keep N] [--grace-window 24h]
```

`hub gc` (`P5-HUB-02`, hardened by `P6-HUB-01`) is the hub-side reclamation counterpart to the per-sync local-cache GC (`gcUnreferencedBlobs`). It first pulls and applies the hub event log (the same pull half `sync` runs, including caching referenced blobs — the cursor advances past those events, so gc is the only chance to fetch them) so the mark set includes every device's latest snapshots, and **refuses to sweep** — non-zero exit, nothing deleted — when its view is incomplete: the pull deferred (awaiting a key grant) or skipped events, the apply quarantined events or held the cursor back, an unconsumed workspace key grant is awaited, or any quarantine-class conflict is still open (a transiently-held event gets a distinct message, since `conflicts resolve` cannot clear a cursor hold; a skew-quarantined event auto-resolves its conflict once it later applies). This completeness gate is the shared `refuseIfIncompleteView` helper, used identically by `hub compact` below. It then prunes superseded `draft_snapshots` rows (keeping the latest `--keep` per project, default 1, so the current snapshot is always retained), lists every blob on the hub (`Hub.ListBlobs`, which reports each blob's `LastModified`), and deletes those no current secret binding or draft snapshot references — except blobs younger than `--grace-window` (default 24h), which are kept even when unreferenced because a device pushes its blob before its referencing event. The window **bounds** that race rather than closing it: a device offline past the window is not protected (it re-pushes on recovery since its push cursor never advanced); a dedup'd re-upload now **refreshes** `LastModified` (`P4-HUB-12`, shipped — R2 re-puts the same bytes unconditionally, FileHub bumps the mtime) AND the sweep re-stats (`Hub.StatBlob`) each candidate immediately before deleting it, so a blob re-referenced by a `>window`-late recovery sync survives even when the refresh lands after gc's `ListBlobs` snapshot. `--dry-run` prunes nothing and reports what would be deleted (it still runs the pull, which is the same converging apply `sync` performs — dry run is not read-only; it also takes no sweep lock, since it deletes nothing). Concurrent destructive hub passes (`gc`/`compact`/`migrate-events`) on cooperating clients are serialized by an **advisory sweep lock** (`meta/sweep.lock`, `P4-HUB-12`): a real (non-dry) run acquires it with a create-only conditional PUT after the completeness gate and before any deletion, refusing with `exit-conflict` (4) and the holder id if another sweep is live, breaking and re-acquiring it once if the lock is older than its 1h TTL (judged by the object's backend mtime, never its self-reported time), and releasing it on every exit path. The lock is advisory — it protects cooperating clients, not a hostile writer (`spec/15`). `gc`'s pre-pull recovers from a hub snapshot exactly like `sync` when its cursor has fallen below the retention floor (`P4-SYNC-02`), so a designated sweeper that fell behind a compaction bootstraps and continues; a keyless device that cannot unseal the snapshot refuses to sweep rather than acting on a partial view. Progress/warnings go to stderr; the summary to stdout.

```bash
devstrap hub compact --hub-file <path> [--dry-run] [--keep-snapshots N] [--min-events N] [--gc-tombstones=false]
```

`hub compact` (`P4-HUB-11`) publishes a full-state snapshot, advances the hub's per-device retention floors, and deletes the now-cold events below them, so the event log does not grow without bound and a fresh joiner never needs a retired key epoch (the snapshot is sealed under the current-epoch WCK). It runs the SAME completeness gate as `hub gc` (`refuseIfIncompleteView`, plus a push of local pending events first so `floors[self]` can cover local history), then, in a load-bearing confirm-before-delete order: computes the per-device floors (each remote device's floor is `pullCursor+1`, the local device's is `pushWatermark+1`; a device that has consumed nothing gets no floor); reconciles them against the current signed retention manifest — refusing to lower any device's floor (floors are monotonic) or to build on a manifest it cannot fail-closed-verify, and carrying forward the floor of any device present in the old manifest but absent from ours; builds and seals the snapshot document (namespace map with source-event coordinates, surviving tombstones, per-device hash-chain anchors) under the current-epoch WCK; `PutSnapshotObject` (content-addressed by sha256); signs and CAS-writes the retention manifest (`If-Match` on the read etag, one re-read-and-retry on a lost race, error on a second); reads the manifest back and confirms it names our snapshot; and only THEN deletes the cold events (`CompactEventsBelow`). A crash anywhere leaves a superset of the committed state (safe). It finally prunes superseded snapshot objects, keeping the manifest-referenced one plus the newest `--keep-snapshots - 1` others (default 2). `--min-events N` refuses (before any hub write) unless at least N events would be deleted (0 = always compact). `--dry-run` performs the converging pre-sync and prints the floors, the event-delete estimate, and the snapshot document size, writing NOTHING to the hub (it skips the local-event push, so its `floors[self]` reflects the current pre-push watermark). A **keyless** device cannot compact (nothing to seal under) and refuses. `--gc-tombstones` (default on; `--gc-tombstones=false` retains tombstones) garbage-collects deleted namespace entries every device has acked (`P4-SYNC-06`): after the completeness gate and before building the snapshot, compact lists the signed sync acks, verifies each against the local registry, and requires a verified ack from **every** approved non-local device — else it SKIPS GC and prints a naming hint. The safe floor is the minimum HLC watermark across the local device's live clock and those acks; tombstones below it are purged and (because GC runs before the snapshot is built) excluded from the published snapshot. Acks from revoked/lost/pending/unknown devices or with a bad signature are ignored, so they can neither pin nor lower the floor. After the confirm read-back and event deletion, compact also reclaims the entire event-log prefix and deletes the stale ack of any revoked/lost device whose stream the committed floors fully cover (its floor and local cursor are retained). `--dry-run` reports the tombstone-GC decision (the safe floor and how many rows would be purged, or the reason it is skipped) without mutating. Like `gc`, a real (non-dry) `compact` acquires the advisory sweep lock (`meta/sweep.lock`, `P4-HUB-12`) after its converging pre-sync and before the destructive seal → publish → CAS → delete sequence, so concurrent destructive passes (`gc`/`compact`/`migrate-events`) on cooperating clients cannot interleave; it refuses with `exit-conflict` (4) and the holder id when another sweep is live, and releases the lock on every exit path (a `--dry-run` writes nothing and takes no lock). A device that falls below a published floor recovers automatically on its next `sync` by importing the snapshot (`P4-SYNC-02`). Progress/warnings go to stderr; the summary to stdout.

### hub migrate-events

```bash
devstrap hub migrate-events --hub-file <path> [--dry-run]
```

`hub migrate-events` (`P4-HUB-12`) re-keys the retired HLC-keyed legacy event layout (`workspaces/<ws>/events/<hlc>/<device>/<seq>/<id>.json`) into the current per-device seq layout (`workspaces/<ws>/eventlog/<device>/<seq>_<id>.json`) and deletes the migrated legacy objects, so the dual-read `Pull` freezes to a cheap empty-prefix list. For each legacy object it re-puts the bytes to the new key with a create-only conditional PUT (a 412 is an already-migrated object, not an error), **verifies by read-back** that the new key serves equal bytes, and only THEN deletes the legacy object — so a mid-migration crash or a wrong-bytes backend never loses an event. It is idempotent and resumable (the dual-read keeps unmigrated objects live; a re-run of a fully migrated hub reports 0 to migrate) and **fails open**: an object whose key does not parse, whose body does not decode, or whose body `(device, seq)` disagree with its key is reported and KEPT, never deleted (a parse bug must never delete an event it cannot account for). `--dry-run` classifies the objects and reports the plan without writing anything. A real run acquires the advisory sweep lock (`meta/sweep.lock`) so it does not interleave with a concurrent `gc`/`compact` on a cooperating client. Run it **once per pre-#59 hub**; against the file-backed test hub (`--hub-file`), which never used the legacy layout, it is a no-op.

### hub login / hub logout

```bash
devstrap hub login [--access-key-id <id>]
devstrap hub logout
```

`hub login` (`P6-HUB-02`; R2/S3 hubs only — the git carrier needs no login) stores the hub S3/R2 credential pair in the OS keychain under the per-workspace account `hub-s3.<workspace_id>` (0600 file fallback `hub-s3-<workspace_id>.json` when the keychain is genuinely unavailable, e.g. `DEVSTRAP_NO_KEYCHAIN=1`; a present-but-failing keychain fails closed). The secret is read from a hidden terminal prompt, or from stdin when piped — never from argv (process listings and shell history). `op://` references are refused here: they belong in `DEVSTRAP_HUB_S3_*` env/config, where they resolve at sync time. The command reports whether the pair landed in the keychain or the file fallback. `hub logout` removes the stored pair from both custody backends. Explicit `DEVSTRAP_HUB_S3_*`/`AWS_*` env values always override the stored pair. Auth failures against the hub surface as `ErrS3Auth` with a remediation hint (`mapS3Error`), not a raw `SignatureDoesNotMatch`.

### open

```bash
devstrap open work/acme/api --cursor
devstrap open work/acme/api --vscode
```

Does:

- hydrate if skeleton;
- validate env/tooling;
- open editor.

Current implementation hydrates if needed, refuses unknown namespace paths, checks that `cursor` or `code` exists, honors a caller-cancelled context before launch but deliberately does not bind the editor process to it, and releases the child process handle so the editor outlives the short-lived CLI invocation. Env/tooling validation is still future work. Planned (`DRAFT-*`): `open` (and `hydrate`) extend beyond `git_repo` projects to materialize `local_git`/`plain_folder`/draft types from decrypted `age_blob:<sha256>` bundles.

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

Current implementation uses partial clone by default, supports `--full` and `--lfs`, refuses to clone into non-empty non-skeleton directories, stages clones in hidden sibling temp directories, promotes only after clone success plus a second target validation, preserves the original skeleton on clone failure, and updates local materialization/dirty state. The eager `materialize`/`sync` path additionally honors the stored `git_repos.lfs_policy` (`P6-GIT-04`): after clone, an `always`/`agent` repo runs `git lfs install --local` + `git lfs pull` (recorded **failed** on error, never available/clean with pointers), and `auto`/`never` warns — applied in `materializeGitRepo` so a `SkeletonProjects` retry of a failed repo cannot silently flip it to available (see `08_GIT_MATERIALIZATION_AND_WORKTREES.md`). The manual `--lfs` flag stays an explicit one-off pull. Planned (`DRAFT-*`): `hydrate` extends beyond `git_repo` projects to materialize `local_git`/`plain_folder`/draft content from decrypted `age_blob:<sha256>` bundles, while `node_modules`/build artifacts are rebuilt (npm/pnpm/uv install) rather than synced.

### run-loop

Each run-loop tick holds the shared state-home maintenance lock used by full backup, restore, and `db down`. Periodic mode prints `maintenance in progress; skipping this cycle` and succeeds when the lock is held; `--once` returns the conflict so schedulers can detect it.

`devstrap run-loop` is the portable, daemonless convergence loop: it runs **scan + sync + materialize** on an interval (`--interval`, default 5m; `--once` for cron/schedulers), identically on macOS and Linux. Progress/diagnostics go to stderr and the sync result stream to stdout (`P5-CLI-05`); a jittered backoff avoids hub stampedes and the loop aborts after 5 consecutive tick failures (`P5-CLI-05`/`P5-QUAL-03`). Per `P6-XP-03` the tick's **scan stage is real and idempotent** — it runs `scan.Walk` (offline since `P6-XP-05`) and adopts only genuinely-new findings (no active `ProjectByPath` row matching the finding's type and, for `git_repo`, `remote_key`), so a new local project reaches the hub without appending duplicate `project.added` events every tick. Warning-class findings (secret-looking files, symlink escapes) and duplicate-remote findings are surfaced on stderr and never auto-adopted; one-shot `scan --adopt` semantics are unchanged.

### service

`devstrap service install|uninstall|status` (`P4-PROD-04`) installs the `run-loop` as a background OS service so the workspace converges unattended without a bespoke daemon: a per-user **launchd LaunchAgent** on macOS (label `com.devstrap.run-loop`, managed with the modern `launchctl bootstrap`/`bootout`/`print` verbs) and a **systemd `--user` service** on Linux (unit `devstrap-run-loop.service`). The OS branch lives entirely behind `internal/platform` — the CLI never reads `runtime.GOOS` — and the platform `ServiceManager.Install` **and** `Uninstall` return OS-idiomatic advisory notes (the Linux linger caveat; the headless unit-file-only removal note, `P7-XP-03`) that the CLI prints verbatim.

- `service install [--interval 5m] [--namespace-only] [--hub-file <path>] [--label <label>] [--exec-path <path>]` refuses up front when no hub is configured (same remedy as `run-loop`) and gates on key custody (`P7-XP-02`): file custody proceeds; recorded keychain custody is refused on systemd (`--allow-keychain-custody` overrides) and warns on launchd (locked-keychain risk); an unknown recorded custody value is refused as corrupt state (`exitInvalidConfig`, re-init remedy) rather than failing open; an install run with `DEVSTRAP_NO_KEYCHAIN=1` set bakes that explicit override into the unit env so the service matches the installing session. An explicit absolute `--exec-path` is baked **verbatim** (bypassing all resolution below), otherwise it resolves the binary path from `os.Executable()` and **refuses an ephemeral path** (a `$TMPDIR` or `go-build` resolution) with a hint to install to a stable location or pass `--exec-path <abs>`. Symlinks are resolved **except** when the invoked path sits in a stable install bin dir (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, Linuxbrew's `bin`, or a keg-only/versioned formula's `<brew prefix>/opt/<formula>/bin`): the symlink itself is baked so `brew upgrade` moving the Cellar target cannot brick the unit, and a path that still resolves into a `Cellar/` segment is refused with the same `--exec-path` remedy (`P7-XP-01`). It bakes `run-loop --interval <d>` (plus `--namespace-only`/`--hub-file` and any explicitly-set `--home`/`--root`/`--config`), writes the plist/unit atomically at mode `0600`, and starts the service with `RestartOnFailure` throttled to 30s (coupled to `run-loop`'s consecutive-failure ceiling). No secret ever enters the service file — the CLI supplies at most the fixed non-secret custody flag above, and the adapters add only a `PATH`.
- `service uninstall [--label]` is idempotent (a not-installed service is a success no-op) and works headless (`P7-XP-03`): on Linux the unit file is removed even when the systemd `--user` manager is unreachable — best-effort `disable --now`/`daemon-reload` run only when it is — with an advisory note for the lingering-session case; a headless uninstall that removed nothing prints no note.
- `service status [--label]` reports `installed`/`running`/`detail`/`unit` (honoring `--json` with `{manager,label,installed,running,detail,unit_path,exec_path,exec_path_missing}`) and exits 0 regardless of run-state. The platform adapters best-effort parse the installed unit's launch binary (`ProgramArguments[0]` / the un-quoted `ExecStart` first word — our own rendered formats; a hand-mangled file degrades to an unknown ExecPath, never an error) and flag `exec_path_missing` when that binary no longer exists, prefixing `ExecPath missing: <path>` to the detail (`P7-XP-05`). On an unsupported *platform* all three exit non-zero with a clear message; on a supported platform with an unreachable session manager, `install` still fails closed while `status` (unit-file stat) and `uninstall` (headless removal, above) keep working. `doctor` folds the same status in as an optional check (omitted when unsupported, ok when running or not-installed, a warning with an inspection remedy when installed-but-stopped, and a dedicated re-run-`service install` warning when the baked ExecPath is missing — e.g. after a `brew upgrade`; while a service is installed, an effective keychain-custody store additionally warns as `run-loop service custody` with the migrate/`--allow-keychain-custody` remedies, `P7-XP-02`).

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

`add` (like `scan --adopt` and the `conflicts resolve` enactment paths) commits its signed `project.added` event and the derived `namespace_entries` row in ONE SQLite transaction (`P6-DATA-03`): a crash can no longer leave a committed event with no derived row — a divergence the origin device could never self-heal, since the apply path skips already-inserted event IDs. Filesystem work (the skeleton write) stays outside the transaction and after the commit.

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

`conflicts` (`PROD-06`) is a command group that turns the detect-don't-merge model from a read-only count into an actionable resolution surface. `list` (the default when `conflicts` is run with no subcommand) shows open conflict rows; `show <id>` prints one conflict's details and status; `resolve <id>` accepts exactly one of `--keep-local` (keep the local version, discard the remote variant), `--keep-remote` (keep the remote version, discard the local), or `--keep-both` (dual-copy: the local entry stays and the remote variant is re-added under a sibling path). Resolving first ENACTS the choice on namespace state, then commits the signed `conflict.resolved` HLC event and the row's `resolved` flip in one transaction (`P6-DATA-03` — the event and the local resolution can no longer split across a crash), so the `status` open-conflict count converges and every device sees the same outcome; the decision is recorded in `resolution_json`. Namespace files are never byte-merged; the dual-copy safe default mirrors the draft-bundle conflict behavior. Until the user resolves a `same_path_different_remote` conflict, sync installs the deterministic interim winner — the variant with the highest `(HLC, deviceID, eventID)` coordinate (HLC-monotonic, consistent with same-remote last-writer-wins; `spec/07`) — and `resolve` keeps or switches away from that installed variant.

## Env commands

```bash
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local
devstrap env check work/acme/api
devstrap env bind work/acme/api .env.refs --provider 1password --profile acme-dev
devstrap env rotate work/acme/api
devstrap run work/acme/api -- uv run pytest
```

`env rotate` re-encrypts a project's captured env blobs to the current set of approved-device age recipients (dropping any device that was revoked/marked lost), emits the same synced `env.profile.updated` pointer as `env capture`, and clears the `needs_rotation` flag on the affected `secret_bindings` rows once the fresh ciphertext is written, so `doctor`'s rotation warnings converge. Provider-ref bindings (`op://`) hold no local plaintext and are marked rotated without re-encryption.

Current implementation supports `env capture`, `env hydrate`, `env bind`, `env rotate`, and top-level `run`. Capture parses a local env file with a non-interpolating grammar, refuses dangerous names, rejects interpolation-looking values unless `--literal` is passed, encrypts the bundle to the local plus approved remote device age recipients, writes a `0600` age blob under `~/.devstrap/blobs`, stores only `age_blob:<sha256>` references in `secret_bindings`, emits `env.profile.updated` in the same transaction as the row upsert, and appends the captured file path to project `.gitignore` when possible. Hydrate decrypts the cached age blob with the local device identity or resolves 1Password provider refs through `op inject`, writes only to an explicit `--write` target, creates the file atomically with mode `0600`, refuses to overwrite unless `--force` is passed, and appends the hydrated target to project `.gitignore` when possible; if the referenced encrypted blob is not cached yet, the error keeps the missing-file class and tells the operator to run `devstrap sync`. Bind stores 1Password `op://` provider refs without resolving plaintext and emits a provider-shaped `env.profile.updated`. `sync` pushes/pulls referenced env blobs through the same content-addressed blob plane as draft snapshots. `run` injects encrypted profiles directly into the subprocess environment or delegates provider refs to `op run --env-file <temp-refs-file> -- <command>`.

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

Current implementation requires `--fresh-upstream` for `worktree new`, fetches `origin/<default_branch>` before resolving the base SHA, writes a per-repo lock under `~/.devstrap/locks`, records worktree metadata, honors the stored LFS policy by either running `git lfs pull` or warning about pointer files, and refuses dirty worktree removal unless `--force` is explicit. `worktree remove --force` handles manually deleted worktree paths by running `git worktree prune` from the main checkout and marking the DB row removed. `worktree status <id>` re-fetches the recorded base ref and reports whether the worktree is fresh or stale. `worktree finalize <id>` reuses the same stale-base check and exits non-zero if the base moved unless `--allow-stale-base` is set. `cleanup --merged` takes no positional args (`cobra.NoArgs`); removes clean, merged worktrees under the project repo lock (with a dirty re-check immediately before remove); skips any worktree that still has a running `agent_runs` row after a stale-PID sweep; prunes stale missing paths; reports a skipped count for unreadable, dirty, lock-contended, or live-agent worktrees; and only removes a merged-but-dirty worktree when `--force` is set. Merged-ness is ancestry OR content-equivalence (`P4-GIT-04`): the `git branch --merged` ancestry check runs first, and its misses consult `git.Runner.IsSquashMerged` — a current-tree merge probe (`git merge-tree --write-tree <base> <branch>`, git ≥ 2.38) that reports merged only when the simulated merge's tree is identical to the current base tree, catching GitHub "Squash and merge", rebase-, and cherry-pick-merges while correctly treating a merged-then-reverted change as NOT merged — with the conservative rule that a conflicting merge, an older git, or any error means NOT merged (a false positive would delete a real worktree and its branch). Reaps are labeled `merged` vs `merged (squash)`, the recorded `remote/branch` base is best-effort fetched under the repo lock first (warn + continue offline), reaped worktrees also get `git branch -D` (warn-only on failure), and forge-API (`gh pr list`) cross-checks are an explicit non-goal so cleanup works offline. `worktree unlock <path>` reports the holder of a project's repo operation lock and clears it when the holder is dead/stale (or when `--force` is set), providing a recovery path after a crash; `doctor` also lists held locks. The default-branch resolution for `worktree new` confirms the remote default authoritatively via `git ls-remote --symref origin HEAD`, repairing a missing `origin/HEAD` with `git remote set-head origin --auto` and warning if the result is not authoritative.

## Agent commands

```bash
devstrap agent run work/acme/api --engine generic --task "fix failing tests" -- npm test
devstrap agent list
devstrap agent show arun_01jz...
devstrap agent pr arun_01jz...
devstrap agent cleanup --merged
```

**PID-reuse guard (`P7-GIT-03`, 2026-07-11):** new runs record both the recorder PID and an opaque platform start-time identity. The list/show/pr/doctor sweep treats a live PID with a different start identity as a recycled PID and interrupts the crashed run; legacy rows with no identity retain PID-only behavior, and an identity lookup error is conservatively treated as indeterminate/alive.

Current implementation supports `agent run/list/show/pr`. `agent run` creates a fresh upstream worktree, runs an explicit generic command with a sanitized no-secret default environment, applies wrapper-level command and file path policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`), records the run in SQLite with `status='running'`, its recorder PID, and sandbox backend/mode/limitations, captures a `0600` log, and stores a labeled Git diff summary: `Committed since base:` diffs the recorded `BaseSHA` against `HEAD`, while `Uncommitted:` records `git status --short` residue. The file path policy denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs; it is a preflight wrapper policy. **On macOS and Linux the run is additionally OS-sandboxed** (`P4-GIT-03`, 2026-07-05): `--sandbox auto|off|require` (env `DEVSTRAP_SANDBOX`, default `auto`) wraps the child argv in `/usr/bin/sandbox-exec` with a generated Seatbelt profile on macOS (whose credential-read denies resolve each anchor's leaf symlinks and deny both the literal alias and its kernel-real target, so a `~/.ssh -> /elsewhere` symlink cannot dodge the deny); on Linux the adapter lazily probes and selects bubblewrap, then Landlock, then unsupported. `DEVSTRAP_SANDBOX_BACKEND=bwrap|landlock` forces a Linux backend, a forced backend never silently falls through to the other one, and an invalid value is an explicit-config error that fails closed in every mode (a typo must never silently disable the sandbox). Bubblewrap provides the full Linux backend (read-only root, read-write worktree/tmp binds, credential masks, optional net namespace, user namespace, pid namespace, die-with-parent, and new-session protections); its credential masks are dropped only when a masked path genuinely does not exist, while permission-denied, symlink loop, or I/O errors keep the literal mask so a credential is never left readable through a resolution failure. Landlock is the layered fallback: it is a real kernel write-confinement boundary, but it is additive-allow, so `agent run` prints one `notice: OS sandbox landlock active with reduced guarantees: ...` line documenting that credential reads are NOT denied, network deny is TCP bind/connect only at Landlock ABI >= 4 (not enforced below that), and mount/pid namespace guarantees are absent. `auto` degrades to one loud warning only when no backend can run; `require` refuses to run unsandboxed with the policy exit class before any worktree is created, and also refuses `readonly`/`cautious` when the selected backend cannot enforce their network deny at all; a Landlock TCP-only deny (ABI >= 4) satisfies `require` but prints a warning that UDP, QUIC, and unix-domain sockets stay open; `yolo-local` is unconfined and conflicts with `require`. Both Linux backends also install a seccomp syscall denylist (mount, kexec/module, ptrace/tracing, keyring, io_uring, and legacy-escape syscalls return `EPERM`; `clone`/`unshare`/`setns`/`execve`/`fork` stay allowed): bubblewrap reads the compiled cBPF filter from an inherited fd (`--seccomp`), and the Landlock shim loads it in-process after the ruleset and before `execve`. It is unconditional hardening for every sandboxed policy and is compiled for the running arch (x86-only names are dropped on arm64). `DEVSTRAP_SANDBOX_SECCOMP=off` disables it (a mistyped value fails closed with the invalid-config exit class), and a kernel without seccomp-filter support degrades to a reduced-guarantees notice rather than failing `require`. macOS Seatbelt profiles now embed a per-run denial tag, and after the run DevStrap best-effort reads matching unified-log rows into the unsigned local `sandbox_violations` table with scrubbed path/detail fields. `agent show` prints a sandbox line plus violation count/details, and `agent show --json` returns the `AgentRun` fields plus a `violations` array; `doctor` warns when any run has recorded denials. Linux runtime denial detection remains future, so Linux runs populate backend/mode/limitations but not violation rows. **Tighter read confinement** is opt-in via `--read-confine auto|on|off` (env `DEVSTRAP_SANDBOX_READ_CONFINE`, default `auto` = on for the `readonly` policy only; `--read-allow <abs>` adds roots): all three backends restrict the child's reads to the worktree/tmp, the OS toolchain/system roots, and the `$HOME` build caches instead of the whole disk, so the rest of `$HOME` and other projects are unreadable. An explicit `--read-confine on` (or `require`) refuses to launch when the backend cannot enforce it — including when no OS sandbox is available at all — while an `auto`-derived request degrades to a warning. A `--read-allow` root that overlaps a protected credential path is refused (read confinement drops bwrap's credential masks and Landlock cannot subtract from an allowed root, so such a root would silently re-expose the credential). The hidden `sandbox-helper` command is internal to the Landlock backend: it re-execs the real binary, applies Landlock to its own process, then `execve()`s the agent argv in the same PID; exit 125 means the shim failed and is surfaced by the parent as 225 via `childExitBase`. `agent list`, `agent show`, `agent pr`, and `doctor` reconcile `running` rows whose recorded PID is confirmed dead to `interrupted`; that status means the run was still `running` when its recording process exited or crashed. `agent pr` refuses any run whose status is not `complete` unless `--allow-incomplete` is passed (then it warns to stderr), refuses stale recorded bases unless `--allow-stale-base` is passed, pushes the agent branch, and creates a PR/MR through the detected forge CLI (`gh`/`glab`/`tea`) when available; unsupported forges get the pushed branch and compare URL instead of a failed hardcoded GitHub path. SSH host aliases resolve via `ssh -G` with a config-file fallback; the forge-resolution chain (`ResolveForge`/`DetectForge`/`resolveForgeHost`/`resolveSSHHostAlias`) threads the caller's context so the bounded `ssh -G` timeout derives from it rather than a fresh `context.Background()` (`P4-QUAL-07` — the `contextcheck` linter is enabled and enforces this); that resolution is exercised hermetically in tests through a PATH-shimmed `ssh` stub, never the developer machine's OpenSSH config (`P6-QUAL-04`). `--dry-run` reports the planned PR without pushing. Non-generic engines, project-env allowlists, and `agent cleanup` remain future work.

## Device commands

```bash
devstrap devices list                      # last column is each device's fingerprint (P4-SEC-04); "-" when a row lacks keys
devstrap devices enroll dev_01jz... --name linux-desktop --os linux --arch arm64 --age-recipient age1... --signing-public-key ed25519:... --approve --fingerprint ABCD-EFGH-...
devstrap devices enroll --code 'devstrap-pair1:...' --approve --fingerprint ABCD-EFGH-...
devstrap devices approve dev_01jz... --fingerprint ABCD-EFGH-...
devstrap devices revoke dev_01jz...
devstrap devices lost dev_01jz...
devstrap devices rename dev_01jz... linux-desktop
devstrap devices pairing-code              # stdout is exactly the devstrap-pair1: blob + newline; stderr prints instructions + fingerprint
devstrap devices recipient                 # print local device's age recipient (for out-of-band enrollment)
devstrap devices recipient --signing       # print local device's Ed25519 signing public key
devstrap devices recipient --workspace-id  # print the workspace id (for init --join --workspace-id on a joining device)
devstrap devices recipient --fingerprint   # print local device's fingerprint (compare out-of-band during approval)
```

Current implementation enrolls remote device records either manually with identity/key flags or via `--code <devstrap-pair1:...>`, lists and renames device records, prints a local pairing code, and updates non-local device trust state to `approved`, `revoked`, or `lost`. `devices pairing-code` reads the local device row and workspace id, refuses if the local device lacks either public key, prints exactly the one-paste blob plus newline on stdout (frozen script contract), and prints the local fingerprint plus operator instructions on stderr. The blob carries workspace id, device id, name, OS, arch, age recipient, and signing public key; it carries **no fingerprint** and is unauthenticated by design, so integrity still comes from confirming the derived fingerprint. `devices enroll --code "$CODE"` is mutually exclusive with the manual identity/key flags and carries the device id itself, so no positional id is accepted; it refuses a workspace-id mismatch before falling through to the existing approval, epoch-contiguity, upsert, grant, and replay flow. Composition target: `devstrap devices enroll --code "$CODE" --approve --fingerprint "$FP"` is the founder-side one-command enrollment. `devices recipient` is a read-only helper that prints the local device's age recipient (or Ed25519 signing public key with `--signing`, the workspace id with `--workspace-id` for `init --join --workspace-id` pairing, or the device fingerprint with `--fingerprint`; `--signing`, `--workspace-id`, and `--fingerprint` are mutually exclusive, and the bare default output stays frozen because scripts consume it unadorned) so it can be shared out-of-band for manual enrollment on another device. `devices list` appends each device's fingerprint as the **last** column (earlier columns unchanged; `-` when a row lacks either key); `--json` is unchanged and does not carry the fingerprint. Env capture encrypts local bundles to the local recipient plus approved remote recipients.

**Fingerprint confirmation (`P4-SEC-04`).** `devices approve` and `enroll --approve` gate the trust-state change on out-of-band fingerprint confirmation *before* any DB write. The fingerprint is a full 256-bit digest binding the device's Ed25519 signing key and age recipient (never a truncated short authentication string), computed from the row/flags/code being approved — never from the local keystore — and rendered as 13 dash-separated base32 groups. Confirmation resolves in one of three ways: `--fingerprint <value>` compares (constant-time, dash/case/space-insensitive) and refuses on mismatch; with no flag and a TTY the fingerprint is printed and the operator must type `yes`; with no flag and no TTY the command refuses with a copy-paste remedy embedding the computed `--fingerprint <value>` (except `init --join --code`, which keeps initialization scriptable by storing the founder as pending and printing the follow-up approve command). `SECU-05`: approving a stored row that lacks a signing key **or** age recipient (a bare pending placeholder auto-created by sync) is refused with a re-enroll remedy rather than pinning a keyless row. `devices revoke`/`lost` are unaffected. `devices approve` and `enroll --approve` grant every held WCK epoch to the newly-approved device (`P4-SEC-07`); on a keyless **joiner** the approve path grants nothing (it is founder-gated — a joiner never self-mints) but still pins the enrolled device's keys and flips verification fail-closed, which is the documented founder-pinning ceremony a joiner runs BEFORE its first sync (`P4-SEC-04` joiner half; in a multi-device fleet the joiner pins every existing device this way — an unpinned signer's events quarantine and replay once that device is approved); `devices approve` and `enroll --approve` also replay open `verification`-kind `event_verification_failure` conflicts from that device using the stored full event JSON and resolve conflicts whose events now apply (`divergent`-kind rows are never auto-resolved). `devices approve` and `enroll --approve` refuse (before any trust write) when this device's own keyring is incomplete — a gap in held epochs `1..max` or an open `key_grant_waits` row — because the grant set would inherit the gap and strand the approved device (`P6-SEC-03` contiguity guard); `--allow-epoch-gap` overrides, after which the approved device quarantines events at the missing epochs until re-approved from a complete device — note those open quarantine conflicts also keep `hub gc` refused on that device for as long as the gap lasts (run gc from a complete device) — and a keyless device always passes (the founder-pinning ceremony grants nothing). The contiguity guard runs before the fingerprint prompt, so an operator is never asked to confirm an approval that will be refused. `devices revoke`/`lost` rotate the WCK to a new epoch (go-forward forward secrecy) before the blob re-encryption pass; env blobs emit superseding `env.profile.updated` events and draft blobs emit superseding `draft.snapshot.created` events before hub cleanup, so peers never replay a deleted ciphertext ref. When a hub is configured, revoke also best-effort deletes the revoked device's signed sync ack from the hub (`P4-SYNC-06`; a compactor already ignores non-approved acks and reclaims the whole stream, so a failure here is non-fatal). It refuses to change the current local device trust state so a user cannot revoke the only active local root by accident. Revoke/lost additionally emit a synced `device.revoked`/`device.lost` event in the same transaction as the trust flip (TRUST-01), so the fleet learns the decision on its next sync — receiving devices flip the target sticky/monotonically and flag `needs_rotation`; approval never propagates (local ceremony only). Automatic remote enrollment remains future work.

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
- bind log verbosity to `DEVSTRAP_LOG_LEVEL`, `--quiet`, and `--verbose`;
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
10 usage error (fully wired: hand-mapped sites, Cobra flag-parse errors, positional-arg validators, and unknown-subcommand errors all map here — P6-CLI-03)
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
devstrap sync   # hub: git@host:path.git (zero-infra git carrier, AD-1) or hub: r2://<bucket> (R2/S3, HUB-*); the --hub-s3 flag was superseded by the hub: config value
```

PR creation becomes forge-agnostic (`gh`/`glab`/`tea`) with a `--forge` override (`FORGE-01`).

## Audit implementation notes (2026-06-28)

- **CLI-02**: `scan --quarantine` progress lines now go to stderr, preserving valid JSON on stdout.
- **CLI-03**: `run` and `agent run` propagate child exit codes as `100+N` (new `childExitBase`).
- **CLI-04**: Added `exitUsage = 10` and `childExitBase = 100` (child process exit codes). `P6-CLI-03` is now shipped: `SetFlagErrorFunc` wraps Cobra flag-parse errors, every leaf command's positional-arg validator is wrapped via `usageArgs`, and unknown top-level subcommands are caught by a narrow match on Cobra's own error text in `ExitCodeWithWriter`. The fallback match is necessary because Cobra's `Find()` resolves unknown subcommands before any command hook runs.
- **PROD-01**: `deriveDisplayStatus` maps materialization+dirty states to user-facing labels; `status` output uses it.
- **PROD-02/PROD-06**: `devstrap conflicts` is a command group (`list`/`show`/`resolve --keep-local|--keep-remote|--keep-both`) that surfaces and resolves open conflicts; `status` shows the open-conflict count and it converges as rows are resolved.
- **ARCH2-04**: Reserved `exitDaemonUnavailable` code for M5 daemon.

## Cloud-sync CLI (2026-06-28)

The cloud-sync architecture (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`) shapes the sync/materialization commands. The eager-clone materialization and R2/S3 hub items below are shipped; the rest remain planned:

- **Eager materialization (`EAGER-*`)**: `devstrap sync` (shipped) clones the whole `~/Code` tree up front via blobless/partial clone as the default behavior (`--namespace-only` opts out) — no FUSE, placeholder, or lazy-VFS layer (StrapFS stays deferred). After a **successful** sync the full tree is present on disk; per-project materialization failures are isolated and leave those entries as `failed`, and the command finishes non-zero (see the `sync`/`materialize` exit codes above), so the "whole tree present" guarantee is scoped to a clean exit.
- **Two-plane zero-knowledge hub (`HUB-*`)**: the hub carries only (a) the signed, HLC-ordered namespace map (event log) and (b) content-addressed `age_blob:<sha256>` ciphertext for env and non-git/draft content. Repo content never traverses the hub — it rides git transport from the existing remote. `hub: r2://<bucket>` (or `s3://`) selects the shipped Cloudflare R2 / S3 backend (`aws-sdk-go-v2` adapter behind `hubFromOptions`) and `hub: git+ssh://…` selects the shipped zero-infrastructure private-git-repo carrier (`AD-1`), both behind one pluggable Hub interface; `--hub-file` stays for tests only. Credentials resolve via env/config (values may be `op://` refs), then `AWS_*` literals, then the `hub login` keychain slot / 0600 file fallback — never the URI (`P6-HUB-02`).
- **Content-type split (`DRAFT-*`)**: env plus non-git/draft folders sync as age-encrypted blobs; `node_modules`/build artifacts are never synced and are rebuilt on hydrate. `hydrate`/`open` extend to `local_git`/`plain_folder`/draft project types; `devstrap promote` walks a folder from plain -> draft -> git (`NOVCS-03`).
- **Conflicts stay detect-don't-merge**: HLC ordering plus tombstones; `devstrap conflicts` (shipped) surfaces them. Files are never byte-merged.
- **Device trust**: revocation re-encrypts affected blobs to the reduced recipient set and flags secrets for rotation; once device enrollment exists, event verification must fail closed (`SECU-03`).

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-CLI-01 — Re-running `init` with a new root splits DB root vs config.yaml — RESOLVED (`fix/p6-cli-01`, 2026-07-03)

**Problem.** `writeDefaultConfig` early-returns without writing when config.yaml exists (`internal/cli/init.go:182-183`), while `state.EnsureWorkspace` unconditionally updates `root_path` (`internal/state/store.go:473-480`), so `init root2` after `init root1` makes `status` (DB) report root2 while config-driven `scan`/`materialize`/`sync` keep using root1.

**Resolution.** Before calling `EnsureWorkspace`, `init` reads the existing workspace row and compares the stored root to the effective resolved requested root (`DEVSTRAP_ROOT`, `--root`, or positional `[root]`, after the same absolute clean normalization used for first init). A different root now exits `exitConflict` and names both roots plus the `--move-root` remedy. `--move-root` accepts the relocation and rewrites `config.yaml` atomically via same-directory temp file + rename with mode `0600`; same-root re-init remains a no-op success and dry-run still writes nothing. Pinned by `TestInitReRunSameRootSucceeds`, `TestInitReRunNewRootRefusedWithConflict`, and `TestInitMoveRootRewritesConfig`.

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

### P6-CLI-04 — `--quiet` only lowers slog verbosity; stdout chatter ignores it — RESOLVED (`fix/p6-cli-04`, 2026-07-04)

**Resolution.** `options.progressf` is the quiet-aware progress seam (`internal/cli/root.go:52-57`), and the flag help now says `suppress progress output (results and errors still print)` (`internal/cli/root.go:87`). Progress and action-summary chatter now routes through that seam for sync snapshot-recovery/status/GC/key-rotation/drained-delete lines (`internal/cli/sync.go:105`, `169-172`, `184`, `186`, `339`, `434`), materialize's human renderer only (`internal/cli/materialize.go:86-88`; `--json` remains structured output), init success/adopt/next-step hints (`internal/cli/init.go:247-300`), run-loop tick and scan-adopt progress (`internal/cli/run_loop.go:72`, `141`), hub login/logout/gc summaries (`internal/cli/hub.go:435`, `461`, `541`), and the `scan --adopt` adopted-count summary (`internal/cli/scan.go:102`). The "awaiting workspace key grant" deferred-push notice (`internal/cli/sync.go:406`, sibling to the always-visible one in `snapshot_recovery.go`) is deliberately left ungated — it is the only explanation of a real actionable state, not chatter. Dry-run output, result rows, warnings, prompts, JSON output, and error/exit-code signals stay ungated. Pinned by `TestQuietSuppressesInitProgressButCreatesWorkspace` and `TestQuietSuppressesMaterializeHumanProgressOnly`.

**P7-CLI-03 follow-up (`fix/p7-cli-03-quiet-routing`, 2026-07-13).** The general rule above suppresses *progress* under `--quiet`, but three commands routed their only confirmation of a **completed state change** through `progressf`, so `--quiet` produced zero output for a real mutation. The terminal confirmation lines are now written with `fmt.Fprintf` directly (never gated), mirroring the deferred-push exception: `installed … service`/`uninstalled … service`/`… not installed; nothing to do` (`internal/cli/service.go`), and `Configured hub:`/`hub already configured …` (`internal/cli/hub.go`, `hub init`). Auxiliary progress on the same commands stays gated — service install's `unit:`/`logs:` lines and `hub init`'s `Next:`/`Joiner:` hints. Pinned by `TestServiceInstallConfirmationSurvivesQuiet`, `TestServiceUninstallConfirmationSurvivesQuiet`, and `TestHubInitConfirmationSurvivesQuiet`.

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
