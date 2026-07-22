#!/usr/bin/env bash
#
# dev/souls-docker-down.sh - tear down docker-hosted souls and purge them from the registry (NIM-26).
#
# Inverse of dev/souls-docker-up.sh. Removes ONLY soul-docker-* containers
# (for a stand - soul-docker-<stand>-*; doesn't touch other docker objects), then removes
# their entries from the keeper registry and per-soul dev directories.
#
# The registry is cleaned directly via psql: there's no DELETE endpoint for /v1/souls/{sid} in
# the Operator API (verified - huma_soul_op.go, audit-guard). Cascade via FK:
# bootstrap_tokens/soul_seeds/incarnation_choir_voices.sid → souls(sid) ON DELETE
# CASCADE (migrations 008/009/060) - a single DELETE FROM souls cleans these 3 tables.
# apply_runs/state_history/audit_log store sid WITHOUT an FK -> they'll become orphaned (for dev this
# is an acceptable audit trail, not a bug).
#
# Parameters via env:
#   DEV_STAND - stand identifier (empty=default; prefix/DB/dev-dir - dev/stand-env.sh)
#   REPO_ROOT - soul-stack repo root (defaults from the script path)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Stand profile: STAND_DEV_DIR / STACK_PREFIX / PG_DB / STAND_SLUG (symmetric with up). NIM-25.
source "${SCRIPT_DIR}/stand-env.sh"

PREFIX="soul-docker${STAND_SLUG:+-${STAND_SLUG}}"

log()  { printf '[souls-docker-down] %s\n' "$*" >&2; }
fail() { printf '[souls-docker-down] [fail] %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "docker not found in PATH"

# 1. Tear down soul-docker-* containers (anchored filter - ours only).
ids="$(docker ps -aq --filter "name=^${PREFIX}-[0-9]+$" 2>/dev/null || true)"
if [ -n "${ids}" ]; then
    log "tearing down containers:"
    docker ps -a --filter "name=^${PREFIX}-[0-9]+$" --format '  {{.Names}} ({{.Status}})' >&2 || true
    # shellcheck disable=SC2086
    docker rm -f ${ids} >/dev/null 2>&1 || log "[warn] some containers failed to be removed"
else
    log "containers ${PREFIX}-* not found"
fi

# 2. Clean up the keeper registry (psql; cascade via FK). Infra is docker-compose.
if docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -c \
    "DELETE FROM souls WHERE sid ~ '^${PREFIX}-[0-9]+$'" >&2 2>/dev/null; then
    log "registry cleared (souls + cascade bootstrap_tokens/soul_seeds/incarnation_choir_voices)"
else
    log "[warn] failed to clean up the registry (is ${STACK_PREFIX}-postgres up? 'make dev-up')"
fi

# 3. Clean up per-soul dev directories.
shopt -s nullglob
dirs=("${STAND_DEV_DIR}/${PREFIX}"-*)
if [ "${#dirs[@]}" -gt 0 ]; then
    rm -rf "${dirs[@]}"
    log "removed directories ${STAND_DEV_DIR}/${PREFIX}-*"
fi
shopt -u nullglob

log "done"
