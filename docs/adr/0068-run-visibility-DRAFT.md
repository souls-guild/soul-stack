# ADR-068. Видимость прогонов в Operator UI — live-ход инкарнации + единый All Runs

> **Статус: proposed (DRAFT), НЕ реализовано.** Дизайн-сессия 2026-07-04 (`NIM-37` live-ход + `NIM-38` единый All Runs). Смежный тикет `NIM-36` (рендер keeper-бейджа) — ортогонален, ссылается ниже.
>
> **DRAFT-пометка в имени файла** намеренна: решения Q1/Q2/Q3 — рекомендованные дефолты, ждут подтверждения пользователя. Регистрация в [README](README.md) / [architecture.md](../architecture.md) / [naming-rules](../naming-rules.md) — **после** подтверждения (см. §«Регистрация», в конце). До того ADR висит отдельным файлом и в индекс не вписан.
>
> **Нумерация.** Макс существующий номер — 0067; 0068 свободен ([README](README.md): пропуски 0034/0036/0037 — ниже).

---

## РЕШЕНИЯ (подтвердить / переопределить)

Блок в начале, чтобы можно было дёшево наложить вето до чтения обоснований.

| # | Развилка | Рекомендованный дефолт | Дешёвая альтернатива |
|---|---|---|---|
| **A0** | SSE-auth handshake | **КОРОТКОЖИВУЩИЙ query-token**: authed `POST /v1/sse-token` минтит JWT TTL ~60s, web открывает `EventSource(…?access_token=<short-jwt>)`; транспорт `access_token` уже в каноне (`auth.go:32`), cookie отвергнут. Полный 30-дневный JWT в URL — **не** дефолт (утечка через логи). Детали — под-развилка A / §A3. | Полный operator-JWT в URL (проще, но долгоживущая утечка). |
| **Q1** | Глубина live-хода на инкарнации | **EXPOSE-EXISTING**: live per-task через уже текущее в applybus SSE-событие `task.executed` (несёт `task_status ∈ OK/CHANGED/FAILED/TIMED_OUT` — «изменилось ли» УЖЕ течёт) + пост-фактум changed из audit `incarnation.run_completed`. **НЕ** персистить per-task в PG (уважать [ADR-012](0012-keeper-soul-grpc.md), «горячее→Redis не PG», audit-scaling-backlog). При reload live per-task detail теряется → fallback на per-host срез (`RunDetail`, уже есть) + audit-changed. | Персистить per-task прогресс в PG (амендмент ADR-012) — дороже, противоречит «горячее→Redis». |
| **Q2** | keeper-side задачи (`on: keeper`) в live-SSE | **ПУБЛИКОВАТЬ в applybus**: сейчас keeper-side `task.executed` идёт только в audit ([`keeper_dispatch.go:145-146`](../../keeper/internal/scenario/keeper_dispatch.go) — явный коммент «SSE/applybus НЕ публикуется»), из-за чего keeper-side прогресс на operator-SSE невидим. Спроектировать симметричный Soul-side публикатор. `sid` остаётся синтетический `keeper` ([`render.KeeperTargetSID`](../../keeper/internal/render/dispatch.go)); рендер бейджа — NIM-36. | Оставить keeper-side только в audit — но тогда `on: keeper`-шаги (cloud/vault/registered) «немые» в live, юзер хочет «вообще всё». |
| **Q3** | Форма All Runs | **ЕДИНЫЙ**: одна страница All Runs со всеми типами прогонов (сценарные `apply_run` + `voyage` + `push` + `errand`) + фильтр по типу + сортировка колонок. Сейчас разбито на `/runs` (client-union voyage+push+errand) и `/incarnation-runs` (сценарные `apply_run`) — сценарных в All Runs НЕТ. | Оставить раздельно, добить сортировку только в `/incarnation-runs`. Проще, но не решает «сценарные в All Runs». |

**Под-развилки (тоже дефолты, тоже под вето):**

- **A (SSE-auth) — короткоживущий query-token от authed-эндпоинта.** browser-`EventSource` не шлёт `Authorization`, cookie-сессия отвергнута архитектурой (web `tokenStore.ts:1-8`). **Транспорт уже в каноне**: middleware [`auth.go:80-108`](../../keeper/internal/api/middleware/auth.go) для путей `*/events` (или `Accept: text/event-stream`) берёт токен из query `access_token` (константа `sseQueryTokenParam`, `:32`) и верифицирует тем же JWT-verifier — новый транспортный код не нужен. **Но токен — НЕ долгоживущий operator-JWT из `localStorage`**: 30-дневный JWT в URL оседает в reverse-proxy/access-log = долгоживущая утечка. Вместо этого web дёргает authed (Bearer) минтинг-эндпоинт `POST /v1/sse-token` → короткоживущий JWT (TTL ~60s, тот же signing key → `Verify` принимает без правок middleware), и открывает `EventSource('…/events?access_token=<short-jwt>')`. RBAC доступа к apply_id — на самом SSE-route (`authorizeSSE`). **НЕ** трогать `/mcp/events` (MCP-плоскость). Security-model — §A3.
- **B (линковка incarnation→прогон).** Отдать в `GET /v1/incarnations/{name}` уже пишущуюся колонку `applying_apply_id` (пишется в `lockRun` [`state.go:45`](../../keeper/internal/scenario/state.go), чистится на терминале [`crud.go:849,1197`](../../keeper/internal/incarnation/crud.go)) — non-null ровно пока прогон идёт. «Последний завершённый» прогон UI берёт из top-1 существующего `GET /v1/incarnations/{name}/runs` (не плодить `last_apply_id` в inc-detail).
- **C (эндпоинт live).** Новый route `GET /v1/incarnations/{name}/runs/{apply_id}/events` — симметрия существующему `RunDetail`-пути `/v1/incarnations/{name}/runs/{apply_id}` + прецеденту `/v1/voyages/{id}/events`. Alt: `/v1/runs/{apply_id}/events` (короче, но теряет incarnation-контекст RBAC-резолва).
- **D (одна тема — один ADR).** Оба тикета покрыты одним ADR-0068 (связная тема «видимость прогонов оператору»). Alt: расщепить на 0068 (live) + 0069 (all-runs) — если пользователь предпочитает по тикету на ADR.

---

## Контекст (что видит оператор сейчас)

**NIM-37.** На странице инкарнации **нет таба прогонов вовсе** — таб «History» показывает `state_history` (снапшоты state), а не ход apply-прогонов. Оператор не видит в реальном времени, какой сценарий идёт (create/rerun/day-2/destroy), какая задача выполняется, изменилось ли что-то. При этом:

- Готовый клиент `keeperApi.incarnations.runs(name)` → `GET /v1/incarnations/{name}/runs` (`soul-stack-web/src/api/keeper.ts:541`) **нигде не вызывается**.
- Live-поток **уже есть** на бэкенде: SSE `GET /mcp/events?apply_id=<ULID>` ([`sse.go:145`](../../keeper/internal/mcp/sse.go)); applybus ([`bus.go`](../../keeper/internal/applybus/bus.go)) публикует `task.executed` ([`events_taskevent.go:322`](../../keeper/internal/grpc/events_taskevent.go)) с payload `{apply_id, sid, task_idx, task_status, passage, error?{code,module}}` и `apply.completed/failed/cancelled` ([`events_runresult.go:248`](../../keeper/internal/grpc/events_runresult.go)). **Потребителя в web нет**: единственный `new EventSource` — `errandRuns.events` (`keeper.ts:996`), аспирационный stub, никем не вызывается, endpoint вне openapi. Даже live-страницы Voyage (`VoyageDetail.tsx:586`, `VoyageTargets.tsx:36`) идут **polling**-ом (`refetchInterval`), не SSE. → наш SSE-консюмер будет **первым авторизованным** в web; auth-handshake проектируется с нуля (прецедента нет).
- Per-task прогресс **не хранится в PG** ([ADR-012](0012-keeper-soul-grpc.md); [`runsview.go:13-18`](../../keeper/internal/applyrun/runsview.go)). Единственная per-task деталь в PG — упавшая задача (`failed_task_idx`/`error_summary`). changed-задачи есть пост-фактум в audit `incarnation.run_completed` (`changed_tasks[]`).
- SSE ключуется по `apply_id`; incarnation detail **не отдаёт** текущий apply_id (`applying_apply_id` есть в БД, но не в `IncarnationGetView` [`incarnation_view.go:34-51`](../../keeper/internal/api/handlers/incarnation_view.go)) → UI вынужден угадывать через `/v1/runs?status=applying`.
- keeper-side `task.executed` в SSE **не публикуется** ([`keeper_dispatch.go:145-146`](../../keeper/internal/scenario/keeper_dispatch.go)).

**NIM-38.** Две страницы прогонов с путаными именами и **разными источниками**:

- `/runs` → `RunsFeed.tsx` = client-side UNION `voyages+push+errands` (`:159-212`). Сценарных `apply_run` здесь **нет**. Колонки Type|ID|Target|Status|Started|Finished. Сортировка захардкожена `startedAt DESC` (`:206-210`), пагинации нет (LIMIT=50/источник).
- `/incarnation-runs` → `IncarnationRunsList.tsx` = сценарные `apply_run` через `GET /v1/runs` (+ `/v1/runs/stats`). Серверная offset/limit пагинация. Сортировки по колонкам нет.
- Backend `GET /v1/runs` ([`runs.go` `AllRunsTyped`](../../keeper/internal/api/handlers/runs.go)) принимает `status/incarnation/offset/limit`, **сортировки нет** — порядок захардкожен `ORDER BY started_at DESC, apply_id DESC` ([`runsglobal.go:131-133`](../../keeper/internal/applyrun/runsglobal.go)).
- Образец сортируемых колонок в проекте: серверная `IncarnationsList.tsx` (`sortKey`/`sortDir`/`toggleSort`/`sortArrow`, `sort`/`sort_dir` в query + queryKey); клиентская `SoulsList.tsx` (`sortItems`/`clickSort`/`▲▼`).

---

## Решение §A — Live-ход прогонов на инкарнации (NIM-37)

### A1. Линковка incarnation → текущий прогон (под-развилка B)

`GET /v1/incarnations/{name}` отдаёт новое nullable-поле **`applying_apply_id`** (source: колонка `incarnation.applying_apply_id`, [ADR-027(m-S1)](0027-apply-work-queue.md), пишется в `lockRun`). Non-null ровно пока прогон идёт; на терминале → null. UI, увидев non-null, **сразу** знает apply_id для SSE-подписки — без гонки `/v1/runs?status=applying`.

«Последний завершённый» прогон UI не требует нового поля: top-1 из `GET /v1/incarnations/{name}/runs` (эндпоинт уже есть, порядок `started_at DESC`).

### A2. keeper-side task.executed → applybus (Q2)

keeper-side задачи (`on: keeper`) исполняются in-process в [`dispatchKeeperTasks`](../../keeper/internal/scenario/keeper_dispatch.go); `emitKeeperTaskExecuted` пишет **только** audit. Добавляем **симметричную** публикацию в applybus (рядом с audit-write, не вместо):

- Прокинуть `ApplyBus *applybus.EventBus` в зависимости `scenario.Runner` (сейчас в `r.deps` его нет; тип уже используется в `grpc.eventStreamHandler.deps.ApplyBus`).
- Собрать payload **идентичной Soul-side формы** ([`events_taskevent.go:301-326`](../../keeper/internal/grpc/events_taskevent.go)): `{apply_id, kind:"task.executed", sid: KeeperTargetSID("keeper"), task_idx: rt.Index, task_status: keeperTaskStatus(...).String(), passage}` и `ApplyBus.Publish`.
- **Секрет-гигиена ровно как Soul-side**: в SSE **не** класть `output`/`register_data`/`message` (keeper-задачи несут vault-резолвленный output); на failed — только `error{code,module}`, без `message`. Финальный `MaskSecrets` на SSE-write-path ([`sse.go:327`](../../keeper/internal/mcp/sse.go)) остаётся вторым барьером.
- Cluster-mode: `Publish` уже форвардит в Redis shard-канал → cross-Keeper SSE-подписчик получит keeper-side событие через свой bridge. Никакого доп. кода.
- Audit-write **не меняется** (это отдельный источник свёртки `changed_tasks`/Tiding, [ADR-052 §k](0052-herald-notifications.md)).

`apply.started` publisher в каноне **отсутствует** ([`bus.go:45-48`](../../keeper/internal/applybus/bus.go): «в M0.7.c publisher отсутствует»). Для live-хода он **не обязателен**: UI стартует из состояния `applying` (знает apply_id из A1) и рисует прогресс по первому же `task.executed`. Опциональный эмит `apply.started` в `lockRun`/`run()` — nice-to-have, **не в скоупе** дефолта.

### A3. SSE-эндпоинт Operator-API (под-развилки A, C)

Новый route **`GET /v1/incarnations/{name}/runs/{apply_id}/events`** (Content-Type `text/event-stream`):

- **Auth — короткоживущий query-token (транспорт из канона).** Route под `RequireJWT`-chain; middleware [`extractToken`](../../keeper/internal/api/middleware/auth.go) для `*/events`-путей уже берёт токен из query `access_token`, если нет `Authorization` (транспорт готов). Токен — **не** localStorage-JWT: web сперва дёргает authed `POST /v1/sse-token` → короткоживущий JWT (TTL ~60s), затем `new EventSource('/v1/incarnations/{name}/runs/{apply_id}/events?access_token=<short-jwt>')`. Middleware верифицирует его тем же ключом — правок middleware нет.
- **Security-model (query-token).** Токен в URL — floor ниже header-а (оседает в reverse-proxy/access-log/browser-history). Поэтому: (1) минтуемый токен **короткоживущий** (TTL ~60s — окна на открытие стрима хватает, утечка из логов истекает почти сразу); (2) **HTTPS-only**; (3) RBAC доступа к apply_id проверяется **на SSE-route** (`authorizeSSE`), не зашит в токен — токен лишь удостоверяет оператора. Полный 30-дневный operator-JWT в URL **отвергнут** (долгоживущая утечка). Cookie-сессия **отвергнута** архитектурой (`tokenStore.ts`). Follow-up-усиление — per-resource scope токена (`aud: run:<apply_id>`).
- **RBAC** — тот же принцип, что `/mcp/events` `authorizeSSE` ([`sse.go:286`](../../keeper/internal/mcp/sse.go)): инициатор прогона ИЛИ `incarnation.get`/`incarnation.history` на инкарнации. Несуществующий/чужой apply_id → **403** (anti-enum, parity `/mcp/events`).
- **Поток** — подписка через `applybus.Subscribe(ctx, applyID)`; тот же frame-формат `event: <kind>\nid: <apply_id>\ndata: <json>\n\n`, heartbeat 30s, max-lifetime 30min, лимиты стримов (вынести общий SSE-хелпер из `mcp/sse.go` или продублировать узко — на усмотрение developer/architect).
- **Почему не `/mcp/events`.** `/mcp/events` — MCP-плоскость (JSON-RPC tool-call streaming), авторизуется своим кодом и не читает `access_token`. Тащить web-UI в MCP-канал = смешение плоскостей + придётся дублировать `access_token`-логику в mcp-handler. Operator-API `/v1/*` уже имеет query-token из коробки.

### A4. Глубина (Q1) — что именно видно

- **Live per-task**: SSE `task.executed` → per-task бейдж `OK/CHANGED/FAILED/TIMED_OUT` по `task_idx` (и `sid`, для keeper-side sid=`keeper`). «Изменилось ли» = `task_status`, уже течёт.
- **Агрегатный статус прогона**: существующий polling `GET /v1/runs`/`RunDetail` (уже работает) + SSE `apply.completed/failed/cancelled`.
- **changed пост-фактум** (после reload / для истории): audit `GET /v1/audit?correlation_id=<apply_id>&types=incarnation.run_completed` → `changed_tasks[]`.
- **Reload/degrade**: live per-task detail в памяти теряется (in-memory bus, late-subscriber не добирает прошлые события — [`bus.go:198-200`](../../keeper/internal/applybus/bus.go)). Fallback: per-host срез `RunDetail` (уже есть, polling) + audit-changed. Приемлемо (Q1).

---

## Решение §B — Единый All Runs + сортировка (NIM-38)

### B1. Backend: `sort`/`sort_dir` в `GET /v1/runs`

- `AllRunsInput` ([`runs.go`](../../keeper/internal/api/handlers/runs.go)) += `Sort string`, `SortDir string`. Whitelist поля: **`started_at|finished_at|status|incarnation|scenario`**; `sort_dir ∈ asc|desc`; дефолт `started_at`/`desc` (byte-exact текущее поведение). Невалидное значение → **422** (parity валидации status/incarnation).
- `buildRunsQuery`/`ListRuns` ([`runsglobal.go:118-159`](../../keeper/internal/applyrun/runsglobal.go)): параметризовать `ORDER BY` из whitelist (не из сырой строки — SQL-инъекция; колонка выбирается `switch`-ом по enum). **Tie-break всегда `, apply_id DESC`** (стабильность при равных значениях — напр. одинаковый `status`).
- **total/offset безопасны**: `sort` влияет только на `ORDER BY` в `listSQL` (строка 131-133); `countSQL` (строка 125) не трогается → `total` от сортировки не зависит. `finished_at` nullable (applying-прогоны) → явный `NULLS LAST` (иначе БД-зависимый порядок NULL).
- Naming — знакомые DevOps-термины `sort`/`sort_dir` (переиспользование `/v1/incarnations`-контракта 1:1), не выдуманные.
- openapi (`vendor/openapi/keeper.yaml`) + `npm run gen:api` в web.

### B2. Web: единый All Runs (Q3) + сортируемые колонки

**Консолидация (под-развилка).** Одна страница All Runs (`/runs`), вбирающая **4 типа**; `/incarnation-runs` → редирект на `/runs`; в Sidebar убрать дубль-пункт. Рекомендованная архитектура объединения — **типовой фильтр (B3-hybrid)**:

- Страница держит фильтр по типу (сегмент `All | Scenario | Voyage | Push | Errand`; `RunsFeed` уже имеет type-chips — расширить `+scenario`).
- **`Scenario`** (`apply_run`) — источник `keeperApi.runs.list()` (`/v1/runs`), **серверная** сортировка+пагинация (B1) по паттерну `IncarnationsList` (`sort`/`sort_dir` в query+queryKey, `toggleSort`, `sortArrow ↑↓`, **сброс `offset` при смене сортировки** — в `IncarnationsList` этого нет, дописать).
- **`Voyage|Push|Errand`** — существующие client-union источники, **клиентская** сортировка по паттерну `SoulsList` (`sortItems`/`▲▼`).
- **`All`** — client-union первых N каждого типа (как `RunsFeed` сегодня + 4-й источник apply_runs), клиентская сортировка. Явно `log`/подпись «показаны первые N на тип» (не выдавать усечение за полноту).

Почему не «единый server-union-эндпоинт» (`apply_runs ∪ voyages ∪ push ∪ errands` одним SQL с общей пагинацией): 4 разные таблицы, разные статус-домены, дорогой UNION+total — большой бэкенд-слайс ради обзорной страницы. Оставлен как **follow-up** (см. §Открытые); текущий дефолт закрывает боль (сценарные видны + сортировка) без него.

---

## Разбивка на implementation-слайсы

Каждый слайс: файлы / guard-инварианты (обязательны) / зависимости. Конвейер каждого — `developer → review → qa (→ docs-writer)`. S37-1 и S38-1 — крупные (>5 файлов, узел Keeper↔Soul/applybus) → **architect-разведка pattern до developer**. Треки NIM-37 и NIM-38 независимы (разные страницы) — параллельны.

### S37-1 — backend live (NIM-37)

- **Файлы:** `incarnation/incarnation.go`(+`crud.go` SELECT) — `ApplyingApplyID *string` в domain + читать в `SelectByName`; `api/handlers/incarnation_view.go` — поле в `IncarnationGetView` + `toIncarnationGetView` + api-проекция (`huma_incarnation_reply.go`); `scenario/keeper_dispatch.go` — `publishKeeperTaskExecuted` + `ApplyBus` в `Runner`-deps (+ wiring в server-bootstrap, где Runner собирается); новый SSE-handler + route `/v1/incarnations/{name}/runs/{apply_id}/events` (router под `RequireJWT`) + минтинг-эндпоинт `POST /v1/sse-token` (короткоживущий JWT TTL ~60s, тем же signing key); `vendor/openapi/keeper.yaml`.
- **Guard-инварианты:** (1) inc-detail отдаёт `applying_apply_id` non-null во время applying, null на терминале; (2) keeper-side задача (`on: keeper`) публикует `task.executed` в applybus с `sid="keeper"`, payload byte-симметричен Soul-side; (3) секрет-гигиена: keeper-side SSE-frame не несёт `output`/`register`/`message`, на failed — только `error{code,module}`; (4) SSE-route: RBAC deny без `incarnation.get`; принимает и `Authorization: Bearer`, и `?access_token=`; несуществующий apply_id → 403; (5) `POST /v1/sse-token` требует Bearer, выдаёт JWT c TTL ~60s; истёкший short-token на SSE-route → 401; `access_token` из query читается только на `*/events` (не на mutating-методах — `isSSERequest`).
- **Зависимости:** нет (NIM-36 бейдж — параллельно, не блокирует).

### S37-2 — web live (NIM-37)

- **Файлы:** `IncarnationDetail`-страница — новая секция «Прогоны» (список через `incarnations.runs(name)` + live-статус); `api/keeper.ts` — SSE-клиент `incarnations.runEvents(name, applyId)` (`new EventSource(...?access_token=)`); drill-in `RunDetail` — live per-task (SSE `task.executed` → бейджи по `task_idx`) поверх существующего per-host среза.
- **Guard-инварианты:** (1) секция рендерит прогоны инкарнации всех kind; (2) live SSE-событие обновляет per-task бейдж без reload; (3) **graceful fallback на polling** при отсутствии/обрыве `EventSource` — данные не пропадают; (4) reload не ломает страницу (per-host + audit-changed остаются).
- **Зависимости:** S37-1.

### S38-1 — backend sort (NIM-38)

- **Файлы:** `api/handlers/runs.go` (`AllRunsInput` + `Sort`/`SortDir` + валидация whitelist → 422); `applyrun/runsglobal.go` (`buildRunsQuery`/`ListRuns` — параметризованный `ORDER BY` из enum-whitelist + tie-break `apply_id DESC` + `NULLS LAST` для `finished_at`); `vendor/openapi/keeper.yaml`.
- **Guard-инварианты:** (1) sort по каждой из 5 колонок asc/desc даёт корректный порядок; (2) стабильный tie-break `apply_id DESC` при равных значениях сорт-колонки; (3) невалидный `sort`/`sort_dir` → 422; (4) `total` не зависит от `sort`; (5) `finished_at NULLS LAST` для applying-прогонов; (6) дефолт (без sort-параметров) byte-exact прежнему `started_at DESC, apply_id DESC`.
- **Зависимости:** нет.

### S38-2 — web консолидация + колонки (NIM-38)

- **Файлы:** `RunsFeed.tsx` — 4-й источник (apply_runs через `runs.list`) + type-фильтр `+scenario` + сортируемые колонки (server для scenario-вкладки по паттерну `IncarnationsList`, client для union по `SoulsList`); `App.tsx`/router — редирект `/incarnation-runs`→`/runs`; `Sidebar.tsx` — убрать дубль; удалить/свести `IncarnationRunsList.tsx`.
- **Guard-инварианты:** (1) сценарные `apply_run` видны в All Runs; (2) клик по колонке сортирует (server в scenario-вкладке — уходит в query+queryKey, сбрасывает offset; client в union-вкладке); (3) tie-break стабилен; (4) фильтр по типу отбирает верно; (5) редирект `/incarnation-runs`→`/runs`; (6) «первые N на тип» в All-вкладке помечены как усечение.
- **Зависимости:** S38-1.

---

## Что НЕ входит (границы)

- Персистенция per-task прогресса в PG (Q1 отвергает; амендмент ADR-012 — отдельный эпик при реальном запросе).
- Единый server-union-эндпоинт All Runs (follow-up; текущий дефлот — client-union + типовой фильтр).
- `apply.started` publisher (опционален; не нужен для live-хода).
- Рендер keeper-бейджа в UI — **NIM-36** (этот ADR только гарантирует, что keeper-side события доходят до SSE).
- Cookie-сессия для web (явно отвергнута в `tokenStore.ts`; SSE-auth решается query-token).
- MCP-плоскость `/mcp/events` не трогается.

## Открытые вопросы / follow-up

- **Server-union All Runs** — единый пагинируемый SQL по 4 таблицам с общей сортировкой/total (когда обзорный client-union упрётся в объёмы).
- **`apply.started`/`apply.progress`** — при желании более «плотного» live (long-running задачи внутри одной task — сейчас нет по ADR-012 «без прогресса long-running в MVP»).
- **Per-resource scope SSE-token** — минтинг-эндпоинт может привязывать короткоживущий токен к конкретному apply_id (claim `aud: run:<apply_id>`) вместо общего `sse`-scope; усиление, если общий short-token сочтут широким.
- **One-time SSE-ticket** — токен, инвалидируемый после первого открытия стрима (Redis-nonce); ещё уже, но добавляет stateful-проверку в hot-path.

## Регистрация (ПОСЛЕ подтверждения Q1/Q2/Q3)

При подтверждении: снять `-DRAFT` из имени файла → `0068-run-visibility.md`; строка в [README](README.md) (статус `active`/`proposed`); стаб-ссылка в [architecture.md](../architecture.md); в [naming-rules](../naming-rules.md) — новых имён словаря нет (`events`/`sort`/`sort_dir` — переиспользование существующих DevOps-контрактов, `applying_apply_id` — существующая колонка), правки не требуются. `make check-doc-links` до коммита.

## Amends / Related

- **Related — [ADR-012](0012-keeper-soul-grpc.md)** (per-task не персистится в PG): Q1 явно уважает границу, live-детализация — эфемерная (SSE/in-memory bus), не PG.
- **Related — [ADR-027](0027-apply-work-queue.md)** (m-S1): переиспользует уже пишущуюся `applying_apply_id` как read-source линковки (не меняет write-path).
- **Related — [ADR-052 §k](0052-herald-notifications.md)**: keeper-side audit `task.executed` (свёртка `changed_tasks`/Tiding) не трогается — applybus-публикация Q2 добавляется рядом, отдельным каналом.
- **Related — NIM-36**: рендер синтетического `sid="keeper"` в UI-бейдж; этот ADR — его backend-предпосылка (keeper-side события в SSE).
