---
last_reviewed: 2026-06-28
tracks_code: [cmd/**, internal/**]
---
# Challenge Matrix and Viable Approaches

This document lists the hard problems and practical ways to solve each one.

## 1. Same project structure across machines

Problem: each machine has a different code folder layout.

Recommended solution:

- define a global namespace rooted at `~/Code`;
- every project has one canonical relative path;
- daemon creates skeleton directories for missing projects;
- path changes are namespace events, not ad hoc renames.

Alternatives:

- use a manifest file only: simpler but does not feel like Dropbox;
- raw sync folder: feels good but unsafe for Git/secrets/dependencies;
- virtual filesystem: best UX but too hard for MVP.

## 2. Lazy project materialization

Problem: full cloning every repo on every machine is slow and wasteful.

Recommended MVP solution:

- create skeleton directories;
- `devstrap open` materializes the repo;
- optional shell hook hydrates when entering a skeleton folder;
- optional editor adapter hydrates before opening.

Later solution:

- StrapFS virtual filesystem;
- macOS File Provider or macFUSE/FSKit;
- Linux FUSE.

Important constraint:

- IDE indexers can accidentally trigger hydration of many repos. Lazy-on-access needs hydration thresholds and indexer detection.

## 3. Stale default branch before agent work

Problem: agent worktrees are created from outdated local default branch.

Recommended solution:

- agent/worktree commands never branch from local default branch;
- always `git fetch origin <default_branch> --prune` first;
- resolve `origin/<default_branch>` SHA;
- create worktree from that SHA;
- record `base_ref` and `base_sha`.

Command pattern:

```bash
DEFAULT=$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's@^origin/@@')
git fetch origin "$DEFAULT" --prune
BASE=$(git rev-parse "origin/$DEFAULT")
git worktree add ~/.devstrap/worktrees/repo/task -b agent/task "$BASE"
```

Extra protection:

- before PR, check whether origin/<default_branch> moved;
- warn or auto-rebase;
- never auto-push without showing diff/test result.

## 4. Missing environment variables

Problem: `.env` exists on one device but not another.

Recommended solution:

Two modes:

```text
Personal mode: encrypted env bundles synced by DevStrap.
Team mode: secret-provider references, runtime injection only.
```

Personal commands:

```bash
devstrap env capture work/acme/api .env
devstrap env hydrate work/acme/api --write .env.local
devstrap run work/acme/api -- npm test
```

Team commands:

```bash
devstrap env bind work/acme/api --provider 1password --profile acme-dev
devstrap run work/acme/api -- uv run pytest
```

Never default to plaintext sync.

## 5. Native dependency folders differ by OS

Problem: `node_modules`, `.venv`, binary packages, and build artifacts differ across macOS/Linux/CPU architectures.

Recommended solution:

- never sync dependency folders;
- use lockfiles and bootstrap commands;
- record toolchain profile;
- maintain per-device readiness status.

Examples:

```text
Python: uv sync
Node: npm ci / pnpm install
Rust: cargo fetch/build
Docker: docker compose pull/build
```

## 6. Git repo corruption from raw file sync

Problem: generic sync can conflict `.git/index`, lock files, packs, and refs. This is not theoretical — the Git project's own FAQ states no part of a repository may be live-synced by a file-sync engine; the failure modes include torn `.git/index`, conflict-copied refs (`refs/heads/main 2`), and **`gc`-pruned unreferenced objects = permanent data loss**.

Recommended solution:

- DevStrap never syncs hydrated Git working tree bytes or `.git` internals across devices;
- Git remote is the source of truth;
- DevStrap syncs only namespace metadata and (planned) encrypted draft/working-state bundles;
- worktree state is device-local unless committed/pushed.

**Decision: continuous working-tree file-sync (Mutagen/Syncthing-style) is rejected** — it reintroduces this entire corruption class and violates the fresh-worktree-from-remote invariant. Do not relitigate.

## 6b. "Forgot to push, now on another machine" (cross-machine working state)

Problem: the user's real pain — uncommitted/unpushed work stranded on machine A while they sit at machine B — is the core "Dropbox for code" promise, but must be solved **without** live-syncing `.git`.

Recommended solution — a git-native, three-layer human-convenience plane, strictly walled off from the agent plane (agents always base from `origin/<default_branch>`):

- **Layer A — validation (Phase 0):** signed read-only `repo.gitstate.observed` snapshots (dirty/untracked/unmerged/ahead/behind/stash counts) so every device knows where every other device's tree stands; `status --all-devices`/`doctor` warn and always render snapshot age (never silent all-clear).
- **Layer B — WIP recovery (Phase 1):** `git stash create` → push to `refs/devstrap/wip/<device>/<path_key>` over git's integrity-checked transport (forge-agnostic); machine B `wip apply` on demand, never as a branch/base.
- **Layer C — encrypted bundle (Phase 3):** for non-git/draft folders only, via `draft.snapshot.created` + age encryption.

See `07_NAMESPACE_AND_SYNC_MODEL.md` (working-state plane) and `AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 5.

## 6c. Non-VCS / remote-less / multi-remote projects

Problem: a real `~/Code` is full of folders with no remote (just `git init`), no git at all (scripts, assets, notebooks), or a non-`origin`/multi-remote setup. Today a no-remote repo is mis-adopted as a clonable `git_repo` and breaks hydration on every other device.

Recommended solution:

- classify a remote-less repo as `local_git` (never a clonable `git_repo`); enforce non-empty `remote_key` for `git_repo` in both `add` and `scan --adopt`;
- emit `plain_folder` for structure-only dirs; sync local-only content via the encrypted bundle path (Layer C); add a `promote` command for `plain → draft/local_git → git_repo`;
- preflight `worktree`/`agent` with a clear "requires a remote" error. See `AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 2 (`NOVCS-*`).

## 6d. Non-GitHub forges

Problem: clone/fetch/push are forge-neutral, but `agent pr` is hardcoded to `gh` and fails post-push on GitLab/Bitbucket/Gitea/self-hosted/Azure.

Recommended solution: detect the forge from the `origin` host; route PR/MR creation through a `Forge` interface (`gh`/`glab`/`tea`) with a forge-aware token allowlist; fail gracefully (print branch + compare/MR URL) on unknown forges. See `AUDIT_RECOMMENDATIONS_2026-06-27.md` Section 3 (`FORGE-*`).

## 7. Draft projects not in Git yet

Problem: experiments start as plain folders but still need to appear on other machines.

Recommended solution:

- support `draft_project` type;
- sync small drafts as encrypted file bundles;
- apply size limits and ignore rules;
- show strong nudges to promote to Git.

Draft lifecycle:

```text
draft → local git repo → remote git repo → normal managed repo
```

Limits:

- default max draft size: 100 MB;
- default max file count: 5,000;
- never include ignored paths;
- no automatic sync of private keys or `.env` unless encrypted env capture is used.

## 8. Conflicting paths/remotes

Problem: same repo exists in different places or same path points to different repos.

Recommended solution:

- detect Git remote URLs during scan;
- normalize SSH/HTTPS Git URLs;
- show adoption plan before moving anything;
- use conflict records in DB.

Conflict examples:

```text
same_remote_multiple_paths
same_path_different_remote
delete_vs_dirty_local
rename_vs_rename
```

## 9. Safe deletion

Problem: deleting a project on one machine could wipe work elsewhere.

Recommended solution:

- soft-delete/tombstone namespace entries;
- never delete dirty hydrated repos automatically;
- move local content to quarantine first;
- require explicit purge.

Quarantine path:

```text
~/.devstrap/quarantine/YYYYMMDD/project-name
```

## 10. File watcher reliability

Problem: watchers can miss events during sleep, reboot, overflow, or daemon downtime.

Recommended solution:

- watcher is a hint, not the source of truth;
- maintain periodic reconciliation scan;
- store last scan time;
- compare namespace vs filesystem;
- use event debouncing.

Mac:

- FSEvents for directory hierarchy notifications.

Linux:

- inotify for file/directory events.

## 11. Case sensitivity differences

Problem: macOS default APFS is usually case-insensitive; Linux is case-sensitive.

Recommended solution:

- store canonical normalized path;
- reject sibling paths that differ only by case;
- warn during scan;
- enforce portable path policy by default.

Example forbidden pair:

```text
work/API
work/api
```

## 12. Symlinks and path escapes

Problem: symlinks can point outside managed tree and leak secrets or break sync.

Recommended solution:

- record symlinks explicitly;
- do not follow symlinks during draft sync by default;
- warn if symlink escapes `~/Code`;
- allow explicit trusted symlink rules.

## 13. Large files and model/data artifacts

Problem: ML/data/video artifacts can be huge.

Recommended solution:

- Git LFS for repo-managed large files;
- DVC/object storage for datasets and model artifacts;
- DevStrap ignore rules for local data folders;
- draft sync size cap.

## 14. Offline behavior

Problem: machines may be offline but still need to work.

Recommended solution:

- local DB is authoritative for last known state;
- event log queues local changes;
- hydrated repos remain usable;
- skeletons cannot hydrate without Git/network;
- conflicts resolved on reconnect.

## 15. Device trust

Problem: a new machine should not automatically receive secrets.

Recommended solution:

- device registration;
- per-device key pair;
- explicit approval for env decryption;
- revoke device capability;
- rotate encrypted env bundles.

## 16. Agent access to secrets

Problem: AI agents can read `.env`, shell history, SSH keys, and config files.

Recommended solution:

- scoped runtime env injection;
- file denylist;
- no plaintext `.env` by default;
- redact logs;
- command policy;
- isolated worktree;
- optional container sandbox later.

## 17. Agents leaving messy branches/worktrees

Problem: many agent tasks create scattered branches and worktrees.

Recommended solution:

- DevStrap-owned worktree directory;
- metadata for every agent run;
- status UI;
- cleanup command;
- stale branch detection;
- optional PR creation.

Commands:

```bash
devstrap agent list
devstrap agent cleanup --merged
devstrap worktree list --stale
```

## 18. Editor direct access

Problem: users open skeleton folders directly in Cursor/Finder instead of CLI.

Recommended solution:

- editor command wrappers;
- shell hook;
- skeleton README with hydration instructions;
- future Finder extension or File Provider;
- future virtual filesystem.

## 19. NAS backup

Problem: the user wants reliable backup of code structure/content.

Recommended solution:

- backup `~/.devstrap` state DB and encrypted blob cache;
- backup Git remotes independently;
- do not rely on NAS backup of `node_modules` or `.venv`;
- export namespace snapshot periodically.

Command:

```bash
devstrap export --output ~/Backup/devstrap-snapshot.tar.age
```

## 20. Multi-device sync backend

Problem: every machine needs namespace updates.

Recommended solution:

- local-first event log;
- hub stores events and encrypted blobs;
- devices push/pull cursors;
- no raw Git repo mirroring;
- offline queue.

MVP options:

1. local home-hub on Mac Mini/GMK;
2. small VPS;
3. encrypted object-store backend;
4. hidden Git backend as a temporary adapter, not the user-facing model.

## 21. Security vs convenience

Problem: Dropbox-like convenience tempts unsafe secret syncing.

Recommended solution:

- safe defaults;
- explicit `--allow-plaintext-env` only for local throwaway projects;
- warnings for secret-looking files;
- trust levels per project and device;
- policy profiles.

## 22. Mac packaging and trust prompts

Problem: macOS background services and filesystem integrations can trigger user approval flows.

Recommended solution:

- MVP uses a user LaunchAgent, not system extension;
- distribute through Homebrew first;
- notarize later for broader distribution;
- avoid File Provider/FUSE until needed.

## 23. Linux compatibility

Problem: Mac-first choices can block Linux.

Recommended solution:

- keep core in Go;
- platform adapters for watcher/service/keychain;
- avoid Swift in core;
- use POSIX paths internally but apply platform path policy;
- test on Ubuntu early.

## 24. Cloud/ephemeral agents

Problem: cloud agents need ready repos without full human setup.

Recommended solution:

- `devstrap bootstrap --token ...`;
- short-lived device identity;
- hydrate selected repo only;
- runtime secrets only;
- auto-clean worktree after task;
- emit patch/PR.

## 25. User trust and debuggability

Problem: a daemon that modifies code folders can feel scary.

Recommended solution:

- dry-run mode;
- explain every planned action;
- write audit log;
- never destructive by default;
- quarantine deletes;
- `devstrap doctor` and `devstrap explain <path>`.
