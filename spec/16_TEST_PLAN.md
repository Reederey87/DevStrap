# Test Plan

## Test philosophy

This project can destroy trust if it loses code, leaks secrets, or creates stale agent branches. Tests must focus on safety invariants.

## Critical invariants to test

1. Agents branch from fetched remote ref, not local main.
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
- dangerous env names.

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
go test ./...
go run ./cmd/devstrap version
go run ./cmd/devstrap doctor
go run ./cmd/devstrap init /tmp/devstrap-code --home /tmp/devstrap-home
go run ./cmd/devstrap status --home /tmp/devstrap-home --json
```

Expected:

- commands exit successfully;
- `init` creates the managed root and SQLite state database;
- Goose applies `internal/state/migrations/00001_initial.sql`;
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
1. create remote main commit A
2. clone locally
3. advance remote main to commit B
4. do not update local main
5. devstrap worktree new --fresh-main
6. assert worktree base SHA == B
```

This is the most important test.

### Env capture/hydrate

```text
1. create .env with TEST_SECRET=abc123
2. capture
3. assert state/hub blob does not contain abc123 plaintext
4. hydrate to .env.local
5. assert file contains abc123 and mode 0600
6. assert logs contain *** not abc123
```

### Draft sync

```text
1. create draft folder
2. include ignored node_modules and .env
3. create snapshot
4. restore on second temp device
5. assert ignored files missing
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

## Mac-specific tests

- LaunchAgent install/uninstall;
- daemon starts after login/reload;
- FSEvents watcher notices create/rename/delete;
- case-insensitive path conflict detection;
- Keychain storage adapter;
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

- worktree created in `~/.devstrap/worktrees`;
- env allowlist applied;
- denied env missing;
- logs captured;
- diff summary generated;
- cleanup blocks dirty worktree;
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
