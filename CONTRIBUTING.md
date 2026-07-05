# Contributing

DevStrap is **trunk-based**: `main` is the single protected branch.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Workflow

- Read `spec/00_START_HERE.md` and the relevant spec before changing behavior.
- **External contributors:** fork the repository, create a topic branch, and open a pull request against `main`.
- **Maintainers:** create a topic branch from the fetched `origin/main` (never a local branch) and open a pull request against `main`.
- `main` is protected: pull requests require green CI, resolved conversations, and linear history; direct pushes, force-pushes, and deletions are blocked.
- CODEOWNERS is advisory while DevStrap is solo-maintained. It should still request relevant review, but 1 approving review plus required CODEOWNERS review are not branch-gated until a second active maintainer with write access exists.
- Do not commit directly to `main`. A maintainer reviews external-contributor PRs and squash-merges once CI is green; maintainer-authored PRs may be squash-merged by the maintainer after green CI. The head branch is deleted automatically.

## Spec Drift and the Work Log

Code, spec, and docs changes normally require a newest-first entry at the top
of `spec/18_WORK_LOG.md`, and a change under `cmd/**` or `internal/**` usually
needs its owning `spec/*.md` file updated too — `go run ./cmd/spec-drift`
checks both and CI enforces it.

**External (fork) PRs run this gate in advisory mode**: missing spec/work-log
updates surface as CI warnings, not failures. You're welcome to add the
work-log entry and spec updates yourself, but you don't have to — the
maintainer completes that bookkeeping at merge. Small fixes (typos, comment
corrections, small bug fixes) need no spec or work-log changes at all on fork
PRs.

Maintainer-authored PRs and pushes to `main` keep the gate blocking.

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
