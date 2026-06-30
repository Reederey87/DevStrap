---
last_reviewed: 2026-06-30
tracks_code: [internal/childenv/**, internal/cli/**, internal/devicekeys/**, internal/envbundle/**, internal/git/**, internal/hub/**, internal/redact/**, internal/sync/**, internal/logging/**]
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
- Snowflake/cloud configs;
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

Hub-backend trust model (`HUB-*`): the hub is a **two-plane zero-knowledge store** — (1) a signed, HLC-ordered append-only event log (the namespace map) and (2) a content-addressed encrypted blob store (`age_blob:<sha256>`) for env values and non-git/draft content. Repo content never transits the hub; it rides git's own transport via blobless clone/fetch from each project's existing remote. The backend is pluggable behind one Hub interface: the chosen production backend is **Cloudflare R2** (S3 API, client-side age encryption, namespaced by `workspace_id`) — any S3-compatible store reuses the same interface — and a file-backed local backend exists **only for tests**. Either backend sees only ciphertext plus the signed map — it cannot read code, secrets, or drafts.

R2/S3 credential custody (shipped, `P5-HUB-01`): the live `aws-sdk-go-v2` S3 adapter is wired behind `hub: r2://<bucket>` (or `s3://`). The bucket and endpoint are non-secret config (the bucket is the URI host; the endpoint comes from the URI `?endpoint=` override or `DEVSTRAP_HUB_S3_ENDPOINT`). The secret access key is supplied via env/config only (`DEVSTRAP_HUB_S3_ACCESS_KEY_ID`/`DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY`, with `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` fallbacks), never in the URI, `state.db`, or logs. The adapter is built with `s3.New(s3.Options{...})` (not `config.LoadDefaultConfig`) plus an inline `aws.CredentialsProviderFunc`, so the SSO/IMDS/STS chain and the `credentials` module are never pulled in. The SDK retryer is disabled (`aws.NopRetryer{}`) so `R2Hub.Retry` is the single retry layer (no double-retry or runaway billing loop); throttling/transient S3 errors are retried with capped backoff + jitter, while auth/precondition/not-found errors fail fast. Hosted temporary prefix-scoped credentials remain a documented future (`CredentialMode == "hosted"`).

Residual risk: a malicious approved device can decrypt bundles it is authorized to receive until revoked, and **age has no native revocation**. Bound this by per-profile recipient scoping, re-encrypting every affected bundle to the reduced recipient set after revocation, and requiring provider/service-side value rotation for secrets that may already have been exposed.

Reality (`SECU-03`/`SECU-05`/`HUB-03`): event signature verification **fails closed once any approved device is enrolled** — `verifyEventSignature` requires a valid signature from a known, approved, non-local device for **all** event types once `hasEnrolledDevices` is true; unknown devices, devices with no signing key, and non-approved devices are rejected (not applied). The local device is exempt from the signing-key requirement (pre-enrollment grace). Destructive event types (`project.deleted`, `project.renamed`) require verification unconditionally. The remaining gap is the **pre-enrollment bootstrap window** (`SEC-04`): before any peer is approved, non-destructive events from unknown devices are accepted so a fresh device can sync its first tree; closing this requires an out-of-band peer-signing-key pinning ceremony plus an authenticated full-state snapshot, which changes the core sync-without-enroll flow and is deferred. The hub must be treated as **zero-knowledge / semi-trusted** (ciphertext + routing metadata only); mTLS device certs should enforce revocation at the transport layer.

Multi-tenant isolation (future SaaS direction, `SCALE-*`): when the hub serves more than one owner, **confidentiality** is by construction — every blob and event is client-side age-encrypted before upload and namespaced by `workspace_id`, so a zero-knowledge hub cannot decrypt across tenants even if its access controls fail. Integrity and availability are not automatic: a leaked bucket-wide key can still delete, overwrite, withhold, or reorder ciphertext. Hosted mode therefore requires prefix-scoped temporary credentials, signed hash chains, fail-closed verification, snapshots/backups, retention discipline, rate limits, and cell/tenant scoping.

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

Current implementation creates a local Ed25519 signing identity during `devstrap init`, stores only the public key in `devices.signing_public_key`, stores private signing material through the platform keychain adapter with `0600` file fallback, signs local events, and verifies signed inserts when the source device's signing public key is known. Manual remote-device enrollment/approval is available for local env capture recipients. Key fingerprint confirmation, automatic enrollment, and signed hub ingestion remain future work.

Key-custody status (`SECR-04`/`SECU-01`): the file fallback is now gated on true keychain unavailability and a present-but-failing keychain fails closed; the fallback warns when engaged. Remaining coverage risk is Linux Secret Service/headless integration (`XP-03`). Event-verification fail-closed-once-enrolled is implemented (`HUB-03`); the pre-enrollment bootstrap window remains open (`SEC-04`, deferred).

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
