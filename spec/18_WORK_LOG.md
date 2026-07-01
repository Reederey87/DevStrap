---
last_reviewed: 2026-06-30
tracks_code: [**]
---
# Work Log

## Purpose

This file records concise end-of-cycle summaries for agent work that modifies the DevStrap codebase.

Each entry should be short and factual so future agents can quickly understand what changed, how it was validated, and what remains.

## Entry Format

```text
## YYYY-MM-DD — <short title>

Changed:
- <code/spec/docs changes>

Validated:
- <commands or checks run>

Follow-ups:
- <remaining work, or "None">
```

Entries are newest-first: each code-modifying cycle prepends ONE dated entry at the top.

## 2026-06-30 — Envelope-encrypt the namespace-map event log (P4-SEC-02 + P4-SEC-07 foundation)

Changed:
- **Envelope crypto** (`internal/sync/eventcrypt.go`): `EncryptEvent`/`DecryptEvent` seal the content tuple (Type/PayloadJSON/ContentHash/PrevEventHash) under XChaCha20-Poly1305 (`chacha20poly1305.NewX`, 24-byte random nonce) with AAD = event.ID||uint64(epoch); the carrier (ID/DeviceID/Seq/HLC/DeviceSig) stays plaintext so hub ordering/dedup/signature verification are unchanged. `enc.v1` sentinel + `encryptedEnvelope{Version,Epoch,CT}`. `NewWCK` mints a 32-byte key. Typed errors: `ErrMissingWorkspaceKey`/`ErrUnknownEnvelopeVersion`/`ErrPlaintextEventFromHub`.
- **Migration** (`internal/state/migrations/00013_workspace_keys.sql`): `workspace_keys(workspace_id,epoch,created_at)` + `workspace_key_grants(workspace_id,epoch,recipient,source_event_id,source_event_hlc,source_event_device_id,created_at)` — epoch metadata + grant audit (the wrapped WCK rides the event payload, never SQLite). Schema version 12→13.
- **Store accessors** (`internal/state/store.go`): `CurrentKeyEpoch`/`RecordKeyEpoch`/`HeldKeyEpochs`/`RecordKeyGrant`/`RecordKeyGrantTx`.
- **WCK custody** (`internal/devicekeys/devicekeys.go`): `HybridStore.StoreWCK`/`LoadWCK` (keychain-preferred, 0600 file fallback) keyed `wck.<workspace_id>.<epoch>`; `FileStore.WriteWCK`/`ReadWCK` (base64, 0600).
- **Keyring** (`internal/workspacekeys/keyring.go`): concrete `Keyring` implementing `dssync.WorkspaceKeyring` — `EnsureBootstrap` (mints epoch 1), `GrantAllEpochs` (wraps every held epoch's WCK to a recipient, emits `device.key.granted` events), `Rotate` (mints epoch+1, wraps to remaining `ApprovedRecipients`), `IngestGrant` (age-unwrap, store WCK, record epoch), `Prime`/`WCK`/`CurrentEpoch`. age wrap/unwrap via `filippo.io/age` X25519.
- **Grant event** (`internal/sync/events.go`): `EventDeviceKeyGranted="device.key.granted"` const + `DeviceKeyGrant{Epoch,Recipient,WrappedKey}` struct + `NewDeviceKeyGrantEvent` + `applyEventTx` case (records grant audit; NOT in `mustVerifyEvent` so the bootstrap chicken-and-egg works).
- **Decorator** (`internal/sync/encryptedhub.go`): `EncryptedHub{Hub,Keyring}` — Push encrypts non-grants under the current epoch (grants passthrough), Pull primes/ingests grants in HLC order then decrypts enc.v1 (anti-downgrade on plaintext, missing-epoch→`ErrMissingWorkspaceKey`), blob ops passthrough. `WorkspaceKeyring` interface defined here so internal/sync has no keychain/platform deps.
- **Wiring** (`internal/cli/hub.go`): `hubFromOptions` wraps both FileHub and R2Hub in `EncryptedHub` via `buildKeyring` (lazy — blob-only paths like `hub gc`/`doctor` never need an epoch). `init.go`: `EnsureBootstrap` at init (no self-grant — avoids epoch collision when a second device joins). `devices.go`: `enroll --approve` and `approve` call `GrantAllEpochs`; `revoke`/`lost` call `Rotate` before blob rewrap; new `devices recipient` read-only helper (prints local age recipient / `--signing` Ed25519 public key for out-of-band enrollment).
- **Tests:** `eventcrypt_test.go` (9), `encryptedhub_test.go` (8, recordingHub+fakeKeyring), `keyring_test.go` (6, incl. `TestNewDeviceReadsHistoryAcrossEpochBump`), devicekeys WCK custody (5), store schema bump. E2E txtar `sync_encrypted.txtar` (ciphertext-only hub `grep enc.v1` + `! grep` plaintext + two-device decrypt + revoke rotate) + `sync_materialize.txtar` updated for enrollment flow.
- **Deps:** `golang.org/x/crypto v0.50.0` promoted indirect→direct.
- **Specs/docs:** spec/07 (envelope format + grant event lifecycle), spec/12 (00013 tables + schema 13 + gitstate bumped to 00014), spec/13 (devices recipient + encryption), spec/15 (metadata-leakage), spec/16 (test plan), spec/18 (this entry); `internal/hub/r2.go`+`internal/sync/hub.go`+`internal/sync/doc.go` doc fixes (event log is envelope-encrypted, not age-encrypted); `docs/audits/README.md` (P4-SEC-02 → shipped, P4-SEC-07 foundation landed).

Validated:
- `go test -race ./...` green (all packages).
- `go build ./...` green.
- E2E: `sync_encrypted.txtar` proves hub stores only `enc.v1` carriers (no plaintext path/remote), B decrypts after enroll+approve, revoke rotates to epoch 2.
- `TestMigrationsDocumented` green (00013 in spec/12).

Follow-ups:
- P4-SEC-07 full: workspace-ID pairing across devices (spec/07 §211 anticipates provisioning the same logical `ws_...` id; currently each `init` mints a separate workspace id, and the joining device's bootstrap WCK is overwritten by the origin device's grant on first pull — functional but not the intended shared-workspace model).
- P4-SEC-08 (hub-side grant verification / anti-replay) remains open.
- Hub-based WCK recovery for a solo device that loses its keychain (self-grant removed to avoid epoch collision; a re-grant from another device is the recovery path in a multi-device workspace).
- `golangci-lint run` + `gofmt -w cmd internal` to be run before PR.

## 2026-06-30 — Wire the live R2/S3 hub (P5-HUB-01)

Changed:
- **Production S3 adapter** (`internal/hub/s3client_awssdk.go`): `S3Adapter` implements the shipped `S3Client` interface over `aws-sdk-go-v2` (`s3.New(s3.Options{...})` with `BaseEndpoint`+`UsePathStyle:true` for R2/MinIO, `aws.NopRetryer{}` so `R2Hub.Retry` is the single retry layer, and an inline `aws.CredentialsProviderFunc` — no `config.LoadDefaultConfig`/SSO/IMDS/STS chain). PutObject (`IfNoneMatch:"*"`), GetObject (deferred Close), ObjectExists (HEAD→404=false), idempotent DeleteObject, ListObjectsV2 (clamped [1,1000], last key as nextStartAfter). `mapS3Error` classifies 412/PreconditionFailed→`ErrPreconditionFailed`, NoSuchKey/NotFound/404→`ErrBlobNotFound`, 429/503/SlowDown/TooManyRequests→`ErrS3Throttle`, 500/502/504/InternalError→`ErrS3Transient`, no-APIError→`ErrS3Transient`, other API→raw terminal.
- **Tests:** `internal/hub/s3client_awssdk_test.go` (hermetic `mapS3Error` + NewS3Client validation); `internal/hub/r2_test.go` refactored to a shared `assertHubRoundTrip` conformance contract (`TestR2ConformanceMemS3`); `internal/hub/r2_minio_test.go` env-gated `TestR2MinIOConformance` (skips unless `DEVSTRAP_HUB_S3_ENDPOINT`).
- **Wiring** (`internal/cli/hub.go`): `hubFromOptions(ctx, opts, store, hubFile)` r2:// branch — workspace id via `store.WorkspaceID`, creds from viper `hub_s3_*` + `AWS_` fallbacks, builds `S3Adapter`, returns `R2Hub{}` with hub-id `"r2:"+ws`. Pure `parseHubURI` (rejects credentials-in-URI) + store-free `hubConfigured`. Call sites updated: `sync.go`, `doctor.go`, `devices.go`, `hub gc`, `run_loop.go` preflight (→`hubConfigured`). `internal/cli/hub_test.go` for `parseHubURI`/`hubConfigured`.
- **Deps:** `go.mod`/`go.sum` — aws-sdk-go-v2 v1.42.0, service/s3 v1.104.1, smithy-go v1.27.3 (+ indirects).
- **Specs/docs:** flipped R2/S3 hub from planned→shipped across spec/00, 01, 02, 03, 04, 09, 13, 14, 15, 16, 17, 19 + `docs/audits/README.md` (P5-HUB-01 → shipped, 5 open → 4 open); `last_reviewed` bumped to 2026-06-30 on edited specs.

Validated:
- `gofmt -w`, `go build ./...`, `go vet ./...` clean; `go mod tidy` idempotent.
- `golangci-lint run` (v2.12.0, bodyclose+gosec) — 0 issues (fixed errcheck on `Body.Close` via named-return defer; 7× errorlint `%v`→`%w`).
- `go test -race -covermode=atomic ./...` — all green; total coverage 54.8% (≥50% floor), `internal/hub` 67.1% (`mapS3Error` 100%, `parseHubURI` 93.8%, `hubConfigured` 100%).
- `govulncheck` — 0 vulnerabilities affecting called code.
- `go run ./cmd/spec-drift --base origin/main --head HEAD` — green after spec updates.

Follow-ups:
- `P5-SYNC-01` (open, latent) — transport-cursor redesign; design in `spec/07`.
- Optional manual live MinIO round-trip (`docker run minio/minio` + `DEVSTRAP_HUB_S3_* go test -run TestR2MinIOConformance ./internal/hub`); requires Docker, not run in CI.
- Open PR `fix/p5-hub-01` → `main`, run adversarial review, merge after green CI.

## 2026-06-30 — Implement the fifth-pass (PASS5) open backlog

Changed (grouped by batch; IDs reference `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-29_PASS5.md` and PASS4 Appendix A):

- **Security (P1 + P2/P3):** `P5-SEC-01` (P1) — revoke rewrap emits a superseding `draft.snapshot.created` event and pushes event+blob to the hub before deleting the old ciphertext (`rewrapHubCleanup` ordering), so peers never replay a deleted ref. `P5-SEC-04` — env (local-only) vs draft (hub-synced) blob refs partitioned (`EnvBlobRefs`/`DraftBlobRefs`); env blobs never uploaded/deleted on the hub. `P5-SEC-02` — `draftbundle.ExtractWithLimits` charges the decompression budget on every tar entry and rejects unknown types. `P5-SEC-03` — `rebuildDependencies` runs through `childenv`. `P5-SEC-05` — `redact.Writer` caps its line buffer at 1 MiB (`emitLine` helper).
- **Sync convergence:** `P5-SYNC-02` — `ResolveConflictByFingerprint` drops the device-local `namespace_id` from its match. `P5-SYNC-03` — `RenameProject` leaves a tombstone at the old path. `P5-SYNC-04` — `conflicts resolve --keep-*` is authoritative (emits dominating `project.*` events via `internal/cli/conflict_resolve.go`; keep-remote delete-then-adds the alternate; keep-both adds a sibling). `P5-QUAL-01` — `materialize` classifies "no draft bundle yet" as skipped, not failed.
- **Hub:** `P5-HUB-01` — `hubFromOptions` selection seam routes sync/run-loop/hub-gc; r2:// returns a not-yet-wired error. `P5-HUB-02` — `devstrap hub gc` + `Hub.ListBlobs` + `PruneDraftSnapshots`. `P5-HUB-03` — `R2Hub.RetentionHLC` floor + `ErrSnapshotRequired`. `P5-HUB-04` — bounded-concurrency `R2Hub.Pull`.
- **Product:** `P5-PROD-01` reachable "ready" status; `P5-PROD-02` `pending_hub_deletes` queue (migration 00011) drained on sync; `P5-PROD-03` `devstrap env rotate`; `P5-PROD-04` README documents `clone`; `P5-PROD-05` `doctor --remote` + `status --watch`.
- **CLI/DX:** `P5-CLI-02` thread `--partial`; `P5-CLI-03` `MarkFlagsMutuallyExclusive` on clone; `P5-CLI-04` `ssh -G` host-alias resolution; `P5-CLI-05` run-loop/devices stderr + consecutive-failure exit; `P5-DX-01` dynamic shell completion (paths + enum flags); `P5-CLI-01` `options.render` seam wired into `materialize` (broader rollout deferred).
- **Data/docs/CI:** `P5-DATA-01` spec/12 migration inventory; `P5-DATA-02` migration 00012 (partial UNIQUE index + `INSERT OR IGNORE`); `P5-DOC-01` spec/07 draft/hub truth; `P5-DOC-02` spec/00 de-contradicted + command inventory; `P5-DX-02` `TestMigrationsDocumented` + AGENTS.md note; `P5-QUAL-02` run-loop/draft testscripts + jitter unit test; `P5-QUAL-03` clamped jitter bound; `P5-QUAL-04` CI coverage floor (50%); `P4-QUAL-07` `bodyclose`+`sqlclosecheck` linters; `P4-SEC-05` SHA-pin `goreleaser-action`.
- Schema version 10 -> 12 (migrations 00011, 00012).

Validated:
- `gofmt -l cmd internal` (clean), `go vet ./...`, `go build ./...`, `go mod tidy` (no diff).
- `golangci-lint run` (v2.12.0) — 0 issues (with the new `bodyclose`/`sqlclosecheck` enabled).
- `GOCACHE=… DEVSTRAP_NO_KEYCHAIN=1 go test -race ./...` — all packages green.
- `go run ./cmd/spec-drift --base origin/main --head HEAD` after this work-log + spec updates.
- Adversarial multi-agent review of the diff (5 dimension reviewers + per-finding verification) surfaced 7 confirmed defects (3 P2, 4 P3), all fixed before handoff: dependency-rebuild now uses `AgentAllowlist`+`HOME=projectdir` (was leaking ssh-agent/real HOME to lifecycle scripts); `conflicts resolve` enacts BEFORE emitting the `conflict.resolved` event (a failed/inapplicable resolution no longer diverges peers) and `--keep-remote` is a single atomic `project.updated` (no delete-then-add tombstone-without-re-add window); `LatestDraftSnapshot` ordering aligned with `PruneDraftSnapshots` (HLC-first) so prune can't delete the materialized snapshot; `hub gc --dry-run` uses `RetainedBlobRefs` for an accurate preview; SSH alias resolution rejects leading-dash aliases (option-injection guard) and the file parser honors OpenSSH negation; `env rotate`/`env capture` share the `git_repo` guard.

Follow-ups (deliberately deferred, documented with design):
- `P5-SYNC-01` (open, P2, latent) — decouple the transport cursor from logical HLC via a hub-assigned ingestion position; a core-engine change best landed as its own PR with multi-device tests, paired with `SYNC-02`/`HUB-11` snapshot/compaction (design recorded in `spec/07`).
- `P5-HUB-01` remaining step — the production `aws-sdk-go-v2` S3 client adapter + MinIO/LocalStack integration test (the seam, keying, retry, conditional-put, and GC logic are shipped and unit-tested).
- `P5-CLI-01` — extend the `render` seam to all leaf commands (or reject `--json` where unsupported).
- `P5-ARCH-01` — convergence is covered by new property-style apply tests; the formal pure `Decide(state,event)` extraction remains.
- PASS4 carried XL items: `SEC-07` envelope encryption (the structural fix for the revoke/rewrap model), `GIT-03` OS-enforced agent sandbox, `SEC-02`/`SEC-04` at-rest map encryption + fail-closed enrollment, `SYNC-02`/`HUB-11` compaction.

## 2026-06-29 — Consolidate audit files into docs/audits/ + status ledger (P5-PROC-01)

Changed:
- Moved all five `AUDIT_RECOMMENDATIONS*.md` from the repo root into `docs/audits/` (`git mv`) to declutter the root and end cross-pass finding-ID collisions (fifth-pass finding `P5-PROC-01`).
- Added `docs/audits/README.md` — the single source-of-truth audit ledger: a pass index, go-forward conventions (pass-scoped unique IDs, archive policy, work-log-rotation note), and a consolidated **open backlog** (Pass 5's 36 findings plus Pass 4's ~25 still-open items, pass-scoped as `P4-*`/`P5-*`; earlier passes superseded).
- Rewrote every reference to the moved files: `tracks_code:` frontmatter in `spec/00`, `spec/12`, `spec/14`, `spec/17`; prose pointers across `spec/00`–`spec/19` and `spec/adr/*`; the two README audit links (now pointing at the archive index with PASS5 as latest); the `internal/sync/doc.go` comment; and the `.github/ISSUE_TEMPLATE` glob example. Historical `spec/18_WORK_LOG.md` entries were left referencing the old root paths (accurate for when they happened).
- Generalized the spec-drift gate: `internal/specdrift/specdrift.go` `requiresWorkLog` now treats any change under `docs/` as work-log-requiring (replacing the exact `AUDIT_RECOMMENDATIONS.md` special-case), so the moved audit docs and future docs still gate on the work log.

Validated:
- `gofmt -l`, `go build ./...`, `DEVSTRAP_NO_KEYCHAIN=1 go test ./... -count=1` (incl. `internal/specdrift`), and `go run ./cmd/spec-drift --base origin/main --head HEAD` (green). A repo-wide sweep confirms no bare root `AUDIT_RECOMMENDATIONS` references remain outside the archive and the historical work-log entries.

Follow-ups:
- Work-log rotation (archive cycles older than the two most recent passes into a dated file) remains a deferred follow-up to keep this PR reviewable.

## 2026-06-29 — Fifth-pass design & implementation audit (post-PR-#20)

Changed:
- Added `AUDIT_RECOMMENDATIONS_2026-06-29_PASS5.md` at the repo root — a fifth-pass audit of trunk `be664ba`, focused on (a) adversarial review of the just-landed PASS4 batch code (forge/conflicts/clone/materialize/run-loop/blob_gc/hub-r2/draftbundle), (b) dimensions PASS4 under-examined (convergence of the new conflict/rename paths, end-to-end hub reachability, CLI scriptability, observability, spec truth, process hygiene), and (c) concrete new features.
- Produced by a verification-driven 7-dimension multi-agent workflow (per-dimension review → independent adversarial verification of every finding against the live code → consolidation): 43 candidate findings, 41 verified, **36 reported (P1=1, P2=12, P3=23)** after merging overlaps. Uses a `P5-` ID prefix to end the cross-pass ID collisions the audit itself flags (`P5-PROC-01`).
- Headline findings: `P5-SEC-01` (P1) revoke rewrap deletes the old hub blob without emitting a superseding namespace event → other devices permanently lose draft access; `P5-HUB-01` the R2 backend is unwired (no aws-sdk dependency, dead `R2Config`, `FileHub` hardcoded, no selection seam); `P5-SYNC-01..04` convergence/conflict regressions in the just-landed code (HLC-keyed pull cursor strands cross-batch events; `conflict.resolved` bakes a device-local namespace_id so it can't converge; rename leaves no source tombstone; `conflicts resolve --keep-*` never mutates state); `P5-QUAL-01` the `materialize` exit-code fix backfires on synced local-only projects; `P5-DX-02` the spec-drift gate is blind to prose staleness.

Validated:
- `go run ./cmd/spec-drift --base origin/main --head HEAD` (green).
- Docs-only change; no Go code modified (gofmt/build/`go test` n/a). Every finding cites `file:line` against `be664ba` and was independently adversarially verified against the live tree before inclusion.

Follow-ups:
- Implementing the 36 findings is future work; highest priority: `P5-SEC-01` + envelope encryption (`SEC-07`), `P5-HUB-01` reachability (S3 adapter + `hubFromOptions` + MinIO integration test), `P5-SYNC-02`/`P5-SYNC-04` conflict convergence, `P5-QUAL-01` exit-code fix, and `P5-DX-02` gate hardening.
- The stale-spec findings (`P5-DOC-01`/`P5-DOC-02`/`P5-DATA-01`) are documentation follow-ups; this cycle adds the audit, not the spec fixes.

## 2026-06-29 — PASS4 audit Phase A/D quick wins (part 4)

Changed:
- Continued PASS4 quick wins: GIT-05 (P2), PROD-06 (P2), GIT-06 (P2).
- **GIT-05**: forge detection now supports self-hosted GitLab/Gitea/Forgejo. New `ResolveForge` with documented precedence — `--forge` flag > per-project `git_repos.forge_kind` column > `[forge] host = kind` config map > `DetectForge` heuristic. Added migration `00010_repo_forge_kind.sql` (new `forge_kind` column, schema version 10), `Store.SetProjectForgeKind`, and threaded `ForgeKind` through `UpsertProjectParams`/`ProjectStatus`/`GitRepo` + the UPSERTs and SELECTs. SSH host aliases (`~/.ssh/config` `Host`->`HostName`) are resolved before detection so `git@work-gitlab:org/repo` maps to the real host. `agent pr` gained a `--forge` flag; `doctor` now iterates adopted git-repo remotes, resolves the forge, and warns when the matching CLI (`gh`/`glab`/`tea`) is missing or the forge is unknown.
- **PROD-06**: `conflicts` is now a command group (`list`/`show`/`resolve`) instead of a list-only leaf. `resolve <id>` accepts exactly one of `--keep-local`/`--keep-remote`/`--keep-both`, marks the row `resolved` (so the `status` open-conflict count converges), records the decision in `resolution_json`, and emits a signed `conflict.resolved` HLC event (`CreateConflictResolvedEvent`) so every device sees the same outcome. `devstrap conflicts` with no subcommand still lists.
- **GIT-06**: the materialize/hydrate clone path now initializes submodules (`--recurse-submodules` + `--also-filter-submodules` for blobless submodules) under a `materialization.submodules` policy (`auto` default / `never`). Added `git.CloneOptions` + `CloneWithOptions` (keeping `Clone` as a thin wrapper) and a `Runner.MaintenanceRun` helper; an opt-in `materialization.maintenance` config runs a one-time `git maintenance run --auto` after clone so blobless clones do not trigger per-object lazy-fetch storms. `doctor` surfaces the blobless-clone offline caveat (historical blobs need the promisor online).
- Tests: `TestParseForgeKind`/`TestResolveForgePrecedence`/`TestSSHHostMatch`/`TestResolveSSHHostAlias`/`TestDetectForgeResolvesSSHAlias` + a `--forge gitlab` fake-CLI override test (GIT-05); `TestResolveActionValidation`/`TestConflictsListShowResolve` (PROD-06); `TestCloneArgsSubmodules`/`TestCloneWithOptionsInitializesSubmodules` (real-git submodule clone) (GIT-06); updated `TestInitStatusAndDBCommands`/`TestMigrateEnsureSummaryAndVersion`/`TestMigrationDownAndUp` for schema version 10.

Validated:
- `gofmt -w cmd internal`, `go vet`, `DEVSTRAP_NO_KEYCHAIN=1 go test ./... -count=1` (all green), `spec-drift` passes (20 specs, 52 changed files).
- golangci-lint not installed in this environment.

Follow-ups:
- GIT-05: per-project forge override has no `set` CLI yet (the column is writable via `SetProjectForgeKind`); native Bitbucket/Azure PR clients; broader FORGE-05 hermetic fake-CLI tests.
- PROD-06: `--keep-both` records the dual-copy intent and clears the row; it does not auto-clone the remote variant under a sibling path (no network/ambiguity risk). Honoring a prior resolution to suppress re-conflict on re-sync is a sync-engine follow-up.
- GIT-06: per-project `materialization.submodules` column (currently config-level only); `git maintenance start` (scheduled) vs the one-time `run --auto`; submodule hydrate-state recording.
- Remaining: SEC-02 (encrypt namespace map, L), SEC-04 (bootstrap pinning, deferred), SEC-05 (release signing), SYNC-02/HUB-11 (compaction, L), SYNC-06 (tombstone GC), SYNC-03 (needs HLC test refactor), SYNC-05 (folded hash + signed head), QUAL-02 (property tests), SEC-07/08, HUB-14/15/16, GIT-04 (worktree GC), GIT-03 (XL sandbox), PROD-04/05, and Phase E.

## 2026-06-29 — PASS4 audit Phase A/D quick wins (part 3)

Changed:
- Continued PASS4 quick wins: PROD-01 (P1), PROD-02 (P1), SYNC-04 (P2), QUAL-01 (P1).
- **PROD-01**: added `devstrap clone <url> [path]` — a one-shot quick path that derives a namespace path from the remote (`work/<org>/<repo>`, overridable), runs the existing add + eager materialize (blobless clone + env hydrate) + optional `--open`/`--vscode`. Extracted a shared `addProject` helper from `add` so clone is a thin orchestrator over existing internals. Registered in `root.go`; documented in `spec/13` (command-doc drift gate satisfied).
- **PROD-02**: `doctor` is now a severity-graded health report. Each check returns `{name, status: ok|warning|error, detail, remedy}`; rendered as a graded table + summary line, with a non-zero exit on any error (CI-gateable). `--json` emits the check array; `--fix` applies safe remediations (create missing state home, run pending migrations, clear stale repo locks via `clearRepoLock`) and re-runs checks; `--no-network` flag added. Checks cover git (required)/gh/go (optional), state home, schema version, SQLite quick_check/foreign_key_check, secrets needing rotation, device age + Ed25519 key health, and held repo locks (stale = warning).
- **SYNC-04**: the push side is now cursor-bounded. `runSyncCycle` reads a per-hub `push:<hubID>` watermark, fetches only local-origin events with `HLC > pushCursor` via the new `Store.LocalPendingEvents(ctx, afterHLC)`, pushes them, and advances the watermark. Remote-origin events are no longer re-uploaded every cycle (the hub already holds them from their origin device), so a no-op sync pushes zero instead of the whole log.
- **QUAL-01**: `draftbundle.Extract` now enforces an aggregate decompression budget (max total uncompressed bytes + max file count) via the new `ExtractWithLimits`, aborting a gzip/tar decompression bomb authored by a compromised-but-trusted device with `ErrBundleTooLarge` (the per-file `LimitReader` alone did not bound total size/count). `Extract` delegates with the Pack-side defaults (100 MiB / 5000 files). Added Go native fuzz targets `FuzzParseBytes` (envfile) and `FuzzCompile` (ignore) with seed corpora from existing table tests; they run as ordinary tests on `go test` and fuzz under `-fuzz`.
- Tests: `TestDeriveClonePath` + `clone.txtar` (PROD-01), `doctor.txtar` + updated `TestInitStatusAndDBCommands`/`init_status.txtar` for the graded format (PROD-02), `sync_push_cursor.txtar` (SYNC-04), `TestExtractRejectsTooManyFiles`/`TestExtractRejectsOversizedBundle` (QUAL-01), `FuzzParseBytes`/`FuzzCompile` seed corpus (QUAL-01).

Validated:
- `gofmt -w cmd internal`, `go vet`, `DEVSTRAP_NO_KEYCHAIN=1 go test ./... -count=1` (all green), `go test -race` on touched packages, `spec-drift` passes.
- golangci-lint not installed in this environment.

Follow-ups:
- QUAL-01 CI fuzz step (`go test -fuzz=... -fuzztime=30s`) and `FuzzCanonicalRemoteKey`/`FuzzExtract` not yet wired (the fuzz targets exist and pass seed corpora; the CI step + the git/draftbundle fuzz targets remain).
- Remaining: SEC-02 (encrypt namespace map, L), SEC-04 (bootstrap pinning, deferred), SEC-05 (release signing), SYNC-02/HUB-11 (compaction, L), SYNC-06 (tombstone GC), SYNC-03 (needs HLC test refactor), QUAL-02 (property tests), GIT-04/05/06, HUB-14/15/16, PROD-04/05/06, and Phase E.

## 2026-06-29 — PASS4 audit Phase A quick wins (part 2)

Changed:
- Continued the PASS4 audit quick wins: SYNC-01 (P1, low-water-mark cursor), QUAL-06 (P2, jitter + aggregate retry budget), PROD-03 (P2, guided init).
- **SYNC-01**: `ApplyEvents` now returns a low-water-mark safe cursor instead of `maxAppliedHLC`. It tracks `lowestUnappliedHLC` over every transiently-skipped event (skew-ahead quarantine and hash-chain breaks) and returns `min(maxAppliedHLC, lowestUnappliedHLC-1)`, so a skipped event with a lower HLC than a higher-HLC applied event is never permanently stranded — the hub pull cursor never advances past it, so it is re-delivered next cycle. Permanently-invalid events (HLC<=0 / below epoch floor) are recorded as conflicts but do NOT hold the cursor (they will never re-apply, and holding at a non-positive cursor would strand every higher event). `runSyncCycle` advances the cursor to the returned safe value. The misleading "will be re-delivered next pull" comment was corrected.
- **QUAL-06**: git network retry backoff switched from deterministic linear (`base*attempt`) to full-jitter capped exponential (`jitterDelay`: uniform in `[1, min(cap, base*2^(attempt-1))]`), the AWS-recommended scheme, so parallel materialize workers no longer retry in lockstep (thundering herd) against a struggling forge. `Runner` gained `RetryCap` (default 5s) and `MaxElapsed` (optional aggregate wall-clock budget per operation; when set, the retry loop stops once elapsed). `sleepBackoff` takes the cap; `jitterDelay` is a pure function taking a `randFn` for deterministic seeded-RNG testing.
- **PROD-03**: `devstrap init` gained a `--scan` flag that runs the existing scan/adopt path inline after workspace creation, so a user with a populated `~/Code` sees their tree adopted on the very first command (the "epiphany" moment). The adopt logic was extracted into a shared `adoptFindings` helper used by both `scan --adopt` and `init --scan`. `init` always prints a short next-steps hint (`devstrap status • devstrap scan --adopt • devstrap sync --hub-file <path>`) per clig.dev guidance.
- **HUB-13**: `FileHub.Pull` and `R2Hub.Pull` now filter with an inclusive `>= afterHLC` boundary instead of strict `>`, so a same-HLC event from another device that arrives after the cursor was advanced to that HLC is still delivered on the next pull (HLC is not globally unique across devices). Re-delivering the boundary is safe because `ApplyEvents`/`InsertEvent` dedups by event ID. The Hub doc comment was updated to document the inclusive boundary. The composite-`(HLC,device,id)` cursor (zero re-delivery) is deferred as a future optimization; the inclusive overlap is the audit's recommended cheap fix.
- Tests: `TestApplyEventsLowWaterMarkCursorHoldsBelowSkippedEvent` / `TestApplyEventsPermanentInvalidDoesNotHoldCursor` (SYNC-01), `TestJitterDelayFullJitterBounded` (QUAL-06), `init_scan.txtar` (PROD-03), `TestFileHubPullInclusiveBoundaryDeliversSameHLC` (HUB-13); updated `TestR2PullCursorIncremental` and `sync_materialize.txtar` for the inclusive boundary.

Validated:
- `gofmt -w cmd internal`, `go vet ./internal/git/ ./internal/cli/ ./internal/sync/`
- `GOCACHE=/tmp/devstrap-gocache DEVSTRAP_NO_KEYCHAIN=1 go test ./... -count=1` (all green)
- `go run ./cmd/spec-drift --base origin/main --head HEAD`
- Subagent code review (PR #20) found one MAJOR: `rewrapBlobsOnRevoke` deleted old hub ciphertext even when the rewrapped-blob push failed (hub data loss). Fixed by gating the delete on a successful push (extracted `rewrapHubCleanup` with early-return gating) + added `TestRewrapHubCleanupKeepsOldBlobOnPushFailure`/`TestRewrapHubCleanupDeletesOldBlobOnSuccess`; also clamped `R2Retry.sleep` exp to `[1,cap]` for overflow robustness. Re-validated full suite + spec-drift green.

Follow-ups:
- Remaining Phase A: SEC-04 (fail-closed bootstrap — the fail-closed-once-enrolled logic is already implemented; the pre-enrollment bootstrap-window closure requires an out-of-band pinning ceremony + authenticated snapshot and changes the core sync-without-enroll demo flow, so it is deferred as L-effort), SEC-02 (encrypt namespace map, L), SEC-05 (sign releases, infra). Then Phases B–E.
- SYNC-03 (P2/S) deferred: raising `epochFloorMS` + adding the past-direction staleness bound requires updating all deterministic sync tests to use realistic HLC physical components (they currently use `physical=0`), a coordinated refactor.
- QUAL-06 materialize-pass aggregate context deadline not yet wired (the per-operation `MaxElapsed` field is in place for callers to opt into); deferred to avoid breaking slow CI clones.

## 2026-06-29 — PASS4 audit Phase A quick wins (part 1)

Changed:
- Implemented the first batch of `AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md` Phase A "make the hub backend safe to turn on" quick wins.
- **GIT-02**: `git.Clone` now uses a clone-specific retry that `os.RemoveAll`+`os.MkdirAll` the destination before every retry, so a transient mid-clone network failure (which leaves the pre-existing `MkdirTemp` dest partially populated) is recoverable instead of fatal "destination path already exists and is not empty". Extracted a shared `sleepBackoff` helper used by both `Clone` and `runWithNetworkRetry`.
- **QUAL-03**: `devstrap materialize` now returns non-zero (`ErrPartialMaterialize`) when any project fails, while still completing the batch (EAGER-04 isolation), so CI/cron gates and `&&` chains can detect partial failure.
- **HUB-09**: `R2Hub.Push`/`PutBlob` dropped the redundant `ObjectExists` (HEAD) pre-check; the conditional put (`If-None-Match: *`) is the atomic guard. Added a typed `ErrPreconditionFailed` (R2 412/10031) returned by the `memS3` double and classified as an idempotent dedup hit, halving Class B request volume and closing the TOCTOU race.
- **SEC-03**: `pullReferencedBlobs` now recomputes sha256 of fetched ciphertext and rejects on mismatch against the signed `age_blob:<sha256>` ref (`verifyBlobContentHash`), so a malicious/buggy hub cannot substitute bytes under a valid content-addressed key. Mismatched blobs are not cached and surface as missing/tampered.
- **GIT-01**: `hydrateProjectUnlocked` verifies a resolvable HEAD after promote; if HEAD is unresolvable it self-heals (re-resolve remote default branch + checkout) and records an honest `materialized-empty` state (surfaced in `status` as "empty checkout") when commits exist but HEAD is broken. A legitimately empty repo (no commits) is still recorded as `available` so hydrating a fresh remote succeeds.
- **HUB-10**: `R2Hub` now wraps every S3 call (`Push`/`Pull`/`PutBlob`/`GetBlob`) in an `R2Retry` seam with throttle/transient/terminal error classification and capped exponential backoff + full jitter; the context is honored between attempts. `ErrS3Throttle`/`ErrS3Transient` sentinels; terminal errors (incl. 412, not-found, auth) fail fast. A zero-value `R2Retry` uses a default policy; tests inject a deterministic jitter + tiny delays. The real aws-sdk-go-v2 standard retryer will slot behind this seam when the SDK is wired.
- **SEC-06**: `redact.tokenPatterns` extended with GitLab (`glpat-`), Stripe (`sk_live_`/`rk_live_`), generic `Bearer <token>`, and a JSON-secret-field redactor (`jsonSecretField`) that masks the value of any field named like a secret (secret/token/password/private_key/api_key/authorization) while preserving the key — catching GCP service-account `private_key` (base64 on one JSON line) and Snowflake config passwords the bare token-prefix patterns miss.
- **SEC-01**: added `DeleteBlob` to the `Hub` interface (+ `FileHub`, `R2Hub`) and `DeleteObject` to `S3Client` (+ `memS3`), the reclamation primitive that makes blob/event GC possible (also serves HUB-12). `rewrapBlobsOnRevoke` is now hub-aware: when a hub is provided it pulls non-cached blobs from the hub (with SEC-03 hash verification) before rewrapping, `PutBlob`s the rewrapped blob, and `DeleteBlob`s the old ciphertext (guarded by `blobRefStillReferenced` so a still-referenced blob is never deleted). `devstrap devices revoke|lost` gained an optional `--hub-file` to trigger hub-side cleanup at revoke time; without it, rewrap is local-only and hub cleanup is deferred to the next sync. `needs_rotation` remains belt-and-suspenders since already-downloaded ciphertext is irrecoverably exposed.
- Tests: `TestCloneRetryCleansPartialDestination` (GIT-02), `TestR2WritePathSkipsObjectExists` (HUB-09), `TestR2PushRetriesThrottling`/`TestR2PushRetriesTransient`/`TestR2PushDoesNotRetryTerminal`/`TestR2RetryRespectsContextCancellation` (HUB-10), `TestVerifyBlobContentHash` (SEC-03), `TestScrubExtendedTokenShapes`/`TestScrubJSONSecretFields` (SEC-06), `TestR2HubDeleteBlob`/`TestFileHubBlobPutGetDelete`/`TestFileHubDeleteBlobLeavesEventLogUntouched` (SEC-01/HUB-12), `materialize_nonzero_on_failure.txtar` (QUAL-03).
- Note: the PASS4 audit reuses `GIT-01`/`GIT-02` IDs (empty-checkout / clone-retry) that collide with the second-pass audit's same-named findings; spec prose will reference the PASS4 audit file to disambiguate. Specs are reconciled in the end-of-session review (AGENTS.md).

Validated:
- `gofmt -w cmd internal`, `go vet ./internal/hub/... ./internal/git/... ./internal/cli/... ./internal/redact/...`
- `GOCACHE=/tmp/devstrap-gocache DEVSTRAP_NO_KEYCHAIN=1 go test ./...` (all green)
- New tests green: GIT-02, HUB-09, HUB-10, SEC-03, SEC-06, QUAL-03 testscript.

Follow-ups:
- Remaining Phase A: SEC-04 (fail-closed bootstrap), SEC-02 (encrypt namespace map, L), SEC-05 (sign releases). Then Phases B–E.
- SEC-01 signed `env.bundle.reencrypted` audit event (audit step 4) not yet emitted; the core revoke-delete + rewrap is done, the audit-trail event is deferred.
- SEC-03 sender-authentication (Ed25519 producer signature over bundles) is a larger sub-item; hash-verification (the headline "verify blob hashes on fetch") is done, producer-signature deferred.

## 2026-06-28 — README rebuild with brand banner + app icon

Changed:
- Rewrote `README.md` to modern open-source conventions: a centered `repo_image2.png` brand banner in the header, a badge row (CI, Go Report Card, Go 1.26, platform, MIT, alpha status), a table of contents, and clear sections (What is it / Why / How it works / Features / Status / Requirements / Install / Quickstart / Command reference / Architecture / Roadmap / Security / Contributing / License).
- Added the brand assets `repo_image2.png` (header banner) and `icon.png` (app icon for the forthcoming desktop/menu-bar app, referenced in the footer) at the repo root.
- Corrected stale content: the old README still described a `dev` integration branch; the Contributing section now states the canonical **trunk-based** model (single protected `main`, no `dev`) per `AGENTS.md`. Updated the status/feature/command sections to reflect the now-shipped cloud-sync workstreams (eager `materialize`, `draft`, the pluggable `Hub` + R2/S3 backend, portable `run-loop`) and the full 19-command surface; linked the latest audit (`AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md`).

Validated:
- Command table cross-checked against `internal/cli/root.go` `AddCommand` registrations and each command's cobra `Short` string; Go version against `go.mod` (1.26); install story against the GoReleaser pipeline (binaries; no Homebrew tap yet — flagged as roadmap `PROD-05`).
- `go run ./cmd/spec-drift --base origin/main --head HEAD`.

Follow-ups:
- None. A Homebrew tap / `curl | sh` installer (audit `PROD-05`) and the brand assets' use in the future app remain future work.

## 2026-06-28 — Fourth-pass design & implementation audit (post-PR-#16)

Changed:
- Added `AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md` at the repo root — a fourth-pass audit of the *now-shipped* cloud-sync system (PR #16). Produced by a six-dimension multi-agent review (per-dimension audit → independent adversarial grounding against the live tree → sectioned synthesis) with external best-practice research, yielding **44 verified findings** (P1=17, P2=23, P3=4) across Security/Crypto (`SEC-*`), Sync Engine & Data Model (`SYNC-*`), Cloud Hub & Scalability (`HUB-09..16`), Git Materialization & Agents (`GIT-*`), Code Quality & Testing (`QUAL-*`), and Product/UX & New Features (`PROD-*`). The new cloud-hub findings are numbered `HUB-09..16` to continue (not collide with) the shipped `HUB-01..08`.
- Linked the new audit from `spec/00_START_HERE.md` (a third top-of-file blockquote, matching the prior two audits) and added it to that file's `tracks_code` frontmatter.
- Headline findings: encrypt the namespace map before R2 upload (`SEC-02`), verify content-addressed blobs on fetch (`SEC-03`), make device revocation actually delete/rotate hub ciphertext (`SEC-01`), wire event-log compaction/snapshot + hub-side GC (`SYNC-02`/`HUB-11`/`HUB-12`), fix the clone network-retry-into-non-empty-dir bug (`GIT-02`) and record honest state on empty checkouts (`GIT-01`), and grow the product surface (`devstrap clone`, graded `doctor --fix`, `service install` daemon — `PROD-01/02/04`).

Validated:
- Each finding carries `file:line`/spec evidence checked by an adversarial verifier (already-implemented / unsupported findings were dropped or rescoped); e.g. the initial `GIT-01` "needs `ls-remote --symref`" claim was corrected to "verify non-empty checkout" after confirming `git clone` already resolves HEAD over the protocol handshake.
- `go run ./cmd/spec-drift --base origin/main --head HEAD`.

Follow-ups:
- This audit is the recommendation backlog; converting `SEC-*`/`SYNC-*`/`HUB-09..16`/`GIT-*`/`QUAL-*`/`PROD-*` into implemented workstreams is the next set of cycles. Sequencing guidance is in the audit's Appendix C.

## 2026-06-28 — go.mod hygiene hotfix (main CI red after PR #16)

Changed:
- `go mod tidy` promoted `golang.org/x/sync v0.21.0` from the `// indirect` block to the direct `require` block. It is a direct dependency — `internal/cli/materialize.go` imports `golang.org/x/sync/errgroup` for the bounded-concurrency eager materialization added in PR #16 — but the dependency was added without re-tidying, so go.mod was left inconsistent.
- This was latent in PR #16: the CI `Go tests` job runs `Test` before `Module hygiene`, so while the e2e testscripts failed (`Test`), the job never reached the `go mod tidy` / `git diff --exit-code` check. The testscript fix unblocked `Test`, which exposed the go.mod drift and left `main` red post-merge.

Validated:
- `go mod tidy` is now idempotent (second run is a no-op); `go.sum` unchanged.
- `gofmt -l cmd internal` (clean), `go vet ./...`, `go build ./...`, `go test -race ./...` (all pass), `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`.
- `go run ./cmd/spec-drift --base origin/main --head HEAD`.

Follow-ups:
- None. Consider reordering the CI `Go tests` job so `Module hygiene`/`go vet`/`gofmt` run before the slow `Test` step, surfacing cheap hygiene failures first.

## 2026-06-28 — Hermetic git in cloud-sync e2e testscripts (PR #16 CI fix)

Changed:
- Made `cmd/devstrap/testdata/script/sync_materialize.txtar` and `headless_keycustody.txtar` hermetic. They passed locally but failed on CI for two environment-dependent reasons:
  - **Git identity**: CI runners have no global `user.name`/`user.email`. `git commit` auto-detects an identity on macOS but fails on Linux (`unable to auto-detect email address`), so Linux failed at the setup commit. Fixed by exporting `GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_NAME`/`GIT_COMMITTER_EMAIL` in each script.
  - **Default branch**: `git init --bare` uses `init.defaultBranch` (defaults to `master`), but the scripts push to `main`. On a clean runner the bare HEAD pointed at `master`, so device B's blobless clone checked out an empty tree (no `README.md`) — the macOS failure. Fixed by `git init --bare -b main`.

Validated:
- Reproduced both failures locally under `GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1` (CI-equivalent stripped git config); both pass after the fix.
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...` — all packages pass.
- `go run ./cmd/spec-drift --base origin/main --head HEAD` — passed.

Follow-ups:
- Consider making devstrap materialization resolve the remote default branch authoritatively (`ls-remote --symref`) rather than trusting the cloned remote's HEAD, so a misconfigured remote HEAD never yields an empty working tree.

## 2026-06-28 — Code review fixes for cloud-sync PR (#16)

Changed:
- **C1**: Fixed HUB-04 rewrap dead code in `devices.go` — the early `return err` after the rotation warning prevented `rewrapBlobsOnRevoke` from running when `flagged > 0` (the exact case it was built for). Wrapped the warning in an `if` block so execution falls through to rewrap.
- **C2**: Added `internal/draftbundle/draftbundle_test.go` — 13 tests covering Pack/Extract round-trip, secret-file refusal (`.env`, `id_rsa`), size/file-count limit enforcement, recipient requirement, bad-identity rejection, dual-copy-on-conflict, node_modules exclusion, `.devstrap` dir skip, empty dir, nested directories, and blob-ref format.
- **I1**: Changed `ApplyEvents` to return `(int64, error)` where int64 is the max HLC of actually-inserted events; cursor now advances only past applied events, not quarantined/conflicted ones (prevents permanent loss of skipped events).
- **I3**: `hasEnrolledDevices` now only swallows the specific "no such table" error (early bootstrap); all other DB errors propagate so HUB-03 fail-closed verification is not silently downgraded.
- **I4**: `pushReferencedBlobs` returns an error when a referenced blob can't be read from local cache, preventing dangling blob references on the hub.
- **I5**: `pullReferencedBlobs` now returns `(int, error)` with a count of missing blobs; caller prints a warning so hub data loss is surfaced.
- **I6**: `draftbundle.Extract` now writes incoming files to `<name>.devstrap-conflict` on conflict instead of silently dropping them (true dual-copy per DRAFT-01).
- **M1**: Removed incorrect tar traversal guard (`filepath.Clean("/"+hdr.Name)` doesn't catch `../`); the `pathWithin` check is the real guard.

Validated:
- `gofmt -w cmd internal`
- `golangci-lint run` — 0 issues
- `go run ./cmd/spec-drift --base origin/main --head HEAD` — passed (20 specs, 35 changed files)
- `DEVSTRAP_NO_KEYCHAIN=1 go test ./...` — all 19 packages pass
- `DEVSTRAP_NO_KEYCHAIN=1 go test -race ./internal/sync/... ./internal/draftbundle/... ./internal/cli/... ./internal/state/...` — all pass

Follow-ups:
- None (all critical and important review issues addressed)

## 2026-06-28 — Cloud-sync audit implementation: EAGER/DRAFT/HUB/XP workstreams

Changed:
- **HUB-01**: Extracted a pluggable `Hub` interface (`Push`/`Pull`/`PutBlob`/`GetBlob`) in `internal/sync/hub.go` with typed errors (`ErrSnapshotRequired`, `ErrBlobNotFound`, `ErrInvalidBlobKey`); `FileHub` now satisfies it and gains a file-backed blob plane.
- **HUB-02/HUB-06**: Added `internal/hub/r2.go` — the Cloudflare R2 zero-knowledge backend with the HUB-06 immutable object-keying scheme (`workspaces/<ws>/events/<hlc-padded>/<device>/<seq>/<id>.json`, `workspaces/<ws>/blobs/<sha256>`), conditional put, bounded `ListObjectsV2` pagination, and cursor-based pulls. S3 operations are abstracted behind an `S3Client` interface with an in-memory conformance double (`internal/hub/mems3_test.go`).
- **HUB-07/HUB-08**: Added `R2Config` with self-hosted vs hosted credential scoping (prefix-scoped temporary credentials for SaaS/runners) and explicit backend naming (file/s3-r2/http-sse) in `internal/hub/doc.go`.
- **HUB-03**: Made event verification fail-closed once enrollment exists — `verifyEventSignature` now requires valid signatures from approved devices for ALL non-local event types when any approved device is enrolled, while preserving the local device's pre-enrollment grace and the bootstrap window.
- **HUB-04**: Added `envbundle.Rewrap` (generic age re-encryption) and `rewrapBlobsOnRevoke` — on device revoke/lost, all referenced blobs are re-encrypted to the reduced recipient set and references repointed; secrets are already flagged `needs_rotation`.
- **HUB-05**: Added `gcUnreferencedBlobs` (local blob cache GC for zero-ref-count blobs) and `store.BlobRefCount`/`AllBlobRefs`/`UpdateBlobRef` methods; retention/snapshot-horizon gating noted as deferred until full-state snapshot exchange exists.
- **EAGER-01/EAGER-04**: Added eager materialization to `sync` (and a standalone `materialize` command) — after applying namespace events, bounded-concurrency (`errgroup.SetLimit(min(4, NumCPU))`) worker pool blobless-clones every skeleton `git_repo` with per-project failure isolation (mark `failed`, continue). New `internal/cli/materialize.go`.
- **EAGER-02**: Wired cursor-based incremental pull — `sync` reads `hub_cursors.last_hlc_applied` before `Pull`, passes it as `afterHLC`, and advances it after `ApplyEvents`. New migration `00008_sync_hub_cursor.sql`. Second sync with no new events pulls zero.
- **EAGER-03**: After materializing a `git_repo`, sync hydrates the project's bound env profile into `.env` (best-effort, no clobber).
- **DRAFT-01**: Added type-dispatch materialization: `git_repo` → blobless clone; `local_git`/`draft_project` → decrypt-and-extract draft bundle (or honest interim error); `plain_folder` → create skeleton directory.
- **DRAFT-02**: Added `internal/draftbundle` (tar+gzip+age pack/unpack with `.devstrapignore` allow-list, size/file-count limits, secret-file refusal, dual-copy-on-conflict extract), `draft.snapshot.created` event type + apply handler, `draft snapshot create` CLI command, blob plane push/pull in sync, and `00009_draft_snapshots.sql` migration.
- **DRAFT-03**: Added `internal/ignore` — the canonical `.devstrapignore` compiler (gitignore-compatible semantics) with one default OS-junk/build-artifact table feeding the scanner prune predicate, bundle walker, and generated `.gitignore` fragments. Scanner's `shouldPruneDir` now delegates to it.
- **DRAFT-04**: Enforced `draft_projects.max_bytes`/`max_files` during `draftbundle.Pack` with actionable error messages.
- **DRAFT-05**: Excluded `node_modules`/build artifacts from bundles via the ignore compiler; added opt-in (`DEVSTRAP_NO_KEYCHAIN`-gated) post-hydrate dependency rebuild (`npm ci`/`pnpm install`/`uv sync`/`go mod download`/`cargo fetch`).
- **XP-02**: Added `devstrap run-loop` — a portable foreground ticker (scan → sync → materialize) with jittered backoff and `--once` for cron; explicitly not a daemon.
- **XP-01**: Added `sync_materialize.txtar` testscript — two-device e2e proving device B gets a real blobless clone after sync and the cursor pulls zero on a second sync.
- **XP-03**: Added `headless_keycustody.txtar` testscript — init + env capture + env hydrate with `DEVSTRAP_NO_KEYCHAIN=1` and file-backed device identity.
- **XP-04**: Added `TestCrossFilesystemCaseFoldNFCInvariant` — locks down the cross-filesystem case-fold + NFC path-key collision invariant (case-only paths collide on every filesystem; NFC vs NFD normalize to the same path).
- Applied remote events now re-stamp `workspace_id` to the local workspace and create placeholder `pending` device rows so the events FK constraint is satisfied across devices.

Validated:
- `gofmt -w cmd internal`
- `golangci-lint run` (v2.12.0) — 0 issues
- `go run ./cmd/spec-drift --base origin/main --head HEAD` — (after spec updates)
- `DEVSTRAP_NO_KEYCHAIN=1 go test ./...` — all packages pass
- `DEVSTRAP_NO_KEYCHAIN=1 go test -race ./internal/...` — all pass

Follow-ups:
- Wire the R2 backend to a real AWS SDK v2 S3 client (the `S3Client` interface is ready; the in-memory double proves the contract).
- Build the full-state snapshot export/import wired to `ErrSnapshotRequired` before enabling retention GC.
- Add Ubuntu CI runner for the XP-01 e2e test (currently runs on macOS; the testscript is platform-portable).
- Re-enable the env blob plane push/pull for env profiles (currently only draft bundles use the blob plane in sync).

## 2026-06-28 — Solo-maintainer OSS branch policy

Changed:
- Updated `AGENTS.md` and `CONTRIBUTING.md` so the trunk-based workflow keeps required PRs, green CI, resolved conversations, linear history, and blocked force-push/deletion while removing branch-gated 1-approval and required CODEOWNERS review until a second active write-access maintainer exists.
- Kept CODEOWNERS advisory for OSS review routing, with external-contributor PRs still requiring maintainer review before merge and maintainer-authored PRs allowed to merge after green CI.
- Updated live GitHub branch protection for `main`: required status checks, strict up-to-date checks, resolved conversations, linear history, and blocked force-push/deletion remain enabled; required approval count is now 0 and required CODEOWNERS review is disabled.

Validated:
- `gh api repos/Reederey87/DevStrap/branches/main/protection` verified required checks and safety gates stayed enabled after the review-policy change.
- `gofmt -w cmd internal`
- `git diff --check`
- `golangci-lint run`
- `go run ./cmd/spec-drift --base origin/main --head HEAD`
- `go test -race ./...` was attempted exactly and failed only in `internal/sync` because the local macOS keychain returned exit status 152 while storing test signing keys; `DEVSTRAP_NO_KEYCHAIN=1 go test -race ./...` passed, matching CI's headless setting.

Follow-ups:
- Re-enable 1 approving review and required CODEOWNERS review after another active maintainer with write access is available.

## 2026-06-28 — Spec/cloud architecture audit rebaseline

Changed:
- Read and reconciled the full `spec/` corpus plus `AUDIT_RECOMMENDATIONS_2026-06-28.md` against live implementation and subagent findings.
- Updated spec/audit docs to mark shipped findings as closed (forge-aware PR routing, no-remote `local_git` classification, env hydration safety, keychain fallback narrowing, agent credential-env stripping), corrected planned-only schema (`device_gitstate`/`00008`), and clarified the next implementation dependency graph.
- Reworked the cloud guidance for Fly.io + Cloudflare R2 + Neon: keep the stack, add R2 immutable event-key/conditional-put/cursor semantics, temporary scoped credentials, Fly runner isolation boundaries, Neon pooled/runtime vs direct/migration DSNs, data-residency/cell planning, cost alerts, and provider alternatives.

Validated:
- `rg` stale-claim sweep across `spec/` and `AUDIT_RECOMMENDATIONS_2026-06-28.md`
- `git diff --check`
- `gofmt -w cmd internal`
- `golangci-lint run` using the CI-pinned `v2.12.0` binary installed under `/tmp` because no local `golangci-lint` was on PATH
- `go run ./cmd/spec-drift --base origin/main --head HEAD`
- `go test -race ./...` was attempted exactly and failed in `internal/sync` because the local macOS keychain returned exit status 152 while storing test signing keys; `DEVSTRAP_NO_KEYCHAIN=1 go test -race ./...` passed, matching CI's headless setting.

Follow-ups:
- Implement `internal/engine` materialization extraction, `internal/hub` interface/conformance, cursor-based sync, eager `sync` materialization, `.devstrapignore` compiler, draft bundles, R2/S3 backend, fail-closed enrollment, and portable `run-loop` in the order captured by the audit addendum.

## 2026-06-28 — Dependabot policy: monthly + grouped

Changed:
- `.github/dependabot.yml`: switched both ecosystems (gomod, github-actions) from `weekly` to `monthly`, and added `groups` so each ecosystem's monthly bumps arrive as a single batched PR instead of many (reduces review churn). `open-pull-requests-limit` left at 5.
- Repo housekeeping this cycle: merged the open dependency PRs into `main` — `actions/checkout` v5→v7 (#5), `golang.org/x/text` 0.36→0.38 (#6), `modernc.org/sqlite` 1.50.1→1.53.0 (#8, rebased), `fsnotify` 1.9→1.10.1 (#9); `go build`/`go mod tidy` clean on `main`.

Validated:
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `.github/dependabot.yml` parses as valid YAML.

Follow-ups:
- `golangci/golangci-lint-action` bump (#7) still open — it edits `.github/workflows/ci.yml`, which the CLI OAuth token cannot merge without the `workflow` scope; merge via the GitHub UI or grant the scope (`gh auth refresh -s workflow`).
- Dependency-only PRs currently trip the spec-drift "mapped spec unchanged" gate (go.mod maps to spec/18's `[**]`), so they need an admin merge; consider exempting dependency manifests / Dependabot authors from that gate in `internal/specdrift`.

## 2026-06-28 — Release pipeline (GoReleaser + RC flow)

Changed:
- CI/release tooling + docs; no `cmd/`/`internal/` code modified.
- Added `.goreleaser.yaml` — cross-compiles macOS + Linux (amd64/arm64) DevStrap binaries, CGO-free (pure-Go `modernc.org/sqlite`), injects `version`/`commit`/`date` into `internal/cli` via `-ldflags`, emits `checksums.txt`, and marks `-rc`/`-beta`/`-alpha` tags as GitHub pre-releases (`release.prerelease: auto`).
- Added `.github/workflows/release.yml` — triggered on `v*` tags, runs GoReleaser (`contents: write`), SHA-pinned checkout/setup-go matching `ci.yml`.
- Added `RELEASING.md` — the release runbook: trunk-based release-candidate → stable flow (`vX.Y.Z-rc.N` pre-release → test the candidate binaries → promote to `vX.Y.Z`), optional `release/vX.Y` branch for stabilization/back-ports, edge install via `@main`, and keeping `main` releasable.
- Updated `spec/14` "Release and upgrade gates" to reference the automated pipeline and the RC pre-release flow.

Validated:
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- The release workflow runs only on `v*` tag pushes; it does not affect PR CI. No release is cut by merging this — releasing is a manual `v*` tag the maintainer pushes when ready.

Follow-ups:
- Pin `goreleaser/goreleaser-action` to a SHA on the next Dependabot bump (currently `@v6`).
- Optional later: Homebrew tap (already in the V1 backlog) and an edge/nightly pre-release channel.

## 2026-06-28 — Trunk-based open-source governance (branch protection + OSS files)

Changed:
- Repo governance / docs only; no `cmd/`/`internal/` code modified.
- Adopted a **trunk-based** branch model: `main` is the single protected default branch; the superseded `dev` branch was deleted. `dev`'s #3 work is fully contained in `main` (superseded by #4) and remains recoverable via PR #3 / the reflog — no work lost.
- Enabled GitHub branch protection on `main`: require a PR with 1 approving review + CODEOWNERS review; required status checks (`Spec drift`, `Go lint`, `Go tests (macos-latest)`, `Go tests (ubuntu-latest)`, `Vulnerability check`) with up-to-date branches; required conversation resolution and linear history; force-pushes and deletions blocked; `enforce_admins=false` so the solo maintainer can still merge.
- Repo merge settings: squash + rebase only (no merge commits), auto-delete head branch on merge; enabled Dependabot automated security fixes.
- Updated `AGENTS.md`, `CONTRIBUTING.md`, and `spec/00_START_HERE.md` to the trunk-based fork-and-PR flow (dropped the `dev`-integration description).
- Added `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1), `.github/ISSUE_TEMPLATE/feature_request.md`, and `.github/ISSUE_TEMPLATE/config.yml`.

Validated:
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- Governance/docs only; Go build/test unaffected.

Follow-ups:
- None.

## 2026-06-28 — Cloud-sync architecture: spec refresh + new audit and provisioning guide (docs only)

Changed:
- Documentation only; no `cmd/`/`internal/` code modified. Encoded the cloud-sync direction across the spec set and added two supporting docs.
- Decisions encoded: file-sync split by content type (repo content via git blobless clone — never the hub; env + non-git/draft via age-encrypted `age_blob:<sha256>` blobs; namespace map via signed HLC event log; `node_modules` rebuilt on hydrate, not synced); eager clone-everything materialization on `devstrap sync` with StrapFS/FUSE deferred; two-plane zero-knowledge `devstraphub` (event log + content-addressed encrypted blob store); Cloudflare R2 as the chosen production hub backend from the start (file-backed backend tests-only, no NAS-first phase) behind a pluggable `Hub` interface; cross-platform core first (macOS + Ubuntu), native daemon/StrapFS deferred; device-revoke re-encryption + secret rotation; fail-closed event verification (SECU-03).
- Updated `spec/00`–`spec/17` (frontmatter `last_reviewed: 2026-06-28`); added `AUDIT_RECOMMENDATIONS_2026-06-28.md` to relevant `tracks_code`; added `spec/19` to the document map.
- New `AUDIT_RECOMMENDATIONS_2026-06-28.md` drives the build: workstreams EAGER-* (eager-clone materialization + sync cursor), DRAFT-* (`.devstrapignore` compiler, encrypted draft bundles, non-git hydrate, node_modules rebuild), HUB-* (pluggable Hub + R2 zero-knowledge backend, fail-closed verification, device-revoke re-encryption, blob GC), XP-* (Ubuntu parity, portable scan/sync loop), SCALE-* (future multi-user: control/data-plane split, R2 per-`workspace_id`, rented microVM runner sandboxes, cell-based scaling), plus an explicit Deferred section.
- New `spec/19_CLOUD_PROVISIONING_GUIDE.md` — register/configure the chosen stack: Cloudflare R2 (storage), Fly.io (compute: control plane + ephemeral Firecracker runner microVMs), Neon (control-plane Postgres) — sign-up, resource creation, least-privilege credentials, DevStrap config via the existing encrypted-secrets path, provisioning order/checklist, credential-custody rules.
- Hosting/scaling decision: Fly.io + Cloudflare R2 + Neon (Railway/Vercel/Hetzner evaluated and rejected; reasons in `spec/03`). The LLM/Claude-API provider for the agent runner is explicitly out of scope of this cycle.

Validated:
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli -run TestEveryCommandIsDocumented` (command-doc drift green; new CLI flags/commands documented as planned)
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- No code changed this cycle, so `gofmt`/`golangci-lint`/`go test -race ./...` were not re-run.

Follow-ups:
- Implement the EAGER-*/DRAFT-*/HUB-* workstreams in a later code cycle (sync materialization + cursor, `.devstrapignore` compiler, encrypted draft bundles, R2 hub backend).
- Reconcile `dev`↔`main` divergence: `origin/dev` is behind `origin/main` and missing the merged #4 audit; this branch was based on `origin/main`.
- SCALE-* (multi-user/SaaS) remains documented-not-built.

## 2026-06-28 — Implement second-pass audit recommendations (P0 + medium severity)

Changed:
- **CI-01**: Pinned `govulncheck@v1.1.4`, moved it to its own `vuln` CI job with `continue-on-error` on PRs, added a daily scheduled run.
- **SECR-01**: `quoteDotenv` now uses POSIX single-quote rendering (literal in every dotenv loader) for values without newlines; multiline values escape `$` and backtick in addition to existing escapes. `looksInterpolated` now flags bare `$VAR` so `$`-containing values require `--literal`.
- **AGEN-02/SECU-02**: Added `childenv.AgentAllowlist()` excluding `SSH_AUTH_SOCK`; `runAgentProcess` uses it instead of `BasicAllowlist`, stripping the live SSH credential capability from agent subprocesses.
- **SECR-04/SECU-01**: `HybridStore.Ensure`/`EnsureSigning` now gate the file fallback on `IsKeychainUnavailable(err)` (exported); a present-but-failing keychain fails closed instead of silently writing a plaintext key. A `slog.Warn` fires when the file fallback is taken.
- **SYNC-05/CODE-01**: `ApplyEvents` now `continue`s after recording a hash-chain-break conflict (was `return err`), so the rest of the batch converges.
- **CODE-02**: Removed volatile `OffsetMS` from persisted `skewConflictDetails` so re-delivered skewed events dedup instead of inserting duplicate conflict rows.
- **SYNC-03**: Added lower-bound HLC validation (`event.HLC <= 0` → quarantine) with an `epochFloorMS` constant.
- **CODE-03**: `Store.WithTx` now uses `defer tx.Rollback()` so a panic inside the closure returns the connection to the single-connection pool.
- **CODE-04**: `writeEnvBlob` uses a named return + deferred Close observation + `file.Sync()` for durability of the encrypted blob.
- **CODE-05**: `state.Open` now takes `ctx context.Context`, uses `db.PingContext(ctx)`, and passes `ctx` to `foreignKeyCheck`; all callers updated.
- **SECR-02**: Hydrated env files now begin with the spec-mandated `# Generated by DevStrap. Do not commit.` header with profile name and timestamp.
- **SECR-05**: `env hydrate` now calls `ensureIgnored` before writing the secret content, so the file is gitignored the instant it exists.
- **SECU-04**: `redact.Writer` now suppresses multi-line PEM private key blocks across line boundaries (BEGIN to `[REDACTED PRIVATE KEY]`, body lines dropped until END).
- **AGEN-01**: `enforceAgentCommandPolicy` now blocks known interpreters/shells/downloaders (sh, bash, python*, node, curl, etc.) under non-yolo policies; `--policy` help text disclaims advisory-only scope.
- **AGEN-04**: Added `ephemeral-ci` to accepted policy profiles; replaced `>` substring check with argv-aware redirection detection.
- **AGEN-05**: `agentTokenLooksSensitive` now includes `credentials.json`, `service-account*.json`, `*.pem`, `*.key`; deny list expanded with `/.kube`, `/.docker`.
- **AGEN-06**: Agent PR body now scrubbed through `redact.Scrub` before forge submission.
- **NOVCS-01**: Scanner classifies no-remote/unvalidated-remote git repos as `local_git` (new `TypeLocalGit` constant) instead of `git_repo`, preventing broken cross-device namespace entries.
- **NOVCS-04**: `createFreshWorktree` preflights `project.RemoteKey == ""` with an actionable error before touching git.
- **FORGE-01**: New `internal/cli/forge.go` with `DetectForge(remoteURL)`, `forgeTokenEnv(kind)`, `createForgePR` routing to `gh`/`glab`/`tea` based on detected forge; unknown forges get graceful degradation (branch + compare URL).
- **FORGE-02**: PR env allowlist is now forge-aware (GH_*/GITLAB_TOKEN/GLAB_*/GITEA_TOKEN/TEA_*/BITBUCKET_*/AZURE_DEVOPS_EXT_PAT).
- **FORGE-03**: `normalizeHostPath` now unifies Azure DevOps SSH (`ssh.dev.azure.com/v3/`) and HTTPS (`dev.azure.com/_git/`) forms to `dev.azure.com/org/proj/repo`.
- **SECU-03**: `verifyEventSignature` now requires valid signatures from known approved devices for destructive event types (`project.deleted`, `project.renamed`); unknown devices and non-local devices without signing keys are rejected for these types.
- **SECU-05**: `devices enroll --approve` now requires `--signing-public-key`.
- **SYNC-01**: Same-remote `project.added`/`updated` now checks HLC-dominance before upserting; a stale event (stored coords dominate incoming) is a no-op so convergence is deterministic.
- **GIT-01**: `repoLockIsStale` now treats same-host liveness as authoritative over age; a live PID is never declared stale regardless of `acquired_at`.
- **CLI-02**: `scan --quarantine` progress lines now go to stderr, preserving valid JSON on stdout.
- **CLI-03**: `run` and `agent run` now propagate child exit codes as `100+N` (new `childExitBase`).
- **CLI-04**: Added `exitUsage = 10` for bad-flag/missing-flag/arg-count errors.
- **PROD-01**: Added `deriveDisplayStatus` function mapping materialization+dirty states to user-facing labels; `status` output uses it.
- **PROD-02**: New `devstrap conflicts` command listing open conflicts; `status` shows open-conflict count.
- **DATA-01**: `Backup()` now validates the backup with `PRAGMA quick_check` + `foreignKeyCheck` after `VACUUM INTO`; removes the partial backup on failure.
- **TEST-04**: Removed gosec `includes` allowlist (all rules now run), added `errorlint`, set `max-same-issues: 0`; fixed two `errorlint` findings in `platform.go`.
- **ARCH2-04**: Added `// reserved for M5 daemon` comment on `exitDaemonUnavailable`.
- Updated affected tests for new quoting, interpreter blocking, signing-key requirement, forge-agnostic PR, and context-threaded `Open`.

Validated:
- Exa best-practice research for dotenv safe quoting (single-quote literal), SSH agent forwarding risks, keychain fail-closed patterns, govulncheck CI pinning, and forge abstraction (git-pkgs/forge precedent).
- `gofmt -w cmd internal`
- `go build ./...`
- `go vet ./...`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run` (0 issues)
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test ./...`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Remaining audit items not yet implemented: ARCH2-01 (extract `internal/engine`), ARCH2-02 (wire sync cursors), SYNC-02 (collapse HLC implementations), SYNC-04 (wire event_delivery), GIT-02..05, CLI-01 (thread --json through all commands), CLI-05 (daemon socket security spec), PROD-03 (draft limits), PROD-04/05 (spec re-baselining), DATA-02..06, TEST-01..03/05/06, PLAT-01..05, CODE-06, SECR-03/06, NOVCS-02/03/05, FORGE-04/05, and all spec-only fixes (ARCH2-03/05, etc.).
- Update all `spec/` files to reflect implemented changes (deferred to spec review pass).

## 2026-06-27 — Second-pass design & implementation audit + full spec refresh

Changed:
- Added `AUDIT_RECOMMENDATIONS_2026-06-27.md` (repo root): a second-pass audit with Executive Summary, Priority Matrix, and 6 sections — CI/CD (`CI-01`), non-VCS/remote-less projects (`NOVCS-01..05`), non-GitHub forges (`FORGE-01..05`), 65 verified cross-dimension findings across 12 dimensions (incl. `ARCH2-*`), cross-machine working-state sync design (3-layer git-native plane), and zero-knowledge sync-hub architecture & services. Findings carry file:line evidence, examples, and actionable steps.
- Updated every `spec/` file to incorporate the new ideas and correct drift: `00` (phases = capability grouping, current position, plane separation), `01` (Alternatives F/G; reject continuous file-sync; architecture rules 7–8), `03` (engine seam `ARCH2-01`, hub HTTP/SSE, reconciler wording `ARCH2-04`), `04` (file-sync rejection + working-state/non-VCS/forge challenges), `07` (`local_git` type + content-sync table, `repo.gitstate.observed`/`repo.wip.pushed` events, working-state plane, HTTP/SSE wire protocol, cursor status `ARCH2-02`), `08` (forge-agnostic provider section, remote-less preflight `NOVCS-04`, WIP-ref base prohibition), `09` (`SECR-01/02/05` hydration safety), `10` (agent-isolation reality `AGEN-01..06`/`SECU-02`, forge-agnostic PR), `12` (`device_gitstate` table, `git_repos` remote-key constraint, dead-table notes), `14` (audit follow-ups + workstreams), `15` (agent/hub reality `SECU-01/03`, audit-log-unimplemented note), and targeted follow-up sections in `02/05/06/11/13/16/17`. Added ADR `0002-working-state-sync.md`. Bumped `last_reviewed` to 2026-06-27.
- No Go code changed this cycle (audit + specs only).

Validated:
- Exa best-practice research across the 12 audit dimensions plus dedicated working-state-sync and sync-architecture design workflows (git-corruption/file-sync consensus, HLC/CRDT, age/SOPS, forge abstraction, SSE/transport, zero-knowledge hub).
- `go run ./cmd/spec-drift --base origin/main --head HEAD`; `go build ./...`; `go test ./...` (unaffected — no code change).

Follow-ups:
- P0 implementation: agent isolation hardening (`AGEN-01/02`), secret-hydration escaping (`SECR-01`), key-custody fallback narrowing (`SECR-04`), forge-agnostic PR (`FORGE-01`), no-remote classification (`NOVCS-01`), CI `govulncheck` pinning/split (`CI-01`).
- Build the working-state validation plane (Layer A) and wire the sync cursor (`ARCH2-02`).
- The spec-update pass was done via direct edits because subagent workflows were session-rate-limited at the time; a workflow re-pass can refine after the reset.

## 2026-06-26 — Audit recommendations: security, sync, git, secrets, tests, specs

Changed:
- Added `internal/redact`: a `Secret` capability type (String/GoString/MarshalText/MarshalJSON/LogValue all render `[REDACTED]`, single `Reveal` boundary), `URL`/`StripURLUserinfo` helpers, a token-shape `Scrub`, a value `Redactor`, and a line-buffering scrubbing `Writer` (ENV-2/SEC-3). Wired it into sync event remote-URL stripping, CLI error printing, the persisted agent log stream, and slog value-level redaction.
- Hardened the scan→adopt→hydrate boundary: scan only persists validated remotes (SEC-1); escaping symlinks are typed (`ErrEscape`/`ErrDangling`), hard-excluded, and conflict-recorded, with use-time revalidation (`pathkey.VerifyWithinRoot`) before hydrate/worktree materialization (SEC-4); added `scan --quarantine` to move secret-looking files into a dated `0600` quarantine (SEC-6).
- Implemented layered default-branch resolution (`ResolveDefaultBranch` with `remote set-head --auto` repair + typed source; `RemoteDefaultBranch` via `ls-remote --symref`), used authoritatively by `worktree new` with a non-authoritative warning (GIT-2).
- Wired the HLC clock-skew guard into `ApplyEvents`: far-future remote events are quarantined as `untrustworthy_remote_time` conflicts (not applied, batch continues) and accepted events advance the local clock via `ReceiveRemoteHLC` (SYNC-3).
- Implemented `project.renamed` (re-key with target-collision conflict), delete-vs-dirty (`pending_delete_conflict` instead of destroying a dirty checkout), and `GCTombstones` (SYNC-5).
- Hardened `worktree cleanup` (distinguish dirty-state errors from dirty trees, skipped count, `--force`) (GIT-3); added `worktree unlock <path>` + `doctor` lock reporting with `readRepoLock`/`clearRepoLock` helpers (SEC-5/OP-UNLOCK/OP-DOCTOR-LOCK).
- Added `secret_bindings.needs_rotation` (migration 00007), `MarkEncryptedBindingsNeedingRotation`/`CountSecretBindingsNeedingRotation`, device revoke/lost rotation flagging, and `doctor` reporting (ENV-4).
- Added a `DEVSTRAP_NO_KEYCHAIN` platform gate forcing the file-backed key store for headless/CI and hermetic e2e tests.
- Added tests: scan classification + unvalidated-remote + quarantine (TEST-1), pathkey case/symlink/verify (TEST-2), worktree HEAD/base-SHA + stale-local assertions (TEST-3), JSON-contract unmarshal assertions (TEST-5), HLC backward-clock/tick/concurrency (SYNC-1/TEST-7), git timeout/ResolveDefaultBranch/DirtyState (GO-1/GIT-2/GIT-6), logger no-ctx + token scrub (GO-6), sync skew/rename/delete-vs-dirty/GC, redact unit tests, and a `testscript` end-to-end harness covering `cmd/devstrap` through the real binary (TEST-6).
- Added a `spec/13` command-doc drift test (SPEC-5), `spec/adr/0001-product-naming.md` (SPEC-3), an `internal/sync/doc.go` spike note (ARCH-2), and spec updates for naming, branch workflow, status JSON, no-daemon guarantee, roadmap gates, single-writer/manifest-hub notes, and the newest-first work-log rule (SPEC-2/3/4/6, ARCH-1/2).
- Hardening from CI/review: a review subagent caught and fixed two real bugs — `StripURLUserinfo` was dropping the ssh `git@` login (would break peer clones) and `VerifyWithinRoot` rejected nested not-yet-created hydration targets; added a `git` `WaitDelay` backstop and broadened keychain-unavailable detection so a missing Secret Service degrades to the file store; set `DEVSTRAP_NO_KEYCHAIN=1` in the CI test job; and bumped the Go toolchain `1.25.7 -> 1.26.4` to clear pre-existing stdlib CVEs that `govulncheck` flagged in CI (code is not affected on 1.26.4).

Validated:
- Exa best-practice research for slog redaction capability types, git default-branch resolution (`ls-remote --symref` / `set-head --auto`), HLC receive/skew semantics, CRDT rename/move, Go 1.24 `os.Root` TOCTOU defense, OWASP secret quarantine, SOPS+age revocation/value rotation, and `rogpeppe/go-internal` testscript.
- `gofmt -w cmd internal`
- `go build ./...`, `go vet ./...`
- `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- `go mod tidy`

Follow-ups:
- Implement automatic remote device enrollment, fingerprint confirmation, and bundle re-encryption (envelope rewrap to remaining recipients + superseded-blob deletion) on revoke.
- Add OS-enforced agent sandboxing/project-env allowlists and non-generic agent engine adapters.
- Implement service installers and evaluate a native FSEvents watcher; wire tombstone GC to approved sync-cursor watermarks once cursor tracking lands.
- Accepted-for-now (raised in review): the local `git_repos.remote_url` keeps any embedded credential so local hydrate still works with credential-in-URL setups (the synced event strips it, so no cross-device/hub leak); and the agent-log scrubber is line-buffered, so a multi-line PEM body echoed by a tool is only header-matched in the owner-only `0600` log.

## 2026-06-24 — Audit hardening and spec refresh

Changed:
- Hardened the SQLite state layer with DSN pragmas, WAL, busy timeout, single-writer pool, secure DB permissions, backup/status helpers, and reversible event-ordering migrations.
- Added `devstrap db migrate/status/backup/down` and tests for CLI, config, state, migration, and status behavior.
- Hardened CI with read-only permissions, SHA-pinned actions, vet/build/race tests, vuln scanning, module hygiene, and a guard against vacuous package tests.
- Renamed the repository default branch to `main`, removed legacy branch-name references, and documented local clone update steps.
- Reviewed and updated all files under `spec/` for current implementation state, local-first sync design, SQLite behavior, secrets/security, platform service behavior, scan scale, release/backup gates, and future web/admin surface best practices.

Validated:
- `gofmt -w cmd internal`
- `go vet ./...`
- `go build ./...`
- `go test ./...`
- `go test -race ./...`
- `go mod tidy`
- `git diff --check`
- spec stale-reference sweeps for old branch/worktree/test/security wording.

Follow-ups:
- (done in later cycles) scanner/adoption workflow, real generated device IDs, and the structured `slog` redaction choke point.
- Keep this work log updated at the end of each code-modifying agent cycle.

## 2026-06-24 — Work-log process requirement

Changed:
- Added this tracking file.
- Updated `AGENTS.md` to require concise end-of-cycle summaries in this file after codebase-modifying work.
- Updated `AGENTS.md` to require a final spec-folder review/update after the last codebase modification in a session.
- Added this file to the `spec/00_START_HERE.md` document map.

Validated:
- `git diff --check`

Follow-ups:
- None.

## 2026-06-24 — Scan, Git hydration, sync spike, and worktrees

Changed:
- Added redacted structured logging, generated `dev_<uuidv7>` IDs, stable local device persistence, namespace path normalization, and expanded state-store methods for projects, events, conflicts, and worktrees.
- Implemented `devstrap scan`, `add`, `hydrate`, `open`, and `worktree new/status/list/remove/cleanup`, including skeleton directories, partial clone by default, local bare remote support, dirty-target refusal, duplicate remote reporting, and fresh upstream worktree base resolution.
- Added `internal/git`, `internal/scan`, `internal/pathkey`, and `internal/sync` primitives for remote normalization, bounded scanner pruning, HLC ordering, idempotent event replay, conflict creation, and a file-backed test hub.
- Updated README and affected specs to reflect implemented commands and remaining daemon/env/hub follow-ups.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./...`
- `GOCACHE=/tmp/devstrap-gocache go vet ./...`
- `GOCACHE=/tmp/devstrap-gocache go build ./...`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- `GOCACHE=/tmp/devstrap-gocache go mod tidy`
- `git diff --check`

Follow-ups:
- Add a user-facing `devstrap sync` command, remote device registration/approval, full snapshot fallback, and real skeleton reconciliation across roots.
- Enforce stale-base checks before PR/finalization workflows once that lifecycle command exists.
- Implement env run, daemon/watchers, and agent runner policy enforcement.

## 2026-06-24 — Git and HLC audit hardening

Changed:
- Hardened git subprocess execution with bounded default timeouts, disabled prompts, sanitized environment, protocol policy, remote URL validation, `--` separators for clone/worktree add, explicit default-branch errors, and URL credential redaction in git errors.
- Added `devstrap worktree status <id>` plus reusable base-drift detection that re-fetches the recorded base ref and reports fresh vs stale state.
- Reworked the HLC implementation to use a mutex, explicit physical/logical packing, 16-bit logical overflow handling, and max-skew rejection.
- Updated README and affected specs after reviewing the spec folder for stale command, HLC, security, test-plan, and roadmap text.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/git ./internal/sync ./internal/scan ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/git ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- spec stale-text sweep for worktree status, default-branch fallback, logger context, and HLC wording.

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Local age device identity

Changed:
- Added `filippo.io/age` and a local `internal/devicekeys` file-backed age X25519 identity store.
- `devstrap init` now ensures the local device has an age identity, persists only the public recipient in `devices.public_key`, and keeps the private identity in `~/.devstrap/keys/<device_id>.agekey` with mode `0600`.
- `devstrap doctor` now reports whether the local age public recipient and age private identity match.
- Threaded device `public_key` through the state API and added a public-key update method.
- Added coverage for key generation/reuse, init persistence, doctor status, and absence of the private identity from `state.db` and `config.yaml`.
- Updated affected secrets, threat-model, data-model, platform, CLI/API, roadmap, start-here, and test-plan specs.

Validated:
- Exa best-practice research for age X25519 identity generation and protected secret-key storage.
- `go mod tidy`
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/devicekeys ./internal/config ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-24 — Transactional sync event apply

Changed:
- Added a state transaction facade and switched sync replay to claim each event and apply project/conflict side effects in the same SQLite transaction.
- Event insertion now computes/verifies payload content hashes, treats exact duplicate deliveries as no-ops, rejects divergent reuse of an event ID, and stores absent per-device sequence numbers as `NULL` instead of colliding on `seq=0`.
- Made conflict insertion idempotent for identical open conflicts and added deterministic `(hlc, device_id, id)` event ordering in the store, sync replay, and file-backed hub.
- Removed mutable delivery/apply state from the insert-only `events` schema; delivery state remains in `event_delivery`.
- Updated affected specs after reviewing sync/data schema text and test-plan coverage.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/sync`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-24 — Unicode path and scan safety coverage

Changed:
- Normalized namespace paths to Unicode NFC before validation, display storage, and case-folded key generation.
- Added direct `internal/scan` coverage for generated-directory pruning, secret-looking filename warnings, symlink escape warnings, duplicate remote reporting, and avoiding descent into pruned Git repos.
- Made `golang.org/x/text` a direct dependency for Unicode normalization.
- Updated affected specs after reviewing path normalization, scanner safety, and test-plan text.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/pathkey ./internal/scan`
- `GOCACHE=/tmp/devstrap-gocache go mod tidy`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-24 — Repo operation lock hardening

Changed:
- Replaced bare repo lockfiles with metadata-backed lock records containing PID, hostname, and acquisition time.
- Added stale lock recovery for dead same-host owners and over-age lockfiles, with double-read removal before deleting stale markers.
- Put `hydrate` under the per-project repo operation lock and changed `worktree new` to hold the same lock through hydrate, fetch, default-branch update, and worktree creation.
- Added CLI tests for active lock refusal and stale-owner reclamation.
- Updated affected specs and reconciled older work-log follow-ups that still listed repo lock extension as remaining.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Stale-base finalization gate

Changed:
- Added `devstrap worktree finalize <id>` as the pre-PR/handoff freshness gate.
- Reused the recorded worktree `base_ref`/`base_sha` drift check so finalization exits with a conflict when the remote base moved unless `--allow-stale-base` is explicitly passed.
- Extended the worktree integration test to assert stale-base refusal and explicit stale override.
- Updated README and affected Git, CLI/API, roadmap, start-here, and test-plan specs.

Validated:
- Exa best-practice research for keeping PR branches up to date with the base branch before merge.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-24 — Open command and init detection hardening

Changed:
- Changed `devstrap open` to start `cursor`/`code` without binding the editor lifetime to the CLI context and to release the child process handle after launch.
- Replaced uninitialized SQLite detection based on `"no such table"` string matching with explicit `sqlite_master` table checks.
- Expanded state tests so summary, current-device, and project-list reads all return `ErrNotInitialized` before migrations.
- Updated affected CLI/API and test-plan specs.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli ./internal/state`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Git LFS policy for agent worktrees

Changed:
- Threaded `git_repos.lfs_policy` through project reads and added `devstrap add --lfs-policy auto|never|agent|always`.
- Added `.gitattributes`-based Git LFS detection and policy-aware agent worktree handling: `agent`/`always` runs `git lfs pull`; `auto`/`never` warns about pointer files.
- Preserved existing LFS policy when project metadata upserts omit the field.
- Added Git, state, and CLI coverage for LFS detection, policy preservation, invalid policy rejection, and agent worktree pointer warnings.
- Updated README and affected Git, CLI/API, roadmap, start-here, and test-plan specs.

Validated:
- Exa best-practice research for Git LFS skip-smudge/pull behavior and pointer-file warnings.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/git ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-24 — Persisted local event clock

Changed:
- Added `device_sync_state` to persist each local device's last HLC and next sequence number.
- Added `Store.InsertLocalEvent`, which stamps local events with the current local device, monotonic HLC, and per-device sequence in the same SQLite transaction that inserts the event.
- Added `sync.CreateProjectEvent` for local project events so callers no longer need to supply HLC/sequence manually.
- Seeded missing local clock state from existing max local `events` rows to avoid timestamp or sequence regression after restart or partial state loss.
- Added state and sync tests for persisted HLC/sequence behavior across reopen and for local project event creation.
- Updated affected specs and reconciled work-log follow-ups that still listed persisted HLC state as remaining.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/sync`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-24 — HLC-gated project tombstones

Changed:
- Implemented `project.deleted` replay as an HLC-stamped namespace tombstone instead of an immediate purge.
- Added tombstone checks so older `project.added`/`project.updated` events are ignored and newer add/update events restore the project.
- Reset tombstones when a newer active project event wins.
- Added sync replay coverage for delete plus older/newer restore ordering.
- Updated affected specs and reconciled work-log follow-ups that still listed tombstone/delete semantics as remaining.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/sync`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Order-independent path conflict reconcile

Changed:
- Added namespace source-event metadata so sync replay can compare the event coordinates that currently own an active namespace entry.
- Made same-path/different-remote replay deterministic across pull windows by selecting the lowest `(hlc, device_id, event_id)` winner and writing a stable conflict keyed by the unordered remote-key pair.
- Added commutativity and late-arriving-winner tests, plus an open-conflict read helper for assertions.
- Updated affected sync, SQLite schema, roadmap, start-here, and test-plan specs.

Validated:
- Exa best-practice research for deterministic local-first conflict resolution.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/sync`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Local event signatures

Changed:
- Added local Ed25519 device signing identities alongside age recipients, storing only the signing public key in SQLite and private signing material in the `0600` key directory.
- Signed locally inserted events over canonical event fields and verified signed event inserts when the source device signing public key is known.
- Extended `devstrap init` and `doctor` to provision and check both age and signing identities.
- Added state, CLI, and device-key coverage for signing key persistence, signature reuse, tamper rejection, and absence of private signing material from `state.db`/`config.yaml`.
- Updated affected data model, security, env/device trust, roadmap, API, platform, start-here, and test-plan specs.

Validated:
- Exa best-practice research for Go Ed25519 signing/verification and deterministic message bytes.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/devicekeys ./internal/state ./internal/sync ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Shared child environment sanitizer

Changed:
- Added `internal/childenv`, an allowlist-based child process environment builder with non-overridable dangerous-name stripping for dynamic linker, interpreter, shell, and Git injection variables.
- Wired Git subprocesses through the shared sanitizer and centralized Git protocol policy on every invocation with an explicit protocol allowlist, denied `ext`, disabled prompts, isolated global/system config, and controlled SSH batch mode via `core.sshCommand`.
- Wired editor launches through the same sanitized child environment and separated path arguments with `--`.
- Added focused child-env and Git tests for inherited-secret stripping, dangerous variable blocking, controlled Git env, and secure argument policy.
- Updated affected start-here, env, agent policy, security, test-plan, and README docs.

Validated:
- Exa best-practice research for Go `os/exec` environment handling and allowlist-based dangerous env stripping.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/childenv ./internal/git ./internal/cli`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Worktree stale-remove prune

Changed:
- Added Git runner helpers for `git worktree remove` and `git worktree prune`.
- Added project lookup by namespace ID so worktree removal can run from the main checkout instead of the linked worktree path.
- Added `devstrap worktree remove <id> --force`; missing manually deleted worktree paths now require force, run `git worktree prune`, and mark the DB row removed.
- Updated `cleanup --merged` to prune missing stale worktree paths and remove their active DB rows.
- Extended CLI coverage for manually deleted worktree removal and updated affected Git/worktree, CLI/API, agent policy, roadmap, and test-plan specs.

Validated:
- Exa best-practice research for `git worktree remove`, `--force`, and `git worktree prune` behavior.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/git ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Encrypted env capture

Changed:
- Added `internal/envfile`, a side-effect-free dotenv parser with explicit grammar, optional `export`, quoted/unquoted comment handling, dangerous-name rejection, size guard, and interpolation refusal unless literal mode is explicit.
- Added `internal/envbundle` to age-encrypt parsed env bindings to device recipients and return content-addressed `age_blob:<sha256>` refs.
- Added `devstrap env capture <path> <env-file> [--literal] [--profile]`; capture reads the env file once, encrypts in memory to the local age recipient, writes a `0600` ciphertext blob under `~/.devstrap/blobs`, stores only encrypted refs in SQLite, and gitignores captured files inside the project.
- Added state helpers for saving and reading captured encrypted env profiles and bindings.
- Added parser, encryption, state, and CLI coverage proving plaintext values are absent from `state.db`, `config.yaml`, and ciphertext blobs.
- Updated affected README, start-here, secrets/env, CLI/API, roadmap, and test-plan specs.

Validated:
- Exa best-practice research for side-effect-free dotenv parsing, no interpolation by default, size guards, and age recipient encryption.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/envfile ./internal/envbundle ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Encrypted env hydrate

Changed:
- Added age bundle decrypt support and `devstrap env hydrate <path> --write <file> [--force]`.
- Hydrate resolves the captured encrypted env profile, reads the local `age_blob:<sha256>` ciphertext, decrypts it with the local device age identity, renders dotenv output, writes atomically with mode `0600`, refuses existing targets unless `--force`, and gitignores the hydrated target.
- Extended env CLI coverage for hydrate success, `0600` file mode, gitignore updates, overwrite refusal, and forced overwrite.
- Updated affected README, start-here, secrets/env, CLI/API, Mac/Linux, threat-model, roadmap, test-plan, and work-log specs.

Validated:
- Exa best-practice research for secure env file hydration, explicit writes, `0600` permissions, `.gitignore`, and overwrite caution.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/envbundle ./internal/envfile ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- `git diff --check`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Platform adapter seams

Changed:
- Added `internal/platform` with concrete interfaces for watcher, service manager, keychain, and editor launch.
- Added build-tagged platform detection, a polling watcher fallback, explicit unsupported service/keychain placeholders, and a platform-owned runtime OS/arch helper.
- Routed `devstrap open` through the platform editor adapter while preserving detached launch and sanitized child environment behavior.
- Added platform tests for detection, polling watcher cancellation, unsupported-adapter sentinel errors, and a source guard that keeps `runtime.GOOS` branching inside `internal/platform`.
- Updated affected README, start-here, architecture, Linux, roadmap, and test-plan specs.

Validated:
- Exa best-practice research for Go platform-specific adapters, build tags/runtime checks, fsnotify watcher limits, and user service manager abstractions.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/platform ./internal/pathkey ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Generated workspace identity

Changed:
- Replaced store/query hardcoding of the local workspace id with a generated, persisted `ws_` workspace identity.
- Added migration `00006_workspace_singleton.sql` to enforce the Phase 0 single-workspace invariant at the database layer.
- Threaded the resolved workspace id through state transactions, project queries, tombstones, env profiles, conflicts, and event insertion.
- Removed `ws_local` from sync test event construction so local apply uses the persisted workspace identity.
- Added coverage for generated workspace id persistence, singleton rejection, concurrent `EnsureWorkspace`, and event workspace inheritance.
- Updated affected start-here, namespace/sync, SQLite data-model, and test-plan specs.

Validated:
- Exa best-practice research for globally unique TEXT ids and explicit workspace/tenant scoping in local-first SQLite sync designs.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/sync ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run/provider mode, OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-25 — CI lint and gosec gate

Changed:
- Added root `.golangci.yml` enabling `errcheck`, `gosec`, `govet`, `ineffassign`, `staticcheck`, and `unconvert`.
- Added a separate pinned `golangci-lint-action` CI job using golangci-lint v2.12.
- Fixed lint findings for unchecked close/rollback paths, stale test assignments, and staticcheck control flow/style issues.
- Added narrow `nolint:gosec` annotations for intentionally variable git/editor subprocesses, managed repo-lock paths, user-selected env paths, and public project skeleton files.
- Documented `golangci-lint run` in `AGENTS.md`, `CONTRIBUTING.md`, and the Phase 0 test-plan gate; added lint references to `spec/17_REFERENCES.md`.

Validated:
- Exa best-practice research for the official golangci-lint GitHub Action, v2 exclusion syntax, and gosec/errcheck/staticcheck configuration.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli ./internal/state ./internal/git ./internal/platform`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run/provider mode, OS keychain/approval, native watcher/service adapters, agent policy enforcement, spec drift gates, and future PR command integration.

## 2026-06-25 — SQLite foreign-key integrity checks

Changed:
- Added state-store startup assertions for `PRAGMA foreign_keys = 1` and `PRAGMA foreign_key_check`.
- Added `Store.ForeignKeyCheck` and surfaced `sqlite foreign_key_check: ok` in `devstrap db status` and `doctor`.
- Added coverage proving `Open` rejects a database with pre-existing FK violations and that CLI status/doctor output includes FK integrity status.
- Updated affected start-here, data-model, CLI/API, and test-plan specs.

Validated:
- Exa best-practice research for SQLite per-connection `foreign_keys`, `foreign_key_check`, and Go SQLite DSN/pool setup.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Active namespace partial index

Changed:
- Added migration `00005_namespace_active_index.sql` with `idx_namespace_active` on `(workspace_id, path_key)` for active namespace entries.
- Added EQP coverage proving the `ListProjects` query uses `idx_namespace_active` and avoids a temporary ORDER BY b-tree.
- Updated migration version expectations and CLI DB status expectations for schema version 5.
- Updated affected SQLite data-model and test-plan specs.

Validated:
- Exa best-practice research for SQLite partial indexes, exact predicate matching, ORDER BY index use, and `EXPLAIN QUERY PLAN`.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Collision-resistant worktree branches

Changed:
- Changed worktree branch names to include UTC date/time plus a dedicated random hex suffix.
- Added bounded retry around `git worktree add -b` when Git reports that the generated branch already exists.
- Added focused coverage that forces a first-attempt branch collision and verifies suffix regeneration in the same second.
- Updated affected Git/worktree, agent policy, and test-plan specs.

Validated:
- Exa best-practice research for `git worktree add -b` branch-exists behavior and worktree branch ownership safeguards.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Sortable state timestamps

Changed:
- Replaced state-layer second-precision timestamp writes with fixed-width UTC nanosecond text.
- Added a stable `id` tiebreaker to `ListWorktrees` ordering for same-timestamp rows.
- Added state coverage for lexically sortable fixed-width timestamp formatting and deterministic same-timestamp worktree listing.
- Updated affected SQLite data-model and test-plan specs.

Validated:
- Exa best-practice research for Go timestamp formatting, `RFC3339Nano` ordering caveats, and SQLite TEXT datetime ordering.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, native watcher/service adapters, agent policy enforcement, previous-event hash-chain validation, and future PR command integration.

## 2026-06-25 — Event previous-hash chain validation

Changed:
- Linked local events to the previous same-device event content hash before signing.
- Added insert-time validation for non-empty `prev_event_hash` values against the previous stored same-device event.
- Added sync conflict recording for incoming `event_hash_chain_break` failures while keeping the broken event unapplied.
- Added focused state/sync coverage for local previous-hash linking, broken chain rejection, conflict recording, and successful replay after repairing the previous hash.
- Updated affected namespace/sync, SQLite data-model, and test-plan specs.

Validated:
- Exa best-practice research for append-only hash chains, canonical signed fields, tamper evidence, and chain verification.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/sync`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: env run, OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-25 — Atomic hydrate promotion

Changed:
- Changed hydrate to clone into a hidden sibling temp directory and promote it only after clone success plus a second target validation.
- Clone failures now leave the original skeleton untouched and clean staged temp directories.
- Added coverage for missing-remote hydrate preserving the skeleton and promotion-time dirty-target refusal.
- Updated affected start-here, Git/materialization, CLI/API, and test-plan specs.

Validated:
- Exa best-practice research for same-directory temp staging, rename promotion, and cleanup semantics.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- `git diff --check`

Follow-ups:
- Continue remaining audit items: env run/provider mode, OS keychain/approval, native watcher/service adapters, agent policy enforcement, spec drift gates, typed git errors/retries, and future PR command integration.

## 2026-06-25 — Spec drift gate

Changed:
- Added frontmatter to every `spec/*.md` file with `last_reviewed` and `tracks_code` metadata.
- Added `internal/specdrift` plus `cmd/spec-drift` to validate spec frontmatter, map changed code/config paths to tracked spec files, and require `spec/18_WORK_LOG.md` on code/spec/doc changes.
- Added a separate CI `spec-drift` job with full checkout history and default-branch fetch before running the gate.
- Extended `CODEOWNERS` to cover the full `spec/` directory and updated affected agent, contribution, start-here, roadmap, reference, and test-plan docs.

Validated:
- Exa best-practice research for CI documentation drift gates, changed-file classification, and path-filter tradeoffs.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/specdrift`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- `git diff --check`

Follow-ups:
- Continue remaining audit items: env run/provider mode, OS keychain/approval, native watcher/service adapters, agent policy enforcement, typed git errors/retries, and future PR command integration.

## 2026-06-25 — Env provider refs and runtime injection

Changed:
- Added `devstrap env bind <path> <refs-file> --provider 1password` to store only `op://` provider references and gitignore refs files inside projects.
- Added top-level `devstrap run <path> -- <command>` for encrypted local profiles and 1Password reference profiles.
- Encrypted runs decrypt age bundles into subprocess env only; provider runs write a temporary `0600` refs file and delegate to `op run --env-file <refs> -- <command>`.
- Added state and CLI wiring for provider env profiles, plus integration coverage for encrypted runtime env and provider-ref delegation through a fake `op`.
- Updated affected start-here, secrets/env, CLI/API, roadmap, test-plan, references, and README docs.

Validated:
- Exa best-practice research for `op run` runtime-scoped secret injection, `op --env-file` reference files, least-privilege service account guidance, and `op inject --file-mode 0600`.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: OS keychain/approval, native watcher/service adapters, agent policy enforcement, typed git errors/retries, and future PR command integration.

## 2026-06-25 — Typed Git errors and transient retry

Changed:
- Added typed Git sentinels for network, authentication, branch-not-found, and missing-remote failures.
- Changed `clone` and `fetch` to retry transient network failures only, leaving auth, branch, and remote errors non-retried.
- Mapped typed Git errors to existing CLI exit codes for auth, network, and generic Git failures.
- Normalized explicit SSH ports out of `ssh://` and scp-like remotes, and tightened malformed scp-like remote validation.
- Added Git tests for typed classification, retry/no-retry behavior, port normalization, and CLI exit-code mapping.
- Updated affected start-here, Git/materialization, and test-plan specs.

Validated:
- Exa best-practice research for Git clone/fetch remote behavior, protocol helper invocation, and transport boundaries.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/git ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`
- `git diff --check`

Unable:
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run` could not be rerun because the escalation approval system rejected the command due an external usage limit, and no local `golangci-lint` binary is installed.

Follow-ups:
- Continue remaining audit items: OS keychain/approval, native watcher/service adapters, agent policy enforcement, and future PR command integration.

## 2026-06-26 — Provider env file hydration

Changed:
- Extended `devstrap env hydrate` so 1Password provider profiles resolve `op://` refs through `op inject` into a temporary `0600` file, then install the requested target through the existing atomic write and overwrite guard.
- Added CLI coverage with a fake `op` for provider `run`, provider `hydrate`, output mode `0600`, `.gitignore` updates, and overwrite refusal before secrets are resolved.
- Added `devstrap agent run/list/show/pr` for the thin generic runner: fresh worktree creation, sanitized no-secret command env, wrapper-level command policy profiles, `0600` logs, persisted `agent_runs`, Git status/diff summaries, and stale-base-gated PR dry-run/create flow.
- Added integration coverage for agent policy denial, run metadata, log capture, diff summary, and `agent pr` stale-base refusal/override.
- Added `devstrap devices list/approve/revoke/lost/rename`, state helpers for device trust-state changes, local-device revocation refusal, and CLI coverage for list/rename/refusal behavior.
- Added `devstrap sync --hub-file` for the file-backed test hub, including namespace-only/dry-run output and CLI coverage for the dry-run path.
- Stabilized fake-Git error-classification tests under race instrumentation by increasing their test-only subprocess timeout.
- Re-sequenced the roadmap/spec recommendation so the thin agent runner milestone follows the fresh worktree manager instead of waiting behind daemon, Linux, and hub work.
- Added the trust-plane dependency gate before hub encrypted-blob sync and clarified that device revocation requires secret value rotation in addition to bundle re-encryption.
- Updated README and affected start-here, architecture, Mac/Linux, secrets/env, agent, security, SQLite data-model, CLI/API, roadmap, and test-plan specs after reviewing the spec folder.

Validated:
- Exa best-practice research for 1Password `op inject --file-mode 0600`, `op run --env-file`, and provider-reference workflows.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli ./internal/state`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./internal/git ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Continue remaining audit items: OS keychain/approval, native watcher/service adapters, agent file policy enforcement, non-generic agent adapters, and real PR execution smoke tests with `gh`.

## 2026-06-26 — Agent file policy and native watcher hardening

Changed:
- Added wrapper-level agent file path policy for non-`yolo-local` runs, denying explicit sensitive-path and outside-worktree arguments before the generic agent command executes.
- Added non-dry `devstrap agent pr` coverage with a fake `gh` executable to verify `gh pr create` receives the expected base/head/title/body argv after the stale-base gate.
- Added an fsnotify-backed Darwin/Linux watcher adapter that recursively watches directories, skips generated trees, debounces bursty filesystem events, and emits reconciliation hints through the existing platform interface.
- Updated README and reviewed/updated every spec file so implementation status, roadmap items, security notes, test plan, and references reflect the new agent policy and watcher behavior.

Validated:
- Exa best-practice research for OS keychain storage, fsnotify watcher semantics/debouncing, agent sandbox/file policy layering, and hermetic `gh pr create` testing.
- `gofmt -w cmd internal`
- `go test ./internal/cli ./internal/platform`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Implement automatic remote device enrollment, fingerprint confirmation, and bundle re-encryption hooks.
- Add OS-enforced agent sandboxing/project-env allowlists and non-generic agent engine adapters.
- Implement service installers and evaluate whether the Darwin fsnotify/kqueue watcher should be replaced by a native FSEvents adapter.

## 2026-06-26 — OS keychain-backed device identities

Changed:
- Added a platform `SystemKeychain` adapter backed by `github.com/zalando/go-keyring`, using macOS Keychain on Darwin and Secret Service/keyring on Linux through the existing platform seam.
- Added a `devicekeys.HybridStore` that prefers OS keychain storage for age X25519 and Ed25519 private identities and falls back to the existing `0600` file store when the keyring is unavailable.
- Wired init, env hydrate/run, doctor, and local event signing through the hybrid store so private identities remain out of SQLite/config while using OS-protected storage when available.
- Added mocked keyring and hybrid-store coverage so tests do not touch the developer's real keychain.
- Updated README and all affected specs to mark OS keychain/Secret Service storage implemented with file fallback and to keep remote approval as the remaining trust-plane gap.

Validated:
- Exa best-practice research for OS keychain/Secret Service storage, documented file fallback behavior, and mocked keyring tests.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/devicekeys ./internal/platform ./internal/state ./internal/cli`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Implement automatic remote device enrollment, fingerprint confirmation, and bundle re-encryption hooks.
- Add OS-enforced agent sandboxing/project-env allowlists and non-generic agent engine adapters.
- Implement service installers and evaluate whether the Darwin fsnotify/kqueue watcher should be replaced by a native FSEvents adapter.

## 2026-06-26 — Manual device approval for env recipients

Changed:
- Added `devstrap devices enroll <device-id>` with required name, OS, arch, age recipient, optional signing public key, and `--approve` support for manually registering remote device records.
- Added `state.UpsertDevice` for non-local device enrollment while refusing to overwrite the current local device identity.
- Changed encrypted env capture to include the local age recipient plus approved remote device age recipients, excluding pending/revoked/lost devices.
- Added CLI coverage proving an approved remote device can decrypt a captured env blob and that capture reports the recipient count.
- Updated README and affected specs to mark manual per-device env-decryption approval implemented while keeping automatic enrollment, fingerprint confirmation, and bundle re-encryption as future production hub work.

Validated:
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli ./internal/state`
- `GOCACHE=/tmp/devstrap-gocache go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`
- `GOCACHE=/tmp/devstrap-gocache go run ./cmd/spec-drift --base origin/main --head HEAD`
- `GOCACHE=/tmp/devstrap-gocache go test -race ./...`

Follow-ups:
- Implement automatic remote device enrollment, fingerprint confirmation, and bundle re-encryption hooks.
- Add OS-enforced agent sandboxing/project-env allowlists and non-generic agent engine adapters.
- Implement service installers and evaluate whether the Darwin fsnotify/kqueue watcher should be replaced by a native FSEvents adapter.

## 2026-06-26 — Add/adopt sync event emission

Changed:
- Fixed `devstrap add` and `scan --adopt` so local namespace writes also stamp signed local project events for `devstrap sync --hub-file` to push.
- Recorded local namespace source-event HLC/device/id metadata when add/adopt writes project rows.
- Added CLI regression coverage that scan-adopted projects appear in `sync --hub-file --dry-run` as pending local events.
- Updated sync and test-plan specs to document add/adopt event emission.

Validated:
- Code review subagent identified the missing add/adopt event emission as a high-severity blocker before commit.
- `gofmt -w cmd internal`
- `GOCACHE=/tmp/devstrap-gocache go test ./internal/cli ./internal/sync ./internal/state`

Follow-ups:
- Implement automatic remote device enrollment, fingerprint confirmation, and bundle re-encryption hooks.
- Add OS-enforced agent sandboxing/project-env allowlists and non-generic agent engine adapters.
- Implement service installers and evaluate whether the Darwin fsnotify/kqueue watcher should be replaced by a native FSEvents adapter.
