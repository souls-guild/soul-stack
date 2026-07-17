# keeper-prod - least-privilege Vault policy for a prod Keeper instance.
#
# Bound to the AppRole that Keeper uses to log in to Vault
# (keeper.yml::vault.auth.method=approle, role_id=keeper-prod —
# see docs/keeper/prod-setup.md and shared/config.AuthMethodAppRole).
#
# Principle: each path grants EXACTLY the capabilities a given Keeper
# subsystem needs, and not one more. No `*` capabilities,
# no broad mount-level grants. Adjust only the concrete
# paths (KV / PKI mount) for your installation, not the capability set.
#
# Apply (after `vault policy write keeper-prod vault-policy.hcl`):
#   vault write auth/approle/role/keeper-prod \
#       token_policies=keeper-prod \
#       secret_id_ttl=... token_ttl=... token_max_ttl=...
#
# IMPORTANT: the paths below match the dev-provisioning defaults
# (dev/provision.sh): KV mount `secret/`, PKI mount `pki/` + role `soul-seed`.
# In prod the KV mount and PKI mount may differ (examples/keeper/keeper.yml
# shows pki_mount: "pki/soulstack") - adjust the path prefixes to match
# your actual keeper.yml::vault.kv_mount / pki_mount / pki_role.

# --- KV v2: reading Keeper's secrets ----------------------------------------
#
# All of Keeper's runtime secrets live under secret/keeper/* (KV v2). Read-only:
# Keeper only reads them (resolving `vault:`-refs at startup and on hot-reload),
# it never writes. KV v2 stores VALUES under the data path `secret/data/...`.
# This includes:
#   - secret/keeper/jwt-signing-key  (auth.jwt.signing_key_ref, HS256 key, ADR-014)
#   - secret/keeper/postgres         (postgres.dsn_ref, field `dsn`)
#   - secret/keeper/redis            (redis.password_ref)
#   - secret/keeper/providers/*      (cloud driver credentials, ADR-017)
#   - essence secrets under secret/keeper/* (resolving `${ vault(...) }` in CEL)
#
# `read` is enough: reading a KV v2 value doesn't need list/data-write.
path "secret/data/keeper/*" {
  capabilities = ["read"]
}

# --- KV v2: dual-mode operator secret intake (ADR-064, NIM-11) ---------------
#
# On plaintext secret intake (Herald signing/channel token, Provider cloud
# credentials) Keeper ITSELF writes the value to Vault at a deterministic path
# secret/<domain>/<entity>/<field> and stores only the ref in Postgres. KV v2 keeps
# VALUES under the data path. Needs create+update (idempotent-write on update
# overwrites at the same path) + read (resolve on consumption: herald delivery
# reads the signing/channel token, cloud flow reads credentials).
#
# Narrow scope (herald/*, provider/*), NOT the whole mount: keeper writes ONLY to its
# own deterministic prefixes. With a custom kv_mount (keeper.yml::vault.kv_mount),
# replace `secret/` with the actual mount.
path "secret/data/herald/*" {
  capabilities = ["create", "update", "read"]
}
path "secret/data/provider/*" {
  capabilities = ["create", "update", "read"]
}

# --- KV v2: reveal for incarnation secrets (NIM-74) --------------------------
#
# An operator with the incarnation.view-secrets right reveals the plaintext of a secret
# declared in the service's `revealable_secrets` (POST .../secrets/reveal). Keeper
# resolves the value keeper-side via the service's vault_ref. For the redis service
# vault_ref = secret/redis/<incarnation>/users/<key>#password → needs read on
# the prefix declared by the manifest. Read-only (the value is only ever read).
# Narrow scope (redis/*), not the whole mount; with a custom kv_mount/path in
# revealable_secrets - adjust the prefix to match your manifest.
path "secret/data/redis/*" {
  capabilities = ["read"]
}

# --- PKI: signing the SoulSeed CSR during onboarding -------------------------
#
# On the Bootstrap RPC (ADR-012(b)) Keeper issues the SoulSeed certificate
# via the PKI issue role `soul-seed` (dev/provision.sh: `pki/issue/soul-seed`).
# `update` (== POST) is the only capability needed for the issue/sign endpoint;
# Keeper does not manage PKI roles/root, it only issues a leaf against a ready-made role.
# Replace `pki/` + `soul-seed` with your keeper.yml::vault.pki_mount / pki_role.
path "pki/issue/soul-seed" {
  capabilities = ["update"]
}

# --- Reaper: report-only reconcile of orphaned Sigil signing keys ----------
#
# The reap_orphan_vault_keys rule (ADR-026(h), reaper.md -> Rules) finds
# Sigil signing private keys in Vault that have no matching row in the
# sigil_signing_keys registry. It ONLY counts/measures/logs the finding:
#   - `list` - enumerate key_ids under the set of Sigil signing keys;
#   - `read` - read `created_time` from the METADATA layer (for age-based grace).
#
# Deliberately NOT included:
#   - `delete` - Reaper deletes NOTHING from Vault (report-only);
#   - access to the data path `secret/data/keeper/sigil-keys/*` - Reaper does NOT read
#     private key VALUES, only metadata (names + created_time).
# This keeps the Reaper's blast radius minimal: it can neither read a
# signing private key nor delete it.
path "secret/metadata/keeper/sigil-keys/*" {
  capabilities = ["list", "read"]
}

# --- Self-renew client token ------------------------------------------------
#
# Keeper logs in via AppRole and gets a renewable client token; TokenRenewer
# (keeper renewer.go) renews it up to token_max_ttl, so Keeper doesn't lose
# access to Vault over a long uptime. Needs exactly one capability for renew-self
# of its own token - without the right to create/revoke other tokens.
path "auth/token/renew-self" {
  capabilities = ["update"]
}
