# Production deployment of Keeper

Operational documentation for moving Keeper from the dev stack ([local-setup.md](../dev/local-setup.md)) to production. The focus is on the differences from dev and on the infra dependencies (Vault, Postgres) that our code does not cover but that are mandatory for safe operation.

The normative config contract is [config.md](config.md); a sample production config is [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml).

## How production differs from dev

| Aspect | dev ([local-setup.md](../dev/local-setup.md)) | production |
|---|---|---|
| Vault auth | `vault.token: "root"` (static root token) | AppRole (`vault.auth.method: approle`) — see below |
| Vault backend | dev-mode, secrets in RAM (lost on restart) | persistent storage backend + auto-unseal |
| Vault TLS | HTTP without TLS (`http://127.0.0.1:8200`) | HTTPS with a valid certificate (`https://vault.internal:8200`) |
| Vault policy | root (everything allowed) | narrow least-privilege policy ([vault-policy.hcl](../../examples/keeper/vault-policy.hcl)) |
| Keeper TLS material | self-signed / Vault-issued leaf in `/tmp/keeper-dev/tls/` | leaf from the production PKI, rotated per the organization's policy |
| `services[]` / destiny | file:// repos from `/tmp/keeper-dev/` | git URLs of the service registry (Postgres, ADR-029) |
| OTel | `otel.enabled: false` | enabled, export to a collector |

The dev shortcut `vault.token: "root"` must **not** be used in production — the Vault root token must not live in a service config. The production path is AppRole + a narrow policy.

## Vault: AppRole + persistent + auto-unseal

### Keeper AppRole authentication

In production, Keeper authenticates to Vault via AppRole (ADR-014; in code — `shared/config.AuthMethodAppRole`). Keeper performs `auth/approle/login` with a `role_id` + `secret_id` pair, obtains a renewable client token, and renews it (TokenRenewer, `keeper/internal/.../renewer.go`).

The `keeper.yml::vault` block:

```yaml
vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod                       # NOT a secret — the role identifier
    secret_id_file: /etc/keeper/vault-secret-id  # secret_id from a mode 0400 file
  pki_mount: "pki/soulstack"
  pki_role: "soul-seed"
```

`role_id` is **not a secret** (a role identifier), so it may be stored in the clear right in `keeper.yml`. `secret_id` is a **secret**, and is **not** stored as plaintext in the main config; its source is set by one of the following (mutually exclusive):

- `secret_id_file` — path to a file with **`mode 0400`** permissions (only the owner process reads it), whose content is the `secret_id` (a trailing newline is stripped). The recommended production option;
- `secret_id_env` — the name of an env variable holding the `secret_id` (for secret injectors such as Vault Agent / k8s-secret-as-env).

AppRole credentials are **intentionally NOT read from Vault** (a `vault:` ref is not allowed) — this is a chicken-and-egg problem: these are the very credentials Keeper uses to log in so that it can then resolve all the other `vault:` refs (`postgres.dsn_ref`, `signing_key_ref`, …). The `secret_id` source is therefore always local (file/env), available before the Vault client is brought up. Contract details — [config.md → `vault`](config.md#vault), comment in `shared/config/keeper.go` (`KeeperVaultAuth`).

Configuring the role on the Vault side (binding the least-privilege policy from the section below):

```sh
vault policy write keeper-prod examples/keeper/vault-policy.hcl
vault write auth/approle/role/keeper-prod \
    token_policies=keeper-prod \
    secret_id_ttl=720h token_ttl=1h token_max_ttl=24h
# role_id — hand it to the operator (into keeper.yml):
vault read auth/approle/role/keeper-prod/role-id
# secret_id — place into /etc/keeper/vault-secret-id (mode 0400):
vault write -f auth/approle/role/keeper-prod/secret-id
```

### Least-privilege Vault policy

In production, Keeper runs under a narrow policy, not under root. A template with commented path entries is [`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl). In brief, what it grants and why it is minimal:

| Path | Capabilities | Why |
|---|---|---|
| `secret/data/keeper/*` | `read` | Reading Keeper's KV secrets (jwt-signing-key, postgres/redis, providers/credentials of cloud drivers, essence secrets). Read only — Keeper does not write them. |
| `pki/issue/soul-seed` | `update` | Signing the SoulSeed CSR during onboarding (Bootstrap-RPC, ADR-012(b)). `update` (POST) is the only thing the issue endpoint needs. |
| `secret/metadata/keeper/sigil-keys/*` | `list`, `read` | Reaper rule `reap_orphan_vault_keys` (ADR-026(h)) — report-only reconcile of orphaned Sigil signing keys: only names (`list`) + `created_time` (metadata). **NO `delete`, NO data path** — Reaper deletes nothing and does not read private-key values. |
| `auth/token/renew-self` | `update` | TokenRenewer renews Keeper's own client token. Without the right to create/revoke other tokens. |

The paths in the template correspond to the defaults of dev provisioning (KV mount `secret/`, PKI mount `pki/` + role `soul-seed` — see [`dev/provision.sh`](../../dev/provision.sh)). In production the KV/PKI mounts may differ (in the sample config `pki_mount: "pki/soulstack"`) — adjust the path prefixes in the `.hcl` to the actual `keeper.yml::vault.kv_mount` / `pki_mount` / `pki_role`.

### Persistent backend + auto-unseal (infra dependency)

This is an **infrastructure dependency of Vault**, not part of the Soul Stack code — operational notes:

- **Persistent storage backend** is mandatory. dev-mode Vault keeps secrets in RAM and loses them on restart — in production that would mean losing the jwt-signing-key (invalidating all JWTs) and the PKI root. Use a persistent backend (raft / consul / a supported storage).
- **Auto-unseal** is strongly recommended (cloud KMS / transit / HSM). Without it, every Vault restart requires a manual unseal with a quorum of keys, which breaks automatic recovery of the Keeper cluster: while Vault is sealed, Keeper does not resolve `vault:` refs and does not start.
- Bringing up and tuning these components is on the Vault operations team; Soul Stack only consumes a ready, unsealed Vault via `keeper.yml::vault.addr`.

## JWT signing key (production)

Operator JWTs (ADR-014) are signed with a key from KV `secret/keeper/jwt-signing-key` (field `signing_key`), resolved via `auth.jwt.signing_key_ref`. In the MVP this is an HS256 (symmetric) key in KV — acceptable; the transit variant (asymmetric signing on the Vault side) is post-MVP.

**Rotating the signing key** is an operational procedure (not automated):

1. Generate a new key and write it to KV: `vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"`.
2. Redeploy / hot-reload Keeper so it re-reads the key.
3. **Recreate bootstrap tokens / reissue operator JWTs** — all previously issued HS256 JWTs become invalid after the key change (the signature will not verify). The short JWT TTL (`auth.jwt.ttl_default`) limits the window, but active tokens will have to be reissued explicitly.

For this reason, plan rotation for a maintenance window, not "on the fly".

## See also

- [local-setup.md](../dev/local-setup.md) — the dev stack (for production see this page; do not use the dev copy of the config).
- [config.md](config.md) — the normative `keeper.yml` contract (blocks `vault`, `auth`, `reaper`, `acolytes`).
- [vault-policy.hcl](../../examples/keeper/vault-policy.hcl) — least-privilege Vault policy with commented paths.
- [reaper.md → Enabling recovery](reaper.md) — a separate gate procedure for `reclaim_apply_runs` in production.
- [rbac.md](rbac.md) — RBAC and Bootstrap of the first Archon (ADR-013).
