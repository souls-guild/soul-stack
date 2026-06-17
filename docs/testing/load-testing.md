# План нагрузочного тестирования Soul Stack

Нормативный **план** (что и как нагружаем, на что смотрим, чем и в каком порядке). Инструмент `soul-legion` построен; **Ф0 + срез Ф1 прогнаны на живом стенде и ИЗМЕРЕНЫ** — фактические числа в разделе [§8 «Измеренные результаты»](#8-измеренные-результаты-ф0--срез-ф1-2026-06-17). Полный ramp до 100k (Ф2) остаётся расчётным.

Load встаёт **отдельным уровнем рядом с L0–L4** ([testing/README.md → Уровни](README.md#уровни)). От L3a/L3b/L3c он отличается целью: те проверяют **корректность** (контракт, реализм apply, HA-кейсы), а load — **пропускную способность и точку обвала (cliff)** под масштабом, который функциональные уровни не создают (тысячи-десятки тысяч стримов, сотни одновременных прогонов).

## 1. Цель

1. **Валидировать расчётные числа sizing.** Таблица [scaling.md → Sizing под 100k VM](../operations/scaling.md#sizing-infrastructure-под-100k-vm-приблизительно) — порядки величин, не замеры; пометка там же: «реальный sizing — по нагрузочным тестам с реальным workload-ом». План закрывает этот разрыв. **Статус:** Ф0 + срез Ф1 ИЗМЕРЕНЫ (N до 1000 коннектов, флот-Voyage на 300 хостов — см. [§8](#8-измеренные-результаты-ф0--срез-ф1-2026-06-17)); полный 100k остаётся **расчётным** до Ф2.
2. **Найти cliff по каждой оси нагрузки** — ступень масштаба, на которой латентность уходит в небо / появляются отказы / начинается reconnect-storm. До cliff-а — рабочая зона, за ним — деградация.
3. **Подтвердить или опровергнуть известные узкие места** из [scaling.md → Узкие места при росте](../operations/scaling.md#узкие-места-при-росте): PG primary CPU на `apply_runs` claim, PG IO на `audit_log` ([known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету)), Redis CPU на SID-lease, OTel-collector drop.
4. **Эмпирически показать архитектурные пробелы**, известные как PLANNED/backlog: отсутствие Shepherd (новый инстанс простаивает после scale-out — [scaling.md → Shepherd](../operations/scaling.md#shepherd--балансировка-нагрузки-при-scale-out)) и cliff audit-INSERT до партиционирования.

**Профиль беты vs план.** Для закрытой малой беты (единицы операторов, флот до сотен хостов — [known-limitations.md](../known-limitations.md)) масштабная ось этого плана **не требуется**: достаточно Ф0 как sanity-валидации расчётных чисел. Полный ramp до 100k — пост-бета backlog (см. §6, Ф2).

## 2. Что тестируем — две оси нагрузки + run

### Ось A — Souls-сторона (стримы)

Подключение **1k / 5k / 10k / 25k / 50k / 100k** stub-агентов, каждый держит долгоживущий `EventStream` (gRPC bidi поверх mTLS, стрим инициирует Soul — [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). Эмулируется: `Hello`-handshake → удержание стрима → периодический heartbeat (gRPC keepalive + app-сообщение обновляют `last_seen_at`, [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)) → `SoulprintReport` → `RunResult` на `ApplyRequest`.

Вопрос оси A: **выдержит ли Keeper / PG / Redis флот N подключённых стримов** — по RAM/горутинам Keeper, числу активных стримов, SID-lease/presence-нагрузке на Redis, `last_seen_at`-flush на PG.

### Ось B — API-сторона (ручки `/v1`)

Нагрузка на операторские ручки `/v1` (**~118 HTTP-операций по ~82 path-ключам** в [openapi.yaml](../keeper/openapi.yaml); домены: `souls` / `incarnations` / `voyages` / `cadences` / `heralds`+`tidings` / `errands` / `push`+`push-providers`+`push-runs` / `oracle`(`decrees`/`vigils`) / `synods` / `roles`+`permissions` / `operators`+`me` / `services` / `audit`+`event-types` / `sigil` / `augur` / `plugins`+`modules`).

Ключевой инвариант оси B: **стоимость многих ручек зависит от размера флота.** Поэтому ось B гоняется **поверх фона из N подключённых stub-souls** (выход оси A), а не на пустом Keeper-е. Примеры флот-зависимых ручек:

- `GET /v1/souls` с фильтром `coven`/`status`/`transport` + pagination — стоимость растёт с числом записей в реестре и presence-резолвом (batch-EXISTS SID-lease в Redis — [scaling.md → Целевой масштаб](../operations/scaling.md#целевой-масштаб-100k-vm)).
- `POST /v1/voyages` — резолвит roster (`on:` + `where:` по флоту): на большом roster — массовый SID-lease check в Redis ([scaling.md → Узкие места](../operations/scaling.md#узкие-места-при-росте), строка «Redis CPU на SID-lease check»). Под Tempo-rate-limit ([config.md → tempo](../keeper/config.md#tempo); `voyage_create` bucket — [observability.md → Tempo](../observability.md#keeper--tempo-per-aid-rate-limiter-write-api-adr-050)) — отдельно проверить, не режет ли лимитер сам нагрузочный профиль.
- `GET /v1/audit` — чтение растущей `audit_log` под фоновой записью прогонов.

### Ось C — Run-нагрузка (Voyage / Cadence по большому флоту)

Самый тяжёлый профиль: **M инкарнаций × N stub-хостов**, прогоняемых через Voyage (разово) и Cadence (по расписанию, спавнит Voyage — [ADR-046](../adr/0046-cadence.md), [conductor.md](../keeper/conductor.md)). Трогает весь горячий путь Keeper-а:

```
render (CEL+text/template, Keeper-side) → apply_runs claim (Acolyte SELECT … FOR UPDATE SKIP LOCKED)
  → dispatch (SendApply в стрим) → RunResult → state-commit (PG-транзакция) → audit-INSERT
```

Это единственная ось, нагружающая Acolyte-пул ([scaling.md → Acolyte-пул](../operations/scaling.md#acolyte-пул)), render-пайплайн и audit-write одновременно. Stub отвечает `RunResult` мгновенно (не применяет реально), поэтому ось C измеряет **Keeper-side throughput оркестрации**, а не реализм apply (реализм — L3b).

## 3. Дизайн harness — три компонента

Harness строится **на фундаменте существующего soulstub** ([`tests/e2e/internal/soulstub/soulstub.go`](../../tests/e2e/internal/soulstub/soulstub.go)): реальный gRPC bidi-mTLS-стрим, горутина-на-стрим (`recvLoop`), `Hello`/`ApplyRequest`→`RunResult`/`ErrandRequest`→`ErrandResult`/`PortentEvent`. Сейчас он под `//go:build e2e` и живёт в `internal/` E2E-пакета — для load его нужно вынести в переиспользуемый load-инструмент.

### Компонент 1 — `soul-legion` (масса стримов)

Имя инструмента — **`soul-legion`** ([naming-rules.md → Soul Legion](../naming-rules.md#soul-legion); метафора «легион» = множество душ). Test-only артефакт: пакет/бинарь `soul-legion` в каталоге `tests/load/` (рядом с `tests/e2e/`/`tests/e2e-live/`/`tests/e2e-k8s/`), **НЕ** поставочный бинарь ([ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) фиксирует только `keeper`/`soul`/`soul-lint`).

- **Фундамент** — `soulstub`: вынести его из `//go:build e2e` в переиспользуемый код, на котором стоит `soul-legion`.
- **Контракт эмуляции (тот же, что у soulstub):** `Hello`/`EventStream`/heartbeat/`SoulprintReport`/`RunResult`. `soul-legion` **НЕ парсит Destiny и НЕ применяет** — это сознательно: ось A/C измеряет нагрузку **на Keeper**, а не реализм apply на хосте (реализм — L3b, real-soul-in-container). Иначе нагрузочный хост сам станет узким местом и измерения будут про него, а не про Keeper.
- **Топология по масштабу:**
  - **1k–10k** — single-process, горутина-на-стрим (прямое расширение текущей soulstub-модели). Цель Ф0.
  - **50k–100k** — распределённо (несколько нагрузочных хостов, каждый держит долю стримов): один процесс не удержит 100k стримов по FD/RAM. Цель Ф2 (backlog).
- **Каждый stub = уникальный SID + mTLS-leaf под этот SID** (как настоящий Soul; soulstub уже принимает `cert/key/caBundle` на `New`). Генерация массы leaf-ов под нагрузку — отдельная подзадача harness-а (batch-issue из dev-CA, не Vault-per-leaf на горячем пути генерации).

**Как запускать** (на поднятом dev-стенде) — одной командой `make stress` (алиас `load-test`); профиль нагрузки задаётся ENV-переменными (`COUNT`/`RAMP`/`API`/`VOYAGE`/…). Таргет сам собирает бинарь, минтит admin-JWT для осей B/C (механизм `make dev-jwt`) и чистит легион из реестра на выходе. Список переменных и примеры — [`tests/load/README.md`](../../tests/load/README.md).

### Компонент 2 — API-нагрузчик (ось B)

- **Инструмент:** k6 / vegeta (внешний HTTP-load по сценариям, JWT-auth) **либо** custom Go-нагрузчик. Развилка — на этапе реализации; для флот-зависимых ручек с резолвом roster (`POST /v1/voyages`) custom Go удобнее (точный контроль над телом запроса и распределением roster-размеров).
- **Профиль по доменам:** отдельный набор для read-тяжёлых (`GET /v1/souls`, `GET /v1/audit`, `GET /v1/incarnations`) и write-тяжёлых (`POST /v1/voyages`, `POST /v1/cadences`). Каждая ручка — со своим целевым RPS и распределением параметров (размер фильтра, размер roster, глубина pagination).
- **Поверх фона оси A:** API-нагрузчик стартует, когда уже подняты N stub-souls компонентом 1 — иначе флот-зависимые ручки измеряются на пустом реестре и дают нерелевантные числа.

### Компонент 3 — Run-нагрузчик (ось C)

- **M инкарнаций × N stub-хостов**, спавн Voyage (разово, `POST /v1/voyages`) и Cadence (расписание, спавнит Voyage Conductor-лидером — [conductor.md](../keeper/conductor.md)).
- **Использует stub-генератор компонента 1** как пул хостов (они и отвечают `RunResult`).
- **Контроль профиля:** число одновременных прогонов, размер roster на прогон, частота спавна Cadence, режим overlap (`skip`/`queue`/`parallel` — [ADR-046](../adr/0046-cadence.md)). Под нагрузкой особенно интересен `parallel` (наложение волн) — стресс Acolyte-пула и render-пайплайна.

## 4. На что смотреть — метрики и наблюдательные пробелы

Базис — **существующие** `keeper_*`/`soul_*`-метрики из [observability.md → Каталог метрик](../observability.md#41-каталог-метрик-наполняется-по-подсистемам). Новых метрик ради load-теста не вводим (это бы ушло за propose-and-wait); меряем тем, что уже инструментировано, а 4 пробела — снаружи.

### 4.1. Метрики (есть в коде)

| Что меряем | Метрика | Ось |
|---|---|---|
| Активные EventStream-стримы | `keeper_grpc_streams_active` (gauge) | A |
| App-сообщения стрима по направлению | `keeper_grpc_messages_total{direction}` | A |
| Dispatch `ApplyRequest` (ok/failed) | `keeper_grpc_apply_dispatch_total{result}` | C |
| Онбординг-rate через `Bootstrap` | `keeper_grpc_bootstrap_total{result}` | A (ramp-up) |
| Латентность HTTP-ручек p50/p99 | `keeper_http_request_duration_seconds` (histogram, route-pattern path) | B |
| In-flight HTTP | `keeper_http_in_flight_requests` (gauge) | B |
| Render-латентность (самая тяжёлая Keeper-фаза) | `keeper_render_duration_seconds` p50/p99 + `keeper_render_errors_total` | C |
| Длительность/исход прогона scenario | `keeper_scenario_run_duration_seconds` + `keeper_scenario_runs_total{result}` | C |
| Vault-резолв секретов на render | `keeper_vault_read_duration_seconds{mount}` + `_errors_total` | C |
| Conductor-спавн Cadence | `keeper_conductor_spawn_duration_seconds` + `_spawned_total` + `_spawn_errors_total` | C |
| Rate-limiter write-API | `keeper_tempo_allowed_total` / `keeper_tempo_rejected_total{endpoint}` | B |
| RBAC-проверки (горячий путь API) | `keeper_rbac_checks_total{result}` | B |
| Soul-side apply-цикл (на stub не наполнится; для real-soul-варианта) | `soul_apply_*` | C |
| Reaper-лидер (фон под нагрузкой) | `keeper_reaper_lease_held` | A/C |

### 4.2. Наблюдательные пробелы — метрик НЕТ, мерить снаружи (на 1-й фазе)

Эти 4 величины критичны для cliff-анализа, но Prometheus-collector-ов под них нет — на Ф0/Ф1 снимаем внешними средствами (CLI/exporter), не дожидаясь инструментации:

| Пробел | Почему важен | Чем мерить снаружи |
|---|---|---|
| **Redis SID-lease / presence rate** | Ось A/B упирается сюда (presence-резолв `GET /v1/souls`, roster `POST /v1/voyages`) | `redis-cli INFO commandstats` / `redis-cli --stat`, latency на `EXISTS`-batch; Redis CPU |
| **PG `apply_runs` claim латентность** | Прямое узкое место ([scaling.md](../operations/scaling.md#узкие-места-при-росте)); `SELECT … FOR UPDATE SKIP LOCKED` на primary | `pg_stat_statements` по claim-запросу; lock-wait |
| **PG `audit_log` INSERT-rate** | Известный отложенный cliff ([ADR-022](../adr/0022-audit-pipeline.md), [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету)): до партиционирования упрётся в INSERT-rate/size | `pg_stat_statements` INSERT-rate, рост размера таблицы, IO-wait |
| **Conclave live-count** | Координация HA / refuse-guard / будущий Shepherd; collector `keeper_conclave_*` отсутствует ([scaling.md → Conclave](../operations/scaling.md#conclave--presence-keeper-инстансов)) | `redis-cli KEYS 'keeper:instance:*'` + TTL |

Дополнительно снаружи: **PG connection pool** (wait-time / насыщение — узкое место под параллельными Acolyte+API+claim), **RAM/горутины Keeper** (`/metrics` go-runtime collectors + `pprof`), **OTel-collector dropped spans** (логи коллектора).

### 4.3. Что покажет архитектурные пробелы

- **Shepherd-rebalance (НЕ реализован).** Тест scale-out под нагрузкой оси A: добавить Keeper-инстанс при N держащихся стримах → новый инстанс **простаивает** (`keeper_grpc_streams_active` на нём ≈0 до естественного churn — [scaling.md → Shepherd](../operations/scaling.md#shepherd--балансировка-нагрузки-при-scale-out)). Тест это зафиксирует количественно (время до перебалансировки / её отсутствие).
- **Leader-election** (Reaper / Conductor / Toll) под нагрузкой и при kill-leader: ровно один держатель lease (`sum(keeper_reaper_lease_held)==1`, `sum(keeper_conductor_lease_held)==1`).

## 5. Критерии cliff

**Ramp 1k → 100k по оси A** (и пропорционально M×N по оси C). На **каждой ступени** фиксируем срез:

- `keeper_http_request_duration_seconds` p99 по ключевым ручкам (ось B);
- `keeper_grpc_streams_active` (фактически удержанные vs целевые);
- Redis CPU + SID-lease rate (§4.2);
- PG CPU + `apply_runs` claim латентность + audit-INSERT-rate (§4.2);
- PG connection-pool wait;
- RAM / число горутин Keeper.

**Cliff = ступень**, на которой выполняется любое из:

- **p99 уходит в небо** — латентность ручки/render/dispatch скачком растёт (не линейно);
- **failed-rate растёт** — `keeper_grpc_apply_dispatch_total{result=failed}` / `keeper_scenario_runs_total{result=failed}` / HTTP 5xx появляются под штатным профилем;
- **reconnect-storm** — `soul_eventstream_reconnects_total` лавиной (для real-soul-варианта) либо массовое падение `keeper_grpc_streams_active` с немедленным восстановлением (стримы рвутся и переподключаются по кругу).

Зафиксированный cliff по каждой оси — **измеренная граница**, заменяющая расчётные числа в [scaling.md](../operations/scaling.md#sizing-infrastructure-под-100k-vm-приблизительно). Ступень до cliff-а — supported-зона для данной инфра-конфигурации.

## 6. Фазирование

| Фаза | Объём | Масштаб | Срок | Инфра |
|---|---|---|---|---|
| **Ф0** | Вынос soulstub → `soul-legion` (компонент 1); ramp single-process; sanity-валидация расчётных чисел | 1k–10k | ~1–2 дня | локальный dev-стек (PG/Redis/Vault через docker-compose) |
| **Ф1** | Ось B (API-нагрузчик) + ось C (run-нагрузчик) поверх фона из N stub-souls; снятие 4 наблюдательных пробелов снаружи | до 10k фон + API/run | ~2–3 дня | локальный / single dedicated-хост |
| **Ф2** | Распределённый генератор; полный ramp до cliff | 50k–100k | ≥1 неделя | **prod-grade инфра + бюджет** (несколько нагрузочных хостов, dedicated PG/Redis-cluster) |

- **Ф0** — минимум, валидирующий расчётные sizing-числа и сам harness; единственная фаза, нужная для малой беты.
- **Ф1** — полноценная нагрузка на фоне флота; даёт первые cliff-числа по API и run на умеренном масштабе.
- **Ф2** — **БЭКЛОГ.** Активировать **вместе с** реализацией audit-партиционирования ([ADR-022](../adr/0022-audit-pipeline.md)) и Shepherd ([scaling.md → Shepherd](../operations/scaling.md#shepherd--балансировка-нагрузки-при-scale-out)) — это один режим работы «крупный флот»: тестировать масштаб 100k без этих двух подсистем = заведомо упереться в известные пробелы. Для закрытой малой беты Ф2 **не нужна**.

## 7. Что в бэклоге (вне этого плана)

- **Ф2 целиком** (50k–100k распределённо) — см. §6.
- **Метрики под 4 наблюдательных пробела** (§4.2): `keeper_conclave_*`, явные collector-ы Redis-lease-rate / PG-pool / audit-INSERT-rate. Их введение — отдельный slice с propose-and-wait по именам (новые метрики = расширение каталога [observability.md](../observability.md#41-каталог-метрик-наполняется-по-подсистемам)), не часть load-плана.
- **`soul-legion`-генератор** (§3, компонент 1) — имя зафиксировано ([naming-rules.md → Soul Legion](../naming-rules.md#soul-legion)); постройка инструмента (вынос soulstub из `//go:build e2e` + ramp single-process) — Ф0, ещё не реализована.
- **Реальный-soul нагрузочный вариант** (real `soul`-бинарь вместо stub) — за рамками: stub намеренно не применяет, чтобы мерить Keeper, а не хост. Реализм apply под нагрузкой — отдельная задача на базе L3b ([testing/README.md → L3b](README.md#уровни)).
- **CI-интеграция load-прогона** — не в `make check` (docker-зависим, дорог по времени и ресурсам); по аналогии с L3a/L3b/L3c — отдельный on-demand таргет.

## 8. Измеренные результаты (Ф0 + срез Ф1, 2026-06-17)

Первый фактический прогон `soul-legion`: Ф0 (ось A до 1000 коннектов) + срез Ф1 (оси B/C на фоне флота). Числа ниже — **измеренные**, не расчётные; ими заменяется framing «sizing — расчётные» для соответствующего масштаба. Коммиты: `91963e8` (Ф0 — soul-legion stub-генератор, validated 1k), `5c88b8d` (срез Ф1 — API + Voyage нагрузка, validated 300).

**Методика.** `soul-legion` на живом dev-стенде: один Keeper (event-stream `:9443`, metrics `:9090`, API `:8080`), dev-PKI (batch-issued mTLS-leaf под каждый фейк-SID), **9 реальных Souls фоном**. Ось A — ramp до N=1000 коннектов в single-process (горутина-на-стрим). **НЕ 100k** — полный ramp остаётся Ф2/бэклогом (§6); цель прогона — sanity-валидация расчётных порядков и самого harness-а на умеренном N.

### 8.1. Ось A — коннекты (N=1000)

- `keeper_grpc_streams_active` = **1009** (9 реальных + 1000 фейковых) — все целевые стримы удержаны.
- Латентность connect: **p50 64 ms / p99 130 ms**, **0 ошибок** на ramp-up.
- **Drain:** после отключения легиона `streams_active` вернулся к baseline — **утечки стримов/горутин нет**.
- **RAM Keeper:** RSS **78 → 195–229 MiB** при +1000 коннектов → **дельта ≈ 0.12–0.15 MiB/душу**.
- **Горутины/FD:** ≈ **9 горутин/душу**, ≈ **2.3 FD/душу**.

**Экстраполяция на 100k по ДЕЛЬТЕ: ≈ 15–19 GiB RSS** — в пределах бюджета [scaling.md → Sizing под 100k VM](../operations/scaling.md#sizing-infrastructure-под-100k-vm-приблизительно) (3×8 GB; при 3+ инстансах горутины и стримы делятся между ними).

> **★ Оговорка по per-soul RSS.** Брать **дельту** на бо́льшем N, а не абсолют. На малых N (например 300) абсолютный RSS/душу завышен base-overhead-ом Keeper-процесса (≈ 0.46 MiB/душу → ложная экстраполяция 45–53 GiB@100k). Честная цифра — приростная дельта на N=1000 (≈ 0.12–0.15 MiB/душу). **Точный per-soul под 100k — задача Ф2** (только реальный ramp до cliff даёт верный коэффициент: на больших N в игру входят PG/Redis-резолв, presence-batch, GC-давление).

### 8.2. Ось B — API под N souls

Поверх фона из подключённых stub-souls:

- **Read-путь — `GET /v1/souls`:** **3811 req/s**, p50 **2.9 ms** / p99 **5.9 ms**, **0 ошибок** → read-ручки держат с большим запасом на этом масштабе.
- **Write-like — `POST /v1/voyages/preview`:** **упёрся в Tempo rate-limiter (HTTP 429)** на уровне ≈ **10 rps**. Bucket `voyage_create` (dev-default `rate=10` / `burst=20`, **per-AID** — [config.md → tempo](../keeper/config.md#tempo), [observability.md → Tempo](../observability.md#keeper--tempo-per-aid-rate-limiter-write-api-adr-050)) делит preview и create.

> **★ Находка (отвечает на вопрос §2: «не режет ли лимитер сам нагрузочный профиль»).** **Да** — write-профиль на одном AID режется на ≈ 10 rps лимитером, а не Keeper-ом. Для полноценного Ф1-замера write-ручек: **(а)** распределять нагрузку по нескольким AID, **(б)** поднять `tempo.voyage_create` в dev-профиле, или **(в)** завести отдельный bucket для read-like `preview` (preview = dry-resolve без audit/persist, по нагрузочному профилю он ближе к read, чем к create). Без развязки этого лимита write-API под нагрузкой измерить нельзя — это перенесено в Ф2.

### 8.3. Ось C — Voyage по флоту

command-Voyage по `coven=legion`, **scope_size=300**:

- **create 21 ms**, **end-to-end 2.82 s**, исход **succeeded 300 / failed 0** (300 `ErrandRequest` → 300 `ErrandResult`).
- **Audit:** ≈ **602 INSERT** на один Voyage по 300 хостам (≈ 2/хост: `errand.invoked` + `errand.completed`).

> **★ Подтверждение.** Audit-INSERT растёт **линейно с размером флота** — прямое эмпирическое подтверждение отложенного cliff-а до партиционирования ([ADR-022](../adr/0022-audit-pipeline.md), [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету)). На 300 хостах это ещё далеко от обвала, но коэффициент роста зафиксирован.

### 8.4. Подтверждено vs остаётся на Ф2

| Подтверждено (измерено) | Остаётся расчётным / на Ф2 (бэклог) |
|---|---|
| Read-API держит с запасом (3811 req/s, p99 5.9 ms на `GET /v1/souls`) | Точный per-soul RSS под 100k (нужен реальный ramp до cliff) |
| Порядок RSS по **дельте** в бюджете scaling.md (≈ 15–19 GiB@100k) | Реальный cliff по каждой оси (на N=1000/300 не достигнут) |
| Утечки стримов/горутин нет (drain → baseline) | Write-API под нагрузкой — **после развязки Tempo-лимита** (§8.2) |
| Audit-INSERT линеен по флоту (≈ 2/хост) | 4 наблюдательных пробела §4.2 под полным масштабом |

## См. также

- [testing/README.md](README.md) — уровни L0–L4; load встаёт рядом как отдельный уровень.
- [operations/scaling.md](../operations/scaling.md) — расчётная таблица sizing, узкие места, Shepherd/Conclave/Acolyte.
- [observability.md](../observability.md) — каталог `keeper_*`/`soul_*`-метрик (база для §4).
- [`tests/load/README.md`](../../tests/load/README.md) — как запускать `make stress` (ENV-переменные, что меряет, предусловие).
- [`tests/e2e/internal/soulstub/soulstub.go`](../../tests/e2e/internal/soulstub/soulstub.go) — фундамент stub-генератора (компонент 1).
- [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету) — отложенный audit-cliff (контекст §4.2 / Ф2).
