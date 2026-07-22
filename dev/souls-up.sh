#!/usr/bin/env bash
#
# dev/souls-up.sh - bring the local souls fleet back up (all localhost processes).
#
# Why: on keeper restart / day rollover (/tmp cleanup) souls fall into
# disconnected. The script brings them back up from the stand DB registry: for
# each registered sid it writes soul.yml (if missing), onboards (if no seed),
# and (re)starts `soul run`. Covens are preserved in the DB - we do NOT
# re-register them.
#
# NIM-25: parameterized by stand profile (DEV_STAND). Empty DEV_STAND = default
# (as historically: keeper DB, keeper openapi :8080 / ES :9443 / bootstrap :9442,
# directories /tmp/keeper-dev, SID as in the registry). A non-empty DEV_STAND is
# a second-or-later stand alongside it: its own DB keeper_<slug>, its own keeper
# ports (offset), directories /tmp/keeper-dev-<slug>; derived variables live in
# dev/stand-env.sh.
#
# LIGHT MODE (DEDICATED_INFRA=0, default): Redis is SHARED across all stands,
# presence/SID-lease live in global keys soul:<sid>:hb / soul:<sid>:lock - so
# only one souls fleet at a time per stand; SIDs colliding with a neighboring
# stand are NOT supported (they would collide in the shared Redis) - register
# the stand's souls with a namespace in the SID (e.g. web-01.<slug>.example.com).
# HA/failover demos (several fleets on overlapping SIDs) require
# DEDICATED_INFRA=1 (own Redis).
#
# Idempotent: re-running kills the old `soul run` of each sid BY PID
# (per-sid pidfile) and brings it back up; an already-valid seed is not
# re-onboarded.
#
# Parameters via env:
#   DEV_STAND - stand identifier (empty=default), see dev/stand-env.sh
#   REPO_ROOT - soul-stack repo root (default derived from the script path)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Stand profile: STAND_DEV_DIR / STAND_SLUG / OPENAPI_PORT / ES_PORT / BOOTSTRAP_PORT /
# PG_DB / STACK_PREFIX / DEDICATED_INFRA / VAULT_ADDR / ...
source "${SCRIPT_DIR}/stand-env.sh"

SOUL_BIN="${REPO_ROOT}/soul/bin/soul"
MINT_JWT="${SCRIPT_DIR}/mint-jwt.sh"
VAULT_CA="${STAND_DEV_DIR}/tls/vault-ca.crt"
API_BASE="http://127.0.0.1:${OPENAPI_PORT}"

log()  { printf '[souls-up] %s\n' "$*" >&2; }
fail() { printf '[souls-up] [fail] %s\n' "$*" >&2; exit 1; }

log "stand: slug=${STAND_SLUG:-<default>} dir=${STAND_DEV_DIR} db=${PG_DB} api=${API_BASE}"

# sid_safe_for_stand SID - 0 if the SID will not collide in the SHARED Redis
# (soul:<sid>:hb|lock) with a neighboring stand: default stand (no slug),
# dedicated (own Redis), or the SID already carries the stand namespace. The
# namespace is derived LOCALLY from STAND_SLUG (not from stand-env). F7.
sid_safe_for_stand() {
    [ -n "${STAND_SLUG}" ] || return 0
    [ "${DEDICATED_INFRA}" = "1" ] && return 0
    case "$1" in *".${STAND_SLUG}."*) return 0 ;; esac
    return 1
}

# 1. soul binary.
if [ ! -x "${SOUL_BIN}" ]; then
    log "soul binary not found (${SOUL_BIN}) - building (go build ./cmd/soul)"
    (cd "${REPO_ROOT}/soul" && go build -o bin/soul ./cmd/soul) \
        || fail "soul build failed - build it manually: make build"
fi

[ -s "${VAULT_CA}" ] || fail "no Vault CA (${VAULT_CA}) - run 'DEV_STAND=${STAND_SLUG} make dev-provision'"

# 2. List of registered sids from the stand DB (${PG_DB}).
SIDS="$(docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -tA -c 'SELECT sid FROM souls' 2>/dev/null)" \
    || fail "failed to read souls from the DB (is ${STACK_PREFIX}-postgres/${PG_DB} up? 'make dev-up' + 'DEV_STAND=${STAND_SLUG} make dev-provision')"

if [ -z "${SIDS}" ]; then
    log "souls registry (${PG_DB}) is empty - nothing to bring up (register souls via API/UI)"
    exit 0
fi

# 3. Admin JWT for issue-token calls (mint-jwt is itself parameterized by the stand - iss/KV/vault).
log "minting admin JWT for issue-token"
ADMIN_JWT="$(AID=archon-alice ROLES='["cluster-admin"]' TTL=3600 bash "${MINT_JWT}")" \
    || fail "failed to issue admin JWT (see mint-jwt output above)"

# write_soul_yml DIR SID - write a default soul.yml for sid if it doesn't exist.
# Key schema is shared/config/soul.go (SoulConfig). priority:1 = the stand's
# single dev-keeper (ES_PORT/BOOTSTRAP_PORT). Per-sid modules/seed dirs live inside DIR.
write_soul_yml() {
    local dir="$1" sid="$2" cfg="$1/soul.yml"
    if [ -f "${cfg}" ]; then
        return 0
    fi
    log "  writing soul.yml for ${sid}"
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

# onboard_if_needed DIR SID CFG - issue a bootstrap token and run `soul init`
# if there is no valid active seed. A seed is valid only when the symlink
# <DIR>/seed/current actually points to the three required files cert/key/ca
# (layout from soul/internal/seed). The mere survival of the current symlink
# is not enough: after a /tmp cleanup the symlink can outlive the deletion of
# its target version (vN/ is empty) - then `soul run` fails with "SoulSeed
# not found". sigil_pubkey.pem is optional - we don't require it. soul init
# on top of a broken seed is safe: bootstrap.Run does not pre-check for an
# existing seed, seed.Write always writes a new version vN+1 and atomically
# repoints current (a broken empty vN/ doesn't get in the way). Before init,
# just in case, we remove the dangling current symlink if cert.pem is
# actually missing.
onboard_if_needed() {
    local dir="$1" sid="$2" cfg="$3"
    if [ -f "${dir}/seed/current/cert.pem" ] \
        && [ -f "${dir}/seed/current/key.pem" ] \
        && [ -f "${dir}/seed/current/ca.pem" ]; then
        log "  seed valid - skipping init (${sid})"
        return 0
    fi
    # Broken/gutted seed: remove the dangling current symlink (target vN/ is
    # empty or deleted) before init, so we don't leave a hanging link to nothing.
    if [ -L "${dir}/seed/current" ] && [ ! -f "${dir}/seed/current/cert.pem" ]; then
        log "  broken seed (current points at an empty version) - clearing symlink (${sid})"
        rm -f "${dir}/seed/current"
    fi
    log "  onboarding ${sid} (issue-token force=true -> soul init)"
    local resp bt
    resp="$(curl -s -X POST \
        -H "Authorization: Bearer ${ADMIN_JWT}" \
        "${API_BASE}/v1/souls/${sid}/issue-token?force=true")" \
        || { log "  [warn] issue-token HTTP call failed for ${sid} - skipping"; return 1; }
    bt="$(printf '%s' "${resp}" | python3 -c '
import sys, json
try:
    print(json.load(sys.stdin).get("bootstrap_token", ""))
except Exception:
    pass
' 2>/dev/null)"
    if [ -z "${bt}" ]; then
        log "  [warn] did not receive bootstrap_token for ${sid} (response: ${resp}) - skipping"
        return 1
    fi
    if ! "${SOUL_BIN}" init --config="${cfg}" --token="${bt}" >>"${dir}/soul.log" 2>&1; then
        log "  [warn] soul init failed for ${sid} (tail of ${dir}/soul.log)"
        return 1
    fi
    log "  ${sid} onboarded"
}

# _kill_pid_if_soul PID CFG - kill -9 strictly by PID, only if it's alive and
# its cmdline contains the cfg path (guard against PID reuse).
_kill_pid_if_soul() {
    local p="$1" cfg="$2"
    [ -n "${p}" ] || return 0
    kill -0 "${p}" 2>/dev/null || return 0
    grep -qa -- "${cfg}" "/proc/${p}/cmdline" 2>/dev/null || return 0
    log "  killing old soul (pid=${p})"
    kill -9 "${p}" 2>/dev/null || true
}

# kill_old_soul PIDFILE CFG - kill the sid's previous `soul run` BY the PID
# from the pidfile (not pkill on a shared pattern - that would hit souls of
# neighboring stands on the shared Redis). Fallback (migrating from an old
# run without a pidfile) - PID by the EXACT cfg path (per-sid, not a shared
# pattern), killed the same way by PID.
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

# Loop over sid: F7-guard, directory, soul.yml, modules, onboarding, (re)start.
while IFS= read -r sid; do
    [ -n "${sid}" ] || continue

    # F7: light non-default stand - a SID without the stand namespace would
    # collide in the SHARED Redis (soul:<sid>:hb|lock) with a neighbor on the
    # same SID. We don't bring up such a SID (guarantees no collision); fixed
    # by registering the soul with a namespaced SID.
    if ! sid_safe_for_stand "${sid}"; then
        log "  [warn] ${sid} has no namespace '${STAND_SLUG}' - risk of collision on the shared Redis; soul run SKIPPED (re-register the sid with '.${STAND_SLUG}.' or set DEDICATED_INFRA=1)"
        continue
    fi

    dir="${STAND_DEV_DIR}/${sid}"
    cfg="${dir}/soul.yml"
    pidfile="${dir}/soul.pid"
    mkdir -p "${dir}/modules"
    log "soul ${sid} (${dir})"

    write_soul_yml "${dir}" "${sid}"
    onboard_if_needed "${dir}" "${sid}" "${cfg}" || true

    # No valid seed after onboarding - run would fail anyway; skip the
    # start. Same criterion as in onboard_if_needed: three files under current.
    if ! { [ -f "${dir}/seed/current/cert.pem" ] \
        && [ -f "${dir}/seed/current/key.pem" ] \
        && [ -f "${dir}/seed/current/ca.pem" ]; }; then
        log "  no valid seed - soul run skipped (${sid})"
        continue
    fi

    # Kill THIS sid's previous `soul run` BY PID (per-sid pidfile).
    kill_old_soul "${pidfile}" "${cfg}"
    nohup "${SOUL_BIN}" run --config="${cfg}" >> "${dir}/soul.log" 2>&1 </dev/null &
    printf '%s\n' "$!" > "${pidfile}"
    log "  soul run started (pid=$!, pidfile=${pidfile})"
done <<EOF
${SIDS}
EOF

# Give the souls time to connect and show a status summary.
sleep 5
log "soul statuses (${PG_DB}):"
docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -c \
    'SELECT status, count(*) FROM souls GROUP BY status ORDER BY status' >&2 \
    || log "[warn] failed to read statuses from the DB"
