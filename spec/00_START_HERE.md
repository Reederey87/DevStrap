# DevStrap / Workspace Passport — Start Here

## Selected architecture

The optimal architecture for the first Mac implementation is a **managed physical code namespace**, not a full virtual filesystem.

In practice:

```text
~/Code is a real folder.
DevStrap owns the structure and metadata.
Git owns repository content.
A local daemon keeps the namespace consistent.
Repos are skeletons until materialized.
Secrets are referenced or encrypted, not blindly copied.
Agents always get fresh worktrees from fetched remote refs.
```

This gives the Dropbox-like experience you want without starting with the hardest possible engineering problem: implementing a reliable cross-platform filesystem.

## Product promise

```text
Install DevStrap on a new Mac, Linux box, cloud machine, or agent runner.
Point it at ~/Code.
Authenticate Git + secrets.
The same project tree appears.
Opening a project hydrates it.
Starting an agent creates a fresh, isolated worktree from origin/main.
```

## Why not FUSE or Apple File Provider first?

A true lazy virtual filesystem is attractive, but it should be a later layer called **StrapFS**, not the MVP.

The first version should avoid:

- macOS kernel-extension or system-extension installation friction;
- Finder/File Provider complexity;
- FUSE performance, caching, file locking, and editor-indexer edge cases;
- cross-platform filesystem semantics before the product loop is proven.

The MVP can still feel close to Dropbox because it creates the same directory tree everywhere and materializes projects through CLI, shell hooks, editor adapters, and agent adapters.

## Architecture phases

```text
Phase 0: Local CLI proof
  - scan ~/Code
  - register projects
  - hydrate repositories
  - create fresh worktrees
  - capture/hydrate env profiles

Phase 1: Mac daemon
  - launchd LaunchAgent
  - FSEvents watcher
  - local SQLite state
  - skeleton directories
  - shell integration
  - Cursor/VS Code open commands

Phase 2: Multi-device sync
  - DevStrap Hub event log
  - device registration
  - namespace sync
  - encrypted env/draft blobs
  - device status

Phase 3: Agent workspaces
  - one branch/worktree per task
  - fresh origin/main base
  - command/file policy
  - logs and PR workflow

Phase 4: Optional StrapFS
  - macOS: File Provider or macFUSE/FSKit evaluation
  - Linux: FUSE
  - Windows future: WinFsp
```

## Recommended implementation stack

```text
Core CLI/daemon: Go
Local database: SQLite with WAL
Mac watcher: native FSEvents adapter preferred; fsnotify/kqueue acceptable for early MVP with periodic reconciliation
Linux watcher: inotify through Go fsnotify, with periodic reconciliation
Mac service: LaunchAgent via launchd
Linux service: systemd user service
Git: shell out to system git initially
Secrets: 1Password/native Mac password adapters + encrypted personal env store
Sync: local-first event log + small DevStrap Hub
TUI: Go Bubble Tea later
Mac GUI/File Provider helper: Swift later, only if needed
Optional npm role: distribution wrapper only, not the core daemon/runtime
```

Why Go: one portable binary, good process management, solid cross-platform filesystem notification libraries, easy launchd/systemd deployment, and clean path toward Linux. Rust is also viable, but Go is faster for this product's first serious implementation.

## Current repository state

Last validated: `2026-06-24`.

Implemented in this repository:

- Go module: `github.com/Reederey87/DevStrap`.
- CLI entrypoint: `cmd/devstrap`.
- Commands: `version`, `init`, `status`, `doctor`.
- Local state package with embedded Goose SQLite migration.
- Initial SQLite schema matching `12_DATA_MODEL_SQLITE.md`.
- README, MIT license, `.gitignore`, GitHub Actions CI, and concise `AGENTS.md`.

Not implemented yet:

- scanner/adoption workflow;
- Git hydration/open/worktree commands;
- env capture/hydrate and encryption;
- daemon, local socket API, watcher, LaunchAgent/systemd installers;
- sync hub and encrypted blob exchange;
- agent runner and policy enforcement.

Local validation performed:

```bash
go test ./...
```

The project intentionally remains Go-first. Node/npm can be added later as a packaging or installer channel if useful, but the runtime should stay Go.

## What to build first

Build the boring but powerful version first:

```bash
devstrap init ~/Code
devstrap scan ~/Code --adopt
devstrap status
devstrap open work/nclh/foc-models --cursor
devstrap worktree new work/nclh/foc-models --fresh-main --name route-tests
devstrap env capture work/nclh/foc-models .env
devstrap env hydrate work/nclh/foc-models
devstrap sync
```

The first killer loop:

```text
1. Add or create a project on Machine A.
2. DevStrap notices it and records its path, remote, env profile, and policy.
3. Machine B receives the namespace update.
4. The same folder path appears as a skeleton.
5. Opening it clones/fetches/hydrates it.
6. Agent work starts from fresh origin/main, not stale local main.
```

## Document map

- `01_ARCHITECTURE_DECISION.md` — final architecture choice and rejected alternatives.
- `02_PRODUCT_REQUIREMENTS.md` — product scope, personas, invariants, success metrics.
- `03_SYSTEM_ARCHITECTURE.md` — components, data flows, daemon, hub, adapters.
- `04_CHALLENGE_MATRIX.md` — every major problem and viable solution options.
- `05_MAC_FIRST_IMPLEMENTATION.md` — launchd, FSEvents, shell/editor integration, packaging.
- `06_LINUX_COMPATIBILITY.md` — systemd, inotify, Linux paths, FUSE future.
- `07_NAMESPACE_AND_SYNC_MODEL.md` — code namespace, event log, devices, conflicts.
- `08_GIT_MATERIALIZATION_AND_WORKTREES.md` — Git, partial clone, LFS, fresh worktrees.
- `09_SECRETS_AND_ENVIRONMENT.md` — env sync, vault adapters, encrypted personal mode.
- `10_AGENT_WORKSPACES_AND_POLICIES.md` — agent isolation, command policy, logs, PR flow.
- `11_IGNORE_AND_LOCAL_GARBAGE.md` — universal ignore compiler and OS-specific junk.
- `12_DATA_MODEL_SQLITE.md` — initial SQLite schema.
- `13_CLI_DAEMON_API.md` — CLI commands, local socket API, daemon jobs.
- `14_MVP_ROADMAP_AND_BACKLOG.md` — staged implementation plan and issue backlog.
- `15_SECURITY_THREAT_MODEL.md` — assets, threats, mitigations.
- `16_TEST_PLAN.md` — unit, integration, e2e, chaos, and cross-platform tests.
- `17_REFERENCES.md` — useful platform and tool references.
