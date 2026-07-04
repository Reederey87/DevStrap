# DevStrap — Design & Implementation Audit (Sixth Pass)

_Date: 2026-07-01 · Trunk audited: `8c739b8` (`feat(sync): envelope-encrypt the namespace-map event log (P4-SEC-02 + P4-SEC-07 foundation) (#25)`)_

## How this relates to the prior audits

This is the **sixth** design & implementation pass. It does **not** restate the still-open recommendations from PASS4/PASS5; those remain tracked in `docs/audits/README.md` (mapped in **Appendix A**). It concentrates where a fresh pass adds the most value:

1. **Adversarial review of the just-landed PR #25 crypto batch** — `internal/workspacekeys/keyring.go`, `internal/sync/eventcrypt.go`, `internal/sync/encryptedhub.go`, migration `00013`. This is the envelope-encryption foundation (`P4-SEC-02` shipped, `P4-SEC-07` foundation) and it carries the densest cluster of new defects in this pass. The ledger was last reconciled **before** #25 landed, so `P4-SEC-02`/`P4-SEC-07` are treated as **shipped** here even though the ledger still lists them open.
2. **Now-reachable surfaces** — the live R2/S3 hub adapter (PR #24) and `hub gc` are wired for real, so this pass audits them as production code, not type theory.
3. **Dimensions the fast batches under-examined** — eager-materialization git behavior, CLI contract honesty, read/write TOCTOU in the data model, CI/release supply-chain gates, the ignore compiler, headless key custody, and spec-truth drift produced *by* the same PRs that were supposed to end it.

**ID scheme.** Every finding is prefixed `P6-` so its ID is globally unique, per the ledger convention (`P5-PROC-01`). Dimension codes: `SEC` (security/crypto), `SYNC` (sync engine/convergence), `HUB` (cloud hub/scale), `GIT` (git materialization/agents), `CLI` (CLI/UX), `DATA` (data model/SQLite), `QUAL` (quality/testing/CI), `XP` (cross-platform/ignore/scan), `DOC` (specs/docs/process).

## Methodology

Findings were produced by a verification-driven multi-agent workflow against the `8c739b8` worktree: **nine dimension reviewers**, each told exactly which PASS4/PASS5 IDs are *shipped* vs *open* so they hunt **new** issues; then **every candidate finding was independently adversarially verified** by a separate agent that re-opened the cited code, reproduced the failure from source (several by building the binary or running probe tests), and tried to refute it. Verifier corrections are folded into every finding below — severities and evidence reflect the verifier's ruling, not the reviewer's first instinct. External best-practice anchors (six exa-backed research topics) are cited inline and collected at the end.

**Severity:** P1 = correctness/security/data-loss or major; P2 = significant; P3 = minor/polish/DX. **Effort:** S ≈ <½ day, M ≈ 1–3 days, L ≈ ~1 week, XL ≈ multi-week.

## Executive summary

> **Note (2026-07-04):** every P1 risk cluster described below — hub-trust (`P6-SEC-01`/`P6-SYNC-01`), GC-deletes-live-data (`P6-HUB-01`/`P6-DATA-01`), and the git timeout (`P6-GIT-01`) — has since shipped; see the status banner in "Findings at a glance." This summary is retained as-found at audit time.

PR #25's envelope-encryption foundation is a real advance — the namespace map is no longer plaintext on the hub, the WCK epoch keyring reuses the shipped keychain custody, and grants are age-wrapped per recipient. But it is **new code written fast against an explicitly untrusted hub**, and this pass found that the trust boundary is not yet closed end-to-end. The crypto cluster is the headline: the hub can still substitute keys, wedge sync, and — through the pre-approval join flow — silently strand a joining device's data fleet-wide. Separately, the now-live R2 backend exposes two data-loss paths in the shipped GC, and a universal 2-minute git timeout quietly breaks the eager-materialization promise for exactly the large repos it exists for.

Risk clusters in five themes:

1. **The envelope layer trusts the hub where it must not.** `P6-SEC-01` (P1): WCK grants are ingested and written to the keychain during `Pull` with **zero signature verification** — before `ApplyEvents` ever runs — so a malicious hub can inject an attacker-known WCK at a high epoch and read all of the victim's future namespace events, fully defeating the `P4-SEC-02` guarantee. `P6-SYNC-01` (P1): any signature/trust failure in `ApplyEvents` aborts the whole batch and freezes the pull cursor, so a revoked-but-still-pushing device permanently wedges every remaining device's sync. `P6-SEC-02`/`P6-SYNC-03`/`P6-SEC-03`/`P6-SYNC-02`/`P6-SYNC-04` are the join-flow data-loss, revoke-reopens-window, epoch-truncation wedge, skip-past-recoverable-event chain-pin, and unauthenticated-carrier-field gaps that ride the same new code.

2. **The now-live hub deletes live data.** `P6-HUB-01` (P1): `devstrap hub gc` sweeps against the **local** replica with no pre-GC sync, no grace window, and an encryption-truncated mark set, so it deletes other devices' live draft blobs. `P6-DATA-01` (P1): the device that runs `draft snapshot create` never records its own `draft_snapshots` row, so routine sync GC and `hub gc` delete the only copy of its own draft blob.

3. **Eager materialization silently under-delivers.** `P6-GIT-01` (P1): a universal 2-minute git timeout, classified as retryable, makes any repo whose blobless clone exceeds 2:00 permanently un-materializable (three wipe-and-retry re-downloads, then "failed"). `P6-GIT-04` (P2): the stored `lfs-policy=always` is ignored on the materialize path, so opted-in LFS repos materialize as pointer files with no warning. `P6-GIT-03` (P2): dependency-rebuild lifecycle scripts run *after* the project's `.env` is decrypted into the same directory (with `$HOME` pointed at it).

4. **The CLI and data model quietly disagree with themselves.** `run-loop` never runs its advertised scan stage (`P6-XP-03`); `scan --adopt` accepts any directory and poisons the fleet namespace (`P6-CLI-02`); re-`init` forks DB-vs-config root (`P6-CLI-01`); event emission and state mutation are dual-written in separate transactions (`P6-DATA-03`); `env rotate <path>` always errors on a non-existent column (`P6-DATA-02`); `db backup` silently omits the local-only secret blobs and keys it would need to restore (`P6-DATA-04`).

5. **The gates and specs meant to catch drift don't.** The spec-drift mapped-spec check is vacuously satisfied by the mandatory work-log entry (`P6-QUAL-01`); the release workflow ships binaries from any tag with zero verification (`P6-QUAL-02`); the only real-backend hub test never runs in CI (`P6-QUAL-03`); and spec/00, spec/13, the audit ledger, and the new `workspacekeys` package all drifted the same week they were supposedly fixed (`P6-DOC-01..04`).

The near-term imperative: **close the grant/verification trust boundary before the R2 hub carries real fleets** (`P6-SEC-01`, `P6-SYNC-01`, `P6-SEC-02`), **make `hub gc` and draft-snapshot recording safe against concurrent/unseen writers** (`P6-HUB-01`, `P6-DATA-01`), and **split the git timeout by command class** (`P6-GIT-01`). Then fix the CLI/data self-contradictions and re-wire the drift gates so the specs stop rotting under each batch.

## Findings at a glance

| Dimension | P1 | P2 | P3 | Total |
|---|---|---|---|---|
| Security & Cryptography | 1 | 2 | 0 | 3 |
| Sync Engine & Convergence | 1 | 3 | 0 | 4 |
| Cloud Hub, Backend & Scale | 1 | 1 | 2 | 4 |
| Git Materialization & Agents | 1 | 4 | 1 | 6 |
| CLI, UX & Developer Experience | 0 | 2 | 3 | 5 |
| Data Model & SQLite | 1 | 3 | 2 | 6 |
| Code Quality, Testing & CI | 0 | 3 | 2 | 5 |
| Cross-Platform, Ignore & Scan | 0 | 5 | 1 | 6 |
| Specs, Docs & Process | 0 | 2 | 2 | 4 |
| **Total** | **5** | **25** | **13** | **43** |

> **Status as of 2026-07-04:** 37 of 43 findings have shipped (all five P1s included). Open: `P6-DOC-01` (test-hardening residual), `P6-CLI-03`, `P6-CLI-04`, `P6-GIT-06`, `P6-HUB-03`, `P6-XP-06`. The live status ledger is `docs/audits/README.md`; the counts and prose below are as-found at audit time (2026-07-01) and are retained as history.

## Prioritized roadmap

| # | Sev | ID | Recommendation | Dim | Effort |
|---|---|---|---|---|---|
| 1 | P1 | P6-SEC-01 | Verify grant carrier signatures + refuse WCK overwrite before writing to the keychain | Sec | M |
| 2 | P1 | P6-SYNC-01 | Quarantine verification/trust failures per-event instead of aborting the whole batch | Sync | M |
| 3 | P1 | P6-HUB-01 | Make `hub gc` sync-first, grace-windowed, and refuse to sweep on a truncated mark set | Hub | M |
| 4 | P1 | P6-GIT-01 | Split the git timeout by command class; stop retrying self-imposed deadline kills | Git | M |
| 5 | P1 | P6-DATA-01 | Record the origin's own `draft_snapshots` row at create time (one txn with the event) | Data | S |
| 6 | P2 | P6-SEC-02 | Founder/join split: joiners don't self-bootstrap epoch 1; key WCKs by (epoch,kid) | Sec | M |
| 7 | P2 | P6-SEC-03 | Separate transient- from permanently-missing epoch; don't truncate-forever | Sec | L |
| 8 | P2 | P6-SYNC-02 | Split skip classes by recoverability; quarantine + surface skipped events | Sync | M |
| 9 | P2 | P6-SYNC-03 | Make the fail-closed window sticky once enrollment has ever happened | Sync | S |
| 10 | P2 | P6-SYNC-04 | Bind the full enc.v1 carrier tuple into the AEAD AAD (enc.v2) | Sync | S |
| 11 | P2 | P6-HUB-02 | Implement the promised keychain/`op://`/age-blob S3 credential resolution | Hub | M |
| 12 | P2 | P6-GIT-02 | Diff `agent` runs against the recorded base SHA, not just the working tree | Git | S |
| 13 | P2 | P6-GIT-03 | Run dependency rebuild before `.env` hydrate; capture a 0600 log | Git | S |
| 14 | P2 | P6-GIT-04 | Honor the stored `lfs_policy` on materialize/hydrate (mirror the worktree path) | Git | M |
| 15 | P2 | P6-GIT-05 | Clean up the worktree+branch on any failure after `git worktree add` | Git | S |
| 16 | P2 | P6-CLI-01 | Detect a root change on re-`init`; rewrite config.yaml or refuse | CLI | S |
| 17 | P2 | P6-CLI-02 | Gate `scan --adopt` on the scanned root matching the workspace root | CLI | S |
| 18 | P2 | P6-DATA-02 | Fix `ClearRotationForProject` to join through `namespace_entries` | Data | S |
| 19 | P2 | P6-DATA-03 | Emit event + state mutation in one transaction at every emission site | Data | M |
| 20 | P2 | P6-DATA-04 | Ship `db backup --full` (blobs + keys) and a restore path/runbook | Data | M |
| 21 | P2 | P6-QUAL-01 | Exclude catch-all specs (`**`) from the mapped-spec drift check | Quality | S |
| 22 | P2 | P6-QUAL-02 | Add a `verify` job gating release on tests + vuln + main-ancestry | Quality | S |
| 23 | P2 | P6-QUAL-03 | Run the MinIO hub conformance test in a CI job | Quality | S |
| 24 | P2 | P6-XP-01 | Delete `ShouldPruneDir`'s bare-name fallback; make relSlash authoritative | XP | S |
| 25 | P2 | P6-XP-02 | Align the ignore compiler with real gitignore semantics | XP | M |
| 26 | P2 | P6-XP-03 | Implement `run-loop`'s advertised scan stage (or fix the docs) | XP | M |
| 27 | P2 | P6-XP-04 | Never mint a WCK when one is published; type-check keychain errors | XP | M |
| 28 | P2 | P6-XP-05 | Keep `scan` offline; defer `set-head --auto` to materialization | XP | M |
| 29 | P2 | P6-DOC-01 | Fix spec/13's stale status block; document `env rotate`; path-anchor the command gate | Doc | S |
| 30 | P2 | P6-DOC-02 | Reconcile the audit ledger's P4-SEC-05 contradiction + convention-#3 violation | Doc | S |
| 31 | P3 | P6-HUB-03 | Fan-out `R2Hub.Push` in HLC-ordered waves | Hub | S |
| 32 | P3 | P6-HUB-04 | Give the retention floor a signed hub-side marker object | Hub | M |
| 33 | P3 | P6-GIT-06 | Gate `agent pr` on run status; reconcile crash-stuck `running` rows | Git | S |
| 34 | P3 | P6-CLI-03 | Wire Cobra usage errors to `exitUsage=10`; fix the spec table | CLI | S |
| 35 | P3 | P6-CLI-04 | Make `--quiet` actually suppress progress chatter (or fix its help) | CLI | S |
| 36 | P3 | P6-CLI-05 | Document the shipped `r2://` hub path; stop steering users to the file hub | CLI | S |
| 37 | P3 | P6-DATA-05 | Add `idx_events_device_hlc` to serve the hot push/doctor query | Data | S |
| 38 | P3 | P6-DATA-06 | Add a single-`local`-device partial unique index + race-tolerant `EnsureDevice` | Data | S |
| 39 | P3 | P6-QUAL-04 | Stub `ssh` via a PATH shim so the alias tests are hermetic | Quality | S |
| 40 | P3 | P6-QUAL-05 | Scope CI push triggers to main + add `concurrency` cancellation | Quality | S |
| 41 | P3 | P6-XP-06 | Compile the scan prune matcher from the root `.devstrapignore` | XP | S |
| 42 | P3 | P6-DOC-03 | Fix spec/00's re-drift (planned-sync comment, command/test inventories) | Doc | S |
| 43 | P3 | P6-DOC-04 | Add `internal/workspacekeys/**` to the spec/07/09/15 `tracks_code` frontmatter | Doc | S |

## Quick wins

- **`P6-DATA-01`** — wire the caller-less `store.RecordDraftSnapshot` into `draft.go`'s create path (one txn with the event). Closes a P1 data-loss path in ~10 lines.
- **`P6-DATA-02`** — one-line SQL fix: join `ClearRotationForProject`'s subquery through `namespace_entries.env_profile_id` instead of the non-existent `env_profiles.namespace_id`.
- **`P6-SYNC-03`** — change `hasEnrolledDevices` to count `trust_state IN ('approved','revoked','lost')` so revocation stays fail-closed.
- **`P6-QUAL-01`** — skip `**`-matched specs in the drift gate's mapped set; add the `[internal/cli/root.go, spec/18]→finding` regression test.
- **`P6-XP-01`** — delete the `m.Match(name, ...)` bare-name fallback in `ShouldPruneDir`; make `relSlash` authoritative.
- **`P6-CLI-02`** — refuse `--adopt` when the scanned root ≠ workspace root.
- **`P6-GIT-05`** — register a `WorktreeRemove`+`branch -D` cleanup and invoke it on the LFS/CurrentDevice/InsertWorktree error returns.

## Strategic bets

- **Close the hub trust boundary before real fleets (`P6-SEC-01` + `P6-SYNC-01` + `P6-SEC-02`).** The envelope layer's whole premise is a zero-knowledge hub, but grant ingestion, verification-failure handling, and the join flow all still trust the hub in ways that break confidentiality, availability, or durability. Fix them as one workstream: a verifier seam on `EncryptedHub.Pull`, per-event quarantine in `ApplyEvents`, a founder/join split, and (epoch,kid)-addressed WCKs. This also finishes the open `P4-SEC-04`/`P4-SEC-07` items.
- **Make the transport plane bound-and-verifiable (`P6-HUB-01` + `P6-DATA-01` + `P6-SEC-03` + `P6-HUB-04`).** GC deletes live data, the origin never records its own draft, epoch-truncation wedges late joiners, and the retention floor has no hub-side representation. The common cause is that the hub log has no authoritative, discoverable manifest and GC trusts a possibly-stale local replica. Adopt the object-storage WAL pattern: a CAS-updated signed manifest (retention floor + snapshot heads), grace-windowed GC keyed on `LastModified`, and a pre-GC sync gate.
- **Split the git timeout and honor stored policy (`P6-GIT-01` + `P6-GIT-04`).** The eager-materialize promise is "after sync the tree is really present." A 2-minute universal timeout and an ignored LFS policy both silently violate it for the exact repos that matter. Per-command-class timeouts + materialize-path LFS handling are small, high-leverage fixes.
- **Re-instrument truth (`P6-QUAL-01` + `P6-DOC-*`).** The spec-drift gate fires on every commit and enforces nothing beyond touching the work log; the specs and ledger drifted the same week they were "fixed." Generate the command/migration/test inventories from the binary and diff them in CI; exclude `**` specs from mapped satisfaction; add new packages to a granular spec's `tracks_code`.

---

## Security & Cryptography

PR #25's envelope layer reuses the shipped keychain custody well and age-wraps grants per recipient, but the trust boundary against the explicitly-untrusted hub is not closed: grants are consumed before verification, joiners self-bootstrap a key the fleet can't use, and never-granted epochs wedge sync.

### P6-SEC-01 — Unauthenticated `device.key.granted` ingestion lets an untrusted hub substitute a device's WCK, defeating envelope encryption — SHIPPED 2026-07-02 (PRs #31/#33/#34)

**Severity / Effort / Category:** P1 / M / security · crypto · confidentiality · _new (PR #25)_

**Problem.** `EncryptedHub.Pull`'s first pass calls `Keyring.IngestGrant` for every `device.key.granted` event in the **raw, unverified** hub batch, before `ApplyEvents`/`verifyEventSignature` ever run. `IngestGrant` checks only `grant.Recipient == k.recipient` and that `unwrapWCK` succeeds, then unconditionally `StoreWCK` + `RecordKeyEpoch` + `cacheWCK` — no Ed25519 verification anywhere, and grants are explicitly excluded from `mustVerifyEvent`. Because age encryption to a public X25519 recipient needs no secret, and every device's recipient string rides the hub as plaintext, a malicious/zero-knowledge hub (or MITM/revoked device) can forge `{Epoch: 1<<40, Recipient: victim, WrappedKey: age.Encrypt(attackerWCK, victimRecipient)}`. `IngestGrant` accepts it; `RecordKeyEpoch` persists it (a separate DB write from `ApplyEvents`, not rolled back by any later apply-path rejection); `CurrentKeyEpoch = MAX(epoch)` returns it; and the victim's next `Push` envelope-encrypts **all** new namespace events (paths, remotes) under a key the attacker chose and knows — a full confidentiality break of `P4-SEC-02`. A low-epoch variant overwrites the legitimate WCK so every real `enc.v1` event fails AEAD open and is silently skipped forever (decrypt DoS).

**Evidence.** `internal/sync/encryptedhub.go:126-141` (ingest on raw batch) → `internal/workspacekeys/keyring.go:229-251` (`IngestGrant` — no signature check, unconditional `StoreWCK`); `internal/state/store.go:2776` `CurrentKeyEpoch = MAX(epoch)`; `encryptedhub.go:60-88` (`Push` encrypts under `CurrentEpoch`); `verifyEventSignature` (`store.go:2533`) runs only inside `ApplyEvents`, after `Pull` returns; grants excluded at `events.go:497-500` / `store.go:2614-2621`. The existing `TestIngestGrantRejectsTamperedWrappedKey` only proves a *corrupted* ciphertext is rejected — a well-formed forged grant is accepted.

**Recommendation.** Fail-closed the grant path before any secret is written. (a) Give `EncryptedHub` a `Verify(ctx, ev) error` seam wired in `hubFromOptions` to the store's signature+trust check; in `Pull`'s first pass, skip (don't ingest) any grant whose carrier event fails verification once any device is enrolled. (b) In `IngestGrant`, refuse to change an already-held epoch's key: `if cur, ok := k.cached(grant.Epoch); ok && !bytes.Equal(cur, wck) { return fmt.Errorf("refusing conflicting WCK for held epoch %d", grant.Epoch) }` (check the store/keychain too, not just cache). (c) Bound `CurrentKeyEpoch` to epochs reached via a verified grant chain so a forged high epoch can't silently become the active `Push` epoch. **Note (verifier):** (b) alone closes the overwrite-DoS and the confidentiality break *for existing/held epochs* only; the headline high-epoch confidentiality break requires (a) and/or (c) — a brand-new epoch has nothing to conflict with.

**Actionable steps.**
1. Thread the carrier `Event` (not just the payload) into `IngestGrant`; add a `VerifyGrant`/`Verify` call gated on `hasEnrolledDevices`.
2. Add the held-epoch overwrite refusal in `IngestGrant`.
3. Constrain `CurrentKeyEpoch` to verified epochs.
4. Test: a well-formed forged grant to the victim's own recipient must be rejected and must not change the keystore or `CurrentKeyEpoch`.

**References:** age is "not in the business of authentication" — any age blob can be replaced by a fresh valid one, so decrypted artifacts must be bound to a trusted signature ([Valsorda](https://words.filippo.io/age-authentication/)); Keybase advertises each key generation in a signed, strictly-sequential chain so clients detect suppressed/rolled-back rotations ([Keybase PUK](https://book.keybase.io/docs/teams/puk)).

### P6-SEC-02 — Every `devstrap init` self-bootstraps epoch 1, so the documented join flow silently loses the joiner's pre-approval data fleet-wide — SHIPPED 2026-07-02 (PRs #32/#33)

**Severity / Effort / Category:** P2 / M / security · data-loss · _new (PR #25); sharpens open `P4-SEC-07`_

**Problem.** `init` unconditionally calls `kr.EnsureBootstrap`, which mints a fresh random epoch-1 WCK on any device with no epoch — including a device *joining* an existing workspace. If the user runs `add`/`scan --adopt` and `sync` on machine B **before** A approves it (the natural order — and the order `init`'s own printed next-steps hint steers them into), B's events are pushed as `enc.v1` sealed under B's private epoch-1 WCK, which is **never granted to anyone**. So: (a) A and every future device parse those events as epoch 1, hold a *different* epoch-1 WCK, fail AEAD, and skip them forever — B's projects silently never appear on any other machine; (b) after approval, `IngestGrant`'s unconditional `StoreWCK` overwrites B's own epoch-1 WCK with A's, destroying the last key that could decrypt B's already-pushed hub ciphertext (so even B can't rebuild from the hub); (c) `sync.go` advances the push cursor past those events, and hub objects are immutable, so nothing re-publishes them. The same bare-integer-epoch collision fires when two devices `Rotate` concurrently. **Note (verifier):** B's plaintext survives in its own SQLite — the loss is hub propagation and hub-based recovery, not local data; and the WCK overwrite is aggravating but not necessary (WCK-B is never granted, so peers could never decrypt regardless).

**Evidence.** `internal/cli/init.go:96` `EnsureBootstrap` (unconditional) → `keyring.go:105-129`; `encryptedhub.go:59-88` (`Push` under `CurrentEpoch=1`, no approval gate); `sync.go:77-87` (push cursor advances, no re-push path); `encryptedhub.go:168-177` (held-epoch decrypt failure → skip, comment names it the "P4-SEC-07 pairing" collision); `keyring.go:243` (unconditional `StoreWCK` overwrite); `init.go:126` next-steps hint points at `sync --hub-file`. The shipped e2e (`sync_encrypted.txtar`) only exercises approve-before-B-pushes.

**Recommendation.** Two coordinated fixes. (1) **Founder/join split:** only the workspace-founding device bootstraps epoch 1; a joining device starts with an empty keyring and `EncryptedHub.Push` refuses (keep the existing `ErrMissingWorkspaceKey` when `CurrentEpoch==0`, don't paper over it with `EnsureBootstrap`) until it has ingested a real granted epoch. Gate bootstrap on "am I the first/only device and is the hub log empty," or add `devstrap init --join`. (2) **Key WCKs by identity, not bare epoch:** mint `kid = hex(sha256(wck)[:8])`, carry it in `DeviceKeyGrant` and the `enc.v1` envelope, and key the keyring by `(epoch,kid)` so colliding keys coexist and concurrent `Rotate` can't clobber. This also makes `P6-SEC-01`'s overwrite-refusal safe (a joiner never holds a conflicting self-minted key). **Also reorder Pull-then-Push for keyless devices** so even a single post-approval cycle can't push under the bogus WCK before the grant-ingesting Pull runs.

**Actionable steps.**
1. Split `init` into founder-bootstrap vs `--join` (empty keyring + push refusal).
2. Add `kid` to grants and the `enc.v1` envelope; key the keyring/keystore by `(epoch,kid)`.
3. Testscript: `init B → add project → sync → approve on A → sync both` asserts B's pre-approval project materializes on A.

**References:** Keybase provisions a new device from an *existing* device over a mutually-authenticated OOB channel — the server never introduces a device key ([Keybase key-exchange](https://book.keybase.io/docs/crypto/key-exchange)); KMS ciphertext records which key version protected it so colliding versions never alias ([GCP KMS rotation](https://docs.cloud.google.com/kms/docs/cmek-rotation)).

### P6-SEC-03 — `Pull` truncates permanently on a never-granted epoch, wedging all sync for a validly-approved device — SHIPPED 2026-07-03 (`fix/never-granted-epoch-grace`)

**Severity / Effort / Category:** P2 / L / security · availability · _new (PR #25)_

**Problem.** In `Pull`'s second pass, `wck, ok := h.Keyring.WCK(env.Epoch); if !ok { return out, nil }` truncates the batch at the first event whose epoch this device doesn't hold, returning only the decryptable prefix; `sync.go` advances the cursor only to the applied prefix, so the next `Pull` re-fetches the same blocking event and truncates again — forever, with no retry bound, skip-forward, or snapshot fallback. `GrantAllEpochs` only wraps epochs the *granting* device holds, and `Rotate` grants only the *new* epoch to approved recipients. So a device B that approves a new device D **after** a rotation minted epoch N but **before** B pulled its own epoch-N grant gives D only B's held epochs; D never receives epoch N and wedges permanently at the first epoch-N event, logged only at Info level. A malicious hub triggers the same wedge deliberately by injecting one `enc.v1` event referencing an epoch nobody will grant, at a low HLC. **Note (verifier):** the "epoch-1 chain" scenario in the reviewer's draft is unreachable — every device bootstraps its own epoch-1 WCK at `init`, so foreign epoch-1 events hit the *skip* branch (`P6-SYNC-02`), not truncate. The real legitimate trigger is a missing intermediate epoch ≥2.

**Evidence.** `internal/sync/encryptedhub.go:159-166` (truncate on missing epoch); `internal/cli/sync.go:96-118` (cursor advances only over applied prefix); `keyring.go:135-170` (`GrantAllEpochs` grants only held epochs); `grantWorkspaceKeyToApprovedDevice` (`devices.go:280-306`) proceeds with warnings, no completeness guard; `TestEncryptedHubMissingEpoch` codifies truncation as intended.

**Recommendation.** Separate "transient missing epoch" from "permanently missing." (1) After a bounded retry/grace window, treat a still-missing epoch as permanently undecryptable and **skip** it (like the held-epoch decrypt-failure branch) rather than truncating, recording a conflict/quarantine so a composite `(HLC,device,id)` cursor can pass it. (2) Make grants transitive on approval — grant every epoch present in the grant log addressed to any approved recipient this device can re-wrap — or add a cheap guard in `grantWorkspaceKeyToApprovedDevice`/`devices approve` that warns/refuses when the approver's held-epoch set isn't contiguous `1..CurrentEpoch`. (3) Ship the `P4-SYNC-02`/`P4-HUB-11` snapshot exchange so a late joiner starts from a snapshot at its lowest held epoch. **Recovery today (undocumented):** re-running `devices approve <D>` from a device holding all epochs re-grants every held epoch and clears the wedge — surface this and the "awaiting workspace key grant" condition in `doctor`/sync output instead of an Info log.

**Actionable steps.**
1. Add a retry/grace bound + skip-with-quarantine for still-missing epochs; move to a composite cursor so skipped events aren't re-stranded.
2. Add a contiguity guard/warning to `devices approve`.
3. Test: a device holding only epoch 2 pulling a batch that starts with an epoch-1 (foreign) then epoch-2 event must not wedge.

---

## Sync Engine & Convergence

The HLC primitives remain correct, but the new envelope decorator and the shipped-but-untested revocation flow reintroduce whole-batch aborts, cursor-advance-past-loss, and unauthenticated-carrier gaps one layer above the fixes PASS4/PASS5 landed.

### P6-SYNC-01 — Any signature/trust failure in `ApplyEvents` aborts the whole batch and permanently wedges the pull cursor — SHIPPED 2026-07-02 (PR #30)

**Severity / Effort / Category:** P1 / M / sync · availability · _new (reachable once revoke+Rotate shipped)_

**Problem.** In `ApplyEvents`, only `errors.Is(err, state.ErrEventHashChain)` gets the record-conflict-and-continue treatment; **every other** `insertEvent` error hits `return 0, err`. Signature/trust failures, content-hash mismatch, and `ErrDivergentEvent` all surface as plain `fmt.Errorf`, so `cli/sync.go` returns before `AdvanceHubCursor` — the cursor never moves and the identical poisoned event is re-pulled and re-fails on every future sync, permanently. This is reachable by **legitimate** revocation traffic: `devices revoke` is local-only (no `device.revoked` event is applied), so a revoked device's `run-loop` keeps pushing signed `project.added` events under an epoch the revoker still holds; on the revoker, decrypt succeeds, then `verifyEventSignature` hits `trustState != "approved"` with `enrolled=true` → error → whole sync aborts, cursor frozen. A stolen/lost device running `run-loop` can permanently DoS every remaining device with one signed event; recovery needs manual DB/hub surgery. **Note (verifier):** the wedge requires `enrolled=true` (a third approved device, or a must-verify event type). In a pure two-device fleet where the revoked device was the only approved one, `enrolled=false` and its non-destructive events are instead *accepted* — a separate gap tracked as `P6-SYNC-03`.

**Evidence.** `internal/sync/events.go:306-323` (only `ErrEventHashChain` continues; else `return 0, err`); `internal/state/store.go:2554/2568/2582/2585` (verification/trust failures are unwrapped `fmt.Errorf`), `:2421` (content-hash), `:2453` (`ErrDivergentEvent`); `cli/sync.go:112-117` returns before `AdvanceHubCursor`; `encryptedhub.go:94-112`'s own design note claims "a single non-conforming object must never wedge sync" — the plaintext apply path violates it.

**Recommendation.** Classify verification/trust failures as per-event quarantine, mirroring the skew path, instead of aborting the batch. (1) Add sentinel errors (`ErrEventVerification`, and reuse `ErrDivergentEvent`) and wrap the relevant returns. (2) In `ApplyEvents`, alongside the `ErrEventHashChain` branch: `if errors.Is(err, state.ErrEventVerification) { insertVerificationConflict(...); continue }`. (3) **Split causes:** revoked/bad-signature/`ErrDivergentEvent` are *permanent* — advance the cursor past them (holding would wedge exactly as today). Unknown/pending-device failures are *possibly transient* — hold the cursor bounded, or re-evaluate quarantined conflicts on trust-state change, so an event from a device approved shortly after isn't permanently skipped once the cursor passes it. (4) Ship a real `device.revoked` apply path so a revoked device's events are rejected by trust, not just by signature-abort.

**Actionable steps.**
1. Introduce `ErrEventVerification`; wrap `verifyEventSignature`/content-hash/`ErrDivergentEvent` returns.
2. Add the per-event quarantine branch; advance the cursor for permanent causes only.
3. Test: batch `[validC1, revokedB1, validC2]` applies C1+C2, records one conflict, advances past all three.

**References:** Replicaché formalizes the "cursor unknown/poisoned" path as an explicit clear-and-rebuild reset rather than a hard stall ([Replicache pull](https://doc.replicache.dev/reference/server-pull)).

### P6-SYNC-02 — Skip-on-decrypt-failure advances the cursor past recoverable events and permanently chain-pins every later event from that device — SHIPPED 2026-07-03 (PR #63)

> **Status/premise note (2026-07-03):** the cursor half of this finding's premise was superseded before the fix landed — the P5-SYNC-01 per-device Seq cursor (PR #59) already made a pull-dropped event HOLD its origin device's cursor at a seq gap instead of being silently passed. What PR #63 closed was the remaining residual: durable, classified records (`sync_skipped_events`) with grace-bounded unknown-version deferral, malformed-envelope quarantine forwarding, `status`/`doctor` surfacing, and a fail-closed `hub gc` refusal. No `--replay-skipped` was built (nothing to rewind under per-device cursors).

**Severity / Effort / Category:** P2 / M / sync · data-loss · _new (PR #25)_

**Problem.** `EncryptedHub.Pull` skips `ParseEncryptedEnvelope` failures — explicitly including `ErrUnknownEnvelopeVersion` ("a newer client may write a version this build cannot read … skip rather than wedge") — and held-epoch decrypt failures; skipped events are dropped before `ApplyEvents`, so the SYNC-01 low-water mark can't hold the cursor below them, and any higher-HLC applied event advances the cursor past them for good (`Pull` filters `HLC >= cursor`). Two of the three skip classes are recoverable, and skipping converts them into permanent loss: (a) a mixed-version fleet — an `enc.v1` device skips a future `enc.v2` device's events, cursor advances, and after upgrade they're never re-delivered → permanent namespace divergence with no resync command; (b) collision-era ciphertext from the join flow (`P6-SEC-02`) stays skipped even after a correct key later exists. Worse, the skip is not contained: the origin device's per-device hash chain now has a hole, so its **next** event hits `ErrEventHashChain` and is held "transiently" forever — the re-pulled batch below the pinned cursor grows monotonically with a warning every sync, the exact soft-brick the decorator claims to prevent. **Note (verifier):** the always-live trigger is this chain-pin from any single corrupted/undecryptable hub object; the mixed-version case is latent until an `enc.v2` exists. Permanent stranding needs an applied event with *strictly higher* HLC (grant events or a third device — the common case); a pure two-device fleet with no other applied events recovers after upgrade.

**Evidence.** `encryptedhub.go:149-157` (skip parse failures incl. unknown version), `:168-177` (skip held-epoch decrypt failures); `cli/sync.go:101-112` (skipped dropped before apply); `events.go:329-340` (low-water mark can't see them); the sealed content carries `PrevEventHash` (`eventcrypt.go:174`), so `validatePrevEventHash` (`store.go:2466-2467`) → `ErrEventHashChain` → `events.go:307-321` pins the cursor forever. Contradicts spec/07:570 ("one bad object can no longer permanently brick a device").

**Recommendation.** Split skip classes by recoverability. (1) Treat `ErrUnknownEnvelopeVersion` like a missing epoch — **truncate** (`return out, nil`), don't skip: wedging until upgrade is correct because the data is decryptable after upgrade (truncate-only, never error, so an old client mid-batch still gets the decryptable prefix). (2) For held-epoch decrypt failures and malformed envelopes, persist a `sync_skipped_events` quarantine row (id, device, HLC, epoch, reason) instead of dropping silently; surface the count in `status`/`doctor`; add `sync --replay-skipped` re-pulling from `min(skipped HLC)` once keys change. (3) When an event is skipped, record that the origin's chain is broken so the successor's `ErrEventHashChain` references the root cause instead of being held transiently forever.

**Actionable steps.**
1. Truncate on unknown version; quarantine (not drop) held-epoch/malformed failures.
2. Add the quarantine table + `status`/`doctor` surfacing + `sync --replay-skipped`.
3. Test: a device holding only epoch 2, batch starting with an unrecoverable object, must not silently strand later same-device events.

### P6-SYNC-03 — Revoking the last approved device reopens the bootstrap window; the revoked device's events are accepted again — SHIPPED 2026-07-03 (PR #38)

**Severity / Effort / Category:** P2 / S / sync · security · _new (inverse of open `P4-SEC-04`)_

**Problem.** `hasEnrolledDevices` counts only `trust_state = 'approved'` rows. After `devices revoke dev_b` in a two-device workspace the count is 0, so `enrolled=false`, and `verifyEventSignature`'s final gate — `!isLocal && trustState != "approved" && (mustVerifyEvent || enrolled)` — evaluates false for non-destructive events, which then fall through to `devicekeys.Verify` with the revoked device's still-valid key and are **accepted and applied**. The primary persona (laptop + desktop) revokes the lost/compromised laptop, and the fail-closed HUB-03 regime silently disengages on the desktop: the laptop can keep injecting `project.added`/`draft.snapshot.created`/`conflict.resolved` events. **Note (verifier):** the impact is broader than the revoked device — once the last approved device is gone, the full pre-enrollment fail-open resumes for non-destructive events, so even **unknown** devices and keyless/unsigned events are accepted again. (Destructive types stay gated, so deletes/renames instead hit the `P6-SYNC-01` abort — both branches wrong.)

**Evidence.** `internal/state/store.go:2593-2608` (`hasEnrolledDevices` counts `approved` only); `:2581` (the final gate); `SetDeviceTrustState` (`:668`) flips only `trust_state`, leaving `signing_public_key` intact; the remote pull path (`internal/sync/events.go:280-305`) has no revoked-device filter. No test covers revoke-then-inject.

**Recommendation.** Make the closed window sticky: enrollment having *ever* happened keeps verification fail-closed. Either (a) `SELECT COUNT(*) FROM devices WHERE trust_state IN ('approved','revoked','lost')` (a revoked/lost row proves enrollment completed — and correctly excludes auto-created `pending` placeholders from `EnsureRemoteDeviceTx`), or (b) persist a monotonic `enrollment_closed` flag set on first approve and OR it into `hasEnrolledDevices`.

**Actionable steps.**
1. Change the `hasEnrolledDevices` predicate (option a) or add the sticky flag (option b).
2. Test: approve B, revoke B, then a signed `project.added` from B must be rejected (and, with `P6-SYNC-01`, recorded as a conflict rather than applied or aborting).

### P6-SYNC-04 — `enc.v1` carrier `DeviceID`/`Seq`/`HLC` are bound by neither the AEAD AAD nor the signature, so the hub can mutate them at the crypto layer — SHIPPED 2026-07-03 (PR #44)

**Severity / Effort / Category:** P2 / S / sync · integrity · _new (PR #25)_

**Problem.** `envelopeAAD` binds only `event.ID || uint64(epoch)`; the sealed tuple holds only `Type/PayloadJSON/ContentHash/PrevEventHash`; and `eventSignaturePayload` covers `ContentHash/HLC/ID/PayloadJSON/PrevEventHash/Type`. So `DeviceID` and `Seq` are authenticated by **nothing** end-to-end, and `HLC` only by the signature (checked after decryption). An untrusted hub can rewrite carrier fields on stored objects: (a) **Seq mutation** passes both AAD and signature — setting `Seq=1` on an event with a non-empty `PrevEventHash` forces `previousEventContentHash` to the wrong predecessor → `ErrEventHashChain` → the cursor is held below that event on every sync forever (a hub-controlled soft-wedge needing no keys); (b) **DeviceID re-attribution** to an unknown/pending device is accepted wholesale in any not-fully-enrolled regime, corrupting the `(HLC, DeviceID, ID)` conflict tiebreak and the per-device chain; (c) `DeviceID`/`HLC` mutation in the enrolled regime lands in the `P6-SYNC-01` hard-abort. All are detectable for free if the AAD covered the full carrier.

**Evidence.** `internal/sync/eventcrypt.go:213-222` (`envelopeAAD` = ID+epoch), `:62-67` (sealed tuple); `internal/state/store.go:2509-2516` (`eventSignaturePayload`); `store.go:2423` vs `2426` (`validatePrevEventHash` runs before `verifyEventSignature`); `store.go:2485-2486` (`Seq==1` predecessor branch); `events.go:623-631` (samePathLess tiebreak).

**Recommendation.** Extend the AAD to the full carrier tuple so any hub-side mutation is an AEAD failure caught in the decorator (which already has a contained skip path) instead of leaking into apply-path semantics: `envelopeAAD = ID || DeviceID || uint64(Seq) || uint64(HLC) || uint64(epoch)`, length-prefixed, derived from the carrier on decrypt. This is backward-incompatible with existing `enc.v1` ciphertext, so **bump to `enc.v2` now** while only the file-hub spike and fresh R2 buckets exist. Also add `DeviceID`/`Seq` to `eventSignaturePayload` under a v2 signature domain (`devstrap:event:v2`) so the binding holds for plaintext-era grants too. **Caveat (verifier):** the decorator's skip path drops undecryptable events *without holding the cursor*, so on AEAD failure the enc.v2 path must **hold the cursor or record a conflict** (per `P6-SYNC-02`) or a hub mutation becomes silent permanent loss. **Reconcile with open `P4-SYNC-05`** (folded hash chain incl. hlc/seq + signed head) so Seq/HLC binding isn't implemented twice with different domains — the AAD approach is the only one that also protects the pre-enrollment window, since it depends on WCK possession, not device trust.

**Actionable steps.**
1. Widen `envelopeAAD` to the full carrier; introduce `enc.v2`.
2. Add `DeviceID`/`Seq` to the signature payload under a v2 domain.
3. On AEAD failure, hold-or-conflict (don't silently skip).
4. Test: mutating `Seq`/`DeviceID`/`HLC` on a stored object yields an AEAD authentication failure, not an apply-path wedge.

---

## Cloud Hub, Backend & Scale

The R2/S3 adapter (PR #24) is now the live sync backend, which turns previously-latent hub findings into reachable ones: GC deletes live data, credential custody diverges from spec, push is serial, and the retention floor has no hub-side representation.

### P6-HUB-01 — `hub gc` sweeps against the stale local replica with no pre-GC sync, no grace window, and an encryption-truncated mark set — it deletes other devices' live draft blobs — SHIPPED 2026-07-02 (PR #36)

**Severity / Effort / Category:** P1 / M / hub · data-loss · _new (defect in shipped `P5-HUB-02`)_

**Problem.** `hubGC` lists every hub blob and deletes any key not in `store.RetainedBlobRefs` — a purely **local** SQLite set — without pulling first. Remote devices' draft snapshots enter that set only when their `draft.snapshot.created` event is applied during sync, so a device that hasn't synced today deletes other devices' live blobs. Worse, `EncryptedHub.Pull` truncates at the first ungranted epoch and skips undecryptable events (`P6-SEC-03`/`P6-SYNC-02`) with no signal to callers, so a device awaiting a grant has a systematically incomplete mark set. And `runSyncCycle` pushes blobs **before** events, so even a fully-synced peer's GC can list a just-uploaded blob whose referencing event isn't on the hub yet — and `pushReferencedBlobs` only covers events above the push cursor, so the origin never re-pushes it. `ListObjectsV2` returns keys only (no `LastModified`), so no age-based grace window is even possible today. Concrete loss: device B `draft snapshot create` + `sync`; device A (stale or awaiting B's grant) runs `hub gc`; B's blob is absent from A's `RetainedBlobRefs` → A deletes it → every device gets "referenced blob(s) missing from hub."

**Evidence.** `internal/cli/hub.go:238-278` (`hubGC`), `:253`/`:263-277` (`RetainedBlobRefs`, local); `internal/sync/events.go:475-491` (remote refs enter only on apply); `encryptedhub.go:99-112`/`159-167` (truncate/skip, no counter); `cli/sync.go:69-74` (blobs pushed before events); `internal/hub/r2.go:66` (`ListObjectsV2` keys only).

**Recommendation.** Make `hub gc` safe against concurrent/unseen writers. (1) Run a full pull+apply inside `hubGC` before computing refs, and **refuse to sweep** if `EncryptedHub` deferred any events (thread a truncated/skipped counter out of `Pull`) or if `ApplyEvents` quarantined anything (thread the safe-cursor/quarantine signal). (2) Add a creation-time grace window: extend the listing to carry `LastModified` (`type ObjectInfo struct { Key string; LastModified time.Time }`; the S3 adapter gets it free from `out.Contents[i].LastModified`; memS3/FileHub record a timestamp), then skip blobs younger than e.g. 24h. (3) Document that `gc` runs on one device. **Note (verifier):** the grace window matches the already-recommended open `P4-HUB-12`; the *new* parts are that shipped `P5-HUB-02` omitted it, that the mark set is a possibly-stale single-device replica (not fixable by any grace TTL — needs the pre-GC pull), and that PR-#25 truncation makes the mark set systematically incomplete.

**Actionable steps.**
1. Pull+apply before marking; refuse to sweep on any deferred/skipped/quarantined signal.
2. Add `LastModified` to the list interface + a 24h grace skip.
3. Test: B creates+syncs a draft; A (unsynced) `hub gc` must not delete B's blob.

**References:** GC on an object-store log must handle the mark/write race with an age-based grace window and delete only below a manifest watermark ([OSWALD](https://nvartolomei.com/oswald/)); coordinate exclusive maintenance via `PUT If-None-Match` lock objects so two devices don't sweep concurrently ([Morling/S3 leader election](https://www.morling.dev/blog/leader-election-with-s3-conditional-writes/)).

### P6-HUB-02 — Hub S3 credential custody contradicts spec/19: only plaintext env/config works; the promised keychain/`op://`/age-blob resolution does not exist — SHIPPED 2026-07-03 (`fix/p6-hub-02`)

**Severity / Effort / Category:** P2 / M / hub · security · spec-drift · _new (defect in shipped `P5-HUB-01`)_

**Problem.** `selectBackendHub` reads the secret literally from viper (`hub_s3_secret_access_key`) or `AWS_SECRET_ACCESS_KEY` and passes it straight to `NewS3Client`. spec/19 states the secret access key "goes through DevStrap's existing encrypted secrets path only: OS keychain / age-encrypted blob / 1Password `op://` … Never plaintext config," and even annotates that custody as "shipped" — but no such resolution exists in the hub path. A user who follows the spec and sets `DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=op://vault/item/key` gets the literal string signed as the AWS secret → an opaque `SignatureDoesNotMatch` (mapS3Error has no auth-specific mapping). The only configs that work — plaintext env or a plaintext config line — are the ones spec/19 forbids. **Note (verifier):** spec/15:138 and spec/13:182 (both updated in the same P5-HUB-01 PR) correctly document plaintext-env custody, so the project's canonical threat model *sanctions* it; the "never plaintext config" invariant lives only in spec/19. So this is primarily spec/19 drift (including a false "shipped" annotation) **plus** a missing promised feature, not a threat-model violation — and the three specs contradict each other.

**Evidence.** `internal/cli/hub.go:106-110` (literal read → `NewS3Client`); `internal/hub/s3client_awssdk.go:60-67` (static credential provider); spec/19:127-131,150,155-158; spec/15:138 and spec/13:182 (contradicting, correct).

**Recommendation.** Implement the promised resolution in `selectBackendHub`: (a) `op://` → resolve via the existing 1Password machinery (`op read`, mirroring `env.go`'s provider path); (b) otherwise try the OS keychain via the shipped `devicekeys.NewHybridStore` under a dedicated entry, with a `devstrap hub login` (or `env bind`-style) command to store it once; (c) keep the plaintext env fallback behind `DEVSTRAP_NO_KEYCHAIN` for CI. Wrap the resolved value in `redact.Secret`. Either way, **reconcile spec/19 with spec/13/spec/15** (they currently disagree) and remove the false "shipped" annotation.

**Actionable steps.**
1. Add `op://`/keychain resolution to `selectBackendHub`; `redact.Secret` the value.
2. Add an auth-error branch to `mapS3Error` with an actionable hint.
3. Reconcile spec/19 ↔ spec/13/15; drop the false "shipped" tag until the feature lands.

**References:** age blobs are sender-unauthenticated, so credential secrets must ride the app's real secrets path, not object metadata ([Valsorda](https://words.filippo.io/age-authentication/)).

### P6-HUB-03 — `R2Hub.Push` uploads one event per serial round-trip; the O(events) defect `P5-HUB-04` fixed on the pull side is still present on push

**Severity / Effort / Category:** P3 / S / hub · performance · _new (asymmetry with shipped `P5-HUB-04`)_

**Problem.** `Push` loops one marshal + conditional-PUT per event with no `errgroup`, while `Pull` got bounded fan-out (`r2PullConcurrency=8`) under `P5-HUB-04`; `pushReferencedBlobs` is likewise serial. First sync after `scan --adopt` of a large `~/Code` (300 projects → hundreds of `project.added`/draft events) issues hundreds of sequential PUTs; at 50-100 ms RTT that's 30-60+ s of pure serial latency before materialization starts, and throttle backoff stacks serially. `run-loop` ticks inherit the cost after bulk local changes.

**Evidence.** `internal/hub/r2.go:120-146` (serial Push), `:182-203` (Pull errgroup, `r2.go:42`); `internal/cli/sync.go:151-166` (serial `pushReferencedBlobs`); `runSyncCycle` (`sync.go:69-87`) advances the push cursor only on Push success; 412s are dedup no-ops.

**Recommendation.** Mirror the pull-side pattern with a limited `errgroup`, **but push in HLC-ordered waves** (complete all PUTs at `HLC <= h` before starting `HLC > h`). **Note (verifier):** unordered concurrent PUTs can make a higher-HLC event visible before a lower-HLC one from the same device; a peer pulling in that window advances its HLC cursor past the not-yet-uploaded event and never re-pulls it — widening the open `P5-SYNC-01` transport-vs-logical-clock gap from cross-device to intra-device. So either wave-order the fan-out or sequence this after `P5-SYNC-01`'s ingestion-position cursor.

**Actionable steps.**
1. `errgroup` with `SetLimit(r2PushConcurrency)` over PUTs, ordered by HLC wave.
2. Fan-out `pushReferencedBlobs` similarly.
3. Document the wave-ordering invariant in the Push comment.

**References:** every mature object-store log fans out and compacts small per-record objects rather than serial round-trips ([WarpStream](https://docs.warpstream.com/warpstream/overview/architecture)).

### P6-HUB-04 — The retention horizon has no hub-side representation, so the shipped `ErrSnapshotRequired` guard can never fire against a real hub — SHIPPED 2026-07-04 (PR #75)

**Severity / Effort / Category:** P3 / M / hub · correctness · _new (unshipped half of `P5-HUB-03`)_

**Problem.** Production wiring always builds `R2Hub{RetentionHLC: 0}` (never set; same for `FileHub`), and `R2Hub.Pull` gates only on that local field. Nothing reads a retention marker from the hub; `RetentionHLC` is non-zero only in tests. So `P5-HUB-03`'s divergence guard is dead config in every production path — the moment event-log compaction lands (`P4-SYNC-02`/`P4-HUB-11`) or an operator manually prunes the `events/` prefix, the compacting device knows the floor but every **other** device still constructs `RetentionHLC=0`, pulls a silently partial log, applies it, advances its cursor, and permanently diverges: exactly what `ErrSnapshotRequired` exists to prevent. **Note (verifier):** `P5-HUB-03`'s own recommendation already prescribed a hub-side `_floor` object, so the genuinely new parts are (1) the shipped fix implemented only the client-side field while the ledger closed the item, and (2) the signed-marker requirement below.

**Evidence.** `internal/cli/hub.go:116` (`R2Hub{S3, WorkspaceID}` — no `RetentionHLC`), `:63,72` (FileHub bare); `internal/hub/r2.go:152-154` (checks only the local field); deferral comments at `internal/cli/blob_gc.go:282` and `internal/state/store.go:2034`.

**Recommendation.** Define the wire format now so compaction can't ship without it: a per-workspace `workspaces/<ws>/meta/retention.json` `{retention_hlc, compacted_at, device_id}` **signed by an approved device's Ed25519 key**. `R2Hub.Pull` fetches it first (404 → floor 0, cache per process) and compares before listing. Give `FileHub` the same file-based marker so the conformance suite covers it; add a memS3 case "pull below a written retention marker → `ErrSnapshotRequired`." Verify the signature so a malicious hub can only DoS (force a snapshot), not silently truncate.

**Actionable steps.**
1. Add the signed `retention.json` marker + a hub-side read on Pull.
2. Wire it into FileHub/memS3 conformance.
3. Fold the signed-marker requirement into the `P4-HUB-11` compaction work.

**References:** maintain a single CAS-updated manifest recording the GC watermark so a lagging reader is told to snapshot rather than diverge ([OSWALD](https://nvartolomei.com/oswald/)); R2 supports `If-Match` CAS as an S3 extension for exactly this manifest update ([Cloudflare R2 extensions](https://developers.cloudflare.com/r2/api/s3/extensions/)).

---

## Git Materialization & Agents

The eager-materialize and agent paths carry a universal timeout that breaks the headline promise, ignore the stored LFS policy, expose the project's decrypted `.env` to lifecycle scripts, and leak worktrees on the error paths.

### P6-GIT-01 — A universal 2-minute git timeout makes materialization of any large repo permanently impossible, and the retry loop re-downloads three times — SHIPPED 2026-07-02 (`fix/p6-git-01`)

**Severity / Effort / Category:** P1 / M / git · reliability · _new (post-`GIT-02` interaction)_

**Problem.** `NewRunner()` sets `Timeout: 2 * time.Minute` applied to every command including clone when the ctx has no deadline (the CLI passes none). A `DeadlineExceeded` is classified retryable (`ErrNetwork`, "timed out after 2m0s"), and `CloneWithOptions` retries `ErrNetwork` up to `RetryAttempts=3`, wiping the destination before each retry (`os.RemoveAll` + `os.MkdirAll`). So any repo whose blobless clone takes > 2:00 (multi-GB-history monorepo, slow link, LFS-heavy checkout) can **never** materialize: SIGKILL at 2:00 → classified `ErrNetwork` → partial staging dir deleted → identical 2-minute attempt twice more → ~6 min and 3× bandwidth → project marked "failed." This silently breaks "`devstrap sync` eagerly reconstructs the whole `~/Code` tree" for exactly the repos eager materialization exists for; `worktree new` LFS pulls hit the same ceiling. **Note (verifier):** the wiped destination is the sibling `MkdirTemp` staging dir, not the final path — harm is wasted bandwidth/time, not data loss. `LFSPull` is called without the retry wrapper, so it hits the ceiling once, not 3×.

**Evidence.** `internal/git/git.go:40` (`Timeout: 2*time.Minute`), `:80-84` (applied when no ctx deadline), `:115-117` (DeadlineExceeded → `ErrNetwork`), `:163-170,176` (retry wipes dest), `:433-436` (`LFSPull` under the same cap); all `internal/cli` callers use bare `dsgit.NewRunner()`; `hydrate.go:133-138` records "failed."

**Recommendation.** Split the timeout into command classes and stop retrying self-imposed deadline kills. Add `CloneTimeout time.Duration` (default 30m, config `materialization.clone_timeout`) to `Runner`; in `CloneWithOptions`/`LFSPull` derive `ctx, cancel := context.WithTimeout(ctx, r.CloneTimeout)` before `Run` (Run already skips its 2m default when the ctx has a deadline). In the retry loops, treat the runner's own deadline as terminal — but since `ctx.Err()` in the loop refers to the still-live parent ctx, have `Run` return a **distinct sentinel** (e.g. `ErrTimeout`) for its self-imposed deadline instead of `ErrNetwork`, and `return err` on it rather than retrying. Cover `Fetch`/`runWithNetworkRetry` too (a large new-history fetch has the same pattern).

**Actionable steps.**
1. Add `CloneTimeout`; derive per-clone/per-LFS/per-fetch deadlines.
2. Return `ErrTimeout` (terminal) for self-imposed deadlines; stop the wipe-and-retry.
3. Tests: a clone that sleeps past a tiny `CloneTimeout` → exactly one attempt + a "timed out" (not "network") error; a 3-minute-simulated clone succeeds under the new default.

**References:** blobless partial clone is the right long-lived default, but users going offline should pre-fetch (`git backfill`) rather than hit on-demand fetch storms — a longer, class-specific timeout is the precondition ([GitHub clone study](https://github.blog/open-source/git/git-clone-a-data-driven-study-on-cloning-behaviors/), [git backfill](https://git-scm.com/docs/git-backfill)).

### P6-GIT-02 — `agentDiffSummary` only sees uncommitted changes, so any agent that commits its work records "(no changes)" and a diff-less PR body — SHIPPED 2026-07-03 (`fix/p6-git-02`)

**Severity / Effort / Category:** P2 / S / git · agents · _new_

**Problem.** `agentDiffSummary` runs `git status --short` and revision-less `git diff --stat` — working-tree/index state only — called once after the agent command exits. The canonical agent workflow is: agent **commits** its work, then `agent pr` pushes the branch (pushing is meaningless without commits). For exactly that flow both commands are empty after the commit, so `DiffSummary` is stored empty, `agent show` reports "(no changes)" for a run that changed dozens of files, and `agentPRBody` silently omits the diff section (`if run.DiffSummary != ""`). spec/10's acceptance criterion "diff summary is available" and PR-flow step 2 are unmet for committing agents. `BaseSHA` is recorded right there but never diffed against.

**Evidence.** `internal/cli/agent.go:479-480` (revision-less status+diff), `:105` (called once), `:181` (`agent show`), `:504` (PR body gate), `:97` (`BaseSHA` recorded, unused for diff).

**Recommendation.** Diff against the recorded base: `committed, _ := r.Run(ctx, wt.Path, "diff", "--stat", wt.BaseSHA+"..HEAD")` plus the existing `status --short` for uncommitted residue; join with labeled sections. Guard the unborn-HEAD case by falling back when `rev-parse --verify HEAD` fails (nearly moot — worktrees are always created from a fetched base — but cheap defense).

**Actionable steps.**
1. Change the signature to take the worktree; compute base-vs-HEAD + uncommitted, labeled.
2. Test: agent command runs `git commit -am x` in the worktree; assert the summary contains the committed file stat.

### P6-GIT-03 — Dependency rebuild runs untrusted postinstall scripts *after* the decrypted `.env` is hydrated into the project, with `$HOME` pointing at it — SHIPPED 2026-07-03 (PR #69)

**Severity / Effort / Category:** P2 / S / git · security · spec-drift · _new (post-`P5-SEC-03`)_

**Problem.** `materializeGitRepo` calls `hydrateProjectEnv` first (decrypting the bound profile to cleartext `.env` in `localPath`), then — gated on the single global env var `DEVSTRAP_REBUILD_DEPS` — runs `npm ci`/`pnpm install`/etc. in that same directory via `runRebuildCommand`, which sets `"HOME": dir` and discards output. So at rebuild time the project's freshly decrypted secrets sit at `$HOME/.env` for the lifecycle scripts. `P5-SEC-03` sanitized the rebuild's *environment* so a malicious postinstall "cannot read the real ~/.ssh/.aws/.npmrc" — but the ordering hands it something better: the project's own production credentials one `cat $HOME/.env` away, output discarded (no forensic trail). The gate is one global env var, not the per-project `rebuild_on_hydrate: ask|always|never` spec/08 requires, and no 0600 log exists (spec/08 requires one). **Note (verifier):** the reorder is defense-in-depth, not a hard barrier (with no OS sandbox a script can resolve the real home via `getpwuid`/`dscl` and read other projects' `.env` by absolute path); the 0600 log is the other load-bearing part. spec/08's rebuild section is headed "planned," so the ask-gate/log gap is partly planned-vs-shipped drift.

**Evidence.** `internal/cli/materialize.go:198` (`hydrateProjectEnv` first), `:282,288,292` (writes cleartext `.env`), `:205-208` (rebuild gated on env var), `:361-362` (output discarded), `:371` (`HOME: dir`); spec/08:105 (ask-gate), spec/08:108 (0600 log).

**Recommendation.** (1) Swap the two calls so `rebuildDependencies` runs **before** `hydrateProjectEnv`. (2) Capture rebuild stdout/stderr to a 0600 log under `~/.devstrap/logs/rebuilds/<project>.log` per spec/08:108. (3) Either implement the per-project `materialization.rebuild_on_hydrate` policy or update spec/08:105 to document the env-var gate honestly. (Treat "skip rebuild when a `.env` already exists" as optional — it would permanently disable rebuild for projects with bound env profiles on re-materialize.)

**Actionable steps.**
1. Reorder rebuild-before-hydrate in `materializeGitRepo`.
2. Add the 0600 rebuild log.
3. Reconcile the spec/08 ask-gate/log claims with the code.
4. Test: `.env` does not exist at rebuild time in the materialize flow.

### P6-GIT-04 — Eager materialize/hydrate ignore the stored `lfs_policy`; an `lfs-policy=always` repo materializes as pointer files with zero warning — SHIPPED 2026-07-04 (`fix/p6-git-04`)

**Severity / Effort / Category:** P2 / M / git · fidelity · _new (materialize-path gap)_

**Problem.** `materializeGitRepo` and `hydrateProjectUnlocked` never call `dsgit.UsesLFS`/`LFSPull` and never read `project.LFSPolicy`; the only consumer is `applyWorktreeLFSPolicy` on the *worktree* path. Worse, `gitEnv` forces `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`, so the user's global `git lfs install` smudge filter is invisible to every devstrap-driven clone/checkout — pointer files result regardless of user config, and since they match the index, `DirtyState` reports clean and the project is recorded available/clean with no warning. A user who ran `clone --lfs-policy always` gets a tree of 3-line pointer files, breaking "after sync the tree is really present." **Note (verifier):** `add.go`'s `--lfs-policy` help scopes it to "agent worktrees," so "declared mandatory" is slightly overstated for `devstrap add`; `clone.go` has no such scoping, and the silent pointer-file/available-clean recording is a gap under any policy.

**Evidence.** `internal/cli/materialize.go:182-211`, `internal/cli/hydrate.go:93-190` (never read `LFSPolicy`); `internal/cli/worktree.go:217-240` (only consumer); `internal/git/git.go:704-712` (`GIT_CONFIG_GLOBAL=/dev/null`); spec/08:271 (policy read only during `worktree new`).

**Recommendation.** Mirror `applyWorktreeLFSPolicy` on the primary materialization path: after `hydrateProjectUnlocked`, `if used, _ := dsgit.UsesLFS(ctx, localPath); used { switch policy { case "always": lfs install --local; LFSPull (fail the project on error); default: Warn("LFS pointer files remain", …) } }`. The `install --local` is required because `GIT_CONFIG_GLOBAL=/dev/null` hides the global filter. Give `LFSPull` the `P6-GIT-01` large-operation timeout. Update spec/08's LFS section.

**Actionable steps.**
1. Add materialize/hydrate-path LFS handling (`install --local` + `LFSPull` for `always`, warn otherwise).
2. Record available/clean only after the LFS decision.
3. Testscript: a fake-LFS repo with `always` pulls; with `auto` warns.

### P6-GIT-05 — `createFreshWorktree` leaks a live, DB-invisible worktree + branch when LFS pull or the DB insert fails after `git worktree add` — SHIPPED 2026-07-03 (`fix/p6-git-05`)

**Severity / Effort / Category:** P2 / S / git · resource-leak · _new (normal-error-path, distinct from `P4-GIT-04`)_

**Problem.** `addWorktreeWithFreshBranch` creates the branch and worktree; the subsequent failure returns — `applyWorktreeLFSPolicy` (error text even says "worktree created but LFS pull failed"), `store.CurrentDevice`, `store.InsertWorktree` — all `return state.Worktree{}, err` without removing the just-created worktree or branch. On an LFS repo with `lfs_policy=agent|always` and a flaky network (or any DB error), every `agent run`/`worktree new` retry leaves a full checkout under `~/.devstrap/worktrees/<project>/` plus an `agent/...` branch, untracked by SQLite — so `worktree list`/`cleanup` (which walk `ListWorktrees`) can't see or reap it, and the planned DB-driven `P4-GIT-04` GC never finds it. The shipped M2 cleanup runs only *after* `createFreshWorktree` returns successfully, so it doesn't cover these earlier points (and even it removes only the worktree, not the branch).

**Evidence.** `internal/cli/worktree.go:170` (`addWorktreeWithFreshBranch`), `:174-176/177-180/181-193` (failure returns, no cleanup), `:232` (error omits `wtPath`), `:566-571` (fresh random-suffixed branch per retry); `agent.go:72-81` (post-success M2 cleanup, worktree only); `doctor.go:332` (only checks repo locks).

**Recommendation.** Register `cleanup := func() { _ = r.WorktreeRemove(ctx, localPath, wtPath, true); _, _ = r.Run(ctx, localPath, "branch", "-D", branch) }` and invoke it on the LFS/CurrentDevice/InsertWorktree error returns — or restructure so `InsertWorktree` runs first and the row is marked removed/dirty on LFS failure, keeping the DB the single owner of cleanup. Apply the branch-`-D` to the M2 path too. Include `wtPath` in the LFS error. Add a doctor check listing on-disk worktrees (`git worktree list --porcelain`) with no `worktrees` row.

**Actionable steps.**
1. Add failure-path cleanup (worktree + branch) at the three returns and the M2 path.
2. Add the doctor orphan-worktree check.
3. Test: stub the worktree adder so LFS pull fails; assert neither the path nor the branch survives.

**References:** use `git worktree list --porcelain` prunable annotations + `git worktree prune`/`repair` for programmatic lifecycle ([git-worktree](https://git-scm.com/docs/git-worktree.html)).

### P6-GIT-06 — `agent pr` never checks the run's status; failed or crash-stuck-`running` runs can be pushed and PR'd

**Severity / Effort / Category:** P3 / S / git · agents · _new_

**Problem.** `newAgentPRCommand` loads the run and proceeds through drift check, push, and PR creation without ever reading `run.Status`. spec/10 PR-flow step 1 is "ensure agent run status is complete or reviewable." So `devstrap agent run … ; devstrap agent pr <id>` after a failed run (tests exited non-zero, `status='failed'`) opens a PR of broken work with no warning. Separately, status is set to `running` at insert and corrected only by `UpdateAgentRunResult` after the process returns, so a SIGKILL/crash leaves the row `running` forever — no reconciliation exists in `doctor` or `agent list`, and that phantom run is also PR-able. **Note (verifier):** `WorktreeByID` filters `status='active'`, so runs whose worktree was finalized/removed are already blocked; the unguarded path is precisely failed/crashed runs, which keep their worktree active.

**Evidence.** `internal/cli/agent.go:203` (loads run), `:220-246` (drift/push/PR, no status read), `:95-99` (insert `running`), `:112` (corrected after return); `AgentRunByID` has no status filter; `processAlive` used only for repo locks.

**Recommendation.** (1) After loading the run: reject unless `Status == "complete"` with a `--allow-incomplete` override that warns. (2) Stale-`running` reconciliation: record the runner PID (needs an `agent_runs` migration + spec/12 doc update, so this half is ~M) and have `doctor`/`agent list` sweep `UPDATE agent_runs SET status='interrupted' WHERE status='running'` for dead PIDs.

**Actionable steps.**
1. Add the status gate + `--allow-incomplete` flag.
2. Add the PID column migration + dead-PID sweep.
3. Testscript: failed run → `agent pr` exits invalid-config; `--allow-incomplete` proceeds to dry-run.

---

## CLI, UX & Developer Experience

The command layer is disciplined on exit codes and redaction, but several commands quietly disagree with their own help/spec: re-`init` forks the root, `scan --adopt` accepts any directory, usage errors don't use the documented code, `--quiet` no-ops, and onboarding still points at the test-only hub.

### P6-CLI-01 — Re-running `devstrap init` with a new root creates a silent split-brain: DB root updates, config.yaml does not — SHIPPED 2026-07-03 (PR #72)

**Severity / Effort / Category:** P2 / S / cli · correctness · _new_

**Problem.** `writeDefaultConfig` returns nil without writing when config.yaml already exists, while `state.EnsureWorkspace` unconditionally `UPDATE workspaces SET … root_path=?`. Reproduced with the built binary: after `init root1` then `init root2`, `devstrap status` prints "Root: …/root2" (DB) but config.yaml still says `root: …/root1`, and `devstrap scan` (root from viper/config) scans root1. A user relocating their workspace (`devstrap init ~/Projects`) gets a success banner and agreeing `status`, but every path-resolving command (scan, materialize, sync's eager clone, open) keeps operating on the old root — future adoptions and clones land in the tree the user believes was abandoned, and status/scan permanently disagree.

**Evidence.** `internal/cli/init.go:182-183` (early-return when config exists); `internal/state/store.go:473-480` (unconditional root UPDATE); status reads DB root (`status.go:86`), scan/add/materialize read viper root (`root.go:144-179`).

**Recommendation.** Before `EnsureWorkspace`, read the existing workspace root; if it differs from the *effective resolved* requested root, fail with `appError{code: exitConflict, err: fmt.Errorf("workspace already rooted at %s; re-run with --move-root to relocate", oldRoot)}`. When accepted (or by default), rewrite config.yaml atomically (temp + rename, 0600) instead of early-returning. **Note (verifier):** `DEVSTRAP_ROOT`/`--root` override config.yaml via viper, so compare against the effective root and cover both the positional-arg and `--root` forms; guard the write path only (`init --dry-run` touches neither store).

**Actionable steps.**
1. Detect a root change; refuse (or `--move-root`) and rewrite config.yaml atomically.
2. Longer term, make the DB workspace row the single source of truth for root; config is bootstrap only.
3. Testscript: `init A`, `init B`; assert `scan` and `status` agree.

### P6-CLI-02 — `scan <dir> --adopt` accepts any directory and poisons the shared namespace with out-of-tree projects — SHIPPED 2026-07-03 (`fix/p6-cli-02`)

**Severity / Effort / Category:** P2 / S / cli · data-integrity · _new_

**Problem.** `scan` takes `root := opts.paths().Root; if len(args)==1 { root = args[0] }` with no check that the positional root is the workspace root; `adoptFindings` then stores namespace-relative `Path: finding.Path` and emits signed `project.added` events. Reproduced: with the workspace at `…/root2`, `devstrap scan $SCRATCH/foreign --adopt` adopted a repo outside the managed tree. Because namespace paths are joined to *each device's* managed root on materialize, `devstrap scan ~/Downloads --adopt` makes every git repo in Downloads a fleet-wide namespace event; on the next sync every other device eagerly blobless-clones them into its `~/Code`. One command silently rewrites the fleet namespace. **Note (verifier):** `LocalPath` isn't carried in the payload, so the broken out-of-root local path afflicts only the scanning device (and `VerifyWithinRoot` refuses to materialize it there) — but the namespace event still propagates, which is the core defect.

**Evidence.** `internal/cli/scan.go:28-31` (any positional root, no check); `adoptFindings` (`scan.go:125`) → `CreateProjectEvent` (no path validation); other devices join `project.Path` to their root (`materialize.go:218-224`).

**Recommendation.** Gate `--adopt` on the scanned root matching the workspace root: after computing `rootAbs`, `if adopt && rootAbs != wsRoot { return appError{code: exitUsage, err: fmt.Errorf("--adopt only adopts from the workspace root %s (scanned %s); scan without --adopt to inspect, or use 'devstrap add' for a single repo", wsRoot, rootAbs)} }`. Keep plain read-only scans of arbitrary directories working. If subtree adoption is wanted later, rebase `finding.Path` against `wsRoot`.

**Actionable steps.**
1. Add the adopt-root guard.
2. CLI test asserting the refusal for an out-of-root `--adopt`.

### P6-CLI-03 — Usage errors exit 1, not the documented `exitUsage=10`; spec/13's CLI-04 note is false

**Severity / Effort / Category:** P3 / S / cli · contract · _new (deeper than pass-2 CLI-04)_

**Problem.** `root.go:30` declares `exitUsage = 10` "for bad-flag/missing-flag/arg-count usage errors," but only two hand-mapped sites use it. Empirically `devstrap --frobnicate`, `devstrap add` (arg-count), and `devstrap frobnicate` all exit 1 — Cobra's flag-parse, Args-validation, and unknown-command errors bypass the `appError` mapping and fall through to `exitGeneric=1`. Yet spec/13:510 claims exitUsage=10 covers exactly these, and the exit-code table (spec/13:470-479) omits 10 and the shipped 100+N child codes. Scripts/agents written against the documented contract can't distinguish a typo'd flag from a runtime failure.

**Evidence.** `internal/cli/root.go:30` (constant), `env.go:104/114` + `conflicts.go:108` (only hand-mapped uses); no `SetFlagErrorFunc`; `add.go:22` `cobra.ExactArgs(1)` raw; spec/13:470-479,510.

**Recommendation.** Wire the Cobra seams: (1) `cmd.SetFlagErrorFunc(func(c, err) error { return appError{code: exitUsage, err: err} })`; (2) wrap positional validators once (`usageArgs(cobra.ExactArgs(1))`) so Args errors carry `exitUsage`; (3) map unknown-command errors in `ExitCodeWithWriter`/a root `Args` check; (4) update spec/13's exit-code table to include 10 and 100+N. (SilenceUsage/SilenceErrors are already set, so no double usage text.)

**Actionable steps.**
1. Add `SetFlagErrorFunc` + `usageArgs` wrapper + unknown-command mapping.
2. Fix the spec/13 exit-code table.
3. `root_test`: `--frobnicate` exits 10.

### P6-CLI-04 — `--quiet` promises "only print errors" but only lowers slog verbosity; every command's stdout output ignores it

**Severity / Effort / Category:** P3 / S / cli · contract · _new_

**Problem.** `--quiet` (help: "only print errors") is consumed solely by `logging.Configure` (slog level). No stdout site checks it: `status --quiet` still prints the full table; sync's "Synced events: …", materialize's "Materialized n/m…", and init's banner + hint all print regardless; the run-loop tick banner prints to stderr unconditionally. A user scheduling `run-loop --once --quiet` from cron expecting mail only on errors gets the full "pushed 0, pulled 0; materialized 0/0" chatter every tick, drowning real failures.

**Evidence.** `internal/cli/root.go:80` (flag), `:69` → `internal/logging/logging.go:19` (only consumer); `sync.go:144`, `materialize.go:81`, `init.go:126`, `run_loop.go:71` print unconditionally; `render.go:13` checks only `--json`.

**Recommendation.** Define quiet semantics and enforce them at the render seam: progress/summary chatter is suppressed; errors and explicitly-requested data (`--json`, `status`/`list`/`show` tables) still print. Add `func (o *options) progressf(w, format, a...) { if o.quiet { return }; fmt.Fprintf(w, format, a...) }` and route sync/materialize/init/hub-gc summary lines through it. Zero-cost stopgap: reword the flag help to "suppress log output (command results still print)" — which matches spec/13:463's verbosity-only documentation.

**Actionable steps.**
1. Add `progressf`; route summary/progress lines through it.
2. Or reword the flag help to verbosity-only (spec-consistent).

### P6-CLI-05 — README and the init next-steps hint steer users to the test-only file hub; the shipped `r2://` path is undocumented — SHIPPED 2026-07-03 (`fix/p6-cli-05`)

**Severity / Effort / Category:** P3 / S / cli · docs · _new (post-PR-#24 staleness)_

**Problem.** README:102 says "the hosted R2 backend is wired but not yet switched on," the quickstart shows only `sync --hub-file /tmp/...`, and README:242 lists "wire the R2/S3 hub backend behind the shipped `hubFromOptions` seam" as a near-term priority — but PR #24 shipped exactly that (`hub: r2://<bucket>`). `init.go:126` hardcodes the hint `devstrap sync --hub-file <path>`, and `sync.go:65`'s dry-run prints "Would push %d local events to %s" with an empty target when the hub comes from config. Neither `hub: r2://` nor `DEVSTRAP_HUB_S3_*` appears anywhere in README. A new user follows the README, syncs two machines through a `/tmp` file hub that can't span devices, and concludes the core promise doesn't work.

**Evidence.** README:102,114,177-178,242; `internal/cli/hub.go:73-116` (`r2://` shipped); `init.go:126` (hint); `sync.go:65` (empty dry-run target).

**Recommendation.** (1) Flip README §Project-status:102 and Roadmap:242 to "R2/S3 backend shipped (`hub: r2://<bucket>` + `DEVSTRAP_HUB_S3_*`)"; add a quickstart step showing the config line + env vars (link spec/19). (2) Change `init.go:126`'s hint to mention configuring a hub instead of `--hub-file`. (3) Fix `sync.go:65` to print the resolved hub ID, not the raw `--hub-file` flag. Optionally add `devstrap init --hub <uri>`.

**Actionable steps.**
1. Update README project-status + roadmap + quickstart.
2. Update the init hint and the dry-run target string.
3. (Optional) `init --hub <uri>` to write the config key.

---

## Data Model & SQLite

The schema is robust, but this pass's TOCTOU/atomicity focus surfaced a data-loss GC interaction, a broken shipped query, a dual-write divergence gap, an incomplete backup, a missing hot-path index, and a half-applied singleton invariant.

### P6-DATA-01 — The origin device never records its own draft snapshot row, so routine sync GC and `hub gc` delete the live draft bundle blob — SHIPPED 2026-07-02 (PR #35)

**Severity / Effort / Category:** P1 / S / data · data-loss · _new_

**Problem.** `draft.go:92` does `InsertLocalEvent(NewDraftSnapshotEvent(...))` and returns — no `draft_snapshots` row is written on the creating device. The only production writer is the sync **apply** path (`RecordDraftSnapshotTx`), but `ApplyEvents` skips application for events already present locally (`if !inserted { return nil }`), and the origin's own event is always already present. `Store.RecordDraftSnapshot` has zero non-test callers. So on the device that ran `draft snapshot create`, the new blob is referenced by nothing in its own SQLite (`LatestDraftSnapshot` nil; `RetainedBlobRefs` omit it). Concrete loss: create a draft snapshot → `sync` (pushes blob, then local GC deletes the local cache copy as unreferenced) → `hub gc` from the same device (`RetainedBlobRefs` lacks the ref → the only hub copy is `DeleteBlob`'d). The immutable `draft.snapshot.created` event survives, so every newly-enrolled device gets "referenced blob missing from hub," and the origin can never re-materialize its own draft. This fires on a single device with routine commands — the normal pre-second-machine state.

**Evidence.** `internal/cli/draft.go:92`; `Store.RecordDraftSnapshot` (`store.go:1713`) — no non-test callers; `internal/sync/events.go:491` (`RecordDraftSnapshotTx`, reachable only via apply), `events.go:299` (`if !inserted { return nil }`); `sync.go:141` (`gcUnreferencedBlobs`), `blob_gc.go:284`; `hub.go:253,272` (`hub gc` uses `RetainedBlobRefs`).

**Recommendation.** Wire the caller-less recording API into the create path, in one transaction with the event insert: a `store.WithTx` block calling `tx.InsertEvent` + `tx.RecordDraftSnapshotTx` so event and row commit atomically. Apply the same audit to `emitSupersedingDraftSnapshot` (`blob_gc.go:181`). **Note (verifier):** the real secondary defect is that on the origin the revoke rewrap loop (`DraftBlobRefs`) never reaches the draft ref because no row exists, so the origin skips rewrapping its own draft blob on revoke (leaving the revoked device's copy readable) — not that `UpdateBlobRef` repoints nonexistent rows. This upgrades the "optional" note in shipped `P5-QUAL-01` from a materialize-UX nicety to a P1 data-loss fix.

**Actionable steps.**
1. Record the `draft_snapshots` row atomically at create time.
2. Audit `emitSupersedingDraftSnapshot` for the same missing-row assumption.
3. Test: create snapshot on A → `sync` + `hub gc` on A → assert `LatestDraftSnapshot` non-nil and the blob still exists locally and on the hub.

### P6-DATA-02 — `ClearRotationForProject` filters on a non-existent `env_profiles.namespace_id` column, so `devstrap env rotate <path>` always fails — SHIPPED 2026-07-03 (`fix/p6-data-02`)

**Severity / Effort / Category:** P2 / S / data · correctness · _new (defect in shipped `P5-PROD-03`)_

**Problem.** The one-arg `env rotate <path>` (flag-clear-only) runs `UPDATE secret_bindings … WHERE … env_profile_id IN (SELECT id FROM env_profiles WHERE namespace_id = ?)`, but `env_profiles` has no `namespace_id` column (the link is `namespace_entries.env_profile_id`). Reproduced against the real schema: `no such column: namespace_id`. So the flag-clear-only form errors on every invocation with "clear rotation for project: SQL logic error." The only test covers `env rotate --all`. **Note (verifier):** the two-arg re-capture form (`env rotate <path> <env-file>`) *does* clear the flags as a side effect — `SaveEncryptedEnvProfile` DELETEs+re-INSERTs `secret_bindings` with `needs_rotation` DEFAULT 0 before the command's trailing error — so "doctor stays red forever" is wrong; only the one-arg form fails with zero effect, and `--all` is a documented workaround. Hence P2 (loud SQL error, not silent), not P1.

**Evidence.** `internal/state/store.go:1632-1637` (bad subquery); migration 00001 `env_profiles(id, workspace_id, name, provider, mode, created_at, updated_at)`; `internal/cli/env.go:130` passes `project.ID`; `env_test.go` covers only `--all`.

**Recommendation.** Join through `namespace_entries`: `UPDATE secret_bindings SET needs_rotation = 0, updated_at = ? WHERE needs_rotation = 1 AND env_profile_id IN (SELECT env_profile_id FROM namespace_entries WHERE id = ? AND env_profile_id IS NOT NULL)`. Add a per-project store test (capture → `MarkEncryptedBindingsNeedingRotation` → `ClearRotationForProject` → assert cleared count). Consider a CI lint that `db.Prepare`s every static query in `store.go` against a migrated in-memory DB so column-reference bugs can't ship.

**Actionable steps.**
1. Fix the subquery + add the per-project test.
2. Add the prepare-all-queries lint.

### P6-DATA-03 — Event emission and derived-state mutation are dual-written in separate transactions; a crash between them permanently diverges the origin — SHIPPED 2026-07-03 (PR #61)

**Severity / Effort / Category:** P2 / M / data · convergence · _new_

**Problem.** `add.go:68-92` calls `CreateProjectEvent` (which commits its own `WithTx`) then `store.UpsertProject` (a second independent transaction); `adoptFindings` and both `conflict_resolve.go` sites share the pattern. The self-heal path is closed: `ApplyEvents` does `if !inserted { return nil }`, and since the pull cursor differs from the push cursor the origin pulls its own event back but insert returns false, so `applyEventTx` never runs. If the process dies (hard kill, power loss, disk-full, `SQLITE_BUSY` past the 5s timeout from a concurrent run-loop) between the two commits, the origin holds a committed `project.added` event with **no `namespace_entries` row**. The event syncs; every other device inserts and applies it (project appears) — but the origin skips apply forever. Silent permanent divergence on the origin, in a product whose core invariant is cross-device convergence. **Note (verifier):** the `SQLITE_BUSY`/disk-full variants surface an error the CLI shows, and a retried `add`/`scan --adopt` heals the origin; the truly silent case is a hard kill/power loss or an error in an unattended run-loop cycle.

**Evidence.** `internal/cli/add.go:68-92`; `store.InsertLocalEvent` commits its own `WithTx` (`store.go:2261-2294`), `store.UpsertProject` opens a second (`:782-836`); `scan.go:125-142`; `conflict_resolve.go:173,207`; `internal/sync/events.go:299-301`; seams exist: `Tx.UpsertProject` (`store.go:839`), `Tx.InsertEvent` (`:2301`), `nextLocalEventStamp` run inside `Tx`.

**Recommendation.** Make event+state a single transaction at every emission site: a `store.WithTx` block with a `Tx`-scoped `CreateProjectEventTx` (reusing `tx.InsertEvent` + `nextLocalEventStamp`) + `tx.UpsertProject`. Cover `add`, `adoptFindings`, and both `conflict_resolve.go` sites. Defense-in-depth alternative that also fixes `P6-DATA-01`'s class: change `events.go:299` to re-run `applyEventTx` even when `inserted==false` — handlers are idempotent (HLC gates, INSERT OR IGNORE), so re-apply is safe and makes the event log the true source of truth.

**Actionable steps.**
1. Add `Tx`-scoped emission helpers; wrap every emission site in one `WithTx`.
2. (Optional) re-apply-on-duplicate in `ApplyEvents`.
3. Test: simulate a crash between the two commits; assert the origin either heals on retry or never diverges.

### P6-DATA-04 — `db backup` produces an incomplete workspace backup: local-only env secret blobs and file-fallback keys are excluded, and there is no restore path — SHIPPED 2026-07-04 (`fix/p6-data-04`)

**Severity / Effort / Category:** P2 / M / data · disaster-recovery · _new_

**Problem.** `Backup` is `VACUUM INTO` + chmod + `validateBackup` — the SQLite file only. Encrypted env values live **outside** the DB as `~/.devstrap/blobs/<hash>.age` files with only `age_blob:<sha256>` refs in `secret_bindings`, and `P5-SEC-04` made env blobs local-only (never pushed to the hub) — so no hub copy exists either. Key fallback (age identity, signing key, and PR-#25 `wck-<ws>-<epoch>.key` files) lives in `<statedir>/keys`. There is no restore command; `doctor.go:203-205` even recommends "restore from a `devstrap db backup`." A user who keeps `db backup` outputs and loses the machine restores a `state.db` whose `secret_bindings` hold **dangling** age_blob refs — every captured env secret is unrecoverable and `env hydrate` fails for all encrypted profiles; on the file-fallback (`DEVSTRAP_NO_KEYCHAIN`/headless) path the device identity and WCK epochs are gone too, so even hub-synced draft blobs become undecryptable. Pass-4 DATA-01 made the backup *validated* but not *complete*.

**Evidence.** `internal/state/store.go:292-306` (`VACUUM INTO` only); `env.go` blob path `paths.Home/blobs/<hash>.age`; `blob_gc.go:53-56` (env blobs local-only, `P5-SEC-04`); key fallback `store.go:233`; no `restore` command; `doctor.go:203-205` (false-confidence remedy).

**Recommendation.** (1) `devstrap db backup --full <out.tar>`: `state.db` via `VACUUM INTO` a temp file + `blobs/` (only refs from `AllBlobRefs`) + `keys/` when the file fallback is active, all 0600 — and in default keychain mode add a keychain export/escrow step (e.g. `devstrap keys export` reading via `HybridStore` into passphrase-protected output), since the age identity never touches the key dir. Include the PR-#25 `wck-<ws>-<epoch>.key` files. (2) `devstrap db restore <in>` (refuse over a non-empty state dir without `--force`) or a documented runbook in spec/12. (3) A doctor "dangling blob refs" check: for each `AllBlobRefs` entry, stat the local blob (and for draft refs fall back to hub `HasBlob`) and warn with a remedy.

**Actionable steps.**
1. Ship `db backup --full` (blobs + keys + keychain escrow) and `db restore`.
2. Add the dangling-blob-refs doctor check.
3. Fix the `doctor.go:203-205` remedy text once `--full` exists.

**References:** a zero-knowledge system needs an explicit user-held recovery secret (Emergency Kit) or losing all devices loses everything — the backup must include the key material, not just the DB ([1Password white paper](https://1passwordstatic.com/files/security/1password-white-paper.pdf)).

### P6-DATA-05 — No index serves `events(device_id, hlc)`; every sync push and doctor run full-scans the event log with a temp B-tree sort — SHIPPED 2026-07-03 (PR #61)

**Severity / Effort / Category:** P3 / S / data · performance · _new_

**Problem.** `LocalPendingEvents` runs `WHERE device_id = ? AND hlc > ? ORDER BY hlc ASC, id ASC`; the only event indexes are `idx_events_order(workspace_id, hlc, device_id, id)` (leads with the unconstrained `workspace_id`) and partial `idx_events_device_seq(device_id, seq) WHERE seq IS NOT NULL` (unusable since `hlc > ?` doesn't imply `seq IS NOT NULL`). `EXPLAIN QUERY PLAN` yields `SCAN events` + `USE TEMP B-TREE FOR ORDER BY`. With event-log compaction still open (`P4-SYNC-02`: the table grows forever), every `devstrap sync`, `run-loop` tick, and `doctor` pays O(total events) scan+sort just to find the usually-tiny new-local-event set.

**Evidence.** `internal/state/store.go:2682-2687` (query); migration 00002 indexes; callers `internal/cli/sync.go:60`, `doctor.go:106`; also serves `previousEventContentHash`'s hlc-fallback (`store.go:2488-2496`).

**Recommendation.** Add migration with `CREATE INDEX idx_events_device_hlc ON events(device_id, hlc, id)` (trailing `id` satisfies the ORDER BY tiebreak, eliminating the temp B-tree; `EXPLAIN` then reports `SEARCH events USING INDEX`). Update spec/12's index inventory; spec/12:184 reserves 00014 for `gitstate_mirror`, so renumber accordingly.

**Actionable steps.**
1. Add the composite index migration + spec/12 update.
2. Verify `EXPLAIN` shows SEARCH + no temp B-tree.

### P6-DATA-06 — No DB invariant enforces a single `local` device; concurrent `devstrap init` can fork the device identity — SHIPPED 2026-07-03 (PR #61)

**Severity / Effort / Category:** P3 / S / data · integrity · _new (asymmetry with the 00006 workspace singleton)_

**Problem.** `EnsureDevice` runs a SELECT for `trust_state = 'local'` and, on `ErrNoRows`, an INSERT of a fresh `dev_<uuidv7>` — two separate autocommit statements (`_txlock=immediate` applies only to `BeginTx`), with no flock/pidfile guarding `init`. Two processes racing first-time init can both see zero local devices and both insert one, yielding two `trust_state='local'` rows with distinct IDs. Migration 00006 gives `workspaces` a singleton index but `devices` has no counterpart. Downstream, the three `LEFT JOIN devices d ON d.trust_state = 'local'` sites (`store.go:1262,1287,1316`) have no dedup, so `ListProjects` returns every project twice; each racing process keys its own device ID; there's no repair path. **Note (verifier):** the "split event seq chains" claim is overstated — post-init stamping goes through `CurrentDevice` (stable MIN(created_at)) — the real damage is the lingering second `local` row, join row-multiplication, duplicate key material bound to a dead ID, and a nondeterministic winner on a created_at tie.

**Evidence.** `internal/state/store.go:487-538` (`EnsureDevice`, two autocommit statements); migration 00006 (workspace singleton, no device counterpart); `UpsertDevice` forbids `local` (`store.go:596-621`), `EnsureRemoteDevice` inserts `pending` (`:2216-2245`) — so the proposed index is safe.

**Recommendation.** Add a partial unique index mirroring 00006: `CREATE UNIQUE INDEX idx_devices_local_singleton ON devices((1)) WHERE trust_state = 'local';` (guard the migration for existing corrupt DBs by first deleting duplicate local rows keeping MIN(created_at)). Make `EnsureDevice` race-tolerant: run SELECT+INSERT inside `s.WithTx` (immediate lock), or treat a UNIQUE-constraint error as "lost the race" and re-run the SELECT. Add a doctor check that `COUNT(trust_state='local') == 1`.

**Actionable steps.**
1. Add the partial unique index + dedup guard migration.
2. Make `EnsureDevice` transactional/race-tolerant.
3. Add the doctor singleton check.

---

## Code Quality, Testing & CI

The drift gate, the release pipeline, and the one real-backend hub test each look like coverage but enforce nothing; two smaller CI/test-hermeticity gaps round it out.

### P6-QUAL-01 — The spec-drift mapped-spec check is vacuously satisfied by the mandatory work-log entry — SHIPPED 2026-07-03 (PR #60)

**Severity / Effort / Category:** P2 / S / process · ci · _new (sharpens `P5-DX-02`)_

**Problem.** `Check()` emits a mapped-spec finding only `if !anyChanged(mapped, changedSet)`, and `spec/18_WORK_LOG.md` declares `tracks_code: [**]`, so spec/18 is in `mapped` for **every** changed file. Because updating spec/18 is already mandatory for any code change (`requiresWorkLog`), every compliant commit automatically satisfies the mapped-spec rule for every file — the entire path→spec mapping table (spec/08 for internal/git, spec/13 for internal/cli, …) is dead in practice. Verified by execution: `Check()` with `ChangedFiles=[internal/cli/root.go, spec/18_WORK_LOG.md]` returns `OK()==true`, no drift finding, despite neither spec/00 nor spec/13 changing. The required "Spec drift" branch-protection check enforces nothing beyond "touch the work log." **Note (verifier):** for files mapped to a spec but outside `requiresWorkLog`'s triggers (only `.gitignore`/spec/11 here), the check can still fire independently; for all code paths the claim holds.

**Evidence.** `internal/specdrift/specdrift.go:76-83` (`anyChanged`), `:264` (`globMatch` true for `**`); `spec/18_WORK_LOG.md:3` (`tracks_code: [**]`); `requiresWorkLog` (`specdrift.go:284`); `cmd/spec-drift/main.go` runs `RequireWorkLog=true`.

**Recommendation.** Exclude catch-all specs from mapped-spec satisfaction. (1) Filter `WorkLogPath` (and any spec matched only via `**`) out of `mapped` before `anyChanged` — record which pattern matched and `if pattern == "**" { continue }`. (2) Precision: when a file has any non-broad mapping, require one of those specific specs to change so touching broad spec/00/02/04/14/16 can't satisfy internal/git changes either. (3) Add the regression test this pass used: `[internal/cli/root.go, spec/18]` must produce a finding. This is complementary to `P5-DX-02`'s generated-inventory recommendation, not replaced by it.

**Actionable steps.**
1. Exclude `**`-matched specs from `mapped`; add the regression test.
2. Require a specific mapped spec when one exists.

### P6-QUAL-02 — The release workflow publishes binaries from any `v*` tag with zero verification — no tests, no vuln check, no main-ancestry check — SHIPPED 2026-07-03 (PR #60)

**Severity / Effort / Category:** P2 / S / ci · supply-chain · _new (distinct from `P4-SEC-05`)_

**Problem.** `release.yml` is a single `goreleaser` job (checkout → setup-go → `goreleaser release --clean`) with `contents: write`, no test/lint/govulncheck step, and no `needs:` gate. Worse, `ci.yml` triggers only on branch push/PR/cron — **tag pushes never trigger CI** — so a tag pointing at a commit never pushed to a branch gets zero automated verification before signed-off GitHub Release binaries publish. A maintainer tagging from a stale/dirty local branch (the repo's own memory notes local `main` runs behind `origin/main`), or any compromised write-scoped credential pushing a tag, ships binaries from a commit that never passed CI — broken tests, a known CVE the daily govulncheck would flag, or code that never landed on main. RELEASING.md's only control is the human step "confirm main is green."

**Evidence.** `.github/workflows/release.yml:12-33` (single job, `contents: write`, no gate); `.github/workflows/ci.yml` has no `tags:` trigger; RELEASING.md's human-only flow.

**Recommendation.** Add a `verify` job and gate goreleaser on it (`needs: verify`): checkout `fetch-depth: 0`; assert the tagged commit is contained in `origin/main` or a `release/*` branch (`git branch -r --contains "$GITHUB_SHA" | grep -Eq 'origin/(main|release/)'`); run `go vet ./... && go test -race ./...` and `govulncheck ./...`. Also add a GitHub tag-protection ruleset for `v*`.

**Actionable steps.**
1. Add the `verify` job + ancestry check; gate goreleaser on it.
2. Add the `v*` tag-protection ruleset.

**References:** the 2025 tj-actions compromise left SHA-pinned workflows untouched; the baseline is pinned actions + least-privilege `permissions:` + Scorecard's pinned-dependencies/token-permissions checks ([Wiz](https://www.wiz.io/blog/github-action-tj-actions-changed-files-supply-chain-attack-cve-2025-30066)); publish SLSA provenance / cosign-signed checksums so the tagged-commit binary is verifiable ([goreleaser SLSA](https://goreleaser.com/blog/slsa-generation-for-your-artifacts/)).

### P6-QUAL-03 — The production S3/R2 adapter's only real-backend integration test is env-gated and never executed by CI — SHIPPED 2026-07-03 (`fix/p6-qual-03`)

**Severity / Effort / Category:** P2 / S / ci · testing · _new (follow-through gap of shipped `P5-HUB-01`)_

**Problem.** `TestR2MinIOConformance` skips unless `DEVSTRAP_HUB_S3_ENDPOINT` is set; `ci.yml` sets no `DEVSTRAP_HUB_S3_*` variable and runs no MinIO container, so it skips on every CI run — only the in-memory `memS3` fake exercises the conformance contract automatically. The live hub path (PR #24) is now the production sync backend, but every automated run proves the adapter only against a hand-written fake. Regressions in conditional-put/`If-None-Match` semantics, pagination, `mapS3Error`, checksum handling, and endpoint resolution pass CI and surface only when a user syncs against live R2; Dependabot aws-sdk-go-v2 bumps merge green with zero real round-trip. **Note (verifier):** the gate is deliberate and loudly documented (the test comment, spec/16:350, spec/18:81, and the P5 recommendation all say "so default CI stays hermetic"), so frame this as challenging that decision. MinIO ≠ R2, so this reduces but doesn't eliminate the real-backend gap.

**Evidence.** `internal/hub/r2_minio_test.go:31-38` (skip gate); `.github/workflows/ci.yml` sets no `DEVSTRAP_HUB_S3_*`; spec/18_WORK_LOG.md:81 ("requires Docker, not run in CI").

**Recommendation.** Add a Linux CI job that boots MinIO via `docker run` (GitHub `services:` can't pass a command to the minio image) and runs the test unmodified with `DEVSTRAP_HUB_S3_*` set; pin the image by digest to match the repo's action-pinning posture; use a 2024+ MinIO image for `If-None-Match` conditional-put support. The `go test` invocations stay hermetic, satisfying the "default CI hermetic" constraint. Run it non-required first (Docker flakiness) before adding to branch protection.

**Actionable steps.**
1. Add the `docker run` MinIO conformance job (digest-pinned).
2. Run non-required initially; promote to required after it proves stable.

### P6-QUAL-04 — SSH-alias forge tests shell out to the real `ssh -G`, which reads the machine's real ssh config; the `ssh -G` branch is never deterministically tested — SHIPPED 2026-07-03 (`fix/p6-qual-04`)

**Severity / Effort / Category:** P3 / S / testing · hermeticity · _new (defect in shipped `P5-CLI-04`)_

**Problem.** `TestResolveSSHHostAlias` sets `HOME` to a temp dir and writes a fixture `~/.ssh/config`, then calls `resolveSSHHostAlias` — which first runs the real `ssh -G alias` via `exec.LookPath`. OpenSSH resolves `~/.ssh/config` from the passwd entry (pw_dir), **not** `$HOME`, so the subprocess reads the developer's real user/system config, not the fixture; the test passes only because the Go fallback parser (which uses `os.UserHomeDir → $HOME`) kicks in when `ssh -G` reports no override. Two failure modes: (a) on any machine whose real config produces a HostName override for the test aliases (common multi-account `Host`/`Include`/`CanonicalizeHostname` blocks), the test returns a machine-dependent value and fails/flips forges; (b) the preferred `ssh -G` code path — the actual P5-CLI-04 fix — has zero deterministic coverage. Reproduced empirically on this machine.

**Evidence.** `internal/cli/forge.go:154,167-174` (real `ssh -G` via LookPath); `forge_test.go:106-130` (fixture config, no ssh stub); no test touches `sshDashGHostName`.

**Recommendation.** Stub `ssh` via a PATH shim so both branches are tested hermetically: write a temp `ssh` script and prepend its dir to `PATH` (`t.Setenv`) — `stubSSH(t, "exit 1")` forces the fallback parser; `stubSSH(t, 'echo "hostname git.acme.com"')` tests the `ssh -G` branch. **Note (verifier):** `t.Setenv("PATH")` affects `exec.LookPath` and the absolute path is exec'd, so the shim covers both branches.

**Actionable steps.**
1. Add `stubSSH` PATH-shim helper.
2. Test the `ssh -G` branch and the fallback deterministically.

### P6-QUAL-05 — CI runs the full 5-job matrix twice per PR commit (push on `**` + pull_request) with no concurrency cancellation — SHIPPED 2026-07-03 (`fix/p6-qual-05`)

**Severity / Effort / Category:** P3 / S / ci · cost · _new_

**Problem.** `ci.yml` triggers on both `push: branches: ["**"]` and `pull_request`, and no `concurrency:` block exists in any workflow. Given the mandated in-repo topic-branch → PR workflow, every PR-branch commit fires both events, running spec-drift + lint + 2×test (ubuntu+macos) + vuln twice per SHA, and rapid pushes stack uncancelled duplicate matrices. Pure waste — the `pull_request` run alone satisfies branch-protection required checks.

**Evidence.** `.github/workflows/ci.yml:3-10` (both triggers, plus schedule); repo-wide grep finds no `concurrency:`.

**Recommendation.** Scope push to `main` (post-merge coverage) and add cancellation:
```yaml
on: { push: { branches: [main] }, pull_request: {}, schedule: [{cron: "17 6 * * *"}] }
concurrency:
  group: ci-${{ github.workflow }}-${{ github.head_ref || github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

**Actionable steps.**
1. Scope push triggers to main; add the concurrency block.

---

## Cross-Platform, Ignore & Scan

Six findings converge on the same theme: the ignore/prune compiler, the portable `run-loop`, headless key custody, and `scan`'s onboarding-time behavior each silently diverge from their own documented contracts — dropping content, minting a second identity, or hanging on the network — without ever raising an error the user can act on.

### P6-XP-01 — `ShouldPruneDir`'s bare-name fallback silently defeats anchored and negation patterns, dropping re-included content from draft bundles — SHIPPED 2026-07-03 (`fix/p6-xp-01`)

**Severity / Effort / Category:** P2 / S / ignore-compiler · draft-bundle · prune-semantics · silent-data-loss · _new (no prior pass audits prune/compiler semantics — fuzzing only checked non-panic)_

**Problem.** `ShouldPruneDir`'s bare-name fallback re-evaluates the full pattern list against a directory's bare name, with all path context stripped, whenever the full-path match fails. That makes root-anchored patterns (`/build/`) prune at every depth, and lets a negation that re-includes a nested directory (`!keep/build/`) be silently defeated — a negation can never match a bare name. The only live consumer today is the draft-bundle path: `devstrap draft snapshot create` compiles a project's `.devstrapignore` via `CompileFromDir` and feeds it to `Pack`, which calls `ShouldPruneDir`. A project with `build/` + `!keep/build/` in its ignore file therefore silently omits `keep/build` from the age-encrypted bundle, with no error or warning — if the source device is later lost, that explicitly re-included content is gone despite sync reporting success. (Scan's own prune matcher is defaults-only today, so the same bug is a latent hazard there rather than a currently-live one — see P6-XP-06.)

**Evidence.** `internal/ignore/ignore.go:73-78` (bare-name fallback pass, redundant given the `(?:^|/)` unanchored-match prefix at `ignore.go:223`); `internal/cli/draft.go:68` (`CompileFromDir` feeds `draftbundle.Pack`); `internal/draftbundle/draftbundle.go:113` (calls `ShouldPruneDir` with the full `relSlash`); probe: with pattern `/build/`, `Match("pkg/foo/build", true)=false` but `ShouldPruneDir("build","pkg/foo/build")=true`; with `build/` + `!keep/build/`, `Match("keep/build", true)=false` (correctly re-included) but `ShouldPruneDir("build","keep/build")=true` (pruned anyway).

**Recommendation.** Delete the fallback and make `relSlash` authoritative, keeping a guard only for callers that genuinely lack a path:

```go
func (m *Matcher) ShouldPruneDir(name, relSlash string) bool {
    if m == nil {
        return DefaultMatcher().ShouldPruneDir(name, relSlash)
    }
    if relSlash == "" {
        relSlash = name
    }
    return m.Match(relSlash, true)
}
```

**Actionable steps.**
1. Replace `ShouldPruneDir`'s body with the `relSlash`-only form above.
2. Add regression tests: `/dist/` must not prune `packages/foo/dist`; `build/` + `!keep/build/` must keep `keep/build`.
3. Extend the draft-bundle test to assert the packed manifest actually contains `keep/build/...` under that policy.

---

### P6-XP-02 — The ignore compiler diverges from the gitignore semantics it advertises (middle-slash anchoring, bracket classes, whole-file failure) — SHIPPED 2026-07-04 (`fix/p6-xp-02`)

**Severity / Effort / Category:** P2 / M / ignore-compiler · gitignore-semantics · draft-sync · _new (no prior pass audits pattern-compiler semantics; spec/11 claims gitignore-compatibility without caveats)_

**Problem.** The compiler's own doc header claims "Pattern semantics follow .gitignore," but three real divergences break that contract on the draft-sync data plane. First, `parseLine` anchors a pattern only on a *leading* `/`; real gitignore anchors on a separator at the beginning **or** middle. So a project's own `.devstrapignore` line `docs/build/` — meant to mean root-only, per real gitignore — also silently excludes `packages/site/docs/build`, and the identical text emitted via `GitignoreFragment` then means something different to git than to DevStrap. Second, `patternToRegex`'s escape set omits `[`/`]`, so `[!a]log` matches the opposite set from fnmatch negation (`alog`/`!log` match, `blog` doesn't). Third, one unclosed `[` — a legal literal character in real gitignore — makes `Compile` return a raw regexp parse error and fail the *whole file*, so `devstrap draft snapshot create` hard-fails until the user finds the offending line, whereas git simply treats the pattern as non-matching (or literal) and never aborts. (The shipped default `data/raw/` also shows the anchoring divergence, though that specific default arguably intends any-depth junk-matching like `node_modules/`; the load-bearing failure is user-authored middle-slash lines.)

**Evidence.** `internal/ignore/ignore.go:185-188` (`p.anchored` set only on a leading `/`); `ignore.go:246` (escape set omits `[`/`]`); `ignore.go:230-238` (`a**b` compiles to `.*`, so it crosses `/` unlike git's `**`); probes: `data/raw/` (shipped default, `ignore.go:301`) matches `experiments/data/raw`; `[!a]log` matches `alog`/`!log` but not `blog`; `Compile("foo[1.txt")` fails with `error parsing regexp: missing closing ]`.

**Recommendation.** Align `parseLine`/`patternToRegex` with gitignore: anchor on any separator, translate bracket classes to a real regex class, and treat unbounded `**` as a plain `*`.

```go
body := strings.TrimSuffix(strings.TrimPrefix(raw, "!"), "/")
p.anchored = strings.Contains(body, "/")
// bracket classes: map leading '!' or '^' to '[^...]', escape '\', and
// fall back to a literal '\[' when unclosed instead of failing Compile.
// '**' not bounded by '/' on both sides -> '[^/]*' (regular *), not '.*'.
```

Add a differential test that runs the Matcher against `git check-ignore --verbose` (skipped when git is absent) over a corpus of middle-slash, bracket, and `a**b` patterns so future drift is caught mechanically.

**Actionable steps.**
1. Change `parseLine` to set `anchored = strings.Contains(body, "/")`.
2. Rewrite bracket-class handling in `patternToRegex` to a proper regex class with correct negation, and make an unclosed `[` degrade to a literal match rather than failing `Compile`.
3. Fix `**` handling so it only crosses `/` when explicitly slash-bounded on both sides.
4. Add the `git check-ignore --verbose` differential test to catch future semantic drift.

---

### P6-XP-03 — `run-loop` never runs its advertised scan stage, so new local projects on device A never reach the hub — SHIPPED 2026-07-04 (`fix/p6-xp-03`)

**Severity / Effort / Category:** P2 / M / run-loop · sync-engine · spec-drift · _new (P5-QUAL-02/03 and P5-CLI-05 touched run-loop for tests/jitter/stderr but none noticed the missing scan stage; the original XP-02 spec explicitly required it)_

**Problem.** `run-loop`'s `Short` text ("Run scan + sync + materialize on an interval (portable, no daemon)") and its package doc comment both promise a scan stage, and the original XP-02 workstream that commissioned the loop explicitly required "scan → sync → materialize." But `runLoopTick` only calls `runSyncCycle` (push-pending → pull/apply → pull blobs → materialize) — there is no `scan.Walk`/`adoptFindings` call anywhere in `run_loop.go`, `sync.go`, or `materialize.go`, and the tick's own stderr header even prints `"run-loop tick: sync + materialize"`, contradicting the command's own help text. Because the FSEvents/inotify watcher is unwired (PLAT-03) and the daemon is deferred, there is currently **no automatic local→hub path at all**: a repo `git init`'d or manually cloned into `~/Code` on device A while `run-loop` runs is never adopted, never emits `project.added`, and device B never materializes it — the user only discovers the gap when work is missing after a device loss. A naive fix has a sharp edge: `adoptFindings` unconditionally calls `dssync.CreateProjectEvent` for every finding on every invocation, so calling it once per tick would append duplicate `project.added` events every interval unless adoption is first made idempotent against the existing `ProjectByPath` row.

**Evidence.** `internal/cli/run_loop.go:32` (`Short: "Run scan + sync + materialize..."`); `run_loop.go:20-24` ("scan → sync → materialize" doc comment); `run_loop.go:69-73` (`runLoopTick` calls only `runSyncCycle`); `run_loop.go:71` (stderr header literally says `"sync + materialize"`); `internal/cli/sync.go:40-147` (`runSyncCycle`'s actual stages); `internal/scan/scan.go:125` (`adoptFindings` unconditionally calls `dssync.CreateProjectEvent` per finding — the idempotency hazard); `README.md:108,197` and `spec/00_START_HERE.md:172` (repeat the "scan → sync → materialize" claim); `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md:478` (original XP-02 requirement).

**Recommendation.** Add a scan+adopt step at the start of `runLoopTick`, reusing the existing `adoptFindings`, but make adoption idempotent first (skip findings whose `ProjectByPath` row already matches the same `remote_key`/type) and route warning-class findings (secret-looking files, symlink escapes, duplicate remotes) to stderr, never auto-adopting them:

```go
func runLoopTick(ctx context.Context, opts *runLoopOptions, stderr io.Writer) error {
    store, err := opts.openState(ctx)
    if err != nil {
        return err
    }
    defer store.Close()

    if res, err := scan.Walk(ctx, opts.paths().Root, scan.Options{IncludePlainFolders: true}); err != nil {
        fmt.Fprintf(stderr, "run-loop scan: %v\n", err)
    } else if n, err := adoptNewFindings(ctx, store, opts.paths().Root, res); err != nil {
        fmt.Fprintf(stderr, "run-loop adopt: %v\n", err)
    } else if n > 0 {
        fmt.Fprintf(stderr, "run-loop tick: adopted %d new project(s)\n", n)
    }
    return runSyncCycle(ctx, opts, stderr)
}
```

If pull-only behavior is instead the deliberate choice, fix `run_loop.go`'s `Short`/doc comment, `README.md:108,197`, and `spec/00_START_HERE.md:172` to say "sync + materialize" — but that choice should be explicit, since it silently drops the A→B half of the Dropbox promise.

**Actionable steps.**
1. Add a `scan.Walk` + idempotent-adopt step before `runSyncCycle` in `runLoopTick`.
2. Make adoption idempotent: skip findings whose `store.ProjectByPath` row already matches the same `remote_key`/type.
3. Fail-safe-route secret/symlink-escape/duplicate-remote warning findings to stderr; never auto-adopt them.
4. If the scan stage is deliberately out of scope, correct the Short text, doc comment, `README.md:108,197`, and `spec/00_START_HERE.md:172` instead.

---

### P6-XP-04 — A keychain-`unavailable` substring heuristic mints a divergent identity on headless Linux, wedging sync for the exact service-install target — SHIPPED 2026-07-03 (PR #62)

**Severity / Effort / Category:** P2 / M / keychain · key-custody · headless-linux · _new (PASS5 twice asserted "the keychain fallback fails closed" — this shows a mixed-context case where it doesn't, and the same bug now also guards the newer WCK custody path)_

**Problem.** `keychainUnavailable` classifies keyring errors by substring match over `err.Error()` (needles like `"not found"`, `"dbus"`, `"connection refused"`), and `loadSecret` maps any match to `os.ErrNotExist`. `EnsureSigning` then mints a brand-new signing identity and persists it to the `0600` file store whenever `ReadSigning` reports not-exists — without ever consulting the device's already-published `devices.signing_public_key`. On headless Linux (exactly the cron/systemd-unit target of the deferred `service install` work), the Secret Service is session-scoped: running any event-stamping command (`add`, `scan --adopt`, `sync`, `run-loop`) without `DBUS_SESSION_BUS_ADDRESS` produces a `"dbus"`/`"connection refused"` error, gets classified unavailable, and `EnsureSigning` mints a second identity into `~/.devstrap/keys`. `ensureLocalEventSignature` calls `EnsureSigning` on every locally-originated event, and only afterwards does `setDeviceSigningPublicKey`'s SQL guard (`WHERE id = ? AND (signing_public_key IS NULL OR signing_public_key = ?)`) reject the mismatch with a cryptic "device signing public key mismatch" — after the divergent key file is already on disk, outside the transaction. Every subsequent headless run then reads the orphan file key and fails permanently (`run-loop` aborts after 5 consecutive-failure ticks), while desktop runs keep working — a split-custody wedge recoverable only by manually deleting the key file. The outcome is also error-string dependent: a dead D-Bus socket address yields `"no such file or directory"` (no needle match), taking the fail-closed `ReadSigning` path instead — two similar bus failures diverge into opposite outcomes. The same substring heuristic now also guards the newer WCK custody path (`StoreWCK`/`LoadWCK`), so this bug's blast radius includes the workspace-key foundation, not just device signing keys.

**Evidence.** `internal/devicekeys/devicekeys.go:414-430` (`keychainUnavailable` substring needles); `devicekeys.go:394-396` (`loadSecret` maps to `os.ErrNotExist`); `devicekeys.go:180-204` (`EnsureSigning` mints + persists without checking the stored pubkey); `internal/state/store.go:2305-2321` (`ensureLocalEventSignature` calls `EnsureSigning` per event); `store.go:2325-2344` (mismatch guard fires too late, after the file is already written); `devicekeys.go:291-322` (`StoreWCK`/`LoadWCK` share the identical heuristic); `internal/platform/platform.go:205-213` (existing typed sentinels `ErrSecretNotFound`/`ErrUnsupported` already wrap keyring errors with `%w`, but `devicekeys` ignores them entirely).

**Recommendation.** Never mint when a key is already published, and replace substring classification with the typed sentinels `internal/platform` already provides:

```go
if storedPub != "" {
    return fmt.Errorf(
        "device signing key exists (%s) but keychain is unreachable "+
            "(session bus missing?); run from your desktop session, or set %s=1 and migrate the key",
        storedPub, platform.NoKeychainEnv,
    )
}
switch {
case errors.Is(err, platform.ErrSecretNotFound):
    return generateAndStore() // key genuinely absent
case errors.Is(err, platform.ErrUnsupported):
    return nil, fmt.Errorf("keychain unreachable, refusing to mint a divergent key: %w", err)
}
```

Add a one-time availability probe at init that records the chosen custody backend (a `key_custody` config/DB field) so later runs honor that decision instead of re-deriving it per error string — a prerequisite for the deferred `service install` daemon, whose unit will run in exactly this D-Bus-less context.

**Actionable steps.**
1. Thread `devices.signing_public_key` into `ensureLocalEventSignature`/`EnsureSigning` and refuse to mint whenever it's already set and the keychain is merely unreachable.
2. Replace `keychainUnavailable`'s substring matching with `errors.Is` against `internal/platform`'s existing `ErrSecretNotFound`/`ErrUnsupported` sentinels.
3. Apply the identical fix to `StoreWCK`/`LoadWCK` (`devicekeys.go:291-322`).
4. Record a `key_custody` decision on first successful probe and honor it on later runs.
5. Add a headless-Linux regression test simulating a dead D-Bus session on a device with an already-published signing key.

---

### P6-XP-05 — `scan` makes a serial per-repo network call (`set-head --auto`), so onboarding stalls for minutes-to-hours offline — SHIPPED 2026-07-04 (`fix/p6-xp-05`)

**Severity / Effort / Category:** P2 / M / scan · git-network · onboarding-performance · _new (no ledger item covers scan-time network use; P4-GIT-07 covers materialize, not scan)_

**Problem.** `scan.Walk` calls `opts.Git.DefaultBranch(ctx, path, "main")` per discovered repo, which resolves to `ResolveDefaultBranch` — and whenever `refs/remotes/origin/HEAD` is missing or stale, that runs `git remote set-head origin --auto`, a genuine network round-trip, serially inside the `WalkDir` callback, under the runner's default 2-minute per-command timeout with no retry, worker pool, scan-specific timeout, or offline mode. Both scan entry points hit this: the manual `devstrap scan`/`scan --adopt` command and first-run `devstrap init` (which also calls `scan.Walk`). Repos with an origin remote but no local `origin/HEAD` — `git init` + `remote add`, single-branch/CI clones, renamed remotes, all common in adopted legacy trees — each cost one serial network call; offline or on a blackholed VPN, each can hang up to 2 minutes, so 30 such repos in a tree turn onboarding's flagship, filesystem-only-looking command into an hour-long stall with zero progress output. (This would also make a future `run-loop` scan step — see P6-XP-03 — unaffordably expensive per tick once that stage exists; today `run-loop` doesn't call `scan.Walk` at all, so it isn't yet paying this specific cost.)

**Evidence.** `internal/scan/scan.go:154` (`DefaultBranch` call inside the `WalkDir` callback); `internal/git/git.go:332,345` (comment: `set-head --auto` "queries the remote"); `git.go:40,76-82` (default 2-minute per-command timeout, no retry); `internal/cli/scan.go:36` and `internal/cli/init.go:106` (both entry points construct `scan.Options` with a zero-value `Options.Git`, so both fall back to `dsgit.NewRunner()` defaults); `scan.go:156-158` (`DefaultBranch` errors already route into `Finding.Warnings` — a ready seam for a non-authoritative warning).

**Recommendation.** Keep `scan` offline: resolve the default branch from the local symbolic ref plus a stored fallback, surfacing a non-authoritative warning, and leave `set-head --auto` repair to hydrate/worktree materialization, which already resolves authoritatively at use time.

```go
opts.Git = dsgit.Runner{Timeout: 5 * time.Second} // if remote repair must stay reachable
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(8) // bounded fan-out instead of serial per-repo calls
```

**Actionable steps.**
1. Add a scan-only, local-only default-branch resolver that reads the symbolic ref/packed-refs without invoking `set-head`.
2. Surface a `DefaultBranchStored`/non-authoritative warning in `Finding.Warnings` when it falls back.
3. If remote repair must remain reachable from scan, gate it behind an explicit `--online` flag with a short (~5s) timeout and bounded concurrency (`errgroup.SetLimit(8)`).
4. Leave authoritative default-branch resolution to hydrate/worktree materialization, which already performs it correctly at use time.

---

### P6-XP-06 — The scanner hardwires the defaults-only ignore matcher, silently skipping any repo under an `env/`/`bin/`/`build/`-named path

**Severity / Effort / Category:** P3 / S / scan · ignore-compiler · discovery-blind-spot · _new (untracked; PLAT-01/04 cover the watcher/agent hardcoded lists, a different gap; spec/11 is implicitly contradicted)_

**Problem.** `scan.go` declares `var pruneMatcher = ignore.DefaultMatcher()` — a package-level, defaults-only matcher — and `scan.Walk` never calls `ignore.CompileFromDir`, even though draft bundling does honor the per-project file. The shipped defaults prune `env/`, `bin/`, `build/`, `dist/`, `out/`, `target/` at any depth, including directly under the scan root, and `WalkDir`'s prune check runs *before* the `dsgit.IsRepo` check, so a repo at `~/Code/env/...` or `~/Code/tools/bin/...` is skipped wholesale with no `Finding` and no warning distinguishing "pruned by policy" from "not a project." Unlike draft bundling, where a `!env/` negation works, there is no override at all on the scan discovery path — `scan --adopt` and first-run `devstrap init` (which also calls `scan.Walk` with the same defaults-only matcher) both silently miss these projects, even though spec/11 describes the compiler as feeding "the scanner prune predicate" from the project's `.devstrapignore` plus defaults — only the defaults half is true. Manual `devstrap add <remote> --path env/myrepo` still works as a workaround since it performs no ignore check, so this is a discovery blind spot rather than a hard block on adoption.

**Evidence.** `internal/scan/scan.go:191` (`var pruneMatcher = ignore.DefaultMatcher()`); `scan.go:111` (prune check runs before the `dsgit.IsRepo` check at `scan.go:131`); `internal/ignore/ignore.go:274-295` (default table includes `env/`, `bin/`, `build/`, `dist/`, `out/`, `target/` matched at any depth); `internal/cli/draft.go:68` (draft bundling does call `CompileFromDir`, unlike scan); `internal/cli/init.go:106` (`init --scan` shares the identical blind spot); `spec/11:253` (claims the compiler "feeds the scanner prune predicate" from the project file plus defaults).

**Recommendation.** Compile the matcher from the scan root per walk instead of the package-level default, falling back to defaults with a warning on compile failure, and count pruned directories so silent pruning becomes observable:

```go
m, err := ignore.CompileFromDir(cleanRoot, true)
if err != nil {
    result.Warnings = append(result.Warnings, fmt.Sprintf("ignore compile failed, using defaults: %v", err))
    m = ignore.DefaultMatcher()
}
// thread m through as Options.Ignore for test injection
```

**Actionable steps.**
1. Call `ignore.CompileFromDir(root, true)` in `scan.Walk`, falling back to `DefaultMatcher()` with a warning on error.
2. Add an `Options.Ignore *ignore.Matcher` field for test injection.
3. Count pruned directories and emit one summary warning ("pruned N directories via ignore policy; add negations to `~/Code/.devstrapignore` to include them").
4. Wire the same compiled matcher through `init.go:106`'s `scan.Walk` call so first-run discovery gets the fix too.

---

## Specs, Docs & Process

The gates and specs built to catch documentation staleness are themselves drifting: the shipped command-doc check still misses an undocumented command, the audit ledger that is supposed to be pass 6's single source of truth self-contradicts on what shipped, spec/00 re-drifted inside the very PR that last fixed it, and the newest crypto subsystem shipped with no granular spec owner to keep its docs honest.

### P6-DOC-01 — spec/13's status block is false in both directions and the command-doc gate silently misses `env rotate` — doc portion applied; test-hardening residual OPEN

**Severity / Effort / Category:** P2 / S / docs · ci · _new (relates `P5-DX-02`)_

**Problem.** spec/13's status block is false in both directions at once. It still lists "production R2/S3 SDK wiring" as **Planned** even though the live S3 adapter shipped in PR #24, and its Implemented inventory omits two shipped commands — `env rotate` and `hub gc`. Worse, `env rotate` (the P5-PROD-03 clear path for `secret_bindings.needs_rotation` after a device revoke) is documented nowhere in spec/13 at all. An agent trusting the status line re-plans already-shipped S3 work or never learns to route revoked-secret cleanup through `env rotate`. The gate shipped specifically to prevent this kind of drift (`P5-DX-02`) still doesn't catch it: its command check is an unanchored substring match, and the bare word "rotate" happens to match unrelated prose about log rotation, so `env rotate`'s absence passes CI green.

**Evidence.** `spec/13_CLI_DAEMON_API.md:50` (status dated `2026-06-28`, unreconciled since PRs #23/#24); `spec/13_CLI_DAEMON_API.md:54-55` (Implemented line omits `env rotate`/`hub gc`; Planned line still says "production R2/S3 SDK wiring"); `internal/cli/env.go:31,93` (`newEnvRotateCommand`, `Use: "rotate [path] [env-file]"`) and `internal/cli/hub.go:73,201` (`r2://`/`s3://` seam plus `hub gc`) both shipped, contradicting :55; `internal/cli/command_doc_test.go:33` (`if !strings.Contains(spec, name)`) matches bare `rotate` against `spec/13:464` ("rotate and retain logs") — the leaf-name substring weakness pass 5 flagged, reproduced live. Note: spec/13 lines 153, 175, 182, 184, 501, and 520 already document the R2/S3 backend as shipped, so the file is internally contradictory rather than uniformly wrong — only the :55 status line and the missing `env rotate` section are actually stale/absent.

**Recommendation.** Bump the status date, move "production R2/S3 SDK wiring" to Implemented, and add `env rotate`/`hub gc` to the :54 inventory. Add a short `env rotate` section near :279 covering what it re-encrypts and how it clears `needs_rotation`. Then close the gate gap by anchoring the command check to full paths instead of leaf names, e.g.:
```go
func collect(cmd *cobra.Command, prefix string, out *[]string) {
  for _, sub := range cmd.Commands() {
    p := strings.TrimSpace(prefix + " " + sub.Name())
    *out = append(*out, p)
    collect(sub, p, out)
  }
}
// then: if !strings.Contains(spec, name) { t.Errorf("command %q not documented", name) }
```
This alone would fail today on `"env rotate"`.

**Actionable steps.**
1. Fix spec/13's :50 date, :54 inventory, and :55 status line; add the `env rotate` doc section near :279.
2. Rewrite `command_doc_test.go` to walk the cobra tree building full command paths (`"env rotate"`, `"hub gc"`) and assert those, not bare leaf names.
3. Extend the same path-anchored check to spec/00's command inventory so both drift surfaces are covered by one gate.

---

### P6-DOC-02 — The audit ledger self-contradicts on P4-SEC-05 and leaves shipped rows (P4-SEC-02) inside "still open," violating its own convention #3 — SHIPPED 2026-07-01 (PR #28)

**Severity / Effort / Category:** P2 / S / process · docs · _new (beyond `P5-PROC-01`)_

**Problem.** `docs/audits/README.md` declares itself the single source of truth for what's left, yet it disagrees with itself on a P1 supply-chain item: the headline credits P4-SEC-05 as landed outright, while its own table still lists the same item as fully open with no partial annotation — and code confirms only the goreleaser-action SHA-pin sub-step shipped, not cosign/SLSA/SBOM. Separately, the ledger violates its own "move shipped findings out" rule (convention #3): the fully-shipped P4-SEC-02 row still sits inside the "still open" table. Per this pass's own context, PR #25 (trunk `8c739b8`) shipped P4-SEC-02 (envelope-encrypt the namespace map) and the P4-SEC-07 foundation (workspace KEK keyring) *after* the ledger was last reconciled, so both rows remain annotated-but-uncleaned inside "still open" — P4-SEC-02 in clear violation since it's fully shipped, P4-SEC-07 more defensibly since only its foundation landed. A reader trusting the headline skips unfinished release-signing work; a reader trusting the table re-plans work this very PR already shipped.

**Evidence.** `docs/audits/README.md:29` ("landed 32 of the 36 Pass-5 findings ... plus P4-SEC-05 and P4-QUAL-07 (partial)", P4-SEC-05 unqualified); `docs/audits/README.md:78` (P4-SEC-05 row listed under "still open" with no partial annotation); `.github/workflows/release.yml:27` (`goreleaser-action@f06c13b6... # v7.2.3`, SHA-pin only) and `.goreleaser.yaml` (only a `checksum:` stanza, no `signs:`/`sboms:`); `spec/18_WORK_LOG.md:93` ("`P4-SEC-05` SHA-pin `goreleaser-action`" — matches code, contradicts :29); `docs/audits/README.md:76` (P4-SEC-02 row, annotated "shipped 2026-06-30," still under the `:72 Pass 4 — still open` header, violating convention `:21` #3); `docs/audits/README.md:14` (index row "~19 shipped (PR #20), ~25 open," stale after PRs #23-#25).

**Recommendation.** Rewrite the P4-SEC-05 row as partial and drop the bare credit from the headline, e.g.: `| P4-SEC-05 | P1 | Sign release binaries — **partial 2026-06-30**: goreleaser-action SHA-pinned (release.yml); cosign keyless signing + SLSA provenance + SBOM still open (fold into P4-QUAL-05) |`. Move the fully-shipped P4-SEC-02 row out of "still open" into a dated shipped subsection; trim P4-SEC-07's row to only its open remainder (full workspace-ID pairing) rather than removing it wholesale, since only its foundation shipped. Refresh the `:14` index counts and add a pass-6 row. While reconciling, promote the work-log-rotation follow-up (`spec/18_WORK_LOG.md` is now 1,212 lines) from a convention bullet into an actual tracked backlog row.

**Actionable steps.**
1. Rewrite README.md:29's headline and :78's row so P4-SEC-05 reads "partial," scoped to the SHA-pin only.
2. Move README.md:76 (P4-SEC-02) into a dated "shipped" section; trim :79 (P4-SEC-07) to its open remainder only.
3. Update the `:14` index counts/pass-6 row, and add a tracked row for work-log rotation.

---

### P6-DOC-03 — spec/00 re-drifted immediately after P5-DOC-02: a "planned" sync comment, a missing command, and a wrong tested-package inventory — SHIPPED 2026-07-01 (PR #28)

**Severity / Effort / Category:** P3 / S / docs · entry-point · _new (recurrence after `P5-DOC-02`)_

**Problem.** spec/00 — the file CLAUDE.md/AGENTS.md force every human and agent to read first — re-drifted almost immediately after `P5-DOC-02` fixed it. Its "What to build first" code block still calls `devstrap sync` a "today: namespace-map reconcile + --hub-file spike" with eager-clone materialization merely "planned," directly contradicting its own "Current position" note and "now built" list ~100 lines earlier in the same file, and contradicting the shipped code. The command inventory omits `devices recipient`, which was added by the same PR #25 that touched this file. The tested-package list names 10 packages but 19 internal packages actually carry `*_test.go` files, omitting `childenv`, `devicekeys`, `draftbundle`, `envbundle`, `envfile`, `hub`, `ignore`, `platform`, and `workspacekeys`. And the per-pass audit blockquote chain silently stops at pass 4, hiding the fifth pass's 36 findings from the mandatory entry point.

**Evidence.** `spec/00_START_HERE.md:200-202` ("today: namespace-map reconcile + --hub-file spike ... planned (EAGER-*/HUB-*)") vs `spec/00_START_HERE.md:102` ("Current position") and `:169-171` ("now built: eager-clone materialization (EAGER-*)") in the same file; `internal/cli/sync.go:134-139` ("sync always materializes with a blobless/partial clone (EAGER-01)") confirming the code side; `spec/00_START_HERE.md:131` (`devices enroll/list/approve/revoke/lost/rename`, omitting `recipient`) vs `internal/cli/devices.go:237-249` (`devices recipient`, added by PR #25/`8c739b8`); `spec/00_START_HERE.md:154` (names 10 tested packages) vs `ls internal/*/*_test.go` (19 packages); zero hits for `PASS5|fifth|2026-06-29` in spec/00 despite blockquotes for passes 2-4. `spec/00_START_HERE.md:148` ("reports that hydration/fetch reconciliation remains future work") is a fourth stale statement in the same vein and can be fixed in the same edit.

**Recommendation.** Replace the stale sync comment with a shipped-status one, e.g.:
```bash
devstrap sync   # shipped: pushes/pulls signed, envelope-encrypted events (--hub-file or hub: r2://<bucket>),
                # then eagerly blobless-clones every repo, extracts draft blobs, and hydrates env (EAGER-01/02)
```
Add `recipient` to the `:131` devices list; revisit `:148`'s reconciliation line in the same pass. Regenerate the `:154` test-package list from `ls internal/*/*_test.go`, or replace it with a self-updating claim like "every internal package except `internal/id` has focused tests" (re-verify the exception at fix time rather than assuming it holds). Replace the three per-pass blockquotes with one durable pointer — "Audit history and the open backlog live in `docs/audits/README.md`" — so spec/00 never needs a new blockquote per pass again.

**Actionable steps.**
1. Fix the `:200-202` sync comment and the `:148` reconciliation line together in one edit.
2. Add `devices recipient` to the `:131` command inventory.
3. Regenerate or generalize the `:154` tested-package list so it can't rot per package.
4. Replace the pass-specific blockquotes with a single pointer to `docs/audits/README.md`.

---

### P6-DOC-04 — The new `workspacekeys` keyring has no granular spec owner, so its lifecycle/forward-secrecy docs can rot without tripping the drift gate — SHIPPED 2026-07-03 (PR #71)

**Severity / Effort / Category:** P3 / S / process · ci · spec-mapping · _new (gap `P5-DX-02` can't close)_

**Problem.** `internal/workspacekeys` (the new WCK epoch keyring: `EnsureBootstrap`/`GrantAllEpochs`/`Rotate`/`IngestGrant`, 329 lines) has no granular spec owner even though spec/07, spec/09, and spec/15 all now carry normative prose describing its behavior — spec/07's full init/approve/revoke/pull WCK lifecycle section, spec/09's HybridStore WCK-custody line, and spec/15's envelope-encryption/forward-secrecy paragraphs. All three specs' `tracks_code` frontmatter matches the package only through the broad `internal/**` catch-all globs in spec/00/01/02/03/04/14/16, never through their own granular lists. Because the spec-drift gate passes as soon as *any* mapped spec is touched, a future PR that changes rotation semantics (e.g., rotate-on-`lost` vs rotate-on-`revoke`, or the grant-event shape) can satisfy CI by editing spec/00's broad inventory alone, leaving spec/07's lifecycle section, spec/09's custody claim, and spec/15's forward-secrecy statement to silently go stale — the same mechanism that produced `P5-DOC-01`, now aimed at the newest security-critical subsystem one PR after it was born.

**Evidence.** `spec/07_NAMESPACE_AND_SYNC_MODEL.md:3` (`tracks_code: [internal/pathkey/**, internal/scan/**, internal/state/**, internal/sync/**]`, no workspacekeys) alongside its PR #25 WCK lifecycle section; `spec/09_SECRETS_AND_ENVIRONMENT.md:3` (tracks `internal/devicekeys/**` but not workspacekeys) alongside `:197` ("HybridStore also custodies per-epoch Workspace Content Keys (WCK) ... keyed `wck.<workspace_id>.<epoch>`"); `spec/15_SECURITY_THREAT_MODEL.md:3` (tracks several packages including `devicekeys`/`sync` but not workspacekeys) alongside its "Event-log envelope encryption" paragraphs; `internal/specdrift/specdrift.go:72-83` (`anyChanged(mapped, changedSet)` — gate passes if any one mapped spec was touched), so workspacekeys changes satisfy the gate through catch-all globs alone.

**Recommendation.** Add `internal/workspacekeys/**` to the `tracks_code` frontmatter of all three specs:
```yaml
# spec/07
tracks_code: [internal/pathkey/**, internal/scan/**, internal/state/**, internal/sync/**, internal/workspacekeys/**]
# spec/09: append internal/workspacekeys/** after internal/devicekeys/**
# spec/15: append internal/workspacekeys/** after internal/devicekeys/**
```
Consider also adding `internal/devicekeys/**` to spec/07 if the WCK-custody sentence stays there. Adopt the rule that any new internal package must land in at least one non-catch-all spec's `tracks_code` in the same PR, enforced by a small test that diffs `ls internal/` against the union of non-catch-all globs.

**Actionable steps.**
1. Add `internal/workspacekeys/**` to spec/07, spec/09, and spec/15 frontmatter.
2. Add `internal/devicekeys/**` to spec/07 if it keeps the shared WCK-custody sentence.
3. Add a test that fails when a new `internal/*` package maps only to catch-all specs, so future packages can't repeat this gap.

---

## Best-practice research notes

Six external best-practice topics were consulted to anchor this pass's recommendations, each cross-checked against DevStrap's actual shipped code rather than applied generically; every topic maps to specific findings above.

### Local-first sync & replicated logs

Compaction, cursors, tombstone GC, and HLC handling were researched across Automerge, ElectricSQL, Replicache, Ditto, and Convex's sync writeup. The common thread: none of these systems trust a single cursor scheme or a wall-clock GC trigger — they separate causal order from delivery order and gate destructive cleanup on provable delivery, which is exactly where DevStrap's new envelope layer reintroduces gaps.

- **Frontier-hash snapshot keys** — key a hub snapshot by the SHA-256 of the event frontier it covers so concurrent compactors never clobber uncompacted events and need no coordination lock ([source](https://automerge.org/docs/reference/under-the-hood/storage/))
- **Cursor on ingestion order, not HLC** — Convex subscribes on server-assigned `_creationTime`, not client causal time, because an offline-authored event uploaded late would fall behind an already-advanced HLC cursor and be skipped forever ([source](https://stack.convex.dev/automerge-and-convex))
- **Compaction preserves cursors** — fold only repeated updates to the same key, never creates/tombstones, and trigger by a dirty-ratio (not a fixed schedule), so no client cursor is ever invalidated ([source](https://github.com/electric-sql/electric/pull/2231))
- **Tombstone GC gated on a stability frontier** — delete only below the minimum acked cursor across all *approved* replicas, not by TTL alone, or a device reconnecting after the tombstone TTL resurrects deleted state fleet-wide ([source](https://docs.ditto.live/sdk/latest/crud/delete))
- **Formal 410-snapshot reset protocol** — treat "cursor too old" as an explicit clear-and-rebuild op, with the snapshot and its cursor published atomically so no client can observe one without the other ([source](https://doc.replicache.dev/reference/server-pull))
- **Anti-entropy via frontier-hash diff, not per-peer buffers** — Ditto rejected delta buffers because no peer can truncate its buffer until all peers ack a stable prefix, which is the unbounded-log problem in disguise; a cheap frontier-hash compare needs no buffer and no known peer set ([source](https://www.ditto.com/blog/an-inside-look-at-dittos-delta-state-crdts))
- **Monitor HLC drift as a runtime property** — expose observed clock skew rather than only rejecting outliers, since 10–250ms drift between cloud VMs is routine ([source](https://cse.buffalo.edu/tech-reports/2014-04.pdf))

→ Anchors: P6-HUB-04, P6-SYNC-02, P6-SYNC-01 (and reinforces the still-open P4-SYNC-02/P4-HUB-11 compaction workstream and P5-SYNC-01's cursor redesign).

### End-to-end multi-device encryption

Primary sources on 1Password's Secret Key model, Keybase's per-user-key generations, Sender Keys post-compromise security, Tailnet Lock, Filippo Valsorda on age, and GCP KMS rotation were mapped against DevStrap's shipped PR #25 WCK epoch keyring. The design already tracks Keybase's rotate-on-revoke pattern closely, but the literature exposes that an *unauthenticated* transport (age to the hub) demands a hard rule the shipped code doesn't yet enforce: no decrypted or ingested secret may be trusted before it is signature-bound.

- **Age is not sender-authenticated** — any party, including the hub, can replace an age ciphertext with a freshly valid one, so every decrypted artifact must be bound to a signature from a trusted key before use ([source](https://words.filippo.io/age-authentication/))
- **Rotate-on-removal alone gives no post-compromise security** — a passive, undetected compromise persists indefinitely without periodic/on-demand epoch rotation, not just revoke-triggered rotation ([source](https://eprint.iacr.org/2023/1385.pdf))
- **Signed, strictly-sequential key generations + prev-boxes** — advertise each key generation in a signed chain so clients detect suppressed/rolled-back rotations, and encrypt the previous generation under the new key so one grant covers all history ([source](https://book.keybase.io/docs/teams/puk))
- **A locally-generated recovery secret the server never sees** — without an Emergency-Kit-style recovery key enrolled as a permanent recipient, losing every device loses the workspace irrecoverably ([source](https://1passwordstatic.com/files/security/1password-white-paper.pdf))
- **Trust-authority changes must themselves be signed by an already-trusted key** — a zero-knowledge server must never be able to unilaterally add a trusted signer, and a retroactive-revocation recovery path must exist for a compromised signing key ([source](https://tailscale.com/docs/concepts/tailnet-lock-whitepaper))
- **Lazy, off-critical-path re-wrap on rotation** — old key versions stay decrypt-only, new encryption uses the new version immediately, and re-encryption of existing ciphertext happens as a background queue, never synchronously on revoke ([source](https://docs.cloud.google.com/kms/docs/cmek-rotation))
- **New devices are provisioned by an existing device over a mutually-authenticated channel** — the server is never trusted to introduce a device key; provisioning needs an out-of-band fingerprint/short-phrase confirmation ([source](https://book.keybase.io/docs/crypto/key-exchange))

→ Anchors: P6-SEC-01, P6-SEC-02, P6-SEC-03, P6-SYNC-04 (and the still-open `P4-SEC-04`/full `P4-SEC-07` device-pairing item).

### CLI design & Cobra

clig.dev, clispec.dev, Cobra's own how-to guides, and production-Go-CLI writeups were audited against `internal/cli`. DevStrap already gets exit-code discipline and a single render seam right; the gaps cluster around the machine-output contract not being honored everywhere it's advertised, and around discoverability of the CLI's own layered config.

- **Structured JSON error envelope on failure** — emit `{"error":{"kind","message","hint"}}` as the last stderr line in machine mode with a stable, finite `kind` set, so scripts branch on error class instead of parsing prose ([source](https://clispec.dev/))
- **Treat `--json` as a versioned contract** — add a `schema_version` field and explicit stability rules (additive-only patches) so agents can pin against the shape ([source](https://cli.bullpen.fi/reference/json-contract/))
- **Data to stdout, messaging to stderr** — piping stdout must yield exactly the data; progress/warning lines never belong on the pipeable stream ([source](https://clig.dev/))
- **Gate ANSI/color/clear-screen on TTY + `NO_COLOR`** — animated or screen-control output must fall back to plain sequential output when non-interactive ([source](https://no-color.org/))
- **Group subcommands with Cobra's `AddGroup`/`GroupID`** — once past ~10 commands, flat help becomes an undifferentiated wall; kubectl/gh-style grouping keeps `--help` readable ([source](https://cobra.dev/docs/how-to-guides/working-with-commands/))
- **Enforce CLI-guideline compliance with an automated test, not review discipline** — a table-driven test walking the Cobra tree should assert Long/Example text, exit-code documentation, and machine-output honoring per command ([source](https://dev.to/bala_paranj_059d338e44e7e/applying-cligdev-to-a-go-cli-with-an-automated-compliance-test-1oa2))
- **Make layered config precedence discoverable** — ship a `config show` that prints each resolved key with its source (flag/env/file/default) so `DEVSTRAP_*` env vars aren't undocumented tribal knowledge ([source](https://novvista.com/building-production-ready-cli-tools-with-go-beyond-the-tutorial/))

→ Anchors: P6-CLI-03, P6-CLI-04, P6-CLI-05, and the "enforce via test, not prose" theme behind P6-QUAL-01.

### Object storage as an event log / blob store

OSWALD, SlateDB, WarpStream, turbopuffer, and the AWS/R2 docs were researched against the now-live `internal/hub/r2.go`. DevStrap's core choices (immutable per-event keys, content-addressed blobs, `If-None-Match` idempotency) are validated, but the literature is unanimous that a log on object storage needs an authoritative, discoverable manifest — which is exactly the piece DevStrap's shipped GC and retention floor are still missing.

- **A single CAS-updated manifest recording the GC watermark** — writers must re-check the watermark after append or a GC that advanced past a lagging writer silently loses the write ([source](https://nvartolomei.com/oswald/))
- **Compact many small per-record objects into segments** — per-object PUTs/GETs are the dominant cost; every mature system batches records into larger objects in the background ([source](https://docs.warpstream.com/warpstream/overview/architecture))
- **`GET If-None-Match` on a small head object for cheap polling** — a 304 avoids the ~12x-more-expensive LIST call that periodic pollers would otherwise pay every tick ([source](https://www.bitsxpages.com/p/protocols-for-transactional-usage))
- **R2's `If-Match` CAS extension** — richer than plain S3 PutObject conditions, but requires injecting the header via SDK middleware since `aws-sdk-go-v2`'s `PutObjectInput` doesn't expose it ([source](https://developers.cloudflare.com/r2/api/s3/extensions/))
- **Epoch-numbered lock objects via `PUT If-None-Match`** — coordinate exclusive maintenance (compaction/GC) with no external lock service; the epoch in the key doubles as a fencing token ([source](https://www.morling.dev/blog/leader-election-with-s3-conditional-writes/))
- **Mark-and-sweep needs an age-based grace window** — a blob uploaded after the mark phase is invisible to the marker, so sweeps must skip objects younger than a grace TTL ([source](https://nvartolomei.com/oswald/))
- **A 412 on a shared log position means "re-list and retry," not accept-or-reject** — this is the whole conflict-resolution protocol for a shared snapshot/manifest key ([source](https://www.bitsxpages.com/p/protocols-for-transactional-usage))

→ Anchors: P6-HUB-01, P6-HUB-03, P6-HUB-04.

### Go engineering & supply chain

2025–2026 practice across SQLite pooling, release signing, property-based testing, linting, and CI supply-chain hygiene was diffed against DevStrap's actual state. DevStrap is already strong on WAL/pragma discipline and SHA-pinned Actions; the gaps are a single-writer pool that also serializes reads, unsigned release artifacts, and CI gates that look like coverage but don't run.

- **Split reads into a separate pool from the single writer** — `MaxOpenConns(1)` is correct for writes but unnecessarily serializes all reads behind it ([source](https://github.com/hollis-labs/go-sqlite))
- **Sign release checksums and publish SLSA build provenance** — goreleaser's checksums.txt is unverifiable without a signing/attestation step over the built artifacts ([source](https://goreleaser.com/blog/slsa-generation-for-your-artifacts/))
- **Property-based state-machine testing bridged into the native fuzzer** — `pgregory.net/rapid` plus `rapid.MakeFuzz` covers ordered/replicated invariants (HLC monotonicity, replay convergence) that `testing.F` alone can't express ([source](https://github.com/flyingmutant/rapid))
- **Adopt v2-era golangci-lint linters for slog/context-heavy code** — `sloglint`, `noctx`, `modernize`, `copyloopvar`, `usetesting`, `nilnesserr` map directly onto DevStrap's structured-logging and subprocess-heavy codebase ([source](https://golangci-lint.run/docs/product/migration-guide/))
- **Enforce (not just practice) SHA-pinned Actions + least-privilege tokens** — the 2025 tj-actions compromise retargeted 350+ tags but left SHA-pinned workflows untouched; add Scorecard's pinned-dependencies/token-permissions checks to catch regressions ([source](https://www.wiz.io/blog/github-action-tj-actions-changed-files-supply-chain-attack-cve-2025-30066))
- **Continuous WAL-shipping complements point-in-time backup** — a Litestream-style sidecar gives a ~1s data-loss window versus losing everything since the last manual backup ([source](https://litestream.io/how-it-works/))
- **OTel CLI semantic conventions + flush-before-exit** — a short-lived CLI process must use `SimpleSpanProcessor` or force a flush before exit, or the default batch processor silently drops all telemetry ([source](https://opentelemetry.io/docs/specs/semconv/cli/cli-spans/))

→ Anchors: P6-QUAL-02, P6-QUAL-03, P6-DATA-04 (and sharpens the still-open `P4-SYNC-07` single-pool gap).

### Git automation at scale

GitHub's engineering blog (partial-clone data study, the Scalar story) and git-scm's own docs for maintenance, backfill, partial-clone, worktree, and credential handling were grounded against `internal/git`. DevStrap's core choices — blobless clone as the long-lived default, `GIT_TERMINAL_PROMPT=0`, explicit origin-ref fetches for agent bases — are validated by the data; the gaps are in timeout policy, LFS/worktree lifecycle, and offline-prep tooling built on top of that foundation.

- **Blobless clone is the right long-lived default; shallow/treeless should only be used for throwaway CI** — GitHub's data-driven study shows fetches into treeless/shallow clones are drastically more expensive for both client and server ([source](https://github.blog/open-source/git/git-clone-a-data-driven-study-on-cloning-behaviors/))
- **`git backfill` batches historical-blob downloads by path** — avoids the one-object-per-request fetch storm that on-demand fetching in a blobless clone triggers on `git log -p`/`blame` ([source](https://git-scm.com/docs/git-backfill))
- **Partial clones require the promisor remote to be reachable** — tooling that eagerly blobless-clones everything should surface the degraded-offline mode explicitly rather than let git fail opaquely ([source](https://git-scm.com/docs/partial-clone))
- **Incremental background maintenance** (`maintenance.strategy=incremental`) — hourly prefetch into `refs/prefetch/*` plus commit-graph replaces expensive monolithic gc with small, non-destructive tasks ([source](https://git-scm.com/docs/git-maintenance))
- **Scalar's config bundle** — blobless partial clone + sparse-checkout + background maintenance + commit-graph-on-fetch/multi-pack-index is the measured-fast config set for large-repo git, shippable as plain config without depending on the `scalar` binary ([source](https://github.blog/open-source/git/the-story-of-scalar/))
- **Use git's own worktree lifecycle primitives** — `git worktree list --porcelain` prunable annotations, `git worktree prune`, and critically `git worktree repair` after a parent-repo path move, which otherwise silently breaks every linked worktree ([source](https://git-scm.com/docs/git-worktree.html))
- **Non-interactive git must fail fast, and credentials flow through the credential-helper protocol** — `GIT_TERMINAL_PROMPT=0` plus `git credential reject` on auth failure evicts a stale cached token instead of looping forever ([source](https://git-scm.com/docs/git-credential))

→ Anchors: P6-GIT-01, P6-GIT-04, P6-GIT-05.

---

## Appendix A — P6 findings mapped to the open P4/P5 backlog

This pass's findings were checked against the still-open Pass-4/Pass-5 backlog tracked in `docs/audits/README.md`; a majority sharpen, break, or half-finish an already-tracked item rather than introducing something wholly unrelated to prior passes.

| P6 finding | Relationship | Open backlog item(s) |
|---|---|---|
| P6-SEC-01 | deepens | P4-SEC-04 (fail-closed enrollment), P4-SEC-07 (envelope encryption / key rotation) |
| P6-SEC-02 | deepens | P4-SEC-07 (full workspace-ID pairing across devices) |
| P6-SEC-03 | deepens | P4-SEC-07 (full workspace-ID pairing across devices) |
| P6-SYNC-01 | deepens | P4-SEC-04 (fail-closed enrollment verification) |
| P6-SYNC-02 | duplicate-adjacent to | P5-SYNC-01 (transport-cursor redesign) |
| P6-SYNC-04 | deepens | P4-SYNC-05 (folded hash chain / signed head), P4-SYNC-08 (workspace-id binding) |
| P6-HUB-01 | defect in shipped | P5-HUB-02 (hub-side GC + draft-snapshot pruning) |
| P6-HUB-02 | defect in shipped | P5-HUB-01 (live S3/R2 adapter) |
| P6-HUB-03 | unshipped half of | P5-HUB-04 (bounded-concurrency pull fetch — push side never got it) |
| P6-HUB-04 | unshipped half of | P5-HUB-03 (retention floor / `ErrSnapshotRequired` guard) |
| P6-GIT-01 | defect in shipped | EAGER-* (eager-clone materialization promise) |
| P6-GIT-04 | unshipped half of | pass-1 `GIT-1` (LFS policy honored on worktrees only, never materialize/hydrate) |
| P6-CLI-03 | defect in shipped | pass-2 `CLI-04` (exit-code contract, ledger/spec claims it shipped) |
| P6-DATA-01 | defect in shipped | P5-QUAL-01 (materialize's "no draft bundle" handling) |
| P6-DATA-02 | defect in shipped | P5-PROD-03 (`devstrap env rotate` + `needs_rotation` clear path) |
| P6-DATA-04 | defect in shipped | P5-SEC-04 (env blobs made local-only, which created this backup gap) |
| P6-QUAL-01 | deepens | P5-DX-02 (spec-drift content/inventory staleness detection) |
| P6-QUAL-03 | defect in shipped | P5-HUB-01 (live S3/R2 adapter — its only real-backend test never runs in CI) |
| P6-QUAL-04 | defect in shipped | P5-CLI-04 (`ssh -G` host-alias resolution) |
| P6-XP-03 | defect in shipped | XP-* (portable `run-loop` convergence loop) |

_(Historical note, resolved: the P4-SEC-02 / P4-SEC-07-foundation status mismatch this appendix originally flagged was reconciled in the ledger the same week — `docs/audits/README.md` is the single source of finding status; this file is the per-finding evidence snapshot.)_
