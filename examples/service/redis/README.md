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

> **Состояние (2026-06-23).** Реализованы **все четыре** режима: **`standalone`**,
> **`cluster`** (honest hash-slot Redis Cluster, 16384 слота), **`sentinel`**
> (master-replica + sentinel-демон) и **`sentinel_only`** (тонкий sentinel-слой:
> только sentinel-демон без локального redis-server, мониторит **внешний** master из
> `input.master_ip`) — все четыре в scenario `create`, выбор через `redis_type`
> ([`sentinel.yml`](scenario/create/sentinel.yml) и
> [`sentinel-only.yml`](scenario/create/sentinel-only.yml) включены в диспетчер).
> Реализована и day-2 операция cluster — присоединение/вывод ноды через отдельные
> scenario [`add_node`](scenario/add_node/main.yml) /
> [`remove_node`](scenario/remove_node/main.yml). Mode-guard первой задачей диспетчера
> рвёт прогон с понятным сообщением только при значении вне enum (защита от footgun
> «нереализованный режим = тихий зелёный no-op»). Остальной backlog (cluster day-2
> reshard, sentinel failover/day-2, sentinel-демон TLS) — см. [«В работе»](#в-работе).
> README описывает только то, что есть в файлах.

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
| `redis_type` | enum `standalone`/`sentinel`/`cluster`/`sentinel_only` | режим развёртывания (реализованы все четыре) |
| `redis_version` | string | версия пакета `redis-server` — distro-native пин (обязателен, см. input `version`) |
| `redis_config` | object | **итог трансляции** — merged-конфиг `redis.conf` (default → preset → вычисления → passthrough; для `cluster` — плюс `cluster-*`-директивы). В `sentinel_only` — пустой `{}` (data-плоскости redis-server нет, `redis.conf` не рендерится; `required`-ключ присутствует) |
| `redis_users` | map `username → {perms, state}` | ACL-пользователи Redis; `perms` — полная ACL-строка (пароли НЕ в state — keeper-side Vault) |
| `redis_hosts` | array `{sid, role}` | хосты топологии (пишется `[]`; точные роли `master`/`replica`/`sentinel` для cluster/sentinel раскладывает apply-сторона — в state не фиксируются) |
| `redis_sentinel` | object `{master_name, quorum}` (+ `master_ip` в `sentinel_only`) | факты sentinel-режимов (`sentinel` и `sentinel_only`): имя monitored master + quorum; `quorum` из input или `0` (auto `size/2+1` вычисляется в apply, в state не материализуется). В `sentinel_only` дополнительно пишется `master_ip` — адрес **внешнего** master-а, заданный оператором (в `sentinel` master выбирается из roster-а и host-вариативен → в state не материализуется). Вне sentinel-режимов — пустой объект |

`required: [redis_type, redis_config]`.

## Входной контракт оператора

[`scenario/create/main.yml → input:`](scenario/create/main.yml) — строго
типизированный структурный ввод (Named Dict, каждый параметр чёткого типа), **не**
свободный текст:

| поле | тип | смысл |
|---|---|---|
| `redis_type` | enum, default `standalone` | режим; выбирает ветку диспетчера; реализованы все четыре (`standalone`/`cluster`/`sentinel`/`sentinel_only`). Значение вне enum ловит mode-guard первой задачей |
| `version` | string, **обяз.** | distro-native пин версии пакета (напр. `5:7.0.15-1~deb12u7`); `core.pkg` всегда ставит `=version` — воспроизводимая инсталляция. Поведение «не задана → latest из репо» удалено (директива пользователя 2026-06-23) |
| `memory_mb` | integer, опц., min `64` | бюджет памяти под Redis на хосте, МБ; `maxmemory` = доля от него |
| `persistence` | enum `off`/`aof`/`rdb`/`rdb_aof`, default `rdb` | режим durability; транслируется в `save`/`appendonly` |
| `maxmemory_policy` | enum eviction-политик | политика вытеснения при достижении `maxmemory` |
| `shards` | integer, обяз. при `cluster` (`required_when`), min `1` | **(cluster)** число master-шардов; 16384 hash-слота делятся поровну между мастерами. Обязателен при `redis_type=cluster` через `required_when: "input.redis_type == 'cluster'"` — пропуск рвёт input-валидацию ДО рендера |
| `replicas_per_shard` | integer, опц., min `0` | **(cluster)** реплик на один master-шард; `0` — кластер без реплик |
| `cluster_node_timeout` | integer, опц., default `5000`, min `1` | **(cluster)** таймаут gossip между нодами, мс (директива `cluster-node-timeout`) |
| `replicas` | integer, опц., default `0`, min `0` | **(sentinel)** реплик master-а; `0` — только master; roster обязан быть `1 + replicas` |
| `sentinel_quorum` | integer, опц., min `1` | **(sentinel)** quorum sentinel-ов для признания master down; не задан → `size(hosts)/2+1` (большинство) |
| `sentinel_master_name` | string, опц., default `mymaster` | **(sentinel/sentinel_only)** логическое имя monitored master в Sentinel (`SENTINEL MONITOR`, `sentinel.conf`) |
| `master_ip` | string, обяз. при `sentinel_only` (`required_when`) | **(sentinel_only)** IP/адрес **внешнего** redis-master, который мониторит sentinel-кластер. Обязателен при `sentinel_only` через `required_when: "input.redis_type == 'sentinel_only'"` — пропуск рвёт input-валидацию ДО рендера (понятный отказ, прогон не стартует), а не shell-guard на хосте. В прочих режимах не используется (master выбирается из roster-а) |
| `master_port` | integer, опц., default `6379`, min `1` | **(sentinel_only)** порт внешнего redis-master; уходит в `sentinel.conf` и в `community.redis.sentinel` (`monitor.port`) |
| `users` | map `имя → {perms, state}` | ACL-юзеры; `perms` — полная ACL-строка Redis, `state` ∈ `on`/`off` |
| `redis_settings` | object (passthrough) | произвольные директивы `redis.conf` key→value; **бьют всё** в итоговом merge |
| `modules` | array enum `search`/`json`/`timeseries`/`bloom`, опц. | **(Redis-модули)** алиасы загружаемых модулей (RediSearch/RedisJSON/RedisTimeSeries/RedisBloom) для Redis < 8; на Redis 8+ игнорируется (модули встроены). Пусто/не задан → модули не подключаются |
| `modules_base_url` | string, обяз. при непустом `modules` | **(Redis-модули)** базовый URL источника `.so` (без арх-сегмента); destiny строит полный URL per-host как `<base_url>/<arch>/<имя.so>` |
| `modules_sha256` | map `алиас → SHA-256` (голый hex), обяз. для каждого `modules` | **(Redis-модули)** контрольная сумма каждого `.so` (fail-closed: `core.url.fetched` верифицирует, mismatch → падение) |

Параметров `logrotate_enable` / `sysctl_enable` / `thp_disable` во входном контракте
**нет**: отключение Transparent Huge Pages, logrotate-конфиг и sysctl-тюнинг — **безусловный
baseline** (рекомендация Redis / hardening, выровнено по sysctl-блоку Ansible-роли), а не
операторский выбор. Эти задачи всегда активны и приходят в destiny через `apply.input`.
Подробнее — [«Host-tuning extras»](#host-tuning-extras).

Топология cluster задаётся **счётчиками** (`shards` + `replicas_per_shard`),
а не списком хостов: сценарий проверяет, что число таргетированных souls roster-а
ровно `shards * (1 + replicas_per_shard)` (size-guard, см.
[«`create` (режим cluster)»](#create-режим-cluster)), и раскладывает роли по этим
хостам сам. Топология sentinel задаётся аналогично — счётчиком `replicas`
(roster `1 + replicas`, size-guard, см. [«`create` (режим sentinel)»](#create-режим-sentinel)).
Топология `sentinel_only` size-guard НЕ имеет: master внешний (`input.master_ip`), а
sentinel-демон поднимается на каждом таргетированном хосте — любое их число валидно
(quorum считается от количества). cloud-создание машин под топологию — post-бета.

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

### Пример простого ввода — sentinel (как в L0-кейсе)

Из [`create/tests/sentinel-create-1master-2replica/case.yml`](scenario/create/tests/sentinel-create-1master-2replica/case.yml)
— `1 + replicas = 1 + 2 = 3` хоста в roster-е прогона:

```yaml
input:
  redis_type: sentinel
  replicas: 2
  sentinel_quorum: 2
  sentinel_master_name: mymaster
```

Оператор объявляет **только счётчик** `replicas` (1 master + 2 реплики).
Master-election (кто master) — первый по SID хост; раскладку ролей и привязку
реплик/sentinel-ов делает apply-сторона. `sentinel_quorum` не задан → берётся
`size(hosts)/2+1` (большинство).

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

## Host-tuning extras

Режим-агностичные per-host добавки, общие для всех режимов — реализованы в
[`destiny/redis/tasks/extras.yml`](../../destiny/redis/tasks/extras.yml). Все три —
**безусловный baseline** (рекомендация Redis / hardening, не операторский выбор):
operator-флагов нет, задачи рендерятся **всегда**.

- **THP — безусловно.** Отключение Transparent Huge Pages — drop-in hardening
  (oneshot systemd-юнит `disable-thp.service` + `core.service.running enabled`),
  рендерится всегда. Это рекомендация Redis (THP даёт latency-спайки на fork
  RDB/AOF), а не операторский выбор — поэтому параметра `thp_disable` во входном
  контракте **нет**.
- **logrotate — безусловно.** Конфиг `/etc/logrotate.d/redis` (ротация
  `/var/log/redis/*.log`, `copytruncate`), рендерится всегда. Прежнего флага
  `logrotate_enable` (opt-out) больше **нет** — задание `logrotate_enable: false`
  отклонит input-валидация Keeper-а как `unknown_key`.
- **sysctl — безусловно.** Один шаг `core.sysctl.applied` (state `applied`):
  модуль сам строит детерминированный drop-in `/etc/sysctl.d/30-redis.conf` из
  map (sorted keys) и реактивно перечитывает его `sysctl -e -p <file>` (точечно по
  drop-in, НЕ весь `--system`; `-e` глушит read-only/несуществующие ключи в
  контейнерах, не валя прогон). Применяется всегда. Набор и значения kernel-
  параметров **выровнены 1:1 по sysctl-блоку Ansible-роли redis** (память/fork-
  overcommit, swappiness, сетевые буферы, бэклоги, TCP-стек); источник значений —
  данные-таблица [`essence/_default.yaml → sysctl_settings`](essence/_default.yaml)
  (не во входе оператора — tuning под Redis, не операционный выбор). Прежнего флага
  `sysctl_enable` (opt-out) больше **нет**. Блок `tcp_bbr` роли НЕ перенесён
  (зависит от модуля ядра `tcp_bbr`, на Debian не загружен по умолчанию) — отложен.

Все три набора значений host-инвариантны → приходят в destiny через `apply.input`.

## Сценарии

### `create` (единый вход, диспетчер по `redis_type`)

Оператор всегда вызывает **`create`**; режим выбирает поле `redis_type`.
[`scenario/create/main.yml`](scenario/create/main.yml) — **диспетчер**: тело
прогона вынесено в четыре ветки, подключаемые через `include:` —
[`standalone.yml`](scenario/create/standalone.yml),
[`cluster.yml`](scenario/create/cluster.yml),
[`sentinel.yml`](scenario/create/sentinel.yml) и
[`sentinel-only.yml`](scenario/create/sentinel-only.yml). `include:` раскрывается
**безусловно** в плоский список ДО render (`when:` на самой include-задаче запрещён),
поэтому ветвление делает не include, а `when:` по `input.redis_type` на каждой задаче
ветки: предикат статичен (только `input.*`), Keeper вычисляет его at-render и гасит
неактивные ветки placeholder-skip-ом (ADR-012(d) Вариант b). Порядок include —
standalone, cluster, sentinel, sentinel_only: ветки добавляются в хвост, индексы
предыдущих веток не сдвигаются.

**Mode-guard первой задачей.** Перед ветками диспетчер ставит задачу `core.cmd.shell`
(`run_once`, без `when:` — рендерится при любом `redis_type`), которая ловит footgun
«нереализованный режим = тихий зелёный no-op»: ветки gated на свой `redis_type`,
поэтому при значении **вне** enum `{standalone, cluster, sentinel, sentinel_only}`
**все** ветки гаснут placeholder-skip-ом и `create` прошёл бы зелёным, ничего не
развернув. Guard вычисляет keeper-side булево «режим реализован» (перечень
mode-литералов синхронен enum `input.redis_type`): реализован → no-op; нет → `echo` с
конкретным `redis_type` + `exit 1` рвут прогон fail-fast. Поскольку все четыре режима
теперь реализованы, на любом легальном `redis_type` mode-guard проходит no-op-ом —
он отвергает только реально-неизвестные значения.

После успешного apply Keeper фиксирует `state_changes` (ADR-009 §7.1, ADR-057):
`redis_type`, `redis_version`, `redis_config` (тот же `merge()`, что ушёл в render —
единый источник истины; для cluster — плюс `cluster-*`-директивы, кроме host-
вариативного `cluster-announce-ip`, который в state не пишется; для `sentinel_only` —
пустой `{}`, data-плоскости нет), `redis_users` (из `input.users`),
`redis_hosts = []`, `redis_sentinel` (`{master_name, quorum}` в режиме sentinel; в
`sentinel_only` — плюс `master_ip` внешнего master-а; иначе пустой объект).

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

   Обязательность `shards` — **декларативно**: схема `input:` несёт
   `required_when: "input.redis_type == 'cluster'"` на поле `shards`. Пропуск `shards`
   в режиме cluster ловит **input-валидация Keeper-а ДО рендера** (понятный отказ,
   прогон не стартует) — это не shell-задача на хосте. Прежний `shards-guard`
   (`core.cmd.shell` с `has(input.shards)`) удалён.

1. **size-guard** (`assert:`, fail-fast ДО любого install) — проверяет, что число
   таргетированных хостов roster-а ровно `shards * (1 + replicas_per_shard)`.
   Предикат `assert.that[]` вычисляется **keeper-side при render** (там доступен
   `soulprint.hosts` — roster прогона); `false` **обрывает render** с `message`
   (`topology mismatch: hosts != shards*(1+replicas_per_shard)`) — ни одной задачи на
   Soul, прогон не стартует. `assert` не emit `RenderedTask` → задачи cluster-ветки
   после него сдвигаются на его позицию.
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

#### `create` (режим sentinel)

[`scenario/create/sentinel.yml`](scenario/create/sentinel.yml) — master-replica +
sentinel-демон. Топология: `1 master + replicas` реплик; master-election declared
(probe на create невозможен — redis ещё не поднят), первый по SID хост = master
(`soulprint.hosts[0]`, отсортирован детерминированно). Шесть шагов:

1. **size-guard** (`core.cmd.shell`, `run_once`, fail-fast) — число хостов roster-а
   обязано быть ровно `1 + replicas`. Соответствие вычисляется keeper-side при render
   (там доступен `soulprint.hosts`); отрендеренное булево встраивается в guard-команду
   (тот же приём, что в cluster). Не совпало → `topology mismatch: hosts != 1 + replicas`.
2. **`apply: destiny: redis`** — та же режим-агностичная destiny, но с
   `sentinel_enabled: true` → дополнительно рендерится `sentinel.conf` + `redis-sentinel`
   unit и поднимается демон. `config` — **базовый** merge (без cluster-директив).
   `sentinel.master_ip` — host-инвариантный адрес master-а (master-election
   `soulprint.hosts[0]`), резолвится `run_once` и одинаков у всех → корректно через
   `apply.input`. `announce-ip` — per-host в `sentinel.conf.tmpl` из `.self.network.primary_ip`.
3. **health-gate PING** (`community.redis.command`, `retry`) — каждый узел обязан
   ответить `PING` на `:6379` ДО привязки реплик/настройки sentinel.
4. **REPLICAOF** (`community.redis.replica`, на **каждом** хосте) — реплики следуют за
   избранным master-ом. `master_addr` — host-инвариантный (`soulprint.hosts[0]`); на
   самом master `addr == master_addr` → плагин no-op (guard в плагине), поэтому задачу
   можно гнать на всех хостах без отдельного `where`.
5. **SENTINEL MONITOR reconcile** (`community.redis.sentinel`, на **каждом** хосте) —
   каждый sentinel-демон мониторит master. `monitor.ip` — host-инвариантный
   (master-election); `quorum` — `input.sentinel_quorum` или большинство `size/2+1`.
   `auth_pass` — keeper-side `vault()`-резолв (маскируется). Алгоритм reconcile
   (`MONITOR`/`REMOVE`+`MONITOR`/`SET`/`CONFIG SET`) перенесён 1:1 из Ansible.
6. **health-gate PONG** (`community.redis.command`, `:26379`) — каждый sentinel-демон
   обязан ответить `PONG`. **Строго same-task** `register.self` (НЕ cross-passage
   flow-control, ADR-056): «дождаться N sentinel» не выражаем — проверяется только
   локальный sentinel этого хоста (`retry`/`until` + `failed_when` по
   `register.self.result == 'PONG'`).

State плагина (states `replica` / `sentinel`, их params и идемпотентность) — в его
per-module doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).

#### `create` (режим sentinel_only)

[`scenario/create/sentinel-only.yml`](scenario/create/sentinel-only.yml) — **тонкий
sentinel-слой**: разворачивается **только** sentinel-демон (без локальной
data-плоскости redis-server), мониторящий **внешний** master. Перенос прежнего
отдельного сервиса redis-sentinel в общий диспетчер. Отличия от `sentinel`:

- **master внешний.** Адрес задаёт оператор явно (`input.master_ip`/`master_port`) —
  master живёт вне этого прогона, master-election и REPLICAOF не нужны. `master_ip` —
  host-инвариант (один внешний master на кластер) → проброс через `apply.input`.
- **нет size-guard.** Любое число sentinel-ов валидно — quorum считается от их
  количества (`input.sentinel_quorum` или большинство `size/2+1`).
- **обязательность `master_ip` — декларативно.** Схема `input:` несёт
  `required_when: "input.redis_type == 'sentinel_only'"` на поле `master_ip`. Пропуск
  в этом режиме ловит **input-валидация Keeper-а ДО рендера** (понятный отказ, прогон
  не стартует) — не shell-guard на хосте. Прежний `master_ip-guard` (`core.cmd.shell`
  с `has(input.master_ip)`) удалён; в render CEL читает `default(input.master_ip, '')`
  как render-safety (валидация уже отвергла бы пропуск).

Три шага:

1. **`apply: destiny: redis`** с `deploy_redis: false` — пакет `redis` ставится
   **всегда** (несёт sentinel-демон), но data-плоскость redis-server (`redis.conf` /
   `users.acl` / PEM / systemd redis-server) гасится placeholder-skip-ом; рендерится
   только `sentinel.conf` + `redis-sentinel` unit + демон (`sentinel_enabled: true`).
   `sentinel.master_ip` — внешний адрес из `input.master_ip` (host-инвариант →
   `apply.input`); `announce-ip` — per-host в `sentinel.conf.tmpl` из
   `.self.network.primary_ip`.
2. **SENTINEL MONITOR reconcile** (`community.redis.sentinel`, на **каждом** хосте) —
   каждый sentinel-демон мониторит внешний master: `monitor.ip` = `input.master_ip`
   (не из `soulprint.hosts` — master вне прогона), `quorum` — `input.sentinel_quorum`
   или большинство `size/2+1`, `auth_pass` — keeper-side `vault()`-резолв
   (маскируется). Тот же state плагина, что в `sentinel`.
3. **health-gate PONG** (`community.redis.command`, `:26379`) — каждый sentinel-демон
   обязан ответить `PONG`. Строго same-task `register.self` (НЕ cross-passage
   flow-control, ADR-056): проверяется только локальный sentinel этого хоста
   (`retry`/`until` + `failed_when` по `register.self.result == 'PONG'`).

### `add_node` (day-2: присоединить ноду к кластеру)

[`scenario/add_node/main.yml`](scenario/add_node/main.yml) — присоединить **одну**
новую ноду к уже сформированному Redis-кластеру (режим `redis_type=cluster`). Аналог
`redis-cli --cluster add-node`, но целиком через плагин `community.redis.cluster`
(`action: add-node`) — без `redis-cli`/shell. Новая нода обязана быть уже привязанной
к incarnation как Soul (онбординг — вне сценария); таргетинг по стабильному SID через
`where:`. Прогон зовётся на весь incarnation: roster (`soulprint.hosts`) содержит и
существующие ноды, и новичка — из него строятся endpoint-ы. Четыре шага:

1. **guard** (`core.cmd.shell`, `run_once`) — `new_node_sid` и `seed_sid` обязаны быть
   различными членами roster-а прогона (keeper-side, тот же приём, что size-guard).
2. **`apply: destiny: redis`** на **новой** ноде (`where: soulprint.self.sid == input.new_node_sid`)
   — install + render `redis.conf` (cluster-директивы из `incarnation.state.redis_config` —
   **источник истины**, зафиксированный `create`, не перевычисляется → нет drift) + systemd.
3. **health-gate PING** на новой ноде — обязана ответить ДО ввода в кластер.
4. **add-node** (`community.redis.cluster`, `action: add-node`, `run_once`) — endpoint-ы
   `new_node`/`seed`/`master` строятся из roster-а по SID. Плагин шлёт `CLUSTER MEET`
   через `seed` + `REPLICATE` (при `role: replica`) или добавляет пустой master (при
   `role: master`; слоты не двигает — это отдельный `reshard`, follow-up). `master`
   передаётся только при `role: replica` и заданном `master_sid`; иначе плагин выбирает
   master сам (балансировка). `incarnation.state` этим сценарием **не мутируется** в
   текущем срезе (точная роль каждого SID в state не пишется — `redis_hosts`-наполнение
   follow-up). Параметры `add-node` state — в
   [per-module doc](../../../docs/module/community/redis/README.md).

### `remove_node` (day-2: вывести ноду из кластера)

[`scenario/remove_node/main.yml`](scenario/remove_node/main.yml) — вывести **одну**
ноду из уже сформированного Redis-кластера (режим `redis_type=cluster`). Аналог
`redis-cli --cluster del-node`, но целиком через плагин `community.redis.cluster`
(`action: remove-node`) — без `redis-cli`/shell. Выводимая нода и `seed` обязаны быть
в roster-е прогона (`soulprint.hosts`); таргетинг по стабильному SID. Два шага:

1. **guard** (`core.cmd.shell`, `run_once`) — `remove_node_sid` и `seed_sid` обязаны
   быть различными членами roster-а (keeper-side, симметрия с `add_node`).
2. **remove-node** (`community.redis.cluster`, `action: remove-node`, `run_once`) —
   endpoint-ы `node`/`seed` строятся из roster-а по SID. Плагин читает `CLUSTER NODES`
   с `seed` и ветвится: master со слотами → **миграция слотов** на оставшиеся masters
   (`SETSLOT`/`MIGRATE`/`SETSLOT NODE`, online — данные не теряются) + `CLUSTER FORGET`
   на всех; replica / master без слотов → просто `CLUSTER FORGET`. Идемпотентен (ноды
   уже нет → no-op). Decommission самого хоста (остановка redis, чистка `nodes.conf`,
   удаление Soul-а) — **вне** сценария; `incarnation.state` не мутируется (симметрия с
   `add_node`). Параметры `remove-node` state — в
   [per-module doc](../../../docs/module/community/redis/README.md).

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
# sentinel
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml
# sentinel_only (только sentinel-демон, внешний master)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/sentinel-only-create/case.yml
# add_node (day-2)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/add_node/tests/add-replica-explicit-master/case.yml
# remove_node (day-2)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/remove_node/tests/remove-node-from-cluster/case.yml
```

[Кейс `create/tests/full-stack`](scenario/create/tests/full-stack/case.yml)
проверяет план standalone: задачи destiny `redis` (install + render `users.acl` +
render `redis.conf` + systemd hardening drop-in + running + restarted) + задача
`community.redis.command` (`PING`). Ручного `daemon-reload`-шага в плане нет:
`core.service` (`daemon_reload: auto`, default) сам перечитывает конфигурацию systemd
при смене unit-файла перед start/restart. Главный guard слайса — что
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
  — `shards=3` против 4 хостов: `assert:` size-guard рвёт render (предикат
  `size(soulprint.hosts) == shards*(1+replicas_per_shard)` ложен → обрыв с `message`,
  ни одной задачи на Soul).

Sentinel-кейсы под [`scenario/create/tests/`](scenario/create/tests/):

- [`sentinel-create-1master-2replica`](scenario/create/tests/sentinel-create-1master-2replica/case.yml)
  — `replicas=2` (3 хоста, size-guard PASS): master-election (первый по SID = master),
  `community.redis.replica` (`master_addr` host-инвариантен), `community.redis.sentinel`
  (`monitor.ip` host-инвариантен, `quorum=2` из input), `redis_sentinel` в state.
- [`sentinel-no-replicas-auto-quorum`](scenario/create/tests/sentinel-no-replicas-auto-quorum/case.yml)
  — `replicas=0`, `sentinel_quorum` не задан → auto `size/2+1`.
- [`sentinel-size-guard-mismatch`](scenario/create/tests/sentinel-size-guard-mismatch/case.yml)
  — FAIL-ветка size-guard (`hosts != 1 + replicas`).
- [`sentinel-only-create`](scenario/create/tests/sentinel-only-create/case.yml)
  — `redis_type=sentinel_only`, `master_ip` задан: input-валидация и mode-guard
  проходят, пакет `redis` ставится при `deploy_redis=false`, `sentinel.conf`
  рендерится (monitor внешнего master, quorum `size/2+1`), data-плоскость
  redis-server в плане placeholder-skip, `community.redis.sentinel` мониторит
  `input.master_ip`, PONG-gate `:26379`; `redis_config` в state пустой.
- [`sentinel-only-requires-master-ip`](scenario/create/tests/sentinel-only-requires-master-ip/case.yml)
  — `redis_type=sentinel_only` без `master_ip`: отказ на **input-валидации**
  (`required_when: "input.redis_type == 'sentinel_only'"`) — рендер не запускается,
  прогон не стартует.

add_node-кейсы под [`scenario/add_node/tests/`](scenario/add_node/tests/):

- [`add-replica-explicit-master`](scenario/add_node/tests/add-replica-explicit-master/case.yml)
  — `role=replica` с явным `master_sid` (`master.addr` = IP указанного хоста).
- [`add-replica-auto-master`](scenario/add_node/tests/add-replica-auto-master/case.yml)
  — `role=replica` без `master_sid` (`master.addr` пуст → плагин балансирует).
- [`add-empty-master`](scenario/add_node/tests/add-empty-master/case.yml)
  — `role=master` (пустой master без слотов).
- [`guard-mismatch-same-sid`](scenario/add_node/tests/guard-mismatch-same-sid/case.yml)
  — `new_node_sid == seed_sid`: FAIL-ветка add_node-guard.

В живом Keeper service + destiny резолвятся как git-репо по ref (ADR-007/009).

## В работе

Следующие батчи эпика redis-консолидации (в этом сервисе **пока не реализованы**):

- day-2 cluster-операция `reshard` (плагин `community.redis.cluster` умеет
  `action: create` / `add-node` / `remove-node`; `reshard` в action-enum пока нет);
- day-2 sentinel: failover (switchover) и прочие day-2-операции sentinel-топологии;
- day-2 в целом: `update_config` (live `CONFIG SET` дельты), `add_user` (плагинный
  state `acl`), безопасный rolling-restart;
- плагинные states `community.redis`: `acl` / `failover` (`command` / `config` /
  `cluster` / `replica` / `sentinel` уже есть);
- TLS sentinel-демона (`:26379`): data-плоскость redis-server TLS уже реализована
  (`tls_enable`/`tls_only`), TLS для sentinel-демона — follow-up.

Состояние плагина `community.redis` (какие states реализованы) — в его per-module
doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).
