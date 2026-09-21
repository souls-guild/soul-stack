# Synod - MCP-tools for managing groups of archons

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.synod.*` (Synod groups, bundling roles). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api.md → Synod](../operator-api/synods.md) (bodies and invariants are [rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)).

### Synod (8)

Management of **Synod groups** (archon groups, bundling roles - model **Archon → Synod → Roles**) via MCP - [ADR-049](../../adr/0049-synod.md), storage in Postgres (`synods` / `synod_operators` / `synod_roles`, [rbac.md → Managing groups of archons](../rbac.md)). Permission ↔ tool — 1:1: `keeper.synod.<action>` ↔ `synod.<action>`. Business invariants (builtin-border, self-lockout, least-privilege subset) live in `rbac.Service`; tool - transport. Effective rights of the initiator with subset-check = direct ∪ roles through his Synods.

#### `keeper.synod.create`

Creating a Synod group (empty - roles are added via `keeper.synod.grant-role`). Permission: `synod.create`. Endpoint: `POST /v1/synods` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Group name (kebab-case). |
| `description` | `string` | optional | A human-readable description of the group. |

**Output:** empty object (`{}`). Corresponds to HTTP `201 Created`.

Errors: `synod-already-exists` (`name` busy), `validation-failed` (dead `name`). Least-privilege/self-lockout is not applicable to create - an empty group of rights does not issue.

#### `keeper.synod.delete`

Deleting a group (membership + bundle cascade). Permission: `synod.delete`. Endpoint: `DELETE /v1/synods/{name}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Group name. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `synod-not-found`, `synod-builtin` (builtin group cannot be deleted, check **before** self-lockout), `would-lock-out-cluster` (disappearance of the group will remove the last effective `*`).

#### `keeper.synod.list`

Enumeration of groups with deployed roles (bundle) and AID members. Permission: `synod.list`. Endpoint: `GET /v1/synods` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:** empty object (`{}`).

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `synods` | `array<object>` | Elements - `{name, description, builtin, roles[], operators[]}`; `roles` / `operators` - non-nil arrays (group without records → `[]`), sorted deterministically. |

#### `keeper.synod.update`

Edit **ONLY `description`** groups (ADR-049 amend). Permission: `synod.update`. Endpoint: `PATCH /v1/synods/{name}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no. **`name` (PK) immutable** - addresses a group from path, is not renamed. **builtin ALLOWED** for editing (`description` - cosmetics for UI/audit, not behavior). **Without subset-check and self-lockout** (`description` does not grant or take away rights); The enforcer's snapshot is not invalidated.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Group name (PK, does not change - addresses the string). |
| `description` | `string` (1..1024 characters) | yes | New description (completely replaces the old one). |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `synod-not-found` (no group), `validation-failed` (empty `description` or limit exceeded 1024). Audit: `synod.updated`.

#### `keeper.synod.add-operator`

Adding an Archon (AID) to the group. Idempotent. Permission: `synod.add-operator`. Endpoint: `POST /v1/synods/{name}/operators` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Group name. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the added Archon. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `synod-not-found`, `not-found` (AID does not exist), `forbidden` (**least-privilege subset**: the member receives the entire bundle of the group - the caller is required to hold all its effective rights). Self-lockout **no** - add only extends admin-set.

#### `keeper.synod.remove-operator`

Removing the Archon (AID) from the group. Permission: `synod.remove-operator`. Endpoint: `DELETE /v1/synods/{name}/operators/{aid}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Group name. |
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID of the removed Archon. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `not-found` (there are no `(synod, aid)` pairs), `would-lock-out-cluster` (**self-lockout**: removal will orphan the last admin whose `*` is held only through this group).

#### `keeper.synod.grant-role`

Adding a role to the group's bundle (issues it to all members). Idempotent. Permission: `synod.grant-role`. Endpoint: `POST /v1/synods/{name}/roles` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Group name. |
| `role` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | The name of the role to be added. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `synod-not-found`, `role-not-found` (no role), `forbidden` (**least-privilege subset**: the role is issued to all members - the caller is required to hold all its effective rights). Self-lockout **no**.

#### `keeper.synod.revoke-role`

Removing a role from a bundle group (for all members). Permission: `synod.revoke-role`. Endpoint: `DELETE /v1/synods/{name}/roles/{role_name}` ([rbac.md → REST `/v1/synods`](../rbac.md#rest-v1synods)). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `synod` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | Group name. |
| `role` | `string` (regex `^[a-z][a-z0-9-]*$`) | yes | The name of the role being filmed. |

**Output:** empty object (`{}`). Corresponds to HTTP `204 No Content`.

Errors: `not-found` (there are no `(synod, role)` bundle pairs), `would-lock-out-cluster` (**self-lockout**: the role being removed is the last `*`-giving role of the group, the member held `*` only through it).
