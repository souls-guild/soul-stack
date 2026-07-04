#!/usr/bin/env bash
#
# dev/souls-docker-down.sh — снести docker-флот souls и вычистить его из реестра (NIM-26).
#
# Обратная к dev/souls-docker-up.sh. Убирает ТОЛЬКО контейнеры soul-docker-*
# (для стенда — soul-docker-<stand>-*; чужие docker-объекты не трогает), затем удаляет
# их записи из реестра keeper и per-soul dev-каталоги.
#
# Реестр чистится напрямую через psql: DELETE-эндпоинта /v1/souls/{sid} в
# Operator API нет (проверено — huma_soul_op.go, audit-guard). Каскад по FK:
# bootstrap_tokens/soul_seeds/incarnation_choir_voices.sid → souls(sid) ON DELETE
# CASCADE (миграции 008/009/060) — один DELETE FROM souls чистит эти 3 таблицы.
# apply_runs/state_history/audit_log хранят sid БЕЗ FK → осиротеют (для dev это
# приемлемый аудит-след, не баг).
#
# Параметры через env:
#   DEV_STAND — идентификатор стенда (пусто=default; префикс/БД/dev-dir — dev/stand-env.sh)
#   REPO_ROOT — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Профиль стенда: STAND_DEV_DIR / STACK_PREFIX / PG_DB / STAND_SLUG (симметрия с up). NIM-25.
source "${SCRIPT_DIR}/stand-env.sh"

PREFIX="soul-docker${STAND_SLUG:+-${STAND_SLUG}}"

log()  { printf '[souls-docker-down] %s\n' "$*" >&2; }
fail() { printf '[souls-docker-down] [fail] %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "docker не найден в PATH"

# 1. Снести контейнеры soul-docker-* (anchored-фильтр — только наши).
ids="$(docker ps -aq --filter "name=^${PREFIX}-[0-9]+$" 2>/dev/null || true)"
if [ -n "${ids}" ]; then
    log "сношу контейнеры:"
    docker ps -a --filter "name=^${PREFIX}-[0-9]+$" --format '  {{.Names}} ({{.Status}})' >&2 || true
    # shellcheck disable=SC2086
    docker rm -f ${ids} >/dev/null 2>&1 || log "[warn] часть контейнеров не удалилась"
else
    log "контейнеры ${PREFIX}-* не найдены"
fi

# 2. Вычистить из реестра keeper (psql; каскад по FK). Инфра docker-compose.
if docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -c \
    "DELETE FROM souls WHERE sid ~ '^${PREFIX}-[0-9]+$'" >&2 2>/dev/null; then
    log "реестр очищен (souls + каскад bootstrap_tokens/soul_seeds/incarnation_choir_voices)"
else
    log "[warn] не удалось вычистить реестр (${STACK_PREFIX}-postgres поднят? 'make dev-up')"
fi

# 3. Почистить per-soul dev-каталоги.
shopt -s nullglob
dirs=("${STAND_DEV_DIR}/${PREFIX}"-*)
if [ "${#dirs[@]}" -gt 0 ]; then
    rm -rf "${dirs[@]}"
    log "удалены каталоги ${STAND_DEV_DIR}/${PREFIX}-*"
fi
shopt -u nullglob

log "готово"
