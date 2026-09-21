#!/usr/bin/env bash
#
# dev/mint-jwt.sh - issue an Archon-JWT for dev-API calls WITHOUT keeper init.
#
# Why: ad-hoc calls to the Operator API (issue-token, souls-list, etc.) need a
# valid Bearer token. `keeper init` issues a token once, and only for the
# first Archon; this script mints an arbitrary token with the same signature.
#
# The key is NOT hardcoded - it is read from the same Vault KV that the stand's keeper
# uses (${VAULT_KV_PREFIX}/jwt-signing-key, field signing_key). The value is base64(32 bytes);
# keeper base64-decodes it (extractSigningKey), so we decode it too ->
# raw HMAC key for HS256. iss=${ISSUER} matches the stand's keeper.dev.yml issuer.
#
# The token is printed to stdout (ONLY the token - so that `TOKEN=$(dev/mint-jwt.sh)`
# works) AND written to the file $STAND_DEV_DIR/archon-dev.jwt (mode 0400,
# overwritten). The file is a fixed point for the stand's scripts/recipes that
# read the token from there instead of stdout on every call. Diagnostic messages go to
# stderr.
#
# NIM-25: parameterized by the stand profile (DEV_STAND) via dev/stand-env.sh - issuer,
# key KV path, vault container, token directory are taken from the profile. Empty DEV_STAND =
# default (iss keeper-dev-01, secret/keeper, soul-stack-vault, /tmp/keeper-dev).
#
# Configuration via env:
#   DEV_STAND - stand profile (empty=default), derived in dev/stand-env.sh
#   AID       - claim sub (default archon-alice)
#   ROLES     - claim roles, JSON array (default '["cluster-admin"]')
#   TTL       - TTL in seconds (default 43200 = 12h)
# iss=${ISSUER} of the stand; VAULT_TOKEN is forced to root; key path/vault container - from the stand.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Stand profile: ISSUER / VAULT_KV_PREFIX / STACK_PREFIX / STAND_DEV_DIR / VAULT_ADDR.
source "${SCRIPT_DIR}/stand-env.sh"

AID="${AID:-archon-alice}"
ROLES="${ROLES:-[\"cluster-admin\"]}"
TTL="${TTL:-43200}"
VAULT_TOKEN=root
TOKEN_FILE="${STAND_DEV_DIR}/archon-dev.jwt"

log()  { printf '[mint-jwt] %s\n' "$*" >&2; }
fail() { printf '[mint-jwt] [fail] %s\n' "$*" >&2; exit 1; }

command -v python3 >/dev/null 2>&1 || fail "python3 not found in PATH (needed for HS256 signing)"

# Read signing_key from Vault (via docker exec into the vault server container -
# there's usually no host-vault CLI on the dev machine). Extract .data.data.signing_key.
log "reading signing_key from Vault (${VAULT_KV_PREFIX}/jwt-signing-key)"
KV_JSON="$(docker exec \
    -e VAULT_ADDR="${VAULT_ADDR}" \
    -e VAULT_TOKEN="${VAULT_TOKEN}" \
    "${STACK_PREFIX}-vault" sh -c "vault kv get -format=json ${VAULT_KV_PREFIX}/jwt-signing-key" 2>/dev/null)" \
    || fail "failed to read Vault (is ${STACK_PREFIX}-vault up? 'make dev-up' + 'make dev-provision')"

SIGNING_KEY_B64="$(printf '%s' "${KV_JSON}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
print(d["data"]["data"]["signing_key"])
' 2>/dev/null)" || fail "did not find .data.data.signing_key in the Vault response"

[ -n "${SIGNING_KEY_B64}" ] || fail "signing_key is empty in Vault - run 'make dev-provision'"

# HS256 signing with plain python3 (no external libs):
#   token = base64url(header) + '.' + base64url(payload) + '.' + base64url(HMAC-SHA256)
# Key - base64-decode of the value from Vault (keeper does the same in extractSigningKey).
# We capture the output into TOKEN: written to the file convention AND to stdout (rather
# than printed directly) - so that make dev-jwt refreshes $TOKEN_FILE with the new token
# instead of leaving the old one there. A python error (set -e) aborts the script before
# the file is written.
TOKEN="$(AID="${AID}" ROLES="${ROLES}" TTL="${TTL}" ISSUER="${ISSUER}" \
SIGNING_KEY_B64="${SIGNING_KEY_B64}" python3 <<'PY'
import base64, hashlib, hmac, json, os, sys, time

def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")

key = base64.b64decode(os.environ["SIGNING_KEY_B64"])
if len(key) < 32:
    sys.stderr.write("[mint-jwt] [fail] signing key < 32 bytes after base64-decode (HS256 minimum)\n")
    sys.exit(1)

try:
    roles = json.loads(os.environ["ROLES"])
except Exception as e:
    sys.stderr.write(f"[mint-jwt] [fail] ROLES does not parse as a JSON array: {e}\n")
    sys.exit(1)
if not isinstance(roles, list):
    sys.stderr.write("[mint-jwt] [fail] ROLES must be a JSON array, e.g. '[\"cluster-admin\"]'\n")
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

# separators without spaces - compact JSON, like golang-jwt.
seg_header = b64url(json.dumps(header, separators=(",", ":")).encode())
seg_payload = b64url(json.dumps(payload, separators=(",", ":")).encode())
signing_input = f"{seg_header}.{seg_payload}".encode("ascii")
sig = hmac.new(key, signing_input, hashlib.sha256).digest()
print(f"{seg_header}.{seg_payload}.{b64url(sig)}")
PY
)"

# Write the token to the stand's file convention: create the directory, overwrite the
# contents, and set mode 0400 (the same way keeper init drops the bootstrap token - a
# secret, read-only to the owner). rm -f before writing - otherwise truncating an
# existing 0400 file hits permission denied (on a repeat make dev-jwt). chmod explicitly:
# umask at creation time is not guaranteed.
mkdir -p "${STAND_DEV_DIR}"
rm -f "${TOKEN_FILE}"
printf '%s\n' "${TOKEN}" > "${TOKEN_FILE}"
chmod 0400 "${TOKEN_FILE}"
log "token written to ${TOKEN_FILE} (mode 0400)"

# stdout - only the token (the `TOKEN=$(dev/mint-jwt.sh)` contract is preserved).
printf '%s\n' "${TOKEN}"
