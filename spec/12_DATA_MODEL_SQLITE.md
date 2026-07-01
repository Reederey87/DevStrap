---
last_reviewed: 2026-07-01
tracks_code: [internal/state/**, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md]
---
# SQLite Data Model

## Database location

```text
~/.devstrap/state.db
```

Open the database with per-connection pragmas in the SQLite DSN. WAL permits concurrent readers, not concurrent writers, so the Go writer pool is limited to one open connection.

```go
file:<path>?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(67108864)&_txlock=immediate
db.SetMaxOpenConns(1)
db.SetMaxIdleConns(1)
```

`state.db` and backups must be mode `0600`; the containing state directory must be `0700`.

On open, DevStrap asserts `PRAGMA foreign_keys = 1` and runs `PRAGMA foreign_key_check`; opening a database with disabled FK enforcement or pre-existing FK violations must fail before normal state access. `devstrap db status` and `doctor` print both `quick_check` and `foreign_key_check` so schema corruption and relational integrity drift are visible separately.

Timestamp text columns use fixed-width UTC nanosecond format:

```text
2006-01-02T15:04:05.000000000Z
```

Do not use Go's variable-width `time.RFC3339Nano` for state ordering columns because trimmed fractional zeros break simple lexicographic ordering. Queries that order by timestamp must include a stable final tiebreaker, such as `id`, when same-timestamp rows can exist.

## Main tables

### workspaces

```sql
CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  root_path TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

`id` is generated once as `ws_<uuidv7>` during `devstrap init` and is treated as data at the store boundary. Phase 0 remains a single-workspace MVP; migration `00006_workspace_singleton.sql` enforces that invariant with a unique expression index so code cannot accidentally create a second local workspace row. Future device pairing must provision the same logical workspace id on every approved device.

### devices

```sql
CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  os TEXT NOT NULL,
  arch TEXT NOT NULL,
  hostname TEXT,
  public_key TEXT,
  signing_public_key TEXT,
  trust_state TEXT NOT NULL DEFAULT 'pending',
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

`public_key` stores the device's age X25519 recipient string (`age1...`). `signing_public_key` stores the device's Ed25519 public key string (`ed25519:<base64>`). Private age and signing identities must never be stored in `state.db`; the current local backend stores them in the OS keychain/Secret Service when available and falls back to `0600` files under the DevStrap key directory on unsupported/headless systems.

### namespace_entries

```sql
CREATE TABLE namespace_entries (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  path TEXT NOT NULL,
  path_key TEXT NOT NULL,
  type TEXT NOT NULL,
  display_name TEXT,
  materialization_policy TEXT NOT NULL DEFAULT 'lazy',
  env_profile_id TEXT,
  tooling_profile_id TEXT,
  agent_policy_id TEXT,
  ignore_profile_id TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  tombstone_hlc INTEGER,
  source_event_hlc INTEGER,
  source_event_device_id TEXT,
  source_event_id TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(workspace_id, path_key),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`path_key` is normalized for case-insensitive conflict detection. `source_event_*` records the event coordinates that produced the active namespace entry so same-path/different-remote conflicts can be reconciled deterministically across pull windows. Current storage still defaults `materialization_policy` to `lazy`; the `EAGER-*` workstream should migrate the default/created rows to `eager` while retaining `lazy` as a future opt-out for StrapFS/manual workflows.

### git_repos

```sql
CREATE TABLE git_repos (
  namespace_id TEXT PRIMARY KEY,
  remote_url TEXT NOT NULL,
  remote_key TEXT NOT NULL,
  default_branch TEXT NOT NULL DEFAULT 'main',
  clone_filter TEXT,
  sparse_config TEXT,
  lfs_policy TEXT NOT NULL DEFAULT 'auto',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);
```

`remote_url`/`remote_key` are `NOT NULL`, but SQLite accepts `''` against `NOT NULL`. A `git_repo` MUST have a non-empty validated `remote_key`; a remote-less repo is the distinct `local_git` namespace type (see `07_NAMESPACE_AND_SYNC_MODEL.md`), never persisted here with an empty remote (`NOVCS-01`). Add `CHECK (remote_key <> '')`, and consider declaring enum/status tables `STRICT` with `CHECK` constraints generally (`DATA-04`).

### draft_projects

```sql
CREATE TABLE draft_projects (
  namespace_id TEXT PRIMARY KEY,
  current_snapshot_id TEXT,
  max_bytes INTEGER NOT NULL DEFAULT 104857600,
  max_files INTEGER NOT NULL DEFAULT 5000,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);
```

`draft_projects` backs the non-git / draft folder content sync (`DRAFT-*`, see `07_NAMESPACE_AND_SYNC_MODEL.md` and `09_SECRETS_AND_ENVIRONMENT.md`). Draft content is captured via a `.devstrapignore` compiler (node_modules / build artifacts excluded, rebuilt on hydrate), packed into age-encrypted content-addressed bundles, and referenced by `current_snapshot_id` — never blanket file-synced and never carried as `.git`. `max_bytes`/`max_files` are intended caps but are **not yet enforced** (`DRAFT-04`): nothing rejects an oversized draft capture today. Enforce them at capture time (refuse the snapshot when the packed tree exceeds the cap) or mark them deferred.

### device_project_state

```sql
CREATE TABLE device_project_state (
  device_id TEXT NOT NULL,
  namespace_id TEXT NOT NULL,
  local_path TEXT NOT NULL,
  materialization_state TEXT NOT NULL DEFAULT 'skeleton',
  git_branch TEXT,
  git_head_sha TEXT,
  git_upstream_sha TEXT,
  dirty_state TEXT NOT NULL DEFAULT 'unknown',
  env_ready INTEGER NOT NULL DEFAULT 0,
  tooling_ready INTEGER NOT NULL DEFAULT 0,
  last_scan_at TEXT,
  last_fetch_at TEXT,
  last_error TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(device_id, namespace_id),
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);
```

Note: `env_ready`/`tooling_ready` exist but are not yet written or read, and the derived display-status set is not computed (`PROD-01`). Either wire them or mark them deferred.

### device_gitstate (working-state validation plane — sidecar)

Mirror of each device's read-only git-state snapshot for the cross-machine "forgot to push" validation plane (`repo.gitstate.observed`, see `07_NAMESPACE_AND_SYNC_MODEL.md`, audit Section 5). Deliberately a **sidecar** with an **opaque `device_id` and NO FK to `devices`**: remote devices are not enrolled until Phase 2, so the `device_project_state.device_id → devices(id)` FK above is exactly why this cannot reuse that table.

```sql
CREATE TABLE device_gitstate (
  device_id TEXT NOT NULL,              -- opaque; NOT a FK to devices(id)
  namespace_id TEXT NOT NULL,
  branch TEXT, head_sha TEXT, upstream_sha TEXT,
  dirty_tracked INTEGER NOT NULL DEFAULT 0,
  untracked INTEGER NOT NULL DEFAULT 0,
  unmerged INTEGER NOT NULL DEFAULT 0,
  ahead INTEGER NOT NULL DEFAULT 0,
  behind INTEGER NOT NULL DEFAULT 0,
  stash_count INTEGER NOT NULL DEFAULT 0,
  no_upstream INTEGER NOT NULL DEFAULT 0,
  source_event_hlc INTEGER NOT NULL,
  attributed_unverified INTEGER NOT NULL DEFAULT 1,  -- 0 after Phase-2 pubkey enrollment
  captured_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(device_id, namespace_id),
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);
```

Status: planned. No `device_gitstate` migration exists yet; add it as `00014_gitstate_mirror.sql` when the Layer A working-state validation plane lands (00010–00013 are now taken — see the migration list below). `sync_cursors` and `event_delivery` are defined; `hub_cursors` (00008) is wired for cursor-based incremental pull (EAGER-02); `pending_hub_deletes` (00011) backs the revoke-rewrap cleanup queue (`P5-PROD-02`). `device_sync_state` and `jobs` remain unwired.

### env_profiles

```sql
CREATE TABLE env_profiles (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  mode TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

### secret_bindings

```sql
CREATE TABLE secret_bindings (
  id TEXT PRIMARY KEY,
  env_profile_id TEXT NOT NULL,
  var_name TEXT NOT NULL,
  provider_ref TEXT,
  encrypted_value_ref TEXT,
  required INTEGER NOT NULL DEFAULT 1,
  needs_rotation INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(env_profile_id, var_name),
  CHECK ((provider_ref IS NOT NULL) <> (encrypted_value_ref IS NOT NULL)),
  CHECK (encrypted_value_ref IS NULL OR encrypted_value_ref LIKE 'age_blob:%'),
  FOREIGN KEY(env_profile_id) REFERENCES env_profiles(id) ON DELETE CASCADE
);
```

`provider_ref` and `encrypted_value_ref` are references only. Plaintext secret values never persist in `state.db`.

### worktrees

```sql
CREATE TABLE worktrees (
  id TEXT PRIMARY KEY,
  namespace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  path TEXT NOT NULL,
  branch TEXT NOT NULL,
  base_ref TEXT NOT NULL,
  base_sha TEXT NOT NULL,
  created_by TEXT NOT NULL,
  agent_run_id TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  dirty_state TEXT NOT NULL DEFAULT 'unknown',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);
```

### agent_runs

```sql
CREATE TABLE agent_runs (
  id TEXT PRIMARY KEY,
  namespace_id TEXT NOT NULL,
  worktree_id TEXT,
  engine TEXT NOT NULL,
  task TEXT NOT NULL,
  policy_id TEXT,
  status TEXT NOT NULL DEFAULT 'pending',
  base_ref TEXT,
  base_sha TEXT,
  branch TEXT,
  log_path TEXT,
  diff_summary TEXT,
  test_summary TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE,
  FOREIGN KEY(worktree_id) REFERENCES worktrees(id) ON DELETE SET NULL
);
```

Current implementation writes `agent_runs` for the thin generic runner. Runs record the associated worktree, engine, task, base ref/SHA, branch, `0600` log path, diff summary, test/command summary, and status (`running`, `complete`, or `failed`).

### events

```sql
CREATE TABLE events (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  seq INTEGER,
  hlc INTEGER NOT NULL DEFAULT 0,
  type TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '',
  device_sig TEXT,
  prev_event_hash TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);
```

Rows in `events` are insert-only. Mutable delivery/apply state belongs in `event_delivery`.

Local event creation links the new event to the previous same-device event content hash, then signs the canonical event payload `(id, hlc, type, payload_json, content_hash, prev_event_hash)` with the local Ed25519 device signing identity. Event insertion verifies `content_hash`, verifies any non-empty `prev_event_hash` against the previous same-device event already stored locally, and verifies `device_sig` when the source device has a known `signing_public_key`; unsigned events from devices without a known signing key remain accepted for current local-only sync tests and pre-approval bootstrap flows. Sync records an `event_hash_chain_break` conflict when incoming previous-hash validation fails.

### device_sync_state

```sql
CREATE TABLE device_sync_state (
  device_id TEXT PRIMARY KEY,
  last_hlc INTEGER NOT NULL DEFAULT 0,
  next_seq INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);
```

This table persists the local writer clock. Local event creation updates `last_hlc` and `next_seq` in the same transaction that inserts the event, seeded from existing max event values when the row is missing.

### sync_cursors

```sql
CREATE TABLE sync_cursors (
  workspace_id TEXT NOT NULL,
  peer_id TEXT NOT NULL,
  last_hlc_applied INTEGER NOT NULL DEFAULT 0,
  last_seq_applied INTEGER,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, peer_id),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(peer_id) REFERENCES devices(id) ON DELETE CASCADE
);
```

`sync_cursors` is the planned (`DATA-02`, `HUB-*`) per-peer resume point for cursor-based incremental sync: one row per remote peer (or hub) holding the highest applied HLC/seq so a pull requests only events after `last_hlc_applied` instead of replaying full history from HLC 0. The cloud hub exposes the same shape as an opaque `cursor=<HLC>` over its event-log plane (`410 -> snapshot` when the cursor is too old, see `07_NAMESPACE_AND_SYNC_MODEL.md`). Until wired, sync ignores this table and replays from 0 (`ARCH2-02`).

```sql
CREATE TABLE event_delivery (
  event_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  applied_at TEXT,
  sync_state TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(event_id, device_id),
  FOREIGN KEY(event_id) REFERENCES events(id) ON DELETE CASCADE,
  FOREIGN KEY(device_id) REFERENCES devices(id) ON DELETE CASCADE
);
```

`event_delivery` is the planned (`DATA-02`, `HUB-*`) per-event, per-device apply ledger that complements `sync_cursors`: it records `sync_state` (`pending`/`applied`/`failed`) and `applied_at` for individual events so re-delivery and partial-failure recovery are idempotent without scanning the whole `events` table. Mutable delivery/apply state lives here precisely because `events` rows are insert-only (see above). Until wired it is unused; the file-backed sync spike applies events directly and tracks progress only through the local writer clock in `device_sync_state`.

```sql
CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  namespace_id TEXT,
  status TEXT NOT NULL DEFAULT 'queued',
  priority INTEGER NOT NULL DEFAULT 100,
  payload_json TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  run_after TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE SET NULL
);
```

### conflicts

```sql
CREATE TABLE conflicts (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  namespace_id TEXT,
  type TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  details_json TEXT NOT NULL,
  resolution_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE SET NULL
);
```

### blobs (content-addressed encrypted blob index — planned)

Planned (`HUB-*`, `DRAFT-*`) local index of the content-addressed encrypted blob store — the hub's second plane (env values + non-git/draft bundles), all age-encrypted client-side and named `age_blob:<sha256>`. The hub sees only ciphertext; this table is the local bookkeeping for what each blob is, whether it is cached locally and/or uploaded, and when it may be reclaimed.

```sql
CREATE TABLE blobs (
  sha256 TEXT PRIMARY KEY,                 -- content address; refs are 'age_blob:<sha256>'
  workspace_id TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  ref_count INTEGER NOT NULL DEFAULT 0,    -- live references from secret_bindings + draft snapshots
  local_cached INTEGER NOT NULL DEFAULT 0, -- ciphertext present on this device
  hub_uploaded INTEGER NOT NULL DEFAULT 0, -- pushed to the hub blob store
  recipient_set_hash TEXT,                 -- age recipients the blob is encrypted to; changes on re-encrypt
  created_at TEXT NOT NULL,
  last_referenced_at TEXT,
  gc_eligible_at TEXT,                     -- set when ref_count hits 0; reclaimed only after the grace period
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`ref_count` is incremented when a `secret_bindings.encrypted_value_ref` or a `draft_projects.current_snapshot_id` (and its packed bundle) points at the blob, and decremented when those references are tombstoned, rotated, or superseded. Garbage collection is **ref-count + grace-period**, never immediate: when `ref_count` drops to 0 the GC job stamps `gc_eligible_at = now`, and a blob is deleted (locally and from the hub) only after the grace period elapses with `ref_count` still 0. The grace window protects against in-flight references during a concurrent sync and against the device-revoke re-encrypt flow, which rewrites affected blobs to the reduced recipient set (new `recipient_set_hash`) and flags secrets for rotation (age has no native revocation, see `15_SECURITY_THREAT_MODEL.md`). Content addressing makes blob writes idempotent: re-capturing identical content reuses the existing row.

**Blob reclamation status (`HUB-05`/`HUB-12`/`SEC-01`):** the `Hub.DeleteBlob` / `S3Client.DeleteObject` reclamation primitive is shipped (idempotent deletes; a missing blob is not an error), enabling both the ref-count GC job and device-revoke hub-side cleanup. Device revoke/lost (`devstrap devices revoke|lost --hub-file`) now pulls non-cached blobs from the hub (with `SEC-03` content-hash verification), rewraps to the reduced recipient set, pushes the new blob, and deletes the old ciphertext from the hub (guarded by a `blobRefStillReferenced` check) so a revoked key can no longer fetch it; without `--hub-file`, rewrap is local-only and hub cleanup is deferred to the next sync. Fetched blobs are hash-verified against their signed `age_blob:<sha256>` ref (`SEC-03`) so an untrusted hub cannot substitute bytes. The R2/S3 backend wraps every call in a retry/backoff seam with throttle/transient/terminal error classification (`HUB-10`), and the write path relies solely on the conditional put (`HUB-09`).

## Indexes

```sql
CREATE INDEX idx_git_remote_key ON git_repos(remote_key);
CREATE INDEX idx_device_state_namespace ON device_project_state(namespace_id);
CREATE INDEX idx_events_order ON events(workspace_id, hlc, device_id, id);
CREATE UNIQUE INDEX idx_events_device_seq ON events(device_id, seq) WHERE seq IS NOT NULL;
CREATE INDEX idx_namespace_active ON namespace_entries(workspace_id, path_key) WHERE status = 'active';
CREATE INDEX idx_jobs_status_priority ON jobs(status, priority, run_after);
CREATE INDEX idx_worktrees_namespace ON worktrees(namespace_id);
CREATE INDEX idx_agent_runs_status ON agent_runs(status);
CREATE INDEX idx_secret_bindings_profile ON secret_bindings(env_profile_id);
CREATE INDEX idx_env_profiles_workspace ON env_profiles(workspace_id);
CREATE INDEX idx_worktrees_device ON worktrees(device_id);
CREATE INDEX idx_agent_runs_namespace ON agent_runs(namespace_id);
CREATE INDEX idx_jobs_namespace ON jobs(namespace_id);
CREATE INDEX idx_conflicts_namespace ON conflicts(namespace_id);
```

`idx_namespace_active` supports the Phase-0 `ListProjects` query shape:

```sql
WHERE n.workspace_id = ? AND n.status = 'active'
ORDER BY n.path_key
```

The predicate text intentionally matches the query's `status = 'active'` term so SQLite can prove the partial index applies and satisfy the active filter plus path ordering without a temporary sort.

## ID format

Use prefixed sortable IDs.

Examples:

```text
ws_01jz...
dev_01jz...
prj_01jz...
evt_01jz...
job_01jz...
wt_01jz...
arun_01jz...
```

ULID or UUIDv7 are good choices.

## Migration strategy

Use Goose SQL migrations embedded into the Go binary. Goose supports SQLite, can run as a library, and keeps the CLI install story simple because migration files do not need to be shipped separately.

Use numbered migrations:

```text
internal/state/migrations/
  00001_initial.sql
  00002_event_ordering.sql
  00003_namespace_source_events.sql
  00004_device_signing_keys.sql
  00005_namespace_active_index.sql
  00006_workspace_singleton.sql
  00007_secret_binding_rotation.sql
  00008_sync_hub_cursor.sql
  00009_draft_snapshots.sql
  00010_repo_forge_kind.sql
  00011_pending_hub_deletes.sql
  00012_draft_snapshot_idempotency.sql
  00013_workspace_keys.sql
```

CLI:

```bash
devstrap db migrate
devstrap db status
devstrap db backup
```

Current implementation:

```text
internal/state/migrations/00001_initial.sql
internal/state/migrations/00002_event_ordering.sql
internal/state/migrations/00003_namespace_source_events.sql
internal/state/migrations/00004_device_signing_keys.sql
internal/state/migrations/00005_namespace_active_index.sql
internal/state/migrations/00006_workspace_singleton.sql
internal/state/migrations/00007_secret_binding_rotation.sql
internal/state/migrations/00008_sync_hub_cursor.sql
internal/state/migrations/00009_draft_snapshots.sql
internal/state/migrations/00010_repo_forge_kind.sql
internal/state/migrations/00011_pending_hub_deletes.sql
internal/state/migrations/00012_draft_snapshot_idempotency.sql
internal/state/migrations/00013_workspace_keys.sql
```

The current schema version is **13**. `00010_repo_forge_kind.sql` adds the per-project forge override (`GIT-05`); `00011_pending_hub_deletes.sql` queues blobs orphaned by a local-only revoke for deletion on the next hub-enabled sync (`P5-PROD-02`/`P5-SEC-01`); `00012_draft_snapshot_idempotency.sql` adds a partial `UNIQUE` index on `draft_snapshots(namespace_id, source_event_id)` so idempotency is enforced by the DB, not only the SELECT-then-INSERT guard (`P5-DATA-02`); `00013_workspace_keys.sql` adds the `workspace_keys` and `workspace_key_grants` tables backing the WCK epoch keyring for envelope encryption of the event log (`P4-SEC-02`/`P4-SEC-07`) — `workspace_keys(workspace_id, epoch, created_at)` records which epochs this device holds, and `workspace_key_grants(workspace_id, epoch, recipient, source_event_id, source_event_hlc, source_event_device_id, created_at)` is a membership audit of device.key.granted events (the wrapped WCK itself rides the event payload, never SQLite). Migrations can be applied by `devstrap init` or explicitly with `devstrap db migrate`.

## Backup

Local backup command:

```bash
devstrap db backup ~/.devstrap/backups/state-20260623.db
```

Backups use SQLite `VACUUM INTO`, not file copy, so WAL/SHM state is captured consistently.

Workspace export:

```bash
devstrap export --encrypted --output devstrap-snapshot.tar.age
```

## Hub backend (planned)

`state.db` is per-device local state. The shared, cross-device data lives in the zero-knowledge **devstraphub**, addressed behind a single pluggable `Hub` interface with two planes (see `03_SYSTEM_ARCHITECTURE.md` and `07_NAMESPACE_AND_SYNC_MODEL.md`):

- the **event log** (the signed, HLC-ordered namespace map), resumed via the `sync_cursors` / `event_delivery` shape above;
- the **content-addressed encrypted blob store** (env + non-git/draft bundles), indexed locally by `blobs`.

The chosen (`HUB-*`) production backend is **Cloudflare R2** (S3-compatible API, zero egress), keyed under a per-workspace prefix so each workspace's objects are namespaced by `workspace_id` (e.g. `s3://<bucket>/workspaces/<workspace_id>/events/...` and `.../blobs/<sha256>`). Because all payloads are age-encrypted client-side and the map is signed, the backend stores only ciphertext plus a signed map and cannot read code, secrets, or drafts. This gives confidentiality by construction; integrity and availability still require scoped credentials, signed hash-chain verification, snapshots/backups, and retention rules. A **file-backed local backend remains only for tests** (`devstrap sync --hub-file <path>`); there is no NAS-first phase. Repo content never transits the hub — it rides git's own transport via blobless clone/fetch from each repo's existing remote. Hub connection settings (backend kind, bucket, region/endpoint, workspace prefix) are configuration, not schema, and never include plaintext credentials in `state.db`.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-DATA-01 — Origin never records its own draft snapshot row, so GC deletes the live bundle

**Problem.** `draft.go:92` inserts the `draft.snapshot.created` event but writes no `draft_snapshots` row; the only writer is the sync apply path, which `ApplyEvents` skips for already-present events (`events.go:299`). So on the creating device the blob is referenced by nothing (`LatestDraftSnapshot` nil, `RetainedBlobRefs` omit it), and `sync` local GC plus `hub gc` delete the only copies — permanent data loss on a single device.

**Actionable steps.**
1. Record the `draft_snapshots` row atomically at create time in one transaction with the event insert (`tx.InsertEvent` + `tx.RecordDraftSnapshotTx`).
2. Audit `emitSupersedingDraftSnapshot` (`blob_gc.go:181`) for the same missing-row assumption, including the revoke-rewrap loop that walks `DraftBlobRefs`.
3. Test: create snapshot on A → `sync` + `hub gc` on A → assert `LatestDraftSnapshot` non-nil and the blob survives locally and on the hub.

```go
store.WithTx(ctx, func(tx *state.Tx) error {
    if err := tx.InsertEvent(ev); err != nil { return err }
    return tx.RecordDraftSnapshotTx(ev.NamespaceID, ev.ID, blobRef)
})
```

### P6-DATA-02 — `ClearRotationForProject` filters on a non-existent `env_profiles.namespace_id` column

**Problem.** The one-arg `env rotate <path>` (flag-clear-only) subquery in `store.go:1632-1637` references `env_profiles.namespace_id`, which does not exist (the link is `namespace_entries.env_profile_id`), so it fails on every call with `no such column: namespace_id`. Only `env rotate --all` is tested.

**Actionable steps.**
1. Rewrite the subquery to join through `namespace_entries` and add a per-project store test.
2. Add a CI lint that `db.Prepare`s every static query in `store.go` against a migrated in-memory DB.

```sql
UPDATE secret_bindings SET needs_rotation = 0, updated_at = ?
WHERE needs_rotation = 1 AND env_profile_id IN (
  SELECT env_profile_id FROM namespace_entries
  WHERE id = ? AND env_profile_id IS NOT NULL);
```

### P6-DATA-03 — Event emission and derived-state mutation are dual-written in separate transactions

**Problem.** `add.go:68-92` calls `CreateProjectEvent` (its own `WithTx`) then `UpsertProject` (a second transaction); `scan.go` adopt and both `conflict_resolve.go` sites share the pattern. A crash between the two commits leaves a synced `project.added` event with no `namespace_entries` row on the origin, and `ApplyEvents` (`events.go:299`) never re-applies the origin's own event — silent permanent divergence.

**Actionable steps.**
1. Add `Tx`-scoped emission helpers (`CreateProjectEventTx` reusing `tx.InsertEvent` + `nextLocalEventStamp` + `tx.UpsertProject`) and wrap every emission site (`add`, `adoptFindings`, both `conflict_resolve.go` sites) in one `WithTx`.
2. (Optional, defense-in-depth) re-run `applyEventTx` even when `inserted==false`; handlers are idempotent.
3. Test: simulate a crash between the two commits and assert the origin heals on retry or never diverges.

```go
store.WithTx(ctx, func(tx *state.Tx) error {
    if err := tx.CreateProjectEventTx(ev); err != nil { return err }
    return tx.UpsertProject(project)
})
```

### P6-DATA-04 — `db backup` is incomplete: env blobs and file-fallback keys excluded, no restore path

**Problem.** `Backup` (`store.go:292-306`) is `VACUUM INTO` the `state.db` file only. Encrypted env values live outside the DB as `~/.devstrap/blobs/<hash>.age` (env blobs are local-only per `P5-SEC-04`) and key material lives in `<statedir>/keys`, so a restored DB holds dangling `age_blob:` refs; there is no `restore` command and `doctor.go:203-205` wrongly recommends restoring from a backup.

**Actionable steps.**
1. Ship `db backup --full <out.tar>` bundling `state.db` + referenced `blobs/` + `keys/` (file-fallback) + keychain escrow (age identity + WCK `wck-<ws>-<epoch>.key` files), all `0600`, and a `db restore <in>` (refuse over a non-empty state dir without `--force`).
2. Add a doctor "dangling blob refs" check that stats each `AllBlobRefs` entry (draft refs falling back to hub `HasBlob`).
3. Fix the `doctor.go:203-205` remedy text once `--full` exists.

```bash
devstrap db backup --full ~/.devstrap/backups/state-20260701.tar   # state.db + blobs + keys + escrow
devstrap db restore ~/.devstrap/backups/state-20260701.tar         # refuses over non-empty state dir without --force
```

### P6-DATA-05 — No index serves `events(device_id, hlc)`; every push/doctor full-scans the log

**Problem.** `LocalPendingEvents` (`store.go:2682-2687`) filters `device_id = ? AND hlc > ? ORDER BY hlc, id`, but neither `idx_events_order` (leads with `workspace_id`) nor partial `idx_events_device_seq` serves it, so `EXPLAIN QUERY PLAN` reports `SCAN events` + `USE TEMP B-TREE FOR ORDER BY` on a table that grows unbounded (`P4-SYNC-02`).

**Actionable steps.**
1. Add a migration `idx_events_device_hlc ON events(device_id, hlc, id)` (trailing `id` satisfies the ORDER BY tiebreak) and update the migration list / index inventory in this file; note spec/12 reserves `00014` for `gitstate_mirror`, so renumber accordingly.
2. Verify `EXPLAIN` reports `SEARCH events USING INDEX` with no temp B-tree.

```sql
CREATE INDEX idx_events_device_hlc ON events(device_id, hlc, id);
```

### P6-DATA-06 — No DB invariant enforces a single `local` device; concurrent `init` can fork identity

**Problem.** `EnsureDevice` (`store.go:487-538`) runs a SELECT for `trust_state = 'local'` then, on `ErrNoRows`, an INSERT as two autocommit statements with no flock, so racing `devstrap init` processes can each insert a `local` device. Migration 00006 gives `workspaces` a singleton index but `devices` has no counterpart, and the three `LEFT JOIN devices d ON d.trust_state = 'local'` sites (`store.go:1262,1287,1316`) then row-multiply `ListProjects`.

**Actionable steps.**
1. Add a partial unique index (with a dedup guard keeping MIN(created_at)) mirroring 00006.
2. Make `EnsureDevice` transactional/race-tolerant (SELECT+INSERT inside `s.WithTx`, or treat a UNIQUE error as "lost the race" and re-SELECT).
3. Add a doctor check asserting `SUM(trust_state = 'local') = 1` (note: `COUNT(trust_state='local')` counts every row, since the expression is non-NULL — use `SUM` of the boolean predicate).

```sql
CREATE UNIQUE INDEX idx_devices_local_singleton ON devices((1)) WHERE trust_state = 'local';
```

## Audit implementation notes (2026-06-28)

- **DATA-01**: `Backup()` validates the backup with `PRAGMA quick_check` + `foreignKeyCheck` after `VACUUM INTO`; removes the partial backup on validation failure.
- **CODE-03**: `Store.WithTx` uses `defer tx.Rollback()` so a panic inside the closure returns the connection to the single-connection pool.
- **CODE-05**: `state.Open` takes `ctx context.Context`, uses `db.PingContext(ctx)`, passes `ctx` to `foreignKeyCheck`; all callers updated.
