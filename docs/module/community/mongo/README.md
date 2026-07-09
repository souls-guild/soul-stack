# community.mongo

ОСНОВНОЙ интерфейс к **живому MongoDB** (PILOT-срез, концепция Ansible-роли
`mongodb`): scenario сервиса оркеструет порядок/таргетинг/health-gate, а плагин
исполняет **одну** операцию над одним `mongod`-инстансом. Custom-плагин
`kind: soul_module` (namespace `community`, name `mongo`), бинарь
`soul-mod-community-mongo`. Реализация —
[`examples/module/soul-mod-community-mongo/`](../../../../examples/module/soul-mod-community-mongo/)
(`impl.go` — диспетчер + states pinged/command, `user.go` — user-state
(createUser/dropUser + localhost-exception bootstrap), `conn.go`/`tls.go` —
коннект, `helpers.go` — structpb/secret-helpers).

Backend — [`go.mongodb.org/mongo-driver`](https://go.mongodb.org/mongo-driver):
плагин сам коннектится к `mongod` по TCP (`host:port`, обычно `127.0.0.1:27017`).
Работа идёт **через драйвер, а не `core.exec` + `mongosh`** (пароль в `argv` —
ИБ-риск, хрупкий парсинг вывода; тот же путь `shell → плагин`, что прошёл
[`community.redis`](../redis/README.md)). Соответствие core-модулям
(`core.pkg`/`core.file`/`core.service`/`core.sysctl`) — для всего, что НЕ
mongo-специфично (установка, render `mongod.conf`, systemd, host-tuning); сам
MongoDB-рантайм — этот плагин.

> **★ PILOT-скоуп (2026-06-30).** Реализованы **3 state** под топологию
> **standalone** (один `mongod`, `security.authorization: enabled`, admin через
> localhost-exception). ВНЕ pilot (следующие слайсы, документируются, когда
> появятся в коде): replica-set (`replSetInitiate`/add-member/member-synced),
> sharded (`mongos`/config/shard + [Choir](../../../naming-rules.md#сущности-предметной-области)),
> `keyFile` (внутрикластерная SCRAM-аутентификация), TLS (mongo в pilot — plain;
> параметры `tls*` объявлены в manifest для forward-compat, в pilot-сценарии не
> задаются).

## Без dry-run preview (осознанно)

Плагин остаётся на `module.BaseModule` — **не** реализует `PlanReadSafe`
([ADR-031](../../../adr/0031-scry-drift.md)) и `ErrandReadSafe`
([ADR-033](../../../adr/0033-errand.md)). Это сознательный выбор (параллель
`community.redis`): на `dry_run` host (Soul) применяет **default-deny** — задача
получает честный «drift не поддержан», а не ложное «нет дрифта».

## States

Текущий срез — **три** state (`manifest.yaml::spec.states`): read-probe `pinged`,
imperative-upsert `user` (createUser/dropUser), императивный `command`.

| State | Назначение | `changed` |
|---|---|---|
| `pinged` | Health-probe через go-mongo-driver `Ping` (primary). Read-only. Заменяет idiom `command { ping: 1 }` — health-gate в сценариях (`retry`/`until`/`failed_when` по `register.self.ok`). | `false` **конструктивно** (probe, не изменение). |
| `user` | `createUser`/`dropUser` (upsert). Юзеры MongoDB живут в `admin.system.users` (imperative), **НЕ** в конфиг-файле (в отличие от redis `users.acl`) — поэтому verb-state, а не рендер. `state: present` создаёт (если нет), `absent` удаляет (если есть). Идемпотентен по `usersInfo`. ★ первый admin создаётся через **localhost-exception** (см. ниже). | `true` при реальном create/drop; `false` (no-op), если юзер уже в нужном состоянии (present+есть / absent+нет). |
| `command` | Raw `db.runCommand` (imperative verb-state, прецедент `community.redis.command`/`core.exec.run`). | `false` по умолчанию (probe); `changed: true` в params — для реально мутирующих команд (оператор отвечает за идемпотентность). |

## pinged — params

Health-probe через go-mongo-driver `Ping` (primary). **Read-only**,
`changed=false` конструктивно. `Output.ok == true` — условие для health-gate
(`until: register.self.ok == true`); в сценарии `create` используется как gate
«mongod ответил» **до** bootstrap admin. Ошибка `Ping` (mongod ещё не поднялся,
недоступен) → `failed`.

> Сам `Ping` авторизации не требует, поэтому `pinged` **до** создания
> `default_admin` проходит по localhost-exception (пустая admin-БД). `password`
> в pilot-сценарии на этом шаге не задаётся.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес `mongod`: `host:port` (обычно `127.0.0.1:27017`). |
| `username` | string | optional | ACL-username для AUTH (если не anonymous). |
| `password` | string (secret) | optional | Пароль MongoDB. vault-ref в operator-input, keeper резолвит до Apply (см. «Пароль»). Маскируется; в `Ping` не передаётся (уходит в коннект). |
| `auth_db` | string | optional (default `admin`) | `authenticationDatabase`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | TLS-параметры коннекта (см. [«TLS-коннект»](#tls-коннект-forward-compat-pilot--plain)). PILOT: mongo в plain — не задаются. |

**Output**: `ok` (bool) — `true` при успешном `Ping`.

## user — params

`createUser`/`dropUser` (upsert) над живым `mongod` целиком через go-mongo-driver.
Идемпотентен по `usersInfo(name)`: `present` + юзер есть → no-op (смена
пароля/ролей существующего юзера — операционный сценарий, вне pilot); `present` + нет →
`createUser` (`changed=true`); `absent` + есть → `dropUser` (`changed=true`);
`absent` + нет → no-op.

> **★★ Localhost-exception bootstrap** (mongo-механика, аналог redis
> `default_admin` bootstrap). `mongod` с `security.authorization: enabled`
> разрешает коннект **без auth** только через loopback (localhost) и только пока
> в admin-БД нет ни одного юзера. Первый admin (`default_admin`) создаётся именно
> так: коннект с auth ещё невозможен (юзера нет). Механика — **внутри плагина**
> (`user.go`), не в render: render передаёт `addr`+`username`+`password`, плагин
> решает auth-путь **по факту live-состояния** (параллель redis-плагину,
> решающему по `INFO`/`CONFIG GET`). При `present`: (1) пробует коннект с auth +
> дешёвый `usersInfo`-ping; (2) auth падает `Unauthorized`(13)/`AuthenticationFailed`(18)
> — это ожидаемо для первого admin → fallback на **no-auth** localhost-коннект;
> (3) `createUser` первого admin проходит по no-auth. Как только admin создан,
> exception закрывается — дальнейшие коннекты идут с auth. `absent`-путь fallback
> **не** делает (снятие юзера требует прав — это не bootstrap-случай).
> Output несёт `used_localhost`/`bootstrap_admin` (сработал ли no-auth путь).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес `mongod`: `host:port`. |
| `name` | string | required | Имя создаваемого/удаляемого MongoDB-юзера. |
| `state` | string | optional (default `present`) | `present` (создать, если нет) / `absent` (удалить, если есть). |
| `database` | string | optional (default `admin`) | БД, в которой заводится юзер (login/roles-контекст). Роли без явного `db` наследуют её. |
| `roles` | list | optional* | Роли юзера — массив `{role, db}` (**точная mongo-модель**: юзер = набор именованных ролей, каждая в конкретной БД). `db` без значения наследует `database`. *Обязателен непустым при `present` (юзер без ролей бессмыслен — проверка в Apply). |
| `password` | string (secret) | optional | Пароль **АДМИН-КОННЕКТА** (под `username` создаётся юзер). vault-ref, keeper резолвит до Apply. Уходит в коннект-кредлы, в события не попадает. ★ Это **не** пароль создаваемого юзера — кроме bootstrap первого admin, где admin создаёт сам себя. |
| `user_password` | string (secret) | optional | Пароль **СОЗДАВАЕМОГО** юзера (`pwd` документа `createUser`). vault-ref, keeper резолвит до Apply. Отделён от `password` (коннект-auth admin). Не задан → fallback на `password` (bootstrap первого admin). Маскируется. |
| `username` | string | optional | ACL-username AUTH-коннекта (админ, под которым создаётся юзер). При bootstrap первого admin auth ещё невозможен → localhost-exception. |
| `auth_db` | string | optional (default `admin`) | `authenticationDatabase` коннекта. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | TLS-параметры коннекта. PILOT: mongo в plain — не задаются. |

**Output**: `present` (bool) — состояние юзера после операции; `changed` (bool) —
был ли реальный create/drop; `used_localhost`/`bootstrap_admin` (bool) — сработал
ли no-auth localhost-путь (только на `present`).

## command — params

Raw `db.runCommand` к MongoDB (imperative verb-state, прецедент
`community.redis.command`/`core.exec.run`). По умолчанию `changed=false` (probe);
оператор отвечает за идемпотентность. Для pilot — single-field команды
(`{ serverStatus: 1 }`, `{ collStats: "events" }`).

> **WARNING (ИБ).** Вывод команды — это ответ mongo, **не** управляемый плагином
> секрет: масками [ADR-010](../../../adr/0010-templating.md) он **не** покрыт
> (Output несёт только флаг `ok`, но текст ошибки команды — ответ сервера). **Не**
> запускайте через `command` read-команды, возвращающие секреты (`usersInfo` с
> `showCredentials`) — их результат ушёл бы открытым текстом; для такого —
> специализированный state / `no_log`. Сам `params.password` при этом маскируется
> и в аргументы команды не попадает (уходит только в коннект).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес `mongod`: `host:port`. |
| `db` | string | optional (default `admin`) | Целевая БД для `runCommand`. |
| `command` | map | required | bson-документ команды (первый/единственный ключ — имя команды): `{ serverStatus: 1 }`. Для pilot — single-field. |
| `username` | string | optional | ACL-username для AUTH. |
| `password` | string (secret) | optional | Пароль MongoDB. vault-ref, keeper резолвит до Apply. Маскируется; в аргументы команды не передаётся (уходит в коннект). |
| `auth_db` | string | optional (default `admin`) | `authenticationDatabase`. |
| `changed` | bool | optional (default `false`) | Пометить результат `changed=true` (для реально мутирующих команд). По умолчанию `false` (probe-семантика). |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | TLS-параметры коннекта. PILOT: mongo в plain — не задаются. |

**Output**: `ok` (bool) — флаг успеха ответа (`{ ok: 1 }` → `true`).

## Пароль (ИБ-инвариант ADR-010)

Capability плагина — **только `network_outbound`**, `vault_access` **нет**:
пароль приходит уже зарезолвленным от Keeper. В operator-input (scenario/destiny)
пароль задаётся vault-ref-ом через CEL `${ vault(...) }`; keeper-side render-фаза
резолвит его **до** `Apply` и передаёт плагину plaintext-значение
([ADR-012](../../../adr/0012-keeper-soul-grpc.md) — Soul/плагин Vault-клиент не
тянет). В манифесте `password`/`user_password`/`tls_*` помечены `secret: true` +
`pattern: "^vault:.*"` — это форсирует vault-ref на входе и маскирование в
логах/трейсах/UI.

Код-инвариант (проверяется L0): ни `params.password`, ни `params.user_password`
**никогда** не попадают в `ApplyEvent.Message`/`.Output`, в текст ошибок и в
stderr. Пароль создаваемого юзера уходит только в `pwd`-поле документа
`createUser`; коннект-пароль — только в коннект-кредлы. Ошибки коннекта/команд
санитизируются (`redactError` в `helpers.go` вырезает подстроку каждого секрета —
для user-пути редактируются **оба**: `user_password` и коннект-`password`).
Пароль создаваемого юзера — в Vault по конвенции
`secret/mongo/<incarnation>/users/<name>#password` (симметрия redis/dragonfly).

## TLS-коннект (forward-compat; pilot — plain)

Все три state объявляют **общий** набор TLS-параметров коннекта
(`tls`/`tls_ca`/`tls_cert`/`tls_key`/`tls_skip_verify`), но **в pilot mongo
работает в plain-режиме** — эти параметры не задаются (mongo TLS на порту 27017
через `net.tls.mode` — отдельный слайс). Параметры объявлены для forward-compat.

| Параметр | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `tls` | bool | `false` | Коннектиться к `mongod` по TLS. |
| `tls_ca` | string (secret, PEM) | — | CA-сертификат для проверки серверного (RootCAs). |
| `tls_cert` | string (secret, PEM) | — | Client-сертификат для mTLS (опц., **только вместе** с `tls_key`). |
| `tls_key` | string (secret, PEM) | — | Client-ключ для mTLS (опц., **только вместе** с `tls_cert`). |
| `tls_skip_verify` | bool | `false` | **ЯВНЫЙ opt-out** проверки серверного сертификата (default secure). |

Модель безопасности (insecure = явный opt-out, default secure): при `tls: true`
плагин по умолчанию **проверяет** серверный сертификат; отключить — только явным
`tls_skip_verify: true`. PEM приходит **целиком** в params (резолв keeper-side из
Vault через `${ vault(...) }`), плагин свой Vault-доступ не тянет; маскинг — по
имени ключа (`shared/audit`).

## Capabilities / side-effects

- `required_capabilities: [network_outbound]` — TCP/TLS-коннект к `mongod`.
  **Без** `vault_access` (пароль и PEM резолвит Keeper), **без**
  `exec_subprocess` / `fs_write_root` (плагин не запускает подпроцессы — работа
  идёт через go-mongo-driver, не через `mongosh` — и не пишет на FS).
- `side_effects: [{ service: mongod }]` — все state работают над живым сервисом
  `mongod`.

## Пример вызова из scenario

```yaml
# Health-gate: дождаться, пока mongod ответит на ping ДО bootstrap admin.
- name: Wait for mongod to answer ping
  module: community.mongo.pinged
  retry:
    count: 15
    delay: 3s
    until: "register.self.ok == true"
  failed_when: "register.self.ok != true"
  params:
    addr: "127.0.0.1:27017"

# Bootstrap первого admin (default_admin) через localhost-exception:
# admin-БД пуста → плагин делает fallback на no-auth localhost-коннект.
- name: Bootstrap the default_admin user (localhost-exception)
  module: community.mongo.user
  params:
    addr:     "127.0.0.1:27017"
    username: default_admin
    # Пароль резолвится keeper-side через vault() в render-фазе (ADR-012):
    # в плагин уезжает уже значение, не ссылка.
    password: "${ vault('secret/mongo/' + incarnation.name + '/users/default_admin#password') }"
    name:     default_admin
    database: admin
    state:    present
    roles:    [{ role: root, db: admin }]
```

## Тесты

- **L0 dispatcher (pinged/command)**
  ([`impl_test.go`](../../../../examples/module/soul-mod-community-mongo/impl_test.go)):
  fake `mongoConn` + fake `ApplyEvent`-stream. `Validate` (пустой addr/command,
  неизвестный state), `pinged` happy-path (`Ping` → `Output.ok`, `changed=false`)
  и ошибка `Ping` → `failed`; `command` happy-path (`runCommand` → `ok`,
  `changed` из params); **ИБ-инвариант** — пароль не течёт в события / в
  санитизированную ошибку коннекта.
- **L0 user (localhost-exception)**
  ([`user_test.go`](../../../../examples/module/soul-mod-community-mongo/user_test.go)):
  fake `mongoConn`. `Validate` (addr+name, state ∈ {present, absent});
  идемпотентность (present+есть / absent+нет → no-op); create/drop
  (`changed=true`); **localhost-exception** (auth-проба падает
  `Unauthorized`/`AuthenticationFailed` → fallback на no-auth, `used_localhost`);
  разведение `password` (коннект) vs `user_password` (createUser-pwd);
  **ИБ-инвариант** (ни `password`, ни `user_password` не течёт).
- **L0 harness**
  ([`helpers_test.go`](../../../../examples/module/soul-mod-community-mongo/helpers_test.go)):
  общий тест-инвентарь для L0 (fake `mongoConn`, fake `ApplyEvent`-stream,
  `mustStruct`-билдер params, ассерт `assertEventsNoSecret` — проверка ИБ-инварианта
  «секрет не течёт в события», используемая в `impl_test`/`user_test`).
- **L1** (integration, testcontainers mongo) — следующий батч.

`GOWORK=off go test ./...`.

## Сборка

```sh
cd examples/module/soul-mod-community-mongo
GOWORK=off go build ./...   # бинарь soul-mod-community-mongo (gitignored)
GOWORK=off go test ./...    # L0
```

Модуль — отдельный go.mod с `replace` на core (`../../../proto/plugin`,
`../../../sdk`); собирается standalone, в `go.work` не входит (конвенция
`examples/module/`).

## См. также

- [README.md](../../README.md) — каталог модулей (статус каталога).
- [community/README.md](../README.md) — каталог community-плагинов.
- [examples/service/mongo/](../../../../examples/service/mongo/) — сервис mongo
  (PILOT, standalone): scenario `create` (install → render `mongod.conf` →
  sysctl → systemd → старт → bootstrap `default_admin` через localhost-exception
  → operator-юзеры) и `destroy` (Soul-side teardown) вызывают states этого
  плагина. Именованный тип [`MongoUser`](../../../../examples/service/mongo/types.yml)
  (`types.yml`) описывает элемент массива `input.users`.
- [ADR-012](../../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, пароль
  доезжает значением.
- [ADR-031 Scry](../../../adr/0031-scry-drift.md) — default-deny на dry_run без
  `PlanReadSafe`.
- [templating.md](../../../templating.md) — секрет-маскинг (§7.4).
