# Releasing DevStrap

DevStrap is **trunk-based**: releases are cut from `main` (or a short-lived release branch cut from `main`), never
from a feature branch. `main` is always green — every PR passes CI before merge — so any commit on `main` is a
release candidate.

Releases are automated by **GoReleaser** via `.github/workflows/release.yml`, triggered on `v*` tags. It
cross-compiles macOS and Linux binaries (amd64 + arm64), packages shell completions into each tarball, generates
`checksums.txt`, publishes a GitHub Release, and — on **stable** tags only (`skip_upload: auto`) — pushes an updated
Homebrew **cask** to `Reederey87/homebrew-devstrap`. The `version`, `commit`, and build `date` are injected into the
binary (check with `devstrap version`).

## One-time prerequisites (already done for this repo; listed for rebuild-from-scratch)

- A public tap repo `Reederey87/homebrew-devstrap` with a `Casks/` directory (GoReleaser writes `Casks/devstrap.rb`).
- A fine-grained PAT scoped to **that repo only**, Contents: Read+Write, stored as the `HOMEBREW_TAP_GITHUB_TOKEN`
  Actions secret on this repo (`gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo Reederey87/DevStrap`). The default
  `GITHUB_TOKEN` cannot push cross-repo.

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
   The workflow publishes the full (non-pre-release) GitHub Release **and** pushes the updated cask to the tap.
   The stable tag may point at the **same commit** as the rc — the workflow pins `GORELEASER_CURRENT_TAG` to the
   triggering tag, so GoReleaser cannot mistake the co-located rc tag for the current one (git's version sort ranks
   `v0.1.0-rc.1` above `v0.1.0`, which made the first `v0.1.0` run rebuild rc artifacts and fail on upload).
5. **If it's not**, fix it on `main` via the normal PR flow, then cut `v0.1.0-rc.2` and repeat.

## Post-release smoke checklist (stable tags)

```bash
# Tap path — completions should install alongside the binary
brew install Reederey87/devstrap/devstrap && devstrap version

# Installer path — no overrides resolves the latest release
curl -fsSL https://raw.githubusercontent.com/Reederey87/DevStrap/main/scripts/install.sh | sh

# Pre-release smoke uses the pinned form instead (the rc never updates the tap):
DEVSTRAP_VERSION=v0.1.0-rc.1 sh scripts/install.sh
```

Confirm the tap repo got exactly one new commit (`Casks/devstrap.rb`) and that rc tags produced **no** tap commit.

Also verify the release's cosign signature and SBOMs are present:

```bash
cosign verify-blob \
  --certificate-identity "https://github.com/Reederey87/DevStrap/.github/workflows/release.yml@refs/tags/v0.1.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
```

The GitHub release assets should include `checksums.txt.sigstore.json` and one `<archive>.sbom.json`
per archive.

## Verifying build provenance (SLSA)

Every release run also attaches a SLSA v1 provenance attestation (`multiple.intoto.jsonl`), signed keyless
via Sigstore/Fulcio and logged in Rekor by the `provenance` job. Verify that a downloaded artifact was built by
this repo's release workflow at the expected tag:

```bash
gh release download vX.Y.Z -R Reederey87/DevStrap -p "multiple.intoto.jsonl" -p "*.tar.gz"
for f in devstrap_*.tar.gz; do
  slsa-verifier verify-artifact \
    --provenance-path multiple.intoto.jsonl \
    --source-uri github.com/Reederey87/DevStrap \
    --source-tag vX.Y.Z \
    "$f"
done
```

A passing check proves the tarball was produced by this repository's release workflow at that tag and signed
keyless (its Fulcio identity is recorded in the Rekor transparency log) — not rebuilt or swapped by a third party.

## Enabling notarization (dormant until Apple enrollment lands)

The release binaries are ad-hoc signed today, so `brew install --cask`'s quarantine bit would
block them — the cask strips it in a post-install hook. Homebrew **removes support for
Gatekeeper-failing casks on 2026-09-01**, so Developer ID signing + notarization must be live
before then. The `.goreleaser.yaml` `notarize:` block is already wired and self-activates when
the secrets below exist (`isEnvSet "MACOS_SIGN_P12"`); nothing else changes.

One-time enrollment checklist:

1. Enroll in the [Apple Developer Program](https://developer.apple.com/programs/) ($99/yr).
2. Create a **Developer ID Application** certificate; import to Keychain, export as `.p12`
   with a password.
3. Create an [App Store Connect API key](https://appstoreconnect.apple.com/access/integrations/api)
   (`.p8`) with Developer access for the notary service.
4. Set the five Actions secrets (base64-encode the two key files):

   ```bash
   gh secret set MACOS_SIGN_P12        --repo Reederey87/DevStrap --body "$(base64 -i devid.p12)"
   gh secret set MACOS_SIGN_PASSWORD   --repo Reederey87/DevStrap
   gh secret set MACOS_NOTARY_KEY      --repo Reederey87/DevStrap --body "$(base64 -i AuthKey.p8)"
   gh secret set MACOS_NOTARY_KEY_ID   --repo Reederey87/DevStrap
   gh secret set MACOS_NOTARY_ISSUER_ID --repo Reederey87/DevStrap
   ```

5. Cut an rc; on the published darwin binaries verify Gatekeeper acceptance:

   ```bash
   codesign -dv ./devstrap        # TeamIdentifier set, not "adhoc"
   spctl -a -vvv -t install ./devstrap
   ```

6. Remove the cask's `xattr -dr com.apple.quarantine` post-install hook from
   `.goreleaser.yaml` (and this file's smoke-checklist mention of it) in the same PR that
   confirms a notarized release — that closes `P4-SEC-05`.

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
