# Toll — cluster-wide detector массового оттока Souls

Реализация [ADR-038](../../../docs/adr/0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition).

## Назначение

Пассивный наблюдатель rate-of-disconnect Soul-агентов на cluster-level. При
превышении порога (default 20% от baseline `souls.status='connected'` за
sliding-окно 60s) выставляет cluster-wide флаг `cluster:degraded` (Redis-key,
TTL 60s) → Operator-API на чувствительных мутирующих ручках возвращает 503 +
Retry-After. Цель — отсечь оператора от запуска scenario / push-apply на
заведомо больном кластере с явной ошибкой, а не давать `partial_failed` или
тихо-сбойный прогон.

Toll — **passive observer**: не закрывает стримы (это `watchman`,
soul-shedding S2 — другая сущность), не делает recovery actions, только
наблюдает и блокирует write-API.

## Архитектура

```
                  ┌──────────────┐
                  │  Toll Leader │   (single Keeper instance via Redis-lease)
                  │  (aggregator)│
                  └──────┬───────┘
                         │ reads
            ┌────────────┴────────────┐
            │ Redis sorted-set        │
            │ toll:disconnects        │   ← все per-instance Watcher-ы публикуют
            │ (score=unix-sec,        │      disconnect-события сюда
            │  value=sid|kid|coven)   │
            └─────────────────────────┘
                         │ leader writes
                         ▼
                   cluster:degraded   (Redis key, TTL 60s)
                         │
                  middleware reads
                         ▼
               POST /v1/incarnations/{name}/scenarios/{scenario} → 503 Retry-After
               POST /v1/push/apply                                 → 503 Retry-After
               (read-API, RBAC, unlock, destroy, Errand           — НЕ блокируются)
```

## Компоненты пакета

| Файл | Что внутри |
|---|---|
| `publisher.go` | `Publisher`-interface, `DegradedReader`-interface, `NoopDegradedReader`, `EncodeDisconnect` (форма member-а sorted-set-а). |
| `degraded.go` | `degradedWriter`-interface (internal, для Leader-а). |
| `watcher.go` | `Watcher` — per-instance hook, фильтры warmup-immunity + graceful-shutdown, ZADD в общий sorted-set через Publisher. |
| `leader.go` | `Leader` — фоновая goroutine, Redis-lease + aggregation-loop + set/clear с asymmetric hysteresis. Sentinel-ы `ErrLeaseTaken` / `ErrLeaseLost`. |
| `baseline.go` | `BaselineReader`-interface + `PGBaselineReader` (PG impl) + cached-обёртка с TTL. |
| `audit.go` | Тонкие враппер-ы записи `cluster.degraded_set` / `cluster.degraded_cleared`. |
| `metrics.go` | `Metrics` — `keeper_cluster_degraded` (gauge) + `keeper_toll_disconnects_total` (counter, label `coven`) + warmup/graceful/leader_active. |
| `middleware.go` | `DegradedMiddleware` — chi/net-http middleware, blocked-routes → 503 + Retry-After + RFC 7807 problem+json. |

## Инварианты (см. ADR-038)

- **Single-leader агрегация** — set `cluster:degraded` ТОЛЬКО leader через
  Redis-lease `cluster:toll:leader`. Два флага одновременно не выставляются.
- **Warmup-immunity 60s** после старта инстанса — disconnect-ы считаются (метрика
  растёт), но НЕ публикуются (cluster cold-start false-positive defense).
- **Graceful-shutdown filter** — закрытия, инициированные самим Keeper-ом
  (Watchman shedding / `ctx.Done()` graceful keeper shutdown), отбрасываются.
- **Asymmetric hysteresis** — сработать на первом превышении, снять только после
  устойчивого grace 60s под threshold-ом.
- **Fail-OPEN middleware** — на reader-error (Redis flap) middleware пропускает
  запрос; доступность важнее перестраховки, флаг гаснет TTL-ом если leader умер.
- **Baseline=0 → no degraded** — пустой кластер не оценивается (защита от
  деления на ноль).

## Wire-up в daemon

Поднимается в `setupToll` (keeper/cmd/keeper/daemon.go) ПОСЛЕ `setupRedis` и ДО
`setupGRPCEventStream`. Gate-ы (любой выполнен → Toll выключен полностью, hook
EventStream-а no-op, middleware passthrough):

- `keeper.toll.enabled: false` в keeper.yml;
- `d.redisClient == nil` (single-instance/dev без Redis).

Wire-up использует тонкие adapter-ы (`keeperRedisToll*`) поверх примитивов
из `keeper/internal/redis/tolldetector.go` (ZADD/ZCOUNT/ZREMRANGEBYSCORE/
SET-DEL-EXISTS), чтобы пакет `toll` не зависел от `*redis.Client` напрямую и
оставался юнит-тестируемым через fake-интерфейсы.

## Конфигурация

См. `KeeperToll` в [shared/config/keeper.go](../../../shared/config/keeper.go).
Опц. блок `toll:` в keeper.yml; все поля имеют дефолты из `DefaultToll*`-констант:

```yaml
toll:
  enabled: true              # default true; false — выключить
  threshold: 0.20            # доля от baseline souls.status='connected'
  window_size: 60s
  degraded_ttl: 60s
  clear_grace: 60s           # asymmetric hysteresis
  lease_ttl: 30s             # cluster:toll:leader, renew каждые 10s
  warmup_delay: 60s          # cluster cold-start immunity
```

## Метрики

| Метрика | Тип | Label-ы | Назначение |
|---|---|---|---|
| `keeper_cluster_degraded` | gauge (0/1) | — | set ТОЛЬКО leader-ом; closed-набор cluster-level. |
| `keeper_toll_disconnects_total` | counter | `coven` | не-graceful disconnect-ы (post-filter). |
| `keeper_toll_warmup_skipped_total` | counter | — | disconnect-ы, отброшенные warmup-immunity. |
| `keeper_toll_graceful_skipped_total` | counter | — | disconnect-ы, отброшенные как graceful-shutdown. |
| `keeper_toll_leader_active` | gauge (0/1) | — | 1 = этот инстанс держит lease. Сумма по кластеру = 1. |

## Audit-events

`cluster.degraded_set` / `cluster.degraded_cleared` (область `cluster.*`), source
`keeper_internal`, `archon_aid: NULL`. Payload — численные параметры (rate,
baseline, threshold, leader_kid, window/grace seconds). Пишет ТОЛЬКО leader.

## Тесты

- `watcher_test.go` — warmup-immunity / graceful filter / publisher-error
  not-fatal / nil-safe receiver.
- `leader_test.go` — acquire-retry / set-degraded / clear-grace / baseline=0 /
  lease-lost recovery / sorted-set error skips tick / cached baseline.
- `middleware_test.go` — passthrough / 503 / fail-open / nil-reader.

L1 / L3 (integration с реальным Redis-кластером) — отдельный slice.
