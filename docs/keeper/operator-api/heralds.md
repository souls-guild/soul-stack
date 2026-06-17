# Herald — endpoints реестра каналов доставки уведомлений

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/heralds*` — CRUD реестра `heralds` (каналы доставки уведомлений о событиях прогонов, [ADR-052](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов), S4). Herald отвечает на вопрос «куда слать» (Tiding — «на что реагировать», [tidings.md](tidings.md)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/heralds.md](../mcp-tools/heralds.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 5 роутов) — в корневом [operator-api.md → Herald (5)](../operator-api.md#herald-5--реестр-каналов-доставки-уведомлений-о-прогонах-adr-052). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`HeraldCreateRequest` / `HeraldUpdateRequest` / `Herald` / `HeraldListReply` — **источник правды по форме**). `herald.*` — NoSelector (управление кластер-уровневое, как `push-provider.*` / `omen.*`). Роуты монтируются только при сконфигурированном реестре (`router.go`: `if heraldH != nil`); при выключенном — catch-all → `404`.

`type` — closed-enum (`webhook` в MVP; `slack`/`email` — additive post-MVP). `config` — per-type JSONB: для webhook `{ url, опц. headers }` плюс SSRF-opt-out-флаги (ниже). После commit-а каждой мутации `herald.Service` инвалидирует снимок dispatcher-кэша in-process и cross-keeper через Redis pub/sub `herald:invalidate`.

### SSRF-контур (взведён по умолчанию)

Webhook = исходящий HTTP-вызов на оператор-заданный URL → SSRF-вектор. Контур взведён по умолчанию (паттерн `core.url`, безопасный default + аудируемый opt-out):

- **https-only по умолчанию** — `http://`-URL только явным `config.http_allowed=true` (с warning).
- **deny приватных IP по умолчанию** — dial в loopback / RFC1918 / link-local / metadata-endpoint блокируется по фактически резолвнутому IP; снимается `config.allow_private=true`.
- **запрет redirect** + **timeout** на доставку (общий keeper-side `shared/netguard`).

Битый/запрещённый URL или конфликтующий config → `422 validation-failed` на create/update.

### `secret_ref` и подпись `X-SoulStack-Signature`

`secret_ref` (nullable) — **только vault-ref** (`vault:<mount>/<path>`) на signing-token; сам секрет в записи НЕ хранится (не cleartext, маскируется в ошибках). При заданном `secret_ref` webhook-доставка подписывает тело запроса заголовком:

```
X-SoulStack-Signature: sha256=<hex>
```

где `<hex>` — `HMAC-SHA256(body, signing-token)` в hex-кодировании. Приёмник валидирует так: берёт сырое тело запроса, считает `HMAC-SHA256` тем же общим signing-token-ом, сравнивает hex-результат с частью после `sha256=` (constant-time-сравнение). При `secret_ref: null` подписи нет — заголовок не выставляется.

Payload уведомления НЕ несёт resolved-секреты (`input`/vault-резолвленные значения не кладутся; проходит secret-masking, инвариант A [ADR-027](../../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)).

### `POST /v1/heralds` — создать Herald

Permission: `herald.create`. MCP-tool: `keeper.herald.create`.

**Request `HeraldCreateRequest`** (`required: name, type, config`): `{name (^[a-z0-9-]{1,63}$), type (enum: webhook), config (object), secret_ref? (vault-ref|null), enabled? (bool, опущено → true)}`.

**Response `201 Herald`:** `{name, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid}`.

Ошибки: `400` (битый JSON / unknown-поле strict-probe), `409` (`name` занят), `422 validation-failed` (битый `name`/`type`/`config`/`secret_ref` или нарушение SSRF-контура). Audit: `herald.created`.

### `GET /v1/heralds` — список Herald-каналов

Permission: `herald.list`. MCP-tool: `keeper.herald.list`. Query `offset`/`limit`. Sort `updated_at` DESC, `name` ASC. Response `200 HeraldListReply` (`{items, offset, limit, total}`).

### `GET /v1/heralds/{name}` — прочитать один канал

Permission: `herald.read`. MCP-tool: `keeper.herald.read`. Response `200 Herald`; `404 not-found` — записи нет.

### `PUT /v1/heralds/{name}` — заменить канал (replace-семантика)

Permission: `herald.update`. MCP-tool: `keeper.herald.update`. **Replace** — тело полностью заменяет mutable-поля (`type`/`config`/`secret_ref`/`enabled`); `name` (PK) immutable. Как у Push-Provider — `PUT` (полная замена), не `PATCH`. SSRF-инвариант тот же, что у create.

**Request `HeraldUpdateRequest`** (`required: type, config`): `{type (enum: webhook), config (object), secret_ref? (vault-ref|null), enabled? (bool)}`.

**Response `200 Herald`.** Ошибки: `400`, `404 not-found`, `422 validation-failed`. Audit: `herald.updated`.

### `DELETE /v1/heralds/{name}` — удалить канал

Permission: `herald.delete`. MCP-tool: `keeper.herald.delete`. Каскадно сносит связанные Tiding-подписки (`tidings.herald ON DELETE CASCADE`). Response `204`; `404 not-found`. Audit: `herald.deleted`.

## Терминалы доставки (не CRUD)

Исходы webhook-доставки worker пишет в audit как `herald.delivered` (успех) / `herald.failed` (провал после исчерпания retry); это область `keeper_internal`, не CRUD-роуты. Статусы in-flight-попыток — hot-данные в Redis (инвариант hot→Redis, [ADR-006](../../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)), синхронно в PG на каждую попытку НЕ пишутся. Семантика доставки — at-least-once (редкий дубль допустим).

## См. также

- [tidings.md](tidings.md) — парный реестр правил подписки (`event_types` → Herald).
- [mcp-tools/heralds.md](../mcp-tools/heralds.md) — MCP-сторона (`keeper.herald.*`).
- [ADR-052](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов) — дизайн Herald/Tiding (tap поверх audit-writer, at-least-once webhook-доставка, SSRF-инварианты).
- [rbac.md](../rbac.md) — каталог permissions `herald.*`.
