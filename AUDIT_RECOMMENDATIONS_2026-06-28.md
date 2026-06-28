# DevStrap — Cloud-Sync Architecture Audit (Third Pass)

**Date:** 2026-06-28
**Auditor:** Automated architecture review (cloud-sync workstream definition + adversarial verification against the live tree)
**Scope:** `spec/` design corpus + Go codebase (`cmd/`, `internal/`) at commit `e982c05`, focused on the "Dropbox experience for code" cloud-sync direction.

## Table of Contents

- How this relates to the prior audits
- Executive Summary
- Architecture decisions encoded by this audit (the substance)
- Priority Matrix — P0 list + full matrix
- **Section 1** — Eager-clone materialization (`EAGER-01..04`) — P0
- **Section 2** — Non-git content sync (`DRAFT-01..05`) — P0
- **Section 3** — Cloud Hub backend (`HUB-01..05`) — P1
- **Section 4** — Cross-platform hardening (`XP-01..04`) — P1
- **Section 5** — Scaling to multi-user (`SCALE-01..05`) — future, P2
- **Section 6** — Deferred (explicit): FUSE/StrapFS, native installers, production HTTP/SSE hub

## How this relates to the prior audits

The first audit (`AUDIT_RECOMMENDATIONS.md`, 58 findings) and the second-pass audit (`AUDIT_RECOMMENDATIONS_2026-06-27.md`, 65 cross-dimension findings + `NOVCS-*`/`FORGE-*`/working-state/hub-architecture sections) are both **largely closed**: the git-injection RCE, the stale-base re-check, the HLC skew guard, the dead redaction layer, the `local_git`/`plain_folder` scan classification, the `AgentAllowlist` that drops `SSH_AUTH_SOCK`/`HOME`, the `mustVerifyEvent` destructive-event gate, and the forge-detection groundwork are now in the tree (commit `e982c05`).

This third pass does **not** rehash those. It **extends** the second-pass audit (build on it, do not revert it) by converting the cloud-sync direction set out on 2026-06-28 into concrete, ID-stamped workstreams that drive the eventual build:

- `EAGER-*` — eager-clone materialization (make `devstrap sync` actually populate `~/Code`).
- `DRAFT-*` — non-git content sync (`.devstrapignore` compiler, encrypted draft bundles, draft limits, non-git hydrate).
- `HUB-*` — cloud hub backend (pluggable `Hub` interface, Cloudflare R2 zero-knowledge backend, fail-closed verification, revoke re-encryption, blob GC).
- `XP-*` — cross-platform hardening (Ubuntu parity, portable periodic scan/sync loop without a native daemon).
- `SCALE-*` — multi-user scaling (control/data-plane split, R2 per-`workspace_id`, microVM runner sandboxes, tenancy spectrum) — **documented as direction, not committed work**.

> Method: each workstream was grounded against the live code (`file:line` evidence), cross-checked against the second-pass findings it inherits, and stated as problem → current code state → recommended fix → reuse pointer → sequencing.

> **Out of scope for these docs (decision #9):** which LLM / Claude API the agent runner calls is a separate concern. No LLM-provider content belongs in any spec or in this audit.

---

## Executive Summary

DevStrap's product goal is the **"Dropbox experience for code"**: one identical `~/Code` structure that appears automatically on every device in the owner's fleet (the two Mac Minis, the incoming GMKtec Ubuntu box, the graphics laptop, the NAS). The second-pass audit proved the *namespace* half of that loop works — `add`/`scan --adopt` stamp signed HLC-ordered events, `sync --hub-file` pushes and applies them — but the *materialization* half is missing: after `sync`, the other device has namespace rows and empty skeletons, and the human is told so verbatim (`internal/cli/sync.go:54`: *"hydration/fetch reconciliation is not implemented yet"*). The product promise is one `sync` away from being demonstrable and one `sync` short of being real.

The single most important architectural commitment in this audit is **file-sync is split by content type — never blanket file-sync, and never file-sync `.git`** (it corrupts the repo, as the second-pass Section 5 established with the Git maintainers' own FAQ). Three transports, three content classes:

1. **Repo content → git blobless clone/fetch** (`git clone --filter=blob:none`) from its existing remote. Rides git's own transport; repo content **never** goes through the DevStrap hub. The primitive already exists (`internal/git/git.go:107-113`); it is simply not invoked by `sync`.
2. **Env vars + non-git/draft folders → age-encrypted, content-addressed `age_blob:<sha256>` blobs.** The primitive already exists (`internal/envbundle/bundle.go:20-49`); it is not wired to a draft path.
3. **The map of all projects → the signed, HLC-ordered append-only event log** (the "namespace map"). This is the one part that is built.

`node_modules` / build artifacts are **never synced** — they are rebuilt on hydrate.

Materialization is **eager clone-everything on `devstrap sync`** (blobless up front): after a sync the whole `~/Code` tree is present. There is **no FUSE / placeholder / lazy-VFS magic in this design** — StrapFS stays explicitly deferred (Section 6). The transport target is a **two-plane zero-knowledge hub** (event log + content-addressed encrypted blob store) where the hub sees only ciphertext plus a signed map; the production backend is **Cloudflare R2 from the start** (S3 API, zero egress, namespaced by `workspace_id`, zero-knowledge via client-side age encryption), with a file-backed backend retained **only for tests**. Cross-platform Go on macOS + Ubuntu comes first; OS-specific magic (native daemon, StrapFS) is deferred this cycle.

Two themes frame the work:

1. **The materialization gap is the gap between "namespace sync" and "the product".** `EAGER-*` and `DRAFT-*` are both P0 because without them `sync` produces an empty tree (git repos) or a permanent empty skeleton (everything else). These are the killer-loop steps 4–5 from `spec/00`.
2. **The hub and its security posture must land before multi-device is trusted, not after.** Verification still fails open for everything outside a two-type destructive allowlist (`HUB-03`), `age` has no native revocation so revoke must re-encrypt (`HUB-04`), and content-addressed blobs need ref-counting before any retention GC (`HUB-05`). These are P1 — required before the production hub ships, gating, not optional.

---

## Architecture decisions encoded by this audit (the substance)

These are the canonical 2026-06-28 decisions the workstreams below implement. They extend, and do not replace, the second-pass audit.

1. **File-sync is split by content type.** Never blanket file-sync; never file-sync `.git`.
   - Repo content → `git clone --filter=blob:none` / fetch from its existing remote. Repo content **never** transits the hub.
   - Env vars + non-git/draft folders → age-encrypted, content-addressed `age_blob:<sha256>` blobs.
   - The project map → a signed, HLC-ordered append-only event log (the namespace map).
   - `node_modules` / build artifacts → **never** synced; rebuilt on hydrate (`npm`/`pnpm`/`uv install`).
2. **Materialization = eager clone-everything on `devstrap sync`** (blobless/partial up front). No FUSE/placeholder/lazy-VFS. After sync, the whole `~/Code` tree is present.
3. **Two-plane zero-knowledge hub (`devstraphub`):** (a) event log = the namespace map; (b) content-addressed encrypted blob store = env + non-git/draft content. The hub sees **only** ciphertext + a signed map. Pluggable backend behind one `Hub` interface.
4. **Cloud backend: Cloudflare R2 from the start** (S3 API, zero egress, namespaced by `workspace_id`, zero-knowledge via client-side age encryption). **No NAS-first phase.** File-backed backend remains **only** for tests.
5. **Cross-platform core first** (portable Go on macOS + Ubuntu; no native daemon/StrapFS this cycle).
6. **Hosting & scaling (future direction, documented not built):** Fly.io for compute (control plane + agent runners; Firecracker microVM isolation, scale-to-zero/suspend-resume, runs the Go binary natively) + Cloudflare R2 for the hub (namespaced by `workspace_id`; zero-knowledge ⇒ tenant isolation by construction) + managed Postgres (Neon/Supabase) for the control-plane DB. Runner escape-hatch: E2B (self-hostable microVM agent sandboxes).
7. **Conflicts:** HLC ordering + tombstones + detect-don't-merge (already built); never byte-merge files (dual-copy is the only safe default for opaque files; CRDTs solve a different problem).
8. **Device trust:** per-device enrollment/approval; revoke ⇒ re-encrypt affected blobs to the reduced recipient set + flag secrets for rotation (age has no native revocation). Event verification must **fail closed** once enrollment exists.
9. **Out of scope:** the LLM/Claude API the agent runner uses — do not add LLM-provider content to any spec.

---

## Priority Matrix

Ranked by leverage toward the demonstrable killer loop. **P0** = blocks the "same tree everywhere" promise, do now; **P1** = required before the production hub / multi-device ships; **P2** = future direction (documented, not committed).

### P0 — highest leverage (start here)

| Severity | ID | Area | Effort | Finding |
|---|---|---|---|---|
| critical | `EAGER-01` | materialization | M | `sync` applies namespace events but never clones; the other device's `~/Code` stays empty skeletons |
| high | `EAGER-02` | materialization | M | Cursor-based incremental pull unwired — `Pull(ctx, 0)` full-replays all history every sync |
| critical | `DRAFT-01` | non-git | M | `hydrate`/`open` refuse every non-`git_repo` type — `local_git`/`draft_project`/`plain_folder` are unhydratable forever |
| high | `DRAFT-02` | non-git | L | No encrypted draft-bundle path — `draft.snapshot.created` + `age_blob` unbuilt despite the primitive existing |
| high | `EAGER-03` | materialization | M | Env profiles are not hydrated as part of eager materialization — a hydrated repo still has no `.env` |

### Full matrix

| Severity | ID | Area | Effort | Finding |
|---|---|---|---|---|
| medium | `EAGER-04` | materialization | M | Clone-everything needs bounded concurrency, per-project failure isolation, and resumability |
| high | `DRAFT-03` | non-git | M | No single `.devstrapignore` compiler — bundle allow-list, scanner, watcher, agent deny-list all drift |
| medium | `DRAFT-04` | non-git | S | `draft_projects.max_bytes`/`max_files` are dead schema — no size/file-count enforcement |
| medium | `DRAFT-05` | non-git | M | `node_modules`/build artifacts must be excluded from bundles and rebuilt on hydrate |
| high | `HUB-01` | hub | M | `FileHub` is concrete; extract a pluggable `Hub` interface so R2 is a backend, not a rewrite |
| high | `HUB-02` | hub | L | Cloudflare R2 zero-knowledge production backend (S3 API, per-`workspace_id`, client-side age) |
| high | `HUB-03` | hub | M | Event verification fails open outside a two-type allowlist; must fail closed once enrollment exists |
| medium | `HUB-04` | hub | M | Device revoke does not re-encrypt blobs to the reduced recipient set (age has no native revocation) |
| medium | `HUB-05` | hub | M | Content-addressed blobs have no ref-count or GC; retention GC must be gated on snapshot exchange |
| high | `XP-01` | cross-platform | M | Ubuntu parity unproven end-to-end for the full materialize loop (the incoming GMKtec box) |
| medium | `XP-02` | cross-platform | M | No portable periodic scan/sync/materialize loop; sync only runs when invoked by hand |
| medium | `XP-03` | cross-platform | S | Secret Service / `DEVSTRAP_NO_KEYCHAIN` headless key custody untested on Linux |
| low | `XP-04` | cross-platform | S | NFC/case-fold path semantics unvalidated on ext4/Ubuntu and on the NAS mount |
| future | `SCALE-01` | multi-user | L | Control/data-plane split for multi-tenant operation |
| future | `SCALE-02` | multi-user | M | R2 namespaced per `workspace_id` ⇒ tenant isolation by construction |
| future | `SCALE-03` | multi-user | L | Rented microVM runner sandboxes for untrusted multi-tenant agent code |
| future | `SCALE-04` | multi-user | M | Tenancy spectrum (pooled → dedicated/BYOC) + cell-based scaling |
| future | `SCALE-05` | multi-user | S | Chosen hosting stack: Fly.io + Cloudflare R2 + managed Postgres (with rejected alternatives) |

---

## Section 1 — Eager-clone materialization (P0)

> **The decision:** materialization is **eager clone-everything on `devstrap sync`** — blobless/partial up front, no FUSE/placeholder/lazy-VFS. After a sync, the whole `~/Code` tree is present. Repo content rides `git clone --filter=blob:none` from its existing remote and **never** transits the hub.

**Verified current behavior.** `devstrap sync --hub-file` pushes local events, pulls remote events with `hub.Pull(cmd.Context(), 0)` (always from HLC 0), applies them to SQLite via `ApplyEvents`, and then prints `"Synced events: pushed %d, pulled %d; hydration/fetch reconciliation is not implemented yet"` (`internal/cli/sync.go:36-55`). The namespace rows arrive; **nothing on disk is created**. The blobless-clone primitive that materialization needs already exists and is used by the manual `hydrate` command (`internal/git/git.go:107-113` `--filter=blob:none`; `internal/cli/hydrate.go:103` `r.Clone(ctx, project.RemoteURL, tmpPath, partial)`), but `sync` never calls it.

### [EAGER-01] `sync` applies namespace events but never materializes; the receiving device's `~/Code` stays empty skeletons
`critical` · `effort: M` · `internal/cli/sync.go:46-55`, `internal/cli/hydrate.go:67-117`, `internal/git/git.go:107-113`

**Problem.** The product promise ("the same folder path appears... opening it hydrates it", `spec/00`) collapses to "the same *row* appears". The killer loop's steps 4–5 (skeleton appears → opening clones/fetches/hydrates) have no automatic path; the user is told verbatim that reconciliation is unimplemented.

**Current state.** After `ApplyEvents` succeeds, `sync` returns with only a printed count. No clone, no fetch, no skeleton creation. `hydrate` exists but is per-project and manual, and it holds the repo lock + clones into a sibling temp dir then atomically promotes (`internal/cli/hydrate.go:103-117`) — exactly the building block a materialize pass should reuse.

**Recommended fix.** Add an eager materialization pass to `sync` (and a standalone `devstrap materialize` / `devstrap sync --materialize`) that, after applying namespace events, iterates every `git_repo` project that is a skeleton and runs the existing blobless clone-or-fetch path. Clone-everything is the default; `--namespace-only` already exists for the metadata-only case (`internal/cli/sync.go:13`), so make materialize the default and metadata-only the opt-out.

**Reuse.** `hydrateProjectUnlocked` (`internal/cli/hydrate.go:67`) already does lock → temp-clone → atomic promote with use-time root revalidation; lift it into the shared engine and call it per project. `Runner.Clone(..., partial=true)` (`internal/git/git.go:107`) already emits `--filter=blob:none`.

**Example.**
```go
// after ApplyEvents in sync:
for _, p := range store.SkeletonGitRepos(ctx) {     // type=git_repo, materialization_state=skeleton
    if _, err := engine.Hydrate(ctx, store, opts, p, /*partial=*/true); err != nil {
        engine.MarkFailed(ctx, store, p, err)        // EAGER-04: isolate, do not abort the batch
        continue
    }
}
```

**Sequencing.** First deliverable of this cycle — it is the difference between "namespace sync" and "the product". Land before any hub backend work; it is fully testable today over `--hub-file` with a local bare-remote fixture.

**References.** `git clone --filter=blob:none` (https://git-scm.com/docs/partial-clone), Mutagen materialization model (https://mutagen.io/documentation/synchronization/).

### [EAGER-02] Cursor-based incremental pull is unwired; `Pull(ctx, 0)` full-replays all history every sync
`high` · `effort: M` · `internal/cli/sync.go:40`, `internal/sync/hub.go:44-46`, `internal/state/migrations/00002_event_ordering.sql` (`sync_cursors`)

**Problem.** Every sync pulls from HLC 0 and re-applies the entire history, relying on idempotent apply to dedupe. This is O(total history) per sync on both sides and makes the `410 → snapshot` retention path (`ErrSnapshotRequired`, `internal/sync/hub.go:15,45`) unreachable because `RetentionHLC` is never set. (This is `ARCH2-02`/`DATA-02` from the second pass, re-scoped here as a hard prerequisite for a real cloud hub: a network backend cannot ship full-history replay.)

**Current state.** `remoteEvents, err := hub.Pull(cmd.Context(), 0)` (`internal/cli/sync.go:40`). `sync_cursors` is schema-complete but has zero non-test readers/writers. `FileHub.Pull` honors `afterHLC` and returns `ErrSnapshotRequired` when `afterHLC < RetentionHLC` (`internal/sync/hub.go:44-46`), so the cursor mechanism is ready on the hub side.

**Recommended fix.** Persist `sync_cursors.last_hlc_applied` per peer inside the same transaction as `ApplyEvents`, pass it as `afterHLC` on the next `Pull`, and add a regression test that a second sync with no new events pulls zero. Set `RetentionHLC` so the snapshot path is exercised.

**Reuse.** The `sync_cursors`/`event_delivery` tables (`00002_event_ordering.sql`) and `FileHub`'s existing `afterHLC` filtering.

**Sequencing.** Pairs with `EAGER-01` (a materialize loop that re-pulls all history every run is unacceptable) and is a hard precondition for `HUB-02` (R2). Closes second-pass `ARCH2-02`/`DATA-02`.

**References.** Syncthing index cursors (https://docs.syncthing.net/users/syncing), cr-sqlite deltas (https://vlcn.io/docs/cr-sqlite/intro).

### [EAGER-03] Env profiles are not hydrated as part of eager materialization
`high` · `effort: M` · `internal/cli/sync.go:46-55`, `internal/cli/env.go`, `internal/envbundle/bundle.go:20-52`

**Problem.** A repo that is cloned but has no `.env` is not "the same project everywhere" — the env plane is the second of the three content classes. Today env capture/hydrate is an entirely separate manual command surface; sync never restores env state, so a freshly materialized project on device B is functionally broken until the user re-runs `env hydrate` by hand.

**Current state.** `env capture` encrypts to the local age recipient and stores `age_blob:<sha256>` refs; `env hydrate` decrypts local blobs or resolves 1Password refs (`internal/cli/env.go`). The encryption/content-addressing primitive is `internal/envbundle.Encrypt`/`Decrypt` (`bundle.go:20-52`). None of this is invoked by `sync`.

**Recommended fix.** Treat the env profile as plane B content: after a project is materialized, hydrate its bound env profile from the synced encrypted blob (decrypt with the local device identity) into the project, atomically and 0600, reusing the existing refusal/`--force` semantics. For 1Password `op://` refs, bind only (no plaintext) — the ref travels in the event, resolution stays local.

**Reuse.** `internal/envbundle` (age encrypt/decrypt + `age_blob` addressing), the atomic-0600 writer already used by `env capture`/`hydrate`, and the device age identity in `internal/devicekeys`.

**Sequencing.** Immediately after `EAGER-01`; both are needed for a materialized project to be usable. The encrypted env blob also becomes the first real exerciser of the plane-B blob store (`HUB-02`/`HUB-05`).

**References.** age (https://github.com/FiloSottile/age), 1Password `op inject`/`op run` (https://developer.1password.com/docs/cli/).

### [EAGER-04] Clone-everything needs bounded concurrency, per-project failure isolation, and resumability
`medium` · `effort: M` · `internal/cli/sync.go`, `internal/cli/hydrate.go:67-117`, `internal/git/git.go`

**Problem.** "Clone everything on sync" across a real `~/Code` (dozens–hundreds of repos) is the first operation that touches the network at scale. A naive sequential loop is slow; an unbounded parallel loop exhausts fds/connections; and a single clone failure must not abort the whole tree or leave it half-materialized with no way to resume.

**Current state.** `hydrate` materializes exactly one project and the metadata-backed repo operation lock already serializes per-repo work; there is no multi-project orchestration, no concurrency bound, and no batch failure model. The clone path already stages into a sibling temp dir and atomically promotes on success (`internal/cli/hydrate.go:103-117`), so partial-clone debris does not pollute the target — but interrupted temp dirs still leak (a known reaper gap from the second pass).

**Recommended fix.** Materialize with a bounded worker pool (e.g. `min(4, NumCPU)`), isolate failures per project (mark `materialization_state=failed` with the typed git error class and continue), and make the pass idempotent/resumable (a re-run only touches skeletons + `failed`). Add a temp-dir reaper for interrupted clones.

**Reuse.** The existing repo operation locks, typed git error classification + transient-network retry (`internal/git`), and `materialization_state` rows.

**Sequencing.** With or immediately after `EAGER-01`; the failure-isolation model is what makes "clone everything" safe to run unattended (and is a prerequisite for the `XP-02` periodic loop).

**References.** `errgroup` bounded concurrency (https://pkg.go.dev/golang.org/x/sync/errgroup).

---

## Section 2 — Non-git content sync (P0)

> **The decision:** env vars + non-git/draft folders sync as **age-encrypted, content-addressed `age_blob:<sha256>` blobs**, referenced by a signed `draft.snapshot.created` event. `node_modules`/build artifacts are **never** synced — rebuilt on hydrate. A **single `.devstrapignore` compiler** is the one source of truth for what is bundled, scanned, watched, and denied to agents. This is the content-sync half of the second pass's `NOVCS-*` recommendation, now stamped as build work.

**Verified current behavior.** `hydrate`/`open` refuse every non-`git_repo` type with `"%s is %s, not git_repo"` (`internal/cli/hydrate.go:67-69`). The scan layer now *classifies* `local_git`/`draft_project`/`plain_folder` (`internal/scan/scan.go:20-23`, closing second-pass `NOVCS-01/03`), but there is no content path for any of them: no bundle code, no `draft.snapshot.created` event handler, and `draft_projects.max_bytes`/`max_files` (`internal/state/migrations/00001_initial.sql:55-59`) are dead. There is **no `.devstrapignore` compiler** — the token is referenced across nine spec files but exists nowhere in `internal/`.

### [DRAFT-01] `hydrate`/`open` refuse every non-`git_repo` type — local-only projects are unhydratable forever
`critical` · `effort: M` · `internal/cli/hydrate.go:67-69`, `internal/sync/events.go`, `internal/scan/scan.go:20-23`

**Problem.** A `local_git`, `draft_project`, or `plain_folder` syncs as a namespace row and then can never be materialized: `hydrate` hard-rejects it. The tertiary persona's core JTBD ("new folders appear on every machine before they are pushed to Git") is structurally unmet, and the device shows a permanent empty skeleton.

**Current state.** `if project.Type != "git_repo" { return ... "not git_repo" }` (`internal/cli/hydrate.go:68-69`). Scan now emits the non-git types but `applyEventTx` has no draft case and `hydrate` has no draft branch.

**Recommended fix.** Add type-dispatch to the materialization path: `git_repo` → blobless clone (`EAGER-01`); `local_git`/`draft_project` → decrypt-and-extract the latest draft bundle (`DRAFT-02`); `plain_folder` → create the skeleton directory only (structure, no content). Until the bundle path lands, return an honest *"local-only project; content sync not yet materialized"* rather than the misleading "not git_repo".

**Reuse.** The same engine materialize dispatch as `EAGER-01`; the scan type taxonomy already in place.

**Sequencing.** Gate `DRAFT-02` (the bundle path) behind this dispatch; ship the honest message immediately so the namespace stops lying.

**References.** second-pass `NOVCS-02` (`AUDIT_RECOMMENDATIONS_2026-06-27.md`), git-bundle (https://git-scm.com/docs/git-bundle).

### [DRAFT-02] No encrypted draft-bundle path — `draft.snapshot.created` + `age_blob` is unbuilt despite the primitive existing
`high` · `effort: L` · `internal/envbundle/bundle.go:20-52`, `internal/sync/events.go`, `internal/state/migrations/00001_initial.sql:55-63`

**Problem.** Non-git content has no transport. The spec designs an encrypted draft bundle (allow-listed files → tar → zstd → age → one blob per project, referenced by a `draft.snapshot.created` event), but no packing/unpacking code or event handler exists. This is the second of the three content classes and the only safe way to move local-only files (file-sync of `.git` is forbidden; opaque drafts are dual-copy-on-conflict, never byte-merged — decision #7).

**Current state.** `internal/envbundle.Encrypt(bindings, recipients) → (ciphertext, "age_blob:<sha256>")` and `Decrypt(ciphertext, identity)` already exist (`bundle.go:20-52`) for env; the same shape generalizes to a directory tar. No `draft.snapshot.created` event constant, no `applyEventTx` case, no blob store PUT/GET.

**Recommended fix.** Add `devstrap draft snapshot create <path>`: walk the dir under the `.devstrapignore` allow-list (`DRAFT-03`) + the `draft_projects` limits (`DRAFT-04`), tar+zstd, age-encrypt to approved-device recipients, store a content-addressed `age_blob:<sha256>`, and emit a signed `draft.snapshot.created` event. Add the apply case and the `draft_project` hydrate branch (decrypt → unzstd → untar into the skeleton). Enforce "no plaintext secret files / private keys in a bundle" via the shared secret detector.

**Reuse.** `internal/envbundle` (generalize the age-encrypt + `age_blob` addressing to a tar stream), the device recipient set in `internal/devicekeys`, the `isSecretName` detector from `internal/scan`, and the existing atomic-0600 write infra.

**Sequencing.** After `DRAFT-01` (type dispatch) and `DRAFT-03` (ignore compiler); it is the first real producer/consumer of the plane-B blob store (`HUB-05`). Layer C of the second pass's working-state design rides this same transport.

**References.** `dew` allow-list bundling (https://github.com/vedanta/dew), age (https://github.com/FiloSottile/age), zstd (https://github.com/klauspost/compress).

### [DRAFT-03] No single `.devstrapignore` compiler — bundle allow-list, scanner prune, watcher exclude, and agent deny-list all drift
`high` · `effort: M` · `internal/scan/scan.go` (prune list), `internal/platform/fsnotify_watcher.go` (watch-skip list), `internal/cli/agent.go` (file deny-list)

**Problem.** Four independent, hardcoded junk/ignore lists already diverge (the second pass's `PLAT-01`/`AGEN-05` root cause). Adding a fifth consumer — the draft-bundle allow-list — without a single compiler guarantees a bundle includes a high-churn generated tree or a secret the scanner would have caught. The `spec/11` `.devstrapignore` compiler is the documented single source of truth and does not exist.

**Current state.** The token `.devstrapignore` appears only in specs. The scanner prune list, the fsnotify watcher skip list, and the agent file deny-list are three separate hardcoded sets; the bundle walker (`DRAFT-02`) would be a fourth.

**Recommended fix.** Build `internal/ignore` as the canonical compiler: one pattern source compiling to (a) the bundle allow-list, (b) the scanner prune predicate, (c) the watcher exclusion set, (d) the agent file deny-list, and (e) generated `.gitignore`/`.dockerignore` fragments. All consumers call it; OS junk (`.DS_Store`, `Thumbs.db`, `.AppleDouble`, `__pycache__`, `.venv`, `dist`, `build`, `target`, `node_modules`) lives in one table.

**Reuse.** Fold the existing scanner prune logic in as the first compiled target; wire the watcher and agent deny-list to the same predicate (directly closes second-pass `PLAT-01`/`PLAT-04`/`AGEN-05`).

**Sequencing.** Before `DRAFT-02` (the bundle walker must consume it) and `DRAFT-05` (artifact exclusion is an ignore rule). Highest-leverage single piece of de-duplication in the codebase.

**References.** `.gitignore` semantics (https://git-scm.com/docs/gitignore), `.dockerignore` (https://docs.docker.com/build/concepts/context/#dockerignore-files).

### [DRAFT-04] `draft_projects.max_bytes`/`max_files` are dead schema — no size or file-count enforcement
`medium` · `effort: S` · `internal/state/migrations/00001_initial.sql:55-63`, `internal/scan/scan.go`

**Problem.** Invariant #8 requires draft folders to have enforced size and ignore limits, so a runaway draft (a multi-GB dataset dropped into `~/Code`) cannot silently turn into a giant encrypted blob pushed to the hub. The limit columns exist but nothing reads them.

**Current state.** `max_bytes INTEGER NOT NULL DEFAULT 104857600` (100 MiB), `max_files INTEGER NOT NULL DEFAULT 5000` (`00001_initial.sql:58-59`); the only reference to these columns is the migration itself.

**Recommended fix.** Enforce both during `draft snapshot create` (`DRAFT-02`): walk under the ignore allow-list, and refuse (with an actionable message naming the offending size/count and the `.devstrapignore` knob) before encrypting if either limit is exceeded. Surface a warning in `scan`/`status` when a draft approaches its limit.

**Reuse.** The `.devstrapignore` compiler (`DRAFT-03`) determines what counts toward the limit; the `draft_projects` rows already carry per-project overrides.

**Sequencing.** Implement together with `DRAFT-02` — the limit check is the bundle walker's precondition. Closes second-pass `PROD-03`.

**References.** second-pass `PROD-03` (`AUDIT_RECOMMENDATIONS_2026-06-27.md`).

### [DRAFT-05] `node_modules` / build artifacts must be excluded from bundles and rebuilt on hydrate, never synced
`medium` · `effort: M` · `internal/cli/hydrate.go`, `internal/ignore` (new, `DRAFT-03`)

**Problem.** `node_modules`, `dist`, `build`, `target`, `.venv`, and `__pycache__` are the largest, highest-churn, most-redundant trees in any project — syncing them (as bundle content or otherwise) is wasteful and corruption-prone (native modules are platform-specific; a macOS `node_modules` is wrong on the Ubuntu box). They must be reconstructed locally from the lockfile, not transported.

**Current state.** No exclusion (these are in the divergent prune/skip lists but not a single bundle-aware source), and no post-hydrate rebuild hook. A hydrated project has its lockfile but no installed deps.

**Recommended fix.** (a) Exclude all artifact trees from bundles and the git working set via the `.devstrapignore` compiler (`DRAFT-03`). (b) Add an opt-in post-hydrate rebuild step that detects the toolchain and runs the appropriate restore (`npm`/`pnpm`/`yarn install`, `uv`/`pip install`, `go mod download`, `cargo fetch`) inside the materialized project, gated and logged, never automatic on metered/offline runs.

**Reuse.** The `.devstrapignore` compiler for exclusion; the existing `devstrap run` / child-env sanitizer for executing the restore command with a sanitized environment.

**Sequencing.** Exclusion ships with `DRAFT-03`; the rebuild hook follows `EAGER-01` (it is a post-materialize step). Keep rebuild opt-in until the periodic loop (`XP-02`) can schedule it sensibly.

**References.** reproducible installs from lockfiles (https://docs.npmjs.com/cli/commands/npm-ci, https://docs.astral.sh/uv/).

---

## Section 3 — Cloud Hub backend (P1)

> **The decision:** a **two-plane zero-knowledge hub** behind **one pluggable `Hub` interface** — (a) the namespace event log, (b) the content-addressed encrypted blob store. The hub sees **only** ciphertext + a signed map. The production backend is **Cloudflare R2 from the start** (S3 API, zero egress, namespaced by `workspace_id`, zero-knowledge via client-side age encryption). The file-backed backend is retained **only** for tests. **No NAS-first phase.**

**Verified current behavior.** The only hub is `FileHub` (`internal/sync/hub.go:17`), a concrete struct with `Push`/`Pull`/`read`/`write` against a single JSON file; there is no `Hub` interface, no `HTTPHub`, and no R2/S3 code anywhere in the tree. `ErrSnapshotRequired` + `RetentionHLC` exist on `FileHub` (`hub.go:15,19,45`) but are unreachable because the cursor is hardcoded to 0 (`EAGER-02`). Event verification fails open outside a two-type destructive allowlist (`internal/state/store.go:1936-1990`, `mustVerifyEvent` covers only `project.deleted`/`project.renamed`).

### [HUB-01] `FileHub` is concrete — extract a pluggable `Hub` interface so R2 is a backend, not a rewrite
`high` · `effort: M` · `internal/sync/hub.go:17-95`, `internal/cli/sync.go:36-44`

**Problem.** Every caller constructs `dssync.FileHub{Path: hubFile}` directly (`internal/cli/sync.go:36`), so swapping in a network backend means touching call sites and re-testing the apply path. Decision #3 requires a pluggable backend behind one interface.

**Current state.** `FileHub` implements `Push(ctx, []Event) error` and `Pull(ctx, afterHLC int64) ([]Event, error)` but only as concrete methods; blobs have no hub surface at all (env/draft blobs are written locally only).

**Recommended fix.** Define `type Hub interface { Push(ctx, []state.Event) error; Pull(ctx, afterHLC int64) ([]state.Event, error); PutBlob(ctx, sha256 string, r io.Reader) error; GetBlob(ctx, sha256 string) (io.ReadCloser, error) }`. Make `FileHub` satisfy it (pure refactor; existing tests pass) and have `sync` accept a `Hub`. This is the seam every other `HUB-*` and `EAGER-02` work item plugs into.

**Reuse.** `FileHub`'s existing `Push`/`Pull`/`ErrSnapshotRequired`/`RetentionHLC` become the reference (test) implementation of the interface.

**Sequencing.** First step of this section (pure refactor, no behavior change); also closes the second pass's `2.0` hub-interface item. Land before `HUB-02`.

**References.** second-pass Section 6 sequencing item 2.0 (`AUDIT_RECOMMENDATIONS_2026-06-27.md`).

### [HUB-02] Cloudflare R2 zero-knowledge production backend (S3 API, per-`workspace_id`, client-side age)
`high` · `effort: L` · new `internal/hub` (R2 backend), `internal/envbundle` (encryption boundary), `internal/devicekeys` (signing)

**Problem.** There is no production transport. Decision #4 fixes the backend as **Cloudflare R2 from the start**: S3-compatible API, zero egress fees, objects namespaced by `workspace_id`, and zero-knowledge by construction because all content is client-side age-encrypted before upload. No NAS-first phase, no bespoke server to operate for the single-user fleet.

**Current state.** Nothing — no S3 client, no R2 config, no object-key scheme. The encryption boundary that makes R2 safe already exists client-side (`internal/envbundle.Encrypt`, `internal/devicekeys.Sign`).

**Recommended fix.** Implement an R2 `Hub` backend (S3 API via the AWS SDK or `minio-go`) with object keys namespaced `workspace/<workspace_id>/events/<hlc>` and `workspace/<workspace_id>/blobs/<sha256>`. The event log is append-only objects (or a manifest + segments); blobs are content-addressed PUT/GET. **All payloads and blobs are age-encrypted and Ed25519-signed before upload** — R2 stores only ciphertext + a signed map. Add a zero-knowledge test asserting the backend, given only what it stores, can decrypt nothing and holds no private key.

**Reuse.** `internal/envbundle` (age encrypt/decrypt above the wire), `internal/devicekeys` (Ed25519 sign/verify), the `Hub` interface from `HUB-01`, the cursor from `EAGER-02`. The file-backed `FileHub` stays the test double.

**Sequencing.** After `HUB-01` (interface) and `EAGER-02` (cursor) — a network backend cannot ship full-history replay. Pair the rollout with `HUB-03` (fail-closed verification) since R2 is a semi-trusted store that can reorder/replay/omit/substitute (all detected off-wire).

**References.** Cloudflare R2 S3 API + zero egress (https://developers.cloudflare.com/r2/api/s3/api/), content-addressed storage (https://docs.ipfs.tech/concepts/content-addressing/).

### [HUB-03] Event verification fails open outside a two-type allowlist; must fail closed once enrollment exists
`high` · `effort: M` · `internal/state/store.go:1936-1990` (`verifyEventSignature`, `mustVerifyEvent`)

**Problem.** Decision #8: once per-device enrollment exists, event verification must **fail closed**. Today it fails closed only for an allowlist of two destructive types and fails open for everything else — an unknown or keyless device's `project.added`/`project.updated`/`draft.snapshot.created`/`repo.gitstate.observed`/env events are accepted unverified. Against a semi-trusted R2 hub (`HUB-02`) or a compromised hub file, that is an injection surface for the entire non-destructive event space, including the new content-bearing events this audit adds.

**Current state.** `verifyEventSignature` returns `nil` for an unknown device (`sql.ErrNoRows`) or empty signing key unless `mustVerifyEvent(event.Type)` is true, and `mustVerifyEvent` returns true only for `project.deleted`/`project.renamed` (`internal/state/store.go:1944-1990`). This is the partial, in-progress closure from commit `e982c05`; it is not yet fail-closed.

**Recommended fix.** Once a workspace has any enrolled (approved) device, require a valid signature from a known, approved device with a non-empty signing key for **all** event types from non-local devices — invert the default from open to closed. Keep the local device's pre-enrollment grace (it may not have signing set up yet) and a documented bootstrap window, but make "enrolled ⇒ every remote event verified" the invariant. Extend `mustVerifyEvent` into a workspace-state-aware policy rather than a static two-type list.

**Reuse.** The existing `devicekeys.Verify`, the `trust_state` column, and the `mustVerifyEvent` seam (broaden it).

**Sequencing.** Land with `HUB-02` — a fail-open verifier behind a network hub is the headline vulnerability. Closes second-pass `SECU-03`/`SECU-05`.

**References.** second-pass `SECU-03`/`SECU-05` (`AUDIT_RECOMMENDATIONS_2026-06-27.md`), OWASP threat modeling (https://owasp.org/www-community/Threat_Modeling_Process).

### [HUB-04] Device revoke does not re-encrypt blobs to the reduced recipient set (age has no native revocation)
`medium` · `effort: M` · `internal/cli/devices.go`, `internal/envbundle/bundle.go`, `internal/devicekeys`

**Problem.** Decision #8: revoke ⇒ re-encrypt affected blobs to the reduced recipient set + flag secrets for rotation. `age` has **no native revocation** — a blob already encrypted to a revoked device's X25519 recipient stays decryptable by that key forever. Without re-encryption, "revoke" is cosmetic for any blob the device already could read.

**Current state.** Device revoke/lost flips `trust_state` and sets `secret_bindings.needs_rotation` (surfaced in `doctor`) — that is the rotation half (second pass `SECR-03`). The re-encryption half is unbuilt: no code re-wraps env/draft blobs to the remaining approved recipients.

**Recommended fix.** On revoke/lost, enumerate blobs encrypted to the removed recipient, decrypt with the local identity, re-encrypt to the current approved recipient set, write new content-addressed blobs, repoint the referencing events, and flag the underlying secrets `needs_rotation`. Document that re-encryption limits future exposure but cannot un-read already-synced ciphertext — hence the mandatory rotation flag.

**Reuse.** `internal/envbundle.Encrypt`/`Decrypt`, the approved-recipient set in `internal/devicekeys`, the existing `needs_rotation` flagging.

**Sequencing.** Required before multi-device enrollment is offered as a trust boundary (with `HUB-03`). Builds on second-pass `SECR-03`.

**References.** age recipients/revocation limits (https://github.com/FiloSottile/age), key rotation guidance (https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html).

### [HUB-05] Content-addressed blobs have no ref-count or GC; retention GC must be gated on snapshot exchange
`medium` · `effort: M` · `internal/sync/hub.go`, new blob store, `internal/state` (blob refs)

**Problem.** Every env/draft snapshot creates a new `age_blob:<sha256>` (a re-capture or `HUB-04` re-encryption mints a fresh object). Without ref-counting and GC, R2 storage grows unbounded; with naive GC, deleting a blob still referenced by an older event corrupts a device that has not yet caught up. And enabling event-log retention GC before a full-state snapshot exchange exists silently diverges any device past the retention horizon (second pass `ARCH2-02`/Section 6).

**Current state.** Blobs are written locally only; there is no blob index, no ref-count, no GC, and no snapshot/full-state export. `ErrSnapshotRequired` exists but is never returned (`EAGER-02`).

**Recommended fix.** Maintain a blob ref-count keyed by `sha256` from the events that reference it; GC a blob only when its ref-count hits zero **and** it is older than the retention/snapshot horizon. Build the full-state snapshot export/import wired to the `410 → ErrSnapshotRequired` path **before** enabling any event-log or blob retention GC.

**Reuse.** Content-addressing already in `internal/envbundle`; the `sync_cursors`/retention machinery from `EAGER-02`.

**Sequencing.** After `HUB-02` (a real store to GC) and the cursor work; snapshot exchange is the hard precondition for any GC. Closes the second pass's "build snapshot before retention GC" risk.

**References.** content-addressed GC (https://git-scm.com/book/en/v2/Git-Internals-Maintenance-and-Data-Recovery), second-pass Section 6 (`AUDIT_RECOMMENDATIONS_2026-06-27.md`).

---

## Section 4 — Cross-platform hardening (P1)

> **The decision:** cross-platform Go core first (macOS + Ubuntu); OS-specific magic (native daemon, StrapFS) deferred this cycle. The fleet already includes an **incoming GMKtec Ubuntu box** and a **NAS**, so Ubuntu is not hypothetical — it is the next device that must show the same `~/Code`.

**Verified current behavior.** Platform adapter seams exist for darwin/linux/other (`internal/platform/detect_*.go`), with `UnsupportedServiceManager` placeholders on both real OSes and a polling-watcher fallback. CI runs `ubuntu-latest` and `macos-latest`. But the materialize loop (`EAGER-*`) does not exist yet, so "the same tree appears on Ubuntu" has never been exercised end-to-end, and there is no scheduled/periodic invocation of `sync` on any platform.

### [XP-01] Ubuntu parity unproven end-to-end for the full materialize loop
`high` · `effort: M` · `internal/platform/detect_linux.go`, `internal/cli/sync.go`, CI matrix

**Problem.** The product promise is "install on a new Mac, Linux box, cloud machine, or agent runner — the same tree appears." The GMKtec Ubuntu box is the test of that. Today the loop that would make a tree appear (`EAGER-01`) does not run anywhere, and the Linux adapters (Secret Service keychain, systemd placeholder) are less exercised than the Darwin ones.

**Current state.** `Detect()` returns linux adapters with a `secret-service` keychain target and an `UnsupportedServiceManager` (`detect_linux.go:5-10`); the fsnotify watcher has a Linux path; CI compiles and tests on `ubuntu-latest`. No end-to-end Linux test drives init → scan → sync → materialize → open.

**Recommended fix.** Once `EAGER-01` lands, add an Ubuntu end-to-end test (testscript through the real binary with a local bare-remote fixture) asserting the full loop materializes an identical tree, and validate the Secret Service path (with `DEVSTRAP_NO_KEYCHAIN` fallback) on Linux. Keep Mac-specific behavior behind the existing adapters so Linux stays first-class (per `AGENTS.md`).

**Reuse.** The existing `internal/platform` adapter seams and the `rogpeppe/go-internal` testscript harness.

**Sequencing.** Immediately after `EAGER-01`/`EAGER-03` — parity is meaningless until there is a loop to run. This is the acceptance test for the GMKtec box joining the fleet.

**References.** `AGENTS.md` ("keep Mac-specific behavior behind adapters so Linux support remains viable").

### [XP-02] No portable periodic scan/sync/materialize loop; sync runs only when invoked by hand
`medium` · `effort: M` · `internal/cli/sync.go`, `internal/cli/scan.go`, new `devstrap run-loop`

**Problem.** "Dropbox for code" implies the tree converges on its own. With no native daemon this cycle (decision #5), there must still be a **portable** way to run scan + sync + materialize on an interval, identically on macOS and Ubuntu, without launchd/systemd plumbing.

**Current state.** `scan`, `sync`, and (after `EAGER-01`) materialize are one-shot commands. The fsnotify watcher and `PollWatcher` are built but unwired (second-pass `PLAT-03`); there is no scheduler.

**Recommended fix.** Add a foreground `devstrap run-loop [--interval]` (a portable Go ticker that runs scan → sync → materialize, with jittered backoff and `--once` for cron) that any OS scheduler (cron, a user `crontab`, Task Scheduler, or a hand-rolled `launchd`/`systemd` unit later) can drive. This delivers periodic convergence now without committing to native installers (Section 6) and gives the future daemon a ready-made reconcile body.

**Reuse.** The materialize pass from `EAGER-01`/`EAGER-04` (bounded, failure-isolated) is exactly the loop body; the cursor from `EAGER-02` keeps each tick cheap.

**Sequencing.** After the materialize loop exists. Explicitly **not** a daemon — the native LaunchAgent/systemd installers stay deferred (Section 6).

**References.** portable scheduling without a daemon (https://man7.org/linux/man-pages/man5/crontab.5.html).

### [XP-03] Secret Service / `DEVSTRAP_NO_KEYCHAIN` headless key custody untested on Linux
`medium` · `effort: S` · `internal/devicekeys`, `internal/platform/detect_linux.go`

**Problem.** Device age/Ed25519 private identities back the entire zero-knowledge model. On Linux they should live in the Secret Service, falling back to `~/.devstrap/keys` (0600) when unavailable, with `DEVSTRAP_NO_KEYCHAIN` forcing the file store for headless/CI. This fallback is asserted but not exercised on a real Ubuntu environment, and the second pass flagged the custody downgrade as silently fail-open on *any* keychain error (`SECR-04`/`SECU-01`).

**Current state.** The linux keychain target is `secret-service`; `DEVSTRAP_NO_KEYCHAIN` gates the file store. No Linux integration test covers the Secret Service path or the headless fallback, and the downgrade is still on-any-error rather than only-on-unavailable.

**Recommended fix.** Add a Linux key-custody test (Secret Service present, absent, and `DEVSTRAP_NO_KEYCHAIN` set), and make the file fallback fail-closed on a present-but-failing keyring with a visible warning (closing second-pass `SECR-04`/`SECU-01`).

**Reuse.** The existing `DEVSTRAP_NO_KEYCHAIN` gate and the `keychainUnavailable` classifier in `internal/devicekeys`.

**Sequencing.** With `XP-01` (same Ubuntu validation pass).

**References.** freedesktop Secret Service (https://specifications.freedesktop.org/secret-service/).

### [XP-04] NFC / case-fold path semantics unvalidated on ext4/Ubuntu and on the NAS mount
`low` · `effort: S` · `internal/pathkey`, `internal/scan`

**Problem.** The namespace uses NFC display normalization + a case-folded `path_key`. macOS (case-insensitive APFS) and Ubuntu (case-sensitive ext4) disagree on case collisions, and the NAS may present yet another filesystem with its own normalization. A `path_key` collision that is benign on one device can be a real two-file collision on another.

**Current state.** `internal/pathkey` normalizes and case-folds; the second pass confirmed unsafe-path rejection and symlink-escape handling. No cross-filesystem test asserts the same namespace materializes consistently on case-sensitive ext4 and the NAS.

**Recommended fix.** Add path-key tests on ext4 (case-sensitive) and a representative network/synced mount asserting collision detection behaves consistently, and warn (per the second pass `DATA-05`) when `state.db` or `~/Code` sits on a networked/synced filesystem.

**Reuse.** `internal/pathkey` and the conflict-recording machinery already in place.

**Sequencing.** With the broader Ubuntu/NAS validation (`XP-01`); low risk, but cheap to lock down before the NAS joins the fleet.

**References.** Unicode normalization on filesystems (https://www.unicode.org/reports/tr15/), second-pass `DATA-05`.

---

## Section 5 — Scaling to multi-user (future, P2)

> **Status: documented direction, not committed work.** Everything in this section is forward-looking and intentionally deferred. The single-user fleet (the two Mac Minis, the GMKtec Ubuntu box, the laptop, the NAS) needs none of it; it is recorded so the Phase-0/2 choices do not foreclose the multi-tenant future. The hosting stack below is a **decision**, but a decision about *direction*, not a build item for this cycle.

### [SCALE-01] Control/data-plane split
`future` · `effort: L` · architecture direction

**Problem (future).** A multi-user DevStrap must separate the **control plane** (account/device enrollment, billing, coordination, agent-run scheduling — stateful, trusted, needs a relational DB) from the **data plane** (the zero-knowledge event log + content-addressed blob store — high-volume, ciphertext-only). Fusing them couples tenant coordination to ciphertext storage and undermines the zero-knowledge property.

**Direction.** Control plane on managed Postgres (Neon/Supabase); data plane on R2 (`SCALE-02`). The control plane never sees plaintext or private keys — it routes signed maps and brokers enrollment; correctness still rests off-wire on the client (HLC + hash chain + Ed25519).

**Reuse.** The existing `internal/state` schema is the per-device authoritative store; the control-plane DB is a new, separate concern.

**Sequencing.** Only when a second tenant exists. Coder is the reference architecture for agents-on-your-infra at scale.

**References.** Coder architecture (https://coder.com/docs/admin/infrastructure/architecture).

### [SCALE-02] R2 namespaced per `workspace_id` ⇒ tenant isolation by construction
`future` · `effort: M` · architecture direction

**Problem (future).** Multi-tenant blob/event storage must isolate tenants without trusting the server. Because every object is client-side age-encrypted and namespaced by `workspace_id` (`HUB-02`), the zero-knowledge property *is* the tenant-isolation property: one tenant's keys cannot decrypt another's objects even given full bucket access.

**Direction.** Keep the `HUB-02` `workspace/<workspace_id>/...` key scheme; per-tenant IAM/bucket policies on R2 are defense-in-depth atop the cryptographic isolation. No per-tenant server logic is required for isolation.

**Sequencing.** Falls out of `HUB-02` for free; formalize bucket/IAM scoping when onboarding tenant #2.

**References.** R2 bucket/access scoping (https://developers.cloudflare.com/r2/api/s3/tokens/).

### [SCALE-03] Rented microVM runner sandboxes for untrusted multi-tenant agent code
`future` · `effort: L` · architecture direction

**Problem (future).** Running other users' agent tasks on shared infrastructure requires hardware-grade isolation — the argv-substring agent policy (second-pass `AGEN-01`) and even an OS sandbox are insufficient against untrusted multi-tenant code. The current agent runner has no OS-enforced sandbox at all.

**Direction.** Per-task **microVM** isolation: **Fly Machines** (Firecracker) as primary (runs the Go binary natively, scale-to-zero/suspend-resume, 35+ regions), with **E2B** (self-hostable microVM agent sandboxes) as the escape hatch. Vercel Sandbox and AWS Lambda MicroVMs are alternatives in the same Firecracker family. Each agent task gets a fresh microVM, a fresh worktree from `origin/<default_branch>`, and no access to other tenants' data.

**Reuse.** The thin generic agent runner (fresh worktree, sanitized env, command/file policy, 0600 log) is the workload; the microVM is the new isolation boundary around it.

**Sequencing.** Only for untrusted multi-tenant execution; the single-user fleet runs the binary directly. Defer until `SCALE-01` exists.

**References.** Fly Machines/Firecracker (https://fly.io/docs/machines/), E2B (https://e2b.dev/docs), Firecracker (https://firecracker-microvm.github.io/).

### [SCALE-04] Tenancy spectrum (pooled → dedicated/BYOC) + cell-based scaling
`future` · `effort: M` · architecture direction

**Problem (future).** A single deployment model does not fit all tenants: hobbyists want pooled/cheap; enterprises want dedicated or bring-your-own-cloud; and a single global control plane is a blast-radius and scaling risk.

**Direction.** Offer a tenancy spectrum — **pooled** shared control plane for small tenants, **dedicated/BYOC** for large/regulated ones — and scale the control plane in **cells** (independent shards of tenants) so a cell failure or noisy-neighbor is contained. Coder is the reference for both the agents-on-your-infra model and the dedicated/BYOC posture.

**Sequencing.** A scaling concern that only matters at meaningful tenant count; record the cell boundary in the `SCALE-01` design so it is not retrofitted.

**References.** cell-based architecture (https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/), Coder (https://coder.com/docs).

### [SCALE-05] Chosen hosting stack: Fly.io + Cloudflare R2 + managed Postgres
`future` · `effort: S` · hosting decision (direction)

**Decision (direction).** When DevStrap is hosted for others, the stack is:
- **Compute (control plane + agent runners): Fly.io** — Firecracker microVM isolation, 35+ regions, scale-to-zero/suspend-resume, runs the Go binary natively.
- **Sync hub storage: Cloudflare R2** — namespaced by `workspace_id`; zero-knowledge ⇒ tenant isolation by construction; zero egress (`HUB-02`).
- **Control-plane DB: managed Postgres (Neon/Supabase).**
- **Runner escape-hatch: E2B** (self-hostable microVM agent sandboxes).

**Rejected as primary (with reasons).**
- **Railway** — shared-kernel containers: fine for the control plane or your own trusted instance, **not** for untrusted multi-tenant code.
- **Vercel** — strong *if* the stack were Next.js/TS (Sandbox + Functions/Workflows), but DevStrap is **Go-first**, so its TS/Python sandbox SDKs are an awkward fit.
- **Hetzner** — cheapest always-on box, good for the solo MVP, but **no microVM / global / scale-to-zero**.

**Sequencing.** No build work this cycle. The only forward-compatibility constraints it imposes are already satisfied by decisions #4 (R2) and #6 (Go-native binary): keep the hub backend pluggable (`HUB-01`) and the runner a plain argv process so it drops into a Fly Machine / E2B microVM unchanged.

**References.** Fly.io (https://fly.io/docs/), Cloudflare R2 (https://developers.cloudflare.com/r2/), Neon (https://neon.tech/docs), E2B (https://e2b.dev/docs).

---

## Section 6 — Deferred (explicit)

These are deliberately **not** built this cycle. They are listed so coverage limits are not mistaken for completeness, and so they are not relitigated mid-stream.

- **FUSE / StrapFS (lazy virtual filesystem).** The materialization design is **eager clone-everything** (Section 1); there is no placeholder/lazy-VFS layer. StrapFS (macOS File Provider / macFUSE/FSKit; Linux FUSE; Windows WinFsp) remains the explicitly-later Phase-4 layer from `spec/00`. Eager clone delivers the Dropbox-like experience without the kernel-extension/File-Provider/FUSE complexity that the architecture decision (`spec/01`) rejected as the MVP.
- **Native launchd / systemd installers.** Periodic convergence this cycle is the **portable** `devstrap run-loop` (`XP-02`), driven by whatever scheduler the user already has. The native LaunchAgent (launchd) and systemd user-service installers, the local socket API, and the FSEvents-specific Mac watcher stay deferred (Phase-1 daemon work), consistent with `CLAUDE.md` "Not implemented yet".
- **Production HTTP/SSE hub server.** This cycle ships the pluggable `Hub` interface (`HUB-01`) and the **Cloudflare R2** client backend (`HUB-02`) — a managed object store, not a server to operate. The bespoke `cmd/devstraphub` HTTP/SSE relay (POST/GET `/events`, SSE `/stream` with `Last-Event-ID`, `410 → snapshot`, mTLS device certs) from the second pass's Section 6 remains deferred; R2 + client-side crypto covers the single-user fleet without standing up and securing a service. Revisit the bespoke server only if a transport need (live push, multi-tenant routing) outgrows R2.

> Coverage note: this audit defines the cloud-sync workstreams only. The second-pass audit's other open items — agent OS-sandboxing (`AGEN-03`), the signed audit-log subsystem (`spec/15`), the daemon socket API hardening (`CLI-05`), and the full `--json`/exit-code contract (`CLI-01..04`) — remain valid and are not superseded here.
