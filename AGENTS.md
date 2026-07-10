# Agent Instructions

> **Scope (AD-8):** this file is the *maintainer's* agent workflow — the conventions coding agents follow when working on DevStrap on the maintainer's behalf. It is not a contributor obligation: external contributors only need `CONTRIBUTING.md`, and the spec-drift/work-log bookkeeping described here is completed by the maintainer at merge for fork PRs.

Read `spec/00_START_HERE.md` before changing behavior. Core stays in Go; keep Mac-specific behavior behind adapters so Linux remains viable. Never log secrets or overwrite dirty worktrees.

## Branches & worktrees (trunk-based)

- `main` is the only long-lived branch, protected: green CI (Spec drift, Go lint, Go tests macOS+Linux, Vulnerability check), resolved conversations, linear history. Never commit to it directly.
- Local `main` is routinely stale. `git fetch origin` first; read status and code from `origin/main` (`git show origin/main:<path>`), not the working tree.
- Base every topic branch and agent worktree on the fetched `origin/main`, never any local branch: `git worktree add <tmp-dir> -b <branch> origin/main`. Use disposable worktrees outside the repo; after merge, remove the worktree and delete the branch.
- External contributors fork and open a PR. CODEOWNERS is advisory while solo-maintained; re-enable one approving review + required CODEOWNERS review once a second write-access maintainer exists.

## PR cycle

One PR per working cycle that modifies the repo; prefer one PR per audit finding.

1. **Contents:** code + tests + spec updates + a newest-first `spec/18_WORK_LOG.md` entry + `docs/audits/README.md` ledger reconciliation (shipped findings move to *Recently shipped*; the pass header's open count must equal its open-table row count).
2. **Gates before pushing:** `gofmt -w cmd internal`; `golangci-lint run` (if not installed: `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run`); `go run ./cmd/spec-drift --base origin/main --head HEAD`; `go test -race ./...`. The drift gate only proves a mapped spec was *touched* (`P5-DX-02`): after the session's last code change, review every `spec/` file so inventories and status claims still match the code; `TestEveryCommandIsDocumented` and `TestMigrationsDocumented` back this.
3. **Review:** an independent review pass (Claude reviewer subagent; add a Codex pass for code changes). Fix all findings before merge. External-contributor PRs additionally require maintainer review.
4. **Merge:** `gh pr merge --squash --auto` after green CI. Every review thread — including CodeRabbit's — must be replied to **and resolved**, or auto-merge stays blocked. The owner may bypass protection for hotfixes but should prefer the PR flow.
5. **Multi-PR waves:** merge serially and rebase each successor — `spec/18` and the ledger conflict by design. Keep all work-log entries, re-derive the ledger count from the table, and `grep -c '<<<<<<<'` every resolved file before `git rebase --continue`.

## GitHub access

Ensure the SSH agent is running and the key is loaded:

```bash
eval "$(ssh-agent -s)"
ssh-add /Users/reederey/.ssh/id_ed25519
```

Use `gh auth switch --user Reederey87`; remote `git@github.com:Reederey87/DevStrap.git`.

## Live-R2 dogfood credentials

Live-R2 runs read their S3 credentials from a stable, `0600`, **never-committed** file at `~/.devstrap/dogfood-r2.env` (outside the repo). It `export`s the five `DEVSTRAP_HUB_S3_*` vars (`ENDPOINT`, `REGION=auto`, `ACCESS_KEY_ID`, `SECRET_ACCESS_KEY`, `BUCKET`; values from the R2 dashboard, see `spec/19` §A).

Per run: `source ~/.devstrap/dogfood-r2.env` in **each** shell invocation (it doesn't persist across tool calls); `export DEVSTRAP_HUB="r2://$DEVSTRAP_HUB_S3_BUCKET"`; simulate devices on one Mac with `DEVSTRAP_NO_KEYCHAIN=1` + per-device `--home`/`--root`; `db migrate` each device home before its first `sync`. If the file exists, don't re-ask how to provide creds; if absent, ask the maintainer to create it once — **never paste the secret into the chat.**
