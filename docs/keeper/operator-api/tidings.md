# Tiding — endpoints реестра правил подписки на уведомления

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/tidings*` — CRUD реестра `tidings` (правила подписки на события прогонов, [ADR-052](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов), S4). Tiding отвечает на вопрос «на что реагировать → каким Herald-ом доставлять»; парный канал доставки — [heralds.md](heralds.md). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/tidings.md](../mcp-tools/tidings.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 5 роутов) — в корневом [operator-api.md → Tiding (5)](../operator-api.md#tiding-5--реестр-правил-подписки-на-уведомления-adr-052). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`TidingCreateRequest` / `TidingUpdateRequest` / `Tiding` / `TidingListReply` — **источник правды по форме**). `tiding.*` — NoSelector. Роуты монтируются только при сконфигурированном реестре (тот же nil-guard, что у Herald).

### Модель матча

Dispatcher на каждое успешно записанное audit-событие прогона матчит включённые Tiding-правила по:

- **`event_types`** — непустой список audit-event-types с **area-glob** (`scenario_run.*`) в scope прогонов: `scenario_run.*` / `command_run.*` / `voyage.*` / `cadence.*` + точечные `incarnation.drift_checked` и `incarnation.run_completed`. Произвольный wildcard (`*`, `foo.*.bar`) запрещён → `422`.
- **фильтры** `only_failures` / `only_changes` (bool);
- **опц. селекторы** `incarnation` / `cadence` / `task` (nullable) — привязка к источнику прогона. См. отдельную секцию [«Селектор `task`»](#селектор-task--подписка-на-изменение-конкретной-задачи) ниже.

На каждый матч ставится задание на доставку через `herald` (FK на `heralds.name`).

### Селектор `task` — подписка на изменение конкретной задачи

`task` (string|null, [ADR-052 §l](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)) — опц. селектор подписки на **конкретную задачу прогона** по её адресу. `null` — без фильтра.

- **Адрес** — значение из адресного пространства `register ∪ id` задачи (то же, что грамматические поля `register`/`id` задачи в DSL). Это **селектор правила** (на что подписан Tiding), а не само поле задачи.
- **Матч** — непустой `task` сужает срабатывание **только** до события `incarnation.run_completed` (per-incarnation итог прогона, несёт `changed_tasks`). Правило матчит, если в `changed_tasks` события есть запись с `register == task` ИЛИ `id == task`. Любой другой `event_type` (без `changed_tasks`) с заданным `task` не матчится.
- **Семантика «изменилась»** — присутствие адреса в `changed_tasks` уже означает, что задача изменилась хотя бы на одном хосте ([ADR-052 §j](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)); отдельный `only_changes` для этого не нужен. Комбинация `task` + `only_changes` остаётся консистентной (матчевое событие не отсеивается).

Чтобы `task`-правило вообще получало события, в `event_types` должен быть `incarnation.run_completed`. Событие `incarnation.run_completed` подробно описано в [naming-rules.md → Audit-events](../../naming-rules.md) (per-incarnation итог, `status` ∈ `success`/`failed`, несёт `changed_tasks` + опц. `cadence_id`).

Селектор `cadence` тоже ловит `incarnation.run_completed`, когда прогон спавнен Cadence-расписанием (событие несёт `cadence_id`) — так постоянное Tiding с `cadence`-селектором подписывается на результаты прогонов расписания, а не только на сами `cadence.*`-события.

### Управление телом доставки (`annotations` / `projection`)

Оператор-задаваемые поля, формирующие тело webhook-доставки ([ADR-052(h)](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)). Доступны в `Create`/`Update` и присутствуют в ответе `Tiding`:

- **`annotations`** (object, опц.) — статические поля оператора (JSON-объект верхнего уровня), мержатся в тело ключом `annotations`. Не-объект на верхнем уровне (массив/скаляр) → `422`.
- **`projection`** (array<string>, опц.) — allow-list путей payload: какие поля события попадут в тело; пусто/опущено — полная форма. Каждый путь — сегменты `[a-z0-9_]` через `.` (`summary.succeeded`); пустой сегмент (ведущая/двойная/хвостовая точка) → `422`.

### Серверные поля разовых правил (`ephemeral` / `voyage_id`)

Поля разовой подписки, привязанной к одному прогону ([ADR-052(g)](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)). **Серверные, read-only** — присутствуют только в ответе `Tiding`, в `Create`/`Update` их нет:

- **`ephemeral`** (bool) — разовое правило. Оператор напрямую ephemeral-Tiding **не создаёт**: их материализует keeper из notify-блока [`POST /v1/voyages`](voyages.md) (`VoyageNotify`) в той же транзакции, что создаёт Voyage. Инвариант `ephemeral=true ⟺ voyage_id != null`.
- **`voyage_id`** (string|null) — ID Voyage привязки разового правила; у постоянного — `null`.

Постоянное правило (созданное через `POST /v1/tidings`) всегда `ephemeral=false`, `voyage_id=null`. Очистка осиротевших ephemeral-правил — фоном Reaper-правилом [`purge_orphan_ephemeral_tidings`](../reaper.md#правила) после терминала прогона.

### Постоянное правило из формы расписания (origin-маркер Cadence)

Постоянный Tiding может быть создан не только напрямую через `POST /v1/tidings`, но и **из блока `notify[]` формы расписания** ([`POST /v1/cadences`](cadences.md#уведомления-notify--постоянные-tiding-из-формы-расписания), [ADR-052 §m](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)). Такие правила несут внутренний origin-маркер `created_from_cadence_id` (= ULID породившего расписания) — он **не отдаётся в API-ответе** `Tiding` (серверное поле, не контракт), но определяет каскад: `DELETE /v1/cadences/{id}` атомарно сносит порождённые формой правила (FK `ON DELETE CASCADE`, [ADR-046 §9](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). Маркер происхождения ортогонален фильтр-селектору `cadence`: вручную заведённое правило с тем же `cadence`-селектором (но без origin-маркера) каскадом **не удаляется** и живёт независимо.

### `POST /v1/tidings` — создать Tiding

Permission: `tiding.create`. MCP-tool: `keeper.tiding.create`.

**Request `TidingCreateRequest`** (`required: name, herald, event_types`): `{name (^[a-z0-9-]{1,63}$), herald (имя Herald-канала, FK), event_types (array<string>, непустой, area-glob), only_failures? (bool), only_changes? (bool), incarnation? (string|null), cadence? (string|null), task? (string|null — адрес register∪id, см. «Селектор task»), annotations? (object), projection? (array<string>), enabled? (bool, опущено → true)}`. Полей `ephemeral`/`voyage_id` в запросе нет — серверные (см. выше).

**Response `201 Tiding`:** `{name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, annotations, projection, ephemeral, voyage_id, enabled, created_at, updated_at, created_by_aid}`.

Ошибки: `400` (битый JSON / unknown-поле), `404 not-found` (`herald` по FK не существует), `409` (`name` занят), `422 validation-failed` (битый `name`/`event_types`, произвольный wildcard, `annotations` не-объект, битый путь `projection`). Audit: `tiding.created`.

### `GET /v1/tidings` — список Tiding-правил

Permission: `tiding.list`. MCP-tool: `keeper.tiding.list`. Query `offset`/`limit`. Sort `updated_at` DESC, `name` ASC. Response `200 TidingListReply` (`{items, offset, limit, total}`).

### `GET /v1/tidings/{name}` — прочитать одно правило

Permission: `tiding.read`. MCP-tool: `keeper.tiding.read`. Response `200 Tiding`; `404 not-found` — записи нет.

### `PUT /v1/tidings/{name}` — заменить правило (replace-семантика)

Permission: `tiding.update`. MCP-tool: `keeper.tiding.update`. **Replace** — тело полностью заменяет mutable-поля; `name` (PK) immutable. Как у Push-Provider/Herald — `PUT` (полная замена), не `PATCH`.

**Request `TidingUpdateRequest`** (`required: herald, event_types`): `{herald, event_types (array<string>), only_failures?, only_changes?, incarnation? (|null), cadence? (|null), task? (|null), annotations? (object), projection? (array<string>), enabled?}`. Replace-семантика: опущенные `incarnation`/`cadence`/`task`/`annotations`/`projection` очищаются (omit==clear — FE шлёт правило целиком, а не дельту). Полей `ephemeral`/`voyage_id` в запросе нет — серверные.

**Response `200 Tiding`.** Ошибки: `400`, `404 not-found` (правила нет или `herald` по FK не существует), `422 validation-failed`. Audit: `tiding.updated`.

### `DELETE /v1/tidings/{name}` — удалить правило

Permission: `tiding.delete`. MCP-tool: `keeper.tiding.delete`. Response `204`; `404 not-found`. Audit: `tiding.deleted`. (Снос Herald-канала каскадно уносит его Tiding-подписки — обратной каскад-зависимости у Tiding нет.)

## См. также

- [heralds.md](heralds.md) — парный реестр каналов доставки (куда слать; webhook, SSRF-guard, подпись).
- [mcp-tools/tidings.md](../mcp-tools/tidings.md) — MCP-сторона (`keeper.tiding.*`).
- [ADR-052](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов) — дизайн Herald/Tiding (tap поверх audit-writer, матч Tiding-правил, scope = только события прогонов).
- [rbac.md](../rbac.md) — каталог permissions `tiding.*`.
