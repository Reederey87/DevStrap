---
last_reviewed: 2026-06-30
tracks_code: [cmd/**, internal/**, .github/**, AGENTS.md, README.md, go.mod, go.sum, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md]
---
# DevStrap — Start Here

> A second-pass design & implementation audit (2026-06-27) is recorded at the repo root in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md`. It drives the new workstreams referenced throughout the specs: cross-machine working-state sync, non-VCS/remote-less project support, forge-agnostic PR creation, and the zero-knowledge sync hub.

> A cloud-sync architecture pass (2026-06-28) is recorded at the repo root in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`. It extends — does not revert — the 2026-06-27 audit, and pins the "Dropbox experience for code" model: `devstrap sync` eagerly reconstructs the whole `~/Code` tree (blobless-clone repos from their existing remotes + pull age-encrypted env/draft blobs + hydrate env); file-sync is split by content type and never blanket-syncs `.git`; the two-plane zero-knowledge hub (signed HLC namespace-map event log + content-addressed encrypted blob store) ships on Cloudflare R2 behind one pluggable Hub interface; cross-platform Go core comes first and StrapFS/FUSE stays explicitly deferred. Its workstream IDs are `EAGER-*`, `DRAFT-*`, `HUB-*`, `XP-*`, and `SCALE-*`. **The `EAGER-*`/`DRAFT-*`/`HUB-01..08`/`XP-*` workstreams from this pass shipped in PR #16.**

> A fourth-pass design & implementation audit (2026-06-28, post-PR-#16) is recorded at the repo root in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md`. It audits the *now-shipped* cloud-sync system (not a re-plan) across six dimensions — Security & Cryptography (`SEC-*`), Sync Engine & Data Model (`SYNC-*`), Cloud Hub & Scalability (`HUB-09..16`, continuing the shipped `HUB-01..08` series), Git Materialization & Agents (`GIT-*`), Code Quality & Testing (`QUAL-*`), and Product/UX & New Features (`PROD-*`) — with 44 grounded findings. Its headline imperative: harden the hub's zero-knowledge guarantees (encrypt the namespace map, verify blob hashes on fetch, make revocation real) and bound sync-log growth (compaction/snapshot, GC) **before** the R2 backend is switched on, then grow the product surface (`devstrap clone`, a graded `doctor --fix`, a `service install` daemon).

"Workspace Passport" is the core-concept tagline — the portable, managed code namespace that appears identically on every device — not a separate product name (see `spec/adr/0001-product-naming.md`).

## Selected architecture

The optimal architecture for the first Mac implementation is a **managed physical code namespace**, not a full virtual filesystem.

In practice:

```text
~/Code is a real folder.
DevStrap owns the structure and metadata.
Git owns repository content.
A local daemon keeps the namespace consistent.
Repos are skeletons until materialized; `devstrap sync` materializes the whole tree eagerly (blobless clone up front), not lazily on open.
Secrets are referenced or encrypted, not blindly copied.
Agents always get fresh worktrees from fetched remote refs.
Local-only / remote-less folders are first-class, synced via encrypted bundles — never adopted as broken clonable git repos.
The materialization layer is forge-agnostic (GitHub/GitLab/Bitbucket/Gitea/self-hosted); only PR/MR creation is forge-specific.
"Forgot to push" is solved git-natively (git-state validation + WIP refs), never by file-sync; this human plane never feeds agent base resolution.
```

This gives the Dropbox-like experience you want without starting with the hardest possible engineering problem: implementing a reliable cross-platform filesystem.

## Product promise

```text
Install DevStrap on a new Mac, Linux box, cloud machine, or agent runner.
Point it at ~/Code.
Authenticate Git + secrets.
Run `devstrap sync` once.
The whole ~/Code tree is reconstructed eagerly: every repo is blobless/partial-cloned
  from its existing remote, env/draft folders are pulled as age-encrypted blobs, and env profiles hydrate.
node_modules and build artifacts are never synced — they are rebuilt on hydrate.
Starting an agent creates a fresh, isolated worktree from the fetched remote default branch.
```

This is the **eager-clone Workspace Passport**: `devstrap sync` materializes content, not just metadata. There is no FUSE/placeholder/lazy-VFS magic in this design — after sync the tree is really present on disk. StrapFS stays explicitly deferred (see "Why not FUSE or Apple File Provider first?" and Phase 4 below).

## Why not FUSE or Apple File Provider first?

A true lazy virtual filesystem is attractive, but it should be a later layer called **StrapFS**, not the MVP.

The first version should avoid:

- macOS kernel-extension or system-extension installation friction;
- Finder/File Provider complexity;
- FUSE performance, caching, file locking, and editor-indexer edge cases;
- cross-platform filesystem semantics before the product loop is proven.

The MVP can still feel close to Dropbox because it creates the same directory tree everywhere and materializes projects through CLI, shell hooks, editor adapters, and agent adapters. The 2026-06-28 cloud-sync pass keeps this position: materialization is **eager whole-tree clone** on `devstrap sync` (blobless/partial clone up front), and StrapFS/FUSE remains explicitly deferred — there is no placeholder/lazy-VFS layer in this design.

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
  - DevStrap Hub event log (zero-knowledge HTTP/SSE; cursor=HLC; 410->snapshot)
  - device registration (mTLS device certs, trust-state revocation)
  - namespace sync
  - encrypted env/draft blobs (content-addressed age_blob:<sha256>)
  - device status
  - cross-machine working-state: git-state validation plane + WIP refs (refs/devstrap/wip/*)

Phase 3: Agent workspaces
  - one branch/worktree per task
  - fresh remote-default base
  - command/file policy (OS-enforced sandbox is a follow-up)
  - logs and forge-agnostic PR/MR workflow (gh/glab/tea)

Phase 4: Optional StrapFS
  - macOS: File Provider or macFUSE/FSKit evaluation
  - Linux: FUSE
  - Windows future: WinFsp
```

**These phases describe capability layers, not the build order.** The actual, deliberately re-ordered sequencing — the thin agent runner ships *before* the daemon and hub — is canonical in `14_MVP_ROADMAP_AND_BACKLOG.md`; defer to it rather than reading the list above as a schedule. **Current position:** Phase 0 CLI and the Phase 3 agent loop are shipped; the Phase 1 daemon is gated; Phase 2 multi-device sync now eager-materializes the whole `~/Code` tree (blobless clone + env hydrate + draft bundle extract) on `devstrap sync` behind a pluggable `Hub` interface with a file-backed test backend and a Cloudflare R2/S3 production backend. Cursor-based incremental pull, fail-closed event verification, device-revoke blob re-encryption, and the `.devstrapignore` compiler are shipped. The portable `run-loop` delivers periodic convergence without a daemon.

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

Last validated: `2026-06-30`.

Implemented in this repository:

- Go module: `github.com/Reederey87/DevStrap`.
- CLI entrypoint: `cmd/devstrap`.
- Commands: `version`, `init`, `scan`, `add`, `clone`, `hydrate`, `open`, `sync --hub-file`, `hub gc`, `materialize`, `draft snapshot create`, `run-loop`, `worktree new/status/finalize/list/remove/cleanup/unlock`, `env capture/hydrate/bind/rotate`, `run`, `agent run/list/show/pr`, `devices enroll/list/approve/revoke/lost/rename`, `conflicts list/show/resolve`, `status` (`--watch`), `doctor` (`--remote`), and `db migrate/status/backup/down`.
- Structured `slog` setup with CLI/env log-level control, secret-key/value redaction helpers, and no whole-context log attributes.
- Local state package with embedded Goose SQLite migrations.
- SQLite open path with per-connection pragmas, WAL, busy timeout, asserted foreign-key enforcement, startup `foreign_key_check`, `0600` database mode, and single-writer pool.
- Generated stable local `ws_<uuidv7>` workspace identity persisted during `init`, with a database-enforced singleton workspace invariant for the Phase 0 MVP.
- Event-ordering schema for HLC, sync cursors, event delivery, hashes/signatures, tombstones, and namespace source-event metadata.
- Generated stable local `dev_<uuidv7>` device identity persisted during `init`, plus age X25519 and Ed25519 signing identities whose public keys are stored in SQLite and whose private identities are kept out of SQLite/config in the OS keychain/Secret Service when available, falling back to `~/.devstrap/keys` with mode `0600` when the system keyring is unavailable.
- Namespace path normalization with NFC display normalization, case-folded `path_key`, and unsafe path rejection.
- Filesystem scan/adopt workflow with generated-folder pruning, secret-looking filename warnings, symlink escape warnings/conflicts, Git remote normalization, duplicate remote reporting, and project status rows.
- Git add/hydrate/open workflow using skeleton directories, sibling temp-dir clone staging with atomic promotion after success, partial clone by default, configurable LFS policy, metadata-backed repo operation locks, git subprocess timeouts, prompt disabling, sanitized git environment, protocol allowlist, typed Git error classification with transient network retry for clone/fetch, and URL credential redaction in git errors.
- Shared child-process environment sanitizer with explicit allowlists and non-overridable dangerous-name stripping, wired into Git subprocesses and editor launch.
- Initial `internal/platform` adapter seams for watcher, service manager, keychain, and editor launch, with build-tagged platform detection, a polling watcher fallback for unsupported platforms, fsnotify-backed watcher adapters for Darwin/Linux with debounce/coalescing, explicit unsupported service/keychain placeholders, and `open` routed through the editor adapter.
- Hardened `.env` parser plus `devstrap env capture/hydrate/bind` and `devstrap run` flows: capture refuses interpolation unless `--literal`, encrypts parsed values to the local age recipient, stores only `age_blob:<sha256>` refs in SQLite, writes `0600` ciphertext blobs, and gitignores captured env files; hydrate decrypts local encrypted blobs or resolves 1Password refs through `op inject`, writes the requested env file atomically with mode `0600`, refuses overwrites unless `--force`, and gitignores the hydrated target; bind stores 1Password `op://` refs without resolving plaintext; run injects encrypted profiles into subprocess env or delegates provider refs to `op run`.
- Fresh upstream worktree creation that holds the repo operation lock, fetches `origin/<default_branch>`, resolves base SHA from the remote ref, honors stored LFS policy with pull-or-warn behavior, records worktree metadata, exposes `worktree status` stale-base detection, and gates `worktree finalize` on the recorded base unless `--allow-stale-base` is explicit.
- Thin generic agent runner that creates a fresh worktree, runs explicit argv commands with a sanitized no-secret default environment and wrapper-level command/file path policy, captures a `0600` log, records `agent_runs`, summarizes Git status/diff, and gates `agent pr` on the recorded base before pushing and creating a forge-aware PR/MR when the relevant CLI is available (`gh`/`glab`/`tea`) or printing a compare URL for unsupported forges.
- Device trust-state CLI for listing, renaming, approving, revoking, and marking non-local devices lost, with refusal to revoke the current local device.
- In-process/file-backed sync spike with mutex-protected HLC send/receive, persisted local HLC/sequence stamping, local event signatures, signed-event verification when the source signing key is known, logical-counter overflow handling, clock-skew rejection, append-only event helpers, HLC-gated project delete tombstones, deterministic replay ordering, duplicate event idempotency, and order-independent same-path/different-remote conflict reconciliation.
- User-facing `devstrap sync --hub-file <path>` for the file-backed test hub; `add` and `scan --adopt` stamp local project events, sync pushes local events, pulls hub events, applies namespace events idempotently, and reports that hydration/fetch reconciliation remains future work.
- Value-level secret redaction in `internal/redact` (a `Secret` capability type plus URL/userinfo stripping, a token-shape scrubber, and a line-buffering scrubbing writer) wired into sync event payloads, CLI error output, the persisted agent log, and slog attributes.
- Scan boundary hardening: only validated remotes are persisted, escaping symlinks are typed and hard-excluded with use-time revalidation before materialization, and `scan --quarantine` isolates secret-looking files in a dated `0600` quarantine.
- Authoritative default-branch resolution for fresh worktrees (`ls-remote --symref` with `set-head --auto` repair and a non-authoritative warning), `worktree cleanup --force`, `worktree unlock <path>`, and `doctor` repo-lock reporting.
- Sync apply-path clock-skew quarantine, local-clock advance on receive, `project.renamed` handling, delete-vs-dirty conflicts, and tombstone GC; plus `secret_bindings.needs_rotation` flagging on device revoke/lost surfaced in `doctor`.
- A `DEVSTRAP_NO_KEYCHAIN` gate forcing the file-backed key store for headless/CI runs.
- Focused tests for `internal/cli`, `internal/config`, `internal/git`, `internal/logging`, `internal/pathkey`, `internal/redact`, `internal/scan`, `internal/specdrift`, `internal/sync`, and `internal/state`, plus a `rogpeppe/go-internal` testscript end-to-end harness exercising `cmd/devstrap` through the real binary.
- Spec frontmatter and a Go-based `cmd/spec-drift` CI gate that maps changed code/config paths to tracked spec files and requires the work log on code/spec/doc changes, plus a command-doc drift test that keeps the spec command list in sync with the binary, and a product-naming ADR at `spec/adr/0001-product-naming.md`.
- README, MIT license, `.gitignore`, GitHub Actions CI with separate spec-drift, test, and golangci-lint jobs, `CONTRIBUTING.md`, `SECURITY.md`, `CODEOWNERS`, Dependabot, issue/PR templates, and concise `AGENTS.md`.

Not implemented yet (genuinely unbuilt — features that are partly shipped are listed under "now built" below, never here):

- the bespoke **HTTP/SSE relay** and full-state snapshot exchange (the live R2/S3 backend is shipped: the `aws-sdk-go-v2` S3 adapter is wired behind the `hubFromOptions` `r2://` seam, with the `Hub` interface, R2 keying/retry/conditional-put logic, blob GC, retention floor, and content-hash verification all shipped and unit-tested, and the same conformance contract proven against MinIO via an env-gated integration test);
- production **remote device registration** and out-of-band fingerprint confirmation (the local trust plane and device-revoke rewrap are shipped); synced encrypted **env-bundle** exchange (draft-bundle exchange is shipped);
- daemon, local socket API, FSEvents-specific Mac watcher, LaunchAgent/systemd installers;
- OS-enforced agent sandboxing, project-env allowlists, and non-generic engine adapters;
- cross-machine working-state sync — git-state validation plane (`repo.gitstate.observed`) and WIP refs (`refs/devstrap/wip/*`); the encrypted draft-bundle layer (Layer C) is shipped;
- forge hardening beyond the shipped PR/MR routing — `agent pr` detects GitHub/GitLab/Gitea/Bitbucket/Azure and routes through `gh`/`glab`/`tea` (or a compare-URL fallback) and resolves SSH host aliases via `ssh -G` (`P5-CLI-04`); `doctor` still needs forge-specific CLI probes and broader hermetic test coverage (`FORGE-04/05`).

Cloud-sync workstreams from the 2026-06-28 audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`), now built:

- eager-clone materialization (`EAGER-*`) — `devstrap sync` reconstructs the whole `~/Code` tree by blobless/partial-cloning every repo from its existing remote up front with bounded concurrency and per-project failure isolation; env profiles hydrate; `node_modules`/build artifacts are rebuilt on hydrate (opt-in), never synced;
- non-git/draft content sync (`DRAFT-*`) — a `.devstrapignore` compiler (`internal/ignore`) and age-encrypted, content-addressed `age_blob:<sha256>` bundles for non-git/draft folders pushed/pulled through the blob plane (`draft snapshot create`, `draft.snapshot.created` event);
- cloud hub backend (`HUB-*`) — the two-plane zero-knowledge `Hub` interface (event log + content-addressed encrypted blob store) with the Cloudflare R2/S3 backend (`internal/hub`, the `aws-sdk-go-v2` S3 adapter wired behind `hub: r2://<bucket>`) and the file-backed backend retained for tests;
- cross-platform hardening (`XP-*`) — portable `devstrap run-loop` (scan → sync → materialize, no daemon); e2e testscript proving two-device materialization; headless key custody test; NFC/case-fold path invariant test;
- multi-user future (`SCALE-*`) — documented-not-built hosting/scaling direction (Fly.io compute + R2 hub + managed Postgres control plane; control/data-plane split and cell-based tenancy);
- fail-closed event verification on enrollment (`HUB-03`) — once any approved device exists, all non-local events require valid signatures from approved devices; device revoke re-encrypts affected blobs to the reduced recipient set, deletes superseded hub ciphertext when `--hub-file` is given, and flags secrets for rotation (`HUB-04`/`SEC-01`).

Local validation performed:

```bash
gofmt -w cmd internal
golangci-lint run
go run ./cmd/spec-drift --base origin/main --head HEAD
GOCACHE=/tmp/devstrap-gocache go test ./...
GOCACHE=/tmp/devstrap-gocache go test -race ./...
```

The project intentionally remains Go-first. Node/npm can be added later as a packaging or installer channel if useful, but the runtime should stay Go.

## What to build first

Build the boring but powerful version first:

```bash
devstrap init ~/Code
devstrap scan ~/Code --adopt
devstrap status
devstrap open work/nclh/foc-models --cursor
devstrap worktree new work/nclh/foc-models --fresh-upstream --name route-tests
devstrap env capture work/nclh/foc-models .env
devstrap env hydrate work/nclh/foc-models --write .env.local
devstrap sync   # today: namespace-map reconcile + --hub-file spike.
                # planned (EAGER-*/HUB-*): eagerly blobless-clone every repo and pull
                # encrypted env/draft blobs so the whole ~/Code tree materializes.
```

The first killer loop (eager-clone Workspace Passport):

```text
1. Add or create a project on Machine A.
2. DevStrap records it in the signed HLC namespace map (path, remote, env profile, policy),
   and pushes any non-git/draft content + env as age-encrypted blobs to the hub.
3. Machine B runs `devstrap sync` and pulls the updated namespace map.
4. Sync eagerly materializes the whole tree: every repo is blobless/partial-cloned from its
   existing remote, env/draft folders are pulled from encrypted blobs, env profiles hydrate.
   (.git is never file-synced; node_modules/build artifacts are rebuilt, not synced.)
5. The same folder paths are really present on disk — no skeleton to "open" first.
6. Agent work starts from a fresh remote default branch, not a stale local default branch.
```

The branch workflow that backs this invariant — trunk-based development on a single protected `main`, all changes via pull request, and the rule that agents and worktrees always base from the fetched `origin/main`, never any local branch — is defined canonically in `AGENTS.md`. Refer to it there rather than restating the model here.

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
- `18_WORK_LOG.md` — concise end-of-cycle implementation tracking and handoff notes.
- `19_CLOUD_PROVISIONING_GUIDE.md` — register/configure the chosen cloud stack (Fly.io + Cloudflare R2 + Neon).
