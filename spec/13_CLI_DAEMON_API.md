---
last_reviewed: 2026-08-09
tracks_code: [cmd/**, internal/cli/**, internal/daemon/**, internal/manifest/**, internal/platform/**, internal/secrets/**, internal/shellhook/**]
---
# CLI and Daemon API

### W13-05 staging-orphan sweep (2026-08-01)

After convergence materialization, a sweep gated by its own `staging.sweep_interval` (default 24h; `0` disables — `P11-SWEEP-01`) reports every clone-staging removal or skip and isolates failures from the tick; an invalid interval warns on stderr naming this sweep instead of being discarded at the call site. Mapped names acquire the per-project repo lock before filesystem inspection; only unmapped names use the one-hour mtime guard. `Lstat` rejects symlinks and non-directories, registered rows are never deleted, and `doctor` surfaces legacy staging-pattern registrations. Namespace validation rejects new peer or scan paths carrying the reserved basename. Accepted residual: unlocked, unregistered manual work under `.X.devstrap-tmp-*` may be removed after the age window.

Linux watch-budget observability is explicit at both API layers: `/v1/health`
optionally reports `watch.watch_limit` without a percentage, while `doctor`
always renders a `watch budget` row and warns at 60% of the runtime-read limit.

## `wip gc`

```text
devstrap wip gc [<project>] [--device <id>] [--ttl <duration>] [--dry-run]
```

Without a project, GC visits every materialized `git_repo`; the default TTL is
`720h`. `--device` explicitly permits an aged live peer ref. `--dry-run`
enumerates and plans but neither deletes nor persists orphan first-seen state.
Origin/auth/network failures are per-project warnings and do not fail the
whole command. Invalid arguments/config use the normal usage/config exit
class; local store failures retain the default class.

`--json` emits one document:
`{"ttl":"720h0m0s","dry_run":false,"actions":[{"path","device_id","ref","sha","reason","delete"}],"warnings":[{"path","message"}]}`.
Warnings are fields in that document, never loose stdout. A nominated object
newer than its mirror record is retained with reason
`"object is newer than its mirror record; not deleted"`.

Plain full `sync` runs this exact sweep after pull and materialization, beside
local blob GC. `wip.gc_interval` defaults to `24h`; `wip.ttl` defaults to
`720h`; `0` disables either gate, while negative/malformed values fail as
invalid config. Operational origin/auth failures warn per project and never
fail convergence. Sync JSON adds `wip_refs_gcd`. Doctor reports last-success
age, warns past twice the interval, and reports disabled when either is zero.

## CLI principles

- dry-run available for mutating commands;
- explain what will happen before destructive changes;
- never hide dirty-state warnings;
- keep commands composable;
- JSON output for automation;
- human-friendly rich output by default.

## `--json` output conventions (P5-CLI-01)

Every command's terminal output should route through the `Renderer` seam (`internal/cli/render.go`): `opts.render(w, humanFunc, typedValue)` encodes `typedValue` as indented JSON when `--json` is set, otherwise it invokes `humanFunc`. This is the single seam for a command's *own result* — never an ad hoc `if opts.v.GetBool("json") { ... }` block deciding what a command's JSON payload looks like — so `--json` is a uniform contract instead of a flag a minority of commands honor. The one narrow exception is an **orchestration-only render-ownership guard**: `runInit` (shared by standalone `init` and, internally, by `up`/`join`) and `up`'s own closing summary each check `opts.v.GetBool("json")` directly to suppress a nested or trailing human-only print that would otherwise corrupt the single outer command's JSON document — see the Part B progress note below for the full nested-render design. These guards never decide *what* gets rendered, only whether an inner/trailing human line is allowed to print at all; the one JSON document per invocation still comes from exactly one `opts.render` call. The seam originally backed only `db backup --full`, `db restore`, and `materialize`; twelve more call sites across eight commands (`agent list`/`agent show`, `conflicts list`/`conflicts show`, `devices list`, `doctor`, `scan`, `worktree unlock`/`worktree status`/`worktree list`, `status`, `service status`) were migrated to it in the same change that added this section (`P5-CLI-01`, part A — see "Migration/compat rule" below). **Part B is now complete (2026-07-16, PR-0 + PR-B1 through PR-B8 — nine PRs total).** Every remaining leaf command is wired to the `Renderer` seam except two documented, deliberate exemptions: `run` (its stdout is a transparent passthrough to a child process — wrapping it would corrupt or discard real child output) and `pair` (an interactive wizard blocking on stdin with no coherent one-shot JSON contract). This satisfies the finding's own "wire it, or reject it" framing; see the Part B progress notes below for the full per-domain breakdown. The conventions below are derived from the precedent already established across all these call sites and the typed values they encode (`state.Device`, `state.ProjectStatus`, `state.Conflict`, `state.AgentRun`, `state.Summary`, `fullBackupResult`, `checkResult`, `repoLockReport`, `worktreeStatusOutput`, `serviceStatusJSON`, `scan.Result`), not invented fresh.

**Field naming.** Every `json:` tag in the codebase is `snake_case` for multi-word fields (`project_id`, `base_ref`, `dirty_state`, `workspace_id`, `remote_key`, `sandbox_backend`, `runner_started_at`, `exec_path_missing`, `warnings`, `secrets`, `entries`, ...). There is no camelCase precedent anywhere in `internal/state`, `internal/scan`, or `internal/cli`. New JSON-emitting types must use `snake_case` tags.

**Named vs. anonymous inline result types.** Existing call sites follow one of three shapes, and new commands should pick the same way:
- If the JSON payload *is* an existing exported type from its owning package (e.g. a list of `state.Device`, `state.ProjectStatus`, `state.Worktree`, or `state.Conflict` rows), encode that type directly — bare array or bare object — rather than introducing a synthetic wrapper. Don't add a wrapper struct purely to "give it a name."
- If the payload assembles fields from multiple sources, needs derived/computed fields not on any store type, or is rendered by more than one code path, define a named struct at file scope in `internal/cli` (the `repoLockReport`, `worktreeStatusOutput`, `checkResult`, `serviceStatusJSON`, `fullBackupResult` pattern). Prefer this default for anything beyond a one-off.
- Reserve an anonymous inline struct literal (the `materialize.go` pattern) for a single trivial summary that exists only to be passed once to `opts.render` and is never referenced elsewhere. When a payload needs to combine an existing typed value with one extra field, anonymous struct embedding is acceptable (`agent show`'s `struct { state.AgentRun; Violations []state.SandboxViolation \`json:"violations"\` }`).

**Optional fields: value + `omitempty`, not pointers.** No type in the codebase uses a pointer field for an optional JSON value (e.g. `*string`); every optional field is a plain value type tagged `,omitempty` (`PID int` `json:"pid,omitempty"`, `Hostname string` `json:"hostname,omitempty"`, `ExecPath string` `json:"exec_path,omitempty"`). Follow this default: a pointer is only justified if the field's zero value (`0`, `""`, `false`) is itself a meaningful, distinct-from-absent observation that must round-trip — no shipped command currently needs that, so don't introduce a pointer field without a concrete case for it.

**Warnings / partial-failure shape.** `P7-CLI-01` set the standard: a result struct carries a `Warnings []string` `json:"warnings,omitempty"` field (see `fullBackupResult`, `scan.Result`, `restoreResult`). Non-fatal warnings are appended to that slice instead of being `Fprintf`'d to stdout ahead of the JSON payload. The human-render callback passed to `opts.render` prints each warning line (`"warning: %s\n"`) before its summary; the JSON branch carries the same warnings inside the payload. This keeps `--json` stdout a single parseable document in both the success and partial-warning cases. New commands that can produce non-fatal warnings should follow this shape rather than writing warning text directly to stdout.

**Migration/compat rule for this PR.** The twelve call sites across eight commands (`agent list`/`agent show`, `conflicts list`/`conflicts show`, `devices list`, `doctor`, `scan`, `worktree unlock`/`worktree status`/`worktree list`, `status`, `service status`) previously emitted `--json` output through an older inline `json.NewEncoder(stdout)` pattern rather than the `Renderer` seam. This change migrated all twelve to `opts.render` while **preserving each command's exact prior JSON output shape byte-for-byte** — only the internal call moved from a raw `json.NewEncoder`/`enc.Encode` block to `opts.render`; no field was renamed, added, removed, reordered, or reshaped as part of that move (see `internal/cli/render_migration_test.go`, which pins the shape for the ten call sites that had no prior `--json` test coverage). The conventions in this section govern *new* commands and any deliberate future reshaping of these commands' output — they do not retroactively authorize a breaking change to `--json` consumers of these commands going forward.

**`status`/`doctor` failure visibility (P4-GIT-07).** `status --json`'s per-project `Projects[]` entries (`state.ProjectStatus`) gained a `last_error` field (empty when the project's last materialize attempt succeeded); human mode prints a "Failed materializations:" section listing only the projects with a non-empty error. `doctor --json` gained one `checkWarn`-status check per `materialization_state=failed` project (`Name: "materialize: <path>"`, `Detail` carries the scrubbed error text). Both are additive fields/rows, not a reshape of the existing `P5-CLI-01` shapes for those two commands. `internal/cli/materialize_failure_test.go` is the end-to-end regression test for this path (clone failure → `last_error` persisted → surfaced in both commands' `--json` output → cleared on a subsequent successful materialize).

**Part B progress: `hub *` domain wired (2026-07-16).** The first part-B batch migrated the entire `hub` command group to the `Renderer` seam: `hub init` (`hubInitResult` — `hub`, `scheme`, `already_configured`), `hub login`/`hub logout` (`hubLoginResult`/`hubLogoutResult` — `workspace_id`, `location`, `action`; no secret material ever enters the payload), `hub gc` (`hubGCResult` — `pruned_snapshots`, `blobs_deleted`, `blobs_retained`, `dry_run`, `grace_window`, `keep`), `hub compact` incl. `--dry-run` (`hubCompactResult` — snapshot/floor/tombstone-GC/dry-run-preview fields), and `hub migrate-events` (`hubMigrateEventsResult` — `migrated`, `already_migrated`, `unparseable_kept`, `dry_run`). `hub init`'s `--quiet`-surviving confirmation lines (`P7-CLI-03`) are unchanged; only the JSON branch is new. Remaining part-B domains (`devices`, `agent`/`conflicts`, `worktree`, `env`/`draft`/`keys`, `db`, `sync`/`run-loop`/`run`, and the rest) are still unwired as of this entry; the "roughly 25 leaf commands" estimate above is now known-stale (closer to ~44, including `up`/`join`/`pair`, which postdate that estimate) and will be corrected once part B fully closes.

**Part B progress: `devices *` domain wired (2026-07-16).** The second part-B batch migrated the remaining `devices` leaf commands (part A already had `devices list` encoding `[]state.Device`) to the `Renderer` seam: `devices enroll` (`deviceEnrollResult` — `device_id`, `trust_state`; both the manual-flags and `--code` call sites go through `runDeviceEnroll`'s single render), `devices pairing-code` (`devicesPairingCodeResult` — `code`, `fingerprint`; stderr guidance text stays always-on human help, not JSON-gated), `devices approve`/`revoke`/`lost` (shared `deviceTrustResult` — `device_id`, `trust_state`; post-trust rotation/rewrap/propagation notes stay on stderr and are not folded into a `Warnings` field), `devices rename` (`deviceRenameResult` — `device_id`, `name`), and `devices recipient` (`deviceRecipientResult` — `kind` one of `recipient`/`signing`/`workspace_id`/`fingerprint`, plus `value`; human mode still prints only the bare value on a line — frozen script contract). Remaining part-B domains (`agent`/`conflicts`, `worktree`, `env`/`draft`/`keys`, `db`, `sync`/`run-loop`/`run`, and the rest) are still unwired as of this entry.

**Part B progress: `agent`/`conflicts` commands wired (2026-07-16).** The third part-B batch migrated the remaining `agent`/`conflicts` leaf commands (part A already had `agent list`/`agent show` and `conflicts list`/`conflicts show`) to the `Renderer` seam: `agent run` (anonymous embed of `state.AgentRun` plus `worktree` — the worktree path is not on the store type; success path only, after refreshing the local `AgentRun` copy with final status/diff/test summaries because `UpdateAgentRunResult` returns only an error; child-exit failures still return `appError`/`error` with no JSON success payload), `agent pr` (`agentPRResult` — `run_id`, `base`, `head`, `url` omitempty, `dry_run` omitempty; both the real create and `--dry-run` call sites), and `conflicts resolve` (`conflictResolveResult` — `conflict_id`, `action`, `note` omitempty; distinct from the event-log `resolution` map written to `details_json`). Remaining part-B domains (`worktree`, `env`/`draft`/`keys`, `db`, `sync`/`run-loop`/`run`, and the rest) are still unwired as of this entry.

**Part B progress: `worktree *` domain wired (2026-07-16).** The fourth part-B batch migrated the remaining `worktree` leaf commands (part A already had `worktree unlock`/`worktree status`/`worktree list`) to the `Renderer` seam: `worktree new` (encodes `state.Worktree` directly — same exported type as list/status, no wrapper), `worktree finalize` (`worktreeFinalizeResult` — `id`, `base_ref`, `base_sha`, `current_sha`, `fresh`, `behind`; both the ready and stale-with-`--allow-stale-base` human branches share one result built after drift is known), `worktree remove` (`worktreeRemoveResult` — `id`, `pruned`; `pruned=true` for the missing-path force prune, `false` for a normal remove), and `worktree cleanup` (`worktreeCleanupResult` — `removed`, `skipped`, `reaped` omitempty of `worktreeReapEntry` rows with `id`/`branch`/`merge_label`/`branch_tip` omitempty). Cleanup was a real output-routing refactor, not a one-line render swap: `cleanupOneWorktree` now returns `(*worktreeReapEntry, error)` instead of printing per-worktree success lines mid-loop, and its non-fatal base-refresh / branch-delete diagnostics write to stderr so `--json` stdout stays a single pure document (same class of purity bug as hub compact's drain-blob notice in PR-B1). Remaining part-B domains (`env`/`draft`/`keys`, `db`, `sync`/`run-loop`/`run`, and the rest) are still unwired as of this entry.

**Part B progress: `env`/`draft`/`keys` domain wired (2026-07-16).** The fifth part-B batch migrated the secret-adjacent leaf commands that previously had zero `--json` support to the `Renderer` seam: `env capture` (`envCaptureResult` — `path`, `ref`, `bindings`, `recipients`), `env rotate` (shared `envRotateResult` — `path`/`ref`/`recaptured`/`recipients` omitempty plus required `cleared`; covers `--all`, bare project clear, and the 2-arg re-capture path), `env hydrate` (`envHydrateResult` — `path`, `target`, `variables`; decrypted plaintext still goes only to the target file via `writeHydratedEnvFile`, never into the JSON payload), `env bind` (`envBindResult` — `path`, `provider`, `refs` as a count only — the `op://` pointer map is not exposed), `draft snapshot create` (`draftSnapshotResult` — `path`, `blob_ref`, `file_count`, `byte_size`, `recipients`), and `keys rotate` (`keysRotateResult` — `epoch`, `grants`; the `clearWCKRotationPending` warning stays on stderr exactly as before). Every result struct is scoped to the same safe metadata already printed by the human lines (counts, content-addressed refs, paths) — no `Content`/`Value`/`Plaintext` fields. Tests in `internal/cli/env_render_test.go` additionally assert known plaintext fixture values never appear in `--json` stdout. Remaining part-B domains (`db`, `sync`/`run-loop`/`run`, and the rest) are still unwired as of this entry.

**Part B progress: `db *` domain wired (2026-07-16).** The sixth part-B batch migrated the remaining plain `db` leaf commands (part A already had `db backup --full` and `db restore`/`db restore --recover`) to the `Renderer` seam: `db migrate` (`dbMigrateResult` — `version`), `db status` (`dbStatusResult` — `version`, `quick_check`, `foreign_key_check`), plain `db backup` without `--full` (`dbBackupResult` — `path`; the `--full` path still uses `fullBackupResult` and was not reshaped), and `db down` (`dbDownResult` — `version`; a separate type from `dbMigrateResult` despite the identical shape, matching this file's one-struct-per-command style). Human lines are unchanged. Remaining part-B domains (`sync`/`run-loop`/`run`, and the rest) are still unwired as of this entry.

**Part B progress: `sync`/`run-loop` wired, `run` documented exempt (2026-07-16).** The seventh part-B batch migrated `sync` (and, free of charge, `run-loop --once`) to the `Renderer` seam via one shared `syncResult` covering dry-run and real cycles (`hub_id`, `dry_run`/`would_push` omitempty, `pushed`/`pulled` omitempty, `deferred`/`namespace_only`/`key_rotated` omitempty, `blobs_gcd` omitempty, `materialized_total`/`materialized_succeeded`/`materialized_skipped` omitempty, `warnings` omitempty). Quiet-gated summary lines still call `opts.progressf` inside the human callback; non-fatal missing-blob warnings ride `Warnings` (P7-CLI-01). Two stdout-purity fixes: `maybeRotateWorkspaceKey` and `runSyncCycle`'s `pushLocalEventsGated` call now write process diagnostics to `stderr` (matching hub compact's PR-B1 fix), with `stderr` threaded through `runSyncCycle` from both `sync` and `runLoopTick`. **`run` is a documented exemption, not a migration:** its `stdout` is the child's transparent stream (`runChildCommand`), so wrapping it in `Renderer` would either corrupt the child's real output or require buffering/discarding it — wire-it-or-reject-it chooses reject. Remaining part-B domains (if any) are still unwired as of this entry.

**Part B progress: final leaf-command batch wired, `pair` documented exempt — part B CLOSED (2026-07-16).** The eighth and final part-B batch migrated the remaining leaf commands to the `Renderer` seam: `init` (`initResult` — `dry_run`/`root`/`home`/`log_dir`/`state_db`/`workspace_name`/`join`/`workspace_id`/`adopted`, all omitempty; covers both the `--dry-run` preview and the real-run outcome), `add` (`addResult` — `path`, `remote`), `clone` (`cloneResult` — `path`, `remote`, `editor`/`open_hint` omitempty; built incrementally across clone's sequential add/materialize/open steps and rendered once, matching the `hubCompactResult`/`envRotateResult` multi-branch-build-then-render-once pattern), `hydrate` (`hydrateResult` — `path`), `open` (`openResult` — `path`, `editor`), `version` (`versionResult` — `version`, `commit`, `date`; required threading `*options` into `newVersionCommand`, which previously took only `stdout`), `service install`/`service uninstall` (`serviceInstallResult`/`serviceUninstallResult` — the existing stderr confirmation lines stay unchanged per `P7-CLI-03`; a new no-op-human-callback `opts.render(stdout, ...)` call is purely additive for `--json`), `up` (no new result type: **`up --json` inherits its single JSON document "for free" from its terminal `runSyncCycle` call**, the same free-inheritance principle `run-loop --once` established in PR-B7 — `up`'s own human-only closing summary is now explicitly gated behind `!opts.v.GetBool("json")` so it does not append plain text after `syncResult`'s JSON, a real stdout-purity bug this PR found and fixed), and `join` (`joinResult` — `workspace_id`, `founder_pinned`, `hub_configured`, `code`, `fingerprint`; unlike `up`, `join` calls no other self-rendering command, so it owns one render at the end of its `RunE`).

`init`/`up`/`join` needed one more architectural fix beyond a mechanical render swap: `runInit` (the shared core of `init`, and — via `initParams.calledFromUp`/`calledFromJoin` — of `up`/`join` too) must never call `opts.render` on the internal invocations, or `up --json`/`join --json` would emit a second, corrupting JSON document alongside `up`'s inherited `syncResult` / `join`'s own `joinResult`. The fix gates `runInit`'s tail on those two flags: under `--json` it returns silently (the outer caller owns the single document); otherwise it still prints its normal human-mode lines exactly as before, layered underneath `up`/`join`'s own human output. `internal/cli/leaf_render_test.go`'s `TestUpJSONInheritsSyncResult` is the regression guard for this — it decodes `up --json`'s stdout and asserts exactly one JSON document, with a `hub_id` key (proving it's `syncResult`) and no `workspace_name`/`adopted` keys (proving it is not `initResult`).

**`pair` is a second and final documented exemption:** like `run` in PR-B7, it is an interactive wizard that blocks on stdin with no coherent one-shot JSON contract to offer; forcing one would fight `P7-PROD-01`'s deliberate design. **This closes Part B and the `P5-CLI-01` finding**: every leaf command is now either wired to the `Renderer` seam or explicitly documented here as intentionally exempt (`run`, `pair` — two commands, both interactive/passthrough by design, not oversights).

## Machine contract surfaces

`P5-CLI-01` made `--json` uniform across leaf commands; this section names the subset an external program — an agent harness, a script, a CI step — may actually depend on as a stable machine contract, as opposed to output that merely happens to be `--json`-shaped today. The depended-on surfaces are: `worktree new`, `worktree list`, `worktree status`, `agent list`, `agent show`, `status`, and — since `W13-02` (2026-08-01) — `export` and `import`, whose `schema_version` lives in the emitted `workspace.yaml` itself as well as in their `--json` payloads.

`worktree new --json` (`AD5-01`) is the first of these to carry an explicit `schema_version` field (currently `1`) rather than relying on the shape being implicitly stable; `worktree status --json` carries one too since `AD5-07` (also `1`). The promise a `schema_version` makes is deliberately narrow, because a version number that promises too much is worse than none. It says: **every key documented at version N is still present, with the same name and meaning, at every version ≥ N.** It does *not* promise that no new keys appear — new keys may be added at any time **without** a bump, which is exactly why **a consumer must ignore keys it does not recognize** rather than treating them as an error. The version is therefore bumped only when a consumer written against the previous version would need to *change* to keep working — and since renaming or removing a shipped key is precisely that, such a change is a deliberate decision about breaking downstream programs, not a routine bump. Read the version as a floor on what you can rely on, never as a description of the exact payload.

`worktree list --json` deliberately carries **no** `schema_version`, and that is a shape constraint rather than an oversight: it emits a **top-level JSON array** of `state.Worktree` objects, and a version field needs an object to live on. Wrapping the array in an envelope to make room for one would *break* every existing consumer — the precise kind of change the additive-only rule above forbids — so the array stays, and the inconsistency with its sibling surfaces is deliberate rather than pending. A caller that wants a version for this surface reads it from a sibling call, or wraps the array itself for its own consumer. `TestWorktreeListJSONIsBareArray` enforces this, because the pre-existing testscript coverage greps for a key *inside* the payload and an envelope would not disturb it.

One wart in that surface is stated rather than implied, because it is the kind a consumer meets on its very first run: with **zero** worktrees the payload is `null`, not `[]`. `Store.ListWorktrees` returns a nil slice and `encoding/json` renders nil as `null`, so `for (const wt of JSON.parse(stdout))` throws on a fresh workspace. Normalizing nil to `[]` is a strictly friendlier shape and is a live candidate, but it is an **output change** and therefore its own decision, not a tidy-up to fold into unrelated work; `TestWorktreeListJSONEmptyIsNull` pins today's behavior so that the change, when made, is made deliberately. Until then, a consumer must treat `null` and `[]` alike.

Two invariants hold for every machine-contract surface, not only `worktree new`: stdout carries exactly one JSON document — all diagnostics, warnings, and progress text go to stderr, never interleaved into stdout — and the process exit code alone signals success or failure. A caller must never parse stdout text to decide whether to trust the payload; a zero exit code plus the single stdout document is the whole contract.

This follows two pieces of prior art rather than inventing a new convention: Terraform's machine-readable output carries a `format_version` that evolves via additive minor bumps, with consumers expected to ignore properties they don't recognize; and Cargo's `--format-version` flag must be passed explicitly by the caller precisely because an unversioned default output shape is a forward-compatibility hazard once the tool adds fields later.

## Command groups

```text
Implemented:
devstrap init [--join] [--workspace-id <id>]
devstrap up --hub <url>
devstrap join <pairing-code>
devstrap pair
devstrap version
devstrap scan
devstrap export
devstrap import
devstrap status
devstrap db
devstrap sync
devstrap open
devstrap hydrate
devstrap promote
devstrap add
devstrap clone
devstrap project
devstrap env
devstrap run
devstrap worktree
devstrap wip
devstrap agent
devstrap devices
devstrap conflicts
devstrap doctor
devstrap hub
devstrap materialize
devstrap run-loop
devstrap daemon
devstrap service
devstrap draft
devstrap completion

Planned:
devstrap ignore
devstrap export
devstrap import
devstrap promote
devstrap gitstate
```

## Initial commands

Current repository status as of `2026-07-01`:

```text
Implemented: devstrap init, up, join, pair, version, scan, export, import, add, clone, hydrate, promote, open, sync --hub-file, sync (hub: git+ssh://…/git@host:path.git zero-infrastructure git carrier — the documented default — and hub: r2://<bucket> production R2/S3 SDK wiring), hub init, hub compact, hub gc, hub login, hub logout, hub migrate-events, keys rotate, materialize, draft snapshot create, run-loop, daemon start, daemon stop, daemon status, daemon sync, daemon events, service install, service uninstall, service status, status, doctor, conflicts list, conflicts show, conflicts resolve, db migrate, db status, db backup, db backup --full, db restore, db down, env capture, env hydrate, env bind, env rotate, env op list, env op set, run, worktree new, worktree status, worktree finalize, worktree list, worktree remove, worktree cleanup, worktree unlock, wip push, wip fetch, wip status, wip show, wip apply, wip drop, agent run, agent list, agent show, agent pr, devices enroll, devices list, devices approve, devices revoke, devices lost, devices rename, devices recipient, devices pairing-code
Planned: env check, automatic remote device enrollment/fingerprint confirmation, the daemon job model and the endpoints beyond health/version, export, gitstate
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

`init` normalizes the effective root (`DEVSTRAP_ROOT`, `--root`, or positional `[root]`) to an absolute clean path, creates `~/.devstrap/config.yaml` with mode `0600` if missing, and does not overwrite an existing config file on same-root re-runs (re-running with `--join` against a pre-existing founder config warns that the config was not modified). If the store is already initialized under a different root, `init` refuses before `EnsureWorkspace` with `exitConflict`, names both the existing and requested roots, and points to `--move-root`; with `--move-root` it proceeds and rewrites `config.yaml` through a same-directory temp file plus rename (`0600`) so the config root and DB root agree afterward. It records `role: founder` (default) or `role: joiner` (`--join`) in the config and, per `P6-SEC-02`, **no longer mints a workspace key** — founding is deferred to the first `devstrap sync` and happens only against an empty hub (see `sync` below). `--workspace-id <id>` (P4-SEC-07 pairing) adopts the founder's `ws_<32 hex>` id instead of minting one so both devices read the same r2/s3 hub prefix; the shape is validated before anything is written, supplying an id implies `--join`, `--dry-run` prints the would-adopted id, and a store already initialized under a different id is **refused** (exit 2) with a remove-the-state-home-and-re-init remedy — there is no post-hoc id rewrite. `--code <devstrap-pair1:/devstrap-pair2:...>` is mutually exclusive with `--workspace-id`, decodes before filesystem writes, implies `--join`, adopts the carried workspace id, and enrolls the carried founder row after initialization: with `--fingerprint <fp>` it approves the founder only if the normalized value matches the fingerprint derived from the carried keys (mismatch fails before any write); with a TTY it prints the derived fingerprint and requires `yes`; with no TTY and no flag it keeps init scriptable by enrolling the founder as `pending` and printing the exact `devices approve <dev-id> --fingerprint <derived-fp>` follow-up. (The one-command `devstrap join` — below — wraps this path and additionally auto-trusts a v2 code's embedded fingerprint and auto-configures the carried hub.) Bare `--join` without `--workspace-id`/`--code` warns non-fatally that remote hubs (git carrier, r2/s3) key events by workspace id (flat file hubs are unaffected). `--join --code` prints the ceremony next steps first (`devices pairing-code` on each side, then `devices enroll --code ... --approve --fingerprint ...`); manual flags remain the documented fallback when `--code` was not passed. Default init prints the `Next: devstrap status • devstrap scan --adopt • set 'hub: git@github.com:<you>/<hub-repo>.git' (any private repo; or r2://<bucket>) in ~/.devstrap/config.yaml then devstrap sync` hint. `--scan` (`PROD-03`) runs the existing `scan --adopt` path inline after workspace creation so a populated root is adopted on the first command and prints the adopted count. Per `P6-CLI-05` (resolved) and the `AD-1` quickstart-default swap (2026-07-04), both hint forms teach the zero-infrastructure git carrier first (`hub: git@github.com:<you>/<hub-repo>.git`, any private repo) with `r2://<bucket>` as the scale-up alternative, rather than the file-backed `--hub-file` test hub — see the P6-CLI-05 section below.

### join

`devstrap join <pairing-code>` (P7-PROD-01 slice 1) is the one-command joiner side of the two-device pairing ceremony: on a fresh device it folds `init --join --code` (adopt the founder's workspace id + pin the founder), an automatic hub configuration from the code's carried **remote** hub URI, and generating this device's own pairing code for the founder to approve. It reuses `runInit` (not a re-shell; `initParams.pinnedFounderOut` reports back whether the founder actually ended up approved, so `join`'s summary never claims success when the founder is left pending), then writes the hub line via `rewriteConfigHub` and prints the local code via the shared `buildLocalPairingCode` helper. **Fingerprint trust:** a v2 code carries the founder's fingerprint, so `join` auto-trusts it by default (no prompt) — this trusts the paste channel, it is **not** cryptographic authentication (an in-transit rewriter regenerates a self-consistent fingerprint). `--fingerprint <fp>` enforces the high-assurance out-of-band compare (constant-time; refuses on mismatch before any filesystem write). A **v1** code (no embedded fingerprint) falls back to `init --join --code`'s existing TTY-prompt / non-TTY-pending behavior. **Hub:** when the code carries a remote hub URI (`r2://`, `s3://`, `git+ssh://`, `git@host:path`; v2, founder had a hub configured), `join` writes it to `config.yaml`; a **local** `file:`/`folder:` URI is reported but never auto-applied (`isLocalHubURI`) — the blob is unauthenticated, so a compromised paste channel must not be able to silently redirect a joiner's sync at an attacker-chosen local filesystem path, and the operator must run `devstrap hub init <url>` themselves to confirm it; when the code carries no hub at all, `join` says so and points at `devstrap hub init <url>` before the first sync — it never silently skips. The joiner's own `devstrap-pair2:` code prints to **stdout unconditionally** (the essential actionable result, never suppressed by `--quiet`, `P7-CLI-03`); the workspace-joined / hub-configured / fingerprint guidance is progress on stderr. `devstrap up` (founder bootstrap) and `devstrap pair` (guided founder wizard) — below — complete the P7-PROD-01 orchestration.

### up

`devstrap up [root] --hub <url>` (P7-PROD-01 slice 2) is the founder-side one-shot bootstrap for the FIRST device standing up a brand-new workspace: it folds `init` + `sync` into one command, plus an idempotent adopt pass and hub configuration. It refuses up front on a device that already joined an existing workspace (role `joiner`) — `up` founds a NEW one; use `sync`/`join` instead. `--hub` is required and accepts the same forms as the manual `hub:` config / `hub init` (a git carrier `git@host:path.git` / `git+ssh://…`, or `r2://<bucket>` / `s3://<bucket>`; `file:`/`folder:` are accepted too for local/test hubs, and a `file:`/`folder:` URI with no path is rejected at this preflight, not downstream in the hub backend); the URI is preflight-validated through the shared `hubConfigured` helper (which also rejects a git carrier's embedded credentials) BEFORE anything is founded, so a typo fails fast with no half-initialized state. `--scan` (default true) walks the root and adopts existing repos as a SEPARATE step after `init` using the same idempotent `adoptNewFindings` path `run-loop`'s per-tick scan uses (`runLoopScanAdopt`) — not `init`'s own one-shot `--scan`/`adoptFindings`, which re-stamps a fresh `project.added` event on every call and would duplicate events on a retried `up`. `--workspace-name` passes through to `init`; the root comes from the positional `[root]`, `--root`, or `DEVSTRAP_ROOT` exactly as `init` resolves it. `up` is a thin SEQUENTIAL orchestrator over the existing commands' internal logic (`runInit`, `runLoopScanAdopt`, `rewriteConfigHub`, `runSyncCycle`) — NOT a new atomic transaction: each step is already independently idempotent, so a failure at any step stops, reports which step failed, leaves the prior steps in place, and a re-run of `devstrap up` (or the single failed step, e.g. `devstrap sync`) continues from where it left off without duplicating anything — `up` never undoes a completed step. A hub-unreachable failure during the sync step surfaces sync's own error **unwrapped** (keeping its exit class). The final `sync` is what actually founds the workspace's key epoch on an empty hub (the `P6-SEC-02` founder gate lives in `runSyncCycle`). It closes with a summary naming the founded workspace id and pointing at `devstrap pair` for the second device.

### pair

`devstrap pair` (P7-PROD-01 slice 2) is the founder-side interactive wizard for the pairing *ceremony* — it assumes the local device is an already-founded workspace (it does NOT bootstrap; run `devstrap up`/`devstrap init` first). It refuses cleanly on an uninitialized store and refuses when the local role is `joiner` (the joiner's half is `devstrap join`, so it points there). It automates the founder's half of the ceremony: (1) print THIS device's `devstrap-pair2:` code + fingerprint via the shared `buildLocalPairingCode` helper (the code prints to **stdout unconditionally**, `P7-CLI-03`); (2) print the exact command the second device runs (`devstrap join '<code>'`); (3) block reading ONE line of stdin — the joiner's code pasted back — bounded by `--timeout` (default 15m; `0` waits indefinitely; a negative value is a usage error, not another way to spell "forever") and interruptible by SIGINT; (4) a blank/whitespace-only line, a bare EOF, or a timeout is treated as "not ready yet / finishing manually" and exits cleanly (exit 0) with the manual follow-up printed, never an error; (5) on a decoded code it first refuses a pasted code that names THIS SAME device (a fat-fingered paste of the founder's own code, printed just above the prompt) or a different workspace, then confirms + approves the joiner through the EXACT same `confirmDeviceFingerprint` path `devices enroll --approve` uses (a single shared stdin reader carries the pasted code and the "yes" confirmation); (6) `sync` publishes the key grant. "Blocks until it observes the peer's enrollment" is implemented as a blocking read of one stdin line from the operator (no new networking, no crypto). The stdin wait is on a goroutine with a buffered channel, `select`ing against a `time.NewTimer(--timeout)` and the command's context so a timeout/Ctrl-C actually interrupts the blocking read rather than hanging. **Interactive paste requires a real terminal**: a non-TTY invocation (script/CI) fails FAST with the manual-flow remedy (`devices pairing-code` / `devices enroll --code --approve` / `sync`) rather than hanging on input that never arrives — the TTY check reuses the same `stdinIsTerminal` detection `confirmDeviceFingerprint` uses. Nothing persistent is written before the paste is received and decoded (only the local device's own code is read, exactly like `devices pairing-code`). The closing summary names the workspace id and the approved device id, states that the founder's `sync` published the grant, and reminds the operator that the joiner still needs to run its own `devstrap sync` once to receive it.

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
- `status` prints schema version (currently **30** after `00030_device_wip.sql` added the working-state validation plane Layer B mirror table, `P7-WIP-01`; see `12_DATA_MODEL_SQLITE.md`), SQLite `quick_check`, and SQLite `foreign_key_check`;
- `backup` uses `VACUUM INTO`, not file copy;
- `backup --full` (`P6-DATA-04`, hardened by `P7-DATA-03/04`) writes a single mode-`0600` tar. The accepted `VACUUM INTO` snapshot is opened read-only and supplies blob refs, custody, device/workspace identity, and held WCK epochs; the existing bounded retry handles concurrent blob rotation/GC. Required key bytes resolve from the live custody backend using that snapshot inventory. Missing/unreadable/content-address-mismatched blobs or required keys are fatal and the partial archive is removed. A final `manifest.json` v1 records size/SHA-256 and required-set membership for every earlier entry. Under `--json` (`P7-CLI-01`), non-fatal warnings stay in the payload's `warnings` array;
- `restore [--force] [--allow-legacy] <archive>` (`P7-DATA-04/05`) stages extraction, rejects unsafe/non-regular entries, verifies manifest format/version, every entry hash/size, required-set membership, and absence of extra files, then read-only validates SQLite and cross-checks content-addressed blobs, current-device keys, and held-WCK files before any live swap. Pre-manifest archives require `--allow-legacy`, which warns and skips only manifest integrity. Promotion uses an atomically rewritten `.restore-journal.json`, one shared aside suffix, and durable per-target `done` markers: recovery rolls forward only when all targets are done and otherwise rolls back in reverse. `restore --recover` takes no archive; plain restore auto-recovers first. State opens fail closed and `doctor` reports the recovery remedy while a journal exists. A keychain-custody restore prints file-custody reconciliation guidance, and every `--json` path emits one result document;
- full backup, restore, `db down`, and each run-loop tick serialize through the state-home maintenance lock;
- state DB and backups are mode `0600`.

`doctor` (`PROD-02`) is a severity-graded health report: each check returns `{name, status: ok|warning|error, detail, remedy}`, rendered as a graded table with a summary line and a non-zero exit code when any check is error (so it can gate CI). Checks cover git/gh/go tools (git required, gh/go optional), state home + permissions, schema version, SQLite `quick_check`/`foreign_key_check`, dangling blob refs (`P6-DATA-04`: every `age_blob:` ref the DB holds must have its ciphertext present under `blobs/`; a missing one is an error whose remedy points at a `db backup --full` restore), secrets needing rotation, the recorded key-custody backend (`P6-XP-04`: a `key custody` row reporting `keychain` or `file`, warning when the recorded backend is currently unreachable, is being overridden by `DEVSTRAP_NO_KEYCHAIN`, or has not been recorded yet on a pre-`P6-XP-04` store), local age + Ed25519 device-key health, workspace keys awaiting grants (`P6-SEC-03`: each open `key_grant_waits` row with epoch/kid/first-seen and the re-approve remedy), the active workspace key's age against `keys.rotate_max_age` (`P4-SEC-07`: ok at epoch 0, warn past the deadline with the `keys rotate` remedy), an owed post-revoke workspace-key rotation (issue #134: warn while the machine-local `wck_rotation_pending` marker is set — a `devices revoke`/`lost` could not rotate the epoch, so events stay readable by the revoked device; remedy names sync's automatic retry and `keys rotate`; silent when nothing is owed), agent-run dead-PID reconciliation (`P6-GIT-06`: `agent run sweep` reports rows flipped from `running` to `interrupted` and the remaining running count), per-project git-state observation freshness (`P7-GITSTATE-01` CLI surfacing, `checkGitstateFreshness`: one `gitstate: <path>` row per local project — a warning "no device has reported git state for this project yet" when `device_gitstate` has zero rows for it, a warning naming the observing device when the newest observation is older than `gitstateStaleAfter` (7 days), otherwise ok with the observation age — mirroring `status --all-devices`'s never-silent-all-clear requirement from spec/07), and held repo locks (stale = warning). `--json` emits the check array; `--fix` applies safe remediations (create the missing state home, run pending migrations, clear stale repo locks) and re-runs the checks. `--remote` (`P5-PROD-05`) additionally probes the configured sync hub (reachability, pending push, queued deletes, device trust) and always reports a `workspace id` row (a warning row when the id is unreadable) so two devices can be compared directly; `--hub-file` selects the file-backed hub for that probe. For workspace-id-keyed remote hubs (R2/S3 and the git carrier), `--remote` also warns `workspace id match` when the local role is `joiner`, the pull cursor is still `0`, and the raw hub backend reports no events under this device's workspace-id prefix — the signature of a joiner reading its own empty `workspaces/<workspace_id>/...` prefix instead of the founder's populated prefix. The remedy text points operators to confirm the founder's workspace id with `devstrap doctor` on the founding device and re-init the joiner with `devstrap init --join --workspace-id <founder workspace id>` (the adoption flag shipped with the P4-SEC-07 pairing wave — see the `init` section above; this change ships the detection and regression-test side). `--remote` additionally reports a `retention manifest version` row (`P7-PROD-03`): it fetches the hub's retention manifest, reads its version stamp directly (bypassing the normal fail-closed parse, so it can explain a version-skew wedge instead of hitting it), and warns — never fails — when the manifest is behind what this binary produces, or when the manifest declares a signed `min_reader_version` this binary cannot satisfy; a hub with no retention manifest yet (no compaction has run) is silent, not a warning. See `07_NAMESPACE_AND_SYNC_MODEL.md`'s "Version-skew policy" for the N-1 read-window policy this check surfaces.

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
- `--quarantine` moves secret-looking files out of the managed tree into a dated `~/.devstrap/quarantine/<YYYYMMDD>/` directory (mode `0600`) instead of leaving them in place;
- emits `plain_folder` for local-only directories (`NOVCS-02`, shipped 2026-08-05), closing the local round trip with `promote`. A directory is classified only when it **groups nothing** — no git repo, recognized project, or materialization skeleton beneath it — and only the topmost of a nested run is recorded. The classification is therefore resolved *after* the walk, not inline: `WalkDir` is pre-order, so skipping a bare grouping directory such as `work/` on sight would hide `work/acme/api-server` underneath it. See `07_NAMESPACE_AND_SYNC_MODEL.md` for the policy and the two guards that keep a `plain_folder` finding from overwriting a tracked project's remote. `Adopted N projects` counts what was adopted, which is now less than the finding count whenever such a finding is dropped.

### status

```bash
devstrap status
devstrap status --json
devstrap status --watch [--interval 2s]
devstrap status --all-devices
devstrap status --prompt
```

`status --watch` re-renders the snapshot on an interval until interrupted.

`status --all-devices` (P7-GITSTATE-01 CLI surfacing) renders the working-state validation plane Layer A mirror instead of the regular snapshot: for every local project it reads `Store.DeviceGitstateForProject` and prints one row per device that has reported that project's git working-state (branch, dirty/untracked/unmerged/ahead/behind/stash counts, and an `Observed` column giving `last seen <duration> ago`, derived from the stored HLC), newest observation first. A project with zero rows in `device_gitstate` always gets one explicit `never synced` row instead of being left out of the output — spec/07's Layer A requirement is that this view **never** presents a silent all-clear. `--json` emits `[]projectGitstateStatus` (`{path, devices: [{device_id, branch, dirty_count, untracked_count, unmerged_count, ahead_count, behind_count, stash_count, observed}]}`). The flag does not compose with `--watch`. The producer is wired: every `devstrap sync` cycle captures each already-materialized `git_repo` project (see `### sync`), so a project reads `never synced` here only until its first post-materialization sync.

`status --prompt` (W12-01) is a distinct, terse renderer meant to sit inside a shell prompt segment: ONE line, purely from already-local mirror state (`ListProjects`' cached `dirty_state`, the workspace-wide `DeviceWipAll` pending-WIP count, and `CountOpenConflicts`) — no git shell-out, no network I/O, no `sync` trigger, so it stays comfortably under a prompt's latency budget. The contract is `"clean"` when nothing needs attention, or space-joined `key:count` segments in priority order — `dirty:N` (projects with a cached `dirty`/`diverged` state), `wip:N` (pending WIP refs across the workspace, from any device), `conflicts:N` (open conflicts) — omitting any segment that is zero. It never routes through `opts.render`/`--json`: the one-line text itself is the machine-parseable contract (in the spirit of `git status --porcelain`), and it does not compose with `--watch` or `--all-devices` (`cmd.MarkFlagsMutuallyExclusive`). `devstrap shell-init` (below) is the documented way to wire it into an interactive prompt.

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

### shell-init

```bash
devstrap shell-init bash
devstrap shell-init zsh
devstrap shell-init fish
```

`shell-init` (W12-01, `internal/shellhook`) prints eval-able shell code that wires `devstrap status --prompt` into an interactive prompt:

```bash
eval "$(devstrap shell-init zsh)"    # ~/.zshrc
eval "$(devstrap shell-init bash)"   # ~/.bashrc
devstrap shell-init fish | source    # ~/.config/fish/config.fish
```

Every emitted snippet WRAPS the shell's existing prompt-hook mechanism instead of replacing it — a hook installer that replaces an existing `precmd`/`PROMPT_COMMAND`/`fish_prompt` handler instead of appending to it is a well-documented breakage class (it broke Starship when direnv's tcsh hook overwrote `precmd`; Starship's fix was `USER_PRECMD`/`USER_POSTCMD` wrapping). Concretely: bash appends a function call to the `PROMPT_COMMAND` array (`PROMPT_COMMAND+=(...)`, relying on Bash ≥5.1 treating `PROMPT_COMMAND` as array-typed natively); zsh appends to `precmd_functions`; fish defines a new function bound via `--on-event fish_prompt` rather than redefining `fish_prompt` itself. Each installed hook sets `$DEVSTRAP_PROMPT` to the current `devstrap status --prompt` line on every prompt render, for the user's own `PS1`/`PROMPT`/`fish_prompt` to embed. This regression is guarded end-to-end by `cmd/devstrap/testdata/script/shell_init_wrap.txtar`, which seeds a pre-existing hook in each shell before sourcing the emitted snippet and asserts the pre-existing hook still fires afterward.

Starship users should keep Starship owning the prompt and add a [custom module](https://starship.rs/config/#custom-commands) pointed at `devstrap status --prompt` directly, rather than installing devstrap's own hook — see `docs/quickstart.md`'s "Shell integration" section for the recipe.

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

Before the push (in both the `--namespace-only` and full-materialize paths), each cycle also runs the working-state validation plane's Layer A capture (`P7-GITSTATE-01`): every already-materialized `git_repo` project gets a `repo.gitstate.observed` snapshot (`git status --porcelain=v2 --branch`), mirrored into this device's own `device_gitstate` row (so `status --all-devices`/`doctor` reflect it immediately, not only once a peer pulls the event) and queued as a signed event so it rides this cycle's push. A capture identical to the device's last-recorded mirror is not re-emitted, so an otherwise-idle sync still pushes zero events (`SYNC-04`). The capture step runs before eager materialization, so a project materialized for the first time in this same cycle is captured starting the *next* sync, not this one. A capture or mirror-write failure for one project records a non-fatal warning on that project (surfaced the same way as a materialize failure, `P4-GIT-07`) and never fails the cycle.

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
wip.gc_interval      # automatic post-materialization WIP sweep cadence; default 24h; 0 disables
wip.ttl              # minimum WIP-ref age; default 720h; 0 disables automatic sweep
staging.sweep_interval # automatic post-materialization clone-staging orphan sweep cadence; default 24h;
                      # 0 disables. Independent of wip.gc_interval (P11-SWEEP-01): the sweep bounds DISK
                      # GROWTH, so turning off the WIP recovery plane must not turn it off too. Parsed
                      # like the wip.* durations — negative/malformed values fail as invalid config, and
                      # that failure warns on stderr naming this sweep rather than being discarded.
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

Current implementation uses partial clone by default, supports `--full` and `--lfs`, refuses to clone into non-empty non-skeleton directories, stages clones in hidden sibling temp directories, promotes only after clone success plus a second target validation, preserves the original skeleton on clone failure, **names those staging directories from `ignore.StagingDirMarker` so the scanner's default prune set and the directory name cannot drift** (`W12-01`: a process killed before the promoting rename leaves a partial clone inside the managed namespace carrying the real remote, and until the pattern was added `scan --adopt` adopted it as a second project which then replicated fleet-wide — the marker sits mid-name, so a prefix match does not catch it), and updates local materialization/dirty state. The eager `materialize`/`sync` path additionally honors the stored `git_repos.lfs_policy` (`P6-GIT-04`): after clone, an `always`/`agent` repo runs `git lfs install --local` + `git lfs pull` (recorded **failed** on error, never available/clean with pointers), and `auto`/`never` warns — applied in `materializeGitRepo` so a `SkeletonProjects` retry of a failed repo cannot silently flip it to available (see `08_GIT_MATERIALIZATION_AND_WORKTREES.md`). The manual `--lfs` flag stays an explicit one-off pull. Planned (`DRAFT-*`): `hydrate` extends beyond `git_repo` projects to materialize `local_git`/`plain_folder`/draft content from decrypted `age_blob:<sha256>` bundles, while `node_modules`/build artifacts are rebuilt (npm/pnpm/uv install) rather than synced.

### promote

```bash
devstrap promote work/acme/notes --draft
devstrap promote work/acme/tool  --git-remote git@github.com:me/tool.git
```

Graduates a remote-less namespace entry (`NOVCS-03`, shipped 2026-08-01). Exactly
one of `--draft` / `--git-remote <url>` is required; both together is a usage
error (exit 10). The full type lattice, the refusals, and the ordering rationale
are canonical in `07_NAMESPACE_AND_SYNC_MODEL.md` § *Promotion*; the CLI-facing
contract is:

- `--draft` moves a `plain_folder` to `draft_project` and does nothing else —
  content is then the shipped `draft snapshot create` bundle plane's job, not a
  second bundling path. Re-running it on an already-`draft_project` entry is an
  idempotent no-op that emits no event.
- `--git-remote <url>` produces a `git_repo`. A `local_git`'s **existing history
  is pushed** (`git remote add origin` + `git push -u origin <current branch>`);
  a `plain_folder`/`draft_project` is `git init`-ed and given one initial commit
  first. `git init` is never run over an existing repository.
- The remote must be reachable and **empty**. A remote already holding refs
  exits `exitInvalidConfig` (2) naming `devstrap scan --adopt`; an unreadable remote exits
  `exitGit` (7) and says to create the empty repository first.
- The row and its `project.updated` event are written only **after** a
  successful push, and a failed push rolls the working tree back to its exact
  pre-command state, so retrying after fixing the remote is unobstructed.
- Refusals: a project that already has a remote (demotion is out of scope),
  `--draft` on a `local_git`, an empty folder, a folder whose staged index would
  carry secret-looking files or a **nested git repository** (staged as a gitlink
  whose objects the remote never receives — `P11-PROMOTE-03`), a folder holding
  an unusable `.git` node such as a dangling symlink (which `git init` would
  otherwise follow OUT of the managed root), a `local_git`
  that already has an `origin` or a detached HEAD or no commits, and any remote
  URL the shared `git.CanonicalRemoteKey` validator rejects. Every one of them
  names `devstrap scan --adopt` rather than `devstrap add`: `add` refuses a
  non-empty, non-skeleton directory, and each of these states has one
  (`P11-PROMOTE-01`).
- `--json` renders `{path, from_type, to_type, remote, branch, pushed, changed}`.

Only the CURRENT branch is pushed; other local branches stay local and are the
user's own `git push` away. Tags are not pushed either.

### run-loop

Each run-loop tick holds the shared state-home maintenance lock used by full backup, restore, and `db down`. Periodic mode prints `maintenance in progress; skipping this cycle` and succeeds when the lock is held; `--once` returns the conflict so schedulers can detect it.

`devstrap run-loop` is the portable, daemonless convergence loop: it runs **scan + sync + materialize** on an interval (`--interval`, default 5m; `--once` for cron/schedulers), identically on macOS and Linux. Progress/diagnostics go to stderr and the sync result stream to stdout (`P5-CLI-05`); a jittered backoff avoids hub stampedes and the loop aborts after 5 consecutive tick failures (`P5-CLI-05`/`P5-QUAL-03`). Per `P6-XP-03` the tick's **scan stage is real and idempotent** — it runs `scan.Walk` (offline since `P6-XP-05`) and adopts only genuinely-new findings (no active `ProjectByPath` row matching the finding's type and, for `git_repo`, `remote_key`), so a new local project reaches the hub without appending duplicate `project.added` events every tick. Warning-class findings (secret-looking files, symlink escapes) and duplicate-remote findings are surfaced on stderr and never auto-adopted; one-shot `scan --adopt` semantics are unchanged. Per `P4-HUB-16`, both this shared sync body and one-shot `devstrap sync` run a post-sync durability export when `hub_replica` is configured and the persisted last-success timestamp is at least `durability.export_interval` old (default `24h`, `0` disables; run-loop flag `--durability-export-interval` overrides for that process). The export copies the primary retention manifest's immutable sealed namespace/event-plane snapshot with `GetSnapshotObject` → `PutSnapshotObject`, then CAS-mirrors the signed retention manifest so the replica is directly snapshot-bootstrappable; it does not mirror env/draft `age_blob` content. No primary snapshot is an informational skip with a `hub compact` remedy. A replica transport/access failure warns and retries on a later sync without failing primary convergence, while malformed or same-as-primary replica configuration remains a hard error. Replica R2 credentials use `DEVSTRAP_HUB_REPLICA_S3_{ENDPOINT,REGION,ACCESS_KEY_ID,SECRET_ACCESS_KEY}` and never fall back to primary credentials.

### daemon

`devstrap daemon start|stop|status` (shipped 2026-07-24) is the CLI surface over the transport core described under *Local daemon API* below. The daemon is an optimization and **never a correctness dependency**: every command works with it absent, and `devstrap run-loop` remains the portable daemonless convergence path.

- `daemon start` runs **in the foreground** until interrupted (Ctrl-C) or terminated by its supervisor. Foreground is deliberate, not a missing feature: launchd and systemd both supervise a foreground process, and a self-daemonizing one would fight them (the same reason `run-loop` is foreground). It handles `SIGINT`/`SIGTERM` via `signal.NotifyContext`, so a supervisor stop drains in-flight requests and unlinks the socket. A second `start` against a live socket fails with the conflict exit class and a `daemon stop` remedy rather than displacing the running daemon.
- `daemon stop` signals the recorded process and then **waits for the daemon to actually be down** rather than reporting success on a delivered signal. "Down" means the daemon **released its record** (or its process is gone). The record is the primary signal on purpose: `daemon start` removes it only after its listener returns, i.e. after the graceful drain finished and every lock it held is released. The socket deliberately is **not** the signal — `http.Server.Shutdown` closes listeners and unlinks the socket *first*, then drains in-flight requests, so a socket check would report "stopped" at the start of the drain. Harmless while the surface is read-only; not harmless once a convergence tick holds the maintenance lock, where it would make `devstrap daemon stop && devstrap db restore` fail with a conflict from a daemon `stop` just declared stopped. Stopping when nothing is running is a **success no-op** (a supervisor or uninstall path must be able to call it unconditionally), and a record left by a crashed daemon is cleared rather than signalled.
- `daemon start`'s `--json` document is emitted **only after the socket is successfully bound**, so it never announces a start that failed. (An earlier draft rendered before binding, which made a losing second start print a success document — with a pid about to exit — while stderr reported the conflict.) It is a start-up announcement, not a result document; the process then runs until stopped.
- `daemon events` streams the daemon's event log until interrupted. It is the wave's **first genuinely daemon-only command**, and therefore the first caller to return the long-reserved `exitDaemonUnavailable` (3): every other command has a local path that works without a daemon, so returning "daemon unavailable" for them would be a regression rather than a feature. A live event stream has no daemonless equivalent, which is what makes the exit code meaningful here rather than merely available. Since `M5D-05` it **preflights `GET /v1/version`** before opening the stream, so an unsupported protocol is refused up front instead of producing events the CLI cannot interpret; that refusal is the typed protocol error and exits generic `1`, deliberately not `exitDaemonUnavailable` — a daemon that answered is not absent.
- `daemon sync [--namespace-only]` asks the running daemon to converge immediately and renders `{mode, started_at, duration_ms, coalesced}` through the shared `Renderer` seam. Human output distinguishes a cycle this caller started from a coalesced request that joined work already in progress. It is daemon-namespaced and daemon-required, so no listener maps to exit 3 like `daemon events`; a reachable daemon started without a converger instead gets a distinct message (mapped from the `503` on the route itself, and independent of whether that response carried a parseable body — an empty or truncated 503 still means "cannot converge"). That message deliberately prescribes no flag: `devstrap daemon start` always wires a converger, so a transport-only daemon is not a user misconfiguration but a programmatically constructed one (today, only a test harness), and offering a "restart it with …" remedy would send a user chasing a setting that was never wrong.
- `daemon status` exits 0 whatever the run state — the same contract `service status` has — and honors `--json` through the `Renderer` seam: `{socket, running, pid, version, uptime, detail}` plus, when the daemon answers, the convergence and watch health it reads off `/v1/health`: `{healthy, last_error, consecutive_failures, last_run_at, last_success_at, watch_backend, watch_degraded, watch_reason, watch_roots, watch_dirs}` (all omitempty). `watch_dirs` is a pointer-valued tri-state: absent means the backend cannot report recursively watched directories, while present `0` is a known zero. The **socket is the source of truth**: a daemon that answers is running whatever the pid file says; the pid file only enriches the report.
- **A protocol the CLI cannot interpret is REPORTED by `daemon status`, not returned as an error.** An unknown `api_version` sets `api_mismatch` in the JSON and prints a line naming both sides; the command still exits 0. This follows from the bullet above rather than contradicting it: an uninterpretable daemon is precisely the situation a user reaches for `status` to explain, so making it the one command that refuses to run would be backwards. Commands that must actually *speak* to the daemon (`daemon events`, `daemon sync`) do fail on it.
- **Running and converging are different questions, and `daemon status` answers both.** A supervised daemon never exits on convergence failure, so the supervisor's restart count — which *is* the failure signal for a run-loop unit — says nothing here: a daemon failing every cycle looks "running" to launchd/systemd indefinitely. The human output therefore prints `converging: FAILING (<n> consecutive; last error: …)` alongside the running line, and a degraded watch plane as `watch: degraded (<reason>)`. `healthy` is reported only when a `Converger` is wired, so a transport-only daemon does not claim to be failing at convergence it never performs.

**Single-instance guard.** The socket is the authoritative lock — only one process can bind it, and `Listen` refuses to displace a live one. `~/.devstrap/devstrapd.pid` records pid plus an opaque platform start-time identity for reporting and for `stop`; it is never the lock itself. The start-time identity is the same PID-reuse guard `repo_lock.go` applies — literally the same helper (`processIdentityAlive`), not a copy — so a recycled PID is never mistaken for the daemon and signalled. **Scope:** `ProcessStartTime` is implemented on darwin and linux only; elsewhere the recorded identity is `0` and the check degrades to liveness alone, leaving the PID-reuse guard inert on those platforms.

**Convergence (shipped 2026-07-24).** `daemon start` takes `--interval` (default 5m; `0` disables periodic convergence and leaves on-demand `/v1/sync` only), `--hub-file`, and `--namespace-only`, mirroring `run-loop`'s flags, and fails fast on an unresolvable hub *before* binding the socket — a daemon that starts and then fails every tick is worse than one that refuses with a clear message.

The daemon runs **no convergence code of its own**. `internal/daemon` declares a one-method `Converger` interface and `internal/cli`'s `cliConverger` implements it by calling the same `runLoopTick` that `devstrap run-loop` calls, so there is no second convergence path to drift (this is the whole of the `ARCH2-01` narrowing in `03_SYSTEM_ARCHITECTURE.md`).

**Single-flight, joining rather than queueing.** At most one cycle runs at a time, and a trigger arriving mid-cycle **joins** it instead of queueing another — a burst of triggers produces one convergence and N identical answers, each flagged `coalesced` so a caller can tell its request did not start the work it observed. A `full` request arriving during a `namespace-only` cycle is remembered and **promotes the next cycle**, so the materialization it asked for is deferred rather than dropped. This is the honest minimal subset of the job model below: two job shapes, single-flight, no persistence — the remaining designed job types stay design intent.

**Failure policy, deliberately unlike `run-loop`'s.** `run-loop` exits after five consecutive tick failures so its supervisor restarts it. A daemon must **not** exit on transient failure, because it is simultaneously serving reads and a restart loop would make those flap. It backs off exponentially instead (capped at 30m, so an hours-long outage still notices recovery promptly), keeps serving, and reports the state on `/v1/health`: `ok` stays `true` (the transport is alive) while `healthy` goes `false` with a scrubbed `last_error` and a `consecutive_failures` count. Periodic waits carry the same ≤10% jitter `run-loop` uses, for the same reason — unjittered fleet-wide intervals stampede the hub.

**`POST /v1/sync`** triggers a cycle (or joins one) and returns `{mode, started_at, duration_ms, coalesced}`. Its optional query parameter is a closed enum: `?mode=full` or `?mode=namespace-only`; absent or empty defaults to `full` for compatibility. Any other value returns `400`, names both accepted values, and performs no convergence — a typo must never silently cause repository cloning. There is deliberately no request body. A daemon started without a converger answers `503` rather than pretending to converge.

`Client.Sync(ctx, mode)` uses the same Unix-socket transport as the other client methods but a dedicated `http.Client` with no client timeout. A convergence cycle can spend minutes blobless-cloning and materializing repositories; using the shared 10-second client would cancel the HTTP request context and abort the daemon-side cycle mid-materialization. The caller's context is the only bound, matching the established `Events` streaming rule. The common request/response helper accepts the HTTP method and client so GET health/version/status retain their 10-second bound without duplicating decoding, unavailable mapping, or bounded error-body handling.

**A coalesced `daemon sync` must not claim work it did not do.** `Converge` is single-flight, so a `full` request arriving during a `namespace-only` cycle JOINS that cycle and returns *its* result: `coalesced`, `mode: namespace-only`, nothing materialized. The scheduler records the stronger mode and promotes the next cycle, so the request is queued rather than lost — but it has not happened when the command returns. Reporting that as a plain success would tell a script its full sync ran.

The command therefore retries **exactly once** after observing a weaker coalesced cycle: the recorded promotion means one retry is normally enough to claim the next cycle. A loop is deliberately not used — a busy watcher can keep starting `namespace-only` cycles indefinitely, and a command that spins until it wins is worse than one that reports honestly. If the retry still observes a weaker cycle, the result carries `requested_mode` and `deferred: true`, and the human line states plainly that the requested sync has *not* run and is queued for the next cycle.

This does not reverse PR #233's decision that core `devstrap status` must not silently prefer the daemon socket: doing so would change a core command's data path merely because a daemon happened to be running. `daemon sync` is different in kind, not an erosion of that boundary — its daemon namespace explicitly promises a daemon-required operation and exit 3 when none is listening, exactly like `daemon events`.

**Watch plane (shipped 2026-07-24).** The daemon is the first consumer of the `internal/platform` watcher, which had been built but wired to nothing since it was written (`PLAT-03`). It watches the **local paths of materialized projects**, not the workspace root: the root contains unmanaged trees too, and on kqueue every watched entry costs a file descriptor, so a blanket watch is both noisier and more expensive than the set of paths whose changes actually mean something.

Two invariants govern it, and both are load-bearing for the Milestone 5 entry gate:

1. **A hint never hydrates.** Watcher-driven convergence always runs `TickNamespaceOnly` — scan + adopt + sync, never materialization. An `FSEvent` carries only the watch root, so it cannot name the file that changed and there is nothing to hydrate *from*; running namespace-only makes that structural fact operational, so no filesystem activity — accidental or hostile — can cause DevStrap to clone repositories. Materialization stays on the periodic cycle and on explicit `POST /v1/sync`.
2. **The watcher is an optimization, never the guarantee.** If it fails, dies, or is unsupported, periodic convergence still runs and correctness is unaffected — only latency degrades. That is what licenses degrading rather than failing.

**Watch state and degrade path (`PLAT-02`, `M5D-04`).** `/v1/health` reports the watch plane as an explicit state: `starting` before roots have been resolved even once, `idle` for the entirely normal fresh-workspace state in which no projects are materialized yet, `watching` when the native watcher is armed, and `degraded` when the watch plane failed — the native watcher errored (the plane is then polling, or has no fallback), or the roots could not be resolved at all with nothing currently armed. `starting` earns its place rather than letting the field default to the empty string: the zero value would reintroduce on the wire exactly the ambiguity this field removes — a consumer could not distinguish "not started yet" from "never reported" — and the window is real, since resolving roots opens SQLite. The compatibility `degraded` boolean is derived from that state. `state`, `degraded`, and `roots` are always present on the wire — including `false` and `0` in the idle case — while `backend`, `reason`, `hints`, and `watched_dirs` remain conditional. `watched_dirs` is present only when the armed adapter implements the optional directory-counter seam; present `0` and absent are deliberately distinct. The idle reason is `no materialized projects yet`; it is status, not an alarm.

The plane re-reads materialized watch roots on its own 60s cadence, independently of periodic convergence (which may be disabled with `--interval 0`). A project materialized after daemon startup is therefore armed without a daemon restart. An unchanged sorted root set leaves the existing descriptors intact. A changed set re-arms them; a degraded plane also re-arms even when the set is unchanged, so every cadence retries the native watcher before falling back to `PollWatcher` again. A successful native retry clears the reason, restores the native backend, and moves the state back to `watching`. A degraded watcher does **not** make the daemon unhealthy — correctness rides on periodic convergence, not on the hints.

**Trigger floor.** The adapter debounces at ~250ms, but a debounce bounds burst-to-hint, not hint-to-convergence. A separate floor (`minTriggerInterval`, 5s) bounds how often hints convert to cycles, so a save-storm that outlasts one convergence cannot immediately start another. Hints arriving inside the floor are dropped rather than queued — a dropped hint costs at most one interval of latency, because periodic convergence is still running underneath.

**The `roots` count is how many PROJECTS are being watched, not how many directories.** `WatchRoots` returns one path per materialized project, and `addRecursiveWatch` then walks each one, so a workspace reporting `roots: 12` can hold thousands of kqueue descriptors. `watched_dirs` is the separate live recursively-watched **directory** total summed across those roots, exposed only by adapters that register per-directory watches. It deliberately does not claim to be a descriptor count: on the measured darwin proxy tree each watched directory cost ~6.8 descriptors. What `roots` is good for is confirming the plane is armed over the projects you expect; use `watched_dirs` for the FSEvents reconsider threshold.

**`GET /v1/status` and `GET /v1/events` (shipped 2026-07-25).** `/v1/status` serves the same workspace summary `devstrap status` prints, without the caller opening SQLite — the cheap read path a shell prompt, editor adapter, or TUI needs. It is backed by a narrow `Reader` seam, deliberately NOT a query API: a consumer wanting per-project detail opens the store itself, because making the daemon a database proxy would give it a second, drifting view of state it does not own. Without a `Reader` the endpoint answers `503` rather than an empty snapshot, which a caller could not distinguish from a genuinely empty workspace. Reader errors become a generic message — a store error string can carry a path or DSN.

`/v1/events` is a Server-Sent Events stream of a **closed set** of event kinds (`converge.started`, `converge.done`, `converge.failed`, `watch.degraded`), so no caller-supplied string ever becomes an event name and a consumer can switch exhaustively. Watch publication follows the same transition-oriented contract as convergence: entering `degraded` publishes `watch.degraded` exactly once, repeated failed retries while already degraded publish nothing, and recovery publishes the same kind once with detail stating that the plane recovered.

**Recovery is announced only after a probation, and that is what makes the previous sentence true.** There is no success signal to wait for — `platform.Watcher.Watch` blocks until it errors or is cancelled — so "armed" can only ever be inferred from "has not failed yet". Inferring it after a few milliseconds is right from a *healthy* state, but wrong when retrying from `degraded`: the motivating failure is descriptor exhaustion on a large tree, where the recursive walk adds thousands of watches before hitting the limit, far later than any short delay. A retry that claimed `watching` on that timer would publish a false recovery, then a fresh degrade when the walk finally failed — once per retry, forever, producing exactly the flapping this plane exists to eliminate. A retry from `degraded` therefore must survive **one full re-discovery interval** (capped at 30s) before it may claim recovery. Delaying recovery visibility by that much is a far better trade than a recurring false alarm, and nothing depends on announcing it sooner.

**The probation and the re-arm cadence would otherwise cancel each other, so the loop refuses to re-arm over a live probation.** A probation lasting one full interval necessarily reaches the next tick — and a degraded plane re-arms on every tick in order to retry native. Treating its own probationary retry as just another degraded arm to replace would tear it down and restart the probation forever: descriptors rebuilt every cadence, recovery never announced, the plane permanently reporting `degraded` while the watcher underneath is healthy. The re-arm decision therefore reads the phase *and* whether a probation is in flight together, and a probation is released the moment its outcome is known — not when the arm exits, which for a failed native arm is only after the polling fallback finally stops, and holding it that long would suppress every future native retry. Because a cancelled arm and its replacement overlap briefly, the probation is tracked by arm generation rather than a flag, so the outgoing arm cannot release the incoming arm's probation.

**A watcher that exits with no error and no cancellation degrades**, rather than leaving the plane reporting `watching` with nothing armed — a liveness lie no later tick would correct, since unchanged roots plus a non-degraded phase never re-arm.

**A failure to resolve roots does not tear down a healthy arm.** Resolving roots opens SQLite, so a busy-timeout blip says nothing about a watcher that is still watching the roots it was given; degrading on it would raise a false alarm *and*, because `degraded` forces a re-arm, rebuild every descriptor for no reason. The reason is logged and the phase left alone whenever an arm is still live. Reusing the existing kind keeps the closed set stable; consumers treat it as an instruction to refresh source-of-truth health, not as a level-triggered alarm. Failure detail is scrubbed before publication. The stream is **explicitly lossy**: each subscriber has a bounded queue and the publisher DROPS for anyone who falls behind rather than blocking, because convergence must never be slowed by a reader. Consumers must treat it as a notification channel, never as a log to reconstruct state from — `devstrap status` remains the source of truth. A 30s heartbeat comment keeps an idle stream from looking hung.

**Events are scoped to the CYCLE, not the request (`M5D-01`, 2026-07-28).** Convergence events are published by the scheduler, not by the HTTP handler. Three consequences, and each is the point:

- **Every trigger publishes.** A periodic tick and a watcher-driven cycle emit exactly what `POST /v1/sync` does. Publishing from the handler — as the first cut did — left the stream **inert on a normally-running daemon**, which is precisely the unattended `service install --daemon` case the stream exists to serve: only API-triggered cycles emitted, and nothing triggers the API on its own.
- **One cycle, one `started`, one terminal event.** `Converge` is single-flight, so N triggers arriving together produce one cycle; a joiner publishes nothing, because it did not start work. Handler-scoped publishing emitted one `started` per *caller*, and left an orphan `started` with no terminal event whenever a joiner's context was cancelled. A caller that wants to know it joined reads `coalesced` on its own response — that is per-request information and belongs there, not on a broadcast stream.
- **Terminal events publish under the scheduler's lock**, before the in-flight cycle is released. That is what orders a cycle's terminal event ahead of the next cycle's `started`; published after the unlock, `started(N+1)` can overtake `done(N)`, and an `Event` carries no cycle id with which a consumer could re-pair them. This is safe only because `eventBus.publish` is non-blocking and never calls back into the scheduler, so the lock order is strictly scheduler → bus. If that ever stops holding, the fix is a cycle id on the event, not a looser lock.

`converge.failed` carries a **scrubbed** detail, on the same terms as `last_error` — a convergence error can quote a hub URL.

**`last_success_at` on `/v1/health`.** `last_run_at` is the last cycle *attempted*, pass or fail. A consumer asking the operationally interesting question — "how stale is my data?" — cannot answer it from `last_run_at` alone, and pairing it with `healthy` disambiguates only while the daemon is currently failing. `last_success_at` records the last cycle that actually succeeded, and is absent until one has. It is the cycle's **start** time, not its completion time — deliberately, because that is the conservative bound: the data a cycle pulled is current as of when it started, so reporting completion would over-claim freshness by the length of the cycle. `devstrap daemon status` surfaces it in human output as well as JSON.

**Deliberately NOT done: existing commands do not silently prefer the daemon.** It would be easy to make `devstrap status` read through the socket when one is available and fall back otherwise. That is rejected: it changes a core command's data path based on whether a daemon happens to be running, which is how "works on my machine" divergence starts, and it buys nothing — the CLI opens the store perfectly well. The daemon's read path exists for *external* consumers that cannot afford a process spawn and a database open per shell prompt. `exitDaemonUnavailable` is therefore returned only by commands that genuinely cannot work without a daemon.

**Maintenance-lock interop.** The daemon does **not** hold the shared state-home maintenance lock for its lifetime: `runLoopTick` takes and releases it per cycle, exactly as `run-loop` does, so `db backup --full`, `db restore`, and `db down` keep working against a running daemon.

### service

`devstrap service install|uninstall|status` (`P4-PROD-04`) installs background convergence as an OS service so the workspace converges unattended: a per-user **launchd LaunchAgent** on macOS (label `com.devstrap.run-loop`, managed with the modern `launchctl bootstrap`/`bootout`/`print` verbs) and a **systemd `--user` service** on Linux (unit `devstrap-run-loop.service`). The OS branch lives entirely behind `internal/platform` — the CLI never reads `runtime.GOOS` — and the platform `ServiceManager.Install` **and** `Uninstall` return OS-idiomatic advisory notes (the Linux linger caveat; the headless unit-file-only removal note, `P7-XP-03`) that the CLI prints verbatim.

**Two supervision modes (`--daemon`, shipped 2026-07-25).** By default the unit runs `run-loop`. With `--daemon` it runs `daemon start` instead, adding the local socket API and the filesystem watcher. Nothing in the platform layer distinguishes them — `ServiceSpec.Args` is a plain argv rendered verbatim into `ProgramArguments`/`ExecStart` — so the mode is only which argv the CLI bakes, plus the unit description and log paths (`run-loop.{out,err}.log` vs `devstrapd.{out,err}.log`).

- **One label, both modes.** Both install under the same `DefaultLabel` (historically named for run-loop). That is deliberate: one label means one convergence service, so switching modes *replaces* the unit rather than leaving two of them converging against the same state home. Installing over a unit in the other mode prints an advisory naming both modes. The label therefore identifies the convergence service, not the mode it runs — `service status` reports the mode separately.
- **Restart semantics differ between the modes, and the difference is the point.** `RestartOnFailure` with a 30s delay exists because `run-loop` *exits* after five consecutive tick failures so its supervisor restarts it. `daemon start` never exits on convergence failure — it backs off internally to a 30m cap and keeps serving reads. So under `--daemon` a supervisor restart means a genuine **crash**, never a failed sync, and convergence health must be read from `/v1/health` (`healthy`, `consecutive_failures`, `last_error`) rather than inferred from the restart count: a daemon-backed unit that is "running" is not thereby converging successfully.
- The daemon runs **foreground** under both supervisors, which is what makes this reuse possible at all — launchd explicitly requires a supervised job not to `fork`+`exit` (it reads the double-fork as the process dying, then respawn-loops or gives up), and systemd's `Type=simple` expects the same. Socket activation is deliberately **not** used: an activated daemon must not unlink its socket, while `daemon.Listen`'s stale-socket takeover and its graceful-shutdown unlink both depend on owning it.
- **`devstrap daemon stop` against a *supervised* daemon leaves convergence off** until the next login or an explicit service restart. `daemon stop` shuts down gracefully, so the process exits 0, and both supervisors restart only on *failure* (`KeepAlive.SuccessfulExit=false` / `Restart=on-failure`) — deliberately, so an intentional stop is not fought by the supervisor. The consequence is worth stating plainly: hand-stopping a service-installed daemon does not hand convergence back to anything, it just stops it. `devstrap service uninstall` is the honest way to end it, and `doctor`'s installed-but-stopped warning is what surfaces the state in the meantime.

- `service install [--daemon] [--interval 5m] [--namespace-only] [--hub-file <path>] [--label <label>] [--exec-path <path>]` refuses up front when no hub is configured (same remedy as `run-loop`) and gates on key custody (`P7-XP-02`): file custody proceeds; recorded keychain custody is refused on systemd (`--allow-keychain-custody` overrides) and warns on launchd (locked-keychain risk); an unknown recorded custody value is refused as corrupt state (`exitInvalidConfig`, re-init remedy) rather than failing open; an install run with `DEVSTRAP_NO_KEYCHAIN=1` set bakes that explicit override into the unit env so the service matches the installing session. An explicit absolute `--exec-path` is baked **verbatim** (bypassing all resolution below), otherwise it resolves the binary path from `os.Executable()` and **refuses an ephemeral path** (a `$TMPDIR` or `go-build` resolution) with a hint to install to a stable location or pass `--exec-path <abs>`. Symlinks are resolved **except** when the invoked path sits in a stable install bin dir (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, Linuxbrew's `bin`, or a keg-only/versioned formula's `<brew prefix>/opt/<formula>/bin`): the symlink itself is baked so `brew upgrade` moving the Cellar target cannot brick the unit, and a path that still resolves into a `Cellar/` segment is refused with the same `--exec-path` remedy (`P7-XP-01`). It bakes `run-loop --interval <d>` — or `daemon start --interval <d>` under `--daemon` — plus `--namespace-only`/`--hub-file` and any explicitly-set `--home`/`--root`/`--config`, writes the plist/unit atomically at mode `0600`, and starts the service with `RestartOnFailure` throttled to 30s (coupled to `run-loop`'s consecutive-failure ceiling; see the mode note above for why that coupling does not hold under `--daemon`). Everything after the subcommand path is identical between the modes, which is the point: one convergence contract, two supervision shapes. No secret ever enters the service file — the CLI supplies at most the fixed non-secret custody flag above, and the adapters add only a `PATH`.
- `service uninstall [--label]` is idempotent (a not-installed service is a success no-op) and works headless (`P7-XP-03`): on Linux the unit file is removed even when the systemd `--user` manager is unreachable — best-effort `disable --now`/`daemon-reload` run only when it is — with an advisory note for the lingering-session case; a headless uninstall that removed nothing prints no note.
- `service status [--label]` reports `installed`/`running`/`mode`/`detail`/`unit` (honoring `--json` with `{manager,label,mode,installed,running,detail,unit_path,exec_path,exec_path_missing}`; `mode` is omitempty) and exits 0 regardless of run-state. The platform adapters best-effort parse the installed unit's launch binary **and its supervision mode** (`ProgramArguments[0]` and `[1..2]` / the un-quoted `ExecStart` words — our own rendered formats; a hand-mangled file degrades to an unknown ExecPath and an empty mode, never an error), yielding `"run-loop"`, `"daemon"`, or `""` for a unit we did not write, and flag `exec_path_missing` when that binary no longer exists, prefixing `ExecPath missing: <path>` to the detail (`P7-XP-05`). On an unsupported *platform* all three exit non-zero with a clear message; on a supported platform with an unreachable session manager, `install` still fails closed while `status` (unit-file stat) and `uninstall` (headless removal, above) keep working. `doctor` folds the same status in as an optional check (omitted when unsupported, ok when running or not-installed, a warning with an inspection remedy when installed-but-stopped, and a dedicated re-run-`service install` warning when the baked ExecPath is missing — e.g. after a `brew upgrade`; while a service is installed, an effective keychain-custody store additionally warns as `run-loop service custody` with the migrate/`--allow-keychain-custody` remedies, `P7-XP-02`).

### add

```bash
devstrap add git@github.com:acme/api.git --path work/acme/api --lfs-policy auto
devstrap add git@github.com:acme/monorepo.git --path work/acme/monorepo --sparse src,tools
```

Options:

```bash
--path
--default-branch
--lfs-policy auto|never|agent|always
--sparse dir1,dir2   # local-only, cone-mode sparse-checkout profile (W12-02)
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

## Project commands (`W12-02`)

```bash
devstrap project sparse set work/acme/monorepo src tools
devstrap project sparse list work/acme/monorepo
devstrap project sparse clear work/acme/monorepo
```

`project sparse` manages a project's cone-mode git sparse-checkout profile — see `08_GIT_MATERIALIZATION_AND_WORKTREES.md` § *Sparse checkout* for the full mechanism and its distinction from `.devstrapignore`. The profile is local-only (never synced through the event log), so `set`/`clear` immediately re-apply against an already-materialized checkout — under the project's repo operation lock, the same lock every other mutating git operation on the project holds — rather than only taking effect on the next sync; a not-yet-materialized project just persists the desired state, applied on the next `sync`/`hydrate`. `set <path> <dir1> [dir2...]` replaces the whole configured set (cone semantics only — a directory argument that looks like a non-cone glob pattern, is absolute, or contains `..` is refused by `internal/git.ValidSparsePath` before it reaches git). `set` additionally probes the requested directories against the tree at `HEAD` on an already-materialized checkout and **warns** (in `--json`, via the `warnings` array) when one does not resolve to a directory there — a typo such as `bakend` for `backend` otherwise narrows the tree to nothing silently (`W14-02`); it is a warning, never a refusal, since a directory can legitimately exist only on another branch. `list <path>` reads the configured set back, or reports "no sparse-checkout profile configured (full working tree)" for the default/unconfigured case. `clear <path>` removes the profile and restores a full working tree (`git sparse-checkout disable` on an already-materialized checkout). `devstrap add --sparse dir1,dir2` sets the initial profile at adopt time; `devstrap worktree new` applies the same project's profile to every fresh worktree it creates; `devstrap worktree adopt` does not apply it to an externally-created worktree and instead warns with the manual remedy. On a project's **first** materialization the profile is applied during the clone rather than after it (`W14-01`): a configured project clones `--no-checkout`, is narrowed in staging, and is then checked out, so the first checkout never fetches blobs it is about to prune — see `08_GIT_MATERIALIZATION_AND_WORKTREES.md` § *Sparse checkout* for the sequence, its submodule re-initialization step, and its never-fail-materialization fallback.

## Env commands

```bash
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local
devstrap env check work/acme/api
devstrap env bind work/acme/api .env.refs --provider 1password --profile acme-dev
devstrap env rotate work/acme/api
devstrap env op list --vault Engineering
devstrap env op set work/acme/api STRIPE_KEY sk_live_... --vault Engineering --item Stripe --field api_key
devstrap run work/acme/api -- uv run pytest
```

`env rotate` re-encrypts a project's captured env blobs to the current set of approved-device age recipients (dropping any device that was revoked/marked lost), emits the same synced `env.profile.updated` pointer as `env capture`, and clears the `needs_rotation` flag on the affected `secret_bindings` rows once the fresh ciphertext is written, so `doctor`'s rotation warnings converge. Provider-ref bindings (`op://`) hold no local plaintext and are marked rotated without re-encryption.

Current implementation supports `env capture`, `env hydrate`, `env bind`, `env rotate`, `env op list`, `env op set`, and top-level `run`. Capture parses a local env file with a non-interpolating grammar, refuses dangerous names, rejects interpolation-looking values unless `--literal` is passed, encrypts the bundle to the local plus approved remote device age recipients, writes a `0600` age blob under `~/.devstrap/blobs`, stores only `age_blob:<sha256>` references in `secret_bindings`, emits `env.profile.updated` in the same transaction as the row upsert, and appends the captured file path to project `.gitignore` when possible. Hydrate decrypts the cached age blob with the local device identity or resolves 1Password provider refs through `op inject`, writes only to an explicit `--write` target, creates the file atomically with mode `0600`, refuses to overwrite unless `--force` is passed, and appends the hydrated target to project `.gitignore` when possible; if the referenced encrypted blob is not cached yet, the error keeps the missing-file class and tells the operator to run `devstrap sync`. Bind stores 1Password `op://` provider refs without resolving plaintext and emits a provider-shaped `env.profile.updated`. `sync` pushes/pulls referenced env blobs through the same content-addressed blob plane as draft snapshots. `run` injects encrypted profiles directly into the subprocess environment or delegates provider refs to `op run --env-file <temp-refs-file> -- <command>`.

`env op` (`W12-03`, `internal/secrets/onepassword`) rounds out the 1Password adapter so refs no longer need to be hand-typed. `env op list` runs `op item list --format=json` then `op item get <id> --format=json` per item and prints every field as a copyable `op://<vault>/<item>/<field>` reference; it never passes `--reveal`, and the decoded `Field` type carries no value member, so a field's secret value has no reachable place to land even if the CLI's own JSON output happens to include one. `env op set <path> <key> <value-or-op-ref>` binds a `op://`-prefixed argument as-is (the same `bindProviderRefs` write path `env bind` uses, so browsing and hand-typing produce identical stored state); given a plaintext value instead, it writes that value into 1Password first — via a private, mode-`0600` JSON template file naming only the single field being changed, passed to `op item edit <item> --vault <vault> --template=<file>`, with the file's mode-`0700` temp directory removed before the call returns on every path — never as a bare `field=value` CLI argument (1Password's own CLI best-practices guidance: an inline assignment is visible in shell history and to other local processes). `-` reads the plaintext from stdin instead of argv. A brand-new key needs `--vault`/`--item` (and optional `--field`, defaulting to the key name); rotating an already-bound key's value needs none of them, defaulting to that key's existing `op://` binding. Both subcommands are gated on `op` being present on PATH, failing immediately with an actionable error naming the CLI (there is no manual-fallback degrade path here, unlike a missing forge CLI in `agent pr`).

## Worktree commands

```bash
devstrap worktree new work/acme/api --fresh-upstream --name fix-tests
devstrap worktree adopt /path/to/externally/created/worktree [--project work/acme/api] [--base-ref origin/gh-pages] [--allow-shallow]
devstrap worktree status wt_01jz...
devstrap worktree finalize wt_01jz... [--allow-stale-base]
devstrap worktree list
devstrap worktree remove wt_01jz... [--force] [--prune]
devstrap worktree cleanup --merged [--force] [--include-adopted]
devstrap worktree unlock work/acme/api [--force]
```

Current implementation requires `--fresh-upstream` for `worktree new`, fetches `origin/<default_branch>` before resolving the base SHA, writes a per-repo lock under `~/.devstrap/locks`, records worktree metadata, honors the stored LFS policy by either running `git lfs pull` or warning about pointer files, and refuses dirty worktree removal unless `--force` is explicit. `worktree new --json` (`AD5-01`) is a versioned machine contract, not the bare `state.Worktree` row: it additively embeds `state.Worktree` and adds `schema_version` (currently `1`), `project_path`, `remote_url` (userinfo-stripped, not placeholder-redacted, so the URL stays usable), `default_branch` (the branch name resolved off `base_ref`), `repo_path` (the main checkout's local path), and an `omitempty` `warnings` array reserved for future non-fatal notices — see "Machine contract surfaces" above for the evolution rules. `worktree remove --force` handles manually deleted worktree paths by running `git worktree prune` from the main checkout and marking the DB row removed. `worktree status <id>` re-fetches the recorded base ref and reports whether the worktree is fresh or stale; its `--json` payload carries `schema_version` (currently `1`, added by `AD5-07`) alongside the pre-existing `id`, `path`, `branch`, `base_ref`, `base_sha`, `current_sha`, `fresh`, `behind`, and `dirty_state` — none of which are `omitempty`, so a consumer always sees `behind: 0` and `fresh: false` rather than an absent key it must interpret as a default. `worktree finalize <id>` reuses the same stale-base check and exits non-zero if the base moved unless `--allow-stale-base` is set. `cleanup --merged` takes no positional args (`cobra.NoArgs`); removes clean, merged worktrees under the project repo lock (with a dirty re-check immediately before remove); skips any worktree that still has a running `agent_runs` row after a stale-PID sweep; prunes stale missing paths; reports a skipped count for unreadable, dirty, lock-contended, or live-agent worktrees; and only removes a merged-but-dirty worktree when `--force` is set. Merged-ness is ancestry OR content-equivalence (`P4-GIT-04`): the `git branch --merged` ancestry check runs first, and its misses consult `git.Runner.IsSquashMerged` — a current-tree merge probe (`git merge-tree --write-tree <base> <branch>`, git ≥ 2.38) that reports merged only when the simulated merge's tree is identical to the current base tree, catching GitHub "Squash and merge", rebase-, and cherry-pick-merges while correctly treating a merged-then-reverted change as NOT merged — with the conservative rule that a conflicting merge, an older git, or any error means NOT merged (a false positive would delete a real worktree and its branch). Reaps are labeled `merged` vs `merged (squash)`, the recorded `remote/branch` base is best-effort fetched under the repo lock first (warn + continue offline), reaped worktrees also get `git branch -D` (warn-only on failure), and forge-API (`gh pr list`) cross-checks are an explicit non-goal so cleanup works offline. `worktree unlock <path>` reports the holder of a project's repo operation lock and clears it when the holder is dead/stale (or when `--force` is set), providing a recovery path after a crash; `doctor` also lists held locks. The default-branch resolution for `worktree new` confirms the remote default authoritatively via `git ls-remote --symref origin HEAD`, repairing a missing `origin/HEAD` with `git remote set-head origin --auto` and warning if the result is not authoritative. `worktree new` also applies the project's configured cone-mode sparse-checkout profile (`W12-02`, see "Project commands" above and `08_GIT_MATERIALIZATION_AND_WORKTREES.md`) to the fresh worktree immediately after `git worktree add` succeeds, best-effort; `worktree adopt` does not apply it to an externally-created worktree and instead warns with the manual remedy. That warning is phrased as a statement about **DevStrap's own action** — "issued no sparse-checkout command" — and deliberately does not claim the adopted checkout is un-narrowed (`W14-06`): a real `git worktree add` against an already-narrowed repo inherits the active cone from git's shared repo-level config, so the resulting tree shape is git's to decide and is not knowable here. See `08_GIT_MATERIALIZATION_AND_WORKTREES.md` § *Sparse checkout*.

`worktree adopt <path>` (AD5-02) registers a linked worktree an agent harness (Claude Code, Cursor, Codex, Devin) created itself, so the registry, stale-base gate, and provenance stay valuable regardless of who created the checkout. It is strictly a *registration* plane, never a *base-resolution* plane (`spec/10` § *Independence from the cross-machine sync plane*): it records what a worktree was actually based on and never rewrites, repairs, or blesses that base, and nothing it stores is ever read by the fresh-worktree resolver. `git.Runner.WorktreeIdentity` (shared with `WorktreeSandboxWriteDirs`, which now derives its sandbox grant from the same resolver) classifies the path first; a main checkout or a non-worktree directory refuses with a distinguishing usage error. An unborn HEAD (no commits) refuses — there is nothing to merge-base against — while a **detached HEAD is adopted, not refused**: it is the common case for agent-harness worktrees, recorded with `branch=""`. The base ref defaults to `--base-ref`, or else `"origin/" + <resolved default branch>` (the same `resolveWorktreeDefaultBranch` helper `worktree new` uses, including its network-degrades-to-a-warning behavior); the recorded `base_sha` is `git.Runner.MergeBase(HEAD, base_ref)` — **never** the base ref's current tip, which would make every adopted worktree report "fresh" forever and would also corrupt `agent pr`'s `Committed since base:` diff. Two refs sharing no common history (`git.Runner.ErrNoMergeBase` — an orphan branch such as `gh-pages` is the common legitimate case) refuse with a usage error naming the `--base-ref` remedy. `--base-ref` is **shape-validated where it is recorded** (`P8-ADOPT-03`, 2026-07-31): it must be `<remote>/<branch>`, and `refs/devstrap/*` is refused outright. `MergeBase` accepts any committish, so an unvalidated value was previously stored verbatim and rejected only much later by `BaseDrift`'s split — with no remedy named — leaving the worktree adopted but permanently unusable by `worktree status`, `finalize`, and `agent pr`; `--base-ref main`, the natural mistake, hit exactly that. The `refs/devstrap/*` refusal is deliberate rather than incidental: nothing automatic reads the working-state plane (the `spec/10` independence invariant is intact), but an explicit flag should not be the one door into that separation. A shallow clone (`git.Runner.IsShallow`) refuses unless `--allow-shallow` is passed, since a grafted history can make the merge-base wrong at the shallow boundary; `--allow-shallow` adopts and records a warning. The project a worktree belongs to is resolved from its main checkout's path against every adopted project's local checkout path (or `--project <namespace-path>` to skip that inference); no match or an ambiguous match is a usage error naming the candidates, mirroring `wip show`'s multi-candidate refusal. Re-running `adopt` on the same path is idempotent: a row this command previously created (`created_by="adopted"`) has its `base_ref`/`base_sha`/`dirty_state` refreshed in place (`already_adopted: true`); a row created any other way (e.g. `worktree new`) is left completely unmutated (`already_registered: true`) — adoption never rewrites a base DevStrap itself resolved. `idx_worktrees_active_path` (`spec/12`, migration `00032`) enforces this at the DB level: at most one active row per `(namespace_id, path)`, with both `worktree new` and `worktree adopt` normalizing the stored path via `filepath.EvalSymlinks` so a `/var` vs `/private/var`-style alias cannot hide a real duplicate. Rows written **before** `00032` predate that normalization — `EvalSymlinks` arrived with `adopt` itself — so they hold whatever spelling the caller used, and the string-keyed index cannot alias-match them (`P8-ADOPT-07`, 2026-07-31). `adopt` therefore retries its lookup with the unresolved spelling and, on a hit, canonicalizes that row's **path only**. It routes such a row to the *reported-and-left-untouched* branch, never the idempotent-refresh one: every pre-`00032` row was written by `worktree new`, so refreshing one would rewrite a `base_sha` adoption did not resolve — the one thing this plane forbids. The retry matches string-equal spellings only, stated rather than implied; the complete form is an `os.SameFile` sweep, deliberately not built for a P3 that also self-heals via `cleanup`'s path-missing prune. `worktree cleanup --merged` skips every `created_by="adopted"` row unless `--include-adopted` is passed (a detached, branch-less adopted worktree is never treated as merge-eligible, regardless of that flag — merged-ness cannot even be evaluated without a branch); `worktree remove` on an adopted row deregisters only, leaving the checkout on disk, unless `--prune` is passed. `agent pr` refuses a branch-less (detached-HEAD) worktree up front with a `git switch -c <name>` remedy, before its network-fetching stale-base check runs — and **that remedy now actually works** (`P8-ADOPT-02`, 2026-07-31): `worktrees.branch` was written once at insert and never updated, so a user who followed the instruction hit the identical refusal forever. `UpdateWorktreeAdoption` refreshes the column and `agent pr` re-reads live HEAD before refusing, persisting what it finds. A remedy that cannot be acted on is worse than none, because it sends the caller in a circle rather than telling them they are stuck.

## WIP commands

```bash
devstrap wip push work/acme/api
devstrap wip fetch work/acme/api [--device <device_id>]
devstrap wip status work/acme/api
devstrap wip show work/acme/api [--device <device_id>]
devstrap wip apply work/acme/api [--device <device_id>]
devstrap wip drop work/acme/api [--device <device_id>]
```

`wip push` is the write half of the working-state validation plane's Layer B (`spec/07`, `repo.wip.pushed`): it runs `git stash create` to capture the working tree's uncommitted state as a commit object without touching the worktree or index, pushes that object to this device's own `refs/devstrap/wip/<device_id>/<path_key>` ref via a raw refspec (`git.Runner.PushRef`), and emits a signed `repo.wip.pushed` event carrying the ref, sha, base sha, and capture time — mirrored into the local `device_wip` table in the SAME transaction as the event insert, so the pushing device sees its own state immediately without waiting on a sync round-trip (`ApplyEvents` dedups a device's own events on pull-back, so relying on that path alone would leave the emitting device blind to its own push). **`git stash create` does NOT capture untracked files and has no `-u` form** (capturing them would require mutating the worktree, which this plane exists not to do), so `wip push` counts them separately and never conflates their absence with a clean tree (`P9-WIP-02`, 2026-07-31). A tree holding only new files renders "Nothing captured … and N untracked file(s) which WIP snapshots cannot capture" with the `git add` remedy, and a mixed tree still pushes but warns that the snapshot omits them — a push that silently omits new files is worse than a refusal, because the user believes their work is recoverable, and a brand-new uncommitted file is exactly what "forgot to push" usually means. `wip push --json` carries `untracked_not_captured` plus the `P7-CLI-01` `warnings` array. A genuinely clean working tree renders "Nothing to push...(working tree is clean)" and exits successfully with no event and no push; repeated pushes with no intervening commit are accepted ref churn (a new stash-create commit each time, even when content is identical) rather than deduplicated, since `wip push` is an explicit, single-shot user command. `wip fetch` is the read/transport half (`git.Runner.FetchRef`): with `--device <id>` it fetches exactly that device's canonically-derived ref (`refs/devstrap/wip/<device_id>/<path_key>` — a stored `Ref` string from a peer's mirror row is never trusted as the fetch target, only the device id + path_key derivation is); without `--device` it discovers candidate devices from the local `device_wip` mirror (populated by synced `repo.wip.pushed` events, never by network probing) and fetches each one's canonically-derived ref in turn. Both forms mirror refs into local git storage only — they never materialize, apply, or touch the working tree, and this plane is never read by the fresh-worktree resolver (`worktree new`'s base is always the fetched `origin/<default_branch>`). A ref missing on the remote (git's "couldn't find remote ref" class, `git.ErrBranchNotFound`) renders a clear "no WIP ref found" message instead of an error; an empty mirror renders "No pending WIP known for `<path>`" and still exits successfully — the same never-a-silent-all-clear convention as `worktree unlock`/`doctor`.

`wip status` and `wip show` are the read/inspect half of Layer B. `wip status <project>` reads the local `device_wip` mirror and renders one row per device with pending WIP (device id, ref, short sha, short base sha, and a "captured N ago" age) — an empty mirror renders a plain "No pending WIP for `<path>`" informational line, never a warning, matching `wip fetch`'s tone for the same zero-rows case, since most projects most of the time legitimately have nothing pending. `wip show <project> [--device <id>]` resolves a target row (an explicit `--device` that names an unknown device is a usage error; with no `--device`, exactly one pending row is used automatically, and more than one is a usage error listing the candidate device ids and asking the caller to pick with `--device`, never a silent guess at "newest"), fetches that device's canonically-derived ref (the same `wipRefFor`/`FetchRef` primitives `wip fetch` uses — a `git.ErrBranchNotFound` renders a clear "ref no longer exists on origin" message, not a crash), VERIFIES the fetched ref resolves to exactly the mirror row's sha (failing closed when the mutable ref has advanced past the last synced record, same as `wip apply` — see below), and displays its content without applying anything to the working tree or index. The diff is produced with `git diff <sha>^ <sha>` over the verified sha — against the stash commit's own first parent (HEAD at capture time) — rather than a plain `git show <ref>`: a `git stash create` commit is a merge commit (parents: HEAD, the index tree), so `git show` defaults to git's MERGE combined-diff format (`diff --cc`), which was empirically confirmed (against a real fixture repo) to render non-standard `@@@`-style hunks that can visually collapse content identical across parents; diffing against the first parent instead produces a normal unified diff of everything the working tree differed from HEAD by, staged and unstaged combined. `status --all-devices` and `doctor` also surface pending WIP: `status --all-devices` adds a compact per-project pending-WIP summary line alongside the existing Layer A gitstate columns, entirely absent for a project with nothing pending (unlike gitstate's forced "never synced" row, since zero pending WIP is the normal, healthy state); `doctor`'s pending-WIP check likewise renders nothing for a project with no or only recently-captured WIP, warning only once a ref is older than 48h (a shorter threshold than gitstate's freshness staleness, since a stash is meant to be resolved within a session or two, not merely observed periodically) with a remedy pointing at `wip show ... --device <id>` to review and apply or discard it.

`wip apply` and `wip drop` are the mutating half of Layer B, sharing `wip show`'s device-resolution logic (an unknown explicit `--device`, or an ambiguous 2+ candidate selection with no `--device`, refuse with the identical usage error). Both commands treat the synced `device_wip` mirror row as the authoritative record of what the (mutable, owner-force-pushed) remote ref is supposed to contain: `wip show` and `wip apply` fetch the resolved device's canonically-derived ref (the same `wipRefFor`/`FetchRef` primitives) and then VERIFY the fetched ref resolves to exactly the mirror row's sha, failing closed with "run `devstrap sync` to pull the newer record, then retry" when the remote has advanced past the last synced record — never showing or materializing content no locally-synced record describes. `wip apply <project> [--device <id>]` then runs `git stash apply <verified-sha>` directly against the working tree — deliberately with **no** pre-emptive dirty-tree gate and **no** `--force` flag, since git's own per-file conflict detection is more precise than any blanket refusal (an unrelated dirty file must not block a clean apply). A clean apply succeeds and leaves the WIP's changes as unstaged working-tree modifications ("Applied WIP for `<path>` from device `<id>` (`<sha>`)"); a real conflict is git refusing safely on its own, in one of two shapes distinguished via `git.Runner.DirtyState` after the failed apply: an outright abort with no changes made (git's own stderr is surfaced directly, since it is already clear and actionable), or a partial merge left with standard `<<<<<<</=======/>>>>>>>` conflict markers (`DirtyState` reports `DirtyConflicted`), reported as unresolved merge conflicts requiring manual resolution — devstrap never attempts automatic conflict resolution, an abort, or a reset; the tree is left exactly as git left it. `wip drop <project> [--device <id>]` deletes the resolved device's remote ref via a COMPARE-AND-DELETE: `git.Runner.DeleteRef` leases the empty-source refspec push to the mirror row's sha (explicit-value `--force-with-lease=<ref>:<sha>`), so a drop driven by a stale mirror can never destroy a newer recovery snapshot whose `repo.wip.pushed` event has not synced here yet. A lease rejection (`git.ErrNonFastForward`, "stale info") is disambiguated by asking the remote what it actually advertises now (`git.Runner.LsRemoteRef`): already gone → idempotent success (repeated drops of the same ref still succeed, reported identically — "Dropped WIP ref for `<path>` (device `<id>`)"); moved to an unknown sha → refuse with the same "run `devstrap sync`, then retry" guidance as apply. After the remote delete (or already-gone case), the command emits `repo.wip.dropped` and tombstones the owner's mirror row locally in the same transaction. Sync propagates that tombstone fleet-wide. Automatic TTL/GC **is shipped** — see § `wip gc` at the top of this file — and rides the convergence cycle (`P7-WIP-08`). (Corrected 2026-07-31, `P9-WIP-04`: this said "remains out of scope" while `wip gc` was documented in the same file.)

## Workspace manifest commands (`AD-7`)

```bash
devstrap export --manifest ~/workspace.yaml [--pinned]
devstrap import --manifest ~/workspace.yaml
```

`export` writes the plain-text workspace manifest — the escape hatch `spec/16` § *Durability / disaster-recovery drill (AD-7)* asks for, and the user-facing guide is `../docs/recovery.md`. The emitted document **is** a [`vcstool`](https://github.com/dirk-thomas/vcstool) `.repos` file: root key `repositories`, entries keyed by relative path, each `{type, url, version}`. That is the whole point rather than a stylistic choice — `db backup --full` is already a complete backup, but only DevStrap can restore it, so it is a backup and not an escape hatch. A format an unrelated binary consumes makes "recover without DevStrap" a checkable claim instead of an assertion, and `internal/manifest`'s `TestVCSToolImportsEmittedManifest` checks it by running the real tool against local bare repos (skipped when `vcstool` is absent; `TestGoldenStaysVCSToolCompatible` is the always-on structural gate, and it reads the golden file as a generic YAML tree rather than through this package's own structs, so it cannot validate the implementation against itself).

DevStrap's own metadata lives under **one** top-level `devstrap` key and is never interleaved into the entries vcstool parses. `vcstool` reads only `root["repositories"]` and only the `type`/`url`/`version` attributes inside each entry (`vcstool/commands/import_.py`, `get_repos_in_vcstool_format`), so a sibling top-level key is ignored by construction while an added attribute *inside* an entry would put DevStrap's evolution and vcstool's parser in one namespace forever. The manifest carries `devstrap.schema_version` under the additive-only rule above. Prior art was followed and its lessons taken deliberately: Android `repo`'s durable lesson (path and revision are separate concerns; do not become a checkout manager) and Zephyr `west`'s (manifest *imports* complicate the mental model) — **there are no manifest imports.**

**The interop claim is scoped, in the spec and in the emitted file's own header.** It holds for `git_repo` projects only. `local_git`, `plain_folder` and `draft_project` rows have no `{url, version}`, so they are recorded under `devstrap.projects` and are structurally invisible to `vcs import`; their content lives in age-encrypted draft bundles and is recoverable only through DevStrap by design. That subset is the bulk of the DR value, but the artifact must not imply whole-tree third-party recovery, so the header states the limit where a user reads it mid-disaster rather than only in this corpus.

`--pinned` records each repository's resolved `HEAD` instead of its branch name, mirroring `vcs export --exact`, because a branch name is not a recovery artifact. When HEAD cannot be resolved — the project was never materialized on this device — the entry's `version` is **omitted** rather than degraded back to a branch: vcstool then clones the remote default, and a file that says `pinned: true` never claims a pin it does not have. The same omission covers a HEAD that resolves but is **not reachable from the remote this entry's `url` names**: an unpushed commit, a local-only topic branch, or — the `P11-MANIFEST-01` case — a commit that lives on a *different* configured remote. The last one is why the check is scoped per-remote rather than asking `git branch -r --contains` about every remote at once: with an empty fork as `origin` and the canonical repo as `upstream`, "on some remote" is true while "on the url this entry records" is false, and `vcs import` would clone the fork and fail its checkout during the actual recovery. See `08_GIT_MATERIALIZATION_AND_WORKTREES.md` § *`RemoteTrackingContains`* for the scoping rules and the both-directions staleness caveat on remote-tracking refs. The project is named in a stderr warning and in the `--json` `warnings` array. Remote URLs are exported through `redact.StripURLUserinfo`, so an embedded https credential never reaches a plaintext artifact meant to be copied off the machine, while an `ssh://git@…` login name survives and the URL stays usable. The file is written atomically at `0600`.

`import` is a **registration** plane, never a materialization plane: it writes namespace rows plus their `project.added` events — exactly as `devstrap add` and `scan --adopt` do, so a recovered namespace propagates to the fleet like any other — and stops. `devstrap sync`/`materialize` then clones through the one existing path. A second cloning path is precisely what this file already refused for `/v1/status` and what `AD5-07`'s acceptance criterion forbids. It deliberately does not reuse `adoptFindings`: that path claims `materialization_state: available` because a scan just observed the checkout, and import observed nothing. Import is idempotent (an identical already-registered project reports `already_present`) and **never overwrites** a project registered differently — a stale manifest must not be able to silently rewrite a live remote — reporting that as a skip. That refusal reads inside the transaction it writes in (`P11-MANIFEST-02`): the existence check runs on the `Tx`, so the write lock `WithTx` takes at `BEGIN` covers check and upsert alike and no concurrent writer — another process, or a service-installed daemon converging a peer's `project.added` — can create the row in between and be overwritten by `UpsertProject`'s `ON CONFLICT DO UPDATE`. A manifest is hand-editable plain text and, after a total local loss, may be the only input left, so import also **validates what it persists** (`P11-MANIFEST-03`) rather than deferring a certain failure to materialize time: `lfs_policy` and `default_branch` are checked with the same validators the rest of the binary uses, and a bad value skips that entry instead of registering a row that fails later on whichever project first happens to trigger an LFS or fetch operation. A skipped entry (a non-git `type`, an unsafe namespace path, a `git_repo` with no `repositories` entry, an unusable `lfs_policy` or `default_branch`, a conflict) makes the command exit non-zero while still registering everything else, mirroring `ErrPartialMaterialize` rather than reporting a whole recovery; a file that is not a manifest at all exits `exitInvalidConfig`. A manifest declaring a **higher** `schema_version` is read with a warning, not refused — under additive-only evolution every key this binary knows still means the same thing, and refusing would break recovery exactly when it matters. A bare `.repos` file with no `devstrap` key imports as all-`git_repo`, adopting each `version` as the default branch unless the manifest is pinned, the value is plainly a 40-hex commit id, or it is not a usable branch name. Those three are best-effort *declines* of a heuristic, not skips: the row still registers and materialize resolves its default branch from the remote.

## Agent commands

```bash
devstrap agent run work/acme/api --engine generic --task "fix failing tests" -- npm test
devstrap agent adopt work/acme/api --engine claude-code --task "fix failing tests" --adopt-worktree [--allow-shallow]
devstrap agent finish arun_01jz... --status complete --test-summary "npm test: 42 passed"
devstrap agent list
devstrap agent show arun_01jz...
devstrap agent pr arun_01jz...
devstrap agent cleanup --merged
```

**PID-reuse guard (`P7-GIT-03`, 2026-07-11):** new runs record both the recorder PID and an opaque platform start-time identity. The list/show/pr/doctor sweep treats a live PID with a different start identity as a recycled PID and interrupts the crashed run; legacy rows with no identity retain PID-only behavior, and an identity lookup error is conservatively treated as indeterminate/alive.

**`agent adopt`/`agent finish` (`AD5-03`, 2026-07-31):** these register and terminate an agent run for a session a real harness (Claude Code, Cursor, Codex) ran itself, so `agent list`, the stale-base gate, and `agent pr` keep their value when `devstrap agent run` did not spawn the process. `agent adopt <worktree-path-or-id> --engine <name> --task <text> [--pid <n>] [--log <path>] [--adopt-worktree] [--allow-shallow] [--project <p>] [--base-ref <ref>]` resolves the target — a `worktrees.id` is used directly, otherwise the argument is treated as a path and looked up (normalized identically to `worktree adopt`) among already-registered worktrees; with no matching row, `--adopt-worktree` registers one first via the exact same resolve-and-register code path `worktree adopt` uses (`adoptWorktreeAt`), while its absence refuses with a usage error naming `--adopt-worktree` as the remedy. `--engine` and `--task` are required and `--engine` is deliberately unvalidated free text — DevStrap is the registry, not a per-harness adapter gatekeeper. `--allow-shallow` is accepted **only** alongside `--adopt-worktree`, since it reaches worktree adoption and nothing else; passing it on its own is a usage error rather than a silently-inert flag, because a flag that is accepted but does nothing reads as "shallow was allowed" and is precisely how a caller concludes a later refusal is a bug rather than a policy. **`--pid` has no default and must never be guessed:** a harness that shells out to run this command has a PPID that is a transient shell about to exit, so defaulting to `os.Getppid()`/`os.Getpid()` would flip a healthy run to `interrupted` the moment that shell exits. Passing `--pid <n>` records both `n` and the start-time identity of *that* pid (the same PID-reuse guard `P7-GIT-03` uses elsewhere), so the run participates in the normal dead-process sweep; omitting `--pid` records neither, which has two concrete consequences worth knowing before you skip it: the run is **never** swept by `RunningAgentRunsWithPID` (a crashed pidless run stays `running` forever until `agent finish` closes it), and `worktree cleanup` skips any worktree that still has a `running` run, so a forgotten pidless run blocks that worktree's cleanup indefinitely — `agent finish` exists specifically to close out that run when the harness itself, not a PID sweep, is the authority on completion. The `agent_runs` insert is performed **under the project repo lock** (`P8-ADOPT-04`, 2026-07-31), the same `P7-GIT-01/02` discipline `agent run` observes by holding it from worktree creation through `InsertAgentRun`: between provisioning and registration a concurrent `worktree cleanup --merged` could otherwise reap the very worktree the run was about to bind to, since a freshly-provisioned worktree has tip == `base_sha` and `git branch --merged` reports it merged. A worktree created outside DevStrap and never registered remains reap-eligible — no lock can cover a row that does not exist yet — which is why `docs/agents.md` tells harnesses to call `agent adopt` promptly. `agent finish <run-id> [--status complete|failed] [--test-summary <text>]` (default `--status complete`) records the harness's own terminal report: `running → complete/failed` and `interrupted → failed` transition silently; `interrupted → complete` is allowed but prints one warning to stderr, since the sweep's "recorder died" inference is weaker evidence than the harness's own explicit report, so the harness wins but the disagreement is surfaced rather than hidden; finishing an already-`complete`/`failed` run refuses (the run is over, and `finish` is not idempotent). The run's diff summary is recomputed and persisted the same way `agent run` does at completion, **unconditionally** (`P8-ADOPT-06`, 2026-07-31). It was previously gated behind `--test-summary`, which meant any run finished without that flag kept a permanently empty diff in `agent show` and in its PR body — even though `finish` is deliberately non-idempotent and is therefore the only chance to capture it for an adopted run, which never had one set at insert time. The forge base branch is derived by cutting `base_ref` at its first separator rather than trimming a hardcoded `origin/` prefix (`P8-ADOPT-03`), so a fork-workflow base such as `upstream/main` no longer reaches the forge verbatim as a branch name it has never heard of; this matches how `BaseDrift` parses the same string. After `agent finish --status complete`, `agent pr` works with no `--allow-incomplete`, exactly as it does for a run `agent run` completed itself.

Current implementation supports `agent run/adopt/finish/list/show/pr`. `agent run` creates a fresh upstream worktree, runs an explicit generic command with a sanitized no-secret default environment, applies wrapper-level command and file path policy (`readonly`, `cautious`, `guarded`, or explicit `yolo-local`), records the run in SQLite with `status='running'`, its recorder PID, and sandbox backend/mode/limitations, captures a `0600` log, and stores a labeled Git diff summary: `Committed since base:` diffs the recorded `BaseSHA` against `HEAD`, while `Uncommitted:` records `git status --short` residue. The file path policy denies explicit sensitive-path and outside-worktree references for non-`yolo-local` runs; it is a preflight wrapper policy. **On macOS and Linux the run is additionally OS-sandboxed** (`P4-GIT-03`, 2026-07-05): `--sandbox auto|off|require` (env `DEVSTRAP_SANDBOX`, default `auto`) wraps the child argv in `/usr/bin/sandbox-exec` with a generated Seatbelt profile on macOS (whose credential-read denies resolve each anchor's leaf symlinks and deny both the literal alias and its kernel-real target, so a `~/.ssh -> /elsewhere` symlink cannot dodge the deny); on Linux the adapter lazily probes and selects bubblewrap, then Landlock, then unsupported. `DEVSTRAP_SANDBOX_BACKEND=bwrap|landlock` forces a Linux backend, a forced backend never silently falls through to the other one, and an invalid value is an explicit-config error that fails closed in every mode (a typo must never silently disable the sandbox). Bubblewrap provides the full Linux backend (read-only root, read-write worktree/tmp binds, credential masks, optional net namespace, user namespace, pid namespace, die-with-parent, and new-session protections); its credential masks are dropped only when a masked path genuinely does not exist, while permission-denied, symlink loop, or I/O errors keep the literal mask so a credential is never left readable through a resolution failure. Landlock is the layered fallback: it is a real kernel write-confinement boundary, but it is additive-allow, so `agent run` prints one `notice: OS sandbox landlock active with reduced guarantees: ...` line documenting that credential reads are NOT denied, network deny is TCP bind/connect only at Landlock ABI >= 4 (not enforced below that), and mount/pid namespace guarantees are absent. `auto` degrades to one loud warning only when no backend can run; `require` refuses to run unsandboxed with the policy exit class before any worktree is created, and also refuses `readonly`/`cautious` when the selected backend cannot enforce their network deny at all; a Landlock TCP-only deny (ABI >= 4) satisfies `require` but prints a warning that UDP, QUIC, and unix-domain sockets stay open; `yolo-local` is unconfined and conflicts with `require`. Both Linux backends also install a seccomp syscall denylist (mount, kexec/module, ptrace/tracing, keyring, io_uring, and legacy-escape syscalls return `EPERM`; `clone`/`unshare`/`setns`/`execve`/`fork` stay allowed): bubblewrap reads the compiled cBPF filter from an inherited fd (`--seccomp`), and the Landlock shim loads it in-process after the ruleset and before `execve`. It is unconditional hardening for every sandboxed policy and is compiled for the running arch (x86-only names are dropped on arm64). `DEVSTRAP_SANDBOX_SECCOMP=off` disables it (a mistyped value fails closed with the invalid-config exit class), and a kernel without seccomp-filter support degrades to a reduced-guarantees notice rather than failing `require`. macOS Seatbelt profiles now embed a per-run denial tag, and after the run DevStrap best-effort reads matching unified-log rows into the unsigned local `sandbox_violations` table with scrubbed path/detail fields. `agent show` prints a sandbox line plus violation count/details, and `agent show --json` returns the `AgentRun` fields plus a `violations` array; `doctor` warns when any run has recorded denials. Linux runtime denial detection remains future, so Linux runs populate backend/mode/limitations but not violation rows. **Tighter read confinement** is opt-in via `--read-confine auto|on|off` (env `DEVSTRAP_SANDBOX_READ_CONFINE`, default `auto` = on for the `readonly` policy only; `--read-allow <abs>` adds roots): all three backends restrict the child's reads to the worktree/tmp, the OS toolchain/system roots, and the `$HOME` build caches instead of the whole disk, so the rest of `$HOME` and other projects are unreadable. An explicit `--read-confine on` refuses to launch when the backend cannot enforce it — including when no OS sandbox is available at all — while an `auto`-derived request degrades to a warning. A `--read-allow` root that overlaps a protected credential path is refused (read confinement drops bwrap's credential masks and Landlock cannot subtract from an allowed root, so such a root would silently re-expose the credential). Since `P8-SEC-02` (2026-07-31) the write grant for a linked worktree's git storage additionally **denies** `commondir`, `gitdir`, and `config.worktree` inside the per-worktree admin dir (`SandboxSpec.GitDenyFiles`): granting that dir wholesale let a sandboxed agent repoint `commondir` at attacker-controlled space so the next UNSANDBOXED git command in the worktree executed a planted hook/fsmonitor. Seatbelt orders the deny AFTER the allow (SBPL is last-match-wins); bubblewrap `--ro-bind-try`s the files after the rw bind; Landlock cannot subtract a child from a granted directory and records the gap in its `Limitations()` instead. The hidden `sandbox-helper` command is internal to the Landlock backend: it re-execs the real binary, applies Landlock to its own process, then `execve()`s the agent argv in the same PID; exit 125 means the shim failed and is surfaced by the parent as 225 via `childExitBase`. `agent list`, `agent show`, `agent pr`, and `doctor` reconcile `running` rows whose recorded PID is confirmed dead to `interrupted`; that status means the run was still `running` when its recording process exited or crashed. `agent pr` refuses any run whose status is not `complete` unless `--allow-incomplete` is passed (then it warns to stderr), refuses stale recorded bases unless `--allow-stale-base` is passed, pushes the agent branch, and creates a PR/MR through the detected forge CLI (`gh`/`glab`/`tea`) when available; unsupported forges get the pushed branch and compare URL instead of a failed hardcoded GitHub path. SSH host aliases resolve via `ssh -G` with a config-file fallback; the forge-resolution chain (`ResolveForge`/`DetectForge`/`resolveForgeHost`/`resolveSSHHostAlias`) threads the caller's context so the bounded `ssh -G` timeout derives from it rather than a fresh `context.Background()` (`P4-QUAL-07` — the `contextcheck` linter is enabled and enforces this); that resolution is exercised hermetically in tests through a PATH-shimmed `ssh` stub, never the developer machine's OpenSSH config (`P6-QUAL-04`). The `gh`/`glab`/`tea` PR-creation invocation itself — the exact argv shape per forge, the self-hosted-override precedence path, graceful degradation with a compare URL when the CLI is missing, and the stderr-wrapped `appError` on a nonzero exit — is likewise exercised hermetically through PATH-shimmed forge-CLI stubs rather than the developer machine's installed tools, as is `doctor`'s per-remote forge-CLI presence check (`FORGE-05`). `--dry-run` reports the planned PR without pushing. `agent cleanup` remains future work; non-generic engines are **withdrawn, not pending** (`AD5-05`) — `agent run` ships exactly one wrapper engine, `generic`, and every other harness is supported by the substrate primitives (`worktree new --json`, `worktree adopt`, `agent adopt`) rather than by a per-harness adapter, so `engine` is a free-text label the caller supplies and DevStrap never validates it against a list; project-env allowlists shipped 2026-07-17 (an opt-in project-root `.devstrapagent.yml` parsed by `internal/agentsecrets` filtering which captured env-profile secrets reach an `agent run` subprocess, deny-wins-on-conflict, byte-identical behavior for projects with no config file).

## MCP server (`AD5-07`)

```bash
devstrap mcp serve
```

Serves five tools over stdio MCP: `devstrap_worktree_new`, `devstrap_worktree_adopt`,
`devstrap_worktree_status`, `devstrap_worktree_list`, and `devstrap_agent_adopt` — one per primitive
in `10_AGENT_WORKSPACES_AND_POLICIES.md` § *DevStrap as the substrate agents run on*. `AD5-07`'s
acceptance criterion is that this surface introduces **no second execution path**: every tool handler
(`internal/cli/mcp_serve.go`) calls the exact same internal Go function its cobra command calls —
`createFreshWorktree`, `adoptWorktreeAt`, `statusWorktree`, `listWorktrees`, `adoptAgentRun`, the
same functions `AD5-07`'s prerequisite PR extracted out of their `RunE` closures for this reason.
Input validation (required fields, `--allow-shallow` requiring worktree adoption, a non-negative
`pid`) is re-checked in the handler rather than trusted from the wire, mirroring the duplication the
CLI itself already carries between `RunE` and `adoptAgentRun` for the identical reason: a non-CLI
caller must not be able to write a blank-`engine` `running` row into the provenance registry.

Tool names are service-prefixed (`devstrap_`) rather than bare, because a server loaded alongside
three to five others makes `worktree_new` a name another tool will also want. `devstrap_worktree_list`
wraps `listWorktrees`' bare `[]state.Worktree` in its own versioned envelope
(`{schema_version, worktrees}`) — a decision deliberately independent of `worktree list --json`, which
stays a bare array (see "Machine contract surfaces" above): a tool call is a different consumer with
no existing-consumer history to break, so it is free to start versioned where the CLI flag is not.

**No authentication.** The local stdio subprocess boundary IS the trust boundary, matching the
precedent of `docker agent serve mcp` and `container-use stdio` — the client that spawns the process
already controls what it can do on this machine. **stdio hygiene:** the reused functions' `io.Writer`
parameters (used for CLI progress/warning text with no structured equivalent in every case) are all
passed `io.Discard`, never a writer touching `os.Stdout` — a stray byte on stdout corrupts the
JSON-RPC stream for every tool call on the connection, and `TestMCPServeRealSubprocess`
(`cmd/devstrap`) proves this by driving the real binary as a real subprocess end to end (mutation-
checked: a single added `fmt.Println` in `runMCPServe` fails it with a JSON-RPC framing error).

Dependency: `github.com/modelcontextprotocol/go-sdk` pinned at the exact version `v1.7.0` (no floating
version), adding seven linked modules to the release binary (`golang.org/x/oauth2`, unused for stdio
but present because the SDK also supports the HTTP transports; `segmentio/asm`/`segmentio/encoding`,
hand-written-assembly JSON codecs on the tool-call parsing path; `google/jsonschema-go`,
`yosida95/uritemplate/v3`, `golang.org/x/time`) — measured and recorded as the `W13-01` decision
record in `14_MVP_ROADMAP_AND_BACKLOG.md` before this row shipped. `govulncheck` reports zero
vulnerabilities reachable from DevStrap's own code through this dependency.

Setup: `claude mcp add devstrap -- devstrap mcp serve`; see `../docs/agents.md` § *Provisioning via
MCP* for the full tool list and worked example.

## Device commands

```bash
devstrap devices list                      # last column is each device's fingerprint (P4-SEC-04); "-" when a row lacks keys
devstrap devices enroll dev_01jz... --name linux-desktop --os linux --arch arm64 --age-recipient age1... --signing-public-key ed25519:... --approve --fingerprint ABCD-EFGH-...
devstrap devices enroll --code 'devstrap-pair2:...' --approve --fingerprint ABCD-EFGH-...
devstrap devices approve dev_01jz... --fingerprint ABCD-EFGH-...
devstrap devices revoke dev_01jz...
devstrap devices lost dev_01jz...
devstrap devices rename dev_01jz... linux-desktop
devstrap devices pairing-code              # stdout is exactly the devstrap-pair2: blob + newline; stderr prints instructions + fingerprint
devstrap join 'devstrap-pair2:...'         # one-command joiner side (adopt id + pin founder + configure hub); --fingerprint opts into the out-of-band compare
devstrap devices recipient                 # print local device's age recipient (for out-of-band enrollment)
devstrap devices recipient --signing       # print local device's Ed25519 signing public key
devstrap devices recipient --workspace-id  # print the workspace id (for init --join --workspace-id on a joining device)
devstrap devices recipient --fingerprint   # print local device's fingerprint (compare out-of-band during approval)
```

Current implementation enrolls remote device records either manually with identity/key flags or via `--code <devstrap-pair1:...>`, lists and renames device records, prints a local pairing code, and updates non-local device trust state to `approved`, `revoked`, or `lost`. `devices pairing-code` reads the local device row and workspace id, refuses if the local device lacks either public key, prints exactly the one-paste blob plus newline on stdout (frozen script contract), and prints the local fingerprint plus operator instructions on stderr. Since `P7-PROD-01` the blob is a **v2** `devstrap-pair2:` code: alongside workspace id, device id, name, OS, arch, age recipient, and signing public key it now embeds the device fingerprint (derived from the same keys — a one-paste convenience + a `Decode`-time corruption check) and, when a **remote** hub is configured locally, the hub URI (so a joiner's `devstrap join` auto-configures it; a local `file:`/`folder:` hub is never embedded into the auto-apply path this way — see `devstrap join` above). The blob is still **unauthenticated by design**: the embedded fingerprint is not a MAC — a channel attacker regenerates a matching one for substituted keys, so genuine integrity still comes from confirming the fingerprint out-of-band. `Decode` still parses a legacy v1 `devstrap-pair1:` blob (no fingerprint/hub) exactly, and a code from a newer devstrap (`devstrap-pair<N>:`, N above this binary's) errors with an upgrade hint. `devices enroll --code "$CODE"` is mutually exclusive with the manual identity/key flags and carries the device id itself, so no positional id is accepted; it refuses a workspace-id mismatch before falling through to the existing approval, epoch-contiguity, upsert, grant, and replay flow. Composition target: `devstrap devices enroll --code "$CODE" --approve --fingerprint "$FP"` is the founder-side one-command enrollment. `devices recipient` is a read-only helper that prints the local device's age recipient (or Ed25519 signing public key with `--signing`, the workspace id with `--workspace-id` for `init --join --workspace-id` pairing, or the device fingerprint with `--fingerprint`; `--signing`, `--workspace-id`, and `--fingerprint` are mutually exclusive, and the bare default output stays frozen because scripts consume it unadorned) so it can be shared out-of-band for manual enrollment on another device. `devices list` appends each device's fingerprint as the **last** column (earlier columns unchanged; `-` when a row lacks either key); `--json` is unchanged and does not carry the fingerprint. Env capture encrypts local bundles to the local recipient plus approved remote recipients.

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
- hub durability export freshness when `hub_replica` is configured (warning after `2 * durability.export_interval`; unconfigured is optional/OK);
- open `event_hash_chain_break` conflicts correlated to pending key-grant predecessors as self-healing warnings; unexplained breaks are errors naming possible hub data loss.

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

### Transport core — SHIPPED 2026-07-24

The MVP transport above is built (`internal/daemon`), as the first slice of the Milestone 5 daemon wave. What is live today:

- **Socket and lifecycle.** `daemon.Listen` creates `~/.devstrap/devstrapd.sock` (`config.Paths.SocketPath`). The path is validated against darwin's portable 103-byte usable `sockaddr_un.sun_path` maximum before either server setup or client construction; an over-long `--home` is an actionable invalid-configuration error, never a bare `EINVAL` or a false "daemon unavailable." `doctor` grades the same constraint. Access control is layered, and the layering is deliberate: the parent directory is forced to `0700` — that is the real gate, since a peer that cannot traverse the directory never reaches the socket — and the socket itself is `chmod`ed to `0600` as defense in depth. The socket mode lands just after `net.Listen`, so it briefly carries umask-derived permissions; that window is exactly why the directory, not the socket mode, is the load-bearing control. **Stale-socket takeover:** probe → remove → bind → chmod is serialized under a non-blocking advisory `flock` on the permanent `devstrapd.lock` fixture. A lock contender yields `ErrAlreadyRunning`; the kernel releases ownership on process exit, including `SIGKILL`, so there is no stale-lock protocol, and the file is never unlinked (unlinking would permit a second inode and defeat serialization). Under the lock, a socket left behind by a crashed daemon is probed with a short-timeout dial — a *live* socket yields `ErrAlreadyRunning` (a running daemon is never displaced by a second one starting up), a *dead* one is unlinked and rebound. A path that exists but is **not** a socket is a hard error and is never removed. Graceful shutdown unlinks the socket (`net.UnixListener`'s default `SetUnlinkOnClose`), so the takeover path is only reached after a crash or `SIGKILL`.
- **Peer-credential authorization (closes the `CLI-05` gap).** Identity is resolved once per *connection* via `http.Server`'s `ConnContext` hook — not per request — so a request cannot be served before the check has run. The syscall lives behind a build-tagged `internal/platform` seam mirroring the `procalive_*.go` convention: `GetsockoptXucred`/`LOCAL_PEERCRED` on darwin, `GetsockoptUcred`/`SO_PEERCRED` on linux, `ErrUnsupported` elsewhere. The seam is **fail-closed** — any error, including an unsupported platform, refuses the connection — which is the deliberate opposite of `ProcessAlive`'s fail-*safe* posture, because the two answer different questions ("may I let this caller in?" versus "may I steal this lock?"). The rule is that the peer must be the daemon's own uid; **root is not exempt**, since root can open a `0600` socket it does not own, which is precisely the case filesystem permissions cannot cover. Linux's pid is recorded for diagnostics only and never gates access (pids recycle).
- **Request hardening.** Any request carrying `Origin` or `Referer` is refused — no legitimate DevStrap client sends either, and a browser always attaches one on a cross-origin request, so this closes the "local API driven by a page the user happens to have open" class. The `Host` header must be empty or the fixed `devstrapd.sock`. Bodies are bounded by `http.MaxBytesReader`, and `ReadHeaderTimeout` is set (an unbounded header read is a slowloris vector even locally, since any process running as this user can open a connection).
- **Build-skew warning (closes `CLI-05`).** Every response — including error responses — carries a `Devstrap-Daemon-Version` header, and the mutex-protected client records the latest advertised value. Daemon-only commands compare it with their own build version after a successful request. A real mismatch warns on stderr, names both versions, and gives the exact `devstrap daemon stop && devstrap daemon start` remedy without failing the command or corrupting `--json`; equal, empty, `unknown`, and development versions are silent. `daemon status` additionally reports `cli_version` and `version_skew` only on mismatch and names the skew in human output. `doctor` grades reachable-daemon skew as a warning, not an error, and treats an absent daemon as an OK/skipped optional subsystem.

  **No message claims which side is stale**, because the comparison cannot know. Detection is equality-only, so a daemon left behind by an upgrade and a stale CLI earlier on `$PATH` are indistinguishable, and naming the wrong one would send an operator looking in the wrong place.

  **The remedy names both supervision modes, because they need different commands and the wrong one makes it worse.** `daemon stop && daemon start` against a *supervised* daemon stops it cleanly — so neither `KeepAlive{SuccessfulExit:false}` nor `Restart=on-failure` brings it back — and then runs a foreground daemon in the operator's shell, which ends when the terminal closes: exactly the "hand-stopping a service-installed daemon does not hand convergence back to anything" state described under *Two supervision modes* above. Re-running `service install --daemon` replaces the unit and restarts it, and is the correct fix there. The stderr warning therefore prints both, labelled; `doctor` already queries the service manager, so it names the single applicable one.

  **`doctor` skips this check when the socket path is over-long** rather than dialling it: the dial would fail with a bare `EINVAL`, which duplicates the socket-path check's already-graded warning and reintroduces the raw errno a user must never be shown.

  The advertised header is **clamped to 64 bytes and stripped of control characters** before it is retained, because it is wire-supplied text that gets printed to a terminal and embedded in `--json`; and a response arriving *without* the header leaves the last known value in place rather than erasing it, since forgetting it would silently switch a real warning off.
- **Reserved API marker, deliberately not negotiation.** `GET /v1/version` returns `{"version","api_version"}`, with `api_version: "v1"`. This field exists so an old client can reject a protocol it does not know before mis-parsing it: an advertised nonempty API version other than `v1` produces a typed error. There is no downgrade path, feature gating, environment override, or version selection because DevStrap currently has exactly one local protocol and the daemon is normally the same binary as the CLI. The absence of negotiation machinery is intentional.

  The marker is **opaque and compared only for equality** — never ordered. So the error says the protocol is unsupported and names both sides; it does not say the daemon is *newer*, which for a `v0`, a malformed value, or anything else unrecognised would be a confident claim about the one thing an equality test cannot establish.

  `daemon events` and `daemon sync` **preflight `GET /v1/version`** before doing their work, which is what makes the "commands that must speak to the daemon do fail on it" rule above true rather than aspirational. Preflighting rather than checking afterwards also means an uninterpretable daemon is never asked to start a convergence cycle whose result the CLI would then discard.

  When the version *body* is unreadable, `daemon status` still reports the daemon's build version, because it rode the response **header** — the same value from the same field. Without that fallback the running line would lose the version and the skew line would render as `daemon , CLI 0.1.3`, dropping the two facts a user most needs in exactly the situation they ran `status` to understand.
- **Endpoints.** `GET /v1/health` (`{"ok", "uptime_seconds"}`) and `GET /v1/version` (`{"version","api_version"}`). JSON tags are `snake_case` per this file's `--json` conventions, and a wrong method on a real route returns `405`, not `404`.
- **Client.** `daemon.NewClient(socketPath)` dials the socket and exports `ErrUnavailable` for "no daemon is there", distinguished from a real transport failure by inspecting the dial. Callers map it to the already-reserved `exitDaemonUnavailable = 3`.

`/v1/status`, `/v1/sync` and `/v1/events` are **shipped** (see their own sections). The remaining designed endpoints and the whole job model are **withdrawn** — see *Withdrawn: the wider socket API and the job model* below, which replaces the inventories that used to stand here. The package is transport only — it holds no convergence logic and imports no command code, so the dependency arrow points `daemon → core` and never `daemon → cobra` (see the `ARCH2-01` narrowing in `03_SYSTEM_ARCHITECTURE.md`).

## API endpoints

```text
GET  /v1/health      shipped
GET  /v1/version     shipped
GET  /v1/status      shipped
POST /v1/sync        shipped
GET  /v1/events      shipped
```

That is the whole surface. The endpoints this section used to list alongside
them — `POST /v1/hydrate`, `POST /v1/open`, `POST /v1/worktrees`,
`POST /v1/agent-runs`, `GET|POST /v1/projects`, `GET /v1/jobs` — are
**withdrawn**, not pending. See below.

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

### Watch-plane failure isolation and hint scoping (`P9-DAEMON-01`/`-02`, 2026-07-31)

Two corrections to what the watcher shipped as, both found by the Pass-9 audit of a plane Pass 8 commissioned and never reached.

**A per-root failure no longer takes the plane down.** `addRecursiveWatch` tolerated only `fs.ErrNotExist`, so an unreadable directory — a test fixture exercising `EACCES`, a root-owned container bind-mount — returned a hard error. The daemon's `watchAll` watched every root in its own goroutine under one shared `context.WithCancel` and returned on the **first** non-context error, so its deferred cancel tore down every *other*, healthy root's watch; the re-discovery loop then re-walked the whole tree each interval and failed identically forever. Native watching for the entire device was disabled by one project, visible only in `/v1/health`'s `watch.reason`. Permission errors now skip the subtree (in **both** the walk callback *and* the `watcher.Add` registration — the kernel refuses the watch even when the entry lists fine from a readable parent, which is the branch a `chmod 0000` directory actually trips), and `watchAll` degrades only when **no** root is watchable. A partial plane beats none: the remaining roots keep sub-interval latency, and the periodic cycle still covers the failed one.

**A hint-triggered convergence is now genuinely cheaper than the periodic tick.** `runLoopTick` ran its full-workspace `scan.Walk` unconditionally, including for watcher-driven cycles. That scan shells out to git twice per discovered repo, and the "cheap enough to run every tick" judgement behind it was made against run-loop's 5-minute interval — while the watcher's trigger floor is 5 seconds. Hint-driven cycles (`TickNamespaceOnly`, which is the only mode the watcher requests) now skip scan+adopt. Adoption of a newly-created local project is picked up by the next periodic tick, which is correct: the watcher is a latency optimization and makes no completeness claim. A daemon configured namespace-only for other reasons keeps scanning, as does `run-loop --namespace-only`, where the scan is exactly what feeds the namespace.

## Withdrawn: the wider socket API and the job model (2026-08-01, `W13-09`)

This file documented six further endpoints and a thirteen-type job model
(`reconcile_root`, `scan_project`, `create_skeleton`, `hydrate_git_repo`,
`fetch_git_repo`, `check_dirty_state`, `capture_env`, `hydrate_env`,
`sync_push`, `sync_pull`, `create_worktree`, `run_agent`, `cleanup_worktree`).
None was ever built. **They are withdrawn rather than left pending**, following
the `AD5-05` precedent: a path the project has decided not to take, described as
"planned", is a promise it has decided not to keep, and a reader cannot tell the
difference from the outside.

**The reason is not that a socket endpoint is inherently a second execution
path.** That argument is refuted by this project's own precedent — `POST /v1/sync`
calls the same `runLoopTick` the CLI calls, and after `W13-01`'s extractions a
`POST /v1/worktrees` would be a thin handler over the same `createFreshWorktree`.
Discipline already solves that problem. Three other things decide it:

- **A second machine contract for one operation is permanent dual maintenance.**
  `AD5-01` made `worktree new --json` a versioned, documented machine contract
  with an additive-only evolution rule, and `AD-5`'s settled answer is that
  harnesses shell out. `POST /v1/worktrees` would be a second contract for the
  same operation, evolving separately, with the same additive obligations.
- **No consumer exists or is designed.** There is no TUI, no editor adapter, no
  shell integration that wants these. The latency argument does not rescue them
  either: worktree creation, hydrate and agent runs are seconds of git and
  network work, so the ~30ms of process spawn plus SQLite open that a daemon
  saves is noise. The one endpoint class where daemon residency genuinely pays —
  fast repeated reads, i.e. `GET /v1/projects` — is precisely the database proxy
  this file **already refused** when it scoped `/v1/status` to a narrow `Reader`
  seam, on the stated grounds that it would give the daemon a second, drifting
  view of state it does not own.
- **A persisted job queue with leases and requeue-on-crash makes the daemon
  stateful**, against the explicit and load-bearing invariant that the daemon is
  never a correctness dependency. Every command works with no daemon, and
  `devstrap run-loop` remains the portable daemonless convergence path.

**What is kept as design intent, because it is qualitatively different.** A
**read-only** surface reporting what the daemon *already does* — watch-triggered
convergence outcomes it has observed — observes existing work rather than
creating a second way to start work. `/v1/events` is that surface and it shipped.
Any future extension of it stays in that shape: it may report, it may not act.

**Re-open trigger, stated so it can be observed rather than argued.** Reinstate
this direction when a **real consumer exists in-tree** — a TUI, an editor
adapter, or a shell integration that is committed, not proposed — **and** that
consumer needs either repeated reads at a rate where per-call process spawn is
measurably the bottleneck, or observability of daemon-initiated work that
`/v1/events` cannot express. Absent a committed consumer, the correct number of
endpoints is the number that has one. This mirrors the FSEvents deferral in
`05_MAC_FIRST_IMPLEMENTATION.md`, which named a threshold (~20,000 watched
directories) instead of "if it becomes a problem".

`W13-01`'s `devstrap mcp serve`, if it ships, further narrows any future case:
MCP over stdio becomes the machine protocol a harness speaks, and it needs no
socket at all.

### The `AD5-07` no-second-execution-path prerequisite (2026-08-05)

`AD5-07`'s acceptance criterion is that every MCP tool handler call the **same**
internal Go function its cobra command calls — the discipline the daemon's
`Converger` seam already follows by calling the same `runLoopTick` the CLI calls,
and the one `import` follows by registering rather than cloning. That criterion
cannot be met against logic that lives inside a `RunE` closure, because a closure
over cobra's flag variables has no caller other than cobra.

Three commands were in that state and are now extracted, ahead of any MCP code:
`worktree status` → `statusWorktree`, `worktree list` → `listWorktrees`, and
`agent adopt` → `adoptAgentRun`. Each takes `*options` and `*state.Store`
directly, returns a typed value, and uses an `io.Writer` only for non-fatal
warnings — the shape `createFreshWorktree` and `adoptWorktreeAt` already had.
What stays in `RunE` is the part that is genuinely about cobra: store lifecycle,
the human-format render closure, and **flag-combination validation**
(`--allow-shallow` requires `--adopt-worktree`; `--engine`/`--task` required;
`--pid` must be positive when passed). Those checks read `Flags().Changed(...)`,
and their relative firing order is observable CLI behavior, so moving them would
change which error a doubly-invalid invocation reports.

That argument forbids *relocating* those checks, not *backstopping* them, and
the distinction matters: `adoptAgentRun` re-validates `engine`/`task` (and
refuses a negative pid) itself, through the **same** `requiredAgentAdoptField`
helper `RunE` calls, so there is one definition of the rule and no second
validation path free to drift. On the CLI path the backstop is unreachable —
`RunE` returns before the store is opened — so precedence and every emitted
error message are unchanged. It exists because `agent_runs.engine` is
`NOT NULL` but happily accepts `""`, so without it a non-CLI caller could write
a blank-engine `running` row into the provenance registry. The
`--allow-shallow` requires `--adopt-worktree` check is genuinely flag semantics
and is *not* duplicated: the flag is inert in the domain function.

`adoptAgentRun` carries "the caller did not name a pid" as a non-positive `pid`
rather than a companion boolean. On the CLI path the two are the same set of
invocations, because `RunE` rejects an explicitly-passed `--pid <= 0` outright —
and the underlying rule (`agent adopt` never guesses a pid, since a harness that
shells out has a PPID belonging to a transient wrapper shell about to exit, which
would flip a healthy run to `interrupted` on the next sweep) is unchanged.

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

### Daemon socket API (M5; `CLI-05` CLOSED)
**Updated 2026-07-28.** The local Unix-socket API's transport core and all three `CLI-05` hardening gaps are shipped (see *Transport core — SHIPPED 2026-07-24* above): peer-credential checks including root rejection; HTTP+JSON message framing with bounded bodies and typed errors; and consumption of the daemon build-version header plus the reserved `api_version` marker. Build skew is a non-fatal restart warning — on stderr from `daemon events`/`daemon sync`, and in its own output from `daemon status`, which reports rather than warns because it *is* the report. An unknown API version is a typed error, reported by `daemon status` and refused by the commands that must speak to the daemon, with no negotiation machinery. The **job model remains design intent** (`ARCH2-04`), as do endpoints not explicitly marked shipped elsewhere in this file.

### Planned commands (not yet registered)
Referenced by the new workstreams; intentionally absent from the live command tree the drift test checks until implemented (`devstrap conflicts` has since shipped — see the 2026-06-28 implementation notes — and is documented under `status`):

```text
devstrap gitstate capture [--fetch]  # WITHDRAWN 2026-08-01 (W13-09): superseded by the sync-wired Layer A capture
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
- **Content-type split (`DRAFT-*`)**: env plus non-git/draft folders sync as age-encrypted blobs; `node_modules`/build artifacts are never synced and are rebuilt on hydrate. `hydrate`/`open` extend to `local_git`/`plain_folder`/draft project types. `devstrap promote` (`NOVCS-03`) is **shipped** — see its section above.
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
