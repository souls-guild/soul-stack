# Scaling Keeper

Горизонтальное масштабирование Keeper-кластера: добавление инстансов, конфигурация Acolyte-пула, поведение Conclave / Watchman, refuse-guard, балансировщик, целевой масштаб 100k VM.

Архитектурный контекст:
- [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) — HA stateless-кластер поверх общей PG/Redis.
- [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) — Conclave (presence Keeper-инстансов) + SID-lease (presence Souls).
- [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) — Acolyte-пул, Ward-claim, Watchman / soul-shedding.

## Базовая модель

```
   Souls ───────► L4-LB (least-conn TCP) ──────►  keeper-1   keeper-2   ...   keeper-N
                                                     │           │              │
                                                     └────────┬──┴──────────────┘
                                                              ▼
                                                       ┌────────────┐
                                                       │  Postgres  │ shared
                                                       └────────────┘
                                                              ▲
                                                              ▼
                                                       ┌────────────┐
                                                       │   Redis    │ shared
                                                       └────────────┘
```

Любой инстанс обслуживает любой запрос. Specifics:

- **Распределение Soul-стримов** — между LB (новые соединения распределяются least-conn) и SID-lease в Redis (один Soul → один Keeper-инстанс на время сессии).
- **Распределение apply-исполнения** — work-queue ([ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)): Acolyte-пул на каждом инстансе клеймит задания из `apply_runs` через `SELECT … FOR UPDATE SKIP LOCKED`.
- **Распределение фоновых задач** — Reaper-лидер выбирается через Redis-lease `reaper:leader` (один live-Reaper в кластере).
- **Распределение балансировки нагрузки** (Shepherd, [ADR-002 amendment](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) — **PLANNED/backlog**, в коде нет. До его появления при scale-out новый инстанс простаивает до естественного churn EventStream-ов (failback по `failback.interval` или обрыв стрима).

## Acolyte-пул

Acolyte = пул воркеров исполнения apply на каждом Keeper-инстансе. **Feature-flag** через `acolytes: N` в `keeper.yml`:

```yaml
acolytes: 4                         # 0 = пул не поднимается (старый синхронный путь)
# acolyte_lease: 30s                # TTL Ward-захвата
# acolyte_batch: 10                 # макс. заданий за один claim-тик
# acolyte_poll_interval: 2s         # период poll-fallback к Summons
# acolyte_drain_grace: 5s           # окно graceful-drain при остановке
```

Полная типизация — [`docs/keeper/config.md` → acolytes](../keeper/config.md#acolytes).

### Когда что-сколько-Acolyte

| `acolytes` | Кому подходит |
|---|---|
| `0` (default) | **Single-keeper-only** ([ADR-027(h)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amendment). Старый run-goroutine путь: incarnation владеет in-memory один инстанс. Прогон, исполненный на K1 с Soul-ом на стриме K2 — **навсегда зависнет** в `applying`. |
| `>0` | **HA-обязательно**. Work-queue: claim+dispatch через PG, завершение через общий PG независимо от инстанса со стримом. |
| `2-8` на инстанс | Типичный прод-диапазон. |
| `>16` | Большие инсталляции — упирается в PG (`SELECT … FOR UPDATE SKIP LOCKED` нагружает primary). |

**Дефолт `0` — не для прод.** При `acolytes: 0` + `Conclave.CountLive > 1` Keeper **отказывается стартовать** (см. [§ Refuse-guard](#refuse-guard)).

### Расчёт числа Acolyte

Грубая оценка: на каждый одновременный прогон нужен ≥1 Acolyte. Прогон занимает Acolyte на время рендера (Vault-резолв + CEL + text/template) + SendApply (быстро, ms-секунды) + ожидание барьера (при serial-кадре — длительность всех задач + межхостовый sync).

Для типичной инсталляции до 100 incarnation с peak-частотой 5-10 одновременных apply на кластер — `acolytes: 2-4` на инстанс достаточно. Под более активную эксплуатацию (десятки одновременных apply) — наращивать пропорционально, мониторя `keeper_render_duration_seconds.p99` и латентность PG.

## Conclave — presence Keeper-инстансов

[Conclave](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) — Redis-реестр живых Keeper-инстансов (новая Redis-роль `e`):

- Каждый инстанс пишет ключ `keeper:instance:<kid>` с TTL `DefaultConclaveTTL=30s` при старте.
- Renew каждые `DefaultConclaveRenewInterval=10s` lease-goroutine-ой.
- Снимает ключ на graceful-shutdown.
- `RegisterInstance` отвергает занятый KID (`ErrConclaveKIDTaken`) — защита от дубль-KID при misconfiguration.

`Conclave.LiveKIDs()` / `Conclave.CountLive()` дают актуальный набор живых инстансов. Используется:

- **Refuse-guard** на старте при `acolytes: 0` (см. ниже).
- **Watchman / soul-shedding** — для координации балансировки нагрузки (планируется через Shepherd).

### Verify Conclave

```sh
redis-cli KEYS 'keeper:instance:*'
# 1) "keeper:instance:keeper-prod-01"
# 2) "keeper:instance:keeper-prod-02"
# 3) "keeper:instance:keeper-prod-03"

redis-cli TTL 'keeper:instance:keeper-prod-01'
# (integer) 27       # ≤ 30, renew каждые 10s
```

Прометей-метрика — отсутствует на момент написания (count Live-инстансов через `len(Conclave.LiveKIDs())` сейчас не экспонируется как `keeper_conclave_*`-collector; см. [open question в `disaster-recovery.md`](disaster-recovery.md#open-questions-runbook)).

## Refuse-guard: `acolytes: 0` в HA — отказ от старта

**Инвариант**: `N > 1` живых Keeper-инстансов **требует** `acolytes > 0` ([ADR-027(h)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) amendment). Реализован двумя слоями:

1. **Refuse при старте.** Keeper с `acolytes: 0` сверяет `Conclave.CountLive() > 1`; если другие инстансы живы — **отказывается стартовать** с `refusing to start: acolytes:0 unsafe under multi-keeper (Conclave reports N=X live instances); set acolytes>0 or set allow_unsafe_single_path_multi_keeper=true`, exit 1. Защищает от misconfig-а.
2. **Dispatch-time WARN.** Runtime safety-net: при попытке dispatch-а на не-Acolyte-пути с SID-lease у другого KID — `WARN` в логи (footgun проявится здесь).

### Явный opt-out

Для редких случаев (одна нода в HA-кластере временно работает на старом пути) — `allow_unsafe_single_path_multi_keeper: true` или env `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER=true`:

```yaml
acolytes: 0
allow_unsafe_single_path_multi_keeper: true   # ОПАСНО, см. ADR-027(h)
```

**Не использовать** в нормальной операционной модели. Известный footgun ([ADR-027(h)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)): прогон зависает в `applying`, incarnation не выходит из локированного состояния.

### Fail-open: Redis недоступен

При недоступном Redis (Conclave не отвечает) refuse-guard **fail-open** — старт не блокируется (см. [ADR-027(h)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim) Consequences). Это сознательный выбор: блокировка старта при отсутствии Redis превратила бы любой инцидент с Redis в catastrophic outage (никто не запустится). Цена — теоретическое окно misconfig в момент апгрейда с downtime Redis; в прод-инсталляции с HA Redis риск приемлем.

## Watchman / soul-shedding — изоляция-детект

[Watchman](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) — подсистема, реагирующая на потерю связи с общим состоянием (PG и/или Redis):

- **Probe-loop** периодически пингует PG + Redis.
- **Debounce-state-machine** переводит инстанс в `isolated` после N подряд фейлов (дефолт 3).
- **`StreamManager.CloseAll()`** — закрывает все локальные EventStream-стримы (отменяет per-stream `ctx`).
- Soul получает EOF → отрабатывает обычный failback на следующий endpoint из priority-листа.
- Обратный переход `isolated → healthy` ничего не «зовёт назад» — Souls возвращаются сами по priority.

Watchman срабатывает **только при реальной изоляции** (debounce), а не при разовом таймауте. Здоровый кластер не флапает.

В коде — `keeper/internal/watchman/`. Конфигурируется (на момент написания — пороги дефолтные, конфиг-блок не выделен в `keeper.yml`).

## Shepherd — балансировка нагрузки при scale-out

⚠️ **PLANNED/backlog**, не реализовано ([ADR-002 amendment](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)).

Проблема: при добавлении нового инстанса за LB существующие долгоживущие EventStream-стримы **залипают** на старых инстансах. LB балансирует только новые соединения; Soul сам не реконнектится. Failback срабатывает только при `priority > 1`. Новый инстанс простаивает до естественного churn (часы-дни).

Решение (когда появится): инстанс через Conclave видит свою долю нагрузки выше справедливой и **сбрасывает излишек** своих стримов (частичный `StreamManager.CloseAll`) с jitter / cap.

**До реализации** — workaround при scale-out:

- **Принудительный rolling-restart существующих инстансов** в окно обслуживания — Souls failback-ом перебалансируются включая новый инстанс. Безопасно при `acolytes > 0` (apply переживут через recovery / другие Acolyte).
- **Long-tail** — естественный churn рассосёт залипание за дни.

## L4-балансировщик: настройки

EventStream + Bootstrap-RPC — TCP, ставим L4-LB (haproxy, IPVS, AWS NLB, Yandex L4-LB):

```haproxy
# Пример HAProxy для EventStream (port 8443) — L4 TCP, least-conn
frontend eventstream_in
    bind *:8443
    mode tcp
    option tcplog
    default_backend eventstream_backends

backend eventstream_backends
    mode tcp
    balance leastconn
    # Health check — простой TCP-probe порта
    option tcp-check
    server keeper-1 keeper-1.internal:8443 check inter 5s
    server keeper-2 keeper-2.internal:8443 check inter 5s
    server keeper-3 keeper-3.internal:8443 check inter 5s
    # Запас по таймауту на долгоживущие стримы (gRPC bidi)
    timeout server 24h
    timeout client 24h
```

Ключевые параметры:

- **`balance leastconn`** — распределяет новые стримы равномерно. Round-robin даёт перекос при долгоживущих стримах разной интенсивности.
- **`timeout server 24h` / `timeout client 24h`** — gRPC keepalive держит соединение через NAT/firewall, но LB должен дать ему окно без сброса по idle.
- **TCP-probe**, не HTTP. gRPC через L4 не парсится — LB не знает healthz; TCP-probe порта `8443` достаточно.
- **Sticky session НЕ нужно** — SID-lease в Redis уже даёт уникальный «один Soul → один инстанс» state.

OpenAPI (`8080`) + MCP (`8081`) — L7-proxy с TLS termination, путь привычный (nginx / traefik / envoy http2 mode).

## Целевой масштаб 100k VM

Соответственно invariant «горячее → Redis, не PG» (см. [ADR-002 amendment](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) и [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). Валидировано мега-приёмкой на 3 keeper + 9-нодовом Redis-cluster:

- Presence Souls деривируется из Redis SID-lease (batch-EXISTS), **не из PG-`souls.status`** (синхронной записи нет на горячем пути).
- Heartbeat — Redis-роль (a), PG-`last_seen_at` flush throttled (default раз в `stale_after/3 = 30s` на SID).
- Conclave / leader-lease / pub/sub — Redis.

### Sizing infrastructure под 100k VM (приблизительно)

| Компонент | Хост | RAM | CPU |
|---|---|---|---|
| Keeper | 3+ инстансов | 8 GB | 8 vCPU |
| Postgres primary | dedicated | 32 GB | 8 vCPU + NVMe |
| Postgres replica (Patroni) | dedicated | 32 GB | 8 vCPU + NVMe |
| Redis cluster | 3+ master + replicas | 4 GB | 4 vCPU |
| Vault | 3 инстанса (raft) | 4 GB | 2 vCPU |
| OTel-collector | 1-3 инстанса | 2 GB | 4 vCPU |

Числа — порядки величин для starting point. Реальный sizing — по нагрузочным тестам с реальным workload-ом.

### Узкие места при росте

| Узкое место | Симптом | Решение |
|---|---|---|
| PG primary CPU на `apply_runs` claim | Латентность claim растёт, `keeper_grpc_apply_dispatch_total{result=ok}.rate` падает | Уменьшить `acolyte_batch` / распределить `acolyte_poll_interval` jitter; увеличить PG-CPU; рассмотреть read-replica для `Holder.Resolve` запросов |
| PG primary IO на `audit_log` | Backup tool отстаёт, `INSERT` лагают | Партиционирование `audit_log` по `created_at` (post-MVP); вынести audit-write на async worker (post-MVP) |
| Redis CPU на SID-lease check | Сценарии с большим roster (`on:` + `where:` по тысячам хостов) тормозят | Pipeline batching SID-lease check (есть в коде); увеличить ресурсы Redis-node, рассмотреть cluster-mode |
| OTel-collector | dropped spans в OTel-collector logs | Increase collector ресурсов, sampling в Keeper (sampler ещё конфигурируемый — backlog) |

## Scale-out процедура (добавление инстанса)

1. **Подготовить хост** по [`deployment.md`](deployment.md) (тот же deb/rpm, тот же конфиг с новым `kid`).
2. **`keeper.yml`** — изменить только `kid:` (плюс local cert-пути если уникальные); остальное — копия рабочего конфига.
3. **L4-LB** — добавить backend (см. выше).
4. **Запустить** `systemctl start keeper`. Conclave-presence появится в Redis через 1-2s.
5. **Verify**: `redis-cli KEYS 'keeper:instance:*'` показывает N+1 ключей.
6. **Балансировка** — см. [§ Shepherd](#shepherd--балансировка-нагрузки-при-scale-out) (на момент написания — без auto-balance после scale-out; rolling-restart существующих инстансов или ждать естественный churn).

## Scale-in процедура (graceful удаление инстанса)

1. **Drain LB** — снять инстанс из активного пула (`server keeper-3 ... disabled` в HAProxy, или health-check fail).
2. **`systemctl stop keeper`** — graceful shutdown:
   - Acolyte-пул прекращает claim новых заданий (`acolyte_drain_grace` default 5s).
   - In-flight claim-ы — отменены ctx-ом; Ward остаётся в БД (`claimed`/`running`), lease истечёт через `acolyte_lease` (30s), recovery-scan подберёт (если включён, см. [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md#включение-recovery-recovery-enable)).
   - Conclave-presence снимается явно.
   - Soul-стримы закрываются → Soul-ы failback на оставшиеся инстансы.
3. **Удалить хост** из инвентаря.

## См. также

- [`docs/keeper/config.md` → acolytes](../keeper/config.md#acolytes) — конфиг Acolyte-пула.
- [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md#включение-recovery-recovery-enable) — гейт включения `reclaim_apply_runs`.
- [`docs/architecture.md` → ADR-002 / ADR-006 / ADR-027](../architecture.md) — обоснования.
- [`monitoring.md`](monitoring.md) — метрики масштаба: `keeper_grpc_streams_active`, `keeper_reaper_lease_held`, и т.д.
- [`upgrade.md`](upgrade.md) — rolling upgrade поверх scale-out / scale-in.
