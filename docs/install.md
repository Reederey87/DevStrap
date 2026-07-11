# Installing DevStrap

DevStrap ships as a single Go binary for macOS and Linux (amd64 + arm64). Pick whichever path
below fits your machine; all of them leave you with a `devstrap` on your `PATH`. Verify with:

```bash
devstrap version   # prints version, commit, and build date
devstrap doctor    # checks Git, editors, forge CLIs, and hub prerequisites
```

Once installed, head to [quickstart.md](quickstart.md).

## Homebrew (macOS and Linux) — recommended

```bash
brew install Reederey87/devstrap/devstrap   # equivalently: brew install --cask Reederey87/devstrap/devstrap
devstrap version
```

This installs a Homebrew **cask** from the `Reederey87/devstrap` tap (the tap-qualified name
resolves to the cask — the flagless form is what the v0.1.0 release smoke verified); bash/zsh/fish shell
completions install alongside the binary. The release binaries are **not Apple-notarized yet**,
so the cask strips the macOS quarantine bit in a documented post-install hook (signing and
notarization are tracked as future work). Upgrade with `brew upgrade devstrap`.

## One-line installer

```bash
curl -fsSL https://raw.githubusercontent.com/Reederey87/DevStrap/main/scripts/install.sh | sh
```

The script detects your OS/arch, resolves the latest release, verifies the downloaded tarball
by first checking the identity-pinned cosign signature over `checksums.txt`, then always checking
the tarball's sha256 **before** extracting. It also verifies the tarball's SLSA provenance. The installer fails closed when cosign or
`slsa-verifier` is unavailable (each with its own explicit escape hatch) and
installs into `/usr/local/bin` (or `~/.local/bin` if that isn't writable). It never uses sudo.
Overrides:

- `DEVSTRAP_VERSION=v0.1.0` pins a specific release (also the way to install a pre-release).
- `DEVSTRAP_INSTALL_DIR=~/bin` picks the destination directory.
- `DEVSTRAP_INSTALL_CHECKSUM_ONLY=1` explicitly accepts weakened verification when cosign is
  unavailable or an older release has no signature bundle. With cosign missing but the bundle
  present, sha256 still runs and SLSA provenance is verified opportunistically (only if
  `slsa-verifier` happens to be installed);
  for a pre-bundle release (bundle 404) SLSA is skipped too and TLS + sha256 is all that remains.
  Intended only as a deliberate compatibility escape hatch.
- `DEVSTRAP_INSTALL_NO_SLSA=1` skips ONLY the SLSA provenance layer (the explicit escape hatch
  for a missing `slsa-verifier`); cosign and sha256 verification still run.

The `main` URL above is mutable even when `DEVSTRAP_VERSION` pins the binary release. For a
high-assurance install, fetch the installer itself from the same immutable release tag:

```bash
tag=v0.1.1   # pick the release you are installing
curl -fsSL "https://raw.githubusercontent.com/Reederey87/DevStrap/${tag}/scripts/install.sh" | DEVSTRAP_VERSION="$tag" sh
```

## Download a release binary

Prebuilt tarballs for macOS and Linux are published on the
[Releases](https://github.com/Reederey87/DevStrap/releases) page (built by GoReleaser). Each
tarball contains the binary, `LICENSE`, `README`, and pre-generated bash/zsh/fish completions,
and every release ships a `checksums.txt`. Download, **verify against the checksum**, extract,
and put `devstrap` on your `PATH`:

```bash
# verify (example for the darwin/arm64 tarball)
shasum -a 256 -c checksums.txt --ignore-missing

# install the extracted binary into ~/.local/bin
tar -xzf devstrap_0.1.0_darwin_arm64.tar.gz
install -m 0755 ./devstrap ~/.local/bin/devstrap
devstrap version
```

## Verify a download

Every release publishes a keyless cosign signature over `checksums.txt` (Fulcio cert + Rekor
transparency log, no long-lived signing key) and an SPDX SBOM per archive (`<archive>.sbom.json`):

```bash
cosign verify-blob \
  --certificate-identity "https://github.com/Reederey87/DevStrap/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
```

The signature ties `checksums.txt` — and transitively every archive it lists — to a run of this
repo's release workflow, not to a possibly-compromised uploader. The one-line installer performs
this cosign check automatically and also verifies the matching `multiple.intoto.jsonl` provenance
(fail-closed; `DEVSTRAP_INSTALL_NO_SLSA=1` is the explicit waiver) before trusting the always-on
sha256 check.

## Bleeding edge: `go install`

To run the tip of `main` without waiting for a release (requires Go 1.26+):

```bash
go install github.com/Reederey87/DevStrap/cmd/devstrap@main
devstrap version
```

Note this builds from source, so the `version` string reflects the pseudo-version rather than a
tagged release, and shell completions are not installed for you (generate them with
`devstrap completion <bash|zsh|fish>`).

## Build from source

```bash
git clone git@github.com:Reederey87/DevStrap.git
cd DevStrap
go build -o bin/devstrap ./cmd/devstrap
./bin/devstrap version
```

Prefer not to install at all? Every command also works via `go run ./cmd/devstrap <cmd> …`
from a source checkout.

## Requirements

- **macOS or Linux.**
- **Git** on your `PATH`.
- **Go 1.26+** — only to build from source or use `go install`.
- **GitHub CLI (`gh`)**, and optionally `glab`/`tea`, for `agent pr` / PR-MR creation.

Optional:

- **1Password CLI (`op`)** for secret-provider mode (`env bind` / `run`).
- **Cursor** or **VS Code** command-line launchers for `devstrap open`.

## Maintainer note

The release pipeline itself (GoReleaser, tagging, the tap cask, the same-commit rc → stable
flow) is documented separately in [`../RELEASING.md`](../RELEASING.md). That file is for people
cutting releases, not installing them.
