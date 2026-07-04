# ADR-069. Видимость прогонов в Operator UI — live-ход инкарнации + история + единый All Runs

> **Статус: proposed (DRAFT); §РЕШЕНИЯ ПРИНЯТЫ юзером — финальное добро 2026-07-04.** Дизайн-сессия 2026-07-04 (`NIM-37` live-ход + история + `NIM-38` единый All Runs). Смежный `NIM-36` (рендер keeper-бейджа) — DONE, ссылается ниже.
>
> **Решения пользователя (2026-07-04):** Q2 ✓ (показывать keeper-шаги), Q3 ✓ (единый All Runs), C ✓ (путь эндпоинта), D ✓ (один ADR), B → architect. **Q1 расширен**: не только live, но и **персистентная история** прогонов (восстановить ход недель/месяц спустя) — при этом «трезво оценивать нагрузку на БД». **★ФИНАЛЬНОЕ ДОБРО 2026-07-04:** пакет §РЕШЕНИЯ принят целиком, impl разблокирована; **A0 РЕШЁН** (координатор): SSE-auth через `fetch`-streaming с `Authorization`-заголовком — токен в URL / `sse-token` НЕ нужны, ротации нет.
>
> **Координация:** бэкенд-волна уже запущена (S37-1 live-инфраструктура ∥ S38-1 sort). Live-часть (SSE-route, applybus, `applying_apply_id`) валидна при любом Q1/A0. Персистентная история (Q1b, §A5) и A0=fetch-streaming (§A3) — уточнения этого пересмотра.
>
> **DRAFT** в имени файла держится теперь ТОЛЬКО из-за промоушена в индекс ([README](README.md)/[architecture.md](../architecture.md)/[naming-rules](../naming-rules.md)) — за координатором (см. §«Регистрация»). **Вето юзера снято.**
>
> **★Номер согласован (2026-07-04):** `0068` уступлен **NIM-34** (версионирование `upgrade/<slug>/`, уже DONE — двигать реализованное дороже, чем DRAFT); этот ADR перенумерован в **`0069`** (свободен в дереве). Координатор (NIM-24-линия / единый push-gate) подтверждает финал при сведении на каноне — если `0069` окажется занят другой веткой, сдвинуть только этот файл.
>
> **Нумерация.** `0069` (был `0068`, но коллизия с NIM-34 upgrade-версионированием, тоже занявшим `0068` — уступлено, см. выше). `0069` свободен в дереве; координатор подтверждает при сведении.

---

## РЕШЕНИЯ — ПРИНЯТЫ юзером (финальное добро 2026-07-04)

| # | Развилка | Решение | Статус |
|---|---|---|---|
| **A0** | SSE-auth handshake | **fetch-streaming ВЫБРАН** (координатор 2026-07-04): `fetch` шлёт обычный `Authorization: Bearer` заголовок, поток через `ReadableStream` → **токен в URL / `sse-token` не нужны, ротации нет**. Query-token (`POST /v1/sse-token`) — **отброшен** (боль ротации: TTL≪lifetime стрима). Cookie отвергнут. Backend live-инфра от auth **не зависит**. | ✓ fetch-streaming (Authorization-header) |
| **Q1a** | Live-ход (пока прогон идёт) | **SSE эфемерный**: per-task через applybus `task.executed` (`task_status ∈ OK/CHANGED/FAILED/TIMED_OUT` уже течёт). | ✓ |
| **Q1b** | История (восстановить ход недель/месяц спустя) | **Из `audit_log`** — per-task ход **уже персистится** туда (`events_taskevent.go:91` пишет `task.executed` в audit, не только в live), индекс `audit_log_correlation_id_idx` **уже есть**, retention **настраиваемый** (`audit.retention_days`). Нужно: UI-timeline-читатель + дефолт retention. **Нулевая новая write-нагрузка** (данные уже пишутся). §A5. | ✓ юзер расширил Q1; нагрузка учтена |
| **Q2** | keeper-side шаги в live | **Публиковать в applybus** (сейчас только audit, `keeper_dispatch.go:145-146`) — чтобы cloud/vault/registered-шаги были видны live под `sid="keeper"`. | ✓ «да, показывать» |
| **Q3** | Форма All Runs | **ЕДИНЫЙ**: одна страница со всеми типами (`apply_run`+`voyage`+`push`+`errand`) + фильтр по типу + сортировка колонок. | ✓ «сильно удобнее» |

**Под-развилки:**

- **A (SSE-auth) — РЕШЕНО: fetch-streaming.** `fetch(url,{headers:{Authorization}})` + `response.body.getReader()` + ручной парсинг SSE-frames. Обычный Bearer (как все запросы) → **нет токена в URL, нет ротации**. Цена — auto-reconnect/парсинг событий руками (EventSource даёт бесплатно), приемлемо. **Отброшено:** короткоживущий query-token (`EventSource('…?access_token=')` + `POST /v1/sse-token`) — токен в URL оседает в логах + TTL≪lifetime стрима → ротация. cookie отвергнут (`tokenStore.ts`).
- **B (линковка incarnation→прогон) → architect.** Кандидат: отдать уже пишущуюся колонку `applying_apply_id` (`lockRun` [`state.go:45`](../../keeper/internal/scenario/state.go), чистится [`crud.go:849,1197`](../../keeper/internal/incarnation/crud.go)) в `GET /v1/incarnations/{name}` — non-null пока прогон идёт. «Последний завершённый» — top-1 из `GET /v1/incarnations/{name}/runs`. Реализовано в S37-1; финальную форму подтверждает architect.
- **C (эндпоинт live) ✓.** `GET /v1/incarnations/{name}/runs/{apply_id}/events` — симметрия `RunDetail`-пути + прецеденту `/v1/voyages/{id}/events`.
- **D (один ADR) ✓.** Оба тикета — один ADR-0069.

---

## Контекст (что видит оператор сейчас)

**NIM-37.** На странице инкарнации **нет таба прогонов** — «History» показывает `state_history` (снапшоты state), не ход apply. Оператор не видит ни live, ни историю прогонов (какой сценарий шёл, какая задача, изменилось ли что-то). При этом:

- Готовый клиент `keeperApi.incarnations.runs(name)` → `GET /v1/incarnations/{name}/runs` (`keeper.ts:541`) **нигде не вызывается**.
- **Live-поток уже есть**: SSE `GET /mcp/events?apply_id=<ULID>` ([`sse.go:145`](../../keeper/internal/mcp/sse.go)); applybus ([`bus.go`](../../keeper/internal/applybus/bus.go)) публикует `task.executed` ([`events_taskevent.go:322`](../../keeper/internal/grpc/events_taskevent.go)) `{apply_id, sid, task_idx, task_status, passage, error?}` и `apply.completed/failed/cancelled` ([`events_runresult.go:248`](../../keeper/internal/grpc/events_runresult.go)). **Потребителя в web нет**: единственный `new EventSource` — `errandRuns.events` (`keeper.ts:996`), stub, никем не вызван. Даже live-страницы Voyage (`VoyageDetail.tsx:586`, `VoyageTargets.tsx:36`) идут **polling**-ом. → наш SSE-консюмер **первый авторизованный** в web (handshake с нуля).
- **История уже пишется** (ключевой факт для Q1b): `task.executed` каждой задачи (Soul + keeper) **персистится в `audit_log`** — [`events_taskevent.go:91`](../../keeper/internal/grpc/events_taskevent.go) (`AuditWriter.Write`, EventType `task.executed`) + keeper-side [`keeper_dispatch.go`](../../keeper/internal/scenario/keeper_dispatch.go); плюс `incarnation.run_completed` с `changed_tasks[]` ([ADR-052 §k](0052-herald-notifications.md)). Таблица `audit_log` (миграция 001) имеет `correlation_id` (= apply_id) **с индексом** `audit_log_correlation_id_idx` и retention `purge_audit_old(max_age)` из `audit.retention_days` ([ADR-022](0022-audit-pipeline.md)). Т.е. ход прогона восстановим из журнала — не хватает только UI-читателя.
- **Что НЕ в PG (apply_runs)**: per-task прогресс не хранится в оперативных runs-таблицах ([ADR-012](0012-keeper-soul-grpc.md); [`runsview.go:13-18`](../../keeper/internal/applyrun/runsview.go)) — там только per-host статус + упавшая задача. **Это отдельное хранилище от `audit_log`** — журнал per-task пишет, оперативная таблица нет.
- incarnation detail **не отдаёт** текущий apply_id (`applying_apply_id` в БД, но не в `IncarnationGetView` [`incarnation_view.go:34-51`](../../keeper/internal/api/handlers/incarnation_view.go)).
- keeper-side `task.executed` в **live-SSE** не публикуется (только в audit; [`keeper_dispatch.go:145-146`](../../keeper/internal/scenario/keeper_dispatch.go)).

**NIM-38.** Две страницы прогонов, разные источники:

- `/runs` → `RunsFeed.tsx` = client-union `voyages+push+errands` (`:159-212`). Сценарных `apply_run` **нет**. Сортировка захардкожена `startedAt DESC`, пагинации нет (LIMIT=50/источник).
- `/incarnation-runs` → `IncarnationRunsList.tsx` = сценарные `apply_run` через `GET /v1/runs`. Серверная пагинация. Сортировки колонок нет.
- Backend `GET /v1/runs` ([`runs.go`](../../keeper/internal/api/handlers/runs.go)) — `status/incarnation/offset/limit`, порядок захардкожен `ORDER BY started_at DESC, apply_id DESC` ([`runsglobal.go:131-133`](../../keeper/internal/applyrun/runsglobal.go)).
- Образец сортировки: серверная `IncarnationsList.tsx` (`sort`/`sort_dir` в query+queryKey, `toggleSort`, `↑↓`); клиентская `SoulsList.tsx` (`sortItems`/`▲▼`).

---

## Решение §A — Live-ход + история прогонов на инкарнации (NIM-37)

### A1. Линковка incarnation → текущий прогон (B)

`GET /v1/incarnations/{name}` отдаёт nullable `applying_apply_id` (колонка `incarnation.applying_apply_id`, пишется в `lockRun`; non-null пока прогон идёт). UI сразу знает apply_id для подписки — без гонки `/v1/runs?status=applying`. «Последний завершённый» — top-1 из `/v1/incarnations/{name}/runs`.

### A2. keeper-side task.executed → applybus (Q2)

keeper-side задачи (`on: keeper`) исполняются in-process в `dispatchKeeperTasks`; `emitKeeperTaskExecuted` пишет только audit. Добавляем **симметричную** публикацию в applybus (рядом с audit-write):

- Прокинуть `ApplyBus *applybus.EventBus` в `scenario.Runner`-deps (тип уже в `grpc.eventStreamHandler.deps.ApplyBus`).
- Payload **идентичной Soul-side формы** ([`events_taskevent.go:301-326`](../../keeper/internal/grpc/events_taskevent.go)): `{apply_id, kind:"task.executed", sid:"keeper", task_idx, task_status, passage}`.
- **Секрет-гигиена как Soul-side**: без `output`/`register`/`message`; на failed — только `error{code,module}`. `MaskSecrets` на SSE-write-path ([`sse.go:327`](../../keeper/internal/mcp/sse.go)) — второй барьер.
- Cluster-mode: `Publish` уже форвардит в Redis shard-канал (cross-Keeper) — доп. кода нет.
- Audit-write не меняется (отдельный источник `changed_tasks`/Tiding).

`apply.started` publisher в каноне отсутствует — для live не обязателен (UI стартует из `applying`, рисует по первому `task.executed`).

### A3. SSE-эндпоинт Operator-API (A, C)

Новый route **`GET /v1/incarnations/{name}/runs/{apply_id}/events`** (`text/event-stream`), поток через `applybus.Subscribe(ctx, applyID)`; frame `event:/id:/data:`, heartbeat 30s, max-lifetime 30min, лимиты стримов (общий SSE-хелпер из `mcp/sse.go` или узкий дубль).

**RBAC** — как `/mcp/events` `authorizeSSE` ([`sse.go:286`](../../keeper/internal/mcp/sse.go)): инициатор прогона ИЛИ `incarnation.get`/`incarnation.history`. Несуществующий/чужой apply_id → **403** (anti-enum). Backend RBAC **не зависит** от способа auth (A0).

**Auth (A0) — РЕШЕНО: fetch-streaming (координатор 2026-07-04).** Backend RBAC/route от способа auth не зависят; web использует `Authorization`-заголовок:

- **fetch-streaming (ВЫБРАНО).** web делает `fetch(url, {headers:{Authorization: Bearer <jwt>}})` и читает `response.body.getReader()` + `TextDecoderStream`, парся SSE-frames вручную. **Токен в URL не нужен** (обычный заголовок, как все запросы) → **нет утечки в логи, нет ротации**. Цена: auto-reconnect и парсинг событий (что EventSource даёт бесплатно) пишутся руками — приемлемо. Backend-route тот же; middleware читает `Authorization` штатно (транспорт `access_token`/`sse-token` не нужен).
- **короткоживущий query-token (ОТБРОШЕНО — для истории).** `EventSource('…/events?access_token=<short-jwt>')` + `POST /v1/sse-token` (TTL ~60s). Отвергнут: токен в URL оседает в reverse-proxy/access-log (security-floor), и TTL 60s ≪ lifetime стрима (до 30 мин) → **ротация** (перевыпуск/переоткрытие) — та самая боль, что отметил юзер. Транспорт `access_token` в каноне (`auth.go:80-108`, `:32`) остаётся, но этим путём не пользуемся.

**Почему не `/mcp/events`**: MCP-плоскость (JSON-RPC tool-call streaming), свой auth-код. Operator-UI в MCP-канал = смешение плоскостей.

### A4. Live-ход (Q1a) — что видно, пока прогон идёт

- **Live per-task**: SSE `task.executed` → бейдж `OK/CHANGED/FAILED/TIMED_OUT` по `task_idx` (+`sid`, keeper-side `sid="keeper"`).
- **Агрегатный статус**: polling `GET /v1/runs`/`RunDetail` (уже) + SSE `apply.completed/failed/cancelled`.
- **Reload пока идёт**: in-memory bus не добирает прошлые live-события → откат на per-host `RunDetail` (polling) + история из audit (§A5). Приемлемо.

### A5. История прогона (Q1b) — восстановить ход недель/месяц спустя

Цель пользователя: открыть завершённый прогон недельной/месячной давности и увидеть детальный ход. **Данные уже персистятся** — реализуем UI-читатель поверх `audit_log`, без нового хранилища.

- **Источник** — `GET /v1/audit?correlation_id=<apply_id>` (эндпоинт есть, [`handlers/audit.go`](../../keeper/internal/api/handlers/audit.go)): `task.executed` каждой задачи (ok/changed/failed, `task_idx`, `sid`, `at`) + `incarnation.run_completed` (`changed_tasks[]`). Даёт полный по-задачный timeline любого прогона в границах retention.
- **Чтение дёшево**: индекс `audit_log_correlation_id_idx` (миграция 001:29-31, partial) → timeline одного прогона = index-scan, не full-scan.
- **Retention настраиваемый**: `purge_audit_old(max_age)` из `keeper.yml → audit.retention_days`. «Месяц назад» = retention ≥ 30 дней (операторская настройка, не хардкод). Задать разумный дефолт + задокументировать.
- **UI**: `RunDetail` для завершённого прогона рисует timeline из audit (в дополнение к live-режиму §A4 для идущего). Один экран, два источника: applybus (идёт) / audit (завершён).

**Нагрузка на БД (пользователь просил трезво):**

- **Запись — нулевая дельта.** `task.executed` пишется в audit **уже сегодня** — Q1b не добавляет ни одного INSERT, только читает.
- **Чтение — дёшево.** Индекс на `correlation_id` уже есть; timeline одного прогона — узкий index-scan.
- **Хранение/масштаб.** Объём audit растёт с прогонами×задачами×хостами. До ~10k VM текущий audit работает без оптимизаций. На **100k VM** audit сам упирается (INSERT-rate, размер таблицы) — это **существующая** проблема ([audit-scaling backlog](0022-audit-pipeline.md): partitioning + retention + hot/cold split), **не создаётся** этой фичей. «Месяц детально» достижим настройкой retention; масштаб 100k наследует общий audit-эпик.

---

## Решение §B — Единый All Runs + сортировка (NIM-38) ✓

### B1. Backend: `sort`/`sort_dir` в `GET /v1/runs`

- `AllRunsInput` ([`runs.go`](../../keeper/internal/api/handlers/runs.go)) += `Sort`, `SortDir`. Whitelist: **`started_at|finished_at|status|incarnation|scenario`**; `sort_dir ∈ asc|desc`; дефолт `started_at`/`desc` (byte-exact). Невалид → **422**.
- `buildRunsQuery`/`ListRuns` ([`runsglobal.go:118-159`](../../keeper/internal/applyrun/runsglobal.go)): `ORDER BY` из whitelist (колонка через `switch` по enum, не сырая строка — SQL-инъекция). **Tie-break `, apply_id DESC`**.
- **total/offset безопасны**: `sort` только в `listSQL` (131-133); `countSQL` (125) не трогается. `finished_at` nullable → `NULLS LAST`.
- Naming — `sort`/`sort_dir` (переиспользование `/v1/incarnations` 1:1). openapi + `npm run gen:api`.

### B2. Web: единый All Runs (Q3) + сортируемые колонки

Одна страница `/runs` со всеми 4 типами; `/incarnation-runs` → редирект; Sidebar — убрать дубль. Архитектура — **типовой фильтр**:

- Сегмент `All | Scenario | Voyage | Push | Errand` (`RunsFeed` уже имеет type-chips — `+scenario`).
- **`Scenario`** (`apply_run`) — `keeperApi.runs.list()`, **серверная** сортировка+пагинация (B1) по паттерну `IncarnationsList` (`sort`/`sort_dir` в query+queryKey, `toggleSort`, `↑↓`, **+ сброс offset** — в образце нет, дописать).
- **`Voyage|Push|Errand`** — client-union, **клиентская** сортировка (`SoulsList` `▲▼`).
- **`All`** — client-union первых N каждого типа (+ 4-й источник apply_runs), client-сортировка; подпись «первые N на тип» (не выдавать усечение за полноту).

Единый server-union-эндпоинт (одним SQL по 4 таблицам) — дорого (разные таблицы/статусы/total), **follow-up**. Текущий дефолт закрывает боль без него.

---

## Разбивка на implementation-слайсы

Конвейер — `developer → review → qa (→ docs-writer)`. S37-1/S38-1 крупные → **architect-разведка pattern**. Треки NIM-37 ∥ NIM-38.

### S37-1 — backend live (NIM-37) · В РАБОТЕ (бэкенд-волна)

- **Файлы:** `incarnation.go`(+`crud.go` SELECT) — `ApplyingApplyID *string`; `incarnation_view.go` — поле в `IncarnationGetView` + проекция; `keeper_dispatch.go` — `publishKeeperTaskExecuted` + `ApplyBus` в `Runner`-deps; новый SSE-route `/v1/incarnations/{name}/runs/{apply_id}/events` (под `RequireJWT`); `vendor/openapi/keeper.yaml`. `POST /v1/sse-token` **НЕ нужен** (A0=fetch-streaming: web шлёт `Authorization`-заголовок; если уже закоммичен в S37-1 — убрать либо оставить мёртвым, на усмотрение координатора).
- **Guard-инварианты:** (1) `applying_apply_id` non-null во время applying, null на терминале; (2) keeper-side публикует `task.executed` с `sid="keeper"`, payload симметричен Soul-side; (3) секрет-гигиена keeper-side SSE (без output/message, failed → `error{code,module}`); (4) SSE-route RBAC deny без `incarnation.get`; чужой apply_id → 403; принимает `Authorization: Bearer` (fetch-streaming).
- **Зависимости:** нет.

### S37-2 — web live (NIM-37)

- **Файлы:** `IncarnationDetail` — секция «Прогоны» (список `incarnations.runs(name)` + live-статус); `api/keeper.ts` — SSE-клиент **fetch-streaming** (`fetch`+`Authorization`-заголовок + `response.body.getReader()`, ручной парсинг SSE-frames + reconnect); `RunDetail` — live per-task бейджи поверх per-host среза.
- **Guard-инварианты:** (1) секция рендерит прогоны всех kind; (2) live-событие обновляет бейдж без reload; (3) **graceful fallback на polling** при обрыве потока; (4) reload не ломает (per-host + история из audit); (5) auth — через `Authorization`-заголовок (токен не в URL).
- **Зависимости:** S37-1. A0=fetch-streaming (решено).

### S37-3 — web история прогона из audit (Q1b) · НОВЫЙ

- **Файлы:** `RunDetail` — для завершённого прогона timeline из `GET /v1/audit?correlation_id=<apply_id>` (`task.executed` + `run_completed`); `api/keeper.ts` — клиент audit-timeline (если ещё нет). Backend: подтвердить дефолт `audit.retention_days` в `keeper.yml` (≥30 дней) + doc. Индекс `correlation_id` уже есть — новых миграций/записи нет.
- **Guard-инварианты:** (1) завершённый прогон N-дневной давности показывает по-задачный ход из audit; (2) прогон за пределом retention — явное «история истекла», не пустой экран; (3) чтение идёт по индексу `correlation_id` (не full-scan) — тест на план запроса/латентность; (4) **ноль новых INSERT** (Q1b только читает).
- **Зависимости:** S37-2 (общий `RunDetail`). Backend audit-endpoint — уже есть.

### S38-1 — backend sort (NIM-38) · В РАБОТЕ

- **Файлы:** `runs.go` (`Sort`/`SortDir` + валидация → 422); `runsglobal.go` (`ORDER BY` из whitelist + tie-break `apply_id DESC` + `NULLS LAST`); openapi.
- **Guard-инварианты:** (1) sort по 5 колонкам asc/desc; (2) tie-break `apply_id DESC`; (3) невалид → 422; (4) `total` независим от `sort`; (5) `finished_at NULLS LAST`; (6) дефолт byte-exact.
- **Зависимости:** нет.

### S38-2 — web консолидация + колонки (NIM-38)

- **Файлы:** `RunsFeed.tsx` — 4-й источник apply_runs + type-фильтр `+scenario` + сортируемые колонки (server для scenario, client для union); `App.tsx` — редирект `/incarnation-runs`→`/runs`; `Sidebar.tsx` — убрать дубль; свести `IncarnationRunsList.tsx`.
- **Guard-инварианты:** (1) сценарные видны в All Runs; (2) клик сортирует (server в scenario — сбрасывает offset; client в union); (3) tie-break; (4) фильтр по типу; (5) редирект; (6) «первые N» помечены.
- **Зависимости:** S38-1.

---

## Что НЕ входит (границы)

- Персистенция per-task в **оперативных runs-таблицах** (`apply_runs`) — не нужна: история идёт из `audit_log` (§A5). ADR-012 (per-task не в apply_runs) остаётся в силе.
- Единый server-union-эндпоинт All Runs (follow-up).
- `apply.started`/`apply.progress` publisher (не нужен для live).
- Оптимизация audit под 100k VM (существующий audit-scaling эпик, не эта фича).
- Рендер keeper-бейджа — **NIM-36** (DONE).
- Cookie-сессия; MCP `/mcp/events` не трогается.

## Открытые вопросы / follow-up

- ~~A0 финал~~ — **РЕШЕНО** (координатор 2026-07-04): fetch-streaming через `Authorization`-заголовок; query-token/`sse-token` отброшены.
- **Server-union All Runs** — когда client-union упрётся в объёмы.
- **Audit-scaling под 100k** — partitioning + retention + hot/cold ([backlog](0022-audit-pipeline.md)); напрямую влияет на глубину «истории прогонов» на больших флотах.
- ~~Per-resource / one-time SSE-token~~ — снято: A0=fetch-streaming, токена в URL нет, вопрос неактуален.

## Регистрация (подтверждение получено 2026-07-04 — осталась механика координатора)

Снять `-DRAFT` → `0069-run-visibility.md`; строка в [README](README.md); стаб в [architecture.md](../architecture.md); [naming-rules](../naming-rules.md) — новых имён нет (`events`/`sort`/`sort_dir` — существующие DevOps-контракты). `make check-doc-links` до коммита.

## Amends / Related

- **[ADR-012](0012-keeper-soul-grpc.md)** — per-task не в оперативных `apply_runs`; **но** per-task ход есть в `audit_log` (§A5 читает его для истории — границы ADR-012 не нарушает).
- **[ADR-022](0022-audit-pipeline.md)** — retention `audit_log` (`purge_audit_old`); глубина «истории прогонов» = `audit.retention_days`.
- **[ADR-027](0027-apply-work-queue.md)** (m-S1) — `applying_apply_id` как read-source линковки (write-path не меняем).
- **[ADR-052 §k](0052-herald-notifications.md)** — audit `task.executed`/`changed_tasks` не трогается; applybus-публикация Q2 — отдельный канал рядом.
- **NIM-36** (DONE) — рендер `sid="keeper"` в бейдж; этот ADR — backend-предпосылка.
