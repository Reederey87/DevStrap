---
last_reviewed: 2026-06-30
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
- forge detection/routing for `agent pr` across GitHub/GitLab/Gitea/Forgejo/Bitbucket/Azure-style remotes, forge-specific token env allowlists, Azure remote-key folding, and graceful unknown-forge compare-URL fallback;
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

#### `.devstrapignore` single-compiler consumers (planned, `DRAFT-*`)

The 2026-06-28 cloud-sync design makes one `.devstrapignore` compiler load-bearing for confidentiality: scan pruning, the `.gitignore` managed block, the watcher exclusion set, the agent denylist, and the draft-bundle exclusion set must all derive from the *same* compiled output (`11_IGNORE_AND_LOCAL_GARBAGE.md`). These tests are required before the non-git content-sync feature ships.

Cases:

- one compile call emits all consumer views (gitignore managed block, draft-sync exclusion set, watcher exclusion set, agent denylist, scan prune set) from a single source; a property test asserts no consumer includes a path another consumer excludes;
- `node_modules`, build artifacts, and OS junk (`.DS_Store`, `.AppleDouble`, `Thumbs.db`) are excluded from every consumer view, including the draft-bundle set;
- secret-looking files and `.git` are always in the draft-bundle exclusion set, so they can never be age-encrypted into an `age_blob:<sha256>` blob;
- user rules in the managed block survive a recompile;
- the draft-bundle packer reads the compiled output, not a re-derived hardcoded list (regression guard for `PLAT-01`/`PLAT-04`/`AGEN-05`).

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
7. run non-dry `agent pr` with fake `gh`, `glab`, and `tea` executables and assert each receives the expected base/head/title/body argv for its forge
8. run `agent pr` against an unsupported forge and assert the branch is pushed, a compare/MR URL is printed, and no GitHub-only CLI is invoked
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

### Namespace-map sync (current file-backed spike)

```text
Device A: add project
Hub: receives event
Device B: sync pulls event
Device B: skeleton appears
Device B: hydrate repo
Device A: status shows Device B ready after heartbeat
```

### Eager-clone two-machine end-to-end (planned, `EAGER-*`/`DRAFT-*`/`HUB-*`)

Proves the "Dropbox experience for code" round trip: one `devstrap sync` on Device B reconstructs the whole `~/Code` tree — repos blobless-cloned from their existing remotes, drafts restored from encrypted blobs, env hydrated — with no skeletons left behind. Required before eager-clone materialization ships.

```text
1. Device A: scan --adopt a git repo (with an existing remote) and a remote-less draft folder
2. Device A: capture an env profile for the repo
3. Device A: devstrap sync (push namespace map + encrypted env/draft blobs to the test hub)
4. Device B (fresh root): devstrap sync
5. assert the repo is materialized at the SAME namespace path via blobless clone (git clone --filter=blob:none), not a skeleton
6. assert the draft folder is restored byte-identical (excluding .devstrapignore-pruned paths)
7. assert env hydrates to the requested file with mode 0600 and the original value
8. assert node_modules / build artifacts are absent (rebuilt on hydrate, never synced)
9. run devstrap sync again on Device B and assert it pulls 0 new events (idempotent)
10. assert NO .git bytes ever transit the hub: the hub backend saw only the signed namespace map + age_blob:<sha256> ciphertext; repo content rode git's own transport
```

## Hub backend tests (`HUB-*`)

The cloud hub is pluggable behind one `Hub` interface with two planes — a signed HLC-ordered namespace-map event log and a content-addressed encrypted blob store (`age_blob:<sha256>`). The same conformance suite must pass against every backend: a file-backed local backend retained ONLY for tests, and Cloudflare R2 (S3 API) as the production backend.

Shipped conformance (`P5-HUB-01`): `internal/hub`'s shared `assertHubRoundTrip` runs the contract below against both the in-memory `memS3` double (`TestR2ConformanceMemS3`) and the production `aws-sdk-go-v2` `S3Adapter` against a live bucket (`TestR2MinIOConformance` in `internal/hub/r2_minio_test.go`). The MinIO/R2 test is **env-gated** (not a build tag): it skips unless `DEVSTRAP_HUB_S3_ENDPOINT` (plus `DEVSTRAP_HUB_S3_ACCESS_KEY_ID`/`DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY`) is set, so the file always compiles (a refactor cannot silently break it and `go mod tidy` keeps the SDK a stable direct require) while CI stays hermetic. The `mapS3Error` sentinel translation is hermetically unit-tested in `s3client_awssdk_test.go` to protect the coverage floor while the gated test is skipped in CI. Run the live test against a 2024+ MinIO image (for `If-None-Match: *` conditional-put support): `docker run -p 9000:9000 minio/minio server /data`, then `DEVSTRAP_HUB_S3_ENDPOINT=http://localhost:9000 DEVSTRAP_HUB_S3_ACCESS_KEY_ID=minioadmin DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=minioadmin go test -run TestR2MinIOConformance ./internal/hub`.

### Hub interface conformance (both backends)

Run the identical suite against the file-backed test backend and a Cloudflare R2 / S3 backend (real bucket or an S3-API stub such as MinIO):

```text
- event-log plane: append is signed + HLC-ordered; cursor=<HLC> pull returns only events after the cursor; a too-old cursor returns 410 -> full-state snapshot
- R2/S3 event-log plane: event objects use immutable unique keys, conditional put (`If-None-Match: *` where supported), bounded `ListObjectsV2` pagination, and `next_cursor` without unbounded prefix scans or a single overwritten manifest
- blob plane: put/get is content-addressed by sha256; a tampered blob fails its content-address check on get; blobs are namespaced by workspace_id with no cross-workspace read
- idempotency: re-putting the same age_blob:<sha256> is a no-op; re-appending a duplicate event is deduplicated
- backend parity: a fixture written via the file-backed backend and read via the R2/S3 backend (and vice versa) yields identical bytes
- hosted credential mode: clients/runners can operate with prefix-scoped temporary credentials or presigned URLs and cannot read/write outside `workspaces/<workspace_id>/...`
```

### Zero-knowledge property

```text
- a server/operator holding only the backend bytes can decrypt nothing: blobs are age ciphertext and the event log is a signed map with no plaintext code/secrets/draft content
- no plaintext repo content, secret value, or draft byte appears anywhere in the backend store
- repo content is never written to either plane (no .git, no working-tree bytes) — it rides git's own transport
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

## Audit follow-ups (2026-06-27)

Testing gaps (`TEST-*`, from `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md`):

- **No fuzz targets** for any untrusted-input parser, incl. the env parser, pathkey, and the secret scrubber (`TEST-01`); add `go test -fuzz`.
- **e2e harness covers only `init`/`status`** (`TEST-02`); the riskiest flows (scan/hydrate/worktree/agent/env/sync) are tested in-process and bypass the real exit-code/`--json` contract. Extend the `rogpeppe/go-internal` testscript suite.
- **Coverage profile is computed then discarded** and the vacuous-test guard checks only 3 packages; `internal/id` is untested (`TEST-03`).
- **gosec is narrowed to a 6-rule allowlist** disabling hardcoded-credential and weak-crypto checks (`TEST-04`); widen it. **[Implemented 2026-06-28: removed `includes` allowlist, all gosec rules now run; added `errorlint`; set `max-same-issues: 0`.]**
- **`govulncheck` is unpinned (`@latest`) and bundled into the "Go tests" job** (`TEST-05`/`CI-01`); pin it and split it into its own (non-blocking/scheduled) job. **[Implemented 2026-06-28: pinned `@v1.1.4`, split into own `vuln` CI job, `continue-on-error` on PRs, daily scheduled run.]**
- **The fsnotify watcher has no tests and concurrent code has no goroutine-leak detection** (`TEST-06`).
- **New coverage:** WIP-ref base-exclusion test, forge detection/routing, non-VCS classification, and a zero-knowledge hub test (server can decrypt nothing).
- **Envelope encryption** (`P4-SEC-02`/`SEC-07`, shipped 2026-06-30): `internal/sync/eventcrypt_test.go` (9: round-trip, signature-payload preserved, wrong-key, mutated-ID, unknown version, plaintext rejection, short CT, wrong WCK length, NewWCK), `internal/sync/encryptedhub_test.go` (8: round-trip, grant passthrough, ingest-then-decrypt two-pass, anti-downgrade, missing epoch, unknown version, blob passthrough, push-no-epoch), `internal/workspacekeys/keyring_test.go` (6: bootstrap, self-grant+ingest, no-op for other recipient, rotate excludes revoked, new-device reads history across epoch bump, tampered wrapped key), `internal/devicekeys` WCK custody (5: keychain round-trip, missing, file perms, file fallback, invalid workspace id), `cmd/devstrap/testdata/script/sync_encrypted.txtar` (e2e: hub stores only `enc.v1` carriers `! grep` plaintext path/remote, two-device decrypt after enroll+approve, revoke rotates to epoch 2).

## Audit follow-ups (2026-06-28)

New test workstreams from `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md` (cloud-sync design; all planned/future until their features ship — they do not satisfy the Phase 0 gate):

- **Eager-clone two-machine e2e** (`EAGER-*`): the round trip in *Multi-device tests* — Device B reconstructs the whole `~/Code` tree from one `sync`, repos blobless-cloned at the same path, drafts byte-identical, env hydrated, a second sync pulls 0 events, and no `.git` bytes transit the hub.
- **`.devstrapignore` single-compiler tests** (`DRAFT-*`): all consumers derive from one compiled output; the draft-bundle exclusion set is the compiler output and nothing else, so secrets, `.git`, and `node_modules` can never be age-encrypted into a blob.
- **Hub backend conformance** (`HUB-*`): one suite passes against the file-backed test backend and the Cloudflare R2 / S3 backend behind the same `Hub` interface, plus the zero-knowledge property (server can decrypt nothing).
- **Device revocation re-encryption** (device-trust): revoke -> affected blobs re-encrypted to the reduced recipient set and secrets flagged for rotation; signed-event verification must **fail closed** once enrollment exists (today `SECU-03` fails open).
- **Cross-platform parity** (`XP-*`): the eager-clone, draft-sync, and hub-backend suites run identically on macOS and Ubuntu from the one Go binary.
- **Deferred:** OS-native daemon/StrapFS sync paths and multi-user/multi-tenant hub scaling (`SCALE-*`) are documented-not-built; no tests required this cycle.
