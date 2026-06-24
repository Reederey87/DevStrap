# Mac-First Implementation Guide

## Goal

Build a Mac solution that feels native enough to solve the daily pain, while keeping the core portable to Linux.

## Recommended Mac MVP

```text
CLI:        /opt/homebrew/bin/devstrap
Daemon:     ~/Library/LaunchAgents/com.devstrap.devstrapd.plist
State:      ~/.devstrap/state.db
Socket:     ~/.devstrap/devstrapd.sock
Managed:    ~/Code
Watcher:    FSEvents through fsnotify/native adapter
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

## Example LaunchAgent plist

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.devstrap.devstrapd</string>

  <key>ProgramArguments</key>
  <array>
    <string>/opt/homebrew/bin/devstrapd</string>
    <string>serve</string>
  </array>

  <key>RunAtLoad</key>
  <true/>

  <key>KeepAlive</key>
  <true/>

  <key>StandardOutPath</key>
  <string>/Users/USER/.devstrap/logs/devstrapd.out.log</string>

  <key>StandardErrorPath</key>
  <string>/Users/USER/.devstrap/logs/devstrapd.err.log</string>
</dict>
</plist>
```

Install command:

```bash
mkdir -p ~/Library/LaunchAgents ~/.devstrap/logs
cp com.devstrap.devstrapd.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.devstrap.devstrapd.plist
launchctl enable gui/$(id -u)/com.devstrap.devstrapd
launchctl kickstart -k gui/$(id -u)/com.devstrap.devstrapd
```

Uninstall command:

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.devstrap.devstrapd.plist
rm ~/Library/LaunchAgents/com.devstrap.devstrapd.plist
```

## Filesystem watcher

Use a Go watcher abstraction for MVP. Prefer a native FSEvents-backed Mac adapter when reliable recursive tree semantics matter. `fsnotify` is still useful as a cross-platform interface and already supports Linux inotify, but its macOS backend is kqueue rather than FSEvents, so the spec must not rely on fsnotify alone for FSEvents behavior.

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

`placeholder.json`:

```json
{
  "version": 1,
  "path": "work/acme/api",
  "type": "git_repo",
  "remote": "git@github.com:acme/api.git",
  "default_branch": "main",
  "materialization": "skeleton"
}
```

`README.devstrap.md`:

````markdown
# DevStrap placeholder

This project is known to DevStrap but is not hydrated on this machine yet.

Run:

```bash
devstrap open work/acme/api --cursor
```
````

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

- store device private key in macOS Keychain;
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
- not needed to solve stale main, env, worktree, and path problems.

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

- `devstrap init ~/Code` creates state, config, and managed root.
- LaunchAgent keeps daemon running after login.
- Daemon recreates skeleton folders from namespace state.
- Scanner adopts existing Git repos.
- `devstrap open <path> --cursor` hydrates and opens repo.
- `devstrap worktree new <path> --fresh-main` fetches origin and creates worktree from remote SHA.
- Env capture/hydrate works with encrypted local store.
- Dirty repos are detected and not overwritten.
- Logs are readable under `~/.devstrap/logs`.
