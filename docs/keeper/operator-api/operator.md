# Operator — endpoints for managing Archons

Domain section [Operator API](../operator-api.md): endpoints `/v1/operators*` (create / review / release JWT / read Archons, [ADR-013](../../adr/0013-bootstrap-archon.md) / [ADR-014](../../adr/0014-operator-identity.md)) and the caller's own tokens `/v1/me/jwts*` (NIM-1052). Conventions, error-format, pagination, secret-masking, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/operator.md](../mcp-tools/operator.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 9 routes + a note about the deferred MCP symmetry of read endpoints) - in the root [operator-api.md → Operator (9)](../operator-api.md).

#### `POST /v1/operators` - create an Archon

Permission: `operator.create`. MCP-tool: `keeper.operator.create`.

Creates a registry entry `operators` ([storage.md](../storage.md), [ADR-014](../../adr/0014-operator-identity.md)), binds to roles from `keeper.yml → rbac.roles[].operators`, releases the first JWT — for `ttl` if the request gives one, otherwise `auth.jwt.ttl_default` ([config.md → auth](../config.md#auth)) — and accounts for it as an `operator_jwts` row of kind `issued` ([§ Tokens](#tokens-of-an-archon)).

With `auth_method: local` the Archon gets a **password instead of a token** (NIM-1051, [ADR-0092](../../adr/0092-operator-local-passwords.md)): no JWT is minted, the reply carries none, and the Archon logs in at `POST /auth/local/login`. Needs `auth.local` on this Keeper ([config.md → auth.local](../config.md#authlocal---local-operator-passwords-adr-0092)) and `local` in the provisioning policy (a token Archon needs `user`). MCP `keeper.operator.create` creates token Archons only.

**Request `OperatorCreateRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the new Archon ([naming-rules.md](../../naming-rules.md)). |
| `display_name` | `string` | yes | Human readable name for UI/audit. |
| `roles` | `string[]` | optional | Roles granted in the same transaction (any error rolls the operator back). |
| `ttl` | `string` (`duration`: `2160h`, `90d`) | optional | Lifetime of the returned JWT. Omitted → `auth.jwt.ttl_default`; between `1m` and `auth.jwt.ttl_max`, else `422`. |
| `label` | `string` (≤ 200 characters, no control characters) | optional | Shown in the Archon's token list. Not with `auth_method: local`. |
| `auth_method` | `jwt` \| `local` | optional | How the Archon logs in. `jwt` (default) — the reply carries its token; `local` — `password` is required and the reply carries no token. |
| `password` | `string` | with `local` | The initial local password: at least `auth.local.min_password_length` (default 12) and at most 128 characters, no control characters, not equal to the AID. Never logged, never returned. |
| `owner_aid` | `string` (AID) | optional, `jwt` only | The person who owns the new machine operator and rotates its tokens ([§ The owner of a machine operator](#the-owner-of-a-machine-operator)). Exclusive with `owner_synod`. |
| `owner_synod` | `string` | optional, `jwt` only | The Synod whose members (those who sign in themselves) own the new operator. Exclusive with `owner_aid`. |

```json
{
  "aid": "archon-alice",
  "display_name": "Alice Smith"
}
```

**Response `201 OperatorCreateReply`:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID of the created Archon. |
| `auth_method` | `jwt` \| `local` | How it logs in; `local` → the three token fields below are absent. |
| `display_name` | `string` | Mirror from request. |
| `created_at` | `string` (RFC 3339) | Postgres record creation time. |
| `created_by_aid` | `string` | AID of the Archon who made the request (from JWT `sub`). FK on `operators(aid)`. |
| `jwt` | `string` | Issued JWT. **One time return** - reissue only via `POST /v1/operators/{aid}/issue-token`. |
| `jwt_id` | `string` | The token's `jti` — revoke it alone with `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke`. |
| `expires_at` | `string` (RFC 3339) | Expiration of the issued JWT. |

```json
{
  "aid": "archon-alice",
  "auth_method": "jwt",
  "display_name": "Alice Smith",
  "created_at": "2026-05-20T15:30:00Z",
  "created_by_aid": "archon-root",
  "expires_at": "2026-05-21T15:30:00Z",
  "jwt": "eyJhbGc...",
  "jwt_id": "01JB2X9Q3W5E7R8T0Y2U4I6O8P"
}
```

> The JWT in the response is masked in all observability channels (masked-keys rule, [§ Secret masking](../operator-api.md)) and **is given once** - save the token immediately upon receipt.

**Errors:** `403 forbidden` (also `provisioning-method-disabled` when the policy does not allow `user` / `local`), `409 operator-already-exists`, `422 validation-failed` (invalid AID, `ttl` outside `[1m, auth.jwt.ttl_max]`, invalid `label`; with `local`: no `auth.local`, a password outside the policy, `ttl`/`label` given; a `password` without `auth_method: local`).

#### `POST /v1/operators/{aid}/revoke` - recall Archon

Permission: `operator.revoke`. MCP-tool: `keeper.operator.revoke`. Path-param: `aid`.

Sets `revoked_at = now()` to `operators` — permanent. Every JWT of the Archon is refused with `401 operator-revoked-token` on its next request ([ADR-014 amendment NIM-421](../../adr/0014-operator-identity.md)). To stop one token and keep the Archon, revoke the token ([§ Tokens](#tokens-of-an-archon)).

**Request body `OperatorRevokeRequest`:**

| Field | Type | Obligation | Meaning |
|---|---|---|---|
| `reason` | `string` | optional | Free text reason for audit-trail (captured in `payload.reason` audit-event `operator.revoked`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |

**Response `204 No Content`** on success. **Errors:** `404 not-found`, `409 would-lock-out-cluster` (you cannot recall the last `*`-Archon, [§ Self-lockout invariant](../operator-api.md#self-lockout-invariant)).

#### `POST /v1/operators/{aid}/issue-token` - release new JWT

Permission: `operator.issue-token`. MCP-tool: `keeper.operator.issue-token`. Path-param: `aid`.

Writes a new JWT for an existing Archon (after token loss, scheduled rotation) and accounts for it as an `operator_jwts` row of kind `issued`. Older tokens stay valid until their `exp` unless revoked one by one ([§ Tokens](#tokens-of-an-archon)) — rotating a service token is issue the new one, switch the client over, revoke the old one.

**Request body `OperatorIssueTokenRequest`** — optional; an empty request keeps the old contract (a token for `auth.jwt.ttl_default`, no label):

| Field | Type | Obligation | Meaning |
|---|---|---|---|
| `ttl` | `string` (`duration`: `2160h`, `90d`) | optional | Lifetime. Omitted → `auth.jwt.ttl_default`; between `1m` and `auth.jwt.ttl_max` (default 90 days), else `422`. |
| `label` | `string` (≤ 200 characters, no control characters) | optional | Shown in the token list (`"Service Desk prod"`). |

```json
{ "ttl": "90d", "label": "Service Desk prod" }
```

**Response `200 IssueTokenReply`:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID. |
| `jwt` | `string` | New JWT. |
| `jwt_id` | `string` | The token's `jti`; the id to revoke it by. |
| `expires_at` | `string` (RFC 3339) | Expiration date. Not to be confused with the JWT-claim `exp` (unix-sec) inside the token itself - this is a decoded form for the convenience of the client. |

**Errors:** `403 forbidden`, `404 not-found`, `409 operator-revoked`, `422 validation-failed` (`ttl` / `label`).

#### `GET /v1/operators` - list of Archons

Permission: `operator.list`. Read-only, no audit.

**Query parameters:**

| Field | Type | Obligation | Meaning |
|---|---|---|---|
| `auth_method` | `string` (jwt/mtls/combined/ldap/oidc/local) | optional | Filter by how the Archon logs in. |
| `revoked` | `bool` (default `false`) | optional | `false` - active only (`revoked_at IS NULL`); `true` - including the reviled. |
| `q` | `string` | optional | Case-insensitive substring of `display_name` or `aid`. |
| `owner_aid` / `owner_synod` | `string` | optional | Only the operators this person / this Synod owns (NIM-1057). |
| `offset` / `limit` | `int` | optional | Standard pagination ([§ Pagination](../operator-api.md#pagination)). |

**Response `200 OperatorListReply`:** `{items: Operator[], offset, limit, total}`. Sort by `created_at DESC` (newest on top).

#### `GET /v1/operators/{aid}` — Archon detail

Permission: `operator.list` (`soul.list` pattern: one permission covers list+get). Path-param: `aid`. Read-only, no audit.

**Response `200 Operator`:** registry entry `operators`; a machine operator with an owner also carries `owner_aid` or `owner_synod`. **Errors:** `404 not-found` if the AID does not exist, `422 validation-failed` for a broken AID.

### Local passwords

An Archon with `auth_method: local` logs in with its AID and password at `POST /auth/local/login` and gets the same session an LDAP/OIDC login gets (NIM-1051, [ADR-0092](../../adr/0092-operator-local-passwords.md)). The hash (argon2id) lives in `operator_passwords` and is never returned by any route. Both routes below need `auth.local` on this Keeper; without it they answer `422`.

#### `PUT /v1/operators/{aid}/password` - set an Archon's password

Permission: `operator.set-password`. No MCP tool. Audit `operator.password-set` (`{aid, tokens_revoked}`).

Replaces the password of a `local` Archon and revokes **all** of its web sessions and personal tokens (`operator_jwts` rows of kind `session` and `personal`), a login still in flight with the old password included — whoever held the old password could have issued itself a personal token. `issued` and `bootstrap` tokens, handed out by an administrator or `keeper init`, are untouched. The caller's own AID is refused: this route asks for no current password, so it is changed with `PUT /v1/me/password`, which does. A personal token cannot be minted from a session the reset revoked, on this node or another (`401 jwt-revoked` on `POST /v1/me/jwts`). **Not restricted by target**, like `operator.issue-token`: whoever holds the permission can log in as any local Archon.

**Request `OperatorPasswordRequest`:** `{ "password": "…" }` (the policy of `POST /v1/operators`).

**Response `204 No Content`.** **Errors:** `403 forbidden`, `404 not-found`, `409 operator-revoked`, `422 validation-failed` (the caller's own AID, not a `local` Archon, policy not met, no `auth.local`).

#### `PUT /v1/me/password` - change my password

No permission: the subject is the caller. No MCP tool. Audit `operator.password-changed` (`{aid, tokens_revoked}`).

The calling `local` Archon replaces its own password, proving the current one. Its other web sessions are revoked; the one this request came with stays, and so do its personal tokens (its scripts) — revoke one in the token list if it may be someone else's. A reset by an administrator committed since the current password was checked refuses the change (`403`) rather than being overwritten.

**Request `MyPasswordChangeRequest`:** `{ "current_password": "…", "new_password": "…" }`.

**Response `204 No Content`.** **Errors:** `403 forbidden` — the current password is wrong (not `401`: the caller's token is fine); it counts towards the per-AID login lockout of `auth.rate_limit`, `429 auth-throttled` once locked; `401 operator-revoked-token` — revoked before the current password was checked; `413` — body over 4 KiB; `422 validation-failed` (not a `local` Archon, policy not met, no `auth.local`).

### Tokens of an Archon

Every JWT Keeper mints is a row in `operator_jwts` ([storage.md](../storage.md), [ADR-014 amendment NIM-1045](../../adr/0014-operator-identity.md)) whose `jwt_id` is the token's `jti`: the bootstrap token of `keeper init`, every token of `POST /v1/operators` / `issue-token`, the session cookie of every LDAP/OIDC/local-password login (the short Bearer `POST /auth/token` exchanges it for carries the session's `jti`), and every personal token an Archon issues to itself ([§ Your own tokens](#your-own-tokens), NIM-1052). A token minted before migration 127 — or outside Keeper, like the dev stand's `make dev-jwt` — has no `jti`, is not listed, and stops only with its Archon.

#### `GET /v1/operators/{aid}/jwts` - tokens of an Archon

Permission: `operator.list`. MCP-tool: `keeper.operator.jwt-list` (without the filters). Read-only, no audit.

**Query parameters:**

| Field | Type | Meaning |
|---|---|---|
| `kind` | repeatable, `bootstrap` / `issued` / `session` / `personal` | Any of the kinds (`?kind=personal&kind=issued`); a value outside the set → 422. |
| `state` | `active` / `revoked` / `expired` | `active` — neither revoked nor expired; `revoked` — revoked, expired since or not; `expired` — expired without being revoked. Omitted — every state. |
| `q` | `string`, ≤ 200 | Case-insensitive substring of the label. |
| `offset` / `limit` | `int` | [§ Pagination](../operator-api.md#pagination). `total` counts the rows under the filters. |

**Response `200 OperatorJWTListReply`:** `{items: OperatorJWT[], offset, limit, total}`, newest first. Expired tokens stay listed until the Reaper rule `purge_operator_jwts` removes them ([reaper.md](../reaper.md)). The token itself is never returned.

| Field | Type | Meaning |
|---|---|---|
| `jwt_id` | `string` | The token's `jti`. |
| `aid` | `string` | The Archon. |
| `kind` | `bootstrap` \| `issued` \| `session` \| `personal` | Where the token came from. |
| `label` | `string`, optional | Given at issue. |
| `created_at` / `expires_at` | `string` (RFC 3339) | Lifetime. |
| `created_by_aid` | `string`, optional | Who minted it; absent for `bootstrap` and `session`, the Archon itself for `personal`. |
| `created_ip` / `created_user_agent` | `string`, optional | What the minting request said about itself. |
| `last_used_at` / `last_used_ip` / `last_used_user_agent` | optional | The last use, recorded at most once per 5 minutes per Keeper instance. |
| `revoked_at` / `revoked_by_aid` | optional | Set once the token is revoked. |

The IP is the direct peer of Keeper: behind an L7 proxy it is the proxy's (trusted proxies — NIM-1050).

**Errors:** `400` (pagination), `403 forbidden`, `404 not-found` (unknown AID), `422 validation-failed`.

#### `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke` - revoke one token

Permission: `operator.jwt-revoke`. MCP-tool: `keeper.operator.jwt-revoke`. No body. Audit `operator.jwt-revoked` (`{aid, jwt_id, kind}`).

Sets `revoked_at`. The token is refused on its next request on every Keeper node — `401 jwt-revoked` on the `/v1` API and the cookie exchange, `unauthenticated` ("token revoked") on MCP `/mcp` and `/mcp/events` — and an open event stream or console opened with it is closed on its next re-check; the Archon and its other tokens keep working. Revoking a `session` row also refuses every Bearer exchanged from it.

**Response `204 No Content`.** **Errors:** `403 forbidden`, `404 not-found` (no such token **of this Archon** — a token of another Archon answers the same), `409 jwt-already-revoked`, `422 validation-failed`.

### Your own tokens

An Archon lists, issues and revokes its own tokens under `/v1/me/jwts` ([ADR-014 amendment NIM-1052](../../adr/0014-operator-identity.md)). The three routes are **auth-only**, like `GET /v1/me/permissions`: the Archon is the token's subject, never a path or body field, so they reach that Archon's rows only and ask no permission. **REST only** — there are no MCP tools for them.

#### `POST /v1/me/jwts` - issue a personal token to yourself

Body optional: `{ttl?, label?}` with the same rules as [`issue-token`](#post-v1operatorsaidissue-token---release-new-jwt). Audit `operator.token-issued` with `kind: personal`.

The token is of kind `personal`. Its rights are its owner's **at each request**, read from the RBAC data like every token's: it can do nothing its owner cannot do at that moment, and a role taken from the owner is taken from the token. What it adds is a lifetime, and that is bounded three ways:

- **Ceiling.** `auth.jwt.ttl_max` (90 days by default) for every Archon but an **OIDC** one. For an OIDC Archon — the lifetime of an OIDC sign-in (`auth.oidc.session_ttl`, 24h by default), and a token asked for from a sign-in is cut to expire with that sign-in: OIDC groups are reconciled into roles only at sign-in, so a person removed from a group at the IdP keeps the roles of the last sign-in for as long as the token lives. That lifts with the OIDC group re-check (NIM-1054). LDAP gets `ttl_max` on the strength of the background group sync (NIM-1053).
- **Counts.** At most **10** active personal tokens (neither revoked nor expired) and at most **20** issued in 24 hours, revoked ones included, per Archon → `409 personal-jwt-limit`. A revoke frees an active slot, not a daily one.
- **Credential.** A request made **with a personal token** is refused → `403 personal-jwt-from-personal`, so a personal token cannot mint its own successor. A sign-in, an `issued` or a `bootstrap` token may ask; so may a token minted before migration 127. An Archon holding `operator.issue-token` can still mint an `issued` token for itself through that route, from any of its tokens. The revocation of the token asked with is read from its row, not from the RBAC snapshot: a session a password reset ended a moment ago mints nothing on a node that has not heard of the reset yet → `401 jwt-revoked` (NIM-1051).

**Response `201 IssueTokenReply`:** `{aid, jwt, jwt_id, expires_at}` — the token is returned once. **Errors:** `400` (malformed body), `401 jwt-revoked` (the token asked with is revoked), `403 personal-jwt-from-personal`, `409 personal-jwt-limit` (either count), `422 validation-failed` (`ttl` above the caller's ceiling or below `1m`, an OIDC sign-in ending too soon for a `1m` token, `label`).

#### `GET /v1/me/jwts` - your own tokens

Every token of the caller, **of every kind** (a client shows its sign-ins apart by asking `kind=session`), with the query parameters of [`GET /v1/operators/{aid}/jwts`](#get-v1operatorsaidjwts---tokens-of-an-archon). Read-only, no audit.

**Response `200 MyJWTListReply`:** `{items: OperatorJWT[], offset, limit, total, personal_ttl_max_seconds, personal_expires_by?, personal_active, personal_limit, personal_last_day, personal_daily_limit}` — the `personal_*` fields say what `POST /v1/me/jwts` would accept from the caller right now, so a client offers only that. `personal_expires_by` is set when the request is made with an OIDC sign-in: a token issued now expires then at the latest.

#### `POST /v1/me/jwts/{jwt_id}/revoke` - revoke one of your own tokens

Any kind, including a token an administrator issued to you. No body. Audit `operator.jwt-revoked`. Same effect as the [administrator's revoke](#post-v1operatorsaidjwtsjwt_idrevoke---revoke-one-token).

Revoking **the token the request is made with** succeeds and signs the caller out of it — every later request with it is `401 jwt-revoked`. That is how the web UI logs out of its session (NIM-1055 decided to allow it; [ADR-014](../../adr/0014-operator-identity.md) NIM-1045 amendment, "Revoking the token you are using").

**Response `204 No Content`.** **Errors:** `404 not-found` (not a token of the caller — another Archon's id answers the same), `409 jwt-already-revoked`, `422 validation-failed`.

### Your own web sessions

A web session is the token of kind `session` a login (password, LDAP, OIDC) mints; every Bearer the web UI exchanges for it carries its `jti`, so ending the session ends them all. It lasts its login method's `session_ttl` — `auth.local.session_ttl` and `auth.ldap.session_ttl` 7 days, `auth.oidc.session_ttl` 24h by default ([config.md → auth](../config.md#auth), [ADR-058(k)](../../adr/0058-operator-auth-ldap-oidc.md)). List them with `GET /v1/me/jwts?kind=session` and end one with `POST /v1/me/jwts/{jwt_id}/revoke` (above); each row says where it was signed in from and where it was last used (`created_ip`, `created_user_agent`, `last_used_*`, written at most every 5 minutes). The IP is the client's when Keeper's proxies are listed in `listen.trusted_proxies`, otherwise the nearest peer's.

#### `POST /v1/me/sessions/revoke` - end your web sessions

**Auth-only**, REST only. Body optional: `{keep_current?: bool}` — `true` ends every session but the one the request is made with ("end all others"); omitted or `false` ends every session, that one included ("end all"; the caller is signed out). Only tokens of kind `session` are revoked: personal and issued tokens keep working. Audit `operator.sessions-revoked` `{aid, revoked, keep_current}`.

**Response `200 MySessionsRevokeReply`:** `{revoked}` — how many sessions were ended (`0` is a success). **Errors:** `422 validation-failed` (malformed body).

### The owner of a machine operator

A machine operator is an Archon with `auth_method: jwt` created through `POST /v1/operators` — one that signs in only with a token issued to it, such as the account of an integration. It may have an **owner**: one person (`owner_aid`, an active Archon with `auth_method` `local`, `ldap` or `oidc`, never the operator itself) or one Synod (`owner_synod`, every current member that signs in itself is an owner — a machine operator in it owns nothing). The owner rotates the operator's tokens itself, without `operator.issue-token`, while its own rights cover the operator's ([ADR-014 amendment NIM-1057](../../adr/0014-operator-identity.md), [rbac.md → The operators you own](../rbac.md#operator-8--adr-014)). Bootstrap, `archon-system` and every Archon that signs in itself have no owner.

#### `PUT /v1/operators/{aid}/owner` - set the owner

Permission: `operator.set-owner` (not restricted by target). MCP-tool: `keeper.operator.set-owner`. Audit `operator.owner-set` (`{aid, owner_aid | owner_synod, previous_owner_aid | previous_owner_synod}`).

**Request `OperatorOwnerRequest`:** exactly one of `owner_aid` / `owner_synod`. Replaces the previous owner.

```json
{"owner_synod": "dba"}
```

**Response `204 No Content`.** **Errors:** `403 forbidden`, `404 not-found` (unknown AID), `409 operator-revoked`, `422 validation-failed` — not a machine operator, both or neither field, the operator itself, an unknown or revoked Archon, an Archon that does not sign in itself, an unknown Synod.

#### `DELETE /v1/operators/{aid}/owner` - clear the owner

Permission: `operator.set-owner`. MCP: `keeper.operator.set-owner` with neither field. Audit `operator.owner-set` without the owner fields. The operator's tokens stay. Idempotent, and works on a revoked operator too. **Response `204 No Content`.** **Errors:** `403 forbidden`, `404 not-found`, `422 validation-failed` (not a machine operator).

A Synod that owns operators cannot be deleted: `DELETE /v1/synods/{name}` answers `409 synod-owns-operators` naming them, revoked ones included. Give them another owner or clear it first — `DELETE` clears the owner of a revoked operator too, `PUT` refuses one (`409`).

#### `GET /v1/me/operators` - the operators you own

**Auth-only**, REST only. The active machine operators the caller owns, directly or through a Synod it is a member of — as a person; a machine operator owns nothing through a Synod — by AID. Query: `offset` / `limit`. **Response `200 OperatorListReply`** (the shape of `GET /v1/operators`).

#### `GET /v1/me/operators/{aid}/jwts` - tokens of an operator you own

**Auth-only**. `GET /v1/operators/{aid}/jwts` without `operator.list`, the same filters and reply. **Errors:** `404 not-found` — not an operator the caller owns (an unknown AID answers the same).

#### `POST /v1/me/operators/{aid}/jwts` - issue a token of an operator you own

**Auth-only**. Optional body `{ttl?, label?}` as for `issue-token`: `ttl` up to `auth.jwt.ttl_max`, omitted → `auth.jwt.ttl_default`. Mints a token of kind `issued` with `created_by_aid` the caller. Audit `operator.token-issued` (`{aid, expires_at, jwt_id, kind, ttl, label?}`, `archon_aid` the owner).

Checked in the transaction that mints, in this order: the caller owns the operator; the operator is not revoked; the request is not made with a personal token or with a token already revoked in the database; **the caller's effective permissions (direct and through Synods, each role resolved through its parent chain and under its effective `default_scope`) cover every effective permission of the operator** — the containment of the least-privilege invariant, so `incarnation.get on service=dragonfly` does not cover `incarnation.get`. The token is authorized against the operator's roles at each request, like every token: a role granted to the operator later reaches the tokens its owner minted before.

```json
{"ttl": "90d", "label": "servicedesk prod"}
```

**Response `201 IssueTokenReply`:** `{aid, jwt, jwt_id, expires_at}` — the JWT once. **Errors:** `401 jwt-revoked` (the caller's token is revoked in the database), `403 owner-lacks-permissions` (detail names the first right not covered), `403 personal-jwt-from-personal`, `404 not-found`, `409 operator-revoked`, `422 validation-failed` (`ttl` / `label`).

#### `POST /v1/me/operators/{aid}/jwts/{jwt_id}/revoke` - revoke a token of an operator you own

**Auth-only**. `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke` without `operator.jwt-revoke`; no coverage check — revoking only takes access away. Audit `operator.jwt-revoked`. **Response `204 No Content`.** **Errors:** `404 not-found` (not an operator the caller owns, or not a token of it), `409 jwt-already-revoked`.
