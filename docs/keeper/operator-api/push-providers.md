# Push-Provider — registry endpoints env-payload params SSH plugins

Domain section [Operator API](../operator-api.md): endpoints `/v1/push-providers*` - CRUD registry `push_providers` (per-provider env-payload params SSH plugin push-flow, [ADR-032](../../adr/0032-push-orchestrator.md) amendment 2026-05-26, S7-2). Long-term canon instead of `keeper.yml::push.providers[]`. Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). The full push mode model is [push.md](../push.md). MCP side - [mcp-tools/push-providers.md](../mcp-tools/push-providers.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 5 routes) - in the root [operator-api.md → Push-Provider (5)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`PushProviderCreateRequest` / `PushProviderUpdateRequest` / `PushProvider` / `PushProviderListReply` - **source of truth by form**). `push-provider.*` - NoSelector.

`params` is the opaque form of the SSH plugin itself (`vault_addr`/`role`/`proxy_addr`/…). **Sensitive keys** (`secret_id`/`token`/`password`/`private_key`) MUST be vault-refs (`vault:<path>`) - validated by `pushprovider.Service`. After committing the mutation - cluster-wide invalidate via Redis pub/sub `push-providers:changed` → SshDispatcher re-spawn the plugin on the nearest RPC.

### `POST /v1/push-providers` — create Push-Provider

Permission: `push-provider.create`. MCP-tool: `keeper.push-provider.create`.

**Request `PushProviderCreateRequest`** (`required: name`): `{name (^[a-z][a-z0-9-]{0,62}$, = plugins.ssh_providers[].name), params? (object; sensitive — vault-refs)}`.

**Response `201 PushProvider`:** `{name, params, created_at, updated_at, created_by_aid, updated_by_aid?}`.

Errors: `400` (broken JSON), `409` (`name` busy), `422 validation-failed` (sensitive parameter not vault-ref / broken `name`). Audit: `push_provider.created` (payload - `name` + keys `params` without values).

### `GET /v1/push-providers` — list of Push Providers

Permission: `push-provider.list`. MCP-tool: `keeper.push-provider.list`. Query `name_pattern` (LIKE prefix, e.g. `vault%`) + `offset`/`limit`. Sort `updated_at` DESC. Response `200 PushProviderListReply` (`{items, offset, limit, total}`).

### `GET /v1/push-providers/{name}` - read one entry

Permission: `push-provider.read`. MCP-tool: `keeper.push-provider.read`. Response `200 PushProvider`; `404 not-found` - no entry.

### `PUT /v1/push-providers/{name}` — replace params (replace semantics)

Permission: `push-provider.update`. MCP-tool: `keeper.push-provider.update`. `name` - key, does not change. The sensitive invariant is the same as that of create.

**Request `PushProviderUpdateRequest`** (`required: params`): `{params (object; sensitive — vault-refs)}`.

**Response `200 PushProvider`.** Errors: `400`, `404 not-found`, `422 validation-failed`. Audit: `push_provider.updated`.

### `DELETE /v1/push-providers/{name}` - delete entry

Permission: `push-provider.delete`. MCP-tool: `keeper.push-provider.delete`. Response `204`; `404 not-found`. Audit: `push_provider.deleted`.
