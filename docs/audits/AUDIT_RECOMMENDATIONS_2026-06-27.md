# DevStrap — Design & Implementation Audit (Second Pass)

**Date:** 2026-06-27
**Auditor:** Automated multi-agent review (12-dimension Exa-researched audit + adversarial verification)
**Scope:** `spec/` design corpus (20 docs) + Go codebase (`cmd/`, `internal/`, ~13.4K LOC) at commit `c76ed4a`.

## Table of Contents

- How this relates to the first audit
- Executive Summary
- Priority Matrix — P0 list + full matrix
- **Section 1** — CI/CD robustness (`CI-01`)
- **Section 2** — Non-VCS / remote-less projects (`NOVCS-01..05`) — *your question #1*
- **Section 3** — Non-GitHub forges (`FORGE-01..05`) — *your question #2*
- **Section 4** — Verified cross-dimension findings (65 findings / 12 dimensions + systemic themes + coverage gaps)
- **Section 5** — Cross-machine working-state sync — *your "forgot to push" idea*
- **Section 6** — Sync hub architecture & services — *your question #3*

## How this relates to the first audit

The first audit (`AUDIT_RECOMMENDATIONS.md`, 58 findings) drove commit `c76ed4a` and is largely **closed** — its headline issues (the `ext::`/`--upload-pack` git-injection RCE `SEC-1`, the stale-base re-check `ARCH-3`, the HLC skew guard `SYNC-3`, the dead redaction layer `ENV-2/SEC-3`, etc.) are now implemented and the build is green. This second pass therefore does **not** rehash those. It:

1. **Verifies** the prior fixes actually hold (adversarial re-check against the real files).
2. Surfaces **net-new** and **deeper** findings the first pass missed.
3. Adds two product dimensions raised after the first audit: **non-VCS / remote-less projects** and **non-GitHub forges**.
4. Captures a **CI/CD robustness** gap exposed by this session's failure notifications.

> Method: each of 12 dimensions ran *Exa best-practice research → grounded audit → adversarial verification*. Only `confirmed`/`adjusted` findings appear below; `refuted` ones (claims that were already fixed or inaccurate) were dropped. Evidence is cited as `file:line`.

---

## Executive Summary

DevStrap remains a well-structured, spec-driven Go project, and the **first audit cycle is largely closed** — its headline issues (the git-injection RCE, the stale-base re-check, the HLC skew guard, the dead redaction layer) are implemented, the build is green, and **CI on the current HEAD is passing** (the failure emails were from earlier, superseded commits whose only failures were `govulncheck` stdlib-CVE findings, since cleared by the Go 1.26.4 bump — see `CI-01`).

This second pass ran a **12-dimension audit** (Exa research → grounded audit → adversarial verification) plus dedicated workflows for your three product questions. It produced **65 verified cross-dimension findings** (4 high, 36 medium, 20 low — Section 4), **5 architecture findings** (`ARCH2-*`), a **CI/CD finding** (`CI-01` — Section 1), and design answers for **non-VCS/remote-less projects** (`NOVCS-*` — Section 2), **non-GitHub forges** (`FORGE-*` — Section 3), **cross-machine working-state sync** (Section 5), and **sync-hub architecture & services** (Section 6).

Two themes dominate and should frame the roadmap:

1. **Security theater — reassuring names over real enforcement.** The agent isolation that is the product's differentiator is the weakest area: the `guarded` policy is argv-substring matching trivially defeated by any interpreter (`AGEN-01`), the "no-secret" agent env actually forwards `HOME` + `SSH_AUTH_SOCK` (a live credential capability) into semi-trusted agent code (`AGEN-02`/`SECU-02`), signed-event verification fails open for unknown devices (`SECU-03`), and key custody silently downgrades to a plaintext key on any keychain error (`SECR-04`). These are MVP-stage but security-relevant, and presented as safe.
2. **Aspirational specs vs. implemented code.** The corpus describes a multi-device, daemon-backed product, but only a single-machine CLI exists; the engine is fused into `internal/cli` with no daemon seam (`ARCH2-01`), and a cluster of tables/types/columns are wired into schema but never executed (`ARCH2-02`, `SYNC-02/04`, `DATA-02`, `PROD-01/02`). Whole specified subsystems — the signed audit log (`spec/15`), the `.devstrapignore` compiler (`spec/11`) — do not exist.

**On your three questions:** the instincts are right. Non-git/remote-less projects are currently mis-handled (a no-remote repo silently corrupts the cross-device namespace — `NOVCS-01`); non-GitHub forges work for everything *except* PR creation, which is hardcoded to `gh` (`FORGE-01`); and the "forgot to push" idea is the core of the product but should **not** be literal file-sync — Section 5 lays out the git-native three-layer design (validation plane → WIP refs → encrypted bundles) that delivers it safely.

## Priority Matrix

Ranked by leverage. **P0** = high/critical, do now; **P1** = important, before the relevant subsystem ships; **P2/P3** = solid improvement / polish. (`WS-A`/`WS-B` are the working-state-sync layers from Section 5; `ARCH2-*` from the architecture re-run; the rest of Section 4 follows in the second table.)

### P0 — highest leverage (start here)

| Severity | ID | Area | Effort | Finding |
|---|---|---|---|---|
| critical | `NOVCS-01` | non-vcs | M | No-remote git repo is adopted into a state broken on every other device |
| high | `AGEN-01` | agent | L | argv-substring agent policy trivially bypassed; default `guarded` has full FS read + net exfil |
| high | `AGEN-02`/`SECU-02` | agent/security | S–M | `SSH_AUTH_SOCK` + `HOME` forwarded into the agent subprocess (live credential capability) |
| high | `SECR-01` | secrets | S | `hydrate` re-emits secrets in unescaped double quotes (`$`/backtick) → command substitution |
| high | `ARCH2-01` | architecture | M | Engine logic fused into `internal/cli`; the CLI/daemon seam the spec promises doesn't exist |
| high | `FORGE-01` | forges | M | `agent pr` hardcoded to `gh`; fails post-push on every non-GitHub forge |
| high | `CI-01` | ci/cd | S | `govulncheck@latest` unpinned & bundled in the "Go tests" job — spurious, misleading failures |
| high | `WS-A` | working-state | M | Ship the git-state **validation plane** (signed read-only snapshots) — "did I leave work on machine B?" |

### Full matrix

Cross-cutting & product-question findings first, then the 12-dimension audit findings by severity:

| Severity | ID | Area | Effort | Finding |
|---|---|---|---|---|
| medium | `SECU-03` | security | M | Event signature verification fails open for unknown/keyless devices |
| medium | `SECR-04`/`SECU-01` | secrets/security | M | Key custody silently downgrades to a 0600 plaintext age key on ANY keychain error |
| medium | `WS-B` | working-state | M | WIP recovery via per-device git refs (Phase 1) — recover forgotten content when machine A is asleep |
| medium | `NOVCS-02` | non-vcs | L | `draft_project`/`plain_folder` have no content path — permanent empty skeletons off-device |
| medium | `NOVCS-03` | non-vcs | M | Local-only folders invisible to scan (`TypePlainFolder` never emitted) |
| medium | `NOVCS-04` | non-vcs | S | `worktree`/`agent` fail with cryptic deep-git errors on remote-less repos |
| medium | `NOVCS-05` | non-vcs | S | Remote-less gap undocumented; `scan --adopt` skips the validation `add` enforces |
| medium | `FORGE-02` | forges | S | PR env allowlist passes only GitHub tokens; the multi-forge fix can't authenticate |
| medium | `FORGE-03` | forges | S | Remote-key normalization doesn't unify Azure DevOps SSH vs HTTPS forms |
| medium | `FORGE-05` | forges | S | No documented stance / graceful degradation for non-GitHub forges |
| medium | `ARCH2-02` | architecture | M | `sync_cursors`/`event_delivery` unwired; `sync` does full-history replay every run |
| medium | `ARCH2-03` | architecture | S | `spec/00` phase model contradicts the re-ordered roadmap & shipped code |
| medium | `ARCH2-04` | architecture | S | "no-daemon via periodic scans" cites a reconciler that doesn't exist |
| medium | `ARCH2-05` | architecture | M | Rejected-alternatives omits the sync-substrate & devcontainer models |
| low | `FORGE-04` | forges | S | `doctor` treats `gh` as a plain requirement and ignores `glab`/`tea` |

#### 12-dimension audit findings (Section 4), by severity

| Severity | ID | Dimension | Effort | Finding |
|---|---|---|---|---|
| high | `SECR-01` | secrets-env | S | Hydrate re-emits captured secrets in double quotes without escaping $ or backtick, undoing the safe-by-default capture and risking silent truncation/command substitution downstream |
| high | `AGEN-01` | agent-workspaces | L | Command/file policy is argv substring matching, trivially defeated by any interpreter; the default `guarded` agent has full filesystem read + network exfil |
| high | `AGEN-02` | agent-workspaces | S | SSH agent socket (SSH_AUTH_SOCK) is forwarded into the agent subprocess, contradicting the documented "no-secret" agent environment and the `~/.ssh/**` deny |
| high | `SECU-02` | security | M | Agent subprocess inherits HOME and SSH_AUTH_SOCK, forwarding a live credential capability to semi-trusted commands |
| medium | `PROD-01` | product-roadmap | M | Readiness model is half-implemented: env_ready/tooling_ready never written and derived display status never computed |
| medium | `PROD-02` | product-roadmap | S | Recorded conflicts are write-only: no CLI surface inspects them after the originating command |
| medium | `PROD-03` | product-roadmap | M | Invariant #8 (draft size/ignore limits) is unenforced and draft projects have no lifecycle commands |
| medium | `PROD-04` | product-roadmap | S | PRD 'Must have' and roadmap 'MVP definition' promise a multi-machine daemon MVP that is unbuilt and deliberately deferred per ARCH-1 |
| medium | `SYNC-01` | sync-namespace | M | project.added/updated apply has no HLC-dominance guard: same-remote convergence is order-of-arrival, not HLC-deterministic |
| medium | `SYNC-02` | sync-namespace | M | The unit-tested sync.HLC type is dead code; the HLC actually executed is a duplicated copy in store.go with no skew clamp / authority reset and no dedicated overflow/skew tests |
| medium | `SYNC-03` | sync-namespace | S | No lower-bound/range validation on incoming HLC values: negative or epoch-zero timestamps bypass the skew quarantine and poison ordering |
| medium | `SYNC-04` | sync-namespace | L | sync_cursors and event_delivery are unused schema: no incremental pull or causal-stability watermark, so GCTombstones ships a 'safe' contract no caller can satisfy |
| medium | `SYNC-05` | sync-namespace | M | A hash-chain break aborts the entire apply batch instead of recording-and-continuing, so one gapped/out-of-order event halts convergence |
| medium | `GIT-01` | git-worktrees | S | Repo lock can be reclaimed from a live same-host holder once acquired_at is older than 30 min |
| medium | `GIT-02` | git-worktrees | M | git clone network retry re-uses a possibly-non-empty destination after a SIGKILL timeout, so the retry fails with an unclassified error |
| medium | `GIT-03` | git-worktrees | M | Within-root TOCTOU revalidation does not cover the destructive promote (RemoveAll + Rename) |
| medium | `GIT-04` | git-worktrees | M | worktree remove/cleanup and BaseDrift fetch run git mutations without the per-repo lock, racing worktree new |
| medium | `GIT-05` | git-worktrees | M | Neutering global/system git config disables the user's credential.helper, breaking HTTPS auth to private repos |
| medium | `SECR-04` | secrets-env | M | HybridStore.Ensure silently downgrades to a plaintext (0600) age private key on ANY keychain error, not only genuine unavailability |
| medium | `AGEN-03` | agent-workspaces | L | No OS-enforced sandbox while running agent code, yet the default policy is the reassuring-sounding `guarded` |
| medium | `AGEN-04` | agent-workspaces | M | Policy profiles are misleading: `cautious` is identical to `guarded`, `readonly` is not read-only, and the spec's `ephemeral-ci` is rejected by the code |
| medium | `AGEN-05` | agent-workspaces | M | Agent file-path deny list is narrower than the spec and ignores the project's own stronger sensitive-file detector |
| medium | `SECU-01` | security | S | Keychain-to-file key custody silently downgrades on ANY keychain error, not just unavailability |
| medium | `SECU-03` | security | M | Event signature verification fails open for unknown devices and devices with no signing key |
| medium | `SECU-04` | security | M | Line-buffered redaction misses multi-line secrets (PEM private keys) in agent logs and live output |
| medium | `SECU-05` | security | M | Device enrollment is blind TOFU: no out-of-band fingerprint confirmation, and a device can be approved with no signing key |
| medium | `PLAT-01` | platform-daemon | M | Watcher exclusion list diverges from the scanner prune list, so the watcher would recursively register watches inside .venv/dist/build/target/__pycache__ |
| medium | `PLAT-02` | platform-daemon | M | Watcher treats every Add/Errors failure as fatal with no ENOSPC/EMFILE handling and no polling fallback, contradicting spec 06 |
| medium | `PLAT-03` | platform-daemon | L | The watcher and PollWatcher are unwired and the periodic filesystem reconciliation backstop does not exist |
| medium | `CLI-01` | cli-api | M | Global --json flag is silently ignored by most commands and never applies to error output |
| medium | `CLI-02` | cli-api | S | `scan --json --quarantine` interleaves human progress lines into the JSON stdout stream, producing invalid JSON |
| medium | `CLI-03` | cli-api | M | `run` and `agent run` collapse subprocess exit codes to generic exit 1, losing actionable status |
| medium | `CLI-04` | cli-api | M | Exit-code taxonomy is overloaded: usage errors and overwrite-conflicts both map to exitInvalidConfig (2), and Cobra arg errors map to 1 |
| medium | `CLI-05` | cli-api | M | Planned daemon Unix-socket API lacks specified peer-credential/root-rejection, framing, and version negotiation |
| medium | `TEST-01` | testing-ci | M | No fuzz targets for any untrusted-input parser, including the secret scrubber |
| medium | `TEST-02` | testing-ci | M | testscript e2e harness covers only init/status; the riskiest flows are validated in-process, bypassing the real exit-code contract |
| medium | `TEST-03` | testing-ci | M | CI computes a coverage profile and discards it; vacuous-test guard checks only 3 packages and internal/id is untested |
| medium | `TEST-04` | testing-ci | M | golangci-lint gosec is narrowed to a 6-rule allowlist that disables hardcoded-credential and weak-crypto checks |
| medium | `CODE-01` | code-quality | M | ApplyEvents aborts the entire sync batch on one event's hash-chain break, wedging forward progress for events sorted after it |
| medium | `CODE-04` | code-quality | S | Deferred Close discards flush errors on writable secret/audit files (ciphertext env blob, agent log) |
| low | `PROD-05` | product-roadmap | S | Roadmap checkbox drift (PR stale-base gate) plus a two-device validation sequencing note; the 'self-blocking gate' framing is overstated |
| low | `SECR-02` | secrets-env | S | Hydrated/rendered env files omit the "Do not commit" header that the spec requires |
| low | `SECR-03` | secrets-env | L | Revocation/approval rewrap (re-encryption to current recipient set) is not implemented; only needs_rotation is flagged |
| low | `SECR-05` | secrets-env | S | Hydrate writes the plaintext secret file before ignoring it, and only .gitignore is updated (spec also requires .devstrapignore) |
| low | `SECR-06` | secrets-env | M | Device approval grants bundle-decryption capability with no out-of-band fingerprint verification |
| low | `AGEN-06` | agent-workspaces | M | Agent log/PR-body scrubbing is token-shape-only by design; non-shaped secrets are not scrubbed, and no agent-level test validates log scrubbing |
| low | `PLAT-04` | platform-daemon | S | No Chmod-only or OS-junk event filtering in the watcher or scanner despite spec 11 enumerating the junk set |
| low | `PLAT-05` | platform-daemon | M | ServiceSpec adapter seam is too thin to render the launchd plist and systemd unit the spec mandates |
| low | `DATA-01` | data-model | S | db backup is never integrity- or FK-validated after VACUUM INTO |
| low | `DATA-02` | data-model | M | sync_cursors / event_delivery tables are dead; sync ships and pulls ALL events every run |
| low | `DATA-03` | data-model | M | Single MaxOpenConns(1) pool serves reads too; spec's 'concurrent readers' rationale is unrealized |
| low | `DATA-04` | data-model | M | Enum/status columns lack CHECK constraints and tables are not STRICT |
| low | `DATA-05` | data-model | S | No detection or warning when state.db lives on a networked/synced filesystem |
| low | `DATA-06` | data-model | S | idx_events_order column order is suboptimal for per-device prev-hash lookup; PendingEvents cannot exploit it |
| low | `TEST-05` | testing-ci | S | govulncheck is installed with @latest (unpinned) and never run on a schedule |
| low | `TEST-06` | testing-ci | M | The production fsnotify watcher adapter has no tests and the concurrent code has no goroutine-leak detection |
| low | `CODE-02` | code-quality | S | Skew-quarantine conflicts dedup on volatile details JSON, so every resync inserts a new duplicate conflict row |
| low | `CODE-03` | code-quality | S | Store.WithTx uses an inline (non-deferred) rollback, so a panic inside the closure leaks the single pooled DB connection |
| low | `CODE-05` | code-quality | M | state.Open ignores the caller's context and substitutes context.Background(), severing cancellation at DB open |
| low | `CODE-06` | code-quality | M | Namespace + git_repos/draft upsert SQL is copy-pasted between Store.UpsertProject and Tx.UpsertProject and can silently drift |

---

## Section 1 — CI/CD robustness (motivated by the failing-run emails)

### [CI-01] `govulncheck@latest` is unpinned and bundled into the "Go tests" job — spurious, misleading failures

`high` · `effort: S` · `.github/workflows/ci.yml:43-77`

**Problem.** The notification emails reported *"Go tests (ubuntu-latest) / (macos-latest) Failed"*, but the actual tests passed — the failures came from the **`Vulnerability check` step** inside the same job, which runs:

```yaml
- name: Vulnerability check
  run: |
    go install golang.org/x/vuln/cmd/govulncheck@latest   # <-- unpinned
    govulncheck ./...
```

Two coupled defects:

1. **Misleading job name.** A `govulncheck` finding fails the job titled *"Go tests"*, so the email says tests failed when they didn't. (The earlier runs `ae6e410`/`0750bc9`/`684c803` failed only on stdlib CVEs — `x509.HostnameError`, a `net.Dial` NUL-byte panic, `os.ReadDir`, `url.Parse` — surfaced through this step; `c76ed4a` cleared them by bumping the Go toolchain to `1.26.4`.)
2. **`@latest` is non-deterministic and time-bombed.** `govulncheck` and the upstream vuln DB change continuously. A brand-new stdlib CVE (or a govulncheck release) can fail CI on a PR that changed nothing relevant — a "green yesterday, red today with no code change" failure. This is exactly the class of failure that produced the emails.

**Recommendation.** Pin the tool, separate it from tests, and make vuln scanning a non-blocking signal (or a scheduled job) rather than a hard gate on every push.

**Actionable steps.**
1. Pin `govulncheck` to a version (and let Dependabot bump it), e.g. `govulncheck@v1.1.4`, or add it to `go.mod` tooling and run `go run golang.org/x/vuln/cmd/govulncheck`.
2. Move it to its own `vuln` job so a finding reads as "Vulnerability check failed", not "Go tests failed".
3. Add a **scheduled** run (`on: schedule: cron`) so newly-published CVEs are caught daily *without* blocking unrelated PRs, and consider `continue-on-error: true` on the per-PR run.

**Example.**
```yaml
  vuln:
    name: Vulnerability check
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@... # pinned SHA (matches existing style)
      - uses: actions/setup-go@...
        with: { go-version-file: go.mod, cache: true }
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
  # plus, in `on:`  schedule: [{ cron: "17 6 * * *" }]
```

**References.** https://go.dev/security/vuln/ (govulncheck), https://github.com/golang/vuln/issues (pinning guidance); see also `TEST-05` in Section 4.

---

## Section 2 — Non-VCS / remote-less projects

> **The question:** what happens to a `~/Code` directory that isn't under git, or is a git repo with no remote? DevStrap's namespace/sync/materialize model is built almost entirely around git remotes (`origin/<default_branch>`, partial clone, fresh worktrees), but a real `~/Code` is full of scratch dirs, pre-remote repos, notebooks, vendored code, and design assets.

**Verified current behavior (the short version):** a git repo with **no `origin`** is still adopted as `type=git_repo` with an empty `remote_url`, emits a `project.added` event, and syncs — but on a *second device* it can never be hydrated (clone fails with "remote URL must not be empty" → project marked `failed`). Genuinely local-only folders without a manifest are silently descended into and dropped. The documented `draft_project`/`plain_folder` content-sync path is unimplemented, and `scan --adopt` skips the remote validation that `add` enforces.

### [NOVCS-01] A no-remote git repo is adopted into a state that is broken on every other device
`critical` · `effort: M` · `internal/scan/scan.go:129-151`, `internal/cli/scan.go:52-78`, `internal/cli/hydrate.go:67-68,103`, `internal/git/git.go:517-519`

**Problem.** A repo with no `origin` (just ran `git init`, or remote not added yet) is adopted as `git_repo` with `remote_url=''` (SQLite accepts `''` against `NOT NULL` at `migrations/00001_initial.sql:44`), emits `project.added`, and syncs. On any other device the entry is a skeleton that can never hydrate — `r.Clone(ctx, project.RemoteURL="", ...)` hits `ValidateRemote("")` → "remote URL must not be empty" and the project is marked `failed`. This silently corrupts the cross-device namespace and undercuts the "same project everywhere" promise. Note `add` validates remotes (`add.go:26-29`) but `scan --adopt` does not — inconsistent contracts for one type.

**Recommendation.** Make the type↔content contract explicit: a `git_repo` namespace entry MUST have a non-empty, validated `remote_key`. Classify a remote-less repo as its own type (`local_git`) and never sync it as clonable.

**Actionable steps.**
1. In `scan.Walk`, when `CanonicalRemoteKey` yields nothing, set the finding type to `local_git` (or `draft_project`), not `git_repo`.
2. In the `scan --adopt` loop, skip/down-classify findings with `RemoteKey == ""` and print a nudge (`git remote add origin <url>` then re-scan).
3. Add a DB guard: `CHECK (remote_key <> '')` on `git_repos`, or assert non-empty in `UpsertProject` for `type=git_repo`.
4. Make `hydrate`/`open` fail fast ("project has no git remote; nothing to clone") instead of an empty-URL git error.

**Example.**
```go
// internal/scan/scan.go, in the IsRepo branch
f := Finding{Path: pk.Display, Type: TypeGitRepo}
if remote, err := opts.Git.RemoteURL(ctx, path); err == nil {
    if key, err := dsgit.CanonicalRemoteKey(remote); err == nil {
        f.RemoteURL, f.RemoteKey = remote, key
    } else { f.Type = TypeLocalGit; f.Warnings = append(f.Warnings, "unvalidated remote; treating as local-only") }
} else {
    f.Type = TypeLocalGit // no origin → never a clonable git_repo
    f.Warnings = append(f.Warnings, "git repo has no remote; add one with 'git remote add origin <url>'")
}
```
**References.** git-bundle (https://git-scm.com/docs/git-bundle), dew (https://github.com/vedanta/dew).

### [NOVCS-02] `draft_project` / `plain_folder` have no content-materialization path — permanent empty skeletons off-device
`high` · `effort: L` · `internal/cli/hydrate.go:67-69`, `internal/sync/events.go:29-35,203-277`, `spec/07:208,442-467`, `spec/04:135-157`

**Problem.** The spec promises draft projects appear and hydrate on other machines via encrypted bundles, but no bundle/snapshot code exists, `applyEventTx` ignores draft events (handles only `project.added/updated/deleted/renamed/conflict.created`), and `hydrate`/`open` refuse any non-`git_repo` type. The tertiary persona's core JTBD ("new folders appear on every machine before they are pushed to Git", `spec/02:84-89`) is unmet.

**Recommendation.** Implement the encrypted draft-bundle path the spec already designed, reusing DevStrap's existing age recipients + atomic-0600-write infra (already used by `env capture`). The `dew` tool is a direct precedent: allow-listed files → tar → zstd → age-encrypt → one blob per project → hydrate on a fresh machine.

**Actionable steps.**
1. Add `draft snapshot create <path>`: walk the dir (apply ignore + the size/file-count limits already in `draft_projects`, `migrations/00001_initial.sql:55-63`), tar+zstd, encrypt to approved-device age recipients, store a blob ref, emit `draft.snapshot.created`.
2. Add a `draft.snapshot.created` case to `applyEventTx` and a `draft_project` hydrate path (decrypt+extract into the skeleton).
3. Enforce "no plaintext secret files / no private keys in bundle" via the existing `isSecretName` detector (`scan.go:192-204`).
4. Until built, make `hydrate`/`open` on draft/plain types return "local-only project; not yet syncable", not "not git_repo".

**Example.**
```text
Pack:    allow-listed files → tar → zstd → age(recipients=approved devices) → blob
Hydrate: blob → age -d -i <device key> → unzstd → untar → write into skeleton path
```
**References.** dew (https://github.com/vedanta/dew), age (https://github.com/FiloSottile/age).

### [NOVCS-03] Genuinely local-only folders are invisible to scan (`TypePlainFolder` is never emitted)
`high` · `effort: M` · `internal/scan/scan.go:154-157,206-213`, `TypePlainFolder` defined unused at `scan.go:22`, `spec/07:87-95`

**Problem.** Only folders with a recognized manifest (`go.mod`, `package.json`, `pyproject.toml`, `Cargo.toml`, `README`) become `draft_project`; everything else (scripts, design assets, datasets, manifest-less notebooks, vendored code, monorepo subdirs) is descended into and dropped. `plain_folder` is a documented first-class type but is dead code, so DevStrap can't even represent "this path exists".

**Recommendation.** Emit `plain_folder` for managed directories that are neither git repos nor recognized projects, so the namespace can carry structure-only entries; pair with NOVCS-02 so a plain folder can be promoted to content sync.

**Actionable steps.**
1. Pick a policy (e.g. record `plain_folder` for a managed grouping dir containing no nested repo/project, instead of silently recursing past it).
2. Add the `plain_folder` branch in `scan.Walk` and surface it with a hint that content won't sync until promoted.
3. Add `devstrap promote <path> --draft|--git-remote <url>` implementing the spec lifecycle `draft → local git → remote git` (`spec/04:146-150`).

**References.** Mutagen sync (https://mutagen.io/documentation/synchronization/), Syncthing (https://docs.syncthing.net/users/syncing).

### [NOVCS-04] `worktree new` / `agent run` fail with cryptic deep-git errors on remote-less repos
`medium` · `effort: S` · `internal/cli/worktree.go:139-161,198-210`, `internal/git/git.go:216-225`, `internal/cli/agent.go:65-69`

**Problem.** Both commands assume a remote. On a no-remote repo they fail several layers deep with `origin default branch unavailable and fallback "main" was not found` or a fetch error, with no guidance that the real issue is "this repo has no remote".

**Recommendation.** Preflight in `createFreshWorktree`: if `project.RemoteKey == ""`, return a typed actionable error before touching git. Optionally add a documented `--base local:<branch>` mode for intentionally remote-less repos (clearly not "fresh-upstream").

**Example.**
```go
func createFreshWorktree(...) (state.Worktree, error) {
    if strings.TrimSpace(project.RemoteKey) == "" {
        return state.Worktree{}, appError{code: exitInvalidConfig,
            err: fmt.Errorf("%s has no git remote; fresh-upstream worktrees require one", project.Path)}
    }
    ...
```
**References.** DevPod workspaces (https://devpod.sh/docs/developing-in-workspaces/create-a-workspace).

### [NOVCS-05] The whole remote-less gap is undocumented; spec promises diverge from implementation
`medium` · `effort: S` · `spec/07:60-95,442-467`, `spec/04:135-157`, `CLAUDE.md` "Not implemented yet"

**Problem.** Specs describe `draft_project`/`plain_folder` and draft-bundle sync as if they work, while the implementation diverges (draft sync unbuilt, `plain_folder` never produced, no-remote repos adopted into a broken state). For an agent-consumed spec corpus this drift is corrosive.

**Recommendation.** Add an explicit "Project content sources & sync status" table to `spec/07`/`spec/08`, enforce the contract consistently in code, and document the current limitation honestly.

**Example (spec table).**
```text
type          remote required   content sync          hydrate/open
git_repo      yes               git clone/fetch       yes
local_git     no                encrypted bundle*     planned
draft_project no                encrypted bundle*     planned
plain_folder  no                none (structure only) n/a
* not implemented yet
```
**References.** git bundling (https://git-scm.com/book/en/v2/Git-Tools-Bundling).

### Recommended product stance (non-VCS)
**Support local-only projects via an alternate content path (encrypted bundle) while refusing to misrepresent them as git repos** — don't refuse non-git dirs, and don't force git-init-with-remote. A real `~/Code` is mostly *not* cleanly remote-backed, and the primary/tertiary personas (`spec/02:53-89`) explicitly need pre-Git folders to appear everywhere — so refusing them guts the "Dropbox for code" promise, and forcing a remote is hostile to the common "init first, push later" flow. Git owns shared, versioned code; a separate encrypted allow-listed tar→zstd→age bundle owns local-only files. DevStrap already has every primitive (age recipients, atomic 0600 writes, ignore/secret detectors, the `draft_projects` schema). Sequence: (1) stop the bleeding (NOVCS-01, NOVCS-04); (2) make local-only first-class (NOVCS-02/03 + a `promote` command); (3) tell the truth in specs/CLI (NOVCS-05).

## Section 3 — Non-GitHub forges

> **The question:** what happens when a codebase lives on GitLab / Bitbucket / Gitea / self-hosted git / Azure DevOps instead of GitHub?

**Verified coupling boundary.** Everything works up to and including **pushing the branch** — `clone`/`fetch`/`push`, `worktree new`, `hydrate`, `scan`, `add`, `sync`, default-branch resolution, and remote-key normalization are all forge-agnostic (verified: `git.go:107,120,189-268,445-514`, `agent.go:460-465`, `git.go:516-559` allow any `https`/`ssh`/`git`/`file` host). The **only hard break is PR creation**: `agent pr` pushes the branch, then unconditionally runs `gh pr create` (`agent.go:472`), which fails on any non-GitHub origin — leaving the user with a pushed branch, no PR, and a misleading `gh pr create failed: ...`.

### [FORGE-01] `agent pr` is hardcoded to `gh` and fails (post-push) on every non-GitHub forge
`high` · `effort: M` · `internal/cli/agent.go:467-486` (`createAgentPR`), call site `agent.go:219-222`

**Problem.** The PR path runs `gh pr create` unconditionally *after* the branch is already pushed. On GitLab/Bitbucket/Gitea/Azure it fails with a misleading `gh pr create failed: ...` (`agent.go:483`), with no preflight `LookPath("gh")`. The user is left with a pushed branch, no MR, and no guidance.

**Recommendation.** Detect the forge from the `origin` host and route accordingly. For MVP, at minimum fail gracefully (clear message + manual MR/compare URL) *before* claiming `gh` failed; ideally introduce a small `Forge` seam with CLI-backed impls (`gh`/`glab`/`tea`).

**Actionable steps.**
1. Add `DetectForge(remoteURL) ForgeKind` (host-keyword match + config/flag override), reusing `git.go`'s URL/scp parsing.
2. In `agent pr`, resolve the kind; if unsupported, print the branch + a constructed "open MR" URL and exit gracefully instead of running `gh`.
3. Extract `Forge.CreatePR(ctx, dir, base, head, title, body)`; make current `gh` logic the `github` impl; add `glab mr create` / `tea pr create` impls.
4. Add `--forge` override and persist `git_repos.forge_kind` (so SSH host-aliases like `git@github-work:` resolve).

**Example.**
```go
type ForgeKind string
const (ForgeGitHub ForgeKind="github"; ForgeGitLab ForgeKind="gitlab"
       ForgeGitea ForgeKind="gitea"; ForgeBitbucket ForgeKind="bitbucket"
       ForgeAzure ForgeKind="azure"; ForgeUnknown ForgeKind="")

func DetectForge(remote string) ForgeKind {
    host := hostOf(remote) // reuse splitSCPLikeRemote/url.Parse from git.go
    switch {
    case strings.Contains(host,"github."):     return ForgeGitHub
    case strings.Contains(host,"gitlab."):     return ForgeGitLab
    case strings.Contains(host,"bitbucket.org"): return ForgeBitbucket
    case strings.Contains(host,"dev.azure.com"), strings.Contains(host,"visualstudio.com"): return ForgeAzure
    default:                                    return ForgeUnknown // self-hosted → require --forge
    }
}
type Forge interface { CreatePR(ctx context.Context, dir, base, head, title, body string) (string, error) }
// gh: gh pr create … | glab: glab mr create … | tea: tea pr create …
```
**References.** git-town forge-type/driver (https://www.git-town.com/preferences/forge-type), gitbutler `determine_forge_from_host` (https://github.com/gitbutlerapp/gitbutler), poly-git-mcp gh/glab/tea wrapping (https://github.com/Grifex-0/poly-git-mcp).

### [FORGE-02] PR env allowlist passes only GitHub tokens, so the fix can't authenticate
`medium` · `effort: S` · `internal/cli/agent.go:468`, `internal/childenv/childenv.go:79-81`

**Problem.** `createAgentPR` builds its child env with only `GH_*` + `GITHUB_TOKEN`, and `childenv.Build` strips anything not allowlisted (`BasicAllowlist` has no forge tokens). Even after swapping to `glab`/`tea`, the GitLab/Gitea/Bitbucket token is removed before the child runs. This is the right secure-by-default posture — it just needs to be forge-aware.

**Recommendation.** Make the PR child-env allowlist a function of the detected forge; keep non-overridable dangerous-name stripping intact.

**Example.**
```go
func forgeTokenEnv(k ForgeKind) []string {
    switch k {
    case ForgeGitHub:    return []string{"GH_*","GITHUB_TOKEN"}
    case ForgeGitLab:    return []string{"GITLAB_TOKEN","GLAB_*","CI_JOB_TOKEN"}
    case ForgeGitea:     return []string{"GITEA_TOKEN","FORGEJO_TOKEN","TEA_*"}
    case ForgeBitbucket: return []string{"BITBUCKET_TOKEN","BITBUCKET_USERNAME","BITBUCKET_APP_PASSWORD"}
    case ForgeAzure:     return []string{"AZURE_DEVOPS_EXT_PAT","SYSTEM_ACCESSTOKEN"}
    default:             return nil
    }
}
env, _ := childenv.FromOS(append(childenv.BasicAllowlist(), forgeTokenEnv(kind)...), nil)
```
**References.** git-pkgs/forge token resolution (https://github.com/git-pkgs/forge), git-town API access (http://www.git-town.com/configuration).

### [FORGE-03] Remote-key normalization doesn't unify Azure DevOps SSH vs HTTPS forms
`medium` · `effort: S` · `internal/git/git.go:504-514` (`normalizeHostPath`), dup detection `scan.go:163-173`

**Problem.** Generic host+path folding works for github/gitlab/bitbucket/gitea/sourcehut, but Azure DevOps uses divergent SSH/HTTPS shapes that produce *different* canonical keys, so cross-protocol duplicate-clone detection misses Azure repos:
- `git@ssh.dev.azure.com:v3/org/proj/repo` → `ssh.dev.azure.com/v3/org/proj/repo`
- `https://dev.azure.com/org/proj/_git/repo` → `dev.azure.com/org/proj/_git/repo`

(Also: AWS CodeCommit `grc://` is rejected by `ValidateRemote` — acceptable, but document as unsupported.) No data loss; duplicate-detection accuracy only.

**Recommendation.** Add per-forge canonicalization quirks atop the generic default.

**Example.**
```go
func azureCanon(host, path string) (string, string) {
    if host == "ssh.dev.azure.com" { host = "dev.azure.com" }
    path = strings.TrimPrefix(path, "v3/")
    path = strings.Replace(path, "/_git/", "/", 1)
    return host, path // → dev.azure.com/org/proj/repo for both forms
}
```
**References.** gitbutler forge-from-host (https://github.com/gitbutlerapp/gitbutler), go-vcsurl (https://github.com/gitsight/go-vcsurl).

### [FORGE-04] `doctor` treats `gh` as a plain requirement and ignores `glab`/`tea`
`low` · `effort: S` · `internal/cli/doctor.go:28`, spec says optional at `spec/13:277`

**Problem.** `doctor` reports `gh` alongside `git`/`go` with no "optional" qualifier and never probes `glab`/`tea`. GitLab/Gitea users get no relevant guidance; GitHub-only users may think `gh` is mandatory for core flows (it's only needed by `agent pr`).

**Recommendation.** Make the forge-CLI check forge-aware and label it optional: inspect adopted projects' remotes, detect forge, report the relevant CLI + auth status (`gh auth status`/`glab auth status`); keep non-fatal.

**References.** git-town per-forge CLI guidance (http://www.git-town.com/configuration).

### [FORGE-05] No documented product stance or graceful degradation for non-GitHub forges
`medium` · `effort: S` · `spec/10:202-203`, `spec/08:318-329`, `README`

**Problem.** Specs note GitHub coupling internally, but nothing tells a *user* that PR creation is GitHub-only today, and the code doesn't degrade gracefully. The forge-neutral namespace/sync promise is undercut the moment an agent tries to ship work on a GitLab/Bitbucket repo.

**Recommendation.** Document the GitHub-only-PR limitation (README + `spec/08`/`spec/10`), ship the graceful-failure path now, and track GitLab/Gitea/Bitbucket PR support as explicit backlog in `spec/14`.

**References.** git-spice configurable forge kinds (https://abhinav.github.io/git-spice/cli/config/).

### Recommended product stance (forges)
**Hybrid (a)+(c), defer native REST clients.** (a) *Now:* detect forge from host, document the GitHub-only PR limitation, make `agent pr` fail gracefully on unsupported forges (push succeeds, then print branch + manual MR URL), mark `gh` optional in `doctor`. (c) *Next:* a thin `Forge` interface whose first impls **shell out to the official per-forge CLIs** (`gh`/`glab`/`tea`) selected by host, with a forge-aware env allowlist (FORGE-02) and `--forge`/`git_repos.forge_kind` override for self-hosted instances — exactly what git-town and poly-git-mcp do, and consistent with DevStrap's existing "shell out to system git/gh" architecture. (b) *Defer:* native REST clients per forge — the right long-term boundary, but the CLI-backed interface lets you add them later without changing callers.

## Section 4 — Verified cross-dimension findings

The 12-dimension audit (Exa research → grounded audit → adversarial verification) produced **65 confirmed/adjusted findings**; refuted or already-fixed claims were dropped. Grouped by dimension. Each shows `severity` · `effort` · `category` · _verify verdict_.

### Architecture & MVP Strategy

#### [ARCH2-01] Core engine logic is fused into `internal/cli`; the CLI/daemon seam the spec promises does not exist
`high` · `effort: M` · `design` · _confirmed_

**Problem.** `spec/03` defines `devstrapd` as the owner of the Namespace Reconciler, Git Materializer, Worktree Manager, Secret Broker, and Policy Engine, with frontends (CLI) talking to it over a Unix socket (`spec/03_SYSTEM_ARCHITECTURE.md:42-66`, `spec/03:97-110`). In the actual code, every one of those operations is implemented as a Cobra command closure plus package-private helpers inside `internal/cli` — at 3,374 LOC the largest package. Hydration (`hydrateProjectUnlocked`, `internal/cli/hydrate.go:67`), fresh-worktree creation (`createFreshWorktree`, `internal/cli/worktree.go:139`), and agent execution (`runAgentProcess`, `internal/cli/agent.go:390`) are all bound to the CLI layer. There is no `internal/engine`/`internal/core`/`internal/materializer`. When the daemon is built, this logic cannot be invoked by a job scheduler or socket handler without first being extracted — a large, risky refactor the platform-adapter seams were created to avoid for OS code but was never applied to the business core.

**Evidence.** `spec/03:42-66`; `internal/cli/hydrate.go:49-117`; `internal/cli/worktree.go:139`; `internal/cli/agent.go:390`; `internal/cli` = 3,374 non-test LOC; no engine/core/service package under `internal/`.

**Recommendation.** Introduce a thin `internal/engine` (or `internal/workspace`) package now, while the surface is small, exposing intent-level operations (`Hydrate`, `NewWorktree`, `RunAgent`, `Sync`) taking a `*state.Store` and `config.Paths`. CLI commands become arg-parsing/printing shells over it; the future daemon's socket handlers and job types (`spec/13:366-382`) call the same package. This is the missing seam that keeps "no command depends on the daemon" (`spec/03:141`) honest later.

**Steps.**
1. Create `internal/engine` with operation functions extracted from the current CLI closures (move `hydrateProjectUnlocked`, `createFreshWorktree`, agent run orchestration, sync push/pull).
2. Reduce `internal/cli/*.go` handlers to flag parsing + calling `engine.*` + rendering output.
3. Add a package-boundary test (like the existing `internal/platform` goos guard) asserting `internal/cli` holds no git/state orchestration beyond rendering.

**References.** https://cocoindex.io/blogs/building-an-invisible-daemon/, https://coder.com/docs/admin/infrastructure/architecture

#### [ARCH2-02] Per-peer cursor, delivery tracking, and snapshot-fallback are schema-complete but unwired; `sync` does full-history replay every run
`medium` · `effort: M` · `implementation` · _confirmed_

**Problem.** Migration `00002` creates `sync_cursors` and `event_delivery` with indexes, and `spec/03:230-238` advertises "devices can replay from last cursor" while `spec/14:417` lists "expired cursor uses full-state snapshot fallback." None is wired. `PendingEvents` (`internal/state/store.go:1957`) selects *all* rows from `events` with no cursor/delivery filter, and `devstrap sync` pushes that entire set then calls `hub.Pull(ctx, 0)` — from HLC 0 every time (`internal/cli/sync.go:37-40`). `FileHub.RetentionHLC` is never set, so `ErrSnapshotRequired` (`internal/sync/hub.go:45`) is dead. Sync is O(total history) on both sides every invocation, and two named correctness guarantees (cursor resume, snapshot fallback) are unimplemented despite their backing schema — over-engineered storage for a path that always replays from zero.

**Evidence.** `internal/state/migrations/00002_event_ordering.sql:20-42`; zero non-test refs to `sync_cursors`/`event_delivery`; `internal/state/store.go:1957-1976`; `internal/cli/sync.go:37-40`; `internal/sync/hub.go:44-47`; `spec/03:236-237`; `spec/14:417`.

**Recommendation.** Either wire the cursor (persist `last_hlc_applied` per peer after a successful `ApplyEvents`, push only events past it, `Pull(ctx, cursor)`) and set `RetentionHLC` so the snapshot path is reachable — or, if multi-device sync stays deferred, drop the unused tables/snapshot branch from the migration and spec until M7 needs them. Don't keep production-shaped storage for a code path that always replays from zero.

**Steps.**
1. Decide: wire cursors now, or remove the unused tables/claims and mark them M7-only.
2. If wiring: add `Store.SyncCursor(peer)`/`SetSyncCursor` and rename/scope `PendingEvents` to `EventsAfter(hlc)`.
3. Set `FileHub.RetentionHLC` and add a test that an `afterHLC` below retention returns `ErrSnapshotRequired`.

**References.** https://docs.syncthing.net/users/syncing, https://mutagen.io/documentation/synchronization/

#### [ARCH2-03] `spec/00` phase model contradicts the deliberately re-ordered roadmap and the shipped code
`medium` · `effort: S` · `docs` · _confirmed_

**Problem.** `CLAUDE.md` mandates reading `spec/00_START_HERE.md` first; it presents a linear plan: Phase 1 = Mac daemon, Phase 2 = sync, Phase 3 = agents, Phase 4 = StrapFS (`spec/00:53-86`). But the project deliberately built agents (Phase 3) *before* the daemon and hub: `spec/14` orders Milestone 3.5 (agent runner) ahead of M5 (daemon) and M7 (hub), stating "This milestone intentionally comes before the daemon, native watcher, Linux service, and production hub" (`spec/14:220`). So the canonical first-read doc tells an agent the next step is a daemon, when agents are done and the daemon is gated/deferred. Two specs encode two different sequencings.

**Evidence.** `spec/00:53-86`; `spec/14:12-24`; `spec/14:220`; `spec/14:256-262`; agent commands implemented per `spec/00:115`.

**Recommendation.** Make `spec/00`'s phase list a *capability grouping*, not a build order, and point sequencing to `spec/14` as the single source of truth (mirroring how branch-workflow defers to `AGENTS.md`). Add a one-line "current position: Phase 0 CLI + Phase 3 agent loop shipped; daemon (Phase 1) gated."

**Steps.**
1. Reword `spec/00:51-86` to say phases describe capability layers and that build order lives in `spec/14`.
2. Add a "current position" sentence after the phases block.
3. Re-check the spec-drift gate still passes (both files tracked).

**References.** https://www.loft.sh/blog/comparing-coder-vs-codespaces-vs-gitpod-vs-devpod

#### [ARCH2-04] The "no-daemon correctness via periodic scans" guarantee cites a reconciler that does not exist
`medium` · `effort: S` · `docs` · _confirmed_

**Problem.** `spec/03:141-143` makes the strongest correctness claim in the corpus: "Every `devstrap` CLI command works correctly without the daemon. State is materialized on demand and the managed tree is reconciled by periodic scans, so no command depends on `devstrapd`." There is no periodic-scan reconciler — no ticker, no background reconcile job; the only reconciliation is the user manually re-running `devstrap scan`. Separately the daemon's socket/IPC/job-queue API is documented in production detail (`spec/13:284-382`) with zero implementing code, and `exitDaemonUnavailable = 3` (`internal/cli/root.go:23`) is never returned. The doc overstates an availability net that is vacuous (true only because there is no daemon at all).

**Evidence.** `spec/03:141-143`; no reconciler/ticker in `internal`/`cmd`; `internal/cli/root.go:23`; `spec/13:284-382`.

**Recommendation.** Soften `spec/03:141-143` to the truthful Phase-0 invariant: commands are stateless and correct on their own; reconciliation today is the explicit `devstrap scan`; periodic-scan reconciliation is a daemon-phase deliverable. Mark the daemon API section and `exitDaemonUnavailable` reserved-for-M5.

**Steps.**
1. Edit `spec/03:141-143` to replace "reconciled by periodic scans" with "reconciled by explicit `devstrap scan`; periodic reconciliation arrives with the daemon."
2. Annotate `spec/13`'s daemon API / exit-code 3 as future/reserved.
3. Add a `// reserved for M5 daemon` comment on `exitDaemonUnavailable`.

**References.** https://medium.com/@yuseferi/daemonization-is-an-anti-pattern-using-os-native-supervision-for-go-binaries-599dbdab18cd

#### [ARCH2-05] Rejected-alternatives analysis omits the two most material competitors to the bespoke hub (existing sync substrate; devcontainer/DevPod model)
`medium` · `effort: M` · `design` · _confirmed_

**Problem.** `spec/01`'s "Alternatives considered" weighs Dropbox-style raw sync, a manifest git repo, FUSE/macFUSE, Apple File Provider, and pure-CLI (`spec/01:119-155`). The FUSE-vs-managed-namespace axis is argued well and remains sound. But the record never evaluates the two alternatives most relevant to the least-proven parts: (a) reusing an existing local-first sync engine (Syncthing/Mutagen) to propagate the namespace event log / encrypted blobs instead of hand-rolling `devstraphub` + HLC + hash-chains + signatures; and (b) a devcontainer/DevPod-style portable-config model where a committed config file is the cross-device source of truth. The bespoke hub is the least-wired, most-deferred component (M7, behind a gate that concedes "a hidden manifest git repo may substitute for a bespoke service," `spec/14:326`). The strongest reason *not* to build the custom sync stack is never given a fair hearing in the decision that authorizes building it.

**Evidence.** `spec/01:119-155`; `spec/01:133-137`; `spec/14:326`; the unwired sync stack per ARCH2-02.

**Recommendation.** Add Alternative F ("existing sync substrate / hidden manifest git repo for namespace + blob transport") and Alternative G ("devcontainer/DevPod-style committed config") to `spec/01`, each with an honest pro/con, and tie the choice to the M7 entry gate so the custom-hub build is contingent on a real two-machine drift signal rather than assumed.

**Steps.**
1. Add Alternatives F and G to `spec/01` with rejection/deferral rationale.
2. Reference the M7 entry gate (`spec/14:326`) from Alternative F so the decision is gated, not foreclosed.
3. Note in `spec/14` M7 that adopting F/G would retire much of the bespoke `internal/sync` surface.

**References.** https://docs.syncthing.net/users/syncing, https://mutagen.io/documentation/synchronization/, https://www.mesa.dev/features/filesystem

### Product Requirements & Roadmap Realism

#### [PROD-01] Readiness model is half-implemented: env_ready/tooling_ready never written and derived display status never computed
`medium` · `effort: M` · `design` · _adjusted_

**Problem.** The PRD defines a richer readiness model (4-dimensional tuple + 9-value derived display status) than the code implements. env_ready and tooling_ready columns exist but no Go path ever writes or reads them, and the derived display labels (current/ready/conflicted) are never computed, so `devstrap status` can only ever show materialization_state + dirty_state. The spec promises display states that are unreachable.

**Evidence.** spec/02_PRODUCT_REQUIREMENTS.md:220-239 (tuple + 9 derived display states); invariant #7 at spec/02:211. internal/state/migrations/00001_initial.sql:74-75 declare env_ready/tooling_ready DEFAULT 0; grep finds zero Go readers/writers. internal/cli/status.go:52-60 prints raw 2-tuple; no deriveDisplayStatus exists (grep empty). internal/state/store.go:114-115 ProjectStatus has only MaterializationState + DirtyState.

**Recommendation.** Either implement the readiness derivation the spec promises, or downgrade the spec to match reality. The cheapest first step is a single pure deriveDisplayStatus function used by both text and --json status output; set env_ready on successful `env hydrate`/`run`; and either implement a tooling_ready probe or explicitly mark tooling_ready (and the unreachable display states) as deferred in spec/02 + spec/07 so the columns are not silently dead.

**Steps.**
1. Add a pure deriveDisplayStatus(materialization, dirty, envReady, toolReady, conflicted) function mapping to the 9 documented states and route both human and --json status output through it.
1. Set env_ready=true on successful `env hydrate`/`run` for a project and add EnvReady/ToolingReady to ProjectStatus.
1. Either implement a minimal tooling_ready probe or explicitly mark tooling_ready and the current/ready display states as deferred in spec/02 and spec/07 so the columns are not dead.
1. Add a status test asserting a hydrated-and-clean project renders the expected derived label.

**Example.**
```
func deriveDisplayStatus(m, dirty string, envReady, toolReady, conflicted bool) string {
  switch {
  case conflicted: return "conflicted"
  case m == "failed": return "failed"
  case m == "skeleton": return "skeleton"
  case m == "hydrating": return "hydrating"
  case dirty == "dirty" || dirty == "diverged": return "dirty"
  case envReady && toolReady: return "ready"
  case dirty == "clean": return "current"
  default: return "available"
  }
}
```

**References.** spec/02_PRODUCT_REQUIREMENTS.md:220, spec/07_NAMESPACE_AND_SYNC_MODEL.md

#### [PROD-02] Recorded conflicts are write-only: no CLI surface inspects them after the originating command
`medium` · `effort: S` · `ux` · _adjusted_

**Problem.** The storage layer records conflict rows (scan symlink-escape/case-only collisions, sync skew/rename/path conflicts) and exposes Store.OpenConflicts/CountOpenConflicts, but nothing in the CLI ever calls them. status and doctor never reference conflicts and there is no `devstrap conflicts` command, so a user has no supported way to view/count/resolve a persisted conflict once the originating command's stderr scrolls away.

**Evidence.** internal/state/store.go:1534 CountOpenConflicts, :1550 OpenConflicts — only test callers (internal/sync/apply_test.go:83,160,205; internal/sync/hlc_test.go:167,287,387). internal/cli/scan.go:82 InsertConflict. No conflict reader in status.go/doctor.go; no `conflicts` command in root.go:85-99. spec/02_PRODUCT_REQUIREMENTS.md:150,214,237.

**Recommendation.** Wire the existing OpenConflicts reader into the user surface: add `devstrap conflicts [--json]` and include an open-conflict count in status/doctor (mirroring the existing 'secrets needing rotation' line in doctor.go:62-68), and feed the open-conflict flag into the PROD-01 display-status derivation so 'conflicted' can render.

**Steps.**
1. Add `devstrap conflicts [--json]` that calls Store.OpenConflicts and prints id/type/namespace/details with a resolve hint.
1. Add 'open conflicts: N' to status summary and doctor output via CountOpenConflicts, mirroring the rotation line at doctor.go:62-68.
1. Mark a project 'conflicted' in derived status when it has an open conflict (ties to PROD-01).
1. Add a CLI test that a scan-induced case-only collision is later listed by `devstrap conflicts`.

**Example.**
```
if n, err := store.CountOpenConflicts(cmd.Context()); err == nil {
    fmt.Fprintf(stdout, "open conflicts: %d (run `devstrap conflicts` to inspect)\n", n)
}
```

**References.** internal/state/store.go:1550, internal/cli/doctor.go:62

#### [PROD-03] Invariant #8 (draft size/ignore limits) is unenforced and draft projects have no lifecycle commands
`medium` · `effort: M` · `design` · _confirmed_

**Problem.** Invariant #8 requires draft folders to have enforced size and ignore limits, and the tertiary persona needs draft folders to appear/sync before Git. The schema reserves draft_projects.max_bytes/max_files but no Go code reads or enforces them, and there is no command to create, size-check, or promote a draft. Drafts only appear as a passive scan classification with a bare row insert.

**Evidence.** spec/02_PRODUCT_REQUIREMENTS.md:212 (invariant #8), :82-88, :198. internal/state/migrations/00001_initial.sql:58-59 (limit columns). grep: only the migration uses max_bytes/max_files. internal/scan/scan.go:21,155 (TypeDraftFolder); internal/state/store.go:776-783 (bare insert). No draft command in internal/cli/root.go:85-99.

**Recommendation.** Resolve the gap explicitly: either enforce the documented draft limits at draft registration and add a minimal draft lifecycle, or move drafts to a clearly-labeled later phase and soften invariant #8 so it is not stated as a binding MVP invariant while unbuilt.

**Steps.**
1. Decide drafts in-or-out for MVP and record it in spec/02 Non-goals or a phase tag; do not leave invariant #8 binding while unbuilt.
1. If in: enforce max_bytes/max_files at draft registration (walk + reject over-limit with a clear error) instead of leaving the columns dead.
1. If in: add minimal `devstrap draft add/list` with a size check; if out: drop/comment the dead columns and mark the tertiary persona deferred.
1. Add a test that a draft folder exceeding max_files/max_bytes is rejected or flagged.

**References.** spec/02_PRODUCT_REQUIREMENTS.md:212, internal/state/migrations/00001_initial.sql:58

#### [PROD-04] PRD 'Must have' and roadmap 'MVP definition' promise a multi-machine daemon MVP that is unbuilt and deliberately deferred per ARCH-1
`medium` · `effort: S` · `design` · _adjusted_

**Problem.** The PRD 'Must have' list and the roadmap 'MVP definition' both describe an MVP including a Mac LaunchAgent daemon, device registration, and multi-machine namespace consistency. None are implemented and all are deliberately sequenced into M5/M7 behind entry gates, and ARCH-1 re-sequenced the thin agent runner ahead of the daemon/hub. The two requirement docs therefore define an MVP that contradicts the project's own re-sequencing decision and current build reality.

**Evidence.** spec/02_PRODUCT_REQUIREMENTS.md:178 ('device registration'), :180 ('Mac LaunchAgent daemon') under 'Must have'. spec/14_MVP_ROADMAP_AND_BACKLOG.md:9 (multi-machine MVP definition); M5 tasks 276-283 and M7 tasks 342-348 largely unchecked. No daemon command in internal/cli/root.go:85-99. ARCH-1 at AUDIT_RECOMMENDATIONS.md:62,142 (applied; M3.5 at spec/14:220).

**Recommendation.** Re-baseline the PRD MVP to the proof-loop-first reality: phase-tag the 'Must have' list so Phase-0 single-machine items are the MVP musts, move daemon + device-registration + multi-machine consistency to a clearly-labeled later phase matching the roadmap and ARCH-1, and rewrite the roadmap MVP definition so it does not promise multi-machine registration as the success bar until M7 ships.

**Steps.**
1. Split spec/02 'Must have' into 'MVP (Phase 0, single machine)' vs 'Later phases', moving daemon/device-registration/multi-device out of MVP musts.
1. Rewrite spec/14:9 so the single-machine killer loop is the MVP bar until M7 ships.
1. Add a one-line cross-reference to ARCH-1 in the PRD so the re-sequencing rationale is visible there, not only the roadmap.
1. Re-run the spec-drift gate and command-doc test after the edit.

**References.** spec/02_PRODUCT_REQUIREMENTS.md:178, spec/14_MVP_ROADMAP_AND_BACKLOG.md:9, AUDIT_RECOMMENDATIONS.md:142

#### [PROD-05] Roadmap checkbox drift (PR stale-base gate) plus a two-device validation sequencing note; the 'self-blocking gate' framing is overstated
`low` · `effort: S` · `process` · _adjusted_

**Problem.** Two issues: (1) verifiable roadmap checkbox drift — the PR stale-base gate is implemented but its M3 task remains unchecked while the M3.5 task that supersedes it is checked, eroding trust in the sequencing doc; (2) the riskiest assumption (real two-machine namespace sync UX) remains unretired while single-machine polish accrues, and no explicit cheap end-to-end two-device validation milestone is scheduled even though the M7 entry gate already permits a manifest-git substitute.

**Evidence.** spec/14_MVP_ROADMAP_AND_BACKLOG.md:174 (unchecked) vs :211 (checked); gate implemented at internal/cli/agent.go:206. spec/14:326 (M7 entry gate with manifest-git substitute clause), :123 (future-work scope). spec/00_START_HERE.md killer loop steps 3-5.

**Recommendation.** Check off (or remove) the now-redundant M3 PR stale-base task at spec/14:174 since the gate ships at agent.go:206, and add a short, explicit time-boxed two-device validation milestone that uses the manifest-git substitute already permitted by the M7 entry gate (or the existing `sync --hub-file`) rather than waiting on the bespoke devstraphub. Do not restate the gate as 'self-blocking' — it already allows the cheap path.

**Steps.**
1. Update spec/14:174 to checked or delete it, noting the gate is implemented at internal/cli/agent.go:206.
1. Add an explicit, time-boxed two-device validation step (manifest-git or `sync --hub-file`) under M1.5/M7 that exercises add-on-A -> skeleton-on-B -> hydrate-on-B end to end.
1. Re-run the spec-drift gate after the roadmap edit.

**References.** spec/14_MVP_ROADMAP_AND_BACKLOG.md:174, spec/14_MVP_ROADMAP_AND_BACKLOG.md:326, internal/cli/agent.go:206

### Namespace & Sync Model (CRDT / HLC / event-log correctness)

#### [SYNC-01] project.added/updated apply has no HLC-dominance guard: same-remote convergence is order-of-arrival, not HLC-deterministic
`medium` · `effort: M` · `correctness` · _adjusted_

**Problem.** For same-remote (or no-remote) project.added/updated events, applyEventTx calls tx.UpsertProject unconditionally (events.go:228) and the SQL upsert overwrites source_event_hlc via COALESCE (store.go:883-885) with no check that the incoming (hlc,device_id,id) dominates the stored coordinates. Final projected state for a path therefore depends on the order events are FIRST observed across sync sessions, not on HLC. A lower-HLC event discovered after a higher-HLC event was already applied silently overwrites the newer state, so two peers observing the same two same-path/same-remote updates in different orders converge to different default_branch/type permanently.

**Evidence.** internal/sync/events.go:215-229 (different-remote routes to reconcileSamePath; same-remote falls through to unconditional UpsertProject at line 228); internal/state/store.go:883-885 (COALESCE source_event_hlc, no MAX/dominance predicate); contrast internal/sync/events.go:344-352 (samePathLess deterministic ordering for different-remote); spec/07_NAMESPACE_AND_SYNC_MODEL.md:170-176.

**Recommendation.** Make same-remote add/update a pure HLC last-writer-wins keyed on (hlc, device_id, event_id): only mutate the entry when the incoming coordinates strictly dominate the stored source_event coordinates; otherwise no-op. Enforce in applyEventTx before UpsertProject, or in the ON CONFLICT clause, so projection is a deterministic function of the event set regardless of arrival order.

**Steps.**
1. In applyEventTx (events.go) load existing.SourceEventHLC/DeviceID/ID for the same path+remote and skip UpsertProject when the stored coordinates dominate the incoming event
1. Alternatively make upsertNamespaceTx conditional: in ON CONFLICT only set fields WHERE excluded.source_event_hlc > namespace_entries.source_event_hlc (with device_id, id tiebreakers)
1. Add an apply test that applies [update_hlc20, update_hlc15] and the reverse and asserts identical final state (hlc20 wins both)
1. Document that same-remote field merges are HLC-LWW and rely on the single-writer-per-path assumption

**Example.**
```
// events.go same-remote branch, before unconditional upsert:
if ex, err := tx.ProjectByPath(ctx, payload.Path); err == nil {
    cur := samePathCandidate{hlc: ex.SourceEventHLC, deviceID: ex.SourceEventDeviceID, eventID: ex.SourceEventID}
    inc := samePathCandidate{hlc: event.HLC, deviceID: event.DeviceID, eventID: event.ID}
    if !samePathLess(cur, inc) { return nil } // stored coords dominate -> stale, skip
}
_, err = tx.UpsertProject(ctx, upsertParamsForEvent(payload, event))
```

**References.** https://learn.microsoft.com/en-us/azure/architecture/patterns/event-sourcing

#### [SYNC-02] The unit-tested sync.HLC type is dead code; the HLC actually executed is a duplicated copy in store.go with no skew clamp / authority reset and no dedicated overflow/skew tests
`medium` · `effort: M` · `testing` · _adjusted_

**Problem.** Two independent HLC implementations exist. The one with comprehensive tests (sync.HLC) is never executed; the one that runs (advanceHLC/receiveHLC in state) has no dedicated overflow/skew tests and, critically, advanceHLC has no maximum-offset clamp or physical-authority reset, so a corrupted/fast local clock is adopted and persisted, causing every peer to quarantine the device's events with no recovery path.

**Evidence.** grep: `.Send()`/`.Receive(`/`HLC{` appear only in internal/sync/hlc_test.go; production stamping at internal/state/store.go:1719-1767 (nextLocalEventStamp/advanceHLC) and 1028-1067/1092-1123 (ReceiveRemoteHLC/receiveHLC); third skew check at internal/sync/events.go:143; store_test.go covers monotonicity-vs-stored-clock (551-571) but has no advanceHLC/receiveHLC overflow or skew/clamp tests.

**Recommendation.** Collapse to one HLC implementation (have nextLocalEventStamp/ReceiveRemoteHLC delegate to sync.HLC, or delete hlc.go and move its tests onto the state-package functions). Add a configurable max-offset clamp with physical-authority reset in advanceHLC, and port overflow/backward-clock/skew tests onto the path that actually runs.

**Steps.**
1. Delete internal/sync/hlc.go or make nextLocalEventStamp/ReceiveRemoteHLC delegate to it so a single packed-HLC implementation exists
1. Add a maxLocalSkew clamp in advanceHLC: when nowPhysical-lastPhysical exceeds the bound, log and either treat the bound as authority or refuse to regress instead of silently adopting the clock
1. Add state-package tests for advanceHLC (logical-counter overflow at hlcLogicalMask) and receiveHLC (remote-ahead, equal-physical tie, overflow)
1. If hlc.go is kept, guard the int64 overflow in `time.Duration(remotePhysical-now)*time.Millisecond` (hlc.go:50)

**Example.**
```
// store.go advanceHLC adopts a corrupt fast clock verbatim, no clamp/reset:
switch {
case nowPhysical > lastPhysical: return packHLC(nowPhysical, 0)
case lastLogical < hlcLogicalMask: return packHLC(lastPhysical, lastLogical+1)
default: return packHLC(lastPhysical+1, 0)
}
```

**References.** https://cse.buffalo.edu/tech-reports/2014-04.pdf

#### [SYNC-03] No lower-bound/range validation on incoming HLC values: negative or epoch-zero timestamps bypass the skew quarantine and poison ordering
`medium` · `effort: S` · `correctness` · _adjusted_

**Problem.** Remote event clocks are only bounded above (forward skew). A negative or implausibly-small (epoch-zero) HLC passes the quarantine, is applied, and — because reconciliation picks the lowest (hlc,device,id) — wins every same-path/different-remote conflict and is persisted as the permanent winner, poisoning ordering cluster-wide from the 'past' direction.

**Evidence.** internal/sync/events.go:139-148 (only `offset > maxSkewMS` checked; no lower/negative/range bound before InsertEvent+applyEventTx); winner selection internal/sync/events.go:344-352 and internal/sync/hub.go:97-107 (lowest wins); tombstone-only guard internal/sync/events.go:210-214; signature check skipped for unknown keys internal/state/store.go:1915-1917.

**Recommendation.** Before trusting an event, validate the decoded physical component against a sane window: quarantine HLCs whose packed value is non-positive, whose physical millis are below a configured epoch floor, or above now+maxSkew. Quarantine (continue), do not abort, consistent with the existing skew path.

**Steps.**
1. In ApplyEvents compute physical := event.HLC >> hlcLogicalBits and quarantine when event.HLC <= 0, physical < epochFloorMS, or physical-now > maxSkewMS
1. Add and document an epoch floor constant (e.g. DevStrap launch epoch)
1. Add apply tests for a negative HLC and a physical=0 HLC asserting both are quarantined and never become the same-path winner
1. Reuse quarantineSkewedEvent with a distinct reason like 'implausible_remote_time'

**Example.**
```
physical := event.HLC >> hlcLogicalBits
if event.HLC <= 0 || physical < epochFloorMS || physical-now > maxSkewMS {
    if err := quarantineSkewedEvent(ctx, st, event, physical-now); err != nil { return err }
    continue
}
```

**References.** https://cse.buffalo.edu/tech-reports/2014-04.pdf

#### [SYNC-04] sync_cursors and event_delivery are unused schema: no incremental pull or causal-stability watermark, so GCTombstones ships a 'safe' contract no caller can satisfy
`medium` · `effort: L` · `design` · _adjusted_

**Problem.** The spec's per-peer cursor sync loop (pull-after-cursor, transactional event_delivery+sync_cursors writes, snapshot fallback) is unimplemented — the schema is dead. Pull is always from 0, so every sync re-scans the whole log (O(n) forever) and the RetentionHLC/ErrSnapshotRequired fallback is dead because RetentionHLC is never set. With no causal-stability watermark, GCTombstones exports a contract (pass MIN of all approved cursors) that no caller can satisfy, so enabling GC could resurrect deletes when a lagging peer replays a retained add.

**Evidence.** internal/state/migrations/00002_event_ordering.sql:20-44 (tables defined) vs grep (no Go references outside store_test.go:219); internal/cli/sync.go:40 (Pull(ctx,0)); internal/sync/hub.go:19,45 (RetentionHLC declared/checked but never assigned anywhere); internal/state/store.go:1069-1090 (GCTombstones contract) with callers only in internal/sync/apply_test.go:215; spec/07_NAMESPACE_AND_SYNC_MODEL.md:217-235,247.

**Recommendation.** As Phase-2 work lands, implement per-peer cursors and event_delivery per spec, drive incremental Pull from the stored cursor, and gate GCTombstones on min(last_hlc_applied) across approved peers. Until causal stability is tracked, unexport GCTombstones (or mark it internal/test-only) to prevent accidental resurrection-prone use, since it currently advertises an unsatisfiable safety precondition.

**Steps.**
1. Add Store methods to read/advance sync_cursors(workspace_id, peer_id, last_hlc_applied) written in the same Tx as event apply
1. Change cli/sync.go to Pull(ctx, cursor) and persist the new cursor; set FileHub.RetentionHLC and handle ErrSnapshotRequired by resetting the cursor and requesting a snapshot
1. Add a CausalStabilityHLC() helper = MIN(last_hlc_applied) over approved devices and require GCTombstones to take that value, or unexport GCTombstones until then
1. Add a test that a deleted+GC'd path stays deleted when a lagging peer later replays its retained add below the stability watermark

**Example.**
```
// today GCTombstones takes any HLC; nothing computes the safe watermark and nothing calls it in production:
store.GCTombstones(ctx, beforeHLC) // beforeHLC must be MIN(cursor.last_hlc_applied) -- not implemented
// needed:
watermark, _ := store.CausalStabilityHLC(ctx)
store.GCTombstones(ctx, watermark)
```

**References.** https://learn.microsoft.com/en-us/azure/architecture/patterns/event-sourcing

#### [SYNC-05] A hash-chain break aborts the entire apply batch instead of recording-and-continuing, so one gapped/out-of-order event halts convergence
`medium` · `effort: M` · `correctness` · _confirmed_

**Problem.** On ErrEventHashChain, ApplyEvents records the conflict then returns the error, aborting the remaining events in the batch instead of deferring the single gapped event and converging the rest. Because delivery is at-least-once and may reorder/gap, one missing predecessor can stall all subsequent events, contradicting both the skew-path behavior and the spec's 'record a conflict' intent.

**Evidence.** internal/sync/events.go:140-148 (skew path quarantines and continues) vs internal/sync/events.go:162-168 (ErrEventHashChain records conflict then `return err`, aborting batch); per-device chain check internal/state/store.go:1842-1845; spec/07_NAMESPACE_AND_SYNC_MODEL.md:161.

**Recommendation.** On ErrEventHashChain, record the event_hash_chain_break conflict and `continue` (defer the single event) like the skew path, so the rest of the batch converges; optionally retain the deferred event for retry once its predecessor arrives, and only surface a non-fatal warning to the CLI rather than aborting the whole sync.

**Steps.**
1. Change the ErrEventHashChain handling in ApplyEvents to `continue` after recording the conflict instead of `return err`
1. Track deferred (gapped) events for re-application on the next pull once their predecessor is present, or after a full-snapshot fallback
1. Make the CLI report deferred/conflicted counts as a warning rather than a hard error exit
1. Add an apply test that feeds [later_event_with_broken_prev, predecessor] and asserts the predecessor still applies and the batch does not abort

**Example.**
```
// events.go: today aborts the whole batch
if errors.Is(err, state.ErrEventHashChain) {
    if conflictErr := insertEventHashChainConflict(ctx, st, event, err); conflictErr != nil {
        return errors.Join(err, conflictErr)
    }
    continue // defer this event, keep converging the rest (was: return err)
}
return err
```

**References.** https://learn.microsoft.com/en-us/azure/architecture/patterns/event-sourcing

### Git Materialization & Worktrees

#### [GIT-01] Repo lock can be reclaimed from a live same-host holder once acquired_at is older than 30 min
`medium` · `effort: S` · `correctness` · _adjusted_

**Problem.** repoLockIsStale treats a lock as definitively stale only when the holder PID is confirmed dead on the same host; a live same-host holder falls through to a pure age check and is declared stale once acquired_at is >30 min old. acquired_at is never refreshed while the lock is held, so a (hypothetical) long cumulative lock-hold could let a second invocation removeStaleRepoLock the still-live lock and run a concurrent git operation against the same repo.

**Evidence.** internal/cli/repo_lock.go:150-153 (dead-PID-only staleness then unconditional age fallback); repoLockStaleAfter=30*time.Minute at repo_lock.go:14; acquired_at written once in tryAcquireRepoLock at repo_lock.go:109-117 (no re-stamp); reclaim path acquireRepoLock->removeStaleRepoLock at repo_lock.go:83-92; lock spans clone via createFreshWorktree->hydrateProjectUnlocked (internal/cli/worktree.go:140-145, internal/cli/hydrate.go:59-64). Per-op 2-minute cap that bounds practical risk: internal/git/git.go:28 and git.go:64-68.

**Recommendation.** Make liveness authoritative over age: a lock whose holder is provably alive on the same host must never be considered stale regardless of acquired_at. Apply the age-based fallback only when liveness is indeterminate (different hostname) or the PID is confirmed dead. Optionally add a heartbeat re-stamp for future long operations.

**Steps.**
1. In repoLockIsStale, when info.Hostname==hostname() && info.PID>0, return !repoLockProcessAlive(info.PID) before any age comparison (alive => not stale)
1. Restrict the time.Since(acquiredAt)>repoLockStaleAfter branch to cross-host / unknown-host / dead-PID cases only
1. Add a test: hold a lock with the current live PID and acquired_at older than repoLockStaleAfter, then assert a second acquireRepoLock fails with exitConflict instead of reclaiming
1. If long operations become possible later (daemon, longer timeouts), re-stamp acquired_at periodically or extend the cross-host window

**Example.**
```
func repoLockIsStale(lockPath string) (bool, error) {
  // ...parse info...
  if info.Hostname == hostname() && info.PID > 0 {
    return !repoLockProcessAlive(info.PID), nil // alive => NOT stale, ignore age
  }
  return time.Since(acquiredAt) > repoLockStaleAfter, nil // cross-host best-effort only
}
```

**References.** https://git-scm.com/docs/api-lockfile

#### [GIT-02] git clone network retry re-uses a possibly-non-empty destination after a SIGKILL timeout, so the retry fails with an unclassified error
`medium` · `effort: M` · `correctness` · _adjusted_

**Problem.** On a clone that exceeds the 2-minute timeout, exec.CommandContext SIGKILLs git before its cleanup runs, leaving a partial destination. The network-retry then re-clones into that non-empty directory; git rejects it with 'destination path already exists and is not an empty directory', which classifyGitError does not map to ErrNetwork, so the retry loop returns a confusing error instead of recovering. Separately, the single 2-minute Timeout is applied to clone/fetch, so large clones over slow links are killed regardless.

**Evidence.** internal/git/git.go:143-171 (runWithNetworkRetry re-runs identical args/dest); git.go:107-118 (Clone); git.go:28 (Timeout 2m for all ops); git.go:99-101 (DeadlineExceeded->ErrNetwork); git.go:598-626 (no 'already exists' case in classifyGitError); internal/git/git_test.go:177-201 (fake git never creates dest). DevStrap cleanup that bounds blast radius: internal/cli/hydrate.go:92-101.

**Recommendation.** Make clone retries idempotent (clean/recreate the destination before each attempt, or clone into a fresh per-attempt temp subdir) and give clone/fetch a dedicated, generous timeout separate from quick local commands.

**Steps.**
1. In Clone, os.RemoveAll(dest) (or generate a new temp subdir) before each git clone attempt so retries always start from an empty path
1. Add a dedicated CloneTimeout/FetchTimeout (e.g. 10-30m) distinct from the 2m default used for status/rev-parse
1. Strengthen TestCloneRetriesTransientNetworkErrors so the fake git creates files in dest on failing attempts, proving the retry still succeeds
1. Optionally classify 'already exists and is not an empty directory' so a genuinely stale dest is reported actionably

**Example.**
```
func (r Runner) Clone(ctx, remote, dest string, partial bool) error {
  attempt := func() error {
    _ = os.RemoveAll(dest) // ensure empty before each git clone
    args := []string{"clone"}
    if partial { args = append(args, "--filter=blob:none") }
    args = append(args, "--", remote, dest)
    _, err := r.Run(ctx, "", args...)
    return err
  }
  // retry attempt() on ErrNetwork
}
```

**References.** https://git-scm.com/docs/git-clone, https://pkg.go.dev/os/exec#CommandContext

#### [GIT-03] Within-root TOCTOU revalidation does not cover the destructive promote (RemoveAll + Rename)
`medium` · `effort: M` · `security` · _confirmed_

**Problem.** If any path component (e.g. the target's parent directory) is replaced with a symlink escaping the managed root during the clone window, os.RemoveAll can follow the parent symlink to delete files outside the root and os.Rename can place the clone outside the root, because the destructive promote step is not re-guarded by VerifyWithinRoot.

**Evidence.** internal/cli/hydrate.go:78-82 (pre-clone VerifyWithinRoot); hydrate.go:103 (clone window); hydrate.go:131-150 (promoteClonedRepo): line 133 ensureHydratableTarget, line 137 os.RemoveAll(targetPath), line 143 os.Rename(tmpPath,targetPath) — no within-root recheck; internal/pathkey/pathkey.go:90-93 (doc-comment over-claims coverage).

**Recommendation.** Re-run VerifyWithinRoot (parent-resolved) inside promoteClonedRepo immediately before RemoveAll/Rename, and prefer symlink-resistant operations on the destructive step (resolve and operate on the real parent, refuse symlinked components that escape).

**Steps.**
1. Thread the managed root into promoteClonedRepo and call pathkey.VerifyWithinRoot(root, targetPath) right before os.RemoveAll/os.Rename
1. filepath.EvalSymlinks(filepath.Dir(targetPath)) and confirm it stays within root before removing/renaming; refuse if a component is a symlink that escapes
1. Add a test that repoints the target's parent outside root after the pre-clone check but before promote and asserts promote refuses with ErrEscape
1. Correct the pathkey.VerifyWithinRoot doc-comment until the promote path also revalidates

**Example.**
```
func promoteClonedRepo(root, tmpPath, targetPath, nsPath, remote string) error {
  if err := pathkey.VerifyWithinRoot(root, targetPath); err != nil {
    return appError{code: exitInvalidConfig, err: fmt.Errorf("refusing to promote outside root: %w", err)}
  }
  // ...existing ensureHydratableTarget / RemoveAll / Rename...
}
```

**References.** https://cwe.mitre.org/data/definitions/367.html, https://man7.org/linux/man-pages/man2/openat.2.html

#### [GIT-04] worktree remove/cleanup and BaseDrift fetch run git mutations without the per-repo lock, racing worktree new
`medium` · `effort: M` · `correctness` · _confirmed_

**Problem.** Per-repo locks exist to serialize git commands against a shared repo, but remove, cleanup, and the BaseDrift fetch run unlocked. Concurrent `worktree new` (worktree add) and `worktree cleanup`/`remove` (worktree prune/remove) can race the same .git/worktrees administrative area; prune can collide with half-created worktree metadata.

**Evidence.** internal/cli/worktree.go:140-144 (only new takes the lock); worktree.go:412,429 (remove: prune/remove unlocked); worktree.go:477,506 (cleanup: prune/remove unlocked); internal/git/git.go:361-368 (BaseDrift fetch unlocked) invoked at worktree.go:274, worktree.go:345, internal/cli/agent.go:201; spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md:299.

**Recommendation.** Acquire the per-project repo lock around every git operation that mutates the shared repo or its worktree administrative files (worktree add/remove/prune); prioritize remove/cleanup. Wrapping the BaseDrift fetch is lower priority but consistent.

**Steps.**
1. Wrap WorktreeRemove/WorktreePrune in newWorktreeRemoveCommand and newWorktreeCleanupCommand with acquireRepoLock(opts.paths().Home, project.ID) and defer unlock()
1. Optionally take the lock around the BaseDrift fetch for status/finalize/agent pr for full consistency
1. Add a concurrency test running `worktree new` and `worktree cleanup` in parallel and assert serialized execution / no corruption
1. Document in spec/08 which commands take the lock so the invariant is auditable

**Example.**
```
// in newWorktreeCleanupCommand, before mutating git:
unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
if err != nil { return err }
defer unlock()
_ = r.WorktreePrune(ctx, repoPath)
```

**References.** https://git-scm.com/docs/git-worktree, spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md

#### [GIT-05] Neutering global/system git config disables the user's credential.helper, breaking HTTPS auth to private repos
`medium` · `effort: M` · `design` · _confirmed_

**Problem.** Blocking config injection via GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1 also removes the user's credential.helper, so HTTPS clone/fetch of private repos fails. The only HTTPS workaround is embedding a token in the remote URL, which DevStrap then persists in SQLite and writes into .git/config — a strictly worse credential posture.

**Evidence.** internal/git/git.go:561-570 (GIT_CONFIG_GLOBAL=/dev/null line 564, GIT_CONFIG_NOSYSTEM=1 line 565, GIT_TERMINAL_PROMPT=0 line 567); internal/childenv/childenv.go:80 (BasicAllowlist has SSH_AUTH_SOCK, no credential vars); internal/scan/scan.go:136 (f.RemoteURL=remote stored verbatim); internal/cli/hydrate.go:103 (project.RemoteURL passed verbatim to Clone); git.go:594-596 (redaction only on error text).

**Recommendation.** Preserve credential.helper resolution without re-opening config injection: point GIT_CONFIG_GLOBAL at a DevStrap-managed minimal config containing only credential.* (and core.sshCommand if needed), or read the configured helper once and re-inject it via `-c credential.helper=<value>`. Document the supported auth matrix and warn against token-in-URL since it is persisted.

**Steps.**
1. Resolve the helper once (git config --global --get-all credential.helper, in a separate invocation without the neutered env) and re-inject via secureArgs as -c credential.helper=<value> for clone/fetch/push
1. Or write a minimal managed gitconfig containing only credential.* and set GIT_CONFIG_GLOBAL to it instead of /dev/null
1. Add an integration test cloning a private HTTPS remote behind a credential helper to prove auth still works
1. Document the SSH-agent vs HTTPS-helper auth matrix in spec/08 and spec/09 and warn against persisting token-in-URL remotes

**Example.**
```
helper, _ := exec.Command("git","config","--global","--get","credential.helper").Output()
// prepend to secureArgs: "-c", "credential.helper="+strings.TrimSpace(string(helper))
```

**References.** https://git-scm.com/docs/gitcredentials, https://git-scm.com/docs/git-config#Documentation/git-config.txt-GITCONFIGGLOBAL

### Secrets & Environment Management

#### [SECR-01] Hydrate re-emits captured secrets in double quotes without escaping $ or backtick, undoing the safe-by-default capture and risking silent truncation/command substitution downstream
`high` · `effort: S` · `security` · _confirmed_

**Problem.** Capture refuses interpolation-looking values (looksInterpolated rejects `$(`, `${`, backtick unless --literal), but the hydrate render path wraps every value in DOUBLE quotes and only escapes \n \r \t \\ and ", leaving `$` and backtick raw. Loaders that interpolate double-quoted values (python-dotenv, ruby dotenv by default; dotenvx command substitution) read a hydrated `KEY="p$ssword"` as `p` (silent truncation) and a --literal-captured `KEY="tok$(whoami)"` as command substitution. Because looksInterpolated never matches a bare `$VAR`, an ordinary secret containing `$` is captured WITHOUT --literal and round-trips into the dangerous double-quoted form, defeating the non-interpolating grammar at the output boundary.

**Evidence.** internal/cli/env.go:378-399 (quoteDotenv: double-quote wrap at 380/397, default WriteRune at 394 leaves $ and backtick raw); internal/envfile/parse.go:181-185 (looksInterpolated misses bare $); internal/cli/env.go:367-376 and :201 (renderDotenv on encrypted hydrate path); internal/cli/run.go:136 (renderDotenv into op-refs temp file); internal/envfile/parse_test.go:42-53 (no $-value round-trip test). NOTE: internal/cli/run.go:176-180 op inject output is escaped by 1Password, not by quoteDotenv.

**Recommendation.** Render captured values in a form no downstream dotenv loader will interpolate: prefer POSIX single-quote output (single-quoted dotenv values are literal in every loader) with `'` escaped as `'\''`, and fall back to double quotes only for values containing characters single quotes cannot carry (e.g. embedded newlines), in which case also escape `$` and backtick. Additionally treat a bare unescaped `$` followed by a letter/`{`/`(` as interpolation-looking so `$VAR` values require --literal, and add a capture->hydrate round-trip test asserting byte-for-byte preservation.

**Steps.**
1. In internal/cli/env.go quoteDotenv, switch to single-quote rendering for values without newlines: wrap in `'`, replacing embedded `'` with `'\''`; for multiline values use double quotes AND additionally escape `$` (as `\$`) and backtick.
1. In internal/envfile/parse.go looksInterpolated, also flag a bare unescaped `$` followed by a letter/`{`/`(` so `$VAR` values require explicit --literal.
1. Add a test in internal/cli that captures `PASS=p$word` (no --literal) and `TOK=a$b$(c)` (with --literal), hydrates, and asserts the written file parses back to the identical value.
1. Document single- vs double-quote literal semantics in spec/09_SECRETS_AND_ENVIRONMENT.md.

**Example.**
```
func quoteDotenv(v string) string {
    if !strings.ContainsAny(v, "\n\r") {
        return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
    }
    r := strings.NewReplacer(`\`,`\\`,`"`,`\"`,"$",`\$`,"`","\\`","\n",`\n`,"\r",`\r`)
    return "\"" + r.Replace(v) + "\""
}
```

**References.** https://github.com/bkeepers/dotenv/issues/507

#### [SECR-02] Hydrated/rendered env files omit the "Do not commit" header that the spec requires
`low` · `effort: S` · `docs` · _adjusted_

**Problem.** spec/09 specifies that every generated env file must begin with a header warning (`# Generated by DevStrap. Do not commit.` / `# Source profile:` / `# Generated at:`). The implementation writes only bare `KEY=value` lines for both the encrypted (renderDotenv) and 1Password (op inject output) paths, so the spec'd header is missing. The actual safety controls (0600 mode and .gitignore entry) are present, so this is a low-impact documentation/affordance drift rather than a security gap.

**Evidence.** spec/09_SECRETS_AND_ENVIRONMENT.md:131-137 (mandated header); internal/cli/env.go:367-376 (renderDotenv, no header); internal/cli/run.go:148-181 (injectProviderRefs returns raw output); no `Generated by DevStrap` string anywhere in internal/.

**Recommendation.** Either prepend the spec'd header (profile name + RFC3339 timestamp) to hydrated content for both the encrypted and provider paths, or relax spec/09:131-137 to make the header optional. Pick one so spec and code agree.

**Steps.**
1. Add a renderHeader(profileName string) []byte helper emitting the three spec'd comment lines plus a generated timestamp.
1. Prepend it in hydratedEnvContent for the devstrap_encrypted path and after reading op inject output for the 1password path.
1. Add a test asserting the hydrated file begins with `# Generated by DevStrap. Do not commit.`.
1. Alternatively, if the header is deemed unnecessary, update spec/09:131-137 to mark it optional.

**Example.**
```
header := fmt.Sprintf("# Generated by DevStrap. Do not commit.\n# Source profile: %s\n# Generated at: %s\n", profile.Name, time.Now().UTC().Format(time.RFC3339))
content = append([]byte(header), content...)
```

**References.** https://www.1password.dev/cli/reference/commands/inject

#### [SECR-03] Revocation/approval rewrap (re-encryption to current recipient set) is not implemented; only needs_rotation is flagged
`low` · `effort: L` · `docs` · _adjusted_

**Problem.** spec/09:199 states add/revoke/lost/rotate events "trigger re-encryption of affected bundles to the current approved-recipient set," phrased as current behavior. In code, revoke/lost only set needs_rotation (a flag); approve never re-encrypts existing blobs to the newly approved recipient. This rewrap is real future work (it matters once multi-device synced blob exchange exists, which is Phase 2 and not yet implemented), and the code/warning text already acknowledge that rewrap does not revoke historical access. The defect today is only that spec/09:199 overstates current capability relative to spec/13:259 and the work log.

**Evidence.** spec/09_SECRETS_AND_ENVIRONMENT.md:199 (present-tense re-encryption claim); internal/cli/devices.go:123-132 (only MarkEncryptedBindingsNeedingRotation); internal/state/store.go:1497-1512 (sets needs_rotation=1); internal/cli/env.go:104-125 (recipients derived only at capture); deferral documented at spec/13_CLI_DAEMON_API.md:259 and spec/18_WORK_LOG.md.

**Recommendation.** Align spec/09:199 wording with the deferred status (use "will trigger"/"planned" and cross-reference spec/13:259). When Phase 2 synced blob exchange lands, implement an envbundle.Rewrap that decrypts each affected bundle with the local identity and re-encrypts to the current approved recipient set, invoked on approve (so new devices gain access) and on revoke/lost (so future ciphertext drops the recipient), while keeping needs_rotation for already-exposed values.

**Steps.**
1. Edit spec/09:199 to mark recipient-set rewrap as planned/future work and reference spec/13:259, distinguishing it from value rotation (the only thing that revokes historical access).
1. Defer implementation until Phase 2: add envbundle.Rewrap(ciphertext, localIdentity, newRecipients) plus a state helper to list bundles per workspace.
1. On future approve and revoke/lost, rewrap affected blobs to content-addressed blobs and update encrypted_value_ref; keep MarkEncryptedBindingsNeedingRotation for source rotation.
1. Keep this tracked as a Phase 2 backlog item rather than a current bug.

**Example.**
```
for _, b := range affectedBlobs {
    pt := envbundle.Decrypt(b, local.Private)
    ct, ref := envbundle.Encrypt(pt.Vars, currentApprovedRecipients)
    writeEnvBlob(paths, ref, ct); store.UpdateBindingRef(b.id, ref)
}
```

**References.** https://github.com/FiloSottile/age/

#### [SECR-04] HybridStore.Ensure silently downgrades to a plaintext (0600) age private key on ANY keychain error, not only genuine unavailability
`medium` · `effort: M` · `security` · _confirmed_

**Problem.** When the keychain Store fails, HybridStore.Ensure unconditionally writes the device's age PRIVATE identity to ~/.devstrap/keys/<id>.agekey as plaintext (mode 0600) with no warning, even if the keychain is actually working and merely hit a transient error. loadSecret correctly distinguishes real unavailability via keychainUnavailable(err), but the store/Ensure path does not, so a one-time keychain error silently and permanently downgrades key storage on a capable machine. At-rest safety of that file then depends entirely on full-disk encryption, which the spec never states.

**Evidence.** internal/devicekeys/devicekeys.go:108-114 (Ensure falls back on any storeSecret error, no warning); :287-295 (storeSecret returns raw error, no unavailability gating); :297-309 (loadSecret DOES gate on keychainUnavailable); :261-272 (writeIdentity WriteFile 0o600 plaintext, line 268); spec/09_SECRETS_AND_ENVIRONMENT.md:289 (0600 fallback, no FDE caveat).

**Recommendation.** Gate the Ensure file fallback behind keychainUnavailable(err) (reuse the existing helper) so a non-availability error fails loudly instead of writing plaintext, and emit a one-line warning whenever the file store is actually chosen so the downgrade is visible. Optionally, for the genuinely-headless case, wrap the file fallback in an AEAD keyed by an Argon2id-derived KEK from a passphrase or use age scrypt encryption, and document the FDE dependency in spec/09:289.

**Steps.**
1. In internal/devicekeys/devicekeys.go Ensure (and EnsureSigning), only fall back to writeIdentity when keychainUnavailable(err); otherwise return the wrapped error without writing a plaintext key.
1. Surface a warning (log or stderr) when the file store is selected so the downgrade is observable.
1. Add a test asserting a non-unavailability keychain Store error does NOT create a plaintext key file.
1. Document the at-rest caveat (file fallback relies on FileVault/LUKS) at spec/09:289; consider age scrypt/XChaCha20-Poly1305 + Argon2id wrapping for the headless fallback.

**Example.**
```
if err := s.storeSecret(ctx, ageAccount(deviceID), identity.Private); err == nil {
    return identity, true, nil
} else if !keychainUnavailable(err) {
    return Identity{}, false, fmt.Errorf("keychain store failed: %w", err)
}
// only here: write fallback + warn
```

**References.** https://github.com/FiloSottile/age/

#### [SECR-05] Hydrate writes the plaintext secret file before ignoring it, and only .gitignore is updated (spec also requires .devstrapignore)
`low` · `effort: S` · `security` · _adjusted_

**Problem.** On hydrate the target secret file is created first (writeHydratedEnvFile) and only afterward passed to ensureIgnored, leaving a brief window where a freshly-written plaintext secret exists but is not yet recorded in .gitignore; if the ignore step fails or the process is interrupted the file remains un-ignored. Separately, ensureIgnored only appends to .gitignore, but spec/09:128 requires the hydrated path to be ignored by both .gitignore and .devstrapignore, so DevStrap's own ignore layer is never updated (it also does not yet read .devstrapignore anywhere).

**Evidence.** internal/cli/env.go:163-168 (write at 163, ensureIgnored at 166); internal/cli/env.go:462-491 (ensureIgnored opens only projectPath/.gitignore at line 468); spec/09_SECRETS_AND_ENVIRONMENT.md:128 (requires .gitignore and .devstrapignore); no .devstrapignore reference anywhere in internal/.

**Recommendation.** Register the path in .gitignore (and, once the ignore layer reads it, .devstrapignore) BEFORE writing the secret content, so the file is ignored the instant it exists; abort if the ignore update fails. Either implement the .devstrapignore append to satisfy spec/09:128 or relax the spec until the ignore layer consumes that file.

**Steps.**
1. Reorder hydrate (and capture) so ensureIgnored runs before the secret bytes are written, and abort on ignore failure.
1. Extend ensureIgnored (or add a sibling) to also append to .devstrapignore per spec/09:128, or update spec/09:128 to defer .devstrapignore until the ignore layer reads it.
1. Add a test asserting the ignore entry exists before the secret file is created.

**Example.**
```
if err := ensureIgnored(localPath, target); err != nil { return err }
if err := writeHydratedEnvFile(target, content, force); err != nil { return err }
```

**References.** https://github.com/bkeepers/dotenv/issues/507

#### [SECR-06] Device approval grants bundle-decryption capability with no out-of-band fingerprint verification
`low` · `effort: M` · `security` · _adjusted_

**Problem.** The implemented `devstrap devices approve` and `devstrap devices enroll --approve` mark a device approved (which makes envRecipients encrypt future bundles to that device's age key) without any out-of-band fingerprint confirmation, contradicting spec/09:191-197 which states approval requires it. This matters only once a real Hub and synced blob exchange exist (Phase 2); both are documented as future work, so today the gap is a spec-wording/sequencing issue rather than an exploitable defect.

**Evidence.** internal/cli/devices.go:103-136 (approve = SetDeviceTrustState only, no fingerprint); internal/cli/devices.go:42-45,60 (enroll --approve immediate); internal/cli/env.go:104-125 (approved device public keys become recipients); spec/09_SECRETS_AND_ENVIRONMENT.md:191-197 (fingerprint requirement); deferral at spec/09:34, spec/09:180, spec/13_CLI_DAEMON_API.md:259.

**Recommendation.** Make spec/09:191-197 note that fingerprint enforcement is pending/Phase-2 (cross-reference spec/13:259), and have approve/enroll --approve print a warning that out-of-band fingerprint verification is not yet enforced. Implement the actual fingerprint-confirmation gate when remote enrollment and Hub key advertisement land in Phase 2.

**Steps.**
1. Edit spec/09:191-197 to mark out-of-band fingerprint verification as not-yet-enforced future work, referencing spec/13:259.
1. Emit a warning from `devices approve` and `devices enroll --approve` that fingerprint verification is pending so operators are not misled.
1. When Phase 2 Hub/remote enrollment lands, add a confirmation step that displays the Hub-advertised public-key fingerprint and requires explicit user match before approval.
1. Track as a Phase 2 backlog item, not a current bug.

**Example.**
```
// approve currently: store.SetDeviceTrustState(ctx, id, "approved") with no fingerprint gate
// Phase 2: confirm advertised age-pubkey fingerprint out-of-band before granting recipient status
```

**References.** https://github.com/FiloSottile/age/

### Agent Workspaces & Command Policy

#### [AGEN-01] Command/file policy is argv substring matching, trivially defeated by any interpreter; the default `guarded` agent has full filesystem read + network exfil
`high` · `effort: L` · `security` · _adjusted_

**Problem.** The agent command/file policy inspects only the literal top-level argv as strings. Any interpreter or downloader (`python3 -c`, `node -e`, `bash -c`, `wget`, `curl`) reads/exfiltrates any file the user can read because the wrapper cannot see what the spawned process does, the run uses the user's full PATH, and there is no OS sandbox. The default `--policy guarded` is bypassed by `python3 -c "print(open('/etc/passwd').read())"` (the -c body is joined under the worktree root so pathWithin returns true) and by obfuscated paths like `'~/.'+'aws/credentials'` that split the `/.aws` substring the grep looks for.

**Evidence.** internal/cli/agent.go:255 (joined argv), :266-270 (substring denylist), :341-342 (token with '/' joined under root), :373 (fixed-substring path grep), :382-388 (pathWithin), :399-407 (BasicAllowlist PATH + exec with no sandbox); spec/10_AGENT_WORKSPACES_AND_POLICIES.md:90,130 acknowledge wrapper-level/deferred-sandbox scope.

**Recommendation.** Treat argv/path inspection as an advisory UX layer only and say so explicitly; make the real control an OS sandbox (tracked in AGEN-03). Near-term, reduce the false sense of security: disclaim the policy in `--help`, and under non-yolo policies refuse known interpreters/shells/downloaders unless an explicit per-project allowlist entry is present.

**Steps.**
1. State in `--help` and code comments that command/file policy is advisory and NOT a security boundary until a sandbox exists.
1. Under non-yolo-local policies, deny bare interpreters/shells/downloaders (sh, bash, zsh, env, python*, node, perl, ruby, wget, curl) unless explicitly allowlisted per project.
1. Stop relying on pathWithin/agentPathLooksSensitive string heuristics as the only barrier; gate reads with an OS mechanism (see AGEN-03).
1. Add tests asserting `python3 -c "open('/etc/passwd')"` and `wget ... | sh` are blocked or sandbox-contained under the default policy.

**Example.**
```
// Verified: this passes the default guarded policy and reads any file:
// devstrap agent run work/acme/api --task x -- python3 -c "print(open('/etc/passwd').read())"
// resolveAgentPathToken joins the -c body under root, so pathWithin()==true and the outside-worktree check never fires.
```

**References.** https://developers.openai.com/codex/concepts/sandboxing, https://docs.kernel.org/userspace-api/landlock.html

#### [AGEN-02] SSH agent socket (SSH_AUTH_SOCK) is forwarded into the agent subprocess, contradicting the documented "no-secret" agent environment and the `~/.ssh/**` deny
`high` · `effort: S` · `security` · _confirmed_

**Problem.** The agent child environment is built from childenv.BasicAllowlist(), which includes SSH_AUTH_SOCK. Forwarding the ssh-agent socket gives the agent live use of every private key in the user's agent (git push to arbitrary repos, ssh into the user's servers, sign commits as the user) without ever reading `~/.ssh`, directly undermining both the documented no-secret agent environment and the `~/.ssh/**` file deny.

**Evidence.** internal/childenv/childenv.go:80 (`SSH_AUTH_SOCK` in BasicAllowlist); internal/cli/agent.go:399 (agent env = childenv.FromOS(childenv.BasicAllowlist(), ...)); spec/10_AGENT_WORKSPACES_AND_POLICIES.md:130,137 (no-secret env / agents receive no secrets); no SSH assertion in internal/cli/root_test.go (only OP_SERVICE_ACCOUNT_TOKEN at :923).

**Recommendation.** Give the agent runner its own minimal allowlist that excludes SSH_AUTH_SOCK (and SSH_ASKPASS), keeping BasicAllowlist intact for Git/editor/gh which legitimately need agent auth. Credentialed push remains the separate, user-initiated `agent pr` step.

**Steps.**
1. Add childenv.AgentAllowlist() = BasicAllowlist minus SSH_AUTH_SOCK and use it in runAgentProcess (agent.go:399).
1. Keep gitEnv/editor/gh on BasicAllowlist so interactive Git/PR over SSH still works.
1. Add a test asserting the agent process env contains no SSH_AUTH_SOCK / SSH_* entries.
1. Document that SSH/credential access for agents is opt-in and granted only at `agent pr` time.

**Example.**
```
func AgentAllowlist() []string {
  return []string{"PATH","HOME","USER","LOGNAME","SHELL","TMPDIR","TERM"} // no SSH_AUTH_SOCK
}
```

**References.** https://man.openbsd.org/ssh-agent, https://developers.openai.com/codex/concepts/sandboxing

#### [AGEN-03] No OS-enforced sandbox while running agent code, yet the default policy is the reassuring-sounding `guarded`
`medium` · `effort: L` · `security` · _adjusted_

**Problem.** The agent feature runs commands with no OS sandbox — only the bypassable wrapper policy (AGEN-01) stands between agent code and the user's files/network. The unconfined run is presented under the default policy name `guarded`, which implies enforced confinement that does not exist. Sandbox mode and approval/denylist policy are conflated into one `--policy` flag.

**Evidence.** internal/cli/agent.go:406-407 (exec.CommandContext + command.Dir, no sandbox wrapper); internal/cli/agent.go:118 (default policy 'guarded'); spec/10_AGENT_WORKSPACES_AND_POLICIES.md:97,130 (sandbox explicitly deferred).

**Recommendation.** Keep the OS sandbox on the roadmap behind the existing internal/platform adapter seams (Darwin sandbox-exec/Seatbelt profile, Linux Landlock+seccomp or bwrap). In the near term, separate sandbox mode from approval/denylist policy and stop presenting an unsandboxed run under a name like `guarded`; require explicit opt-in (e.g. `--no-sandbox`/`yolo-local`) when no sandbox backend is available, rather than implying confinement.

**Steps.**
1. Define a platform.AgentSandbox adapter seam (Darwin: sandbox-exec profile confining reads/writes to the worktree+TMPDIR; Linux: Landlock ruleset) and track it in the roadmap as the actual control.
1. Split flags: a sandbox mode separate from the approval/denylist policy.
1. Rename or re-message the default so an unsandboxed run is not labeled 'guarded'; print clearly when no sandbox backend is active.
1. Add an integration test that an agent under the default mode cannot read a file outside the worktree even via `python3 -c`.

**Example.**
```
# macOS roadmap: wrap the agent command
# sandbox-exec -p '(version 1)(deny default)(allow process-exec)(allow file-read* (subpath "WORKTREE"))(allow file-write* (subpath "WORKTREE"))' -- python3 ...
```

**References.** https://docs.kernel.org/userspace-api/landlock.html, https://github.com/containers/bubblewrap

#### [AGEN-04] Policy profiles are misleading: `cautious` is identical to `guarded`, `readonly` is not read-only, and the spec's `ephemeral-ci` is rejected by the code
`medium` · `effort: M` · `correctness` · _confirmed_

**Problem.** The named policy profiles imply distinct guarantees the code does not deliver: `cautious` and `guarded` are the same code path; `readonly` blocks only a small hardcoded command list and still allows arbitrary destructive/mutating commands and interpreter writes; the `>` substring check produces false positives; and `ephemeral-ci` exists in the spec but is rejected at runtime, so spec and code disagree.

**Evidence.** internal/cli/agent.go:249 (shared readonly/cautious/guarded case), :271-277 (readonly-only extra deny list incl. bare `>`), :252-253 (default → 'unsupported agent policy', rejecting ephemeral-ci); spec/10_AGENT_WORKSPACES_AND_POLICIES.md:64-68 (profile list incl. ephemeral-ci).

**Recommendation.** Either implement the differentiated behavior the names promise or collapse to honest names. At minimum make `readonly` actually block all writes/mutations and any interpreter, give `cautious` vs `guarded` a real documented distinction (or merge them), fix the `>` false positive with argv-aware redirection detection, and reconcile `ephemeral-ci` between spec and code.

**Steps.**
1. Make `readonly` reject all known write/mutate commands and any interpreter (and back it with the read-only sandbox from AGEN-03).
1. Give `cautious` and `guarded` distinct, documented enforcement, or rename to remove the false distinction.
1. Replace the `>` substring check with argv-aware redirection detection to remove false positives like `grep 'a->b'`.
1. Implement `ephemeral-ci` or remove it from the spec, and extend the command-doc drift test to also assert the policy-name set matches between spec and binary.

**Example.**
```
// cautious == guarded today (agent.go:249):
// switch policy { case "readonly","cautious","guarded": /* same shared deny loop */ }
```

**References.** https://martinfowler.com/bliki/PrincipleOfLeastPrivilege.html

#### [AGEN-05] Agent file-path deny list is narrower than the spec and ignores the project's own stronger sensitive-file detector
`medium` · `effort: M` · `security` · _confirmed_

**Problem.** The agent file deny list is a weaker, drifted reimplementation: service-account JSON files (required by spec/10:87) are not denied at all, and the project already has a stronger detector in internal/scan that the agent policy does not reuse. All detection is also substring-based, inheriting the obfuscation bypass from AGEN-01.

**Evidence.** internal/cli/agent.go:350-358 (agentTokenLooksSensitive name set), :373 (agentPathLooksSensitive denyParts); internal/scan/scan.go:194,203 (stronger isSecretName: service-account.json, credentials.json, *.pem, .snowflake/config.toml, .aws/credentials); spec/10_AGENT_WORKSPACES_AND_POLICIES.md:83-87 (deny list incl. **/*service-account*.json).

**Recommendation.** Extract one shared sensitive-file predicate (reuse the scan detector) and call it from both scan and the agent file policy, so the two cannot drift, then keep relying on the OS read-confinement from AGEN-03 as the real barrier since substring detection is bypassable.

**Steps.**
1. Move scan's isSecretName logic into a shared package and call it from both internal/scan and the agent file policy.
1. Add *service-account*.json, credentials.json, and *.pem/*.key to the agent denied set to match the spec and the scan detector.
1. Consider adding ~/.kube/config and ~/.docker/config.json (note ~/.config/gh/hosts.yml is already covered by the /.config/gh denyPart).
1. Add tests covering a service-account JSON token and the scan-detected names being denied under the default policy.

**Example.**
```
if scan.LooksSensitive(rel) { return policyDenied(...) } // single source of truth shared by scan + agent policy
```

**References.** https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html

#### [AGEN-06] Agent log/PR-body scrubbing is token-shape-only by design; non-shaped secrets are not scrubbed, and no agent-level test validates log scrubbing
`low` · `effort: M` · `security` · _adjusted_

**Problem.** Persisted agent log scrubbing is best-effort token-shape only (nil Redactor), so secrets that do not match a known shape are not removed; the agent PR body is not scrubbed at all; and while the redact package is unit-tested, there is no agent-level test confirming the log integration actually scrubs an echoed secret.

**Evidence.** internal/cli/agent.go:413 (redact.NewWriter(logFile, nil)), :447-458 (agentPRBody, unscrubbed); internal/redact/redact.go:65-85 (token-shape patterns), :199-204 (nil-Redactor → built-in Scrub only); internal/redact/redact_test.go:97,150 (scrubber unit tests exist); internal/cli/root_test.go:923 (checks env-var NAME absence, not scrubbing).

**Recommendation.** When agents are later granted secrets via the planned project opt-in, register those concrete secret VALUES with a per-run Redactor passed to redact.NewWriter so value-level secrets are scrubbed from both the log and any PR body; meanwhile add an agent-level test that proves the log writer scrubs an echoed token-shaped secret.

**Steps.**
1. Pass a per-run *redact.Redactor (seeded with any granted secret values) to redact.NewWriter at agent.go:413 instead of nil.
1. Scrub the agent PR body through the same Redactor before `gh pr create`.
1. Add an agent integration test: have the agent command echo a ghp_-shaped token and assert the persisted log contains [REDACTED], not the token.
1. Document that log/PR-body scrubbing is token-shape best-effort unless project secret values are registered.

**Example.**
```
r := redact.NewRedactor(); r.AddValue(grantedSecretValues...)
logScrub := redact.NewWriter(logFile, r) // value-level + token-shape, not nil
```

**References.** https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html

### Security Threat Model & Cross-cutting Hardening

#### [SECU-01] Keychain-to-file key custody silently downgrades on ANY keychain error, not just unavailability
`medium` · `effort: S` · `security` · _adjusted_

**Problem.** On a present-but-failing keychain (locked, user-denied, transient/quota error) at init time, both the device age private key and the Ed25519 signing private key are silently written to a 0600 file in ~/.devstrap/keys instead of the keychain, with no warning. This contradicts the spec's 'file fallback only when keyring is unavailable' guarantee and is inconsistent with the type's own read path, which already classifies keychain errors before falling back.

**Evidence.** internal/devicekeys/devicekeys.go:108-114 (Ensure) and 185-191 (EnsureSigning) write the file on any non-nil storeSecret error; storeSecret returns the raw error (devicekeys.go:287-295); the read path gates fallback with keychainUnavailable(err) at devicekeys.go:303. Spec promise at spec/15_SECURITY_THREAT_MODEL.md:138.

**Recommendation.** Make the write path fail-closed: reuse the existing keychainUnavailable classifier (export it) and only fall back to the file store when the keyring is genuinely absent/unsupported. On a present-but-failing keyring, return an actionable error instead of silently writing the key to disk, and emit a one-line warning whenever the file fallback is actually taken so the downgrade is never silent.

**Steps.**
1. Export a classifier (e.g. devicekeys.IsKeychainUnavailable) wrapping the existing keychainUnavailable check and use it in Ensure/EnsureSigning before the File fallback.
1. On a Store error that is NOT 'unavailable', return fmt.Errorf("store device key in keychain: %w", err) instead of writing the file.
1. When the file fallback IS taken, emit a slog.Warn so the downgrade is visible.
1. Add unit tests: a backend returning a locked/auth error makes Ensure return an error with no .agekey written, while an 'unavailable' error still falls back.

**Example.**
```
if err := s.storeSecret(ctx, ageAccount(deviceID), identity.Private); err == nil {
    return identity, true, nil
} else if !IsKeychainUnavailable(err) {
    return Identity{}, false, fmt.Errorf("store device key in keychain: %w", err)
}
slog.Warn("keychain unavailable; writing device key to file fallback")
if err := s.File.writeIdentity(deviceID, identity); err != nil { ... }
```

**References.** https://go.dev/doc/security/best-practices

#### [SECU-02] Agent subprocess inherits HOME and SSH_AUTH_SOCK, forwarding a live credential capability to semi-trusted commands
`high` · `effort: M` · `security` · _adjusted_

**Problem.** The generic agent runner forwards the user's real HOME and SSH_AUTH_SOCK to agent commands. SSH_AUTH_SOCK is a live credential capability that lets a malicious or compromised agent command authenticate as the user over SSH and push to arbitrary git remotes without any key file. HOME exposes every dotfile credential (~/.ssh, ~/.aws/credentials, ~/.config/gh, ~/.netrc, ~/.npmrc). The argv-only file policy cannot stop runtime access (e.g. python -c reading os.environ['HOME']).

**Evidence.** internal/childenv/childenv.go:80 (BasicAllowlist returns ..."SSH_AUTH_SOCK"... and "HOME"); internal/cli/agent.go:399 (runAgentProcess uses childenv.FromOS(childenv.BasicAllowlist(), ...)); argv-only file policy at internal/cli/agent.go:281-319. Spec contradiction at spec/15_SECURITY_THREAT_MODEL.md:78-80.

**Recommendation.** Add a dedicated agent allowlist that drops SSH_AUTH_SOCK (the clear credential hole) instead of reusing BasicAllowlist, which is meant for trusted git/editor launches. Removing SSH_AUTH_SOCK is safe and high-value now; repointing HOME at a per-run scratch dir is a stronger isolation step but must be validated against agent tooling that legitimately needs HOME. Document that until OS sandboxing lands, agent commands still reach whatever inherited PATH binaries can reach.

**Steps.**
1. Add childenv.AgentAllowlist() that excludes SSH_AUTH_SOCK and use it in runAgentProcess instead of BasicAllowlist.
1. Optionally set HOME/TMPDIR to a worktree-local scratch dir, gated and tested so it does not break tools that need ~/.gitconfig and caches.
1. Update spec/10 and spec/15 to record that SSH_AUTH_SOCK forwarding was a credential hole now removed and that env isolation remains partial until OS sandboxing.
1. Add a test asserting the agent child env contains no SSH_AUTH_SOCK.

**Example.**
```
func AgentAllowlist() []string {
    return []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM"} // no SSH_AUTH_SOCK
}
// runAgentProcess:
env, err := childenv.FromOS(childenv.AgentAllowlist(), map[string]string{...})
```

**References.** https://cheatsheetseries.owasp.org/cheatsheets/AI_Agents_Cheat_Sheet.html

#### [SECU-03] Event signature verification fails open for unknown devices and devices with no signing key
`medium` · `effort: M` · `security` · _adjusted_

**Problem.** Because the verify path opportunistically accepts any event whose source device is unknown or has no signing key, a malicious peer or compromised hub file can inject unsigned destructive namespace events (e.g. project.deleted, which writes a tombstone) from a never-enrolled device_id and have them applied. There is no allowlist of event types that MUST carry a valid signature from a known, approved device.

**Evidence.** internal/state/store.go:1915-1916 (verifyEventSignature returns nil on sql.ErrNoRows or empty signing key), called at internal/state/store.go:1801 inside insertEvent for all types; EventProjectDeleted at internal/sync/events.go:20 with tombstone application at internal/sync/events.go:230-249. Spec mitigation claim at spec/15_SECURITY_THREAT_MODEL.md:124; future-work admission at spec/15:248.

**Recommendation.** Before the production hub ships, add a required-signature policy: define the set of trust-affecting/destructive event types and reject them at insert time unless they carry a valid signature from a known AND approved device. Keep opportunistic verification for the rest.

**Steps.**
1. Introduce mustVerify(eventType) covering destructive/trust-affecting types (project.deleted, project.renamed, device.*, env.*) and consult it in verifyEventSignature/insertEvent.
1. When mustVerify is true, require the device row to exist, be trust_state='approved', have a non-empty signing_public_key, and verify event.DeviceSig; otherwise return an error.
1. Record the mandatory-signed type list in spec/15 and remove wording that hides the fail-open default.
1. Add abuse tests: unsigned project.deleted from an unknown device is rejected; signed project.deleted from an approved device is applied.

**Example.**
```
if errors.Is(err, sql.ErrNoRows) || signingPublicKey == "" {
    if mustVerify(event.Type) {
        return fmt.Errorf("event %s of type %s requires a signature from a known approved device", event.ID, event.Type)
    }
    return nil
}
```

**References.** https://owasp.org/www-community/Threat_Modeling_Process

#### [SECU-04] Line-buffered redaction misses multi-line secrets (PEM private keys) in agent logs and live output
`medium` · `effort: M` · `security` · _confirmed_

**Problem.** When an agent or git subprocess echoes a PEM private key (RSA/Ed25519/OpenSSH) or other multi-line secret, only the BEGIN header line is redacted; the base64 body lines are written verbatim to the 0600 agent log and to stdout.

**Evidence.** internal/redact/redact.go:208-225 (Writer.Write scrubs each line independently); internal/redact/redact.go:82 (PEM pattern matches only the BEGIN header); internal/cli/agent.go:413-414 (nil redactor => built-in Scrub only).

**Recommendation.** Detect the PEM/multi-line case across line boundaries: when a BEGIN PRIVATE KEY header is seen, suppress all buffered lines until the matching END line rather than scrubbing line-by-line.

**Steps.**
1. Add a stateful inPEMBlock flag to redact.Writer: on a BEGIN .* PRIVATE KEY line emit a single [REDACTED PRIVATE KEY] and drop subsequent lines until END.
1. Apply the same multi-block handling to a top-level redact helper for non-streaming callers.
1. Cap any single buffered line/block length to bound memory if no newline arrives.
1. Add a test feeding a full multi-line PEM key through redact.Writer and asserting no base64 body bytes reach the destination.

**Example.**
```
if w.inPEM {
    if strings.Contains(line, "PRIVATE KEY-----") && strings.Contains(line, "END") { w.inPEM = false }
    continue
}
if pemBegin.MatchString(line) {
    w.inPEM = true
    io.WriteString(w.dst, "[REDACTED PRIVATE KEY]\n")
    continue
}
```

**References.** https://blog.gitguardian.com/keeping-secrets-out-of-logs/

#### [SECU-05] Device enrollment is blind TOFU: no out-of-band fingerprint confirmation, and a device can be approved with no signing key
`medium` · `effort: M` · `security` · _adjusted_

**Problem.** `devstrap devices enroll --approve` marks an attacker-suppliable age recipient 'approved' with no fingerprint display or out-of-band confirmation, and signing-public-key is optional, so a device can be approved with an empty signing key. Combined with the fail-open event verification (SECU-03), such a device's events bypass signature checks. Full out-of-band fingerprint confirmation is a deferred Phase-2 production-hub requirement.

**Evidence.** internal/cli/devices.go:39 (signing-public-key not in required set), :43-45 (--approve sets trustState='approved'), :71 (signing-public-key is an optional flag); no 'fingerprint' anywhere in *.go (grep). Deferral documented at spec/15_SECURITY_THREAT_MODEL.md:126,248, spec/09_SECRETS_AND_ENVIRONMENT.md:197, spec/00_START_HERE.md:144.

**Recommendation.** Track out-of-band fingerprint confirmation as a hard gate that must land before production remote enrollment/blob exchange (already noted as future work). Now, harden the manual command: require a signing public key when --approve is used so an approved device can never silently combine with the SECU-03 fail-open path, and display the age/signing key fingerprint on enroll so the operator can verify it out-of-band.

**Steps.**
1. Require --signing-public-key when --approve is set; refuse approval of a device with no signing key.
1. Print a stable fingerprint (e.g. SHA-256 of the age recipient and signing key) on enroll so it can be confirmed out-of-band.
1. Add the fingerprint-confirmation gate to spec/14 as an explicit dependency before production remote enrollment / encrypted blob exchange.
1. Add a test asserting `enroll --approve` without a signing key is rejected.

**Example.**
```
if approve && strings.TrimSpace(signingPublicKey) == "" {
    return appError{code: exitInvalidConfig, err: fmt.Errorf("--approve requires --signing-public-key")}
}
```

**References.** https://owasp.org/www-community/Threat_Modeling_Process

### Platform Adapters, Daemon & Watcher

#### [PLAT-01] Watcher exclusion list diverges from the scanner prune list, so the watcher would recursively register watches inside .venv/dist/build/target/__pycache__
`medium` · `effort: M` · `correctness` · _adjusted_

**Problem.** There are two independent, inconsistent hardcoded junk lists. The fsnotify watcher's shouldSkipWatchDir skips only .git/node_modules/.devstrap/vendor, while the scanner's shouldPruneDir skips ~17 generated trees plus suffix patterns. The lists diverge in both directions (watcher skips vendor; scanner skips .venv/dist/build/target/.gradle/__pycache__/etc.). When the watcher is eventually wired into a daemon, addRecursiveWatch would walk into and register a watch (one kqueue fd per path on darwin / one inotify watch on Linux) inside exactly the high-churn generated trees the spec says must never be descended into, risking descriptor/watch-budget exhaustion and debounce-window flooding across a ~/Code namespace.

**Evidence.** internal/platform/fsnotify_watcher.go:139-146 (shouldSkipWatchDir: only .git/node_modules/.devstrap/vendor); internal/scan/scan.go:179-190 (shouldPruneDir: 17 names + /data/raw, /data/interim, /.devstrap/tmp, /.devstrap/cache suffixes); internal/platform/fsnotify_watcher.go:121-137 (addRecursiveWatch Adds every non-skipped dir); spec/05_MAC_FIRST_IMPLEMENTATION.md:115 ('The ignore compiler should feed richer watcher exclusions later so .venv, dist, build, and other generated trees do not exhaust watcher budgets or trigger hydration storms.')

**Recommendation.** Extract one canonical generated-directory predicate shared by both the scanner and the watcher (and the future ignore compiler from spec/11) so neither side can drift. The watcher and scanner must skip an identical set. Until the watcher is wired (PLAT-03), this can be tracked alongside the spec/05:115 'later' deferral.

**Steps.**
1. Move shouldPruneDir's name/relpath logic into a shared package (e.g. internal/ignore) exposing a single SkipDir(name, rel string) bool
1. Call that shared predicate from both internal/scan/scan.go:109 and internal/platform/fsnotify_watcher.go:129
1. Add a test asserting the watcher and scanner skip identical directory names for a fixture containing .venv/dist/target/__pycache__/vendor
1. Wire the eventual .devstrapignore compiler as the single source so all consumers stay in sync

**Example.**
```
// internal/ignore/ignore.go
func SkipDir(name, rel string) bool {
    switch name {
    case ".git", "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache",
        ".mypy_cache", ".ruff_cache", ".ipynb_checkpoints", ".next", ".turbo",
        "dist", "build", "coverage", "target", ".gradle", "checkpoints", ".devstrap", "vendor":
        return true
    }
    return strings.HasSuffix(rel, "/data/raw") || strings.HasSuffix(rel, "/data/interim")
}
// watcher and scanner both call ignore.SkipDir(...)
```

**References.** https://man7.org/linux/man-pages/man7/inotify.7.html, https://git-scm.com/docs/gitignore

#### [PLAT-02] Watcher treats every Add/Errors failure as fatal with no ENOSPC/EMFILE handling and no polling fallback, contradicting spec 06
`medium` · `effort: M` · `correctness` · _adjusted_

**Problem.** The watcher returns from Watch on any filesystem error or failed watcher.Add, tearing down the entire namespace watcher on a single transient failure (a dir removed between Create and Add, inotify ENOSPC, or kqueue EMFILE). There is no ENOSPC/EMFILE detection, no doctor reporting, and no degradation to the already-implemented PollWatcher. Combined with the absent periodic reconciliation backstop (PLAT-03), a silent watcher death once wired would mean the namespace stops noticing changes entirely.

**Evidence.** internal/platform/fsnotify_watcher.go:27-29, :85-89, :96-98, :132-134 (all error paths return and tear down the watcher); spec/06_LINUX_COMPATIBILITY.md:137 ('If inotify returns ENOSPC or EMFILE, doctor must report the limit condition with remediation guidance and the daemon must fall back to periodic polling rather than silently missing changes.'); internal/cli/doctor.go has no watch/inotify/ENOSPC handling (grep returns nothing); PollWatcher fallback already exists at internal/platform/platform.go:95-121.

**Recommendation.** When the watcher is wired into the daemon, classify watcher.Add errors: treat per-path failures (stale dir, EACCES) as logged warnings that continue the walk, and detect ENOSPC/EMFILE specifically via a typed sentinel so the caller falls back to PollWatcher and doctor surfaces remediation (raise fs.inotify.max_user_watches / kern.maxfilesperproc).

**Steps.**
1. Define ErrWatchLimit (wrapping syscall.ENOSPC/EMFILE) in internal/platform and detect it via errors.Is on watcher.Add / watcher.Errors
1. In addRecursiveWatch, downgrade per-path Add failures to logged skips (continue the walk) instead of returning the error
1. On ErrWatchLimit, have the caller fall back to the existing PollWatcher and have doctor report the limit with sysctl/sysfs remediation
1. Add a doctor check that reads fs.inotify.max_user_watches (Linux) / kern.maxfiles (darwin) and warns when the namespace dir count approaches it

**Example.**
```
if addErr := watcher.Add(path); addErr != nil {
    if errors.Is(addErr, syscall.ENOSPC) || errors.Is(addErr, syscall.EMFILE) {
        return fmt.Errorf("%w: %s", ErrWatchLimit, path)
    }
    slog.Warn("skip unwatchable dir", "path", path, "err", addErr)
    return nil // do not kill the whole watcher
}
```

**References.** https://man7.org/linux/man-pages/man7/inotify.7.html, https://github.com/fsnotify/fsnotify

#### [PLAT-03] The watcher and PollWatcher are unwired and the periodic filesystem reconciliation backstop does not exist
`medium` · `effort: L` · `design` · _adjusted_

**Problem.** The watcher subsystem (NativeWatcher debounce/coalescing, PollWatcher fallback, FSEventScan signal) is fully built but never invoked outside a unit test. There is no daemon, no scheduler, and no periodic filesystem-vs-SQLite reconciliation loop. The spec's central correctness rule ('watch events are hints, not truth; periodic scan is the source of truth') therefore has no implementation. This is expected deferred Phase 1 work, but the specs describe the adapters in present tense, which reads as if they are active.

**Evidence.** grep '\.Watch(' across repo -> only internal/platform/platform_test.go:30; internal/cli/root.go:85-99 (no daemon/serve command registered); grep 'reconcil' non-test -> only internal/sync/events.go (conflict reconciliation) and internal/cli/sync.go:54; spec/05_MAC_FIRST_IMPLEMENTATION.md:96-113 and spec/11_IGNORE_AND_LOCAL_GARBAGE.md:178 (watcher events are hints, periodic scan is source of truth); CLAUDE.md 'Not implemented yet' explicitly lists 'daemon, local socket API, FSEvents-specific Mac watcher, LaunchAgent/systemd installers'.

**Recommendation.** Until the Phase 1 daemon is built, mark NativeWatcher/PollWatcher explicitly as unwired in spec/05 and spec/06 (they currently read as active adapters). When the daemon is implemented, add a reconciler that runs a periodic full rescan diffed against persisted project_status rows independent of watcher liveness, with watcher FSEventScan hints as an accelerant, never the sole change signal.

**Steps.**
1. In spec/05 and spec/06, state that the fsnotify adapters are implemented but not yet invoked by any running loop (no daemon exists)
1. When the daemon lands, add a reconciler that on each FSEventScan or timer tick rescans the root and diffs against persisted project_status rows
1. Run reconciliation on a schedule independent of watcher liveness so missed/coalesced events are caught
1. Add an integration test that drops a repo into the tree with the watcher disabled and asserts the periodic scan still adopts it

**References.** https://github.com/fsnotify/fsnotify

#### [PLAT-04] No Chmod-only or OS-junk event filtering in the watcher or scanner despite spec 11 enumerating the junk set
`low` · `effort: S` · `correctness` · _adjusted_

**Problem.** The watcher arms its debounce timer for every fsnotify event, including pure Chmod events (which Spotlight/antivirus/Time Machine generate in floods on macOS) and OS metadata writes (.DS_Store, .AppleDouble, .fseventsd, .nfs*/.Trash-*). None of the spec-11 junk patterns are filtered anywhere. Once the watcher is wired, idle machines would continuously arm the debounce timer and trigger full-root rescan hints. There is no atomic-save/temp-file modeling, though the coarse root-scan hint masks that.

**Evidence.** internal/platform/fsnotify_watcher.go:90-112 (acts on event without inspecting Op except Create; no Chmod-only skip, no junk-basename filter); grep DS_Store|AppleDouble|Thumbs.db|desktop.ini|fseventsd|.nfs|Trash across *.go returns nothing; spec/11_IGNORE_AND_LOCAL_GARBAGE.md:122-145 (.DS_Store, .AppleDouble, Icon?, .fseventsd, .Trash-*, .nfs*, Thumbs.db, desktop.ini).

**Recommendation.** When the watcher is wired, drop events whose only operation is Chmod, and filter OS-junk basenames in the watcher event loop so they never arm the debounce timer. Source the junk list from the same canonical ignore predicate as PLAT-01. The scanner already excludes these files from Findings, so scanner changes are optional cleanliness only.

**Steps.**
1. In the watcher loop, skip events where event.Op == fsnotify.Chmod (or Has(Chmod) with no Create/Write/Remove/Rename)
1. Add a junk-basename matcher (.DS_Store, ._*, .AppleDouble, Icon\r, .fseventsd, Thumbs.db, desktop.ini, .nfs*, .Trash-*) and ignore matching paths before setting pending
1. Add a test that a stream of Chmod-only and .DS_Store events produces zero FSEventScan emissions

**Example.**
```
if event.Op == fsnotify.Chmod || isOSJunk(filepath.Base(event.Name)) {
    continue // ignore metadata churn; do not arm the debounce timer
}
```

**References.** https://git-scm.com/docs/gitignore, https://developer.apple.com/library/archive/documentation/Darwin/Conceptual/FSEvents_ProgGuide/Introduction/Introduction.html

#### [PLAT-05] ServiceSpec adapter seam is too thin to render the launchd plist and systemd unit the spec mandates
`low` · `effort: M` · `design` · _adjusted_

**Problem.** ServiceSpec carries only Label/ExecPath/Args/Env, but the spec's own launchd plist needs RunAtLoad, KeepAlive throttle behavior, ThrottleInterval, and stdout/stderr paths, and the systemd unit needs restart-on-failure, restart backoff, start-rate limiting, and survive-logout (linger). When the launchd/systemd backends are eventually written, a too-thin spec would push these platform-specific lifecycle knobs back into the backends, partially defeating the purpose of the adapter seam.

**Evidence.** internal/platform/platform.go:42-47 (ServiceSpec{Label, ExecPath, Args, Env}); internal/platform/platform.go:123-145 (ServiceManager is UnsupportedServiceManager placeholders only); spec/05_MAC_FIRST_IMPLEMENTATION.md:53-71 (RunAtLoad, KeepAlive{Crashed/SuccessfulExit:false}, ThrottleInterval 10, StandardOut/ErrorPath); spec/06_LINUX_COMPATIBILITY.md:89-99,111-115 (Type=simple, Restart=on-failure, RestartSec=5, StartLimitIntervalSec=60, StartLimitBurst=5, loginctl enable-linger).

**Recommendation.** When implementing the launchd/systemd backends, expand ServiceSpec into a platform-neutral lifecycle policy (run-at-load, restart-on-failure-only, restart backoff, rate-limit window+burst, stdout/stderr paths, working dir, survive-logout) and map each onto plist keys or unit directives. Prefer Type=exec over Type=simple in the systemd template for accurate start-failure reporting.

**Steps.**
1. Add fields: RunAtLoad bool, RestartOnFailureOnly bool, RestartBackoff time.Duration, RateLimit struct{Window time.Duration; Burst int}, StdoutPath/StderrPath, WorkingDir, SurviveLogout bool
1. Map them in the launchd backend (KeepAlive{SuccessfulExit:false}, ThrottleInterval, Standard*Path, RunAtLoad) and systemd backend (Restart=on-failure, RestartSec, StartLimitIntervalSec/Burst, Type=exec, loginctl enable-linger when SurviveLogout)
1. Add golden-file tests rendering the plist and unit from one ServiceSpec and asserting the spec's documented keys are present

**Example.**
```
type ServiceSpec struct {
    Label, ExecPath string
    Args []string
    Env map[string]string
    RunAtLoad, RestartOnFailureOnly, SurviveLogout bool
    RestartBackoff time.Duration
    RateLimit struct{ Window time.Duration; Burst int }
    StdoutPath, StderrPath, WorkingDir string
}
```

**References.** https://www.freedesktop.org/software/systemd/man/systemd.service.html

### CLI / Daemon API Design & UX

#### [CLI-01] Global --json flag is silently ignored by most commands and never applies to error output
`medium` · `effort: M` · `ux` · _adjusted_

**Problem.** --json is declared as a global persistent flag but only 8 of ~30 leaf commands branch on it; the rest silently emit human text and exit 0, so a non-human caller cannot tell the flag was a no-op. Additionally, the central error path always writes scrubbed plain text to stderr even when --json was requested, so a caller that asked for JSON gets an unparseable error on every failure.

**Evidence.** root.go:75 declares the persistent --json flag; only scan.go:97, agent.go:136/:164, status.go:39, worktree.go:76/:293/:366, devices.go:90 branch on it (grep-confirmed). root.go:157-163 (ExitCodeWithWriter) always does fmt.Fprintln(stderr, redact.Scrub(err.Error())) with no JSON branch. spec/13:13 lists 'JSON output for automation' as a CLI principle.

**Recommendation.** Thread one output-mode decision through every command and the error path: give silent commands a JSON branch (or reject --json where no JSON shape exists yet), and emit a structured JSON error envelope to stderr when JSON mode is active. Prefer a shared helper so the contract is uniform.

**Steps.**
1. Add a shared emit(opts, w, v, human) helper and convert the silent commands (add, sync, db status/migrate/down/backup, doctor, env capture/hydrate/bind, hydrate, open, run, worktree new/finalize/remove/cleanup, devices enroll/approve/revoke/lost/rename, agent run/pr) to use it.
1. In ExitCodeWithWriter, when JSON mode is active emit {"error":true,"kind":"...","message":"...","exit_code":N} to stderr instead of plain text, deriving kind from the exit-code taxonomy.
1. For commands with no defined JSON shape, return a usage error when --json is passed so the flag is never a silent no-op.
1. Extend the command_doc_test enumeration with a table-driven test asserting --json on every command yields valid JSON or a clear error.

**Example.**
```
func emit(opts *options, w io.Writer, v any, human func() error) error {
    if opts.v.GetBool("json") {
        enc := json.NewEncoder(w); enc.SetIndent("", "  "); return enc.Encode(v)
    }
    return human()
}
```

**References.** https://clig.dev/#output, https://clispec.dev/

#### [CLI-02] `scan --json --quarantine` interleaves human progress lines into the JSON stdout stream, producing invalid JSON
`medium` · `effort: S` · `correctness` · _adjusted_

**Problem.** When both --quarantine and --json are set, quarantine progress lines are written to stdout (the machine-readable stream) before the JSON object is encoded, producing invalid JSON. More broadly, several diagnostics are written to stdout instead of stderr, violating data-vs-diagnostics separation.

**Evidence.** scan.go:94 fmt.Fprintf(stdout, "quarantined secret file %s -> %s\n", ...) inside the quarantine block; scan.go:97-101 encodes the JSON result to the same stdout. worktree.go:207 and worktree.go:230 emit 'warning:' diagnostics via fmt.Fprintf(stdout, ...).

**Recommendation.** Route progress/warning/diagnostic output to stderr, reserving stdout for the command's primary result. In JSON mode, fold quarantine actions into the JSON document (e.g. a 'quarantined' array) instead of printing them separately.

**Steps.**
1. Change the quarantine progress prints in scan.go to cmd.ErrOrStderr(), or add a quarantined array to the JSON result struct so the JSON stays valid.
1. Move the worktree.go:207 and :230 warnings to cmd.ErrOrStderr() (pre-emptive, before those commands gain JSON output).
1. Add a testscript/unit test running scan against a tree with a secret file using --json --quarantine and assert stdout unmarshals cleanly.

**Example.**
```
// scan.go: before -> fmt.Fprintf(stdout, "quarantined secret file %s -> %s\n", m.from, m.to)
// after  -> fmt.Fprintf(cmd.ErrOrStderr(), "quarantined secret file %s -> %s\n", m.from, m.to)
```

**References.** https://clig.dev/#output

#### [CLI-03] `run` and `agent run` collapse subprocess exit codes to generic exit 1, losing actionable status
`medium` · `effort: M` · `design` · _adjusted_

**Problem.** devstrap run and devstrap agent run are passthrough wrappers but neither propagates the child's exit code; any child failure becomes generic exit 1, so callers cannot distinguish a test failure (e.g. pytest exit 7) from a transient error.

**Evidence.** run.go:194 `return fmt.Errorf("run %s: %w", strings.Join(args, " "), err)`; agent.go:110 `return fmt.Errorf("agent run %s failed: %w", run.ID, commandErr)`; root.go:177 fallthrough `return exitGeneric`; grep of internal/ for exec.ExitError/ProcessState/ExitCode() returns no hits; main.go calls os.Exit(cli.ExitCode(err)).

**Recommendation.** Extract the child exit code via exec.ExitError and propagate it for passthrough commands, reconciling with the reserved 1-9 taxonomy (either carve out a dedicated child-exit range above 9 or document that run/agent run mirror the child status). Document the contract in spec/13.

**Steps.**
1. In runChildCommand detect `var ee *exec.ExitError; errors.As(err, &ee)` and return an appError carrying ee.ExitCode(), mapping/escaping any value that collides with the reserved 1-9 codes.
1. Apply the same in agent run so a failed agent command surfaces its real exit code while still recording the agent_runs row.
1. Document the exit-code passthrough contract for run/agent run in spec/13's exit-code section (spec/13:406-418).
1. Add testscript cases: `devstrap run <p> -- sh -c 'exit 7'` and the agent equivalent asserting the documented code.

**Example.**
```
if err := command.Run(); err != nil {
    var ee *exec.ExitError
    if errors.As(err, &ee) {
        return appError{code: childExitBase + ee.ExitCode(), err: fmt.Errorf("command exited %d", ee.ExitCode())}
    }
    return appError{code: exitGeneric, err: err}
}
```

**References.** https://clig.dev/#exit-codes

#### [CLI-04] Exit-code taxonomy is overloaded: usage errors and overwrite-conflicts both map to exitInvalidConfig (2), and Cobra arg errors map to 1
`medium` · `effort: M` · `design` · _confirmed_

**Problem.** Distinct failure classes collapse into the same or wrong exit codes: missing-flag usage errors use exitInvalidConfig (2) rather than a usage code; env overwrite refusals use exitInvalidConfig (2) instead of the spec's conflict code (4); and Cobra arg-count errors fall through to generic 1, so two flavors of the same usage mistake yield different codes.

**Evidence.** Constants root.go:20-30. exitInvalidConfig for required-flag errors at add.go:24, worktree.go:112/:115, agent.go:46, env.go:136, sync.go:21, worktree.go:451. Overwrite refusals at env.go:439 and env.go:455. Missing-worktree-path at worktree.go:409. root.go:174-177 maps unclassified (incl. Cobra arg) errors to exitGeneric. Spec taxonomy at spec/13:406-418 (4=conflict, 5=dirty).

**Recommendation.** Add a usage exit code consistent with the existing compact scheme (e.g. 10, documented in spec/13's table), use it for all required-flag/bad-flag errors, reclassify env-overwrite refusals to exitConflict (4), and ensure Cobra usage errors map to the same usage code as hand-written required-flag errors.

**Steps.**
1. Add exitUsage as the next free number (10) and document it in the spec/13:406-418 table; use it for every '--flag is required' / bad-flag-value error instead of exitInvalidConfig.
1. Reclassify env-overwrite refusals (env.go:439, env.go:455) to exitConflict (4) and the missing-worktree-path case (worktree.go:409) to a not-found/usage code.
1. Set a custom Cobra FlagErrorFunc/arg validator (or sentinel) so Cobra usage errors are detected in ExitCodeWithWriter and mapped to exitUsage instead of collapsing to exitGeneric.
1. Reserve exitInvalidConfig strictly for malformed/unreadable config.yaml, matching its spec name.
1. Add tests asserting each class (missing flag, bad flag value, overwrite conflict, not found) returns its intended distinct code.

**Example.**
```
// usage error
return appError{code: exitUsage, err: fmt.Errorf("--path is required")}
// overwrite conflict
return appError{code: exitConflict, err: fmt.Errorf("refusing to overwrite %s; pass --force", path)}
```

**References.** https://clig.dev/#exit-codes

#### [CLI-05] Planned daemon Unix-socket API lacks specified peer-credential/root-rejection, framing, and version negotiation
`medium` · `effort: M` · `security` · _adjusted_

**Problem.** The spec for the (not-yet-built) daemon socket API defines only transport and a URL prefix, with no documented peer-credential/root-rejection policy, no per-user 0700 socket-directory placement requirement, no API version negotiation beyond the /v1 path, and no request framing/resource limits. Designing these before the Phase-1 daemon is cheaper than retrofitting.

**Evidence.** spec/13:284-316 (Local daemon API + endpoints) has no auth/framing/versioning section. spec/15:194-197 daemon mitigations are only run-as-user/no-root/socket-restricted/state-dir-0700. spec/03:124 'serve local API'. grep of spec/15 for SO_PEERCRED|getpeereid|peercred|version negotiat|framing returns no hits.

**Recommendation.** Before any daemon code is written, add a daemon-socket security and protocol section to spec/13 covering: socket placement in a per-user 0700 directory (primary control for same-user isolation), explicit root rejection plus peer-credential logging as defense in depth, API version negotiation beyond the /v1 path, and request framing with body-size/timeouts/content-type pinning. Capture transport choice (HTTP-over-UDS vs gRPC vs JSON-RPC) in an ADR.

**Steps.**
1. Document that the socket lives in a per-user 0700 directory as the operative same-user isolation control, and note that peer-credential UID matching is defense-in-depth that does not isolate same-UID processes.
1. Specify explicit rejection of UID 0 connections and logging of every rejected connection.
1. Define API versioning beyond the /v1 path: a Server/Min-Client header handshake and mismatch behavior.
1. Define request framing and resource limits: max body size, request timeouts, and content-type pinning to application/json to bound a hostile local client.
1. Add an ADR capturing the HTTP-over-UDS vs gRPC vs JSON-RPC transport decision before the daemon lands.

**Example.**
```
// spec/13 daemon section (new):
// socket dir: ~/.devstrap (0700); socket 0600
// reject UID 0; log peer UID on every connect
// headers: Server: devstrapd/1; reject if client < Min-Client
// limits: max body 1MiB, 30s timeout, Content-Type: application/json
```

**References.** https://man7.org/linux/man-pages/man2/getsockopt.2.html, https://clig.dev/#exit-codes

### Data Model & SQLite

#### [DATA-01] db backup is never integrity- or FK-validated after VACUUM INTO
`low` · `effort: S` · `correctness` · _adjusted_

**Problem.** Backup() produces the backup with VACUUM INTO and chmods it 0600 but never validates the result. Project design surfaces quick_check + foreign_key_check separately for the live DB but not for the copy, so an unvalidated backup could propagate corruption that only surfaces during a restore.

**Evidence.** internal/state/store.go:287-295 (no post-backup check); internal/state/store.go:297-347 (QuickCheck/ForeignKeyCheck/foreignKeyCheck exist for live DB only); spec/12_DATA_MODEL_SQLITE.md:425-433 (backup section omits validation).

**Recommendation.** After VACUUM INTO + chmod, open the backup file with a fresh modernc connection and run PRAGMA quick_check and foreignKeyCheck(); on failure, os.Remove the partial backup and return a wrapped error. Note the validation in spec/12.

**Steps.**
1. In Backup(), after VACUUM INTO and chmod, sql.Open the backup path read-only and defer Close.
1. Run PRAGMA quick_check and reuse foreignKeyCheck() against that *sql.DB handle.
1. If either fails, os.Remove(outputPath) and return a wrapped error so a bad backup is never left on disk.
1. Add a test that corrupts a row/FK in a copy and asserts Backup rejects it; update spec/12 backup section.

**Example.**
```
if err := validateBackup(ctx, outputPath); err != nil {
    _ = os.Remove(outputPath)
    return fmt.Errorf("backup failed validation: %w", err)
}
```

**References.** https://www.sqlite.org/lang_vacuum.html, https://www.sqlite.org/pragma.html#pragma_integrity_check

#### [DATA-02] sync_cursors / event_delivery tables are dead; sync ships and pulls ALL events every run
`low` · `effort: M` · `design` · _adjusted_

**Problem.** The schema defines sync_cursors (per-peer cursors) and event_delivery per the delta-sync shape, but no production code reads/writes either. Sync pushes every local event (full table scan) and pulls with afterHLC=0, re-shipping/re-pulling the entire history each run and relying on idempotent apply to dedupe; spec/12 presents both tables as part of the model without a 'reserved/not-yet-used' caveat.

**Evidence.** internal/cli/sync.go:40 (afterHLC=0); internal/state/store.go:1957-1962 (no WHERE / no cursor); internal/sync/hub.go:44-63 (HLC>afterHLC); internal/state/migrations/00002_event_ordering.sql:20-42 (table defs); grep shows refs only in internal/state/store_test.go:219.

**Recommendation.** Since delta sync is deferred to the Phase-2 hub, mark sync_cursors/event_delivery as reserved/not-yet-implemented in spec/12 (and a code comment) so the schema does not masquerade as wired. When the hub is built, read last_hlc_applied for the peer, pass it as afterHLC, persist the max applied HLC after ApplyEvents in the same transaction, and bound PendingEvents by workspace_id + an outbound cursor.

**Steps.**
1. Add a spec/12 note (and code comment near the migration) that sync_cursors/event_delivery are reserved for the deferred production hub and currently unused.
1. When implementing the hub: add Store.SyncCursor(peerID)/AdvanceSyncCursor(peerID, hlc) that read/upsert sync_cursors inside the apply transaction.
1. In sync.go pass the stored cursor to hub.Pull instead of 0 and persist the highest applied HLC after ApplyEvents succeeds.
1. Add `WHERE workspace_id = ?` plus an outbound cursor to PendingEvents so push ships only deltas.

**References.** https://vlcn.io/docs/cr-sqlite/intro

#### [DATA-03] Single MaxOpenConns(1) pool serves reads too; spec's 'concurrent readers' rationale is unrealized
`low` · `effort: M` · `performance` · _adjusted_

**Problem.** One *sql.DB at MaxOpenConns(1) with _txlock=immediate serves both reads and writes, so reads serialize behind the single write-locked connection. Fine for Phase-0 CLI, but the spec's stated WAL concurrent-reader benefit is unrealized and the planned Phase-1 daemon (watcher + socket API + background readers) would serialize all reads.

**Evidence.** internal/state/store.go:209-210 (MaxOpenConns/MaxIdleConns=1); internal/state/store.go:242 (_txlock=immediate, global); internal/state/store.go:32 (single db handle); spec/12_DATA_MODEL_SQLITE.md:13 (concurrent-readers rationale).

**Recommendation.** Defer until the Phase-1 daemon. When built, adopt the two-pool pattern: keep the writer *sql.DB at MaxOpenConns(1) with _txlock=immediate, add a separate read-only *sql.DB (no _txlock, MaxOpenConns ~max(4,NumCPU), same foreign_keys/busy_timeout pragmas) sharing the WAL file, and route read methods to it. For now, soften spec/12:13 so the single-connection rationale is not overstated.

**Steps.**
1. Reword spec/12:13 to state the single connection is a Phase-0 simplification, not a realization of WAL read concurrency.
1. When the daemon lands: add a readDB field with a read-oriented DSN and MaxOpenConns ~NumCPU.
1. Point ListProjects/ProjectByPath/Summary/QuickCheck and other read paths at readDB; keep writes/migrations/backup on the writer db.
1. Close both handles in Store.Close().

**References.** https://kerkour.com/sqlite-for-servers

#### [DATA-04] Enum/status columns lack CHECK constraints and tables are not STRICT
`low` · `effort: M` · `correctness` · _confirmed_

**Problem.** Every enum-like TEXT column except secret_bindings has no DB-level constraint and no table is STRICT, so validity depends on inconsistent Go-side checks. A future code path or malformed sync payload can persist an invalid state the queries silently mishandle; without STRICT, INTEGER columns such as events.hlc accept text affinity.

**Evidence.** internal/state/migrations/00001_initial.sql:17,30,35,49,69,73,121,122,136,165 (enum columns, DEFAULT only); internal/state/migrations/00001_initial.sql:106-107 (only CHECKs); no STRICT in internal/state/migrations/; internal/state/store.go:565-569 (trust_state validated) vs store.go:723-805 (UpsertProject sets status/policy without a CHECK backstop).

**Recommendation.** Add a migration introducing CHECK(col IN (...)) for each enum column and, where feasible, rebuild hot tables as STRICT so type affinity is enforced, making the DB the source of truth.

**Steps.**
1. Author 00007_enum_constraints.sql adding CHECK constraints for trust_state, namespace status/materialization_policy, lfs_policy, materialization_state, dirty_state, worktrees/agent_runs/jobs status (use the SQLite 12-step table rebuild to add CHECK to existing tables).
1. Consider declaring rebuilt tables STRICT so events.hlc/seq reject non-integer values.
1. Add tests asserting an out-of-range status insert fails.
1. Document the allowed value sets in spec/12 next to each table.

**References.** https://www.sqlite.org/stricttables.html, https://www.sqlite.org/lang_altertable.html

#### [DATA-05] No detection or warning when state.db lives on a networked/synced filesystem
`low` · `effort: S` · `correctness` · _adjusted_

**Problem.** WAL's wal-index -shm file is documented as unsafe on NFS/network/synced filesystems. Nothing detects or warns if ~/.devstrap sits on a network or synced mount; the WAL+busy_timeout config is applied unconditionally.

**Evidence.** internal/state/store.go:231-244 (unconditional journal_mode(WAL)); no statfs/getfsstat/fstype detection anywhere in Go (grep); spec/12_DATA_MODEL_SQLITE.md documents the DB location without a networked-placement warning.

**Recommendation.** Detect the filesystem type of the state directory at Open() and in doctor and emit a non-fatal warning when it appears to be a network/synced mount, behind the existing platform adapter seam (Darwin statfs f_fstypename; Linux statfs f_type magics).

**Steps.**
1. Add a platform helper FilesystemKind(path) returning local/network/unknown via statfs (Darwin f_fstypename; Linux f_type magics like NFS_SUPER_MAGIC, SMB, FUSE).
1. Call it in doctor and warn when the state dir is non-local; optionally warn once at Open().
1. Add guidance to spec/12 (and spec/15) that state.db must be on a local filesystem.
1. Unit-test the detection mapping for common fstype constants.

**References.** https://sqlite.org/wal.html

#### [DATA-06] idx_events_order column order is suboptimal for per-device prev-hash lookup; PendingEvents cannot exploit it
`low` · `effort: S` · `performance` · _confirmed_

**Problem.** Two access paths do not align to idx_events_order: (1) the prev-hash fallback filters workspace_id+device_id with an hlc range but device_id follows hlc in the index, forcing an hlc scan with post-filter; (2) PendingEvents has no workspace_id predicate so the leading index column is unconstrained and an index-ordered scan is not guaranteed. Latent per-event cost during bulk sync apply, negligible today.

**Evidence.** internal/state/migrations/00002_event_ordering.sql:9 (idx_events_order definition); internal/state/store.go:1863-1871 (fallback prev-hash query); internal/state/store.go:1957-1962 (PendingEvents, no WHERE).

**Recommendation.** Add a covering index aligned to the per-device causal lookup, e.g. (workspace_id, device_id, hlc, id), keeping idx_events_order for global replay; verify both paths with EXPLAIN QUERY PLAN. Combine the PendingEvents workspace_id predicate with the DATA-02 cursor work.

**Steps.**
1. Add a migration creating idx_events_device_order on events(workspace_id, device_id, hlc, id).
1. Add `WHERE workspace_id = ?` to PendingEvents (combine with DATA-02 cursor work).
1. Add an EXPLAIN QUERY PLAN test for the prev-hash fallback asserting it uses the new index with no temp b-tree.
1. Reuse the existing idx_namespace_active EXPLAIN test pattern.

**References.** https://www.sqlite.org/queryplanner.html, https://www.sqlite.org/eqp.html

### Testing Strategy & CI

#### [TEST-01] No fuzz targets for any untrusted-input parser, including the secret scrubber
`medium` · `effort: M` · `testing` · _adjusted_

**Problem.** Three hand-rolled functions consume untrusted input with no fuzz coverage: the secret scrubber (internal/redact/redact.go), the .env parser (internal/envfile/parse.go), and the git remote normalizer (internal/git/git.go). Only enumerated fixed-case tests exist. Parse/normalize code is where panics and silent bypasses hide, and the test-plan spec never mentions fuzzing.

**Evidence.** `grep -rn "func Fuzz" --include='*.go' .` returns nothing; no testdata/fuzz directory. internal/redact/redact.go:125 Scrub, :170 Redactor.Scrub, :241 buildReplacer. internal/envfile/parse.go:30 ParseBytes (byte-level parser; parseLine:74, parseValue:103, parseDoubleQuoted:125). internal/git/git.go:445 CanonicalRemoteKey, :480 splitSCPLikeRemote. `grep -rni fuzz spec/` is empty.

**Recommendation.** Add native Go fuzz targets primarily for the two byte-level parsers (env, git URL) asserting no-panic and idempotent re-normalization, plus a redaction fuzz target asserting no-panic and that token-shape patterns and registered values never re-expose input. Run them with a bounded -fuzztime in CI and commit any crashers as a regression corpus. Note the registered-value path is already deterministic, so the parser surfaces are the higher-yield targets.

**Steps.**
1. Add `FuzzParseBytes` in internal/envfile asserting ParseBytes never panics and that any returned binding, re-serialized and re-parsed, is stable.
1. Add `FuzzCanonicalRemoteKey` in internal/git asserting no panic and that ValidateRemote-accepted inputs normalize deterministically (idempotent on re-application).
1. Add `FuzzRedactorScrub` in internal/redact seeding unicode/regex-special/overlapping values, asserting no panic and that neither registered values nor token-shaped substrings survive.
1. Add a CI step `go test -run=^$ -fuzz=Fuzz -fuzztime=30s ./internal/envfile ./internal/git ./internal/redact` and commit testdata/fuzz crashers.
1. Add a 'fuzz targets for env/git-URL/redact parsers' bullet to spec/16_TEST_PLAN.md.

**Example.**
```
func FuzzParseBytes(f *testing.F) {
	f.Add([]byte("A=1\nexport B=\"x y\"\n# c\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		bs, err := ParseBytes(raw, Options{})
		if err != nil { return }
		_ = bs // assert re-serialize/re-parse stability
	})
}
```

**References.** https://go.dev/security/fuzz/

#### [TEST-02] testscript e2e harness covers only init/status; the riskiest flows are validated in-process, bypassing the real exit-code contract
`medium` · `effort: M` · `testing` · _confirmed_

**Problem.** The only testscript covers init/status/version/doctor. The safety-critical flows the spec calls out (hydrate dirty-target refusal, the 'most important' fresh-worktree stale-base refusal, the agent-pr stale-base gate) are exercised only in-process via executeForTest, so the actual non-zero exit codes that users/CI/agents depend on for those error paths are never asserted end-to-end through the binary.

**Evidence.** find shows only cmd/devstrap/testdata/script/init_status.txtar. cmd/devstrap/script_test.go:12-27 (real-binary harness runs one dir). internal/cli/root_test.go:26 executeForTest (in-process), :805 stale-base finalize assertion, :849 agent-pr gate test. spec/16_TEST_PLAN.md:188-202 'This is the most important test.'

**Recommendation.** Extend the testscript suite to cover the killer-loop error paths through the real binary, asserting exit codes (`! exec`), stderr regexes, and on-disk state with a local bare-remote fixture. Align the CLAUDE.md/spec 'through the real binary' claim with the commands actually covered.

**Steps.**
1. Add testdata/script/worktree_stale_base.txtar: set up a local bare remote, advance it, run `worktree new --fresh-upstream`, assert `! exec devstrap worktree finalize` with stderr matching `--allow-stale-base`, then assert success with the flag.
1. Add testdata/script/hydrate_dirty_refusal.txtar asserting a non-zero exit and that local files survive on a dirty-target refusal.
1. Add testdata/script/agent_pr_stale.txtar driving a fake `gh` on $PATH and asserting `! exec devstrap agent pr` until --allow-stale-base.
1. Use testscript Params.Setup to isolate HOME/XDG/DEVSTRAP_HOME so scripts never touch a real ~/Code or keychain.
1. Either expand the harness or soften the CLAUDE.md claim to name the covered commands.

**References.** https://bitfieldconsulting.com/posts/cli-testing

#### [TEST-03] CI computes a coverage profile and discards it; vacuous-test guard checks only 3 packages and internal/id is untested
`medium` · `effort: M` · `process` · _adjusted_

**Problem.** CI generates a coverage profile that is never consumed (no threshold, no upload, no artifact), so packages can lose meaningful assertions without CI noticing. The vacuous-test guard enforces test-file presence on only 3 packages, and internal/id - which mints every workspace/device/project/event identifier - has no tests at all.

**Evidence.** ci.yml:73 `go test -race -covermode=atomic -coverprofile=coverage.out ./...`; no later reference to coverage.out in the workflow. ci.yml:64-66 loops only `internal/state internal/config internal/cli`. internal/id/ contains only id.go (0 test files). (Correction: internal/specdrift, which holds the spec-drift mapping logic, is tested in internal/specdrift/specdrift_test.go.)

**Recommendation.** Either enforce a meaningful coverage floor or stop producing the unused profile; add tests for internal/id; broaden the vacuous-test guard beyond the 3 named packages.

**Steps.**
1. Add a CI step that fails when total coverage drops below an agreed floor (parse `go tool cover -func=coverage.out` total), or upload coverage.out as an artifact so reviewers see deltas.
1. Add internal/id tests asserting the prefix format, rejection of invalid prefixes, uniqueness across N calls, and time-ordering of uuidv7 ids.
1. Extend the ci.yml vacuous-test loop to cover the remaining non-trivial packages (e.g. internal/git, internal/scan, internal/redact, internal/sync, internal/envfile) with documented exemptions for thin main wrappers like cmd/spec-drift.
1. Drop the misclaim about cmd/spec-drift mapping coverage; its logic is already tested via internal/specdrift.

**References.** https://go.dev/blog/cover

#### [TEST-04] golangci-lint gosec is narrowed to a 6-rule allowlist that disables hardcoded-credential and weak-crypto checks
`medium` · `effort: M` · `security` · _adjusted_

**Problem.** The gosec includes-allowlist silently disables G101 (hardcoded credentials) and the weak-crypto rules (G401/G501/G502) - exactly the rules a tool whose threat model centers on secret material and age/Ed25519 crypto identities should keep on. The linter set also omits errorlint despite heavy errors.Is/As use, and issues.max-same-issues is unset so findings can be truncated.

**Evidence.** .golangci.yml:6-13 (six linters), :15-22 (gosec includes allowlist). `grep -rln 'errors.Is|errors.As'` matches 12 non-test files (errorlint applies). NOTE: store.go row handling is already correct (store.go:540/549/558 use `rows, err :=` + deferred Close + `return rows.Err()`), so sqlclosecheck/rowserrcheck are not needed there.

**Recommendation.** Remove or invert the gosec includes-allowlist so credential/crypto rules run, add errorlint, and set issues.max-same-issues: 0. Drop the unjustified sqlclosecheck/rowserrcheck rationale tied to store.go (those sites are already correct); add them only as cheap future insurance, not to fix an existing defect.

**Steps.**
1. Replace the gosec `includes` block with an `excludes` of only justified suppressions, ensuring G101/G401/G501 stay on (or remove includes entirely to run the default set).
1. Add errorlint to linters.enable (12 files already rely on errors.Is/As).
1. Set `issues: { max-same-issues: 0, max-issues-per-linter: 0 }` so nothing is silently truncated.
1. Run `golangci-lint run` locally and triage only genuinely new findings; do not expect store.go rows fixes since those sites already close rows and check rows.Err().

**Example.**
```
linters:
  enable: [errcheck, gosec, govet, ineffassign, staticcheck, unconvert, errorlint]
  settings:
    gosec: {}        # no includes: allowlist
issues:
  max-same-issues: 0
```

**References.** https://securego.io/docs/rules/rule-intro.html

#### [TEST-05] govulncheck is installed with @latest (unpinned) and never run on a schedule
`low` · `effort: S` · `security` · _adjusted_

**Problem.** The vulnerability gate installs govulncheck@latest (a future release could change behavior or break the build with no commit), runs once per matrix OS for an identical Go call graph, and has no scheduled trigger, so CVEs disclosed in already-merged dependencies are not caught between PRs.

**Evidence.** .github/workflows/ci.yml:74-77 `go install golang.org/x/vuln/cmd/govulncheck@latest` then `govulncheck ./...`, inside the os-matrix test job (ci.yml:45-49). `grep -rn 'schedule\|cron' .github/workflows/` returns nothing.

**Recommendation.** Add a daily scheduled run so post-merge CVEs surface, run the check once (single-OS job) rather than per matrix entry, and pin govulncheck to a specific version (or the official action by SHA) for build reproducibility. The scheduled run is the highest-value change.

**Steps.**
1. Add a `schedule: - cron: '0 6 * * *'` trigger that runs govulncheck against the default branch.
1. Move the vulnerability check into the single-OS spec-drift/lint job (or its own job) so it runs once, not per matrix entry.
1. Pin `@latest` to a specific version, e.g. `govulncheck@v1.1.4`, or use `golang/govulncheck-action` pinned by commit SHA.
1. Keep the go.mod toolchain current so security point releases are picked up.

**References.** https://go.dev/doc/security/best-practices, https://github.com/golang/govulncheck-action

#### [TEST-06] The production fsnotify watcher adapter has no tests and the concurrent code has no goroutine-leak detection
`low` · `effort: M` · `testing` · _adjusted_

**Problem.** The fsnotify-backed Darwin/Linux watcher adapter (the eventual production watcher) has complex debounce/coalescing and manual-timer concurrency that is entirely untested - only the PollWatcher fallback has a test - and no goroutine-leak detection guards the package's concurrent code paths.

**Evidence.** internal/platform/fsnotify_watcher.go:13-119 NativeWatcher.Watch (timer arm/disarm:48-66, flush:67-79, select loop:81-118). internal/platform/platform_test.go covers only PollWatcher (:25), Detect (:12), keychain (:53,:64) - no NativeWatcher test. No goleak usage anywhere (grep empty). Watcher is unwired: spec/00_START_HERE.md lists the daemon/FSEvents watcher as not implemented.

**Recommendation.** Before the Phase 1 daemon consumes it, add a NativeWatcher test using a temp dir asserting create/write produces a coalesced scan event, that the maxLatency cap fires, and that context cancellation returns context.Canceled with no leaked goroutine. Add go.uber.org/goleak (TestMain VerifyNone) to internal/platform and internal/sync.

**Steps.**
1. Add internal/platform/fsnotify_watcher_test.go: create a temp dir, start NativeWatcher with short Debounce/MaxLatency, write files, and assert a single coalesced FSEventScan is delivered.
1. Assert the maxLatency cap path (:106-111) flushes under a burst, and that cancelling ctx returns context.Canceled and stops the goroutine.
1. Add go.uber.org/goleak and a TestMain calling goleak.VerifyTestMain in internal/platform and internal/sync to catch leaked watcher/timer goroutines.
1. Sequence this before the Phase 1 daemon wiring so the watcher is covered when it first goes on an active path.

**References.** https://github.com/uber-go/goleak

### Go Implementation Quality (cross-cutting)

#### [CODE-01] ApplyEvents aborts the entire sync batch on one event's hash-chain break, wedging forward progress for events sorted after it
`medium` · `effort: M` · `correctness` · _adjusted_

**Problem.** In ApplyEvents the clock-skew guard deliberately records a conflict and continues so the rest of the batch converges, but the hash-chain-break path records a conflict and then returns, aborting every remaining (sorted-after) event in the batch including unrelated events from other devices. Because the offending event is never inserted into `events` and the pull cursor is hardcoded to 0, the next sync re-delivers it, re-aborts, and events ordered after it can never apply.

**Evidence.** internal/sync/events.go:161-168 (return err on ErrEventHashChain) vs internal/sync/events.go:143-148 (continue on skew); ErrEventHashChain at internal/state/store.go:40 and internal/state/store.go:1842-1845; reached via tx.InsertEvent store.go:1798; internal/cli/sync.go:40 hub.Pull(ctx, 0) re-pulls all events.

**Recommendation.** Treat a hash-chain break like the skew case: record the conflict and `continue` so the rest of the batch (and other devices' events) still apply. The hash-chain conflict details are already stable, so insertConflict will dedup the recurring broken event and no unbounded growth results; a per-event quarantine marker is an optional optimization, not required for correctness.

**Steps.**
1. After insertEventHashChainConflict succeeds, `continue` instead of `return err`, mirroring the SYNC-3 skew branch.
1. Reserve `return err` for non-conflict (infrastructure) failures from WithTx (DB write/commit errors).
1. Optionally persist a per-(device_id,event_id) quarantine marker so a re-pulled broken event is skipped without re-running validation.
1. Add a sync test: a batch of [validA(deviceX), brokenChain(deviceY), validB(deviceX)] applies validA and validB and records exactly one conflict.

**Example.**
```
if errors.Is(err, state.ErrEventHashChain) {
    if cErr := insertEventHashChainConflict(ctx, st, event, err); cErr != nil {
        return errors.Join(err, cErr)
    }
    continue // keep converging, like the skew branch
}
return err
```

**References.** https://pkg.go.dev/errors

#### [CODE-02] Skew-quarantine conflicts dedup on volatile details JSON, so every resync inserts a new duplicate conflict row
`low` · `effort: S` · `correctness` · _adjusted_

**Problem.** quarantineSkewedEvent serializes a per-call OffsetMS into the conflict details, but insertConflict only dedups on an exact details_json match. Since the quarantined event is never stored and the pull cursor is 0, it is re-quarantined with a fresh OffsetMS on every sync, producing a brand-new conflict row each time for a single persistently-skewed peer.

**Evidence.** internal/sync/events.go:137 (`now`), events.go:143/176-181 (OffsetMS into details), skewConflictDetails.OffsetMS at events.go:47; dedup on full details_json at internal/state/store.go:1589; skip-without-insert at events.go:147; full re-pull at internal/cli/sync.go:40.

**Recommendation.** Make the idempotency key stable: drop the volatile OffsetMS from the stored skewConflictDetails (it is already logged at events.go:174-175), or dedup sync-generated conflicts on a stable (type, event_id) subset rather than full details_json.

**Steps.**
1. Remove OffsetMS from skewConflictDetails, or round/omit it from the persisted details (keep it only in the warn log line at events.go:174-175).
1. Alternatively, change insertConflict for sync-generated conflicts to match on a stable (type, event_id) key, or add an explicit dedup-key column.
1. Add a test that calls ApplyEvents twice with the same skewed event and asserts exactly one open conflict.

**References.** https://go.dev/wiki/CodeReviewComments

#### [CODE-03] Store.WithTx uses an inline (non-deferred) rollback, so a panic inside the closure leaks the single pooled DB connection
`low` · `effort: S` · `implementation` · _adjusted_

**Problem.** WithTx rolls back inline only on the error path; if fn panics mid-transaction the rollback never runs and the transaction's connection is never returned to the one-connection pool. The current one-shot CLI just dies on panic, but a future Phase-1 in-process daemon that recovers per-request panics would block forever on the next state access.

**Evidence.** internal/state/store.go:366-374 (inline `_ = tx.Rollback()`, no defer); contrast internal/state/store.go:755 (`defer func() { _ = tx.Rollback() }()`); pool cap at internal/state/store.go:209.

**Recommendation.** Use the idiomatic defer-rollback pattern so rollback runs on both error and panic; Commit on success and let the deferred Rollback become a harmless no-op (sql.ErrTxDone) on the already-committed tx.

**Steps.**
1. Replace the inline rollback with `defer func() { _ = tx.Rollback() }()` immediately after BeginTx.
1. Return fn's error directly; on success call Commit (post-commit Rollback returns sql.ErrTxDone and is ignored).
1. Audit other hand-rolled BeginTx sites for the same pattern (UpsertProject is already correct).
1. Add a test that panics inside WithTx and asserts a subsequent query still succeeds.

**Example.**
```
tx, err := s.db.BeginTx(ctx, nil)
if err != nil { return fmt.Errorf("begin transaction: %w", err) }
defer func() { _ = tx.Rollback() }()
if err := fn(&Tx{tx: tx, workspaceID: workspaceID}); err != nil { return err }
return tx.Commit()
```

**References.** https://go.dev/doc/database/execute-transactions, https://go.dev/blog/defer-panic-and-recover

#### [CODE-04] Deferred Close discards flush errors on writable secret/audit files (ciphertext env blob, agent log)
`medium` · `effort: S` · `correctness` · _confirmed_

**Problem.** Two writable files holding security-relevant data are closed with a deferred error-discarding Close, so a final-flush failure that the preceding Write did not surface is silently lost: writeEnvBlob can report success on a truncated encrypted secret blob, and runAgentProcess can silently truncate the 0600 audit log.

**Evidence.** internal/cli/env.go:312-326 (env blob: defer _ = file.Close(); returns without checking Close); internal/cli/agent.go:394-398 and agent.go:419-423 (agent log: defer _ = logFile.Close(); only scrubbers folded into errors.Join); correct sibling at internal/cli/env.go:428.

**Recommendation.** For writable files, observe the Close error on the success path (named return + deferred closure that sets err only if no prior error), and call Sync() before Close where durability matters for the ciphertext blob.

**Steps.**
1. In writeEnvBlob, replace the deferred discard with `defer func() { if cErr := file.Close(); cErr != nil && err == nil { err = ... } }()` (named return) and add file.Sync() before Close for the encrypted blob.
1. In runAgentProcess, fold logFile.Close() into the returned error via errors.Join alongside the scrubber Close calls at agent.go:419.
1. Add a CI grep/golangci-lint check for `defer .*\.Close()` on os.OpenFile/os.Create write handles.

**Example.**
```
func writeEnvBlob(...) (err error) {
  f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
  if err != nil { return ... }
  defer func() { if cErr := f.Close(); cErr != nil && err == nil { err = fmt.Errorf("close env blob: %w", cErr) } }()
  if _, err = f.Write(ciphertext); err != nil { return ... }
  return f.Sync()
}
```

**References.** https://www.joeshaw.org/dont-defer-close-on-writable-files/, https://pkg.go.dev/os#File.Close

#### [CODE-05] state.Open ignores the caller's context and substitutes context.Background(), severing cancellation at DB open
`low` · `effort: M` · `implementation` · _confirmed_

**Problem.** state.Open performs blocking DB work (db.Ping and foreignKeyCheck with context.Background()) and takes no context, even though every caller reaches it from a cobra RunE with cmd.Context() (the SIGINT/SIGTERM-aware signal context). A Ctrl-C during a slow/locked open cannot be cancelled, and a context.Background() is synthesized mid-call-chain.

**Evidence.** internal/state/store.go:203 (Open signature), store.go:212 (db.Ping()), store.go:220 (foreignKeyCheck(context.Background(), db)); internal/cli/root.go:141-146 (openState drops ctx); cmd/devstrap/main.go:13 (signal.NotifyContext available).

**Recommendation.** Add ctx as the first parameter to Open (and openState), use db.PingContext(ctx), and pass ctx into foreignKeyCheck instead of context.Background().

**Steps.**
1. Change signature to `Open(ctx context.Context, path string)` and replace db.Ping() with db.PingContext(ctx).
1. Pass ctx through to foreignKeyCheck (store.go:220).
1. Update openState (root.go:141-146) and all call sites to thread cmd.Context().
1. Keep a thin `Open(path)` shim only if needed for tests, defaulting to context.Background() explicitly there.

**References.** https://go.dev/blog/context-and-structs, https://pkg.go.dev/database/sql#DB.PingContext

#### [CODE-06] Namespace + git_repos/draft upsert SQL is copy-pasted between Store.UpsertProject and Tx.UpsertProject and can silently drift
`low` · `effort: M` · `implementation` · _confirmed_

**Problem.** The git_repos and draft_projects upsert blocks (including the lfs_policy-preservation CASE and the clone_filter literal) are duplicated between Store.UpsertProject and Tx.UpsertProject. A future change to one path (new column, changed conflict rule) can be missed in the other, producing data that differs depending on whether a project was added locally or applied via a sync event.

**Evidence.** internal/state/store.go:763-772 (Store git_repos branch) identical to internal/state/store.go:828-837 (Tx git_repos branch); draft branches at store.go:776-785 vs store.go:841-850; reusable seams: sqlExecutor at store.go:169 and upsertNamespaceTx at store.go:854.

**Recommendation.** Extract the shared git_repos/draft upsert into one helper that takes the common *sql.Tx (or sqlExecutor) and have both UpsertProject variants call it; keep only path-specific behavior (device_project_state writes) in Store.UpsertProject.

**Steps.**
1. Define `upsertProjectRows(ctx, tx *sql.Tx, workspaceID string, pk pathkey.Path, params UpsertProjectParams) (NamespaceEntry, error)` wrapping the namespace + git_repos/draft logic.
1. Route both UpsertProject variants through it; leave device_project_state handling only in Store.UpsertProject (store.go:786-800).
1. Add one test exercising both entry points against identical params and asserting identical git_repos rows (especially lfs_policy preservation when params.LFSPolicy is empty).

**References.** https://go.dev/wiki/CodeReviewComments, https://go.dev/doc/modules/layout


### Systemic themes (from the completeness critic)

These recurring patterns matter more than any single finding — they are the shape of the work remaining:

1. **Aspirational specs vs. implemented code.** The spec corpus and `CLAUDE.md` describe a multi-device, daemon-backed product, but only a single-machine CLI exists. The entire "Local engine" tier (`devstrapd`: job queue, Unix-socket API, reconciler, watcher wiring, service installers) and the multi-device killer loop are unbuilt or non-functional end-to-end (`ARCH2-01/03/04`, `PROD-04`, `PLAT-03/05`).
2. **Security theater — reassuring names over real enforcement.** `guarded` policy, the "no-secret" agent env, "signed events from day one", and keychain key custody all carry names/docs implying protection but fail open or are trivially bypassed (`AGEN-01/02/03/04`, `SECU-02/03`, `SECR-04`). These are MVP gaps that are *security-relevant* yet presented as safe.
3. **Dead / half-wired infrastructure ahead of behavior.** `sync_cursors`, `event_delivery`, `jobs`, `device_sync_state` tables; the `sync.HLC` type; `PollWatcher`; `env_ready`/`tooling_ready` columns; and conflict rows that are written but never read all create a false impression of completeness — schema/types exist before the behavior (`ARCH2-02`, `SYNC-02/04`, `DATA-02`, `PROD-01/02`, `PLAT-03`).
4. **Single-source-of-truth duplication and drift.** HLC logic duplicated between `store.go` and `sync/hlc.go`; `UpsertProject` SQL copy-pasted (`CODE-06`); and ignore/prune/secret/deny lists hardcoded and divergent across scan, watcher, and agent because the `spec/11` `.devstrapignore` compiler was never built (root cause of `PLAT-01`, `PLAT-04`, `AGEN-05`).
5. **Correctness validated in-process, not against the real binary contract.** The testscript e2e harness covers only `init`/`status`; the riskiest flows are tested in-process and bypass the exit-code/`--json` contract (`TEST-02`, `CLI-01..04`); no untrusted-input parser is fuzzed (`TEST-01`).
6. **GitHub / single-`origin` monoculture.** Every remote and PR path assumes one `origin` remote and the `gh` CLI, so remote-less, multi-remote, non-`origin`, and non-GitHub-forge projects are silently unsupported (`FORGE-*`, `NOVCS-*`, `GIT-05`).

### Coverage gaps (subsystems the audit found materially under-built)

Beyond the 65 findings, these whole subsystems are specified but absent — each is a candidate epic:

- **Audit-log subsystem (`spec/15`) is entirely unimplemented.** The spec mandates a persisted, Ed25519-signed audit log (project/env/device/worktree/agent/PR/destructive/conflict events). There is no `audit_log` table or recording code — destructive/trust actions leave no signed record.
- **Ignore-compiler (`spec/11`) does not exist.** No canonical `.devstrapignore` compiling to `.gitignore`/`.dockerignore`/draft-sync/agent-denylist; OS junk (`.DS_Store`, `Thumbs.db`) is filtered nowhere. Root cause of the duplication in theme #4.
- **Remote-less / non-VCS / multi-remote projects** — see Section 2 (`NOVCS-*`).
- **Non-GitHub forges** — see Section 3 (`FORGE-*`).
- **Daemon crash-recovery & orphaned-state cleanup.** `spec/14`'s "requeue leased jobs and prune partial clones/worktrees" has no implementation or reaper; interrupted clone temp dirs and partial worktrees leak.
- **Observability / operability.** No metrics/telemetry plumbing, no log rotation, no daemon status/logs surface despite `spec/14` observability gates.
- **Performance / scalability at scale.** Single-threaded `filepath.WalkDir` with no incremental mtime markers and `MaxOpenConns(1)`; the `spec/11` "first visible tree under 5 minutes on a large ~/Code" target has no benchmark fixture.
- **Upgrade / migration rollback safety.** Down-migration data-loss safety and the `spec/14` upgrade-and-rollback runbook are untested.
- **Cross-process concurrency on one `state.db`.** Two concurrent `devstrap` processes have no coordination model beyond the in-process single-writer pool + SQLite busy-timeout; the failure mode is unspecified/untested.

### Highest-impact findings (start here)

1. **`AGEN-01`** — argv-substring command/file policy is trivially bypassed by any interpreter; the default `guarded` agent has full filesystem read + network exfil. The core differentiator is security theater.
2. **`SECU-02` / `AGEN-02`** — agent subprocess inherits `HOME` and `SSH_AUTH_SOCK`, handing a live Git/SSH credential capability to semi-trusted agent code; combined with `AGEN-01` this is a working exfil path that contradicts the documented no-secret env.
3. **`SECR-01`** — `hydrate` re-emits captured secrets in unescaped double quotes (`$`/backtick), enabling downstream command substitution and silent truncation — undoing safe-by-default capture.
4. **`SECU-03`** — event signature verification fails open for unknown/keyless devices, defeating the day-one signed-event trust model against the explicitly semi-trusted hub.
5. **`SYNC-02` / `SYNC-01`** — the executed HLC is a duplicated copy in `store.go` with no skew clamp / authority reset (the unit-tested `sync.HLC` is dead code), and apply has no HLC-dominance guard, so same-remote convergence is order-of-arrival, not deterministic.
6. **`SECR-04` / `SECU-01`** — key custody silently downgrades to a `0600` plaintext age private key on *any* keychain error (not just unavailability).
7. **`SYNC-05` / `CODE-01`** — one hash-chain break aborts the entire apply batch, wedging convergence for all later-sorted events (sync liveness failure).
8. **`ARCH2-01`** — engine logic is fused into `internal/cli`; the CLI/daemon seam the spec promises does not exist, so the daemon phase starts with a large, risky extraction.

---

## Section 5 — Cross-machine working-state sync ("forgot to push") — evaluating the Dropbox idea

> **Your idea:** "If a user forgets to push, then goes to another machine, DevStrap should include a Dropbox-like file-update mechanism AND validate the git state across all the user's machines."
>
> This was pressure-tested with a dedicated workflow: 4 Exa-researched angles → 4 competing designs → adversarial scoring → synthesis. (Note: some design *scores* were lost to a mid-run session limit; the validation-plane design was fully scored at **4.33 avg, git-safety 5/5**, and the full synthesis completed.)

### Verdict: the instinct is right; literal file-sync is the wrong primitive

**Reject continuous/Dropbox-style file-sync of code outright.** The research found a hard, *official* consensus (the Git project's own FAQ, authored by a Git maintainer) that no part of a repository should ever be live-synced by a file-sync engine. The failure class is severe and partly **permanent**:

- Torn `.git/index` mid-write → `fatal: index file corrupt` / `bad index file sha1 signature`.
- Sync engines resolve their own conflicts by creating duplicate files (`refs/heads/main 2`, `index.sync-conflict-…`) that git silently mis-reads.
- Divergent refs per machine leave objects unreferenced → **`git gc` prunes them = real data loss**, not a transient error.
- `index.lock` contention; on cloud-tiered FS (OneDrive/iCloud), placeholder objects make git hang.

Git's atomicity depends on POSIX atomic-rename + lock-file choreography that file replication cannot reproduce. This is also *why* DevStrap's fresh-worktree-from-remote invariant is correct — concurrent writers to one repo are unsafe even without a sync engine.

So **split your one sentence into its two real halves and serve each with a git-native mechanism.**

### The four designs evaluated

| Design | Job | git-safety | Verdict |
|---|---|---|---|
| **1. Validation plane** (signed read-only repo-state snapshots) | "validate / be aware" | **5/5** (avg 4.33) | ✅ **Wins the awareness job; ships in Phase 0** |
| **2. Auto-WIP push to per-device git refs** | "recover my forgotten content" | high (git-native) | ✅ **Wins the recovery job; Phase 1** |
| **3. Encrypted diff/tar bundle** (`draft.snapshot.created`) | non-git / untracked fallback | high | ◐ **Narrow fallback only** |
| **4. Continuous file-sync engine** (Mutagen/Syncthing-style) | literal "Dropbox" | low | ❌ **Rejected — git corruption + invariant violation** |

### Recommended design: a three-layer hybrid (human-convenience plane, walled off from the agent plane)

**Layer A — Validation plane (the smoke detector; Phase 0, no daemon needed).**
Each device publishes a signed, read-only `repo.gitstate.observed` event: branch, HEAD sha, upstream sha, and counts for dirty-tracked / untracked / unmerged / ahead / behind / stash. Capture uses `git --no-optional-locks status --porcelain=v2 --branch` so it **never writes `.git/index`**. Apply is mirror-only (HLC last-writer-wins per device+path). Then `devstrap status --all-devices` / `doctor` can warn: *"⚠ mac-mini has 3 uncommitted + 1 unpushed in work/nclh/foc-models (last seen 2h ago)."*

**Layer B — WIP recovery via per-device git refs (the fire brigade; Phase 1).**
On a debounced trigger, run `git stash create` (produces a commit object **without touching the worktree or index**), then `git push origin <sha>:refs/devstrap/wip/<device_id>/<path_key>` over git's own integrity-checked, forge-agnostic transport, and emit `repo.wip.pushed`. Machine B fetches into the same non-default ref namespace; `devstrap wip show/apply` materializes it into the working tree **on explicit command — never as a branch, never as a worktree base.** This is what actually solves "forgot to push and machine A is asleep," which Layer A alone cannot.

**Layer C — Encrypted bundle fallback (narrow; Phase 3).**
Only for `draft_project`/non-git folders/untracked-only recovery where there's no remote to push a ref to — build out the already-specced `draft.snapshot.created` flow (`spec/07:442-467`) using `internal/envbundle.Encrypt` to approved device recipients. **Not** the general code path; git refs (Layer B) are strictly safer and cheaper for tracked content.

**INVARIANT INTERLOCK (non-negotiable):** every WIP ref/bundle lives under reserved `refs/devstrap/wip/*`, flagged human-plane. The fresh-worktree resolver (`origin/<default_branch>`, `spec/10:13-16`) must resolve base **only** from the default branch and is *forbidden* from reading `refs/devstrap/wip/*`. Better still, use the validation plane to make the invariant legible: *"you have unpushed work on mac-mini; an agent starting here bases from origin and will NOT see it."*

### Phased sequencing (tied to the roadmap)

1. **Phase 0 (now):** Layer A only, capture-on-command, riding the existing `devstrap sync --hub-file` transport. **Guardrail:** render missing/stale snapshots as *"never synced / last seen N ago"* — never silent all-clear (a device that forgot to push likely also never published, and silence-as-safety is the dangerous default for a safety tool).
2. **Phase 1 (daemon):** FSEvents/fsnotify re-capture on git activity (debounced) + periodic reconciliation; add Layer B WIP push/fetch (daemon-triggered). Awareness is only actionable once Layer B can move bytes without machine A awake.
3. **Phase 2 (hub + device enrollment):** enroll remote Ed25519/X25519 pubkeys so remote gitstate/wip events are **verified**, not attributed-but-unverified. Closes the spoofed-peer gap.
4. **Phase 3:** Layer C encrypted bundles for the no-remote gap.
5. **Never:** Design 4 continuous file-sync — out of scope, documented as rejected in `spec/04` so it isn't relitigated.

### Concrete schema / events / CLI (Layer A is shippable now)

```text
EVENT repo.gitstate.observed   # rides existing events table (HLC/seq/sig via InsertLocalEvent)
  payload { namespace_id, device_id, branch, head_sha, upstream_sha,
            dirty_tracked:int, untracked:int, unmerged:int,
            ahead:int, behind:int, no_upstream:bool, stash_count:int,
            captured_at, fetched:bool }

EVENT repo.wip.pushed (Phase 1)
  payload { namespace_id, device_id, ref, sha, base_sha, captured_at }

REF namespace (Phase 1): refs/devstrap/wip/<device_id>/<path_key>
  - non-default, integrity-checked git transport, forge-agnostic
  - FORBIDDEN as a fresh-worktree base

TABLE migration 00008_gitstate_mirror.sql  -- SIDECAR, device_id is opaque TEXT, NO FK to devices(id)*
  PRIMARY KEY(device_id, namespace_id); counts + branch + shas
  + source_event_hlc + attributed_unverified(default 1, →0 after Phase-2 enrollment) + captured_at
* The devices(id) FK (migration 00001:78) is the trap: remote devices aren't enrolled until Phase 2,
  so a sidecar table with opaque device_id is required — this also resolves the "extend vs sidecar" question.

CLI (Phase 0):  devstrap gitstate capture [--fetch] · devstrap status --all-devices · devstrap doctor
CLI (Phase 1):  devstrap wip push|status|fetch|show|apply|drop <project> [--device X]
```

### Key actionable steps
1. `internal/git/git.go`: add `--no-optional-locks` to the read-only status path (verified not currently used) and a `GitSnapshot` fn; reuse the existing `# branch.ab +A -B` parse in `DirtyState` (`git.go:399-435`); offline ahead/behind vs last-fetched origin, opt-in `--fetch`.
2. `internal/sync/events.go`: add `EventRepoGitStateObserved` const + `GitStatePayload`; emit via `st.InsertLocalEvent` (reuses HLC/seq + Ed25519 signing); add a **mirror-only** HLC-gated case to `applyEventTx` (no filesystem reconciliation).
3. `internal/state/migrations/00008_gitstate_mirror.sql`: new sidecar table, **opaque** `device_id` (no FK), `attributed_unverified` flag.
4. `internal/cli`: `gitstate capture`, `status --all-devices`, doctor section; warn on dirty/unpushed/stash>0; **always** print snapshot age; remediation is git-native text, never an automated move.
5. **Secret hygiene:** publish counts + branch + HEAD sha only — **never** changed-file path lists; route branch names through `internal/redact`; optional age-encryption of the payload for paranoid teams.
6. **Invariant guard:** add a test that the base resolver refuses any `refs/devstrap/wip/*`, and a testscript e2e proving an agent worktree created after a WIP push still bases from origin default and does **not** see the WIP content.

### Top risks
- **Silent staleness / false all-clear** (Phase 0): absence of a snapshot reads as "safe." Must render "never synced / last seen N ago"; Phase-1 daemon periodic capture is effectively required, not optional.
- **Layer A doesn't move bytes** — ship Layer B in Phase 1; don't ship validation alone and call the request done.
- **Attributed-but-unverified** remote snapshots until Phase-2 enrollment — a spoofed peer could publish a false/scary snapshot; flag clearly, gate trust on Phase 2.
- **WIP refs accumulate** on origin (one per device per project) and leak branch-name/intent — need TTL/GC of `refs/devstrap/wip/*`; confirm the forge doesn't auto-create PRs/notifications for non-default refs.
- **Capture cost** on large/many worktrees — gate behind mtime/FSEvents incremental triggers; `--no-optional-locks` avoids lock contention but not CPU.
- **Scope creep toward Design 4** — hold the line; document the rejection in `spec/04`.

---

## Section 6 — Sync hub architecture & services (Exa-validated)

> Your request: "validate all aspects of how to do it — architecture, services, etc." This ran a 6-angle Exa workflow (transport, hub architecture, blob/E2E storage, device trust, consistency, deployment) → DevStrap-mapped recommendations → synthesis. The session limit cut several individual recos, but the **synthesis completed** and is grounded in the actual code; the transport and deployment recos survived in full.

### Reference architecture: a thin, zero-knowledge, store-and-forward hub

**`DevStrap Hub` is a dumb relay.** It never orders, authenticates, or decrypts — those guarantees live *off the wire* on the client. Three planes:

- **Local plane (authoritative, unchanged):** each device's SQLite (WAL, single-writer) is the source of truth; all CLI commands work fully offline (`spec/03:141-143`). The future daemon is only a freshness hint.
- **Network plane A — namespace event log:** append-only, Ed25519-signed events POSTed up / pulled down, ordered by a global **HLC int64 that is simultaneously the ordering key, the resume cursor, and the SSE `Last-Event-ID`**. Integrity = HLC ordering + `ContentHash` + `prev_event_hash` chain + per-device Ed25519 signatures (`internal/sync`, `internal/devicekeys`, `state.Event` at `store.go:150-160`).
- **Network plane B — working-state (human plane):** uncommitted/unpushed code captured as a git bundle/diff, age-encrypted to device recipients, content-addressed as `age_blob:<sha256>` (mirroring `internal/envbundle/bundle.go:20-50`), PUT to the blob store, referenced by a signed `draft.snapshot.created` event. **Never consulted by agent base resolution** — agents stay anchored to `origin/<default_branch>` (`spec/03:172-184`, `AGENTS.md`).

**Encryption boundary — what the server can and cannot see.** Can see (routing metadata): event id, workspace id, device id, HLC, type string, `content_hash`, `prev_event_hash`, signature, byte sizes, timestamps, blob `sha256` refs, TLS client-cert identity. **Cannot see:** plaintext code, secrets/env values, blob contents, WIP/draft contents, or any private key (`spec/15:42-47`, `spec/07:269-285`). A compromised hub can reorder/replay/omit/substitute — all detected client-side by the off-wire integrity chain (`spec/15:53`).

### Transport: HTTPS + SSE (defer everything heavier)

```text
POST /v1/{ws}/events                 # push (idempotent on event id)
GET  /v1/{ws}/events?after=<hlc>     # catch-up pull (<hlc> = sync_cursors.last_hlc_applied)
GET  /v1/{ws}/stream  (SSE)          # live notify only; Last-Event-ID=<hlc>; ': ping' heartbeats
PUT/GET /v1/{ws}/blobs/{sha256}      # encrypted bundles, content-addressed
410 Gone {snapshot_required:true}    # maps 1:1 to internal/sync.ErrSnapshotRequired (hub.go:15)
```

SSE is a **freshness hint only** — correctness rests entirely on cursor-based pull, preserving the no-daemon guarantee. Automatic HTTP long-poll fallback when a proxy strips SSE; jittered backoff (1s–60s) honoring `Retry-After`/429. **Deferred:** WebSocket (full-duplex overkill for infrequent upstream), gRPC (needs h2c end-to-end; protobuf gives ~zero benefit on opaque ciphertext), QUIC/HTTP3 (UDP often blocked; revisit Phase 3+ for roaming agent runners), P2P/NAT traversal (biggest complexity sink — start relayed, opportunistically upgrade over a tailnet only for large bundles), mobile push.

### Encryption · Device trust · Consistency

- **Encryption (E2E / zero-knowledge):** age X25519 recipients (one per approved device) encrypt every blob and sensitive payload; Ed25519 (domain-separated) signs every event. Reuse `internal/envbundle.Encrypt` and `internal/devicekeys` Sign/Verify. Private identities stay in OS keychain / `~/.devstrap/keys` (0600), never SQLite/hub. On device revoke: re-encrypt affected bundles to remaining recipients + flag `secret_bindings.needs_rotation` (already surfaced in `doctor`). Enforce with a **hub test asserting it can decrypt nothing**.
- **Device trust = transport identity:** each device's Ed25519/age identity mints a TLS **client certificate** bound to `dev_<id>`; the hub verifies it per-handshake and **rejects any device whose `devices.trust_state` is revoked/lost** (`spec/15:132-140`) — revocation must be enforced at the TLS layer (handshake check or short-lived certs) or it's cosmetic. Token auth is the headless/CI fallback (mirror the `DEVSTRAP_NO_KEYCHAIN` gating). New devices need explicit `devices approve` + (production) out-of-band fingerprint confirmation. **Hub trust never gates correctness** — clients verify signatures + hash chain + HLC locally.
- **Consistency (local-first eventual):** append-only HLC-ordered log; per-peer `sync_cursors(last_hlc_applied)`; push → pull-after-cursor → apply in deterministic HLC/device/event-id order → write `event_delivery` + cursor transactionally; idempotent on event id. Conflicts are recorded, never silently resolved (same-path/different-remote, rename-target, delete-vs-dirty, clock-skew quarantine, hash-chain break — all already in `internal/sync`). **Build the full-state snapshot exchange BEFORE enabling hub retention GC**, or a device past retention silently diverges (`spec/07:235`).

### Deployment: self-hostable single Go binary

Ship `devstraphub` as **one Go binary in the same module**, reusing `internal/state`, `internal/sync`, `internal/devicekeys`, `internal/redact`. Two reference topologies:
- **(A) Home hub** — binary on an always-on machine, SQLite event store + filesystem blob store, exposed over a **Tailscale/Headscale tailnet** with no public TLS (lowest friction).
- **(B) VPS/cloud hub** — same binary behind **Caddy** (auto Let's Encrypt) with Postgres + MinIO/S3 for blobs.

Forge-agnostic (the hub never talks to GitHub/GitLab; git materialization stays client-side). **Reject** the hidden-Git backend (`spec/07` Option D — reintroduces merge conflicts); object storage is for blobs only, never the event-log transport.

### Service matrix

| Component | build/buy/oss | Recommended option |
|---|---|---|
| `devstraphub` event+blob server | build | Go single binary, std `net/http` (SSE = flushed `text/event-stream`); optional chi/echo router |
| Hub event store backend | build (thin) | SQLite (WAL) home hub; Postgres for VPS/multi-tenant; insert-only events table mirroring `spec/12` |
| Encrypted blob store | oss-self-host | Local FS (home hub); MinIO/any S3 (VPS) — blobs only |
| Client transport (in the binary) | build | `net/http` + a ~80-line `bufio.Scanner` SSE reader (no dep) + long-poll fallback; promote `FileHub` → `Hub` interface, add `HTTPHub` |
| TLS / public ingress | oss-self-host | Caddy (auto Let's Encrypt), OR tailnet-only and skip public TLS |
| Device auth / revocation at hub | build | `crypto/tls` client-cert verify → `dev_<id>` → cross-check `devices.trust_state`; token fallback for CI |
| Encryption / signing | reuse (built) | `internal/envbundle` age Encrypt/Decrypt + `internal/devicekeys` Sign/Verify, kept above the wire |
| Snapshot / full-state exchange | build | **Required before** enabling hub retention GC (`spec/07:235`) |
| NAT traversal / P2P | oss-self-host | **Deferred** Phase 3+; ride the user's tailnet for large bundles, don't embed libp2p/STUN-TURN |
| Wake/push (mobile) | buy | **Deferred**; SSE-while-awake + `Pull(cursor)` on wake |

### Sequencing (Phase 2 → 3)

1. **2.0 (pure refactor):** extract a `Hub` interface (`Push`, `Pull`) from `internal/sync/hub.go` so `FileHub` is one backend; **wire the real resume cursor** in `internal/cli/sync.go:40` (read/persist `sync_cursors.last_hlc_applied` instead of hardcoded `0`). *(This also closes `ARCH2-02`.)*
2. **2.1 (MVP hub, namespace plane):** `cmd/devstraphub` net/http binary (append-only store, `GET ?after`, content-addressed blobs); `HTTPHub` client; `410 → ErrSnapshotRequired`; token auth first.
3. **2.2 (security hardening):** mTLS device certs from the existing identity + hub-side revocation check; zero-knowledge hub test; encrypt sensitive payloads.
4. **2.3 (working-state plane B):** WIP → git bundle/diff → `age_blob:<sha256>` → PUT + `draft.snapshot.created`; pull/decrypt/materialize; assert WIP excluded from agent base resolution. *(This is the transport for Section 5's Layer C.)*
5. **2.4 (liveness + durability):** client `Stream()` SSE + daemon loop; build full-state snapshot exchange, then enable retention GC.
6. **3+ (deferred):** QUIC/HTTP3 for roaming runners; opportunistic P2P over a tailnet; APNs/FCM for a companion app.

### Key actionable steps
1. `internal/sync/hub.go`: define `type Hub interface { Push(ctx, []state.Event) error; Pull(ctx, afterHLC int64) ([]state.Event, error) }`; make `FileHub` satisfy it (pure refactor, existing tests pass).
2. Fix the resume cursor at `internal/cli/sync.go:40` (`hub.Pull(ctx, 0)` → cursor); persist transactionally after `ApplyEvents`; add a regression test that a second sync pulls nothing new.
3. Add `HTTPHub` (POST/GET events, `410 → ErrSnapshotRequired`, `PutBlob`/`GetBlob`).
4. `cmd/devstraphub`: insert-only event table, `?after` pull, SSE `/stream` replaying from `Last-Event-ID` then tailing, content-addressed blobs; reject in-place mutation of payload/HLC/sig/hash.
5. Zero-knowledge hub test (server decrypts neither payloads nor blobs; never persists a private key).
6. Hub mTLS: verify client cert → `dev_<id>` → reject revoked/lost; token fallback gated like `DEVSTRAP_NO_KEYCHAIN`.
7. Full-state snapshot export/import wired to the `410` path; gate any retention GC on it.

### Top risks
- **Hub is semi-trusted** and can reorder/replay/omit/substitute — transport gives *no* integrity; correctness rests on off-wire HLC + content/prev-hash + Ed25519. Any code path trusting hub ordering/auth is a vulnerability.
- **Retention/cursor divergence:** enabling event GC before snapshot exchange exists → silent divergence. Build snapshot first, gate GC on it.
- **mTLS revocation** must be at the handshake / short-lived certs, or a revoked device's long-lived cert defeats revocation.
- **SSE buffered/stripped by proxies** → use `: ping` heartbeats + long-poll fallback; never make liveness a correctness dependency.
- **Working-state leaking into agent isolation** — a WIP/draft bundle must never become an agent base ref; add an explicit exclusion test.
- **Self-host TLS friction** (home hub) — recommend Caddy auto-HTTPS or a tailnet to keep self-hosting realistic.
