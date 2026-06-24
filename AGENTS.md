# Agent Instructions

- Read `spec/00_START_HERE.md` before changing behavior.
- Keep the core implementation in Go.
- Run `gofmt -w cmd internal` and `go test ./...` before handoff.
- Never log secrets, overwrite dirty worktrees, or branch agent work from local `main`.
- Keep Mac-specific behavior behind adapters so Linux support remains viable.
- Use feature branches and PRs; do not commit directly to the default branch, use dev branch for development. You can make PR and merge to dev branch after all green, then merge dev to main after background agent's review and another subagent approval.
