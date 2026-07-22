# Augur - MCP-tools for Omen / Rite registries

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.augur.omen.*` / `keeper.augur.rite.*` (registries of external systems and grants of the Augur broker, [ADR-025](../../adr/0025-augur.md), [augur.md](../augur.md)). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api/augur.md](../operator-api/augur.md).

### Augur (6)

Augur registries - Omen (external system) and Rite (grant) ([ADR-025](../../adr/0025-augur.md), [augur.md](../augur.md)). 4-segment tool-name `keeper.augur.<resource>.<action>` ↔ 2-segment permission `<resource>.<action>` (`omen.create` / `rite.list` / …, selector - NoSelector). Business logic (validation `name`/`source_type`/`auth_ref`, XOR subject, allow-shape by `source_type`, token fields only for vault-delegate) lives in `augur.Service`; tool - transport. Tools are only available when the registry is connected; when disabled, the call returns `internal-error` ("augur registry is not configured"). **Live-fetch from Soul (`AugurRequest`) is NOT controlled by these tools** - this is a machine gRPC request, not an operator operation ([rbac.md §Augur](../rbac.md)).

#### `keeper.augur.omen.create`

Creates Omen in `omens`: external system (`vault`/`prometheus`/`elk`) + `endpoint` + `auth_ref` (vault-ref on master-cred, **not** the secret itself - [augur.md §4.1](../augur.md)). Permission: `omen.create`. Endpoint: [`POST /v1/augur/omens`](../operator-api/augur.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` | yes | Omen's name (kebab-case `^[a-z0-9-]{1,63}$`). |
| `source_type` | `string` | yes | `vault` / `prometheus` / `elk`. |
| `endpoint` | `string` | yes | External system URL (not secret). |
| `auth_ref` | `string` | yes | vault-ref `vault:<mount>/<path>` on master-credential. |

**Output:** `OmenView` — `{name, source_type, endpoint, auth_ref, created_by_aid?, created_at}`.

Errors: `omen-already-exists` (`name` busy), `validation-failed` (broken `name`/`source_type`/`endpoint`/`auth_ref`). Audit: `omen.created` (payload `{name, source_type, endpoint, auth_ref, created_by_aid}` - secret values ​​are NOT included).

#### `keeper.augur.omen.list`

Enumeration of Omens (sort `created_at` DESC, `name` ASC). Permission: `omen.list`. Endpoint: [`GET /v1/augur/omens`](../operator-api/augur.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `offset` | `integer` | no | Pagination offset (≥ 0). |
| `limit` | `integer` | no | Page size (≥ 1). |

**Output:** `{omens: array<OmenView>, total}`.

#### `keeper.augur.omen.delete`

Removes Omen by name; cascade removes associated Rites (`ON DELETE CASCADE`). Permission: `omen.delete`. Endpoint: [`DELETE /v1/augur/omens/{name}`](../operator-api/augur.md). Async: no.

**Input:** `{name}`.

**Output:** empty object (REST equivalent - 204 No Content).

Errors: `not-found` (no entry). Audit: `omen.revoked` (payload `{name}`).

#### `keeper.augur.rite.create`

Creates Rite (grant): subject (`coven` **XOR** `sid`) × `omen` → `allow`-list + `delegate` + opt. `token_ttl`/`token_num_uses` (vault-delegate only). Permission: `rite.create`. Endpoint: [`POST /v1/augur/rites`](../operator-api/augur.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `omen` | `string` | yes | Omen, which grant refers to. |
| `coven` | `string` | no | Subject by Coven tag (XOR with `sid`). |
| `sid` | `string` | no | Subject by specific SID (XOR with `coven`). |
| `allow` | `object` | yes | Allow-list; form by `source_type` Omen (vault `{paths?,policies?}` / prometheus `{queries}` / elk `{indices}`). |
| `delegate` | `boolean` | no | `false` - broker; `true` - delegation. |
| `token_ttl` | `string` | no | TTL of minted scoped token; vault-delegate only. |
| `token_num_uses` | `integer` | no | Token usage limit; vault-delegate only. |

**Output:** `RiteView` — `{id, omen, coven?, sid?, allow, delegate, token_ttl?, token_num_uses?, created_by_aid?, created_at}`.

Errors: `not-found` (Omen does not exist), `validation-failed` (XOR violation / broken `allow` / token field). Audit: `rite.created` (payload `{id, omen, subject, delegate, created_by_aid}` - `allow`-list is NOT included).

#### `keeper.augur.rite.list`

Enumeration of Rites of one Omen (filter `omen` is required; sort `created_at` DESC, `id` ASC). Permission: `rite.list`. Endpoint: [`GET /v1/augur/rites`](../operator-api/augur.md). Async: no.

**Input:** `{omen}` (required).

**Output:** `{rites: array<RiteView>}`.

#### `keeper.augur.rite.delete`

Removes Rite by surrogate `id`. Permission: `rite.delete`. Endpoint: [`DELETE /v1/augur/rites/{id}`](../operator-api/augur.md). Async: no.

**Input:** `{id}` (positive integer).

**Output:** empty object (REST equivalent - 204 No Content).

Errors: `not-found` (no entry). Audit: `rite.revoked` (payload `{id}`).
