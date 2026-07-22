#!/usr/bin/env bash
#
# dev/souls-docker-up.sh - bring up local souls as docker containers (NIM-26).
#
# Why: day-2 scenarios and UI tests without a cloud need souls with an isolated FS/
# packages/services. Host souls (dev/souls-up.sh) aren't suitable for this - all souls
# share the host FS. Here each soul is a privileged Debian-12 systemd container with
# a freshly mounted soul binary; onboards to the keeper process on the host.
#
# Names are predictable: soul-docker-1..N (per-stand - soul-docker-<stand>-1..N; sid ==
# container name). Idempotent: an existing container is recreated (docker rm -f before run).
#
# The image reuses the base e2e-live Dockerfile (tests/e2e-live/dockerfiles/),
# the context is self-contained. The fresh soul binary is bind-mounted ro (no image
# rebuild needed) - requires `make build-linux` (soul/bin/soul-linux-amd64).
#
# WSL2: from the Docker-Desktop VM, `host.docker.internal` resolves to the DD-VM
# gateway, where the keeper does NOT listen -> bootstrap fails. On WSL2 run with
# KEEPER_HOST=host-IP:
#   KEEPER_HOST=$(hostname -I | awk '{print $1}') bash dev/souls-docker-up.sh 3
# and reissue the keeper cert with this IP in the SAN (DEV_KEEPER_EXTRA_IP=<ip> make dev-provision).
#
# Parameters via env / positional:
#   $1 / SOULS_COUNT   - how many souls to bring up (default 3)
#   KEEPER_HOST        - keeper endpoint host as seen from the container (default host.docker.internal)
#   SOUL_DOCKER_IMAGE  - dev image tag (default soul-stack-dev-soul:debian12)
#   DEV_STAND          - stand identifier (empty=default; prefix/DB/ports/dev-dir - dev/stand-env.sh)
#   REPO_ROOT          - soul-stack repo root (defaults from the script path)
#   VAULT_TOKEN        - dev-Vault root token for mint-jwt (default root)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

# Stand profile: STAND_DEV_DIR / OPENAPI_PORT / BOOTSTRAP_PORT / ES_PORT / STACK_PREFIX / PG_DB / STAND_SLUG. NIM-25.
source "${SCRIPT_DIR}/stand-env.sh"

COUNT="${1:-${SOULS_COUNT:-3}}"
KEEPER_HOST="${KEEPER_HOST:-host.docker.internal}"
IMAGE="${SOUL_DOCKER_IMAGE:-soul-stack-dev-soul:debian12}"

SOUL_BIN="${REPO_ROOT}/soul/bin/soul-linux-amd64"
DOCKERFILE_DIR="${REPO_ROOT}/tests/e2e-live/dockerfiles"
DOCKERFILE="${DOCKERFILE_DIR}/debian-12.Dockerfile"
MINT_JWT="${SCRIPT_DIR}/mint-jwt.sh"
VAULT_CA="${STAND_DEV_DIR}/tls/vault-ca.crt"
API_BASE="http://127.0.0.1:${OPENAPI_PORT}"
PREFIX="soul-docker${STAND_SLUG:+-${STAND_SLUG}}"
STAND_ENV_HINT="${STAND_SLUG:+DEV_STAND=${STAND_SLUG} }"

log()  { printf '[souls-docker-up] %s\n' "$*" >&2; }
fail() { printf '[souls-docker-up] [fail] %s\n' "$*" >&2; exit 1; }

# 1. Preflight.
command -v docker >/dev/null 2>&1 || fail "docker not found in PATH"
[ -x "${SOUL_BIN}" ] || fail "no linux soul binary (${SOUL_BIN}) - build it: make build-linux"
[ -s "${VAULT_CA}" ] || fail "no Vault CA (${VAULT_CA}) - run '${STAND_ENV_HINT}make dev-provision'"
[ -f "${DOCKERFILE}" ] || fail "no base Dockerfile (${DOCKERFILE})"

code="$(curl -s -o /dev/null -w '%{http_code}' "${API_BASE}/healthz" 2>/dev/null || true)"
[ "${code}" = "200" ] || fail "keeper is not responding on ${API_BASE}/healthz (code=${code:-none}) - run '${STAND_ENV_HINT}make dev-keeper'"

case "${COUNT}" in ''|*[!0-9]*) fail "COUNT must be a number (got: ${COUNT})" ;; esac
[ "${COUNT}" -ge 1 ] || fail "COUNT must be >= 1"

# WSL2 + default host.docker.internal - an almost guaranteed bootstrap failure. NIM-26.
if [ "${KEEPER_HOST}" = "host.docker.internal" ] && grep -qi microsoft /proc/version 2>/dev/null; then
    host_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    log "[warn] WSL2 detected, and KEEPER_HOST=host.docker.internal - the keeper is unreachable from the DD-VM."
    log "[warn] restart with KEEPER_HOST=${host_ip:-<host-IP>} and reissue the cert:"
    log "[warn]   ${STAND_ENV_HINT}DEV_KEEPER_EXTRA_IP=${host_ip:-<host-IP>} make dev-provision && ${STAND_ENV_HINT}make dev-keeper"
fi

# 2. Image (idempotent: docker caches layers; picks up Dockerfile edits).
log "building dev image ${IMAGE} (context ${DOCKERFILE_DIR})"
docker build -t "${IMAGE}" -f "${DOCKERFILE}" "${DOCKERFILE_DIR}" >&2 \
    || fail "docker build of image ${IMAGE} failed"

# 3. Admin-JWT for the Operator API.
log "minting admin-JWT (issue-token/create)"
ADMIN_JWT="$(AID=archon-alice ROLES='["cluster-admin"]' TTL=3600 bash "${MINT_JWT}")" \
    || fail "failed to issue admin-JWT (see mint-jwt output above)"

# write_soul_yml CFG SID -- soul.yml for the docker soul. A large max_attempts +
# failback:true -> the soul survives a keeper restart (reconnect via retry). NIM-26.
write_soul_yml() {
    local cfg="$1" sid="$2"
    cat > "${cfg}" <<YAML
sid: ${sid}

paths:
  modules: /var/lib/soul-stack/modules
  seed:    /var/lib/soul-stack/seed

keeper:
  endpoints:
    - host: ${KEEPER_HOST}
      event_stream_port: ${ES_PORT}
      bootstrap_port: ${BOOTSTRAP_PORT}
      priority: 1
  retry:
    max_attempts: 30
    backoff: { initial: 1s, max: 10s, jitter: true }
    handshake_timeout: 10s
  failback:
    enabled: true
  tls:
    ca: /etc/soul/ca.pem

logging:
  level: info
  format: text
YAML
}

# json_field FIELD -- extract a top-level string field from JSON on stdin.
json_field() {
    python3 -c '
import sys, json
try:
    print(json.load(sys.stdin).get("'"$1"'", "") or "")
except Exception:
    pass
' 2>/dev/null
}

# issue_bootstrap_token SID -- register the soul (transport=agent -> token comes
# right in the response); if it already exists -- fallback to issue-token?force=true. NIM-26.
issue_bootstrap_token() {
    local sid="$1" resp bt
    resp="$(curl -s -X POST \
        -H "Authorization: Bearer ${ADMIN_JWT}" \
        -H "Content-Type: application/json" \
        -d "{\"sid\":\"${sid}\",\"transport\":\"agent\",\"covens\":[\"dev\"],\"note\":\"NIM-26 docker dev fleet\"}" \
        "${API_BASE}/v1/souls")"
    bt="$(printf '%s' "${resp}" | json_field bootstrap_token)"
    if [ -z "${bt}" ]; then
        resp="$(curl -s -X POST \
            -H "Authorization: Bearer ${ADMIN_JWT}" \
            "${API_BASE}/v1/souls/${sid}/issue-token?force=true")"
        bt="$(printf '%s' "${resp}" | json_field bootstrap_token)"
    fi
    [ -n "${bt}" ] || return 1
    printf '%s' "${bt}"
}

# wait_systemd_ready NAME -- wait for systemd-PID-1 readiness inside the container
# (running or degraded -- degraded is normal for slim-Debian without units). NIM-26.
wait_systemd_ready() {
    local name="$1" i st
    for i in $(seq 1 60); do
        st="$(docker exec "${name}" systemctl is-system-running 2>/dev/null || true)"
        case "${st}" in
            running|degraded) return 0 ;;
        esac
        sleep 1
    done
    return 1
}

# spawn_soul I -- bring up a single docker soul soul-docker-$I end to end.
spawn_soul() {
    local i="$1"
    local sid="${PREFIX}-${i}" name="${PREFIX}-${i}"
    local dir="${STAND_DEV_DIR}/${name}" cfg
    cfg="${dir}/soul.yml"
    mkdir -p "${dir}"
    log "soul ${sid}"

    local bt
    if ! bt="$(issue_bootstrap_token "${sid}")"; then
        log "  [warn] didn't get a bootstrap_token for ${sid} -- skipping"
        return 1
    fi

    write_soul_yml "${cfg}" "${sid}"

    # Idempotency: remove any previous container with this name.
    docker rm -f "${name}" >/dev/null 2>&1 || true

    # Flags -- parity with the e2e-live harness (privileged systemd-PID-1, host cgroup,
    # tmpfs /run). Bind: fresh binary + soul.yml + Vault-CA (all ro). NIM-26.
    if ! docker run -d \
        --name "${name}" \
        --hostname "${sid}" \
        --privileged \
        --cgroupns=host \
        --tmpfs /run \
        --tmpfs /run/lock \
        -v /sys/fs/cgroup:/sys/fs/cgroup \
        --add-host host.docker.internal:host-gateway \
        -v "${SOUL_BIN}:/usr/local/bin/soul:ro" \
        -v "${cfg}:/etc/soul/soul.yml:ro" \
        -v "${VAULT_CA}:/etc/soul/ca.pem:ro" \
        "${IMAGE}" >/dev/null; then
        log "  [warn] docker run ${name} failed -- skipping"
        return 1
    fi

    if ! wait_systemd_ready "${name}"; then
        log "  [warn] systemd in ${name} didn't come up within 60s -- skipping"
        return 1
    fi

    # soul init -- CSR bootstrap flow. We pass the token WITHOUT a value in argv
    # (docker takes it from its own environment) -> not visible in host ps/proc cmdline. NIM-26.
    if ! SOUL_BOOTSTRAP_TOKEN="${bt}" docker exec -e SOUL_BOOTSTRAP_TOKEN "${name}" \
        soul init --config /etc/soul/soul.yml --sid "${sid}" >/dev/null 2>&1; then
        log "  [warn] soul init failed in ${name} (docker logs / journalctl) -- skipping"
        return 1
    fi

    # soul run -- background daemon inside the container.
    docker exec -d "${name}" \
        sh -c "nohup soul run --config /etc/soul/soul.yml >/var/log/soul.log 2>&1 </dev/null &"
    log "  ${sid} onboarded, soul run started"
}

# 4. Bring up the souls.
failed=0
for i in $(seq 1 "${COUNT}"); do
    spawn_soul "${i}" || failed=$((failed + 1))
done

# 5. Wait for connected (up to 60s) and print a summary.
log "waiting for connected for ${COUNT} souls (up to 60s)"
connected=0
for _ in $(seq 1 60); do
    connected="$(docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -tA -c \
        "SELECT count(*) FROM souls WHERE sid ~ '^${PREFIX}-[0-9]+$' AND status='connected'" 2>/dev/null || echo 0)"
    [ "${connected:-0}" -ge "${COUNT}" ] && break
    sleep 1
done

log "docker soul statuses:"
docker exec "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -c \
    "SELECT sid, status FROM souls WHERE sid ~ '^${PREFIX}-[0-9]+$' ORDER BY sid" >&2 \
    || log "[warn] failed to read statuses from the DB"

log "summary: connected=${connected}/${COUNT}, onboarding errors=${failed}"
if [ "${connected:-0}" -lt "${COUNT}" ] || [ "${failed}" -gt 0 ]; then
    fail "not all docker souls are connected (see summary above; logs: docker logs ${PREFIX}-1 / docker exec ${PREFIX}-1 cat /var/log/soul.log)"
fi
log "done: ${COUNT} docker souls connected"
