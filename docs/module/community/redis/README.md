# community.redis

ОСНОВНОЙ интерфейс к **живому Redis** в redis-консолидации (концепция
Ansible-роли): scenario сервиса оркеструет порядок/таргетинг/rolling, а плагин
исполняет **одну** операцию над одним Redis-инстансом. Custom-плагин
`kind: soul_module` (namespace `community`, name `redis`), бинарь
`soul-mod-community-redis`. Реализация —
[`examples/module/soul-mod-community-redis/`](../../../../examples/module/soul-mod-community-redis/)
(`impl.go` — диспетчер + states command/config, `cluster.go` — cluster-state,
`replica.go` — replica-state (REPLICAOF), `sentinel.go` — sentinel-state
(MONITOR/SET reconcile), `helpers.go` — structpb/secret-helpers).

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

Текущий срез — пять state. Остальные **планируются** следующими батчами и пока в
`manifest.yaml::spec.states` **отсутствуют**.

| State | Назначение | `changed` |
|---|---|---|
| `command` | Raw-команда к Redis (imperative verb-state, прецедент `core.cmd.shell`/`core.exec.run`/`core.http.probe`). | `false` по умолчанию; `changed: true` в params — для реально мутирующих команд. |
| `config` | Применить map директив `redis.conf` через `CONFIG SET` (+ опц. `CONFIG REWRITE`). | `true` при ≥1 применённой директиве. |
| `cluster` | Управление hash-slot-кластером (16384 слота) через `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE`/`FORGET`/`SETSLOT`/`MIGRATE`. Реализованы `action: create` (сборка с нуля), `action: add-node` (присоединение одной ноды day-2), `action: remove-node` (вывод одной ноды day-2, с миграцией слотов master-а) и `action: reshard` (перенос N слотов с master-а на master, day-2). | `create`/`add-node`/`remove-node` идемпотентны: `true` при изменении, `false` (no-op), если кластер уже сформирован / нода уже в кластере / ноды уже нет. **`reshard` НЕ идемпотентен** (см. ниже): `true` при успешном переносе, `failed` при ошибке ввода; no-op-ветки нет. |
| `replica` | Привязать инстанс к master-у через `REPLICAOF` (+ `CONFIG SET masterauth`). | `true` при настройке; `false` (no-op), если уже реплика нужного master-а или `addr == master_addr` (сам master). |
| `sentinel` | Реконсилировать Redis Sentinel (`SENTINEL MONITOR`/`REMOVE`/`SET`/`CONFIG SET`). Алгоритм 1:1 из Ansible `redis_sentinel_update.py`. | `true` при изменении монитора/параметров; `false` (no-op), если всё совпало. |
| `acl` *(планируется)* | Idempotent ACL-user (`SETUSER`/`DELUSER`). | — |
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

Управляет Redis-кластером **целиком через go-redis** (никакого `redis-cli`/shell).
Операция выбирается полем `action`: реализованы `create` (сборка кластера с нуля),
`add-node` (присоединение одной ноды day-2), `remove-node` (вывод одной ноды
day-2) и `reshard` (перенос N слотов с master-а на master, day-2).

> **★ Идемпотентность.** `create`/`add-node`/`remove-node` **идемпотентны** —
> повторный apply на сошедшемся входе даёт `changed=false` (no-op), их безопасно
> держать в converge. **`reshard` — НЕТ.** Это императивная **exec-style** day-2
> операция (как старый `redis-cluster-live` без `unless`): повторный apply
> сдвинет **ещё** `slots` слотов с `from` на `to`. Оператор зовёт reshard
> **явно**, ровно столько раз, сколько нужно переносов; reshard **не** часть
> converge-цикла.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `action` | string | required | Операция: `create` (сборка из `nodes`), `add-node` (присоединение одной ноды), `remove-node` (вывод одной ноды) или `reshard` (перенос N слотов; **НЕ идемпотентен**). |
| `password` | string (secret) | optional | См. «Пароль». Применяется при коннекте к **каждой** ноде (`create`) / к `new_node`+`seed`+`master` (`add-node`) / к `node`+`seed`+оставшимся masters (`remove-node`) / к `from`+`to` (`reshard`). |
| `username` | string | optional | ACL-username для `AUTH`. |

### cluster — params (`action: create`)

Собирает кластер из набора `nodes`: `CLUSTER MEET` (gossip) → `CLUSTER ADDSLOTS`
мастерам → `CLUSTER REPLICATE` репликам. Идемпотентен — повторный вызов на уже
сформированном кластере (`cluster_state:ok`, состав совпал, 16384 слота
покрыты) даёт `changed=false`, no-op.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `nodes` | map | required (`create`) | Узлы: map стабильный-ключ (SID/имя) → `{ addr: "host:port" }` либо `{ ip: "10.0.0.1", port: 6379 }`. **Ключи сортируются** — детерминируют раскладку master/replica. `addr` — для коннекта, `ip`+`port` — для `CLUSTER MEET` (gossip оперирует `ip:port`, не DNS-именем). |
| `replicas_per_shard` | int | optional (default `0`) | Реплик на шард. `shards = len(nodes) / (1 + replicas_per_shard)`; `len(nodes)` обязан делиться на размер шарда без остатка. |

**Детерминированная раскладка.** Ключи `nodes` сортируются; первые `shards`
нод — мастера, остальные — реплики round-robin к мастерам (`replica j →
master j%shards`). 16384 слота делятся поровну между мастерами; остаток
(`16384 % shards`) распределяется по одному слоту первым мастерам. Один и тот
же вход `nodes` всегда даёт одну и ту же топологию и одни и те же диапазоны
слотов.

**Сходимость gossip** — ограниченный retry (не бесконечный цикл): после `MEET`
плагин ждёт, пока `CLUSTER NODES` покажет все ноды, и лишь затем шлёт
`ADDSLOTS`/`REPLICATE`. Если за лимит не сошлось — `failed`.

### cluster — params (`action: add-node`)

Присоединяет **одну** новую ноду к уже сформированному кластеру (`CLUSTER MEET`
через `seed` → `CLUSTER REPLICATE` к master при `role: replica` либо пустой
master при `role: master`). Идемпотентен (`CLUSTER NODES`): нода уже в кластере →
`changed=false`, no-op. `role: master` добавляет **пустой** master без слотов —
перенос слотов это отдельный `reshard` (add-node слоты не двигает).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `new_node` | map | required (`add-node`) | Присоединяемая нода: `{ addr: "host:port" }` либо `{ ip, port }`. `addr` — для коннекта (`REPLICATE` исполняется на ней), `ip`+`port` — для `CLUSTER MEET`. |
| `seed` | map | required (`add-node`) | Любая существующая нода кластера — контакт для `MEET` и источник `CLUSTER NODES` (идемпотентность): `{ addr: "host:port" }` либо `{ ip, port }`. |
| `role` | string | optional (default `replica`) | Роль новичка: `replica` (`CLUSTER REPLICATE` к `master` или к наименее загруженному) или `master` (пустой master без слотов). |
| `master` | map | optional (`role: replica`) | Master, чьей репликой станет новичок: `{ addr: "host:port" }` либо `{ ip, port }`. Не задан → плагин выбирает master с наименьшим числом реплик (балансировка, как `redis-cli` без `--cluster-master-id`). |

### cluster — params (`action: remove-node`)

Выводит **одну** ноду из уже сформированного кластера (зеркало `redis-cli
--cluster del-node`/`reshard`, но целиком через go-redis). Плагин читает `CLUSTER
NODES` с `seed` и ветвится по роли удаляемой ноды:

- **master со слотами** — сначала **миграция слотов** на оставшиеся masters
  (round-robin по их отсортированному node-id, детерминированно): на каждый слот
  `CLUSTER SETSLOT <slot> IMPORTING <src-id>` на цели → `MIGRATING <dst-id>` на
  источнике → перенос ключей пакетами (`CLUSTER GETKEYSINSLOT` + `MIGRATE … KEYS …`,
  online — данные не теряются) → `CLUSTER SETSLOT <slot> NODE <dst-id>` на обеих
  нодах. Затем `CLUSTER FORGET <remove-id>` на всех оставшихся.
- **replica или master без слотов** — просто `CLUSTER FORGET <remove-id>` на всех
  оставшихся нодах (слоты не двигаются).

Идемпотентен (`CLUSTER NODES`): ноды уже нет в кластере → `changed=false`, no-op.
`FORGET` по уже забытой ноде на отдельном узле (gossip-anti-entropy) трактуется как
no-op, не ошибка. `MIGRATE` к password-protected destination несёт `AUTH <pass>`
**на проводе** (как и сам go-redis) — это единственное место; в события/логи/ошибки
пароль не попадает (см. «Пароль»). Decommission самого хоста (остановка redis,
чистка `nodes.conf`) — **вне** этой операции.

> **★ Partial-failure (нет авто-отката).** Для master-а **со слотами** миграция
> (`SETSLOT IMPORTING`/`MIGRATING` → `MIGRATE` → `SETSLOT NODE`) — та же
> неатомарная, без отката, что и у `reshard`. Если операция упадёт **после**
> `SETSLOT IMPORTING`/`MIGRATING`, но **до** финального `SETSLOT NODE` (обрыв на
> `MIGRATE`, ошибка посреди multi-batch слота `> 100` ключей), слот застрянет в
> подвешенном IMPORTING(target)/MIGRATING(source), уже перенесённые слоты —
> **остаются** перенесёнными, `FORGET` ещё **не** выполнен, apply вернёт `failed`,
> кластер — в неконсистентном промежуточном состоянии. Recovery **ручной**:
> проверить `CLUSTER NODES`, на застрявших слотах либо `CLUSTER SETSLOT <slot>
> STABLE`, либо повторить `remove-node` (он домигрирует остаток и доделает
> `FORGET`). Это **осознанная семантика** императивной операции (как `redis-cli
> --cluster`), **не баг**.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `node` | map | required (`remove-node`) | Выводимая нода: `{ addr: "host:port" }` либо `{ ip, port }`. Если это master со слотами — слоты сперва мигрируют на оставшиеся masters. |
| `seed` | map | required (`remove-node`) | Любая существующая нода кластера — контакт для `CLUSTER NODES` (топология + идемпотентность) и источник списка нод для `FORGET`: `{ addr: "host:port" }` либо `{ ip, port }`. |

### cluster — params (`action: reshard`)

Переносит `slots` hash-слотов с master-а `from` на master `to` (зеркало `redis-cli
--cluster reshard`, целиком через go-redis). Плагин читает `CLUSTER NODES` с `from`,
берёт **первые `slots` слотов источника по возрастанию** и переносит каждый: на
цели `CLUSTER SETSLOT <slot> IMPORTING <from-id>` → на источнике `MIGRATING
<to-id>` → перенос ключей пакетами (`CLUSTER GETKEYSINSLOT` + `MIGRATE … KEYS …`,
online — данные не теряются, **whitespace-имена ключей** переезжают как один
аргумент за счёт типизированного `GetKeysInSlot`) → `CLUSTER SETSLOT <slot> NODE
<to-id>` на обеих нодах.

> **★ reshard НЕ ИДЕМПОТЕНТЕН (осознанно).** Повторный apply сдвинет **ещё**
> `slots` слотов с `from` на `to` — это императивная exec-style day-2 операция,
> **не** часть converge. Нет `unless`/probe «уже перенесено»: оператор отвечает
> за то, сколько раз её зовёт. L0 (`cluster_test.go`) доказывает
> **последовательность** команд и лосслесс на fake-conn, но не «доказывает»
> идемпотентность — её здесь и нет by design. Реальную смену владельца слотов и
> перенос ключей (вкл. whitespace+TTL) на живом кластере проверяет **L3c**
> (`cluster_reshard_l3c_test.go`, build-tag `e2e_live`, `t.Skip` до harness-а).

Ошибки ввода (`from`/`to` — не master в кластере, `from == to`, `slots < 1`,
`slots` больше числа слотов у источника) → `failed`, перенос не начат.

> **★ Partial-failure (нет авто-отката).** Слот-миграция **не атомарна** и **не
> откатывается**. Если операция упадёт **после** `CLUSTER SETSLOT IMPORTING`
> (цель) / `MIGRATING` (источник), но **до** финального `SETSLOT NODE` (обрыв на
> `MIGRATE`, ошибка посреди multi-batch слота `> 100` ключей), этот слот
> застрянет в подвешенном состоянии IMPORTING(`to`)/MIGRATING(`from`), уже
> перенесённые ранее слоты — **остаются** перенесёнными, apply вернёт `failed`,
> кластер — в неконсистентном промежуточном состоянии. Recovery **ручной**:
> проверить `CLUSTER NODES`, на застрявших слотах либо добить `CLUSTER SETSLOT
> <slot> STABLE`, либо повторить `reshard` (он добьёт остаток). Это **осознанная
> семантика** императивной операции (как `redis-cli --cluster`), **не баг**.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `from` | map | required (`reshard`) | Master-**источник** слотов: `{ addr: "host:port" }` либо `{ ip, port }`. Обязан быть master-ом в кластере и владеть `>= slots` слотами. |
| `to` | map | required (`reshard`) | Master-**получатель** слотов: `{ addr: "host:port" }` либо `{ ip, port }`. Обязан быть master-ом в кластере и отличаться от `from`. |
| `slots` | int | required (`reshard`) | Сколько слотов перенести (`>= 1`). Берутся первые `slots` слотов источника по возрастанию. **Не идемпотентно** — повторный apply перенесёт ещё `slots`. |

## replica — params

Привязывает инстанс к master-у через `REPLICAOF` (go-redis). `masterauth`
ставится `CONFIG SET` **до** `REPLICAOF` (реплика обязана знать пароль master-а).
Идемпотентен (`INFO replication`): уже реплика нужного master-а со здоровым
линком → `changed=false`, no-op.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес **этого** инстанса: `host:port` или `unix:/path`. На redis-хосте локальный (`127.0.0.1:6379`). |
| `master_addr` | string | required | Адрес master-а `host:port`. **HOST-ИНВАРИАНТНЫЙ** (один на кластер) — scenario резолвит его run_once (`soulprint.hosts[0]`). `addr == master_addr` → инстанс есть master, `changed=false`, no-op (guard в плагине, чтобы scenario звал `replica` на всех хостах). |
| `password` | string (secret) | optional | Пароль master-а. Ставится как `masterauth` до `REPLICAOF`. Пустой → `masterauth` не ставится. См. «Пароль». |
| `username` | string | optional | ACL-username для репликации (`CONFIG SET masteruser`). |

## sentinel — params

Реконсилирует Redis Sentinel **целиком через go-redis** (без `redis-cli`):
`SENTINEL MONITOR`/`REMOVE`+`MONITOR` (монитор) → `SENTINEL SET` (per-master) →
`SENTINEL CONFIG SET` (globals). Источник желаемого — `config` (директивы в
**файловой форме** `sentinel.conf`); плагин сам делит их на globals/per-master
(top-level `CONFIG` в режиме Sentinel не поддерживается). Алгоритм перенесён 1:1
из Ansible `library/redis_sentinel_update.py`
(`classify_config`/`compute_monitor_action`/`compute_set_updates`). Идемпотентен
(diff против `SENTINEL MASTER`/`CONFIG GET`).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Sentinel-инстанса `host:port` (обычно `127.0.0.1:26379`). |
| `master_name` | string | optional (default `mymaster`) | Логическое имя monitored master-а. |
| `monitor` | map | optional | Желаемый адрес master: `{ ip, port, quorum }`. `ip` — **HOST-ИНВАРИАНТНЫЙ**. Не задан → монитор не трогается (только `SET`/`CONFIG SET`). |
| `config` | map | optional | Директивы Sentinel в **файловой форме** (`"sentinel down-after-milliseconds mymaster": "12000"`, `"sentinel announce-ip": "10.0.0.1"`, `"loglevel": "notice"`). Startup-only (`dir`/`port`/`tls-*`) игнорируются — они меняются рестартом. |
| `auth_user` | string | optional | Пользователь для AUTH Sentinel-а на master (`SENTINEL SET auth-user`). Ставится при создании/пересоздании монитора. |
| `auth_pass` | string (secret) | optional | Пароль для AUTH Sentinel-а на master (`SENTINEL SET auth-pass`). Маскируется — **не** попадает в события/логи. |
| `redis_version` | string | optional | Версия Redis для version-gate глобальных параметров (`loglevel` доступен в Sentinel с 7.0). Не задана → version-gated параметры отбрасываются. |
| `password` | string (secret) | optional | Пароль для коннекта **к самому** Sentinel (его `requirepass`), если задан. См. «Пароль». |
| `username` | string | optional | ACL-username для коннекта к Sentinel. |

## Пароль (ИБ-инвариант ADR-010)

Capability плагина — **только `network_outbound`**, `vault_access` **нет**:
пароль приходит уже зарезолвленным от Keeper. В operator-input
(scenario/destiny) пароль задаётся vault-ref-ом через CEL `${ vault(...) }`;
keeper-side render-фаза резолвит его **до** `Apply` и передаёт плагину
plaintext-значение (ADR-012 — Soul/плагин Vault-клиент не тянет). В манифесте
`password` помечен `secret: true` + `pattern: "^vault:.*"` — это форсирует
vault-ref на входе и маскирование в логах/трейсах/UI.

Код-инвариант (проверяется L0): `params["password"]` (и `auth_pass` в `sentinel`)
**никогда** не попадают в `ApplyEvent.Message`/`.Output`, в текст ошибок и в
stderr. Ошибки коннекта санитизируются (`redactError` вырезает подстроку
пароля); вывод самих команд (`result`) — это ответ сервера, не секрет оператора.
В `sentinel` Output несёт только **имена** применённых действий (`sentinel_monitor`/
`sentinel_set`/…), не их секретные значения; `replica` ставит `masterauth`
аргументом `CONFIG SET` (нужно Redis для синхронизации), но в события его не пишет.

## TLS-коннект (концепция Ansible-роли redis: `redis_tls_*`)

Все states (`command`/`config`/`cluster`/`replica`/`sentinel`) принимают **общие**
TLS-параметры коннекта. По умолчанию TLS выключен (plaintext, back-compat); при
`tls: true` плагин коннектится к Redis по TLS.

| Параметр | Тип | По умолчанию | Назначение |
|---|---|---|---|
| `tls` | bool | `false` | Коннектиться по TLS. **Обязателен в only-TLS** (Redis `port 0`, plain закрыт): без него плагин не достучится. |
| `tls_ca` | string (secret, PEM) | — | CA-сертификат для проверки серверного (RootCAs). При частном PKI практически обязателен. |
| `tls_cert` | string (secret, PEM) | — | Client-сертификат для mTLS (опц., **только вместе** с `tls_key`). |
| `tls_key` | string (secret, PEM) | — | Client-ключ для mTLS (опц., **только вместе** с `tls_cert`). |
| `tls_skip_verify` | bool | `false` | **ЯВНЫЙ opt-out** проверки серверного сертификата. По умолчанию проверка **включена** (default secure). |

Модель безопасности (security-инвариант: insecure = явный opt-out, default secure):
при `tls: true` плагин по умолчанию **проверяет** серверный сертификат (`RootCAs`
из `tls_ca`). Отключить проверку можно **только** явным `tls_skip_verify: true`.
`tls_cert`+`tls_key` задаются строго вместе (один без другого → ошибка валидации
конфигурации, без утечки PEM в текст).

PEM приходит **целиком** в params: scenario резолвит его из Vault через
`${ vault(...) }` в render-фазе и кладёт PEM в `apply.input` (как `requirepass`) —
плагин свой Vault-доступ не тянет (capability остаётся `network_outbound`). В
манифесте `tls_ca`/`tls_cert`/`tls_key` помечены `secret: true` + `pattern:
"^vault:.*"` (декларация secret-источника); маскинг — по **имени ключа**:
`shared/audit` маскирует `tls_key`/`tls_cert`/`tls_ca` в логах/OTel/RunResult/UI.
Код-инвариант (L0): PEM client-ключ не попадает в `ApplyEvent`/ошибки коннекта
(TLS-handshake-ошибка санитизируется `redactError` по `password` **и** PEM-ключу).

**Mutual cluster-bus поддержан.** При взаимной аутентификации нод (Redis
`tls-cluster yes`) ноды требуют клиентский сертификат от того, кто к ним
коннектится. Scenario сервиса `redis` в шаге `cluster` `action: create`
пробрасывает плагину `tls_cert`/`tls_key` (резолв из тех же
`vault(essence.tls_cert_ref/tls_key_ref)`, что и серверный PEM redis.conf) —
плагин строит mTLS-пару (`tls.go`: client-cert добавляется, когда заданы **оба**
`tls_cert`+`tls_key`). Без mutual-bus эти параметры не мешают handshake
(используются только если сервер их запросил). `cert`/`key` host-инвариантны
(один на кластер) → корректно идут через `apply.input`.

**Anti-downgrade (ИБ).** Подключения плагина в scenario `redis` гейтятся на
`essence.tls_enable`, **не** `tls_only`: при `tls_enable: true` плагин коннектится
по TLS даже когда plain-порт ещё открыт (`tls_only: false`). Иначе AUTH-пароль ушёл
бы по сети plaintext-ом (plaintext-downgrade). Порт коннекта — `tls_port` при
`tls_enable`, иначе plain `6379`.

## Capabilities / side-effects

- `required_capabilities: [network_outbound]` — TCP/unix/**TLS**-коннект к Redis
  (для `cluster` — коннект к каждой ноде из `nodes`). **Без** `vault_access` (пароль
  и PEM резолвит Keeper), **без** `exec_subprocess` / `fs_write_root` (плагин не
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
  ключей; деление слотов с остатком; **add-node** (replica авто/явный master,
  пустой master, идемпотентность); **remove-node** (replica → только `FORGET`;
  master со слотами → миграция слотов `SETSLOT`/`MIGRATE`/`SETSLOT NODE` +
  `FORGET`; пустой master → только `FORGET`; идемпотентность «ноды уже нет» →
  no-op); **reshard** (`Validate` — пустые `from`/`to`/`slots`, `from == to` вкл.
  смешанную `{addr}`/`{ip,port}`-форму, `slots < 1`; happy-перенос — первые N
  слотов источника по возрастанию через `SETSLOT IMPORTING`/`MIGRATING`/`MIGRATE`/
  `SETSLOT NODE` на обеих нодах; **whitespace-lossless**; `from` не master →
  `failed`; `slots` больше имеющихся → `failed`); **ИБ-инвариант** (пароль не
  течёт в события/команды/ошибку коннекта; единственный wire-AUTH — в `MIGRATE`,
  проверяется отдельным assert-ом).
- **L3c reshard skeleton**
  ([`cluster_reshard_l3c_test.go`](../../../../examples/module/soul-mod-community-redis/cluster_reshard_l3c_test.go)):
  e2e-live против настоящего кластера (build-tag `e2e_live` + `t.Skip` до
  harness-сущности «live redis cluster»). TODO-инвариант: запись ключей в слоты
  source (вкл. whitespace+TTL), один императивный reshard, проверка реальной
  смены владельца слотов + лосслесс ключей + TTL + сходимости `DBSIZE`.
  Компилируется в гейте, реально не гоняется без живого кластера.
- **L0 replica**
  ([`replica_test.go`](../../../../examples/module/soul-mod-community-redis/replica_test.go)):
  fake `redisConn` со скриптованным `INFO replication`. Покрывает `Validate`
  (нет `master_addr`); `REPLICAOF` + `masterauth` ДО него; идемпотентность (уже
  реплика нужного master-а → no-op); `addr == master_addr` → master-guard no-op
  (ни одной команды); пустой пароль → `masterauth` не ставится; **ИБ-инвариант**
  (пароль не течёт в события/санитизированную ошибку коннекта).
- **L0 sentinel**
  ([`sentinel_test.go`](../../../../examples/module/soul-mod-community-redis/sentinel_test.go)):
  fake `redisConn` со скриптованными `SENTINEL MASTER`/`CONFIG GET`. Покрывает
  чистые функции переноса (`classifyConfig`/`supportedGlobals` version-gate +
  секрет-фильтр/`computeMonitorAction`/`computeSetUpdates`); `Validate`; `MONITOR`
  + auth-set для нового монитора; идемпотентность (адрес совпал → no-op); `readd`
  (`REMOVE`+`MONITOR`) при смене адреса; per-master `SET` reconcile (только
  отличия); globals `CONFIG SET` reconcile; **ИБ-инвариант** (`auth_pass` не течёт
  в события/ошибку коннекта).
- **L1** (integration, testcontainers redis/sentinel) — следующий батч.

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
- [examples/service/redis/](../../../../examples/service/redis/) — сервис redis:
  scenario `create` (standalone/cluster/sentinel), `add_node`, `remove_node` и
  `reshard` (day-2, **НЕ идемпотентен**) вызывают states
  `command`/`cluster`/`replica`/`sentinel` этого плагина.
- [examples/destiny/redis/](../../../../examples/destiny/redis/) —
  режим-агностичный per-host кирпич (install + render `redis.conf` + systemd).
- [ADR-012](../../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, пароль
  доезжает значением.
- [ADR-031 Scry](../../../adr/0031-scry-drift.md) — default-deny на dry_run без
  `PlanReadSafe`.
- [templating.md](../../../templating.md) — секрет-маскинг (§7.4).
