# Linux Compatibility Plan

## Goal

Keep the Mac-first implementation portable enough that a GMK Ubuntu box can become a first-class DevStrap node early.

## Linux target

Initial target:

```text
Ubuntu 24.04+ or current LTS
x86_64 and arm64 where practical
systemd user service
inotify watcher
~/Code managed root
~/.devstrap state
```

## Platform-neutral core

These parts must be identical on Mac and Linux:

- namespace model;
- project types;
- materialization states;
- Git orchestration;
- worktree creation rules;
- env/secrets abstractions;
- ignore compiler;
- sync event protocol;
- daemon job scheduler;
- CLI command behavior;
- SQLite schema.

## Platform adapters

```go
type Platform interface {
    Watcher(root string) (Watcher, error)
    ServiceManager() ServiceManager
    KeyStore() KeyStore
    OpenEditor(editor string, path string) error
    DefaultCodeRoot() string
    Paths() PlatformPaths
}
```

Mac adapter:

```text
Watcher: FSEvents
Service: launchd LaunchAgent
Secrets: Keychain
Paths: ~/.devstrap, ~/Code
```

Linux adapter:

```text
Watcher: inotify
Service: systemd --user
Secrets: libsecret/keyring or encrypted file protected by OS user permissions
Paths: ~/.devstrap, ~/Code, XDG optional
```

## systemd user service

Example:

```ini
[Unit]
Description=DevStrap user daemon
After=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/devstrapd serve
Restart=on-failure
RestartSec=5
Environment=DEVSTRAP_HOME=%h/.devstrap

[Install]
WantedBy=default.target
```

Install:

```bash
mkdir -p ~/.config/systemd/user
cp devstrapd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now devstrapd.service
```

## Linux watcher

Use inotify through the same watcher abstraction.

Caveats:

- inotify watches can hit system limits;
- deep trees with many folders need careful watch management;
- dependency folders should be ignored aggressively;
- periodic scan is still required.

Recommended behavior:

```text
watch managed root
ignore generated/dependency paths
coalesce events
periodically reconcile namespace and filesystem
```

## Linux future virtual filesystem

Linux FUSE is the natural future StrapFS backend.

Possible V2 behavior:

```text
~/Code is a FUSE mount
listing shows namespace entries
open skeleton repo triggers materialization
read-only until hydrated
```

Do not build this first.

## Path compatibility rules

Use a portable namespace policy even on Linux.

Default forbidden:

- paths differing only by case;
- path traversal `..`;
- absolute paths inside namespace entries;
- symlink escape without explicit allow;
- control characters;
- very long path segments.

## Permissions

Linux permissions can differ from macOS.

Rules:

- DevStrap runs as normal user;
- no root required for MVP;
- `~/Code` owned by user;
- `~/.devstrap` mode `0700`;
- encrypted cache files mode `0600`;
- socket mode restricted to user.

## Secret handling on Linux

Options:

1. external vault CLI: 1Password, Doppler, Infisical;
2. OS keyring/libsecret adapter;
3. encrypted local key file protected by passphrase for MVP;
4. age-encrypted per-device env bundles.

Do not require GNOME Keyring in headless Ubuntu environments. Headless support needs non-GUI auth, for example service token or passphrase unlock.

## Toolchain differences

Linux machines may have different dependencies than Mac.

Tooling profile should support OS-specific blocks:

```yaml
tooling:
  common:
    - git
    - uv
  macos:
    brew:
      - git-lfs
      - uv
  linux:
    apt:
      - git-lfs
      - build-essential
      - python3-dev
```

## Container strategy

Linux is the better host for agent-heavy development.

Recommended:

- support Docker/Podman detection;
- prefer devcontainers for reproducible agents;
- allow repo profile to specify container target;
- keep local native dependencies out of sync.

## Linux MVP acceptance criteria

- `devstrap init ~/Code` works on Ubuntu.
- systemd user service starts daemon.
- daemon creates skeleton directories.
- Git hydration works.
- fresh worktree creation works.
- env capture/hydrate works with encrypted store.
- watcher detects new folders and Git repos.
- same namespace event stream syncs with Mac.

