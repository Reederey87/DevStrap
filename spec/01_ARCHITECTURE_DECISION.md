---
last_reviewed: 2026-07-01
tracks_code: [cmd/**, internal/**, spec/**]
---
# Architecture Decision: Mac-First Managed Code Namespace

## Decision

Build DevStrap as a **Mac-first, Linux-compatible managed physical code namespace**.

The user-visible folder is real:

```text
~/Code/
  work/
  personal/
  experiments/
```

DevStrap maintains the structure, metadata, device state, Git freshness, secrets mapping, and agent worktrees. The target sync loop eagerly materializes the namespace on `devstrap sync`; skeleton directories are transient recovery/fallback state, not the primary UX.

## Final architecture

```text
┌────────────────────────────────────────────────────────────┐
│                   DevStrap Hub / Sync Layer                 │
│  namespace events, device state, encrypted env/draft blobs   │
└──────────────────────────┬─────────────────────────────────┘
                           │
                           ▼
┌────────────────────────────────────────────────────────────┐
│                       devstrapd                             │
│  local daemon, watcher, reconciler, materializer, policies   │
└──────────────────────────┬─────────────────────────────────┘
                           │
        ┌──────────────────┼──────────────────┐
        ▼                  ▼                  ▼
┌──────────────┐    ┌──────────────┐   ┌────────────────────┐
│ Managed tree │    │ Git/Secrets  │   │ Editors / Agents    │
│ ~/Code       │    │ providers    │   │ Cursor, VS Code, CI │
└──────────────┘    └──────────────┘   └────────────────────┘
```

Target local services (Phase 1 daemon — not yet built; only the SQLite WAL store is shipped today):

```text
Mac:   launchd LaunchAgent + FSEvents watcher   (planned)
Linux: systemd user service + inotify watcher   (planned)
DB:    SQLite WAL under ~/.devstrap/state.db     (shipped)
IPC:   Unix domain socket: ~/.devstrap/devstrapd.sock   (planned)
```

## Cloud-sync architecture (2026-06-28 extension)

> Extends the 2026-06-27 second-pass audit (see `00_START_HERE.md` and `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md`); driven by `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md` (workstreams `EAGER-*`, `DRAFT-*`, `HUB-*`, `XP-*`, `SCALE-*`). The product goal is the **Dropbox experience for code**: one identical `~/Code` tree that appears automatically on every device in a developer's fleet (e.g. multiple desktops, a laptop, a home server or NAS, and cloud/agent runners).

### Sync is split by content type — never blanket file-sync

DevStrap never blanket-syncs files, and **never file-syncs `.git`** (a torn `.git` corrupts the repo — see Alternative A). Each content class rides its own purpose-built channel:

| Content | Channel |
| --- | --- |
| Repo content | git blobless/partial clone + fetch (`git clone --filter=blob:none`) from its **existing remote**, over git's own transport. Repo content **never** traverses the DevStrap hub. |
| Env vars + non-git/draft folders | age-encrypted, content-addressed `age_blob:<sha256>` blobs. |
| Project map ("namespace map") | a signed, HLC-ordered, append-only event log. |
| `node_modules` / build artifacts | **never synced**; rebuilt on hydrate (`npm`/`pnpm`/`uv install`). |

See `07_NAMESPACE_AND_SYNC_MODEL.md` and `08_GIT_MATERIALIZATION_AND_WORKTREES.md`.

### Decision 2 — Eager clone-everything materialization (`EAGER-*`)

`devstrap sync` materializes **eagerly**: every project in the namespace map is blobless/partial-cloned up front, so after a sync the whole `~/Code` tree is present on the device. There is **no FUSE/placeholder/lazy-VFS magic** in this design — StrapFS stays explicitly deferred (see Alternatives C/D and the build order below). This trades disk for predictability and debuggability, and keeps the core portable Go on macOS and Ubuntu (`XP-*`). Eager whole-tree materialization on `devstrap sync` is **shipped** (`EAGER-*`): `sync` blobless/partial-clones every mapped repo, hydrates env/draft blobs, and rebuilds deps opt-in; `--hub-file` remains the file-backed test backend.

### Decisions 3–4 — Two-plane zero-knowledge cloud hub (`HUB-*`)

`devstraphub` is a **two-plane, zero-knowledge** service behind a single pluggable `Hub` interface:

1. **Event-log plane** — the signed, HLC-ordered namespace map.
2. **Content-addressed encrypted blob-store plane** — env + non-git/draft content as `age_blob:<sha256>` ciphertext.

The hub sees **only ciphertext plus a signed map**; it cannot read code, secrets, or drafts. Repo content is absent from the hub entirely (it rides git, above).

The chosen cloud backend is **Cloudflare R2 from the start** (S3 API, zero egress, namespaced by `workspace_id`; client-side age encryption gives confidentiality by construction). Integrity and availability still require signed hash chains, fail-closed event verification, scoped credentials, snapshots/backups, and retention discipline. There is **no NAS-first phase**. The `Hub` interface stays pluggable, and the file-backed backend (`devstrap sync --hub-file`) remains a supported local/offline escape hatch and is the backend used by the test suite; R2/S3 (`hub: r2://<bucket>`) is the production backend.

The cloud hub sync path and R2 backend selection are **shipped** (`P5-HUB-01`): `hub: r2://<bucket>` (or `s3://`) wires the `aws-sdk-go-v2` S3 adapter behind `hubFromOptions`, with `DEVSTRAP_HUB_S3_*` env/config credentials and `--hub-file` retained for tests. See `13_CLI_DAEMON_API.md` and `14_MVP_ROADMAP_AND_BACKLOG.md`.

## What DevStrap owns

DevStrap owns:

- the global `~/Code` namespace;
- project path mapping;
- repo metadata;
- device inventory;
- materialization state;
- env profile mappings;
- encrypted personal secret bundles when enabled;
- agent worktree lifecycle;
- local policy enforcement;
- ignore rule compilation;
- editor/agent open/run workflows.

DevStrap does **not** own:

- Git history for normal repositories;
- plaintext secrets;
- native dependency directories like `node_modules` or `.venv`;
- large binary artifacts unless stored as encrypted draft blobs temporarily;
- production-grade package management.

## Why this is optimal

### 1. It solves the actual pain immediately

The user pain is not only remote file sync. It is:

- inconsistent project paths;
- stale local default branches;
- missing env variables;
- scattered worktrees;
- agent work starting from stale local default branches;
- different machines having different readiness states.

A managed namespace + daemon solves those without waiting for a kernel/filesystem-level implementation.

### 2. It keeps Linux compatibility natural

The same model works on Linux:

```text
Mac watcher:   FSEvents
Linux watcher: inotify
Mac service:   launchd
Linux service: systemd user service
Future VFS:    macFUSE/File Provider vs Linux FUSE
```

The daemon, state model, Git logic, env logic, sync protocol, and agent worktree logic remain platform-neutral.

### 3. It avoids the hardest failure class early

A virtual filesystem creates difficult problems:

- IDE indexers reading thousands of files and accidentally hydrating everything;
- file locking and atomic rename semantics;
- Git operating inside a mounted virtual filesystem;
- case sensitivity differences;
- code signing/system-extension friction on macOS;
- difficult bug reports when editor, OS, and filesystem behavior interact.

The MVP uses real folders and real Git worktrees, which are debuggable.

## Alternatives considered

### Alternative A — Raw Dropbox-style filesystem sync

Rejected.

Problems:

- `.git` internals can be corrupted or conflicted by generic sync;
- OS-specific native dependencies differ;
- `.env` files expose secrets;
- generated folders explode sync volume;
- stale local default branches and bad agent worktrees remain unsolved.

The legitimate need behind this idea — "I forgot to push and I'm now on another machine" — is real and is the core product promise, but it is served **git-natively, never by live-syncing `.git`**: a signed read-only git-state **validation plane** (so every machine knows where every other machine's working tree stands), plus auto-**WIP pushes** to a reserved `refs/devstrap/wip/*` ref namespace over git's own integrity-checked transport, plus encrypted **draft bundles** for non-git folders. The git project's own guidance is that no part of a repository may be live-synced by a file-sync engine (torn `.git/index`, conflict-copied refs, `gc`-pruned objects = data loss). Continuous working-tree file-sync (Mutagen/Syncthing-style) is therefore **explicitly rejected**; see `07_NAMESPACE_AND_SYNC_MODEL.md` (working-state plane) and `04_CHALLENGE_MATRIX.md`.

### Alternative B — Another manifest Git repo

Rejected as the product interface.

A hidden manifest repository can be an implementation adapter, but the user-facing product should not require users to manage another repo, submodules, or Git gymnastics.

### Alternative C — FUSE/macFUSE from day one

Rejected for MVP, reserved for V2.

FUSE is the right long-term tool for true lazy-on-open behavior, especially on Linux. On macOS, macFUSE's newer FSKit backend may reduce kernel-extension friction, but it is still too much complexity for the first build.

### Alternative D — Apple File Provider first

Rejected for MVP, reserved for Mac-native V2 exploration.

Apple File Provider is designed for local/remote file-provider sync and Finder-integrated on-demand files. That makes it relevant, but it adds app-extension complexity and may not map cleanly to Git-aware developer workflows with worktrees, agents, and env hydration.

### Alternative E — Pure CLI, no daemon

Rejected for product feel, acceptable for Phase 0.

A CLI-only tool is useful but will not feel like Dropbox forever. It is acceptable for the current portable cycle when paired with the shipped `devstrap run-loop` to run sync -> materialize on an interval (the advertised scan stage is not yet wired; tracked as `P6-XP-03`) without native launchd/systemd installers. A native daemon remains the later product-feel layer for noticing new projects, reconciling state, and integrating with OS watchers.

### Alternative F — Reuse an existing sync substrate (Syncthing/Mutagen) or a hidden manifest Git repo for namespace + blob transport

Deferred and superseded for the current cloud-sync cycle.

Instead of hand-rolling the logical Hub (HLC + content-hash chain + Ed25519 signatures + R2/S3 or later HTTP/SSE transport), the namespace event log and encrypted blobs could ride an existing local-first sync engine, or a hidden manifest Git repo (Alternative B as a transport adapter). This remains a useful historical comparison, but it is **not** the M7 target after the 2026-06-28 rebaseline: M7 builds the Hub interface and direct R2/S3 backend first, with file-backed tests and HTTP/SSE deferred.

Rejected as the current implementation path because the direct R2/S3 Hub keeps end-to-end encryption (age, per-device recipients), signed-event integrity, provider-independent object-store semantics, and forge-agnostic repo transport without returning Git merge conflicts to the namespace map. Reopen only if R2/S3 conformance fails or a later product decision intentionally trades those properties for a different sync substrate.

> **Zero-infrastructure carrier (`AD-1`) — COMPLETE 2026-07-05.** Because the hub only ever holds ciphertext plus signed events, a dumb carrier is a safe zero-knowledge boundary. Two carriers now remove the "provision an R2 bucket first" adoption friction using infrastructure the user already has: the **private-git-repo backend** (`hub: git+ssh://…`, the documented quickstart default) and the **local-folder / cloud-drive-folder backend** (`hub: folder:<abs-path>` — a Dropbox/iCloud/Drive folder or network mount as the carrier). The merge-conflict objection above does not apply to either: every object key is content-addressed or `(device,seq)`-unique, so no `git merge` ever runs — the git carrier's lost push race refetches and re-applies, and the folder carrier lets the drive replicate the object tree with same-machine writes serialized by a local cross-process lock (cross-device CAS is best-effort by nature; see `15_SECURITY_THREAT_MODEL.md`). `r2://` remains the scale/power option. See `03_SYSTEM_ARCHITECTURE.md` (Hub backends) and `14_MVP_ROADMAP_AND_BACKLOG.md`.

### Alternative G — devcontainer/DevPod-style committed config as the cross-device source of truth

Deferred.

A committed per-repo config could define the portable workspace, letting existing tools (DevPod, devcontainers) reconstruct environments instead of a custom namespace + hub. Rejected as the *primary* model because it is per-repo rather than a cross-repo `~/Code` namespace, and does not address local-only/draft folders, secrets hydration, or the fresh-worktree agent invariant. Worth borrowing from for the per-project env/tooling descriptor.

## Final recommendation

Build in this order:

```text
1. CLI proof: scan, adopt, hydrate, open, worktree, env.
2. Thin agent runner: branch/worktree per task, scoped env, logs, diff summary, PR gate.
3. Shared materialization engine + eager sync: cursor pull, blobless clone/fetch, env/draft hydrate, bounded resumable workers.
4. Logical Hub interface + R2/S3 backend: immutable event objects, encrypted blob store, scoped credentials, fail-closed enrollment, snapshots.
5. Portable run-loop: foreground sync -> materialize loop (the advertised scan stage is not yet wired; tracked as `P6-XP-03`) for macOS/Linux before native services.
6. Local daemon: watcher, reconciler, LaunchAgent/systemd installers, socket API.
7. StrapFS: optional virtual filesystem layer.
```

## Non-negotiable architecture rules

1. **Never branch agents from a local default branch.** Always resolve the remote default branch, fetch it, and branch from `origin/<default_branch>` or an explicitly configured upstream ref.
2. **Never sync plaintext secrets by default.** Use secret references or encrypted bundles.
3. **Never sync dependency folders.** Recreate `.venv`, `node_modules`, build output locally.
4. **Never silently overwrite dirty worktrees.** Detect, warn, branch, stash, or skip.
5. **Always maintain one canonical project path.** The namespace path is the product.
6. **Keep platform-specific code behind adapters.** Mac and Linux should share the core.
7. **Keep the human working-state plane separate from the agent plane.** Cross-machine "forgot to push" recovery lives under the reserved `refs/devstrap/wip/*` ref namespace and encrypted draft bundles; the fresh-worktree base resolver must never read them. Validation (read-only git-state) and recovery (WIP refs) are human-convenience; agents always base from `origin/<default_branch>`.
8. **Treat the hub as semi-trusted.** It transports opaque, signed, end-to-end-encrypted payloads; it never sees plaintext code/secrets and is never trusted to order or authenticate events.
9. **Repo content rides git and never traverses the hub.** Repositories materialize by blobless/partial clone + fetch from their own remote, over git's own transport. The two-plane hub carries only the signed namespace map and `age_blob:<sha256>` ciphertext (env + non-git drafts); `.git` is never file-synced. `node_modules`/build artifacts are never synced — they are rebuilt on hydrate.
