---
last_reviewed: 2026-06-26
tracks_code: [cmd/**, internal/**, README.md]
---
# Product Requirements

## Product name

The naming decision is RECORDED and BINDING in `spec/adr/0001-product-naming.md`:

```text
Product: DevStrap
Core concept: Workspace Passport
Future virtual filesystem: StrapFS
```

This is no longer a non-binding recommendation. The Go module, binary, and all code and user-facing strings use only "DevStrap". "Workspace Passport" is the core-concept tagline (the portable, managed code namespace), never a product or binary name; "StrapFS" is reserved for the future optional virtual filesystem layer.

## Problem statement

Developers with multiple machines and AI agents lose time because their code workspace is inconsistent across devices.

Common failure modes:

- a repo exists on one machine but not another;
- projects are organized differently on each machine;
- agents run from stale local default branches;
- worktrees are forgotten or scattered;
- environment variables exist on one device but not another;
- local generated folders create huge sync noise;
- experimental folders are not in Git yet but still need to appear elsewhere;
- cloud/agent machines require repetitive setup;
- opening a project does not guarantee it is ready to run.

## Product thesis

DevStrap should make `~/Code` behave like a Dropbox-style shared namespace while using developer-native primitives underneath.

```text
Structure sync      → DevStrap namespace
Repo content        → Git
Lazy clone/fetch    → Git partial clone/sparse checkout
Worktrees           → git worktree
Secrets             → vault references or encrypted personal env bundles
Dependencies        → recreated locally
Draft folders       → encrypted DevStrap draft sync
Large assets        → Git LFS/DVC/object store
Agent tasks         → isolated fresh worktrees
```

## Target users

### Primary persona: multi-machine AI-heavy developer

Uses:

- Mac Mini / MacBook / Linux box / cloud machine;
- Cursor, VS Code, Codex/Copilot/Cursor/Claude Code agents;
- multiple repos and worktrees;
- many `.env` files;
- local home-lab or cloud runners.

Needs:

- same folder layout everywhere;
- open project anywhere;
- env available everywhere;
- fresh agent worktrees;
- no stale base mistakes;
- minimal manual setup.

### Secondary persona: small engineering team

Needs:

- consistent onboarding;
- safe env distribution;
- approved repo catalog;
- agent workspace governance;
- audit trail.

### Tertiary persona: power user with many draft projects

Needs:

- new folders appear on every machine before they are pushed to Git;
- lightweight encrypted sync for small experiments;
- easy promotion from draft to Git repo.

## Core jobs-to-be-done

### JTBD 1 — New machine bootstrap

When I get a new Mac or Linux box, I want to run one command and have my full code namespace appear.

Acceptance:

- `~/Code` tree is created;
- known projects appear as skeletons or hydrated repos;
- device is registered;
- secrets provider is connected;
- missing prerequisites are reported.

### JTBD 2 — Open project anywhere

When I open a project on a different machine, I want DevStrap to materialize it in the correct path with the right env and tools.

Acceptance:

- missing repo is cloned;
- remote is fetched;
- branch is checked correctly;
- env profile is ready;
- bootstrap runs if needed;
- editor opens in the right folder.

### JTBD 3 — Start agent safely

When I ask an agent to work on a repo, I want it to start from the latest upstream default branch in an isolated workspace.

Acceptance:

- remote branch fetched;
- worktree created from `origin/<default_branch>` or configured upstream;
- new branch named predictably;
- env injected with minimal scope;
- command/file policy applied;
- output summarized.

### JTBD 4 — Capture and distribute env safely

When I configure env variables on one machine, I want other machines to have them without copying plaintext `.env` files casually.

Acceptance:

- env variables captured into encrypted store or mapped to a vault;
- sensitive values are redacted in logs;
- other devices can hydrate or runtime-inject values;
- plaintext files are optional and explicit.

### JTBD 5 — Adopt messy existing code folders

When I already have code scattered across machines, I want DevStrap to scan, deduplicate, and normalize paths.

Acceptance:

- Git remotes detected;
- duplicate clones detected;
- canonical path proposed;
- conflicts reported;
- adoption can be staged.

## Non-goals for MVP

Do not build these first:

- full custom filesystem;
- Dropbox-like byte sync for all files;
- team admin UI;
- hosted SaaS billing;
- full secret manager replacement;
- full package manager;
- Windows support;
- production-grade sandboxing of arbitrary agent commands.

## MVP feature set

### Must have

- `devstrap init ~/Code`
- `devstrap scan ~/Code --adopt`
- namespace entries for Git repos and draft projects;
- skeleton directory creation;
- repo hydration using Git;
- safe fetch/pull behavior;
- fresh worktree creation from remote branch;
- env capture/hydrate with encrypted local store;
- device registration;
- local SQLite state;
- Mac LaunchAgent daemon;
- Linux-compatible abstractions;
- basic status and doctor commands.

### Should have

- Cursor and VS Code open adapters;
- 1Password / Apple Password adapter;
- Doppler adapter;
- GitHub CLI integration for PR creation;
- shell `cd` hydration hook;
- universal ignore compiler;
- TUI status view.

### Could have

- DevPod/Coder target adapters;
- cloud agent runner support;
- encrypted draft-project sync;
- macOS menu bar helper;
- File Provider experiment;
- FUSE/StrapFS experiment.

## Product invariants

1. Same project path everywhere.
2. Git repos are never raw-file-synced as normal folders.
3. Agents never use stale local default branch as a base.
4. Local changes are never overwritten silently.
5. Env secrets are never logged.
6. Generated dependencies are recreated, not synced.
7. Each project has a known readiness state per device.
8. Draft folders can exist before Git but must have size and ignore limits.
9. Platform-specific behavior is isolated behind adapters.
10. User can always inspect what DevStrap plans to do.

## Readiness states

Authoritative readiness is a tuple, not a single stored enum. See `07_NAMESPACE_AND_SYNC_MODEL.md` for the canonical state machine.

```text
materialization_state: skeleton | hydrating | available | failed
dirty_state:           unknown | clean | dirty | ahead | behind | diverged | conflicted
env_ready:             true | false
tooling_ready:         true | false
```

User-facing display status is derived:

```text
unknown      not registered
skeleton     known path, not hydrated
hydrating    clone/fetch/bootstrap in progress
available    code exists locally
current      fetched and branch is clean/current
ready        env/tools/bootstrap validated
dirty        local changes exist or branch diverged
conflicted   local/remote or namespace conflict needs resolution
failed       last job failed
```

## Success metrics

For personal/MVP use:

- new machine from zero to visible `~/Code` tree in under 5 minutes;
- materialize average repo in under 2 minutes, excluding dependency install;
- zero stale-default-branch agent branches in normal DevStrap flow;
- zero plaintext secret exposure in logs;
- all project paths consistent across registered devices;
- 90% of repo openings require no manual terminal setup after initial adoption.

For future product use:

- number of registered repos;
- number of active devices;
- number of successful hydrations;
- number of stale-worktree preventions;
- agent tasks run through DevStrap;
- env/profile errors caught before runtime.

## Future web/admin surface requirements

The MVP does not include a team admin UI or hosted SaaS console. If/when DevStrap adds a web surface for Hub administration, team policy, or billing, it must follow current web-product guardrails:

- server-first rendering by default, with minimal client JavaScript and explicit performance budgets;
- Core Web Vitals targets: INP <= 200 ms, LCP <= 2.5 s, CLS <= 0.1 at p75;
- WCAG 2.2 AA accessibility baseline, including keyboard navigation, visible focus, and non-color-only status;
- OWASP Top 10 controls, secure headers/CSP, short-lived tokens, dependency scanning, and SAST/SCA in CI;
- API-first boundaries with stateless request handling, no sticky sessions, and horizontal scaling assumptions;
- OpenTelemetry-compatible traces/metrics/logs, SLOs, error budgets, and rollback strategy before production launch;
- no telemetry by default; opt-in product analytics only, with clear metadata disclosure.
