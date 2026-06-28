# DevStrap

DevStrap is a local-first workspace manager for developers who work across multiple machines and AI agents.

The core idea is simple: keep a consistent `~/Code` namespace everywhere, but use developer-native tools underneath. Git remains the source of truth for repository contents, SQLite tracks local workspace state, secrets are referenced or encrypted instead of casually copied, and agent work starts from fresh upstream refs in isolated worktrees.

## Current Status

This repository is at the Phase 0 foundation stage:

- Go module and CLI entrypoint are scaffolded.
- `devstrap init`, `devstrap scan`, `devstrap add`, `devstrap hydrate`, `devstrap open`, `devstrap sync --hub-file`, `devstrap worktree`, `devstrap env capture/hydrate/bind`, `devstrap run`, `devstrap agent run/list/show/pr`, `devstrap devices list/approve/revoke/lost/rename`, `devstrap status`, `devstrap doctor`, `devstrap db`, and `devstrap version` exist.
- Structured `slog` logging is wired through global CLI flags and redacts secret-like fields.
- SQLite migrations cover workspace, device, namespace, Git repo, device state, event, conflict, job, and worktree metadata.
- `init` persists a generated local device ID, `scan --adopt` records discovered projects, `add` creates skeletons, `hydrate` clones Git repos, and `worktree new --fresh-upstream` bases work on fetched remote refs with `worktree status`/`finalize` stale-base checks.
- Git/editor subprocesses use a sanitized child environment instead of inheriting the full shell environment, `env capture/hydrate` stores and restores age-encrypted local blobs instead of persisting plaintext `.env` values in state, `env hydrate` can resolve 1Password provider refs through `op inject`, and `run` injects encrypted profiles or delegates 1Password refs through `op run`.
- Initial platform adapter interfaces exist for watcher, service manager, keychain, and editor launch; Darwin/Linux use an fsnotify watcher adapter and an OS keyring-backed keychain adapter with `0600` file fallback, while `open` uses the editor adapter and native service implementations remain future work.
- CI is configured for macOS and Linux.
- Product, architecture, security, data model, and test specifications live under `spec/`.

The daemon, FSEvents-specific Mac watcher, hosted sync hub, automatic remote device enrollment/fingerprint confirmation, native service installers, and OS-enforced agent sandboxing are planned but not implemented yet.

## Architecture

DevStrap is designed as a Mac-first, Linux-compatible managed physical namespace.

```text
~/Code                         user-visible managed tree
~/.devstrap/state.db           local SQLite state
~/.devstrap/devstrapd.sock      future local daemon socket
~/.devstrap/worktrees/          managed agent/human worktrees
```

Planned components:

- `devstrap`: CLI for workspace setup, status, hydration, worktrees, env, and agents.
- `devstrapd`: local daemon for reconciliation, watchers, jobs, and local API.
- DevStrap Hub: future event-log sync service for namespace, device, and encrypted blob sync.

See [spec/00_START_HERE.md](spec/00_START_HERE.md) for the full spec map.

## Requirements

- macOS or Linux
- Go 1.25 or newer
- Git
- GitHub CLI (`gh`) for repository and PR workflows

Optional future tools:

- 1Password, Doppler, or Infisical CLI for secret-provider mode
- Cursor or VS Code command-line launchers
- npm only as a possible future distribution wrapper, not as the core runtime

## Installation

For local development:

```bash
git clone git@github.com:Reederey87/DevStrap.git
cd DevStrap
go mod download
go test -race ./...
go build -o bin/devstrap ./cmd/devstrap
```

On macOS, install prerequisites with Homebrew if needed:

```bash
brew install go git gh
```

## Usage

Initialize a workspace:

```bash
go run ./cmd/devstrap init ~/Code --workspace-name personal
```

Check status:

```bash
go run ./cmd/devstrap status
go run ./cmd/devstrap status --json
```

Scan/adopt existing projects:

```bash
go run ./cmd/devstrap scan ~/Code --dry-run --json
go run ./cmd/devstrap scan ~/Code --adopt
```

Add and hydrate a Git project:

```bash
go run ./cmd/devstrap add git@github.com:acme/api.git --path work/acme/api --lfs-policy auto
go run ./cmd/devstrap hydrate work/acme/api
go run ./cmd/devstrap open work/acme/api --cursor
```

Create a fresh upstream worktree:

```bash
go run ./cmd/devstrap worktree new work/acme/api --fresh-upstream --name fix-tests
go run ./cmd/devstrap worktree status wt_01jz...
go run ./cmd/devstrap worktree finalize wt_01jz...
go run ./cmd/devstrap worktree list
```

Run a generic command in a fresh agent worktree:

```bash
go run ./cmd/devstrap agent run work/acme/api --engine generic --task "run tests" -- npm test
go run ./cmd/devstrap agent list
go run ./cmd/devstrap agent pr arun_01jz... --dry-run
```

Inspect local device trust state:

```bash
go run ./cmd/devstrap devices list
```

Push/pull namespace events through the file-backed test hub:

```bash
go run ./cmd/devstrap sync --hub-file /tmp/devstrap-hub/events.json --namespace-only
```

Check prerequisites:

```bash
go run ./cmd/devstrap doctor
```

Capture and hydrate a local env file:

```bash
go run ./cmd/devstrap env capture work/acme/api .env
go run ./cmd/devstrap env hydrate work/acme/api --write .env.local
```

Bind 1Password refs and either inject them at runtime or explicitly hydrate a local file:

```bash
go run ./cmd/devstrap env bind work/acme/api .env.refs --provider 1password
go run ./cmd/devstrap run work/acme/api -- npm test
go run ./cmd/devstrap env hydrate work/acme/api --write .env.local
```

Print version:

```bash
go run ./cmd/devstrap version
```

## Development

Run the standard checks:

```bash
gofmt -w cmd internal
go test -race ./...
```

Build locally:

```bash
go build -o bin/devstrap ./cmd/devstrap
```

The next implementation milestones are:

1. Harden scan/adopt around larger fixtures, incremental reconciliation, and richer conflict reporting.
2. Expand file-backed sync toward peer registration, skeleton reconciliation across roots, and a production hub.
3. Add OS-enforced sandboxing and project-env allowlists for the thin agent runner.
4. Add automatic remote device enrollment, fingerprint confirmation, and bundle re-encryption hooks.
5. Add the Mac LaunchAgent daemon and evaluate whether fsnotify/kqueue must be replaced by a native FSEvents adapter.

## Repository Workflow

Use the `Reederey87/DevStrap` GitHub repository. The branch workflow — trunk `main`, integration branch `dev`, feature branches from `dev` into `dev`, `dev` merged to `main` only after green CI and review, and agents/worktrees always based on the fetched `origin/<default_branch>` — is defined canonically in [AGENTS.md](AGENTS.md). Follow it there rather than restating it here.

## Contributing

1. Read [spec/00_START_HERE.md](spec/00_START_HERE.md) and the relevant spec file before changing behavior.
2. Keep implementation aligned with the safety invariants: do not overwrite dirty worktrees, do not log secrets, and never branch agent work from a stale local default branch.
3. Add focused tests for any behavior that touches Git, secrets, filesystem reconciliation, or destructive actions.
4. Run `gofmt -w cmd internal` and `go test -race ./...` before opening a PR.

## License

DevStrap is licensed under the MIT License. See [LICENSE](LICENSE).
