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

# Postgres wrappers, symmetric to vault_cli. Defined here rather than next to the
# reachability check in step 8 because step 1b has to ask Postgres what it remembers
# BEFORE step 2 generates anything: the whole point of that guard is to run before the
# first write. psql_admin - the always-existing bootstrap DB `keeper` (reachability +
# CREATE DATABASE for the stand); psql_stand - the stand's DB ${PG_DB} (seed); psql_db -
# an arbitrary DB by name, which neither of the other two can express and step 1b needs
# because one Vault serves every stand on this host. For default all three hit `keeper`.
# NIM-25.
PG_ADMIN_DSN="postgres://keeper:keeper@localhost:${PG_PORT}/keeper?sslmode=disable"
PG_REACHABLE=0
if command -v psql >/dev/null 2>&1; then
    psql_admin() { psql "${PG_ADMIN_DSN}" -v ON_ERROR_STOP=1 -q "$@"; }
    psql_stand() { psql "${PG_DSN}" -v ON_ERROR_STOP=1 -q "$@"; }
    psql_db() { local db="$1"; shift; psql "postgres://keeper:keeper@localhost:${PG_PORT}/${db}?sslmode=disable" -v ON_ERROR_STOP=1 -q "$@"; }
    if psql "${PG_ADMIN_DSN}" -c 'SELECT 1' >/dev/null 2>&1; then
        PG_REACHABLE=1
        log "postgres reachable via host psql"
    fi
else
    psql_admin() { docker exec -i "${STACK_PREFIX}-postgres" psql -U keeper -d keeper -v ON_ERROR_STOP=1 -q "$@"; }
    psql_stand() { docker exec -i "${STACK_PREFIX}-postgres" psql -U keeper -d "${PG_DB}" -v ON_ERROR_STOP=1 -q "$@"; }
    psql_db() { local db="$1"; shift; docker exec -i "${STACK_PREFIX}-postgres" psql -U keeper -d "${db}" -v ON_ERROR_STOP=1 -q "$@"; }
    if docker exec "${STACK_PREFIX}-postgres" pg_isready -U keeper -d keeper >/dev/null 2>&1; then
        PG_REACHABLE=1
        log "postgres reachable via docker exec pg_isready"
    fi
fi

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

# 1b. Trust-anchor consistency: what Vault still holds vs what Postgres still remembers.
#
# The dev Vault runs in dev-mode with in-memory storage, Postgres runs on a volume. That
# asymmetry is the bug (NIM-365): a container restart - machine reboot, `docker restart`,
# a crashed daemon - empties Vault while every registry survives. Each step below is
# idempotent "by its own state", so against an empty Vault not one of them skips, and
# provision used to quietly mint NEW anchors under live databases. Nothing errored: the
# operator's token simply started answering 401 and souls stopped establishing mTLS, with
# no line anywhere naming the cause. Neither loss can be repaired in place: a token has to
# be re-minted, and a soul whose seed no longer chains to the root has to be re-onboarded -
# NIM-365 records 16 hosts lost that way on a single restart of the default stand.
#
# So before anything is generated: refuse when Vault has lost an anchor that a live
# database still depends on, and name what proceeding would destroy. Softening this to a
# warning would leave the stand in exactly the state the ticket describes, only with a
# line nobody reads at the moment of failure.
#
# The PKI root is checked against EVERY keeper database on this Postgres, not just this
# stand's. In lightweight mode the stands share one Vault and therefore one `pki/`, so
# provisioning a brand-new stand after a restart would regenerate the root out from under
# the souls of every other stand on the host - the same silent break, one stand removed
# from whoever triggered it.
DEV_VAULT_REISSUE_ANCHORS="${DEV_VAULT_REISSUE_ANCHORS:-0}"

# db_row_count <db> <table> - rows in <table> of <db>; 0 when the database or the table is
# absent. Two queries rather than one CASE over to_regclass: Postgres still parses the
# branch it will not take, so a missing relation is an error before it is a zero.
#
# A count that does not come back is 0 too, and both halves of that matter under
# `set -euo pipefail`. Unguarded, a failing psql pipeline makes the assignment at the
# call site non-zero and kills provision on the spot, with no line printed - the guard
# below exists precisely to replace silent deaths with named ones. And an empty result
# substituted into `[ "${n}" != "0" ]` reads as "not zero", which would refuse the
# provision over a registry nobody has shown to hold anything.
db_row_count() {
    local db="$1" table="$2" n
    if [ "$(psql_db "${db}" -tAc "SELECT to_regclass('public.${table}') IS NOT NULL" 2>/dev/null | tr -d '[:space:]')" != "t" ]; then
        printf '0'
        return 0
    fi
    n="$(psql_db "${db}" -tAc "SELECT count(*) FROM ${table}" 2>/dev/null | tr -d '[:space:]')" || n=""
    if [ -z "${n}" ]; then
        # >&2 because stdout is this function's return channel: `log` writes there, and a
        # warning captured into the count would read as a very large non-zero row count.
        log "[warn] could not count ${db}.${table} - treating it as empty; if it does hold rows, the trust-anchor check below is blind to them" >&2
        printf '0'
        return 0
    fi
    printf '%s' "${n}"
}

# keeper_databases - every stand DB on this Postgres: the default `keeper` plus the
# per-stand `keeper_<slug>` of NIM-25. One name per line; non-zero when the list could not
# be read at all.
#
# That second half is the point. The caller consults this only when the PKI root is gone,
# and it is the one loss that reaches other people's stands. A query that failed and a
# Postgres with nothing on it are the same empty string, so returning it either way would
# let "psql is broken" read as "no stand depends on the root" and wave through exactly the
# regeneration this guard exists to stop. `pg_isready` answers 0 for a server that responds
# at all, rejected connections included, so PG_REACHABLE=1 does not make the query safe.
# The connection itself uses `keeper`, so a query that worked always names at least that
# one - an empty result means it did not work.
keeper_databases() {
    local out
    out="$(psql_admin -tAc "SELECT datname FROM pg_database WHERE datname = 'keeper' OR datname LIKE 'keeper\\_%'" 2>/dev/null | tr -d '[:blank:]')" || out=""
    [ -n "${out}" ] || return 1
    printf '%s\n' "${out}"
}

check_vault_anchors_against_registries() {
    local jwt_gone=0 pki_gone=0 sigil_gone=0
    vault_cli kv get -field=signing_key "${VAULT_KV_PREFIX}/jwt-signing-key" >/dev/null 2>&1 || jwt_gone=1
    vault_cli read pki/cert/ca >/dev/null 2>&1 || pki_gone=1
    vault_cli kv get -field=signing_key "${VAULT_KV_PREFIX}/sigil-signing-key" >/dev/null 2>&1 || sigil_gone=1
    if [ "${jwt_gone}${pki_gone}${sigil_gone}" = "000" ]; then
        skip "vault trust anchors present (jwt-signing-key, pki root, sigil-signing-key)"
        return 0
    fi

    if [ "${PG_REACHABLE}" != "1" ]; then
        log "[warn] vault is missing a trust anchor and postgres is unreachable, so a first-ever stand cannot be told apart from a wiped Vault - generating anyway; if this stand already had Archons or souls, stop and re-run provision once postgres is up"
        return 0
    fi

    # foreign_loss - at least one of the losses belongs to a stand that is not this one.
    # It decides whether the surgical way out exists at all: `pki/` is shared, so nothing
    # done to THIS stand's database keeps the root from being regenerated under a
    # neighbour's souls. Offering the drop there would be advice that quietly costs
    # someone else their fleet.
    local losses=() foreign_loss=0 n db seeds dbs
    if [ "${jwt_gone}" = "1" ]; then
        n="$(db_row_count "${PG_DB}" operators)"
        if [ "${n}" != "0" ]; then
            losses+=("${VAULT_KV_PREFIX}/jwt-signing-key is gone while ${PG_DB}.operators still holds ${n} Archon(s) - every JWT ever issued to them stops verifying (401), ${STAND_DEV_DIR}/archon-alice.jwt included; the Archon keeps its rights in the registry, only the signature no longer matches")
        fi
    fi
    if [ "${sigil_gone}" = "1" ]; then
        n="$(db_row_count "${PG_DB}" plugin_sigils)"
        if [ "${n}" != "0" ]; then
            losses+=("${VAULT_KV_PREFIX}/sigil-signing-key is gone while ${PG_DB}.plugin_sigils still holds ${n} grant(s) - they were signed by an anchor that no longer exists, so no plugin they cover can be admitted (ADR-026)")
        fi
    fi
    if [ "${pki_gone}" = "1" ]; then
        # `if dbs=...` and not a bare assignment: the condition context suspends errexit, so
        # a failed listing is handled here instead of killing provision without a word.
        if dbs="$(keeper_databases)"; then
            for db in ${dbs}; do
                seeds="$(db_row_count "${db}" soul_seeds)"
                if [ "${seeds}" != "0" ]; then
                    if [ "${db}" = "${PG_DB}" ]; then
                        losses+=("the pki root is gone while ${db}.soul_seeds still holds ${seeds} seed(s) signed by it - those souls cannot establish mTLS and have to be re-onboarded")
                    else
                        foreign_loss=1
                        losses+=("the pki root is gone while ${db}.soul_seeds still holds ${seeds} seed(s) signed by it - that is ANOTHER stand, and one Vault serves them all, so regenerating the root here breaks its souls, not yours")
                    fi
                fi
            done
        else
            # Unknown, not empty. foreign_loss=1 with it: with the list unread, this stand's
            # database cannot be shown to be the only one at risk, and the surgical way out
            # is exactly the advice that must not be given on a guess.
            foreign_loss=1
            losses+=("the pki root is gone and the keeper database list could not be read (psql failed against ${STACK_PREFIX}-postgres), so whether any stand still holds seeds signed by that root is UNKNOWN - proceeding would regenerate it blind")
        fi
    fi

    if [ "${#losses[@]}" -eq 0 ]; then
        log "vault trust anchors missing, but no registry depends on them yet - generating"
        return 0
    fi

    if [ "${DEV_VAULT_REISSUE_ANCHORS}" = "1" ]; then
        log "[warn] DEV_VAULT_REISSUE_ANCHORS=1 - reissuing trust anchors and destroying:"
        for n in "${losses[@]}"; do
            log "[warn]   - ${n}"
        done
        return 0
    fi

    printf '[provision] [fail] vault lost a trust anchor that a live database still depends on:\n' >&2
    for n in "${losses[@]}"; do
        printf '[provision] [fail]   - %s\n' "${n}" >&2
    done
    printf '[provision] [fail] cause: the dev Vault keeps secrets in memory (dev/docker-compose.yml), so a\n' >&2
    printf '[provision] [fail]   container restart empties it while Postgres survives on its volume.\n' >&2
    printf '[provision] [fail] ways out, narrowest blast radius first:\n' >&2
    if [ "${foreign_loss}" = "1" ]; then
        printf '[provision] [fail]   1. (no surgical option here: the losses above either name another stand'"'"'s souls or\n' >&2
        printf '[provision] [fail]      leave it unknown whether they do, and one `pki/` serves every stand - regenerating\n' >&2
        printf '[provision] [fail]      the root breaks whatever is there, whatever you do to this stand'"'"'s database.\n' >&2
        printf '[provision] [fail]      Ask whoever runs it before taking 2 or 3.)\n' >&2
    elif [ "${PG_DB}" != "keeper" ]; then
        printf '[provision] [fail]   1. drop this stand'"'"'s database and re-provision - consistent again, no other stand touched.\n' >&2
        printf '[provision] [fail]      Stop this stand'"'"'s keeper first: Postgres refuses to drop a database that still\n' >&2
        printf '[provision] [fail]      has a session on it, and a restarted Vault leaves the keeper running - which is\n' >&2
        printf '[provision] [fail]      exactly the state you are in right now.\n' >&2
        printf '[provision] [fail]        docker exec -i %s-postgres psql -U keeper -d keeper -c '"'"'DROP DATABASE "%s"'"'"' && make dev-provision\n' "${STACK_PREFIX}" "${PG_DB}" >&2
    else
        printf '[provision] [fail]   1. (not available on the default stand: `keeper` is created once by the postgres\n' >&2
        printf '[provision] [fail]      container at first init and by nothing afterwards, so dropping it leaves a stand\n' >&2
        printf '[provision] [fail]      that cannot be provisioned back - run under DEV_STAND=<slug> for a droppable one)\n' >&2
    fi
    printf '[provision] [fail]   2. accept the loss listed above on purpose, keeping every database:\n' >&2
    printf '[provision] [fail]        DEV_VAULT_REISSUE_ANCHORS=1 make dev-provision\n' >&2
    printf '[provision] [fail]      then re-mint an operator token (AID=archon-alice dev/mint-jwt.sh) and re-onboard the souls.\n' >&2
    # dev-reset is `docker compose down -v`, and which volumes that reaches depends on the
    # mode: DEDICATED_INFRA gives the stand its own compose project, lightweight stands
    # share the default one. Saying "wipes everything" in the dedicated case would push an
    # operator away from the cheapest correct action and toward option 2.
    #
    # The flag alone does not make a stand dedicated. stand-env.sh suffixes STACK_PREFIX with
    # the slug only when there is one, so DEDICATED_INFRA=1 without DEV_STAND still lands on
    # the shared `soul-stack-*` containers - compose pins container_name to that prefix, so
    # the slug is the only thing separating one stand's containers from another's - and on
    # the shared `keeper` database with them. Calling that "wipes this stand and nothing
    # else" would recommend destroying every neighbour's data as the cheap option. Key off
    # the prefix, which is what dev-reset exports as the compose project.
    if [ "${DEDICATED_INFRA:-0}" = "1" ] && [ "${STACK_PREFIX}" != "soul-stack" ]; then
        printf '[provision] [fail]   3. make dev-reset - this stand runs its own docker project (DEDICATED_INFRA=1),\n' >&2
        printf '[provision] [fail]      so it wipes this stand and nothing else. Cheapest way back to a clean stand.\n' >&2
    else
        printf '[provision] [fail]   3. make dev-reset - WIPES THE SHARED POSTGRES VOLUME: every stand that shares it,\n' >&2
        printf '[provision] [fail]      not only this one (a stand with DEDICATED_INFRA=1 AND a slug has its own and\n' >&2
        printf '[provision] [fail]      survives; this one does not). Check who else is running before you do.\n' >&2
    fi
    fail "refusing to mint new trust anchors under a live database"
}

check_vault_anchors_against_registries

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
# The psql_* wrappers themselves are defined above, next to vault_cli - step 1b needs
# them before the first Vault write. This step only re-establishes reachability.
if [ "${PG_REACHABLE}" != "1" ]; then
    log "postgres NOT reachable (keeper init will retry)"
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
# go is needed to build the redis plugin (step 9b) - plugingit F-fetch expects
# a BUILT binary in dist/, Keeper does not compile (ADR-026).
if ! command -v go >/dev/null 2>&1; then
    fail "go CLI not found in PATH - needed to build the redis plugin"
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

# 9b. redis plugin (SoulModule) - materializing the STAMPED artifact into a git repo.
#
# Unlike service/destiny (provision_git_repo commits SOURCES), the plugingit resolver
# (ADR-026 F-fetch) on Keeper neither compiles nor executes: it takes the one executable
# in dist/ and reads the plugin's disclosure out of a TRAILER on that artifact. There is
# no manifest.yaml - NIM-377 deleted it and put the generated schema document in its
# place, and ADR-065(g) says the slot holds the artifact and nothing else. So this step
# publishes what a plugin author publishes: build, stamp the document into the binary,
# put the same bytes beside it as schema.json.
#
# Parity with tests/e2e-live/harness/plugin.go (BuildRedisPlugin), which builds
# the same fixture for L3b, and with plugingit/resolver_test.go fixtureRepo. Held by
# tests/e2e-live/harness/devprovision_test.go: the two must not drift, or a stand stops
# being able to reproduce what the gate sees. This step read manifest.yaml for a whole
# release after NIM-377 removed it, and every fresh stand died at bring-up (NIM-516).
#
# In-place build (cwd=source directory): the plugin's go.mod uses relative-replace
# (../../../sdk, ../../../proto/plugin) - the tree cannot be copied. GOWORK=off - the plugin
# is outside go.work (precedent from Makefile test-plugins). -trimpath -ldflags "-buildid=" -
# a reproducible sha256: otherwise a repeat provision changes the binary → invalidates an
# already-issued Sigil grant. Stamping keeps that property: the same binary and the same
# document append the same bytes.
provision_redis_plugin() {
    local src="${EXAMPLES}/module/redis"
    local dest="${KEEPER_DEV_DIR}/plugin-repos/redis"
    local bin="redis"
    # The document IS the module's contract now, and it is read without running the
    # artifact (at plugin.allow the binary is not approved yet). Absent, there is nothing
    # to stamp - and an unstamped artifact is one plugingit rejects per-entry, so the
    # stand would come up looking healthy and the first scenario touching redis
    # would fail at runtime, where the cause costs far more to find.
    if [ ! -f "${src}/schema.json" ]; then
        fail "redis plugin schema document not found: ${src}/schema.json"
    fi

    log "building redis plugin (${bin}, linux/amd64, reproducible)"
    local tmp
    tmp="$(mktemp -d)"
    if ! ( cd "${src}" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -trimpath -ldflags "-buildid=" -o "${tmp}/${bin}" . ); then
        rm -rf "${tmp}"
        fail "go build of the redis plugin failed (${src})"
    fi
    # cwd=REPO_ROOT so `go run` finds go.work and resolves sdk/schema - the trailer format
    # is defined there and nowhere else (see dev/stamp-artifact.go).
    # GOWORK= (empty, not `off`) resets an operator's exported GOWORK: stamp-artifact.go
    # is outside every module and resolves sdk/schema only through the workspace.
    if ! ( cd "${REPO_ROOT}" && GOWORK= go run ./dev/stamp-artifact.go "${tmp}/${bin}" "${src}/schema.json" ); then
        rm -rf "${tmp}"
        fail "stamping the schema document into the redis artifact failed"
    fi

    # Rebuild from scratch: deterministic commit (GIT_* above) → same SHA for an
    # unchanged binary, keeper reuses the snapshot instead of spawning orphans in the cache.
    rm -rf "${dest}"
    mkdir -p "${dest}/dist"
    cp "${tmp}/${bin}" "${dest}/dist/${bin}"
    chmod 0755 "${dest}/dist/${bin}"
    # The published copy sits beside the artifact for soul-lint, which should not have to
    # download a binary to check a destiny. Non-executable, so dist/ still holds exactly
    # one executable and the resolver's single-artifact rule stays unambiguous.
    cp "${src}/schema.json" "${dest}/dist/schema.json"
    chmod 0644 "${dest}/dist/schema.json"
    rm -rf "${tmp}"

    git -C "${dest}" init -q -b main
    git -C "${dest}" add -A
    # The resolver takes THE single executable in dist/ (pluginhost.SingleArtifactIn) and
    # fails the entry closed on zero or on two - and a closed entry is only a warning, so
    # keeper still comes up green with the plugin silently absent. Both mistakes are one
    # chmod away: git carries only two modes and records the exec bit only where
    # core.fileMode holds (lose it -> zero), and the document copied beside the artifact
    # is one typo from 0755 (-> two). What the resolver sees is what git recorded, so that
    # is what this counts, right here where it can still say so out loud.
    local mode execs
    mode="$(git -C "${dest}" ls-files -s "dist/${bin}" | cut -d' ' -f1)"
    if [ "${mode}" != "100755" ]; then
        fail "git recorded dist/${bin} as ${mode:-nothing}, not 100755 (core.fileMode off under ${dest}?) — plugingit would find no executable in dist/ and redis would silently never arrive"
    fi
    execs="$(git -C "${dest}" ls-files -s dist/ | grep -c '^100755 ' || true)"
    if [ "${execs}" != "1" ]; then
        fail "dist/ carries ${execs} executables, not exactly 1 — plugingit cannot tell which is the artifact and redis would silently never arrive (is ${dest}/dist/schema.json 0755 by mistake?)"
    fi
    git -C "${dest}" -c commit.gpgsign=false commit -q -m "redis plugin snapshot (dev-provision)"
    git -C "${dest}" -c tag.gpgsign=false tag -f v1.0.0 >/dev/null
    log "redis plugin git repo @ ${dest} (branch main + tag v1.0.0, dist/${bin} stamped + dist/schema.json)"
}
provision_redis_plugin

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
    # a keeper placeholder -- the destiny-source template, NOT the renamed registry
    # column (ADR-0085/NIM-729 moved that to `id`). There are no other $-literals in the SQL.
    psql_stand -f - <<SQL
INSERT INTO service_registry (id, git, ref) VALUES
    ('hello-world', 'file://${KEEPER_DEV_DIR}/repos/hello-world', 'main'),
    ('redis',       'file://${KEEPER_DEV_DIR}/repos/redis',       'main')
ON CONFLICT (id) DO NOTHING;

INSERT INTO keeper_settings (key, value) VALUES
    ('default_destiny_source', 'file://${KEEPER_DEV_DIR}/destiny/{name}')
ON CONFLICT (key) DO NOTHING;
SQL
    log "service registry seeded (hello-world, redis, default_destiny_source)"
}

seed_service_registry

log "done"
