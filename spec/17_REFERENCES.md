---
last_reviewed: 2026-06-28
tracks_code: [.github/**, go.mod, go.sum, AUDIT_RECOMMENDATIONS.md, AUDIT_RECOMMENDATIONS_2026-06-27.md, AUDIT_RECOMMENDATIONS_2026-06-28.md]
---
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

- Go keyring adapter: https://github.com/zalando/go-keyring
- 1Password CLI environment variable injection: https://www.1password.dev/cli/secrets-environment-variables
- 1Password CLI `inject` command: https://www.1password.dev/cli/reference/commands/inject
- Doppler CLI: https://docs.doppler.com/docs/cli
- Infisical CLI: https://infisical.com/docs/cli/usage
- Infisical run command: https://infisical.com/docs/cli/commands/run
- age encryption: https://github.com/FiloSottile/age

## Local database

- SQLite WAL: https://sqlite.org/wal.html
- SQLite PRAGMA statements: https://sqlite.org/pragma.html
- Goose migrations: https://github.com/pressly/goose
- Go + SQLite best practices: https://jacob.gold/posts/go-sqlite-best-practices/
- River SQLite guidance: https://riverqueue.com/docs/sqlite

## Go CLI

- Cobra CLI framework: https://cobra.dev/
- Viper configuration: https://github.com/spf13/viper
- golangci-lint documentation: https://golangci-lint.run/
- golangci-lint GitHub Action: https://github.com/golangci/golangci-lint-action
- gosec linter: https://github.com/securego/gosec

## Documentation and spec drift

- Docs-gate pattern for changed-file classification and CI gating: https://github.com/sarvesh-ghl/docs-gate
- CI documentation drift workflow tradeoffs and path-filter caution: https://dosu.dev/blog/how-to-catch-documentation-drift-claude-code-github-actions
- Documentation drift gate discussion: https://blog.sarvesh.pro/the-ci-check-that-forces-your-docs-to-keep-up-with-your-code/

## Local-first sync

- Notes on local-first and HLC ordering: https://www.sandromaglione.com/articles/notes-on-local-first
- Clock systems for sync: https://agenticdevelopercookbook.com/guidelines/planning/data/clock-systems
- JSON offline-first sync and tombstones: https://jsonic.io/guides/json-offline-sync
- Syncthing Block Exchange Protocol: https://docs.syncthing.net/specs/bep-v1.html

## Security and supply chain

- OWASP Secrets Management Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html
- OWASP Top Ten: https://owasp.org/www-project-top-ten/
- GitHub Actions secure use reference: https://docs.github.com/en/actions/reference/security/secure-use
- GitHub default branch rename guidance: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-branches-in-your-repository/renaming-a-branch
- Go security best practices: https://go.dev/doc/security/

## Future web/admin surfaces

- Microsoft Azure Architecture Center, Modern Web App pattern: https://aka.ms/eap/mwa/dotnet/doc
- Web.dev Core Web Vitals: https://web.dev/vitals/
- WCAG 2.2: https://www.w3.org/TR/WCAG22/
- OpenTelemetry: https://opentelemetry.io/docs/
- DORA metrics overview: https://dora.dev/guides/dora-metrics-four-keys/

## Architecture audit notes

Exa MCP review on `2026-06-24` supported the existing Go-first architecture for a local daemon/CLI product and identified one spec correction:

- Keep Go + Cobra/Viper for CLI and config layering.
- Use Goose embedded SQL migrations for local SQLite schema management.
- Use OS-native service managers (`launchd`, `systemd`) rather than self-daemonizing.
- Treat watcher events as hints and keep periodic reconciliation.
- Do not claim fsnotify provides FSEvents; fsnotify's current macOS backend is kqueue while FSEvents support remains separate/future.

Exa MCP review on `2026-06-24` also supported these updates:

- Local-first sync should avoid raw wall-clock conflict ordering; use HLC or vector-clock-style causal markers, idempotent apply, per-peer cursors, and tombstones.
- Go + SQLite daemon designs should use WAL, busy timeouts, per-connection foreign key pragmas, and controlled writer concurrency.
- Secret handling for agents should prefer references, short-lived scoped access, append-only audit trails, and redaction at process/log boundaries.
- Any future web/admin surface should be modular, server-first/API-first, accessible to WCAG 2.2 AA, measured by Core Web Vitals, secured against OWASP risks, and observable through logs/metrics/traces.

Exa MCP review on `2026-06-26` supported provider-reference hydration behavior:

- Prefer 1Password `op run --env-file` for runtime-scoped environment injection.
- Use `op inject --in-file --out-file --file-mode 0600` for explicit file hydration, and delete resolved files when no longer needed.
- Keep provider reference files in source control or local state as references only; resolved plaintext files remain explicit, `0600`, and gitignored.

Exa MCP review on `2026-06-26` supported OS-backed device-key storage behavior:

- Prefer OS keychain/Secret Service for local private keys and use `0600` file storage only as a clearly documented fallback for unsupported/headless systems.
- Keep private device identities out of SQLite, config files, logs, and command output.
- Test keychain code through mocked keyring backends so automated tests never touch the user's real keychain.

Exa MCP review on `2026-06-26` supported agent/watcher hardening behavior:

- Treat local agent filesystem policy as layered defense: wrapper-level allow/deny checks help, but sensitive-path and outside-worktree controls eventually need OS sandboxing for strong enforcement.
- Default agent workspace access should be worktree-scoped, with explicit personal override for broad local access.
- Watcher implementations should coalesce bursty editor/indexer events and feed reconciliation, not mutate state directly from low-level events.
- `gh pr create` tests should be hermetic by using fake/stub executables or explicit command interfaces for non-dry command coverage.

## Audit follow-ups (2026-06-27) — added references

- Git FAQ — never sync a repository via a file-sync service: https://git-scm.com/docs/gitfaq
- git-bundle: https://git-scm.com/docs/git-bundle
- jujutsu (auto-committed working copy): https://github.com/martinvonz/jj
- Mutagen: https://mutagen.io/documentation/synchronization/ ; Syncthing: https://docs.syncthing.net/users/syncing
- age: https://github.com/FiloSottile/age ; SOPS: https://github.com/getsops/sops
- govulncheck: https://go.dev/security/vuln/
- git-town forge drivers: https://www.git-town.com/preferences/forge-type
- Server-Sent Events: https://html.spec.whatwg.org/multipage/server-sent-events.html
- Caddy automatic HTTPS: https://caddyserver.com/docs/automatic-https ; Tailscale: https://tailscale.com/
- Full per-finding sources: `AUDIT_RECOMMENDATIONS_2026-06-27.md`.

## Cloud-sync architecture (2026-06-28) — added references

These sources back the cloud-sync architecture cycle: the "Dropbox experience for code" (one identical `~/Code` tree on every device in the owner's fleet), the content-type-split sync model (git transport for repo content, age blobs for env/draft content, a signed HLC event log for the namespace map), eager clone-everything materialization, and the two-plane zero-knowledge hub. They drive the EAGER-*, DRAFT-*, HUB-*, XP-*, and SCALE-* workstreams in `AUDIT_RECOMMENDATIONS_2026-06-28.md`.

### Never file-sync a git repo

- Git FAQ — a working tree / `.git` directory must never be sync'd by a file-sync service (it corrupts the repo); replicate via git transport instead: https://git-scm.com/docs/gitfaq (also cited above for the 2026-06-27 follow-ups).
- Blobless/partial clone as the repo-content transport (`git clone --filter=blob:none`): https://git-scm.com/docs/partial-clone ; https://github.blog/2020-12-21-get-up-to-speed-with-partial-clone-and-shallow-clone/

### Dropbox / Drive system design (two-plane, content-addressed, change feed, dual-copy)

- Dropbox sync engine rewrite (Nucleus) — split metadata plane from content plane, content-addressed blocks: https://dropbox.tech/infrastructure/rewriting-the-heart-of-our-sync-engine
- Dropbox cursor-based change feed (`list_folder` / `list_folder/continue`): https://www.dropbox.com/developers/documentation/http/documentation#files-list_folder-continue
- Google Drive API change feed (start page token + incremental changes): https://developers.google.com/workspace/drive/api/guides/manage-changes
- Dropbox "conflicted copy" — dual-copy as the safe conflict default for opaque files (never byte-merge): https://help.dropbox.com/sync/conflicted-copy

### Zero-knowledge / content-addressed encrypted sync engines

- secsync — end-to-end-encrypted CRDT/document sync (client holds keys, server stores ciphertext): https://github.com/serenity-kit/secsync
- Tahoe-LAFS — provider-independent, zero-knowledge content-addressed storage grid: https://tahoe-lafs.org/
- Content addressing (encrypt-then-hash; immutable `<sha256>` blob keys): https://docs.ipfs.tech/concepts/content-addressing/
- Convergent / content-derived encryption background: https://en.wikipedia.org/wiki/Convergent_encryption

### age encryption (multi-recipient, rotation, no native revocation)

- age project: https://github.com/FiloSottile/age (also cited above)
- age v1 format spec — multi-recipient stanzas; revocation means re-encrypting to the reduced recipient set (no native revoke), so revoke flags secrets for rotation: https://age-encryption.org/v1

### Object storage backends (pluggable Hub blob store)

- Cloudflare R2 — chosen from the start: S3-compatible API, zero egress fees, namespaced by `workspace_id`: https://developers.cloudflare.com/r2/ ; pricing/egress: https://developers.cloudflare.com/r2/pricing/
- Cloudflare R2 consistency and S3 compatibility — strong global consistency for object writes/listing; conditional puts and paged listing are available, but append-only semantics are a DevStrap object-key/hash-chain responsibility: https://developers.cloudflare.com/r2/reference/consistency/ ; https://developers.cloudflare.com/r2/api/s3/api/
- Cloudflare R2 temporary credentials — hosted clients/runners should receive short-lived bucket/prefix/operation-scoped credentials instead of bucket-wide long-lived keys: https://developers.cloudflare.com/r2/api/s3/temporary-credentials/
- Cloudflare R2 data location and jurisdictions — bucket location/jurisdiction is a provisioning decision and may not be changeable later: https://developers.cloudflare.com/r2/reference/data-location/
- Tigris — Fly-native S3-compatible object storage alternative with zero egress/global placement tradeoffs: https://www.tigrisdata.com/pricing/ ; https://fly.io/docs/tigris/
- Backblaze B2 (S3-compatible): https://www.backblaze.com/docs/cloud-storage-s3-compatible-api
- Amazon S3 API reference: https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html
- MinIO (self-hostable, S3-compatible; useful for a non-cloud Hub backend): https://min.io/docs/minio/linux/index.html

### Agent-runner sandboxes (microVM isolation for the future control plane)

- Fly Machines — chosen compute: Firecracker microVMs, global regions, scale-to-zero/suspend-resume, runs the Go binary natively: https://fly.io/docs/machines/ ; regions: https://fly.io/docs/reference/regions/ ; pricing: https://fly.io/docs/about/pricing/
- Fly app secrets and suspend/resume — app-wide secrets are injected into Machines; runner apps must receive only per-task scoped credentials, and destroy-after-task is safer than suspending untrusted tasks with memory state: https://fly.io/docs/apps/secrets/ ; https://fly.io/docs/reference/suspend-resume/
- E2B — self-hostable microVM agent sandboxes (runner escape-hatch): https://e2b.dev/docs
- Modal sandboxes: https://modal.com/docs/guide/sandbox
- Daytona dev-environment runtime: https://www.daytona.io/docs/
- Firecracker — the microVM technology behind AWS Lambda and Fargate: https://firecracker-microvm.github.io/
- Vercel Sandbox (strong for a Next.js/TS stack; awkward for Go-first): https://vercel.com/docs/vercel-sandbox
- Coder — reference architecture for agents/dev workspaces on your own infra at scale: https://coder.com/docs

### Multi-tenant SaaS scaling (future direction)

- Control plane vs. application/data plane split: https://docs.aws.amazon.com/whitepapers/latest/saas-architecture-fundamentals/control-plane-vs.-application-plane.html
- SaaS tenant-isolation strategies (pooled → siloed/dedicated → BYOC tenancy spectrum): https://docs.aws.amazon.com/whitepapers/latest/saas-tenant-isolation-strategies/saas-tenant-isolation-strategies.html
- Cell-based architecture (reducing scope of impact): https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/reducing-scope-of-impact-with-cell-based-architecture.html
- Managed Postgres options for the control-plane DB: Neon pricing/plans/scale-to-zero/connection pooling (`pooled` runtime DSN vs direct migration/admin DSN): https://neon.com/pricing ; https://neon.com/docs/introduction/plans ; https://neon.com/docs/introduction/scale-to-zero ; https://neon.com/docs/connect/connection-pooling
- Supabase managed Postgres/BaaS alternative: https://supabase.com/pricing ; https://supabase.com/docs/guides/database
- Render and Railway app-hosting alternatives for simpler trusted deployments: https://render.com/pricing ; https://railway.com/pricing
- Cloudflare Workers/Durable Objects/D1 + R2 alternative for a future serverless edge control/hub layer if the project accepts a non-Go edge runtime: https://developers.cloudflare.com/workers/platform/pricing/ ; https://developers.cloudflare.com/durable-objects/platform/pricing/ ; https://developers.cloudflare.com/d1/platform/pricing/
- Full per-finding sources: `AUDIT_RECOMMENDATIONS_2026-06-28.md`.
