# DevStrap — Design & Implementation Audit: Recommendations

| | |
|---|---|
| **Date** | 2026-06-24 |
| **Scope** | Specs `spec/00`–`spec/18` plus the Go codebase (`internal/*`, `cmd/devstrap`) |
| **Method** | 10-dimension multi-agent audit with adversarial verification of every finding, augmented by Exa best-practice research across 6 topic briefs |
| **Build status** | `gofmt`, `go vet`, and `go test -race ./...` currently pass; this document targets correctness/robustness/security gaps below the green-build line |

> 50 recommendations (post-merge) drawn from 56 findings — 55 confirmed by adversarial verification, 1 refuted (DATA-6, excluded). 5 merge operations folded 10 near-duplicate findings into 5 combined entries. Where a finding's verdict was `revise`, the corrected text has been applied.

---

## Table of Contents

1. [Executive Summary](#executive-summary)
2. [Priority Matrix](#priority-matrix)
3. [Architecture & Design](#architecture--design)
4. [Go Implementation Quality](#go-implementation-quality)
5. [Data Model & SQLite](#data-model--sqlite)
6. [Sync Correctness](#sync-correctness)
7. [Security](#security)
8. [Git & Worktrees](#git--worktrees)
9. [Secrets & Environment](#secrets--environment)
10. [Testing & CI](#testing--ci)
11. [CLI / DX / Operations](#cli--dx--operations)
12. [Spec Quality & Consistency](#spec-quality--consistency)
13. [Phased Roadmap](#phased-roadmap)
14. [Best Practices Appendix](#best-practices-appendix)

---

## Executive Summary

DevStrap is a well-structured, spec-driven Go project whose architecture (managed physical namespace, daemon-deferred MVP, local-first event log, fresh-upstream worktrees) is sound and aligns closely with current industry best practice — the research briefs largely *confirm* the design rather than redirect it. The build is green and the codebase already implements the recommended SQLite baseline (DSN pragmas, WAL, single-writer pool, `_txlock=immediate`, embedded goose migrations) and the correct HLC send/receive *shape*. The two biggest themes are **(1) the gap between promised invariants and their implementation** — the headline "never branch agents from a stale base" check, env encryption, the device keypair root-of-trust, tombstone/delete convergence, and the secret-redaction backstop are all specified but unbuilt or unwired — and **(2) a cluster of security and robustness defects on the untrusted-input boundary**, chiefly a **critical git argument/protocol-injection RCE path** (`ext::`/`--upload-pack` remotes flowing from scan→adopt→hydrate without a `--` separator or protocol allowlist) plus inherited-environment, hanging-subprocess, and stale-lock footguns. The top risks, in order, are: the git-remote RCE (SEC-1), the dead secret-redaction layer that can leak credentials in error strings (ENV-2/SEC-3), the unimplemented stale-base re-check that undermines the product's reason to exist (ARCH-3), and several HLC correctness gaps (unpersisted clock, no skew guard, non-transactional apply, missing tombstones) that will cause silent multi-device divergence once sync ships. None of these block the current single-machine CLI, but most are cheap to fix now and expensive to retrofit after the daemon, hub, and agent runner land. Spec quality is high but drifting: CLI syntax, the status JSON contract, the branch-workflow docs, and the work log have small but real inconsistencies that an agent-consumed spec corpus cannot tolerate. The recommended sequencing promotes a thin agent runner ahead of the daemon/hub so the differentiating loop can be proven on one machine before the hardest subsystems are built.

---

## Priority Matrix

Ranked by leverage. **P0** = critical or high-value quick win to do now; **P1** = important, do before the relevant subsystem ships; **P2** = solid improvement; **P3** = polish / future-proofing.

| Priority | ID | Title | Severity | Effort | Theme |
|---|---|---|---|---|---|
| **P0** | SEC-1 | Git arg/protocol injection RCE via untrusted remote URLs | critical | M | Security |
| **P0** | ARCH-3 | Stale-base re-check (the #1 invariant) is unimplemented | high | S | Architecture |
| **P0** | GO-1 | git Runner hangs forever (no timeout, no prompt disable) | high | S | Go quality |
| **P0** | GIT-2 | `DefaultBranch` silently falls back to `main` on any error | high | S | Git |
| **P0** | SYNC-3 | HLC.Receive has no clock-skew guard; one bad remote poisons clock | high | S | Sync |
| **P1** | ENV-2/SEC-3 | Redaction is key-name-only and dead; secret *values* leak | high | M | Secrets |
| **P1** | SEC-2 | git subprocesses inherit full env & global config | high | M | Security |
| **P1** | SEC-5/GO-2 | Repo lock: stale-lock DoS + hydrate clone TOCTOU | medium/high | M | Security |
| **P1** | SYNC-1/GO-4 | HLC counter packed into time bits; unbounded drift | high | M | Sync |
| **P1** | SYNC-2 | HLC never persisted, never wired into event creation | high | M | Sync |
| **P1** | SYNC-4 | ApplyEvents not transactional; conflicts non-idempotent | high | M | Sync |
| **P1** | DATA-2 | events table mutable state contradicts insert-only design | high | M | Data |
| **P1** | ENV-1 | Device has no keypair → age recipient model has no root of trust | high | M | Secrets |
| **P1** | GIT-1 | Agent worktrees ignore stored LFS policy; pointer files | high | M | Git |
| **P1** | TEST-1 | `internal/scan` safety core has 0% test coverage | high | M | Testing |
| **P1** | TEST-2 | Spec-mandated Unicode path normalization not implemented | high | M | Testing |
| **P1** | SPEC-1 | Spec 13 documents CLI syntax the code rejects | high | S | Spec |
| **P2** | ARCH-1 | Promote thin agent runner ahead of daemon/hub | high | M | Architecture |
| **P2** | ARCH-4 | Trust plane (device keys) must precede blob/hub data plane | medium | M | Architecture |
| **P2** | GO-7 | hydrate RemoveAll-before-clone is non-atomic | low (revised) | M | Go quality |
| **P2** | SEC-4 | Symlink escape is advisory-only + TOCTOU | medium | M | Security |
| **P2** | SEC-6 | Spec/15 controls (signatures, env sanitization, policy) absent | medium | L | Security |
| **P2** | SYNC-5 | Tombstone/delete/rename semantics entirely unimplemented | medium | M | Sync |
| **P2** | SYNC-6 | Same-path conflict resolution is order-dependent | medium | M | Sync |
| **P2** | DATA-3 | FK enforcement not asserted at open | medium | S | Data |
| **P2** | DATA-4/ARCH-6 | Hardcoded `ws_local` singleton vs sortable-id contract | medium | M | Data |
| **P2** | DATA-5 | RFC3339 TEXT timestamps + HLC packing fragile ordering | medium | M | Data |
| **P2** | GIT-3 | Dirty-worktree guard bypassable; remove never prunes | medium | M | Git |
| **P2** | GIT-4/GO-8 | Two SQLite writer pools per command | medium | M | Git |
| **P2** | GIT-6 | Network/origin/scp-port git failures unhandled; thin tests | medium | M | Git |
| **P2** | ENV-3 | `.env` capture needs hardened non-interpolating parser | medium | M | Secrets |
| **P2** | ENV-4 | Revocation overstates age re-encryption; needs value rotation | medium | S | Secrets |
| **P2** | ENV-5 | Provider-reference mode should default; lean on `op run` | medium | M | Secrets |
| **P2** | TEST-3 | "Most important test" asserts stdout, not checked-out HEAD | medium | S | Testing |
| **P2** | TEST-4 | CI has no linter (golangci-lint/gosec) | medium | S | Testing |
| **P2** | TEST-5 | JSON CLI contract verified by fragile substrings | medium | M | Testing |
| **P2** | TEST-6 | No end-to-end binary tests; cmd/devstrap 0% covered | medium | L | Testing |
| **P2** | SPEC-2 | status JSON example stale (omits device_id, projects[]) | medium | S | Spec |
| **P2** | SPEC-4 | Branch-workflow docs internally contradictory | medium | M | Spec |
| **P2** | SPEC-5 | No spec drift-detection; mandate unenforceable | medium | M | Spec |
| **P2** | SPEC-6 | Work-log out of order; stale follow-ups | medium | S | Spec |
| **P3** | ARCH-2 | Sync layer over-engineered for single-user MVP | medium | S | Architecture |
| **P3** | ARCH-5 | Platform-adapter interfaces documented but not in code | medium | M | Architecture |
| **P3** | GO-3 | Brittle `strings.Contains("no such table")` detection | medium | S | Go quality |
| **P3** | GO-5 | `open` ties editor lifetime to CLI context | medium | S | Go quality |
| **P3** | GO-6 | Logger injects entire context.Context as attribute | low | S | Go quality |
| **P3** | GIT-5 | Worktree branch suffix risks collisions | low | S | Git |
| **P3** | TEST-7 | HLC overflow/precision edges untested | low | S | Testing |
| **P3** | SPEC-3 | Product name ambiguous (DevStrap vs Workspace Passport) | low | S | Spec |

**Merges performed (5):** GO-4→SYNC-1 · GO-2→SEC-5 · GO-8→GIT-4 · SEC-3→ENV-2 · ARCH-6→DATA-4. (Five merge operations folding 10 IDs into 5 surviving entries; the surviving entry IDs carry the merged IDs in their headings.)

---

## Architecture & Design

### [ARCH-3] The #1 invariant "never PR from a stale base" is unimplemented
`high` · `effort: S` · `internal/cli/worktree.go:64-109`, `spec/04:64-68`, `spec/14:165`

**Problem.** The architecture's primary reason to exist (01:167, 02:208, JTBD 3) is preventing stale-base agent work. `worktree new` correctly fetches `origin/<default_branch>` and records `base_ref`/`base_sha` at creation, but the matching protection the matrix calls for — "before PR, check whether `origin/<default_branch>` moved; warn or auto-rebase" — is the one unchecked box in Milestone 3 and there is no PR/finalize command at all. A worktree created Monday from a fresh base, finalized Friday, will silently PR against a now-stale base: exactly the failure the product promises to eliminate. Creation is fresh, but the lifecycle that makes freshness meaningful is missing.

**Evidence.**
```go
// worktree.go records base at creation but never re-validates it:
BaseRef: baseRef, BaseSHA: baseSHA
// the only later git interaction is `branch --merged` in cleanup (worktree.go:205)
// roadmap 14:165: "[ ] Implement stale-base detection before PR/finalization"
```

**Recommendation.** Implement stale-base detection as a first-class, reusable check wired into a `worktree status`/`agent finalize` path (and later the `gh` PR command). The persisted `base_sha` is already there; add a function that re-fetches `origin/<default>`, compares, reports ahead/behind, and refuses or warns on finalize. Small code, headline payoff, closes the only open M3 box.

**Actionable Steps.**
1. Add a `Runner` method that fetches + rev-parses `origin/<default>`, returning `(newSHA, behindCount)` vs the recorded `base_sha`.
2. Add `devstrap worktree status <id>` printing `fresh | stale(behind N)` plus a `--rebase` hint.
3. Make any future finalize/PR command call this and exit non-zero (`exitConflict`) when stale unless `--allow-stale-base`.
4. Add the risk-traceability test promised at 14:377 plus a "finalize refuses when base moved" test.

**Example.**
```go
// internal/git/git.go
func (r Runner) BaseDrift(ctx context.Context, dir, baseRef, recordedSHA string) (current string, behind int, err error) {
    parts := strings.SplitN(baseRef, "/", 2) // origin/main
    if err = r.Fetch(ctx, dir, parts[0], parts[1]); err != nil { return }
    if current, err = r.RevParse(ctx, dir, baseRef); err != nil { return }
    if current == recordedSHA { return current, 0, nil }
    out, err := r.Run(ctx, dir, "rev-list", "--count", recordedSHA+".."+current)
    fmt.Sscanf(out, "%d", &behind)
    return
}
// finalize: if behind > 0 && !allowStale ->
//   appError{code: exitConflict, err: fmt.Errorf("base %s moved %d commits; rebase or pass --allow-stale-base", baseRef, behind)}
```

**References.** [DevPod: what-is-devpod](https://devpod.sh/docs/what-is-devpod)

---

### [ARCH-1] Promote a thin agent runner ahead of the daemon, Linux, and hub
`high` · `effort: M` · `spec/14:9-20`, `spec/00`, `spec/01:155-167`

**Problem.** *(verdict: revise — corrected text applied.)* The roadmap sequences M0 skeleton → M1 scan/adopt → M1.5 sync spike → M2 hydrate/open → M3 fresh worktree → M4 env → M5 Mac daemon/watcher → M6 Linux → M7 hub → **M8 agent runner**. Contrary to a front-loading concern, the daemon (M5) and hub (M7) already come *after* the core single-machine loop. The real weakness is narrower: the thin agent runner (M8) — which completes the north-star loop "never branch agents from a stale local default branch" and depends only on the worktree manager, not on any daemon/watcher/hub — is placed last, behind the two hardest, lowest-confidence subsystems. The specs themselves name the watcher as "a hint, not the source of truth" (04:198) and already mandate periodic-reconciliation correctness (14:381), so the agent loop can ship and be validated with real usage well before the daemon exists.

**Evidence.**
```text
Build order (14:9-20): ... M3 fresh worktree -> M4 env -> M5 Mac daemon/watcher
                       -> M6 Linux -> M7 hub -> M8 agent runner
04:198: "watcher is a hint, not the source of truth"
```

**Recommendation.** Promote a thin agent runner to immediately after the worktree manager (e.g. M3.5) so the differentiating loop (worktree → agent run → diff/logs/PR) can be proven on a single machine with zero daemon. Keep M5 daemon and M7 hub where they are (already post-core-loop) but gate them behind a written "do we still need it?" review using the already-listed sleep/wake and indexer-hydration-storm tests (14:369, 14:381) as entry criteria, and demote the hub behind a real two-machine usage signal. Add an explicit cross-command no-daemon guarantee to `spec/03`.

**Actionable Steps.**
1. Re-sequence: M3 fresh worktree → **M3.5 thin agent runner** (generic command in worktree, capture diff/logs) → M4 env → daemon/Linux/hub.
2. Add a "no-daemon mode" guarantee to `spec/03` so every command works via periodic-scan reconciliation; make the daemon a pure perf/UX optimization, not a correctness dependency.
3. Gate M5 daemon behind a written review with the indexer-hydration-storm and sleep/wake tests as entry criteria.
4. Gate M7 hub behind a real two-machine path-drift usage signal.

**Example.**
```text
devstrap init ~/Code
devstrap scan ~/Code --adopt                                      # M1, manual/explicit
devstrap open work/org/repo --cursor                              # M2, hydrate-on-open
devstrap worktree new work/org/repo --fresh-upstream --name fix   # M3, the differentiator
devstrap agent run work/org/repo --engine generic --task "tests" --command "uv run pytest"  # M3.5, proves the loop
devstrap status                                                   # periodic-scan reconcile replaces watcher for v0
# M5 daemon/watcher, M6 Linux, M7 hub only AFTER this loop has real users
```

**References.** [DevPod](https://devpod.sh/docs/what-is-devpod) · [devcontainer-to-mise migration](https://www.milkstraw.ai/blog/devcontainer-to-mise-migration)

---

### [ARCH-4] Sequence the device-trust plane before any blob/hub data plane
`medium` · `effort: M` · `spec/02:204-214`, `spec/04:264-288`, `spec/14:181-208`

**Problem.** Several architectural promises depend on subsystems that do not exist: "env secrets are never logged" and "safe env distribution" depend on the env parser + encrypted store + per-device key model (M4, every box unchecked); "a new machine should not automatically receive secrets" (04:264-273) depends on per-device keypairs and explicit env-decryption approval, also unbuilt. Today the only realized piece is log redaction (and even that is unwired — see ENV-2). The risk is architectural: if the hub/blob machinery (ARCH-2) lands before the device-key model (ENV-1), the data plane outpaces the trust plane.

**Evidence.**
```text
00_START_HERE.md: "Not implemented yet: env capture/hydrate and encryption;
  ... production sync hub, remote device approval, encrypted blob exchange"
Hub design (03:25, 01:23) already references "encrypted env/draft blobs" as if available.
```

**Recommendation.** Sequence the trust plane before (or with) any blob/hub data plane, and make the dependency explicit so encrypted-blob sync cannot ship before device keys + approval exist. Until then keep env strictly local (no sync). Use a boring, well-reviewed primitive (age/X25519) rather than rolling key management.

**Actionable Steps.**
1. Add a hard dependency edge in `spec/14`: "encrypted-blob exchange (M7) requires device-key model + per-device approval (M4)."
2. Ship env local-only first: `env capture` / `env hydrate --write .env.local`, no network.
3. Specify the device-trust handshake (register → approve → grant env-decrypt → revoke) in 09/15 with concrete key types (age recipients per device) before building blob upload.
4. Add the risk test (14:379 `grep -r <secret> ~/.devstrap` finds nothing) as a gate on the env milestone.

**Example.**
```text
# 14_MVP_ROADMAP_AND_BACKLOG.md — dependency note
Blocking edge: M7 "encrypted blob upload/download" REQUIRES
  M4 "device key model" + "per-device env-decryption approval".
Until device trust exists, env is LOCAL-ONLY:
  devstrap env capture <path> .env                  # encrypted-at-rest local store
  devstrap env hydrate <path> --write .env.local    # same machine only, no sync
```

**References.** [Conflict-resolution planning guidelines](https://agenticdevelopercookbook.com/guidelines/planning/data/conflict-resolution)

---

### [ARCH-2] Right-size the sync spike; defer speculative crypto/delivery machinery
`medium` · `effort: S` · `internal/sync/*`, `internal/state/migrations/00002_event_ordering.sql`, `spec/03:213-257`

**Problem.** *(verdict: revise — corrected text applied.)* Milestone 1.5 builds an HLC, append-only event log, per-peer cursors, `event_delivery`, content hashes, and `device_sig`/`prev_event_hash` columns plus same-path conflict detection — a substantial distributed-systems substrate — before there is a second device, a real hub, or a user-facing `devstrap sync`. For the primary persona (one developer, multiple owned machines) the namespace is single-writer-per-path most of the time, and git working trees are explicitly NOT synced. The `device_sig`/`prev_event_hash` columns are currently unwritten placeholders whose chain format risks being frozen prematurely.

**Evidence.**
```sql
-- 00002 adds device_sig TEXT, prev_event_hash TEXT, sync_cursors, event_delivery
-- ahead of any hub. 14:118: "A user-facing devstrap sync command ... remain future work."
```

**Recommendation.** Keep the event log (right local-first spine) but defer the speculative cryptographic/delivery machinery and multi-peer hub protocol until a concrete hub exists. Right-size to: append-only events keyed by content hash, HLC ordering with device-id tiebreak (done), idempotent apply. Document the single-writer-per-path assumption and the "detect, never auto-merge namespace conflicts" rule for the path/remote conflict class — while the safe-automatic class already defined in `spec/03` (duplicate skeleton creation, heartbeat latest-wins, recreate-missing-skeleton) may be resolved without prompting.

**Actionable Steps.**
1. Mark `internal/sync` as an explicitly-experimental spike in its package doc comment; gate further investment on a written hub protocol spec.
2. Defer `device_sig`/`prev_event_hash` to the hub milestone so the chain format isn't frozen prematurely.
3. Document the single-writer-per-path + detect-only (path/remote class) rule in `spec/07`.
4. Add a decision note: a hidden manifest git repo (01:129-133 / 04:357) may be a faster hub than a bespoke service — re-evaluate before building `devstraphub`.

**Example.**
```go
// Package sync is a Phase-0 SPIKE: it proves HLC ordering, idempotent apply,
// and same-path/different-remote conflict DETECTION across two local roots.
// It is NOT the production hub protocol. Namespace state is treated as
// single-writer per path; the path/remote conflict class is surfaced for the
// user and never auto-merged (safe-automatic cases per spec/03 may still be
// resolved without prompting). Do not freeze the device_sig/prev_event_hash
// chain format until spec 07/15 define the on-wire hub protocol; those columns
// are currently unwritten placeholders.
package sync
```

**References.** [Data replication & conflict resolution](https://codelit.io/blog/data-replication-conflict-resolution) · [Conflict-resolution guidelines](https://agenticdevelopercookbook.com/guidelines/planning/data/conflict-resolution)

---

### [ARCH-5] Introduce platform-adapter interfaces in code before the daemon forces the split
`medium` · `effort: M` · `spec/03:259-310`, `internal/git/git.go`, `internal/cli/open.go`, `internal/config/paths.go`

**Problem.** `spec/03` defines the right adapter seams (Watcher, ServiceManager, Keychain, EditorAdapter, SecretProvider, AgentRunner) and an 80/15/5 platform split, and AGENTS.md makes "keep Mac-specific behavior behind adapters" a rule. But none of these interfaces exist in code; the code is platform-neutral mostly by accident (it shells out to git and an editor binary). The spec says adapters "should be introduced before implementing platform-specific watcher or service-manager code" (03:310), yet the roadmap introduces the Mac daemon (M5) before Linux (M6) with no "define adapter interfaces" task between — precisely how Mac assumptions calcify.

**Evidence.**
```go
// 03:263-271 interfaces are empty placeholders, e.g. type Watcher interface {}
// 03:310: "adapter interfaces ... should be introduced before implementing
//          platform-specific watcher or service-manager code."
```

**Recommendation.** Introduce the adapter interfaces as a dedicated, testable `internal/platform` package BEFORE the M5 watcher work, with the Mac implementation as the first concrete adapter and a no-op/poll fallback as the second, so Linux is a peer from day one. Define thin method sets now for Watcher, ServiceManager, EditorAdapter, Keychain; this also gives the daemon a clean injection point.

**Actionable Steps.**
1. Create `internal/platform` with non-empty interfaces and `platform.Detect()` returning the right set per `GOOS`.
2. Refactor the editor-open logic in `open.go` behind `EditorAdapter` immediately (cheap now, expensive later).
3. Add a "define platform adapter interfaces + poll fallback watcher" task as a prerequisite gate on M5.
4. Add a CI lint convention forbidding `runtime.GOOS` branches outside `internal/platform`.

**Example.**
```go
// internal/platform/platform.go
type Watcher interface { Watch(ctx context.Context, root string, events chan<- FSEvent) error }
type ServiceManager interface { Install(ctx context.Context) error; Status(ctx context.Context) (string, error) }
type EditorAdapter interface { Open(ctx context.Context, dir, editor string) error }

func Detect() Set {
    switch runtime.GOOS {
    case "darwin": return Set{Watcher: fseventsWatcher{}, Service: launchdManager{}}
    case "linux":  return Set{Watcher: inotifyWatcher{}, Service: systemdManager{}}
    default:       return Set{Watcher: pollWatcher{}}
    }
}
```

**References.** [DevPod](https://devpod.sh/docs/what-is-devpod) · [uclab devcontainer-build](https://uclab.dev/posts/devcontainer-build/)

---

## Go Implementation Quality

### [GO-1] git Runner never disables prompts and sets no network timeout
`high` · `effort: S` · `internal/git/git.go:24-67`, `internal/cli/worktree.go:68`, `internal/cli/hydrate.go:77`

**Problem.** `Runner.Run` builds the command with `exec.CommandContext` but never sets `cmd.Env`, so git inherits the ambient environment. With no `GIT_TERMINAL_PROMPT=0` and no `GIT_SSH_COMMAND='ssh -oBatchMode=yes'`, a private HTTPS repo blocks reading credentials from the terminal, and an SSH remote with a passphrase-protected key blocks on ssh's own prompt. None of the network operations impose a timeout: a hung `devstrap hydrate` / `worktree new` over a flaky network or a typo'd remote sits forever — worse inside the planned non-interactive agent runner/daemon. *(See SEC-2 for the security-isolation dimension of the same `cmd.Env` gap; that finding strips dangerous env names, this one disables prompts and bounds time.)*

**Evidence.**
```go
cmd := exec.CommandContext(ctx, bin, args...) // no cmd.Env, no GIT_TERMINAL_PROMPT
// grep for context.WithTimeout across internal/ returned nothing
```

**Recommendation.** Always set a non-interactive environment and wrap every network operation in a bounded `context.WithTimeout`, configurable per call.

**Actionable Steps.**
1. Add a `NetworkTimeout` field (default ~120s) to `Runner`; derive a child context for `Clone`/`Fetch`.
2. In `Run`, set `cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=accept-new")` (reconcile with SEC-2's allowlist env).
3. Detect `context.DeadlineExceeded` in `Run` and surface it as `appError{code: exitNetwork}`.
4. Add a test using `https://127.0.0.1:1/x.git` asserting the call returns within the timeout.

**Example.**
```go
func (r Runner) Fetch(ctx context.Context, dir, remote, branch string) error {
    timeout := r.NetworkTimeout
    if timeout == 0 { timeout = 120 * time.Second }
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    // ... build args, call r.Run(ctx, dir, args...) ...
}
```

**References.** [Disabling credential prompting](https://stackoverflow.com/questions/46440000/how-do-i-disable-credential-prompting-for-git-clone-in-go) · [go issue 44904](https://github.com/golang/go/issues/44904) · [os/exec CommandContext](https://pkg.go.dev/os/exec#CommandContext)

---

### [GO-7] hydrate removes the target in place before clone (non-atomic, destroys-then-rewrites skeleton)
`low` *(revised down from medium)* · `effort: M` · `internal/cli/hydrate.go:64-104`

**Problem.** *(verdict: revise — corrected text applied.)* `hydrateProject` calls `ensureHydratableTarget(localPath)` then unconditionally `os.RemoveAll(localPath)` before cloning. Two robustness gaps follow: (1) a TOCTOU window — the directory can change between `ensureHydratableTarget`'s `os.ReadDir` and the subsequent `RemoveAll`; (2) non-atomic failure handling — if `Clone` fails, the original target (already removed) is gone and only a fresh skeleton is rewritten. **This is NOT user-data loss**: `ensureHydratableTarget` only permits an empty dir or a recognized skeleton, and `isSkeleton` is an exhaustive allowlist (returns false if any entry other than `.devstrap` and `README.devstrap.md` is present), so a directory holding real uncommitted work cannot pass; the `IsRepo` branch also short-circuits any real checkout. Skeletons contain no user content. The defect is atomicity/robustness, not data destruction.

**Evidence.**
```go
if err := ensureHydratableTarget(localPath); err != nil { return "", err }
if err := os.RemoveAll(localPath); err != nil { ... }          // destroys before clone
if err := r.Clone(ctx, project.RemoteURL, localPath, partial); err != nil { _ = writeSkeleton(...) }
```

**Recommendation.** Clone into a sibling temp directory on the same filesystem and atomically `os.Rename` into place only on clone success; never `RemoveAll` the target before a successful clone. On failure, `RemoveAll` only the temp dir and leave the original untouched. This also closes the TOCTOU window because nothing is destroyed until the rename.

**Actionable Steps.**
1. Clone into `localPath + ".devstrap-tmp-<rand>"` (same parent for atomic rename).
2. On success, re-verify `localPath` is still empty/skeleton, remove only known skeleton marker files, then `os.Rename(tmp, localPath)`.
3. On any failure, `RemoveAll` the temp dir; rewrite the skeleton only if the target was already a skeleton.
4. Add a test placing a non-skeleton file in the dir and asserting hydrate refuses without deleting it.

**Example.**
```go
tmp := localPath + ".devstrap-tmp-" + short
if err := r.Clone(ctx, project.RemoteURL, tmp, partial); err != nil {
    _ = os.RemoveAll(tmp)                 // original target untouched
    return "", err
}
if err := ensureHydratableTarget(localPath); err != nil { _ = os.RemoveAll(tmp); return "", err } // re-check, close TOCTOU
_ = removeSkeletonMarkers(localPath)      // delete only .devstrap/* and README.devstrap.md
if err := os.Rename(tmp, localPath); err != nil { return "", fmt.Errorf("promote clone: %w", err) }
```

**References.** [os.Rename](https://pkg.go.dev/os#Rename) · [os.RemoveAll](https://pkg.go.dev/os#RemoveAll)

---

### [GO-3] Replace brittle `strings.Contains(err.Error(), "no such table")` with explicit init detection
`medium` · `effort: S` · `internal/state/store.go:255-303,472`

**Problem.** Multiple code paths decide whether the workspace is uninitialized by substring-matching the driver's error text. This couples control flow to an unstable, driver-specific message and can misfire: any error containing "no such table" is silently reclassified as `ErrNotInitialized`, masking the real failure.

**Evidence.**
```go
if strings.Contains(err.Error(), "no such table") { return Device{}, ErrNotInitialized }
```

**Recommendation.** Detect initialization explicitly (does a known table exist?) rather than inferring from arbitrary query failures. If you must inspect the error, match the typed driver error via `errors.As`, not the message string.

**Actionable Steps.**
1. Add `Store.IsInitialized(ctx)` querying `sqlite_master` for the `workspaces` table; callers return `ErrNotInitialized` based on that, leaving real query errors un-rewritten.
2. If keeping inline detection, use `errors.As` against `*sqlite.Error` and its code.
3. Remove the duplicated substring logic from `EnsureDevice`/`CurrentDevice`/`Summary`/`ListProjects`.

**Example.**
```go
func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
    var one int
    err := s.db.QueryRowContext(ctx,
        `SELECT 1 FROM sqlite_master WHERE type='table' AND name='workspaces' LIMIT 1`).Scan(&one)
    if errors.Is(err, sql.ErrNoRows) { return false, nil }
    return err == nil, err
}
```

**References.** [Go 1.13 errors](https://go.dev/blog/go1.13-errors) · [modernc sqlite.Error](https://pkg.go.dev/modernc.org/sqlite#Error)

---

### [GO-5] `open` ties the GUI editor's lifetime to the command context and blocks on `.Run()`
`medium` · `effort: S` · `internal/cli/open.go:33-35`

**Problem.** The command does `exec.CommandContext(cmd.Context(), editor, localPath).Run()`. `CommandContext` kills the child when the context is done, and `main.go` binds that context to SIGINT/SIGTERM. The intent is fire-and-forget launch; the code instead couples the editor to the CLI's lifecycle: if the launcher does not detach, `.Run()` blocks until the editor exits, and a Ctrl-C signal-kills the editor.

**Evidence.**
```go
if err := exec.CommandContext(cmd.Context(), editor, localPath).Run(); err != nil { return fmt.Errorf("open editor: %w", err) }
```

**Recommendation.** Launch detached: use `exec.Command` (not `CommandContext`) and `Start()` (not `Run()`) so the editor outlives the CLI and a Ctrl-C doesn't kill it.

**Actionable Steps.**
1. Replace `CommandContext` with `exec.Command(editor, localPath)`; call `.Start()`.
2. Rely on the earlier `exec.LookPath` plus `Start`'s error to detect launch failure.
3. Optionally set `SysProcAttr{Setpgid: true}` so the editor is in its own process group.

**Example.**
```go
proc := exec.Command(editor, localPath) // not CommandContext
if err := proc.Start(); err != nil { return fmt.Errorf("open editor: %w", err) }
_ = proc.Process.Release()              // let the editor run independently
```

**References.** [Cmd.Start](https://pkg.go.dev/os/exec#Cmd.Start) · [CommandContext](https://pkg.go.dev/os/exec#CommandContext)

---

### [GO-6] `logging.Logger` injects the entire `context.Context` as a log attribute
`low` · `effort: S` · `internal/logging/logging.go:56-58`

**Problem.** `Logger(ctx)` returns `slog.Default().With("component","devstrap").With("ctx", ctx)`. Attaching the whole context as a `ctx` attribute is an anti-pattern: slog formats it via reflection, printing internal cancel/value state, possibly large, and may incidentally serialize values stored in the context (including, in future, anything sensitive) bypassing the key-name-only secret redaction.

**Evidence.**
```go
func Logger(ctx context.Context) *slog.Logger { return slog.Default().With("component", "devstrap").With("ctx", ctx) }
```

**Recommendation.** Drop the `ctx` attribute. Return a logger scoped with the component attribute only; have callers use the `*Context` slog methods (`logger.InfoContext(ctx, ...)`). Optionally extract a stable scalar trace id.

**Actionable Steps.**
1. Change `Logger` to return `slog.Default().With("component", "devstrap")`.
2. Update call sites to use `InfoContext`/`DebugContext`, passing `ctx` first.
3. Add a test asserting logged output does not contain `ctx=` / context internals.

**Example.**
```go
func Logger(ctx context.Context) *slog.Logger {
    l := slog.Default().With("component", "devstrap")
    if id, ok := ctx.Value(traceIDKey{}).(string); ok && id != "" { l = l.With("trace_id", id) }
    return l
}
// usage: logging.Logger(ctx).InfoContext(ctx, "hydrated", "path", p)
```

**References.** [slog Logger.InfoContext](https://pkg.go.dev/log/slog#Logger.InfoContext) · [slog blog](https://go.dev/blog/slog)

---

## Data Model & SQLite

### [DATA-2] events table keeps mutable `sync_state`/`applied_at` contradicting the insert-only design; `INSERT OR IGNORE` silently drops divergent duplicates
`high` · `effort: M` · `internal/state/migrations/00001_initial.sql:149-161`, `spec/12:235`, `internal/state/store.go:554-557`

**Problem.** The spec states events are insert-only and that mutable apply/delivery state belongs in `event_delivery` (added by 00002). Yet the events table still carries mutable `sync_state`/`applied_at`, and `PendingEvents` reads `events.sync_state='pending'` rather than `event_delivery` — so the new delivery table is dead and the design is contradicted in code. Separately, `InsertEvent` uses `INSERT OR IGNORE` keyed on a fresh-per-call ULID `id`, so a re-delivered peer event carrying the SAME id is silently ignored even if its body differs — masking corruption/forgery instead of detecting it, despite `content_hash`/`prev_event_hash` columns being present for integrity.

**Evidence.**
```sql
-- store.go: INSERT OR IGNORE INTO events (id, ..., content_hash, ...) VALUES (...)
-- PendingEvents: WHERE sync_state = 'pending'
-- spec/12:235: "Rows in events are insert-only. Mutable delivery/apply state belongs in event_delivery."
```

**Recommendation.** Pick one source of truth for delivery state (drop `sync_state`/`applied_at` and read from `event_delivery`, *or* defer `event_delivery` until sync ships). Make idempotency meaningful: enforce per-device monotonic `seq` (the partial unique index already exists) and, on duplicate id, verify `content_hash` matches and raise a conflict if it differs.

**Actionable Steps.**
1. Decide the canonical delivery-state location; update spec and code together.
2. If keeping `event_delivery`: change `PendingEvents` to join it; add a migration dropping `events.sync_state`/`applied_at`.
3. In `InsertEvent`, on conflict `SELECT` the existing `content_hash` and compare; if different, insert an `event.hash_mismatch` conflict instead of ignoring.
4. Populate `seq` (currently 0/NULL) so the device-monotonicity index is exercised; test that out-of-order/duplicate seq is rejected.

**Example.**
```go
res, _ := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events (...) VALUES (...)`)
if n, _ := res.RowsAffected(); n == 0 {
    var existing string
    _ = tx.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE id = ?`, event.ID).Scan(&existing)
    if existing != event.ContentHash {
        return s.InsertConflict(ctx, "", "event.hash_mismatch",
            fmt.Sprintf(`{"id":%q,"stored":%q,"incoming":%q}`, event.ID, existing, event.ContentHash))
    }
}
```

**References.** [goose SQL migrations](https://pressly.github.io/goose/blog/2022/overview-sql-file/) · [SQLite Go best practices](https://tessl.io/registry/tessl-labs/sqlite-go-best-practices)

---

### [DATA-4 / ARCH-6] Hardcoded singleton `ws_local` collides with the sortable-id contract and breaks multi-workspace/sync replay
`medium` · `effort: M` · `internal/state/store.go:209-215` + ~12 literals, `internal/sync/events.go:42`, `spec/12:325-339`
*(Merged: ARCH-6 folded in — the design-coherence framing of the same hardcoded constant.)*

**Problem.** The schema models workspaces as a first-class, sortable-id (`ws_<ulid>`/uuidv7) entity and the events/sync model is workspace-scoped, but `ws_local` is hardcoded in ~12 places across the store and in the sync event constructor. The workspace concept is real in the schema yet fictional in practice: the sync layer emits events stamped with a constant workspace id and every query filters on the constant. Consequences: (1) cross-device event matching on `workspace_id` is ambiguous; (2) a second/shared workspace (secondary persona) cannot be represented; (3) `NewProjectEvent` hardcodes `ws_local` so workspace identity is not actually synchronized — a latent correctness bug for the "same namespace appears on Machine B" loop. Threading this through ~12 call sites grows with every new query added now.

**Evidence.**
```go
// EnsureWorkspace: INSERT INTO workspaces (id, ...) VALUES ('ws_local', ?, ?, ?, ?)
// NewProjectEvent (sync/events.go:42): WorkspaceID: "ws_local"
// store.go:545-546: if event.WorkspaceID == "" { event.WorkspaceID = "ws_local" }
```

**Recommendation.** Treat the local workspace id as data: generate `ws_<uuidv7>` once at init (like the device id), persist it, cache it on the `Store` at `Open`, and thread it as a parameter at the store boundary instead of inlining the literal. Keep a single-row invariant (CHECK or singleton table) so the MVP runs exactly one workspace, but stop hardcoding the value so sync carries a real shared identity (all devices in one logical workspace must share the SAME `ws` id, provisioned during pairing).

**Actionable Steps.**
1. At init, generate the workspace id with `id.New("ws")`; persist it; expose `Store.WorkspaceID(ctx)` / a cached `s.workspaceID`.
2. Replace every `ws_local` literal in `store.go` and `sync/events.go` with the loaded id (define one exported constant if a sentinel is still needed).
3. Have the sync event constructor take the workspace id from the store/device context.
4. Add a uniqueness guard and a note in `spec/12` stating the MVP runs one workspace and where the assumption is centralized.

**Example.**
```go
const LocalWorkspaceID = "ws_local" // interim sentinel; centralized so it is the only literal
func (s *Store) ProjectByPath(ctx context.Context, path string) (Project, error) {
    row := s.db.QueryRowContext(ctx, q, s.workspaceID, key) // s.workspaceID set at Open()
}
```

**References.** [goose SQL migrations](https://pressly.github.io/goose/blog/2022/overview-sql-file/) · [SDK SYNC doc](https://github.com/Rajeev02/rajeev-sdk/blob/main/docs/usage/SYNC.md)

---

### [DATA-1] `ListProjects` applies `status='active'` as an unindexed residual filter
`low–medium` *(revised down from high)* · `effort: S` · `internal/state/store.go:459-488`, `internal/state/migrations/00001_initial.sql:23-40`

**Problem.** *(verdict: revise — corrected text applied.)* `ProjectByPath` and `upsertNamespaceTx` equality lookups are already served by the `UNIQUE(workspace_id, path_key)` autoindex (verified by EQP: `SEARCH ... (workspace_id=? AND path_key=?)`) — these need no change. The only real issue is `ListProjects`: `WHERE n.workspace_id='ws_local' AND n.status='active' ORDER BY n.path_key`. EQP shows SQLite range-scans the workspace and gets `path_key` ordering for free, but `status='active'` is a residual filter applied to every active+inactive row in that workspace. For a local single-user namespace the row count is small, so impact is minor; it only matters at high project counts. *(The earlier "FK to workspaces not guaranteed" and "path_key lookups unindexed" claims were false and are dropped.)*

**Evidence.**
```text
EQP: SEARCH n USING INDEX sqlite_autoindex_namespace_entries_2 (workspace_id=?)
-- status='active' is a residual filter on every active+inactive row in the workspace
```

**Recommendation.** Add a partial index keyed on the filter plus the sort column so SQLite satisfies both the filter and `ORDER BY` from the index; verify with `EXPLAIN QUERY PLAN`.

**Actionable Steps.**
1. Run `EXPLAIN QUERY PLAN` for `ListProjects` against a seeded DB to confirm the residual filter.
2. Add migration `00003_indexes.sql` with the partial index below.
3. Confirm the `devices(trust_state='local')` LEFT JOIN is cheap (single-row table) or bind the device id as a parameter.
4. Add an EQP-based test asserting the plan uses the index.

**Example.**
```sql
-- +goose Up
CREATE INDEX idx_namespace_active
  ON namespace_entries(workspace_id, path_key)
  WHERE status = 'active';
-- +goose Down
DROP INDEX idx_namespace_active;
```

**References.** [SQLite Go best practices](https://tessl.io/registry/tessl-labs/sqlite-go-best-practices) · [SQLite pragma](https://sqlite.org/pragma.html)

---

### [DATA-3] FK enforcement works but is not asserted at open; cascades silently no-op if a future path omits the DSN pragma
`medium` · `effort: S` · `internal/state/store.go:112-145`, migrations relying on `ON DELETE CASCADE`/`SET NULL`

**Problem.** `foreign_keys` is a per-connection pragma, OFF by default and a silent no-op inside a transaction. The schema leans heavily on cascade/set-null integrity. The current DSN *does* enable it (verified: `PRAGMA foreign_keys` = 1, FK-violating insert fails with error 787). The risk is regression: any future path that opens the DB without the exact DSN (backup tool, reader pool, test helper, goose CLI) or a driver upgrade that changes DSN parsing will silently disable cascade integrity with zero error, and orphan rows accumulate.

**Evidence.**
```text
sqliteDSN builds _pragma=foreign_keys(1) only; Open() never reads it back.
Runtime: foreign_keys = 1; FK violation insert err = 787.
```

**Recommendation.** Assert FK enforcement after `Open` (consider `RegisterConnectionHook` so the pragma cannot be forgotten by any future pool), and add a startup `PRAGMA foreign_key_check` to surface pre-existing orphans.

**Actionable Steps.**
1. After `db.Ping()`, run `PRAGMA foreign_keys` and error if it isn't 1.
2. Add `PRAGMA foreign_key_check` to `QuickCheck`/`db status`.
3. Optionally adopt `sqlite.RegisterConnectionHook` to set `foreign_keys`/`journal_mode`/`busy_timeout` on every connection (keeps a future reader pool correct).
4. Add a test opening with a wrong DSN asserting `Open()` fails fast.

**Example.**
```go
var fk int
if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
    _ = db.Close()
    return nil, fmt.Errorf("foreign keys not enforced (got %d): %w", fk, err)
}
```

**References.** [SQLite pragma](https://sqlite.org/pragma.html) · [modernc sqlite](https://context7.com/modernc-org/sqlite/llms.txt) · [sqliteutil hook example](https://github.com/djs55/cagent/blob/main/pkg/sqliteutil/sqlite.go)

---

### [DATA-5] RFC3339 TEXT timestamps + HLC `UnixMilli<<16` packing make ordering fragile
`medium` · `effort: M` · `internal/state/store.go` (many `RFC3339` writes), `internal/sync/hlc.go:38`, `store.go:569`

**Problem.** All temporal columns are RFC3339 TEXT at second precision, so same-second inserts share `created_at` and `ORDER BY created_at DESC` (e.g. `ListWorktrees`) is non-deterministic. The HLC packs `UnixMilli()<<16`; the 16-bit logical room means after 65535 events in one millisecond `Send()` overflows into the next millisecond's space with no guard. The events `ORDER BY hlc, device_id` lacks a final unique tiebreaker, and `prev_event_hash` chains are never validated, so replay order can diverge from causal order undetected. *(See SYNC-1 for the HLC algorithm fix and TEST-7 for the overflow tests.)*

**Evidence.**
```go
// hlc.go:38: return now.UnixMilli() << 16
// PendingEvents: ORDER BY hlc ASC, device_id ASC
// ListWorktrees: ORDER BY created_at DESC  (second-precision RFC3339)
```

**Recommendation.** Store machine timestamps as `RFC3339Nano` (cheap, no schema change) or INTEGER epoch-millis for ordering columns; make the HLC explicit (separate physical-ms and logical-counter, or document/guard the 16-bit bound); add the event id (ULID, monotonic) as the final `ORDER BY` tiebreaker so replay is fully deterministic.

**Actionable Steps.**
1. Switch `created_at`/`updated_at` ordering-column writes to `RFC3339Nano` (or INTEGER millis).
2. In `HLC.Send`, bound the logical counter (<65536) and roll the physical forward on exhaustion (see SYNC-1).
3. Add `id ASC` as the final tiebreaker in `PendingEvents` and any replay path.
4. Validate `prev_event_hash` chains during `ApplyEvents` and raise a conflict on a break.

**Example.**
```go
// deterministic event ordering: add event id as final tiebreaker
// ... ORDER BY hlc ASC, device_id ASC, id ASC;

// HLC with explicit physical/logical split (overflow-safe)
const logicalBits = 16
const logicalMask = (1 << logicalBits) - 1
func (h *HLC) Send() int64 {
    phys := h.physicalMillis() << logicalBits        // physicalMillis returns UnixMilli, NOT pre-shifted
    if phys > (h.Last &^ logicalMask) { h.Last = phys; return h.Last } // new ms, logical = 0
    if (h.Last & logicalMask) == logicalMask {                          // logical exhausted this ms
        h.Last = (h.Last &^ logicalMask) + (1 << logicalBits); return h.Last
    }
    h.Last++; return h.Last                                             // same ms, bump logical
}
```

**References.** [SQLite pragma](https://sqlite.org/pragma.html)

---

## Sync Correctness

### [SYNC-1 / GO-4] HLC packs the logical counter into the physical-time bits with no bound, and is shared mutable state with no mutex
`high` · `effort: M` · `internal/sync/hlc.go:5-39`
*(Merged: GO-4 folded in — the concurrency/mutex dimension of the same HLC type.)*

**Problem.** *(verdict: revise — corrected text applied.)* `physical()` returns `now.UnixMilli() << 16`, reserving the low 16 bits for a logical counter. `Send`/`Receive` never treat the counter as a separate bounded field: same-millisecond `Send` does `h.Last++` and `Receive` does `max+1` on the full packed value. (A bare `++` actually carries correctly into the ms bits — `(ms<<16|0xFFFF)+1 == (ms+1)<<16` — so total ordering is preserved; the "corrupts time bits" framing is inaccurate.) The two **real** defects: (1) there is no explicit bounded counter and no overflow guard, leaving the design implicit and fragile; (2) the "stays close to physical time" property is genuinely broken — `Receive` unconditionally returns `max+1` and never resets the counter when physical time advances, and `Send` increments whenever `now <= Last`, so under a frozen/stuck clock or a sustained burst `l - pt` grows without bound, defeating HLC's bounded-drift guarantee. **Additionally** (GO-4): `HLC.Last` is read-modify-written with no `sync.Mutex`; the planned daemon + agent runner will call `Send`/`Receive` concurrently, an unsynchronized race that `go test -race` (mandated by AGENTS.md) will flag.

**Evidence.**
```go
func (h *HLC) Send() int64 {
    now := h.physical()
    if now > h.Last { h.Last = now } else { h.Last++ }
    return h.Last
}
func (h *HLC) physical() int64 { return now.UnixMilli() << 16 }
```

**Recommendation.** Model the clock explicitly as `(physicalMs int64, counter uint16)` plus a `sync.Mutex`, combine via `Encoded() = ms<<16 | counter`, reset the counter to 0 whenever physical time advances, increment otherwise, and on `counter==maxCounter` borrow a logical ms (`ms++; counter=0`). `Receive` picks `l = max(localMs, nowMs, remoteMs)` and sets the counter per the standard HLC rule (0 if `l` beats both, else `max(local,remote counter)+1`, same overflow borrow). Lock around every read-modify-write.

**Actionable Steps.**
1. Add a `sync.Mutex` and lock `Send`/`Receive`; add a `-race` test sending from N goroutines asserting strictly increasing, unique values.
2. Change `physical()` to return raw `UnixMilli()` (no shift); store `LastMs`/`Counter` separately; bit-pack only in `Encoded()`.
3. On counter exhaustion, advance the physical ms rather than incrementing into it.
4. Add tests for a >65535 same-ms burst and a backwards clock (`LastMs` must not regress).

**Example.**
```go
type HLC struct { mu sync.Mutex; LastMs int64; Counter uint16; Now func() time.Time }
const counterBits = 16
const maxCounter = 1<<counterBits - 1
func (h *HLC) advance(ms int64) {
    if ms > h.LastMs { h.LastMs, h.Counter = ms, 0; return }
    if h.Counter == maxCounter { h.LastMs++; h.Counter = 0; return } // borrow a logical ms
    h.Counter++
}
func (h *HLC) Send() int64 { h.mu.Lock(); defer h.mu.Unlock(); h.advance(h.physicalMs()); return h.Encoded() }
func (h *HLC) Encoded() int64 { return h.LastMs<<counterBits | int64(h.Counter) }
```

**References.** [HLC paper (Kulkarni et al.)](https://cse.buffalo.edu/tech-reports/2014-04.pdf) · [Hybrid clock walkthrough](https://singhajit.com/distributed-systems/hybrid-clock/) · [CockroachDB hlc.go](https://github.com/cockroachdb/cockroach/blob/master/pkg/util/hlc/hlc.go) · [Go race detector](https://go.dev/doc/articles/race_detector)

---

### [SYNC-2] HLC state is never persisted and never wired into event creation
`high` · `effort: M` · `internal/sync/hlc.go` (whole type), `internal/sync/events.go:30`, no callers in `internal/cli`

**Problem.** `HLC.Last` lives only in memory on a stack-allocated struct: no load-from-SQLite at startup, no store-on-change. After any restart the clock resets to the current wall-clock millisecond. If the wall clock went backwards (NTP step, VM snapshot restore) or more than one event was generated in the last persisted millisecond, the fresh HLC can re-issue values `<=` ones already written, violating `e hb f => hlc.e < hlc.f` and the per-device monotonicity that `(hlc, device_id)` ordering relies on. Compounding this, `NewProjectEvent` receives `hlc` as a caller-supplied int64 and the only callers are tests passing `1` and `2` — the HLC type is not actually used to stamp real events anywhere.

**Evidence.**
```text
spec 07:178-179: "send: hlc = max(physical_now_ms<<16, last_hlc+1)"
hlc.go has only an in-memory Last int64; the only .Send()/.Receive() usages are in hlc_test.go.
events.go: func NewProjectEvent(deviceID, typ string, hlc int64, ...) — hlc injected, never produced by the clock.
```

**Recommendation.** Persist the local HLC (and per-device `seq`) in SQLite, load it at init/sync startup, and have a single `Clock` owned by the store/sync layer stamp every outgoing event. On startup initialize to `max(persisted_last_hlc, decode(MAX(hlc) FROM events WHERE device_id=self))` so a lost/younger persisted value cannot regress. Persist after each `Send` in the SAME transaction that inserts the event.

**Actionable Steps.**
1. Add a `device_clock` row (or reuse the device row) with `last_hlc`, `last_seq`.
2. On startup, load `last_hlc` and take the max with `SELECT MAX(hlc) FROM events` for the local device.
3. Make `clock.Send()` the only source of `hlc`/`seq`, written atomically with the event INSERT.
4. Add a regression test: create events, reopen the store, assert the next `Send()` exceeds the max persisted hlc even when `Now()` is earlier.

**Example.**
```go
func (s *Store) NextHLC(ctx context.Context, tx *sql.Tx, physMs int64) (int64, int64, error) {
    var lastHLC, lastSeq int64
    tx.QueryRowContext(ctx, `SELECT last_hlc, last_seq FROM device_clock WHERE device_id=?`, s.deviceID).Scan(&lastHLC, &lastSeq)
    next := physMs << 16
    if next <= lastHLC { next = lastHLC + 1 }
    lastSeq++
    if _, err := tx.ExecContext(ctx, `UPDATE device_clock SET last_hlc=?, last_seq=? WHERE device_id=?`, next, lastSeq, s.deviceID); err != nil {
        return 0, 0, err
    }
    return next, lastSeq, nil
}
```

**References.** [CockroachDB hlc.go](https://github.com/cockroachdb/cockroach/blob/master/pkg/util/hlc/hlc.go) · [HLC blog](https://muratbuffalo.blogspot.com/2014/07/hybrid-logical-clocks.html)

---

### [SYNC-3] `HLC.Receive` has no clock-skew guard; one bad remote timestamp permanently poisons the clock
`high` · `effort: S` · `internal/sync/hlc.go:20-31`

**Problem.** `Receive` computes `max(now, Last, remote)+1` and unconditionally adopts it. There is no maximum-offset check, so a remote event claiming to be 10 years in the future is accepted, pinning this device's clock ~10 years ahead forever — every subsequent local `Send()` inherits the poison, the device wins all future conflict tiebreaks, and HLCs never return near physical time. The threat model assumes untrusted, eventually signature-only peers, so a single buggy or malicious device can corrupt ordering for the whole namespace.

**Evidence.**
```go
func (h *HLC) Receive(remote int64) int64 {
    now := h.physical(); max := now
    if h.Last > max { max = h.Last }
    if remote > max { max = remote }
    h.Last = max + 1; return h.Last
}
```

**Recommendation.** Add a configurable `maxOffset` (e.g. 500ms like CockroachDB, or minutes for a loosely-synced laptop fleet) and reject remote HLCs whose physical component exceeds local physical time by more than the offset. Return an error so the sync loop can quarantine the offending event/peer instead of silently advancing.

**Actionable Steps.**
1. Add `MaxOffsetMs` to the clock config; decode the physical component via `hlc >> 16`.
2. In `Receive`, if `remotePhysMs - localPhysMs > maxOffset`, return `ErrUntrustworthyRemoteTime` and do NOT update `Last`.
3. In the apply loop, write a conflict/quarantine row and skip the event rather than aborting the batch.
4. Emit a structured `slog` warning (no secrets) with peer `device_id` and offset; add tests for within-offset (advances) and beyond-offset (rejected, `Last` unchanged).

**Example.**
```go
var ErrUntrustworthyRemoteTime = errors.New("remote hlc too far ahead")
func (h *HLC) Receive(remote int64) (int64, error) {
    nowMs, remoteMs := h.physicalMs(), remote>>counterBits
    if h.MaxOffsetMs > 0 && remoteMs-nowMs > h.MaxOffsetMs {
        return 0, fmt.Errorf("%w: +%dms", ErrUntrustworthyRemoteTime, remoteMs-nowMs)
    }
    // ... normal max(now, last, remote) advance ...
}
```

**References.** [CockroachDB hlc.go](https://github.com/cockroachdb/cockroach/blob/master/pkg/util/hlc/hlc.go) · [Clock management in CockroachDB](https://www.cockroachlabs.com/blog/clock-management-cockroachdb/)

---

### [SYNC-4] `ApplyEvents` is not transactional; event insert and side effects diverge on crash/retry, and conflicts are non-idempotent
`high` · `effort: M` · `internal/sync/events.go:51-100`, `internal/state/store.go:521,537`

**Problem.** For each event, `ApplyEvents` calls `st.InsertEvent` then separately calls `UpsertProject` or `InsertConflict` — three independent, auto-committed statements with no enclosing transaction. Idempotency holds only for the events row (via `INSERT OR IGNORE`); side effects are not gated on whether the event was newly inserted. Two failure modes: (1) a crash after `InsertEvent` but before the side effect re-runs the side effect on replay — fine for an idempotent upsert, but `InsertConflict` is NOT idempotent and duplicates a conflict row every replay; (2) the conflict branch `continue`s with no dedup key, so redelivering the same conflict-causing event creates another conflict row each time. There is also no `sync_cursors`/`event_delivery` update, so the loop has no durable "already applied" marker.

**Evidence.**
```go
for _, event := range events {
    if err := st.InsertEvent(ctx, event); err != nil { return err }
    switch event.Type {
    case EventProjectAdded, EventProjectUpdated:
        if err := st.InsertConflict(ctx, existing.ID, "same_path_different_remote", string(details)); err != nil { return err }
        continue
    case EventConflictCreated:
        if err := st.InsertConflict(ctx, "", "remote_conflict", event.PayloadJSON); err != nil { return err }
    }
}
```

**Recommendation.** Wrap each event's insert + side effects + cursor advance in a single transaction, and make "apply" conditional on the event being newly inserted (rows-affected from `INSERT OR IGNORE`, or an `event_delivery` row). Give conflicts a natural dedup key (`UNIQUE(namespace_id, type, incoming_event_id)`) so redelivery is a no-op.

**Actionable Steps.**
1. Begin a transaction per event so insert + upsert/conflict + cursor commit atomically.
2. Make `InsertEvent` report whether the row was new; skip side effects when not new.
3. Add a `UNIQUE` constraint / `ON CONFLICT DO NOTHING` for conflicts keyed by `(namespace_id, type, incoming_event_id)`.
4. Update `sync_cursors`/`event_delivery` in the same transaction; test that applying the same conflict-triggering event twice yields exactly one conflict row.

**Example.**
```go
func ApplyEvents(ctx context.Context, st *state.Store, events []state.Event) error {
    sortEvents(events)
    for _, e := range events {
        if err := st.WithTx(ctx, func(tx *state.Tx) error {
            inserted, err := tx.InsertEventIfNew(ctx, e)
            if err != nil || !inserted { return err } // already applied -> no-op
            if err := applySideEffects(ctx, tx, e); err != nil { return err }
            return tx.AdvanceCursor(ctx, e.DeviceID, e.HLC, e.Seq)
        }); err != nil { return err }
    }
    return nil
}
```

**References.** [SQLite transactions](https://www.sqlite.org/lang_transaction.html) · [HLC paper](https://cse.buffalo.edu/tech-reports/2014-04.pdf)

---

### [SYNC-5] Tombstone / delete / rename / restore semantics are specified in detail but entirely unimplemented
`medium` · `effort: M` · `internal/sync/events.go:51-100`, `migration 00002:2` (dead `tombstone_hlc`), `spec/07:229-253,396-426`

**Problem.** The spec defines precise tombstone semantics: `project.deleted` sets `status=deleted` and `tombstone_hlc=<event hlc>`; incoming `project.added`/`restored` older than the tombstone must be ignored; `project.renamed` is metadata-first with conflict cases. The migration even adds `namespace_entries.tombstone_hlc`. But `ApplyEvents` handles only `project.added`/`project.updated` and `conflict.created` — `EventProjectDeleted` is a declared constant never handled, and there are no rename/restore branches. A delete event is logged but never tombstones the entry, `tombstone_hlc` is never written, and a stale re-add resurrects a deleted project (classic lost-delete). Deletes don't converge and "silent divergence" — which the spec forbids — is exactly what happens.

**Evidence.**
```go
// events.go:18 EventProjectDeleted = "project.deleted" — defined but the switch handles
// only `case EventProjectAdded, EventProjectUpdated:` and `case EventConflictCreated:`.
// migration 00002:2 ALTER TABLE namespace_entries ADD COLUMN tombstone_hlc INTEGER; -- never referenced in store.go
```

**Recommendation.** Implement delete/restore/rename with HLC-gated tombstones: on `project.deleted` set `status=deleted` and `tombstone_hlc=event.HLC`; on `added`/`restored`, ignore if `event.HLC <= tombstone_hlc`; on rename, apply the metadata move or raise a conflict per the spec's per-state rules. Add tombstone GC only after all approved cursors pass the tombstone HLC.

**Actionable Steps.**
1. Add `EventProjectDeleted`, `EventProjectRestored`, `EventProjectRenamed` cases to the switch.
2. Add `TombstoneProject(path, hlc)` and a read of `tombstone_hlc` in the add/restore path.
3. In add/restore: if `tombstone_hlc != NULL && event.HLC <= tombstone_hlc`, skip (lost-delete guard); else clear the tombstone and upsert.
4. For delete-vs-dirty-local, never delete a dirty checkout — create a `pending_delete_conflict` row. Add tests for stale re-add (stays deleted), newer restore (reappears), and rename-onto-existing (conflicts).

**Example.**
```go
case EventProjectDeleted:
    var p ProjectPayload; json.Unmarshal([]byte(event.PayloadJSON), &p)
    if err := st.TombstoneProject(ctx, p.Path, event.HLC); err != nil { return err }
case EventProjectAdded, EventProjectUpdated:
    ts, _ := st.TombstoneHLC(ctx, payload.Path)
    if ts != 0 && event.HLC <= ts { continue } // stale add older than tombstone: ignore
    // ...clear tombstone + upsert...
```

**References.** [HLC blog](https://muratbuffalo.blogspot.com/2014/07/hybrid-logical-clocks.html) · [Local-first software](https://www.inkandswitch.com/local-first/)

---

### [SYNC-6] Same-path/different-remote conflict resolution is order-dependent across pull windows
`medium` · `effort: M` · `internal/sync/events.go:68-92`

**Problem.** *(verdict: revise — corrected text applied.)* The conflict check reads mutable DB state via `st.ProjectByPath` instead of re-evaluating the winner from the full set of competing adds, and conflict creation is non-idempotent. Within a single replay batch this is deterministic (a total `(hlc, device_id)` order is imposed), but divergence arises **across pull windows**: `FileHub.Pull(afterHLC)` returns only events with `HLC > afterHLC`, so a device that applied add A earlier and later receives competing add B alone keeps A; a device that applied B first in an earlier window keeps B. The surviving entry and the conflict's `existing_key`/`incoming_key` thus depend on per-device pull history rather than a pure function of the competing pair. Compounding this, `InsertConflict` has no unique constraint and no `INSERT OR IGNORE`, so re-applying after a snapshot re-bootstrap appends duplicate conflict rows.

**Evidence.**
```go
existing, err := st.ProjectByPath(ctx, payload.Path)
if err == nil && existing.RemoteKey != "" && payload.RemoteKey != "" && existing.RemoteKey != payload.RemoteKey {
    ... InsertConflict(...same_path_different_remote...) ...
    continue
}
_, err = st.UpsertProject(ctx, ...) // otherwise last-writer-wins by arrival
```

**Recommendation.** Make `reconcileSamePath(a, b)` a pure, commutative function of the competing pair `(path, remote_key, hlc, device_id)` that deterministically picks the canonical winner and emits a stable conflict record keyed by the unordered pair of remote_keys — re-evaluated from the full set of competing adds, independent of arrival order (ties into SYNC-4's conflict idempotency/uniqueness).

**Actionable Steps.**
1. Define a deterministic winner rule, e.g. lowest `(hlc, device_id)`.
2. Key the conflict by `sorted(remote_key_a, remote_key_b) + path` so it is created identically and idempotently on every device.
3. When both adds are present, set the surviving entry to the deterministic winner regardless of arrival.
4. Document that `created_at` must never influence this; add a property test feeding the same two events in both orders asserting identical final project + identical conflict. Extend the shape to concurrent renames and delete-vs-dirty.

**Example.**
```go
func reconcileSamePath(a, b ProjectPayload, ha, hb int64, da, db string) (winner ProjectPayload, conflictKey string) {
    if less(ha, da, hb, db) { winner = a } else { winner = b }
    k1, k2 := a.RemoteKey, b.RemoteKey
    if k2 < k1 { k1, k2 = k2, k1 }
    return winner, a.Path + "|" + k1 + "|" + k2 // stable, dedup-able
}
```

**References.** [Local-first software](https://www.inkandswitch.com/local-first/) · [HLC paper](https://cse.buffalo.edu/tech-reports/2014-04.pdf)

---

## Security

### [SEC-1] Git argument/protocol injection: untrusted remote URLs reach git with no `--` separator or protocol allowlist (RCE via scan→adopt→hydrate)
`critical` · `effort: M` · `internal/git/git.go:49-71`, `internal/cli/hydrate.go:77`, `internal/scan/scan.go:119-133`

**Problem.** Remote URLs flow into the system git binary as positional arguments with no `--` end-of-options separator and no protocol restriction. Git's `ext::` transport executes arbitrary shell commands, and `--upload-pack=<cmd>` / `-u <cmd>` also execute commands. The `add` command rejects these because `CanonicalRemoteKey` errors on `ext::`/leading-dash, but **`scan` stores the RAW value** of `git remote get-url origin` even when `CanonicalRemoteKey` fails (it only appends a warning), and adopt persists it verbatim. On the next `hydrate`/`open`/`worktree new`, `hydrate.go:77` calls `r.Clone(ctx, project.RemoteURL, ...)` with that attacker-controlled string. A repo whose `.git/config` contains `[remote "origin"] url = ext::sh -c <cmd>` (a shared repo, malicious template, or tampered checkout) yields RCE the moment the user materializes it. `file://`/absolute-path remotes are also accepted.

**Evidence.**
```text
git.go:54  args = append(args, remote, dest) then r.Run(ctx, "", args...) — no `--`
hydrate.go:77  r.Clone(ctx, project.RemoteURL, localPath, partial)
scan.go:121-128  f.RemoteURL = remote  and only warns on CanonicalRemoteKey failure
```

**Recommendation.** Treat every remote URL as untrusted at the git boundary: (1) add `--` before any user-controlled positional in `Clone`/`Fetch`/`RemoteURL`/`RevParse`; (2) hard-deny dangerous protocols with `-c protocol.ext.allow=never -c protocol.file.allow=user -c protocol.allow=user` and `GIT_PROTOCOL_FROM_USER=0` (see SEC-2); (3) validate remotes with `CanonicalRemoteKey`/a scheme allowlist BEFORE storing them in scan, not just warn; (4) reject any remote string beginning with `-`.

**Actionable Steps.**
1. Add a hardening-config prefix to every git invocation and insert `--` before positional remote/dest/ref args.
2. Add `safeRemote(remote)` rejecting leading `-` and `ext::`/`fd::`/bare `file://` (unless an explicit `AllowLocal` flag); call it in `Clone`/`Fetch`.
3. In `scan.go`, only set `f.RemoteURL` when `CanonicalRemoteKey` succeeds; never persist an unvalidated origin.
4. In `hydrate.go`, call `safeRemote` before `r.Clone`; add table-driven tests for `ext::`, `--upload-pack=`, `-u`, `fd::`, `file://` payloads asserting end-to-end rejection.

**Example.**
```go
var hardenedConfig = []string{
    "-c", "protocol.ext.allow=never",
    "-c", "protocol.file.allow=user",
    "-c", "protocol.allow=user",
}
func safeRemote(remote string) error {
    if strings.HasPrefix(remote, "-") { return fmt.Errorf("refusing remote that looks like a flag: %q", remote) }
    lower := strings.ToLower(remote)
    for _, bad := range []string{"ext::", "fd::"} {
        if strings.HasPrefix(lower, bad) { return fmt.Errorf("refusing dangerous git transport: %q", remote) }
    }
    return nil
}
func (r Runner) Clone(ctx context.Context, remote, dest string, partial bool) error {
    if err := safeRemote(remote); err != nil { return err }
    args := append([]string{}, hardenedConfig...)
    args = append(args, "clone")
    if partial { args = append(args, "--filter=blob:none") }
    args = append(args, "--", remote, dest) // -- stops option parsing
    _, err := r.Run(ctx, "", args...)
    return err
}
```

**References.** [GitPython arg-injection fix](https://github.com/gitpython-developers/GitPython/pull/1516) · [go-git GHSA-v725-9546-7q7m](https://github.com/go-git/go-git/security/advisories/GHSA-v725-9546-7q7m) · [Exploiting git ext::](https://www.codeant.ai/blogs/exploiting-git%E2%80%99s-ext-protocol-for-command-execution) · [git-remote-ext](https://git-scm.com/docs/git-remote-ext)

---

### [SEC-2] Git subprocesses inherit full environment and system/global config
`high` · `effort: M` · `internal/git/git.go:24-47`, `internal/cli/open.go:33`

**Problem.** `Runner.Run` never sets `cmd.Env`, so git inherits the entire parent environment, and no `GIT_CONFIG_NOSYSTEM`/`GIT_CONFIG_GLOBAL` isolation is applied. An attacker who influences the environment or the user's global/system gitconfig can set `GIT_SSH_COMMAND`, `core.fsmonitor`, `protocol.ext.allow=always`, or `url.<base>.insteadOf` to redirect or execute on every git call — directly defeating SEC-1 mitigations that rely only on `-c` flags a malicious repo-local `.git/config` could fight. `spec/15:170` lists `GIT_SSH_COMMAND` as a dangerous env name to be "stripped last and unconditionally"; nothing strips it. `open` also runs `cursor`/`code <path>` with full inherited env and a path argument with no `--`. *(Pairs with GO-1, which disables prompts and bounds time on the same `cmd.Env`.)*

**Evidence.**
```text
git.go:29  cmd := exec.CommandContext(ctx, bin, args...) — cmd.Env never assigned
spec/15:170 lists GIT_SSH_COMMAND under dangerous env names
spec/09:233 "Child process environments start empty; DevStrap must not inherit os.Environ() by default."
```

**Recommendation.** Construct an explicit, minimal env for all git subprocesses: start from a curated allowlist (`PATH`, `HOME`, plus git auth vars you intend to support), unconditionally strip the dangerous set (`LD_PRELOAD`, `DYLD_INSERT_LIBRARIES`, `BASH_ENV`, `NODE_OPTIONS`, `PYTHONPATH`, `GIT_SSH_COMMAND`, `GIT_CONFIG`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`), set `GIT_TERMINAL_PROMPT=0`, and for untrusted-repo materialization set `GIT_CONFIG_NOSYSTEM=1`. Apply the same discipline to the editor exec.

**Actionable Steps.**
1. Add a `baseEnv()` helper returning a sanitized `[]string` (allowlist + `GIT_TERMINAL_PROMPT=0` + `GIT_CONFIG_NOSYSTEM=1`) and assign `cmd.Env` in `Run`.
2. Strip the dangerous-name set unconditionally even if a caller adds vars.
3. For not-yet-trusted repos, also set `GIT_CONFIG_GLOBAL=/dev/null` during the clone.
4. Add `--` before the path arg in `open.go` and pass a minimal env; unit-test that `GIT_SSH_COMMAND` set in the parent does not appear in the child (fake git that dumps env).

**Example.**
```go
var dangerousEnv = map[string]bool{
    "LD_PRELOAD": true, "DYLD_INSERT_LIBRARIES": true, "BASH_ENV": true,
    "NODE_OPTIONS": true, "PYTHONPATH": true, "GIT_SSH_COMMAND": true,
    "GIT_CONFIG": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
}
func baseEnv() []string {
    out := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"}
    for _, kv := range os.Environ() {
        name, _, _ := strings.Cut(kv, "=")
        if dangerousEnv[name] { continue }
        switch name { case "PATH", "HOME", "SSH_AUTH_SOCK": out = append(out, kv) }
    }
    return out
}
// in Run: cmd.Env = baseEnv()
```

**References.** [git-config protocol.allow](https://git-scm.com/docs/git-config#Documentation/git-config.txt-protocolallow) · [GIT_CONFIG_NOSYSTEM](https://git-scm.com/docs/git#Documentation/git.txt-codeGITCONFIGNOSYSTEMcode)

---

### [SEC-5 / GO-2] Repo lock is a non-atomic stale lockfile (no PID/owner, leaks on crash); hydrate clone is unprotected and has a TOCTOU
`medium`/`high` · `effort: M` · `internal/cli/worktree.go:225-240`, `internal/cli/hydrate.go:64-87`
*(Merged: GO-2 folded in — the same stale-lock reliability hole.)*

**Problem.** *(verdict: revise — corrected text applied.)* `acquireRepoLock` uses `O_CREATE|O_EXCL` to make `<home>/locks/<projectID>.lock` and returns a closure that only `os.Remove`s it. For a normal Ctrl-C/SIGTERM this is fine (the signal cancels the context, the in-flight git op errors out, and the deferred unlock runs during normal unwinding). The real hole is the cases where defers do NOT run — **SIGKILL, an unrecovered panic, `os.Exit`, or power loss during the long fetch/clone/worktree-add** — plus the empty lock body (no PID/timestamp/host). Because there is no stale-lock detection anywhere, any such crash leaves a permanent lock file and every subsequent `worktree new` on that project fails with "repo operation already in progress" until the user manually deletes it. Separately (SEC-5), **hydrate is not protected by this lock** (it runs before `acquireRepoLock`, and `hydrate`/`open` take no lock at all), so two concurrent `hydrate`/`open` invocations race: `RemoveAll` then clone, two processes deleting each other's partial clone, with a check-then-act TOCTOU against `IsRepo`.

**Evidence.**
```go
worktree.go:231  file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // writes nothing, no PID
hydrate.go:73    if err := os.RemoveAll(localPath); err != nil { ... }                        // no lock held by hydrate/open
```

**Recommendation.** Use an advisory OS lock the kernel releases automatically on process exit/crash. `golang.org/x/sys` is already an (indirect) dependency, so `unix.Flock(fd, LOCK_EX|LOCK_NB)` on the open fd is preferable to adding `github.com/gofrs/flock`. Keep the fd open for the operation; write PID + timestamp into the file for diagnostics. Acquire the lock **around the entire hydrate** (RemoveAll/clone) critical section (and before any fetch/clone work in `worktree new`), and replace check-then-RemoveAll with atomic temp-clone-then-rename (see GO-7).

**Actionable Steps.**
1. Open the lockfile and `unix.Flock(int(f.Fd()), LOCK_EX|LOCK_NB)`; keep the fd open for the lock lifetime so the OS releases it on crash.
2. Write `{pid, host, started_at}` for diagnostics/stale detection; the unlock closure releases the flock and closes the fd.
3. Acquire the lock inside `hydrateProject` (or before hydrate) so RemoveAll+Clone is mutually exclusive; move `acquireRepoLock` ahead of the hydrate call in `worktree new`.
4. Add a `worktree unlock <path>`/`doctor` escape hatch reporting and clearing dead locks; document recovery.

**Example.**
```go
func acquireRepoLock(home, projectID string) (func(), error) {
    lockDir := filepath.Join(home, "locks")
    if err := os.MkdirAll(lockDir, 0o700); err != nil { return nil, err }
    f, err := os.OpenFile(filepath.Join(lockDir, projectID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
    if err != nil { return nil, err }
    if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
        f.Close()
        return nil, appError{code: exitConflict, err: fmt.Errorf("repo busy: %s", projectID)}
    }
    fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
    return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil // kernel releases on death
}
```

**References.** [flock(2)](https://man7.org/linux/man-pages/man2/flock.2.html) · [gofrs/flock](https://pkg.go.dev/github.com/gofrs/flock) · [TOCTOU](https://owasp.org/www-community/vulnerabilities/Time_of_check_to_time_of_use)

---

### [SEC-4] Symlink-escape handling is advisory-only and TOCTOU-racy
`medium` · `effort: M` · `internal/scan/scan.go:91-99`, `internal/pathkey/pathkey.go:57-77`

**Problem.** *(verdict: revise — corrected text applied.)* `spec/15:132-134` requires "do not follow symlinks by default", "detect symlink targets", and "block escapes from managed root". The scan only WARNs on an escaping symlink and records a non-blocking conflict on `--adopt`; it does not hard-exclude or block materialization. (`filepath.WalkDir` does not descend into symlinked directories and the symlink is never recorded as a Finding, so the escaped target is not directly adopted — but nothing prevents a later operation that resolves the link.) `CheckSymlinkWithinRoot` calls `filepath.EvalSymlinks` then a separate `Rel` check: a classic check-then-use TOCTOU, run ONLY at scan time (no callers in `internal/git`, `internal/sync`, or hydrate/worktree), so the link can be repointed before any later use. The function also conflates dangling/IO errors with escapes.

**Evidence.**
```go
// scan.go:92 warning only:
if err := pathkey.CheckSymlinkWithinRoot(cleanRoot, path); err != nil {
    result.Warnings = append(result.Warnings, fmt.Sprintf("symlink escape: %s", rel))
}
// pathkey.go:62 EvalSymlinks then Rel — decoupled from any use
```

**Recommendation.** Make escapes a hard exclusion, record a blocking conflict, return typed errors (`ErrEscape` vs dangling vs IO) so callers can distinguish, and re-validate symlink targets immediately before any hydrate/worktree operation that uses them (open-then-verify / `RESOLVE_BENEATH`-style, or Go 1.24 `os.Root`) to close the TOCTOU window.

**Actionable Steps.**
1. In `scan.go`, on `ErrEscape` skip the entry entirely (no Finding) and emit a conflict; only warn for non-escape resolution errors.
2. Return typed errors from `CheckSymlinkWithinRoot`.
3. Re-validate the symlink target at use-time in hydrate/worktree paths, not just at scan-time.
4. Add tests for: symlink outside root, symlink repointed between scan and hydrate, and dangling symlink.

**Example.**
```go
if d.Type()&fs.ModeSymlink != 0 {
    switch err := pathkey.CheckSymlinkWithinRoot(cleanRoot, path); {
    case errors.Is(err, pathkey.ErrEscape):
        result.Warnings = append(result.Warnings, fmt.Sprintf("symlink escape (excluded): %s", relSlash))
        return fs.SkipDir // exclude, do NOT create a Finding
    case err != nil:
        result.Warnings = append(result.Warnings, fmt.Sprintf("symlink unresolved: %s: %v", relSlash, err))
        return nil
    }
}
```

**References.** [openat2](https://man7.org/linux/man-pages/man2/openat2.2.html) · [TOCTOU](https://owasp.org/www-community/vulnerabilities/Time_of_check_to_time_of_use) · [os.Root](https://go.dev/blog/osroot)

---

### [SEC-6] Spec/15 controls are documented as partial but are absent: device-key signatures, env sanitization, agent policy
`medium` · `effort: L` · `spec/15`, `spec/09:233`, `spec/10`, `CLAUDE.md`, `internal/state/store.go:537-562`

**Problem.** *(verdict: revise — corrected text applied.)* The threat model promises controls with no implementation: (a) dangerous-env stripping and empty-by-default child env — no `internal/env` exists, so any future child inherits the full environment; (b) 0600 enforcement for env files — no capture exists, and secret-looking files are only WARNED on; (c) device-key **signatures** on trust-affecting audit events — there is NO signature column (migration 00002 adds only `content_hash` and `prev_event_hash`) and no signing key, so "event signatures from day one" is unmet and forgery detection is absent; (d) agent command/file policy — no policy package exists. **NOTE:** contrary to a naive reading of `store.go:552`, content hashes are NOT mere placeholders on the operative path — `internal/sync/events.go:39-47` computes a real sha256 over the canonical payload and `InsertEvent` only falls back to `sha256:unset` when `ContentHash` is empty; the remaining weakness is that `InsertEvent` trusts the caller's hash without recomputation. This is a documentation-vs-reality integrity gap (`CLAUDE.md` lists redaction as done while it is unwired — see ENV-2).

**Evidence.**
```go
// store.go:552  event.ContentHash = "sha256:unset"  (fallback only; NewProjectEvent sets a real hash)
// scan.go:103   warn-only on isSecretName(...)
// migration 00002 adds content_hash + prev_event_hash but NO signature column
```

**Recommendation.** Prioritize the genuinely-absent controls. (1) Add an ed25519 device signing key at init (OS keychain or 0600 interim), add a `signature` column, and sign the trust-affecting event types (spec/15:217-228) over `(id,hlc,type,payload_json,content_hash,prev_event_hash)`. (2) Harden hash integrity by having `InsertEvent` recompute/verify `content_hash` rather than trusting the caller (reuse `NewProjectEvent`'s sha256 logic — do not add a parallel impl). (3) Implement the empty-by-default sanitized child-env builder (shared with SEC-2) and a `--quarantine`/ignore-rule action for secret-looking files instead of warn-only. (4) Update `CLAUDE.md`/spec to explicitly enumerate unimplemented security controls.

**Actionable Steps.**
1. Generate an ed25519 key at init; add the `signature` column; sign trust-affecting events.
2. Make `InsertEvent` recompute and reject mismatched `content_hash` instead of defaulting to a placeholder.
3. Implement the sanitized env builder and the secret-file quarantine action.
4. Correct `CLAUDE.md`'s redaction claim and enumerate unimplemented controls; test that a tampered payload fails signature verification.

**Example.**
```go
// store.go InsertEvent: verify, do not re-introduce, the content hash.
expected := "sha256:" + hex.EncodeToString(sha256New(event.PayloadJSON))
if event.ContentHash != "" && event.ContentHash != expected { return fmt.Errorf("content hash mismatch") }
if event.ContentHash == "" { event.ContentHash = expected }
// Separately: add an ed25519 signature column populated for trust-affecting types
// over (id, hlc, type, payload_json, content_hash, prev_event_hash).
```

**References.** [age authentication](https://words.filippo.io/dispatches/age-authentication/) · [crypto/ed25519](https://pkg.go.dev/crypto/ed25519)

---

## Git & Worktrees

### [GIT-2] `DefaultBranch` silently falls back to `main` on any git error; can base a worktree on a non-existent branch
`high` · `effort: S` · `internal/git/git.go:73-82`, consumed at `internal/cli/worktree.go:64-75`

**Problem.** `DefaultBranch` runs `git symbolic-ref --short refs/remotes/origin/HEAD` and on ANY error returns the caller's fallback, ultimately `"main"`. The error is swallowed — no distinction between "remote default is genuinely main" and "origin/HEAD unset / git errored / network blip". In `worktree new`, this branch is fed straight into `git fetch origin <branch>` and `git rev-parse origin/<branch>`. If the true default is `master`/`develop`/`trunk` and `origin/HEAD` is unset (common after `--single-branch`, mirror clones, older fetch configs), DevStrap fetches and bases the worktree on `origin/main`, which may not exist — a confusing fetch failure or a base on the wrong branch. The spec (08:100-114) requires resolving from `origin/HEAD` and falling back only when the remote default is genuinely unavailable.

**Evidence.**
```go
// git.go:74-81: out, err := r.Run(ctx, dir, "symbolic-ref", ...); if err == nil { return ... }
//               if fallback != "" { return fallback }; return "main"
// fallback taken on every non-nil error with no logging or repair attempt.
```

**Recommendation.** When `origin/HEAD` is missing, attempt `git remote set-head origin --auto` (queries the remote) before falling back, and return a typed result distinguishing `remote`|`stored`|`hardcoded`. Log at warn level when falling back. In `worktree new`, prefer `git ls-remote --symref origin HEAD` (authoritative, network) to confirm the default before fetching.

**Actionable Steps.**
1. Add a `git remote set-head origin --auto` attempt inside a new `ResolveDefaultBranch` before falling back.
2. Return `(branch, source)` and warn when source isn't `remote`.
3. In `worktree new`, confirm with `git ls-remote --symref origin HEAD` before fetching.
4. Add tests for `origin/HEAD` set to a non-main branch, unset, and a remote whose default is `develop`.

**Example.**
```go
func (r Runner) ResolveDefaultBranch(ctx context.Context, dir, fallback string) (string, string) {
    if out, err := r.Run(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
        return strings.TrimPrefix(out, "origin/"), "remote"
    }
    _, _ = r.Run(ctx, dir, "remote", "set-head", "origin", "--auto")
    if out, err := r.Run(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
        return strings.TrimPrefix(out, "origin/"), "remote"
    }
    if fallback != "" { return fallback, "stored" }
    return "main", "hardcoded"
}
```

**References.** [git-clone](https://git-scm.com/docs/git-clone) · [git-remote](https://git-scm.com/docs/git-remote)

---

### [GIT-1] Agent worktrees from blobless clones ignore the stored LFS policy; objects remain pointer files
`high` · `effort: M` · `internal/cli/worktree.go:54,89`, `internal/cli/hydrate.go:47-87`

**Problem.** *(verdict: revise — corrected text applied.)* `worktree new` always hydrates with `partial=true` and never resolves Git LFS, so on an LFS repo the agent worktree contains pointer files. This is partly by design (spec/08 sets `lfs_pull_for_agent: false`), but the code (a) never reads the existing `git_repos.lfs_policy` column (stored at `store.go:62` `LFSPolicy` but entirely unused), and (b) gives no warning that pointers are unresolved. Additionally, because partial clone uses `--filter=blob:none`, `git worktree add <base-sha>` back-fills blobs via on-demand promisor fetch, which fails mid-checkout if the promisor remote is unreachable (offline/flaky).

**Evidence.**
```go
worktree.go:54  localPath, err := hydrateProject(cmd.Context(), opts, args[0], true) // hardcoded partial=true
// git_repos.lfs_policy (00001:49, store.go:62 LFSPolicy) is stored but never read
```

**Recommendation.** Honor the stored `lfs_policy`/spec policy rather than auto-pulling. Default for agent worktrees should be NO LFS pull (per spec `lfs_pull_for_agent: false`); when LFS is detected and policy does not opt in, warn loudly that objects are pointers. Only run `git -C <worktree> lfs pull` when policy opts in. Wire up the already-present `lfs_policy` column. Surface a clear error if on-demand blob fetch fails because the promisor remote is unreachable.

**Actionable Steps.**
1. Read `project.LFSPolicy` (from `git_repos.lfs_policy`) in `newWorktreeNewCommand`.
2. Add `func (r Runner) UsesLFS(ctx, dir string) bool` (check `.gitattributes` for `filter=lfs`).
3. After `WorktreeAdd`, branch on policy: `always`/`agent` → `lfs pull` (fail on error); otherwise warn that the worktree holds pointer files.
4. Add an integration test creating a worktree from an LFS repo and asserting the policy-driven behavior.

**Example.**
```go
if r.UsesLFS(cmd.Context(), wtPath) {
    switch project.LFSPolicy { // read from git_repos.lfs_policy (already stored)
    case "always", "agent":
        if _, err := r.Run(cmd.Context(), wtPath, "lfs", "pull"); err != nil {
            return appError{code: exitInvalidConfig,
                err: fmt.Errorf("worktree created but LFS pull failed (objects are pointers): %w", err)}
        }
    default: // spec default for agents is no pull
        fmt.Fprintf(stdout, "warning: %s uses Git LFS; worktree %s contains pointer files (lfs_policy=%s)\n",
            project.Path, wtPath, project.LFSPolicy)
    }
}
```

**References.** [Partial & shallow clone](https://github.blog/open-source/git/get-up-to-speed-with-partial-clone-and-shallow-clone/) · [partial-clone](https://git-scm.com/docs/partial-clone.html) · [git-worktree](https://git-scm.com/docs/git-worktree.html)

---

### [GIT-3] Dirty-worktree guard is bypassable: missing path errors out instead of being handled; remove never prunes
`medium` · `effort: M` · `internal/cli/worktree.go:160-211`, `internal/cli/hydrate.go:66`

**Problem.** The core invariant is "never destroy dirty worktrees." In `remove`, a non-nil `DirtyState` error aborts the whole command, but `DirtyState` errors when the worktree dir is missing/moved/unreadable — so a stale DB row with a missing tree cannot be removed without a manual git fix. In `cleanup`, the symmetric path treats a `DirtyState` error as "skip", so an unreadable worktree is silently left behind forever. There is no `--force` escape hatch (spec 08:219-224 calls for `cleanup: block unless --force or branch merged`), and `worktree remove` never calls `git worktree prune`, so dangling metadata accumulates.

**Evidence.**
```go
// remove:  dirty, err := r.DirtyState(...); if err != nil { return err }
// cleanup: dirty, err := r.DirtyState(...); if err != nil || dirty != DirtyClean { continue }
// hydrate.go:66: dirty, _ := r.DirtyState(ctx, localPath) // error discarded
```

**Recommendation.** Treat "path missing" distinctly from "dirty." If the tree is gone, allow removing the DB row plus `git worktree prune`. Add `--force` to `remove` (respected in cleanup) that still refuses on genuinely dirty trees unless forced. Always `git worktree prune` after a successful remove. Log, don't swallow, `DirtyState` errors.

**Actionable Steps.**
1. Add a helper returning `(state, exists bool, err error)` distinguishing ENOENT from git errors.
2. In `remove`: if `!exists`, mark removed + prune and succeed; if dirty and not `--force`, refuse.
3. In `cleanup`: surface a count of worktrees skipped due to errors; prune after removal.
4. Add `--force` to `worktree remove`; run `git worktree prune` after every successful remove.

**Example.**
```go
st, exists, err := r.DirtyStateAt(ctx, wt.Path)
if !exists { _ = r.Run(ctx, localPath, "worktree", "prune"); return store.MarkWorktreeRemoved(ctx, args[0]) }
if err != nil { return err }
if st != dsgit.DirtyClean && !force {
    return appError{code: exitDirtyWorktree, err: fmt.Errorf("dirty: %s", st)}
}
```

**References.** [git-worktree](https://git-scm.com/docs/git-worktree.html)

---

### [GIT-4 / GO-8] Two independent SQLite writer pools opened against the same DB within one command
`medium` · `effort: M` · `internal/cli/worktree.go:45-57`, `internal/cli/hydrate.go:47-52`, `internal/cli/root.go:134-140`
*(Merged: GO-8 folded in — the same per-helper open/close fragmentation.)*

**Problem.** `openState` builds a new `*sql.DB` with `SetMaxOpenConns(1)`. In `worktree new`, the command opens a store and, while it is still open, calls `hydrateProject`, which opens a *second* independent `*sql.DB` to the same SQLite file and writes (`UpdateProjectLocalState`). A single logical operation holds two single-connection writer pools against one WAL database. WAL + `busy_timeout(5000)` usually masks this, but it is a self-contention footgun: if the outer connection holds a write transaction, the inner pool can hit `SQLITE_BUSY` after the timeout. It also undermines the per-repo lock's intent (acquired *after* the second store is already opened and used) and does not map onto the planned long-lived daemon, where the `Store` should be a singleton.

**Evidence.**
```go
worktree.go:45  store, err := opts.openState()
worktree.go:54  hydrateProject(...)   // hydrate.go:48 opens a second store + defer store.Close()
root.go:139     return state.Open(paths.StateDB()) // no shared singleton
```

**Recommendation.** Open the `Store` once per command and thread it (or a `*Store` on `options`) through helpers like `hydrateProject` instead of having each helper open its own connection. Acquire the repo lock before any hydrate/fetch work. This also prepares cleanly for the daemon (single `*state.Store` singleton).

**Actionable Steps.**
1. Change `hydrateProject` to accept an already-open `*state.Store` it does not own (caller closes).
2. Update `worktree new`/`open`/`hydrate` to open the store once, defer `Close`, and pass it down.
3. For the daemon, store a single `*state.Store` on `options` and have `openState` return it rather than calling `state.Open` each time.
4. Move `acquireRepoLock` ahead of the hydrate call; add a test running hydrate within an open outer transaction asserting no `SQLITE_BUSY`.

**Example.**
```go
func hydrateProject(ctx context.Context, store *state.Store, opts *options, nsPath string, partial bool) (string, error) {
    project, err := store.ProjectByPath(ctx, nsPath) // reuse caller's store; no defer Close here
    // ...
}
// worktree new:
store, _ := opts.openState(); defer store.Close()
unlock, _ := acquireRepoLock(opts.paths().Home, project.ID); defer unlock()
localPath, err := hydrateProject(cmd.Context(), store, opts, args[0], true)
```

**References.** [database/sql DB](https://pkg.go.dev/database/sql#DB) · [SQLite WAL](https://www.sqlite.org/wal.html) · [SQLITE_BUSY](https://www.sqlite.org/rescode.html#busy)

---

### [GIT-6] Network/origin/scp-with-port git failures lack handling; thin `git_test.go` coverage
`medium` · `effort: M` · `internal/git/git.go:59-71,151-185`, `internal/git/git_test.go`

**Problem.** *(verdict: revise — corrected text applied.)* Real-world git failures surface as raw, undifferentiated errors with no retry/typing: (a) `Fetch`/`Clone` network failures bubble up as opaque strings — no distinction between auth/unreachable/branch-not-found, so a transient blip aborts worktree creation with no retry; (b) `RemoteURL` assumes `origin` exists (the worktree flow does not handle a repo with no/renamed origin); (c) port normalization is inconsistent — note the `:22` case **is** already handled, but non-standard ssh:// ports are not stripped (`ssh://git@host:2222/org/repo.git` → `host:2222/org/repo` because `u.Host` is used, not `u.Hostname()`), and scp-like remotes with an explicit port are mishandled (`git@host:2222:org/repo.git` → `host/2222:org/repo` because `SplitN(remote, ":", 2)` folds the port into the path) — both defeat duplicate detection; (d) `git_test.go` has only 2 tests, both `CanonicalRemoteKey`.

**Evidence.**
```text
git.go:60-66 Fetch returns the raw error; cli/worktree.go:68 surfaces it opaquely
git.go:178 uses u.Host (includes :port) instead of u.Hostname()
git.go:157 SplitN(remote, ":", 2) folds an scp-like port into the path
git_test.go: exactly 2 test funcs
```

**Recommendation.** Introduce typed errors (`ErrNetwork`, `ErrAuth`, `ErrBranchNotFound`) by classifying git stderr, add a bounded retry with backoff for transient network errors only, and verify `origin` exists with a clear message. Normalize `:port` consistently: use `u.Hostname()` for url-form and drop the port segment for scp-like. Expand `git_test.go` to cover `DefaultBranch`, `DirtyState`, scp-with-port, and a local-bare end-to-end worktree creation.

**Actionable Steps.**
1. Add `classifyGitError(stderr string) error` mapping known substrings to typed sentinels.
2. Wrap `Fetch`/`Clone` in retry-with-backoff for transient network errors only (not auth/not-found).
3. In `CanonicalRemoteKey`, use `u.Hostname()` for the url branch and parse/drop `:<port>` in the scp-like branch.
4. Add table-driven tests (ssh with port, scp with port, uppercase org, trailing slash) and a local-bare e2e: clone → worktree new → assert base SHA == origin/<default>.

**Example.**
```go
func CanonicalRemoteKey(remote string) (string, error) {
    // url.Parse branch:
    host := u.Hostname() // drops :port
    return normalizeHostPath(host, strings.TrimPrefix(u.Path, "/")), nil
}
var errNetwork = errors.New("git network error")
func classifyGitError(stderr string) error {
    switch {
    case strings.Contains(stderr, "Could not resolve host"),
         strings.Contains(stderr, "Connection timed out"):
        return errNetwork
    case strings.Contains(stderr, "Authentication failed"),
         strings.Contains(stderr, "Permission denied"):
        return errAuth
    }
    return nil
}
```

**References.** [git-fetch](https://git-scm.com/docs/git-fetch) · [git-clone](https://git-scm.com/docs/git-clone)

---

### [GIT-5] Worktree branch suffix derived from last 4 chars of a UUIDv7 risks collisions
`low` · `effort: S` · `internal/cli/worktree.go:79-84`, `internal/git/git.go:89`

**Problem.** *(verdict: revise — corrected text applied.)* The branch name uses `short, _ := id.New("x")` and then `short[len(short)-4:]` as the disambiguator. UUIDv7's trailing characters are the random tail, so 4 hex chars (~16 bits) gives a non-trivial birthday-collision probability when many worktrees are created the same day with the same task slug. With only the date (YYYYMMDD, no time) plus this 4-char tail, two same-day worktrees for the same task have only ~16 bits of separation. `git worktree add -b <branch>` fails if the branch exists, and there is no retry, so a collision aborts creation — an avoidable, confusing failure. (NOTE: the spec format is date-only with a 4-char shortid, so the code already matches the spec; there is no spec-vs-code divergence on the time component.)

**Evidence.**
```go
branch := fmt.Sprintf("agent/%s-%s-%s", slug, time.Now().UTC().Format("20060102"), short[len(short)-4:])
// git.go:89 WorktreeAdd uses `worktree add -b <branch>` with no retry
```

**Recommendation.** Independently of the spec, harden disambiguation: use a longer dedicated random suffix (6–8 chars from a fresh token) instead of slicing a UUID tail, optionally add a time component (HHMMSS), and wrap `WorktreeAdd` in a small retry loop that regenerates the suffix when git reports the branch already exists.

**Actionable Steps.**
1. Add `HHMMSS` to the timestamp: `Format("20060102-150405")`.
2. Generate a dedicated 6-char random suffix instead of slicing the UUID tail.
3. Wrap `WorktreeAdd` in a retry loop regenerating the suffix on an "already exists" error.
4. Test creating two worktrees for the same slug in the same second and assert both succeed.

**Example.**
```go
branch := fmt.Sprintf("agent/%s-%s-%s", slug, time.Now().UTC().Format("20060102-150405"), randSuffix(6))
for i := 0; i < 3; i++ {
    if err := r.WorktreeAdd(ctx, localPath, wtPath, branch, baseSHA); err == nil { break } else if !strings.Contains(err.Error(), "already exists") { return err }
    branch = fmt.Sprintf("agent/%s-%s-%s", slug, time.Now().UTC().Format("20060102-150405"), randSuffix(6))
}
```

**References.** [git-worktree](https://git-scm.com/docs/git-worktree.html)

---

## Secrets & Environment

### [ENV-1] Device identity has no keypair: the entire age recipient model has no root of trust
`high` · `effort: M` · `internal/state/store.go:222`, `internal/state/migrations/00001_initial.sql:10-21`, `internal/cli/init.go:74`

**Problem.** `spec/09` states "Each device has an age X25519 identity" and "device private identity → local OS keychain only", with encryption defined as "one recipient stanza per approved device X25519 public key". But `EnsureDevice` creates the local device row with `public_key` NULL, never generates an X25519 keypair, never stores a private identity, and never records the public key. There is therefore no recipient to encrypt env bundles to and no identity to decrypt with — the foundational primitive for Mode A is absent even though the schema and threat model assume it. Without it, device approval, fingerprint verification, and bundle re-encryption are all unbuildable.

**Evidence.**
```sql
-- EnsureDevice INSERT writes no public_key column
-- grep for GenerateX25519/keyring/keychain in internal/ returns nothing
-- spec/09: device public key -> devices.public_key; device private identity -> local OS keychain only
```

**Recommendation.** On `init` (and lazily on first env command), generate an age X25519 identity, store the private identity string in the OS keychain via a small adapter (macOS `security`, Linux Secret Service), and persist only the public recipient in `devices.public_key`. The private key must never land in `state.db`, `config.yaml`, or logs. Add a `doctor` check that the local device has a public key and the private identity is retrievable.

**Actionable Steps.**
1. Add `filippo.io/age` and `internal/crypto/identity.go` wrapping `age.GenerateX25519Identity()`.
2. Add `internal/keychain` with a `Provider` interface; implement darwin via `security add/find-generic-password` and linux via `go-keyring` (Secret Service, keyctl fallback) — behind the Mac/Linux adapter.
3. In `EnsureDevice` (or `EnsureDeviceIdentity`), if `public_key` is empty, generate the identity, store `identity.String()` in the keychain, and `UPDATE devices SET public_key = identity.Recipient().String()`.
4. Persist a key fingerprint for out-of-band approval display; add a doctor check + a test asserting the keychain round-trips and the private string never appears in `state.db`/`config.yaml`.

**Example.**
```go
// internal/crypto/identity.go
import "filippo.io/age"
func NewDeviceIdentity() (priv string, recipient string, err error) {
    id, err := age.GenerateX25519Identity()
    if err != nil { return "", "", err }
    return id.String(), id.Recipient().String(), nil // "AGE-SECRET-KEY-1...", "age1..."
}
// macOS adapter: security add-generic-password -U -s devstrap -a device-identity:<dev_id> -w "AGE-SECRET-KEY-1..."
// Linux:        keyring.Set("devstrap", "device-identity:<dev_id>", priv)
// persist ONLY recipient in devices.public_key.
```

**References.** [age tests](https://github.com/FiloSottile/age/blob/main/age_test.go) · [filippo.io/age](https://pkg.go.dev/filippo.io/age) · [zalando/go-keyring](https://github.com/zalando/go-keyring/blob/master/README.md)

---

### [ENV-2 / SEC-3] Redaction is key-name-only and effectively dead; secret *values* leak via error output and git arg echoing
`high` · `effort: M` · `internal/logging/logging.go:43-68`, `internal/git/git.go:44`, `internal/cli/root.go:150`, `internal/sync/events.go:30-49`
*(Merged: SEC-3 folded in — the same dead-redaction/credential-leak issue from the security dimension.)*

**Problem.** `spec/09`/`spec/15` mandate that secrets never appear in logs and that redaction is a backstop. In practice the redaction never runs on the data that actually leaks: (1) there are ZERO structured-log call sites passing secret-bearing attributes, so the `ReplaceAttr`/`SecretString` machinery is unexercised; (2) all user-facing errors are emitted by `fmt.Fprintln(stderr, err)` in `root.go:150`, bypassing slog entirely; (3) git errors embed the full command line: `fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)`. A credential-bearing remote like `https://user:ghp_xxx@github.com/org/repo.git` is echoed verbatim into the error and printed to stderr, and may be stored in conflict/event payloads. `shouldRedact` matches only attribute KEY substrings, so a `SecretString` formatted into a string, a value under a benign key, or any field marshalled into `payload_json` bypasses redaction entirely.

**Evidence.**
```go
git.go:44   return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
root.go:150 _, _ = fmt.Fprintln(stderr, err)
logging.go  func shouldRedact(key string) bool { ... strings.Contains(key, needle) } // key-only
// events.go marshals ProjectPayload (incl. RemoteURL) straight into payload_json with no scrub
```

**Recommendation.** Make the secret type a true capability that renders `***` for `String`/`GoString`/`MarshalText`/`MarshalJSON`, reachable only via one audited `Reveal()` boundary. Add a value-level scrubber (a `strings.Replacer` over known secret values plus a regex pass for common token shapes) used on (a) the event payload writer and (b) subprocess stdout/stderr. Strip credentials from git remote URLs (`url.Parse`, drop userinfo) before they enter errors/logs/events, and route CLI error printing through the same sanitizer. Add the spec-mandated test: load a known secret, exercise log/event/db paths, assert the value is absent.

**Actionable Steps.**
1. Change `SecretString` to a struct whose `String`/`GoString`/`MarshalText`/`MarshalJSON` all return `[REDACTED]`; expose a single `Reveal()`.
2. Add `redactURL(s)` (drop userinfo) and an `internal/redact.Redactor` with `AddValue` + a `strings.Replacer`-backed `Scrub` and a regex set (`ghp_`, `github_pat_`, `xoxb-`, `AKIA`, `sk-`, `AGE-SECRET-KEY-`, JWT).
3. In `git.go Run`, pass args through `redactURL` before `Join`; wrap the `root.go` stderr print with the sanitizer.
4. Strip credentials from remotes in `sync/events.go` and `scan`/`git`; wrap subprocess output with the `Redactor` and mark the agent log stream "tainted" until scrubbed. Add the leak-absence test in `internal/logging` and `internal/sync`.

**Example.**
```go
type Secret struct{ v string }
func (s Secret) String() string                { return "[REDACTED]" }
func (s Secret) GoString() string              { return "[REDACTED]" }
func (s Secret) MarshalText() ([]byte, error)  { return []byte("[REDACTED]"), nil }
func (s Secret) Reveal() string                { return s.v } // single audited boundary

func redactURL(s string) string {
    if u, err := url.Parse(s); err == nil && u.User != nil { u.User = url.User("***"); return u.String() }
    return s
}
// git.go Run(): safeArgs[i] = redactURL(a); return fmt.Errorf("git %s: %s", strings.Join(safeArgs, " "), redactURL(msg))
```

**References.** [slog LogValuer](https://pkg.go.dev/log/slog#LogValuer) · [Redacting with slog](https://blog.arcjet.com/redacting-sensitive-data-from-logs-with-go-log-slog/) · [slog secret example](https://go.dev/src/log/slog/example_logvaluer_secret_test.go)

---

### [ENV-3] `.env` capture needs a defined, hardened, non-interpolating parser and an immediate-encrypt path
`medium` · `effort: M` · `spec/09:13-28,244-264`, `internal/scan/scan.go:103-104,174-186`

**Problem.** `scan.go` detects secret-looking files and warns, but the capture path (`devstrap env capture <proj> .env`) is undefined beyond "reads local .env once; parses variables." There is no specified dotenv grammar (quoting, multiline, `export` prefixes, comments, interpolation), no rule that parsed plaintext is encrypted in-memory and never written to a temp file, and no guard against capturing a `.env` containing shell interpolation or command substitution. Dotenv parsing is notoriously inconsistent across tools; owning a precise grammar now prevents silent value corruption and avoids accidentally executing `$(...)`/`${VAR}` during capture.

**Evidence.**
```go
// scan.go: if isSecretName(name, relSlash) { result.Warnings = append(..., "secret-looking file found: ...") } // detection only
// spec/09 capture "Behavior" does not name a parser or forbid interpolation/temp files
```

**Recommendation.** Define an explicit, non-interpolating `.env` grammar (`KEY=VALUE`, optional `export`, `#` comments, single/double quotes with documented escaping, NO substitution), implement it in `internal/env`, and have `env capture` parse directly into a `Secret`-typed map that is age-encrypted to approved recipients before anything touches disk. Refuse capture if a value looks like it needs interpolation unless `--literal`. Never write a plaintext temp; pipe ciphertext straight into the age blob store and record only `age_blob:<sha256>`.

**Actionable Steps.**
1. Write `internal/env/parse.go` implementing the documented grammar; unit-test tricky inputs (quotes, `=`, `#`, trailing spaces, CRLF).
2. Add `env capture` that parses into `map[string]Secret`, encrypts the bundle via age to all approved recipients, computes the ciphertext sha256, stores `<home>/blobs/<sha256>.age` (0600), and inserts `secret_bindings` with `encrypted_value_ref='age_blob:<sha256>'`.
3. On scan detection, offer the capture action and auto-append the file to `.gitignore`/`.devstrapignore`.
4. Use 0600 for generated files and 0700 for the blob dir; test that capture never writes plaintext to the home dir.

**Example.**
```go
func EncryptBundle(plaintext []byte, recipients []age.Recipient) ([]byte, string, error) {
    var buf bytes.Buffer
    w, err := age.Encrypt(&buf, recipients...) // one X25519 stanza per device
    if err != nil { return nil, "", err }
    if _, err := w.Write(plaintext); err != nil { return nil, "", err }
    if err := w.Close(); err != nil { return nil, "", err }
    sum := sha256.Sum256(buf.Bytes())
    return buf.Bytes(), "age_blob:" + hex.EncodeToString(sum[:]), nil
}
```

**References.** [1Password secret references](https://www.1password.dev/cli/secret-references) · [age tests](https://github.com/FiloSottile/age/blob/main/age_test.go)

---

### [ENV-4] Revocation/forward-secrecy claims overstate what age re-encryption delivers; spec needs explicit mandatory value rotation
`medium` · `effort: S` · `spec/09:159-187,276`, `spec/15:116`

**Problem.** *(verdict: revise — corrected text applied.)* `spec/09` frames device add/revoke/lost as triggering "re-encryption of affected bundles to the current approved-recipient set," and `spec/15:116` bounds a malicious device's exposure with the same step. With age (same envelope model as SOPS+age), re-encrypting only changes who can read *future* blobs; any blob the revoked device already received, or that persists in Hub/local history, remains decryptable by its key forever. The spec mentions "rotate env bundles" but never distinguishes envelope rewrap (change recipients) from value rotation (replace the actual secret values), never states revocation security depends on value rotation, and never requires pruning Hub-retained ciphertext. The `spec/15:116` residual-risk wording ("until revoked") understates this: historical/Hub ciphertext stays decryptable *after* revocation until values are rotated.

**Evidence.**
```text
spec/09: "Device add, revoke, lost, or rotate events trigger re-encryption ... to the current approved-recipient set."
SOPS+age: "Removing a recipient != revoking their access. ... rotate every secret they had access to."
```

**Recommendation.** Split the spec into two operations: (a) re-encrypt-to-new-recipient-set (envelope rewrap, governs future reads) and (b) value rotation (replace actual secret values), and state that revocation security depends on (b). Make `devices revoke` enqueue a rotation TODO per affected binding and surface unrotated secrets as a warning in `status`/`doctor`. Generate a fresh per-bundle data key on every re-encryption, and immediately delete superseded blobs locally + request Hub deletion to shrink the exposure window.

**Actionable Steps.**
1. Edit `spec/09`/`spec/15` to distinguish "rewrap recipients" from "rotate values"; state revocation is only complete after value rotation.
2. On revoke/lost: rewrap to the remaining set (new file key each), tombstone+delete old blobs locally and request Hub deletion.
3. Add a `needs_rotation` flag to `secret_bindings`; report the count in `status`/`doctor`.
4. Document that Hub blob history must be pruned; test that after revoke the new bundle has no stanza decryptable by the revoked identity and the old blob is gone.

**Example.**
```text
# spec/09 revocation, corrected wording
devstrap devices revoke dev_gmk_ubuntu
  1. rewrap every affected bundle to remaining approved recipients (new file key each)
  2. delete superseded age blobs locally + request Hub deletion (tombstone)
  3. mark every binding the device could read as needs_rotation
  4. WARNING: values synced before revocation are still known to the revoked
     device. Rotate them at the source to actually revoke access.
```

**References.** [GitOps secrets with SOPS+age](https://www.bigiron.cc/guides/gitops-secrets-the-sops-and-age-pattern) · [SOPS+age GitOps](https://www.systemshardening.com/articles/cicd/sops-age-gitops-secrets/)

---

### [ENV-5] Provider-reference mode should be the default for non-personal projects; lean on `op run`/`op inject`
`medium` · `effort: M` · `spec/09:30-56,86-129`, `internal/state/migrations/00001_initial.sql:96-109`

**Problem.** *(verdict: revise — corrected text applied.)* `spec/09`'s provider-priority list ranks "DevStrap encrypted personal store" #1, ahead of 1Password/Doppler/Infisical. This contradicts the spec's own persona guidance and policy examples, which already steer team/company projects to reference mode (company policy = `runtime_only`, provider: 1password, `write_file_default: never`). For non-personal projects, reference mode is strictly safer: DevStrap stores only `op://` references (already modeled as `secret_bindings.provider_ref`) and the provider CLI resolves values into a subprocess env DevStrap never persists. The runtime-injection algorithm re-implements what `op run`/`op inject` already do (no echoing, subprocess-scoped lifetime), so delegating reduces bespoke crypto/keychain code that must be correct.

**Evidence.**
```text
spec/09 provider priority: "1. DevStrap encrypted personal store; 2. 1Password CLI; ..."
Schema: secret_bindings has provider_ref with CHECK ((provider_ref IS NOT NULL) <> (encrypted_value_ref IS NOT NULL))
```

**Recommendation.** Reorder the provider-priority list so 1Password/Doppler/Infisical rank ahead of the encrypted store for team-strict projects; keep the encrypted store as the opt-in default only for the solo/homelab persona. Implement `devstrap run` for provider profiles by shelling out to `op run` (passing refs via the child env or an `--env-file` of refs — NOT repeated `--env NAME=ref` flags, which `op run` does not support). For file hydration, use `op inject -i template -o .env.local` (0600, gitignored verified). Build the child env with what `op` needs to authenticate plus the allowlisted profile names, dangerous names stripped last (an empty base would break `op`'s own auth/resolution).

**Actionable Steps.**
1. Reorder/annotate the provider priority in `spec/09` (team-strict → providers first; personal → encrypted store).
2. Implement provider resolution that detects `provider=1password` and builds `op run --env-file <refs> -- <cmd>`.
3. Build the child env as `op` auth vars + allowlisted names, stripping dangerous names last (shared with SEC-2).
4. For `hydrate --write`, use `op inject --file-mode 0600` after verifying the target is gitignored; add doctor checks that `op`/`doppler`/`infisical` are installed and authenticated.

**Example.**
```go
refFile := writeRefEnvFile(profile.Bindings) // NAME=op://Engineering/OpenAI/api_key per line
args := []string{"run", "--env-file", refFile, "--"}
args = append(args, userCmd...)
c := exec.CommandContext(ctx, "op", args...)
// op needs OP_SERVICE_ACCOUNT_TOKEN (or OP_CONNECT_*), HOME, plus allowlisted profile names;
// dangerous names (LD_PRELOAD, DYLD_INSERT_LIBRARIES, BASH_ENV, NODE_OPTIONS, PYTHONPATH, GIT_SSH_COMMAND) stripped last.
c.Env = buildOpAndAllowlistedEnv(profile)
// op resolves refs and scopes secret VALUES to this subprocess only; DevStrap never stores them.
```

**References.** [op secrets in env vars](https://www.1password.dev/cli/secrets-environment-variables) · [op inject](https://www.1password.dev/cli/reference/commands/inject)

---

## Testing & CI

### [TEST-1] `internal/scan` safety core has 0% direct test coverage
`high` · `effort: M` · `internal/scan/scan.go` (no `scan_test.go`)

**Problem.** *(verdict: revise — corrected text applied.)* `internal/scan` reports 0.0% coverage. It implements three stated safety invariants: never store/log plaintext secrets (`isSecretName`), ignore dependency folders (`shouldPruneDir`), and detect path/symlink escapes. The only thing touching it is one CLI test asserting a single repo is found. There is no test that `.env.example`/`.env.template` are NOT flagged while `.env.production` IS, that `node_modules`/`.venv`/`target` are pruned, that an escaping symlink produces the warning, or that duplicate remotes yield a deterministic `RecommendedPath`. A regression in `isSecretName` could silently start or stop flagging real secrets with no failing test.

**Evidence.**
```go
// scan.go: if strings.HasPrefix(name, ".env.") && name != ".env.example" && name != ".env.template" && name != ".env.schema" { return true }
//          case ".git", "node_modules", ".venv", ...: return true
// none of these branches are asserted by any test
```

**Recommendation.** Add a table-driven `internal/scan/scan_test.go` (package `scan`). **Correction to the original injection plan:** `dsgit.Runner` is a concrete struct and `dsgit.IsRepo` is a package-level function — you cannot "inject a fake Runner." Instead: (1) call the unexported `isSecretName`/`shouldPruneDir` directly (no git needed) for fast classification coverage; (2) for Walk-level coverage (symlink escape, case-only conflicts, duplicate remotes), build a fixture tree under `t.TempDir()` with real `git init` repos sharing one remote, then call `scan.Walk` and assert on `Result.Findings/Warnings/Duplicates`. Assert sorted ordering so the test is not flaky.

**Actionable Steps.**
1. Add `TestIsSecretName` and `TestShouldPruneDir` calling the unexported functions directly.
2. Add a Walk-level test materializing `node_modules`/`.venv`/`target`, an escaping symlink, a `.env`, and two real `git init` repos sharing one remote.
3. Assert `Result.Findings`, `Result.Warnings`, `Result.Duplicates` (and sorted determinism).
4. Optionally refactor the `dsgit.IsRepo` call site behind a small interface to fully avoid system git (larger, optional).

**Example.**
```go
func TestIsSecretName(t *testing.T) {
    cases := []struct{ name, rel string; want bool }{
        {".env", "work/api/.env", true},
        {".env.production", "work/api/.env.production", true},
        {".env.example", "work/api/.env.example", false},
        {"key.pem", "work/api/key.pem", true},
        {"README.md", "work/api/README.md", false},
        {"credentials", "work/api/.aws/credentials", true},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            if got := isSecretName(c.name, c.rel); got != c.want {
                t.Fatalf("isSecretName(%q,%q)=%v want %v", c.name, c.rel, got, c.want)
            }
        })
    }
}
```

**References.** [Table-driven tests](https://go.dev/wiki/TableDrivenTests) · `spec/16_TEST_PLAN.md` (Ignore compiler / Redaction sections)

---

### [TEST-2] Spec-mandated Unicode path normalization is claimed but not implemented or tested
`high` · `effort: M` · `internal/pathkey/pathkey.go:46`, `internal/pathkey/pathkey_test.go`

**Problem.** `spec/16` lists "Unicode normalization" as a required path-normalization test, and the namespace model depends on `path_key` being a stable cross-device collision key. But `pathkey.Clean` derives `Key` via `strings.ToLower(clean)` with no NFC/NFD normalization. On macOS the filesystem normalizes to NFD, so a folder `café` typed on Linux (NFC, U+00E9) and on a Mac (NFD, `e`+U+0301) produce DIFFERENT `path_key` values, silently breaking duplicate detection and cross-device sync matching — exactly the collision `path_key` exists to prevent. No test covers this; `pathkey` is at 39% coverage and `DetectCaseConflicts`/`FromRoot`/`CheckSymlinkWithinRoot` have no direct tests.

**Evidence.**
```go
// pathkey.go:46: return Path{Display: clean, Key: strings.ToLower(clean)}, nil  // no norm.NFC
// spec/16:60 lists "Unicode normalization" as required
```

**Recommendation.** Normalize the key with NFC before lowercasing (`norm.NFC.String`) so NFC/NFD spellings collapse to one key; add a table-driven test asserting NFC and NFD inputs yield the same `Key`. Add direct tests for `DetectCaseConflicts` and `CheckSymlinkWithinRoot`.

**Actionable Steps.**
1. Add `golang.org/x/text/unicode/norm`; change key derivation to `strings.ToLower(norm.NFC.String(clean))`.
2. Add a test asserting `Clean(NFC "café")` and `Clean(NFD "café")` produce identical `.Key`.
3. Add `TestDetectCaseConflicts` (`work/API` vs `work/api`).
4. Add `TestCheckSymlinkWithinRoot` using `os.Symlink` in a temp dir covering within-root and escape; document NFC as canonical in `spec/16`.

**Example.**
```go
import "golang.org/x/text/unicode/norm"
// in Clean:
key := strings.ToLower(norm.NFC.String(clean))
return Path{Display: clean, Key: key}, nil

func TestCleanUnicodeNormalization(t *testing.T) {
    a, _ := Clean("work/café")   // é precomposed (NFC)
    b, _ := Clean("work/café")   // e + combining acute (NFD)
    if a.Key != b.Key { t.Fatalf("NFC key %q != NFD key %q", a.Key, b.Key) }
}
```

**References.** `spec/16_TEST_PLAN.md` · [x/text/unicode/norm](https://pkg.go.dev/golang.org/x/text/unicode/norm) · `spec/07` (path_key as cross-device key)

---

### [TEST-3] The "most important test" asserts on stdout text, not the checked-out commit or recorded base SHA
`medium` · `effort: S` · `internal/cli/root_test.go:233-239`

**Problem.** `spec/16` calls the fresh-upstream worktree test "the most important test"; the invariant is "agents branch from the fetched remote default ref, not the local default branch." The current test only asserts `strings.Contains(stdout, latest)`. It never (a) asserts the worktree's actual HEAD via `git rev-parse HEAD`, (b) asserts the persisted base SHA, nor (c) deliberately leaves the LOCAL clone's default branch stale and proves the worktree did not pick up the local ref. A buggy implementation that checked out the local default could still print the right SHA and pass.

**Evidence.**
```go
// root_test.go:237: if !strings.Contains(stdout, latest) { t.Fatalf(...) }
// worktree path never stat'd or rev-parsed; worktrees DB row never read
```

**Recommendation.** Prove the worktree filesystem HEAD equals the advanced remote SHA and that the stale local default was NOT used. Capture the worktree path, `git rev-parse HEAD` in it, and assert the persisted base SHA.

**Actionable Steps.**
1. After hydrate, capture the local clone's default-branch SHA and assert it differs from `latest` (local is stale).
2. After `worktree new --fresh-upstream`, resolve the worktree dir (parse output or query `ListWorktrees`).
3. `git rev-parse HEAD` in the worktree; assert it equals `latest`; assert the worktrees row base SHA == `latest`.
4. Add a negative variant: without `--fresh-upstream`, assert base is the stale local ref.

**Example.**
```go
wtHead := strings.TrimSpace(runGitOutput(t, worktreeDir, "rev-parse", "HEAD"))
if wtHead != latest { t.Fatalf("worktree HEAD = %s, want fresh remote SHA %s", wtHead, latest) }
localHead := strings.TrimSpace(runGitOutput(t, localClone, "rev-parse", "HEAD"))
if localHead == latest { t.Fatal("local clone was not stale; test no longer proves fresh-upstream behavior") }
```

**References.** `spec/16_TEST_PLAN.md:170-172` ("the most important test") · `spec/16:39` (invariant 1)

---

### [TEST-4] CI has no linter (golangci-lint/gosec) — permission, errcheck, and command-exec issues go uncaught
`medium` · `effort: S` · `.github/workflows/ci.yml`

**Problem.** The workflow runs gofmt, `go vet`, and govulncheck, but no static linter. For a tool whose threat model centers on file permissions (0600/0700), shelling out to git, and never leaking secrets, gosec rules G104 (unchecked errors), G204 (command-exec audit), G301/G302/G306 (file/dir perms), and G304 (tainted file path) are directly relevant. `errcheck` would catch ignored returns (e.g. `details, _ := json.Marshal(...)`). `go vet` alone does not cover these.

**Evidence.**
```text
ci.yml steps: Check formatting / Vacuous guard / Vet / Build / Test / Vulnerability check / Module hygiene — no golangci-lint or gosec
```

**Recommendation.** Add a separate `golangci-lint` job (its own parallel job) using the official action, with a checked-in `.golangci.yml` enabling errcheck, gosec, staticcheck, govet, ineffassign, unconvert.

**Actionable Steps.**
1. Add `.golangci.yml` enabling the linters and configuring gosec includes (G104/G204/G301/G302/G304/G306).
2. Add a `lint` job using `golangci/golangci-lint-action` pinned by SHA (matching the repo's pinning convention).
3. Run locally once and fix or `//nolint`-annotate findings before making it required.
4. Document the lint command in `CONTRIBUTING.md` and `AGENTS.md` alongside gofmt/`go test -race`.

**Example.**
```yaml
  lint:
    name: Lint
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with: { go-version-file: go.mod }
      - uses: golangci/golangci-lint-action@v9
        with: { version: v2.12 }
# .golangci.yml
version: "2"
linters:
  enable: [errcheck, gosec, staticcheck, govet, ineffassign, unconvert]
  settings:
    gosec: { includes: [G104, G204, G301, G302, G304, G306] }
```

**References.** [golangci-lint-action](https://github.com/golangci/golangci-lint-action/blob/main/README.md) · [golangci-lint config](https://golangci-lint.run/docs/linters/configuration/) · `spec/15`

---

### [TEST-5] JSON CLI output is a machine contract but is verified only by fragile substring matches
`medium` · `effort: M` · `internal/cli/root_test.go:98,132,150`

**Problem.** `status --json` and `scan --dry-run --json` emit JSON that other tools and future sync devices parse, so the schema is a contract. Tests verify it with brittle substring checks like `strings.Contains(stdout, "\"project_count\": 1")`. These depend on exact indentation, do not validate the full shape, miss field renames/removals elsewhere, and break noisily on cosmetic changes while passing on real regressions in untested fields.

**Evidence.**
```go
// root_test.go:98  if !strings.Contains(stdout, "\"workspace_name\": \"personal\"")
// root_test.go:150 if !strings.Contains(stdout, "\"project_count\": 1") — no unmarshal, no full-doc comparison
```

**Recommendation.** Unmarshal JSON output into the response struct (or a map) and assert on fields; add golden-file tests for canonical documents (with a `-update` flag), normalizing volatile fields (`device_id`, abs paths) so the full schema is pinned and intentional changes are reviewed as golden diffs.

**Actionable Steps.**
1. Change assertions to `json.Unmarshal` into the response type and compare fields.
2. Add `testdata/*.golden` for representative status/scan JSON, normalizing volatile fields.
3. Add a `-update` flag to regenerate goldens.
4. Redact the `dev_<uuidv7>` id and temp paths before comparing so goldens are stable across machines.

**Example.**
```go
func TestStatusJSONContract(t *testing.T) {
    stdout, _, _ := executeForTest("--home", home, "status", "--json")
    var got struct {
        WorkspaceName string `json:"workspace_name"`
        ProjectCount  int    `json:"project_count"`
        RootPath      string `json:"root_path"`
    }
    if err := json.Unmarshal([]byte(stdout), &got); err != nil {
        t.Fatalf("status --json is not valid JSON: %v\n%s", err, stdout)
    }
    if got.WorkspaceName != "personal" || got.ProjectCount != 0 { t.Fatalf("unexpected: %+v", got) }
}
```

**References.** [testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript) · `spec/16_TEST_PLAN.md:130`

---

### [TEST-6] No end-to-end CLI tests through the real binary; `cmd/devstrap` and ExitCode paths are 0% covered
`medium` · `effort: L` · `cmd/devstrap/main.go` (0% coverage); no testscript harness

**Problem.** The product is defined as a CLI loop, yet tests invoke cobra commands in-process via `NewRootCommand` and never exercise `main()` argv parsing, `signal.NotifyContext` wiring, `cli.Execute`, or `os.Exit(cli.ExitCode(err))`. `cmd/devstrap` shows 0.0% coverage. The standard answer is `rogpeppe/go-internal/testscript`, used by the go tool itself, which runs the registered entrypoint as a subprocess so argv/stdout/stderr/exit codes go through the real path and it integrates with `-coverpkg`.

**Evidence.**
```text
Coverage: cmd/devstrap coverage: 0.0% of statements
root_test.go always calls NewRootCommand(...).Execute() in-process, bypassing main() and the exit-code path
```

**Recommendation.** Add a testscript-based `script_test.go` registering the `devstrap` entrypoint and driving the full init→scan→add→hydrate→status loop from txtar fixtures, asserting stdout/stderr regexes, exit codes, and on-disk state, with `DEVSTRAP_HOME` pointed at `$WORK`.

**Actionable Steps.**
1. Add `github.com/rogpeppe/go-internal` to `go.mod`.
2. Add `cmd/devstrap/script_test.go` with `TestMain` calling `testscript.RunMain` registering `devstrap`.
3. Create `testdata/script/*.txtar` exercising the core loop with `DEVSTRAP_HOME=$WORK/home` and a local bare git remote built in a Setup func.
4. Assert exit codes (e.g. `! exec devstrap status` before init; stderr "workspace is not initialized"); wire coverage with `-coverpkg=./...`.

**Example.**
```go
func TestMain(m *testing.M) {
    os.Exit(testscript.RunMain(m, map[string]func() int{
        "devstrap": func() int { return cli.ExitCode(cli.Execute(context.Background())) },
    }))
}
func TestScripts(t *testing.T) { testscript.Run(t, testscript.Params{Dir: "testdata/script"}) }
// testdata/script/init_status.txtar
// env DEVSTRAP_HOME=$WORK/home
// ! exec devstrap status
// stderr 'workspace is not initialized'
// exec devstrap init $WORK/Code
// exec devstrap status --json
// stdout '"project_count": 0'
```

**References.** [How go tests go test](https://atlasgo.io/blog/2024/09/09/how-go-tests-go-test) · [testscript CLI](https://rednafi.com/go/testscript-cli/) · [go-internal](https://github.com/rogpeppe/go-internal)

---

### [TEST-7] HLC bit-shifted clock has untested overflow/precision edges; only a single happy-path case exists
`low` · `effort: S` · `internal/sync/hlc.go:38`, `internal/sync/hlc_test.go`

**Problem.** `physical()` computes `now.UnixMilli() << 16`, a deliberate encoding ordering correctness depends on, but it is essentially untested: the only HLC test sends twice at a fixed time and receives once. There is no test for (a) the logical counter wrapping past 65535 in one ms, (b) the clock going backwards, (c) a far-future remote HLC, or (d) the int64 overflow of the shift. Because `ApplyEvents` and `FileHub` sort by `(hlc, device_id)`, an ordering bug here corrupts convergence silently. `sync` is at 40% coverage. *(Pairs with SYNC-1's algorithm fix.)*

**Evidence.**
```go
// hlc.go:38: return now.UnixMilli() << 16
// hlc_test.go:12-24 is the only HLC test — no backward-clock, counter-saturation, or large-remote case
```

**Recommendation.** Add a table-driven HLC test injecting a controllable `Now` covering backward clock movement (monotonicity preserved), many sends within one ms (counter increments, then next tick dominates), and `Receive` with a far-ahead remote. Assert strict monotonicity in every case.

**Actionable Steps.**
1. Build an HLC with a settable fake clock captured by the `Now` closure.
2. Frozen clock: two `Send` calls assert `second == first+1`.
3. Advance 1ms: next `Send` jumps to the new physical-derived value.
4. Move backwards: `Send` still strictly increases; `Receive(veryLargeRemote)` then `Send` stays monotonic.

**Example.**
```go
func TestHLCMonotonicUnderBackwardClock(t *testing.T) {
    now := time.UnixMilli(1000)
    clock := HLC{Now: func() time.Time { return now }}
    a := clock.Send()
    now = time.UnixMilli(500) // clock jumps backward
    b := clock.Send()
    if b <= a { t.Fatalf("HLC went backward: a=%d b=%d", a, b) }
}
```

**References.** [Table-driven tests](https://go.dev/wiki/TableDrivenTests) · `spec/07` (HLC ordering / deterministic replay)

---

## CLI / DX / Operations

No finding is *primarily* a CLI/DX/operations defect — the relevant operational concerns surfaced under other dimensions, where their root cause lives. They are indexed here so the team can treat "operability" as a cross-cutting checklist:

| Operational concern | Where addressed | Why it matters for DX/ops |
|---|---|---|
| `devstrap open` hangs / Ctrl-C kills the editor | [GO-5](#go-5-open-ties-the-gui-editors-lifetime-to-the-command-context-and-blocks-on-run) | Fire-and-forget launch is the expected UX |
| Hung clone/fetch with no timeout or prompt-disable | [GO-1](#go-1-git-runner-never-disables-prompts-and-sets-no-network-timeout) | Unattended/agent runs must never block forever |
| Permanent self-DoS from a stale repo lock; no recovery command | [SEC-5 / GO-2](#sec-5--go-2-repo-lock-is-a-non-atomic-stale-lockfile-no-pidowner-leaks-on-crash-hydrate-clone-is-unprotected-and-has-a-toctou) | Add `worktree unlock` / `doctor` clear-dead-lock escape hatch |
| `worktree remove` of a missing tree errors out; no `--force`; no prune | [GIT-3](#git-3-dirty-worktree-guard-is-bypassable-missing-path-errors-out-instead-of-being-handled-remove-never-prunes) | Users must be able to clean stale state |
| CLI syntax in docs the binary rejects (`worktree list <path>`) | [SPEC-1](#spec-1-spec-13-documents-cli-command-syntax-the-code-does-not-accept) | Copy-pasting documented commands must work |
| `status --json` contract drift | [SPEC-2](#spec-2-phase-0-status-json-example-in-spec13-is-stale-omits-device_id-and-projects) / [TEST-5](#test-5-json-cli-output-is-a-machine-contract-but-is-verified-only-by-fragile-substring-matches) | Downstream tools/devices parse this output |
| No end-to-end binary exit-code coverage | [TEST-6](#test-6-no-end-to-end-cli-tests-through-the-real-binary-cmddevstrap-and-exitcode-paths-are-0-covered) | The exit-code contract is part of the CLI surface |
| Daemon/service install UX (launchd/systemd, UDS API) | [ARCH-1](#arch-1-promote-a-thin-agent-runner-ahead-of-the-daemon-linux-and-hub) + Appendix §1 | Sequenced after the proven single-machine loop |

> Recommended new operability primitives to add when their parent fix lands: `devstrap worktree unlock <path>`, a `devstrap doctor` check that reports/clears dead locks and verifies FK enforcement (DATA-3), and a `devstrap worktree status <id>` that reports stale-base drift (ARCH-3).

---

## Spec Quality & Consistency

### [SPEC-1] Spec 13 documents CLI command syntax the code does not accept
`high` · `effort: S` · `spec/13:206,192-230`, `internal/cli/worktree.go:120`

**Problem.** `spec/13` shows `devstrap worktree list work/acme/api`, but the subcommand is `Use: "list"` with no `Args` constraint and no positional-path handling, so the path argument is silently ignored. More broadly, the env, agent, and devices sections present full command syntaxes with no inline "planned/not implemented" marker, even though 13 itself earlier states (line 41) these are "Planned." `worktree`/`open`/`hydrate` get accurate per-section notes, but env/agent/devices read as if shipped — an agent following spec/13 as source of truth will write tests against commands that error with "unknown command."

**Evidence.**
```text
spec/13: devstrap worktree list work/acme/api
worktree.go:120: Use: "list" (no <path>, no Args:)
spec/13:41: "Planned: sync command, env, agent, devices, daemon, hub, export"
```

**Recommendation.** Make the implemented-vs-planned status local to every command block, and fix the `worktree list` signature (either accept and use a path filter, or drop the arg from the spec). Prefer the convention already used in the open/hydrate sections.

**Actionable Steps.**
1. Either add an optional `[path]` arg to `newWorktreeListCommand` and filter by it, OR change spec/13:206 to `devstrap worktree list`.
2. Add a `> Status: not implemented (Milestone N)` banner under each of Env, Agent, Device headings.
3. Add the same banner to the `sync` section, also unimplemented but reading as shipped.
4. Grep every spec for command invocations and reconcile against `grep -rE 'Use:' internal/cli` as a pre-handoff check.

**Example.**
```md
### worktree
> Status: implemented. `list` takes no path argument; it lists all worktrees.

### env
> Status: NOT IMPLEMENTED (Milestone 4). Syntax below is a design target.
```

**References.** [Living docs / spec drift](https://levelup.gitconnected.com/living-documentation-in-sdd-spec-drift-6-traps-and-the-sync-owner-gate-mechanism-74b706c9db95) · [What makes a good spec](https://addyosmani.com/blog/good-spec/)

---

### [SPEC-2] Phase-0 status JSON example in spec/13 is stale: omits `device_id` and `projects[]`
`medium` · `effort: S` · `spec/13:299-307`, `internal/state/store.go:30-73`

**Problem.** *(verdict: revise — corrected text applied.)* `spec/13` documents the status response as three fields (`workspace_name`, `root_path`, `project_count`). The actual `Summary` struct serializes more: `device_id` (omitempty) and `projects` (a `[]ProjectStatus` with id/path/path_key/type/materialization/dirty fields). An integrator coding against the three-field shape is blindsided. The spec even contradicts itself: 13:115 says status now shows "local device ID, and adopted project rows," yet the example omits both. (Attribution correction: the "JSON output for automation" contract language lives at spec/13:9, not spec/00.)

**Evidence.**
```text
spec/13: { "workspace_name": ..., "root_path": ..., "project_count": 0 }
store.go:31-35: WorkspaceName ... DeviceID `json:"device_id,omitempty"` ... Projects []ProjectStatus `json:"projects,omitempty"`
```

**Recommendation.** Regenerate the documented JSON from the real struct (or derive it from a golden-file test) so it includes `device_id` and a representative `projects[]` element matching the always-present NamespaceEntry fields plus the omitempty git/local fields; keep the "future project-level response" example clearly separate.

**Actionable Steps.**
1. Run `devstrap init` + `scan --adopt` against a fixture; capture real `status --json`.
2. Replace spec/13:299-307 with that output, including `device_id` and one `projects[]` entry.
3. Add a golden-file test in `internal/cli/status_test.go` that fails when the shape drifts (ties to TEST-5).
4. Cross-check spec/13:115 prose against the regenerated example.

**Example.**
```json
{
  "workspace_name": "artem-main",
  "root_path": "/Users/artem/Code",
  "project_count": 1,
  "device_id": "dev_01jz...",
  "projects": [
    {
      "id": "ns_01jz...", "path": "work/acme/api", "path_key": "work/acme/api",
      "type": "git_repo", "materialization_policy": "on_demand", "status": "active",
      "remote_url": "git@github.com:acme/api.git", "default_branch": "main",
      "materialization_state": "skeleton", "dirty_state": "unknown"
    }
  ]
}
```

**References.** [Living architecture docs](https://www.archyl.com/blog/living-architecture-documentation) · [anchored-dev](https://anchored-dev.org/)

---

### [SPEC-4] Branch-workflow guidance is internally contradictory during the "main rename"
`medium` · `effort: M` · `AGENTS.md:6,8`, `README.md:143-159`, `spec/00:16`

**Problem.** *(verdict: revise — corrected text applied.)* The repo is mid-"main rename" and the workflow docs disagree. AGENTS.md:8 and README.md:145 mandate a `dev`→`main` model, while README.md:147-159 frames `main` as trunk and bans "the legacy default branch name" yet the next snippet (154) tells contributors to run `git branch -m <old-default-branch> main`, a self-referential placeholder. The remote has BOTH `origin/dev` and `origin/main` (`origin/HEAD -> origin/main`), so the ambiguity is live. AGENTS.md:6 forbids branching agent work "from local `main`" but never states the correct agent base under the dev model. (Correction: spec/00:16 does NOT mandate dev→main — it states only the worktree/agent-base invariant; drop it from the "mandate dev→main" list.)

**Evidence.**
```text
AGENTS.md:8 "Use feature branches into dev; merge dev into main"
README.md:149 "The legacy default branch name should not be used in code, docs..."
README.md:154 git branch -m <old-default-branch> main
Remote: origin/dev, origin/main, origin/HEAD -> origin/main
```

**Recommendation.** Pick ONE integration model and state it canonically in AGENTS.md (including the explicit agent base: branch from fetched `origin/<default>`, never local); have README and spec/00 link to it. Replace the `<old-default-branch>` placeholder with the concrete prior name or delete the rename section since `origin/HEAD` already points at main. (Correction: do NOT "update CI branch filters" — `ci.yml` runs on `branches: ["**"]` and unfiltered `pull_request`, so there are no dev/main filters to change.)

**Actionable Steps.**
1. Decide whether `dev` remains the integration branch given `origin/HEAD -> main`.
2. State the chosen model once in AGENTS.md, including the concrete agent base.
3. Make README.md:143-145 and spec/00:16 reference AGENTS.md instead of restating.
4. Replace README.md:154's placeholder with the actual prior name, or delete the rename section; if `dev` is retired, delete `origin/dev`.

**Example.**
```md
## Branching (canonical — AGENTS.md)
- Trunk is `main`. Integration branch is `dev`.
- Branch features from `dev`; open PRs into `dev`; merge `dev` -> `main` after green CI.
- Agents/worktrees base from the **fetched** `origin/<default_branch>`, never any local branch.
```

**References.** [Renaming a branch (GitHub)](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-branches-in-your-repository/renaming-a-branch) · [ADR](https://martinfowler.com/bliki/ArchitectureDecisionRecord.html)

---

### [SPEC-5] No per-document provenance or drift-detection; the "review every spec" mandate is unenforceable
`medium` · `effort: M` · `spec/00:103`, `spec/13:37`, `spec/17`, `AGENTS.md:10`

**Problem.** AGENTS.md:10 requires reviewing every `spec/` file after the last code change. The only enforcement is a manually re-typed date `2026-06-24`, identical across 00/03/13/17/18. There is no frontmatter, no "maps to code at <path>" anchor, and no CI check that fails when `internal/cli` changes without a matching spec touch. The "Current repository state" list in spec/00 is the most drift-prone artifact (it enumerates exact command names) yet relies on a human remembering to update prose. For an agent-consumed spec corpus, an unenforced "keep specs current" rule is the highest-leverage maintainability gap.

**Evidence.**
```text
spec/00:103 "Last validated: 2026-06-24"
AGENTS.md:10 "review every file in spec/ ... so the specs remain accurate"
Identical date string in 00/03/13/17/18.
```

**Recommendation.** Add a lightweight, CI-enforced drift gate so spec staleness becomes loud, and replace the global hand-typed date with per-file frontmatter a script can validate. (Correction: CODEOWNERS already assigns per-file owners for spec/09 and spec/15 — extend this pattern; and the CI gate needs `fetch-depth: 0` to resolve `origin/main`.)

**Actionable Steps.**
1. Add YAML frontmatter (`last_reviewed`, `tracks_code:` globs) to each spec file.
2. Add a CI job failing a PR when `internal/cli/` or `internal/state/` change but no `spec/` file changes (sync-owner-gate).
3. Convert spec/00's "Current repository state" command list into a generated artifact (`make spec-commands` diffing `devstrap --help` against the spec list in CI).
4. Extend CODEOWNERS per-spec-file ownership beyond 09/15.

**Example.**
```yaml
# top of each spec/*.md
---
last_reviewed: 2026-06-24
tracks_code: [internal/cli/**, cmd/devstrap/**]
---
# .github/workflows/ci.yml (drift gate)
- uses: actions/checkout@v5
  with: { fetch-depth: 0 }   # required so origin/main resolves
- name: spec-drift
  run: |
    git fetch origin main
    if git diff --name-only origin/main... | grep -qE '^internal/(cli|state)/'; then
      git diff --name-only origin/main... | grep -qE '^spec/' \
        || { echo 'internal/cli or internal/state changed but no spec/ update'; exit 1; }
    fi
```

**References.** [anchored-dev](https://anchored-dev.org/) · [Living docs / spec drift](https://levelup.gitconnected.com/living-documentation-in-sdd-spec-drift-6-traps-and-the-sync-owner-gate-mechanism-74b706c9db95) · [Living architecture docs](https://www.archyl.com/blog/living-architecture-documentation)

---

### [SPEC-6] Work-log entries are out of order and follow-ups already describe completed work
`medium` · `effort: S` · `spec/18:24-83`, `AGENTS.md:9`

**Problem.** *(verdict: revise — corrected text applied.)* AGENTS.md:9 makes the work log the authoritative end-of-cycle handoff. Two defects undermine it: (a) ordering is unspecified and inconsistent — the format section defines no ordering rule, and the entry that created the log sits in the middle rather than top or bottom; (b) a stale follow-up — the top entry's follow-up lists "scanner/adoption workflow and real generated device IDs" as remaining, but the bottom entry already reports both as completed. A future agent reading top-down gets a contradictory picture. (Correction: the earlier claim that the log lags the working tree / is missing an entry for the current cycle is incorrect — spec/18 is itself untracked and its three entries already cover the uncommitted changes.)

**Evidence.**
```text
spec/18:24 "Audit hardening and spec refresh" ... :43 follow-up "scanner/adoption workflow and real generated device IDs"
vs spec/18:65 "generated dev_<uuidv7> IDs ... Implemented devstrap scan"
```

**Recommendation.** Define and enforce newest-first ordering in the work-log format, reconcile the stale follow-ups against completed milestones, and (optionally) add a CI check that a PR touching code/specs also updates a work-log entry.

**Actionable Steps.**
1. Add an explicit "entries are newest-first" rule to spec/18:11-22 and reorder the three existing entries.
2. Remove/correct the top entry's follow-ups already shown completed (scanner/adoption, device IDs).
3. Optionally add a CI check mirroring SPEC-5's gate.
4. Drop the "add a missing entry for the current cycle" step — the cycle is already documented.

**Example.**
```md
## Entry Format
Entries are newest-first. Each code-modifying cycle prepends ONE dated entry at the top.

## YYYY-MM-DD — <short title>
Changed: ...
Validated: gofmt; go vet; go test -race ./...
Follow-ups: <reconciled against later/earlier entries, or "None">
```

**References.** [anchored-dev](https://anchored-dev.org/) · [ADR](https://martinfowler.com/bliki/ArchitectureDecisionRecord.html)

---

### [SPEC-3] Product name is ambiguous (DevStrap vs Workspace Passport) with no enforced canonical decision
`low` · `effort: S` · `spec/02:5-17`, `spec/00:1`, `README.md:1`

**Problem.** `spec/02` lists "DevStrap", "Workspace Passport", and "StrapFS" as working names and "recommends" DevStrap=product / Workspace Passport=core concept / StrapFS=future VFS, but never elevates this to a binding decision. `spec/00` titles itself "DevStrap / Workspace Passport" (treating them as interchangeable), while the module/binary/README use only "DevStrap" and "Workspace Passport" appears nowhere in code. Two competing brand strings with no single source of truth — for an agent-driven project, agents will inconsistently pick one or the other.

**Evidence.**
```text
spec/02:5-9 "Working names: DevStrap / Workspace Passport / StrapFS"
spec/00:1 "# DevStrap / Workspace Passport — Start Here"
README.md:1 "# DevStrap"
```

**Recommendation.** Promote the naming choice to a recorded ADR and make every other doc reference it. Drop "Workspace Passport" from the spec/00 title or relabel it explicitly as the "core concept" tagline so the two strings stop reading as synonyms.

**Actionable Steps.**
1. Add `spec/adr/0001-product-naming.md` (product = DevStrap, concept = Workspace Passport, future VFS = StrapFS; status accepted).
2. Change spec/02:1-17 to reference the ADR.
3. Change spec/00's H1 to "# DevStrap — Start Here"; introduce "Workspace Passport" only as the concept tagline.
4. Add a CI grep flagging "Workspace Passport" used as a product string outside the ADR.

**Example.**
```md
# spec/adr/0001-product-naming.md
---
status: accepted
date: 2026-06-24
---
Decision: Product = **DevStrap**; namespace concept = **Workspace Passport**;
future virtual filesystem = **StrapFS**. Code, binary, and user-facing strings use only "DevStrap".
```

**References.** [ADR (Fowler)](https://martinfowler.com/bliki/ArchitectureDecisionRecord.html) · [MADR](https://adr.github.io/madr/)

---

## Phased Roadmap

Mapping the recommendations onto DevStrap's MVP phases. **Bold P0** items should land before anything else; "harden now" items are cheap insurance that becomes expensive after the relevant subsystem grows.

### Phase 0 — Local CLI proof (current)
The single-machine loop must be correct, safe, and well-tested before the daemon exists. Do these now.

| Do now (P0/P1) | Harden now (cheap insurance) | Test/Spec hygiene |
|---|---|---|
| **SEC-1** git RCE (`--` + protocol allowlist) | SEC-2 sanitized git env · GO-1 timeouts/prompts | TEST-1 scan coverage |
| **ARCH-3** stale-base re-check (the differentiator) | SEC-5/GO-2 advisory lock + hydrate lock | TEST-2 Unicode path key |
| **GIT-2** default-branch resolution | GO-7 atomic temp-clone-rename | TEST-3 worktree HEAD assertion |
| ENV-2/SEC-3 wire redaction + URL scrub | GIT-4/GO-8 single Store per command | TEST-4 golangci-lint/gosec job |
| GIT-1 honor `lfs_policy` | GIT-3 worktree remove/prune/force · GIT-6 typed git errors | TEST-5 JSON contract · TEST-6 testscript e2e |
| | GO-3/GO-5/GO-6 (errors, open, logger) · GIT-5 branch suffix | SPEC-1/2/3/4/5/6 spec drift fixes |
| **+ M3.5 thin agent runner (ARCH-1):** worktree → `agent run` → capture diff/logs — proves the loop with zero daemon | DATA-3 assert FK at open | DATA-1 partial index |

### Phase 1 — Mac daemon
Gate this phase behind a written "do we still need it?" review (ARCH-1) using sleep/wake + indexer-hydration-storm tests as entry criteria.

- **ARCH-5** introduce `internal/platform` adapter interfaces *before* writing FSEvents/launchd code (Linux is a peer from day one).
- Daemon entrypoint via `signal.NotifyContext` + `errgroup`; thread `ctx` through every git/SQLite call (Appendix §1).
- Watcher as advisory-only over a normalize→debounce→batch pipeline feeding the existing scan/adopt reconciliation; keep periodic full reconcile.
- Local UDS API at 0600 with `SO_PEERCRED`/`LOCAL_PEERPID` peer-credential verification.
- Daemon needs the single-`*state.Store` singleton from GIT-4/GO-8; consider the read/writer dual-pool (Appendix §3).
- Add a "no-daemon mode" correctness guarantee to `spec/03` (ARCH-1).

### Phase 2 — Multi-device sync
Do NOT ship the hub before the trust plane (ARCH-4). The HLC and apply path must be correct first.

- **SYNC-1/GO-4** explicit `(ms, counter)` HLC + mutex · **SYNC-2** persist + wire the clock · **SYNC-3** clock-skew guard.
- **SYNC-4** transactional `ApplyEvents` + idempotent conflicts · **SYNC-5** tombstone/delete/rename · **SYNC-6** order-independent reconcile.
- **DATA-2** resolve events-table delivery-state contradiction · **DATA-4/ARCH-6** real `ws_<uuidv7>` · **DATA-5** deterministic timestamps/ordering.
- **ARCH-2** keep `internal/sync` an explicit spike; defer `device_sig`/`prev_event_hash` chain format until the hub protocol is written.
- ENV-4 split rewrap vs value rotation in the spec before any blob sync.

### Phase 3 — Agent workspaces
The thin runner (M3.5) is promoted to Phase 0; this phase adds isolation and policy.

- **SEC-6** ed25519 event signatures, sanitized child env, agent command/file policy.
- ENV-5 provider-reference default + `op run`/`op inject` injection (ties to the agent's least-privilege model).
- Agent sandbox via Seatbelt (macOS) + bubblewrap/Landlock/seccomp (Linux), empty-env + allowlist, planted-symlink defense (Appendix §6).
- ARCH-3's stale-base check becomes the gate on `agent finalize`/PR.

### Phase 4 — Optional StrapFS
- Out of scope for these findings; prerequisite is that the Phase 0–3 loop is loved on real usage. The `internal/platform` seams (ARCH-5) and os.Root path-safety (SEC-4 / Appendix §6) are the groundwork that makes a future File Provider/FUSE layer tractable.

---

## Best Practices Appendix

Synthesized from the six research briefs. Each subsection cites the brief's primary sources; the research overwhelmingly **confirms** DevStrap's design rather than redirecting it.

### §1 — Go CLI + daemon as a single portable binary
- Keep the cobra tree thin in `cmd/`; push logic into `internal/`. Use `RunE`, `SilenceUsage`/`SilenceErrors`, and a kubectl-style Factory/IOStreams so the same logic is callable from the daemon. Clean stdout (data only), prompts/progress on stderr.
- One shutdown context via `signal.NotifyContext(ctx, SIGINT, SIGTERM)`, fanned out with `errgroup.WithContext`; thread `ctx` through every git shell-out and SQLite query (non-cooperative code makes "graceful" shutdown hang until SIGKILL). For server drain, always create a FRESH deadline context — never derive it from the cancelled signal context.
- Run as a USER service: macOS LaunchAgent (`RunAtLoad`+`KeepAlive`), Linux `systemd --user` (`Restart=on-failure`, `loginctl enable-linger`). Hardcode absolute `ExecStart` and explicit `HOME`/`PATH`.
- CLI↔daemon over a 0600 Unix domain socket; verify peer identity with `SO_PEERCRED` (Linux) / `LOCAL_PEERPID` (macOS), then map PID→CWD→git-root (canonicalize with `EvalSymlinks`). Start with length-prefixed JSON; leave a gRPC-over-UDS path open.
- Watch directory trees recursively, treat the watcher as advisory + periodic reconcile, and coalesce events (a single editor save = 3–5 events; `npm install` = 10,000+).
- Distribute with GoReleaser + a Homebrew tap, `CGO_ENABLED=0` (keep the pure-Go `modernc` SQLite driver), `-ldflags` version injection.
- Sources: [cobra.dev](https://cobra.dev/docs/explanations/enterprise-guide/) · [VictoriaMetrics graceful shutdown](https://victoriametrics.com/blog/go-graceful-shutdown/) · [user-level daemons](https://til.jingkaihe.com/kodelet-gpt-guest-post-user-level-daemons-systemd-launchd/) · [SO_PEERCRED in Go](https://blog.jbowen.dev/2019/09/using-so_peercred-in-go/) · [fsnotify #372](https://github.com/fsnotify/fsnotify/issues/372) · [GoReleaser Go builder](https://goreleaser.com/customization/builds/builders/go/)

### §2 — Local-first sync, HLC, idempotent delivery, deterministic replay
- HLC is the canonical `(l, c)` pair; never derive ordering from `created_at`. Send: `hlc = max(physical_now<<16, last_hlc+1)`; receive: `max(local, remote, now)` + counter bump; break ties with stable `device_id`.
- Bound against skew: reject/flag remote HLCs whose physical component exceeds local by a configured max-offset (CockroachDB uses ~500ms), and define logical-counter overflow behavior so it cannot bleed into the time bits.
- Idempotent delivery on stable `event_id`, strictly insert-only log, per-device monotonic `seq` for gap detection, apply/delivery state in a separate table.
- Deterministic replay: sort by `(hlc ASC, device_id ASC[, id ASC])` and apply through a pure `Reconcile(local, incoming)` so all replicas converge regardless of arrival order.
- Conflict strategy per data class: HLC-LWW only for low-stakes fields; explicit first-class conflict records (no auto-overwrite) for dangerous cases (delete-vs-dirty, same-path/different-remote).
- Drive sync with a per-peer cursor; write `event_delivery` + cursor advance in the SAME transaction as the apply — never advance before a durable commit. Use HLC-stamped tombstones with cursor-gated GC; snapshot+cursor-reset fallback when the hub log is truncated.
- Sources: [HLC paper (Kulkarni et al.)](https://cse.buffalo.edu/tech-reports/2014-04.pdf) · [Local-first software](https://www.inkandswitch.com/local-first/) · [CockroachDB hlc.go](https://github.com/cockroachdb/cockroach/blob/master/pkg/util/hlc/hlc.go) · [Hybrid Logical Clocks (Forsyth)](https://jaredforsyth.com/posts/hybrid-logical-clocks/)

### §3 — SQLite for embedded Go apps
- Set per-connection pragmas via DSN `_pragma=` (WAL, `busy_timeout`, `foreign_keys(ON)`, `synchronous(NORMAL)`), never a one-off `db.Exec`. DevStrap already does this correctly.
- WAL + `synchronous=NORMAL` + `busy_timeout`; serialize writes via `SetMaxOpenConns(1)` (DevStrap does this) or the higher-performance dual-pool (writer `MaxOpenConns(1)`+`_txlock=immediate`, separate `mode=ro` reader pool) — adopt the dual pool when the daemon runs concurrent reads.
- `_txlock=immediate` to take the write lock at BEGIN (DevStrap has it). Prefer `modernc.org/sqlite` (pure Go) for a distributable binary; avoid `cache=shared` with modernc.
- goose: `SetDialect("sqlite3")` before any Up/Down (DevStrap does this), embed via `//go:embed`. Back up a live WAL DB with `VACUUM INTO` (read-lock only; DevStrap does this).
- Use STRICT tables and explicit indexes for every FK and frequent WHERE/ORDER BY column; SQLite does not auto-index FKs (relevant to DATA-1).
- Sources: [River SQLite docs](https://riverqueue.com/docs/sqlite) · [SQLite Go best practices](https://jacob.gold/posts/go-sqlite-best-practices/) · [modernc with Go](https://www.theitsolutions.io/blog/modernc.org-sqlite-with-go) · [VACUUM](https://www.sqlite.org/lang_vacuum.html) · [goose provider](https://pressly.github.io/goose/documentation/provider/)

### §4 — Git automation at scale (partial clone, worktrees, default branch)
- Default to blobless clone (`--filter=blob:none`) for persistent dev/build environments (matches "skeletons until materialized"); treeless (`--filter=tree:0`) only for ephemeral CI; avoid `--depth=1` for worktrees a developer keeps.
- Blobs download on checkout — pre-warm/warn when offline (relevant to GIT-1's promisor-fetch failure).
- Resolve the remote default branch layered: cached `symbolic-ref refs/remotes/origin/HEAD` → `ls-remote --symref origin HEAD` → `remote set-head origin --auto`; never fall back to `init.defaultBranch` or local `HEAD` (this is GIT-2's exact bug class, fixed repeatedly in Zed/VSCode/taf).
- For fresh-upstream worktrees: fetch first, resolve the base SHA from `refs/remotes/origin/<default>`, then `git worktree add -b <task> <path> <base-sha>`.
- Manage worktrees as first-class objects (`list --porcelain`, `lock`, `remove`, `prune --expire`, `repair`); gate destructive ops on `git status --porcelain`; defer LFS (`GIT_LFS_SKIP_SMUDGE=1`) and submodules until open/hydrate.
- Shell out to system git (go-git lacks partial clone, worktree, full LFS); parse via `--porcelain`/`-z`; set `GIT_TERMINAL_PROMPT=0` (GO-1/SEC-2).
- Sources: [Partial & shallow clone (Stolee)](https://github.blog/open-source/git/get-up-to-speed-with-partial-clone-and-shallow-clone/) · [git-worktree](https://git-scm.com/docs/git-worktree) · [Default-branch handling](https://engineered.at/articles/consistent-handling-of-git-repositories-with-different-default-branches) · [wtx worktree pool](https://github.com/aixolotls/wtx) · [go-git](https://github.com/go-git/go-git/)

### §5 — Secrets & environment
- Use age v1 (`filippo.io/age`) for at-rest bundles, one X25519 recipient stanza per approved device; recipient public keys safe to store, private identities only in the OS keychain (ENV-1).
- Store the private identity via `zalando/go-keyring` (pure Go, no cgo) behind the Mac/Linux adapter; mind macOS's ~4096-byte item limit (store the small identity, not bundles).
- age already provides AEAD; bind `bundle_id`/`workspace_id` into a signed/authenticated manifest header. Only reach for `nacl/secretbox` if a non-age symmetric path is needed (32-byte key, unique 24-byte nonce, scrypt-derived).
- Prefer reference mode for team projects: store `op://` references, resolve with `op run --`/`op inject -i tpl -o out` (subprocess-scoped, masked by default), backed by a least-privilege Service Account (ENV-5).
- Revocation = rewrap recipients **plus** rotate the actual values; removing a recipient does not revoke past access (ENV-4).
- Leak prevention is defense-in-depth: `.gitignore` + pre-commit (gitleaks/detect-secrets) + CI scanning + push protection; model secrets as a redacting capability type, not plain strings; "assume CI logs are public; rotate after any suspected leak" (ENV-2/SEC-3).
- Sources: [filippo.io/age](https://pkg.go.dev/filippo.io/age) · [age authentication](https://words.filippo.io/age-authentication/) · [SOPS+age GitOps](https://www.bigiron.cc/guides/gitops-secrets-the-sops-and-age-pattern) · [op secret references](https://www.1password.dev/cli/secret-references) · [go-keyring](https://github.com/zalando/go-keyring) · [gitleaks](https://github.com/gitleaks/gitleaks)

### §6 — Security for local dev tools & agent sandboxes
- Validate untrusted git remote URLs at config-parse/command-entry: reject control chars and leading dash (CLI flag injection), enforce a scheme allowlist, separate scp-style validation (SEC-1).
- Defend the `ext::`/`file://` RCE class explicitly: `GIT_PROTOCOL_FROM_USER=0` / `protocol.allow` policy on any non-interactive fetch; never interpolate user values into `--upload-pack`/`-c` (SEC-1/SEC-2). Note git normalizes config keys to lowercase — uppercase regex bypasses are real (CVE-2026-28292).
- Use TOCTOU-resistant fd-based traversal: Go 1.24 `os.Root` (openat-based) for all `~/Code` access instead of `EvalSymlinks`+prefix checks; for the strongest Linux guarantee, `openat2` with `RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS` (SEC-4). `os.Root` does not cover bind mounts.
- Redact via allow-list `LogValuer` on distinct secret types (not deny-list `ReplaceAttr`); use masq-style recursion since slog reflection skips `LogValue` on nested struct fields (ENV-2/SEC-3).
- Enforce 0600/0700 with explicit post-write `chmod` (mode args only apply at creation; re-chmod loose perms on upgrade).
- Sandbox agent worktrees with OS-native primitives layered: Seatbelt (macOS) + bubblewrap + Landlock + seccomp (Linux), default-deny egress; empty child env + allowlist + unconditional dangerous-name stripping last; block planted-symlink sandbox expansion; command-execution policy (prefix-match, auto-allow read-only, forbid `rm`/`sudo`/force-push). Process sandboxes share the host kernel — a microVM tier is needed only for genuinely untrusted third-party code (SEC-6, Phase 3).
- Sources: [go-git GHSA-v725-9546-7q7m](https://github.com/go-git/go-git/security/advisories/GHSA-v725-9546-7q7m) · [git protocol policy commit](https://github.com/git/git/commit/f1762d772e9b415a3163abf5f217fc3b71a3b40e) · [os.Root](https://go.dev/blog/osroot) · [openat2](https://man7.org/linux/man-pages/man2/openat2.2.html) · [Redacting with slog](https://blog.arcjet.com/redacting-sensitive-data-from-logs-with-go-log-slog/) · [Anthropic sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime/)

---

*End of report. 50 recommendations across 10 themes; 5 merges folded 10 near-duplicate findings into 5 surviving entries (GO-4→SYNC-1, GO-2→SEC-5, GO-8→GIT-4, SEC-3→ENV-2, ARCH-6→DATA-4). No confirmed finding was dropped.*
