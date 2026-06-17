# Soul — endpoints реестра хостов

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/souls*` (реестр хостов, bootstrap-токены, bulk-назначение Coven-меток). Conventions, error-format, pagination, mapping-таблица целиком — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools.md → Soul](../mcp-tools/souls.md).

## Endpoint-секции

### Soul endpoints

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 8 роутов: create / coven-assign / list + read-роуты `{sid}` / `{sid}/soulprint` / `{sid}/history` под `soul.list` + issue-token + `{sid}/ssh-target`; ремарка про отложенный `soul.get`) — в корневом [operator-api.md → Soul (8)](../operator-api.md#soul-8--реестр-хостов).

#### `POST /v1/souls/coven` — bulk назначение Coven-меток

Массовое добавление (`mode: append`) / снятие (`mode: remove`) **одной** Coven-метки либо **замена** (`mode: replace`) набора Coven-меток целиком на хостах по селектору. Coven — холодная PG-метка ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)): чистый `UPDATE souls`, без записи в Redis. MCP-tool [`keeper.soul.coven-assign`](../mcp-tools/souls.md#keepersoulcoven-assign) — паритет REST. CEL-предикат — будущий слайс.

**Тело (append/remove — одна метка):**

```json
{
  "mode": "append",
  "label": "prod",
  "selector": { "all": false, "sids": ["a.example.com"], "coven": "staging", "incarnation": "redis", "status": "connected" },
  "dry_run": false
}
```

**Тело (replace — набор):**

```json
{
  "mode": "replace",
  "labels": ["prod", "dc-eu"],
  "selector": { "incarnation": "redis" }
}
```

- `mode` — `append` | `remove` | `replace`.
- `label` — одна kebab-case Coven-метка для `append`/`remove`. Обязателен для этих режимов, **запрещён** для `replace`.
- `labels` — массив kebab-case Coven-меток для `replace`. Обязателен для `replace`, **запрещён** для `append`/`remove`. Пустой массив = «снять все метки».
- `selector` — подмножество словаря таргетинга `soul.*`:
  - `all` — весь реестр ∩ scope (без host-фильтра);
  - `sids` — точечный список хостов (SID = FQDN);
  - `coven` — хосты с уже имеющейся Coven-меткой;
  - `incarnation` — хосты этой incarnation (имя incarnation — корневая Coven-метка по ADR-008; матчинг через `name = ANY(coven)`);
  - `status` — фильтр по статусу `souls`.

  Должен задавать хотя бы один критерий (`all: true` или непустой `sids`/`coven`/`incarnation`/`status`), иначе `422`. Комбинации соединяются **AND** (`incarnation: redis` + `status: connected` → connected-хосты incarnation `redis`). Свободный CEL-предикат сознательно не поддержан (доказуемость scope-проверки).
- `dry_run` — `true` посчитать `matched` без `UPDATE`. Эквивалент query-параметра `?dry_run=true` (объединяются по OR).

**Ответ `200`:** `{mode, label?, labels?, matched, changed, status, dry_run}`, где `label`/`labels` отражают применённую форму mode-а; `matched` — хостов под `selector ∩ scope`, `changed` — фактически изменённых строк (идемпотентный отсев: `append` не трогает хост, где метка уже есть; `remove` — где метки нет; `replace` — где набор уже совпадает с целевым через `coven IS DISTINCT FROM $labels`); `status` — `completed` | `partial`.

**scope-поведение:** действие двухслойно авторизуется (см. [rbac.md → Soul](../rbac.md#soul-4--реестр-хостов)). Оператор с `soul.coven-assign on coven=dev` может навешивать только `dev`/входящие в его scope метки и только на хосты с этими метками; назначаемая метка вне scope → `422`, хост вне scope просто не попадает в `UPDATE`. Для `replace` гейт (b) проверяет КАЖДУЮ метку набора (любая метка вне scope → `422` ДО UPDATE; иначе оператор с scope `dev` мог бы навесить `prod` через `labels: [dev, prod]`). Bare-permission `soul.coven-assign` или `*` снимают оба ограничения.

**partial-семантика:** массовый `UPDATE` идёт чанками с коммитом на чанк (keyset-итерация по PK) — иначе одна гигантская транзакция держала бы row-lock на `souls` десятки секунд, блокируя горячий heartbeat-flush. При сбое середины часть чанков уже закоммичена; ответ — `200` со `status: partial`. Это безопасно: операция идемпотентна, оператор просто повторяет тот же запрос (закоммиченные изменения повторно не применяются).

#### `POST /v1/souls` — зарегистрировать Soul

Permission: `soul.create`. MCP-tool: `keeper.soul.create`.

Создаёт запись в реестре `souls` в статусе `pending` ([onboarding.md → Поток онбординга](../../soul/onboarding.md#поток-онбординга-агентский-режим)). Для `transport: agent` в той же операции выпускается первый bootstrap-токен (одноразовый, TTL по умолчанию `24h`); plain-токен возвращается **один раз**, в БД пишется только `token_hash`. Для `transport: ssh` bootstrap-токен не выпускается — онбординг сводится к настройке SSH-доступа ([push.md](../push.md)), поле `bootstrap_token` в ответе отсутствует.

**Request `SoulCreateRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | SID нового хоста = FQDN ([identity.md](../../soul/identity.md)). |
| `transport` | `enum` (`agent`/`ssh`) | yes | Способ управления хостом ([push.md](../push.md)). От него зависит, выпускается ли bootstrap-токен. |
| `covens` | `list<string>` | optional | Стабильные Coven-метки хоста ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). По умолчанию `[]`. |

```json
{
  "sid": "redis-prod-01.example.com",
  "transport": "agent",
  "covens": ["redis-prod", "dc-eu-west"]
}
```

**Response `201 SoulCreateReply`:**

| Поле | Тип | Смысл |
|---|---|---|
| `sid` | `string` | SID созданной записи. |
| `transport` | `enum` | Зеркало из request. |
| `covens` | `list<string>` | Зеркало из request. |
| `status` | `enum` | Всегда `pending` сразу после создания. |
| `registered_at` | `string` (RFC 3339) | Время создания записи в Postgres. |
| `created_by_aid` | `string` | AID Архонта, выполнившего запрос (из JWT `sub`). FK на `operators(aid)`. |
| `bootstrap_token` | `string` (optional) | Plain bootstrap-токен. Присутствует только для `transport: agent`. **Возвращается один раз** — повторный выпуск через `POST /v1/souls/{sid}/issue-token`. |
| `expires_at` | `string` (RFC 3339, optional) | Срок истечения bootstrap-токена. Присутствует вместе с `bootstrap_token`. |

```json
{
  "sid": "redis-prod-01.example.com",
  "transport": "agent",
  "covens": ["redis-prod", "dc-eu-west"],
  "status": "pending",
  "registered_at": "2026-05-22T15:30:00Z",
  "created_by_aid": "archon-ops-01",
  "bootstrap_token": "bt_3f9a…",
  "expires_at": "2026-05-23T15:30:00Z"
}
```

> **Важно для клиента:** `bootstrap_token` отдаётся один раз и нигде не сохраняется. Доставьте его на хост сразу ([onboarding.md → Способы доставки](../../soul/onboarding.md#способы-доставки-токена)); потерянный токен восстановить нельзя — только выпустить новый через `POST /v1/souls/{sid}/issue-token`. Поле `bootstrap_token` маскируется во всех observability-каналах (правило masked-keys, [§ Secret masking](../operator-api.md#secret-masking-в-логах-и-трейсах)).

**Errors:** `403 forbidden`, `409 soul-already-exists`, `422 validation-failed` (невалидный SID/transport).

#### `POST /v1/souls/{sid}/issue-token` — перевыпустить bootstrap-токен

Permission: `soul.issue-token`. MCP-tool: `keeper.soul.issue-token`. Path-param: `sid` (FQDN, URL-encoded — см. [§ Conventions → ID в path](../operator-api.md#conventions)).

Выпускает новый bootstrap-токен для существующей Soul с `transport: agent` (оператор потерял токен, плановая ре-выписка). Действует инвариант `UNIQUE (sid) WHERE used_at IS NULL` — у Soul не более одного активного токена ([identity.md](../../soul/identity.md)).

- **Без `force`** при уже-активном (не сожжённом, не истёкшем) токене — `409 bootstrap-token-active`: новый токен не выпускается, чтобы не плодить параллельные валидные токены.
- **С `?force=true`** — текущий активный токен помечается использованным (`used_at = now()`, не `expires_at`: иначе строка продолжала бы держать partial-unique slot `WHERE used_at IS NULL` и reissue упёрся бы в 409), выпускается новый.

Для Soul с `transport: ssh` — `422 validation-failed` (ssh-хост не имеет bootstrap-фазы / SoulSeed).

**Query:**

| Param | Тип | Required | Смысл |
|---|---|---|---|
| `force` | `bool` | optional | Default `false`. `true` — истечь активный токен и выпустить новый. При `false` и наличии активного токена → `409 bootstrap-token-active`. |

**Request body:** пустой (TTL берётся из дефолта токена, `24h`).

**Response `200 SoulIssueTokenReply`:**

| Поле | Тип | Смысл |
|---|---|---|
| `sid` | `string` | SID. |
| `bootstrap_token` | `string` | Новый plain bootstrap-токен. **Возвращается один раз**, маскируется в логах. |
| `expires_at` | `string` (RFC 3339) | Срок истечения нового токена. |

```json
{
  "sid": "redis-prod-01.example.com",
  "bootstrap_token": "bt_a17c…",
  "expires_at": "2026-05-23T16:00:00Z"
}
```

**Errors:** `403 forbidden`, `404 not-found` (SID отсутствует в реестре), `409 bootstrap-token-active` (активный токен есть, `force` не указан), `422 validation-failed` (`transport: ssh`).

#### `GET /v1/souls` — список Souls

Permission: `soul.list`. MCP-tool: `keeper.soul.list`.

**Query:** `offset`, `limit` + фильтры:

| Param | Тип | Смысл |
|---|---|---|
| `coven` | `string` | Фильтр по coven-метке (exact-match по любому значению из `souls.coven[]`). Множественный — повторение query-параметра. |
| `status` | `enum` | `pending` / `connected` / `disconnected` / `expired`. |
| `transport` | `enum` | `agent` / `ssh` ([push.md](../push.md)). |

**Response `200 SoulListReply`:**

```json
{
  "items": [
    {
      "sid": "redis-prod-01.example.com",
      "transport": "agent",
      "status": "connected",
      "covens": ["redis-prod", "dc-eu-west"],
      "last_seen_at": "2026-05-20T15:29:55Z",
      "last_seen_by_kid": "keeper-eu-west-01",
      "registered_at": "2026-04-12T09:00:00Z"
    }
  ],
  "offset": 0,
  "limit": 50,
  "total": 137
}
```

Поля элемента (`SoulListEntry`) — проекция реестра `souls` из Postgres ([storage.md](../storage.md), [`../soul/identity.md`](../../soul/identity.md)).
