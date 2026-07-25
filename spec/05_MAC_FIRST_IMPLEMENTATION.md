---
last_reviewed: 2026-07-24
tracks_code: [internal/platform/**, internal/cli/open.go, internal/cli/hydrate.go, .github/**]
---
# Mac-First Implementation Guide

## Goal

Build a Mac solution that feels native enough to solve the daily pain, while keeping the core portable to Linux.

## Sequencing note (2026-06-28): cross-platform core first

The 2026-06-28 cloud-sync decisions (see `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`, workstream `XP-*`) re-order this guide's build sequence: ship the **portable Go core first on both macOS and Ubuntu**, before any native macOS magic. The "Dropbox experience for code" — one identical `~/Code` tree on every device in a mixed macOS/Linux fleet (workstations, laptops, headless boxes, agent runners) — is delivered this cycle by the portable core (eager blobless clone on `devstrap sync`, age-encrypted env/draft blobs, and the signed HLC-ordered namespace map), not by a daemon or virtual filesystem.

Consequently, treat the daemon, native FSEvents watcher, Endpoint Security, File Provider, and FUSE/StrapFS content below as **later layers, not this-cycle work** — with one exception: the LaunchAgent shipped as `devstrap service install` (`P4-PROD-04`, wrapping the portable `run-loop`; see the shipped section below). The Mac-specific adapter seams in `internal/platform` stay valuable as the eventual home for that behavior and as the proof that Mac specifics stay behind adapters so Ubuntu remains first-class — but they are deferred. Materialization in the cross-platform core is **eager clone-everything on `devstrap sync`** (partial/blobless clone up front); there is no placeholder/lazy-VFS step in this design.

## Recommended Mac MVP

```text
CLI:        /opt/homebrew/bin/devstrap
Daemon:     ~/Library/LaunchAgents/com.devstrap.devstrapd.plist
State:      ~/.devstrap/state.db
Socket:     ~/.devstrap/devstrapd.sock
Managed:    ~/Code
Watcher:    fsnotify/kqueue (native FSEvents deferred — see the decision below)
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
- **keychain-custody warning at install (`P7-XP-02`)** — a store with recorded keychain custody installs with a warning: a locked keychain (before the first GUI login after a reboot) makes run-loop ticks fail closed until unlock, and `doctor` names it while the service is installed.
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

### Native FSEvents vs fsnotify/kqueue — DEFERRED pending measurement (2026-07-24)

**Decision: the daemon wave stays on the fsnotify/kqueue adapter. A native FSEvents adapter is deferred, not adopted and not abandoned.** The tracked task in `14_MVP_ROADMAP_AND_BACKLOG.md` — *"Implement FSEvents-specific Mac watcher if fsnotify/kqueue proves insufficient"* — stays unchecked; what changes here is that *"proves insufficient"* now has an operational definition and a slice of work that produces the evidence.

**The cost is concrete and reaches past the watcher.** FSEvents is a CoreServices C API and there is no pure-Go binding, so a native adapter means **cgo on darwin**. The release pipeline builds every artifact with `CGO_ENABLED=0` and cross-compiles darwin *and* linux for amd64+arm64 from a single `ubuntu-latest` runner (`.goreleaser.yaml`, `.github/workflows/release.yml`); `modernc.org/sqlite` was chosen as a pure-Go driver precisely to keep that true. Enabling cgo for darwin requires a macOS builder with an SDK for the release build, forfeits the single self-contained static binary, and adds a linked-library dimension to the cosign/SBOM/notarization work (`P4-SEC-05`/`P4-QUAL-05`). That is a distribution-wide cost paid for a watcher improvement — it needs evidence, not a preference.

**The benefit is real but unquantified.** fsnotify's macOS backend is kqueue, which needs an open file descriptor per watched path and — because kqueue has no directory-content notification — additionally one per entry inside each watched directory, so descriptor usage tracks the *file* count under `~/Code`, not the directory count. `addRecursiveWatch` registers the whole managed root at start and on every directory create, pruning only `.git`, `node_modules`, `.devstrap`, and `vendor` (`PLAT-01`: the ignore compiler still does not feed this list). FSEvents by contrast is one per-session stream subscribed to a path prefix, with no per-path descriptor, and it survives sleep/wake and volume remount through a replayable event id. Whether the kqueue cost actually bites depends on the size and shape of a real `~/Code` and on a per-process descriptor limit that varies by macOS version and launch context — not a constant this spec can assert.

**We have no measurement, because the watcher has no consumer.** The only non-test reference to `NativeWatcher` in the tree is its own implementation; `platform.Watcher` has never been wired into a running command. Choosing FSEvents today would be guessing at a cost we have never observed.

**Backed by:** the watcher-wiring slice of the daemon wave is the adapter's first real consumer (hints only) and instruments the thing in dispute — descriptor usage and `Add`/`Errors` failures against a real managed root — while degrading to `PollWatcher` on `EMFILE`/`ENOSPC`/any watcher error instead of failing (`PLAT-02`/`PLAT-03`). Correctness never rides on this choice: watch events are hints and periodic reconciliation is the backstop, so the worst case of staying on kqueue is added latency or a fall back to polling, never namespace divergence.

**Reconsider when** either holds: (a) measured descriptor usage on a representative `~/Code` approaches the process limit even after the `spec/11` ignore compiler feeds watcher exclusions (`PLAT-01`) — i.e. the pruning fix is exhausted and the backend is still the constraint; or (b) the kqueue backend demonstrably drops or duplicates events across sleep/wake or volume remount in a way periodic reconciliation cannot absorb. Either finding makes cgo-on-darwin (or a separately shipped native helper) worth its distribution cost. Neither is demonstrated today.

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
- **No ENOSPC/EMFILE handling (`PLAT-02`):** the watcher treats every Add/Errors failure as fatal with no fallback; add degraded polling + periodic reconciliation. **[Implemented 2026-07-24: the daemon's watch plane (`internal/daemon/watch.go`) catches a native-watcher failure and falls back to `PollWatcher`, recording `degraded` + a `reason` on `/v1/health` so the degradation is visible rather than silent. Periodic convergence runs underneath either way, so a lost watcher costs latency, never correctness.]**
- **Watcher/PollWatcher unwired; no periodic reconciliation backstop (`PLAT-03`).** **[Implemented 2026-07-24: the daemon is the adapter's first consumer — it watches materialized project roots and converts coalesced hints into namespace-only convergence, with the daemon's own periodic cycle as the backstop. `PLAT-01` (unify the exclusion set on the `spec/11` ignore compiler) and `PLAT-04` (chmod/OS-junk filtering) remain open.]**
- **No Chmod-only / OS-junk event filtering (`PLAT-04`).**
- **`ServiceSpec` seam too thin to render the launchd plist (`PLAT-05`) — RESOLVED (`P4-PROD-04`).** `ServiceSpec` now carries Description/WorkingDir/Stdout+StderrPath/RestartOnFailure/RestartDelaySeconds and `ServiceManager` renders + installs the LaunchAgent (`internal/platform/service_launchd.go` + `service_darwin.go`, golden-tested) and the systemd user unit on Linux, driven by `devstrap service install|uninstall|status`. A native FSEvents watcher remains a follow-up, scoped by the FSEvents/CGO decision under *Filesystem watcher* above.

## Audit follow-ups (2026-06-28)

Cross-platform findings (`XP-*`, from `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`):

- **Ship the portable Go core on macOS + Ubuntu before any native magic (`XP-01`):** the eager-clone materialization (`EAGER-*`), encrypted env/draft sync (`DRAFT-*`), and cloud hub backend (`HUB-*`) must run identically on both platforms via portable Go. No native daemon, FSEvents watcher, LaunchAgent installer, or StrapFS is in scope this cycle.
- **Keep Mac specifics behind adapters so Ubuntu stays first-class (`XP-02`):** the `internal/platform` watcher/service/keychain/editor seams remain the only place macOS behavior may diverge; the Linux fsnotify/inotify + periodic-reconciliation path must reach feature parity for the eager-sync loop.
- **Defer the native daemon and StrapFS (`XP-03`, Deferred section):** the LaunchAgent/FSEvents/Endpoint Security/File Provider/FUSE material above is explicitly deferred. Materialization stays eager clone-everything on `devstrap sync`; there is no placeholder/lazy-VFS layer in this design.

## Audit follow-ups (2026-07-07)

- **Seatbelt sandbox must grant the linked worktree's git dirs (`P7-SANDBOX-01`):** a DevStrap agent worktree is a git *linked* worktree whose index/objects/refs live in the parent clone's `.git`, outside the worktree dir — so under the default write confinement the kernel returned `EPERM` for `git add`/`git commit`, silently breaking the `agent run → agent pr` loop on Macs. The Seatbelt profile (and the Linux bwrap/Landlock backends) now also write-allow the linked worktree's `<git-common-dir>/{objects,refs,logs}` and the per-worktree admin dir, resolved by `git.Runner.WorktreeSandboxWriteDirs`; the common dir's `hooks/` and `config` are deliberately excluded (granting them would let the child plant a hook that runs unsandboxed). Kernel-proven by the env-gated `TestSeatbeltAllowsLinkedWorktreeCommit`. Full detail in `spec/10_AGENT_WORKSPACES_AND_POLICIES.md`.
- **Sandbox credential deny-list gains cloud/git token stores (`P7-SEC-01`):** the single `sensitiveHomeDirs`/`sensitiveHomeFiles` set that feeds the Seatbelt profile, the Linux bwrap masks, `credentialAnchors`, and `readConfineRoots` now also denies `~/.config/gcloud` (GCP refresh tokens), `~/.azure` (Azure CLI tokens), and `~/.git-credentials` (git's plaintext `credential.helper store` — the `.gitconfig` that was already masked merely points at it). Regression-pinned by `TestBwrapSensitivePathsCoversCloudAndGitCredentials` / `TestCredentialAnchorsCoverCloudAndGitCredentials`.
- **Seatbelt `Available()` launch-probes instead of stat-only (`P7-XP-07`):** resolve-time honesty parity with the Linux backends, which do a real `landlock_create_ruleset` syscall / bwrap launch probe. The macOS check `os.Stat`'d `/usr/bin/sandbox-exec` and tested the executable bit but never launched it, so a present-but-broken binary (a future Apple removal, or a policy block) was reported available until first agent use. `probeSeatbelt` (a package-level `sync.OnceValues` cache mirroring `probeBwrap`/`probeLandlock`) now stats the binary and then runs a real minimal launch — `sandbox-exec -p '(version 1)(allow default)' /usr/bin/true` under a 3s context timeout — wrapping any failure in the shared `ErrUnsupported` sentinel, so `--sandbox auto` degrades to a loud warning at resolve time and `require` fails closed. Exercised by darwin-tagged `TestSeatbeltAvailableLaunchProbes`. Full detail in `spec/10_AGENT_WORKSPACES_AND_POLICIES.md`.
