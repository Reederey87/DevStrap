---
last_reviewed: 2026-07-01
tracks_code: [internal/pathkey/**, internal/scan/**, internal/state/**, internal/sync/**, internal/workspacekeys/**, internal/devicekeys/**]
---
# Namespace and Sync Model

## Core abstraction

The core object is a **namespace entry**.

A namespace entry maps a stable relative path to an intention:

```text
work/acme/api        → Git repo at git@github.com:acme/api.git
experiments/scratch  → encrypted draft project
personal/scripts     → plain managed folder
```

The path is the product.

## Namespace entry example

```yaml
id: prj_01jz8devstrapabc
path: work/acme/api
type: git_repo
remote: git@github.com:acme/api.git
default_branch: main
materialization_policy: eager
env_profile: api-dev
tooling_profile: python-uv
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

A git repository with **no usable remote** (just ran `git init`, or the remote is not added yet). Tracked so the path appears everywhere, but its content syncs via an encrypted bundle (like `draft_project`), never via clone. Promote to `git_repo` once a remote is added (planned command: `devstrap promote <path> --git-remote <url>`; not yet implemented — today re-adopt via `devstrap add` after adding the remote).

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

*The encrypted draft-bundle path (`draft.snapshot.created`, see Draft sync model) is **shipped** (`P5-DOC-01`): `internal/draftbundle` packs/extracts age-encrypted, content-addressed bundles, `devstrap draft snapshot create` emits the event, and `materialize`/`sync` extract it on receive. `materialize` on a `local_git`/`draft_project` with no synced bundle yet returns an honest "content sync not yet materialized" interim, classified *skipped* (not failed — `P5-QUAL-01`), never a misleading clone error (`NOVCS-02`). What remains deferred is cross-device recipient enrollment (the live R2/S3 client is shipped, `P5-HUB-01`).

## Device state

Each device has local state for every namespace entry.

Example:

```yaml
device_id: dev_01jz…
path: work/acme/api
state: ready
local_path: /Users/<you>/Code/work/acme/api
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
materialization_state: skeleton | available | materialized-empty | failed   (hydrating: reserved, no writer yet)
dirty_state:           unknown | clean | dirty | ahead | behind | diverged | conflicted
env_ready:             true | false
tooling_ready:         true | false
```

Display status is derived from that tuple by `deriveDisplayStatus` (`internal/cli/status.go`):

```text
conflicted     dirty_state=conflicted
failed         materialization_state=failed
skeleton       materialization_state=skeleton
empty checkout materialization_state=materialized-empty
dirty          dirty_state=dirty|ahead|diverged
current        materialization_state=available && dirty_state=clean
ready          current  (shipped ready = available && clean; env_ready/tooling_ready gating is planned — the fields exist but are unwired, PROD-01)
```

The `hydrating` branch was removed as dead code (no writer ever set it, `P5-PROD-01`); the state remains reserved.

## Event log

DevStrap sync should use append-only events.

Event fields:

```json
{
  "event_id": "evt_01jz...",
  "workspace_id": "ws_01jz...",
  "device_id": "dev_01jz...",
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

Rows in `events` are insert-only. The shipped per-hub progress cursor is `hub_cursors` (migration 00008); the richer per-peer `event_delivery` / `sync_cursors` shape is defined in the schema but **not yet wired** (future per-peer optimization). Implementations must not update event payload, HLC, signatures, or hashes in place. Local event creation links each sequential same-device event to the previous event content hash before signing. Incoming events with a non-empty `prev_event_hash` must match the previous same-device event already present locally; a missing or mismatched predecessor is treated as a hash-chain break and recorded as an `event_hash_chain_break` conflict.

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

Event types. **Shipped (emitted/applied today** — `internal/sync`**):**

```text
project.added
project.updated
project.renamed
project.deleted
draft.snapshot.created        # encrypted working-tree bundle (non-git / draft fallback — Layer C)
device.key.granted            # age-wrapped Workspace Content Key for a recipient+epoch (P4-SEC-07)
conflict.created
conflict.resolved
```

**Planned (no constructor or apply handler yet):**

```text
workspace.created
device.registered
device.revoked                # revoke is local-only today; no applied event path (P6-SYNC-01/03)
device.heartbeat
project.restored              # today restoration happens via a project.added event with HLC above the tombstone
repo.remote.changed
env.profile.bound
tooling.profile.bound
agent.policy.bound
repo.gitstate.observed        # signed read-only git-state snapshot (working-state validation plane — Layer A)
repo.wip.pushed               # a WIP commit pushed to refs/devstrap/wip/<device>/<path_key> (recovery — Layer B)
```

### Working-state plane (cross-machine "forgot to push")

The human-convenience plane that answers "I forgot to push and I'm now on another machine." It is **strictly separate from the agent plane** — agents always base from `origin/<default_branch>` and the fresh-worktree resolver must never read `refs/devstrap/wip/*`. Three layers (see `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 5):

- **Layer A — validation (Phase 0):** each device emits `repo.gitstate.observed` (branch, HEAD sha, upstream sha, dirty/untracked/unmerged/ahead/behind/stash counts), captured with `git --no-optional-locks status --porcelain=v2 --branch` so capture never writes `.git/index`. Apply is mirror-only into a sidecar `device_gitstate` table (opaque `device_id`, **no FK** to `devices` since remote devices are not enrolled until Phase 2). `status --all-devices`/`doctor` warn on un-backed-up work and **always render snapshot age** ("never synced / last seen N ago"), never silent all-clear.
- **Layer B — WIP recovery (Phase 1):** `git stash create` (no worktree/index mutation) → `git push origin <sha>:refs/devstrap/wip/<device_id>/<path_key>` over git's integrity-checked transport → emit `repo.wip.pushed`. Forge-agnostic. Machine B fetches into the same ref namespace; `wip apply` materializes into the worktree only on explicit command, never as a branch or base.
- **Layer C — encrypted bundle (Phase 3, narrow):** only for `draft_project`/`local_git`/untracked-only where there is no remote to push a ref to — `draft.snapshot.created` with `internal/envbundle` age encryption.

**Literal continuous file-sync of the working tree is rejected** (git-corruption + invariant violation); see `04_CHALLENGE_MATRIX.md`.

## Sync protocol

Each device maintains a per-hub cursor — shipped as `hub_cursors(workspace_id, hub_id, last_hlc_applied)` (migration 00008). The richer per-peer shape `sync_cursors(workspace_id, peer_id, last_hlc_applied, last_seq_applied)` plus `event_delivery` is defined in the schema but **not yet wired** (future per-peer optimization):

```text
hub_cursors(workspace_id, hub_id, last_hlc_applied)             # shipped
sync_cursors(workspace_id, peer_id, last_hlc_applied, last_seq_applied)   # planned
```

Sync loop:

```text
1. push local queued events to the hub (only local-origin events with HLC > the push:<hubID> watermark — the push cursor is exclusive)
2. cursor-based incremental pull: GET events with HLC >= hub_cursors.last_hlc_applied — never a full replay from HLC 0 (the pull cursor is inclusive, `HUB-13`, so a same-HLC late arrival from another device is not dropped)
3. verify signatures / decrypt blob refs where needed
4. apply events to local SQLite in (hlc, device_id) order
5. materialize the local filesystem to match the applied namespace (eager clone-everything; see below)
6. advance hub_cursors.last_hlc_applied transactionally (per-peer event_delivery/sync_cursors: planned)
7. update device heartbeat
```

The pull cursor is `hub_cursors.last_hlc_applied`. Because the HLC int64 is simultaneously the global ordering key and the resume cursor, an incremental pull only ever transfers events the device has not already applied — there is no full-history replay on a steady-state sync. A full replay is reserved for the `410 Gone {snapshot_required:true}` recovery path.

If the hub no longer retains events after a cursor, the device must fall back to a full-state snapshot plus cursor reset. Silent divergence is not allowed.

### Sync materialization — eager clone-everything (`EAGER-*`)

`devstrap sync` is **eager clone-everything**, not a lazy/placeholder/VFS scheme. After the namespace events apply (steps 4-5), the device walks every non-deleted entry and brings the whole `~/Code` tree toward `available` in one pass — materializing **by content type**, honoring the file-sync split (never blanket file-sync, never route repo content through the hub):

- `git_repo` → blobless/partial clone or fetch (`git clone --filter=blob:none`) from the entry's **existing** remote, riding git's own integrity-checked transport. Repo content never traverses the DevStrap hub.
- `local_git` / `draft_project` → download the newest `draft.snapshot.created` encrypted bundle from the hub blob store and extract it (see Draft sync model). [shipped, `DRAFT-*`]
- env profiles → decrypt `age_blob:<sha256>` env blobs / resolve provider refs and hydrate the bound env files (see `09_SECRETS_AND_ENVIRONMENT.md`).
- `node_modules` / build artifacts → **never synced**; rebuilt on hydrate from the tooling profile (`npm`/`pnpm`/`uv install`).

After a completed sync the entire tree is present on disk; `materialization_state=skeleton` is only the transient pre-clone state before the first sync finishes, and the `materialization_policy` field is retained for a future opt-in lazy mode (`StrapFS`, `spec/00_START_HERE.md` Phase 4) — it is not the shipped/target default. There is no FUSE/File-Provider materialization in this design. Status today: the eager full-tree clone/fetch/bundle/env pass **is now wired** (`EAGER-01/03/04`); `devstrap sync` blobless-clones every skeleton `git_repo`, hydrates env profiles, and extracts draft bundles with bounded concurrency and per-project failure isolation. See `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`.

### Wire protocol (Phase 2 hub)

The production hub is a thin, zero-knowledge, store-and-forward relay over HTTPS; correctness lives **off the wire** (HLC ordering + content-hash + `prev_event_hash` chain + Ed25519 signatures), so the hub is never trusted to order or authenticate.

```text
POST /v1/{ws}/events              # push (idempotent on event id)
GET  /v1/{ws}/events?after=<hlc>  # catch-up pull; <hlc> = sync_cursors.last_hlc_applied
GET  /v1/{ws}/stream  (SSE)       # live notify only; Last-Event-ID=<hlc>; ': ping' heartbeats; long-poll fallback
PUT/GET /v1/{ws}/blobs/{sha256}   # encrypted bundles, content-addressed (age_blob:<sha256>)
410 Gone {snapshot_required:true} # cursor fell past retention -> full-state snapshot + cursor reset
```

The HLC int64 is simultaneously the ordering key, the resume cursor, and the SSE `Last-Event-ID`. SSE is a freshness hint only; correctness rests on cursor-based pull, preserving the no-daemon guarantee. WebSocket/gRPC/QUIC/P2P/mobile-push are deferred. See `03_SYSTEM_ARCHITECTURE.md` and `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 6.

**Cursor-wiring status (`ARCH2-02`/`EAGER-02`/`SYNC-01`/`HUB-13`):** `hub_cursors` (migration 00008) is wired — `devstrap sync` reads `last_hlc_applied` before `Pull`, passes it as `afterHLC`, and advances it after `ApplyEvents`. `ApplyEvents` returns a **low-water-mark** safe cursor (`SYNC-01`): `min(maxAppliedHLC, lowestUnappliedHLC-1)`, so a transiently-skipped event (skew-ahead quarantine or hash-chain break) with a lower HLC than a higher-HLC applied event is never permanently stranded — the cursor never advances past it, so it is re-delivered next cycle. This protection covers only events that reach `ApplyEvents`; events skipped earlier by `EncryptedHub.Pull` (decrypt failures, malformed envelopes) are dropped before the low-water mark can see them and ARE permanently passed by the cursor — see the **P6-SYNC-02** section below. Permanently-invalid events (`HLC<=0` / below epoch floor) do not hold the cursor. `Pull` uses an **inclusive `>= afterHLC` boundary** (`HUB-13`): packed HLC is not globally unique across devices, so a same-HLC event from another device that arrives after the cursor was advanced to that HLC is still delivered on the next pull; `ApplyEvents`/`InsertEvent` dedup by event ID, so re-delivering the boundary is a no-op for already-applied events (a no-op sync therefore re-pulls only the boundary overlap, deduped — not the whole log). The composite-`(HLC,device,id)` cursor (zero re-delivery) and the `sync_cursors`/`event_delivery` per-peer tables remain available as future optimizations. Build the full-state snapshot exchange **before** enabling hub retention GC.

**Known limitation — cross-batch late arrivals (`P5-SYNC-01`, open):** the SYNC-01 low-water mark only protects events skipped *within the current batch*. An event that lands on the hub **after** a peer has already advanced its cursor past that event's HLC is not re-pulled — exactly the "offline device forgot to push, syncs late" scenario DevStrap exists to solve. The HLC is doing double duty as both the logical ordering key and the transport cursor. The planned fix is to **decouple the transport cursor from the logical clock**: the hub assigns each event an arrival-ordered, monotonically increasing *ingestion position* (an append index for `FileHub`; an ingestion-sequence/timestamp prefix in the R2 object key), the device pulls by that position (so no appended event is ever skipped regardless of HLC), and `ApplyEvents` keeps using HLC strictly for apply ordering. This is a core-engine change best landed as its own focused PR with dedicated multi-device tests, paired with the snapshot/compaction work (`SYNC-02`/`HUB-11`); it is **exposed now that the live R2 hub is wired** (`P5-HUB-01`); the `--hub-file` backend is a single-writer spike where it does not manifest, and the decoupling fix remains a focused follow-up.

**Push cursor (`SYNC-04`):** the push side is also cursor-bounded. `devstrap sync` reads a per-hub `push:<hubID>` watermark from `hub_cursors`, fetches only local-origin events with `HLC > pushCursor` via `LocalPendingEvents`, pushes them, and advances the watermark to their max HLC. Remote-origin events are never re-pushed (the hub already holds them from their origin device), so a no-op sync pushes zero and the client no longer re-uploads the entire event log every cycle.

**DIRECTION — "one bad object never wedges or silently skips a device" as a tested invariant (AD-6, planned).** The pass-6 criticals (`P6-SYNC-01` whole-batch abort, `P6-SYNC-02` skip-past-cursor, `P6-SYNC-03` sticky-enrollment gap) share one root: the apply/pull path lacks a uniform per-event failure discipline. The forward direction makes this a first-class architectural invariant:

- a persisted `sync_skipped_events` quarantine table (see the P6-SYNC-02 section) surfaced in `status`/`doctor` and replayable via `sync --replay-skipped`;
- **record-and-continue** for permanent causes (bad signature, divergent, revoked origin), **bounded hold** for possibly-transient causes (pending grant, skew);
- **sticky enrollment** — count `trust_state IN ('approved','revoked','lost')` so revoking the last peer cannot reopen the bootstrap window (`P6-SYNC-03`);
- a real applied `device.revoked` path so revoked traffic is rejected by trust, not by an aborting signature check;
- **chaos-style multi-device tests** (hostile hub reorder/omit/substitute, mid-rotation approval, revoked-device traffic) in `16_TEST_PLAN.md`.

Current implementation includes the local HLC type, persisted local event stamping with per-device sequence numbers, project event constructors, `add`/`scan --adopt` project-event emission, local previous-event hash linking, content-hash and previous-hash verification, transactional event claim plus side-effect apply, hash-chain break conflict recording, HLC-gated project delete tombstones/restores, deterministic replay order, exact duplicate no-ops, divergent duplicate rejection, order-independent same-path/different-remote conflict reconciliation, a file-backed hub adapter and the live R2/S3 hub adapter (`aws-sdk-go-v2`, `P5-HUB-01`), and user-facing `devstrap sync` (file-backed `--hub-file` or live `hub: r2://<bucket>`), `hub gc`, and `devices revoke` commands. Production peer authentication, remote device registration, full snapshot exchange, and real cross-root skeleton reconciliation remain future work (encrypted payload handling and hub/blob GC are shipped).

## Tombstones and deletes

Deletes create HLC-stamped tombstones instead of immediate purges:

```text
project.deleted -> namespace_entries.status=deleted, tombstone_hlc=<event hlc>
```

Incoming `project.added` or `project.restored` events older than the tombstone are ignored. (`project.restored` is planned and has no constructor yet; today restoration happens via a `project.added` event carrying an HLC above the tombstone.) Tombstones can be garbage-collected only after every approved device cursor has advanced beyond the tombstone HLC and the local filesystem is clean or quarantined.

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

The hub is **two planes**, both zero-knowledge: (a) the append-only, signed, HLC-ordered event log — the namespace map — whose payloads are **envelope-encrypted** (`enc.v1`, XChaCha20-Poly1305 under a per-epoch Workspace Content Key, `P4-SEC-02`/`SEC-07`) so the hub stores only ciphertext carriers plus the signed carrier fields (ID/DeviceID/Seq/HLC/DeviceSig); and (b) a content-addressed encrypted blob store (`age_blob:<sha256>`) for env and non-git/draft content. The hub sees only ciphertext plus a signed carrier map — it cannot read code, secrets, drafts, or event payloads. Repo content rides git's own transport and never enters the hub. Confidentiality comes from client-side encryption; integrity and availability come from signed event/hash chains, scoped credentials, snapshots, and backups.

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

Run `devstraphub` on any always-on home machine (a Mac mini, a small Linux box, or a NAS-adjacent server).

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
Phase 2: logical Hub interface + file-backed conformance backend.
Phase 3: Cloudflare R2 / S3 direct backend with immutable event objects and encrypted blobs.
Phase 4: HTTP/SSE relay only if live push or multi-tenant routing requires a service.
```

**2026-06-28 update (`HUB-*`):** the chosen production backend is now **Cloudflare R2 from the start** (S3 API, zero egress, namespaced by `workspace_id`), realized as Option C (object-store) behind a single pluggable `Hub` interface. R2 event-log objects must be immutable, unique, lexicographically sortable, and created conditionally; steady-state pulls use cursor pagination, not unbounded prefix scans or a single overwritten manifest. There is **no NAS-first / home-hub-first phase**; the Option A/B/D variants above are retained as historical alternatives, and the file-backed adapter (`devstrap sync --hub-file`) is kept **only for tests**. Future compute for the control plane and agent runners is documented (not built) in `03_SYSTEM_ARCHITECTURE.md`; see `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`.

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

This encrypted-bundle flow is **Layer C** of the working-state plane: the fallback for `draft_project`/`local_git`/non-git folders and untracked-only content where there is no remote ref to push to. For tracked content in a `git_repo`, the **WIP-ref path (Layer B) is strictly preferred** — git's own integrity-checked transport is safer and cheaper than re-bundling. The bundle/snapshot layer is **shipped** (`P5-DOC-01`): `internal/draftbundle.Pack`/`Extract` produce age-encrypted, content-addressed `age_blob:<sha256>` bundles with a decompression-bomb budget on every entry (`P5-SEC-02`) and directory-fidelity (`P5-QUAL-05`); `devstrap draft snapshot create` emits `draft.snapshot.created`; and `sync`/`materialize` pull and extract it. A revoke rewrap emits a superseding snapshot event before deleting the old hub ciphertext so peers never lose access (`P5-SEC-01`). Deferred: cross-device recipient enrollment (`NOVCS-02`) (the live network hub / R2-S3 client is shipped, `P5-HUB-01`).

Draft project snapshot (`draft.snapshot.created`, workstream `DRAFT-*`):

```text
1. scan the draft folder
2. apply the .devstrapignore compiler (universal ignore + node_modules/build artifacts excluded; see 11_IGNORE_AND_LOCAL_GARBAGE.md)
3. create a deterministic tar stream
4. age-encrypt for the current approved device recipient set (internal/envbundle)
5. content-address the ciphertext as age_blob:<sha256>
6. PUT the blob to the hub blob store (idempotent; identical content dedups)
7. emit draft.snapshot.created carrying {path_key, age_blob:<sha256>, size, file_count}
```

Restore (pulled during sync materialization for `local_git` / `draft_project`):

```text
1. select the newest draft.snapshot.created for the path in HLC order
2. GET the age_blob:<sha256> from the hub blob store
3. decrypt locally with the device age identity
4. extract to the skeleton path
5. preserve metadata where possible
```

The bundle is content-addressed, so an unchanged draft re-snapshots to the same `age_blob:<sha256>` and uploads nothing; the hub blob store sees only ciphertext keyed by hash, never plaintext or filenames. Because every snapshot is encrypted to the approved recipient *set*, a device revocation forces affected bundles to be re-encrypted to the reduced set (see Device trust and revocation).

Draft conflict rule:

```text
If two devices modify the same draft offline, create two snapshots and require manual merge.
```

## Device trust and revocation

Devices are enrolled and approved per-device (`devstrap devices`, `15_SECURITY_THREAT_MODEL.md`). Encrypted env and draft blobs are age-encrypted to the **set** of approved device recipients, so a trust change carries a cryptographic cost — age has no native revocation, and a recipient that still holds the old key can read any ciphertext it already pulled.

`device.revoked` (revoke / lost) therefore drives two actions:

```text
1. re-encrypt every affected age_blob:<sha256> to the REDUCED recipient set,
   re-upload under its new content hash (only protects FUTURE pulls).
2. flag every secret reachable through the revoked device's env profiles for
   rotation: secret_bindings.needs_rotation, surfaced in `doctor`.
```

Re-encryption shrinks the recipient set for new pulls; **rotation is what actually invalidates already-exposed secret values**, so both steps are required. Status: the `needs_rotation` flag on revoke/lost is shipped; the blob re-encryption pass is shipped (`P5-SEC-01`/`HUB-04`: revoke/lost rewraps affected blobs to the reduced recipient set and deletes superseded hub ciphertext).

**Fail-closed verification (`HUB-03`):** once any approved device enrollment exists, signed-event verification fails CLOSED — an event whose signing key is unknown or not approved is rejected, not applied. Before enrollment (the bootstrap window), only destructive event types (`project.deleted`, `project.renamed`) require verification. The local device is always exempt from the signing-key requirement (pre-enrollment grace). See `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`.

### Workspace Content Key (WCK) envelope encryption (`P4-SEC-02`/`SEC-07`)

Status: shipped (foundation). The event-log payloads are envelope-encrypted at the hub boundary by an `EncryptedHub` decorator (`internal/sync/encryptedhub.go`) wrapping the backend Hub (FileHub or R2Hub). The symmetric layer is XChaCha20-Poly1305 (`chacha20poly1305.NewX`, 24-byte random nonce) under a 32-byte per-epoch Workspace Content Key (WCK); AAD = `event.ID || uint64(epoch)`. The WCK is age-wrapped (X25519) to each approved device recipient and published as a plaintext `device.key.granted` event (the hub cannot decrypt the wrapped WCK without the recipient's private key). The carrier (ID/DeviceID/Seq/HLC/DeviceSig) stays plaintext so hub ordering, dedup, and Ed25519 verification are unchanged; decryption restores Type/PayloadJSON/ContentHash/PrevEventHash before `ApplyEvents` re-derives ContentHash and verifies the signature.

Lifecycle:
- **Init** (`devstrap init`): `EnsureBootstrap` mints epoch 1 WCK, stored in the OS keychain / 0600 file fallback (`devicekeys.HybridStore`, keyed `wck.<workspace_id>.<epoch>`). No self-grant is emitted (avoids epoch collision when a second device joins and its bootstrap WCK is overwritten by the origin's grant on first pull).
- **Approve** (`devices approve` / `enroll --approve`): `GrantAllEpochs(recipient)` wraps every held epoch's WCK to the newly-approved device and emits one `device.key.granted` event per epoch. The new device ingests them on its first pull and decrypts the entire history.
- **Revoke / lost** (`devices revoke` / `lost`): `Rotate` mints a fresh WCK at epoch+1 and wraps it to the remaining `ApprovedRecipients` (the revoked device is already excluded), emitting grant events. Go-forward events encrypt under the new epoch, giving forward secrecy without re-encrypting past events (a revoked device keeps its already-downloaded history — the residual risk age's no-native-revocation model accepts, bounded by secret rotation). The existing blob re-encryption pass runs after the rotate.
- **Pull**: `EncryptedHub.Pull` primes the keyring, ingests in-batch grants in HLC order (so a new device obtains its WCKs before decrypting history), then decrypts `enc.v1` envelopes. Because the hub is untrusted, a single non-conforming object must never wedge sync, so Pull degrades instead of aborting the whole batch: an event whose **epoch key has not yet been granted** *truncates* the batch (the decryptable prefix is returned and applies; the cursor advances up to but not past it; the next sync retries once the grant arrives), while a **held-epoch decrypt failure** (corruption, forgery, or a cross-device epoch-key collision), a **malformed/unknown envelope**, or a **non-grant plaintext event** (anti-downgrade) is *skipped with a loud warning* and Pull continues. Bad events are never applied — the security property (no unauthenticated data enters the log) is preserved. **Known defect (P6-SYNC-02, open):** because skipped events are dropped before `ApplyEvents`, the low-water-mark cursor cannot see them; a skipped event punches a hole in its origin device's hash chain and permanently wedges that device's subsequent events. The skip path does not yet deliver the non-wedging behavior it was designed for; see the P6-SYNC-02 section below. (`ErrPlaintextEventFromHub`/`ErrUnknownEnvelopeVersion` still surface from `ParseEncryptedEnvelope`, and `ErrMissingWorkspaceKey` still guards `Push`.)

SQLite holds only non-secret metadata (`workspace_keys`, `workspace_key_grants` — migration 00013); the secret WCK lives only in the keychain / 0600 file fallback.

#### DIRECTION — break the wire format once (AD-3, planned)

Only the file-hub spike and fresh R2 buckets exist, so the envelope format can still change cheaply. The forward direction is a **single coordinated break** — declare `enc.v1` and bare-integer epochs **dead** (not supported legacy) and land together:

- **`enc.v2`** with a full-carrier AEAD AAD binding `ID || DeviceID || Seq || HLC || epoch` (`P6-SYNC-04`), so a carrier field cannot be swapped without failing decryption;
- a keyring keyed by `(epoch, kid)` where `kid = hex(sha256(wck))` (full digest, or at least a ≥128-bit prefix — **not** 8 hex chars), so self-minted colliding keys never alias (`P6-SEC-01`/`P6-SEC-02`);
- founder-vs-`--join` `init` so a joining device does **not** self-bootstrap epoch 1 (`P6-SEC-02`);
- a signed hub-side retention marker so a truncating hub cannot silently drop history (`P6-HUB-04`).

#### DIRECTION — reduce the crypto surface; seek external review (AD-4, planned)

Three of the four critical pass-6 security findings live in this bespoke WCK epoch/rotation protocol, and the namespace map it protects leaks paths/remotes, not secrets. Forward direction: **evaluate descoping event-log envelope encryption to a simpler per-recipient age-wrap** (the model already proven in the blob plane) unless forward secrecy on the namespace map is a firm requirement; if the epoch design stays, obtain at least **one external cryptographic review before advertising the "zero-knowledge" property** to other users. See `15_SECURITY_THREAT_MODEL.md`.

## Namespace snapshot export

**Planned disaster-recovery export (not yet implemented).** No `export` command exists in `internal/cli` today; the only shipped backup is `devstrap db backup`, which captures `state.db` but **not** blobs or key material (see `P6-DATA-04` in `12_DATA_MODEL_SQLITE.md`). The intended command:

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

### DIRECTION — human-readable escape hatch + recovery drill (AD-7, planned)

The SQLite event log is opaque relative to a human-readable manifest, which raises the trust barrier of a tool that owns `~/Code`. Future direction (not shipped):

- add a plain-text **workspace manifest** export/import (`workspace.yaml` — paths, remotes, profile bindings) as an escape hatch, a team-sharing surface, and an interop format that reconstructs the namespace **without DevStrap**;
- pair it with `db backup --full` / `db restore` (state.db + referenced blobs + key material) — see `P6-DATA-04` in `12_DATA_MODEL_SQLITE.md`;
- back both with a durability/recovery drill in `16_TEST_PLAN.md` (simulate total hub loss and total local loss; prove the tree reconstructs).

## Audit implementation notes (2026-06-28)

- **SYNC-01**: Same-remote `project.added`/`updated` now checks HLC-dominance before upserting; a stale event (stored coords dominate incoming) is a no-op, ensuring deterministic convergence.
- **SYNC-03**: Added lower-bound HLC validation (`event.HLC <= 0` → quarantine) with `epochFloorMS` constant.
- **SYNC-05/CODE-01**: `ApplyEvents` now `continue`s after recording a hash-chain-break conflict (was `return err`), so the rest of the batch converges.
- **CODE-02**: Removed volatile `OffsetMS` from persisted `skewConflictDetails` so re-delivered skewed events dedup instead of inserting duplicate conflict rows.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-SEC-02 — Every `init` self-bootstraps epoch 1, so a joining device's pre-approval events are lost fleet-wide

**Problem.** `init` unconditionally calls `EnsureBootstrap` (`internal/cli/init.go:96`), minting a fresh epoch-1 WCK even on a device *joining* an existing workspace. If B runs `add`/`sync` before A approves it, B's events are sealed under a never-granted epoch-1 WCK — peers fail AEAD and skip them forever, and `IngestGrant`'s unconditional `StoreWCK` (`keyring.go:243`) later overwrites B's own key, destroying hub-based recovery. The same bare-integer-epoch collision fires on concurrent `Rotate`.

**Actionable steps.**
1. Split `init` into founder-bootstrap vs `--join` (empty keyring + `Push` refusal that keeps `ErrMissingWorkspaceKey` when `CurrentEpoch==0`).
2. Add `kid = hex(sha256(wck))` (the full 32-byte digest, or at least a ≥128-bit / 16-byte prefix — **not** 8 hex chars, which is only 32 bits and not collision-resistant) to `DeviceKeyGrant` and the `enc.v1` envelope; key the keyring/keystore by `(epoch,kid)` so colliding keys coexist.
3. Reorder Pull-then-Push for keyless devices; add a testscript asserting B's pre-approval project materializes on A after approve.

```text
kid = hex(sha256(wck))       # full digest (or >=128-bit prefix); carry in DeviceKeyGrant + enc.v1 envelope
keyring key: (epoch, kid)    # colliding self-minted keys never alias
```

### P6-SEC-03 — `Pull` truncates permanently on a never-granted epoch, wedging a validly-approved device

**Problem.** `Pull`'s second pass truncates the batch at the first event whose epoch this device lacks (`internal/sync/encryptedhub.go:159-166`); `sync.go` advances the cursor only over the applied prefix, so the same blocking event re-fetches and re-truncates forever. A device approved after a `Rotate` minted epoch N (but before the approver pulled its own epoch-N grant) never receives epoch N and wedges permanently.

**Actionable steps.**
1. After a bounded retry/grace window, treat a still-missing epoch as permanently undecryptable and **skip** (like the held-epoch branch), recording a conflict/quarantine so a composite `(HLC,device,id)` cursor can pass it.
2. Make grants transitive on approval, or add a contiguity guard to `devices approve` that warns/refuses when the approver's held epochs aren't `1..CurrentEpoch`.
3. Surface the "awaiting workspace key grant" condition (and the re-run-`devices approve` recovery) in `doctor`/sync output; ship the snapshot exchange for late joiners.

```text
# recovery today (undocumented): re-grant every held epoch from a complete device
devstrap devices approve <D>   # clears the wedge
```

### P6-SYNC-01 — Any signature/trust failure in `ApplyEvents` aborts the whole batch and wedges the cursor

**Problem.** Only `state.ErrEventHashChain` gets record-and-continue in `ApplyEvents` (`internal/sync/events.go:306-323`); every other `insertEvent` error (signature/trust, content-hash, `ErrDivergentEvent`) returns before `AdvanceHubCursor` (`cli/sync.go:112-117`), so the poisoned event re-pulls and re-fails forever. Reachable by legitimate revocation traffic: `devices revoke` is local-only, so a revoked device keeps pushing signed events that hit the `trustState != "approved"` gate and abort every remaining device.

**Actionable steps.**
1. Add sentinel `ErrEventVerification`; wrap `verifyEventSignature`/content-hash/`ErrDivergentEvent` returns.
2. Add a per-event quarantine branch alongside the hash-chain branch; advance the cursor for permanent causes (revoked/bad-sig/divergent) only, hold bounded for possibly-transient pending-device failures.
3. Ship a real `device.revoked` apply path so revoked events are rejected by trust, not signature-abort.

```go
if errors.Is(err, state.ErrEventVerification) { insertVerificationConflict(...); continue }
// batch [validC1, revokedB1, validC2] applies C1+C2, records one conflict, advances past all three
```

### P6-SYNC-02 — Skip-on-decrypt-failure advances the cursor past recoverable events and chain-pins later events

**Problem.** `EncryptedHub.Pull` skips `ParseEncryptedEnvelope` failures (incl. `ErrUnknownEnvelopeVersion`) and held-epoch decrypt failures (`encryptedhub.go:149-157,168-177`); skipped events drop before `ApplyEvents`, so the SYNC-01 low-water mark can't hold the cursor and any higher-HLC applied event advances past them for good. The origin device's per-device hash chain then has a hole, so its **next** event hits `ErrEventHashChain` and is held "transiently" forever — the exact soft-brick the decorator claims (spec:570) to prevent.

**Actionable steps.**
1. **Truncate** (not skip) on `ErrUnknownEnvelopeVersion` — decryptable after upgrade, so wedging-until-upgrade is correct.
2. For held-epoch decrypt failures and malformed envelopes, persist a `sync_skipped_events` quarantine row instead of dropping; surface the count in `status`/`doctor`; add `sync --replay-skipped` re-pulling from `min(skipped HLC)`.
3. When an event is skipped, record that the origin's chain is broken so the successor references the root cause instead of being held forever.

```sql
sync_skipped_events(id, device_id, hlc, epoch, reason)   -- surfaced in status/doctor
```

### P6-SYNC-03 — Revoking the last approved device reopens the bootstrap window; revoked-device events are accepted again

**Problem.** `hasEnrolledDevices` counts only `trust_state = 'approved'` rows (`internal/state/store.go:2593-2608`). After revoking the only other device the count is 0, so `enrolled=false` and the final gate (`:2581`) lets non-destructive events from the revoked device — even unknown/unsigned ones — fall through to `Verify` and apply, silently disengaging fail-closed HUB-03.

**Actionable steps.**
1. Make the closed window sticky: count `trust_state IN ('approved','revoked','lost')` (a revoked/lost row proves enrollment completed, excluding auto-created `pending` placeholders), or persist a monotonic `enrollment_closed` flag OR'd into `hasEnrolledDevices`.
2. Test: approve B, revoke B, then a signed `project.added` from B must be rejected (and, with P6-SYNC-01, recorded as a conflict).

```sql
SELECT COUNT(*) FROM devices WHERE trust_state IN ('approved','revoked','lost');
```

### P6-XP-03 — `run-loop` never runs its advertised scan stage, so new local projects never reach the hub

**Problem.** `runLoopTick` calls only `runSyncCycle` — there is no `scan.Walk`/adopt anywhere in `run_loop.go` — yet its `Short`, doc comment, README, and `spec/00` all promise "scan → sync → materialize" (`internal/cli/run_loop.go:32,20-24,69-73`). With the watcher unwired and the daemon deferred, there is no automatic local→hub path: a repo cloned into `~/Code` on A is never adopted and B never sees it.

**Actionable steps.**
1. Add a `scan.Walk` + adopt step before `runSyncCycle` in `runLoopTick`.
2. Make adoption idempotent first: skip findings whose `store.ProjectByPath` row already matches the same `remote_key`/type (else `adoptFindings` at `scan.go:125` appends a duplicate `project.added` every tick).
3. Route secret/symlink-escape/duplicate-remote warning findings to stderr; never auto-adopt them. If the scan stage is deliberately out of scope, correct the `Short`/doc/README/spec-00 text to "sync + materialize" instead.

```go
if res, err := scan.Walk(ctx, opts.paths().Root, scan.Options{IncludePlainFolders: true}); err == nil {
    n, _ := adoptNewFindings(ctx, store, opts.paths().Root, res) // idempotent vs ProjectByPath
    _ = n
}
return runSyncCycle(ctx, opts, stderr)
```

### P6-XP-05 — `scan` makes a serial per-repo network call (`set-head --auto`), stalling offline onboarding

**Problem.** `scan.Walk` calls `Git.DefaultBranch` per repo (`internal/scan/scan.go:154`); when `refs/remotes/origin/HEAD` is missing/stale that runs `git remote set-head origin --auto` — a network round-trip, serially inside the `WalkDir` callback, under the 2-minute per-command timeout with no worker pool or offline mode. Both `scan` and first-run `init` hit it, so 30 no-`origin/HEAD` repos offline turn onboarding into an hour-long stall.

**Actionable steps.**
1. Add a scan-only, local-only default-branch resolver that reads the symbolic ref/packed-refs without invoking `set-head`.
2. Surface a `DefaultBranchStored`/non-authoritative warning in `Finding.Warnings` on fallback.
3. If remote repair must stay reachable from scan, gate it behind an explicit `--online` flag with a short (~5s) timeout and bounded concurrency; leave authoritative resolution to hydrate/worktree materialization.

```go
opts.Git = dsgit.Runner{Timeout: 5 * time.Second} // only if remote repair must stay reachable
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(8) // bounded fan-out instead of serial per-repo calls
```
