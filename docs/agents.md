# Running agents on DevStrap

DevStrap is the **substrate** coding agents run on, not an agent runner. Its durable value is the
things a harness does not give you: the same workspace on every machine, a worktree provably based
on a freshly-fetched `origin/<default_branch>` with the base SHA recorded, a queryable registry of
what ran where, and a gate that refuses to open a PR from a stale base.

Modern harnesses — Claude Code, Cursor, Codex — already manage their own worktrees and their own
OS-level sandboxes, and they authenticate with credentials DevStrap deliberately strips. So DevStrap
does not try to run them. It gives them a worktree, or adopts the one they made, and keeps the
provenance either way.

This guide covers both directions. Neither requires a plugin, an adapter, or a running daemon.

---

## Direction 1 — DevStrap provisions, the harness runs

The primitive is one command, and it works in **every** harness today, because every harness can
run a shell command and read JSON:

```bash
devstrap worktree new work/acme/api --fresh-upstream --name fix-login --json
```

```json
{
  "id": "wt_01jz...",
  "path": "/Users/you/.devstrap/worktrees/ns_.../agent-fix-login-20260731-120405-a13f92c0b31d",
  "branch": "agent/fix-login-20260731-120405-a13f92c0b31d",
  "base_ref": "origin/main",
  "base_sha": "9a786b6193b8b8b301073d03bc14b5cc650ad1ff",
  "created_by": "agent",
  "status": "active",
  "dirty_state": "clean",
  "schema_version": 1,
  "project_path": "work/acme/api",
  "remote_url": "git@github.com:acme/api.git",
  "default_branch": "main",
  "repo_path": "/Users/you/Code/work/acme/api"
}
```

`cd` to `.path` and start the agent there. The base is a **fetched** `origin/main`, not whatever
your local `main` happened to be — which is the single most common way agent work ends up rebased
onto history it never saw.

`schema_version` is a floor, not a description: every key documented at version N is still present
with the same meaning at every later version, new keys may appear **without** a bump, and your
parser must ignore keys it does not recognize. `remote_url` has had any credentials stripped while
staying usable. Full contract: `spec/13_CLI_DAEMON_API.md` § *Machine contract surfaces*.

## Direction 2 — the harness creates the worktree, DevStrap adopts it

This is the common case, because harnesses do this on their own. Claude Code puts worktrees under
`.claude/worktrees/`; the Codex app uses `$CODEX_HOME/worktrees` and checks them out **detached**;
plain `git worktree add` is just as valid.

```bash
devstrap worktree adopt /path/to/that/worktree --json
```

Adoption is **registration, never base-resolution.** DevStrap records what the worktree was
*actually* based on — the merge-base of its HEAD with `origin/<default_branch>` — and never rewrites,
repairs, or blesses that base. An adopted worktree that is three weeks behind will say so:

```bash
devstrap worktree status wt_01jz...   # stale (behind 47)
```

Recording the branch *tip* instead would make every adopted worktree report "fresh" forever, which
is worse than having no gate at all. Detached HEAD is adopted normally — it is the common shape, not
an edge case.

Adoption refuses rather than guesses when it cannot record an honest base: an unborn HEAD, a shallow
clone (whose grafted history can yield a plausible-but-wrong merge-base — override with
`--allow-shallow`), or two histories with no common ancestor. That last one is usually a legitimate
adoption of an orphan branch, and the error tells you the fix:

```
HEAD and origin/main share no common history (adopting an orphan branch such as gh-pages is a
common, legitimate case); pass an explicit --base-ref origin/<branch that shares history with …>
```

Pass `--base-ref` as a full `remote/branch` pair (`origin/gh-pages`). A bare branch name is recorded
as-is today but then fails the later freshness check, so prefer the qualified form.

## Register the run, then ship it

```bash
devstrap agent adopt /path/to/that/worktree \
  --engine claude-code --task "fix the login redirect" --pid $$ --adopt-worktree

devstrap agent list
devstrap agent finish arun_01jz... --status complete --test-summary "42 passed"
devstrap agent pr arun_01jz...
```

`--engine` is free text. DevStrap is the registry, not the gatekeeper — label the run whatever your
harness is actually called.

`--adopt-worktree` adopts the worktree first if it is not registered yet, through the exact same
code path as `worktree adopt`. Add `--allow-shallow` alongside it if the repository is a shallow
clone; on its own that flag is a usage error, since it only reaches worktree adoption.

`agent pr` then behaves identically whether DevStrap ran the agent or merely watched: it refuses a
stale base unless you pass `--allow-stale-base`, pushes the branch, and opens the PR/MR through
`gh`/`glab`/`tea`.

One current limitation to know about: a worktree adopted while detached records an empty branch, and
that record is not refreshed by a later `git switch -c`. `agent pr` will keep refusing such a run.
Create the branch **before** adopting if you intend to open a PR from it.

### About `--pid`

`--pid` is how DevStrap learns your run died. It records that process's identity, and when the
process is gone the run is reconciled to `interrupted` instead of sitting at `running` forever.

**Pass your harness's own long-lived PID, never a wrapper shell's.** If you invoke
`sh -c 'devstrap agent adopt …'`, the shell exits immediately and DevStrap would mark a perfectly
healthy run as interrupted. There is no default for exactly this reason — DevStrap will not guess.

Omitting `--pid` is fine, but know what you are opting into: the run is **never** swept, and a
`running` run **blocks `devstrap worktree cleanup` for that worktree indefinitely**. That is why
`agent finish` exists — call it, or clean up by hand later.

## Making adoption automatic (Claude Code)

Claude Code fires hooks on session lifecycle events. A `SessionStart` hook registers every worktree
you open, with no step to remember. Add to your own `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "devstrap worktree adopt \"$PWD\" --json >/dev/null 2>&1 || true"
          }
        ]
      }
    ]
  }
}
```

The `|| true` is deliberate: most directories are not linked worktrees, and adoption refusing is the
normal case. A hook that fails your session because you opened an ordinary folder is worse than no
hook.

This is a **recipe you own**, not something DevStrap installs. DevStrap never writes to a harness's
configuration.

The commands in this guide are executed end to end by
`cmd/devstrap/testdata/script/agents_guide_provisioning.txtar` and
`cmd/devstrap/testdata/script/agent_adopt_roundtrip.txtar` on every CI run, so the sequences cannot
silently rot. What those tests cannot prove is that Claude Code actually fires the hook — CI does not
run the harness. The integration is verified up to the harness boundary, and the hook itself is
Claude Code's documented behavior, not DevStrap's guarantee.

## Provisioning via MCP

`devstrap mcp serve` exposes the same provisioning primitives as an MCP stdio server, for harnesses
that speak MCP instead of shelling out to the CLI. Add it once:

```bash
claude mcp add devstrap -- devstrap mcp serve
```

It ships five tools, one per primitive documented above, each service-prefixed so it does not collide
with another server's `worktree_new`:

- `devstrap_worktree_new` — Direction 1: create a fresh worktree for an already-registered project.
- `devstrap_worktree_adopt` — Direction 2: register a linked worktree the harness created itself.
- `devstrap_worktree_status` — is a worktree fresh or stale against its recorded base?
- `devstrap_worktree_list` — every worktree DevStrap has registered.
- `devstrap_agent_adopt` — register the run, optionally adopting the worktree in the same call.

**There is no second execution path.** Every tool handler calls the exact same internal Go function
its cobra command calls — `devstrap_worktree_new` calls `createFreshWorktree`, `devstrap_agent_adopt`
calls `adoptAgentRun`, and so on. A fresh worktree from either surface has the identical
fetched-`origin/<default_branch>`-plus-recorded-base-SHA provenance; adoption from either surface
records the same honest base and never rewrites it.

**The local stdio subprocess boundary is the trust boundary.** There is no authentication, matching
the precedent of `docker agent serve mcp` and `container-use stdio` — the client that spawns the
process already controls what it can do on this machine, so a second credential layer would protect
against nothing an MCP client's own process boundary does not already stop.

This is the same primitive as the shell-out shown above, not a replacement for it: use whichever your
harness's integration surface makes easier. `cmd/devstrap/TestMCPServeRealSubprocess` builds the real
binary, spawns `mcp serve` as a real subprocess, and speaks the actual MCP wire protocol to it over its
real stdin/stdout — pinning the five tool names above and one real `tools/call` round trip — so this
list cannot drift from what the server actually advertises.

## What DevStrap does not promise

**The wrapper's command and file policy is guardrails, not a sandbox.** `devstrap agent run`'s
argv-substring policy is bypassable by any interpreter — `bash -c`, `python -c`, base64, a script
file. Treat it as protection against accidents, not against an adversary.

Real isolation comes from the OS sandbox (macOS Seatbelt, Linux bubblewrap → Landlock+seccomp) that
`agent run` applies, or — for an externally-run agent — from **your harness's own sandbox**, composed
*inside* a DevStrap worktree. DevStrap does not sandbox a process it did not launch, and adopting a
run does not confine it.

DevStrap ships exactly one wrapper engine, `generic`. There are no `cursor-cli` / `codex-cli` /
`copilot-cli` adapters, and none are planned — that path was considered and rejected in favour of
the primitives above, which work with every harness instead of chasing each one.

## Secrets

Agents get no secrets by default. A project opts in per-key by committing a `.devstrapagent.yml`:

```yaml
agent_secrets:
  allow:
    - GITHUB_TOKEN_READONLY
    - API_BASE_URL
  deny:
    - AWS_SECRET_ACCESS_KEY
```

Deny wins on conflict. A project with no such file injects nothing, and an empty `allow` list still
denies everything — the file's presence is the opt-in, never a fallback to "no policy". This governs
only what `devstrap agent run` injects into its child's environment; it has no effect on an
externally-run agent, which inherits whatever your harness gives it.

Claude Code and the Codex app both use a `.worktreeinclude` file to copy gitignored files such as
`.env` into new worktrees. DevStrap does not intercept that. Its own answer is `devstrap env hydrate`,
which writes the file from an age-encrypted blob at `0600` — no plaintext secret is copied around
your filesystem or synced through the hub.

## Where to read further

- `spec/10_AGENT_WORKSPACES_AND_POLICIES.md` — agent isolation, policy profiles, sandbox reality
- `spec/13_CLI_DAEMON_API.md` — the full CLI contract and `--json` conventions
- `spec/14_MVP_ROADMAP_AND_BACKLOG.md` § *AD-5 backlog* — what is built, what is deliberately not
