# Operator — endpoints for managing Archons

Domain section [Operator API](../operator-api.md): endpoints `/v1/operators*` (create / review / release JWT / read Archons, [ADR-013](../../adr/0013-bootstrap-archon.md) / [ADR-014](../../adr/0014-operator-identity.md)). Conventions, error-format, pagination, secret-masking, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/operator.md](../mcp-tools/operator.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 5 routes + a note about the deferred MCP symmetry of read endpoints) - in the root [operator-api.md → Operator (5)](../operator-api.md).

#### `POST /v1/operators` - create an Archon

Permission: `operator.create`. MCP-tool: `keeper.operator.create`.

Creates a registry entry `operators` ([storage.md](../storage.md), [ADR-014](../../adr/0014-operator-identity.md)), binds to roles from `keeper.yml → rbac.roles[].operators`, releases the first JWT with `auth.jwt.ttl_default` ([config.md → auth](../config.md#auth)).

**Request `OperatorCreateRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the new Archon ([naming-rules.md](../../naming-rules.md)). |
| `display_name` | `string` | yes | Human readable name for UI/audit. |

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
| `jwt` | `string` | Issued JWT, TTL = `auth.jwt.ttl_default`. **One time return** - reissue only via `POST /v1/operators/{aid}/issue-token`. |

```json
{
  "aid": "archon-alice",
  "display_name": "Alice Smith",
  "created_at": "2026-05-20T15:30:00Z",
  "created_by_aid": "archon-root",
  "jwt": "eyJhbGc..."
}
```

> The JWT in the response is masked in all observability channels (masked-keys rule, [§ Secret masking](../operator-api.md)) and **is given once** - save the token immediately upon receipt.

**Errors:** `403 forbidden`, `409 operator-already-exists`, `422 validation-failed` (invalid AID).

#### `POST /v1/operators/{aid}/revoke` - recall Archon

Permission: `operator.revoke`. MCP-tool: `keeper.operator.revoke`. Path-param: `aid`.

Sets `revoked_at = now()` to `operators`. Active JWTs continue to run until `exp` (no revocation-blocklist; short TTL - natural protection, [ADR-014](../../adr/0014-operator-identity.md)).

**Request body `OperatorRevokeRequest`:**

| Field | Type | Obligation | Meaning |
|---|---|---|---|
| `reason` | `string` | optional | Free text reason for audit-trail (captured in `payload.reason` audit-event `operator.revoked`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |

**Response `204 No Content`** on success. **Errors:** `404 not-found`, `409 would-lock-out-cluster` (you cannot recall the last `*`-Archon, [§ Self-lockout invariant](../operator-api.md#self-lockout-invariant)).

#### `POST /v1/operators/{aid}/issue-token` - release new JWT

Permission: `operator.issue-token`. MCP-tool: `keeper.operator.issue-token`. Path-param: `aid`.

Writes a new JWT for an existing Archon (after token loss, scheduled rotation). Old JWTs remain valid until `exp`.

**Request body:** empty (TTL is taken from `auth.jwt.ttl_default`).

**Response `200 IssueTokenReply`:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID. |
| `jwt` | `string` | New JWT. |
| `expires_at` | `string` (RFC 3339) | Expiration date. Not to be confused with the JWT-claim `exp` (unix-sec) inside the token itself - this is a decoded form for the convenience of the client. |

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
