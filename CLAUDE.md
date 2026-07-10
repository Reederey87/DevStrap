Read @AGENTS.md and @spec/00_START_HERE.md before changing behavior.

# Personal Preferences

## Tech Stack Preferences
When uncertain, prefer: Tailwind, Cloudflare, Fly.io, Neon, TypeScript, Go, Convex, Clerk, Vercel.

## Code Style
- Always strive for concise, simple solutions.
- If a problem can be solved in a simpler way, propose it.

## General Preferences
- If asked to do too much work at once, stop and state that clearly.
- If computer use is helpful for completing or verifying work, shell out to gpt-5.6 with Codex for it.

# Picking the right models for workflows and subagents
Rankings, higher = better. Cost reflects what I actually pay (grok-4.5 has really generous
limits), not list price. Intelligence is how hard a problem you can hand the model unsupervised. Taste covers UI/UX, code quality, API design, and copy.

| Model     | Cost | Intelligence | Taste |
|-----------|------|--------------|-------|
| gpt-5.6   | 6    | 8            | 6     |
| grok-4.5  | 9    | 6            | 5     |
| sonnet-5  | 4    | 4            | 7     |
| opus-4.8  | 5    | 5            | 8     |
| fable-5   | 2    | 9            | 9     |


How to apply:
- These are defaults, not limits. You have standing permission to override them: if a cheaper model's output doesn't meet the bar, rerun or redo the work with a smarter model without asking. Judge the output, not the price tag. Escalating costs less than shipping mediocre work.
- Cost is a tie-breaker only; when axes conflict for anything that ships, intelligence > taste > cost.
- Bulk/mechanical work (clear-spec implementation, data analysis, migrations).
- Anything user-facing (UI, copy, API design) needs taste ≥ 7.
- Reviews of plans/implementations: fable-5 or opus-4.8, optionally gpt-5.6 as an extra independent perspective.
- Use Grok-4.5 for implementation of predefined plans, but only if the plan is clear and complete. Otherwise, use gpt-5.6. Use Grok-4.5 for exploration work on codebase and searching information using `exa` ncp search tools.
- Never use Haiku model.

## gpt-5.6 (via Codex plugin)
- Mechanics: gpt-5.6 is only reachable through the `codex:codex-rescue` subagent (Codex plugin for Claude Code), my `~/.codex/config.toml` defaults to gpt-5.6.
- `/codex:review` - review uncommitted changes or branch vs base (`--base <ref>`). Supports `--wait` and `--background`. Not steerable, no custom focus text.
- `/codex:adversarial-review` - challenge a specific decision or risk area.
- `/codex:rescue` - investigate a bug, try a fix, continue a previous task, or take a cheaper pass. Supports `--background`, `--wait`, `--resume`, `--fresh`.
- `/codex:transfer` - create a persistent Codex thread from the current Claude Code session. Prints `codex resume <session-id>`.
- `/codex:status` - check progress on background work, see latest completed job, confirm if a task is still running.
- `/codex:result` - show final stored Codex output for a finished job. Includes Codex session ID for `codex resume <session-id>`.
- `/codex:cancel` - cancel an active background Codex job.
- Stall-detecting loop: use `/codex:status` every 10 mins.

## grok-4.5 (via Grok plugin)
- Use `/grok:rescue --write` for low-cost implementation of predefined plans. All headless writes require explicit `--always-approve` or `--yolo`.
- `/grok:setup` - check local Grok Build CLI availability and auth.
- `/grok:ask [question]` - read-only repository question.
- `/grok:review [--base <ref>] [--scope working-tree|branch|repo] [--background]` - read-only code review of local git changes. Grok Build supports review via headless prompts.
- `/grok:rescue [--write] [--always-approve|--yolo] [--background|--wait] [--resume|--fresh] [task]` - diagnosis, fix, or implementation. Write mode requires Git repo and `--always-approve`.
- `/grok:status [job-id]` - show active/recent Grok jobs for this repo.
- `/grok:result [job-id]` - show stored final output for a finished Grok job.
- `/grok:cancel [job-id]` - cancel an active background Grok job. Without a job-id, cancels the latest active job in the current session.


- Claude models (sonnet-5, opus-4.8, fable-5) run via the Agent/Workflow model parameter.

- Using gpt-5.6 inside workflows and subagents by using Codex plugin for Claude Code (the model parameter only takes Claude models, so use a wrapper):
- Spawn a thin Claude wrapper agent with 'model: 'sonnet', effort: "low"' whose prompt instructs it to write a self-contained codex prompt, run 'codex exec' via Bash, and return
- Exception: for ordinary (non-Workflow) delegation, the `codex:codex-rescue` subagent IS the wrapper — spawn it directly with the task spec; no sonnet shim needed.

- gpt-5.6 via `codex:codex-rescue` went 3/3 clean on clear-spec fixes when the prompt contained a **written line-level spec** (defect + exact fix + named tests + "commit nothing"). It also did convention-consistent spec/ledger updates unprompted. The line-by-line diff review remains mandatory — it has since caught real issues (an out-of-spec test drive-by, placeholder finding IDs).
- Dual review (opus-4.8/fable-5 + Codex) on small PRs surfaces 1–2 real Minors per PR that the implementer missed (symlink over-refusal, cleanup no-op under the cancelling ctx). Worth the cost even for S-effort fixes; skip only for pure-docs PRs (single reviewer).
- Reviewer/worker subagents sometimes go idle without posting their result — a SendMessage nudge reliably shakes the report loose; don't respawn.
- Nudges to workers must be **report-only** ("post your report; make no further edits") — a nudged worker may run another pass and silently overwrite your fixes in its worktree. Generic check: after any delegated-worktree interaction, re-diff immediately before committing; never commit on the assumption the tree still matches your last review.
- Parallel grok-4.5 implementation agents must use isolation: "worktree" so codex edits don't collide in the shared checkout.
