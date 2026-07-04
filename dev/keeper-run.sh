#!/usr/bin/env bash
#
# dev/keeper-run.sh — рестарт локального keeper с ПОЛНЫМ dev-env.
#
# NIM-25: параметризован профилем стенда (DEV_STAND). Пустой DEV_STAND = default-стенд
# (поведение как исторически: /tmp/keeper-dev, порты 8080/8081/9090/9442/9443, kid
# keeper-dev-01). Непустой DEV_STAND — второй+ стенд рядом (свой dev-dir, порты offset,
# БД keeper_<slug>, KV secret/keeper/<slug>); derived-переменные — dev/stand-env.sh.
#
# Идемпотентен: рендерит конфиг стенда из шаблона, гасит старый keeper ЭТОГО стенда
# по PID (pidfile + держатель metrics-порта; не pkill-по-имени, чтобы не задеть соседей),
# ждёт освобождения порта, поднимает заново, ждёт healthz.
#
# Параметры через env:
#   DEV_STAND — идентификатор стенда (пусто=default), см. dev/stand-env.sh
#   REPO_ROOT — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Профиль стенда: STAND_DEV_DIR / KID / порты / KV-префикс / STACK_PREFIX / REDIS_ADDR / …
source "${SCRIPT_DIR}/stand-env.sh"

KEEPER_BIN="${REPO_ROOT}/keeper/bin/keeper"
KEEPER_TMPL="${SCRIPT_DIR}/keeper.dev.yml.tmpl"
KEEPER_CONFIG="${STAND_DEV_DIR}/keeper.dev.yml"
KEEPER_LOG="${STAND_DEV_DIR}/keeper.log"
KEEPER_CRT="${STAND_DEV_DIR}/tls/keeper.crt"
PID_FILE="${STAND_DEV_DIR}/keeper.pid"
HEALTHZ_URL="http://127.0.0.1:${OPENAPI_PORT}/healthz"

log()  { printf '[keeper-run] %s\n' "$*" >&2; }
fail() { printf '[keeper-run] [fail] %s\n' "$*" >&2; exit 1; }

log "стенд: slug=${STAND_SLUG:-<default>} slot=${STAND_SLOT} dir=${STAND_DEV_DIR} kid=${KID} openapi=${OPENAPI_PORT}"

# 1. Бинарь keeper. Отсутствует — собрать.
if [ ! -x "${KEEPER_BIN}" ]; then
    log "keeper-бинарь не найден (${KEEPER_BIN}) — собираю (go build ./cmd/keeper)"
    (cd "${REPO_ROOT}/keeper" && go build -o bin/keeper ./cmd/keeper) \
        || fail "сборка keeper не удалась — собери вручную: make build"
fi

# 2. Per-стенд каталоги: корень + оба cache-каталога file://-резолва (keeper их сам
# не создаёт — без них резолв артефактов падает 502).
mkdir -p "${STAND_DEV_DIR}" "${STAND_DEV_DIR}/services" "${STAND_DEV_DIR}/destiny-cache"

# 3. TLS-материал (выписывает dev-provision в STAND_DEV_DIR/tls). НЕ запускаем provision
# сами — дорогой шаг с побочкой, оставляем явным шагом оператора.
if [ ! -s "${KEEPER_CRT}" ]; then
    fail "нет TLS-материала (${KEEPER_CRT}) — запусти 'DEV_STAND=${STAND_SLUG} make dev-provision' и повтори"
fi

# 4. Рендер конфига стенда из шаблона. Whitelist derived-переменных — из stand-env
# (KEEPER_RENDER_WHITELIST, единый источник с dev-smoke/check-stand-template; вкл. $VAULT_ADDR).
command -v envsubst >/dev/null 2>&1 || fail "envsubst не найден (пакет gettext) — нужен для рендера конфига"
envsubst "${KEEPER_RENDER_WHITELIST}" < "${KEEPER_TMPL}" > "${KEEPER_CONFIG}"
log "конфиг отрендерен: ${KEEPER_CONFIG}"

# 5. Гасим старый keeper ЭТОГО стенда ПО PID (не pkill-по-имени): pidfile + fallback
# на держателя metrics-порта стенда (порт уникален на стенд — не заденет соседей).
kill_old() {
    local pid="$1"
    [ -n "${pid}" ] || return 0
    kill -0 "${pid}" 2>/dev/null || return 0
    grep -qa keeper "/proc/${pid}/cmdline" 2>/dev/null || return 0
    log "гашу старый keeper стенда (pid=${pid})"
    kill -9 "${pid}" 2>/dev/null || true
}
if [ -f "${PID_FILE}" ]; then
    kill_old "$(cat "${PID_FILE}" 2>/dev/null || true)"
    rm -f "${PID_FILE}"
fi
if command -v lsof >/dev/null 2>&1; then
    for p in $(lsof -t -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN 2>/dev/null || true); do
        kill_old "${p}"
    done
fi
sleep 1

# 6. Ждём освобождения metrics-порта стенда.
log "жду освобождения :${METRICS_PORT} (до 20s)"
for _ in $(seq 1 20); do
    lsof -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 1
done
if lsof -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
    fail ":${METRICS_PORT} всё ещё занят — кто-то держит порт (lsof -iTCP:${METRICS_PORT})"
fi

# 7. Чистка leader-leases (conductor:leader/reaper:leader). ТОЛЬКО default-стенд ИЛИ
# dedicated: в лёгком не-default режиме Redis ОБЩИЙ — DEL задел бы соседние стенды (NIM-25).
if [ -z "${STAND_SLUG}" ] || [ "${DEDICATED_INFRA}" = "1" ]; then
    if docker exec "${STACK_PREFIX}-redis" redis-cli -p 6379 DEL conductor:leader reaper:leader >/dev/null 2>&1; then
        log "leader-leases очищены (conductor:leader, reaper:leader)"
    else
        log "redis недоступен (${STACK_PREFIX}-redis) — leases не очищены; подними 'make dev-up'"
    fi
else
    log "лёгкий не-default стенд: leader-leases НЕ чистим (общий Redis — не задеть соседей)"
fi

# 8. dev-env для file://-резолва артефактов + явный dev VAULT_TOKEN=root (в env юзера
# бывает прод-токен; форсим root). VAULT_ADDR — из stand-env (лёгкий=общий :8200).
export VAULT_ADDR
export VAULT_TOKEN=root
export KEEPER_SERVICE_CACHE_DIR="${STAND_DEV_DIR}/services"
export KEEPER_DESTINY_CACHE_DIR="${STAND_DEV_DIR}/destiny-cache"
export SOUL_STACK_ALLOW_FILE_REPOS=1
# Свежая БД стенда → пустой реестр operators; без флага keeper run отказывается
# стартовать (ADR-013). KEEPER_INITIALIZE=true = bootstrap-pending (healthz живой,
# ждёт первого Архонта). Default не трогаем (init делает dev-smoke).
if [ -n "${STAND_SLUG}" ]; then export KEEPER_INITIALIZE=true; fi

# 9. Поднимаем keeper в фоне, пишем PID-файл стенда.
log "запускаю keeper (config=${KEEPER_CONFIG}, log=${KEEPER_LOG})"
nohup "${KEEPER_BIN}" run --config="${KEEPER_CONFIG}" > "${KEEPER_LOG}" 2>&1 &
KEEPER_PID=$!
printf '%s\n' "${KEEPER_PID}" > "${PID_FILE}"

# 10. Ждём healthz 200 стенда (до 30s).
log "жду healthz 200 (${HEALTHZ_URL}, до 30s)"
for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${HEALTHZ_URL}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then
        log "keeper готов: pid=${KEEPER_PID}, healthz 200 (:${OPENAPI_PORT})"
        printf 'keeper pid=%s healthz=200 openapi=%s stand=%s\n' "${KEEPER_PID}" "${OPENAPI_PORT}" "${STAND_SLUG:-default}"
        exit 0
    fi
    if ! kill -0 "${KEEPER_PID}" 2>/dev/null; then
        log "keeper-процесс умер на старте — хвост ${KEEPER_LOG}:"
        tail -n 30 "${KEEPER_LOG}" >&2 || true
        fail "keeper не стартовал"
    fi
    sleep 1
done

log "healthz не поднялся за 30s — хвост ${KEEPER_LOG}:"
tail -n 30 "${KEEPER_LOG}" >&2 || true
fail "keeper не отвечает на healthz"
