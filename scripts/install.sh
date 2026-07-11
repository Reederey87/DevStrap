#!/bin/sh
# DevStrap installer — https://github.com/Reederey87/DevStrap
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Reederey87/DevStrap/main/scripts/install.sh | sh
#
# Environment overrides:
#   DEVSTRAP_VERSION                release tag to install (e.g. v0.1.0); default: latest release
#   DEVSTRAP_INSTALL_DIR            install directory; default: /usr/local/bin if writable,
#                                   otherwise ~/.local/bin (created if needed)
#   DEVSTRAP_INSTALL_CHECKSUM_ONLY  set to 1 to permit checksum-only verification without
#                                   cosign (SLSA then runs only if slsa-verifier is present)
#   DEVSTRAP_INSTALL_NO_SLSA        set to 1 to skip ONLY the SLSA provenance layer
#                                   (cosign + sha256 still verified)
#
# The script verifies the release-workflow cosign signature over checksums.txt
# AND the archive's SLSA provenance (both fail closed when their verifier is
# missing, each with its own explicit escape hatch above), then always verifies
# sha256 before extracting. It never invokes sudo. If the default install
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

bundle_available=1
# ONE request decides: the status and the body come from the same transfer, so
# a transient 5xx/network failure can never be misread as "no bundle" (404).
http_status=$(curl -sSL -o "${tmp}/checksums.txt.sigstore.json" -w '%{http_code}' "${base_url}/checksums.txt.sigstore.json") || http_status=""
if [ "$http_status" != "200" ]; then
  rm -f "${tmp}/checksums.txt.sigstore.json"
  if [ "$http_status" = "404" ]; then
    if [ "${DEVSTRAP_INSTALL_CHECKSUM_ONLY:-}" = "1" ]; then
      echo "WARNING: this release has no signature bundle; signature verification skipped (DEVSTRAP_INSTALL_CHECKSUM_ONLY=1); relying on TLS + sha256 only" >&2
      bundle_available=0
    else
      fail "this release has no signature bundle; refusing checksum-only verification (re-run with DEVSTRAP_INSTALL_CHECKSUM_ONLY=1 to accept TLS + sha256 only)"
    fi
  else
    fail "download failed (HTTP ${http_status:-error}): ${base_url}/checksums.txt.sigstore.json"
  fi
fi

if [ "$bundle_available" = "1" ]; then
  if command -v cosign >/dev/null 2>&1; then
    if ! cosign_out=$(cosign verify-blob \
      --certificate-identity "https://github.com/Reederey87/DevStrap/.github/workflows/release.yml@refs/tags/${version}" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      --bundle "${tmp}/checksums.txt.sigstore.json" \
      "${tmp}/checksums.txt" 2>&1); then
      printf '%s\n' "$cosign_out" >&2
      fail "cosign signature verification FAILED for checksums.txt — the release may have been tampered with; refusing to install"
    fi
    echo "Signature verified (cosign, release workflow identity)."
  elif [ "${DEVSTRAP_INSTALL_CHECKSUM_ONLY:-}" = "1" ]; then
    echo "WARNING: cosign signature verification skipped (DEVSTRAP_INSTALL_CHECKSUM_ONLY=1); relying on TLS + sha256 (plus SLSA provenance if slsa-verifier is installed)" >&2
  else
    fail "cosign not found; install it with 'brew install cosign' or from https://docs.sigstore.dev/cosign/system_config/installation/, or re-run with DEVSTRAP_INSTALL_CHECKSUM_ONLY=1 to accept checksum-only verification"
  fi
fi

# A release without a signature bundle predates provenance too — skip SLSA
# when the bundle 404'd and the checksum-only hatch was taken. Otherwise SLSA
# verification is MANDATORY like cosign (the P7-QUAL-02 fail-closed contract):
# a missing slsa-verifier fails the install unless explicitly waived.
if [ "$bundle_available" = "1" ] && [ "${DEVSTRAP_INSTALL_NO_SLSA:-}" != "1" ] && [ "${DEVSTRAP_INSTALL_CHECKSUM_ONLY:-}" != "1" ]; then
  command -v slsa-verifier >/dev/null 2>&1 ||
    fail "slsa-verifier not found; install it with 'brew install slsa-verifier' or from https://github.com/slsa-framework/slsa-verifier#installation, or re-run with DEVSTRAP_INSTALL_NO_SLSA=1 to skip only the provenance layer"
  curl -fsSL -o "${tmp}/multiple.intoto.jsonl" "${base_url}/multiple.intoto.jsonl" ||
    fail "download failed: ${base_url}/multiple.intoto.jsonl"
  if ! slsa_out=$(slsa-verifier verify-artifact --provenance-path "${tmp}/multiple.intoto.jsonl" \
    --source-uri github.com/Reederey87/DevStrap --source-tag "${version}" \
    "${tmp}/${archive}" 2>&1); then
    printf '%s\n' "$slsa_out" >&2
    fail "SLSA provenance verification FAILED for ${archive}; refusing to install"
  fi
  echo "SLSA provenance verified."
elif [ "$bundle_available" = "1" ] && [ "${DEVSTRAP_INSTALL_NO_SLSA:-}" = "1" ]; then
  echo "WARNING: SLSA provenance verification skipped (DEVSTRAP_INSTALL_NO_SLSA=1); cosign + sha256 still verified" >&2
elif [ "$bundle_available" = "1" ]; then
  # CHECKSUM_ONLY with the bundle present: cosign may already have been skipped
  # above; run SLSA opportunistically when the verifier happens to exist.
  if command -v slsa-verifier >/dev/null 2>&1; then
    curl -fsSL -o "${tmp}/multiple.intoto.jsonl" "${base_url}/multiple.intoto.jsonl" ||
      fail "download failed: ${base_url}/multiple.intoto.jsonl"
    if ! slsa_out=$(slsa-verifier verify-artifact --provenance-path "${tmp}/multiple.intoto.jsonl" \
      --source-uri github.com/Reederey87/DevStrap --source-tag "${version}" \
      "${tmp}/${archive}" 2>&1); then
      printf '%s\n' "$slsa_out" >&2
      fail "SLSA provenance verification FAILED for ${archive}; refusing to install"
    fi
    echo "SLSA provenance verified."
  fi
fi

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
