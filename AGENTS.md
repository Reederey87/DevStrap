# Agent Instructions

- Read `spec/00_START_HERE.md` before changing behavior.
- Keep the core implementation in Go.
- Run `gofmt -w cmd internal`, `golangci-lint run`, `go run ./cmd/spec-drift --base origin/main --head HEAD`, and `go test -race ./...` before handoff.
- Never log secrets, overwrite dirty worktrees, or branch agent work from any local branch.
- Keep Mac-specific behavior behind adapters so Linux support remains viable.
- Branch workflow (canonical source of truth): trunk is `main`, integration branch is `dev`. Create feature branches from `dev` and merge them back into `dev`; merge `dev` into `main` only after green CI and review. Do not commit directly to `main` branch. Agents and worktrees base from the fetched `origin/<default_branch>`, never any local branch.
- At the end of each agent working cycle that modifies the codebase, append a concise summary of completed work, validation, and remaining follow-ups to `spec/18_WORK_LOG.md`.
- After the last codebase modification in a session, review every file in `spec/` and update each file as needed so the specs remain accurate, complete, and current before handoff.
- Make PR at the end of each agent working cycle that modifies the codebase, and assign it to the subagent for review. Fix all issues identified during the review. Merge PR only after green CI and review. Do not commit directly to `main` branch.
