# DevStrap — Design & Implementation Audit (Fifth Pass)

_Date: 2026-06-29 · Trunk audited: `be664ba` (`fix: PASS4 audit Phase A hardening (12 findings) (#20)`)_

## How this relates to the prior audits

This is the **fifth** design & implementation pass. It does **not** restate the still-open recommendations from `AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md`; those remain tracked (re-prioritized in **Appendix A**). Instead it focuses where a fresh pass adds value:

1. **Adversarial review of the *just-landed* PASS4 batch code** — `forge.go`, `conflicts.go`, `clone.go`, `materialize.go`, `run_loop.go`, `blob_gc.go`, `hub/r2.go`, `draftbundle`, `envbundle`. Fresh code carries fresh defects; PASS4 reviewed the *pre-batch* tree.
2. **Dimensions PASS4 under-examined** — convergence correctness of the new conflict/rename paths, the *end-to-end* reachability of the cloud hub, CLI/scriptability discipline, observability, spec-vs-code truth, and process hygiene.
3. **Concrete new features** grounded in the architecture and in comparable tools (Mutagen, Syncthing, devpod/devcontainers, chezmoi, mise, git-annex, Coder/Gitpod).

**ID scheme (and a note on `PROC-01`).** Every finding here is prefixed `P5-` so its ID is globally unique. The prior audits reused bare IDs (`GIT-01` denotes two unrelated problems across passes) — that collision is itself a finding (`P5-PROC-01`), and this pass demonstrates the fix.

## Methodology

Findings were produced by a verification-driven multi-agent workflow against the `be664ba` worktree: **seven dimension reviewers** (security/crypto, sync/convergence, CLI/UX, hub/scale, quality/CI, product/architecture, spec/data-model/process), each told exactly which PASS4 IDs are *shipped* vs *open* so they hunt **new** issues; then **every candidate finding was independently adversarially verified** by a separate agent that re-opened the cited code and tried to refute it. 43 candidates → 41 survived → consolidated to the 36 below (overlapping findings merged; two rejected as duplicates of already-tracked items). External best-practice anchors are cited inline. Severities reflect the verifier's correction, not the reviewer's first instinct.

**Severity:** P1 = correctness/security/data-loss or major; P2 = significant; P3 = minor/polish/DX. **Effort:** S ≈ <½ day, M ≈ 1–3 days, L ≈ ~1 week, XL ≈ multi-week.

## Table of contents

- [Executive summary](#executive-summary)
- [Findings at a glance](#findings-at-a-glance)
- [Prioritized roadmap](#prioritized-roadmap)
- [Quick wins](#quick-wins)
- [Strategic bets](#strategic-bets)
- [Security & Cryptography](#security--cryptography)
- [Sync Engine, Conflicts & Convergence](#sync-engine-conflicts--convergence)
- [Cloud Hub, Backend & Scale](#cloud-hub-backend--scale)
- [CLI, UX & Developer Experience](#cli-ux--developer-experience)
- [Code Quality, Testing & CI](#code-quality-testing--ci)
- [Product, Architecture & New Features](#product-architecture--new-features)
- [Specs, Data Model & Process Hygiene](#specs-data-model--process-hygiene)
- [Appendix A — Carried-forward PASS4 open items (re-prioritized)](#appendix-a--carried-forward-pass4-open-items-re-prioritized)
- [Appendix B — New feature catalog](#appendix-b--new-feature-catalog)

## Executive summary

The PASS4 hardening batch genuinely landed and the codebase is in good shape: the HLC primitives are correct, content-address verification is enforced on every hub-trusting pull, `childenv` sanitization is applied to git/agent/editor/`op` subprocesses, the keychain fallback fails closed, the R2 retry/conditional-put/DeleteBlob logic is well unit-tested against an in-memory double, and the eager-materialize flow is race-checked end-to-end through the real binary. This pass found **no systemic rot** — it found the kind of issues you only see *after* a fast feature batch lands.

Risk and opportunity cluster in five themes:

1. **The revoke/re-encryption path is architecturally incompatible with content-addressed immutable blobs.** `P5-SEC-01` (the one P1): on `devices revoke --hub-file`, the code rewraps each blob, repoints the *mutable local table*, and deletes the old ciphertext from the hub — **but emits no superseding namespace event**, so every other device replays the original (now-deleted) `age_blob:<sha256>` ref and permanently loses draft access. The root cause (re-encryption changes the content address, which the append-only log cannot retroactively update) is precisely what **envelope encryption** (`SEC-07`) dissolves. This is latent today (the hub isn't live) but is a data-loss bug the moment it is — exactly the pre-live window PASS4 said to harden.

2. **The cloud hub is unreachable end-to-end.** `P5-HUB-01`: there is no `aws-sdk-go-v2` dependency, the `R2Config` struct is never instantiated, `R2Hub` is never constructed by the CLI, and `sync`/`run-loop` hard-error `--hub-file is required until the production hub exists`. The "production R2 backend" is well-tested *logic* with no client and no selection seam. The whole zero-knowledge thesis cannot make a single real round-trip; this pass gives the concrete path (S3 adapter + `hubFromOptions` factory + MinIO/LocalStack integration test).

3. **Convergence regressions in the new conflict/rename code.** `conflict.resolved` bakes a device-local `namespace_id` into its match fingerprint, so it cannot converge for the two conflict types users actually resolve (`P5-SYNC-02`); the resolution *action* (`--keep-local|--keep-remote|--keep-both`) is recorded but never applied to project state (`P5-SYNC-04`); `project.renamed` leaves no tombstone at the old path, so a stale add/update resurrects the renamed-away project (`P5-SYNC-03`); and the transport cursor is keyed on logical HLC, silently stranding cross-batch late-arriving events from an offline device (`P5-SYNC-01`).

4. **The `materialize` exit-code fix backfires, and the newest surfaces are untested.** `P5-QUAL-01`: `materialize` counts the *normal* "no draft bundle synced yet" state as a hard failure, so it returns non-zero on any `~/Code` holding a synced local-only project — re-breaking the QUAL-03 CI gate it was meant to fix. `run-loop` and the entire `draft` round-trip have **zero tests** (`P5-QUAL-02`), hiding a latent jitter panic (`P5-QUAL-03`).

5. **Spec/doc truth has drifted and the gate is blind to it.** Several specs re-stamped `last_reviewed: 2026-06-29` still call shipped features "not yet implemented" (one claim — "no bundle/snapshot code exists today" — is flatly false: `P5-DOC-01`), because the spec-drift gate only checks that a path-mapped file was *touched* and that command names appear as raw substrings (`P5-DX-02`). Four root-level audit files (6,525 lines) with colliding IDs compound this (`P5-PROC-01`).

The near-term imperative: **fix the revoke event-emission bug and decide the envelope-encryption migration before the hub goes live; make `conflicts resolve` actually converge and act; un-break the `materialize` exit code; and wire the spec-drift gate to catch prose staleness.** Then invest in the reachability and observability (`hubFromOptions`, `doctor --remote`, `status --watch`) that turn the proven engine into a daily-driver.

## Findings at a glance

| Dimension | P1 | P2 | P3 | Total |
|---|---|---|---|---|
| Security & Cryptography | 1 | 2 | 2 | 5 |
| Sync Engine, Conflicts & Convergence | 0 | 4 | 1 | 5 |
| Cloud Hub, Backend & Scale | 0 | 2 | 2 | 4 |
| CLI, UX & Developer Experience | 0 | 1 | 5 | 6 |
| Code Quality, Testing & CI | 0 | 2 | 3 | 5 |
| Product, Architecture & New Features | 0 | 0 | 5 | 5 |
| Specs, Data Model & Process Hygiene | 0 | 1 | 5 | 6 |
| **Total** | **1** | **12** | **23** | **36** |

## Prioritized roadmap

| # | Sev | ID | Recommendation | Dim | Effort |
|---|---|---|---|---|---|
| 1 | P1 | P5-SEC-01 | Emit a superseding namespace event before deleting an old hub blob on revoke; don't strand other devices | Sec | L |
| 2 | P2 | P5-SYNC-02 | Drop the device-local `namespace_id` from the `conflict.resolved` match so resolution converges | Sync | S |
| 3 | P2 | P5-QUAL-01 | Stop `materialize` counting "no draft bundle yet" as failure; classify it `skipped` | Quality | M |
| 4 | P2 | P5-SYNC-04 | Make `conflicts resolve --keep-*` actually mutate namespace state (or relabel as advisory) | Sync | M |
| 5 | P2 | P5-SYNC-03 | Write a tombstone at the old path on rename so a stale add/update can't resurrect it | Sync | M |
| 6 | P2 | P5-SYNC-01 | Key the pull cursor on hub ingestion order, not logical HLC, so late events aren't stranded | Sync | L |
| 7 | P2 | P5-HUB-01 | Wire a real S3 adapter + `hubFromOptions` factory + MinIO/LocalStack integration test | Hub | L |
| 8 | P2 | P5-HUB-02 | Add hub-side GC + draft-snapshot pruning so superseded blobs are reclaimable | Hub | M |
| 9 | P2 | P5-SEC-02 | Bound the decompression budget on *every* tar entry, not just `TypeReg`/`TypeDir` | Sec | S |
| 10 | P2 | P5-SEC-03 | Run dependency-rebuild lifecycle scripts through `childenv`, not the raw parent env | Sec | S |
| 11 | P2 | P5-QUAL-02 | Add tests for `run-loop` and the `draft` cross-device round-trip | Quality | M |
| 12 | P2 | P5-CLI-01 | Introduce a `Renderer`; honor `--json` everywhere (or reject it) | CLI | M |
| 13 | P2 | P5-DX-02 | Make the spec-drift gate detect content/inventory staleness, not just file-touch | Specs | M |
| 14 | P3 | P5-SEC-04 | Don't push local-only env secret blobs to the hub during revoke rewrap | Sec | M |
| 15 | P3 | P5-SEC-05 | Cap `redact.Writer`'s line buffer so a newline-free stream can't grow memory unbounded | Sec | S |
| 16 | P3 | P5-ARCH-01 | Extract a pure `Decide(state, event)` layer so convergence can be property-tested | Sync | M |
| 17 | P3 | P5-HUB-03 | Give `R2Hub.Pull` a retention floor + `ErrSnapshotRequired` before compaction lands | Hub | M |
| 18 | P3 | P5-HUB-04 | Fetch the pull log with bounded concurrency + an exclusive composite cursor | Hub | M |
| 19 | P3 | P5-CLI-02 | Wire or remove the dead `materialize --partial` flag | CLI | S |
| 20 | P3 | P5-CLI-03 | Reject `--open`/`--vscode` conflict before the network clone (`MarkFlagsMutuallyExclusive`) | CLI | S |
| 21 | P3 | P5-CLI-04 | Use `ssh -G` (or honor `Include`/negation) for SSH host-alias resolution | CLI | M |
| 22 | P3 | P5-CLI-05 | Route progress/warnings to stderr; surface repeated `run-loop` tick failures | CLI | S |
| 23 | P3 | P5-DX-01 | Add dynamic shell completion for namespace paths and enum flags | CLI | M |
| 24 | P3 | P5-QUAL-03 | Clamp the `run-loop` jitter bound so `rand.Int64N(0)` can't panic | Quality | S |
| 25 | P3 | P5-QUAL-04 | Consume or drop the CI coverage profile (gate, upload, or remove) | Quality | S |
| 26 | P3 | P5-QUAL-05 | Emit `tar.TypeDir` headers so draft bundles preserve empty directories | Quality | S |
| 27 | P3 | P5-PROD-01 | Reconcile `deriveDisplayStatus` with the states writers actually produce ("ready" is dead) | Product | M |
| 28 | P3 | P5-PROD-02 | Make the default local-only revoke's hub-cleanup promise real (pending-delete queue) | Product | M |
| 29 | P3 | P5-PROD-03 | Add `devstrap env rotate` + a `needs_rotation` clear path | Product | M |
| 30 | P3 | P5-PROD-04 | Document the shipped `devstrap clone` in the README (it's still listed as future) | Product | S |
| 31 | P3 | P5-PROD-05 | New: `doctor --remote` hub-health probe + `status --watch`/TUI convergence view | Product | L |
| 32 | P3 | P5-DOC-01 | Fix spec/07's false "not yet implemented" claims about shipped DRAFT/HUB code | Specs | M |
| 33 | P3 | P5-DOC-02 | De-contradict spec/00's "Not implemented yet" block; add the 7 missing commands | Specs | M |
| 34 | P3 | P5-DATA-01 | Reconcile spec/12's migration/index inventory (00010 collision) | Specs | S |
| 35 | P3 | P5-DATA-02 | Add a DB `UNIQUE` index for `draft_snapshots` idempotency (defense-in-depth) | Specs | S |
| 36 | P3 | P5-PROC-01 | Consolidate audit files + adopt pass-scoped IDs + a single status ledger | Specs | M |

## Quick wins

- **`P5-SYNC-02`** — Deleting one predicate from the `conflict.resolved` match (`namespace_id`) makes resolution converge across devices. One-line WHERE-clause change + a test.
- **`P5-SEC-02`** — Move `totalBytes += hdr.Size` (with the abort check) above the type switch, or reject non-`TypeReg`/`TypeDir` entries. Closes the decompression-budget bypass.
- **`P5-SEC-03`** — Set `cmd.Env = childenv.FromOS(childenv.BasicAllowlist(), nil)` in `runRebuildCommand`. The only subprocess in the tree that doesn't sanitize.
- **`P5-QUAL-03`** — `bound := int64(interval)/10; if bound < 1 { bound = 1 }`. Removes a latent panic in an unattended loop.
- **`P5-CLI-03`** — `cmd.MarkFlagsMutuallyExclusive("open", "vscode")` rejects the bad combo before the network clone.
- **`P5-PROD-04`** — Add the shipped `devstrap clone` to the README table/quickstart and remove it from the "future" sentence; near-zero effort, immediate onboarding payoff.
- **`P5-CLI-02`** — Thread `--partial` through to `hydrateProjectUnlocked` (or delete the flag). Stop shipping a flag that lies.

## Strategic bets

- **`P5-SEC-01` + envelope encryption (`SEC-07`)** — The just-landed rewrap-and-delete design is fundamentally incompatible with content-addressed, immutable, event-referenced blobs. Adopt `MasterSecret → KEK → per-blob DEK`: encrypt content once with a stable DEK (so the `age_blob:<sha256>` ref never changes), wrap the DEK/KEK to each approved device, and make revocation a *metadata-only* KEK rewrap + epoch bump. This dissolves `P5-SEC-01`, `P5-SEC-04`, `P5-PROD-02`, and the open `SEC-07`/`HUB-12` in one stroke and gives forward secrecy + crypto-erasure. ([Umbrel home-backup](https://sadensmol.com/posts/2026/04/learning-system-design-10-umbrel-home-backup/), [Meta Labyrinth](https://engineering.fb.com/wp-content/uploads/2026/05/Minos-Updates-2026-Encrypted-Backups-White-Paper.pdf))
- **`P5-HUB-01` — reachability before more hardening.** The hub is the product's headline and it has never made a real round-trip. Wiring the S3 adapter + `hubFromOptions` factory + a MinIO integration test converts a large body of untested-against-reality logic into something exercisable, and is the precondition for every other `HUB-*` item to be more than theoretical.
- **`P5-SYNC-01` — separate the transport clock from the logical clock.** Pull on a hub-assigned, monotonic *ingestion* position; keep HLC strictly for apply-time ordering. This is the canonical pattern (Linear/Figma) and is the only robust fix for the offline-device "forgot to push, synced late" scenario DevStrap exists to solve.
- **`P5-DX-02` + `P5-PROC-01` — make truth cheap.** Generate the spec's command/migration inventory from the binary and diff it in CI; consolidate the four audit files into one keyed ledger. Otherwise every fast batch will keep silently desyncing the specs that AGENTS.md makes required reading.

---

## Security & Cryptography

The just-landed crypto hardening is mostly sound — SEC-03 hash-verify-on-fetch is enforced on every hub-trusting path and never bypassed, blob keys are validated against traversal, `childenv` is an allowlist, the keychain fallback fails closed, and the redaction regexes are RE2 (no ReDoS). The defects below are concentrated in the *revoke/re-encryption* path that PASS4's SEC-01 introduced, plus two smaller new gaps.

### P5-SEC-01 — Revoke rewrap deletes the old hub blob but emits no superseding event, so every other device permanently loses draft access

**Severity / Effort / Category:** P1 / L / security · correctness · data-loss · _new (root-cause of `SEC-01`)_

**Problem.** `rewrapBlobsOnRevoke` re-encrypts each referenced blob to the reduced recipient set, writes the new local blob, calls `store.UpdateBlobRef(old→new)` to repoint the **mutable local `secret_bindings`/`draft_snapshots` tables**, then `rewrapHubCleanup` PUTs the new blob and DELETEs the old ciphertext from the hub. It never appends a namespace event. But `draft_snapshots.blob_ref` is reconstructed on every device from the **immutable, append-only `draft.snapshot.created` event payload**. The event log still carries `oldRef`. So after `devices revoke --hub-file`:

1. the revoking device deletes `oldRef` from the hub and pushes `newRef` — but pushes **no event** referencing `newRef`;
2. every other approved device replays the original event (`oldRef`), sets `draft_snapshots.blob_ref = oldRef`, and `GetBlob(oldRef)` → **deleted** → the draft never materializes, forever;
3. the SYNC-04 push cursor means the revoking device won't even re-push the original event, so the hub log permanently points at the deleted ref while `newRef` is undiscoverable;
4. on the revoking device, `gcUnreferencedBlobs` reads refs from the now-repointed tables (`newRef` only) and reaps the local `oldRef` too — so a DB rebuild from the hub breaks the revoking device as well.

The blast radius is the `--hub-file` spike *today* (multi-device sync is a file-backed spike; the R2 hub isn't live), but this is a P1-class data-loss bug the instant the hub goes live — exactly the pre-live window PASS4 said to harden. The spec shares the gap: `spec/09:65` assumes the superseded blob simply "becomes unreferenced and is garbage-collected" with no event-emission requirement.

**Evidence.** `internal/cli/blob_gc.go:80` `store.UpdateBlobRef(ctx, ref, newRef)`; `blob_gc.go:147` `hub.DeleteBlob(ctx, blobHashHex(oldRef))`; `rewrapBlobsOnRevoke` (`blob_gc.go:37-95`) contains **no** `InsertLocalEvent` (the only one in `internal/cli` is `draft.go:92`). `internal/sync/events.go:454` `tx.RecordDraftSnapshotTx(ctx, project.ID, payload.BlobRef, …)` (table fed from the immutable event). `internal/cli/sync.go:218-219` `if event.Type != dssync.EventDraftSnapshotCreated { return "", false }` — only `draft.snapshot.created` carries a blob ref; there is no rewrap event type. `spec/09_SECRETS_AND_ENVIRONMENT.md:65`.

**Recommendation.** Never delete the old hub blob until a superseding event that references the new blob is durably pushed. Short-term: after rewrap, emit a new `draft.snapshot.created` (or a dedicated `draft.snapshot.rewrapped`) event carrying `newRef`, push event + `newRef`, verify, *then* `DeleteBlob(oldRef)`. Long-term: adopt envelope encryption (`SEC-07`) so revocation rewraps a small KEK map in the event log and never repoints/deletes content-addressed blobs at all.

**Actionable steps.**
1. In `rewrapBlobsOnRevoke`, after `UpdateBlobRef`+`writeEnvBlob`, `InsertLocalEvent` a superseding event per affected draft project carrying `newRef`.
2. Reorder `rewrapHubCleanup`: push event → push `newRef` → verify → only then `DeleteBlob(oldRef)`.
3. Add a multi-device test: A snapshots a draft, B approves+syncs, A revokes a third device with `--hub-file`, B re-syncs and must still materialize the draft.
4. Fix `spec/07:549-555` and `spec/09:65` to state that re-encryption emits a superseding event before the old blob is deleted.

**Example.**
```go
// blob_gc.go — make the new ref discoverable before deleting the old one.
ev, err := dssync.CreateDraftSnapshotRewrapEvent(ctx, store, project.ID, newRef) // append-only, carries newRef
if err != nil { return rewrapped, err }
if hub != nil {
    if err := hub.Push(ctx, []state.Event{ev}); err != nil { /* keep oldRef; abort delete */ }
    if err := hub.PutBlob(ctx, blobHashHex(newRef), bytes.NewReader(newCiphertext)); err != nil { return }
    // only now is newRef both pushed AND referenced by a pushed event:
    if !blobRefStillReferenced(ctx, store, oldRef) { _ = hub.DeleteBlob(ctx, blobHashHex(oldRef)) }
}
```

**References:** [Meta Labyrinth — epoch rotation & device revocation](https://engineering.fb.com/wp-content/uploads/2026/05/Minos-Updates-2026-Encrypted-Backups-White-Paper.pdf); [Umbrel encrypted backup — envelope encryption makes revocation metadata-only](https://sadensmol.com/posts/2026/04/learning-system-design-10-umbrel-home-backup/).

### P5-SEC-02 — Decompression-bomb guard is bypassable: the budget is only charged for `TypeReg`/`TypeDir` entries

**Severity / Effort / Category:** P2 / S / security · _new (sharpens `QUAL-01`)_

**Problem.** `draftbundle.ExtractWithLimits` only increments the aggregate `totalBytes`/`fileCount` budget for `tar.TypeReg` (and `TypeDir`) entries. Every other typeflag falls into a no-op `default:` branch. A compromised-but-trusted device can craft a bundle whose entries declare a large `Size` under a header type the guard ignores, so `tar.Next()` walks past unaccounted data. (Verifier nuance: Go's `archive/tar` forces `nb=0` for header-only types like symlink/char/block/fifo, so *those* specific types don't decompress their declared region — but the **fix is identical and the class is real**: any non-`TypeReg` entry that does carry data, including GNU/PAX extension records, escapes the budget. The guard should bound *every* entry rather than enumerate safe types.)

**Evidence.** `internal/draftbundle/draftbundle.go:258-320` — `case tar.TypeReg` increments `fileCount`/`totalBytes` and bounds `hdr.Size` against the remaining budget; `default:` (`321-323`, `// Skip symlinks, devices, and other special types for safety.`) does **no** accounting. `Pack` only ever emits `Typeflag: tar.TypeReg` (`draftbundle.go:142`), so any non-`TypeReg` entry is attacker-introduced.

**Recommendation.** Charge the budget on every entry, or reject any entry that is not `TypeReg`/`TypeDir` outright (Pack never produces them).

**Actionable steps.**
1. Before the type switch: `totalBytes += hdr.Size; if totalBytes > limits.MaxBytes { return ErrBundleTooLarge }`.
2. Change `default:` to `return fmt.Errorf("unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)` — Pack never emits these.
3. Add a test crafting a non-`TypeReg` entry with oversized declared `Size` asserting `ErrBundleTooLarge` / rejection.

### P5-SEC-03 — `rebuildDependencies` runs package-manager lifecycle scripts with the full unsanitized parent environment

**Severity / Effort / Category:** P2 / S / security · _new (relates `GIT-03`)_

**Problem.** Every other subprocess in the tree is launched with a sanitized `childenv` (git: `git.go:704`; agent: `agent.go:449`; editor: `editor.go:29`). `runRebuildCommand` alone constructs `exec.CommandContext`, sets `cmd.Dir`, but **never sets `cmd.Env`** — a nil `Env` means the child inherits the entire `os.Environ()`. So `npm ci` / `pnpm install` / `poetry` / `cargo`, which execute package lifecycle (`postinstall`) scripts, run with whatever `OP_*`, cloud credentials, CI tokens, or `LD_*`/`DYLD_*`/`NODE_OPTIONS` the user exported. The rebuild is opt-in (`DEVSTRAP_REBUILD_DEPS`), and running `postinstall` is arbitrary code execution regardless — but env sanitization is the cheap, consistent mitigation the rest of the codebase already applies.

**Evidence.** `internal/cli/materialize.go:316-324` `runRebuildCommand(...) { cmd := exec.CommandContext(ctx, command, args...); cmd.Dir = dir; … return cmd.Run() }` — no `cmd.Env`. Contrast `internal/git/git.go:704` `childenv.FromOS(childenv.BasicAllowlist(), …)` and `internal/cli/agent.go:449`.

**Recommendation.** Build the rebuild environment through `childenv` so dangerous names are stripped and ambient secrets aren't handed to lifecycle scripts.

**Actionable steps.**
1. `cmd.Env = childenv.FromOS(childenv.BasicAllowlist(), nil)` (plus any toolchain-required vars).
2. Document that dependency rebuild runs untrusted lifecycle scripts under a reduced environment.
3. Add a test asserting a `Dangerous`/ambient secret var in `os.Environ` is not visible to the rebuild subprocess.

### P5-SEC-04 — Revoke rewrap silently uploads local-only env secret blobs to the hub, widening footprint and orphaning blobs

**Severity / Effort / Category:** P3 / M / security · data-minimization · _new (relates `SEC-01`)_

**Problem.** `rewrapBlobsOnRevoke` iterates `store.AllBlobRefs`, which UNIONs `secret_bindings.encrypted_value_ref` (env) **and** `draft_snapshots.blob_ref` (draft), and calls `rewrapHubCleanup` for each — which unconditionally `PutBlob`s to the hub. But env secret blobs are never part of normal hub sync (only `draft.snapshot.created` blobs are pushed/pulled; synced encrypted env-bundle exchange is documented as unbuilt). So the first time a locally-captured `.env` blob exists, a revoke pushes it to the hub — and, like `P5-SEC-01`, it becomes an event-unreferenced orphan. Confidentiality is preserved (blobs are age-encrypted, hub is zero-knowledge); this is footprint/orphan hygiene, hence P3.

**Evidence.** `internal/state/store.go:1743-1762` (`AllBlobRefs` UNIONs `secret_bindings.encrypted_value_ref` and `SELECT DISTINCT blob_ref FROM draft_snapshots …`); `internal/cli/blob_gc.go:139` `hub.PutBlob(...)` for every ref; `internal/cli/sync.go:218-219` `blobRefFromEvent` returns false for everything but `draft.snapshot.created`.

**Recommendation.** Scope hub upload/delete in the rewrap path to blobs the hub actually holds (draft-snapshot refs); rewrap env blobs locally only until env-bundle exchange is intentionally implemented.

**Actionable steps.**
1. Partition `AllBlobRefs` into env (`secret_bindings`) and draft (`draft_snapshots`) refs.
2. Local rewrap + `UpdateBlobRef` for all; call `rewrapHubCleanup` only for draft refs.
3. Test: revoking with `--hub-file` does not create a new hub blob for a purely local env binding.

### P5-SEC-05 — `redact.Writer`'s line buffer is unbounded; a newline-free stream grows memory without limit

**Severity / Effort / Category:** P3 / S / reliability · _new_

**Problem.** `redact.Writer.Write` appends all input to `w.buf` and flushes only complete `\n`-terminated lines. A semi-trusted agent subprocess (captured to the `0600` log) that emits a very long line or a binary blob with no newline grows `w.buf` without bound until process exit or OOM. (Confidentiality is intact — buffered bytes are never forwarded until scrubbed — so this is memory-DoS, not a secret leak.)

**Evidence.** `internal/redact/redact.go:251-281` `w.buf.Write(p)` then `for { idx := bytes.IndexByte(data, '\n'); if idx < 0 { break } … }` — no cap when no newline; wired to agent stdout/stderr at `internal/cli/agent.go:464-467`.

**Recommendation.** Cap the buffered line length: when `w.buf` exceeds a threshold (e.g. 1 MiB) without a newline, scrub-and-flush the partial buffer.

**Actionable steps.**
1. Add a `maxLine` constant; in `Write`, when `w.buf.Len()` exceeds it without a newline, scrub and emit the partial buffer.
2. Test a multi-megabyte newline-free write asserting bounded growth and that registered secret values are still scrubbed.

---

## Sync Engine, Conflicts & Convergence

The HLC primitives are correct (monotonic `Send`, skew-bounded `Receive`, counter-overflow carry, deterministic `(HLC, deviceID, eventID)` sort), and the SYNC-01 low-water-mark cursor + HUB-13 inclusive boundary correctly handle *within-batch* skips and same-HLC re-delivery. The findings below are in the *new* conflict/rename code and the *cross-batch* cursor semantics.

### P5-SYNC-01 — Pull cursor keyed on logical HLC silently strands cross-batch late-arriving lower-HLC events

**Severity / Effort / Category:** P2 / L / sync · data-loss · _new (beyond `SYNC-01`)_

**Problem.** Both hub backends pull by the event's own HLC (`Pull` returns events with `HLC >= afterHLC`) and advance the cursor monotonically to `maxAppliedHLC`. SYNC-01's low-water mark only protects events *skipped within the current batch*. It does nothing for an event that lands on the hub **after** a puller has already advanced its cursor past that event's HLC — precisely the "Dropbox for code" scenario: an offline device creates events stamped with their creation-time HLC and pushes them later; any peer whose cursor already moved past that HLC never sees them. (Severity corrected P1→P2 because the R2 hub isn't live, so this is latent on the `--hub-file` spike — but it's the core offline scenario DevStrap exists to solve.)

**Evidence.** `internal/sync/hub.go:113` `if event.HLC >= afterHLC` (FileHub); `internal/hub/r2.go:137,166` (R2 lists/filters by HLC-padded key); `internal/state/store.go:2486` `last_hlc_applied = MAX(...)` (cursor only advances); the `hub_cursors` schema stores only `last_hlc_applied`.

**Recommendation.** Decouple the transport cursor from the logical clock: have the hub expose an arrival-ordered, monotonically increasing pull position (an ingestion sequence, or an ingestion-timestamp prefix in the R2 object key; an append index for FileHub) and pull by that position so no appended event is skipped regardless of HLC. Keep HLC strictly for apply-time ordering. Pair with the snapshot/compaction work (`SYNC-02`/`HUB-11`).

**Actionable steps.**
1. Add an ingestion-order component to `R2Hub` keys (e.g. `workspaces/<ws>/log/<ingest_seq>/<event_id>.json`); for FileHub, append in receive order and return events after a stored append index.
2. Change the cursor column from `last_hlc_applied` to an opaque monotonic `last_ingest_position`; keep `ApplyEvents` HLC-ordered for apply.
3. Regression test: device X pushes `hlc=100,110` *after* a puller advanced its cursor to `hlc=300`; assert the puller still receives 100/110.

**References:** [Clock systems for sync — HLC for ordering, server-assigned position for transport](https://agenticdevelopercookbook.com/guidelines/planning/data/clock-systems); [Local-first bidirectional sync — advance the cursor by downloaded position, reconcile by HLC](https://www.welcomedeveloper.com/posts/local-first-architecture-5-bidirectional-sync/).

### P5-SYNC-02 — `conflict.resolved` bakes a device-local `namespace_id` into its match fingerprint, so it never converges for the two conflict types users actually resolve

**Severity / Effort / Category:** P2 / S / sync · convergence · _new (`PROD-06`)_

**Problem.** PROD-06's whole point is that a `conflict.resolved` event marks the matching open conflict resolved on every device so the open-conflict count converges, matching on a "stable `(namespace_id, type, details_json)` fingerprint." But `namespace_id` is **not** stable across devices — a project's id is a locally-minted `prj_<uuidv7>`. The two conflict types a user actually resolves — `same_path_different_remote` and `pending_delete_conflict` — are inserted with `namespace_id = existing.ID` (the local `prj_` id). So `ResolveConflictByFingerprint` matches on a per-device id and never fires on the receiving device; the conflict stays open there forever. (Only the existing test passes because it uses an empty `namespace_id`.)

**Evidence.** `internal/sync/events.go:363` `tx.InsertConflict(ctx, existing.ID, "same_path_different_remote", details)` and `events.go:395` (`pending_delete_conflict`); `internal/state/store.go:897` `existingID, err = id.New("prj")` (per-device); `internal/cli/conflicts.go:143-145` copies the local `prj_` id into the synced payload.

**Recommendation.** Resolve by `(workspace_id, type, details_json)` only — `details_json` already embeds the stable `Path` and event-coordinate winner/loser, so `(type, details_json)` is globally unique and converges. Keep `namespace_id` on the local row for display only.

**Actionable steps.**
1. Drop the `namespace_id` predicate from `ResolveConflictByFingerprint` (match on `workspace_id, type, details_json, status='open'`).
2. Stop sending `NamespaceID` in `ConflictResolvedPayload` (or treat it as advisory).
3. Apply test with a deliberately *mismatched* `namespace_id` on the local row (two devices' distinct ids) asserting convergence.

### P5-SYNC-03 — `project.renamed` leaves no tombstone at the source path; a stale or cross-batch add/update resurrects the renamed-away project

**Severity / Effort / Category:** P2 / M / sync · convergence · _new_

**Problem.** `RenameProject` re-keys the same row's `path_key` from old to new in place, so the old `path_key` disappears and no tombstone is left. The add/update apply path guards deletes via `TombstoneHLC`, but there is no equivalent guard for renames. So any add/update event targeting the old path — even one with a *lower* HLC than the rename — finds no active row and no tombstone, calls `UpsertProject`, and resurrects a ghost project at the old path. This is non-commutative and breaks LWW across batches: `[add(x,h10), rename(x→y,h20)]` then a late `[update(x,h15)]` diverges.

**Evidence.** `internal/state/store.go:1048-1054` `UPDATE namespace_entries SET path = ?, path_key = ?, … WHERE id = ?` (re-key in place, no tombstone); `internal/sync/events.go:347-351` — the only resurrection guard is the delete tombstone.

**Recommendation.** On rename, write a tombstone at the old `path_key` stamped with the rename event's HLC (mirroring delete), so a later add/update at the old path is HLC-gated by the same `TombstoneHLC` check.

**Actionable steps.**
1. In `RenameProject`, after re-keying, insert/keep a deleted `namespace_entries` row (or a rename-tombstone) at the old `path_key` with `tombstone_hlc = renameHLC`.
2. Confirm the existing `TombstoneHLC` guard then covers renamed-away paths.
3. Convergence test: `add(x,h10)+rename(x→y,h20)` in one batch, then `update(x,h15)` in a later batch → assert no ghost `x`; plus the symmetric `update(x,h30)` legitimately re-creates `x`.

### P5-SYNC-04 — `conflicts resolve --keep-local|--keep-remote|--keep-both` is recorded but never applied to project state

**Severity / Effort / Category:** P2 / M / sync · product · _new (`PROD-06`)_

**Problem.** `resolve` presents three meaningful choices, but the action is only written into `resolution_json` and the `conflict.resolved` payload — nothing consumes it to mutate the namespace. The project's actual remote for a `same_path_different_remote` conflict is decided purely by `reconcileSamePath`/`samePathLess` at apply time (HLC → deviceID → eventID). So `conflicts resolve <id> --keep-local`, when the deterministic HLC winner was the *remote* variant, silently leaves the project on the remote variant; the user's choice is ignored at the data level. `--keep-both` only prints a manual instruction. The flag help ("keep the remote version, discard the local variant") is therefore false.

**Evidence.** `internal/cli/conflicts.go:122-152` builds the resolution, emits the event, calls `ResolveConflict` — no project mutation; `internal/sync/events.go:422-437` the `EventConflictResolved` handler only calls `ResolveConflictByFingerprint`, never touches `git_repos`; no consumer of `payload.Action` beyond storage.

**Recommendation.** Make the resolution authoritative: on `--keep-local`/`--keep-remote`, upsert the chosen remote into `git_repos` and emit a `project.updated` event (so the choice converges via the normal LWW path with a fresh dominating HLC); implement `--keep-both` by materializing the loser at a deterministic sibling path. If enforcement is out of scope short-term, relabel the command so the help doesn't promise discarding the other variant.

**Actionable steps.**
1. Switch on `action` in the resolve `RunE` and the `EventConflictResolved` handler; for `--keep-remote`, write the remote variant from `DetailsJSON` and emit `project.updated`.
2. Implement `--keep-both` to emit `project.added` for the loser at `<path>.<remote>` instead of printing a manual workaround.
3. Test: HLC winner = remote, user picks `--keep-local` → assert the project ends on the local remote and converges; document the LWW-vs-choice relationship in `spec/07`.

### P5-ARCH-01 — Apply/reconciliation is coupled to `*state.Tx`; only winner-selection is pure, blocking deterministic property testing

**Severity / Effort / Category:** P3 / M / architecture · testability · _new (enables `QUAL-02`)_

**Problem.** Best practice (and the project's own zero-knowledge direction) is for reconciliation to be a pure function of `(state, event) → decision`, decoupled from transport and storage, so convergence can be property-tested by replaying event permutations. Here `samePathLess`/`reconcileSamePath` *are* pure, but the dispatch (`applyEventTx`) interleaves reads (`ProjectByPath`, `TombstoneHLC`), policy, conflict inserts, and writes (`UpsertProject`, `TombstoneProject`, `RenameProject`) inside one DB transaction. There is no pure `Decide(state, event) → mutations` layer, so the convergence bugs above (`P5-SYNC-02`, `P5-SYNC-03`) shipped with green example tests.

**Evidence.** `internal/sync/events.go:340` `func applyEventTx(ctx, tx *state.Tx, event state.Event) error` — reads/decides/writes in one block; only `events.go:479-533` is pure. Tests in `internal/sync/apply_test.go` drive everything through `state.Open`.

**Recommendation.** Extract `Decide(projection, event) → ([]Mutation, []ConflictRecord)` that the transactional applier replays; then add property tests asserting any permutation of a fixed event set converges to the same projection and is idempotent under re-delivery.

**Actionable steps.**
1. Introduce a `Projection` value type and a pure `Decide`; move `ProjectByPath`/`TombstoneHLC` reads into the projection load.
2. Reduce `applyEventTx` to: load projection → `Decide` → persist mutations/conflicts.
3. Add a `rapid`/quickcheck property test over random event permutations (equal final projections + re-delivery idempotency); wire it into CI alongside `-race`.

**References:** [Reconciliation must be deterministic pure functions, separate from sync/transport](https://www.welcomedeveloper.com/posts/local-first-architecture-5-bidirectional-sync/).

---

## Cloud Hub, Backend & Scale

The PASS4 hub-correctness batch (HUB-09 conditional-put, HUB-10 retry/backoff/jitter, HUB-13 inclusive Pull, SEC-01 DeleteBlob) is implemented correctly and well unit-tested against the in-memory `memS3` double. But the backend is logic-only, and there are two new unbounded-growth gaps.

### P5-HUB-01 — The R2 backend is unwired logic: no AWS SDK, a dead `R2Config`, `FileHub` hardcoded — it has never made a real round-trip

**Severity / Effort / Category:** P2 / L / hub · architecture · _new (beyond `HUB-10`)_

**Problem.** The "Cloudflare R2 zero-knowledge backend" exists only as keying/retry/conditional-put logic talking to the `S3Client` *interface*. There is **no production `S3Client` implementation, no `aws-sdk-go-v2` in `go.mod`/`go.sum`**, the `R2Config` struct that would carry endpoint/bucket/credentials is **never instantiated**, and the CLI never constructs `R2Hub` — `sync` unconditionally builds `FileHub` and refuses to run without `--hub-file` (`"--hub-file is required until the production hub exists"`). So the production hub is unreachable end-to-end; `r2_test.go` exercises it solely against the in-memory double. There is also no runtime pluggability seam (`ARCH-03`, merged here): every call site hardcodes `dssync.FileHub{}` and `internal/hub` is imported by nothing in `cmd/` or `internal/cli`. The "pluggable Hub" thesis is real in type theory but not in practice.

**Evidence.** `internal/cli/sync.go:49` `hub := dssync.FileHub{Path: hubFile}`; `sync.go:28-29` the hard error; `internal/hub/r2.go:417` `type R2Config struct {` — only the definition, never instantiated; `rg aws` over `go.mod`/`go.sum` → empty; `internal/hub/r2.go:270` `// ErrNotImplemented signals an S3Client method that has no production wiring yet.`; `internal/cli/devices.go:150` second hardcoded `FileHub`.

**Recommendation.** Add an `aws-sdk-go-v2/service/s3` adapter implementing `S3Client` against an R2 endpoint (mapping S3 error codes to the existing typed errors), introduce a `hubFromOptions(opts) (Hub, error)` factory keyed on one config value, route `sync`/`run-loop`/`devices`/`blob_gc` through it, and add a hermetic MinIO/LocalStack integration test so the hub is exercised against a real object store at least once.

**Actionable steps.**
1. `internal/hub/s3client_awssdk.go`: wrap `s3.Client`; `PutObject` with `IfNoneMatch: aws.String("*")` → classify `412`/`PreconditionFailed`; map `429`/`503`→throttle, network→transient, `403`/`NoSuchKey`→terminal.
2. Add a `hub` config key (`hub: file:/path` or `hub: r2://<bucket>?endpoint=…`) and a `hubFromOptions` factory; replace the two hardcoded `FileHub{}` sites; keep `--hub-file` as an override but let no-arg `devstrap sync` resolve from config.
3. Use or delete `R2Config`; if kept, build `R2Hub.S3` from it and assert `PrefixScope` is enforced on every key.
4. Add a docker-compose MinIO (or LocalStack S3) integration test running `Push`/`Pull`/`PutBlob`/`GetBlob`/`DeleteBlob` through the real SDK, gated behind a build tag / `DEVSTRAP_HUB_S3_*` env so default CI stays hermetic.

### P5-HUB-02 — No hub-side reclamation of superseded blobs, and `draft_snapshots` are never pruned — so even local GC can't reclaim superseded drafts

**Severity / Effort / Category:** P2 / M / hub · storage · _new (relates `SEC-01`)_

**Problem.** `DeleteBlob` is invoked in exactly one place — `rewrapHubCleanup`, on the revoke path. There is no routine, command, or sync step that lists hub blobs and deletes those unreferenced by any current namespace event. Worse, the only blob GC (`gcUnreferencedBlobs`, run each sync) operates solely on the local `~/.devstrap/blobs` dir and treats a blob as referenced if it appears in `AllBlobRefs`, which UNIONs *every* row of `draft_snapshots` — and `RecordDraftSnapshot` only ever INSERTs new rows (repointing `current_snapshot_id` but never deleting superseded rows). So updating a draft (snapshot N+1) leaves snapshot N's row forever, keeping its blob "referenced" — local GC never reclaims superseded draft blobs, and the hub accumulates them indefinitely. `spec/19:159-161` even tells operators no R2 lifecycle rule is needed.

**Evidence.** `rg DeleteBlob -g'!*_test.go'` → only `internal/cli/blob_gc.go:147`; `gcUnreferencedBlobs` reads `filepath.Join(paths.Home, "blobs")` (`blob_gc.go:168`); `store.go AllBlobRefs` UNIONs all `draft_snapshots.blob_ref`; `RecordDraftSnapshot` (`store.go:1650+`) only INSERTs.

**Recommendation.** Add a retention-bounded prune of superseded `draft_snapshots` (keep current + N, or by age) and, when a row is pruned and its `blob_ref` becomes zero-referenced, delete the blob locally and on the hub. Add a `devstrap hub gc [--dry-run]` (or a sync-cycle step) that lists hub blob keys and deletes those unreferenced by any current binding/snapshot, gated by a snapshot horizon.

**Actionable steps.**
1. Add `PruneDraftSnapshots(retention)`; delete rows older than the horizon for each draft project.
2. Extend GC to delete orphaned hub blobs via `DeleteBlob`, guarded by `blobRefStillReferenced`.
3. Add `devstrap hub gc [--dry-run]` listing `ListObjectsV2(blobs/)` and deleting keys absent from `AllBlobRefs`.
4. Update `spec/19:159-161` (recommend an R2 lifecycle rule as a cost safety net) and add a coverage test.

### P5-HUB-03 — `R2Hub.Pull` never returns `ErrSnapshotRequired`; the retention-horizon half of the Hub contract is unimplemented

**Severity / Effort / Category:** P3 / M / hub · correctness · _new (relates `HUB-13`/`SYNC-02`)_

**Problem.** The `Hub` contract says `Pull` must return `ErrSnapshotRequired` when `afterHLC` falls below the retention horizon, forcing a full-state snapshot exchange. `FileHub` honors it (`RetentionHLC` + `ErrSnapshotRequired`), and the CLI branches on it — but `R2Hub.Pull` has no retention concept at all. Latent today (R2 never deletes events), but a silent correctness footgun the moment hub-side compaction (`HUB-11`/`P5-HUB-02`) lands: a stale device would get a partial event set and silently diverge instead of being told to snapshot.

**Evidence.** `internal/sync/hub.go:43-46` (contract); `hub.go:101-102` FileHub honors it; `internal/hub/r2.go:62-74` `R2Hub` has only `S3`/`WorkspaceID`/`Retry` — no `RetentionHLC`, and `r2.go:133-185` never returns `ErrSnapshotRequired`.

**Recommendation.** Give `R2Hub` a retention floor (a `workspaces/<id>/events/_floor` object holding the minimum retained HLC, or derive it from the smallest event prefix); `Pull` returns `ErrSnapshotRequired` when `afterHLC` is below it. Land this *with* the snapshot-bootstrap exchange.

**Actionable steps.**
1. Add a retention-floor source to `R2Hub`; compare `afterHLC` against it.
2. Return `dssync.ErrSnapshotRequired` (not a partial set) when below.
3. `r2_test`: set a floor above a stale cursor, assert `ErrSnapshotRequired`; document the `_floor` object in `doc.go`'s key scheme.

### P5-HUB-04 — `Pull` fetches the post-cursor log serially, one `GetObject` at a time, into memory; cold start is O(events)

**Severity / Effort / Category:** P3 / M / hub · performance · _new_

**Problem.** `R2Hub.Pull` issues one `GetObject` per event key, serially, accumulating the entire post-cursor result into a slice before sorting. For a brand-new device (`cursor=0`) syncing a workspace that accrued thousands of namespace events across hundreds of repos, this is thousands of serial Class-B round-trips plus full in-memory materialization before any apply — no bounded concurrency, no per-page streaming-apply, no snapshot bootstrap. (Idle-poll re-fetch cost is small — it scales with events at the single high-water HLC, not log size — so the concern is cold start specifically.)

**Evidence.** `internal/hub/r2.go:150-169` `for _, key := range keys { … h.S3.GetObject(ctx, key) … out = append(out, event) }` serially; sort at `r2.go:175-183` after the whole log is in memory.

**Recommendation.** Fetch page objects with bounded concurrency (an `errgroup` with a small limit, like `materializePass` already uses), apply per page rather than accumulating, introduce event-segment/snapshot objects so cold start reads O(segments), and make the incremental cursor exclusive (track the last applied composite key) so the boundary HLC isn't re-downloaded.

**Actionable steps.**
1. Wrap the per-key `GetObject` loop in a bounded `errgroup`.
2. Stream-apply each page (or return a channel) instead of one giant slice.
3. Persist the last-applied composite `(HLC, device, id)` cursor; ties into `P5-HUB-02`/`P5-HUB-03` segment/snapshot objects.

---

## CLI, UX & Developer Experience

The PASS4 command code is generally disciplined — typed exit codes, `appError`+remedies, child exit codes propagated as `100+N`, secrets scrubbed at the output boundary. The gaps are scriptability and a few new no-op/late-validation flaws.

### P5-CLI-01 — `--json` is a global persistent flag honored by a minority of commands; JSON is sprinkled ad hoc with no `Renderer`

**Severity / Effort / Category:** P2 / M / dx · scriptability · _carried-forward (2026-06-27 `CLI-01`), still open_

**Problem.** `--json` is registered as a persistent root flag (so it parses for every subcommand) but only ~11 leaf commands honor it (`status`, `scan`, `doctor`, `agent list/show`, `devices list`, `conflicts list/show`, `worktree status/list/unlock`) via copy-pasted inline `json.NewEncoder` blocks. The automation/eager-clone surface — `clone`, `materialize`, `hydrate`, `sync`, `run-loop`, `add`, `draft`, `env`, `db`, `init`, plus the mutating `devices`/`conflicts`/`worktree` subcommands — silently ignores `--json` and emits human text. For a product explicitly targeting agent runners and CI gating, a global flag that no-ops without error is a scripting footgun. (This restates the still-open 2026-06-27 `CLI-01`; re-surfaced because it directly blocks the agent-runner use case and the no-Renderer pattern is spreading with each batch.)

**Evidence.** `internal/cli/root.go:77` `cmd.PersistentFlags().BoolVar(&opts.json, "json", …)`; honored only via inline encoders in the commands above; `materialize.go:67-69` prints `"Materialized %d/%d projects\n"` with no JSON branch.

**Recommendation.** Introduce a single `Renderer` (`opts.render(stdout, humanFn, jsonValue)`) selected once from `opts`; route every command's terminal output through it (never `if json` inside business logic). For commands without a JSON shape yet, reject `--json` with `exitUsage` rather than silently ignoring it. Consider `--output text|json|yaml` per clig.dev with `--json` as an alias.

**Actionable steps.**
1. Add a `Renderer` helper taking a typed result + a human-render func.
2. Give `clone`/`materialize`/`hydrate`/`sync`/`run-loop`/`add`/`draft`/`env`/`db` structured result types routed through it.
3. Add a command-doc/golden test that runs `<cmd> --json` for every leaf command and asserts stdout parses as JSON (or returns a clear unsupported error).

**Example.**
```go
// internal/cli/render.go
func (o *options) render(w io.Writer, human func(io.Writer), v any) error {
    if o.json {
        enc := json.NewEncoder(w); enc.SetIndent("", "  "); return enc.Encode(v)
    }
    human(w); return nil
}
// materialize.go: build a typed result, then `return o.render(stdout, res.text, res)`.
```

**References:** [clig.dev / Go CLI best practices — `--output json` everywhere, results→stdout, progress→stderr, a Renderer not inline branches](https://www.nazarboyko.com/articles/building-production-cli-tools-in-go).

### P5-CLI-02 — `materialize --partial` is a dead flag: declared and advertised, never read

**Severity / Effort / Category:** P3 / S / dx · _new_

**Problem.** `materialize` exposes `--partial` (default true, "use partial clone with blob filtering"), but the variable is never consulted — `materializeGitRepo` hardcodes `hydrateProjectUnlocked(…, true)`. So `materialize --partial=false`, run by someone who wants full clones for reliable offline `git blame`/`log -p` (the exact GIT-06 caveat `doctor` warns about), silently performs a blobless clone. The flag lies; the only full-clone path is per-project `hydrate --full`. It slips past linters because `BoolVar` takes the variable's address.

**Evidence.** `materialize.go:42` `var partial bool`; `materialize.go:79` the flag registration; `materialize.go:153` `hydrateProjectUnlocked(ctx, store, opts, project, true)` (hardcoded); `rg partial materialize.go` shows the variable is never read.

**Recommendation.** Thread `partial` through `materializePass → materializeOne → materializeGitRepo → hydrateProjectUnlocked`, or remove the flag (and document `hydrate --full` as the escape hatch).

**Actionable steps.**
1. Add a `partial bool` parameter down the call chain; capture the flag in the `RunE` closure.
2. Add a testscript asserting `materialize --partial=false` produces a full clone, or delete the flag.

### P5-CLI-03 — `clone` validates `--open`/`--vscode` exclusivity only *after* the add + network clone completes

**Severity / Effort / Category:** P3 / S / dx · _new (`PROD-01`)_

**Problem.** The one-shot `clone` checks `--open`/`--vscode` mutual exclusivity inside the post-materialization block, so `devstrap clone <url> --open --vscode` performs the full `addProject` + skeleton write + eager blobless clone (a network op) and only then fails with `exitInvalidConfig`. Bad-flag combos should be rejected before expensive/side-effecting work. (The sibling `open` command validates at the top of `RunE`, so this is specific to `clone`, not systemic.)

**Evidence.** `clone.go:67-70` — the check sits after `materializeOne(...)` at `clone.go:61`; `rg MarkFlagsMutuallyExclusive internal/cli` → empty.

**Recommendation.** `cmd.MarkFlagsMutuallyExclusive("open", "vscode")` in `newCloneCommand` so cobra rejects the combo during flag parsing; map the resulting usage error to `exitUsage`.

### P5-CLI-04 — SSH host-alias parser ignores `Include`/`Match`/negation and `key=value` syntax, and re-reads `~/.ssh/config` per remote

**Severity / Effort / Category:** P3 / M / dx · correctness · _new (`GIT-05`)_

**Problem.** GIT-05 forge resolution depends on `resolveSSHHostAlias` to map `git@work-gitlab:org/repo` to the real host. The hand-rolled parser only understands top-level `Host`/`HostName` lines: it silently ignores `Include` (ubiquitous in modern configs, e.g. the macOS/1Password `Include ~/.ssh/config.d/*` pattern), `Match` blocks, and `key=value` (equals-delimited) syntax, and treats OpenSSH negation patterns (`!host`) as literals. So an alias whose `HostName` lives in an Included file never resolves and `DetectForge` falls back to the literal alias → `ForgeUnknown`. (Bounded impact: SSH-alias resolution is the *lowest*-precedence tier, with three documented overrides and a graceful manual-compare-URL fallback — hence P3.)

**Evidence.** `forge.go:146` opens the config every call; `forge.go:159-182` `strings.Fields` + a `switch key` handling only `host`/`hostname` (no `include`/`match`, no `=` splitting); `forge.go:194-201` `sshHostMatch` uses `filepath.Match` and only special-cases `*`.

**Recommendation.** Prefer shelling to `ssh -G <alias>` (authoritative — handles `Include`/`Match`/negation/tokens) and parse its `hostname` line; fall back to the manual parser only when `ssh` is unavailable. At minimum honor `Include` and negation, and memoize the parsed config for the duration of a command (so `doctor`'s per-remote loop doesn't re-read it).

**Actionable steps.**
1. Add an `ssh -G <alias>` fast path (sanitized env, timeout) reading the resolved `hostname`.
2. If keeping the manual parser, implement `Include` expansion and treat a `!pattern` match as a hard veto; consider `github.com/kevinburke/ssh_config`.
3. Parse `~/.ssh/config` once per process and memoize `alias→host`.

### P5-CLI-05 — Inconsistent stdout/stderr discipline: `run-loop` progress and `devices revoke` warnings go to stdout; `run-loop` swallows tick errors

**Severity / Effort / Category:** P3 / S / dx · _new_

**Problem.** `scan` deliberately routes diagnostics to stderr so result streams on stdout stay clean — but `run-loop` prints its per-tick header, `"run-loop stopped"`, and `"run-loop tick error: %v"` to **stdout**, and a tick error never sets a non-zero exit (the loop swallows it), so a scheduler/log consumer can't distinguish progress from results and never notices repeated failures. `devices revoke`/`lost` print `"warning: N secret value(s) must be rotated"`, `"Re-encrypted N blob(s)"`, and the (misleading — see `P5-PROD-02`) `--hub-file` note to stdout.

**Evidence.** `run_loop.go:46,58,63,78` (stdout); `devices.go:137,154,161` (stdout).

**Recommendation.** Route progress/warnings/notes to `cmd.ErrOrStderr()`; keep stdout for results. For `run-loop`, track consecutive tick failures and exit non-zero past a threshold so a scheduler notices.

### P5-DX-01 — No dynamic shell completion for namespace-path arguments or enum flags

**Severity / Effort / Category:** P3 / M / dx · _new feature (relates `PROD-05`)_

**Problem.** Cobra auto-provides a `completion` command, but DevStrap wires no dynamic completion: no `ValidArgsFunction` on the many commands taking a `<path>` argument (`open`/`hydrate`/`materialize`/`worktree new`/`env capture`/`agent run`/`draft snapshot`/`conflicts`), which is the single highest-value completion for a managed-namespace tool — the whole pitch is that the same paths exist everywhere, so tab-completing them from the local DB would make the CLI feel native. Enum flags (`--lfs-policy`, `--forge`, `--policy`) have no `RegisterFlagCompletionFunc` either.

**Evidence.** `rg 'ValidArgsFunction|RegisterFlagCompletionFunc|ValidArgs' internal/cli cmd` → empty; path args accepted as raw strings (e.g. `worktree.go:122 store.ProjectByPath(args[0])`); enum flags validated only at runtime.

**Recommendation.** Add a shared `completePaths(opts)` `cobra.CompletionFunc` that lists namespace paths from the store; attach it via `ValidArgsFunction` on path-taking commands; `RegisterFlagCompletionFunc` the fixed enum sets. PROD-05's distribution then ships the generated completion scripts.

**Actionable steps.**
1. Implement `completePaths(opts)` opening the store and returning project paths.
2. Attach to `open`/`hydrate`/`materialize`/`worktree new`/`env capture`/`agent run`/`draft snapshot`.
3. Register static completion for `--lfs-policy`/`--forge`/`--policy`; add a `__complete` smoke test.

**References:** [Cobra dynamic completions](https://cobra.dev/docs/explanations/enterprise-guide/).

---

## Code Quality, Testing & CI

CI is genuinely good for a solo OSS project: a macOS+Linux matrix with `-race`, `go vet`, gofmt gating, module hygiene, `govulncheck`, gosec with no allowlist, and SHA-pinned actions. The issues are in the just-landed EAGER/DRAFT batch and a couple of CI follow-throughs.

### P5-QUAL-01 — `materialize` counts the normal "no draft bundle yet" state as a hard failure, making the QUAL-03 exit code perpetually non-zero

**Severity / Effort / Category:** P2 / M / quality · reliability · _new (`QUAL-03`)_

**Problem.** The just-landed QUAL-03 exit-code logic treats the *expected interim* state of `local_git`/`draft_project` projects (no synced bundle yet) as a materialization failure: `materializeDraft` returns a non-nil error when no bundle exists, `materializePass` increments `res.failed`, and the command returns `appError{code: exitGeneric}` whenever `failed > 0`. Because `SkeletonProjects` returns all project types in skeleton state, any device that *received* a `local_git`/`draft` project via sync — or an explicit `devstrap materialize <path>` — flips the whole command to a non-zero exit and prints "failed", re-breaking the exact CI/cron gating QUAL-03 was meant to enable.

**Evidence.** `materialize.go:206` `return fmt.Errorf("%s is %s; content sync not yet materialized (no draft bundle synced)", …)`; `materialize.go:111-114` `res.failed++ … return nil` for any non-nil error; `materialize.go:74` `appError{code: exitGeneric, …}` when `failed > 0`; `internal/sync/events.go:472` hardcodes skeleton state for all applied types.

**Recommendation.** Distinguish "pending / not-yet-materializable" from "failed". Return a typed sentinel (`ErrDraftNotMaterializable`) from `materializeDraft`; have `materializePass` classify it as *skipped* (not failed), not log a warning, and not flip the exit code. Report succeeded/skipped/failed counts. Optionally record the `draft_snapshots` row on local creation so the creating device's row exists pre-sync.

**Actionable steps.**
1. Add a `skipped int` to `materializeResult` and a `var ErrDraftNotMaterializable = errors.New(...)` sentinel.
2. In `materializePass`, `errors.Is(err, ErrDraftNotMaterializable)` → increment `skipped`, no warning.
3. Return `exitGeneric` only when `res.failed > 0`; print succeeded/skipped/failed.
4. Testscript: `devstrap materialize` exits 0 for a workspace whose only non-git project is an un-snapshotted draft.

### P5-QUAL-02 — `run-loop` (XP-02) and the `draft` snapshot CLI + cross-device round-trip (DRAFT-02) have zero test coverage

**Severity / Effort / Category:** P2 / M / quality · testing · _new (`QUAL-04`)_

**Problem.** Two of the newest user-facing surfaces are completely untested. `run-loop` (the portable daemonless convergence loop billed as the XP-02 path) has no unit test and no testscript — `runLoopForever`'s ticker/jitter/graceful-shutdown and `runLoopTick` error handling are never exercised (and hide the `P5-QUAL-03` panic). The `draft snapshot create` command and the full DRAFT-02 story (`Pack` → `pushReferencedBlobs` → `Pull` → `extractDraftBundle`, with dual-copy conflict handling) is not covered end-to-end — only the lower-level `draftbundle.Pack`/`Extract` units exist.

**Evidence.** `rg 'run-loop|runLoop' --include=*_test.go --include=*.txtar` → empty; `rg 'newDraftCommand|draft snapshot' --include=*_test.go --include=*.txtar` → empty; `testdata/script/` has no `run-loop`/`draft` script.

**Recommendation.** Add an in-process cobra unit test for `runLoopForever` (short interval + cancelled context → one-tick-then-stop), and a testscript exercising `devstrap draft snapshot create` on two devices through the file hub, asserting the draft materializes on device B with the dual-copy conflict file on overlap. (Note: the "extend the vacuous-test guard" idea doesn't catch this — `internal/cli` already has tests; only coverage thresholds would.)

**Actionable steps.**
1. `internal/cli/run_loop_test.go`: `runLoopForever` with `interval=10ms` + context cancelled after the first tick.
2. `testdata/script/draft_roundtrip.txtar`: A `add` a plain folder, `draft snapshot create`, `sync --hub-file`; B `sync` and assert materialization + dual-copy on overlap.
3. Add coverage thresholds (see `P5-QUAL-04`) so new command files can't ship untested.

### P5-QUAL-03 — `run-loop` jitter computation panics for sub-10ns intervals (`rand.Int64N(0)`)

**Severity / Effort / Category:** P3 / S / quality · reliability · _new_

**Problem.** `runLoopForever` guards only `interval <= 0`, then computes `rand.Int64N(int64(interval) / 10)`. For any interval in `[1ns, 9ns]` the divisor is 0, and `math/rand/v2.Int64N` panics on `n<=0`, crashing the unattended loop with an unrecovered panic. Input is unrealistic (`--interval 5ns`), but it's a real latent crash with no `recover` and no test in a command meant to run under a scheduler.

**Evidence.** `run_loop.go:51` `if interval <= 0 { interval = 5 * time.Minute }`; `run_loop.go:69` `jitter := time.Duration(rand.Int64N(int64(interval) / 10))`; import `math/rand/v2` at `run_loop.go:7`.

**Recommendation.** Clamp the jitter bound to at least 1: `bound := int64(interval)/10; if bound < 1 { bound = 1 }`. Add a unit test with a tiny interval asserting no panic.

### P5-QUAL-04 — CI computes a coverage profile on every test run but no step consumes it

**Severity / Effort / Category:** P3 / S / quality · ci · _new (`QUAL-04`)_

**Problem.** The test job runs `go test -race -covermode=atomic -coverprofile=coverage.out ./...`, but no step uploads, reports, or gates on `coverage.out` — it's written and discarded. There's neither coverage visibility (no badge/PR comment) nor a regression gate, so under-tested new code (`run-loop`, `draft`, `materialize` variants — `P5-QUAL-02`) keeps dropping coverage silently. (Instrumentation cost is negligible since `-race` already uses atomic counters — the gap is visibility/gating, not wasted CPU.)

**Evidence.** `.github/workflows/ci.yml:76` the coverage line; a repo-wide grep finds `coverage`/`codecov` only there — no upload/threshold/artifact step, no `codecov.yml`.

**Recommendation.** Either consume the profile (a `go tool cover -func` floor gate, or upload to a coverage service / artifact for trend visibility) or drop `-coverprofile` so instrumentation isn't wasted.

**Actionable steps.**
1. Add a step: `go tool cover -func=coverage.out` failing below an agreed floor; or upload `coverage.out`.
2. If neither is wanted yet, remove `-covermode/-coverprofile`.

### P5-QUAL-05 — Draft bundles silently drop empty directories on the cross-device round-trip

**Severity / Effort / Category:** P3 / S / quality · fidelity · _new_

**Problem.** `draftbundle.Pack` walks the project and writes only `tar.TypeReg` headers; the directory branch returns nil. On `Extract`, directories are only recreated as parents of files (`os.MkdirAll(filepath.Dir(target))`). So any empty directory in a draft (a placeholder `logs/`, `tmp/`, scaffold dir) is lost when materialized on another device. For opaque drafts DRAFT-02 is meant to reproduce faithfully, this is a silent fidelity loss. (`Extract` already has a `tar.TypeDir` case — it's just dead for Pack-authored bundles.)

**Evidence.** `internal/draftbundle/draftbundle.go:108-117` dir branch returns nil without writing; `:137-143` header is `Typeflag: tar.TypeReg` only; `:253-258` `Extract` handles `tar.TypeDir` but Pack never emits it.

**Recommendation.** Emit a `tar.TypeDir` header for each non-pruned directory during the Pack walk (respecting the ignore matcher) and count it against `MaxFiles` to keep the bomb guard (`P5-SEC-02`) sound.

**Actionable steps.**
1. In Pack's dir branch, after prune checks, write a `tar.TypeDir` header (`relSlash+"/"`, mode `0o750`) and increment `fileCount`.
2. Verify empty-dir round-trip with a unit test.

---

## Product, Architecture & New Features

The core loop (`init → scan → clone/add → sync → open → worktree → agent`) is coherent and largely shipped. The findings below are partial-wiring artifacts of the batch plus two concrete new features.

### P5-PROD-01 — `deriveDisplayStatus` reads materialization states no writer ever produces — the flagship "ready" status is unreachable dead code

**Severity / Effort / Category:** P3 / M / product · _new (`PROD-01`)_

**Problem.** The PROD-01 fix added `deriveDisplayStatus` mapping the materialization+dirty tuple to a user label, but it branches on `materialization == "hydrated"`/`"hydrating"` — values **no code path writes**. The only states ever stored are `"skeleton"`, `"available"`, `"failed"`, and `"materialized-empty"`. So the product's headline readiness state (invariant #7) is unreachable; repos display correct labels otherwise, so the harm is a dead branch + an unattainable "ready", not wrong output — hence P3, partly downstream of the deferred `env_ready`/`tooling_ready` features.

**Evidence.** `internal/cli/status.go:80-91` (`"hydrated"`→"ready", `"hydrating"`→"hydrating"); writers only set `"available"`/`"failed"`/`"materialized-empty"`/`"skeleton"` (`hydrate.go:112`, `materialize.go:212,228`, `add.go`, scan).

**Recommendation.** Make renderer and writers agree: either have the materialize path write `"hydrated"`/`"hydrating"` and wire `env_ready`/`tooling_ready` (as the `status.go:73-74` comment anticipates), or delete the unreachable branches and mark "ready" explicitly deferred. Add a table-driven test enumerating producible `(materialization, dirty)` pairs.

### P5-PROD-02 — Default (local-only) device revoke permanently strands old ciphertext on the hub; the printed note falsely promises `sync` will clean it

**Severity / Effort / Category:** P3 / M / product · _new (`SEC-01`)_

**Problem.** The SEC-01 fix deletes superseded hub ciphertext only when `--hub-file` is passed to `devices revoke`. That flag is optional and usually omitted, so the common path is local-only rewrap: local refs repoint to new refs, the old ciphertext stays on the hub, and the command prints `"note: --hub-file not set; old ciphertext remains on the hub until the next sync rewraps with --hub-file"` — but `runSyncCycle` never rewraps (it only calls local-cache `gcUnreferencedBlobs`). So the promised cleanup never happens. (Mitigated: an immediately-preceding *mandatory* warning tells the user to rotate the secret at its source, which is the real protection — hence P3.)

**Evidence.** `devices.go:160-163` the misleading note; `sync.go` `runSyncCycle` never calls `rewrapBlobsOnRevoke` (only `gcUnreferencedBlobs` at `sync.go:132`); `blob_gc.go:156-196` operates on the local cache dir, never `hub.DeleteBlob`.

**Recommendation.** Make the promised cleanup real or stop promising it. Either (a) record orphaned old refs in a `pending_hub_deletes` table on local-only rewrap and drain it on the next `sync --hub-file` (`DeleteBlob`), or (b) require a configured hub on revoke and correct the note to state plainly that local-only revoke leaves ciphertext until rotation. (Folds into `P5-SEC-01`/envelope-encryption.)

**Actionable steps.**
1. Persist orphaned old refs (a `pending_hub_deletes` queue) instead of dropping them.
2. Drain the queue against the hub in `runSyncCycle` when a hub is available.
3. Fix the `devices.go` note to describe actual behavior; test revoke-without-`--hub-file` then `sync --hub-file` deletes the old blob.

### P5-PROD-03 — `needs_rotation` is a one-way latch with no clear path and no rotation workflow

**Severity / Effort / Category:** P3 / M / product · _new feature_

**Problem.** Device revoke flags every encrypted secret binding `needs_rotation = 1`, and `doctor` surfaces the count with remedy "rotate at source." But no store method or command ever clears the flag, and none drives the rotation. So once any device is revoked, `doctor` warns forever — even after the operator dutifully rotates and re-captures every secret — training users to ignore it; and the product tells the user to rotate but offers no guided path. (The remedy text is *correct* — you regenerate at the provider — but never says that re-running `devstrap env capture` clears the warning; that's the real, smaller gap.)

**Evidence.** `store.go:1583` `MarkEncryptedBindingsNeedingRotation` (set=1); `store.go:1602` count; `rg 'ClearRotation|ResetRotation'` → none; `doctor.go:165-166` the remedy; `env.go` has only capture/hydrate/bind.

**Recommendation.** Add a guided rotation workflow and a flag-clearing path: `devstrap env rotate <path> [env-file] [--all]` that re-captures+re-encrypts the binding's value to the current recipient set and clears `needs_rotation`; add `Store.ClearBindingRotation`; point `doctor`'s remedy at the new command so it naturally returns to OK.

**Actionable steps.**
1. Add `Store.ClearBindingRotation(ctx, bindingID)` + a bulk variant.
2. Implement `devstrap env rotate`; clear the flag on re-capture.
3. Update `doctor` remedy text; test revoke→warn→`env rotate`→OK.

### P5-PROD-04 — README omits the shipped headline `devstrap clone` and still lists it as future roadmap

**Severity / Effort / Category:** P3 / S / docs · _new (`PROD-01`)_

**Problem.** PROD-01 shipped `clone` (`clone.go` wired in `root.go`; `spec/13` documents it), and PROD-01 explicitly said to make it the headline README command. But the README never documents it: absent from the command-reference table and the quickstart (which still onboards via `add`+`hydrate`), and the only mention lists `devstrap clone` among *future* items. So the intended one-command onboarding is invisible and actively miscategorized as unbuilt.

**Evidence.** `README.md:186-207` table has no `clone` row; `README.md:164-166` quickstart uses `add`+`hydrate`; `README.md:241` lists clone as a near-term *priority to grow the product surface*; contrast `spec/13:54,232-239` (Implemented + documented).

**Recommendation.** Add a `devstrap clone` row to the README table, make it the headline of the quickstart (replacing `add`+`hydrate`+`open` for the new-repo case), remove it from the "future" sentence, and add `--help` examples mirroring `spec/13:235-236` (the command currently has no cobra `Long`/`Example`).

### P5-PROD-05 — New feature: a hub-health probe (`doctor --remote`) and a live convergence view (`status --watch` / TUI)

**Severity / Effort / Category:** P3 / L / product · _new feature (relates `HUB-14`, `PROD-02`)_

**Problem.** DevStrap's wedge is a multi-machine fleet where the same `~/Code` converges everywhere — but there's no way to *see* convergence or remote health. `doctor` checks only local prerequisites (it has `--fix` but no `--remote`): no hub reachability, cursor lag, pending-push backlog, missing-blob count, or device-trust summary. `status` is a one-shot with no `--watch`; during eager `sync` (which can blobless-clone many repos) the user gets no live progress. The repo ships an `icon.png` and `spec/02:199` defers a "TUI status view," signalling a planned dashboard. Comparable tools make this a first-class surface (`mutagen sync monitor`, Syncthing's web UI).

**Evidence.** `doctor.go:69` registers only `--fix`; `runDoctorChecks` is entirely local; `status.go:14-69` is single-shot, no `--watch`; `spec/02_PRODUCT_REQUIREMENTS.md:199` "TUI status view" deferred; `icon.png` present.

**Recommendation.** Ship two thin observability features on the existing event log + cursors: (1) `devstrap doctor --remote` reporting hub reachability, local-vs-hub cursor lag (pending pull), pending-push backlog (events > push cursor), missing-blob count, and device-trust summary; (2) `devstrap status --watch [--interval]` (later a Bubble Tea TUI) re-rendering per-project readiness, open conflicts, and worktree state. Surface `materialize` progress during long `sync` passes. This also delivers the `HUB-14` "expose hub metrics via status/doctor" recommendation.

**Actionable steps.**
1. `doctor --remote`: load the hub via the `P5-HUB-01` factory, ping it, report cursor lag / pending push / missing blobs / device trust.
2. `status --watch`: loop the existing Summary + open-conflicts + worktree queries, clear/redraw at an interval.
3. Stream `succeeded/total` during `sync` so long clone passes show motion.
4. Plan a Bubble Tea TUI once the watch view proves the data model.

**References:** [Mutagen `sync monitor`/`list`](https://mutagen.io/documentation/synchronization); [DevPod — client-only, IDE-integrated dev environments](https://devpod.sh/).

---

## Specs, Data Model & Process Hygiene

The data model itself is robust (FK enforcement, single-writer pool, deterministic HLC, backup validation, idempotent event insert, correct cross-device `workspace_id` re-stamping). The weakness is documentation *truth* and the gate's inability to detect drift.

### P5-DX-02 — The spec-drift gate only path-maps and substring-matches; it is structurally blind to prose staleness

**Severity / Effort / Category:** P2 / M / process · ci · _new (relates `QUAL-04`)_

**Problem.** CI gives false confidence about spec accuracy. The gate enforces only (a) that for every changed code file, at least one *path-mapped* spec file is also in the changed set (file *touched*, content never inspected), and (b) that every cobra command name appears as a raw *substring* somewhere in `spec/13`. Because `spec/00` maps to `cmd/**`, `internal/**` (very broad), any incidental edit to `spec/00` satisfies (a) without anyone verifying the stale inventory/contradictions were fixed. The command check is satisfied by common English substrings (`run`, `add`, `open`, `down`, `list`, `show`, `new`). This is exactly why `P5-DOC-01`/`P5-DOC-02`/`P5-DATA-01` shipped with green CI.

**Evidence.** `internal/specdrift/specdrift.go:72-83` maps changed file → tracking and fails only `if !anyChanged(mapped, changedSet)` (content never inspected); `internal/cli/command_doc_test.go:33` `if !strings.Contains(spec, name)` (substring over the whole text), only against `spec/13`.

**Recommendation.** Make the gate detect semantic drift: (1) auto-generate the "Current state"/command inventory and migration list from the binary + `migrations/` dir, and have CI diff the generated block against the committed spec (fail on mismatch); (2) replace the substring command check with an anchored check (a `### ` + name backtick heading) run against both `spec/00` and `spec/13`.

**Actionable steps.**
1. Add `devstrap docs gen` (or a test) emitting the command tree + embedded migration filenames; CI fails on a diff against the committed block.
2. Tighten `command_doc_test.go` to require an anchored heading/code-fence per command; extend to `spec/00`.
3. Note in `AGENTS.md` that the gate proves a spec was *touched*, not that it is *correct*.

### P5-DOC-01 — `spec/07` (re-stamped `last_reviewed: 2026-06-29`) still says shipped DRAFT/HUB features are "not yet implemented" — one claim is flatly false

**Severity / Effort / Category:** P3 / M / docs · _new_

**Problem.** The Namespace & Sync Model spec — the canonical reference AGENTS.md tells contributors to read before changing behavior — still describes PR-#16 DRAFT/HUB features as future work. The most damning line asserts code that demonstrably exists does not: `spec/07:510` "This flow is specified but **not yet implemented** (no bundle/snapshot code exists today, `NOVCS-02`)." A reader (human or agent) trusting this concludes the draft-bundle plane is unbuilt and either re-implements it or skips wiring it.

**Evidence.** `spec/07:510` (the false claim) vs `internal/draftbundle/draftbundle.go` (`Pack`, `Extract`, `ExtractWithLimits`) wired at `internal/cli/draft.go:72` and `state/store.go:1650`; also `spec/07:110-114,275,299,555` stale.

**Recommendation.** Rewrite `spec/07`'s status lines to match shipped code: delete "no bundle/snapshot code exists today"; flip the `local_git`/`draft_project` rows and lines 299/510/555 to "shipped," noting only the network-hub/full-snapshot pieces remain deferred. Re-bump `last_reviewed` only after the prose matches code. (Best fixed durably by `P5-DX-02`.)

### P5-DOC-02 — `spec/00`'s "Not implemented yet" section is self-contradictory and its command inventory omits 7 shipped commands

**Severity / Effort / Category:** P3 / M / docs · _new_

**Problem.** `spec/00` is the START_HERE doc CLAUDE.md/AGENTS.md require everyone to read first. Its "Not implemented yet:" section contains bullets that explicitly say the features **are** shipped (lines 165, 167 — "are now shipped (`DRAFT-*`)", "are shipped"), so a reader can't tell what's built. Separately, the one-line command inventory at `spec/00:131` omits `clone`, `materialize`, `draft`, `run-loop`, `conflicts`, `devices`(subcommands), `worktree unlock`, and the "Last validated" date predates `be664ba`.

**Evidence.** `spec/00:158` header vs `:165,167` contradicting bullets; `:131` inventory; "Last validated: 2026-06-28".

**Recommendation.** Split `spec/00`'s status into clean "Shipped" vs "Not yet built"; move the contradicting bullets to the shipped list; add the 7 missing commands (or generate the inventory — `P5-DX-02`); bump "Last validated."

### P5-DATA-01 — `spec/12`'s migration & index inventory is stale: omits 00010 and reserves the 00010 number for a different (unbuilt) migration

**Severity / Effort / Category:** P3 / S / docs · data-model · _new_

**Problem.** The SQLite Data Model spec (`last_reviewed: 2026-06-29`) documents the migration set ending at 00009 and reserves the 00010 slot for a future `gitstate` migration — but `00010_repo_forge_kind.sql` already occupies it. Anyone following the spec to add the next migration picks a colliding number; the index list also omits two shipped indexes. (Self-correcting — Goose rejects duplicate versions — hence P3, but it's the data-model authority.)

**Evidence.** `internal/state/migrations/00010_repo_forge_kind.sql` exists; `spec/12:455-463,477-485` lists only 00001..00009; `spec/12:184` reserves `00010_gitstate_mirror.sql`.

**Recommendation.** Add `00010_repo_forge_kind.sql` to both lists, renumber the planned gitstate migration to `00011`, add the two missing indexes (`idx_hub_cursors_workspace`, `idx_draft_snapshots_namespace`). Generate the list from the embedded `migrations/` dir to prevent recurrence (`P5-DX-02`).

### P5-DATA-02 — `draft_snapshots` idempotency is enforced only in app code, not by a DB constraint

**Severity / Effort / Category:** P3 / S / data-model · defense-in-depth · _new_

**Problem.** Re-applying the same `draft.snapshot.created` event must not create a duplicate snapshot. This is guaranteed only by a SELECT-then-INSERT guard in Go; the table has no `UNIQUE(namespace_id, source_event_id)` and `source_event_id` is nullable — diverging from how the `events` table protects idempotency (PK + `INSERT OR IGNORE`). Today the only writer is gated upstream by the events PK, so it's strictly defense-in-depth, but any future writer (or relaxation of the single-writer pool) could silently duplicate rows and mis-point `current_snapshot_id`.

**Evidence.** `00009_draft_snapshots.sql:13` `source_event_id TEXT` (nullable), `grep -c UNIQUE` = 0; idempotency only in `store.go:1117-1123,1660-1666` (SELECT-then-INSERT).

**Recommendation.** Add a partial unique index in a new migration and switch the inserts to `INSERT OR IGNORE` so the DB is the idempotency authority.

**Example.**
```sql
-- 00011_draft_snapshot_idempotency.sql
CREATE UNIQUE INDEX idx_draft_snapshots_source_event
  ON draft_snapshots(namespace_id, source_event_id)
  WHERE source_event_id IS NOT NULL;
```

### P5-PROC-01 — Audit-file sprawl + finding-ID collisions + a 1,103-line work log, with no single source of truth for "what's done"

**Severity / Effort / Category:** P3 / M / process · _new_

**Problem.** Traceability is degrading. Four `AUDIT_RECOMMENDATIONS*.md` files (6,525 lines total) sit at the repo root, finding IDs collide across them (`GIT-01` denotes a repo-lock bug in the 2026-06-27 pass and an empty-checkout bug in PASS4), and the only running ledger is a 1,103-line append-only work log. There's no consolidated, queryable status of which findings are open/shipped, so a reviewer must read ~620 KB of prose to learn current state, and an ID like "GIT-01" is ambiguous in PR discussion.

**Evidence.** Root: `AUDIT_RECOMMENDATIONS.md` (2112), `_2026-06-27.md` (2155), `_2026-06-28.md` (617), `_2026-06-28_PASS4.md` (1641). Collision: `_2026-06-27.md:744` `[GIT-01] Repo lock can be reclaimed…` vs `_2026-06-28_PASS4.md:935` `GIT-01 — Eager materialization…`. `spec/18_WORK_LOG.md` is 1,103 lines.

**Recommendation.** Adopt one source of truth: either GitHub Issues (one per finding, labeled by pass) or a single committed `STATUS.md`/`findings.csv` keyed by globally-unique IDs (pass-prefixed, like this pass's `P5-`). Archive the historical audit files under `docs/audits/` and stop reusing ID namespaces. Cap/rotate the work log (move older cycles to a dated archive). _This pass already adopts the `P5-` prefix as a first step._

**Actionable steps.**
1. Create a single tracker (Issues or `STATUS.md`) with one row per finding: id, pass, severity, status (open/shipped), PR.
2. Make finding IDs pass-scoped/unique going forward.
3. Move `AUDIT_RECOMMENDATIONS*.md` into `docs/audits/`; rotate `spec/18_WORK_LOG.md` into a dated archive.

---

## Appendix A — Carried-forward PASS4 open items (re-prioritized)

These remain valid and are **not** superseded; several gain urgency in light of the new findings. (Full text in `AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md`.)

| PASS4 ID | Item | Note from this pass |
|---|---|---|
| `SEC-07` | Envelope encryption (KEK/DEK) + key rotation/forward secrecy | **Promote toward P1-adjacent.** It is the structural fix for `P5-SEC-01`/`P5-SEC-04`/`P5-PROD-02`; the just-landed rewrap design proved per-blob re-encryption is incompatible with content-addressed immutable blobs. |
| `SEC-02` | Encrypt the namespace map at rest on R2 | Still open and foundational; do before `P5-HUB-01` makes R2 live. |
| `SEC-04` | Close the bootstrap window (fail-closed enrollment) | Still open; `EnsureRemoteDeviceTx` accepts unenrolled-peer events during bootstrap. |
| `SEC-05` | Sign releases (cosign/SLSA/SBOM) + pin `goreleaser-action` to a SHA | Cheap immediate sub-step: pin `goreleaser/goreleaser-action@v7` to a full SHA like every other action (it's the one unpinned action, in a `contents: write` job). |
| `SYNC-02` / `HUB-11` | Event-log compaction + snapshot exchange | Hard precondition for `P5-HUB-02`/`P5-HUB-03`; `ErrSnapshotRequired` is still dead-ended. |
| `SYNC-03` | Raise `epochFloorMS` above 0; past-direction quarantine | Still `0`; revisit with the test-HLC refactor `P5-ARCH-01` enables. |
| `SYNC-05` / `SYNC-06` | Folded hash chain + signed head; tombstone GC | Open. |
| `SYNC-07` | `MaxOpenConns(1)` serializes WAL reads | Open. |
| `HUB-12` / `HUB-14` / `HUB-15` / `HUB-16` | Delete path (done) / metrics / quotas / versioning+backup | `HUB-14` is partly delivered by `P5-PROD-05` (`doctor --remote`). The rest open; sequence after `P5-HUB-01`. |
| `GIT-03` / `GIT-04` / `GIT-07` | OS-enforced agent sandbox / worktree GC / materialize resume+progress | Open; `P5-SEC-03` is a cheap interim env-sanitization step toward `GIT-03`. |
| `QUAL-02` / `QUAL-04` / `QUAL-05` / `QUAL-07` | Property tests / coverage gate + Windows CI / SBOM+provenance / leak linters | `P5-ARCH-01` unblocks `QUAL-02`; `P5-QUAL-04` is the concrete coverage-gate sub-step. |
| `PROD-04` / `PROD-05` | `service install` daemon / Homebrew tap + `curl\|sh` + completions | Open; `P5-DX-01` (dynamic completions) is a prerequisite for the `PROD-05` completion scripts. |

## Appendix B — New feature catalog

Concrete features that advance the Workspace Passport thesis, grounded in the architecture and competitors:

1. **`devstrap doctor --remote` + `status --watch`/TUI** (`P5-PROD-05`) — the fleet-convergence visibility the product currently lacks. (cf. `mutagen sync monitor`, Syncthing web UI.)
2. **`devstrap env rotate`** (`P5-PROD-03`) — close the secret-rotation loop so `doctor` can return to green after a revoke.
3. **`devstrap hub gc [--dry-run]`** (`P5-HUB-02`) — operator-facing reclamation of orphaned hub blobs.
4. **`hubFromOptions` + real R2 backend + MinIO integration test** (`P5-HUB-01`) — turns the "pluggable Hub" from type theory into a config flip.
5. **Dynamic shell completion of namespace paths** (`P5-DX-01`) — makes a managed-namespace CLI feel native; the highest-leverage DX win.
6. **Deeper ecosystem integrations** (future) — emit a `.envrc`/`mise.toml` shim on `env hydrate` so `direnv`/`mise` pick up DevStrap-managed profiles automatically; a `devcontainer.json`-aware `open` so `devstrap` complements rather than competes with devpod/Codespaces ("if you can `git clone` it, you can `devstrap` it"). These position DevStrap as the *namespace + secrets* layer beneath existing dev-environment tools rather than a replacement for them.

---

_Produced by a verification-driven multi-agent audit of trunk `be664ba`: 7 dimension reviewers → adversarial per-finding verification against the live code → consolidation. 43 candidate findings, 41 verified, 36 reported after merging overlaps._
