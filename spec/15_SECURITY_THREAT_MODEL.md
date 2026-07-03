---
last_reviewed: 2026-07-03
tracks_code: [internal/childenv/**, internal/cli/**, internal/devicekeys/**, internal/envbundle/**, internal/git/**, internal/hub/**, internal/redact/**, internal/state/**, internal/sync/**, internal/logging/**, internal/workspacekeys/**]
---
# Security Threat Model

## Security posture

DevStrap is dangerous if designed casually because it touches code, secrets, Git credentials, and AI agents.

The product should be safe by default and explicit when convenience weakens security.

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

Reality (`AGEN-01`, `AGEN-02`/`SECU-02`): the credential-env leak is fixed — `SSH_AUTH_SOCK` is excluded and `HOME` is repointed to the worktree — but the wrapper command/file policy is still argv-substring matching and **trivially bypassed by any interpreter** (`bash -c`, `python -c`, base64-decode, variable indirection). The default `guarded` agent still has ordinary process-user filesystem read capability and network egress unless an OS sandbox constrains it. Treat the wrapper as accident-prevention rather than a security boundary, and move to an allowlist + OS sandbox (Seatbelt / bubblewrap-landlock-seccomp).

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

Residual risk: a malicious approved device can decrypt bundles it is authorized to receive until revoked, and **age has no native revocation**. Bound this by per-profile recipient scoping, re-encrypting every affected bundle to the reduced recipient set after revocation, and requiring provider/service-side value rotation for secrets that may already have been exposed.

Reality (`SECU-03`/`SECU-05`/`HUB-03`): event signature verification **fails closed once any device has ever been enrolled** — `verifyEventSignature` requires a valid signature from a known, approved, non-local device for **all** event types once `hasEnrolledDevices` is true; unknown devices, devices with no signing key, and non-approved devices are rejected (not applied). Enrollment is **sticky** (`P6-SYNC-03`, shipped): `hasEnrolledDevices` counts `trust_state IN ('approved','revoked','lost')` — a revoked/lost row proves a deliberate local operator trust decision (no sync/remote path can inject one) — so revoking or losing the last approved device keeps the window closed — post-revoke traffic from the revoked (or any unknown) device quarantines instead of applying. The local device is exempt from the signing-key requirement (pre-enrollment grace). Destructive event types (`project.deleted`, `project.renamed`) require verification unconditionally. The remaining gap is the **pre-enrollment bootstrap window** (`SEC-04`), now narrowed: the **joiner half is closed** by the documented founder-pinning ceremony — a keyless joiner runs `devices enroll <founder-device-id> … --approve` BEFORE its first sync; the grant path is founder-gated so the joiner mints and grants nothing, but the approved founder row flips `hasEnrolledDevices`, so `verifyEventSignature` and `EncryptedHub.Verify` fail closed before the joiner's first pull (pinned in `devices_pin_founder_test.go` and the `sync_join_flow` e2e). In a fleet with more than one existing device the joiner pins **every** existing approved device the same way — device records are not synced, so events signed by a device the joiner has not yet pinned quarantine as `event_verification_failure` conflicts and are replayed automatically when that device is later enrolled and approved (the `devices approve` replay path; recoverable and visible in `conflicts list`, never silently lost). Before pinning (or on a founder before any peer is approved), non-destructive events from unknown devices are still accepted so a fresh device can sync its first tree; the residual is founder-side automation of the exchange, an in-band fingerprint-confirmation UX, and an authenticated full-state snapshot. The hub must be treated as **zero-knowledge / semi-trusted** (ciphertext + routing metadata only); mTLS device certs should enforce revocation at the transport layer.

Multi-tenant isolation (future SaaS direction, `SCALE-*`): when the hub serves more than one owner, **confidentiality** is by construction — every blob and event is client-side age-encrypted before upload and namespaced by `workspace_id`, so a zero-knowledge hub cannot decrypt across tenants even if its access controls fail. Integrity and availability are not automatic: a leaked bucket-wide key can still delete, overwrite, withhold, or reorder ciphertext. Hosted mode therefore requires prefix-scoped temporary credentials, signed hash chains, fail-closed verification, snapshots/backups, retention discipline, rate limits, and cell/tenant scoping.

### Threat: hub key-substitution defeats envelope confidentiality (`P6-SEC-01`, mitigated once enrolled)

Attacker = the untrusted/zero-knowledge hub (or a MITM/revoked device). Because age encryption to a public X25519 recipient needs no secret and every device's recipient string rides the hub as plaintext, a hostile hub can forge a `device.key.granted` grant that wraps an **attacker-chosen** Workspace Content Key to the victim's own recipient. Before `P6-SEC-01(a)`, `EncryptedHub.Pull` ingested grants from the raw hub batch before any signature/trust check, so a forged high epoch could become the active `Push` epoch and a low-epoch variant could overwrite the legitimate WCK.

Mitigation status: **step (a) shipped** — `EncryptedHub.Pull` now runs a `Verify` seam (`internal/sync/encryptedhub.go`, wired by `hubFromOptions` to `(*state.Store).VerifyRemoteEvent`) on every grant carrier *before* `IngestGrant`, so once any device is approved a grant from an unknown/unapproved/bad-signature device is refused and never reaches the keyring; the refused carrier still flows to `ApplyEvents` and lands in the `event_verification_failure` quarantine (one visible conflict, not silent). This shares the apply-path trust regime exactly, so no new trust policy is introduced and the pre-enrollment bootstrap window (`P4-SEC-04`) is the only residual open-ingest path. **Steps (b)/(c) shipped (PR-3b):** keys are addressed by `(epoch, kid)` with `kid = hex(sha256(wck))` (full digest), so a grant can never displace an existing key — distinct keys land in distinct slots (a same-slot custody rewrite additionally byte-compares and refuses a mismatch), `IngestGrant` rejects a carried kid that disagrees with the unwrapped bytes, and every key row records its `origin` (`self` bootstrap/rotate, verified `grant`, or migration `legacy` — the only write paths). Push-key selection prefers verified `grant`-origin keys, so a forged or stale self-mint can no longer become the push key once the fleet key arrives. **Kid binding (shipped with `enc.v2`, `P6-SYNC-04`):** the envelope's kid FIELD remains a candidate-ordering hint — every held key at the epoch is tried before deciding, so RELABELING a decryptable event's kid neither wedges nor loses it (post-#33 review, fable-5) — while the SEALING key's kid is bound into the AAD (derived from the candidate on decrypt), so a ciphertext only ever authenticates under the exact key that sealed it. STRIPPING the kid from an event a colliding-key holder cannot decrypt no longer drops it silently OR permanently: the AEAD failure forwards the carrier to a visible `undecryptable` quarantine conflict, and every subsequent pull replays open undecryptable conflicts against the keys held then (`ReplayUndecryptableConflicts`, wired into `pullAndApplyEvents`) — once the real grant lands, the carrier decrypts, applies through the normal verified path, and the conflict auto-resolves. A hub tampering with the kid hint can therefore only DELAY a not-yet-granted event, never destroy it (post-#44 review fix, gpt-5.5 Major). The pre-enrollment bootstrap window (`P4-SEC-04`) remains the residual open-ingest path — and note its full extent (post-#33 review, gpt-5.5): a grant ingested during that window records `origin='grant'` and is therefore push-preferred, so until the first device is enrolled a hostile hub can still hand a fresh joiner an attacker-known key. Closing it is the P4-SEC-04 out-of-band fingerprint work, not a keyring change.

### Threat: hub tampers with unauthenticated carrier fields (`P6-SYNC-04`, mitigated)

Attacker = the untrusted hub. Under the retired `enc.v1`, the AAD bound only `event.ID || epoch` and the signature omitted `DeviceID`/`Seq`, so a hostile hub could rewrite `Seq` (forcing an `ErrEventHashChain` soft-wedge that held the cursor forever) or re-attribute `DeviceID` (corrupting the conflict tiebreak) without breaking AEAD or the signature. **Mitigated (`enc.v2`, shipped 2026-07-03):** the AAD now binds the full carrier tuple (`ID`, `DeviceID`, sealing-key kid, `Seq`, `HLC`, epoch — length-prefixed/big-endian), so any carrier mutation is an AEAD authentication failure at decrypt time; the failure forwards the carrier to an `undecryptable` quarantine conflict (permanent-class — it never holds the cursor — but auto-recovered by the undecryptable replay once the key arrives; visible, blocks `hub gc`) instead of a silent skip. The signature domain moved to `devstrap:event:v2` with `device_id` + `seq` in the payload; verification accepts v2 then falls back to v1. **Residual:** v1-signed historical events (re-pushed verbatim when a hub is re-founded) lack the DeviceID/Seq *signature* binding. Every enc.v2 event gets the AAD binding regardless (possession-based, so it also covers the pre-enrollment window) — the one plaintext-plane exception is `device.key.granted` carriers, which are never enc.v2-wrapped, so a *legacy v1-signed* grant's `Seq` is bound by neither AAD nor signature (its DeviceID is still caught by the signing-key lookup; all grants this build creates are v2-signed). Reconciles with open `P4-SYNC-05`, which would extend Seq/HLC binding to a folded hash chain.

### Threat: a wrong or hostile `workspace_id` (`P4-SEC-07` pairing, by design)

Attacker = a mistaken operator, or the untrusted hub steering a device onto the wrong prefix. Devices converge only when they share one `workspace_id`, which keys every hub object under `workspaces/<workspace_id>/` (`19_CLOUD_PROVISIONING_GUIDE.md` §E). The id is a **prefix selector, not an authenticated field**: it is **excluded from event signatures by design** (remote events are re-stamped `WorkspaceID=""` on apply, so signing it would break verification across devices), and it is exchanged out-of-band alongside the enrollment key exchange (Syncthing-style — a non-secret identifier whose authorization comes from key verification, not from the id itself). This is safe because it selects a namespace, it does not grant one: pointing a device at a **wrong** id yields an empty prefix (no content), and pointing it at a **hostile** id yields only ciphertext the device cannot decrypt without a granted WCK, and events it cannot verify without the founder's pinned keys — so a bad id surfaces as an empty pull or a quarantined `event_verification_failure`/`undecryptable` conflict, never as accepted content or another workspace's plaintext. Content protection is therefore carried entirely by fail-closed signature verification, the `enc.v2` AAD binding, and the founder-pinning ceremony (`E.2`), not by the id. Residual: the pre-enrollment bootstrap window (`P4-SEC-04`) — a keyless joiner that has not yet pinned the founder still accepts non-destructive events, so founder-side automation and an in-band fingerprint UX remain future work.

### Threat: one bad signed event wedges every device's sync (`P6-SYNC-01`, mitigated)

Attacker = a revoked/lost device that keeps pushing signed events (or a hub replaying one). Previously any signature/trust failure in `ApplyEvents` aborted the whole batch before the cursor advanced, so a single poisoned event re-pulled and re-failed forever — a fleet-wide availability break requiring no key compromise. **Mitigated:** permanent verification failures (signature/trust/content-hash, wrapped in `state.ErrEventVerification`) and divergent duplicate IDs are now quarantined per-event as `event_verification_failure` conflicts (full event JSON retained for replay) while the rest of the batch applies and the cursor advances safely; `devices approve` replays a newly-approved device's quarantined events. Residual: `devices revoke` is local-only — a synced `device.revoked` trust event remains future work.

**Related (owned elsewhere):** `P6-HUB-01` — hub-side blob GC data-loss (availability, owned in `spec/03`, shipped); `P6-SYNC-03` — revoking the last approved device previously reopened the fail-open bootstrap window (owned in `spec/07`, **shipped**: sticky enrollment, see above).

### Threat: hub epoch-injection denial of service (`P6-SEC-03`, mitigated — grace-bounded)

Attacker = the untrusted hub (or an approver whose grant never propagated). Pre-fix, `EncryptedHub.Pull` truncated forever at the first event sealed under an `(epoch, kid)` this device was never granted — one well-formed `enc.v2` object naming a bogus high epoch, or a forged random kid at a held epoch, stalled a device's sync permanently with no key compromise; the same wedge hit legitimately-approved devices whose approver lagged a rotation. **Mitigated:** the missing-key defer is now bounded by `sync.key_grant_grace` (default 72h, `0` = immediate). The first sighting is persisted (`key_grant_waits`, stable first-seen; the grace clock is the earliest first-seen across every kid at the epoch, so per-pull kid relabeling cannot restart it); past the window the still-encrypted carrier is quarantined as a visible, `hub gc`-blocking, replay-recoverable `undecryptable` conflict and the cursor advances. An injected-garbage epoch therefore costs at most one bounded delay plus one open conflict (resolvable via `conflicts resolve`); a real-but-late grant recovers its events automatically via `ReplayUndecryptableConflicts`. The `devices approve` contiguity guard (`--allow-epoch-gap` to override) stops an incomplete approver from propagating the gap, and `doctor` surfaces every open wait ("awaiting key grants"). **Residuals:** a hostile hub that keeps serving ciphertext at a validly-shaped never-granted (epoch, kid) can pin an open wait row — a standing `doctor` warning and an approval-guard trip (`--allow-epoch-gap` overrides) — but no longer a sync wedge (malformed kids and phantom kid rows are excluded: missing-epoch waits are epoch-level and non-canonical kids quarantine without a wait). A rotator grants only to approved devices it knows locally, so a device unknown to it always rides the grace→quarantine→replay path after a rotation until re-approved; old-epoch containment (retiring long-compromised epochs) is documented-not-built. A hostile hub can still WITHHOLD events (zero-knowledge transport was never an integrity guarantee against omission — see `P6-HUB-04`).

### Threat: MITM/tamper on the untrusted pairing channel (`P4-SEC-04` part 1, mitigated)

Attacker = anything that can alter the seven values a device pastes during enrollment (age recipient, signing key, workspace id, …) as they cross the out-of-band channel — a swapped recipient, a compromised copy-paste path, or a hostile hub steering the exchange. Substituting the enrollee's keys would let the attacker be pinned instead of the intended device, defeating the founder-pinning that fail-closed verification depends on.

**Mitigation (shipped):** approval is gated on out-of-band confirmation of a **device fingerprint** that binds *both* the Ed25519 signing key and the age recipient — `sha256("devstrap/device-fp/v1" || 0x00 || canonicalSigning || 0x00 || canonicalRecipient)`, rendered as unpadded uppercase base32 in 13 dash-separated groups of 4. Both inputs are canonicalized (parse-then-re-encode) so cosmetic encoding differences do not change the value. `devices approve` and `enroll --approve` compute the fingerprint from the row/flags being approved (never the local keystore) and refuse the trust-state change unless it is confirmed — via `--fingerprint <value>` (constant-time compare), an interactive `yes` on a TTY, or, on a non-TTY, a hard refusal with a copy-paste remedy. The operator reads the fingerprint off the far device (`devices recipient --fingerprint`) or its `devices list` row and compares character-for-character. Because the channel is untrusted, this is a **full 256-bit fingerprint, never a short authentication string (SAS)**: a truncated code would let an attacker grind a colliding key pair offline and pass the comparison, so no truncation is offered. `SECU-05`: a keyless placeholder row cannot be approved at all (nothing to bind). Residual: the exchange is still hand-pasted seven-value; a one-paste pairing code that bundles and integrity-checks the whole set is `P4-SEC-04` part 2. This does not close the pre-enrollment bootstrap window on its own — it makes the pinning step that closes it trustworthy.

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

Current implementation centralizes this in `internal/childenv` and wires it into Git subprocesses, editor launches, and generic agent commands. Generic agent runs receive no project secrets by default and apply wrapper-level command and file path policies that deny obvious destructive commands, secret-reading commands, explicit sensitive paths, and explicit outside-worktree paths unless `--policy yolo-local` is selected. OS-enforced sandboxing and env-profile-scoped secret injection remain future work.

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

Current implementation creates a local Ed25519 signing identity during `devstrap init`, stores only the public key in `devices.signing_public_key`, stores private signing material through the platform keychain adapter with `0600` file fallback, signs local events, and verifies signed inserts when the source device's signing public key is known. Manual remote-device enrollment/approval is available for local env capture recipients. Full-strength device fingerprints and a compare-and-confirm approval gate are shipped (`P4-SEC-04` part 1); a one-paste pairing code, automatic enrollment, and signed hub ingestion remain future work.

Key-custody status (`SECR-04`/`SECU-01`): the file fallback is now gated on true keychain unavailability and a present-but-failing keychain fails closed; the fallback warns when engaged. Remaining coverage risk is Linux Secret Service/headless integration (`XP-03`). Event-verification fail-closed-once-enrolled is implemented (`HUB-03`); the pre-enrollment bootstrap window (`SEC-04`) is narrowed — the joiner half is closed by the founder-pinning ceremony and the fingerprint compare-and-confirm gate is shipped (`P4-SEC-04` part 1, see the pairing-channel threat above); the one-paste pairing code + founder-side automation (part 2) remain.

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
- OS sandboxing is required before public release.
- Remaining question: should DevStrap refuse to manage repos with secret-looking tracked files, or warn and require explicit adoption?

## Audit implementation notes (2026-06-28)

- **SECU-01**: Key custody fallback now gated on `IsKeychainUnavailable(err)`; present-but-failing keychain fails closed.
- **SECU-02**: `SSH_AUTH_SOCK` excluded from agent subprocess environment via `AgentAllowlist`; HOME repointed to worktree path so `~/.ssh`, `~/.aws`, `~/.config/gh` are not reachable.
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
- a signed hub-side retention marker so GC floors are authenticated (`P6-HUB-04`).

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
