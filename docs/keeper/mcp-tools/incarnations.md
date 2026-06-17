# Incarnation — MCP-tools жизненного цикла runtime-инстансов

Доменная секция [каталога MCP-tools](../mcp-tools.md): tools `keeper.incarnation.*` (создание / прогон сценариев / чтение / unlock / upgrade / drift / destroy). Транспорт, auth, формат tool declaration, async-convention `_apply_id`, error mapping — в корневом [mcp-tools.md](../mcp-tools.md). Источник правды по семантике — [operator-api.md → Incarnation](../operator-api/incarnations.md).

### Incarnation (9)

#### `keeper.incarnation.create`

Создание instance — запуск scenario `create`. Permission: `incarnation.create`. Endpoint: [`POST /v1/incarnations`](../operator-api/incarnations.md#post-v1incarnations--создать-instance). Async: **да**.

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Имя нового instance. |
| `service` | `string` | yes | Имя сервиса. |
| `input` | `object` | optional | Input scenario `create`. |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID запуска. |
| `incarnation` | `string` | Имя созданного instance. |

#### `keeper.incarnation.rerun-create`

Перезапуск scenario `create` из `error_locked`: зеркало REST [`POST /v1/incarnations/{name}/rerun-create`](../operator-api/incarnations.md#post-v1incarnationsnamererun-create--перезапустить-create-из-error_locked). Permission: `incarnation.create-rerun`. Async: **да**.

Под одним `FOR UPDATE` снимает блок (`state` НЕ трогается — last known-good, snapshot в `state_history`) и тем же действием перезапускает scenario `create` (`error_locked → applying` минуя `ready`). Отличие от `keeper.incarnation.unlock`: тот лишь снимает блок, rerun снимает и перезапускает bootstrap одним действием. Scope ЖЁСТКО ограничен сценарием `create` — если последний упавший сценарий не `create`, tool возвращает `incarnation-locked`. Опрос статуса — `keeper.incarnation.get`. Audit-событие — `incarnation.create_rerun` (НЕ `incarnation.unlocked`).

**Input:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Имя instance. |
| `reason` | `string` | yes | Свободный текст для audit-trail (payload `incarnation.create_rerun`). |

**Output:**

| Поле | Тип | Смысл |
|---|---|---|
| `_apply_id` | `string` (ULID) | ID перезапущенного прогона. |
| `incarnation` | `string` | Имя instance. |

Ошибки: `not-found` (incarnation не существует), `incarnation-locked` (статус не `error_locked` ИЛИ последний упавший сценарий не `create`), `validation-failed` (пустой `reason` / битый `name`), `internal-error` (runner не сконфигурирован / транзакция / запуск).

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
