# ADR-022. Audit-pipeline: storage, schema, retention

- **Контекст.** К моменту фиксации ADR-014 / ADR-020 / ADR-021 в системе уже **называются** конкретные audit-event-ы — `operator.created` / `operator.revoked` ([ADR-014(e)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), `policy_violation` для нарушений `side_effects` ([ADR-020(g)](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)), `config.reload_succeeded` / `config.reload_failed` ([ADR-021(g)](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). Каталог имён открыт в [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events). **Не зафиксировано:** общая нормировка pipeline-а — где audit-событие физически хранится, какая у него schema, кто его пишет (write-path), как живёт корреляция с другими подсистемами (OTel, FK на оператора, apply-цепочки), как чистится по retention. Без этой нормировки имплементация `shared/audit` будет угадывать поведение, а `GET /v1/audit` нельзя добавить в Operator API.

  Audit-trail — это compliance-фундамент (SOC2 / ISO 27001 — изменения identity / authn / authz обязаны журналироваться), инструмент расследования инцидентов («кто и когда менял role X?»), и debugging-канал (агрегация `TaskEvent` / `RunResult` от Soul в один Keeper-уровень). [ADR-014(e)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) уже зафиксировал инвариант FK-полей `created_by_aid` / `changed_by_aid` («снимок кто»); audit-log даёт «снимок когда + что».

- **Решение.**

  **(a) Storage — Postgres-таблица `audit_log`.** Single source of truth — таблица в общей Postgres-БД Keeper-кластера ([ADR-005](0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)). Не append-only-файл, не отдельный сервис (Elasticsearch / Kafka), не shared filesystem. Аргументы: Keeper уже зависит от Postgres, не плодим storage; HA-кластер ([ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) естественно shared через PG, audit виден глобально без cross-host-координации; запросы — стандартный SQL под Operator API. Схема ([keeper/storage.md](../keeper/storage.md) дополняется):

  ```sql
  CREATE TABLE audit_log (
    audit_id        TEXT        PRIMARY KEY,                       -- ULID (sortable timestamp prefix)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    event_type      TEXT        NOT NULL,                          -- <area>.<action>
    source          TEXT        NOT NULL,                          -- enum, см. (b)
    archon_aid      TEXT        REFERENCES operators(aid) ON DELETE SET NULL,  -- nullable; audit-trail сохраняется при удалении operator-а
    correlation_id  TEXT,                                           -- ULID, optional
    payload         JSONB       NOT NULL DEFAULT '{}'::jsonb
  );

  CREATE INDEX idx_audit_event_type   ON audit_log (event_type, created_at);
  CREATE INDEX idx_audit_aid          ON audit_log (archon_aid, created_at) WHERE archon_aid IS NOT NULL;
  CREATE INDEX idx_audit_correlation  ON audit_log (correlation_id)         WHERE correlation_id IS NOT NULL;
  ```

  `audit_id` — ULID (text), а не UUID и не bigint-autoincrement: timestamp-prefix даёт стабильный chronological order без зависимости от `created_at`-clock-skew между Keeper-инстансами, при этом random-component даёт глобальную уникальность без координации между инстансами (см. (e)). Все поля snake_case (как `operators` / `souls` / `bootstrap_tokens`). `payload` хранит kind-specific полезную нагрузку (`changed_paths`, `validation_errors[]`, и т.п.); типизация payload — per-event-type, нормируется в `docs/naming-rules.md → Audit-events` по мере наполнения каталога.

  **(b) `source` — closed enum, 5 значений MVP.** Категория источника события — кто его инициировал:

  | Значение | Когда |
  |---|---|
  | `signal` | SIGHUP file-edit-path hot-reload ([ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). |
  | `api` | HTTP/JSON Operator API request от Архонта. |
  | `mcp` | MCP-tool вызов от Архонта (LLM-агент). |
  | `keeper_internal` | Внутренняя инициатива Keeper-а (Reaper, scheduled tasks, bootstrap). |
  | `soul_grpc` | События от Soul через gRPC EventStream ([ADR-012](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)), forwarded Keeper-ом. |

  Closed enum: расширение — propose-and-wait в [`docs/naming-rules.md`](../naming-rules.md). Не freeform-строка — это нужно для индексирования (фильтр `WHERE source = 'api'`), и для типизированного RBAC-фильтра `GET /v1/audit` (отдельной задачей).

  **(c) `correlation_id` — ULID.** Один и тот же id связывает audit-event с downstream-событиями (другими audit-event-ами, OTel spans, `apply_id` / `RunResult`).

  Выбран ULID (text), а не UUID и не OTel trace-id:

  - sortable timestamp prefix — фильтр по времени без `created_at` join.
  - compact (26 chars).
  - уже используется в проекте (`apply_id` в `RunResult` / `TaskEvent`, `audit_id`).
  - OTel trace-id — другой concept (span tree вокруг одного RPC-вызова), не business-level корреляция; audit-pipeline должен переживать сброс OTel-цепочки.

  Когда выставляется:
  - **API/MCP-driven action** → HTTP-middleware / MCP-handler генерирует `correlation_id` при входе вызова, прокидывает в downstream-context, все audit-events одной цепочки пишут одинаковый id.
  - **Soul gRPC events** → `apply_id` reuse как `correlation_id` (одна цепочка `RunResult` ↔ N `TaskEvent` ↔ `apply.started` audit-event связывается).
  - **`signal` / `keeper_internal`** → fresh ULID per-reload-attempt / per-Reaper-cycle. Поле опциональное (`NULL` допустим для одиночных самостоятельных событий).

  **(d) Retention — через новое Reaper-правило `purge_audit_old`.** Чистка `audit_log` встраивается в существующий механизм Жнеца ([reaper.md](../keeper/reaper.md), [ADR-006(d)](0006-cache-redis.md#adr-006-кэш-и-координация--redis)) — не отдельный воркер, не cron внутри `shared/audit`. Новое правило в `keeper.yml → reaper.rules`:

  ```yaml
  purge_audit_old:
    enabled: true
    max_age:  365d            # default
    action:   delete
  ```

  `action: delete` (по таблице грамматики правил — [reaper.md → Структура правила](../keeper/reaper.md#структура-правила)), обязательно `max_age`. Точное значение `max_age` подбирается под compliance-требования инсталляции через hot-reload.

  **(e) Cross-host (HA-кластер).** Каждый Keeper-инстанс пишет в shared `audit_log` независимо. Уникальность через `audit_id` ULID (random-component защищает от коллизий без координации). Чтение для `GET /v1/audit` — стандартный SQL-запрос с pagination; все инстансы видят одинаковую global-картину без cross-host-механизмов.

  **(f) OTel dual-write — опциональный, default включён.** Audit-event пишется и в Postgres (durable, source of truth), и в OTel span как attribute (transient, для distributed tracing). Postgres — авторитативный источник, OTel — debugging aid и cross-cutting корреляция (Архонт-вызов → Keeper-событие → Soul-вызов в одном trace tree).

  Контролируется через `keeper.yml → audit.otel_export: bool` (default `true`, см. [config.md → audit](../keeper/config.md#audit)). Soul-side **отдельного** audit-блока в `soul.yml` не получает: Soul физически не имеет доступа к Postgres `audit_log` (изоляция — [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Audit-события Soul-стороны идут через Keeper как `source: soul_grpc` (см. (g)); OTel-attributes Soul пишет в свой gRPC EventStream span штатно через `otel:` блок `soul.yml`.

  **(g) Write-path — кто пишет audit.** Закрытый набор инициаторов внутри Keeper-процесса (все пишут в одну `audit_log` через общий helper `shared/audit`):

  | Инициатор | `source` | Когда |
  |---|---|---|
  | HTTP-middleware Operator API | `api` | На каждый authenticated request (включая read-only); permission-deny → `*.access_denied` audit-event. |
  | MCP-handler | `mcp` | То же для MCP-tool вызовов. |
  | Reaper | `keeper_internal` | На каждое action правила (`reaper.purge_souls.executed`, `reaper.expire_pending_seeds.executed`, и т.п. — конкретные имена нормируются по факту имплементации правил). |
  | Hot-reload pipeline | `signal` или `api` | `config.reload_succeeded` / `config.reload_failed` ([ADR-021(g)](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). |
  | `keeper.cloud` | `api` (если инициатор Архонт через scenario) или `keeper_internal` | `cloud.vm.created` / `cloud.vm.destroyed` ([ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)). |
  | `keeper.push` | `api` или `keeper_internal` | `push.applied` / `push.failed`. |
  | Bootstrap (`keeper init`) | `keeper_internal` | Первый `operator.created` с `archon_aid: NULL` ([ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). |
  | Soul gRPC events forwarded by Keeper | `soul_grpc` | `TaskEvent` / `RunResult` / `SoulprintReport` принимаются Keeper-ом и перекладываются в audit с reuse `apply_id` как `correlation_id`. |

  **(h) Каталог event-types.** Convention имени — `<area>.<action>` (lowercase, dots), как у RBAC permissions ([rbac.md → Формат permissions](../keeper/rbac.md#формат-permissions)). Каталог открытый и расширяется обычным PR в [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events) **по факту имплементации подсистемы** — не пытаемся перечислить все 50+ имён заранее. К моменту фиксации ADR-022 в каталог наполнены принципы (5 категорий по `source` enum) и примеры по областям (`config.*`, `operator.*`, `incarnation.*`, `push.*`, `cloud.*`, `reaper.*`, `task.*`).

  **(i) Конфиг блок `audit:` в `keeper.yml`.** Три поля, все reload-able без рестарта:

  ```yaml
  audit:
    enabled:        true        # default
    otel_export:    true        # default — дублировать в OTel spans
    retention_days: 365         # default; alias на reaper.rules.purge_audit_old.max_age
  ```

  Нормативная типизация — [config.md → audit](../keeper/config.md#audit). `retention_days` в `audit:` — alias на `reaper.rules.purge_audit_old.max_age` (нормированный текущим ADR, см. (d)): один источник правды на retention, удобная читаемая форма в дне в `audit:` и `duration` в `reaper:`. В `soul.yml` audit-блока **нет** — обоснование см. (f).

  **(j) Read API `GET /v1/audit` — отдельной задачей.** В этом ADR не нормируется: имплементация Operator API extension при первом реальном запросе. Permission — `audit.read` (одно имя), фильтры — `event_type` / `aid` / `correlation_id` / `date_range`, pagination — стандартная для Operator API. Добавление endpoint-а к Operator API + permission в [rbac.md → Каталог permissions](../keeper/rbac.md#каталог-permissions) — без breaking changes ([ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) допускает расширение без breaking).

  **(k) PII / Secret masking — общий механизм.** Те же правила, что нормированы в [operator-api.md → Secret masking](../keeper/operator-api.md#secret-masking-в-логах-и-трейсах): JWT-токены / private_key / password / credentials_ref / Vault-ref-значения маскируются `***` перед записью в `payload` или в OTel span attributes. Конкретный список masked-полей — общий middleware в `shared/audit` (поверх той же таблицы, что использует OTel-exporter Operator API) — отдельная задача security перед релизом. Audit-pipeline не вводит **собственного** списка masked-полей — берёт его из общего реестра в `shared/`.

- **Consequences.**
  - **Postgres-таблица `audit_log`** — обязательный реестр, schema нормирована в (a). [keeper/storage.md](../keeper/storage.md) дополняется (раздел Postgres-таблиц). Миграция — отдельная задача при имплементации первого audit-инициатора.
  - **Новое Reaper-правило `purge_audit_old`** — [reaper.md](../keeper/reaper.md) (таблица правил + структура + пример). Параметризуется через `keeper.yml → reaper.rules`.
  - **Новый блок `audit:` в `keeper.yml`** — [config.md → audit](../keeper/config.md#audit). Три поля, все reload-able. Summary per-block reload-policy таблица в [config.md → Hot-reload](../keeper/config.md#hot-reload) дополняется строкой.
  - **`shared/audit`-пакет** — общий helper для всех write-path инициаторов (HTTP-middleware, MCP-handler, Reaper, hot-reload, `keeper.cloud`, `keeper.push`, bootstrap, Soul-event forwarder). Insert в `audit_log`, OTel-dual-write по `audit.otel_export`, secret-masking. Конкретный публичный API — отдельная задача имплементации Tier 2.
  - **`GET /v1/audit` + permission `audit.read`** — отдельной задачей при имплементации Operator API extension (см. (j)).
  - **Каталог event-types в `docs/naming-rules.md → Audit-events`** — расширяется обычным PR при добавлении новой подсистемы (полный каталог имён в этом ADR не нормируется, см. (h)).
  - **Cross-link sync.** ADR-014(e) / ADR-020(g) / ADR-021(g) — упоминают, что общая нормировка audit-pipeline — здесь.
  - `examples/keeper/keeper.yml` — обновляется отдельной мини-задачей (включить `audit:` блок).

- **Trade-offs.**
  - **Postgres-only vs отдельный audit-store.** Выбран Postgres: одна зависимость, SQL-query из коробки, нет нового operational компонента. Растущий объём (365 дней × все подсистемы × HA-кластер) — со временем upcoming концерн; mitigation — archive partition в отдельную БД post-MVP без breaking changes (column-layer schema стабильна, `audit_log` можно реплицировать в data lake фоновым worker-ом). Альтернатива «писать сразу в Elasticsearch / S3 / Kafka» отвергнута: на MVP overengineering, на роста — будет миграция, не breaking.
  - **OTel dual-write vs only-Postgres.** Выбран опциональный dual-write (default включён): distributed tracing нужен для cross-cutting корреляции (Архонт → Keeper → Soul в одном trace), но Postgres-запись — авторитет (OTel-коллектор может быть unavailable, audit это переживёт). Отключаемо через `audit.otel_export: false` — для инсталляций без OTel-инфраструктуры. Цена — двойная запись на каждое событие; mitigation — OTel-export асинхронный, не блокирует hot-path.
  - **Single shared `audit_log` в HA-кластере vs per-host.** Выбран shared: одно место для запроса (`GET /v1/audit` не координирует cross-host), естественная глобальная картина для compliance-audit-а. Цена — все инстансы пишут в одну таблицу: при высокой write-нагрузке (десятки тысяч событий в секунду) может потребоваться partitioning by `created_at` (BRIN-индекс на `created_at` хорошо ложится на time-series-pattern). Mitigation post-MVP без breaking changes.
  - **`source` closed enum vs freeform.** Closed — `soul-lint` и парсер `keeper.yml` могут типизированно валидировать; индексирование стабильное. Цена — расширение enum-а требует PR через propose-and-wait. Соответствует общему стилю Soul Stack (capabilities / side_effects тоже closed, [ADR-020](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).
  - **Retention через Reaper vs встроенный partition-pruning.** Выбран Reaper: единая модель чистки во всех таблицах Keeper-а ([reaper.md](../keeper/reaper.md)), один механизм метрик и dry-run-а, hot-reload `max_age` через тот же блок. Цена — Reaper batch-удаление вместо мгновенного DROP PARTITION; для масштабов MVP приемлемо. Партиционирование `audit_log` по `created_at` — расширение post-MVP при росте объёма, не breaking (`purge_audit_old` тогда заменяет batch-DELETE на DROP PARTITION).

---

## Amendment-указатель (2026-06-24, pluggable audit sink — см. ADR-059)

PG-`audit_log` (этот ADR) остаётся **default-ом и source-of-truth ПОКА**. На целевом масштабе (100k VM) синхронная PG-запись audit не масштабируется ([known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету)); решение направления — backend audit-выгрузки становится **выбираемым** через `keeper.yml → audit.sink: pg | kafka | off` (выбор реализации `shared/audit.Writer`, абстракция уже есть). Kafka-sink — opt-in, OPTIONAL-with-degradation ([ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)), не меняет обязательный контур. Дизайн (proposed / deferred, кода в бете нет) — **[ADR-059](0059-audit-sink-pluggable.md)**; там же зафиксирована hard-зависимость: `changed_tasks`/`GET /v1/audit` сегодня деривят данные из `audit_log` в PG и при Kafka-only-режиме требуют альтернативного источника (решить ДО реализации).
