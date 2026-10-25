# Operator — MCP-tools for managing Archons

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.operator.*` (creation / review / release of JWT Archons). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api.md → Operator](../operator-api/operator.md).

### Operator (5)

#### `keeper.operator.create`

Creation of a new Archon. Permission: `operator.create`. Endpoint: [`POST /v1/operators`](../operator-api/operator.md). Async: no. Creates a token Archon (`auth_method: jwt`) only: an Archon that logs in with a password (`auth_method: local`, NIM-1051) is created over HTTP, so that a password never travels through an agent's channel; for the same reason there is no MCP tool for setting or changing a password.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the new Archon. |
| `display_name` | `string` | yes | Human-readable name. |
| `ttl` | `string` (`duration`: `2160h`, `90d`) | no | Lifetime of the returned JWT. Omitted → `auth.jwt.ttl_default`; between `1m` and `auth.jwt.ttl_max`, else `validation-failed`. |
| `label` | `string` (≤ 200 characters) | no | Shown in the Archon's token list. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID of the created Archon. |
| `display_name` | `string` | Mirror input. |
| `created_at` | `string` (RFC 3339) | Time of creation. |
| `created_by_aid` | `string` | AID of the Archon who performed the challenge. |
| `jwt` | `string` | Issued JWT, **issued once**. |
| `jwt_id` | `string` | The token's `jti`; revoke it alone with `keeper.operator.jwt-revoke`. |
| `expires_at` | `string` (RFC 3339) | Expiration of the issued JWT. |

#### `keeper.operator.revoke`

Feedback from Archon. Permission: `operator.revoke`. Endpoint: [`POST /v1/operators/{aid}/revoke`](../operator-api/operator.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` | yes | AID of the Archon being recalled. |
| `reason` | `string` | no | Free text reason for audit-trail (captured in `payload.reason` audit-event `operator.revoked`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

#### `keeper.operator.issue-token`

Release of new JWT for existing Archon. Permission: `operator.issue-token`. Endpoint: [`POST /v1/operators/{aid}/issue-token`](../operator-api/operator.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` | yes | AID. |
| `ttl` | `string` (`duration`: `2160h`, `90d`) | no | Lifetime. Omitted → `auth.jwt.ttl_default`; between `1m` and `auth.jwt.ttl_max` (default 90 days). |
| `label` | `string` (≤ 200 characters) | no | Shown in the token list. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID. |
| `jwt` | `string` | New JWT. |
| `jwt_id` | `string` | The token's `jti`. |
| `expires_at` | `string` (RFC 3339) | Expiration date. Not to be confused with JWT-claim `exp` (unix-sec) inside the token itself. |

#### `keeper.operator.jwt-list`

Every JWT Keeper minted for an Archon, newest first — never the token itself ([ADR-014 amendment NIM-1045](../../adr/0014-operator-identity.md)). Permission: `operator.list`. Endpoint: [`GET /v1/operators/{aid}/jwts`](../operator-api/operator.md#tokens-of-an-archon). Async: no.

**Input:** `aid` (required), `offset` (default 0), `limit` (1..1000, default 50). The `kind` / `state` / `q` filters of the HTTP route are not offered here (NIM-1052 added them to HTTP only).

**Output:** `{items, offset, limit, total}`; an item carries `jwt_id`, `aid`, `kind` (`bootstrap` / `issued` / `session` / `personal`), `label`, `created_at`, `expires_at`, `created_by_aid`, `created_ip`, `created_user_agent`, `last_used_at`, `last_used_ip`, `last_used_user_agent`, `revoked_at`, `revoked_by_aid` — the same fields as the HTTP reply. **Errors:** `not-found` (unknown AID), `forbidden`.

#### `keeper.operator.jwt-revoke`

Revokes one JWT of an Archon: the token is refused on its next request on every door, the Archon and its other tokens keep working. Permission: `operator.jwt-revoke`. Endpoint: [`POST /v1/operators/{aid}/jwts/{jwt_id}/revoke`](../operator-api/operator.md#tokens-of-an-archon). Async: no. Audit `operator.jwt-revoked`.

**Input:** `aid`, `jwt_id` (both required). **Output:** empty object. **Errors:** `not-found` (not a token of this Archon), `jwt-already-revoked`, `forbidden`.
