# Incarnation — endpoints жизненного цикла runtime-инстансов

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/incarnations*` (создание / прогон сценариев / чтение / unlock / upgrade / drift / destroy, [ADR-009](../../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Conventions, error-format, pagination, secret-masking (вкл. маскинг `state`/`spec` в GET-ответах), mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/incarnations.md](../mcp-tools/incarnations.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 12 роутов) — в корневом [operator-api.md → Incarnation (12)](../operator-api.md#incarnation-12--жизненный-цикл-runtime-инстансов-adr-009).

#### `POST /v1/incarnations` — создать instance

Permission: `incarnation.create`. MCP-tool: `keeper.incarnation.create`.

Запускает выбранный стартовый (bootstrap) сценарий указанного сервиса; создаёт запись `incarnation` в Postgres ([architecture.md → Incarnation](../../architecture.md#incarnation--runtime-инстанс-сервиса)). Стартовый сценарий задаётся полем `create_scenario` (механизм нескольких create-сценариев — годен любой сценарий с top-level `create: true`, см. [§ Выбор стартового сценария и bare-инкарнация](#выбор-стартового-сценария-и-bare-инкарнация) ниже). Асинхронная операция (для bare-инкарнации — синхронная, без прогона).

**Request `IncarnationCreateRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | Имя нового instance, оно же корневая Coven-метка ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). |
| `service` | `string` | yes | Имя сервиса из `keeper.yml → services[].name` ([config.md → services](../config.md#services--default_destiny_source--default_module_source)). |
| `covens` | `list<string>` | optional | Declared environment-теги incarnation ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) amendment a). Формат каждой метки — `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` (как у Soul-меток). По умолчанию `[]`. Несут RBAC coven-scope incarnation-операций (см. ниже). |
| `traits` | `object` | optional | Operator-set key-value trait-метки инкарнации ([ADR-060](../../adr/0060-traits.md) R1 slice a): ключ → значение `scalar` ИЛИ `list of scalars` (`{"owner": "alice", "owners": ["alice", "bob"]}`). Кладутся в `incarnation.traits` (источник истины) и материализованно проецируются в `souls.traits` хостов-членов. Вложенный объект/массив-в-массиве → `422`. По умолчанию `{}` (нет меток). Day-2 замена — `PUT /v1/incarnations/{name}/traits`. |
| `create_scenario` | `string` | conditional | Имя стартового (bootstrap) сценария — сценария с top-level `create: true` в `scenario/<name>/main.yml` (механизм нескольких create-сценариев; имя `create` НЕ привилегировано, годность даёт только ключ `create: true`). Формат `^[a-z][a-z0-9_]*$`. **Required, если сервис предлагает ≥1 create-сценарий**: пустое поле → `422 validation-failed` с текстом-перечислением годных сценариев. Значение вне create-набора (operational-сценарий, напр. `add_user`, либо несуществующее имя) → `422 validation-failed`. **Для сервиса без create-сценариев поле игнорируется** — создаётся bare-инкарнация (см. ниже). Сохраняется в `incarnation.created_scenario`; `rerun-last` использует его на create-пути (когда последним упавшим был именно стартовый сценарий). |
| `input` | `object` | optional | Input для выбранного стартового сценария, валидируется против `scenario/<create_scenario>/input:`-схемы сервиса (НЕ обязательно `create`). Для bare-инкарнации не валидируется (прогона нет). По умолчанию `{}`. |

```json
{
  "name": "redis-prod",
  "service": "redis",
  "covens": ["prod", "dc-eu-west"],
  "input": {
    "spawn": {
      "provider": "aws-prod",
      "profile": "redis-medium-eu",
      "count": 3
    }
  }
}
```

**RBAC coven-scope (ADR-008 amendment a).** `covens` задаёт env-теги, по которым RBAC ограничивает incarnation-операции. Эффективный scope incarnation = `covens ∪ {name}` (имя — корневая Coven-метка). Роль `incarnation.* on coven=prod` получает доступ к incarnation-ам с `prod` в declared `covens` (или с именем `prod`); роль `incarnation.* on service=redis` — ко всем incarnation сервиса `redis` независимо от тегов. На **create** scope проверяется по `service` + declared `covens ∪ {name}` из тела: оператор со scope `coven=prod` не может создать incarnation с `covens=["dev"]` (получит `403 forbidden`) — это защита от privilege-escalation через тег вне своего scope. Подробности — [rbac.md → Грамматика селектора](../rbac.md#грамматика-селектора).

**Response `202 Accepted`:**

```json
{
  "apply_id": "01HABCDEFGHJKMNPQRSTVWXYZ",
  "incarnation": "redis-prod"
}
```

`apply_id` — ULID запуска (присутствует в OTel-трейсах, аудит-логе, `state_history.apply_id` после успешного коммита). Опрос статуса — `GET /v1/incarnations/redis-prod` (`status`/`status_details`) и `GET /v1/incarnations/redis-prod/history`.

**Errors:** `403 forbidden`, `409 incarnation-already-exists`, `422 service-not-registered`, `422 validation-failed`.

**Манифест `lifecycle.auto_create` ([architecture.md → Service](../../architecture.md#service--структура-и-manifest)).** Если `manifest.lifecycle.auto_create: false`, `POST /v1/incarnations` создаёт запись в `ready` **без** прогона стартового сценария — `apply_id` в ответе отсутствует, оператор запускает выбранный сценарий вручную из Run-формы. По умолчанию (`true`, backcompat) стартовый сценарий запускается сразу. Резолвится из снапшота развёрнутого service-ref-а на момент запроса. Это **не** bare-инкарнация: bootstrap-сценарий выбран (`created_scenario` непустое), просто прогон отложен.

##### Выбор стартового сценария и bare-инкарнация

Стартовый набор сервиса = **ровно** сценарии с top-level `create: true` в `scenario/<name>/main.yml` (auto-discover, [service/manifest.md → Стартовый сценарий](../../service/manifest.md#стартовый-сценарий--create-true)). Имя `create` НЕ привилегировано — оно попадает в набор только если сам `scenario/create/main.yml` несёт `create: true`. От значения `create_scenario` и состава этого набора зависят три ветви:

- **Сервис предлагает ≥1 create-сценарий + `create_scenario` непустое и в наборе** → запускается выбранный сценарий, `input` валидируется против ЕГО `input:`-схемы, `created_scenario` = выбранное имя. Async-прогон (`202` + `apply_id`).
- **Сервис предлагает ≥1 create-сценарий + `create_scenario` пустое** → `422 validation-failed` (`create_scenario_required`): выбор обязателен, потому что `input` валидируется против схемы КОНКРЕТНОГО сценария, и Keeper не угадывает который. `detail` перечисляет годные сценарии.
- **Сервис без единого create-сценария + `create_scenario` пустое** → **bare-инкарнация**: запись создаётся в `ready` **синхронно, без прогона**, `apply_id` в ответе отсутствует, `created_scenario` = `null`. Готова к day-2-операциям через `POST /v1/incarnations/{name}/scenarios/{scenario}`. Непустое `create_scenario` для такого сервиса → `422 validation-failed` (имя не в наборе).

Значение `create_scenario`, не входящее в стартовый набор (operational-сценарий вроде `add_user` или несуществующее имя), всегда → `422 validation-failed`, incarnation не создаётся (отказ на этапе модели). Невалидное по формату имя (`^[a-z][a-z0-9_]*$`, path-traversal guard) отбивается тем же `422` до резолва набора.

Пример (redis несёт три create-сценария — `create` / `create_from_souls` / `migrate_cluster`): чтобы поднять кластер с нуля, оператор передаёт `"create_scenario": "create"`; чтобы залить данные с внешнего кластера при создании — `"create_scenario": "migrate_cluster"`.

#### `POST /v1/incarnations/{name}/rerun-last` — перезапустить последний упавший сценарий из `error_locked`

Permission: `incarnation.rerun-last`. MCP-tool: `keeper.incarnation.rerun-last`. Path-param: `name`. OperationID: `rerunLastIncarnation`.

Атомарно снимает блок `error_locked` и **тем же действием** перезапускает **последний упавший сценарий** incarnation ([architecture.md → Атомарность и `error_locked`](../../architecture.md#атомарность-и-error_locked)) — это может быть как bootstrap-сценарий (`create`/…, если создание провалилось), так и любая day-2-операция (`add_user`, `restart`, …). Имя упавшего сценария читается под `FOR UPDATE` (create-путь — `incarnation.created_scenario`, day-2-путь — сценарий последнего провалившегося прогона). Под одним `FOR UPDATE`: `error_locked → applying` минуя `ready` (race-free), `state` НЕ трогается (last known-good сохраняется, snapshot перехода пишется в `state_history` с общим `apply_id`). Отличие от `unlock`: `unlock` только снимает блок (оператор сам решает, что делать дальше), а `rerun-last` снимает блок и перезапускает упавший сценарий одним подтверждённым действием. Асинхронная операция — `202` + `apply_id`, опрос статуса через `GET /v1/incarnations/{name}`.

**Восстановление input упавшего прогона.** Сценарий перезапускается с ТЕМИ ЖЕ входными значениями, что были у упавшего прогона (а не с дефолтами) — иначе rerun с required-полями (например redis-кластер: `version`/`shards`) упал бы на input-валидации либо применил дефолты. Источник input:

- **create-путь** — `incarnation.spec.input` (то, что задекларировал оператор при создании);
- **day-2-путь** — рецепт упавшего прогона `apply_runs.recipe.input` (читается по `apply_id` последнего snapshot-а того же `FOR UPDATE`; vault-refs хранятся строками, секреты не раскрыты).

Работает **только из статуса `error_locked`**. Запрос отвергается `409 incarnation-locked` (`detail` поясняет — нужен обычный `unlock` + ручной `run`) в двух случаях:

- **статус не `error_locked`** (нечего перезапускать — прогона в ошибке нет);
- **input упавшего прогона недоступен** (fail-closed): рецепт вычищен ретеншном Reaper (`purge_apply_runs`) либо это legacy-прогон без сохранённого рецепта (`recipe IS NULL`). Транзакция НЕ коммитится; оператор снимает блок обычным `unlock` и запускает нужный сценарий вручную с явным input.

Тот же `apply_id` идёт и в `state_history`-snapshot unlock-перехода, и в перезапускаемый прогон — снимок коррелирует с прогоном.

**Request `IncarnationRerunLastRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `reason` | `string` (1..500 символов) | yes | Свободный текст для audit-trail (пишется в payload audit-события `incarnation.rerun_last`). Подтверждение осознанности действия — UI требует confirm. |

```json
{ "reason": "fixed network ACL — retry failed scenario on redis-prod" }
```

**Response `202 Accepted`:** `{"apply_id": "<ULID>", "incarnation": "redis-prod", "scenario": "add_user"}` — `scenario` эхует имя перезапущенного (упавшего) сценария.

**Errors:** `403 forbidden` (нет `incarnation.rerun-last`), `404 not-found` (incarnation не существует), `409 incarnation-locked` (статус не `error_locked`; ИЛИ input упавшего прогона недоступен — рецепт вычищен ретеншном / legacy-прогон), `422 validation-failed` (пустой `reason` / `reason` длиннее 500 символов / невалидный path-`name`), `500 internal-error` (runner не сконфигурирован / транзакция / запуск прогона).

**RBAC:** scope тот же, что у `incarnation.run` / `incarnation.unlock` — `coven=`/`service=`/`incarnation=` (приземляется по path-`name`: declared `covens ∪ {name}` + `service` из строки incarnation).

**Audit:** `incarnation.rerun_last` (`source: api` / `mcp`, `correlation_id=apply_id`, payload `{name, reason, scenario, previous_status, apply_id}`) — пишется handler-ом после успешного unlock-перехода (`previous_status` известен только после него), НЕ переиспользует `incarnation.unlocked`.

#### `POST /v1/incarnations/{name}/scenarios/{scenario}` — запустить произвольный сценарий

Permission: `incarnation.run`. MCP-tool: `keeper.incarnation.run`. Path-params: `name`, `scenario`.

Запускает scenario `<scenario>` против existing incarnation. Асинхронная операция, ответ `202` + `apply_id`. Длинный path выбран сознательно — RESTful (scenario как sub-resource incarnation-а).

**Существование сценария — async-контракт.** Keeper синхронно проверяет только грамматику имени (`scenario.ScenarioNamePattern`), не его наличие: сценарии живут в git-репо сервиса (`scenario/<name>/main.yml`) и резолвятся только после git-load внутри прогона, не в registry. Поэтому **неизвестное-но-грамматически-валидное** имя сценария даёт `202 Accepted`, а прогон затем уходит в `error_locked` со `scenario_load_failed` в `status_details`. Это осознанный async-контракт, консистентный с `POST /v1/incarnations` (Create): оператор узнаёт результат через `GET /v1/incarnations/{name}` (`status: applying` → `ready` или `error_locked`), а не из синхронного `404`/`422`. Синхронный `422 validation-failed` возвращается только на имя, не прошедшее `ScenarioNamePattern` (path-traversal guard).

**Request:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `input` | `object` | optional | Input scenario, валидируется против `scenario/<scenario>/input:`. По умолчанию `{}`. |

> **Батч / invocation-time chunking — на `/v1/voyages`.** Прежние invocation-time поля `target` / `wave` / `concurrency` (Tide, [ADR-040](../../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) **удалены в Wave 5**. Этот эндпоинт — только single-incarnation scenario-run (без батча). Батч N инкарнаций — `POST /v1/voyages` с `kind=scenario` + `batch_size` / `concurrency` ([ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), см. [operator-api/voyages.md](voyages.md)).

```json
// single-incarnation scenario-run
{
  "input": { "username": "alice", "role": "readonly" }
}
```

**Response `202 Accepted`:**
- Classic single-run (без `wave`): `{"apply_id": "<ULID>", "incarnation": "redis-prod", "scenario": "add-user"}`.
- Батч (несколько инкарнаций) — отдельный эндпоинт `POST /v1/voyages` (`kind=scenario`): per-incarnation `apply_id` привязываются к Voyage через `voyage_targets.apply_id` (back-link живёт в таблице оркестратора, не в `apply_runs`). Прогресс — `GET /v1/voyages/{voyage_id}` ([ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)).

**Errors:** `403 forbidden`, `404 not-found` (incarnation не существует), `409 incarnation-locked`, `409 migration-failed`, `422 validation-failed` (имя сценария не прошло `ScenarioNamePattern`). Несуществующий-но-валидный сценарий — **не** ошибка этого эндпоинта: `202` → `error_locked` (см. async-контракт выше).

#### `GET /v1/incarnations/{name}` — прочитать spec + state + status

Permission: `incarnation.get`. MCP-tool: `keeper.incarnation.get`. Path-param: `name`.

**Response `200 IncarnationGetReply`:**

| Поле | Тип | Смысл |
|---|---|---|
| `name` | `string` | Имя instance. |
| `service` | `string` | Имя сервиса. |
| `service_version` | `string` (git-ref) | Пин-версия сервиса. |
| `state_schema_version` | `int` | Версия state_schema ([ADR-019](../../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). |
| `covens` | `list<string>` | Declared environment-теги ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) amendment a). Источник RBAC coven-scope (`covens ∪ {name}`). Всегда массив (пустой, если тегов нет). |
| `created_scenario` | `string` (optional) | Имя стартового (bootstrap) сценария, которым создана инкарнация (механизм нескольких create-сценариев). `rerun-last` использует его на create-пути (когда последним упавшим был именно стартовый сценарий). Для **bare-инкарнации** (создана без bootstrap-сценария) — `null`/опущен (поле с `omitempty`). |
| `spec` | `object` | jsonb — то, что задекларировал оператор ([architecture.md → Incarnation](../../architecture.md#incarnation--runtime-инстанс-сервиса)). Sensitive-значения замаскированы (`***MASKED***`, см. [§ Маскинг state/spec в GET-ответах](../operator-api.md#маскинг-state--spec-в-get-ответах-defense-in-depth)). |
| `state` | `object` | jsonb — текущая структурированная конфигурация. Sensitive-значения замаскированы (см. там же). |
| `status` | `enum` | `provisioning` / `ready` / `applying` / `error_locked` / `migration_failed` / `drift` / `destroying`. |
| `status_details` | `object` (nullable) | Детали ошибки, если `status` локирующий. |
| `created_by_aid` | `string` | FK на `operators(aid)`. |
| `created_at`, `updated_at` | `string` (RFC 3339) | Аудит. |
| `last_drift_check_at` | `string` (RFC 3339, optional) | [ADR-031](../../architecture.md) Slice C: время завершения последнего dry_run-прогона `converge` — фон (Reaper-правило `scry_background`) или on-demand (`POST /v1/incarnations/{name}/check-drift`, Slice B). Отсутствует, если incarnation ни разу не сканировалась. |
| `last_drift_summary` | `object` (optional) | [ADR-031](../../architecture.md) Slice C: counts-агрегат последнего DriftReport. Ключи: `hosts_drifted`, `hosts_clean`, `hosts_unsupported`, `hosts_failed`, `total_hosts`, `scanned_at` (RFC 3339). Counts-only — полный DriftReport в БД не хранится (Slice B возвращает его прямо в response check-drift). Отсутствует, если incarnation ни разу не сканировалась. |

#### `GET /v1/incarnations` — список instance-ов

Permission: `incarnation.list`. MCP-tool: `keeper.incarnation.list`.

**Query:** `offset`, `limit` ([§ Pagination](../operator-api.md#pagination)) + опциональные фильтры:

| Param | Тип | Смысл |
|---|---|---|
| `service` | `string` | Фильтр по имени сервиса. |
| `status` | `enum` (см. выше) | Фильтр по статусу. |

**Response `200`:** `{items: [IncarnationGetReply], offset, limit, total}` (элементы — та же форма, что в `GET /v1/incarnations/{name}`).

#### `GET /v1/incarnations/{name}/history` — журнал изменений state

Permission: `incarnation.history`. MCP-tool: `keeper.incarnation.history`. Path-param: `name`. Query:

| Параметр | Тип | Required | Смысл |
|---|---|---|---|
| `offset` | `int` | no | Pagination offset (≥ 0, default 0). |
| `limit` | `int` | no | Pagination limit (1..200, default 50). |
| `apply_id` | `string` (ULID) | no | Опциональный фильтр по `state_history.apply_id`. Валидируется как Crockford-base32 ULID (26 символов). Несуществующий, но синтаксически валидный `apply_id` для существующей incarnation → `200` + `items=[]`, не `404` (отсутствие row-а под фильтр — нормальный исход, например прогон ещё не завершился коммитом или не приводил к state-changes). Невалидный по формату — `400 malformed-request`. |

**Response `200`:** `{items: [StateHistoryEntry], offset, limit, total}`, где элемент — запись `state_history` ([architecture.md → state_history](../../architecture.md#state_history--журнал-изменений-state)):

| Поле | Тип | Смысл |
|---|---|---|
| `history_id` | `string` (UUID) | PK. |
| `scenario` | `string` | Имя сценария, приведшего к изменению (`"migration"` для шагов миграции). |
| `state_before` | `object` | jsonb состояние до. Sensitive-значения замаскированы (`***MASKED***`, см. [§ Маскинг state/spec в GET-ответах](../operator-api.md#маскинг-state--spec-в-get-ответах-defense-in-depth)). |
| `state_after` | `object` | jsonb состояние после. Sensitive-значения замаскированы (см. там же). |
| `changed_by_aid` | `string` | FK на `operators(aid)`. |
| `apply_id` | `string` (ULID) | ULID запуска. |
| `created_at` | `string` (RFC 3339) | Когда. |

#### ~~`GET /v1/incarnations/{name}/tides` — список Tide-прогонов~~ — superseded-by `GET /v1/voyages` ([ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), эндпоинт `/v1/tides` и таблица `tides` удалены в Wave 5; раздел ниже — историческая запись)

Permission: `incarnation.history` (parity GET `/history` — read о runtime-состоянии прогонов incarnation; отдельный `tide.read` perm не вводится до запроса оператора, [ADR-040 § RBAC reuse](../../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)). MCP-tool: `keeper.tide.list` (ADR-040 W-4). Path-param: `name`.

**Query:** `offset`, `limit` ([§ Pagination](../operator-api.md#pagination)) + опциональный фильтр по статусу:

| Param | Тип | Смысл |
|---|---|---|
| `status` | `enum` (`pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`) | Фильтр по статусу Tide. |

**Response `200`:** `{items: [Tide], offset, limit, total}`. Сортировка `started_at DESC` (свежие первыми). Form-а одного элемента — та же, что в `GET /v1/tides/{tide_id}`.

#### ~~`GET /v1/tides/{tide_id}` — snapshot Tide-прогона~~ — superseded-by `GET /v1/voyages/{voyage_id}` ([ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), удалён в Wave 5; раздел ниже — историческая запись)

Permission: `incarnation.history` (parity GET `/history`, см. выше). MCP-tool: `keeper.tide.get` (ADR-040 W-4). Path-param: `tide_id` (ULID).

UI делает client-side polling для прогресса (раз в 2–5 с) до появления нативного SSE-эндпоинта (отложено, см. ADR-040 open Q «Tide-progress SSE для UI»).

**Response `200 Tide`:**

| Поле | Тип | Смысл |
|---|---|---|
| `tide_id` | `string` (ULID) | PK Tide. |
| `incarnation_name` | `string` | На какой incarnation запущен. |
| `scenario_name` | `string` | Какой scenario разбит на Surge-волны. |
| `status` | `enum` | `pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`. |
| `total_surges` | `int` | Запланированное число Surge-волн (`ceil(scope_size / surge_size)`). |
| `current_surge_index` | `int` | 1-based номер текущей Surge (0 = ничего не запущено / Tide pending). |
| `surge_size` | `int` | Souls per Surge (echo `wave.size`). |
| `scope_size` | `int` | Размер snapshot SID[] (`target_resolved_souls`). |
| `on_surge_failure` | `enum` | `abort`/`continue` (echo `wave.on_failure`). |
| `target_coven_override` | `array<string>` (optional) | Эхо invocation-time `target.coven`-override. |
| `target_where_override` | `string` (optional) | Эхо invocation-time `target.where`-override. |
| `concurrency_override` | `int` (optional) | Эхо REPLACE-override scenario `serial:`. |
| `current_apply_id` | `string` (ULID, optional) | ULID head apply_run-а текущей Surge (NULL для pending). |
| `attempt` | `int` | Сколько раз Tide подбирался TideWorker-ом (инкремент при reclaim Reaper-правилом `reclaim_tides`). |
| `started_by_aid` | `string` | FK на `operators(aid)`. |
| `started_at` | `string` (RFC 3339) | Когда Tide вставлен (POST-handler). |
| `finished_at` | `string` (RFC 3339, optional) | Время финализации (NULL для pending/running). |
| `summary` | `object` (optional) | `{surges: [TideSurgeRecord]}` — per-Surge итог после финализации Tide. |

`TideSurgeRecord` поля: `surge_index` (int) / `apply_id` (ULID) / `terminal` (`success`/`failed`/`cancelled`/`orphaned`/`no_match`) / `started_at`, `finished_at` (RFC 3339) / `failed_souls` (int, omitempty) / `state_commit_error` (string, omitempty — per-Surge state-commit ошибка, [ADR-009 §7](../../architecture.md), [ADR-040 amendment](../../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).

**Errors:** `400 malformed-request` (невалидный ULID в path), `403`, `404` (`tide_id` не существует), `500`.

#### `POST /v1/incarnations/{name}/unlock` — снять `error_locked`

Permission: `incarnation.unlock`. MCP-tool: `keeper.incarnation.unlock`. Path-param: `name`.

Снимает статус `error_locked` после ручного разбора последствий частичного сбоя. Оператор берёт на себя ответственность, что хосты в консистентном состоянии.

**Request `IncarnationUnlockRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `reason` | `string` (1..500 символов) | yes | Свободный текст для audit-trail. Пишется в `state_history.metadata.unlock_reason`. |

```json
{ "reason": "manual cleanup verified — orphan keys removed on redis-prod-02" }
```

**Response `200`:**

| Поле | Тип | Смысл |
|---|---|---|
| `name` | `string` | Имя instance. |
| `previous_status` | `enum` | `error_locked` (для подтверждения). |
| `status` | `enum` | Обычно `ready`. |
| `unlocked_by_aid` | `string` | AID, выполнивший unlock. |
| `unlocked_at` | `string` (RFC 3339) | Время. |

**Errors:** `404 not-found`, `409` если статус не `error_locked` (`detail` указывает текущий статус), `422 validation-failed` если `reason` пустой.

#### `POST /v1/incarnations/{name}/upgrade` — перевод на новую state_schema_version

Permission: `incarnation.upgrade`. MCP-tool: `keeper.incarnation.upgrade`. Path-param: `name`.

Запускает миграцию state по [ADR-019](../../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) + переключает `service_version`. Одной PG-транзакцией ([migrations.md](../../migrations.md)).

**Request:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `to_version` | `string` (git-ref сервиса) | yes | Целевая версия сервиса. |

**Response `202 Accepted`:** `{"apply_id": "<ULID>"}`. Опрос — `GET /v1/incarnations/{name}` (`status: applying` → `ready` или `migration_failed`).

**Errors:** `404 not-found`, `409 incarnation-locked`, `409 migration-failed`, `422 validation-failed` (целевая версия не зарегистрирована).

#### `POST /v1/incarnations/{name}/check-drift` — Scry-проверка drift

Permission: `incarnation.check-drift`. MCP-tool: `keeper.incarnation.check-drift`. Path-param: `name`. **Sync-операция** (не async, в отличие от `run`/`upgrade`/`destroy`): handler блокируется до сборки `DriftReport` и возвращает его 200-ответом.

Реализует on-demand-пилот [ADR-031](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile). Keeper парсит `scenario/converge/main.yml` из текущего git-снапшота сервиса, рендерит план как для обычного apply, но шлёт всем хостам `ApplyRequest{dry_run:true}` через work-queue (Acolyte). Soul зовёт `mod.Plan` (pure-read) вместо `mod.Apply`, возвращает машинный `changed` для каждой задачи. Keeper собирает per-host агрегаты и формирует `DriftReport`. Информационный статус `drift` ставится post-check, если есть hosts_drifted/hosts_failed > 0 (НЕ блокирующий, [ADR-031(d)](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)).

**Конвенция input-резолва.** converge-сценарий объявляет `input:` схему; для каждого параметра значение берётся:
1. из `input.<имя>` body запроса, если оператор передал override;
2. иначе из `incarnation.state.<имя>` (конвенция «по имени»);
3. иначе из `default:` схемы;
4. иначе `required: true` без источника → `422 validation-failed` (drift-input-missing).

**Request:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `input` | `object` | no | Override converge-параметров. Имена/типы совпадают со схемой `input:` в `scenario/converge/main.yml` сервиса. |

**Response `200 OK`:** `DriftReport` (см. [openapi.yaml → DriftReport](../openapi.yaml)):

```json
{
  "checked_at": "2026-05-26T10:15:30Z",
  "incarnation": "redis-prod",
  "scenario_ref": "converge",
  "hosts": [
    {
      "sid": "host-a.example.com",
      "status": "drifted",
      "tasks": [
        {"idx": 0, "module": "core.pkg.installed", "action": "Install redis", "changed": false},
        {"idx": 1, "module": "core.file.present", "action": "redis.conf", "changed": true}
      ]
    }
  ],
  "summary": {"hosts_drifted": 1, "hosts_clean": 0, "hosts_unsupported": 0, "hosts_failed": 0}
}
```

**Per-host `status`:**
- `clean` — все task-ы хоста вернули `changed=false`;
- `drifted` — хотя бы один task `changed=true`;
- `unsupported` — хотя бы один community-модуль без `PlanReadSafe`-capability (default-deny, [ADR-031(f)](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile));
- `failed` — реальная ошибка Plan (отличается от `unsupported` по коду в `TaskError`).

**Errors:** `404 not-found`, `422 validation-failed` (converge отсутствует в текущем service-snapshot-е — «drift-проверка недоступна для этого сервиса», информационно; либо drift-input не резолвится), `500` (drift-checker не сконфигурирован — единственный inline-режим acolytes=0).

**RBAC:** scope тот же, что у `incarnation.run` — `coven=`/`service=`/`incarnation=` (env-RBAC, OR-Check по `IncarnationCovenContexts`).

**Audit:** `incarnation.drift_checked` пишется handler-ом после сборки отчёта, `correlation_id=apply_id`, payload `{name, scenario, apply_id, drift_summary}`.

#### `DELETE /v1/incarnations/{name}` — удалить instance

Permission: `incarnation.destroy`. MCP-tool: `keeper.incarnation.destroy`. Path-param: `name`.

Сносит instance. Operator-facing флаг `allow_destroy` маппится в internal `force` (унификация force↔allow_destroy): `false` — штатный destroy через teardown-сценарий `destroy` сервиса (с tombstone-периодом для облачных VM, [cloud.md → Безопасность destroy](../cloud.md#безопасность-destroy)); `true` — снос без teardown (DELETE строки напрямую, escape-hatch для instance без внешних ресурсов, warning в audit). Асинхронная операция.

**Query:**

| Param | Тип | Required | Смысл |
|---|---|---|---|
| `allow_destroy` | `bool` | yes | Обязательный confirmation flag (отсутствует или не-boolean → `400 malformed-request`). `false` — destroy через teardown-сценарий `destroy`; если в снапшоте сервиса нет сценария `destroy` → `422 validation-failed` (нечем выполнить teardown, передайте `true`). `true` — снос без teardown (force). Маппится в internal `force` (статус `destroying`, [`status_details.force`]). Симметрия с MCP-tool [`keeper.incarnation.destroy`](../mcp-tools/incarnations.md#keeperincarnationdestroy). |

**Response `202 Accepted`:** `{"apply_id": "<ULID>"}`. **Errors:** `400 malformed-request` (`allow_destroy` отсутствует / не boolean), `404 not-found`, `409 incarnation-locked` (статус не допускает destroy — `applying` / `destroying`), `422 validation-failed` (`allow_destroy=false` и нет сценария `destroy`).

**Манифест `lifecycle.auto_destroy` ([architecture.md → Service](../../architecture.md#service--структура-и-manifest)).** Если `manifest.lifecycle.auto_destroy: false`, удаление **всегда** прямое (DELETE без teardown), приоритет над `allow_destroy` — даже `allow_destroy=false` не запускает teardown-сценарий и не упирается в `422` «нет сценария `destroy`». По умолчанию (`true`, backcompat) удаление идёт по обычной логике `allow_destroy`. Резолвится из снапшота развёрнутого service-ref-а.

#### `PATCH /v1/incarnations/{name}/hosts` — править declared `spec.hosts[]`

Permission: `incarnation.update-hosts`. Path-param: `name`. **REST-only — MCP-tool нет** (`manifest.go` не содержит `keeper.incarnation.hosts.update`; UI Hosts editing ходит напрямую в REST). **Sync-операция** (не async): правка declared `spec.hosts[]` — это не прогон, ответом возвращается обновлённый incarnation, без `apply_id`.

Редактирует declared список хостов incarnation (`spec.hosts[]`, [ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). `spec.hosts` — declared-вход следующего прогона (source of truth для bootstrap-`create` и topology-резолва `soulprint.hosts[].role`), **не** state-переход: `state_history`-row не пишется. Атомарность — одна PG-транзакция (`SELECT FOR UPDATE` → guard статуса → batch-валидация SID в реестре `souls` → `UPDATE spec`).

**Три mode-семантики** над текущим `spec.hosts[]`:
- `replace` — полная замена списка переданным набором. Пустой `hosts: []` валиден — осознанная очистка declared-spec (`422` на пустой набор сознательно **не** выдаётся).
- `append` — insert-or-update по SID: новые хосты добавляются, при совпадении SID `role` существующей записи перезаписывается. Пустой `hosts: []` — no-op.
- `remove` — удалить записи с переданными SID-ами; `role` в payload при `remove` игнорируется (важен только `sid`). Пустой `hosts: []` — no-op.

**Request `IncarnationUpdateHostsRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `mode` | `enum` (`replace`/`append`/`remove`) | yes | Тип операции над `spec.hosts[]`. Неизвестное значение → `422 validation-failed`. |
| `hosts` | `list<IncarnationSpecHost>` | yes | Набор для применения mode-операции. Может быть пустым (см. семантику mode выше). |

`IncarnationSpecHost` (item):

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | SID хоста; обязан существовать в реестре `souls` (иначе `422`). |
| `role` | `string` (kebab-case, 1..63) | optional | Declared-роль. Формат `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` либо отсутствует/пустая ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) допускает null для хостов вне declared-spec). Operator-asserted строка, список не предопределён (`master`/`replica` — частые, но не исчерпывающие). |

```json
{
  "mode": "append",
  "hosts": [
    { "sid": "redis-prod-04.example.com", "role": "replica" },
    { "sid": "redis-prod-05.example.com" }
  ]
}
```

**Response `200 OK`:** полный `IncarnationGetReply` (та же форма, что у `GET /v1/incarnations/{name}`) с уже применённой правкой `spec.hosts[]`. `state`/`spec` маскируются по общему правилу ([§ Маскинг state/spec в GET-ответах](../operator-api.md#маскинг-state--spec-в-get-ответах-defense-in-depth)).

**Errors:** `400 malformed-request` (битый JSON / неизвестное поле тела — decoder в strict-режиме `DisallowUnknownFields`), `403 forbidden`, `404 not-found` (incarnation не существует), `409 incarnation-locked` (статус `destroying` / `destroy_failed` — правка spec при сносе бессмысленна; прочие статусы, включая `applying`, допустимы), `422 validation-failed` (невалидный path-`name` / невалидный `sid` / невалидная `role` / неизвестный `mode` / SID-ы отсутствуют в реестре `souls`), `500 internal-error`.

**RBAC:** scope-селектор `incScope` (env-RBAC, паритет `run`/`upgrade`/`destroy` — `coven=`/`service=`/`incarnation=` по path-`name`: declared `covens ∪ {name}` + `service`). Permission `incarnation.update-hosts` сужена с прежней `incarnation.update` (PM-decision 2026-06-02); backcompat-alias `incarnation.update` канонизируется в `incarnation.update-hosts` на load снимка RBAC.

**Audit:** `incarnation.hosts_updated` (`source: api` / `mcp`, `archon = JWT.sub`, payload `{name, mode, old_hosts, new_hosts}`) — пишется handler-ом **после** commit-а (payload содержит old/new snapshot, доступный только после `UpdateHosts`); не идёт через generic audit-middleware.

#### `PUT /v1/incarnations/{name}/traits` — заменить trait-метки инкарнации

Permission: `incarnation.traits-set`. MCP-tool: `keeper.incarnation.traits-set`. Path-param: `name`. **Sync-операция** (не async): правка operator-set меток — это не прогон, ответом возвращается обновлённый incarnation, без `apply_id`.

Целостно **заменяет** operator-set trait-метки инкарнации (`incarnation.traits` jsonb — источник истины, [ADR-060](../../adr/0060-traits.md) R1 slice a). Trait — организационная метка владельца/продукта/namespace всего инстанса (`owner=alice`, `product=aboba`, `namespace=dba-ns`), **отдельная ось рядом с плоским Coven** ([ADR-008](../../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)): Coven — членство/таргетинг/RBAC, Trait — key-value атрибуты. Атомарность — одна PG-транзакция (`SELECT FOR UPDATE` → `UPDATE traits`); статус-гейта нет (метки безопасно менять на любом статусе). После commit-а sync-hook **материализованно проецирует** новый набор в `souls.traits` всех хостов-членов инкарнации (хост = SID, у которого имя инкарнации ∈ `souls.coven[]`). Проекция best-effort: её сбой не валит запрос (`incarnation.traits` уже записан, до-сойдётся при следующем bind/sync).

Заменяет per-soul write-путь `POST /v1/souls/traits` (deprecated, см. [Soul → bulk traits](souls.md)). Per-soul write ещё работает, но **перетирается проекцией** `incarnation.traits` при следующем sync.

**Request `IncarnationSetTraitsRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `traits` | `object` | optional | Полный набор trait-меток: ключ → значение `scalar` (`string`/`number`/`boolean`) ИЛИ `list of scalars` (`["alice", "bob"]`). Ключ — `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`. **Replace-семантика** — переданный набор заменяет текущий целиком; пустой `{}` / опущенное поле = **очистить** все метки. Вложенный объект / массив-в-массиве → `422`. |

```json
{
  "traits": {
    "owner": "alice",
    "owners": ["alice", "bob"],
    "namespace": "dba-ns"
  }
}
```

**Response `200 OK`:** полный `IncarnationGetReply` (та же форма, что у `GET /v1/incarnations/{name}`) с уже применённой заменой `traits`. `state`/`spec` маскируются по общему правилу ([§ Маскинг state/spec в GET-ответах](../operator-api.md#маскинг-state--spec-в-get-ответах-defense-in-depth)).

**Errors:** `400 malformed-request` (битый JSON / неизвестное поле тела), `403 forbidden`, `404 not-found` (incarnation не существует), `422 validation-failed` (невалидный path-`name` / невалидный ключ / вложенное trait-значение), `500 internal-error`.

**RBAC:** scope-селектор тот же, что у `incarnation.update-hosts` (env-RBAC, `coven=`/`service=`/`incarnation=` по path-`name`: declared `covens ∪ {name}` + `service`). trait-**ключ** НЕ scope-измерение — гейта на ключи нет.

**Audit:** `incarnation.traits_changed` (`source: api` / `mcp`, `archon = JWT.sub`, payload `{name, old_keys, new_keys}`) — пишется handler-ом **после** commit-а. Payload несёт только отсортированные списки trait-**КЛЮЧЕЙ** до и после; сами trait-**ЗНАЧЕНИЯ** в audit НЕ кладутся (секрет-гигиена: trait-value может нести инфраструктурные данные хоста — симметрично `soul.traits-changed`).
