# Security Policy

## Reporting a Vulnerability

Do not open a public issue for suspected vulnerabilities.

Report privately to the repository owner with:

- affected version or commit;
- reproduction steps;
- impact assessment;
- whether secrets, device keys, or private source paths may have been exposed.

Expected acknowledgement target: 3 business days.

## Scope

Security-sensitive areas include:

- secret capture, hydration, redaction, and runtime injection;
- device identity keys and bundle recipients;
- local SQLite state and backups;
- daemon socket access;
- Git worktree creation and destructive sync behavior;
- agent command policy and logs.

## Defaults

DevStrap must not transmit telemetry by default. Secrets must not be logged, uploaded as plaintext, stored directly in `state.db`, or written to files without an explicit user command.
