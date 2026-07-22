#!/usr/bin/env bash
#
# dev/upgrade-demo/ui-stand.sh - CLICKABLE UI stand for feature NIM-34 (upgrade v2).
#
# Brings up an ISOLATED demo-keeper (:8090, dev/upgrade-demo/keeper-demo.yml,
# without the push block) + web-UI (:5174, companion repo soul-stack-web branch
# feat/nim34-upgrade-paths-ui) with proxy /v1 -> :8090. Seeds the demo service
# upgrade-demo (3 tags) and the incarnation updemo-ui pinned at v1.0.0. Prints
# the URL + JWT + what to click at the end.
#
# Isolation from the shared stand (someone else's dev session on :8080): its own ports (8090/5174),
# its own kid, acolytes:0 + reaper/voyage OFF (does not touch the neighbor's data on the shared PG).
#
# Run:  bash dev/upgrade-demo/ui-stand.sh
# Env:
#   KEEPER_DEV_DIR - root of dev artifacts (default /tmp/keeper-dev)
#   WEB_DIR        - path to soul-stack-web (default next to the soul-stack root)
#   PG_DSN         - DSN of the dev Postgres

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TREE_DIR="${SCRIPT_DIR}/tree"
DEV="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
API="http://127.0.0.1:8090"
KCONF="${SCRIPT_DIR}/keeper-demo.yml"
WEB_PORT=5174
# soul-stack-web is a sibling checkout of the soul-stack repo (../soul-stack-web from the repo root).
# Override with WEB_DIR=/path/to/soul-stack-web if your layout differs.
WEB_DIR="${WEB_DIR:-$(cd "${REPO_ROOT}/.." 2>/dev/null && pwd)/soul-stack-web}"
PG_DSN="${PG_DSN:-postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable}"
# GOTCHA (MEMORY): the operator's shell often carries a prod VAULT_TOKEN - the dev Vault rejects it.
export VAULT_TOKEN=root VAULT_ADDR=http://127.0.0.1:8200

SERVICE=upgrade-demo
REPO_DIR="${DEV}/repos/${SERVICE}"
REPO_URL="file://${REPO_DIR}"
TAGS=(v1.0.0 v2.0.0 v2.0.1)
INC=updemo-ui

log()  { printf '\n[ui-stand] %s\n' "$*"; }
step() { printf '\n\033[1m===== %s =====\033[0m\n' "$*"; }
fail() { printf '\n[ui-stand][FAIL] %s\n' "$*" >&2; exit 1; }

command -v git   >/dev/null 2>&1 || fail "git not found"
command -v curl  >/dev/null 2>&1 || fail "curl not found"
command -v npm   >/dev/null 2>&1 || fail "npm not found"
command -v openssl >/dev/null 2>&1 || fail "openssl not found (needed for the sigil key)"
[ -d "${TREE_DIR}" ] || fail "${TREE_DIR} not found"
[ -f "${KCONF}" ]    || fail "${KCONF} not found"
[ -d "${WEB_DIR}" ]  || fail "web repo not found at ${WEB_DIR} (set WEB_DIR=<path>)"

healthz(){ curl -s -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || echo 000; }
# kill processes matching a cmdline pattern, BUT only with the expected comm (avoid self-killing the shell).
kill_by(){ local pat="$1" want="$2" p; for p in $(pgrep -f "$pat" 2>/dev/null || true); do [ "$(ps -o comm= -p "$p" 2>/dev/null)" = "$want" ] && kill -9 "$p" 2>/dev/null || true; done; }

if command -v psql >/dev/null 2>&1 && psql "${PG_DSN}" -c 'SELECT 1' >/dev/null 2>&1; then
    psql_cli(){ psql "${PG_DSN}" -v ON_ERROR_STOP=1 -tAq "$@"; }
else
    psql_cli(){ docker exec -i soul-stack-postgres psql -U keeper -d keeper -v ON_ERROR_STOP=1 -tAq "$@"; }
fi

# ── 1. Infra (docker + Vault + keys) ─────────────────────────────────────────
step "1. Infra: docker + Vault + jwt/sigil keys"
if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q soul-stack-vault; then
    log "docker infra is not up - running make dev-up"
    make -C "${REPO_ROOT}" dev-up
    for _ in $(seq 1 30); do [ "$(healthz http://127.0.0.1:8200/v1/sys/health)" != 000 ] && break; sleep 1; done
fi
if ! bash "${REPO_ROOT}/dev/mint-jwt.sh" >/dev/null 2>&1; then
    log "mint-jwt fails (no jwt key/TLS) - running make dev-provision"
    make -C "${REPO_ROOT}" dev-provision
fi
if ! docker exec -e VAULT_TOKEN=root soul-stack-vault vault kv get -field=signing_key secret/keeper/sigil-signing-key >/dev/null 2>&1; then
    log "no sigil-signing-key - generating ed25519 and storing it in Vault"
    openssl genpkey -algorithm ed25519 -out "${DEV}/sigil-signing.pem" 2>/dev/null
    docker cp "${DEV}/sigil-signing.pem" soul-stack-vault:/tmp/sigil.pem >/dev/null
    docker exec -e VAULT_TOKEN=root soul-stack-vault vault kv put secret/keeper/sigil-signing-key signing_key=@/tmp/sigil.pem >/dev/null
fi

# ── 2. Demo-keeper :8090 ──────────────────────────────────────────────────────
step "2. Demo-keeper :8090 (keeper-demo.yml)"
if [ "$(healthz ${API}/healthz)" = 200 ]; then
    log "keeper already on :8090 - REUSING"
else
    log "building keeper from the worktree (carries the upgrade-paths route)"
    ( cd "${REPO_ROOT}/keeper" && go build -o bin/keeper ./cmd/keeper )
    kill_by 'keeper-demo\.yml' keeper
    sleep 1
    mkdir -p "${DEV}/services" "${DEV}/destiny-cache" "${DEV}/plugin-sockets-demo"
    KEEPER_SERVICE_CACHE_DIR="${DEV}/services" KEEPER_DESTINY_CACHE_DIR="${DEV}/destiny-cache" SOUL_STACK_ALLOW_FILE_REPOS=1 \
        nohup "${REPO_ROOT}/keeper/bin/keeper" run --config="${KCONF}" > "${DEV}/keeper-demo.log" 2>&1 &
    kp=$!
    for i in $(seq 1 30); do [ "$(healthz ${API}/healthz)" = 200 ] && { log "healthz :8090 = 200 (${i}s)"; break; }; kill -0 "$kp" 2>/dev/null || { tail -14 "${DEV}/keeper-demo.log"; fail "demo-keeper died on startup"; }; sleep 1; done
    [ "$(healthz ${API}/healthz)" = 200 ] || { tail -14 "${DEV}/keeper-demo.log"; fail "demo-keeper failed to come up"; }
fi

# ── 3. git repo upgrade-demo (3 tags) ─────────────────────────────────────────
step "3. git repo ${SERVICE} (${TAGS[*]})"
export GIT_AUTHOR_NAME="soul-stack-dev" GIT_AUTHOR_EMAIL="dev@soul-stack.local"
export GIT_COMMITTER_NAME="soul-stack-dev" GIT_COMMITTER_EMAIL="dev@soul-stack.local"
rm -rf "${REPO_DIR}"; mkdir -p "${REPO_DIR}"
git -C "${REPO_DIR}" init -q -b main
i=0
for tag in "${TAGS[@]}"; do
    [ -d "${TREE_DIR}/${tag}" ] || fail "${TREE_DIR}/${tag} not found"
    find "${REPO_DIR}" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
    cp -R "${TREE_DIR}/${tag}/." "${REPO_DIR}/"
    d="2020-01-0$((i+1))T00:00:00Z"
    git -C "${REPO_DIR}" add -A
    GIT_AUTHOR_DATE="$d" GIT_COMMITTER_DATE="$d" git -C "${REPO_DIR}" -c commit.gpgsign=false commit -q -m "${SERVICE} ${tag}"
    git -C "${REPO_DIR}" -c tag.gpgsign=false tag -f "${tag}" >/dev/null
    i=$((i+1))
done
log "tags: $(git -C "${REPO_DIR}" tag | tr '\n' ' ')"

# ── 4. JWT + registry + waiting for the holder ─────────────────────────────────────────
step "4. JWT + service_registry + holder"
TOKEN="$(bash "${REPO_ROOT}/dev/mint-jwt.sh" 2>/dev/null)" || fail "mint-jwt failed"
[ -n "${TOKEN}" ] || fail "empty JWT"
AUTH=(-H "Authorization: Bearer ${TOKEN}")
psql_cli <<SQL
INSERT INTO service_registry (name, git, ref) VALUES ('${SERVICE}', '${REPO_URL}', 'v1.0.0')
ON CONFLICT (name) DO UPDATE SET git=EXCLUDED.git, ref=EXCLUDED.ref;
SQL
for i in $(seq 1 20); do [ "$(curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${API}/v1/services/${SERVICE}/refs")" = 200 ] && { log "holder saw ${SERVICE} (~${i}s)"; break; }; sleep 1; done

# ── 5. Fresh incarnation updemo-ui at v1.0.0 (for clicking) ───────────────────────
step "5. Incarnation ${INC} at v1.0.0"
curl -s -X DELETE "${AUTH[@]}" "${API}/v1/incarnations/${INC}?allow_destroy=true" -o /dev/null || true
sleep 1
curl -s -X POST "${AUTH[@]}" -H 'Content-Type: application/json' "${API}/v1/incarnations" -d "{\"name\":\"${INC}\",\"service\":\"${SERVICE}\"}" -o /dev/null -w '[ui-stand] create HTTP %{http_code}\n'
curl -s "${AUTH[@]}" "${API}/v1/incarnations/${INC}" | python3 -c 'import sys,json;d=json.load(sys.stdin);print("[ui-stand]",d.get("name"),d.get("service_version"),"schema",d.get("state_schema_version"),d.get("status"))'

# ── 6. Web-UI :5174 → :8090 ───────────────────────────────────────────────────
step "6. Web-UI :${WEB_PORT} (branch feat/nim34-upgrade-paths-ui)"
if [ "$(git -C "${WEB_DIR}" branch --show-current)" != "feat/nim34-upgrade-paths-ui" ]; then
    log "WARNING: the web repo is not on feat/nim34-upgrade-paths-ui (current: $(git -C "${WEB_DIR}" branch --show-current)) - the upgrade-paths panel will not show. Switch it: git -C ${WEB_DIR} checkout feat/nim34-upgrade-paths-ui"
fi
if [ "$(healthz http://127.0.0.1:${WEB_PORT})" = 200 ] || [ "$(healthz http://127.0.0.1:${WEB_PORT})" = 302 ]; then
    log "web already on :${WEB_PORT} - REUSING"
else
    kill_by "vite.*--port ${WEB_PORT}" node
    sleep 1
    ( cd "${WEB_DIR}" && VITE_KEEPER_API=http://localhost:8090 nohup npm run dev -- --host --port "${WEB_PORT}" > "${DEV}/web-demo.log" 2>&1 & )
    for i in $(seq 1 45); do c=$(healthz http://127.0.0.1:${WEB_PORT}); { [ "$c" = 200 ] || [ "$c" = 302 ]; } && { log "web :${WEB_PORT} ready (~${i}s)"; break; }; sleep 1; done
fi

# ── 7. Instructions ─────────────────────────────────────────────────────────────
step "7. DONE - open in the browser"
cat <<EOF

  URL:    http://localhost:${WEB_PORT}/ui/
  Login:  paste the JWT below into the token field (Login):

${TOKEN}

  Next:
    1. Incarnations -> open  ${INC}  (pinned v1.0.0, status ready)
    2. Click the  Upgrade  button
    3. In the "To version" dropdown pick:
         v2.0.0  -> panel: mode=FOUND (green), direction forward, migrations 1->2  (host orchestration will start)
         v2.0.1  -> panel: mode=LEGACY (gray), direction forward, migrations 1->2   (version change -> drift)
    4. (optional) click Upgrade on v2.0.1 -> the incarnation will drift to v2.0.1 (schema 2) - a live legacy transition with no hosts.

  Demo-keeper: :8090 (log ${DEV}/keeper-demo.log) | web: :${WEB_PORT} (log ${DEV}/web-demo.log)
  Stop:  pkill -f 'keeper-demo.yml' ; pkill -f 'vite.*--port ${WEB_PORT}'
EOF
