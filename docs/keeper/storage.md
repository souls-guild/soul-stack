# Хранилища Keeper-кластера: Postgres + Redis

Keeper — stateless по дизайну ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). Всё, что переживает рестарт инстанса, лежит снаружи бинаря: **Postgres** для холодного состояния и **Redis** для горячего слоя с координацией.

## Postgres — холодное хранилище (source of truth)

Закреплено [ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres). Embedded KV не используется ни в каком виде — только PG.

| Реестр / таблица | Что хранит | Подробности |
|---|---|---|
| `souls` | Реестр всех Soul-записей в системе (и agent-, и push-хостов): `sid`, `transport`, `status`, `coven[]`, `registered_at`, `last_seen_at`, `last_seen_by_kid`, оператор-аудит. | [`../soul/identity.md → Реестр souls`](../soul/identity.md#реестр-souls). |
| `soul_seeds` | История mTLS-сертификатов Soul: только `fingerprint`, **без PEM и приватных ключей**. Статусы `active` / `superseded` / `expired` / `revoked`. | [`../soul/identity.md → Реестр soul_seeds`](../soul/identity.md#реестр-soul_seeds). |
| `bootstrap_tokens` | Одноразовые токены онбординга: `token_hash` (SHA-256), TTL, `used_at`, `used_by_kid`. Plain-токен в БД **не хранится**. Инвариант: `UNIQUE (sid) WHERE used_at IS NULL`. | [`../soul/identity.md → Реестр bootstrap_tokens`](../soul/identity.md#реестр-bootstrap_tokens) и [`../soul/onboarding.md`](../soul/onboarding.md) — жизненный цикл. |
| `operators` | Реестр Архонтов: `aid` (PK, kebab-case), `display_name`, `auth_method` (`jwt`/`mtls`/`combined`), `created_at`, `created_by_aid` (FK на `operators(aid)`, `NULL` только у первого Архонта — инвариант через partial unique index), `revoked_at`, `metadata` (jsonb). FK-поля `created_by_aid` / `changed_by_aid` в других таблицах ссылаются сюда. | [architecture.md → ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon), [rbac.md](rbac.md). |
| `service_registry` | Реестр Service-ов (тип сервиса → git-координаты): `name` (PK, kebab-case), `git`, `ref` ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте): version = git ref), `refresh` (опц. duration авто-fetch), оператор-аудит (`created_by_aid` / `updated_by_aid` FK на `operators(aid)`). Перенесён из `keeper.yml → services:` ([ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres)); управление через `service.*` API/MCP ([operator-api.md](operator-api.md)). Runtime читает in-memory снимок (`serviceregistry.Holder`, TTL-poll + pub/sub-инвалидация `service:invalidate`). | [architecture.md → ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres), [config.md](config.md). |
| `keeper_settings` | Well-known скаляры кластера (`key` PK → `value`), перенесённые из `keeper.yml`. MVP-ключ: `default_destiny_source` (шаблон git-URL Destiny по умолчанию, см. [config.md](config.md)). Управление через `service.*` API/MCP, читается тем же `serviceregistry.Holder`. | [architecture.md → ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres), [config.md](config.md). |
| Destiny-каталог | Резолвенные и провалидированные артефакты Service / Destiny / Module по `ref:`-ам из реестра `service_registry` (Service) и `service.yml → destiny[]` (Destiny), git-URL — `service_registry.git` / `keeper_settings.default_destiny_source` ([ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres)). Реестр в БД, git — исходник. | [architecture.md → Артефакты Soul Stack](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд). |
| `incarnation` | Runtime-инстансы сервисов: `spec`, `state`, `status`, `service_version`, `state_schema_version`. Управляются через API/MCP. | [architecture.md → Incarnation](../architecture.md#incarnation--runtime-инстанс-сервиса). |
| `state_history` | Snapshot per-change для `incarnation.state`: `state_before` / `state_after`, инициатор, время. Retention — [open Q №19](../architecture.md#открытые-вопросы). | [architecture.md → `state_history`](../architecture.md#state_history--журнал-изменений-state). |
| `apply_runs` | Correlation `apply_id` ↔ incarnation/scenario: одна строка на `(apply_id, sid)` хост-fan-out-а прогона. `status` (`planned`/`claimed`/`dispatched`/`running`/`success`/`failed`/`cancelled`/`no_match`/`orphaned`), `error_summary`, `started_by_aid`, `cancel_requested` (флаг cluster-wide Cancel, см. ниже), **Ward-claim** колонки `claim_by_kid` / `claim_at` / `claim_expires_at` / `attempt` ([ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). **`apply_runs` orchestrator-agnostic — колонки back-link на родительский прогон НЕ несёт** (прямой `incarnation.run` живёт без Voyage). Связь с Voyage обратная: `voyage_targets.apply_id → apply_runs(apply_id)` пишется в таблице оркестратора ([ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), см. строку `voyages` + `voyage_targets` ниже). _(Эскизный back-link `apply_runs.run_id`, как и прежний `apply_runs.tide_id`/`surge_index` на таблицу `tides`, в схеме отсутствуют: `tide_id` снесён в Wave 5 вместе с таблицей `tides`, migration `061`; `run_id` так и не был реализован — связь инвертирована.)_ scenario-runner на dispatch-е пишет строку в `planned` и шлёт Summons; Acolyte клеймит `planned → claimed` через `FOR UPDATE SKIP LOCKED` (`attempt++` — fencing-epoch), резолвит+рендерит just-in-time и переводит `claimed → dispatched` **перед** отправкой `ApplyRequest` (deliver-once intent-маркер, [ADR-027 amend GATE-1](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)); RunResult-handler переводит в терминал. Протухший `claimed` (после GATE-1 reclaim сужен до `status = 'claimed'`, не `running`/`dispatched`: `status = 'claimed' AND claim_expires_at < NOW`) возвращает в `planned` recovery-скан Reaper-лидера — закрывает дыру «недо-доставленный рендер». Чистится Reaper-правилом `purge_apply_runs`. **Lifecycle: `planned → claimed → dispatched → terminal`** (`running` — vestigial-статус старого синхронного пути `dispatchWave` при `acolytes:0`, в Acolyte-флоу не используется; `dispatched` НЕ реклеймится — после отдачи Soul-у повторный `SendApply` = двойной apply). Терминалы **`no_match`** (нецелевой roster-хост на Acolyte-пути: `on:`/`where:` отфильтровал все задачи → benign-терминал, barrier засчитывает на success-сторону, incarnation `ready` — FINDING-01 fix; **остаточный диалект**: старый путь `acolytes:0` нецелевые строки не заводит вовсе, Acolyte-путь пишет весь roster с `no_match`) и **`orphaned`** (S6 Soul-reconcile, [ADR-027(g)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim): `dispatched`-строка, не подтверждённая Soul-ом в `WardRoster` на reconnect → barrier fail → `error_locked`). | [architecture.md → ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim), [architecture.md → ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация), [reaper.md](reaper.md), [§ Cluster-wide Cancel](#cluster-wide-cancel-прогона). |
| `apply_task_register` | Накопитель register-данных задач прогона для `state_changes.sets`: одна строка на `(apply_id, sid, task_idx)` с `register_data` (jsonb). Handler пишет из `TaskEvent.register_data`; scenario-runner после барьера читает per-host и резолвит `task_idx → register-имя`. FK на `apply_runs(apply_id, sid)` ON DELETE CASCADE (чистится каскадом вместе с прогоном правилом `purge_apply_runs`). Транзиентный run-state с потенциальными секретами в `register_data`: чистится агрессивнее отдельным Reaper-правилом `purge_apply_task_register` (default grace 1h после терминала apply_run, [reaper.md](reaper.md)). | [architecture.md → ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация), [scenario/orchestration.md §7.1](../scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций). |
| `voyages` + `voyage_targets` | Реестр Voyage-прогонов (унифицированный батч, [ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон); эскизные имена `runs`/`run_targets` уточнены до `voyages`/`voyage_targets` в migration `059`): `voyages` — `voyage_id` (PK), `kind` (`scenario`/`command`), таргет/`batch_size`/`concurrency`/`schedule_at`/`inter_batch_interval`/`on_failure`, `status` (`scheduled`/`pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`), **PG-based failover-claim** (`claimed_by_kid` / `last_renewed_at` / `claim_expires_at` / `attempt` — параллель Ward-claim из `apply_runs`); `voyage_targets` — единицы батча (Leg) с `batch_index` и back-link на дочерний прогон: `apply_id` (`kind=scenario`, FK-смысл → `apply_runs(apply_id)`) либо `errand_id` (`kind=command` → `errands(errand_id)`), partial-UNIQUE индексы (migration `063`) дают «один apply_id/errand_id → максимум одна строка». Направление связи **инвертировано относительно эскиза**: back-link живёт здесь, а не в `apply_runs` (тот orchestrator-agnostic). Подбирается `VoyageWorker`-пулом; протухший running-Voyage Reaper-правило `reclaim_voyages` возвращает в `pending`. **Заменил удалённые в Wave 5 таблицы `tides` (migration `061`, поглощена Tide [ADR-040](../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) и `errand_runs` (migration `062`, ErrandRun [ADR-041](../adr/0041-errandrun.md#adr-041-errandrun--multi-target-обвязка-над-errand)).** | [architecture.md → ADR-043](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон), [reaper.md](reaper.md). |
| Журналы прогонов | События каждого pull/push apply-а (что, куда, кем, результат). С RBAC-фильтром. | [push.md](push.md), [rbac.md](rbac.md). |
| Cloud-runtime | `Provider`, `Profile`, реестр созданных VM. Управляется через API/MCP. | [cloud.md](cloud.md). [architecture.md → Cloud-интеграция](../architecture.md#cloud-интеграция-через-keepercloud). |
| RBAC-политики | Роли, операторы, permissions. | [rbac.md](rbac.md). |
| `audit_log` | Audit-trail Keeper-кластера: `audit_id` (ULID PK), `created_at`, `event_type` (`<area>.<action>`), `source` (closed enum: `signal` / `api` / `mcp` / `keeper_internal` / `soul_grpc`), `archon_aid` (nullable FK на `operators(aid)`), `correlation_id` (ULID, opt), `payload` (jsonb). Пишется helper-ом `shared/audit` всеми write-path-инициаторами Keeper-а; чистится Reaper-правилом `purge_audit_old`. Schema — [§ Таблица `audit_log`](#таблица-audit_log). | [architecture.md → ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention), [reaper.md](reaper.md), [config.md → audit](config.md#audit). |
| `plugin_sigils` | Keeper-signed allow-list допущенных бинарей плагинов (`soul-mod-*` / `soul-cloud-*` / `soul-ssh-*`): заменяет TOFU. Ключ `(namespace, name, ref)` (тип / имя бинаря / git-ref версии; active partial-unique — одна активная запись на тройку, re-allow после revoke = новый INSERT) → `sha256` (hex, lowercase, 64) допущенного бинаря; `signature` (bytea — сырая подпись Keeper-а ed25519/ECDSA) над подписываемым блоком `(… , binary_sha256, manifest)`; `manifest` (jsonb — пришит в подписываемый блок, чтобы `side_effects` / `capabilities` не подделывались). Lifecycle: `allowed_by_aid` (NOT NULL) / `allowed_at` — Архонт явно допустил через OpenAPI/MCP → `revoked_at` / `revoked_by_aid` (NULL) — отозвал. FK на `operators(aid)`: `allowed_by_aid` RESTRICT (нельзя удалить автора активного допуска), `revoked_by_aid` SET NULL. Schema — [§ Таблица `plugin_sigils`](#таблица-plugin_sigils). | [architecture.md → ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), [plugins.md → Integrity-model](plugins.md#integrity-model). |

Соединение задаётся в [config.md](config.md) → блок `postgres:` (`dsn_ref` из Vault, размер пула).

### Таблица `audit_log`

Полная нормировка — [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention). Schema:

```sql
CREATE TABLE audit_log (
  audit_id        TEXT        PRIMARY KEY,                       -- ULID (sortable timestamp prefix + random component; глобальная уникальность без cross-host-координации)
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  event_type      TEXT        NOT NULL,                          -- '<area>.<action>', каталог: docs/naming-rules.md → Audit-events
  source          TEXT        NOT NULL,                          -- closed enum: signal | api | mcp | keeper_internal | soul_grpc
  archon_aid      TEXT        REFERENCES operators(aid) ON DELETE SET NULL,  -- nullable: NULL для source IN ('signal', 'keeper_internal'); при удалении operator-а audit-trail сохраняется через SET NULL
  correlation_id  TEXT,                                           -- ULID, optional; reuse apply_id для source='soul_grpc'
  payload         JSONB       NOT NULL DEFAULT '{}'::jsonb       -- kind-specific полезная нагрузка, маскированная по operator-api.md → Secret masking
);

CREATE INDEX idx_audit_event_type   ON audit_log (event_type, created_at);
CREATE INDEX idx_audit_aid          ON audit_log (archon_aid, created_at) WHERE archon_aid IS NOT NULL;
CREATE INDEX idx_audit_correlation  ON audit_log (correlation_id)         WHERE correlation_id IS NOT NULL;
```

Партиционирование по `created_at` (например, BRIN-индекс или PG declarative partitioning по месяцам) — расширение post-MVP при росте объёма; не breaking (Reaper-правило `purge_audit_old` тогда заменяет batch-DELETE на DROP PARTITION).

Все write-path-инициаторы Keeper-а (HTTP-middleware Operator API, MCP-handler, Reaper, hot-reload pipeline, `keeper.cloud`, `keeper.push`, bootstrap, Soul gRPC event forwarder) пишут через общий helper `shared/audit` — он же отвечает за secret-masking ([operator-api.md → Secret masking](operator-api.md#secret-masking-в-логах-и-трейсах)) и опциональный OTel dual-write (`keeper.yml → audit.otel_export`). Чтение для `GET /v1/audit` (отдельная задача Operator API extension) — стандартный SQL-запрос с RBAC-фильтром.

### Таблица `plugin_sigils`

Реестр Sigil-ов — Keeper-signed allow-list допущенных бинарей плагинов ([ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), [plugins.md → Integrity-model](plugins.md#integrity-model)). Запись появляется, только когда Архонт **явно** допускает плагин через OpenAPI/MCP; это заменяет TOFU-семантику «host сам решает доверять». Schema:

```sql
CREATE TABLE plugin_sigils (
  id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  namespace       TEXT        NOT NULL,                          -- тип плагина: cloud / ssh / mod
  name            TEXT        NOT NULL,                          -- имя бинаря: soul-cloud-hetzner и т.п.
  ref             TEXT        NOT NULL,                          -- git-ref версии (ADR-007)
  sha256          TEXT        NOT NULL,                          -- digest допущенного бинаря (hex, lowercase, 64)
  signature       BYTEA       NOT NULL,                          -- подпись Keeper-а (ed25519/ECDSA) над подписываемым блоком; сырые байты, без base64
  manifest        JSONB       NOT NULL,                          -- пришит в подписываемый блок (ADR-026(c)) → side_effects/capabilities не подделываемы
  allowed_by_aid  TEXT        NOT NULL REFERENCES operators(aid),               -- кто допустил; default NO ACTION (эффективно RESTRICT) — нельзя удалить автора активного допуска
  allowed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  revoked_at      TIMESTAMPTZ,                                   -- NULL = активен; NOT NULL = допуск отозван (мягко, для аудита)
  revoked_by_aid  TEXT        REFERENCES operators(aid) ON DELETE SET NULL,     -- история ревокации переживает удаление оператора

  CONSTRAINT plugin_sigils_sha256_format CHECK (sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX plugin_sigils_allowed_by_aid_idx ON plugin_sigils (allowed_by_aid);
-- Инвариант: не более одной АКТИВНОЙ записи на (namespace, name, ref); этот же
-- индекс покрывает lookup при verify. Прецедент — bootstrap_tokens (миграция 008).
CREATE UNIQUE INDEX plugin_sigils_active_idx ON plugin_sigils (namespace, name, ref) WHERE revoked_at IS NULL;
```

**Выбор `signature BYTEA`.** Подпись ed25519/ECDSA — сырые бинарные байты фиксированной (ed25519 — 64 байта) длины. `BYTEA` хранит их напрямую, без накладного base64-кодирования (`text`) и без риска рассинхрона кодировки между write-path-ом (S2a/S3) и verify-путём (S6). Точный формат подписываемого блока — слайс S3.

**Lifecycle.** `allowed` (Архонт допустил — audit-event `plugin.allowed`, [naming-rules.md → область `plugin.*`](../naming-rules.md#sigil-реестр-и-поля-целостности)) → опционально `revoked` (`revoked_at`/`revoked_by_aid` — audit-event `plugin.revoked`). Ревокация мягкая: строка остаётся для аудита, активность определяется `revoked_at IS NULL`. Уникальность — partial-unique по активным записям (`plugin_sigils_active_idx`): активная запись на тройку `(namespace, name, ref)` всегда одна, а **re-allow после revoke создаёт НОВУЮ запись** (чистый INSERT) — история ревокаций сохраняется, прежние `sha256`/`signature`/`allowed_by_aid` не затираются. Провал верификации Sigil перед seal/exec на host-е — audit-event `plugin.verify_failed`. CRUD реестра, подпись и verify — слайсы S2a/S3/S6, не часть этой миграции.

## Redis — горячий слой и координация

Закреплено [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis). PG = source of truth, Redis = производный горячий слой и шина координации.

| Роль | Что лежит | Зачем |
|---|---|---|
| **(a) Heartbeat-кэш** | `last_seen_at` и `last_seen_by_kid` Souls. EventStream-handler пишет real-time значение в Redis на каждое app-сообщение, а PG-`souls.last_seen_at` сбрасывает **throttled** — не чаще раза в `mark_disconnected.stale_after / 3` (default 30s) на каждый SID. «Список активных Souls» читается из Set/HSET в Redis, не из PG. | Снимает UPDATE-шторм по `souls.last_seen_at` при каждом сообщении по стриму, но при этом держит PG-снимок свежим, чтобы Reaper-правило `mark_disconnected` не помечало живой стрим disconnected. |
| **(b) Lease на SID** | `SET sid:lock <kid> NX EX <ttl>` с продлением. Какой Keeper держит активный bidi-стрим к данному Soul. | При разрыве TTL истекает, следующий Keeper свободно занимает. Без собственного консенсуса. |
| **(c) Pub/Sub** | Сигналы между Keeper-инстансами: «новый Soul зашёл», «Destiny обновлён», «отзыв SoulSeed». | Уход от поллинга Postgres. |
| **(d) Лидерский lease** | `reaper:leader` — кто из Keeper-ов сейчас Жнец; `conductor:leader` — кто исполняет Cadence-расписания ([Conductor](conductor.md), [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)). Ключи **независимы** — лидеры могут быть на разных инстансах. | Каждая single-executor-подсистема (Reaper / Conductor) работает на одном инстансе одновременно — см. [reaper.md](reaper.md), [conductor.md](conductor.md). |

Кэш rendered Destiny / Soulprint **отложен** до появления реальной нагрузки — не часть MVP ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis), последний пункт).

Соединение задаётся в [config.md](config.md) → блок `redis:` (`addr`, `password_ref` из Vault).

## Cluster-wide Cancel прогона

Прогон scenario исполняется **run-goroutine-ом в памяти одного** Keeper-инстанса (тот, что принял запрос на запуск, [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). Локальная отмена (`Runner.Cancel`) отменяет только `runCtx` этой goroutine. Если оператор вызвал Cancel на инстансе **Keeper-B**, а run-goroutine живёт на **Keeper-A** — локальная отмена не доходит (G1).

Механизм — **PG-флаг** `apply_runs.cancel_requested` (миграция 024), multi-instance-safe, ложится на уже существующий barrier-поллинг:

- **Постановка флага (любой инстанс).** `Runner.RequestCancel` ставит `cancel_requested = true` на все ещё-нетерминальные строки прогона в guard-окне (`UPDATE apply_runs SET cancel_requested = true WHERE apply_id = $1 AND status IN ('planned','claimed','running')`). Это работает с любого Keeper-инстанса — флаг живёт в общей Postgres, не в памяти. Окно включает `planned`/`claimed` (отмена ДО `SendApply` безопасна: Acolyte перед отправкой `ApplyRequest` проверяет `cancel_requested` и не шлёт apply). **Known-gap (ADR-027 amend GATE-1(f)):** `dispatched` в guard НЕ включён — by design pilot: Keeper уже отданным Soul-ам `CancelApply` не шлёт (best-effort cancel — отдельный слой). Прогон с dispatched-строкой завершится штатно, а barrier увидит его терминал по `apply_id`; cluster-wide отмена уже-отданного — отдельный под-слайс (guard += `dispatched` вместе с `CancelApply`-emit), сводится с Soul-reconcile (Q2).
- **Чтение флага (инстанс-владелец).** Run-goroutine в barrier-поллинге (`waitBarrier`) уже опрашивает `apply_runs` каждые `poll_interval` (default 200ms); тем же запросом она читает `cancel_requested`. Увидев флаг, прерывает барьер и уходит в тот же abort-путь, что и локальный ctx-Cancel.
- **Быстрый локальный путь.** Если run-goroutine живёт на том же инстансе, что принял Cancel, `RequestCancel` дополнительно дёргает локальный `Runner.Cancel` — отмена срабатывает немедленно, не дожидаясь барьерного тика. Период поллинга = верхняя граница задержки cross-Keeper-отмены.

**Семантика отмены** идентична локальному Cancel: incarnation переходит в `error_locked` (`status_details.reason = dispatch_failed`), state не меняется — оператор снимает блокировку через `POST /v1/incarnations/{name}/unlock` после разбора последствий ([operator-api/incarnations.md → unlock](operator-api/incarnations.md#post-v1incarnationsnameunlock--снять-error_locked)). Уже отправленным Soul-ам Keeper в pilot-е не шлёт `CancelApply` (best-effort cancel — отдельный слой через `Outbound.SendCancel`).

**Идемпотентность и гонки:**

- Повторный Cancel (double-cancel) — no-op: флаг `true → true`, локальный `Cancel` второй раз не находит goroutine.
- Cancel **уже завершённого** прогона (терминальный `status`) — no-op: guard `status IN ('planned','claimed','running')` не трогает терминальных (и `dispatched`-) строк, локальной goroutine нет (затронуто 0 строк → `RequestCancel` возвращает `found=false`).
- Флаг не мешает нормальному завершению: success-/failed-строки не несут `cancel_requested=true` (он ставится только в guard-окне `planned`/`claimed`/`running`), а если флаг успел встать в зазоре до прихода RunResult — barrier видит его раньше, чем classify, и отменяет, что и есть требуемое поведение Cancel.

## Граница git ↔ Postgres

Это инвариант системы, важный при работе с любым артефактом ([architecture.md → Артефакты Soul Stack](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд)):

- **Git** — код и определения: Service, Destiny, Module. Версионирование через git ref ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)), ревью через PR, история через `git log`.
- **Postgres** — runtime-state: Soul, SoulSeed, Incarnation, Coven, Provider, Profile, **реестр Service-ов** (`service_registry` + скаляры `keeper_settings`, [ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres)). Мутации через OpenAPI/MCP, source of truth — БД. (Сам код Service-репозитория остаётся в git — в БД только координаты `name → git@ref`.)

Опциональный экспорт incarnation в YAML для backup/audit — это **не** primary path и не обязательная фича.

## См. также

- [config.md](config.md) — блоки `postgres:` и `redis:` в `keeper.yml`.
- [reaper.md](reaper.md) — Жнец чистит таблицы Postgres, лидер — через Redis-lease.
- [`../soul/identity.md`](../soul/identity.md) — детали `souls`, `soul_seeds`, `bootstrap_tokens` и онбординга.
- [architecture.md → ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres), [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis).
- [architecture.md → Артефакты Soul Stack](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд).
