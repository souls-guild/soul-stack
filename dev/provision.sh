#!/usr/bin/env bash
#
# dev/provision.sh - idempotent local-dev provisioning for Soul Stack.
#
# Runs after `make dev-up`. Populates Vault KV/PKI, creates
# self-signed TLS material for the Keeper and materializes git repositories
# for service/destiny artifacts from examples/ - so the prod resolver
# (artifact.ServiceLoader / DestinyLoader, ADR-007/ADR-009) can clone
# them via a file://-URL from keeper.dev.yml. Re-running is safe: each step
# checks its own state before write/enable/commit and prints "[skip] ..." if
# it's already done.
#
# Does not require the `vault` CLI to be installed on the host: if it's absent,
# commands are proxied through `docker exec soul-stack-vault vault ...`.
#
# Parameters via env:
#   DEV_STAND      - stand identifier (empty=default); derived variables (STAND_DEV_DIR /
#                    PG_DB / VAULT_KV_PREFIX / STACK_PREFIX / ports) - dev/stand-env.sh (NIM-25)
#   VAULT_TOKEN    - forced to root (dev); VAULT_ADDR/PG_PORT - from stand-env
#   PG_DSN         - DSN for ${VAULT_KV_PREFIX}/postgres (default derived from the stand: DB ${PG_DB})
#   PKI_ROLE_DOMAINS - allowed_domains for the soul-seed role
#                    (default example.com,test,localhost,host.docker.internal,soul-docker-*)
#   DEV_KEEPER_EXTRA_IP - opt. extra IP in ip_sans of the keeper cert (WSL2 host-IP for
#                    docker souls, NIM-26); empty → only 127.0.0.1
#   REPO_ROOT      - root of the soul-stack repository (source of examples/);
#                    defaults to being derived from this script's path

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Stand profile: STAND_DEV_DIR / PG_DB / VAULT_KV_PREFIX / STACK_PREFIX / PG_PORT / ... (NIM-25).
source "${SCRIPT_DIR}/stand-env.sh"

# Explicit dev VAULT_TOKEN=root: the user's env sometimes has a prod token - force root.
# VAULT_ADDR - from stand-env (lightweight mode = shared :8200).
VAULT_TOKEN=root
KEEPER_DEV_DIR="${STAND_DEV_DIR}"
PG_DSN="${PG_DSN:-postgres://keeper:keeper@localhost:${PG_PORT}/${PG_DB}?sslmode=disable}"
# host.docker.internal - keeper-cert SAN; soul-docker-* - glob for bare-CN docker souls (NIM-26).
PKI_ROLE_DOMAINS="${PKI_ROLE_DOMAINS:-example.com,test,localhost,host.docker.internal,soul-docker-*}"
# Opt. host IP in ip_sans of the keeper cert (WSL2: keeper endpoint = host IP, NIM-26).
DEV_KEEPER_EXTRA_IP="${DEV_KEEPER_EXTRA_IP:-}"
# REPO_ROOT - repo root: the directory one level above dev/ (where this script lives).
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

export VAULT_ADDR VAULT_TOKEN

log_stand() { printf '[provision] stand: slug=%s slot=%s dir=%s pg_db=%s kv=%s stack=%s\n' "${STAND_SLUG:-<default>}" "${STAND_SLOT}" "${STAND_DEV_DIR}" "${PG_DB}" "${VAULT_KV_PREFIX}" "${STACK_PREFIX}"; }

log() { printf '[provision] %s\n' "$*"; }
skip() { printf '[provision] [skip] %s\n' "$*"; }
fail() { printf '[provision] [fail] %s\n' "$*" >&2; exit 1; }

# vault_cli - wrapper around the `vault` CLI. On macOS dev machines vault is usually
# not installed, so we fall back to the CLI inside the vault server container.
# We use `docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=...`
# so the in-container CLI talks to that same container's dev listener.
if command -v vault >/dev/null 2>&1; then
    vault_cli() { vault "$@"; }
    VAULT_ENDPOINT_DESC="${VAULT_ADDR} (host vault CLI)"
    log "vault CLI: host ($(command -v vault))"
else
    if ! command -v docker >/dev/null 2>&1; then
        fail "neither 'vault' nor 'docker' CLI found in PATH"
    fi
    # Inside the container we talk to that same container's dev listener,
    # ignoring the host-level VAULT_ADDR (it may point at a prod Vault).
    vault_cli() {
        docker exec \
            -e VAULT_ADDR=http://127.0.0.1:8200 \
            -e VAULT_TOKEN="$VAULT_TOKEN" \
            "${STACK_PREFIX}-vault" vault "$@"
    }
    VAULT_ENDPOINT_DESC="http://127.0.0.1:8200 (via docker exec ${STACK_PREFIX}-vault)"
    log "vault CLI: docker exec ${STACK_PREFIX}-vault vault"
fi

log_stand

# Sanity: Vault is reachable and unsealed.
if ! vault_cli status >/dev/null 2>&1; then
    fail "vault not reachable at ${VAULT_ENDPOINT_DESC} (run 'make dev-up' first)"
fi
log "vault reachable at ${VAULT_ENDPOINT_DESC}"

# 1. KV: ${VAULT_KV_PREFIX}/postgres (field `dsn`). Prefix is per-stand (NIM-25).
if vault_cli kv get -field=dsn "${VAULT_KV_PREFIX}/postgres" >/dev/null 2>&1; then
    skip "${VAULT_KV_PREFIX}/postgres already set"
else
    log "writing ${VAULT_KV_PREFIX}/postgres"
    vault_cli kv put "${VAULT_KV_PREFIX}/postgres" dsn="${PG_DSN}" >/dev/null
fi

# 2. KV: ${VAULT_KV_PREFIX}/jwt-signing-key (field `signing_key`).
# signing_key - 32 bytes of random data in base64, generated once
# and pinned in Vault. On script re-run the existing key is NOT
# regenerated, otherwise all previously issued JWTs would become invalid.
if vault_cli kv get -field=signing_key "${VAULT_KV_PREFIX}/jwt-signing-key" >/dev/null 2>&1; then
    skip "${VAULT_KV_PREFIX}/jwt-signing-key already set"
else
    log "generating and writing ${VAULT_KV_PREFIX}/jwt-signing-key"
    SIGNING_KEY="$(openssl rand -base64 32)"
    vault_cli kv put "${VAULT_KV_PREFIX}/jwt-signing-key" signing_key="${SIGNING_KEY}" >/dev/null
fi

# 2b. KV: ${VAULT_KV_PREFIX}/sigil-signing-key (field `signing_key`, ed25519 PEM PKCS#8).
# Required: with an empty sigil_signing_keys registry keeper fails at startup without it
# (fallback to cfg.signing_key_ref, ADR-026(h)). Generated once, NOT
# regenerated on re-run (otherwise already-issued Sigil grants would break).
if vault_cli kv get -field=signing_key "${VAULT_KV_PREFIX}/sigil-signing-key" >/dev/null 2>&1; then
    skip "${VAULT_KV_PREFIX}/sigil-signing-key already set"
else
    log "generating and writing ${VAULT_KV_PREFIX}/sigil-signing-key (ed25519 PEM PKCS#8)"
    SIGIL_KEY="$(openssl genpkey -algorithm ed25519 2>/dev/null)"
    [ -n "${SIGIL_KEY}" ] || fail "openssl genpkey ed25519 produced no key (requires openssl >=1.1.1)"
    vault_cli kv put "${VAULT_KV_PREFIX}/sigil-signing-key" signing_key="${SIGIL_KEY}" >/dev/null
fi

# 3. PKI secrets engine at path `pki/`.
# We parse `vault secrets list -format=json` without jq - grep on the path key.
if vault_cli secrets list -format=json 2>/dev/null | grep -q '"pki/"'; then
    skip "pki/ secrets engine already enabled"
else
    log "enabling pki/ secrets engine"
    vault_cli secrets enable -path=pki pki >/dev/null
    vault_cli secrets tune -max-lease-ttl=87600h pki >/dev/null
fi

# 4. PKI root certificate.
# `vault read pki/cert/ca` returns 0 only if the root has already been generated.
if vault_cli read pki/cert/ca >/dev/null 2>&1; then
    skip "pki root certificate already generated"
else
    log "generating pki root certificate (CN=soul-stack, ttl=87600h)"
    vault_cli write pki/root/generate/internal \
        common_name="soul-stack" ttl=87600h >/dev/null
fi

# 5. PKI role `soul-seed` (signs the keeper cert AND SoulSeed CSRs for souls). Idempotency
# by CONTENT: we rewrite until allowed_domains includes soul-docker (otherwise
# the old role without the glob would remain and docker-soul CSRs would fail with 400).
# allow_bare_domains - exact host.docker.internal; allow_glob_domains - docker-CN
# soul-docker-N (bare names outside domains) matched by the glob soul-docker-*;
# the host fleet *.example.com is unaffected. NIM-26.
if vault_cli read -field=allowed_domains pki/roles/soul-seed 2>/dev/null | grep -q 'soul-docker'; then
    skip "pki role soul-seed already allows soul-docker-* (glob)"
else
    log "writing pki role soul-seed (allowed_domains=${PKI_ROLE_DOMAINS})"
    vault_cli write pki/roles/soul-seed \
        allowed_domains="${PKI_ROLE_DOMAINS}" \
        allow_subdomains=true \
        allow_bare_domains=true \
        allow_glob_domains=true \
        allow_localhost=true \
        max_ttl=720h >/dev/null
fi

# 6. Keeper dev directories.
# tls/ - Vault-issued cert + Vault-root CA for the bootstrap+event_stream listener (see step 7 + keeper.dev.yml).
# plugins/ - cache of downloaded plugins (plugins.cache_root).
# plugin-sockets/ - unix sockets for the per-plugin process (plugin_runtime.socket_dir).
mkdir -p "${KEEPER_DEV_DIR}/tls" \
         "${KEEPER_DEV_DIR}/plugins" \
         "${KEEPER_DEV_DIR}/plugin-sockets"
log "ensured ${KEEPER_DEV_DIR}/{tls,plugins,plugin-sockets}"

# 7. TLS material for the Keeper listeners - issued from Vault PKI.
#
# The Keeper server cert MUST chain to the same root (CN=soul-stack)
# as the SoulSeed certificates: otherwise on EventStream (mTLS) Soul doesn't trust
# the Keeper's server cert (after bootstrap Soul only trusts the Vault root
# from seed/ca.pem), and Keeper doesn't trust Soul's client cert. A self-signed
# cert worked only for the Bootstrap phase (there Soul takes the CA from config),
# but broke EventStream - hence a Vault-issued leaf + Vault root as the
# trust-anchor/ClientCAs here.
#
#   keeper.crt    — leaf (CN=localhost, SAN DNS:localhost,IP:127.0.0.1).
#   keeper.key    - private key of the leaf.
#   vault-ca.crt  - Vault PKI root (CN=soul-stack); in keeper.dev.yml this is
#                   event_stream.tls.ca (ClientCAs), in soul.dev.yml -
#                   keeper.tls.ca (verification of the server cert on bootstrap).
CRT="${KEEPER_DEV_DIR}/tls/keeper.crt"
KEY="${KEEPER_DEV_DIR}/tls/keeper.key"
VAULT_CA="${KEEPER_DEV_DIR}/tls/vault-ca.crt"

# issue_keeper_cert - issue a leaf from Vault PKI and lay out crt/key/ca into files.
# SAN includes host.docker.internal (docker souls of the dev fleet, NIM-26) + opt.
# DEV_KEEPER_EXTRA_IP (WSL2 host IP). localhost/127.0.0.1 are kept (host fleet).
issue_keeper_cert() {
    log "issuing keeper server cert from Vault PKI (CN=localhost, SAN=DNS:localhost,host.docker.internal,IP:127.0.0.1${DEV_KEEPER_EXTRA_IP:+,${DEV_KEEPER_EXTRA_IP}}, ttl=720h)"
    local issue_json
    issue_json="$(vault_cli write -format=json pki/issue/soul-seed \
        common_name=localhost \
        ip_sans="127.0.0.1${DEV_KEEPER_EXTRA_IP:+,${DEV_KEEPER_EXTRA_IP}}" \
        alt_names=localhost,host.docker.internal \
        ttl=720h)"
    printf '%s' "${issue_json}" | python3 -c "
import sys, json
d = json.load(sys.stdin)['data']
open('${CRT}', 'w').write(d['certificate'] + '\n')
open('${KEY}', 'w').write(d['private_key'] + '\n')
open('${VAULT_CA}', 'w').write(d['issuing_ca'] + '\n')
"
    chmod 0600 "${KEY}"
    log "wrote keeper.crt + keeper.key (Vault-issued) + vault-ca.crt (root CN=soul-stack)"
}

# tls_material_current - true if crt/key/ca are present AND the certs still chain
# to the CURRENT Vault PKI root. Reset-aware: after `make dev-reset` the Vault root
# is recreated (new serial), while the old keeper.crt/vault-ca.crt remain on
# disk - a plain `[ -s ... ]` would then wrongly skip re-issuance, breaking mTLS
# onboarding for a new Soul (Keeper's ClientCAs would trust the old root). We check:
#   (1) the saved vault-ca.crt matches the live `vault read pki/cert/ca`;
#   (2) keeper.crt verifies against the saved CA (catches leaf rotation).
tls_material_current() {
    [ -s "${CRT}" ] && [ -s "${KEY}" ] && [ -s "${VAULT_CA}" ] || return 1

    local live_ca
    live_ca="$(vault_cli read -field=certificate pki/cert/ca 2>/dev/null)" || return 1
    [ -n "${live_ca}" ] || return 1

    # Normalize the PEM of both certs through openssl and compare the DER hash:
    # robust against trailing-newline/line-ending differences between Vault and the file.
    local saved_fp live_fp
    saved_fp="$(openssl x509 -in "${VAULT_CA}" -outform DER 2>/dev/null | openssl dgst -sha256)" || return 1
    live_fp="$(printf '%s\n' "${live_ca}" | openssl x509 -outform DER 2>/dev/null | openssl dgst -sha256)" || return 1
    [ "${saved_fp}" = "${live_fp}" ] || return 1

    # keeper.crt must chain to the saved (== live) root.
    openssl verify -CAfile "${VAULT_CA}" "${CRT}" >/dev/null 2>&1 || return 1

    # SAN must include host.docker.internal (docker souls, NIM-26) + opt.
    # DEV_KEEPER_EXTRA_IP - otherwise re-issue, so a docker soul doesn't hit a SAN mismatch.
    local san
    san="$(openssl x509 -in "${CRT}" -noout -ext subjectAltName 2>/dev/null || true)"
    printf '%s' "${san}" | grep -q 'host.docker.internal' || return 1
    if [ -n "${DEV_KEEPER_EXTRA_IP}" ]; then
        printf '%s' "${san}" | grep -q "${DEV_KEEPER_EXTRA_IP}" || return 1
    fi
    return 0
}

if tls_material_current; then
    skip "keeper TLS material present and chains to current Vault root (${CRT}, ${KEY}, ${VAULT_CA})"
else
    if [ -s "${CRT}" ] || [ -s "${VAULT_CA}" ]; then
        log "keeper TLS material stale or missing (Vault root rotated after dev-reset?) — re-issuing"
    fi
    issue_keeper_cert
fi

# 8. Sanity: Postgres reachable. Applying migrations is done by `keeper init`/`keeper run`
# itself (idempotently, via migrate.Apply in the stand's DB ${PG_DB}), so a separate
# schema-bootstrap in provision.sh isn't needed - that would be a duplicate.
#
# Two wrappers (symmetric to vault_cli): psql_admin - the always-existing bootstrap DB
# `keeper` (reachability + CREATE DATABASE for the stand); psql_stand - the stand's DB
# ${PG_DB} (seed). For default both hit `keeper` (identical to the old psql_cli). NIM-25.
PG_ADMIN_DSN="postgres://keeper:keeper@localhost:${PG_PORT}/keeper?sslmode=disable"
PG_REACHABLE=0
if command -v psql >/dev/null 2>&1; then
    psql_admin() { psql "${PG_ADMIN_DSN}" -v ON_ERROR_STOP=1 -q "$@"; }
    psql_stand() { psql "${PG_DSN}" -v ON_ERROR_STOP=1 -q "$@"; }
    if psql "${PG_ADMIN_DSN}" -c 'SELECT 1' >/dev/null 2>&1; then
        PG_REACHABLE=1
        log "postgres reachable via host psql"
    else
        log "postgres NOT reachable via host psql (keeper init will retry)"
    fi
else
    psql_admin() { docker exec -i "${STACK_PREFIX}-postgres" psql -U keeper -d keeper -v ON_ERROR_STOP=1 -q "$@"; }
    psql_stand() { docker exec -i "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -v ON_ERROR_STOP=1 -q "$@"; }
    if docker exec "${STACK_PREFIX}-postgres" pg_isready -U keeper -d keeper >/dev/null 2>&1; then
        PG_REACHABLE=1
        log "postgres reachable via docker exec pg_isready"
    else
        log "postgres NOT ready yet (keeper init will retry)"
    fi
fi

# 8b. Stand DB ${PG_DB} - create idempotently (CREATE DATABASE without IF NOT EXISTS).
# Lightweight isolation: shared Postgres, separate DB per stand. Default (keeper) is
# created by docker-compose - we skip it. Created BEFORE keeper init/run (which migrates
# ${PG_DB}). NIM-25.
ensure_stand_db() {
    if [ "${PG_DB}" = "keeper" ]; then
        skip "DB keeper (default) - created by docker-compose"
        return 0
    fi
    if [ "${PG_REACHABLE}" != "1" ]; then
        skip "DB ${PG_DB}: postgres unreachable - creation deferred (re-run provision)"
        return 0
    fi
    if [ "$(psql_admin -tAc "SELECT 1 FROM pg_database WHERE datname='${PG_DB}'" 2>/dev/null)" = "1" ]; then
        skip "DB ${PG_DB} already exists"
    else
        log "creating DB ${PG_DB} (owner keeper)"
        psql_admin -c "CREATE DATABASE \"${PG_DB}\" OWNER keeper" >/dev/null
    fi
}
ensure_stand_db

# 9. Git repositories for service/destiny artifacts from examples/.
#
# The Keeper's prod resolver (artifact.ServiceLoader / DestinyLoader, ADR-007/ADR-009)
# clones service and destiny repos by git URL+ref. The resolve coordinates live in
# the service registry in Postgres (service_registry + keeper_settings, ADR-029) -
# seeded by step 10 below:
#   - service repo   - from service_registry entries (git/ref);
#   - destiny repo   - from keeper_settings[default_destiny_source] with {name}
#                      substitution, ref from service.yml::destiny[] (v1.0.0 for redis).
# Nobody creates the repositories themselves automatically - this step materializes them
# from examples/ as local git repos under file://-URLs, pointed to by the
# seeded registry.
#
# file:// repos require SOUL_STACK_ALLOW_FILE_REPOS=1 on the keeper run side
# (see docs/dev/local-setup.md) - provision only creates the repo and seeds the registry
# (step 10), the flag belongs to keeper.

# Fixed author/committer for a deterministic commit SHA: identical
# examples/ content → identical SHA → keeper reuses the snapshot instead of
# spawning orphans in the cache on every provision (see snapshot cache by SHA).
export GIT_AUTHOR_NAME="soul-stack-dev"
export GIT_AUTHOR_EMAIL="dev@soul-stack.local"
export GIT_COMMITTER_NAME="soul-stack-dev"
export GIT_COMMITTER_EMAIL="dev@soul-stack.local"
export GIT_AUTHOR_DATE="2020-01-01T00:00:00Z"
export GIT_COMMITTER_DATE="2020-01-01T00:00:00Z"

# provision_git_repo SRC DEST REF KIND
#   SRC  - source directory in examples/ (content copied without .git);
#   DEST - target git repo directory (under KEEPER_DEV_DIR);
#   REF  - git ref the artifact should point to (branch `main` or a tag
#          like `v1.0.0`; a tag is recognized by the `v` + digit prefix);
#   KIND - label for the log ("service"/"destiny").
# Idempotency: the repo is recreated from scratch (rm -rf DEST) every time, but
# the deterministic commit guarantees the same SHA for unchanged content.
provision_git_repo() {
    local src="$1" dest="$2" ref="$3" kind="$4"
    if [ ! -d "${src}" ]; then
        fail "${kind} source not found: ${src}"
    fi

    # tag ref (v1.0.0, ...) goes on branch main + a tag; branch ref - just the branch.
    local is_tag=0
    case "${ref}" in
        v[0-9]*) is_tag=1 ;;
    esac

    # Rebuild from scratch: cheap for small examples/, avoids a stale tree.
    rm -rf "${dest}"
    mkdir -p "${dest}"
    # Copy src's content WITHOUT the root directory and without .git (there isn't one in src).
    cp -R "${src}/." "${dest}/"

    git -C "${dest}" init -q -b main
    git -C "${dest}" add -A
    # -c *.gpgsign=false: drop the operator's signature (no ssh-askpass in WSL, dev artifacts don't need signing).
    git -C "${dest}" -c commit.gpgsign=false commit -q -m "${kind} snapshot from examples/ (dev-provision)"
    if [ "${is_tag}" = "1" ]; then
        git -C "${dest}" -c tag.gpgsign=false tag -f "${ref}" >/dev/null
        log "git repo ${kind} @ ${dest} (branch main + tag ${ref})"
    else
        log "git repo ${kind} @ ${dest} (branch ${ref})"
    fi
}

if ! command -v git >/dev/null 2>&1; then
    fail "git CLI not found in PATH - needed to materialize service/destiny repos"
fi
# go is needed to build the community.redis plugin (step 9b) - plugingit F-fetch expects
# a BUILT binary in dist/, Keeper does not compile (ADR-026).
if ! command -v go >/dev/null 2>&1; then
    fail "go CLI not found in PATH - needed to build the community.redis plugin"
fi

EXAMPLES="${REPO_ROOT}/examples"
# plugin-repos/ - git repo of built plugins (source for plugins.soul_modules);
# plugin-work/ - writable work_root for the resolver (plugins.work_root, default is not writable).
mkdir -p "${KEEPER_DEV_DIR}/repos" "${KEEPER_DEV_DIR}/destiny" \
         "${KEEPER_DEV_DIR}/plugin-repos" "${KEEPER_DEV_DIR}/plugin-work"

# service repos (service_registry entries, see step 10; ref: main).
provision_git_repo \
    "${EXAMPLES}/service/hello-world" \
    "${KEEPER_DEV_DIR}/repos/hello-world" \
    main "service hello-world"
provision_git_repo \
    "${EXAMPLES}/service/redis" \
    "${KEEPER_DEV_DIR}/repos/redis" \
    main "service redis"

# destiny repos (keeper_settings[default_destiny_source]=file://.../destiny/{name},
# see step 10; ref: v1.0.0 - from redis/service.yml::destiny[]). The directory name
# = {name} from destiny[], and the examples directory is now also a bare {name}.
provision_git_repo \
    "${EXAMPLES}/destiny/redis" \
    "${KEEPER_DEV_DIR}/destiny/redis" \
    v1.0.0 "destiny redis"
provision_git_repo \
    "${EXAMPLES}/destiny/redis-exporter" \
    "${KEEPER_DEV_DIR}/destiny/redis-exporter" \
    v1.0.0 "destiny redis-exporter"
# node-exporter (examples/destiny/node-exporter/, binary wb_node_exporter,
# version-aware install, textfile collectors). Resolved uniformly via
# default_destiny_source ({name}=node-exporter), no per-entry git override.
provision_git_repo \
    "${EXAMPLES}/destiny/node-exporter" \
    "${KEEPER_DEV_DIR}/destiny/node-exporter" \
    v1.0.0 "destiny node-exporter"
# vector (log pipeline, Slice I of redis monitoring) - declared in redis/service.yml::destiny[].
provision_git_repo \
    "${EXAMPLES}/destiny/vector" \
    "${KEEPER_DEV_DIR}/destiny/vector" \
    v1.0.0 "destiny vector"

# 9b. community.redis plugin (SoulModule) - materializing the BUILT binary into a git repo.
#
# Unlike service/destiny (provision_git_repo commits SOURCES), the plugingit
# resolver (ADR-026 F-fetch) on Keeper does NOT compile - it expects a ready binary in
# dist/<binary-name> next to manifest.yaml. The layout mirrors the harness
# tests/e2e-live/harness/plugin.go (parity with plugingit/resolver_test.go fixtureRepo).
#
# In-place build (cwd=source directory): the plugin's go.mod uses relative-replace
# (../../../sdk, ../../../proto/plugin) - the tree cannot be copied. GOWORK=off - the plugin
# is outside go.work (precedent from Makefile test-plugins). -trimpath -ldflags "-buildid=" -
# a reproducible sha256: otherwise a repeat provision changes the binary → invalidates an
# already-issued Sigil grant.
provision_community_redis_plugin() {
    local src="${EXAMPLES}/module/soul-mod-community-redis"
    local dest="${KEEPER_DEV_DIR}/plugin-repos/community-redis"
    local bin="soul-mod-redis"
    if [ ! -f "${src}/manifest.yaml" ]; then
        fail "community.redis plugin manifest not found: ${src}/manifest.yaml"
    fi

    log "building community.redis plugin (${bin}, linux/amd64, reproducible)"
    local tmp
    tmp="$(mktemp -d)"
    if ! ( cd "${src}" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -trimpath -ldflags "-buildid=" -o "${tmp}/${bin}" . ); then
        rm -rf "${tmp}"
        fail "go build of the community.redis plugin failed (${src})"
    fi

    # Rebuild from scratch: deterministic commit (GIT_* above) → same SHA for an
    # unchanged binary, keeper reuses the snapshot instead of spawning orphans in the cache.
    rm -rf "${dest}"
    mkdir -p "${dest}/dist"
    cp "${src}/manifest.yaml" "${dest}/manifest.yaml"
    cp "${tmp}/${bin}" "${dest}/dist/${bin}"
    chmod 0755 "${dest}/dist/${bin}"
    rm -rf "${tmp}"

    git -C "${dest}" init -q -b main
    git -C "${dest}" add -A
    git -C "${dest}" -c commit.gpgsign=false commit -q -m "community.redis plugin snapshot (dev-provision)"
    git -C "${dest}" -c tag.gpgsign=false tag -f v1.0.0 >/dev/null
    log "community.redis plugin git repo @ ${dest} (branch main + tag v1.0.0, dist/${bin})"
}
provision_community_redis_plugin

# 10. Seed the service registry in Postgres (service_registry + keeper_settings).
#
# Before ADR-029 these coordinates lived in keeper.dev.yml::services[] /
# default_destiny_source; the S4 hard-cut removed them from config - now the resolver
# (serviceregistry.Holder.Resolve / DefaultDestinySource) reads only the DB.
# Without the seed, E2E-smoke would come up with an empty registry and
# Resolve("hello-world"/"redis") would return false. We seed the same entries that
# used to be in services[]:
#   - service hello-world → file://${KEEPER_DEV_DIR}/repos/hello-world @ main
#   - service redis       → file://${KEEPER_DEV_DIR}/repos/redis @ main
#   - keeper_settings[default_destiny_source] = file://${KEEPER_DEV_DIR}/destiny/{name}
#
# Method - direct psql INSERT (provision has PG access; an Archon/JWT for the
# service.* API isn't issued at this step yet). Idempotent: ON CONFLICT DO
# NOTHING (a repeat provision doesn't touch entries already seeded/edited by an
# operator). created_by_aid/updated_by_aid = NULL - seed with no initiating Archon
# (the schema allows this, FK ON DELETE SET NULL).
#
# Order in make dev-smoke: provision runs BEFORE `keeper init`, which is what
# creates the schema (migrate.Apply). On a fresh DB (dev-reset) the tables don't
# exist yet - then seed logs [skip] and provisioning stays green; the registry gets
# seeded on the next `make dev-provision` after `keeper init` (provision is
# idempotent, see the header). If the schema is already in place (the DB survived or
# init already ran) - we seed right away.
seed_service_registry() {
    if [ "${PG_REACHABLE}" != "1" ]; then
        skip "service registry: postgres unreachable - seed skipped (retry provision after keeper init)"
        return 0
    fi
    # The schema is created by keeper init/run (migrate.Apply) in the stand's DB. Seed is impossible before that.
    if ! psql_stand -tAc "SELECT to_regclass('public.service_registry') IS NOT NULL AND to_regclass('public.keeper_settings') IS NOT NULL" 2>/dev/null | grep -qx t; then
        skip "service registry: schema not applied yet (no service_registry/keeper_settings) - seed deferred until a run after keeper init; retry 'make dev-provision'"
        return 0
    fi

    log "seeding service_registry (hello-world, redis) + keeper_settings[default_destiny_source]"
    # Unquoted heredoc: only ${KEEPER_DEV_DIR} gets substituted; {name} (without $) remains
    # a keeper placeholder. There are no other $-literals in the SQL.
    psql_stand -f - <<SQL
INSERT INTO service_registry (name, git, ref) VALUES
    ('hello-world', 'file://${KEEPER_DEV_DIR}/repos/hello-world', 'main'),
    ('redis',       'file://${KEEPER_DEV_DIR}/repos/redis',       'main')
ON CONFLICT (name) DO NOTHING;

INSERT INTO keeper_settings (key, value) VALUES
    ('default_destiny_source', 'file://${KEEPER_DEV_DIR}/destiny/{name}')
ON CONFLICT (key) DO NOTHING;
SQL
    log "service registry seeded (hello-world, redis, default_destiny_source)"
}

seed_service_registry

log "done"
