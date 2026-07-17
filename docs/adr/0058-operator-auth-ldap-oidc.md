# ADR-058. Federated operator authentication (Archon) - LDAP + OAuth2/OIDC

> **Status: accepted + end-to-end (LDAP, stage 1 / OIDC, stage 2).** LDAP and OIDC authentication are **implemented and brought end-to-end**. LDAP: search-bind, auto-provision by groups **with seeding of the `operators` row**, cookie delivery of JWT - the blocker of seeding the system operator `archon-system` **is closed** (`created_via`-relaxation of the bootstrap index, migration 086; see below in (d)). OIDC (stage 2): authorization-code flow with **mandatory PKCE (S256)**, discovery (`go-oidc/v3`) + JWKS validation of `id_token` (signature/iss/aud/exp/nonce), a cluster-shared server-side flow-state store on Redis (state -> nonce/PKCE-verifier, single-use GETDEL, TTL 5m), endpoints `GET /auth/oidc/{login,callback}`, cookie delivery of JWT (`SameSite=Strict`, unified with LDAP - security-fix 2026-06-24, see (g)/(#4)). The mapper is generalized for both methods (`auth_method`/`created_via` = `ldap`|`oidc` from `cfg.Method`). All forks are resolved: enum name - `oidc` (locked in), PKCE - mandatory (no config flag, not left to the operator).
>
> **Security-fix pass 2026-06-24 (consolidated):** (1) **account-takeover (CRIT)** - federated login of an existing operator is rejected if its `auth_method` != the mapper's method (`ErrAuthFailed`, anti-oracle): bootstrap/system/mTLS/another-federated operator is NOT assigned a matched derived-AID. The federated path serves only operators enrolled by the same method. See (d) revocation invariant. (2) **scoped role-revoke (HIGH)** - role reconciliation now removes membership-roles that left the groups (not just grant), scoped by the union of `values(group_role_map)` (Synod/manually-assigned roles outside the domain are NOT touched), grant+revoke in a single transaction - implementation of the intent from (d). (3) **anti-bruteforce (HIGH)** - public login endpoints got per-IP+per-username throttle + lockout after a streak of failures + OIDC flow-state flood limit (`auth.rate_limit` block, default-ON, Redis, fail-closed on lockout). (4) **SameSite unified to Strict** for LDAP and OIDC; **aid_claim default `sub`** (immutable) + WARN on user-mutable `email`/`preferred_username`.

- **Context.** The operator identity model was fixed in [ADR-013](0013-bootstrap-archon.md) (Bootstrap of the first Archon via `keeper init --archon`) and [ADR-014](0014-operator-identity.md) (registry `operators` in Postgres: `aid` PK, `auth_method` enum, JWT-credential MVP with claims `iss`/`sub`/`iat`/`exp`/`roles`/`bootstrap_initial`, signing key from Vault KV `secret/keeper/jwt-signing-key`). `auth_method` already carries an extensible enum (`jwt` implemented, `mtls`/`combined` declared post-MVP as only-add). RBAC ([ADR-028](0028-rbac-storage.md)/[ADR-047](0047-purview.md)) operates on roles by AID and does not depend on the authentication method.

  Currently the only way to enroll an operator is `keeper init` (the first one) or `POST /v1/operators` by an existing admin with permission `operator.create` (issues a JWT in a file/in the response). For integration with corporate identity providers (Active Directory / LDAP, Keycloak / Okta / Google / Azure AD via OIDC), federated login is needed: a human Archon authenticates with an external IdP, and Keeper trusts the result and issues its own internal JWT.

- **Decision (recommended - federated model).** External authentication is **validated at Keeper** and **mapped** onto the `operators` registry (AID) + RBAC roles, after which Keeper issues an **internal JWT** using the existing `jwt.Issuer` (ADR-014). The rest of the system (auth-middleware, RBAC, MCP, OpenAPI) remains JWT-based and **does not change**.

  ```
  ┌──────────┐   LDAP bind / OIDC code-flow    ┌──────────────┐
  │  Archon  │ ──────────────────────────────▶ │  external IdP │
  └──────────┘                                  └──────────────┘
       │                                                │
       │  (1) login / (3) callback with id_token|creds  │ (2) authentication
       ▼                                                ▼
  ┌──────────────────────────────────────────────────────────┐
  │  Keeper  /auth/*  (NEW, public endpoints outside /v1)      │
  │  ┌────────────┐  validate   ┌──────────┐  map AID+roles    │
  │  │ ldap.Authn │ ──────────▶ │ identity │ ────────────────▶ │
  │  │ oidc.Authn │             │  mapper  │                    │
  │  └────────────┘             └──────────┘                    │
  │        │ (4) issue INTERNAL JWT via jwt.Issuer (ADR-014)     │
  └────────┼──────────────────────────────────────────────────┘
           ▼
   Bearer JWT → /v1/* (RequireJWT + RBAC UNCHANGED)
  ```

  ### Why federated and not the alternatives

  - **Rejected: passing the external token directly into `/v1/*`** (Keeper validates someone else's id_token on every request). Downsides: (a) auth-middleware and MCP would have to learn two token formats; (b) every `/v1` request would be an external JWKS lookup or a repeated LDAP bind (latency, IdP availability on the critical path); (c) RBAC roles would have to be extracted from someone else's claims on every request. The federated model localizes the external dependency to the moment of login, not every request.
  - **Rejected: external IdP as the only issuer (Keeper without its own JWT).** Breaks [ADR-013](0013-bootstrap-archon.md) (bootstrap of the first Archon without an IdP) and offline/air-gapped scenarios. The internal JWT is a single point for revocation ([ADR-014](0014-operator-identity.md) amend) and short TTL.
  - **Accepted: hybrid.** Bootstrap-Archon (ADR-013) and `POST /v1/operators` remain (the lower trust layer, independent of an external IdP). LDAP/OIDC is an additional login method for subsequent operators. External identity always reduces to a `operators(aid)` registry row - a single authorization subject.

- **(a) Extending the `auth_method` enum (only-add, no breaking changes).** To the existing `jwt`/`mtls`/`combined` (ADR-014) the following are added:

  | value | meaning |
  |---|---|
  | `ldap` | operator authenticates via LDAP bind, Keeper issues an internal JWT |
  | `oidc` | operator authenticates via OIDC code-flow, Keeper issues an internal JWT |

  This is an additive extension in the spirit of ADR-014 (mTLS/combined were already declared post-MVP via the same enum). Implemented: Go const `operator.AuthMethod` (`AuthMethodLDAP`/`AuthMethodOIDC`), `operator.Insert` accepts both, huma-enum `OperatorAuthMethod` + query-filter list `enum:"jwt,mtls,combined,ldap,oidc"` (`huma_enums.go`/`huma_operator_op.go`), SQL `CHECK auth_method_valid` extended by migration **083** (only-add, forward-only). `oidc` was set up right away (stage 2) so the OIDC implementation would not touch the enum/CHECK again. `auth_method` on the `operators` row records which method the operator arrived through (for audit and UI); the internal JWT itself is identical after issuance for all methods.

  > **Enum-value name:** `oidc` (locked in) versus `oauth2`. **`oidc`** was chosen - Keeper relies on the OIDC id_token (identity layer over OAuth2), not on a plain OAuth2 access-token. "oauth2" would be more accurate for authorization-without-identity, which does not suit us.

- **(b) OAuth2/OIDC - authorization-code flow with PKCE (human-Archon login). IMPLEMENTED (stage 2).**

  1. `GET /auth/oidc/login` (public) -> Keeper generates `state` + `nonce` + PKCE `code_verifier` (256-bit crypto/rand), puts `{state->(nonce,verifier)}` into the server-side flow-state store (Redis `OIDCFlowStore`, TTL 5 min, ADR-006), redirects to the IdP's `authorization_endpoint` with an S256 `code_challenge`. `code_verifier`/`nonce` NEVER leave the server (only `state` + `code_challenge` are in the URL).
  2. The Archon authenticates with the IdP.
  3. `GET /auth/oidc/callback?code=...&state=...` (public) -> Keeper: **Consumes `state`** from the store (`GETDEL` - atomically reads AND deletes, single-use: anti-CSRF + anti-replay; unknown/already-consumed/expired -> rejection), exchanges `code` for tokens with `code_verifier` (**PKCE-enforced**: without the verifier, the IdP rejects the exchange), **validates `id_token`** via the `go-oidc` verifier: signature via JWKS (`jwks_uri` from discovery), `iss` == issuer, `aud` == client_id, `exp`/`iat`; then **checks `nonce`** (anti-replay of the id_token). Extracts `sub` + configured claim fields (`aid_claim` default `sub` -> AID, email/preferred_username/`groups_claim`).
  4. **Mapping** onto `operators(aid)` + roles (shared `auth.DBMapper`, `Method=oidc`; auto-provision by groups, see (e)/(i)). Issues an **internal JWT** via `jwt.Issuer`, puts it into an HttpOnly+Secure cookie `soul_session` (`SameSite=Strict`, unified with LDAP - security-fix 2026-06-24, see (g)/(#4)) and redirects to `/ui` (302). No JSON token in the body (cookie-only, parity with LDAP). PKCE is **mandatory** - no config flag (fork #6 resolved in favor of "mandatory"). OIDC requires a live Redis (flow-store, cluster-shared, ADR-053 OPTIONAL-tier): without Redis the endpoints are not mounted.

  **Libraries (added in stage 2):** `github.com/coreos/go-oidc/v3` (discovery + JWKS + id_token verify, Apache-2.0) + `golang.org/x/oauth2` (code-exchange + PKCE helpers `GenerateVerifier`/`S256ChallengeOption`, BSD-3 - compatible with Apache-2.0). Discovery (`/.well-known/openid-configuration`) - a network call at load-time (`setupOIDCAuth`); JWKS - with TTL + key-rotation refetch (provided by go-oidc). A custom IdP CA (`tls.ca_ref`) is threaded into discovery/JWKS/token-exchange via `oidc.ClientContext`. Layout: `keeper/internal/auth/oidc` (authenticator) + `keeper/internal/redis/oidcflow.go` (store) + `keeper/internal/api/huma_oidc.go` (endpoints).

- **(c) LDAP (bind + group->role). IMPLEMENTED (stage 1).**

  1. `POST /auth/ldap/login` (public) with body `{username, password}` over **HTTPS** (Keeper listener TLS is mandatory).
  2. Keeper connects to LDAP via **LDAPS** or **StartTLS** (plaintext LDAP is forbidden by config validation AND by the `ldap.New` constructor). Bind mode - **search-bind** (the only one in stage 1, fork #7 resolved): a service account (`bind_dn` + `bind_password_ref` from Vault) does a search by `user_filter` (`(uid=%s)` / `(sAMAccountName=%s)`); the username is escaped with `ldap.EscapeFilter` (anti-injection); exactly one record, otherwise rejection; then a re-bind is done with the found user-DN + the entered password (password check). Direct-bind is deferred (extension without breaking change).
  3. After a successful user-bind - group-search (`group_filter`, e.g. `(member=%s)`, user-DN escaped) -> list of groups by `group_attr` (default `cn`).
  4. **Mapping** groups onto roles + `operators(aid)` (see (e)). Issuance of the internal JWT in a cookie.

  All rejection reasons are sanitized into a single `auth.ErrAuthFailed` (anti-oracle: 401 exposed outward with no reason); password/bind-creds never end up in errors or logs (only in a debug step without the secret). Implementation - `keeper/internal/auth/ldap/ldap.go` (conn interface over `*ldap.Conn` for unit tests without a real LDAP).

  **Library:** `github.com/go-ldap/ldap/v3` (MIT - compatible with Apache-2.0), added to `keeper/go.mod`.

- **(d) Mapping external identity -> `operators(aid)` + roles. IMPLEMENTED (LDAP, `keeper/internal/auth/mapper.go`).**

  - **AID-derivation (RESOLVED):** the AID is derived from the LDAP attribute given by config `aid_attr` (`uid` | `mail`). **Default is `uid`** (if `aid_attr` is not set): shorter and more stable than `mail` (an email address can be reassigned), almost always present in the `person`/`inetOrgPerson` schema. The AID is lowercase-normalized. The AID charset (ADR-014 amend: `^[a-z0-9][a-z0-9._@-]{1,127}$`) allows `@`/`.` for email-like names; an invalid derived-AID -> `ErrAuthFailed` (without leaking). The authenticator puts the derived-AID into a new field `ExternalIdentity.AID` (kept separate from `Subject`=user-DN, additive extension of the `auth.go` contract).
  - **Role-mapping (RESOLVED - roles come from groups, fork #2; RECONCILIATION - security-fix 2026-06-24):** LDAP/OIDC groups -> RBAC roles via `group_role_map: {group: [role,...]}`. The source of roles is the external groups (not the registry), both for a new and for an existing operator. For an existing operator, membership is **RECONCILED** (not just granted): roles from the current groups are granted, roles that have **left the groups** are removed (`rbac.RevokeOperator`), grant+revoke in a SINGLE transaction. The removal is **scoped to the union of `values(group_role_map)`** - reconciliation owns only its own domain; roles granted via Synod / manually / by another path OUTSIDE `group_role_map` are NOT touched. This implements the intent "roles = external groups, group changes are reflected in RBAC" (previously reconciliation was grant-only -> permanent escalation: removal from a group at the IdP did not revoke the role). `granted_by_aid=NULL` (federated membership with no initiator); RBAC authority is the membership table, not the JWT claim `roles` (ADR-028(c)). Roles are deduplicated and sorted stably.
  - **Provisioning (RESOLVED - auto-provision by groups, fork #1):** the first federated login **creates** an `operators` row (`auth_method=ldap`), **if** the user's groups intersect `group_role_map`; outside those groups - rejection `ErrNoRoleMapping` (403, operator is NOT created). Roles are taken from the groups. Writes audit `operator.provisioned`.
  - **`created_by_aid` for federated (bootstrap invariant ADR-013 - RESOLVED via `created_via`).** The prior problem: the partial unique index `operators_first_archon_idx` (migration 003) enforced "exactly one row with `created_by_aid IS NULL`" (the first bootstrap-Archon), and federated-provision **could not** set `NULL` (it would break the invariant on the second federated user) and could not set self (CHECK `self_reference_ok`). The previous workaround - `created_by_aid = 'archon-system'` (`auth.FederatedSourceAID`) - **has been removed**. Solution: the registry got a new field **`created_via`** ([ADR-014 amendment 2026-06-23](0014-operator-identity.md#adr-014-operator-identity-model-archon), domain `{bootstrap,user,ldap,oidc,system}`, migration 084), the bootstrap invariant was moved onto a partial-index `WHERE created_via='bootstrap'` (migration 085). After this relaxation **`created_by_aid=NULL` is legal for non-bootstrap rows**, and federated-provision writes `created_by_aid=NULL` + `created_via='ldap'` (the source is attributed by `created_via` itself, not by a surrogate FK to `archon-system`). The CHECK `self_reference_ok` remains. `archon-system` was legally seeded by migration **086** (`created_via='system'`, `created_by_aid=NULL`) and continues to be the FK anchor for push auto-import (`push.AutoImportSystemAID` writes `push_providers.created_by_aid='archon-system'`) - but the federated mapper no longer uses it.
  - **Revoked check (IMPLEMENTED):** federated login for an operator with `revoked_at != nil` is rejected (`ErrOperatorRevoked` -> 403) BEFORE the JWT is issued - revocation cannot be bypassed by an external IdP.
  - **Auth-method-match (account-takeover invariant, CRIT-fix 2026-06-24):** federated login of an existing operator is rejected if its `operators.auth_method` != the mapper's method (`ErrAuthFailed` -> 401, anti-oracle - the reason is not leaked outward). The federated path serves ONLY operators enrolled by the SAME federated method. The bootstrap-Archon (`auth_method=jwt`), `archon-system` (`jwt`), mTLS operators, and an operator enrolled via a DIFFERENT federated method are NOT assigned a matched derived-AID. Without this check, a controlling external IdP could issue itself a derived-AID equal to the AID of a privileged operator (e.g. a bootstrap cluster-admin) and take over that operator's session.
  - **Audit separation:** `operator.provisioned` is written by the Mapper (the fact of row creation); `operator.login` is written by the endpoint after JWT issuance (one login event per login, no duplication). Both - `source: api`, payload without password/bind-creds.

  > **Seeding `archon-system` - CLOSED (2026-06-23, previously needs_architect).** The prior blocker: the system operator `archon-system` had to have a `created_by_aid`, which could not be `NULL` (conflict with the old bootstrap index `WHERE created_by_aid IS NULL`) and could not be self (CHECK). The architect's decision has been approved and implemented - **relaxation of the bootstrap index via `created_via`**: the index was moved onto `WHERE created_via='bootstrap'` (migration 085), after which `archon-system` was seeded by migration **086** (`created_via='system'`, `created_by_aid=NULL`, `ON CONFLICT DO NOTHING`) **legally**, without violating the uniqueness of the first Archon. **End-to-end auto-provision WORKS:** the first federated login of a new operator creates an `operators` row (`created_by_aid=NULL`, `created_via='ldap'`) - role mapping, cookie, and the revoked invariant are all implemented and tested. The previous workaround `auth.FederatedSourceAID` (federated wrote `created_by_aid='archon-system'`) has been removed.

- **(e) Config schema (new blocks `auth.ldap` / `auth.oidc`, secrets - Vault KV).** Symmetric to the existing blocks ([keeper.go](../../shared/config/keeper.go): `KeeperRedis.password_ref`, `KeeperVault`, `KeeperAuthJWT.signing_key_ref`). All secrets go through `*_ref` (`vault:<mount>/<path>[#field]`, resolved at load-time same as `password_ref`/`dsn_ref`). TLS is required.

  ```yaml
  auth:
    jwt:                                  # already exists (ADR-014) - internal token issuer
      signing_key_ref: vault:secret/keeper/jwt-signing-key
      issuer: keeper.example.com
      ttl_default: 8h

    ldap:                                 # NEW (opt. block)
      url: ldaps://ldap.example.com:636   # ldaps:// or ldap:// (the latter only with start_tls)
      start_tls: false                    # StartTLS over ldap:// (mutually exclusive with ldaps://)
      tls:
        ca_ref: vault:secret/keeper/ldap-ca   # CA bundle for LDAPS (opt., if not the system one)
        insecure_skip_verify: false       # dev-only, semantic-WARN
      bind_mode: search                   # search | direct
      bind_dn: cn=svc-keeper,ou=svc,dc=example,dc=com   # service account (search mode)
      bind_password_ref: vault:secret/keeper/ldap-bind  # service account password
      base_dn: dc=example,dc=com
      user_filter: (uid=%s)               # %s = entered username
      user_dn_template: uid=%s,ou=people,dc=example,dc=com  # direct mode
      group_filter: (member=%s)           # %s = found user-DN
      group_attr: cn                      # group attribute -> name for role-map
      aid_attr: uid                        # attribute -> AID (or email)
      timeout: 10s
      group_role_map:                      # external group -> RBAC roles
        keeper-admins: [cluster-admin]
        keeper-ops: [operator]

    oidc:                                 # NEW (opt. block, requires Redis)
      issuer: https://keycloak.example.com/realms/soulstack   # discovery base (HTTPS-only)
      client_id: soul-stack-keeper
      client_secret_ref: vault:secret/keeper/oidc-client-secret  # opt. (public client can go without it)
      redirect_url: https://keeper.example.com/auth/oidc/callback
      scopes: [openid, email, profile, groups]
      tls:
        ca_ref: vault:secret/keeper/oidc-idp-ca   # opt. custom CA for the IdP
      aid_claim: sub                       # claim -> AID (default sub - immutable; email/preferred_username -> semantic-WARN, identity-spoofing risk)
      groups_claim: groups                # claim -> list of groups for role-map (default groups)
      group_role_map:
        /keeper-admins: [cluster-admin]
        /keeper-ops: [operator]

    rate_limit:                           # NEW (opt., anti-bruteforce login, HIGH-fix; default-ON, requires Redis)
      enabled: true                       # default-ON (footgun-guard like Tempo/Toll)
      rate: 0.5                           # token-bucket: attempts/sec per principal (IP/username)
      burst: 10                           # bucket depth of attempts
      lockout_threshold: 5                # failures in the window -> block
      lockout_window: 15m                 # failure-counting window
      lockout_backoff: 15m                # block duration
  ```

  > PKCE (S256) is enabled by the implementation always - there is no `use_pkce` config flag (not left to the operator's choice, ADR-058 fork #6 resolved in favor of "mandatory").

  Both blocks are **optional** ([ADR-053](0053-dependency-tiers.md) tier: OPTIONAL-with-degradation - if not set, that login method is simply unavailable, Keeper still starts; OIDC additionally requires Redis for the flow-store). Semantic validation of `auth.ldap` **is implemented** (`shared/config/semantic.go::checkAuthLDAP`): TLS-required (`ldaps://` OR `ldap://`+`start_tls`, otherwise ERROR `ldap_plaintext_forbidden`), mutual exclusivity of `ldaps://`+`start_tls` (ERROR `ldap_tls_conflict`), `bind_mode=search` (or empty=default) requires `bind_dn`+`bind_password_ref`, `*_ref` via `checkVaultRef`, `timeout` - duration format, `insecure_skip_verify=true` -> WARN. Semantic validation of `auth.oidc` **is implemented** (`checkAuthOIDC`): `issuer` HTTPS-only (ERROR `oidc_issuer_not_https`), `client_id`/`redirect_url` required (ERROR `oidc_client_id_required`/`oidc_redirect_url_required`), `client_secret_ref`/`tls.ca_ref` via `checkVaultRef` (secret optional for a public client). Resolution of `*_ref` from Vault + discovery -> `auth/{ldap,oidc}.Config` - at load-time in the daemon (`setupLDAPAuth`/`setupOIDCAuth`, mirroring `bootstrap.LoadSigningKey`). Hot-reload - via the same `Store[T]` (ADR-021).

- **(f) API surface (new public `/auth/*` endpoints outside `/v1`).** Registered at the root level of the chi router (parity with `/healthz`/`/docs`/`/ui` - WITHOUT `RequireJWT`/RBAC, see [router.go](../../keeper/internal/api/router.go)):

  | method + path | auth | purpose |
  |---|---|---|
  | `GET /auth/methods` | public | backend-driven catalog of available login methods ([ADR-042](0042-backend-driven-ui.md)): which of `password`/`ldap`/`oidc` are configured; for the UI login form - **follow-up** |
  | `GET /auth/oidc/login` | public | **IMPLEMENTED:** start of the OIDC code-flow with PKCE (302 redirect to the IdP) |
  | `GET /auth/oidc/callback` | public | **IMPLEMENTED:** OIDC callback, id_token validation, issuance of the internal JWT in a cookie (302 to /ui) |
  | `POST /auth/ldap/login` | public (HTTPS) | **IMPLEMENTED:** LDAP search-bind, issuance of the internal JWT in a cookie |

  `POST /auth/ldap/login` (`huma_auth.go`) and `GET /auth/oidc/{login,callback}` (`huma_oidc.go`) are implemented: ROOT-mounted outside `/v1` (parity with `/healthz`, without `RequireJWT`/RBAC/metrics; anti-DoS body-limit is in place), mounted only when `LDAPAuth`/`OIDCAuth` is non-nil (opt-in, like `pushH`/`errandH`; OIDC additionally requires Redis). FULL-TYPED huma operations; present in the committed `docs/keeper/openapi.yaml` (groups `prefix /auth` in `fullSpecGroups`). Protection: TLS-required, body limit, OIDC - `state` (single-use)/`nonce`/PKCE. A successful login writes audit `operator.login` (`source: api`, payload `{method, aid, provisioned}` - no secrets). `GET /auth/methods` (backend-driven catalog) + rate-limit/lockout by username (anti-bruteforce) - follow-up. The MCP surface is **untouched** - MCP accepts an already-issued internal JWT. OpenAPI - `/auth/*` are registered as huma operations, they land in `/openapi.yaml` automatically (ADR-054).

- **(g) Security model (security-critical, "security comes first").**

  - **TLS everywhere:** LDAPS/StartTLS mandatory (plaintext LDAP forbidden by config validation); OIDC IdP - HTTPS issuer only; `/auth/ldap/login` - only over the Keeper's HTTPS listener.
  - **OIDC id_token validation (IMPLEMENTED):** signature via JWKS, pin `iss`, pin `aud`==client_id, `exp`/`iat` (go-oidc verifier), `nonce` match (anti-replay), `state` match single-use via GETDEL (anti-CSRF + anti-replay code), mandatory PKCE S256 (anti code-interception). Any rejection reason is sanitized into `auth.ErrAuthFailed` outward (anti-oracle); details go to a debug log without tokens. Guard tests: `keeper/internal/auth/oidc/oidc_test.go` (mock IdP on TLS-httptest + JWKS) cover bad-signature/expired/wrong-aud/nonce-mismatch/state-mismatch/single-use/PKCE-enforced.
  - **Secrets not in logs:** `bind_password`/`client_secret`/entered passwords/issued JWT are masked (like `OperatorCreateReply.JWT`, ADR-014); resolved via Vault, no plaintext in the config.
  - **Anti-bruteforce (IMPLEMENTED, HIGH-fix 2026-06-24):** public login endpoints (`POST /auth/ldap/login`, `GET /auth/oidc/{login,callback}`) are protected by the `AuthLoginLimit` middleware on top of the `LoginGuard` Redis primitive (cluster-shared): (a) **rate throttle** of attempts (token-bucket) per-IP + per-username - dampens flooding, including flow-state flooding at `/auth/oidc/login`; (b) **lockout** after N failed logins within a window (for IP and username independently) for a backoff interval. Parameters - the `auth.rate_limit` block (`rate`/`burst`/`lockout_threshold`/`lockout_window`/`lockout_backoff`, default-ON, defaults `config.DefaultAuthLogin*`). **Fail-CLOSED on lockout** (a Redis error is treated as "blocked": login is a security perimeter, Redis unavailability must not open the door to bruteforce - in CONTRAST to Tempo's fail-open), **fail-open on throttle** (login-page availability). A successful login does not touch the failure counter. Rejection - `429` + `Retry-After` + problem-type `auth-throttled` (anti-oracle: detail without scope/reason). The client IP is taken from `RemoteAddr` (X-Forwarded-For is not trusted without trusted-proxy configuration - not present yet, so behind an L7-proxy the main protection is carried by the per-username layer). nil-Redis -> passthrough (login without throttle, OPTIONAL-tier). Guard tests - `keeper/internal/api/middleware/authlimit_test.go` + integration `keeper/internal/redis/loginguard_integration_test.go`.
  - **Timeouts:** on LDAP-connect/bind, OIDC token-exchange, JWKS-fetch - so that IdP unavailability does not hang Keeper.
  - **Revocation invariant:** a revoked operator cannot log in via the federated path (`revoked_at` check before JWT issuance).
  - **Internal JWT unchanged:** federated methods reuse `jwt.Issuer` (ADR-014) - a single format, single revocation, short TTL. The external token does NOT outlive the moment of login.
  - **JWT delivery (RESOLVED - cookie-only, fork #4; SameSite unified 2026-06-24):** the internal JWT is delivered **only** in a `Set-Cookie` `soul_session` with `HttpOnly` + `Secure` + **`SameSite=Strict`** + `Path=/` - by a SINGLE factory `newSessionCookie` for both LDAP AND OIDC (symmetry of methods is mandatory). **Strict is safe for OIDC-callback too** (the prior Lax<->Strict mismatch has been eliminated): `SameSite` restricts the SENDING of the cookie on a cross-site request, not its SETTING; on a cross-site top-level redirect from the IdP the cookie IS SET (Set-Cookie on the callback response), the next step is a same-site top-level navigation to `/ui` (302), on which the Strict cookie IS SENT. The previous `Lax` on OIDC was an unnecessary relaxation. There is NO JSON token in the response body (minimizes XSS exfiltration). Cookie TTL = `auth.jwt.ttl_default`. Programmatic token issuance (CI/scripts) remains via the existing `POST /v1/operators/{aid}/issue-token` (ADR-014).
  - **AID-claim immutability (OIDC, security-fix 2026-06-24):** the default `aid_claim` is **`sub`** (the IdP's immutable opaque subject). Semantic validation (`checkAuthOIDC`) issues a **WARN** (`oidc_aid_claim_mutable`) if `aid_claim` in {`email`, `preferred_username`}: these claims are user/admin-mutable at the IdP, and reassigning them would let a different human log in under an existing AID (identity-spoofing). WARN, not ERROR - the operator can make a deliberate choice (e.g. a stable corporate email), security = quality of the hint, not a ban.
  - **Refresh (RESOLVED - none, fork #5):** there is no refresh mechanism; once the short internal JWT expires - a repeat federated login.

- **(h) Libraries and licenses (core is BSL 1.1, [ADR-016](0016-parity-license.md); dependencies are permissive, includable into the core distribution).**

  | library | purpose | license | includable into the core |
  |---|---|---|---|
  | `github.com/coreos/go-oidc/v3` | OIDC discovery / JWKS / id_token verify | Apache-2.0 | yes |
  | `golang.org/x/oauth2` | OAuth2 code-exchange | BSD-3-Clause | yes |
  | `github.com/go-ldap/ldap/v3` | LDAP bind / search | MIT | yes |
  | `github.com/golang-jwt/jwt/v5` | internal JWT (already in the project, ADR-014) | MIT | yes |

  All are under permissive licenses (Apache-2.0 / BSD-3 / MIT) - includable into the core distribution under BSL 1.1; CLA - per [ADR-016](0016-parity-license.md) (set up before the first external contributor). `go-ldap/v3` **has been added** to `keeper/go.mod` (stage 1). `go-oidc/v3` + `x/oauth2` **have been added** to `keeper/go.mod` (stage 2). Tests use `github.com/go-jose/go-jose/v4` (a transitive dependency of go-oidc, Apache-2.0) to sign id_tokens for the mock IdP.

- **(i) Operator provisioning policy (IMPLEMENTED, Part B).** A cluster-level gate on "which methods are even allowed to CREATE an operator" - separate from which login methods are configured.

  - **Key `provisioning_allowed_methods`** - a well-known scalar in `keeper_settings` ([ADR-029](0029-service-registry.md#adr-029-service-registry--postgres), key-value cluster-settings; parity with `default_destiny_source`). Value - **CSV from the domain `{user, ldap, oidc}`** (methods for CREATING an operator). **`bootstrap` and `system` are NEVER gated** (not part of the key's domain) - `keeper init` and the system seed of `archon-system` must always work (first-level anti-lockout). Resolution - a synchronous snapshot of `serviceregistry.Holder` (the ADR-029(b) pattern, getter `ProvisioningMethodAllowed(method)` / `ProvisioningPolicy()`), TTL-poll + `service:invalidate`.
    - **ABSENCE of the key** = all methods are allowed (back-compat - the pre-Part-B behavior is preserved).
    - **Set-but-EMPTY** (csv `""` / commas only) = **config-error** at snapshot load (`ErrEmptyProvisioningMethods`) - second-level anti-lockout: you cannot forbid ALL creation methods and thereby lock out operator enrollment. Unreachable at runtime (PUT validates BEFORE writing).
    - A method outside `{user,ldap,oidc}` -> `ErrInvalidProvisioningMethod`.
  - **Gating ONLY on the operator-CREATION branch:** `POST /v1/operators` (method `user`) and federated auto-provision (method `ldap`/`oidc`, mapper). **An existing operator can log in even if its method is subsequently forbidden** - the policy does not revoke access, it only forbids enrolling new ones. Federated login of an existing `operators` row does not pass through the method gate.
  - **Runtime API** - `GET`/`PUT` `/v1/provisioning-policy` (RBAC permission `provisioning.read` / `provisioning.update`; selector NoSelector - cluster-level, like `operator.*`/`role.*`). `GET` - read without audit (`policy_set=false` -> key not set, default "everything allowed"). `PUT` - replace semantics, writes audit `provisioning.policy_changed`; an empty list -> 422, a method outside the domain -> 422 (problem-type `provisioning-method-disabled` for a rejection on creation; `validation-failed`/422 for PUT errors). Mounted only when a non-nil reader/gate is present (opt-in, parity with push/errand routes).
  - **Rejection on creation** returns `application/problem+json` with problem-type **`provisioning-method-disabled`** (`TypeProvisioningMethodDisabled`).

- **What is NOT included (boundaries).** Not part of this ADR: SAML, WebAuthn/passkeys, mTLS authentication of an operator (ADR-014 already reserves `mtls`/`combined` separately), SCIM provisioning/deactivation, refresh-token rotation of external tokens (the internal JWT is short - re-login instead), per-IdP multi-tenancy (one LDAP + one OIDC in the MVP scope of this ADR). These are separate ADRs on real demand.

- **Forks.**

  Resolved (stage 1, LDAP):

  1. **Auto-provision vs pre-register -> RESOLVED: auto-provision by groups.** The first login creates the operator if there is an intersection of groups with `group_role_map`; outside those groups - rejection (not created).
  2. **Source of roles -> RESOLVED: external groups.** Roles come from `group_role_map`, synced into `rbac_role_operators`.
  4. **JWT delivery form -> RESOLVED: HttpOnly+Secure+SameSite=Strict cookie `soul_session`** (LDAP AND OIDC symmetrically, by a SINGLE factory - the Lax<->Strict mismatch was eliminated 2026-06-24, see (g)). No JSON token in the body. Programmatic issuance - via the existing `operator issue-token`.
  5. **Refresh -> RESOLVED: none.** Short internal JWT + re-login.
  7. **LDAP bind mode -> RESOLVED: search-bind** (the only one in stage 1; direct-bind deferred without a breaking change).
  8. **LDAP library -> RESOLVED: `github.com/go-ldap/ldap/v3`.**
  9. **Config form `auth.ldap` -> RESOLVED** (the YAML schema from (e) approved).

  Resolved (stage 2, OIDC):

  3. **Enum-value name -> RESOLVED: `oidc`** (locked in; Keeper relies on the OIDC id_token, not on a plain OAuth2 access-token).
  6. **PKCE for OIDC -> RESOLVED: mandatory** (S256 always-on, no config flag - not left to the operator's choice; protection against code-interception comes first).
  - **OIDC libraries -> RESOLVED: `go-oidc/v3` + `x/oauth2`** (added to `keeper/go.mod`).

  Resolved (stage 1, seeding + provisioning):

  10. **Seeding `archon-system` without breaking the bootstrap index -> RESOLVED** (previously needs_architect): relaxation of the bootstrap partial-unique index onto `WHERE created_via='bootstrap'` (migrations 084/085) legalizes `created_by_aid=NULL` for non-bootstrap rows; `archon-system` seeded by migration 086. End-to-end auto-provision completed. See (d).
  11. **Provisioning policy -> RESOLVED** (see (i)): well-known `provisioning_allowed_methods` (CSV `{user,ldap,oidc}`, absent=all, empty=config-error), runtime API `/v1/provisioning-policy`, permission `provisioning.read`/`provisioning.update`, audit `provisioning.policy_changed`, gating only on the operator-creation branch.

---

> **Relation to other ADRs.** Amends: [ADR-014](0014-operator-identity.md) (extension of the `auth_method` enum + field `operators.created_via` + provisioning audit - amendment 2026-06-23), [ADR-013](0013-bootstrap-archon.md) (bootstrap invariant via `created_via='bootstrap'` - amendment 2026-06-23), [ADR-029](0029-service-registry.md) (well-known key `keeper_settings.provisioning_allowed_methods`). Reuses: [ADR-013](0013-bootstrap-archon.md) (bootstrap remains the lower trust layer), [ADR-006](0006-cache-redis.md) (Redis for the state/nonce store), [ADR-042](0042-backend-driven-ui.md) (`/auth/methods` catalog), [ADR-053](0053-dependency-tiers.md) (LDAP/OIDC - OPTIONAL tier), [ADR-054](0054-openapi-code-first.md) (huma registration of `/auth/*`). RBAC ([ADR-028](0028-rbac-storage.md)/[ADR-047](0047-purview.md)) - UNCHANGED (new `provisioning.*` permissions are registered in the catalog, the enforcer mechanics do not change).
