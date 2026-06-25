# monitoring

Пример сервиса, который разворачивает Prometheus-экспортеры на VM:

- **node-exporter** — системные метрики, слушает стандартный `:9100` по умолчанию.
  Делегируется в переиспользуемую standalone-destiny
  [`node-exporter`](../../destiny/node-exporter/) через
  `apply:destiny` (изоляция input, [ADR-009](../../../docs/adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)):
  бинарь `node_exporter` под **стабильным system-аккаунтом `node_exporter`**
  (stateful-ветка), version-aware install + hardened-unit, `arch` из soulprint.
  Опциональные textfile-коллекторы железа (smartmon/nvme/ipmi) выключены здесь
  (`node_exporter_collectors: []`) — на VM железа нет, ставится только ядро;
- **redis_exporter** — метрики Redis, подключается к Redis **через unix-сокет**
  (`--redis.addr=unix:///<path>`), слушает `:9121` по умолчанию. Работает под
  выделенным системным пользователем `redis-exporter` в группе `redis`
  (least-privilege доступ к сокету). **Остаётся ИНЛАЙН** (не `apply:destiny`)
  осознанно — см. «Допущения» ниже.

Авторинг на существующих core-модулях MVP ([ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)),
без нового Go-кода ядра.

> **Почему redis_exporter — инлайн, а node-exporter — destiny.** Прод-destiny
> [`redis-exporter`](../../destiny/redis-exporter/) требует
> `redis_password` (required) и несёт `Requires=redis-server.service` в unit-е —
> она рассчитана на хост с локальным Redis под паролем. Назначение
> `monitoring` иное: экспортеры на хосте с **уже работающим** Redis
> (возможно, без `requirepass`), сервис сам Redis НЕ ставит. Перевод сломал бы это
> и потребовал бы расширения публичного контракта новым required-полем. Дрейф с
> node-exporter устранён; redis_exporter здесь — отдельный кейс, не дубль эталона.

## Допущения

- **Target:** Linux + systemd (Debian/Ubuntu/RHEL/Alpine с systemd).
- **redis_exporter ↔ Redis — через unix-сокет**, путь сокета — параметр
  `redis_socket` (дефолт `/var/run/redis/redis-server.sock`).
- **Группа `redis` уже существует на хосте.** Этот сервис Redis НЕ ставит — он
  подключается к уже работающему Redis. Сокет создаётся redis-server с правами
  `unixsocketperm 770` owner `redis:redis`, поэтому redis_exporter работает под
  выделенным системным пользователем `redis-exporter` (шаг `core.user`, no-login),
  включённым в supplementary-группу `redis` — доступ к сокету по «group»-битам.
  Группу `redis` создаёт пакет redis-server; если Redis на хосте есть, группа
  уже на месте. `DynamicUser` для redis_exporter **не используется**: эфемерный
  UID systemd не входит в группу `redis` и получил бы `EACCES` на `connect()`.
  node-exporter сокета не требует; его аккаунт — стабильный system-user
  `node_exporter` (stateful-ветка: textfile-каталог `/var/lib/node_exporter`
  переживает рестарты и читается root-сборщиками), это решает сама destiny
  `node-exporter`. Если группы
  `redis` нет (Redis на хосте ещё не установлен) — шаг `core.user` упадёт; в этом
  случае установите Redis заранее или добавьте `core.group.present name: redis`.
- **Версии экспортеров** пиннятся через input на конкретные релизы
  (`node_exporter_version`, `redis_exporter_version`). Для redis_exporter
  (инлайн-fetch) вместе с версией оператор обязан передать checksum tarball-а
  (`redis_exporter_sha256`, `required: true`); node_exporter качается через destiny
  `node-exporter`, у которой checksum-input убран (скачивание без verification).
- **Установка — единый паттерн «тройка»:** `core.url.fetched` (скачать tarball с
  GitHub Releases) → `core.archive.extracted` (распаковать) → `core.cmd.shell`
  (`install -m0755` бинаря в `bin_dir`). Скачивание делает специализированный
  `core.url.fetched` — **https-only**; при заданном checksum — **верификация ДО
  публикации файла** (неверный хэш не материализуется, supply-chain), при пустом
  checksum — idempotency по SHA-256 содержимого. `core.cmd.shell` остаётся только
  для локального install-шага (`core.file.present` копировать из пути не умеет —
  только inline-content), сети в нём нет.
- **Checksum redis_exporter — обязательный input** (`redis_exporter_sha256`,
  форма `"sha256:<hex>"`, `required: true` **без default**, fail-closed по
  [прод-конвенции §7](../../../docs/destiny/production-conventions.md#7-supply-chain)).
  Дефолта нет: нет хэша → честный отказ резолва, а не fetch с placeholder-ом.
  Хэш берётся из `sha256sums.txt` соответствующего GitHub-релиза под пару
  (version, arch); `core.url.fetched` верифицирует его ДО публикации файла.

Если какое-то из допущений не так (например, нужен пакет из репозитория дистрибутива
вместо tarball, или подключение к Redis по TCP, а не сокету) — поправьте input/scenario.

## Раскладка

```
monitoring/
├── service.yml                              # манифест: state_schema_version=1,
│                                            #   destiny[] (node-exporter),
│                                            #   state_schema {node/redis версии, redis_socket}
├── essence/
│   └── _default.yaml                        # baseline: версии + путь сокета (подложка)
└── scenario/
    └── create/
        ├── main.yml                         # input + tasks (apply:destiny node-exporter
        │                                     #   + инлайн redis_exporter) + state_changes
        ├── templates/
        │   └── redis_exporter.service.tmpl   # systemd-unit redis_exporter (инлайн, unix-сокет)
        └── tests/
            └── render-defaults/case.yml     # L0-trial (render-only)
```

`node_exporter.service.tmpl` удалён: node-exporter рендерит сама destiny.
`redis_exporter.service.tmpl` остаётся — его использует инлайн-блок redis_exporter.

Каталога `migrations/` нет: `state_schema_version = 1` ([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## Что делает scenario `create`

На каждом хосте incarnation:

1. **node-exporter** — один `apply:destiny` в
   [`node-exporter`](../../destiny/node-exporter/): тройка
   `core.url.fetched` (checksum-пин) → `core.archive.extracted` → version-aware
   `core.cmd.shell install` (бинарь раскладывается как `node_exporter`,
   `unless --version` ловит апгрейд) → `core.group.present`/`core.user.present`
   (стабильный system-аккаунт `node_exporter`) → `core.file.rendered` hardened-unit
   → `core.service.running` (`enabled: true`) + рестарт по `onchanges`. При
   `node_exporter_collectors: []` (дефолт сервиса) ставится только ядро; шаги
   привилегированных коллекторов выключены по `when:`. scenario
   прокидывает destiny её input; `arch` берётся из `soulprint.self.os.arch`.
2. **redis_exporter** — инлайн (не `apply:destiny`, см. выше): та же тройка
   url.fetched → archive.extracted → install → `core.user.present`
   (`redis-exporter` в группе `redis`) → `core.file.rendered` unit (без пароля,
   `User=redis-exporter`) → `core.service.enabled` → `core.service.running` →
   рестарт по `onchanges`. Имя tarball-а несёт префикс `v` перед версией
   (`redis_exporter-v<v>.linux-<arch>`).

`core.service.enabled` и `core.service.running` в инлайн-блоке redis_exporter —
**два отдельных шага**: state `running` модуля `core.service` управляет только
активностью и не читает параметр `enabled`. (В destiny node-exporter применена
единая enable-идиома — `enabled: true` внутри `running`.)

### Подключение redis_exporter через unix-сокет

В scenario `redis_addr` собирается CEL-ом: `${ 'unix://' + input.redis_socket }`
и пробрасывается в `redis_exporter.service.tmpl`, где попадает в
`ExecStart … --redis.addr={{ .vars.redis_addr }}`. При дефолтном сокете итог —
`--redis.addr=unix:///var/run/redis/redis-server.sock`.

## input-контракт (docs/input.md)

| Параметр | Тип | Default | Назначение |
|---|---|---|---|
| `node_exporter_version` | string (semver-like) | `1.8.2` | Версия node_exporter. |
| `redis_exporter_version` | string (semver-like) | `1.62.0` | Версия redis_exporter. |
| `redis_exporter_sha256` | string `sha256:<hex>`, **required** | — | SHA-256 tarball-а redis_exporter. Из `sha256sums` GitHub-релиза под пару (version, arch). |
| `arch` | enum `amd64`/`arm64` | `amd64` | Архитектура релизного tarball-а. Задаётся в контракте scenario, но обе destiny берут arch напрямую из `soulprint.self.os.arch` — input в задачи не доезжает (один incarnation может смешивать amd64/arm64-хосты). |
| `bin_dir` | string (abs path) | `/usr/local/bin` | Куда раскладываются бинарники. |
| `redis_socket` | string (abs path) | `/var/run/redis/redis-server.sock` | Unix-сокет Redis. |
| `node_exporter_listen` | string `host:port` | `:9100` | Listen-адрес node_exporter (`--web.listen-address`). |
| `node_exporter_collectors` | array enum `smartmon`/`nvme`/`ipmi` | `[]` | Какие textfile-коллекторы железа node-exporter ставить. `[]` = только ядро (на VM железа нет). |
| `redis_exporter_listen` | string `host:port` | `:9121` | Listen-адрес redis_exporter. |
| `redis_exporter_extra_args` | array string | `[]` | Доп. флаги инлайн-redis_exporter (`--check-keys=…`, `--web.config.file=…` для TLS/basic-auth и пр.). Каждый элемент — отдельный токен `ExecStart`. |

Обязателен checksum-параметр redis_exporter (`redis_exporter_sha256`, fail-closed по
[прод-конвенции §7](../../../docs/destiny/production-conventions.md#7-supply-chain));
node_exporter качается через destiny `node-exporter` без checksum; остальное — дефолты.

## Идемпотентность

Все шаги повторно-применимы:

- `core.url.fetched` — checksum совпал с уже скачанным tarball-ом → no-op (контент
  не качается заново);
- `core.archive.extracted` — marker-файл `.soul-archive.sha256` совпал → no-op
  (повторно не распаковывает тот же архив);
- `core.cmd.shell` — `creates: <bin>` → no-op, если бинарь уже стоит;
- `core.file.rendered` — пишет файл только при diff содержимого (SHA-256);
- `core.user.present` — no-op, если пользователь `redis-exporter` уже существует
  (present-or-create, без reconcile групп в MVP — см. `coremod/user`);
- `core.service.enabled` / `core.service.running` — declarative, no-op если уже
  enabled/active;
- `core.service.restarted` — срабатывает только по `onchanges` от изменившегося unit-а.

## Валидация

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/monitoring/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/monitoring/scenario/create/main.yml
```

Оба дают exit 0 и `OK: <path>`.

## L0-trial (render-only)

```bash
./keeper/bin/soul-trial run examples/service/monitoring/scenario/create/tests/render-defaults/case.yml
```

Кейс герметичен (render-only, ничего не качает): проверяет CEL-render задач при
штатных input-ах. План — композит destiny node-exporter (`apply:destiny`, индексы
0..5) + инлайн redis_exporter (6..13). Ассертит сборку url-ов из `version`+`arch`
(`arch` из soulprint) для `core.url.fetched` (checksum-пин — только у redis_exporter),
сборку `redis_addr=unix://...`, создание пользователя `redis-exporter`
(`core.user.present`) и декларативные `core.service.{enabled,running}`-шаги. Даёт
`PASS`.

> `apply:destiny node-exporter` резолвится зеркалом прода (slice A, ADR-023): имя →
> `service.yml::destiny[]` + URL из `fixtures.default_destiny_source` кейса
> (`file://../../destiny/{name}`, герметично). `fixtures.soulprint.os.arch`
> даёт `arch` хоста, который экспортеры берут из `soulprint.self.os.arch`.

> **Реальный прогон** (установка бинарей, поднятие сервисов) требует Linux+systemd
> и сетевого доступа к GitHub Releases — на dev-mac не выполняется. L0-trial
> покрывает render-фазу; интеграционный прогон — на linux-стенде через
> `keeper.push` / pull-агента. `redis_exporter_sha256` — обязательный параметр
> (без дефолта): для реального прогона передайте настоящий хэш из `sha256sums`
> GitHub-релиза, иначе `core.url.fetched` упадёт на checksum mismatch (а без
> значения вовсе — резолв откажет fail-closed). node_exporter качается через
> destiny `node-exporter` без checksum (idempotency по SHA-256 содержимого).

## Чего здесь специально нет

- `migrations/` — `state_schema_version = 1`.
- `node_exporter.service.tmpl` — нет: node-exporter рендерит сама destiny
  (`redis_exporter.service.tmpl` остаётся для инлайн-блока).
- `on:` / `where:` — опущенный `on:` означает «весь incarnation»
  ([orchestration.md §3](../../../docs/scenario/orchestration.md)).
- `core.cmd.shell` для скачивания — скачивание делает `core.url.fetched`
  (https-only + checksum); `core.cmd.shell` остался только под локальный
  install-шаг (см. «Допущения»).
