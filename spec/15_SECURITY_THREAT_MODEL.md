---
last_reviewed: 2026-07-11
tracks_code: [internal/childenv/**, internal/cli/**, internal/devicekeys/**, internal/envbundle/**, internal/git/**, internal/hub/**, internal/redact/**, internal/state/**, internal/sync/**, internal/logging/**, internal/workspacekeys/**]
---
# Security Threat Model

## Security posture

DevStrap is dangerous if designed casually because it touches code, secrets, Git credentials, and AI agents.

The product should be safe by default and explicit when convenience weakens security.

Local repo-lock and agent-run crash reconciliation pair each recorded PID with an opaque platform process start-time identity when available (`P7-GIT-03`). A recycled PID therefore cannot impersonate the crashed holder and indefinitely preserve a repo lock or a `running` agent row; an unavailable/failed identity lookup remains fail-safe and does not break a lock it cannot disprove.

## Assets

Protect:

- Git credentials;
- SSH keys;
- API keys;
- `.env` values;
- cloud/provider configs (AWS, GCP, Snowflake, etc.);
- private source code;
- draft project contents;
- agent logs;
- device identity keys;
- namespace integrity.

## Trust boundaries

```text
User shell/editor
  ↕
devstrap CLI
  ↕ local socket
devstrapd daemon
  ↕
local filesystem / Git / secret providers
  ↕ network
DevStrap Hub
```

The `devstrapd` daemon and local socket are Phase 1 (gated, not built); today the `devstrap` CLI and `run-loop` cross the filesystem / Git / provider / Hub boundaries directly, so every daemon-plane mitigation below applies to the CLI process itself.

Hub should be treated as semi-trusted:

- can store encrypted blobs;
- can store metadata/events;
- must not see plaintext secrets;
- should not be able to decrypt env bundles.

## Adversaries

Model these actors explicitly:

- compromised Hub that can reorder, replay, omit, or substitute metadata;
- malicious approved device with valid keys;
- compromised but later revoked device;
- malicious agent process running in a worktree;
- local unprivileged process on the same machine;
- network attacker between device and Hub;
- user error during destructive actions.

## Threats and mitigations

### Threat: plaintext secret sync

Mitigation:

- never sync `.env` by default;
- encrypted env capture/hydrate only;
- sync only `env.profile.updated` metadata plus `age_blob:<sha256>` ciphertext, never plaintext;
- runtime injection preferred;
- generated `.env.local` must be explicit;
- permissions `0600`;
- secret redaction in logs.

### Threat: malicious or compromised agent reads secrets

Mitigation:

- no secrets by default for agents;
- child process env starts empty, never inherited wholesale;
- env allowlist resolved only from the bound profile;
- dangerous env names stripped last and unconditionally;
- file denylist;
- isolated worktree plus OS sandbox before public release;
- separate process environment;
- log redaction;
- tainted-log handling when secrets are present.

Reality (`AGEN-01`, `AGEN-02`/`SECU-02`, updated 2026-07-05): the credential-env leak is fixed — `SSH_AUTH_SOCK` is excluded and `HOME` is repointed to the worktree — and the wrapper argv/file policy (still substring-based and interpreter-bypassable on its own) is now backed by a kernel boundary **on macOS and Linux**. macOS wraps the child in a Seatbelt profile (`P4-GIT-03`) that confines writes to the worktree/tmp dirs (the 0600 run log is parent-written and child-untouchable), denies reads of `~/.ssh`-class credential paths — resolving each anchor's leaf symlinks and denying both the literal alias and the symlink-real target so a `~/.ssh -> /elsewhere` alias cannot dodge the deny — and denies all network for `readonly`/`cautious`. Linux first tries bubblewrap with a read-only root, read-write worktree/tmp binds, targeted credential masks, optional net namespace, user namespace, pid namespace, die-with-parent, and new-session protections; userns-restricted hosts now degrade to the Landlock fallback instead of advisory-only. Landlock still enforces the most important fallback boundary — writes and raw `truncate(2)` are confined to the worktree/tmp allow dirs — but it does NOT deny credential reads (that guarantee stays bubblewrap-only), its network deny is TCP bind/connect only at kernel Landlock ABI >= 4 and is not enforced below ABI 4, and it has no mount or pid namespace. **Both Linux backends additionally install a seccomp syscall denylist** (`P4-GIT-03` slice 4): mount, kexec/module-load, ptrace/tracing, keyring, io_uring, and legacy-escape syscalls return `EPERM`, closing the class of kernel-surface and namespace-escape attacks the filesystem/network boundaries do not cover; `clone`/`unshare`/`setns`/`execve`/`fork` stay allowed so nested sandboxes and the agent's own launches work, and `DEVSTRAP_SANDBOX_SECCOMP=off` is the opt-out. `sandbox.violation` telemetry is now live as **unsigned local visibility**, not the signed audit log: `agent_runs` records backend/mode/limitations for every run, and macOS Seatbelt denials are collected from tagged unified-log rows into scrubbed local `sandbox_violations` rows surfaced by `agent show` and `doctor`. Linux runtime denial detection remains future. What the sandbox does NOT yet cover: XPC/`mach-lookup` and unix-domain sockets under `(deny network*)` on macOS — an allow-default profile still lets a `readonly`/`cautious` child talk to system daemons (e.g. out-of-process `nsurlsessiond` transfers) or a local socket proxy, so the network deny is best-effort against a deliberately evasive agent (tightening `mach-lookup` is a follow-up slice); pathname unix sockets on the Linux read-only root remain connectable under `--unshare-net` (e.g. `/var/run/docker.sock`, the Linux analogue of the mach-lookup gap); Linux tmpfs/`/dev/null` masks hide credential paths (ENOENT/empty) rather than returning EPERM; Landlock keeps credential reads readable by design; the seccomp denylist does NOT arg-filter `ioctl`, so `TIOCSTI` terminal injection stays covered only by bubblewrap's `--new-session` and remains open on the Landlock path (no `--new-session` analogue); and the `ptrace` deny also breaks in-sandbox debuggers/`strace` (an accepted trade). Non-credential reads are confinable via `--read-confine` (shipped 2026-07-05, default-on for the `readonly` policy): all three backends restrict reads to the worktree/tmp, OS toolchain roots, and `$HOME` build caches, so the rest of `$HOME` and other projects become unreadable — and because the credential dirs are outside the allow-list, read confinement gives even the Landlock fallback a credential-read boundary. Without it, reads stay allow-default (the documented, opt-in tradeoff, not a defect). `--sandbox require` is the fail-closed mode. **Linked-worktree git grant (`P7-SANDBOX-01`, 2026-07-07):** because an agent worktree is a git *linked* worktree whose index/objects/refs live in the parent clone's `.git` (outside the worktree write-allow), all three backends now also write-allow the linked worktree's `<git-common-dir>/{objects,refs,logs}` and per-worktree admin dir (`git.Runner.WorktreeSandboxWriteDirs`), or the agent's own `git add`/`git commit` are kernel-EPERM'd and `agent pr` has nothing to push. Security-critical detail: the grant is per-subpath, **never the common dir itself** — its `hooks/` and `config` are withheld, because granting write there would let a confined agent plant a hook or config that executes UNSANDBOXED on any later git operation in that clone (a sandbox escape). This is also why the grant cannot be "just bind the whole `.git`": Landlock cannot carve a read-only hole out of an RW grant, so the subpaths are enumerated explicitly.

### Threat: destructive sync deletes code

Mitigation:

- tombstones instead of immediate delete;
- quarantine before purge;
- never delete dirty worktree;
- dry-run;
- audit log.

### Threat: stale branch causes bad agent output

Mitigation:

- resolve the remote default branch, then fetch that upstream before worktree creation;
- record base SHA;
- record the agent diff summary against that base SHA (`BaseSHA..HEAD`) plus any uncommitted residue;
- expose `devstrap worktree status <id>` to re-fetch the recorded base ref and detect drift;
- enforce stale-base check before worktree finalization and agent PR creation;
- never use local `main` or any other local default branch as agent base.

### Threat: malicious or credential-bearing Git remote

Mitigation:

- reject option-like remotes and unsupported schemes before storing or cloning;
- allow only explicit SSH, HTTPS, Git, scp-like, absolute path, and `file://` remotes;
- run git with interactive prompts disabled, bounded command contexts, sanitized environment, and protocol policy that denies `ext::`;
- redact URL credentials from git command and stderr text before surfacing errors.

### Threat: hub compromise

Mitigation:

- hub stores encrypted blobs;
- per-device encryption;
- device revocation;
- event signatures from day one for trust-affecting events;
- HLC ordering and content hashes detect replay/reorder/drop classes when paired with cursors;
- out-of-band fingerprint confirmation before device approval;
- no raw secrets;
- no raw Git mirror by default.

Hub-backend trust model (`HUB-*`): the hub is a **two-plane zero-knowledge store** — (1) a signed, HLC-ordered append-only event log (the namespace map) whose payloads are **envelope-encrypted** (`enc.v2`, XChaCha20-Poly1305 under a per-epoch Workspace Content Key with the full carrier tuple bound into the AEAD AAD, `P4-SEC-02`/`SEC-07`/`P6-SYNC-04`, shipped) and (2) a content-addressed encrypted blob store (`age_blob:<sha256>`) for env values and non-git/draft content. Repo content never transits the hub; it rides git's own transport via blobless clone/fetch from each project's existing remote. The backend is pluggable behind one Hub interface: the chosen production backend is **Cloudflare R2** (S3 API, client-side encryption, namespaced by `workspace_id`) — any S3-compatible store reuses the same interface — and a file-backed local backend exists **only for tests**. Either backend sees only ciphertext plus the signed carrier map — it cannot read code, secrets, drafts, or event payloads.

**Event-log envelope encryption (`P4-SEC-02`/`SEC-07`, shipped):** the `EncryptedHub` decorator wraps the backend Hub so `Push` seals event payloads (Type/PayloadJSON/ContentHash/PrevEventHash) under the current epoch's WCK and `Pull` decrypts them. The WCK is age-wrapped to each approved device recipient and published as a `device.key.granted` event. Adding a device re-wraps the small WCK (one grant per held epoch), never bulk content. Revoking a device mints a fresh WCK at epoch+1 for go-forward forward secrecy. The secret WCK lives only in the OS keychain / 0600 file fallback; SQLite holds only non-secret key/grant metadata (migrations 00013 + 00014, keyed `(workspace_id, epoch, kid)` with an `origin` audit column).

**Metadata leakage residual (`P4-SEC-02`):** envelope encryption hides event payloads (paths, remotes, types, content hashes) from the hub, but the carrier is necessarily plaintext for ordering/dedup/signature verification. A hub operator can still observe: object sizes and counts, the HLC/Seq/DeviceID in event keys, the `device.key.granted` event type (revealing coarse membership and epoch transitions), and blob sizes. This is accepted: the residual is routing metadata, not content.

R2/S3 credential custody (shipped, `P5-HUB-01`; keychain/`op://` resolution shipped, `P6-HUB-02`): the live `aws-sdk-go-v2` S3 adapter is wired behind `hub: r2://<bucket>` (or `s3://`). The bucket and endpoint are non-secret config (the bucket is the URI host; the endpoint comes from the URI `?endpoint=` override or `DEVSTRAP_HUB_S3_ENDPOINT`). The secret access key resolves most-explicit-first (`P6-HUB-02`): `DEVSTRAP_HUB_S3_ACCESS_KEY_ID`/`DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY` env/config — either value may be a 1Password `op://` ref resolved via `op read` under the sanitized child env — then `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` literals, then the per-workspace OS-keychain slot written by `devstrap hub login` (0600 file fallback under `DEVSTRAP_NO_KEYCHAIN`; removed by `hub logout`); never the URI, `state.db`, or logs (the resolved value rides `redact.Secret` and is revealed only at the SDK constructor; the login prompt never accepts the secret via argv). Plaintext env remains the sanctioned CI/override fallback. Auth failures map to `ErrS3Auth` with remediation guidance instead of a raw `SignatureDoesNotMatch`. The adapter is built with `s3.New(s3.Options{...})` (not `config.LoadDefaultConfig`) plus an inline `aws.CredentialsProviderFunc`, so the SSO/IMDS/STS chain and the `credentials` module are never pulled in. The SDK retryer is disabled (`aws.NopRetryer{}`) so `R2Hub.Retry` is the single retry layer (no double-retry or runaway billing loop); throttling/transient S3 errors are retried with capped backoff + jitter, while auth/precondition/not-found errors fail fast. Hosted temporary prefix-scoped credentials remain a documented future (`CredentialMode == "hosted"`).

Git-carrier hub trust model (`AD-1` first slice, shipped 2026-07-04): the private-git-repo backend (`hub: git+ssh://…`) introduces **no new credential plane** — transport auth is the user's existing ssh agent / git credential helpers, resolved by git itself under the sanitized non-interactive child environment (`GIT_TERMINAL_PROMPT=0`, BatchMode ssh); DevStrap never reads, stores, or logs those credentials. The host sees exactly what R2 sees — `enc.v2` event ciphertext, age blobs, sealed snapshots, signed manifests/acks — **plus** git-level metadata (object counts/sizes, push timing, a constant `devstrap` committer identity), so the carrier repo MUST be private. **Carrier rewind threat (`P7-HUB-02`, shipped 2026-07-11):** an accidental host restore, branch deletion, or hostile force-push is detected at the transport boundary using the locally persisted last verified head and retention fingerprint in `head.json`; descendant heads pass, branch deletion is refused, and a non-descendant passes only as plausible compaction: its manifest bytes must be identical to the recorded fingerprint (production compact's parentless squash reuses the pre-squash manifest) or strictly advanced (ProducedAt greater, no floor regression), AND — when the prior head is still in the object database — a content gate verifies the rewrite deletes no event object at or above the new floors, the only deletions `hub compact` performs. This is deliberately not an authenticity oracle: a repo-write attacker can forge a carrier-level plausible advancing manifest, but the sync layer then catches it by signature-verifying the manifest against pinned approved devices and enforcing signed floor monotonicity. Availability against such a writer remains out of scope under the existing dumb-carrier posture. One deliberate weakening relative to R2: object `LastModified` (gc grace windows, the advisory sweep lock's TTL) is carried in client-written timestamp sidecars, i.e. **self-reported**. Destructive age decisions therefore never trust the sidecar alone (adversarial-review hardening, PR #96): every reader keeps a per-clone **observation floor** (first-seen time on the reader's own clock, persisted beside the clone) and reports a blob no older than that floor — so a skewed-slow writer cannot make a just-uploaded blob look past another device's gc grace window, at the fail-safe cost that a fresh clone retains everything for one extra window; the sweep-lock time is clamped the OPPOSITE way (down to the observation floor) so a dead holder's future-dated clock cannot make its lock unbreakable — once a reader has watched the lock for a full TTL it is breakable regardless of its self-reported time. URIs carrying credentials are rejected outright — including an https token in the *username* slot, since the remote URL persists into the carrier clone's `.git/config` — and the rejection error never echoes the URI. The same-machine cross-process clone lock records an immutable PID/hostname/nonce owner (plus the opaque `platform.ProcessStartTime` identity, so a crashed holder whose PID is recycled to a long-lived process cannot wedge the lock forever — P7-GIT-03 semantics, applicable only when a usable process identity is available; identity-less records keep conservative liveness) — the record is staged in full and link-published atomically, so a contender can never observe an empty or torn record whose TTL aging would steal a suspended creator's lock — and is **heartbeated** while held; same-host process liveness is authoritative over mtime, so suspension or laptop sleep cannot cause a live holder's checkout to be stolen. A stale break removes only after identical owner bytes are observed twice, and release removes only its own nonce generation when that generation is observed. The bounded residual is the final filesystem change between verification and `Remove` in either path — concretely, two independent breakers that both verified the same stale record can interleave so the slower one's path-based `Remove` deletes the faster one's freshly created lock (two live holders until the theft is noticed via the stopped heartbeat); nonce entropy makes an identical successor record infeasible, but the filesystem API provides no portable compare-and-delete primitive. Candidate close (follow-up): serialize the decide+remove section with a same-machine `flock` on a sibling breaker file — valid here because the lock file lives on the local filesystem, never a cloud-synced tree. A second accepted residual: a same-host owner that falsely reads alive (Linux start-time identity is boot-relative ticks, so a post-reboot recycled PID at a coincidentally equal offset collides; unsupported platforms record identity 0) wedges the lock until that process exits — the acquire timeout names the pid/host for manual recovery, and the deliberate no-mtime-override for live owners is what keeps a suspended holder's checkout unstealable. A hostile carrier TREE is also confined on the reader's machine: every object file API runs through an `os.Root` handle, so a committed symlink (for example `workspaces -> /etc`) that survives `reset --hard` can neither exfiltrate host files on reads nor clobber them on writes; the marker uses the same handle and retains its explicit symlink refusal. Residually, a hostile writer with push access to a dumb carrier can still rewrite the carrier's own content; integrity never rests on carrier metadata.

Folder-carrier hub trust model (`AD-1` final slice, shipped 2026-07-05): the local-folder / cloud-drive-folder backend (`hub: folder:<abs-path>`) shares the git carrier's `fsObjectStore` posture — the same zero-knowledge object set, the same self-reported timestamp sidecars with the per-clone observation floor and sweep-lock down-clamp, and the same `os.Root`-confined object access — strengthened with **use-time root revalidation**: because the folder root is a replicated/shared directory (unlike the git carrier's private clone), every operation re-resolves the root under the cross-process lock and refuses when it no longer denotes the directory registered at construction. Each object-store call then opens an `os.Root`, verifies that the opened handle still identifies the directory registered at construction, and holds it through all payload and timestamp-sidecar file APIs. A root, parent, or object-key component swapped for a symlink therefore cannot redirect reads or writes; the former check-then-use window is closed at the file-API level. On macOS and Linux, `os.Root` documents no remaining material escape residual for the operations used here (its documented Unix races apply to `Chmod`, `Chown`, and `Chtimes`, which this store does not call). It adds **no new credential plane** (the drive is whatever the OS already mounts) and the lock file + observation floor live in the LOCAL home cache, never in the replicated folder. That local lock shares the git carrier's owner-aware semantics: live same-host PIDs are never broken on mtime alone, dead owners break immediately, legacy/corrupt records age out for upgrade safety, stale break double-checks identical owner bytes, and release is nonce-verified. The remaining decide/remove race is bounded to the final filesystem change after verification in either the stale-break or release path because portable filesystems provide no compare-and-delete operation. One residual weakening relative to R2 and the git carrier: there is no cross-writer linearization point (no atomic push-ref CAS, no conditional PUT), so cross-DEVICE conditional writes (retention manifest, sweep-lock acquisition) are **best-effort** — the cross-process lock only serializes same-machine processes, and two devices writing simultaneously through the drive can each "win", which the drive surfaces as a conflicted copy. This is the same advisory-cooperation residual class as the sweep lock's byzantine residuals below, acceptable because the folder carrier targets the single-user, few-devices, rarely-simultaneous case and every object is content-addressed or `(device,seq)`-unique, so ordinary convergence never collides. **Mutable object durability (`P7-HUB-05`, shipped 2026-07-10):** `fsObjectStore.PutObject` / `PutObjectIfMatch` (and the timestamp sidecars that ride the same shared tree) write via a root-relative same-directory temp + `fsync` + `os.Root.Rename` + a best-effort root-relative directory `fsync`, so a crash mid-write no longer leaves a truncated single-key head object (`retention.json` / `sweep.lock`) that would fail-close every device's `Pull` at `retentionFloors`, and a crash immediately after `Rename` is far less likely to revert the directory entry to the prior generation. Rename atomicity is per-filesystem only; it **reduces but does not eliminate** the cloud-drive mid-replication window (Dropbox/iCloud/OneDrive may still observe or replicate the new inode while the client is uploading) — content-addressed blobs/events remain hash-safe either way; the residual for mutable heads is accepted and documented rather than claimed closed. Orphaned `.tmp-*` writer temps are never surfaced as objects and are reclaimed once safely stale (an hour old) so a crash-abandoned temp cannot accumulate indefinitely in the shared folder.

R2/event and blob push concurrency (`P6-HUB-03`, shipped): bounded errgroup fan-out changes throughput, not trust semantics. `R2Hub.Push` still validates every event has a positive per-device Seq before any PUT, uses conditional event-object writes as the idempotent append guard, treats precondition failures as duplicate no-ops, and returns one aggregate error for the batch. `runSyncCycle` advances the push watermark only after `hub.Push` returns nil for the whole batch, so a mid-batch failure leaves the watermark unchanged and the next sync replays the same batch safely; successful duplicates collapse at the hub. Referenced blob pushes are also unordered bounded fan-out because blobs are content-addressed ciphertext and carry no event-log ordering invariant. `env.profile.updated` is trust-affecting and must verify like destructive namespace events: it controls hydrated files and runtime env injection, and its referenced env blob rides the same hash-verified blob plane as draft snapshots.

Residual risk: a malicious approved device can decrypt bundles it is authorized to receive until revoked, and **age has no native revocation**. Bound this by per-profile recipient scoping, re-encrypting every affected env/draft bundle to the reduced recipient set after revocation, emitting superseding `env.profile.updated`/`draft.snapshot.created` events before hub cleanup so peers never replay deleted refs, and requiring provider/service-side value rotation for secrets that may already have been exposed.

Reality (`SECU-03`/`SECU-05`/`HUB-03`): event signature verification **fails closed once any device has ever been enrolled** — `verifyEventSignature` requires a valid signature from a known, approved, non-local device for **all** event types once `hasEnrolledDevices` is true; unknown devices, devices with no signing key, and non-approved devices are rejected (not applied). Enrollment is **sticky** (`P6-SYNC-03`, shipped): `hasEnrolledDevices` counts `trust_state IN ('approved','revoked','lost')` — a revoked/lost row proves a deliberate local operator trust decision (no sync/remote path can inject one) — so revoking or losing the last approved device keeps the window closed — post-revoke traffic from the revoked (or any unknown) device quarantines instead of applying. The local device is exempt from the signing-key requirement (pre-enrollment grace). Destructive/trust-affecting event types (`project.deleted`, `project.renamed`, `env.profile.updated`, `device.revoked`, `device.lost`) require verification unconditionally. The remaining gap is the **pre-enrollment bootstrap window** (`SEC-04`), now narrowed: the **joiner half is closed** by the documented founder-pinning ceremony — a keyless joiner runs `init --join --code <founder-code> --fingerprint <founder-fp>` or `devices enroll <founder-device-id> … --approve` BEFORE its first sync; the grant path is founder-gated so the joiner mints and grants nothing, but the approved founder row flips `hasEnrolledDevices`, so `verifyEventSignature` and `EncryptedHub.Verify` fail closed before the joiner's first pull (pinned in `devices_pin_founder_test.go` and the `sync_join_flow` e2e). In a fleet with more than one existing device the joiner pins **every** existing approved device the same way — device records are not synced, so events signed by a device the joiner has not yet pinned quarantine as `event_verification_failure` conflicts and are replayed automatically when that device is later enrolled and approved (the `devices approve` replay path; recoverable and visible in `conflicts list`, never silently lost). Before pinning (or on a founder before any peer is approved), non-destructive events from unknown devices are still accepted so a fresh device can sync its first tree; the residual was authenticated full-state snapshot (shipped, `P4-SYNC-02`) and remote trust propagation (shipped, TRUST-01 — revocations sync; approval stays local by design), not the local pairing ceremony. The hub must be treated as **zero-knowledge / semi-trusted** (ciphertext + routing metadata only); mTLS device certs should enforce revocation at the transport layer.

Multi-tenant isolation (future SaaS direction, `SCALE-*`): when the hub serves more than one owner, **confidentiality** is by construction — every blob and event is client-side age-encrypted before upload and namespaced by `workspace_id`, so a zero-knowledge hub cannot decrypt across tenants even if its access controls fail. Integrity and availability are not automatic: a leaked bucket-wide key can still delete, overwrite, withhold, or reorder ciphertext. Hosted mode therefore requires prefix-scoped temporary credentials, signed hash chains, fail-closed verification, snapshots/backups, retention discipline, rate limits, and cell/tenant scoping.

### Threat: hub key-substitution defeats envelope confidentiality (`P6-SEC-01`, mitigated once enrolled)

Attacker = the untrusted/zero-knowledge hub (or a MITM/revoked device). Because age encryption to a public X25519 recipient needs no secret and every device's recipient string rides the hub as plaintext, a hostile hub can forge a `device.key.granted` grant that wraps an **attacker-chosen** Workspace Content Key to the victim's own recipient. Before `P6-SEC-01(a)`, `EncryptedHub.Pull` ingested grants from the raw hub batch before any signature/trust check, so a forged high epoch could become the active `Push` epoch and a low-epoch variant could overwrite the legitimate WCK.

Mitigation status: **step (a) shipped** — `EncryptedHub.Pull` now runs a `Verify` seam (`internal/sync/encryptedhub.go`, wired by `hubFromOptions` to `(*state.Store).VerifyRemoteEvent`) on every grant carrier *before* `IngestGrant`, so once any device is approved a grant from an unknown/unapproved/bad-signature device is refused and never reaches the keyring; the refused carrier still flows to `ApplyEvents` and lands in the `event_verification_failure` quarantine (one visible conflict, not silent). This shares the apply-path trust regime exactly, so no new trust policy is introduced and the pre-enrollment bootstrap window (`P4-SEC-04`) is the only residual open-ingest path. **Steps (b)/(c) shipped (PR-3b):** keys are addressed by `(epoch, kid)` with `kid = hex(sha256(wck))` (full digest), so a grant can never displace an existing key — distinct keys land in distinct slots (a same-slot custody rewrite additionally byte-compares and refuses a mismatch), `IngestGrant` rejects a carried kid that disagrees with the unwrapped bytes, and every key row records its `origin` (`self` bootstrap/rotate, verified `grant`, or migration `legacy` — the only write paths). Push-key selection prefers verified `grant`-origin keys, so a forged or stale self-mint can no longer become the push key once the fleet key arrives. **Kid binding (shipped with `enc.v2`, `P6-SYNC-04`):** the envelope's kid FIELD remains a candidate-ordering hint — every held key at the epoch is tried before deciding, so RELABELING a decryptable event's kid neither wedges nor loses it (post-#33 review, fable-5) — while the SEALING key's kid is bound into the AAD (derived from the candidate on decrypt), so a ciphertext only ever authenticates under the exact key that sealed it. STRIPPING the kid from an event a colliding-key holder cannot decrypt no longer drops it silently OR permanently: the AEAD failure forwards the carrier to a visible `undecryptable` quarantine conflict, and every subsequent pull replays open undecryptable conflicts against the keys held then (`ReplayUndecryptableConflicts`, wired into `pullAndApplyEvents`) — once the real grant lands, the carrier decrypts, applies through the normal verified path, and the conflict auto-resolves. A hub tampering with the kid hint can therefore only DELAY a not-yet-granted event, never destroy it (post-#44 review fix, gpt-5.5 Major). The pre-enrollment bootstrap window (`P4-SEC-04`) remains the residual open-ingest path — and note its full extent (post-#33 review, gpt-5.5): a grant ingested during that window records `origin='grant'` and is therefore push-preferred, so until the first device is enrolled a hostile hub can still hand a fresh joiner an attacker-known key. Closing it is the P4-SEC-04 out-of-band fingerprint work, not a keyring change.

### Threat: hub tampers with unauthenticated carrier fields (`P6-SYNC-04`, mitigated)

Attacker = the untrusted hub. Under the retired `enc.v1`, the AAD bound only `event.ID || epoch` and the signature omitted `DeviceID`/`Seq`, so a hostile hub could rewrite `Seq` (forcing an `ErrEventHashChain` soft-wedge that held the cursor forever) or re-attribute `DeviceID` (corrupting the conflict tiebreak) without breaking AEAD or the signature. **Mitigated (`enc.v2`, shipped 2026-07-03):** the AAD now binds the full carrier tuple (`ID`, `DeviceID`, sealing-key kid, `Seq`, `HLC`, epoch — length-prefixed/big-endian), so any carrier mutation is an AEAD authentication failure at decrypt time; the failure forwards the carrier to an `undecryptable` quarantine conflict (permanent-class — it never holds the cursor — but auto-recovered by the undecryptable replay once the key arrives; visible, blocks `hub gc`) instead of a silent skip. The signature domain moved to `devstrap:event:v2` with `device_id` + `seq` in the payload; verification accepts v2 then falls back to v1. **Residual:** v1-signed historical events (re-pushed verbatim when a hub is re-founded) lack the DeviceID/Seq *signature* binding. Every enc.v2 event gets the AAD binding regardless (possession-based, so it also covers the pre-enrollment window) — the one plaintext-plane exception is `device.key.granted` carriers, which are never enc.v2-wrapped, so a *legacy v1-signed* grant's `Seq` is bound by neither AAD nor signature (its DeviceID is still caught by the signing-key lookup; all grants this build creates are v2-signed). Reconciles with open `P4-SYNC-05`, which would extend Seq/HLC binding to a folded hash chain.

### Threat: hub withholds an event while serving a forgery at its slot (`P5-SYNC-01`, documented residual)

Attacker = the untrusted hub, combining WITHHOLDING (never serving a real event object) with a FORGED undecryptable carrier claiming the same `(device, seq)` slot at a held epoch. The per-device Seq cursor treats a quarantined carrier as consumed (its quarantine is durable and replayable), so the cursor advances past the withheld slot and the real event — if the hub ever returns it — is below the cursor and no longer pulled; the origin device's successor then holds forever on its hash-chain break. Contained: this needs a byzantine hub (plain withholding without the forgery self-heals — the slot is a gap, the cursor holds, delivery resumes when the hub stops withholding), it is LOUD (a durable `undecryptable` conflict that blocks `hub gc`, plus per-device cursor-held warnings on every sync), and integrity is never affected — no unauthenticated content applies. Deliberate trade-off: the alternative (holding on every sole-occupant undecryptable slot) would let one genuinely corrupt object wedge its origin device forever, the exact `P6-SEC-03`-class failure the quarantine path exists to remove. A hub that can withhold objects can always deny availability; the residual here is only that this specific combination converts a recoverable delay into a permanent per-device gap. That gap is now recoverable end-to-end via the shipped snapshot exchange (`P4-SYNC-02`) and the shipped `hub compact` producer (`P4-HUB-11`): once a `hub compact` publishes a floor above the stranded slot, the affected device's next pull returns `ErrSnapshotRequired` and it re-bootstraps its state from the signed snapshot, past the withheld object — no re-founding required. The recovery path is real end-to-end (producer + import both shipped), not just a documented future.

### Threat: a withheld, stale, or forged sync ack subverts tombstone GC (`P4-SYNC-06`, by design)

Attacker = the untrusted hub (withholding or serving stale ack objects) or a revoked/unknown device (forging one). Tombstone GC in `hub compact` is gated on signed **sync acks** (`meta/acks/<device_id>.json`), so a tampered ack plane is a plausible attack surface. It is **availability-only, never integrity**: (1) a **withheld** ack from any approved device makes compact SKIP tombstone GC entirely (a missing peer ack is not treated as consent), so the worst outcome is retained tombstones — the log stays correct, just larger. (2) A **stale** ack (an old object the hub keeps serving) can only carry a LOWER HLC watermark, which lowers the `min` and again retains more tombstones; it can never raise the floor to purge a tombstone a device has not actually consumed. (3) **Forging** an ack requires an approved device's private Ed25519 signing key: acks are verified under `devstrap:ack:v1` against `ApprovedDeviceSigningKey`, and acks from revoked/lost/pending/unknown devices — or with a bad signature, a device/workspace mismatch, or a parse error — are ignored, so a revoked device cannot pin GC and a hostile hub cannot mint consent. The integrity guarantee rests on the **tombstone-safety clock**, not on the hub: an ack is written only after a fully-clean cycle whose push watermark reached the device's local max `Seq`, so every event that device mints later is strictly above its acked watermark and no device can produce a resurrecting add below the minimum acked watermark. A hub can therefore only DELAY reclamation (deny availability, which a zero-knowledge transport never guaranteed against omission), never resurrect a deleted path or purge a still-live one.

### Threat: a wrong or hostile `workspace_id` (`P4-SEC-07` pairing, by design)

Attacker = a mistaken operator, or the untrusted hub steering a device onto the wrong prefix. Devices converge only when they share one `workspace_id`, which keys every hub object under `workspaces/<workspace_id>/` (`19_CLOUD_PROVISIONING_GUIDE.md` §E). The id is a **prefix selector, not an authenticated field**: it is **excluded from event signatures by design** (remote events are re-stamped `WorkspaceID=""` on apply, so signing it would break verification across devices), and it is exchanged out-of-band alongside the enrollment key exchange (Syncthing-style — a non-secret identifier whose authorization comes from key verification, not from the id itself). This is safe because it selects a namespace, it does not grant one: pointing a device at a **wrong** id yields an empty prefix (no content), and pointing it at a **hostile** id yields only ciphertext the device cannot decrypt without a granted WCK, and events it cannot verify without the founder's pinned keys — so a bad id surfaces as an empty pull or a quarantined `event_verification_failure`/`undecryptable` conflict, never as accepted content or another workspace's plaintext. Content protection is therefore carried entirely by fail-closed signature verification, the `enc.v2` AAD binding, and the founder-pinning ceremony (`E.2`), not by the id. Residual: the pre-enrollment bootstrap window — a keyless joiner that has not yet pinned the founder still accepts non-destructive events; the local pairing ceremony that closes it is fully shipped (`P4-SEC-04` parts 1+2: fingerprint confirm + pairing codes). Bootstrap-time **state acquisition** is now signature-authenticated end-to-end (`P4-SYNC-02`): a device that bootstraps from a hub snapshot verifies the retention manifest against a locally pinned approved device before importing, so the snapshot path never widens the open-ingest window. Remote REVOCATION propagation is now shipped (TRUST-01, `device.revoked`/`device.lost`); approval deliberately stays a local ceremony, so the remaining work here is nil.

### Threat: one bad signed event wedges every device's sync (`P6-SYNC-01`, mitigated)

Attacker = a revoked/lost device that keeps pushing signed events (or a hub replaying one). Previously any signature/trust failure in `ApplyEvents` aborted the whole batch before the cursor advanced, so a single poisoned event re-pulled and re-failed forever — a fleet-wide availability break requiring no key compromise. **Mitigated:** permanent verification failures (signature/trust/content-hash, wrapped in `state.ErrEventVerification`) and divergent duplicate IDs are now quarantined per-event as `event_verification_failure` conflicts (full event JSON retained for replay) while the rest of the batch applies and the cursor advances safely; `devices approve` replays a newly-approved device's quarantined events. The synced trust event is now shipped (TRUST-01): `devices revoke`/`lost` emit `device.revoked`/`device.lost` (mustVerify, same-tx with the local flip), every receiving device applies it sticky/monotonically (local device exempt; approval never propagates — it stays the local P4-SEC-04 ceremony) and flags `secret_bindings.needs_rotation` once. Residuals: mutual revocation across different pull windows can leave bystanders divergent (fail-closed either way, loud, recovered by the two-step local re-approval — re-approving one side replays its counter-revoke); a FAILED WCK rotation during revoke leaves the old epoch active, so events pushed before a later successful rotation (including the revoke event itself) stay readable by the revoked device — the trust flip is deliberately kept (refusing the revoke would keep a compromised device approved, which is worse), and the exposure window is now bounded by the next SUCCESSFUL rotation (issue #134, shipped): the revoke records the owed rotation in a machine-local `wck_rotation_pending` marker, every `devstrap sync` cycle's rotation gate retries it — even with `keys.rotate_max_age=0`, since disabling periodic rotation must not disable committed revoke containment — and the marker resolves ONLY on this device's own successful `Rotate`, never on merely observing a newer epoch (adversarial-review HIGH: an uninformed peer's age rotation can grant the new epoch to the revoked device, so "newer epoch" is not proof of exclusion). `P7-SYNC-04` closes the fleet gap in this containment: the same `wck_rotation_pending` marker is armed on every device that LEARNS of the revoke — a synced `device.revoked`/`device.lost` apply or a snapshot import (`SetWCKRotationPendingTx`, guarded on epoch>0, storm-guarded to preserve the original owed-since) — so if the revoker's own rotation failed and it went offline, the next device to sync still closes the epoch. Because a newer epoch is not proof of exclusion, each learner rotates once; the cost is bounded (one rotation per device per distinct revocation — grants never arm the marker), terminating, and forward-secure. P7-SEC-02 additionally records the full rotation/secret-flag/blob-rewrap containment obligation in `revoke_containment_pending` inside the SAME transaction as the trust flip and synced event; `devstrap sync` resumes that sequence after pull, and `doctor` names every pending device and timestamp, so a crash or early `CurrentEpoch` failure cannot leave distrust durable with no containment record. An early retry failure warns and lets the cycle continue so the `device.revoked` event still propagates (aborting would keep the fleet ignorant of the revoke — the availability regression the adversarial review flagged); a mid-commit failure (epoch advanced, grants possibly unpublished) stays fatal for the cycle. `devices revoke` preflights the remaining approved recipients so the likeliest failure (a malformed recipient row) is named before the trust write, and `doctor` warns `workspace key rotation` while the rotation is owed; bounded conflict-row aggregation for a still-pushing revoked device. Revocation now also **survives event-log compaction** (`P7-SYNC-01`, snapshot.v2): terminal trust rows (`revoked`/`lost`) ride in the sealed snapshot and are re-derived on import via the same sticky/monotonic apply, so a device offline across the revoke-to-compaction window learns the revocation on snapshot recovery instead of permanently keeping the revoked device approved; the snapshot version bump is fail-closed in both directions (an old binary refuses v2; this binary refuses trust-less v1 snapshots with a re-compact remedy). Accepted residual (same class as the replayed counter-revoke): a stale snapshot's trust row re-flips a device the importer re-approved locally after the snapshot was produced — fail-closed, loud, recovered by the local re-approval ceremony.
**Related (owned elsewhere):** `P6-HUB-01` — hub-side blob GC data-loss (availability, owned in `spec/03`, shipped); `P6-SYNC-03` — revoking the last approved device previously reopened the fail-open bootstrap window (owned in `spec/07`, **shipped**: sticky enrollment, see above).

**Advisory sweep lock (`P4-HUB-12`, shipped).** The destructive hub passes (`hub gc`, `hub compact`, `hub migrate-events`) coordinate through an advisory lock object (`meta/sweep.lock`, a create-only conditional PUT with a 1h TTL and one stale-break-and-retry). This is an **availability/consistency aid for COOPERATING clients only, never a security boundary**: a hostile writer that ignores the protocol — deleting the lock, holding it forever, or writing a bogus one — is explicitly out of scope, exactly as for every other object on the zero-knowledge hub (the hub is already trusted for nothing but availability). Its staleness judgment reads the object's **backend mtime**, never the lock body's self-reported time, so a clock-skewed or malicious `acquired_at` cannot extend a lock past its TTL; the worst a lock-plane attacker achieves is the DoS a hub can always mount by denying availability. The same change closes the `P6-HUB-01` dedup-`PutBlob` residual **end-to-end**: a content-addressed blob re-referenced by a late recovery sync refreshes its `LastModified` (byte-identical re-put / mtime bump), AND `hub gc` re-stats (`StatBlob`) each candidate immediately before deleting it, so a refresh that lands AFTER gc's `ListBlobs` snapshot is still honored and the just-re-referenced blob survives. The refresh alone was insufficient — gc held a stale mtime from its pre-sweep list — and the sweep lock does not help here (it serializes sweepers, not the syncing device racing them); the two together close it. An availability fix, integrity was never in question.

### Threat: hub epoch-injection denial of service (`P6-SEC-03`, mitigated — grace-bounded)

Attacker = the untrusted hub (or an approver whose grant never propagated). Pre-fix, `EncryptedHub.Pull` truncated forever at the first event sealed under an `(epoch, kid)` this device was never granted — one well-formed `enc.v2` object naming a bogus high epoch, or a forged random kid at a held epoch, stalled a device's sync permanently with no key compromise; the same wedge hit legitimately-approved devices whose approver lagged a rotation. **Mitigated:** the missing-key defer is now bounded by `sync.key_grant_grace` (default 72h, `0` = immediate). The first sighting is persisted (`key_grant_waits`, stable first-seen; the grace clock is the earliest first-seen across every kid at the epoch, so per-pull kid relabeling cannot restart it); past the window the still-encrypted carrier is quarantined as a visible, `hub gc`-blocking, replay-recoverable `undecryptable` conflict and the cursor advances. An injected-garbage epoch therefore costs at most one bounded delay plus one open conflict (resolvable via `conflicts resolve`); a real-but-late grant recovers its events automatically via `ReplayUndecryptableConflicts`. The `devices approve` contiguity guard (`--allow-epoch-gap` to override) stops an incomplete approver from propagating the gap, and `doctor` surfaces every open wait ("awaiting key grants"). **Residuals:** a hostile hub that keeps serving ciphertext at a validly-shaped never-granted (epoch, kid) can pin an open wait row — a standing `doctor` warning and an approval-guard trip (`--allow-epoch-gap` overrides) — but no longer a sync wedge (malformed kids and phantom kid rows are excluded: missing-epoch waits are epoch-level and non-canonical kids quarantine without a wait). A rotator grants only to approved devices it knows locally, so a device unknown to it always rides the grace→quarantine→replay path after a rotation until re-approved; old-epoch containment is now **narrowed** by the shipped `hub compact` producer (`P4-HUB-11`): a snapshot is sealed under the CURRENT-epoch WCK, so a fresh joiner that bootstraps from it never needs a retired epoch's key, and each compaction is the natural retirement boundary for pre-rotation ciphertext once the events carrying it are deleted. The documented-not-built remainder is only the containment of history EXISTING holders already have (re-encrypting or rewrapping long-retired-epoch ciphertext they retain). A hostile hub can still WITHHOLD events (zero-knowledge transport was never an integrity guarantee against omission — see `P6-HUB-04`).

### Threat: long-lived workspace content key (silent compromise is permanent) (`P4-SEC-07` remainder, mitigated — periodic rotation)

The WCK is a symmetric group key; before periodic rotation, a silently exfiltrated key read every future namespace event until a revoke happened to rotate the epoch. **Mitigated (shipped):** `devstrap keys rotate` and the `sync`-integrated age trigger (`keys.rotate_max_age`, default 90 days, `0` disables) bound the FORWARD exposure window: once the fleet converges on the new epoch, the old key stops decrypting new traffic. This is the standard periodic-epoch-rotation posture (Megolm/MLS-style) and it is **forward exposure only** — no retroactive protection: everything sealed under a compromised epoch stays readable to whoever holds it, and rotation deliberately performs none of revoke's containment (no secret flags, no blob rewrap). For a KNOWN compromise use `devices revoke`. Documented-not-built residuals: **old-epoch containment** (re-encrypting or rewrapping history EXISTING holders retain under long-retired epochs — now narrowed by `hub compact`, which seals snapshots under the current epoch so fresh joiners never need a retired key and each compaction retires the pre-rotation ciphertext it deletes, `P4-HUB-11`) and **keychain-slot growth** (every historical epoch's WCK stays in the OS keychain / file store forever so history remains decryptable; unbounded but tiny — one 32-byte key per epoch, ~4/year at the default cadence).


### Threat: MITM/tamper on the untrusted pairing channel (`P4-SEC-04`, closed locally)

Attacker = anything that can alter the seven values a device pastes during enrollment (age recipient, signing key, workspace id, …) as they cross the out-of-band channel — a swapped recipient, a compromised copy-paste path, or a hostile hub steering the exchange. Substituting the enrollee's keys would let the attacker be pinned instead of the intended device, defeating the founder-pinning that fail-closed verification depends on.

**Mitigation (shipped):** approval is gated on out-of-band confirmation of a **device fingerprint** that binds *both* the Ed25519 signing key and the age recipient — `sha256("devstrap/device-fp/v1" || 0x00 || canonicalSigning || 0x00 || canonicalRecipient)`, rendered as unpadded uppercase base32 in 13 dash-separated groups of 4. Both inputs are canonicalized (parse-then-re-encode) so cosmetic encoding differences do not change the value. `devices approve`, `enroll --approve`, and `init --join --code --fingerprint` compute the fingerprint from the row/flags/code being approved (never the local keystore) and refuse the trust-state change unless it is confirmed — via `--fingerprint <value>` (constant-time compare), an interactive `yes` on a TTY, or, for `devices approve`/`enroll`, a non-TTY hard refusal with a copy-paste remedy. The operator reads the fingerprint off the far device (`devices recipient --fingerprint`) or its `devices pairing-code` stderr and compares character-for-character. Because the channel is untrusted, this is a **full 256-bit fingerprint, never a short authentication string (SAS)**: a truncated code would let an attacker grind a colliding key pair offline and pass the comparison, so no truncation is offered. `SECU-05`: a keyless placeholder row cannot be approved at all (nothing to bind).

`P4-SEC-04` part 2 is now shipped: `devices pairing-code` emits a one-paste `devstrap-pair1:` blob carrying workspace id, device id, display name, OS, arch, age recipient, and signing public key; `init --join --code` adopts the workspace id and pins (or pending-enrolls) the founder; `devices enroll --code "$CODE" --approve --fingerprint "$FP"` performs founder-side enrollment in one command. The blob is **deliberately unauthenticated** and carries no fingerprint. The fingerprint binds ONLY the two keys (it must stay byte-compatible with part 1's `devices recipient --fingerprint`), so the confirmation catches any key substitution — the only tampering that could forge trust. The other carried fields are NOT fingerprint-bound; tampering with them cannot mint trust but can break the ceremony **visibly**: a swapped device id pins an approved row no real signer matches, so the founder's events quarantine (`event_verification_failure`, replayed after a correct re-pair) while fail-closed verification stays engaged; a swapped workspace id lands the joiner on a wrong/empty hub prefix, detected by `doctor --remote`'s `workspace id match` heuristic, and even a hostile prefix yields only quarantined ciphertext (the pinned founder keys are the real ones — anything else failed the fingerprint); name/os/arch are cosmetic labels. `init --join --code` prints every carried field next to the confirmation prompt and tells the operator to cross-check the workspace id against `devstrap status` on the founder, and `UpsertDevice` refuses the local device id at both the CLI and SQL layers, so a blob cannot clobber the local identity row. Remedy for any tampered-field breakage: re-run the ceremony with a fresh code. This closes the local pairing-channel residual from parts 1+2; authenticated snapshot exchange is now shipped (`P4-SYNC-02` — a bootstrapping device verifies the snapshot's retention manifest against a locally pinned approved device before importing), and remote revocation propagation is shipped (TRUST-01); approval deliberately never propagates.

### Threat: split-custody wedge mints a divergent device identity on headless Linux (`P6-XP-04`, mitigated)

Attacker = none; this is a self-inflicted availability/integrity failure with a confidentiality edge. Pre-fix, `HybridStore` classified keyring errors by `err.Error()` substring and collapsed both "secret genuinely absent" and "backend unreachable" into `os.ErrNotExist`. On headless Linux (the future `service install` target — cron/systemd units run with no D-Bus session bus), any event-stamping command (`add`, `scan --adopt`, `sync`, `run-loop`) hit an unreachable Secret Service, was misread as "no key," and `EnsureSigning` minted a **second** signing identity into the `0600` file store without consulting the device's already-published `devices.signing_public_key`. The SQL guard then rejected the mismatch — but only after the divergent key file was on disk, wedging every subsequent headless run (and `run-loop` aborting after 5 consecutive failures) while desktop runs kept working; recovery required manually deleting the orphan file. The outcome was also error-string-dependent (a dead socket vs. an unset address diverged into opposite paths). **Mitigated (shipped, see `spec/09` key-custody model):** classification is now typed at the platform seam (`ErrUnsupported` for unreachable vs. `ErrSecretNotFound` for absent), the mint paths refuse to generate a divergent key when a public key is already published or the keychain is merely unreachable (remedy: desktop session, or `DEVSTRAP_NO_KEYCHAIN=1` + migrate the key file), and a one-time init probe records the custody backend (`local_meta.key_custody`, migration `00016`) so a `keychain`-custody store fails closed instead of silently minting a file identity. The same never-mint-over-held rule guards the WCK custody path. **Residuals:** a store initialized on a desktop session and later run headless without `DEVSTRAP_NO_KEYCHAIN=1` fails closed (by design — it will not silently downgrade), so the operator must choose file custody explicitly; the confidentiality edge (a divergent key persisted in plaintext) is removed at its source since no divergent key is minted.

### Threat: device lost/stolen

Mitigation:

- revoke device;
- rotate env bundles;
- OS keychain/Secret Service for private keys, with `0600` file fallback only when keyring is unavailable;
- optional passphrase lock;
- no plaintext secrets unless explicitly hydrated.

### Threat: symlink escape leaks files into draft sync

Mitigation:

- do not follow symlinks by default;
- detect symlink targets;
- block escapes from managed root;
- explicit allow rules only.

### Threat: path spoofing/case conflicts

Mitigation:

- normalized path key;
- reject case-only siblings;
- portable path policy;
- conflict records.

### Threat: command injection through project config

Mitigation:

- bootstrap commands require approval by default;
- show command before running;
- trusted profiles only auto-run;
- never execute commands from untrusted draft without prompt.

### Threat: malicious env variable names

Mitigation:

- block dangerous env names by default;
- warn for shell-sensitive variables;
- reject dangerous names even when allowlisted.

Danger examples:

```text
LD_PRELOAD
DYLD_INSERT_LIBRARIES
BASH_ENV
NODE_OPTIONS
PYTHONPATH
GIT_SSH_COMMAND
```

Current implementation centralizes this in `internal/childenv` and wires it into Git subprocesses, editor launches, and generic agent commands. Generic agent runs receive no project secrets by default and apply wrapper-level command and file path policies that deny obvious destructive commands, secret-reading commands, explicit sensitive paths, and explicit outside-worktree paths unless `--policy yolo-local` is selected. macOS Seatbelt and Linux bubblewrap now provide default-on OS confinement where available; env-profile-scoped secret injection remains future work.

### Threat: daemon privilege escalation

Mitigation:

- run as user LaunchAgent/systemd user service;
- no root in MVP;
- socket restricted to user;
- state dir `0700`;
- logs avoid secrets.

## Secret handling rules

1. Secret values never appear in event payloads.
2. Secret values never appear in logs.
3. Secret values are encrypted before leaving device.
4. Device must be approved before receiving encrypted env bundle.
5. Agents receive no secrets unless profile allows.
6. Plaintext env files are generated only by explicit command.
7. `state.db` stores secret references only; encrypted personal values are addressed as `age_blob:<sha256>`.
8. `provider_ref` and `encrypted_value_ref` are mutually exclusive.

## Audit log

**Status: NOT implemented.** There is no `audit_log` table in `internal/state/migrations` and no code records these events; only sync `events`, `conflicts`, and `agent_runs` exist. Destructive and trust-affecting actions currently leave **no signed audit record** — a security-relevant gap. Build the table + recording + Ed25519 signing for the trust-affecting subset below.

Record:

- project added/renamed/deleted;
- env captured/hydrated;
- device approved/revoked;
- worktree created/deleted;
- agent run started/completed;
- PR created;
- destructive action requested;
- conflict resolved.

Do not record:

- secret values;
- full env dumps;
- raw private key contents;
- raw token-bearing command output.

Trust-affecting audit events are signed with the device signing key:

```text
device.approved
device.revoked
device.rotated
env.captured
env.bundle.reencrypted
policy.network_grant
sandbox.violation
worktree.created
agent_run.started
agent_run.completed
```

Event signatures cover `(id, hlc, type, payload_json, content_hash, prev_event_hash)`.

`sandbox.violation` is live today only as unsigned local telemetry (`agent_runs` sandbox columns plus `sandbox_violations` rows for macOS Seatbelt denials). Recording it in this signed audit-log stream remains future work with the rest of the audit-log subsystem.

Current implementation creates a local Ed25519 signing identity during `devstrap init`, stores only the public key in `devices.signing_public_key`, stores private signing material through the platform keychain adapter with `0600` file fallback, signs local events, and verifies signed inserts when the source device's signing public key is known. Manual remote-device enrollment/approval and one-paste `devstrap-pair1:` pairing codes are available for local env capture recipients. Full-strength device fingerprints, compare-and-confirm approval, and pairing-code enrollment are shipped (`P4-SEC-04` parts 1+2); automatic remote enrollment and signed hub ingestion remain future work.

Key-custody status (`SECR-04`/`SECU-01`, refined by `P6-XP-04`): the file fallback is gated on **typed** keychain reachability (`platform.ErrUnsupported` vs. `ErrSecretNotFound`, no error-string matching), a present-but-failing keychain fails closed, the mint paths never generate a divergent identity over an already-published key or an unreachable keychain, and the custody backend is recorded once at init and honored thereafter (`local_meta.key_custody`; `doctor` reports it). The headless-Linux split-custody wedge is closed (see the threat above); remaining coverage risk is live Linux Secret Service integration under a real daemon (`XP-03`). Event-verification fail-closed-once-enrolled is implemented (`HUB-03`); the pre-enrollment bootstrap window (`SEC-04`) is narrowed — the joiner half is closed by the founder-pinning ceremony, the fingerprint compare-and-confirm gate is shipped, and the one-paste pairing code + founder-side enrollment automation are shipped (`P4-SEC-04` parts 1+2, see the pairing-channel threat above).

## Security profiles

### personal-relaxed

- encrypted env sync allowed;
- `.env.local` generation allowed;
- agent yolo mode allowed with warning.

### personal-normal

- encrypted env sync allowed;
- `.env.local` generation explicit;
- guarded agent default.

### team-strict

- external secret manager only;
- runtime injection only;
- no plaintext env files;
- command policy required;
- audit log required.

## Security decisions and remaining questions

- Personal encrypted env uses age v1 with per-device X25519 recipients. Current implementation creates the local recipient, keeps the private identity out of SQLite/config, and stores it through the OS keychain/Secret Service when available; encrypted bundle sync remains future work.
- Trust-affecting hub event payloads must require known, approved device signing keys before hub sync ships. Local event signing is wired; remote signing-key enrollment and approval are not.
- Agent command execution should be mediated through a PTY proxy before team mode.
- OS sandboxing is required before public release — macOS Seatbelt, Linux bubblewrap, Linux Landlock fallback, the Linux seccomp syscall denylist, unsigned local `sandbox.violation` telemetry, and tighter read confinement (`--read-confine`) shipped 2026-07-05 (`P4-GIT-03`, default-on via `--sandbox auto`); the named remaining trio is complete, leaving only the containerization direction.
- Remaining question: should DevStrap refuse to manage repos with secret-looking tracked files, or warn and require explicit adoption?

## Audit implementation notes (2026-06-28)

- **SECU-01**: Key custody fallback now gated on `IsKeychainUnavailable(err)`; present-but-failing keychain fails closed.
- **SECU-02**: `SSH_AUTH_SOCK` excluded from agent subprocess environment via `AgentAllowlist`; HOME repointed to the worktree so RELATIVE dotfile lookups (`~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.config/gcloud`, `~/.azure`, `~/.git-credentials`) miss. This is environment isolation only — it does NOT stop an agent reading the REAL home by absolute path. Absolute-path credential reads are denied by the OS sandbox's credential deny-list (`sensitiveHomeDirs`/`sensitiveHomeFiles`, P7-SEC-01) but ONLY on the full-fidelity backends (macOS Seatbelt, Linux bubblewrap); under the Landlock fallback the standalone credential deny is not enforceable (credential paths stay readable unless `--read-confine` is engaged, whose allow-list excludes them — see `spec/10:141`), and `--sandbox auto` can degrade to advisory behavior on hosts with no sandbox adapter. Use `--sandbox require` to refuse an unenforced run.
- **SECU-03**: `verifyEventSignature` requires valid signatures from known approved devices for destructive event types (`project.deleted`, `project.renamed`) unconditionally, and for **all** non-local event types once any approved device is enrolled (`HUB-03` fail-closed-once-enrolled). The pre-enrollment bootstrap window for non-destructive events remains (`SEC-04`).
- **SECU-04**: `redact.Writer` suppresses multi-line PEM private key blocks across line boundaries. Fixed `pemBegin` pattern indexing bug (was pointing to age-key pattern instead of PEM header). Added test coverage.
- **SECU-05**: `devices enroll --approve` now requires `--signing-public-key`.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-SEC-01 — Unauthenticated grant ingestion lets the hub substitute a device's WCK

**Problem.** Before `P6-SEC-01(a)`, `EncryptedHub.Pull` called `IngestGrant` for every `device.key.granted` event in the raw, unverified hub batch before any signature/trust check, so a hostile hub could forge a grant wrapping an attacker-chosen WCK to the victim's own recipient — breaking `P4-SEC-02` confidentiality or DoSing decryption. The carrier-verification part is now shipped; the overwrite and verified-chain epoch controls remain open.

**Actionable steps.**
1. **[SHIPPED — step (a)]** `EncryptedHub` carries a `Verify func(ctx, state.Event) error` seam; `Pull` calls it on each grant carrier *before* `IngestGrant` and skips (never ingests) on failure, so an unverified grant never reaches `StoreWCK`, `RecordKeyEpoch`, or the WCK cache — the keystore, cache, and `CurrentKeyEpoch` are untouched. `hubFromOptions` wires it to `(*state.Store).VerifyRemoteEvent`, which delegates to `verifyEventSignature`, so the trust regime (fail-closed once enrolled, `P4-SEC-04` bootstrap window otherwise) is identical to the apply path. The refused carrier still flows to `ApplyEvents` → `event_verification_failure` quarantine.
2. **[SHIPPED — step (b), PR-3b]** `IngestGrant` cannot change an already-held key: `(epoch, kid)` keying gives every distinct key its own metadata row and custody slot (no overwrite path remains), a carried-kid/unwrapped-bytes mismatch is rejected, and a same-slot custody rewrite byte-compares and refuses a mismatch.
3. **[SHIPPED — step (c), PR-3b]** Key rows only enter `workspace_keys` via a verified-grant `IngestGrant` (origin `grant`), founder bootstrap/`Rotate` (origin `self`), or the one-time migration backfill (origin `legacy`) — recorded in the `origin` column — and `PushKey` selects the highest epoch preferring `grant` > `self` > `legacy`, so a forged epoch can no longer become the push key once enrolled.
4. **[SHIPPED]** `TestSyncRejectsForgedGrantBeforeWCKIngest` (`internal/cli/sync_grant_injection_test.go`): a well-formed forged grant (epoch 2^40, attacker WCK age-wrapped to the victim's own recipient, unknown device) is refused — `CurrentKeyEpoch` unchanged, no WCK file written for the forged epoch, exactly one `event_verification_failure` conflict.

### P6-SYNC-04 — carrier fields bound by neither AAD nor signature (SHIPPED 2026-07-03)

**Was.** The `enc.v1` `envelopeAAD` bound only `event.ID || epoch` and `eventSignaturePayload` omitted `DeviceID`/`Seq`, so an untrusted hub could rewrite `Seq` (forcing an `ErrEventHashChain` cursor wedge) or re-attribute `DeviceID` without breaking AEAD or the signature.

**Shipped.** `enc.v2` (`internal/sync/eventcrypt.go`): hard cut, v1 is dead (loud skip + re-found guidance). AAD = `u32len(ID)||ID || u32len(DeviceID)||DeviceID || u32len(kid)||kid || u64(Seq) || u64(HLC) || u64(epoch)`; the kid is the sealing key's `KIDForWCK`, derived from the candidate on decrypt (the envelope field stays a routing hint). Signature domain `devstrap:event:v2` adds `device_id`/`seq`; verify falls back to v1 for re-pushed history. Held-key AEAD failure forwards the carrier to an `undecryptable` `event_verification_failure` conflict (never inserted, never approve-replayed, cursor advances, `hub gc` refuses while open); every pull replays open undecryptable conflicts against the keys held then, so a carrier mis-quarantined by kid tampering recovers automatically once its grant arrives (`ReplayUndecryptableConflicts`). Pinned by `TestDecryptMutatedCarrierFails`, `TestEncryptedHubPoisonEventDoesNotWedge`, `TestApplyEventsQuarantinesUndecryptableCarrier`, `TestEventSignatureV2BindsDeviceIDAndSeq`.

### Direction: one coordinated wire-format break (AD-3, future)

The three critical crypto findings above (`P6-SEC-01`, `P6-SYNC-04`) plus the epoch-selection
gap all touch the envelope wire format. Because only the file-hub spike and fresh R2 buckets
exist today, the format can still change cheaply. DIRECTION — land a **single coordinated
break** rather than a string of compatible patches:

- **[SHIPPED 2026-07-03]** `enc.v2` full-carrier AEAD AAD binding `ID || DeviceID || kid || Seq || HLC || epoch` (`P6-SYNC-04`), with the `devstrap:event:v2` signature domain and undecryptable-carrier quarantine;
- **[SHIPPED — PR-3b]** a WCK keyring keyed by `(epoch, kid)` where `kid = hex(sha256(wck))` (full
  digest), with per-row `origin` (`self`/`grant`/`legacy`) as the verified write-path record and
  grant-preferring push-key selection (`P6-SEC-01`/`P6-SEC-02`); the kid now rides inside the
  `enc.v2` AAD (via the sealing key on seal/open);
- **[SHIPPED]** founder-vs-`--join` `init` so a joining device never self-bootstraps epoch 1 (`P6-SEC-02`): `init` no longer mints a WCK, founding is deferred to the first `sync` against an empty hub, and a keyless device seeing a non-empty hub defers its push until granted — closing the pre-approval data loss (e2e `sync_join_flow.txtar`). The `(epoch,kid)` overwrite/collision hardening is now **shipped** (PR-3b) ahead of the coordinated break;
- **[SHIPPED (format + hub plane + import verification)]** a signed hub-side retention marker so GC floors are authenticated (`P6-HUB-04`): the retention manifest (`meta/retention.json`) carries the per-device floors plus the current snapshot's sha256, Ed25519-signed under `devstrap:retention:v1` and written with compare-and-swap. Trust split by design: backends read the floors UNVERIFIED on the pull path (they hold no device registry, and an unverified floor can only FORCE the snapshot path — a garbled manifest is a hard error, never "no floor"), while the snapshot IMPORT verifies fail-closed with no pre-enrollment window (`produced_by` must be a locally pinned approved device; then sha256-from-manifest; then the AEAD under a held WCK). A malicious hub can only DoS — withhold or garble the manifest/object into a forced, loud refusal — never inject state. The `sync`/`hub gc`-side import + `ErrSnapshotRequired` recovery, floor-monotonicity caching (`retention_floor:<hubID>` in `local_meta`), and the `hub compact` producer (which signs and CAS-writes the manifest, monotonic-refusing a floor rollback) are now shipped (`P4-SYNC-02`/`P4-HUB-11`, `recoverFromSnapshot`/`ImportSnapshot`/`hubCompact`); the per-device sync acks complete this in the same wave.

`enc.v1` and bare-integer epochs are now **dead**, not supported legacy — there was no
production data to migrate; a v1 envelope on a hub is skipped loudly and the remedy is
re-founding on a fresh hub.

### Direction: reduce the crypto surface, seek external review (AD-4, future)

Three of the four critical security findings live in the bespoke WCK epoch/rotation protocol,
yet the namespace map it protects leaks paths/remotes, not secret *values* (those already ride
the per-recipient age blob plane). DIRECTION — before this "zero-knowledge" property is
advertised to other users:

- evaluate **descoping event-log envelope encryption to the simpler per-recipient age-wrap**
  already proven in the blob plane, unless forward secrecy on the namespace map is a firm
  requirement;
- if the epoch design stays, obtain at least **one external cryptographic review** of the
  WCK epoch/rotation protocol before making the zero-knowledge claim load-bearing for
  third-party users.

### Threat: tampered, incomplete, or interrupted local recovery (`P7-DATA-03/04/05`, mitigated)

A full backup takes DB-derived inventory from the immutable read-only SQLite snapshot and fails if referenced ciphertext cannot be captured. Restore verifies the versioned manifest, every entry's SHA-256/size, the absence of unlisted files, SQLite integrity, and all DB-referenced blobs/device keys/held WCK files before touching the live home. Journaled all-target promotion keeps every old target under one shared aside suffix until every new target is durably marked done; recovery rolls forward only from that committed state and otherwise restores the exact prior generation in reverse. Pending-journal state opens fail closed until `db restore --recover` (or a plain restore's initial auto-recovery) completes. The state-home maintenance lock serializes promotion with full backup, `db down`, and run-loop ticks.

Residuals: the manifest supplies integrity, not provenance—a local operator who can replace the whole tar can forge the manifest too, and authenticated backup signing is out of scope. One-shot `devstrap sync` deliberately does not take the maintenance lock, so operators must not run it concurrently with full backup/restore.
