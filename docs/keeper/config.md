# Format `keeper.yml`

Configuration of one Keeper cluster instance. Several instances with different `kid` are behind the common Postgres + Redis (see [concept.md](concept.md), [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)).

Reference example - [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml). This document describes each block and **standardizes** all fields - the parser is written based on it.

## Type conventions

A single type dictionary is used throughout:

| Record | Meaning |
|---|---|
| `string` | arbitrary UTF-8 string. |
| `int` | signed 64-bit integer. |
| `bool` | `true` / `false`. |
| `duration` | Go-duration string (`1s` / `500ms` / `1h30m`) + valid suffix `<N>d` for days (`30d` = 720h). Used for all duration fields. Composite form `1d2h` is not supported. |
| `enum{a,b,c}` | a string from an explicitly enumerated set. |
| `string(host:port)` | string `host:port`; `host` - IP or DNS name, `port` - `1..65535`. |
| `vault-ref` | string like `vault:<path>` (`vault:secret/keeper/postgres`); read through the client Vault at the start of Keeper. |
| `path` | absolute path in the local FS of the host on which Keeper is running. |
| `git-url` | git-URL (`git@host:org/repo.git` / `https://…/repo.git`). |
| `git-ref` | git tag or branch (without semver-range, [ADR-007](../adr/0007-versioning-git-ref.md)). |
| `list<T>` / `map<K,V>` | as usual. |

`default: —` denotes a required field without a default. Optional fields are marked `optional`. The values ​​of `enum{…}` are lowercase ASCII, no spaces.

## `kid`

```yaml
kid: keeper-eu-west-01
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `kid` | `string` (kebab-case, unique in the cluster; regex `^[a-z][a-z0-9-]{0,62}$`, see [naming-rules.md → Identifiers](../naming-rules.md)) | — | A stable human-readable identifier for this Keeper instance. Used in lease on SID (`SET sid:lock <kid>`), in the `last_seen_by_kid` column of the `souls` table, in audit events and metrics. See [concept.md → KID](concept.md#kid). |

## `listen`

Network listeners. gRPC is formalized as two independent sub-listeners
by [ADR-012(b)](../adr/0012-keeper-soul-grpc.md):

- `listen.grpc.bootstrap` - Bootstrap-RPC, **server-only TLS** (Soul does not have a SoulSeed certificate before onboarding).
- `listen.grpc.event_stream` - long-lived bidi stream, **mTLS** (SoulSeed validation of incoming Souls by `tls.ca`).

```yaml
listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
        # ca - NOT supported, parser returns unknown_key
    event_stream:
      addr: "0.0.0.0:9443"
      max_apply_size_mb: 8            # send limit ApplyRequest, default 8 MiB
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
        ca:   /etc/keeper/tls/ca.crt
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `listen.grpc.bootstrap.addr` | `string(host:port)` | — | bind address of the Bootstrap-RPC listener (server-only TLS). Mandatory (ADR-012(b)); empty → diag `grpc_bootstrap_listener_required`. |
| `listen.grpc.bootstrap.tls.cert` | `path` | — | Keeper server certificate for Bootstrap. |
| `listen.grpc.bootstrap.tls.key` | `path` | — | private key to `cert`. |
| `listen.grpc.event_stream.addr` | `string(host:port)` | — | bind address of the EventStream listener (mTLS). Required; empty → diag `grpc_event_stream_listener_required`. Must be different from `bootstrap.addr` (otherwise diag `bootstrap_eventstream_port_conflict`). |
| `listen.grpc.event_stream.tls.cert` | `path` | — | Keeper server certificate for EventStream. Matches with bootstrap are allowed. |
| `listen.grpc.event_stream.tls.key` | `path` | — | private key to `cert`. |
| `listen.grpc.event_stream.tls.ca` | `path` | — | CA against which SoulSeed certificates of incoming Souls are validated. |
| `listen.grpc.event_stream.max_apply_size_mb` | `int` (MiB, ≥1) | `8` | The ceiling is the size of one outgoing FromKeeper message, primarily `ApplyRequest` with a bunch of rendered `RenderedTask` (render of Destiny - Keeper-side, [ADR-012](../adr/0012-keeper-soul-grpc.md)). Used as `grpc.MaxSendMsgSize` on the EventStream server: when trying to send more, Keeper crashes fail-fast (`ResourceExhausted`), rather than giving Soul a message, which he will silently reject. `0`/omitted → default `8`; `<1` → diag `value_out_of_range`. **Must be ≤ soul-recv-limit** (`keeper.max_apply_size_mb` in [soul/config.md](../soul/config.md#keeper)); defaults of both parties are the same (8 MiB). This is the outgoing send limit; recv limit for incoming FromSoul is a separate internal invariant (1 MiB), not controlled by the config. |
| `listen.openapi.addr` | `string(host:port)` | — | bind address of the OpenAPI facade (primary operator interface, [ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)). Mandatory listener according to the end-to-end requirement "built-in OpenAPI support" ([requirements.md](../requirements.md)); disabling is prohibited by the parser grammar. |
| `listen.mcp.addr` | `string(host:port)` | — | bind address of the MCP server (primary interface on a par with OpenAPI). Mandatory listener according to the end-to-end requirement "embedded MCP" ([requirements.md](../requirements.md)); disabling is prohibited by the parser grammar. The directory of tools available through this listener is [mcp-tools.md](mcp-tools.md). |
| `listen.metrics.addr` | `string(host:port)` | — | bind address of the **dedicated** Prometheus-`/metrics` listener (separate port, usually `9090`, [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). The endpoint is **not** mounted on the openapi router: scrape goes here, without the auth-chain Operator API. keeper_http_* metrics are still collected by middleware on `/v1/*` and displayed here (one registry). Mandatory listener according to the end-to-end requirement "publication of metrics" ([requirements.md](../requirements.md)); disabling is prohibited by the parser grammar. Opt. protection - [`metrics.auth.basic`](#metrics). |

## `postgres`

```yaml
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }
```

Connection to Postgres - the only cold state storage of the Keeper cluster ([ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres), [storage.md](storage.md)).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `postgres.dsn_ref` | `vault-ref` | — | Vault-reference to full DSN. Plain-DSN is not written to the file. The field in Vault KV is **`dsn`** (`vault kv put secret/keeper/postgres dsn="postgres://..."`). The convention is symmetrical between `signing_key` and `auth.jwt.signing_key_ref`. |
| `postgres.pool.min` | `int` (≥1) | `2` | Minimum pool size per instance. |
| `postgres.pool.max` | `int` (≥`min`) | `20` | Maximum pool size. Total flow to PG = `max × number_of_keeper_instances`. |

## `redis`

Connection to Redis - hot layer and coordination bus ([ADR-006](../adr/0006-cache-redis.md), [storage.md](storage.md)). The Keeper client supports three topologies natively - `mode: standalone | sentinel | cluster`; empty/omitted `mode` is treated as `standalone` (forward-compat for configs without a field).

```yaml
# Standalone (default): one node.
redis:
  mode: standalone               # can be omitted - this is default
  addr: "redis.internal:6379"
  password_ref: vault:secret/keeper/redis

# Sentinel: HA with automatic failover (recommended on-premise product path).
redis:
  mode: sentinel
  master_name: mymaster
  sentinels:
    - "sentinel-1.internal:26379"
    - "sentinel-2.internal:26379"
    - "sentinel-3.internal:26379"
  password_ref: vault:secret/keeper/redis                    # Redis node password
  sentinel_password_ref: vault:secret/keeper/redis#sentinel  # optional, password of the sentinel nodes themselves

# Cluster: slot sharding (horizontal scaling).
redis:
  mode: cluster
  nodes:
    - "redis-1.internal:6379"
    - "redis-2.internal:6379"
    - "redis-3.internal:6379"
  password_ref: vault:secret/keeper/redis
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `redis.mode` | `enum{standalone,sentinel,cluster}` | `standalone` (blank/omitted) | Redis topology. `standalone` - one node (`addr`). `sentinel` - Redis Sentinel HA (master-discovery via sentinel nodes). `cluster` - Redis Cluster (slot-routing natively in the client). Value out of set → diag `value_not_in_enum`. |
| `redis.addr` | `string(host:port)` | — | Node address. **Required** for `mode: standalone` (otherwise diag `missing_required_field`); when `sentinel`/`cluster` is ignored (diag `redis_unused_field`, warn). |
| `redis.master_name` | `string` | — (`optional`) | Monitored master group name for Sentinel. **Required** for `mode: sentinel`. In other modes, there is an extra field (warn). |
| `redis.sentinels` | `list<string(host:port)>` | — (`optional`) | Addresses of sentinel nodes. **Required (non-empty)** for `mode: sentinel`. Each element is validated as `host:port`. In other modes, there is an extra field (warn). |
| `redis.nodes` | `list<string(host:port)>` | — (`optional`) | Addresses of cluster nodes for bootstrap-discovery (the client itself will pull up the full topology and slot-map). **Required (non-empty)** for `mode: cluster`. Each element is validated as `host:port`. In other modes, there is an extra field (warn). |
| `redis.password_ref` | `vault-ref` or `string` | — | Redis password. `vault:<mount>/<path>[#field]` — resolved from Vault by the keeper-vault-client (default field `password`, override via `#field`); plaintext-string works as is (dev/tests); empty—connection without password. Vault-ref is validated by the semantic phase (`vault_ref_invalid` for a broken format). |
| `redis.sentinel_password_ref` | `vault-ref` or `string` | — (`optional`) | The password of the sentinel nodes themselves (separate from the Redis password). Same form and resolution as `password_ref`. Only makes sense with `mode: sentinel`. |

Vault KV-secret with password is placed under the field `password` (`vault kv put secret/keeper/redis password="<redis-password>"`); another field is selected by the suffix `#field` in the ref (for example `vault:secret/keeper/redis#sentinel`). If the field in KV is missing/empty, Keeper crashes fail-fast at startup (`password field missing or empty`); if ref starts with `vault:`, but the vault client is not up - `vault client is required`.

## `vault`

```yaml
# Dev/local: static token (root in dev-Vault).
vault:
  addr: "http://127.0.0.1:8200"
  token: "root"
  auth: { method: token }     # default; the auth block can be omitted entirely
  pki_mount: "pki"

# Cont: AppRole.
vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod                       # NOT a secret, you can do it online
    secret_id_file: /etc/keeper/vault-secret-id  # mode-restricted file (0400/0600)
    # OR instead of file:
    # secret_id_env: KEEPER_VAULT_SECRET_ID # name of env variable with secret_id
  pki_mount: "pki/soulstack"
```

Vault is a required Keeper dependency: Essence secrets, PKI for SoulSeed release, SSH-CA for `keeper.push`, signing key JWT (see [`auth:`](#auth)), cloud driver credentials ([requirements.md](../requirements.md)).

`vault.auth.method` selects the Keeper authentication method in Vault ([ADR-014](../adr/0014-operator-identity.md)):

- `token` (**default**) - static token from `vault.token`. Dev-shortcut: `dev/docker-compose.yml` raises Vault in dev mode with a root token. The `auth` block can be omitted entirely - this is the equivalent of `method: token` (forward-compat for existing `keeper.yml`).
- `approle` — prod-path: Keeper does `auth/approle/login` with `role_id` + `secret_id` and receives a renewable client-token, which is further renewed in the background (token auto-renew, [requirements.md](../requirements.md)).

**Where does `secret_id` come from.** AppRole-credentials are NOT read from Vault itself (no `vault:`-ref): with these credentials Keeper logs in, and then resolves the remaining `*_ref`-fields (`postgres.dsn_ref`, `auth.jwt.signing_key_ref`, ...) - this would be cyclic addiction. Therefore, the source is local, before the Vault client is raised:

- `role_id` - role identifier, **not a secret**; is set inline in `keeper.yml`.
- `secret_id` - **secret**; plaintext in `keeper.yml` is not provided by the schema. Exactly one source:
  - `secret_id_file` - path to the mode-restricted file (recommended `0400`/`0600`), contents = `secret_id` (trailing newline is removed);
  - `secret_id_env` is the name of the env variable with `secret_id` (CI / Vault Agent / k8s-secret-as-env).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `vault.addr` | `string` (URL) | — | Vault address. |
| `vault.token` | `string` | — | Static Vault-token for `method: token` (for example, `root` for local-dev). When `method: approle` cannot be set (conflict, validation error). |
| `vault.kv_mount` | `string` | `secret` | Mount point KV (without trailing slash). Used by the client to read the JWT signing-key and other secrets. Mount can be KV **v1 or v2** - the version is determined automatically (probe `sys/internal/ui/mounts/<mount>` on the first reading), there is no need to specify it in `kv_mount`. The reading contract is the same for both versions: the module/resolver receives a flat payload (for v2, the `data.data` wrapper is unpacked by the client). |
| `vault.kv_version` | `enum{"1","2"}` | — (auto) | Optional override version of KV mount. By default (empty/omitted) - **autodetection** via probe, the operator does not need to specify anything. Set **only** in a rare case when autodetection is closed: a hardened Vault, whose ACL policy does not allow reading `sys/internal/ui/mounts` (probe then fail-closed with an obvious error and a prompt to specify `kv_version`). Value out of set → diag `vault_kv_version_invalid`. Operations specific to KV v2 (list/metadata - orphan-reconcile Sigil) are not available by design on v1-mount. |
| `vault.auth.method` | `enum{token,approle}` | `token` | Keeper authentication method in Vault. Empty = `token`. |
| `vault.auth.role_id` | `string` | — | `role_id` AppRole (not a secret). Mandatory for `method: approle`. |
| `vault.auth.secret_id_file` | `string` (abs path) | — | Path to the mode-restricted file with `secret_id`. Mutually exclusive with `secret_id_env`; exactly one is required for `method: approle`. |
| `vault.auth.secret_id_env` | `string` | — | The name of the env variable with `secret_id`. Mutually exclusive with `secret_id_file`. |
| `vault.pki_mount` | `string` | — | Path of the PKI engine mount through which Keeper issues SoulSeed certificates. |
| `vault.pki_role` | `string` | `soul-seed` (optional) | The name of the PKI role in the specified mount. Vault signs the CSR via `<pki_mount>/sign/<pki_role>`. Provisioning role - see [docs/dev/local-setup.md → Vault PKI](../dev/local-setup.md). |

## `auth`

Operator Authentication (Archon) for OpenAPI / MCP. Subblocks:

- `jwt` - internal JWT-issuer ([ADR-014](../adr/0014-operator-identity.md)), signature and token format (actual part; below).
- `ldap` - federated LDAP authentication ([ADR-058](../adr/0058-operator-auth-ldap-oidc.md), stage 1).
- `oidc` - federated OAuth2/OIDC authentication ([ADR-058](../adr/0058-operator-auth-ldap-oidc.md), stage 2, requires Redis).
- `rate_limit` - anti-bruteforce login endpoints ([ADR-058(g)](../adr/0058-operator-auth-ldap-oidc.md)).

Federated methods (`ldap`/`oidc`) validate the external identity on Keeper, map it to the `operators(aid)` + RBAC roles line and release **internal** JWT - RBAC/MCP/middleware work without changes. All three subblocks are optional: not specified → login method is not available, Keeper starts. The operator registry and RBAC directory are in Postgres (`operators` and `rbac_*`, respectively, ADR-028), role management is through `role.*` API/MCP ([rbac.md](rbac.md)). Secret fields of federated blocks - via `*_ref` (`vault:<mount>/<path>[#field]`, resolve load-time as `redis.password_ref`).

```yaml
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 24h
    ttl_bootstrap: 720h        # 30d
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `auth.jwt.signing_key_ref` | `vault-ref` | `vault:secret/keeper/jwt-signing-key` | Vault KV-path to signing-key, which is used to sign operator JWTs (`iss`/`sub`/`iat`/`exp`/`roles`/`bootstrap_initial`). Post-MVP - Vault Transit without key export ([ADR-014(b)](../adr/0014-operator-identity.md)). |
| `auth.jwt.issuer` | `string` | `<kid>` | The value of claim is `iss` in issued JWTs. If there is no value, the parser substitutes the value of the `kid:` field of the instance ([ADR-014(b)](../adr/0014-operator-identity.md)). It is permissible to redefine - a single name per cluster instead of per-instance. |
| `auth.jwt.ttl_default` | `duration` | `24h` | TTL of regular operator tokens issued through `operator.issue-token`. Short TTL is a natural defense against revocation-blocklist ([ADR-014(d)/(tradeoffs)](../adr/0014-operator-identity.md)). |
| `auth.jwt.ttl_bootstrap` | `duration` | `720h` (30 days) | TTL of the first bootstrap token issued by `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md), [ADR-014(b)](../adr/0014-operator-identity.md)). |

If according to `signing_key_ref` there is no key in Vault at the time Keeper starts, there is an implementation fork ([ADR-014, section Consequences](../adr/0014-operator-identity.md): "either Keeper generates it itself and puts it at `keeper init`, or refuses to start"). Before closing with a separate task, normative behavior was not recorded.

### `auth.ldap` - federated LDAP authentication ([ADR-058](../adr/0058-operator-auth-ldap-oidc.md))

Optional block. If present, `POST /auth/ldap/login` is mounted (outside `/v1`): Keeper validates login/password against LDAP, maps external groups to RBAC roles (`group_role_map`), upon the first successful login, auto-provision line `operators` (`created_via='ldap'`, `auth_method='ldap'`) and releases **internal** JWT in HttpOnly+Secure+`SameSite=Strict` cookie `soul_session` (further RBAC/MCP/middleware work without changes). Not specified → login method is not available, Keeper starts ([ADR-053](../adr/0053-dependency-tiers.md) OPTIONAL-tier).

Implemented only **search-bind** (`bind_mode: search`, default): service-account (`bind_dn` + `bind_password_ref`) searches for user by `user_filter`, then bind with verified credentials. `bind_mode: direct` (field `user_dn_template`) deferred (stage 1) - any value of `bind_mode` outside `{"", search}` is rejected at start (`ldap_bind_mode_unsupported`). **TLS required**: either `ldaps://` or `ldap://` + `start_tls: true` (mutually exclusive); plaintext-LDAP is rejected at start.

```yaml
auth:
  ldap:
    url: ldaps://ldap.corp.internal:636   # ldaps:// | ldap:// (the latter requires start_tls)
    # start_tls: true # StartTLS over ldap://; mutually exclusive with ldaps://
    bind_mode: search                       # search only (default); direct is postponed
    bind_dn: "cn=keeper,ou=svc,dc=corp,dc=internal"
    bind_password_ref: vault:secret/keeper/ldap-bind   # field in KV - password
    base_dn: "ou=people,dc=corp,dc=internal"
    user_filter: "(uid=%s)"
    group_filter: "(member=%s)"
    group_attr: cn
    aid_attr: uid                           # user attribute → AID
    timeout: 5s
    tls:
      ca_ref: vault:secret/keeper/ldap-ca   # opt. CA-bundle for LDAPS (field in KV - ca)
      # insecure_skip_verify: true # dev-only, at start WARN
    group_role_map:
      "cn=ops,ou=groups,dc=corp,dc=internal":   [operator]
      "cn=admins,ou=groups,dc=corp,dc=internal": [cluster-admin]
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `auth.ldap.url` | `string` (URL) | — | `ldaps://host:636` or `ldap://host:389`. plaintext (`ldap://` without `start_tls`) is rejected at start. |
| `auth.ldap.start_tls` | `bool` | `false` | StartTLS over `ldap://`. Mutually exclusive with `ldaps://` (`ldaps://` already encrypts the channel) - the conflict is rejected at the start. |
| `auth.ldap.bind_mode` | `enum{search}` | `search` | `search` only (or omitted = `search`). `direct` deferred (stage 1): value outside `{"", search}` → diag `ldap_bind_mode_unsupported`. |
| `auth.ldap.bind_dn` | `string` (DN) | — | DN service-account for search-bind. Mandatory for `bind_mode: search` (otherwise `ldap_search_requires_bind_dn`). |
| `auth.ldap.bind_password_ref` | `vault-ref` | — | Vault-reference to the service-account password (field in KV - `password`). Mandatory for `bind_mode: search` (`ldap_search_requires_bind_password`). Plaintext is not provided in the config. |
| `auth.ldap.base_dn` | `string` (DN) | — | User search database. |
| `auth.ldap.user_filter` | `string` | — | LDAP user search filter, `%s` is substituted with login (for example `(uid=%s)`). |
| `auth.ldap.user_dn_template` | `string` | — | DN pattern for `bind_mode: direct` (for example `uid=%s,ou=people,...`). The field is reserved (direct is deferred), `bind_mode` is not activated. |
| `auth.ldap.group_filter` | `string` | — | User group search filter (`%s` = user-DN), for example `(member=%s)`. |
| `auth.ldap.group_attr` | `string` | — | An attribute of the found group, the value of which is keyed in `group_role_map` (for example, `cn`). |
| `auth.ldap.aid_attr` | `string` | `uid` | A user attribute whose value becomes the operator's AID. |
| `auth.ldap.timeout` | `duration` | — | LDAP operations timeout. |
| `auth.ldap.tls.ca_ref` | `vault-ref` | — | Opt. Vault-reference to CA-bundle for LDAPS certificate verification (field in KV - `ca`). |
| `auth.ldap.tls.insecure_skip_verify` | `bool` | `false` | Disables TLS certificate verification. **dev-only**: `true` → WARN at start (`ldap_insecure_skip_verify`). |
| `auth.ldap.group_role_map` | `map<string, list<string>>` | — | External group (value `group_attr`) → list of RBAC roles that the operator will receive. Source of roles for auto-provision and reconciliation on each login. |

### `auth.oidc` - federated OAuth2/OIDC authentication ([ADR-058](../adr/0058-operator-auth-ldap-oidc.md))

Optional block. If available (**and running Redis**), `GET /auth/oidc/login` and `GET /auth/oidc/callback` are mounted (outside `/v1`): authorization-code flow with discovery (`go-oidc`), JWKS validation `id_token`, mapping `groups_claim`→roles and auto-provision (`created_via='oidc'`, `auth_method='oidc'`); the final internal JWT is in cookie `soul_session` (`SameSite=Lax` - cross-site redirect from IdP). **PKCE (S256) is always enabled** by the implementation - it is not left to the operator to choose from, there is no config flag `use_pkce`. Flow-state (state→nonce/verifier) ​​is stored in Redis (single-use, TTL 5m), so endpoints are not mounted without Redis ([ADR-053](../adr/0053-dependency-tiers.md) OPTIONAL-tier). `issuer` - HTTPS only.

```yaml
auth:
  oidc:
    issuer: https://idp.corp.internal/realms/soul-stack    # discovery base, https only
    client_id: soul-stack-keeper
    client_secret_ref: vault:secret/keeper/oidc-client      # opt. (public-client may not have); field in KV - client_secret
    redirect_url: https://keeper.corp.internal/auth/oidc/callback
    scopes: [openid, email, profile, groups]
    aid_claim: sub                # sub | email | preferred_username (default sub)
    groups_claim: groups
    tls:
      ca_ref: vault:secret/keeper/oidc-ca    # opt. custom CA IdP (field in KV - ca)
    group_role_map:
      ops:    [operator]
      admins: [cluster-admin]
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `auth.oidc.issuer` | `string` (https URL) | — | Base URL of the OIDC provider for discovery (`/.well-known/openid-configuration`). HTTPS only (otherwise diag at start). Required. |
| `auth.oidc.client_id` | `string` | — | OAuth2 client-id registered with the IdP. Required (`oidc_client_id_required`). |
| `auth.oidc.client_secret_ref` | `vault-ref` | — | Opt. Vault-reference to client-secret (field in KV - `client_secret`); public-client omits it without a secret. Plaintext is not provided - non-vault-ref is rejected at the start. |
| `auth.oidc.redirect_url` | `string` (URL) | — | Callback-URL registered with IdP; must point to `…/auth/oidc/callback` of this Keeper. Required (`oidc_redirect_url_required`). |
| `auth.oidc.scopes` | `list<string>` | — | OAuth2 request scopes (typically `openid`, `email`, `profile`, `groups`). |
| `auth.oidc.aid_claim` | `string` | `sub` | Claim `id_token`, whose value becomes AID. Default `sub` (immutable subject). Mutable-claim (`email`/`preferred_username`) → WARN at start (`oidc_aid_claim_mutable`, identity-spoofing risk when reassigning). |
| `auth.oidc.groups_claim` | `string` | `groups` | Claim `id_token` with a list of groups keyed in `group_role_map`. |
| `auth.oidc.tls.ca_ref` | `vault-ref` | — | Opt. Vault-reference to custom CA IdP (field in KV - `ca`). |
| `auth.oidc.group_role_map` | `map<string, list<string>>` | — | External group (value from `groups_claim`) → list of RBAC roles. Source of roles for auto-provision and reconciliation on login. |

### `auth.rate_limit` - anti-bruteforce login endpoints ([ADR-058(g)](../adr/0058-operator-auth-ldap-oidc.md))

Optional block. Protection of public login paths (`/auth/ldap/login`, `/auth/oidc/login`) from brute force: per-IP + per-username throttle the frequency of attempts (token-bucket) **plus** lockout after a series of failures. **Default-ON** (nil/omitted → enabled, footgun-guard as `tempo`/`toll`); explicit `enabled: false` - opt-out. In fact, it only rises when Redis is running (limiter cluster-shared is the authority in Redis, not ×N for stateless instances); Without Redis, login paths degrade without throttle. Lockout check **fail-closed** for Redis error (login is a security perimeter, Redis inaccessibility should not open brute force). Excess → `429` + `Retry-After` + `application/problem+json` with type [`auth-throttled`](../naming-rules.md#error-codes) (detail anti-oracle - without specifying scope/reason). Hot-reloadable. Primitive - **LoginGuard** (`keeper/internal/redis/loginguard.go`).

```yaml
auth:
  rate_limit:
    enabled: true              # nil/omitted → true (default-ON); false → opt-out
    rate: 0.5                  # attempts/sec per principal (token-bucket refill)
    burst: 10                  # retry bucket depth
    lockout_threshold: 5       # failures in the window before locking the principal
    lockout_window: 15m        # failure count window
    lockout_backoff: 15m       # blocking duration after threshold
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `auth.rate_limit.enabled` | `bool` (tri-state) | `nil` → `true` | Turning on. Omitted/`null` → default-ON; explicit `false` → opt-out (dev). The actual lifting requires Redis. |
| `auth.rate_limit.rate` | `float` (attempts/sec) | `0.5` | Refill rate of token-bucket attempts per principal (IP **or** username). `0`/omitted → default. |
| `auth.rate_limit.burst` | `int` | `10` | Depth of the bucket of attempts (one-time burst). `0`/omitted → default. |
| `auth.rate_limit.lockout_threshold` | `int` | `5` | The number of failed logins in the window after which the principal is blocked. `0`/omitted → default. |
| `auth.rate_limit.lockout_window` | `duration` | `15m` | Sliding window for counting failures. Empty → default. |
| `auth.rate_limit.lockout_backoff` | `duration` | `15m` | Duration of blocking after reaching the threshold. Empty → default. |

### Operator Provisioning Policy - `provisioning_allowed_methods` ([ADR-058(i)](../adr/0058-operator-auth-ldap-oidc.md))

The **CREATE** method of the operator (`created_via`) can be limited by the `provisioning_allowed_methods` policy - this is the **well-known key `keeper_settings` in Postgres**, and not the `keeper.yml` field (control via API/MCP, not by file editing, symmetrical to the Service registry). CSV from domain `{user, ldap, oidc}`: gates only the creation branch (`POST /v1/operators` → `user`; federated auto-provision → `ldap`/`oidc`). Existing operators log in regardless of policy; `bootstrap`/`system` **never gated** (not included in the domain).

- **Key missing** → all methods are allowed (back-compat).
- **The key has been specified, but is empty** → config-error (anti-lockout: it is impossible to prohibit ALL methods and lock the establishment of operators).
- Method outside `{user,ldap,oidc}` → failure.

Runtime management - endpoints `GET`/`PUT /v1/provisioning-policy` (permissions `provisioning.read` / `provisioning.update`, [rbac.md → Provisioning](rbac.md#provisioning-2--adr-058)). `GET` without audit (`policy_set=false` → key not set = default "everything is allowed"); `PUT` — replace semantics, writes audit `provisioning.policy_changed`, empty list → `422` anti-lockout, creation method prohibited by policy → `422` [`provisioning-method-disabled`](../naming-rules.md#error-codes). See [storage.md](storage.md) (`keeper_settings`), [naming-rules.md → `provisioning_allowed_methods`](../naming-rules.md).

**Soul does not have the `auth:` block** - Soul authenticates to Keeper via mTLS / SoulSeed, see [`docs/soul/identity.md`](../soul/identity.md). JWT - only for operators (OpenAPI/MCP).

**What's NOT in `auth:`:**

- Archon Registry - in Postgres (`operators`, [storage.md](storage.md), [ADR-014(a)](../adr/0014-operator-identity.md)).
- Roles and permissions - in Postgres (`rbac_*`, ADR-028), managed via `role.*` API/MCP ([rbac.md](rbac.md)).
- Bootstrap semantics (first Archon, `--initialize`) - [ADR-013](../adr/0013-bootstrap-archon.md), [rbac.md → Bootstrap of the first Archon](rbac.md).
- mTLS for machine-identity and `combined` auth-method - post-MVP, extending `auth_method` enum to `operators` without breaking change.

## `metrics`

Optional block of settings for the `/metrics` endpoint (bind address - `listen.metrics.addr`, above). In MVP it carries only opt. endpoint protection; in the absence of the block `/metrics` is served without auth.

```yaml
metrics:
  auth:
    basic:
      enabled: true
      username: scrape
      password_ref: vault:secret/keeper/metrics-password
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `metrics.auth.basic.enabled` | `bool` | `false` | Enable HTTP Basic-auth on `/metrics`. |
| `metrics.auth.basic.username` | `string` | — | Username. Mandatory for `enabled: true` (otherwise diag `missing_required_field`). |
| `metrics.auth.basic.password_ref` | `vault-ref` | — | Link `vault:<mount>/<path>` to a secret with field `password`. Resolved by the same keeper-vault client that reads the JWT signing-key. **Plaintext password not allowed** ("security first"): non-vault-ref → diag `vault_ref_invalid`. Mandatory for `enabled: true`. |

Password is compared constant-time ([`subtle.ConstantTimeCompare`](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). The resolved password and `password_ref` are not logged and do not end up in config-dump.

> **Soul does not have symmetric auth.** The Soul agent does not have a vault client ([ADR-012](../adr/0012-keeper-soul-grpc.md)) to resolve `password_ref`; Soul metrics are protected by loopback (`metrics.listen` = `127.0.0.1`, [soul/config.md](../soul/config.md#metrics)). Auth for Soul is a separate future task.

## `otel`

```yaml
otel:
  enabled: true
  exporter: otlp
  endpoint: "otel-collector.internal:4317"
  export_metrics: false
```

OpenTelemetry - end-to-end requirement ([requirements.md](../requirements.md)). End-to-end traces: operator → Keeper → Soul via propagation in gRPC metadata.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `otel.enabled` | `bool` | `false` | Enable export. |
| `otel.exporter` | `enum{otlp}` (MVP) | `otlp` | Export format. |
| `otel.endpoint` | `string(host:port)` | — | OTel collector address (gRPC). Mandatory for `enabled: true`. |
| `otel.export_metrics` | `bool` | `false` | Opt. push metrics by OTLP in addition to Prometheus-scrape ([ADR-024 §1.2](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) / [observability.md §5](../observability.md)). **Stub for Slice 2:** the field is read, but the OTLP-metrics-pipeline is not raised yet - only traces are exported to Slice 0. By default, metrics go only through Prometheus-`/metrics`. |

For `enabled: true`, the `endpoint` field is required; with `enabled: false` the entire block can be omitted.

## `logging`

```yaml
logging:
  level: info
  format: json
  file: /var/log/keeper/keeper.log    # empty/omitted → stderr without rotation
  rotation:
    max_size_mb: 100
    max_age_days: 7
    max_files: 10
    compress: true
```

Behavior depends on `logging.file` (symmetrical to Soul-side, see [`../soul/config.md → logging:`](../soul/config.md#logging)):

- **`logging.file` not specified** → output to `stderr` without rotation (dev mode, convenient for systemd/journald and in a container).
- **`logging.file` set** → write to this file with built-in rotation (general builder [`shared/log`](../adr/0011-go-layout.md)); archives are stacked side by side according to the pattern `<file>-<timestamp>.<ext>`.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `logging.level` | `enum{debug,info,warn,error}` | `info` | Logging level. |
| `logging.format` | `enum{json,text}` | `json` | `json` for machine processing, `text` for human processing. |
| `logging.file` | `string` (path) | —(stderr) | Path to the log file. Empty - output to `stderr` without rotation. Must be absolute; relative → diag `path_not_absolute`. |
| `logging.rotation.max_size_mb` | `int` (MB) | `100` | Rotation threshold for one file. |
| `logging.rotation.max_age_days` | `int` (≥0) | `7` | How many days to store the rotated file. Empty/`0` → builder default (7 days); "without age limit" is not expressed in the current grammar (MVP restriction). |
| `logging.rotation.max_files` | `int` | `10` | How many archives to keep? |
| `logging.rotation.compress` | `bool` | `true` | Whether to compress archives. In MVP, `false` does not disable compression (always `true`); the shutdown will appear later. |

Built-in default rotation ([requirements.md](../requirements.md)) - no dependency on external logrotate. The `logging.rotation.*` fields only apply when `logging.file` is specified.

## Service registry and `default_destiny_source` - in Postgres

Service registry (`services[]`) and scalar `default_destiny_source` **moved to Postgres** ([ADR-029](../adr/0029-service-registry.md)): the source of truth is tables `service_registry` + `keeper_settings` ([storage.md](storage.md)), not `keeper.yml`. Management - via `service.*` OpenAPI/MCP ([operator-api.md](operator-api.md)), and not by editing the file; runtime reads in-memory snapshot (`serviceregistry.Holder`, TTL-poll + pub/sub-invalidation). The keys `services:`, `default_destiny_source:` and `default_module_source:` in `keeper.yml` are **no longer accepted** - rejected as `unknown_key` (see section ["`services` / `default_destiny_source` / `default_module_source`"](#services--default_destiny_source--default_module_source) below).

`default_module_source` was abolished without replacement - the field did not have a consumer (resolving modules through it is not implemented).

Resolve `apply: { destiny: <name> }` (ADR-009, isolated render pass) does not change semantically - **source hybrid** (per-entry git override):

1. `<name>` is searched in `service.yml → destiny[]` of the loaded service snapshot → the `{name, ref, git?}` entry is taken (only the declared dependency; otherwise an error). `ref` is taken from the record in both cases.
2. git-URL by hybrid rule:
   - entry carries `git:` → it is used directly (override; `default_destiny_source` is ignored);
   - there is no `git:` entry → git-URL = `default_destiny_source` (from `keeper_settings`) with `{name}` substitution. Empty / not specified `default_destiny_source` at this step → resolution error (there is nowhere to put the name).
3. destiny is loaded as a separate immutable snapshot, rendered with OWN `input:` (resolved `apply.input`), its tasks are pasted into the parent's plan. scenario-scope (input/vars/register/soulprint) is NOT visible in destiny - structural isolation boundary.

See also [architecture.md → Service](../architecture.md), [storage.md → service_registry / keeper_settings](storage.md).

## `plugins`

Plugin directory with host = `keeper` ([plugins.md](plugins.md)). Five scalars (`cache_root` / `work_root` / `fetch_timeout` / `max_artifact_size_mb` / `max_clone_size_mb`) + two directory subblocks (`cloud_drivers` / `ssh_providers`).

### `plugins.cache_root`

```yaml
plugins:
  cache_root: /var/lib/soul-stack-keeper/plugins
```

The root of the plugin artifact cache on the keeper-host (the path where the git resolver `plugins.{cloud_drivers,ssh_providers}` lays out the collected binaries/manifests). Discovery host ([`keeper/internal/pluginhost`](../../keeper/internal/pluginhost/pluginhost.go)) scans this directory when the keeper starts.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugins.cache_root` | `path` (absolute) | `/var/lib/soul-stack-keeper/plugins` (built-in `pluginhost.DefaultCacheRoot`) | Optional. Must be absolute; relative-path schema-phase rejects with `path_not_absolute`. Empty value / no key - the built-in default is used. Env-override `KEEPER_PLUGIN_CACHE_DIR` (dev/CI) is only applied if there is no value in `keeper.yml` (precedence: yaml > env > default). |

### `plugins.work_root`

```yaml
plugins:
  work_root: /var/lib/soul-stack-keeper/plugin-src
```

Root of working git clones of plugin resolver ([ADR-026](../adr/0026-sigil.md) F-fetch). At startup, Keeper git resolves `plugins.{cloud_drivers,ssh_providers}` into this directory (clone/fetch + checkout via **go-git**, without depending on the system binary `git`), then extracts the collected artifact `dist/<binary-name>` into the commit_sha cache slot.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugins.work_root` | `path` (absolute) | `/var/lib/soul-stack-keeper/plugin-src` | Optional. Must be absolute (`path_not_absolute`). **STRICTLY outside `cache_root`** - otherwise `.git`/checkout would end up in the cache directory read by Discovery/ReadSlot (schema phase rejects with `plugins_work_root_within_cache_root`). Env-override `KEEPER_PLUGIN_WORK_DIR` (dev/CI), priority: yaml > env > default. |

### `plugins.fetch_timeout`

```yaml
plugins:
  fetch_timeout: 120s
```

The ceiling of one chain of git operations for resolving a plugin (clone/fetch → resolve → checkout, via go-git). git-egress - external call, timeout required.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugins.fetch_timeout` | `duration` | `120s` (`config.DefaultPluginFetchTimeout`) | Optional. The format is `duration` (Go-`time.ParseDuration` or `<N>d`), validated by the semantic phase (`duration_invalid`). Empty / incorrect → default. |

### `plugins.max_artifact_size_mb` / `plugins.max_clone_size_mb`

```yaml
plugins:
  max_artifact_size_mb: 256    # ceiling of one binary dist/<binary-name>, default 256 MiB
  max_clone_size_mb: 1024      # clone working tree ceiling (checkout + .git), default 1024 MiB
```

Size limits git-egress hardening ([ADR-026(g)](../adr/0026-sigil.md)). `source` directory is operator-asserted, but the repository itself is **untrusted**: `fetch_timeout` limits git-egress **by time**, but not by volume - a hostile/huge repository could clog `work_root` + `cache_root` keeper-host disk (DoS). These two fields cap **by volume**: `max_clone_size_mb` is measured against the working tree (du-like walk checkout + `.git`) **before** extracting the artifact, `max_artifact_size_mb` is measured against the `dist/<binary-name>` binary before copying to the cache. Exceeding - **fail-closed**: the slot is not created (the plugin has nothing to allow through Sigil), and for the clone limit, `work_root/<name>` is additionally cleared (sentinels `ErrCloneTooLarge` / `ErrArtifactTooLarge`).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugins.max_artifact_size_mb` | `int` (MiB, ≥1) | `256` (`config.DefaultPluginMaxArtifactSizeMB`) | Optional. The size ceiling for one extracted binary is `dist/<binary-name>`. `0`/omitted → default; `<1` → diag `value_out_of_range` (the submegabyte ceiling would reject any real Go plugin binary). Excess on resolve → `ErrArtifactTooLarge`, slot is not created. |
| `plugins.max_clone_size_mb` | `int` (MiB, ≥1) | `1024` (`config.DefaultPluginMaxCloneSizeMB`) | Optional. Ceiling of the total size of the clone working tree (checkout + `.git`), measured before the artifact is extracted. `0`/omitted → default; `<1` → diag `value_out_of_range`. Excess → `ErrCloneTooLarge` + cleanup `work_root/<name>`. Obviously more than the artifact limit (the tree carries the artifact itself plus other files and shallow-`.git`). |

### `plugins.cloud_drivers`

```yaml
plugins:
  cloud_drivers:
    - { name: aws, source: "git@github.com:soul-stack-ecosystem/soul-cloud-aws.git", ref: v2.0.0 }
    - { name: yc,  source: "git@github.com:our-company/soul-cloud-yc.git",          ref: v0.3.1 }
```

CloudDriver plugins (`soul-cloud-<provider>`), used by [`keeper.cloud`](cloud.md).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugins.cloud_drivers[].name` | `string` (kebab-case) | — | Provider name to link from Provider to Postgres (`type=<name>`, [cloud.md](cloud.md)). |
| `plugins.cloud_drivers[].source` | `git-url` | — | git-URL of the plugin repository. |
| `plugins.cloud_drivers[].ref` | `git-ref` | — | git tag or branch ([ADR-007](../adr/0007-versioning-git-ref.md)). |

### `plugins.ssh_providers`

```yaml
  ssh_providers:
    - { name: vault-ssh, source: "git@github.com:soul-stack-ecosystem/soul-ssh-vault.git", ref: v1.0.0 }
    - { name: static,    source: "git@github.com:soul-stack-ecosystem/soul-ssh-static.git", ref: main }
```

SshProvider plugins (`soul-ssh-<provider>`), used by [`keeper.push`](push.md).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugins.ssh_providers[].name` | `string` (kebab-case) | — | The name of the provider that the push operation refers to when selecting SSH authentication ([push.md](push.md)). |
| `plugins.ssh_providers[].source` | `git-url` | — | git-URL of the plugin repository. |
| `plugins.ssh_providers[].ref` | `git-ref` | — | git tag or branch ([ADR-007](../adr/0007-versioning-git-ref.md)). |

## `plugin_runtime`

```yaml
plugin_runtime:
  socket_dir: /var/run/soul-stack-keeper/plugins
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false
```

Lifecycle host process for plugins running on the Keeper side (`cloud_driver`, `ssh_provider`): handshake and shutdown timeouts, whitelist capabilities and resource conflict policy, optional TLS on the plugin socket. Full lifecycle semantics, handshake string format, plugin launch diagram - [plugins.md → Lifecycle](plugins.md#lifecycle); regulatory decision - [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugin_runtime.socket_dir` | `path` | `/var/run/soul-stack-keeper/plugins/` | The directory in which the host creates Unix-domain plugin sockets (`<namespace>-<name>-<pid>.sock`). Created with mode `0700`, owned by service user `keeper` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md)). The path is different for Keeper-host (`soul-stack-keeper`) and Soul-host (`soul-stack`), see [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime). |
| `plugin_runtime.startup_timeout` | `duration` | `10s` | Time from `fork()` plugin process until handshake line `"soul_stack":"plugin-v1"` appears in stdout. Exceeding - the host sends SIGTERM, then SIGKILL after `shutdown_grace` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md), [plugins.md → Host behavior during handshake](plugins.md)). |
| `plugin_runtime.shutdown_grace` | `duration` | `10s` | Time from SIGTERM to SIGKILL. The SDK provides a signal-handler, the plugin must close the in-flight RPC and terminate itself within this window ([ADR-020(d)](../adr/0020-plugin-infrastructure.md)). |
| `plugin_runtime.allowed_capabilities` | `list<enum>` | all 6 capabilities (see YAML-block above) | Closed enum (full catalog - [plugins.md → required_capabilities-table](plugins.md), [ADR-020(f)](../adr/0020-plugin-infrastructure.md)). Whitelist: `soul-lint` rejects destiny **before launch** if `manifest.required_capabilities` plugin ⊄ this list. Default allows all six; the operator narrows it down according to security policy. The parser rejects values ​​outside the closed enum with `unknown_capability`. |
| `plugin_runtime.conflict_policy` | `enum{warn,fail}` | `warn` | Policy for the case when two plugins in the same run claim the same resource in `side_effects` (same pair `<resource_type>:<value>`). `warn` — host writes audit-event and continues running; `fail` - the step is marked `failed`, the reason `policy_violation` is reflected in the diagnostic channel `TaskEvent` / `RunResult` ([ADR-020(g)](../adr/0020-plugin-infrastructure.md), [plugins.md → Host behavior on side_effects](plugins.md)). |
| `plugin_runtime.enable_tls` | `bool` | `false` | Enabling mTLS on a plugin socket. In MVP - `false`: security is provided by file-permissions `0700` on Unix-socket ([ADR-020(h)](../adr/0020-plugin-infrastructure.md)). Post-MVP - `true` uses the `server_cert` (base64-PEM) field of the handshake string, already reserved by forward-compat-reserve. Before closing a separate task, the behavior at `true` is rejected by the parser with `tls_not_implemented`. |

### Hot-reload block `plugin_runtime:`

Hot-reload config - end-to-end requirement ([requirements.md](../requirements.md)). The general reload mechanism is standardized in [ADR-021](../adr/0021-hot-reload-config.md), see [§ Hot-reload](#hot-reload) below. Per-field policy block `plugin_runtime:`:

| Field | Reload without restarting the host process | Rationale |
|---|---|---|
| `allowed_capabilities` | yes | Parameter of a specific plugin launch: host reads the value during fork, new runs see the new value. |
| `conflict_policy` | yes | The same: the evaluation of the conflict `side_effects` occurs at the time the run is assembled, in-memory. |
| `startup_timeout` | yes | Applies to new plugin runs that do not affect those already running. |
| `shutdown_grace` | yes | Same. |
| `socket_dir` | **no, requires a restart** | Changes the external surface of the host (file-system layout); already running plugin sockets are in the old directory. |
| `enable_tls` | **no, requires a restart** | Changes the TLS handshake chain of the plugin protocol. |

Rule: change-without-restart what is used as a parameter of a specific plugin run; require-restart is what changes the external surface of the host.

## `sigil`

Plugin approval signature - **Sigil** trust seal ([ADR-026](../adr/0026-sigil.md), [plugins.md → Integrity-model](plugins.md#integrity-model)). Optional block: if `signing_key_ref` is missing (or empty), the signature is not available - Keeper starts normally, but the allow operation (allowing the plugin by the Archon) will return the error "sigil key not configured". Loading the nil-safe key.

```yaml
sigil:
  signing_key_ref: vault:secret/keeper/sigil-signing-key
# sigil_anchors_reload_interval: 30s   # TTL-fallback re-reading a set of anchors (top-level)
```

| Field | Type | Default | Description |
|---|---|---|---|
| `sigil.signing_key_ref` | `vault-ref` | - (optional) | Vault KV-path to **ed25519-private**, with which Keeper signs the Sigil block (the field in Vault KV is **`signing_key`**, as in `auth.jwt.signing_key_ref`). The key is **asymmetric** (ed25519), in contrast to the HS256-symmetric JWT signing-key: the private part signs to Keeper, the public part goes to Soul in bootstrap as a trust-anchor for verify ([ADR-026(d)](../adr/0026-sigil.md)). Valid forms of the value in KV are PEM (PKCS#8), base64(DER), base64/raw 64-byte `seed‖pub` or 32-byte seed. **Plaintext key is prohibited in the config** ("security comes first"): non-vault-ref → diag `vault_ref_invalid`. The format of the signed block is [plugins.md → Format of the signed block](plugins.md). |

### `sigil_anchors_reload_interval` (top-level)

| Field | Type | Default | Description |
|---|---|---|---|
| `sigil_anchors_reload_interval` | `duration` | `30s` | **TTL-fallback-reread** period of the Sigil signature trust-anchor key set ([ADR-026(h)](../adr/0026-sigil.md), R3). Channel `sigil:anchors-changed` (Redis pub/sub) - best-effort; a missed signal would leave the lagging node with the old set of anchors until the restart (fail-open with Retire). Periodic re-read (`reloadAnchors` by ticker, sample TTL-poll RBAC / Summons poll-fallback) self-heals skipping the interval. The tick rises **independently of Redis**: when Redis is turned off (single-instance / dev), this is the only way along which the runtime rotation reaches without restarting. The format is validated in the semantic phase; empty/`0`/incorrect → default. The key is **top-level** (style `acolyte_*`), not nested in `sigil:`. |

## `reaper`

```yaml
reaper:
  enabled: true
  interval: 1h
  dry_run: false
  batch_size: 500
  lock_ttl: 5m
  rules:
    expire_pending_seeds: { enabled: true, max_age: 24h, action: delete }
    # … other rules
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `reaper.enabled` | `bool` | `true` | Turn on the Reaper. |
| `reaper.interval` | `duration` | `1h` | Passage interval. |
| `reaper.dry_run` | `bool` | `false` | Dry run without mutations. |
| `reaper.batch_size` | `int` | `500` | Batch size of one pass. |
| `reaper.lock_ttl` | `duration` | `5m` | TTL Redis-lease for leadership ([ADR-006](../adr/0006-cache-redis.md)). |
| `reaper.rules` | `map<string, object>` | — | Cleaning rules. The structure of each rule (fields, types, conditional mandatoryness according to `action`) is normatively defined in [reaper.md → Rule structure](reaper.md); directory of predefined rules and binding to tables - [reaper.md → Rules](reaper.md). The `reclaim_apply_runs` rule is **disabled** by default and is enabled only under the gate - see [reaper.md → Enabling recovery](reaper.md) (WARN: do not enable when `acolytes: 0`). Cadence spawn (`spawn_due_cadence` / `action: spawn`) **is no longer in Reaper** - it went to the [Conductor](conductor.md) subsystem ([ADR-048](../adr/0048-conductor.md)); see block [`cadence_scheduler`](#cadence_scheduler). |

## `cadence_scheduler`

Subsystem config [Conductor](conductor.md) - leader-elected executor of [Cadence](../naming-rules.md)-schedules ([ADR-048](../adr/0048-conductor.md)). Conductor, based on its tick, selects mature Cadence and spawns a regular Voyage run; lease `conductor:leader` is independent from `reaper:leader`. Block **optional** - if absent, defaults apply + default-ON when Redis (footgun-guard) is configured. Full description of behavior, including [adaptive polling step](conductor.md) and [floor minimum Cadence period](conductor.md) - [conductor.md](conductor.md).

**Adaptive polling step** ([ADR-048 "Adaptive interval"](../adr/0048-conductor.md)), not fixed: before each tick the leader outputs a step from the Cadence enabled register - `clamp(min(periods of enabled schedules), poll_floor, poll_ceiling)`; cron rules contribute 60s; empty enabled registry → `poll_idle`. The default profile "Calm" is 30s / 60s / 120s.

```yaml
cadence_scheduler:
  enabled: true        # nil/omitted → ON when Redis is configured; false → OFF
  poll_floor: 30s      # lower bound of adaptive polling step
  poll_ceiling: 60s    # upper bound of adaptive polling step
  poll_idle: 120s      # polling step when Cadence enabled registry is empty
  lock_ttl: 5m         # TTL Redis-lease conductor:leader
  # interval: 60s # backcompat-alias poll_ceiling; new configs write poll_*
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `cadence_scheduler.enabled` | `bool` (optional, tri-state) | `nil` → ON with Redis | Enable Conductor. **Omitted / `null`** → default-ON if Redis is present ([footgun-guard ADR-048 §5](../adr/0048-conductor.md): Cadence does not silently spawn without a running scheduler); explicit **`false`** → Conductor does not rise; explicit **`true`** → raised (requires Redis for lease leadership). Disabling an individual schedule - per-Cadence `enabled: false` ([ADR-046 §3](../adr/0046-cadence.md)), not global blanking here. **Read at start** (not hot-reload) - the change requires an instance restart. |
| `cadence_scheduler.poll_floor` | `duration` | `30s` | The lower limit of the adaptive survey step (the "Quiet" profile). Coincides with the floor-limit of the minimum period Cadence - the same key, single source 30s (write-path floor-reject `interval_seconds ≥ poll_floor` reads the same resolve, see [conductor.md → Floor](conductor.md)). **Absolute minimum**: `< 30s` → diag `value_out_of_range` at the start (the sub-30s period is meaningless - downstream will not work more accurately, the reactive domain is Beacons). Empty/invalid → default. **Hot-reload** (re-read at every tick from a fresh Store snapshot). |
| `cadence_scheduler.poll_ceiling` | `duration` | `60s` | Upper bound on adaptive polling step: the sparse schedule (`interval=1h`) does not stretch the polling so that the missed-slot mechanism becomes the only insurance. Invariant `poll_floor ≤ poll_ceiling` (aka `value_out_of_range`). Empty/invalid → default. **Hot-reload**. |
| `cadence_scheduler.poll_idle` | `duration` | `120s` | Polling step with **empty enabled registry** Cadence (there is nothing to spawn - polling is less frequent than the corridor, not in vain). Invariant `poll_idle ≥ poll_ceiling` (otherwise `value_out_of_range`: idle no more often than normal polling). Empty/invalid → default. **Hot-reload**. |
| `cadence_scheduler.interval` | `duration` | — (alias) | **Backcompat-alias** `poll_ceiling`. Before the amendment, 2026-06-07 was a fixed tick period; Now the step is adaptive, `interval` is abandoned for the sake of the old `keeper.yml`. If `poll_ceiling` is **not** set → `poll_ceiling = max(interval, poll_floor)` (clamp up to floor). Sub-floor `interval` (for example, the previous dev-config with `5s`) **does not drop the config**: rises to floor with WARNING (`value_clamped`, a hint about Beacons for sub-30s). If `interval` and `poll_ceiling` are specified simultaneously, `poll_ceiling` wins. New configs write `poll_*`. **Hot-reload**. |
| `cadence_scheduler.lock_ttl` | `duration` | `5m` | TTL Redis-lease `conductor:leader` ([ADR-006](../adr/0006-cache-redis.md)), parity `reaper.lock_ttl`. Large enough to survive a leader's temporary stall; short enough for quick failover; renew to `lock_ttl/3`. Empty/`0`/invalid → default. **Hot-reload** (used between re-acquire leases). |

Format `poll_floor` / `poll_ceiling` / `poll_idle` / `interval` / `lock_ttl` passes semantic check `checkDuration` (like `reaper.interval` / `acolyte_*`): invalid duration rejects config at start, range (`>0`) achieves default. The mutual order of the corridor (`poll_floor ≥ 30s ≤ poll_ceiling ≤ poll_idle`) is checked against **resolved** values ​​(taking into account alias-clamp), therefore it also catches implicit violations through `interval`.

> **The previous dev-recommendation `interval: 5s` is no longer there.** At floor 30s, sub-30s polling is unattainable by design - for a frequent rhythm, set the corridor to 30–60s, for a reaction faster than 30s, use [Beacons](../adr/0030-vigil-oracle.md) (Vigil/Oracle, ADR-030), this is not Cadence's task.

## `acolytes`

```yaml
acolytes: 0
# acolyte_lease: 30s          # TTL Ward-capture (claim_expires_at = NOW()+lease)
# acolyte_batch: 10           # max. tasks for one claim tick (LIMIT)
# acolyte_poll_interval: 2s   # poll-fallback period to Summons signal
# acolyte_drain_grace: 5s     # graceful-drain pool window when Keeper stops
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `acolytes` | `int` (≥0) | `0` | Number of workers in the execution pool apply (**Acolyte**, [ADR-027](../adr/0027-apply-work-queue.md)). **Feature-flag**: `0` - the pool **does not rise**, execution of runs proceeds the same way (run-goroutine of the owner instance in scenario-runner); `>0` - a pool of N workers starts on the instance, each periodically brands planned tasks (`apply_runs`) through `FOR UPDATE SKIP LOCKED`. Transfer of execution to the pool (cutover, removal of run-goroutine) - step by step in [Phase 1.4 / Phase 2](../adr/0027-apply-work-queue.md); before this, `acolytes: 0` is the standard value. A negative value is rejected with error `value_out_of_range`. |
| `acolyte_lease` | `duration` | `30s` | TTL Ward-capture planned-task ([ADR-027(d)](../adr/0027-apply-work-queue.md): `claim_expires_at = NOW()+lease`). Expired Ward will relabel the recovery scan (Phase 2). Empty → default. |
| `acolyte_batch` | `int` (≥0) | `10` | Maximum planned tasks captured by one claim tick (LIMIT claim request). Workers of different instances share the queue via `FOR UPDATE SKIP LOCKED` - the batch only limits the appetite of one tick. `0`/omitted → default. Negative is rejected by `value_out_of_range`. |
| `acolyte_poll_interval` | `duration` | `2s` | The worker's poll-tick period is fallback to the Summons signal ([ADR-027(a)](../adr/0027-apply-work-queue.md)). Even if the pub/sub signal is lost, the task will be picked up at the nearest tick. Empty → default. |
| `acolyte_drain_grace` | `duration` | `5s` | Graceful-drain window of the Acolyte pool when Keeper stops ([ADR-027 Phase 2](../adr/0027-apply-work-queue.md)): from the "no more claim" signal to the hard cancellation of claim-ctx for in-flight workers who did not have time. An interrupted claim leaves Ward in the database (`claimed`/`running`) - the lease will expire, the task will pick up a recovery scan (ADR-027(i)); commit/rollback states are NOT forced. Empty → default. |

> **HA invariant: `N>1` live Keeper instances require `acolytes>0`.** `acolytes: 0`
> (run-goroutine-path) - **single-keeper-only**. In this mode, run ownership
> lives **in-memory** in the run-goroutine of the instance that launched it, and `RunResult`
> from Soul comes to the instance holding **EventStream** of this Soul (its SID-lease,
> [ADR-006(b)](../adr/0006-cache-redis.md)). On one Keeper
> it's always the same instance. In an HA cluster (≥2 live Keepers on shared PG/Redis)
> a run created on Keeper-A, but with Soul on the Keeper-B stream, **will work on the host**
> (`apply_runs.status=success`), however the incarnation **will forever get stuck in `applying`**:
> owner-run on Keeper-A will never see the one left on Keeper-B `RunResult` and his
> barrier will expire at `runTimeout`. When `acolytes>0` (work-queue, [ADR-027](../adr/0027-apply-work-queue.md))
> this is not the case: claim+dispatch go through the general queue (`apply_runs` + Summons), and completion
> is observed through a common PG, regardless of which instance is hosting the stream.
>
> Guards in the code - **two layers**:
>
> 1. **Refuse at start** (Finding-A, soul-shedding S3). Having a registry of live Keeper instances
> ([Conclave](#allow_unsafe_single_path_multi_keeper) - presence in Redis), Keeper when
> `acolytes == 0` And the presence of **other** live instances (`Conclave.CountLive > 1`, counting
> includes its own presence record) **refuses to start** with an understandable error
> (`refusing to start`) and `exit 1` - the operator sees the problem and fixes the config before receiving
> Soul streams. This is **default** (safe). Can be removed by explicit opt-out -
> [`allow_unsafe_single_path_multi_keeper`](#allow_unsafe_single_path_multi_keeper). Guard
> fail-open: if Conclave is unavailable (Redis off / SCAN error), the start is not blocked.
> Conclave keys TTL 30s → a just dead instance may still be listed (stale window) -
> for startup-refuse is acceptable.
> 2. **Runtime-WARN on dispatch** (safety-net). At the time of dispatch of the old path, if
> SID-lease of the target Soul belongs to **different** KID, dotted `WARN` is printed
> "the run may hang in applying - for HA set `keeper.acolytes>0`." Relies on
> an already existing SID-lease with a KID owner remains as insurance after the start (for example.
> the second instance rose after passing the refuse-guard of the first).

## `allow_unsafe_single_path_multi_keeper` (top-level)

Explicit **opt-out** from refuse-guard soul-shedding (Finding-A, [ADR-027](../adr/0027-apply-work-queue.md)). By default, Keeper with `acolytes == 0` and the number of living Keeper instances in **Conclave** (presence registry in Redis, keys `keeper:instance:<kid>`, TTL 30s) is more than one - **refuses to start** (see HA invariant above). `true` removes the ban: refuse is replaced by a loud `WARN`, the start continues.

```yaml
# allow_unsafe_single_path_multi_keeper: false   # default (refuse); top-level
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `allow_unsafe_single_path_multi_keeper` | `bool` | `false` | Remove refuse-guard `acolytes=0 + Conclave.CountLive>1`. `false` (default, **safe**) - Keeper **refuses to start** in this configuration. `true` - **conscious** choice of operator (for example, intentional single-keeper-for-LB during migration / rolling-restart, where the "other" instance is the outgoing one): refuse → `WARN`, start is in progress. Duplicated by the env flag `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER` (truthy-OR: `1`/`t`/`true`/…; empty/garbage string → does not include; pattern `KEEPER_INITIALIZE`). Any `bool` value is valid - there is no schema check. The key is **top-level**. |

> **Why default = refuse, and not warn.** Previously (before the Conclave registry) with multi-keeper + `acolytes:0` there was only `WARN` - the operator could not notice it, and apply on Keeper-A with Soul on stream Keeper-B would forever hang in `applying` (footgun). With a registry of live instances, Keeper can detect "I'm not alone" at the start and refuse to accept streams **before** - fail-closed according to the "security comes first" principle. Opt-out exists for legitimate transition states (migration/rolling-restart), where the operator deliberately keeps one running instance per LB.

## `watchman_interval` / `watchman_fail_threshold` (top-level)

**Watchman** - isolation detection + soul-shedding of one Keeper instance (soul-shedding S2, [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) HA cluster). The background goroutine periodically pings PG+Redis (same dependencies as `/readyz`); with stable instance isolation **actively closes all local EventStream streams** (hard-close, no drain), and the connected Souls, according to their failback list, go to the live Keeper. Without this, an isolated instance would keep already-installed long-lived streams (`/readyz` only takes NEW connections through LB - existing gRPC bidi streams do not depend on HTTP-health).

```yaml
# watchman_interval: 5s          # period probe PG+Redis (top-level)
# watchman_fail_threshold: 3     # successive failures probe before shedding (debounce)
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `watchman_interval` | `duration` | `5s` | Watchman probe tick period: how often the instance pings PG+Redis for isolation. The format is validated in the semantic phase; empty/`0`/incorrect → default (resolved in daemon, style `acolyte_*`). The key is **top-level**. |
| `watchman_fail_threshold` | `int` (≥0) | `3` | Number of **consecutive** probe failures before isolation and shedding are announced. Debounce/flap-guard: a single network spike should not reset all the streams at once (thundering-herd reconnect across the cluster). One successful probe resets the counter. `0`/omitted → default. Negative is rejected by `value_out_of_range`. Time until shedding ≈ `watchman_interval × watchman_fail_threshold` (default ≈ 15s). |

> Probe dependencies are the same `health.Pinger`s as `/readyz` (PG is required; Redis - only with a live client: dev-fallback without Redis leaves probe only on PG). Vault in probe is **not** enabled - it is optional for serving streams and its unavailability does not equal isolating the instance from the Souls. The "I'm isolated" solution is centralized in Watchman (not duplicated in per-stream renewal loops). The metrics are `keeper_watchman_isolated` (gauge 0/1) and `keeper_watchman_streams_shed_total`.

## `toll`

**Toll** — cluster-wide detector of Souls outflow ([ADR-038](../adr/0038-toll.md)). Per-instance Watcher watches gRPC disconnect events of the EventStream, filters graceful-shutdown / warmup-immunity and publishes the surviving event to the common Redis sorted-set. Cluster-leader (via Redis-lease `cluster:toll:leader`) every 5s aggregates the sorted-set over a sliding window of 60s, compares it with the baseline `souls.status='connected'` and, if the threshold is exceeded, sets the Redis key `cluster:degraded` (TTL 60s). Middleware on the Operator API blocks `POST /v1/incarnations/{name}/scenarios/{scenario}` and `POST /v1/push/apply` with HTTP 503 + Retry-After on each request when the flag is checked. Read-API, RBAC, unlock, destroy, Errand are NOT blocked (recovery actions).

Opt. block: if absent - Toll is enabled with defaults; `enabled: false` - explicit opt-out. Toll only works with a live Redis client (single-instance/dev without Redis → Toll infrastructure is not raised, the flag is not set by anyone, middleware passthrough).

```yaml
toll:
  enabled: true              # default true; false — turn off the detector completely
  threshold: 0.20            # share of baseline souls.status='connected' (0..1]
  window_size: 60s           # sliding window (per-second buckets in Redis sorted-set)
  degraded_ttl: 60s          # TTL key cluster:degraded
  clear_grace: 60s           # asymmetric hysteresis: stable low rate window before clearing
  lease_ttl: 30s             # TTL cluster:toll:leader (renew every ttl/3)
  warmup_delay: 60s          # immunity-window after the instance starts (cluster cold-start defense)

  # Per-coven threshold-overrides (ADR-038 amendment 2026-05-27, extensions).
  # OR-semantics: leader cocks cluster:degraded when exceeding EITHER global,
  # OR any per-coven threshold. With per-coven trigger audit-payload and
  # webhook-payload carry coven_name field; with global - without it.
  per_coven_thresholds:
    production-eu: 0.15
    production-us: 0.25

  # Webhook alert channel (ADR-038 amendment 2026-05-27, extensions). When
  # nil/enabled=false notifier is not raised, audit + gauge + metrics remain.
  webhook:
    enabled: true
    url_ref: "vault:secret/keeper/toll-webhook-url"  # field `url` in Vault KV
    format: pagerduty_v2                              # generic / pagerduty_v2 / slack
    timeout: 10s
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `toll.enabled` | `*bool` | `true` | Turn on the detector. `false` — Watcher does not build, Leader does not start, middleware = noop. Omitted / `null` → `true`. |
| `toll.threshold` | `float` | `0.20` | The share of the baseline `souls.status='connected'`, above which the Toll-leader cocks `cluster:degraded`. Range `(0, 1]`. Omitted / `0` → default. |
| `toll.window_size` | `duration` | `60s` | Sliding window length. Records older than the leader window are deleted via ZREMRANGEBYSCORE. |
| `toll.degraded_ttl` | `duration` | `60s` | Redis key TTL `cluster:degraded`. If the leader died and did not have time to renew, the flag goes out on its own and the blocking is removed. |
| `toll.clear_grace` | `duration` | `60s` | Stable window of low rate before clearing (asymmetric hysteresis). Trigger at the first excess; remove only after grace under the threshold. |
| `toll.lease_ttl` | `duration` | `30s` | TTL lease `cluster:toll:leader` (Renew every `ttl/3`). If the leader crashes, the next candidate will pick up after ≤ ttl. |
| `toll.warmup_delay` | `duration` | `60s` | Immunity window after the instance starts. The first `warmup_delay` disconnects after the start are NOT published (cluster cold-start defense - all Souls reconnect at once). The `keeper_toll_warmup_skipped_total` metric is still growing. |
| `toll.per_coven_thresholds` | `map[string]float` | nil (not specified) | Per-coven threshold-overrides. The key is the coven name (as in `souls.coven[]`), the value is the threshold in `(0, 1]`. Leader additionally ticks `ZRANGEBYSCORE` and counts per-coven rates; when exceeding ANY - cocks `cluster:degraded` with `coven_name` in payload. With global trigger (`toll.threshold` exceeded), `coven_name` remains empty. An empty key is rejected by the schema phase (for global - top-level `threshold`). |
| `toll.webhook.enabled` | `bool` | `false` | Raise [WebhookNotifier]. If `false` notifier is nil, there is no alert channel (audit + gauge + metrics remain). |
| `toll.webhook.url_ref` | `string` | — | webhook-receiver URL. Can be **vault-ref** (`vault:<mount>/<path>`; field `url` in Vault KV - recommended for prod) or inline-URL (`https://...`; for dev/local receivers). Mandatory for `enabled: true`. |
| `toll.webhook.format` | `enum` | `generic` | POST-payload format: `generic` (flat JSON), `pagerduty_v2` (Events API v2 schema - `routing_key` taken from the same Vault KV under the `routing_key` field), `slack` (incoming webhook with attachment). |
| `toll.webhook.timeout` | `duration` | `10s` | The ceiling of one POST call. Best-effort: timeout is logged, but does not block Set/Clear. |

> **Audit + alert semantics.** `cluster.degraded_set` / `cluster.degraded_cleared` (source `keeper_internal`) - writes ONLY leader (single-winner). For a per-coven trigger, audit-payload contains an additional field `coven_name`. Webhook (optional) - best-effort POST to set/cleared, the same `coven_name` is forwarded to payload (generic/pagerduty/slack). PagerDuty: `dedup_key` = `soul-stack/cluster:degraded` (one incident per set+resolve); `event_action: trigger` is set, `resolve` is cleared. Slack: `color: danger` to set, `good` to cleared.

> **Metrics:** `keeper_cluster_degraded` (gauge 0/1, set leader ONLY), `keeper_toll_disconnects_total{coven}` (counter), `keeper_toll_warmup_skipped_total`, `keeper_toll_graceful_skipped_total`, `keeper_toll_leader_active` (gauge 0/1, cluster sum = 1).

> **Cardinality risk of per-coven.** ADR-038(item 5) initially postponed per-coven due to cardinality in Prometheus. After the amendment: the list of keys `per_coven_thresholds` is clearly limited by the operator in keeper.yml (a final closed set), Prometheus counter `keeper_toll_disconnects_total{coven}` itself already carries the same cardinality - adding triggers on top does not multiply the label-set.

> **Hot-reload `toll.*`** ([ADR-021](../adr/0021-hot-reload-config.md)). Reload-able without restarting Leader: `threshold`, `window_size`, `degraded_ttl`, `clear_grace`, `per_coven_thresholds`, `webhook.*`. On a successful SIGHUP / API-reload, the daemon calls `Leader.UpdateConfig` - atomic swap of fields under RWMutex (tick reads snapshot at the beginning of each aggregation tick, without blocking update during Redis calls). The webhook-notifier is recreated only when the block `webhook.*` is diffed (URLRef / Format / Timeout / Enabled) - frequent reloads with an unchanged webhook do not affect the Vault resolve; during the first mutation `url_ref` (including the inline↔vault-ref transition), a new notifier is built through `NewWebhookNotifier` (the Vault resolve is deferred to the nearest `Notify`). Restart-required (quietly ignored on reload): `enabled` (Toll infrastructure is raised/switched off only at start), `lease_ttl` (captured in renew-loop), `warmup_delay` (used by per-instance Watcher at start). These restrictions are symmetrical to policy `logging.file` / `logging.format` (writer-restart-required).

## `tempo`

**Tempo** — per-AID rate-limiter resolver-heavy write endpoints ([ADR-050](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)). Token-bucket in Redis (per-Archon, by `claims.Subject` = AID), takes one token **after** JWT authentication and **before** launching resolvers; when exhausted - `429 tempo-exceeded` + `Retry-After`. The third anti-DoS layer after body-limit and [Toll](#toll): body-limit cuts by body size, Toll - by cluster health (cluster-wide 503), Tempo - by per-AID frequency (429).

Opt. block: if absent, Tempo is enabled with defaults (footgun-guard, like [Toll](#toll)); `enabled: false` - explicit opt-out. Tempo rises **only when the Redis client is live** (token-bucket lives in Redis): single-instance/dev without Redis → the limiter is not constructed, middleware passthrough. If Redis is unavailable on the fly (flap / connection refused), the limiter degrades **fail-OPEN** (the request passes without rate-check) - the same behavior as Toll; conscious security-trade-off (availability > reinsurance, [ADR-050(b)](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)).

```yaml
tempo:
  enabled: true              # default true (default-ON for Redis); false - opt-out
  voyage_create:             # bucket POST /v1/voyages (create)
    rate: 10                 # refill speed, tokens per second (rps)
    burst: 20                # bucket depth (capacity)
  voyage_preview:            # bucket POST /v1/voyages/preview (dry-resolve scope)
    rate: 30                 # softer create: preview read-like (without persist/audit)
    burst: 60                # bucket depth (capacity)
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `tempo.enabled` | `*bool` | `true` | Turn on the limiter. `false` - the limiter is not constructed, middleware = passthrough. Omitted / `null` → `true` (default-ON). The actual lifting additionally requires Redis. |
| `tempo.voyage_create.rate` | `float` | `10` | Refill bucket speed `voyage_create`, tokens per second (rps). Omitted / `0` → default. Negative → schema error `value_out_of_range`. |
| `tempo.voyage_create.burst` | `int` | `20` | The depth (capacity) of the bucket `voyage_create` is the permissible splash. Omitted / `0` → default. Negative → schema error `value_out_of_range`. |
| `tempo.voyage_preview.rate` | `float` | `30` | Refill bucket speed `voyage_preview`, tokens per second (rps). Omitted / `0` → default. Negative → schema error `value_out_of_range`. |
| `tempo.voyage_preview.burst` | `int` | `60` | The depth (capacity) of the bucket `voyage_preview` is the permissible splash. Omitted / `0` → default. Negative → schema error `value_out_of_range`. |

> **Two buckets - two endpoints.** `voyage_create` serves `POST /v1/voyages` (create); `voyage_preview` — `POST /v1/voyages/preview` ([ADR-043 amendment](../adr/0043-voyage.md)). Previously, preview shared bucket `voyage_create`; now it has its own, **softer** per-AID limit (`30/60` vs `10/20`), because preview is a dry-resolve scope: read-like in effect (without persist/audit), but resolver-heavy in cost, therefore not unlimited. Other write endpoints for Tempo - additive later (new bucket in the config + middleware, without breaking change). Read-API and cheap write (GET/list/cancel) are not limited.

> **Redis key and atomicity.** Bucket state - Redis-hash `tempo:<aid>:<bucket>` (`tokens` / `last_refill_ts` + `PEXPIRE`); refill+take - with one Lua script (atomic read-modify-write, coherent limit on top of stateless-HA cluster, [ADR-050(a)](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)). In-memory per-instance was rejected (it would have multiplied ×N instances). The time bucket reads from Redis itself (`redis.call("TIME")`), not from the Go clock - the refill does not depend on clock mismatch between instances. A cold bucket (new AID) starts full - the operator is not penalized by the first request.

> **Metrics.** `keeper_tempo_allowed_total{endpoint}` / `keeper_tempo_rejected_total{endpoint}` (counter; `endpoint` = bucket name - `voyage_create` or `voyage_preview`, **AID label NO** - cardinality). Who exactly exceeds is visible in the audit/logs for `claims.Subject`, not in the metrics. The complete register of Keeper metrics is [observability.md → Keeper Metrics](../observability.md). When Tempo is turned off (no Redis / `enabled: false`), counters remains at 0 - a valid signal "limiter is not active".

> **Hot-reload `tempo.*`** ([ADR-021](../adr/0021-hot-reload-config.md)). `rate` / `burst` reload-able without restart: stateless limiter, reads live `config.Store`-snapshot on **each** request - the new limit is applied from the next request, current buckets in Redis live according to their `PEXPIRE`. Invalid (≤0) values ​​from reload are interpreted as fail-OPEN passthrough (you cannot block if the config fails). Restart-required: `enabled` (Tempo-infrastructure / Redis token-bucket is raised or suppressed only at the start of `setupTempo`, symmetrically `toll.enabled`).

## `web_ui_enabled` (top-level)

Toggle the built-in operator web-UI on route `/ui` ([ADR-055](../adr/0055-embed-ui-bundle.md)). The real UI **compiled into the `keeper` binary** (`go:embed` statics from the companion repo `soul-stack-web`, see [docs/web/README.md](../web/README.md)) and is given to the keeper out of the box - **does not require a separate process, port or backend**. The static is mounted on an already existing OpenAPI-listener (`listen.openapi.addr`, usually `:8080`); **does not introduce new listeners and ports `web_ui_enabled`** - the UI shares `:8080` with the Operator API (`/v1/*`) and the OpenAPI viewer (`/docs`).

```yaml
# web_ui_enabled: true     # default (omitted / null → ON); false - opt-out. top-level
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `web_ui_enabled` | `*bool` | `true` | Whether to mount the embedded UI on `/ui`. **`*bool` to distinguish "not set" from an explicit `false`:** omitted / `null` → `true` (**default-ON** - beta wants a single-binary UI out of the box, footgun-guard in the spirit of [`tempo.enabled`](#tempo) / [`toll`](#toll)); explicit `false` → **opt-out**: static `/ui` is NOT mounted, API `/v1/*` and `/docs` are not affected. Unlike Tempo/Toll **does not depend on the infrastructure** - the UI is built into the binary, no external backend is needed. The key is **top-level**. Resolve effective value - method `WebUIMounted()` (`shared/config/keeper.go`): `nil` → `true`. |

> **Hot-reload `web_ui_enabled`** ([ADR-021](../adr/0021-hot-reload-config.md)). **Restart-required** (quietly ignored on reload, symmetrically [`toll.enabled`](#toll) / [`tempo.enabled`](#tempo)): the effective value is read once at startup and baked into the mounted router; SIGHUP/API-reload does not switch `/ui`-mount on the fly. To enable/disable the UI, change the key and restart `keeper`. Routing toggle does not justify the atomic re-mount of the router on a hot one: the static is built into the binary, does not carry state, switching is rare (beta-onboarding), and the disposal/swap of a chi-router under traffic is an extra risk for the sake of a binary flag. New ports do not open when turned on (static is located on the same `:8080`).

## `push`

**Pilot-path wire-up SshDispatcher** (S6, 2026-05-26, [ADR-032 amendment](../architecture.md)). Pilot includes `keeper.push.apply` in the production through 3 inline fields in `keeper.yml`. Long-term canon (S7) - migration to `souls.ssh_target jsonb` + PG-table `push_providers`; S7-3 introduced multi-CA `push.host_ca_refs[]`. Singular `push.host_ca_ref` remains under the 1-release WARN deprecation window.

Optional block: if there is no (or empty `targets[]` / missing `host_ca_ref` AND `host_ca_refs[]`) push-orchestrator is not raised, and `POST /v1/push/apply` / MCP `keeper.push.apply` is returned "not configured". To enable push in a product, three conditions are required:

1. `plugins.ssh_providers[]` is declared and at least one SshProvider plugin is disked in the cache (see [`plugins.ssh_providers`](#pluginsssh_providers)).
2. `push.host_ca_refs[]` is non-empty and resolves to Vault (public host-CA, field `public_key`). Singular `push.host_ca_ref` (deprecated) — backward-compat path: with a filled singular and an empty `host_ca_refs[]` daemon auto-adapt singular in `host_ca_refs[0]` with auto-name `default` + one-time WARN.
3. `push.targets[]` contains entries by SID of push hosts (FQDN matching `souls.sid`) or at least one entry `souls.ssh_target jsonb` (S7-1 canonical).

```yaml
push:
  host_ca_refs:                                   # S7-3: multi-CA OR check via CertChecker.IsHostAuthority
    - ref: vault:secret/keeper/ssh-host-ca-prod   # vault-ref, required
      name: trusted-bastion-1                     # kebab-case, label in keeper_push_host_ca_used_total{ca_name=...}
    - ref: vault:secret/keeper/ssh-host-ca-stage
      name: trusted-bastion-2
  targets:
    - sid: soul-a.example.com                    # = souls.sid (FQDN)
      ssh_port: 22                               # opt., default 22
      ssh_user: root                             # opt., default root
      soul_path: /usr/local/bin/soul             # opt., default /usr/local/bin/soul
    - sid: soul-b.example.com
      ssh_port: 2222
      ssh_user: deploy
      soul_path: /opt/soul/bin/soul
  providers:
    - name: vault-bastion                         # = plugins.ssh_providers[].name
      params:                                     # opaque form of the provider, serialized to JSON
        vault_addr: https://vault.internal:8200   # and passed to env SOUL_SSH_VAULT_BASTION_PARAMS
        role: ssh-bastion-role
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `push.host_ca_refs[]` | `array<{ref, name}>` | — (one of: `host_ca_refs[]` or deprecated `host_ca_ref` is required) | Multi-CA set for verify host-cert on SSH handshake (S7-3, [ADR-032 amendment](../architecture.md)). Each `ref` is a vault-ref (`vault:<mount>/<path>`), `name` is an operator-defined kebab-case (label in `keeper_push_host_ca_used_total{ca_name=...}`). During handshake, **OR-check** is done via `ssh.CertChecker.IsHostAuthority` for all CAs: cert signed by any → trusted; otherwise reject. Names in the set must be unique (`duplicate_push_host_ca_name`). Plaintext-PEM is disabled (`vault_ref_invalid`). Per-provider CA-override - deferred post-MVP. |
| `push.host_ca_ref` | `vault-ref` | — | **Deprecated (S7-3, 1-release WARN window).** Singular vault-ref on public host-CA. When singular is filled and `host_ca_refs[]` is empty, daemon auto-adapts singular in `host_ca_refs[0]` with auto-name `default` and writes a one-time WARN. Concurrent presence with `host_ca_refs[]` is rejected by the schema phase (`mutually_exclusive_keys`). Plaintext-inline-PEM is prohibited ("security first"): non-vault-ref → diag `vault_ref_invalid`. Symmetry with `auth.jwt.signing_key_ref` / `sigil.signing_key_ref`. |
| `push.targets[].sid` | `string` (FQDN) | — | Mandatory. The SID of the push host is the same as `souls.sid`. SID without entry in `targets[]` → `target_not_configured` on resolve in SshDispatcher. Duplicate SIDs are rejected (`duplicate_push_target_sid`). |
| `push.targets[].ssh_port` | `int` (1..65535) | `22` | TCP port of sshd on push host. `0`/omitted → default. |
| `push.targets[].ssh_user` | `string` | `root` | SSH user to login. Opt. (the typical value depends on the provider: vault-issued user-cert is usually the principal of a specific user). |
| `push.targets[].soul_path` | `path` | `/usr/local/bin/soul` | The absolute path to the soul binary on the push host. Delivered by ShaDeliverer during the first push pass (see [push.md → Delivery](push.md)); the path must match where Deliverer puts the binary. |
| `push.providers[].name` | `string` (kebab-case) | — | Mandatory. The name of the SshProvider plugin, references `plugins.ssh_providers[].name`. Duplicates are rejected (`duplicate_push_provider_name`). |
| `push.providers[].params` | `map<string, any>` | — | Opaque form of provider parameters (vault_addr/role/proxy_addr/...). When the plugin is spawned, it is serialized in JSON and placed in an env variable named `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` ([ADR-020 amendment l](../adr/0020-plugin-infrastructure.md)): `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`. There is no entry → the plugin starts without env-payload (the behavior depends on the plugin itself: `soul-ssh-static` works with defaults, `soul-ssh-vault` without params will return an error). |
| `push.allow_legacy_push_targets` | `bool` | `false` | S7-1 deprecation window: PG source (`souls.ssh_target` jsonb) canonical, `push.targets[]` legacy. When `false` entry is not in PG → `target_not_configured` (fail-closed); with `true` → fallback to inline-`targets[]` + one-time WARN at start. After S8 hard-cut the field is deleted (`unknown_key`). |
| `push.allow_legacy_push_providers` | `bool` | `false` | S7-2 deprecation window: PG source (`push_providers` table) canonical, `push.providers[]` legacy. When `false` plugin is not written to PG → the plugin starts without env-payload; at `true` → fallback to inline-`providers[]` + one-time WARN. Symmetrical `allow_legacy_push_targets`. |
| `push.auto_import_legacy_targets` | `bool` | `false` | S7-4 opt-in one-shot migration (ADR-032 amendment 2026-05-26). When `true` daemon starts (step `runLegacyAutoImport` after `setupPushProviderSvc`) it goes through `push.targets[]`: for each SID with `souls.ssh_target IS NULL` writes data + audit-event `soul.ssh-target.imported_from_config` (`source: config_bootstrap`). Idempotent (PG-row canonical, not overwritten). PG read/write fail → start failure. Missing `souls`-row → WARN-skip. See [push.md → S7-4](push.md#s7-4-auto-import-legacy-on-start-2026-05-26). |
| `push.auto_import_legacy_providers` | `bool` | `false` | S7-4 opt-in one-shot migration symmetrically `auto_import_legacy_targets`: `push.providers[]` → PG table `push_providers`. Imported entries carry `created_by_aid='archon-system'` (system-AID must exist in `operators` before first import). Audit-event - `push-provider.imported_from_config`. Idempotent (`SelectByName` → `ErrPushProviderNotFound` gates INSERT). |

> **Pilot single-provider routing.** S6 raises SshDispatcher to the **first discovered** SshProvider plugin (single dispatcher, without routing by `push_runs.ssh_provider`). Multi-provider routing (`vault-bastion` for prod hosts, `static` for dev hosts in one keeper instance) - S7. Typical pilot case: one provider per keeper (typically `soul-ssh-vault` OR `soul-ssh-static`, not both).

> **Migration to S7.** S7-1 moved `targets[]` to `souls.ssh_target jsonb` (canonical), S7-2 - `providers[]` to PG-table `push_providers`, S7-3 - `host_ca_ref` (singular) to `host_ca_refs[]` (multi-CA), S7-4 - opt-in auto-import of inline blocks when Keeper starts (flags `auto_import_legacy_*`). All legacy fields will remain under 1-release WARN deprecation window, then (S8) `unknown_key` hard-cut. Before closing the window, it is permissible to mix legacy with canonical (PG has priority).

## `rbac`

**Moved to the database (ADR-028).** The RBAC directory (roles, operator bindings, permissions) is no longer part of the config contract - it lives in Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`), managed via the `role.*` API/MCP. The key `rbac:` in `keeper.yml` is **not accepted**: the parser rejects it with error `unknown_key`.

Regulatory description of RBAC - [rbac.md](rbac.md) (permissions format, selector grammar, directory, Bootstrap of the first Archon).

## `reactor`

**Architecturally not fixed.** The name `reactor` is not yet fixed in [`naming-rules.md`](../naming-rules.md), requires propose-and-wait the next time you enter an event-driven circuit (see [open Q No. 23](../architecture.md)). The format of the rules, triggers, actions, RBAC, restrictions, relation to the event-driven circuit - **not fixed by any ADR**.

Before a separate ADR by design, the block is **non-normative**: parser `keeper.yml` rejects key `reactor:` with error `unknown_key`.

See [open Q #23](../architecture.md) - event-driven circuit - related question.

## `services` / `default_destiny_source` / `default_module_source`

**Moved to the database (ADR-029).** The Service registry and the `default_destiny_source` scalar live in Postgres (`service_registry` / `keeper_settings`, see [storage.md](storage.md)), managed through the `service.*` API/MCP ([operator-api.md](operator-api.md)); The resolution semantics are described above in the section "Service registry and `default_destiny_source` - in Postgres". `default_module_source` was canceled without replacement (there was no consumer). All three keys (`services:` / `default_destiny_source:` / `default_module_source:`) in `keeper.yml` are **not accepted**: the parser rejects each with error `unknown_key`.

## `audit`

```yaml
audit:
  enabled: true
  otel_export: true
  retention_days: 365
```

General audit-pipeline normalization - [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention): storage (Postgres table `audit_log`, see [storage.md](storage.md)), schema (`audit_id` / `created_at` / `event_type` / `source` / `archon_aid` / `correlation_id` / `payload`, write-path (HTTP-middleware / MCP-handler / Reaper / hot-reload / `keeper.cloud` / `keeper.push` / bootstrap / Soul gRPC forwarded), retention (via Reaper rule `purge_audit_old`, see [reaper.md](reaper.md)). Event-types directory - [naming-rules.md → Audit-events](../naming-rules.md#audit-events).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `audit.enabled` | `bool` | `true` | Global switch audit-pipeline. With `false`, none of the write-path initiators (see [ADR-022(g)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) writes to Postgres `audit_log`; OTel dual-write (see `otel_export` below) is also disabled. Use only for development / incident investigation - the production installation must keep `true` for compliance invariants. |
| `audit.otel_export` | `bool` | `true` | Duplicate audit-event in OTel span as attribute (transient debugging aid, Postgres - source of truth). When `false` audit is written only to `audit_log`; Keeper's OTel-spans continue to go through [`otel:`](#otel) as usual, but audit-attributes are not added to them. Useful for installations without OTel infrastructure ([ADR-022(f)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |
| `audit.retention_days` | `int` (≥1) | `365` | Record retention period is `audit_log` (days). **Alias** on `reaper.rules.purge_audit_old.max_age` ([ADR-022(d)/(i)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) - one source of truth for retention, a convenient form in days here and `duration` in [`reaper:`](#reaper) ([reaper.md → Rules](reaper.md)). If the values ​​in two blocks diverge, the `keeper.yml` parser rejects the config with the error `audit_retention_mismatch`. |

**Soul does not have block `audit:`** - Soul does not physically have access to Postgres `audit_log` (module isolation, [ADR-011](../adr/0011-go-layout.md)). Soul-side audit events (`TaskEvent` / `RunResult` / `SoulprintReport`) go through Keeper and are written by it from `source: soul_grpc` ([ADR-022(b)/(g)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). OTel-attributes Soul writes to its gRPC EventStream spans normally via the `otel:` block `soul.yml`.

### Hot-reload block `audit:`

All three fields are **reload-able without restart** of the `keeper` process: these are the parameters for a specific launch of write-path initiators (read in-memory with each write to `audit_log`) and the Reaper rule parameter (read at the next iteration of the Reaper, [reaper.md](reaper.md)).

| Field | Reload without restarting the `keeper` process | Rationale |
|---|---|---|
| `enabled` | yes | Applies in-memory on every write; in-flight records are updated with the old value. |
| `otel_export` | yes | The same: per-record flag, read by helper `shared/audit` at the time of write. |
| `retention_days` | yes | Apply by Reaper at the next iteration (via alias to `reaper.rules.purge_audit_old.max_age`). |

## `hot_reload`

```yaml
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true
```

The block regulates the activation of hot-reload mechanism triggers (`SIGHUP` / `inotify`) and the generation of `correlation_id` for audit-events `config.reload_succeeded` / `config.reload_failed`. The semantics and invariants of the mechanism itself are [ADR-021](../adr/0021-hot-reload-config.md) and [§ Hot-reload](#hot-reload) below. The entire block is optional: if `keeper.yml` is missing, the defaults from the table are applied.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `hot_reload.enable_signal` | `bool` | `true` | Enable `SIGHUP` trigger file-edit-path: the process catches the signal, rereads `keeper.yml` from disk, runs the validation pipeline and does atomic swap ([ADR-021(b)](../adr/0021-hot-reload-config.md)). When `false` file-edit-path is disabled (file changes are not picked up without a restart); API/MCP-path works independently. |
| `hot_reload.enable_inotify` | `bool` | `false` | Enable auto-reload via `inotify`/`fsnotify` (Linux-only) - respond to changes in `keeper.yml` without `SIGHUP`. Post-MVP optional extension ([ADR-021(b)](../adr/0021-hot-reload-config.md)): watch-handle overhead and Linux-only dependency - a reason not to default. |
| `hot_reload.audit_correlation_id` | `bool` | `true` | Generate `correlation_id` for audit-events `config.reload_succeeded` / `config.reload_failed` ([naming-rules.md → Audit-events](../naming-rules.md#audit-events)). With `true`, each reload event receives a unique id that goes into both OTel-spans and audit-trail (links file-edit-path with API-path for successive mutations). |

### Hot-reload block `hot_reload:`

All three fields **require a restart** of the `keeper` process - because they control the hot-reload mechanism itself: change `enable_signal` / `enable_inotify` without restart = race condition for installing/removing a signal-handler or `inotify`-watch; changing `audit_correlation_id` on the fly would split one logical reload operation into two different audit logging modes.

| Field | Reload without restarting the `keeper` process | Rationale |
|---|---|---|
| `enable_signal` | **no, requires a restart** | Changes the signal-handler binding to `SIGHUP`; race on installing/removing the handler. |
| `enable_inotify` | **no, requires a restart** | Changes the `inotify`-watch registration to the config path; race on handle. |
| `audit_correlation_id` | **no, requires a restart** | Parameter of the reload-pipeline itself; changing it with one of the reloads would mean writing an audit-event about your own mutation in two different modes. |

## Hot-reload

Hot-reload of the config with rewriting the changed value back to disk - end-to-end requirement ([requirements.md](../requirements.md), [architecture.md → End-to-end requirements](../architecture.md)). The mechanism is normalized **[ADR-021](../adr/0021-hot-reload-config.md)**; implementation - package [`shared/config/`](../adr/0011-go-layout.md) (Tier 2).

**Two ways to change the config.**

| Path | Trigger | Pipeline |
|---|---|---|
| **File-edit** | The operator edits `keeper.yml` on the host → sends `SIGHUP` to the process. | parse → schema-validate → semantic-validate → atomic swap → audit. |
| **API/MCP** | OpenAPI/MCP mutation of the config (specific endpoints are deferred to the Operator API, see [ADR-021 → Consequences](../adr/0021-hot-reload-config.md)). | mutate → schema-validate → semantic-validate → atomic swap → **write-back YAML** → audit. |

**SIGHUP - single trigger in MVP** for file-edit-path. `inotify`/`fsnotify` - post-MVP optional extension, disabled by default (see [ADR-021(b)](../adr/0021-hot-reload-config.md)).

**Validation pipeline** - three stages before atomic swap; any error → in-memory state is unchanged, the file is not modified (even for API-path), audit-event `config.reload_failed` with `phase ∈ {parse, schema_validate, semantic_validate}` ([ADR-021(c)](../adr/0021-hot-reload-config.md)).

**Write-back YAML** - only for API-path: round-trip preservation (comments, key order, anchors are saved) + atomic rename (write-to-tmp in the same directory + `rename(2)`) + permissions from the source file. File-edit-path does not write anything (the file is already on disk).

**Scope** - general principle: reload-able without restart - parameters of a specific launch / run (timeouts, policies, thresholds, capabilities whitelist); require restart - external process surface (listener-addresses, socket paths, TLS certificates, Postgres/Redis DSN, log-rotation file paths). The summary table below covers all blocks `keeper.yml` 1:1; blocks with non-trivial per-field semantics (`plugin_runtime`, `hot_reload`) are additionally normalized by separate tables in their sections.

**Summary per-block reload-policy** (standard, one line per block):

| Block | Reload-able without restart | Require restart | Note |
|---|---|---|---|
| `kid` | — | `kid` | Instance ID; change = new instance. |
| `listen.*` | — | all (`grpc.addr`, `openapi.addr`, `mcp.addr`, `metrics.addr`, `grpc.tls.*`, `grpc.event_stream.max_apply_size_mb`) | External surface; TLS files are read at init context, `MaxSendMsgSize` is set on the gRPC server once at startup. |
| `postgres.pool.*` | `min`/`max` (in-memory grows/shrinks) | — | The pool settings are applied to new connections. |
| `postgres.dsn_ref` | — | yes | Open connections are not recreated. |
| `redis.*` | — | yes | Connection-strings + password. |
| `vault.addr` | — | yes | Open Vault-client connection. |
| `vault.auth.*` | — | yes | Re-auth only at the start. |
| `vault.pki_mount` | yes | — | Read per-request. |
| `auth.jwt.signing_key_ref` | — | yes | The Signing key is loaded into memory at start. |
| `auth.jwt.issuer` / `ttl_default` / `ttl_bootstrap` | yes | — | Apply to **new** issued tokens; already issued JWTs are valid until their exp. |
| `metrics.auth.basic.*` | — | yes | The password is resolved from the vault at the start, the listener is raised once. |
| `otel.*` | — | yes | Re-init exporter/connection; `SetupOTel` is called once per process ([ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). |
| `logging.level` | yes | — | In-memory variable. |
| `logging.format` / `logging.file` / `logging.rotation.*` | — | yes | Re-init log writer / file handles. |
| `plugins.*` | yes | — | Cache reload artifact-store. |
| `reaper.enabled` / `dry_run` / `batch_size` / `rules.*` | yes | — | In-memory loop, next iteration sees new things. |
| `reaper.interval` | yes | — | Next iteration with a new interval. |
| `reaper.lock_ttl` | — | yes | Redis-lease TTL is set upon acquire. |
| `cadence_scheduler.interval` | yes | — | Conductor rereads at each tick from the latest Store snapshot ([conductor.md](conductor.md), ADR-048). |
| `cadence_scheduler.lock_ttl` | yes | — | TTL Redis-lease `conductor:leader` applies between re-acquire. |
| `cadence_scheduler.enabled` | — | yes | Conductor up/down is read at startup `setupConductor`; hot-toggle subsystems (lease + goroutine on the fly) - a separate slice. Operational management of schedules - per-Cadence `enabled` (ADR-046), without restart. |
| `acolytes` / `acolyte_lease` / `acolyte_batch` / `acolyte_poll_interval` / `acolyte_drain_grace` | — | yes | Worker pool parameters (ADR-027) are read at startup `setupAcolyte`; restarting the pool / reinjecting claim parameters on the fly - separate slice. |
| `watchman_interval` / `watchman_fail_threshold` | — | yes | Watchman (soul-shedding S2) parameters are read at startup `setupWatchman`; reinjection of the period/threshold into a live probe-loop - a separate slice. |
| `allow_unsafe_single_path_multi_keeper` | — | yes | Opt-out refuse-guard (Finding-A) is read at startup `setupConclaveRefuseGuard`; The decision "refuse vs warn" is made once before receiving streams. |
| `tempo.voyage_create.rate` / `tempo.voyage_create.burst` | yes (both) | — | Stateless limiter: rate/burst are read from a fresh Store snapshot on each request ([§ tempo](#tempo), ADR-050). |
| `tempo.voyage_preview.rate` / `tempo.voyage_preview.burst` | yes (both) | — | The same, separate bucket for `POST /v1/voyages/preview` ([§ tempo](#tempo), ADR-050). |
| `tempo.enabled` | — | yes | Raise/quench Tempo (Redis token-bucket) read at startup `setupTempo`; hot-toggle subsystem - separate slice (symmetrically `toll.enabled`). |
| `rbac` | — | — | Moved to database (ADR-028); the parser rejects the key with `unknown_key` ([§ rbac](#rbac)). |
| `reactor` | — | — | The parser rejects with `unknown_key` ([§ reactor](#reactor)). |
| `audit.enabled` / `audit.otel_export` / `audit.retention_days` | yes (all three) | — | Parameters write-path (`enabled`/`otel_export`) - in-memory per-record; `retention_days` - alias to `reaper.rules.purge_audit_old.max_age`, read by Reaper at the next iteration ([§ Hot-reload block `audit:`](#hot-reload-block-audit)). |
| **`plugin_runtime.*`** | per-field - see [§ Hot-reload block `plugin_runtime:`](#hot-reload-block-plugin_runtime) | | |
| **`hot_reload.*`** | per-field - see [§ Hot-reload block `hot_reload:`](#hot-reload-block-hot_reload) | | (all require restart) |

**Multi-host coordination** - not in MVP. Each Keeper instance of the HA cluster ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) reboots its `keeper.yml` independently. Cross-host "reload by cluster-wide event" via Redis pub/sub - post-MVP ([ADR-021(f)](../adr/0021-hot-reload-config.md)).

**Audit-events** - two names, directory in [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events):
- `config.reload_succeeded` — fields `source ∈ {signal, api}`, `archon.aid` (for API-path), `changed_paths` (list of YAML-paths), `correlation_id`.
- `config.reload_failed` - fields `source`, `archon.aid` (if applicable), `validation_errors[]`, `phase ∈ {parse, schema_validate, semantic_validate}`.

**History** - git-blame YAML (for file-edit-path) + audit-trail in Postgres (for API-path). Separate database table `config_history` with snapshots - post-MVP ([ADR-021(i)](../adr/0021-hot-reload-config.md)).

**Optional block `hot_reload:`** in `keeper.yml` (fields `enable_signal`, `enable_inotify`, `audit_correlation_id`) - normative typing of fields in [`## hot_reload`](#hot_reload) above. If there is no block, defaults are applied from there (built into `shared/config`).

## Full example

Minimum valid `keeper.yml` with all required fields:

```yaml
kid: keeper-eu-west-01

listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
    event_stream:
      addr: "0.0.0.0:9443"
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
        ca:   /etc/keeper/tls/ca.crt
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }

postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }

redis:
  addr: "redis-cluster.internal:6379"
  password_ref: vault:secret/keeper/redis

vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id
  pki_mount: "pki/soulstack"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 24h
    ttl_bootstrap: 720h

otel:
  enabled: true
  exporter: otlp
  endpoint: "otel-collector.internal:4317"

logging:
  level: info
  format: json
  rotation: { max_size_mb: 100, max_files: 10, compress: true }

# Service registry and default_destiny_source - in Postgres (ADR-029),
# they are not in keeper.yml. Management via service.* API/MCP.

plugins:
  cache_root: /var/lib/soul-stack-keeper/plugins
  cloud_drivers:
    - { name: aws, source: "git@github.com:soul-stack-ecosystem/soul-cloud-aws.git", ref: v2.0.0 }
    - { name: yc,  source: "git@github.com:our-company/soul-cloud-yc.git",          ref: v0.3.1 }
  ssh_providers:
    - { name: vault-ssh, source: "git@github.com:soul-stack-ecosystem/soul-ssh-vault.git", ref: v1.0.0 }
    - { name: static,    source: "git@github.com:soul-stack-ecosystem/soul-ssh-static.git", ref: main }

plugin_runtime:
  socket_dir: /var/run/soul-stack-keeper/plugins
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false

# Optional: the entire block can be omitted - defaults will be applied
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true

audit:
  enabled: true
  otel_export: true
  retention_days: 365

watchman_interval: 5s
watchman_fail_threshold: 3

# web_ui_enabled: true   # default ON - built-in UI on /ui (ADR-055); false - opt-out

# allow_unsafe_single_path_multi_keeper: false  # default refuse with multi-keeper + acolytes=0

reaper:
  enabled: true
  interval: 1h
  dry_run: false
  batch_size: 500
  lock_ttl: 5m
  rules:
    expire_pending_seeds: { enabled: true, max_age: 24h, action: delete }
    purge_used_tokens:    { enabled: true, max_age: 90d, action: delete }
    purge_souls:          { enabled: true, statuses: [disconnected, expired], max_age: 30d, action: delete }
    purge_old_seeds:      { enabled: true, statuses: [superseded, expired, revoked], max_age: 90d, action: delete }
    mark_disconnected:    { enabled: true, stale_after: 90s, action: set_status, target_status: disconnected }
    purge_audit_old:      { enabled: true, max_age: 365d, action: delete }
```

The RBAC directory (roles, bindings, permissions) is not set in `keeper.yml` - it is in Postgres (ADR-028), controlled via `role.*` API/MCP; the key `rbac:` is rejected as `unknown_key`.

The reference example in the file is [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml).

## See also

- [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) - a working example of the entire config.
- [concept.md](concept.md) - what tasks Keeper solves, which config blocks relate to which tasks.
- [storage.md](storage.md) - what lies behind `postgres:` and `redis:`, the registry `operators` for `auth:` and the RBAC table (ADR-028).
- [push.md](push.md) - consumer `plugins.ssh_providers`.
- [cloud.md](cloud.md) - consumer `plugins.cloud_drivers`.
- [reaper.md](reaper.md) - complete description of the `reaper:` block and cleaning rules.
- [rbac.md](rbac.md) - full description of the `rbac:` block, parsing permissions, Bootstrap of the first Archon.
- [architecture.md → ADR-013](../adr/0013-bootstrap-archon.md) — bootstrap of the first Archon.
- [architecture.md → ADR-014](../adr/0014-operator-identity.md) - operator identity model, source of truth according to `auth:`.
- [architecture.md → End-to-end requirements](../architecture.md) - Vault, OTel, RBAC, MCP, OpenAPI, hot-reload, log rotation as mandatory.
- [naming-rules.md](../naming-rules.md) - dictionary of names.
