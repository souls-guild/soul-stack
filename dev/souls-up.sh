#!/usr/bin/env bash
#
# dev/souls-up.sh — переподнять локальный флот souls (все localhost-процессы).
#
# Зачем: при рестарте keeper / смене суток (чистка /tmp) souls слетают в
# disconnected. Скрипт переподнимает их по реестру БД: на каждый зарегистрированный
# sid пишет soul.yml (если нет), онбордит (если нет seed) и (пере)запускает
# `soul run`. Covens сохранены в БД — заново НЕ регистрируем, только run.
#
# Идемпотентен: повторный запуск гасит старый `soul run` каждого sid и поднимает
# заново; уже-валидный seed не переонбордит (cert валиден против неизменного
# Vault PKI-root).
#
# Параметры через env:
#   KEEPER_DEV_DIR — корень dev-артефактов (default /tmp/keeper-dev)
#   REPO_ROOT      — корень репо soul-stack (по умолчанию из пути скрипта)

set -euo pipefail

KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

SOUL_BIN="${REPO_ROOT}/soul/bin/soul"
MINT_JWT="${SCRIPT_DIR}/mint-jwt.sh"
VAULT_CA="${KEEPER_DEV_DIR}/tls/vault-ca.crt"
API_BASE="http://127.0.0.1:8080"

log()  { printf '[souls-up] %s\n' "$*" >&2; }
fail() { printf '[souls-up] [fail] %s\n' "$*" >&2; exit 1; }

# 1. Бинарь soul.
if [ ! -x "${SOUL_BIN}" ]; then
    log "soul-бинарь не найден (${SOUL_BIN}) — собираю (go build ./cmd/soul)"
    (cd "${REPO_ROOT}/soul" && go build -o bin/soul ./cmd/soul) \
        || fail "сборка soul не удалась — собери вручную: make build"
fi

[ -s "${VAULT_CA}" ] || fail "нет Vault-CA (${VAULT_CA}) — запусти 'make dev-provision'"

# 2. Список зарегистрированных sid из БД.
SIDS="$(docker exec soul-stack-postgres psql -U keeper -d keeper -tA -c 'SELECT sid FROM souls' 2>/dev/null)" \
    || fail "не удалось прочитать souls из БД (soul-stack-postgres поднят? 'make dev-up')"

if [ -z "${SIDS}" ]; then
    log "реестр souls пуст — нечего поднимать (зарегистрируй souls через API/UI)"
    exit 0
fi

# 3. Admin-JWT для issue-token-вызовов.
log "минчу admin-JWT для issue-token"
ADMIN_JWT="$(AID=archon-alice ROLES='["cluster-admin"]' TTL=3600 bash "${MINT_JWT}")" \
    || fail "не удалось выпустить admin-JWT (см. mint-jwt вывод выше)"

# write_soul_yml DIR SID — записать дефолтный soul.yml для sid, если его нет.
# Схема ключей — shared/config/soul.go (SoulConfig). priority:1 = единственный
# dev-keeper-a (9443/9442). Per-sid modules/seed-каталоги внутри DIR.
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
      event_stream_port: 9443
      bootstrap_port: 9442
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

# Цикл по sid: каталог, soul.yml, modules, онбординг, (пере)запуск.
while IFS= read -r sid; do
    [ -n "${sid}" ] || continue
    dir="${KEEPER_DEV_DIR}/${sid}"
    cfg="${dir}/soul.yml"
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

    pkill -9 -f "soul run.*${dir}/soul.yml" || true
    nohup "${SOUL_BIN}" run --config="${cfg}" >> "${dir}/soul.log" 2>&1 &
    log "  soul run запущен (pid=$!)"
done <<EOF
${SIDS}
EOF

# Дать souls подключиться и показать сводку статусов.
sleep 5
log "статусы souls:"
docker exec soul-stack-postgres psql -U keeper -d keeper -c \
    'SELECT status, count(*) FROM souls GROUP BY status ORDER BY status' >&2 \
    || log "[warn] не удалось прочитать статусы из БД"
