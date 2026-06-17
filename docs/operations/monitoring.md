# Monitoring & alerts

Что и как мониторить в Soul Stack-инсталляции. Полная спека observability — [`docs/observability.md`](../observability.md) (ADR-024). Здесь — операционная часть: ключевые алерты, dashboards-указания, как читать критичные метрики.

## Каналы телеметрии

| Канал | Что несёт | Эндпоинт |
|---|---|---|
| **Prometheus `/metrics`** (pull) | Метрики Keeper и Soul (counters / gauges / histograms) | Keeper: `listen.metrics.addr` (default `:9090`); Soul: `metrics.listen` (default `127.0.0.1:9091`) |
| **OpenTelemetry** (push OTLP gRPC) | Трейсы; опционально OTLP-метрики (post-MVP) | `otel.endpoint` (`*:4317`) |
| **Логи** (stdout/file) | Структурированные JSON-логи | `logging.file` или stderr (systemd journal); встроенная ротация |

Полное обоснование Prometheus-primary + OTel-bridge — [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge).

### Защита `/metrics`

| Бинарь | Защита | Как настроить |
|---|---|---|
| Keeper | HTTP Basic-auth (опц.) | `metrics.auth.basic` + пароль из `password_ref` (Vault); см. [config.md → metrics](../keeper/config.md#metrics). |
| Soul | Loopback bind по default + опц. Basic-auth | `metrics.listen: 127.0.0.1:9091` (default) — недоступен снаружи. Если нужен внешний scrape — bind на интерфейс и Basic-auth через `password_file` (см. [`docs/soul/config.md` → metrics](../soul/config.md#metrics)). |

## Namespace метрик

| Префикс | Кто экспонирует |
|---|---|
| `keeper_*` | Keeper-side метрики (gRPC, scenario, RBAC, render, vault, reaper, …) |
| `soul_*` | Soul-side метрики (apply, eventstream, soulprint, beacon) |

Различение по префиксу, не по label. Полные конвенции (snake_case, `_total`/`_seconds`/`_bytes`, label-cardinality правила) — [`docs/observability.md` § 2](../observability.md). Подробный каталог по подсистемам — [`docs/observability.md` § 4.1](../observability.md).

OTel resource-attributes идентифицируют инстанс:
- `service.name = "keeper" | "soul"` (стандарт OTel semconv).
- `soulstack.kid = <KID>` (только на Keeper).
- `soulstack.sid = <SID>` (только на Soul).

## Ключевые алерты

Условные expression-ы — для Prometheus / alertmanager. Точные пороги — подобрать под инсталляцию.

### Critical (немедленное внимание)

| Алерт | Expression | Что означает |
|---|---|---|
| **Keeper down** | `up{job="keeper"} == 0 for 1m` | Keeper-инстанс не отвечает. Investigate через journalctl / logs. |
| **Reaper not running** | `sum(keeper_reaper_lease_held) == 0 for 5m` | Ни один инстанс не держит Reaper-lease. Чистка БД остановлена. Если затянется — `bootstrap_tokens` / `apply_runs` / `audit_log` разрастутся. |
| **Reaper split-brain** | `sum(keeper_reaper_lease_held) > 1` | Два инстанса параллельно считают себя Reaper-лидером. **Не должно происходить** (Redis-lease с TTL). Логировать инцидент, перезагрузить Redis. |
| **Conductor not running** | `sum(keeper_conductor_lease_held) == 0 for 5m` | Ни один инстанс не держит lease `conductor:leader` — [Cadence](../naming-rules.md#сущности-предметной-области)-расписания **не спавнят Voyage** ([conductor.md](../keeper/conductor.md), [ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)). Если планировщик намеренно выключен (`cadence_scheduler.enabled: false` / нет Redis) — алерт не должен взводиться (collectors не публикуются). |
| **Conductor split-brain** | `sum(keeper_conductor_lease_held) > 1` | Два инстанса параллельно считают себя Conductor-лидером. **Не должно происходить** (Redis-lease с TTL). Логировать инцидент, перезагрузить Redis. |
| **PG connection failure** | `keeper_postgres_connection_errors_total[5m] > 0` (когда метрика появится) | Keeper не может подключиться к PG. Большинство операций упадут. |
| **Vault unreachable** | `keeper_vault_read_errors_total{kind="error"}[5m] > 10` | Vault недоступен. Новые операции (резолв секретов на старте) упадут; in-flight выживут. |
| **`incarnation` stuck in `applying`** | (no native метрика, через SQL: `SELECT COUNT(*) FROM incarnation WHERE status = 'applying' AND (NOW() - applying_started_at) > '15 minutes'`) | Прогон висит → дыра owned-by-dead-instance ([ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). При `acolytes:0` под HA — known footgun. Diagnostics — [`faq.md` → applying-stuck](faq.md). |
| **Conclave split / no presence** | (no native метрика — через redis `KEYS keeper:instance:*` count) | Если `KEYS` пусто — все инстансы потеряли Redis-связность; если меньше реального числа — часть изолирована. |

### Warning (триаж в рабочее время)

| Алерт | Expression | Что означает |
|---|---|---|
| **gRPC streams below baseline** | `keeper_grpc_streams_active < expected_souls_count * 0.95 for 10m` | Часть Souls не подключена. Проверить: `souls.status = 'disconnected'`, причины (сетевой outage / Soul-бинарь упал / SoulSeed просрочен). |
| **Drift в `souls.status` от lease** | (требует cross-source: SQL count `souls.status = 'connected'` vs Redis `KEYS soul:*:lock` count) | Большое расхождение → `mark_disconnected` reconcile не успевает. Проверить Reaper-логи, lock_ttl. |
| **Apply failure rate** | `rate(keeper_scenario_runs_total{result="failed"}[15m]) / rate(keeper_scenario_runs_total[15m]) > 0.05` | >5% прогонов фейлится. Investigate scenario / hosts. |
| **Apply tasks high retry rate** | `rate(soul_apply_task_retries_total[15m]) > 0.5` | Задачи часто ретраятся. Нестабильные хосты / flaky задачи. |
| **Vault read latency** | `histogram_quantile(0.99, rate(keeper_vault_read_duration_seconds_bucket[5m])) > 1.0` | Vault отвечает медленно. Investigate Vault-инстанс. |
| **RBAC snapshot stale** | `time() - keeper_rbac_snapshot_last_success_timestamp_seconds > 300` | Снимок RBAC не пересобрался > 5 мин. Проверить `keeper_rbac_snapshot_rebuild_errors_total{kind=...}` за деталями. |
| **Service-registry snapshot stale** | `time() - keeper_serviceregistry_snapshot_last_success_timestamp_seconds > 300` | То же для service-реестра. |
| **Augur deny rate** | `rate(keeper_augur_fetch_total{decision="denied"}[15m]) / rate(keeper_augur_fetch_total[15m]) > 0.1` | >10% Augur-запросов отвергнуты — possibly misconfigured Omens или попытка escalation. |
| **Sigil last delivered drops** | `keeper_sigil_anchors_last_delivered < keeper_grpc_streams_active * 0.8` | Re-broadcast trust-anchor-набора не дошёл до большинства Souls — проверить EventStream-состояние перед `Retire` старого ключа. |
| **Apply timed-out rate** | `rate(soul_apply_task_timed_out_total[15m]) > 0` | Задачи начали падать по таймауту. |
| **Beacon portents dropped** | `rate(soul_beacon_portents_dropped_total[15m]) > 0` | Soul теряет beacon-события. Beacon-реакции в Oracle пропускаются. |
| **Oracle circuit tripped** | `rate(keeper_oracle_circuit_tripped_total[15m]) > 0` | Какое-то Decree сорвалось в петлю и было auto-disabled. Investigate. |
| **Conductor spawn errors** | `rate(keeper_conductor_spawn_errors_total[15m]) > 0` | Тик Conductor падает при спавне Cadence (PG-сбой / резолв target расписания). Расписания не порождают Voyage. Investigate — [conductor.md → Метрики](../keeper/conductor.md#метрики). |
| **dispatched stuck (post-recovery-enable)** | (через SQL: `SELECT COUNT(*) FROM apply_runs WHERE status = 'dispatched' AND claim_at < NOW() - INTERVAL '1 hour'`) | После включения `reclaim_apply_runs` — строки `dispatched`, не подтверждённые Soul-ом (S6 Soul-reconcile должен их орфанить). Если зависают — Soul-reconcile не работает / Soul old. |

### Info (для capacity planning, не алерты)

- `keeper_grpc_apply_dispatch_total{result="ok"}` — пропускная способность апплая. Тренд → planning.
- `soul_apply_duration_seconds` — длительность прогонов. p95/p99 → ожидания пользователей.
- `keeper_reaper_rule_purged_total{rule=*}` — объём cleanup. Тренд `audit_log` retention → планировать партиционирование.

## Дашборды

### Keeper overview (один на кластер)

Группировка по `instance` (KID):

- `keeper_grpc_streams_active` — sum + per-instance breakdown.
- `keeper_grpc_messages_total` — rate by `direction`.
- `keeper_grpc_apply_dispatch_total` — rate by `result`.
- `keeper_scenario_runs_total` — rate by `result`.
- `keeper_scenario_run_duration_seconds` — p50/p95/p99.
- `keeper_render_duration_seconds` — p95.
- `keeper_reaper_lease_held` — gauge per-instance (sum=1).
- `keeper_reaper_rule_*` — per-rule purged + duration.
- `keeper_conductor_lease_held` — gauge per-instance (sum=1, если Conductor поднят).
- `keeper_conductor_spawned_total` / `keeper_conductor_spawn_errors_total` — спавн Cadence идёт по графику / ошибки.
- `keeper_vault_read_duration_seconds` — p99 by `mount`.
- `keeper_rbac_checks_total` — rate by `result`.

### Soul fleet (один на coven / весь флот)

- `soul_eventstream_connected` — sum / count Souls.
- `soul_eventstream_reconnects_total` — rate.
- `soul_apply_tasks_total` — rate by `result`.
- `soul_apply_duration_seconds` — p95.
- `soul_apply_task_skipped_total` — rate by `reason`.

### Audit / RBAC (compliance)

- `rate(keeper_rbac_checks_total{result="deny"}[5m])` — base-rate denied запросов.
- SQL-запросы к `audit_log` — отдельный dashboard через PG datasource (Grafana поддерживает PG как source). Фильтр по `event_type`, `archon_aid`, `correlation_id`.

## Логи

### Формат

`logging.format: json` (default) — структурированный JSON, парсится любой log-aggregator-ой:

```json
{
  "time": "2026-05-26T14:30:00.123Z",
  "level": "info",
  "msg": "soul connected",
  "sid": "host-01.example.com",
  "kid": "keeper-prod-01",
  "trace_id": "...",
  "span_id": "..."
}
```

`logging.format: text` — для интерактивного debug-а через journalctl.

### Ротация

Встроенная (см. [config.md → logging](../keeper/config.md#logging) / [soul/config.md → logging](../soul/config.md#logging)):

```yaml
logging:
  file: /var/log/keeper/keeper.log
  rotation:
    max_size_mb: 100      # ротация при достижении 100 MB
    max_age_days: 7       # удалять архивы старше 7 дней
    max_files: 10         # держать max 10 архивов
    compress: true        # gzip
```

Архивы — `<file>-<timestamp>.<ext>` рядом с `file`. Без зависимости от `logrotate` ([requirements.md](../requirements.md): «встроенная ротация логов по умолчанию»).

### Что искать в логах при инциденте

| Симптом | Грепать |
|---|---|
| Soul не подключается | `bootstrap` / `mTLS` / `SoulSeed verify` / `unauthorized` |
| RBAC deny / 403 | `permission denied` / `rbac` / `denied for aid` |
| Vault резолв упал | `vault` / `kv-read` / `ErrVaultKVNotFound` |
| Hot-reload провалился | `config.reload_failed` |
| Reaper-правило упало | `reaper` / `dispatch_error` |
| Apply failed | `apply` + `apply_id=<ULID>` для конкретного прогона |
| Conclave не зарегистрировал KID | `Conclave` / `ErrConclaveKIDTaken` |

## OTel-трейсы

Сквозные трейсы оператор → Keeper → Soul через trace-context в `ApplyRequest.trace_context` (W3C traceparent, [ADR-012(c)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) only-add). Реализованные span-ы:

| Span | Tracer | Где |
|---|---|---|
| `scenario.run` | `keeper/scenario` | Прогон scenario на Keeper. |
| `grpc.bootstrap` | `keeper/grpc` | Bootstrap RPC онбординга Soul. |
| `grpc.apply_dispatch` | `keeper/grpc` | Один dispatch `ApplyRequest`. |
| `render.pipeline` | `keeper/render` | CEL+text/template-фаза рендера. |
| `augur.request` | `keeper/augur` | Обработка `AugurRequest`. |
| `sigil.anchors_reload` | `keeper/sigil` | Runtime-ротация trust-anchor-набора. |
| `apply.run` | `soul/runtime` | Apply на Soul (child от `grpc.apply_dispatch`). |

Атрибуты несут доменные идентификаторы (`apply_id`, `sid`, `scenario`, `incarnation`), запрещённые в metric-labels (cardinality).

### Где смотреть трейсы

- **dev** — Jaeger UI на `http://127.0.0.1:16686` ([`docs/dev/local-setup.md`](../dev/local-setup.md)).
- **прод** — реальный OTel-backend (Jaeger / Tempo / DataDog / Honeycomb / …), endpoint в `otel.endpoint`.

### Sampling

`ParentBased(AlwaysSample)` зашит в коде (на момент написания). Конфигурируемый сэмплер — при первом реальном запросе ([`docs/observability.md` § 5](../observability.md)). Для прод-инсталляции с высоким трафиком — sampling придётся настраивать через OTel-collector (tail-based sampling в коллекторе).

## Capacity planning

Триггеры для рассмотрения масштабирования:

| Метрика | Trigger | Решение |
|---|---|---|
| `keeper_grpc_streams_active` per-instance | >5000 на одном инстансе | Scale-out: больше Keeper-инстансов; LB перебалансирует (см. [`scaling.md`](scaling.md)). |
| `keeper_scenario_run_duration_seconds.p99` | растёт без видимой причины | Investigate render-стадию (`keeper_render_duration_seconds`), затем PG (claim-bottleneck). |
| `keeper_reaper_rule_duration_seconds{rule="purge_audit_old"}` | растёт | `audit_log` разрастается, рассмотреть партиционирование. |
| Размер таблиц `apply_runs` / `state_history` / `audit_log` | растёт за пределы планирования | См. retention в [`infra.md`](infra.md). |
| OTel collector dropped spans | >0 | Увеличить ресурсы collector-а или добавить sampling. |

## См. также

- [`docs/observability.md`](../observability.md) — полная нормативная спека observability (ADR-024).
- [`docs/keeper/reaper.md` → Метрики](../keeper/reaper.md#метрики) — `keeper_reaper_*`.
- [`docs/keeper/conductor.md` → Метрики](../keeper/conductor.md#метрики) — `keeper_conductor_*`.
- [`docs/keeper/config.md` → metrics / otel / logging](../keeper/config.md) — конфигурация observability-блоков.
- [`docs/soul/config.md` → metrics / otel / logging](../soul/config.md) — то же для Soul.
- [`faq.md`](faq.md) — типичные триаж-сценарии.
