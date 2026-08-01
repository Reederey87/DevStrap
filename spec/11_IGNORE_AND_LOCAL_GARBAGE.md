---
last_reviewed: 2026-08-01
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

**Why DevStrap's own staging directories are in the default set (`W12-01`, 2026-07-31).** `hydrate` clones into a sibling `.<target>.devstrap-tmp-<random>` and promotes it by rename on success. A process killed before that rename — a daemon crash, a power loss, a `kill -9` — leaves a **partial clone inside the managed namespace, carrying the real remote in its `.git/config`**. Until this pattern was added the scanner walked straight into it: `scan --adopt` adopted the orphan as a *second* project sharing the real project's remote (reproduced: `Adopted 2 projects`), which then replicates to every device as a namespace event — and the duplicate-remote resolver **recommended the orphan**, because `.` sorts before a letter.

Note the marker sits in the **middle** of the name, not at its start, so a prefix match does not find it. The pattern and the directory name both derive from `ignore.StagingDirMarker` for that reason: two spellings of one fact will drift, and the drift is silent. `TestCloneTempDirIsPrunedByTheScanner` calls the real `cloneTempDir` and asserts the scanner prunes what it actually produced, so a naming change that outruns the pattern fails the build rather than quietly reopening the hole.

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

# DevStrap clone-staging directories (W12-01)
*.devstrap-tmp-*/

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
**/.devstrap/tmp/
**/.devstrap/cache/
```

> **Warning: the shipped defaults contain NO secret patterns** — no `.env`, `.aws/credentials`, `*.pem`, `id_rsa`, or `id_ed25519`. Do not assume the default policy keeps secrets out of draft bundles; secret exclusion is enforced separately by `ignore.IsSecretName`/`IsSecretPath`, a membership test alongside the table rather than an entry in it, and deliberately so (see "Draft sync" below and `P6-XP-06`). The defaults also prune `env/` and `bin/` at any depth, which was the source of the now-fixed `P6-XP-06` scan discovery blind spot: a workspace-root `.devstrapignore` negation (e.g. `!bin/`) now overrides this on the scan path too.

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

Use the compiled `.devstrapignore` output directly to exclude files from encrypted draft bundles. This consumer is load-bearing for confidentiality: anything not pruned here is what gets age-encrypted into an `age_blob:<sha256>` blob and pushed to the hub. The effective exclusion policy is therefore **the exact compiler output plus `ignore.IsSecretPath`** — never a re-derived list — and between them the two must cover secrets, `node_modules`, build artifacts, and OS junk. The split is deliberate and explained below: the compiler's `defaultPatterns` deliberately carry no secret names, because a bundle must *refuse* on a secret rather than silently skip it.

Current state: the compiler output drives directory/artifact exclusion in `draftbundle.Pack`, and secret exclusion now reads the canonical `ignore.IsSecretPath`. The two hardcoded clones — `draftbundle.isSecretPath` and `internal/scan`'s `isSecretName` — are retired. They were not literally byte-identical (different signatures, and one took the basename as a separate argument while the other derived it with `filepath.Base`), but they applied **equivalent detection rules** for the slash-normalized paths both call sites actually pass. Their shared table now lives in `internal/ignore`, with the scanner's own case-for-case test moved alongside it, plus a differential test asserting the unified function agrees with verbatim copies of *both* retired implementations across a hostile input space (empty paths, trailing slashes, native separators, root-level anchored-suffix cases).

**The secret names are a membership test (`IsSecretName`/`IsSecretPath`), deliberately NOT entries in the compiler's `defaultPatterns`** — the same shape as `IsOSJunkName`, for a stronger reason. Both consumers use this judgement to **act**, not to skip: `internal/scan` *reports* a hit into `Warnings` and `Secrets` so the user learns a secret is sitting in the tree, and `draftbundle.Pack` *hard-refuses* the bundle with a named error, ordered after `matcher.Match` so a user-ignored secret is skipped quietly instead. Folding `.env` into the ignore table would silently convert the first into prune-and-never-report — destroying the signal that is the entire point — and the second into a silent skip.

The original behavior-neutral unification preserved one leading-slash gap. `AGEN-05` closes it deliberately: directory-anchored suffix matching evaluates `"/"+relSlash`, so `.snowflake/config.toml` and `.aws/credentials` are detected even when they sit directly at the scan or bundle root. The differential test still compares every other input with both retired implementations and encodes these two intended divergences explicitly.

### Watcher exclusion set (built — `PLAT-01`/`PLAT-04`)

The fsnotify watcher now compiles the watch root's `.devstrapignore` with the canonical defaults once per `Watch`, passes true root-relative slash paths into `ShouldPruneDir`, and applies the same matcher to initial registration and to create/rename registration. A compile/read failure falls back to `DefaultMatcher`, so callers that construct `NativeWatcher{}` keep the default policy and a malformed user file cannot disable pruning altogether. Rename/delete events best-effort remove the affected watch subtree **including descendants** — the leak that actually mattered, since the kernel drops a renamed directory's own watch but keeps every watch below it. `PLAT-01` and `PLAT-04` are built.

**Two layers, and the split is load-bearing rather than an inconsistency.** The compiler's defaults answer *"is this project content?"* — they drive `internal/scan` adoption and what rides a draft bundle, so adding to them changes what gets **synced**. A small watcher-local layer answers only *"is it worth a watch descriptor, or a wakeup?"*, a budget question local to that adapter. `M5D-06` therefore changed **nothing** in the canonical defaults.

- **Directories the watcher skips that the compiler does not:** `vendor/`, `.devstrap/`, `.Trash/`. `vendor/` is committed source in many Go projects, and putting it in the canonical set would silently drop it from a non-git draft folder's bundle; skipping it in the watcher costs only hints for files that change when dependencies are re-vendored, which the periodic cycle catches anyway. `.devstrap/` is DevStrap's own state home — the compiler prunes its `tmp/` and `cache/` subtrees at any depth (`P6-XP-02`) and keeps the rest visible on purpose, while the watcher has no reason to watch any of it. `.Trash/` is the OS's, not the project's.
- **Event noise the watcher drops unconditionally:** `Chmod`-only events (they cannot change the namespace) and OS/editor scratch *by name* — `.DS_Store`, `Thumbs.db`, `desktop.ini`, `.AppleDouble`, `.LSOverride`, `4913`, trailing-`~` backups, and emacs `#foo#`/`.#foo` files — dropped **before** the debounce and the 5s trigger floor, so a save-storm's worth of backup files cannot spend the floor a real change needs.

  Two properties of that second bullet are deliberate. It does **not** run through the matcher, because the matcher applies the user's `.devstrapignore`, and a negation (`!*~`) would switch noise filtering back on — asking for `foo~` to be *synced* is a content choice, not a request to be woken every time an editor writes a backup. And `*~` must not be canonical content policy at all: unlike the fixed names beside it, it is a glob over **user filenames**, so putting it in the defaults would silently stop syncing a draft legitimately named `proposal~` — the same class of mistake as pruning `vendor/` there. Dropping a hint is safe by construction: a hint is an optimization, so the cost is at most one interval of latency before periodic convergence notices, and the file itself still syncs.

### Agent denylist (built — `AGEN-05`)

`internal/ignore` now exposes defensive-copy `CredentialHomeDirs` and `CredentialHomeFiles` membership lists. The agent builds its slash-prefixed `denyParts` from them, while the sandbox sources `sensitiveHomeDirs`/`sensitiveHomeFiles` from the same APIs; Seatbelt, Landlock, bubblewrap, and read-allow conflict anchors inherit the table. Agent basename checks reuse `IsSecretName`, including the checked-in `.env` template exceptions. `*.key` remains in a documented agent-only predicate because it is a glob over user filenames, not a fixed credential name: making it canonical would report ordinary `en-US.key` files and hard-refuse a whole draft bundle over one fixture.

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

## Unicode normalization and case sensitivity (P7-XP-04 / P7-XP-06)

Ignore matching is **NFC-normalized and case-sensitive**, deliberately:

- **NFC (shipped, `P7-XP-04`).** A macOS tree routinely carries decomposed (NFD) names — APFS
  is normalization-preserving (unlike HFS+, it does not force NFD), so whatever an HFS+-migrated
  volume, an extracted archive, a network filesystem, or an NFD-writing app put on disk is what
  readdir returns — while `.devstrapignore` files are almost always composed (NFC). A byte-exact
  matcher therefore silently failed to prune an NFD `café/` on macOS while pruning it on a Linux
  NFC tree — the same policy then
  diverges across a fleet, and a draft bundle can ship content the pattern was written to
  exclude. The compiler now applies `norm.NFC.String` to every pattern line at compile time and
  to every match target in `Match` (and therefore `ShouldPruneDir`), the same normalization
  `internal/pathkey` has always applied to namespace keys. `GitignoreFragment` emits the
  normalized (NFC) pattern text, so compile → fragment → recompile is a fixed point.
- **Case-sensitive (documented, `P7-XP-06`).** git on default macOS (`core.ignorecase=true`)
  matches ignores case-insensitively, so a mixed-case gitignore ported into `.devstrapignore`
  can prune differently under git than under DevStrap on the same machine. DevStrap keeps
  matching case-sensitive anyway: the compiled policy must mean the same thing on every device
  in the fleet (macOS, Linux, agents), and per-OS case folding would reintroduce exactly the
  divergence the NFC fix removes. Note the contrast with `path_key`, which case-folds — that is
  namespace *identity* (two projects may not differ only by case), not content *matching*.
  Write patterns in the on-disk case; add both spellings if a tree genuinely mixes them.
  **`path_key` genuinely case-*folds* only since `W13-07`** (2026-08-01): it previously applied
  `strings.ToLower` after NFC, which is simple case *mapping* and is not normalization-preserving,
  so two spellings of one path could yield two keys. See `internal/pathkey.foldKey`.

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

**The single `.devstrapignore` compiler is now built** as `internal/ignore` (DRAFT-03). It compiles *gitignore-inspired* patterns from a project's `.devstrapignore` file plus a canonical default OS-junk/build-artifact table, and feeds the draft-bundle allow-list from one source; a `GitignoreFragment` API exists but has no consumer yet (no code writes a managed `.gitignore` block). The compiler now follows gitignore semantics (`P6-XP-02`, shipped 2026-07-04, differential-tested against `git check-ignore`): it anchors on a leading **or** middle separator, translates bracket classes, degrades an unclosed `[` to a literal, and treats non-standalone `**` as a single `*`. The scanner prune predicate is now fixed too (`P6-XP-06`, shipped 2026-07-04): `scan.Walk` calls `ignore.CompileFromDir` once per walk, offers an `Options.Ignore` test-injection seam, falls back to the default matcher with a warning on compile error, and sources pruning from `internal/ignore`, matching the draft-bundle path. The watcher now consumes the same compiler (`PLAT-01`/`PLAT-04`, shipped 2026-07-28), including project-local patterns and descriptor release when watched directories leave the tree; OS/editor event noise is filtered in the adapter rather than added to the canonical table, for the reasons under *Watcher exclusion set* above. The agent deny-list remains to be folded in as follow-up.

## Audit follow-ups (2026-06-28)

The 2026-06-28 cloud-sync design **promotes the single `.devstrapignore` compiler from absent to designed-and-required** and makes it a hard dependency of the new non-git content-sync workstream. The "Dropbox experience for code" splits sync strictly by content type — repo content rides git's own blobless clone/fetch from its existing remote and never touches the hub; env vars and non-git/draft folders ship as age-encrypted, content-addressed `age_blob:<sha256>` blobs; `node_modules` and build artifacts are never synced and are rebuilt on hydrate. Because the draft-bundling layer is the only path by which uncontrolled files reach the zero-knowledge hub, its exclusion set MUST be the compiled `.devstrapignore` output and nothing else.

Required follow-ups (workstream `DRAFT-*` in `docs/audits/AUDIT_RECOMMENDATIONS_2026-06-28.md`):

- build the one canonical compiler and route every consumer through it — **shipped**, including `internal/scan`, draft bundling, the platform watcher, the agent deny-list, and sandbox credential masks (`PLAT-01`, `PLAT-04`, `AGEN-05`);
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

> Repo housekeeping note (2026-07-05): the repository's own `.gitignore` gained `/completions/` — GoReleaser's `before` hook regenerates shell completions there on every release build; they are build output, never source.

> Repo housekeeping note (2026-07-31): the repository's own `.gitignore` gained an explicit
> `!docs/agents.md` negation, and it is worth recording *why*, because the trap generalizes to any
> repo that ignores a filename rather than a path. The "# Agents files" block ignores `CLAUDE.md`
> and `AGENTS.md` to keep local agent-instruction scratch out of the tree. Git matches such a
> pattern by **basename at any depth**, and `core.ignorecase` is `true` on macOS and Windows — so
> the genuinely-new user guide `docs/agents.md` matched the `AGENTS.md` rule and `git add -A`
> skipped it **silently**, which is exactly what `add` does with an ignored path. The repository's
> own root `AGENTS.md` was never affected, because an ignore rule does not untrack an
> already-tracked file; that is precisely why the rule looked harmless for months and why the
> failure surfaced only when a *new* file happened to collide.
>
> Two lessons this file is the right home for. First, prefer **anchored** patterns (`/AGENTS.md`)
> when the intent is "this specific file at the repo root", since an unanchored basename pattern
> quietly claims that name everywhere in the tree. Second, and more general: `git add` reports
> nothing when it skips an ignored path, so "I created the file and ran `git add -A`" is not
> evidence the file is in the commit — and neither is `test -f`, which passes for an untracked file
> sitting in the working tree. Verify with `git diff --cached --name-only` or
> `git cat-file -e <rev>:<path>`. This was found by the Pass-8 audit as `P8-ADOPT-01` after the file
> had already shipped as a broken link with five specs asserting it existed.
