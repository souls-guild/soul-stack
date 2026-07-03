#!/usr/bin/env bash
#
# dev/souls-docker-up.sh — поднять локальный флот souls как docker-контейнеры (NIM-26).
#
# Зачем: day-2 сценарии и UI-тесты без облака требуют душ с изолированной ФС/
# пакетами/сервисами. Host-флот (dev/souls-up.sh) для этого не годится — все души
# делят ФС хоста. Здесь каждая душа = privileged Debian-12 systemd-контейнер с
# примонтированным свежим soul-бинарём; онбординг к keeper-процессу на хосте.
#
# Имена предсказуемые: soul-docker-1..N (sid == имя контейнера). Идемпотентно:
# существующий контейнер soul-docker-$i пересоздаётся (docker rm -f перед run).
#
# Образ переиспользует базовый Dockerfile e2e-live (tests/e2e-live/dockerfiles/),
# контекст самодостаточен. Свежий soul-бинарь bind-mount-ится ro (ребилд образа
# не нужен) — нужен `make build-linux` (soul/bin/soul-linux-amd64).
#
# WSL2: из Docker-Desktop-VM `host.docker.internal` резолвится в DD-VM-шлюз, где
# keeper НЕ слушает → bootstrap падает. На WSL2 запускать с KEEPER_HOST=host-IP:
#   KEEPER_HOST=$(hostname -I | awk '{print $1}') bash dev/souls-docker-up.sh 3
# и переиздать keeper-cert с этим IP в SAN (DEV_KEEPER_EXTRA_IP=<ip> make dev-provision).
#
# Параметры через env / позиционный:
#   $1 / SOULS_COUNT   — сколько душ поднять (default 3)
#   KEEPER_HOST        — host keeper-эндпоинта из контейнера (default host.docker.internal)
#   SOUL_DOCKER_IMAGE  — тег dev-образа (default soul-stack-dev-soul:debian12)
#   KEEPER_DEV_DIR     — корень dev-артефактов (default /tmp/keeper-dev)
#   REPO_ROOT          — корень репо soul-stack (по умолчанию из пути скрипта)
#   VAULT_TOKEN        — root-token dev-Vault для mint-jwt (default root)

set -euo pipefail

COUNT="${1:-${SOULS_COUNT:-3}}"
KEEPER_HOST="${KEEPER_HOST:-host.docker.internal}"
IMAGE="${SOUL_DOCKER_IMAGE:-soul-stack-dev-soul:debian12}"
KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

SOUL_BIN="${REPO_ROOT}/soul/bin/soul-linux-amd64"
DOCKERFILE_DIR="${REPO_ROOT}/tests/e2e-live/dockerfiles"
DOCKERFILE="${DOCKERFILE_DIR}/debian-12.Dockerfile"
MINT_JWT="${SCRIPT_DIR}/mint-jwt.sh"
VAULT_CA="${KEEPER_DEV_DIR}/tls/vault-ca.crt"
API_BASE="http://127.0.0.1:8080"
PREFIX="soul-docker"

log()  { printf '[souls-docker-up] %s\n' "$*" >&2; }
fail() { printf '[souls-docker-up] [fail] %s\n' "$*" >&2; exit 1; }

# 1. Preflight.
command -v docker >/dev/null 2>&1 || fail "docker не найден в PATH"
[ -x "${SOUL_BIN}" ] || fail "нет linux-бинаря soul (${SOUL_BIN}) — собери: make build-linux"
[ -s "${VAULT_CA}" ] || fail "нет Vault-CA (${VAULT_CA}) — запусти 'make dev-provision'"
[ -f "${DOCKERFILE}" ] || fail "нет базового Dockerfile (${DOCKERFILE})"

code="$(curl -s -o /dev/null -w '%{http_code}' "${API_BASE}/healthz" 2>/dev/null || true)"
[ "${code}" = "200" ] || fail "keeper не отвечает на ${API_BASE}/healthz (code=${code:-none}) — запусти 'make dev-keeper'"

case "${COUNT}" in ''|*[!0-9]*) fail "COUNT должен быть числом (получено: ${COUNT})" ;; esac
[ "${COUNT}" -ge 1 ] || fail "COUNT должен быть >= 1"

# WSL2 + дефолтный host.docker.internal — почти гарантированный bootstrap-фейл. NIM-26.
if [ "${KEEPER_HOST}" = "host.docker.internal" ] && grep -qi microsoft /proc/version 2>/dev/null; then
    host_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    log "[warn] WSL2 обнаружен, а KEEPER_HOST=host.docker.internal — из DD-VM keeper недостижим."
    log "[warn] перезапусти с KEEPER_HOST=${host_ip:-<host-IP>} и переиздай cert:"
    log "[warn]   DEV_KEEPER_EXTRA_IP=${host_ip:-<host-IP>} make dev-provision && make dev-keeper"
fi

# 2. Образ (идемпотентно: docker кеширует слои; подхватит правку Dockerfile).
log "собираю dev-образ ${IMAGE} (контекст ${DOCKERFILE_DIR})"
docker build -t "${IMAGE}" -f "${DOCKERFILE}" "${DOCKERFILE_DIR}" >&2 \
    || fail "docker build образа ${IMAGE} не удался"

# 3. Admin-JWT для Operator API.
log "минчу admin-JWT (issue-token/create)"
ADMIN_JWT="$(AID=archon-alice ROLES='["cluster-admin"]' TTL=3600 bash "${MINT_JWT}")" \
    || fail "не удалось выпустить admin-JWT (см. mint-jwt вывод выше)"

# write_soul_yml CFG SID — soul.yml для docker-души. Большой max_attempts +
# failback:true → душа переживает рестарт keeper (reconnect по retry). NIM-26.
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
      event_stream_port: 9443
      bootstrap_port: 9442
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

# json_field FIELD — вынуть строковое поле верхнего уровня из JSON на stdin.
json_field() {
    python3 -c '
import sys, json
try:
    print(json.load(sys.stdin).get("'"$1"'", "") or "")
except Exception:
    pass
' 2>/dev/null
}

# issue_bootstrap_token SID — зарегистрировать душу (transport=agent → токен сразу
# в ответе); при уже существующей — fallback на issue-token?force=true. NIM-26.
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

# wait_systemd_ready NAME — дождаться готовности systemd-PID-1 внутри контейнера
# (running или degraded — degraded нормально для slim-Debian без unit-ов). NIM-26.
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

# spawn_soul I — поднять одну docker-душу soul-docker-$I целиком.
spawn_soul() {
    local i="$1"
    local sid="${PREFIX}-${i}" name="${PREFIX}-${i}"
    local dir="${KEEPER_DEV_DIR}/${name}" cfg
    cfg="${dir}/soul.yml"
    mkdir -p "${dir}"
    log "soul ${sid}"

    local bt
    if ! bt="$(issue_bootstrap_token "${sid}")"; then
        log "  [warn] не получил bootstrap_token для ${sid} — пропуск"
        return 1
    fi

    write_soul_yml "${cfg}" "${sid}"

    # Идемпотентность: снести прежний контейнер этого имени.
    docker rm -f "${name}" >/dev/null 2>&1 || true

    # Флаги — паритет с e2e-live harness (privileged systemd-PID-1, cgroup хоста,
    # tmpfs /run). Bind: свежий бинарь + soul.yml + Vault-CA (все ro). NIM-26.
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
        log "  [warn] docker run ${name} не удался — пропуск"
        return 1
    fi

    if ! wait_systemd_ready "${name}"; then
        log "  [warn] systemd в ${name} не поднялся за 60s — пропуск"
        return 1
    fi

    # soul init — CSR Bootstrap-flow. Токен пробрасываем БЕЗ значения в argv
    # (docker берёт из своего окружения) → не виден в host ps/proc cmdline. NIM-26.
    if ! SOUL_BOOTSTRAP_TOKEN="${bt}" docker exec -e SOUL_BOOTSTRAP_TOKEN "${name}" \
        soul init --config /etc/soul/soul.yml --sid "${sid}" >/dev/null 2>&1; then
        log "  [warn] soul init не удался в ${name} (docker logs / journalctl) — пропуск"
        return 1
    fi

    # soul run — фоновый демон внутри контейнера.
    docker exec -d "${name}" \
        sh -c "nohup soul run --config /etc/soul/soul.yml >/var/log/soul.log 2>&1 </dev/null &"
    log "  ${sid} онбордён, soul run запущен"
}

# 4. Поднять флот.
failed=0
for i in $(seq 1 "${COUNT}"); do
    spawn_soul "${i}" || failed=$((failed + 1))
done

# 5. Дождаться connected (до 60s) и показать сводку.
log "жду connected для ${COUNT} душ (до 60s)"
connected=0
for _ in $(seq 1 60); do
    connected="$(docker exec soul-stack-postgres psql -U keeper -d keeper -tA -c \
        "SELECT count(*) FROM souls WHERE sid LIKE '${PREFIX}-%' AND status='connected'" 2>/dev/null || echo 0)"
    [ "${connected:-0}" -ge "${COUNT}" ] && break
    sleep 1
done

log "статусы docker-душ:"
docker exec soul-stack-postgres psql -U keeper -d keeper -c \
    "SELECT sid, status FROM souls WHERE sid LIKE '${PREFIX}-%' ORDER BY sid" >&2 \
    || log "[warn] не удалось прочитать статусы из БД"

log "итог: connected=${connected}/${COUNT}, ошибок онбординга=${failed}"
if [ "${connected:-0}" -lt "${COUNT}" ] || [ "${failed}" -gt 0 ]; then
    fail "не все docker-души в connected (см. сводку выше; логи: docker logs ${PREFIX}-1 / docker exec ${PREFIX}-1 cat /var/log/soul.log)"
fi
log "готово: ${COUNT} docker-душ в connected"
