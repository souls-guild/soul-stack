# Push-Provider — endpoints реестра env-payload params SSH-плагинов

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/push-providers*` — CRUD реестра `push_providers` (per-provider env-payload params SSH-плагина push-flow, [ADR-032](../../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) amendment 2026-05-26, S7-2). Long-term canon вместо `keeper.yml::push.providers[]`. Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). Полная модель push-режима — [push.md](../push.md). MCP-сторона — [mcp-tools/push-providers.md](../mcp-tools/push-providers.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 5 роутов) — в корневом [operator-api.md → Push-Provider (5)](../operator-api.md#push-provider-5--реестр-env-payload-params-ssh-плагинов-push-flow-adr-032-amendment-s7-2). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`PushProviderCreateRequest` / `PushProviderUpdateRequest` / `PushProvider` / `PushProviderListReply` — **источник правды по форме**). `push-provider.*` — NoSelector.

`params` — opaque-форма самого SSH-плагина (`vault_addr`/`role`/`proxy_addr`/…). **Sensitive ключи** (`secret_id`/`token`/`password`/`private_key`) ОБЯЗАНЫ быть vault-refs (`vault:<path>`) — валидируется `pushprovider.Service`. После commit-а мутации — cluster-wide invalidate через Redis pub/sub `push-providers:changed` → SshDispatcher re-spawn-ит плагин на ближайшем RPC.

### `POST /v1/push-providers` — создать Push-Provider

Permission: `push-provider.create`. MCP-tool: `keeper.push-provider.create`.

**Request `PushProviderCreateRequest`** (`required: name`): `{name (^[a-z][a-z0-9-]{0,62}$, = plugins.ssh_providers[].name), params? (object; sensitive — vault-refs)}`.

**Response `201 PushProvider`:** `{name, params, created_at, updated_at, created_by_aid, updated_by_aid?}`.

Ошибки: `400` (битый JSON), `409` (`name` занят), `422 validation-failed` (sensitive-параметр не vault-ref / битый `name`). Audit: `push_provider.created` (payload — `name` + ключи `params` без значений).

### `GET /v1/push-providers` — список Push-Provider-ов

Permission: `push-provider.list`. MCP-tool: `keeper.push-provider.list`. Query `name_pattern` (LIKE-префикс, напр. `vault%`) + `offset`/`limit`. Sort `updated_at` DESC. Response `200 PushProviderListReply` (`{items, offset, limit, total}`).

### `GET /v1/push-providers/{name}` — прочитать одну запись

Permission: `push-provider.read`. MCP-tool: `keeper.push-provider.read`. Response `200 PushProvider`; `404 not-found` — записи нет.

### `PUT /v1/push-providers/{name}` — заменить params (replace-семантика)

Permission: `push-provider.update`. MCP-tool: `keeper.push-provider.update`. `name` — ключ, не меняется. Sensitive-инвариант тот же, что у create.

**Request `PushProviderUpdateRequest`** (`required: params`): `{params (object; sensitive — vault-refs)}`.

**Response `200 PushProvider`.** Ошибки: `400`, `404 not-found`, `422 validation-failed`. Audit: `push_provider.updated`.

### `DELETE /v1/push-providers/{name}` — удалить запись

Permission: `push-provider.delete`. MCP-tool: `keeper.push-provider.delete`. Response `204`; `404 not-found`. Audit: `push_provider.deleted`.
