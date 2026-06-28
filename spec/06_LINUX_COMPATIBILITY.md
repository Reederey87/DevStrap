---
last_reviewed: 2026-06-28
tracks_code: [internal/platform/**, .github/**]
---
# Linux Compatibility Plan

## Goal

Keep the portable Go core identical on macOS and Linux so an incoming GMKtec Ubuntu box becomes a first-class DevStrap node now, not "early" — Ubuntu parity is a present requirement, not a later port.

The 2026-06-28 cloud-sync decisions (recorded in `AUDIT_RECOMMENDATIONS_2026-06-28.md`, extending the 2026-06-27 second-pass audit) make this explicit: **cross-platform core first, OS-specific magic deferred** (workstream `XP-*`). The owner's fleet is mixed by design — two Mac Minis, the GMKtec Ubuntu box, a graphics laptop, and a NAS — so the same `~/Code` tree and the same `devstrap sync` eager-clone behavior must appear identically on macOS and Ubuntu running the one portable binary. No native daemon, FSEvents/inotify-specific watcher, or StrapFS is built this cycle on either platform; those remain deferred. The systemd unit below is a documented target, not shipped code.

## Linux target

Initial target:

```text
Ubuntu 24.04+ or current LTS
x86_64 and arm64 where practical
systemd user service          (deferred OS layer)
inotify watcher               (deferred OS layer)
~/Code managed root           (portable core, runs now)
~/.devstrap state             (portable core, runs now)
```

The portable core — `init`, `scan/adopt`, `add`, `hydrate`, `open`, `worktree`, `env`, and the file-backed `devstrap sync --hub-file` spike — must run identically on Ubuntu and macOS this cycle from the single Go binary. The `devstrap sync` eager blobless clone-everything flow (`EAGER-*`) and the cloud hub backend (`HUB-*`) are the cross-platform sync targets that land next, also platform-neutral. The systemd user service and native inotify watcher are deferred OS-specific layers (see "systemd user service" and "Linux watcher" below); the product is usable on Ubuntu through the foreground CLI before they land.

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

Deferred this cycle (`XP-*` defers the native daemon on both platforms); documented here as the target so the cross-platform `ServiceManager` seam stays accurate. On Ubuntu today the foreground CLI is the supported entry point. Example unit:

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

Current repository implementation covers the portable CLI pieces for init, scan/adopt, add, hydrate, env capture/hydrate/bind, provider-backed env runtime injection through `op run`, provider file hydration through `op inject`, status, fresh worktree creation, the file-backed `devstrap sync --hub-file` spike, platform adapter interfaces, build-tagged platform detection, and a polling watcher fallback. These already run from the one portable binary on both platforms. The systemd service, native inotify watcher, the cloud hub backend (`HUB-*`), and the `devstrap sync` eager blobless clone-everything materialization (`EAGER-*`) remain future work shared across macOS and Ubuntu — not Linux-specific.

## Audit follow-ups (2026-06-27)

The platform findings in `05_MAC_FIRST_IMPLEMENTATION.md` (`PLAT-01..05`) apply equally to the Linux adapters: unify watcher exclusions with the `spec/11` ignore compiler, add inotify `ENOSPC`/`max_user_watches` handling + polling fallback + periodic reconciliation, filter OS junk, and make the `ServiceSpec` seam rich enough to render the systemd user unit. Keep all Linux specifics behind `internal/platform` adapters.

## Audit follow-ups (2026-06-28)

The 2026-06-28 cloud-sync architecture (`AUDIT_RECOMMENDATIONS_2026-06-28.md`, workstream `XP-*`) sets the ordering this file follows: **cross-platform core first, OS-specific magic deferred.**

- Ubuntu is a first-class target now because the owner's fleet already includes an incoming GMKtec Ubuntu box alongside two Mac Minis, a graphics laptop, and a NAS. The same `~/Code` tree must appear on all of them via the one Go binary.
- The cloud sync hub (`devstraphub`) is platform-neutral by construction: repo content rides git's own blobless clone/fetch transport from each repo's existing remote and never touches the hub, env/draft content moves as age-encrypted content-addressed `age_blob:<sha256>` blobs, and the namespace map is a signed HLC-ordered event log. None of these planes are OS-specific, so Ubuntu and macOS sync identically (`HUB-*`, `DRAFT-*`). Backend is Cloudflare R2 from the start, pluggable behind one Hub interface, with a file-backed local backend kept only for tests — see `07_NAMESPACE_AND_SYNC_MODEL.md` and `13_CLI_DAEMON_API.md`.
- `devstrap sync` materializes eagerly (blobless/partial clone-everything up front; `node_modules`/build artifacts are never synced and are rebuilt on hydrate). There is no FUSE/placeholder/lazy-VFS layer in this design on either platform; StrapFS (the "Linux future virtual filesystem" section above) stays explicitly deferred (`EAGER-*`).
- The native systemd daemon and native inotify watcher remain deferred OS layers; the portable foreground CLI is the supported Ubuntu entry point this cycle.
