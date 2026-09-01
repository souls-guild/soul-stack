# Push-Provider - MCP-tools registry env-payload params SSH plugins

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.push-provider.*` (registry CRUD `push_providers`, [ADR-032](../../adr/0032-push-orchestrator.md) amendment S7-2). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api/push-providers.md](../operator-api/push-providers.md).

### Push-Provider (6)

5 tools 1:1 `keeper.push-provider.<verb>` ↔ permission `push-provider.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/push-providers*` (selector - NoSelector). Business logic (validation of sensitive-params as vault-refs, Redis invalidate-publish `push-providers:changed`) lives in `pushprovider.Service`; tool - transport. Tools are only available when the registry is connected; when disabled, the call returns `internal-error` ("push-provider registry is not configured").

#### `keeper.push-provider.create`

Creates a Push-Provider in `push_providers` (per-provider env-payload params of the push-flow SSH plugin). Sensitive params (`secret_id`/`token`/`password`/`private_key`) MUST be vault-refs (`vault:<path>`). After commit - cluster-wide invalidate via Redis pub/sub. Permission: `push-provider.create`. Endpoint: [`POST /v1/push-providers`](../operator-api/push-providers.md). Async: no.

**Input** (`required: name`): `{name (^[a-z][a-z0-9-]{0,62}$), params? (object; sensitive — vault-refs)}`.

**Output:** `PushProvider` — `{name, params, created_at, updated_at, created_by_aid, updated_by_aid?}`.

#### `keeper.push-provider.update`

Replaces `params` Push-Provider (replace semantics; `name` is a key, does not change). The sensitive invariant is the same. Permission: `push-provider.update`. Endpoint: [`PUT /v1/push-providers/{name}`](../operator-api/push-providers.md). Async: no.

**Input:** `{name, params (object; sensitive — vault-refs)}`. **Output:** `PushProvider`. Errors: `not-found` (no entry).

#### `keeper.push-provider.label-set`

Replaces the Push-Provider's **display caption** ([ADR-0085](../../adr/0085-entity-id-and-label.md)). The caption is free text - capitals and spaces allowed, nothing validates its form; `null` (or an omitted `label`) clears it and consumers fall back to showing `name`. `name` addresses the row and is NOT changed. Unlike `keeper.push-provider.update` it publishes **no** `push-providers:changed` invalidation: the dispatcher snapshot carries params, and a caption is not one of them. The caption is also not the `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` env-var name - which is why `name` keeps the letter-first rule and the caption needs no rule at all. Permission: `push-provider.label-set`. Endpoint: [`PUT /v1/push-providers/{name}/label`](../operator-api.md). Async: no.

**Input** (`required: name`): `{name (^[a-z][a-z0-9-]{0,62}$), label? (string|null)}`. **Output:** `PushProvider` - the row as it now reads. Errors: `not-found`.

#### `keeper.push-provider.delete`

Deletes a Push-Provider entry. Permission: `push-provider.delete`. Endpoint: [`DELETE /v1/push-providers/{name}`](../operator-api/push-providers.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.

#### `keeper.push-provider.list`

Enumeration of Push-Providers (sort `updated_at` DESC). Permission: `push-provider.list`. Endpoint: [`GET /v1/push-providers`](../operator-api/push-providers.md). Async: no.

**Input:** `{name_pattern?, offset?, limit?}`. **Output:** `{items: array<PushProvider>, offset, limit, total}`.

#### `keeper.push-provider.read`

Reads one Push-Provider entry by name. Permission: `push-provider.read` (separated from `list` - parallel to `operator.read`↔`operator.list`). Endpoint: [`GET /v1/push-providers/{name}`](../operator-api/push-providers.md). Async: no.

**Input:** `{name}`. **Output:** `PushProvider`. Errors: `not-found`.
