# MCP-tools — каталог Keeper-tools для LLM-агентов

Нормативная спецификация **MCP-tools каталога**, который Keeper-кластер публикует на listener-е `listen.mcp.addr` ([config.md → listen](config.md#listen)). Каталог — declarative wrapper над [Operator API](operator-api.md): каждый MCP-tool жёстко соответствует одному HTTP endpoint-у `/v1/*` и одной permission ([rbac.md → Permission ↔ MCP-tool / OpenAPI endpoint](rbac.md#permission--mcp-tool--openapi-endpoint)).

**Источник правды по семантике** — [operator-api.md](operator-api.md). Этот документ описывает:

- транспорт и auth MCP-стороны;
- формат tool declaration по MCP spec;
- async-convention `_apply_id`;
- mapping ошибок RFC 7807 → MCP-tool error;
- каталог 82 tool с input/output schemas;
- что **не** публикуется как MCP-tool;
- формат SSE event-payloads для `GET /mcp/events?apply_id=<ULID>`.

Документ адресован:

- авторам LLM-агентов и MCP-host-приложений (Claude Code, IDE-плагины), подключающимся к Keeper MCP-серверу;
- разработчикам Keeper-а, реализующим MCP-handler-ы;
- `soul-lint` и аналогичным инструментам, валидирующим сценарии вызова tools.

## Транспорт и auth

| Decision | Value | Rationale |
|---|---|---|
| **Транспорт** | MCP-HTTP (Streamable HTTP) — текущая стабильная revision MCP spec. | Cross-platform, серверная модель без stdio/SSE-ограничений. |
| **Listener** | `listen.mcp.addr` — отдельный HTTP listener Keeper-кластера ([config.md → listen](config.md#listen)). | Обязательный listener согласно сквозному требованию «встроенный MCP» ([requirements.md](../requirements.md)). |
| **Auth** | `Authorization: Bearer <jwt>` — тот же JWT, что и в Operator API ([operator-api.md → Auth](operator-api.md#auth), [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). MCP-клиент передаёт header при connect. | Единая identity-модель: один Archon, один токен, один RBAC. Не плодим вторую auth-цепочку. |
| **Bootstrap-bypass** | Не применимо. MCP-tools требуют JWT всегда; первый Архонт выпускается через `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)), не через MCP. | Симметрия с Operator API. |
| **Naming** | `keeper.<resource>.<action>` (4-сегментный, точки как separators). | Зафиксировано в [rbac.md](rbac.md#permission--mcp-tool--openapi-endpoint) и [operator-api.md](operator-api.md#mapping-endpoint--mcp-tool--permission). |
| **Input naming** | `snake_case` для всех полей input. | Совпадает с JSON body HTTP endpoint-а. |
| **Output naming** | `snake_case` для бизнес-полей + top-level `_apply_id` для async-операций (underscore-префикс отличает MCP-convention от business-data). | См. [§ Async operations в MCP](#async-operations-в-mcp). |
| **Pagination** | `offset` (int, ≥0, default `0`) + `limit` (int, 1..1000, default `50`). Output list-tools: `{items, offset, limit, total}`. | Симметрия с [operator-api.md → Pagination](operator-api.md#pagination). |
| **Source of truth по семантике** | [operator-api.md](operator-api.md). | MCP-tools не дублируют business-логику. |
| **Tracing** | Каждый MCP-вызов получает OTel-span с атрибутом `archon.aid=<aid>` (из JWT `sub`) и `mcp.tool=<name>`. | Симметрия с Operator API ([operator-api.md → Conventions](operator-api.md#conventions)). |
| **Secret masking** | JWT в выходе (`jwt`-поле) пишется один раз в результат tool-а; в логи/OTel — маскируется по тем же правилам, что и в Operator API ([operator-api.md → Secret masking](operator-api.md#secret-masking-в-логах-и-трейсах)). | Single rule across транспортов. |

Подробности MCP transport / handshake / session lifecycle — в актуальной [MCP spec](https://spec.modelcontextprotocol.io/); этот документ их не дублирует.

## Формат tool declaration

Каждый MCP-tool публикуется согласно MCP spec со следующими полями:

| Поле | Тип | Смысл |
|---|---|---|
| `name` | `string` | 4-сегментное имя `keeper.<resource>.<action>` (точки — separators). |
| `description` | `string` | Краткое описание операции, 1–3 предложения для LLM. Полная семантика — в operator-api.md. |
| `inputSchema` | `object` | JSON Schema draft 2020-12, описывает входные параметры. Required-поля в `required: [...]`. Дополнительные поля запрещены (`additionalProperties: false`). |
| `outputSchema` | `object` | JSON Schema draft 2020-12 для structured output. Для async-tools содержит `_apply_id: string`. |

### Пример: `keeper.incarnation.create`

```json
{
  "name": "keeper.incarnation.create",
  "description": "Создать новый Incarnation: запустить scenario 'create' указанного Service, создать запись в Postgres. Асинхронная операция — возвращает _apply_id; статус опрашивать через keeper.incarnation.get / keeper.incarnation.history.",
  "inputSchema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "additionalProperties": false,
    "required": ["name", "service"],
    "properties": {
      "name": {
        "type": "string",
        "pattern": "^[a-z][a-z0-9-]*$",
        "description": "Имя нового instance, корневая Coven-метка."
      },
      "service": {
        "type": "string",
        "description": "Имя сервиса из keeper.yml → services[].name."
      },
      "input": {
        "type": "object",
        "description": "Input для scenario 'create', валидируется против input-схемы сервиса.",
        "default": {}
      }
    }
  },
  "outputSchema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "additionalProperties": false,
    "required": ["_apply_id", "incarnation"],
    "properties": {
      "_apply_id": { "type": "string", "description": "ULID запуска." },
      "incarnation": { "type": "string", "description": "Имя созданного instance." }
    }
  }
}
```

По этому шаблону декларируется каждый из 82 tool каталога. Полное соответствие input/output schema полям HTTP endpoint-а — см. [operator-api.md → Endpoint-секции](operator-api.md#endpoint-секции).

**Enum serialization.** MCP-сервер использует тот же enum mapping (короткие snake_case значения без family-prefix: `"ready"`, `"connected"`, `"agent"`, …), что HTTP API — см. [operator-api.md → Conventions → Enum serialization](operator-api.md#conventions). Полные proto-константы (`INCARNATION_STATUS_READY`, …) в MCP input/output не пробрасываются.

## Async operations в MCP

В MCP-протоколе нет HTTP status codes — `202 Accepted + body {apply_id}` Operator API в MCP отображается как **structured output** tool-а с top-level полем `_apply_id` (underscore-prefix отличает MCP-convention от business-data).

Список async-tools: `keeper.incarnation.create`, `keeper.incarnation.rerun-create`, `keeper.incarnation.run`, `keeper.incarnation.upgrade`, `keeper.incarnation.destroy`, `keeper.push.apply`, `keeper.push.cleanup`. `keeper.soul.errand.run` — sync-by-default (server-cap 30s), при превышении возвращает `async=true` со `status=running`; poll через `keeper.errand.get`.

**Опрос статуса:**

- `keeper.incarnation.get` → читает `status` / `status_details` instance.
- `keeper.incarnation.history` → возвращает записи `state_history` с полем `apply_id`; элемент с `apply_id == <ULID>` появляется после успешного коммита.

Отдельного tool-а `keeper.apply.get` в MVP **нет** — симметрия с Operator API ([operator-api.md → Async operations](operator-api.md#async-operations)).

## Errors

RFC 7807 ProblemDetails Operator API ([operator-api.md → Error format](operator-api.md#error-format-rfc-7807)) отображается в MCP-tool error следующим образом:

| RFC 7807 поле | MCP-tool error поле | Преобразование |
|---|---|---|
| `type` (URI suffix под `https://soul-stack.io/errors/`) | `code` | Берётся suffix URN: `https://soul-stack.io/errors/incarnation-locked` → `code: "incarnation-locked"`. |
| `title` | — | Не пробрасывается отдельно (короткий текст, дублирует `code`). |
| `status` | — | HTTP status code в MCP не применим; MCP-клиент не парсит его. |
| `detail` | `message` | Свободный текст из `detail` ProblemDetails. |
| `instance` | `data.instance` | URI неудачного запроса (`/v1/...`); полезен для аудита. Поле `data` MCP error может содержать произвольный structured-payload. |

Полный список error codes — стабильные suffix-ы URN из [operator-api.md → Типы ошибок](operator-api.md#типы-ошибок):

| Code | Когда возникает в MCP |
|---|---|
| `unauthenticated` | JWT отсутствует/не валиден/истёк. |
| `forbidden` | RBAC-проверка не прошла. `message` содержит требуемую permission. |
| `not-found` | Ресурс не существует. |
| `validation-failed` | Семантическая ошибка валидации input. |
| `malformed-request` | Неверный JSON/неверные query params. |
| `incarnation-locked` | Incarnation в `error_locked` — вызвать `keeper.incarnation.unlock` перед новым прогоном. В `keeper.incarnation.rerun-create` — статус не `error_locked` ИЛИ последний упавший сценарий не `create` (rerun перезапускает только `create`). |
| `migration-failed` | Incarnation в `migration_failed` — нужен ручной разбор state_history. |
| `would-lock-out-cluster` | Операция оставила бы кластер без активного Архонта с эффективным `*`-permission. Возникает в `keeper.operator.revoke` (отзыв последнего `*`-Архонта), в role-операциях `keeper.role.delete` / `keeper.role.update` / `keeper.role.revoke-operator` (см. [§ Role](#role-6)) и в synod-операциях `keeper.synod.delete` / `keeper.synod.remove-operator` / `keeper.synod.revoke-role` (эффективный `*` может приходить через Synod, см. [§ Synod](#synod-8)). |
| `role-not-found` | Роль с указанным `name` отсутствует в `rbac_roles` (`keeper.role.delete` / `keeper.role.update` / `keeper.role.grant-operator` / `keeper.role.revoke-operator`; `keeper.synod.grant-role` над несуществующей ролью). |
| `role-already-exists` | `name` роли занят (`keeper.role.create`). |
| `role-builtin` | Роль с `builtin=true` (`cluster-admin`) — `keeper.role.delete` / `keeper.role.update` над ней запрещены. `grant-operator` / `revoke-operator` над builtin-ролью разрешены. |
| `synod-not-found` | Synod-группа с указанным `name` отсутствует в `synods` (`keeper.synod.update` / `keeper.synod.delete` / `keeper.synod.add-operator` / `keeper.synod.grant-role`). |
| `synod-already-exists` | `name` группы занят (`keeper.synod.create`). |
| `synod-builtin` | Группа с `builtin=true` — `keeper.synod.delete` над ней запрещён. |
| `incarnation-already-exists` | Incarnation с указанным `name` уже создан. |
| `operator-already-exists` | AID уже занят. |
| `soul-already-exists` | SID уже зарегистрирован в реестре `souls`. |
| `bootstrap-token-active` | У Soul уже есть активный bootstrap-токен — повторный выпуск с `force: true` (`keeper.soul.issue-token`). |
| `plugin-not-in-cache` | Активного слота плагина `(namespace, name)` нет в кеше host-а (нет `current`-symlink / битый слот, `keeper.plugin.allow`). |
| `sigil-already-active` | Активный допуск на `(namespace, name, ref)` уже есть (`keeper.plugin.allow`). |
| `sigil-not-found` | Активной записи allow-list-а на `(namespace, name, ref)` нет (`keeper.plugin.revoke`). |
| `sigil-key-not-found` | Ключа подписи с таким `key_id` нет (`keeper.sigil.key.set-primary` / `keeper.sigil.key.retire`). |
| `sigil-key-last-active` | Нельзя вывести последний active-ключ подписи — набор не должен опустеть (`keeper.sigil.key.retire`). |
| `sigil-key-primary` | Нельзя вывести primary-ключ напрямую — сперва set-primary другому active (`keeper.sigil.key.retire`). |
| `sigil-key-concurrent-change` | Гонка установки primary либо ключ retired при set-primary; retry (`keeper.sigil.key.introduce` / `set-primary`). |
| `service-already-exists` | `name` Service-а занят в реестре `service_registry` (`keeper.service.register`). |
| `service-not-registered` | `service` отсутствует в `keeper.yml → services[]`. |
| `omen-already-exists` | `name` Omen-а занят в реестре `omens` (`keeper.augur.omen.create`). |
| `errand-not-cancellable` | Errand уже в терминальном статусе — отменять нечего (`keeper.errand.cancel`, ADR-033 slice E5). |
| `internal-error` | Незапланированная ошибка; полная диагностика — в OTel-trace. |

> Неизвестный-но-валидный сценарий в `keeper.incarnation.run` — **не** ошибка вызова: tool возвращает `_apply_id` (async-accepted), прогон затем уходит в `error_locked` (`scenario_load_failed`), статус опрашивается через `keeper.incarnation.get`. Симметрично [operator-api/incarnations.md → `POST …/scenarios/{scenario}`](operator-api/incarnations.md#post-v1incarnationsnamescenariosscenario--запустить-произвольный-сценарий).

Расширение списка кодов — only-add симметрично Operator API.

## Каталог 82 MCP-tool

1:1 с HTTP endpoints из [operator-api.md → Mapping endpoint ↔ MCP-tool ↔ permission](operator-api.md#mapping-endpoint--mcp-tool--permission). Для каждого tool: input schema (краткая таблица полей), output schema, cross-link на endpoint-секцию operator-api.md как источник правды по семантике.

Имена полей input — 1:1 с JSON body HTTP endpoint-а; пути в output — те же, что в HTTP response. Async-tools помечены в столбце **Async**.

### Operator (3)

Вынесены в доменный файл — [mcp-tools/operator.md](mcp-tools/operator.md): `keeper.operator.create`, `keeper.operator.revoke`, `keeper.operator.issue-token`. Источник правды по семантике — [operator-api/operator.md](operator-api/operator.md).

### Role (6)

Вынесены в доменный файл — [mcp-tools/roles.md](mcp-tools/roles.md): `keeper.role.create`, `keeper.role.delete`, `keeper.role.list`, `keeper.role.update`, `keeper.role.grant-operator`, `keeper.role.revoke-operator`. Источник правды по семантике — [operator-api/roles.md](operator-api/roles.md) (тела и инварианты — [rbac.md → REST `/v1/roles`](rbac.md#rest-v1roles)).

### Synod (8)

Вынесены в доменный файл — [mcp-tools/synods.md](mcp-tools/synods.md): `keeper.synod.create`, `keeper.synod.delete`, `keeper.synod.list`, `keeper.synod.update`, `keeper.synod.add-operator`, `keeper.synod.remove-operator`, `keeper.synod.grant-role`, `keeper.synod.revoke-role`. Источник правды по семантике — [operator-api/synods.md](operator-api/synods.md) (тела и инварианты — [rbac.md → REST `/v1/synods`](rbac.md#rest-v1synods)).

### Incarnation (10)

Вынесены в доменный файл — [mcp-tools/incarnations.md](mcp-tools/incarnations.md): `keeper.incarnation.create`, `keeper.incarnation.rerun-create`, `keeper.incarnation.run`, `keeper.incarnation.get`, `keeper.incarnation.list`, `keeper.incarnation.history`, `keeper.incarnation.unlock`, `keeper.incarnation.upgrade`, `keeper.incarnation.check-drift`, `keeper.incarnation.destroy` — десять tool-ов с MCP-парностью к REST-роутам [operator-api.md → Incarnation (11)](operator-api.md#incarnation-11--жизненный-цикл-runtime-инстансов-adr-009). 11-й REST-роут `PATCH /v1/incarnations/{name}/hosts` — REST-only (MCP-tool-а нет). Источник правды по семантике — [operator-api/incarnations.md](operator-api/incarnations.md).

> **Счётчик-ремарка.** В `keeper/internal/mcp/manifest.go` секция-комментарий помечен «Incarnation (9)», но фактических tool-деклараций — **10** (`rerun-create` добавлен позже комментария). Источник правды по числу — сам список деклараций, не комментарий; здесь и в operator-api.md — 10.

### Soul (5)

Вынесены в доменный файл — [mcp-tools/souls.md](mcp-tools/souls.md): `keeper.soul.create`, `keeper.soul.issue-token`, `keeper.soul.coven-assign`, `keeper.soul.list`, `keeper.soul.ssh-target.update`. Источник правды по семантике — [operator-api/souls.md](operator-api/souls.md). Read-роуты реестра (`GET /v1/souls/{sid}`, `/soulprint`, `/history`) — REST-only (MCP-tool-ов нет).

### Plugin (3)

Вынесены в доменный файл — [mcp-tools/plugins.md](mcp-tools/plugins.md): `keeper.plugin.allow`, `keeper.plugin.revoke`, `keeper.plugin.list`. Источник правды по семантике — [operator-api/plugins.md](operator-api/plugins.md) (детали Integrity-model — [plugins.md → Integrity-model](plugins.md#integrity-model)). Ротация самих ключей **подписи** (отдельная зона) — [mcp-tools/sigils.md](mcp-tools/sigils.md).

### Sigil-key (4)

Вынесены в доменный файл — [mcp-tools/sigils.md](mcp-tools/sigils.md): `keeper.sigil.key.introduce`, `keeper.sigil.key.list`, `keeper.sigil.key.set-primary`, `keeper.sigil.key.retire`. Источник правды по семантике — [operator-api/sigils.md](operator-api/sigils.md). Допуски самих бинарей (allow-list) — [mcp-tools/plugins.md](mcp-tools/plugins.md).

### Service (4)

Реестр Service-ов `service_registry` (ADR-028-паттерн RBAC-storage: каталог `services[]` переносится из статического `keeper.yml` в managed-через-OpenAPI/MCP PG-таблицу). 1:1 с REST `POST/GET/PATCH/DELETE /v1/services*` и permission (`keeper.service.<action>` ↔ `service.<action>`, selector — NoSelector, как `operator.*`/`role.*`). Бизнес-логика (валидация `name`/`git`/`ref`/`refresh`, cluster-wide-инвалидация снимка после commit-а) живёт в `serviceregistry.Service`; tool — транспорт. Tools доступны только при подключённом реестре; при выключенном вызов возвращает `internal-error` («service registry is not configured»).

#### `keeper.service.register`

Регистрирует Service в `service_registry`: git-источник service-репо + `ref` (версия = git ref, [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)) + опц. авто-`refresh`. Permission: `service.register`. Endpoint: [`POST /v1/services`](operator-api.md). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя Service-а (kebab-case `^[a-z][a-z0-9-]*$`). |
| `git` | `string` | yes | git-источник service-репо (URL; не секрет). |
| `ref` | `string` | yes | git ref (tag/branch) — версия Service-а. |
| `refresh` | `string` | no | duration авто-refresh (`5m`); опущено — без авто-refresh. |

**Output:** `ServiceView` — `{name, git, ref, refresh?, created_by_aid?, updated_by_aid?, created_at, updated_at}`.

Ошибки: `service-already-exists` (`name` занят), `not-found` (AID создателя отсутствует в `operators`), `validation-failed` (битый `name`/`git`/`ref`/`refresh`). Audit: `service.registered`.

#### `keeper.service.update`

Заменяет mutable-поля записи Service-а (`git`/`ref`/`refresh`, replace-семантика); `name` — ключ, не меняется. Permission: `service.update`. Endpoint: [`PATCH /v1/services/{name}`](operator-api.md). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя Service-а (ключ записи). |
| `git` | `string` | yes | Новый git-источник. |
| `ref` | `string` | yes | Новый git ref. |
| `refresh` | `string` | no | duration авто-refresh (`5m`). |

**Output:** `ServiceView`.

Ошибки: `not-found` (записи нет либо AID правщика отсутствует в `operators`), `validation-failed` (битый `git`/`ref`/`refresh`). Audit: `service.updated`.

#### `keeper.service.list`

Перечисление зарегистрированных Service-ов (sort `name` ASC). Permission: `service.list`. Endpoint: [`GET /v1/services`](operator-api.md). Async: нет.

**Input:** пустой объект.

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `services` | `array<ServiceView>` | Элементы — `{name, git, ref, refresh?, created_by_aid?, updated_by_aid?, created_at, updated_at}`. |

#### `keeper.service.deregister`

Удаляет запись Service-а из `service_registry` по имени. Permission: `service.deregister`. Endpoint: [`DELETE /v1/services/{name}`](operator-api.md). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя Service-а. |

**Output:** пустой объект (REST-эквивалент — 204 No Content).

Ошибки: `not-found` (записи нет). Audit: `service.deregistered`.

### Augur (6)

Вынесены в доменный файл — [mcp-tools/augur.md](mcp-tools/augur.md): `keeper.augur.omen.create`, `keeper.augur.omen.list`, `keeper.augur.omen.delete`, `keeper.augur.rite.create`, `keeper.augur.rite.list`, `keeper.augur.rite.delete`. Источник правды по семантике — [operator-api/augur.md](operator-api/augur.md). **Live-fetch от Soul (`AugurRequest`) этими tool-ами НЕ управляется** ([rbac.md §Augur](rbac.md)).

### Oracle (6)

Вынесены в доменный файл — [mcp-tools/oracle.md](mcp-tools/oracle.md): `keeper.oracle.vigil.create`, `keeper.oracle.vigil.list`, `keeper.oracle.vigil.delete`, `keeper.oracle.decree.create`, `keeper.oracle.decree.list`, `keeper.oracle.decree.delete`. Источник правды по семантике — [operator-api/oracle.md](operator-api/oracle.md). **Reactor-флоу (Portent → match Decree → enqueue) этими tool-ами НЕ управляется** ([rbac.md §Oracle](rbac.md)).

### Errand (4)

Вынесены в доменный файл — [mcp-tools/errands.md](mcp-tools/errands.md): `keeper.soul.errand.run`, `keeper.errand.list`, `keeper.errand.get`, `keeper.errand.cancel`. Источник правды по семантике — [operator-api/errands.md](operator-api/errands.md).

### Voyage (4)

Вынесены в доменный файл — [mcp-tools/voyages.md](mcp-tools/voyages.md): `keeper.voyage.start`, `keeper.voyage.get`, `keeper.voyage.list`, `keeper.voyage.cancel`. `POST /v1/voyages/preview` — REST-only (MCP-tool нет). Источник правды по семантике — [operator-api/voyages.md](operator-api/voyages.md).

### Push (2)

Вынесены в доменный файл — [mcp-tools/push.md](mcp-tools/push.md): `keeper.push.apply`, `keeper.push.cleanup`. Источник правды по семантике — [operator-api/push.md](operator-api/push.md).

### Cloud (2)

#### `keeper.provider.create`

Создание Provider. Permission: `provider.create`. **Tool без REST-роута** — `POST /v1/providers` не реализован (cloud-CRUD отложен, [operator-api.md → Cloud](operator-api.md#cloud--отложено-rest-роутов-нет)); tool остаётся в manifest как stub. Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Имя Provider. |
| `type` | `string` | yes | Имя CloudDriver-плагина. |
| `region` | `string` | yes | Регион/zone. |
| `credentials_ref` | `string` (`vault:<path>`) | yes | Vault-ref до credentials. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `name`, `type`, `region`, `credentials_ref` | `string` | Зеркало input. |
| `created_at` | `string` (RFC 3339) | Время создания. |
| `created_by_aid` | `string` | AID создателя. |

#### `keeper.profile.create`

Создание Profile. Permission: `profile.create`. **Tool без REST-роута** — `POST /v1/profiles` не реализован (cloud-CRUD отложен, [operator-api.md → Cloud](operator-api.md#cloud--отложено-rest-роутов-нет)); tool остаётся в manifest как stub. Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Имя Profile. |
| `provider` | `string` | yes | Имя зарегистрированного Provider. |
| `params` | `object` | yes | Параметры VM, валидируются против `profile_schema` CloudDriver-плагина. |
| `cloud_init` | `string` | optional | Сырая cloud-init userdata. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `name`, `provider`, `params`, `cloud_init` | `string` / `object` | Зеркало input. |
| `created_at` | `string` (RFC 3339) | Время создания. |
| `created_by_aid` | `string` | AID создателя. |

### Push-Provider (5)

Вынесены в доменный файл — [mcp-tools/push-providers.md](mcp-tools/push-providers.md): `keeper.push-provider.create`, `keeper.push-provider.update`, `keeper.push-provider.delete`, `keeper.push-provider.list`, `keeper.push-provider.read`. Источник правды по семантике — [operator-api/push-providers.md](operator-api/push-providers.md). Sensitive params (`secret_id`/`token`/`password`/`private_key`) ОБЯЗАНЫ быть vault-refs.

### Herald (5)

Вынесены в доменный файл — [mcp-tools/heralds.md](mcp-tools/heralds.md): `keeper.herald.create`, `keeper.herald.update`, `keeper.herald.delete`, `keeper.herald.list`, `keeper.herald.read`. Источник правды по семантике — [operator-api/heralds.md](operator-api/heralds.md). Каналы доставки уведомлений о прогонах ([ADR-052](../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)); webhook + SSRF-guard (https-only/deny-private по умолчанию), `secret_ref` — vault-ref на signing-token (подпись `X-SoulStack-Signature: sha256=<hex>`).

### Tiding (5)

Вынесены в доменный файл — [mcp-tools/tidings.md](mcp-tools/tidings.md): `keeper.tiding.create`, `keeper.tiding.update`, `keeper.tiding.delete`, `keeper.tiding.list`, `keeper.tiding.read`. Источник правды по семантике — [operator-api/tidings.md](operator-api/tidings.md). Правила подписки (`event_types` area-glob в scope прогонов → Herald); `herald` — FK на существующий Herald.

### Cadence (0) и Choir (0) — REST-only

Домены **Cadence** (`/v1/cadences*`, [ADR-046](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)) и **Choir** (`/v1/incarnations/{name}/choirs*`, [ADR-044](../adr/0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации)) MCP-tool-ов **не имеют** — `manifest.go` их не содержит. Стаб-файлы фиксируют отсутствие: [mcp-tools/cadences.md](mcp-tools/cadences.md), [mcp-tools/choirs.md](mcp-tools/choirs.md). Управление этими доменами — только через Operator API ([operator-api/cadences.md](operator-api/cadences.md), [operator-api/choirs.md](operator-api/choirs.md)).

## Что НЕ публикуется как MCP-tool

### Health / Meta endpoints

`/healthz`, `/readyz`, `/metrics`, `/openapi.yaml` — **не MCP-tools**. LLM-агент не должен дёргать healthcheck/metrics; их потребители — оркестраторы, monitoring, документация. Доступ — напрямую через HTTP на `listen.openapi.addr` / `listen.metrics.addr` без auth (метрики — на отдельном listener-е, см. [config.md → listen](config.md#listen)).

### Bootstrap первого Архонта

`keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) — administrative subcommand, выполняется на keeper-хосте оператором с shell-доступом. MCP-tool-а вида `keeper.bootstrap.init` **нет**: первый Архонт создаётся, когда реестр `operators` пуст; MCP-доступ требует JWT, который ещё не выпущен. Bootstrap-bypass через MCP не вводится сознательно — это снизит security-границу.

### Будущие чтения и удаления

Каталог 82 tool — 1:1 с MVP-каталогом permissions из [rbac.md → Каталог permissions](rbac.md#каталог-permissions). Чтения / удаления, отложенные до появления соответствующих permissions (`operator.get`, `soul.get`, `provider.list`, `profile.list`, `provider.delete`, `profile.delete` и т.п.), добавляются в этот каталог одним PR с расширением `rbac.md` + `operator-api.md` + этого документа.

## SSE event payloads

`GET /mcp/events?apply_id=<ULID>` отдаёт Server-Sent Events stream с typed apply-event-payloads, публикуемыми in-memory шиной [`keeper/internal/applybus`](https://github.com/souls-guild/soul-stack/tree/main/keeper/internal/applybus). Шина связывает publisher-ов (Keeper-side handler-ы EventStream-payload-ов [`TaskEvent`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_taskevent.go) / [`RunResult`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_runresult.go) и, в будущем, scenario-runner) с SSE-subscriber-ами.

Этот раздел — нормативная фиксация формата SSE-frame и payload-ов; код-publisher-ы и handler — у источника правды по семантике (см. ссылки выше).

### SSE frame

Каждое apply-событие транслируется одним SSE-frame-ом:

```
event: <kind>
id: <apply_id>
data: <json payload>

```

- `event:` — имя `EventKind` (см. ниже), стабильный snake-case с точкой-разделителем.
- `id:` — `apply_id` события (ULID); SSE-клиент видит его в `MessageEvent.lastEventId`.
- `data:` — одна строка JSON-payload (без переносов); структура зависит от `kind`-а (см. § Per-kind schema).
- Завершающий пустой строкой по SSE-spec.

### Common payload fields

Все payload-ы — JSON-объект с тремя обязательными ключами:

| Поле | Тип | Описание |
|---|---|---|
| `apply_id` | string ULID | Идентификатор apply-run; дублирует SSE-frame `id:`-line (subscriber может опираться на любой). |
| `kind` | string (closed enum) | Тип события (см. ниже); дублирует SSE-frame `event:`-line. |
| `sid` | string FQDN | Soul-инициатор события. Источник payload-а: Soul-side TaskEvent/RunResult, mTLS peer cert при доставке EventStream — авторитативный SID. |

**Чего нет в SSE payload-е:**

- Поля `at` (timestamp публикации) в JSON-payload **нет** — оно живёт во внутренней структуре `applybus.Event.At` и используется только для логирования/диагностики. SSE-клиент берёт timestamp у себя при получении event-а либо проставляет на стороне keeper-side scenario-runner-а в `state_changes`/audit-log (источник — Postgres `audit_log.created_at`).
- Поля `register_data` (TaskEvent) в SSE payload-е **нет**. Это сознательное упрощение: register-data может быть крупным и/или содержать секреты — для аудит-цепочки она пишется в `audit_log.payload.register_data` (с прогоном через [`audit.MaskSecrets`](https://github.com/souls-guild/soul-stack/tree/main/shared/audit)); SSE-клиенту, отслеживающему ход прогона, она не нужна.

### Event kinds (closed enum)

| `kind` | Когда публикуется | Источник |
|---|---|---|
| `apply.started` | Apply-прогон начат. Зарезервировано за keeper-side scenario-runner-ом; в M0.7.c publisher-а **нет**, но SSE-клиент должен распознавать `kind` для forward-compat. | scenario-runner (post-MVP). |
| `task.executed` | Одна задача внутри прогона завершилась (любой `TaskStatus`: OK / FAILED / CANCELLED / SKIPPED). | [`handleTaskEvent`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_taskevent.go) при получении `TaskEvent` от Soul. |
| `apply.completed` | Прогон завершился успешно (`RUN_STATUS_SUCCESS`). | [`handleRunResult`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/grpc/events_runresult.go). |
| `apply.failed` | Прогон завершился ошибкой (`RUN_STATUS_FAILED` / `RUN_STATUS_ERROR_LOCKED` / любой неуспех, не отнесённый к `cancelled`). | `handleRunResult`. |
| `apply.cancelled` | Прогон отменён (`RUN_STATUS_CANCELLED`). | `handleRunResult`. |

Расширение enum-а — only-add: новые kind-ы добавляются здесь и в [`applybus.EventKind`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/applybus/bus.go) одним PR, без переименования существующих.

### Per-kind schema

#### `apply.started`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.started",
  "sid": "host-01.example.com"
}
```

Без дополнительных полей. Зарезервировано за scenario-runner-ом — в M0.7.c таких событий публикатор не выпускает, но SSE-клиент не должен ронять stream при встрече kind-а.

#### `task.executed`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "task.executed",
  "sid": "host-01.example.com",
  "task_idx": 3,
  "task_status": "TASK_STATUS_FAILED",
  "error": {
    "code": "module.failed",
    "module": "core.pkg"
  }
}
```

| Поле | Тип | Required | Описание |
|---|---|---|---|
| `task_idx` | integer (≥0) | yes | Индекс задачи в `RenderedTask[]` apply-прогона. |
| `task_status` | string | yes | Полное имя enum-константы `TaskStatus` из proto (`TASK_STATUS_OK` / `TASK_STATUS_FAILED` / `TASK_STATUS_CANCELLED` / …). При расширении enum-а Soul-side новые значения уходят в payload «как есть» — SSE-клиент должен трактовать неизвестные значения как `failed`-аналог для UX, не падать. |
| `error` | object | optional | Заполнено только при `task_status` ≠ OK. Структура — `{code, module}` (подмножество `keeperv1.ModuleError`). **`message` (stderr задачи) на SSE НЕ публикуется** (BUG-3 floor): stderr упавшей задачи может нести plaintext-секрет (особенно `no_log: true`-задачи), который `MaskSecrets` по vault-ref не ловит; флаг `no_log` живёт в run-goroutine-е, а SSE-publish (grpc-слой) на multi-Keeper его не знает (ADR-002, ADR-012(d)). Детальную безопасную причину оператор получает через `status_details` / `GET /v1/incarnations/<name>` (там no_log подавлён + двойной `MaskSecrets`, см. `scenario.failureReason`). `code`/`module` несут триаж без тела stderr. |

#### `apply.completed`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.completed",
  "sid": "host-01.example.com",
  "run_status": "RUN_STATUS_SUCCESS",
  "state_changes": {
    "users": [{"name": "alice", "action": "added"}]
  }
}
```

| Поле | Тип | Required | Описание |
|---|---|---|---|
| `run_status` | string | yes | Полное имя enum-константы `RunStatus` из proto. Для `apply.completed` — всегда `RUN_STATUS_SUCCESS`. |
| `state_changes` | object | optional | Дельта состояния, посчитанная Soul-side scenario-runner-ом и переданная в `RunResult.state_changes`. JSON-объект (decoded из `google.protobuf.Struct`). Может отсутствовать, если scenario не модифицирует state. |

#### `apply.failed`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.failed",
  "sid": "host-01.example.com",
  "run_status": "RUN_STATUS_FAILED"
}
```

| Поле | Тип | Required | Описание |
|---|---|---|---|
| `run_status` | string | yes | `RUN_STATUS_FAILED` / `RUN_STATUS_ERROR_LOCKED` / любой иной не-success / не-cancelled. SSE-клиент должен распознавать конкретный sub-status по этому полю. |

Поле `state_changes` для `apply.failed` **не публикуется**: на ошибочный прогон state не перезаписывается (см. `commitRunState`), и отдавать частичную дельту наружу запрещено. Per-task диагностика собирается клиентом из предшествующих `task.executed`-событий с `error`-полем.

#### `apply.cancelled`

```json
{
  "apply_id": "01J9F0K8XA7YZ2EXAMPLEULID01",
  "kind": "apply.cancelled",
  "sid": "host-01.example.com",
  "run_status": "RUN_STATUS_CANCELLED"
}
```

| Поле | Тип | Required | Описание |
|---|---|---|---|
| `run_status` | string | yes | Всегда `RUN_STATUS_CANCELLED`. |

`state_changes` отсутствует по той же причине, что и для `apply.failed`.

### Lifecycle: subscribe semantics

- **In-memory only.** В M0.7.c шина — in-memory single-Keeper, без persistence-а: событие доставляется только подписчикам, существующим на момент `Publish`-а. Late subscriber (подключился ПОСЛЕ публикации события) уже отданное событие **не получит** — replay-а в M0.7.c **нет**.
- **Subscribe-then-call.** Корректный порядок клиента: сначала подписаться `GET /mcp/events?apply_id=<ULID>`, дождаться `200 OK` + первого `:keepalive\n\n` от сервера, и только после этого вызывать async-tool (`tools/call keeper.incarnation.create` / `keeper.incarnation.run` / …). Иначе риск пропустить ранние `task.executed`-события мелких прогонов.
- **Buffer overflow → drop-oldest.** Per-subscriber buffer — 64 события ([`applybus.SubscriberBufferSize`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/applybus/bus.go)). При переполнении (slow client) шина дропает самое старое событие и пишет `warn` в slog — publisher никогда не блокируется. Клиенту следует читать stream без задержек; гарантия порядка «новее старее» сохраняется.
- **Connection close → auto-unsubscribe.** Закрытие SSE-соединения (клиент-side `EventSource.close()` или транспортный disconnect) отменяет HTTP-request-context; шина детектирует `ctx.Done()`, удаляет subscriber-а из map-ы и закрывает канал. Явного unsubscribe-вызова не требуется.

### Heartbeat

Каждые 30 секунд (`sseHeartbeatInterval` в [`keeper/internal/mcp/sse.go`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/mcp/sse.go)) сервер пишет в stream SSE-comment-line:

```
:keepalive

```

Это **не event** (нет `event:`/`data:`-полей); reverse-proxy (nginx, AWS ALB) и браузерный `EventSource` уважают comment-line, не доставляют его JS-обработчику и не закрывают соединение по idle-timeout. Frequency не настраивается в M0.7.c.

### Auth и RBAC

- SSE-handler требует тот же JWT, что и `POST /mcp` (см. § Транспорт и auth). На auth-ошибку отдаётся HTTP `401` / `400` c JSON-error-body (не SSE-формат) — клиент ещё не подписался, нет смысла открывать stream только чтобы сразу закрыть.
- Отдельной RBAC-permission на `/mcp/events` в M0.7.c **нет**: подписаться на свой `apply_id` может любой авторизованный Архонт. Detail check «может ли этот AID знать состояние конкретного apply_id» отложен до scenario-runner-а (mapping `apply_id → archon_aid` будет частью runner-таблицы).

### Cluster-mode (cross-instance routing)

В горизонтально масштабируемом кластере (ADR-002) Soul прогона может быть подключён к Keeper-инстансу B, тогда как SSE-subscriber `GET /mcp/events?apply_id=X` висит на Keeper-инстансе A. Cross-instance routing applybus-событий **реализован** через Redis pub/sub ([ADR-006(c.1)](https://github.com/souls-guild/soul-stack/blob/main/docs/adr/0006-cache-redis.md)): publisher транслирует событие в **шардированный канал `events:shard:<n>`**, где `n = fnv32a(apply_id) % 256` (фиксированное множество из K=256 шардов). Каждый Keeper держит Redis-bridge **per-shard** (не per-applyID): первый Subscribe любого applyID данного shard-а поднимает одну подписку на shard-канал, остальные applyID того же shard-а её переиспользуют. Forward-loop фильтрует входящие события по `envelope.apply_id` и раздаёт только local-subscriber-ам соответствующего applyID — коллизия двух прогонов в один shard (частота ≈ 1/K) их payload-ы не смешивает. Sticky-session на LB не требуется: subscriber на любом инстансе получит события прогона с любого другого. Реализация — [`keeper/internal/redis/applybus.go`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/redis/applybus.go) (`ApplyBusChannel`/`ApplyBusShardIndex`/`ApplyBusShardCount`) + per-shard `bridges` и `deliverFromCluster`-фильтр в [`keeper/internal/applybus/bus.go`](https://github.com/souls-guild/soul-stack/blob/main/keeper/internal/applybus/bus.go).

### Client examples

#### curl

```bash
APPLY_ID=$(curl -sS -X POST https://keeper.example.com/mcp \
  -H "Authorization: Bearer ${KEEPER_JWT}" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{
    "name":"keeper.incarnation.run",
    "arguments":{"name":"prod-app","scenario":"deploy","input":{"version":"v1.2.3"}}
  }}' | jq -r '.result.structuredContent._apply_id')

curl -N -sS https://keeper.example.com/mcp/events?apply_id=${APPLY_ID} \
  -H "Authorization: Bearer ${KEEPER_JWT}" \
  -H "Accept: text/event-stream"
```

#### JavaScript (EventSource)

Стандартный `EventSource` не поддерживает кастомные headers — используйте [`@microsoft/fetch-event-source`](https://github.com/Azure/fetch-event-source) (или аналог) для прокидывания `Authorization`:

```javascript
import { fetchEventSource } from '@microsoft/fetch-event-source';

await fetchEventSource(`/mcp/events?apply_id=${applyId}`, {
  headers: { Authorization: `Bearer ${jwt}` },
  onmessage(ev) {
    const payload = JSON.parse(ev.data);
    switch (ev.event) {
      case 'task.executed':
        console.log(`[${payload.task_idx}] ${payload.task_status}`,
                    payload.error ?? '');
        break;
      case 'apply.completed':
        console.log('done', payload.state_changes);
        break;
      case 'apply.failed':
      case 'apply.cancelled':
        console.error(ev.event, payload.run_status);
        break;
    }
  },
  onerror(err) { throw err; },
});
```

## См. также

- [operator-api.md](operator-api.md) — источник правды по семантике, request/response schemas, error codes.
- [rbac.md → Каталог permissions](rbac.md#каталог-permissions) и [rbac.md → Permission ↔ MCP-tool / OpenAPI endpoint](rbac.md#permission--mcp-tool--openapi-endpoint) — каталог permissions и 1:1 mapping.
- [config.md → `listen.mcp.addr`](config.md#listen) — bind-адрес MCP-listener-а. [config.md → `auth`](config.md#auth) — JWT-подпись.
- [`../architecture.md → ADR-013`](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) — Bootstrap первого Архонта (вне MCP).
- [`../architecture.md → ADR-014`](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) — identity-модель оператора, JWT-claims.
- [`../requirements.md`](../requirements.md) — «встроенный MCP» как сквозное требование.
- [MCP spec](https://spec.modelcontextprotocol.io/) — детали transport / handshake / session lifecycle.
