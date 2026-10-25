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
  - TTL of the first bootstrap token — **30 days** (longer than usual, so the operator has time to set up further administration). Ordinary post-bootstrap tokens are shorter (recommendation: 24h with refresh, concrete TTLs — in `keeper.yml → auth:`). A caller may ask for a longer one, up to `auth.jwt.ttl_max` (amendment 2026-10-24, NIM-1045).
  - `Authorization: Bearer <jwt>` on all HTTP/MCP calls.
  - On the Keeper side — JWT middleware on all OpenAPI/MCP listeners; the payload `sub` → AID → RBAC check.

  An mTLS cert for machine identity (CI, MCP agents) and the combined shape are a separate post-MVP task. The extension is implemented via the `auth_method` enum in `operators` without a breaking change.

  **(c) Identifier — AID (Archon ID).** Symmetric to [SID/KID](../naming-rules.md#identifiers). A lowercase ASCII string, human-readable. Free of conflicts with standards (see [ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-the-first-archon)(a)). Validation: `^[a-z0-9][a-z0-9._@-]{1,127}$` — the first character is a letter/digit, then the charset `a-z 0-9 . _ @ -`, total length 2..128 (examples: `archon-alice`, `archon-ops-01`, `archon-ci-deployer`, the email-like `alice@corp.com`, the ldap-uid `uid-4815`).

  **Amendment (2026-05-29): the mandatory `archon-` prefix is removed, the charset is widened.** The former shape `^archon-[a-z0-9-]{1,62}$` required a hard prefix and did not allow `. _ @`. Under LDAP/Keycloak auto-provision of external identities (where `sub`/`uid` is `alice@corp.com` or `uid-4815`) the AID must directly hold the external name without an artificial wrapper. The new charset is deliberately safe: no `/`/`\` (path traversal — the AID is embedded in bootstrap-token file names), ASCII-lowercase only (no unicode look-alikes and no case), no control chars/quotes (no injection); starting with an alphanumeric excludes hidden `..`/`.` prefixes. Previous AIDs of the form `archon-<...>` remain valid (the charset is a superset). Identity protection **no longer** relies on the presence of the `archon-` prefix, but on the strict charset + start with an alphanumeric. **External identity mapping** (issuer / external_subject → aid) is **not introduced** by this amendment: it is provided for via the existing `metadata` jsonb + `auth_method` enum, with materialization by a separate later ADR at the first real request, so a new entity is not spawned now.

  **(d) Archon lifecycle.**
  - **Creation:** via `keeper init` (the first Archon only) or via OpenAPI/MCP with the RBAC permission `operator.create` (for the rest). The API returns a JWT token; a repeated JWT request for an existing Archon is a separate endpoint `operator.issue-token` with auth from another Archon holding the `operator.issue-token` right.
  - **Revocation:** via OpenAPI/MCP with the `operator.revoke` permission — sets `revoked_at`. ~~Active JWT tokens of a revoked Archon keep working until their `exp` (a short TTL is natural protection; forced revocation of "all live JWTs" is a separate post-MVP task, requiring a JWT blocklist or session store).~~ **Superseded by the amendment 2026-08-07 below (NIM-421):** a revoked Archon is refused on the next request. The token is not invalidated cryptographically — no blocklist was needed — but every authenticated HTTP route consults the operator record through `RejectRevoked` and answers `401 operator-revoked-token`. **One token can be revoked without its operator** — `operator_jwts`, amendment 2026-10-24 (NIM-1045) below.
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
  - ~~JWT without a revocation blocklist means a revoked Archon can work until the end of its token's TTL. For MVP we accept this — the TTL is short (24h by default for non-bootstrap tokens), the blast radius is limited.~~ **Superseded 2026-08-07 (NIM-421):** the accepted trade-off turned out to be unnecessary. Revocation is enforced without a blocklist, because the RBAC snapshot already carries `revoked_at` and every authenticated request already consults it — what was missing was a gate that asked. The short TTL stays valuable for the *stolen*-token case, which revocation does not address.
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

**Amendment 2026-08-07 (NIM-421 / NIM-356): the 2026-05-27 promise held on
neither axis — the refusal nor the window.**

The amendment above states two things that a live sweep of a two-node stand
disproved. Both are now true by construction rather than by which middleware a
route happens to use.

- **"Middleware mapping: a revoked AID on verify → 401" was route-dependent.**
  It held only for routes gated by `RequirePermission` → `PermissionChecker.Check`,
  which can return `ErrOperatorRevoked`. Routes gated by the existence-gate
  `RequireAction` ([ADR-047](0047-purview.md)) ask `HoldsAction() bool` — a
  bare bool cannot carry a reason, so a revoked Archon got **403 "operator lacks
  required permission X"**: the wrong verdict *and* an unactionable one, since no
  grant can fix a revoked identity. Worse, four read routes
  (`/v1/me/permissions`, `/v1/permissions`, `/v1/event-types`,
  `/v1/herald-types`) carry no RBAC gate at all and kept answering **200**
  indefinitely. Measured: of 45 parameterless routes, 24 answered 401, 8
  answered 403 and 4 answered 200.
  **Decision:** revocation stops being a per-route concern. `RejectRevoked` is
  one middleware link installed immediately after `RequireJWT`, once per
  authenticated chain, so every route under it — including ones added later, and
  the two spec routes outside `/v1` — answers `401 operator-revoked-token`. The
  per-route gates keep their own verdicts for what they actually decide;
  "revoked" is no longer among them.

  **Scope of that claim: the HTTP surface.** MCP is a separate listener that
  verifies the JWT itself, so `RejectRevoked` does not run there. Every one of
  the 94 implemented tools *is* refused — each gates on the same
  revocation-aware `Check` — but the refusal is rendered `forbidden` /
  "operator lacks required permission X", because 87 call sites across 37 files
  discard `Check`'s error and hard-code that code. So on the tool surface the
  door is shut and mislabelled: the exact conflation this amendment removes from
  HTTP, left standing on the other front door (NIM-551).
  
  Two MCP paths are not merely mislabelled. `initialize` and `tools/list` carry
  no gate at all, so a revoked operator can still handshake and enumerate the
  catalog — no data, the MCP counterpart of the four ungated HTTP catalog
  routes. That one is NIM-551 and is still open.

  The second — `authorizeSSE` (`internal/mcp/sse.go`) returning `true` for an
  apply the caller started *before* it reaches `Check`, so a revoked operator
  kept streaming events for her own runs — is **CLOSED (NIM-858, 2026-09-14)**.
  It was worse than a read that survives revocation: the initiator branch is the
  only path on this listener that consults the RBAC surface at all, so nothing
  anywhere asked. `authorizeSSE` now asks revocation FIRST, and an established
  stream re-asks the same rule every 15s
  (`keeper/internal/api/middleware/longlived.go`) — so revocation reaches an open
  MCP stream on the same terms as every ordinary route.
  
  The timing half above is surface-independent: it is a property of the
  snapshot, so MCP gets it too.
- **"single-digit milliseconds with a healthy Redis" excluded the one node that
  mattered.** `rbac:invalidate` messages are self-filtered by `OriginKID`, and
  the publishing node had no local refresh — so the node that performed the
  revoke was the only node that did not learn about it, and fell back to the 10s
  TTL poll. Measured: a subscriber flipped in 0.04–0.06s, the origin took
  9.82 / 9.82 / 10.19s. Since the operator who pressed revoke is usually talking
  to that very node, the observed behaviour was the opposite of the promise.
  **Decision:** the invalidator refreshes the local snapshot before publishing.
  The wire-level self-filter stays correct (a node must not re-handle its own
  message); the fix is that the local half now happens at the source. It is wired
  unconditionally, so a Redis-less single-node stand gets it too. A failed
  refresh is logged and falls back to the TTL poll — the write has already
  committed and must not be reported as failed.

**Federated login answers 401 for a revoked operator too.** `/auth/ldap/login`
and `/auth/oidc/callback` mapped `ErrOperatorRevoked` to 403, so the same state
had two codes depending on which door it was met at. Both now answer
`401 operator-revoked-token`. `ErrNoRoleMapping` and `ErrProvisioningDisabled`
stay 403 — those are statements about permission, and this is the border: 401
means "this identity is not valid", 403 means "this identity may not do this".
No OpenAPI change — both endpoints already declared 401.

**Amendment 2026-10-24 (NIM-1045): every token is accounted for, one token can be revoked, and a caller may ask for a lifetime.**

A machine operator (the Service Desk dictionary service, NIM-1044) needs a token that outlives the 24h `ttl_default`, and the only stop a token had was revoking its operator — permanently, taking every other token with it. A long lifetime without a stop for one token was not acceptable. Options weighed with the user: `ttl` alone; `ttl` plus a per-operator cutoff; mTLS client certificates (the Operator API listener has no TLS at all); a Kubernetes ServiceAccount or Vault identity token exchanged for a Keeper JWT (no stored secret, but a new federated method — left for later, nothing here blocks it); OAuth2 client credentials; an opaque API key; self-refresh (a stateless pod loses the refreshed token). A rotation CronJob stays rejected: `operator.issue-token` has no target restriction, so it would hold the right to mint `cluster-admin`. Decision: account for every token, and let a caller ask for a lifetime up to a ceiling. The rest of the login model the user set out (local passwords, 7-day web sessions, personal tokens, a background LDAP group sync) is epic NIM-1049.

- **One row per token.** `operator_jwts` (migration 127): `jwt_id` (a ULID, the token's `jti`), `aid`, `kind` (`bootstrap` — `keeper init`; `issued` — `POST /v1/operators` and `issue-token`; `session` — the cookie of an LDAP/OIDC login, and since NIM-1051 of a local-password login), `label`, `created_at` / `expires_at`, `created_by_aid` (NULL for `bootstrap` and `session`), `created_ip` / `created_user_agent`, `last_used_at` / `last_used_ip` / `last_used_user_agent` (written at most once per 5 minutes per token per instance, off the request path), `revoked_at` / `revoked_by_aid`. The row is written **before** the token is signed, and `jwt.Issuer.Issue` requires the id, so a token no row can revoke cannot be minted by omission.
- **The exchanged Bearer has no row of its own.** `POST /auth/token` signs it under the session's `jti`, so revoking the session refuses both. A cookie minted before migration 127 has no `jti` to hand on; the exchange refuses it (`401`, sign in again) rather than mint a Bearer nothing could revoke.
- **Revoking one token.** `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke` and MCP `keeper.operator.jwt-revoke`, permission **`operator.jwt-revoke`** — separate from `operator.revoke` because revoking an operator is permanent and ends every token, revoking a token ends that token only. A token id of another operator answers 404, an already revoked one 409 `jwt-already-revoked`. The write publishes `rbac:invalidate` and then refreshes its own node's snapshot, both before answering — the same invalidator an operator revocation uses (`rbacInvalidator.Invalidate`; the NIM-421 text above says the local refresh comes first, the code publishes first and documents why), so the window is the same: milliseconds with Redis, at most the 10s TTL poll without — and, as for an operator revocation, a snapshot that fails to rebuild keeps the previous one until the RBAC data is fixed.
- **Where the refusal happens.** The RBAC snapshot gains a projection of revoked ids whose token may still verify (`expires_at` later than five minutes ago — the margin covers the clock gap between the database that answers the query and the instance that checks `exp`). The RBAC holder is the JWT **verifier's** revocation source (`NewVerifier` requires one), so a revoked `jti` fails verification on every door that verifies a token: the `/v1` chain and the spec routes, MCP `/mcp` and `/mcp/events`, and the cookie exchange — `401` problem type **`jwt-revoked`** (detail `token revoked`) over HTTP, `unauthenticated` on MCP. `RejectRevoked` repeats the check on `/v1`, and the long-lived channels (run-events SSE, `/mcp/events`, the console socket) re-ask it on their re-check interval as they do for operator revocation.
- **Asking for a lifetime.** `POST /v1/operators` and `issue-token` (HTTP and MCP) take an optional `ttl` in the `duration` convention (`2160h`, `90d`) and an optional `label`; omitted, the token lives `ttl_default` as before. The range is `[1m, auth.jwt.ttl_max]`. `ttl_max` defaults to `2160h` (90 days, the lifetime agreed for service accounts) — or to `ttl_default` when that is longer, so a cluster already running a longer default keeps starting; set explicitly below `ttl_default` it is refused by the config check and at startup. Replies carry `jwt_id`; create also carries `expires_at`.
- **Listing.** `GET /v1/operators/{aid}/jwts` (permission `operator.list`) and MCP `keeper.operator.jwt-list`: every row, newest first, never the token. Expired rows stay until the Reaper rule **`purge_operator_jwts`** deletes those expired more than `max_age` ago (default 30 days, floored at one hour so a revoked id never leaves the database inside the snapshot's clock margin). Like most Reaper rules it runs only when listed in `keeper.yml → reaper.rules` (it is not one of the default-on ones); the shipped example and dev configs list it.
- **Audit.** `operator.token-issued` carries `jwt_id`, `ttl` and `label`; `operator.created` carries `jwt_id` and `label` (no `jwt_id` for a `local` operator, created without a token — NIM-1051); a new `operator.jwt-revoked` records `{aid, jwt_id, kind}`. Every record written by the `/v1` audit middleware and by an MCP tool carries **`actor_jwt_id`** — the token the action was taken with — so an action traces to one token and to where it was minted and last used. Records written outside those two writers (a handler or a background job building its own event) do not carry it yet.
- **Tokens minted before migration 127** have no `jti`: they verify until `exp` and can be stopped only by revoking their operator (or rotating the signing key). With default config the longest of them is a bootstrap token, 30 days; a cluster that configured longer `ttl_bootstrap` / `ttl_default` carries them for that long.

**Revoking the token you are using locks you out of it** (a finding of the independent review): the request that revokes it succeeds, every later one is refused (`401 jwt-revoked` over HTTP, `unauthenticated` on MCP). An LDAP/OIDC or `local` operator signs in again; but if that was the only token of the last `cluster-admin` without a federated or password login, and nobody else holds `operator.issue-token`, the cluster has no administrator and recovery is manual SQL — the same end state as a lost bootstrap token (Consequences above), reached in one call. No guard refuses it yet; whether to refuse revoking the caller's current token conflicts with signing out of a session (NIM-1050) and is left as a decision.

What this does not change: a long-lived token is still a bearer secret, and stopping a leaked one needs someone to notice — `last_used_ip` / `last_used_user_agent` are the signal. Rotation stays manual, once per lifetime instead of once a day. The recorded IP is the direct peer: behind an L7 proxy it is the proxy's (trusted proxies are NIM-1050). And `operator.issue-token` is still unrestricted by target, so whoever holds it can now mint a 90-day token for any operator, `cluster-admin` included — the token is listed and revocable, which it was not before.

**Amendment 2026-10-24 (NIM-1052): an operator issues, lists and revokes its own tokens.**

A person needs a long-lived token for a script or a test without asking an administrator who holds `operator.issue-token` — and an administrator needs to see an operator's tokens in one place. Decided with the user (epic NIM-1049); the options weighed and refused are recorded with each bullet.

- **A fourth kind, `personal`** (migration 129 rewrites the CHECK `operator_jwts_kind_valid` and `purge_operator_jwts`; down turns `personal` rows into `issued` instead of deleting them, so a revoked one stays revoked, and restores the purge function). `created_by_aid` is the owner. Minted only through `MintJWT`, like every kind.
- **`/v1/me/jwts`, auth-only.** `POST` issues a personal token to the caller, `GET` lists the caller's tokens of every kind, `POST /{jwt_id}/revoke` revokes one of them of any kind. The owner is the claims' subject and never a request field, so the routes reach one operator's rows; the revoke matches the row on `jwt_id` **and** that AID, so another operator's id answers 404. No permission is asked (refused: a new `operator.issue-own-token` — every human role would need it, and it guards nothing the bounds below do not). REST only, like `/v1/me/permissions`; no MCP tools.
- **A personal token has no rights of its own.** Authorization reads the owner's roles from the RBAC snapshot at each request, as for every token (the `roles` claim stays informational, (a) above): the token can do nothing its owner cannot do at that moment, and a role taken from the owner is taken from the token. Scoped or read-only tokens are not in this version. What a personal token adds is a **lifetime**, and that is what is bounded:
  - **Ceiling** — `auth.jwt.ttl_max`, except for an **OIDC** operator, whose token lives no longer than an OIDC sign-in: OIDC groups are reconciled into roles only at sign-in ([ADR-058(d)](0058-operator-auth-ldap-oidc.md)), so a person removed from a group at the IdP keeps the roles of the last sign-in for as long as the token lives. Two halves: a `ttl` above the sign-in lifetime is refused (`422`; the lifetime is the one OIDC sign-ins are minted with, wired from the OIDC login's own TTL — today `auth.jwt.ttl_default`), and a token asked for **from** an OIDC sign-in is cut to expire with that sign-in, so one minted late in a sign-in does not outlive it. The cap goes with the OIDC group re-check (NIM-1054). LDAP gets `ttl_max` on the strength of the background group sync (NIM-1053, the same release); without that sync an LDAP operator is in the OIDC operator's position. Refused: refusing OIDC operators outright; giving them 90 days.
  - **Counts** — at most **10 active** (neither revoked nor expired) personal tokens per operator, and at most **20 issued in 24 hours**, revoked ones included; either answers `409 personal-jwt-limit`. The first bounds the credentials a person can have exposed at once; a revoke frees its slot. The second bounds issuance: issuing and revoking take no permission, so without it any operator could loop issue-then-revoke, growing `operator_jwts`, growing every node's snapshot of revoked ids until those tokens expire, and reloading every node's snapshot on each revoke. The counts and the insert run under a row lock on the operator, so concurrent requests cannot both take the last slot. The Reaper rule `purge_operator_jwts` keeps a personal row at least 24 hours after it was created (migration 129 extends the function), so a short `max_age` cannot purge one-minute tokens out of the daily count. When both limits are reached the daily one is reported, since a revoke does not lift it.
  - **Origin** — a request made with a personal token is refused, `403 personal-jwt-from-personal`; a sign-in, an `issued` or a `bootstrap` token may ask, and so may a token from before migration 127 (no `jti`, so not personal). A `jti` with no row of the caller is refused rather than read as "not personal". This stops a personal token from minting its own successor; the price is that a script cannot rotate itself — a person does, from a sign-in. It does **not** stop an owner who holds `operator.issue-token` (every `cluster-admin`): that route mints an `issued` token for anyone, the owner included, from any of its tokens and up to `ttl_max`, OIDC or not — the power that permission always carried (the NIM-1045 paragraph above).
- **Listing filters.** `GET /v1/operators/{aid}/jwts` and `GET /v1/me/jwts` take `kind` (repeatable), `state` (`active` / `revoked` / `expired`) and `q` (label substring), so a list of hundreds stays usable; `GET /v1/me/jwts` also answers what `POST /v1/me/jwts` would accept now (`personal_ttl_max_seconds`, `personal_expires_by` for an OIDC sign-in, `personal_active` / `personal_limit`, `personal_last_day` / `personal_daily_limit`). The MCP `jwt-list` keeps its unfiltered input.
- **Audit.** `operator.token-issued` now carries `kind` — `issued` from `issue-token` (HTTP and MCP), `personal` from `/v1/me/jwts`; a revoke through `/v1/me` writes the same `operator.jwt-revoked` as the administrator's.
- **Revoking your own current token** through `/v1/me` behaves as `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke` does (the paragraph above): it succeeds and signs the caller out of it. Whether to refuse it stays the open decision NIM-1055; a sign-out of the current session, which NIM-1050 plans, would be that very call.

**Amendment 2026-10-24 (NIM-1051): `auth_method` gains `local` — a password Keeper checks itself.**

- **Only-add** (SQL CHECK `auth_method_valid`, migration 130): `local` is an operator that logs in with a password at `POST /auth/local/login` and gets the same `session` token an LDAP/OIDC login gets. Only a `local` operator logs in by password — the anti-takeover rule of ADR-058(d) extended to it — and a `jwt` operator is never given one.
- **The credential of a `local` operator is its password, not a token in the create reply.** `POST /v1/operators` with `auth_method: local` takes `password` and returns no `jwt`/`jwt_id`/`expires_at`; the reply gains `auth_method`. `issue-token` still works on a local operator, as on any other — a script run by that person is not a reason to give it the password.
- The hash, its table, the reset and change routes, the policy and the bootstrap flag: [ADR-0092](0092-operator-local-passwords.md).

**Amendment 2026-06-23: the `operators.created_via` field + moving the bootstrap invariant + extending the `auth_method` enum.**

Under the operator provisioning model ([ADR-058](0058-operator-auth-ldap-oidc.md#adr-058-federated-operator-authentication-archon---ldap--oauth2oidc)):

- **The `auth_method` enum is extended (only-add)** with the values `ldap` / `oidc` (SQL CHECK `auth_method_valid`, migration 083) — an operator created via a federated method records with it its login method; the internal JWT after issuance is identical (shapes (a)/(b) above are unchanged). This is an additive extension in the spirit of the `mtls`/`combined` already declared in (a). **Security invariant (CRIT-fix 2026-06-24, ADR-058(d)):** federated login serves ONLY operators with a matching `auth_method` — bootstrap/system (`jwt`; the bootstrap Archon may be `local` since NIM-1051), mTLS, `local` and an operator of another federated method are NOT assigned to a matched derived AID (anti account-takeover).

The `operators` registry gains a new field:

- **`created_via`** — TEXT NOT NULL DEFAULT `'user'`, domain `{bootstrap, user, ldap, oidc, system}` (CHECK `created_via_valid`). Semantics — **"where the operator was created from"**, orthogonal to `auth_method` (**"what the operator logs in with"**). Examples: bootstrap Archon (`keeper init`) → `created_via='bootstrap'`, `auth_method='jwt'` (`'local'` with `--password-stdin`, NIM-1051); an operator via `POST /v1/operators` → `'user'`; LDAP auto-provision → `'ldap'`; the system `archon-system` → `'system'`. DEFAULT `'user'` — a safe fallback for rows created via the Operator API. Introduced by migration **084** (ADD COLUMN + CHECK + reconcile of existing rows: `created_by_aid IS NULL → 'bootstrap'`, `aid='archon-system' → 'system'`).

- **The bootstrap invariant is moved to `created_via`.** The former formulation ((c) / Trade-offs above) — "exactly one record with `created_by_aid IS NULL`", guaranteed by the partial unique index `operators_first_archon_idx` `WHERE created_by_aid IS NULL`. Now the invariant reads as **"exactly one operator with `created_via='bootstrap'`"**, the index is moved to `WHERE created_via='bootstrap'` (migration **085**). Consequence: federated operators (`created_via='ldap'/'oidc'`) and system ones (`created_via='system'`) set **`created_by_aid=NULL` legally** — previously this broke the bootstrap index on the second such operator. The CHECK `self_reference_ok` (`created_by_aid != aid`) **remains** unchanged. Wherever above in this ADR it says "`created_by_aid IS NULL` only for the first Archon" — it now reads as "`created_via='bootstrap'` only for the first Archon".
