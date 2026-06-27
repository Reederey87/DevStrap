---
last_reviewed: 2026-06-26
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
