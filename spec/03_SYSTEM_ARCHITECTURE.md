---
last_reviewed: 2026-06-26
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

### `devstraphub`

Small sync service.

Responsibilities:

- store append-only namespace events;
- store encrypted blobs for env bundles and draft projects;
- store device heartbeats;
- never store plaintext secrets;
- support offline-first sync.

MVP hub can be self-hosted on a Mac Mini, Linux box, or small VPS. Later it can become a hosted SaaS.

### No-daemon mode (correctness guarantee)

Every `devstrap` CLI command works correctly without the daemon. State is materialized on demand and the managed tree is reconciled by periodic scans, so no command depends on `devstrapd` being installed or running. The daemon is purely a performance/UX optimization — its filesystem watcher is a hint, not the source of truth — and is never a correctness dependency. If the daemon is absent, stopped, or behind, results stay correct; only freshness and latency degrade until the next on-demand materialization or periodic-scan reconciliation.

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

### Project opened on another machine

```text
1. User runs devstrap open experiments/fs2.
2. Daemon checks namespace entry.
3. If Git repo: clone/fetch/materialize.
4. If draft: download/decrypt draft blob.
5. Env profile is checked/hydrated.
6. Tooling profile is checked.
7. Editor opens.
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
```

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

As of `2026-06-25`, the repository contains the Phase 0 Go workspace:

- `cmd/devstrap` main package;
- `internal/cli` command skeleton;
- `internal/config` path defaults;
- `internal/state` SQLite store, embedded Goose migrations, HLC/event-ordering tables, and database backup/status helpers;
- `internal/platform` adapter contracts for watcher, service manager, keychain, and editor launch, with build-tagged platform detection, fsnotify-backed Darwin/Linux watchers that debounce bursts into reconciliation hints, an advisory polling watcher fallback for unsupported platforms, system keyring-backed Darwin/Linux keychain adapters with explicit fallback handling, unsupported service placeholders, `devstrap open` routed through the editor adapter, and a test guard keeping `runtime.GOOS` checks inside `internal/platform`;
- a thin generic agent runner that creates fresh worktrees, runs explicit argv commands with sanitized no-secret env, applies wrapper-level command and file path policy, records `agent_runs`, captures logs/diff summaries, and gates `agent pr` on stale-base detection;
- CI for macOS/Linux Go tests, race tests, vet, build, vuln scanning, and module hygiene;
- focused tests for the implemented CLI/config/state/platform packages.

The daemon, FSEvents-specific Mac watcher, and service installers are still design targets. Native platform-specific watcher or service-manager code must implement the `internal/platform` interfaces instead of branching through the core.
