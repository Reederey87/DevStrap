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

## Threats and mitigations

### Threat: plaintext secret sync

Mitigation:

- never sync `.env` by default;
- encrypted env capture only;
- runtime injection preferred;
- generated `.env.local` must be explicit;
- permissions `0600`;
- secret redaction in logs.

### Threat: malicious or compromised agent reads secrets

Mitigation:

- no secrets by default for agents;
- env allowlist;
- file denylist;
- isolated worktree;
- separate process environment;
- log redaction;
- future container sandbox.

### Threat: destructive sync deletes code

Mitigation:

- tombstones instead of immediate delete;
- quarantine before purge;
- never delete dirty worktree;
- dry-run;
- audit log.

### Threat: stale branch causes bad agent output

Mitigation:

- fetch upstream before worktree;
- record base SHA;
- stale-base check before PR;
- never use local main as agent base.

### Threat: hub compromise

Mitigation:

- hub stores encrypted blobs;
- per-device encryption;
- device revocation;
- event signatures later;
- no raw secrets;
- no raw Git mirror by default.

### Threat: device lost/stolen

Mitigation:

- revoke device;
- rotate env bundles;
- OS keychain for private key;
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
- allow explicit override.

Danger examples:

```text
LD_PRELOAD
DYLD_INSERT_LIBRARIES
BASH_ENV
NODE_OPTIONS
PYTHONPATH
GIT_SSH_COMMAND
```

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

## Audit log

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

## Open security questions

- Should personal encrypted env use age or libsodium sealed boxes?
- Should hub event payloads be signed from day one?
- Should agent command execution be mediated through a PTY proxy?
- How much sandboxing is needed before public release?
- Should DevStrap refuse to manage repos with secret-looking tracked files?

