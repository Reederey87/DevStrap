# DevStrap — Design & Implementation Audit (Fourth Pass)

**Date:** 2026-06-28
**Auditor:** Automated multi-agent design & implementation review (6 dimension auditors → adversarial grounding against the live tree → sectioned synthesis), with external best-practice research via Exa.
**Scope:** `spec/` design corpus + Go codebase (`cmd/`, `internal/`) at the **post‑PR‑#16** state (`main` after the cloud-sync workstreams `EAGER-*`/`DRAFT-*`/`HUB-*`/`XP-*` landed, plus the go.mod hygiene hotfix). Focus: what is still weak, risky, or missing now that the "Dropbox experience for code" cloud-sync loop is actually wired end to end.

## How this relates to the prior audits

Three prior audits are now **largely implemented**, and this pass deliberately does **not** rehash them:

- **First pass** (`AUDIT_RECOMMENDATIONS.md`, 58 findings) — foundational hardening (git-injection RCE, env sanitizer, redaction layer, stale-base re-check, HLC skew guard).
- **Second pass** (`AUDIT_RECOMMENDATIONS_2026-06-27.md`) — cross-machine working-state, non-VCS/remote-less support, forge-agnostic PR/MR, the zero-knowledge hub architecture.
- **Third pass** (`AUDIT_RECOMMENDATIONS_2026-06-28.md`) — the cloud-sync direction as ID-stamped workstreams. **Shipped in PR #16:** the pluggable `Hub` interface + Cloudflare R2/S3 backend (`HUB-01..08`), eager-clone materialization (`EAGER-01..04`), encrypted draft bundles + `.devstrapignore` compiler (`DRAFT-01..05`), and the portable cross-platform run loop + e2e testscripts (`XP-01..04`).

This **fourth pass** audits the system *as it now exists* — the freshly-landed hub, sync engine, materialization, and crypto — and identifies the next layer of design improvements and high-value features. It **extends** the prior audits; it does not revert them. To avoid colliding with the already-shipped `HUB-01..08`, the cloud-hub findings below continue the series from `HUB-09`.

> **Method:** each finding was produced by a dimension auditor that read the cited specs and code and researched external best practices, then independently re-checked by an adversarial verifier against the live tree (anything already implemented or unsupported by evidence was dropped). Every retained finding carries `file:line`/spec evidence, a concrete recommendation, actionable steps, an example, and an external reference.

> **Out of scope (unchanged from prior audits):** which LLM / Claude API the agent runner calls is a separate concern and appears in no spec or audit.

---
## Table of contents

- [Executive summary](#executive-summary)
- [Findings at a glance](#findings-at-a-glance)
- [Prioritized roadmap](#prioritized-roadmap)
- [Quick wins](#quick-wins)
- [Strategic bets](#strategic-bets)
- **Section 1 — Security & Cryptography** (`SEC-01..08`)
- **Section 2 — Sync Engine, Conflicts & Data Model** (`SYNC-01..08`)
- **Section 3 — Cloud Hub, Backend & Scalability** (`HUB-09..16`)
- **Section 4 — Git Materialization, Worktrees & Agents** (`GIT-01..07`)
- **Section 5 — Code Quality, Concurrency, Reliability & Testing** (`QUAL-01..07`)
- **Section 6 — Product, UX, DevEx & New Features** (`PROD-*`)
- [Appendix A — Methodology](#appendix-a--methodology)
- [Appendix B — Relationship to the shipped HUB-01..08 workstreams](#appendix-b--relationship-to-the-shipped-hub-0108-workstreams)
- [Appendix C — Suggested sequencing](#appendix-c--suggested-sequencing)

---
## Executive summary

DevStrap has crossed a real maturity threshold: the Phase 0 local CLI and the Phase 3 fresh-worktree agent loop are shipped and tested, and PR #16 has now landed the cloud-sync workstreams — EAGER eager-clone materialization, DRAFT age-encrypted bundles, the HUB two-plane zero-knowledge design, and XP cross-platform hardening — moving the product from a local tool toward a true Workspace Passport. The catch is that the R2 hub backend is wired but not yet active, so the highest-risk surface in the whole system is also the least exercised, and several of its core guarantees are still aspirational rather than enforced. Remaining risk and opportunity concentrate in five themes. First, **zero-knowledge gaps in the just-landed hub**: revocation re-encryption is a no-op, the namespace map ships to R2 in plaintext, blobs are never hash-verified, and the bootstrap window trusts unenrolled peers (SEC-01, SEC-02, SEC-03, SEC-04). Second, **unbounded-growth and compaction debt** spanning the local sync log and the hub, where snapshots are dead-ended and nothing can ever be deleted (SYNC-02, HUB-11, HUB-12, SYNC-06). Third, **materialization and agent-isolation edge cases** — empty-checkout false "clean" states, a fatal clone-retry bug, and an agent runner with no OS-enforced sandbox (GIT-01, GIT-02, GIT-03). Fourth, **reliability and test scaffolding that is still thin** — no fuzzing or aggregate decompression cap, only example-tested CRDT convergence, and a `materialize` that exits 0 on failure (QUAL-01, QUAL-02, QUAL-03). Fifth, **a thin product surface** with no daemon, TUI, one-command onboarding, or distribution channel yet (PROD-01 through PROD-06). The near-term imperative is unambiguous: harden the hub's crypto guarantees and bound sync-log growth **before** the R2 backend goes live, then invest in the product surface that turns a working engine into something people adopt.

## Findings at a glance

| Dimension | P1 | P2 | P3 | Total |
|---|---|---|---|---|
| Security & Cryptography | 5 | 3 | 0 | 8 |
| Sync Engine, Conflicts & Data Model | 2 | 4 | 2 | 8 |
| Cloud Hub, Backend & Scalability | 3 | 5 | 0 | 8 |
| Git Materialization, Worktrees & Agents | 2 | 4 | 1 | 7 |
| Code Quality, Concurrency, Reliability & Testing | 3 | 3 | 1 | 7 |
| Product, UX, DevEx & New Features | 2 | 4 | 0 | 6 |
| **Total** | **17** | **23** | **4** | **44** |

## Prioritized roadmap

Ordered top-to-bottom by execution priority (severity P1 → P2 → P3, then dimension). The **Sev** column carries the finding's severity; **Effort** legend: S ≈ <½ day, M ≈ 1–3 days, L ≈ ~1 week, XL ≈ multi-week.

| # | Sev | ID | Recommendation | Dimension | Effort |
|---|---|---|---|---|---|
| 1 | P1 | SEC-01 | Make device revocation actually delete/re-encrypt blobs so a revoked key loses access | Security | M |
| 2 | P1 | SEC-02 | Encrypt namespace-map events at rest; stop leaking paths, remotes, and device timelines to R2 | Security | L |
| 3 | P1 | SEC-03 | Verify content-addressed blobs against their hash on fetch and sender-authenticate multi-recipient age | Security | M |
| 4 | P1 | SEC-04 | Close the bootstrap window so an unenrolled/malicious hub can't inject attacker-remote project events | Security | M |
| 5 | P1 | SEC-05 | Sign release binaries with provenance/SBOM and pin the release action to an immutable SHA | Security | M |
| 6 | P1 | SYNC-01 | Stop the pull cursor from silently skipping quarantined or dependency-broken events | Sync | M |
| 7 | P1 | SYNC-02 | Implement event-log compaction and snapshot exchange so the events table stops growing forever | Sync | L |
| 8 | P1 | HUB-10 | Add retry, backoff, and error classification to the R2/S3 backend | Hub | M |
| 9 | P1 | HUB-11 | Add event-log compaction / working-snapshot exchange for R2 to bound Pull cost and memory | Hub | L |
| 10 | P1 | HUB-12 | Give the hub a Delete path so blob/event GC becomes possible | Hub | L |
| 11 | P1 | GIT-02 | Fix clone network-retry to use a clean destination instead of failing fatally | Git | S |
| 12 | P1 | GIT-03 | Give the agent runner a real OS-enforced sandbox instead of argv substring matching | Git | XL |
| 13 | P1 | QUAL-01 | Add fuzz targets for untrusted parsers and an aggregate decompression cap on draft bundles | Quality | M |
| 14 | P1 | QUAL-02 | Property/model-check HLC monotonicity and conflict-resolution convergence | Quality | M |
| 15 | P1 | QUAL-03 | Make `devstrap materialize` return non-zero when any project fails | Quality | S |
| 16 | P1 | PROD-01 | Add a `devstrap clone <url>` one-shot quick path that collapses onboarding to one command | Product | M |
| 17 | P1 | PROD-02 | Upgrade `doctor` to a severity-graded report with `--fix`, `--json`, and non-zero exit on errors | Product | M |
| 18 | P2 | SEC-06 | Extend value-shape redaction to Snowflake, GitLab, Stripe, GCP service-account, and Bearer secrets | Security | S |
| 19 | P2 | SEC-07 | Add device-key rotation, forward secrecy, and a KEK/envelope layer | Security | L |
| 20 | P2 | SEC-08 | Implement hosted-mode prefix-scoped/temporary credentials and object immutability | Security | L |
| 21 | P2 | SYNC-03 | Re-enable past-direction quarantine and stop near-epoch HLCs from stealing path ownership | Sync | S |
| 22 | P2 | SYNC-04 | Give push a cursor so sync stops re-uploading the entire local event log every run | Sync | M |
| 23 | P2 | SYNC-05 | Replace the pointer hash chain with a folded running hash plus a signed per-device head | Sync | L |
| 24 | P2 | SYNC-06 | Wire up tombstone GC and per-peer cursor/delivery tables and enforce the GC-safety invariant | Sync | M |
| 25 | P2 | HUB-09 | Drop the redundant ObjectExists-before-conditional-Put to cut request cost and close the TOCTOU | Hub | S |
| 26 | P2 | HUB-13 | Fix the HLC-only cursor so a new event sharing an HLC value isn't dropped | Hub | M |
| 27 | P2 | HUB-14 | Emit metrics/traces and op/byte counters on the hub data path to see the Class A/B cost driver | Hub | M |
| 28 | P2 | HUB-15 | Add cost controls, quotas, and rate limiting so a runaway loop can't blow the R2 bill | Hub | M |
| 29 | P2 | HUB-16 | Defend at-rest availability/integrity: versioning/Object-Lock, backup/replication, delete-detection | Hub | M |
| 30 | P2 | GIT-01 | Verify a non-empty checkout before recording a project available/clean | Git | S |
| 31 | P2 | GIT-04 | Implement agent worktree cleanup and time-based GC that can reap squash/rebase-merged worktrees | Git | M |
| 32 | P2 | GIT-05 | Support self-hosted GitLab/Gitea forge detection with overrides and `doctor` glab/tea probes | Git | M |
| 33 | P2 | GIT-06 | Handle submodules and add prefetch/maintenance to avoid blobless lazy-fetch storms | Git | M |
| 34 | P2 | QUAL-04 | Enforce the coverage profile in CI and add a Windows build to honor the cross-platform mandate | Quality | M |
| 35 | P2 | QUAL-05 | Sign release artifacts and add SBOM/build provenance beyond checksums.txt | Quality | M |
| 36 | P2 | QUAL-06 | Add jitter and an aggregate deadline budget to network retries | Quality | S |
| 37 | P2 | PROD-03 | Make `init` a guided first-run with scan-on-init and an always-printed next command | Product | S |
| 38 | P2 | PROD-04 | Ship `devstrap service install` to generate a LaunchAgent/systemd unit wrapping the run-loop | Product | M |
| 39 | P2 | PROD-05 | Close the distribution gap: Homebrew tap, curl\|sh installer, and shell completions | Product | S |
| 40 | P2 | PROD-06 | Give detect-don't-merge a resolution surface via `devstrap conflicts resolve` | Product | M |
| 41 | P3 | SYNC-07 | Stop MaxOpenConns(1) from serializing all WAL reads behind the single writer | Sync | M |
| 42 | P3 | SYNC-08 | Unblock the multi-workspace future and add a signed binding to re-stamped workspace ids | Sync | M |
| 43 | P3 | GIT-07 | Persist per-project materialize failure records with resume and progress detail | Git | M |
| 44 | P3 | QUAL-07 | Enable the resource/context-leak linters in golangci-lint | Quality | S |

## Quick wins

- **GIT-02** — One-line clean-destination fix turns transient mid-clone failures from fatal to recoverable; pure reliability upside.
- **QUAL-03** — Trivial exit-code fix that immediately unblocks CI gating and automation around `materialize`.
- **SEC-03** — Cheap hash-verify-on-fetch closes a silent integrity hole before the R2 backend starts serving blobs.
- **SEC-01** — Moderate effort to make revocation real, removing a P1 zero-knowledge promise that is currently a no-op.
- **HUB-10** — Adding retry/backoff to the R2 backend is the difference between a sync that survives a throttle and one that aborts wholesale.
- **GIT-01** — Small non-empty-checkout guard stops broken remote HEADs from being recorded as clean, empty trees.
- **SEC-06** — A few extra regex shapes meaningfully widen secret-leak coverage at near-zero cost.
- **HUB-09** — Deleting the redundant ObjectExists call cuts request cost and removes a TOCTOU window in one stroke.
- **PROD-03** — Guided `init` with a printed next command is a tiny change that disproportionately improves first-run UX.

## Strategic bets

- **GIT-03** — An OS-enforced agent sandbox is the only path to a real isolation guarantee; substring matching plus an interpreter denylist is bypassable and blocks safe autonomous agent execution.
- **SEC-02** — Encrypting the namespace map is foundational to the zero-knowledge promise; without it R2 sees every path, remote, and device timeline, undermining the entire hub thesis.
- **SYNC-02** (with **HUB-11**) — Compaction and snapshot exchange on both the local log and the hub are what keep cost, memory, and replay time bounded as the system scales; today both are dead-ended.
- **HUB-12** — Adding a Delete path is the prerequisite for any GC at all; without it unreferenced ciphertext accumulates on R2 forever and revocation can never reclaim storage.
- **SEC-07** — Key rotation, forward secrecy, and a KEK/envelope layer determine the long-term crypto posture and recoverability of the hub once it holds real user data.
- **PROD-01 / PROD-04** — A one-command `clone` and a real background service are what turn a proven engine into a daily-driver "Dropbox for code," not just a CLI you have to remember to run.

---
## Security & Cryptography

DevStrap's threat model and data model show real cryptographic sophistication — signed HLC event chains, age-encrypted content-addressed blobs, OS-keychain-backed device identities — but several of the hardest zero-knowledge guarantees are documented rather than enforced. The most serious gaps cluster around the sync hub trust boundary: the namespace map ships in plaintext, fetched blobs are never re-verified against their content address, revocation does not actually remove decryptable ciphertext from the hub, and a fresh device will apply unverified peer events on first sync. The findings below are ordered roughly by blast radius; SEC-01 through SEC-04 are confidentiality/integrity failures at the hub plane, while SEC-05 through SEC-08 cover supply-chain, redaction, and key-hierarchy hardening.

### SEC-01 — Device-revocation re-encryption is a no-op on the hub: old ciphertext blobs are never deleted and stay decryptable by the revoked key forever

**Severity / Effort / Category:** P1 / M / reliability

**Problem:** When a device is revoked or lost, `rewrapBlobsOnRevoke` re-encrypts blobs to the reduced recipient set and produces a new content-addressed ref, but (a) it only operates on blobs already cached on the local disk (`readEnvBlob`), never touching the hub, and (b) the `Hub` interface has no `DeleteBlob`/`DeleteEvent` method at all. The old ciphertext — encrypted to the revoked device's still-valid X25519 identity — therefore remains on R2/the hub indefinitely, and the revoked device (or anyone who exfiltrated its key) can keep pulling and decrypting it. The HUB-04 "limit future exposure" story is unenforced for the one store that matters; the only real protection left is the `needs_rotation` flag, i.e. manual source rotation.

**Evidence:** `internal/sync/hub.go:53-58` — the `Hub` interface is `Push`/`Pull`/`PutBlob`/`GetBlob`, with no `Delete`. `internal/cli/blob_gc.go:42-63` — `rewrapBlobsOnRevoke` loops `AllBlobRefs`, reads each via `readEnvBlob(opts.paths(), ref)` (local cache only; logs "blob not cached locally, skipping" on miss), writes the new blob locally with `writeEnvBlob`, and calls `store.UpdateBlobRef`; it never calls `hub.PutBlob` for the new ref nor any delete for the old. The doc comment at `blob_gc.go:17-23` itself admits the revoked recipient "stays decryptable by that key forever." `spec/15_SECURITY_THREAT_MODEL.md:134` admits age has no native revocation and prescribes "re-encrypting every affected bundle," but that mitigation never reaches hub deletion.

**Recommendation:** Add `DeleteBlob` (plus a tombstone/GC for superseded event-referenced blobs) to the `Hub` interface and R2 backend, and make revocation a hub operation: rewrap to the reduced recipient set, `PutBlob` the new ciphertext, repoint the signed namespace event, then delete the old object from the hub. Keep `needs_rotation` as belt-and-suspenders, since anything the revoked device already downloaded is irrecoverably exposed. Longer term, adopt envelope encryption (see SEC-07) so revocation becomes a metadata-only KEK rewrap instead of bulk blob re-encryption.

**Actionable steps:**
1. Extend `internal/sync.Hub` with `DeleteBlob(ctx, sha256Hex)` and implement it on `R2Hub` (S3 `DeleteObject`) and `FileHub`.
2. In `rewrapBlobsOnRevoke`, after writing the new local blob, `hub.PutBlob` the new ref and `hub.DeleteBlob` the old ref (guarded by a ref-count so a still-referenced blob is not deleted).
3. Pull blobs that are not locally cached before rewrapping (today non-cached blobs are skipped, leaving them encrypted to the revoked key on the hub).
4. Emit a signed `env.bundle.reencrypted` audit event recording old ref, new ref, and removed recipient.
5. Add a test: revoke device B, assert the old blob key returns `ErrBlobNotFound` from the hub and the new blob does not list B as a recipient.

**Example:**
```go
// internal/sync/hub.go
type Hub interface {
    Push(ctx context.Context, events []state.Event) error
    Pull(ctx context.Context, afterHLC int64) ([]state.Event, error)
    PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error
    GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error)
    DeleteBlob(ctx context.Context, sha256Hex string) error // NEW
}
// blob_gc.go after UpdateBlobRef(oldRef -> newRef):
_ = hub.PutBlob(ctx, blobHashHex(newRef), bytes.NewReader(newCiphertext))
if refcount[oldRef] == 0 { _ = hub.DeleteBlob(ctx, blobHashHex(oldRef)) }
```

**References:**
- https://sadensmol.com/posts/2026/04/learning-system-design-10-umbrel-home-backup/ — Revocation means deleting the revoked device's wrapped KEK, rotating the KEK, rewrapping DEKs, and uploading new wrapped keys; data already downloaded by the revoked device stays exposed, so source rotation is still required.
- https://news.ycombinator.com/item?id=46812099 — In E2E systems revocation usually only affects future access; old keys still decrypt already-synced ciphertext, and crypto-erasure (deleting keys/objects) plus epoch keys are the standard mitigations.

### SEC-02 — Namespace-map events are uploaded to R2 in plaintext, leaking every project path, git remote, and per-device activity timeline to a supposedly zero-knowledge hub

**Severity / Effort / Category:** P1 / L / hardening

**Problem:** `R2Hub.Push` marshals the full `state.Event` — including `PayloadJSON` with the project path (e.g. `work/nclh/foc-models`), `remote_url`, `remote_key`, and `default_branch` — to JSON and `PutObject`s it without any encryption. The package doc comment claims "All payloads and blobs are age-encrypted... R2 stores only ciphertext plus a signed map," but the map is plaintext: it is signed, not encrypted, and signing gives integrity, not confidentiality. A curious or compromised hub operator therefore learns the entire namespace — private repo names, the orgs/hosts in use, the folder taxonomy. Worse, the object key itself (`workspaces/<id>/events/<hlc>/<device_id>/<seq>/...`) embeds `device_id` and a wall-clock-derived HLC in plaintext, leaking device count and a precise per-device activity timeline (classic traffic analysis) before the body is even read.

**Evidence:** `internal/hub/r2.go` `Push` (~line 99) does `raw, err := json.Marshal(event); h.S3.PutObject(ctx, key, raw, true)` — the event body, including `PayloadJSON`, is uploaded as plaintext JSON. `internal/sync/events.go:11-18` — `ProjectPayload` carries `Path`/`RemoteURL`/`RemoteKey`/`DefaultBranch`; `events.go:89` only strips userinfo via `redact.StripURLUserinfo` (host/org/repo path remain). `internal/hub/r2.go:65-67` — `eventKey` embeds `%s` `device_id` and `%020d` HLC in the object key. The `r2.go:1-4` package doc comment conflates "signed" with "encrypted." `spec/15:53` lists "substitute metadata" as an in-scope hub threat and the design is branded zero-knowledge, so leaking full paths/remotes contradicts the stated posture.

**Recommendation:** Envelope-encrypt the event payload to the approved-device recipient set, leaving only minimal routing fields (HLC, device_id, seq, content_hash, signature) outside the ciphertext, and treat even those as a deliberately accepted, minimized leak. At minimum, hash/HMAC the path into the object key instead of leaking the literal HLC+device triple, and document precisely which metadata the hub sees (size, count, timing) as accepted risk. Correct the `r2.go` and `Hub` doc comments so they stop claiming the namespace map is encrypted.

**Actionable steps:**
1. Split `state.Event` into a plaintext routing envelope (id, hlc, seq, device_id, content_hash, device_sig) and an age-encrypted payload blob keyed to the recipient set.
2. Encrypt `PayloadJSON` before `Push`; decrypt after `Pull` using the local age identity. Keep the Ed25519 signature over the ciphertext+routing fields so integrity/ordering still verify.
3. Replace the literal-HLC object key with a random/opaque ULID-style key plus a separate signed index, or accept the HLC ordering leak explicitly and document it.
4. Fix the `r2.go` and `Hub`-interface doc comments to state that events are signed (integrity) and, after this change, encrypted (confidentiality) — not conflate the two.
5. Add a metadata-leakage section to `spec/15` enumerating exactly what R2 can observe (object sizes, counts, request timing, IP), with padding/batching mitigations.

**Example:**
```go
// Today (plaintext map):
raw, _ := json.Marshal(event) // includes path=work/nclh/foc-models, remote=github.com/acme/secret
h.S3.PutObject(ctx, key, raw, true)
// Proposed: encrypt the payload, sign the envelope.
enc, _ := envbundle.EncryptBytes(event.PayloadJSON, recipients)
envelope := EventEnvelope{ID: event.ID, HLC: event.HLC, Seq: event.Seq, DeviceID: event.DeviceID, ContentHash: event.ContentHash, Cipher: enc, Sig: event.DeviceSig}
h.S3.PutObject(ctx, key, mustJSON(envelope), true)
```

**References:**
- https://github.com/nextcloud/end_to_end_encryption_rfc/blob/master/RFC.md — "Access to ciphertext must not leak file content nor file names nor the names of subfolders" (file count and topmost folder name are the only accepted leaks); DevStrap currently leaks full paths and remotes.
- https://github.com/syncthing/syncthing/blob/main/lib/protocol/encryption.go — Syncthing's untrusted-device mode encrypts `FileInfo` and wraps it in a fake `FileInfo` with an encrypted (base32-slashified) name so the relay never sees real paths.
- https://www.priviy.com/en/blog/cloud-metadata-zero-knowledge-2026 — Serious zero-knowledge encrypts filenames and folder structures client-side; only technical metadata (IP, timing) is unavoidable, and linked size/timestamp/access-frequency signals enable re-identification via traffic analysis.

### SEC-03 — Content-addressed blobs are never verified against their hash on fetch, and multi-recipient age is not sender-authenticated — enabling forged/substituted env & draft bundles

**Severity / Effort / Category:** P1 / M / bug-risk

**Problem:** Two issues compound. (1) On pull, the client fetches a blob by its sha256 key and writes it to cache without ever recomputing `sha256(ciphertext)` and comparing it to the signed `blob_ref` — it trusts the hub to honor content-addressing, so a malicious or buggy hub can return arbitrary bytes under that key. (2) age with multiple recipients is explicitly not sender-authenticated: per the age author, anyone holding the identity for recipient B can forge a file that decrypts cleanly under identity A by reusing the file key. DevStrap encrypts every env and draft bundle to the full approved-device set (multi-recipient), so a single malicious-but-approved device — an actor explicitly in the threat model — can fabricate a valid-looking bundle. The signed `draft.snapshot.created` event binds `path -> blob_ref` and would catch substitution, but only if the client enforced the hash, which it does not. The integrity anchor exists in the data model and is discarded at the one place it must be checked.

**Evidence:** `internal/cli/sync.go` `pullReferencedBlobs` (~lines 140-155) does `reader, err := hub.GetBlob(ctx, blobHashHex(ref)); ciphertext, _ := io.ReadAll(reader); writeEnvBlob(paths, ref, ciphertext)` — no sha256 recompute/compare against `blobHashHex(ref)`. `internal/hub/r2.go` `GetBlob` (~line 177) returns raw object bytes with only an `isValidHexKey` format check, no content verification. `internal/envbundle/bundle.go:18-49` — `Encrypt` builds age recipients from `recipients []string` (multi-recipient) and writes plaintext with no producer signature; `internal/draftbundle` `Pack` mirrors this. The age author confirms the multi-recipient forgery caveat (`words.filippo.io/age-authentication` and discussion #613).

**Recommendation:** Make the content-address authoritative on the client: after `GetBlob`, recompute sha256 of the bytes and reject on mismatch (the `blob_ref` comes from a signed event, turning the hub into an untrusted bit-bucket). Independently, add sender authentication for bundles so a malicious approved peer cannot forge them — sign the bundle plaintext (or the ciphertext) with the producing device's Ed25519 key and verify on extract, or move to single-recipient-per-blob age (envelope/KEK, see SEC-07) which restores age's built-in sender authentication.

**Actionable steps:**
1. In `pullReferencedBlobs`, compute `sum := sha256.Sum256(ciphertext)` and compare `hex(sum)` to `blobHashHex(ref)`; on mismatch, treat as `ErrBlobNotFound` and surface a tamper warning.
2. Ideally enforce the hash inside `R2Hub.GetBlob` itself so substitution is impossible regardless of caller.
3. Add an Ed25519 producer signature over the bundle and persist it in the signed draft/env event; verify in `envbundle.Decrypt`/`draftbundle.Extract` before trusting plaintext.
4. Document the multi-recipient age forgery caveat in `spec/09` and `spec/15`, citing age-authentication.
5. Add tests: (a) hub returns wrong bytes under a valid key → rejected; (b) a non-producing approved recipient forges a bundle → signature check fails.

**Example:**
```go
// internal/cli/sync.go after io.ReadAll:
sum := sha256.Sum256(ciphertext)
if hex.EncodeToString(sum[:]) != blobHashHex(ref) {
    return missing, fmt.Errorf("blob %s failed content-address verification (hub tampering?)", ref)
}
```

**References:**
- https://words.filippo.io/age-authentication/ — Filippo Valsorda: with multiple recipients, "Alice can take the file, derive the file key using her identity, and then use the file key to change the contents... that new file will still decrypt successfully with Bob's identity." Sender authentication only holds for a single secret recipient.
- https://github.com/FiloSottile/age/discussions/613 — With multiple recipients the file key is shared, so any co-recipient can forge a ciphertext that decrypts for the others; you need a MAC/signature over the ciphertext or single-recipient encoding.
- https://github.com/FiloSottile/age/discussions/640 — age's own sender-authentication design explicitly refuses to support multiple recipients for exactly this reason.

### SEC-04 — Bootstrap window trusts unenrolled peers: a malicious hub can inject project.added events pointing at attacker-controlled remotes before any device is approved

**Severity / Effort / Category:** P1 / M / hardening

**Problem:** `verifyEventSignature` only fails closed once `hasEnrolledDevices()` is true (at least one approved device exists). On a freshly initialized device that has not yet approved any peer — exactly the new-machine "point it at `~/Code`, run `devstrap sync`" moment the product is built around — non-destructive events (`project.added`, `project.updated`, `draft.snapshot.created`) from completely unknown device IDs are accepted unverified, and `EnsureRemoteDeviceTx` silently auto-creates a placeholder "pending" device row for them. `mustVerifyEvent` only covers `project.deleted`/`project.renamed`. A compromised hub (an explicit adversary in `spec/15`) can therefore inject a `project.added` with `remote_url` = an attacker repo; when eager-clone materialization runs, the victim clones attacker-controlled code into their tree. This is a trust-on-first-use gap with no out-of-band pinning: nothing requires confirming a peer's signing-key fingerprint before the first namespace apply.

**Evidence:** `internal/state/store.go:2200-2255` — `verifyEventSignature`: when the device is unknown (`sql.ErrNoRows`) it returns `nil` unless `mustVerifyEvent(type) || enrolled` (`store.go:2220-2224`); same pattern for the no-signing-key and non-approved branches. `internal/state/store.go:2281-2288` — `mustVerifyEvent` returns true only for `project.deleted`/`project.renamed`. `internal/sync/events.go:202` calls `tx.EnsureRemoteDeviceTx(ctx, event.DeviceID)` which (per the comment at `events.go:199-201`) auto-creates "pending" device rows "accepted during the bootstrap window (HUB-03)." `spec/15:136` explicitly acknowledges the fail-open and that "a malicious hub could inject a rogue device" is remediation-deferred; `00_START_HERE` lists "out-of-band fingerprint confirmation" as not implemented yet.

**Recommendation:** Replace trust-on-first-use with an explicit enrollment ceremony: a new device must out-of-band pin at least one peer's Ed25519 signing-key fingerprint (QR / short-authentication-string) before applying any remote namespace event, and the first sync should require an authenticated full-state snapshot signed by a pinned device. Until a peer is pinned, treat all remote events as untrusted and quarantine them rather than applying. Gate eager-clone so a project whose source event is unverified is never auto-cloned without explicit user confirmation.

**Actionable steps:**
1. Add a "pending enrollment" state: on a device that has pulled remote events but pinned no peer, quarantine remote events into conflicts instead of applying them.
2. Implement out-of-band fingerprint confirmation (`devices enroll --signing-public-key` exists per SECU-05; add a short-authentication-string display plus confirm prompt).
3. Bind the bootstrap snapshot to a pinned device signature so the first apply is authenticated, mirroring AAD-bound device enrollment in the zero-trust demo below.
4. Gate eager-clone materialization so a project whose source event is unverified is never auto-cloned without an explicit user confirm.
5. Add a test: hub injects `project.added` from an unknown device on a fresh init → event quarantined, no clone, until the author key is pinned.

**Example:**
```go
// store.go mustVerifyEvent — once a workspace is multi-device, ALL types must verify:
func mustVerifyEvent(t string) bool {
    switch t {
    case "project.deleted", "project.renamed", "project.added", "project.updated", "draft.snapshot.created":
        return true
    }
    return false
}
// And refuse remote apply until >=1 peer signing key is pinned out-of-band.
```

**References:**
- https://github.com/cryptomator/docs/blob/develop/docs/security/hub.mdx — Cryptomator Hub acts purely as a key broker; the device/user key hierarchy means even a compromised Hub cannot mint trust — new devices are enrolled by the user, not auto-trusted by the server.
- https://github.com/guenni81/zero-trust-cloud-storage-education-demo — Device enrollment uses an X25519+ML-KEM envelope with AAD binding tenant/user/device plus nonce/timestamp to resist replay/rogue-device injection; identifiers are cached locally to prevent re-enrollment spoofing.

### SEC-05 — Release binaries are unsigned with no provenance/SBOM, and the release action is pinned to a floating tag

**Severity / Effort / Category:** P1 / M / hardening

**Problem:** For a tool that installs a daemon and handles git credentials, SSH keys, and age private keys, the distribution channel is the highest-leverage supply-chain target — yet the release pipeline produces only a plaintext `checksums.txt` with no cryptographic signature, no SLSA provenance/attestation, and no SBOM. A consumer downloading a binary cannot verify it was built by this repo's CI from this source. The `.goreleaser.yaml` has a `checksum` block but no `signs` section. Compounding it, the release workflow pins `goreleaser/goreleaser-action` to the mutable `@v7` tag (its own comment says to pin to a SHA) and lacks `id-token: write`, so keyless cosign cannot be adopted without a permissions change.

**Evidence:** `.github/workflows/release.yml` — `actions/checkout` and `actions/setup-go` are SHA-pinned, but goreleaser-action is `uses: goreleaser/goreleaser-action@v7 # ... pin to a SHA on the next bump` (floating). The permissions block is `contents: write` only — no `id-token: write` or `attestations: write`. `.goreleaser.yaml` has a `checksum:` block (`name_template` `checksums.txt`) but grep for `sign`/`cosign`/`sbom`/`attest` returns nothing. The `release.yml` note "Verify downloads against checksums.txt" is the only integrity story.

**Recommendation:** Adopt GoReleaser's standard supply-chain stack: cosign keyless (Sigstore/Fulcio/Rekor) signing of `checksums.txt`, Syft SBOM generation, and SLSA build provenance via `gh attestation` or slsa-github-generator. Add `id-token: write` + `attestations: write` to the workflow and pin goreleaser-action to a full commit SHA. Publish verification instructions so users run `cosign verify-blob` / `gh attestation verify`.

**Actionable steps:**
1. Add `id-token: write` and `attestations: write` to `release.yml` permissions; pin goreleaser-action and add the cosign-installer + syft actions, all SHA-pinned.
2. Add a `signs:` block to `.goreleaser.yaml` using cosign keyless over `checksums.txt`; prefer the modern `--bundle ${artifact}.sigstore.json` form over separate `.sig`/`.pem`.
3. Add an `sboms:` block (syft) and enable provenance attestation (`actions/attest-build-provenance` or slsa-github-generator's gh-hosted-builder) for each artifact.
4. Document verification in `README`/`SECURITY.md`: `cosign verify-blob --certificate-identity .../release.yml@refs/tags/$VERSION --certificate-oidc-issuer https://token.actions.githubusercontent.com --bundle checksums.txt.sigstore.json`.
5. Optionally add gitsign/commit signing so the tag itself is attestable.

**Example:**
```yaml
# .goreleaser.yaml (modern cosign keyless, bundle form)
signs:
  - cmd: cosign
    signature: "${artifact}.sigstore.json"
    args: ["sign-blob", "--bundle=${signature}", "${artifact}", "--yes"]
    artifacts: checksum
sboms:
  - artifacts: archive
# release.yml
permissions:
  contents: write
  id-token: write   # keyless cosign OIDC
  attestations: write
```

**References:**
- https://github.com/goreleaser/example-supply-chain — Canonical GoReleaser + Actions config: keyless cosign signing of `checksums.txt` via `--bundle checksums.txt.sigstore.json`, Syft SBOMs, and `gh attestation verify`, with copy-paste verify commands using `--certificate-identity .../release.yml@refs/tags/$VERSION`.
- https://goreleaser.com/customization/sign/sign/ — Official `signs:` reference; cosign keyless uses the `--bundle` flag combining cert+signature into one `.sigstore.json`.
- https://goreleaser.com/blog/slsa-generation-for-your-artifacts/ — Using slsa-github-generator with GoReleaser's hashes output and `id-token: write` to emit verifiable SLSA provenance logged to Rekor.

### SEC-06 — Value-shape redaction misses Snowflake, GitLab, Stripe, GCP service-account, and generic Bearer secrets that this product specifically handles

**Severity / Effort / Category:** P2 / S / hardening

**Problem:** `redact.tokenPatterns` is the best-effort net for secrets that were never registered as known values (e.g. a token a child process echoes into an agent log). It covers GitHub/Slack/AWS/OpenAI/Google-API/age/JWT/PEM, but omits several credential shapes directly in scope for DevStrap: GitLab PATs (`glpat-...`), Stripe live keys (`sk_live_`/`rk_live_` — note the existing `sk-[...]` regex requires a hyphen and will not match the underscore-delimited `sk_live_`), GCP service-account JSON `private_key` blocks (the key body is base64 within a single JSON line, not a bare PEM), Snowflake-style credentials (the threat model lists Snowflake/cloud configs as a protected asset and the surrounding tooling is Snowflake-centric), and generic `Authorization: Bearer <token>` headers. These leak into agent logs (persisted `0600` but on disk and shippable in bug reports) and CLI error output.

**Evidence:** `internal/redact/redact.go:65-85` — `tokenPatterns` lists exactly: URL userinfo, `gh[pousr]_`, `github_pat_`, `xox[baprs]-`, AWS `AKIA`/`ASIA`/..., `sk-[A-Za-z0-9_-]{20,}`, `AIza...`, `AGE-SECRET-KEY-1...`, `BEGIN PRIVATE KEY` PEM header, and JWT `eyJ...`. No `glpat-`, no `(sk|rk)_live_`, no bearer, no Snowflake pattern. The `sk-[A-Za-z0-9_-]{20,}` pattern needs a literal hyphen after `sk`, so `sk_live_...` (underscore) does not match. `spec/15:20-21` lists Snowflake/cloud configs and API keys as protected assets.

**Recommendation:** Extend `tokenPatterns` with the missing high-value shapes and add a JSON-aware redactor that masks the value of any key matching `(?i)(secret|token|password|private_key|api_key|authorization)`. Keep the registered-value `Redactor` as the primary defense; this is the safety net.

**Actionable steps:**
1. Add regexes: `glpat-[0-9A-Za-z_-]{20,}`, `(sk|rk)_live_[0-9A-Za-z]{20,}`, `(?i)bearer\s+[A-Za-z0-9._-]{20,}`, and a Snowflake-style password/oauth-token pattern.
2. Add a JSON-field redactor that, when a line parses as JSON or contains `"private_key":"-----BEGIN`, masks the value.
3. Verify the `redact.Writer` handles the GCP service-account case where the private key is one JSON line with literal `\n` escapes.
4. Add table-driven tests for each new shape, including the GCP SA JSON case.
5. Cross-check that the `internal/childenv` dangerous-name list and redact stay in sync as new providers are added.

**Example:**
```go
// internal/redact/redact.go tokenPatterns += 
regexp.MustCompile(`glpat-[0-9A-Za-z_-]{20,}`),
regexp.MustCompile(`(?:sk|rk)_live_[0-9A-Za-z]{20,}`),
regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{20,}`),
regexp.MustCompile(`"private_key"\s*:\s*"-----BEGIN[^"]+"`),
```

**References:**
- https://docs.gitlab.com/security/token_overview/ — GitLab personal/project/group access tokens carry the `glpat-` prefix; recommended detection is prefix-based scanning.
- https://docs.stripe.com/keys — Stripe live secret/restricted keys use the `sk_live_`/`rk_live_` prefixes, distinct from test `sk_test_` keys; these are the high-value shapes to scrub.
- https://www.priviy.com/en/blog/cloud-metadata-zero-knowledge-2026 — Reinforces that anything written client-side in plaintext (logs, error text) is a leak surface a zero-knowledge posture must scrub, not just synced blobs.

### SEC-07 — Device age identities are long-lived with no rotation or forward secrecy, and there is no KEK/envelope layer, forcing expensive per-blob re-encryption on every membership change

**Severity / Effort / Category:** P2 / L / design-improvement

**Problem:** Each device's X25519 identity is generated once at init and never rotated, and every env/draft blob is encrypted directly to the set of device recipients with no key-encryption-key (KEK) intermediary. Consequences: (1) no forward secrecy — if a device identity leaks, every historical blob ever encrypted to it (all still on the hub per SEC-01) is decryptable; (2) revocation and routine key-hygiene rotation require bulk re-encrypting the actual content of every blob rather than a cheap metadata-only re-wrap; (3) there is no scheduled/epoch rotation at all. The industry-standard fix (Cryptomator Hub, Tresorit, the envelope designs below) is a key hierarchy: a per-blob random DEK, wrapped by a rotatable user/workspace KEK, with KEKs wrapped per device. Then add/revoke/rotate is a tiny rewrap of wrapped-DEKs, not a re-encrypt of gigabytes.

**Evidence:** `internal/envbundle/bundle.go:18-49` — `Encrypt` encrypts the content body directly to the recipient device keys (`age.Encrypt(&buf, ageRecipients...)`) with no DEK/KEK split; `internal/draftbundle` mirrors this. `internal/envbundle/bundle.go:74-...` — `Rewrap` decrypts and re-encrypts the whole content on revoke (bulk), proving the absence of an envelope layer; `internal/cli/blob_gc.go:51` calls it per blob. No `rotate` command exists in the CLI surface (the `00_START_HERE` command list has no `keys rotate`). Device identities are generated once in `internal/devicekeys` (`NewIdentity`) and never rotated.

**Recommendation:** Introduce envelope encryption: a per-blob DEK (random 256-bit, used once) encrypts the content; a rotatable workspace KEK wraps the DEK; the KEK is wrapped per approved device. Add epoch-based KEK rotation on every membership change plus a scheduled hygiene rotation, so revoke/rotate is a metadata-only re-wrap. This also fixes SEC-01's cost problem and gives forward secrecy from each rotation point. Add a `devstrap keys rotate` command and `env.bundle.reencrypted` / `device.rotated` audit events.

**Actionable steps:**
1. Define a workspace KEK with an epoch counter; wrap it per device using each device's X25519 recipient.
2. Change `envbundle`/`draftbundle` to generate a random per-bundle DEK, encrypt content with the DEK, and store the KEK-wrapped DEK alongside the ciphertext (or in the signed event).
3. On approve/revoke, bump the KEK epoch and rewrap only the wrapped-DEKs (metadata) rather than re-encrypting content; old epochs become unreadable to removed devices.
4. Add a scheduled/manual `devstrap keys rotate` that advances the epoch and re-wraps.
5. Document the recovery tradeoff (loss of all devices + recovery phrase = data loss) explicitly, as the zero-knowledge designs below do.

**Example:**
```go
// Conceptual layering:
DEK := random 32 bytes                 // per blob, single-use
ciphertext := AEAD(DEK, content)       // content encrypted once
wrappedDEK := age.Encrypt(KEK_epoch_recipient, DEK)
wrappedKEK[deviceX] := age.Encrypt(deviceX_recipient, KEK_epoch)
// revoke(deviceX): epoch++; re-wrap KEK for remaining devices; rewrap wrappedDEKs to new KEK. No content re-encryption.
```

**References:**
- https://sadensmol.com/posts/2026/04/learning-system-design-10-umbrel-home-backup/ — Master Secret → rotatable KEK → per-chunk DEK (XChaCha20-Poly1305): "Rotating a KEK is cheap — re-wrap all the DEKs (metadata-only)... device revocation, periodic hygiene rotation, and post-exposure rotation are all affordable." Per-device X25519 wrapping enables multi-device without breaking zero-knowledge.
- https://github.com/cryptomator/docs/blob/develop/docs/security/hub.mdx — Device → User → Vault key chain: adding a device only re-wraps the user key once, never re-encrypting vault keys — the same intermediary-key pattern DevStrap lacks.
- https://github.com/guenni81/zero-trust-cloud-storage-education-demo — Forward-secure ratcheting + epoch/snapshot key hierarchy + crypto-erasure: removing a user rotates the root of trust and revokes access without re-encrypting stored data.

### SEC-08 — Hosted-mode prefix-scoped/temporary credentials and object immutability are documented but unimplemented and unenforced, leaving a leaked write key able to delete or roll back any tenant's ciphertext

**Severity / Effort / Category:** P2 / L / scalability

**Problem:** `R2Config` documents two credential modes and a `PrefixScope` that "restricts all operations to `workspaces/<workspace_id>/` so a scoped credential cannot touch another tenant's objects" — but `PrefixScope` is a struct field that is never read anywhere in `R2Hub`; all keys are built directly from `WorkspaceID` with no enforcement, and there is no temporary-credential broker (the production S3 client is unimplemented — `ErrNotImplemented`). With no object-lock/versioning and no scoped/short-lived credentials, a single leaked R2 key (or a compromised runner that received the parent key) can wipe or roll back the encrypted event log and blobs — a denial-of-service / rollback attack the zero-knowledge encryption does nothing to prevent. (Note: the per-device `prev_event_hash` chain is validated on local apply, so naive event deletion/reorder is partially detectable client-side; the real gap is credential scoping, immutability/WORM, and a broker — not the chain check.)

**Evidence:** `internal/hub/r2.go:230-242` — `R2Config` declares `CredentialMode` and `PrefixScope`; grep shows `PrefixScope` appears only in its own declaration and doc comment (lines 231, 240, 242), never in `eventKey`/`blobKey`/`Push`/`Pull`, which build keys from `h.WorkspaceID` only (`r2.go:64-72`). `r2.go:217` `ErrNotImplemented` — no production AWS SDK wiring/credential broker exists. `spec/15:138` explicitly lists the residual integrity/availability risks (a leaked bucket-wide key can "delete, overwrite, withhold, or reorder ciphertext") and names prefix-scoped temporary creds, object-lock, and snapshots as required-but-not-built. Note: `validatePrevEventHash` is invoked on `InsertEvent` (`internal/state/store.go:2090`) and `ErrEventHashChain` exists (`store.go:40`), so the chain check is partially present locally for a single device's stream.

**Recommendation:** Before any hosted/multi-tenant deployment: (1) implement a control-plane credential broker issuing short-lived, prefix-scoped (`workspaces/<id>/*`) STS-style credentials or presigned URLs so runners never hold the parent key; (2) actually enforce `PrefixScope` in `R2Hub` by asserting every constructed key carries the scope prefix; (3) enable R2/S3 object-lock (WORM) + versioning so ciphertext cannot be silently deleted/overwritten, and snapshot the event log; (4) extend the `prev_event_hash` chain check into cross-device/full-snapshot integrity verification so a hub that drops or reorders across authors is detectable, not just within one device's stream.

**Actionable steps:**
1. Implement a control-plane broker that mints prefix-scoped, short-TTL credentials (R2 temp tokens / S3 STS) per device/runner; never ship the bucket-wide key to runners.
2. Enforce `PrefixScope`: add an assert in `R2Hub.eventKey`/`blobKey` that the key begins with the configured scope, and reject operations otherwise.
3. Enable object versioning + object-lock (compliance/WORM) on the bucket and a retention policy so deletes/overwrites are recoverable.
4. Strengthen integrity verification on `Pull`: the per-device `prev_event_hash` chain is validated on local apply (`validatePrevEventHash`); add cross-author/snapshot-anchored verification so withhold/reorder across devices is detectable.
5. Add periodic snapshots/backups of the event log and document RPO/RTO in `spec/19`.

**Example:**
```go
// Enforce the scope that is currently only documented:
func (h R2Hub) eventKey(e state.Event) string {
    key := fmt.Sprintf("workspaces/%s/events/%020d/%s/%d/%s.json", h.WorkspaceID, e.HLC, e.DeviceID, e.Seq, e.ID)
    if h.PrefixScope != "" && !strings.HasPrefix(key, h.PrefixScope) { panic("key escapes prefix scope") }
    return key
}
// Broker (hosted): issue creds scoped to s3:prefix workspaces/<id>/* with TTL ~15m; runners never see the parent key.
```

**References:**
- https://github.com/cryptomator/docs/blob/develop/docs/security/hub.mdx — A compromised Hub cannot decrypt content, but the design still relies on the storage layer for availability; DevStrap must add object-lock/versioning since encryption alone does not protect integrity/availability.
- https://developers.cloudflare.com/r2/buckets/object-lock/ — Cloudflare R2 Object Lock provides WORM retention so objects cannot be deleted or overwritten until retention expires — the immutability control DevStrap's event log/blob store needs against a leaked write key.
- https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-points-policies.html — Scoped access-point/STS prefix policies (`s3:prefix` condition) are the standard way to confine a credential to one tenant prefix, matching the unimplemented `PrefixScope`.

---
## Sync Engine, Conflicts & Data Model

The sync engine is conceptually well-formed — HLC stamping, signed events, skew quarantine, and order-independent conflict reconciliation are all present — but several load-bearing pieces of the documented contract are either dead code or actively unsafe. The most serious issues are a pull cursor that can silently and permanently strand quarantined events behind a high-water mark (SYNC-01) and the complete absence of the documented snapshot/compaction path, which leaves the event log unbounded and any lagging device unrecoverable (SYNC-02). The remaining findings cover hardening gaps in the conflict and tamper-evidence model, push-side and read-path scalability, and a data model that hard-blocks the stated multi-workspace future.

### SYNC-01 — Pull cursor advances to a high-water mark, silently and permanently skipping quarantined or dependency-broken events below it

**Severity / Effort / Category:** P1 / M / reliability

**Problem:** `ApplyEvents` advances the hub cursor to `maxAppliedHLC` (the highest HLC of *newly-inserted* events). Within the same batch, events that are skew-quarantined or fail the `prev_event_hash` check are skipped with `continue` and never inserted, while higher-HLC events from other devices *are* inserted and push the cursor past them. Because `Pull(afterHLC)` only returns events with HLC strictly greater than the cursor, any skipped event whose HLC is below the new cursor is never re-delivered — a permanent, silent gap. The code's own comment (`events.go:222-227`) claims the broken event "will be re-delivered on the next pull," but that is only true if no higher-HLC event from another device advances the cursor past it — exactly the common multi-device case where the guarantee fails.

**Evidence:** Verified `internal/sync/events.go:175-180` (skew/epoch quarantine `continue`), `:184-189` (skew-ahead quarantine `continue`), `:218-229` (hash-chain break `continue` with an incorrect "re-delivered next pull" comment), `:232-234` (`maxAppliedHLC` = max of inserted events only). `internal/cli/sync.go:79-83` advances the cursor to `appliedMaxHLC` whenever `appliedMaxHLC > cursor`. `internal/sync/hub.go:104` (FileHub) and `internal/hub/r2.go:133` (R2Hub) both filter `event.HLC > afterHLC`, so anything `<= cursor` is never returned again. The `event_delivery` table (migration `00002`, line 31) is unused outside migrations and the schema-list test (only `internal/state/store_test.go:219` references it), so there is no per-event delivery tracking to fall back on.

**Recommendation:** Make the cursor a low-water mark: track the lowest HLC of any event in the batch that was *not* successfully applied (quarantined or hash-chain-broken) and never advance the cursor at or past it. Concretely, advance to `min(maxAppliedHLC, lowestUnappliedHLC-1)`. Longer term, wire the existing `event_delivery` table so skipped events stay "pending" and are re-pulled by id, decoupling delivery progress from a single HLC scalar.

**Actionable steps:**
1. In `ApplyEvents`, track `lowestUnappliedHLC` = min HLC over every event hit by an epoch/skew quarantine `continue` or an `ErrEventHashChain` `continue`.
2. Return both `maxAppliedHLC` and `lowestUnappliedHLC`; in `runSyncCycle` advance the hub cursor to `min(appliedMaxHLC, lowestUnappliedHLC-1)`, never higher.
3. Add a regression test: a batch `[A.hlc=10 quarantined, A.hlc=11 chain-dep-on-10, B.hlc=20 valid]` must leave the cursor `< 10` so A's events are re-pulled next cycle (this currently advances the cursor to 20 and strands A).
4. Correct the misleading comment at `events.go:222-227` that asserts unconditional re-delivery.
5. Longer term, populate `event_delivery(event_id, device_id, sync_state)` and re-pull "pending" rows by id instead of relying solely on the HLC scalar; emit a log/metric for "cursor held back by N pending events."

**Example:**
```go
// events.go
lowestUnapplied := int64(math.MaxInt64)
// on every quarantine/hash-chain `continue`:
if event.HLC < lowestUnapplied { lowestUnapplied = event.HLC }

// caller (sync.go):
next := appliedMaxHLC
if lowestUnapplied != math.MaxInt64 && lowestUnapplied-1 < next { next = lowestUnapplied - 1 }
if next > cursor { store.AdvanceHubCursor(ctx, hubID, next) }
```

**References:**
- [Cursor-based sync patterns (arcflow.me)](https://www.arcflow.me/patterns/cursor-based-sync) — advance the cursor only after records are safely applied; advancing too early creates permanent gaps.
- [Delta-sync checkpoints (cursa.app)](https://cursa.app/en/page/delta-sync-checkpoints-and-efficient-data-transfer) — checkpoints must be stored transactionally with applied changes so the client never advances past data it has not applied.

### SYNC-02 — No event-log compaction or snapshot exchange: ErrSnapshotRequired is dead-ended, R2 never emits it, and the events table grows forever

**Severity / Effort / Category:** P1 / L / scalability

**Problem:** The Hub contract documents a "410 -> snapshot" model (Pull returns `ErrSnapshotRequired` when the cursor falls below retention). But there is no snapshot import/export anywhere: the CLI just turns `ErrSnapshotRequired` into an `exitNetwork` error and stops (`cli/sync.go:70-72`), the production `R2Hub.Pull` never returns it (no retention horizon at all), and no code ever truncates the events table or the namespace event history. Consequently every new or long-offline device replays the entire history from HLC 0 (one `ListObjectsV2` page-walk + one `GetObject` per event on R2), and the log is unbounded. A device that ever falls behind a future retention window is permanently bricked because the recovery path does not exist.

**Evidence:** Verified `cli/sync.go:70-72` (`if errors.Is(err, dssync.ErrSnapshotRequired) { return appError{exitNetwork, err} }` — no snapshot fetch). `internal/hub/r2.go:110-152` `R2Hub.Pull` has no retention/horizon logic and structurally cannot return `ErrSnapshotRequired`. FileHub retention (`hub.go:91-94`, `RetentionHLC` field) exists only for tests. `grep 'DELETE FROM events|prune|truncate'` over `internal/state/store.go` returns nothing — the events table is never pruned. `GCTombstones` (`store.go:1141`) only deletes `namespace_entries` rows, never events. `spec/12` line 324 confirms "Until wired, sync ignores this table and replays from 0 (ARCH2-02)." A grep for Snapshot import/export over `internal/sync`, `internal/hub`, `internal/cli` finds only the draft-bundle snapshot (a different feature) and the `ErrSnapshotRequired` sentinel — no full-state namespace snapshot.

**Recommendation:** Implement a state-convergence snapshot: export the current namespace map (latest-op-per-path + surviving tombstones) as a signed, content-addressed snapshot object at a known HLC floor; add `Hub.Snapshot()`/`ImportSnapshot()`; have the CLI fetch the snapshot, apply it, set the cursor to the snapshot boundary, then resume incremental pulls. Compact event objects below the durable snapshot floor only after the snapshot that supersedes them is confirmed (monotonic floor).

**Actionable steps:**
1. Define a snapshot object: `{floorHLC, namespace map entries with source-event coords, surviving tombstones, signed map digest}` keyed by content hash under `workspaces/<id>/snapshots/`.
2. Add `Snapshot(ctx)` to the Hub interface and a retention floor to `R2Hub.Pull` so it returns `ErrSnapshotRequired` when `afterHLC < floor`.
3. In `runSyncCycle`, on `ErrSnapshotRequired`: `GetSnapshot`, verify signature, import (`ApplyEvents`-equivalent), set hub cursor = `snapshot.floorHLC`, then re-`Pull` from that floor.
4. Add a scheduled compactor that writes a new snapshot every N events and deletes event objects strictly below the snapshot floor, with a monotonic (never-lowered) floor.
5. Test: a device at `cursor=0` against a hub whose `floor=K` receives the snapshot, then only events `> K`; and exercise the snap path in normal operation, not only as a fallback.

**Example:**
```go
// hub.go
type Hub interface {
  Push(...); Pull(...);
  Snapshot(ctx context.Context) (Snapshot, error) // namespace map @ floorHLC
  PutBlob(...); GetBlob(...)
}

// sync.go
if errors.Is(err, dssync.ErrSnapshotRequired) {
  snap, _ := hub.Snapshot(ctx)
  importSnapshot(ctx, store, snap)
  store.AdvanceHubCursor(ctx, hubID, snap.FloorHLC)
  remoteEvents, err = hub.Pull(ctx, snap.FloorHLC)
}
```

**References:**
- [Sync architecture ADR (tanstack-do-db-collection)](https://github.com/grrowl/tanstack-do-db-collection/blob/main/docs/adr/0001-sync-architecture.md) — state-convergence log compacted to latest-op-per-key beyond a horizon; the retention floor *is* the reconnect snap-fallback boundary, and the snap path should be exercised in normal operation rather than being dead code.
- [Event compaction & snapshot truncation ADR (evt)](https://github.com/photon-grove/evt/blob/main/docs/adr/0001-event-compaction-and-snapshot-truncation.md) — compaction never deletes an event unless a durable snapshot already captures it (`snapshot.EventSequence >= throughSequence`); the snapshot floor must be monotonic.

### SYNC-03 — epochFloorMS=0 disables past-direction quarantine, and first-writer-wins same-path reconciliation lets a near-epoch HLC permanently win path ownership

**Severity / Effort / Category:** P2 / S / hardening

**Problem:** The code self-documents that a malicious/buggy peer must not be able to win conflicts "from the past direction" by emitting tiny HLC values, and adds `epochFloorMS` for exactly that — then sets it to 0, so any event with `HLC>0` passes. This matters because same-path/different-remote reconciliation is *first-writer-wins* (`samePathLess` picks the *smaller* HLC as winner), whereas the same-remote add/update path is *last-writer-wins*. So an event stamped just above epoch always wins path ownership in a cross-remote conflict and cannot be displaced — the opposite of the same-remote path. A peer with a stuck-in-the-past clock or a crafted near-epoch event can claim any namespace path against a different remote, and the rightful owner can never reclaim it. There is also no symmetric past-direction skew bound (only the +5min ahead bound is enforced).

**Evidence:** Verified `internal/sync/events.go:31-38` (`epochFloorMS` const = 0, with a comment that "A production deployment should raise this"); `events.go:174-175` (`physical := event.HLC >> hlcLogicalBits; if event.HLC <= 0 || physical < epochFloorMS` — with floor 0 only non-positive HLC is rejected); `events.go:184` enforces only the `+maxSkew` ahead bound, no behind bound. `events.go:414` (`if samePathLess(next, current) { winner=next, incomingWins=true }`) → smaller-HLC incoming wins = first-writer-wins for different-remote. `events.go:300-306` uses last-writer-wins for the same remote (`if !samePathLess(cur, inc) { return nil }`). The asymmetry is real and the zero floor removes the only guard against weaponizing it.

**Recommendation:** Set `epochFloorMS` to a real launch-epoch constant (a fixed ms timestamp) so events claiming an implausibly old physical time are quarantined, and add a symmetric staleness bound (reject events whose physical time is more than `maxStaleMS` *behind* local, not only ahead). Decide first-writer-vs-last-writer for same-path/different-remote deliberately and document the intent; if first-writer is intended, gate it on the event passing the epoch floor.

**Actionable steps:**
1. Define `epochFloorMS` = a fixed ms timestamp (e.g. DevStrap GA / 2025-01-01) instead of 0.
2. Add a past-direction guard: quarantine when `now-physical > maxStaleMS` so "stuck in the past" peers cannot win first-writer-wins ownership.
3. Decide first-writer vs last-writer for same-path/different-remote explicitly and document it; if first-writer is intended, require the event to pass the epoch floor before it can be crowned winner.
4. Update the deterministic apply tests to use realistic HLC physical components (the comment notes tests currently use `physical=0`, which is why the floor is pinned at 0).
5. Add an adversarial test: a forged near-epoch `project.added` against a different remote must be quarantined, not crowned path owner.

**Example:**
```go
const epochFloorMS = 1735689600000 // 2025-01-01T00:00:00Z
const maxStaleMS = 90 * 24 * 60 * 60 * 1000 // 90d

// in ApplyEvents:
if event.HLC <= 0 || physical < epochFloorMS || now-physical > maxStaleMS {
    quarantineSkewedEvent(...); continue
}
```

**References:**
- [Logical Physical Clocks / HLC (Kulkarni & Demirbas)](https://cse.buffalo.edu/tech-reports/2014-04.pdf) — HLC bounds l-pt drift to a conservative Δ; when Δ is violated the local-reset and ignore-message actions fire, making HLC robust to stragglers (pt stuck in the past) and rushers. A finite both-directions drift bound is the documented defense.
- [CockroachDB HLC doc.go](https://fossies.org/linux/cockroach/pkg/util/hlc/doc.go) — CockroachDB HLC enforces a bounded max clock offset; events beyond the offset bound are rejected/handled rather than trusted, exactly the past-direction guard this code disables with `floor=0`.

### SYNC-04 — Push has no cursor: every sync re-reads and re-uploads the entire local event log (including remote-origin events)

**Severity / Effort / Category:** P2 / M / scalability

**Problem:** `runSyncCycle` calls `store.PendingEvents`, which `SELECT`s *all* rows from events with no watermark and no device filter, then `Push()`es the whole set every cycle. FileHub dedups by id and R2Hub does an `ObjectExists` before each conditional `PutObject`, so correctness holds, but the client still materializes the full history in memory and performs O(total events) HEAD requests against R2 on every sync — including for events that originated on other devices and were merely applied locally (`PendingEvents` has no device filter, so it re-pushes remote-origin events the hub already has). The pull side has `hub_cursors` for incremental fetch; the push side has nothing, and the migration-defined `device_sync_state.next_seq` / `sync_cursors` are not used to bound it.

**Evidence:** Verified `internal/state/store.go:2317-2336` `PendingEvents` = `SELECT ... FROM events ORDER BY hlc ASC, device_id ASC, id ASC` (no `WHERE`, no device filter, despite the name "Pending"). `cli/sync.go:46,60` push `localEvents` = all rows. `internal/hub/r2.go:84-108` `Push` issues `ObjectExists` + conditional `PutObject` per event on every call. `device_sync_state` exists (`store.go:2015,2035`) but only stamps the local HLC/next_seq, never a push watermark; `sync_cursors` is unused (grep).

**Recommendation:** Add a per-hub push cursor (reuse `hub_cursors` with a distinct key prefix, or `sync_cursors`) recording the highest local-origin HLC already pushed; add `LocalPendingEvents(ctx, afterHLC)` filtering to the local device's events with `HLC > pushCursor`. Advance the push cursor only after `Push` succeeds, and keep remote-origin events out of push entirely.

**Actionable steps:**
1. Add `LocalPendingEvents(ctx, afterHLC)` with `WHERE device_id = <local device id> AND hlc > ? ORDER BY hlc, id`.
2. Store a push watermark per hub (e.g. a `hub_cursors` row keyed `'push:'+hubID`).
3. In `runSyncCycle`: read the push cursor, fetch only new local events, `Push`, then advance the push cursor to their max HLC.
4. Keep remote-origin events out of push entirely — the hub already holds them from their origin device.
5. Test that a second sync with no new local activity issues zero `ObjectExists`/`PutObject` calls.

**Example:**
```go
func (s *Store) LocalPendingEvents(ctx context.Context, afterHLC int64) ([]Event, error) {
  // SELECT ... FROM events WHERE device_id = <local> AND hlc > ? ORDER BY hlc, id
}

// sync.go
pushCur, _ := store.HubCursor(ctx, "push:"+hubID)
local, _ := store.LocalPendingEvents(ctx, pushCur)
hub.Push(ctx, local)
store.AdvanceHubCursor(ctx, "push:"+hubID, maxHLC(local))
```

**References:**
- [Cursor-based sync patterns (arcflow.me)](https://www.arcflow.me/patterns/cursor-based-sync) — per-device durable cursors bound transfer in both directions; a device should not re-send everything it already pushed.
- [Delta-sync checkpoints (cursa.app)](https://cursa.app/en/page/delta-sync-checkpoints-and-efficient-data-transfer) — send only deltas past the checkpoint, not the full set; partition logs by workspace to keep queries efficient.

### SYNC-05 — The event "hash chain" is a pointer to the previous content hash, not a folded running hash, and there is no signed per-device head — an untrusted hub can truncate/omit the newest events undetectably

**Severity / Effort / Category:** P2 / L / hardening

**Problem:** `content_hash = sha256(payload_json)` is independent of any previous entry, and `prev_event_hash` merely stores the previous event's `content_hash` as a verification pointer. This is *not* a folded/Merkle running hash (`entry_hash = H(prev_hash || content)`), and there is no single signed head/root committing the whole per-device log. The hub is explicitly zero-knowledge and untrusted, yet a malicious or buggy hub can withhold or truncate the newest events of a device (omission attack) and the client cannot detect it: there is no signed head HLC/seq to compare against, validation only fires when `prev_event_hash` is non-empty (`validatePrevEventHash` returns nil for empty), and verification only checks that each present event's immediate predecessor is present. Per-device chain validity does not prove the device's log is complete.

**Evidence:** Verified `internal/state/store.go:2290-2293` `ContentHash = sha256(payload only)`. `:2125-2172` `validatePrevEventHash`/`previousEventContentHash` compare `event.PrevEventHash` to the prior event's `content_hash` (a stored pointer, not a folded hash); the linkage lookup is by seq (`seq-1`) — so the predecessor is resolved by seq, not by hash value, and the real defect is the missing committed head + missing completeness check, not linkage ambiguity. `:2126-2128` `validatePrevEventHash` returns nil when `PrevEventHash == ''` (so a missing-newest-events truncation is undetectable). `:2176-2198` `eventSignaturePayload` signs `{content_hash, hlc, id, payload_json, prev_event_hash, type}` — nothing binds a running root or a per-device head count/seq high-water. `R2Hub.Pull` (`internal/hub/r2.go:110-152`) trusts `ListObjectsV2` results with no completeness/head check.

**Recommendation:** Make the chain a true running hash: `entry_hash = sha256(prev_entry_hash || content_hash || hlc || seq)`, sign `entry_hash`, and have each device publish a signed head (latest seq + entry_hash) that clients compare to detect omission/truncation. On receive, verify seq is contiguous per device (gap = omission). Optionally anchor heads cross-device so a hub cannot show inconsistent log views to different devices.

**Actionable steps:**
1. Add an `entry_hash` column; compute `entry_hash = H(prev_entry_hash || content_hash || hlc || seq)` and include it in the signature payload (currently `eventSignaturePayload` omits any running root).
2. On receive, recompute and verify `entry_hash`, and verify seq is contiguous per device (detect omission by seq gaps) — note the current `validatePrevEventHash` short-circuits on empty `prev_event_hash`, so add an explicit per-device seq-completeness check.
3. Publish a per-device signed head object (max seq + entry_hash) to the hub; the client fails the sync if `Pull` returns fewer than `head.seq` events for that device.
4. Add a `devstrap doctor` chain-verify that walks each device's chain and reports the first break by seq.
5. Test truncation: drop the newest 2 events of a device on the hub and assert the client detects the missing head (currently undetectable).

**Example:**
```go
func EntryHash(prev, contentHash string, hlc, seq int64) string {
  h := sha256.Sum256([]byte(prev+"|"+contentHash+"|"+strconv.FormatInt(hlc,10)+"|"+strconv.FormatInt(seq,10)))
  return "sha256:"+hex.EncodeToString(h[:])
}
// signed head: {device_id, max_seq, entry_hash, sig}
```

**References:**
- [Merkle chain for audit tamper-evidence (Microsoft agent-governance-toolkit)](https://microsoft.github.io/agent-governance-toolkit/adr/0017-merkle-chain-for-audit-tamper-evidence/) — each entry's hash includes the previous entry's hash; detects modification, deletion (chain break), and reordering — the `H(prev||content)` property the current scheme lacks.
- [Efficient Tamper-Evident Logging (Crosby & Wallach)](https://static.usenix.org/event/sec09/tech/full_papers/crosby.pdf) — a tamper-evident log must detect when an untrusted logger makes inconsistent claims over time; per-entry pointers without a committed head do not prove completeness/consistency.

### SYNC-06 — Tombstone GC and the per-peer cursor/delivery tables are dead code: unbounded growth and an unenforced GC-safety invariant

**Severity / Effort / Category:** P2 / M / scalability

**Problem:** `GCTombstones` is defined with a careful safety contract ("pass the minimum HLC that every approved sync cursor has already passed") but is never called from production code, and nothing computes that minimum. The `sync_cursors` (per-peer resume) and `event_delivery` (per-event application) tables from migration `00002` are never read or written outside migrations. Result: deleted namespace entries accumulate forever as `status='deleted'` rows, the events table is never trimmed, and the documented invariant that GC only runs below every peer's cursor is unimplemented — so when GC is eventually wired there is no safe floor, and a stale "add" from a lagging device could resurrect a GC'd path.

**Evidence:** Verified `internal/state/store.go:1137-1158` `GCTombstones`; its only callers are tests (`internal/sync/apply_test.go:233,241`) — grep finds no non-test invocation. The function's own doc comment states callers "must pass the minimum HLC that every approved sync cursor has already passed," but no `SafeGCFloor` computation exists. `sync_cursors` and `event_delivery` from migration `00002` (lines 20, 31) are referenced only by migrations and the schema-list test (`store_test.go:219`); `device_sync_state` *is* used but only for local clock stamping. `spec/12:324` confirms "sync ignores this table and replays from 0 (ARCH2-02)."

**Recommendation:** Compute a global safe-GC floor = `min(last_hlc_applied)` across all approved peers (and the hub retention floor from SYNC-02), persist per-peer progress in `sync_cursors`, and run `GCTombstones(floor)` on a schedule. Trim events below the snapshot floor introduced in SYNC-02. Until then, annotate the unused tables as explicitly deferred so they do not imply capability that does not exist.

**Actionable steps:**
1. Populate `sync_cursors(workspace_id, peer_id, last_hlc_applied)` when applying remote events, keyed by the event's origin device.
2. Add `SafeGCFloor(ctx)` = min over approved peers' `last_hlc_applied` (and the hub retention floor).
3. Schedule `GCTombstones(SafeGCFloor)` and an events-below-snapshot-floor prune in the sync cycle or a maintenance job.
4. Add a test: a tombstone is *not* GC'd while any approved peer's cursor is below its `tombstone_hlc`; an "add" below a surviving tombstone stays suppressed.
5. If deferring, annotate `sync_cursors`/`event_delivery` "reserved, not yet wired" in `spec/12` (it already partially does for `sync_cursors`; do the same for `event_delivery`).

**Example:**
```go
func (s *Store) SafeGCFloor(ctx context.Context) (int64, error) {
  // SELECT COALESCE(MIN(last_hlc_applied),0) FROM sync_cursors
  //   JOIN devices ON devices.id = peer_id WHERE devices.trust_state='approved'
}
floor, _ := store.SafeGCFloor(ctx)
store.GCTombstones(ctx, floor)
```

**References:**
- [Sync architecture ADR (tanstack-do-db-collection)](https://github.com/grrowl/tanstack-do-db-collection/blob/main/docs/adr/0001-sync-architecture.md) — deletes survive as tombstones until pruned below the floor; the retention floor is the GC boundary, derived from how far clients have advanced.
- [Delta-sync checkpoints (cursa.app)](https://cursa.app/en/page/delta-sync-checkpoints-and-efficient-data-transfer) — warns that the server deleting history before offline devices sync is a top failure mode; GC must respect the slowest cursor.

### SYNC-07 — MaxOpenConns(1) collapses WAL read concurrency: all reads serialize behind the single writer connection

**Severity / Effort / Category:** P3 / M / scalability

**Problem:** `Store.Open` forces a single shared connection (`SetMaxOpenConns(1)`/`SetMaxIdleConns(1)`) with `_txlock=immediate`. This reliably avoids `SQLITE_BUSY`, but it also discards WAL's defining benefit — many concurrent readers alongside one writer. Once the daemon (FSEvents reconciliation), `devstrap sync` (per-event `WithTx` loop), and agent/status reads contend, every read query queues behind in-flight write transactions on the one connection, and a long materialization or sync cycle starves interactive reads (`status`, `doctor`). This is a deliberate current tradeoff that becomes a real concern once the daemon ships; the recommended Go pattern is a split: one single-connection writer pool plus a separate read-only pool sized to CPU cores.

**Evidence:** Verified `internal/state/store.go:209-210` (`db.SetMaxOpenConns(1); db.SetMaxIdleConns(1)`) on the one `*sql.DB` used for both reads and writes; `sqliteDSN` sets `_txlock=immediate` (`store.go:244`). `ApplyEvents` wraps each event in its own `WithTx` (`events.go:191`), so a multi-hundred-event pull holds the single connection across the whole loop's worth of short transactions, blocking concurrent reads on the same handle.

**Recommendation:** Open two handles to the same file: a writer DB with `MaxOpenConns=1` + `_txlock=immediate`, and a read-only DB (`mode=ro`, default deferred txlock) with a bounded pool (~`runtime.NumCPU()`). Route reads to the reader and all writes/transactions to the writer. Open the writer first so it can establish WAL mode. Keep write transactions short.

**Actionable steps:**
1. Add a second `*sql.DB` opened with `file:...?mode=ro` and a pool of ~`runtime.NumCPU()`; call `Ping` on the writer first to force WAL setup.
2. Expose Store read methods through the reader handle; keep `WithTx`/`Insert` on the writer handle.
3. Verify `foreign_keys` + WAL-supporting pragmas on both handles (pragmas are per-connection).
4. Benchmark `status`/`doctor` latency during a large `sync` materialization before/after the split.
5. Document the read/write split in `spec/12` alongside the existing WAL notes.

**Example:**
```go
writeDB, _ := sql.Open("sqlite", dsn(path, "immediate", false))
writeDB.SetMaxOpenConns(1)
writeDB.Ping() // force WAL setup before any read-only handle opens
readDB, _ := sql.Open("sqlite", dsn(path, "" /*deferred*/, true /*mode=ro*/))
readDB.SetMaxOpenConns(runtime.NumCPU())
return &Store{w: writeDB, r: readDB, ...}
```

**References:**
- [go-sqlite3 maintainer guidance (issue #1179)](https://github.com/mattn/go-sqlite3/issues/1179) — two `sql.DB` pools: a read-write pool with `BEGIN IMMEDIATE` throttled to `MaxOpenConns=1`, and a separate read-only pool with default deferred `BEGIN` sized to any number of connections; open the writer first so the reader cannot block WAL conversion.
- [Your SQLite connection pool might be ruining your write performance (emschwartz.me)](https://emschwartz.me/psa-your-sqlite-connection-pool-might-be-ruining-your-write-performance/) — the fix is a single writer connection with writes queued at the application level plus a separate read-only pool for concurrent reads, mirroring SQLite's many-readers/one-writer model.

### SYNC-08 — Workspace-singleton index hard-blocks the multi-workspace future, and workspace-id re-stamping makes events portable across workspaces with no signed namespace binding

**Severity / Effort / Category:** P3 / M / design-improvement

**Problem:** Migration `00006` enforces a singleton workspace via a unique expression index on `((1))`, and `ApplyEvents` blanks and re-stamps `event.WorkspaceID` to the local workspace because the signature payload deliberately excludes `workspace_id`. Two consequences: (1) the documented multi-workspace / multi-tenant `SCALE-*` direction has no migration path — adding a second workspace row is impossible without dropping the index, and all queries assume a single implicit workspace; (2) because signatures do not bind `workspace_id` and apply re-stamps it, an event object placed under the wrong workspace prefix on a shared/misconfigured hub would be accepted and absorbed into the local namespace. R2 keying is per-workspace prefix today, so this is latent, but the data model bakes in "events are workspace-agnostic," which fights the stated future and weakens tenant isolation.

**Evidence:** Verified `internal/state/migrations/00006_workspace_singleton.sql` (`CREATE UNIQUE INDEX idx_workspaces_singleton ON workspaces((1))`). `internal/sync/events.go:196-199` ("Re-stamp the workspace_id with the local workspace ... The signature payload does not include workspace_id, so this does not invalidate it") and `events.go:197` sets `event.WorkspaceID = ''`. `internal/state/store.go:2176-2183` `eventSignaturePayload` omits `WorkspaceID`. `spec/12` calls the singleton a Phase-0 invariant; `00_START_HERE` lists `SCALE-*` multi-user as the future direction. `R2Hub` keys per-workspace prefix (`r2.go:68-82`), which is the only thing localizing events today.

**Recommendation:** Decide and document whether a stable logical namespace id is part of the signed identity. For tenant isolation, bind a shared `logical_namespace_id` into the signature and verify it on apply (reject foreign-namespace events) instead of silently re-stamping. Plan a migration that relaxes the singleton index to a per-device-provisioned shared logical workspace id so the same namespace can exist on N devices without the `(1)` constraint.

**Actionable steps:**
1. Introduce a `logical_namespace_id` shared across a workspace's devices, distinct from the per-device workspace row id.
2. Include `logical_namespace_id` in `eventSignaturePayload` and verify it on apply; quarantine events for a different namespace instead of re-stamping.
3. Replace the singleton index with `UNIQUE(logical_namespace_id)` provisioning so device pairing sets the same id rather than forbidding a second row.
4. Backfill existing rows with the current workspace's logical id in a migration with a tested Down.
5. Add a test: an event signed for namespace X is rejected when applied against a store provisioned for namespace Y.

**Example:**
```go
type eventSignaturePayload struct {
  NamespaceID   string `json:"namespace_id"` // shared logical id, now signed
  ContentHash   string `json:"content_hash"`
  HLC           int64  `json:"hlc"`
  // ...
}
// apply: if event.NamespaceID != local.NamespaceID { quarantine }
```

**References:**
- [Logical Physical Clocks / HLC (Kulkarni & Demirbas)](https://cse.buffalo.edu/tech-reports/2014-04.pdf) — HLC's happened-before guarantee is only meaningful within a single causal namespace; mixing logs from distinct namespaces without binding the namespace identity breaks the ordering the system relies on.
- [Delta-sync checkpoints (cursa.app)](https://cursa.app/en/page/delta-sync-checkpoints-and-efficient-data-transfer) — for multi-tenant scopes, partition logs by tenant/workspace and treat the tenant id as a first-class, verified dimension rather than re-stamping it on receipt.

---
## Cloud Hub, Backend & Scalability

The R2/S3 hub is the planned zero-knowledge backend for namespace events and encrypted blobs, but it is still pre-production: there is no real AWS SDK client wired in, and the file-backed `--hub-file` spike is the only live path. That early state is exactly why this section matters — the data-path primitives (conditional writes, retries, compaction, GC, cursors, metering, quotas, and durability) should be designed correctly *before* the SDK is wired against live R2, because R2 bills on requests, not storage, and several of the gaps below become silent correctness or runaway-cost problems the moment the backend goes live. The findings range from a low-effort hot-path waste (HUB-09) to structural scalability gaps with no current reclamation or compaction capability (HUB-11, HUB-12).

### HUB-09 — Redundant ObjectExists-before-conditional-Put doubles request cost and reintroduces a TOCTOU window the conditional put already eliminates

**Severity / Effort / Category:** P2 / S / design-improvement

**Problem:** `R2Hub.Push` and `PutBlob` each perform an `ObjectExists` (HEAD) call and only then a `PutObject` with `If-None-Match`. The conditional put alone is already atomic and idempotent, so the extra HEAD is pure waste: it doubles the per-event request count (a Class B HEAD per object, and the provisioning guide A.5 warns that R2 *requests*, not storage, are "the first bill"), and it opens a check-then-act race where a concurrent writer can insert the object between the HEAD and the PUT. Worse, the code never classifies the 412 Precondition Failed (R2 error 10031) that the conditional PUT returns when the object already exists, so once a real `S3Client` is wired those become hard sync failures instead of the intended idempotent no-op.

**Evidence:** `internal/hub/r2.go:92-105` (Push: `exists, err := h.S3.ObjectExists(ctx, key)` at line 92, then `h.S3.PutObject(ctx, key, raw, true)` at line 103) and `internal/hub/r2.go:159-172` (PutBlob: `ObjectExists` at line 159, `PutObject(...,true)` at line 170). The `S3Client.PutObject` contract at `r2.go:38-52` returns only `error` with no typed 412 distinction; the `memS3` double returns a generic `fmt.Errorf("condition failed: object already exists")` (`internal/hub/mems3_test.go:29-32`), so no caller can treat already-exists as success today.

**Recommendation:** Drop the `ObjectExists` pre-check on the write path. Rely solely on the conditional PUT and classify its result: treat 412 PreconditionFailed (R2 error code 10031 — for a content-addressed/immutable blob it is definitionally the identical ciphertext) as idempotent success. Reserve `ObjectExists`/HEAD for read-side existence checks only. This halves Class B request volume on the hot push path and removes the race.

**Actionable steps:**
1. Change `S3Client.PutObject` to surface a typed error (e.g. `ErrPreconditionFailed`), or have `R2Hub` classify the SDK `ResponseError` HTTP status 412 (R2 code 10031 PreconditionFailed).
2. In `Push`, call `PutObject(ctx,key,raw,true)` directly; on `ErrPreconditionFailed` `continue` (idempotent no-op).
3. In `PutBlob`, do the same — `PutObject(...,true)` and swallow 412 as a dedup hit, dropping the `ObjectExists` call.
4. Update the `memS3` double to return the typed precondition error and add a conformance test proving idempotent re-push is a no-op without a HEAD.
5. Document that for content-addressed blobs a 412 is definitionally a dedup hit (same sha256 = same ciphertext).

**Example:**
```go
// before: HEAD then conditional PUT (2 ops, racy)
// after:
if err := h.S3.PutObject(ctx, key, raw, true); err != nil {
    if errors.Is(err, ErrPreconditionFailed) { continue } // idempotent dedup hit (R2 10031)
    return fmt.Errorf("put event %s: %w", event.ID, err)
}
```

**References:**
- https://developers.cloudflare.com/r2/api/error-codes/ — R2 error 10031 PreconditionFailed (HTTP 412): conditional headers were not satisfied; the conditional PUT is itself the atomic guard, so a separate HEAD is redundant.
- https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html — `If-None-Match` uploads only if the key does not exist and returns 412 otherwise; the conditional write is atomic, no read-before-write needed.
- https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-requests.html — conditional writes incur no extra charge beyond the request itself, so the extra HEAD is avoidable cost with no benefit.

### HUB-10 — R2/S3 backend has no retry, backoff, or error classification

**Severity / Effort / Category:** P1 / M / reliability

**Problem:** Every S3 operation in `R2Hub` is a single-shot call whose error is wrapped and returned straight up. There is no exponential backoff with jitter, no retry budget, no distinction between throttling/transient/terminal errors, and crucially no production `S3Client` at all (`go.mod` has no `aws-sdk-go-v2`; the only retry-adjacent dep is `github.com/sethvargo/go-retry v0.3.0 // indirect`, which is unused in `internal/` and `cmd/`). R2 rate-limits and returns 429 TooManyRequests / 503 SlowDown under load; a single such response during a multi-page Pull or multi-event Push will fail the entire `devstrap sync` cycle, and because Pull re-lists from the cursor each attempt it re-pays the full Class A listing cost on retry. This is forward-looking: the R2 path is not yet production-wired (`sync.go:25-27` hard-requires `--hub-file`), so this must be designed in when the SDK is added.

**Evidence:** `internal/hub/r2.go:84-152` — Push/Pull call `h.S3.*` once and `return ... err` with no retry loop; Pull's pagination loop (`r2.go:116-141`) has no backoff between pages. `grep -niE 'aws-sdk|retry|backoff|x/time/rate' go.mod` returns only `sethvargo/go-retry v0.3.0 // indirect`, referenced nowhere in `internal/` or `cmd/`. `sync.go:25-27` returns "until the production hub exists" for an empty `--hub-file`, confirming the R2 path is unwired.

**Recommendation:** When wiring the real AWS SDK v2 S3 client against the R2 endpoint, configure the standard retryer (exponential backoff + full jitter, capped max attempts, throttle-aware token-bucket rate limiter) and add `R2Hub`-level error classification so throttling/transient errors retry and terminal errors (auth, malformed, 412 precondition) fail fast. Add a fault-injecting `memS3` variant to the conformance suite so retry semantics are tested before any live wiring.

**Actionable steps:**
1. Add `github.com/aws/aws-sdk-go-v2` and construct the S3 client with `retry.NewStandard`, a capped `MaxAttempts`, and a token-bucket `RateLimiter` so retries cannot create a runaway billing loop.
2. Classify R2 responses: 429 TooManyRequests / 503 SlowDown → throttling (~1s base delay), 500/connection-reset → transient (50ms base), 4xx auth/precondition → terminal (no retry).
3. Honor `ctx` cancellation/deadline through to the HTTP transport so a stuck Pull is bounded.
4. Add a `memS3` variant that returns injected throttle/transient errors on the Nth call and assert Push/Pull recover (and that terminal errors do not retry).
5. Cap Pull memory: stream-apply events per page instead of accumulating all of `out` before sorting (ties to HUB-11).

**Example:**
```go
cfg, _ := config.LoadDefaultConfig(ctx,
  config.WithRegion("auto"),
  config.WithRetryer(func() aws.Retryer {
    return retry.NewStandard(func(o *retry.StandardOptions){
      o.MaxAttempts = 5
      o.RateLimiter = ratelimit.NewTokenRateLimit(500)
    })
  }))
s3c := s3.NewFromConfig(cfg, func(o *s3.Options){ o.BaseEndpoint = aws.String(r2Endpoint); o.UsePathStyle = true })
```

**References:**
- https://docs.aws.amazon.com/sdkref/latest/guide/feature-retry-behavior.html — standard mode classifies errors as transient/throttling/non-retryable and uses exponential backoff with full jitter (50ms transient, 1000ms throttling base, 20s cap) to avoid retry storms.
- https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-retries-timeouts.html — `retry.NewStandard` with `NewTokenRateLimit` (token-bucket retry quota) prevents runaway workloads and inflated billing; use context deadlines to bound stuck operations.
- https://developers.cloudflare.com/r2/api/error-codes/ — R2 returns InternalError/SlowDown classes the doc marks as retryable after backoff.

### HUB-11 — No event-log compaction or working-snapshot exchange for R2

**Severity / Effort / Category:** P1 / L / scalability

**Problem:** The Hub contract documents `ErrSnapshotRequired` and a retention horizon, and the provisioning guide reserves `workspaces/<id>/snapshots/<hlc-padded>.json.age`, but none of it is implemented for R2. `R2Hub` has no `RetentionHLC` field and `Pull` never returns `ErrSnapshotRequired` — it unconditionally lists the entire `events/` prefix from the cursor and GETs every object. The log is append-only with one object per event and is never compacted, so a long-lived workspace accumulates unbounded objects; each sync pays a `ListObjectsV2` (Class A, the expensive op per A.5) per 1000 events plus a GET per new event, and Pull buffers all results in a single slice before sorting. There is no way to bootstrap a new device from a compact snapshot instead of replaying full history. The `FileHub` honors `RetentionHLC` (`internal/sync/hub.go:92-94`) but the real backend silently does not, so the snapshot path is dead code in production.

**Evidence:** `internal/hub/r2.go:110-152` Pull — no retention check, no `ErrSnapshotRequired` return, `out` accumulates all events (`r2.go:115,134`) then sorts. `R2Hub` struct (`r2.go:56-59`) has only S3+WorkspaceID, no `RetentionHLC`. Contrast `FileHub.Pull` (`internal/sync/hub.go:91-94`) which returns `ErrSnapshotRequired` when `afterHLC < RetentionHLC`. `spec/19_CLOUD_PROVISIONING_GUIDE.md:84` reserves the `snapshots/` key and line 51 lists "snapshots/backups, retention discipline" as a requirement, but `grep -rn snapshot internal/hub` finds no snapshot read/write code.

**Recommendation:** Implement periodic event-log compaction into signed, age-encrypted snapshot objects plus a retention horizon. After compaction, `R2Hub.Pull` returns `ErrSnapshotRequired` when the cursor predates the horizon, and a SnapshotExport/Import path lets devices bootstrap from `snapshots/<hlc>.json.age` then resume incremental pulls. Use R2 native object-lifecycle rules (delete-by-age on the `events/` prefix) as the low-effort mechanism to expire cold event segments once a snapshot covers them, instead of (or alongside) per-object deletes.

**Actionable steps:**
1. Add a `Snapshot(ctx)` Hub method (or a separate compactor) that materializes current namespace state into `snapshots/<hlc-padded>.json.age` (signed + age-encrypted like events).
2. Track a `RetentionHLC` marker object; once a snapshot covers events below it, expire those event objects — an R2 lifecycle rule on the `events/<oldHLC>` prefix is the simplest native mechanism.
3. Make `R2Hub.Pull` read the retention marker and return `dssync.ErrSnapshotRequired` when `afterHLC < retention`, mirroring `FileHub`.
4. Wire the CLI: on `ErrSnapshotRequired`, fetch the latest snapshot, import it, set the cursor, then resume Pull — `sync.go:70-72` currently just surfaces the error as a network failure.
5. Stream-apply per page so Pull memory is O(page), not O(history).

**Example:**
```go
// after compaction the cold tail is one object, not N:
key := fmt.Sprintf("workspaces/%s/snapshots/%020d.json.age", h.WorkspaceID, horizonHLC)
// Pull gate:
if afterHLC < h.retentionHLC() { return nil, dssync.ErrSnapshotRequired }
```

**References:**
- https://developers.cloudflare.com/r2/buckets/object-lifecycles/ — R2 object-lifecycle rules delete objects after N days by prefix; an object is no longer billable once deleted, the native way to bound an append-only log's storage.
- https://blog.cloudflare.com/introducing-object-lifecycle-management-for-cloudflare-r2/ — up to 1,000 lifecycle rules to expire objects by age/prefix; the recommended way to stop unbounded object growth driving storage cost.
- https://learn.microsoft.com/en-us/azure/architecture/guide/multitenant/approaches/storage-data — unbounded per-tenant operation counts in shared object storage trigger throttling that affects all tenants; cap list/scan growth via snapshots and segments.

### HUB-12 — Hub-side blob/event GC is structurally impossible (S3Client has no Delete)

**Severity / Effort / Category:** P1 / L / scalability

**Problem:** `gcUnreferencedBlobs` only reclaims the *local* on-disk blob cache (`~/.devstrap/blobs`); it never touches the hub. The `S3Client` interface has no `DeleteObject` method and the Hub interface has no `DeleteBlob`, so the R2 backend physically cannot garbage-collect anything. Consequences: (1) every draft/env blob and every event ever pushed lingers on R2 indefinitely → unbounded storage cost with no reclamation, contradicting the guide's claim (A.4) that GC of unreferenced `age_blob` objects "happen client-side" (client-side GC reaches only the local cache, never the hub). (2) After device revoke, `rewrapBlobsOnRevoke` re-encrypts blobs to the reduced recipient set and repoints local refs, but the OLD ciphertext is never deleted from the hub, so a revoked device that retains hub read credentials can still fetch the stale blob. Note: `blob_gc.go:18-24` already acknowledges this residual-ciphertext exposure as an accepted limitation (age has no native revocation; secrets are flagged `needs_rotation`), so the revocation angle is a known trade-off — the core new gap is that the hub has no reclamation capability at all.

**Evidence:** `internal/cli/blob_gc.go:81` — `gcUnreferencedBlobs` operates on `filepath.Join(paths.Home, "blobs")` (local only); no hub handle is passed in. `grep -rn DeleteObject\|DeleteBlob internal/` returns nothing — the `S3Client` interface (`r2.go:38-52`) exposes only `PutObject`/`GetObject`/`ObjectExists`/`ListObjectsV2`, and the Hub interface (`internal/sync/hub.go:53-58`) has Push/Pull/PutBlob/GetBlob and no delete. `internal/cli/blob_gc.go:54-60` `rewrapBlobsOnRevoke` writes the new blob and `UpdateBlobRef` but never deletes old ciphertext from any hub; the comment at `blob_gc.go:18-24` documents the residual-exposure limitation as accepted.

**Recommendation:** Add `DeleteObject` to `S3Client` and `DeleteBlob`/`DeleteEvent` to the Hub interface, then implement a mark-and-sweep hub GC with a safety grace window: derive the live blob set from the applied event log and delete hub objects that are both unreferenced AND older than a grace TTL, to avoid racing a concurrent push. For time-based expiry (old event segments) prefer R2 native lifecycle rules over per-object deletes. On revoke, after rewrap succeeds, delete the superseded ciphertext objects from the hub so revoked recipients lose access — closing the residual-exposure gap the code currently accepts.

**Actionable steps:**
1. Add `DeleteObject(ctx,key)` to `S3Client` and `DeleteBlob(ctx,sha256)`/`DeleteEvent` to Hub.
2. Implement hub GC: derive the live sha256 set from the namespace map, list `workspaces/<id>/blobs/`, delete objects not in the set AND whose `LastModified` is older than a grace TTL (e.g. 24h) to dodge in-flight pushes.
3. On device revoke, after `rewrapBlobsOnRevoke` succeeds, delete the old `blobKey(oldRef)` from the hub so revoked recipients lose access to that ciphertext (upgrading the accepted limitation at `blob_gc.go:18-24`).
4. Use an epoch/undelete-marker barrier so a blob written during the sweep is never collected (concurrent-deletion safety).
5. Add a conformance test: concurrent `PutBlob` during GC must not lose the just-written blob; revoke must remove old ciphertext.

**Example:**
```go
type S3Client interface {
    // ...existing...
    DeleteObject(ctx context.Context, key string) error
}
// hub GC with grace window:
if !live[sha] && obj.LastModified.Before(now.Add(-graceTTL)) {
    _ = h.S3.DeleteObject(ctx, h.blobKey(sha))
}
```

**References:**
- https://developers.cloudflare.com/sandbox/guides/backup-restore/ — Cloudflare's own guidance: expired backups remain in your bucket and keep consuming storage until you explicitly delete them or configure cleanup; use delete-by-prefix or lifecycle rules — exactly the reclamation primitive the hub lacks.
- https://www.usenix.org/system/files/conference/fast13/fast13-final91.pdf — "Concurrent Deletion in a Distributed Content-Addressable Storage System": epochs + undelete markers create a boundary so deletion runs safely concurrently with writes; a naive ref-count-then-sweep races writers.
- https://developers.cloudflare.com/r2/buckets/object-lifecycles/ — native delete-by-age lifecycle rules for time-expirable objects (event segments), complementing reference-based blob GC.

### HUB-13 — HLC-only sync cursor can drop a legitimately-new event that shares an HLC value with another device's already-applied event

**Severity / Effort / Category:** P2 / M / bug-risk

**Problem:** The pull cursor is a single int64 HLC, and both Pull implementations filter with strict `event.HLC > afterHLC`, advancing the cursor to the max applied HLC. The packed HLC (physical_ms, logical) is NOT unique across devices — two devices can independently mint the same packed value. Failure sequence: device A's event at HLC=H is pulled and applied in pull #1 (cursor advances to H); a device-B event that also stamped HLC=H is uploaded to the hub afterward; on pull #2 it is filtered out by the strict `> H` test and never delivered to that device — silent loss of a namespace event. The deterministic sort tie-breaks on `(HLC, device_id, id)` precisely because HLC collisions are expected, yet the cursor collapses that to a single counter. Probability is low (requires an exact cross-device packed-HLC collision AND staggered arrival across two pulls), but for a product whose whole promise is "the same tree on every device" a silent-drop class is worth closing — and the fix is cheap because `ApplyEvents` is already idempotent on event ID.

**Evidence:** `internal/cli/sync.go:64` (`cursor, err := store.HubCursor(...)` returns int64 per `internal/state/store.go:2340`) and `sync.go:79-82` (`store.AdvanceHubCursor(ctx, hubID, appliedMaxHLC)`). `internal/hub/r2.go:133` `if event.HLC > afterHLC` (strict) and `internal/sync/hub.go:104` (FileHub, same strict filter). The sort tie-breaks on `(HLC, device_id, id)` at `r2.go:142-150` and `hub.go:211-220`, proving same-HLC collisions across devices are an expected case. `ApplyEvents` idempotency-on-ID is at `internal/sync/events.go:205-211` (`InsertEvent` returns `inserted` false on duplicate), so an overlap re-pull is safe.

**Recommendation:** Make the cursor a composite `(HLC, device_id, seq/id)` or resume with an inclusive overlap (`>=` the boundary HLC) and rely on idempotent `ApplyEvents` to dedup the boundary. Because re-applying an already-seen event is a no-op, a small overlap window eliminates the drop at zero correctness cost.

**Actionable steps:**
1. Store the cursor as the last fully-applied `(HLC, device_id, id)` tuple, not just max HLC — or keep the int64 HLC but pull with an inclusive `>=` boundary.
2. In Pull, start-after the composite key and include events with `HLC >= afterHLC`, relying on idempotent `InsertEvent` to dedup the boundary.
3. Add a regression test: two devices stamp the same packed HLC; a third device must receive both after sequential pulls where the first event was applied before the second arrived.
4. Update the Hub doc comment (`internal/sync/hub.go:40-42`) which currently promises "HLC strictly greater than afterHLC" — that contract is the source of the drop.
5. Confirm `FileHub` and `R2Hub` share the corrected boundary semantics so the spike and production agree.

**Example:**
```go
// drop strict filter; rely on idempotent apply + composite resume
startAfter := compositeKey(afterHLC, afterDevice, afterSeq)
for _, e := range listed {
    if e.HLC > afterHLC || (e.HLC == afterHLC && keyAfter(e, afterDevice, afterSeq)) {
        out = append(out, e) // InsertEvent dedups by e.ID
    }
}
```

**References:**
- https://jaredforsyth.com/posts/hybrid-logical-clocks/ — HLC timestamps are not globally unique across nodes; a resume cursor or merge boundary must include node identity, not just the timestamp, or concurrent same-timestamp events are lost.
- https://www.usenix.org/system/files/conference/fast13/fast13-final91.pdf — correctness in append-only distributed logs requires a boundary that captures full event identity; collapsing identity to a coarse counter creates lost-update races.

### HUB-14 — The hub data path emits no metrics, traces, or op/byte counters

**Severity / Effort / Category:** P2 / M / devex

**Problem:** There is no instrumentation anywhere in `internal/hub` or the sync cycle: no count of `ListObjectsV2`/`GetObject`/`PutObject` calls, no retry counter, no bytes-up/down, no per-operation latency, and no per-sync summary beyond a printed "pushed N, pulled M". The provisioning guide A.5 explicitly warns that Class A operations (`ListObjectsV2`) can dominate the bill, yet nothing counts them. When the hub starts throttling or the bill spikes, there is no signal to diagnose which operation, workspace, or sync loop is responsible — and the `XP-*`/`SCALE-*` multi-device/multi-tenant direction makes this strictly worse.

**Evidence:** `grep -rniE 'prometheus|otel|opentelemetry|metric|histogram|IncOp' internal/hub internal/sync internal/cli/sync.go` returns only unrelated `hlc.go` variable names (`remoteLogical` etc.), no telemetry. `internal/cli/sync.go:106-108` emits only a human string. `R2Hub` methods (`r2.go:84-185`) increment no counters and record no timings. The only observability is `logging.Logger` warnings on isolated failures.

**Recommendation:** Add a small metrics/telemetry seam to the Hub: count S3 operations by class (List=A, Get/Put=B), retries, bytes in/out, and per-operation latency, surfaced via `slog` structured fields and an optional OpenTelemetry exporter. Print a cost-relevant per-sync summary (list/get/put op counts) so operators can correlate with the R2 bill and feed the `SCALE-*` control plane.

**Actionable steps:**
1. Define a `hubmetrics` interface (`IncOp(class, op)`, `AddBytes(dir, n)`, `ObserveLatency(op, d)`, `IncRetry(op)`) and thread it through `R2Hub`.
2. Wrap each S3 call to record op class (List=A, Get/Put=B), latency, and byte counts.
3. Emit a structured per-sync summary: `classA_ops`, `classB_ops`, `bytes_up`, `bytes_down`, `retries` — log it and expose via `devstrap status`/`doctor`.
4. Add an optional OTel/Prometheus exporter behind a build tag for the hosted control plane.
5. Add a threshold hook so a sync exceeding an op budget warns loudly (ties into HUB-15).

**Example:**
```go
func (h R2Hub) listEvents(ctx, prefix, after string) (...){
  start := time.Now()
  keys, next, err := h.S3.ListObjectsV2(ctx, prefix, after, 1000)
  h.m.IncOp("A", "list"); h.m.ObserveLatency("list", time.Since(start))
  return keys, next, err
}
```

**References:**
- https://learn.microsoft.com/en-us/azure/architecture/guide/multitenant/approaches/storage-data — it is important to monitor for throttled requests and meter per-tenant consumption via built-in or custom application metrics.
- https://docs.aws.amazon.com/sdkref/latest/guide/feature-retry-behavior.html — retry-quota depletion and throttling are observable signals; without counting retries/throttles you cannot tell a transient blip from sustained overload.

### HUB-15 — No cost controls, quotas, or rate limiting

**Severity / Effort / Category:** P2 / M / hardening

**Problem:** The hub has no client-side rate limiter, no per-workspace object/byte/op budget, and no quota enforcement. `ListObjectsV2` is hard-coded to `maxKeys=1000` with no backoff between pages, and `runSyncCycle` (the reusable loop body reused by the `XP-02` run-loop) has no floor on op spend. The `SCALE-*` section promises multi-tenant hosting on a shared R2 bucket separated only by key prefix, but there is no metering hook, no per-tenant cap, and no noisy-neighbor protection — one busy or buggy workspace can exhaust the shared bucket's request capacity (R2 throttles per bucket) and degrade everyone, with no enforced spending limit. Overlaps with HUB-10 (token-bucket retryer) and HUB-14 (metering); design them as one seam.

**Evidence:** `internal/hub/r2.go:117` `ListObjectsV2(ctx, h.eventsPrefix(), startAfter, 1000)` fixed page size, loop `r2.go:116-141` with no inter-page delay. No rate limiter or budget anywhere in `internal/hub` (grep clean for `x/time/rate`). `spec/14_MVP_ROADMAP_AND_BACKLOG.md:525-534` documents `SCALE-*` multi-tenant scaling as "future direction, documented not built" with no metering primitive. `runSyncCycle` (`internal/cli/sync.go:40`) is the reusable loop body with no op accounting.

**Recommendation:** Add a client-side token-bucket rate limiter and a per-workspace op/byte budget to the hub, with a configurable ceiling that backs off (or fails closed) when exceeded. Design the metering seam now (shared with HUB-14) so the `SCALE-*` control plane can aggregate per-tenant usage and apply tier-aware quotas before the SaaS build, not after.

**Actionable steps:**
1. Wrap `S3Client` in a rate-limited decorator (`golang.org/x/time/rate` token bucket) sized to stay inside R2 free-tier op rates.
2. Add a per-sync op budget (max list/get/put) that aborts with a clear error and backoff hint when exceeded.
3. Emit per-workspace usage (ops, bytes, storage estimate) via the HUB-14 metrics so the control plane can meter and bill.
4. For `SCALE-*`, document burst-vs-sustained limits and a weighted-fair-queue/tier model so free tenants cannot starve paid ones.
5. Add a `devstrap doctor` check that surfaces projected monthly R2 op cost from recent sync op counts.

**Example:**
```go
// rate-limited S3 decorator
type throttledS3 struct{ inner S3Client; lim *rate.Limiter }
func (t throttledS3) ListObjectsV2(ctx context.Context, p, a string, n int) ([]string, string, error) {
    if err := t.lim.Wait(ctx); err != nil { return nil, "", err }
    return t.inner.ListObjectsV2(ctx, p, a, n)
}
```

**References:**
- https://learn.microsoft.com/en-us/azure/architecture/guide/multitenant/approaches/storage-data — multi-tenant object storage needs per-tenant throttling/quotas and noisy-neighbor isolation; monitor for throttled requests and cap per-tenant consumption.
- https://docs.min.io/aistor/administration/qos/ — per-bucket/prefix QoS (rps, concurrency, storage quota) returning 429 on breach is the standard primitive to stop one tenant's load overloading shared object storage.

### HUB-16 — Hub availability/integrity at rest is undefended (R2 has no versioning/Object-Lock and no backup/replication runbook)

**Severity / Effort / Category:** P2 / M / reliability (rescoped)

**Problem:** Encryption gives confidentiality but, as the guide notes (A.2/A.4), a bucket-wide or compromised scoped credential can delete or withhold ciphertext it cannot decrypt. The in-code detection one might expect largely already exists: pulled remote events are run through a per-device `prev_event_hash` chain (`validatePrevEventHash` / `ErrEventHashChain`) so a dropped or out-of-order event is detected and refused-with-a-conflict rather than silently applied, and Ed25519 signatures are verified (`verifyEventSignature`) — currently fail-open, which is already a tracked `SECU-03` gap. What is genuinely undefended is availability-at-rest: there is no backup, no replication, and no delete-detection runbook. Critically, R2 does NOT support S3-style object versioning or Object Lock (Cloudflare has no GA versioning feature), so "enable bucket versioning + Object Lock" is not applicable to R2 — the correct defenses are an R2 lifecycle/retention discipline plus cross-bucket or cross-cloud replication of the signed event/snapshot stream.

**Evidence:** Gap detection and signature verification already exist: `internal/state/store.go:2090-2098` `InsertEvent` calls `validatePrevEventHash` AND `verifyEventSignature`; `validatePrevEventHash` (`store.go:2125-2174`) detects a missing prior event per `(device_id, seq)` and returns `ErrEventHashChain`, which `ApplyEvents` records as an `event_hash_chain_break` conflict and refuses to apply (`internal/sync/events.go:218-229`). Signature verify is at `store.go:2200-2279` (fail-open per `SECU-03`). What is missing: `internal/hub/r2.go:110-152` Pull does no backup/replication and there is no provisioning step for durability. External check: R2 has no native object versioning/Object Lock (Cloudflare community + rclone reports, Oct 2023+), so S3-versioning guidance does not transfer; R2 does support object-lifecycle rules. `spec/19:51` lists "snapshots/backups, retention discipline" as a requirement with no implementing code/runbook.

**Recommendation:** Treat hub durability as a provisioning + backup concern, not bucket versioning (which R2 lacks). Add a periodic signed-snapshot + event export replicated to a second R2 bucket or a different cloud (e.g. rclone to GCS/S3 ARCHIVE) as the disaster-recovery primitive, document an RPO target and a point-in-time restore runbook keyed off snapshot HLC, and add a `devstrap doctor` check that the backup job is healthy. Keep the existing hash-chain gap detection and (once enrollment lands) flip signature verification to fail-closed per `SECU-03` — do not re-implement these, they exist.

**Actionable steps:**
1. Document that R2 has no native versioning/Object Lock; choose cross-bucket or cross-cloud replication (rclone scheduled job, or R2 event-notification-driven copy) of the signed `events/` and `snapshots/` prefixes as the durability layer.
2. Add a periodic signed snapshot export to a second bucket/account as a recoverable backup with a documented RPO; ties into the HUB-11 snapshot work.
3. Add a `devstrap doctor` / provisioning checklist item that verifies the backup/replication job ran recently and the `snapshots/` prefix is non-empty.
4. Keep the existing per-device `prev_event_hash` gap detection (`store.go:2125-2174`); surface an `event_hash_chain_break` conflict to the user as "possible hub data loss" in `doctor` rather than burying it as a generic conflict.
5. Once device enrollment exists, flip `verifyEventSignature` to fail-closed (`SECU-03`) — already tracked, not new.

**Example:**
```bash
# durability via replication, since R2 has no versioning:
# rclone sync r2:devstrap-hub/workspaces/<id>/events  gcs-archive:devstrap-backup/<id>/events
# doctor check:
if backupAgeHours > rpoHours { warn("hub backup stale: last replicate %dh ago (RPO %dh)", backupAgeHours, rpoHours) }
```

**References:**
- https://zenn.dev/catnose99/articles/c0c710f98a0be8?locale=en — as of Oct 2023, R2 does not support versioning like S3 or GCS, so you cannot restore accidentally deleted/overwritten objects; the documented workaround is scheduled rclone replication to a backup bucket.
- https://developers.cloudflare.com/sandbox/guides/backup-restore/ — Cloudflare's own pattern: explicit delete + lifecycle cleanup and a second buffer/replica to prevent race-driven data loss; R2 needs application-level backup, not Object Lock.
- https://cloud.google.com/storage/docs/protection-backup-recovery-overview — soft delete, object versioning, retention/bucket-lock, and cross-region replication are the recommended object-store recovery primitives; on R2 only the replication/lifecycle subset is available.

---
## Git Materialization, Worktrees & Agents

The git materialization, worktree, and agent layers are the load-bearing core of DevStrap's "the whole tree is really present on disk" promise, and they are largely well-built — atomic clone promotion, network-retry classification, and base-SHA gating all show care. The findings below cluster around three themes: honesty under partial failure (empty checkouts, retry idempotency, opaque batch failures), the gap between an advisory command denylist and a real OS-enforced agent sandbox, and forge/worktree coverage that quietly favors GitHub and ancestry-merge workflows. None invalidate the architecture; each closes a seam where eager clone-everything across a fleet turns an edge case into a recurring failure mode.

### GIT-01 — Eager materialization records "available/clean" without verifying a non-empty checkout

**Severity / Effort / Category:** P2 / S / bug-risk

**Problem:** The eager materialization path clones with `git clone --filter=blob:none -- <remote> <dest>` and then records the project as `available` with whatever `DirtyState` reports, without ever asserting the checkout actually produced a populated working tree. In the rare-but-real case where the remote's advertised HEAD points at a branch missing from the fetched refs (a broken/stale mirror whose default branch was renamed without updating HEAD), `git clone` prints `remote HEAD refers to nonexistent ref, unable to checkout` and leaves an empty/detached checkout — yet hydrate still marks the project `available` because `DirtyState` on an empty tree returns clean. The product promise is "the same folder paths are really present on disk"; an empty checkout recorded as clean breaks that promise silently and is invisible to the user.

**Evidence:** `internal/cli/hydrate.go:103` clones via `r.Clone(ctx, project.RemoteURL, tmpPath, partial)`; `internal/git/git.go:107-118` `Clone` runs `clone --filter=blob:none -- remote dest` with no post-clone checkout verification. After promotion, `hydrate.go:112-113` calls `r.DirtyState` and unconditionally writes `UpdateProjectLocalState(..., "available", dirty)` — there is no `rev-parse --verify HEAD` or non-empty-tree assertion.

> Scope correction: the original claim that clone "trusts the cloned repo's local origin/HEAD instead of resolving the default branch authoritatively," fixable by adding `ls-remote --symref` + `--branch`, is a false premise. A fresh `git clone` resolves the checkout branch from the remote's advertised HEAD symref during the protocol handshake (`builtin/clone.c` `update_head` + `remote_head_points_at`); there is no pre-existing local `origin/HEAD` to be stale, and `ls-remote --symref` returns the identical answer. The only genuine residual gap is post-clone empty/detached-checkout detection and honest state, not authoritative branch resolution. Severity downgraded P1→P2, effort M→S accordingly.

**Recommendation:** After promoting the clone, verify `git rev-parse --verify HEAD` succeeds and the worktree is non-empty; on failure attempt `git remote set-head origin --auto` plus checkout of the resolved branch, and if still empty record an honest state (e.g. `materialized-empty`) instead of `available`/`clean`.

**Actionable steps:**
1. After `promoteClonedRepo`, run `git rev-parse --verify HEAD`; if it fails — or `git status --porcelain` plus a directory-entry check shows an empty tree — treat the materialization as incomplete.
2. Attempt self-heal: `git remote set-head origin --auto`, then checkout the resolved default branch, and re-verify.
3. If still empty, call `UpdateProjectLocalState` with a distinct state (e.g. `materialized-empty`) and surface it in `status`/`doctor` instead of recording `available`/`clean`.
4. Do NOT add an `ls-remote`-then-`--branch` clone for this purpose — it does not change the checkout git already performs; only the verification + honest-state step is valuable.
5. Add a regression test: a bare remote whose HEAD points at a ref absent from the pack must NOT be recorded as `available`/`clean`.

**Example:**
```go
// internal/cli/hydrate.go — after promoteClonedRepo succeeds
if _, err := r.RevParse(ctx, localPath, "HEAD"); err != nil {
    _, _ = r.Run(ctx, localPath, "remote", "set-head", "origin", "--auto")
    if branch, derr := r.RemoteDefaultBranch(ctx, localPath, "origin"); derr == nil {
        _, _ = r.Run(ctx, localPath, "checkout", branch)
    }
}
if _, err := r.RevParse(ctx, localPath, "HEAD"); err != nil {
    _ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "materialized-empty", "unknown")
    return localPath, nil // honest state, not 'available'
}
```

**References:**
- [git/builtin/clone.c](https://github.com/git/git/blob/master/builtin/clone.c) — `update_head()`/`remote_head_points_at`: clone sets local HEAD from the remote's advertised HEAD symref and only detaches/leaves empty when that HEAD points at a non-branch or an absent ref. Confirms detection, not re-resolution, is the fix.
- [clone: respect remote unborn HEAD](https://public-inbox.org/git/20201211210508.2337494-1-jonathantanmy@google.com/) — shows clone already negotiates the remote default/HEAD branch over protocol v2; the gap to cover is the empty/unresolvable-HEAD case.
- [GitHub: partial and shallow clone](https://github.blog/open-source/git/get-up-to-speed-with-partial-clone-and-shallow-clone/) — blobless clone still checks out HEAD; the value-add is verifying the checkout, since lazy blob fetch hides nothing about an empty/wrong ref.

### GIT-02 — Clone network-retry reuses a now-non-empty destination, turning transient mid-clone failures into fatal "already exists and is not empty"

**Severity / Effort / Category:** P1 / S / reliability

**Problem:** `Clone` wraps `git clone -- <remote> <dest>` in `runWithNetworkRetry`, which re-invokes the identical argv on `ErrNetwork`. But git populates `dest` as it runs and does NOT remove a directory it did not create (here `dest` is a pre-existing `os.MkdirTemp` dir). When the network drops mid-pack (early EOF / RPC failed / connection reset — all classified `ErrNetwork`), the second attempt runs against a now non-empty `dest` and fails with `fatal: destination path '...' already exists and is not empty`, which `classifyGitError` leaves unclassified (`Kind` nil) and returns as a hard error. Net effect: the retry is a no-op for the most common partial-failure case. With eager clone-everything across a whole `~/Code` fleet (`materializeConcurrency` up to 4 parallel) over flaky networks, mid-clone interruption is likely, so many repos that one clean retry would have fixed instead fail the sync.

**Evidence:** `internal/git/git.go:107-118` `Clone` → `r.runWithNetworkRetry(ctx, "", "clone", "--filter=blob:none", "--", remote, dest)`. `git.go:143-171` `runWithNetworkRetry` retries the same argv on `ErrNetwork` with no destination cleanup between attempts. `git.go:621-629` classifies `early eof`/`rpc failed`/`connection reset`/`the remote end hung up` as `ErrNetwork` (so a retry IS attempted); `already exists and is not empty` is absent from `classifyGitError`, so the retried failure returns with `Kind` nil and is fatal. The temp dir is created at `internal/cli/hydrate.go:124` via `os.MkdirTemp` before `Clone` runs, so git treats it as pre-existing and will not clean it on failure.

**Recommendation:** Make the clone retry idempotent: on a retryable network failure, `RemoveAll`+recreate `dest` before the next attempt (or clone into a fresh per-attempt temp dir and promote the successful one). Also classify `already exists and is not empty` so it is never silently a non-actionable error.

**Actionable steps:**
1. Add a clone-specific retry that removes and recreates `dest` (`os.RemoveAll` + `os.MkdirAll`) before each attempt after the first.
2. Alternatively, clone into a per-attempt fresh temp dir and only promote the attempt that succeeds.
3. Add `already exists and is not empty` to `classifyGitError` (or guarantee a clean `dest`) so it can never silently abort the batch.
4. Add a test injecting a network failure on attempt 1 and asserting attempt 2 succeeds into a clean directory.
5. Surface attempt counts in the materialize log so flaky-network resume is debuggable.

**Example:**
```go
func (r Runner) Clone(ctx context.Context, remote, dest string, partial bool) error {
    if err := ValidateRemote(remote); err != nil { return err }
    attempts := r.RetryAttempts; if attempts <= 0 { attempts = 1 }
    for attempt := 1; attempt <= attempts; attempt++ {
        if attempt > 1 { _ = os.RemoveAll(dest); _ = os.MkdirAll(dest, 0o750) }
        err := r.run(ctx, cloneArgs(remote, dest, partial)...)
        if err == nil { return nil }
        if !errors.Is(err, ErrNetwork) || attempt == attempts { return err }
        sleepBackoff(ctx, r.RetryBackoff*time.Duration(attempt))
    }
    return nil
}
```

**References:**
- [git-clone docs](https://git-scm.com/docs/git-clone) — clone removes the target directory on failure only when git itself created it; a pre-existing (`MkdirTemp`) directory is left populated, so a naive retry hits "destination path already exists and is not empty."
- [git partial-clone docs](https://git-scm.com/docs/partial-clone) — partial/dynamic fetch is network-dependent and may incur multiple round-trips; transient failures during the initial transfer are expected and must be retried cleanly.
- [GitHub: partial and shallow clone](https://github.blog/open-source/git/get-up-to-speed-with-partial-clone-and-shallow-clone/) — initial blobless clone transfers the full commit/tree graph over the network, so a fleet materializer must handle interrupted transfers idempotently.

### GIT-03 — Agent runner has no OS-enforced sandbox; "guarded" is argv substring matching plus an interpreter denylist that any indirection bypasses

**Severity / Effort / Category:** P1 / XL / hardening

**Problem:** The default `guarded` agent profile is enforced purely by argv substring matching plus an interpreter-name denylist (`enforceAgentCommandPolicy`/`enforceAgentFilePolicy`). The spec itself states this "oversells its safety" and is bypassable by any interpreter, base64, variable indirection, or a script file. HOME is repointed to the worktree and `SSH_AUTH_SOCK` is stripped (good), but the process still runs with the user's full ambient filesystem-read and network-egress authority, so a prompt-injected or buggy agent can read arbitrary readable paths or exfiltrate over the network the moment it shells out indirectly. Comparable autonomous coding tools (Codex, Claude Code) have moved to OS-enforced isolation; DevStrap remains the outlier calling a denylist "guarded."

**Evidence:** `internal/cli/agent.go:256-312` `enforceAgentCommandPolicy` is `strings.Contains(joined, pattern)` over a small deny list plus a fixed interpreter map; `agent.go:130` the `--policy` help text already disclaims "advisory only — not a security boundary until OS sandboxing lands." `agent.go:432-467` `runAgentProcess` uses `childenv.AgentAllowlist()` + repointed HOME but `exec.CommandContext` runs with no namespace/seccomp/Landlock confinement. A repo-wide grep for `seccomp|landlock|bubblewrap|bwrap|sandbox-exec|seatbelt` returns ZERO hits anywhere in `internal/`. `spec/10` lines 142-152 (Enforcement reality, AGEN-01/03) and the spec's "Not implemented yet" list both flag OS-enforced sandboxing as unbuilt.

**Recommendation:** Introduce a platform Sandbox adapter (mirroring `internal/platform`) that wraps the agent argv in bubblewrap+Landlock+seccomp on Linux and `sandbox-exec`/Seatbelt on macOS, defaulting `guarded` to deny-network + writable-only-worktree with an explicit opt-out, and a loud "unsandboxed" warning on unsupported platforms.

**Actionable steps:**
1. Add an `internal/platform` Sandbox interface `Wrap(argv, policy) ([]string, error)` with Linux (bwrap + Landlock V3 + seccomp-bpf) and macOS (`sandbox-exec` Seatbelt) implementations and a no-op fallback that prints a loud unsandboxed warning.
2. Default guarded/cautious to: read-only filesystem except the worktree, no network namespace, and never mount `~/.ssh` `~/.aws` `~/.config` — enforced at the kernel level, not by argv matching.
3. Add `devstrap doctor` checks for sandbox prerequisites (Landlock kernel ≥ 5.13, bwrap/slirp4netns, `sandbox-exec`) and report which isolation mode is active.
4. Provide an outbound-domain allowlist proxy for profiles that need package/forge access (cautious).
5. Make readonly/cautious/guarded semantics actually distinct (AGEN-04) once backed by OS enforcement; keep argv matching only as a fast accident-guard.

**Example:**
```go
// internal/platform/sandbox_linux.go
func (l linuxSandbox) Wrap(argv []string, p Policy) ([]string, error) {
    base := []string{"bwrap", "--die-with-parent", "--unshare-pid", "--unshare-ipc",
        "--ro-bind", "/", "/", "--bind", p.WorktreePath, p.WorktreePath, "--tmpfs", "/tmp"}
    if p.DenyNetwork { base = append(base, "--unshare-net") }
    return append(append(base, "--"), argv...), nil // plus a Landlock V3 + seccomp pre-exec hook
}
```

**References:**
- [OpenAI Codex agent approvals & security](https://developers.openai.com/codex/agent-approvals-security) — Codex defaults network OFF with an OS-enforced sandbox (Linux: bwrap+seccomp; macOS: Seatbelt) and writes limited to the workspace; the model DevStrap should match.
- [Claude Code secure deployment](https://code.claude.com/docs/en/agent-sdk/secure-deployment.md) — sandbox-runtime uses bubblewrap (Linux) / `sandbox-exec` (macOS) for filesystem+network restriction; recommends `--cap-drop ALL`, seccomp, `--network none`, never mounting `~/.ssh` `~/.aws`.
- [Sandlock (arXiv)](https://arxiv.org/html/2605.26298v1) — Landlock + seccomp-bpf kernel-enforced confinement adds ~5ms startup (44× faster than Docker), showing per-command kernel sandboxing is cheap enough for short-lived agent commands.

### GIT-04 — Worktree GC cannot reap squash/rebase-merged worktrees, and the spec'd `agent cleanup` / time-based GC are unimplemented

**Severity / Effort / Category:** P2 / M / devex

**Problem:** `worktree cleanup --merged` decides eligibility with `git branch --merged <baseRef> --list <branch>`, which only detects branches whose commits are ancestors of the base. Squash-merge and rebase-merge (the default on GitHub/GitLab for many teams) rewrite commit identity, so a squash-merged feature branch is NEVER detected as merged and its worktree accumulates forever. Combined with eager clone-everything plus one-worktree-per-agent-task, this guarantees unbounded growth of `~/.devstrap/worktrees`. Separately, `spec/10` documents `devstrap agent cleanup --merged` and `--older-than 14d` as the cleanup UX, but neither the `agent cleanup` subcommand nor any time-based GC exists in code.

**Evidence:** `internal/cli/worktree.go:506-507` `mergedOut, err := r.Run(ctx, wt.Path, "branch", "--merged", wt.BaseRef, "--list", wt.Branch)` then `if err != nil || !strings.Contains(mergedOut, wt.Branch) { skipped++; continue }` — fails for squash/rebase merges. `newAgentCommand` (`internal/cli/agent.go:22-32`) registers only run/list/show/pr — no `cleanup`. `spec/10` lines 235-236 advertise `devstrap agent cleanup --merged` and `--older-than 14d`; `spec/10:140` confirms `agent cleanup` "remain[s] future work." There is no `--older-than` flag on worktree cleanup either (`worktree.go:524-525` exposes only `--merged` and `--force`).

**Recommendation:** Add a patch-id / cherry-equivalence fallback (and optional forge merged-PR check) to detect squash merges, plus a time-based `--older-than` GC and the missing `agent cleanup` command — keeping the dirty/unpushed safety guard.

**Actionable steps:**
1. When `branch --merged` reports not-merged, fall back to the commit-tree+cherry technique: synthesize the branch's squashed commit and run `git cherry <base>`; a `-`-prefixed result means the content is already on base (squash-merged).
2. Add a patch-id fallback for multi-commit branches: `git diff-tree -p --merge-base <base> <branch> | git patch-id --stable` matched against base commit patch-ids.
3. Optionally consult the forge (`gh`/`glab pr list --head <branch> --state merged`) to confirm a merged PR before reaping.
4. Implement `devstrap agent cleanup --merged|--older-than <dur>` and a `worktree cleanup --older-than` flag that prunes clean, pushed, age-exceeded worktrees.
5. Preserve the dirty/unpushed guard (warn + require `--force`) so squash/age signals never delete unmerged work.

**Example:**
```go
// squash-merge aware eligibility (worktree.go cleanup loop)
if err != nil || !strings.Contains(mergedOut, wt.Branch) {
    ancestor, _ := r.Run(ctx, wt.Path, "merge-base", wt.BaseRef, wt.Branch)
    tree, _ := r.Run(ctx, wt.Path, "rev-parse", wt.Branch+"^{tree}")
    synth, _ := r.Run(ctx, wt.Path, "commit-tree", tree, "-p", ancestor, "-m", "_")
    cherry, _ := r.Run(ctx, wt.Path, "cherry", wt.BaseRef, synth)
    if !strings.HasPrefix(strings.TrimSpace(cherry), "-") { skipped++; continue }
}
```

**References:**
- [git branch --merged doesn't detect squash merges (gist)](https://gist.github.com/nikitagor-zen/1880733cab88467c5ac14dbbd8b10973) — the ancestry-based check misses squash merges; the commit-tree+cherry (patch-id) technique is the standard fix, same approach as `git-delete-squashed` and `git-trim`; no native git support as of 2025.
- [Removing squash-merged local git branches](https://blog.takanabe.tokyo/en/2020/04/remove-squash-merged-local-git-branches/) — working `git merge-base`+`git commit-tree`+`git cherry` recipe to detect and delete squash-merged branches.
- [Claude Code issue #40137](https://github.com/anthropics/claude-code/issues/40137) — `ExitWorktree` had the identical SHA-comparison bug after squash merge; recommended fix is `git cherry`/merged-PR check rather than ancestry.

### GIT-05 — Forge detection fails for self-hosted GitLab/Gitea, has no override, and doctor never probes glab/tea

**Severity / Effort / Category:** P2 / M / design-improvement

**Problem:** `DetectForge` matches on host substrings (`github.`/`gitlab.`/`gitea.`/`bitbucket.org`/`dev.azure.com`), so self-hosted instances (`git.acme.com`, `code.internal`, `scm.corp.net` running GitLab/Gitea/Forgejo) fall through to `ForgeUnknown` and `agent pr` degrades to a printed compare URL even though a perfectly good `glab`/`tea` exists. There is no `--forge` flag and no `git_repos.forge_kind` column to teach DevStrap that a host is a given forge. `doctor` only `LookPath`s `git, gh, go`, so it never warns that the forge CLI for an adopted self-hosted GitLab (`glab`) is missing — the failure only surfaces at `agent pr` time. For a tool branded forge-agnostic, the abstraction is GitHub-biased in practice.

**Evidence:** `internal/cli/forge.go:29-46` `DetectForge` uses `strings.Contains(host, "gitlab.")` etc.; a self-hosted host like `git.acme.com` matches nothing and returns `ForgeUnknown` (`forge.go:43-44`). `forge.go:137-146` unknown forge → compare-URL degradation. Grep confirms no `forge_kind` DB column and no `--forge` flag anywhere (only the `ForgeKind` type exists in `forge.go`). `internal/cli/doctor.go:28` probes only `{git, gh, go}`. `spec/08:370` explicitly lists FORGE-04 (a `--forge`/`git_repos.forge_kind` override for self-hosted instances + SSH host aliases, plus doctor per-remote CLI probes) and FORGE-05 (broader hermetic fake-CLI tests) as remaining work.

**Recommendation:** Add a per-project forge override resolved from flag/config/host-map, and make doctor probe the right forge CLI per adopted remote.

**Actionable steps:**
1. Add `git_repos.forge_kind`, a `[forge] host = kind` config map, and an `agent pr --forge` flag; resolution order flag > project column > host map > `DetectForge` heuristic.
2. Allow registering self-hosted hosts (e.g. `git.acme.com = gitlab`) so glab/tea routing works without per-call flags.
3. Extend `doctor` to iterate adopted remotes, resolve the forge (with overrides), and `LookPath` the matching CLI (`gh`/`glab`/`tea`), reporting missing CLIs and unknown forges up front.
4. Resolve SSH host aliases (`~/.ssh/config` Host → HostName) before `DetectForge` so `git@work-gitlab:org/repo` maps to the real host.
5. Add hermetic fake-CLI tests covering self-hosted GitLab/Gitea PR creation and override precedence (FORGE-05).

**Example:**
```go
// resolution
kind := overrideForge(flagForge, project.ForgeKind, hostMap[host], DetectForge(remoteURL))
```
```toml
# config
[forge]
  "git.acme.com" = "gitlab"
  "scm.internal" = "gitea"
```
```go
// doctor
for _, p := range adoptedRemotes {
    if cli := forgeCLI(resolveForge(p)); cli != "" {
        if _, err := exec.LookPath(cli); err != nil { warn(p.Path, cli+" missing") }
    }
}
```

**References:**
- [git-pkgs/forge](https://github.com/git-pkgs/forge) — reference forge abstraction: detects backend from remote but supports `--forge-type`/`FORGE_HOST` overrides, a committed `.forge` file mapping self-hosted hosts to forge types (`[gitlab.internal.dev] type = gitlab`), and explicit `RegisterDomain` registration for self-hosted instances.
- [git-pkgs forge module docs](https://git-pkgs.dev/docs/modules/forge/) — precedence model (CLI flags > env > per-project `.forge` > user config > built-in) is the pattern DevStrap should mirror for `forge_kind` resolution.

### GIT-06 — Eager materialization ignores submodules and has no prefetch/maintenance to avoid blobless lazy-fetch storms

**Severity / Effort / Category:** P2 / M / reliability

**Problem:** The materialize/clone path never initializes submodules, so any repo using submodules materializes structurally incomplete on every device — violating the "tree is really present on disk" promise for a non-trivial slice of real repos. Separately, because every repo is a blobless clone, the first `git blame`/`git log -p`/IDE history op triggers per-object lazy fetches; with no `git maintenance`/prefetch/backfill strategy, users hit lazy-fetch latency storms (each missing object is a separate fetch-pack + auth round-trip) that feel like hangs — multiplied by clone-everything across the fleet.

**Evidence:** `internal/git/git.go:107-118` `Clone` builds `clone --filter=blob:none -- remote dest` with no `--recurse-submodules`/`--also-filter-submodules`. A repo-wide grep for `submodule` returns ZERO hits in `internal/`. A repo-wide grep for `maintenance`/`backfill`/`prefetch` returns ZERO hits — no post-clone maintenance scheduling. `materializePass` runs up to `materializeConcurrency()` (cap 4, `internal/cli/materialize.go:27-32`) repos in parallel, multiplying lazy-fetch pressure.

**Recommendation:** Add a per-project submodule materialization policy and an opt-in prefetch/maintenance step so common history ops don't trigger lazy-fetch storms; surface the offline caveat in doctor.

**Actionable steps:**
1. Add `--recurse-submodules` (and `--also-filter-submodules` when filtering) under a per-project `materialization.submodules: auto|always|never` policy; record submodule hydrate state.
2. Offer `materialization.prefetch` / `git backfill` and schedule `git maintenance` (commit-graph, prefetch) so `git blame`/`log -p` don't trigger per-object fetches.
3. Document the offline caveat (blobless clones need the promisor online for historical blobs) and surface it in doctor for repos materialized blobless.
4. De-scope the "no partial-clone capability fallback" sub-claim: `git clone --filter=blob:none` against a server lacking the filter capability degrades to a full clone (not an error), so this is acceptable behavior — at most add an informational note.
5. Add tests for a submodule repo and verify maintenance/prefetch is wired.

**Example:**
```go
args := []string{"clone"}
if partial { args = append(args, "--filter=blob:none", "--also-filter-submodules") }
if submodulePolicy != "never" { args = append(args, "--recurse-submodules") }
args = append(args, "--", remote, dest)
// post-clone, opt-in:
_, _ = r.Run(ctx, dest, "maintenance", "start") // commit-graph + prefetch
```

**References:**
- [GitHub: partial and shallow clone](https://github.blog/open-source/git/get-up-to-speed-with-partial-clone-and-shallow-clone/) — blobless clones make `git blame`/checkout slower on first run due to on-demand blob fetches; plan prefetch/maintenance accordingly.
- [git partial-clone docs](https://git-scm.com/docs/partial-clone) — "Dynamic object fetching invokes fetch-pack once for each item... may incur significant overhead and multiple authentication requests if many objects are needed."
- [git-maintenance docs](https://git-scm.com/docs/git-maintenance) — `git maintenance` (commit-graph, prefetch) is the supported mechanism to keep partial-clone repos responsive without manual fetches.

### GIT-07 — Eager materialization pass has no persisted per-project failure record, resume, or progress detail, so partial sync failures are opaque

**Severity / Effort / Category:** P3 / M / reliability

**Problem:** `materializePass` isolates a single project's failure so the batch continues (good), but the only output is aggregate counts ("Materialized 3/5 projects"). Failures are logged at Warn and otherwise dropped: there is no persisted per-project materialization error, no `--only-failed`/resume flag, and status/doctor don't surface which projects failed or why. On a fleet of dozens of repos over a flaky network, a user sees "40/52 materialized" with no actionable list and no way to retry just the 12 that failed without re-walking everything.

**Evidence:** `internal/cli/materialize.go:82-112` `materializePass` increments counters and at line 97 logs `Warn("materialize failed, isolating failure", ...)` then returns nil and stores nothing actionable; `materializeResult` carries only total/succeeded/failed. The materialize command (`materialize.go:59-64`) prints only the aggregate and points to "doctor/status," but those commands do not list failed projects with their error. Failed git repos do get `UpdateProjectLocalState(..., "failed", "unknown")` (`hydrate.go:104/108`), but the error TEXT is discarded; grep confirms no `last_materialize_error` column, no `RecordMaterializeError`, and no `--only-failed` flag anywhere.

**Recommendation:** Persist the last materialization error per project, surface the failed set in status/doctor, and add a resume path that re-drives only failed/skeleton projects with backoff.

**Actionable steps:**
1. Add a `last_materialize_error` (plus timestamp/attempt count) column updated whenever `materializeOne` fails, storing the redacted error message.
2. Have `devstrap status`/`doctor` list projects in `failed` state with their last error and a one-line remediation hint.
3. Add `devstrap materialize --only-failed` (and `sync --only-failed`) that selects `state='failed'` and retries with capped exponential backoff.
4. Return a structured per-project result list from `materializePass` (path, type, ok, err) so the CLI can print an actionable failure table, not just counts.
5. Add a test asserting that after a simulated failure the project is queryable as failed and `--only-failed` re-drives exactly it.

**Example:**
```go
type projectResult struct{ Path, Type string; OK bool; Err string }
// on failure:
_ = store.RecordMaterializeError(ctx, project.ID, redact.Scrub(err.Error()))
// CLI:
for _, r := range failed { fmt.Fprintf(stdout, "FAILED  %-30s %s\n", r.Path, r.Err) }
// resume:
// devstrap materialize --only-failed   // selects state='failed', retries with backoff
```

**References:**
- [git partial-clone docs](https://git-scm.com/docs/partial-clone) — partial clone is explicitly online-dependent and on-demand fetch can fail transiently, so a fleet materializer must make per-repo failure inspectable and resumable rather than collapsing to an aggregate count.
- [The Twelve-Factor App: Disposability](https://12factor.net/disposability) — robust batch processes should be resumable and surface per-item state; reapplying only failed items is the standard fleet-operations pattern.

---
## Code Quality, Concurrency, Reliability & Testing

DevStrap's Go core is disciplined — gosec and errorlint are wired in, the state store enforces a single-writer pool, and there is a real testscript end-to-end harness — but the test strategy leans almost entirely on hand-built example tests, and several reliability seams (untrusted parsers, HLC convergence, partial-failure exit codes, retry backoff) are asserted by single fixed scenarios rather than by property tests, fuzzing, or machine-readable contracts. The supply-chain and CI posture is similarly mid-maturity: coverage is measured but never enforced, Windows is uncompiled despite a cross-platform mandate, and releases ship unsigned without SBOM or provenance. The findings below are ordered by severity and are each independently actionable.

### QUAL-01 — No fuzz targets for any untrusted parser, and draft-bundle extraction has no aggregate decompression cap

**Severity / Effort / Category:** P1 / M / hardening

**Problem:** Every parser reachable by attacker-influenced bytes ships only hand-written table tests; `grep -rn 'func Fuzz' internal cmd` returns nothing. These parsers sit on the untrusted boundary: the `.env` parser (`internal/envfile/parse.go` `ParseBytes`/`parseLine`/`parseDoubleQuoted`), the `.devstrapignore` → regexp compiler (`internal/ignore/ignore.go:217` `patternToRegex`), the git URL canonicalizer (`internal/git/git.go:445` `CanonicalRemoteKey` / `:480` `splitSCPLikeRemote`), and the age-decrypted tar extractor (`internal/draftbundle/draftbundle.go:188` `Extract`). Separately, `Extract` bounds each file with `io.LimitReader(tr, hdr.Size)` (draftbundle.go:242,256) but never caps total entry count or total uncompressed bytes during the extraction loop (draftbundle.go:210-266). The `Pack` side does track a running `totalBytes`/`fileCount` budget (draftbundle.go:137-149), but `Extract` — the hydrate path that runs on every device pulling a bundle authored by a compromised-but-trusted device — has no ceiling, so it is a gzip/tar decompression bomb.

**Evidence:** Verified: `grep -rn 'func Fuzz' internal cmd` → 0 results, and `grep rapid/quick/gopter go.mod go.sum` → 0 results. `internal/draftbundle/draftbundle.go:188-268` `Extract` loop has only per-file `io.LimitReader(tr, hdr.Size)` (lines 242,256) and NO running `MaxFiles`/`MaxTotalBytes` guard, despite `Pack` enforcing exactly those bounds at lines 144-149. Parser entrypoints confirmed: `internal/envfile/parse.go:30` `ParseBytes` / `:74` `parseLine` / `:125` `parseDoubleQuoted`; `internal/ignore/ignore.go:217` `patternToRegex` (regexp.Compile of user `.devstrapignore`); `internal/git/git.go:445` `CanonicalRemoteKey` / `:480` `splitSCPLikeRemote` / `:504` `normalizeHostPath`.

**Recommendation:** Add Go native fuzz targets (Go 1.18+) for each parser and run them in CI with a short `-fuzztime`, and add an aggregate extraction budget (max files + max total uncompressed bytes) to `draftbundle.Extract` mirroring the Pack-side `Limits`.

**Actionable steps:**
1. Add `FuzzParseBytes` in `internal/envfile`, `FuzzCompile` + a `Match` invariant in `internal/ignore`, `FuzzCanonicalRemoteKey` in `internal/git`, and `FuzzExtract` (over crafted age-encrypted-to-a-test-key tars) in `internal/draftbundle`, each seeded with `f.Add` corpus from existing table tests.
2. Assert crash-free + bounded behavior: no panic, `Match` always terminates, extracted paths always satisfy `pathWithin(cleanDest, target)`.
3. Pass a `Limits` value (or `MaxFiles`/`MaxTotalBytes` params) into `draftbundle.Extract` and abort once running `fileCount`/`totalBytes` exceed it, reusing the Pack-side defaults (`MaxBundleBytes=100MiB`, 5000 files); track bytes via the `io.Copy` return values already computed at lines 242/256.
4. Add a CI step `go test -run=^$ -fuzz=Fuzz -fuzztime=30s ./internal/envfile/... ./internal/ignore/... ./internal/git/... ./internal/draftbundle/...` (one package per step) so regressions surface.
5. Commit any minimized crashers under `testdata/fuzz` so they run as ordinary unit tests on every `go test`.

**Example:**
```go
func FuzzCompile(f *testing.F) {
    f.Add("node_modules/\n!keep/**\n/a/b?c")
    f.Fuzz(func(t *testing.T, src string) {
        m, err := Compile(src, true)
        if err != nil { return } // rejecting bad patterns is fine; panicking is not
        _ = m.Match("a/b/c", false)        // must terminate, never panic
        _ = m.ShouldPruneDir("x", "a/x")
    })
}
// Extract budget:
// if fileCount++ > lim.MaxFiles || totalBytes += n; totalBytes > lim.MaxBytes { return errBundleTooLarge }
```

**References:**
- https://go.dev/doc/security/fuzz/ — Go native fuzzing is coverage-guided and ideal for finding DoS/crash edge cases parsers miss; failures are auto-minimized and persisted as a regression corpus.
- https://learn.microsoft.com/en-us/dotnet/standard/io/zip-tar-best-practices — authoritative: TAR/ZIP readers do NOT limit total uncompressed size or entry count; you must track BOTH cumulative size and entry count as you iterate, exactly the guard `Extract` lacks today.
- https://github.com/hashicorp/go-extract/blob/main/config.go — canonical Go archive-extraction library exposing `maxFiles`, `maxExtractionSize` (total), and `maxInputSize` as first-class config — the aggregate-budget pattern to mirror in `Extract`.

### QUAL-02 — HLC monotonicity and conflict-resolution convergence are only example-tested, never property/model-checked

**Severity / Effort / Category:** P1 / M / reliability

**Problem:** The multi-device promise rests on two invariants: the HLC is strictly monotonic under concurrent `Send`/`Receive` across arbitrary clock skew (`internal/sync/hlc.go:21-71`), and event apply is commutative/idempotent so all devices converge to the same namespace regardless of pull-window ordering (`internal/sync` apply path). Today these are asserted only with hand-built scenarios — `TestReconcileSamePathIsCommutative` (hlc_test.go:176) and `TestApplyEventsSamePathDifferentRemoteUsesCanonicalWinnerAcrossPullWindows` (hlc_test.go:234) — each a single fixed permutation. `TestHLCSendIsRaceFreeAndStrictlyIncreasing` (hlc_test.go:78) only checks 16×64 concurrent `Send()` at a FROZEN clock (`now` fixed at `UnixMilli(1000)`); it never exercises random skew, interleaved `Receive`, or the 3-device conflict case. There is no generator exploring random event sets, delivery orders, or partition/replay, so a convergence bug that only appears on the 3-device interleaved-pull path passes CI.

**Evidence:** Verified: `internal/sync/hlc.go:21-71` `Send`/`Receive`. `internal/sync/hlc_test.go` has 13 example tests (lines 15-489); the convergence ones at `:176` and `:234` are single hardcoded permutations, and the concurrency test at `:78` holds the clock constant (`now := time.UnixMilli(1000)`), so it proves uniqueness under no skew, not monotonicity under skew + `Receive`. `grep rapid|testing/quick|gopter go.mod go.sum` → 0 results: no property/model framework is a dependency.

**Recommendation:** Add property-based tests (`pgregory.net/rapid`) that generate random event streams across N devices and random delivery orders, then assert HLC strict monotonicity and that applying any permutation yields identical final namespace state (convergence). Three replicas is sufficient to surface most conflict-resolution corner cases.

**Actionable steps:**
1. Add `pgregory.net/rapid` as a test-only dependency.
2. Write a rapid property: draw a random sequence of `Send`/`Receive` ops with random clock offsets (within and beyond `MaxSkew`) and assert the returned HLC is strictly increasing and never decreases under a backward wall clock.
3. Write a model/state-machine test over 3 in-memory stores: generate a random set of project add/rename/delete events, apply them in independently shuffled delivery orders, and assert the final `ProjectStatus` rows are byte-identical (strong eventual consistency: same update set → same state regardless of order).
4. Include duplicate delivery and tombstone-GC interleavings in the generator so idempotency is exercised, not just ordering.
5. Wrap one property with `rapid.MakeFuzz` so it also runs under the coverage-guided fuzzer in CI.

**Example:**
```go
func TestHLCStrictlyMonotonic(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        h := &HLC{Now: skewedClock(rapid.Int64Range(-3e5, 3e5).Draw(t, "skew"))}
        prev := int64(0)
        for i := 0; i < rapid.IntRange(1, 500).Draw(t, "ops"); i++ {
            v := h.Send()
            if v <= prev { t.Fatalf("HLC went backwards: %d <= %d", v, prev) }
            prev = v
        }
    })
}
```

**References:**
- https://pkg.go.dev/pgregory.net/rapid — property-based testing with automatic minimization and built-in state-machine/model testing, ideal for ordering/convergence invariants; any rapid test can become a fuzz target via `MakeFuzz`.
- https://ar5iv.labs.arxiv.org/html/2204.14129 — "Met: Model Checking-Driven Explorative Testing of CRDT Designs": demonstrates that THREE replicas suffice to express the convergence corner cases, and that the property to assert is "any two replicas converge to the same state given the same set of updates, regardless of order" — exactly what the apply path needs.
- https://ar5iv.labs.arxiv.org/html/1707.01747 — "Verifying Strong Eventual Consistency": formalizes that convergence follows iff concurrent operations commute, justifying a generator-based commutativity/idempotency assertion over example tests.

### QUAL-03 — `devstrap materialize` returns exit code 0 even when projects fail, breaking automation and CI gating

**Severity / Effort / Category:** P1 / S / reliability

**Problem:** `materializePass` deliberately isolates per-project failures (correct — EAGER-04), but the command's `RunE` prints "Materialized N/total" and "M project(s) failed" and then returns `nil` unconditionally (materialize.go:60-64). So `devstrap materialize` exits 0 whether 0 or all projects failed. Any script, cron job, or CI step gating on `devstrap materialize && ...` — exactly the eager-clone Workspace Passport bootstrap loop the product is built around — cannot detect a failed clone/hydrate. The failure is logged at `Warn` and counted into `res.failed`, but the only machine-readable signal (exit status) lies. There is also no structured (e.g. `--json`) failure summary for downstream tooling; `res.failed` is a bare counter (materialize.go:74,99-101) with no failed-path/error-kind detail.

**Evidence:** Verified: `internal/cli/materialize.go:60-64` — `RunE` prints results then `return nil` with no branch on `results.failed`. `materializePass` returns `materializeResult{total,succeeded,failed}` (lines 71-75) and increments `res.failed` at 99-101 inside the isolating goroutine, but the caller drops the count.

**Recommendation:** Return a distinct non-zero error when any project failed (while still completing the batch), and emit a structured, machine-readable failure summary so callers can branch on it.

**Actionable steps:**
1. Define `var ErrPartialMaterialize = errors.New("one or more projects failed to materialize")` in the cli package.
2. In `RunE`, after printing: `if results.failed > 0 { return fmt.Errorf("%w: %d/%d failed", ErrPartialMaterialize, results.failed, results.total) }`.
3. Have `materializePass` collect the failed project paths + classified error kinds (not just a counter) and surface them in the summary line and in a `--json` output mode.
4. Ensure cobra is configured (`SilenceUsage`/`SilenceErrors`) so the non-zero exit does not dump usage text for a runtime failure.
5. Add an end-to-end testscript asserting a non-zero exit when one repo clone fails but a sibling succeeds.

**Example:**
```go
if results.failed > 0 {
    fmt.Fprintf(stdout, "%d project(s) failed; run 'devstrap doctor' for details\n", results.failed)
    return fmt.Errorf("%w: %d/%d projects failed", ErrPartialMaterialize, results.failed, results.total)
}
return nil
```

**References:**
- https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/ — reliable clients must make failure observable to callers; silently absorbing partial failure removes the signal upstream automation needs to retry or alert.
- https://12factor.net/ (and POSIX convention) — a CLI must exit non-zero on operation failure so `&&` chains and CI gates can branch; print-and-swallow defeats composability.

### QUAL-04 — CI generates a coverage profile but never enforces it, and has no Windows build despite the cross-platform Go-core mandate

**Severity / Effort / Category:** P2 / M / devex

**Problem:** The test job runs `go test -race -covermode=atomic -coverprofile=coverage.out` (ci.yml:76) but `coverage.out` is never uploaded, thresholded, or diffed in any subsequent step — coverage can silently rot, and the newer `internal/hub`, `internal/draftbundle`, `internal/ignore` packages can regress without a gate. Separately the matrix is only `ubuntu-latest` + `macos-latest` (ci.yml:52). The architecture commits to a portable Go core (XP-*) and a Windows/WinFsp future (Phase 4), and the codebase already carries Windows-divergent path/separator logic (draftbundle.go:218 backslash rejection, `pathWithin` using `filepath.Separator` at draftbundle.go:275, `internal/pathkey` normalization, git `CanonicalRemoteKey` filepath handling) — none compiled or tested on `windows-latest`. GoReleaser also only builds darwin/linux (.goreleaser.yaml:13-15), so no Windows binary is ever produced either, compounding the blind spot.

**Evidence:** Verified: `.github/workflows/ci.yml:76` produces `coverage.out` with no later coverage step (the job ends at "Module hygiene", 77-80). Matrix at `ci.yml:52` is `[ubuntu-latest, macos-latest]`. `draftbundle.go:218` rejects backslashes and `:275` uses `filepath.Separator` — platform-divergent and Windows-untested. `.goreleaser.yaml:13-18` builds goos darwin/linux only.

**Recommendation:** Add a coverage floor (fail-under or per-PR coverage diff) and add a `windows-latest` build+vet+test lane (at least `go build`/`go vet`, ideally `go test` for the pure-Go packages).

**Actionable steps:**
1. Add a CI step running `go tool cover -func=coverage.out` that fails when total (or critical-package) coverage drops below an agreed floor (e.g. 70%), or wire codecov/coveralls with a patch-coverage gate.
2. Add `windows-latest` to the test matrix; gate POSIX-only adapters with build tags and run `go build ./... && go vet ./...` plus `go test ./internal/ignore/... ./internal/pathkey/... ./internal/git/... ./internal/draftbundle/...` on Windows.
3. Extend the existing "Guard against vacuous package tests" check (ci.yml:66-70, currently `internal/state`, `internal/config`, `internal/cli`) to require tests for `internal/hub`, `internal/draftbundle`, `internal/ignore`.
4. Document the coverage floor in `spec/16_TEST_PLAN.md` so the gate is a spec contract, not just CI config.

**Example:**
```yaml
      - name: Coverage gate
        run: |
          pct=$(go tool cover -func=coverage.out | awk '/^total:/ {sub(/%/,"",$3); print $3}')
          awk -v p="$pct" 'BEGIN{ if (p+0 < 70) { print "coverage " p "% < 70%"; exit 1 } }'
# matrix: os: [ubuntu-latest, macos-latest, windows-latest]
```

**References:**
- https://go.dev/blog/cover — Go's coverage tooling is built into `go test`; a produced profile should gate (fail-under), not just be discarded.
- https://github.com/marketplace/actions/go-test-coverage — maintained action that enforces total and per-package/per-file coverage thresholds in CI as a required check.
- https://golangci-lint.run/docs/ — cross-platform Go projects must build/vet on every target OS; separator and case-folding bugs only surface when Windows is actually compiled in CI.

### QUAL-05 — Release artifacts are unsigned with no SBOM or build provenance (checksums.txt only)

**Severity / Effort / Category:** P2 / M / hardening

**Problem:** The GoReleaser config (`.goreleaser.yaml` — note the actual extension is `.yaml`, not `.yml`) produces a `checksums.txt` and tarballs but has no `signs:` block, no `sboms:` block, and the release workflow emits no SLSA/GitHub build attestation. `checksums.txt` by itself only proves an archive matches a checksum that lives in the same untrusted release — it is not a signature and proves nothing about provenance (the config footer literally tells users to "Verify downloads against checksums.txt", which is non-cryptographic). For a tool whose value proposition is installing a privileged binary on every developer machine, cloud box, and agent runner (and which holds device signing/age keys), an unsigned, unattested release is a soft supply-chain target: a tampered GitHub release or compromised token can ship a backdoored `devstrap` users cannot cryptographically verify.

**Evidence:** Verified: `.goreleaser.yaml` has `checksum: {name_template: checksums.txt}` (lines 32-33) and a footer telling users to verify against `checksums.txt` (53), but NO `signs:`/`sboms:` sections anywhere in the 54-line file. `.github/workflows/release.yml` runs `goreleaser release --clean` with only `GITHUB_TOKEN` and `permissions: contents: write` — no `id-token: write`, no cosign/syft install, no `actions/attest-build-provenance`. The goreleaser-action is pinned to the moving tag `@v7` (release.yml), unlike every other action in the repo which is SHA-pinned, with a code comment acknowledging it should be SHA-pinned "on the next bump".

**Recommendation:** Adopt the GoReleaser secure-supply-chain pattern: cosign keyless-sign `checksums.txt`, generate Syft SBOMs per artifact, and add GitHub OIDC build-provenance attestation; and SHA-pin goreleaser-action.

**Actionable steps:**
1. Add a `signs:` block invoking `cosign sign-blob --bundle=${signature} ${artifact} --yes` over the checksum artifact (keyless via OIDC).
2. Add a `sboms:` block (syft) to emit a per-archive SBOM uploaded with the release.
3. In release.yml add `permissions: { contents: write, id-token: write }`, install cosign + syft (cosign-installer / anchore actions), and add an `actions/attest-build-provenance` step over `dist/*.tar.gz`.
4. Document verification (`cosign verify-blob --bundle checksums.txt.sigstore.json ...` and `gh attestation verify`) in README/SECURITY.md and RELEASING.md.
5. SHA-pin `goreleaser/goreleaser-action` (currently `@v7`, a moving tag) to match the rest of the repo's pinned actions.

**Example:**
```yaml
signs:
  - cmd: cosign
    signature: "${artifact}.sigstore.json"
    args: ["sign-blob", "--bundle=${signature}", "${artifact}", "--yes"]
    artifacts: checksum
sboms:
  - artifacts: archive
# release.yml: permissions: { contents: write, id-token: write }
```

**References:**
- https://goreleaser.com/customization/sign/ — GoReleaser signs the checksum file (cosign `--bundle .sigstore.json`); signing the single checksum file is enough to authenticate every artifact it lists.
- https://github.com/goreleaser/example-supply-chain — canonical GoReleaser + GitHub Actions example wiring cosign keyless signing, Syft SBOMs, and SLSA provenance attestations end to end.
- https://github.com/actions/attest-build-provenance — official GitHub action producing SLSA build provenance verifiable with `gh attestation verify`.

### QUAL-06 — Network retries use deterministic linear backoff with no jitter and no aggregate deadline budget

**Severity / Effort / Category:** P2 / S / reliability

**Problem:** `runWithNetworkRetry` sleeps `backoff * time.Duration(attempt)` (git.go:162) — fixed, un-randomized, identical for every caller (200ms then 400ms between the three attempts). The eager materialization pass runs up to `materializeConcurrency()=4` clones in parallel (materialize.go:27-32) against (commonly) the same forge; on a forge hiccup all four fail and retry in lockstep at the same 200ms/400ms boundaries — a synchronized thundering herd that amplifies load exactly when the forge is already struggling. Additionally, `Run()` installs a fresh 2-minute timeout only when the parent `ctx` has no deadline (git.go:64-68); the materialize CLI path sets no deadline, so a hung clone can consume `RetryAttempts × 2min` (~6min) per project with no aggregate cancellation budget across the batch.

**Evidence:** Verified: `internal/git/git.go:162` `timer := time.NewTimer(backoff * time.Duration(attempt))` — linear, no randomization. `NewRunner()` sets `RetryAttempts:3, Timeout:2m` (git.go:28). `Run()` applies the timeout only when `_, ok := ctx.Deadline(); !ok` (git.go:64-68). `materializeConcurrency()` returns `min(NumCPU,4)` (materialize.go:27-32) and all workers drive `runWithNetworkRetry` via `Clone`/`Fetch` (git.go:116,132).

**Recommendation:** Switch to capped exponential backoff with full jitter, and thread an overall per-operation (or per-batch) deadline budget so retries cannot run unbounded.

**Actionable steps:**
1. Replace `backoff * attempt` with full jitter: `sleep = rand.Int63n(min(cap, base*2^(attempt-1)) + 1)`.
2. Add a `RetryCap`/`MaxElapsed` to `Runner` so total retry time per operation is bounded regardless of attempt count.
3. Set a context deadline budget for the materialize pass (or per project) so a single hung clone cannot wedge a worker slot for ~6 minutes.
4. Add a token-bucket / retry-quota cap so repeated forge failures fail fast instead of every worker retrying at full rate.
5. Unit-test the backoff function with a seeded RNG asserting delays stay within `[0, cap]` and increase in expectation.

**Example:**
```go
import "math/rand"
// full jitter, capped
d := time.Duration(rand.Int63n(int64(min(r.RetryCap, r.RetryBackoff*(1<<uint(attempt-1)))) + 1))
timer := time.NewTimer(d)
```

**References:**
- https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/ — no-jitter backoff is the measured loser; Full Jitter spreads synchronized retries to a near-constant rate and substantially cuts client work and server load.
- https://docs.aws.amazon.com/general/latest/gr/api-retries.html — standard retry mode = exponential backoff with full jitter PLUS a retry quota (token bucket) so clients fail fast during disruptions instead of retrying at full rate.
- https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/ — also argues for a bounded overall retry deadline so a slow dependency cannot consume unbounded client time.

### QUAL-07 — golangci-lint omits the resource/context-leak linters that matter most for a DB + subprocess + future-HTTP codebase

**Severity / Effort / Category:** P3 / S / hardening

**Problem:** The lint config enables gosec and errorlint (good — those earlier recommendations are closed) but stops short of the linters that catch the bug classes this codebase is structurally exposed to: unclosed `sql.Rows`/`Stmt` and unchecked `Rows.Err` on the 2,571-line state store, context not propagated into helpers (the R2 Hub and git layers thread `ctx` by hand), and the now-redundant `project := project` loop copy at materialize.go:91 (unnecessary and flagged by copyloopvar on go 1.26.4). The store runs on a `SetMaxOpenConns(1)` single-writer pool (store.go:209), so a single leaked `Rows` handle pins the only connection and can deadlock the whole process the moment an iterator is left open. Without sqlclosecheck/rowserrcheck/noctx/contextcheck/copyloopvar these classes are invisible.

**Evidence:** Verified: `.golangci.yml:6-14` enables only errcheck, gosec, govet, ineffassign, staticcheck, unconvert, errorlint — none of bodyclose/sqlclosecheck/rowserrcheck/noctx/contextcheck/copyloopvar. `internal/state/store.go` is 2,571 lines on a `db.SetMaxOpenConns(1)` pool (store.go:209-210). `internal/cli/materialize.go:91` `project := project` is a redundant copy on go 1.26.4 (go.mod:3). The test exclusion (.golangci.yml:17-22) blanket-disables errcheck AND gosec on all `_test.go`.

**Recommendation:** Enable the database, context, and concurrency linters (bodyclose, sqlclosecheck, rowserrcheck, noctx, contextcheck, copyloopvar) and narrow the blanket `_test.go` gosec/errcheck exclusion to only the rules that genuinely false-positive on tests.

**Actionable steps:**
1. Add bodyclose, sqlclosecheck, rowserrcheck, noctx, contextcheck, copyloopvar to `linters.enable` in `.golangci.yml`.
2. Run `golangci-lint run` and fix surfaced findings (any unclosed `Rows`/`Stmt` in `internal/state`, any `ctx` not propagated, the redundant loop copy at materialize.go:91).
3. Replace the blanket `_test.go: [errcheck, gosec]` exclusion with a narrower one targeting only the specific rules that false-positive on test helpers, so security-relevant test code is still scanned.
4. Re-run the suite under `-race` to confirm no lint-driven refactor introduced a data race.
5. Record the enabled linter set in `spec/16_TEST_PLAN.md` so it is a tracked contract.

**Example:**
```yaml
linters:
  enable:
    - errcheck
    - gosec
    - errorlint
    - bodyclose
    - sqlclosecheck
    - rowserrcheck
    - noctx
    - contextcheck
    - copyloopvar
```

**References:**
- https://golangci-lint.run/docs/linters/ — sqlclosecheck (`sql.Rows`/`Stmt` closed), rowserrcheck (`Rows.Err` checked), bodyclose, noctx, and contextcheck target exactly the resource/context-leak classes that bite DB + HTTP + subprocess code.
- https://github.com/golangci/golangci-lint/blob/master/.golangci.reference.yml — the maintained reference config enabling bodyclose, contextcheck, copyloopvar, and the sqlclosecheck-class linters as a strict baseline.

---
## Product, UX, DevEx & New Features

DevStrap's core engine — eager-clone materialization, fresh worktrees, encrypted env blobs — is well built, but the surfaces a developer actually touches first lag behind that quality. The onboarding loop spans four commands, `doctor` cannot grade or fix anything, distribution still means "clone and `go build`," and several detect-only surfaces (conflicts, the deferred daemon) lack the action half of their design. The six findings below are mostly thin orchestrators over internals that already exist, so they convert disproportionate adoption value for small-to-medium effort.

### PROD-01 — Add a `devstrap clone <url>` one-shot quick path to collapse onboarding to a single command

**Severity:** P1 / **Effort:** M / **Category:** new-feature

**Problem**
Getting one repo into the managed namespace today requires `init`, then `add --path ...`, then `hydrate`, then `open` — four commands plus a hand-chosen namespace path. There is no single quick path that accepts a clone URL and does the right thing. Developer tools win or lose on time-to-first-success; a long first loop signals a slow product and drives abandonment before the differentiating worktree/sync value is ever seen.

**Evidence**
Verified: `root.go` registers `add`/`hydrate`/`open`/`materialize` but no `clone` command — the `AddCommand` list runs scan→add→hydrate→open→worktree→sync→materialize→draft (`internal/cli/root.go:93-98`). `add.go` hard-requires `--path`: `if nsPath == "" { return appError{... err: fmt.Errorf("--path is required")} }` (`internal/cli/add.go:23-25`). README usage chains `add --path` + `hydrate` + `open` (`README.md:95-100`).

**Recommendation**
Add `devstrap clone <url> [path]` that derives a namespace path from the remote (`org/repo`), runs add + eager materialize + optional `--open`, and prints the resulting path. Make it the headline command in the README and `--help` examples. Reuse the existing `add`/`materializeOne` internals so it stays a thin orchestrator, not new core logic.

**Actionable steps**
1. Add `newCloneCommand` wrapping the existing add → `materializeOne` (git_repo) → optional editor-open path.
2. Derive the default namespace path from the normalized remote (e.g. `work/<org>/<repo>`), allowing an override via positional arg.
3. Register it in `root.go` and lead the README/quickstart with it; add a command-doc test entry (`command_doc_test.go`) and update the spec command list in `13_CLI_DAEMON_API.md` (the command-doc drift test fails CI otherwise).

**Example**
```console
$ devstrap clone git@github.com:acme/api.git
  added   work/acme/api
  cloned  (blobless)  142 files
  env     hydrated .env
Open with: devstrap open work/acme/api --cursor
```

**References**
- https://www.skene.ai/resources/blog/developer-onboarding-guide — time-to-first-success is the dominant onboarding metric for dev tools.
- https://mise.jdx.dev/ — single-command setup is the modern baseline for developer-environment tooling.

### PROD-02 — Upgrade `doctor` to a severity-graded health report with `--fix`, `--json`, and a non-zero exit on errors

**Severity:** P1 / **Effort:** M / **Category:** devex

**Problem**
`doctor` prints raw `key: value` lines with no ok/warning/error grading, no remediation, no JSON, and always exits 0 — so it cannot gate CI or guide a fix. chezmoi's `doctor` grades every check (ok/info/warning/error) and exits 1 on any error, making it the canonical first-thing-to-run. DevStrap's own spec even lists planned checks (env check, daemon) the code never surfaces.

**Evidence**
Verified: `doctor.go` emits flat `fmt.Fprintf` lines with no severity (e.g. `"%s: missing\n"` for git/gh/go at `internal/cli/doctor.go:28-35`) and returns `nil` on every path — it always exits 0 on a healthy-or-degraded check. No `--json`, `--fix`, or `--no-network` flags are registered (the command has no `Flags()` block, `internal/cli/doctor.go:17-84`). Stale repo locks are already *detected* in `doctorReportLocks` (lines 86-121) but only printed, never fixed; the code even tells the user to run `worktree unlock` (lines 112-115) without being able to do it. A global `--json` viper key exists (used by `status.go:39` and `conflicts.go:25`) but `doctor` does not honor it. Spec lists `env check` as Planned (`13_CLI_DAEMON_API.md:55`).

**Recommendation**
Introduce a check-result type `{name, status: ok|warn|error, detail, remedy}`. Render a graded table, support `--json` and `--no-network`, and exit non-zero when any check is error. Add an opt-in `--fix` that performs safe remediations (create the missing state home, run pending migrations, clear the stale repo locks already detected in `doctorReportLocks`).

**Actionable steps**
1. Refactor doctor checks to return `[]checkResult` instead of printing inline.
2. Add severity rendering, `--json`, `--no-network`, and exit-code-on-error.
3. Add `--fix` that remediates the already-detected stale locks and the missing-migration / missing-state-home cases.

**Example**
```console
$ devstrap doctor
ok      git                /usr/bin/git
warning gh                 not found (PR creation will fall back to compare URL)
error   schema             3 migrations pending — run `devstrap db migrate` (or doctor --fix)
2 ok, 1 warning, 1 error
```

**References**
- https://chezmoi.io/reference/commands/doctor/ — the canonical graded-checks `doctor` that exits non-zero on error.
- https://clig.dev/ — CLIs should make state easy to see and exit with meaningful codes.

### PROD-03 — Make `init` a guided first-run: offer scan-on-init and always print the next command to run

**Severity:** P2 / **Effort:** S / **Category:** ux

**Problem**
`init` ends with a single "Initialized…" line and no guidance, leaving the user to discover that scan/add/sync even exist. clig.dev explicitly recommends suggesting the next command and making state easy to see. An opt-in `--scan` that adopts existing repos in `~/Code` immediately would deliver the a-ha moment — the user's tree appearing — without a multi-screen wizard.

**Evidence**
Verified: `init.go` prints only `"Initialized DevStrap workspace %q at %s\n"` with no follow-up guidance (`internal/cli/init.go:86`) and never invokes scan/adopt; the only flags registered are `--workspace-name` and `--dry-run` (`internal/cli/init.go:91-92`). There is no `--scan` flag and no next-steps block. The scan/adopt path already exists (`scan.go`), so wiring `--scan` is genuinely small.

**Recommendation**
After init, print a short "Next steps" block (scan to adopt, clone to add a repo, status to view). Add `devstrap init ~/Code --scan` that runs the existing `scan --adopt` inline so a user with a populated `~/Code` sees projects on the very first command. Keep it skippable and non-interactive by default (TTY-gate any prompt).

**Actionable steps**
1. Append a 2-3 line next-steps hint to the init success output.
2. Add a `--scan` flag that calls the existing scan/adopt path after workspace creation.
3. Print the adopted project count so the user sees an immediate result.

**Example**
```console
$ devstrap init ~/Code --scan
Initialized workspace "personal" at /Users/me/Code
Adopted 7 existing projects.
Next: devstrap status • devstrap clone <url> • devstrap sync --hub-file <path>
```

**References**
- https://clig.dev/ — suggest the next command and surface state after every action.
- https://evilmartians.com/chronicles/easy-and-epiphany-4-ways-to-stop-misguided-dev-tools-users-onboarding — engineer an early "epiphany" moment instead of a wizard.

### PROD-04 — Ship `devstrap service install` to generate a user LaunchAgent/systemd unit wrapping `run-loop`

**Severity:** P2 / **Effort:** M / **Category:** new-feature

**Problem**
The "Dropbox experience for code" promise needs background convergence, but the daemon is deferred and the only periodic mechanism, `run-loop`, runs in the foreground and refuses to start without `--hub-file`. There is no command to register a background service, so users must hand-write launchd/systemd units. A thin generator that wraps `run-loop --once` as a user-level LaunchAgent / `systemd --user` unit delivers auto-sync now, without the full Phase-1 daemon.

**Evidence**
Verified: `run_loop.go` hard-requires `--hub-file` (`if hubFile == "" { return appError{... "--hub-file is required until the production hub exists"} }`, `internal/cli/run_loop.go:27-29`) and is a foreground ticker (`runLoopForever`, lines 50-82). Its own docstring anticipates "a later launchd/systemd unit can drive it with --once" (lines 13-17). The `platform.ServiceManager` seam exists with Install/Uninstall/Status + `ServiceSpec` (`internal/platform/platform.go:55-66`) but only an `UnsupportedServiceManager` returning `ErrUnsupported` is wired for every OS (`detect_linux.go:9`, `detect_other.go:11`, `platform.go:135-144`). No `service` command is registered in `root.go` (lines 87-105). Roadmap M5/M6 LaunchAgent + systemd install tasks are unchecked (`14_MVP_ROADMAP_AND_BACKLOG.md:308, 335`).

**Recommendation**
Add `devstrap service install/uninstall/status` that writes a user-owned unit (`~/Library/LaunchAgents/…plist` / `~/.config/systemd/user/devstrap.service`) invoking `devstrap run-loop --once` on a schedule, behind the existing `platform.ServiceManager` seam. No sudo; logs under `~/.devstrap/logs`. This is the highest-value step toward unattended sync before the resident daemon lands. Note the `--hub-file` gate: until `HUB-*` ships, the installed service only works against a file-backed hub, so either document that in the unit-generation step or relax the gate for namespace-only ticks.

**Actionable steps**
1. Implement `ServiceManager.Install` for darwin (LaunchAgent) and linux (`systemd --user`), keeping all OS branching in `internal/platform`.
2. Generate a unit invoking `run-loop --once` (or `--interval`) with logs to the user state dir.
3. Add `service status`/`uninstall` and document the headless macOS auto-login / Linux linger caveats.

**Example**
```console
$ devstrap service install --interval 5m
Wrote ~/Library/LaunchAgents/com.devstrap.sync.plist
Loaded user agent (no sudo). Logs: ~/.devstrap/logs/run-loop.log
$ devstrap service status
running  last tick 2026-06-28T18:04:11Z
```

**References**
- https://til.jingkaihe.com/kodelet-gpt-guest-post-user-level-daemons-systemd-launchd/ — pattern for shipping user-level daemons via launchd/systemd without sudo.
- https://developer.apple.com/library/archive/technotes/tn2083/_index.html — Apple's authoritative guidance on per-user daemons and LaunchAgents.

### PROD-05 — Close the distribution gap: publish a Homebrew tap, a curl|sh installer, and shell completions

**Severity:** P2 / **Effort:** S / **Category:** devex

**Problem**
A GoReleaser pipeline exists but only emits tarballs and a GitHub Release; there is no Homebrew tap, no `.deb`/`.rpm`, and no one-line installer, so the README tells users to clone and `go build`. Build-from-source is a major adoption tax and forces a Go toolchain onto every machine the product is meant to make disposable — directly contradicting the "install on any new box" promise.

**Evidence**
Verified: `.goreleaser.yaml` contains only `builds`, `archives` (tar.gz), `checksum`, `changelog`, and `release` stanzas — no `brews:`, no `nfpms:`, no installer (full file read). README install is `git clone … && go build -o bin/devstrap ./cmd/devstrap` (`README.md:57-65`), and every Usage example runs `go run ./cmd/devstrap …`. The backlog lists "Homebrew tap" unchecked (`14_MVP_ROADMAP_AND_BACKLOG.md:402`). Since GoReleaser is already configured and Cobra is the CLI framework (every command is a `cobra.Command`, so `completion` generation is built in), adding `brews`/`nfpms` and bundling completions is genuinely small.

**Recommendation**
Add a `brews:` block to GoReleaser publishing a formula to a `homebrew-tap` repo, add `nfpms:` for `.deb`/`.rpm`, ship a `curl https://…/install.sh | sh` script that fetches the right checksum-verified release asset, and wire Cobra completion generation into packaging. Update the README to lead with `brew install reederey87/tap/devstrap`.

**Actionable steps**
1. Add `brews` + `nfpms` stanzas to `.goreleaser.yaml` and create a `homebrew-tap` repo.
2. Add an `install.sh` that resolves OS/arch and downloads the matching checksum-verified tarball.
3. Bundle generated shell completions and update README install order (brew → installer → source).

**Example**
```yaml
brews:
  - repository: { owner: Reederey87, name: homebrew-tap }
    homepage: https://github.com/Reederey87/DevStrap
    install: |
      bin.install "devstrap"
      generate_completions_from_executable(bin/"devstrap", "completion")
```

**References**
- https://iamanuragh.in/blog/2026-02-08-building-cli-tools-done-right/ — distribution channels (brew, installer, packages) are table stakes for CLI adoption.
- https://goreleaser.com — first-class `brews`/`nfpms` support makes multi-channel release a config addition, not new code.

### PROD-06 — Give the detect-don't-merge model a resolution surface: `devstrap conflicts resolve`

**Severity:** P2 / **Effort:** M / **Category:** ux

**Problem**
The design is explicitly detect-don't-merge with dual-copy as the safe default, and `status` surfaces an open-conflict count pointing at `devstrap conflicts` — but that command only lists. There is no way to act on a namespace conflict (keep local, keep remote, keep both), so the count can only grow and the safe-default dual-copy is never operationalized at the namespace layer.

**Evidence**
Verified: `conflicts.go` is a single command that only lists `OpenConflicts` and tells the user to "Resolve the underlying issue and re-run the originating command" (`internal/cli/conflicts.go:11-46`) — no subcommands, no resolve action. It is registered as one leaf command at `root.go:105`. `status.go` surfaces the count and points at the list-only command: `"Open conflicts: %d (run \`devstrap conflicts\` to inspect)\n"` (`internal/cli/status.go:50-52`). The detect-don't-merge framing is at `13_CLI_DAEMON_API.md:483`; the dual-copy safe default is documented at `14_MVP_ROADMAP_AND_BACKLOG.md:511` and `17_REFERENCES.md:153`. Dual-copy is already implemented for *draft-bundle file* conflicts (draftbundle writes `<name>.devstrap-conflict`, `18_WORK_LOG.md:54`), but there is no equivalent resolution surface for *namespace* conflicts — so extending it is consistent with the shipped design, not invented.

**Recommendation**
Make `conflicts` a command group: keep `list`, add `resolve <id> --keep-local|--keep-remote|--keep-both` and `show <id>`. `--keep-both` materializes the dual-copy the design already promises, records the resolution as an HLC event, and clears the row. Surface the chosen action in `status` so the count reflects real progress.

**Actionable steps**
1. Promote `conflicts` to a command group with `list`/`show`/`resolve` subcommands.
2. Implement the resolve actions, with `--keep-both` producing the design's dual-copy plus an audit/HLC event.
3. Mark the conflict row resolved and reflect it in the `status` open-conflict count.

**Example**
```console
$ devstrap conflicts list
cfl_01jz  work/acme/api  same-path/different-remote  open
$ devstrap conflicts resolve cfl_01jz --keep-both
Kept local at work/acme/api and remote copy at work/acme/api.remote
Conflict cfl_01jz resolved.
```

**References**
- https://mutagen.io/documentation/synchronization — proven conflict modes (alpha/beta/two-way-safe) and dual-copy-style safe defaults for file sync.
- https://clig.dev/ — surface state and give the user a clear, actionable way to act on it.

---

## Appendix A — Methodology

This audit was produced by a six-dimension multi-agent review of the post-PR-#16 tree:

1. **Dimension audit (parallel).** One agent per dimension (Security & Cryptography, Sync Engine & Data Model, Cloud Hub & Scalability, Git Materialization & Worktrees, Code Quality & Testing, Product/UX & New Features) read the cited specs and source files and researched external best practices via web search (Exa).
2. **Adversarial grounding (parallel).** A second, independent verifier re-checked every finding against the live code — reading the cited files, grepping for the named symbols, and cross-referencing the "Implemented" list in `spec/00_START_HERE.md`. Findings that were already implemented, unsupported by evidence, or mis-framed were dropped or rescoped. (For example, the original `GIT-01` claim that materialization needs `ls-remote --symref` to resolve the default branch was downgraded after the verifier confirmed `git clone` already resolves HEAD over the protocol handshake; the genuine residual gap — recording an empty checkout as `available`/`clean` — was kept.)
3. **Synthesis.** Surviving findings were rendered into the per-dimension sections above, with a cross-cutting executive summary and prioritized roadmap.

Only `confirmed` and `rescoped` findings appear in this document. Each carries `file:line`/spec evidence, a recommendation, actionable steps, a concrete example, and at least one external reference.

## Appendix B — Relationship to the shipped `HUB-01..08` workstreams

The third-pass audit (`AUDIT_RECOMMENDATIONS_2026-06-28.md`) defined `HUB-01..08` for the cloud-hub backend, all of which shipped in PR #16 (pluggable `Hub` interface, R2/S3 backend, fail-closed verification, revoke re-encryption, blob ref-count/GC, scoped credentials). To avoid colliding with those identifiers, the **new** cloud-hub findings in this fourth pass are numbered **`HUB-09..HUB-16`**. Where a finding here references a shipped workstream (e.g. `SEC-01` discussing the `HUB-04` revoke story), that reference points at the *implemented* `HUB-01..08`, not at a finding in this document.

## Appendix C — Suggested sequencing

A pragmatic order that front-loads correctness/safety of the just-landed cloud-sync system before its first real multi-device use, then pays down growth/scale debt, then invests in the product surface:

1. **Make the hub backend safe to turn on** — `HUB-09`/`HUB-10` (retry+backoff, drop the TOCTOU pre-check), `SEC-03` (verify blob content-address on fetch), `GIT-02` (clone-retry into a fresh dir), `QUAL-03` (non-zero exit on materialize failure). Mostly small, all high-leverage.
2. **Close the zero-knowledge gaps before multi-device trust** — `SEC-01` (real hub-side revoke delete), `SEC-02` (encrypt the namespace map), `SEC-04` (fail-closed bootstrap), `SEC-05` (sign releases).
3. **Stop unbounded growth** — `SYNC-02`/`HUB-11` (event-log compaction + snapshot exchange), `SYNC-06` (tombstone GC), `HUB-12` (hub-side GC).
4. **Harden the engine** — `SYNC-01` (cursor skip), `QUAL-01`/`QUAL-02` (fuzz + property tests), `HUB-13` (HLC-tie cursor).
5. **Grow the product** — the `PROD-*` daemon/TUI/onboarding/clone features that turn the proven loop into a daily-driver tool.

> Effort legend: **S** ≈ <½ day, **M** ≈ 1–3 days, **L** ≈ ~1 week, **XL** ≈ multi-week / cross-cutting.
> Severity legend: **P1** = correctness/security/data-loss risk to ship the cloud-sync loop safely; **P2** = important hardening/scale/UX; **P3** = polish / future-facing.
