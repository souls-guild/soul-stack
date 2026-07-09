# community.redis

ОСНОВНОЙ интерфейс к **живому Redis** в redis-консолидации (концепция
Ansible-роли): scenario сервиса оркеструет порядок/таргетинг/rolling, а плагин
исполняет **одну** операцию над одним Redis-инстансом. Custom-плагин
`kind: soul_module` (namespace `community`, name `redis`), бинарь
`soul-mod-community-redis`. Реализация —
[`examples/module/soul-mod-community-redis/`](../../../../examples/module/soul-mod-community-redis/)
(`impl.go` — диспетчер + states command/config/acl, `probe.go` — read-probe states
pinged/role/replica-synced/offset-synced, `cluster.go` + `migrate.go` — cluster-state
(create/add-node/remove-node/reshard + join-external/failover-takeover/forget-external),
`replica.go` — replica-state (REPLICAOF, вкл. внешний источник `source_external`),
`detach.go` — detached-state (REPLICAOF NO ONE, промоушен),
`sentinel.go` — sentinel-state (MONITOR/SET reconcile), `helpers.go` —
structpb/secret-helpers).

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

Текущий срез — **двенадцать** state (`manifest.yaml::spec.states`): три declarative
над одним инстансом (`config`/`acl`/`replica`/`detached`), четыре read-probe
(`pinged`/`role`/`replica-synced`/`offset-synced`), императивный `command`,
многонодный `cluster` и `sentinel`.

| State | Назначение | `changed` |
|---|---|---|
| `command` | Raw-команда к Redis (imperative verb-state, прецедент `core.cmd.shell`/`core.exec.run`/`core.http.probe`). | `false` по умолчанию; `changed: true` в params — для реально мутирующих команд. |
| `pinged` | Health-probe через go-redis `PING` (ожидает `PONG`). Read-only. Заменяет idiom `command args:[PING]` — health-gate в сценариях (`retry`/`until`/`failed_when` по `register.self.result`). | `false` **конструктивно** (probe, не изменение). |
| `role` | Role-probe через go-redis `INFO replication` — фактическая **волатильная** роль инстанса. Read-only. Используется для `where`-таргетинга rolling-restart (ADR-008: роль волатильна, замеряется живым probe перед таргетингом). | `false` **конструктивно** (probe). |
| `replica-synced` | Probe ресинка реплики через go-redis `INFO replication` (`master_link_status == "up"`). Read-only. Только для **реплики** (у master-а поля нет → `synced=false`). Health-gate rolling-restart — дождаться полного ресинка реплики перед рестартом следующей. | `false` **конструктивно** (probe). |
| `offset-synced` | Safety-gate миграции из **внешнего** источника: сверяет `slave_repl_offset` своего инстанса с `master_repl_offset` внешнего master-а (второй коннект к `source_addr`). Read-only. «Link жив ≠ данные догнаны»; `caught_up=true` только при `link up` + нет идущей full-sync + `lag <= lag_threshold`. | `false` **конструктивно** (probe). |
| `config` | Применить map директив `redis.conf` через `CONFIG SET` (+ опц. `CONFIG REWRITE`). Startup-only-директивы (`port`/`dir`/`aclfile`/… — денилист) **пропускаются** (CONFIG SET их отвергает). | `true` при ≥1 применённой директиве. |
| `acl` | Hot-reload ACL живого Redis через `ACL LOAD` (перечитать `aclfile` целиком — `users.acl` рендерит destiny ДО этого шага). Идемпотентен **по конструкции**; вывод `ACL LIST` в Output **не** попадает (может нести password-hash). | `true`/`false` по diff `ACL LIST` до/после `LOAD` (совпал → `false`, no-op). |
| `cluster` | Управление hash-slot-кластером (16384 слота) через `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE`/`FORGET`/`SETSLOT`/`MIGRATE`/`FAILOVER`. Реализованы `action: create` (сборка с нуля), `add-node` (присоединение ноды в эксплуатации), `remove-node` (вывод ноды в эксплуатации, с миграцией слотов master-а), `reshard` (перенос N слотов master→master, в эксплуатации) и три шага live-миграции между кластерами: `join-external` (влить новые ноды репликами старого кластера 1:1), `failover-takeover` (промоушен новых реплик в мастера через graceful failover), `forget-external` (выкинуть старые узлы). | `create`/`add-node`/`remove-node`/`join-external`/`failover-takeover`/`forget-external` идемпотентны: `true` при изменении, `false` (no-op) на сошедшемся входе. **`reshard` НЕ идемпотентен** (см. ниже): `true` при успешном переносе, `failed` при ошибке ввода; no-op-ветки нет. |
| `replica` | Привязать инстанс к master-у через `REPLICAOF` (+ `CONFIG SET masterauth`). Опц. `source_external: true` — привязка к **внешнему** master-у (миграция) с отдельными реквизитами `master_*`. | `true` при настройке; `false` (no-op), если уже реплика нужного master-а или `addr == master_addr` (сам master, guard отключён при `source_external`). |
| `detached` | Отвязать инстанс от master-а через `REPLICAOF NO ONE`, промоутя в самостоятельный master. Финальный шаг миграции из внешнего источника (после `offset-synced` подтвердил догонку). Идемпотентен: уже master → no-op. | `true` при промоушене; `false` (no-op), если инстанс уже master. |
| `sentinel` | Реконсилировать Redis Sentinel (`SENTINEL MONITOR`/`REMOVE`/`SET`/`CONFIG SET`). Алгоритм 1:1 из Ansible `redis_sentinel_update.py`. | `true` при изменении монитора/параметров; `false` (no-op), если всё совпало. |

## command — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis: `host:port` (TCP) или `unix:/path` (unix-сокет). |
| `args` | list | required | Команда массивом аргументов **без shell**: `["CONFIG","SET","maxmemory","256mb"]`. Первый элемент — verb. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref в operator-input, keeper резолвит до Apply (см. «Пароль»). Маскируется. |
| `username` | string | optional | ACL-username для `AUTH` (если не default-user). |
| `db` | int | optional (default `0`) | Номер БД (`SELECT`) перед командой. |
| `changed` | bool | optional (default `false`) | Пометить результат `changed=true` (probe-семантика по умолчанию). |

## pinged — params

Health-probe через go-redis `PING` (ожидает `PONG`). **Read-only**, `changed=false`
конструктивно. Заменяет idiom `command args:[PING]`: ответ сервера кладётся в тот же
`Output.result`, поэтому `register.self.result == 'PONG'` в health-gate
(`retry`/`until`/`failed_when`) работает без правок. Используется как health-gate
перед привязкой реплик / сборкой кластера / настройкой sentinel. Ошибка `PING`
(`LOADING`/`MASTERDOWN`/…) → `failed` (ответ сервера, не секрет).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis: `host:port` (TCP) или `unix:/path`. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref в operator-input, keeper резолвит до Apply (см. «Пароль»). Маскируется; в аргументы `PING` не передаётся. |
| `username` | string | optional | ACL-username для `AUTH` (если не default-user). |
| `db` | int | optional (default `0`) | Номер БД (`SELECT`) перед `PING`. |

**Output**: `result` — ответ сервера (`PONG`).

## role — params

Role-probe через go-redis `INFO replication` — фактическая **волатильная** роль
инстанса. **Read-only**, `changed=false` конструктивно. Заменяет shell-idiom
`redis-cli role | head -1 | tr -d '\n'`: `Output.role` несёт `master`/`slave` (те же
значения, что отдавал `redis-cli role`). Используется для `where`-таргетинга
rolling-restart (`register.self.role == 'master'`/`'slave'`); роль волатильна
(ADR-008), берётся живым probe перед таргетингом, **не** из `incarnation.state`.
`INFO replication` без поля `role` (обрезанный INFO / сломанный инстанс) → `failed`
(а не пустая роль, тихо никого не таргетящая).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis: `host:port` (TCP) или `unix:/path`. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref, keeper резолвит до Apply (см. «Пароль»). Маскируется. |
| `username` | string | optional | ACL-username для `AUTH` (если не default-user). |
| `db` | int | optional (default `0`) | Номер БД (`SELECT`) перед `INFO`. |

**Output**: `role` — фактическая роль инстанса, `master` либо `slave`.

## replica-synced — params

Probe ресинка реплики через go-redis `INFO replication`: проверяет
`master_link_status == "up"` (реплика **догнала** master-а после рестарта).
**Read-only**, `changed=false` конструктивно. Строже `pinged` (`PONG` означает лишь,
что демон жив, но реплика могла ещё не догнать master). `Output.synced` (bool) —
условие для health-gate (`until: register.self.synced == true`);
`Output.master_link_status` (строка) — для диагностики.

> **★ Только slave-путь.** Поле `master_link_status` присутствует в `INFO
> replication` **только** у реплики (`role:slave`) — у master-а его нет. State
> предназначен для slave-пути rolling-restart (`block.where` slave). Если поля нет
> (инстанс — master либо нештатный INFO) → `synced=false` с явной причиной в
> `Message` (**не** тихий success): иначе health-gate реплики молча прошёл бы на
> инстансе, который ещё не реплика.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis: `host:port` (TCP) или `unix:/path`. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref, keeper резолвит до Apply (см. «Пароль»). Маскируется. |
| `username` | string | optional | ACL-username для `AUTH` (если не default-user). |
| `db` | int | optional (default `0`) | Номер БД (`SELECT`) перед `INFO`. |

**Output**: `synced` (bool) — `true` ⇔ `master_link_status == "up"`;
`master_link_status` (строка) — сырой статус линка для диагностики (`""`, если поля
нет — инстанс не реплика).

## offset-synced — params

Safety-gate миграции реплики из **внешнего** источника через go-redis. Строже
`replica-synced`: «link жив ≠ данные догнаны». Сверяет `slave_repl_offset` **своего**
инстанса (`addr`) с `master_repl_offset` **внешнего** master-а (**второй** коннект к
`source_addr` со `source_*`-реквизитами — он авторитетный «head» для расчёта lag).
**Read-only**, `changed=false` конструктивно. `Output.caught_up` (bool) — условие
health-gate (`until: register.self.caught_up == true`) финального `detached`-шага
миграции. Используется после `replica source_external` (см. миграционный контракт
[«source_external» ниже](#replica--params-source_external)).

`caught_up=true` **только** когда одновременно: `master_link_status == "up"` +
`master_sync_in_progress == 0` (нет идущей full-sync) + `lag_bytes <= lag_threshold`.
Без обоих offset-ов (свой `addr` не реплика или `source_addr` не master) — `lag`
неопределён → `caught_up=false` (нештатный ввод, не тихий success).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес **своего** инстанса (реплики): `host:port` или `unix:/path`. |
| `source_addr` | string | required | Адрес **внешнего** master-источника `host:port` — второй коннект (источник `master_repl_offset`). |
| `password` | string (secret) | optional | Пароль **своего** инстанса. vault-ref, keeper резолвит до Apply. Маскируется. |
| `source_password` | string (secret) | optional | Пароль **внешнего** источника для второго коннекта. vault-ref, keeper резолвит до Apply. Маскируется. |
| `lag_threshold` | int | optional (default `0`) | Допустимое отставание (`master_repl_offset − slave_repl_offset`) в **байтах** для `caught_up=true`. `0` — строгая полная догонка. |
| `skip_checksum` | bool | optional (default `false`) | Пропустить опц. сверку `DBSIZE` обоих инстансов. По умолчанию `DBSIZE` источника и реплики кладутся в Output как вспомогательный сигнал (на `caught_up` **не** влияют — авторитет offset). |
| `tls` / `tls_ca` | — | optional | TLS-коннект к **своему** инстансу (только `tls` + `tls_ca`; mTLS-пары у этого state нет). |
| `source_tls` / `source_tls_ca` | — | optional | TLS-коннект к **внешнему** источнику (второй коннект). `source_tls_ca` — PEM CA источника (secret). |

**Output**: `caught_up` (bool) — итоговое условие догонки; `lag_bytes` (int64) —
`master_repl_offset − slave_repl_offset` (отрицательный клампится в `0` —
read-after-write-окно, не отрицательный lag); `master_sync_in_progress` (bool) — идёт
ли full-sync; при `!skip_checksum` дополнительно `dbsize_source` / `dbsize_replica`
(грубый sanity-чек размеров — разный DBSIZE на ходу нормален из-за TTL/eviction, на
`caught_up` не влияет).

## config — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis (как у `command`). |
| `config` | map | required | Директивы `redis.conf`: `{ maxmemory: "256mb", maxmemory-policy: allkeys-lru }`. Каждая (кроме startup-only — см. ниже) → `CONFIG SET <key> <value>`. Применяются в детерминированном порядке (по ключу). Числовые значения стрингифицируются (`20000`, не `20000.000000`). |
| `password` | string (secret) | optional | См. «Пароль». |
| `username` | string | optional | ACL-username для `AUTH`. |
| `rewrite` | bool | optional (default `false`) | После `CONFIG SET` выполнить `CONFIG REWRITE` (персист в `redis.conf`). |

**Honest diff (идемпотентность).** Перед каждым `CONFIG SET <key>` плагин делает
`CONFIG GET <key>` и шлёт `SET` **только** при реальном расхождении значения.
Совпало для всех → `changed=false` (no-op). Это даёт повторному `update_config`
идемпотентность на стороне плагина.

> **★ Startup-only-директивы пропускаются (денилист).** Часть директив `redis.conf`
> задаётся **только при старте процесса** — `CONFIG SET` их отвергает («can't set …
> at runtime» / «Unknown option»). Операционный `update_config` рендерит **полный** `redis.conf`
> (включая такие директивы — они нужны при следующем рестарте процесса) и передаёт
> плагину **весь** `config`-map; плагин такие ключи **пропускает** (не падает на них),
> hot-settable применяет как обычно. Смена startup-only-директивы вступит в силу при
> **следующем рестарте** процесса (его триггерит смена hardening-юнита, destiny
> [`redis/tasks/server.yml`](../../../../examples/destiny/redis/tasks/server.yml)).
> Денилист (`startupOnlyDirectives` в [`impl.go`](../../../../examples/module/soul-mod-community-redis/impl.go)):
> `port` · `tls-port` · `bind` · `unixsocket` · `unixsocketperm` · `io-threads` ·
> `io-threads-do-reads` · `cluster-enabled` · `cluster-config-file` · `aclfile` ·
> `logfile` · `pidfile` · `dir` · `daemonize` · `supervised` · `dbfilename` ·
> `loadmodule` · `syslog-enabled` · `syslog-ident` · `syslog-facility` · `databases` ·
> `always-show-logo` · `set-proc-title` · `locale-collate` · `socket-mark-id`.

**Output**: `applied` (CSV применённых директив) · `count` (число применённых) ·
`rewrite` (выполнялся ли `CONFIG REWRITE`) · `skipped` (CSV пропущенных startup-only) ·
`skippedCount` (их число — для аудита). Значения директив идут в Output (это конфиг
redis, не секрет); error-path всё равно санитизируется `redactError` по значению
(директива могла прийти из Vault, напр. `requirepass`).

## acl — params

Hot-reload ACL живого Redis: `ACL LOAD` заставляет инстанс перечитать `aclfile`
**целиком**. `users.acl` рендерит destiny `redis` **до** этого шага (через
`core.file.rendered`, plaintext-пароль не пишется — `.tmpl` хеширует) — `acl` лишь
заставляет Redis перечитать готовый файл. **Идемпотентен по конструкции**: `ACL LOAD`
приводит живой инстанс к декларированному файлу независимо от текущего состояния.

`changed`-семантика: сама `ACL LOAD` «changed» не сообщает, поэтому плагин делает
дешёвый честный diff — `ACL LIST` **до и после** `LOAD` (типизированный путь, строка
на пользователя; порядок значим). Совпали → `changed=false` (живой инстанс уже
совпадал с файлом, no-op — симметрия с `config`/`cluster`/`sentinel`); отличаются →
`changed=true`.

Params — **только коннект** (`addr` + опц. `auth`/`db`/TLS), как у read-probe:
никаких acl-специфичных полей (файл — источник правды, его рендерит destiny).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес Redis: `host:port` (TCP) или `unix:/path`. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref, keeper резолвит до Apply (см. «Пароль»). Маскируется; в аргументы ACL-команд **не** передаётся (уходит только в коннект). |
| `username` | string | optional | ACL-username для `AUTH` (если не default-user). |
| `db` | int | optional (default `0`) | Номер БД (`SELECT`) перед `ACL LOAD`. |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | Общие TLS-параметры коннекта (см. [«TLS-коннект»](#tls-коннект-концепция-ansible-роли-redis-redis_tls_)). |

**Output**: `users` — число ACL-пользователей **после** `LOAD`. Сам вывод `ACL LIST`
(правила пользователей) в Output **НЕ** попадает (ИБ): строка пользователя может
нести password-hash (`>hash` / `#sha256`). `ACL LOAD` фейлит при битом `aclfile` /
не сконфигурированном `aclfile` — это ответ Redis (не секрет оператора), идёт в
`Message` как `failed`. Без dry-run preview (плагин не реализует `PlanReadSafe`).

## cluster — params

Управляет Redis-кластером **целиком через go-redis** (никакого `redis-cli`/shell).
Операция выбирается полем `action`. Реализованы:

- развёртывание и эксплуатация над **своим** кластером: `create` (сборка с нуля), `add-node`
  (присоединение одной ноды), `remove-node` (вывод одной ноды), `reshard` (перенос
  N слотов master→master);
- три шага **live-миграции между кластерами** (старый → новый, без даунтайма):
  `join-external` (влить новые cluster-mode ноды репликами старого кластера 1:1),
  `failover-takeover` (промоушен новых реплик в мастера через graceful failover),
  `forget-external` (выкинуть старые узлы).

> **★ Идемпотентность.** `create`/`add-node`/`remove-node`/`join-external`/
> `failover-takeover`/`forget-external` **идемпотентны** — повторный apply на
> сошедшемся входе даёт `changed=false` (no-op), их безопасно держать в converge.
> **`reshard` — НЕТ.** Это императивная **exec-style** операция (без `unless`):
> повторный apply сдвинет **ещё** `slots` слотов с `from` на `to`. Оператор зовёт
> reshard **явно**, ровно столько раз, сколько нужно переносов; reshard **не** часть
> converge-цикла.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `action` | string | required | `create` / `add-node` / `remove-node` / `reshard` (**НЕ идемпотентен**) / `join-external` / `failover-takeover` / `forget-external`. |
| `password` | string (secret) | optional | См. «Пароль». Применяется при коннекте к **каждой** ноде (`create`) / к `new_node`+`seed`+`master` (`add-node`) / к `node`+`seed`+оставшимся masters (`remove-node`) / к `from`+`to` (`reshard`) / к новым `nodes`+`source_nodes` (`join-external`/`failover-takeover`/`forget-external` — **общий** пароль старого и нового кластера). |
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
> `slots` слотов с `from` на `to` — это императивная exec-style операция,
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

### cluster — live-миграция между кластерами (`join-external` → `failover-takeover` → `forget-external`)

Три шага переносят нагрузку со **старого** cluster-mode кластера на **новый** без
даунтайма (зеркало ручного `redis-cli --cluster add-node`/`--cluster failover`/
`--cluster del-node`, целиком через go-redis). Оба кластера — в **одной** сети под
**одним** паролем/TLS (оператор выравнивает `новый == старый` до миграции). Порядок
строгий: `join-external` (реплики догоняют старых мастеров) → `failover-takeover`
(промоушен, слоты переезжают на новые) → `forget-external` (старые узлы забыты).
Реализация — [`migrate.go`](../../../../examples/module/soul-mod-community-redis/migrate.go).

**`join-external`** — влить новые ноды в старый кластер и сделать каждую репликой
старого мастера **1:1**: коннект к `source_nodes` → `CLUSTER NODES` старого кластера
→ маппинг новый-узел↔старый-мастер (узлы по ключу `nodes`, мастера по первому слоту)
→ `CLUSTER MEET` old-seed + waitConverge + `CLUSTER REPLICATE` на **каждом** новом
узле. **Fail-fast**: число старых мастеров `!= shards_dest` → 1:1 невозможен
(runtime-assert, `shards_source` render-фазе не виден). Идемпотентен: узел уже
реплика нужного мастера → no-op.

**`failover-takeover`** — промоутить новые узлы (реплики старых мастеров после
`join-external`) в мастера через **graceful** `CLUSTER FAILOVER`. **Сначала
sync-gate**: на **каждом** новом узле `INFO replication` `master_link_status == up`
(реплика догнала старого мастера) — хоть один не догнал → **ошибка до первого
failover** (ранний failover теряет хвост). Затем на каждом узле graceful `CLUSTER
FAILOVER` (без аргументов: мастер останавливает запись + досылает хвост, лосслесс) →
poll до `role==master` со слотами. **Fail-closed**: graceful не сошёлся за лимит →
**ошибка, БЕЗ** эскалации на `FORCE`/`TAKEOVER` (иначе split-brain). Идемпотентен:
узел уже master → no-op.

**`forget-external`** — выкинуть старые узлы: коннект к `source_nodes` → `CLUSTER
NODES` старого кластера → **все** старые node-id (мастера **и** реплики) → `CLUSTER
FORGET <old-id>` на **каждой** новой ноде. **Без** миграции слотов (слоты уже у новых
мастеров после `failover-takeover`). Идемпотентен: старый id уже неизвестен ноде
(`Unknown node`) → глотается как no-op. Decommission самих старых хостов (остановка
redis, чистка `nodes.conf`) — **вне** этой операции.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `nodes` | map | required | **Новые** узлы (map стабильный-ключ → `{ addr }` / `{ ip, port }`). `join-external` маппит их 1:1 на старых мастеров (ключи ↔ мастера), `failover-takeover` промоутит, `forget-external` исполняет на каждом `CLUSTER FORGET`. |
| `source_nodes` | list | required | Seed-ноды **старого** кластера (список `host:port`). Перебираются по порядку — первая ответившая `CLUSTER NODES` задаёт топологию. Тот же пароль/TLS, что у новых нод. |
| `shards_dest` | int | required (`join-external`) | Ожидаемое число шардов назначения (`>= 1`). Обязано совпасть И с числом новых узлов (`nodes`), И с числом мастеров старого кластера — иначе 1:1-маппинг невозможен (fail-fast; assert в Apply, т.к. `shards_source` виден только в живой топологии). |

## replica — params

Привязывает инстанс к master-у через `REPLICAOF` (go-redis). `masterauth`
ставится `CONFIG SET` **до** `REPLICAOF` (реплика обязана знать пароль master-а).
Идемпотентен (`INFO replication`): уже реплика нужного master-а со здоровым
линком → `changed=false`, no-op.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес **этого** инстанса: `host:port` или `unix:/path`. На redis-хосте локальный (`127.0.0.1:6379`). |
| `master_addr` | string | required | Адрес master-а `host:port`. **HOST-ИНВАРИАНТНЫЙ** (один на кластер) — scenario резолвит его run_once (`soulprint.hosts[0]`). `addr == master_addr` → инстанс есть master, `changed=false`, no-op (guard в плагине, чтобы scenario звал `replica` на всех хостах; при `source_external: true` guard **отключён**). |
| `password` | string (secret) | optional | Пароль master-а **своей** инкарнации. Ставится как `masterauth` до `REPLICAOF`. Пустой → `masterauth` не ставится. См. «Пароль». При `source_external: true` `masterauth` берётся **не** отсюда, а из `master_password`. |
| `username` | string | optional | ACL-username для репликации своей инкарнации (`CONFIG SET masteruser`). При `source_external: true` `masteruser` берётся из `master_username`. |

### replica — params (`source_external`)

Привязка к **внешнему** master-у (чужая инкарнация / миграция), а не к хосту своей
инкарнации — первый шаг миграции данных со старого Redis. При `source_external: true`:
(1) self-guard `addr == master_addr` **отключён** (внешний адрес заведомо не свой);
(2) `masterauth` берётся из `master_password` (не `password`); (3) `masteruser` — из
`master_username`. Дальше миграция идёт через `offset-synced` (догонка) → `detached`
(промоушен). TLS исходящего replication-линка к источнику включается `master_tls`.

> **★ TLS-линк к источнику требует render на диске.** `master_tls: true` включает
> `CONFIG SET tls-replication yes` **до** `REPLICAOF`, но CA/cert/key источника Redis
> читает **с диска по пути**, не inline. Плагин файлы **не** пишет: `master_tls_ca`/
> `master_tls_cert`/`master_tls_key` должен положить на диск реплики scenario (через
> `core.file.rendered`) и указать пути через `config`-state (`tls-ca-cert-file`/
> `tls-cert-file`/`tls-key-file`) **до** `replica`-шага. Иначе верификация server-cert
> источника при handshake провалится. Сами PEM-значения плагин в путь не преобразует.

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `source_external` | bool | optional (default `false`) | `master_addr` указывает на внешний master (миграция). `true` — включает `master_*`-реквизиты и снимает self-guard. |
| `master_password` | string (secret) | optional | Пароль внешнего master-источника (`CONFIG SET masterauth`). vault-ref, keeper резолвит до Apply. Маскируется. Пустой → `masterauth` не ставится. |
| `master_username` | string | optional | ACL-username внешнего источника для репликации (`CONFIG SET masteruser`). |
| `master_tls` | bool | optional (default `false`) | Источник принимает реплику по TLS. `true` → `CONFIG SET tls-replication yes` до `REPLICAOF`. Требует render CA/cert источника на диск (см. врезку). |
| `master_tls_ca` | string (secret, PEM) | optional | PEM CA внешнего источника (проверка его server-cert на replication-линке). Маскируется. Кладётся на диск render-ом, путь — через `config`-state. |
| `master_tls_cert` / `master_tls_key` | string (secret, PEM) | optional | PEM client-cert/key реплики для mTLS на replication-линке к источнику (только вместе). Маскируется. Применяются как `tls-cert-file`/`tls-key-file` render-ом, не плагином. |

## detached — params

Отвязывает инстанс от master-а через `REPLICAOF NO ONE` (go-redis), промоутя его в
самостоятельный master. **Финальный** шаг миграции из внешнего источника — после
`offset-synced` подтвердил догонку (`caught_up == true`). Идемпотентен (`INFO
replication`): инстанс уже `role == master` → `changed=false`, no-op (безопасен к
повтору). Реализация — [`detach.go`](../../../../examples/module/soul-mod-community-redis/detach.go).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `addr` | string | required | Адрес **этого** инстанса: `host:port` или `unix:/path`. |
| `password` | string (secret) | optional | Пароль Redis. vault-ref, keeper резолвит до Apply. Маскируется; в аргументы `REPLICAOF` не передаётся (уходит только в коннект). |
| `username` | string | optional | ACL-username для коннекта (если не default-user). |
| `tls` / `tls_ca` / `tls_cert` / `tls_key` / `tls_skip_verify` | — | optional | Общие TLS-параметры коннекта (см. [«TLS-коннект»](#tls-коннект-концепция-ansible-роли-redis-redis_tls_)). |

**Output**: `changed` (bool) — был ли инстанс промоутнут; `previous_master`
(строка `host:port`) — прежний master для аудита (`""`, если инстанс уже был master
или поля `master_host`/`master_port` отсутствовали).

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

Код-инвариант (проверяется L0): `params["password"]` (и `auth_pass` в `sentinel`,
`master_password` в `replica source_external`, `source_password` в `offset-synced`)
**никогда** не попадают в `ApplyEvent.Message`/`.Output`, в текст ошибок и в
stderr. Ошибки коннекта санитизируются (`redactError` вырезает подстроку
пароля — при втором коннекте `offset-synced` редактирует **оба** пароля); вывод
самих команд (`result`) — это ответ сервера, не секрет оператора. В `sentinel`
Output несёт только **имена** применённых действий (`sentinel_monitor`/`sentinel_set`/…),
не их секретные значения; `replica` ставит `masterauth` аргументом `CONFIG SET`
(нужно Redis для синхронизации), но в события его не пишет.

## TLS-коннект (концепция Ansible-роли redis: `redis_tls_*`)

Все states (`command`/`pinged`/`role`/`replica-synced`/`offset-synced`/`config`/`acl`/
`cluster`/`replica`/`detached`/`sentinel`) принимают **общие** TLS-параметры коннекта.
По умолчанию TLS выключен (plaintext, back-compat); при `tls: true` плагин коннектится
к Redis по TLS. Два state имеют **второй** набор TLS-параметров для внешнего линка:
`offset-synced` — `source_tls`/`source_tls_ca` (коннект к внешнему источнику),
`replica source_external` — `master_tls`/`master_tls_ca`/`master_tls_cert`/`master_tls_key`
(исходящий replication-линк реплики к источнику; см. врезку в [«replica `source_external`»](#replica--params-source_external)).

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
  (для `cluster` — коннект к каждой ноде из `nodes`, для cluster live-миграции — ещё
  и к `source_nodes` старого кластера; для `offset-synced` — **второй** коннект к
  внешнему `source_addr`; для `replica source_external` — исходящий replication-линк
  к внешнему master-у). **Без** `vault_access` (пароль и PEM резолвит Keeper), **без**
  `exec_subprocess` / `fs_write_root` (плагин не запускает подпроцессы — `cluster`
  идёт целиком через go-redis, не через `redis-cli` — и не пишет на FS; файлы CA/cert
  внешнего источника для TLS-миграции кладёт scenario через `core.file.rendered`).
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

Миграция из внешнего Redis (три шага, health-gate по `caught_up`):

```yaml
# 1. Привязать локальный инстанс репликой к ВНЕШНЕМУ master-у.
- name: Replicate from external source
  module: community.redis.replica
  params:
    addr: "127.0.0.1:6379"
    master_addr: "${ input.source_addr }"
    source_external: true
    master_password: "${ vault('secret/redis/legacy#password') }"

# 2. Дождаться полной догонки данных (не просто живого линка).
- name: Wait until caught up with source
  module: community.redis.offset-synced
  register: sync
  until: register.self.caught_up == true
  retry: { attempts: 60, delay: 5 }
  params:
    addr: "127.0.0.1:6379"
    source_addr: "${ input.source_addr }"
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
    source_password: "${ vault('secret/redis/legacy#password') }"

# 3. Отвязать и промоутить в самостоятельный master (финал миграции).
- name: Detach and promote to master
  module: community.redis.detached
  params:
    addr: "127.0.0.1:6379"
    password: "${ vault('secret/redis/' + incarnation.name + '#password') }"
```

## Тесты

- **L0 command/config/acl**
  ([`impl_test.go`](../../../../examples/module/soul-mod-community-redis/impl_test.go)):
  fake `redisConn` + fake `ApplyEvent`-stream. Покрывает `Validate` (пустой
  addr/args/config, acl требует addr, нереализованный state), Apply happy-path
  command/config, unix-socket-парсинг, `changed`-семантику, стрингификацию числовых
  значений, `CONFIG REWRITE`; **startup-only-денилист** (`config` пропускает
  `port`/`dir`/`aclfile`/`cluster-enabled`/`loadmodule` — ни `CONFIG GET`, ни `SET` по
  ним не вызваны, `skipped`/`skippedCount` в Output корректны; все-startup-only →
  `changed=false`, ни одного `SET`); **acl** (`ACL LOAD` шлётся между `ACL LIST`
  до/после, `changed=true` при diff / `false` при совпадении, ошибка `LOAD`/`LIST` →
  `failed`); и **ИБ-инвариант** — пароль не утекает ни в события, ни в аргументы
  команд, ни в санитизированную ошибку коннекта.
- **L0 probe (pinged/role/replica-synced)**
  ([`probe_test.go`](../../../../examples/module/soul-mod-community-redis/probe_test.go)):
  fake `redisConn`. Покрывает `Validate` (пустой `addr`); `pinged` happy-path
  (`PING` → `Output.result == 'PONG'`, `changed=false`), ошибка `PING` → `failed`;
  `role` happy-path (`INFO replication` → `Output.role` = `master`/`slave`,
  `changed=false`), `INFO replication` без поля `role` → `failed`; `replica-synced`
  (`master_link_status: up` → `synced=true`; поле отсутствует → `synced=false` с
  причиной); **ИБ-инвариант** (пароль не течёт в события/санитизированную ошибку
  коннекта).
- **L0 offset-synced**
  ([`offset_synced_test.go`](../../../../examples/module/soul-mod-community-redis/offset_synced_test.go)):
  fake `redisConn` (свой + внешний источник). Покрывает `Validate` (требует `addr` +
  `source_addr`, отвергает отрицательный `lag_threshold`); `caught_up=true` при
  догонке; `lag > threshold` / `lag <= threshold`; `master_sync_in_progress` → не
  caught_up; `link down` → не caught_up; отсутствие offset-а → не caught_up; опц.
  `DBSIZE`-checksum и `skip_checksum`; что **второй** коннект использует `source_*`-
  реквизиты и `source_tls` (независимо от своего TLS); **ИБ-инвариант** (ни свой, ни
  source-пароль не течёт — в т.ч. при фейле второго коннекта).
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
  (ни одной команды); пустой пароль → `masterauth` не ставится; `source_external`
  (self-guard снят, `masterauth`/`masteruser` из `master_*`, `tls-replication yes`
  при `master_tls`); **ИБ-инвариант** (ни `password`, ни `master_password` не течёт
  в события/санитизированную ошибку коннекта).
- **L0 detached**
  ([`detach_test.go`](../../../../examples/module/soul-mod-community-redis/detach_test.go)):
  fake `redisConn` со скриптованным `INFO replication`. Покрывает `Validate` (пустой
  `addr`); slave → `REPLICAOF NO ONE` + `changed=true` + `previous_master` в Output;
  уже master → no-op (`changed=false`, ни одной команды); ошибка `INFO` → `failed`;
  **ИБ-инвариант** (пароль не течёт в санитизированную ошибку коннекта).
- **L0 cluster live-миграция (join-external/failover-takeover/forget-external)**
  ([`migrate_test.go`](../../../../examples/module/soul-mod-community-redis/migrate_test.go)
  + [`migrate_failover_test.go`](../../../../examples/module/soul-mod-community-redis/migrate_failover_test.go)):
  fake-флот нод (новые + старый кластер по `source_nodes`). `join-external`:
  `Validate` (пустые `nodes`/`source_nodes`/невалидный `shards_dest`); happy 1:1-
  маппинг узлы↔мастера (по **первому слоту**, не node-id); fail-fast при mismatch
  числа мастеров / числа узлов и `shards_dest`; идемпотентность (узел уже реплика →
  no-op), partial-идемпотентность; failover seed-нод к следующей; no-leak при фейле
  source-коннекта. `failover-takeover`: **sync-gate** блокирует до **первого**
  failover, если хоть один узел не догнал; **fail-closed** без эскалации на
  `FORCE`/`TAKEOVER`; идемпотентность (узел уже master → no-op), partial. `forget-
  external`: `FORGET` всех старых id на **каждой** новой ноде; не форгетит себя
  (`Cant forget self` глотается); **без** миграции слотов; `Unknown node` → no-op;
  seed-failover и «все seed-ы легли» → `failed`. Во всех — **ИБ-инвариант** (пароль
  не течёт).
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
  scenario `create` (standalone/cluster/sentinel), `add_node`, `remove_node`,
  `reshard` (**НЕ идемпотентен**) и hot-reload `update_config`
  (→ state `config`), `add_user` (→ state `acl`), `rotate_tls` (→ state `command`,
  force re-read SSL_CTX), `migrate_cluster` (→ `cluster` live-миграция + `replica`
  `source_external` + `offset-synced`), `detach_source` (→ `detached` + `offset-synced`)
  вызывают states этого плагина.
- [examples/destiny/redis/](../../../../examples/destiny/redis/) —
  режим-агностичный per-host кирпич (install + render `redis.conf` + systemd).
- [ADR-012](../../../adr/0012-keeper-soul-grpc.md) — render Keeper-side, пароль
  доезжает значением.
- [ADR-031 Scry](../../../adr/0031-scry-drift.md) — default-deny на dry_run без
  `PlanReadSafe`.
- [templating.md](../../../templating.md) — секрет-маскинг (§7.4).
