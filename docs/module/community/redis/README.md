# community.redis

ОСНОВНОЙ интерфейс к **живому Redis** в redis-консолидации (концепция
Ansible-роли): scenario сервиса оркеструет порядок/таргетинг/rolling, а плагин
исполняет **одну** операцию над одним Redis-инстансом. Custom-плагин
`kind: soul_module` (namespace `community`, name `redis`), бинарь
`soul-mod-community-redis`. Реализация —
[`examples/module/soul-mod-community-redis/`](../../../../examples/module/soul-mod-community-redis/)
(`impl.go` — диспетчер + states command/config, `cluster.go` — cluster-state,
`helpers.go` — structpb/secret-helpers).

Backend — [`github.com/redis/go-redis/v9`](https://github.com/redis/go-redis):
плагин сам коннектится к Redis по TCP (`host:port`) или unix-сокету
(`unix:/path`). Соответствие core-модулям (`core.pkg`/`core.file`/`core.service`/
`core.sysctl`) — для всего, что НЕ redis-специфично (установка, render
`redis.conf`, systemd); сам Redis-рантайм — этот плагин.

## Без dry-run preview (осознанно)

Плагин **не** реализует `PlanReadSafe`
([ADR-031](../../../adr/0031-scry-drift.md)) и `ErrandReadSafe`
([ADR-033](../../../adr/0033-errand.md)): он остаётся на
`module.BaseModule`. Это сознательный выбор (решение пользователя 2026-06-22):
на `dry_run` host (Soul) применяет **default-deny** — задача получает честный
«предпросмотр не поддержан», а не ложное «нет дрифта». Лучше явный отказ, чем
тихое clean от no-op Plan.

## States

Текущий срез — три state. Остальные **планируются** следующими батчами и пока в
`manifest.yaml::spec.states` **отсутствуют**.

| State | Назначение | `changed` |
|---|---|---|
| `command` | Raw-команда к Redis (imperative verb-state, прецедент `core.cmd.shell`/`core.exec.run`/`core.http.probe`). | `false` по умолчанию; `changed: true` в params — для реально мутирующих команд. |
| `config` | Применить map директив `redis.conf` через `CONFIG SET` (+ опц. `CONFIG REWRITE`). | `true` при ≥1 применённой директиве. |
| `cluster` | Собрать hash-slot-кластер (16384 слота) через `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE`. Сейчас `action: create` (add-node/remove-node/reshard **планируются**). | `true` при сборке; `false` (no-op), если кластер уже сформирован. |
| `acl` *(планируется)* | Idempotent ACL-user (`SETUSER`/`DELUSER`). | — |
| `replica` *(планируется)* | `REPLICAOF` / промоут реплики. | — |
| `sentinel` *(планируется)* | `SENTINEL MONITOR`/`SET`. | — |
| `failover` *(планируется)* | Switchover (вливание старого `soul-mod-redis-failover`). | — |

## command — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis: `host:port` (TCP) или `unix:/path` (unix-сокет). |
| `args` | list | required | Команда массивом аргументов **без shell**: `["CONFIG","SET","maxmemory","256mb"]`. Первый элемент — verb. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref в operator-input, keeper резолвит до Apply (см. «Пароль»). Маскируется. |
| `username` | string | optional | ACL-username для `AUTH` (если не default-user). |
| `db` | int | optional (default `0`) | Номер БД (`SELECT`) перед командой. |
| `changed` | bool | optional (default `false`) | Пометить результат `changed=true` (probe-семантика по умолчанию). |

## config — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis (как у `command`). |
| `config` | map | required | Директивы `redis.conf`: `{ maxmemory: "256mb", maxmemory-policy: allkeys-lru }`. Каждая → `CONFIG SET <key> <value>`. Применяются в детерминированном порядке (по ключу). Числовые значения стрингифицируются (`20000`, не `20000.000000`). |
| `password` | string (secret) | optional | См. «Пароль». |
| `username` | string | optional | ACL-username для `AUTH`. |
| `rewrite` | bool | optional (default `false`) | После `CONFIG SET` выполнить `CONFIG REWRITE` (персист в `redis.conf`). |

## cluster — params

Собирает Redis-кластер из набора nodes **целиком через go-redis** (никакого
`redis-cli`/shell): `CLUSTER MEET` (gossip) → `CLUSTER ADDSLOTS` мастерам →
`CLUSTER REPLICATE` репликам. Идемпотентен — повторный вызов на уже
сформированном кластере (`cluster_state:ok`, состав совпал, 16384 слота
покрыты) даёт `changed=false`, no-op.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `action` | string | required | Операция. Сейчас поддержано **только `create`**. `add-node`/`remove-node`/`reshard` — планируются. |
| `nodes` | map | required | Узлы: map стабильный-ключ (SID/имя) → `{ addr: "host:port" }` либо `{ ip: "10.0.0.1", port: 6379 }`. **Ключи сортируются** — детерминируют раскладку master/replica. `addr` — для коннекта, `ip`+`port` — для `CLUSTER MEET` (gossip оперирует `ip:port`, не DNS-именем). |
| `replicas_per_shard` | int | optional (default `0`) | Реплик на шард. `shards = len(nodes) / (1 + replicas_per_shard)`; `len(nodes)` обязан делиться на размер шарда без остатка. |
| `password` | string (secret) | optional | См. «Пароль». Применяется при коннекте к **каждой** ноде. |
| `username` | string | optional | ACL-username для `AUTH`. |

**Детерминированная раскладка.** Ключи `nodes` сортируются; первые `shards`
нод — мастера, остальные — реплики round-robin к мастерам (`replica j →
master j%shards`). 16384 слота делятся поровну между мастерами; остаток
(`16384 % shards`) распределяется по одному слоту первым мастерам. Один и тот
же вход `nodes` всегда даёт одну и ту же топологию и одни и те же диапазоны
слотов.

**Сходимость gossip** — ограниченный retry (не бесконечный цикл): после `MEET`
плагин ждёт, пока `CLUSTER NODES` покажет все ноды, и лишь затем шлёт
`ADDSLOTS`/`REPLICATE`. Если за лимит не сошлось — `failed`.

## Пароль (ИБ-инвариант ADR-010)

Capability плагина — **только `network_outbound`**, `vault_access` **нет**:
пароль приходит уже зарезолвленным от Keeper. В operator-input
(scenario/destiny) пароль задаётся vault-ref-ом через CEL `${ vault(...) }`;
keeper-side render-фаза резолвит его **до** `Apply` и передаёт плагину
plaintext-значение (ADR-012 — Soul/плагин Vault-клиент не тянет). В манифесте
`password` помечен `secret: true` + `pattern: "^vault:.*"` — это форсирует
vault-ref на входе и маскирование в логах/трейсах/UI.

Код-инвариант (проверяется L0): `params["password"]` **никогда** не попадает в
`ApplyEvent.Message`/`.Output`, в текст ошибок и в stderr. Ошибки коннекта
санитизируются (`redactError` вырезает подстроку пароля); вывод самих команд
(`result`) — это ответ сервера, не секрет оператора.

## Capabilities / side-effects

- `required_capabilities: [network_outbound]` — TCP/unix-коннект к Redis (для
  `cluster` — коннект к каждой ноде из `nodes`). **Без** `vault_access` (пароль
  резолвит Keeper), **без** `exec_subprocess` / `fs_write_root` (плагин не
  запускает подпроцессы — `cluster` идёт целиком через go-redis, не через
  `redis-cli` — и не пишет на FS).
- `side_effects: [{ service: redis-server }]` — все state работают над живым
  сервисом redis.

## Пример вызова из scenario

```yaml
# Применить итоговый redis_config к живому Redis после render redis.conf destiny-ем.
- name: Apply redis runtime config
  module: community.redis.config
  params:
    addr: "127.0.0.1:6379"
    # Пароль резолвится keeper-side через vault() в render-фазе (ADR-012):
    # в плагин уезжает уже значение, не ссылка.
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
    config: "${ state.redis_config }"

# Raw-команда (probe): changed=false по умолчанию.
- name: Ping redis
  module: community.redis.command
  register: pong
  params:
    addr: "127.0.0.1:6379"
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
    args: ["PING"]
```

## Тесты

- **L0 command/config**
  ([`impl_test.go`](../../../../examples/module/soul-mod-community-redis/impl_test.go)):
  fake `redisConn` + fake `ApplyEvent`-stream. Покрывает `Validate` (пустой
  addr/args/config, нереализованный state), Apply happy-path command/config,
  unix-socket-парсинг, `changed`-семантику, стрингификацию числовых значений,
  `CONFIG REWRITE`, и **ИБ-инвариант** — пароль не утекает ни в события, ни в
  аргументы команд, ни в санитизированную ошибку коннекта.
- **L0 cluster**
  ([`cluster_test.go`](../../../../examples/module/soul-mod-community-redis/cluster_test.go)):
  fake-флот нод по addr. Покрывает `Validate` (пустой `nodes`, не-`create`
  action, недели́мый состав, отрицательный `replicas_per_shard`); happy create
  (`MEET`/`ADDSLOTS`/`REPLICATE` с правильными аргументами, полное покрытие
  16384 слотов, роли детерминированы); already-formed → `changed=false`, no-op;
  детерминизм раскладки на нескольких прогонах; раскладка ролей по сортировке
  ключей; деление слотов с остатком; **ИБ-инвариант** (пароль не течёт в
  события/команды/ошибку коннекта).
- **L1** (integration, testcontainers redis) — следующий батч.

`GOWORK=off go test ./...`.

## Сборка

```sh
cd examples/module/soul-mod-community-redis
GOWORK=off go build ./...   # бинарь soul-mod-community-redis (gitignored)
GOWORK=off go test ./...    # L0
```

Модуль — отдельный go.mod с `replace` на core (`../../../proto/plugin`,
`../../../sdk`); собирается standalone, в `go.work` не входит (конвенция
`examples/module/`).

## См. также

- [README.md](../../README.md) — каталог модулей (статус каталога).
- [examples/service/redis/](../../../../examples/service/redis/) — сервис redis,
  scenario `create` (standalone) вызывает `community.redis.config`.
- [examples/destiny/redis/](../../../../examples/destiny/redis/) —
  режим-агностичный per-host кирпич (install + render `redis.conf` + systemd).
- [ADR-012](../../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, пароль
  доезжает значением.
- [ADR-031 Scry](../../../adr/0031-scry-drift.md) — default-deny на dry_run без
  `PlanReadSafe`.
- [templating.md](../../../templating.md) — секрет-маскинг (§7.4).
