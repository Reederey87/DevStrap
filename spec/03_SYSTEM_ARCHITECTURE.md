---
last_reviewed: 2026-07-12
tracks_code: [cmd/**, internal/**, internal/config/**, .github/**, .goreleaser.yaml, scripts/**]
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

Every `devstrap` CLI command works correctly without the daemon. State is materialized on demand. Reconciliation of local filesystem changes today is the **explicit `devstrap scan`**; there is no watcher-driven reconciler yet (`ARCH2-04`). The shipped `devstrap run-loop` already provides daemonless periodic convergence (a jittered sync + eager-materialize tick, default every 5 minutes), but it does not scan the local tree for new projects — that still requires `devstrap scan` (its advertised scan stage is tracked as `P6-XP-03` and not yet run by the loop). Watcher-driven and periodic local-scan reconciliation arrive with the daemon. No command depends on `devstrapd` being installed or running. The daemon is purely a performance/UX optimization — its filesystem watcher is a hint, not the source of truth — and is never a correctness dependency. If the daemon is absent, stopped, or behind, results stay correct; only freshness and latency degrade until the next on-demand materialization or the next explicit `devstrap scan`. (The daemon socket/IPC/job API in `13_CLI_DAEMON_API.md` and the reserved `exitDaemonUnavailable=3` are design intent for the M5 daemon, not shipped behavior.)

## Data flows

### New project created locally

(design intent — the watcher/daemon steps 2 and 5 are unbuilt; today this flow is `devstrap add`/`scan --adopt` followed by `devstrap sync`.)

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
2. The engine (today: the CLI, per the ARCH2-01 seam note) resolves the remote default branch from `origin/HEAD` or stored repo metadata.
3. The engine (today: the CLI) fetches that upstream ref.
4. The engine (today: the CLI) resolves `origin/<default_branch>` SHA.
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
{"type":"device.seen","device_id":"dev_01jz…"}
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
// Backends store opaque signed events and age-encrypted blobs only; envelope encryption of event
// payloads (per-epoch WCK, P4-SEC-02/SEC-07) is applied by the EncryptedHub decorator in internal/sync,
// which hubFromOptions wraps around every backend.
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
workspaces/<workspace_id>/snapshots/<hlc-padded>.json.age   # planned — full-state snapshot exchange (deferred with the HTTP/SSE backend); not yet implemented in internal/hub
```

Hub backends:

- **file-backed**: the `devstrap sync --hub-file` (or `hub: file:<path>`) backend — **tests only**, never production;
- **Cloudflare R2 / S3**: the chosen **production** backend, **shipped** (`P5-HUB-01`) — the `aws-sdk-go-v2` S3 adapter behind `hub: r2://<bucket>` (S3 API, zero egress, namespaced by `workspace_id`);
- **private git repo (zero-infrastructure carrier)**: **shipped** (`AD-1` first slice, 2026-07-04) — `hub: git+ssh://…` (below);
- **HTTP/SSE hub service**: the later networked backend implementing the wire protocol above (still deferred).

**Zero-infrastructure Hub backend (`AD-1`) — first slice SHIPPED 2026-07-04: the private-git-repo carrier.** Requiring a provisioned R2/S3 bucket undercuts the "new machine in a few minutes" promise and is the top first-run adoption friction. Because the hub only ever holds ciphertext plus signed events, even a "dumb" carrier is a safe zero-knowledge boundary. `hub: git+ssh://git@host/you/devstrap-hub.git` (also `git+https://`, `git+file://` for tests, scp-like `git@host:path.git`, optional `?branch=`, default `main`) points sync at any private git repository the user can already push to — no bucket, no new credential plane (the user's existing ssh agent / git credential helpers; git runs non-interactively, so the key must be agent-loaded). The backend (`internal/hub/gitcarrier.go`, `GitCarrierHub`) composes the proven `R2Hub` keying/semantics over a plain-filesystem `S3Client` rooted in a local clone (`~/.devstrap/hub-git/<hash>/repo`), adding only the git transport: reads fetch + hard-reset to the remote head; writes apply idempotent file mutations (every key is content-addressed or `(device,seq)`-unique, so concurrent devices never touch the same path and **no `git merge` ever runs** — the merge-conflict concern that rejected git as the primary hub in `spec/01` Alternatives does not apply), commit once per batch, and push. **Head continuity (`P7-HUB-02`, shipped 2026-07-11):** each accepted fetch persists the last verified head plus its retention-manifest fingerprint in `~/.devstrap/hub-git/<hash>/head.json`; a descendant advances normally; a non-descendant is accepted only when its retention manifest is byte-identical to the last verified fingerprint or strictly advanced (the two shapes a `hub compact` squash presents — the parentless squash reuses the pre-squash manifest bytes) and, when the prior head is locally known, only if no event object at or above the new floors was deleted (the content gate); a rewind or deleted branch is refused instead of being silently re-founded. The atomic push-ref compare-and-swap is the linearization point replacing S3 conditional PUT; a non-fast-forward rejection refetches and re-applies with capped backoff, and the CAS outcomes (`ErrRetentionConflict`, `ErrSweepLockHeld`) re-evaluate against the race winner's state. Object freshness (gc grace windows, sweep-lock TTL) rides RFC3339Nano timestamp sidecars under `.devstrap-meta/times/` — outside every listing prefix — because commit times neither survive history rewrites nor register dedup re-puts. `hub compact` is the history-bounding operation: after deleting cold events it rewrites the branch to a single parentless commit and pushes `--force-with-lease` (the caller holds the advisory sweep lock), so the host GCs the now-unreachable history; concurrent pushers recover through their own fetch-and-reapply loop. A `devstrap-hub.json` marker refuses non-hub repositories and foreign workspace ids. Forge single-file limits (e.g. 100 MB) bound blob size; chunking is out of scope for this slice.

**Zero-infrastructure Hub backend (`AD-1`) — final slice SHIPPED 2026-07-05: the local-folder / cloud-drive-folder carrier.** `hub: folder:<abs-path>` points sync at a plain shared directory — a Dropbox/iCloud/Google Drive folder, an SMB/NFS mount, anything the OS presents as a filesystem path — carrying the same zero-knowledge object set as R2 and the git carrier, with no bucket, no git remote, and no credential plane beyond the drive the user already syncs. The backend (`internal/hub/folder.go`, `FolderHub`) composes the proven `R2Hub` keying/semantics over the same plain-filesystem `S3Client` (`fsObjectStore`) the git carrier uses, but rooted DIRECTLY in the shared folder — there is **no fetch/commit/push loop**, because the cloud drive (or network mount) is the replication transport. Each `Hub` method is simply: acquire the cross-process lock → delegate to `R2Hub` → release. The git and folder carriers share one extracted cross-process lock helper (`fsLock`: in-process mutex + O_EXCL lock file + immutable JSON owner identity + mtime heartbeat). Staleness is owner-aware: a live same-host PID is never broken regardless of mtime; a provably dead same-host PID — including a live PID whose `platform.ProcessStartTime` identity no longer matches the recorded owner (PID recycled after a crash, P7-GIT-03 semantics) — is broken immediately; legacy/corrupt records and cross-host records retain the stale-TTL fallback. Break double-reads the owner bytes before removal, and nonce-verified release does not remove an already-present successor generation. The portable read/remove sequence retains a final path-replacement race after verification. **Lock/observation placement:** the lock file and the per-clone observation floor (`observed.json`) live in the LOCAL home cache (`~/.devstrap/hub-folder/<hash>/`, keyed by the resolved-once folder path), NEVER inside the shared folder — replicating lock churn through a cloud drive would cause false contention and "conflicted copy" duplicates, and the observation floor is inherently per-device local state; only the ciphertext object payloads and their RFC3339Nano timestamp sidecars (`.devstrap-meta/times/`, the same freshness mechanism the git carrier uses) live in the shared folder, where they must replicate to converge. The root is `EvalSymlinks`-resolved at construction (cloud-drive roots are frequently symlinks), `MkdirAll(0700)`d when missing, and the constructor refuses a relative path, an empty workspace id, and an existing non-directory; every subsequent operation then **re-resolves and revalidates the root under the lock**, refusing to proceed when it no longer denotes the construction-time directory — a shared root later swapped for a symlink cannot redirect reads or writes outside the registered folder (see `15_SECURITY_THREAT_MODEL.md`). **Cross-device CAS is best-effort by nature:** a cloud drive gives no cross-writer linearization point (unlike the git carrier's atomic push-ref CAS or R2's conditional PUT), so the cross-process lock only serializes SAME-machine processes; two devices writing a conditional object (retention manifest, sweep-lock acquisition) simultaneously through the same drive can each "win", which the drive resolves as a conflicted copy. This is the documented residual (`15_SECURITY_THREAT_MODEL.md`), in the same advisory-cooperation class as the sweep lock's byzantine residuals — acceptable because the folder carrier targets the single-user, few-devices, rarely-simultaneous case, and every object is content-addressed or `(device,seq)`-unique so ordinary convergence never collides. `hub init` stays git-only; the folder scheme is set in `config.yaml`/`DEVSTRAP_HUB` directly. **`AD-1` is now complete.**

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

## Distribution (AD-8 / P4-PROD-05)

Releases are cut by GoReleaser on `v*` tags (`.goreleaser.yaml`, gated by `release.yml`'s
verify job; see `RELEASING.md`). The workflow pins `GORELEASER_CURRENT_TAG` to the
triggering tag: the documented rc → stable flow puts two tags on one commit, and without
the pin GoReleaser resolves the current tag by version sort — which ranks `v0.1.0-rc.1`
above `v0.1.0`, making the stable run rebuild rc artifacts (observed live on the first
`v0.1.0` attempt). Rc tags retain the single-phase prerelease flow. A stable tag is built
exactly once into a draft release with tap upload disabled; SLSA provenance is attached to
that draft, and native Linux/macOS jobs verify checksums, SBOM coverage, the cosign bundle,
provenance, completions, and the staged executable's version metadata. Only after both
native jobs pass does the workflow publish the same draft bytes and commit GoReleaser's
staged cask to the tap. A failed smoke leaves the draft and tag for diagnosis and manual
delete-and-re-cut; it never rebuilds between smoke and publish. The distribution surface,
in the order users should reach for it:

1. **Homebrew tap** — `brew install Reederey87/devstrap/devstrap`. GoReleaser renders a
   **cask** (not a formula: `brews:` is deprecated since GoReleaser v2.16, and casks now
   cover Linux) but skips its own upload for stable staging; after native smoke passes,
   `stable-publish` commits that exact rendered cask into `Reederey87/homebrew-devstrap`.
   Prereleases still skip the tap via `auto`. The binary is not Apple-notarized yet
   (tracked under `P4-SEC-05`), so the cask strips the quarantine bit in a documented
   post-install hook. Shell completions install with the cask.
2. **Supply-chain verification (P4-SEC-05 / P4-QUAL-05)** — the `goreleaser` job signs
   `checksums.txt` with cosign in keyless mode (job `id-token: write` mints a GitHub OIDC
   token; cosign exchanges it for a short-lived Fulcio cert and logs the signature to the
   public Rekor transparency log, so no signing key is stored) and generates an SPDX SBOM
   per archive via syft. The signature transitively covers every artifact listed in
   `checksums.txt`. README documents the `cosign verify-blob` + `sha256sum -c` verification
   flow, and the `provenance` job attaches a SLSA v1 attestation to the release (including
   while a stable release is still a draft; shipped PR #117). A dormant
   `notarize:` block (Developer ID + notarization, the P4-SEC-05 remainder) keeps its existing
   `isEnvSet "MACOS_SIGN_P12"` activation, while the release workflow enforces that exactly
   zero or all five `MACOS_*` secrets are set before GoReleaser runs. Because the publisher
   runs on Ubuntu and cannot execute `spctl`, Gatekeeper assessment of the published darwin
   binary on a Mac is a required manual post-release smoke step — see `RELEASING.md`
   "Enabling notarization".
3. **`curl | sh` installer (P7-QUAL-02)** — `scripts/install.sh`, served raw from `main`
   (with a tag-pinned script URL documented for high-assurance installs). POSIX sh; picks
   os/arch, resolves the latest tag (or `DEVSTRAP_VERSION`), downloads the cosign bundle and
   verifies `checksums.txt` against the exact release-workflow identity before trusting any hash,
   verifies the archive's SLSA provenance (fail-closed like cosign; `DEVSTRAP_INSTALL_NO_SLSA=1` is the explicit provenance-only waiver), and then
   performs the always-on sha256 check before extraction. It fails closed when cosign, `slsa-verifier`, or
   the bundle is unavailable; `DEVSTRAP_INSTALL_CHECKSUM_ONLY=1` is the explicit, loud-warning
   escape hatch for a legacy bundle-less release or a checksum-only install, and
   `DEVSTRAP_INSTALL_NO_SLSA=1` waives only the provenance layer. It installs into
   `/usr/local/bin` or `~/.local/bin` (`DEVSTRAP_INSTALL_DIR` overrides) and never invokes sudo.
4. **Release tarballs** — binary + LICENSE + README + pre-generated bash/zsh/fish
   completions (a `before` hook runs `devstrap completion <shell>`; generation is stateless).
5. **Build from source / `go install …@main`** for the bleeding edge.

`.goreleaser.yaml` and `scripts/**` are tracked by this spec and work-log-gated in
`internal/specdrift` (`TestReleaseTierFilesRequireWorkLog`) — a lone packaging change is
still a behavior change.

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
- the cloud-sync layer (`P5`/`HUB-*`): `internal/sync` (the `Hub` interface, `FileHub`, the `EncryptedHub` envelope-encryption decorator, event apply/dedup, cursor logic), `internal/workspacekeys` (the per-epoch Workspace Content Key keyring, `P4-SEC-02`/`SEC-07`), `internal/hub` (the `R2Hub` two-plane backend with keying/retry/conditional-put/retention-floor logic and the **shipped** `aws-sdk-go-v2` `S3Adapter` behind `hubFromOptions` `r2://` wiring, `P5-HUB-01`), `internal/envbundle`/`internal/draftbundle`/`internal/ignore`/`internal/childenv`/`internal/git`/`internal/devicekeys`/`internal/redact`, `devstrap sync`/`run-loop`/`hub gc`/`devices revoke`, and an env-gated MinIO conformance test.

The daemon and FSEvents-specific Mac watcher are still design targets; the service installers are SHIPPED (`devstrap service install|uninstall|status`, `P4-PROD-04` — a launchd LaunchAgent / systemd `--user` unit wrapping `run-loop`). Native platform-specific watcher or service-manager code must implement the `internal/platform` interfaces instead of branching through the core, as the shipped `LaunchdManager`/`SystemdUserManager` do.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

The hub-hardening imperative that these `HUB` items land alongside the zero-knowledge namespace-map encryption gap (`P6-SEC-01`, `spec/15`) and the transport-vs-logical-clock cursor gap (`P6-SYNC-01`/`P5-SYNC-01`, `spec/07`) is now satisfied — all are shipped.

**DIRECTION — multi-device hardening freeze before new planes (`AD-2`): COMPLETE 2026-07-03.** All four confirmed criticals are shipped (`P6-SEC-01` confidentiality break, `P6-SYNC-01` whole-batch wedge, `P6-HUB-01` live-data-loss GC, and `P5-SYNC-01` cursor drops — per-origin-device Seq transport cursors, PR #59). New capability planes (the HTTP/SSE relay, the daemon, StrapFS, hosting/SaaS docs) are unblocked from the freeze's perspective; see `spec/14` for the roadmap ordering and the next core-engine candidate (compaction + snapshot exchange, `P4-SYNC-02`/`P4-HUB-11`).

### P6-HUB-01 — `hub gc` sweeps a stale local replica with no pre-GC sync, no grace window, and a truncated mark set — **shipped (2026-07-02)**

**Was.** `hubGC` deleted any hub blob absent from the purely-local `store.RetainedBlobRefs` without pulling first; remote draft blobs only enter that set on `draft.snapshot.created` apply, and `EncryptedHub.Pull` silently truncated at the first ungranted epoch, so a stale or awaiting-grant device deleted other devices' live blobs.

**Shipped fix.** Three gates in `hubGC` (`internal/cli/hub.go`): (1) a pre-GC pull+apply (the `pullAndApplyEvents` helper shared with `runSyncCycle`) so every device's latest events enter the mark set; (2) refuse-to-sweep when the view is incomplete — `EncryptedHub.PullStats.Truncated`/`Skipped` counters, `ApplyEventsWithStats` quarantine/cursor-held signals, or any open quarantine-class conflict (`dssync.QuarantineConflictTypes`) abort with a non-zero exit and a remedy hint; (3) an age grace window — `Hub.ListBlobs` now returns `BlobInfo{Key, LastModified}` (S3: `out.Contents[i].LastModified`; FileHub: blob mtime; a zero time is treated as young/kept) and unreferenced blobs younger than `--grace-window` (default 24h) survive, **bounding** the blob-pushed-before-event race to the window (a device offline longer than the window is not protected; it re-pushes on its next successful sync because its push cursor never advanced). The pre-GC pull also caches referenced blobs exactly as `sync` does — the cursor advances past those events, so gc is the only chance to fetch them. A late-applying skew-quarantined event now auto-resolves its `untrustworthy_remote_time` conflict so one transient clock hiccup cannot block gc forever. The dedup-`PutBlob` residual is **closed end-to-end** (`P4-HUB-12`, shipped): both backends refresh a blob's `LastModified` on a dedup hit (R2 with one unconditional same-bytes re-put, FileHub with an mtime bump — content addressing makes the re-write byte-safe), AND the sweep re-stats (`Hub.StatBlob`, HEAD on R2 / `os.Stat` on FileHub) each candidate immediately before deleting it, so a refresh that lands AFTER the pre-sweep `ListBlobs` snapshot is still honored and a blob re-referenced by a `>grace-window`-late recovery sync survives (the refresh alone was insufficient against gc's stale list, and the sweep lock does not help — it serializes sweepers, not the syncing device racing them). One remaining residual: an undecryptable event parked at the log tail keeps `Skipped` non-zero until any newer event advances the cursor (`P6-SEC-03`'s class). The "run gc from one designated device" caveat is **retired**: the destructive hub passes (`gc`/`compact`/`migrate-events`) are now serialized by an advisory sweep lock (`meta/sweep.lock`, create-only conditional PUT, 1h TTL judged by the object's backend mtime, one stale-break-and-retry; `P4-HUB-12`). The lock is advisory — it protects cooperating clients only, not a hostile writer (`spec/15`). The signed retention manifest is shipped (`P6-HUB-04`).

### P6-HUB-02 — hub S3 credential custody (shipped 2026-07-03, PR #45)

**Was.** `selectBackendHub` accepted the S3 secret access key only from a plaintext env var or config value and passed it to a static credentials provider; the keychain / `op://` credential resolution promised in `spec/19` did not exist, and `spec/13`/`spec/15`/`spec/19` contradicted each other on hub credential custody.

**Shipped fix.** Hub S3/R2 credentials now resolve most-explicit-first through `resolveHubS3Credentials` (`internal/cli/hub.go`): `DEVSTRAP_HUB_S3_*` env/config — where either value may be a 1Password `op://` ref resolved via `op read` under the sanitized child env — then `AWS_*` literals, then the per-workspace OS-keychain slot written by the new `devstrap hub login` (0600 file fallback; removed by `hub logout`). The resolved secret rides `redact.Secret` and is revealed only at the `hub.NewS3Client` constructor. `mapS3Error` gained an `ErrS3Auth` branch so rejected/expired credentials surface a typed remediation hint instead of a raw `SignatureDoesNotMatch`. `spec/13`/`spec/15`/`spec/19` are reconciled — `spec/15` owns the custody threat model, `spec/19` the provisioning steps.

### P6-HUB-03 — `R2Hub.Push` fans out with bounded concurrency (shipped 2026-07-04, `fix/p6-hub-03`)

**Was.** `R2Hub.Push` serialized one marshal + conditional PUT per event while `Pull` already used bounded fan-out, and `pushReferencedBlobs` serialized each content-addressed blob PUT. A first sync after a large `scan --adopt` or draft snapshot wave could therefore spend one full network round-trip per event/blob even though the backend and in-memory conformance double are safe for concurrent use.

**Shipped fix.** `R2Hub.Push` now validates the whole batch's positive Seq invariant before any network work, then uses `errgroup.WithContext` with `r2PushConcurrency=8` to write events concurrently (`internal/hub/r2.go:64-67`, `internal/hub/r2.go:213-258`). Each goroutine preserves the old body semantics: marshal, build the seq-keyed object key, retry the conditional `PutObject`, treat `ErrPreconditionFailed` as an idempotent duplicate, and return `put event <id>: ...` for other failures. Plain fan-out is correct: `runSyncCycle` pushes blobs, calls `hub.Push`, and only advances the per-device push watermark after the whole batch returns nil (`internal/cli/sync.go:412-429`), so any mid-batch failure leaves the watermark unchanged and the next cycle re-pushes the same batch; successful duplicates collapse through the conditional PUT. No HLC/Seq wave-ordering machinery is needed because `P5-SYNC-01`'s per-origin-device Seq transport cursor superseded the old HLC-gap concern. Blob pushes got the same unordered bounded fan-out with `blobPushConcurrency=8` (`internal/cli/sync.go:24-27`, `internal/cli/sync.go:454-474`) because blobs are content-addressed and have no event-log ordering invariant. Regression coverage proves a concurrent mid-batch event PUT failure surfaces while other event objects land, 50 concurrent event PUTs land with the expected bytes, multiple blob refs push correctly, and one blob failure preserves the existing error prefix (`internal/hub/r2_test.go:57-119`, `internal/cli/sync_test.go:44-102`).

### P6-HUB-04 — the retention horizon has no hub-side representation, so `ErrSnapshotRequired` can never fire in production

**Problem.** Production wiring always builds `R2Hub{RetentionHLC: 0}` (`internal/cli/hub.go:116`) and `R2Hub.Pull` gates only on that local field (`internal/hub/r2.go:152-154`); nothing reads a retention marker from the hub, so once event-log compaction lands every non-compacting device pulls a silently partial log and permanently diverges.

**Actionable steps.**
1. Define a per-workspace `workspaces/<ws>/meta/retention.json` marker signed by an approved device's Ed25519 key; `R2Hub.Pull` re-fetches and re-validates it on **every** pull (or with a short TTL) — 404 → floor 0 — and **rejects any retention-HLC regression** so a replayed older signed marker can't silently downgrade the floor. Do not cache the floor indefinitely per process.
2. Give `FileHub`/memS3 the same file-based marker and add a conformance case "pull below a written retention marker → `ErrSnapshotRequired`."
3. Fold the signed-marker requirement into the `P4-HUB-11` compaction work; verify the signature so a malicious hub can only DoS, not silently truncate.

```json
// workspaces/<ws>/meta/retention.json (Ed25519-signed by an approved device)
{"retention_hlc": 174213, "compacted_at": "2026-07-01T12:00:00Z", "device_id": "dev_..."}
```
