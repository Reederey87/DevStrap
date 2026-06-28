---
last_reviewed: 2026-06-28
tracks_code: [internal/childenv/**, internal/cli/**, internal/devicekeys/**, internal/envbundle/**, internal/git/**, internal/logging/**]
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

Reality (`AGEN-01`, `AGEN-02`/`SECU-02`): the wrapper command/file policy is argv-substring matching, **trivially bypassed by any interpreter** (`bash -c`, `python -c`, base64-decode, variable indirection), so the default `guarded` agent has full filesystem read + network exfil; and the agent subprocess currently **inherits `HOME` and `SSH_AUTH_SOCK`**, forwarding a live Git/SSH credential capability. The "no secrets by default" and "OS sandbox" bullets above are not yet true. Strip `HOME`/`SSH_AUTH_SOCK`, treat the wrapper as accident-prevention rather than a security boundary, and move to an allowlist + OS sandbox (Seatbelt / bubblewrap-landlock-seccomp).

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

Residual risk: a malicious approved device can decrypt bundles it is authorized to receive until revoked. Bound this by per-profile recipient scoping, re-encrypting every affected bundle after revocation, and requiring provider/service-side value rotation for secrets that may already have been exposed.

Reality (`SECU-03`, `SECU-05`): event signature verification currently **fails open** — events from a device whose signing key is unknown (or that has no signing key) are accepted unverified, so the "event signatures from day one" and "out-of-band fingerprint confirmation" bullets above are not yet enforced; a malicious hub could inject a rogue device. The hub must be treated as **zero-knowledge / semi-trusted** (ciphertext + routing metadata only); clients must **fail closed** on events from unverified devices once enrollment exists, and mTLS device certs should enforce revocation at the transport layer.

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

Key-custody caveat (`SECR-04`/`SECU-01`): the keychain→file fallback currently triggers on **any** keychain error, not only genuine unavailability, so a transient keychain failure silently downgrades to a `0600` plaintext age/Ed25519 private key on disk. Narrow the fallback to true "unavailable" conditions and surface a warning when it engages. Verification fail-open (`SECU-03`) must become fail-closed once enrollment lands.

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
- **SECU-02**: `SSH_AUTH_SOCK` excluded from agent subprocess environment via `AgentAllowlist`.
- **SECU-03**: `verifyEventSignature` requires valid signatures from known approved devices for destructive event types (`project.deleted`, `project.renamed`).
- **SECU-04**: `redact.Writer` suppresses multi-line PEM private key blocks across line boundaries.
- **SECU-05**: `devices enroll --approve` now requires `--signing-public-key`.
