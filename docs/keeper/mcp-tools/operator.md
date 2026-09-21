# Operator — MCP-tools for managing Archons

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.operator.*` (creation / review / release of JWT Archons). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api.md → Operator](../operator-api/operator.md).

### Operator (3)

#### `keeper.operator.create`

Creation of a new Archon. Permission: `operator.create`. Endpoint: [`POST /v1/operators`](../operator-api/operator.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the new Archon. |
| `display_name` | `string` | yes | Human-readable name. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID of the created Archon. |
| `display_name` | `string` | Mirror input. |
| `created_at` | `string` (RFC 3339) | Time of creation. |
| `created_by_aid` | `string` | AID of the Archon who performed the challenge. |
| `jwt` | `string` | Issued JWT, **issued once**. |

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

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `aid` | `string` | AID. |
| `jwt` | `string` | New JWT. |
| `expires_at` | `string` (RFC 3339) | Expiration date. Not to be confused with JWT-claim `exp` (unix-sec) inside the token itself. |
