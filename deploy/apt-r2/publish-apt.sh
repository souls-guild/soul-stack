#!/usr/bin/env bash
# Mirror Soul Stack .deb packages into a flat apt repository and sync it to a
# Cloudflare R2 bucket (NIM-135). The primary caller is .github/workflows/apt-publish.yml
# (hosted runner, R2 + GPG credentials from repo secrets), which runs on `release:
# published`. Running it by hand is the fallback path for an operator with the same
# credentials.
#
# Flow: vet DEB_DIR → build pool/ + dists/<suite>/ index → sign Release → push
# to R2 with rclone. Idempotent: re-running re-indexes and re-syncs the whole
# tree. See README.md for the one-time R2 + GPG + client setup.
#
# PUBLISHING IS OUTWARD-FACING AND HARD TO REVERSE. What lands in the bucket is
# decided entirely by DEB_DIR: the remote prune deletes whatever the staging tree
# no longer carries, so an incomplete DEB_DIR does not just publish less — it
# UNPUBLISHES the packages it is missing. Hence the two gates below, both
# fail-closed, and the dry-run preview before the real sync.
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
#   APT_ALLOW_UNRELEASED=1  publish anyway when a package version is a dev build
#   APT_ALLOW_PARTIAL=1     publish anyway when packages are missing vs .goreleaser.yaml
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
need dpkg-deb
: "${APT_GPG_KEY_ID:?set APT_GPG_KEY_ID}"
: "${RCLONE_REMOTE:?set RCLONE_REMOTE (rclone remote for the R2 bucket)}"

[ -d "$DEB_DIR" ] || die "DEB_DIR not found: $DEB_DIR"
debs=$(find "$DEB_DIR" -maxdepth 1 -name '*.deb' | wc -l)
[ "$debs" -gt 0 ] || die "no .deb files in $DEB_DIR"

# 0. Vet the input before touching the bucket, and show it. `make pkg` writes an
# incomplete set into this very directory, and a stale tree from an earlier build
# survives there indefinitely — either one would quietly rewrite the live repo.
echo "publish-apt: DEB_DIR=$(cd "$DEB_DIR" && pwd)"
echo "publish-apt: $debs package(s) → suite=$SUITE component=$COMPONENT"

present=""
unreleased=""
while IFS= read -r deb; do
  pkg=$(dpkg-deb -f "$deb" Package)
  ver=$(dpkg-deb -f "$deb" Version)
  arch=$(dpkg-deb -f "$deb" Architecture)
  printf '  %-24s %-40s %s\n' "$pkg" "$ver" "$arch"
  present="$present $pkg"
  # git-describe leftovers (-dirty, -g<sha>) and goreleaser snapshots are dev
  # builds; a released tag renders a clean version. Publishing one of these puts
  # an unreproducible binary on every user's machine.
  if [[ "$ver" =~ (dirty|SNAPSHOT|snapshot|-g[0-9a-f]{7,}) ]]; then
    unreleased="$unreleased $pkg=$ver"
  fi
done < <(find "$DEB_DIR" -maxdepth 1 -name '*.deb' | sort)

if [ -n "$unreleased" ]; then
  echo "publish-apt: dev build(s) in DEB_DIR:$unreleased" >&2
  [ "${APT_ALLOW_UNRELEASED:-0}" = "1" ] \
    || die "refusing to publish a dev build; set APT_ALLOW_UNRELEASED=1 to override"
  echo "publish-apt: APT_ALLOW_UNRELEASED=1 — publishing a dev build anyway" >&2
fi

# The expected package set comes from .goreleaser.yaml, the only source of truth
# for what a release ships — so a rename or a new package is picked up here for
# free, without a second list to keep in sync.
goreleaser="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.goreleaser.yaml"
if [ -f "$goreleaser" ]; then
  missing=""
  for want in $(grep -oE 'package_name:[[:space:]]*soul-stack-[a-z-]+' "$goreleaser" \
                | awk '{print $2}' | sort -u); do
    case " $present " in *" $want "*) ;; *) missing="$missing $want" ;; esac
  done
  if [ -n "$missing" ]; then
    echo "publish-apt: missing vs $goreleaser:$missing" >&2
    echo "publish-apt: these would be DELETED from the live repo, not merely skipped." >&2
    [ "${APT_ALLOW_PARTIAL:-0}" = "1" ] \
      || die "refusing to publish an incomplete set; set APT_ALLOW_PARTIAL=1 to override (e.g. re-publishing an older tag)"
    echo "publish-apt: APT_ALLOW_PARTIAL=1 — publishing an incomplete set anyway" >&2
  fi
else
  echo "publish-apt: WARNING: $goreleaser not found — completeness gate SKIPPED" >&2
fi

# 1. Layout: pool/<component>/ holds the .deb blobs; dists/ holds the indexes.
# The pool is rebuilt from DEB_DIR every run: WORK_DIR persists between runs, so a
# package dropped from the release would otherwise linger here and be re-uploaded
# forever — the remote prune below can only remove what the staging tree no longer
# has. dists/ is left alone so old by-hash copies survive as a grace window for
# clients that read InRelease just before a publish.
pool="$WORK_DIR/pool/$COMPONENT"
rm -rf "$pool"
mkdir -p "$pool"
cp -f "$DEB_DIR"/*.deb "$pool/"

# 2. Per-arch Packages index (+ .gz), then the component-wide binary indexes.
for arch in $ARCHS; do
  bindir="$WORK_DIR/dists/$SUITE/$COMPONENT/binary-$arch"
  mkdir -p "$bindir"
  ( cd "$WORK_DIR" && apt-ftparchive --arch "$arch" packages "pool/$COMPONENT" ) > "$bindir/Packages"
  gzip -9nc "$bindir/Packages" > "$bindir/Packages.gz"   # -n: reproducible, stable hash across no-op runs
  cat > "$bindir/Release" <<EOF
Archive: $SUITE
Component: $COMPONENT
Origin: Soul Stack
Label: Soul Stack
Architecture: $arch
EOF
done

# 3. Top-level Release over the whole suite (checksums of every index).
# The generator config lives in a temp file so it never lands in the published tree.
archs_csv=$(echo "$ARCHS" | tr ' ' ' ')
relfile="$WORK_DIR/dists/$SUITE/Release"
relconf="$(mktemp)"; trap 'rm -f "$relconf"' EXIT
cat > "$relconf" <<EOF
APT::FTPArchive::Release::Origin "Soul Stack";
APT::FTPArchive::Release::Label "Soul Stack";
APT::FTPArchive::Release::Suite "$SUITE";
APT::FTPArchive::Release::Codename "$SUITE";
APT::FTPArchive::Release::Components "$COMPONENT";
APT::FTPArchive::Release::Architectures "$archs_csv";
APT::FTPArchive::Release::Acquire-By-Hash "yes";
EOF
( cd "$WORK_DIR/dists/$SUITE" && apt-ftparchive -c "$relconf" release . ) > "$relfile"

# 3b. by-hash: content-addressed copies of each index under every digest the Release
# advertises (apt fetches by the strongest — SHA512 — so all must exist, or it 404s
# and falls back to the plain path). These hash-named URLs have no cacheable extension
# and are immutable, so a CDN edge can never serve a stale index against the fresh
# Release. Built AFTER the Release so its checksums cover only the canonical paths.
for arch in $ARCHS; do
  bindir="$WORK_DIR/dists/$SUITE/$COMPONENT/binary-$arch"
  for f in Packages Packages.gz Release; do
    [ -f "$bindir/$f" ] || continue
    for algo in MD5Sum:md5sum SHA1:sha1sum SHA256:sha256sum SHA512:sha512sum; do
      mkdir -p "$bindir/by-hash/${algo%%:*}"
      cp -f "$bindir/$f" "$bindir/by-hash/${algo%%:*}/$(${algo##*:} "$bindir/$f" | cut -d' ' -f1)"
    done
  done
done

# 4. Sign: detached Release.gpg + inline InRelease. apt verifies both forms.
gpg --batch --yes --local-user "$APT_GPG_KEY_ID" -abs -o "$WORK_DIR/dists/$SUITE/Release.gpg" "$relfile"
gpg --batch --yes --local-user "$APT_GPG_KEY_ID" --clearsign -o "$WORK_DIR/dists/$SUITE/InRelease" "$relfile"

# 5. Public signing key so clients can `apt-key`/keyring it.
gpg --armor --export "$APT_GPG_KEY_ID" > "$WORK_DIR/soul-stack.gpg.key"

# 6. Sync the whole tree to R2. --checksum: skip unchanged blobs; the pool is
# content-addressed by filename+version so re-uploads are cheap.
# --s3-no-check-bucket: scoped R2 tokens can't HeadBucket/CreateBucket at account scope.
# --delete-before: prune stale objects up front; rclone skips its default post-run
# delete pass whenever an upload reports an IO error (harmless R2 retry noise), which
# would otherwise leave removed packages lingering in the bucket forever.
sync_flags=(--checksum --transfers 8 --fast-list --s3-no-check-bucket --delete-before)

# Preview first: deletions are the irreversible half of a sync, and they are
# driven by what the staging tree lacks rather than by anything stated here.
echo "publish-apt: dry run against $RCLONE_REMOTE"
dryrun=$(rclone sync "$WORK_DIR" "$RCLONE_REMOTE" "${sync_flags[@]}" --dry-run 2>&1) || true
deletions=$(printf '%s\n' "$dryrun" | grep -iE 'delete' || true)
if [ -n "$deletions" ]; then
  echo "publish-apt: objects that will be REMOVED from the bucket:"
  printf '%s\n' "$deletions" | sed 's/^/  /'
else
  echo "publish-apt: no objects will be removed"
fi

echo "publish-apt: syncing to $RCLONE_REMOTE"
rclone sync "$WORK_DIR" "$RCLONE_REMOTE" "${sync_flags[@]}"

echo "publish-apt: done. Clients add:"
echo "  deb [signed-by=/usr/share/keyrings/soul-stack.gpg] https://<r2-public-host>/ $SUITE $COMPONENT"
