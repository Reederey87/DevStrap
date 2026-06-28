---
last_reviewed: 2026-06-28
tracks_code: [internal/platform/**, .github/**]
---
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
type Watcher interface {
    Watch(ctx context.Context, root string, events chan<- FSEvent) error
}

type ServiceManager interface {
    Install(ctx context.Context, spec ServiceSpec) error
    Status(ctx context.Context, label string) (ServiceStatus, error)
}

type Keychain interface {
    Store(ctx context.Context, service, account string, secret []byte) error
    Load(ctx context.Context, service, account string) ([]byte, error)
}

type EditorAdapter interface {
    Open(ctx context.Context, dir, editor string) error
}
```

Mac adapter:

```text
Watcher: fsnotify/kqueue now; native FSEvents target
Service: unsupported placeholder now; launchd LaunchAgent target
Secrets: system keyring-backed age and Ed25519 identities with file fallback
Paths: ~/.devstrap, ~/Code
```

Linux adapter:

```text
Watcher: fsnotify/inotify now
Service: unsupported placeholder now; systemd --user target
Secrets: Secret Service/keyring-backed age and Ed25519 identities with file fallback
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
StartLimitIntervalSec=60
StartLimitBurst=5
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

Headless machines that need the user service active without an interactive login may require:

```bash
loginctl enable-linger "$USER"
```

## Linux watcher

Use inotify through the same watcher abstraction. The current Linux adapter uses fsnotify, recursively adds directory watches below the managed root, skips generated trees, and emits debounced reconciliation hints.

Caveats:

- inotify watches can hit system limits;
- deep trees with many folders need careful watch management;
- dependency folders should be ignored aggressively;
- periodic scan is still required because watcher events are hints, not truth.

Recommended behavior:

```text
watch managed root
ignore generated/dependency paths
coalesce events for 500-1000 ms
periodically reconcile namespace and filesystem
```

If inotify returns `ENOSPC` or `EMFILE`, `doctor` must report the limit condition with remediation guidance and the daemon must fall back to periodic polling rather than silently missing changes.

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

The Unix socket is created under a `umask(077)` path, checked for stale instances by dialing before unlink, and removed on SIGTERM/SIGINT. CLI commands that require the daemon exit with code 3 when the socket is unavailable.

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
- env capture/hydrate works with encrypted local blobs, and runtime injection is added before Linux packaging is called complete.
- watcher detects new folders and Git repos.
- same namespace event stream syncs with Mac.

Current repository implementation covers the portable CLI pieces for init, scan/adopt, add, hydrate, env capture/hydrate/bind, provider-backed env runtime injection through `op run`, provider file hydration through `op inject`, status, fresh worktree creation, platform adapter interfaces, build-tagged platform detection, and a polling watcher fallback. The systemd service, native inotify watcher, and cross-device sync command remain future Linux work.

## Audit follow-ups (2026-06-27)

The platform findings in `05_MAC_FIRST_IMPLEMENTATION.md` (`PLAT-01..05`) apply equally to the Linux adapters: unify watcher exclusions with the `spec/11` ignore compiler, add inotify `ENOSPC`/`max_user_watches` handling + polling fallback + periodic reconciliation, filter OS junk, and make the `ServiceSpec` seam rich enough to render the systemd user unit. Keep all Linux specifics behind `internal/platform` adapters.
