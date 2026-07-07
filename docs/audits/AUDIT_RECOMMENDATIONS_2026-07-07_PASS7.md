# DevStrap — Design & Implementation Audit (Seventh Pass)

_Date: 2026-07-07 · Trunk audited: `d667530` (`docs: unattended-operation wave close-out (PRs #136-#139) (#140)`)_

## How this relates to the prior audits

This is the **seventh** design & implementation pass. Pass 6 (2026-07-01, trunk `8c739b8`) closed
43/43. Since then **~40 PRs (#77–#140) landed without an adversarial audit checkpoint**. The ledger
(`docs/audits/README.md`) itself tracked the shipped waves and is current through #140 — but the new
code had never been audited, and two design specs drifted behind it (`spec/00` and `ARCHITECTURE.md`
still called the OS sandbox and the LaunchAgent/systemd installers unbuilt; see `P7-DOC-01/02/03`,
fixed in this PR). This pass **adversarially audits the new #77–#140 code** — the densest cluster of
fast-written, security-critical work the project has shipped in one stretch — and re-syncs those specs:

1. **The OS-enforced agent sandbox** (`P4-GIT-03`, PRs #107–#129) — macOS Seatbelt, Linux
   bubblewrap + Landlock + seccomp, read-confinement, `sandbox.violation` telemetry. This is new
   kernel-boundary code and it carries the densest defect cluster in this pass.
2. **The AD-1 zero-infrastructure carrier hub** (PRs #96–#106) — a private-git-repo and a
   local/cloud-drive-folder backend behind the pluggable `Hub` interface, now the quickstart default.
3. **The supply-chain + distribution pipeline** (PRs #105–#120) — cosign keyless signing, SLSA v1
   provenance, per-archive SBOMs, the `curl|sh` installer + Homebrew cask, dormant notarization.
4. **The multi-device completeness wave** (PRs #131–#139) — `ENV-SYNC-01` cross-device env-profile
   exchange, `TRUST-01` synced device-revocation propagation, self-healing WCK rotation, and
   `devstrap service install|uninstall|status`.

This pass does **not** restate still-open PASS4/PASS5 recommendations; those remain tracked in the
ledger (mapped in **Appendix A**). It concentrates where a fresh pass adds the most value: the surfaces
above, which no prior pass has seen.

**ID scheme.** Every finding is prefixed `P7-` so its ID is globally unique, per the ledger convention
(`P5-PROC-01`). Dimension codes: `SANDBOX` (OS agent sandbox), `HUB` (carrier hub), `SEC`
(key/trust lifecycle), `SYNC` (sync engine), `SUPPLY`/`QUAL` (supply-chain/CI), `SVC` (service manager),
`CLI` (CLI/UX), `DATA` (data model), `DOC` (specs/docs/process).

## Methodology

Findings were produced by a verification-driven multi-agent workflow against the `d667530` worktree,
mirroring the Pass-6 method: **nine dimension reviewers**, each told exactly which PASS4/5/6 IDs are
*shipped vs open* (so they hunt **new** issues on the #77–#140 surfaces), then **every candidate finding
was independently adversarially verified** by a separate agent that re-opened the cited code, reproduced
the failure from source (several by building a real git worktree or reading the exact deny-set), and tried
to refute it. **The three P1 candidates were additionally cross-verified by a second model family
(gpt-5.5 via Codex)** — the project's dual-review norm applied to the audit itself. Verifier corrections
are folded into every finding: **22 candidates → 21 confirmed/plausible, 1 refuted** (P1=3, P2=11, P3=7).
Two candidates were downgraded at verification (`P7-SUPPLY-01` P1→P3, `P7-SANDBOX-02` scope-narrowed)
and one was upgraded (`P7-SANDBOX-01` P2→P1). External best-practice anchors (five exa-backed research
topics on agent-sandbox escapes, SLSA/cosign verification, git-carrier concurrency, and E2E-encrypted
git services) are cited inline and collected at the end.

**Severity:** P1 = correctness/security/data-loss or major; P2 = significant; P3 = minor/polish/DX.
**Effort:** S ≈ <½ day, M ≈ 1–3 days, L ≈ ~1 week, XL ≈ multi-week.

## Executive summary

The #77–#140 wave is real, disciplined progress — every obvious hardening surface is already closed
(the carrier already fetch-resets-and-retries on non-fast-forward, `--force-with-lease`es compaction, and
carries an adversarial test; the Seatbelt profile already denies `~/.ssh`/`~/.aws`/`.netrc`; the cosign
verify recipe already pins `--certificate-identity` + `--certificate-oidc-issuer`; release permissions are
per-job-scoped). So this pass found **no gaping holes — the value is in the subtle gaps a fast wave leaves
behind**, and there are three that matter.

Risk clusters in four themes:

1. **The default-on sandbox quietly breaks the core agent loop.** `P7-SANDBOX-01` (P1): the write
   allow-list grants only the worktree dir and tmp dir, but a DevStrap agent worktree is a git *linked*
   worktree whose index/HEAD/objects/refs live in the **parent clone's `.git`** — outside both. Under the
   default `guarded` policy with `--sandbox auto` (the common Mac dev host), any `git add`/`git commit`
   the agent runs is EPERM'd by the kernel, so `agent pr` has no commits to push. All three backends share
   the gap, and the only e2e canary commits into a *fresh nested* repo, so it never exercises the linked-worktree
   path and the bug is untested. This silently defeats the "agent commits → `agent pr` pushes" promise.

2. **Two multi-device data paths lose or leak data at the edges.** `P7-DATA-01` (P1): the revoke-triggered
   env-blob rewrap is the **one** env-profile writer that skips the source-coordinate compare-and-swap that
   `events.go` and `snapshot_import.go` both apply, so a concurrent newer `env.profile.updated` can be
   clobbered by stale re-encrypted content. `P7-SEC-01` (P1): a device that recovers via a hub snapshot
   never learns of a `device.revoked` compacted below the retention floor — the snapshot carries no trust
   state — so it keeps the revoked device `approved` and its next WCK rotation **re-grants the fresh epoch
   to the revoked device**, defeating `TRUST-01` forward-secrecy-on-revoke for the stale-approved recovering
   replica. `P7-SYNC-02` (P2): env profiles are encrypted only to devices approved *at capture time*, with no
   rewrap-on-approve, so a later-enrolled device gets the pointer and blob but can never decrypt it.

3. **The new carrier + service surfaces have small correctness edges.** Folder-carrier objects are written
   truncate-then-stream with no temp+rename (`P7-HUB-01`), exposing torn reads to cloud-drive replication;
   the shared stale-lock breaker is a non-atomic Stat-then-Remove that can double-grant the carrier lock
   (`P7-HUB-02`); `devstrap service` bakes an unvalidated/Cellar-resolved exec path into the unit
   (`P7-CLI-01`/`P7-SVC-01`) and a single global `DefaultLabel` silently overwrites a second workspace's
   service (`P7-SVC-02`); a transient blob GET permanently strands a referenced blob because the pointer
   event still consumes the cursor (`P7-SYNC-01`).

4. **The specs and gates drifted the same wave they documented.** `spec/00`'s "Not implemented yet" block
   and `ARCHITECTURE.md` still call the OS sandbox and LaunchAgent/systemd installers unbuilt after they
   shipped (`P7-DOC-01/02/03`); the convergence property generator never grew
   the new `env.profile.updated`/`draft.snapshot.created` event types (`P7-SYNC-03`); and the credential
   deny-set the OS sandbox enforces drifted from the advisory scanner's set (`.snowflake` missing, `P7-SANDBOX-02`).

The near-term imperative: **grant the linked-worktree git-common-dir in all three sandbox backends**
(`P7-SANDBOX-01`) so the default agent loop works; **add the CAS guard to the revoke rewrap and rewrap-on-approve
for env profiles** (`P7-DATA-01`, `P7-SYNC-02`); and **carry device-trust state in the snapshot** (`P7-SEC-01`)
so revocation survives compaction. Then close the carrier/service edges and re-sync the specs.

## Findings at a glance

| Dimension | P1 | P2 | P3 | Total |
|---|---|---|---|---|
| OS Agent Sandbox | 1 | 1 | 1 | 3 |
| Carrier Hub | 0 | 2 | 0 | 2 |
| Key & Trust Lifecycle | 1 | 0 | 1 | 2 |
| Sync Engine | 0 | 3 | 0 | 3 |
| Service Manager | 0 | 2 | 0 | 2 |
| CLI, UX & Data | 1 | 1 | 2 | 4 |
| Supply-Chain & CI | 0 | 0 | 1 | 1 |
| Specs, Docs & Process | 0 | 2 | 2 | 4 |
| **Total** | **3** | **11** | **7** | **21** |

> One further candidate (`P7-DOC-04`) was **refuted** at verification: spec/00's "remaining core-engine
> candidate" phrasing for `P4-GIT-03` is accurate — spec/14 itself marks it `[~]` partial (only
> containerization remains), so there is no contradiction.

## Prioritized roadmap

| # | Sev | ID | Recommendation | Dim | Effort |
|---|---|---|---|---|---|
| 1 | P1 | P7-SANDBOX-01 | Grant the linked worktree's git-common-dir write access in all three sandbox backends (+ an e2e that commits in a real linked worktree) | Sandbox | M |
| 2 | P1 | P7-DATA-01 | Add the `EnvProfileSourceCoords`/`envCoordLess` CAS guard to the revoke rewrap writer | Data | M |
| 3 | P1 | P7-SEC-01 | Carry device-trust/revocation state in the snapshot so revocation survives compaction | Sec | L |
| 4 | P2 | P7-SYNC-02 | Rewrap env blobs on device approve, not only on revoke | Sync | M |
| 5 | P2 | P7-SYNC-01 | Retry stranded referenced blobs on later syncs; don't consume the pointer's cursor when its blob is missing | Sync | M |
| 6 | P2 | P7-HUB-01 | Write folder-carrier objects temp+rename (atomic) | Hub | S |
| 7 | P2 | P7-HUB-02 | Make the stale-lock breaker atomic (rename/O_EXCL), not Stat-then-Remove | Hub | S |
| 8 | P2 | P7-CLI-01 | Validate `--exec-path` exists+executable before writing the unit | Svc/CLI | S |
| 9 | P2 | P7-SVC-01 | Don't `EvalSymlinks` the exec path into the Homebrew Cellar; keep the stable install path | Svc | M |
| 10 | P2 | P7-SVC-02 | Derive the service label from `--home`/workspace id; refuse to overwrite a different workspace's unit | Svc | M |
| 11 | P2 | P7-SANDBOX-02 | Add `.snowflake` (and `.config/gcloud`/`.azure`) to the OS-sandbox credential deny-set; unify with the advisory scanner set (`AGEN-05`) | Sandbox | S |
| 12 | P2 | P7-SYNC-03 | Fold `env.profile.updated`/`draft.snapshot.created` into the convergence property/model-check generator | Sync | M |
| 13 | P2 | P7-DOC-02 | ARCHITECTURE.md: Linux sandbox shipped, not "next slice" *(fixed in this PR)* | Doc | S |
| 14 | P2 | P7-DOC-03 | spec/00 "Not implemented yet": OS sandbox shipped *(fixed in this PR)* | Doc | S |
| 15 | P3 | P7-SUPPLY-01 | Have `install.sh` cosign-verify when `cosign` is present, or point curl\|sh users at the signed path | Supply | S |
| 16 | P3 | P7-SEC-02 | Owed-rotation retry: warn-and-continue on a transient epoch read, don't fail the sync | Sec | S |
| 17 | P3 | P7-SANDBOX-03 | Exclude credential files inside allowed `$HOME` build caches (`~/.cargo/credentials.toml`, …) under read-confine | Sandbox | S |
| 18 | P3 | P7-CLI-02 | `service install`/`uninstall` honor `--json` like `service status` | CLI | S |
| 19 | P3 | P7-CLI-03 | Classify a bad `--label` as `exitUsage(10)`, not `exitGeneric(1)` | CLI | S |
| 20 | P3 | P7-DOC-01 | ARCHITECTURE.md: `service` installers shipped, not "not built" *(fixed in this PR)* | Doc | S |
| 21 | P3 | P7-DOC-05 | quickstart.md: document the Linux sandbox + `service install` *(fixed in this PR)* | Doc | S |

---

## P1 findings

### P7-SANDBOX-01 (P1, M) — Sandbox write-allow omits the linked worktree's shared git dir → `git commit` EPERMs inside the default sandbox
`internal/platform/sandbox_profile.go:53-59` (+ `sandbox_bwrap_args.go:68-71`, `sandbox_landlock.go:117-121`)

**Defect.** All three sandbox backends re-allow writes to exactly `spec.WorktreeDir` and `spec.TmpDir`
and deny everything else. But `createFreshWorktree` (`internal/cli/worktree.go:139`) creates a git
**linked** worktree via `git worktree add` against the `~/Code` clone, so the worktree's index/HEAD/logs
live at `<clone>/.git/worktrees/<name>/…` and its objects/refs at `<clone>/.git/objects` + `.git/refs`
— none of which is under `WorktreeDir` or `TmpDir`. `SandboxSpec` (`sandbox.go:15-55`) has no field for the
git-common-dir, and `agentSandboxSpec` (`agent.go:66-77`) never grants the parent clone's `.git`.

**Failure scenario (verified, reproduced against a real linked worktree).** Under the default `guarded`
policy with `--sandbox auto` (`agent.go:382-383`) — the common Mac dev host — an agent that runs
`git add`/`git commit` inside its worktree has the write EPERM'd/EROFS'd by the kernel (Seatbelt
`(deny file-write*)`; bwrap `--ro-bind / /`; Landlock RWDirs = only the two dirs). The commit fails, so
`pushAgentBranch` (`agent.go:949`) does a bare `git push` of an unchanged branch — the "agent commits →
`agent pr` pushes" loop produces nothing, and the failure surfaces as a confusing EPERM buried in agent
output, not a loud error. The only sandbox+git e2e canary (`sandbox_landlock_e2e_test.go:82`) does
`git init -q sc && … commit` — a fresh *nested* repo, which never touches a shared parent `.git`, so the
linked-worktree path is untested.

**Verifier ruling.** CONFIRMED; **upgraded P2→P1** (opus verifier independently reproduced the write
targets with a real `git worktree add`+`commit`; confirmed all three backends and the missing spec field;
traced the caller chain). This is the default configuration and it silently breaks the product's core loop.

**Fix.** Resolve the worktree's `git rev-parse --git-common-dir` (and the per-worktree
`.git/worktrees/<name>` dir) and add them to the write allow-list in all three backends; plumb a
`GitCommonDir` onto `SandboxSpec`. Add an e2e that commits inside a real linked worktree under each
sandbox. *Anchor:* the Codex/Claude-Code sandboxes carve `.git` **read-only within writable roots** and
still allow the index/objects writes commits need — the inverse of the DevStrap gap.

### P7-DATA-01 (P1, M) — Revoke-triggered env-blob rewrap overwrites a concurrently-updated env profile (no CAS guard)
`internal/cli/blob_gc.go:93-96, 248-260`

**Defect.** `rewrapEnvBlob` reads the affected env profiles once (`EnvProfilesForBlobRef`, line 94), then
in a *later, separate* transaction unconditionally re-emits and upserts them via `emitSupersedingEnvProfile`
→ `tx.UpsertEnvProfileTx` (lines 248-260) — with **no** check of the profile's current source-event
coordinates. Every other env-profile writer in the ENV-SYNC-01 wave guards with a compare-and-swap:
`internal/sync/events.go:897-903` and `internal/sync/snapshot_import.go:142-148` both call
`tx.EnvProfileSourceCoords` + `envCoordLess` and return early if the local coordinates dominate. The
revoke rewrap is the one writer that skips it.

**Failure scenario (verified by both model families).** The profiles are read before any transaction;
between that read and the rewrap write, a concurrent `sync` applies a newer `env.profile.updated` (each in
its own `WithTx`). The rewrap then overwrites name/provider/mode and **deletes+reinserts all bindings**
(`store.go:1755-1797`) from the stale snapshot plus the new blob ref, clobbering the newer profile with
stale content. `MaxOpenConns(1)`+`_txlock=immediate` serialize individual transactions but do **not** make
the read and the later write one atomic read-modify-write.

**Verifier ruling.** CONFIRMED by opus (P2, race-window-bounded) and independently by gpt-5.5 (P1,
data-loss class, with line-level evidence). Recorded **P1**: silent overwrite of a newer env profile is
data-loss, and it is the lone writer missing the guard the wave established.

**Fix.** Wrap the read+rewrap in one transaction (or re-read inside the tx) and apply the same
`EnvProfileSourceCoords`/`envCoordLess` guard the other two writers use before upserting.

### P7-SEC-01 (P1, L) — Synced device revocations are lost on snapshot recovery → revoked device re-admitted to go-forward keys
`internal/sync/snapshot_build.go:20-107`, `snapshot_import.go:39-75`

**Defect.** `TRUST-01` (#132) propagates `device.revoked` through the event log only
(`events.go:916-947` → `ApplyRemoteDeviceTrustTx`). `BuildSnapshot` captures workspace id, producer, HLC,
floor, namespace entries, tombstones, and anchors — **no device/trust field** (the wire `Snapshot` struct,
`snapshot.go:81-99`, has none). `ImportSnapshot` never touches the `devices` table. Recovery pulls only
from `floor-1` (`snapshot_recovery.go:88-105`), and `CompactEventsBelow` deletes events below the floor
(`hub.go:124-127`), so a `device.revoked` compacted below the floor is unreachable, and the signed
retention manifest carries no trust state either.

**Failure scenario (verified by both families).** A replica C that already has device B marked `approved`
locally recovers via a snapshot (or joins after `hub compact`) whose floor is above B's `device.revoked`.
C never learns B was revoked, keeps B `approved`, and its next WCK `Rotate` grants the **fresh epoch** to
B (`ApprovedRecipients`, `keyring.go:358-408`) — defeating forward-secrecy-on-revoke. gpt-5.5 refined the
scope: the confirmed failure is the **stale-approved recovering replica**; a fresh empty-registry joiner is
not affected (nothing synthesizes an `approved` row for it).

**Verifier ruling.** CONFIRMED P1 by both families, with the scope refinement above.

**Fix.** Include the device trust table (at minimum the revoked/lost set, with signing-key ids) in the
snapshot, verified against the producing device's identity on import (mirror the fail-closed producer check
the snapshot already applies), so a compacted revocation is reconstructed on recovery before the next rotation.

---

## P2 findings

### P7-SYNC-02 (P2, M) — Env profile is undecryptable by any device approved *after* capture (no rewrap-on-approve)
`internal/cli/env.go:206-227`

`captureEnvProfile` encrypts the env bundle to the age-recipient set of devices approved **at capture time**
(`envRecipients` = local device + `ListDevices` filtered to `approved`). Only *revoke* rewraps existing env
blobs; there is no rewrap when a **new** device is later approved. So a device enrolled after capture syncs
the pointer (`env.profile.updated`) and the blob, but is not a recipient and can never decrypt it — the
symmetric hole to the draft plane, which the wave otherwise closed. CONFIRMED P2. **Fix:** on
`devices approve`, rewrap the workspace's env blobs to the enlarged recipient set (the revoke rewrap path is
the model; add the approve trigger).

### P7-SYNC-01 (P2, M) — Transient blob GET permanently strands a referenced env/draft blob
`internal/cli/sync.go:543-579`

`pullBlobsByRef` fetches blobs only for the current pull batch; on a `hub.GetBlob` error it does
`missing++; continue`, but the pointer event (`env.profile.updated`/`draft.snapshot.created`) is still
**consumed and advances the per-device Seq cursor**. A transient GET failure (network blip, eventual-consistency
lag on R2/carrier) is therefore never retried on later syncs — the blob is stranded and the profile/draft
stays unmaterializable with no self-heal. CONFIRMED P2. **Fix:** track referenced-but-missing blobs (a
pending set, like the undecryptable/skipped-event records) and re-attempt on subsequent syncs; don't treat
the pointer as fully consumed until its blob is present.

### P7-HUB-01 (P2, S) — Folder-carrier writes object payloads in-place (no temp+rename)
`internal/hub/gitcarrier.go:802-822` (`fsObjectStore.PutObject`/`PutObjectIfMatch`)

`PutObject` does a truncate-then-stream `MkdirAll`+write directly into the store root. For `FolderHub`, that
root is the shared Dropbox/iCloud/Google-Drive folder, so a peer's sync daemon can replicate and another
device can read a **partially-written** object. CONFIRMED P2. **Fix:** write to a sibling temp file and
`os.Rename` into place (atomic on POSIX); this also fixes the conditional-put read-back to see whole objects.

### P7-HUB-02 (P2, S) — Stale-lock breaker can double-grant the carrier lock
`internal/hub/folder.go:398-447` (`fsLock.acquire`)

On `EEXIST`, the breaker does a non-atomic `os.Stat`-then-`os.Remove` of the lock path when it looks stale.
Two waiting processes can both observe staleness, both `Remove`, and both re-create → both believe they hold
the single-writer lock, defeating the discipline the git and folder carriers depend on. The in-process
`l.mu` only serializes goroutines within one process. CONFIRMED P2. **Fix:** break the lock by `Rename`-ing
the stale file to a unique name and only proceeding if *your* rename won (compare-and-swap), or recreate via
`O_CREATE|O_EXCL` and treat `EEXIST` as "someone else won."

### P7-SVC-01 (P2, M) — `resolveServiceExecPath` resolves through the Homebrew Cellar symlink → unstable versioned path in the unit
`internal/cli/service.go:213-225`

It calls `filepath.EvalSymlinks(os.Executable())` before baking the result into `ServiceSpec.ExecPath`. For a
Homebrew install that resolves the stable `/opt/homebrew/bin/devstrap` symlink down to the version-pinned
`Cellar/devstrap/<version>/bin/devstrap` — so the next `brew upgrade` leaves the plist/unit pointing at a
deleted path, the opposite of the "stable location" the function's own error text asks for. CONFIRMED P2.
**Fix:** bake the *unresolved* stable path (or the caller-provided `--exec-path`) into the unit; only
`EvalSymlinks` to *validate* existence, not to rewrite.

### P7-SVC-02 (P2, M) — A single global `DefaultLabel` silently overwrites a second workspace's service
`internal/platform/service_darwin.go:25`, `service_linux.go:25`

`DefaultLabel()` returns one hardcoded string with no dependency on `--home`/`--root`/workspace id, and
`service.go` falls back to it whenever `--label` is omitted. Installing a service for a second `--home`
overwrites the first workspace's plist/unit (same label) with no persisted record of which home a label was
installed for. CONFIRMED P2. **Fix:** derive the default label from the workspace id (or `--home` hash);
refuse to overwrite a unit whose recorded home differs, unless `--force`.

### P7-CLI-01 (P2, S) — `service install` bakes an unvalidated `--exec-path` into the unit
`internal/cli/service.go:206-226`

`resolveServiceExecPath` accepts any absolute `--exec-path` verbatim with no `os.Stat`/executable check
(the implicit default is validated only incidentally by `EvalSymlinks` requiring existence), and neither
`service_darwin.go` nor `service_systemd.go` checks `ExecPath` existence — so a typo silently installs a
service that always fails to start. CONFIRMED P2. **Fix:** stat + executable-bit check the exec path before
writing the unit; fail with a usage error naming the bad path.

### P7-SANDBOX-02 (P2, S) — OS-sandbox credential deny-set drifted from the advisory scanner set
`internal/platform/sandbox_profile.go:7`

`sensitiveHomeDirs` (the sole anchor for the OS-enforced credential deny across all three backends) is
`{.ssh, .aws, .gnupg, .config/gh, .kube, .docker}` — it omits `~/.snowflake`, which the *advisory* layer
explicitly protects (`agent.go:622`, cited "AGEN-05: match the scan detector's set so the deny list and
scanner cannot drift"). So under `guarded`, an agent that spawns a subprocess to sidestep the argv-substring
advisory check can read `~/.snowflake/config.toml` despite the sandbox being active — the exact cross-layer
drift `AGEN-05` warns against, now between advisory and the kernel boundary. CONFIRMED P2 (verifier
narrowed: `.config/gcloud`/`.azure` are protected by *no* layer, so they are a pre-existing gap to add, not
"drift"). **Fix:** add `.snowflake` (and, while here, `.config/gcloud`/`.azure`) to `sensitiveHomeDirs`;
unify the OS deny-set and the advisory scanner set behind one source per `AGEN-05`.

### P7-SYNC-03 (P2, M) — New applied event types absent from the convergence property/model-check generator
`internal/sync/property_helpers_test.go:72`

The `P4-QUAL-02` property/model-check harness folds `genEventSet` over `Decide`, which handles only
`project.added/updated/deleted`. The new inline-applied `env.profile.updated` and `draft.snapshot.created`
LWW/idempotency logic (exactly the fast-written new code) is exercised only by hand-written example tests —
an unverified convergence gap. CONFIRMED P2. **Fix:** extend the generator to emit the new event types and
assert their convergence + duplicate-delivery idempotency, closing the property coverage the wave should
have grown with it.

### P7-DOC-02 (P2, S) — ARCHITECTURE.md says the Linux OS sandbox is still "the next slice" *(fixed in this PR)*
`ARCHITECTURE.md:140-141`

Reads "Linux OS-level confinement (bubblewrap/landlock/seccomp) is the next slice; until it lands the Linux
wrapper is advisory and says so at run start." — but bubblewrap + Landlock + seccomp shipped
2026-07-05 (PRs #121–#129). CONFIRMED P2 (a security capability materially understated). Fixed in this PR.

### P7-DOC-03 (P2, S) — spec/00's "Not implemented yet" block still lists OS-enforced agent sandboxing *(fixed in this PR)*
`spec/00_START_HERE.md:161`

"- OS-enforced agent sandboxing, project-env allowlists, and non-generic engine adapters;" sits under
"Not implemented yet (genuinely unbuilt…)". The OS-enforced sandbox shipped (`P4-GIT-03`,
Seatbelt+bwrap+Landlock+seccomp); only containerization remains (`[~]` in spec/14). CONFIRMED P2. Fixed in
this PR by moving the sandbox out of the unbuilt block and qualifying it "shipped except containerization."

---

## P3 findings

### P7-SUPPLY-01 (P3, S) — The recommended `curl|sh` installer never checks the cosign signature it advertises
`scripts/install.sh:65-90`

`install.sh` (the headline one-line install path) verifies the archive's sha256 against `checksums.txt`
downloaded from the **same, unauthenticated** release — it never invokes `cosign verify-blob` on
`checksums.txt.sigstore.json`. So an attacker who can serve/replace release assets slips a consistent
`(archive, checksums.txt)` pair past this path; the keyless signature protects nothing here. **Downgraded
P1→P3** by both families: README scopes `install.sh` to "verifies against checksums.txt" and the signed-verify
recipe is a cleanly-separated manual section, so it's a defense-in-depth gap on an optional convenience path,
not a broken promise. **Fix:** when `cosign` is on `PATH`, have `install.sh` verify the bundle at the pinned
identity; otherwise print a one-line pointer to the manual signed-verify path. *Anchors:* SLSA v1.1
verifying-artifacts; goreleaser example-supply-chain verify recipe.

### P7-SEC-02 (P3, S) — Owed-rotation retry turns a transient epoch-read error into a fatal sync
`internal/cli/sync.go:356-371`

In the self-healing owed-rotation retry (#137), a transient error reading the current epoch after a failed
`Rotate` is treated as fatal even in the `pending` path, aborting the whole sync cycle and blocking the
queued `device.revoked` from propagating — contradicting #137's stated intent that an early retry failure
should warn and let the cycle continue. CONFIRMED P3. **Fix:** warn-and-continue on the transient read in
the `pending` path; only fail when the rotation is genuinely required this cycle.

### P7-SANDBOX-03 (P3, S) — Read-confinement re-exposes credential files inside allowed `$HOME` build caches
`internal/platform/sandbox_read_confine.go:14-16`

`readConfineHomeCaches` whitelists whole `$HOME` cache dirs (`.cargo`, `.npm`, `.local`, …) for reading, but
some contain credential files not in the deny anchor set (e.g. `~/.cargo/credentials.toml`,
`~/.npmrc` is a file-level deny but `~/.config/*/credentials` under an allowed dir is not), so even the
strictest read-confine mode leaks them. CONFIRMED P3. **Fix:** intersect the cache allow-list with the
credential deny-set (deny-wins), or whitelist specific cache subdirs rather than whole dotdirs.

### P7-CLI-02 (P3, S) — `service install`/`uninstall` ignore `--json`, unlike `service status`
`internal/cli/service.go:87-96, 130-134`

`install`/`uninstall` always emit human text via `opts.progressf`; only `status` branches on `--json`.
CONFIRMED P3 (the open `P5-CLI-01`/#112 render-seam gap, now widened by the new command). **Fix:** route
`install`/`uninstall` results through the render seam with a typed JSON payload.

### P7-CLI-03 (P3, S) — A bad `--label` is misclassified `exitGeneric(1)` instead of `exitUsage(10)`
`internal/cli/service.go:85, 129, 171`

When `Install`/`Uninstall`/`Status` fail `validateServiceLabel` (e.g. `/` or leading `.` in `--label`), the
error is a bare `return err` (not `platform.ErrUnsupported` and not wrapped in an `appError`), so
`ExitCodeWithWriter` falls through to `exitGeneric(1)` — even though every other flag-validation failure in
the file is deliberately wrapped with a specific non-generic code and `root.go` documents `exitUsage(10)`
for bad-flag errors. CONFIRMED P3. **Fix:** wrap the label-validation error in `appError{exitUsage}`.

### P7-DOC-01 (P3, S) — ARCHITECTURE.md claims LaunchAgent/systemd installers are "deliberately not built" *(fixed in this PR)*
`ARCHITECTURE.md:148-152`

The "What is deliberately not built" section lists "LaunchAgent/systemd installers" alongside the daemon —
but `P4-PROD-04` shipped them as `devstrap service install|uninstall|status` (#139). CONFIRMED P3. Fixed in
this PR by moving the installers out of the "not built" list (the resident *daemon* stays correctly listed).

### P7-DOC-05 (P3, S) — quickstart.md's sandbox section documents only macOS Seatbelt *(fixed in this PR)*
`docs/quickstart.md:127-129`

Documents only the macOS Seatbelt sandbox, omitting the shipped Linux bubblewrap/Landlock/seccomp stack and
the shipped `devstrap service install` for unattended operation. CONFIRMED P3. Fixed in this PR.

---

## Appendix A — PASS4/PASS5 open items carried forward (not re-audited here)

These remain the live backlog; see `docs/audits/README.md` for status. After the #77–#140 wave the genuinely
open, buildable remainder is small: `P4-HUB-14` (hub op/byte counters — none exist in `internal/hub`),
`P4-HUB-15` (cost controls/quotas/rate-limiting), `P4-HUB-16` (R2 at-rest versioning/Object-Lock +
backup/replication runbook), `P4-QUAL-04` (Windows build — CI is macOS+Linux only), `P4-SYNC-03`
(`epochFloorMS`>0), `P4-SYNC-05` (signed per-device head), `P4-SYNC-07` (`MaxOpenConns(1)`), `P4-GIT-07`
(persisted materialize-failure record), `P4-SEC-08` (hosted-mode scoped creds — premature until hosted mode),
`P5-CLI-01`/#112 (render seam), `P4-QUAL-07` `contextcheck`/#113, and #111 (clone flag mutual-exclusivity).
Apple notarization is blocked on Developer-ID enrollment; the daemon/HTTP-SSE relay/StrapFS planes stay gated.

## Appendix B — ledger & spec truth-up state (#95–#140)

The **ledger** (`docs/audits/README.md`) was already reconciled through #140 by the shipping PRs
themselves (per convention #3): `P4-GIT-03` (OS sandbox, #107–#129), `P4-GIT-04` (worktree GC, #138),
`P4-PROD-04` (`service`, #139), `P4-PROD-05` (distribution, #105/#108), `P4-SEC-05` (narrowed to
notarization-only — cosign/SLSA/SBOM live-verified on v0.1.1, #115/#117/#119), `P4-QUAL-05`
(SBOM+provenance, #115/#117), `P4-SYNC-06` (ack wave), and the `reconcileSamePath` HLC-monotonic winner
(#95) are all in *Recently shipped*; `TRUST-01`/`ENV-SYNC-01`/the unattended-operation wave are noted in
the open-backlog blockquote. This pass adds only the **Pass-7 index row + open table** and a dated note.

What genuinely **drifted** (and is fixed in this PR) was the design-spec prose, not the ledger:
`spec/00_START_HERE.md` and `ARCHITECTURE.md` still described the OS sandbox and the LaunchAgent/systemd
installers as unbuilt (`P7-DOC-01/02/03`), and `docs/quickstart.md` documented only the macOS sandbox
(`P7-DOC-05`).

## External best-practice anchors (exa)

- **Agent sandbox escapes & pitfalls** — "A deep dive on agent sandboxes" (pierce.dev); "Inside the Codex
  Sandbox: Platform-Specific Implementation" (danielvaughan) — the zsh-fork bypass class, Seatbelt custom-policy
  holes, "workspace root ≠ security boundary", Ubuntu-24.04 AppArmor userns degradation; `cdxgen/safer-exec`.
  Informs `P7-SANDBOX-01/02/03`.
- **Supply-chain verification** — SLSA v1.1 "Verifying artifacts"; `slsa-framework/slsa-verifier`;
  `goreleaser/example-supply-chain`; cosign keyless issue #2659. Informs `P7-SUPPLY-01`.
- **Git carrier concurrency & E2E-encrypted git** — "High Performance Git" ch.18 (ref locking under write
  pressure); the single-committer pattern; git push atomicity (SO); "End-to-End Encrypted Git Services"
  (ACM CCS 2025 — confidentiality + repository unforgeability vs a malicious server). Confirmed the carrier's
  existing fetch-reset-retry + `--force-with-lease` design is sound; informs `P7-HUB-01/02`.
