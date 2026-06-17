# Augur — endpoints реестров Omen / Rite

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/augur/omens*` + `/v1/augur/rites*` (реестры внешних систем и grant-ов брокера Augur, [ADR-025](../../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul), [augur.md](../augur.md)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/augur.md](../mcp-tools/augur.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 7 роутов) — в корневом [operator-api.md → Augur (7)](../operator-api.md#augur-7--реестры-omen--rite-adr-025--augurmd).

Реестры Augur — Omen (внешняя система) и Rite (grant) ([ADR-025](../../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul), [augur.md](../augur.md)). Selector — NoSelector (CRUD оперирует самим реестром). Master-credential внешней системы в реестре НЕ хранится — только `auth_ref` (vault-ref на него, [augur.md §4.1](../augur.md#41-таблица-omens--реестр-внешних-систем)).

### `POST /v1/augur/omens` — создать Omen

Permission: `omen.create`. MCP-tool: `keeper.augur.omen.create`.

**Request `OmenCreateRequest`:** `{name, source_type, endpoint, auth_ref}` — `name` kebab `^[a-z0-9-]{1,63}$`; `source_type` ∈ `vault`/`prometheus`/`elk`; `endpoint` — URL (не секрет); `auth_ref` — vault-ref `vault:<mount>/<path>`.

**Response `201` `OmenView`:** `{name, source_type, endpoint, auth_ref, created_by_aid?, created_at}`.

Ошибки: `400` (битый JSON), `409 omen-already-exists` (`name` занят), `422 validation-failed` (битый `name`/`source_type`/`endpoint`/`auth_ref`). Audit: `omen.created`.

### `GET /v1/augur/omens` — список Omen-ов

Permission: `omen.list`. MCP-tool: `keeper.augur.omen.list`. Query — `offset`/`limit` ([§ Pagination](../operator-api.md#pagination)). Response `200` — `PagedResponse<OmenView>` (`{items, offset, limit, total}`).

### `GET /v1/augur/omens/{name}` — прочитать Omen

Permission: `omen.list`. MCP-tool: `keeper.augur.omen.list` (одно permission покрывает list и get). Response `200` `OmenView`; `404 not-found` — записи нет; `422 validation-failed` — битый `name`.

### `DELETE /v1/augur/omens/{name}` — удалить Omen

Permission: `omen.delete`. MCP-tool: `keeper.augur.omen.delete`. Каскадно удаляет связанные Rite-ы (`ON DELETE CASCADE`). Response `204`; `404 not-found` — записи нет. Audit: `omen.revoked`.

### `POST /v1/augur/rites` — создать Rite

Permission: `rite.create`. MCP-tool: `keeper.augur.rite.create`.

**Request `RiteCreateRequest`:** `{omen, coven?, sid?, allow, delegate?, token_ttl?, token_num_uses?}` — субъект `coven` **XOR** `sid`; `allow`-объект, форма по `source_type` Omen-а (vault `{paths?,policies?}` / prometheus `{queries}` / elk `{indices}`); `token_ttl`/`token_num_uses` — только vault-delegate.

**Response `201` `RiteView`:** `{id, omen, coven?, sid?, allow, delegate, token_ttl?, token_num_uses?, created_by_aid?, created_at}`.

Ошибки: `400` (битый JSON), `404 not-found` (Omen не существует), `422 validation-failed` (нарушение XOR / битый `allow` / token-поля). Audit: `rite.created`.

### `GET /v1/augur/rites` — список Rite-ов Omen-а

Permission: `rite.list`. MCP-tool: `keeper.augur.rite.list`. Query `omen` ОБЯЗАТЕЛЕН (фильтр by-omen, [augur.md §6](../augur.md#6-авторизация-keeper-side)). Response `200` — `{items: [RiteView, …]}`; `422 validation-failed` — `omen` не передан / битый.

### `DELETE /v1/augur/rites/{id}` — удалить Rite

Permission: `rite.delete`. MCP-tool: `keeper.augur.rite.delete`. Response `204`; `404 not-found` — записи нет; `422 validation-failed` — `id` не положительное целое. Audit: `rite.revoked`.
