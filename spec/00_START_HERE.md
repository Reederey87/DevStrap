---
last_reviewed: 2026-07-17
tracks_code: [cmd/**, internal/**, .github/**, AGENTS.md, README.md, go.mod, go.sum, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-07-10_PASS7.md]
---
# DevStrap — Start Here

> Audit history and the open backlog live in `docs/audits/README.md`; the latest pass is `docs/audits/AUDIT_RECOMMENDATIONS_2026-07-10_PASS7.md`. The seventh pass (2026-07-10, trunk `d667530`) produced 47 findings (P1=1, P2=25, P3=21) tracked in that ledger; the sixth pass (2026-07-01) is fully closed (43/43).

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
  - command/file policy + OS-enforced sandbox (Seatbelt / bubblewrap+Landlock+seccomp, shipped)
  - logs and forge-agnostic PR/MR workflow (gh/glab/tea)

Phase 4: Optional StrapFS
  - macOS: File Provider or macFUSE/FSKit evaluation
  - Linux: FUSE
  - Windows future: WinFsp
```

**These phases describe capability layers, not the build order.** The actual, deliberately re-ordered sequencing — the thin agent runner ships *before* the daemon and hub — is canonical in `14_MVP_ROADMAP_AND_BACKLOG.md`; defer to it rather than reading the list above as a schedule. **Current position:** Phase 0 CLI and the Phase 3 agent loop are shipped; the Phase 1 daemon is gated; Phase 2 multi-device sync now eager-materializes the whole `~/Code` tree (blobless clone + env hydrate + draft bundle extract) on `devstrap sync` behind a pluggable `Hub` interface with a file-backed test backend and a Cloudflare R2/S3 production backend. Per-origin-device Seq transport cursors (late pushes can never be stranded, PR #59), durable classified pull-drop records, fail-closed event verification, device-revoke blob re-encryption, the `.devstrapignore` compiler, and event-log compaction + snapshot exchange (`hub compact`, snapshot bootstrap for fresh devices, tombstone GC over signed sync acks) are shipped. The portable `run-loop` delivers periodic convergence without a daemon, and `devstrap service install` wraps it in a per-user launchd LaunchAgent / systemd user service so that convergence runs unattended (`P4-PROD-04`).

> **Near-term direction (pass-6, future-facing).** The **multi-device hardening freeze** (`AD-2`) is **complete** (2026-07-03): all four confirmed sync/crypto criticals are shipped — `P6-SEC-01`, `P6-SYNC-01`, `P6-HUB-01`, and `P5-SYNC-01` (per-origin-device Seq transport cursors, PR #59, ending the stranded-late-push loss class). New capability planes (HTTP/SSE relay, daemon, StrapFS, hosting/SaaS docs) are unblocked from the freeze's perspective. **Event-log compaction + full-state snapshot exchange (`P4-SYNC-02`/`P4-HUB-11`) SHIPPED 2026-07-04** (PRs #65/#73–#76: sealed `snapshot.v1` objects — `snapshot.v2` since `P7-SYNC-01` (2026-07-10), which adds the terminal device-trust projection — + signed CAS retention manifest, fail-closed import/recovery, `hub compact` with tombstone GC over signed per-device sync acks, `hub migrate-events`, advisory sweep lock) — the event log is now bounded and a fresh device bootstraps from a snapshot instead of replaying all history. The Pass-6 backlog is fully closed (43/43), convergence property-testing (`P4-QUAL-02`, plus the `reconcileSamePath` HLC-monotonic follow-up, PR #95) shipped 2026-07-04, and the **zero-infrastructure hub carrier (`AD-1`) shipped 2026-07-04** — `hub: git+ssh://…` syncs through any private git repo (see `03_SYSTEM_ARCHITECTURE.md`, Hub backends), was forge-proven the same day by a live dogfood against a real private GitHub repo (`spec/19` §F.2), and is now the **documented quickstart default** (README/`init` hints; `r2://` demoted to the scale/power path). OS-enforced agent sandboxing (`P4-GIT-03`) shipped 2026-07-05 (macOS Seatbelt, Linux bubblewrap → Landlock+seccomp fallback); the remaining core-engine candidates are the later `AD-1` slices (`hub init` bootstrap, folder carrier); see `14_MVP_ROADMAP_AND_BACKLOG.md`. **Positioning:** DevStrap's durable value is as the **substrate agents run on** — cross-machine workspace consistency, fresh-base provenance (fetched `origin/<default_branch>`, recorded base SHA), and a queryable worktree/run registry — not itself an agent runner; the generic wrapper's command/file policy is guardrails, not a sandbox (`AD-5`; see `10_AGENT_WORKSPACES_AND_POLICIES.md`).

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

Last validated: `2026-07-05`.

Implemented in this repository:

- Go module: `github.com/Reederey87/DevStrap`.
- CLI entrypoint: `cmd/devstrap`.
- Commands: `version`, `init`, `up`, `join`, `pair`, `scan`, `add`, `clone`, `hydrate`, `open`, `sync --hub-file`, `hub init`, `hub compact`, `hub gc`, `hub login`, `hub logout`, `hub migrate-events`, `keys rotate`, `materialize`, `draft snapshot create`, `run-loop`, `service install`, `service uninstall`, `service status`, `worktree new`, `worktree status`, `worktree finalize`, `worktree list`, `worktree remove`, `worktree cleanup`, `worktree unlock`, `env capture`, `env hydrate`, `env bind`, `env rotate`, `run`, `agent run`, `agent list`, `agent show`, `agent pr`, `devices enroll`, `devices list`, `devices approve`, `devices revoke`, `devices lost`, `devices rename`, `devices recipient`, `devices pairing-code`, `conflicts list`, `conflicts show`, `conflicts resolve`, `status` (`--watch`), `doctor` (`--remote`), `db migrate`, `db status`, `db backup` (`--full`), `db restore`, and `db down`.
- Structured `slog` setup with CLI/env log-level control, secret-key/value redaction helpers, and no whole-context log attributes.
- Local state package with embedded Goose SQLite migrations.
- SQLite open path with per-connection pragmas, WAL, busy timeout, asserted foreign-key enforcement, startup `foreign_key_check`, `0600` database mode, and single-writer pool.
- Generated stable local `ws_<uuidv7>` workspace identity persisted during `init`, with a database-enforced singleton workspace invariant for the Phase 0 MVP; a joining device adopts the founder's id via `init --join --workspace-id <id>` (P4-SEC-07 pairing — the id is surfaced by `status` / `devices recipient --workspace-id`, validated before anything is written, and a store initialized under a different id is refused rather than rewritten).
- Event-ordering schema for HLC, sync cursors, event delivery, hashes/signatures, tombstones, and namespace source-event metadata.
- Generated stable local `dev_<uuidv7>` device identity persisted during `init`, plus age X25519 and Ed25519 signing identities whose public keys are stored in SQLite and whose private identities are kept out of SQLite/config in the OS keychain/Secret Service when available, falling back to `~/.devstrap/keys` with mode `0600` when the system keyring is unavailable.
- Namespace path normalization with NFC display normalization, case-folded `path_key`, and unsafe path rejection.
- Filesystem scan/adopt workflow with generated-folder pruning, secret-looking filename warnings, symlink escape warnings/conflicts, Git remote normalization, duplicate remote reporting, and project status rows.
- Git add/hydrate/open workflow using skeleton directories, sibling temp-dir clone staging with atomic promotion after success, partial clone by default, configurable LFS policy, metadata-backed repo operation locks, git subprocess timeouts, prompt disabling, sanitized git environment, protocol allowlist, typed Git error classification with transient network retry for clone/fetch, and URL credential redaction in git errors.
- Shared child-process environment sanitizer with explicit allowlists and non-overridable dangerous-name stripping, wired into Git subprocesses and editor launch.
- Initial `internal/platform` adapter seams for watcher, service manager, keychain, and editor launch, with build-tagged platform detection, a polling watcher fallback for unsupported platforms, fsnotify-backed watcher adapters for Darwin/Linux with debounce/coalescing, explicit unsupported service/keychain placeholders, and `open` routed through the editor adapter.
- Hardened `.env` parser plus `devstrap env capture/hydrate/bind` and `devstrap run` flows: capture refuses interpolation unless `--literal`, encrypts parsed values to the local age recipient, stores only `age_blob:<sha256>` refs in SQLite, writes `0600` ciphertext blobs, and gitignores captured env files; hydrate decrypts local encrypted blobs or resolves 1Password refs through `op inject`, writes the requested env file atomically with mode `0600`, refuses overwrites unless `--force`, and gitignores the hydrated target; bind stores 1Password `op://` refs without resolving plaintext; run injects encrypted profiles into subprocess env or delegates provider refs to `op run`.
- Fresh upstream worktree creation that holds the repo operation lock, fetches `origin/<default_branch>`, resolves base SHA from the remote ref, honors stored LFS policy with pull-or-warn behavior, records worktree metadata, exposes `worktree status` stale-base detection, and gates `worktree finalize` on the recorded base unless `--allow-stale-base` is explicit.
- Thin generic agent runner that creates a fresh worktree, runs explicit argv commands with a sanitized no-secret default environment and wrapper-level command/file path policy, captures a `0600` log, records `agent_runs`, summarizes Git status/diff, and gates `agent pr` on the recorded base before pushing and creating a forge-aware PR/MR when the relevant CLI is available (`gh`/`glab`/`tea`) or printing a compare URL for unsupported forges. Project-env allowlists (`P4-GIT-06`, `internal/agentsecrets`): a project-root `.devstrapagent.yml`, read from the agent's fresh worktree, opts `agent run` into exposing its captured `devstrap_encrypted` env profile — filtered to `agent_secrets.allow` minus `agent_secrets.deny` (deny wins) — into the child process; a project with no config file injects no captured secrets, matching the pre-existing default.
- Device trust-state CLI for listing, renaming, approving, revoking, and marking non-local devices lost, with refusal to revoke the current local device.
- In-process/file-backed sync spike with mutex-protected HLC send/receive, persisted local HLC/sequence stamping, local event signatures, signed-event verification when the source signing key is known, logical-counter overflow handling, clock-skew rejection, append-only event helpers, HLC-gated project delete tombstones, deterministic replay ordering, duplicate event idempotency, per-event quarantine for permanent verification/divergence failures, and order-independent same-path/different-remote conflict reconciliation.
- User-facing `devstrap sync --hub-file <path>` (or `hub: r2://<bucket>`, or the zero-infra `hub: git+ssh://…` private-git-repo carrier); `add` and `scan --adopt` stamp local project events, sync pushes local events, pulls hub events, applies namespace events idempotently, and then eagerly materializes the tree — blobless/partial-cloning every repo, extracting draft blobs, and hydrating env (EAGER-01/02).
- Value-level secret redaction in `internal/redact` (a `Secret` capability type plus URL/userinfo stripping, a token-shape scrubber, and a line-buffering scrubbing writer) wired into sync event payloads, CLI error output, the persisted agent log, and slog attributes.
- Scan boundary hardening: only validated remotes are persisted, escaping symlinks are typed and hard-excluded with use-time revalidation before materialization, and `scan --quarantine` isolates secret-looking files in a dated `0600` quarantine.
- Authoritative default-branch resolution for fresh worktrees (`ls-remote --symref` with `set-head --auto` repair and a non-authoritative warning), `worktree cleanup --force`, `worktree unlock <path>`, and `doctor` repo-lock reporting.
- Sync apply-path clock-skew quarantine, local-clock advance on receive, `project.renamed` handling, delete-vs-dirty conflicts, event verification-failure conflicts with full replay payloads, and tombstone GC; plus `secret_bindings.needs_rotation` flagging on device revoke/lost surfaced in `doctor`.
- A `DEVSTRAP_NO_KEYCHAIN` gate forcing the file-backed key store for headless/CI runs.
- Focused tests for every internal package (including `internal/id`, whose canonical-shape validator backs `--workspace-id`), plus a `rogpeppe/go-internal` testscript end-to-end harness exercising `cmd/devstrap` through the real binary.
- Spec frontmatter and a Go-based `cmd/spec-drift` CI gate that maps changed code/config paths to tracked spec files and requires the work log on code/spec/doc changes, plus a command-doc drift test that keeps the spec command list in sync with the binary, and a product-naming ADR at `spec/adr/0001-product-naming.md`. The gate proves only that a mapped spec was *touched*, not that its `last_reviewed` frontmatter reflects the change; `AGENTS.md` (PR-cycle step 1) makes bumping `last_reviewed` on a substantive spec edit — and deliberately *not* on a cross-reference/typo touch — the author's obligation (`P7-DOC-03`).
- README, MIT license, `.gitignore`, GitHub Actions CI with separate spec-drift, test, and golangci-lint jobs, `CONTRIBUTING.md`, `SECURITY.md`, `CODEOWNERS`, Dependabot, issue/PR templates, and concise `AGENTS.md`. The README's Install section now includes a "Verify a download" subsection documenting the release pipeline's cosign keyless signature + per-archive SBOM (`P4-SEC-05`/`P4-QUAL-05`; see `03_SYSTEM_ARCHITECTURE.md` Distribution).

Not implemented yet (genuinely unbuilt — features that are partly shipped are listed under "now built" below, never here):

- the bespoke **HTTP/SSE relay** (full-state snapshot exchange is now SHIPPED — sealed snapshots, signed retention manifest, `hub compact`, fail-closed import; the live R2/S3 backend is shipped: the `aws-sdk-go-v2` S3 adapter is wired behind the `hubFromOptions` `r2://` seam, with the `Hub` interface, R2 keying/retry/conditional-put logic, blob GC, retention floor, and content-hash verification all shipped and unit-tested, and the same conformance contract proven against MinIO via an env-gated integration test);
- daemon, local socket API, FSEvents-specific Mac watcher (the LaunchAgent/systemd installers are now shipped as `devstrap service`, wrapping `run-loop`);
- non-generic engine adapters (`cursor-cli`/`codex-cli`/`copilot-cli`) — `10_AGENT_WORKSPACES_AND_POLICIES.md` "Direction: DevStrap as the substrate agents run on (AD-5)" argues for a `worktree new --fresh-upstream --json` provisioning primitive plus a harness hook/MCP integration over growing more per-harness wrapper adapters, so this stays deliberately unbuilt rather than a near-term gap;
- cross-machine working-state sync — git-state validation plane (`repo.gitstate.observed`) and WIP refs (`refs/devstrap/wip/*`); the encrypted draft-bundle layer (Layer C) is shipped;
- forge hardening beyond the shipped PR/MR routing — `agent pr` detects GitHub/GitLab/Gitea/Bitbucket/Azure and routes through `gh`/`glab`/`tea` (or a compare-URL fallback) and resolves SSH host aliases via `ssh -G` (`P5-CLI-04`); `doctor` now probes the matching forge CLI per adopted remote (`FORGE-04`, shipped — `checkForgeCLIs` in `internal/cli/doctor.go`), leaving broader hermetic test coverage (`FORGE-05`) as the remaining gap.

Cloud-sync workstreams from the 2026-06-28 audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`), now built:

- eager-clone materialization (`EAGER-*`) — `devstrap sync` reconstructs the whole `~/Code` tree by blobless/partial-cloning every repo from its existing remote up front with bounded concurrency and per-project failure isolation; env profiles hydrate; `node_modules`/build artifacts are rebuilt on hydrate (opt-in), never synced;
- non-git/draft content sync (`DRAFT-*`) — a `.devstrapignore` compiler (`internal/ignore`) and age-encrypted, content-addressed `age_blob:<sha256>` bundles for non-git/draft folders pushed/pulled through the blob plane (`draft snapshot create`, `draft.snapshot.created` event);
- cloud hub backend (`HUB-*`) — the two-plane zero-knowledge `Hub` interface (event log + content-addressed encrypted blob store) with the Cloudflare R2/S3 backend (`internal/hub`, the `aws-sdk-go-v2` S3 adapter wired behind `hub: r2://<bucket>`), the zero-infrastructure private-git-repo carrier (`GitCarrierHub` behind `hub: git+ssh://…`, `AD-1` first slice), and the file-backed backend retained for tests; the event log is envelope-encrypted at the hub boundary (`EncryptedHub`, XChaCha20-Poly1305 under a per-epoch Workspace Content Key, `P4-SEC-02`/`SEC-07`), and grant-carrier events are verified before their WCK is ingested once the store has an enrolled device (`P6-SEC-01(a)`);
- cross-platform hardening (`XP-*`) — portable `devstrap run-loop` (scan → sync → materialize on an interval, no daemon; the per-tick scan+adopt stage is idempotent and shipped, `P6-XP-03`); e2e testscript proving two-device materialization; headless key custody test; NFC/case-fold path invariant test;
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
devstrap open work/acme/api-server --cursor
devstrap worktree new work/acme/api-server --fresh-upstream --name route-tests
devstrap env capture work/acme/api-server .env
devstrap env hydrate work/acme/api-server --write .env.local
devstrap sync   # shipped: pushes/pulls signed, envelope-encrypted events (hub: git+ssh://… /
                # git@host:path.git — any private git repo as the zero-infra carrier, the documented
                # default — or hub: r2://<bucket> for scale; --hub-file for tests),
                # then eagerly blobless-clones every repo, extracts draft blobs, and hydrates env (EAGER-01/02).
```

The first killer loop (eager-clone Workspace Passport):

```text
1. Add or create a project on Machine A.
2. DevStrap records it in the signed HLC namespace map (path, remote, env profile, policy),
   and pushes any env/non-git/draft content as age-encrypted blobs to the hub. Captured
   profiles emit `env.profile.updated`; draft bundles emit `draft.snapshot.created`.
3. Machine B runs `devstrap sync` and pulls the updated namespace map.
4. Sync eagerly materializes the whole tree: every repo is blobless/partial-cloned from its
   existing remote, env and draft blobs are pulled from the encrypted blob plane, and env profiles hydrate.
   (.git is never file-synced; node_modules/build artifacts are rebuilt, not synced.)
5. The same folder paths are really present on disk — no skeleton to "open" first.
6. Agent work starts from a fresh remote default branch, not a stale local default branch.
```

The branch workflow that backs this invariant — trunk-based development on a single protected `main`, all changes via pull request, and the rule that agents and worktrees always base from the fetched `origin/main`, never any local branch — is defined canonically in `AGENTS.md`. Refer to it there rather than restating the model here. (`AGENTS.md` is the *maintainer's* agent workflow, per the AD-8 reframe — external contributors follow `CONTRIBUTING.md`, and the maintainer completes the spec-drift/work-log bookkeeping at merge for fork PRs.)

## Document map

The user-facing tier lives outside `spec/` (`AD-8`, shipped 2026-07-05):

- `../ARCHITECTURE.md` (repo root) — the human "explanation" tier bridging the README and this
  corpus: the managed-physical-namespace decision, the Workspace Passport promise, the two-plane
  zero-knowledge hub and its carriers, compaction/snapshot bootstrap, device trust, agent
  workspaces, and what is deliberately not built. Each section links back here for depth.
- `../docs/install.md`, `../docs/quickstart.md`, `../docs/self-hosting.md` — task-oriented user
  guides (all install paths; the first-run loop + pairing + agent loop; choosing and operating a
  hub). `../docs/audits/` holds the audit archive and open backlog.

The design corpus:

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
- `19_CLOUD_PROVISIONING_GUIDE.md` — register/configure the chosen cloud stack (Fly.io + Cloudflare R2 + Neon). Live-R2 dogfood credentials are kept in a stable `0600` env file (`~/.devstrap/dogfood-r2.env`) that agents `source` for any live-R2 run — see `AGENTS.md` § *Live-R2 dogfood credentials*.
- `20_COMMERCIALIZATION_AND_PRICING.md` — the plan (not a shipped feature) for a managed-hub commercial tier alongside the free OSS BYO-hub product: open-core boundary, comparable-product pricing research, R2 cost model, packaging, and the engineering prerequisites (control plane, credential broker, quotas).
- `21_WEBSITE_PLAN.md` — the marketing + docs site: goals, information architecture, tech-stack recommendation (Astro + Starlight on Cloudflare Pages), design direction, hosting, domain, and phased launch.
