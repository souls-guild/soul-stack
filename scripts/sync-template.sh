#!/usr/bin/env bash
# sync-template.sh - syncs the plugin template tree between the companion repo
# (source of truth) and the core-repo embed.
#
# Source of truth: ../soul-stack-plugins/soul-mod-template/
# Mirror:           soul-lint/internal/plugininit/template/ (go:embed)
#
# Run after editing the template in the companion repo. After running, `make check`
# in the core repo, then commit both repositories.
#
# Usage:
#   scripts/sync-template.sh              # default: ../soul-stack-plugins
#   scripts/sync-template.sh /path/to/soul-stack-plugins
set -euo pipefail

CORE_REPO="$(cd "$(dirname "$0")/.." && pwd)"
PLUGINS_REPO="${1:-$(cd "${CORE_REPO}/../soul-stack-plugins" && pwd)}"

SRC="${PLUGINS_REPO}/soul-mod-template"
DST="${CORE_REPO}/soul-lint/internal/plugininit/template"

if [[ ! -d "${SRC}" ]]; then
  echo "sync-template.sh: source not found: ${SRC}" >&2
  exit 2
fi

echo "sync-template.sh: ${SRC} → ${DST}"

# Full mirror: --delete, so renames/deletions are picked up.
# If rsync is unavailable - fallback to rm+cp.
if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "${SRC}/" "${DST}/"
else
  rm -rf "${DST}"
  mkdir -p "${DST}"
  cp -R "${SRC}/." "${DST}/"
fi

echo "sync-template.sh: done. Run 'make check' next."
