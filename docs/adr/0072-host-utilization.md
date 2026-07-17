# ADR-072. Host-Utilization — лёгкая телеметрия утилизации хостов через presence-канал

- **Контекст.** Оператору при заходе в инкарнацию нужна **свежая утилизация хостов** (CPU%/load/mem/disk/uptime) — «не задыхается ли инстанс прямо сейчас» — **без разворачивания Prometheus**. Живой утилизации сегодня нет нигде: `SoulprintReport` ([ADR-018](0018-soulprint-typed.md)) несёт **статические** grains (refresh 5m, CEL-адресуемо `soulprint.self.*` — targeting-факты, не живая нагрузка); node-exporter (детальный pull-путь метрик хоста — Prometheus-primary [ADR-024](0024-observability.md), эталон рядом с log-shipping [ADR-067](0067-vector-log-shipping.md)) — **дорогой opt-in**: требует scrape-инфраструктуры и разворачивается не везде. Нужен **третий, дешёвый push-слой** поверх уже существующего presence-стрима Soul→Keeper — чтобы latest-утилизация была под рукой сразу, без внешней инфры.

- **Решение.**

  - **(a) Новый независимый слой Host-Utilization.** Динамика утилизации — **волатильная**, живёт отдельным слоем. Отличать от Soulprint (статика grains, [ADR-018](0018-soulprint-typed.md)) и node-exporter (детальный pull). Прецедент независимого слоя наблюдаемости — [ADR-067](0067-vector-log-shipping.md) (Vector — лог-плоскость рядом с метриками). Host-Utilization **НЕ targeting-факт** (не CEL-адресуем, в `soulprint.*`-namespace не попадает) и **НЕ замена Prometheus** (грубое latest-окно, не история метрик).

  - **(b) Транспорт B — пиггибэк presence-стрима через новый `FromSoul.host_utilization = 10`** (сообщение `HostUtilization`, новый файл `proto/keeper/v1/utilization.proto`). Отвергнута **альтернатива A** (reserved-поля 8–14 в `SoulprintFacts` [ADR-018](0018-soulprint-typed.md)): семантическое нарушение (статика→живое в одном сообщении), медленный каденс soulprint 5m неприемлем для «прямо сейчас», засорение `soulprint`-CEL-namespace волатильными полями. Only-add по [ADR-012(c)](0012-keeper-soul-grpc.md) — новый oneof-слот, номера не реюзаются; breaking-изменение — только через `proto/keeper/v2/`.

  - **(c) Свой экономный pulse.** Дефолт-интервал **30s**, floor **10s** (anti-DoS: интервал ниже floor → clamp + warn), отдельно от soulprint-каденса 5m. Soul-side тикер; все `Send` из select-loop `handleSession` (**single writer** по стриму — без гонок на отправке).

  - **(d) Хранение только Redis** (горячее, не PG — инвариант проекта «hot data → Redis», [ADR-006](0006-cache-redis.md)): latest — Hash `soul:<sid>:util` + **TTL 3×интервал** (90s при дефолте); короткое окно спарклайнов — list-ring `soul:<sid>:util:win` (`LPUSH` + `LTRIM`, N=60). Выбор **list-ring, а не RedisTimeSeries**: стенд на `redis:7-alpine` (модуля TimeSeries нет), целевой DragonFly тоже без него → решение **переносимо** между обоими бэкендами.

  - **(e) Инвариант живости.** Host-Utilization **не влияет на авторитет живости** — им остаётся единственный lease `soul:<sid>:lock` ([ADR-006](0006-cache-redis.md)). Как любое app-сообщение ([ADR-012](0012-keeper-soul-grpc.md)), pulse обновляет `last_seen_at` — но это не индикатор живости (авторитет — lease), и его Send идёт через тот же единственный select-loop сессии, поэтому pulse не может «пережить» зависший loop и замаскировать мёртвого агента. Отсутствие утилизации (старый агент / коллектор выключен) — **graceful degrade**: presence, lease и UI не ломаются.

  - **(f) Свежесть (freshness).** API отдаёт признак `stale` (возраст `received_at` > TTL, либо ключа нет) — **протухшее НЕ выдаётся за свежее**. Паттерн skew Soulprint `collected_at`/`received_at` ([ADR-018](0018-soulprint-typed.md)): момент сбора ставит Soul, момент приёма — Keeper.

  - **(g) Аутентичность.** Утилизация принимается только с **аутентифицированным SID** (mTLS peer cert, `authenticatedSIDFrom`), **НИКОГДА из payload** — подмена чужого хоста невозможна (паттерн [ADR-012](0012-keeper-soul-grpc.md): SID в payload — echo для логов, авторитет — сертификат).

  - **(h) API.** `GET /v1/souls/{sid}/telemetry` — latest + window + freshness одного хоста; `GET /v1/incarnations/{name}/telemetry` — агрегат по хостам инкарнации (скоуп `coven && ARRAY[name]`).

- **Что нормирует, что откладывает.** **Нормирует** слой, транспорт (`FromSoul` #10 / `HostUtilization`), хранение (Redis latest + list-ring), инварианты (живость/свежесть/аутентичность), API (`/telemetry`). **Откладывает:** доставку конфига агенту + essence-override + тумблеры коллекторов — **NIM-87**; web-панель HostsTab — **NIM-88**; расширяемость набора коллекторов (новые метрики — только through only-add новых полей `HostUtilization`, generic-map не вводится).

- **Consequences.**
  - Новый файл `proto/keeper/v1/utilization.proto` + `FromSoul.host_utilization = 10`.
  - Soul-пакет `soul/internal/utilization` (сбор + тикер).
  - Keeper: `events_utilization.go` (приём oneof) + `redis/utilization.go` (latest Hash + list-ring + freshness).
  - Два huma-эндпоинта `/telemetry` (per-soul + per-incarnation агрегат).
  - Строка в [naming-rules](../naming-rules.md) — сообщение `HostUtilization` (+ вложенное `DiskUtilization`) и его поля.
  - Amendment к [ADR-024](0024-observability.md) (Host-Utilization как дополнительный лёгкий слой утилизации).
  - Опц. блок `utilization:` в `soul.yml` (интервал pulse).

- **Trade-offs.**
  - **push-pulse vs pull.** Pulse — дёшево и универсально (без scrape-инфры), но несёт меньше деталей: node-exporter остаётся для глубины (per-core, детальные счётчики).
  - **list-ring vs RedisTimeSeries.** Ring — переносим между `redis:7-alpine` и DragonFly, но без server-side downsampling/агрегаций; для крошечного окна спарклайнов (N=60) это приемлемо.
  - **типизированные фиксированные поля vs generic-map.** Фиксированные поля дают type-safety и чистый OpenAPI, но новые метрики требуют only-add правки proto (не «докинуть ключ в map»).

- **Amends / Related.** **Amends [ADR-024](0024-observability.md)** — добавляет лёгкий слой утилизации рядом с метриками (Prometheus pull, §a) / трейсами (OTel-bridge) / логами ([ADR-067](0067-vector-log-shipping.md), push). Related (НЕ amend): [ADR-018](0018-soulprint-typed.md) (статические grains — соседний слой, Host-Utilization их не дополняет и не заменяет); [ADR-012](0012-keeper-soul-grpc.md) (only-add `FromSoul` #10, аутентичность SID); [ADR-006](0006-cache-redis.md) (Redis-хранилище + lease-авторитет живости).
