# redis — единый Redis-сервис (концепция Ansible-роли)

Сервис «redis» — **один сервис на все режимы развёртывания** Redis, по концепции
Ansible-роли Redis. Режим выбирается полем `redis_type`
(`standalone` / `sentinel` / `cluster` / `sentinel_only`), а не отдельным
сервисом на каждый стек.

Оператор задаёт **простые типизированные понятия** (сколько памяти, какая
persistence, какая eviction-политика, какие ACL-юзеры), а сервис **транслирует**
их в детальный `redis_config` (полный `redis.conf`). Трансляция — через CEL
`merge()`: дефолты автора + persistence-пресет + вычисленный `maxmemory` +
passthrough-директивы оператора, SHALLOW last-wins ([templating.md §2.3](../../../docs/templating.md)).

> **Состояние (PILOT, 2026-06-22).** Реализованы режимы **`standalone`** и
> **`cluster`** (honest hash-slot Redis Cluster, 16384 слота) — оба в scenario
> `create`, выбор через `redis_type`. Режимы `sentinel` / `sentinel_only`, day-2
> cluster-операции и day-2 в целом (см. [«В работе»](#в-работе)) — следующие батчи
> эпика redis-консолидации, **в этом сервисе их пока нет**. README описывает только
> то, что есть в файлах.

Разделение обязанностей (architect B-гибрид, ADR-009):

- **destiny [`redis`](../../destiny/redis/)** — режим-агностичный per-host кирпич:
  установка пакета `redis-server`, render `redis.conf` из **готового merged**-конфига,
  render `users.acl`, systemd-hardening drop-in, запуск сервиса. destiny «глупая» —
  ничего сама не сливает и не оркеструет; merge делает scenario сервиса.
- **scenario сервиса** — трансляция простого ввода в `redis_config` (через
  `merge()`) + оркестрация (порядок шагов, таргетинг, health-gate, размотка
  cluster-топологии в `nodes`-MAP; в будущих батчах — rolling-restart, day-2).
- **плагин [`community.redis`](../../../docs/module/community/redis/README.md)**
  (бинарь `soul-mod-community-redis`) — **основной интерфейс** к **живому** Redis
  (`CONFIG SET`, ACL, cluster, sentinel, failover, raw-команды). Подключён через
  [`service.yml → modules[]`](service.yml).

## state_schema

[`service.yml → state_schema`](service.yml) (`state_schema_version: 1`).
`incarnation.state` фиксирует, что развёрнуто, чтобы оператор видел инсталляцию, а
повторный apply был идемпотентен:

| поле | тип | смысл |
|---|---|---|
| `redis_type` | enum `standalone`/`sentinel`/`cluster`/`sentinel_only` | режим развёртывания (реализованы `standalone`/`cluster`) |
| `redis_version` | string | версия пакета `redis-server` (пусто → версия по умолчанию из репо дистрибутива) |
| `redis_config` | object | **итог трансляции** — merged-конфиг `redis.conf` (default → preset → вычисления → passthrough; для `cluster` — плюс `cluster-*`-директивы) |
| `redis_users` | map `username → {perms, state}` | ACL-пользователи Redis; `perms` — полная ACL-строка (пароли НЕ в state — keeper-side Vault) |
| `redis_hosts` | array `{sid, role}` | хосты топологии (пишется `[]`; точные роли `master`/`replica` для cluster раскладывает плагин — в state не фиксируются) |

`required: [redis_type, redis_config]`.

## Входной контракт оператора

[`scenario/create/main.yml → input:`](scenario/create/main.yml) — строго
типизированный структурный ввод (Named Dict, каждый параметр чёткого типа), **не**
свободный текст:

| поле | тип | смысл |
|---|---|---|
| `redis_type` | enum, default `standalone` | режим; обрабатываются `standalone` и `cluster` (выбирает ветку диспетчера) |
| `version` | string, опц. | distro-native версия пакета (напр. `5:7.0.15-1~deb12u7`); не задана → версия по умолчанию из репо |
| `memory_mb` | integer, опц., min `64` | бюджет памяти под Redis на хосте, МБ; `maxmemory` = доля от него |
| `persistence` | enum `off`/`aof`/`rdb`/`rdb_aof`, default `rdb` | режим durability; транслируется в `save`/`appendonly` |
| `maxmemory_policy` | enum eviction-политик | политика вытеснения при достижении `maxmemory` |
| `shards` | integer, опц., min `1` | **(cluster)** число master-шардов; 16384 hash-слота делятся поровну между мастерами |
| `replicas_per_shard` | integer, опц., min `0` | **(cluster)** реплик на один master-шард; `0` — кластер без реплик |
| `cluster_node_timeout` | integer, опц., default `5000`, min `1` | **(cluster)** таймаут gossip между нодами, мс (директива `cluster-node-timeout`) |
| `users` | map `имя → {perms, state}` | ACL-юзеры; `perms` — полная ACL-строка Redis, `state` ∈ `on`/`off` |
| `redis_settings` | object (passthrough) | произвольные директивы `redis.conf` key→value; **бьют всё** в итоговом merge |

Топология cluster задаётся **счётчиками** (`shards` + `replicas_per_shard`),
а не списком хостов: сценарий проверяет, что число таргетированных souls roster-а
ровно `shards * (1 + replicas_per_shard)` (size-guard, см.
[«`create` (режим cluster)»](#create-режим-cluster)), и раскладывает роли по этим
хостам сам. cloud-создание машин под топологию — post-бета.

Пароли **не во входном контракте** — лежат в Vault, резолвятся keeper-side (см.
[«Безопасность»](#безопасность)).

### Пример простого ввода — standalone (как в L0-кейсе)

Из [`create/tests/full-stack/case.yml`](scenario/create/tests/full-stack/case.yml):

```yaml
input:
  redis_type: standalone
  version: "5:7.0.15-1~deb12u7"
  memory_mb: 1024
  persistence: rdb
  maxmemory_policy: volatile-lru
  users:
    app:
      perms: "~app:* +@read +@write -@dangerous"
      state: "on"
  redis_settings:
    timeout: 60
    tcp-backlog: 511
```

### Пример простого ввода — cluster (как в L0-кейсе)

Из [`create/tests/cluster-create-2shards-1replica/case.yml`](scenario/create/tests/cluster-create-2shards-1replica/case.yml)
— `2 * (1 + 1) = 4` хоста в roster-е прогона:

```yaml
input:
  redis_type: cluster
  shards: 2
  replicas_per_shard: 1
  cluster_node_timeout: 8000
```

Оператор объявляет **только счётчики**: 2 шарда по 1 реплике. Раскладку ролей
master/replica и hash-слотов делает плагин; оператор не перечисляет хосты
поимённо — они берутся из таргетированного roster-а прогона.

## Трансляция простого ввода в redis_config

Сервис сливает четыре слоя через `merge()` (SHALLOW last-wins, слева направо —
правый бьёт левый по ключу верхнего уровня):

1. `essence.redis_config` — дефолты автора (defaults роли);
2. `essence.persistence_presets[persistence]` — `save`/`appendonly` выбранного режима;
3. вычисленные `maxmemory` + `maxmemory-policy` — производные от `memory_mb` / input;
4. `input.redis_settings` — passthrough оператора (перекрывает всё).

Данные-таблицы трансляции живут в [`essence/_default.yaml`](essence/_default.yaml):
`persistence_presets`, `memory_reserve_percent`, merge-подложка `redis_config`.

Трансляция на одном поле:

- **`persistence: rdb`** → `essence.persistence_presets["rdb"]` =
  `{save: "900 1 300 10 60 10000", appendonly: "no"}` — RDB-снапшоты включены, AOF выключен.
- **`memory_mb: 1024`** → `maxmemory = 1024 * memory_reserve_percent(75) / 100 = 768`
  → директива `maxmemory: "768mb"` (резерв 25% — под ОС/накладные). Не задан →
  `maxmemory` берётся из merge-подложки `essence.redis_config`.

Слой 3 материализует только заданные ключи (has-guard): пустой `memory_mb` не
уедет строкой `"0mb"`, отсутствующий `maxmemory_policy` не затрёт дефолт/пресет.

## Сценарии

### `create` (единый вход, диспетчер по `redis_type`)

Оператор всегда вызывает **`create`**; режим выбирает поле `redis_type`.
[`scenario/create/main.yml`](scenario/create/main.yml) — **диспетчер**: тело
прогона вынесено в две ветки, подключаемые через `include:` —
[`standalone.yml`](scenario/create/standalone.yml) и
[`cluster.yml`](scenario/create/cluster.yml). `include:` раскрывается **безусловно**
в плоский список ДО render (`when:` на самой include-задаче запрещён), поэтому
ветвление делает не include, а `when:` по `input.redis_type` на каждой задаче ветки:
предикат статичен (только `input.*`), Keeper вычисляет его at-render и гасит
неактивную ветку placeholder-skip-ом (ADR-012(d) Вариант b). Порядок include —
standalone, потом cluster: standalone-задачи остаются на прежних индексах плана,
cluster-placeholder-ы добавляются в хвост.

После успешного apply Keeper фиксирует `state_changes` (ADR-009 §7.1, ADR-057):
`redis_type`, `redis_version`, `redis_config` (тот же `merge()`, что ушёл в render —
единый источник истины; для cluster — плюс `cluster-*`-директивы, кроме host-
вариативного `cluster-announce-ip`, который в state не пишется), `redis_users`
(из `input.users`), `redis_hosts = []`.

#### `create` (режим standalone)

[`scenario/create/standalone.yml`](scenario/create/standalone.yml) — два шага:

1. **`apply: destiny: redis`** — per-host install `redis-server` + render
   **полного** `redis.conf` (из готового merged `config`) + render `users.acl`
   (полные ACL-строки, пароли хешами) + systemd-hardening drop-in + запуск
   сервиса. Конфиг приходит **готовым** merged-map-ом через `apply.input.config` —
   destiny сама ничего не сливает. ACL-юзеры передаются **списком** объектов
   `{name, perms, state, password}` (CEL `.map(...)` над map оператора даёт список).
   Изолированный render-проход destiny (ADR-009): внутри видно только переданный
   `apply.input`.
2. **`module: community.redis.command` (`PING`)** — health-gate после старта:
   raw-команда к живому Redis (`addr: 127.0.0.1:6379`, `retry`), демонстрация
   плагина. **Конфиг при create НЕ пишется в живой инстанс** — на create источник
   истины это render `redis.conf` (at-create). Live-синхронизация (`CONFIG SET`
   дельты) — это day-2 `update_config`, см. [«В работе»](#в-работе).

#### `create` (режим cluster)

[`scenario/create/cluster.yml`](scenario/create/cluster.yml) — honest hash-slot
Redis Cluster (16384 слота). Четыре шага:

1. **size-guard** (`core.cmd.shell`, `run_once`, fail-fast ДО любого install) —
   проверяет, что число таргетированных хостов roster-а ровно
   `shards * (1 + replicas_per_shard)`. Соответствие вычисляется **keeper-side при
   render** (там доступен `soulprint.hosts` — roster прогона); отрендеренное булево
   встраивается в guard-команду: совпало → `test "true" = "true"` (no-op), не
   совпало → `test "false" = "true"` → `echo ...; exit 1` рвёт прогон с понятным
   сообщением `topology mismatch`. `changed_when: false` — это проверка, не изменение.
2. **`apply: destiny: redis`** — та же режим-агностичная destiny, что и в
   standalone, но merged `config` **дополнен cluster-директивами** поверх базового
   merge: `cluster-enabled: yes`, `cluster-config-file: nodes.conf`,
   `cluster-node-timeout` (из input или дефолт `5000`) — все три host-инвариантны и
   корректно идут через config-merge (`apply.input` резолвится один раз на первом по
   SID хосте). `cluster-announce-ip` рендерится **per-host** в `redis.conf.tmpl` из
   `.self.network.primary_ip` (IP **этого** хоста, критично за NAT/в облаке), под
   гейтом `cluster-enabled` — НЕ через merged config: он host-инвариантен (как
   `bind`), и проброс через config-map зафиксировал бы IP первой ноды для всех.
3. **health-gate PING** (`community.redis.command`, `retry`) — каждая нода обязана
   ответить `PING` ДО сборки кластера.
4. **cluster-build** (`community.redis.cluster`, `action: create`, `run_once` на
   bootstrap-ноде) — собирает кластер. Сценарий строит детерминированный `nodes`-MAP
   из roster-а прогона (`soulprint.hosts`): ключ = `SID` (стабилен и сортируем),
   значение = `{addr: "<primary_ip>:6379"}`, и передаёт его плагину вместе с
   `replicas_per_shard`. Сам `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE` (через go-redis) и
   деление 16384 слотов делает **плагин** детерминированно по сортировке ключей —
   scenario топологию НЕ транслирует, передаёт готовую (иначе две раскладки
   рассинхронятся). Состояние state плагина — в его per-module doc
   [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).

## Безопасность

Пароли — **из Vault**, не во входном контракте сценария. Сценарий читает их
keeper-side CEL-функцией `vault(...)` в render-фазе (templating.md §2.3/§4) по
конвенции:

- requirepass: `secret/redis/<incarnation.name>#password`;
- per-user: `secret/redis/<incarnation.name>/users/<name>#password`.

Путь строится из доверенного контекста (incarnation, не operator-input). В destiny
и в плагин через `apply.input` / `params` уходит уже **зарезолвленное значение** —
пароль доезжает на хост значением, а не ссылкой; Soul vault-клиент не тянет
(ADR-012). В git нет ни значения, ни operator-указателя на секрет. В `users.acl`
пароль пишется **хешем** (`#<sha256>`), plaintext в файл не попадает. Плагин
`community.redis` не логирует `params["password"]` (ADR-010).

## Прогон L0

L0-испытание (Trial, ADR-023), render-only, герметично — из каталога `keeper/`:

```sh
# standalone
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/full-stack/case.yml
# cluster
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/cluster-create-3shards/case.yml
```

[Кейс `create/tests/full-stack`](scenario/create/tests/full-stack/case.yml)
проверяет план standalone: задачи destiny `redis` (install + render `users.acl` +
render `redis.conf` + systemd hardening drop-in + daemon-reload + running +
restarted) + задача `community.redis.command` (`PING`). Главный guard слайса — что
вычисленный `maxmemory` (`768mb`), persistence-пресет, last-wins на конфликтующем
`timeout`, `maxmemory-policy` из input и passthrough `tcp-backlog` правильно
сложились в merged `redis.conf`, а `users.acl` отрендерен с полной ACL-строкой
оператора. Vault мокается `fixtures.vault` → в плане уже **значения** паролей
(регресс на keeper-side `vault()`-резолв). destiny `redis` резолвится зеркалом прода
через `fixtures.default_destiny_source` (`file://../../destiny/{name}`, путь
относительно service-root).

Cluster-кейсы под [`scenario/create/tests/`](scenario/create/tests/):

- [`cluster-create-3shards`](scenario/create/tests/cluster-create-3shards/case.yml)
  — `shards=3`, `replicas_per_shard=0` (3 хоста, size-guard PASS): проверяет
  cluster-директивы в render `redis.conf` (`cluster-enabled`/`cluster-config-file`/
  `cluster-node-timeout`/`cluster-announce-ip`), детерминированный `nodes`-MAP по
  SID и наличие `community.redis.cluster` (`action: create`) в плане; standalone-
  ветка погашена placeholder-skip-ом.
- [`cluster-create-2shards-1replica`](scenario/create/tests/cluster-create-2shards-1replica/case.yml)
  — `shards=2`, `replicas_per_shard=1` (4 хоста): non-zero replicas в size-guard и в
  `nodes`-MAP; `cluster_node_timeout` из input (`8000`, не дефолт).
- [`cluster-size-guard-mismatch`](scenario/create/tests/cluster-size-guard-mismatch/case.yml)
  — `shards=3` против 4 хостов: FAIL-ветка size-guard (отрендеренное
  `test "false" = "true"` → fail-fast).

В живом Keeper service + destiny резолвятся как git-репо по ref (ADR-007/009).

## В работе

Следующие батчи эпика redis-консолидации (в этом сервисе **пока не реализованы**):

- режимы `sentinel`, `sentinel_only` (топология + плагинные states);
- day-2 cluster-операции: add-node / remove-node / reshard (плагин
  `community.redis.cluster` сейчас умеет только `action: create`);
- day-2 в целом: `update_config` (live `CONFIG SET` дельты), `add_user`, реплики,
  failover, безопасный rolling-restart;
- плагинные states `community.redis`: `acl` / `replica` / `sentinel` (`command` /
  `config` / `cluster` уже есть);
- TLS и загрузка Redis-модулей.

Состояние плагина `community.redis` (какие states реализованы) — в его per-module
doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).
