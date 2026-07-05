---
last_reviewed: 2026-07-01
tracks_code: [cmd/**, internal/**, .github/**, docs/audits/AUDIT_RECOMMENDATIONS.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md, docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md]
---
# MVP Roadmap and Backlog

## MVP definition

The MVP is successful when one user can register multiple Macs/Linux machines, keep a consistent `~/Code` structure, materialize repos (eager clone-everything on `sync` — `EAGER-*`, shipped — with on-demand `hydrate` retained for single repos), hydrate env safely, and create fresh worktrees for agents.

The "Dropbox experience for code" target — one identical `~/Code` tree that appears automatically across the owner's fleet — is delivered by the 2026-06-28 cloud-sync architecture: content split by type (repo content rides git's own blobless clone/fetch from its existing remote and never transits the hub; env + non-git/draft folders ride age-encrypted content-addressed `age_blob:<sha256>` blobs; the project map rides the signed HLC-ordered event log), eager materialization on `sync`, and a two-plane zero-knowledge hub on Cloudflare R2. See `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`.

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

The next cycle is re-sequenced around the **eager-clone core + cloud backend**, not a native daemon. IDs reference `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`. This overlay supersedes the priority of the historic M5–M7 above without renumbering them:

```text
Next:      eager-clone materialization (EAGER-*) — clone-everything (blobless) on `devstrap sync`;   [shipped 2026-06-29]
           the whole ~/Code tree is present after sync; no FUSE/placeholder/lazy-VFS
Then:      non-git/draft content sync + .devstrapignore compiler + encrypted bundles (DRAFT-*)         [shipped 2026-06-29]
Then:      cloud zero-knowledge hub on Cloudflare R2, two planes (event log + blob store) (HUB-*)      [shipped 2026-06-30]
Alongside: cross-platform core — portable Go on macOS + Ubuntu, no native daemon (XP-*)               [shipped]

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

Current implementation has the core HLC, persisted local event stamping with per-device sequence numbers, project event helpers, `add`/`scan --adopt` project-event emission, transactional idempotent insert/apply, HLC-gated project delete tombstones/restores, content-hash duplicate validation, order-independent same-path/different-remote conflict reconciliation, a file-backed hub adapter for tests, and `devstrap sync --hub-file` for file-backed namespace event push/pull. Automatic remote device registration remains future work; tombstone GC and cross-root materialization have since shipped (see Milestone 7 status and the `EAGER-*` workstream).

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

Entry gate (review before starting M7): require the logical Hub interface and file-backed conformance suite first, then implement the R2/S3 direct backend. Hidden-manifest Git and home/VPS hub approaches are historical alternatives, not the current target. The bespoke HTTP/SSE `cmd/devstraphub` relay remains deferred until live push or multi-tenant routing requires a service.

Dependency gate: encrypted blob upload/download must not ship until Milestone 4 has device key identities plus per-device env-decryption approval. The local trust plane now supports manual device enrollment/approval for capture recipients; production blob sync still requires automatic remote enrollment and fingerprint confirmation.

Deliverables:

- `internal/hub` logical interface with event + blob planes and a file-backed conformance backend — **shipped**;
- cursor-based event push/pull with snapshot-required recovery — **shipped**;
- Cloudflare R2/S3 direct backend with immutable event objects, conditional puts, paged cursor pulls, and content-addressed encrypted blob upload/download — **shipped** (`P5-HUB-01`: the `aws-sdk-go-v2` `S3Adapter` behind `hub: r2://<bucket>`);
- event payload validation before apply, fail-closed verification after enrollment, and device heartbeat/trust metadata.
- envelope encryption of the event log at the hub boundary (`EncryptedHub`, XChaCha20-Poly1305 under a per-epoch WCK, `P4-SEC-02`/`SEC-07` foundation) — **shipped** (`fix/p4-sec-02-envelope-encryption`): `init` bootstraps epoch 1, `devices approve` grants all epochs, `devices revoke`/`lost` rotates, `Pull` verifies grant carriers before WCK ingest (`P6-SEC-01(a)`), ingests verified grants in HLC order, then decrypts; the hub stores only `enc.v1` ciphertext carriers.

Tasks:

```text
[x] Extract Hub interface and run the same conformance tests against FileHub and an S3/R2-compatible backend
[x] Define R2 object keys: workspaces/<ws>/events/<hlc-padded>/<device>/<seq>/<event>.json and workspaces/<ws>/blobs/<sha256>
[x] Implement conditional event PUT, ListObjectsV2 pagination, limit/next_cursor pulls, and snapshot-required recovery
[x] Implement encrypted blob PUT/GET/HEAD and local blob ref-counting
[x] Validate incoming project payloads before apply (e.g. git_repo remote_url/remote_key are non-empty and validated)
[~] Implement remote device registration/fingerprint confirmation and fail-closed event verification for enrolled workspaces
[x] Implement namespace + blob sync across two machines with no .git or plaintext secret bytes in the hub
```

Status (2026-06-30): the `Hub` interface, `FileHub`/`memS3` conformance, R2/S3 keying, conditional-put/pagination/snapshot recovery, encrypted blob PUT/GET/HEAD, payload validation, fail-closed verification (`HUB-03`), and two-machine namespace+blob sync are shipped. The live R2/S3 production adapter (`aws-sdk-go-v2` behind `hubFromOptions` `r2://`) shipped in `P5-HUB-01` (branch `fix/p5-hub-01`), with an env-gated MinIO conformance test. The one partial item is automatic **remote** device registration / out-of-band fingerprint confirmation (the local trust plane, manual enrollment, and device-revoke rewrap are shipped; see `spec/00_START_HERE.md` "Not implemented yet").

Acceptance:

```text
Add project on Mac A.
One `devstrap sync` on Linux B materializes the repo through blobless clone from its git remote.
Env/draft blobs hydrate from encrypted age_blob:<sha256> content.
Status shows both devices.
```

## Backlog: V1

```text
[ ] TUI dashboard
[ ] 1Password adapter beyond op run/op inject provider refs (item browsing, write-back)
[ ] Doppler adapter
[ ] Infisical adapter
[ ] DevPod adapter
[ ] Coder adapter
[ ] GitHub App integration
[x] Git LFS policy support
[ ] sparse checkout profiles
[x] draft project encrypted sync (DRAFT-*, shipped 2026-06-29)
[ ] conflict resolution UI
[ ] shell cd hydration hook
[ ] zsh/fish/bash integrations
[x] Homebrew tap + curl|sh installer + completions (v0.1.0 SHIPPED 2026-07-05 — `brew install Reederey87/devstrap/devstrap` live)
[~] code signing/notarization (cosign keyless signing + SBOMs + SLSA provenance SHIPPED and LIVE-VERIFIED on v0.1.1, 2026-07-05 — `P4-QUAL-05` closed; remaining: Apple Developer ID + notarization, dormant config ready per `RELEASING.md`, Homebrew Gatekeeper deadline 2026-09-01)
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
[~] containerized agent sandbox (macOS Seatbelt + Linux bubblewrap + Landlock fallback + Linux seccomp denylist + unsigned local `sandbox.violation` telemetry + tighter read confinement (`--read-confine`) shipped 2026-07-05, P4-GIT-03; only containerization still open)
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
- Releases are automated by GoReleaser (`.goreleaser.yaml`, `.github/workflows/release.yml`), triggered on `v*` tags: a read-only verify job first confirms the tagged commit is on `main` or `release/*` and passes `go vet`, race tests, and pinned `govulncheck`; the gated publisher then cross-compiles macOS + Linux (amd64/arm64) binaries plus `checksums.txt` and publishes a GitHub Release with `version`/`commit`/`date` injected via `-ldflags`, packages bash/zsh/fish completions into every tarball, and — on stable tags only (`skip_upload: auto`) — publishes a Homebrew **cask** to `Reederey87/homebrew-devstrap` (quarantine-strip post-install hook until signing/notarization lands under `P4-SEC-05`/`P4-QUAL-05`). A checksum-verifying `curl|sh` installer lives at `scripts/install.sh`; `.goreleaser.yaml`/`scripts/**` are spec-tracked and work-log-gated. See `RELEASING.md` and `03_SYSTEM_ARCHITECTURE.md` (Distribution).
- Pre-release testing uses **release-candidate tags** (`vX.Y.Z-rc.N`, auto-published as GitHub pre-releases): validate the candidate binaries, then promote to a stable tag. A `release/vX.Y` branch is used only for stabilization or back-ports; tags are cut from `main` (or such a branch), never a feature branch.

## Observability and privacy gates

- DevStrap sends no telemetry by default.
- Any future telemetry is opt-in per device and disabled by `DEVSTRAP_TELEMETRY=off`.
- Daemon logs use `log/slog` with one redaction choke point.
- Logs rotate under `~/.devstrap/logs/` and never include plaintext secrets.

## Audit follow-ups (second pass, 2026-06-27)

Workstreams added by the second-pass design & implementation audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md`). Ordered by leverage; IDs reference that document.

### P0 — security-relevant rebaseline
- **Agent wrapper policy is still not the sandbox** (`AGEN-01`, `AGEN-03`): argv-substring policy is bypassable by any interpreter. Credential env stripping (`AGEN-02`/`SECU-02`) is shipped, and macOS Seatbelt + Linux bubblewrap + Linux Landlock fallback + a Linux seccomp syscall denylist now provide default-on OS enforcement where available; unsigned local `sandbox.violation` telemetry is shipped for backend/mode/limitations plus macOS Seatbelt denials (Linux runtime denial detection remains future), and tighter read confinement (`--read-confine`, default-on for readonly) restricts reads to the toolchain/worktree allow-list on all three backends. Remaining hardening is project/env allowlists plus the containerization follow-up.
- **Secret hydration unsafe writer** (`SECR-01/02/05`) is shipped: safe quoting, generated header, `0600` atomic write, and ignore-before-write are implemented. Remaining work is routing ignore updates through the planned `.devstrapignore` compiler.
- **Key custody silent downgrade** (`SECR-04`/`SECU-01`) is shipped for present-but-failing keychains; Linux Secret Service/headless integration coverage remains under `XP-03`.
- **Forge-aware `agent pr`** (`FORGE-01/02/03`) is shipped for GitHub/GitLab/Gitea/Azure key folding and graceful fallback. Remaining work is `doctor` probes, self-hosted overrides, native Bitbucket/Azure clients where useful, and broader fake-CLI tests (`FORGE-04/05`).
- **No-remote repo corruption** (`NOVCS-01`) is shipped: scanner classifies no-remote/unvalidated remotes as `local_git`; remaining non-git work is `plain_folder`, `promote`, and draft bundle materialization (`NOVCS-02..05`, `DRAFT-*`).
- **CI fragility** (`CI-01`) is shipped: `govulncheck` is pinned/split.

### Cross-machine working-state sync (the "forgot to push" feature)
- **Layer A — git-state validation plane** (Phase 0): `repo.gitstate.observed` events + `device_gitstate` sidecar table + `status --all-devices`/`doctor`.
- **Layer B — WIP refs** (Phase 1): `git stash create` → `refs/devstrap/wip/*` push + `wip` subcommands; resolver-exclusion test.
- **Layer C — encrypted bundles** (Phase 3): build out `draft.snapshot.created` for non-git/draft folders.

### Non-VCS / forge support
- Non-VCS/remote-less/multi-remote handling (`NOVCS-02..05`): `plain_folder` emission, `promote` command, remote preflight.
- Forge-agnostic PR via a `Forge` interface (`FORGE-02..05`): `gh`/`glab`/`tea`, token allowlist, Azure key folding, `doctor` probe.

### Sync hub (Phase 2)
- Logical Hub interface + file-backed conformance backend + R2/S3 direct production backend first; HTTP/SSE `cmd/devstraphub` relay and mTLS device certs are deferred until live push or multi-tenant routing needs a service.
- The resume cursor is shipped (per-origin-device Seq cursors, `P5-SYNC-01`), and full-state snapshot exchange + retention GC are shipped (2026-07-04: `hub compact`, sealed snapshots + signed CAS retention manifest, fail-closed import/recovery, tombstone GC over signed sync acks, `hub migrate-events`, advisory sweep lock).

### Architecture & hygiene epics
- Extract `internal/engine` from `internal/cli` (`ARCH2-01`) before the daemon phase.
- Signed **audit-log subsystem** (`spec/15`) — currently absent.
- **`.devstrapignore` compiler** (`spec/11`) — currently absent; root cause of duplicated prune/secret/deny lists.
- Daemon crash-recovery/reaper, observability/log-rotation, large-namespace scan benchmarks, cross-process `state.db` coordination, migration-rollback tests (audit coverage gaps).

## Audit follow-ups (cloud-sync pass, 2026-06-28)

Workstreams added by the cloud-sync architecture pass (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`). These **extend** the 2026-06-27 second-pass audit above — they do not revert it. The product goal is the "Dropbox experience for code": one identical `~/Code` tree that appears automatically across the owner's fleet. The core rule is **file-sync split by content type — never blanket file-sync, never file-sync `.git`** (it corrupts the repo). New planned commands/flags are **future**, not yet shipped.

### EAGER-* — eager-clone materialization
- Make `devstrap sync` perform eager **clone-everything** up front via blobless/partial clone (`git clone --filter=blob:none`) of every namespaced repo from its existing remote; the whole `~/Code` tree is present after sync.
- Repo content rides git's own transport and **never** transits the DevStrap hub. No FUSE/placeholder/lazy-VFS — StrapFS stays explicitly deferred.
- `node_modules`/build artifacts are never synced; rebuild on hydrate (`npm`/`pnpm`/`uv install`).

### DRAFT-* — non-git / draft content sync
- Sync env vars and non-git/draft folders as **age-encrypted, content-addressed `age_blob:<sha256>` blobs** (the human/draft plane), never as repo content and never byte-merged.
- Build the **`.devstrapignore` compiler** (shared with `spec/11`) so bundle contents exclude generated/secret/junk paths deterministically.
- Encrypted working-tree/draft bundles (`draft.snapshot.created`) build on the 2026-06-27 Layer C; conflicts use detect-don't-merge with dual-copy as the safe default (no byte-merge of opaque files; CRDTs solve a different problem).

### HUB-* — cloud zero-knowledge hub
- Ship the **logical** two-plane zero-knowledge Hub first: (a) event log = the namespace map; (b) content-addressed encrypted blob store = env + non-git/draft content. Implementations are file-backed tests, direct R2/S3 production, and later HTTP/SSE relay if needed.
- **Cloudflare R2 backend from the start** (S3 API, zero egress, namespaced by `workspace_id`; client-side age encryption). **No NAS-first phase.** Keep the backend pluggable behind one `Hub` interface; retain a file-backed local backend **only for tests**.
- R2 event log must use immutable unique object keys, conditional puts, cursor pagination, snapshots, and cost-aware polling/backoff. Never append by overwriting one manifest object.
- SaaS/runners require temporary prefix-scoped R2 credentials or presigned URLs; bucket-wide long-lived keys are acceptable only for single-owner self-hosted mode.
- The resume cursor is shipped (per-origin-device Seq cursors, `P5-SYNC-01`), and full-state snapshot exchange + retention GC are shipped (2026-07-04: `hub compact`, sealed snapshots + signed CAS retention manifest, fail-closed import/recovery, tombstone GC over signed sync acks, `hub migrate-events`, advisory sweep lock). HTTP/SSE mTLS relay remains deferred.
- **Device trust must fail closed** once enrollment exists (today `SECU-03` fails open for non-destructive event types). Revoke ⇒ re-encrypt affected blobs to the reduced recipient set + flag secrets for rotation (age has no native revocation).

### XP-* — cross-platform core first
- Ship a **portable Go core on macOS + Ubuntu** with OS-specific magic deferred: no native daemon, no StrapFS this cycle. Eager-clone on `sync` plus periodic reconciliation cover the loop without a resident watcher.
- Validated on both macOS and Ubuntu targets from the single Go binary.

### SCALE-* — multi-user / multi-tenant scaling (future direction, documented not built)
- **Chosen stack remains sound:** Fly.io for Go-native compute and per-task runner Machines + Cloudflare R2 for the encrypted sync data plane + Neon managed Postgres as the low-idle control-plane DB default. R2 gives low storage cost and free egress; Neon gives scale-to-zero/branching; Fly runs the Go binary and isolates runner tasks with VMs. No immediate provider switch is recommended.
- **Provider constraints:** use current Fly region/pricing docs rather than fixed region counts or "near-zero" claims; runners must be separated from the control-plane app, receive only per-task scoped credentials, and be destroyed after untrusted tasks. Neon needs two DSNs when hosted: pooled runtime and direct migration/admin. R2 gives confidentiality by encryption, but integrity/availability still need scoped credentials, signatures, snapshots, backups, and budget alerts.
- **Alternatives:** Tigris is a credible Fly-native S3 alternative when global data placement/one-vendor integration outweighs R2's lower cost/free-tier advantage. Cloudflare Workers/Durable Objects/D1 + R2 is a credible serverless HTTP/SSE/control-edge alternative if the project later accepts a non-Go edge runtime. Supabase is attractive when Auth/Storage/BaaS are needed; Render/Railway are simpler app-hosting options but do not replace microVM runner isolation for untrusted multi-tenant code; Hetzner remains a cheap solo/self-host option, not the hosted default.
- **Scaling model:** control/data-plane split, tenancy spectrum (pooled → dedicated/BYOC), cell-based scaling, data-residency placement across Fly/Neon/R2 jurisdiction choices; Coder is the reference architecture for agents-on-your-infra at scale.

### Deferred (2026-06-28)
- **StrapFS / FUSE / macFUSE / FSKit / Apple File Provider** — no lazy virtual filesystem in this design; remains Backlog V2.
- **Mac daemon + native FSEvents watcher and Linux native systemd/inotify adapters** — portable Go core first (`XP-*`); native service work stays behind the Milestone 5/6 entry gates.
- **Multi-user / multi-tenant build-out (`SCALE-*`)** — documented direction only; nothing built this cycle.
- Out of scope for these docs: which LLM/agent API the runner uses (a separate concern, deliberately not specified here).

### Pass 6 backlog (2026-07-01)

The sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`; index in `docs/audits/README.md`) verified the shipped system against trunk `8c739b8`. Five **P1 must-fix** findings form the near-term wave; land them before broadening the product surface further:

- **P6-SEC-01** — Verify grant-carrier signatures and refuse WCK overwrite before writing to the keychain. Status: **shipped** — pre-ingest verification (`P6-SEC-01(a)`, PR #31) plus `(epoch, kid)`-addressed custody with overwrite refusal and grant-preferred key selection (`P6-SEC-01(b/c)`, PR #33/#34).
- **P6-SYNC-01** — Quarantine verification/trust failures per-event instead of aborting the whole pull batch. Status: apply-path quarantine and approval replay are shipped; synced `device.revoked` trust propagation remains open.
- **P6-HUB-01** — Make `hub gc` sync-first and grace-windowed; refuse to sweep on a truncated mark set. Status: **shipped 2026-07-02** (`fix/p6-hub-01` — pre-GC pull+apply, refuse-to-sweep on truncated/skipped/quarantined signals, `--grace-window` default 24h; pinned by the `hub_gc_stale_marks` e2e script).
- **P6-GIT-01** — Split the git subprocess timeout by command class and stop classifying self-imposed deadline kills as retryable. Status: **shipped 2026-07-02** (`fix/p6-git-01` — `Runner.LongTimeout` 30m per-attempt for clone/fetch/LFS via `materialization.clone_timeout`; terminal `ErrTimeout` ends the wipe-and-retry). **This completes all five Pass 6 P1s.**
- **P6-DATA-01** — Record the origin device's own `draft_snapshots` row at snapshot-create time, in one transaction with the event. Status: **shipped 2026-07-02** (`fix/p6-data-01` — `Store.InsertLocalEventTx` + `tx.RecordDraftSnapshotTx` in one `WithTx` on both the create and revoke-rewrap paths; pinned by the `draft_snapshot_gc_retains_origin` e2e script).

P6-DOC-02's audit-ledger reconciliation was applied this cycle (the `docs/audits/` status ledger and cross-references were brought back in line with shipped state).

The P2 quick-win wave is now underway: **P6-SYNC-03** (sticky fail-closed enrollment — `hasEnrolledDevices` counts `trust_state IN ('approved','revoked','lost')`, so revoking the last approved device no longer reopens the bootstrap window) is **shipped**; `P6-DATA-02`, `P6-GIT-05`, and `P6-CLI-02` ride the same wave.

### Pass 6 direction (2026-07-01) — forward-looking, not yet built

These are validated forward-direction decisions from the sixth-pass viability review. They are recorded here as roadmap direction and backlog intent, not as shipped work.

- **DIRECTION — hardening freeze before new planes (AD-2): COMPLETE 2026-07-03.** All four named sync/crypto criticals are shipped — the confidentiality/keychain gap (`P6-SEC-01`, PRs #31/#33/#34), the whole-batch pull wedge (`P6-SYNC-01`, PR #30), the live-blob-loss GC (`P6-HUB-01`, PR #36), and the dropped-cursor path (`P5-SYNC-01`, PR #59 — per-origin-device Seq transport cursors). New capability planes (HTTP/SSE relay, native daemon, StrapFS, hosted SaaS) are unblocked from the freeze's perspective; sequence them against the remaining Pass-6 P2/P3 backlog (6 open) and the Pass-4 XL items. **The compaction/snapshot-exchange item (`P4-SYNC-02`/`P4-HUB-11`) SHIPPED 2026-07-04** (PRs #65/#73–#76) — it was also the designed recovery path for the byzantine-hub residuals in `15_SECURITY_THREAT_MODEL.md`, which is now real. Remaining XL items: OS-enforced agent sandbox (`P4-GIT-03`) — **macOS Seatbelt, Linux bubblewrap, Linux Landlock fallback, Linux seccomp syscall denylist, unsigned local `sandbox.violation` telemetry, and tighter read confinement SHIPPED 2026-07-05** (`agent run`, `--sandbox auto|off|require` default-on where available; `DEVSTRAP_SANDBOX_SECCOMP=off` opts out of the denylist; `--read-confine` default-on for readonly; the named remaining trio is complete — only the containerization direction stays open) — property/model-checked convergence (`P4-QUAL-02`) shipped 2026-07-04 (PR #94, with the reconcileSamePath HLC-monotonic follow-up in PR #95), and the zero-infrastructure hub carrier's first slice (`AD-1`, the private-git-repo backend) shipped 2026-07-04.
- **AD-1 — zero-infrastructure Hub backend for first-run adoption: COMPLETE 2026-07-05.** Both carriers are live behind the pluggable `Hub` interface. The private-git-repo carrier: `hub: git+ssh://…` (also `git+https://`, `git+file://`, scp-like, `?branch=`) syncs through any private git repo the user can already push to — `GitCarrierHub` composes the proven `R2Hub` semantics over a filesystem `S3Client` in a local clone, with a fetch/commit/push write loop (push-ref CAS replaces conditional PUT; non-fast-forward retries re-apply idempotent, path-disjoint mutations) and a `--force-with-lease` orphan-squash on `hub compact` to bound carrier history. The local-folder / cloud-drive-folder carrier: `hub: folder:<abs-path>` roots the same `fsObjectStore` directly in a shared directory (a cloud-drive folder or network mount is the replication transport, so there is no push loop — each op is lock → delegate → unlock), with the cross-process lock and observation floor kept in the LOCAL home cache (never replicated) and only ciphertext objects in the shared folder; `FolderHub` reuses the extracted `fsLock` cross-process lock helper. See `03_SYSTEM_ARCHITECTURE.md` (Hub backends) for the full design. Backlog rows: `[x] zero-infrastructure Hub backend — private-git-repo carrier (AD-1, 2026-07-04)`, `[x] live dogfood against a real private GitHub repo (spec/19 §F.2, 2026-07-04)`, `[x] git carrier as the documented quickstart default — r2:// demoted to scale/power (README/init hints/spec-13/spec-19, 2026-07-04)`; `[x] devstrap hub init <git-url> bootstrap convenience (2026-07-04)`; `[x] local-folder / cloud-drive-folder carrier (AD-1 complete, 2026-07-05)`.
- **DIRECTION — distribution + OSS onboarding workstream (AD-8).** Adoption is bottlenecked on distribution and contributor friction, not features. Planned goals: cut `v0.1.0` through the existing GoReleaser pipeline (**v0.1.0 SHIPPED 2026-07-05** — tap/cask + `curl|sh` installer + completions live; the pipeline's same-commit rc → stable promotion was hardened live via the `GORELEASER_CURRENT_TAG` pin, PR #108); make the spec-drift/work-log gate **advisory on fork PRs** (**SHIPPED 2026-07-05** — `cmd/spec-drift --advisory` downgrades findings to `::warning::` annotations and exits 0; the CI workflow flips it on only when `github.event.pull_request.head.repo.full_name != github.repository`, so same-repo PRs and pushes stay blocking; `CONTRIBUTING.md` documents the fast path — the maintainer completes the bookkeeping at merge); extract a user-facing `docs/` tier (install / quickstart / self-hosting) distinct from the `spec/` design corpus (**SHIPPED 2026-07-05** — `docs/install.md`, `docs/quickstart.md`, `docs/self-hosting.md`); write a short human `ARCHITECTURE.md` (**SHIPPED 2026-07-05** — repo-root explanation tier with per-section depth pointers into `spec/`; the README's Install/Quickstart/Architecture sections now link the new tier and a Documentation pointer block); enable GitHub Discussions + good-first-issue labels (**SHIPPED 2026-07-05** — Discussions enabled, issue-template contact link, three curated starter issues #111–#113 labeled from the open backlog: `P5-CLI-03`, `P5-CLI-01` render rollout, `P4-QUAL-07` contextcheck); reframe `AGENTS.md` as the maintainer's agent workflow rather than a contributor obligation (**SHIPPED 2026-07-05** — scope banner at the top of `AGENTS.md`); and plan for bus-factor by recruiting a second write-access maintainer.
- **DIRECTION — multi-device completeness wave (2026-07-05).** With Pass 6 closed (43/43), the AD-2 freeze complete, and AD-1/AD-8 shipped, the next wave closes the two remaining genuinely-unbuilt gaps in the Phase-2 multi-device story (both still "Not implemented yet" in `00_START_HERE.md`): [ ] `ENV-SYNC-01` — synced encrypted env-bundle exchange: a new `env.profile.updated` event carries the captured profile's `age_blob:<sha256>` ref + binding metadata over the existing blob plane (capture already encrypts to every approved device's age recipient via `envRecipients`), with LWW apply by source-event coordinate, `mustVerifyEvent` inclusion, revoke-rewrap promotion to hub-carried semantics, and a snapshot-plane env pointer so profiles survive compaction; `env hydrate`/`materialize` on the receiving device then work unchanged. [ ] `TRUST-01` — synced device-trust propagation: `device.revoked`/`device.lost` events (signed, `mustVerifyEvent`) so one device's revoke reaches the whole fleet; sticky/monotonic apply (only `pending`/`approved` transition; a local fingerprint ceremony is the only resurrection path); `device.approved` deliberately NOT propagated — approval stays the local P4-SEC-04 ceremony; receiving devices flag `secret_bindings.needs_rotation`.
