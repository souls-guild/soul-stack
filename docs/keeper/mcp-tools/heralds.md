# Herald — MCP-tools реестра каналов доставки уведомлений

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.herald.*` (CRUD реестра `heralds`, [ADR-052](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов), S4). Транспорт, auth, формат tool declaration, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике, телам, кодам ошибок — [operator-api/heralds.md](../operator-api/heralds.md).

### Herald (5)

5 tool-ов 1:1 `keeper.herald.<verb>` ↔ permission `herald.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/heralds*` (selector — NoSelector). Бизнес-логика (валидация `config`/`secret_ref` + SSRF-контур https-only + deny приватных IP + инвалидация dispatcher-кэша через Redis `herald:invalidate`) живёт в `herald.Service`; tool — транспорт. Tools доступны только при подключённом реестре (`HeraldSvc`); при выключенном вызов возвращает `internal-error` («herald registry is not configured»).

#### `keeper.herald.create`

Создаёт Herald-канал доставки уведомлений (webhook в MVP: `config.url` + опц. `headers`). SSRF-контур (https-only + deny приватных IP) взведён по умолчанию, снимается `config.http_allowed` / `config.allow_private`. `secret_ref` (опц.) — vault-ref на signing-token (подпись webhook `X-SoulStack-Signature: sha256=<hex>`, HMAC-SHA256). Permission: `herald.create`. Endpoint: [`POST /v1/heralds`](../operator-api/heralds.md#post-v1heralds--создать-herald). Async: нет.

**Input** (`required: name, type, config`): `{name (^[a-z0-9-]{1,63}$), type (enum: webhook), config (object), secret_ref? (vault-ref|null), enabled? (bool, опущено → true)}`.

**Output:** `Herald` — `{name, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid}`. Ошибки: `herald-already-exists` (`name` занят), `validation-failed` (битый `name`/`type`/`config`/`secret_ref` или нарушение SSRF-контура).

#### `keeper.herald.update`

Заменяет mutable-поля Herald-канала (replace-семантика; `name` — ключ, не меняется). SSRF-инвариант тот же, что у create. Permission: `herald.update`. Endpoint: [`PUT /v1/heralds/{name}`](../operator-api/heralds.md#put-v1heraldsname--заменить-канал-replace-семантика). Async: нет.

**Input** (`required: name, type, config`): `{name, type (enum: webhook), config (полный новый config — replace), secret_ref? (|null), enabled?}`. **Output:** `Herald`. Ошибки: `not-found` (записи нет).

#### `keeper.herald.delete`

Удаляет Herald-канал; каскадно сносит связанные Tiding-подписки (`ON DELETE CASCADE`). Permission: `herald.delete`. Endpoint: [`DELETE /v1/heralds/{name}`](../operator-api/heralds.md#delete-v1heraldsname--удалить-канал). Async: нет.

**Input:** `{name}`. **Output:** пустой объект (REST-эквивалент — 204). Ошибки: `not-found`.

#### `keeper.herald.list`

Перечисление Herald-каналов (sort `updated_at` DESC, `name` ASC). Permission: `herald.list`. Endpoint: [`GET /v1/heralds`](../operator-api/heralds.md#get-v1heralds--список-herald-каналов). Async: нет.

**Input:** `{offset?, limit?}`. **Output:** `{items: array<Herald>, offset, limit, total}`.

#### `keeper.herald.read`

Читает один Herald-канал по имени. Permission: `herald.read` (отделён от `list` — параллель `operator.read`↔`operator.list`). Endpoint: [`GET /v1/heralds/{name}`](../operator-api/heralds.md#get-v1heraldsname--прочитать-один-канал). Async: нет.

**Input:** `{name}`. **Output:** `Herald`. Ошибки: `not-found`.
