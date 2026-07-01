# DevStrap Audit Archive

This directory holds DevStrap's chronological design & implementation audits. **This file is the single source of truth** for the audit program: the index of passes, the conventions for keeping them sane, and the **consolidated open backlog** (what's still actionable). Per-finding detail and `file:line` evidence live in each pass's own file.

> Moved here from the repo root on 2026-06-29 to declutter the root and end the cross-pass finding-ID collisions (fifth-pass finding `P5-PROC-01`).

## Index of passes

| Pass | Date | File | Scope | Findings | Status |
|---|---|---|---|---|---|
| 1 | (initial) | [`AUDIT_RECOMMENDATIONS.md`](AUDIT_RECOMMENDATIONS.md) | First-pass design & implementation review | — | Largely implemented / superseded by later passes |
| 2 | 2026-06-27 | [`AUDIT_RECOMMENDATIONS_2026-06-27.md`](AUDIT_RECOMMENDATIONS_2026-06-27.md) | Second pass: CI (`CI-*`), non-VCS/remote-less (`NOVCS-*`), forges (`FORGE-*`), working-state sync, zero-knowledge hub | 65 | Largely implemented / superseded |
| 3 | 2026-06-28 | [`AUDIT_RECOMMENDATIONS_2026-06-28.md`](AUDIT_RECOMMENDATIONS_2026-06-28.md) | Cloud-sync architecture: `EAGER-*`, `DRAFT-*`, `HUB-01..08`, `XP-*`, `SCALE-*` | workstreams | `EAGER-*`/`DRAFT-*`/`HUB-01..08`/`XP-*` shipped (PR #16); `SCALE-*` documented-not-built |
| 4 | 2026-06-28 | [`AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md`](AUDIT_RECOMMENDATIONS_2026-06-28_PASS4.md) | Fourth pass: audit of the now-shipped cloud-sync system | 44 (P1=17, P2=23, P3=4) | ~19 shipped (PR #20), ~25 open — see below |
| 5 | 2026-06-29 | [`AUDIT_RECOMMENDATIONS_2026-06-29_PASS5.md`](AUDIT_RECOMMENDATIONS_2026-06-29_PASS5.md) | Fifth pass: adversarial review of the PASS4 batch + under-examined dimensions + new features | 36 (P1=1, P2=12, P3=23) | Open — see below |

## Conventions (going forward)

1. **Pass-scoped, globally-unique finding IDs.** Prefix every finding with its pass: `P4-SEC-01`, `P5-HUB-01`, etc. The bare-`ID` scheme of passes 1–4 caused collisions (`GIT-01` denotes a repo-lock bug in pass 2 *and* an empty-checkout bug in pass 4). Pass 5 onward is pass-scoped; this ledger back-labels earlier passes with `P<n>-` where it lists them.
2. **New audits live in `docs/audits/`**, not the repo root.
3. **Update this ledger every pass:** add the new file to the index and reconcile the open backlog (move shipped findings out, add new ones).
4. **Work-log rotation** (`spec/18_WORK_LOG.md`, now 1,100+ lines): rotating older cycles into a dated archive is recommended but deliberately deferred from the consolidation PR to keep it reviewable; track it as a follow-up.
5. The spec-drift gate (`internal/specdrift`) treats any change under `docs/` as requiring a `spec/18_WORK_LOG.md` entry, and the four specs that track audit files (`spec/00`, `spec/12`, `spec/14`, `spec/17`) point their `tracks_code:` frontmatter at `docs/audits/`.

## Open backlog — single source of truth for "what's left"

Currently-actionable findings, pass-scoped. Earlier passes (1–3) are largely implemented or superseded (see `spec/18_WORK_LOG.md` for the shipped history); the open backlog is concentrated in passes 4 and 5.

> **2026-06-30 — most of Pass 5 shipped.** The PASS5 implementation cycle (`fix/pass5-backlog`, see `spec/18_WORK_LOG.md`) landed **32 of the 36** Pass-5 findings (now including `P5-HUB-01`) plus `P4-SEC-05` and `P4-QUAL-07` (partial). **Still open:** `P5-SYNC-01` (transport-cursor redesign — deferred with design in `spec/07`, latent), `P5-CLI-01` (the `render` seam landed and is wired into `materialize`; full rollout to every leaf command remains), `P5-ARCH-01` (convergence property tests shipped; the formal pure `Decide` extraction remains), and `P4-QUAL-07`'s `contextcheck` (deferred — needs threading a context through the forge chain). `P5-HUB-01` shipped today (branch `fix/p5-hub-01`): the `aws-sdk-go-v2` S3 adapter is wired behind `hub: r2://<bucket>` (`hubFromOptions`), with `DEVSTRAP_HUB_S3_*` env/config credentials, `aws.NopRetryer{}` single-retry, and an env-gated MinIO conformance test (`TestR2MinIOConformance`) plus hermetic `mapS3Error`/conformance unit tests. The PASS4 carried-forward XL items (`SEC-07` envelope encryption **foundation shipped** 2026-06-30, `GIT-03` OS sandbox, `SEC-02` **shipped** 2026-06-30, `SEC-04`, `SYNC-02`/`HUB-11` compaction) — `SEC-07` full workspace-ID pairing and `SEC-08` remain open.

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
| P4-SEC-02 | P1 | Encrypt namespace-map events at rest on R2 (stop leaking paths/remotes/device timelines) — **shipped 2026-06-30** (`fix/p4-sec-02-envelope-encryption`) |
| P4-SEC-04 | P1 | Close the bootstrap window: fail-closed enrollment verification |
| P4-SEC-05 | P1 | Sign release binaries (cosign/SLSA/SBOM); pin `goreleaser-action` to a commit SHA |
| P4-SEC-07 | P2 | Envelope encryption (KEK/DEK) + key rotation + forward secrecy — **foundation shipped 2026-06-30** (`fix/p4-sec-02-envelope-encryption`: WCK epoch keyring + age-wrapped grants + Rotate on revoke; full workspace-ID pairing across devices remains) |
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
