#!/usr/bin/env bash
# sync-webui.sh — вендоринг собранного build-снапшота UI из companion-репо
# (source of truth) в core-repo embed (ADR-055, single-binary keeper с UI).
#
# Источник правды: ../soul-stack-web/dist/   (vite-build, base:'/ui/')
# Зеркало:          keeper/internal/webui/assets/   (go:embed, раздаётся на /ui)
#
# Папка зеркала — assets/ (НЕ dist/): gitignore-правило `dist/` молча съело бы
# дерево и embed получил бы пустую FS (ADR-055 §а). assets/ нейтрально к gitignore.
#
# Запускать после правки/сборки UI в companion-репо. После запуска — `make check`
# в core-repo (включая check-webui), затем коммит обоих репозиториев.
#
# Использование:
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

# dist/ может отсутствовать (свежий чекаут companion) или устареть. Собираем,
# если dist/index.html нет; в остальных случаях доверяем готовому build-у
# (пересборку при правке исходников делает оператор сам — `npm run build`).
if [[ ! -f "${SRC}/index.html" ]]; then
  echo "sync-webui.sh: dist отсутствует — собираю companion (npm run build)"
  (cd "${WEB_REPO}" && npm run build)
fi

if [[ ! -f "${SRC}/index.html" ]]; then
  echo "sync-webui.sh: build не дал ${SRC}/index.html — проверь сборку companion" >&2
  exit 2
fi

echo "sync-webui.sh: ${SRC} → ${DST}"

# Полное зеркало: --delete, чтобы переименованные хеш-чанки и удалённые файлы
# (включая стаб app.js/index.html пилота) подхватились. Если rsync недоступен —
# fallback на rm+cp.
mkdir -p "${DST}"
if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "${SRC}/" "${DST}/"
else
  rm -rf "${DST}"
  mkdir -p "${DST}"
  cp -R "${SRC}/." "${DST}/"
fi

echo "sync-webui.sh: done. Run 'make check' next."
