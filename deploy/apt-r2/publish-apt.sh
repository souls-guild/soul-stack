#!/usr/bin/env bash
# Mirror Soul Stack .deb packages into a flat apt repository and sync it to a
# Cloudflare R2 bucket (NIM-135). Runs OUT OF BAND from the GitHub release
# workflow — it needs R2 credentials + the signing GPG key, which we keep off
# GitHub Actions. Trigger it by hand (or a self-hosted runner) after a release.
#
# Flow: collect .deb → build pool/ + dists/<suite>/ index → sign Release → push
# to R2 with rclone. Idempotent: re-running re-indexes and re-syncs the whole
# tree. See README.md for the one-time R2 + GPG + client setup.
#
# Required env:
#   APT_GPG_KEY_ID      key id/email used to sign the Release file
#   RCLONE_REMOTE       rclone remote name pointing at the R2 bucket (e.g. r2:soul-stack-apt)
# Optional env:
#   APT_SUITE           suite name (default: stable)
#   APT_COMPONENT       component (default: main)
#   APT_ARCHS           space-separated arches (default: "amd64 arm64")
#   DEB_DIR             dir holding the .deb files to mirror (default: ./dist/pkg)
#   WORK_DIR            local repo staging dir (default: ./dist/apt-repo)
set -euo pipefail

SUITE="${APT_SUITE:-stable}"
COMPONENT="${APT_COMPONENT:-main}"
ARCHS="${APT_ARCHS:-amd64 arm64}"
DEB_DIR="${DEB_DIR:-./dist/pkg}"
WORK_DIR="${WORK_DIR:-./dist/apt-repo}"

die() { echo "publish-apt: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing tool: $1"; }

need apt-ftparchive   # from apt-utils
need gpg
need rclone
: "${APT_GPG_KEY_ID:?set APT_GPG_KEY_ID}"
: "${RCLONE_REMOTE:?set RCLONE_REMOTE (rclone remote for the R2 bucket)}"

[ -d "$DEB_DIR" ] || die "DEB_DIR not found: $DEB_DIR"
debs=$(find "$DEB_DIR" -maxdepth 1 -name '*.deb' | wc -l)
[ "$debs" -gt 0 ] || die "no .deb files in $DEB_DIR"
echo "publish-apt: mirroring $debs package(s) into suite=$SUITE component=$COMPONENT"

# 1. Layout: pool/<component>/ holds the .deb blobs; dists/ holds the indexes.
pool="$WORK_DIR/pool/$COMPONENT"
mkdir -p "$pool"
cp -f "$DEB_DIR"/*.deb "$pool/"

# 2. Per-arch Packages index (+ .gz), then the component-wide binary indexes.
for arch in $ARCHS; do
  bindir="$WORK_DIR/dists/$SUITE/$COMPONENT/binary-$arch"
  mkdir -p "$bindir"
  ( cd "$WORK_DIR" && apt-ftparchive --arch "$arch" packages "pool/$COMPONENT" ) > "$bindir/Packages"
  gzip -9c "$bindir/Packages" > "$bindir/Packages.gz"
  cat > "$bindir/Release" <<EOF
Archive: $SUITE
Component: $COMPONENT
Origin: Soul Stack
Label: Soul Stack
Architecture: $arch
EOF
done

# 3. Top-level Release over the whole suite (checksums of every index).
archs_csv=$(echo "$ARCHS" | tr ' ' ' ')
relfile="$WORK_DIR/dists/$SUITE/Release"
cat > "$WORK_DIR/apt-ftparchive-release.conf" <<EOF
APT::FTPArchive::Release::Origin "Soul Stack";
APT::FTPArchive::Release::Label "Soul Stack";
APT::FTPArchive::Release::Suite "$SUITE";
APT::FTPArchive::Release::Codename "$SUITE";
APT::FTPArchive::Release::Components "$COMPONENT";
APT::FTPArchive::Release::Architectures "$archs_csv";
EOF
( cd "$WORK_DIR/dists/$SUITE" && apt-ftparchive -c ../../apt-ftparchive-release.conf release . ) > "$relfile"

# 4. Sign: detached Release.gpg + inline InRelease. apt verifies both forms.
gpg --batch --yes --local-user "$APT_GPG_KEY_ID" -abs -o "$WORK_DIR/dists/$SUITE/Release.gpg" "$relfile"
gpg --batch --yes --local-user "$APT_GPG_KEY_ID" --clearsign -o "$WORK_DIR/dists/$SUITE/InRelease" "$relfile"

# 5. Public signing key so clients can `apt-key`/keyring it.
gpg --armor --export "$APT_GPG_KEY_ID" > "$WORK_DIR/soul-stack.gpg.key"

# 6. Sync the whole tree to R2. --checksum: skip unchanged blobs; the pool is
# content-addressed by filename+version so re-uploads are cheap.
echo "publish-apt: syncing to $RCLONE_REMOTE"
rclone sync "$WORK_DIR" "$RCLONE_REMOTE" --checksum --transfers 8 --fast-list

echo "publish-apt: done. Clients add:"
echo "  deb [signed-by=/usr/share/keyrings/soul-stack.gpg] https://<r2-public-host>/ $SUITE $COMPONENT"
