---
last_reviewed: 2026-07-03
tracks_code: [internal/pathkey/**, internal/scan/**, internal/state/**, internal/sync/**, internal/workspacekeys/**, internal/devicekeys/**, internal/id/**, internal/pairing/**]
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

Rows in `events` are insert-only. The shipped per-hub progress cursor is `hub_cursors` (migration 00008); the richer per-peer `event_delivery` / `sync_cursors` shape is defined in the schema but **not yet wired** (future per-peer optimization). Implementations must not update event payload, HLC, signatures, or hashes in place. Local event creation links each sequential same-device event to the previous event content hash before signing. Local emission sites that also mutate derived state now commit the event and derived row in the same SQLite transaction (`P6-DATA-03`), so an origin device cannot crash after publishing an event row but before recording the namespace/conflict/key-grant state that the duplicate-skip apply path will never re-derive locally. Incoming events with a non-empty `prev_event_hash` must match the previous same-device event already present locally; a missing or mismatched predecessor is treated as a hash-chain break and recorded as an `event_hash_chain_break` conflict.

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

The HLC implementation is mutex-protected for concurrent daemon/agent use. Local outgoing events are stamped through the state store, which persists `(last_hlc, next_seq)` per device in the same SQLite transaction that inserts the event. If the persisted clock row is missing, startup/event creation seeds from `MAX(hlc)` and `MAX(seq)` for the local device so restarts cannot regress or reuse local timestamps. The `(hlc, device_id)` pair is the deterministic tiebreaker. The device id and workspace id are stable generated identifiers, not hardcoded local rows: the device id is minted during `devstrap init` on every device, while the workspace id is minted on the **founder** and adopted by joiners via `devstrap init --join --workspace-id <id>` (P4-SEC-07 pairing — shipped; the id is carried in the same out-of-band exchange as the enrollment keys; runbook: `19_CLOUD_PROVISIONING_GUIDE.md` §E). Phase 0 enforces one local workspace row, and all workspace-scoped tables carry `workspace_id`, so the same logical `ws_...` id is provisioned across devices and every device reads the same r2/s3 hub prefix. A store already initialized under a different id is refused, never rewritten in place.

Event types. **Shipped (emitted/applied today** — `internal/sync`**):**

```text
project.added
project.updated
project.renamed
project.deleted
draft.snapshot.created        # encrypted working-tree bundle (non-git / draft fallback — Layer C)
device.key.granted            # age-wrapped Workspace Content Key for a recipient+(epoch, kid) (P4-SEC-07/P6-SEC-02)
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

**Cursor-wiring status (`ARCH2-02`/`EAGER-02`/`SYNC-01`/`P5-SYNC-01`):** the transport cursor is **per-origin-device sequence numbers** (`hub_device_cursors`, migration 00017), fully decoupled from the HLC, which remains the apply-ordering key only. Every device's own stream is gapless in `Seq` (UNIQUE `events(device_id, seq)`, stamped in the same transaction as the HLC), so `devstrap sync` reads the per-device cursor map, `Pull` returns every event with `Seq > after[device]` for every device stream on the hub (discovery is the hub's job), and `ApplyEvents` returns the per-device **safe cursor**: for each origin device, the end of the contiguous CONSUMED run from `after[dev]+1`. An event is consumed when it applied, deduped (dedup-is-consumption: a device re-pulling its own events advances past them), or permanently quarantined (implausible HLC, verification/divergence failure, undecryptable enc.v2 carrier — re-delivery would fail identically forever). Only transiently-held events stop a run — skew-ahead quarantine and hash-chain breaks — and the hold is scoped to the offending origin device (per-device fault isolation: one skewed device no longer pins the whole fleet's cursor, which the old global HLC low-water mark did). A hub-side seq GAP also stops the run loudly — a safety invariant the HLC cursor never had. That gap rule also changes the SKIP classes' failure mode (`P6-SYNC-02`): an event `EncryptedHub.Pull` skips (malformed envelope, retired enc.v1, anti-downgrade plaintext) leaves a seq gap, so its origin device's cursor now HOLDS at the gap and the object is retried every pull — no longer silently, permanently passed. Since `P6-SYNC-02` (shipped) each drop leaves a durable `sync_skipped_events` record surfaced by `status`/`doctor` and gating `hub gc`, the unknown-version class is grace-bounded, and malformed junk forwards to the undecryptable quarantine — see the **P6-SYNC-02** section below. A contested slot (a forged, consumed carrier at the same `(device, seq)` as a real held event — carrier fields of an undecryptable envelope are unauthenticated) never advances: held dominates consumed. The `HUB-13` inclusive-HLC-boundary overlap is retired — the Seq boundary is exact, so a no-op sync pulls zero. `hub_cursors` (00008) is frozen read-only: the founder gate still consults it so a pre-migration device that ever synced can never self-found (`P6-SEC-02`), and the push watermark backfilled from it once. Build the full-state snapshot exchange **before** enabling hub retention GC.

**Cross-batch late arrivals (`P5-SYNC-01`, shipped 2026-07-03):** an event that lands on the hub **after** a peer's view has moved past its HLC — the "offline device forgot to push, syncs late" scenario DevStrap exists to solve — is now always delivered: the puller resumes each origin device's stream at that device's own contiguous `Seq`, so no appended event can be skipped regardless of its HLC. The R2 layout writes per-device seq-ordered keys (`workspaces/<ws>/eventlog/<device_id>/<seq pad20>_<event_id>.json`); the retired HLC-keyed `events/` prefix stays dual-READ (parsed `(device, seq)` with the same cursor; unparseable keys fail open toward fetching) so pre-migration hubs keep working with no bucket surgery — a follow-up `hub migrate-events` can re-key and delete the legacy prefix. E2e-pinned by `sync_late_push.txtar` (three devices: the founder's view passes the queued event's HLC via a third device before the late push; verified failing on the pre-cursor code). Retention (`ErrSnapshotRequired`) is re-based per device: `after[dev]+1 < minRetainedSeq[dev]` forces the snapshot exchange; the per-device compaction marker is the signed **retention manifest** (`meta/retention.json` — one manifest carrying a floor MAP, so the multi-device floor update is atomic; see *Snapshot exchange* below). The wire format, hub snapshot plane, snapshot import + `ErrSnapshotRequired` recovery, and the `hub compact` producer are shipped (`P4-SYNC-02`/`P4-HUB-11`).

**Push cursor (`SYNC-04`/`P5-SYNC-01`):** the push side is also cursor-bounded, keyed by the gapless local `Seq` (a `push:<hubID>` row in `hub_device_cursors`; the legacy HLC watermark is deliberately NOT trusted for a backfill — inferring "pushed" from `hlc <= watermark` would permanently strand an unpushed regressed-HLC event, the exact loss mode this cursor fixes, so a migrated store simply re-pushes its local history once, idempotent per event ID and an opportunistic re-key into the seq-keyed layout): `devstrap sync` fetches only local-origin events with `Seq > pushCursor` via `LocalPendingEventsBySeq`, pushes them, and advances the watermark to their max Seq. The retired `hlc >` selection could silently strand an event behind the watermark if the local HLC ever regressed relative to seq order; Seq cannot regress. Remote-origin events are never re-pushed (the hub already holds them from their origin device), so a no-op sync pushes zero and the client no longer re-uploads the entire event log every cycle.

**DIRECTION — "one bad object never wedges or silently skips a device" as a tested invariant (AD-6, planned).** The pass-6 criticals (`P6-SYNC-01` whole-batch abort, `P6-SYNC-02` skip-past-cursor, `P6-SYNC-03` sticky-enrollment gap) share one root: the apply/pull path lacks a uniform per-event failure discipline. The forward direction makes this a first-class architectural invariant:

- a persisted `sync_skipped_events` quarantine table (see the P6-SYNC-02 section) surfaced in `status`/`doctor` and gating `hub gc` — **shipped**; replay is automatic (held classes retry at the per-device seq gap; quarantined classes ride `ReplayUndecryptableConflicts`), so no `--replay-skipped` flag exists;
- **record-and-continue** for permanent causes (bad signature, divergent, revoked origin) — shipped for `ApplyEvents` as `event_verification_failure` conflicts with full replay payloads — plus **bounded hold** for possibly-transient causes (pending grant, skew);
- **sticky enrollment** — count `trust_state IN ('approved','revoked','lost')` so revoking the last peer cannot reopen the bootstrap window (`P6-SYNC-03`) — **shipped**;
- a real applied `device.revoked` path so revoked traffic is rejected by trust, not by an aborting signature check;
- **chaos-style multi-device tests** (hostile hub reorder/omit/substitute, mid-rotation approval, revoked-device traffic) in `16_TEST_PLAN.md`.

Current implementation includes the local HLC type, persisted local event stamping with per-device sequence numbers, project event constructors, `add`/`scan --adopt` project-event emission, local previous-event hash linking, content-hash and previous-hash verification, transactional event claim plus side-effect apply, hash-chain break conflict recording, `event_verification_failure` conflict recording for permanent signature/trust/content-hash/divergent failures, HLC-gated project delete tombstones/restores, deterministic replay order, exact duplicate no-ops, divergent duplicate quarantine, order-independent same-path/different-remote conflict reconciliation, a file-backed hub adapter and the live R2/S3 hub adapter (`aws-sdk-go-v2`, `P5-HUB-01`), and user-facing `devstrap sync` (file-backed `--hub-file` or live `hub: r2://<bucket>`), `hub gc`, and `devices revoke` commands. Production peer authentication, remote device registration, full snapshot exchange, and real cross-root skeleton reconciliation remain future work (encrypted payload handling and hub/blob GC are shipped).

## Snapshot exchange and event-log compaction (`P4-SYNC-02` / `P4-HUB-11`)

**Status: wire format, hub snapshot plane, snapshot import + `ErrSnapshotRequired` recovery, the `hub compact` producer, signed per-device sync acks (`P6-HUB-04` completion), and ack-gated tombstone GC + revoked-stream cleanup (`P4-SYNC-06` narrowed) are all shipped.**

The event log grows forever without compaction, and a device whose cursor has fallen below the hub's retention floor (or a fresh joiner after compaction) can no longer catch up incrementally. The snapshot exchange bounds both: a compactor publishes a sealed **full-state snapshot** plus a signed **retention manifest**, then deletes event objects below the published floors.

**Snapshot object (`snapshot.v1`, `internal/sync/snapshot.go`).** The plaintext document is exactly the derived state `applyEventTx` produces: the active namespace map with `source_event_*` coordinates (so import is a pure LWW merge), the latest draft-bundle pointer per draft project, surviving tombstones, per-device **chain anchors** (the content hash of the last covered event per device, so a bootstrapped device can verify the first post-floor event's prev-hash), and the per-device floor map. Device-local state (conflicts, `key_grant_waits`, `sync_skipped_events`) is excluded; grants are NOT embedded — approval re-grants all held epochs as fresh events, which always land above the floor. The document is sealed under the **current-epoch WCK** with the enc.v2 AEAD plane (XChaCha20-Poly1305; the AAD binds workspace id, producing device, sealing key's kid, producer HLC, and epoch — a hub-side carrier mutation is an authentication failure; the envelope's kid field stays an unauthenticated routing hint exactly like enc.v2). Sealing under the current epoch makes each compaction a natural old-epoch retirement boundary: a fresh joiner never needs a retired epoch's key. The sealed object is **content-addressed** (`snapshots/<sha256(bytes)>.json`), so concurrent compactors can never clobber each other — the manifest CAS decides the winner.

**Retention manifest (`meta/retention.json`).** The single mutable head object: `{v, workspace_id, floors: {device→minRetainedSeq}, snapshot: {sha256, epoch, kid, hlc, produced_by}, produced_by, produced_at_hlc, prev_sha256, sig}`, Ed25519-signed under the domain `devstrap:retention:v1` (canonical alphabetical-key payload, same style as the v2 event signature). It is written with compare-and-swap (`If-None-Match: *` create / `If-Match` update; `ErrRetentionConflict` on a lost race) and chained via `prev_sha256` for audit. One manifest with a floor MAP — rather than one marker per device stream — keeps the multi-device floor update atomic.

**Trust model.** Backends read the manifest UNVERIFIED on the pull path (they hold no device registry); an unverified floor can only FORCE the snapshot path — and a garbled manifest is a hard error, never "no floor". Verification is fail-closed at import, with **no pre-enrollment window** (unlike event verification): a snapshot import is wholesale state replacement, so the manifest's `produced_by` must be a locally pinned, **approved** device whose stored signing key verifies the signature; then the fetched object's sha256 must match the manifest; then the AEAD must open under a held WCK candidate. A malicious hub can therefore only DoS (withhold/garble → forced refusal), never inject state. The shipped pairing ceremony makes the fresh-joiner path sound: `init --join --code --fingerprint` pins the founder as approved before the first sync, and the WCK arrives via the verified in-batch grant on the same pull.

**Import (`ImportSnapshot`, `internal/sync/snapshot_import.go`, shipped).** Import is a pure last-writer-wins merge in one transaction — it writes derived namespace state directly from each row's `source_event_*` coordinates, emitting NO synthetic events and fabricating no history, so it is idempotent and order-independent with respect to event replay (import-then-replay and replay-then-import converge, because both resolve every path by the same `(hlc, device_id, event_id)` order). Each entry: skip when a dominating tombstone (`tombstone_hlc >= entry.source_event_hlc`) exists, else overwrite only when the entry's coordinates strictly dominate the stored row's — a snapshot carries pre-reconciled winners, so no same-path/different-remote conflict logic runs. Each tombstone: a newer local add keeps the path; a **dirty** local checkout defers to a `pending_delete_conflict` instead of being destroyed (mirroring the event delete path); otherwise the path is tombstoned by `path_key` so a stale add cannot resurrect it. Each anchor upserts a `sync_chain_anchors` row (the prev-hash fallback for the first post-floor event per device). After the merge commits, the per-device pull cursors advance to `floor-1` (forward-only) and the highest verified floor is cached in `local_meta` (`retention_floor:<hubID>`, monotonic per device) for the rollback guard.

**Recovery (`recoverFromSnapshot`, shipped).** `ErrSnapshotRequired` is no longer a dead end: on it, `devstrap sync` (and `hub gc`'s pre-pull) run one recovery per cycle in this order — (1) get + fail-closed-verify the retention manifest (unapproved/unknown producer or bad signature ⇒ refuse, exit `invalid-config`); (2) floor-rollback guard: a manifest floor below a cached one warns loudly and the higher cached floor drives the cursor math; (3) pull the tail from `max(cursor, floor-1)` FIRST so an in-batch grant is ingested before we unseal (a second `ErrSnapshotRequired` here means the floor raced upward — re-run sync); (4) fetch + sha256-check + unseal the snapshot object under held WCK candidates; a producer whose epoch key this device does NOT hold yet is the **keyless-joiner defer** — print the awaiting-grant message and return without importing (exit 0, next sync retries once the grant lands); (5) cross-check `workspace_id` and that the sealed floor map equals the signed manifest floors; (6) `ImportSnapshot` (sets cursors); (7) pull the blobs referenced by imported draft pointers (they have no carrier event on the tail); then the normal incremental pull re-runs and now succeeds. Verification failure ⇒ refuse and keep state + cursors, never quarantine.

**Producer / compaction (`hub compact`, `internal/cli/hub_compact.go`, shipped).** Only `hub compact` advances floors, and only from a complete view: it runs the SAME completeness gate as `hub gc` (`refuseIfIncompleteView` — clean pull + apply with no deferred/skipped/quarantined/cursor-held events, no open quarantine conflict or `sync_skipped_events`, plus the compaction-specific gate that **no `key_grant_waits` row is open**), then pushes local pending events so `floors[self]` can cover local history. It computes the base floors from the transport cursors (each remote device's floor is `pullCursor+1`, the local device's is `pushWatermark+1`; a device that has consumed nothing gets no floor), then reconciles them against the current manifest: it refuses to build on a manifest whose producer is not the local device or a locally approved device (fail-closed verify), refuses any device whose new floor is BELOW the current one (floors are monotonic — refusal is safer than silently taking the max), and carries forward the floor of any device present in the current manifest but absent from ours. **"Confirmed" is load-bearing**: `PutSnapshotObject` (content-addressed) → sign + CAS `PutRetention` (one re-read-and-retry on a lost race, error on a second) → read the manifest back and confirm it names our snapshot — and ONLY THEN `CompactEventsBelow` deletes the cold events. A crash anywhere leaves a superset of the committed state (safe). The producing device then advances its own pull cursors to the floors (forward-only) — it originated or has already consumed everything the floors cover, so this keeps its next sync incremental rather than demanding a snapshot of its own state. `--min-events N` refuses (before any hub write) unless at least N events would be deleted; `--dry-run` runs the converging pre-sync and prints the plan (floors, delete estimate, snapshot size) while writing nothing; `--keep-snapshots N` (default 2) prunes superseded snapshot objects, always keeping the manifest-referenced one. A keyless device cannot compact (nothing to seal under). Run `compact` from one designated device; concurrent compactions are not yet coordinated (a sweep lock is a follow-up). The rollback guard cache (`retention_floor:<hubID>`) means a hub that walks a floor backward is detected on the next recovery.

**Signed sync acks + tombstone GC (`P4-SYNC-06`, `internal/sync/ack.go`, shipped).** After a **fully-clean** sync cycle — push not deferred; no truncated/skipped/undecryptable pull; no quarantined or cursor-held apply; no open `sync_skipped_events` — `devstrap sync` publishes a small **signed ack marker** to `meta/acks/<device_id>.json`: `{v, cursor: {device→consumedSeq}, device_id, hlc_watermark, produced_at_hlc, pushed_through_seq, workspace_id, sig}`, Ed25519-signed under the domain `devstrap:ack:v1` (canonical alphabetical-key payload, same style as the retention manifest). The write is **last-writer-wins** (each device writes only its own key, unconditional PUT) and **best-effort** (a `PutAck` failure logs a warning and never fails the sync — a missing ack only DELAYS a compactor's tombstone GC, never risks integrity). An unchanged cycle (identical consumed cursor + push watermark) skips the redundant PUT; the HLC clock is deliberately excluded from that comparison (it drifts every cycle, and an unchanged cursor+push means the last published watermark still bounds the consumed set).

`hub compact --gc-tombstones` (default on) is the first production caller of `GCTombstones`. After the completeness gate and BEFORE building the snapshot, it lists every ack, verifies each against the local registry (self via the local signing key, peers via `ApprovedDeviceSigningKey`, and ignores acks from revoked/lost/pending/unknown devices or with a bad signature, key/workspace mismatch, or parse error), and requires a verified ack from **every** approved non-local device — else it SKIPS GC and prints a naming hint. The safe floor is `beforeHLC = min(local live HLC, each verified ack's hlc_watermark)`; tombstones with `tombstone_hlc < beforeHLC` are purged, and because GC runs before `BuildSnapshot` the purged rows are automatically excluded from the produced snapshot. **The tombstone-safety clock:** an ack is written only after a clean cycle whose push watermark reached this device's local max `Seq`, so every event that device mints LATER carries an HLC strictly above its acked watermark — no device can still produce an add below the minimum acked watermark, and a clean cycle consumes the whole hub log so every device has already applied the delete. A later add above the floor is a legitimate restore, not a resurrection.

**Revoked-stream cleanup (`P4-SYNC-06`, shipped).** `devices revoke`/`lost` best-effort delete the revoked device's ack from the hub (when a hub is configured). `hub compact`, after `CompactEventsBelow` and the confirm read-back, reclaims the entire `eventlog/<dev>/` prefix (`DeleteDeviceStream`, covering any residual pre-sequence/legacy objects `CompactEventsBelow` cannot) and deletes the ack of every revoked/lost device whose stream the committed floors fully cover. The device's floor and local pull cursor are **retained** — a floor + cursor for a now-empty stream is harmless, and deleting the cursor while the floor stays would reopen the retention gate and force a needless snapshot recovery on the next sync (the fuller "drop the floor too" variant is deferred as riskier on partial failure). `event_delivery`/`sync_cursors` (migration 00002) remain dead code, superseded by the ack plane.

## Tombstones and deletes

Deletes create HLC-stamped tombstones instead of immediate purges:

```text
project.deleted -> namespace_entries.status=deleted, tombstone_hlc=<event hlc>
```

Incoming `project.added` or `project.restored` events older than the tombstone are ignored. (`project.restored` is planned and has no constructor yet; today restoration happens via a `project.added` event carrying an HLC above the tombstone.) Tombstone GC is now a checkable invariant, not an aspiration: `hub compact --gc-tombstones` purges a tombstone only when its `tombstone_hlc` is below the **minimum HLC watermark across the local device's live clock and every approved non-local device's signed sync ack** (`P4-SYNC-06`; see *Signed sync acks + tombstone GC* above). A missing peer ack skips GC entirely, so no tombstone is purged before a device that could still resurrect it has consumed the delete.

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

The hub is **two planes**, both zero-knowledge: (a) the append-only, signed, HLC-ordered event log — the namespace map — whose payloads are **envelope-encrypted** (`enc.v2`, XChaCha20-Poly1305 under a per-epoch Workspace Content Key with the full carrier tuple bound into the AEAD AAD, `P4-SEC-02`/`SEC-07`/`P6-SYNC-04`) so the hub stores only ciphertext carriers plus the signed carrier fields (ID/DeviceID/Seq/HLC/DeviceSig); and (b) a content-addressed encrypted blob store (`age_blob:<sha256>`) for env and non-git/draft content. The hub sees only ciphertext plus a signed carrier map — it cannot read code, secrets, drafts, or event payloads. Repo content rides git's own transport and never enters the hub. Confidentiality comes from client-side encryption; integrity and availability come from signed event/hash chains, scoped credentials, snapshots, and backups.

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
   AND record the origin's own draft_snapshots row in the same SQLite transaction
   (P6-DATA-01) — the event never re-applies locally, so without the row the
   origin's GC would see its own live bundle as unreferenced
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

Status: shipped (foundation). The event-log payloads are envelope-encrypted at the hub boundary by an `EncryptedHub` decorator (`internal/sync/encryptedhub.go`) wrapping the backend Hub (FileHub or R2Hub). The symmetric layer is XChaCha20-Poly1305 (`chacha20poly1305.NewX`, 24-byte random nonce) under a 32-byte per-epoch Workspace Content Key (WCK); the enc.v2 AAD binds the full carrier tuple — `u32len(ID)||ID || u32len(DeviceID)||DeviceID || u32len(kid)||kid || u64(Seq) || u64(HLC) || u64(epoch)`, big-endian, length-prefixed, with the kid derived from the sealing key (`KIDForWCK`) on both seal and open, never from the unauthenticated envelope field (`P6-SYNC-04`) — so a hub-side mutation of any carrier field is an AEAD authentication failure at decrypt time. The WCK is age-wrapped (X25519) to each approved device recipient and published as a plaintext `device.key.granted` event (the hub cannot decrypt the wrapped WCK without the recipient's private key). The carrier (ID/DeviceID/Seq/HLC/DeviceSig) stays plaintext so hub ordering, dedup, and Ed25519 verification are unchanged; decryption restores Type/PayloadJSON/ContentHash/PrevEventHash before `ApplyEvents` re-derives ContentHash and verifies the signature.

Lifecycle:
- **Init** (`devstrap init`): the **founder** mints the workspace id; a second device does not mint its own but adopts the founder's with `devstrap init --join --workspace-id <id>` (`--workspace-id` implies `--join`; the R2/S3 hub keys under `workspaces/<workspace_id>/`, so a self-minted id keys a disjoint prefix — see the pairing runbook in `19_CLOUD_PROVISIONING_GUIDE.md` §E). Adopting is born-correct: a mismatch against an already-initialized store is refused with a remove-and-reinit remedy, never a post-hoc rewrite. Init does **not** mint a WCK (`P6-SEC-02`, shipped). Founding is deferred to the first `devstrap sync`: the founder/join gate in `runSyncCycle` mints epoch 1 (`EnsureBootstrap`) only when the hub is genuinely empty — **both** the pull and push cursors are 0 (this device has never observed hub content) **and** the pull returned zero raw objects (`EncryptedHub.PullStats.RawSeen == 0`; `RawSeen` alone only proves "nothing new after the pull cursor", so a keyless device whose pull cursor already advanced past quarantined hub events must not found) — and the device did not `init --join`. A device that JOINS an existing workspace (`init --join`, or any device whose first pull sees a non-empty hub) never self-mints a key nobody else holds, so it can never seal its pre-approval events under a never-granted WCK — the SEC-02 data loss. The joiner receives the fleet WCK via `devices approve` (`GrantAllEpochs`) on an existing device; approving another device from a never-synced **founder** still founds defensively, but a **joiner** approving a device while holding no key never self-mints (`grantWorkspaceKeyToApprovedDevice` is founder-gated — it warns and grants nothing, closing the last self-mint path a joiner could reach). WCKs are stored in the OS keychain / 0600 file fallback (`devicekeys.HybridStore`).
- **Approve** (`devices approve` / `enroll --approve`): gated on out-of-band **fingerprint confirmation** before any trust-state write (`P4-SEC-04`, shipped) — the full 256-bit fingerprint binding the device's signing key and age recipient is computed from the row/flags/code being approved and must be confirmed via `--fingerprint`, an interactive `yes`, or (non-TTY) a copy-paste remedy; a keyless placeholder row is refused outright (`SECU-05`). The exchanged values may ride in an unauthenticated `devstrap-pair1:` blob (`devices pairing-code`, `init --join --code`, `devices enroll --code`); integrity still comes from the derived fingerprint, never from the blob. Once confirmed, `GrantAllEpochs(recipient)` wraps every held epoch's WCK to the newly-approved device and emits one `device.key.granted` event per epoch. The new device ingests them on its first pull and decrypts the entire history. On a keyless **joiner**, approve grants nothing (`grantWorkspaceKeyToApprovedDevice` is founder-gated) but the approved row still pins the device's keys and flips verification fail-closed — the `P4-SEC-04` founder-pinning ceremony a joiner runs before its first sync.
- **Periodic rotation** (`keys rotate`, or automatically during `sync` once the active epoch is older than `keys.rotate_max_age`, default 90d): a pure `Rotate` — mint epoch+1, grant to every locally-known approved device, publish on the next push. Deliberately none of revoke's side effects (no secret flags, no blob rewrap, no hub deletes): it bounds FORWARD exposure of a silently compromised key and nothing else. The auto-rotate check runs AFTER the pull (a freshly ingested grant resets the local age, so at most one device in a fleet rotates per deadline instead of storming) and BEFORE the push (grants + events sealed under the new epoch ride one cycle; the peer converges in one pull because grants ingest before decryption — pinned by `sync_rotate_converge.txtar`). Concurrent mints from two devices racing the same deadline are harmless by design: `(epoch, kid)` keying lets both keys coexist at the epoch and each device keeps PUSHING under its own mint until it ingests the other's grant — push-key selection converges on `grant`-origin keys, and readers hold both keys either way, so do not "fix" the transient push-key divergence. Residual (also spec/15): the rotator grants only to approved devices its LOCAL registry knows; an unknown fleet device rides the `P6-SEC-03` grace→quarantine→replay path until re-approved by a device that knows it.
- **Revoke / lost** (`devices revoke` / `lost`): `Rotate` mints a fresh WCK at epoch+1 and wraps it to the remaining `ApprovedRecipients` (the revoked device is already excluded), emitting grant events. Go-forward events encrypt under the new epoch, giving forward secrecy without re-encrypting past events (a revoked device keeps its already-downloaded history — the residual risk age's no-native-revocation model accepts, bounded by secret rotation). The existing blob re-encryption pass runs after the rotate.
- **Pull**: `EncryptedHub.Pull` primes the keyring, **verifies each `device.key.granted` carrier before ingesting its WCK** (`P6-SEC-01`, shipped), ingests the verified in-batch grants in HLC order (so a new device obtains its WCKs before decrypting history), then decrypts `enc.v2` envelopes. The verification uses the `EncryptedHub.Verify` seam wired by `hubFromOptions` to `(*state.Store).VerifyRemoteEvent` (the same content-hash self-consistency check plus `verifyEventSignature` the apply path runs, so the pre-ingest gate rejects exactly the apply-path permanent-failure set), so once any device is approved a grant forged by an unknown/unapproved/bad-signature device — e.g. a hostile hub wrapping an attacker-chosen WCK to the victim's own recipient — is refused and never reaches `StoreWCK`/`RecordKeyEpoch`/the cache; the refused carrier still flows to `ApplyEvents` and is quarantined as an `event_verification_failure` conflict. Before enrollment, grants are accepted (the `P4-SEC-04` bootstrap window). The key-overwrite refusal (`P6-SEC-01` steps b/c) is shipped via `(epoch, kid)` addressing (see the P6-SEC-02 section below): `IngestGrant` computes `kid = hex(sha256(wck))` from the unwrapped bytes, rejects a grant whose carried kid disagrees, and stores each key in its own `(epoch, kid)` slot — nothing ever displaces an existing key (a same-slot custody write additionally byte-compares and refuses a mismatch), and push-key selection prefers verified `grant`-origin keys over `self` mints over `legacy` backfills. Because the hub is untrusted, a single non-conforming object must never wedge sync, so Pull degrades instead of aborting the whole batch: an event whose **(epoch, kid) key has not yet been granted** — a missing epoch, or an unheld kid at a held epoch (the fleet key vs. a legacy self-mint collision) — *truncates* the batch **within a bounded grace window** (`P6-SEC-03`: the decryptable prefix is returned and applies; the cursor advances up to but not past it; the next sync retries once the grant arrives) and **quarantines past it** (the first sighting of the missing key is recorded in `key_grant_waits` through the `MissingKeyWait` seam — the earliest first-seen across every kid at the epoch, so re-pulls and hostile kid relabeling never restart the clock — and once `sync.key_grant_grace` (default 72h, `0` = immediate) has elapsed the still-encrypted carrier is forwarded to the same undecryptable quarantine as an AEAD failure, so the cursor advances, later held-epoch events in the batch still apply, and a grant that eventually arrives recovers the carrier via the replay path), while an **AEAD failure on the held candidate key(s)** (corruption, forgery, or a hub-side carrier mutation; kid-less envelopes try every held key at the epoch) *forwards the still-encrypted carrier* so `ApplyEvents` records a permanent `event_verification_failure` conflict of kind `undecryptable` (`P6-SYNC-04`): visible in `conflicts list`, it blocks `hub gc`, never enters the event log, is never replayed by `devices approve`, and the cursor advances past it — visible refusal without a wedge and without silent loss. Because the defer-vs-quarantine classification necessarily reads the untrusted kid hint, a hostile hub could steer a NOT-yet-granted event into this quarantine by stripping/relabeling the hint; every pull therefore replays open undecryptable conflicts against the keys held then (`ReplayUndecryptableConflicts` in `pullAndApplyEvents`) — once the grant lands the carrier decrypts, applies through the normal verified path, and the conflict auto-resolves, so kid tampering delays that event but cannot destroy it. A **malformed/unknown envelope**, **retired `enc.v1` traffic** (the remedy is re-founding the hub), or a **non-grant plaintext event** (anti-downgrade) is still *skipped with a loud warning* and Pull continues. Bad events are never applied — the security property (no unauthenticated data enters the log) is preserved. **P6-SYNC-02 (shipped):** every drop leaves a durable, self-clearing `sync_skipped_events` record; unknown envelope versions defer per-device within the grace window then quarantine; malformed envelopes forward straight to the undecryptable quarantine; retired v1 and anti-downgrade plaintext hold their origin device's cursor at the seq gap with `status`/`doctor` surfacing and a `hub gc` refusal. See the P6-SYNC-02 section below. (`ErrPlaintextEventFromHub`/`ErrUnknownEnvelopeVersion` still surface from `ParseEncryptedEnvelope`, and `ErrMissingWorkspaceKey` still guards `Push`.)

SQLite holds only non-secret metadata (`workspace_keys`, `workspace_key_grants` — migrations 00013 + 00014, keyed `(workspace_id, epoch, kid)` with an `origin` column); the secret WCK lives only in the keychain / 0600 file fallback, addressed by the same `(epoch, kid)`.

#### DIRECTION — break the wire format once (AD-3, partially shipped)

The break was taken while only the file-hub spike and fresh R2 buckets existed. `enc.v1` and bare-integer epochs are **dead** (not supported legacy): a v1 envelope pulled from a hub is skipped with a loud "re-found the workspace on a fresh hub" warning.

- ~~**`enc.v2`** with a full-carrier AEAD AAD~~ — **shipped** (`P6-SYNC-04`, 2026-07-03): the AAD binds `ID || DeviceID || kid || Seq || HLC || epoch` (length-prefixed strings, big-endian integers), with the kid derived from the sealing key on both seal and open so the envelope's kid field stays a routing hint and a hub relabel cannot wedge a decryptable event. The signature domain moved to `devstrap:event:v2` (payload now includes `device_id` + `seq`); verification accepts v2 then falls back to v1 for re-pushed historical events (residual documented in `15_SECURITY_THREAT_MODEL.md`). A held-key AEAD failure now forwards the carrier to a permanent `undecryptable` quarantine conflict instead of a silent skip;
- ~~a keyring keyed by `(epoch, kid)`~~ — **shipped**: `kid = hex(sha256(wck))` (full digest) rides `DeviceKeyGrant` and the envelope, and the keyring/keystore key by `(epoch, kid)` so self-minted colliding keys never alias (`P6-SEC-01`/`P6-SEC-02`);
- ~~founder-vs-`--join` `init`~~ — **shipped** (`P6-SEC-02`, see the Init lifecycle above);
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

### P6-SEC-02 — A joining device no longer self-bootstraps epoch 1 (founder/join split shipped)

**Status.** The **founder/join split is shipped**, closing the pre-approval data loss. `init` no longer mints a WCK; founding is deferred to the first `sync` and gated on an empty hub (see the Init lifecycle above). `runSyncCycle` now **pulls before it pushes**, and a keyless device's push runs behind the founder/join gate (`pushLocalEventsGated`): a founder on an empty hub mints epoch 1 then pushes; a joiner (or any device whose first pull sees a non-empty hub, `EncryptedHub.PullStats.RawSeen > 0`) DEFERS the push — its local events stay queued behind an unadvanced push cursor and re-push on a later cycle once it is approved and ingests the fleet WCK. `init --join` records `role: joiner` in config as an explicit disambiguator (belt-and-suspenders for the empty-hub race). **Split-brain caveat:** two default-role (founder) devices racing their first sync against the same empty hub can both found, minting different epoch-1 keys; each then defers on the other's events until the devices mutually approve each other (which grants the keys and converges via kid coexistence). Always `init --join` on the second and later machines. E2E `sync_join_flow.txtar` proves a `--join` device's pre-approval project survives and materializes on the founder after approval, with the hub holding only ciphertext throughout.

**`(epoch, kid)` keying — SHIPPED (PR-3b).** The bare-integer-epoch overwrite (`IngestGrant`'s unconditional `StoreWCK` displacing a legacy self-mint) and the concurrent-`Rotate` collision are closed by kid-addressed keys:

```text
kid = hex(sha256(wck))       # full digest; carried in DeviceKeyGrant + the enc.v2 envelope (and bound into its AAD via the sealing key)
keyring key: (epoch, kid)    # colliding self-minted keys never alias — they coexist
```

Mechanics (migration `00014_workspace_key_kids.sql` + `internal/workspacekeys`): every key row records `origin` (`self` = founder bootstrap/rotate, `grant` = verified `device.key.granted` ingest, `legacy` = migration backfill — the only three write paths, `P6-SEC-01c`). `IngestGrant` computes the kid from the unwrapped bytes, rejects a carried-kid mismatch, byte-compares before any same-slot custody rewrite, and never overwrites (`P6-SEC-01b`) — a founder grant at a legacy self-minted epoch lands alongside, and **push-key selection** (`PushKey`: highest epoch, then `grant` > `self` > `legacy`) converges the device onto the fleet key while `GrantAllEpochs` forwards that same preferred key per epoch. On Pull, an envelope naming an unheld kid at a held epoch **truncates** (defers until the grant arrives) rather than skipping — bounded by the `P6-SEC-03` grace window, past which it quarantines recoverably instead — so fleet events are never permanently jumped by a legacy self-minted device; kid-less legacy envelopes fall back to trying every held key at the epoch (the AEAD authenticates, a wrong candidate just fails). Pre-kid rows backfill as `kid=''`/`origin='legacy'` and `Prime` lazily upgrades them (computes the kid, re-stores the custody slot kid-aware, rewrites the metadata row). Pinned by keyring coexistence/kid-mismatch/no-clobber/legacy-backfill tests, envelope kid round-trip tests, and the `TestEncryptedHubUnheldKidTruncates` durability pin.

### P6-SEC-03 — Never-granted epochs no longer truncate forever (grace-bounded quarantine, SHIPPED)

**Was.** `Pull`'s second pass truncated the batch at the first event whose `(epoch, kid)` key this device lacked; `sync.go` advances the cursor only over the applied prefix, so the same blocking event re-fetched and re-truncated forever. A device approved after a `Rotate` minted epoch N (but before the approver pulled its own epoch-N grant) never received epoch N and wedged permanently. Since the `(epoch, kid)` keying landed, the same stall primitive was reachable via a **forged kid**: a hostile hub injecting a well-formed `enc.v2` object naming a held epoch with a random kid (post-#33 review, gpt-5.5). The truncate-vs-skip trade was deliberate (skipping would permanently lose legitimately-decryptable-later fleet events, the P6-SEC-02 durability property) — the fix is a bounded grace window, not a skip.

**Shipped fix.** Three cooperating pieces:

1. **Grace-bounded quarantine at both truncate sites** (`EncryptedHub.MissingKeyWait` + `GraceWindow`, wired by `hubFromOptions` to `Store.NoteMissingKeyGrant` and `sync.key_grant_grace`, default **72h**, `0` = immediate, parsed manually so a malformed value falls back to the default instead of 0). The first sighting of a missing `(epoch, kid)` is recorded in `key_grant_waits` (migration `00015`) with a stable `first_seen_at`; the grace clock is the **earliest first-seen across every kid at the epoch**, so re-pulls — and a hostile hub relabeling the unauthenticated kid hint per pull — cannot restart it. Within the window the pull truncates exactly as before (grant presumed in flight). A wholly-missing epoch records its wait EPOCH-LEVEL (kid `''`) — with no key at the epoch the envelope kid is a useless unauthenticated hint, and persisting it would leave a phantom kid-specific row the real grant's `RecordKeyEpoch` never clears — and a kid not even shaped like `hex(sha256)` at a held epoch is provably never-grantable, so it skips the wait entirely and quarantines immediately (post-#55 Codex review). Past it, the still-encrypted carrier is forwarded to the `P6-SYNC-04` undecryptable quarantine: the conflict is visible in `conflicts list`, blocks `hub gc`, the cursor advances, and **later events at held epochs in the same batch still apply** — the wedge becomes a bounded, recoverable delay. Recovery is the existing replay path: `ReplayUndecryptableConflicts` (now run **before** `ApplyEventsWithStats` in `pullAndApplyEvents`, so a batch [recovered predecessor, same-device successor] converges in ONE cycle instead of quarantining the successor on a broken hash chain) decrypts the preserved carrier once the grant finally lands, applies it through the normal verified path, and auto-resolves the conflict; an event that applies after an earlier hash-chain hold also auto-resolves its `event_hash_chain_break` conflict (`Tx.ResolveOpenConflictsByEventID`). `RecordKeyEpoch` clears satisfied waits (any key at the epoch clears the epoch-level wait; a kid-specific wait clears only on that kid), so the wait table cannot grow past the set of genuinely missing keys. Nil seam = legacy truncate-forever (unit tests only).
2. **Epoch-contiguity guard on approval** (`checkEpochContiguity` in `devices approve` and `devices enroll --approve`, refusing BEFORE any trust write). Approval grants exactly the approver's held epochs, so a hole in `1..max` — or an open `key_grant_waits` row (ciphertext seen, key never granted) — would be inherited by the approved device. The guard names the gap and the remedy; `--allow-epoch-gap` overrides (precedent: `worktree finalize --allow-stale-base`), after which the approved device lands on the grace→quarantine→replay path above until re-approved from a complete device. A device holding NO keys passes trivially: a keyless joiner grants nothing on approve — that approval is the `P4-SEC-04` founder-pinning ceremony and stays friction-free.
3. **Doctor surfacing**: `doctor` warns `awaiting key grants` with each open wait's epoch/kid/first-seen and the re-approve remedy; the wait rows also power the guard in (2).

Pinned by `internal/sync` grace tests (within-grace truncates, expired quarantines + tail still applies, both truncate sites, nil-seam legacy), `internal/state` first-seen-stability/kid-churn/clearing tests, the CLI quarantine-then-recover cycle test (`TestSyncQuarantinesNeverGrantedEpochThenRecovers`), guard tests, and the `sync_never_granted_epoch_wedge.txtar` e2e (revoke-rotated epoch 2, a third device unknown to the rotator quarantines instead of wedging, the guard trips on the laggard, `--allow-epoch-gap` overrides, re-approve recovers).

**Residual (documented, deliberate).** A rotator grants only to the approved devices *it knows locally* (the device registry is per-device), so a fleet device unknown to the rotator always takes the grace→quarantine→replay path after a rotation until any device that knows it re-approves it. Old-epoch containment (retiring long-compromised epochs outright) is documented-not-built.

### P6-SYNC-01 — Signature/trust failures in `ApplyEvents` no longer abort the whole batch

**Status.** Steps 1-2 are shipped: verification failures wrap `state.ErrEventVerification`, and `ApplyEvents` records `event_verification_failure` conflicts for signature/trust/content-hash failures and `ErrDivergentEvent`, then continues applying the rest of the batch. Quarantined events are counted as *consumed* for the cursor (a batch ending in one must not be re-delivered forever by the inclusive pull boundary). `insertEvent` verifies signature/trust **before** the prev-hash chain check — otherwise a revoked device's second chained event would surface as a transient `ErrEventHashChain` (its quarantined predecessor is never inserted) and permanently hold the cursor, reintroducing the wedge. Conflict details carry a machine-readable `kind` (`verification` vs `divergent`) plus the full marshaled `state.Event`, and dedup by event ID (the error string is volatile across trust-state changes). `devices approve` and `devices enroll --approve` replay matching `verification`-kind quarantined events and resolve those that now apply; a replayed `device.key.granted` additionally **ingests its WCK into the keyring** (post-#33 review, gpt-5.5) — `EncryptedHub.Pull` is the only other ingest path and it already advanced past the quarantined carrier, so without replay-time ingestion the granted `(epoch, kid)` would be permanently lost and every fleet event sealed under it would defer forever. `divergent`-kind rows are data-integrity disputes and are never auto-resolved by approval. Remaining gap: `devices revoke` is still local-only, so there is no applied `device.revoked` trust event for other devices to learn automatically; a still-pushing revoked device also grows one open conflict row per distinct poisoned event (bounded aggregation is a follow-up).

**Remaining actionable step.**
1. Ship a real `device.revoked` apply path so revoked events are rejected by synced trust state, not only by the verifier that made the local revoke decision.

```go
if errors.Is(err, state.ErrEventVerification) { insertVerificationConflict(...); continue }
// batch [validC1, revokedB1, validC2] applies C1+C2, records one conflict, advances past all three
```

### P6-SYNC-02 — Pull-dropped events are durably recorded, classified, and self-clearing (shipped 2026-07-03)

**Shipped fix.** The pull's drop classes are now classified by recoverability with a durable record for every drop (`sync_skipped_events`, migration 00018, written through the `EncryptedHub.NoteSkipped` seam with a stable first-seen per (event, reason)):

- **Unknown envelope version** (a newer client's format — decryptable after upgrading this binary): defers the origin device's batch tail per-device within the `sync.key_grant_grace` window (the seq gap holds that device's cursor; the post-upgrade pull consumes the event and clears the record) and hands the carrier to the undecryptable quarantine past it, so one abandoned old client cannot wedge forever on a permanently-newer fleet — the replay recovers it after the upgrade.
- **Malformed envelope** (junk that parses as neither v2 nor any version): FORWARDED to the undecryptable quarantine — the durable, deduped, gc-blocking conflict IS the record; holding on it would let one corrupt object wedge its device forever.
- **Retired enc.v1** and **anti-downgrade plaintext** (a non-grant plaintext event where ciphertext is required): still dropped — never applied — with a durable record; the seq gap holds the origin device's cursor, loudly.

Records surface as `status` "Skipped hub events: N" and a graded `doctor` "skipped hub events" check with per-reason remedies (upgrade / re-found / investigate the hub); `hub gc` refuses to sweep while any record is open (the durable table outlives one pull's in-memory stats). A record clears in the same transaction that finally CONSUMES its event — applied, or deduped when the hub replaces a garbled object with the real one this device already holds. The grant-ingestion skip branches deliberately do NOT write records: a missing grant is already surfaced precisely by `key_grant_waits` and an unverified carrier by its verification conflict — a second row would double-count with no clear lifecycle. There is deliberately NO `sync --replay-skipped`: under the per-device Seq cursor there is nothing to rewind — held classes retry automatically at the gap, and quarantined classes replay via `ReplayUndecryptableConflicts`.
### P6-SYNC-03 — Sticky fail-closed enrollment window (SHIPPED)

**Was.** `hasEnrolledDevices` counted only `trust_state = 'approved'` rows, so revoking the only other device dropped the count to 0, `enrolled=false`, and the final verification gate let non-destructive events from the revoked device — even unknown/unsigned ones — fall through and apply, silently disengaging fail-closed HUB-03.

**Shipped.** Enrollment is sticky: `hasEnrolledDevices` counts

```sql
SELECT COUNT(*) FROM devices WHERE trust_state IN ('approved', 'revoked', 'lost');
```

A revoked/lost row proves a deliberate local operator trust decision happened (revoking a never-approved `pending` device also closes the window — the safe, more-fail-closed direction, since no sync/remote path can inject a revoked/lost row), so revoking (or losing) the last approved device keeps verification fail-closed; auto-created `pending` placeholders from `EnsureRemoteDeviceTx` deliberately do not count, and the genuinely-never-enrolled bootstrap window (`P4-SEC-04`) is unchanged. Post-revoke events from the revoked device or any unknown device land in the `P6-SYNC-01` per-event quarantine (`event_verification_failure` conflicts) instead of applying or aborting the batch. Pinned by `TestHasEnrolledDevicesStickyAfterRevoke` (`internal/state`) and `TestApplyEventsRevokedLastDeviceStaysFailClosed` (`internal/sync`). The deeper fix — synced `device.revoked` trust propagation so *other* devices also learn of the revocation — remains open (tracked with the `P6-SYNC-01` residuals).

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
