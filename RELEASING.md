# Releasing DevStrap

DevStrap is **trunk-based**: releases are cut from `main` (or a short-lived release branch cut from `main`), never
from a feature branch. `main` is always green — every PR passes CI before merge — so any commit on `main` is a
release candidate.

Releases are automated by **GoReleaser** via `.github/workflows/release.yml`, triggered on `v*` tags. It
cross-compiles macOS and Linux binaries (amd64 + arm64), generates `checksums.txt`, and publishes a GitHub Release.
The `version`, `commit`, and build `date` are injected into the binary (check with `devstrap version`).

## Versioning

- Follows [SemVer](https://semver.org): `vMAJOR.MINOR.PATCH`.
- Pre-release tags use a suffix: `-rc.N` (release candidate), `-beta.N`, or `-alpha.N`. GoReleaser publishes these as
  GitHub **pre-releases** automatically (`release.prerelease: auto` in `.goreleaser.yaml`).
- Tags are annotated and start with `v` (e.g. `v0.1.0`, `v0.1.0-rc.1`).

## Standard flow: release candidate → stable

1. **Confirm `main` is green** and contains everything you want to ship.
2. **Cut a release candidate** from `main`:
   ```bash
   git checkout main && git pull --ff-only
   git tag -a v0.1.0-rc.1 -m "v0.1.0-rc.1"
   git push origin v0.1.0-rc.1
   ```
   The Release workflow builds the binaries and publishes a GitHub **pre-release**.
3. **Test the candidate** — download the pre-release artifacts (the exact binaries users will get) and smoke-test on
   macOS and Linux:
   ```bash
   tar -xzf devstrap_0.1.0-rc.1_darwin_arm64.tar.gz
   ./devstrap version     # confirms version/commit/date are injected
   ./devstrap doctor
   # exercise the core loop: init / scan / status / ...
   ```
   Verify the download against `checksums.txt`.
4. **If it's good**, promote to a stable release:
   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```
   The workflow publishes the full (non-pre-release) GitHub Release.
5. **If it's not**, fix it on `main` via the normal PR flow, then cut `v0.1.0-rc.2` and repeat.

## When to use a release branch

Use a release branch only when you need to stabilize a release while `main` keeps moving, or to back-port fixes to an
older line:

```bash
git checkout -b release/v0.1 <commit-on-main>
git push origin release/v0.1
# cherry-pick only the fixes that belong on this line (via PRs targeting the branch)
git tag -a v0.1.1 -m "v0.1.1" && git push origin v0.1.1
```

You do **not** need a release branch for a normal single-line release — tag `main` directly.

## Edge / nightly

Adventurous users can run the bleeding edge straight from `main` without waiting for a release:

```bash
go install github.com/Reederey87/DevStrap/cmd/devstrap@main
```

## Keeping `main` releasable

- Merge incomplete features behind a flag (e.g. an `--experimental` flag / `DEVSTRAP_EXPERIMENTAL` env) so partial work
  never blocks a release.
- Required CI (Go tests on macOS + Linux, lint, spec-drift, vulnerability check) gates every PR, so `main` stays
  shippable.

## Rollback

A bad release is superseded by a higher patch (e.g. `v0.1.1`). For local state/DB rollback during upgrades, see
`spec/14_MVP_ROADMAP_AND_BACKLOG.md` → "Release and upgrade gates" (`devstrap db backup` via `VACUUM INTO` before
migrating).
