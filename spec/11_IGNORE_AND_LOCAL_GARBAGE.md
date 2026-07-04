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

> **Warning: the shipped defaults contain NO secret patterns** — no `.env`, `.aws/credentials`, `*.pem`, `id_rsa`, or `id_ed25519`. Do not assume the default policy keeps secrets out of draft bundles; secret exclusion in draft sync is enforced separately by a hardcoded detector (see "Draft sync" below and `P6-XP-06`). The defaults also prune `env/` and `bin/` at any depth, which is the source of the `P6-XP-06` scan discovery blind spot.

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

- never descend into `.git`, `node_modules`, `.venv`/`venv`/`env`, `dist`, `build`, `out`, `target`, `bin`, `obj`, `.gradle`, and the other default generated trees (see the shipped default table above). Note: because the defaults prune `env/` and `bin/` at any depth and the scanner currently ignores the project `.devstrapignore` (defaults-only matcher — `configured ignored directories` is not yet implemented on the scan path), repos under those names are invisible to `scan --adopt` — see `P6-XP-06` below;
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

**The single `.devstrapignore` compiler is now built** as `internal/ignore` (DRAFT-03). It compiles *gitignore-inspired* patterns from a project's `.devstrapignore` file plus a canonical default OS-junk/build-artifact table, and feeds the draft-bundle allow-list from one source; a `GitignoreFragment` API exists but has no consumer yet (no code writes a managed `.gitignore` block). The compiler now follows gitignore semantics (`P6-XP-02`, shipped 2026-07-04, differential-tested against `git check-ignore`): it anchors on a leading **or** middle separator, translates bracket classes, degrades an unclosed `[` to a literal, and treats non-standalone `**` as a single `*`. Also, the scanner prune predicate does **not** yet read the project's `.devstrapignore` at all — `scan.Walk` hardwires the defaults-only matcher (see `P6-XP-06`), so only the defaults half of "feeds the scanner prune predicate" is currently true. The watcher and agent deny-list still carry some hardcoded entries to be folded in as follow-up.

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

### P6-XP-06 — Scanner hardwires the defaults-only ignore matcher, skipping repos under `env/`/`bin/`/`build/`

**Problem.** `scan.go:191` declares `var pruneMatcher = ignore.DefaultMatcher()` and `scan.Walk` never calls `ignore.CompileFromDir`, so the per-project `.devstrapignore` is ignored on the discovery path. Because the prune check (`scan.go:111`) runs before `dsgit.IsRepo` (`scan.go:131`) and the defaults prune `env/`/`bin/`/`build/`/`dist/`/`out/`/`target/` at any depth, a repo at `~/Code/env/...` is skipped with no `Finding` or warning; `init --scan` (`internal/cli/init.go:106`) shares the blind spot.

**Actionable steps.**
1. Call `ignore.CompileFromDir(root, true)` in `scan.Walk`, falling back to `DefaultMatcher()` with a warning on error.
2. Add an `Options.Ignore *ignore.Matcher` field for test injection.
3. Count pruned directories and emit one summary warning pointing users to add negations in `~/Code/.devstrapignore`.
4. Wire the same compiled matcher through `init.go:106`'s `scan.Walk` call.

```go
m, err := ignore.CompileFromDir(cleanRoot, true)
if err != nil {
    result.Warnings = append(result.Warnings, fmt.Sprintf("ignore compile failed, using defaults: %v", err))
    m = ignore.DefaultMatcher()
}
// thread m through as Options.Ignore for test injection
```
