---
last_reviewed: 2026-06-28
tracks_code: [internal/scan/**, .gitignore]
---
# Ignore Rules and Local Garbage

## Problem

A developer Dropbox cannot sync everything.

Dangerous or noisy folders:

- `.git` internals;
- `.env` files;
- `node_modules`;
- `.venv`;
- build output;
- caches;
- model/data artifacts;
- OS metadata;
- editor machine state;
- agent scratch files.

## Principle

DevStrap needs one canonical ignore policy that can compile to multiple systems.

```text
.devstrapignore → .gitignore, .dockerignore, draft-sync ignore, agent denylist
```

## Default `.devstrapignore`

```gitignore
# Secrets
**/.env
**/.env.*
!**/.env.example
!**/.env.template
!**/.env.schema
**/.snowflake/config.toml
**/.aws/credentials
**/*service-account*.json
**/*.pem
**/id_rsa
**/id_ed25519

# Python
**/.venv/
**/venv/
**/__pycache__/
**/.pytest_cache/
**/.mypy_cache/
**/.ruff_cache/
**/.ipynb_checkpoints/

# Node
**/node_modules/
**/.next/
**/.turbo/
**/dist/
**/build/
**/coverage/

# Rust/Go/Java
**/target/
**/bin/
**/.gradle/
**/build/

# Data / ML artifacts
**/data/raw/
**/data/interim/
**/models/*.pkl
**/models/*.joblib
**/models/*.onnx
**/checkpoints/

# OS/editor
**/.DS_Store
**/Thumbs.db
**/.idea/workspace.xml
**/.vscode/.ropeproject/

# DevStrap internals
**/.devstrap/tmp/
**/.devstrap/cache/
```

## Ignore compiler targets

### Git

Generate or update `.gitignore` safely.

Rules:

- do not overwrite user file;
- insert managed block;
- preserve custom rules.

Managed block:

```gitignore
# BEGIN DEVSTRAP MANAGED
...
# END DEVSTRAP MANAGED
```

### Docker

Generate `.dockerignore` block to avoid huge Docker build contexts.

### Draft sync

Use `.devstrapignore` directly to exclude files from encrypted draft bundles.

### Agent denylist

Translate secret patterns to agent file-deny policy.

## OS-specific local garbage

Mac:

```text
.DS_Store
.AppleDouble
Icon?
.fseventsd if inside external volumes
```

Linux:

```text
.Trash-*
.nfs*
```

Windows future:

```text
Thumbs.db
desktop.ini
```

## Native dependency strategy

Never sync:

```text
node_modules
.venv
target
build
dist
```

Instead, tooling profiles run:

```bash
uv sync
npm ci
pnpm install
cargo build
```

## Scan scale rules

`devstrap scan --adopt` must prune ignored and generated trees during the filesystem walk, not after collecting all paths.

Rules:

- never descend into `.git` internals, `node_modules`, `.venv`, `dist`, `build`, `target`, `.gradle`, or configured ignored directories;
- bound parallelism to `GOMAXPROCS`;
- batch namespace writes in one short `BEGIN IMMEDIATE` transaction per scan batch;
- use mtime/inode markers for incremental rescans;
- treat watcher events as hints and periodic scan as the source of truth;
- benchmark against a large `~/Code` fixture and keep the first visible tree target under 5 minutes.

Current implementation prunes the default generated directories before descent, warns on secret-looking filenames, reports symlink escapes, detects duplicate remotes, and has direct scanner coverage plus CLI integration coverage for generated-folder pruning during scan/adopt. Incremental mtime/inode markers, configured ignore files, parallel walking, and large benchmark fixtures remain future hardening work.

## Large artifact strategy

Rules:

- if repo needs large tracked binaries, use Git LFS;
- if repo needs datasets/models, use DVC/object storage;
- if local-only, ignore;
- if small draft artifact, encrypted draft sync with size cap.

## Secret detection during scan

DevStrap should scan file names and optionally content patterns.

Filename warnings:

```text
.env
.env.production
credentials.json
service-account.json
*.pem
id_rsa
id_ed25519
```

Output:

```text
⚠ Secret-looking file found: work/acme/api/.env
Action: capture encrypted env, ignore file, or leave unmanaged.
```

## Policy levels

```text
strict     company/team projects
normal     default personal projects
loose      experiments, explicit opt-in
```

Strict:

- no plaintext `.env`;
- env schema required;
- dependencies ignored;
- agent denylist enforced.

Normal:

- warnings for plaintext `.env`;
- encrypted capture allowed;
- generated ignores inserted.

Loose:

- less enforcement;
- still block private keys by default.

## Audit follow-ups (2026-06-27)

**The single `.devstrapignore` compiler described here does not exist yet** (audit coverage gap). Prune/secret lists are hardcoded and divergent across three places — `internal/scan` (`shouldPruneDir`/`isSecretName`), the platform watcher, and the agent deny list — which is the root cause of `PLAT-01`, `PLAT-04`, and `AGEN-05`; OS junk (`.DS_Store`, `.AppleDouble`, `Thumbs.db`) is filtered nowhere. Build one canonical compiler that emits the `.gitignore` managed block, the draft-sync ignore set, the watcher exclusion set, and the agent denylist from a single source.
