#!/usr/bin/env bash
#
# dev/stand-env.sh - single source of derived variables for the dev stand from DEV_STAND.
# SOURCED helper (do not run directly): `source dev/stand-env.sh`. NIM-25.
#
# Empty DEV_STAND = DEFAULT stand: all values byte-for-byte as before
# (slot 0, offset 0, /tmp/keeper-dev, ports 8080/8081/9090/9442/9443, DB keeper,
# kid keeper-dev-01, KV secret/keeper). Non-empty DEV_STAND - second+ stand:
# its own dev-dir, ports (offset=slot*10), DB keeper_<slug>, KV secret/keeper/<slug>.
#
# Light mode (DEDICATED_INFRA=0, default): shared containers soul-stack-{pg,vault,redis},
# separation is free-only (own DB / KV prefix / ports). Redis - SHARED, not separated.
# DEDICATED_INFRA=1 - its own set of containers (variables laid in; full
# docker-compose wiring - TODO for a later batch).

DEV_STAND="${DEV_STAND:-}"
DEDICATED_INFRA="${DEDICATED_INFRA:-0}"
# Registry file for auto-slots (slug<TAB>slot). Read-modify-write under flock (race of
# parallel first launches). Free a slot: `make dev-stand-free` or remove the line from the registry.
STAND_REGISTRY="${STAND_REGISTRY:-/tmp/soul-stack-stands.tsv}"

# Input validation BEFORE use: raw DEV_STAND flows into /tmp/keeper-dev-<slug>,
# PG_DB, vault path, KID, container name - traversal/injection guard (NIM-25).
if [ -n "${DEV_STAND}" ] && ! printf '%s' "${DEV_STAND}" | grep -qE '^[a-z0-9][a-z0-9-]{0,30}$'; then
    printf 'stand-env: invalid DEV_STAND=%s - allowed ^[a-z0-9][a-z0-9-]{0,30}$\n' "${DEV_STAND}" >&2
    return 1 2>/dev/null || exit 1
fi
if [ -n "${DEV_STAND_SLOT:-}" ] && ! printf '%s' "${DEV_STAND_SLOT}" | grep -qE '^[123]$'; then
    printf 'stand-env: DEV_STAND_SLOT=%s out of range 1..3\n' "${DEV_STAND_SLOT}" >&2
    return 1 2>/dev/null || exit 1
fi

# _stand_alloc_slot SLUG - return slot 1..3 for the slug: override DEV_STAND_SLOT ->
# reuse from the registry -> first free one. No free slots -> exit code 1.
_stand_alloc_slot() {
    local slug="$1"
    if [ -n "${DEV_STAND_SLOT:-}" ]; then
        printf '%s' "${DEV_STAND_SLOT}"
        return 0
    fi
    # Read-modify-write of the registry under flock: otherwise a parallel first launch of different
    # slugs would both grab the first free slot -> port collision (NIM-25).
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
            printf 'stand-env: no free slots 1..3 (registry %s is full) - free a slot (make dev-stand-free) or set DEV_STAND_SLOT\n' "${STAND_REGISTRY}" >&2
            exit 1
        fi
        printf '%s\t%s\n' "${slug}" "${free}" >> "${STAND_REGISTRY}"
        printf '%s' "${free}"
    ) 9>"${STAND_REGISTRY}.lock"
}

# _stand_free_slot SLUG - remove the slug's line from the registry (idempotent, under flock),
# freeing the slot/ports for the next stand. Wrapper - `make dev-stand-free`.
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
    # STACK_PREFIX is slug-suffixed only in dedicated mode (its own set of containers);
    # in light mode containers are shared.
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

# INFRA_OFFSET: only dedicated separates infra ports; light mode = shared infra (0).
if [ "${DEDICATED_INFRA}" = "1" ]; then INFRA_OFFSET="${OFFSET}"; else INFRA_OFFSET=0; fi
PG_PORT=$(( 5434 + INFRA_OFFSET ))
VAULT_PORT=$(( 8200 + INFRA_OFFSET ))
REDIS_PORT=$(( 6381 + INFRA_OFFSET ))
OTEL_PORT=$(( 4317 + INFRA_OFFSET ))
JAEGER_PORT=$(( 16686 + INFRA_OFFSET ))

# Redis is SHARED in light mode; the address changes only under dedicated (INFRA_OFFSET).
REDIS_ADDR="127.0.0.1:${REDIS_PORT}"
VAULT_ADDR="http://127.0.0.1:${VAULT_PORT}"
OTEL_ENDPOINT="127.0.0.1:${OTEL_PORT}"

# Whitelist envsubst for rendering keeper.dev.yml.tmpl - the SINGLE source for
# keeper-run.sh, dev-smoke, and check-stand-template (anti-drift). New ${VAR} in the template -> add it here.
KEEPER_RENDER_WHITELIST='$KID $ISSUER $OPENAPI_PORT $MCP_PORT $METRICS_PORT $BOOTSTRAP_PORT $ES_PORT $STAND_DEV_DIR $PG_DSN_REF $JWT_KEY_REF $SIGIL_KEY_REF $REDIS_ADDR $OTEL_ENDPOINT $VAULT_ADDR'

# soul-stand sid: default = the historical web-01.example.com; non-empty = namespaced
# by slug (souls registry isolation between stands). NIM-25.
if [ -z "${STAND_SLUG}" ]; then SOUL_SID="web-01.example.com"; else SOUL_SID="web-01.${STAND_SLUG}.example.com"; fi

# Whitelist envsubst for rendering soul.dev.yml.tmpl - the SINGLE source for
# soul-run/dev-souls and check-soul-template (anti-drift). New ${VAR} in the template -> add it here.
SOUL_RENDER_WHITELIST='$SOUL_SID $STAND_DEV_DIR $BOOTSTRAP_PORT $ES_PORT $SOUL_METRICS_PORT'

export DEV_STAND STAND_SLUG STAND_SLOT OFFSET STAND_DEV_DIR KID ISSUER \
    OPENAPI_PORT MCP_PORT METRICS_PORT BOOTSTRAP_PORT ES_PORT WEB_PORT SOUL_METRICS_PORT \
    PG_DB VAULT_KV_PREFIX DEDICATED_INFRA STACK_PREFIX INFRA_OFFSET \
    PG_PORT VAULT_PORT REDIS_PORT OTEL_PORT JAEGER_PORT \
    PG_DSN_REF JWT_KEY_REF SIGIL_KEY_REF REDIS_ADDR VAULT_ADDR OTEL_ENDPOINT \
    SOUL_SID SOUL_RENDER_WHITELIST

# stand_summary - print the current stand (make-echo/logs).
stand_summary() {
    printf '[stand] slug=%s slot=%s offset=%s dir=%s dedicated=%s\n' "${STAND_SLUG:-<default>}" "${STAND_SLOT}" "${OFFSET}" "${STAND_DEV_DIR}" "${DEDICATED_INFRA}"
    printf '[stand] kid=%s pg_db=%s kv=%s stack=%s\n' "${KID}" "${PG_DB}" "${VAULT_KV_PREFIX}" "${STACK_PREFIX}"
    printf '[stand] ports: openapi=%s mcp=%s metrics=%s bootstrap=%s es=%s web=%s soul-metrics=%s\n' "${OPENAPI_PORT}" "${MCP_PORT}" "${METRICS_PORT}" "${BOOTSTRAP_PORT}" "${ES_PORT}" "${WEB_PORT}" "${SOUL_METRICS_PORT}"
}
