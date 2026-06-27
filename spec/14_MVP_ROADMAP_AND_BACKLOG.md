---
last_reviewed: 2026-06-26
tracks_code: [cmd/**, internal/**, .github/**, AUDIT_RECOMMENDATIONS.md]
---
# MVP Roadmap and Backlog

## MVP definition

The MVP is successful when one user can register multiple Macs/Linux machines, keep a consistent `~/Code` structure, materialize repos on demand, hydrate env safely, and create fresh worktrees for agents.

## Recommended build order

```text
Milestone 0: repo skeleton and CLI
Milestone 1: local state and scan/adopt
Milestone 1.5: namespace event-log and two-root sync spike
Milestone 2: Git hydration and open
Milestone 3: fresh worktree manager
Milestone 3.5: thin agent runner MVP
Milestone 4: env capture/hydrate and runtime injection
Milestone 5: Mac daemon and watcher
Milestone 6: Linux compatibility
Milestone 7: multi-device hub
```

## Milestone 0 — Project skeleton

Deliverables:

- Go module; done
- CLI skeleton; done
- config loading; initial Viper wiring done
- structured logging; CLI redaction and log-level wiring done, daemon/service output still future
- initial docs; done
- CI for macOS/Linux; done

Tasks:

```text
[x] Create local repo workspace scaffold
[x] Add Cobra CLI
[x] Add Goose SQLite migration framework
[x] Add slog logger with redaction ReplaceAttr and retention policy
[x] Add config path resolver
[x] Add Go test harness and CI workflow
[x] Add CI spec-drift gate with per-spec frontmatter and work-log enforcement
```

Acceptance:

```bash
devstrap version
devstrap help
```

Current validation:

```bash
gofmt -w cmd internal
go test -race ./...
```

Remaining Milestone 0 work:

- add daemon/service log sinks when the daemon exists;
- keep focused unit tests for config/state/CLI packages current.

## Milestone 1 — Local workspace state

Deliverables:

- `devstrap init ~/Code`;
- SQLite schema;
- workspace/device record;
- namespace entries;
- scan existing repos.

Tasks:

```text
[x] Implement init command
[x] Implement DB migrations
[x] Implement device ID generation
[x] Implement path normalization
[x] Implement Git repo scanner
[x] Implement duplicate remote detection
[x] Implement status command
```

Acceptance:

```bash
devstrap init ~/Code
devstrap scan ~/Code --adopt
devstrap status
```

## Milestone 1.5 — Namespace event-log sync spike

Deliverables:

- two temporary roots representing two devices;
- one in-process or file-backed test Hub;
- append-only events ordered by HLC;
- per-peer sync cursors;
- idempotent apply keyed by event id;
- skeleton creation on the second root;
- at least one conflict detector.

Acceptance:

```text
Device A adds work/org/repo
Device A pushes project.added
Device B pulls from cursor
Device B creates the skeleton
Replaying the same event is a no-op
Concurrent same-path/different-remote events converge to the canonical `(hlc, device_id, event_id)` winner and create one stable conflict
```

This milestone is required before the daemon and watcher become the main implementation focus.

Current implementation has the core HLC, persisted local event stamping with per-device sequence numbers, project event helpers, `add`/`scan --adopt` project-event emission, transactional idempotent insert/apply, HLC-gated project delete tombstones/restores, content-hash duplicate validation, order-independent same-path/different-remote conflict reconciliation, a file-backed hub adapter for tests, and `devstrap sync --hub-file` for file-backed namespace event push/pull. Remote device registration, tombstone garbage collection, and skeleton reconciliation across real roots remain future work.

Decision note: the `device_sig` / `prev_event_hash` chain columns were implemented locally as a deliberate, accepted divergence from the right-size-the-spike guidance (ARCH-2). Their on-wire format must be re-reviewed before a production hub freezes it.

## Milestone 2 — Git hydration and open

Deliverables:

- skeleton directories;
- repo add;
- hydrate Git repo;
- open in Cursor/VS Code.

Tasks:

```text
[x] Create skeleton folder writer
[x] Implement devstrap add
[x] Implement git clone partial/full
[x] Implement git fetch/status
[x] Implement hydrate command
[x] Implement editor adapters
[x] Implement dirty-state detection
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
[x] Implement upstream ref resolver
[x] Implement fetch before worktree
[x] Implement worktree path naming
[x] Implement branch naming
[x] Implement worktree DB records
[x] Implement reusable stale-base detection via `devstrap worktree status <id>`
[x] Enforce stale-base detection in `devstrap worktree finalize`
[ ] Wire the same stale-base gate into PR creation once that command exists
[x] Honor stored Git LFS policy for agent worktrees
[x] Implement cleanup command
[x] Implement worktree remove `--force` and stale missing-path prune
```

Acceptance:

```bash
devstrap worktree new work/org/repo --fresh-upstream --name fix-tests
```

Must prove:

```text
base_sha equals current origin/<default_branch> at creation time
```

## Milestone 3.5 — Thin agent runner MVP

Deliverables:

- generic command runner; done
- Cursor CLI adapter placeholder; future
- scoped no-secret default env; done
- logs/diff summary; done

Tasks:

```text
[x] Implement agent run model
[x] Implement generic agent adapter
[x] Implement wrapper-level command policy profile
[x] Implement wrapper-level file path policy profile
[x] Implement no-secret default env scope
[x] Implement log capture
[x] Implement diff summary
[x] Implement PR command using gh with stale-base gate
```

Acceptance:

```bash
devstrap agent run work/org/repo --engine generic --task "run tests" --command "uv run pytest"
```

This milestone intentionally comes before the daemon, native watcher, Linux service, and production hub. It proves the differentiating single-machine loop on top of the fresh worktree manager without making watcher or hub work a correctness dependency.

## Milestone 4 — Env capture/hydrate

Deliverables:

- parse `.env`;
- encrypted env bundle;
- env check;
- runtime injection.

Tasks:

```text
[x] Implement env parser
[x] Implement encrypted local store
[x] Implement local age device identity and public recipient persistence
[x] Implement local Ed25519 device signing identity and event signatures
[x] Move device private identities to OS keychain/Secret Service adapters with file fallback
[x] Implement local device trust-state CLI
[x] Implement manual per-device env-decryption approval for local captures
[x] Implement env capture
[x] Implement env hydrate to .env.local
[x] Implement 1Password provider-ref hydration via `op inject`
[x] Implement devstrap run
[x] Implement log redaction
```

Acceptance:

```bash
devstrap env capture work/org/repo .env
devstrap env hydrate work/org/repo --write .env.local
devstrap run work/org/repo -- printenv SOME_VAR
```

## Milestone 5 — Mac daemon and watcher

Entry gate (review before starting M5):

- the indexer-hydration-storm test must pass (watcher treats events as hints and does not hydrate without explicit open/adopt);
- the Mac sleep/wake watcher test must pass;
- a written "do we still need the daemon?" review must confirm periodic-scan reconciliation is insufficient for real usage before daemon work begins.

Deliverables:

- `devstrapd serve`;
- LaunchAgent install;
- watcher/reconciler;
- local socket API.

Tasks:

```text
[x] Define platform adapter interfaces and detection seam before native watcher/service work
[x] Add guard that prevents `runtime.GOOS` branching outside `internal/platform`
[ ] Implement daemon process
[ ] Implement job queue
[ ] Implement HTTP over Unix socket
[x] Implement fsnotify watcher adapter for Darwin/Linux
[ ] Implement FSEvents-specific Mac watcher if fsnotify/kqueue proves insufficient
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
[x] Implement initial Linux platform detection with polling/unsupported fallbacks
[ ] Implement native Linux platform adapter
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

Entry gate (review before starting M7): require a real two-machine path-drift usage signal before building a bespoke `devstraphub`; a hidden manifest git repo (see `spec/01` / `spec/04`) may substitute for a bespoke service.

Decision note: re-evaluate whether a hidden manifest git repo (see `spec/01` / `spec/04`) is a faster hub than a bespoke service before building `devstraphub`.

Dependency gate: encrypted blob upload/download must not ship until Milestone 4 has device key identities plus per-device env-decryption approval. The local trust plane now supports manual device enrollment/approval for capture recipients; production blob sync still requires automatic remote enrollment and fingerprint confirmation.

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

## Backlog: V1

```text
[ ] TUI dashboard
[ ] 1Password adapter
[ ] Doppler adapter
[ ] Infisical adapter
[ ] DevPod adapter
[ ] Coder adapter
[ ] GitHub App integration
[x] Git LFS policy support
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

- stale-default-branch test suite;
- dirty-worktree safety tests;
- secret redaction tests;
- path conflict tests;
- Mac sleep/wake watcher test;
- Linux watcher limit test;
- hydration interruption test.

## Risk traceability

| Risk | Owning milestone | Required test |
|---|---:|---|
| stale agent base | 3 | worktree base SHA equals fetched `origin/<default_branch>` while local branch is stale |
| dirty local code overwritten | 2 | dirty repo sync performs fetch/classify only |
| plaintext secret leak | 4 | `grep -r <secret> ~/.devstrap` finds nothing after capture/run |
| symlink escape | 1 | scanner refuses links escaping managed root |
| indexer hydration storm | 5 | watcher treats events as hints and does not hydrate without explicit open/adopt |
| long-offline device | 1.5 | expired cursor uses full-state snapshot fallback |
| daemon crash mid-job | 5 | startup requeues leased jobs and prunes partial clones/worktrees |
| database corruption/backup | 0 | `db backup` uses `VACUUM INTO`; `doctor` reports `quick_check` |

## Release and upgrade gates

- Versioning follows SemVer.
- Release builds inject `version`, `commit`, and `date` via `-ldflags`.
- `doctor` reports binary version, schema version, and pending migration status.
- Upgrade runbook: stop daemon, `devstrap db backup`, migrate, run `doctor`, restart daemon.
- Rollback requires a pre-migration `VACUUM INTO` backup.
- Socket/API responses include protocol and schema versions once the daemon exists.

## Observability and privacy gates

- DevStrap sends no telemetry by default.
- Any future telemetry is opt-in per device and disabled by `DEVSTRAP_TELEMETRY=off`.
- Daemon logs use `log/slog` with one redaction choke point.
- Logs rotate under `~/.devstrap/logs/` and never include plaintext secrets.
