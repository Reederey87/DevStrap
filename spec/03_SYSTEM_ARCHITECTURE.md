---
last_reviewed: 2026-07-01
tracks_code: [cmd/**, internal/**, .github/**]
---
# System Architecture

## Overview

DevStrap is a local-first system with a small sync layer.

```text
User interface:
  devstrap CLI
  shell hooks
  editor adapters
  future TUI/menu bar

Local engine:
  devstrapd daemon
  local SQLite state
  filesystem watcher
  namespace reconciler
  Git materializer
  env/secrets broker
  agent worktree manager

Sync layer:
  DevStrap Hub event log
  encrypted blob store
  device registry
```

## Core component diagram

```text
┌────────────────────────────────────────────────────────────────────┐
│                              User                                  │
│  terminal, Claude Code, Cursor, VS Code, agents, Finder, scripts                 │
└───────────────┬────────────────────────────────────────────────────┘
                │
                ▼
┌────────────────────────────────────────────────────────────────────┐
│                         DevStrap Frontends                         │
│  CLI | shell hook | editor adapter | future TUI | future GUI         │
└───────────────┬────────────────────────────────────────────────────┘
                │ Unix socket / local HTTP
                ▼
┌────────────────────────────────────────────────────────────────────┐
│                            devstrapd                               │
├────────────────────────────────────────────────────────────────────┤
│ Namespace Reconciler                                                │
│ Git Materializer                                                    │
│ Worktree Manager                                                    │
│ Secret Broker                                                       │
│ Ignore Compiler                                                     │
│ Device Sync Client                                                  │
│ Watcher Adapter: FSEvents/inotify                                   │
│ Policy Engine                                                       │
│ Job Scheduler                                                       │
└───────────────┬────────────────────────────────────────────────────┘
                │
     ┌──────────┼──────────┬──────────────┬─────────────┐
     ▼          ▼          ▼              ▼             ▼
  SQLite      ~/Code      GitHub/GitLab  Vaults       Hub
  state       tree        remotes        1P/Doppler   event log
```

## Local filesystem layout

```text
~/Code/                                  # user-visible managed namespace
  work/
    company/
      repo-a/
      repo-b/
  personal/
    project-x/
  experiments/
    draft-y/

~/.devstrap/                             # internal state
  state.db
  devstrapd.sock
  logs/
  cache/
    git/
    blobs/
  worktrees/
    repo-a/
      agent-2026-06-23-fix-tests/
  tmp/
  config.yaml
```

## Main local processes

### `devstrap`

The command-line interface.

Responsibilities:

- initialize workspace;
- scan/adopt projects;
- trigger hydration;
- open editor;
- create worktrees;
- capture/hydrate env;
- show status;
- interact with daemon through IPC.

### `devstrapd`

The local daemon.

Responsibilities:

- watch managed tree;
- sync namespace events;
- reconcile skeletons;
- run queued jobs;
- maintain device state;
- enforce policy;
- serve local API;
- write logs and audit events.

**Engine seam (`ARCH2-01`):** these responsibilities (reconciler, materializer, worktree manager, secret broker, policy engine) are today implemented as Cobra command closures inside `internal/cli`, not a separate package. Extract a thin `internal/engine` exposing intent-level operations (`Hydrate`, `NewWorktree`, `RunAgent`, `Sync`) so the daemon's job handlers and the CLI call the same core — otherwise the daemon phase must begin with a large, risky extraction from `internal/cli`.

### `devstraphub`

The **two-plane, zero-knowledge** sync service (`HUB-*`, see `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`). It is deliberately split by content type so that no single channel ever blanket-syncs files or touches `.git` (which would corrupt repos):

**Plane A — the namespace map (event log).** An append-only, Ed25519-signed, HLC-ordered event log. This is the *map of all projects* in the workspace: paths, types, remotes, env/tooling/ignore profiles, draft metadata, and tombstones. Each device replays from its last cursor to reconstruct the identical `~/Code` tree.

**Plane B — the encrypted blob store (content-addressed).** A content-addressed store of age-encrypted `age_blob:<sha256>` blobs holding env profiles and non-git / draft / plain-folder content. Blobs are encrypted client-side to the enrolled device recipient set before upload; the hub stores opaque ciphertext keyed by its SHA-256.

What never flows through the hub:

- **Repo content rides git's own transport**, not the hub — a blobless/partial clone/fetch (`git clone --filter=blob:none`) from the project's *existing* remote (`08_GIT_MATERIALIZATION_AND_WORKTREES.md`). Repo bytes never pass through `devstraphub`.
- **`node_modules` / build artifacts are never synced** — they are rebuilt on hydrate (`npm`/`pnpm`/`uv install`), per the `.devstrapignore` compiler (`11_IGNORE_AND_LOCAL_GARBAGE.md`).

Responsibilities:

- store the append-only, signed, HLC-ordered namespace event log (Plane A);
- store content-addressed age-encrypted env + draft/non-git blobs (Plane B);
- store device heartbeats and enrollment/trust metadata;
- never store plaintext secrets, code, or drafts — **the hub sees only ciphertext plus a signed map**;
- support offline-first sync.

Because the hub only ever holds ciphertext blobs plus a signed event map, it cannot read code, secrets, or drafts. That gives tenant **confidentiality** by construction for any future multi-user deployment. Integrity and availability still require access controls, signed hash chains, fail-closed event verification, snapshots/backups, and scoped credentials.

**Hub backend is pluggable behind one interface** (`HUB-*`). The chosen **production backend is Cloudflare R2 from the start** (S3-compatible API, zero egress, namespaced by `workspace_id`; confidentiality via client-side age encryption — there is no NAS-first phase). A **file-backed local backend remains only for tests** (today's `devstrap sync --hub-file` spike). An **HTTP/SSE hub service** (the wire protocol below) is the later networked backend. See the `Hub` boundary under *Platform adapter boundaries*.

R2 event-log correctness is part of the DevStrap design, not delegated to object-storage locking. Events are written as immutable, unique, lexicographically sortable objects under a workspace prefix and created with conditional put semantics (`If-None-Match: *` where supported). Pulls page by prefix and cursor; they must never append by overwriting one shared manifest object. The signed event hash chain and local replay rules detect reordering, omission, substitution, and duplicate replay.

Wire protocol — *planned* networked backend (see `07_NAMESPACE_AND_SYNC_MODEL.md`, `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 6, and `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`): a thin, **zero-knowledge**, store-and-forward relay over HTTPS — `POST /v1/{ws}/events`, `GET /v1/{ws}/events?after=<hlc>`, SSE `GET /v1/{ws}/stream` (Last-Event-ID=HLC, a live hint only), content-addressed `PUT/GET /v1/{ws}/blobs/{sha256}`, `410 Gone` → full-state snapshot. The hub is **semi-trusted**: it sees only signed, end-to-end-encrypted payloads plus routing metadata, never plaintext code/secrets, and is never trusted to order or authenticate (correctness lives off the wire via HLC + content/prev-hash chain + Ed25519). Device auth via mTLS client certs derived from the device identity, rejecting revoked/lost devices. As a single Go binary it ships in the same module, reusing `internal/state`, `internal/sync`, and `internal/devicekeys`.

#### Hosting & scaling (FUTURE direction — documented, not built)

This subsection records the target deployment shape (`SCALE-*`, `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md` decision 6). None of it is implemented this cycle; the cross-platform Go core and the R2/file-backed hub backends come first.

Chosen stack:

- **Compute — Fly.io** for the control plane and agent runners. Firecracker microVMs suit Go-native control services and per-task runners; use current Fly regions (`fly platform regions`) rather than hard-coding a region count. Runner Machines must be isolated from the control-plane app/org/process boundary, receive only per-task scoped credentials, and be destroyed after untrusted tasks rather than suspended with live memory.
- **Sync hub — Cloudflare R2** (as above), namespaced by `workspace_id`; client-side encryption provides confidentiality, while prefix-scoped temporary credentials, hash-chain verification, and snapshots/backups preserve integrity and availability.
- **Control-plane DB — managed Postgres** (Neon default; Supabase/Render/Railway as alternatives) for non-secret control-plane metadata. Runtime code should use a provider-neutral `ControlStore` boundary with separate pooled runtime and direct migration/admin DSNs when the hosted control plane exists.

Rejected as the *primary* platform, with reasons:

- **Railway** — shared-kernel containers; fine for the control plane or a single trusted self-hosted instance, but not safe for untrusted multi-tenant code.
- **Vercel** — strong *if* the stack were Next.js/TS (Sandbox + Functions/Workflows), but DevStrap is Go-first, so its TS/Python sandbox SDKs are an awkward fit.
- **Hetzner** — cheapest always-on box and good for the solo MVP, but no microVM isolation, no global footprint, no scale-to-zero.

Multi-tenant scaling principles (for the eventual SaaS): a **control-plane / data-plane split**, a **tenancy spectrum** from pooled to dedicated/BYOC, and **cell-based scaling**. **Coder** is the reference architecture for running agents on the operator's own infrastructure at scale.

### No-daemon mode (correctness guarantee)

Every `devstrap` CLI command works correctly without the daemon. State is materialized on demand, and reconciliation today is the **explicit `devstrap scan`** — there is no periodic-scan reconciler yet (`ARCH2-04`); periodic reconciliation arrives with the daemon. No command depends on `devstrapd` being installed or running. The daemon is purely a performance/UX optimization — its filesystem watcher is a hint, not the source of truth — and is never a correctness dependency. If the daemon is absent, stopped, or behind, results stay correct; only freshness and latency degrade until the next on-demand materialization or the next explicit `devstrap scan`. (The daemon socket/IPC/job API in `13_CLI_DAEMON_API.md` and the reserved `exitDaemonUnavailable=3` are design intent for the M5 daemon, not shipped behavior.)

## Data flows

### New project created locally

```text
1. User creates ~/Code/experiments/fs2.
2. Watcher detects directory create.
3. Scanner classifies it: Git repo, draft project, or plain folder.
4. Namespace event is written locally.
5. Daemon syncs event to Hub.
6. Other devices receive event.
7. Skeleton directory appears on other devices.
```

### Project materialized on another machine (eager clone on sync)

Materialization is **eager, not lazy** (`EAGER-*`, `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`): `devstrap sync` clones everything up front (blobless/partial clone), so after a sync the whole `~/Code` tree is present rather than a skeleton awaiting first open. There is no FUSE / placeholder / lazy-VFS magic in this design — StrapFS stays explicitly deferred.

```text
1. Machine B runs devstrap sync.
2. It replays the namespace map (Plane A) to learn every project, path, type, and remote.
3. For each Git repo: blobless/partial clone (git clone --filter=blob:none) from the project's existing remote — repo content rides git's own transport, never the hub.
4. For each draft / non-git / plain folder: download + age-decrypt its content-addressed blob (Plane B).
5. Env profiles are hydrated from their encrypted blobs (or resolved via provider refs).
6. node_modules / build artifacts are rebuilt on hydrate (npm/pnpm/uv install), never synced.
7. The whole ~/Code tree is now present; later `devstrap open` just launches the editor.
```

### Agent starts a task

```text
1. User runs devstrap agent run repo --task "...".
2. Daemon resolves the remote default branch from `origin/HEAD` or stored repo metadata.
3. Daemon fetches that upstream ref.
4. Daemon resolves `origin/<default_branch>` SHA.
5. Worktree is created from that SHA.
6. New branch is created.
7. Env is injected according to policy.
8. Agent process is launched.
9. Logs, diff, and test result are captured.
10. Optional PR is created.
```

## State model

Every project has two levels of state.

### Global namespace state

Shared across devices:

- path;
- project type;
- Git remote;
- default branch;
- env profile;
- tooling profile;
- ignore profile;
- agent policy;
- draft metadata;
- tombstone/deletion status.

### Device-local state

Specific to each machine:

- materialization status;
- local path;
- current branch;
- local dirty status;
- last fetched SHA;
- env readiness;
- tool readiness;
- last error;
- watcher health.

## Sync architecture

Use an append-only event log rather than last-write-wins file sync.

Event examples:

```json
{"type":"project.added","path":"work/acme/api","remote":"git@github.com:acme/api.git"}
{"type":"project.renamed","old_path":"work/api","new_path":"work/acme/api"}
{"type":"env.profile.bound","path":"work/acme/api","profile":"acme-dev"}
{"type":"device.seen","device_id":"mac-mini-upstairs"}
```

Why event log:

- easier conflict handling;
- auditable;
- offline compatible;
- devices can replay from last cursor;
- future team policies can review changes.

## Conflict resolution model

Do not hide conflicts.

Conflict examples:

- same path maps to different Git remotes;
- same remote appears at two paths;
- two devices renamed the same project differently;
- draft project changed on two devices while offline;
- local dirty repo blocks remote path rename.

Conflict handling:

```text
Safe automatic:
  - duplicate skeleton creation: merge
  - device heartbeat conflicts: latest wins
  - missing local folder for known project: recreate skeleton

Needs user decision:
  - path/remotes mismatch
  - draft file edit conflict
  - delete vs local dirty changes
  - env profile replacement
```

## Platform adapter boundaries

Keep these behind interfaces:

```go
type Watcher interface {
    Name() string
    Watch(ctx context.Context, root string, events chan<- FSEvent) error
}

type ServiceManager interface {
    Name() string
    Install(ctx context.Context, spec ServiceSpec) error
    Uninstall(ctx context.Context, label string) error
    Status(ctx context.Context, label string) (ServiceStatus, error)
}

type Keychain interface {
    Name() string
    Store(ctx context.Context, service, account string, secret []byte) error
    Load(ctx context.Context, service, account string) ([]byte, error)
    Delete(ctx context.Context, service, account string) error
}

type EditorAdapter interface {
    Name() string
    Open(ctx context.Context, dir, editor string) error
}

// Hub is the pluggable two-plane backend boundary (HUB-*).
// Intended package: internal/hub. Implementations: file-backed (tests only),
// Cloudflare R2/S3 (production), HTTP/SSE hub service (later networked backend).
// All implementations handle signed events (envelope-encrypted payloads under a per-epoch WCK, P4-SEC-02/SEC-07) + age-encrypted blobs only.
type Hub interface {
    Name() string

    // Plane A: append-only, signed, HLC-ordered namespace event log.
    PushEvents(ctx context.Context, workspaceID string, events []SignedEvent) error
    PullEvents(ctx context.Context, workspaceID string, after Cursor, limit int) (PullResult, error)

    // Plane B: content-addressed age-encrypted blob store (age_blob:<sha256>).
    PutBlob(ctx context.Context, workspaceID, sha256 string, size int64, ciphertext io.Reader) error
    GetBlob(ctx context.Context, workspaceID, sha256 string) (io.ReadCloser, BlobMeta, error)
    HasBlob(ctx context.Context, workspaceID, sha256 string) (bool, error)
}
```

The shipped `internal/sync.Hub` interface (`HUB-01`, see `internal/sync/hub.go` and the `R2Hub` S3 adapter in `internal/hub`) is the pragmatic form of this contract: the workspace scope is configured on the hub instance (not passed per-call), `Pull` takes an `afterHLC int64` and returns `[]state.Event`, `PutBlob`/`GetBlob` take a `sha256Hex` string + `io.Reader`/`io.ReadCloser` (no `BlobMeta`), and `HasBlob` is replaced by `DeleteBlob` (idempotent, for GC/revoke, `SEC-01`/`HUB-12`) plus `ListBlobs` (mark-and-sweep enumeration, `P5-HUB-02`). The two-plane zero-knowledge contract above is unchanged.

The interface contract is idempotent for duplicate event/blob writes and fails with typed errors such as `ErrSnapshotRequired`, `ErrBlobNotFound`, and `ErrInvalidBlobKey`. Object keys are:

```text
workspaces/<workspace_id>/events/<hlc-padded>/<device_id>/<seq>/<event_id>.json
workspaces/<workspace_id>/blobs/<sha256>
workspaces/<workspace_id>/snapshots/<hlc-padded>.json.age
```

Hub backends:

- **file-backed**: the `devstrap sync --hub-file` (or `hub: file:<path>`) backend — **tests only**, never production;
- **Cloudflare R2 / S3**: the chosen **production** backend, **shipped** (`P5-HUB-01`) — the `aws-sdk-go-v2` S3 adapter behind `hub: r2://<bucket>` (S3 API, zero egress, namespaced by `workspace_id`);
- **HTTP/SSE hub service**: the later networked backend implementing the wire protocol above (still deferred).

Mac implementation:

- watcher: native FSEvents binding preferred; fsnotify/kqueue acceptable for early MVP but not equivalent to FSEvents;
- service: launchd LaunchAgent;
- secrets: Keychain + external vault CLI;
- future VFS: File Provider or macFUSE/FSKit.

Linux implementation:

- watcher: inotify through fsnotify;
- service: systemd user service;
- secrets: libsecret/keyring + external vault CLI;
- future VFS: FUSE.

## Design principle

The codebase should be written as:

```text
80% platform-neutral core
15% platform adapter code
5% packaging/install code
```

That keeps Mac-first work from painting Linux into a corner.

## Implementation status

As of `2026-06-30`, the repository contains the Go workspace:

- `cmd/devstrap` main package;
- `internal/cli` command skeleton;
- `internal/config` path defaults;
- `internal/state` SQLite store, embedded Goose migrations, HLC/event-ordering tables, and database backup/status helpers;
- `internal/platform` adapter contracts for watcher, service manager, keychain, and editor launch, with build-tagged platform detection, fsnotify-backed Darwin/Linux watchers that debounce bursts into reconciliation hints, an advisory polling watcher fallback for unsupported platforms, system keyring-backed Darwin/Linux keychain adapters with explicit fallback handling, unsupported service placeholders, `devstrap open` routed through the editor adapter, and a test guard keeping `runtime.GOOS` checks inside `internal/platform`;
- a thin generic agent runner that creates fresh worktrees, runs explicit argv commands with sanitized no-secret env, applies wrapper-level command and file path policy, records `agent_runs`, captures logs/diff summaries, and gates `agent pr` on stale-base detection;
- CI for macOS/Linux Go tests, race tests, vet, build, vuln scanning, and module hygiene;
- focused tests for the implemented CLI/config/state/platform packages;
- the cloud-sync layer (`P5`/`HUB-*`): `internal/sync` (the `Hub` interface, `FileHub`, event apply/dedup, cursor logic), `internal/hub` (the `R2Hub` two-plane backend with keying/retry/conditional-put/retention-floor logic and the **shipped** `aws-sdk-go-v2` `S3Adapter` behind `hubFromOptions` `r2://` wiring, `P5-HUB-01`), `internal/envbundle`/`internal/ignore`/`internal/childenv`/`internal/git`/`internal/devicekeys`/`internal/redact`, `devstrap sync`/`run-loop`/`hub gc`/`devices revoke`, and an env-gated MinIO conformance test.

The daemon, FSEvents-specific Mac watcher, and service installers are still design targets. Native platform-specific watcher or service-manager code must implement the `internal/platform` interfaces instead of branching through the core.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

The near-term hub-hardening imperative is that these `HUB` items land alongside the zero-knowledge namespace-map encryption gap (`P6-SEC-01`, `spec/15`) and the transport-vs-logical-clock cursor gap (`P6-SYNC-01`, `spec/07`), which bound the correctness of every hub push/pull/GC path described above.

### P6-HUB-01 — `hub gc` sweeps a stale local replica with no pre-GC sync, no grace window, and a truncated mark set

**Problem.** `hubGC` (`internal/cli/hub.go:238-278`) deletes any hub blob absent from the purely-local `store.RetainedBlobRefs` without pulling first; remote draft blobs only enter that set on `draft.snapshot.created` apply (`internal/sync/events.go:475-491`), and `EncryptedHub.Pull` silently truncates at the first ungranted epoch, so a stale or awaiting-grant device deletes other devices' live blobs.

**Actionable steps.**
1. Run a full pull+apply inside `hubGC` before computing refs, and refuse to sweep if `Pull` deferred/skipped any events or `ApplyEvents` quarantined anything (thread those signals out).
2. Extend the list interface with `LastModified` and skip blobs younger than a ~24h grace window.
3. Test: device B creates+syncs a draft; an unsynced device A `hub gc` must not delete B's blob.

```go
type ObjectInfo struct {
    Key          string
    LastModified time.Time // S3: out.Contents[i].LastModified; memS3/FileHub record on put
}
// hub gc: skip when time.Since(info.LastModified) < 24*time.Hour
```

### P6-HUB-03 — `R2Hub.Push` uploads one event per serial round-trip

**Problem.** `Push` (`internal/hub/r2.go:120-146`) loops one marshal + conditional-PUT per event with no `errgroup`, while `Pull` got bounded fan-out (`r2PullConcurrency=8`, `r2.go:182-203`) under `P5-HUB-04`; `pushReferencedBlobs` (`internal/cli/sync.go:151-166`) is likewise serial, so a first sync after a large `scan --adopt` stalls 30-60+ s on sequential PUTs.

**Actionable steps.**
1. Fan out PUTs with an `errgroup` `SetLimit(r2PushConcurrency)`, but push in HLC-ordered waves (finish all PUTs at `HLC <= h` before starting `HLC > h`) — or sequence this after `P6-SYNC-01`'s ingestion-position cursor to avoid widening the intra-device clock gap.
2. Fan out `pushReferencedBlobs` similarly.
3. Document the wave-ordering invariant in the `Push` comment.

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(r2PushConcurrency)
for _, wave := range groupByHLCWave(events) { // ascending HLC
    for _, ev := range wave { ev := ev; g.Go(func() error { return putEvent(ctx, ev) }) }
    if err := g.Wait(); err != nil { return err } // barrier per wave
}
```

### P6-HUB-04 — the retention horizon has no hub-side representation, so `ErrSnapshotRequired` can never fire in production

**Problem.** Production wiring always builds `R2Hub{RetentionHLC: 0}` (`internal/cli/hub.go:116`) and `R2Hub.Pull` gates only on that local field (`internal/hub/r2.go:152-154`); nothing reads a retention marker from the hub, so once event-log compaction lands every non-compacting device pulls a silently partial log and permanently diverges.

**Actionable steps.**
1. Define a per-workspace `workspaces/<ws>/meta/retention.json` marker signed by an approved device's Ed25519 key; `R2Hub.Pull` fetches it first (404 → floor 0, cached per process) and compares before listing.
2. Give `FileHub`/memS3 the same file-based marker and add a conformance case "pull below a written retention marker → `ErrSnapshotRequired`."
3. Fold the signed-marker requirement into the `P4-HUB-11` compaction work; verify the signature so a malicious hub can only DoS, not silently truncate.

```json
// workspaces/<ws>/meta/retention.json (Ed25519-signed by an approved device)
{"retention_hlc": 174213, "compacted_at": "2026-07-01T12:00:00Z", "device_id": "dev_..."}
```
