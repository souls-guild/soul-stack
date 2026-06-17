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
    max_attempts: 5
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
- `retry.max_attempts` — сколько раз подряд пробовать **один** endpoint, прежде чем считать его выбывшим из текущего захода.
- `retry.backoff.initial` / `retry.backoff.max` / `retry.backoff.jitter` — экспоненциальный бэкофф с jitter между попытками к одному endpoint.
- `retry.handshake_timeout` — таймаут на установление TLS+gRPC-соединения с одним endpoint.
- `failback.enabled` — пытаться ли возвращаться на более предпочтительный приоритет после переключения вниз.
- `failback.interval` — как часто запускать попытку failback (типично 1h).
- `failback.spray` — равномерный jitter ±spray вокруг interval. **Не растягивает интервал**, защищает только от стадного эффекта (тысячи Souls не должны просыпаться синхронно). Базовый `interval` сохраняется.

## Алгоритм

### Установление первого подключения

1. Группируем `endpoints` по `priority`, сортируем приоритеты по возрастанию.
2. Берём минимальный приоритет, **перемешиваем** его endpoints (shuffle), пробуем **последовательно**: на каждый — per-endpoint retry-policy (`max_attempts` попыток с бэкоффом).
3. Первый успешно установивший gRPC-стрим побеждает; следующие endpoints не трогаем. Текущий приоритет фиксируется.
4. Если все endpoints на этом приоритете исчерпали `retry.max_attempts` — переходим на следующий приоритет, повторяем шаг 2 (с новым shuffle).
5. Если исчерпаны все приоритеты — ждём `retry.backoff.max`, начинаем сначала с минимального приоритета.

### Failback (возврат на более предпочтительный приоритет)

6. После того как стрим установлен на приоритете `current` и `failback.enabled: true`:
   - запускаем таймер `failback.interval` со случайным сдвигом в пределах `±failback.spray`;
   - по срабатыванию — последовательная попытка по приоритетам **от 1 до current-1**: на каждом приоритете endpoints перемешиваются и пробуются по очереди;
   - первый успех на приоритете K (K < current): открываем новый стрим, **затем** закрываем старый (zero-downtime), `current := K`, таймер запускается заново;
   - если все попытки провалились — ждём следующий `failback.interval ± spray`, без быстрых ретраев. Никуда не спешим.

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
