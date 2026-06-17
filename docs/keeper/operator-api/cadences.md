# Cadence — endpoints регулярных запусков (scheduled/recurring Voyage)

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/cadences*` — расписания, по времени спавнящие обычный [Voyage](voyages.md)-прогон ([ADR-046](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage), реестр `cadences`). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). Поведение исполнителя расписаний (due-выборка, `overlap_policy`, пересчёт `next_run_at`, adaptive poll) — [conductor.md](../conductor.md) (source-of-truth поведению). **MCP-стороны нет** — у Cadence MCP-tool-ов не заведено ([mcp-tools/cadences.md](../mcp-tools/cadences.md)).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 8 роутов) — в корневом [operator-api.md → Cadence (8)](../operator-api.md#cadence-8--регулярные-запуски-scheduledrecurring-voyage-adr-046--adr-048). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`CadenceCreateRequest` / `CadencePatchRequest` / `Cadence` / `CadenceCreateReply` / `CadenceEnabledReply` / `CadenceListReply` — **источник правды по форме**). Ниже — нормативная семантика поведения, на которую опирается контракт.

Cadence — это строка в `cadences` с «рецептом» прогона (то же множество полей, что [`VoyageCreateRequest`](voyages.md): `kind`/`scenario_name`|`module`/`target`/`input`/batch-настройки) + правилом повторения (`schedule_kind` `interval`|`cron`) + `overlap_policy`. Исполняет триггер [Conductor](../conductor.md) — leader-elected подсистема внутри `keeper`; спавнутый Voyage входит в обычный Voyage-lifecycle.

### Двухуровневый RBAC (security-критичный fail-closed)

Право `cadence.*` управляет самим расписанием, но рецепт спавнит Voyage от имени создателя ([ADR-046 §7](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). Поэтому на **создании** (и на правке target/рецепта) действует **второй уровень** — Voyage-permission по `kind` рецепта (`scenario`→`incarnation.run`, `command`→`errand.run`, [ADR-043 §6](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)): иначе Cadence стала бы privilege-escalation-обходом RBAC. `kind` виден только из тела → второй гейт живёт внутри `CadenceHandler.Create`/`.Patch` (первый, `cadence.create`/`cadence.update`, гейтит middleware-route).

Для `kind=scenario` сверх bare-check работает **per-target coven-scope-check** (parity `VoyageHandler.createScenario`): резолвнутый target (его `covens ∪ {name}`) обязан лежать в RBAC-скоупе создателя на каждой инкарнации — иначе scoped-Архонт «run on `coven=A`» создал бы Cadence на `coven=B` (вне scope) и фоновый спавн исполнил бы вне scope. `kind=command` — bare-check `errand.run` (per-host селекторы отложены пост-MVP, parity Voyage).

PATCH несёт тот же двухуровневый guard: он меняет `target`/`scenario_name`, поэтому без guard-а scoped-Архонт создал бы Cadence на разрешённом `coven=A` и PATCH-ом перенаправил target на `coven=B`. `kind` в PATCH **не меняется** (смена `kind` = delete + create).

### `POST /v1/cadences` — создать Cadence

Permission: `cadence.create` (middleware) **+** Voyage-permission по `kind` (handler, см. выше). MCP-tool: нет. Async: нет (создание расписания синхронно; спавн прогонов — отложенный, ведёт Conductor).

**Request `CadenceCreateRequest`** (`required: name, schedule_kind, overlap_policy, kind, target`):

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `name` | `string` | yes | Человекочитаемое имя расписания. |
| `schedule_kind` | `string` (`interval`/`cron`) | yes | Вид правила повторения (`interval_seconds` XOR `cron_expr`). |
| `interval_seconds` | `integer` (≥30) | для interval | Период. Минимум **30s** (floor-лимит, ADR-046 Pass B); `<30` → `422`. Для реакции быстрее 30s — Beacons (Vigil/Oracle, [ADR-030](../../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor)). |
| `cron_expr` | `string` | для cron | Стандартное 5-полевое cron-выражение (UTC). |
| `overlap_policy` | `string` (`skip`/`queue`/`parallel`) | yes | Поведение при наложении (предыдущий ребёнок ещё не терминален). |
| `kind` | `string` (`scenario`/`command`) | yes | Тип рецепта-прогона. |
| `scenario_name` | `string` | для scenario | Обязательно для `kind=scenario`; запрещено для `command`. |
| `module` | `string` | для command | Обязательно для `kind=command`; запрещено для `scenario`. |
| `target` | `VoyageTarget` | yes | Таргет прогона (резолвится на **спавне**, не на создании). |
| `input` | `object` | no | Параметры прогона. **НЕ логируется** (инвариант A [ADR-027](../../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). |
| `batch` | `string` | no | Размер Leg: `N` единиц / `N%` (1..100) от spawn-scope. Взаимоисключающе с `batch_size`/`batch_percent` → `422 voyage_batch_spec_conflict`. |
| `max_failures` | `string` | no | Порог провалов: `N` абсолют / `N%` от единиц прогона spawn-scope. Взаимоисключающе с `fail_threshold` → `422`. |
| `batch_size` / `batch_percent` | `integer` | no | **DEPRECATED** (используйте `batch`). |
| `fail_threshold` | `integer` | no | **DEPRECATED** (используйте `max_failures`). |
| `concurrency` | `integer` (≥1) | no | Параллелизм внутри Leg (barrier) / ширина окна (window). |
| `batch_mode` | `string` (`barrier`/`window`) | no | Режим батчинга (`NULL` ⇒ `barrier`). |
| `inter_batch_interval_ms` / `inter_unit_interval_ms` | `integer` (≥0) | no | Паузы между Leg-ами (barrier) / per-unit (window). |
| `require_alive` | `boolean` | no | Presence-фильтр живых на резолве scope (kind=command). Default `false`. |
| `on_failure` | `string` (`abort`/`continue`) | no | Поведение при провале Leg. |
| `enabled` | `boolean` | no | Включено ли расписание. Default **`true`** (default-ON; `false` → пауза). |
| `notify` | `array<VoyageNotify>` | no | Подписки на уведомления о прогонах ЭТОГО расписания. Форма элемента — та же [`VoyageNotify`](voyages.md), что у разового `voyage.notify`. См. [«Уведомления `notify[]`»](#уведомления-notify--постоянные-tiding-из-формы-расписания) ниже. |

Процентные `batch`/`max_failures` (формат `N%`) **не резолвятся в абсолют на create** — у Cadence spawn-scope неизвестен на создании; процент стешится в колонки `batch_percent`/`fail_threshold_percent` и резолвится на spawn-scope при спавне Voyage.

### Уведомления `notify[]` — постоянные Tiding из формы расписания

`notify` ([ADR-052 §m](../../adr/0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов), опц.) — список подписок на уведомления о прогонах **этого** расписания. Форма каждого элемента — та же [`VoyageNotify`](voyages.md) (`herald` + опц. `on`/`only_failures`/`only_changes`/`annotations`/`projection`), что у разового `voyage.notify`; валидация и RBAC переиспользуются. Отличие от Voyage-формы — в природе создаваемого правила:

- **Постоянное, не ephemeral.** В отличие от `voyage.notify` (разовое правило `ephemeral=true` на один прогон), каждый элемент `cadence.notify` материализуется keeper-ом в **постоянный** Tiding (`ephemeral=false`), который слушает прогоны расписания и дальше — пока расписание живёт.
- **Привязка по ULID расписания.** Правило несёт селектор `cadence` (фильтр «слать только про прогоны этого расписания») + внутренний origin-маркер `created_from_cadence_id = cadences.id` (стабильный ULID-PK, rename-safe — имя расписания мутабельно через PATCH). Имя автоправила — `<name>-notify[-N]` (детерминированный суффикс).
- **Атомарность.** Insert правил идёт в **той же PG-транзакции**, что создаёт Cadence: либо Cadence + все правила, либо ничего (FK/коллизия имени/валидация откатывает весь `POST`).
- **Каскад при удалении.** `DELETE /v1/cadences/{id}` каскадно сносит порождённые формой правила ([ADR-046 §9](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage), `tidings.created_from_cadence_id ON DELETE CASCADE`). Вручную заведённые Tiding-и с тем же `cadence`-селектором (но `created_from_cadence_id = NULL`) **не трогаются** — маркер происхождения ортогонален фильтр-селектору.
- **RBAC `herald.read`.** Инициатор обязан иметь permission `herald.read` на **каждый** указанный канал доставки — иначе `403` (нельзя «подвесить» доставку через чужой Herald). Несуществующий канал → `422`.
- **Cap 64 канала.** Длина `notify[]` ограничена **64** каналами на расписание (превышение → `422` до открытия транзакции).

`notify=[]`/опущено ⇒ расписание без уведомлений (одна транзакция с единственным Insert Cadence).

**Response `201 CadenceCreateReply`** (`required: cadence_id, name, enabled, location`) + заголовок `Location: /v1/cadences/{id}`:

```json
{
  "cadence_id": "01HABCDEFGHJKMNPQRSTVWXYZ",
  "name": "nightly-converge",
  "enabled": true,
  "next_run_at": "2026-06-11T00:00:00Z",
  "location": "/v1/cadences/01HABCDEFGHJKMNPQRSTVWXYZ"
}
```

`next_run_at` вычисляется при создании (чистая функция от расписания; для `enabled=false` тоже считается — серия не «залипает», стартует при enable). `created_by_aid = JWT.sub`. Audit: `cadence.created`.

**Errors:** `400` (невалидный JSON); `401` `unauthenticated` / `operator-revoked-token` (AID ревокнут); `403 forbidden` (двухуровневый RBAC deny: нет Voyage-permission по `kind`, либо target вне scope для scenario, **либо нет `herald.read` на канал из `notify[]`**); `404 not-found` (явная инкарнация target-а не существует, scenario); `422 validation-failed` (невалидный рецепт/расписание: XOR interval/cron, enum `overlap_policy`/`kind`/`batch_mode`/`on_failure`, `kind`↔`scenario_name`/`module`, битый cron, batch-spec conflict, пустой резолв `cadence_empty_target`, **floor-лимит `interval_seconds < 30`**, **несуществующий Herald-канал в `notify[]` / `notify[]` > 64 каналов**); `500` (store/enforcer не сконфигурирован / БД-сбой).

### `GET /v1/cadences` — список Cadence-расписаний

Permission: `cadence.list`. MCP-tool: нет. Query: `enabled` (`true` → только enabled; `false`/опущен → все), `kind` (exact `scenario`/`command`) + `offset`/`limit` ([§ Pagination](../operator-api.md#pagination)). Sort `created_at` DESC. Response `200 CadenceListReply` (`{items, offset, limit, total}`). `input` рецепта в list-выдаче **не отдаётся** (инвариант A ADR-027).

### `GET /v1/cadences/{id}` — деталь Cadence

Permission: `cadence.list`. MCP-tool: нет. `id` — ULID. Response `200 Cadence` (рецепт + расписание + `next_run_at`/`last_run_at` + audit-метаданные; `input` НЕ отдаётся). `404 cadence_not_found` (записи нет); `422 validation-failed` (`id` не ULID).

### `PATCH /v1/cadences/{id}` — обновить Cadence

Permission: `cadence.update` (middleware) **+** двухуровневый guard (handler, см. выше). MCP-tool: нет. Read-modify-write: заданные поля перезаписывают, опущенные сохраняются. Request `CadencePatchRequest` (все поля опциональны; `kind` отсутствует — не меняется). При смене расписания (`schedule_kind`/`interval_seconds`/`cron_expr`) — пересчёт `next_run_at`. Response `200 Cadence`. Audit: `cadence.updated`.

**Errors:** `400` (невалидный JSON); `403` (двухуровневый RBAC deny на пост-patch target); `404 cadence_not_found`; `422 validation-failed` (невалидный рецепт/расписание, batch-spec conflict, **floor-лимит** — в т.ч. при переводе расписания на `interval`); `500` (БД-сбой).

### `POST /v1/cadences/{id}/enable` | `POST /v1/cadences/{id}/disable` — toggle расписания

Permission: **`cadence.enable` ИЛИ `cadence.update`** (enable) / **`cadence.disable` ИЛИ `cadence.update`** (disable) — OR-гейт `RequireAnyPermission` (backcompat: роли со старым `cadence.update` сохраняют toggle, [ADR-046 amendment 2026-06-02](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). MCP-tool: нет. Lightweight toggle без перезаписи рецепта. Response `200 CadenceEnabledReply` (`{cadence_id, enabled}`). Audit: `cadence.updated`. `404 cadence_not_found`; `422` (`id` не ULID).

### `GET /v1/cadences/{id}/runs` — дочерние Voyage расписания

Permission: **`incarnation.history`** (read runtime-состояния прогонов, parity Voyage-list — не `cadence.*`). MCP-tool: нет. Drill «расписание → его прогоны» (`voyages WHERE cadence_id=$1`, reuse [Voyage-DTO](voyages.md)). Query: `status` (multi-value `?status=X&status=Y`, OR-семантика) + `offset`/`limit`. Response `200 VoyageListReply`. `404 cadence_not_found` (если Cadence не существует — пустой список неотличим от несуществующего id, поэтому existence-probe). `422 validation-failed` (`id` не ULID / битый `status`-фильтр).

> **Note (поведение исполнителя).** При `DELETE /v1/cadences/{id}` (`cadence.delete`) порождённые Voyage **остаются** (FK `voyages.cadence_id ON DELETE SET NULL` — история детей и ручные прогоны сохраняются), но постоянные Tiding-и из блока `notify[]` **сносятся каскадом** (FK `tidings.created_from_cadence_id ON DELETE CASCADE`, [ADR-046 §9](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)); вручную заведённые правила с тем же `cadence`-селектором (`created_from_cadence_id = NULL`) не трогаются. Спавн-семантику (due-выборка, три `overlap_policy`, anchored-пересчёт `next_run_at`, missed-slot anti-storm, авторство дочернего Voyage от `created_by_aid` Cadence с audit `source: background`) ведёт [Conductor](../conductor.md), а не эти эндпоинты.
