#!/bin/sh
# DevStrap installer — https://github.com/Reederey87/DevStrap
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Reederey87/DevStrap/main/scripts/install.sh | sh
#
# Environment overrides:
#   DEVSTRAP_VERSION      release tag to install (e.g. v0.1.0); default: latest release
#   DEVSTRAP_INSTALL_DIR  install directory; default: /usr/local/bin if writable,
#                         otherwise ~/.local/bin (created if needed)
#
# The script downloads the release tarball AND checksums.txt, verifies the
# sha256 before extracting, and never invokes sudo. If the default install
# directory is not writable, it falls back to ~/.local/bin and tells you.
set -eu

REPO="Reederey87/DevStrap"
PROJECT="devstrap"

fail() {
  echo "install.sh: $*" >&2
  exit 1
}

# --- platform detection ---------------------------------------------------
os=$(uname -s)
case "$os" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) fail "unsupported OS: $os (devstrap ships darwin and linux binaries)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) fail "unsupported architecture: $arch (devstrap ships amd64 and arm64 binaries)" ;;
esac

# --- version resolution ---------------------------------------------------
version="${DEVSTRAP_VERSION:-}"
if [ -z "$version" ]; then
  # The releases/latest endpoint 302s to .../tag/<version>; no API token needed.
  redirect=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest") ||
    fail "could not resolve the latest release (set DEVSTRAP_VERSION=vX.Y.Z to pin one)"
  version="${redirect##*/}"
  case "$version" in
    v*) ;;
    *) fail "could not parse the latest release tag from ${redirect} (set DEVSTRAP_VERSION=vX.Y.Z)" ;;
  esac
fi
# Normalize a user-supplied DEVSTRAP_VERSION without the leading v — tags are
# v-prefixed, so "0.1.0" would otherwise 404 with an unhelpful download error.
version="v${version#v}"
# GoReleaser artifact names drop the tag's leading v.
bare_version="${version#v}"

archive="${PROJECT}_${bare_version}_${os}_${arch}.tar.gz"
base_url="https://github.com/${REPO}/releases/download/${version}"

# --- download + checksum verification --------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading ${archive} (${version})..."
curl -fsSL -o "${tmp}/${archive}" "${base_url}/${archive}" ||
  fail "download failed: ${base_url}/${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${base_url}/checksums.txt" ||
  fail "download failed: ${base_url}/checksums.txt"

# Extract the one matching checksum line FIRST and fail hard when it is
# absent — piping an empty grep result into `sha256sum -c` must never be able
# to read as "verified".
checksum_line=$(grep " ${archive}\$" "${tmp}/checksums.txt") ||
  fail "checksums.txt has no entry for ${archive}; refusing to install an unverified archive"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp" && printf '%s\n' "$checksum_line" | sha256sum -c - >/dev/null) ||
    fail "sha256 verification failed for ${archive}"
elif command -v shasum >/dev/null 2>&1; then
  (cd "$tmp" && printf '%s\n' "$checksum_line" | shasum -a 256 -c - >/dev/null) ||
    fail "sha256 verification failed for ${archive}"
else
  fail "neither sha256sum nor shasum found; refusing to install unverified binaries"
fi
echo "Checksum verified."

tar -xzf "${tmp}/${archive}" -C "$tmp"
[ -f "${tmp}/${PROJECT}" ] || fail "archive did not contain the ${PROJECT} binary"

# --- install ----------------------------------------------------------------
install_dir="${DEVSTRAP_INSTALL_DIR:-}"
if [ -z "$install_dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="${HOME}/.local/bin"
  fi
fi
mkdir -p "$install_dir" || fail "could not create ${install_dir} (set DEVSTRAP_INSTALL_DIR to a writable directory)"
[ -w "$install_dir" ] || fail "${install_dir} is not writable (set DEVSTRAP_INSTALL_DIR to a writable directory; this script never uses sudo)"

install -m 0755 "${tmp}/${PROJECT}" "${install_dir}/${PROJECT}"
echo "Installed ${install_dir}/${PROJECT}"

case ":${PATH}:" in
  *":${install_dir}:"*) ;;
  *) echo "note: ${install_dir} is not on your PATH — add it, e.g.: export PATH=\"${install_dir}:\$PATH\"" ;;
esac

"${install_dir}/${PROJECT}" version
