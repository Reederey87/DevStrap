---
last_reviewed: 2026-06-26
tracks_code: [cmd/**, internal/**, .github/**, go.mod, go.sum]
---
# Test Plan

## Test philosophy

This project can destroy trust if it loses code, leaks secrets, or creates stale agent branches. Tests must focus on safety invariants.

## Current coverage gate

Phase 0 currently implements `cmd/devstrap`, `internal/cli`, `internal/config`, `internal/git`, `internal/logging`, `internal/pathkey`, `internal/platform`, `internal/scan`, `internal/state`, and `internal/sync`. These packages must have executable tests before handoff.

Required now:

```text
golangci-lint run
go run ./cmd/spec-drift --base origin/main --head HEAD
go test -race ./...
```

The Phase 0 suite must cover:

- CI lint/security gate: `.golangci.yml` enables `errcheck`, `gosec`, `govet`, `ineffassign`, `staticcheck`, and `unconvert`; the workflow runs it as a separate Ubuntu job using the official pinned `golangci-lint-action`;
- CI spec-drift gate: every `spec/*.md` file has `last_reviewed` and `tracks_code` frontmatter; `cmd/spec-drift` fails when changed code/config paths do not touch mapped specs or when code/spec/doc changes omit `spec/18_WORK_LOG.md`;
- SQLite open path: foreign keys enabled and asserted, startup `PRAGMA foreign_key_check`, non-zero busy timeout, single-writer pool, `state.db` mode `0600`;
- migrations: idempotent `Migrate`, schema version, required tables, generated `ws_` workspace id persistence, singleton workspace enforcement, `PRAGMA quick_check`, `PRAGMA foreign_key_check`, fixed-width UTC nanosecond timestamp formatting, deterministic same-timestamp worktree listing, and an EQP assertion that `ListProjects` uses `idx_namespace_active`;
- uninitialized state detection uses explicit schema-table checks and returns the friendly `run devstrap init` hint for summary, device, and project reads;
- config precedence: flags > env > config file > defaults, including relocated `--home` config discovery;
- CLI exit codes and stderr for config/status errors;
- `init` absolute root normalization, dry-run output, home/log/db creation, secure default `config.yaml`;
- `db migrate`, `db status`, and `db backup`.
- generated local device persistence, age public recipient persistence, Ed25519 signing public-key persistence, OS keychain-backed private identity storage with `0600` file fallback, `doctor` device-key checks, local event signatures, tamper rejection, and absence of private identities from `state.db`/`config.yaml`;
- device trust-state CLI list/rename behavior and refusal to revoke the current local device;
- logging redaction for secret-like keys and `SecretString` values;
- path normalization rejection for absolute paths, escapes, and empty segments, plus NFC normalization for Unicode-equivalent paths;
- Git remote normalization for SSH, HTTPS, `ssh://`, absolute, and `file://` remotes;
- Git remote safety rejection for leading-option remotes, unsupported protocols such as `ext::`, malformed scp-like remotes, SSH/scp explicit-port normalization, typed Git error classification for network/auth/branch/remote failures, transient-network-only clone/fetch retry, and URL credential redaction in git errors;
- `internal/scan` direct coverage for generated-folder pruning, secret-looking filename warnings, symlink escape warnings, duplicate remote detection, and no descent into pruned repos;
- `init -> scan --dry-run --json -> scan --adopt -> status --json` with a Git repo, generated folder, and secret-looking filename fixture;
- `sync --hub-file <path> --dry-run` exposes the file-backed hub plan without writing;
- `add -> hydrate` against a local bare remote, refusal to write skeletons into non-empty directories, missing-remote clone failure preserving the original skeleton without temp-dir leaks, and promotion-time dirty-target refusal without removing local files;
- repo operation locks reject active concurrent operations and reclaim stale same-host owners before hydrate/worktree mutation;
- fresh worktree creation from an advanced remote SHA while local clone state is stale, collision-resistant worktree branch naming with retry, `worktree status` reporting stale after the remote base advances again, `worktree finalize` refusing stale bases unless `--allow-stale-base`, and LFS-policy warning/pull branching for agent worktrees;
- HLC monotonic send/receive, max-skew rejection, logical-counter overflow behavior, persisted local event HLC/sequence stamping and previous-hash linking across reopen, transactional idempotent event apply, divergent duplicate event rejection, incoming `prev_event_hash` chain-break rejection with conflict recording, HLC-gated delete tombstone restore/ignore behavior, and order-independent same-path/different-remote conflict protection with stable conflict details.

Future-phase sections below are required before their corresponding features ship; they are not allowed to satisfy the Phase 0 gate until the commands exist.

## Critical invariants to test

1. Agents branch from fetched remote default ref, not local default branch.
2. Dirty repos are never overwritten by sync.
3. Plaintext secrets are not uploaded or logged.
4. Dependency folders are ignored.
5. Skeletons can be safely recreated.
6. Deletes quarantine before purge.
7. Path conflicts are detected.
8. Mac/Linux behavior is consistent.

## Unit tests

### Path normalization

Cases:

```text
work/API vs work/api
trailing slash
leading slash
../escape
Unicode normalization
spaces
symlink paths
```

### Git remote normalization

Cases:

```text
git@github.com:org/repo.git
https://github.com/org/repo.git
ssh://git@github.com/org/repo.git
```

Expected canonical key:

```text
github.com/org/repo
```

### Ignore compiler

Cases:

- secret files excluded;
- `.env.example` included;
- `node_modules` excluded;
- generated managed block preserves user rules.

### Env parser

Cases:

- quoted values;
- multiline values if supported;
- comments;
- empty values;
- export prefix;
- invalid names;
- dangerous env names;
- interpolation-looking values rejected unless literal mode is explicit.

### Child process env

Cases:

- empty-by-default builder returns no inherited variables without an allowlist;
- explicit allowlist passes only named or prefix-matched variables;
- dangerous variables such as `LD_PRELOAD`, `DYLD_INSERT_LIBRARIES`, `NODE_OPTIONS`, `PYTHONPATH`, and `GIT_SSH_COMMAND` are stripped even when allowlisted;
- dangerous explicit sets are rejected;
- git subprocess env includes only basic process context plus controlled Git prompt/config variables.
- `devstrap run` injects decrypted local env values only into the child process, provider profiles delegate to `op run --env-file` with a temporary refs file, and provider file hydration delegates to `op inject` through temporary `0600` files without overwriting existing targets unless `--force` is explicit.
- `devstrap agent run` starts with the same basic allowlist plus DevStrap run metadata only; project secrets are not inherited by default.

### Redaction

Cases:

- exact secret value in logs;
- token substring;
- multiline secret;
- command output containing secret;
- JSON output containing secret.

## Integration tests

### Phase 0 CLI scaffold

```bash
gofmt -w cmd internal
go test -race ./...
go run ./cmd/devstrap version
go run ./cmd/devstrap doctor
go run ./cmd/devstrap init /tmp/devstrap-code --home /tmp/devstrap-home
go run ./cmd/devstrap status --home /tmp/devstrap-home --json
go run ./cmd/devstrap db status --home /tmp/devstrap-home
```

Expected:

- commands exit successfully;
- `init` creates the managed root and SQLite state database;
- Goose applies all embedded migrations;
- `status --json` returns initialized workspace metadata.

### Init and scan

```bash
devstrap init /tmp/Code
devstrap scan /tmp/Code --adopt
devstrap status --json
```

### Hydrate Git repo

Use local bare Git remote in test fixture.

```bash
git init --bare /tmp/remotes/repo.git
devstrap add /tmp/remotes/repo.git --path work/repo
devstrap hydrate work/repo
```

### Dirty repo safety

```text
1. hydrate repo
2. create local uncommitted file
3. update remote
4. run devstrap sync --fetch
5. assert no pull/rebase occurred
6. assert dirty state reported
```

### Fresh worktree

```text
1. create remote default branch commit A
2. clone locally
3. advance remote default branch to commit B
4. do not update local default branch
5. devstrap worktree new --fresh-upstream
6. assert worktree base SHA == B
7. advance remote default branch to commit C
8. run devstrap worktree status <id>
9. assert status reports stale behind 1
10. run devstrap worktree finalize <id>
11. assert finalize exits conflict unless --allow-stale-base is passed
```

This is the most important test.

### Agent policy and PR creation

```text
1. register a local bare remote-backed project
2. assert guarded agent policy rejects explicit `.env` reads
3. assert guarded agent file policy rejects outside-worktree and sensitive-home path arguments
4. run a generic agent command that writes inside the fresh worktree
5. assert an `agent_runs` row, `0600` log, and diff summary are recorded
6. advance the remote base and assert `agent pr` refuses without `--allow-stale-base`
7. run non-dry `agent pr` with a fake `gh` executable and assert `gh pr create` receives base/head/title/body argv
```

### Env capture/hydrate

```text
1. create .env with TEST_SECRET=abc123
2. capture
3. assert state/local blob does not contain abc123 plaintext
4. assert blob mode is 0600 and state stores only `age_blob:<sha256>`
5. assert captured file is gitignored
6. hydrate to .env.local
7. assert file contains abc123 and mode 0600
8. assert logs contain *** not abc123
```

### Provider env bind/run/hydrate

```text
1. bind a `.env.refs` file containing only `op://` references
2. assert state stores provider refs, not plaintext values
3. run through a fake `op run --env-file` and assert command args/env refs are delegated
4. hydrate through a fake `op inject` and assert the output file is `0600`
5. assert existing provider-hydrated files are refused unless `--force`
```

### Manual device env approval

```text
1. create a remote device age identity
2. enroll it with `devstrap devices enroll --approve`
3. capture an env profile
4. assert the ciphertext decrypts with the approved remote identity
5. assert local device revocation remains refused
```

### Draft sync

```text
1. create draft folder
2. include ignored node_modules and .env
3. create snapshot
4. restore on second temp device
5. assert ignored files missing
```

### Add/adopt namespace event emission

```text
1. initialize a workspace
2. add or scan-adopt a project
3. run `devstrap sync --hub-file <path> --dry-run`
4. assert at least one local project event would be pushed
5. assert the local namespace row records the source event HLC/device/id
```

## Daemon tests

### Watcher create project

```text
1. start daemon
2. mkdir managed root/new-project
3. wait for reconcile
4. assert namespace entry candidate created
```

### Daemon restart

```text
1. stop daemon
2. create folder
3. start daemon
4. periodic scan finds folder
```

### Sleep/wake simulation

Approximate by stopping watcher and doing bulk changes.

Expected:

```text
periodic reconciliation catches drift
```

## Multi-device tests

Use two temporary roots and one test hub.

```text
Device A: add project
Hub: receives event
Device B: sync pulls event
Device B: skeleton appears
Device B: hydrate repo
Device A: status shows Device B ready after heartbeat
```

## Conflict tests

### Same path different remotes

```text
Device A: work/api → remote A
Device B: work/api → remote B
Sync both
Assert conflict open
No folder overwritten
```

### Delete vs dirty local

```text
Device A: delete project
Device B: dirty hydrated project
Sync B
Assert delete conflict
Assert files still exist
```

### Rename conflict

```text
Device A: rename work/api → work/acme/api
Device B: rename work/api → personal/api
Assert conflict
```

## Platform adapter tests

- `internal/platform.Detect` returns watcher, service manager, keychain, and editor adapters for the current OS;
- polling watcher emits advisory scan events and stops on context cancellation;
- unsupported service/keychain placeholders return a sentinel error until native adapters land;
- source guard fails if `runtime.GOOS` branching appears outside `internal/platform`.

## Mac-specific tests

- LaunchAgent install/uninstall;
- daemon starts after login/reload;
- FSEvents watcher notices create/rename/delete;
- case-insensitive path conflict detection;
- Keychain storage adapter with file fallback;
- Homebrew install path compatibility;
- shell hook behavior in zsh.

## Linux-specific tests

- systemd user service install/uninstall;
- inotify watcher detects changes;
- watcher limit warning;
- headless secret unlock path;
- case-sensitive path policy still rejects case-only duplicates;
- Ubuntu smoke test in CI/container/VM.

## Agent tests

- `agent run` creates a fresh worktree in `~/.devstrap/worktrees`;
- generic command runs in the worktree cwd with sanitized no-secret env;
- wrapper-level command policy denies obvious destructive or secret-reading commands unless `--policy yolo-local` is explicit;
- env allowlist applied;
- denied env missing;
- dangerous env still stripped after profile resolution;
- `0600` logs captured;
- Git status/diff summary generated, including untracked files;
- `agent list`/`agent show` expose recorded run metadata;
- `agent pr --dry-run` refuses stale recorded bases unless `--allow-stale-base`;
- cleanup blocks dirty worktree;
- manually deleted worktree requires `remove --force`, prunes Git metadata, and removes the active DB row;
- stale base detected before PR.

## Chaos tests

- kill daemon during hydrate;
- network drops during clone;
- hub unavailable during local changes;
- corrupt local event queue;
- interrupted env capture;
- partial draft upload;
- Git lock file exists;
- repo deleted manually outside DevStrap.

## Manual acceptance scenario

End-to-end personal scenario:

```text
1. Mac Mini A: init workspace.
2. Add 5 repos and 1 draft project.
3. Capture env for 2 repos.
4. Start hub.
5. GMK Ubuntu: install DevStrap and join workspace.
6. Confirm tree appears.
7. Open one repo on Ubuntu.
8. Confirm env/tooling readiness.
9. Start agent worktree from fresh main.
10. Push PR or show diff.
11. Delete a project on Mac A and verify Ubuntu dirty clone is not deleted.
```
