#!/usr/bin/env bash
#
# dev/web-run.sh - vite dev server for the companion repo soul-stack-web.
#
# NIM-25: parameterized by stand profile (DEV_STAND). Empty DEV_STAND = default stand
# (port 5173, VITE_KEEPER_API on :8080, dir /tmp/keeper-dev). Non-empty DEV_STAND -
# a second+ stand alongside: its own web port (5173+offset), VITE_KEEPER_API on the
# stand's keeper (:8080+offset), pidfile/log in STAND_DEV_DIR; derived variables - dev/stand-env.sh.
#
# `--host` is required: without it vite listens only on [::1] and 127.0.0.1 refuses.
# `--port ${WEB_PORT}` is required: vite.config.ts strictPort:true (fails if the port is
# busy) keeps stands apart via an explicit port. We kill the old vite of THIS stand by PID
# (pidfile + web-port holder), NOT pkill-by-name: the web repo is shared across stands, pkill
# would hit a neighbor.
#
# Parameters via env:
#   DEV_STAND - stand identifier (empty=default), see dev/stand-env.sh
#   WEB_DIR   - web repo directory (default ../soul-stack-web, sibling)
#   REPO_ROOT - root of the soul-stack repo (default derived from the script path)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Stand profile: STAND_DEV_DIR / WEB_PORT / OPENAPI_PORT / STAND_SLUG.
source "${SCRIPT_DIR}/stand-env.sh"

WEB_DIR="${WEB_DIR:-${REPO_ROOT}/../soul-stack-web}"
WEB_LOG="${STAND_DEV_DIR}/web-dev.log"
WEB_URL="http://127.0.0.1:${WEB_PORT}"
PID_FILE="${STAND_DEV_DIR}/web.pid"

log()  { printf '[web-run] %s\n' "$*" >&2; }
fail() { printf '[web-run] [fail] %s\n' "$*" >&2; exit 1; }

log "stand: slug=${STAND_SLUG:-<default>} web=${WEB_PORT} keeper-api=${OPENAPI_PORT} dir=${STAND_DEV_DIR}"

[ -d "${WEB_DIR}" ] || fail "web repo not found (${WEB_DIR}) - set WEB_DIR=<path>"
command -v npm >/dev/null 2>&1 || fail "npm not found in PATH"

mkdir -p "${STAND_DEV_DIR}"

# Kill the old vite of THIS stand BY PID (not pkill-by-name - the web repo is shared,
# that would hit a neighboring stand): pidfile + fallback to the web-port holder
# (the npm PID may have died while the vite child still holds the port; the port is
# unique per stand - won't touch neighbors).
kill_old() {
    local pid="$1"
    [ -n "${pid}" ] || return 0
    kill -0 "${pid}" 2>/dev/null || return 0
    grep -qaE 'vite|node|npm' "/proc/${pid}/cmdline" 2>/dev/null || return 0
    log "killing old vite of the stand (pid=${pid})"
    kill -9 "${pid}" 2>/dev/null || true
}
if [ -f "${PID_FILE}" ]; then
    kill_old "$(cat "${PID_FILE}" 2>/dev/null || true)"
    rm -f "${PID_FILE}"
fi
if command -v lsof >/dev/null 2>&1; then
    for p in $(lsof -t -nP -iTCP:"${WEB_PORT}" -sTCP:LISTEN 2>/dev/null || true); do
        kill_old "${p}"
    done
fi
sleep 1

# Wait for the stand's web port to free up (strictPort:true - restart fails if the old
# vite still holds the port).
if command -v lsof >/dev/null 2>&1; then
    log "waiting for :${WEB_PORT} to free up (up to 20s)"
    for _ in $(seq 1 20); do
        lsof -nP -iTCP:"${WEB_PORT}" -sTCP:LISTEN >/dev/null 2>&1 || break
        sleep 1
    done
    if lsof -nP -iTCP:"${WEB_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
        fail ":${WEB_PORT} is still busy - someone is holding the port (lsof -iTCP:${WEB_PORT})"
    fi
fi

# The stand's frontend talks to the stand's keeper: vite.config.ts proxy-target reads
# process.env at dev server startup.
export VITE_KEEPER_API="http://127.0.0.1:${OPENAPI_PORT}"

# exec in a subshell -> subshell PID = npm PID (written to pidfile for killing on restart).
log "starting vite (cwd=${WEB_DIR}, port=${WEB_PORT}, keeper-api=${OPENAPI_PORT}, log=${WEB_LOG})"
( cd "${WEB_DIR}" && exec nohup npm run dev -- --host --port "${WEB_PORT}" ) > "${WEB_LOG}" 2>&1 &
WEB_PID=$!
printf '%s\n' "${WEB_PID}" > "${PID_FILE}"

# Wait for 200 on the stand's web port (up to 30s).
log "waiting for ${WEB_URL} (up to 30s)"
for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${WEB_URL}" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then
        log "web ready: ${WEB_URL} (pid=${WEB_PID}, stand=${STAND_SLUG:-default})"
        printf 'web %s pid=%s stand=%s\n' "${WEB_URL}" "${WEB_PID}" "${STAND_SLUG:-default}"
        exit 0
    fi
    if ! kill -0 "${WEB_PID}" 2>/dev/null; then
        log "vite process died on startup - tail of ${WEB_LOG}:"
        tail -n 30 "${WEB_LOG}" >&2 || true
        fail "web dev server did not come up"
    fi
    sleep 1
done

log "vite did not respond 200 within 30s - tail of ${WEB_LOG}:"
tail -n 30 "${WEB_LOG}" >&2 || true
fail "web dev server did not come up"
