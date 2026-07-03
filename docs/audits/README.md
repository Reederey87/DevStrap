# DevStrap Audit Archive

This directory holds DevStrap's chronological design & implementation audits. **This file is the single source of truth** for the audit program: the index of passes, the conventions for keeping them sane, and the **consolidated open backlog** (what's still actionable). Per-finding detail and `file:line` evidence live in each pass's own file.

> Moved here from the repo root on 2026-06-29 to declutter the root and end the cross-pass finding-ID collisions (fifth-pass finding `P5-PROC-01`).

## Index of passes

| Pass | Date | File | Scope | Findings | Status |
|---|---|---|---|---|---|
| 1 | (initial) | [`AUDIT_RECOMMENDATIONS.md`](AUDIT_RECOMMENDATIONS.md) | First-pass design & implementation review | — | Largely implemented / superseded by later passes |
| 2 | 2026-06-27 | [`AUDIT_RECOMMENDATIONS_2026-06-27.md`](AUDIT_RECOMMENDATIONS_2026-06-27.md) | Second pass: CI (`CI-*`), non-VCS/remote-less (`NOVCS-*`), forges (`FORGE-*`), working-state sync, zero-knowledge hub | 65 | Largely implemented / superseded |
| 3 | 2026-06-28 | [`AUDIT_RECOMMENDATIONS_2026-06-28.md`](AUDIT_RECOMMENDATIONS_2026-06-28.md) | Cloud-sync architecture: `EAGER-*`, `DRAFT-*`, `HUB-01..08`, `XP-*`, `SCALE-*` | workstreams | `EAGER-*`/`DRAFT-*`/`HUB-01..08`/`XP-*` shipped (PR #16); `SCALE-*` documented-not-built |
| 4 | 2026-06-28 | [`AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md`](AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md) | Fourth pass: audit of the now-shipped cloud-sync system | 44 (P1=17, P2=23, P3=4) | ~22 shipped/partial across PRs #20/#23/#25 (incl. P4-SEC-02, P4-SEC-07 foundation, P4-SEC-05 partial); remainder open — see below |
| 5 | 2026-06-29 | [`AUDIT_RECOMMENDATIONS_2026-06-29_PASS5.md`](AUDIT_RECOMMENDATIONS_2026-06-29_PASS5.md) | Fifth pass: adversarial review of the PASS4 batch + under-examined dimensions + new features | 36 (P1=1, P2=12, P3=23) | 32 shipped (PR #23); 4 open — see below |
| 6 | 2026-07-01 | [`AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`](AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md) | Sixth pass: adversarial audit of the PR #24/#25 batch (live R2 hub + envelope-encryption foundation) + under-examined dimensions | 43 (P1=5, P2=25, P3=13) | Open — see below |

## Conventions (going forward)

1. **Pass-scoped, globally-unique finding IDs.** Prefix every finding with its pass: `P4-SEC-01`, `P5-HUB-01`, etc. The bare-`ID` scheme of passes 1–4 caused collisions (`GIT-01` denotes a repo-lock bug in pass 2 *and* an empty-checkout bug in pass 4). Pass 5 onward is pass-scoped; this ledger back-labels earlier passes with `P<n>-` where it lists them.
2. **New audits live in `docs/audits/`**, not the repo root.
3. **Update this ledger every pass:** add the new file to the index and reconcile the open backlog (move shipped findings out, add new ones).
4. **Work-log rotation** (`spec/18_WORK_LOG.md`, now 1,200+ lines): rotating older cycles into a dated archive is recommended but deliberately deferred to keep each PR reviewable; tracked as a follow-up (see `P6-DOC-02`, which recommends promoting this from a convention bullet into an actual backlog row).
5. The spec-drift gate (`internal/specdrift`) treats any change under `docs/` as requiring a `spec/18_WORK_LOG.md` entry, and the four specs that track audit files (`spec/00`, `spec/12`, `spec/14`, `spec/17`) point their `tracks_code:` frontmatter at `docs/audits/`.

## Open backlog — single source of truth for "what's left"

Currently-actionable findings, pass-scoped. Earlier passes (1–3) are largely implemented or superseded (see `spec/18_WORK_LOG.md` for the shipped history); the open backlog is concentrated in passes 4, 5, and 6.

> **2026-07-01 — Pass 6 landed.** The sixth pass ([`AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`](AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md)) audited trunk `8c739b8` (PR #25) via a verification-driven nine-dimension workflow: 43 findings (P1=5, P2=25, P3=13), each adversarially verified against the code and checked for novelty against this backlog. Headlines: `P6-SEC-01`/`P6-SYNC-01` (the envelope layer still trusts the hub — unverified grant ingestion + whole-batch abort on one bad event), `P6-HUB-01`/`P6-DATA-01` (the now-live hub GC deletes live draft blobs), and `P6-GIT-01` (a universal 2-minute git timeout silently breaks eager materialization of large repos). This pass also **reconciled the ledger** below per convention #3: `P4-SEC-02` moved to *Recently shipped*, `P4-SEC-05` corrected to *partial* (its own finding is `P6-DOC-02`). **2026-07-02 update:** the hub-trust workstream (`P6-SYNC-01`, `P6-SEC-01`, `P6-SEC-02`; PRs #30–#34), `P6-DATA-01` (PR #35), `P6-HUB-01` (PR #36), and `P6-GIT-01` are shipped and moved to *Recently shipped* — **all five Pass 6 P1s are closed** (the AD-2 hardening-freeze P1 wave is complete). **2026-07-03 update:** `P6-DATA-02` is shipped; the open backlog is now the remaining P2/P3 set below. A separate 2026-07-03 **pairing wave** (`init --join --workspace-id` adoption, the founder-pinning ceremony, doctor mismatch detection, and the two-device runbook; PRs #48–#50 + the docs PR) closed the cross-device workspace-ID pairing remainder of `P4-SEC-07` and the joiner half of `P4-SEC-04` — both are Pass-4 findings tracked in the Pass 4 table, so the **Pass-6 count is unchanged** (they narrow, not close, and never counted toward the 43).

<!-- MD028 separator between adjacent dated blockquotes -->

> **2026-06-30 — most of Pass 5 shipped.** The PASS5 implementation cycle (`fix/pass5-backlog`, see `spec/18_WORK_LOG.md`) landed **32 of the 36** Pass-5 findings (now including `P5-HUB-01`) plus `P4-SEC-05` (partial — `goreleaser-action` SHA-pin only) and `P4-QUAL-07` (partial). **Still open:** `P5-SYNC-01` (transport-cursor redesign — deferred with design in `spec/07`, latent), `P5-CLI-01` (the `render` seam landed and is wired into `materialize`; full rollout to every leaf command remains), `P5-ARCH-01` (convergence property tests shipped; the formal pure `Decide` extraction remains), and `P4-QUAL-07`'s `contextcheck` (deferred — needs threading a context through the forge chain). `P5-HUB-01` shipped today (branch `fix/p5-hub-01`): the `aws-sdk-go-v2` S3 adapter is wired behind `hub: r2://<bucket>` (`hubFromOptions`), with `DEVSTRAP_HUB_S3_*` env/config credentials, `aws.NopRetryer{}` single-retry, and an env-gated MinIO conformance test (`TestR2MinIOConformance`) plus hermetic `mapS3Error`/conformance unit tests. The PASS4 carried-forward XL items (`SEC-07` envelope encryption **foundation shipped** 2026-06-30, `GIT-03` OS sandbox, `SEC-02` **shipped** 2026-06-30, `SEC-04`, `SYNC-02`/`HUB-11` compaction) — `SEC-07` full workspace-ID pairing and `SEC-08` remained open as of that date (pairing shipped 2026-07-03; see the Pass-4 table below).

### Recently shipped (moved out of "still open" per convention #3)

| ID | Sev | Shipped | Note |
|---|---|---|---|
| P4-SEC-02 | P1 | PR #25 (`8c739b8`) | Namespace-map event log is envelope-encrypted at rest on the hub (`internal/sync/eventcrypt.go`, `encryptedhub.go`). Fully shipped — no longer open. |
| P4-SEC-07 | P2 | PR #25 (`8c739b8`), foundation | WCK epoch keyring (`internal/workspacekeys`) + age-wrapped grants + `Rotate` on revoke. Foundation only; **open remainder** tracked below. |
| P4-SEC-07 (pairing) | P2 | pairing wave (2026-07-03, PRs #48–#50 + docs) | Cross-device workspace-ID pairing shipped: `devstrap init --join --workspace-id <id>` adopts the founder's `ws_<uuidv7>` at init (born-correct — a mismatch on an already-initialized store refuses with a remove-and-reinit remedy), `devstrap status` / `devices recipient --workspace-id` surface the id, a keyless joiner pins the founder (`devices enroll … --approve`) before first sync — closing the joiner half of the `P4-SEC-04` TOFU window — and `doctor --remote` detects the joiner/empty-prefix mismatch signature (regression-tested by `TestR2WorkspacePrefixIsolation`). Docs: `spec/19` §E runbook, `spec/07` init lifecycle, `spec/15` threat note, README two-device quickstart. Open remainders (Pass 4 table): `P4-SEC-07` periodic (non-revoke) rotation; `P4-SEC-04` part 2 — one-paste pairing code + founder-side automation (fingerprint confirmation shipped 2026-07-03). |
| P6-SYNC-01 | P1 | PR #30 | Per-event `event_verification_failure` quarantine + approve-time replay; one bad signed event no longer wedges the batch. Open residual: synced `device.revoked` trust propagation (revoke is still local-only). |
| P6-SEC-01 | P1 | PRs #31/#33/#34 | (a) grant carriers verified before WCK ingestion; (b/c) `(epoch, kid)`-addressed custody, overwrite refusal, grant-preferred `PushKey`, replay-time grant ingestion. Fully shipped. |
| P6-SEC-02 | P2 | PRs #32/#33 | Founder/join split (`init --join`, pull-before-push, founder gate) + `(epoch, kid)` keying; a joiner never self-mints or loses pre-approval events. Open residuals: `P6-SEC-03` truncate wedge, `P4-SEC-04` part 2 (one-paste pairing code; fingerprint confirmation shipped). |
| P6-DATA-01 | P1 | PR #35 (2026-07-02) | Origin records its own `draft_snapshots` row in one transaction with the `draft.snapshot.created` event (`InsertLocalEventTx` + `RecordDraftSnapshotTx`), on both the create and revoke-rewrap paths; e2e-pinned by `draft_snapshot_gc_retains_origin.txtar`. |
| P6-HUB-01 | P1 | PR #36 (2026-07-02) | `hub gc` is sync-first (pre-GC pull+apply via `pullAndApplyEvents`, including blob caching), refuses to sweep on any truncated/skipped pull (`PullStats`), quarantined/cursor-held apply (`ApplyEventsWithStats`), or open quarantine conflict, and keeps unreferenced blobs younger than `--grace-window` (default 24h; `Hub.ListBlobs` now carries `LastModified`). Skew quarantines auto-resolve when their event later applies, so a clock hiccup cannot block gc forever. E2e-pinned by `hub_gc_stale_marks.txtar`. Follow-ups: signed retention marker (`P6-HUB-04`); sweep lock + dedup-`PutBlob` `LastModified` refresh, so gc racing a >window-late recovery sync cannot delete a just-re-referenced blob (`P4-HUB-12`); skipped-at-log-tail keeps gc refused until any newer event advances the cursor (`P6-SEC-03`'s class). |
| P6-GIT-01 | P1 | `fix/p6-git-01` (2026-07-02) | Git timeout split by command class: `Runner.LongTimeout` (default 30m, config `materialization.clone_timeout`, `gitRunner(opts)` at every CLI call site) applies per attempt to clone/fetch/LFS; a self-imposed deadline kill is the distinct terminal `ErrTimeout` (network exit code), ending the wipe-and-retry — a >2-minute blobless clone now completes. Pinned by the one-attempt/no-wipe fake-git tests + config round-trips. **Completes all five Pass 6 P1s (AD-2 P1 wave).** |
| P6-SYNC-03 | P2 | PR #38 (2026-07-03) | Sticky fail-closed enrollment: `hasEnrolledDevices` counts `trust_state IN ('approved','revoked','lost')`, so revoking/losing the last approved device keeps verification fail-closed instead of reopening the pre-enrollment window; post-revoke traffic (revoked or unknown devices) quarantines per `P6-SYNC-01`. Pending placeholders still don't count; the never-enrolled bootstrap window (`P4-SEC-04`) is unchanged. Pinned by `TestHasEnrolledDevicesStickyAfterRevoke` + `TestApplyEventsRevokedLastDeviceStaysFailClosed`. Residual: synced `device.revoked` trust propagation. |
| P6-DATA-02 | P2 | `fix/p6-data-02` (2026-07-03) | `ClearRotationForProject` now filters via `namespace_entries.env_profile_id` instead of the non-existent `env_profiles.namespace_id`; store coverage proves per-project isolation and CLI coverage proves one-arg `env rotate <path>` succeeds. |
| P6-CLI-02 | P2 | `fix/p6-cli-02` (2026-07-03) | `scan <dir> --adopt` is gated on the scanned root naming the same directory as the workspace root (`sameResolvedDir`: byte-exact after `EvalSymlinks`, no case-folding; refusal is `exitUsage`), so one command can no longer rewrite the fleet namespace with out-of-tree repos; adoption proceeds under the canonical root spelling and read-only scans of arbitrary directories keep working. Pinned by `TestScanAdoptRefusesNonWorkspaceRoot`/`...ExplicitWorkspaceRootSucceeds`/`...AcceptsSymlinkedWorkspaceRoot`/`...ReadOnlyAllowsNonWorkspaceRoot`. |
| P6-GIT-05 | P2 | `fix/p6-git-05` (2026-07-03) | `createFreshWorktree` failures after `git worktree add` (LFS pull, current-device lookup, DB insert) now remove the just-created checkout and delete its `agent/...` branch under a detached bounded context (`context.WithoutCancel` + 2m cap, so a Ctrl-C that caused the failure cannot no-op the cleanup); the `agent run` file-policy-denial cleanup deletes the branch too, and the LFS error names the worktree path. Pinned by `TestCreateFreshWorktreeCleansUpAfterLFSPullFailure`/`...AfterInsertWorktreeFailure`. Doctor orphan-worktree check deliberately out of scope. |
| P6-SYNC-04 | P2 | PR #44 (2026-07-03) | Hard cut to `enc.v2`: the AEAD AAD binds the full carrier tuple (ID, DeviceID, sealing-key kid, Seq, HLC, epoch), the signature domain moves to `devstrap:event:v2` (+`device_id`/`seq`, v1 verify fallback for re-pushed history), and a held-key AEAD failure forwards the carrier to an `undecryptable` quarantine conflict that the per-pull replay auto-recovers once its grant arrives. v1 is dead (loud skip + re-found guidance). |
| P6-QUAL-03 | P2 | `fix/p6-qual-03` (2026-07-03) | A `minio-conformance` ubuntu CI job boots a digest-pinned `minio/minio` via `docker run` (checkout with `persist-credentials: false`) and runs `TestR2MinIOConformance` against it on every push/PR, so the production `aws-sdk-go-v2` S3 adapter is now exercised against a real backend in CI instead of only the in-memory `memS3` fake; `go test ./...` stays hermetic by default. |
| P6-HUB-02 | P2 | `fix/p6-hub-02` (2026-07-03) | Hub S3 credentials resolve env/config (`op://` refs via `op read`, 60s-bounded) → `AWS_*` → per-workspace keychain slot written by new `devstrap hub login`/`logout` (0600 file fallback under `DEVSTRAP_NO_KEYCHAIN`); resolved secret rides `redact.Secret` (with struct-level Stringer guards); auth failures map to `ErrS3Auth` with a hint. spec/13/15/19 reconciled (age-blob variant deliberately not built). |
| P6-SEC-03 | P2 | `fix/never-granted-epoch-grace` (2026-07-03) | Never-granted epochs no longer truncate sync forever: the missing-key defer is grace-bounded (`sync.key_grant_grace`, default 72h, `0` = immediate; first sighting persisted in `key_grant_waits` (migration `00015`) with the epoch's earliest first-seen as the clock, so hostile kid relabeling cannot restart it); past the window the carrier quarantines as a replay-recoverable `undecryptable` conflict and the cursor advances; `ReplayUndecryptableConflicts` now runs BEFORE the batch applies (one-cycle recovery) and a late-applying successor auto-resolves its `event_hash_chain_break` conflict; `devices approve`/`enroll --approve` gain the epoch-contiguity guard (`--allow-epoch-gap` override; keyless pinning ceremony exempt); `doctor` warns `awaiting key grants`. E2e-pinned by `sync_never_granted_epoch_wedge.txtar`. Residual (documented): a rotator grants only locally-known approved devices — unknown fleet devices ride grace→quarantine→replay until re-approved; old-epoch containment documented-not-built. |
| P6-CLI-05 | P3 | `fix/p6-cli-05` (2026-07-03) | README project-status/features/quickstart/roadmap now document the shipped `hub: r2://<bucket>` + `DEVSTRAP_HUB_S3_*` path (with `hub login`/`op://` custody and a spec/19 link); both `init` next-steps hints teach configuring the hub in config.yaml; `sync --dry-run` prints the resolved hub ID instead of an empty target. Non-goal: no `init --hub` flag. |
| P6-DOC-02 | P2 | audit PR #28 | Ledger P4-SEC-05 contradiction + convention-#3 violation reconciled in the PR that landed the audit. Fully applied. |
| P6-DOC-03 | P3 | audit PR #28 | spec/00 re-drift (planned-sync comment, command/test inventories) fixed in the PR that landed the audit. Fully applied. |

### Pass 6 (2026-07-01) — 26 open of 43; **all five P1s shipped**; **P2 quick-win wave complete**

> The header count equals the rows in the table below (CodeRabbit, PR #39): 43 findings − 17 shipped **P6** rows in *Recently shipped* (the `P4-SEC-02` + `P4-SEC-07` foundation + `P4-SEC-07 (pairing)` rows there are Pass-4 findings and do **not** count toward Pass 6's 43; the 17 include the 2 fully-applied doc fixes `P6-DOC-02`/`P6-DOC-03`) = 26. `P6-DOC-01`/`P6-DOC-04` stay listed because their test-hardening residuals are open even though their doc portions were applied in the audit PR. `P6-SYNC-01`, `P6-SEC-01`, `P6-SEC-02` (PRs #30–#34), `P6-DATA-01` (PR #35), `P6-HUB-01` (PR #36), `P6-GIT-01` (PR #37), `P6-SYNC-03` (PR #38), `P6-DATA-02` (PR #39), `P6-CLI-02` (PR #40), `P6-GIT-05`, `P6-SYNC-04` (PR #44), `P6-QUAL-03` (`fix/p6-qual-03`), and `P6-SEC-03` (`fix/never-granted-epoch-grace`) moved to *Recently shipped* above per convention #3.

| ID | Sev | Effort | Finding |
|---|---|---|---|
| P6-CLI-01 | P2 | S | Detect a root change on re-`init`; rewrite config.yaml or refuse |
| P6-DATA-03 | P2 | M | Emit event + state mutation in one transaction at every emission site |
| P6-DATA-04 | P2 | M | Ship `db backup --full` (blobs + keys) and a restore path/runbook |
| P6-DOC-01 | P2 | S | Fix spec/13's stale status block; document `env rotate`; path-anchor the command gate _(doc portion applied this PR; command-gate test hardening open)_ |
| P6-GIT-02 | P2 | S | Diff `agent` runs against the recorded base SHA, not just the working tree |
| P6-GIT-03 | P2 | S | Run dependency rebuild before `.env` hydrate; capture a 0600 log |
| P6-GIT-04 | P2 | M | Honor the stored `lfs_policy` on materialize/hydrate (mirror the worktree path) |
| P6-QUAL-01 | P2 | S | Exclude catch-all specs (`**`) from the mapped-spec drift check |
| P6-QUAL-02 | P2 | S | Add a `verify` job gating release on tests + vuln + main-ancestry |
| P6-SYNC-02 | P2 | M | Split skip classes by recoverability; quarantine + surface skipped events |
| P6-XP-01 | P2 | S | Delete `ShouldPruneDir`'s bare-name fallback; make relSlash authoritative |
| P6-XP-02 | P2 | M | Align the ignore compiler with real gitignore semantics |
| P6-XP-03 | P2 | M | Implement `run-loop`'s advertised scan stage (or fix the docs) |
| P6-XP-04 | P2 | M | Never mint a WCK when one is published; type-check keychain errors |
| P6-XP-05 | P2 | M | Keep `scan` offline; defer `set-head --auto` to materialization |
| P6-CLI-03 | P3 | S | Wire Cobra usage errors to `exitUsage=10`; fix the spec table |
| P6-CLI-04 | P3 | S | Make `--quiet` actually suppress progress chatter (or fix its help) |
| P6-DATA-05 | P3 | S | Add `idx_events_device_hlc` to serve the hot push/doctor query |
| P6-DATA-06 | P3 | S | Add a single-`local`-device partial unique index + race-tolerant `EnsureDevice` |
| P6-DOC-04 | P3 | S | Add `internal/workspacekeys/**` to the spec/07/09/15 `tracks_code` frontmatter _(applied this PR; new-package mapping gate test open)_ |
| P6-GIT-06 | P3 | S | Gate `agent pr` on run status; reconcile crash-stuck `running` rows |
| P6-HUB-03 | P3 | S | Fan-out `R2Hub.Push` in HLC-ordered waves |
| P6-HUB-04 | P3 | M | Give the retention floor a signed hub-side marker object |
| P6-QUAL-04 | P3 | S | Stub `ssh` via a PATH shim so the alias tests are hermetic |
| P6-QUAL-05 | P3 | S | Scope CI push triggers to main + add `concurrency` cancellation |
| P6-XP-06 | P3 | S | Compile the scan prune matcher from the root `.devstrapignore` |

### Pass 5 (2026-06-29) — 32 shipped (2026-06-30), 4 open: `P5-SYNC-01`, `P5-CLI-01`, `P5-ARCH-01` (partial), `contextcheck`

| ID | Sev | Finding | Effort |
|---|---|---|---|
| P5-SEC-01 | P1 | Revoke rewrap deletes the old hub blob with no superseding event → other devices lose draft access | L |
| P5-SYNC-02 | P2 | Drop device-local `namespace_id` from the `conflict.resolved` match so resolution converges | S |
| P5-QUAL-01 | P2 | Stop `materialize` counting "no draft bundle yet" as failure (perpetual non-zero exit) | M |
| P5-SYNC-04 | P2 | Make `conflicts resolve --keep-*` actually mutate namespace state (or relabel advisory) | M |
| P5-SYNC-03 | P2 | Write a tombstone at the old path on rename so a stale add/update can't resurrect it | M |
| P5-SYNC-01 | P2 | Key the pull cursor on hub ingestion order, not logical HLC, so late events aren't stranded | L |
| P5-HUB-01 | P2 | Wire a real S3 adapter + `hubFromOptions` factory + MinIO/LocalStack integration test — **shipped 2026-06-30** (`fix/p5-hub-01`) | L |
| P5-HUB-02 | P2 | Hub-side GC + draft-snapshot pruning so superseded blobs are reclaimable | M |
| P5-SEC-02 | P2 | Bound the decompression budget on *every* tar entry, not just `TypeReg`/`TypeDir` | S |
| P5-SEC-03 | P2 | Run dependency-rebuild lifecycle scripts through `childenv`, not the raw parent env | S |
| P5-QUAL-02 | P2 | Add tests for `run-loop` and the `draft` cross-device round-trip | M |
| P5-CLI-01 | P2 | Introduce a `Renderer`; honor `--json` everywhere (or reject it) | M |
| P5-DX-02 | P2 | Make the spec-drift gate detect content/inventory staleness, not just file-touch | M |
| P5-SEC-04 | P3 | Don't push local-only env secret blobs to the hub during revoke rewrap | M |
| P5-SEC-05 | P3 | Cap `redact.Writer`'s line buffer so a newline-free stream can't grow memory unbounded | S |
| P5-ARCH-01 | P3 | Extract a pure `Decide(state, event)` layer so convergence can be property-tested | M |
| P5-HUB-03 | P3 | Give `R2Hub.Pull` a retention floor + `ErrSnapshotRequired` before compaction lands | M |
| P5-HUB-04 | P3 | Fetch the pull log with bounded concurrency + an exclusive composite cursor | M |
| P5-CLI-02 | P3 | Wire or remove the dead `materialize --partial` flag | S |
| P5-CLI-03 | P3 | Reject `--open`/`--vscode` conflict before the network clone (`MarkFlagsMutuallyExclusive`) | S |
| P5-CLI-04 | P3 | Use `ssh -G` (or honor `Include`/negation) for SSH host-alias resolution | M |
| P5-CLI-05 | P3 | Route progress/warnings to stderr; surface repeated `run-loop` tick failures | S |
| P5-DX-01 | P3 | Add dynamic shell completion for namespace paths and enum flags | M |
| P5-QUAL-03 | P3 | Clamp the `run-loop` jitter bound so `rand.Int64N(0)` can't panic | S |
| P5-QUAL-04 | P3 | Consume or drop the CI coverage profile (gate, upload, or remove) | S |
| P5-QUAL-05 | P3 | Emit `tar.TypeDir` headers so draft bundles preserve empty directories | S |
| P5-PROD-01 | P3 | Reconcile `deriveDisplayStatus` with the states writers actually produce ("ready" is dead) | M |
| P5-PROD-02 | P3 | Make the default local-only revoke's hub-cleanup promise real (pending-delete queue) | M |
| P5-PROD-03 | P3 | Add `devstrap env rotate` + a `needs_rotation` clear path | M |
| P5-PROD-04 | P3 | Document the shipped `devstrap clone` in the README | S |
| P5-PROD-05 | P3 | New: `doctor --remote` hub-health probe + `status --watch`/TUI convergence view | L |
| P5-DOC-01 | P3 | Fix spec/07's false "not yet implemented" claims about shipped DRAFT/HUB code | M |
| P5-DOC-02 | P3 | De-contradict spec/00's "Not implemented yet" block; add the 7 missing commands | M |
| P5-DATA-01 | P3 | Reconcile spec/12's migration/index inventory (00010 collision) | S |
| P5-DATA-02 | P3 | Add a DB `UNIQUE` index for `draft_snapshots` idempotency (defense-in-depth) | S |
| P5-PROC-01 | P3 | Consolidate audit files + adopt pass-scoped IDs + a single status ledger _(this PR)_ | M |

### Pass 4 (2026-06-28) — still open (re-prioritized in PASS5 Appendix A)

| ID | Sev | Finding |
|---|---|---|
| P4-SEC-02 | — | **Shipped PR #25** — moved to _Recently shipped_ above (was here in violation of convention #3; fixed by `P6-DOC-02`). |
| P4-SEC-04 | P1 | Close the pre-enrollment bootstrap window — the **joiner half is closed** by the 2026-07-03 founder-pinning ceremony (a keyless joiner pins the founder via `devices enroll … --approve` before first sync; `devices_pin_founder_test.go` + the `sync_join_flow` e2e; runbook: `spec/19` §E.2), the **fingerprint-confirmation half is shipped** (part 1, 2026-07-03): a full 256-bit device fingerprint (`internal/devicekeys/fingerprint.go`) binding the signing key + age recipient, with a compare-and-confirm gate on `devices approve` / `enroll --approve` before any DB write and `SECU-05` keyless-placeholder refusal (`spec/13`, `spec/15`), and the one-paste `devstrap-pair1:` pairing code + founder-side enrollment composition is now shipped (part 2, 2026-07-03). Residual closed by parts 1+2; row moves to Recently shipped in the wave close-out docs PR. |
| P4-SEC-05 | P1 | Sign release binaries — **partial 2026-06-30**: `goreleaser-action` SHA-pinned (`release.yml`); cosign keyless signing + SLSA provenance + SBOM still open (fold into `P4-QUAL-05`; see `P6-QUAL-02`) |
| P4-SEC-07 | P2 | Envelope encryption (KEK/DEK) + key rotation + forward secrecy — **foundation shipped PR #25** (WCK epoch keyring + age-wrapped grants + Rotate on revoke); **workspace-ID pairing shipped 2026-07-03** (`init --join --workspace-id` adopts the founder's id; surfaced by `status`/`devices recipient --workspace-id`; doctor-side mismatch detection + the `TestR2WorkspacePrefixIsolation` hub prefix-isolation regression test shipped in the same wave); remainder **shipped 2026-07-03** (`feat/periodic-wck-rotation`): `keys rotate` + age-triggered auto-rotation in `sync` (`keys.rotate_max_age`, default 90d) — forward exposure only, pure Rotate, no revoke side effects; row moves to *Recently shipped* in the wave's docs close-out PR. Documented-not-built: old-epoch containment, keychain-slot growth |
| P4-SEC-08 | P2 | Hosted-mode prefix-scoped/temporary credentials + object immutability |
| P4-SYNC-02 | P1 | Event-log compaction + snapshot exchange (events table grows forever) |
| P4-SYNC-03 | P2 | Raise `epochFloorMS` above 0; past-direction quarantine |
| P4-SYNC-05 | P2 | Folded running hash chain + signed per-device head |
| P4-SYNC-06 | P2 | Wire tombstone GC + per-peer cursor/delivery tables; enforce GC-safety invariant |
| P4-SYNC-07 | P3 | `MaxOpenConns(1)` serializes all WAL reads behind the single writer |
| P4-SYNC-08 | P3 | Unblock the multi-workspace future + sign re-stamped workspace-id binding |
| P4-HUB-11 | P1 | Event-log compaction / working-snapshot exchange for R2 (bound Pull cost/memory) |
| P4-HUB-12 | P1 | Hub Delete path — primitive shipped via SEC-01; full mark-and-sweep GC still open (see P5-HUB-02) |
| P4-HUB-14 | P2 | Emit hub metrics/traces + op/byte counters (partly delivered by P5-PROD-05 `doctor --remote`) |
| P4-HUB-15 | P2 | Cost controls, quotas, rate limiting |
| P4-HUB-16 | P2 | At-rest versioning/Object-Lock + backup/replication runbook |
| P4-GIT-03 | P1 | OS-enforced agent sandbox (XL) — P5-SEC-03 is a cheap interim env-sanitization step |
| P4-GIT-04 | P2 | Worktree GC that reaps squash/rebase-merged worktrees |
| P4-GIT-07 | P3 | Persisted per-project materialize failure record, resume, progress detail |
| P4-QUAL-02 | P1 | Property/model-check HLC monotonicity + conflict convergence (P5-ARCH-01 unblocks this) |
| P4-QUAL-04 | P2 | Enforce coverage in CI + add a Windows build (P5-QUAL-04 is the concrete sub-step) |
| P4-QUAL-05 | P2 | SBOM + build provenance beyond `checksums.txt` |
| P4-QUAL-07 | P3 | Enable resource/context-leak linters (`bodyclose`, `sqlclosecheck`, `contextcheck`, …) |
| P4-PROD-04 | P2 | `devstrap service install` (LaunchAgent/systemd unit wrapping `run-loop`) |
| P4-PROD-05 | P2 | Distribution: Homebrew tap + `curl\|sh` installer + shell completions (P5-DX-01 is a prerequisite) |

### Pass 3 (2026-06-28 cloud-sync) — residual

| ID | Status |
|---|---|
| `SCALE-*` | Documented-not-built (multi-user hosting/scaling direction; Fly.io + R2 + managed Postgres). Future/strategic. |

_Passes 1–2 (`AUDIT_RECOMMENDATIONS.md`, `..._2026-06-27.md`) are largely implemented or superseded by the cloud-sync re-baseline; consult `spec/18_WORK_LOG.md` for the shipped history before treating any pass-1/2 finding as open._
