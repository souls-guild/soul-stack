# ADR-014. Operator identity model (Archon)

- **Context.** Currently in docs/ the operator name appears as a plain string in `roles[].operators` ([rbac.md](../keeper/rbac.md)) and in the FK fields `created_by_operator_id` / `changed_by_operator_id` in several tables ([`soul/identity.md`](../soul/identity.md), `incarnation`, `state_history`) — but **the `operators` table itself does not exist in the Postgres schema**. The FKs reference "nowhere". [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-the-first-archon) fixes the mechanism of the first Archon; this ADR settles the identity shape, the registry and the audit.
- **Decision.**

  **(a) The `operators` registry in Postgres — mandatory.** Fields at minimum:
  - `aid` — primary key, a kebab-case string (`archon-alice`).
  - `display_name` — a human-readable name.
  - `auth_method` — enum: `jwt` (MVP), `mtls` / `combined` (post-MVP — without a breaking change, via a migration).
  - `created_at` — timestamp.
  - `created_by_aid` — FK to `operators(aid)`; for the first Archon `NULL` (invariant: exactly one record with `created_by_aid IS NULL`, allowed only at bootstrap).
  - `revoked_at` — timestamp or `NULL` (active).
  - `metadata` — `jsonb`, for future extensions (email, MFA flags, etc.).

  The FK fields `created_by_operator_id` in other tables are renamed to `created_by_aid` / `changed_by_aid` and become real FKs to `operators(aid)`.

  Storing the registry in Postgres (rather than in `keeper.yml`) gives hot-add of operators via OpenAPI/MCP, a proper audit trail, and working FKs.

  **RBAC storage moved to the DB ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** At the moment ADR-014 was fixed, the roles / permissions / "operator ↔ role" binding remained in `keeper.yml::rbac` — this produced BUG-1 (init creates the Archon in the DB, but writes the membership into a JWT claim that the enforcer does not read, resolving it from YAML instead; see [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)). With ADR-028 all RBAC is in Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`). **Disambiguating the term "FK":** the real PG foreign keys are `created_by_aid` / `changed_by_aid` (this item) and `rbac_role_operators.aid` / `granted_by_aid` ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)); the former metaphorical "FK" of the YAML list `roles[].operators` is a **membership** (binding), now materialized by a `rbac_role_operators` row rather than a reference in a file. The JWT claim `roles` ([ADR-014(b)](#adr-014-operator-identity-model-archon)) stops being the source of membership — the authority for membership is the `rbac_role_operators` table.

  **Amendment (2026-06-09, Synod — a group of archons [ADR-049](0049-synod.md#adr-049-synod--a-group-of-archons)).** "Archon ↔ role" membership is no longer exhausted by direct `rbac_role_operators` rows: an intermediate level **[Synod](0049-synod.md#adr-049-synod--a-group-of-archons)** is introduced (a group of archons that bundles roles) — the model **Archon → Synod → Roles**. **An archon's effective roles = direct (`rbac_role_operators`) ∪ roles via all of its Synods** (`synod_operators` ⋈ `synod_roles`). The union is assembled during the enforcer's snapshot build ([ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)); the JWT claim `roles` is still not the source of membership. Wherever in this ADR / reconciliation it says "an operator's effective rights/roles" (audit `archon.aid`, the self-lockout invariant) — it now reads as "direct ∪ via Synod".

  **(b) Credential shape — JWT (MVP).** `keeper init` and subsequent API calls issuing new Archons return a JWT token:
  - Claims: `iss: <kid-cluster>`, `sub: <aid>`, `iat`, `exp`, `roles: [...]`, `bootstrap_initial: true|false`.
  - Signing key — keeper-side, stored in **Vault KV** (for MVP) under the path `secret/keeper/jwt-signing-key`. Post-MVP — Vault Transit for signing without exporting the key.
  - **The Keeper's own authentication to Vault** (to read the signing key and other `*_ref`) — `vault.auth.method`: `token` (dev/local, a static token) and **`approle` (implemented)** for production. AppRole credentials are taken locally (`role_id` inline + `secret_id_file`/`secret_id_env`), not from Vault — otherwise a circular dependency. The obtained client token is renewed in the background. The field spec — [`docs/keeper/config.md → vault`](../keeper/config.md#vault). Vault Transit for signing and mTLS auth Keeper↔Vault — post-MVP.
  - TTL of the first bootstrap token — **30 days** (longer than usual, so the operator has time to set up further administration). Ordinary post-bootstrap tokens are shorter (recommendation: 24h with refresh, concrete TTLs — in `keeper.yml → auth:`).
  - `Authorization: Bearer <jwt>` on all HTTP/MCP calls.
  - On the Keeper side — JWT middleware on all OpenAPI/MCP listeners; the payload `sub` → AID → RBAC check.

  An mTLS cert for machine identity (CI, MCP agents) and the combined shape are a separate post-MVP task. The extension is implemented via the `auth_method` enum in `operators` without a breaking change.

  **(c) Identifier — AID (Archon ID).** Symmetric to [SID/KID](../naming-rules.md#identifiers). A lowercase ASCII string, human-readable. Free of conflicts with standards (see [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-the-first-archon)(a)). Validation: `^[a-z0-9][a-z0-9._@-]{1,127}$` — the first character is a letter/digit, then the charset `a-z 0-9 . _ @ -`, total length 2..128 (examples: `archon-alice`, `archon-ops-01`, `archon-ci-deployer`, the email-like `alice@corp.com`, the ldap-uid `uid-4815`).

  **Amendment (2026-05-29): the mandatory `archon-` prefix is removed, the charset is widened.** The former shape `^archon-[a-z0-9-]{1,62}$` required a hard prefix and did not allow `. _ @`. Under LDAP/Keycloak auto-provision of external identities (where `sub`/`uid` is `alice@corp.com` or `uid-4815`) the AID must directly hold the external name without an artificial wrapper. The new charset is deliberately safe: no `/`/`\` (path traversal — the AID is embedded in bootstrap-token file names), ASCII-lowercase only (no unicode look-alikes and no case), no control chars/quotes (no injection); starting with an alphanumeric excludes hidden `..`/`.` prefixes. Previous AIDs of the form `archon-<...>` remain valid (the charset is a superset). Identity protection **no longer** relies on the presence of the `archon-` prefix, but on the strict charset + start with an alphanumeric. **External identity mapping** (issuer / external_subject → aid) is **not introduced** by this amendment: it is provided for via the existing `metadata` jsonb + `auth_method` enum, with materialization by a separate later ADR at the first real request, so a new entity is not spawned now.

  **(d) Archon lifecycle.**
  - **Creation:** via `keeper init` (the first Archon only) or via OpenAPI/MCP with the RBAC permission `operator.create` (for the rest). The API returns a JWT token; a repeated JWT request for an existing Archon is a separate endpoint `operator.issue-token` with auth from another Archon holding the `operator.issue-token` right.
  - **Revocation:** via OpenAPI/MCP with the `operator.revoke` permission — sets `revoked_at`. Active JWT tokens of a revoked Archon keep working until their `exp` (a short TTL is natural protection; forced revocation of "all live JWTs" is a separate post-MVP task, requiring a JWT blocklist or session store).
  - **Deleting a record** — not provided for. Archons are only revoked (for auditing, `created_by_aid` must remain a valid FK).

  **(e) Audit trail.**
  - The `operators` registry itself journals creation/revocation.
  - All mutations by an Archon of other entities write `changed_by_aid` / `created_by_aid` — this is an FK, the invariant is maintained by the DB.
  - OTel: every API/MCP call after successful JWT authentication has the attribute `archon.aid=<aid>`.
  - Archon lifecycle audit events (`operator.created` / `operator.revoked` / `operator.access_denied` etc.) are written to the common audit pipeline — a single normalization of storage, schema, write-path and retention — [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention).

- **Consequences.**
  - The `operators` table is a mandatory registry in Postgres ([keeper/storage.md](../keeper/storage.md) is extended).
  - FK fields in `souls`, `bootstrap_tokens` (Soul-side), `incarnation`, `state_history`: rename `created_by_operator_id` → `created_by_aid`, add an FK to `operators(aid)`. This requires a migration (a separate task — while there is no code yet, the migration will be the first one).
  - `keeper.yml` gains an `auth:` block with JWT settings (signing-key Vault path, default TTL, JWT issuer name). Details — a separate task, not part of this ADR.
  - Vault must contain `secret/keeper/jwt-signing-key` before the Keeper starts (or the Keeper generates and puts it during `keeper init` — an implementation fork, not a blocker).
  - The shape of the Operator API (management of Archons + the rest of the endpoints under permissions) is defined by **the Go types of the handlers** (huma v2 full-typed, code-first, [ADR-054](0054-openapi-code-first.md#adr-054-operator-api---reversal-to-code-first-go-types---openapi-via-huma-v2) replaced [ADR-051](0051-operator-api-codegen.md#adr-051-operator-api-codegen-openapi--go-types-oapi-codegen-types-only--strict)); the OpenAPI spec ([`docs/keeper/openapi.yaml`](../keeper/openapi.yaml)) is a derived snapshot, oapi-codegen is gone. Transport — REST/HTTP, without a gRPC service. The former `proto/operator/v1/*.proto` is abolished (amendment [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules)). The markdown normalization of the HTTP facade and the endpoint ↔ MCP-tool ↔ permission mapping — [`docs/keeper/operator-api.md`](../keeper/operator-api.md).
- **Trade-offs.**
  - JWT without a revocation blocklist means a revoked Archon can work until the end of its token's TTL. For MVP we accept this — the TTL is short (24h by default for non-bootstrap tokens), the blast radius is limited.
  - Hot-add of operators via the API requires at least one Archon with the `operator.create` right to exist — a natural consequence, not a contradiction.
  - `created_by_aid IS NULL` only for the first Archon — this is a data invariant, it must be maintained by a partial unique index in Postgres (`CREATE UNIQUE INDEX ON operators ((created_by_aid IS NULL)) WHERE created_by_aid IS NULL`).
  - A JWT token in a `mode 0400` file after `keeper init` — the operator must reliably save it before restarting the Keeper; a "lost bootstrap token" can be recovered only via a manual SQL operation (or `keeper init --reissue-token --force` — a separate task).

**Amendment 2026-05-27: near-instant revocation via the RBAC snapshot.**

ADR-014(d) originally marked "forced revocation of all live JWTs" as
post-MVP. At the production-release gate the case "a fired employee
retains JWT access until exp" was raised. The decision — not to introduce a new
JWT-blocklist / refresh-token entity, but to extend the already existing
RBAC-snapshot mechanism (B2, `rbac:invalidate`, [ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)):

  - `operators.revoked_at` is read in `rbac.LoadSnapshot` as a fourth
    projection `Revoked map[string]time.Time`.
  - `rbac.Enforcer.Check` at the start of the method rejects requests from a revoked
    AID with a new sentinel `ErrOperatorRevoked`.
  - `operator.service.Revoke` after a successful UPDATE publishes
    `rbac:invalidate` (the same topic as role mutations) — all Keeper instances
    rebuild the snapshot in milliseconds.
  - Fail-soft fallback — TTL poll `rbac.DefaultRefreshInterval` (10s).
  - Middleware mapping: a revoked AID on verify → 401 (parity with expired);
    a separate `TypeOperatorRevokedToken` (URN `…/errors/operator-revoked-token`),
    so as not to overlap with the 409 `TypeOperatorRevoked` on the write side
    (IssueToken/Revoke for an already-revoked AID).

The window of real unblocking is single-digit milliseconds with a healthy Redis,
up to 10 seconds on loss of a pub/sub message.

**JWT TTL remains defense-in-depth.** Recommendation — lower the default
`auth.jwt.ttl_default` to 1h in production.

**Amendment 2026-06-23: the `operators.created_via` field + moving the bootstrap invariant + extending the `auth_method` enum.**

Under the operator provisioning model ([ADR-058](0058-operator-auth-ldap-oidc.md#adr-058-federated-operator-authentication-archon---ldap--oauth2oidc)):

- **The `auth_method` enum is extended (only-add)** with the values `ldap` / `oidc` (SQL CHECK `auth_method_valid`, migration 083) — an operator created via a federated method records with it its login method; the internal JWT after issuance is identical (shapes (a)/(b) above are unchanged). This is an additive extension in the spirit of the `mtls`/`combined` already declared in (a). **Security invariant (CRIT-fix 2026-06-24, ADR-058(d)):** federated login serves ONLY operators with a matching `auth_method` — bootstrap/system (`jwt`), mTLS and an operator of another federated method are NOT assigned to a matched derived AID (anti account-takeover).

The `operators` registry gains a new field:

- **`created_via`** — TEXT NOT NULL DEFAULT `'user'`, domain `{bootstrap, user, ldap, oidc, system}` (CHECK `created_via_valid`). Semantics — **"where the operator was created from"**, orthogonal to `auth_method` (**"what the operator logs in with"**). Examples: bootstrap Archon (`keeper init`) → `created_via='bootstrap'`, `auth_method='jwt'`; an operator via `POST /v1/operators` → `'user'`; LDAP auto-provision → `'ldap'`; the system `archon-system` → `'system'`. DEFAULT `'user'` — a safe fallback for rows created via the Operator API. Introduced by migration **084** (ADD COLUMN + CHECK + reconcile of existing rows: `created_by_aid IS NULL → 'bootstrap'`, `aid='archon-system' → 'system'`).

- **The bootstrap invariant is moved to `created_via`.** The former formulation ((c) / Trade-offs above) — "exactly one record with `created_by_aid IS NULL`", guaranteed by the partial unique index `operators_first_archon_idx` `WHERE created_by_aid IS NULL`. Now the invariant reads as **"exactly one operator with `created_via='bootstrap'`"**, the index is moved to `WHERE created_via='bootstrap'` (migration **085**). Consequence: federated operators (`created_via='ldap'/'oidc'`) and system ones (`created_via='system'`) set **`created_by_aid=NULL` legally** — previously this broke the bootstrap index on the second such operator. The CHECK `self_reference_ok` (`created_by_aid != aid`) **remains** unchanged. Wherever above in this ADR it says "`created_by_aid IS NULL` only for the first Archon" — it now reads as "`created_via='bootstrap'` only for the first Archon".
