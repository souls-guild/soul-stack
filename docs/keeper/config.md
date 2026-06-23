# Формат `keeper.yml`

Конфиг одного инстанса Keeper-кластера. Несколько инстансов с разным `kid` стоят за общими Postgres + Redis (см. [concept.md](concept.md), [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)).

Эталонный пример — [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml). Этот документ описывает каждый блок и **нормативно типизирует** все поля — по нему пишется парсер.

## Конвенции типов

Везде ниже используется единый словарь типов:

| Запись | Смысл |
|---|---|
| `string` | произвольная строка UTF-8. |
| `int` | знаковое 64-битное целое. |
| `bool` | `true` / `false`. |
| `duration` | Go-duration string (`1s` / `500ms` / `1h30m`) + допустимый суффикс `<N>d` для дней (`30d` = 720h). Используется для всех duration-полей. Композитная форма `1d2h` не поддерживается. |
| `enum{a,b,c}` | строка из явно перечисленного множества. |
| `string(host:port)` | строка `host:port`; `host` — IP или DNS-имя, `port` — `1..65535`. |
| `vault-ref` | строка вида `vault:<path>` (`vault:secret/keeper/postgres`); читается через клиентский Vault на старте Keeper-а. |
| `path` | абсолютный путь в локальной ФС хоста, на котором запущен Keeper. |
| `git-url` | git-URL (`git@host:org/repo.git` / `https://…/repo.git`). |
| `git-ref` | git tag или branch (без semver-range, [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). |
| `list<T>` / `map<K,V>` | как обычно. |

`default: —` обозначает обязательное поле без default-а. Опциональные поля помечены `optional`. Значения `enum{…}` — lowercase ASCII, без пробелов.

## `kid`

```yaml
kid: keeper-eu-west-01
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `kid` | `string` (kebab-case, уникален в кластере; regex `^[a-z][a-z0-9-]{0,62}$`, см. [naming-rules.md → Идентификаторы](../naming-rules.md#идентификаторы)) | — | Стабильный человекочитаемый идентификатор этого Keeper-инстанса. Используется в lease на SID (`SET sid:lock <kid>`), в колонке `last_seen_by_kid` таблицы `souls`, в аудит-событиях и метриках. См. [concept.md → KID](concept.md#kid). |

## `listen`

Сетевые слушатели. gRPC формализован как два независимых sub-listener-а
по [ADR-012(b)](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add):

- `listen.grpc.bootstrap` — Bootstrap-RPC, **server-only TLS** (у Soul-а до онбординга нет SoulSeed-сертификата).
- `listen.grpc.event_stream` — долгоживущий bidi-стрим, **mTLS** (валидация SoulSeed входящих Souls по `tls.ca`).

```yaml
listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
        # ca — НЕ поддерживается, парсер выдаёт unknown_key
    event_stream:
      addr: "0.0.0.0:9443"
      max_apply_size_mb: 8            # send-лимит ApplyRequest, default 8 MiB
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
        ca:   /etc/keeper/tls/ca.crt
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `listen.grpc.bootstrap.addr` | `string(host:port)` | — | bind-адрес Bootstrap-RPC listener-а (server-only TLS). Обязателен (ADR-012(b)); пустое → diag `grpc_bootstrap_listener_required`. |
| `listen.grpc.bootstrap.tls.cert` | `path` | — | серверный сертификат Keeper-а для Bootstrap. |
| `listen.grpc.bootstrap.tls.key` | `path` | — | приватный ключ к `cert`. |
| `listen.grpc.event_stream.addr` | `string(host:port)` | — | bind-адрес EventStream listener-а (mTLS). Обязателен; пустое → diag `grpc_event_stream_listener_required`. Должен отличаться от `bootstrap.addr` (иначе diag `bootstrap_eventstream_port_conflict`). |
| `listen.grpc.event_stream.tls.cert` | `path` | — | серверный сертификат Keeper-а для EventStream. Допускается совпадение с bootstrap. |
| `listen.grpc.event_stream.tls.key` | `path` | — | приватный ключ к `cert`. |
| `listen.grpc.event_stream.tls.ca` | `path` | — | CA, по которой валидируются SoulSeed-сертификаты входящих Souls. |
| `listen.grpc.event_stream.max_apply_size_mb` | `int` (МиБ, ≥1) | `8` | Потолок размера одного исходящего FromKeeper-сообщения, прежде всего `ApplyRequest` с пачкой отрендеренных `RenderedTask` (рендер Destiny — Keeper-side, [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Применяется как `grpc.MaxSendMsgSize` на EventStream-сервере: при попытке отправить больше Keeper падает fail-fast (`ResourceExhausted`), а не отдаёт Soul-у сообщение, которое тот молча отвергнет. `0`/опущено → дефолт `8`; `<1` → diag `value_out_of_range`. **Должен быть ≤ Soul-recv-лимиту** (`keeper.max_apply_size_mb` в [soul/config.md](../soul/config.md#keeper)); дефолты обеих сторон совпадают (8 MiB). Это send-лимит исходящего; recv-лимит входящих FromSoul — отдельный внутренний инвариант (1 MiB), конфигом не управляется. |
| `listen.openapi.addr` | `string(host:port)` | — | bind-адрес OpenAPI-фасада (первичный интерфейс оператора, [ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)). Обязательный listener согласно сквозному требованию «встроенная поддержка OpenAPI» ([requirements.md](../requirements.md)); отключение запрещено грамматикой парсера. |
| `listen.mcp.addr` | `string(host:port)` | — | bind-адрес MCP-сервера (первичный интерфейс наравне с OpenAPI). Обязательный listener согласно сквозному требованию «встроенный MCP» ([requirements.md](../requirements.md)); отключение запрещено грамматикой парсера. Каталог tools, доступных через этот listener — [mcp-tools.md](mcp-tools.md). |
| `listen.metrics.addr` | `string(host:port)` | — | bind-адрес **выделенного** Prometheus-`/metrics` listener-а (отдельный порт, обычно `9090`, [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). Эндпоинт **не** монтируется на openapi-роутер: scrape идёт сюда, без auth-chain Operator API. keeper_http_*-метрики при этом по-прежнему собираются middleware на `/v1/*` и экспонируются здесь же (один registry). Обязательный listener согласно сквозному требованию «публикация метрик» ([requirements.md](../requirements.md)); отключение запрещено грамматикой парсера. Опц. защита — [`metrics.auth.basic`](#metrics). |

## `postgres`

```yaml
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }
```

Подключение к Postgres — единственному холодному хранилищу состояния Keeper-кластера ([ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres), [storage.md](storage.md)).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `postgres.dsn_ref` | `vault-ref` | — | Vault-reference на полный DSN. Plain-DSN в файл не пишется. Поле в Vault KV — **`dsn`** (`vault kv put secret/keeper/postgres dsn="postgres://..."`). Конвенция симметрична `signing_key` в `auth.jwt.signing_key_ref`. |
| `postgres.pool.min` | `int` (≥1) | `2` | Минимальный размер пула на инстанс. |
| `postgres.pool.max` | `int` (≥`min`) | `20` | Максимальный размер пула. Общий поток к PG = `max × количество_keeper_инстансов`. |

## `redis`

Подключение к Redis — горячему слою и шине координации ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis), [storage.md](storage.md)). Клиент Keeper-а поддерживает три топологии нативно — `mode: standalone | sentinel | cluster`; пустой/опущенный `mode` трактуется как `standalone` (forward-compat для конфигов без поля).

```yaml
# Standalone (default): один узел.
redis:
  mode: standalone               # можно опустить — это default
  addr: "redis.internal:6379"
  password_ref: vault:secret/keeper/redis

# Sentinel: HA с автоматическим failover (рекомендуемый прод-путь on-premise).
redis:
  mode: sentinel
  master_name: mymaster
  sentinels:
    - "sentinel-1.internal:26379"
    - "sentinel-2.internal:26379"
    - "sentinel-3.internal:26379"
  password_ref: vault:secret/keeper/redis                    # пароль Redis-узлов
  sentinel_password_ref: vault:secret/keeper/redis#sentinel  # опц., пароль самих sentinel-узлов

# Cluster: шардирование по слотам (горизонтальное масштабирование).
redis:
  mode: cluster
  nodes:
    - "redis-1.internal:6379"
    - "redis-2.internal:6379"
    - "redis-3.internal:6379"
  password_ref: vault:secret/keeper/redis
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `redis.mode` | `enum{standalone,sentinel,cluster}` | `standalone` (пусто/опущено) | Топология Redis. `standalone` — один узел (`addr`). `sentinel` — Redis Sentinel HA (master-discovery через sentinel-узлы). `cluster` — Redis Cluster (slot-routing нативно в клиенте). Значение вне множества → diag `value_not_in_enum`. |
| `redis.addr` | `string(host:port)` | — | Адрес узла. **Обязателен** при `mode: standalone` (иначе diag `missing_required_field`); при `sentinel`/`cluster` игнорируется (diag `redis_unused_field`, warn). |
| `redis.master_name` | `string` | — (`optional`) | Имя monitored master group для Sentinel. **Обязателен** при `mode: sentinel`. В других режимах — лишнее поле (warn). |
| `redis.sentinels` | `list<string(host:port)>` | — (`optional`) | Адреса sentinel-узлов. **Обязателен (непустой)** при `mode: sentinel`. Каждый элемент валидируется как `host:port`. В других режимах — лишнее поле (warn). |
| `redis.nodes` | `list<string(host:port)>` | — (`optional`) | Адреса узлов кластера для bootstrap-discovery (клиент сам подтянет полную топологию и slot-map). **Обязателен (непустой)** при `mode: cluster`. Каждый элемент валидируется как `host:port`. В других режимах — лишнее поле (warn). |
| `redis.password_ref` | `vault-ref` или `string` | — | Пароль Redis. `vault:<mount>/<path>[#field]` — резолвится из Vault keeper-vault-клиентом (default-поле `password`, override через `#field`); plaintext-строка работает как есть (dev/тесты); пустое — подключение без пароля. Vault-ref валидируется semantic-фазой (`vault_ref_invalid` на битый формат). |
| `redis.sentinel_password_ref` | `vault-ref` или `string` | — (`optional`) | Пароль самих sentinel-узлов (отдельный от пароля Redis). Та же форма и резолв, что `password_ref`. Имеет смысл только при `mode: sentinel`. |

Vault KV-секрет с паролем кладётся под полем `password` (`vault kv put secret/keeper/redis password="<redis-password>"`); другое поле выбирается суффиксом `#field` в ref (например `vault:secret/keeper/redis#sentinel`). Если поле в KV отсутствует/пустое — Keeper падает fail-fast на старте (`password field missing or empty`); если ref начинается с `vault:`, но vault-клиент не поднят — `vault client is required`.

## `vault`

```yaml
# Dev / local: статический token (root в dev-Vault).
vault:
  addr: "http://127.0.0.1:8200"
  token: "root"
  auth: { method: token }     # default; блок auth можно опустить целиком
  pki_mount: "pki"

# Прод: AppRole.
vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod                       # НЕ секрет, можно inline
    secret_id_file: /etc/keeper/vault-secret-id  # mode-ограниченный файл (0400/0600)
    # ИЛИ вместо файла:
    # secret_id_env: KEEPER_VAULT_SECRET_ID    # имя env-переменной с secret_id
  pki_mount: "pki/soulstack"
```

Vault — обязательная зависимость Keeper-а: Essence-секреты, PKI для выпуска SoulSeed, SSH-CA для `keeper.push`, signing key JWT (см. [`auth:`](#auth)), credentials cloud-driver-ов ([requirements.md](../requirements.md)).

`vault.auth.method` выбирает способ аутентификации Keeper-а в Vault ([ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)):

- `token` (**default**) — статический токен из `vault.token`. Dev-shortcut: `dev/docker-compose.yml` поднимает Vault в dev-режиме с root-токеном. Блок `auth` целиком можно опустить — это эквивалент `method: token` (forward-compat для существующих `keeper.yml`).
- `approle` — прод-путь: Keeper делает `auth/approle/login` с `role_id` + `secret_id` и получает renewable client-token, который дальше продлевается в фоне (token auto-renew, [requirements.md](../requirements.md)).

**Откуда берётся `secret_id`.** AppRole-credentials НЕ читаются из самого Vault (никаких `vault:`-ref): этими credentials Keeper и логинится, чтобы потом резолвить остальные `*_ref`-поля (`postgres.dsn_ref`, `auth.jwt.signing_key_ref`, …) — это была бы циклическая зависимость. Поэтому источник локальный, до подъёма Vault-клиента:

- `role_id` — идентификатор роли, **не секрет**; задаётся inline в `keeper.yml`.
- `secret_id` — **секрет**; plaintext в `keeper.yml` не предусмотрен схемой. Ровно один источник:
  - `secret_id_file` — путь к mode-ограниченному файлу (рекомендуется `0400`/`0600`), содержимое = `secret_id` (trailing newline снимается);
  - `secret_id_env` — имя env-переменной с `secret_id` (CI / Vault Agent / k8s-secret-as-env).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `vault.addr` | `string` (URL) | — | Адрес Vault. |
| `vault.token` | `string` | — | Статический Vault-token для `method: token` (например, `root` для local-dev). При `method: approle` задавать нельзя (конфликт, ошибка валидации). |
| `vault.kv_mount` | `string` | `secret` | Mount point KV (без trailing slash). Используется client-ом для чтения signing-key JWT и других secrets. Mount может быть KV **v1 или v2** — версия определяется автоматически (probe `sys/internal/ui/mounts/<mount>` на первом чтении), указывать её в `kv_mount` не нужно. Контракт чтения единый для обеих версий: модуль/резолвер получает уже плоский payload (для v2 обёртка `data.data` распаковывается клиентом). |
| `vault.kv_version` | `enum{"1","2"}` | — (auto) | Опциональный override версии KV mount-а. По умолчанию (пусто/опущено) — **автоопределение** через probe, оператору ничего указывать не надо. Задаётся **только** в редком случае, когда автоопределение закрыто: захардненный Vault, чья ACL-политика не даёт читать `sys/internal/ui/mounts` (probe тогда fail-closed с явной ошибкой и подсказкой указать `kv_version`). Значение вне множества → diag `vault_kv_version_invalid`. Операции, специфичные для KV v2 (list/metadata — orphan-reconcile Sigil), на v1-mount-е недоступны по построению. |
| `vault.auth.method` | `enum{token,approle}` | `token` | Метод аутентификации Keeper-а в Vault. Пустой = `token`. |
| `vault.auth.role_id` | `string` | — | `role_id` AppRole (не секрет). Обязателен при `method: approle`. |
| `vault.auth.secret_id_file` | `string` (abs path) | — | Путь к mode-ограниченному файлу с `secret_id`. Взаимоисключающ с `secret_id_env`; ровно один обязателен при `method: approle`. |
| `vault.auth.secret_id_env` | `string` | — | Имя env-переменной с `secret_id`. Взаимоисключающ с `secret_id_file`. |
| `vault.pki_mount` | `string` | — | Путь mount-а PKI engine, через который Keeper выпускает SoulSeed-сертификаты. |
| `vault.pki_role` | `string` | `soul-seed` (optional) | Имя PKI role в указанном mount-е. Vault подписывает CSR через `<pki_mount>/sign/<pki_role>`. Provisioning role-а — см. [docs/dev/local-setup.md → Vault PKI](../dev/local-setup.md). |

## `auth`

JWT-аутентификация операторов (Archon) для OpenAPI / MCP, согласно [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon). Блок отвечает только за **подпись и формат токенов**; реестр операторов и RBAC-каталог — в Postgres (`operators` и `rbac_*` соответственно, ADR-028), управление ролями — через `role.*` API/MCP ([rbac.md](rbac.md)).

```yaml
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 24h
    ttl_bootstrap: 720h        # 30d
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `auth.jwt.signing_key_ref` | `vault-ref` | `vault:secret/keeper/jwt-signing-key` | Vault KV-путь до signing-key, которым подписываются операторские JWT (`iss`/`sub`/`iat`/`exp`/`roles`/`bootstrap_initial`). Post-MVP — Vault Transit без экспорта ключа ([ADR-014(b)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). |
| `auth.jwt.issuer` | `string` | `<kid>` | Значение claim `iss` в выпускаемых JWT. При отсутствии значения парсер подставляет значение поля `kid:` инстанса ([ADR-014(b)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Допустимо переопределить — единое имя на кластер вместо per-инстанс. |
| `auth.jwt.ttl_default` | `duration` | `24h` | TTL обычных операторских токенов, выпускаемых через `operator.issue-token`. Короткий TTL — естественная защита от revocation-blocklist ([ADR-014(d)/(трейдоффы)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). |
| `auth.jwt.ttl_bootstrap` | `duration` | `720h` (30 дней) | TTL первого bootstrap-токена, выпускаемого `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта), [ADR-014(b)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). |

Если по `signing_key_ref` в Vault ключа нет на момент старта Keeper-а — реализационная развилка ([ADR-014, раздел Consequences](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon): «либо Keeper сам генерирует и кладёт при `keeper init`, либо отказывается стартовать»). До закрытия отдельной задачей нормативное поведение не зафиксировано.

**У Soul нет блока `auth:`** — Soul аутентифицируется к Keeper-у через mTLS / SoulSeed, см. [`docs/soul/identity.md`](../soul/identity.md). JWT — только для операторов (OpenAPI/MCP).

**Что НЕ в `auth:`:**

- Реестр Архонтов — в Postgres (`operators`, [storage.md](storage.md), [ADR-014(a)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
- Роли и permissions — в Postgres (`rbac_*`, ADR-028), управление через `role.*` API/MCP ([rbac.md](rbac.md)).
- Bootstrap-семантика (первый Архонт, `--initialize`) — [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта), [rbac.md → Bootstrap первого Архонта](rbac.md#bootstrap-первого-архонта).
- mTLS для machine-identity и `combined` auth-method — post-MVP, расширение `auth_method` enum в `operators` без breaking change.

## `metrics`

Опциональный блок настроек `/metrics`-эндпоинта (bind-адрес — `listen.metrics.addr`, выше). В MVP несёт только опц. защиту эндпоинта; при отсутствии блока `/metrics` обслуживается без auth.

```yaml
metrics:
  auth:
    basic:
      enabled: true
      username: scrape
      password_ref: vault:secret/keeper/metrics-password
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `metrics.auth.basic.enabled` | `bool` | `false` | Включить HTTP Basic-auth на `/metrics`. |
| `metrics.auth.basic.username` | `string` | — | Имя пользователя. Обязательно при `enabled: true` (иначе diag `missing_required_field`). |
| `metrics.auth.basic.password_ref` | `vault-ref` | — | Ссылка `vault:<mount>/<path>` на секрет с полем `password`. Резолвится тем же keeper-vault-клиентом, что читает JWT signing-key. **Plaintext-пароль запрещён** («безопасность на первом месте»): не-vault-ref → diag `vault_ref_invalid`. Обязательно при `enabled: true`. |

Пароль сравнивается constant-time ([`subtle.ConstantTimeCompare`](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). Зарезолвленный пароль и `password_ref` не логируются и не попадают в config-dump.

> **У Soul симметричной auth нет.** У Soul-агента нет vault-клиента ([ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)), которым резолвить `password_ref`; Soul-метрики защищены loopback-ом (`metrics.listen` = `127.0.0.1`, [soul/config.md](../soul/config.md#metrics)). Auth для Soul — отдельная будущая задача.

## `otel`

```yaml
otel:
  enabled: true
  exporter: otlp
  endpoint: "otel-collector.internal:4317"
  export_metrics: false
```

OpenTelemetry — сквозное требование ([requirements.md](../requirements.md)). Трейсы сквозные: оператор → Keeper → Soul через propagation в gRPC-метаданных.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `otel.enabled` | `bool` | `false` | Включить экспорт. |
| `otel.exporter` | `enum{otlp}` (MVP) | `otlp` | Формат экспорта. |
| `otel.endpoint` | `string(host:port)` | — | Адрес OTel-коллектора (gRPC). Обязателен при `enabled: true`. |
| `otel.export_metrics` | `bool` | `false` | Опц. push метрик по OTLP в дополнение к Prometheus-scrape ([ADR-024 §1.2](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) / [observability.md §5](../observability.md)). **Заглушка под Slice 2:** поле читается, но OTLP-метрик-pipeline ещё не поднимается — в Slice 0 экспортируются только трейсы. По умолчанию метрики идут только через Prometheus-`/metrics`. |

При `enabled: true` поле `endpoint` обязательно; при `enabled: false` блок может быть опущен целиком.

## `logging`

```yaml
logging:
  level: info
  format: json
  file: /var/log/keeper/keeper.log    # пусто/опущено → stderr без ротации
  rotation:
    max_size_mb: 100
    max_age_days: 7
    max_files: 10
    compress: true
```

Поведение зависит от `logging.file` (симметрично Soul-side, см. [`../soul/config.md → logging:`](../soul/config.md#logging)):

- **`logging.file` не задан** → вывод в `stderr` без ротации (dev-режим, удобно под systemd/journald и в контейнере).
- **`logging.file` задан** → запись в этот файл с встроенной ротацией (общий билдер [`shared/log`](../adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)); архивы складываются рядом по шаблону `<file>-<timestamp>.<ext>`.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `logging.level` | `enum{debug,info,warn,error}` | `info` | Уровень логирования. |
| `logging.format` | `enum{json,text}` | `json` | `json` для машинной обработки, `text` для человека. |
| `logging.file` | `string` (путь) | — (stderr) | Путь к лог-файлу. Пусто — вывод в `stderr` без ротации. Должен быть абсолютным; relative → diag `path_not_absolute`. |
| `logging.rotation.max_size_mb` | `int` (МБ) | `100` | Порог ротации одного файла. |
| `logging.rotation.max_age_days` | `int` (≥0) | `7` | Сколько дней хранить ротированный файл. Пусто/`0` → дефолт билдера (7 дней); «без ограничения по возрасту» в текущей грамматике не выражается (MVP-ограничение). |
| `logging.rotation.max_files` | `int` | `10` | Сколько архивов держать. |
| `logging.rotation.compress` | `bool` | `true` | Сжимать ли архивы. В MVP `false` не отключает сжатие (всегда `true`); отключение появится позже. |

Встроенная ротация по умолчанию ([requirements.md](../requirements.md)) — без зависимости от внешнего logrotate. Поля `logging.rotation.*` применяются только когда задан `logging.file`.

## Реестр Service-ов и `default_destiny_source` — в Postgres

Реестр Service-ов (`services[]`) и скаляр `default_destiny_source` **перенесены в Postgres** ([ADR-029](../adr/0029-service-registry.md#adr-029-реестр-service-ов--postgres)): источник правды — таблицы `service_registry` + `keeper_settings` ([storage.md](storage.md)), а не `keeper.yml`. Управление — через `service.*` OpenAPI/MCP ([operator-api.md](operator-api.md)), а не правкой файла; runtime читает in-memory снимок (`serviceregistry.Holder`, TTL-poll + pub/sub-инвалидация). Ключи `services:`, `default_destiny_source:` и `default_module_source:` в `keeper.yml` **больше не принимаются** — отвергаются как `unknown_key` (см. раздел [«`services` / `default_destiny_source` / `default_module_source`»](#services--default_destiny_source--default_module_source) ниже).

`default_module_source` упразднён без замены — у поля не было потребителя (резолв модулей через него не реализован).

Резолв `apply: { destiny: <name> }` (ADR-009, изолированный render-проход) семантически не меняется — **гибрид источника** (per-entry git override):

1. `<name>` ищется в `service.yml → destiny[]` загруженного service-снапшота → берётся запись `{name, ref, git?}` (только декларированная зависимость; иначе ошибка). `ref` берётся из записи в обоих случаях.
2. git-URL по гибридному правилу:
   - запись несёт `git:` → используется он напрямую (override; `default_destiny_source` игнорируется);
   - записи `git:` нет → git-URL = `default_destiny_source` (из `keeper_settings`) с подстановкой `{name}`. Пустой / не заданный `default_destiny_source` на этом шаге → ошибка резолва (имени некуда подставить).
3. destiny грузится отдельным immutable-снапшотом, рендерится со СВОИМ `input:` (резолвнутый `apply.input`), её задачи вклеиваются в план родителя. scenario-scope (input/vars/register/soulprint) в destiny НЕ виден — структурная граница изоляции.

См. также [architecture.md → Service](../architecture.md#service--структура-и-manifest), [storage.md → service_registry / keeper_settings](storage.md).

## `plugins`

Каталог плагинов с host = `keeper` ([plugins.md](plugins.md)). Пять скаляров (`cache_root` / `work_root` / `fetch_timeout` / `max_artifact_size_mb` / `max_clone_size_mb`) + два подблока-каталога (`cloud_drivers` / `ssh_providers`).

### `plugins.cache_root`

```yaml
plugins:
  cache_root: /var/lib/soul-stack-keeper/plugins
```

Корень кеша артефактов плагинов на keeper-host-е (путь, куда git-резолв `plugins.{cloud_drivers,ssh_providers}` раскладывает собранные бинари / манифесты). Discovery host-а ([`keeper/internal/pluginhost`](../../keeper/internal/pluginhost/pluginhost.go)) сканирует этот каталог при старте keeper-а.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugins.cache_root` | `path` (абсолютный) | `/var/lib/soul-stack-keeper/plugins` (встроенный `pluginhost.DefaultCacheRoot`) | Опциональное. Должен быть абсолютным; relative-путь schema-фаза отвергает с `path_not_absolute`. Пустое значение / отсутствие ключа — используется встроенный default. Env-override `KEEPER_PLUGIN_CACHE_DIR` (dev/CI) применяется только при отсутствии значения в `keeper.yml` (приоритет: yaml > env > default). |

### `plugins.work_root`

```yaml
plugins:
  work_root: /var/lib/soul-stack-keeper/plugin-src
```

Корень рабочих git-клонов резолвера плагинов ([ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) F-fetch). Keeper при старте git-резолвит `plugins.{cloud_drivers,ssh_providers}` в этот каталог (clone/fetch + checkout через **go-git**, без зависимости от системного бинаря `git`), затем извлекает собранный артефакт `dist/<binary-name>` в commit_sha-слот кеша.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugins.work_root` | `path` (абсолютный) | `/var/lib/soul-stack-keeper/plugin-src` | Опциональное. Должен быть абсолютным (`path_not_absolute`). **СТРОГО вне `cache_root`** — иначе `.git`/checkout попали бы в кеш-каталог, читаемый Discovery/ReadSlot (schema-фаза отвергает с `plugins_work_root_within_cache_root`). Env-override `KEEPER_PLUGIN_WORK_DIR` (dev/CI), приоритет: yaml > env > default. |

### `plugins.fetch_timeout`

```yaml
plugins:
  fetch_timeout: 120s
```

Потолок одной цепочки git-операций резолва плагина (clone/fetch → resolve → checkout, через go-git). git-egress — внешний вызов, таймаут обязателен.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugins.fetch_timeout` | `duration` | `120s` (`config.DefaultPluginFetchTimeout`) | Опциональное. Формат `duration` (Go-`time.ParseDuration` или `<N>d`), валидируется semantic-фазой (`duration_invalid`). Пустое / некорректное → дефолт. |

### `plugins.max_artifact_size_mb` / `plugins.max_clone_size_mb`

```yaml
plugins:
  max_artifact_size_mb: 256    # потолок одного бинаря dist/<binary-name>, default 256 MiB
  max_clone_size_mb: 1024      # потолок рабочего дерева клона (checkout + .git), default 1024 MiB
```

Size-лимиты git-egress hardening ([ADR-026(g)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). `source` каталога — operator-asserted, но сам репозиторий **недоверенный**: `fetch_timeout` ограничивает git-egress **по времени**, но не по объёму — враждебный/огромный репозиторий мог бы забить `work_root` + `cache_root` диска keeper-host-а (DoS). Эти два поля ставят cap **по объёму**: `max_clone_size_mb` мерится по рабочему дереву (du-подобный walk checkout + `.git`) **до** извлечения артефакта, `max_artifact_size_mb` — по бинарю `dist/<binary-name>` перед копированием в кеш. Превышение — **fail-closed**: слот не создаётся (плагину нечего допускать через Sigil), а для clone-лимита дополнительно чистится `work_root/<name>` (sentinel-ы `ErrCloneTooLarge` / `ErrArtifactTooLarge`).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugins.max_artifact_size_mb` | `int` (МиБ, ≥1) | `256` (`config.DefaultPluginMaxArtifactSizeMB`) | Опциональное. Потолок размера одного извлекаемого бинаря `dist/<binary-name>`. `0`/опущено → дефолт; `<1` → diag `value_out_of_range` (сабмегабайтный потолок отверг бы любой реальный Go-бинарь плагина). Превышение на резолве → `ErrArtifactTooLarge`, слот не создаётся. |
| `plugins.max_clone_size_mb` | `int` (МиБ, ≥1) | `1024` (`config.DefaultPluginMaxCloneSizeMB`) | Опциональное. Потолок суммарного размера рабочего дерева клона (checkout + `.git`), мерится до извлечения артефакта. `0`/опущено → дефолт; `<1` → diag `value_out_of_range`. Превышение → `ErrCloneTooLarge` + cleanup `work_root/<name>`. Заведомо больше artifact-лимита (дерево несёт сам артефакт плюс прочие файлы и shallow-`.git`). |

### `plugins.cloud_drivers`

```yaml
plugins:
  cloud_drivers:
    - { name: aws, source: "git@github.com:soul-stack-ecosystem/soul-cloud-aws.git", ref: v2.0.0 }
    - { name: yc,  source: "git@github.com:our-company/soul-cloud-yc.git",          ref: v0.3.1 }
```

CloudDriver-плагины (`soul-cloud-<provider>`), используются [`keeper.cloud`](cloud.md).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugins.cloud_drivers[].name` | `string` (kebab-case) | — | Имя провайдера для ссылки из Provider в Postgres (`type=<name>`, [cloud.md](cloud.md)). |
| `plugins.cloud_drivers[].source` | `git-url` | — | git-URL репозитория плагина. |
| `plugins.cloud_drivers[].ref` | `git-ref` | — | git tag или branch ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). |

### `plugins.ssh_providers`

```yaml
  ssh_providers:
    - { name: vault-ssh, source: "git@github.com:soul-stack-ecosystem/soul-ssh-vault.git", ref: v1.0.0 }
    - { name: static,    source: "git@github.com:soul-stack-ecosystem/soul-ssh-static.git", ref: main }
```

SshProvider-плагины (`soul-ssh-<provider>`), используются [`keeper.push`](push.md).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugins.ssh_providers[].name` | `string` (kebab-case) | — | Имя провайдера, на которое ссылается push-операция при выборе SSH-аутентификации ([push.md](push.md)). |
| `plugins.ssh_providers[].source` | `git-url` | — | git-URL репозитория плагина. |
| `plugins.ssh_providers[].ref` | `git-ref` | — | git tag или branch ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). |

## `plugin_runtime`

```yaml
plugin_runtime:
  socket_dir: /var/run/soul-stack-keeper/plugins
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

Lifecycle host-процесса для плагинов, запускаемых на Keeper-стороне (`cloud_driver`, `ssh_provider`): таймауты handshake-а и shutdown-а, whitelist capabilities и resource-конфликт-политика, опциональный TLS на plugin-сокете. Полная семантика lifecycle, формат handshake-строки, диаграмма запуска плагина — [plugins.md → Lifecycle](plugins.md#lifecycle); нормативное решение — [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `plugin_runtime.socket_dir` | `path` | `/var/run/soul-stack-keeper/plugins/` | Каталог, в котором host создаёт Unix-domain socket-ы плагинов (`<namespace>-<name>-<pid>.sock`). Создаётся с mode `0700`, owned by service user `keeper` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Путь различается для Keeper-host-а (`soul-stack-keeper`) и Soul-host-а (`soul-stack`), см. [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime). |
| `plugin_runtime.startup_timeout` | `duration` | `10s` | Время от `fork()` плагин-процесса до появления handshake-строки `"soul_stack":"plugin-v1"` в stdout. Превышение — host шлёт SIGTERM, далее SIGKILL по истечении `shutdown_grace` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle), [plugins.md → Поведение host-а при handshake](plugins.md#поведение-host-а-при-handshake)). |
| `plugin_runtime.shutdown_grace` | `duration` | `10s` | Время от SIGTERM до SIGKILL. SDK предоставляет signal-handler, плагин должен закрыть in-flight RPC и завершиться сам в пределах этого окна ([ADR-020(d)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). |
| `plugin_runtime.allowed_capabilities` | `list<enum>` | все 6 capabilities (см. YAML-block выше) | Closed enum (полный каталог — [plugins.md → required_capabilities-таблица](plugins.md#required_capabilities-таблица), [ADR-020(f)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Whitelist: `soul-lint` отвергает destiny **до запуска**, если `manifest.required_capabilities` плагина ⊄ этого списка. Default разрешает все шесть; оператор сужает по политике безопасности. Значения вне closed enum-а парсер отвергает с `unknown_capability`. |
| `plugin_runtime.conflict_policy` | `enum{warn,fail}` | `warn` | Политика на случай, когда два плагина в одном прогоне claim-ят один и тот же ресурс в `side_effects` (одинаковая пара `<resource_type>:<value>`). `warn` — host пишет audit-event и продолжает прогон; `fail` — шаг помечается `failed`, причина `policy_violation` отражается в диагностическом канале `TaskEvent` / `RunResult` ([ADR-020(g)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle), [plugins.md → Поведение host-а на side_effects](plugins.md#поведение-host-а-на-side_effects)). |
| `plugin_runtime.enable_tls` | `bool` | `false` | Включение mTLS на plugin-сокете. В MVP — `false`: безопасность обеспечивается file-permissions `0700` на Unix-socket ([ADR-020(h)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). Post-MVP — `true` использует поле `server_cert` (base64-PEM) handshake-строки, уже зарезервированное forward-compat-резервом. До закрытия отдельной задачи поведение при `true` парсер отвергает с `tls_not_implemented`. |

### Hot-reload блока `plugin_runtime:`

Hot-reload конфига — сквозное требование ([requirements.md](../requirements.md)). Общий механизм перезагрузки нормирован [ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml), см. [§ Hot-reload](#hot-reload) ниже. Per-поле политика блока `plugin_runtime:`:

| Поле | Reload без рестарта host-процесса | Обоснование |
|---|---|---|
| `allowed_capabilities` | да | Параметр конкретного запуска плагина: host читает значение при fork-е, новые прогоны видят новое значение. |
| `conflict_policy` | да | То же: оценка конфликта `side_effects` происходит в момент сборки прогона, in-memory. |
| `startup_timeout` | да | Применяется к новым plugin-прогонам, не аффектит уже запущенные. |
| `shutdown_grace` | да | То же. |
| `socket_dir` | **нет, требует рестарта** | Меняет внешнюю поверхность host-а (file-system layout); уже запущенные plugin-сокеты лежат в старой директории. |
| `enable_tls` | **нет, требует рестарта** | Меняет TLS-handshake-цепочку plugin-протокола. |

Правило: меняем-без-рестарта то, что используется как параметр конкретного plugin-прогона; требуем-рестарт то, что меняет внешнюю поверхность host-а.

## `sigil`

Подпись допусков плагинов — печать доверия **Sigil** ([ADR-026](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), [plugins.md → Integrity-model](plugins.md#integrity-model)). Optional-блок: при отсутствии (или пустом `signing_key_ref`) подпись недоступна — Keeper стартует нормально, но allow-операция (допуск плагина Архонтом) вернёт ошибку «sigil key not configured». Загрузка ключа nil-safe.

```yaml
sigil:
  signing_key_ref: vault:secret/keeper/sigil-signing-key
# sigil_anchors_reload_interval: 30s   # TTL-fallback перечита набора якорей (top-level)
```

| Поле | Тип | Default | Описание |
|---|---|---|---|
| `sigil.signing_key_ref` | `vault-ref` | — (optional) | Vault KV-путь до **ed25519-приватника**, которым Keeper подписывает блок Sigil (поле в Vault KV — **`signing_key`**, как в `auth.jwt.signing_key_ref`). Ключ **асимметричный** (ed25519), в отличие от HS256-симметричного JWT signing-key: приватник подписывает на Keeper, публичная часть едет Soul-у в bootstrap как trust-anchor для verify ([ADR-026(d)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)). Допустимые формы значения в KV — PEM (PKCS#8), base64(DER), base64/raw 64-байтного `seed‖pub` или 32-байтного seed. **Plaintext-ключ в конфиге запрещён** («безопасность на первом месте»): не-vault-ref → diag `vault_ref_invalid`. Формат подписываемого блока — [plugins.md → Формат подписываемого блока](plugins.md#формат-подписываемого-блока-нормативный-s3). |

### `sigil_anchors_reload_interval` (top-level)

| Поле | Тип | Default | Описание |
|---|---|---|---|
| `sigil_anchors_reload_interval` | `duration` | `30s` | Период **TTL-fallback-перечита** набора trust-anchor-ключей подписи Sigil ([ADR-026(h)](../adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс), R3). Канал `sigil:anchors-changed` (Redis pub/sub) — best-effort; пропущенный сигнал оставил бы отставшую ноду со старым набором якорей до рестарта (fail-open при Retire). Периодический re-read (`reloadAnchors` по тикеру, образец TTL-poll RBAC / Summons poll-fallback) самоисцеляет пропуск за интервал. Тик поднимается **независимо от Redis**: при выключенном Redis (single-instance / dev) это единственный путь, по которому runtime-ротация доезжает без рестарта. Формат валидируется в semantic-фазе; пусто/`0`/некорректно → дефолт. Ключ — **top-level** (стиль `acolyte_*`), не вложен в `sigil:`. |

## `reaper`

```yaml
reaper:
  enabled: true
  interval: 1h
  dry_run: false
  batch_size: 500
  lock_ttl: 5m
  rules:
    expire_pending_seeds: { enabled: true, max_age: 24h, action: delete }
    # … остальные правила
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `reaper.enabled` | `bool` | `true` | Включить Жнеца. |
| `reaper.interval` | `duration` | `1h` | Интервал прохода. |
| `reaper.dry_run` | `bool` | `false` | Сухой прогон без мутаций. |
| `reaper.batch_size` | `int` | `500` | Размер batch-а одного прохода. |
| `reaper.lock_ttl` | `duration` | `5m` | TTL Redis-lease на лидерство ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). |
| `reaper.rules` | `map<string, object>` | — | Правила чистки. Структура каждого правила (поля, типы, условная обязательность по `action`) нормативно определена в [reaper.md → Структура правила](reaper.md#структура-правила); каталог предопределённых rules и привязка к таблицам — [reaper.md → Правила](reaper.md#правила). Правило `reclaim_apply_runs` по дефолту **выключено** и включается только под гейтом — см. [reaper.md → Включение recovery](reaper.md#включение-recovery-recovery-enable) (WARN: не включать при `acolytes: 0`). Cadence-спавн (`spawn_due_cadence` / `action: spawn`) **в Reaper больше нет** — он уехал в подсистему [Conductor](conductor.md) ([ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)); см. блок [`cadence_scheduler`](#cadence_scheduler). |

## `cadence_scheduler`

Конфиг подсистемы [Conductor](conductor.md) — leader-elected исполнителя [Cadence](../naming-rules.md#сущности-предметной-области)-расписаний ([ADR-048](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)). Conductor по своему тику отбирает созревшие Cadence и спавнит обычный Voyage-прогон; lease `conductor:leader` независим от `reaper:leader`. Блок **опциональный** — при отсутствии действуют дефолты + default-ON при настроенном Redis (footgun-guard). Полное описание поведения, включая [адаптивный шаг опроса](conductor.md#адаптивный-шаг-опроса) и [floor минимального периода Cadence](conductor.md#floor-минимального-периода-cadence) — [conductor.md](conductor.md).

**Шаг опроса адаптивный** ([ADR-048 «Adaptive interval»](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний)), не фиксированный: перед каждым тиком лидер выводит шаг из enabled-реестра Cadence — `clamp(min(периоды enabled-расписаний), poll_floor, poll_ceiling)`; cron-правила дают вклад 60s; пустой enabled-реестр → `poll_idle`. Дефолтный профиль «Спокойный» — 30s / 60s / 120s.

```yaml
cadence_scheduler:
  enabled: true        # nil/опущено → ON при настроенном Redis; false → OFF
  poll_floor: 30s      # нижняя граница адаптивного шага опроса
  poll_ceiling: 60s    # верхняя граница адаптивного шага опроса
  poll_idle: 120s      # шаг опроса при пустом enabled-реестре Cadence
  lock_ttl: 5m         # TTL Redis-lease conductor:leader
  # interval: 60s      # backcompat-alias poll_ceiling; новые конфиги пишут poll_*
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `cadence_scheduler.enabled` | `bool` (опц., tri-state) | `nil` → ON при Redis | Включение Conductor. **Опущено / `null`** → default-ON при наличии Redis ([footgun-guard ADR-048 §5](../adr/0048-conductor.md#adr-048-conductor--leader-elected-исполнитель-cadence-расписаний): Cadence без работающего планировщика молча не спавнит); явный **`false`** → Conductor не поднимается; явный **`true`** → поднимается (требует Redis для lease-лидерства). Выключение отдельного расписания — per-Cadence `enabled: false` ([ADR-046 §3](../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)), не глобальным гашением здесь. **Читается на старте** (не hot-reload) — изменение требует рестарта инстанса. |
| `cadence_scheduler.poll_floor` | `duration` | `30s` | Нижняя граница адаптивного шага опроса (профиль «Спокойный»). Совпадает с floor-лимитом минимального периода Cadence — тот же ключ, единый источник 30s (write-path floor-reject `interval_seconds ≥ poll_floor` читает этот же резолв, см. [conductor.md → Floor](conductor.md#floor-минимального-периода-cadence)). **Абсолютный минимум**: `< 30s` → diag `value_out_of_range` на старте (суб-30s период бессмыслен — downstream не отработает точнее, реактивный домен — Beacons). Пустое/невалидное → дефолт. **Hot-reload** (перечитывается на каждом тике из свежего Store-снимка). |
| `cadence_scheduler.poll_ceiling` | `duration` | `60s` | Верхняя граница адаптивного шага опроса: редкое расписание (`interval=1h`) не растягивает опрос так, чтобы missed-slot-механизм стал единственной страховкой. Инвариант `poll_floor ≤ poll_ceiling` (иначе `value_out_of_range`). Пустое/невалидное → дефолт. **Hot-reload**. |
| `cadence_scheduler.poll_idle` | `duration` | `120s` | Шаг опроса при **пустом enabled-реестре** Cadence (спавнить нечего — опрос реже коридора, не вхолостую). Инвариант `poll_idle ≥ poll_ceiling` (иначе `value_out_of_range`: idle не чаще обычного опроса). Пустое/невалидное → дефолт. **Hot-reload**. |
| `cadence_scheduler.interval` | `duration` | — (alias) | **Backcompat-alias** `poll_ceiling`. До амендмента 2026-06-07 был фиксированным периодом тика; теперь шаг адаптивный, `interval` оставлен ради старых `keeper.yml`. Если задан и `poll_ceiling` **не** задан → `poll_ceiling = max(interval, poll_floor)` (clamp вверх до floor). Суб-floor `interval` (например прежний dev-конфиг с `5s`) **не роняет конфиг**: поднимается до floor с WARNING (`value_clamped`, подсказка про Beacons для суб-30s). При одновременно заданных `interval` и `poll_ceiling` побеждает `poll_ceiling`. Новые конфиги пишут `poll_*`. **Hot-reload**. |
| `cadence_scheduler.lock_ttl` | `duration` | `5m` | TTL Redis-lease `conductor:leader` ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)), parity `reaper.lock_ttl`. Достаточно большой, чтобы пережить временный stall лидера; достаточно короткий для быстрого failover; renew на `lock_ttl/3`. Пустое/`0`/невалидное → дефолт. **Hot-reload** (применяется между re-acquire lease-а). |

Формат `poll_floor` / `poll_ceiling` / `poll_idle` / `interval` / `lock_ttl` проходит semantic-проверку `checkDuration` (как `reaper.interval` / `acolyte_*`): невалидный duration отвергает конфиг на старте, диапазон (`>0`) добивается дефолтом. Взаимный порядок коридора (`poll_floor ≥ 30s ≤ poll_ceiling ≤ poll_idle`) проверяется по **резолвнутым** значениям (с учётом alias-clamp), поэтому ловит и неявные нарушения через `interval`.

> **Прежней dev-рекомендации `interval: 5s` больше нет.** При floor 30s суб-30s опрос недостижим by design — для частого ритма ставьте коридор к 30–60s, для реакции быстрее 30s используйте [Beacons](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor) (Vigil/Oracle, ADR-030), это не задача Cadence.

## `acolytes`

```yaml
acolytes: 0
# acolyte_lease: 30s          # TTL Ward-захвата (claim_expires_at = NOW()+lease)
# acolyte_batch: 10           # макс. заданий за один claim-тик (LIMIT)
# acolyte_poll_interval: 2s   # период poll-fallback-а к Summons-сигналу
# acolyte_drain_grace: 5s     # окно graceful-drain пула при остановке Keeper
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `acolytes` | `int` (≥0) | `0` | Число воркеров пула исполнения apply (**Acolyte**, [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). **Feature-flag**: `0` — пул **не поднимается**, исполнение прогонов идёт прежним путём (run-goroutine инстанса-владельца в scenario-runner-е); `>0` — на инстансе стартует пул из N воркеров, каждый периодически клеймит planned-задания (`apply_runs`) через `FOR UPDATE SKIP LOCKED`. Перевод исполнения на пул (cutover, удаление run-goroutine) — поэтапно в [Phase 1.4 / Phase 2](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim); до этого `acolytes: 0` — штатное значение. Отрицательное значение отвергается с ошибкой `value_out_of_range`. |
| `acolyte_lease` | `duration` | `30s` | TTL Ward-захвата planned-задания ([ADR-027(d)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim): `claim_expires_at = NOW()+lease`). Просроченный Ward переклеймит recovery-скан (Phase 2). Пусто → дефолт. |
| `acolyte_batch` | `int` (≥0) | `10` | Максимум planned-заданий, захватываемых одним claim-тиком (LIMIT claim-запроса). Воркеры разных инстансов делят очередь через `FOR UPDATE SKIP LOCKED` — батч лишь ограничивает аппетит одного тика. `0`/опущено → дефолт. Отрицательное отвергается `value_out_of_range`. |
| `acolyte_poll_interval` | `duration` | `2s` | Период poll-tick-а воркера — fallback к Summons-сигналу ([ADR-027(a)](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). Даже при потере pub/sub-сигнала задание подхватится на ближайшем тике. Пусто → дефолт. |
| `acolyte_drain_grace` | `duration` | `5s` | Окно graceful-drain пула Acolyte при остановке Keeper ([ADR-027 Phase 2](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)): от сигнала «больше не claim-ить» до жёсткой отмены claim-ctx у не успевших in-flight-воркеров. Прерванный claim оставляет Ward в БД (`claimed`/`running`) — lease истечёт, задание подберёт recovery-скан (ADR-027(i)); commit/rollback состояния НЕ форсится. Пусто → дефолт. |

> **Инвариант HA: `N>1` живых Keeper-инстансов требует `acolytes>0`.** `acolytes: 0`
> (run-goroutine-путь) — **single-keeper-only**. В этом режиме владение прогоном
> живёт **in-memory** в run-goroutine инстанса, который его запустил, а `RunResult`
> от Soul-а приходит инстансу, держащему **EventStream** этого Soul-а (его SID-lease,
> [ADR-006(b)](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)). На одном Keeper-е
> это всегда один и тот же инстанс. В HA-кластере (≥2 живых Keeper на общих PG/Redis)
> прогон, созданный на Keeper-A, но c Soul-ом на стриме Keeper-B, **отработает на хосте**
> (`apply_runs.status=success`), однако incarnation **навсегда зависнет в `applying`**:
> владелец-прогон на Keeper-A никогда не увидит ушедший на Keeper-B `RunResult` и его
> barrier истечёт по `runTimeout`. При `acolytes>0` (work-queue, [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim))
> этого нет: claim+dispatch идут через общую очередь (`apply_runs` + Summons), а завершение
> наблюдается через общий PG, независимо от того, какой инстанс держит стрим.
>
> Guard'ы в коде — **два слоя**:
>
> 1. **Refuse на старте** (Finding-A, soul-shedding S3). Имея реестр живых Keeper-инстансов
> ([Conclave](#allow_unsafe_single_path_multi_keeper) — presence в Redis), Keeper при
> `acolytes == 0` И присутствии **других** живых инстансов (`Conclave.CountLive > 1`, в счёт
> входит и собственная presence-запись) **отказывается стартовать** с понятной ошибкой
> (`refusing to start`) и `exit 1` — оператор видит проблему и чинит конфиг до приёма
> Soul-стримов. Это **дефолт** (безопасно). Снимается явным opt-out —
> [`allow_unsafe_single_path_multi_keeper`](#allow_unsafe_single_path_multi_keeper). Guard
> fail-open: при недоступном Conclave (Redis off / SCAN-ошибка) старт не блокируется.
> Conclave-ключи TTL 30s → только что умерший инстанс может ещё числиться (stale-окно) —
> для startup-refuse приемлемо.
> 2. **Runtime-WARN на dispatch-е** (safety-net). В момент dispatch-а старого пути, если
> SID-lease целевого Soul-а принадлежит **другому** KID, печатается точечный `WARN`
> «прогон может зависнуть в applying — для HA выставьте `keeper.acolytes>0`». Опирается на
> уже существующий SID-lease с KID-владельцем, остаётся как страховка после старта (напр.
> второй инстанс поднялся уже после прохождения refuse-guard-а первого).

## `allow_unsafe_single_path_multi_keeper` (top-level)

Явный **opt-out** из refuse-guard-а soul-shedding (Finding-A, [ADR-027](../adr/0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). По умолчанию Keeper при `acolytes == 0` И числе живых Keeper-инстансов в **Conclave** (presence-реестр в Redis, ключи `keeper:instance:<kid>`, TTL 30s) больше одного — **отказывается стартовать** (см. инвариант HA выше). `true` снимает запрет: refuse заменяется громким `WARN`, старт продолжается.

```yaml
# allow_unsafe_single_path_multi_keeper: false   # дефолт (refuse); top-level
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `allow_unsafe_single_path_multi_keeper` | `bool` | `false` | Снять refuse-guard `acolytes=0 + Conclave.CountLive>1`. `false` (дефолт, **безопасно**) — Keeper **отказывается стартовать** в этой конфигурации. `true` — **осознанный** выбор оператора (напр. намеренный single-keeper-за-LB на время миграции / rolling-restart, где «другой» инстанс — уходящий): refuse → `WARN`, старт идёт. Дублируется env-флагом `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER` (truthy-OR: `1`/`t`/`true`/…; пустая/мусорная строка → не включает; паттерн `KEEPER_INITIALIZE`). Любое `bool`-значение валидно — schema-проверки нет. Ключ — **top-level**. |

> **Почему дефолт = refuse, а не warn.** Прежде (до Conclave-реестра) при multi-keeper + `acolytes:0` был только `WARN` — оператор мог его не заметить, и apply на Keeper-A c Soul-ом на стриме Keeper-B навсегда зависал в `applying` (footgun). С реестром живых инстансов Keeper может обнаружить «я не один» на старте и отказать **до** приёма стримов — fail-closed по принципу «безопасность на первом месте». Opt-out существует для легитимных переходных состояний (миграция/rolling-restart), где оператор сознательно держит один работающий инстанс за LB.

## `watchman_interval` / `watchman_fail_threshold` (top-level)

**Watchman** — изоляция-детект + soul-shedding одного Keeper-инстанса (soul-shedding S2, [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper) HA-кластер). Фоновая goroutine периодически пингует PG+Redis (те же зависимости, что `/readyz`); при устойчивой изоляции инстанса **активно закрывает все локальные EventStream-стримы** (hard-close, без drain), и подключённые Souls по своему failback-list-у уходят на живой Keeper. Без этого изолированный инстанс держал бы уже-установленные долгоживущие стримы (`/readyz` уводит только НОВЫЕ подключения через LB — существующие gRPC bidi-стримы от HTTP-health не зависят).

```yaml
# watchman_interval: 5s          # период probe PG+Redis (top-level)
# watchman_fail_threshold: 3     # подряд-провалов probe до shedding-а (debounce)
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `watchman_interval` | `duration` | `5s` | Период probe-тика Watchman: как часто инстанс пингует PG+Redis на предмет изоляции. Формат валидируется в semantic-фазе; пусто/`0`/некорректно → дефолт (резолв в daemon, стиль `acolyte_*`). Ключ — **top-level**. |
| `watchman_fail_threshold` | `int` (≥0) | `3` | Число **подряд идущих** провалов probe до объявления изоляции и shedding-а. Debounce/flap-guard: единичный сетевой spike не должен сбросить весь флот стримов разом (thundering-herd reconnect по кластеру). Один успешный probe сбрасывает счётчик. `0`/опущено → дефолт. Отрицательное отвергается `value_out_of_range`. Время до shedding-а ≈ `watchman_interval × watchman_fail_threshold` (дефолт ≈ 15s). |

> Probe-зависимости — те же `health.Pinger`-ы, что `/readyz` (PG обязателен; Redis — только при живом клиенте: dev-fallback без Redis оставляет probe только по PG). Vault в probe **не** включается — он опционален для обслуживания стримов и его недоступность не равна изоляции инстанса от флота. Решение «я изолирован» централизовано в Watchman (не дублируется в per-stream renewal-loop-ах). Метрики — `keeper_watchman_isolated` (gauge 0/1) и `keeper_watchman_streams_shed_total`.

## `toll`

**Toll** — cluster-wide detector массового оттока Souls ([ADR-038](../adr/0038-toll.md#adr-038-toll--cluster-wide-detector-массового-оттока-souls)). Per-instance Watcher наблюдает gRPC disconnect-события EventStream-а, фильтрует graceful-shutdown / warmup-immunity и публикует выживший event в общий Redis sorted-set. Cluster-leader (через Redis-lease `cluster:toll:leader`) каждые 5s агрегирует sorted-set за sliding-окно 60s, сравнивает с baseline `souls.status='connected'` и при превышении threshold выставляет Redis-ключ `cluster:degraded` (TTL 60s). Middleware на Operator API блокирует `POST /v1/incarnations/{name}/scenarios/{scenario}` и `POST /v1/push/apply` с HTTP 503 + Retry-After на каждом запросе при взведённом флаге. Read-API, RBAC, unlock, destroy, Errand — НЕ блокируются (recovery actions).

Опц. блок: при отсутствии — Toll включён с дефолтами; `enabled: false` — явный opt-out. Toll работает только при живом Redis-клиенте (single-instance/dev без Redis → Toll-инфраструктура не поднимается, флаг никем не выставляется, middleware passthrough).

```yaml
toll:
  enabled: true              # default true; false — выключить детектор полностью
  threshold: 0.20            # доля от baseline souls.status='connected' (0..1]
  window_size: 60s           # sliding окно (per-second buckets в Redis sorted-set)
  degraded_ttl: 60s          # TTL ключа cluster:degraded
  clear_grace: 60s           # asymmetric hysteresis: устойчивое окно низкого rate до clearing
  lease_ttl: 30s             # TTL cluster:toll:leader (renew каждые ttl/3)
  warmup_delay: 60s          # immunity-окно после старта инстанса (cluster cold-start defense)

  # Per-coven threshold-overrides (ADR-038 amendment 2026-05-27, extensions).
  # OR-семантика: leader взводит cluster:degraded при превышении ЛИБО global,
  # ЛИБО любого per-coven threshold. При per-coven trigger-е audit-payload и
  # webhook-payload несут поле coven_name; при global — без него.
  per_coven_thresholds:
    production-eu: 0.15
    production-us: 0.25

  # Webhook alert-канал (ADR-038 amendment 2026-05-27, extensions). При
  # nil/enabled=false notifier не поднимается, audit + gauge + metrics остаются.
  webhook:
    enabled: true
    url_ref: "vault:secret/keeper/toll-webhook-url"  # поле `url` в Vault KV
    format: pagerduty_v2                              # generic / pagerduty_v2 / slack
    timeout: 10s
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `toll.enabled` | `*bool` | `true` | Включить детектор. `false` — Watcher не собирается, Leader не стартует, middleware = noop. Опущено / `null` → `true`. |
| `toll.threshold` | `float` | `0.20` | Доля от baseline `souls.status='connected'`, при превышении которой Toll-leader взводит `cluster:degraded`. Диапазон `(0, 1]`. Опущено / `0` → дефолт. |
| `toll.window_size` | `duration` | `60s` | Длина sliding-окна. Записи старше окна leader удаляет через ZREMRANGEBYSCORE. |
| `toll.degraded_ttl` | `duration` | `60s` | TTL Redis-ключа `cluster:degraded`. Если leader умер и не успел продлить — флаг гаснет сам, блокировка снимается. |
| `toll.clear_grace` | `duration` | `60s` | Устойчивое окно низкого rate-а до clearing (asymmetric hysteresis). Сработать на первом превышении; снять только после grace под threshold-ом. |
| `toll.lease_ttl` | `duration` | `30s` | TTL lease `cluster:toll:leader` (Renew каждые `ttl/3`). При crash-е leader-а следующий кандидат подхватит через ≤ ttl. |
| `toll.warmup_delay` | `duration` | `60s` | Immunity-окно после старта инстанса. Первые `warmup_delay` после старта disconnect-ы НЕ публикуются (cluster cold-start defense — все Souls reconnect-ят разом). Метрика `keeper_toll_warmup_skipped_total` всё равно растёт. |
| `toll.per_coven_thresholds` | `map[string]float` | nil (не задано) | Per-coven threshold-overrides. Ключ — имя coven (как в `souls.coven[]`), значение — порог в `(0, 1]`. Leader дополнительно тикает `ZRANGEBYSCORE` и считает rate-ы per-coven; при превышении ЛЮБОГО — взводит `cluster:degraded` с `coven_name` в payload. При global-trigger-е (`toll.threshold` превышен) `coven_name` остаётся пустым. Пустой ключ отвергается schema-фазой (для global — top-level `threshold`). |
| `toll.webhook.enabled` | `bool` | `false` | Поднять [WebhookNotifier]. При `false` notifier nil, alert-канал отсутствует (audit + gauge + metrics остаются). |
| `toll.webhook.url_ref` | `string` | — | URL webhook-receiver-а. Может быть **vault-ref** (`vault:<mount>/<path>`; поле `url` в Vault KV — рекомендуется для prod) либо inline-URL (`https://...`; для dev/локальных receiver-ов). Обязателен при `enabled: true`. |
| `toll.webhook.format` | `enum` | `generic` | Формат POST-payload-а: `generic` (плоский JSON), `pagerduty_v2` (Events API v2 schema — `routing_key` берётся из того же Vault KV под полем `routing_key`), `slack` (incoming webhook с attachment). |
| `toll.webhook.timeout` | `duration` | `10s` | Потолок одного POST-вызова. Best-effort: тайм-аут логируется, но не блокирует Set/Clear. |

> **Audit + alert семантика.** `cluster.degraded_set` / `cluster.degraded_cleared` (source `keeper_internal`) — пишет ТОЛЬКО leader (single-winner). При per-coven trigger-е audit-payload содержит дополнительное поле `coven_name`. Webhook (опц.) — best-effort POST на set/cleared, тот же `coven_name` пробрасывается в payload (generic/pagerduty/slack). PagerDuty: `dedup_key` = `soul-stack/cluster:degraded` (одна incident на set+resolve); `event_action: trigger` на set, `resolve` на cleared. Slack: `color: danger` на set, `good` на cleared.

> **Метрики:** `keeper_cluster_degraded` (gauge 0/1, set ТОЛЬКО leader-ом), `keeper_toll_disconnects_total{coven}` (counter), `keeper_toll_warmup_skipped_total`, `keeper_toll_graceful_skipped_total`, `keeper_toll_leader_active` (gauge 0/1, сумма по кластеру = 1).

> **Cardinality-риск per-coven.** ADR-038(п.5) изначально отложил per-coven из-за cardinality в Prometheus. После amendment: список ключей `per_coven_thresholds` явно ограничен оператором в keeper.yml (конечный закрытый набор), сам Prometheus counter `keeper_toll_disconnects_total{coven}` уже несёт ту же cardinality — добавление триггеров поверх не множит label-set.

> **Hot-reload `toll.*`** ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). Reload-able без рестарта Leader-а: `threshold`, `window_size`, `degraded_ttl`, `clear_grace`, `per_coven_thresholds`, `webhook.*`. На успешный SIGHUP / API-reload daemon вызывает `Leader.UpdateConfig` — атомарный swap полей под RWMutex (tick читает snapshot в начале каждого aggregation-тика, не блокируя update во время Redis-вызовов). Webhook-notifier пересоздаётся только при diff блока `webhook.*` (URLRef / Format / Timeout / Enabled) — частые reload-ы с неизменным webhook не дёргают Vault-резолв; при первой mutation `url_ref` (включая переход inline↔vault-ref) новый notifier строится через `NewWebhookNotifier` (Vault-резолв отложен до ближайшего `Notify`). Restart-required (тихо игнорируются на reload-е): `enabled` (Toll-инфраструктура поднимается/выключается только на старте), `lease_ttl` (захвачен в renew-loop), `warmup_delay` (применяется per-instance Watcher-ом на старте). Эти ограничения симметричны policy `logging.file` / `logging.format` (writer-restart-required).

## `tempo`

**Tempo** — per-AID rate-limiter resolver-тяжёлых write-эндпоинтов ([ADR-050](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)). Token-bucket в Redis (per-Архонт, по `claims.Subject` = AID), берёт один токен **после** JWT-аутентификации и **до** запуска резолверов; при исчерпании — `429 tempo-exceeded` + `Retry-After`. Третий anti-DoS-слой после body-limit и [Toll](#toll): body-limit режет по размеру тела, Toll — по здоровью кластера (cluster-wide 503), Tempo — по частоте per-AID (429).

Опц. блок: при отсутствии — Tempo включён с дефолтами (footgun-guard, как [Toll](#toll)); `enabled: false` — явный opt-out. Tempo поднимается **только при живом Redis-клиенте** (token-bucket живёт в Redis): single-instance/dev без Redis → лимитер не конструируется, middleware passthrough. При недоступном Redis на лету (флап / connection refused) лимитер деградирует **fail-OPEN** (запрос проходит без rate-check) — то же поведение, что у Toll; осознанный security-trade-off (доступность > перестраховка, [ADR-050(b)](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)).

```yaml
tempo:
  enabled: true              # default true (default-ON при Redis); false — opt-out
  voyage_create:             # bucket POST /v1/voyages (create)
    rate: 10                 # refill-скорость, токенов в секунду (rps)
    burst: 20                # глубина бакета (capacity)
  voyage_preview:            # bucket POST /v1/voyages/preview (dry-resolve scope)
    rate: 30                 # мягче create: preview read-like (без persist/audit)
    burst: 60                # глубина бакета (capacity)
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `tempo.enabled` | `*bool` | `true` | Включить лимитер. `false` — лимитер не конструируется, middleware = passthrough. Опущено / `null` → `true` (default-ON). Фактический подъём дополнительно требует Redis. |
| `tempo.voyage_create.rate` | `float` | `10` | Refill-скорость bucket-а `voyage_create`, токенов в секунду (rps). Опущено / `0` → дефолт. Отрицательное → schema-ошибка `value_out_of_range`. |
| `tempo.voyage_create.burst` | `int` | `20` | Глубина (capacity) bucket-а `voyage_create` — допустимый всплеск. Опущено / `0` → дефолт. Отрицательное → schema-ошибка `value_out_of_range`. |
| `tempo.voyage_preview.rate` | `float` | `30` | Refill-скорость bucket-а `voyage_preview`, токенов в секунду (rps). Опущено / `0` → дефолт. Отрицательное → schema-ошибка `value_out_of_range`. |
| `tempo.voyage_preview.burst` | `int` | `60` | Глубина (capacity) bucket-а `voyage_preview` — допустимый всплеск. Опущено / `0` → дефолт. Отрицательное → schema-ошибка `value_out_of_range`. |

> **Два bucket-а — два эндпоинта.** `voyage_create` обслуживает `POST /v1/voyages` (create); `voyage_preview` — `POST /v1/voyages/preview` ([ADR-043 amendment](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)). Раньше preview делил bucket `voyage_create`; теперь у него собственный, **более мягкий** per-AID лимит (`30/60` против `10/20`), потому что preview — это dry-resolve scope: по эффекту read-like (без persist/audit), но resolver-heavy по стоимости, поэтому не безлимитен. Прочие write-эндпоинты под Tempo — additive позже (новый bucket в конфиге + навеска middleware, без breaking change). Read-API и дешёвые write (GET/list/cancel) не лимитятся.

> **Redis-ключ и атомарность.** Состояние бакета — Redis-hash `tempo:<aid>:<bucket>` (`tokens` / `last_refill_ts` + `PEXPIRE`); refill+take — одним Lua-скриптом (атомарный read-modify-write, когерентный лимит поверх stateless-HA-кластера, [ADR-050(a)](../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)). In-memory per-инстанс отвергнут (размножился бы ×N инстансов). Время бакет читает из самого Redis (`redis.call("TIME")`), не из Go-часов — refill не зависит от рассинхрона часов между инстансами. Холодный бакет (новый AID) стартует полным — оператор не штрафуется первым запросом.

> **Метрики.** `keeper_tempo_allowed_total{endpoint}` / `keeper_tempo_rejected_total{endpoint}` (counter; `endpoint` = bucket-имя — `voyage_create` либо `voyage_preview`, **AID-лейбла НЕТ** — кардинальность). Кто именно превышает — видно в audit/логах по `claims.Subject`, не в метриках. Полный реестр Keeper-метрик — [observability.md → Метрики Keeper](../observability.md). При выключенном Tempo (нет Redis / `enabled: false`) counters остаются на 0 — валидный сигнал «лимитер не активен».

> **Hot-reload `tempo.*`** ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). `rate` / `burst` reload-able без рестарта: лимитер stateless, читает живой `config.Store`-snapshot на **каждом** запросе — новый лимит применяется со следующего запроса, текущие бакеты в Redis доживают по своему `PEXPIRE`. Невалидные (≤0) значения из reload-а трактуются как fail-OPEN passthrough (на сбое конфига блокировать нельзя). Restart-required: `enabled` (Tempo-инфраструктура / Redis token-bucket поднимается или гасится только на старте `setupTempo`, симметрично `toll.enabled`).

## `web_ui_enabled` (top-level)

Тоггл встроенного операторского web-UI на маршруте `/ui` ([ADR-055](../adr/0055-embed-ui-bundle.md#adr-055-embed-ui-bundle--опциональный-single-binary-keeper-с-ui-на-ui)). Реальный UI **вкомпилён в `keeper`-бинарь** (`go:embed` статики из companion-репо `soul-stack-web`, см. [docs/web/README.md](../web/README.md)) и отдаётся keeper-ом из коробки — **отдельного процесса, порта и backend-а не требует**. Статика монтируется на уже существующий OpenAPI-listener (`listen.openapi.addr`, обычно `:8080`); **новых listener-ов и портов `web_ui_enabled` не вводит** — UI делит `:8080` с Operator API (`/v1/*`) и OpenAPI-вьювером (`/docs`).

```yaml
# web_ui_enabled: true     # дефолт (опущено / null → ON); false — opt-out. top-level
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `web_ui_enabled` | `*bool` | `true` | Монтировать ли встроенный UI на `/ui`. **`*bool`, чтобы отличить «не задано» от явного `false`:** опущено / `null` → `true` (**default-ON** — бета хочет single-binary UI «из коробки», footgun-guard в духе [`tempo.enabled`](#tempo) / [`toll`](#toll)); явный `false` → **opt-out**: статика `/ui` НЕ монтируется, API `/v1/*` и `/docs` не затрагиваются. В отличие от Tempo/Toll **не зависит от инфраструктуры** — UI вшит в бинарь, внешнего backend-а не нужно. Ключ — **top-level**. Резолв эффективного значения — метод `WebUIMounted()` (`shared/config/keeper.go`): `nil` → `true`. |

> **Hot-reload `web_ui_enabled`** ([ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). **Restart-required** (тихо игнорируется на reload-е, симметрично [`toll.enabled`](#toll) / [`tempo.enabled`](#tempo)): эффективное значение читается один раз на старте и запекается в смонтированный роутер; SIGHUP / API-reload не переключает `/ui`-mount на лету. Чтобы включить/выключить UI — изменить ключ и перезапустить `keeper`. Routing-тоггл не оправдывает atomic re-mount роутера на горячую: статика вшита в бинарь, состояния не несёт, переключение редкое (бета-onboarding), а disposal/swap chi-роутера под трафиком — лишний риск ради бинарного флага. Новых портов при включении не открывается (статика садится на тот же `:8080`).

## `push`

**Pilot-path wire-up SshDispatcher** (S6, 2026-05-26, [ADR-032 amendment](../architecture.md)). Pilot включает `keeper.push.apply` в проде через 3 inline-поля в `keeper.yml`. Long-term canon (S7) — миграция в `souls.ssh_target jsonb` + PG-table `push_providers`; S7-3 ввёл multi-CA `push.host_ca_refs[]`. Singular `push.host_ca_ref` остаётся под 1-release WARN deprecation window.

Optional-блок: при отсутствии (либо пустых `targets[]` / отсутствующих `host_ca_ref` И `host_ca_refs[]`) push-orchestrator не поднимается, и `POST /v1/push/apply` / MCP `keeper.push.apply` возвращают «не сконфигурировано». Для включения push в проде обязательны три условия:

1. `plugins.ssh_providers[]` объявлен и хотя бы один SshProvider-плагин дискаверен в кеше (см. [`plugins.ssh_providers`](#pluginsssh_providers)).
2. `push.host_ca_refs[]` непустой и резолвится в Vault (public host-CA, поле `public_key`). Singular `push.host_ca_ref` (deprecated) — backward-compat path: при заполненном singular и пустом `host_ca_refs[]` daemon auto-adapt-ит singular в `host_ca_refs[0]` с auto-name `default` + одноразовый WARN.
3. `push.targets[]` содержит записи по SID push-хостов (FQDN, совпадающий с `souls.sid`) либо хотя бы одна запись `souls.ssh_target jsonb` (S7-1 canonical).

```yaml
push:
  host_ca_refs:                                   # S7-3: multi-CA OR-проверка через CertChecker.IsHostAuthority
    - ref: vault:secret/keeper/ssh-host-ca-prod   # vault-ref, обязательно
      name: trusted-bastion-1                     # kebab-case, label в keeper_push_host_ca_used_total{ca_name=...}
    - ref: vault:secret/keeper/ssh-host-ca-stage
      name: trusted-bastion-2
  targets:
    - sid: soul-a.example.com                    # = souls.sid (FQDN)
      ssh_port: 22                               # опц., default 22
      ssh_user: root                             # опц., default root
      soul_path: /usr/local/bin/soul             # опц., default /usr/local/bin/soul
    - sid: soul-b.example.com
      ssh_port: 2222
      ssh_user: deploy
      soul_path: /opt/soul/bin/soul
  providers:
    - name: vault-bastion                         # = plugins.ssh_providers[].name
      params:                                     # opaque-форма провайдера, сериализуется в JSON
        vault_addr: https://vault.internal:8200   # и передаётся в env SOUL_SSH_VAULT_BASTION_PARAMS
        role: ssh-bastion-role
```

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `push.host_ca_refs[]` | `array<{ref, name}>` | — (обязателен один из: `host_ca_refs[]` или deprecated `host_ca_ref`) | Multi-CA-набор для verify host-cert на SSH-handshake-е (S7-3, [ADR-032 amendment](../architecture.md)). Каждый `ref` — vault-ref (`vault:<mount>/<path>`), `name` — operator-defined kebab-case (label в `keeper_push_host_ca_used_total{ca_name=...}`). На handshake-е делается **OR-проверка** через `ssh.CertChecker.IsHostAuthority` по всем CA: cert, подписанный любым → доверенный; иначе reject. Имена в наборе обязаны быть уникальны (`duplicate_push_host_ca_name`). Plaintext-PEM запрещён (`vault_ref_invalid`). Per-provider CA-override — отложено пост-MVP. |
| `push.host_ca_ref` | `vault-ref` | — | **Deprecated (S7-3, 1-release WARN window).** Singular vault-ref на public host-CA. При заполненном singular и пустом `host_ca_refs[]` daemon auto-adapt-ит singular в `host_ca_refs[0]` c auto-name `default` и пишет одноразовый WARN. Одновременное присутствие с `host_ca_refs[]` отвергается schema-фазой (`mutually_exclusive_keys`). Plaintext-inline-PEM запрещён («безопасность на первом месте»): не-vault-ref → diag `vault_ref_invalid`. Симметрия с `auth.jwt.signing_key_ref` / `sigil.signing_key_ref`. |
| `push.targets[].sid` | `string` (FQDN) | — | Обязательное. SID push-хоста, совпадает с `souls.sid`. SID без записи в `targets[]` → `target_not_configured` на резолве в SshDispatcher. Дубликаты SID отвергаются (`duplicate_push_target_sid`). |
| `push.targets[].ssh_port` | `int` (1..65535) | `22` | TCP-порт sshd на push-хосте. `0`/опущено → дефолт. |
| `push.targets[].ssh_user` | `string` | `root` | SSH-пользователь для входа. Опц. (типовое значение зависит от провайдера: vault-issued user-cert обычно принципалит конкретного юзера). |
| `push.targets[].soul_path` | `path` | `/usr/local/bin/soul` | Абсолютный путь к soul-бинарю на push-хосте. Доставляется ShaDeliverer-ом при первом push-прогоне (см. [push.md → Доставка](push.md#доставка-soul-бинаря-и-модулей-на-хост)); путь должен совпадать с тем, куда Deliverer кладёт бинарь. |
| `push.providers[].name` | `string` (kebab-case) | — | Обязательное. Имя SshProvider-плагина, ссылается на `plugins.ssh_providers[].name`. Дубликаты отвергаются (`duplicate_push_provider_name`). |
| `push.providers[].params` | `map<string, any>` | — | Opaque-форма параметров провайдера (vault_addr / role / proxy_addr / …). При spawn-е плагина сериализуется в JSON и кладётся в env-переменную с именем `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` ([ADR-020 amendment l](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)): `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`. Запись отсутствует → плагин стартует без env-payload (поведение зависит от самого плагина: `soul-ssh-static` работает с дефолтами, `soul-ssh-vault` без params вернёт ошибку). |
| `push.allow_legacy_push_targets` | `bool` | `false` | S7-1 deprecation window: PG-источник (`souls.ssh_target` jsonb) canonical, `push.targets[]` legacy. При `false` запись отсутствует в PG → `target_not_configured` (fail-closed); при `true` → fallback на inline-`targets[]` + одноразовый WARN на старте. После S8 hard-cut поле удаляется (`unknown_key`). |
| `push.allow_legacy_push_providers` | `bool` | `false` | S7-2 deprecation window: PG-источник (`push_providers` таблица) canonical, `push.providers[]` legacy. При `false` plugin без записи в PG → плагин стартует без env-payload; при `true` → fallback на inline-`providers[]` + одноразовый WARN. Симметрично `allow_legacy_push_targets`. |
| `push.auto_import_legacy_targets` | `bool` | `false` | S7-4 opt-in one-shot миграция (ADR-032 amendment 2026-05-26). При `true` daemon на старте (шаг `runLegacyAutoImport` после `setupPushProviderSvc`) проходит по `push.targets[]`: для каждого SID с `souls.ssh_target IS NULL` пишет данные + audit-event `soul.ssh-target.imported_from_config` (`source: config_bootstrap`). Идемпотентно (PG-row canonical, не перезаписывается). PG read/write fail → отказ старта. Отсутствующая `souls`-row → WARN-skip. См. [push.md → S7-4](push.md#s7-4-auto-import-legacy-on-start-2026-05-26). |
| `push.auto_import_legacy_providers` | `bool` | `false` | S7-4 opt-in one-shot миграция симметрично `auto_import_legacy_targets`: `push.providers[]` → PG-таблица `push_providers`. Импортированные записи несут `created_by_aid='archon-system'` (system-AID должен существовать в `operators` до первого импорта). Audit-event — `push-provider.imported_from_config`. Идемпотентно (`SelectByName` → `ErrPushProviderNotFound` гейтит INSERT). |

> **Pilot single-provider routing.** S6 поднимает SshDispatcher на **первый дискаверенный** SshProvider-плагин (single dispatcher, без routing по `push_runs.ssh_provider`). Multi-provider routing (`vault-bastion` для prod-хостов, `static` для dev-хостов в одном keeper-инстансе) — S7. Типичный pilot-кейс: один провайдер на keeper (типично `soul-ssh-vault` ИЛИ `soul-ssh-static`, не оба).

> **Migration к S7.** S7-1 перенёс `targets[]` в `souls.ssh_target jsonb` (canonical), S7-2 — `providers[]` в PG-table `push_providers`, S7-3 — `host_ca_ref` (singular) в `host_ca_refs[]` (multi-CA), S7-4 — opt-in auto-import inline-блоков при старте Keeper-а (флаги `auto_import_legacy_*`). Все legacy-поля останутся под 1-release WARN deprecation window, далее (S8) `unknown_key` hard-cut. До закрытия окна допустимо смешивать legacy с canonical (PG имеет priority).

## `rbac`

**Перенесён в БД (ADR-028).** Каталог RBAC (роли, привязки операторов, permissions) больше не часть конфиг-контракта — он живёт в Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`), управление через `role.*` API/MCP. Ключ `rbac:` в `keeper.yml` **не принимается**: парсер отвергает его с ошибкой `unknown_key`.

Нормативное описание RBAC — [rbac.md](rbac.md) (формат permissions, грамматика селектора, каталог, Bootstrap первого Архонта).

## `reactor`

**Архитектурно не закреплён.** Имя `reactor` пока не зафиксировано в [`naming-rules.md`](../naming-rules.md), требует propose-and-wait при следующем заходе на event-driven контур (см. [open Q №23](../architecture.md#текущие)). Формат правил, триггеры, действия, RBAC, ограничения, отношение к event-driven контуру — **не зафиксированы ни одним ADR**.

До отдельного ADR по дизайну блок **не нормативен**: парсер `keeper.yml` отвергает ключ `reactor:` с ошибкой `unknown_key`.

См. [open Q №23](../architecture.md#текущие) — event-driven контур (Salt beacons / engines-эквивалент) — родственный вопрос.

## `services` / `default_destiny_source` / `default_module_source`

**Перенесены в БД (ADR-029).** Реестр Service-ов и скаляр `default_destiny_source` живут в Postgres (`service_registry` / `keeper_settings`, см. [storage.md](storage.md)), управление — через `service.*` API/MCP ([operator-api.md](operator-api.md)); семантика резолва описана выше в разделе [«Реестр Service-ов и `default_destiny_source` — в Postgres»](#реестр-service-ов-и-default_destiny_source--в-postgres). `default_module_source` упразднён без замены (потребителя не было). Все три ключа (`services:` / `default_destiny_source:` / `default_module_source:`) в `keeper.yml` **не принимаются**: парсер отвергает каждый с ошибкой `unknown_key`.

## `audit`

```yaml
audit:
  enabled: true
  otel_export: true
  retention_days: 365
```

Общая нормировка audit-pipeline — [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention): storage (Postgres-таблица `audit_log`, см. [storage.md](storage.md)), schema (`audit_id` / `created_at` / `event_type` / `source` / `archon_aid` / `correlation_id` / `payload`), write-path (HTTP-middleware / MCP-handler / Reaper / hot-reload / `keeper.cloud` / `keeper.push` / bootstrap / Soul gRPC forwarded), retention (через Reaper-правило `purge_audit_old`, см. [reaper.md](reaper.md)). Каталог event-types — [naming-rules.md → Audit-events](../naming-rules.md#audit-events).

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `audit.enabled` | `bool` | `true` | Глобальный switch audit-pipeline-а. При `false` ни один из write-path инициаторов (см. [ADR-022(g)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) не пишет в Postgres `audit_log`; OTel dual-write (см. `otel_export` ниже) при этом тоже отключается. Использовать только для development / расследования инцидента — production-инсталляция должна держать `true` для compliance-инвариантов. |
| `audit.otel_export` | `bool` | `true` | Дублировать audit-event в OTel span как attribute (transient debugging aid, Postgres — источник правды). При `false` audit пишется только в `audit_log`; OTel-spans Keeper-а штатно продолжают идти через [`otel:`](#otel), но audit-attributes в них не добавляются. Полезно для инсталляций без OTel-инфраструктуры ([ADR-022(f)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |
| `audit.retention_days` | `int` (≥1) | `365` | Срок хранения записей в `audit_log` (дней). **Alias** на `reaper.rules.purge_audit_old.max_age` ([ADR-022(d)/(i)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — один источник правды на retention, удобная форма в днях здесь и `duration` в [`reaper:`](#reaper) ([reaper.md → Правила](reaper.md#правила)). При расхождении значений в двух блоках парсер `keeper.yml` отвергает конфиг с ошибкой `audit_retention_mismatch`. |

**У Soul нет блока `audit:`** — Soul физически не имеет доступа к Postgres `audit_log` (изоляция модулей, [ADR-011](../adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Audit-события Soul-стороны (`TaskEvent` / `RunResult` / `SoulprintReport`) идут через Keeper и пишутся им с `source: soul_grpc` ([ADR-022(b)/(g)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). OTel-attributes Soul пишет в свои gRPC EventStream spans штатно через `otel:` блок `soul.yml`.

### Hot-reload блока `audit:`

Все три поля **reload-able без рестарта** `keeper`-процесса: это параметры конкретного запуска write-path-инициаторов (читаются in-memory при каждой записи в `audit_log`) и параметр Reaper-правила (читается на следующей итерации Жнеца, [reaper.md](reaper.md)).

| Поле | Reload без рестарта `keeper`-процесса | Обоснование |
|---|---|---|
| `enabled` | да | Применяется in-memory при каждой записи; in-flight записи дорабатывают со старым значением. |
| `otel_export` | да | То же: per-record флаг, читается helper-ом `shared/audit` в момент write. |
| `retention_days` | да | Применяется Reaper-ом на следующей итерации (через alias на `reaper.rules.purge_audit_old.max_age`). |

## `hot_reload`

```yaml
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true
```

Блок регулирует включение триггеров hot-reload-механизма (`SIGHUP` / `inotify`) и генерацию `correlation_id` для audit-events `config.reload_succeeded` / `config.reload_failed`. Семантика и инварианты самого механизма — [ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) и [§ Hot-reload](#hot-reload) ниже. Блок целиком опционален: при отсутствии в `keeper.yml` применяются defaults из таблицы.

| Поле | Тип | Default | Смысл |
|---|---|---|---|
| `hot_reload.enable_signal` | `bool` | `true` | Включить `SIGHUP`-триггер file-edit-path: процесс ловит сигнал, перечитывает `keeper.yml` с диска, прогоняет validation pipeline и делает atomic swap ([ADR-021(b)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). При `false` file-edit-path отключён (изменения файла без рестарта не подхватываются); API/MCP-path работает независимо. |
| `hot_reload.enable_inotify` | `bool` | `false` | Включить auto-reload через `inotify`/`fsnotify` (Linux-only) — реагировать на изменение `keeper.yml` без `SIGHUP`. Post-MVP опциональное расширение ([ADR-021(b)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)): watch-handle overhead и Linux-only зависимость — причина не делать дефолтом. |
| `hot_reload.audit_correlation_id` | `bool` | `true` | Генерировать `correlation_id` для audit-events `config.reload_succeeded` / `config.reload_failed` ([naming-rules.md → Audit-events](../naming-rules.md#audit-events)). При `true` каждое событие reload получает уникальный id, попадающий и в OTel-spans, и в audit-trail (сшивает file-edit-path c API-path при последовательных мутациях). |

### Hot-reload блока `hot_reload:`

Все три поля **требуют рестарта** `keeper`-процесса — потому что они контролируют сам hot-reload-механизм: менять `enable_signal` / `enable_inotify` без рестарта = race условие на установку/снятие signal-handler-а или `inotify`-watch; менять `audit_correlation_id` на лету разделило бы одну логическую reload-операцию на два разных режима audit-логирования.

| Поле | Reload без рестарта `keeper`-процесса | Обоснование |
|---|---|---|
| `enable_signal` | **нет, требует рестарта** | Меняет привязку signal-handler-а к `SIGHUP`; race на установке/снятии обработчика. |
| `enable_inotify` | **нет, требует рестарта** | Меняет регистрацию `inotify`-watch на путь конфига; race на handle. |
| `audit_correlation_id` | **нет, требует рестарта** | Параметр самого reload-pipeline-а; менять его одним из reload-ов означало бы писать audit-event о собственной мутации в двух разных режимах. |

## Hot-reload

Hot-reload конфига с перезаписью изменённого значения обратно на диск — сквозное требование ([requirements.md](../requirements.md), [architecture.md → Сквозные требования](../architecture.md#сквозные-требования-и-где-они-приземляются)). Механизм нормирован **[ADR-021](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)**; имплементация — пакет [`shared/config/`](../adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам) (Tier 2).

**Два пути изменения конфига.**

| Путь | Триггер | Pipeline |
|---|---|---|
| **File-edit** | Оператор редактирует `keeper.yml` на хосте → шлёт `SIGHUP` процессу. | parse → schema-validate → semantic-validate → atomic swap → audit. |
| **API/MCP** | OpenAPI/MCP-мутация конфига (конкретные endpoints — отложены до Operator API, см. [ADR-021 → Consequences](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). | mutate → schema-validate → semantic-validate → atomic swap → **write-back YAML** → audit. |

**SIGHUP — single trigger в MVP** для file-edit-path. `inotify`/`fsnotify` — post-MVP опциональное расширение, по умолчанию выключено (см. [ADR-021(b)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)).

**Validation pipeline** — три этапа до atomic swap; любая ошибка → in-memory state неизменен, файл не модифицируется (даже для API-path), audit-event `config.reload_failed` с `phase ∈ {parse, schema_validate, semantic_validate}` ([ADR-021(c)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)).

**Write-back YAML** — только для API-path: round-trip preservation (сохраняются комментарии, порядок ключей, anchors) + atomic rename (write-to-tmp в той же директории + `rename(2)`) + permissions от исходного файла. File-edit-path ничего не пишет (файл уже на диске).

**Scope** — общий принцип: reload-able без рестарта — параметры конкретного запуска / прогона (timeouts, policies, thresholds, capabilities whitelist); require restart — внешняя поверхность процесса (listener-addresses, socket paths, TLS-сертификаты, Postgres/Redis DSN, log-rotation file paths). Summary-таблица ниже охватывает все блоки `keeper.yml` 1:1; блоки с нетривиальной per-поле семантикой (`plugin_runtime`, `hot_reload`) дополнительно нормированы отдельными таблицами в своих разделах.

**Summary per-block reload-policy** (нормативно, по одной строке на блок):

| Блок | Reload-able без рестарта | Require restart | Примечание |
|---|---|---|---|
| `kid` | — | `kid` | Идентификатор инстанса; смена = новый инстанс. |
| `listen.*` | — | все (`grpc.addr`, `openapi.addr`, `mcp.addr`, `metrics.addr`, `grpc.tls.*`, `grpc.event_stream.max_apply_size_mb`) | External surface; TLS files читаются при init context, `MaxSendMsgSize` задаётся на gRPC-сервере один раз при старте. |
| `postgres.pool.*` | `min`/`max` (растёт/сужается in-memory) | — | Параметры пула применяются к новым подключениям. |
| `postgres.dsn_ref` | — | yes | Open connections не пересоздаются. |
| `redis.*` | — | yes | Connection-strings + password. |
| `vault.addr` | — | yes | Open Vault-client connection. |
| `vault.auth.*` | — | yes | Re-auth только на старте. |
| `vault.pki_mount` | yes | — | Читается per-request. |
| `auth.jwt.signing_key_ref` | — | yes | Signing key загружается в memory на старте. |
| `auth.jwt.issuer` / `ttl_default` / `ttl_bootstrap` | yes | — | Применяются к **новым** выпускаемым токенам; уже выданные JWT действуют до своего exp. |
| `metrics.auth.basic.*` | — | yes | Пароль резолвится из vault на старте, listener поднимается один раз. |
| `otel.*` | — | yes | Re-init exporter / connection; `SetupOTel` вызывается один раз за процесс ([ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). |
| `logging.level` | yes | — | In-memory variable. |
| `logging.format` / `logging.file` / `logging.rotation.*` | — | yes | Re-init log writer / file handles. |
| `plugins.*` | yes | — | Cache reload artifact-store. |
| `reaper.enabled` / `dry_run` / `batch_size` / `rules.*` | yes | — | In-memory loop, next iteration видит новое. |
| `reaper.interval` | yes | — | Next iteration уже с новым интервалом. |
| `reaper.lock_ttl` | — | yes | Redis-lease TTL устанавливается при acquire. |
| `cadence_scheduler.interval` | yes | — | Conductor перечитывает на каждом тике из свежего Store-снимка ([conductor.md](conductor.md), ADR-048). |
| `cadence_scheduler.lock_ttl` | yes | — | TTL Redis-lease `conductor:leader` применяется между re-acquire. |
| `cadence_scheduler.enabled` | — | yes | Поднятие/гашение Conductor читается при старте `setupConductor`; hot-toggle подсистемы (lease + goroutine на лету) — отдельный slice. Оперативное управление расписаниями — per-Cadence `enabled` (ADR-046), без рестарта. |
| `acolytes` / `acolyte_lease` / `acolyte_batch` / `acolyte_poll_interval` / `acolyte_drain_grace` | — | yes | Параметры пула воркеров (ADR-027) читаются при старте `setupAcolyte`; перезапуск пула / переинъекция claim-параметров на лету — отдельный slice. |
| `watchman_interval` / `watchman_fail_threshold` | — | yes | Параметры Watchman (soul-shedding S2) читаются при старте `setupWatchman`; переинъекция периода/порога в живой probe-loop — отдельный slice. |
| `allow_unsafe_single_path_multi_keeper` | — | yes | Opt-out refuse-guard-а (Finding-A) читается при старте `setupConclaveRefuseGuard`; решение «refuse vs warn» принимается один раз до приёма стримов. |
| `tempo.voyage_create.rate` / `tempo.voyage_create.burst` | yes (оба) | — | Лимитер stateless: rate/burst читаются из свежего Store-снимка на каждом запросе ([§ tempo](#tempo), ADR-050). |
| `tempo.voyage_preview.rate` / `tempo.voyage_preview.burst` | yes (оба) | — | То же, отдельный bucket для `POST /v1/voyages/preview` ([§ tempo](#tempo), ADR-050). |
| `tempo.enabled` | — | yes | Подъём/гашение Tempo (Redis token-bucket) читается при старте `setupTempo`; hot-toggle подсистемы — отдельный slice (симметрично `toll.enabled`). |
| `rbac` | — | — | Перенесён в БД (ADR-028); парсер отвергает ключ с `unknown_key` ([§ rbac](#rbac)). |
| `reactor` | — | — | Парсер отвергает с `unknown_key` ([§ reactor](#reactor)). |
| `audit.enabled` / `audit.otel_export` / `audit.retention_days` | yes (все три) | — | Параметры write-path (`enabled`/`otel_export`) — in-memory per-record; `retention_days` — alias на `reaper.rules.purge_audit_old.max_age`, читается Reaper-ом на следующей итерации ([§ Hot-reload блока `audit:`](#hot-reload-блока-audit)). |
| **`plugin_runtime.*`** | per-поле — см. [§ Hot-reload блока `plugin_runtime:`](#hot-reload-блока-plugin_runtime) | | |
| **`hot_reload.*`** | per-поле — см. [§ Hot-reload блока `hot_reload:`](#hot-reload-блока-hot_reload) | | (все require restart) |

**Multi-host coordination** — нет в MVP. Каждый Keeper-инстанс HA-кластера ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) перезагружает свой `keeper.yml` независимо. Cross-host «reload по cluster-wide событию» через Redis pub/sub — post-MVP ([ADR-021(f)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)).

**Audit-events** — два имени, каталог в [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events):
- `config.reload_succeeded` — поля `source ∈ {signal, api}`, `archon.aid` (для API-path), `changed_paths` (список YAML-paths), `correlation_id`.
- `config.reload_failed` — поля `source`, `archon.aid` (если применимо), `validation_errors[]`, `phase ∈ {parse, schema_validate, semantic_validate}`.

**History** — git-blame YAML (для file-edit-path) + audit-trail в Postgres (для API-path). Отдельная БД-таблица `config_history` со snapshots — post-MVP ([ADR-021(i)](../adr/0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)).

**Опциональный блок `hot_reload:`** в `keeper.yml` (поля `enable_signal`, `enable_inotify`, `audit_correlation_id`) — нормативная типизация полей в [`## hot_reload`](#hot_reload) выше. При отсутствии блока применяются defaults оттуда (встроены в `shared/config`).

## Полный пример

Минимальный валидный `keeper.yml` со всеми обязательными полями:

```yaml
kid: keeper-eu-west-01

listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
    event_stream:
      addr: "0.0.0.0:9443"
      tls:
        cert: /etc/keeper/tls/server.crt
        key:  /etc/keeper/tls/server.key
        ca:   /etc/keeper/tls/ca.crt
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }

postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }

redis:
  addr: "redis-cluster.internal:6379"
  password_ref: vault:secret/keeper/redis

vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id
  pki_mount: "pki/soulstack"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 24h
    ttl_bootstrap: 720h

otel:
  enabled: true
  exporter: otlp
  endpoint: "otel-collector.internal:4317"

logging:
  level: info
  format: json
  rotation: { max_size_mb: 100, max_files: 10, compress: true }

# Реестр Service-ов и default_destiny_source — в Postgres (ADR-029),
# в keeper.yml их нет. Управление через service.* API/MCP.

plugins:
  cache_root: /var/lib/soul-stack-keeper/plugins
  cloud_drivers:
    - { name: aws, source: "git@github.com:soul-stack-ecosystem/soul-cloud-aws.git", ref: v2.0.0 }
    - { name: yc,  source: "git@github.com:our-company/soul-cloud-yc.git",          ref: v0.3.1 }
  ssh_providers:
    - { name: vault-ssh, source: "git@github.com:soul-stack-ecosystem/soul-ssh-vault.git", ref: v1.0.0 }
    - { name: static,    source: "git@github.com:soul-stack-ecosystem/soul-ssh-static.git", ref: main }

plugin_runtime:
  socket_dir: /var/run/soul-stack-keeper/plugins
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

audit:
  enabled: true
  otel_export: true
  retention_days: 365

watchman_interval: 5s
watchman_fail_threshold: 3

# web_ui_enabled: true   # дефолт ON — встроенный UI на /ui (ADR-055); false — opt-out

# allow_unsafe_single_path_multi_keeper: false  # дефолт refuse при multi-keeper + acolytes=0

reaper:
  enabled: true
  interval: 1h
  dry_run: false
  batch_size: 500
  lock_ttl: 5m
  rules:
    expire_pending_seeds: { enabled: true, max_age: 24h, action: delete }
    purge_used_tokens:    { enabled: true, max_age: 90d, action: delete }
    purge_souls:          { enabled: true, statuses: [disconnected, expired], max_age: 30d, action: delete }
    purge_old_seeds:      { enabled: true, statuses: [superseded, expired, revoked], max_age: 90d, action: delete }
    mark_disconnected:    { enabled: true, stale_after: 90s, action: set_status, target_status: disconnected }
    purge_audit_old:      { enabled: true, max_age: 365d, action: delete }
```

RBAC-каталог (роли, привязки, permissions) в `keeper.yml` не задаётся — он в Postgres (ADR-028), управляется через `role.*` API/MCP; ключ `rbac:` отвергается как `unknown_key`.

Эталонный пример в файле — [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml).

## См. также

- [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) — рабочий пример всего конфига целиком.
- [concept.md](concept.md) — какие задачи решает Keeper, какие блоки конфига к каким задачам относятся.
- [storage.md](storage.md) — что лежит за `postgres:` и `redis:`, реестр `operators` для `auth:` и RBAC-таблицы (ADR-028).
- [push.md](push.md) — потребитель `plugins.ssh_providers`.
- [cloud.md](cloud.md) — потребитель `plugins.cloud_drivers`.
- [reaper.md](reaper.md) — полное описание блока `reaper:` и правил чистки.
- [rbac.md](rbac.md) — полное описание блока `rbac:`, разбор permissions, Bootstrap первого Архонта.
- [architecture.md → ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) — bootstrap первого Архонта.
- [architecture.md → ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon) — identity-модель оператора, источник правды по `auth:`.
- [architecture.md → Сквозные требования](../architecture.md#сквозные-требования-и-где-они-приземляются) — Vault, OTel, RBAC, MCP, OpenAPI, hot-reload, ротация логов как обязательные.
- [naming-rules.md](../naming-rules.md) — словарь имён.
