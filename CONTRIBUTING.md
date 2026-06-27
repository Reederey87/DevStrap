# Contributing

## Workflow

- Read `spec/00_START_HERE.md` and the relevant spec before changing behavior.
- Branch from `dev`; open pull requests back into `dev`.
- Merge `dev` into `main` only after review and green CI.
- Do not commit directly to `main`.
- Do not use or recreate the legacy default branch.

## Local Checks

Run before handoff:

```bash
gofmt -w cmd internal
golangci-lint run
go run ./cmd/spec-drift --base origin/main --head HEAD
go test -race ./...
go vet ./...
go build ./...
```

## Safety Expectations

- Never log secrets.
- Never overwrite dirty worktrees.
- Never create agent work from a stale local default branch.
- Keep Mac-specific behavior behind adapters so Linux remains viable.
- Add focused tests for Git, secrets, filesystem reconciliation, database migrations, and destructive actions.

## Branch Rename

Existing clones should update to `main`:

```bash
git branch -m <old-default-branch> main
git fetch origin
git branch -u origin/main main
git remote set-head origin -a
git remote prune origin
```
