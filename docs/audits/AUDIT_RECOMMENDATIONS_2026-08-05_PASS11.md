# DevStrap — Design & Implementation Audit (Eleventh Pass)

_Date: 2026-08-05 · Trunk audited: `0e66277` (`feat(cli): workspace.yaml export/import, the escape hatch (W13-02) (#286)`)_

## How this relates to the prior audits

This is the **eleventh** design & implementation pass. It does not restate open recommendations from earlier passes; those remain tracked in `docs/audits/README.md`. No P1 is open in any pass, and Pass 7's remaining four rows are the business-gated commercial cluster.

It concentrates on the **three W13 surfaces that shipped on 2026-08-01 and have had no adversarial review at all**:

1. **`devstrap promote`** (`W13-03`, PR #284) — `internal/cli/promote.go`, `internal/git/promote.go`. It runs `git init`/`remote add`/`push` against real repositories, rolls back on failure, and screens the staged index for secret-looking filenames before allowing a push.
2. **`workspace.yaml` export/import** (`W13-02`, PR #286) — `internal/manifest/`, `internal/cli/export.go`, `internal/cli/import.go`. A vcstool-compatible file format, a `--pinned` reachability gate, and a clobber refusal resting on a `state.ErrProjectNotFound` sentinel.
3. **The clone-staging sweeper** (`W13-05`) — `internal/cli/staging_sweeper.go`. Its own work-log entry calls it "a remote-triggered deletion primitive"; this pass asks whether its guards are *sufficient*, not whether they exist.

**ID scheme.** Findings are prefixed `P11-` per the ledger convention. Dimension codes: `PROMOTE`, `MANIFEST`, `SWEEP`.

## Methodology

Following Pass 10's shape rather than open-ended browsing: **five concrete hypotheses were stated up front and chased to a verdict**, each recorded as confirmed, partial, or refuted with its reasoning. A refuted hypothesis is reported in full, so a later pass does not re-walk the ground.

Two companion reviewers (GPT-5.6 via Codex on the tricky logic, Grok-4.5 for adversarial pressure) were run against the same excerpts and told which findings the coordinator already held, so they hunted only novel ones. **Every claim below was verified by the coordinator against the actual code before it counted** — and that discipline paid: the strongest companion finding (a `git init` reusing a pre-existing `.git`'s hooks to execute attacker-controlled code) is **refuted** in this document, because `dsgit.IsRepo` is a bare `.git`-exists check and the guard it feeds therefore already covers the case.

**Two findings were reproduced by executing the defect through the real binary**, not by re-reading the code.

**Severity:** P1 = correctness/security/data-loss; P2 = significant; P3 = minor/polish. **Effort:** S ≈ <½ day, M ≈ 1–3 days, L ≈ ~1 week.

## Executive summary

The three surfaces hold up structurally. The properties each was built to guarantee — that `promote` never runs `git init` over an existing repository, that a failed promotion leaves the working tree at its exact pre-command state, that the emitted manifest is a real vcstool document, that the sweeper cannot be aimed by a peer — were attacked directly and **hold**.

What this pass found is narrower and more specific: **two guarantees that are true in the code and false in the message the user is given.**

The headline (`P11-PROMOTE-01`) is that two of `promote`'s three refusals name `devstrap add` as the remedy, and `devstrap add` refuses a non-empty directory — which is precisely the state those two refusals always leave behind. **This is the same defect that `W13-03`'s own review caught and fixed in the third message of the same file**, along with a test named for the general property. The fix was applied to the one message the review looked at; its two siblings were not checked, and the test's assertion window is 400 bytes wide. Demonstrated by running the exact command the error prints.

The second (`P11-MANIFEST-01`) is that `--pinned`'s reachability gate — added by the same wave's review, for exactly this reason — asks "is this SHA on **a** remote?" when the property it needs is "is this SHA on **the remote this manifest names?**". In a fork workflow (`origin` = your fork, `upstream` = canonical) a HEAD reachable only from `upstream/main` passes the gate, and the manifest pins it against the fork URL. Reproduced end to end through the real binary: the exported `version` is a SHA the exported `url` does not contain, with no warning.

Both are the same shape: **a remedy or a guarantee that was fixed once, at the one call site the review looked at.**

## Findings at a glance

| ID | Sev | Finding | Effort | Status |
|---|---|---|---|---|
| `P11-PROMOTE-01` | P2 | Two `promote` refusals name `devstrap add`, which cannot run in the state they leave | S | Open |
| `P11-MANIFEST-01` | P2 | `--pinned` reachability counts **any** remote, not the one being exported | S | Open |
| `P11-MANIFEST-02` | P3 | The clobber refusal reads outside the transaction it then writes in | S | Open |
| `P11-MANIFEST-03` | P3 | `import` persists `lfs_policy`/`default_branch` that `add` and the git layer validate | S | Open |
| `P11-SWEEP-01` | P3 | The staging sweep rides `wip.gc_interval` and its error is discarded entirely | S | Open |
| `P11-PROMOTE-02` | P3 | `promoteInitRepo` arms its rollback only *after* `InitRepo` returns | S | Open |
| `P11-PROMOTE-03` | P3 | A nested repo becomes an unscreened gitlink; the recovered clone is empty there | S | Open |

## Promote

### `P11-PROMOTE-01` (P2) — a remedy that cannot run in the state it is offered — CONFIRMED BY EXECUTION

`internal/cli/promote.go` prints three remedies that name another DevStrap command. One of them was corrected during `W13-03`'s own review; the other two were not, and both name `devstrap add`:

- `refuseAlreadyRemote` (`promote.go:130`) — "use \`devstrap add --path %s <remote>\` to track a different remote".
- The non-empty-remote refusal (`promote.go:238`) — "use \`devstrap add --path %s %s\` to track an existing repository".

`addProject` calls `ensureHydratableTarget` (`add.go:73` → `hydrate.go:338-352`), which returns `refusing to hydrate into non-empty directory` for any directory that is neither empty nor a skeleton. **Every project reachable by either refusal has a populated directory by definition** — a `local_git` is a repository with commits, and a `git_repo` already tracking a remote is a materialized checkout.

Executed against the built binary, both legs:

```text
$ devstrap promote notes --git-remote /…/upstream.git
/…/upstream.git already has refs; promote pushes into an EMPTY remote only —
use `devstrap add --path notes /…/upstream.git` to track an existing repository

$ devstrap add --path notes /…/upstream.git
refusing to hydrate into non-empty directory: /…/Code/notes
```

```text
$ devstrap promote proj --git-remote /…/fork.git
proj is already git_repo tracking /…/fork.git; promote only graduates remote-less
projects — use `devstrap add --path proj <remote>` to track a different remote

$ devstrap add --path proj /…/upstream.git
refusing to hydrate into non-empty directory: /…/Code/proj
```

**The non-empty-remote leg is the one that matters**, because it is where a *failed* promotion lands. A `git push` can update the remote and then fail locally when the connection drops before the report-status arrives. `promoteToGitRepo` treats every push error as "no remote update occurred", rolls back, and leaves the project unpromoted (`promote.go:311-314`). The user retries; `RemoteIsEmpty` now answers false; they get the refusal above; and the one command it names cannot run. The remote may at that moment hold the only pushed copy of their history.

**Why this recurred is the transferable part.** The review that fixed the third message also wrote `TestPromoteRemediesNameCommandsThatWorkInTheStateTheyAreOffered` (`promote_test.go:459`) — a name that states the general property. The test greps `promote.go` for the literal `"but recording it failed"` and inspects **400 bytes** from that offset. It passes with both defects above present, in the same file, a hundred lines away. A test named for a property while asserting one instance of it reads exactly like a test for the property.

**Fix.** Point both messages at `devstrap scan --adopt`, which is what the corrected third message already names and what actually works on a populated checkout whose origin validates. Widen the test from a keyed 400-byte window to *every* `devstrap add` occurrence in the file, with an explicit allowlist for any that is genuinely offered in an empty-directory state.

### `P11-PROMOTE-02` (P3) — the rollback is armed one statement too late

`promoteInitRepo` (`promote.go:369-374`):

```go
if err := r.InitRepo(ctx, localPath, branch); err != nil {
    return nil, appError{code: exitGit, err: err}
}
rollback := func() { _ = os.RemoveAll(filepath.Join(localPath, ".git")) }
```

`git init` creates `.git` and then populates it; a failure after the `mkdir` (ENOSPC, a permission fault on a subdirectory, an interrupt) returns non-zero with a partial `.git` on disk and no rollback armed. Every subsequent path then refuses: `promote --git-remote` at `if dsgit.IsRepo(localPath)` (`promote.go:274`), `promote --draft` at the same check (`:180`), and after a `scan --adopt` reclassifies the folder `local_git`, `promote --git-remote` again at `HasCommits` (`:264`). **No message names `rm -rf .git`,** which is the only thing that unsticks it. The user's own files are untouched throughout.

**The stronger version of this is refuted.** A companion reviewer argued that `git init` over a *pre-existing* `.git` would reuse its `hooks/`, so `CommitStaged` executes an attacker-planted `pre-commit`, and the rollback then deletes content the command did not create. Verified against the code: `dsgit.IsRepo` (`git.go:1207-1213`) is `dirExists(path/.git) || fileExists(path/.git)` — it does not parse the repository. A folder holding `.git/hooks/pre-commit` therefore *is* `IsRepo`, the `plain_folder`/`draft_project` arm refuses before `promoteInitRepo` is ever called, and `promoteInitRepo`'s comment that "everything below created `.git`" is true. The finding reduces to the ordering above.

**Fix.** Arm the rollback before calling `InitRepo`, or `RemoveAll` on the `InitRepo` error path.

### `P11-PROMOTE-03` (P3) — a nested repository becomes an unscreened gitlink

`StageAll` runs `git add -A`, which records a nested git repository as a single **gitlink** (mode `160000`) rather than descending into it. Confirmed in a scratch tree:

```text
$ git ls-files -s
160000 b4f28308…  0  sub
100644 bf1a1fde…  0  top.txt
```

Two consequences. The secret screen sees `sub`, never `sub/.env` — but nothing leaks, because the gitlink's commit is not in the outer repository's object store and the push does not carry it. The real cost is the mirror image: **the promotion silently loses that content**. The pushed commit references an object the remote will never have, there is no `.gitmodules`, and a device that materializes the project gets an empty `sub/`. Git's own `warning: adding embedded git repository` goes to the subprocess's stderr, which `Runner.Run` does not surface to the user.

Reachability is narrow today: `dsgit.IsRepo` only inspects the top level, so the folder must be a `plain_folder`/`draft_project` containing a nested repo one level down, and `NOVCS-02` (scan-side `plain_folder` emission) is unbuilt, so that type currently only arrives by sync. It widens the day `NOVCS-02` ships.

**Fix.** Detect mode-`160000` entries in `StagedFiles` and refuse, naming them — a promotion that silently drops a subtree is the same class as the empty-folder refusal already in this function.

## Manifest export / import

### `P11-MANIFEST-01` (P2) — `--pinned` proves reachability from the wrong remote — CONFIRMED BY EXECUTION

`resolveExportHead` (`export.go:198`) gates the pin on `RemoteTrackingContains`, which is (`git.go:805-817`):

```go
r.Run(ctx, dir, "branch", "-r", "--contains", sha, "--format=%(refname)")
```

`git branch -r` lists remote-tracking branches for **every** configured remote. The check therefore answers "this SHA is on some remote I have fetched", while the manifest entry written two statements later pairs that SHA with **one specific URL** — `project.RemoteURL`, the registered origin.

The fork workflow separates those. Reproduced end to end against the built binary: a checkout with `origin` = an empty fork and `upstream` = the canonical repository, HEAD on `upstream/main`, adopted by `scan --adopt`, then

```text
$ devstrap export --manifest ws.yaml --pinned
Wrote ws.yaml: 1 project(s), 1 of them git repositories `vcs import` can rebuild
```

```yaml
repositories:
  proj:
    type: git
    url: /…/fork.git
    version: 982465784683674abd59252ea5e47850aff92e24
```

```text
$ git -C fork.git cat-file -e 98246578…
ABSENT from the exported remote
```

No warning was emitted, and the omit-the-version path — the whole point of the gate — never fired. `vcs import` clones the fork and fails its checkout during the actual recovery, which is the failure `W13-03`'s review added `RemoteTrackingContains` to prevent.

**The work log's stated safety argument is one-directional and the property is not.** It records that "a stale remote-tracking ref answers *no*, which is the safe direction". Remote-tracking refs are a local cache and can be stale in *both* directions: after an upstream force-push or branch deletion with no intervening fetch, `--contains` still answers **yes** for an object the remote no longer serves. The fork case is the same gap without needing any staleness at all.

**Fix.** Scope the check to the remote being exported — resolve the remote name whose URL matches `project.RemoteURL` and pass `--list '<remote>/*'` — so the question asked is the question the manifest answers. `export.go`'s own comment already states the correct standard: *"the file never claims a pin it does not have."*

### `P11-MANIFEST-02` (P3) — the clobber refusal reads outside the transaction it writes in

`registerManifestEntry` (`import.go:264-310`) reads with `store.ProjectByPath` and, on `ErrProjectNotFound`, opens a *separate* `store.WithTx` that calls `tx.UpsertProject`. `UpsertProject` is `ON CONFLICT(workspace_id, path_key) DO UPDATE` (`store.go:1267`) and overwrites `remote_url`/`remote_key` — the exact write the refusal above it exists to prevent.

`W13-02`'s review closed the *error* leg of this (a transient read failure no longer reads as "absent"; that is what the `state.ErrProjectNotFound` sentinel is for). The *concurrency* leg is still open: any writer that creates the row between the read and the transaction is silently overwritten. The realistic writer is the supported configuration — `service install --daemon` converging in the background while the user runs `devstrap import`, with a peer's `project.added` for the same path applying in the window.

Checked and clean on the adjacent axis: **case is not a bypass.** `ProjectByPath` resolves through `pathkey.Clean` and queries `n.path_key` (`store.go:1684`, `:1722`), which is the same case-folded column `UpsertProject`'s conflict target uses, so two spellings that collide on insert also collide on lookup.

**Fix.** One line: move the lookup inside the transaction using the already-existing `Tx.ProjectByPath` (`store.go:1703`).

### `P11-MANIFEST-03` (P3) — `import` persists fields it does not validate

`resolveManifestEntry` copies `project.LFSPolicy` and `project.DefaultBranch` verbatim (`import.go:206-209`) and `registerManifestEntry` writes them. Neither is validated, and both have validators the sibling command applies:

- `validLFSPolicy` (`worktree.go:689`) has exactly **two** references in the tree: its definition and `add.go:69`. There is no CHECK constraint on `git_repos.lfs_policy` either (`00001_initial.sql:49`). A manifest carrying `lfs_policy: alwyas` imports and reports success; `applyMaterializeLFSPolicy` then fails that project with `unsupported lfs_policy` (`hydrate.go:263`) — but only at materialize time, and only for a repository that actually uses LFS.
- An option-shaped `default_branch` is **not** an injection: `ResolveDefaultBranch` gates the stored fallback through `safeBranchName` before any git invocation (`git.go:663-665`), and `safeBranchName` rejects a leading `-`. It fails closed with `invalid fallback branch %q`. The cost is the same deferred failure, not a compromise.

The pattern is what matters: import is a **boundary** — the manifest is a hand-editable plain-text file, and after a total local loss it is the only input. Deferring validation to materialize moves the error to the worst possible moment.

**Fix.** Validate both in `resolveManifestEntry` with the existing validators, warn, and skip the entry — the batch semantics `ErrPartialImport` already defines.

## Staging sweeper

### `P11-SWEEP-01` (P3) — the disk-growth sweep rides an unrelated config key, and its error is discarded

`maybeSweepStagingOrphansAfterSync` (`staging_sweeper.go:181`) opens with `interval, err := wipGCInterval(opts)` and returns immediately when the interval is `0`. Its sole caller discards both return values (`sync.go:270`):

```go
_, _ = maybeSweepStagingOrphansAfterSync(ctx, stderr, opts, store, time.Now())
```

Two consequences.

**`wip.gc_interval: 0` disables the staging sweep.** `spec/13:314` documents that key as *"automatic post-materialization **WIP** sweep cadence; default 24h; 0 disables"*, and `spec/07:16` likewise scopes it to WIP GC. A user who turns off WIP-ref GC — a recovery-plane feature — silently turns off the clone-staging orphan sweep, restoring the unbounded disk growth `W13-05` exists to end. (`spec/11` does say the sweep runs "on the WIP-GC interval cadence"; the config reference where a user actually sets the value does not.)

**An invalid value disables it with no output at all.** `parseWipDuration` rejects `"30d"` (Go has no day unit — the natural typo, and the one that already caused `P9-WIP-05`). The sweep returns that error before printing anything, the caller drops it, and the sweep never runs again. An error *is* visible on that cycle, but it comes from `maybeGCWipRefsAfterSync` further down and names WIP GC — so the operator fixes a WIP-GC message with no reason to suspect the orphan sweeper stopped too.

**This is the third appearance of one shape.** `P9-WIP-05` was recorded as *"two unrelated subsystems shared a failure mode because one ran first"*, and `sync.go:279-286` still carries the comment explaining why the durability export was decoupled from it. `W13-05` then attached a third subsystem to the same key, in code written after that fix shipped.

**Fix.** Give the sweep its own `staging.sweep_interval` (defaulting to the WIP-GC interval is fine as a default; sharing the key is not), and surface its error as a warning the way its own body already does for every other failure.

## Hypotheses chased and refuted

Recorded in full so no later pass re-walks them.

1. **A push that fails mid-transfer leaves a partial ref update the rollback cannot undo.** Refuted at the mechanism. `PushBranch` is `git push -u <remote> <branch>` (`git.go:304-309`) — no `--force`, one ref. A server-side ref update is atomic, and a concurrent pusher that advanced the same branch causes a non-fast-forward *rejection*, so the local rollback is correct. **The residual is the ambiguous case, not a partial one**: the remote may be updated while the client reports failure. Every branch of the rollback restores the working tree (the `local_git` arm removes only the `origin` it added; the init arm removes only the `.git` it created; the user's own files are never touched in either), so there is no local data loss — the cost is the stranded state and the unusable remedy, which is `P11-PROMOTE-01`.

2. **The `RemoteIsEmpty`-vs-push TOCTOU is exploitable by a concurrent pusher.** Real but **not novel**: `W13-03` documented it verbatim, and `spec/07` records the emptiness check as *best effort* because the check and the push are not atomic. Consequence bounded: a racer pushing the same branch causes a rejection and a clean rollback; a racer pushing a *different* branch lets both succeed and leaves two unrelated histories in one remote, which is the outcome the check exists to make unlikely rather than impossible. DevStrap's own state stays consistent either way. Not counted as a Pass-11 finding.

3. **The staged-index secret screen can be evaded by an adversarial `.gitignore`.** Refuted, and the reason is structural rather than incidental. The screen reads `git ls-files -z --cached` — *the index itself*. An ignore rule cannot hide a staged path from that listing, and `git add -A` cannot stage an ignored one, so the screen's view is exactly the set of paths the commit will contain. A missing or malformed `.gitignore` only widens what is staged, which widens what is screened. The one adjacent gap is the gitlink (`P11-PROMOTE-03`), which is a *listing granularity* issue, not an ignore-rule one.

4. **A crafted `url`/`version` pair reaches `vcs import` unsafely.** Refuted on all three axes checked. Namespace paths pass `pathkey.Clean` before any write (`import.go:141`), which rejects traversal and unsafe components. URLs pass `dsgit.CanonicalRemoteKey` (`import.go:226`), the same protocol-allowlist gate `add` and `scan` use, and a `file://` remote is subject to that one policy rather than a second. An option-shaped `version` adopted as `default_branch` is refused by `safeBranchName` inside `ResolveDefaultBranch` before it can reach a git argument list. What survives is the *deferred*-validation cost, recorded as `P11-MANIFEST-03`.

5. **The sweeper races a second DevStrap process starting a genuinely new staging clone between its `Lstat` and its `Root.RemoveAll`.** Refuted, for three independent reasons — any one of which is sufficient, which is the useful part:
   - **Names are unique by construction.** A new clone's directory comes from `os.MkdirTemp` (`hydrate.go:310`), which creates exclusively with fresh randomness. The path the sweeper resolved and is about to remove can never be the path a concurrent clone just created, so the window contains no shared object.
   - **The clone takes the lock before it makes the directory.** `hydrate` acquires the repo lock at `:126` and only reaches `MkdirTemp` at `:310`, so a live clone is always visible to the sweeper's try-lock — there is no "directory exists but lock not yet held" window to exploit.
   - **A long clone cannot have its lock aged out.** `repoLockIsStale` treats same-host PID liveness as authoritative over the age window (`repo_lock.go:170-176`), so a 45-minute clone keeps its lock.

   **One residual is recorded rather than filed.** When the lock was written by a *different* hostname — a shared or network-mounted `~/.devstrap` — staleness falls back to a flat 30-minute age, which is exactly `LongTimeout` for a clone. A cross-host clone running past that can have its lock broken and its staging directory swept. This is a pre-existing lock-layer property, but `W13-05` is the first caller to make a **destructive** action depend on it, and that change of stakes is worth stating.

## Checked and found sound

Recorded so the next pass does not repeat the work.

- **`promote` never initializes over an existing repository.** The `local_git` arm cannot reach `InitRepo` (`internal/git/promote.go` exposes no such primitive to it), and both non-git arms refuse when `IsRepo` is true. The type switch is genuinely the only place the distinction lives.
- **The event-design argument behind reusing `project.updated` holds.** `decideUpsert`'s conflict branch requires a non-empty *existing* `RemoteKey`; every legitimate promotion source has none; `refuseAlreadyRemote` closes the one case that would reach it. The testscript's `conflicts list` assertion is mutation-checked and is the only thing in the tree that would catch an edit to that guard.
- **The repo lock covers the whole git-mutating stretch of `promote`,** the same lock `hydrate`/`materialize`/`worktree` take.
- **The manifest carries no secrets.** `env_profile` is a boolean marker; URLs pass `redact.StripURLUserinfo`; the file is written `0600` through a temp-file-and-rename.
- **The sweeper cannot be aimed by a peer.** Two independent guards (reserved basename at validation, plus the registered-row skip and its strict-descendant extension) and the `os.Root`-scoped removal, which bounds the worst outcome of a symlink swap to an error rather than a deletion outside the managed root.
- **`registeredUnder` is case-sensitive** while the store's own path uniqueness is case-folded — but both sides of the comparison derive from the same recorded spellings rather than from independent sources, and the population it guards is legacy-only. Thin enough not to file; noted in case a later change introduces a second source.
- **The 2-minute default git timeout applies to `git add -A`/`ls-files`/`commit`** in the init arm, so a sufficiently large folder promotion has a hard wall. **Not measured**, and it is the tree-wide default rather than anything specific to `promote`, so it is recorded as a bound rather than claimed as a finding.

## Recommended sequencing

1. **`P11-PROMOTE-01` and `P11-MANIFEST-01` first**, separately. Both are single-call-site corrections whose real cost is the missing test — each needs a test that would fail across *every* instance of its property, not the one instance a review looked at.
2. **`P11-MANIFEST-02`/`03`** as one import-hardening slice; they touch adjacent lines and share a fixture.
3. **`P11-SWEEP-01`** — small, and it retires a shape that has now recurred three times.
4. **`P11-PROMOTE-02`/`03`** as polish, foldable into (1).
