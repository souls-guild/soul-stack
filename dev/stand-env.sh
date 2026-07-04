#!/usr/bin/env bash
#
# dev/stand-env.sh — единый источник derived-переменных dev-стенда из DEV_STAND.
# SOURCED-хелпер (не запускать напрямую): `source dev/stand-env.sh`. NIM-25.
#
# Пустой DEV_STAND = DEFAULT-стенд: все значения байт-в-байт как исторически
# (slot 0, offset 0, /tmp/keeper-dev, порты 8080/8081/9090/9442/9443, БД keeper,
# kid keeper-dev-01, KV secret/keeper). Непустой DEV_STAND — второй+ стенд:
# свой dev-dir, порты (offset=slot*10), БД keeper_<slug>, KV secret/keeper/<slug>.
#
# Лёгкий режим (DEDICATED_INFRA=0, default): общие контейнеры soul-stack-{pg,vault,redis},
# разведение только бесплатное (своя БД / KV-префикс / порты). Redis — ОБЩИЙ, не разводим.
# DEDICATED_INFRA=1 — свой комплект контейнеров (переменные заложены; полный
# docker-compose проброс — TODO батча).

DEV_STAND="${DEV_STAND:-}"
DEDICATED_INFRA="${DEDICATED_INFRA:-0}"
# Файл-реестр авто-слотов (slug<TAB>slot). Read-modify-write под flock (гонка
# параллельных первых запусков). Освободить слот: `make dev-stand-free` или убрать строку из реестра.
STAND_REGISTRY="${STAND_REGISTRY:-/tmp/soul-stack-stands.tsv}"

# Валидация входов ДО использования: сырой DEV_STAND течёт в /tmp/keeper-dev-<slug>,
# PG_DB, vault-путь, KID, имя контейнера — traversal/injection-guard (NIM-25).
if [ -n "${DEV_STAND}" ] && ! printf '%s' "${DEV_STAND}" | grep -qE '^[a-z0-9][a-z0-9-]{0,30}$'; then
    printf 'stand-env: невалидный DEV_STAND=%s — допустимо ^[a-z0-9][a-z0-9-]{0,30}$\n' "${DEV_STAND}" >&2
    return 1 2>/dev/null || exit 1
fi
if [ -n "${DEV_STAND_SLOT:-}" ] && ! printf '%s' "${DEV_STAND_SLOT}" | grep -qE '^[123]$'; then
    printf 'stand-env: DEV_STAND_SLOT=%s вне диапазона 1..3\n' "${DEV_STAND_SLOT}" >&2
    return 1 2>/dev/null || exit 1
fi

# _stand_alloc_slot SLUG — вернуть слот 1..3 для слага: override DEV_STAND_SLOT →
# переиспользование из реестра → первый свободный. Нет свободных → код 1.
_stand_alloc_slot() {
    local slug="$1"
    if [ -n "${DEV_STAND_SLOT:-}" ]; then
        printf '%s' "${DEV_STAND_SLOT}"
        return 0
    fi
    # Read-modify-write реестра под flock: иначе параллельный первый запуск разных
    # слагов оба возьмут первый свободный слот → коллизия портов (NIM-25).
    (
        flock 9
        existing=""
        if [ -f "${STAND_REGISTRY}" ]; then
            existing="$(awk -F'\t' -v s="${slug}" '$1==s {print $2; exit}' "${STAND_REGISTRY}")"
        fi
        if [ -n "${existing}" ]; then printf '%s' "${existing}"; exit 0; fi
        free=""
        for slot in 1 2 3; do
            taken=""
            if [ -f "${STAND_REGISTRY}" ]; then
                taken="$(awk -F'\t' -v n="${slot}" '$2==n {print $1; exit}' "${STAND_REGISTRY}")"
            fi
            if [ -z "${taken}" ]; then free="${slot}"; break; fi
        done
        if [ -z "${free}" ]; then
            printf 'stand-env: нет свободных слотов 1..3 (реестр %s заполнен) — освободи слот (make dev-stand-free) или задай DEV_STAND_SLOT\n' "${STAND_REGISTRY}" >&2
            exit 1
        fi
        printf '%s\t%s\n' "${slug}" "${free}" >> "${STAND_REGISTRY}"
        printf '%s' "${free}"
    ) 9>"${STAND_REGISTRY}.lock"
}

# _stand_free_slot SLUG — убрать строку slug из реестра (идемпотентно, под flock),
# освобождая слот/порты следующему стенду. Обёртка — `make dev-stand-free`.
_stand_free_slot() {
    local slug="$1" tmp
    [ -n "${slug}" ] || return 0
    [ -f "${STAND_REGISTRY}" ] || return 0
    (
        flock 9
        tmp="$(mktemp "${STAND_REGISTRY}.XXXXXX")"
        awk -F'\t' -v s="${slug}" '$1!=s' "${STAND_REGISTRY}" > "${tmp}"
        mv "${tmp}" "${STAND_REGISTRY}"
    ) 9>"${STAND_REGISTRY}.lock"
}

if [ -z "${DEV_STAND}" ]; then
    STAND_SLUG=""
    STAND_SLOT=0
else
    STAND_SLUG="${DEV_STAND}"
    STAND_SLOT="$(_stand_alloc_slot "${STAND_SLUG}")"
fi

OFFSET=$(( STAND_SLOT * 10 ))

if [ -z "${STAND_SLUG}" ]; then
    STAND_DEV_DIR="/tmp/keeper-dev"
    KID="keeper-dev-01"
    PG_DB="keeper"
    VAULT_KV_PREFIX="secret/keeper"
    STACK_PREFIX="soul-stack"
else
    STAND_DEV_DIR="/tmp/keeper-dev-${STAND_SLUG}"
    KID="keeper-dev-${STAND_SLUG}"
    PG_DB="keeper_${STAND_SLUG}"
    VAULT_KV_PREFIX="secret/keeper/${STAND_SLUG}"
    # STACK_PREFIX slug-суффиксируется только в dedicated (свой комплект контейнеров);
    # в лёгком режиме контейнеры общие.
    if [ "${DEDICATED_INFRA}" = "1" ]; then STACK_PREFIX="soul-stack-${STAND_SLUG}"; else STACK_PREFIX="soul-stack"; fi
fi
ISSUER="${KID}"

OPENAPI_PORT=$(( 8080 + OFFSET ))
MCP_PORT=$(( 8081 + OFFSET ))
METRICS_PORT=$(( 9090 + OFFSET ))
BOOTSTRAP_PORT=$(( 9442 + OFFSET ))
ES_PORT=$(( 9443 + OFFSET ))
WEB_PORT=$(( 5173 + OFFSET ))
SOUL_METRICS_PORT=$(( 9191 + OFFSET ))

PG_DSN_REF="vault:${VAULT_KV_PREFIX}/postgres"
JWT_KEY_REF="vault:${VAULT_KV_PREFIX}/jwt-signing-key"
SIGIL_KEY_REF="vault:${VAULT_KV_PREFIX}/sigil-signing-key"

# INFRA_OFFSET: только dedicated разводит инфра-порты; лёгкий режим = общая инфра (0).
if [ "${DEDICATED_INFRA}" = "1" ]; then INFRA_OFFSET="${OFFSET}"; else INFRA_OFFSET=0; fi
PG_PORT=$(( 5434 + INFRA_OFFSET ))
VAULT_PORT=$(( 8200 + INFRA_OFFSET ))
REDIS_PORT=$(( 6381 + INFRA_OFFSET ))
OTEL_PORT=$(( 4317 + INFRA_OFFSET ))
JAEGER_PORT=$(( 16686 + INFRA_OFFSET ))

# Redis ОБЩИЙ в лёгком режиме; адрес меняется только при dedicated (INFRA_OFFSET).
REDIS_ADDR="127.0.0.1:${REDIS_PORT}"
VAULT_ADDR="http://127.0.0.1:${VAULT_PORT}"
OTEL_ENDPOINT="127.0.0.1:${OTEL_PORT}"

# Whitelist envsubst для рендера keeper.dev.yml.tmpl — ЕДИНЫЙ источник для
# keeper-run.sh, dev-smoke и check-stand-template (anti-drift). Новый ${VAR} в шаблоне → добавить сюда.
KEEPER_RENDER_WHITELIST='$KID $ISSUER $OPENAPI_PORT $MCP_PORT $METRICS_PORT $BOOTSTRAP_PORT $ES_PORT $STAND_DEV_DIR $PG_DSN_REF $JWT_KEY_REF $SIGIL_KEY_REF $REDIS_ADDR $OTEL_ENDPOINT $VAULT_ADDR'

# sid soul-стенда: default = исторический web-01.example.com; непустой = namespace
# слагом (изоляция реестра souls между стендами). NIM-25.
if [ -z "${STAND_SLUG}" ]; then SOUL_SID="web-01.example.com"; else SOUL_SID="web-01.${STAND_SLUG}.example.com"; fi

# Whitelist envsubst для рендера soul.dev.yml.tmpl — ЕДИНЫЙ источник для
# soul-run/dev-souls и check-soul-template (anti-drift). Новый ${VAR} в шаблоне → добавить сюда.
SOUL_RENDER_WHITELIST='$SOUL_SID $STAND_DEV_DIR $BOOTSTRAP_PORT $ES_PORT $SOUL_METRICS_PORT'

export DEV_STAND STAND_SLUG STAND_SLOT OFFSET STAND_DEV_DIR KID ISSUER \
    OPENAPI_PORT MCP_PORT METRICS_PORT BOOTSTRAP_PORT ES_PORT WEB_PORT SOUL_METRICS_PORT \
    PG_DB VAULT_KV_PREFIX DEDICATED_INFRA STACK_PREFIX INFRA_OFFSET \
    PG_PORT VAULT_PORT REDIS_PORT OTEL_PORT JAEGER_PORT \
    PG_DSN_REF JWT_KEY_REF SIGIL_KEY_REF REDIS_ADDR VAULT_ADDR OTEL_ENDPOINT \
    SOUL_SID SOUL_RENDER_WHITELIST

# stand_summary — печать текущего стенда (make-echo/логи).
stand_summary() {
    printf '[stand] slug=%s slot=%s offset=%s dir=%s dedicated=%s\n' "${STAND_SLUG:-<default>}" "${STAND_SLOT}" "${OFFSET}" "${STAND_DEV_DIR}" "${DEDICATED_INFRA}"
    printf '[stand] kid=%s pg_db=%s kv=%s stack=%s\n' "${KID}" "${PG_DB}" "${VAULT_KV_PREFIX}" "${STACK_PREFIX}"
    printf '[stand] ports: openapi=%s mcp=%s metrics=%s bootstrap=%s es=%s web=%s soul-metrics=%s\n' "${OPENAPI_PORT}" "${MCP_PORT}" "${METRICS_PORT}" "${BOOTSTRAP_PORT}" "${ES_PORT}" "${WEB_PORT}" "${SOUL_METRICS_PORT}"
}
