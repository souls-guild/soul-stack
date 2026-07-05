#!/usr/bin/env bash
#
# dev/upgrade-demo/ui-stand.sh — КЛИКАБЕЛЬНЫЙ UI-стенд фичи NIM-34 (upgrade v2).
#
# Поднимает ИЗОЛИРОВАННЫЙ демо-keeper (:8090, dev/upgrade-demo/keeper-demo.yml,
# без push-блока) + web-UI (:5174, companion-репо soul-stack-web ветка
# feat/nim34-upgrade-paths-ui) с прокси /v1 -> :8090. Сеет демо-сервис
# upgrade-demo (3 тега) и инкарнацию updemo-ui на пине v1.0.0. В конце печатает
# URL + JWT + что кликать.
#
# Изоляция от общего стенда (чужая dev-сессия на :8080): свои порты (8090/5174),
# свой kid, acolytes:0 + reaper/voyage OFF (не трогает данные соседа на общем PG).
#
# Запуск:  bash dev/upgrade-demo/ui-stand.sh
# Env:
#   KEEPER_DEV_DIR — корень dev-артефактов (default /tmp/keeper-dev)
#   WEB_DIR        — путь к soul-stack-web (default рядом с корнем soul-stack)
#   PG_DSN         — DSN dev-Postgres

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TREE_DIR="${SCRIPT_DIR}/tree"
DEV="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
API="http://127.0.0.1:8090"
KCONF="${SCRIPT_DIR}/keeper-demo.yml"
WEB_PORT=5174
# soul-stack-web — sibling КОРНЯ soul-stack (не worktree). REPO_ROOT здесь = worktree,
# поэтому берём главный репо (../../.. от dev/upgrade-demo -> worktree; главный —
# на 3 уровня выше .claude/worktrees/<wt>). Явный дефолт по известному размещению.
WEB_DIR="${WEB_DIR:-/home/co-cy/vscode/soulstack/soul-stack-web}"
PG_DSN="${PG_DSN:-postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable}"
# ГРАБЛЯ (MEMORY): в shell оператора часто прод-VAULT_TOKEN — dev-Vault его отвергает.
export VAULT_TOKEN=root VAULT_ADDR=http://127.0.0.1:8200

SERVICE=upgrade-demo
REPO_DIR="${DEV}/repos/${SERVICE}"
REPO_URL="file://${REPO_DIR}"
TAGS=(v1.0.0 v2.0.0 v2.0.1)
INC=updemo-ui

log()  { printf '\n[ui-stand] %s\n' "$*"; }
step() { printf '\n\033[1m===== %s =====\033[0m\n' "$*"; }
fail() { printf '\n[ui-stand][FAIL] %s\n' "$*" >&2; exit 1; }

command -v git   >/dev/null 2>&1 || fail "нет git"
command -v curl  >/dev/null 2>&1 || fail "нет curl"
command -v npm   >/dev/null 2>&1 || fail "нет npm"
command -v openssl >/dev/null 2>&1 || fail "нет openssl (нужен для sigil-ключа)"
[ -d "${TREE_DIR}" ] || fail "нет ${TREE_DIR}"
[ -f "${KCONF}" ]    || fail "нет ${KCONF}"
[ -d "${WEB_DIR}" ]  || fail "нет web-репо ${WEB_DIR} (задай WEB_DIR=<путь>)"

healthz(){ curl -s -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || echo 000; }
# kill процессов по паттерну cmdline, НО только с нужным comm (не self-kill шелла).
kill_by(){ local pat="$1" want="$2" p; for p in $(pgrep -f "$pat" 2>/dev/null || true); do [ "$(ps -o comm= -p "$p" 2>/dev/null)" = "$want" ] && kill -9 "$p" 2>/dev/null || true; done; }

if command -v psql >/dev/null 2>&1 && psql "${PG_DSN}" -c 'SELECT 1' >/dev/null 2>&1; then
    psql_cli(){ psql "${PG_DSN}" -v ON_ERROR_STOP=1 -tAq "$@"; }
else
    psql_cli(){ docker exec -i soul-stack-postgres psql -U keeper -d keeper -v ON_ERROR_STOP=1 -tAq "$@"; }
fi

# ── 1. Инфра (docker + Vault + ключи) ─────────────────────────────────────────
step "1. Инфра: docker + Vault + jwt/sigil-ключи"
if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q soul-stack-vault; then
    log "docker-инфра не поднята — make dev-up"
    make -C "${REPO_ROOT}" dev-up
    for _ in $(seq 1 30); do [ "$(healthz http://127.0.0.1:8200/v1/sys/health)" != 000 ] && break; sleep 1; done
fi
if ! bash "${REPO_ROOT}/dev/mint-jwt.sh" >/dev/null 2>&1; then
    log "mint-jwt не проходит (нет jwt-ключа/TLS) — make dev-provision"
    make -C "${REPO_ROOT}" dev-provision
fi
if ! docker exec -e VAULT_TOKEN=root soul-stack-vault vault kv get -field=signing_key secret/keeper/sigil-signing-key >/dev/null 2>&1; then
    log "нет sigil-signing-key — генерю ed25519 и кладу в Vault"
    openssl genpkey -algorithm ed25519 -out "${DEV}/sigil-signing.pem" 2>/dev/null
    docker cp "${DEV}/sigil-signing.pem" soul-stack-vault:/tmp/sigil.pem >/dev/null
    docker exec -e VAULT_TOKEN=root soul-stack-vault vault kv put secret/keeper/sigil-signing-key signing_key=@/tmp/sigil.pem >/dev/null
fi

# ── 2. Демо-keeper :8090 ──────────────────────────────────────────────────────
step "2. Демо-keeper :8090 (keeper-demo.yml)"
if [ "$(healthz ${API}/healthz)" = 200 ]; then
    log "keeper уже на :8090 — ПЕРЕИСПОЛЬЗУЮ"
else
    log "собираю keeper из worktree (несёт роут upgrade-paths)"
    ( cd "${REPO_ROOT}/keeper" && go build -o bin/keeper ./cmd/keeper )
    kill_by 'keeper-demo\.yml' keeper
    sleep 1
    mkdir -p "${DEV}/services" "${DEV}/destiny-cache" "${DEV}/plugin-sockets-demo"
    KEEPER_SERVICE_CACHE_DIR="${DEV}/services" KEEPER_DESTINY_CACHE_DIR="${DEV}/destiny-cache" SOUL_STACK_ALLOW_FILE_REPOS=1 \
        nohup "${REPO_ROOT}/keeper/bin/keeper" run --config="${KCONF}" > "${DEV}/keeper-demo.log" 2>&1 &
    kp=$!
    for i in $(seq 1 30); do [ "$(healthz ${API}/healthz)" = 200 ] && { log "healthz :8090 = 200 (${i}s)"; break; }; kill -0 "$kp" 2>/dev/null || { tail -14 "${DEV}/keeper-demo.log"; fail "демо-keeper умер на старте"; }; sleep 1; done
    [ "$(healthz ${API}/healthz)" = 200 ] || { tail -14 "${DEV}/keeper-demo.log"; fail "демо-keeper не поднялся"; }
fi

# ── 3. git-репо upgrade-demo (3 тега) ─────────────────────────────────────────
step "3. git-репо ${SERVICE} (${TAGS[*]})"
export GIT_AUTHOR_NAME="soul-stack-dev" GIT_AUTHOR_EMAIL="dev@soul-stack.local"
export GIT_COMMITTER_NAME="soul-stack-dev" GIT_COMMITTER_EMAIL="dev@soul-stack.local"
rm -rf "${REPO_DIR}"; mkdir -p "${REPO_DIR}"
git -C "${REPO_DIR}" init -q -b main
i=0
for tag in "${TAGS[@]}"; do
    [ -d "${TREE_DIR}/${tag}" ] || fail "нет ${TREE_DIR}/${tag}"
    find "${REPO_DIR}" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
    cp -R "${TREE_DIR}/${tag}/." "${REPO_DIR}/"
    d="2020-01-0$((i+1))T00:00:00Z"
    git -C "${REPO_DIR}" add -A
    GIT_AUTHOR_DATE="$d" GIT_COMMITTER_DATE="$d" git -C "${REPO_DIR}" -c commit.gpgsign=false commit -q -m "${SERVICE} ${tag}"
    git -C "${REPO_DIR}" -c tag.gpgsign=false tag -f "${tag}" >/dev/null
    i=$((i+1))
done
log "теги: $(git -C "${REPO_DIR}" tag | tr '\n' ' ')"

# ── 4. JWT + реестр + ожидание holder ─────────────────────────────────────────
step "4. JWT + service_registry + holder"
TOKEN="$(bash "${REPO_ROOT}/dev/mint-jwt.sh" 2>/dev/null)" || fail "mint-jwt упал"
[ -n "${TOKEN}" ] || fail "пустой JWT"
AUTH=(-H "Authorization: Bearer ${TOKEN}")
psql_cli <<SQL
INSERT INTO service_registry (name, git, ref) VALUES ('${SERVICE}', '${REPO_URL}', 'v1.0.0')
ON CONFLICT (name) DO UPDATE SET git=EXCLUDED.git, ref=EXCLUDED.ref;
SQL
for i in $(seq 1 20); do [ "$(curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${API}/v1/services/${SERVICE}/refs")" = 200 ] && { log "holder увидел ${SERVICE} (~${i}s)"; break; }; sleep 1; done

# ── 5. Свежая инкарнация updemo-ui на v1.0.0 (для клика) ───────────────────────
step "5. Инкарнация ${INC} на v1.0.0"
curl -s -X DELETE "${AUTH[@]}" "${API}/v1/incarnations/${INC}?allow_destroy=true" -o /dev/null || true
sleep 1
curl -s -X POST "${AUTH[@]}" -H 'Content-Type: application/json' "${API}/v1/incarnations" -d "{\"name\":\"${INC}\",\"service\":\"${SERVICE}\"}" -o /dev/null -w '[ui-stand] create HTTP %{http_code}\n'
curl -s "${AUTH[@]}" "${API}/v1/incarnations/${INC}" | python3 -c 'import sys,json;d=json.load(sys.stdin);print("[ui-stand]",d.get("name"),d.get("service_version"),"schema",d.get("state_schema_version"),d.get("status"))'

# ── 6. Web-UI :5174 → :8090 ───────────────────────────────────────────────────
step "6. Web-UI :${WEB_PORT} (ветка feat/nim34-upgrade-paths-ui)"
if [ "$(git -C "${WEB_DIR}" branch --show-current)" != "feat/nim34-upgrade-paths-ui" ]; then
    log "ВНИМАНИЕ: web-репо не на feat/nim34-upgrade-paths-ui (текущая: $(git -C "${WEB_DIR}" branch --show-current)) — панель upgrade-paths не покажется. Переключи: git -C ${WEB_DIR} checkout feat/nim34-upgrade-paths-ui"
fi
if [ "$(healthz http://127.0.0.1:${WEB_PORT})" = 200 ] || [ "$(healthz http://127.0.0.1:${WEB_PORT})" = 302 ]; then
    log "web уже на :${WEB_PORT} — ПЕРЕИСПОЛЬЗУЮ"
else
    kill_by "vite.*--port ${WEB_PORT}" node
    sleep 1
    ( cd "${WEB_DIR}" && VITE_KEEPER_API=http://localhost:8090 nohup npm run dev -- --host --port "${WEB_PORT}" > "${DEV}/web-demo.log" 2>&1 & )
    for i in $(seq 1 45); do c=$(healthz http://127.0.0.1:${WEB_PORT}); { [ "$c" = 200 ] || [ "$c" = 302 ]; } && { log "web :${WEB_PORT} готов (~${i}s)"; break; }; sleep 1; done
fi

# ── 7. Инструкция ─────────────────────────────────────────────────────────────
step "7. ГОТОВО — открывай в браузере"
cat <<EOF

  URL:    http://localhost:${WEB_PORT}/ui/
  Логин:  вставь JWT ниже в поле токена (Login):

${TOKEN}

  Дальше:
    1. Incarnations -> открой  ${INC}  (пин v1.0.0, status ready)
    2. Нажми кнопку  Upgrade
    3. В дропдауне "To version" выбери:
         v2.0.0  -> панель: mode=FOUND (зелёный), direction forward, миграции 1->2  (host-оркестрация запустится)
         v2.0.1  -> панель: mode=LEGACY (серый), direction forward, миграции 1->2   (смена версии -> drift)
    4. (опц.) нажми Upgrade на v2.0.1 -> инкарнация уйдёт в drift на v2.0.1 (schema 2) — живой legacy-переход без хостов.

  Демо-keeper: :8090 (лог ${DEV}/keeper-demo.log) | web: :${WEB_PORT} (лог ${DEV}/web-demo.log)
  Остановить:  pkill -f 'keeper-demo.yml' ; pkill -f 'vite.*--port ${WEB_PORT}'
EOF
