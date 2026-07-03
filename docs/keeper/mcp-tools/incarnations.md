# Incarnation — MCP-tools жизненного цикла runtime-инстансов

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.incarnation.*` (создание / прогон сценариев / чтение / unlock / upgrade / drift / destroy / traits-set). Транспорт, auth, формат tool declaration, async-convention `_apply_id`, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Incarnation](../operator-api/incarnations.md).

### Incarnation (11)

#### `keeper.incarnation.create`

Создание instance — запуск выбранного стартового сценария (либо bare-инкарнация, если сервис без create-сценариев). Permission: `incarnation.create`. Endpoint: [`POST /v1/incarnations`](../operator-api/incarnations.md#post-v1incarnations--создать-instance). Async: **да** (для bare — sync, без прогона).

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Имя нового instance. |
| `service` | `string` | yes | Имя сервиса. |
| `covens` | `array<string>` | optional | Declared env-Coven-метки ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) amendment a). |
| `traits` | `object` | optional | Operator-set trait-метки инкарнации (ключ → `scalar`\|`list of scalars`, [ADR-060](../../adr/0060-traits.md)). Кладутся в `incarnation.traits` + проекция в `souls.traits` хостов-членов. Day-2 замена — `keeper.incarnation.traits-set`. |
| `create_scenario` | `string` | conditional | Имя стартового сценария (scenario с `create: true`). Required, если сервис предлагает ≥1 create-сценарий (пусто → `validation-failed` со списком годных); значение вне набора → `validation-failed`. Сервис без create-сценариев → пусто даёт bare-инкарнацию. Подробности — [operator-api/incarnations.md → Выбор стартового сценария](../operator-api/incarnations.md#выбор-стартового-сценария-и-bare-инкарнация). |
| `input` | `object` | optional | Input выбранного стартового сценария (валидируется против его `input:`-схемы). |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска. |
| `incarnation` | `string` | Имя созданного instance. |

#### `keeper.incarnation.rerun-last`

Перезапуск **последнего упавшего** сценария из `error_locked`: зеркало REST [`POST /v1/incarnations/{name}/rerun-last`](../operator-api/incarnations.md#post-v1incarnationsnamererun-last--перезапустить-последний-упавший-сценарий-из-error_locked). Permission: `incarnation.rerun-last`. Async: **да**.

Под одним `FOR UPDATE` снимает блок (`state` НЕ трогается — last known-good, snapshot в `state_history`) и тем же действием перезапускает **последний упавший сценарий** инкарнации — bootstrap (`create`/…, если провалилось создание) ИЛИ day-2-операцию (`add_user`/…) — с сохранённым input упавшего прогона (`error_locked → applying` минуя `ready`). Input восстанавливается из `incarnation.spec.input` (create-путь) либо из рецепта упавшего прогона (`apply_runs.recipe.input`, day-2-путь), не из дефолтов. Отличие от `keeper.incarnation.unlock`: тот лишь снимает блок, rerun снимает и перезапускает упавший сценарий одним действием. Работает только из `error_locked`; статус не `error_locked` → `incarnation-locked`, input упавшего прогона недоступен (прогон упал до dispatch и рецепт не записан / рецепт вычищен ретеншном / legacy-прогон, fail-closed) → отдельный код `rerun-input-unavailable` ([mcp-tools.md → Errors](../mcp-tools.md#errors)). Опрос статуса — `keeper.incarnation.get`. Audit-событие — `incarnation.rerun_last` (НЕ `incarnation.unlocked`).

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `reason` | `string` (1..500 символов) | yes | Свободный текст для audit-trail (payload `incarnation.rerun_last`). |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID перезапущенного прогона. |
| `incarnation` | `string` | Имя instance. |
| `scenario` | `string` | Имя перезапущенного (последнего упавшего) сценария — bootstrap `create`/… или day-2 `add_user`/…. |

Ошибки: `not-found` (incarnation не существует), `incarnation-locked` (статус не `error_locked`), `rerun-input-unavailable` (input упавшего day-2-прогона недоступен — прогон упал до dispatch и рецепт не записан / рецепт вычищен ретеншном / legacy-прогон без рецепта), `validation-failed` (пустой `reason` / `reason` длиннее 500 символов / битый `name` / сервис инкарнации не зарегистрирован), `internal-error` (runner не сконфигурирован / транзакция / запуск).

#### `keeper.incarnation.run`

Запуск произвольного сценария над existing instance. Permission: `incarnation.run`. Endpoint: [`POST /v1/incarnations/{name}/scenarios/{scenario}`](../operator-api/incarnations.md#post-v1incarnationsnamescenariosscenario--запустить-произвольный-сценарий). Async: **да**.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `scenario` | `string` | yes | Имя сценария из `scenario/<name>/`. |
| `input` | `object` | optional | Input сценария. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска. |
| `incarnation` | `string` | Имя instance. |
| `scenario` | `string` | Имя сценария. |

#### `keeper.incarnation.get`

Чтение spec + state + status. Permission: `incarnation.get`. Endpoint: [`GET /v1/incarnations/{name}`](../operator-api/incarnations.md#get-v1incarnationsname--прочитать-spec--state--status). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |

**Output:** schema `IncarnationGetReply` — поля `name`, `service`, `service_version`, `state_schema_version`, `spec` (object), `state` (object), `status` (enum), `status_details` (object\|null), `created_by_aid`, `created_at`, `updated_at`. Подробно — [operator-api.md → IncarnationGetReply](../operator-api/incarnations.md#get-v1incarnationsname--прочитать-spec--state--status).

#### `keeper.incarnation.list`

Перечисление instance-ов. Permission: `incarnation.list`. Endpoint: [`GET /v1/incarnations`](../operator-api/incarnations.md#get-v1incarnations--список-instance-ов). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `service` | `string` | optional | Фильтр по сервису. |
| `status` | `string` (enum) | optional | Фильтр по статусу: `provisioning` / `ready` / `applying` / `error_locked` / `migration_failed` / `drift` / `destroying`. |
| `offset` | `integer` (≥0) | optional | Default `0`. |
| `limit` | `integer` (1..1000) | optional | Default `50`. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `items` | `array<IncarnationGetReply>` | Элементы — та же форма, что в `keeper.incarnation.get`. |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.incarnation.history`

Журнал `state_history`. Permission: `incarnation.history`. Endpoint: [`GET /v1/incarnations/{name}/history`](../operator-api/incarnations.md#get-v1incarnationsnamehistory--журнал-изменений-state). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `offset` | `integer` | optional | Default `0`. |
| `limit` | `integer` | optional | Default `50`. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `items` | `array<StateHistoryEntry>` | Элементы — `{history_id, scenario, state_before, state_after, changed_by_aid, apply_id, at}`. |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.incarnation.unlock`

Снятие `error_locked`. Permission: `incarnation.unlock`. Endpoint: [`POST /v1/incarnations/{name}/unlock`](../operator-api/incarnations.md#post-v1incarnationsnameunlock--снять-error_locked). Async: нет.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `reason` | `string` (1..500 символов) | yes | Свободный текст для audit. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `name` | `string` | Имя instance. |
| `previous_status` | `string` (enum) | Обычно `error_locked`. |
| `status` | `string` (enum) | Обычно `ready`. |
| `unlocked_by_aid` | `string` | AID, выполнивший unlock. |
| `unlocked_at` | `string` (RFC 3339) | Время. |

#### `keeper.incarnation.upgrade`

Перевод на новую `state_schema_version` + смена `service_version`. Permission: `incarnation.upgrade`. Endpoint: [`POST /v1/incarnations/{name}/upgrade`](../operator-api/incarnations.md#post-v1incarnationsnameupgrade--перевод-на-новую-state_schema_version). Async: **да**.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `to_version` | `string` (git-ref сервиса) | yes | Целевая версия сервиса. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска миграции. |

#### `keeper.incarnation.check-drift`

Scry on-demand-проверка drift ([ADR-031](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)). Permission: `incarnation.check-drift`. Endpoint: [`POST /v1/incarnations/{name}/check-drift`](../operator-api/incarnations.md#post-v1incarnationsnamecheck-drift--scry-проверка-drift). Async: **нет** (sync — handler блокируется до сборки `DriftReport`).

Keeper рендерит `scenario/converge/main.yml` сервиса и шлёт всем хостам `ApplyRequest{dry_run:true}` через work-queue (Acolyte). Soul зовёт `mod.Plan` вместо `mod.Apply` (pure-read), собирает per-host per-task `changed` и возвращает `DriftReport`. converge-input резолвится автоматически по конвенции имени из `incarnation.state.<param>` + opt-override оператора.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `input` | `object` | optional | Override converge-параметров. Имена/типы совпадают со схемой `input:` в `scenario/converge/main.yml` сервиса. |

**Output `DriftReport`:** см. `DriftReport`-схему в [openapi.yaml](../openapi.yaml).

| Поле | Тип | Смысл |
|---|---|---|
| `checked_at` | `string` (RFC 3339) | Время сборки отчёта. |
| `incarnation` | `string` | Имя проверенного instance. |
| `scenario_ref` | `string` | Имя сценария Scry — всегда `converge`. |
| `hosts` | `array<DriftHostReport>` | Per-host агрегаты (`{sid, status, tasks}`). status ∈ `clean`/`drifted`/`unsupported`/`failed`. |
| `summary` | `DriftSummary` | Агрегаты: `{hosts_drifted, hosts_clean, hosts_unsupported, hosts_failed}`. |

**Errors:** `validation-failed` (converge отсутствует в service-snapshot-е — «drift-проверка недоступна», информационно; либо drift-input не резолвится), `not-found` (incarnation), `internal-error` (drift-checker не сконфигурирован — единственный inline-режим acolytes=0).

#### `keeper.incarnation.destroy`

Снос instance. Permission: `incarnation.destroy`. Endpoint: [`DELETE /v1/incarnations/{name}`](../operator-api/incarnations.md#delete-v1incarnationsname--удалить-instance). Async: **да**. Operator-facing `allow_destroy` маппится в internal `force` (унификация force↔allow_destroy): `false` — destroy через teardown-сценарий `destroy`; `true` — снос без teardown.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `allow_destroy` | `boolean` | yes | Обязательный confirmation flag (маппится в internal `force`). `false` — destroy через teardown-сценарий `destroy`; нет сценария `destroy` в снапшоте сервиса → `validation-failed`. `true` — снос без teardown (force, DELETE строки напрямую). |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска. |

#### `keeper.incarnation.traits-set`

Целостная замена operator-set trait-меток инкарнации. Permission: `incarnation.traits-set`. Endpoint: [`PUT /v1/incarnations/{name}/traits`](../operator-api/incarnations.md#put-v1incarnationsnametraits--заменить-trait-метки-инкарнации). Async: **нет** (sync — replace + проекция в `souls.traits`, ответом компактная сводка).

Заменяет `incarnation.traits` (jsonb — источник истины, [ADR-060](../../adr/0060-traits.md) R1 slice a) целиком: пустой/опущенный `traits` = очистить метки. Одной tx `FOR UPDATE`, затем sync-hook материализованно проецирует набор в `souls.traits` хостов-членов инкарнации. RBAC — body-scoped OR-Check по coven/service-scope инкарнации (`covens ∪ {name}`, зеркало REST). Заменяет per-soul [`keeper.soul.traits-assign`](souls.md) (deprecated). Audit-событие — `incarnation.traits_changed` (только trait-**КЛЮЧИ**, не значения).

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `traits` | `object` | optional | Полный набор trait-меток: ключ → `scalar` (`string`/`number`/`boolean`) ИЛИ `list of scalars`. Replace-семантика; пустой/опущен = очистить. Вложенный объект/массив → `validation-failed`. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `incarnation` | `string` | Имя instance. |
| `keys` | `array<string>` | Отсортированные trait-**КЛЮЧИ** после замены (значения НЕ эхуются — секрет-гигиена). |

**Errors:** `validation-failed` (битый `name` / невалидный ключ / вложенное trait-значение), `not-found` (incarnation не существует), `forbidden` (нет `incarnation.traits-set` в scope инкарнации), `internal-error`.
