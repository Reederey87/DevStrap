---
status: accepted
date: 2026-06-27
---

# 0002 — Cross-machine working-state sync ("forgot to push")

## Context

The product promise is "Dropbox for code." The sharpest user need behind it is: *"I forgot to push, I'm now on another machine, and my uncommitted work is stranded."* A natural first instinct is literal file-sync of the working tree (Dropbox/OneDrive/iCloud/Syncthing/Mutagen style). This was researched and pressure-tested (four competing designs, adversarial scoring; see `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 5).

The Git project's own FAQ is categorical: no part of a repository may be live-synced by a file-sync engine. The failure class includes torn `.git/index`, conflict-copied refs (`refs/heads/main 2`), `index.lock` contention, and — worst — divergent refs that leave objects unreferenced and **permanently `gc`-pruned (data loss)**. File-sync also has no principled merge for two concurrently-edited dirty worktrees and directly fights DevStrap's fresh-worktree-from-remote invariant.

## Decision

**Reject continuous/Dropbox-style working-tree file-sync.** Serve the need git-natively with a three-layer **human-convenience plane**, strictly separated from the agent plane (agents always base from `origin/<default_branch>`; the resolver must never read `refs/devstrap/wip/*`):

- **Layer A — validation (Phase 0):** signed read-only `repo.gitstate.observed` snapshots (branch/HEAD/upstream + dirty/untracked/unmerged/ahead/behind/stash counts), captured with `git --no-optional-locks status --porcelain=v2 --branch` (never writes `.git/index`). Mirror-only apply into a sidecar `device_gitstate` table (opaque `device_id`, no FK to `devices`). `status --all-devices`/`doctor` warn and **always render snapshot age** (never silent all-clear).
- **Layer B — WIP recovery (Phase 1):** `git stash create` (no worktree/index mutation) → `git push origin <sha>:refs/devstrap/wip/<device_id>/<path_key>` over git's integrity-checked, forge-agnostic transport → `repo.wip.pushed`. Machine B `wip apply` materializes on explicit command only, never as a branch/base.
- **Layer C — encrypted bundle (Phase 3, narrow):** for `draft_project`/`local_git`/untracked-only where there is no remote ref to push to, via `draft.snapshot.created` + `internal/envbundle` age encryption.

## Consequences

- New events `repo.gitstate.observed` and `repo.wip.pushed`; planned `device_gitstate` sidecar table (future migration `00008_gitstate_mirror.sql`, not present as of 2026-06-28); new `gitstate`/`wip` CLI commands; reserved `refs/devstrap/wip/*` namespace.
- A test must assert the fresh-worktree resolver refuses any `refs/devstrap/wip/*` base.
- Layer A alone is a "smoke detector, not fire brigade" — it must ship together with (or quickly before) Layer B, and must surface staleness explicitly.
- Remote snapshots are attributed-but-unverified until Phase-2 device enrollment; flag clearly.
- `04_CHALLENGE_MATRIX.md` records the file-sync rejection so it is not relitigated; `07_NAMESPACE_AND_SYNC_MODEL.md` documents the plane and events.
