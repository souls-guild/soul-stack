# Voyage — endpoints унифицированного батчевого прогона

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/voyages*` (батч N инкарнаций по scenario / N хостов по command, [ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/voyages.md](../mcp-tools/voyages.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 5 роутов + RBAC-by-kind) — в корневом [operator-api.md → Voyage (5)](../operator-api.md#voyage-5--унифицированный-батчевый-прогон-adr-043).

Унифицированный батчевый прогон ([ADR-043](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)): `kind=scenario` — применить named scenario к набору ИНКАРНАЦИЙ (батч = N инкарнаций по Leg-ам); `kind=command` — выполнить whitelisted-модуль на наборе ХОСТОВ (батч = N хостов, `incarnation.state` не трогается). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`VoyageCreateRequest` / `VoyageCreateReply` / `VoyagePreviewReply`). Ниже — нормативная семантика поведения, на которую опирается контракт.

#### `POST /v1/voyages` — создать Voyage

Permission: **RBAC-by-kind** ([ADR-043 §6](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), security-критичный fail-closed guard). Permission выбирается по `kind` из тела (`scenario`→`incarnation.run`, `command`→`errand.run`) — middleware-route этого не может (kind виден только после декода body), поэтому проверка живёт внутри handler-а. MCP-tool: `keeper.voyage.start`.

Async-by-default: **202** + `{voyage_id, kind, scope_size, status, location}` + заголовок `Location: /v1/voyages/{id}`. VoyageWorker подбирает строку через claim-loop; прогресс — `GET /v1/voyages/{id}`.

**Target-резолв и scope-границы:**

- **kind=scenario** — bare-check `incarnation.run` (быстрый отказ до резолва), затем per-incarnation scope-check над каждой резолвнутой инкарнацией (её covens ∪ `{name}`): старт на инкарнации вне permission-скоупа = privilege escalation → **403**.
- **kind=command** — target ∩ Purview оператора ([ADR-047 §S4](../../adr/0047-purview.md#s4--target--purview-для-command-пути-voyage-security-fix), security-fix). См. ниже.

**Errors:** `400` malformed JSON; `401` `unauthenticated` (нет/невалиден JWT) или `operator-revoked-token` (AID ревокнут, командный existence-gate, см. ниже); `403` RBAC deny по kind / явный чужой хост (command) / инкарнация вне scope (scenario); `404` явная инкарнация не существует (scenario); `422` невалидный `kind` / пустой `scenario_name`/`module` по kind / нет target / невалидный SID/coven/имя / `where` > 4 KiB / `on_failure` не из `{abort, continue}` / `batch_size`+`batch_percent` одновременно / `batch_size`+`batch_mode=window` / диапазоны / пустой резолв (`voyage_empty_target`) / scope > `voyage.max_scope` (`voyage_scope_too_large`) / эффективный batch выше `voyage.max_batch_size` (`voyage_batch_size_too_large`); `429` `tempo-exceeded` (rate-limit, см. ниже); `500` orchestrator не сконфигурирован / БД-сбой.

> **`input` не логируется.** Audit-события `scenario_run.started` / `command_run.invoked` не несут тело `input` (инвариант A ADR-027).

##### command ∩ Purview — security-fix с изменением поведения

**Изменение поведения для scoped-ролей.** Раньше `kind=command` резолвил target **cluster-wide** (bare NoSelector, без пересечения с Purview): scoped-Архонт с `errand.run on coven=A` мог запустить command-Voyage на `coven=B`. Теперь резолвнутый target **пересекается с Purview** оператора через тот же `soulpurview`-резолвер, что фильтрует `GET /v1/souls` ([ADR-047 §S4](../../adr/0047-purview.md#s4--target--purview-для-command-пути-voyage-security-fix)). Покрытие = `target ∩ ResolvePurview(aid, "errand", "run")`. **Для `Unrestricted` / cluster-admin (`*`-permission) поведение не меняется** — `Unrestricted` → весь флот, как раньше. Зафиксировано как **security-fix в release-notes**.

Гибрид-семантика — три ветки по форме target-а (выбор пользователя 2026-06-09):

1. **Явный чужой хост в `sids[]`** (оператор перечислил конкретные SID, часть вне Purview) → **403** (anti-escalation, parity со scenario-путём). Явное указание чужого хоста — попытка эскалации, а не широкий фильтр; молчаливое урезание тут было бы маскировкой.
2. **Широкий target** (`coven=…` / `where:`-предикат, late-binding) → **урезается** до `target ∩ Purview` без отказа (как list-видимость: оператор получает то, что внутри его границы).
3. **Пустое пересечение** (после урезания не осталось хостов) → **422 `voyage_empty_target`** (валидный запрос, нечего исполнять — отличаем от 403-эскалации).

**Existence-gate** — единый, через `ResolvePurview("errand","run")`: «держит ли оператор право хоть в каком-то scope» проверяется тем же резолвом, что даёт scope-границу — без отдельного nil-context bare-check-а (одиночная `Check(nil)` ложно денит scoped-роль с непустым scope). Пустой Purview (`Scope.Empty`: ни одного измерения и не `Unrestricted`) → отказ ДО резолва target-а; причина классифицируется enforcer-ом: **revoked-токен → `operator-revoked-token` (401)**, no-perm → **403** (`operator lacks required permission errand.run`).

> **AND-семантика `coven` для command.** `coven=[A,B]` означает хост, входящий во **ВСЕ** перечисленные covens (`souls.coven @> [A,B]`) — намеренно, в отличие от scenario-пути (где coven — один env-тег фильтра). Это AND-merge security-инвариант ([ADR-043 §5](../../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)): invocation сужает scope, не расширяет.

> **soulprint/state-измерения Purview** в command∩Purview пока под-показ (fail-closed: при неполной поддержке измерения резолвер скорее урежет свой хост, доступный ТОЛЬКО по soulprint, чем покажет чужой); coven/regex/host работают полноценно (S3b-2b отложен).

##### Tempo rate-limit

`POST /v1/voyages` — resolver-тяжёлый write-эндпоинт под [Tempo](../config.md#tempo) per-AID rate-limiter ([ADR-050](../../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)): bucket `voyage_create` (default `10 rps`, burst `20`). Превышение → **429 `tempo-exceeded`** + `Retry-After`. При недоступном Redis лимит fail-OPEN (passthrough). GET/list/cancel не лимитятся (дёшевы).

#### `POST /v1/voyages/preview` — dry-resolve scope без создания Voyage

Permission: **RBAC-by-kind** (та же, что Create). REST-only (MCP-tool нет). Назначение — **предпоказ числа батчей в UI** для late-binding target-а (`coven` / `require_alive`, где число хостов резолвит Keeper): прогоняет тот же резолв и те же гейты, что Create, но **не пишет** в `voyages`/`voyage_targets` и **не раскрывает SID-список** — отдаёт только числа. Для snapshot-таргета (явные `incarnations[]`/`sids[]`) клиент считает число батчей сам — эндпоинт не обязателен.

**Request** — тот же body, что `POST /v1/voyages` (`VoyageCreateRequest`). Учитываются (влияют на резолв/арифметику): `target`, `kind`, `batch`/`batch_size`/`batch_percent`/`batch_mode`, `concurrency`, `max_failures`, `require_alive`. Игнорируются (не читаются в reply): `dry_run`, `schedule_at`, `inter_batch_interval_ms`, `inter_unit_interval_ms`, `on_failure`, `input`.

**Response `200 VoyagePreviewReply`:**

| Поле | Тип | Смысл |
|---|---|---|
| `kind` | `string` | `scenario` / `command` (эхо). |
| `scope_size` | `int` | Число резолвнутых единиц (инкарнаций / хостов). SID-список НЕ раскрывается. |
| `total_batches` | `int` | Число Leg-ов (barrier) либо `1` (window). 0 невозможен — пустой scope отсечён 422 до построения reply. |
| `batch_mode` | `string` | `barrier` / `window`. Присутствует ВСЕГДА — объясняет семантику остальных полей. |
| `effective_batch_size` | `int?` | Резолвнутый размер Leg (barrier). **Опущен** при `batch_mode=window` (ширина окна = `concurrency`, не Leg — UI читает `concurrency`) либо если весь scope идёт одним Leg (batch не задан). |

**Консистентность с Create** — preview отказывает РОВНО там же: тот же общий путь декода/валидации/резолва. `403`/`404`/`422` приходят при тех же условиях, что у Create (scenario — per-incarnation scope-check; command — гибрид-семантика 403/урезание/422; scope > `voyage.max_scope` → 422). Rate-limited через **собственный** Tempo-bucket `voyage_preview` ([ADR-050 amendment](../../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)) — **более мягкий**, чем create (default `30 rps`, burst `60` против `10`/`20`), потому что preview по эффекту read-like (без persist/audit), но resolver-heavy по стоимости. Превышение → **429 `tempo-exceeded`** + `Retry-After`.

> **Note (mapping↔router).** В [`router.go`](../../../keeper/internal/api/router.go) у Voyage есть шестой read-роут — `GET /v1/voyages/{id}/targets` (All-runs drill, ADR-043 S5), не сведённый в корневую mapping-таблицу `### Voyage (5)`. Его MCP-симметрии нет (REST-only). Сведение — отдельный doc-PR (см. отчёт docs-writer по drift).
