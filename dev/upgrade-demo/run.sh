#!/usr/bin/env bash
#
# dev/upgrade-demo/run.sh — живой ручной тест фичи NIM-34 (upgrade v2, ADR-0068).
#
# Что делает: материализует демо-сервис `upgrade-demo` (git-репо с тремя тегами
# v1.0.0/v2.0.0/v2.0.1 из dev/upgrade-demo/tree/), сеет его в service_registry,
# bare-создаёт инкарнацию (без хостов) на пине v1.0.0 и прокручивает curl по
# upgrade-paths / upgrade, печатая ответы. Демонстрирует cheap / found / legacy /
# state-миграции + живой legacy-upgrade -> drift. Полное покрытие кейсов — README.md.
#
# Идемпотентность: сервис-репо и реестр пересоздаются/ON CONFLICT DO NOTHING;
# инкарнация — уникальное имя updemo-<rand> за прогон (в конце — подсказка сноса).
#
# Стенд ПЕРЕИСПОЛЬЗУЕТСЯ, если keeper уже на :8080 (healthz 200). Иначе скрипт
# пытается поднять цепочку (make dev-up -> dev-provision при отсутствии TLS ->
# dev-keeper). Чужие данные (redis/hello-world, чужие инкарнации) не трогаются.
#
# Запуск:  bash dev/upgrade-demo/run.sh
#
# Env (все с дефолтами):
#   KEEPER_DEV_DIR  — корень dev-артефактов (default /tmp/keeper-dev)
#   API             — Operator API base (default http://127.0.0.1:8080)
#   PG_DSN          — DSN dev-Postgres (default postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable)
#   VAULT_TOKEN     — форсится в root (dev-Vault); прод-токен из env игнорируется.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TREE_DIR="${SCRIPT_DIR}/tree"

KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
API="${API:-http://127.0.0.1:8080}"
PG_DSN="${PG_DSN:-postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable}"
# ГРАБЛЯ (MEMORY): в shell оператора часто унаследован ПРОД-VAULT_TOKEN — dev-Vault
# его отвергает. Форсим root явно на весь скрипт (mint-jwt читает VAULT_TOKEN).
export VAULT_TOKEN=root

SERVICE_NAME="upgrade-demo"
REPO_DIR="${KEEPER_DEV_DIR}/repos/${SERVICE_NAME}"
REPO_URL="file://${REPO_DIR}"
TAGS=(v1.0.0 v2.0.0 v2.0.1)
# Уникальное имя инкарнации за прогон (не трогаем чужое, короткий rand-суффикс).
INC_NAME="updemo-$(date +%s | tail -c 6)$(( RANDOM % 90 + 10 ))"

log()  { printf '\n[upgrade-demo] %s\n' "$*"; }
step() { printf '\n\033[1m========== %s ==========\033[0m\n' "$*"; }
fail() { printf '\n[upgrade-demo] [FAIL] %s\n' "$*" >&2; exit 1; }

# pretty — JSON через python3 (jq не гарантирован на dev-машине).
pretty() { python3 -m json.tool 2>/dev/null || cat; }

command -v git >/dev/null 2>&1 || fail "git не найден в PATH"
command -v curl >/dev/null 2>&1 || fail "curl не найден в PATH"
command -v python3 >/dev/null 2>&1 || fail "python3 не найден в PATH"
[ -d "${TREE_DIR}" ] || fail "нет каталога снапшотов: ${TREE_DIR}"

# ── psql-обёртка (как в dev/provision.sh): host psql или docker exec fallback ──
if command -v psql >/dev/null 2>&1 && psql "${PG_DSN}" -c 'SELECT 1' >/dev/null 2>&1; then
    psql_cli() { psql "${PG_DSN}" -v ON_ERROR_STOP=1 -tAq "$@"; }
else
    psql_cli() { docker exec -i soul-stack-postgres psql -U keeper -d keeper -v ON_ERROR_STOP=1 -tAq "$@"; }
fi

# ── 1. Boot-или-reuse keeper ──────────────────────────────────────────────────
step "1. Keeper: reuse или boot"
healthz_code() { curl -s -o /dev/null -w '%{http_code}' "${API}/healthz" 2>/dev/null || true; }
if [ "$(healthz_code)" = "200" ]; then
    log "keeper уже на ${API} (healthz 200) — ПЕРЕИСПОЛЬЗУЮ (без dev-provision/dev-keeper)"
else
    log "keeper не отвечает — поднимаю цепочку (make dev-up -> [dev-provision] -> dev-keeper)"
    make -C "${REPO_ROOT}" dev-up
    if [ ! -s "${KEEPER_DEV_DIR}/tls/keeper.crt" ]; then
        log "нет TLS-материала — запускаю dev-provision (единожды)"
        make -C "${REPO_ROOT}" dev-provision
    fi
    make -C "${REPO_ROOT}" dev-keeper
    [ "$(healthz_code)" = "200" ] || fail "keeper так и не поднялся (см. ${KEEPER_DEV_DIR}/keeper.log)"
fi

# ── 2. Сборка git-репо upgrade-demo с тремя тегами ────────────────────────────
step "2. git-репо ${SERVICE_NAME} (теги: ${TAGS[*]})"
# Детерминированные author/committer (как provision.sh): стабильный SHA снапшота
# -> keeper переиспользует кеш, а не плодит сироты при каждом прогоне.
export GIT_AUTHOR_NAME="soul-stack-dev"    GIT_AUTHOR_EMAIL="dev@soul-stack.local"
export GIT_COMMITTER_NAME="soul-stack-dev" GIT_COMMITTER_EMAIL="dev@soul-stack.local"
rm -rf "${REPO_DIR}"
mkdir -p "${REPO_DIR}"
git -C "${REPO_DIR}" init -q -b main
i=0
for tag in "${TAGS[@]}"; do
    [ -d "${TREE_DIR}/${tag}" ] || fail "нет снапшота ${TREE_DIR}/${tag}"
    # Чистим рабочее дерево (кроме .git) и выкладываем снапшот тега — независимые
    # деревья с минимальными диффами (v2.0.1 = v2.0.0 без upgrade/).
    find "${REPO_DIR}" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
    cp -R "${TREE_DIR}/${tag}/." "${REPO_DIR}/"
    # Разные даты на тег — разные коммиты вдоль ветки main (детерминированно).
    d="2020-01-0$((i+1))T00:00:00Z"
    git -C "${REPO_DIR}" add -A
    GIT_AUTHOR_DATE="$d" GIT_COMMITTER_DATE="$d" \
        git -C "${REPO_DIR}" -c commit.gpgsign=false commit -q -m "${SERVICE_NAME} ${tag}"
    git -C "${REPO_DIR}" -c tag.gpgsign=false tag -f "${tag}" >/dev/null
    log "тег ${tag} -> $(git -C "${REPO_DIR}" rev-parse --short "${tag}")"
    i=$((i+1))
done
log "git-репо готов: ${REPO_DIR}"
log "ls-remote --tags (что увидит keeper):"
git ls-remote --tags "${REPO_DIR}" | sed 's/^/    /'

# ── 3. JWT оператора + API-хелперы ────────────────────────────────────────────
step "3. JWT (dev/mint-jwt.sh)"
TOKEN="$(bash "${REPO_ROOT}/dev/mint-jwt.sh" 2>/dev/null)" || fail "mint-jwt упал (VAULT_TOKEN=root? dev-Vault поднят?)"
[ -n "${TOKEN}" ] || fail "пустой JWT"
log "JWT выпущен (len ${#TOKEN})"

AUTH=(-H "Authorization: Bearer ${TOKEN}")
api() {  # api METHOD PATH [JSON]  — curl с Bearer, тело в stdout.
    local method="$1" path="$2" body="${3:-}"
    if [ -n "${body}" ]; then
        curl -s "${AUTH[@]}" -H 'Content-Type: application/json' -X "${method}" \
            -d "${body}" "${API}${path}"
    else
        curl -s "${AUTH[@]}" -X "${method}" "${API}${path}"
    fi
}
api_code() {  # api_code METHOD PATH — только HTTP-код (для poll).
    curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" -X "${1}" "${API}${2}" 2>/dev/null || true
}

# Детект NIM-34-роута: upgrade-paths введён этой фичей. Если запущенный keeper
# собран ДО NIM-34 (напр. чужая dev-сессия подняла старый бинарь), роут отдаёт
# "no such endpoint" (chi-404 после auth) — тогда все upgrade-paths-кейсы были бы
# пусты. Ловим сразу с понятной инструкцией: пересобрать+перезапустить keeper из
# ЭТОГО worktree (его бинарь несёт фичу). 'nope' валидно по path-паттерну, роут
# есть -> 404 not-found (без 'no such endpoint').
if api GET "/v1/incarnations/nope/upgrade-paths" | grep -q "no such endpoint"; then
    fail "keeper на ${API} собран ДО NIM-34 (нет роута upgrade-paths). Пересобери и перезапусти keeper из этого worktree:
    (cd ${REPO_ROOT}/keeper && go build -o bin/keeper ./cmd/keeper) && VAULT_TOKEN=root bash ${REPO_ROOT}/dev/keeper-run.sh"
fi

# ── 4. Реестр сервиса: psql INSERT + ожидание holder ──────────────────────────
step "4. service_registry INSERT + ожидание holder (TTL refresh ~10s)"
if ! psql_cli -c "SELECT to_regclass('public.service_registry') IS NOT NULL" | grep -qx t; then
    fail "нет таблицы service_registry — keeper init не прогонялся? (make dev-provision + keeper)"
fi
psql_cli <<SQL
INSERT INTO service_registry (name, git, ref) VALUES
    ('${SERVICE_NAME}', '${REPO_URL}', 'v1.0.0')
ON CONFLICT (name) DO NOTHING;
SQL
log "реестр (запись ${SERVICE_NAME}):"
psql_cli -c "SELECT name, git, ref FROM service_registry WHERE name='${SERVICE_NAME}'" | sed 's/^/    /'
# Живой keeper держит реестр in-memory-снапшотом (serviceregistry.Holder,
# DefaultRefreshInterval=10s) — прямой psql его НЕ будит синхронно. Ждём, пока
# Resolve увидит сервис: GET .../refs идёт через holder.Resolve -> 200, когда
# снапшот освежён (иначе create инкарнации упал бы 422 service-not-registered).
log "жду, пока holder освежит снапшот (GET /v1/services/${SERVICE_NAME}/refs -> 200, до 20s)"
code=""
for i in $(seq 1 20); do
    code="$(api_code GET "/v1/services/${SERVICE_NAME}/refs")"
    [ "${code}" = "200" ] && { log "holder увидел ${SERVICE_NAME} за ~${i}s"; break; }
    sleep 1
done
[ "${code}" = "200" ] || fail "holder не увидел ${SERVICE_NAME} за 20s (code=${code})"

# ── 5. Bare-создание инкарнации на пине v1.0.0 ────────────────────────────────
step "5. Bare-инкарнация ${INC_NAME} (service=${SERVICE_NAME})"
CREATE_RESP="$(api POST /v1/incarnations "{\"name\":\"${INC_NAME}\",\"service\":\"${SERVICE_NAME}\"}")"
printf '%s\n' "${CREATE_RESP}" | pretty
# Bare: без create-сценария -> синхронно ready, без apply_id.
GET_RESP="$(api GET "/v1/incarnations/${INC_NAME}")"
log "GET /v1/incarnations/${INC_NAME}:"
printf '%s\n' "${GET_RESP}" | pretty
echo "${GET_RESP}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
st  = d.get("status"); sv = d.get("service_version"); ss = d.get("state_schema_version")
print(f"\n[assert] status={st} service_version={sv} state_schema_version={ss}")
assert st == "ready",        f"ожидал status=ready, получил {st}"
assert sv == "v1.0.0",       f"ожидал service_version=v1.0.0, получил {sv}"
assert ss == 1,              f"ожидал state_schema_version=1, получил {ss}"
print("[assert] OK: bare-инкарнация ready на пине v1.0.0, schema=1")
' || fail "assert по созданной инкарнации не прошёл"

# ── 6. Кейсы upgrade-paths / upgrade ──────────────────────────────────────────
step "6a. CHEAP: GET upgrade-paths (без ?to=) — список тегов + is_current"
CHEAP="$(api GET "/v1/incarnations/${INC_NAME}/upgrade-paths")"
printf '%s\n' "${CHEAP}" | pretty

step "6b. FOUND: GET upgrade-paths?to=v2.0.0 — mode=found, slug=to_v2, миграции [1->2]"
FOUND="$(api GET "/v1/incarnations/${INC_NAME}/upgrade-paths?to=v2.0.0")"
printf '%s\n' "${FOUND}" | pretty

step "6c. LEGACY: GET upgrade-paths?to=v2.0.1 — mode=legacy, reachable, миграции [1->2]"
LEGACY="$(api GET "/v1/incarnations/${INC_NAME}/upgrade-paths?to=v2.0.1")"
printf '%s\n' "${LEGACY}" | pretty

step "6d. LIVE LEGACY UPGRADE: POST upgrade {to_version: v2.0.1} -> 202, затем drift"
UP="$(api POST "/v1/incarnations/${INC_NAME}/upgrade" '{"to_version":"v2.0.1"}')"
printf '%s\n' "${UP}" | pretty
# Legacy -> смена пина + миграция + drift одной tx; дадим keeper момент устаканиться.
for _ in $(seq 1 10); do
    POST_GET="$(api GET "/v1/incarnations/${INC_NAME}")"
    st="$(echo "${POST_GET}" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
    [ "${st}" = "applying" ] || break
    sleep 1
done
log "GET /v1/incarnations/${INC_NAME} после upgrade:"
printf '%s\n' "${POST_GET}" | pretty
echo "${POST_GET}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
st = d.get("status"); sv = d.get("service_version"); ss = d.get("state_schema_version")
print(f"\n[assert] status={st} service_version={sv} state_schema_version={ss}")
assert sv == "v2.0.1", f"ожидал service_version=v2.0.1, получил {sv}"
assert ss == 2,        f"ожидал state_schema_version=2, получил {ss}"
assert st == "drift",  f"ожидал status=drift (legacy-upgrade без хостов), получил {st}"
print("[assert] OK: legacy-upgrade сменил пин v1.0.0->v2.0.1, мигрировал schema 1->2, status=drift")
' || fail "assert после upgrade не прошёл"

# ── 7. Итог + как повторить + очистка ─────────────────────────────────────────
step "7. Готово"
cat <<EOF

Инкарнация ${INC_NAME} прошла путь: v1.0.0 (ready) -> v2.0.1 (drift, schema 2).

Кейсы, доказанные прогоном:
  6a cheap   — теги v1.0.0/v2.0.0/v2.0.1 + is_current=v1.0.0
  6b found   — ?to=v2.0.0: mode=found, slug=to_v2, reachable, state_migrations [1->2]
  6c legacy  — ?to=v2.0.1: mode=legacy, reachable, state_migrations [1->2]
  6d upgrade — живой legacy POST upgrade -> 202 -> drift (пин v2.0.1, schema 2)

Повторить вручную (JWT в переменной ниже):
  TOKEN=\$(VAULT_TOKEN=root bash ${REPO_ROOT}/dev/mint-jwt.sh)
  curl -s -H "Authorization: Bearer \$TOKEN" ${API}/v1/incarnations/${INC_NAME}/upgrade-paths | python3 -m json.tool
  curl -s -H "Authorization: Bearer \$TOKEN" "${API}/v1/incarnations/${INC_NAME}/upgrade-paths?to=v2.0.0" | python3 -m json.tool

Очистить эту инкарнацию (force-destroy без хостов):
  curl -s -X DELETE -H "Authorization: Bearer \$TOKEN" "${API}/v1/incarnations/${INC_NAME}?allow_destroy=true"

Сервис ${SERVICE_NAME} и его репо остаются в реестре для повторных прогонов.
EOF
