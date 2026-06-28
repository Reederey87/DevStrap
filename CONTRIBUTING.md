# Contributing

DevStrap is **trunk-based**: `main` is the single protected branch.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Workflow

- Read `spec/00_START_HERE.md` and the relevant spec before changing behavior.
- **External contributors:** fork the repository, create a topic branch, and open a pull request against `main`.
- **Maintainers:** create a topic branch from the fetched `origin/main` (never a local branch) and open a pull request against `main`.
- `main` is protected: pull requests require green CI, an approving review, CODEOWNERS review, resolved conversations, and linear history; direct pushes, force-pushes, and deletions are blocked.
- Do not commit directly to `main`. A maintainer reviews and squash-merges PRs once CI is green; the head branch is deleted automatically.

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

## Keeping a clone current

`main` is the only long-lived branch. Keep your fork/clone in sync before starting work:

```bash
git fetch origin
git checkout main
git merge --ff-only origin/main
```
