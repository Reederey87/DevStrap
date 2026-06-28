---
last_reviewed: 2026-06-28
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
