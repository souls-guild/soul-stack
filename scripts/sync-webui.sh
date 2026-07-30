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
WEB_REPO="${1:-$(cd "${CORE_REPO}/../soul-stack-web" && pwd)}"

SRC="${WEB_REPO}/dist"
DST="${CORE_REPO}/keeper/internal/webui/assets"

if [[ ! -d "${WEB_REPO}" ]]; then
  echo "sync-webui.sh: companion soul-stack-web not found: ${WEB_REPO}" >&2
  exit 2
fi

# dist/ may be missing (a fresh companion checkout) or stale. We build
# if dist/index.html is absent; otherwise we trust the existing build
# (rebuilding after source edits is on the operator - `npm run build`).
if [[ ! -f "${SRC}/index.html" ]]; then
  echo "sync-webui.sh: dist missing - building companion (npm run build)"
  (cd "${WEB_REPO}" && npm run build)
fi

if [[ ! -f "${SRC}/index.html" ]]; then
  echo "sync-webui.sh: build did not produce ${SRC}/index.html - check the companion build" >&2
  exit 2
fi

# PROVENANCE (NIM-277). The embedded bundle is a build artefact of another
# repository committed into this one, and until now it carried no record of
# WHERE it came from: looking at core, nobody could say which soul-stack-web
# commit produced the bytes in assets/. That is why an unpaired web merge is
# invisible in review — the assets diff is minified noise, and there is no line
# a reviewer can read.
#
# Refuse to vendor from a dirty companion: a bundle built from uncommitted work
# is not reproducible, and recording its SHA would be a lie.
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
WEB_DESC="$(git -C "${WEB_REPO}" describe --tags --always 2>/dev/null || echo "-")"
WEB_BRANCH="$(git -C "${WEB_REPO}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "-")"

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
cat > "${CORE_REPO}/keeper/internal/webui/WEBUI_SOURCE" <<EOF
# Provenance of keeper/internal/webui/assets/ (NIM-277). Written by
# scripts/sync-webui.sh; do not edit by hand. Its purpose is to make an unpaired
# web merge visible: if this SHA does not move when the UI changed, the vendored
# bundle is stale.
commit=${WEB_SHA}
describe=${WEB_DESC}
branch=${WEB_BRANCH}
EOF

echo "sync-webui.sh: recorded companion commit ${WEB_SHA} (${WEB_DESC}) in keeper/internal/webui/WEBUI_SOURCE"
echo "sync-webui.sh: done. Run 'make check' next."
