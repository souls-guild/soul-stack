# Role — endpoints RBAC-CRUD

Domain section [Operator API](../operator-api.md): endpoints `/v1/roles*` (roles / permissions / membership of Archons, [ADR-013](../../adr/0013-bootstrap-archon.md) / [ADR-028](../../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/roles.md](../mcp-tools/roles.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 6 routes + remark about replace semantics `PATCH .../permissions`, NoSelector, audit) - in the root [operator-api.md → Role (6)](../operator-api.md).

**Source of truth on semantics, bodies and error codes** REST `/v1/roles*` (`role-already-exists`, `role-builtin`, `would-lock-out-cluster`) - [rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles) (business invariants of builtin boundaries and self-lockout live in `rbac.Service`). This file is the Operator API domain anchor; body details are not duplicated.

Summary of 6 routes:

| Method/Path | Permission | MCP-tool | Semantics |
|---|---|---|---|
| `POST /v1/roles` | `role.create` | [`keeper.role.create`](../mcp-tools/roles.md#keeperrolecreate) | Create a role + its permissions. |
| `GET /v1/roles` | `role.list` | [`keeper.role.list`](../mcp-tools/roles.md#keeperrolelist) | List of roles with expanded permissions/operators. Read-only, no audit. |
| `DELETE /v1/roles/{name}` | `role.delete` | [`keeper.role.delete`](../mcp-tools/roles.md#keeperroledelete) | Delete a role (permissions + membership cascade). |
| `PATCH /v1/roles/{name}/permissions` | `role.update` | [`keeper.role.update`](../mcp-tools/roles.md#keeperroleupdate) | **Replace** set role permissions (not merge). |
| `POST /v1/roles/{name}/operators` | `role.grant-operator` | [`keeper.role.grant-operator`](../mcp-tools/roles.md#keeperrolegrant-operator) | Bind an Archon (AID) to a role. Idempotent. |
| `DELETE /v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | [`keeper.role.revoke-operator`](../mcp-tools/roles.md#keeperrolerevoke-operator) | Remove the membership line `(role, aid)`. |

`role.*` - NoSelector (cluster-level operation without coven/host-scope, like `operator.*` / `synod.*`). Mutating 5 routes are audited (authorization change, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); `role.list` - read-only, no audit.
