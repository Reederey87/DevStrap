---
last_reviewed: 2026-06-28
tracks_code: [cmd/**, internal/**, .github/**, AUDIT_RECOMMENDATIONS.md, AUDIT_RECOMMENDATIONS_2026-06-27.md, AUDIT_RECOMMENDATIONS_2026-06-28.md]
---
# MVP Roadmap and Backlog

## MVP definition

The MVP is successful when one user can register multiple Macs/Linux machines, keep a consistent `~/Code` structure, materialize repos (on-demand `hydrate` today, moving to eager clone-everything on `sync` — `EAGER-*`), hydrate env safely, and create fresh worktrees for agents.

The "Dropbox experience for code" target — one identical `~/Code` tree that appears automatically across the owner's fleet — is delivered by the 2026-06-28 cloud-sync architecture: content split by type (repo content rides git's own blobless clone/fetch from its existing remote and never transits the hub; env + non-git/draft folders ride age-encrypted content-addressed `age_blob:<sha256>` blobs; the project map rides the signed HLC-ordered event log), eager materialization on `sync`, and a two-plane zero-knowledge hub on Cloudflare R2. See `AUDIT_RECOMMENDATIONS_2026-06-28.md`.

## Recommended build order

Historic milestone numbering (M0–M3.5/M4 are shipped; M5–M7 below describe capability layers):

```text
Milestone 0: repo skeleton and CLI                          [shipped]
Milestone 1: local state and scan/adopt                     [shipped]
Milestone 1.5: namespace event-log and two-root sync spike  [shipped]
Milestone 2: Git hydration and open                         [shipped]
Milestone 3: fresh worktree manager                         [shipped]
Milestone 3.5: thin agent runner MVP                        [shipped]
Milestone 4: env capture/hydrate and runtime injection      [shipped]
Milestone 5: Mac daemon and watcher                         [deferred — see below]
Milestone 6: Linux compatibility                            [portable Go first; native parts deferred]
Milestone 7: multi-device hub                               [reframed as the cloud R2 hub — see below]
```

### 2026-06-28 cloud-sync re-sequencing

The next cycle is re-sequenced around the **eager-clone core + cloud backend**, not a native daemon. IDs reference `AUDIT_RECOMMENDATIONS_2026-06-28.md`. This overlay supersedes the priority of the historic M5–M7 above without renumbering them:

```text
Next:      eager-clone materialization (EAGER-*) — clone-everything (blobless) on `devstrap sync`;
           the whole ~/Code tree is present after sync; no FUSE/placeholder/lazy-VFS
Then:      non-git/draft content sync + .devstrapignore compiler + encrypted bundles (DRAFT-*)
Then:      cloud zero-knowledge hub on Cloudflare R2, two planes (event log + blob store) (HUB-*)
Alongside: cross-platform core — portable Go on macOS + Ubuntu, no native daemon (XP-*)

--- deferred this cycle ---
Mac daemon + native watcher (historic M5):      behind its entry gate, not this cycle
Linux native service/inotify (historic M6 native): portable Go first; native adapter deferred
StrapFS / FUSE / File Provider (Backlog V2):    explicitly deferred
Multi-user / multi-tenant scaling (SCALE-*):    future direction, documented not built
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

On-demand `hydrate` ships and stays valid for materializing one repo. The 2026-06-28 cycle layers **eager clone-everything on `sync`** (`EAGER-*`) on top: `devstrap sync` performs a blobless/partial clone (`git clone --filter=blob:none`) of every namespaced repo from its existing remote up front, so the whole `~/Code` tree is present afterward. Repo content rides git's own transport and never transits the hub; `node_modules`/build artifacts are never synced and are rebuilt on hydrate (`npm`/`pnpm`/`uv install`).

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

> Deferred (2026-06-28). The cloud-sync cycle ships a **cross-platform core first** (`XP-*`) — portable Go on macOS + Ubuntu — with no native daemon or StrapFS this cycle. Eager-clone materialization on `sync` (`EAGER-*`) plus periodic reconciliation cover the loop without a resident watcher. Keep this milestone behind its entry gate below; do not start it until the gate is satisfied and the cloud planes (`EAGER-*`/`DRAFT-*`/`HUB-*`) are in place.

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

> Reframed (2026-06-28) as the **cloud zero-knowledge hub** (`HUB-*`). The chosen backend is **Cloudflare R2 from the start** (S3 API, zero egress, namespaced by `workspace_id`), not a NAS-first phase. The hub stays pluggable behind one `Hub` interface; the file-backed local backend remains **only for tests**. The hub is two planes and sees only ciphertext + a signed map: (a) the **event log** = the namespace map, and (b) a **content-addressed encrypted blob store** = env + non-git/draft content (`age_blob:<sha256>`). It cannot read code, secrets, or drafts. Repo content never transits the hub — it rides git's own blobless clone/fetch from the existing remote.

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
[ ] StrapFS Linux FUSE prototype                 (deferred — see Backlog V2 note)
[ ] macFUSE/FSKit prototype                       (deferred)
[ ] Apple File Provider prototype                 (deferred)
[ ] menu bar app
[ ] Finder status icons
[ ] hosted SaaS hub
[ ] multi-user / multi-tenant scaling (SCALE-*)   (future direction, documented not built)
[ ] team policies
[ ] SSO
[ ] audit logs
[ ] containerized agent sandbox
[ ] network policy enforcement
```

StrapFS / FUSE / File Provider stay **explicitly deferred**: the 2026-06-28 design is eager clone-everything with no lazy virtual filesystem. Multi-user scaling is a documented future direction only; see the `SCALE-*` workstream below for the chosen hosting stack and tenancy model.

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

## Audit follow-ups (second pass, 2026-06-27)

Workstreams added by the second-pass design & implementation audit (`AUDIT_RECOMMENDATIONS_2026-06-27.md`). Ordered by leverage; IDs reference that document.

### P0 — security-relevant, do now
- **Agent isolation is security theater** (`AGEN-01`, `AGEN-02`/`SECU-02`): argv-substring policy is bypassable by any interpreter, and `HOME`/`SSH_AUTH_SOCK` are forwarded into the agent subprocess. Strip credential env; move toward allowlist + OS sandbox.
- **Secret hydration re-emits unescaped values** (`SECR-01`): escape `$`/backtick when writing env files.
- **Key custody silent downgrade** (`SECR-04`/`SECU-01`): downgrades to a plaintext age key on ANY keychain error, not only unavailability.
- **`agent pr` is GitHub-only** (`FORGE-01`): fails post-push on non-GitHub forges.
- **No-remote repo corrupts the namespace** (`NOVCS-01`): classify as `local_git`, enforce non-empty `remote_key`.
- **CI fragility** (`CI-01`): pin `govulncheck`, split it out of the "Go tests" job.

### Cross-machine working-state sync (the "forgot to push" feature)
- **Layer A — git-state validation plane** (Phase 0): `repo.gitstate.observed` events + `device_gitstate` sidecar table + `status --all-devices`/`doctor`.
- **Layer B — WIP refs** (Phase 1): `git stash create` → `refs/devstrap/wip/*` push + `wip` subcommands; resolver-exclusion test.
- **Layer C — encrypted bundles** (Phase 3): build out `draft.snapshot.created` for non-git/draft folders.

### Non-VCS / forge support
- Non-VCS/remote-less/multi-remote handling (`NOVCS-02..05`): `plain_folder` emission, `promote` command, remote preflight.
- Forge-agnostic PR via a `Forge` interface (`FORGE-02..05`): `gh`/`glab`/`tea`, token allowlist, Azure key folding, `doctor` probe.

### Sync hub (Phase 2)
- HTTP/SSE zero-knowledge hub `cmd/devstraphub` + `HTTPHub` client; mTLS device certs; full-state snapshot exchange before retention GC (audit Section 6).
- Wire the resume cursor (`ARCH2-02`): `sync` currently replays from HLC 0.

### Architecture & hygiene epics
- Extract `internal/engine` from `internal/cli` (`ARCH2-01`) before the daemon phase.
- Signed **audit-log subsystem** (`spec/15`) — currently absent.
- **`.devstrapignore` compiler** (`spec/11`) — currently absent; root cause of duplicated prune/secret/deny lists.
- Daemon crash-recovery/reaper, observability/log-rotation, large-namespace scan benchmarks, cross-process `state.db` coordination, migration-rollback tests (audit coverage gaps).

## Audit follow-ups (cloud-sync pass, 2026-06-28)

Workstreams added by the cloud-sync architecture pass (`AUDIT_RECOMMENDATIONS_2026-06-28.md`). These **extend** the 2026-06-27 second-pass audit above — they do not revert it. The product goal is the "Dropbox experience for code": one identical `~/Code` tree that appears automatically across the owner's fleet. The core rule is **file-sync split by content type — never blanket file-sync, never file-sync `.git`** (it corrupts the repo). New planned commands/flags are **future**, not yet shipped.

### EAGER-* — eager-clone materialization
- Make `devstrap sync` perform eager **clone-everything** up front via blobless/partial clone (`git clone --filter=blob:none`) of every namespaced repo from its existing remote; the whole `~/Code` tree is present after sync.
- Repo content rides git's own transport and **never** transits the DevStrap hub. No FUSE/placeholder/lazy-VFS — StrapFS stays explicitly deferred.
- `node_modules`/build artifacts are never synced; rebuild on hydrate (`npm`/`pnpm`/`uv install`).

### DRAFT-* — non-git / draft content sync
- Sync env vars and non-git/draft folders as **age-encrypted, content-addressed `age_blob:<sha256>` blobs** (the human/draft plane), never as repo content and never byte-merged.
- Build the **`.devstrapignore` compiler** (shared with `spec/11`) so bundle contents exclude generated/secret/junk paths deterministically.
- Encrypted working-tree/draft bundles (`draft.snapshot.created`) build on the 2026-06-27 Layer C; conflicts use detect-don't-merge with dual-copy as the safe default (no byte-merge of opaque files; CRDTs solve a different problem).

### HUB-* — cloud zero-knowledge hub
- Ship `cmd/devstraphub` as the **two-plane** zero-knowledge hub: (a) event log = the namespace map; (b) content-addressed encrypted blob store = env + non-git/draft content. The hub sees only ciphertext + a signed map.
- **Cloudflare R2 backend from the start** (S3 API, zero egress, namespaced by `workspace_id`; zero-knowledge via client-side age encryption). **No NAS-first phase.** Keep the backend pluggable behind one `Hub` interface; retain a file-backed local backend **only for tests**.
- mTLS device certs, full-state snapshot exchange before retention GC, and wire the resume cursor (`ARCH2-02`, `sync` currently replays from HLC 0).
- **Device trust must fail closed** once enrollment exists (today `SECU-03` fails open). Revoke ⇒ re-encrypt affected blobs to the reduced recipient set + flag secrets for rotation (age has no native revocation).

### XP-* — cross-platform core first
- Ship a **portable Go core on macOS + Ubuntu** with OS-specific magic deferred: no native daemon, no StrapFS this cycle. Eager-clone on `sync` plus periodic reconciliation cover the loop without a resident watcher.
- Validates the GMKtec Ubuntu box and graphics-laptop targets alongside the Mac Minis.

### SCALE-* — multi-user / multi-tenant scaling (future direction, documented not built)
- **Chosen stack:** Fly.io for compute (control plane + agent runners — Firecracker microVM isolation, 35+ regions, scale-to-zero/suspend-resume, runs the Go binary natively) + Cloudflare R2 for the sync hub (namespaced by `workspace_id`; zero-knowledge ⇒ tenant isolation by construction) + managed Postgres (Neon/Supabase) for the control-plane DB. Runner escape-hatch: **E2B** (self-hostable microVM agent sandboxes).
- **Rejected as primary:** Railway (shared-kernel containers — fine for the control plane or a trusted single instance, not for untrusted multi-tenant code); Vercel (strong if the stack were Next.js/TS via Sandbox + Functions/Workflows, but DevStrap is Go-first, so its TS/Python sandbox SDKs are an awkward fit); Hetzner (cheapest always-on box, good for the solo MVP, but no microVM/global/scale-to-zero).
- **Scaling model:** control/data-plane split, tenancy spectrum (pooled → dedicated/BYOC), cell-based scaling; Coder is the reference architecture for agents-on-your-infra at scale.

### Deferred (2026-06-28)
- **StrapFS / FUSE / macFUSE / FSKit / Apple File Provider** — no lazy virtual filesystem in this design; remains Backlog V2.
- **Mac daemon + native FSEvents watcher and Linux native systemd/inotify adapters** — portable Go core first (`XP-*`); native service work stays behind the Milestone 5/6 entry gates.
- **Multi-user / multi-tenant build-out (`SCALE-*`)** — documented direction only; nothing built this cycle.
- Out of scope for these docs: which LLM/agent API the runner uses (a separate concern, deliberately not specified here).
