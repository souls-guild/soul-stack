#!/usr/bin/env bash
#
# dev/web-run.sh — vite dev-сервер companion-репо soul-stack-web.
#
# Зачем отдельный скрипт: `npm run dev` без `--host` биндит vite только на IPv6
# [::1], и http://127.0.0.1:5173 отказывает (connection refused). `--host`
# заставляет vite слушать на всех интерфейсах, включая IPv4-loopback.
#
# Параметры через env:
#   WEB_DIR        — каталог web-репо (default ../soul-stack-web, sibling)
#   KEEPER_DEV_DIR — корень dev-артефактов для лога (default /tmp/keeper-dev)
#   REPO_ROOT      — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
WEB_DIR="${WEB_DIR:-${REPO_ROOT}/../soul-stack-web}"

WEB_LOG="${KEEPER_DEV_DIR}/web-dev.log"
WEB_URL="http://127.0.0.1:5173"

log()  { printf '[web-run] %s\n' "$*" >&2; }
fail() { printf '[web-run] [fail] %s\n' "$*" >&2; exit 1; }

[ -d "${WEB_DIR}" ] || fail "web-репо не найдено (${WEB_DIR}) — задай WEB_DIR=<путь>"
command -v npm >/dev/null 2>&1 || fail "npm не найден в PATH"

mkdir -p "${KEEPER_DEV_DIR}"

# Гасим только vite ЭТОГО web-репо (по WEB_DIR в cmdline), а не любой
# node-vite в системе — иначе убьёт vite-сервер другого проекта.
pkill -f "vite.*${WEB_DIR}" || true
sleep 1

# `--host` обязателен: без него vite слушает только [::1], 127.0.0.1 отказывает.
log "запускаю vite (cwd=${WEB_DIR}, log=${WEB_LOG})"
( cd "${WEB_DIR}" && nohup npm run dev -- --host > "${WEB_LOG}" 2>&1 & )

# Ждём 200 на IPv4-loopback (до 30s).
log "жду ${WEB_URL} (до 30s)"
for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${WEB_URL}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then
        log "web готов: ${WEB_URL}"
        printf 'web %s\n' "${WEB_URL}"
        exit 0
    fi
    sleep 1
done

log "vite не ответил 200 за 30s — хвост ${WEB_LOG}:"
tail -n 30 "${WEB_LOG}" >&2 || true
fail "web dev-сервер не поднялся"
