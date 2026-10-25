# Operator — endpoints for managing Archons

Domain section [Operator API](../operator-api.md): endpoints `/v1/operators*` (create / review / release JWT / read Archons, [ADR-013](../../adr/0013-bootstrap-archon.md) / [ADR-014](../../adr/0014-operator-identity.md)). Conventions, error-format, pagination, secret-masking, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/operator.md](../mcp-tools/operator.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 7 routes + a note about the deferred MCP symmetry of read endpoints) - in the root [operator-api.md → Operator (7)](../operator-api.md).

#### `POST /v1/operators` - create an Archon

Permission: `operator.create`. MCP-tool: `keeper.operator.create`.

Creates a registry entry `operators` ([storage.md](../storage.md), [ADR-014](../../adr/0014-operator-identity.md)), binds to roles from `keeper.yml → rbac.roles[].operators`, releases the first JWT — for `ttl` if the request gives one, otherwise `auth.jwt.ttl_default` ([config.md → auth](../config.md#auth)) — and accounts for it as an `operator_jwts` row of kind `issued` ([§ Tokens](#tokens-of-an-archon)).

**Request `OperatorCreateRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the new Archon ([naming-rules.md](../../naming-rules.md)). |
| `display_name` | `string` | yes | Human readable name for UI/audit. |
| `roles` | `string[]` | optional | Roles granted in the same transaction (any error rolls the operator back). |
| `ttl` | `string` (`duration`: `2160h`, `90d`) | optional | Lifetime of the returned JWT. Omitted → `auth.jwt.ttl_default`; between `1m` and `auth.jwt.ttl_max`, else `422`. |
| `label` | `string` (≤ 200 characters, no control characters) | optional | Shown in the Archon's token list. |

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
| `display_name` | `string` | Mirror from request. |
| `created_at` | `string` (RFC 3339) | Postgres record creation time. |
| `created_by_aid` | `string` | AID of the Archon who made the request (from JWT `sub`). FK on `operators(aid)`. |
| `jwt` | `string` | Issued JWT. **One time return** - reissue only via `POST /v1/operators/{aid}/issue-token`. |
| `jwt_id` | `string` | The token's `jti` — revoke it alone with `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke`. |
| `expires_at` | `string` (RFC 3339) | Expiration of the issued JWT. |

```json
{
  "aid": "archon-alice",
  "display_name": "Alice Smith",
  "created_at": "2026-05-20T15:30:00Z",
  "created_by_aid": "archon-root",
  "expires_at": "2026-05-21T15:30:00Z",
  "jwt": "eyJhbGc...",
  "jwt_id": "01JB2X9Q3W5E7R8T0Y2U4I6O8P"
}
```

> The JWT in the response is masked in all observability channels (masked-keys rule, [§ Secret masking](../operator-api.md)) and **is given once** - save the token immediately upon receipt.

**Errors:** `403 forbidden`, `409 operator-already-exists`, `422 validation-failed` (invalid AID, `ttl` outside `[1m, auth.jwt.ttl_max]`, invalid `label`).

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
| `auth_method` | `string` (jwt/mtls/combined) | optional | Filter by credential form. |
| `revoked` | `bool` (default `false`) | optional | `false` - active only (`revoked_at IS NULL`); `true` - including the reviled. |
| `offset` / `limit` | `int` | optional | Standard pagination ([§ Pagination](../operator-api.md#pagination)). |

**Response `200 OperatorListReply`:** `{items: Operator[], offset, limit, total}`. Sort by `created_at DESC` (newest on top).

#### `GET /v1/operators/{aid}` — Archon detail

Permission: `operator.list` (`soul.list` pattern: one permission covers list+get). Path-param: `aid`. Read-only, no audit.

**Response `200 Operator`:** registry entry `operators`. **Errors:** `404 not-found` if the AID does not exist, `422 validation-failed` for a broken AID.

### Tokens of an Archon

Every JWT Keeper mints is a row in `operator_jwts` ([storage.md](../storage.md), [ADR-014 amendment NIM-1045](../../adr/0014-operator-identity.md)) whose `jwt_id` is the token's `jti`: the bootstrap token of `keeper init`, every token of `POST /v1/operators` / `issue-token`, and the session cookie of every LDAP/OIDC login (the short Bearer `POST /auth/token` exchanges it for carries the session's `jti`). A token minted before migration 127 — or outside Keeper, like the dev stand's `make dev-jwt` — has no `jti`, is not listed, and stops only with its Archon.

#### `GET /v1/operators/{aid}/jwts` - tokens of an Archon

Permission: `operator.list`. MCP-tool: `keeper.operator.jwt-list`. Read-only, no audit. Query: `offset` / `limit` ([§ Pagination](../operator-api.md#pagination)).

**Response `200 OperatorJWTListReply`:** `{items: OperatorJWT[], offset, limit, total}`, newest first. Expired tokens stay listed until the Reaper rule `purge_operator_jwts` removes them ([reaper.md](../reaper.md)). The token itself is never returned.

| Field | Type | Meaning |
|---|---|---|
| `jwt_id` | `string` | The token's `jti`. |
| `aid` | `string` | The Archon. |
| `kind` | `bootstrap` \| `issued` \| `session` | Where the token came from. |
| `label` | `string`, optional | Given at issue. |
| `created_at` / `expires_at` | `string` (RFC 3339) | Lifetime. |
| `created_by_aid` | `string`, optional | Who minted it; absent for `bootstrap` and `session`. |
| `created_ip` / `created_user_agent` | `string`, optional | What the minting request said about itself. |
| `last_used_at` / `last_used_ip` / `last_used_user_agent` | optional | The last use, recorded at most once per 5 minutes per Keeper instance. |
| `revoked_at` / `revoked_by_aid` | optional | Set once the token is revoked. |

The IP is the direct peer of Keeper: behind an L7 proxy it is the proxy's (trusted proxies — NIM-1050).

**Errors:** `400` (pagination), `403 forbidden`, `404 not-found` (unknown AID), `422 validation-failed`.

#### `POST /v1/operators/{aid}/jwts/{jwt_id}/revoke` - revoke one token

Permission: `operator.jwt-revoke`. MCP-tool: `keeper.operator.jwt-revoke`. No body. Audit `operator.jwt-revoked` (`{aid, jwt_id, kind}`).

Sets `revoked_at`. The token is refused on its next request on every Keeper node — `401 jwt-revoked` on the `/v1` API and the cookie exchange, `unauthenticated` ("token revoked") on MCP `/mcp` and `/mcp/events` — and an open event stream or console opened with it is closed on its next re-check; the Archon and its other tokens keep working. Revoking a `session` row also refuses every Bearer exchanged from it.

**Response `204 No Content`.** **Errors:** `403 forbidden`, `404 not-found` (no such token **of this Archon** — a token of another Archon answers the same), `409 jwt-already-revoked`, `422 validation-failed`.
