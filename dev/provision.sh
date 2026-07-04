#!/usr/bin/env bash
#
# dev/provision.sh — idempotent local-dev provisioning для Soul Stack.
#
# Запускается после `make dev-up`. Заполняет Vault KV/PKI, создаёт
# self-signed TLS-материал для Keeper-а и материализует git-репозитории
# для service/destiny-артефактов из examples/ — чтобы прод-резолв
# (artifact.ServiceLoader / DestinyLoader, ADR-007/ADR-009) мог склонировать
# их по file://-URL из keeper.dev.yml. Повторный запуск безопасен: каждый шаг
# проверяет своё состояние перед write/enable/commit и пишет "[skip] ..." если
# уже сделано.
#
# Не требует установленного `vault`-CLI на хосте: если он отсутствует,
# команды проксируются через `docker exec soul-stack-vault vault ...`.
#
# Параметры через env:
#   DEV_STAND      — идентификатор стенда (пусто=default); derived-переменные (STAND_DEV_DIR /
#                    PG_DB / VAULT_KV_PREFIX / STACK_PREFIX / порты) — dev/stand-env.sh (NIM-25)
#   VAULT_TOKEN    — форсится root (dev); VAULT_ADDR/PG_PORT — из stand-env
#   PG_DSN         — DSN для ${VAULT_KV_PREFIX}/postgres (default выводится из стенда: БД ${PG_DB})
#   PKI_ROLE_DOMAINS — allowed_domains для роли soul-seed
#                    (default example.com,test,localhost,host.docker.internal,soul-docker-*)
#   DEV_KEEPER_EXTRA_IP — опц. доп. IP в ip_sans keeper-серта (WSL2 host-IP для
#                    docker-душ, NIM-26); пусто → только 127.0.0.1
#   REPO_ROOT      — корень репозитория soul-stack (источник examples/);
#                    по умолчанию выводится из пути этого скрипта

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Профиль стенда: STAND_DEV_DIR / PG_DB / VAULT_KV_PREFIX / STACK_PREFIX / PG_PORT / … (NIM-25).
source "${SCRIPT_DIR}/stand-env.sh"

# Явный dev VAULT_TOKEN=root: в env юзера бывает прод-токен — форсим root.
# VAULT_ADDR — из stand-env (лёгкий режим = общий :8200).
VAULT_TOKEN=root
KEEPER_DEV_DIR="${STAND_DEV_DIR}"
PG_DSN="${PG_DSN:-postgres://keeper:keeper@localhost:${PG_PORT}/${PG_DB}?sslmode=disable}"
# host.docker.internal — keeper-cert SAN; soul-docker-* — glob для bare-CN docker-душ (NIM-26).
PKI_ROLE_DOMAINS="${PKI_ROLE_DOMAINS:-example.com,test,localhost,host.docker.internal,soul-docker-*}"
# Опц. host-IP в ip_sans keeper-серта (WSL2: keeper endpoint = host-IP, NIM-26).
DEV_KEEPER_EXTRA_IP="${DEV_KEEPER_EXTRA_IP:-}"
# REPO_ROOT — корень репо: каталог на уровень выше dev/ (где лежит этот скрипт).
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

export VAULT_ADDR VAULT_TOKEN

log_stand() { printf '[provision] стенд: slug=%s slot=%s dir=%s pg_db=%s kv=%s stack=%s\n' "${STAND_SLUG:-<default>}" "${STAND_SLOT}" "${STAND_DEV_DIR}" "${PG_DB}" "${VAULT_KV_PREFIX}" "${STACK_PREFIX}"; }

log() { printf '[provision] %s\n' "$*"; }
skip() { printf '[provision] [skip] %s\n' "$*"; }
fail() { printf '[provision] [fail] %s\n' "$*" >&2; exit 1; }

# vault_cli — обёртка над `vault` CLI. На macOS-dev-машинах vault обычно
# не установлен, поэтому fallback на CLI внутри контейнера vault-сервера.
# Используем `docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=...`,
# чтобы контейнерный CLI ходил на dev-listener самого же контейнера.
if command -v vault >/dev/null 2>&1; then
    vault_cli() { vault "$@"; }
    VAULT_ENDPOINT_DESC="${VAULT_ADDR} (host vault CLI)"
    log "vault CLI: host ($(command -v vault))"
else
    if ! command -v docker >/dev/null 2>&1; then
        fail "neither 'vault' nor 'docker' CLI found in PATH"
    fi
    # Внутри контейнера ходим на dev-listener самого же контейнера,
    # игнорируя host-level VAULT_ADDR (он может указывать на прод-Vault).
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

# Sanity: Vault достижим и unsealed.
if ! vault_cli status >/dev/null 2>&1; then
    fail "vault not reachable at ${VAULT_ENDPOINT_DESC} (run 'make dev-up' first)"
fi
log "vault reachable at ${VAULT_ENDPOINT_DESC}"

# 1. KV: ${VAULT_KV_PREFIX}/postgres (поле `dsn`). Префикс per-стенд (NIM-25).
if vault_cli kv get -field=dsn "${VAULT_KV_PREFIX}/postgres" >/dev/null 2>&1; then
    skip "${VAULT_KV_PREFIX}/postgres already set"
else
    log "writing ${VAULT_KV_PREFIX}/postgres"
    vault_cli kv put "${VAULT_KV_PREFIX}/postgres" dsn="${PG_DSN}" >/dev/null
fi

# 2. KV: ${VAULT_KV_PREFIX}/jwt-signing-key (поле `signing_key`).
# signing_key — 32 байта случайных данных в base64, генерится один раз
# и фиксируется в Vault. При re-run скрипта существующий ключ НЕ
# перегенерируется, иначе все ранее выпущенные JWT станут невалидны.
if vault_cli kv get -field=signing_key "${VAULT_KV_PREFIX}/jwt-signing-key" >/dev/null 2>&1; then
    skip "${VAULT_KV_PREFIX}/jwt-signing-key already set"
else
    log "generating and writing ${VAULT_KV_PREFIX}/jwt-signing-key"
    SIGNING_KEY="$(openssl rand -base64 32)"
    vault_cli kv put "${VAULT_KV_PREFIX}/jwt-signing-key" signing_key="${SIGNING_KEY}" >/dev/null
fi

# 2b. KV: ${VAULT_KV_PREFIX}/sigil-signing-key (поле `signing_key`, ed25519 PEM PKCS#8).
# Обязателен: при пустом реестре sigil_signing_keys keeper падает на старте без него
# (fallback на cfg.signing_key_ref, ADR-026(h)). Генерится один раз, при re-run НЕ
# перегенерируется (иначе сломались бы уже введённые Sigil-допуски).
if vault_cli kv get -field=signing_key "${VAULT_KV_PREFIX}/sigil-signing-key" >/dev/null 2>&1; then
    skip "${VAULT_KV_PREFIX}/sigil-signing-key already set"
else
    log "generating and writing ${VAULT_KV_PREFIX}/sigil-signing-key (ed25519 PEM PKCS#8)"
    SIGIL_KEY="$(openssl genpkey -algorithm ed25519 2>/dev/null)"
    [ -n "${SIGIL_KEY}" ] || fail "openssl genpkey ed25519 не дал ключ (нужен openssl ≥1.1.1)"
    vault_cli kv put "${VAULT_KV_PREFIX}/sigil-signing-key" signing_key="${SIGIL_KEY}" >/dev/null
fi

# 3. PKI secrets engine на пути `pki/`.
# `vault secrets list -format=json` парсим без jq — grep по ключу пути.
if vault_cli secrets list -format=json 2>/dev/null | grep -q '"pki/"'; then
    skip "pki/ secrets engine already enabled"
else
    log "enabling pki/ secrets engine"
    vault_cli secrets enable -path=pki pki >/dev/null
    vault_cli secrets tune -max-lease-ttl=87600h pki >/dev/null
fi

# 4. PKI root certificate.
# `vault read pki/cert/ca` возвращает 0 только если root уже сгенерирован.
if vault_cli read pki/cert/ca >/dev/null 2>&1; then
    skip "pki root certificate already generated"
else
    log "generating pki root certificate (CN=soul-stack, ttl=87600h)"
    vault_cli write pki/root/generate/internal \
        common_name="soul-stack" ttl=87600h >/dev/null
fi

# 5. PKI role `soul-seed` (подписывает keeper-cert И SoulSeed-CSR душ). Идемпотентность
# по СОДЕРЖИМОМУ: перезаписываем, пока allowed_domains не включает soul-docker (иначе
# старая роль без glob осталась бы и CSR docker-душ падал бы 400). allow_bare_domains —
# точное host.docker.internal; allow_glob_domains — docker-CN soul-docker-N (bare-имена
# вне доменов) матчатся glob-ом soul-docker-*; host-флот *.example.com не затронут. NIM-26.
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

# 6. Dev-каталоги Keeper-а.
# tls/ — Vault-issued cert + Vault-root CA для bootstrap+event_stream listener (см. шаг 7 + keeper.dev.yml).
# plugins/ — кеш скачанных плагинов (plugins.cache_root).
# plugin-sockets/ — unix-сокеты per-plugin процесса (plugin_runtime.socket_dir).
mkdir -p "${KEEPER_DEV_DIR}/tls" \
         "${KEEPER_DEV_DIR}/plugins" \
         "${KEEPER_DEV_DIR}/plugin-sockets"
log "ensured ${KEEPER_DEV_DIR}/{tls,plugins,plugin-sockets}"

# 7. TLS-материал для Keeper listener-ов — выписывается из Vault PKI.
#
# Серверный cert Keeper-а ДОЛЖЕН цепляться к тому же корню (CN=soul-stack),
# что и SoulSeed-сертификаты: иначе на EventStream (mTLS) Soul не доверяет
# серверному cert-у Keeper-а (Soul после bootstrap верит только Vault-root
# из seed/ca.pem), а Keeper не доверяет client-cert-у Soul-а. Self-signed
# cert работал только для Bootstrap-фазы (там Soul берёт CA из конфига),
# но ломал EventStream — поэтому здесь Vault-issued leaf + Vault-root как
# trust-anchor/ClientCAs.
#
#   keeper.crt    — leaf (CN=localhost, SAN DNS:localhost,IP:127.0.0.1).
#   keeper.key    — приватный ключ leaf-а.
#   vault-ca.crt  — корень Vault PKI (CN=soul-stack); в keeper.dev.yml это
#                   event_stream.tls.ca (ClientCAs), в soul.dev.yml —
#                   keeper.tls.ca (верификация серверного cert-а на bootstrap).
CRT="${KEEPER_DEV_DIR}/tls/keeper.crt"
KEY="${KEEPER_DEV_DIR}/tls/keeper.key"
VAULT_CA="${KEEPER_DEV_DIR}/tls/vault-ca.crt"

# issue_keeper_cert — выписать leaf из Vault PKI и разложить crt/key/ca по файлам.
# SAN включает host.docker.internal (docker-души dev-флота, NIM-26) + опц.
# DEV_KEEPER_EXTRA_IP (WSL2 host-IP). localhost/127.0.0.1 сохранены (host-флот).
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

# tls_material_current — true, если crt/key/ca на месте И серты всё ещё цепляются
# к ТЕКУЩЕМУ Vault PKI root. Reset-aware: после `make dev-reset` Vault root
# пересоздаётся (новый serial), а старый keeper.crt/vault-ca.crt остаются на
# диске — простой `[ -s ... ]` тогда ошибочно скипает перевыпуск, и mTLS-онбординг
# нового Soul ломается (Keeper ClientCAs доверяют старому root). Проверяем:
#   (1) сохранённый vault-ca.crt совпадает с живым `vault read pki/cert/ca`;
#   (2) keeper.crt верифицируется против сохранённого CA (ловит ротацию leaf-а).
tls_material_current() {
    [ -s "${CRT}" ] && [ -s "${KEY}" ] && [ -s "${VAULT_CA}" ] || return 1

    local live_ca
    live_ca="$(vault_cli read -field=certificate pki/cert/ca 2>/dev/null)" || return 1
    [ -n "${live_ca}" ] || return 1

    # Нормализуем PEM обоих сертификатов через openssl и сравниваем DER-хэш:
    # устойчиво к различиям в trailing-newline/переносах между Vault и файлом.
    local saved_fp live_fp
    saved_fp="$(openssl x509 -in "${VAULT_CA}" -outform DER 2>/dev/null | openssl dgst -sha256)" || return 1
    live_fp="$(printf '%s\n' "${live_ca}" | openssl x509 -outform DER 2>/dev/null | openssl dgst -sha256)" || return 1
    [ "${saved_fp}" = "${live_fp}" ] || return 1

    # keeper.crt должен цепляться к сохранённому (== живому) root.
    openssl verify -CAfile "${VAULT_CA}" "${CRT}" >/dev/null 2>&1 || return 1

    # SAN должен включать host.docker.internal (docker-души, NIM-26) + опц.
    # DEV_KEEPER_EXTRA_IP — иначе перевыпуск, чтобы docker-душа не поймала SAN-mismatch.
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

# 8. Sanity: Postgres reachable. Apply миграций делает сам `keeper init`/`keeper run`
# (идемпотентно через migrate.Apply в БД стенда ${PG_DB}), поэтому отдельный
# schema-bootstrap в provision.sh не нужен — это был бы дубликат.
#
# Две обёртки (симметрично vault_cli): psql_admin — всегда существующая bootstrap-БД
# `keeper` (reachability + CREATE DATABASE стенда); psql_stand — БД стенда ${PG_DB}
# (seed). Для default обе бьют в `keeper` (идентично прежнему psql_cli). NIM-25.
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

# 8b. БД стенда ${PG_DB} — создать идемпотентно (CREATE DATABASE без IF NOT EXISTS).
# Лёгкая изоляция: общий Postgres, отдельная БД на стенд. Default (keeper) создаётся
# docker-compose — пропускаем. Создаём ДО keeper init/run (тот мигрирует ${PG_DB}). NIM-25.
ensure_stand_db() {
    if [ "${PG_DB}" = "keeper" ]; then
        skip "БД keeper (default) — создаётся docker-compose"
        return 0
    fi
    if [ "${PG_REACHABLE}" != "1" ]; then
        skip "БД ${PG_DB}: postgres недоступен — создание отложено (повтори provision)"
        return 0
    fi
    if [ "$(psql_admin -tAc "SELECT 1 FROM pg_database WHERE datname='${PG_DB}'" 2>/dev/null)" = "1" ]; then
        skip "БД ${PG_DB} уже существует"
    else
        log "создаю БД ${PG_DB} (owner keeper)"
        psql_admin -c "CREATE DATABASE \"${PG_DB}\" OWNER keeper" >/dev/null
    fi
}
ensure_stand_db

# 9. Git-репозитории service/destiny-артефактов из examples/.
#
# Прод-резолв Keeper-а (artifact.ServiceLoader / DestinyLoader, ADR-007/ADR-009)
# клонирует service- и destiny-репо по git-URL+ref. Координаты резолва живут в
# реестре сервисов в Postgres (service_registry + keeper_settings, ADR-029) —
# их сеет шаг 10 ниже:
#   - service-репо   — по записям service_registry (git/ref);
#   - destiny-репо   — по keeper_settings[default_destiny_source] с подстановкой
#                      {name}, ref из service.yml::destiny[] (для redis — v1.0.0).
# Сами репозитории никто не создаёт автоматически — этот шаг материализует их
# из examples/ как локальные git-репо под file://-URL-ами, на которые указывает
# засеянный реестр.
#
# file://-репо требуют SOUL_STACK_ALLOW_FILE_REPOS=1 на стороне keeper run
# (см. docs/dev/local-setup.md) — provision только создаёт репо и сеет реестр
# (шаг 10), флаг — у keeper.

# Фиксированные author/committer для детерминированного commit-SHA: одинаковое
# содержимое examples/ → одинаковый SHA → keeper переиспользует снапшот, а не
# плодит сироты в cache при каждом provision (см. snapshot cache by SHA).
export GIT_AUTHOR_NAME="soul-stack-dev"
export GIT_AUTHOR_EMAIL="dev@soul-stack.local"
export GIT_COMMITTER_NAME="soul-stack-dev"
export GIT_COMMITTER_EMAIL="dev@soul-stack.local"
export GIT_AUTHOR_DATE="2020-01-01T00:00:00Z"
export GIT_COMMITTER_DATE="2020-01-01T00:00:00Z"

# provision_git_repo SRC DEST REF KIND
#   SRC  — каталог-источник в examples/ (содержимое копируется без .git);
#   DEST — целевой каталог git-репо (под KEEPER_DEV_DIR);
#   REF  — git-ref, на который должен указывать артефакт (branch `main` или tag
#          вида `v1.0.0`; tag распознаётся по префиксу `v` + цифра);
#   KIND — метка для лога ("сервиса"/"destiny").
# Идемпотентность: репо пересоздаётся с нуля (rm -rf DEST) каждый раз, но
# детерминированный commit гарантирует тот же SHA при неизменном содержимом.
provision_git_repo() {
    local src="$1" dest="$2" ref="$3" kind="$4"
    if [ ! -d "${src}" ]; then
        fail "источник ${kind} не найден: ${src}"
    fi

    # tag-ref (v1.0.0, …) кладём на ветку main + тег; branch-ref — просто ветку.
    local is_tag=0
    case "${ref}" in
        v[0-9]*) is_tag=1 ;;
    esac

    # Пересборка с нуля: дёшево для маленьких examples/, исключает stale-tree.
    rm -rf "${dest}"
    mkdir -p "${dest}"
    # Копируем содержимое src БЕЗ корневого каталога и без .git (его в src нет).
    cp -R "${src}/." "${dest}/"

    git -C "${dest}" init -q -b main
    git -C "${dest}" add -A
    # -c *.gpgsign=false: снять подпись оператора (нет ssh-askpass в WSL, dev-артефактам подпись не нужна).
    git -C "${dest}" -c commit.gpgsign=false commit -q -m "${kind} snapshot from examples/ (dev-provision)"
    if [ "${is_tag}" = "1" ]; then
        git -C "${dest}" -c tag.gpgsign=false tag -f "${ref}" >/dev/null
        log "git-репо ${kind} @ ${dest} (branch main + tag ${ref})"
    else
        log "git-репо ${kind} @ ${dest} (branch ${ref})"
    fi
}

if ! command -v git >/dev/null 2>&1; then
    fail "git CLI не найден в PATH — нужен для материализации service/destiny-репо"
fi

EXAMPLES="${REPO_ROOT}/examples"
mkdir -p "${KEEPER_DEV_DIR}/repos" "${KEEPER_DEV_DIR}/destiny"

# service-репо (записи service_registry, см. шаг 10; ref: main).
provision_git_repo \
    "${EXAMPLES}/service/hello-world" \
    "${KEEPER_DEV_DIR}/repos/hello-world" \
    main "сервиса hello-world"
provision_git_repo \
    "${EXAMPLES}/service/redis" \
    "${KEEPER_DEV_DIR}/repos/redis" \
    main "сервиса redis"

# destiny-репо (keeper_settings[default_destiny_source]=file://.../destiny/{name},
# см. шаг 10; ref: v1.0.0 — из redis/service.yml::destiny[]). Имя каталога
# = {name} из destiny[], и каталог examples теперь тоже голый {name}.
provision_git_repo \
    "${EXAMPLES}/destiny/redis" \
    "${KEEPER_DEV_DIR}/destiny/redis" \
    v1.0.0 "destiny redis"
provision_git_repo \
    "${EXAMPLES}/destiny/redis-exporter" \
    "${KEEPER_DEV_DIR}/destiny/redis-exporter" \
    v1.0.0 "destiny redis-exporter"
# node-exporter (examples/destiny/node-exporter/, бинарь wb_node_exporter,
# version-aware install, textfile-коллекторы). Резолвится единообразно через
# default_destiny_source ({name}=node-exporter), без per-entry git override.
provision_git_repo \
    "${EXAMPLES}/destiny/node-exporter" \
    "${KEEPER_DEV_DIR}/destiny/node-exporter" \
    v1.0.0 "destiny node-exporter"
# vector (лог-пайплайн, Слайс I мониторинга redis) — объявлен в redis/service.yml::destiny[].
provision_git_repo \
    "${EXAMPLES}/destiny/vector" \
    "${KEEPER_DEV_DIR}/destiny/vector" \
    v1.0.0 "destiny vector"

# 10. Seed реестра сервисов в Postgres (service_registry + keeper_settings).
#
# До ADR-029 эти координаты жили в keeper.dev.yml::services[] /
# default_destiny_source; hard-cut S4 убрал их из конфига — теперь резолв
# (serviceregistry.Holder.Resolve / DefaultDestinySource) читает только БД.
# Без seed-а E2E-smoke поднял бы пустой реестр и Resolve("hello-world"/"redis")
# вернул бы false. Сеем те же записи, что раньше были в services[]:
#   - service hello-world → file://${KEEPER_DEV_DIR}/repos/hello-world @ main
#   - service redis       → file://${KEEPER_DEV_DIR}/repos/redis @ main
#   - keeper_settings[default_destiny_source] = file://${KEEPER_DEV_DIR}/destiny/{name}
#
# Способ — прямой psql INSERT (provision имеет PG-доступ; Архонт/JWT для
# service.* API на этом шаге ещё не выпущены). Идемпотентно: ON CONFLICT DO
# NOTHING (повторный provision не трогает уже засеянные/правленные оператором
# записи). created_by_aid/updated_by_aid = NULL — seed без инициатора-Архонта
# (схема это допускает, FK ON DELETE SET NULL).
#
# Порядок в make dev-smoke: provision запускается ДО `keeper init`, который и
# создаёт схему (migrate.Apply). На свежей БД (dev-reset) таблиц ещё нет — тогда
# seed логирует [skip] и провижининг остаётся зелёным; реестр засеется на
# повторном `make dev-provision` уже после `keeper init` (provision идемпотентен,
# см. шапку). Если схема на месте (БД переживает или init уже прошёл) — сеем
# сразу.
seed_service_registry() {
    if [ "${PG_REACHABLE}" != "1" ]; then
        skip "service-реестр: postgres недоступен — seed пропущен (повторить provision после keeper init)"
        return 0
    fi
    # Схема создаётся keeper init/run-ом (migrate.Apply) в БД стенда. До неё seed невозможен.
    if ! psql_stand -tAc "SELECT to_regclass('public.service_registry') IS NOT NULL AND to_regclass('public.keeper_settings') IS NOT NULL" 2>/dev/null | grep -qx t; then
        skip "service-реестр: схема ещё не применена (нет service_registry/keeper_settings) — seed отложен до запуска после keeper init; повторить 'make dev-provision'"
        return 0
    fi

    log "seeding service_registry (hello-world, redis) + keeper_settings[default_destiny_source]"
    # Unquoted heredoc: подставляется только ${KEEPER_DEV_DIR}; {name} (без $) остаётся
    # плейсхолдером keeper-а. Иных $-литералов в SQL нет.
    psql_stand -f - <<SQL
INSERT INTO service_registry (name, git, ref) VALUES
    ('hello-world', 'file://${KEEPER_DEV_DIR}/repos/hello-world', 'main'),
    ('redis',       'file://${KEEPER_DEV_DIR}/repos/redis',       'main')
ON CONFLICT (name) DO NOTHING;

INSERT INTO keeper_settings (key, value) VALUES
    ('default_destiny_source', 'file://${KEEPER_DEV_DIR}/destiny/{name}')
ON CONFLICT (key) DO NOTHING;
SQL
    log "service-реестр засеян (hello-world, redis, default_destiny_source)"
}

seed_service_registry

log "done"
