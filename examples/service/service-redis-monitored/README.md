# service-redis-monitored

Базовый пример сервиса: **Redis + мониторинг** на одной VM, собранный
**КОМПОЗИЦИЕЙ трёх переиспользуемых standalone-destiny** через `apply:destiny`
(изолированный render каждой, [ADR-009](../../../docs/adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).
Scenario `create` оркеструет на каждом хосте incarnation три компонента в строгом
порядке:

- **redis-single** — redis-server из пакета дистрибутива + `redis.conf` (dual-access
  TCP+unix-сокет) + systemd-hardening drop-in;
- **redis-exporter** — метрики Redis, подключается к Redis **через unix-сокет**
  (`--redis.addr=unix:///<path>`), слушает `:9121`. Сервис-аккаунт — `DynamicUser=yes`
  + `SupplementaryGroups=redis` (group-доступ к сокету 770), пароль через
  `Environment=REDIS_PASSWORD`;
- **node-exporter** — системные метрики, слушает `:9100`, под `DynamicUser=yes`.

Сервис ничего не рендерит сам — он только прокидывает каждой destiny её
`apply.input`; внутри destiny виден ТОЛЬКО переданный ей input (изоляция ADR-009).
Прод-grade установка (hardening, `DynamicUser`, passthrough `extra_args`, `arch`
из soulprint) живёт в самих destiny — прежний инлайн-дрейф устранён.

Авторинг на существующих core-модулях MVP ([ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)),
без нового Go-кода ядра. Те же три destiny, что и в
[`service-redis`](../service-redis/) (full-stack-эталон apply:destiny): они лежат
в [`examples/destiny/`](../../destiny/) (`destiny-redis-single` /
`destiny-redis-exporter` / `destiny-node-exporter`).

## Допущения

- **Target:** Linux + systemd (Debian/Ubuntu/RHEL/Alpine с systemd). Имена
  `redis-server` (пакет/unit) и путь `/etc/redis/redis.conf` — конвенция
  debian/ubuntu-пакета redis.
- **redis ↔ redis_exporter — через unix-сокет.** Путь сокета — единый параметр
  `redis_socket` (дефолт `/var/run/redis/redis-server.sock`): тот же путь идёт
  в `redis.conf` (`unixsocket`) и в `redis_exporter` (`--redis.addr=unix://...`).
  Это точка интеграции — менять надо в одном месте.
- **Версии пиннятся через input.** `redis_version` — версия пакета redis-server;
  `node_exporter_version` / `redis_exporter_version` — релизы стороннего ПО с
  GitHub Releases; вместе с версией пиннится checksum tarball-а
  (`node_exporter_sha256`, `redis_exporter_sha256`).
- **Экспортеры ставятся единым паттерном «тройка»:** `core.url.fetched` (скачать
  tarball с GitHub Releases) → `core.archive.extracted` (распаковать) →
  `core.cmd.shell` (`install -m0755` бинаря в `bin_dir`). Скачивание делает
  специализированный `core.url.fetched` — **https-only** и **checksum-верификация
  ДО публикации** (неверный хэш не материализуется, supply-chain).
  `core.cmd.shell` остаётся только под локальный install-шаг (`core.file.present`
  копировать из пути не умеет — только inline-content), сети в нём нет. Redis —
  наоборот, declarative-пакет (`core.pkg.installed`).
- **Checksum tarball-ов пиннится через input** (`node_exporter_sha256`,
  `redis_exporter_sha256`, форма `"sha256:<hex>"`). Дефолты — **placeholder
  (нули)** под версии/arch по умолчанию: реальный sha256 без сети не подтверждён,
  в `main.yml` стоят `TODO`-пометки. Для реального прогона передайте настоящие
  хэши (из `sha256sums.txt` GitHub-релиза), иначе `core.url.fetched` упадёт на
  checksum mismatch.
- **Пароль Redis** (`redis_password`, `secret`, обязателен) уходит в обе destiny:
  в `redis-single` пишется в `requirepass` redis.conf, в `redis-exporter`
  пробрасывается через `Environment=REDIS_PASSWORD` (не в `ExecStart` — не светится
  в `ps`/journalctl). Отличие от [`service-redis`](../service-redis/): там пароль
  читается keeper-side через `vault()`, здесь — литерал/vault-ref из `input`.
- **Доступ redis_exporter к сокету — через группу `redis`.** Сокет создаётся
  redis-server с правами `unixsocketperm 770` owner `redis:redis` → «other» не
  имеет доступа. В прод-destiny `redis-exporter` сервис-аккаунт даёт сам systemd
  через `DynamicUser=yes`, а group-доступ к сокету — через `SupplementaryGroups=redis`
  (эфемерный uid добавляется в статически-named группу `redis`, которую создаёт
  пакет redis-server из `redis-single`). Ручного `core.user` больше нет —
  least-privilege без стабильного uid. node-exporter сокет не нужен — тоже под
  `DynamicUser=yes`.
- **arch — из фактов хоста.** `arch` экспортеров берётся из `soulprint.self.os.arch`
  (а не из статичного `input.arch`): один incarnation может смешивать
  amd64/arm64-хосты. destiny soulprint напрямую не видит (изоляция ADR-009) —
  значение доезжает через `apply:input`.

Если допущение не так (TCP вместо сокета, redis из tarball вместо пакета,
не-systemd target) — поправьте input scenario или соответствующую destiny.

## Раскладка

```
service-redis-monitored/
├── service.yml                              # манифест: state_schema_version=1,
│                                            #   destiny[] (redis-single/-exporter/node-exporter),
│                                            #   state_schema {3 версии + redis_socket}
├── essence/
│   └── _default.yaml                        # baseline: версии + путь сокета (подложка)
└── scenario/
    └── create/
        ├── main.yml                         # input + tasks (3× apply:destiny) + state_changes
        └── tests/
            └── render-defaults/case.yml     # L0-trial (render-only, композит destiny)
```

Каталога `templates/` больше нет: redis.conf и systemd-unit'ы рендерят сами
destiny (`destiny-redis-single` / `destiny-redis-exporter` / `destiny-node-exporter`),
прежние инлайн-копии удалены вместе с дрейфом.

Каталога `migrations/` нет: `state_schema_version = 1`
([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## Что делает scenario `create`

На каждом хосте incarnation — три `apply:destiny` в строгом порядке. Каждая
destiny раскрывается изолированным render-проходом в свои N задач со сквозными
индексами; внутри неё виден только переданный ей `apply.input`.

1. **redis-single** (`apply:destiny`): `core.pkg redis-server` → каталог сокета
   (`core.exec install -d`) → `core.file.rendered` redis.conf (dual-access TCP+socket)
   → systemd-hardening drop-in + `daemon-reload` → `core.service.running` +
   рестарт по изменению. Пакет создаёт группу `redis` и сокет (770) — предпосылка
   для redis-exporter.
2. **redis-exporter** (`apply:destiny`): «тройка» `core.url.fetched` (checksum-пин)
   → `core.archive.extracted` → `core.cmd.shell install` → `core.file.rendered`
   systemd-unit (`DynamicUser=yes`, `SupplementaryGroups=redis`,
   `Environment=REDIS_PASSWORD`, hardening) → `core.service.running` + рестарт.
   Идёт **после** redis-single — нужны группа/сокет. Имя tarball-а несёт префикс
   `v` перед версией (`redis_exporter-v<v>.linux-<arch>`).
3. **node-exporter** (`apply:destiny`): та же «тройка» + systemd-unit под
   `DynamicUser=yes` + hardening. От Redis не зависит, идёт последним для
   детерминизма.

### Подключение redis_exporter через unix-сокет

Scenario передаёт один `input.redis_socket` в обе destiny: в `redis-single` он
становится `unixsocket` в redis.conf, в `redis-exporter` — `redis_addr` собирается
destiny как `'unix://' + redis_socket` и уходит в
`ExecStart … --redis.addr=unix:///<socket>`. При дефолтном сокете итог —
`--redis.addr=unix:///var/run/redis/redis-server.sock` — **тот же путь**, что
`unixsocket`. Это единая точка интеграции — менять надо в одном месте (input).

## input-контракт (docs/input.md)

| Параметр | Тип | Default | Назначение |
|---|---|---|---|
| `redis_version` | string (semver-like) | `7.2.4` | Версия пакета redis-server. |
| `redis_password` | string, **secret**, **required**, min_length 16 | — | `requirepass` Redis + env redis_exporter. |
| `redis_socket` | string (abs path) | `/var/run/redis/redis-server.sock` | Unix-сокет Redis (точка интеграции). |
| `redis_maxmemory` | string (redis-формат) | `256mb` | Лимит памяти Redis. |
| `node_exporter_version` | string (semver-like) | `1.8.2` | Версия node_exporter. |
| `node_exporter_sha256` | string `sha256:<hex>` | placeholder (нули) | SHA-256 tarball-а node_exporter. **TODO: реальный хэш релиза.** |
| `redis_exporter_version` | string (semver-like) | `1.62.0` | Версия redis_exporter. |
| `redis_exporter_sha256` | string `sha256:<hex>` | placeholder (нули) | SHA-256 tarball-а redis_exporter. **TODO: реальный хэш релиза.** |
| `arch` | enum `amd64`/`arm64` | `amd64` | Архитектура релизного tarball-а экспортеров. |
| `bin_dir` | string (abs path) | `/usr/local/bin` | Куда раскладываются бинарники экспортеров. |
| `node_exporter_listen` | string `host:port` | `:9100` | Listen-адрес node_exporter. |
| `redis_exporter_listen` | string `host:port` | `:9121` | Listen-адрес redis_exporter. |

Единственный обязательный параметр — `redis_password`. Остальное — дефолты;
`create` на чистой VM проходит с одним аргументом-паролем.

## state_changes

После успешного apply Keeper фиксирует в `incarnation.state` (Form C, map
поле→CEL, пилот-контекст `input`/`incarnation`/`soulprint.self`):

```yaml
redis_version, node_exporter_version, redis_exporter_version, redis_socket
```

Этого достаточно, чтобы оператор видел развёрнутое и чтобы повторный `create`
был идемпотентен.

## Идемпотентность

Все шаги — внутри destiny; идемпотентность гарантируют сами модули (подробности —
в README каждой destiny в [`examples/destiny/`](../../destiny/)):

- `core.pkg.installed` — declarative, no-op если уже стоит нужная версия;
- `core.url.fetched` — checksum совпал с уже скачанным tarball-ом → no-op;
- `core.archive.extracted` — marker-файл `.soul-archive.sha256` совпал → no-op;
- `core.cmd.shell` install — `creates: <bin>` → no-op, если бинарь уже стоит;
- `core.exec.run install -d` — `creates: <dir>` → no-op, если каталог сокета есть;
- `core.file.rendered` — пишет файл только при diff содержимого (SHA-256);
- `core.service.running` (`enabled: true`) — declarative, no-op если уже
  enabled/active;
- `core.service.restarted` — срабатывает только по `onchanges` от изменившегося
  конфига/unit-а/drop-in-а.

Ручного `core.user`/`core.group` для экспортеров больше нет — сервис-аккаунт даёт
`DynamicUser=yes` в unit-е (прод-конвенция, см. destiny redis-exporter/node-exporter).

## Валидация

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/service-redis-monitored/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/service-redis-monitored/scenario/create/main.yml
```

Оба дают exit 0 и `OK: <path>`.

## L0-trial (render-only)

```bash
./keeper/bin/soul-trial run examples/service/service-redis-monitored
```

Кейс герметичен (render-only, ничего не качает и не ставит): проверяет CEL-render
композита из трёх destiny при штатных input-ах. План — конкатенация задач
redis-single (0..6) + redis-exporter (7..12) + node-exporter (13..18) со сквозными
индексами. Ассертит проброс пути сокета в redis.conf, сборку `redis_addr=unix://...`
для redis_exporter, `arch` из soulprint в url-ах экспортеров и декларативные
`core.service.running`-шаги. Даёт `PASS`.

> `apply:destiny` резолвится зеркалом прода (slice A, ADR-023): имя →
> `service.yml::destiny[]` + URL из `fixtures.default_destiny_source` кейса
> (`file://../../destiny/destiny-{name}`, герметично). `fixtures.soulprint.os.arch`
> даёт `arch` хоста, который экспортеры берут из `soulprint.self.os.arch`.

> L0 **не применяет** scenario input-defaults (их подставляет Keeper при
> инвокации, до render-фазы), поэтому fixtures кейса дублируют дефолты + задают
> обязательный `redis_password`.

> **Реальный прогон** (установка пакета/бинарей, поднятие сервисов) требует
> Linux+systemd и сетевого доступа к зеркалу дистрибутива и GitHub Releases — на
> dev-mac не выполняется. L0-trial покрывает render-фазу; интеграционный прогон —
> на linux-стенде через `keeper.push` / pull-агента. Перед реальным прогоном
> замените placeholder-хэши `node_exporter_sha256` / `redis_exporter_sha256` на
> настоящие — иначе `core.url.fetched` упадёт на checksum mismatch.

## Чего здесь специально нет

- `migrations/` — `state_schema_version = 1`.
- `templates/` — нет: redis.conf и unit'ы рендерят сами destiny.
- Инлайн-задачи установки — нет: всё делегировано в три standalone-destiny через
  `apply:destiny` (изоляция input, ADR-009). Прежние инлайн-копии удалены вместе с
  дрейфом; см. full-stack-эталон [`service-redis`](../service-redis/).
- `on:` / `where:` — опущенный `on:` означает «весь incarnation»
  ([orchestration.md §3](../../../docs/scenario/orchestration.md)).
