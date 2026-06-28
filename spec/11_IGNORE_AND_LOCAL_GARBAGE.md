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
.devstrapignore → .gitignore, .dockerignore, draft-sync ignore, watcher exclusion set, agent denylist
```

As of the 2026-06-28 cloud-sync design, this single `.devstrapignore` compiler is **designed and required** (no longer an optional convenience). It is the prerequisite for safe non-git content sync: the draft-bundling layer that ships env vars and non-git/draft folders as age-encrypted, content-addressed `age_blob:<sha256>` blobs must derive its exclusion set from exactly the same compiler that drives scan, the watcher, and the agent deny-list. Any divergence between those consumers can leak a secret or a `node_modules` tree into a draft bundle, so they MUST all read one compiled output rather than maintain separate hardcoded lists (workstream `DRAFT-*` in `AUDIT_RECOMMENDATIONS_2026-06-28.md`).

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

# OS/editor (OS junk is compiled into every consumer, including draft sync)
**/.DS_Store
**/.AppleDouble
**/.LSOverride
**/Icon?
**/Thumbs.db
**/desktop.ini
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

Use the compiled `.devstrapignore` output directly to exclude files from encrypted draft bundles. This consumer is load-bearing for confidentiality: anything not pruned here is what gets age-encrypted into an `age_blob:<sha256>` blob and pushed to the hub, so the draft-sync exclusion set MUST be the exact compiler output (not a re-derived list) and MUST cover secrets, `node_modules`, build artifacts, and OS junk.

### Watcher exclusion set

Compile the same source into the FSEvents/inotify watcher's exclusion set so the watcher never raises change events for ignored or generated trees. Today the watcher carries its own hardcoded list (`PLAT-01`/`PLAT-04`); it must consume the compiler instead.

### Agent denylist

Translate secret patterns to agent file-deny policy from the same compiled source (`AGEN-05`).

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

**The single `.devstrapignore` compiler is now built** as `internal/ignore` (DRAFT-03). It compiles gitignore-compatible patterns from a project's `.devstrapignore` file plus a canonical default OS-junk/build-artifact table, and feeds the scanner prune predicate, the draft-bundle allow-list, and generated `.gitignore` fragments from one source. The watcher and agent deny-list still carry some hardcoded entries to be folded in as follow-up.

## Audit follow-ups (2026-06-28)

The 2026-06-28 cloud-sync design **promotes the single `.devstrapignore` compiler from absent to designed-and-required** and makes it a hard dependency of the new non-git content-sync workstream. The "Dropbox experience for code" splits sync strictly by content type — repo content rides git's own blobless clone/fetch from its existing remote and never touches the hub; env vars and non-git/draft folders ship as age-encrypted, content-addressed `age_blob:<sha256>` blobs; `node_modules` and build artifacts are never synced and are rebuilt on hydrate. Because the draft-bundling layer is the only path by which uncontrolled files reach the zero-knowledge hub, its exclusion set MUST be the compiled `.devstrapignore` output and nothing else.

Required follow-ups (workstream `DRAFT-*` in `AUDIT_RECOMMENDATIONS_2026-06-28.md`):

- build the one canonical compiler and route every consumer through it — `internal/scan`, the draft-bundling/encrypted-blob layer, the platform watcher, and the agent deny-list — retiring the divergent hardcoded lists behind `PLAT-01`, `PLAT-04`, and `AGEN-05`;
- guarantee OS junk (`.DS_Store`, `.AppleDouble`, `Thumbs.db`, `Icon?`, `desktop.ini`) is compiled into every consumer, especially draft sync, so it never enters an encrypted blob or the namespace map;
- treat this compiler as a blocking prerequisite for shipping non-git content sync: no draft bundle is created until its exclusion set is sourced from the compiler.
