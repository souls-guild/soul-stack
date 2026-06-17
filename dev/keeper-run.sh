#!/usr/bin/env bash
#
# dev/keeper-run.sh — рестарт локального keeper с ПОЛНЫМ dev-env.
#
# Почему отдельный скрипт: при ручном `keeper run` легко потерять обязательный
# для file://-резолва env (SOUL_STACK_ALLOW_FILE_REPOS=1 + writable cache-dirs).
# Без него артефакт-лоадер отвергает file://-репо сервисов → сценарии не грузятся
# (502). Скрипт фиксирует выверенный env одной командой (`make dev-keeper`).
#
# Идемпотентен: гасит старый keeper по специфичному паттерну dev-конфига,
# ждёт освобождения порта, чистит leader-leases, поднимает заново и ждёт healthz.
#
# Параметры через env:
#   KEEPER_DEV_DIR — корень dev-артефактов (default /tmp/keeper-dev)
#   REPO_ROOT      — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

KEEPER_BIN="${REPO_ROOT}/keeper/bin/keeper"
KEEPER_CONFIG="${REPO_ROOT}/dev/keeper.dev.yml"
KEEPER_LOG="${KEEPER_DEV_DIR}/keeper.log"
KEEPER_CRT="${KEEPER_DEV_DIR}/tls/keeper.crt"

# Порт metrics-listener-а keeper-а (listen.metrics в keeper.dev.yml); по нему
# ждём освобождения после kill старого процесса.
METRICS_PORT=9090
# OpenAPI-listener — на нём живёт /healthz (listen.openapi в keeper.dev.yml).
HEALTHZ_URL="http://127.0.0.1:8080/healthz"

log()  { printf '[keeper-run] %s\n' "$*" >&2; }
fail() { printf '[keeper-run] [fail] %s\n' "$*" >&2; exit 1; }

# 1. Бинарь keeper. Отсутствует — собрать (или подсказать make build).
if [ ! -x "${KEEPER_BIN}" ]; then
    log "keeper-бинарь не найден (${KEEPER_BIN}) — собираю (go build ./cmd/keeper)"
    (cd "${REPO_ROOT}/keeper" && go build -o bin/keeper ./cmd/keeper) \
        || fail "сборка keeper не удалась — собери вручную: make build"
fi

# 2. TLS-материал. Без него keeper падает на mTLS-listener-е — подсказываем
# dev-provision (НЕ запускаем сами: provision дорогой и с побочкой, оставляем
# явным шагом оператора).
if [ ! -s "${KEEPER_CRT}" ]; then
    fail "нет TLS-материала (${KEEPER_CRT}) — запусти 'make dev-provision' и повтори"
fi

# Создаём корень + оба cache-каталога (KEEPER_SERVICE_CACHE_DIR /
# KEEPER_DESTINY_CACHE_DIR, экспортируются ниже). keeper их сам не создаёт —
# без них file://-резолв падает 502. Пути держим в синхроне с export'ами.
mkdir -p "${KEEPER_DEV_DIR}" "${KEEPER_DEV_DIR}/services" "${KEEPER_DEV_DIR}/destiny-cache"

# 3. Гасим старый keeper dev-воркфлоу (специфичный паттерн, как dev-stop).
pkill -9 -f 'keeper run.*keeper\.dev\.yml' || true
sleep 1

# 4. Ждём освобождения metrics-порта (старый процесс мог не успеть отпустить).
log "жду освобождения :${METRICS_PORT} (до 20s)"
for _ in $(seq 1 20); do
    if ! lsof -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if lsof -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
    fail ":${METRICS_PORT} всё ещё занят — кто-то держит порт (lsof -iTCP:${METRICS_PORT})"
fi

# 5. Чистим leader-leases в Redis: после kill старый keeper не отпускает
# conductor:leader / reaper:leader gracefully — свежий процесс ждёт TTL.
# DEL форсирует немедленный re-election. redis dev — без пароля.
if docker exec soul-stack-redis redis-cli -p 6379 DEL conductor:leader reaper:leader >/dev/null 2>&1; then
    log "leader-leases очищены (conductor:leader, reaper:leader)"
else
    log "redis недоступен (soul-stack-redis) — leases не очищены; подними 'make dev-up'"
fi

# 6. dev-env для file://-резолва service/destiny-артефактов (см.
# docs/dev/local-setup.md → «Артефакты service/destiny»). Без этих переменных
# артефакт-лоадер отвергает file://-репо → 502 на резолве сценариев.
export VAULT_ADDR="${VAULT_ADDR:-http://127.0.0.1:8200}"
export VAULT_TOKEN="${VAULT_TOKEN:-root}"
export KEEPER_SERVICE_CACHE_DIR="${KEEPER_DEV_DIR}/services"
export KEEPER_DESTINY_CACHE_DIR="${KEEPER_DEV_DIR}/destiny-cache"
export SOUL_STACK_ALLOW_FILE_REPOS=1

# 7. Поднимаем keeper в фоне.
log "запускаю keeper (config=${KEEPER_CONFIG}, log=${KEEPER_LOG})"
nohup "${KEEPER_BIN}" run --config="${KEEPER_CONFIG}" > "${KEEPER_LOG}" 2>&1 &
KEEPER_PID=$!

# 8. Ждём healthz 200 (до 30s).
log "жду healthz 200 (${HEALTHZ_URL}, до 30s)"
for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${HEALTHZ_URL}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then
        log "keeper готов: pid=${KEEPER_PID}, healthz 200"
        printf 'keeper pid=%s healthz=200\n' "${KEEPER_PID}"
        exit 0
    fi
    # Процесс мог упасть на старте — показываем хвост лога и выходим раньше.
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
