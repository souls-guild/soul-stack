#!/usr/bin/env bash
#
# dev/keeper-run.sh - restart the local keeper with a FULL dev-env.
#
# NIM-25: parameterized by stand profile (DEV_STAND). Empty DEV_STAND = default stand
# (behavior as historically: /tmp/keeper-dev, ports 8080/8081/9090/9442/9443, kid
# keeper-dev-01). Non-empty DEV_STAND - second+ stand alongside (own dev-dir, port offset,
# DB keeper_<slug>, KV secret/keeper/<slug>); derived vars - dev/stand-env.sh.
#
# Idempotent: renders the stand config from the template, kills the old keeper of THIS stand
# by PID (pidfile + metrics-port holder; not pkill-by-name, to avoid hitting neighbors),
# waits for the port to free up, brings it back up, waits for healthz.
#
# Parameters via env:
#   DEV_STAND - stand identifier (empty=default), see dev/stand-env.sh
#   REPO_ROOT - soul-stack repo root (default from script path)
#   KEEPER_CONFIG_OVERLAY - optional YAML file deep-merged over the rendered config
#               (maps merge, everything else replaces); needs python3 + PyYAML.
#               The committed template stays the single source; a caller that needs
#               different keys (dev/bench/linear-dispatch) states only those.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Stand profile: STAND_DEV_DIR / KID / ports / KV prefix / STACK_PREFIX / REDIS_ADDR / ...
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

log "stand: slug=${STAND_SLUG:-<default>} slot=${STAND_SLOT} dir=${STAND_DEV_DIR} kid=${KID} openapi=${OPENAPI_PORT}"

# 1. keeper binary. ALWAYS rebuilt, through `make build-keeper` - never reused as-is.
#
# Reusing an existing binary is what made a stand unfalsifiable (NIM-342): the
# script used to build only when ${KEEPER_BIN} was MISSING, so `make dev-keeper`
# would relaunch an arbitrarily old build without a word. There are dozens of
# worktrees in this repo, each with its own keeper/bin/keeper, and any of them
# binds the same default stand.
#
# The build goes through the Makefile rather than a local `go build` so the version
# stamp has ONE definition ($(VERSION) = git describe --tags --always --dirty).
# Without -ldflags the binary reports `0.0.0-dev`, and a stand that cannot name its
# own commit cannot be held to anything.
log "building keeper (make build-keeper)"
make -C "${REPO_ROOT}" build-keeper >&2 || fail "keeper build failed"
[ -x "${KEEPER_BIN}" ] || fail "build reported success but ${KEEPER_BIN} is not executable"

# The version is read back FROM THE ARTIFACT, not recomputed here: this is the
# string the process will report, so it is what we later hold /healthz to.
BUILT_VERSION="$("${KEEPER_BIN}" version 2>/dev/null | awk '{print $2}')"
[ -n "${BUILT_VERSION}" ] || fail "cannot read the version out of ${KEEPER_BIN} ('keeper version')"
if [ "${BUILT_VERSION}" = "0.0.0-dev" ]; then
    log "[warn] binary carries no version stamp (0.0.0-dev) - git describe found nothing."
    log "[warn] this stand cannot be identified from the outside; expect no acceptance proof."
else
    log "keeper built: version=${BUILT_VERSION}"
fi

# 2. Per-stand directories: root + both cache dirs for file://-resolve (keeper does not
# create them itself - without them artifact resolution fails with 502).
mkdir -p "${STAND_DEV_DIR}" "${STAND_DEV_DIR}/services" "${STAND_DEV_DIR}/destiny-cache"

# 3. TLS material (issued by dev-provision into STAND_DEV_DIR/tls). We do NOT run provision
# ourselves - an expensive step with side effects, we leave it as an explicit operator step.
if [ ! -s "${KEEPER_CRT}" ]; then
    fail "no TLS material (${KEEPER_CRT}) - run 'DEV_STAND=${STAND_SLUG} make dev-provision' and retry"
fi

# 4. Render the stand config from the template. Whitelist of derived vars - from stand-env
# (KEEPER_RENDER_WHITELIST, single source with dev-smoke/check-stand-template; incl. $VAULT_ADDR).
command -v envsubst >/dev/null 2>&1 || fail "envsubst not found (gettext package) - needed to render the config"
envsubst "${KEEPER_RENDER_WHITELIST}" < "${KEEPER_TMPL}" > "${KEEPER_CONFIG}"
log "config rendered: ${KEEPER_CONFIG}"
if [ -n "${KEEPER_CONFIG_OVERLAY:-}" ]; then
    [ -s "${KEEPER_CONFIG_OVERLAY}" ] || fail "KEEPER_CONFIG_OVERLAY=${KEEPER_CONFIG_OVERLAY} is missing or empty"
    python3 - "${KEEPER_CONFIG}" "${KEEPER_CONFIG_OVERLAY}" <<'PY' || fail "overlay merge failed (python3 + PyYAML needed)"
import sys, yaml
def merge(base, over):
    if isinstance(base, dict) and isinstance(over, dict):
        for k, v in over.items():
            base[k] = merge(base.get(k), v)
        return base
    return over
cfg_path, over_path = sys.argv[1], sys.argv[2]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f)
with open(over_path) as f:
    over = yaml.safe_load(f) or {}
with open(cfg_path, "w") as f:
    yaml.safe_dump(merge(cfg, over), f, sort_keys=False)
PY
    log "overlay merged: ${KEEPER_CONFIG_OVERLAY}"
fi

# 4a. Teleport push is opt-in (NIM-266) and needs an identity file this repo cannot
# mint - `tctl auth sign` against a live Teleport. Without it keeper dies four steps
# later with "build bootstrap teleport dialer: identity file could not be decoded",
# which reads like a keeper defect rather than a missing credential. Say it here,
# where the fix is obvious.
if [ "${DEV_PUSH_TRANSPORT}" = "teleport" ] && [ ! -s "${DEV_TELEPORT_IDENTITY_FILE}" ]; then
    fail "DEV_PUSH_TRANSPORT=teleport but no identity file at ${DEV_TELEPORT_IDENTITY_FILE} -
       mint one with 'tctl auth sign --user=<node-access-user> --out=${DEV_TELEPORT_IDENTITY_FILE} --format=file',
       point DEV_TELEPORT_IDENTITY_FILE elsewhere, or drop DEV_PUSH_TRANSPORT for the default 'direct' stand"
fi

# 5. Kill the old keeper of THIS stand BY PID (not pkill-by-name): pidfile + fallback
# to the stand's metrics-port holder (port is unique per stand - won't hit neighbors).
kill_old() {
    local pid="$1"
    [ -n "${pid}" ] || return 0
    kill -0 "${pid}" 2>/dev/null || return 0
    grep -qa keeper "/proc/${pid}/cmdline" 2>/dev/null || return 0
    log "killing old keeper of the stand (pid=${pid})"
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

# 6. Wait for the stand's metrics-port to free up.
log "waiting for :${METRICS_PORT} to free up (up to 20s)"
for _ in $(seq 1 20); do
    lsof -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 1
done
if lsof -nP -iTCP:"${METRICS_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
    fail ":${METRICS_PORT} is still busy - someone is holding the port (lsof -iTCP:${METRICS_PORT})"
fi

# 7. Clean up leader-leases (conductor:leader/reaper:leader). ONLY default stand OR
# dedicated: in the lightweight non-default mode Redis is SHARED - DEL would hit neighboring stands (NIM-25).
if [ -z "${STAND_SLUG}" ] || [ "${DEDICATED_INFRA}" = "1" ]; then
    if docker exec "${STACK_PREFIX}-redis" redis-cli -p 6379 DEL conductor:leader reaper:leader >/dev/null 2>&1; then
        log "leader-leases cleared (conductor:leader, reaper:leader)"
    else
        log "redis unavailable (${STACK_PREFIX}-redis) - leases not cleared; bring up 'make dev-up'"
    fi
else
    log "lightweight non-default stand: NOT clearing leader-leases (shared Redis - avoid hitting neighbors)"
fi

# 7a. Drop the SID leases the killed process still holds. They carry this stand's KID,
# so the new process reads them as its own, answers every reconnect with AlreadyExists
# ("soul lease still held by self") and its Souls stay off for a whole lease TTL; a run
# started meanwhile dies at send_apply_failed. Compare-and-delete by owner, so a lease
# of any other KID - a neighbour on shared Redis, keeper-b - is never touched.
dropped="$(docker exec "${STACK_PREFIX}-redis" sh -c "redis-cli -p 6379 --scan --pattern 'soul:*:lock' | while read -r k; do
    redis-cli -p 6379 EVAL \"if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0\" 1 \"\$k\" '${KID}'
done" 2>/dev/null | grep -c '^1$' || true)"
log "orphaned SID leases of ${KID} dropped: ${dropped:-0}"

# 8. dev-env for file://-resolve of artifacts + explicit dev VAULT_TOKEN=root (user's env
# may have a prod token; force root). VAULT_ADDR - from stand-env (lightweight=shared :8200).
export VAULT_ADDR
export VAULT_TOKEN=root
export KEEPER_SERVICE_CACHE_DIR="${STAND_DEV_DIR}/services"
export KEEPER_DESTINY_CACHE_DIR="${STAND_DEV_DIR}/destiny-cache"
export SOUL_STACK_ALLOW_FILE_REPOS=1
# Fresh stand DB -> empty operators registry; without the flag keeper run refuses
# to start (ADR-013). KEEPER_INITIALIZE=true = bootstrap-pending (healthz alive,
# waiting for the first Archon). Default is left untouched (init is done by dev-smoke).
if [ -n "${STAND_SLUG}" ]; then export KEEPER_INITIALIZE=true; fi

# 9. Bring up keeper in the background, write the stand's PID file.
log "starting keeper (config=${KEEPER_CONFIG}, log=${KEEPER_LOG})"
nohup "${KEEPER_BIN}" run --config="${KEEPER_CONFIG}" > "${KEEPER_LOG}" 2>&1 &
KEEPER_PID=$!
printf '%s\n' "${KEEPER_PID}" > "${PID_FILE}"

# 10. Wait for the stand's healthz 200 (up to 30s), then PROVE the answer comes from
# the binary we just built.
#
# A 200 proves only that *something* serves the port. `/healthz` reports the build
# version of the instance actually answering (ADR-0076(h)), so comparing it with the
# artifact's own version turns "keeper is up" into "THIS keeper is up". Without the
# comparison the loop happily reports success while our process died on
# `bind: address already in use` and a foreign keeper answers the probe - which is
# how a stand ends up serving code nobody can name (NIM-342).
log "waiting for healthz 200 (${HEALTHZ_URL}, up to 30s)"
for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${HEALTHZ_URL}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then
        body="$(curl -s -m 5 "${HEALTHZ_URL}" 2>/dev/null || true)"
        served="$(printf '%s' "${body}" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
        [ -n "${served}" ] || fail "healthz answered 200 without a version field - the stand cannot be verified: ${body}"
        if [ "${served}" != "${BUILT_VERSION}" ]; then
            fail "healthz on :${OPENAPI_PORT} reports version=${served}, but we built ${BUILT_VERSION} -
       a FOREIGN keeper holds this port. Kill it (lsof -nP -iTCP:${OPENAPI_PORT} -sTCP:LISTEN) or
       bring up a separate stand (DEV_STAND=<slug> make dev-keeper)."
        fi
        log "keeper ready: pid=${KEEPER_PID}, healthz 200, version=${served} (:${OPENAPI_PORT})"
        printf 'keeper pid=%s healthz=200 version=%s openapi=%s stand=%s\n' "${KEEPER_PID}" "${served}" "${OPENAPI_PORT}" "${STAND_SLUG:-default}"
        exit 0
    fi
    if ! kill -0 "${KEEPER_PID}" 2>/dev/null; then
        log "keeper process died on startup - tail of ${KEEPER_LOG}:"
        tail -n 30 "${KEEPER_LOG}" >&2 || true
        fail "keeper failed to start"
    fi
    sleep 1
done

log "healthz did not come up within 30s - tail of ${KEEPER_LOG}:"
tail -n 30 "${KEEPER_LOG}" >&2 || true
fail "keeper is not responding to healthz"
