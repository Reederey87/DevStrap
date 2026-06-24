# DevStrap

DevStrap is a local-first workspace manager for developers who work across multiple machines and AI agents.

The core idea is simple: keep a consistent `~/Code` namespace everywhere, but use developer-native tools underneath. Git remains the source of truth for repository contents, SQLite tracks local workspace state, secrets are referenced or encrypted instead of casually copied, and agent work starts from fresh upstream refs in isolated worktrees.

## Current Status

This repository is at the Phase 0 foundation stage:

- Go module and CLI entrypoint are scaffolded.
- `devstrap init`, `devstrap status`, `devstrap doctor`, and `devstrap version` exist.
- The initial SQLite schema is embedded as a Goose migration.
- CI is configured for macOS and Linux.
- Product, architecture, security, data model, and test specifications live under `spec/`.

The daemon, filesystem watcher, Git hydration, env capture, sync hub, and agent runner are planned but not implemented yet.

## Architecture

DevStrap is designed as a Mac-first, Linux-compatible managed physical namespace.

```text
~/Code                         user-visible managed tree
~/.devstrap/state.db           local SQLite state
~/.devstrap/devstrapd.sock      future local daemon socket
~/.devstrap/worktrees/          future managed agent/human worktrees
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
go test ./...
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

Check prerequisites:

```bash
go run ./cmd/devstrap doctor
```

Print version:

```bash
go run ./cmd/devstrap version
```

## Development

Run the standard checks:

```bash
gofmt -w cmd internal
go test ./...
```

Build locally:

```bash
go build -o bin/devstrap ./cmd/devstrap
```

The first implementation milestones are:

1. Finish local workspace state and project scan/adopt.
2. Add Git repository hydration and editor open adapters.
3. Add fresh worktree creation from fetched upstream refs.
4. Add env capture/hydrate with encrypted local store.
5. Add the Mac LaunchAgent daemon and watcher.

## Repository Workflow

Use the `Reederey87/DevStrap` GitHub repository. Work on feature branches from the default branch and open pull requests for review. Do not commit directly to the default branch.

## Contributing

1. Read [spec/00_START_HERE.md](spec/00_START_HERE.md) and the relevant spec file before changing behavior.
2. Keep implementation aligned with the safety invariants: do not overwrite dirty worktrees, do not log secrets, and never branch agent work from stale local `main`.
3. Add focused tests for any behavior that touches Git, secrets, filesystem reconciliation, or destructive actions.
4. Run `gofmt -w cmd internal` and `go test ./...` before opening a PR.

## License

DevStrap is licensed under the MIT License. See [LICENSE](LICENSE).
