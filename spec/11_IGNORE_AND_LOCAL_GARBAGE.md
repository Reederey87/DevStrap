---
last_reviewed: 2026-07-01
tracks_code: [internal/ignore/**, internal/draftbundle/**, internal/scan/**, .gitignore]
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

As of the 2026-06-28 cloud-sync design, this single `.devstrapignore` compiler is **designed and required** (no longer an optional convenience). It is the prerequisite for safe non-git content sync: the draft-bundling layer that ships env vars and non-git/draft folders as age-encrypted, content-addressed `age_blob:<sha256>` blobs must derive its exclusion set from exactly the same compiler that drives scan, the watcher, and the agent deny-list. Any divergence between those consumers can leak a secret or a `node_modules` tree into a draft bundle, so they MUST all read one compiled output rather than maintain separate hardcoded lists (workstream `DRAFT-*` in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`).

## Shipped default table (`internal/ignore` `defaultPatterns`)

This is the table the shipped compiler actually applies when no project `.devstrapignore` is present (`internal/ignore/ignore.go`, `defaultPatterns`):

```gitignore
# VCS internals
.git/

# OS junk
.DS_Store
Thumbs.db
ehthumbs.db
.AppleDouble
.LSOverride
desktop.ini

# Build artifacts
node_modules/
dist/
build/
out/
target/
bin/
obj/
.next/
.nuxt/
.turbo/
.gradle/
.stack-work/
_build/
__pycache__/
.pytest_cache/
.mypy_cache/
.ruff_cache/
.ipynb_checkpoints/

# Virtualenvs
.venv/
venv/
env/

# Coverage / checkpoints
coverage/
.nyc_output/
checkpoints/

# Data conventions
data/raw/
data/interim/

# DevStrap internals
.devstrap/tmp/
.devstrap/cache/
```

> **Warning: the shipped defaults contain NO secret patterns** — no `.env`, `.aws/credentials`, `*.pem`, `id_rsa`, or `id_ed25519`. Do not assume the default policy keeps secrets out of draft bundles; secret exclusion in draft sync is enforced separately by a hardcoded detector (see "Draft sync" below and `P6-XP-06`). The defaults also prune `env/` and `bin/` at any depth, which was the source of the now-fixed `P6-XP-06` scan discovery blind spot: a workspace-root `.devstrapignore` negation (e.g. `!bin/`) now overrides this on the scan path too.

## Recommended per-project `.devstrapignore` (target)

The following is the recommended per-project policy (a target, not the shipped default table above); notably it adds the secret patterns and the ML-artifact conventions the defaults omit:

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

### Git (unbuilt — API exists, no writer)

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

Status: `ignore.GitignoreFragment`/`DefaultGitignoreFragment` exist but have no consumer — no code writes a `# BEGIN DEVSTRAP MANAGED` block (`env capture` appends its own individual entries in `internal/cli/env.go`). This managed-block target is not yet built.

### Docker (unbuilt)

Generate `.dockerignore` block to avoid huge Docker build contexts. Not yet built.

### Draft sync (built)

Use the compiled `.devstrapignore` output directly to exclude files from encrypted draft bundles. This consumer is load-bearing for confidentiality: anything not pruned here is what gets age-encrypted into an `age_blob:<sha256>` blob and pushed to the hub, so the draft-sync exclusion set MUST be the exact compiler output (not a re-derived list) and MUST cover secrets, `node_modules`, build artifacts, and OS junk.

Current state: the compiler output drives directory/artifact exclusion in `draftbundle.Pack`, but secret exclusion is enforced by a separate hardcoded secret-name detector (`draftbundle.isSecretPath`, duplicated from `internal/scan`) because the shipped compiler defaults carry no secret patterns. Folding that detector's patterns into the canonical compiler table — so scan, draft sync, and the future agent denylist read one source — is the open follow-up (extends `PLAT-01`/`AGEN-05`).

### Watcher exclusion set (unbuilt — `PLAT-01`/`PLAT-04`)

Compile the same source into the FSEvents/inotify watcher's exclusion set so the watcher never raises change events for ignored or generated trees. Today the watcher carries its own hardcoded list (`PLAT-01`/`PLAT-04`); it must consume the compiler instead. Not yet built.

### Agent denylist (unbuilt — `AGEN-05`)

Translate secret patterns to agent file-deny policy from the same compiled source (`AGEN-05`). Not yet built.

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

- never descend into `.git`, `node_modules`, `.venv`/`venv`/`env`, `dist`, `build`, `out`, `target`, `bin`, `obj`, `.gradle`, and the other default generated trees (see the shipped default table above). Note: the defaults still prune `env/` and `bin/` at any depth by default, but the scanner now compiles the workspace-root `.devstrapignore` (`P6-XP-06`, shipped), so users can add a negation pattern such as `!bin/` or `!env/` to make repos under those names visible to `scan --adopt` again;
- bound parallelism to `GOMAXPROCS`;
- batch namespace writes in one short `BEGIN IMMEDIATE` transaction per scan batch;
- use mtime/inode markers for incremental rescans;
- treat watcher events as hints and periodic scan as the source of truth;
- benchmark against a large `~/Code` fixture and keep the first visible tree target under 5 minutes.

Current implementation compiles the workspace-root `.devstrapignore` plus defaults before descent, prunes ignored/generated directories, warns on secret-looking filenames, reports symlink escapes, detects duplicate remotes, and has direct scanner coverage plus CLI integration coverage for generated-folder pruning during scan/adopt. Incremental mtime/inode markers, parallel walking, and large benchmark fixtures remain future hardening work.

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

**The single `.devstrapignore` compiler is now built** as `internal/ignore` (DRAFT-03). It compiles *gitignore-inspired* patterns from a project's `.devstrapignore` file plus a canonical default OS-junk/build-artifact table, and feeds the draft-bundle allow-list from one source; a `GitignoreFragment` API exists but has no consumer yet (no code writes a managed `.gitignore` block). The compiler now follows gitignore semantics (`P6-XP-02`, shipped 2026-07-04, differential-tested against `git check-ignore`): it anchors on a leading **or** middle separator, translates bracket classes, degrades an unclosed `[` to a literal, and treats non-standalone `**` as a single `*`. The scanner prune predicate is now fixed too (`P6-XP-06`, shipped 2026-07-04): `scan.Walk` calls `ignore.CompileFromDir` once per walk, offers an `Options.Ignore` test-injection seam, falls back to the default matcher with a warning on compile error, and sources pruning from `internal/ignore`, matching the draft-bundle path. The watcher and agent deny-list still carry some hardcoded entries to be folded in as follow-up.

## Audit follow-ups (2026-06-28)

The 2026-06-28 cloud-sync design **promotes the single `.devstrapignore` compiler from absent to designed-and-required** and makes it a hard dependency of the new non-git content-sync workstream. The "Dropbox experience for code" splits sync strictly by content type — repo content rides git's own blobless clone/fetch from its existing remote and never touches the hub; env vars and non-git/draft folders ship as age-encrypted, content-addressed `age_blob:<sha256>` blobs; `node_modules` and build artifacts are never synced and are rebuilt on hydrate. Because the draft-bundling layer is the only path by which uncontrolled files reach the zero-knowledge hub, its exclusion set MUST be the compiled `.devstrapignore` output and nothing else.

Required follow-ups (workstream `DRAFT-*` in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`):

- build the one canonical compiler and route every consumer through it — `internal/scan`, the draft-bundling/encrypted-blob layer, the platform watcher, and the agent deny-list — retiring the divergent hardcoded lists behind `PLAT-01`, `PLAT-04`, and `AGEN-05`;
- guarantee OS junk (`.DS_Store`, `.AppleDouble`, `Thumbs.db`, `Icon?`, `desktop.ini`) is compiled into every consumer, especially draft sync, so it never enters an encrypted blob or the namespace map;
- treat this compiler as a blocking prerequisite for shipping non-git content sync: no draft bundle is created until its exclusion set is sourced from the compiler.

## Pass 6 audit recommendations (2026-07-01)

From the sixth-pass audit (`docs/audits/AUDIT_RECOMMENDATIONS_2026-07-01_PASS6.md`); IDs link to full evidence there.

### P6-XP-01 — `ShouldPruneDir` bare-name fallback defeats anchored and negation patterns — SHIPPED

**Resolved.** `ShouldPruneDir` (`internal/ignore/ignore.go`) no longer re-evaluates patterns against a directory's bare name as a fallback; `relSlash` is now the single, authoritative match target, with the empty-path guard (`relSlash == "" -> name`) kept only for the theoretical case of a caller with no path at all:

```go
func (m *Matcher) ShouldPruneDir(name, relSlash string) bool {
    if m == nil {
        return DefaultMatcher().ShouldPruneDir(name, relSlash)
    }
    if relSlash == "" {
        relSlash = name
    }
    return m.Match(relSlash, true)
}
```

Both live callers (`scan.Walk` and `draftbundle.Pack`) already compute `relSlash`/`rel` via `filepath.Rel` against their respective walk root for every non-root directory, so no caller changes were needed. Root-anchored patterns (`/dist/`) no longer prune nested directories that merely share a base name, and a negation re-including a nested path (`!keep/build/`) is honored instead of silently defeated. Regression coverage: `TestShouldPruneDirAnchoredPatternDoesNotPruneNested`, `TestShouldPruneDirNegationReincludes`, `TestShouldPruneDirRootLevelStillPruned` (`internal/ignore/ignore_test.go`).

### P6-XP-02 — Ignore compiler diverges from the gitignore semantics it advertises — **SHIPPED 2026-07-04 (`fix/p6-xp-02`)**

**Resolved.** `parseLine` now anchors on a leading **or middle** separator (`anchored = hasLeadingSlash || strings.Contains(body, "/")`); `patternToRegex` translates bracket classes into real regex classes (leading `!`/`^` → `[^…]`, escaping `\`/`]`) and **degrades an unclosed `[` to a literal `\[`** instead of failing `Compile`; and `**` only crosses `/` when it is a standalone segment (slash-bounded on both sides), so `a**b` matches like a single `*`. A `git check-ignore --verbose` differential test (`ignore_gitdiff_test.go`, skipped when git is absent) pins agreement with real git over the middle-slash, bracket, and `a**b` corpus. The built-in default patterns with a middle slash (`data/raw/`, `data/interim/`, `.devstrap/tmp/`, `.devstrap/cache/`) were rewritten with an explicit `**/` prefix so they keep pruning at any depth (project-level, not just the workspace root) under the corrected anchoring — a behavior-preserving change (`TestMatchDefaults`, `internal/scan` `TestShouldPruneDir`). User-authored patterns follow exact git anchoring.

**Original problem (now fixed).** The compiler's doc header claimed "Pattern semantics follow .gitignore," but `parseLine` anchored only on a *leading* `/`, `patternToRegex` omitted `[`/`]` from its escape set so `[!a]log` matched the wrong set, and one unclosed `[` made `Compile` fail the *whole file*, hard-failing `devstrap draft snapshot create`.

**Actionable steps (done).**
1. Change `parseLine` to set `anchored = strings.Contains(body, "/")`.
2. Rewrite bracket-class handling to a proper regex class with correct negation, and degrade an unclosed `[` to a literal match instead of failing `Compile`.
3. Fix `**` so it only crosses `/` when explicitly slash-bounded on both sides.
4. Add a `git check-ignore --verbose` differential test (skipped when git is absent) over middle-slash, bracket, and `a**b` patterns.

```go
body := strings.TrimSuffix(strings.TrimPrefix(raw, "!"), "/")
p.anchored = strings.Contains(body, "/")
// bracket classes: map leading '!'/'^' to '[^...]', escape '\', and
// fall back to a literal '\[' when unclosed instead of failing Compile.
// '**' not slash-bounded on both sides -> '[^/]*' (regular *), not '.*'.
```

### P6-XP-06 — Scanner hardwires the defaults-only ignore matcher, skipping repos under `env/`/`bin/`/`build/` — SHIPPED 2026-07-04 (`fix/p6-xp-06`)

**Resolved.** `scan.Options` now has an `Ignore *ignore.Matcher` seam for tests, and `scan.Walk` compiles the workspace root's `.devstrapignore` once per walk via `ignore.CompileFromDir(cleanRoot, true)` when that seam is nil. A malformed ignore file emits a compile-failure warning and falls back to `ignore.DefaultMatcher()`, so default generated-tree pruning remains fail-safe. The old package-level defaults-only matcher and scan-local `shouldPruneDir` shim are gone; directory pruning now uses the per-walk matcher and counts pruned directories into `Result.PrunedDirs`, which the interactive `scan` surfaces as ONE informational line (deliberately not a `Result.Warnings` entry: `run-loop` prints scan warnings every tick, and routine default prunes like `node_modules` would become permanent per-tick chatter — the exact class `P6-CLI-04` removed). Compile failures stay real warnings. Re-include a pruned dir with a root-`.devstrapignore` negation (e.g. `!bin/`). Regression coverage: `TestWalkCompilesDevstrapignoreAndPrunesCustomPatternWithDefaults`, `TestWalkMalformedDevstrapignoreWarnsAndFallsBackToDefaults`, and `TestWalkDevstrapignoreNegationReincludesDefaultPrunedDirectory` (`internal/scan/scan_test.go`).
