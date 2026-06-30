# Agent Instructions

- Read `spec/00_START_HERE.md` before changing behavior.
- Keep the core implementation in Go.
- Run `gofmt -w cmd internal`, `golangci-lint run`, `go run ./cmd/spec-drift --base origin/main --head HEAD`, and `go test -race ./...` before handoff.
- Never log secrets, overwrite dirty worktrees, or branch agent work from any local branch.
- Keep Mac-specific behavior behind adapters so Linux support remains viable.
- Branch workflow (canonical source of truth): **trunk-based**. `main` is the single, protected default branch — there is no `dev` branch. All changes land via pull request to `main`: external contributors fork and open a PR; maintainers create a topic branch from the fetched `origin/main` (never a local branch) and open a PR. `main` is protected — PRs require green CI (Spec drift, Go lint, Go tests on macOS + Linux, Vulnerability check), resolved conversations, and linear history; force-pushes and deletions are blocked. CODEOWNERS remains advisory and should still request relevant review, but one approving review and required CODEOWNERS review are not branch-gated while this is a solo-maintained OSS repo. Re-enable 1 approving review plus required CODEOWNERS review after a second active maintainer with write access is available. Never commit directly to `main`. Agents and worktrees base from the fetched `origin/main`, never any local branch.
- At the end of each agent working cycle that modifies the codebase, append a concise summary of completed work, validation, and remaining follow-ups to `spec/18_WORK_LOG.md`.
- After the last codebase modification in a session, review every file in `spec/` and update each file as needed so the specs remain accurate, complete, and current before handoff. The spec-drift gate (`cmd/spec-drift`) only proves a path-mapped spec file was *touched*, not that its prose is *correct* (`P5-DX-02`); manually verify the inventories and status claims still match the code. Two content-staleness checks back this: `internal/cli` `TestEveryCommandIsDocumented` (every command appears in `spec/13`) and `TestMigrationsDocumented` (every migration appears in `spec/12`).
- Open a PR to `main` at the end of each agent working cycle that modifies the repo, and assign it to the review subagent. Fix all issues identified during the review. Merge only after green CI; external contributor PRs still require maintainer review before merge. Maintainer-authored PRs may be squash-merged by the maintainer after green CI, with the subagent review treated as the required internal check until another write-access maintainer exists. The owner may bypass protection for hotfixes but should prefer the PR flow. Never commit directly to `main`.


## GitHub access
Ensure the SSH agent is running and the enterprise key is loaded:

```bash
eval "$(ssh-agent -s)"
ssh-add /Users/reederey/.ssh/id_ed25519 
```

Use `gh auth switch --user Reederey87`; remote `git@github.com:Reederey87/DevStrap.git`.
