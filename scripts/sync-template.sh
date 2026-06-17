#!/usr/bin/env bash
# sync-template.sh — синхронизация template-дерева плагина между companion-repo
# (source of truth) и core-repo embed.
#
# Источник правды: ../soul-stack-plugins/soul-mod-template/
# Зеркало:          soul-lint/internal/plugininit/template/ (go:embed)
#
# Запускать после правки template в companion-repo. После запуска — `make check`
# в core-repo, затем коммит обоих репозиториев.
#
# Использование:
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

# Полное зеркало: --delete, чтобы переименования/удаления подхватились.
# Если rsync недоступен — fallback на rm+cp.
if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "${SRC}/" "${DST}/"
else
  rm -rf "${DST}"
  mkdir -p "${DST}"
  cp -R "${SRC}/." "${DST}/"
fi

echo "sync-template.sh: done. Run 'make check' next."
