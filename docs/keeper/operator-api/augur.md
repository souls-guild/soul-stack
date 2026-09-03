# Augur - Omen / Rite registry endpoints

Domain section [Operator API](../operator-api.md): endpoints `/v1/augur/omens*` + `/v1/augur/rites*` (registries of external systems and grants of the Augur broker, [ADR-025](../../adr/0025-augur.md), [augur.md](../augur.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/augur.md](../mcp-tools/augur.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 7 routes) - in the root [operator-api.md → Augur (7)](../operator-api.md).

Augur registries - Omen (external system) and Rite (grant) ([ADR-025](../../adr/0025-augur.md), [augur.md](../augur.md)). Selector - NoSelector (CRUD operates on the registry itself). The master-credential of the external system is NOT stored in the registry - only `auth_ref` (vault-ref for it, [augur.md §4.1](../augur.md)).

### `POST /v1/augur/omens` - create Omen

Permission: `omen.create`. MCP-tool: `keeper.augur.omen.create`.

**Request `OmenCreateRequest`:** `{id, source_type, endpoint, auth_ref}` - `name` kebab `^[a-z0-9-]{1,63}$`; `source_type` ∈ `vault`/`prometheus`/`elk`; `endpoint` - URL (not secret); `auth_ref` - vault-ref `vault:<mount>/<path>`.

**Response `201` `OmenView`:** `{id, source_type, endpoint, auth_ref, created_by_aid?, created_at}`.

Errors: `400` (broken JSON), `409 omen-already-exists` (`name` busy), `422 validation-failed` (broken `name`/`source_type`/`endpoint`/`auth_ref`). Audit: `omen.created`.

### `GET /v1/augur/omens` - list of Omens

Permission: `omen.list`. MCP-tool: `keeper.augur.omen.list`. Query — `offset`/`limit` ([§ Pagination](../operator-api.md#pagination)). Response `200` — `PagedResponse<OmenView>` (`{items, offset, limit, total}`).

### `GET /v1/augur/omens/{id}` — read Omen

Permission: `omen.list`. MCP-tool: `keeper.augur.omen.list` (one permission covers list and get). Response `200` `OmenView`; `404 not-found` - no entry; `422 validation-failed` - broken `name`.

### `PUT /v1/augur/omens/{id}/label` — set the display caption

Permission: `omen.label-set`. MCP-tool: `keeper.augur.omen.label-set`. OperationID: `setOmenLabel`. The caption participates in **nothing derived** — no Vault path, no RBAC scope, no snapshot directory, no CEL root ([ADR-0085](../../adr/0085-entity-id-and-label.md)) — which is what makes *"I changed the label and nothing moved"* a guarantee rather than a hope. `name` addresses the row and does not change; there is no rename operation anywhere.

This is the registry's only mutation: `endpoint` and `auth_ref` stay immutable so the Rites granted against an Omen cannot silently follow it to a different external system, and the caption is not the `rites.omen` FK.

**Request `LabelSetRequest`:** `{label? (string|null)}` — free text with capitals, spaces and punctuation; no `pattern`, no `maxLength`. `null`, an omitted field or an empty body `{}` **clears** the caption, after which consumers show `name` again; surrounding whitespace is trimmed and an all-whitespace value stores NULL.

**Response `200 OmenView`** — the row as it now reads. Errors: `400`, `403`, `404 not-found`, `422`. Audit: `omen.label_changed`, payload `{id, old_label, new_label}`.

### `DELETE /v1/augur/omens/{id}` - remove Omen

Permission: `omen.delete`. MCP-tool: `keeper.augur.omen.delete`. Cascade deletes associated Rites (`ON DELETE CASCADE`). Response `204`; `404 not-found` - no entry. Audit: `omen.revoked`.

### `POST /v1/augur/rites` - create Rite

Permission: `rite.create`. MCP-tool: `keeper.augur.rite.create`.

**Request `RiteCreateRequest`:** `{omen, subject, allow, delegate?, token_ttl?, token_num_uses?}` - `subject` is a nested object carrying **exactly one** of `sid: [...]` / `incarnation: {service, name}` / `coven: [...]` / `trait: {key, value}` ([§ Subject](#subject) below); `allow`-object, form by `source_type` Omen (vault `{paths?,policies?}` / prometheus `{queries}` / elk `{indices}`); `token_ttl`/`token_num_uses` - vault-delegate only.

**Response `201` `RiteView`:** `{id, omen, subject, allow, delegate, token_ttl?, token_num_uses?, created_by_aid?, created_at}` - `subject` echoes back with only the populated dimension present.

Errors: `400` (broken JSON), `404 not-found` (Omen does not exist), `422 validation-failed` (zero or two subject dimensions / half an `incarnation` or `trait` pair / broken `allow` / token fields). Audit: `rite.created`.

### `GET /v1/augur/rites` - list of Rite Omens

Permission: `rite.list`. MCP-tool: `keeper.augur.rite.list`. Query `omen` REQUIRED (filter by-omen, [augur.md §6](../augur.md)). Response `200` - `{items: [RiteView, …]}`; `422 validation-failed` - `omen` not transmitted / broken.

### `DELETE /v1/augur/rites/{id}` - remove Rite

Permission: `rite.delete`. MCP-tool: `keeper.augur.rite.delete`. Response `204`; `404 not-found` - no entry; `422 validation-failed` - `id` is not a positive integer. Audit: `rite.revoked`.

## Subject

`subject` is the nested object shared by Rite, [Vigil and Decree](oracle.md#subject) ([NIM-280](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-280-a-rules-subject-reads-both-levels--targeting-only)). Exactly one of its four dimensions is set; zero or two → `422`. On a response only the populated one is present.

| written as | grants |
|---|---|
| `{"sid": ["db-01.example.com"]}` | those hosts, by identity |
| `{"incarnation": {"service": "redis", "name": "redis-prod"}}` | every host on that incarnation's roster |
| `{"coven": ["prod"]}` | a host carrying the label, **and** every member of an incarnation carrying it |
| `{"trait": {"key": "tier", "value": "gold"}}` | the same two-level reach, over traits |

Both halves of `incarnation` (`service` + `name`) and of `trait` (`key` + `value`) are required together - half a pair is `422`. An empty array counts as **absent**, so `{"coven": []}` is a subject with zero dimensions and is refused rather than stored as a grant matching everything. The incarnation address is the pair `<service>.<name>`, never the bare name.

⚠ **On a Rite this is a grant, and the label dimensions read two levels.** Putting `prod` on an incarnation widens every existing `{"coven": ["prod"]}` Rite to its members - i.e. widens who may read a secret - with no Rite edited and only an incarnation-update in the audit trail. Use `incarnation` or `sid` where that is not wanted. Symmetrically, unbinding a host revokes every Rite that reached it that way, on its next request.

★ Targeting only: an Archon's RBAC scope (`soul.list on coven=prod`) is resolved from the host's own column and is never widened by an incarnation's labels ([rbac.md](../rbac.md)).
