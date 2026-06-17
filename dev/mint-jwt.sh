#!/usr/bin/env bash
#
# dev/mint-jwt.sh — выпустить Archon-JWT для dev-API-вызовов БЕЗ keeper init.
#
# Зачем: для ad-hoc вызовов Operator API (issue-token, souls-list и т.п.) нужен
# валидный Bearer-токен. `keeper init` выпускает токен один раз и только для
# первого Архонта; этот скрипт минтит произвольный токен той же подписью.
#
# Ключ НЕ хардкодится — читается из того же Vault KV, что использует keeper
# (secret/keeper/jwt-signing-key, поле signing_key). Значение — base64(32 байт);
# keeper его base64-декодирует (extractSigningKey), поэтому и мы декодируем →
# raw HMAC-ключ для HS256. iss=keeper-dev-01 совпадает с issuer в keeper.dev.yml.
#
# Токен печатается в stdout (ТОЛЬКО токен — чтобы `TOKEN=$(dev/mint-jwt.sh)`
# работало) И записывается в файл $KEEPER_DEV_DIR/archon-dev.jwt (mode 0400,
# перезапись). Файл — фиксированная точка для скриптов/рецептов стенда, которые
# читают токен оттуда, а не из stdout каждого вызова. Служебные сообщения — в
# stderr.
#
# Параметры через env или позиционные не используются; настройка через env:
#   AID           — claim sub (default archon-alice)
#   ROLES         — claim roles, JSON-массив (default '["cluster-admin"]')
#   TTL           — время жизни в секундах (default 43200 = 12h)
#   ISSUER        — claim iss (default keeper-dev-01, как auth.jwt.issuer в keeper.dev.yml)
#   VAULT_TOKEN   — root-token dev-Vault (default root)
#   KEEPER_DEV_DIR — корень dev-артефактов; токен пишется в $KEEPER_DEV_DIR/archon-dev.jwt
#                    (default /tmp/keeper-dev — как у dev/keeper-run.sh)

set -euo pipefail

AID="${AID:-archon-alice}"
ROLES="${ROLES:-[\"cluster-admin\"]}"
TTL="${TTL:-43200}"
ISSUER="${ISSUER:-keeper-dev-01}"
VAULT_TOKEN="${VAULT_TOKEN:-root}"
KEEPER_DEV_DIR="${KEEPER_DEV_DIR:-/tmp/keeper-dev}"
TOKEN_FILE="${KEEPER_DEV_DIR}/archon-dev.jwt"

log()  { printf '[mint-jwt] %s\n' "$*" >&2; }
fail() { printf '[mint-jwt] [fail] %s\n' "$*" >&2; exit 1; }

command -v python3 >/dev/null 2>&1 || fail "python3 не найден в PATH (нужен для HS256-подписи)"

# Читаем signing_key из Vault (через docker exec в контейнер vault-сервера —
# host-vault CLI на dev-машине обычно нет). Вынимаем .data.data.signing_key.
log "читаю signing_key из Vault (secret/keeper/jwt-signing-key)"
KV_JSON="$(docker exec \
    -e VAULT_ADDR=http://127.0.0.1:8200 \
    -e VAULT_TOKEN="${VAULT_TOKEN}" \
    soul-stack-vault sh -c 'vault kv get -format=json secret/keeper/jwt-signing-key' 2>/dev/null)" \
    || fail "не удалось прочитать Vault (soul-stack-vault поднят? 'make dev-up' + 'make dev-provision')"

SIGNING_KEY_B64="$(printf '%s' "${KV_JSON}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
print(d["data"]["data"]["signing_key"])
' 2>/dev/null)" || fail "не нашёл .data.data.signing_key в ответе Vault"

[ -n "${SIGNING_KEY_B64}" ] || fail "signing_key пустой в Vault — запусти 'make dev-provision'"

# HS256-подпись чистым python3 (без внешних либ):
#   token = base64url(header) + '.' + base64url(payload) + '.' + base64url(HMAC-SHA256)
# Ключ — base64-decode значения из Vault (keeper делает то же в extractSigningKey).
# Вывод перехватываем в TOKEN: пишем в файл-конвенцию И в stdout (а не печатаем
# напрямую) — чтобы make dev-jwt обновлял $TOKEN_FILE свежим токеном, а не оставлял
# там старый. Ошибка python (set -e) прервёт скрипт до записи файла.
TOKEN="$(AID="${AID}" ROLES="${ROLES}" TTL="${TTL}" ISSUER="${ISSUER}" \
SIGNING_KEY_B64="${SIGNING_KEY_B64}" python3 <<'PY'
import base64, hashlib, hmac, json, os, sys, time

def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")

key = base64.b64decode(os.environ["SIGNING_KEY_B64"])
if len(key) < 32:
    sys.stderr.write("[mint-jwt] [fail] signing key < 32 байт после base64-decode (HS256-минимум)\n")
    sys.exit(1)

try:
    roles = json.loads(os.environ["ROLES"])
except Exception as e:
    sys.stderr.write(f"[mint-jwt] [fail] ROLES не парсится как JSON-массив: {e}\n")
    sys.exit(1)
if not isinstance(roles, list):
    sys.stderr.write("[mint-jwt] [fail] ROLES должен быть JSON-массивом, например '[\"cluster-admin\"]'\n")
    sys.exit(1)

now = int(time.time())
ttl = int(os.environ["TTL"])

header = {"alg": "HS256", "typ": "JWT"}
payload = {
    "iss": os.environ["ISSUER"],
    "sub": os.environ["AID"],
    "iat": now,
    "exp": now + ttl,
    "roles": roles,
}

# separators без пробелов — компактный JSON, как у golang-jwt.
seg_header = b64url(json.dumps(header, separators=(",", ":")).encode())
seg_payload = b64url(json.dumps(payload, separators=(",", ":")).encode())
signing_input = f"{seg_header}.{seg_payload}".encode("ascii")
sig = hmac.new(key, signing_input, hashlib.sha256).digest()
print(f"{seg_header}.{seg_payload}.{b64url(sig)}")
PY
)"

# Пишем токен в файл-конвенцию стенда: создаём каталог, перезаписываем содержимое
# и ставим mode 0400 (как keeper init кладёт bootstrap-токен — секрет, read-only
# владельцу). rm -f до записи — иначе truncation поверх существующего 0400-файла
# упрётся в permission denied (повторный make dev-jwt). chmod явно: umask на
# момент создания не гарантирован.
mkdir -p "${KEEPER_DEV_DIR}"
rm -f "${TOKEN_FILE}"
printf '%s\n' "${TOKEN}" > "${TOKEN_FILE}"
chmod 0400 "${TOKEN_FILE}"
log "токен записан в ${TOKEN_FILE} (mode 0400)"

# stdout — только токен (контракт `TOKEN=$(dev/mint-jwt.sh)` сохранён).
printf '%s\n' "${TOKEN}"
