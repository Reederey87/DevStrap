# DevStrap — Design & Implementation Audit (Eighth Pass)

_Date: 2026-07-31 · Trunk audited: `a7938dd` (`docs(spec): reconcile the AD-5 backlog rows with what actually shipped (#257)`)_

## How this relates to the prior audits

This is the **eighth** design & implementation pass. It does not restate open recommendations from earlier passes; those remain tracked in `docs/audits/README.md`. Pass 7's engineering backlog is fully closed — its four remaining rows are the commercial/hosted-tier cluster, gated on a business decision rather than on engineering.

This pass concentrates on the **111 commits / 244 files / +38k lines** that landed after Pass 7's trunk snapshot (`d667530`), none of which had been audited:

1. **The AD-5 agent-substrate wave** (`AD5-00`…`AD5-06`, PRs #251–#257, merged the same day as this audit) — `worktree adopt`, `agent adopt`/`agent finish`, the `worktree new --json` machine contract, migration `00032`. The newest and least-exercised code in the tree.
2. **The Milestone 5 daemon and its honesty follow-up** (`M5D-01..07`, PRs #229–#241) — socket API, single-flight convergence, watch plane.
3. **The working-state plane, Layer A and Layer B** (PRs #217–#226, #242–#249) — gitstate capture, WIP refs, `wip gc` on the convergence path.
4. **Security, secrets and data integrity across all of it** — the sandbox refactor, new `--json` payloads, migrations `00029`–`00032`.

**ID scheme.** Findings are prefixed `P8-` per the ledger convention. Dimension codes used here: `SEC` (security/sandbox/data integrity), `ADOPT` (the adoption/registry plane), `DAEMON`, `WIP`.

## Methodology, and its honest limits

Four dimension reviewers ran against the `a7938dd` worktree — fable-5 on the adoption plane, opus-5 on the daemon and on the WIP plane, GPT-5.6 (via Codex) on security and data. Each was told which earlier findings are shipped so it hunts **new** issues, and each was held to a four-part evidence standard: cite `file:line`; give a concrete failure scenario; **adversarially self-verify** and report what the attempt to refute found; and check novelty against this ledger.

Reviewers were also told explicitly that *three verified findings beat fifteen speculative ones*, and that "nothing above P3" is a useful result. This is deliberate: Pass 7 downgraded three candidate P1s during verification, and a plausible-but-wrong finding costs a real implementation cycle.

**Every finding below that is marked CONFIRMED was reproduced by the coordinator independently** — not accepted on the reviewer's report. Two were reproduced by executing the defect rather than by re-reading the code, and one of those changed its severity.

**Coverage is uneven, and that is stated rather than smoothed over.** The adoption and security dimensions reported in full. The **WIP dimension did not report** and its findings are absent from this document; the working-state plane is therefore **not** covered by this pass and should be carried into Pass 9. The daemon dimension's report was outstanding at write-up. A partial audit recorded as partial is more useful than one whose gaps are invisible.

**Severity:** P1 = correctness/security/data-loss; P2 = significant; P3 = minor/polish. **Effort:** S ≈ <½ day, M ≈ 1–3 days, L ≈ ~1 week.

## Executive summary

The post-Pass-7 waves hold up structurally. The AD-5 base-resolution invariant — that adoption never influences how an agent worktree resolves its base — was attacked directly and **holds**; the new test suite was attacked for revert-survivability and, unusually, **no test was found that cannot fail**.

What this pass found instead is a **working sandbox escape** in code that predates the wave, and a **data-visibility loss** that the wave made reachable for the first time. Both were demonstrated, not argued.

The headline (`P8-SEC-02`) is that the OS sandbox grants a linked worktree's git admin directory **wholesale**, and that directory contains `commondir` — the pointer to the shared `.git`. An agent that rewrites it relocates git's entire configuration into its own writable space, and the next **unsandboxed** `git status` executes whatever it planted. DevStrap triggers that `git status` itself, from `worktree cleanup`. This is the same class as `P7-SANDBOX-01`, which closed the hole for the common dir while leaving the pointer to it writable.

The second (`P8-SEC-01`) is that tombstone GC hard-deletes `namespace_entries`, and `worktrees.namespace_id` is `ON DELETE CASCADE` — so a routine `hub compact` silently erases live worktree registrations, including dirty ones. Structurally pre-existing; `AD5-02` widened the blast radius by making long-lived adopted registrations common.

A third finding is a process failure worth more than its severity: **a document five specs asserted had shipped was never committed at all**, silently swallowed by a case-insensitive `.gitignore` match.

## Findings at a glance

| ID | Sev | Finding | Effort | Status |
|---|---|---|---|---|
| `P8-SEC-02` | **P1** | Sandbox grants the worktree gitdir wholesale, incl. `commondir` → unsandboxed code execution | M | Open |
| `P8-SEC-01` | P2 | Tombstone GC cascade-deletes live worktree registrations | M | Open |
| `P8-ADOPT-01` | P2 | `docs/agents.md` never committed; five specs asserted it existed | S | **Fixed** (PR #258) |
| `P8-ADOPT-02` | P2 | `worktrees.branch` frozen at insert → `agent pr`'s own remedy can never work | S | Open |
| `P8-ADOPT-03` | P2 | `--base-ref` recorded unvalidated; also admits `refs/devstrap/*` | S | Open |
| `P8-ADOPT-04` | P2 | Provision→register gap: fresh worktree reap-eligible; `agent adopt` inserts without the repo lock | M | Open |
| `P8-ADOPT-06` | P3 | `agent finish` without `--test-summary` skips the only diff-summary capture | S | Open |
| `P8-ADOPT-07` | P3 | Pre-`00032` rows hold unresolved paths; index cannot see the alias | S | Open |

## Security & Sandbox

### `P8-SEC-02` (P1) — the granted worktree gitdir contains `commondir` — CONFIRMED BY DEMONSTRATION

`WorktreeSandboxWriteDirs` (`internal/git/git.go:944-946`) appends `identity.GitDir` — the whole per-worktree admin directory — to the sandbox's writable set for a linked worktree. That directory contains `commondir  gitdir  HEAD  index  logs  ORIG_HEAD  refs`.

Demonstrated in a scratch repository, since removed:

1. The agent creates `<worktree>/evil-common/` containing a `config` and `hooks/` — entirely inside its own writable area.
2. It rewrites `.git/worktrees/<name>/commondir` to point there. **This file is writable by grant.**
3. `git config --get core.hooksPath` run from the worktree returns the attacker's path — git honours the relocated common dir's configuration.
4. An ordinary `git status --porcelain`, **outside any sandbox**, executed the injected `core.fsmonitor` program.

**DevStrap triggers step 4 itself.** `worktree cleanup` and `worktree remove` call `git.Runner.DirtyState` (`internal/cli/worktree.go:1021`, `:1058`), which runs `git status` in the worktree unsandboxed. The escape does not require the user to inspect the agent's work; routine housekeeping suffices.

This is precisely the class `P7-SANDBOX-01` closed for the common dir, where `hooks/` and `config` are deliberately excluded with the comment that granting them "would let the child plant a hook or config that executes UNSANDBOXED on a later git operation." The per-worktree gitdir never received that treatment — and it is the pointer *to* the protected directory.

**Not introduced by the AD-5 wave.** The security dimension diffed `WorktreeSandboxWriteDirs` byte-for-byte against its pre-refactor form (`5ed3b6a`): the grant set is identical, and the `WorktreeIdentity` extraction changed nothing here.

**Fix.** Stop granting the admin dir wholesale. Grant the specific children git needs to write — `index`, `HEAD`, `ORIG_HEAD`, `COMMIT_EDITMSG`, `logs/`, `refs/` — and deny `commondir`, `gitdir`, and `config.worktree` (the last reachable whenever `extensions.worktreeConfig` is on, which sparse-checkout enables automatically). Pin it with a live Seatbelt e2e in the mould of `TestSeatbeltAllowsLinkedWorktreeCommit`: prove `git commit` still succeeds **and** that writing `commondir` is refused. A test that only proves the commit still works would pass with the hole open.

### `P8-SEC-01` (P2) — tombstone GC cascade-deletes live worktree registrations — CONFIRMED

`GCTombstones` (`internal/state/store.go:1607-1624`) hard-`DELETE`s tombstoned `namespace_entries` rows and never consults `worktrees`. `worktrees.namespace_id` is `ON DELETE CASCADE` (`00001_initial.sql:125`). `hub compact --gc-tombstones` **defaults to true** (`internal/cli/hub_compact.go:86`), so this is the routine path.

Reproduced directly: an adopted worktree with `dirty_state='dirty'`, its project tombstoned, `GCTombstones` past the HLC — `worktrees` rows for that project went **1 → 0**.

The delete-vs-dirty guard offers no protection: `decideDelete` (`internal/sync/decide.go:216-234`) inspects only the *main checkout's* `device_project_state.dirty_state`, never the `worktrees` table, so a dirty **linked** worktree does not block the delete. `agent_runs.worktree_id` is `ON DELETE SET NULL`, so run rows are silently detached at the same moment.

The on-disk checkout and its uncommitted diff survive; DevStrap's record of them does not. The worktree vanishes from `worktree list`, `status`, and `doctor`, and the stale-base gate and `agent pr` base-provenance guarantee — the core AD-5 invariant — can no longer be applied to it.

Structurally pre-existing (schema since `00001`, GC wired ~2026-07-04). **`AD5-02` materially widened the blast radius**: before adoption only `worktree new` created rows, and those are typically short-lived; adoption makes long-lived, externally-created registrations common for the first time.

**Fix.** `GCTombstones` must skip (and report) an entry that still has `worktrees` rows, or the FK becomes `SET NULL` with a `doctor` orphan check. Separately, `decideDelete` should consult `worktrees` for any active dirty row, not only the main checkout.

## Adoption & Registry

### `P8-ADOPT-01` (P2) — a document five specs asserted had shipped was never committed — FIXED IN PR #258

`docs/agents.md`, the deliverable of `AD5-04`, was absent from every commit: `git log --all -- docs/agents.md` was empty. Meanwhile `README.md` linked it, `spec/00` referenced it three times, `spec/10` twice, `spec/14`'s `AD5-04` acceptance criterion read *"the recipe published in `docs/agents.md`"*, and `spec/18` recorded it as delivered.

**Cause.** `.gitignore` carries `AGENTS.md` under "# Agents files". Git matches such a pattern by basename at any depth, and `core.ignorecase` is `true` on macOS and Windows — so `docs/agents.md` matched, and `git add -A` skipped it **silently**, as `add` does with ignored paths. The repository's own root `AGENTS.md` was unaffected because an ignore rule never untracks an already-tracked file, which is exactly why the rule looked harmless for months.

**The verification failure is the transferable lesson.** The post-rebase check was `test -f docs/agents.md`, which passes for an *untracked* file in the working tree: it proved the file existed on disk, never that it was in the commit. `git cat-file -e <rev>:<path>` or `git diff --cached --name-only` are the checks that would have caught it. See `spec/11` for the full note.

### `P8-ADOPT-02` (P2) — the frozen branch column dead-ends `agent pr`

`UpdateWorktreeAdoption` (`internal/state/store.go:4494-4505`) updates `base_ref`/`base_sha`/`dirty_state` only; no code path ever updates `worktrees.branch`. A worktree adopted while detached records `branch=""` permanently.

`agent pr` refuses a branchless run and prints the remedy "`git switch -c <name>`, then re-run" (`internal/cli/agent.go:661-663`) — but following it changes nothing, because the stored column is never refreshed. Re-running `worktree adopt` does not help either: `adoptWorktreeAt` has the fresh `identity.Branch` in hand and discards it (`internal/cli/worktree.go:448`). **The command's own documented remedy cannot work**, which is worse than no remedy: it sends the user in a circle.

**Fix.** Add `branch` to the adoption UPDATE and its call site, or have `agent pr` re-read live HEAD when the stored branch is empty.

### `P8-ADOPT-03` (P2) — `--base-ref` is recorded without validation

`MergeBase` accepts any committish, so `--base-ref` is stored verbatim (`internal/cli/worktree.go:406`) and only rejected much later, by `BaseDrift`'s `remote/branch` shape check (`internal/git/git.go:1049-1052`), with no remedy named. Three concrete consequences:

- `--base-ref main` — the natural mistake — adopts successfully, then every subsequent `worktree status`, `finalize`, and `agent pr` fails with `base ref must be remote/branch`.
- A legitimate `upstream/main` (fork workflow) survives `BaseDrift`, but `agent pr`'s `strings.TrimPrefix(base, "origin/")` (`internal/cli/agent.go:671`) then sends `upstream/main` to the forge as the base branch.
- **`--base-ref refs/devstrap/wip/<device>/<path>` is accepted**, recording a base derived from the human WIP plane into an agent-visible row. This does not breach the AD-8 invariant — that governs *automatic* resolution, and nothing automatic reads the WIP plane — but validation should refuse `refs/devstrap/*` explicitly rather than relying on a later shape check to fail by accident.

**Fix.** Shape-validate at record time with a usage error naming the expected `remote/branch` form; reject `refs/devstrap/*`; split the remote properly in `agent pr` instead of trimming a hardcoded prefix.

### `P8-ADOPT-04` (P2) — the provision→register gap

Two defects compose. A freshly-provisioned worktree has tip == `base_sha`, so `git branch --merged` reports it merged (it is an ancestor) and `worktree cleanup --merged` considers it reap-eligible from the moment it is created until a run row exists. And `agent adopt` inserts its `agent_runs` row **without holding the repo lock** (`internal/cli/agent_adopt.go:123`), breaking the `P7-GIT-01` discipline `agent run` observes by holding it across worktree creation and `InsertAgentRun` (`internal/cli/agent.go:365-374`).

A harness that calls `worktree new --json`, starts work, and calls `agent adopt` moments later can therefore have its workspace removed underneath it by a concurrent `cleanup`. The worktree is clean at that instant so no content is lost, but the session's directory disappears and a `running` run can end up bound to a removed worktree.

**Fix.** Hold the repo lock across resolve→`InsertAgentRun` in `agent adopt`. Add a `tip == base_sha` no-work guard to cleanup — a pristine worktree contains no merged work to reclaim.

### `P8-ADOPT-06` (P3) — the diff summary is captured only when `--test-summary` is passed

`agent finish` recomputes the diff summary only on the `--test-summary` branch (`internal/cli/agent_adopt.go:218-236`), whose own comment states this is "the only chance to record it" for an adopted run. Since `finish` is deliberately non-idempotent, a run finished without `--test-summary` has an empty diff summary in `agent show` and in its PR body forever. **Fix:** recompute unconditionally whenever the worktree resolves.

### `P8-ADOPT-07` (P3) — legacy rows hold unresolved paths

Path normalization (`EvalSymlinks`) arrived with `AD5-02` (`internal/cli/worktree.go:1143`). Rows written before it store the unresolved spelling, so on a symlinked home prefix `adopt` misses the legacy row and the string-keyed unique index admits a second active row for one physical worktree. Self-heals via cleanup's path-missing prune. Migration `00032` itself **cannot** fail on real data: only `worktree new` inserted rows before it, and its paths embed a timestamp plus 12 hex characters of randomness.

## Checked and found sound

Recorded so the next pass does not repeat the work:

- **The AD-5 base-resolution invariant holds.** `adoptWorktreeAt` never calls `UpdateGitDefaultBranch`; `RemoteDefaultBranch` is a pure read with no `set-head` repair; the fresh-worktree resolver never reads the `worktrees` table; the WIP plane is untouched by adoption.
- **No test-that-cannot-fail was found** among the wave's new tests — notable given the base rate (this wave shipped two that review had to fix, and the daemon wave two before it). `TestWorktreeAdoptRecordsAncestorNotBaseTip` genuinely fails under a records-the-tip mutant; the pidless-run test fails if the `runner_pid IS NOT NULL` filter is dropped; the finish matrix asserts persisted DB state rather than output.
- **The sandbox refactor widened nothing** — byte-for-byte grant-set equivalence against `5ed3b6a`.
- **No secret leaks** in any new `--json` payload, walked field by field: `worktreeProvisionResult`, `worktreeAdoptResult`, `agentAdoptResult`/`agentFinishResult`, and the pairing-code JSON.
- **Daemon socket auth is sound** — peer-credential check fail-closed, root correctly not exempt, Origin/Referer/Host guards, 1 MiB body cap.
- **Migrations `00029`–`00032`** are well-reasoned, including `00031`'s down-migration re-deriving dead rows before rebuilding.
- **Within-process concurrency on the adoption plane** — adopt/adopt, adopt/new, adopt/cleanup all serialize on the per-project repo lock, with the partial unique index as a structural backstop.
- **The refusal matrix** — unborn HEAD, main checkout, bare/`--separate-git-dir`, shallow, orphan branch, `--project` mismatch — gives actionable messages with correct exit classes.

## Not covered by this pass

- **The working-state plane (Layer A gitstate, Layer B WIP refs, `wip gc`).** Its reviewer did not report. The lease/corroboration logic in `wip drop`/`wip gc`, tombstone-vs-late-push resurrection, and `wip apply`'s deliberate lack of a dirty-tree gate are **unaudited** and should head Pass 9. Note the plane was live-dogfooded on 2026-07-31 (`spec/18`), including an attack on the corroboration veto that held — that is evidence, but it is not an audit.
- **The daemon and watcher plane**, whose reviewer's report was outstanding at write-up.
- Commercial/hosted-tier readiness — unchanged since Pass 7, still business-gated.

## Recommended sequencing

1. **`P8-SEC-02` first, alone.** It is a working escape in shipped code with a self-triggering path, and its fix is well-bounded. It should not share a wave with anything.
2. **`P8-SEC-01`** — data-visibility loss on a default-on code path.
3. **`P8-ADOPT-02`/`03`/`04`** as one adoption-hardening slice; they touch adjacent code and share a test fixture.
4. **`P8-ADOPT-06`/`07`** as polish, foldable into (3).
5. **Pass 9 should begin with the WIP plane**, which this pass did not reach.
