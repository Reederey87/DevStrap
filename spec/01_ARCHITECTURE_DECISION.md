---
last_reviewed: 2026-06-28
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

DevStrap maintains the structure, metadata, device state, Git freshness, secrets mapping, and agent worktrees. Repos may be fully hydrated or represented as skeleton directories until opened.

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

Local services:

```text
Mac:   launchd LaunchAgent + FSEvents watcher
Linux: systemd user service + inotify watcher
DB:    SQLite WAL under ~/.devstrap/state.db
IPC:   Unix domain socket: ~/.devstrap/devstrapd.sock
```

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

A CLI-only tool is useful but will not feel like Dropbox. The daemon is needed to notice new projects, reconcile state, create skeletons, and sync across machines.

### Alternative F — Reuse an existing sync substrate (Syncthing/Mutagen) or a hidden manifest Git repo for namespace + blob transport

Deferred, **gated** — not foreclosed.

Instead of hand-rolling `devstraphub` (HLC + content-hash chain + Ed25519 signatures + HTTP/SSE), the namespace event log and encrypted blobs could ride an existing local-first sync engine, or a hidden manifest Git repo (Alternative B as a transport adapter). This is the strongest argument *against* building the bespoke sync stack, and it must be re-evaluated at the M7 entry gate in `14_MVP_ROADMAP_AND_BACKLOG.md` (which already concedes "a hidden manifest git repo may substitute for a bespoke service"). Adopting F would retire much of the bespoke `internal/sync` surface.

Rejected only as the *default*, because a thin zero-knowledge hub gives end-to-end encryption (age, per-device recipients) and signed-event integrity guarantees that a generic file-sync engine does not, and because the bespoke hub is forge-agnostic by construction. The decision is contingent on a real two-machine drift signal, not assumed.

### Alternative G — devcontainer/DevPod-style committed config as the cross-device source of truth

Deferred.

A committed per-repo config could define the portable workspace, letting existing tools (DevPod, devcontainers) reconstruct environments instead of a custom namespace + hub. Rejected as the *primary* model because it is per-repo rather than a cross-repo `~/Code` namespace, and does not address local-only/draft folders, secrets hydration, or the fresh-worktree agent invariant. Worth borrowing from for the per-project env/tooling descriptor.

## Final recommendation

Build in this order:

```text
1. CLI proof: scan, adopt, hydrate, open, worktree, env.
2. Thin agent runner: branch/worktree per task, scoped env, logs, diff summary, PR gate.
3. Local daemon: watcher, reconciler, skeletons, LaunchAgent.
4. Multi-device hub: event sync, device status, encrypted blobs.
5. StrapFS: optional virtual filesystem layer.
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
