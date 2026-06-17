# Tiding — MCP-tools реестра правил подписки на уведомления

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.tiding.*` (CRUD реестра `tidings`, [ADR-052](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов), S4). Транспорт, auth, формат tool declaration, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике, телам, кодам ошибок — [operator-api/tidings.md](../operator-api/tidings.md).

### Tiding (5)

5 tool-ов 1:1 `keeper.tiding.<verb>` ↔ permission `tiding.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/tidings*` (selector — NoSelector). `event_types` — area-glob (`scenario_run.*`) в scope прогонов; `herald` — FK на существующий Herald. Те же `HeraldSvc` / nil-guard, что у herald-tools; при выключенном реестре — `internal-error`.

#### `keeper.tiding.create`

Создаёт ПОСТОЯННОЕ Tiding-правило подписки: на какие `event_types` (area-glob `scenario_run.*` в scope прогонов — `scenario_run` / `command_run` / `voyage` / `cadence` + точечные `incarnation.drift_checked` и `incarnation.run_completed`) реагировать → каким Herald-ом доставлять. Фильтры `only_failures` / `only_changes`, опц. селекторы `incarnation` / `cadence` / `task`, управление телом доставки `annotations` / `projection` ([ADR-052(h)](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)). Permission: `tiding.create`. Endpoint: [`POST /v1/tidings`](../operator-api/tidings.md#post-v1tidings--создать-tiding). Async: нет.

`task` (string|null, [ADR-052 §l](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)) — опц. селектор подписки на конкретную задачу по адресу `register ∪ id`. Непустой `task` сужает матч только до `incarnation.run_completed`, в `changed_tasks` которого есть запись с `register == task` ИЛИ `id == task` (см. [operator-api/tidings.md → «Селектор task»](../operator-api/tidings.md#селектор-task--подписка-на-изменение-конкретной-задачи)). Для срабатывания `task`-правила в `event_types` нужен `incarnation.run_completed`.

**Input** (`required: name, herald, event_types`): `{name (^[a-z0-9-]{1,63}$), herald (FK на Herald), event_types (array<string>, area-glob), only_failures?, only_changes?, incarnation? (|null), cadence? (|null), task? (|null), annotations? (object), projection? (array<string>), enabled? (опущено → true)}`. Поля `ephemeral`/`voyage_id` — серверные ([ADR-052(g)](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)): на вход не принимаются, разовое правило материализует keeper из notify-блока Voyage.

**Output:** `Tiding` — `{name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, annotations, projection, ephemeral, voyage_id, enabled, created_at, updated_at, created_by_aid}` (`ephemeral`/`voyage_id` — read-only; у постоянного правила `ephemeral=false`, `voyage_id=null`). Ошибки: `tiding-already-exists` (`name` занят), `not-found` (`herald` не существует), `validation-failed` (битый `name`/`event_types`, произвольный wildcard, `annotations` не-объект, битый путь `projection`).

#### `keeper.tiding.update`

Заменяет mutable-поля Tiding-правила (replace-семантика; `name` — ключ). Permission: `tiding.update`. Endpoint: [`PUT /v1/tidings/{name}`](../operator-api/tidings.md#put-v1tidingsname--заменить-правило-replace-семантика). Async: нет.

**Input** (`required: name, herald, event_types`): `{name, herald, event_types, only_failures?, only_changes?, incarnation? (|null), cadence? (|null), task? (|null), annotations? (object), projection? (array<string>), enabled?}` (replace: опущенные `incarnation`/`cadence`/`task`/`annotations`/`projection` очищаются — omit==clear; `ephemeral`/`voyage_id` на вход не принимаются — серверные). **Output:** `Tiding`. Ошибки: `not-found` (правила нет или `herald` по FK не существует).

#### `keeper.tiding.delete`

Удаляет Tiding-правило по имени. Permission: `tiding.delete`. Endpoint: [`DELETE /v1/tidings/{name}`](../operator-api/tidings.md#delete-v1tidingsname--удалить-правило). Async: нет.

**Input:** `{name}`. **Output:** пустой объект (REST-эквивалент — 204). Ошибки: `not-found`.

#### `keeper.tiding.list`

Перечисление Tiding-правил (sort `updated_at` DESC, `name` ASC). Permission: `tiding.list`. Endpoint: [`GET /v1/tidings`](../operator-api/tidings.md#get-v1tidings--список-tiding-правил). Async: нет.

**Input:** `{offset?, limit?}`. **Output:** `{items: array<Tiding>, offset, limit, total}`.

#### `keeper.tiding.read`

Читает одно Tiding-правило по имени. Permission: `tiding.read`. Endpoint: [`GET /v1/tidings/{name}`](../operator-api/tidings.md#get-v1tidingsname--прочитать-одно-правило). Async: нет.

**Input:** `{name}`. **Output:** `Tiding`. Ошибки: `not-found`.
