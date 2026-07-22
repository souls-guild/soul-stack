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

echo "sync-webui.sh: done. Run 'make check' next."
