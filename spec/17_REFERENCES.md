# References

These references shaped the architecture choices.

## macOS

- Apple File Provider framework: https://developer.apple.com/documentation/fileprovider
- Apple sample for synchronizing files with File Provider extensions: https://developer.apple.com/documentation/FileProvider/synchronizing-files-using-file-provider-extensions
- Apple File System Events: https://developer.apple.com/documentation/coreservices/file_system_events
- Apple launchd Launch Daemons and Agents: https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html
- Apple Service Management framework: https://developer.apple.com/documentation/ServiceManagement
- Apple notarization: https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution
- macFUSE: https://macfuse.github.io/

## Linux and filesystem

- Linux inotify man page: https://man7.org/linux/man-pages/man7/inotify.7.html
- Linux FUSE kernel documentation: https://www.kernel.org/doc/html/next/filesystems/fuse.html
- Go fsnotify: https://github.com/fsnotify/fsnotify
- fsnotify package docs and backend status: https://pkg.go.dev/github.com/fsnotify/fsnotify
- Rust notify crate: https://docs.rs/notify
- WinFsp for future Windows support: https://winfsp.dev/

## Git

- Git partial clone: https://git-scm.com/docs/partial-clone
- Git worktree: https://git-scm.com/docs/git-worktree
- Git LFS: https://git-lfs.com/
- GitHub docs on Git LFS: https://docs.github.com/repositories/working-with-files/managing-large-files/about-git-large-file-storage

## Secrets

- 1Password CLI environment variable injection: https://www.1password.dev/cli/secrets-environment-variables
- Doppler CLI: https://docs.doppler.com/docs/cli
- Infisical CLI: https://infisical.com/docs/cli/usage
- Infisical run command: https://infisical.com/docs/cli/commands/run
- age encryption: https://github.com/FiloSottile/age

## Local database

- SQLite WAL: https://sqlite.org/wal.html
- SQLite PRAGMA statements: https://sqlite.org/pragma.html
- Goose migrations: https://github.com/pressly/goose

## Go CLI

- Cobra CLI framework: https://cobra.dev/
- Viper configuration: https://github.com/spf13/viper

## Architecture audit notes

Exa MCP review on `2026-06-24` supported the existing Go-first architecture for a local daemon/CLI product and identified one spec correction:

- Keep Go + Cobra/Viper for CLI and config layering.
- Use Goose embedded SQL migrations for local SQLite schema management.
- Use OS-native service managers (`launchd`, `systemd`) rather than self-daemonizing.
- Treat watcher events as hints and keep periodic reconciliation.
- Do not claim fsnotify provides FSEvents; fsnotify's current macOS backend is kqueue while FSEvents support remains separate/future.
