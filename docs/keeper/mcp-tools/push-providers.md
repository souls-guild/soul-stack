# Push-Provider — MCP-tools реестра env-payload params SSH-плагинов

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.push-provider.*` (CRUD реестра `push_providers`, [ADR-032](../../adr/0032-push-orchestrator.md#adr-032-push-orchestrator-variant-c--multi-host-destiny-push-без-incarnationscenario) amendment S7-2). Транспорт, auth, формат tool declaration, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api/push-providers.md](../operator-api/push-providers.md).

### Push-Provider (5)

5 tool-ов 1:1 `keeper.push-provider.<verb>` ↔ permission `push-provider.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/push-providers*` (selector — NoSelector). Бизнес-логика (валидация sensitive-params как vault-refs, Redis invalidate-publish `push-providers:changed`) живёт в `pushprovider.Service`; tool — транспорт. Tools доступны только при подключённом реестре; при выключенном вызов возвращает `internal-error` («push-provider registry is not configured»).

#### `keeper.push-provider.create`

Создаёт Push-Provider в `push_providers` (per-provider env-payload params SSH-плагина push-flow). Sensitive params (`secret_id`/`token`/`password`/`private_key`) ОБЯЗАНЫ быть vault-refs (`vault:<path>`). После commit-а — cluster-wide invalidate через Redis pub/sub. Permission: `push-provider.create`. Endpoint: [`POST /v1/push-providers`](../operator-api/push-providers.md#post-v1push-providers--создать-push-provider). Async: нет.

**Input** (`required: name`): `{name (^[a-z][a-z0-9-]{0,62}$), params? (object; sensitive — vault-refs)}`.

**Output:** `PushProvider` — `{name, params, created_at, updated_at, created_by_aid, updated_by_aid?}`.

#### `keeper.push-provider.update`

Заменяет `params` Push-Provider-а (replace-семантика; `name` — ключ, не меняется). Sensitive-инвариант тот же. Permission: `push-provider.update`. Endpoint: [`PUT /v1/push-providers/{name}`](../operator-api/push-providers.md#put-v1push-providersname--заменить-params-replace-семантика). Async: нет.

**Input:** `{name, params (object; sensitive — vault-refs)}`. **Output:** `PushProvider`. Ошибки: `not-found` (записи нет).

#### `keeper.push-provider.delete`

Удаляет запись Push-Provider-а. Permission: `push-provider.delete`. Endpoint: [`DELETE /v1/push-providers/{name}`](../operator-api/push-providers.md#delete-v1push-providersname--удалить-запись). Async: нет.

**Input:** `{name}`. **Output:** пустой объект (REST-эквивалент — 204). Ошибки: `not-found`.

#### `keeper.push-provider.list`

Перечисление Push-Provider-ов (sort `updated_at` DESC). Permission: `push-provider.list`. Endpoint: [`GET /v1/push-providers`](../operator-api/push-providers.md#get-v1push-providers--список-push-provider-ов). Async: нет.

**Input:** `{name_pattern?, offset?, limit?}`. **Output:** `{items: array<PushProvider>, offset, limit, total}`.

#### `keeper.push-provider.read`

Читает одну запись Push-Provider-а по имени. Permission: `push-provider.read` (отделён от `list` — параллель `operator.read`↔`operator.list`). Endpoint: [`GET /v1/push-providers/{name}`](../operator-api/push-providers.md#get-v1push-providersname--прочитать-одну-запись). Async: нет.

**Input:** `{name}`. **Output:** `PushProvider`. Ошибки: `not-found`.
