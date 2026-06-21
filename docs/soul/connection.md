# Подключение Soul к Keeper-кластеру

Спецификация алгоритма `priority + failback` для pull-режима (`transport: agent`). В push-режиме (`transport: ssh`) алгоритм **не применяется** — Keeper сам инициирует SSH-сессию к хосту, см. [architecture.md → Push-режим](../architecture.md#push-режим-keeperpush).

## Соглашения

- В конфиге Soul-а задан список endpoints Keeper-а с числовым полем `priority`.
- **Меньшее число = более предпочтительно** (как DNS MX, systemd, ip route).
- **По умолчанию `priority: 1`** — все равны, самый предпочтительный уровень. Указываем явно только когда хотим кого-то понизить.
- Между приоритетами — **последовательно** (1 → 2 → 3).
- Внутри одного приоритета — **последовательно** с **рандомизацией порядка** endpoints на каждой попытке (shuffle). Это:
  - даёт равномерную нагрузку: 1000 Souls с одинаковым набором endpoints приоритета 2 не ломятся синхронно в первый по списку;
  - проще реализовать корректно (не нужно отменять параллельные TLS-handshake-и при появлении победителя);
  - стоимость — чуть больше времени, если первый из shuffle-списка недоступен (нужен `handshake_timeout`, потом следующий). При разумных значениях `handshake_timeout` (10s) это терпимо.

## YAML-конфиг

```yaml
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
    - host: k2.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
    - host: k3.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 2
    - host: k4.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 2
    - host: k1.dc2.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 3

  retry:
    max_attempts: 2          # per-endpoint попытки до spray; default 2
    backoff:
      initial: 1s
      max: 30s
      jitter: true
    handshake_timeout: 10s

  failback:
    enabled: true
    interval: 1h
    spray: 10m          # фактический момент = interval ± spray, равномерно
```

Полная раскладка `soul.yml` (включая `paths`, `tls`, `soulprint`, `cleanup`, `logging`, `metrics`, `otel`) — в [config.md](config.md).

## Две фазы, два порта

Keeper держит два gRPC-listener-а на **разных портах** (ADR-012): Bootstrap (server-only TLS) и EventStream (mTLS). Хосты кластера для обеих фаз совпадают, поэтому endpoint несёт один `host` и два порта — список не дублируется.

| Фаза | Команда | Порт | TLS |
|---|---|---|---|
| Онбординг | `soul init` | `bootstrap_port` | server-only TLS (Soul верифицирует Keeper по `keeper.tls.ca`) |
| Рабочий цикл | `soul run` | `event_stream_port` | mTLS (SoulSeed-cert ↔ кластерная CA из seed) |

Оба порта **обязательны** при наличии endpoint-а («безопасность на первом месте»: явность важнее краткости, в проде порты обычно разные; молчаливого ухода bootstrap на event_stream-порт нет).

`priority` упорядочивает **обе** фазы (хосты те же), но алгоритмы перебора различаются. **EventStream** внутри одного приоритета перемешивает endpoints (shuffle / spray, см. §Алгоритм). **Bootstrap** этого не делает: one-shot-перебор идёт по priority от меньшего к большему **без in-group shuffle** — порядок детерминирован. Также к bootstrap **неприменим failback** (онбординг one-shot, переключения вниз/вверх нет; failback работает только в EventStream-фазе).

## Параметры

- `endpoints[].host` — хост Keeper-инстанса (FQDN или IP), общий для обеих фаз.
- `endpoints[].event_stream_port` — порт EventStream-listener-а (mTLS, фаза `soul run`). Обязателен, 1..65535.
- `endpoints[].bootstrap_port` — порт Bootstrap-listener-а (server-only TLS, фаза `soul init`). Обязателен, 1..65535.
- `endpoints[].priority` — целое число ≥ 1, по умолчанию `1`. Упорядочивает обе фазы.
- `retry.max_attempts` — сколько раз подряд пробовать **один** endpoint при retriable-ошибке (см. §Классификация ошибок), прежде чем spray-ить к следующему endpoint. **Default `2`** (не `5`): per-endpoint упорство держим малым — worst-case failover растёт как `N×max_attempts×handshake_timeout`, а устойчивость даёт spray по fallback-list-у + внешний exponential-reconnect (§Установление первого подключения шаг 5). Опущенное/`0` → `2`.
- `retry.backoff.initial` / `retry.backoff.max` / `retry.backoff.jitter` — параметры экспоненциального backoff-а между **полными проходами** по fallback-list-у (внешний reconnect-loop, §Установление шаг 5 и §Lease-held). **Между попытками к одному endpoint** (per-endpoint retry) пауза другая: `backoff.initial` берётся **плоско** (без экспоненциального роста), с `±25%` jitter при `backoff.jitter: true`. Рост cap-а — только между полными проходами, не между попытками к одному endpoint. Отдельного конфиг-ключа на inter-attempt-паузу нет (переиспользуется `backoff.initial`/`backoff.jitter`). ⚠️ Inter-attempt-пауза **restart-required**: читается один раз при сборке EventStream-клиента; SIGHUP-hot-reload её не обновляет (в отличие от reconnect-backoff `keeper.retry.backoff.*`, который перечитывается per-iteration).
- `retry.handshake_timeout` — таймаут на установление TLS+gRPC-соединения с одним endpoint.
- `failback.enabled` — пытаться ли возвращаться на более предпочтительный приоритет после переключения вниз.
- `failback.interval` — как часто запускать попытку failback (типично 1h).
- `failback.spray` — равномерный jitter ±spray вокруг interval. **Не растягивает интервал**, защищает только от стадного эффекта (тысячи Souls не должны просыпаться синхронно). Базовый `interval` сохраняется.

## Алгоритм

### Установление первого подключения

1. Группируем `endpoints` по `priority`, сортируем приоритеты по возрастанию.
2. Берём минимальный приоритет, **перемешиваем** его endpoints (shuffle), пробуем **последовательно**: на каждый endpoint — per-endpoint retry-loop (`max_attempts` попыток `dialOne` **подряд** к ТОМУ ЖЕ endpoint-у, между попытками — плоская пауза `backoff.initial ± jitter`). Повтор к тому же endpoint-у делается **только при retriable-ошибке** (§Классификация ошибок); non-retriable (lease-held / auth / контрактный отказ) — сразу spray к следующему endpoint без повтора.
3. Первый успешно установивший gRPC-стрим побеждает; следующие endpoints не трогаем. Текущий приоритет фиксируется.
4. Если endpoint исчерпал `retry.max_attempts` (или вернул non-retriable-ошибку) — spray к следующему endpoint этого приоритета; когда все endpoints приоритета выбыли — переходим на следующий приоритет, повторяем шаг 2 (с новым shuffle).
5. Если исчерпаны все приоритеты — это один полный проход по fallback-list-у; внешний reconnect-loop ждёт `delay` (экспоненциальный рост между проходами, capped к `retry.backoff.max`) и начинает сначала с минимального приоритета. Именно здесь, а НЕ между попытками к одному endpoint, работает экспоненциальный рост backoff-а.

### Failback (возврат на более предпочтительный приоритет)

6. После того как стрим установлен на приоритете `current` и `failback.enabled: true`:
   - запускаем таймер `failback.interval` со случайным сдвигом в пределах `±failback.spray`;
   - по срабатыванию — последовательная попытка по приоритетам **от 1 до current-1**: на каждом приоритете endpoints перемешиваются и пробуются по очереди;
   - первый успех на приоритете K (K < current): открываем новый стрим, **затем** закрываем старый (zero-downtime), `current := K`, таймер запускается заново;
   - если все попытки провалились — ждём следующий `failback.interval ± spray`, без быстрых ретраев. Никуда не спешим.

### Классификация ошибок (что ретраит per-endpoint, что сразу spray)

Per-endpoint retry-loop (шаг 2) повторяет `dialOne` к **тому же** endpoint-у только при transient-ошибке транспорта; неисправимый отказ повтором не лечится — нужен другой endpoint, поэтому такой фейл сразу прерывает loop и переходит к spray. Матрица нормативна (матчинг по gRPC-status-коду):

| Класс | gRPC-коды | Поведение per-endpoint |
|---|---|---|
| **retriable** | `Unavailable`, `DeadlineExceeded`, `Internal`, `Unknown`, `Aborted` + локальный handshake-timeout (не gRPC-status → `Unknown`) | повтор к тому же endpoint до `max_attempts`, между попытками — плоская пауза `backoff.initial ± jitter` |
| **non-retriable (spray-on)** | `AlreadyExists` (lease-held), `Unauthenticated`, `PermissionDenied`, `InvalidArgument`, `FailedPrecondition`, `Unimplemented` | ровно **одна** попытка `dialOne`, дальше сразу spray к следующему endpoint |
| **default** | любой неклассифицированный код | retriable (консервативно) |

Логика: `Unauthenticated`/`PermissionDenied` — auth-проблема (cert/RBAC), сама не исправится за `backoff.initial`; `InvalidArgument`/`FailedPrecondition`/`Unimplemented` — контрактный отказ; `AlreadyExists` — другой Keeper держит SID-lease (см. §Lease-held ниже). Во всех этих случаях повтор к тому же endpoint бессмыслен. Transient-флейк транспорта (`Unavailable`/timeout/…) — наоборот, второй заход к тому же endpoint часто проходит.

Связь с §Lease-held: `AlreadyExists` намеренно **не ретраится** per-endpoint (ровно один `dialOne` на каждый lease-held endpoint). Это комплементарно lease-held soft-failure backoff-у внешнего reconnect-loop-а — per-endpoint retry не создаёт churn на выживших Keeper-ах, пока lease ещё держится; быстрый возврат после force-release обеспечивает именно модест-cap reconnect-loop-а, а не повтор к одному endpoint.

### Lease-held soft-failure (reconnect после краха holder-а)

Отдельная ветка reconnect-backoff, **внутри** алгоритма выше — не новый параметр, а различение причины фейла Dial.

**Что это.** После краха Keeper-инстанса, который держал стрим (holder), SID-lease этого Soul-а (`soul:<sid>:lock`) живёт до истечения Conclave-presence прежнего holder-а (~30s). Пока lease держится, reconnect того же SID к **выжившим** Keeper-ам отвергается на handshake gRPC-кодом `AlreadyExists` (сессия отброшена, хотя транспорт поднялся). Soul различает этот **lease-held soft-failure** от обычного transport-сбоя.

**Backoff не такой, как у transport-сбоя.** При lease-held Soul **не** капирует backoff общим transport-cap-ом `retry.backoff.max` (по умолчанию 30s), а отдельным модест-cap-ом **3s** (внутренний инвариант, не конфиг-ключ). Цель — recovery-latency:

- не долбить выживших Keeper-ов log-шумом и churn-ом всё presence-окно;
- переподключиться в пределах секунд после того, как lease освободится, а не ждать раздутый общий cap.

Lease освобождает **keeper-сторона**: после истечения presence прежнего holder-а выживший Keeper делает presence-gated force-release SID-lease-а (доказанно-мёртвый holder → CAS-перехват ключа на новый KID). Soul-сторона комплементарна — терпеливо ретраит с модест-backoff, пока keeper не освободит ключ. Подробности keeper-стороны (presence-gate, split-brain-безопасность, residual-окно ≤Conclave-TTL) — [recovery-reclaim-apply-runs.md → presence-gated force-release SID-lease](../operations/recovery-reclaim-apply-runs.md#presence-gated-force-release-sid-lease--сокращение-окна-невидимости-soul-а).

**Spray не затронут.** `AlreadyExists` на **одном** endpoint не прерывает перебор fallback-list — следующий endpoint мог уже перехватить lease после force-release. Модест-cap включается, только когда **все** пробованные endpoint-ы отдали `AlreadyExists` (значит lease ещё держится везде); если хоть один фейл — не `AlreadyExists`, это transport-сбой → общий exponential до `retry.backoff.max`.

## Гарантии

- В каждый момент Soul держит ровно один активный стрим к одному Keeper-у.
- Внутри одного приоритета — последовательно с рандомизацией порядка endpoints; между приоритетами — последовательно.
- Failback максимум один раз за `interval`, со случайным сдвигом ±`spray`.
- Сценарий «один ЦОД-локальный Keeper приоритета 1, два резервных приоритета 2, один кросс-ЦОД приоритета 3, возврат раз в час» выражается ровно через эти три параметра.

## См. также

- [config.md](config.md) — раскладка `soul.yml` целиком, включая блок `keeper:`.
- [onboarding.md](onboarding.md) — `soul init` использует тот же алгоритм при первом CSR.
- [architecture.md → Подключение Soul: priority и failback](../architecture.md#подключение-soul-priority-и-failback) — короткий архитектурный обзор и связь с push-режимом.
- [architecture.md → ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) — обоснование bidi-стрима и HA Keeper-кластера.
- [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — рабочий пример конфига.
