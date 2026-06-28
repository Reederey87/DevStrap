---
last_reviewed: 2026-06-28
tracks_code: [internal/pathkey/**, internal/scan/**, internal/state/**, internal/sync/**]
---
# Namespace and Sync Model

## Core abstraction

The core object is a **namespace entry**.

A namespace entry maps a stable relative path to an intention:

```text
work/nclh/foc-models → Git repo at git@github.com:org/foc-models.git
experiments/fs2      → encrypted draft project
personal/scripts     → plain managed folder
```

The path is the product.

## Namespace entry example

```yaml
id: prj_01jz8devstrapabc
path: work/nclh/foc-models
type: git_repo
remote: git@github.com:org/foc-models.git
default_branch: main
materialization_policy: lazy
env_profile: snowflake-dev
tooling_profile: python-uv-snowflake
agent_policy: guarded
ignore_profile: default-code
created_at: 2026-06-23T12:00:00Z
updated_at: 2026-06-23T12:00:00Z
```

## Project types

### `git_repo`

Normal managed Git project.

Content source:

```text
Git remote
```

DevStrap syncs:

```text
path, remote, branch, profiles, state
```

DevStrap does not sync:

```text
working tree bytes, .git internals, dependencies
```

**Requirement:** a `git_repo` entry MUST have a non-empty, validated `remote_key`. A git repository with no usable remote is classified `local_git` (below) — never adopted as a clonable `git_repo`. Adopting a no-remote repo as `git_repo` silently breaks hydration on every other device (`NOVCS-01`). `scan --adopt` applies the same remote validation `add` does.

### `local_git`

A git repository with **no usable remote** (just ran `git init`, or the remote is not added yet). Tracked so the path appears everywhere, but its content syncs via an encrypted bundle (like `draft_project`), never via clone. Promote to `git_repo` once a remote is added (`devstrap promote <path> --git-remote <url>`).

### `draft_project`

Small project without remote Git yet.

Content source:

```text
encrypted DevStrap draft bundle
```

Use for:

- experiments;
- scratch tools;
- early prototypes.

Limits:

```text
100 MB default max
5,000 files default max
ignore rules always applied
no plaintext secret files
```

### `plain_folder`

Structure-only folder.

Use for:

- grouping folders;
- documentation buckets;
- local-only areas.

**Status:** `plain_folder` is a documented type but `scan` does not yet emit it; local-only folders without a recognized manifest are currently descended into and dropped (`NOVCS-03`).

### Content-sync status (type ↔ content)

| type | remote required | content sync | hydrate / open |
|---|---|---|---|
| `git_repo` | yes | git clone / fetch | yes |
| `local_git` | no | encrypted bundle* | planned |
| `draft_project` | no | encrypted bundle* | planned |
| `plain_folder` | no | none (structure only) | n/a |

*The encrypted working-tree bundle path (`draft.snapshot.created`, see Draft sync model) is specified but not yet implemented. Until then `hydrate`/`open` on a non-`git_repo` type must return an explicit "local-only; not yet syncable" message, not a misleading clone error (`NOVCS-02`).

## Device state

Each device has local state for every namespace entry.

Example:

```yaml
device_id: dev_macmini_upstairs
path: work/nclh/foc-models
state: ready
local_path: /Users/artem/Code/work/nclh/foc-models
current_branch: main
last_fetch_sha: abc123
local_dirty: false
env_ready: true
tooling_ready: true
last_seen_at: 2026-06-23T12:03:00Z
```

## Materialization states

DevStrap stores readiness as an orthogonal tuple, not one overloaded string:

```text
materialization_state: skeleton | hydrating | available | failed
dirty_state:           unknown | clean | dirty | ahead | behind | diverged | conflicted
env_ready:             true | false
tooling_ready:         true | false
```

Display status is derived from that tuple:

```text
conflicted  dirty_state=conflicted
failed      materialization_state=failed
skeleton    materialization_state=skeleton
hydrating   materialization_state=hydrating
dirty       dirty_state=dirty|ahead|diverged
current     materialization_state=available && dirty_state=clean
ready       current && env_ready && tooling_ready
```

## Event log

DevStrap sync should use append-only events.

Event fields:

```json
{
  "event_id": "evt_01jz...",
  "workspace_id": "ws_01jz...",
  "device_id": "dev_macmini_upstairs",
  "seq": 42,
  "hlc": 115763879690240001,
  "type": "project.added",
  "payload": {},
  "content_hash": "sha256:...",
  "device_sig": "ed25519:...",
  "prev_event_hash": "sha256:...",
  "created_at": "2026-06-23T12:00:00Z"
}
```

Rows in `events` are insert-only. Delivery/apply state lives in `event_delivery`, and per-peer progress lives in `sync_cursors`; implementations must not update event payload, HLC, signatures, or hashes in place. Local event creation links each sequential same-device event to the previous event content hash before signing. Incoming events with a non-empty `prev_event_hash` must match the previous same-device event already present locally; a missing or mismatched predecessor is treated as a hash-chain break and recorded as an `event_hash_chain_break` conflict.

## Clock and ordering

Events are ordered by a Hybrid Logical Clock (HLC), not by wall-clock timestamps.

Rules:

```text
1. created_at is display-only and MUST NOT resolve conflicts.
2. seq is per-device monotonic and used for gap detection.
3. Global replay order is ORDER BY hlc ASC, device_id ASC, id ASC.
4. Apply is idempotent on event_id; duplicate deliveries are no-ops.
5. Incoming events whose causal marker does not descend from the local version are concurrent.
6. Dangerous concurrent events create conflicts instead of overwriting local state.
```

HLC update:

```text
send:
  if physical_now_ms > last_physical_ms: counter = 0
  else counter++
  if counter overflows 16 bits: physical_ms++, counter = 0

receive:
  reject remote timestamps beyond the configured max clock skew
  physical_ms = max(local_physical_ms, remote_physical_ms, physical_now_ms)
  counter follows the standard HLC max/tie rule, with the same overflow guard
```

The HLC implementation is mutex-protected for concurrent daemon/agent use. Local outgoing events are stamped through the state store, which persists `(last_hlc, next_seq)` per device in the same SQLite transaction that inserts the event. If the persisted clock row is missing, startup/event creation seeds from `MAX(hlc)` and `MAX(seq)` for the local device so restarts cannot regress or reuse local timestamps. The `(hlc, device_id)` pair is the deterministic tiebreaker. The device id and workspace id are stable generated identifiers created during `devstrap init`, not hardcoded local rows. Phase 0 enforces one local workspace row, but all workspace-scoped tables still carry `workspace_id` so future pairing can provision the same logical `ws_...` id across devices.

Event types:

```text
workspace.created
device.registered
device.revoked
device.heartbeat
project.added
project.updated
project.renamed
project.deleted
project.restored
repo.remote.changed
env.profile.bound
tooling.profile.bound
agent.policy.bound
draft.snapshot.created        # encrypted working-tree bundle (non-git / draft fallback — Layer C)
repo.gitstate.observed        # signed read-only git-state snapshot (working-state validation plane — Layer A)
repo.wip.pushed               # a WIP commit pushed to refs/devstrap/wip/<device>/<path_key> (recovery — Layer B)
conflict.created
conflict.resolved
```

### Working-state plane (cross-machine "forgot to push")

The human-convenience plane that answers "I forgot to push and I'm now on another machine." It is **strictly separate from the agent plane** — agents always base from `origin/<default_branch>` and the fresh-worktree resolver must never read `refs/devstrap/wip/*`. Three layers (see `AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 5):

- **Layer A — validation (Phase 0):** each device emits `repo.gitstate.observed` (branch, HEAD sha, upstream sha, dirty/untracked/unmerged/ahead/behind/stash counts), captured with `git --no-optional-locks status --porcelain=v2 --branch` so capture never writes `.git/index`. Apply is mirror-only into a sidecar `device_gitstate` table (opaque `device_id`, **no FK** to `devices` since remote devices are not enrolled until Phase 2). `status --all-devices`/`doctor` warn on un-backed-up work and **always render snapshot age** ("never synced / last seen N ago"), never silent all-clear.
- **Layer B — WIP recovery (Phase 1):** `git stash create` (no worktree/index mutation) → `git push origin <sha>:refs/devstrap/wip/<device_id>/<path_key>` over git's integrity-checked transport → emit `repo.wip.pushed`. Forge-agnostic. Machine B fetches into the same ref namespace; `wip apply` materializes into the worktree only on explicit command, never as a branch or base.
- **Layer C — encrypted bundle (Phase 3, narrow):** only for `draft_project`/`local_git`/untracked-only where there is no remote to push a ref to — `draft.snapshot.created` with `internal/envbundle` age encryption.

**Literal continuous file-sync of the working tree is rejected** (git-corruption + invariant violation); see `04_CHALLENGE_MATRIX.md`.

## Sync protocol

Each device maintains a cursor:

```text
sync_cursors(workspace_id, peer_id, last_hlc_applied, last_seq_applied)
```

Sync loop:

```text
1. push local queued events to Hub
2. pull remote events after the peer cursor
3. verify/decrypt where needed
4. apply events to local SQLite in (hlc, device_id) order
5. reconcile local filesystem
6. write event_delivery and sync_cursors transactionally
7. update device heartbeat
```

If the hub no longer retains events after a cursor, the device must fall back to a full-state snapshot plus cursor reset. Silent divergence is not allowed.

### Wire protocol (Phase 2 hub)

The production hub is a thin, zero-knowledge, store-and-forward relay over HTTPS; correctness lives **off the wire** (HLC ordering + content-hash + `prev_event_hash` chain + Ed25519 signatures), so the hub is never trusted to order or authenticate.

```text
POST /v1/{ws}/events              # push (idempotent on event id)
GET  /v1/{ws}/events?after=<hlc>  # catch-up pull; <hlc> = sync_cursors.last_hlc_applied
GET  /v1/{ws}/stream  (SSE)       # live notify only; Last-Event-ID=<hlc>; ': ping' heartbeats; long-poll fallback
PUT/GET /v1/{ws}/blobs/{sha256}   # encrypted bundles, content-addressed (age_blob:<sha256>)
410 Gone {snapshot_required:true} # cursor fell past retention -> full-state snapshot + cursor reset
```

The HLC int64 is simultaneously the ordering key, the resume cursor, and the SSE `Last-Event-ID`. SSE is a freshness hint only; correctness rests on cursor-based pull, preserving the no-daemon guarantee. WebSocket/gRPC/QUIC/P2P/mobile-push are deferred. See `03_SYSTEM_ARCHITECTURE.md` and `AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 6.

**Cursor-wiring status (`ARCH2-02`):** `sync_cursors` and `event_delivery` exist but are not yet wired — `devstrap sync` currently pushes all local events and pulls from HLC 0 every run (full-history replay). Wiring `last_hlc_applied` (push only events past the peer cursor; `Pull(ctx, cursor)`) and setting hub retention so the `410`→snapshot path is reachable is the first Phase-2 step. Build the full-state snapshot exchange **before** enabling hub retention GC.

Current implementation includes the local HLC type, persisted local event stamping with per-device sequence numbers, project event constructors, `add`/`scan --adopt` project-event emission, local previous-event hash linking, content-hash and previous-hash verification, transactional event claim plus side-effect apply, hash-chain break conflict recording, HLC-gated project delete tombstones/restores, deterministic replay order, exact duplicate no-ops, divergent duplicate rejection, order-independent same-path/different-remote conflict reconciliation, a file-backed hub adapter, and a user-facing `devstrap sync --hub-file <path>` command for the file-backed test hub. Production peer authentication, remote device registration, encrypted payload handling, tombstone garbage collection, full snapshot exchange, and real cross-root skeleton reconciliation remain future work.

## Tombstones and deletes

Deletes create HLC-stamped tombstones instead of immediate purges:

```text
project.deleted -> namespace_entries.status=deleted, tombstone_hlc=<event hlc>
```

Incoming `project.added` or `project.restored` events older than the tombstone are ignored. Tombstones can be garbage-collected only after every approved device cursor has advanced beyond the tombstone HLC and the local filesystem is clean or quarantined.

## Conflict detection

Conflict handling is a pure reconciliation function:

```text
Reconcile(local, incoming) -> updated entry OR conflict record
```

The MVP assumes a single writer per path: for the primary persona (one developer with multiple owned devices) a given namespace path is normally mutated on one device at a time. Under that assumption the path/remote conflict class is detect-only — DevStrap surfaces it and never auto-merges — while the safe-automatic class defined in `spec/03` (duplicate skeleton creation, heartbeat latest-wins, recreate-missing-skeleton) may still be resolved without prompting.

Detectors:

- same normalized path with different remotes;
- concurrent renames from the same source;
- delete event against a dirty local checkout;
- add/restore older than a tombstone;
- remote/default-branch changes concurrent with local edits.

On dangerous conflicts, write a `conflicts` row and never auto-overwrite local files. For same-path/different-remote namespace events, the active entry is selected by the canonical event order `(hlc, device_id, event_id)` and the conflict identity is keyed by `path + sorted(remote_key_a, remote_key_b)`. `created_at` is display-only and must not affect the winner.

## Hub storage

Hub stores:

- append-only events;
- device records;
- encrypted env bundles;
- encrypted draft snapshots;
- sync cursors;
- conflict records.

Hub does not store:

- plaintext secrets;
- raw hydrated Git repos;
- dependency folders;
- private keys.

## Hub deployment options

### Option A — Home hub

Run `devstraphub` on Mac Mini or GMK Ubuntu box.

Pros:

- quick for personal use;
- private;
- good for home-lab workflow;
- can be backed up by NAS.

Cons:

- remote access setup needed;
- hub availability tied to home network unless exposed securely.

### Option B — VPS/cloud hub

Run small service on a VPS.

Pros:

- always available;
- easier for cloud agents;
- path to SaaS.

Cons:

- hosting/security burden.

### Option C — Object-store backend

Use encrypted event/blob files in object storage.

Pros:

- simple infrastructure;
- cheap;
- durable.

Cons:

- conflict handling and locking are harder;
- less real-time.

### Option D — Hidden Git backend

Use a private implementation repo for events/manifest.

Pros:

- very fast MVP;
- free remote transport;
- easy audit.

Cons:

- psychologically conflicts with the product promise;
- Git merge conflicts return;
- should not be the long-term user-facing model.

Recommendation:

```text
Phase 1: local-only SQLite.
Phase 2: home-hub HTTP event log.
Phase 3: hosted hub or object-store adapter.
```

## Conflict model

Conflict is a first-class state.

Do not auto-resolve dangerous cases.

### Conflict: same path different remote

Example:

```text
work/api → git@github.com:acme/api.git
work/api → git@github.com:personal/api.git
```

Resolution options:

```text
keep local
use remote
rename one project
mark one unmanaged
```

Current sync replay records the source event coordinates on the active namespace entry. If a competing same-path remote arrives in a later pull window, replay re-evaluates the pair and promotes the lowest `(hlc, device_id, event_id)` winner, then writes the same stable conflict details regardless of arrival order. This is an interim deterministic default; the conflict remains open until a user chooses the final resolution.

### Conflict: same remote multiple paths

Example:

```text
work/api → git@github.com:acme/api.git
work/acme/api → git@github.com:acme/api.git
```

Resolution options:

```text
choose canonical path
move local clone
leave duplicate unmanaged
```

### Conflict: delete vs dirty local

Rule:

```text
Never delete dirty local clone.
Move to quarantine or keep unmanaged.
```

## Delete semantics

Namespace deletion creates a tombstone.

```text
project.deleted event → skeleton removed on clean devices
hydrated dirty devices → mark pending_delete_conflict
hydrated clean devices → move to quarantine, then purge later
```

Quarantine default retention:

```text
30 days
```

## Rename semantics

Rename is metadata-first.

```text
project.renamed old_path new_path
```

On each device:

- if skeleton: rename folder;
- if hydrated clean: move folder;
- if hydrated dirty: mark conflict;
- if target path exists: mark conflict.

## Draft sync model

This encrypted-bundle flow is **Layer C** of the working-state plane: the fallback for `draft_project`/`local_git`/non-git folders and untracked-only content where there is no remote ref to push to. For tracked content in a `git_repo`, the **WIP-ref path (Layer B) is strictly preferred** — git's own integrity-checked transport is safer and cheaper than re-bundling. This flow is specified but **not yet implemented** (no bundle/snapshot code exists today, `NOVCS-02`).

Draft project snapshot:

```text
scan draft folder
apply ignore rules
create tar stream
encrypt for approved devices
upload encrypted blob
emit draft.snapshot.created event
```

Restore:

```text
download encrypted blob
decrypt locally
extract to skeleton path
preserve metadata where possible
```

Draft conflict rule:

```text
If two devices modify the same draft offline, create two snapshots and require manual merge.
```

## Namespace snapshot export

Support disaster recovery:

```bash
devstrap export --output devstrap-workspace-20260623.tar.age
```

Contains:

- namespace entries;
- device records;
- profiles;
- ignore rules;
- encrypted env bundles if requested;
- draft snapshots if requested.

## Audit implementation notes (2026-06-28)

- **SYNC-01**: Same-remote `project.added`/`updated` now checks HLC-dominance before upserting; a stale event (stored coords dominate incoming) is a no-op, ensuring deterministic convergence.
- **SYNC-03**: Added lower-bound HLC validation (`event.HLC <= 0` → quarantine) with `epochFloorMS` constant.
- **SYNC-05/CODE-01**: `ApplyEvents` now `continue`s after recording a hash-chain-break conflict (was `return err`), so the rest of the batch converges.
- **CODE-02**: Removed volatile `OffsetMS` from persisted `skewConflictDetails` so re-delivered skewed events dedup instead of inserting duplicate conflict rows.
