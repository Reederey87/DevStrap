# MVP Roadmap and Backlog

## MVP definition

The MVP is successful when one user can register multiple Macs/Linux machines, keep a consistent `~/Code` structure, materialize repos on demand, hydrate env safely, and create fresh worktrees for agents.

## Recommended build order

```text
Milestone 0: repo skeleton and CLI
Milestone 1: local state and scan/adopt
Milestone 2: Git hydration and open
Milestone 3: fresh worktree manager
Milestone 4: env capture/hydrate
Milestone 5: Mac daemon and watcher
Milestone 6: Linux compatibility
Milestone 7: multi-device hub
Milestone 8: agent runner MVP
```

## Milestone 0 — Project skeleton

Deliverables:

- Go module; done
- CLI skeleton; done
- config loading; initial Viper wiring done
- logging; not started
- initial docs; done
- CI for macOS/Linux; done

Tasks:

```text
[x] Create local repo workspace scaffold
[x] Add Cobra CLI
[x] Add Goose SQLite migration framework
[ ] Add structured logger
[x] Add config path resolver
[x] Add Go test harness and CI workflow
```

Acceptance:

```bash
devstrap version
devstrap help
```

Current validation:

```bash
go test ./...
```

Remaining Milestone 0 work:

- initialize/publish the GitHub repository;
- add structured logging before daemon work;
- add focused unit tests for config/state packages.

## Milestone 1 — Local workspace state

Deliverables:

- `devstrap init ~/Code`;
- SQLite schema;
- workspace/device record;
- namespace entries;
- scan existing repos.

Tasks:

```text
[ ] Implement init command
[ ] Implement DB migrations
[ ] Implement device ID generation
[ ] Implement path normalization
[ ] Implement Git repo scanner
[ ] Implement duplicate remote detection
[ ] Implement status command
```

Acceptance:

```bash
devstrap init ~/Code
devstrap scan ~/Code --adopt
devstrap status
```

## Milestone 2 — Git hydration and open

Deliverables:

- skeleton directories;
- repo add;
- hydrate Git repo;
- open in Cursor/VS Code.

Tasks:

```text
[ ] Create skeleton folder writer
[ ] Implement devstrap add
[ ] Implement git clone partial/full
[ ] Implement git fetch/status
[ ] Implement hydrate command
[ ] Implement editor adapters
[ ] Implement dirty-state detection
```

Acceptance:

```bash
devstrap add git@github.com:org/repo.git --path work/org/repo
devstrap sync --namespace-only
devstrap open work/org/repo --cursor
```

## Milestone 3 — Fresh worktree manager

Deliverables:

- create worktree from fetched remote ref;
- metadata tracking;
- cleanup safeguards.

Tasks:

```text
[ ] Implement upstream ref resolver
[ ] Implement fetch before worktree
[ ] Implement worktree path naming
[ ] Implement branch naming
[ ] Implement worktree DB records
[ ] Implement stale-base detection
[ ] Implement cleanup command
```

Acceptance:

```bash
devstrap worktree new work/org/repo --fresh-main --name fix-tests
```

Must prove:

```text
base_sha equals current origin/main at creation time
```

## Milestone 4 — Env capture/hydrate

Deliverables:

- parse `.env`;
- encrypted env bundle;
- env check;
- runtime injection.

Tasks:

```text
[ ] Implement env parser
[ ] Implement encrypted local store
[ ] Implement device key model
[ ] Implement env capture
[ ] Implement env hydrate to .env.local
[ ] Implement devstrap run
[ ] Implement log redaction
```

Acceptance:

```bash
devstrap env capture work/org/repo .env
devstrap env hydrate work/org/repo --write .env.local
devstrap run work/org/repo -- printenv SOME_VAR
```

## Milestone 5 — Mac daemon and watcher

Deliverables:

- `devstrapd serve`;
- LaunchAgent install;
- watcher/reconciler;
- local socket API.

Tasks:

```text
[ ] Implement daemon process
[ ] Implement job queue
[ ] Implement HTTP over Unix socket
[ ] Implement FSEvents/fsnotify watcher
[ ] Implement reconcile job
[ ] Implement LaunchAgent install/uninstall
[ ] Implement logs and daemon status
```

Acceptance:

```bash
devstrap daemon install
devstrap daemon status
mkdir ~/Code/experiments/new-project
devstrap status
```

## Milestone 6 — Linux compatibility

Deliverables:

- Linux build;
- systemd user service;
- inotify watcher;
- Ubuntu smoke tests.

Tasks:

```text
[ ] Implement Linux platform adapter
[ ] Implement systemd service install
[ ] Test watcher on Ubuntu
[ ] Test Git hydration
[ ] Test env hydration
[ ] Test same namespace DB import/export
```

Acceptance:

```bash
devstrap daemon install --user
systemctl --user status devstrapd
devstrap status
```

## Milestone 7 — Multi-device hub

Deliverables:

- small HTTP hub;
- event push/pull;
- encrypted blob upload/download;
- device heartbeat.

Tasks:

```text
[ ] Implement event table in hub
[ ] Implement device registration
[ ] Implement auth token
[ ] Implement sync push/pull
[ ] Implement encrypted blob store
[ ] Implement conflict detection
[ ] Implement namespace sync across two machines
```

Acceptance:

```text
Add project on Mac A.
Skeleton appears on Linux B after sync.
Hydrate on Linux B.
Status shows both devices.
```

## Milestone 8 — Agent runner MVP

Deliverables:

- generic command runner;
- Cursor CLI adapter placeholder;
- scoped env;
- logs/diff summary.

Tasks:

```text
[ ] Implement agent run model
[ ] Implement generic agent adapter
[ ] Implement policy profile
[ ] Implement env allowlist
[ ] Implement log capture
[ ] Implement diff summary
[ ] Implement PR command using gh
```

Acceptance:

```bash
devstrap agent run work/org/repo --engine generic --task "run tests" --command "uv run pytest"
```

## Backlog: V1

```text
[ ] TUI dashboard
[ ] 1Password adapter
[ ] Doppler adapter
[ ] Infisical adapter
[ ] DevPod adapter
[ ] Coder adapter
[ ] GitHub App integration
[ ] Git LFS policy support
[ ] sparse checkout profiles
[ ] draft project encrypted sync
[ ] conflict resolution UI
[ ] shell cd hydration hook
[ ] zsh/fish/bash integrations
[ ] Homebrew tap
[ ] code signing/notarization
```

## Backlog: V2

```text
[ ] StrapFS Linux FUSE prototype
[ ] macFUSE/FSKit prototype
[ ] Apple File Provider prototype
[ ] menu bar app
[ ] Finder status icons
[ ] hosted SaaS hub
[ ] team policies
[ ] SSO
[ ] audit logs
[ ] containerized agent sandbox
[ ] network policy enforcement
```

## MVP risk reducers

Build these early:

- stale-main test suite;
- dirty-worktree safety tests;
- secret redaction tests;
- path conflict tests;
- Mac sleep/wake watcher test;
- Linux watcher limit test;
- hydration interruption test.
