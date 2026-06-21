# Формат `soul.yml`

Конфиг `soul`-агента на управляемом хосте. Один файл на хост, лежит по соглашению в `/etc/soul/soul.yml` (точный путь — на усмотрение оператора, бинарь принимает `--config <path>`). Применяется и в pull-демоне, и в push-режиме — но в push большая часть полей не нужна, потому что Keeper передаёт `soul`-у отрендеренный план прогона (`ApplyRequest` как protojson) через stdin одной командой `soul apply`, а не через долгоживущую конфигурацию. Сырой Destiny/Essence на push-хост не попадает.

Рабочий пример со всеми полями — [`examples/soul/soul.yml`](../../examples/soul/soul.yml). Этот документ **нормативно типизирует** все поля — по нему пишется парсер.

**У Soul нет блока `auth:`.** Аутентификация к Keeper-у — только через mTLS (SoulSeed), см. [`identity.md`](identity.md), [`onboarding.md`](onboarding.md). JWT-блок `auth:` есть только у Keeper-а (для операторов через OpenAPI/MCP, см. [`docs/keeper/config.md → auth:`](../keeper/config.md#auth) и [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).

## Конвенции типов

| Запись | Смысл |
|---|---|
| `string` | произвольная строка UTF-8. |
| `int` | знаковое 64-битное целое. |
| `bool` | `true` / `false`. |
| `duration` | Go-duration string (`5s` / `500ms` / `1h30m`). |
| `enum{a,b,c}` | строка из явно перечисленного множества. |
| `string(host:port)` | строка `host:port`; `host` — IP или DNS-имя, `port` — `1..65535`. |
| `fqdn` | DNS-имя по RFC 1035/1123 hostname: набор labels через точку, каждый label `^[a-z0-9-]{1,63}$`, не начинается и не заканчивается дефисом. Пример: `redis-01.prod.example.com`. |
| `path` | абсолютный путь в локальной ФС хоста. |
| `list<T>` | как обычно. |

`default: —` обозначает обязательное поле без default-а. Опциональные поля помечены `optional`. Значения `enum{…}` — lowercase ASCII, без пробелов.

## Раскладка

```yaml
# sid: redis1.cache-test-dev.example         # ОПЦ.: явный SID; по умолчанию = FQDN

paths:
  modules: /var/lib/soul-stack/modules        # кеш custom-модулей
  seed:    /var/lib/soul-stack/seed           # каталог SoulSeed (версионная раскладка: current -> vN)

keeper:
  endpoints:                                  # см. connection.md
    - host: k1.dc1.example
      event_stream_port: 9443                  # mTLS, `soul run`
      bootstrap_port: 9442                     # server-only TLS, `soul init`
  retry: {...}
  failback: {...}
  max_apply_size_mb: 8                         # recv-лимит ApplyRequest, default 8 MiB
  tls:
    ca: /var/lib/soul-stack/seed/ca.crt

soulprint:
  refresh_interval: 5m

cleanup:
  modules_ttl_days: 30
  run_interval: 24h

logging:
  level: info
  format: json
  file: /var/log/soul/soul.log         # пусто/опущено → stderr без ротации
  rotation: { max_size_mb: 50, max_age_days: 7, max_files: 5, compress: true }

metrics:
  enabled: true
  listen: "127.0.0.1:9091"
  basic_auth:                            # опц.; default — loopback-bind без auth
    enabled: true
    username: scrape
    password_file: /etc/soul/metrics-password   # mode 0400, одна строка

otel:
  enabled: true
  endpoint: "k1.dc1.example:4317"
```

## Поля верхнего уровня

### `sid:`

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `sid` | `fqdn` | FQDN хоста | Опциональный. По умолчанию вычисляется как FQDN хоста (см. [identity.md → Идентичность](identity.md#идентичность)). Переопределение допустимо, но требуется крайне редко — например, в окружениях, где FQDN недостаточно стабилен. При несовпадении `sid` в конфиге с тем, на который выписан SoulSeed, подключение к Keeper-у не пройдёт TLS-уровень. |

### `paths:`

Файловые пути на хосте. Если хост следует convention-раскладке `/var/lib/soul-stack/`, эти поля можно опустить и положиться на дефолты бинаря.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `paths.modules` | `path` | `/var/lib/soul-stack/modules` | Каталог кеша custom-модулей. Внутри лежат файлы вида `soul-mod-<имя>-<sha>` (см. [modules.md](modules.md)). |
| `paths.seed` | `path` | `/var/lib/soul-stack/seed` | Каталог с SoulSeed в версионной раскладке: активная версия — симлинк `current` на каталог `vN/` с `cert.pem`/`key.pem`/`ca.pem` (нормативно — [identity.md → On-disk-формат](identity.md#on-disk-формат-pathsseed-нормативно)). Приватный ключ генерируется локально при `soul init` и из этого каталога никуда не уходит. Для оператора значение не меняется: достаточно указать каталог. См. [onboarding.md](onboarding.md). |

В push-режиме `paths.seed` не используется — у push-хоста нет SoulSeed.

### `keeper:`

Подключение к Keeper-кластеру: список endpoints, retry-policy, failback, mTLS-материалы. **Полная нормативная спецификация алгоритма и семантика каждого поля — [connection.md](connection.md).** Здесь — типизация и краткий смысл.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `keeper.endpoints` | `list<KeeperEndpoint>` | — | Непустой список endpoints Keeper-кластера; см. [connection.md → YAML-конфиг](connection.md#yaml-конфиг). Минимум один. |
| `keeper.endpoints[].host` | `string` | — | **Обязателен.** Хост одного Keeper-инстанса (FQDN или IP), общий для обеих фаз. Пустой → diag `missing_required_field`. |
| `keeper.endpoints[].event_stream_port` | `int` (1..65535) | — | **Обязателен.** Порт EventStream-listener-а (mTLS, фаза `soul run`). Отсутствует → diag `event_stream_port_required`; вне диапазона → `port_out_of_range`. |
| `keeper.endpoints[].bootstrap_port` | `int` (1..65535) | — | **Обязателен.** Порт Bootstrap-listener-а (server-only TLS, фаза `soul init`). Отсутствует → diag `bootstrap_port_required`; вне диапазона → `port_out_of_range`. Обязателен явно (ADR-012(b), «безопасность на первом месте»): никакого молчаливого ухода bootstrap на event_stream-порт. |
| `keeper.endpoints[].priority` | `int` (≥1) | `1` | Приоритет (меньше = предпочтительнее, [connection.md → Соглашения](connection.md#соглашения)). Упорядочивает **обе** фазы (хосты совпадают). |
| `keeper.retry.max_attempts` | `int` (≥1) | `2` | Сколько раз подряд пробовать один endpoint при retriable-ошибке (per-endpoint retries), прежде чем spray-ить к следующему endpoint. Default `2` (не `5`): per-endpoint упорство держим малым, устойчивость даёт spray + внешний reconnect; см. [connection.md → Параметры](connection.md#параметры) и [→ Классификация ошибок](connection.md#классификация-ошибок-что-ретраит-per-endpoint-что-сразу-spray). Опущенное/`0` → `2`. |
| `keeper.retry.backoff.initial` | `duration` | `1s` | Начальный интервал экспоненциального backoff-а **между полными проходами** по fallback-list-у (внешний reconnect-loop). **Также** переиспользуется как **плоская** (без роста) пауза между попытками к одному endpoint в per-endpoint retry — отдельного ключа на inter-attempt-паузу нет. ⚠️ inter-attempt-паузу hot-reload не подхватывает (restart-required), reconnect-backoff — подхватывает. См. [connection.md → Параметры](connection.md#параметры). |
| `keeper.retry.backoff.max` | `duration` | `30s` | Верхняя граница backoff-а между полными проходами (на per-endpoint retry не влияет — там пауза плоская). |
| `keeper.retry.backoff.jitter` | `bool` | `true` | Применять ли случайный jitter к бэкоффу. По [connection.md → YAML-конфиг](connection.md#yaml-конфиг) — `bool` (`true`/`false`), не duration: это **тумблер** «использовать ли jitter», конкретная величина — внутренняя для алгоритма бэкоффа. |
| `keeper.retry.handshake_timeout` | `duration` | `10s` | Таймаут на установление TLS+gRPC-соединения с одним endpoint. |
| `keeper.failback.enabled` | `bool` | `true` | Пытаться ли возвращаться на более предпочтительный приоритет после переключения вниз. |
| `keeper.failback.interval` | `duration` | `1h` | Как часто запускать попытку failback. |
| `keeper.failback.spray` | `duration` | `10m` | **Амплитуда** случайного jitter-а вокруг `interval` (фактический момент = `interval ± spray`, равномерно). Защита от стадного эффекта при тысячах Souls. По [connection.md → Параметры](connection.md#параметры) — `duration`, не bool: тип согласован с «не растягивает интервал, защищает от синхронных пробуждений». |
| `keeper.tls.ca` | `path` | — | Путь к CA-сертификату Keeper-кластера. Soul использует его, чтобы валидировать серверную сторону при mTLS-handshake. Сам клиентский сертификат и приватник лежат в `paths.seed/`. |
| `keeper.max_apply_size_mb` | `int` (МиБ, ≥1) | `8` | Потолок размера одного входящего FromKeeper-сообщения, прежде всего `ApplyRequest` с пачкой отрендеренных `RenderedTask` (рендер Destiny — Keeper-side, [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Применяется как `grpc.MaxCallRecvMsgSize` в dial EventStream-клиента, заменяя малый gRPC-дефолт recv (4 MiB), которого не хватает крупному Destiny. `0`/опущено → дефолт `8`; `<1` → diag `value_out_of_range`. **Должен быть ≥ Keeper-send-лимиту** (`listen.grpc.event_stream.max_apply_size_mb` в [keeper/config.md](../keeper/config.md#listen)), иначе Keeper отправит то, что Soul отвергнет; дефолты обеих сторон совпадают (8 MiB). В push-режиме не применяется (план приходит через stdin `soul apply`, не по gRPC). |

В push-режиме блок `keeper:` игнорируется — Soul-host получает отрендеренный план (`ApplyRequest` protojson) через stdin `soul apply` от Keeper-а. (Operator-side CLI-форма push-операции — `keeper.push.apply` — пока не нормирована, отдельный backlog; host-side entry-point `soul apply` зафиксирован — [keeper/push.md](../keeper/push.md).)

### `soulprint:`

Параметры сборки Soulprint (фактов о хосте). Typed-схема MVP — [`soulprint.md`](soulprint.md), фиксация — [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `soulprint.refresh_interval` | `duration` | `5m` | Как часто Soul пересобирает факты и (для pull) отдаёт обновление по стриму через `SoulprintReport`. |

Набор полей `SoulprintFacts` — нормативно в [`soulprint.md`](soulprint.md); собирается Soul-агентом по фиксированной таблице (`os.family`/`pkg_mgr`/`init_system` и т.д.) и в конфиге **не декларируется**. User-collectors (`/etc/soul/soulprint.d/*`) — отложены, см. [open Q №22](../architecture.md#текущие) (требует решений по sandbox/правам/format коллектора — отдельный ADR).

### `cleanup:`

Локальная чистка кеша на хосте. Применяется в pull-режиме (демон выполняет периодический проход). В push-режиме чистка идёт со стороны `keeper.push`, не из `soul.yml`. См. [modules.md → Локальный cleanup](modules.md#локальный-cleanup-кеша).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `cleanup.modules_ttl_days` | `int` (дней) | `30` | Сколько **дней** неиспользованная версия модуля или бинаря живёт в `/var/lib/soul-stack/{bin,modules}/`, прежде чем демон её удалит. Единица фиксирована в имени поля. |
| `cleanup.run_interval` | `duration` | `24h` | Как часто демон запускает проход по кешу. |

`modules_ttl_days` — намеренное исключение из конвенции `duration`: единица фиксирована в имени поля для удобства оператора (TTL модулей естественно мерять в днях).

### `logging:`

Логи Soul-а. Ротация — встроенная (сквозное требование, см. [requirements.md](../requirements.md)).

Поведение зависит от `logging.file`:

- **`logging.file` не задан** → вывод в `stderr` без ротации (dev-режим, удобно под systemd/journald и в контейнере).
- **`logging.file` задан** → запись в этот файл с ротацией по размеру/возрасту (встроенный ротатор), архивы складываются рядом по шаблону `<file>-<timestamp>.<ext>`.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `logging.level` | `enum{debug,info,warn,error}` | `info` | Уровень логирования. |
| `logging.format` | `enum{json,text}` | `json` | `json` для машинной обработки, `text` для человека. |
| `logging.file` | `string` (путь) | — (stderr) | Путь к лог-файлу. Пусто — вывод в `stderr` без ротации. |
| `logging.rotation.max_size_mb` | `int` (МБ) | `50` | Размер одного файла лога до ротации. |
| `logging.rotation.max_age_days` | `int` (≥0) | `7` | Сколько дней хранить ротированный файл. Пусто/`0` → дефолт билдера (7 дней); «без ограничения по возрасту» в текущей грамматике не выражается (MVP-ограничение — поле перешло на плоский `int`, различение «0 vs не задано» снято). |
| `logging.rotation.max_files` | `int` | `5` | Сколько ротированных файлов хранить. |
| `logging.rotation.compress` | `bool` | `true` | Сжимать ли ротированные файлы. В MVP `false` не отключает сжатие (всегда `true`); отключение появится позже. |

Поля `logging.rotation.*` применяются только когда задан `logging.file`.

### `metrics:`

Публикация метрик Soul-а (Prometheus-совместимый эндпоинт). Сквозное требование.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `metrics.enabled` | `bool` | `true` | Включить публикацию. При `false` listener `/metrics` не поднимается. |
| `metrics.listen` | `string(host:port)` | `127.0.0.1:9091` | Локальный адрес выделенного `/metrics` listener-а. По умолчанию **loopback** (`127.0.0.1`), чтобы не торчать наружу — скрейпер ходит через node_exporter-паттерн или sidecar. Если `enabled: true`, но `listen` пуст — применяется дефолт `127.0.0.1:9091`. |
| `metrics.basic_auth.enabled` | `bool` | `false` | Включить HTTP Basic-auth на `/metrics`. По умолчанию выключено — защита эндпоинта обеспечивается loopback-bind-ом. Нужно при bind-е не на loopback (scrape с другого хоста). |
| `metrics.basic_auth.username` | `string` | — | Имя пользователя для Basic-auth. Обязателен при `basic_auth.enabled: true`. |
| `metrics.basic_auth.password_file` | `string(path)` | — | Путь к файлу с паролем (одна строка, trailing-newline отбрасывается). Обязателен при `basic_auth.enabled: true`. Источник — **файл**, не vault-ref: у Soul нет vault-клиента ([ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Plaintext-пароль прямо в YAML запрещён грамматикой — только путь. Права на файл — забота оператора (рекомендация `0400`). Существование файла проверяется на старте `soul run` (fail-fast при отсутствии/пустоте), не в `soul-lint`. |

На Soul метрики опциональны (в отличие от Keeper, где `listen.metrics` обязателен): не каждый управляемый хост хочет открывать порт. Сквозное требование «публикация метрик» из [`requirements.md`](../requirements.md) относится к компонентам в целом — Keeper всегда экспонирует, Soul по выбору оператора.

> **Auth на Soul-`/metrics` — через `password_file`.** В отличие от Keeper (`metrics.auth.basic`, пароль из vault-ref, [keeper/config.md](../keeper/config.md#metrics)), у Soul нет vault-клиента ([ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)), которым резолвить креды. Поэтому источник пароля — файл на диске (`metrics.basic_auth.password_file`); сама constant-time проверка — общий хелпер `obs.ServeMetrics` (тот же, что у Keeper). При `basic_auth.enabled: false` (default) защита `/metrics` — loopback-bind.

### `otel:`

OpenTelemetry-трейсинг. Сквозное требование.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `otel.enabled` | `bool` | `false` | Включить OTLP-экспорт. |
| `otel.endpoint` | `string(host:port)` | — | Адрес OTLP-receiver-а (gRPC). Обязателен при `enabled: true`. В дефолтной поставке принимающая сторона — Keeper-инстанс с включённым OTLP-приёмом. |
| `otel.export_metrics` | `bool` | `false` | Опц. push метрик по OTLP в дополнение к Prometheus-scrape ([ADR-024 §1.2](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) / [observability.md §5](../observability.md)). **Заглушка под Slice 2:** поле читается, но OTLP-метрик-pipeline ещё не поднимается — в Slice 0 экспортируются только трейсы. По умолчанию метрики идут только через Prometheus-`/metrics`. |

При `enabled: true` поле `endpoint` обязательно; при `enabled: false` блок может быть опущен целиком.

### `plugin_runtime:`

```yaml
plugin_runtime:
  socket_dir: /var/run/soul-stack/plugins
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false
```

Lifecycle host-процесса для плагинов, запускаемых на Soul-стороне (`soul_module` — бинари `soul-mod-<name>`): таймауты handshake-а и shutdown-а, whitelist capabilities и resource-конфликт-политика, опциональный TLS на plugin-сокете. Применяется и в pull-демоне, и в push-режиме — set capabilities/конфликтов одинаков. Полная семантика lifecycle, формат handshake-строки, диаграмма запуска плагина — [`../keeper/plugins.md → Lifecycle`](../keeper/plugins.md#lifecycle); нормативное решение — [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugin_runtime.socket_dir` | `path` | `/var/run/soul-stack/plugins/` | Каталог, в котором Soul-host создаёт Unix-domain socket-ы плагинов (`<namespace>-<name>-<pid>.sock`). Создаётся с mode `0700`, owned by service user `soul` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Путь различается для Soul-host-а (`soul-stack`) и Keeper-host-а (`soul-stack-keeper`), см. [`../keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime). |
| `plugin_runtime.startup_timeout` | `duration` | `10s` | Время от `fork()` плагин-процесса до появления handshake-строки `"soul_stack":"plugin-v1"` в stdout. Превышение — host шлёт SIGTERM, далее SIGKILL по истечении `shutdown_grace` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle), [`../keeper/plugins.md → Поведение host-а при handshake`](../keeper/plugins.md#поведение-host-а-при-handshake)). |
| `plugin_runtime.shutdown_grace` | `duration` | `10s` | Время от SIGTERM до SIGKILL. SDK предоставляет signal-handler, плагин должен закрыть in-flight RPC и завершиться сам в пределах этого окна ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). |
| `plugin_runtime.allowed_capabilities` | `list<enum>` | все 6 capabilities (см. YAML-block выше) | Closed enum (полный каталог — [`../keeper/plugins.md → required_capabilities-таблица`](../keeper/plugins.md#required_capabilities-таблица), [ADR-020(f)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Whitelist: `soul-lint` отвергает destiny **до запуска**, если `manifest.required_capabilities` плагина ⊄ этого списка. Default разрешает все шесть; оператор сужает по политике безопасности. Значения вне closed enum-а парсер отвергает с `unknown_capability`. |
| `plugin_runtime.conflict_policy` | `enum{warn,fail}` | `warn` | Политика на случай, когда два плагина в одном прогоне claim-ят один и тот же ресурс в `side_effects` (одинаковая пара `<resource_type>:<value>`). `warn` — host пишет audit-event и продолжает прогон; `fail` — шаг помечается `failed`, причина `policy_violation` отражается в диагностическом канале `TaskEvent` / `RunResult` ([ADR-020(g)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle), [`../keeper/plugins.md → Поведение host-а на side_effects`](../keeper/plugins.md#поведение-host-а-на-side_effects)). |
| `plugin_runtime.enable_tls` | `bool` | `false` | Включение mTLS на plugin-сокете. В MVP — `false`: безопасность обеспечивается file-permissions `0700` на Unix-socket ([ADR-020(h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Post-MVP — `true` использует поле `server_cert` (base64-PEM) handshake-строки, уже зарезервированное forward-compat-резервом. До закрытия отдельной задачи поведение при `true` парсер отвергает с `tls_not_implemented`. |

#### Hot-reload блока `plugin_runtime:`

Per-поле политика (общий механизм перезагрузки нормирован [ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml), см. [Hot-reload](#hot-reload) ниже):

| Поле | Reload без рестарта `soul`-процесса | Обоснование |
|---|---|---|
| `allowed_capabilities` | да | Параметр конкретного запуска плагина: host читает значение при fork-е, новые прогоны видят новое значение. |
| `conflict_policy` | да | Оценка конфликта `side_effects` происходит в момент сборки прогона, in-memory. |
| `startup_timeout` | да | Применяется к новым plugin-прогонам, не аффектит уже запущенные. |
| `shutdown_grace` | да | То же. |
| `socket_dir` | **нет, требует рестарта** | Меняет внешнюю поверхность host-а (file-system layout); уже запущенные plugin-сокеты лежат в старой директории. |
| `enable_tls` | **нет, требует рестарта** | Меняет TLS-handshake-цепочку plugin-протокола. |

Правило: меняем-без-рестарта то, что используется как параметр конкретного plugin-прогона; требуем-рестарт то, что меняет внешнюю поверхность host-а. Симметрично с Keeper-side, см. [`../keeper/config.md → Hot-reload блока plugin_runtime`](../keeper/config.md#hot-reload-блока-plugin_runtime).

В push-режиме блок `plugin_runtime:` применяется так же — Soul-host поднимает плагин из переданного Keeper-ом артефакт-кеша через те же таймауты и whitelist capabilities. (Operator-side CLI-форма push-операции пока не нормирована, отдельный backlog; host-side entry-point — `soul apply`.)

### `hot_reload:`

```yaml
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true
```

Блок регулирует включение триггеров hot-reload-механизма (`SIGHUP` / `inotify`) и генерацию `correlation_id` для audit-events `config.reload_succeeded` / `config.reload_failed`. Семантика и инварианты самого механизма — [ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) и [Hot-reload](#hot-reload) ниже. Defaults идентичны Keeper-side ([`../keeper/config.md → hot_reload`](../keeper/config.md#hot_reload)). Блок целиком опционален: при отсутствии в `soul.yml` применяются defaults из таблицы. В push-режиме блок игнорируется — Soul-host one-shot, hot-reload не применим (см. [Hot-reload](#hot-reload) ниже).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `hot_reload.enable_signal` | `bool` | `true` | Включить `SIGHUP`-триггер file-edit-path: pull-демон ловит сигнал, перечитывает `soul.yml` с диска, прогоняет validation pipeline и делает atomic swap ([ADR-021(b)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). При `false` file-edit-path отключён. |
| `hot_reload.enable_inotify` | `bool` | `false` | Включить auto-reload через `inotify`/`fsnotify` (Linux-only) — реагировать на изменение `soul.yml` без `SIGHUP`. Post-MVP опциональное расширение ([ADR-021(b)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)): watch-handle overhead и Linux-only зависимость — причина не делать дефолтом. |
| `hot_reload.audit_correlation_id` | `bool` | `true` | Генерировать `correlation_id` для audit-events `config.reload_succeeded` / `config.reload_failed` ([naming-rules.md → Audit-events](../naming-rules.md#audit-events)). |

#### Hot-reload блока `hot_reload:`

Все три поля **требуют рестарта** `soul`-процесса — потому что они контролируют сам hot-reload-механизм: менять `enable_signal` / `enable_inotify` без рестарта = race условие на установку/снятие signal-handler-а или `inotify`-watch; менять `audit_correlation_id` на лету разделило бы одну логическую reload-операцию на два разных режима audit-логирования. Симметрично с Keeper-side, см. [`../keeper/config.md → Hot-reload блока hot_reload`](../keeper/config.md#hot-reload-блока-hot_reload).

| Поле | Reload без рестарта `soul`-процесса | Обоснование |
|---|---|---|
| `enable_signal` | **нет, требует рестарта** | Меняет привязку signal-handler-а к `SIGHUP`; race на установке/снятии обработчика. |
| `enable_inotify` | **нет, требует рестарта** | Меняет регистрацию `inotify`-watch на путь конфига; race на handle. |
| `audit_correlation_id` | **нет, требует рестарта** | Параметр самого reload-pipeline-а; менять его одним из reload-ов означало бы писать audit-event о собственной мутации в двух разных режимах. |

## Hot-reload

Hot-reload конфига с перезаписью изменённого значения обратно на диск — сквозное требование ([requirements.md](../requirements.md), [architecture.md → Сквозные требования](../architecture.md#сквозные-требования-и-где-они-приземляются)). Механизм нормирован **[ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)** — симметрично с Keeper-side (см. [`../keeper/config.md → Hot-reload`](../keeper/config.md#hot-reload) для полной формулировки); имплементация — общий пакет [`shared/config/`](../adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) (Tier 2).

**Pull-режим (демон).** `soul`-демон читает локальный `soul.yml` — hot-reload работает:

- **File-edit path** — оператор редактирует `soul.yml` на хосте → шлёт `SIGHUP` процессу `soul`. Pipeline parse → schema-validate → semantic-validate → atomic swap → audit-event `config.reload_succeeded` / `config.reload_failed` ([ADR-021(c, g)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)).
- **API/MCP path** — **не предусмотрен в MVP** для Soul-host. Soul admin-surface (локальный HTTP/MCP listener) — отложено post-MVP ([open Q №8](../architecture.md#текущие)). Централизованный rollout `soul.yml` в MVP — через CI / Ansible / SSH (operator's choice); по получению нового файла оператор шлёт `SIGHUP`. API/MCP path Keeper-side — нормирован, см. [`../keeper/config.md → Hot-reload`](../keeper/config.md#hot-reload).
- **Validation** — три этапа (parse / schema-validate / semantic-validate); ошибка любого этапа → in-memory state неизменен, файл не модифицируется, audit `config.reload_failed` с `phase ∈ {parse, schema_validate, semantic_validate}`.
- **Scope** — общий принцип: reload-able без рестарта — параметры конкретного запуска / прогона (timeouts, policies, thresholds, capabilities whitelist); require restart — внешняя поверхность процесса (Keeper endpoints в `keeper.endpoints`, TLS-сертификаты файлов, log-rotation paths). Summary-таблица ниже охватывает все блоки `soul.yml` 1:1; блоки с нетривиальной per-поле семантикой (`plugin_runtime`, `hot_reload`) дополнительно нормированы отдельными таблицами в своих разделах.

**Summary per-block reload-policy** (нормативно, по одной строке на блок; симметрично с Keeper-side, см. [`../keeper/config.md → Hot-reload`](../keeper/config.md#hot-reload)):

| Блок | Reload-able без рестарта `soul`-процесса | Require restart | Примечание |
|---|---|---|---|
| `sid` | — | yes | SID привязан к mTLS-сертификату SoulSeed. |
| `paths.*` (`modules`, `seed`) | — | yes | File-system layout, cache locations. |
| `keeper.endpoints` | — | yes | Open gRPC bidi stream connection. |
| `keeper.retry.*` / `keeper.failback.*` | yes | — | Параметры next retry / failback iteration. **Исключение:** per-endpoint inter-attempt-пауза (reuse `backoff.initial`/`jitter`) restart-required — читается один раз при сборке EventStream-клиента; reconnect-backoff и failback подхватываются per-iteration. |
| `keeper.tls.ca` | — | yes | TLS-context init. |
| `keeper.max_apply_size_mb` | — | yes | recv-лимит задаётся dial-опцией открытого gRPC-стрима; новое значение подхватывается на следующем reconnect. |
| `soulprint.refresh_interval` | yes | — | Применяется к next collection iteration. |
| `cleanup.modules_ttl_days` / `cleanup.run_interval` | yes | — | In-memory cleanup loop. |
| `logging.level` | yes | — | In-memory variable. |
| `logging.format` / `logging.file` / `logging.rotation.*` | — | yes | Re-init log writer. |
| `metrics.enabled` / `metrics.listen` / `metrics.basic_auth.*` | — | yes | Listener address + basic-auth креды (резолв `password_file` на старте listener-а). |
| `otel.*` | — | yes | Re-init exporter. |
| **`plugin_runtime.*`** | per-поле — см. [§ Hot-reload блока `plugin_runtime:`](#hot-reload-блока-plugin_runtime) | | |
| **`hot_reload.*`** | per-поле — см. [§ Hot-reload блока `hot_reload:`](#hot-reload-блока-hot_reload) | | (все require restart) |
- **Per-host без координации** — каждый Soul-host перезагружает свой `soul.yml` независимо; cross-host координация через Keeper — post-MVP ([ADR-021(f)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)).
- **Audit-events** — `config.reload_succeeded` / `config.reload_failed`, каталог в [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events). Soul-side audit пишется в локальный журнал и опционально стримится Keeper-у в общий audit-trail (механизм — backlog).
- **History** — git-blame `soul.yml` (если оператор хранит конфиги в git/CI) + локальный audit-журнал.

**Push-режим (`keeper.push`).** В push-сессии `soul.yml` на удалённом хосте **не используется** (Soul поднимается one-shot, отрендеренный план прогона приходит от Keeper-а через stdin `soul apply` — см. [«Push-режим»](../architecture.md#push-режим-keeperpush)). **Hot-reload не применим** в push: процесс короткоживущий, файла на диске нет.

**Опциональный блок `hot_reload:`** в `soul.yml` (поля `enable_signal`, `enable_inotify`, `audit_correlation_id`) — нормативная типизация полей в [`### hot_reload:`](#hot_reload) выше. При отсутствии блока применяются defaults оттуда (встроены в `shared/config`), симметрично с Keeper-side.

## Что в `soul.yml` НЕ лежит

- **`auth:` блок.** Soul не аутентифицируется через JWT — только mTLS / SoulSeed, см. [identity.md](identity.md). JWT — для операторов Keeper-а, не для Soul.
- **Destiny и Essence.** Сырыми на Soul-хост не попадают вообще — Keeper рендерит их у себя (`vault-resolve → input-validation → CEL-render → text/template-render`, ADR-012(d)). Soul получает только готовый план: в pull — `ApplyRequest` по живому стриму, в push — `ApplyRequest` (protojson) через stdin. На диске Soul-а их нет.
- **SoulSeed-токен.** Используется однократно в `soul init`, читается из stdin или env `SOUL_BOOTSTRAP_TOKEN`, после успешного CSR удаляется (см. [onboarding.md → На стороне Soul](onboarding.md#на-стороне-soul)).
- **Список модулей или их источников.** Реестр модулей живёт на Keeper-е; Soul получает модули через core-модуль `core.module.installed` (pull) или массовой передачей в push-сессии (см. [modules.md](modules.md)).
- **`version:` поле.** Версия Soul-бинаря — git ref / SHA артефакта; в `soul.yml` не дублируется (см. [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)).
- **Набор Soulprint-коллекторов.** Фиксирован Soul-бинарём по [ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp); user-collectors — отложены ([open Q №22](../architecture.md#текущие)).

## Полный пример

Минимальный валидный `soul.yml` со всеми обязательными и опциональными полями:

```yaml
# sid: redis1.cache-test-dev.example         # опц.: явный SID; по умолчанию = FQDN

paths:
  modules: /var/lib/soul-stack/modules
  seed:    /var/lib/soul-stack/seed

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
    spray: 10m

  tls:
    ca: /var/lib/soul-stack/seed/ca.crt

soulprint:
  refresh_interval: 5m

cleanup:
  modules_ttl_days: 30
  run_interval: 24h

logging:
  level: info
  format: json
  file: /var/log/soul/soul.log         # пусто/опущено → stderr без ротации
  rotation: { max_size_mb: 50, max_age_days: 7, max_files: 5, compress: true }

metrics:
  enabled: true
  listen: "127.0.0.1:9091"

otel:
  enabled: true
  endpoint: "k1.dc1.example:4317"

plugin_runtime:
  socket_dir: /var/run/soul-stack/plugins
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false

# Опционально: блок целиком можно опустить — применятся defaults
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true
```

Эталонный пример в файле — [`examples/soul/soul.yml`](../../examples/soul/soul.yml).

## См. также

- [connection.md](connection.md) — нормативная спецификация блока `keeper:` (алгоритм priority + failback).
- [modules.md](modules.md) — что лежит в `paths.modules`, как работает `cleanup`.
- [identity.md](identity.md) — что лежит в `paths.seed`, почему у Soul нет `auth:` (mTLS / SoulSeed вместо JWT).
- [onboarding.md](onboarding.md) — как SoulSeed появляется в `paths.seed`.
- [soulprint.md](soulprint.md) — typed-схема Soulprint MVP, `refresh_interval`.
- [`docs/keeper/config.md`](../keeper/config.md) — Keeper-side конфиг (включая `auth:` для операторов).
- [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — рабочий пример.
- [architecture.md → ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) — typed Soulprint MVP.
- [architecture.md → Сквозные требования](../architecture.md#сквозные-требования-и-где-они-приземляются) — почему `logging.rotation`, `metrics`, `otel` обязательны.
