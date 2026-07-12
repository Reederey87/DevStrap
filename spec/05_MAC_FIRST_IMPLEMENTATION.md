---
last_reviewed: 2026-07-11
tracks_code: [internal/platform/**, internal/cli/open.go, internal/cli/hydrate.go, .github/**]
---
# Mac-First Implementation Guide

## Goal

Build a Mac solution that feels native enough to solve the daily pain, while keeping the core portable to Linux.

## Sequencing note (2026-06-28): cross-platform core first

The 2026-06-28 cloud-sync decisions (see `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`, workstream `XP-*`) re-order this guide's build sequence: ship the **portable Go core first on both macOS and Ubuntu**, before any native macOS magic. The "Dropbox experience for code" — one identical `~/Code` tree on every device in a mixed macOS/Linux fleet (workstations, laptops, headless boxes, agent runners) — is delivered this cycle by the portable core (eager blobless clone on `devstrap sync`, age-encrypted env/draft blobs, and the signed HLC-ordered namespace map), not by a daemon or virtual filesystem.

Consequently, treat the daemon, native FSEvents watcher, LaunchAgent, Endpoint Security, File Provider, and FUSE/StrapFS content below as **later layers, not this-cycle work**. The Mac-specific adapter seams in `internal/platform` stay valuable as the eventual home for that behavior and as the proof that Mac specifics stay behind adapters so Ubuntu remains first-class — but they are deferred. Materialization in the cross-platform core is **eager clone-everything on `devstrap sync`** (partial/blobless clone up front); there is no placeholder/lazy-VFS step in this design.

## Recommended Mac MVP

```text
CLI:        /opt/homebrew/bin/devstrap
Daemon:     ~/Library/LaunchAgents/com.devstrap.devstrapd.plist
State:      ~/.devstrap/state.db
Socket:     ~/.devstrap/devstrapd.sock
Managed:    ~/Code
Watcher:    fsnotify/kqueue now; native FSEvents target
Secrets:    macOS Keychain + external CLI providers
```

## Mac service model

Use a **LaunchAgent**, not a LaunchDaemon, for the first version.

Why LaunchAgent:

- runs as the logged-in user;
- has access to user home directory;
- avoids root-level install;
- safer for `~/Code` management;
- easier Homebrew install/uninstall story.

LaunchDaemon is only needed later if you need system-wide service behavior before login.

## LaunchAgent plist — shipped via `devstrap service install` (`P4-PROD-04`)

The **native daemon (`devstrapd serve`) stays deferred**, but the LaunchAgent installer is shipped: `devstrap service install` renders and installs a per-user LaunchAgent that wraps the portable `run-loop`, so the workspace converges unattended without the Phase 1 daemon. The rendered plist:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.devstrap.run-loop</string>

  <key>ProgramArguments</key>
  <array>
    <string>{{ .ExecPath }}</string>
    <string>run-loop</string>
    <string>--interval</string>
    <string>5m0s</string>
  </array>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>

  <key>ThrottleInterval</key>
  <integer>30</integer>

  <key>StandardOutPath</key>
  <string>{{ .Home }}/.devstrap/logs/run-loop.out.log</string>

  <key>StandardErrorPath</key>
  <string>{{ .Home }}/.devstrap/logs/run-loop.err.log</string>
</dict>
</plist>
```

Shipped commands:

```bash
devstrap service install     # renders the plist, then bootstraps it
devstrap service status      # installed / running / detail / unit (also --json)
devstrap service uninstall   # bootout + remove the plist (idempotent)
```

The adapter renders the plist with Go `text/template` (every value XML-escaped through `encoding/xml.EscapeText`) using `os.UserHomeDir()` and `os.Executable()` (symlinks resolved by default — see the stable-dir exception below), and writes it atomically at mode `0600`. It manages the service with the **modern per-domain verbs** — `launchctl bootstrap gui/<uid> <plist>` and `launchctl bootout gui/<uid>/<label>` (a best-effort `bootout` precedes `bootstrap` so a reinstall is idempotent; because `bootout` tears the old job down asynchronously the adapter then polls `launchctl print` until the label leaves the domain, so reinstalling over a *running* service does not race into an EIO `Bootstrap failed: 5` — caught in live dogfood) and `launchctl print` for status — never the deprecated `load`/`unload`. `ExecPath` is refused when it resolves to an ephemeral `$TMPDIR`/`go-build` path (install `devstrap` to a stable location or pass `--exec-path <abs>`); when the invoked path sits in a stable install bin dir (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, Linuxbrew's `bin`, or a keg-only/versioned formula's `<brew prefix>/opt/<formula>/bin`) the symlink itself is baked unresolved so `brew upgrade` moving the Cellar target cannot brick the LaunchAgent, and a path that still resolves into a `Cellar/` segment is refused (`P7-XP-01`); `Status` best-effort parses `ProgramArguments[0]` from the plist and reports `ExecPath missing: <path>` when the baked binary is gone, with a matching `doctor` warning (`P7-XP-05`); `PATH` is seeded to `<execdir>:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`. Do not hardcode `/Users/USER`, `~`, or Homebrew paths; launchd does not expand them in plist fields.

Troubleshooting (`launchctl print` surfaces `last exit code = N`):

- **exit 78** (`EX_CONFIG`) — the plist is malformed or references a missing path. Re-run `devstrap service install`; it rewrites and re-bootstraps the plist atomically.
- **exit 127** — the service could not find the `devstrap` binary or a sibling tool (`git`) on the seeded `PATH`. Install `devstrap` to a stable directory and re-run `devstrap service install` so `ExecPath`/`PATH` point at it. `service status` and `doctor` now name this case directly (`ExecPath missing: <path>`, `P7-XP-05`).

The deferred native daemon (`devstrapd serve`) would install its own `com.devstrap.devstrapd` LaunchAgent later and run in the foreground under launchd; `devstrap service` targets the shipped `run-loop` today.

## Filesystem watcher

Use a Go watcher abstraction for MVP. The current Darwin adapter uses fsnotify/kqueue and debounces bursts into reconciliation hints. Prefer a native FSEvents-backed Mac adapter later when reliable recursive tree semantics matter. `fsnotify` is useful as the current cross-platform adapter and already supports Linux inotify, but its macOS backend is kqueue rather than FSEvents, so the spec must not rely on fsnotify alone for FSEvents behavior.

Important implementation rule:

```text
Watch events are hints, not truth.
```

Why:

- events can be coalesced;
- daemon may be stopped;
- machine may sleep;
- folders can be moved by external tools;
- editor behavior can create bursts of temporary files.

Therefore:

```text
Watcher event → enqueue reconciliation job
Periodic scan → validate actual state
```

Watcher events are debounced and batched before enqueueing reconciliation. The current fsnotify adapter defaults to a 250 ms debounce with a 2 s maximum latency and skips `.git`, `node_modules`, `.devstrap`, and `vendor` trees. The ignore compiler should feed richer watcher exclusions later so `.venv`, `dist`, `build`, and other generated trees do not exhaust watcher budgets or trigger hydration storms.

## Reconciler behavior

Every reconciliation checks:

- namespace entry exists but folder missing → recreate skeleton;
- folder exists but namespace missing → classify as new project;
- Git repo found → detect remote/default branch;
- placeholder opened/hydrated → update materialization state;
- local dirty repo → mark dirty, do not modify;
- ignored folders → skip.

## Skeleton directory design

A skeleton project should be safe and obvious.

Example:

```text
~/Code/work/acme/api/
  .devstrap/
    placeholder.json
  README.devstrap.md
```

`placeholder.json` (shipped on-disk format, written by `writeSkeleton` in `internal/cli/hydrate.go`):

```json
{
  "path": "work/acme/api",
  "remote": "git@github.com:acme/api.git",
  "state": "skeleton"
}
```

The richer `{version, type, default_branch, materialization}` schema is a **planned** extension, not the current on-disk format — any tooling (e.g. the zsh `chpwd` hook below) must parse only the three shipped fields today.

`README.devstrap.md` (shipped text, written verbatim by `writeSkeleton`):

```markdown
# DevStrap skeleton

This directory maps to `work/acme/api` and will be hydrated from `git@github.com:acme/api.git`.
```

## Shell integration

Add optional zsh hook:

```bash
_devstrap_auto_hydrate_cd() {
  if [ -f ".devstrap/placeholder.json" ]; then
    command devstrap hydrate .
  fi
}

autoload -Uz add-zsh-hook
add-zsh-hook chpwd _devstrap_auto_hydrate_cd
```

Keep this optional. Some users will not want `cd` to trigger network operations.

## Editor integration

MVP wrappers:

```bash
devstrap open work/acme/api --cursor
devstrap open work/acme/api --vscode
```

Implementation:

1. resolve namespace path;
2. hydrate if skeleton;
3. verify env/tooling;
4. run editor command:

```bash
cursor ~/Code/work/acme/api
code ~/Code/work/acme/api
```

Future:

- Cursor extension;
- VS Code extension;
- Finder Quick Action;
- menu bar app.

## Mac secrets storage

For device identity and personal encryption keys:

- target: store device private key in macOS Keychain;
- current CLI foundation: store private age and Ed25519 signing identities through the platform keychain adapter, using macOS Keychain when available and `~/.devstrap/keys` with mode `0600` as a fallback, while persisting only public keys in SQLite; the keychain-vs-file choice is a typed, recorded custody decision made once at init and honored thereafter, and the mint paths never generate a divergent identity over an already-published key or an unreachable keychain (`P6-XP-04`, see `spec/09`);
- store encrypted env bundles in Hub/local cache;
- decrypt only on approved device;
- never log secret values.

External vault adapters:

- 1Password CLI;
- Doppler CLI;
- Infisical CLI.

## macOS path policy

Default macOS filesystems are often case-insensitive. Linux is usually case-sensitive.

Policy:

- store canonical lowercase comparison key;
- reject paths that differ only by case;
- normalize Unicode path forms if needed;
- avoid `:` and other problematic characters;
- warn for spaces if desired but do not forbid them.

## Avoid Endpoint Security for MVP

Endpoint Security is powerful, but it requires deeper macOS security entitlements and is unnecessary for MVP.

Use:

```text
native FSEvents or fsnotify/kqueue + periodic reconciliation + shell/editor hooks
```

Only consider Endpoint Security later if you need low-level process/file access monitoring for enterprise-grade agent policy enforcement.

## Avoid File Provider for MVP

File Provider is relevant for Finder-integrated file-on-demand behavior, but it should not be the first implementation.

Reasons:

- requires Mac app/extension architecture;
- better suited to cloud-file-provider semantics;
- more difficult to map to Git-aware repo hydration;
- not needed to solve stale default branch, env, worktree, and path problems.

Possible later use:

- Finder-native skeletons;
- cloud-style status icons;
- hydrate-on-open behavior;
- user-facing polished Mac app.

## Avoid FUSE/macFUSE for MVP

FUSE is attractive for true lazy materialization, but it is high-risk early.

Reasons:

- user installation friction;
- editor/indexer performance concerns;
- cache invalidation complexity;
- file locking and rename semantics;
- hard-to-debug support issues.

Possible later use:

- StrapFS virtual namespace;
- true lazy file access;
- read-only skeleton mode;
- advanced cloud/agent workspace mounts.

## Packaging

MVP developer install:

```bash
brew tap yourname/devstrap
brew install devstrap
```

Or direct install:

```bash
curl -fsSL https://devstrap.dev/install.sh | sh
```

Production distribution should include:

- signed binary;
- notarized package/app if distributed broadly;
- uninstall command;
- LaunchAgent management;
- auto-update strategy.

## Mac MVP acceptance criteria

- `devstrap init ~/Code` creates state, config, and managed root. (shipped)
- LaunchAgent keeps daemon running after login. (deferred — daemon layer, not shipped)
- Daemon recreates skeleton folders from namespace state. (deferred — daemon layer, not shipped)
- Scanner adopts existing Git repos. (shipped)
- `devstrap open <path> --cursor` hydrates and opens repo. (shipped)
- `devstrap worktree new <path> --fresh-upstream` fetches origin and creates worktree from remote SHA. (shipped)
- Env capture/hydrate now stores and restores encrypted local blobs, provider ref hydration delegates to `op inject`, and runtime injection delegates encrypted profiles or 1Password refs through `devstrap run`. (shipped)
- Dirty repos are detected and not overwritten. (shipped)
- Logs are readable under `~/.devstrap/logs`. (shipped)

## Audit follow-ups (2026-06-27)

Platform findings (`PLAT-*`, from `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-27.md`):

- **Watcher exclusion diverges from the scanner prune list (`PLAT-01`):** the fsnotify watcher would recursively register watches inside `.venv`/`dist`/`build`/`target`/`__pycache__`. Unify on the single `spec/11` ignore compiler.
- **No ENOSPC/EMFILE handling (`PLAT-02`):** the watcher treats every Add/Errors failure as fatal with no fallback; add degraded polling + periodic reconciliation.
- **Watcher/PollWatcher unwired; no periodic reconciliation backstop (`PLAT-03`).**
- **No Chmod-only / OS-junk event filtering (`PLAT-04`).**
- **`ServiceSpec` seam too thin to render the launchd plist (`PLAT-05`) — RESOLVED (`P4-PROD-04`).** `ServiceSpec` now carries Description/WorkingDir/Stdout+StderrPath/RestartOnFailure/RestartDelaySeconds and `ServiceManager` renders + installs the LaunchAgent (`internal/platform/service_launchd.go` + `service_darwin.go`, golden-tested) and the systemd user unit on Linux, driven by `devstrap service install|uninstall|status`. A native FSEvents watcher remains a follow-up.

## Audit follow-ups (2026-06-28)

Cross-platform findings (`XP-*`, from `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`):

- **Ship the portable Go core on macOS + Ubuntu before any native magic (`XP-01`):** the eager-clone materialization (`EAGER-*`), encrypted env/draft sync (`DRAFT-*`), and cloud hub backend (`HUB-*`) must run identically on both platforms via portable Go. No native daemon, FSEvents watcher, LaunchAgent installer, or StrapFS is in scope this cycle.
- **Keep Mac specifics behind adapters so Ubuntu stays first-class (`XP-02`):** the `internal/platform` watcher/service/keychain/editor seams remain the only place macOS behavior may diverge; the Linux fsnotify/inotify + periodic-reconciliation path must reach feature parity for the eager-sync loop.
- **Defer the native daemon and StrapFS (`XP-03`, Deferred section):** the LaunchAgent/FSEvents/Endpoint Security/File Provider/FUSE material above is explicitly deferred. Materialization stays eager clone-everything on `devstrap sync`; there is no placeholder/lazy-VFS layer in this design.

## Audit follow-ups (2026-07-07)

- **Seatbelt sandbox must grant the linked worktree's git dirs (`P7-SANDBOX-01`):** a DevStrap agent worktree is a git *linked* worktree whose index/objects/refs live in the parent clone's `.git`, outside the worktree dir — so under the default write confinement the kernel returned `EPERM` for `git add`/`git commit`, silently breaking the `agent run → agent pr` loop on Macs. The Seatbelt profile (and the Linux bwrap/Landlock backends) now also write-allow the linked worktree's `<git-common-dir>/{objects,refs,logs}` and the per-worktree admin dir, resolved by `git.Runner.WorktreeSandboxWriteDirs`; the common dir's `hooks/` and `config` are deliberately excluded (granting them would let the child plant a hook that runs unsandboxed). Kernel-proven by the env-gated `TestSeatbeltAllowsLinkedWorktreeCommit`. Full detail in `spec/10_AGENT_WORKSPACES_AND_POLICIES.md`.
- **Sandbox credential deny-list gains cloud/git token stores (`P7-SEC-01`):** the single `sensitiveHomeDirs`/`sensitiveHomeFiles` set that feeds the Seatbelt profile, the Linux bwrap masks, `credentialAnchors`, and `readConfineRoots` now also denies `~/.config/gcloud` (GCP refresh tokens), `~/.azure` (Azure CLI tokens), and `~/.git-credentials` (git's plaintext `credential.helper store` — the `.gitconfig` that was already masked merely points at it). Regression-pinned by `TestBwrapSensitivePathsCoversCloudAndGitCredentials` / `TestCredentialAnchorsCoverCloudAndGitCredentials`.
