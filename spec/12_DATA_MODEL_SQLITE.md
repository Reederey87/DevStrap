---
last_reviewed: 2026-06-28
tracks_code: [internal/state/**]
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

`path_key` is normalized for case-insensitive conflict detection. `source_event_*` records the event coordinates that produced the active namespace entry so same-path/different-remote conflicts can be reconciled deterministically across pull windows.

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

(migration `00008_gitstate_mirror.sql`.) `sync_cursors`, `event_delivery`, `device_sync_state`, and `jobs` are defined but **not yet wired** — `sync` replays full history from HLC 0 (`ARCH2-02`, `DATA-02`); either wire cursor-based resume or mark them deferred.

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

### event_delivery

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

### jobs

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
```

Migrations can be applied by `devstrap init` or explicitly with `devstrap db migrate`.

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

## Audit implementation notes (2026-06-28)

- **DATA-01**: `Backup()` validates the backup with `PRAGMA quick_check` + `foreignKeyCheck` after `VACUUM INTO`; removes the partial backup on validation failure.
- **CODE-03**: `Store.WithTx` uses `defer tx.Rollback()` so a panic inside the closure returns the connection to the single-connection pool.
- **CODE-05**: `state.Open` takes `ctx context.Context`, uses `db.PingContext(ctx)`, passes `ctx` to `foreignKeyCheck`; all callers updated.
