#!/usr/bin/env bash
#
# dev/bench/linear-dispatch/bench.sh - a scenario run under the staged (ADR-056) and the
# linear (ADR-0091) execution model, on one commit, on a fleet of real Souls
# (NIM-950). What is measured and how each number is derived: README.md beside this file.
#
# Runs on a DEDICATED stand that is already up (see README.md "Bring-up"):
#   DEV_STAND=<slug> DEDICATED_INFRA=1 dev/bench/linear-dispatch/bench.sh [setup|run|report|all]
#
# Parameters (env):
#   BENCH_FLEETS      agent fleet sizes, one incarnation each        (default "1 4 8")
#   BENCH_DELAYS_MS   netem delay added to every bench host's egress (default "0 20")
#   BENCH_MODELS      keeper.yml scenario.execution_model values      (default "staged linear")
#   BENCH_ACOLYTES    keeper.yml acolytes: 0 = inline, >0 = queue     (default "0 2")
#   BENCH_REPS        measured runs per cell, after one warm-up      (default 10)
#   BENCH_PUSH        1 = also measure a push host (transport: ssh)  (default 1)
#   BENCH_OUT         results root     (default ${STAND_DEV_DIR}/bench-linear-dispatch)
#   KEEPER_HOST       the keeper address as seen from a container    (default: first host IP)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

log()  { printf '[bench] %s\n' "$*" >&2; }
fail() { printf '[bench] [fail] %s\n' "$*" >&2; exit 1; }

[ -n "${DEV_STAND:-}" ] || fail "set DEV_STAND=<slug>: the benchmark restarts its keeper and needs a stand of its own"
[ "${DEDICATED_INFRA:-0}" = "1" ] || fail "set DEDICATED_INFRA=1: the benchmark enables a Vault audit device and must not touch shared infra"
# This machine's shell exports a production Vault address and token; nothing below may see them.
unset VAULT_ADDR VAULT_TOKEN
# shellcheck source=../../stand-env.sh
source "${REPO_ROOT}/dev/stand-env.sh"

FLEETS="${BENCH_FLEETS:-1 4 8}"
DELAYS="${BENCH_DELAYS_MS:-0 20}"
MODELS="${BENCH_MODELS:-staged linear}"
ACOLYTES="${BENCH_ACOLYTES:-0 2}"
REPS="${BENCH_REPS:-10}"
PUSH="${BENCH_PUSH:-1}"
KEEPER_HOST="${KEEPER_HOST:-$(hostname -I | awk '{print $1}')}"

SERVICE_ID="bench-linear"
SCENARIO="bench"
SECRET_PATH="secret/bench-shared/token"
SOUL_PREFIX="soul-docker-${STAND_SLUG}"
SOUL_METRICS_PORT_IN=9191
PUSH_SID="127.0.0.1"
PUSH_CONTAINER="bench-push-${STAND_SLUG}"
PUSH_IMAGE="soul-stack-bench-push:debian12"
PUSH_SSH_PORT=$(( 2222 + OFFSET ))
PUSH_PROVIDER="static"
PUSH_DIR="${STAND_DEV_DIR}/bench-push"
SSH_STATIC_REPO="${STAND_DEV_DIR}/plugin-repos/ssh-static"
HOST_CA_REF="vault:${VAULT_KV_PREFIX}/bench-ssh-host-ca"
JWT_FILE="${STAND_DEV_DIR}/archon-alice.jwt"
API="http://127.0.0.1:${OPENAPI_PORT}"
KEEPER_METRICS="http://127.0.0.1:${METRICS_PORT}/metrics"
OVERLAY="${STAND_DEV_DIR}/bench-overlay.yml"
COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short=8 HEAD)"
OUT="${BENCH_OUT:-${STAND_DEV_DIR}/bench-linear-dispatch}/${COMMIT}"

max_fleet=0
for n in ${FLEETS}; do [ "${n}" -gt "${max_fleet}" ] && max_fleet="${n}"; done

psql_stand() { docker exec -i "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -v ON_ERROR_STOP=1 -tAq "$@"; }
vault_exec() { docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=root "${STACK_PREFIX}-vault" vault "$@"; }

# api METHOD PATH [JSON] - an Operator API call as the stand's first Archon; prints the
# body, fails on any status outside 2xx unless ALLOW is set to the status.
api() {
    local method="$1" path="$2" body="${3:-}" out code
    out="$(mktemp)"
    code="$(curl -sS -o "${out}" -w '%{http_code}' -X "${method}" \
        -H "Authorization: Bearer $(cat "${JWT_FILE}")" \
        -H "Content-Type: application/json" \
        ${body:+-d "${body}"} "${API}${path}")" || { rm -f "${out}"; fail "${method} ${path}: curl failed"; }
    if [ "${code#2}" = "${code}" ] && [ "${code}" != "${ALLOW:-}" ]; then
        cat "${out}" >&2; rm -f "${out}"
        fail "${method} ${path}: HTTP ${code}"
    fi
    cat "${out}"; rm -f "${out}"
}

json_get() { python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get(sys.argv[1], ""))' "$1"; }

soul_names() { local n="$1" i; for i in $(seq 1 "${n}"); do printf '%s-%s\n' "${SOUL_PREFIX}" "${i}"; done; }
bench_containers() { soul_names "${max_fleet}"; [ "${PUSH}" = "1" ] && printf '%s\n' "${PUSH_CONTAINER}"; return 0; }

# ---------------------------------------------------------------- setup

materialize_service() {
    local dest="${STAND_DEV_DIR}/repos/${SERVICE_ID}"
    rm -rf "${dest}"
    mkdir -p "${dest}"
    cp -R "${SCRIPT_DIR}/service/." "${dest}/"
    git -C "${dest}" init -q -b main
    git -C "${dest}" add -A
    # A throwaway stand artifact, never pushed - the same as dev/provision.sh::provision_git_repo.
    # Fixed identity and dates: an unchanged fixture keeps its SHA, so keeper's cached
    # checkout of `main` stays the one being measured.
    GIT_AUTHOR_NAME=bench GIT_AUTHOR_EMAIL=bench@localhost GIT_AUTHOR_DATE=2026-01-01T00:00:00Z \
    GIT_COMMITTER_NAME=bench GIT_COMMITTER_EMAIL=bench@localhost GIT_COMMITTER_DATE=2026-01-01T00:00:00Z \
        git -C "${dest}" -c commit.gpgsign=false commit -q -m "bench fixture (dev/bench/linear-dispatch)"
    ALLOW=409 api POST /v1/services "{\"id\":\"${SERVICE_ID}\",\"git\":\"file://${dest}\",\"ref\":\"main\"}" >/dev/null
    log "service ${SERVICE_ID} @ ${dest} ($(git -C "${dest}" rev-parse --short HEAD)) registered"
}

setup_vault() {
    vault_exec kv put "${SECRET_PATH}" value=bench >/dev/null
    if ! vault_exec audit list 2>/dev/null | grep -q '^bench/'; then
        vault_exec audit enable -path=bench file file_path=/tmp/bench-audit.log >/dev/null
    fi
    log "vault: ${SECRET_PATH} written, audit device bench/ -> /tmp/bench-audit.log"
}

ensure_souls() {
    local have=0 name
    for name in $(soul_names "${max_fleet}"); do
        if docker exec "${name}" curl -sf "127.0.0.1:${SOUL_METRICS_PORT_IN}/metrics" >/dev/null 2>&1; then
            have=$((have + 1))
        fi
    done
    if [ "${have}" -lt "${max_fleet}" ]; then
        log "bringing up ${max_fleet} docker souls with /metrics (have ${have})"
        KEEPER_HOST="${KEEPER_HOST}" SOUL_DOCKER_METRICS_PORT="${SOUL_METRICS_PORT_IN}" \
            bash "${REPO_ROOT}/dev/souls-docker-up.sh" "${max_fleet}" >&2
    fi
}

ensure_incarnation() {
    local inc="$1"; shift
    if [ -z "$(psql_stand -c "SELECT 1 FROM incarnation WHERE id = '${inc}'")" ]; then
        api POST /v1/incarnations "{\"id\":\"${inc}\",\"service\":\"${SERVICE_ID}\"}" >/dev/null
    fi
    local sids
    sids="$(printf '"%s",' "$@")"
    api POST "/v1/incarnations/${inc}/members" "{\"sids\":[${sids%,}]}" >/dev/null
    log "incarnation ${inc}: $*"
}

build_ssh_static() {
    local src tmp bin=soul-ssh-static mode
    src="$("${REPO_ROOT}/scripts/plugin-source.sh" dir ssh-static)"
    tmp="$(mktemp -d)"
    ( cd "${src}" && GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags "-buildid=" -o "${tmp}/${bin}" . ) \
        || fail "go build of ssh-static failed (${src})"
    ( cd "${REPO_ROOT}" && GOWORK= go run ./dev/stamp-artifact.go "${tmp}/${bin}" "${src}/schema.json" ) \
        || fail "stamping ssh-static failed"
    rm -rf "${SSH_STATIC_REPO}"
    mkdir -p "${SSH_STATIC_REPO}/dist"
    cp "${tmp}/${bin}" "${SSH_STATIC_REPO}/dist/${bin}"
    chmod 0755 "${SSH_STATIC_REPO}/dist/${bin}"
    cp "${src}/schema.json" "${SSH_STATIC_REPO}/dist/schema.json"
    chmod 0644 "${SSH_STATIC_REPO}/dist/schema.json"
    rm -rf "${tmp}"
    git -C "${SSH_STATIC_REPO}" init -q -b main
    git -C "${SSH_STATIC_REPO}" add -A
    mode="$(git -C "${SSH_STATIC_REPO}" ls-files -s "dist/${bin}" | cut -d' ' -f1)"
    [ "${mode}" = "100755" ] || fail "git recorded dist/${bin} as ${mode:-nothing}, not 100755 - the resolver would find no artifact"
    git -C "${SSH_STATIC_REPO}" -c commit.gpgsign=false commit -q -m "ssh-static snapshot (bench)"
    git -C "${SSH_STATIC_REPO}" -c tag.gpgsign=false tag -f v1.0.0 >/dev/null
    log "ssh-static @ ${SSH_STATIC_REPO} (tag v1.0.0)"
}

setup_push_keys() {
    mkdir -p "${PUSH_DIR}"
    [ -s "${PUSH_DIR}/host_ca" ] || ssh-keygen -q -t ed25519 -N '' -C bench-host-ca -f "${PUSH_DIR}/host_ca"
    [ -s "${PUSH_DIR}/client" ] || ssh-keygen -q -t ed25519 -N '' -C bench-client -f "${PUSH_DIR}/client"
    if [ ! -s "${PUSH_DIR}/bench_host_key" ]; then
        ssh-keygen -q -t ed25519 -N '' -C bench-push-host -f "${PUSH_DIR}/bench_host_key"
        # No -n: a host certificate without principals holds for any address dialed.
        ssh-keygen -q -s "${PUSH_DIR}/host_ca" -I bench-push -h -V +52w "${PUSH_DIR}/bench_host_key.pub"
    fi
    vault_exec kv put "${VAULT_KV_PREFIX}/bench-ssh-host-ca" public_key="$(cat "${PUSH_DIR}/host_ca.pub")" >/dev/null
}

start_push_host() {
    docker build -q -t "${PUSH_IMAGE}" -f "${SCRIPT_DIR}/push-host.Dockerfile" "${SCRIPT_DIR}" >/dev/null \
        || fail "docker build ${PUSH_IMAGE} failed (is soul-stack-dev-soul:debian12 built? dev/souls-docker-up.sh builds it)"
    docker rm -f "${PUSH_CONTAINER}" >/dev/null 2>&1 || true
    docker run -d --name "${PUSH_CONTAINER}" --hostname bench-push --privileged --cgroupns=host \
        --tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup \
        -p "127.0.0.1:${PUSH_SSH_PORT}:22" "${PUSH_IMAGE}" >/dev/null
    local f
    for f in bench_host_key bench_host_key.pub bench_host_key-cert.pub; do
        docker cp "${PUSH_DIR}/${f}" "${PUSH_CONTAINER}:/etc/ssh/${f}"
    done
    docker cp "${PUSH_DIR}/client.pub" "${PUSH_CONTAINER}:/root/.ssh/authorized_keys"
    docker exec "${PUSH_CONTAINER}" sh -c 'chown root:root /etc/ssh/bench_host_key* /root/.ssh/authorized_keys && chmod 600 /etc/ssh/bench_host_key /root/.ssh/authorized_keys'
    local i
    for i in $(seq 1 30); do
        docker exec "${PUSH_CONTAINER}" systemctl restart ssh >/dev/null 2>&1 && break
        sleep 1
    done
    docker exec "${PUSH_CONTAINER}" systemctl is-active ssh >/dev/null || fail "sshd did not start in ${PUSH_CONTAINER}"
    log "push host ${PUSH_CONTAINER} on 127.0.0.1:${PUSH_SSH_PORT}"
}

# write_overlay MODEL ACOLYTES [PUSH_MODE] - PUSH_MODE `catalog` declares the provider
# without enabling push: the push dispatcher spawns its provider at start and refuses to
# start without a Sigil grant, and the grant can only be issued to a running keeper.
write_overlay() {
    local model="$1" acol="$2" push_mode="${3:-full}"
    {
        printf 'acolytes: %s\n' "${acol}"
        printf 'scenario:\n  execution_model: %s\n' "${model}"
        if [ "${PUSH}" = "1" ]; then
            cat <<YAML
plugins:
  ssh_providers:
    - { name: ${PUSH_PROVIDER}, source: "file://${SSH_STATIC_REPO}", ref: "v1.0.0" }
YAML
        fi
        if [ "${PUSH}" = "1" ] && [ "${push_mode}" = "full" ]; then
            cat <<YAML
push:
  soul_binary_path: ${REPO_ROOT}/soul/bin/soul-linux-amd64
  host_ca_refs:
    - { ref: "${HOST_CA_REF}", name: bench }
YAML
        fi
    } > "${OVERLAY}"
}

start_keeper() {
    write_overlay "$1" "$2" "${3:-full}"
    KEEPER_CONFIG_OVERLAY="${OVERLAY}" bash "${REPO_ROOT}/dev/keeper-run.sh" >&2
}

# restart_keeper MODEL ACOLYTES [PUSH_MODE] - a fresh keeper process for each
# configuration, so every cell starts from the same state, then wait until every bench
# Soul streams to it again.
restart_keeper() {
    start_keeper "$@"
    # keeper_grpc_streams_active is not the signal: it read 8 while every Soul was being
    # turned away with AlreadyExists. A session the fresh process opened is.
    local i active=0
    for i in $(seq 1 120); do
        active="$( (grep -o 'msg="eventstream: session opened" sid=[^ ]*' "${STAND_DEV_DIR}/keeper.log" || true) | sort -u | wc -l)"
        [ "${active}" -ge "${max_fleet}" ] && return 0
        sleep 1
    done
    fail "only ${active} of ${max_fleet} Souls opened a session within 120s (${STAND_DEV_DIR}/keeper.log)"
}

setup_push() {
    build_ssh_static
    setup_push_keys
    start_push_host
    restart_keeper staged 0 catalog
    ALLOW=409 api POST /v1/plugins/sigils \
        "{\"alias\":\"${PUSH_PROVIDER}\",\"source\":\"file://${SSH_STATIC_REPO}\",\"ref\":\"v1.0.0\"}" >/dev/null
    ALLOW=409 api POST /v1/push-providers \
        "{\"id\":\"${PUSH_PROVIDER}\",\"params\":{\"key_path\":\"${PUSH_DIR}/client\"}}" >/dev/null
    if [ -z "$(psql_stand -c "SELECT 1 FROM souls WHERE sid = '${PUSH_SID}'")" ]; then
        api POST /v1/souls "{\"sid\":\"${PUSH_SID}\",\"transport\":\"ssh\",\"covens\":[\"dev\"],\"note\":\"bench push host\"}" >/dev/null
    fi
    api PUT "/v1/souls/${PUSH_SID}/ssh-target" \
        "{\"ssh_port\":${PUSH_SSH_PORT},\"ssh_user\":\"root\",\"soul_path\":\"/var/lib/soul-stack/bin/soul\",\"ssh_provider\":\"${PUSH_PROVIDER}\"}" >/dev/null
    if [ -z "$(psql_stand -c "SELECT 1 FROM incarnation WHERE id = 'bench-push'")" ]; then
        api POST /v1/incarnations "{\"id\":\"bench-push\",\"service\":\"${SERVICE_ID}\"}" >/dev/null
    fi
    # The members API binds only a `connected` host (incarnation.ScreenBindCandidates),
    # and a push host never streams, so it stays `pending` forever: written directly.
    psql_stand -c "INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ('bench-push', '${PUSH_SID}') ON CONFLICT DO NOTHING"
    log "incarnation bench-push: ${PUSH_SID} (membership row written directly)"
    # The sigil and the provider row take effect on the next start.
    restart_keeper staged 0
}

cmd_setup() {
    [ -s "${JWT_FILE}" ] || fail "no ${JWT_FILE} - run keeper init (README.md, Bring-up)"
    [ -x "${REPO_ROOT}/soul/bin/soul-linux-amd64" ] || fail "no soul/bin/soul-linux-amd64 - make build-linux"
    start_keeper staged 0 catalog
    materialize_service
    setup_vault
    ensure_souls
    local n
    for n in ${FLEETS}; do
        # shellcheck disable=SC2046
        ensure_incarnation "bench-h${n}" $(soul_names "${n}")
    done
    if [ "${PUSH}" = "1" ]; then setup_push; fi
    log "setup done"
}

# ---------------------------------------------------------------- run

set_delay() {
    local d="$1" c
    for c in $(bench_containers); do
        if [ "${d}" = "0" ]; then
            docker exec "${c}" tc qdisc del dev eth0 root >/dev/null 2>&1 || true
        else
            docker exec "${c}" tc qdisc replace dev eth0 root netem delay "${d}ms"
        fi
    done
}

# probe_rtt - median TCP connect time from the first Soul to keeper's EventStream port.
# Only the connect is timed; the TLS handshake then fails (no client certificate).
probe_rtt() {
    docker exec "${SOUL_PREFIX}-1" sh -c "for i in 1 2 3 4 5 6 7; do curl -sk -o /dev/null -m 3 -w '%{time_connect}\n' https://${KEEPER_HOST}:${ES_PORT}/ || true; done" \
        | sort -n | sed -n 4p
}

snapshot() {
    local dir="$1" phase="$2" target="$3"; shift 3
    (curl -s "${KEEPER_METRICS}" || true) | grep -E '^keeper_(render_duration_seconds|vault_read_duration_seconds|grpc_messages_total)' \
        > "${dir}/${phase}.keeper.prom" || true
    docker exec "${STACK_PREFIX}-vault" sh -c "grep '\"path\":\"secret/data/bench-shared/token\"' /tmp/bench-audit.log | grep '\"type\":\"request\"' | grep -c '\"operation\":\"read\"' || true" \
        > "${dir}/${phase}.vault" 2>/dev/null || true
    if [ "${target}" = "push" ]; then
        docker exec "${PUSH_CONTAINER}" sh -c "journalctl -u ssh --no-pager -q | grep -c 'Accepted publickey' || true" \
            > "${dir}/${phase}.sshd" 2>/dev/null || true
    else
        local c
        for c in "$@"; do
            docker exec "${c}" curl -s "127.0.0.1:${SOUL_METRICS_PORT_IN}/metrics" \
                | grep -E '^soul_apply_duration_seconds_(sum|count)' > "${dir}/${phase}.soul.${c}.prom" || true
        done
    fi
}

run_data_sql() {
    local id="$1"
    cat <<SQL
SELECT json_build_object(
  'apply_id', '${id}',
  'started_ms', (SELECT round((extract(epoch FROM created_at) * 1000)::numeric, 3) FROM audit_log
                 WHERE event_type = 'incarnation.scenario_started' AND payload->>'apply_id' = '${id}'
                 ORDER BY created_at LIMIT 1),
  'completed_ms', (SELECT round((extract(epoch FROM created_at) * 1000)::numeric, 3) FROM audit_log
                   WHERE event_type = 'incarnation.run_completed' AND correlation_id = '${id}' LIMIT 1),
  'completed', (SELECT payload FROM audit_log
                WHERE event_type = 'incarnation.run_completed' AND correlation_id = '${id}' LIMIT 1),
  'task_events', (SELECT count(*) FROM audit_log WHERE event_type = 'task.executed' AND correlation_id = '${id}'),
  'rows', (SELECT coalesce(json_agg(json_build_object(
              'sid', sid, 'passage', passage, 'status', status, 'attempt', attempt,
              'started_ms', round((extract(epoch FROM started_at) * 1000)::numeric, 3),
              'claim_ms', round((extract(epoch FROM claim_at) * 1000)::numeric, 3),
              'finished_ms', round((extract(epoch FROM finished_at) * 1000)::numeric, 3))
            ORDER BY passage, sid), '[]'::json)
           FROM apply_runs WHERE apply_id = '${id}')
);
SQL
}

# measure INC TARGET DIR SOULS... - one run: snapshots around it, then its rows.
measure() {
    local inc="$1" target="$2" dir="$3"; shift 3
    mkdir -p "${dir}"
    snapshot "${dir}" pre "${target}" "$@"
    local apply_id i done=""
    apply_id="$(api POST "/v1/incarnations/${inc}/scenarios/${SCENARIO}" '{}' | json_get apply_id)"
    [ -n "${apply_id}" ] || fail "run of ${inc} returned no apply_id"
    for i in $(seq 1 6000); do
        done="$(psql_stand -c "SELECT 1 FROM audit_log WHERE event_type = 'incarnation.run_completed' AND correlation_id = '${apply_id}' LIMIT 1")"
        [ -n "${done}" ] && break
        sleep 0.1
    done
    [ -n "${done}" ] || fail "run ${apply_id} of ${inc} did not complete within 6000 polls (over 10 min)"
    # The terminal event lands just before the run unlocks; let the lock go before the next run.
    for i in $(seq 1 100); do
        [ "$(psql_stand -c "SELECT status FROM incarnation WHERE id = '${inc}'")" != "applying" ] && break
        sleep 0.1
    done
    snapshot "${dir}" post "${target}" "$@"
    psql_stand -c "$(run_data_sql "${apply_id}")" > "${dir}/run.json"
    local status
    status="$(python3 -c 'import sys,json; print((json.load(open(sys.argv[1])).get("completed") or {}).get("status",""))' "${dir}/run.json")"
    if [ "${status}" != "success" ]; then
        log "[warn] ${inc} run ${apply_id} ended ${status:-without a status}; unlocking"
        ALLOW=409 api POST "/v1/incarnations/${inc}/unlock" '{"reason":"bench: run failed, next run"}' >/dev/null
    fi
}

cmd_run() {
    mkdir -p "${OUT}"
    : > "${OUT}/keepers.txt"
    local version wakes=False
    version="$(curl -s "${API}/healthz" | json_get version)"
    # The barrier wakes on the write from NIM-984 on; report.py reads `gap` differently.
    [ -d "${REPO_ROOT}/keeper/internal/applywake" ] && wakes=True
    python3 - "${OUT}/meta.json" <<PY
import json, sys
json.dump({
    "commit": "${COMMIT}", "fleets": "${FLEETS}", "delays_ms": "${DELAYS}", "models": "${MODELS}",
    "acolytes": "${ACOLYTES}", "reps": ${REPS}, "push": "${PUSH}", "cpus": $(nproc),
    "keeper_version_before_run": "${version}", "barrier_wakes": ${wakes},
}, open(sys.argv[1], "w"), indent=1, sort_keys=True)
PY
    local model acol delay n rep cell
    for model in ${MODELS}; do
        for acol in ${ACOLYTES}; do
            log "=== keeper: execution_model=${model} acolytes=${acol}"
            restart_keeper "${model}" "${acol}"
            # Each restart rebuilds Keeper from the working tree; name what actually served.
            printf '%s-a%s %s\n' "${model}" "${acol}" "$(curl -s "${API}/healthz" | json_get version)" >> "${OUT}/keepers.txt"
            for delay in ${DELAYS}; do
                set_delay "${delay}"
                probe_rtt > "${OUT}/rtt-d${delay}.txt"
                for n in ${FLEETS}; do
                    cell="${OUT}/${model}-a${acol}-d${delay}-h${n}"
                    for rep in w $(seq 1 "${REPS}"); do
                        # shellcheck disable=SC2046
                        measure "bench-h${n}" agent "${cell}/${rep}" $(soul_names "${n}")
                    done
                    log "${cell##*/}: done"
                done
                # The work-queue does not reach a push host: the one-Passage staged fixture is
                # refused before dispatch (run.go, dispatch_failed) and a linear step goes
                # inline (dispatch.go queuesStep), so a "queue" push cell would measure nothing
                # or the inline path.
                if [ "${PUSH}" = "1" ] && [ "${acol}" = "0" ]; then
                    cell="${OUT}/${model}-a${acol}-d${delay}-push"
                    for rep in w $(seq 1 "${REPS}"); do
                        measure bench-push push "${cell}/${rep}"
                    done
                    log "${cell##*/}: done"
                fi
            done
            set_delay 0
        done
    done
    log "raw results in ${OUT}"
}

cmd_report() {
    python3 "${SCRIPT_DIR}/report.py" "${OUT}" | tee "${OUT}/results.md"
}

case "${1:-all}" in
    setup)  cmd_setup ;;
    run)    cmd_run ;;
    report) cmd_report ;;
    all)    cmd_setup; cmd_run; cmd_report ;;
    *)      fail "unknown subcommand '$1' (setup|run|report|all)" ;;
esac
