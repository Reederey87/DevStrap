Read @AGENTS.md and @spec/00_START_HERE.md before changing behavior.

## Picking the right models for workflows and subagents
Rankings, higher = better. Cost reflects what I actually pay (OpenAI has really generous
limits), not list price. Intelligence is how hard a problem you can hand the model unsupervised. Taste covers UI/UX, code quality, API design, and copy.

| Model     | Cost | Intelligence | Taste |
|-----------|------|--------------|-------|
| gpt-5.5   | 9    | 8            | 5     |
| sonnet-5  | 5    | 5            | 7     |
| opus-4.8  | 4    | 7            | 8     |
| fable-5   | 2    | 9            | 9     |


How to apply:
- These are defaults, not limits. You have standing permission to override them: if a cheaper model's output doesn't meet the bar, rerun or redo the work with a smarter model without asking. Judge the output, not the price tag. Escalating costs less than shipping mediocre work.
- Cost is a tie-breaker only; when axes conflict for anything that ships, intelligence > taste > cost.
- Bulk/mechanical work (clear-spec implementation, data analysis, migrations): gpt-5.5 - it's effectively free.
- Anything user-facing (UI, copy, API design) needs taste ≥ 7.
- Reviews of plans/implementations: fable-5 or opus-4.8, optionally gpt-5.5 as an extra independent perspective.
- Never use Haiku model.
- Mechanics: gpt-5.5 is only reachable through the Codex through the `codex:codex-rescue` subagent (Codex plugin for Claude Code), my `~/.codex/config.toml` defaults to gpt-5.5.
- For gpt-5.5 use the `/codex:review` to run a normal Codex review on your current work. Use it when you want:
    - a review of your current uncommitted changes
    - a review of your branch compared to a base branch like `main`
    - use `--base <ref>` for branch review. It also supports `--wait` and `--background`. It is not steerable and does not take custom focus text. 
    - use `/codex:adversarial-review` when you want to challenge a specific decision or risk area.
- For gpt-5.5 use the `/codex:rescue` when you want Codex to:
    - investigate a bug
    - try a fix
    - continue a previous Codex task
    - take a faster or cheaper pass with a smaller model.
    - It supports `--background`, `--wait`, `--resume`, and `--fresh`. If you omit `--resume` and `--fresh`, the plugin can offer to continue the latest rescue thread for this repo.
- For gpt-5.5 use the `/codex:transfer` to Creates a persistent Codex thread from the current Claude Code session and prints a `codex resume <session-id>` command. Use it when you started a debugging or implementation conversation in Claude Code and want to continue that same context directly in Codex.
- For gpt-5.5 use the `/codex:status` to see running and recent Codex jobs for the current repository. Use it to:
    - check progress on background work
    - see the latest completed job
    - confirm whether a task is still running
- For gpt-5.5 use the `/codex:result` to show the final stored Codex output for a finished job. When available, it also includes the Codex session ID so you can reopen that run directly in Codex with `codex resume <session-id>`.
- For gpt-5.5 use the `/codex:cancel` to cancel an active background Codex job.
- For gpt-5.5 use the Codex job stall-detecting loop by using `/codex:status` every 10 mins.
- Claude models (sonnet-5, opus-4.8, fable-5) run via the Agent/Workflow model parameter.

Using gpt-5.5 inside workflows and subagents by using Codex plugin for Claude Code (the model parameter only takes Claude models, so use a wrapper):
- Spawn a thin Claude wrapper agent with 'model: 'sonnet', effort: "low"' whose prompt instructs it to write a self-contained codex prompt, run 'codex exec' via Bash, and return
- Exception: for ordinary (non-Workflow) delegation, the `codex:codex-rescue` subagent IS the wrapper — spawn it directly with the task spec; no sonnet shim needed.

- gpt-5.5 via `codex:codex-rescue` went 3/3 clean on clear-spec fixes when the prompt contained a **written line-level spec** (defect + exact fix + named tests + "commit nothing"). It also did convention-consistent spec/ledger updates unprompted. The line-by-line diff review remains mandatory — it has since caught real issues (an out-of-spec test drive-by, placeholder finding IDs).
- Dual review (opus-4.8/fable-5 + Codex) on small PRs surfaces 1–2 real Minors per PR that the implementer missed (symlink over-refusal, cleanup no-op under the cancelling ctx). Worth the cost even for S-effort fixes; skip only for pure-docs PRs (single reviewer).
- Reviewer/worker subagents sometimes go idle without posting their result — a SendMessage nudge reliably shakes the report loose; don't respawn.
- Nudges to workers must be **report-only** ("post your report; make no further edits") — a nudged worker may run another pass and silently overwrite your fixes in its worktree. Generic check: after any delegated-worktree interaction, re-diff immediately before committing; never commit on the assumption the tree still matches your last review.
- Give each parallel Codex job its own git worktree; they collide otherwise.

