#!/usr/bin/env bash
# sync-webui.sh - vendors the built UI build snapshot from the companion repo
# (source of truth) into the core-repo embed (ADR-055, single-binary keeper with UI).
#
# Source of truth: ../soul-stack-web/dist/   (vite-build, base:'/ui/')
# Mirror:           keeper/internal/webui/assets/   (go:embed, served at /ui)
#
# The mirror folder is assets/ (NOT dist/): the gitignore rule `dist/` would silently
# swallow the tree and embed would get an empty FS (ADR-055 §a). assets/ is neutral to gitignore.
#
# Run after editing/building the UI in the companion repo. After running - `make check`
# in the core repo (including check-webui), then commit both repositories.
#
# Usage:
#   scripts/sync-webui.sh                 # default: ../soul-stack-web
#   scripts/sync-webui.sh /path/to/soul-stack-web
set -euo pipefail

CORE_REPO="$(cd "$(dirname "$0")/.." && pwd)"
# NOT `$(cd ... && pwd)` here: under `set -e` that dies with a raw `cd: no such
# file or directory` and rc=1 the moment the companion is absent - i.e. exactly
# the case the friendly message below exists for, reported instead as a shell
# error with the wrong exit code. Normalise AFTER the directory is known to exist.
WEB_REPO="${1:-${CORE_REPO}/../soul-stack-web}"

if [[ ! -d "${WEB_REPO}" ]]; then
  echo "sync-webui.sh: companion soul-stack-web not found: ${WEB_REPO}" >&2
  echo "  Clone it next to this repository, or name it: scripts/sync-webui.sh /path/to/soul-stack-web" >&2
  exit 2
fi
WEB_REPO="$(cd "${WEB_REPO}" && pwd)"

SRC="${WEB_REPO}/dist"
DST="${CORE_REPO}/keeper/internal/webui/assets"

# PROVENANCE (NIM-277). The embedded bundle is a build artefact of another
# repository committed into this one, and until now it carried no record of
# WHERE it came from: looking at core, nobody could say which soul-stack-web
# commit produced the bytes in assets/. That is why an unpaired web merge is
# invisible in review — the assets diff is minified noise, and there is no line
# a reviewer can read.
#
# Refuse to vendor from a dirty companion: a bundle built from uncommitted work
# is not reproducible, and recording its SHA would be a lie. This runs BEFORE the
# build below, because the answer does not depend on building and refusing after
# a full vite run only wastes the operator's time. dist/ is gitignored in the
# companion, so the build itself never makes the tree dirty.
WEB_SHA="$(git -C "${WEB_REPO}" rev-parse HEAD 2>/dev/null || true)"
if [[ -z "${WEB_SHA}" ]]; then
  echo "sync-webui.sh: ${WEB_REPO} is not a git checkout - refusing to vendor a bundle with no provenance" >&2
  exit 2
fi
if [[ -n "$(git -C "${WEB_REPO}" status --porcelain 2>/dev/null)" ]]; then
  echo "sync-webui.sh: companion working tree is dirty - the bundle would not be reproducible." >&2
  echo "sync-webui.sh: commit or stash in ${WEB_REPO}, rebuild, then sync again." >&2
  exit 2
fi

# ALWAYS build. This used to build only when dist/index.html was absent and
# otherwise trust whatever was already there, which quietly broke the one property
# every downstream check rests on: that the mirrored bytes and the recorded commit
# describe the same thing. A companion checkout whose dist/ was built before the
# last few commits would be mirrored as-is while commit= below is read fresh from
# HEAD - so the bundle stays stale, its fingerprint matches (assets_sha256 is
# written below from these same bytes), and its provenance now points at the tip
# (check-webui-freshness is satisfied). All three guards go green over exactly the
# defect they exist to catch, and this script is the remedy every one of them
# recommends, so that is the last place that can afford to be optimistic.
#
# The cost is a rebuild on every sync. That is bounded - this is an on-demand
# target, not part of `make check` - and vite is incremental enough that an
# unchanged tree rebuilds to identical bytes.
echo "sync-webui.sh: building companion (npm run build) in ${WEB_REPO}"
if ! (cd "${WEB_REPO}" && npm run build); then
  echo "sync-webui.sh: companion build failed - the vendored bundle was NOT touched." >&2
  echo "sync-webui.sh: if this is a fresh checkout, install its dependencies first: (cd ${WEB_REPO} && npm ci)" >&2
  exit 2
fi

if [[ ! -f "${SRC}/index.html" ]]; then
  echo "sync-webui.sh: build did not produce ${SRC}/index.html - check the companion build" >&2
  exit 2
fi

WEB_DESC="$(git -C "${WEB_REPO}" describe --tags --always 2>/dev/null || echo "-")"
# NOT `rev-parse --abbrev-ref HEAD`: on a detached companion checkout that prints
# the literal string `HEAD` and exits 0, so the provenance would claim the bundle
# came from a branch named HEAD - and check-webui-freshness would then ask the
# remote for refs/heads/HEAD and report a missing branch instead of a detached
# source. Recording nothing is the truthful answer; the freshness check falls back
# to the branch being assembled when this line is empty.
WEB_BRANCH="$(git -C "${WEB_REPO}" symbolic-ref --quiet --short HEAD 2>/dev/null || printf '')"

# Say what an empty branch= costs, here, where it is still cheap to fix. The
# freshness check falls back to the recorded branch when the branch being
# assembled is not in the companion yet; with nothing recorded there is nothing
# to fall back TO, and on release/* that becomes a red whose real cause (this
# checkout was detached, an hour ago, in another repository) is invisible from
# the tree that reds.
if [[ -z "${WEB_BRANCH}" ]]; then
  {
    echo "sync-webui.sh: NOTE - the companion is on a detached HEAD, so the provenance will record no"
    echo "  branch=. That is truthful, but it removes the fallback check-webui-freshness uses when the"
    echo "  companion has not opened the branch being assembled yet. Check out the branch you are"
    echo "  vendoring from and re-run if you want that fallback."
  } >&2
fi

echo "sync-webui.sh: ${SRC} -> ${DST}"

# Full mirror: --delete so renamed hash chunks and removed files
# (including the pilot's stub app.js/index.html) get picked up. If rsync is
# unavailable - fall back to rm+cp.
mkdir -p "${DST}"
if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "${SRC}/" "${DST}/"
else
  rm -rf "${DST}"
  mkdir -p "${DST}"
  cp -R "${SRC}/." "${DST}/"
fi

# The provenance record lives OUTSIDE assets/ on purpose: everything under
# assets/ is go:embed'ed and shipped to browsers, and this is a build fact for
# reviewers, not a served file. One key per line so a re-sync shows up in the
# diff as a readable SHA change rather than as minified churn.
#
# assets_sha256 is a fingerprint of the mirrored tree, and it is what makes the
# commit= line above trustworthy (NIM-341). Without it, "the SHA did not move"
# has two possible meanings — the bundle did not change, or the bundle changed
# without going through this script — and a reviewer cannot tell them apart. With
# it, `make check-webui-embed` catches a hand-edited bundle, or one vendored by an
# older copy of this script, on every CI push and without needing the companion.
ASSETS_SHA="$(cd "${DST}" && find . -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"

cat > "${CORE_REPO}/keeper/internal/webui/WEBUI_SOURCE" <<EOF
# Provenance of keeper/internal/webui/assets/ (NIM-277). Written by
# scripts/sync-webui.sh; do not edit by hand. Its purpose is to make an unpaired
# web merge visible: if this SHA does not move when the UI changed, the vendored
# bundle is stale.
commit=${WEB_SHA}
describe=${WEB_DESC}
branch=${WEB_BRANCH}
assets_sha256=${ASSETS_SHA}
EOF

echo "sync-webui.sh: recorded companion commit ${WEB_SHA} (${WEB_DESC}) in keeper/internal/webui/WEBUI_SOURCE"
echo "sync-webui.sh: done. Run 'make check' next."
