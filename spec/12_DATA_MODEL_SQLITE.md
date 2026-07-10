---
last_reviewed: 2026-07-10
tracks_code: [internal/state/**, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-07-10_PASS7.md]
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
  forge_kind TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);
```

`forge_kind` is the per-project forge override (`GIT-05`, migration 00010); resolution order: `--forge` flag > this column > `[forge]` host map > `DetectForge` heuristic; empty = not set. `remote_url`/`remote_key` are `NOT NULL`, but SQLite accepts `''` against `NOT NULL`. A `git_repo` MUST have a non-empty validated `remote_key`; a remote-less repo is the distinct `local_git` namespace type (see `07_NAMESPACE_AND_SYNC_MODEL.md`), never persisted here with an empty remote (`NOVCS-01`). Add `CHECK (remote_key <> '')`, and consider declaring enum/status tables `STRICT` with `CHECK` constraints generally (`DATA-04`).

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

`draft_projects` backs the non-git / draft folder content sync (`DRAFT-*`, see `07_NAMESPACE_AND_SYNC_MODEL.md` and `09_SECRETS_AND_ENVIRONMENT.md`). Draft content is captured via a `.devstrapignore` compiler (node_modules / build artifacts excluded, rebuilt on hydrate), packed into age-encrypted content-addressed bundles, and referenced by `current_snapshot_id` — never blanket file-synced and never carried as `.git`. `max_bytes`/`max_files` are enforced at capture time (`DRAFT-04`, **shipped**): `devstrap draft snapshot create` loads the per-project limits via `DraftProjectLimits` and `draftbundle.Pack` refuses to pack a tree exceeding either cap (mapped to `exitInvalidConfig`); the same byte budget guards extraction against decompression bombs (`P5-SEC-02`).

### draft_snapshots (shipped)

```sql
CREATE TABLE draft_snapshots (
  id TEXT PRIMARY KEY,
  namespace_id TEXT NOT NULL,
  blob_ref TEXT NOT NULL,                 -- 'age_blob:<sha256>'
  byte_size INTEGER NOT NULL DEFAULT 0,
  file_count INTEGER NOT NULL DEFAULT 0,
  source_event_hlc INTEGER,
  source_event_device_id TEXT,
  source_event_id TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE
);
```

`draft_snapshots` (migration 00009) records each `draft.snapshot.created` bundle for a `draft_project`/`local_git` entry; `draft_projects.current_snapshot_id` points at the newest row, and the packed `age_blob:<sha256>` bundle it references is the live blob the GC must retain. Migration 00012 adds a partial `UNIQUE` index on `(namespace_id, source_event_id)` so idempotent re-apply is DB-enforced (`P5-DATA-02`). The creating device records its own `draft_snapshots` row in the same transaction as the `draft.snapshot.created` event (`Store.InsertLocalEventTx` + `tx.RecordDraftSnapshotTx` inside one `WithTx`, in both `draft snapshot create` and the revoke-rewrap superseding path), so the origin's bundle blob is always retained by GC (`P6-DATA-01`, shipped).

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

Note: `env_ready`/`tooling_ready` exist but are not yet written or read; the derived display status IS computed (`deriveDisplayStatus`, `P5-PROD-01`) from `materialization_state` + `dirty_state` only — expand it to require `env_ready`/`tooling_ready` once those are wired.

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

Status: planned. No `device_gitstate` migration exists yet; add it at the next free migration number at landing time when the Layer A working-state validation plane lands (00010–00023 are now taken — `00023` adds env profile source-event coordinates; see the migration list below). `sync_cursors` and `event_delivery` are defined; `hub_cursors` (00008) is frozen legacy since 00017 (see its section below); `pending_hub_deletes` (00011) backs the revoke-rewrap cleanup queue (`P5-PROD-02`). `device_sync_state` and `jobs` remain unwired.

### env_profiles

```sql
CREATE TABLE env_profiles (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  mode TEXT NOT NULL,
  source_event_hlc INTEGER,
  source_event_device_id TEXT,
  source_event_id TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

Migration `00023_env_profile_source_events.sql` adds the nullable
`source_event_hlc`, `source_event_device_id`, and `source_event_id` columns.
They stamp the `env.profile.updated` event that last wrote the active profile so
cross-device replay is idempotent and LWW by `(HLC, device_id, event_id)`.

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
  runner_pid INTEGER,
  sandbox_backend TEXT NOT NULL DEFAULT '',
  sandbox_mode TEXT NOT NULL DEFAULT '',
  sandbox_limitations TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE CASCADE,
  FOREIGN KEY(worktree_id) REFERENCES worktrees(id) ON DELETE SET NULL
);
```

Current implementation writes `agent_runs` for the thin generic runner. Runs record the associated worktree, engine, task, base ref/SHA, branch, `0600` log path, diff summary, test/command summary, recorder PID, sandbox backend/mode/limitations, and status (`running`, `complete`, `failed`, or `interrupted`). The sandbox fields are unsigned local visibility: `sandbox_backend` is the selected OS backend (`seatbelt`, `bwrap`, `landlock`, or empty when unconfined), `sandbox_mode` records the requested `--sandbox` mode even when `auto` degrades to advisory-only, and `sandbox_limitations` stores a JSON array string for reduced-guarantee backends (empty when none).

### sandbox_violations (shipped — unsigned local sandbox-denial visibility, P4-GIT-03 slice 5)

```sql
CREATE TABLE sandbox_violations (
  run_id TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  backend TEXT NOT NULL,
  operation TEXT NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL,
  FOREIGN KEY (run_id) REFERENCES agent_runs(id) ON DELETE CASCADE
);
CREATE INDEX idx_sandbox_violations_run ON sandbox_violations(run_id);
```

`sandbox_violations` (migration `00022_sandbox_telemetry.sql`, `P4-GIT-03` slice 5) is the unsigned local visibility layer for OS sandbox denials surfaced by the macOS Seatbelt backend after an agent run. It stores only run coordinates plus denial reason fields (`operation`, scrubbed `path`, scrubbed raw `detail`, and `source='seatbelt-log'`), matching the no-secret-material posture of `sync_skipped_events`. It is deliberately **not** the signed `audit_log` described in `spec/15`, which remains unbuilt. Linux runtime denial detection is out of scope for this slice; Linux runs still populate the `agent_runs` sandbox backend/mode/limitations columns.

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

Local event creation links the new event to the previous same-device event content hash, then signs the canonical event payload `(id, hlc, type, payload_json, content_hash, prev_event_hash)` with the local Ed25519 device signing identity. Event insertion verifies `content_hash`, then `device_sig` (when the source device has a known `signing_public_key`), and only then any non-empty `prev_event_hash` against the previous same-device event already stored locally — signature/trust before chain, so an untrusted device's chained successor fails with the permanent verification verdict rather than a transient cursor-holding chain break; unsigned events are accepted only during the pre-enrollment bootstrap window (and always from the local device); once any approved device exists, verification fails closed (`HUB-03`) — events from unknown, keyless, or non-approved devices are rejected for every event type. `Store.VerifyRemoteEvent` exposes the same signature/trust gate without inserting so `EncryptedHub.Pull` can verify a grant carrier before mutating the WCK keyring (`P6-SEC-01(a)`). Sync records an `event_hash_chain_break` conflict when incoming previous-hash validation fails, and records an `event_verification_failure` conflict when permanent signature/trust/content-hash/divergent failures reject one event while the rest of the batch continues.

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

`sync_cursors` was the planned (`DATA-02`, `HUB-*`) per-peer resume point for cursor-based incremental sync. That role is now filled by the shipped `hub_device_cursors` (00017, below): per-origin-device Seq cursors — the cloud hub exposes the same shape as an opaque per-device cursor over its event-log plane (`410 -> snapshot` when a device's cursor falls below its retention floor, see `07_NAMESPACE_AND_SYNC_MODEL.md`). `sync_cursors`/`event_delivery` remain unwired **dead** legacy shapes, now definitively **superseded**: cross-device convergence proof rides the signed **ack plane** (`meta/acks/<device_id>.json`, `P4-SYNC-06` — a hub head object, not a local table) rather than a per-event apply ledger, and the transport resume point is `hub_device_cursors`. Neither table is scheduled to be wired; they are candidates for a future drop migration.

### hub_cursors (legacy — frozen since 00017)

```sql
CREATE TABLE hub_cursors (
  workspace_id TEXT NOT NULL,
  hub_id TEXT NOT NULL,
  last_hlc_applied INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, hub_id),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`hub_cursors` (migration 00008) was the HLC-watermark cursor for cursor-based incremental pull (`EAGER-02`). Since `P5-SYNC-01` (00017) it is **retained frozen, read-only**: the founder/join gate still consults it (a pre-migration device that ever synced must never self-found, `P6-SEC-02`), and the push watermark backfills from its `push:<hubID>` row once. It is never advanced again.

### hub_device_cursors (shipped — per-origin-device transport cursor, P5-SYNC-01)

```sql
CREATE TABLE hub_device_cursors (
  workspace_id TEXT NOT NULL,
  hub_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  last_seq_pulled INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, hub_id, device_id),
  FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
CREATE INDEX idx_hub_device_cursors_hub ON hub_device_cursors(workspace_id, hub_id);
```

`hub_device_cursors` (migration `00017_hub_device_cursors.sql`, `P5-SYNC-01`) decouples the sync transport cursor from the logical HLC clock: for each origin device on the hub, `last_seq_pulled` is the highest **contiguous** per-device sequence number pulled and consumed (applied, deduped, or permanently quarantined). Every device's own stream is gapless in Seq (UNIQUE `events(device_id, seq)`), so an event pushed late — with an HLC far below every peer's old watermark, the "offline device forgot to push" case — can never be skipped: its device's cursor simply hasn't passed its seq. `ApplyEvents` computes the per-device safe cursor (a transient skew/hash-chain hold or a hub-side seq gap stops only that device's advance — per-device fault isolation); `devstrap sync` advances one row per device, forward-only. `device_id` deliberately has **no FK to `devices`**: a cursor may advance for a device whose events all quarantined and that was never enrolled. The push watermark is the `(hub_id = 'push:<hubID>', device_id = <local device>)` row, keyed by the gapless local Seq (the retired `hlc >` selection could strand events behind an HLC regression); there is deliberately no backfill from the legacy `hub_cursors` row — a fresh watermark re-pushes local history once (idempotent), while a backfill could mark a stranded regressed-HLC event as pushed forever.

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

Conflict details are stable JSON so repeated pulls dedup identical open rows. `event_verification_failure` conflicts store `kind` (`verification` — replayable after approval — or `divergent` — a data-integrity dispute that approval never auto-resolves), `event_id`, `device_id`, `hlc`, `seq`, `type`, `error`, and `event_json`; `event_json` is the full marshaled `state.Event` so a later approval replay can call `ApplyEvents` without re-pulling the event from the hub. These conflicts additionally dedup by `event_id` (not exact details) so the same event re-failing with a different error string never opens a second row. `OpenConflictsByType` filters open rows by type for this replay path. `conflicts list/show` pass details through `redact.Scrub` before display — the payload is attacker-influenced remote input.

### pending_hub_deletes (shipped)

```sql
CREATE TABLE pending_hub_deletes (
  workspace_id TEXT NOT NULL,
  blob_ref TEXT NOT NULL,
  queued_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, blob_ref),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`pending_hub_deletes` (migration 00011) queues blobs orphaned by a local-only `devices revoke`/`lost` (no `--hub-file`) for deletion on the next hub-enabled sync (`P5-PROD-02`/`P5-SEC-01`).

### workspace_keys / workspace_key_grants (shipped)

```sql
CREATE TABLE workspace_keys (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  kid TEXT NOT NULL DEFAULT '',                                                       -- 00014: hex(sha256(wck)), '' = legacy
  origin TEXT NOT NULL DEFAULT 'legacy' CHECK(origin IN ('self','grant','legacy')),   -- 00014
  created_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch, kid),                                              -- 00014: was (workspace_id, epoch)
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE TABLE workspace_key_grants (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  recipient TEXT NOT NULL,
  source_event_id TEXT NOT NULL,
  source_event_hlc INTEGER,
  source_event_device_id TEXT,
  created_at TEXT NOT NULL,
  kid TEXT,                                                                           -- 00014: audit only, NULL on legacy grants
  PRIMARY KEY(workspace_id, epoch, recipient),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`workspace_keys`/`workspace_key_grants` (migrations 00013 + 00014) back the Workspace Content Key keyring for envelope encryption of the event log (`P4-SEC-02`/`P4-SEC-07`): `workspace_keys` records which keys this device holds, and `workspace_key_grants` is a membership audit of `device.key.granted` events. Both hold only non-secret metadata — the wrapped WCK itself rides the event payload and the raw WCK lives only in the keychain / 0600 file fallback, never in `state.db`. Migration `00014_workspace_key_kids.sql` (`P6-SEC-02`/`P6-SEC-01b`) re-keys `workspace_keys` by `(workspace_id, epoch, kid)` — `kid = hex(sha256(wck))` (the full digest — a short prefix would leave a preimage-prefix aliasing vector) names the specific key — so two keys minted independently at the same epoch (a joiner's legacy self-mint and the founder's fleet key) coexist instead of overwriting each other, and adds `origin` (`'self'` = founder bootstrap/rotate, `'grant'` = verified `device.key.granted` ingest, `'legacy'` = the migration's backfill of pre-kid rows — the only three paths permitted to write rows, `P6-SEC-01c`). Pre-kid rows are backfilled as `kid=''`/`origin='legacy'` and lazily upgraded to their real kid by `Keyring.Prime`. The down-migration is lossy (kids at the same epoch collapse to one row).

### key_grant_waits (shipped)

```sql
CREATE TABLE key_grant_waits (
  workspace_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  kid TEXT NOT NULL DEFAULT '',   -- '' = whole epoch missing; non-'' = unheld kid at a held epoch (P6-SEC-02 collision)
  first_seen_at TEXT NOT NULL,
  PRIMARY KEY(workspace_id, epoch, kid),
  FOREIGN KEY(workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`key_grant_waits` (migration `00015_key_grant_waits.sql`, `P6-SEC-03`) is the grace-window bookkeeping for workspace keys this device has seen ciphertext for but never been granted. `EncryptedHub.Pull` records the first sighting through the `MissingKeyWait` seam (`Store.NoteMissingKeyGrant`, INSERT-OR-IGNORE then SELECT, so `first_seen_at` is stable across re-pulls); the grace comparison uses the **earliest** `first_seen_at` across every kid at the epoch, so a hostile hub relabeling the unauthenticated envelope kid hint cannot restart the window per label. `RecordKeyEpoch` deletes satisfied rows the moment a matching key is held (an epoch-level `''` wait clears on any key at the epoch; a kid-specific wait only on that kid). Open rows are surfaced by `doctor` ("awaiting key grants") and consulted by the `devices approve`/`enroll --approve` epoch-contiguity guard. No secret material is stored.

### sync_skipped_events (shipped — durable pull-drop record, P6-SYNC-02)

```sql
CREATE TABLE sync_skipped_events (
  workspace_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  device_id TEXT NOT NULL DEFAULT '',
  seq INTEGER NOT NULL DEFAULT 0,
  hlc INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, event_id, reason),
  FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE
);
```

`sync_skipped_events` (migration `00018_sync_skipped_events.sql`, `P6-SYNC-02`) records every event `EncryptedHub.Pull` drops from the batch (`unknown-envelope-version`, `retired-enc-v1`, `plaintext-anti-downgrade`), written through the `NoteSkipped` seam (`Store.NoteSkippedEvent`, INSERT-OR-IGNORE so `first_seen_at` is stable — it is the grace clock for the recoverable unknown-version class). Under the per-device Seq cursor a dropped event is a seq gap that HOLDS its origin device's cursor; these rows are that wedge's visibility: `status` counts them, `doctor` grades them per reason with remedies, and `hub gc` refuses to sweep while any is open. A row clears in the same transaction that finally consumes its event (apply or dedup — `Tx.ClearSkippedEventTx`). No secret material is stored.

### sync_chain_anchors (shipped — snapshot-imported per-device hash-chain anchor, P4-SYNC-02)

```sql
CREATE TABLE sync_chain_anchors (
  workspace_id TEXT NOT NULL,
  device_id TEXT NOT NULL,
  anchor_seq INTEGER NOT NULL,
  anchor_content_hash TEXT NOT NULL,
  anchor_hlc INTEGER NOT NULL,
  snapshot_sha256 TEXT NOT NULL,
  imported_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, device_id)
);
```

`sync_chain_anchors` (migration `00020_sync_chain_anchors.sql`, `P4-SYNC-02`) records, per origin device, the content hash of the LAST event a snapshot covers (at `anchor_seq = floor-1`). A snapshot-bootstrapped device holds no event rows below the retention floor, so `previousEventContentHash` (the prev-hash check in `validatePrevEventHash`) would otherwise fail the first post-floor event of every device forever; the anchor is that check's fallback predecessor. `Tx.UpsertChainAnchor` writes one row per device inside `ImportSnapshot`'s transaction and keeps the HIGHEST `anchor_seq` on conflict (a floor only ever moves forward, so a stale re-import must never lower an anchor). The lookup is by `(device_id, anchor_seq)` — the store enforces a singleton workspace. `snapshot_sha256` records which sealed object carried the anchor, for audit. No secret material is stored.

### blobs (content-addressed encrypted blob index — planned)

**Status: PLANNED — no migration exists.** Planned (`HUB-*`, `DRAFT-*`) local index of the content-addressed encrypted blob store — the hub's second plane (env values + non-git/draft bundles), all age-encrypted client-side and named `age_blob:<sha256>`. The hub sees only ciphertext; this table is the local bookkeeping for what each blob is, whether it is cached locally and/or uploaded, and when it may be reclaimed.

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

-- added by later migrations
CREATE INDEX idx_event_delivery_state ON event_delivery(sync_state);                        -- 00002
CREATE UNIQUE INDEX idx_workspaces_singleton ON workspaces((1));                             -- 00006 (single-workspace invariant)
CREATE INDEX idx_hub_cursors_workspace ON hub_cursors(workspace_id);                         -- 00008
CREATE INDEX idx_draft_snapshots_namespace ON draft_snapshots(namespace_id);                 -- 00009
CREATE UNIQUE INDEX idx_draft_snapshots_source_event
  ON draft_snapshots(namespace_id, source_event_id)
  WHERE source_event_id IS NOT NULL AND source_event_id != '';                                -- 00012 (unsourced rows stay non-unique)
CREATE INDEX idx_workspace_key_grants_epoch ON workspace_key_grants(workspace_id, epoch);    -- 00013
CREATE INDEX idx_events_device_hlc ON events(device_id, hlc);                                 -- 00016
CREATE UNIQUE INDEX idx_devices_single_local ON devices((1)) WHERE trust_state = 'local';      -- 00016
```

`idx_events_device_hlc` (`P6-DATA-05`) serves device-scoped HLC scans such as `LocalPendingEvents`, avoiding a full event-log scan and temporary order-by B-tree on the sync push path. `idx_devices_single_local` (`P6-DATA-06`) enforces the Phase-0 single-local-device invariant at the schema layer; a store that already contains two `trust_state = 'local'` rows fails migration loudly and must be repaired manually by deleting the divergent local-device row after operator review. DevStrap never auto-deletes device identity.

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
  00014_workspace_key_kids.sql
  00015_key_grant_waits.sql
  00016_device_hlc_index_single_local.sql
  00017_hub_device_cursors.sql
  00018_sync_skipped_events.sql
  00019_local_meta.sql
  00020_sync_chain_anchors.sql
  00021_agent_run_runner_pid.sql
  00022_sandbox_telemetry.sql
  00023_env_profile_source_events.sql
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
internal/state/migrations/00014_workspace_key_kids.sql
internal/state/migrations/00015_key_grant_waits.sql
internal/state/migrations/00016_device_hlc_index_single_local.sql
internal/state/migrations/00017_hub_device_cursors.sql
internal/state/migrations/00018_sync_skipped_events.sql
internal/state/migrations/00019_local_meta.sql
internal/state/migrations/00020_sync_chain_anchors.sql
internal/state/migrations/00021_agent_run_runner_pid.sql
internal/state/migrations/00022_sandbox_telemetry.sql
internal/state/migrations/00023_env_profile_source_events.sql
```

The current schema version is **23**. `00010_repo_forge_kind.sql` adds the per-project forge override (`GIT-05`); `00011_pending_hub_deletes.sql` queues blobs orphaned by a local-only revoke for deletion on the next hub-enabled sync (`P5-PROD-02`/`P5-SEC-01`); `00012_draft_snapshot_idempotency.sql` adds a partial `UNIQUE` index on `draft_snapshots(namespace_id, source_event_id)` so idempotency is enforced by the DB, not only the SELECT-then-INSERT guard (`P5-DATA-02`); `00013_workspace_keys.sql` adds the `workspace_keys` and `workspace_key_grants` tables backing the WCK epoch keyring for envelope encryption of the event log (`P4-SEC-02`/`P4-SEC-07`) — `workspace_keys(workspace_id, epoch, created_at)` records which epochs this device holds, and `workspace_key_grants(workspace_id, epoch, recipient, source_event_id, source_event_hlc, source_event_device_id, created_at)` is a membership audit of device.key.granted events (the wrapped WCK itself rides the event payload, never SQLite); `00014_workspace_key_kids.sql` re-keys `workspace_keys` by `(workspace_id, epoch, kid)` and adds `origin` (`P6-SEC-02`/`P6-SEC-01b` — same-epoch keys coexist under content-derived kids instead of overwriting; pre-kid rows backfill as `kid=''`/`origin='legacy'`) and adds the nullable audit `kid` column to `workspace_key_grants`; `00015_key_grant_waits.sql` adds the `key_grant_waits` grace-window table for never-granted workspace keys (`P6-SEC-03`, see its section above); `00016_device_hlc_index_single_local.sql` adds `idx_events_device_hlc` for device-scoped HLC event scans (`P6-DATA-05`) and `idx_devices_single_local` to enforce exactly one local device row (`P6-DATA-06`); `00017_hub_device_cursors.sql` adds the per-origin-device Seq transport cursor table (`P5-SYNC-01`, see its section above) and freezes `hub_cursors` as a read-only legacy row set; `00018_sync_skipped_events.sql` adds the durable pull-drop record (`P6-SYNC-02`, see its section below); `00019_local_meta.sql` adds the `local_meta(key, value, updated_at)` key/value table for machine-local, never-synced decisions — its first consumer is `key_custody` (`keychain` or `file`), the device/workspace secret-custody backend recorded once at init from a keychain reachability probe and honored on every later run so a store never silently migrates backends (`P6-XP-04`; the split-custody wedge that let a headless run mint a divergent signing key). `local_meta` holds no secret material and is intentionally not workspace-scoped — it describes the host, not the synced namespace; `00020_sync_chain_anchors.sql` adds the `sync_chain_anchors(workspace_id, device_id, anchor_seq, anchor_content_hash, anchor_hlc, snapshot_sha256, imported_at)` table backing per-device hash-chain anchors imported from a snapshot (`P4-SYNC-02`, see its section below) — a snapshot-bootstrapped device has no event rows below the retention floor, so the prev-hash verification of the first post-floor event per origin device falls back to its anchor (the content hash of the last covered event, at `seq = floor-1`); `00021_agent_run_runner_pid.sql` adds nullable `agent_runs.runner_pid` so the CLI can distinguish a genuinely running agent recorder from a crash-stuck `running` row and reconcile dead processes to `interrupted` (`P6-GIT-06`); `00022_sandbox_telemetry.sql` adds `agent_runs.sandbox_backend`, `sandbox_mode`, `sandbox_limitations`, and the local `sandbox_violations` table for macOS Seatbelt denial visibility (`P4-GIT-03` slice 5); `00023_env_profile_source_events.sql` adds env profile source-event coordinates for `env.profile.updated` LWW/idempotency (`ENV-SYNC-01`). Migrations can be applied by `devstrap init` or explicitly with `devstrap db migrate`.

## Backup

### DB-only backup

```bash
devstrap db backup ~/.devstrap/backups/state-20260623.db
```

Backups use SQLite `VACUUM INTO`, not file copy, so WAL/SHM state is captured consistently. `db backup` (no `--full`) captures `state.db` **only**. This is a database snapshot, **not a recoverable workspace backup**: a workspace's captured secrets live outside the DB as `age`-encrypted blobs, and the DB holds only `age_blob:<sha256>` string refs. Restoring a lone `state.db` therefore leaves every `age_blob:` ref dangling — `env hydrate` fails and, on the file-custody path, the device identity and WCK epochs needed to decrypt even hub-synced draft blobs are gone. Use `--full` for disaster recovery.

### Full backup / restore (disaster recovery, `P6-DATA-04` — shipped)

```bash
devstrap db backup --full ~/.devstrap/backups/workspace-20260704.tar   # state.db + blobs + keys
devstrap db restore ~/.devstrap/backups/workspace-20260704.tar         # refuses over a non-empty state dir without --force
```

`db backup --full` writes a single uncompressed `tar` archive with a fixed layout; every entry and the output file are mode `0600`:

| Entry                     | Contents                                                                                                    |
|---------------------------|-------------------------------------------------------------------------------------------------------------|
| `state.db`                | `VACUUM INTO` snapshot of the live state database (consistent on a live WAL DB; no exclusive lock).          |
| `config.yaml`             | The workspace config when present — the `hub:` pointer (`r2://<bucket>`), custom `root:`, `workspace_name:`, `role:`, and `keys.rotate_max_age`. Non-secret but still written `0600`. Without it a restored workspace cannot re-pull hub-synced drafts or target a custom root. |
| `blobs/<sha256>.age`      | Every `age`-encrypted blob the DB references via `AllBlobRefs` (env `secret_bindings` + `draft_snapshots`). A referenced blob missing on disk is warned and skipped. |
| `keys/…`                  | The device age identity, the Ed25519 signing key, the WCK epoch keys (`wck-<ws>-<epoch>[-<kid>].key`), and hub S3 credentials when present. |

**Key capture is custody-aware.** Under **file custody** (`DEVSTRAP_NO_KEYCHAIN=1` or no OS keychain) the KeyDir (`~/.devstrap/keys`) already holds the private material, so it is captured verbatim (with a hard-error guard that the device age + signing identities are actually present, symmetric with the escrow path). Under **keychain custody** the private material lives in the OS keychain, not on disk, so `--full` performs a **keychain escrow**: it reads the private identities and every held WCK epoch out of the keychain (via the `devicekeys` `HybridStore`) and writes them into the archive's `keys/` using the file-store on-disk format. If a required secret genuinely cannot be read, the backup **fails loudly naming what could not be captured** rather than silently producing an incomplete "full" archive.

`db restore` extracts the archive and replaces the captured paths — `state.db`, `config.yaml`, `blobs/`, and `keys/` — **in place**. It is not a wipe: anything else already in the state directory (e.g. `quarantine/`, `logs/`) is left untouched. It refuses when any captured target already exists unless `--force` is given (so an un-captured sibling never blocks a restore, and a genuinely fresh state dir restores without `--force`). Extraction is staged in a sibling temp directory, guarded against path traversal (zip-slip: absolute paths and `..` components are rejected; entries are confined to the `state.db`/`config.yaml`/`blobs/`/`keys/` layout; only regular-file entries are accepted), the staged DB is validated with `quick_check` + `foreign_key_check` before any swap, and each captured path is swapped in via a temp `.bak` sibling + rename with rollback, written `0600`.

**Restore runbook (recovering a workspace on a fresh or wiped machine):**

1. Install `devstrap` and retrieve the `--full` archive from wherever you stored it (see the operator duty below).
2. Point `DEVSTRAP_HOME` at the intended state directory (default `~/.devstrap`), or pass `--home`. The captured targets must be absent (or pass `--force` to overwrite them; un-captured siblings are preserved either way).
3. `devstrap db restore <archive.tar>` — reconstructs `state.db`, `config.yaml` (so the `hub:` pointer and custom `root:` come back), `blobs/`, and `keys/`.
4. `devstrap doctor` — confirms the schema, that no blob refs dangle, and reports the recorded key custody.
5. `devstrap sync` — now that `config.yaml` is restored the hub pointer resolves, so hub-synced draft blobs re-pull — and `devstrap env hydrate <path> --write .env` to prove secrets decrypt.

> **Custody note.** A `--full` archive always lands key material as **files** under `keys/`. If the source device used keychain custody, run the restored device under file custody (`DEVSTRAP_NO_KEYCHAIN=1`) or re-migrate the material into the keychain — the restored `state.db` still records the original custody backend, and a keychain-custody store will not read the escrowed files until custody is reconciled. `db restore` prints exactly this guidance when the restored DB records keychain custody.

> **Operator duty — this archive is your Emergency Kit.** Model it on a 1Password Emergency Kit: the `keys/` entries are your **private** age identity, Ed25519 signing key, and Workspace Content Keys. Anyone with the archive can decrypt every captured secret and impersonate the device. Store it **encrypted at rest** (e.g. an encrypted disk image, a password manager's document store, or `age`/`gpg` the tar) and off the working machine. Never commit it to git, never place it inside `~/Code`, and treat its loss the same as losing the keychain itself. Losing the archive **and** the source machine means the workspace's encrypted secrets and draft bundles are permanently unrecoverable.

Workspace export (**planned** — no command exists yet):

```bash
devstrap export --encrypted --output devstrap-snapshot.tar.age
```

This pairs with the human-readable `workspace.yaml` escape hatch in `07_NAMESPACE_AND_SYNC_MODEL.md`.

## Hub backend (planned)

`state.db` is per-device local state. The shared, cross-device data lives in the zero-knowledge **devstraphub**, addressed behind a single pluggable `Hub` interface with two planes (see `03_SYSTEM_ARCHITECTURE.md` and `07_NAMESPACE_AND_SYNC_MODEL.md`):

- the **event log** (the signed, HLC-ordered namespace map), resumed via the `sync_cursors` / `event_delivery` shape above;
- the **content-addressed encrypted blob store** (env + non-git/draft bundles), indexed locally by `blobs`.

The chosen (`HUB-*`) production backend is **Cloudflare R2** (S3-compatible API, zero egress), keyed under a per-workspace prefix so each workspace's objects are namespaced by `workspace_id` (e.g. `s3://<bucket>/workspaces/<workspace_id>/events/...` and `.../blobs/<sha256>`). Because all payloads are age-encrypted client-side and the map is signed, the backend stores only ciphertext plus a signed map and cannot read code, secrets, or drafts. This gives confidentiality by construction; integrity and availability still require scoped credentials, signed hash-chain verification, snapshots/backups, and retention rules. A **file-backed local backend remains only for tests** (`devstrap sync --hub-file <path>`); there is no NAS-first phase. Repo content never transits the hub — it rides git's own transport via blobless clone/fetch from each repo's existing remote. Hub connection settings (backend kind, bucket, region/endpoint, workspace prefix) are configuration, not schema, and never include plaintext credentials in `state.db`.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-DATA-01 — Origin never records its own draft snapshot row, so GC deletes the live bundle — **shipped (2026-07-02)**

**Was.** `draft.go:92` inserted the `draft.snapshot.created` event but wrote no `draft_snapshots` row; the only writer was the sync apply path, which `ApplyEvents` skips for already-present events (`events.go:299`). So on the creating device the blob was referenced by nothing, and `sync` local GC plus `hub gc` deleted the only copies.

**Shipped fix.** `Store.InsertLocalEvent`'s stamping body is extracted into `Store.InsertLocalEventTx(ctx, tx, event)`; `draft snapshot create` and the revoke-rewrap `emitSupersedingDraftSnapshot` now run `InsertLocalEventTx` + `tx.RecordDraftSnapshotTx` inside one `WithTx`, so event and row commit atomically. `DraftSnapshotRef` carries `NamespaceID` so the rewrap path can record the superseding row. Pinned by `TestInsertLocalEventTxMatchesInsertLocalEvent`, `TestDraftSnapshotCreateRecordsOriginSnapshotRow`, `TestRewrapDraftBlobRecordsOriginSupersedingSnapshot`, and the e2e `draft_snapshot_gc_retains_origin.txtar` (create → sync → `hub gc` on the origin → blob survives locally and on the hub).

### P6-DATA-02 — `ClearRotationForProject` filters on a non-existent `env_profiles.namespace_id` column — **shipped (2026-07-03)**

**Was.** The one-arg `env rotate <path>` (flag-clear-only) subquery in `store.go` referenced `env_profiles.namespace_id`, which does not exist (the link is `namespace_entries.env_profile_id`), so it failed on every call with `no such column: namespace_id`. Only `env rotate --all` was tested.

**Shipped fix.** `ClearRotationForProject` now joins through `namespace_entries`, and regression coverage verifies both store-level per-project isolation and the one-arg CLI success path.

**Remaining follow-up.** Add a CI lint that `db.Prepare`s every static query in `store.go` against a migrated in-memory DB.

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

### P6-DATA-04 — `db backup` is incomplete: env blobs and file-fallback keys excluded, no restore path — **shipped (2026-07-04)**

**Was.** `Backup` (`store.go`) was `VACUUM INTO` the `state.db` file only. Encrypted env values live outside the DB as `~/.devstrap/blobs/<hash>.age` and key material lives in `<statedir>/keys` (file custody) or the OS keychain (keychain custody), so a restored DB held dangling `age_blob:` refs; there was no `restore` command and `doctor.go` wrongly recommended restoring from a DB-only backup.

**Shipped fix.**
1. `db backup --full <out.tar>` writes a single `tar` (stdlib `archive/tar`, no new deps) bundling `state.db` (via the reused `Store.Backup` `VACUUM INTO` primitive), `config.yaml` (the `hub:` pointer + custom `root:`, when present), the referenced `blobs/`, and `keys/` — captured verbatim from the KeyDir under file custody (with a hard-error guard that the device age + signing files are present), or escrowed out of the keychain under keychain custody via the new `devicekeys.HybridStore.ExportForBackup` (device age + Ed25519 signing identities, every held WCK epoch, and hub S3 credentials when present). A missing referenced blob is warned and skipped; unreadable required key material fails the backup loudly. Every entry and the output file are `0600`. `db restore <in.tar>` replaces only the captured paths **in place** (leaving un-captured state-dir contents like `quarantine/`/`logs/` intact), refuses when a captured target already exists without `--force`, is zip-slip-guarded, validates the extracted DB (`quick_check` + `foreign_key_check`, via the new exported `state.ValidateDBFile`) before any swap, swaps each captured path via a `.bak` sibling + rename with rollback, and prints the keychain-custody reconciliation guidance when the restored DB records keychain custody.
2. `doctor` gained a "dangling blob refs" check that stats each `AllBlobRefs` entry under `blobs/` and grades a missing one an error.
3. The `doctor` `quick_check` remedy now points at `db backup --full`.

Covered by `db_backup_test.go` (capture → `--full` → wipe → restore → hydrate recovers the same plaintext, with `config.yaml`'s hub pointer round-tripped; restore refuses/forces over a non-empty dir while preserving an un-captured `quarantine/` file; zip-slip rejection), a `devicekeys` escrow test, a doctor dangling-ref test, and the `db_full_backup_restore.txtar` e2e testscript.

```bash
devstrap db backup --full ~/.devstrap/backups/state-20260704.tar   # state.db + blobs + keys (+ keychain escrow)
devstrap db restore ~/.devstrap/backups/state-20260704.tar         # refuses over non-empty state dir without --force
```

### P6-DATA-05 — `events(device_id, hlc)` index shipped

**Status.** Migration `00016_device_hlc_index_single_local.sql` adds `idx_events_device_hlc ON events(device_id, hlc)` so device-scoped HLC scans use an index instead of full-scanning the append-only event log. This serves the sync push path and doctor-style event scans that filter by local device and HLC.

### P6-DATA-06 — Single-local-device invariant shipped

**Status.** Migration `00016_device_hlc_index_single_local.sql` adds `idx_devices_single_local`, a partial unique expression index over rows where `trust_state = 'local'`. `EnsureDevice` now runs inside one transaction: select the local row, otherwise `INSERT ... ON CONFLICT DO NOTHING`, then re-select so a concurrent caller adopts the winner instead of minting a second local identity.

The migration intentionally fails if an existing store already has multiple local rows. Recovery is manual: inspect the divergent identities and delete the wrong `local` row by hand before rerunning migration. Device identity is never auto-deleted.

## Audit implementation notes (2026-06-28)

- **DATA-01**: `Backup()` validates the backup with `PRAGMA quick_check` + `foreignKeyCheck` after `VACUUM INTO`; removes the partial backup on validation failure.
- **CODE-03**: `Store.WithTx` uses `defer tx.Rollback()` so a panic inside the closure returns the connection to the single-connection pool.
- **CODE-05**: `state.Open` takes `ctx context.Context`, uses `db.PingContext(ctx)`, passes `ctx` to `foreignKeyCheck`; all callers updated.
