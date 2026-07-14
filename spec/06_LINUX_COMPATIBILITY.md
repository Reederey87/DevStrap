---
last_reviewed: 2026-07-14
tracks_code: [internal/platform/**, internal/devicekeys/**, .github/**]
---
# Linux Compatibility Plan

## Goal

Keep the portable Go core identical on macOS and Linux so a Linux box is a first-class DevStrap node now, not "early" — Ubuntu parity is a present requirement, not a later port. DevStrap targets mixed macOS/Linux fleets (developer workstations, headless servers, cloud machines, and agent runners), so the same `~/Code` tree and the same `devstrap sync` behavior must appear identically on both platforms from the one portable binary.

The 2026-06-28 cloud-sync decisions (recorded in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`, extending the 2026-06-27 second-pass audit) make this explicit: **cross-platform core first, OS-specific magic deferred** (workstream `XP-*`). Because target fleets are mixed by design — desktops and laptops across macOS and Linux, plus headless/cloud/agent runners — the same `~/Code` tree and the same `devstrap sync` eager-clone behavior must appear identically on macOS and Ubuntu running the one portable binary. No native daemon, FSEvents/inotify-specific watcher, or StrapFS is built this cycle on either platform; those remain deferred. The systemd unit below is a documented target, not shipped code.

## Windows first-pass smoke (`P4-QUAL-04`)

Windows is not yet a supported parity target, but process-liveness checks used by repo and folder-hub locks are now routed through one build-tagged `internal/platform.ProcessAlive` adapter. Darwin/Linux use signal 0 and treat only `ESRCH`/`os.ErrProcessDone` as dead; Windows uses `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` plus `GetExitCodeProcess` and treats `STILL_ACTIVE` (259), access denial, and other indeterminate results as alive. Unsupported platforms return alive unconditionally. The shared contract is fail-safe: a lock is stealable only when its holder is positively confirmed gone.

CI has a separate advisory `windows-latest` smoke job that builds and vets `./...`, then tests only the platform-safe `internal/platform`, `pathkey`, `ignore`, `git`, `draftbundle`, `envfile`, `redact`, `id`, and `pairing` packages. It deliberately excludes the broader CLI/hub/state suites and remains `continue-on-error` for its first cycle; this is build/vet/narrow-test visibility, not a claim of full Windows support.

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

The portable core — `init`, `scan/adopt`, `add`, `hydrate`, `open`, `worktree`, `env`, and `devstrap sync` (`--hub-file <path>` for the file-backed test backend, or `hub: r2://<bucket>` for the shipped R2/S3 production backend) — must run identically on Ubuntu and macOS this cycle from the single Go binary. The `devstrap sync` eager blobless clone-everything flow (`EAGER-*`) and the cloud hub backend (`HUB-*`) are shipped and platform-neutral (the live R2/S3 adapter landed in `P5-HUB-01`). The systemd user service and native inotify watcher are deferred OS-specific layers (see "systemd user service" and "Linux watcher" below); the product is usable on Ubuntu through the foreground CLI before they land.

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

## systemd user service — shipped via `devstrap service install` (`P4-PROD-04`)

The **native daemon (`devstrapd serve`) stays deferred**, but the systemd user-unit installer is shipped: `devstrap service install` renders and installs a `--user` service wrapping the portable `run-loop`, so a Linux box converges unattended without the daemon. The rendered unit (`~/.config/systemd/user/devstrap-run-loop.service`, written atomically at mode `0600`):

```ini
[Unit]
Description=DevStrap run-loop (scan + sync + materialize)
After=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=/usr/local/bin/devstrap run-loop --interval 5m0s
Restart=on-failure
RestartSec=30
Environment="PATH=/usr/local/bin/…:/home/you/.local/bin:/usr/local/bin:/usr/bin:/bin"

[Install]
WantedBy=default.target
```

Shipped commands (the adapter runs `systemctl --user daemon-reload`/`enable`/`restart`; **install** is gated behind a `systemctl --user show-environment` availability probe — a missing systemd or D-Bus-less session fails closed as `ErrUnsupported`, never a confusing raw error):

```bash
devstrap service install     # render unit → daemon-reload → enable → restart
devstrap service status      # is-active + `journalctl --user -u …` hint (also --json)
devstrap service uninstall   # best-effort disable --now → ALWAYS remove unit → daemon-reload
                             # when reachable (idempotent; works headless)
```

**Headless uninstall (`P7-XP-03`).** `Uninstall` mirrors launchd: the availability probe no longer gates removal. When the `--user` manager is unreachable (SSH, cron, no session D-Bus) the adapter still deletes the unit file — the durable install artifact `status` reports — and returns an advisory note (a new `ServiceManager.Uninstall` notes return, printed verbatim even under `--quiet`): if a lingering session still runs the service, finish with `systemctl --user disable --now devstrap-run-loop.service && systemctl --user daemon-reload` from a user session. A headless uninstall that finds no unit file stays a clean, note-free no-op. Install keeps failing closed — installing needs the manager; removing must not.

`ExecStart` words are systemd-quoted (whitespace/quotes double-quoted, `%` doubled to escape specifier expansion); `ExecPath` defaults to `os.Executable()` (an explicit absolute `--exec-path` is honored verbatim instead, bypassing all resolution) and is refused at an ephemeral `$TMPDIR`/`go-build` resolution. Symlinks are resolved **except** when the invoked path sits in a stable install bin dir (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, Linuxbrew's `bin`, or a keg-only/versioned formula's `<brew prefix>/opt/<formula>/bin`) — the symlink itself is baked so a package-manager upgrade that moves the target (the Homebrew Cellar) cannot brick the unit (`P7-XP-01`); a path that still resolves into a `Cellar/` segment is refused with an `--exec-path` remedy. `Status` best-effort parses the unit's `ExecStart` first word (undoing the systemd quoting) and reports `ExecPath missing: <path>` when the baked binary no longer exists, and `doctor` warns with a re-run-`service install` remedy (`P7-XP-05`).

**Lingering advisory.** A systemd `--user` manager stops when the user's last session ends, so on install the adapter probes `loginctl show-user "$USER" --property=Linger --value` and, when linger is off (or the probe fails), returns an advisory note the CLI prints verbatim:

```bash
loginctl enable-linger "$USER"   # may require sudo; needed for headless/boot persistence
```

The probe never fails the install — it only advises.

**Keychain custody (fail-closed, `P6-XP-04`; install-time gate, `P7-XP-02`).** The service runs in exactly the D-Bus-less context where the Secret Service is unreachable, and a keychain-custody store under the unit fails closed every tick until the run-loop exits into a restart loop. `service install` therefore checks custody UP FRONT: a `file`-custody store proceeds silently; recorded `keychain` custody is **refused** on systemd with a migrate-to-file remedy unless `--allow-keychain-custody` is passed (for the rare desktop-Linux-with-linger box that really has a session D-Bus at service runtime); unrecorded (pre-`P6-XP-04`) custody warns. The no-silent-downgrade rule stands: the installer never *invents* file custody — but when the operator runs the install with `DEVSTRAP_NO_KEYCHAIN=1` already set (an explicit, non-secret override that makes custody effectively file-backed), that override is baked into the unit's `Environment=` so the service behaves like the session that installed it instead of stranding. `doctor` repeats the custody warning while a service is installed. See the headless custody model at `P6-XP-04` below.

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

When the deferred daemon ships, its Unix socket must be created under a `umask(077)` path, checked for stale instances by dialing before unlink, and removed on SIGTERM/SIGINT; CLI commands that require the daemon will exit with the reserved code 3 (`exitDaemonUnavailable`, currently reserved and never returned — see `internal/cli/root.go`). No daemon, socket, or socket-requiring command exists today.

## Linux agent sandbox — credential reads fail closed on the Landlock fallback (`P7-SEC-03`)

`devstrap agent run` sandboxes the child on Linux by lazily choosing bubblewrap, then the Landlock fallback (used on Ubuntu 23.10+/kernel ≥ 6.5, where AppArmor's user-namespace restrictions break bubblewrap). Both back a kernel write-confinement boundary; the full contract lives in `spec/10` and `spec/15`. One Linux-specific behavior belongs here: because Landlock is additive-allow with no subtraction operator, the fallback originally granted `RODirs("/")` wholesale and left credential reads open, so `--sandbox require` did not actually fail closed for `~/.ssh`/`~/.aws`/`~/.git-credentials`-class reads. It now follows the Landlock kernel docs' leaf-hierarchy "good practice": under the default (non-`--read-confine`) policy the fallback grants read+execute on filesystem leaves that omit the credential anchors (`sensitiveHomeDirs`/`sensitiveHomeFiles` + `~/.devstrap/keys`, the same deny-list bubblewrap masks and Seatbelt denies), walking down only through anchor ancestors and granting their siblings wholesale — directories via `RODirs`, regular files via `ROFiles` (Landlock rejects directory rights on a regular file), and skipping a sibling symlink whose target aliases an anchor. An anchor that is ITSELF a symlink (the stow/chezmoi dotfiles layout, `~/.ssh -> ~/dotfiles/ssh`) is resolved with `EvalSymlinks` before the carve-out is built, so both the literal path and its real target subtree enter the denied set and the target can no longer be re-granted wholesale as a non-anchor sibling — matching how the Seatbelt/bubblewrap deny-lists resolve the same aliases. Unlike bubblewrap's tmpfs/`/dev/null` masks (which show empty), an omitted Landlock path returns `EACCES`. A directory that cannot be enumerated receives no grant (fail closed). This makes credential-read denial hold on the Landlock fallback in both `--sandbox auto` and `--sandbox require`, not only when `--read-confine` is engaged.

## Secret handling on Linux

Options:

1. external vault CLI: 1Password, Doppler, Infisical;
2. OS keyring/libsecret adapter;
3. encrypted local key file protected by passphrase for MVP;
4. age-encrypted per-device env bundles.

Do not require GNOME Keyring in headless Ubuntu environments. Headless support needs non-GUI auth, for example service token or passphrase unlock. A headless box that never had a Secret Service records `file` custody at init and keeps using the `0600` file store; a box that had a keychain at init and later runs without a session bus fails closed rather than silently minting a divergent file identity, and the operator opts into file custody with `DEVSTRAP_NO_KEYCHAIN=1` (`P6-XP-04`, see the shipped section below).

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

- `devstrap init ~/Code` works on Ubuntu. (shipped)
- systemd user service starts daemon. (deferred — daemon layer)
- daemon creates skeleton directories. (deferred — daemon layer)
- Git hydration works. (shipped)
- fresh worktree creation works. (shipped)
- env capture/hydrate works with encrypted local blobs, and runtime injection is added before Linux packaging is called complete. (shipped)
- watcher detects new folders and Git repos. (deferred — daemon layer)
- same namespace event stream syncs with Mac. (shipped)

Shipped daemonless operation: `devstrap run-loop` runs sync + materialize on an interval from the one portable binary (its advertised scan stage is pending, `P6-XP-03`); pair it with cron or a systemd user timer until the native daemon lands.

Current repository implementation covers the portable CLI pieces for init, scan/adopt, add, hydrate, env capture/hydrate/bind, provider-backed env runtime injection through `op run`, provider file hydration through `op inject`, status, fresh worktree creation, `devstrap sync` (`--hub-file <path>` for the file-backed test backend, or `hub: r2://<bucket>` for the shipped R2/S3 production backend), `run-loop`, `materialize`, `draft snapshot create`, the `devices` trust CLI, `conflicts`, `doctor`, `hub gc`, platform adapter interfaces, build-tagged platform detection, and a polling watcher fallback (see `spec/00`'s command inventory for the full list). These already run from the one portable binary on both platforms. The systemd service and native inotify watcher remain future work shared across macOS and Ubuntu — not Linux-specific. (The cloud hub backend `HUB-*` and the `devstrap sync` eager blobless clone-everything materialization `EAGER-*` are now shipped; the live R2/S3 adapter landed in `P5-HUB-01`.)

## Audit follow-ups (2026-06-27)

The platform findings in `05_MAC_FIRST_IMPLEMENTATION.md` (`PLAT-01..05`) apply equally to the Linux adapters: unify watcher exclusions with the `spec/11` ignore compiler, add inotify `ENOSPC`/`max_user_watches` handling + polling fallback + periodic reconciliation, filter OS junk, and make the `ServiceSpec` seam rich enough to render the systemd user unit. Keep all Linux specifics behind `internal/platform` adapters.

## Audit follow-ups (2026-06-28)

The 2026-06-28 cloud-sync architecture (`docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`, workstream `XP-*`) sets the ordering this file follows: **cross-platform core first, OS-specific magic deferred.**

- Ubuntu is a first-class target now because DevStrap targets mixed macOS/Linux fleets — desktops and laptops across both platforms, plus headless/cloud machines and agent runners. The same `~/Code` tree must appear on all of them via the one Go binary.
- The cloud sync hub (`devstraphub`) is platform-neutral by construction: repo content rides git's own blobless clone/fetch transport from each repo's existing remote and never touches the hub, env/draft content moves as age-encrypted content-addressed `age_blob:<sha256>` blobs, and the namespace map is a signed HLC-ordered event log. None of these planes are OS-specific, so Ubuntu and macOS sync identically (`HUB-*`, `DRAFT-*`). Backend is Cloudflare R2 from the start, pluggable behind one Hub interface, with a file-backed local backend kept only for tests — see `07_NAMESPACE_AND_SYNC_MODEL.md` and `13_CLI_DAEMON_API.md`.
- `devstrap sync` materializes eagerly (blobless/partial clone-everything up front; `node_modules`/build artifacts are never synced and are rebuilt on hydrate). There is no FUSE/placeholder/lazy-VFS layer in this design on either platform; StrapFS (the "Linux future virtual filesystem" section above) stays explicitly deferred (`EAGER-*`).
- The native systemd daemon and native inotify watcher remain deferred OS layers; the portable foreground CLI is the supported Ubuntu entry point this cycle.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-XP-04 — headless Linux keychain-`unavailable` heuristic mints a divergent identity, wedging sync — **shipped (2026-07-03)**

**Resolution (shipped).** Fixed with a small deviation from step 2 below, made deliberately and with a better outcome: rather than **keeping** the D-Bus substring classification inside `internal/devicekeys` and layering typed checks on top, the substring recognition was **moved down to `internal/platform` (`mapKeyringError`/`secretServiceUnreachable`)** — the layer closest to go-keyring, where the untyped godbus error actually arrives — and re-emitted as the typed `platform.ErrUnsupported`. `internal/devicekeys` now classifies purely with `errors.Is` against `ErrUnsupported`/`ErrSecretNotFound` and no longer inspects any error string, so the file-store fallback still engages on headless Linux (the concern step 2 raised) while the classification lives in one correct place instead of two. The published-key mint guard (step 1), the WCK custody guard (step 3), the recorded `key_custody` decision in the new `local_meta` table (step 4, migration `00016`), and the headless-dead-D-Bus regression test (step 5) all shipped as specified. See the key-custody model in `spec/09` and the split-custody-wedge threat in `spec/15`. Note the one behavioral tightening: a store initialized under a reachable keychain and later run headless now **fails closed** (rather than silently minting a file identity) unless `DEVSTRAP_NO_KEYCHAIN=1` is set — the deliberate no-silent-downgrade rule.

**Problem.** On headless Linux — the exact cron/systemd-unit target of `devstrap service install` (shipped, `P4-PROD-04`) — the Secret Service is session-scoped, so any event-stamping command run without `DBUS_SESSION_BUS_ADDRESS` produces a `"dbus"`/`"connection refused"` error. `keychainUnavailable` (`internal/devicekeys/devicekeys.go:414-430`) classifies these by substring, `loadSecret` (`devicekeys.go:394-396`) maps them to `os.ErrNotExist`, and `EnsureSigning` (`devicekeys.go:180-204`) mints a brand-new signing identity into `~/.devstrap/keys` without ever consulting the device's already-published `devices.signing_public_key`. The too-late SQL guard in `store.go:2325-2344` then rejects the mismatch after the orphan key file is on disk, permanently wedging every later headless run (`run-loop` aborts after 5 failing ticks) while desktop runs keep working. The same substring heuristic also guards the WCK custody path (`StoreWCK`/`LoadWCK`, `devicekeys.go:291-322`), extending the blast radius to the workspace-key foundation.

**Actionable steps.**
1. Thread `devices.signing_public_key` into `ensureLocalEventSignature`/`EnsureSigning` and refuse to mint whenever it is already set and the keychain is merely unreachable.
2. **Keep** the existing Linux D-Bus substring classification (raw `"dbus"`/`"connection refused"` errors from a session-less Secret Service are not `errors.Is`-matchable), and **layer** `errors.Is` against `internal/platform`'s `ErrSecretNotFound`/`ErrUnsupported` sentinels (already `%w`-wrapped at `platform.go:205-213`) on top only where the backend already wraps those errors — do not swap the substring cases out, or the file-store fallback stops on headless Linux and the sync wedge returns.
3. Apply the identical fix to `StoreWCK`/`LoadWCK` (`devicekeys.go:291-322`).
4. Record a one-time `key_custody` decision on first successful probe (config/DB field) and honor it on later runs — a prerequisite for `devstrap service install` (shipped, `P4-PROD-04`), whose systemd `--user` unit runs in exactly this D-Bus-less context.
5. Add a headless-Linux regression test simulating a dead D-Bus session on a device with an already-published signing key.

**Example.**

```go
if storedPub != "" {
    return fmt.Errorf(
        "device signing key exists (%s) but keychain is unreachable "+
            "(session bus missing?); run from your desktop session, or set %s=1 and migrate the key",
        storedPub, platform.NoKeychainEnv,
    )
}
switch {
case errors.Is(err, platform.ErrSecretNotFound):
    return generateAndStore() // key genuinely absent
case errors.Is(err, platform.ErrUnsupported):
    return nil, fmt.Errorf("keychain unreachable, refusing to mint a divergent key: %w", err)
}
```
