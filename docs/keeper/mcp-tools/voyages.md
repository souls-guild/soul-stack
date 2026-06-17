# Voyage — MCP-tools унифицированного батчевого прогона

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.voyage.*` (батч N инкарнаций по scenario / N хостов по command, [ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Транспорт, auth, формат tool declaration, async-convention, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Voyage](../operator-api/voyages.md).

### Voyage (4)

Четыре tool-а 1:1 с REST `POST /v1/voyages` + `GET /v1/voyages{,/{id}}` + `DELETE /v1/voyages/{id}` ([operator-api/voyages.md](../operator-api/voyages.md)). `POST /v1/voyages/preview` — **REST-only** (MCP-tool нет). Permission — **RBAC-by-kind** на create/cancel (`scenario`→`incarnation.run`, `command`→`errand.run`, security-критичный fail-closed guard внутри handler-а), `incarnation.history` на read. Tools доступны только при поднятом VoyageWorker-стеке; иначе `internal-error` («voyage orchestrator is not configured»).

#### `keeper.voyage.start`

Создаёт Voyage — унифицированный батчевый прогон. `kind=scenario`: применить named scenario к набору ИНКАРНАЦИЙ (target `incarnations[]` ∪ `service`/`coven`; per-incarnation state-commit). `kind=command`: выполнить whitelisted-модуль на наборе ХОСТОВ (target `sids`/`coven`/`where`, AND-merge; state не трогается). Permission: **RBAC-by-kind**. Endpoint: [`POST /v1/voyages`](../operator-api/voyages.md#post-v1voyages--создать-voyage). Async: **да** (202 + `voyage_id`; прогресс — polling `keeper.voyage.get`).

**Input** (`required: kind, target`):

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `kind` | `string` (enum `scenario`/`command`) | yes | Тип прогона. |
| `scenario_name` | `string` | для scenario | Имя сценария (обязательно для `kind=scenario`). |
| `module` | `string` | для command | Адрес whitelisted-модуля Soul-side (обязательно для `kind=command`). |
| `input` | `object` | optional | Параметры прогона (**НЕ логируются** в audit). |
| `target` | `object` | yes | Таргет: `incarnations[]` / `service` / `sids[]` / `where` (CEL, ≤4 KiB) / `coven[]`. scenario использует `incarnations`/`service`/`coven` (any-of env-тег); command — `sids`/`coven` (AND) / `where`. |
| `batch` | `string` | optional | Размер батча `N` единиц либо `N%` (1..100) от scope. Взаимоисключающе с `batch_size` (смешение → `voyage_batch_spec_conflict`). |
| `max_failures` | `string` | optional | Порог провалов `N` абсолют либо `N%` от единиц прогона. Взаимоисключающе с `fail_threshold`. |
| `batch_size` | `integer` (≥1) | optional | **DEPRECATED** (используйте `batch`). Размер Leg; `null` → весь прогон одним Leg. |
| `concurrency` | `integer` (1..500) | optional | `0`/отсутствует → default `50`. |
| `dry_run` | `boolean` | optional | Dry-run прогона. |
| `schedule_at` | `string` (date-time) | optional | Отложенный старт → `status=scheduled`. |
| `inter_batch_interval_ms` | `integer` (≥0) | optional | Пауза между Leg-ами (мс). |
| `on_failure` | `string` (enum `abort`/`continue`) | optional | Поведение при провале Leg. |

**Output** (`required: voyage_id, kind, scope_size, status, location`):

| Поле | Тип | Смысл |
|---|---|---|
| `voyage_id` | `string` (ULID) | ID Voyage. |
| `kind` | `string` (enum) | Эхо input. |
| `scope_size` | `integer` | Число резолвнутых единиц. |
| `status` | `string` (enum `pending`/`scheduled`) | `scheduled` при заданном `schedule_at`, иначе `pending`. |
| `location` | `string` | REST path для get/poll (`/v1/voyages/<id>`). |

Ошибки (как у REST): `forbidden` (RBAC-by-kind deny / явный чужой хост в command / инкарнация вне scope в scenario), `not-found` (явная инкарнация не существует, scenario), `validation-failed` (невалидный `kind` / пустой `scenario_name`/`module` по kind / нет target / битый SID/coven/имя / `where` > 4 KiB / пустой резолв `voyage_empty_target` / scope > `voyage.max_scope` / batch-spec conflict), `tempo-exceeded` (rate-limit bucket `voyage_create`). См. гибрид-семантику command∩Purview — [operator-api/voyages.md](../operator-api/voyages.md#command--purview--security-fix-с-изменением-поведения).

#### `keeper.voyage.get`

Читает snapshot Voyage по ULID (detail + summary). Permission: `incarnation.history`. Endpoint: [`GET /v1/voyages/{id}`](../operator-api/voyages.md#post-v1voyages--создать-voyage). Async: нет.

**Input:** `{voyage_id}` (ULID, required).

**Output `VoyageView`** (`required: voyage_id, kind, status, scope_size, total_batches, current_batch_index, started_by_aid, created_at`):

| Поле | Тип | Смысл |
|---|---|---|
| `voyage_id` | `string` (ULID) | PK. |
| `kind` | `string` | `scenario` / `command`. |
| `status` | `string` | `scheduled`/`pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`. |
| `scope_size` | `integer` | Размер scope. |
| `total_batches` | `integer` | Число Leg-ов. |
| `current_batch_index` | `integer` | Текущий Leg. |
| `started_by_aid` | `string` | FK на `operators(aid)`. |
| `created_at` | `string` (date-time) | Когда вставлен. |

Ошибки: `not-found` (`voyage_id` не существует).

#### `keeper.voyage.list`

Перечисление Voyage-прогонов с фильтрами `kind`/`status` (multi-value) и pagination (sort `created_at` DESC). Permission: `incarnation.history`. Endpoint: [`GET /v1/voyages`](../operator-api/voyages.md#post-v1voyages--создать-voyage). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `kind` | `string` (enum) | optional | Фильтр по типу. |
| `status` | `array<string>` (enum) | optional | Multi-value фильтр по статусу. |
| `offset` | `integer` (≥0) | optional | Pagination. |
| `limit` | `integer` (1..1000) | optional | Pagination. |

**Output:** `{items: array<object>, offset, limit, total}` (элементы — форма `VoyageView`).

#### `keeper.voyage.cancel`

Отменяет Voyage (`pending`/`scheduled` → `cancelled`). Running-abort — post-MVP. Permission: **RBAC-by-kind** (как `keeper.voyage.start`). Endpoint: [`DELETE /v1/voyages/{id}`](../operator-api/voyages.md#post-v1voyages--создать-voyage). Async: нет.

**Input:** `{voyage_id}` (ULID, required).

**Output:** `{voyage_id, status}`, где `status` = `cancelled`.

Ошибки: `not-found` (`voyage_id` не существует), `errand-not-cancellable` (Voyage уже `running`/в терминальном статусе — отменять нечего).
