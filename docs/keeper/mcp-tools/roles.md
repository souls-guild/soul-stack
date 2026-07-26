# Role — MCP-tools RBAC-CRUD

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.role.*` (roles / permissions / membership of Archons). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api.md → Role](../operator-api/roles.md) (bodies and invariants are [rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)).

### Role (6)

RBAC management (roles, permissions, membership) via MCP - [ADR-028](../../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), storage in Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`, [rbac.md → Storage](../rbac.md)). Permission ↔ tool — 1:1: `keeper.role.<action>` ↔ `role.<action>`. Business invariants (builtin-boundary, self-lockout) live in `rbac.Service`; tool — transport ([rbac.md → Role management](../rbac.md)).

#### `keeper.role.create`

Creating a role + its permissions. Permission: `role.create`. Endpoint: `POST /v1/roles` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Role name (kebab-case). |
| `description` | `string` | optional | Human-readable description of the role. |
| `permissions` | `array<string>` | yes | Permission lines `<resource>.<action>` (+ opt. ` on <selector>`), [rbac.md → Permissions format](../rbac.md). |
| `default_scope` | `string\|null` | optional | Role scope ([ADR-047](../../adr/0047-purview.md)), inherited by permissions without their own selector. With `parent_role` set it is the attenuating **delta** — write only the ADDED narrowing. |
| `parent_role` | `string\|null` | optional | Derive from this role ([ADR-078](../../adr/0078-rbac-derived-roles.md)): it becomes the ceiling and the new role can never exceed it. Omitted/`null` → a plain role. |

**Output:** empty object (`{}`). Corresponds to HTTP `201 Created`.

Errors: `role-already-exists` (`name` busy), `validation-failed` (broken `name` / `permission` / a cycle or over-deep chain), `not-found` (`parent_role` outside the catalog), `forbidden` (the role would exceed its parent, or the caller's own rights).

#### `keeper.role.delete`

Removing a role (permissions + membership cascade). Permission: `role.delete`. Endpoint: `DELETE /v1/roles/{name}` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Role name. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `role-not-found`, `role-builtin` (builtin role cannot be deleted), `would-lock-out-cluster` (deletion will remove the last effective `*`).

#### `keeper.role.list`

Listing roles with expanded permissions and assigned Archons. Permission: `role.list`. Endpoint: `GET /v1/roles` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: no.

**Input:** empty object (`{}`).

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `roles` | `array<object>` | Elements - `{name, description, builtin, permissions[], operators[], default_scope, parent_role, effective_permissions[], effective_scope}`; `permissions` / `operators` / `effective_permissions` - non-nil arrays (role without entries → `[]`). |

Each role comes in **both** forms ([ADR-078](../../adr/0078-rbac-derived-roles.md)): as stored (`permissions` / `default_scope` — on a derived role these are its own rows and its delta, not its rights) and as resolved against its derivation chain (`effective_permissions` / `effective_scope` — `own ∩ the parent's effective`, every scope capped by the chain's ceiling). On a plain role the two coincide. **Read the effective fields**: re-deriving inheritance from `parent_role` is a second implementation of the attenuation rules, free to disagree with the enforcer.

#### `keeper.role.update`

Replacing the set of role permissions (replace semantics). Permission: `role.update`. Endpoint: `PATCH /v1/roles/{name}/permissions` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Role name. |
| `permissions` | `array<string>` | yes | New set of permissions (completely replaces the existing one). |
| `default_scope` | `string\|null` | optional | **Key ABSENT** → scope untouched; present → replaces it (`null` clears it). |
| `parent_role` | `string\|null` | optional | Same presence rule: **key ABSENT** → derivation untouched; present → replaces it (`null` makes the role plain again), [ADR-078](../../adr/0078-rbac-derived-roles.md). |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `role-not-found`, `role-builtin`, `would-lock-out-cluster` (removing the last `*` — including by making that role derived), `validation-failed` (broken `permission`, a cycle or over-deep chain), `forbidden` (the result would exceed its parent role). The ceiling is re-checked on **every** update, not only the one that sets `parent_role`.

#### `keeper.role.grant-operator`

Binding the Archon (AID) to the role - adding the membership line `(role, aid)`. Idempotent. Permission: `role.grant-operator`. Endpoint: `POST /v1/roles/{name}/operators` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `role` | `string` | yes | Role name. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the bound Archon. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `not-found` (role or AID does not exist). Self-lockout checks **no**: grant only extends admin-set, the cluster cannot lock. Over builtin role is allowed.

#### `keeper.role.revoke-operator`

Removing the membership string `(role, aid)`. Permission: `role.revoke-operator`. Endpoint: `DELETE /v1/roles/{name}/operators/{aid}` ([rbac.md → REST `/v1/roles`](../rbac.md#rest-v1roles)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `role` | `string` | yes | Role name. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the removed Archon. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `not-found` (no pair `(role, aid)`), `would-lock-out-cluster` (removal of the last effective `*`). Over builtin role is allowed.
