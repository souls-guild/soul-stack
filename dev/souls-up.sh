#!/usr/bin/env bash
#
# dev/souls-up.sh — переподнять локальный флот souls (все localhost-процессы).
#
# Зачем: при рестарте keeper / смене суток (чистка /tmp) souls слетают в
# disconnected. Скрипт переподнимает их по реестру БД стенда: на каждый
# зарегистрированный sid пишет soul.yml (если нет), онбордит (если нет seed) и
# (пере)запускает `soul run`. Covens сохранены в БД — заново НЕ регистрируем.
#
# NIM-25: параметризован профилем стенда (DEV_STAND). Пустой DEV_STAND = default
# (как исторически: БД keeper, keeper openapi :8080 / ES :9443 / bootstrap :9442,
# каталоги /tmp/keeper-dev, SID как в реестре). Непустой DEV_STAND — второй+ стенд
# рядом: своя БД keeper_<slug>, свои порты keeper (offset), каталоги
# /tmp/keeper-dev-<slug>; derived-переменные — dev/stand-env.sh.
#
# ЛЁГКИЙ РЕЖИМ (DEDICATED_INFRA=0, default): Redis ОБЩИЙ на все стенды, presence/
# SID-lease живут в глобальных ключах soul:<sid>:hb / soul:<sid>:lock — поэтому
# один флот душ за раз на стенд; SID, пересекающиеся с соседним стендом, НЕ
# поддержаны (столкнутся в общем Redis) — регистрируй души стенда с namespace в
# SID (напр. web-01.<slug>.example.com). HA/failover-демо (несколько флотов на
# пересекающихся SID) — только DEDICATED_INFRA=1 (свой Redis).
#
# Идемпотентен: повторный запуск гасит старый `soul run` каждого sid ПО PID
# (per-sid pidfile) и поднимает заново; уже-валидный seed не переонбордит.
#
# Параметры через env:
#   DEV_STAND — идентификатор стенда (пусто=default), см. dev/stand-env.sh
#   REPO_ROOT — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Профиль стенда: STAND_DEV_DIR / STAND_SLUG / OPENAPI_PORT / ES_PORT / BOOTSTRAP_PORT /
# PG_DB / STACK_PREFIX / DEDICATED_INFRA / VAULT_ADDR / …
source "${SCRIPT_DIR}/stand-env.sh"

SOUL_BIN="${REPO_ROOT}/soul/bin/soul"
MINT_JWT="${SCRIPT_DIR}/mint-jwt.sh"
VAULT_CA="${STAND_DEV_DIR}/tls/vault-ca.crt"
API_BASE="http://127.0.0.1:${OPENAPI_PORT}"

log()  { printf '[souls-up] %s\n' "$*" >&2; }
fail() { printf '[souls-up] [fail] %s\n' "$*" >&2; exit 1; }

log "стенд: slug=${STAND_SLUG:-<default>} dir=${STAND_DEV_DIR} db=${PG_DB} api=${API_BASE}"

# sid_safe_for_stand SID — 0, если SID не столкнётся в ОБЩЕМ Redis (soul:<sid>:hb|lock)
# с соседним стендом: default-стенд (нет slug), dedicated (свой Redis) или SID уже
# несёт namespace стенда. namespace выводим ЛОКАЛЬНО из STAND_SLUG (не из stand-env). F7.
sid_safe_for_stand() {
    [ -n "${STAND_SLUG}" ] || return 0
    [ "${DEDICATED_INFRA}" = "1" ] && return 0
    case "$1" in *".${STAND_SLUG}."*) return 0 ;; esac
    return 1
}

# 1. Бинарь soul.
if [ ! -x "${SOUL_BIN}" ]; then
    log "soul-бинарь не найден (${SOUL_BIN}) — собираю (go build ./cmd/soul)"
    (cd "${REPO_ROOT}/soul" && go build -o bin/soul ./cmd/soul) \
        || fail "сборка soul не удалась — собери вручную: make build"
fi

[ -s "${VAULT_CA}" ] || fail "нет Vault-CA (${VAULT_CA}) — запусти 'DEV_STAND=${STAND_SLUG} make dev-provision'"

# 2. Список зарегистрированных sid из БД стенда (${PG_DB}).
SIDS="$(docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -tA -c 'SELECT sid FROM souls' 2>/dev/null)" \
    || fail "не удалось прочитать souls из БД (${STACK_PREFIX}-postgres/${PG_DB} поднят? 'make dev-up' + 'DEV_STAND=${STAND_SLUG} make dev-provision')"

if [ -z "${SIDS}" ]; then
    log "реестр souls (${PG_DB}) пуст — нечего поднимать (зарегистрируй souls через API/UI)"
    exit 0
fi

# 3. Admin-JWT для issue-token-вызовов (mint-jwt сам параметризован стендом — iss/KV/vault).
log "минчу admin-JWT для issue-token"
ADMIN_JWT="$(AID=archon-alice ROLES='["cluster-admin"]' TTL=3600 bash "${MINT_JWT}")" \
    || fail "не удалось выпустить admin-JWT (см. mint-jwt вывод выше)"

# write_soul_yml DIR SID — записать дефолтный soul.yml для sid, если его нет.
# Схема ключей — shared/config/soul.go (SoulConfig). priority:1 = единственный
# dev-keeper стенда (ES_PORT/BOOTSTRAP_PORT). Per-sid modules/seed-каталоги внутри DIR.
write_soul_yml() {
    local dir="$1" sid="$2" cfg="$1/soul.yml"
    if [ -f "${cfg}" ]; then
        return 0
    fi
    log "  пишу soul.yml для ${sid}"
    cat > "${cfg}" <<YAML
sid: ${sid}

paths:
  modules: ${dir}/modules
  seed:    ${dir}/seed

keeper:
  endpoints:
    - host: 127.0.0.1
      event_stream_port: ${ES_PORT}
      bootstrap_port: ${BOOTSTRAP_PORT}
      priority: 1
  failback:
    enabled: true
  tls:
    ca: ${VAULT_CA}

soulprint:
  refresh_interval: 5m

logging:
  level: info
  format: text
YAML
}

# onboard_if_needed DIR SID CFG — выпустить bootstrap-token и `soul init`, если
# нет валидного активного seed. Seed валиден только когда по симлинку
# <DIR>/seed/current реально лежат три обязательных файла cert/key/ca (раскладка
# soul/internal/seed). Сам факт уцелевшего симлинка current недостаточен:
# после /tmp-чистки симлинк может пережить удаление целевой версии (vN/ пуста) —
# тогда `soul run` падает «SoulSeed not found». sigil_pubkey.pem опционален —
# не требуем. soul init поверх битого seed безопасен: bootstrap.Run не делает
# pre-check на существующий seed, seed.Write всегда пишет новую версию vN+1 и
# атомарно переставляет current (битая пустая vN/ не мешает). Перед init на
# всякий случай снимаем оборванный симлинк current, если cert.pem реально нет.
onboard_if_needed() {
    local dir="$1" sid="$2" cfg="$3"
    if [ -f "${dir}/seed/current/cert.pem" ] \
        && [ -f "${dir}/seed/current/key.pem" ] \
        && [ -f "${dir}/seed/current/ca.pem" ]; then
        log "  seed валиден — пропускаю init (${sid})"
        return 0
    fi
    # Битый/выпотрошенный seed: оборванный симлинк current (целевая vN/ пуста или
    # удалена) убираем до init, чтобы не оставлять висячую ссылку на пустоту.
    if [ -L "${dir}/seed/current" ] && [ ! -f "${dir}/seed/current/cert.pem" ]; then
        log "  битый seed (current указывает на пустую версию) — чищу симлинк (${sid})"
        rm -f "${dir}/seed/current"
    fi
    log "  онбордю ${sid} (issue-token force=true → soul init)"
    local resp bt
    resp="$(curl -s -X POST \
        -H "Authorization: Bearer ${ADMIN_JWT}" \
        "${API_BASE}/v1/souls/${sid}/issue-token?force=true")" \
        || { log "  [warn] issue-token HTTP-вызов не прошёл для ${sid} — пропуск"; return 1; }
    bt="$(printf '%s' "${resp}" | python3 -c '
import sys, json
try:
    print(json.load(sys.stdin).get("bootstrap_token", ""))
except Exception:
    pass
' 2>/dev/null)"
    if [ -z "${bt}" ]; then
        log "  [warn] не получил bootstrap_token для ${sid} (ответ: ${resp}) — пропуск"
        return 1
    fi
    if ! "${SOUL_BIN}" init --config="${cfg}" --token="${bt}" >>"${dir}/soul.log" 2>&1; then
        log "  [warn] soul init не удался для ${sid} (хвост ${dir}/soul.log)"
        return 1
    fi
    log "  ${sid} онбордён"
}

# _kill_pid_if_soul PID CFG — kill -9 строго по PID, только если он жив и cmdline
# содержит cfg-путь (страховка от переиспользования PID).
_kill_pid_if_soul() {
    local p="$1" cfg="$2"
    [ -n "${p}" ] || return 0
    kill -0 "${p}" 2>/dev/null || return 0
    grep -qa -- "${cfg}" "/proc/${p}/cmdline" 2>/dev/null || return 0
    log "  гашу старый soul (pid=${p})"
    kill -9 "${p}" 2>/dev/null || true
}

# kill_old_soul PIDFILE CFG — погасить прежний `soul run` sid-а ПО PID из pidfile
# (не pkill по общему паттерну — задел бы души соседних стендов в общем Redis).
# Fallback (миграция со старого запуска без pidfile) — PID по ТОЧНОМУ cfg-пути
# (per-sid, не общий паттерн), гасим его же по PID.
kill_old_soul() {
    local pidfile="$1" cfg="$2"
    if [ -f "${pidfile}" ]; then
        _kill_pid_if_soul "$(cat "${pidfile}" 2>/dev/null || true)" "${cfg}"
        rm -f "${pidfile}"
    fi
    if command -v pgrep >/dev/null 2>&1; then
        for p in $(pgrep -f "soul run.*${cfg}" 2>/dev/null || true); do
            _kill_pid_if_soul "${p}" "${cfg}"
        done
    fi
}

# Цикл по sid: F7-guard, каталог, soul.yml, modules, онбординг, (пере)запуск.
while IFS= read -r sid; do
    [ -n "${sid}" ] || continue

    # F7: лёгкий non-default стенд — SID без namespace стенда столкнётся в ОБЩЕМ
    # Redis (soul:<sid>:hb|lock) с соседом на том же SID. Не поднимаем такой SID
    # (гарантия непересечения); чинится регистрацией души с namespaced SID.
    if ! sid_safe_for_stand "${sid}"; then
        log "  [warn] ${sid} без namespace '${STAND_SLUG}' — риск коллизии в общем Redis; soul run ПРОПУЩЕН (перерегистрируй sid с '.${STAND_SLUG}.' или ставь DEDICATED_INFRA=1)"
        continue
    fi

    dir="${STAND_DEV_DIR}/${sid}"
    cfg="${dir}/soul.yml"
    pidfile="${dir}/soul.pid"
    mkdir -p "${dir}/modules"
    log "soul ${sid} (${dir})"

    write_soul_yml "${dir}" "${sid}"
    onboard_if_needed "${dir}" "${sid}" "${cfg}" || true

    # Нет валидного seed после онбординга — run всё равно упадёт; пропускаем
    # запуск. Критерий тот же, что в onboard_if_needed: три файла по current.
    if ! { [ -f "${dir}/seed/current/cert.pem" ] \
        && [ -f "${dir}/seed/current/key.pem" ] \
        && [ -f "${dir}/seed/current/ca.pem" ]; }; then
        log "  нет валидного seed — soul run пропущен (${sid})"
        continue
    fi

    # Гасим прежний `soul run` ЭТОГО sid ПО PID (per-sid pidfile).
    kill_old_soul "${pidfile}" "${cfg}"
    nohup "${SOUL_BIN}" run --config="${cfg}" >> "${dir}/soul.log" 2>&1 </dev/null &
    printf '%s\n' "$!" > "${pidfile}"
    log "  soul run запущен (pid=$!, pidfile=${pidfile})"
done <<EOF
${SIDS}
EOF

# Дать souls подключиться и показать сводку статусов.
sleep 5
log "статусы souls (${PG_DB}):"
docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -c \
    'SELECT status, count(*) FROM souls GROUP BY status ORDER BY status' >&2 \
    || log "[warn] не удалось прочитать статусы из БД"
