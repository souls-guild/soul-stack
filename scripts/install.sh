#!/usr/bin/env sh
# Soul Stack curl-installer (NIM-135). Downloads a release binary from GitHub and
# drops it into a bin dir. One-liner:
#
#   curl -fsSL https://raw.githubusercontent.com/souls-guild/soul-stack/main/scripts/install.sh | sh
#
# Configurable via env:
#   SOULSTACK_BIN   which binary to install: soulctl (default) | keeper | soul | soul-lint
#   SOULSTACK_VERSION  tag to install, e.g. v0.1.0-beta.1 (default: latest release)
#   SOULSTACK_INSTALL_DIR  install target (default: /usr/local/bin, falls back to ~/.local/bin)
#
# POSIX sh, no bashisms. Verifies the sha256 checksum against the release's
# checksums.txt before installing.
set -eu

REPO="souls-guild/soul-stack"
BIN="${SOULSTACK_BIN:-soulctl}"
VERSION="${SOULSTACK_VERSION:-latest}"

err() { echo "install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || err "required tool not found: $1"; }

need uname
need mktemp
# curl or wget — whichever is present.
if command -v curl >/dev/null 2>&1; then
  DL="curl -fsSL -o"
  DLO="curl -fsSL"
elif command -v wget >/dev/null 2>&1; then
  DL="wget -qO"
  DLO="wget -qO-"
else
  err "need curl or wget"
fi

case "$BIN" in
  soulctl|keeper|soul|soul-lint) ;;
  *) err "unknown SOULSTACK_BIN='$BIN' (want soulctl|keeper|soul|soul-lint)" ;;
esac

# Detect OS/arch → GoReleaser's name_template ({os}_{arch}). Linux-only builds.
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
[ "$os" = "linux" ] || err "only linux builds are published (got '$os'); build from source for $os"
case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) err "unsupported arch: $(uname -m) (published: amd64, arm64)" ;;
esac

# Resolve the tag. `latest` → the redirect target of the releases/latest URL.
if [ "$VERSION" = "latest" ]; then
  # -w '%{...}' unsupported on wget; use the GitHub API to read tag_name.
  api="https://api.github.com/repos/${REPO}/releases/latest"
  VERSION="$($DLO "$api" | sed -n 's/.*"tag_name"[ ]*:[ ]*"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$VERSION" ] || err "could not resolve latest release tag"
fi

# Strip a leading 'v' for the archive version field (GoReleaser uses the bare
# version in name_template), keep the tag for the download path. The release
# ships one bundle archive per platform holding all four binaries.
ver_noprefix="${VERSION#v}"
asset="soul-stack_${ver_noprefix}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${VERSION}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "install: fetching ${BIN} ${VERSION} (${os}/${arch})"
$DL "$tmp/$asset" "${base}/${asset}" || err "download failed: ${base}/${asset}"
$DL "$tmp/checksums.txt" "${base}/checksums.txt" || err "checksums download failed"

# Verify sha256 (sha256sum or shasum -a 256).
if command -v sha256sum >/dev/null 2>&1; then
  SHA="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA="shasum -a 256"
else
  echo "install: warning — no sha256 tool, skipping checksum verification" >&2
  SHA=""
fi
if [ -n "$SHA" ]; then
  want="$(grep " ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')"
  [ -n "$want" ] || err "checksum for ${asset} not found in checksums.txt"
  got="$(cd "$tmp" && $SHA "$asset" | awk '{print $1}')"
  [ "$want" = "$got" ] || err "checksum mismatch for ${asset}: want $want got $got"
  echo "install: checksum OK"
fi

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/$BIN" ] || err "binary '$BIN' not found inside archive"
chmod +x "$tmp/$BIN"

# Pick an install dir writable by the user.
dir="${SOULSTACK_INSTALL_DIR:-/usr/local/bin}"
if [ ! -w "$dir" ] && [ "$(id -u)" -ne 0 ]; then
  if [ -z "${SOULSTACK_INSTALL_DIR:-}" ]; then
    dir="$HOME/.local/bin"
    mkdir -p "$dir"
  else
    err "install dir '$dir' not writable (run as root or set SOULSTACK_INSTALL_DIR)"
  fi
fi

install -m 0755 "$tmp/$BIN" "$dir/$BIN" 2>/dev/null || { cp "$tmp/$BIN" "$dir/$BIN" && chmod 0755 "$dir/$BIN"; }
echo "install: installed ${BIN} → ${dir}/${BIN}"
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) echo "install: note — ${dir} is not on PATH; add it to your shell profile" ;;
esac
