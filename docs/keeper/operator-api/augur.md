# Augur - Omen / Rite registry endpoints

Domain section [Operator API](../operator-api.md): endpoints `/v1/augur/omens*` + `/v1/augur/rites*` (registries of external systems and grants of the Augur broker, [ADR-025](../../adr/0025-augur.md), [augur.md](../augur.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/augur.md](../mcp-tools/augur.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 7 routes) - in the root [operator-api.md → Augur (7)](../operator-api.md).

Augur registries - Omen (external system) and Rite (grant) ([ADR-025](../../adr/0025-augur.md), [augur.md](../augur.md)). Selector - NoSelector (CRUD operates on the registry itself). The master-credential of the external system is NOT stored in the registry - only `auth_ref` (vault-ref for it, [augur.md §4.1](../augur.md)).

### `POST /v1/augur/omens` - create Omen

Permission: `omen.create`. MCP-tool: `keeper.augur.omen.create`.

**Request `OmenCreateRequest`:** `{name, source_type, endpoint, auth_ref}` - `name` kebab `^[a-z0-9-]{1,63}$`; `source_type` ∈ `vault`/`prometheus`/`elk`; `endpoint` - URL (not secret); `auth_ref` - vault-ref `vault:<mount>/<path>`.

**Response `201` `OmenView`:** `{name, source_type, endpoint, auth_ref, created_by_aid?, created_at}`.

Errors: `400` (broken JSON), `409 omen-already-exists` (`name` busy), `422 validation-failed` (broken `name`/`source_type`/`endpoint`/`auth_ref`). Audit: `omen.created`.

### `GET /v1/augur/omens` - list of Omens

Permission: `omen.list`. MCP-tool: `keeper.augur.omen.list`. Query — `offset`/`limit` ([§ Pagination](../operator-api.md#pagination)). Response `200` — `PagedResponse<OmenView>` (`{items, offset, limit, total}`).

### `GET /v1/augur/omens/{name}` — read Omen

Permission: `omen.list`. MCP-tool: `keeper.augur.omen.list` (one permission covers list and get). Response `200` `OmenView`; `404 not-found` - no entry; `422 validation-failed` - broken `name`.

### `DELETE /v1/augur/omens/{name}` - remove Omen

Permission: `omen.delete`. MCP-tool: `keeper.augur.omen.delete`. Cascade deletes associated Rites (`ON DELETE CASCADE`). Response `204`; `404 not-found` - no entry. Audit: `omen.revoked`.

### `POST /v1/augur/rites` - create Rite

Permission: `rite.create`. MCP-tool: `keeper.augur.rite.create`.

**Request `RiteCreateRequest`:** `{omen, coven?, sid?, allow, delegate?, token_ttl?, token_num_uses?}` - subject `coven` **XOR** `sid`; `allow`-object, form by `source_type` Omen (vault `{paths?,policies?}` / prometheus `{queries}` / elk `{indices}`); `token_ttl`/`token_num_uses` - vault-delegate only.

**Response `201` `RiteView`:** `{id, omen, coven?, sid?, allow, delegate, token_ttl?, token_num_uses?, created_by_aid?, created_at}`.

Errors: `400` (broken JSON), `404 not-found` (Omen does not exist), `422 validation-failed` (XOR violation / broken `allow` / token fields). Audit: `rite.created`.

### `GET /v1/augur/rites` - list of Rite Omens

Permission: `rite.list`. MCP-tool: `keeper.augur.rite.list`. Query `omen` REQUIRED (filter by-omen, [augur.md §6](../augur.md)). Response `200` - `{items: [RiteView, …]}`; `422 validation-failed` - `omen` not transmitted / broken.

### `DELETE /v1/augur/rites/{id}` - remove Rite

Permission: `rite.delete`. MCP-tool: `keeper.augur.rite.delete`. Response `204`; `404 not-found` - no entry; `422 validation-failed` - `id` is not a positive integer. Audit: `rite.revoked`.
