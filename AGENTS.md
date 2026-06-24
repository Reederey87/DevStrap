# Agent Instructions

- Read `spec/00_START_HERE.md` before changing behavior.
- Keep the core implementation in Go.
- Run `gofmt -w cmd internal` and `go test ./...` before handoff.
- Never log secrets, overwrite dirty worktrees, or branch agent work from local `main`.
- Keep Mac-specific behavior behind adapters so Linux support remains viable.
- Use feature branches into `dev`; merge `dev` into `master` only after green CI and review. Do not commit directly to `master`.
