#!/usr/bin/env bash
#
# dev/web-run.sh — vite dev-сервер companion-репо soul-stack-web.
#
# NIM-25: параметризован профилем стенда (DEV_STAND). Пустой DEV_STAND = default-стенд
# (порт 5173, VITE_KEEPER_API на :8080, dir /tmp/keeper-dev). Непустой DEV_STAND —
# второй+ стенд рядом: свой web-порт (5173+offset), VITE_KEEPER_API на keeper стенда
# (:8080+offset), pidfile/лог в STAND_DEV_DIR; derived-переменные — dev/stand-env.sh.
#
# `--host` обязателен: без него vite слушает только [::1] и 127.0.0.1 отказывает.
# `--port ${WEB_PORT}` обязателен: vite.config.ts strictPort:true (падает при занятом
# порту) разводит стенды явным портом. Гасим старый vite ЭТОГО стенда по PID (pidfile +
# держатель web-порта), НЕ pkill-по-имени: web-репо общий на стенды, pkill задел бы соседа.
#
# Параметры через env:
#   DEV_STAND — идентификатор стенда (пусто=default), см. dev/stand-env.sh
#   WEB_DIR   — каталог web-репо (default ../soul-stack-web, sibling)
#   REPO_ROOT — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Профиль стенда: STAND_DEV_DIR / WEB_PORT / OPENAPI_PORT / STAND_SLUG.
source "${SCRIPT_DIR}/stand-env.sh"

WEB_DIR="${WEB_DIR:-${REPO_ROOT}/../soul-stack-web}"
WEB_LOG="${STAND_DEV_DIR}/web-dev.log"
WEB_URL="http://127.0.0.1:${WEB_PORT}"
PID_FILE="${STAND_DEV_DIR}/web.pid"

log()  { printf '[web-run] %s\n' "$*" >&2; }
fail() { printf '[web-run] [fail] %s\n' "$*" >&2; exit 1; }

log "стенд: slug=${STAND_SLUG:-<default>} web=${WEB_PORT} keeper-api=${OPENAPI_PORT} dir=${STAND_DEV_DIR}"

[ -d "${WEB_DIR}" ] || fail "web-репо не найдено (${WEB_DIR}) — задай WEB_DIR=<путь>"
command -v npm >/dev/null 2>&1 || fail "npm не найден в PATH"

mkdir -p "${STAND_DEV_DIR}"

# Гасим старый vite ЭТОГО стенда ПО PID (не pkill-по-имени — web-репо общий, задел бы
# соседний стенд): pidfile + fallback на держателя web-порта (npm-PID мог умереть, а
# vite-child висит на порту; порт уникален на стенд — не заденет соседей).
kill_old() {
    local pid="$1"
    [ -n "${pid}" ] || return 0
    kill -0 "${pid}" 2>/dev/null || return 0
    grep -qaE 'vite|node|npm' "/proc/${pid}/cmdline" 2>/dev/null || return 0
    log "гашу старый vite стенда (pid=${pid})"
    kill -9 "${pid}" 2>/dev/null || true
}
if [ -f "${PID_FILE}" ]; then
    kill_old "$(cat "${PID_FILE}" 2>/dev/null || true)"
    rm -f "${PID_FILE}"
fi
if command -v lsof >/dev/null 2>&1; then
    for p in $(lsof -t -nP -iTCP:"${WEB_PORT}" -sTCP:LISTEN 2>/dev/null || true); do
        kill_old "${p}"
    done
fi
sleep 1

# Ждём освобождения web-порта стенда (strictPort:true — рестарт падает, если старый
# vite ещё держит порт).
if command -v lsof >/dev/null 2>&1; then
    log "жду освобождения :${WEB_PORT} (до 20s)"
    for _ in $(seq 1 20); do
        lsof -nP -iTCP:"${WEB_PORT}" -sTCP:LISTEN >/dev/null 2>&1 || break
        sleep 1
    done
    if lsof -nP -iTCP:"${WEB_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
        fail ":${WEB_PORT} всё ещё занят — кто-то держит порт (lsof -iTCP:${WEB_PORT})"
    fi
fi

# Фронт стенда бьёт в keeper стенда: vite.config.ts proxy-target читает process.env
# при старте dev-сервера.
export VITE_KEEPER_API="http://127.0.0.1:${OPENAPI_PORT}"

# exec в подоболочке → PID подоболочки = PID npm (пишем в pidfile для гашения при рестарте).
log "запускаю vite (cwd=${WEB_DIR}, port=${WEB_PORT}, keeper-api=${OPENAPI_PORT}, log=${WEB_LOG})"
( cd "${WEB_DIR}" && exec nohup npm run dev -- --host --port "${WEB_PORT}" ) > "${WEB_LOG}" 2>&1 &
WEB_PID=$!
printf '%s\n' "${WEB_PID}" > "${PID_FILE}"

# Ждём 200 на web-порту стенда (до 30s).
log "жду ${WEB_URL} (до 30s)"
for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${WEB_URL}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then
        log "web готов: ${WEB_URL} (pid=${WEB_PID}, stand=${STAND_SLUG:-default})"
        printf 'web %s pid=%s stand=%s\n' "${WEB_URL}" "${WEB_PID}" "${STAND_SLUG:-default}"
        exit 0
    fi
    if ! kill -0 "${WEB_PID}" 2>/dev/null; then
        log "vite-процесс умер на старте — хвост ${WEB_LOG}:"
        tail -n 30 "${WEB_LOG}" >&2 || true
        fail "web dev-сервер не поднялся"
    fi
    sleep 1
done

log "vite не ответил 200 за 30s — хвост ${WEB_LOG}:"
tail -n 30 "${WEB_LOG}" >&2 || true
fail "web dev-сервер не поднялся"
