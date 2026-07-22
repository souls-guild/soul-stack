# Synod - endpoints for managing archon groups

Domain section [Operator API](../operator-api.md): endpoints `/v1/synods*` (Synod groups - role bundling according to the model **Archon → Synod → Roles**, [ADR-049](../../adr/0049-synod.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/synods.md](../mcp-tools/synods.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 8 routes + remark about `PATCH /v1/synods/{name}` editing only `description`, NoSelector) - in the root [operator-api.md → Synod (8)](../operator-api.md).

**Source of truth on semantics, bodies and error codes** CRUD `/v1/synods*` (`synod-already-exists`, `synod-not-found`, `synod-builtin`, `would-lock-out-cluster`) - [rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods) (business invariants builtin boundaries, self-lockout, least-privilege subset live in `rbac.Service`). Effective rights of the initiator with subset-check = direct ∪ roles through his Synods.

Summary of 8 routes:

| Method/Path | Permission | MCP-tool | Semantics |
|---|---|---|---|
| `POST /v1/synods` | `synod.create` | [`keeper.synod.create`](../mcp-tools/synods.md#keepersynodcreate) | Create a group (empty - roles are added grant-role). |
| `GET /v1/synods` | `synod.list` | [`keeper.synod.list`](../mcp-tools/synods.md#keepersynodlist) | List of groups with expanded roles and AID members. |
| `PATCH /v1/synods/{name}` | `synod.update` | [`keeper.synod.update`](../mcp-tools/synods.md#keepersynodupdate) | Edit **`description` ONLY** (ADR-049 amend); `name` (PK) immutable; builtin is allowed. |
| `DELETE /v1/synods/{name}` | `synod.delete` | [`keeper.synod.delete`](../mcp-tools/synods.md#keepersynoddelete) | Delete a group (cascade membership + bundle). |
| `POST /v1/synods/{name}/operators` | `synod.add-operator` | [`keeper.synod.add-operator`](../mcp-tools/synods.md#keepersynodadd-operator) | Add an archon to the group. Idempotent. |
| `DELETE /v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | [`keeper.synod.remove-operator`](../mcp-tools/synods.md#keepersynodremove-operator) | Remove the archon from the group. |
| `POST /v1/synods/{name}/roles` | `synod.grant-role` | [`keeper.synod.grant-role`](../mcp-tools/synods.md#keepersynodgrant-role) | Add a role to the bundle (all members). Idempotent. |
| `DELETE /v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | [`keeper.synod.revoke-role`](../mcp-tools/synods.md#keepersynodrevoke-role) | Remove a role from the bundle (all members). |

`synod.*` - NoSelector. `PATCH /v1/synods/{name}` (ADR-049 amend) changes **ONLY `description`** (body `{description}`, required, 1..1024 characters); `name` (PK) immutable. Codes: `204` (success), `404 synod-not-found`, `422 validation-failed` (empty/exceeding `description`), `400 malformed-request` (broken JSON / unknown field, including `name` in the body). builtin group is editable (`description` cosmetics, without subset/self-lockout). Audit-event `synod.updated`.
