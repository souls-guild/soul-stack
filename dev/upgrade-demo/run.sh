#!/usr/bin/env bash
#
# dev/upgrade-demo/run.sh - live manual test of the NIM-34 feature (upgrade v2, ADR-0068).
#
# What it does: materializes the demo service `upgrade-demo` (a git repo with three tags
# v1.0.0/v2.0.0/v2.0.1 from dev/upgrade-demo/tree/), seeds it into service_registry,
# bare-creates an incarnation (without hosts) pinned at v1.0.0, and cycles curl through
# upgrade-paths / upgrade, printing responses. Demonstrates cheap / found / legacy /
# state migrations + a live legacy-upgrade -> drift. Full case coverage - README.md.
#
# Idempotency: the service repo and registry are recreated/ON CONFLICT DO NOTHING;
# the incarnation - a unique name updemo-<rand> per run (a cleanup hint is printed at the end).
#
# The stand is REUSED if keeper is already up on :8080 (healthz 200). Otherwise the script
# tries to bring up the chain (make dev-up -> dev-provision if TLS is missing ->
# dev-keeper). Foreign data (redis/hello-world, other incarnations) is left untouched.
#
# Run:  bash dev/upgrade-demo/run.sh
#
# Env (all with defaults):
#   KEEPER_DEV_DIR  - root of dev artifacts (default /tmp/keeper-dev)
#   API             - Operator API base (default http://127.0.0.1:8080)
#   PG_DSN          - dev-Postgres DSN (default postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable)
#   VAULT_TOKEN     - forced to root (dev-Vault); a prod token from env is ignored.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TREE_DIR="${SCRIPT_DIR}/tree"

KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
API="${API:-http://127.0.0.1:8080}"
PG_DSN="${PG_DSN:-postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable}"
# GOTCHA (MEMORY): the operator's shell often inherits a PROD-VAULT_TOKEN - dev-Vault
# rejects it. Force root explicitly for the whole script (mint-jwt reads VAULT_TOKEN).
export VAULT_TOKEN=root

SERVICE_NAME="upgrade-demo"
REPO_DIR="${KEEPER_DEV_DIR}/repos/${SERVICE_NAME}"
REPO_URL="file://${REPO_DIR}"
TAGS=(v1.0.0 v2.0.0 v2.0.1)
# Unique incarnation name per run (don't touch someone else's, short rand suffix).
INC_NAME="updemo-$(date +%s | tail -c 6)$(( RANDOM % 90 + 10 ))"

log()  { printf '\n[upgrade-demo] %s\n' "$*"; }
step() { printf '\n\033[1m========== %s ==========\033[0m\n' "$*"; }
fail() { printf '\n[upgrade-demo] [FAIL] %s\n' "$*" >&2; exit 1; }

# pretty - JSON via python3 (jq is not guaranteed on the dev machine).
pretty() { python3 -m json.tool 2>/dev/null || cat; }

command -v git >/dev/null 2>&1 || fail "git not found in PATH"
command -v curl >/dev/null 2>&1 || fail "curl not found in PATH"
command -v python3 >/dev/null 2>&1 || fail "python3 not found in PATH"
[ -d "${TREE_DIR}" ] || fail "no snapshot directory: ${TREE_DIR}"

# ── psql wrapper (as in dev/provision.sh): host psql or docker exec fallback ──
if command -v psql >/dev/null 2>&1 && psql "${PG_DSN}" -c 'SELECT 1' >/dev/null 2>&1; then
    psql_cli() { psql "${PG_DSN}" -v ON_ERROR_STOP=1 -tAq "$@"; }
else
    psql_cli() { docker exec -i soul-stack-postgres psql -U keeper -d keeper -v ON_ERROR_STOP=1 -tAq "$@"; }
fi

# ── 1. Boot-or-reuse keeper ──────────────────────────────────────────────────
step "1. Keeper: reuse or boot"
healthz_code() { curl -s -o /dev/null -w '%{http_code}' "${API}/healthz" 2>/dev/null || true; }
if [ "$(healthz_code)" = "200" ]; then
    log "keeper already up on ${API} (healthz 200) - REUSING (skipping dev-provision/dev-keeper)"
else
    log "keeper not responding - bringing up the chain (make dev-up -> [dev-provision] -> dev-keeper)"
    make -C "${REPO_ROOT}" dev-up
    if [ ! -s "${KEEPER_DEV_DIR}/tls/keeper.crt" ]; then
        log "no TLS material - running dev-provision (once)"
        make -C "${REPO_ROOT}" dev-provision
    fi
    make -C "${REPO_ROOT}" dev-keeper
    [ "$(healthz_code)" = "200" ] || fail "keeper never came up (see ${KEEPER_DEV_DIR}/keeper.log)"
fi

# ── 2. Building the git repo upgrade-demo with three tags ────────────────────────────
step "2. git repo ${SERVICE_NAME} (tags: ${TAGS[*]})"
# Deterministic author/committer (as in provision.sh): a stable snapshot SHA
# -> keeper reuses the cache instead of spawning orphans on every run.
export GIT_AUTHOR_NAME="soul-stack-dev"    GIT_AUTHOR_EMAIL="dev@soul-stack.local"
export GIT_COMMITTER_NAME="soul-stack-dev" GIT_COMMITTER_EMAIL="dev@soul-stack.local"
rm -rf "${REPO_DIR}"
mkdir -p "${REPO_DIR}"
git -C "${REPO_DIR}" init -q -b main
i=0
for tag in "${TAGS[@]}"; do
    [ -d "${TREE_DIR}/${tag}" ] || fail "no snapshot ${TREE_DIR}/${tag}"
    # Clean the working tree (except .git) and lay down the tag snapshot - independent
    # trees with minimal diffs (v2.0.1 = v2.0.0 without upgrade/).
    find "${REPO_DIR}" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
    cp -R "${TREE_DIR}/${tag}/." "${REPO_DIR}/"
    # Different dates per tag - different commits along the main branch (deterministic).
    d="2020-01-0$((i+1))T00:00:00Z"
    git -C "${REPO_DIR}" add -A
    GIT_AUTHOR_DATE="$d" GIT_COMMITTER_DATE="$d" \
        git -C "${REPO_DIR}" -c commit.gpgsign=false commit -q -m "${SERVICE_NAME} ${tag}"
    git -C "${REPO_DIR}" -c tag.gpgsign=false tag -f "${tag}" >/dev/null
    log "tag ${tag} -> $(git -C "${REPO_DIR}" rev-parse --short "${tag}")"
    i=$((i+1))
done
log "git repo ready: ${REPO_DIR}"
log "ls-remote --tags (what keeper will see):"
git ls-remote --tags "${REPO_DIR}" | sed 's/^/    /'

# ── 3. Operator JWT + API helpers ────────────────────────────────────────────
step "3. JWT (dev/mint-jwt.sh)"
TOKEN="$(bash "${REPO_ROOT}/dev/mint-jwt.sh" 2>/dev/null)" || fail "mint-jwt failed (VAULT_TOKEN=root? dev-Vault up?)"
[ -n "${TOKEN}" ] || fail "empty JWT"
log "JWT issued (len ${#TOKEN})"

AUTH=(-H "Authorization: Bearer ${TOKEN}")
api() {  # api METHOD PATH [JSON]  - curl with Bearer, body to stdout.
    local method="$1" path="$2" body="${3:-}"
    if [ -n "${body}" ]; then
        curl -s "${AUTH[@]}" -H 'Content-Type: application/json' -X "${method}" \
            -d "${body}" "${API}${path}"
    else
        curl -s "${AUTH[@]}" -X "${method}" "${API}${path}"
    fi
}
api_code() {  # api_code METHOD PATH - HTTP code only (for poll).
    curl -s -o /dev/null -w '%{http_code}' "${AUTH[@]}" -X "${1}" "${API}${2}" 2>/dev/null || true
}

# Detect the NIM-34 route: upgrade-paths was introduced by this feature. If the running
# keeper was built BEFORE NIM-34 (e.g. someone else's dev session started an old binary),
# the route returns "no such endpoint" (chi-404 after auth) - then all upgrade-paths cases
# would be empty. Catch this early with a clear instruction: rebuild+restart keeper from
# THIS worktree (its binary carries the feature). 'nope' is valid per the path pattern, the
# route exists -> 404 not-found (without 'no such endpoint').
if api GET "/v1/incarnations/nope/upgrade-paths" | grep -q "no such endpoint"; then
    fail "keeper at ${API} was built BEFORE NIM-34 (no upgrade-paths route). Rebuild and restart keeper from this worktree:
    (cd ${REPO_ROOT}/keeper && go build -o bin/keeper ./cmd/keeper) && VAULT_TOKEN=root bash ${REPO_ROOT}/dev/keeper-run.sh"
fi

# ── 4. Service registry: psql INSERT + wait for holder ──────────────────────────
step "4. service_registry INSERT + wait for holder (TTL refresh ~10s)"
if ! psql_cli -c "SELECT to_regclass('public.service_registry') IS NOT NULL" | grep -qx t; then
    fail "no service_registry table - was keeper init not run? (make dev-provision + keeper)"
fi
psql_cli <<SQL
INSERT INTO service_registry (name, git, ref) VALUES
    ('${SERVICE_NAME}', '${REPO_URL}', 'v1.0.0')
ON CONFLICT (name) DO NOTHING;
SQL
log "registry (row for ${SERVICE_NAME}):"
psql_cli -c "SELECT name, git, ref FROM service_registry WHERE name='${SERVICE_NAME}'" | sed 's/^/    /'
# A live keeper holds the registry as an in-memory snapshot (serviceregistry.Holder,
# DefaultRefreshInterval=10s) - a direct psql write does NOT wake it up synchronously. Wait
# until Resolve sees the service: GET .../refs goes through holder.Resolve -> 200, once
# the snapshot is refreshed (otherwise creating the incarnation would fail 422 service-not-registered).
log "waiting for holder to refresh the snapshot (GET /v1/services/${SERVICE_NAME}/refs -> 200, up to 20s)"
code=""
for i in $(seq 1 20); do
    code="$(api_code GET "/v1/services/${SERVICE_NAME}/refs")"
    [ "${code}" = "200" ] && { log "holder saw ${SERVICE_NAME} after ~${i}s"; break; }
    sleep 1
done
[ "${code}" = "200" ] || fail "holder did not see ${SERVICE_NAME} within 20s (code=${code})"

# ── 5. Bare-creation of the incarnation pinned at v1.0.0 ────────────────────────────────
step "5. Bare incarnation ${INC_NAME} (service=${SERVICE_NAME})"
CREATE_RESP="$(api POST /v1/incarnations "{\"name\":\"${INC_NAME}\",\"service\":\"${SERVICE_NAME}\"}")"
printf '%s\n' "${CREATE_RESP}" | pretty
# Bare: without a create scenario -> synchronously ready, no apply_id.
GET_RESP="$(api GET "/v1/incarnations/${INC_NAME}")"
log "GET /v1/incarnations/${INC_NAME}:"
printf '%s\n' "${GET_RESP}" | pretty
echo "${GET_RESP}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
st  = d.get("status"); sv = d.get("service_version"); ss = d.get("state_schema_version")
print(f"\n[assert] status={st} service_version={sv} state_schema_version={ss}")
assert st == "ready",        f"expected status=ready, got {st}"
assert sv == "v1.0.0",       f"expected service_version=v1.0.0, got {sv}"
assert ss == 1,              f"expected state_schema_version=1, got {ss}"
print("[assert] OK: bare incarnation ready pinned at v1.0.0, schema=1")
' || fail "assert on the created incarnation failed"

# ── 6. upgrade-paths / upgrade cases ──────────────────────────────────────────
step "6a. CHEAP: GET upgrade-paths (without ?to=) - list of tags + is_current"
CHEAP="$(api GET "/v1/incarnations/${INC_NAME}/upgrade-paths")"
printf '%s\n' "${CHEAP}" | pretty

step "6b. FOUND: GET upgrade-paths?to=v2.0.0 - mode=found, slug=to_v2, migrations [1->2]"
FOUND="$(api GET "/v1/incarnations/${INC_NAME}/upgrade-paths?to=v2.0.0")"
printf '%s\n' "${FOUND}" | pretty

step "6c. LEGACY: GET upgrade-paths?to=v2.0.1 - mode=legacy, reachable, migrations [1->2]"
LEGACY="$(api GET "/v1/incarnations/${INC_NAME}/upgrade-paths?to=v2.0.1")"
printf '%s\n' "${LEGACY}" | pretty

step "6d. LIVE LEGACY UPGRADE: POST upgrade {to_version: v2.0.1} -> 202, then drift"
UP="$(api POST "/v1/incarnations/${INC_NAME}/upgrade" '{"to_version":"v2.0.1"}')"
printf '%s\n' "${UP}" | pretty
# Legacy -> pin change + migration + drift in one tx; give keeper a moment to settle.
for _ in $(seq 1 10); do
    POST_GET="$(api GET "/v1/incarnations/${INC_NAME}")"
    st="$(echo "${POST_GET}" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
    [ "${st}" = "applying" ] || break
    sleep 1
done
log "GET /v1/incarnations/${INC_NAME} after upgrade:"
printf '%s\n' "${POST_GET}" | pretty
echo "${POST_GET}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
st = d.get("status"); sv = d.get("service_version"); ss = d.get("state_schema_version")
print(f"\n[assert] status={st} service_version={sv} state_schema_version={ss}")
assert sv == "v2.0.1", f"expected service_version=v2.0.1, got {sv}"
assert ss == 2,        f"expected state_schema_version=2, got {ss}"
assert st == "drift",  f"expected status=drift (legacy-upgrade without hosts), got {st}"
print("[assert] OK: legacy-upgrade moved pin v1.0.0->v2.0.1, migrated schema 1->2, status=drift")
' || fail "assert after upgrade failed"

# ── 7. Summary + how to repeat + cleanup ─────────────────────────────────────────
step "7. Done"
cat <<EOF

Incarnation ${INC_NAME} went through: v1.0.0 (ready) -> v2.0.1 (drift, schema 2).

Cases proven by this run:
  6a cheap   - tags v1.0.0/v2.0.0/v2.0.1 + is_current=v1.0.0
  6b found   - ?to=v2.0.0: mode=found, slug=to_v2, reachable, state_migrations [1->2]
  6c legacy  - ?to=v2.0.1: mode=legacy, reachable, state_migrations [1->2]
  6d upgrade - live legacy POST upgrade -> 202 -> drift (pin v2.0.1, schema 2)

Repeat manually (JWT in the variable below):
  TOKEN=\$(VAULT_TOKEN=root bash ${REPO_ROOT}/dev/mint-jwt.sh)
  curl -s -H "Authorization: Bearer \$TOKEN" ${API}/v1/incarnations/${INC_NAME}/upgrade-paths | python3 -m json.tool
  curl -s -H "Authorization: Bearer \$TOKEN" "${API}/v1/incarnations/${INC_NAME}/upgrade-paths?to=v2.0.0" | python3 -m json.tool

Clean up this incarnation (force-destroy without hosts):
  curl -s -X DELETE -H "Authorization: Bearer \$TOKEN" "${API}/v1/incarnations/${INC_NAME}?allow_destroy=true"

Service ${SERVICE_NAME} and its repo remain in the registry for repeated runs.
EOF
