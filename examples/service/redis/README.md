# redis — единый Redis-сервис (концепция Ansible-роли)

Сервис «redis» — **один сервис на все режимы развёртывания** Redis, по концепции
Ansible-роли Redis. Режим выбирается полем `redis_type` (`sentinel` / `cluster`),
а не отдельным сервисом на каждый стек.

Оператор задаёт **простые типизированные понятия** (сколько памяти, какая
persistence, какая eviction-политика, какие ACL-юзеры), а сервис **транслирует**
их в детальный `redis_config` (полный `redis.conf`). Трансляция — через CEL
`merge()`: дефолты автора + persistence-пресет + вычисленный `maxmemory` +
passthrough-директивы оператора, SHALLOW last-wins ([templating.md §2.3](../../../docs/templating.md)).

> **Состояние (2026-06-25).** Сервис разворачивает **два** режима — **`sentinel`**
> (master-replica + sentinel-демон) и **`cluster`** (honest hash-slot Redis Cluster,
> 16384 слота) — оба в scenario `create`, выбор через `redis_type`
> ([`cluster.yml`](scenario/create/cluster.yml) и
> [`sentinel.yml`](scenario/create/sentinel.yml) включены в диспетчер). **Режимы
> `standalone` и `sentinel_only` из сервиса убраны** (решение пользователя
> 2026-06-25): standalone-развёртывание покрывает `sentinel` с
> `replicas_per_master: 0` (один master + sentinel-демон), а тонкий sentinel-слой
> над внешним master (`sentinel_only`) остаётся **capability destiny-кирпича
> [`redis`](../../destiny/redis/)** (флаги `deploy_redis` / `sentinel_enabled`) для
> переиспользования другими сервисами — например DragonFly, — но не как режим этого
> сервиса. Сужение enum `redis_type` до `[sentinel, cluster]` — bump
> `state_schema_version` 3→4 + миграция [`003_to_004`](migrations/003_to_004.yml)
> (forward-only remap старых режимов, см. [«state_schema»](#state_schema)).
>
> Реализованы day-2 cluster-операции — присоединение/вывод/решардинг ноды через
> отдельные scenario [`add_node`](scenario/add_node/main.yml) /
> [`remove_node`](scenario/remove_node/main.yml) /
> [`reshard`](scenario/reshard/main.yml) и rolling-restart
> [`restart`](scenario/restart/main.yml), а также day-2 hot-reload
> [`update_config`](scenario/update_config/main.yml) /
> [`add_user`](scenario/add_user/main.yml) /
> [`update_users`](scenario/update_users/main.yml) /
> [`rotate_tls`](scenario/rotate_tls/main.yml). Опционально `create` может в **том же
> прогоне** поднять VM под топологию через cloud-provision (`input.provision`,
> state_schema bumped 4→7, общее тело [`redis-provision.yml`](scenario/redis-provision.yml))
> — см. [«Cloud-provision»](#cloud-provision-create-поднимает-vm-live-ждёт-c1).
> **★ Cloud-provision — keeper-side реализован, живой провизион ждёт C1 + live-e2e.**
> Доставка bootstrap-токена реализована keeper-side (модуль `core.bootstrap.delivered`,
> [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md), SSH-доставка). Поток
> валиден на render (L0 Trial) и проходит unit-тесты, но живой провизион «VM→redis в
> один прогон» ещё **не пройдёт live-e2e** без слайса **C1** (cloud-init генерит CA-signed
> host-key, чтобы Keeper верифицировал host-cert свежей VM без TOFU) — подробности в
> разделе. Неизвестный `redis_type` (вне enum)
> отвергает **input-валидация Keeper-а ДО рендера** (понятный отказ, прогон не
> стартует) — прежний shell-mode-guard убран. Остальной backlog (sentinel
> failover/day-2, плагинный `failover`, sentinel-демон TLS) — см.
> [«В работе»](#в-работе). README описывает только то, что есть в файлах.

Разделение обязанностей (architect B-гибрид, ADR-009):

- **destiny [`redis`](../../destiny/redis/)** — режим-агностичный per-host кирпич:
  установка `redis-server` (диспетчер по `install.method`: distro-пакет **или**
  upstream-tarball), render `redis.conf` из **готового merged**-конфига, render
  `users.acl`, TLS-PEM (`core.file.present` + `${ vault(ref) }` в ячейке),
  systemd-hardening drop-in, host-tuning extras (THP/logrotate/sysctl), запуск
  сервиса. destiny «глупая» — ничего сама не сливает и не оркеструет; merge делает
  scenario сервиса.
- **scenario сервиса** — трансляция простого ввода в `redis_config` (через
  `merge()`) + оркестрация (порядок шагов, таргетинг, health-gate, размотка
  cluster-топологии в `nodes`-MAP; в будущих батчах — rolling-restart, day-2).
- **плагин [`community.redis`](../../../docs/module/community/redis/README.md)**
  (бинарь `soul-mod-community-redis`) — **основной интерфейс** к **живому** Redis
  (`CONFIG SET`, ACL, cluster, sentinel, failover, raw-команды). Подключён через
  [`service.yml → modules[]`](service.yml).

## state_schema

[`service.yml → state_schema`](service.yml), `state_schema_version: 10`. Цепочка
миграций forward-only (ADR-019):

- [`001_to_002.yml`](migrations/001_to_002.yml) — `redis_users` из списка имён в map
  `name → {perms, state}`;
- [`002_to_003.yml`](migrations/002_to_003.yml) — «richer state»: opaque
  `redis_config` дополнен именованными полями намерения (read-model day-2:
  `tls`/`install`/`persistence`/`memory_mb`/`maxmemory_policy`/`modules` + топология
  `shards`/`replicas`/`sentinel_quorum`);
- [`003_to_004.yml`](migrations/003_to_004.yml) — **сужение enum `redis_type` до
  `[sentinel, cluster]`**: forward-only remap старых режимов `standalone → sentinel`
  и `sentinel_only → sentinel`, чтобы state остался схема-валиден после удаления этих
  режимов из сервиса. Сам файл миграции несёт флаг `needs_architect` (★★): remap
  меняет живой смысл инкарнации (standalone не имел sentinel-демона; `sentinel_only`
  не имел data-плоскости) — корректность target-режима для живых
  standalone/`sentinel_only`-инкарнаций должна быть подтверждена до прода;
- [`004_to_005.yml`](migrations/004_to_005.yml) — тайминги failover sentinel:
  `redis_sentinel` дополнен `down_after_ms`/`failover_timeout_ms` (read-model day-2,
  default-роль `5000`/`60000`); только для sentinel-инкарнаций, для cluster (`redis_sentinel
  {}`) — no-op;
- [`005_to_006.yml`](migrations/005_to_006.yml) — `redis_users` из map `name→{perms,state}`
  в **типизированный массив `[{name, perms, state}]`** (ADR-062 `AclUser`): map-ключ
  (имя) переезжает в поле `name`;
- [`006_to_007.yml`](migrations/006_to_007.yml) — **cloud-provision read-model**
  ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md), Вариант A):
  state дополнен двумя независимыми полями `provisioned_vm_ids` (array string) и
  `provisioned_provider` (string). Существующим инкарнациям (развёрнутым **без** provision)
  — консервативный default `[]` / `''` (хосты — declared roster, не провиженные сервисом);
  два плоских `set` с `has()`-guard (back-compat + идемпотентность повторного прогона);
- [`007_to_008.yml`](migrations/007_to_008.yml) — **cascade-destroy read-model**
  (дополнение к v7): state дополнен `provisioned_sids` (array string) — Keeper-side
  SID/FQDN VM, поднятых нашим прогоном. Day-2 teardown читает их для cascade-чистки
  реестров `souls`/`soul_seeds`/`bootstrap_tokens` (Reaper не снимает pending-VM-souls
  сам). Пара с `provisioned_vm_ids`: `vm_ids` — provider-id (для `core.cloud.destroyed`),
  `sids` — Keeper-id Soul (для cascade) — разные подсистемы. Один плоский `set`, default
  `[]` существующим (`has()`-guard);
- [`008_to_009.yml`](migrations/008_to_009.yml) — **seed-source read-model** (пилот
  `migrate_cluster`): state дополнен вложенным объектом `seeded_from` — **откуда** залиты
  данные при миграции с внешнего кластера: `source_endpoints` (array string) и `detached`
  (bool — `false` после migrate, `true` после day-2 `detach_source`). Время намеренно не в
  state (детерминизм L0; `now()` в migration-CEL запрещён) — берётся из `state_history`.
  Обычному `create` (не миграция) — консервативный default `{source_endpoints: [],
  detached: false}`; один `set` с `has()`-guard на **весь объект**;
- [`009_to_010.yml`](migrations/009_to_010.yml) — `seeded_from` дополнен **ссылками** на
  учётку внешнего источника: `source_password_ref` (Vault-**путь** к паролю) +
  `source_tls_ca_ref` (Vault-путь к CA), оба string. Прод-фикс блокера: `detach_source`
  делает финальный offset-gate к **внешнему** источнику, но раньше учётка источника в state
  не хранилась (только в `input.source.*` у `migrate_cluster`) → коннект к AUTH/TLS-источнику
  падал → detach никогда не проходил (`error_locked`). Теперь `migrate` **персистит ссылки**
  (не секреты — Vault-пути), `detach` читает их `vault()`-ом keeper-side. Существующим
  v9-инкарнациям (и обычному `create`) — пустые ref'ы (источник без AUTH/TLS). **Лист-guard**
  (`has()` на каждое новое поле внутри уже существующего `seeded_from`), не объект-guard как
  `008_to_009` — иначе перезатёрло бы `source_endpoints`/`detached`.

`incarnation.state` фиксирует, что развёрнуто, чтобы оператор видел инсталляцию, а
повторный apply был идемпотентен:

| поле | тип | смысл |
|---|---|---|
| `redis_type` | enum `sentinel`/`cluster` | режим развёртывания (реализованы оба) |
| `redis_version` | string | эффективная версия Redis: distro-native пин (`install.method=package`, из input `version`) **или** upstream-semver (`install.method=binary`, из `install.version`); см. input `version` / `install` |
| `redis_config` | object | **итог трансляции** — merged-конфиг `redis.conf` (default → preset → вычисления → passthrough; для `cluster` — плюс `cluster-*`-директивы) |
| `redis_users` | array `AclUser` (`[{name, perms, state}]`) | **operator-extra** ACL-пользователи Redis (только заведённые оператором). Элемент — типизированный `AclUser` (`name` + `perms` обязательны, `state` дефолт `on`) из [`types.yml`](types.yml), переиспользуемый через `$type: AclUser` в `input:` сценариев (ADR-062). До state_schema v6 это был map `username → {perms, state}` — миграция [`005_to_006.yml`](migrations/005_to_006.yml) свернула map→array (имя-ключ → поле `name`). `perms` — полная ACL-строка (пароли НЕ в state — keeper-side Vault). **Системные** служебные юзеры (`replica`/`monitoring`/`sentinel`/`haproxy` и т.д.) сюда **НЕ** пишутся — они доливаются в `users.acl` из `essence.system_acl_users` на каждом рендере (см. [«Системные ACL-юзеры»](#системные-acl-юзеры)) |
| `redis_hosts` | array `{sid, role}` | хосты топологии (пишется `[]`; точные роли `primary`/`replica`/`sentinel` для cluster/sentinel раскладывает apply-сторона — в state не фиксируются) |
| `redis_sentinel` | object `{master_name, master_ip, quorum, down_after_ms, failover_timeout_ms}` | факты sentinel-режима: имя monitored master (из `essence.sentinel_master_name`, дефолт `master`) + quorum + тайминги failover (v5, default `5000`/`60000`). `quorum` всегда `0` (auto `size/2+1` вычисляется в apply, в state не материализуется); `master_ip` материализуется только для убранного режима `sentinel_only` (внешний master). Вне режима `sentinel` — пустой объект |
| `provisioned_vm_ids` | array string | **(v7, cloud-provision read-model, [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md))** provider-vm-id VM, поднятых **этим** create-прогоном через `core.cloud.created` (из `register.provision.vm_ids`). Day-2 teardown читает их для `core.cloud.destroyed`. Без provision (`input.provision` опущен/`enabled:false`) — `[]` (хосты — declared roster, не провиженные сервисом). См. [«Cloud-provision»](#cloud-provision-create-поднимает-vm-live-ждёт-c1) |
| `provisioned_provider` | string | **(v7)** имя cloud-Provider в реестре (тот же `destroy`-вызов; из `input.provision.provider`). Без provision — `''` |
| `provisioned_sids` | array string | **(v8)** Keeper-side SID/FQDN VM, поднятых **этим** прогоном. `destroy` читает их для cascade-чистки реестров `souls`/`soul_seeds`/`bootstrap_tokens` (читается **парой** с `provisioned_vm_ids` — длины обязаны совпадать, иначе orphan; `destroy` несёт `assert`-guard на парность). Без provision — `[]` |
| `seeded_from` | object `{source_endpoints, detached, source_password_ref, source_tls_ca_ref}` | **(v8/v10)** **откуда** залиты данные при `migrate_cluster`: `source_endpoints` (array string — внешний источник), `detached` (bool — `true` после `detach_source`), `source_password_ref`/`source_tls_ca_ref` (Vault-**пути** учётки источника, **не** секреты — v10). Обычному `create` (не миграция) — `{source_endpoints: [], detached: false}` + пустые ref'ы |

Кроме перечисленных, `state_schema` несёт **именованные поля намерения** (read-model,
v3): `tls` / `install` / `persistence` / `memory_mb` / `maxmemory_policy` / `modules`
(read-model развёрнутых модулей) / `modules_base_url` / `conf_dir` / `data_dir` /
`sysctl_settings` + counts `shards` / `replicas` / `sentinel_quorum`. Они согласованы
с `redis_config` по построению (один `compute`-проход — см.
[«Трансляция»](#трансляция-простого-ввода-в-redis_config)), а day-2-сценарии читают их
**именованно**, не парся `redis_config`.

`required: [redis_type, redis_config]`.

## Входной контракт оператора

[`scenario/create/main.yml → input:`](scenario/create/main.yml) — строго
типизированный структурный ввод (Named Dict, каждый параметр чёткого типа), **не**
свободный текст:

| поле | тип | смысл |
|---|---|---|
| `redis_type` | enum `sentinel`/`cluster`, default `sentinel` | режим; выбирает ветку диспетчера. `sentinel` — master-replica + sentinel-демон (с `replicas_per_master: 0` — standalone-эквивалент); `cluster` — honest hash-slot Redis Cluster. Значение вне enum отвергает input-валидация Keeper-а по enum ДО рендера |
| `version` | string, обяз. **только при `install.method=package`** (`required_when`) | distro-native пин версии пакета (напр. `5:7.0.15-1~deb12u7`); `core.pkg` всегда ставит `=version` — воспроизводимая инсталляция. Поведение «не задана → latest из репо» удалено (директива пользователя 2026-06-23). При `install.method=binary` НЕ используется — версия бинаря в `install.version` (upstream-semver) |
| `install` | object `{method, base_url, version}`, опц. | способ установки redis (концепция `redis_install_*` роли): `method` ∈ `package` (default — distro-пакет, поведение-сохраняющий) / `binary` (opt-in — upstream-tarball: `base_url` + `version` (semver) + `soulprint.self.os.arch` → `/usr/local/bin` + свой systemd-юнит + distro-юзер/группа redis). `base_url`+`version` обяз. при `method=binary` (`validate`). Tarball качается по content-идемпотентности (SHA-256 содержимого, без integrity-verify, доверие HTTPS+store) — как node-exporter. Пустой/не передан → `method=package` |
| `conf_dir` | string, опц., default `/etc/redis` | каталог конфигурации Redis (`redis.conf`, `users.acl`, `sentinel.conf`, `tls/`). Оператор может переопределить под свой layout; HOST-инвариант (один на кластер). Прокидывается в destiny `redis` и **персистится в `state`** для day-2-консистентности (`add_node`/`update_config`/`add_user` читают его из `state`). Прежний хардкод `aclfile /etc/redis/users.acl` в compute убран — override теперь доезжает до `redis.conf` (`dir`/`aclfile` выводит destiny-шаблон из vars) |
| `data_dir` | string, опц., default `/var/lib/redis` | рабочий каталог данных Redis (RDB/AOF; `modules/` = `<data_dir>/modules`). Оператор может переопределить под свой storage-layout; HOST-инвариант, персистится в `state` для day-2. Прежний хардкод `dir /var/lib/redis` убран — override доезжает до `redis.conf` через vars шаблона |
| `memory_mb` | integer, опц., min `64` | бюджет памяти под Redis на хосте, МБ; `maxmemory` = доля от него |
| `persistence` | enum `off`/`aof`/`rdb`/`rdb_aof`, default `rdb` | режим durability; транслируется в `save`/`appendonly` |
| `maxmemory_policy` | enum eviction-политик | политика вытеснения при достижении `maxmemory` |
| `replicas_per_master` | integer, опц., default `0`, min `0` | **(оба режима)** реплик на каждый master. `cluster` — реплик на шард (roster = `shards * (1 + replicas_per_master)`); `sentinel` — реплик master-а (roster = `1 + replicas_per_master`). `0` — без реплик (`sentinel` с `0` = standalone-эквивалент). Унифицировано 2026-06-25 (заменило прежние `replicas` + `replicas_per_shard`) |
| `shards` | integer, обяз. при `cluster` (`required_when`), min `1` | **(cluster)** число master-шардов; 16384 hash-слота делятся поровну между мастерами. Обязателен при `redis_type=cluster` через `required_when: "input.redis_type == 'cluster'"` — пропуск рвёт input-валидацию ДО рендера |
| `cluster_node_timeout` | integer, опц., default `5000`, min `1` | **(cluster)** таймаут gossip между нодами, мс (директива `cluster-node-timeout`) |
| `users` | array `AclUser` (`[{name, perms, state}]`) | **operator-extra** ACL-юзеры; элемент — типизированный `AclUser` ([`types.yml`](types.yml), ADR-062 `$type`): `name` + `perms` обязательны, `state` ∈ `on`/`off` (дефолт `on`). `perms` — полная ACL-строка Redis, валидируется re2-паттерном `AclUser` (token-shape-фильтр). Имя **не может** совпасть с системным служебным (`default`/`replica`/`monitoring`/`sentinel`/`haproxy`) — коллизию отвергает input-валидация (422, см. [«Системные ACL-юзеры»](#системные-acl-юзеры)). Сливается **поверх** системных (last-wins) в `users.acl` |
| `redis_settings` | object (passthrough) | произвольные директивы `redis.conf` key→value; **бьют всё** в итоговом merge. **Имена** директив version-aware-валидируются `assert`-ом против каталога валидных директив выбранной версии Redis (см. [«Валидатор имён директив»](#валидатор-имён-директив-redis_settings)) — опечатка / директива из чужой версии рвёт прогон на render (422) ДО applying |
| `tls` | object `{enable, only, port, cert_ref, key_ref, ca_ref}`, опц. | **(TLS)** параметры TLS Redis (концепция `redis_tls_*` роли). operator-input **бьёт essence** (каждое под-поле опционально; недостающие берут дефолт из `essence.tls_*`). `enable` — главный гейт рендера PEM/директив; `only` — закрыть plain-порт (scenario ставит `port 0`); `port` — TLS-порт (директива `tls-port`, дефолт essence `7379`); `cert_ref`/`key_ref`/`ca_ref` — Vault-**ПУТИ** серверного cert/key и CA (форма `<mount>/<path>#<field>`, **не** сам PEM). destiny читает PEM через `vault(ref)` в ячейке `content` (`core.file.present`, seal-маскинг — НЕ `.tmpl`). Пустой/не передан → TLS off. `tls.only` требует `tls.enable` (`validate`) |
| `provision` | object `{enabled, provider, profile, ssh_provider, await_timeout}`, опц. | **★ keeper-side реализован, live ждёт C1** ([«Cloud-provision»](#cloud-provision-create-поднимает-vm-live-ждёт-c1)): поднять VM под топологию в **том же** create-прогоне ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md), Вариант A). `enabled: true` → cloud-create + доставка токена + барьер онбординга, потом redis-роль на созданные хосты; опущен / `enabled: false` → прогон по существующему roster-у (поведение бит-в-бит как без фичи). `provider`/`profile` — имена в реестрах `providers`/`profiles`; `ssh_provider` — имя SshProvider-плагина для доставки токена (`core.bootstrap.delivered`, [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md)); `await_timeout` (`<N>{s\|m\|h}`) — потолок ожидания онбординга (не задан → `essence.provision_await_timeout`, дефолт `10m`). **Числа VM в `provision` нет** — выводится из топологии (`shards`/`replicas_per_master`). **Живой провизион ждёт слайса C1** (cloud-init CA-signed host-key) + live-e2e — см. раздел |
**Чего во входном контракте нет (essence-параметры или авто-вычисление):**

- `sentinel_quorum` — **авто** `size(hosts)/2+1` (большинство), вычисляется в apply.
  Операторского поля больше нет (убрано 2026-06-25).
- `sentinel_master_name` — переехало в `essence.sentinel_master_name` (author-context,
  дефолт `master`; оператор переопределяет в `spec.essence`, не в Run-форме).
- `modules` (набор Redis-модулей) — больше **не** operator-выбор: директива «модули
  **всегда все**» — destiny разворачивает полный набор из своего `vars.redis_modules`
  на Redis < 8, на Redis 8+ модули встроены (`.so` не качаются). Подмножество не
  выбирается.
- `modules_base_url` (источник `.so`) — переехал в `essence.modules_base_url`
  (author-context, дефолт `""`; оператор переопределяет в `spec.essence` под свой
  mirror). На Redis < 8 без заданного источника destiny не построит URL `.so` (мягко
  проявится при render-е destiny, не на input-валидации — `validate`-контекст
  input-only, essence в нём недоступен).
- `master_ip` / `master_port` — относились к убранному режиму `sentinel_only`
  (внешний master). В `sentinel` master выбирается из roster-а.

**Кросс-полевые инварианты ввода** ([`create/main.yml → validate:`](scenario/create/main.yml),
input-only, первый провал → 422 `validation_failed` ДО коммита incarnation и ДО
applying):

- `tls.only` требует `tls.enable` — только-TLS без включённого TLS закрывает
  plain-порт и не открывает TLS-порт (у Redis не остаётся ни одного listener-а);
- `install.method=binary` требует `install.base_url` и `install.version` — без них
  не из чего собрать URL upstream-tarball-а;
- имя `input.users` **не** входит в `[default, replica, monitoring, sentinel, haproxy]`
  — operator-extra не может занять имя системного служебного юзера (его молча
  перекрыли бы доливаемые системные perms; см. [«Системные ACL-юзеры»](#системные-acl-юзеры)).
  Тот же guard в `add_user` ([`validate:`](scenario/add_user/main.yml)).

Параметров `logrotate_enable` / `sysctl_enable` / `thp_disable` во входном контракте
**нет**: отключение Transparent Huge Pages, logrotate-конфиг и sysctl-тюнинг — **безусловный
baseline** (рекомендация Redis / hardening, выровнено по sysctl-блоку Ansible-роли), а не
операторский выбор. Эти задачи всегда активны и приходят в destiny через `apply.input`.
Подробнее — [«Host-tuning extras»](#host-tuning-extras).

Топология обоих режимов задаётся **счётчиками**, а не списком хостов. cluster —
`shards` + `replicas_per_master`: ожидаемый roster — `shards * (1 + replicas_per_master)`
souls, плагин раскладывает роли по фактическому roster-у сам (см.
[«`create` (режим cluster)»](#create-режим-cluster)). sentinel — счётчиком
`replicas_per_master` (roster `1 + replicas_per_master`, см.
[«`create` (режим sentinel)»](#create-режим-sentinel)); с
`replicas_per_master: 0` это один master + sentinel-демон (standalone-эквивалент).
Render-гарда на соответствие числа таргетированных souls этим счётчикам **нет** —
несоответствие проявляется при сборке топологии плагином, не fail-fast при рендере.
cloud-создание машин под топологию — post-бета.

Пароли **не во входном контракте** — лежат в Vault, резолвятся keeper-side (см.
[«Безопасность»](#безопасность)).

### Пример простого ввода — sentinel single-host / standalone-эквивалент (как в L0-кейсе)

Из [`create/tests/full-stack/case.yml`](scenario/create/tests/full-stack/case.yml)
(`replicas_per_master: 0` → один хост = master + sentinel-демон):

```yaml
input:
  redis_type: sentinel
  version: "5:7.0.15-1~deb12u7"
  replicas_per_master: 0
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
  version: "7.2.4"
  shards: 2
  replicas_per_master: 1
  cluster_node_timeout: 8000
```

Оператор объявляет **только счётчики**: 2 шарда по 1 реплике. Раскладку ролей
master/replica и hash-слотов делает плагин; оператор не перечисляет хосты
поимённо — они берутся из таргетированного roster-а прогона.

### Пример простого ввода — sentinel (как в L0-кейсе)

Из [`create/tests/sentinel-create-1master-2replica/case.yml`](scenario/create/tests/sentinel-create-1master-2replica/case.yml)
— `1 + replicas_per_master = 1 + 2 = 3` хоста в roster-е прогона:

```yaml
input:
  redis_type: sentinel
  version: "7.2.4"
  replicas_per_master: 2
```

Оператор объявляет **только счётчик** `replicas_per_master` (1 master + 2 реплики).
Master-election (кто master) — первый по SID хост; раскладку ролей и привязку
реплик/sentinel-ов делает apply-сторона. `sentinel_quorum` авто = `size(hosts)/2+1`
(большинство), операторского поля нет; `sentinel_master_name` берётся из
`essence.sentinel_master_name` (дефолт `master`).

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

## Системные ACL-юзеры

Помимо operator-extra (`input.users`) сервис **всегда доливает** в `users.acl` набор
**служебных** ACL-юзеров, без которых кластер не работает: `replica` (репликация
`PSYNC`), `monitoring` (экспортёр метрик), `sentinel` (AUTH sentinel↔redis), `haproxy`
(health-check балансировщика). perms каждого скопированы 1:1 из Ansible-роли redis
(`redis_system_users` / `redis_sentinel_system_users`) и живут в
[`essence/_default.yaml`](essence/_default.yaml) (`system_acl_users` —
для `redis-server`; `system_acl_users_sentinel` — для sentinel-демона). Оператор их **не
задаёт** и **не видит** во входной форме — это author-context essence.

### Merge: системные снизу, operator-extra сверху

Во **всех** задачах, рендерящих `users.acl` (create cluster/sentinel + day-2
`add_user`/`update_config`/`add_node`), `apply.input.users` собирается двойным
`merge()`: **сначала** системные из `essence.system_acl_users` (нижний слой, `state: on`,
пароль из Vault), **поверх** — operator-extra (`input.users` на create / `state.redis_users`
на day-2 / `compute.users_new` в `add_user`). Порядок аргументов `merge()` = приоритет:
last-wins, operator-extra перекрыл бы одноимённого системного — но коллизия имён
закрыта validate-guard (ниже), поэтому слои не пересекаются.

Доступ к набору — через `default(essence.system_acl_users, {})`: **back-compat** —
инкарнации со старым essence без этого поля системных НЕ получают (фича включается
**обновлением essence**, не кодом scenario).

### ★ Долив на day-2 (инвариант)

`add_user` / `update_config` / `add_node` перерендеривают **весь** `users.acl` (не
append) тем же destiny-кирпичом. Поэтому каждая из этих задач **обязана** долить
системных — иначе перерендер **стёр бы** `replica`/`monitoring`/`sentinel`/`haproxy`
и сломал бы на day-2 репликацию / sentinel↔redis / health-check. Системные **не
лежат в state** (state хранит только operator-extra), поэтому доливаются из `essence`
на **каждом** рендере. Особо критичен `add_node`: без системного `replica` новая нода
не выполнит `PSYNC` — кластер не примет её как реплику.

### default-юзер: разделён по двум aclfile

- **redis-server** — `default` в `system_acl_users` **отсутствует**: он задаётся
  `requirepass` в `redis.conf` (`users.acl` его не дублирует, как и в Ansible-роли).
- **sentinel-демон** — `default` в `system_acl_users_sentinel` **есть**: у sentinel-демона
  нет `requirepass`-эквивалента для aclfile-доступа, поэтому `default` объявляется в
  `sentinel-users.acl` с тем же главным секретом `secret/redis/<incarnation>#password`
  (что и requirepass redis-server — иначе sentinel-демон не пустил бы default-клиента
  общим паролем).

### 2-й aclfile sentinel-демона

sentinel-демон требует **отдельный** perms-набор (`sentinel|*`-команды, не обычные
redis-команды), поэтому пишется во **второй** aclfile —
[`sentinel-users.acl`](../../destiny/redis/templates/sentinel-users.acl.tmpl) (на него
указывает `aclfile` в `sentinel.conf`). Только в режиме `sentinel` scenario передаёт
destiny новое additive-поле `input.sentinel_users` (из `essence.system_acl_users_sentinel`);
operator-extra сюда **не** доливается (sentinel-доступ служебный). Пароли:
`default` → главный `secret/redis/<incarnation>#password`; остальные
(`monitoring`/`sentinel`/`haproxy`) → ветка `secret/redis/<incarnation>/users/<name>#password`.
Полную механику рендера см. в
[destiny-README → «Системные ACL-юзеры и второй aclfile»](../../destiny/redis/README.md#системные-acl-юзеры-и-второй-aclfile).

### validate-guard на резервированные имена

operator-extra (`input.users` на create, `input.user.name` в `add_user`,
`input.users[].name` в `update_users`) **не может** занять имя системного:
`[default, replica, monitoring, sentinel, haproxy]` зарезервированы. Коллизию ловит
input-валидация (`validate:`, [create](scenario/create/main.yml) +
[add_user](scenario/add_user/main.yml) + [update_users](scenario/update_users/main.yml))
— **422 `validation_failed` ДО applying**, а не молчаливое переопределение (operator
`replica` с `~* +@all` молча выдал бы лишние права и сломал бы replica-perms репликации).
Guard **input-only**: список имён захардкожен в каждом сценарии (essence в
`validate`-контексте недоступен — структурный CEL-барьер).

## Сценарии

### `create` (единый вход, диспетчер по `redis_type`)

Оператор всегда вызывает **`create`**; режим выбирает поле `redis_type`.
[`scenario/create/main.yml`](scenario/create/main.yml) — **диспетчер**: тело
прогона вынесено в две ветки, подключаемые через `include:` —
[`cluster.yml`](scenario/create/cluster.yml) и
[`sentinel.yml`](scenario/create/sentinel.yml). `include:` раскрывается
**безусловно** в плоский список ДО render (`when:` на самой include-задаче запрещён),
поэтому ветвление делает не include, а `when:` по `input.redis_type` на каждой задаче
ветки: предикат статичен (только `input.*`), Keeper вычисляет его at-render и гасит
неактивную ветку placeholder-skip-ом (ADR-012(d) Вариант b). Порядок include —
cluster, sentinel: ветки добавляются в хвост, индексы предыдущих веток не сдвигаются.

**Неизвестный режим — input-валидация, не shell-guard.** Прежняя `core.cmd.shell`
«mode-guard» первой задачей **убрана**: неизвестный `redis_type` отвергает
**input-валидация Keeper-а по enum** `input.redis_type` (`[sentinel, cluster]`)
**ДО рендера** — раньше и понятнее, чем shell-test на Soul-е. Обе ветки gated на свой
`redis_type`, а enum гарантирует попадание ровно в одну из них, поэтому footgun
«нереализованный режим = тихий зелёный no-op» конструктивно невозможен (значение вне
enum не доходит до рендера).

После успешного apply Keeper фиксирует `state_changes` (ADR-009 §7.1, ADR-057):
`redis_type`, `redis_version`, `redis_config` (тот же `compute.redis_config`, что ушёл
в render — единый источник истины; для cluster — плюс `cluster-*`-директивы, кроме
host-вариативного `cluster-announce-ip`, который в state не пишется), `redis_users`
(из `input.users`), `redis_hosts = []`, `redis_sentinel` (`{master_name, quorum}` в
режиме sentinel; иначе пустой объект) + именованные read-model-поля
(`tls`/`install`/`persistence`/`memory_mb`/`maxmemory_policy`/`modules`/`modules_base_url`/`conf_dir`/`data_dir`/`sysctl_settings`
+ counts `shards`/`replicas`/`sentinel_quorum`).

#### `create` (режим cluster)

[`scenario/create/cluster.yml`](scenario/create/cluster.yml) — honest hash-slot
Redis Cluster (16384 слота). **Это тонкая ветка-диспетчер**: size-guard (ниже) + один
`- include: redis-deploy-cluster.yml`. Само деплой-тело вынесено на **service-level**
([`scenario/redis-deploy-cluster.yml`](scenario/redis-deploy-cluster.yml)) и инклудится
**по имени файла** (scenario-include фоллбэкает `scenario/<name>/<file>` →
`scenario/<file>`, [orchestration.md §6](../../../docs/scenario/orchestration.md)) — то же тело
делит `migrate_cluster` (joinable-вариант), дубликата нет. Шаги деплой-тела:

   Обязательность `shards` — **декларативно**: схема `input:` несёт
   `required_when: "input.redis_type == 'cluster'"` на поле `shards`. Пропуск `shards`
   в режиме cluster ловит **input-валидация Keeper-а ДО рендера** (понятный отказ,
   прогон не стартует) — это не shell-задача на хосте. Прежний `shards-guard`
   (`core.cmd.shell` с `has(input.shards)`) удалён.

   **★ Size-guard топологии — есть (render-time `assert`).** Первый шаг ветки
   ([`create/cluster.yml`](scenario/create/cluster.yml), **перед** include деплой-тела) —
   `assert:` на `size(soulprint.hosts) == shards * (1 + replicas_per_master)`. Предикат
   вычисляется **keeper-side на рендере** (там доступен `soulprint.hosts`, на Soul-е его
   нет); `false` обрывает render с понятным `message` **ДО любого dispatch** — иначе
   плагин `community.redis.cluster` вывел бы число шардов из числа нод и **молча**
   проигнорировал declared `shards` (silent рассинхрон declared↔actual). Кейс
   [`cluster-size-guard-mismatch`](scenario/create/tests/cluster-size-guard-mismatch/case.yml).
   Это **не** `required_when` (тот про присутствие одного поля; size-guard — про сверку с
   roster-ом). **Почему `assert`, а не `validate`:** size-guard нужен `soulprint.hosts`, а
   `validate`-контекст input-only (структурный CEL-барьер).

1. **`apply: destiny: redis`** — режим-агностичная destiny, но merged `config`
   **дополнен cluster-директивами** поверх базового merge: `cluster-enabled: yes`,
   `cluster-config-file: nodes.conf`, `cluster-node-timeout` (из input или дефолт
   `5000`) — все три host-инвариантны и корректно идут через config-merge
   (`apply.input` резолвится один раз на первом по SID хосте). `cluster-announce-ip`
   рендерится **per-host** в `redis.conf.tmpl` из `.self.network.primary_ip` (IP
   **этого** хоста, критично за NAT/в облаке), под гейтом `cluster-enabled` — НЕ через
   merged config: он host-инвариантен (как `bind`), и проброс через config-map
   зафиксировал бы IP первой ноды для всех.
2. **health-gate PING** (`community.redis.command`, `retry`) — каждая нода обязана
   ответить `PING` ДО сборки кластера.
3. **cluster-build** (`community.redis.cluster`, `action: create`, `run_once` на
   bootstrap-ноде) — собирает кластер. Сценарий строит детерминированный `nodes`-MAP
   из roster-а прогона (`soulprint.hosts`): ключ = `SID` (стабилен и сортируем),
   значение = `{addr: "<primary_ip>:6379"}`, и передаёт его плагину вместе с
   `replicas_per_shard` (контракт плагина; источник — `input.replicas_per_master`).
   Сам `CLUSTER MEET`/`ADDSLOTS`/`REPLICATE` (через go-redis) и деление 16384 слотов
   делает **плагин** детерминированно по сортировке ключей — scenario топологию НЕ
   транслирует, передаёт готовую (иначе две раскладки рассинхронятся). Состояние state
   плагина — в его per-module doc
   [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).

#### `create` (режим sentinel)

[`scenario/create/sentinel.yml`](scenario/create/sentinel.yml) — master-replica +
sentinel-демон (с `replicas_per_master: 0` — standalone-эквивалент: один master +
sentinel-демон). **Тонкая ветка-диспетчер**: size-guard (ниже) + один
`- include: redis-deploy-sentinel.yml`. Деплой-тело вынесено на **service-level**
([`scenario/redis-deploy-sentinel.yml`](scenario/redis-deploy-sentinel.yml)) и инклудится
**по имени файла** ([orchestration.md §6](../../../docs/scenario/orchestration.md)) — то же тело
делит `migrate_cluster/sentinel.yml`, дубликата нет. Топология: `1 master +
replicas_per_master` реплик; master-election declared (probe на create невозможен — redis
ещё не поднят), первый по SID хост = master (`soulprint.hosts[0]`, отсортирован
детерминированно). Шаги деплой-тела:

   **★ Size-guard топологии — есть (render-time `assert`).** Первый шаг ветки
   ([`create/sentinel.yml`](scenario/create/sentinel.yml), **перед** include) — `assert:`
   на `size(soulprint.hosts) == 1 + replicas_per_master` (тот же механизм, что в cluster:
   keeper-side на рендере, `false` обрывает render с `message` до dispatch). Иначе лишние
   хосты молча стали бы незаявленными репликами, нехватка — недобором реплик. Кейс
   [`sentinel-size-guard-mismatch`](scenario/create/tests/sentinel-size-guard-mismatch/case.yml).

1. **`apply: destiny: redis`** — та же режим-агностичная destiny, но с
   `sentinel_enabled: true` → дополнительно рендерится `sentinel.conf` + `redis-sentinel`
   unit и поднимается демон. `config` — **базовый** merge (без cluster-директив).
   `sentinel.master_ip` — host-инвариантный адрес master-а (master-election
   `soulprint.hosts[0]`), резолвится `run_once` и одинаков у всех → корректно через
   `apply.input`. `sentinel.master_name` — из `essence.sentinel_master_name` (дефолт
   `master`). `announce-ip` — per-host в `sentinel.conf.tmpl` из `.self.network.primary_ip`.
2. **health-gate PING** (`community.redis.command`, `retry`) — каждый узел обязан
   ответить `PING` на `:6379` ДО привязки реплик/настройки sentinel.
3. **REPLICAOF** (`community.redis.replica`, на репликах — `where:` исключает
   избранного master по SID) — реплики следуют за избранным master-ом. `master_addr` —
   host-инвариантный (`soulprint.hosts[0]`); `where:` гарантирует, что задача не
   рендерится на самом master (плагин-guard `addr == master_addr` остаётся
   defense-in-depth).
4. **SENTINEL MONITOR reconcile** (`community.redis.sentinel`, на **каждом** хосте) —
   каждый sentinel-демон мониторит master. `monitor.ip` — host-инвариантный
   (master-election); `quorum` — **авто** большинство `size(hosts)/2+1` (операторского
   поля `sentinel_quorum` нет). `auth_pass` — keeper-side `vault()`-резолв
   (маскируется). Алгоритм reconcile (`MONITOR`/`REMOVE`+`MONITOR`/`SET`/`CONFIG SET`)
   перенесён 1:1 из Ansible. **★ Гейт `when: vars.sentinel_monitor_now`** (контракт-флаг
   деплой-тела): на `create` флаг **безусловно `true`** (node-1 — настоящий master,
   sentinel мониторит его сразу). При `migrate_cluster` тот же флаг **`false`** — новый
   master во время заливки в роли slave, ранний MONITOR инициировал бы failover →
   split-brain; там MONITOR **откладывается** и выполняется в `detach_source` после
   `REPLICAOF NO ONE` (см. [«`migrate_cluster`»](#migrate_cluster-day-2-миграция-с-внешнего-кластера)).
5. **health-gate PONG** (`community.redis.command`, `:26379`) — каждый sentinel-демон
   обязан ответить `PONG`. **Строго same-task** `register.self` (НЕ cross-passage
   flow-control, ADR-056): «дождаться N sentinel» не выражаем — проверяется только
   локальный sentinel этого хоста (`retry`/`until` + `failed_when` по
   `register.self.result == 'PONG'`).

State плагина (states `replica` / `sentinel`, их params и идемпотентность) — в его
per-module doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).

> **Тонкий sentinel-слой над внешним master (`sentinel_only`)** убран из сервиса как
> отдельный режим. Возможность развернуть **только** sentinel-демон без локальной
> data-плоскости redis-server остаётся **capability destiny-кирпича
> [`redis`](../../destiny/redis/)** (флаги `deploy_redis: false` / `sentinel_enabled:
> true`) — для переиспользования другими сервисами (например DragonFly), но через
> сервис redis она больше не вызывается.

### Cloud-provision (`create` поднимает VM, live ждёт C1)

> **★★ Keeper-side реализован, живой провизион ждёт C1 + live-e2e.** Доставка per-VM
> bootstrap-токена реализована keeper-side — модуль `core.bootstrap.delivered`
> ([ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md), SSH-доставка токена;
> заменил заглушку `keeper.push.applied`). Поток валиден на render (L0 Trial) и проходит
> unit-тесты. Но живой прогон «VM→redis в один проход» ещё **не пройдёт live-e2e** без
> слайса **C1**: `push.Dial` доверяет только host-cert, подписанному host-CA (отказ от
> TOFU), а у свежей VM после cloud-init host-key пока **голый** (не CA-signed) — handshake
> при доставке токена реджектится на connect-е, токен не доставится → созданные VM не
> пройдут CSR-онбординг → барьер `await_online` (в) не наберёт presence → `error_locked`.
> C1 = cloud-init (B-flat userdata) генерит CA-signed host-key тем же host-CA, чтобы Keeper
> верифицировал host-cert. До C1 + live-валидации WB cloud `input.provision.enabled: true`
> против живого Keeper не доводит прогон до конца. Образец того же флоу —
> [`examples/service/example-cloud-bootstrap/`](../example-cloud-bootstrap/).

Опциональная способность `create` (state_schema v7, [ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md),
Вариант A): **один** create-прогон поднимает VM под топологию **и** деплоит на них Redis.
Гейтится `input.provision` ([«Входной контракт»](#входной-контракт-оператора)) — задан и
`enabled: true` включает **общее provision-тело service-level**
[`scenario/redis-provision.yml`](scenario/redis-provision.yml) (conditional-include
**строго перед** cluster/sentinel-веткой); опущен / `enabled: false` — прогон по
существующему roster-у, поведение бит-в-бит как без фичи. Тело лежит **на service-level**
(`scenario/<file>`, не внутри одного сценария) и инклудится по имени файла **обоими**
потребителями — `create` и `migrate_cluster`: scenario-include резолвит
`scenario/<name>/<file>`, затем фоллбэкает на `scenario/<file>`
([orchestration.md §6](../../../docs/scenario/orchestration.md)), поэтому дубликата нет.

**Поток** (`redis-provision.yml`, все три шага `on: keeper`):

1. **(а) cloud-create** — `module: core.cloud.created` ([keeper-side core](../../../docs/keeper/cloud.md),
   ADR-017). Создаёт VM через CloudDriver-плагин `soul-cloud-<provider>`. **Число VM
   выводится из топологии** (`cluster`: `shards * (1 + replicas_per_master)`; `sentinel`:
   `1 + replicas_per_master`) — отдельного `node_count` в `input.provision` нет (рассинхронился
   бы с size-формулой). `generate_userdata: true` (ADR-017(h) B-flat): Keeper рендерит
   cloud-init из `keeper.yml::cloud_init` (CA + soul-binary URL); userdata токены **НЕ**
   несёт. Per-VM bootstrap-токен → `register.provision.hosts[].bootstrap_token` (plain
   one-time, маскируется `audit.MaskSecrets`).
2. **(б) доставка токена** — `module: core.bootstrap.delivered` ([keeper-side core](../../../docs/keeper/modules.md#corebootstrapdelivered),
   [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md)). Берёт
   `register.provision.hosts` (sid + primary_ip + plain bootstrap-токен из шага (а)) и
   тонко доставляет **только токен** по SSH на каждую VM (бинарь/CA/unit уже поставил
   cloud-init): токен в STDIN (не argv) → `/etc/soul/token` (`mode 0400`), затем
   `systemctl start soul` (`start_soul` default `true`). `ssh_provider` —
   `input.provision.ssh_provider` (имя SshProvider-плагина). **B1-strict**: ошибка любого
   хоста → шаг `failed` → state не коммитится → `error_locked`. **★ Реализовано
   keeper-side, но требует C1** (CA-signed host-key VM) — без него `push.Dial` реджектит
   host-cert на connect-е (см. врезку выше).
3. **(в) регистрация + барьер онбординга** — `module: core.soul.registered` ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md)).
   `sid` — **список** SID созданных VM (`register.provision.hosts.map(h, h.sid)`, list-SID
   ADR-061); `coven` — корневой `incarnation.name`. `await_online: true` блокирующе ждёт,
   пока созданные Souls станут online (Redis SID-lease) под `await_timeout`; **B1-strict**:
   недобор кворума → шаг `failed` → state не коммитится → `error_locked`. `refresh_soulprint:
   true` → после успеха scenario-runner пере-резолвит roster **перед** cluster/sentinel-веткой
   (иначе ветка увидела бы пустой/старый `soulprint.hosts` — созданные VM ещё не в roster-е).

После успеха cluster/sentinel-ветка катит redis-роль на уже-online хосты. `state_changes`
пишет `provisioned_vm_ids` (из `register.provision.vm_ids`), `provisioned_sids` (Keeper-id
созданных Souls) и `provisioned_provider` (из `input.provision.provider`) — **только** на
provision-пути (guard по `input.provision`, не `has(register.provision)`); на
non-provision-прогоне остаются `[]` / `[]` / `''`.

**Teardown — сценарий [`destroy`](#destroy-teardown-cloud-provisioned-vm).** Снятие
provisioned-VM реализовано отдельным **lifecycle**-сценарием (`scenario/destroy/`,
не runnable из Run-формы — terminal-флоу `DELETE /v1/incarnations/{name}`): читает
`provisioned_vm_ids` / `provisioned_sids` / `provisioned_provider` из `incarnation.state`
и зовёт `core.cloud.destroyed` + cascade-чистку реестров. На не-провиженной инкарнации
(deploy на существующие VM) задача `destroyed` group-drop-ается (нечего снимать). Детали —
[«`destroy`»](#destroy-teardown-cloud-provisioned-vm).

### Валидатор имён директив `redis_settings`

`create` сверяет **каждое имя** директивы в `input.redis_settings` (passthrough в
`redis.conf`) против каталога валидных имён для выбранной версии Redis — `assert:` на
render-стороне ([`scenario/create/main.yml`](scenario/create/main.yml), шаг
«Version-aware валидатор»). Неизвестное имя (опечатка `maxmemoyr` или директива из чужой
версии) рвёт прогон на render (**422 `assert_failed`**) **ДО applying**, понятным
сообщением — а не поздним `redis-server`-фейлом на хосте.

- **Каталог — `essence.redis_directives`** ([`essence/_default.yaml`](essence/_default.yaml)).
  ★ Имя `redis_directives` — рабочее (предложено в этом эпике). Структура: ключ = серия
  Redis `major.minor`, значение = плоский список валидных имён директив этой серии.
  Покрыты **шесть** серий: `6.2` / `7.0` / `7.2` / `7.4` / `8.0` / `8.2`.
- **Источник — `src/config.c` апстрима Redis** (таблица `standardConfig` + специальные
  директивы), **не** `redis.conf` (прозаические комментарии неотличимы от commented-примеров
  директив). Каталог **committed** (на render-пути сети нет); перегенерация при добавлении
  версии — [`scripts/gen-redis-catalog.sh`](../../../scripts/gen-redis-catalog.sh) (от корня
  репо), вывод вклеивается в `essence`.
- **MVP — только имена**, значения директив не валидируются.
- **Series-skip:** `input.version` (distro-пин `5:7.0.15-1~deb12u7` **или** semver) → серия
  `X.Y` извлекается регуляркой; серии **нет** в каталоге → `assert` пропускается (валидируются
  только известные версии, не-каталожная не блокируется). **Empty-catalog-skip:** нет
  `essence.redis_directives` (старый essence) → проверка пропускается (back-compat).
- **Почему `assert`, а не `validate`:** каталог живёт в `essence`, а `validate`-контекст
  **input-only** (essence недоступен, структурный CEL-барьер); `assert` видит
  `essence`+`input`+`compute`.
- **Только в `create`:** day-2 `update_config` (тоже принимает `redis_settings`) этого
  валидатора пока **не несёт** — расширение follow-up.

### `add_node` (day-2: присоединить ноду к кластеру)

[`scenario/add_node/main.yml`](scenario/add_node/main.yml) — присоединить **одну**
новую ноду к уже сформированному Redis-кластеру (режим `redis_type=cluster`). Аналог
`redis-cli --cluster add-node`, но целиком через плагин `community.redis.cluster`
(`action: add-node`) — без `redis-cli`/shell. Новая нода обязана быть уже привязанной
к incarnation как Soul (онбординг — вне сценария); таргетинг по стабильному SID через
`where:`. Прогон зовётся на весь incarnation: roster (`soulprint.hosts`) содержит и
существующие ноды, и новичка — из него строятся endpoint-ы. Четыре шага:

1. **guard** (`core.cmd.shell`, `run_once`) — `new_node_sid` и `seed_sid` обязаны быть
   различными членами roster-а прогона (keeper-side при render по `soulprint.hosts`).
2. **`apply: destiny: redis`** на **новой** ноде (`where: soulprint.self.sid == input.new_node_sid`)
   — install + render `redis.conf` (cluster-директивы из `incarnation.state.redis_config` —
   **источник истины**, зафиксированный `create`, не перевычисляется → нет drift) + systemd.
   `users.acl` новой ноды доливает **системных** из `essence.system_acl_users` поверх
   `state.redis_users` — без системного `replica` нода не выполнит `PSYNC` и кластер не
   примет её как реплику (см. [«★ Долив на day-2»](#-долив-на-day-2-инвариант)).
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

### `reshard` (day-2: перенести N слотов между мастерами)

[`scenario/reshard/main.yml`](scenario/reshard/main.yml) — перенести **N** hash-слотов
с одного master-а (`from_sid`) на другой (`to_sid`) в уже сформированном кластере
(режим `redis_type=cluster`). Аналог `redis-cli --cluster reshard`, целиком через
плагин `community.redis.cluster` (`action: reshard`) — без `redis-cli`/shell. Оба
master-а обязаны быть в roster-е прогона (`soulprint.hosts`); таргетинг по стабильному
SID. Два шага:

1. **guard** (`assert:`, keeper-side при render) — `from_sid` и `to_sid` обязаны быть
   различными членами roster-а; `false` обрывает render с `message` ДО dispatch.
2. **reshard** (`community.redis.cluster`, `action: reshard`, `run_once`) — endpoint-ы
   `from`/`to` строятся из roster-а по SID. Плагин читает `CLUSTER NODES` с `from`,
   берёт первые `slots` слотов источника по возрастанию и переносит каждый
   (`SETSLOT`/`MIGRATE`/`SETSLOT NODE`, online).

> **★ `reshard` НЕ идемпотентен** (осознанно, exec-style day-2): повторный apply
> сдвинет **ещё** `slots` слотов с `from` на `to`. Оператор зовёт его **явно**, ровно
> столько раз, сколько нужно переносов — это **не** часть converge. Семантика
> partial-failure (нет авто-отката) — в
> [per-module doc](../../../docs/module/community/redis/README.md).

### `restart` (day-2: безопасный rolling-restart)

[`scenario/restart/main.yml`](scenario/restart/main.yml) — rolling-restart Redis без
изменения конфига (режимы sentinel/cluster). Фактическая роль каждого хоста
**волатильна** (после возможного failover declared-роль `create` уже неверна),
поэтому роль берётся **живым probe** (`community.redis.role`, `INFO replication`)
непосредственно перед таргетингом, а не из `incarnation.state` (ADR-008). Реплики
рестартятся **по одной** (`block` + `serial: 1`: волна = {рестарт, health-gate}),
master — **отдельной задачей после всех реплик** (rolling-инвариант «master
последним»). Health-gate реплики — `community.redis.replica-synced` (строгий ресинк
`master_link_status:up`, не просто `PONG`); рестарт самого демона —
`core.service.restarted`. `state` не меняется — только запись в `state_history`.

> **★ Источник истины day-2 = `incarnation.state`, не `essence`** ([production-conventions §7a](../../../docs/destiny/production-conventions.md)).
> TLS-дискриминатор коннекта плагина (по TLS или plaintext, на каком порту) берётся
> из развёрнутого `incarnation.state.redis_config` (`'tls-port' in
> incarnation.state.redis_config`), а **не** из `essence.tls_*`: на create оператор
> мог переопределить TLS через `input.tls` (бьёт essence), и эта развёрнутая
> конфигурация зафиксирована только в `state`. Смотреть на `essence` дало бы
> plaintext-коннект на TLS-only Redis (провал health-gate). Секрет CA в `state` не
> лежит (ИБ) — резолвится `vault(essence.tls_ca_ref)`, в `state` материализуется
> только путь к PEM на хосте.

Живой failover (`SENTINEL FAILOVER` / `CLUSTER FAILOVER` до рестарта master-а) — в
backlog (плагинный verb `failover`), пока master рестартится напрямую (краткая
недоступность на рестарт). См. [«В работе»](#в-работе).

> **★ `restart` — за systemd-unit-уровнем, не за конфигом** ([production-conventions §6a](../../../docs/destiny/production-conventions.md#6a-hot-reload-предпочтительнее-рестарта-для-сервисов-с-live-reconfig)).
> Изменения `redis.conf`/`users.acl`/TLS-материала **не** требуют рестарта — их
> применяют day-2 hot-reload-сценарии ниже (`update_config`/`add_user`/`rotate_tls`).
> `restart` нужен только когда меняется сам systemd-юнит (hardening drop-in) или для
> явного rolling-перезапуска демона; в destiny [`redis/tasks/server.yml`](../../destiny/redis/tasks/server.yml)
> реактивный рестарт сужен до `onchanges: [redis_hardening]`.

### `update_config` (day-2: hot-reload директив `redis.conf`)

[`scenario/update_config/main.yml`](scenario/update_config/main.yml) — изменить
директивы `redis.conf` на **уже работающем** Redis **без рестарта процесса**
(hot-reload через `CONFIG SET`). Оператор задаёт изменённое подмножество create-входа
(`memory_mb`/`maxmemory_policy`/`persistence`/`redis_settings`); сценарий
**перевычисляет** итоговый `redis_config` тем же compute-переводом, что `create`, но с
подложкой из `incarnation.state` (не заданное оператором сохраняет ранее
развёрнутое — day-2 источник истины = `state`, [§7a](../../../docs/destiny/production-conventions.md#7a-day-2-источник-истины--incarnationstate)). Два шага:

1. **re-render** `redis.conf` на диск с новым merged config (полный файл — desired
   state для следующего рестарта процесса). Тот же destiny-кирпич перерендеривает и
   `users.acl`, поэтому задача **доливает системных** из `essence.system_acl_users`
   поверх `state.redis_users` — иначе hot-reload-проход стёр бы служебных юзеров (см.
   [«★ Долив на day-2»](#-долив-на-day-2-инвариант)).
2. **hot-reload** (`community.redis.config`, `CONFIG SET` + `CONFIG REWRITE`): передаётся
   **весь** `compute.redis_config`, плагин сам пропускает startup-only-директивы по
   денилисту (`port`/`dir`/`aclfile`/…) и применяет только hot-settable. Идемпотентно —
   honest `CONFIG GET`-diff в плагине → повторный прогон `changed=false`.

`validate` требует хотя бы одно изменяемое поле. TLS-материал и ACL **не** трогаются
(для них — отдельные сценарии). `state` фиксирует новый `redis_config` + изменённые
namedfields. Параметры state `config` (вкл. денилист) — в
[per-module doc](../../../docs/module/community/redis/README.md#config--params).

### `add_user` (day-2: добавить ACL-пользователя через `ACL LOAD`)

[`scenario/add_user/main.yml`](scenario/add_user/main.yml) — добавить (или
переопределить) **одного** ACL-пользователя на работающем Redis **без рестарта**.
Оператор задаёт **одну запись `AclUser`** (`input.user`: `name` + `perms` + `state`) —
тот же тип `$type: AclUser`, что элемент массива create `input.users`
([`types.yml`](types.yml), ADR-062). `perms` валидируется **тем же re2-паттерном
`AclUser`**, что в create (закрывает day-2-мусор: невалидная ACL-строка отсекается на
input-валидации, а не доезжает до `users.acl`). Пароль **не** во входе — лежит в Vault
по конвенции `secret/redis/<incarnation>/users/<name>#password`, резолвится keeper-side.
Массовое редактирование **всего** набора operator-extra — отдельный сценарий
[`update_users`](#update_users-day-2-массовое-редактирование-набора-acl-юзеров)
(bulk-replace). Два шага:

1. **re-render** `users.acl` на диск с новым набором: **системные** служебные юзеры
   (из `essence.system_acl_users`) + operator-extra (`state.redis_users` + добавляемый,
   upsert по имени). re-render пишет **весь** файл, поэтому долив системных обязателен —
   иначе перерендер стёр бы `replica`/`monitoring`/`sentinel`/`haproxy` (см.
   [«★ Долив на day-2»](#-долив-на-day-2-инвариант)). per-user пароли из Vault, `.tmpl`
   пишет хеш, не plaintext. `input.username` ∈ резервированных имён отвергает
   [`validate:`](scenario/add_user/main.yml) (422).
2. **hot-reload ACL** (`community.redis.acl`, `ACL LOAD`): живой инстанс перечитывает
   `aclfile` целиком. Идемпотентно по конструкции; плагин делает diff `ACL LIST`
   до/после (`changed=false` при совпадении).

`state.redis_users` мутируется новым набором — типизированный массив `AclUser` (`[{name, perms, state}]`, **upsert** по полю `name`, **без** пароля — ИБ; тип из [`types.yml`](types.yml), ADR-062).
Один юзер за прогон (атомарная операция). Параметры state `acl` — в
[per-module doc](../../../docs/module/community/redis/README.md#acl--params).

### `update_users` (day-2: массовое редактирование набора ACL-юзеров)

[`scenario/update_users/main.yml`](scenario/update_users/main.yml) — **массовое**
редактирование **всех** operator-добавленных ACL-юзеров на работающем Redis **без
рестарта** (**bulk-replace** через `ACL LOAD`). Оператор задаёт **полный новый набор**
operator-extra юзеров — `input.users` (массив `AclUser`, тот же `$type: AclUser`, что
create/add_user). Этот набор **целиком заменяет** `incarnation.state.redis_users`:

- **граница с `add_user`** — `add_user` добавляет/переопределяет **одного** (точечный
  upsert, набор растёт); `update_users` задаёт **весь** набор разом, поэтому **удаление**
  юзера выражается его **исключением** из массива (юзер, которого нет в новом наборе,
  выпадает из `users.acl` и из `state`). Пустой `input.users: []` убирает **всех**
  operator-extra;
- **системные не трогаются** — `replica`/`monitoring`/`sentinel`/`haproxy` **не** в
  `state.redis_users` (state хранит только operator-extra), они доливаются в `users.acl`
  из `essence.system_acl_users` на **каждом** рендере (см.
  [«★ Долив на day-2»](#-долив-на-day-2-инвариант)). Bulk-replace operator-extra их **не**
  удаляет; имя системного в `input.users` отвергает `validate`-guard (422).

Два шага (как `add_user`): **re-render** полного `users.acl` (системные из essence +
operator-extra = `input.users` целиком, per-user пароли из Vault) + **hot-reload**
(`community.redis.acl`, `ACL LOAD` — живой инстанс перечитывает `aclfile`; удалённые из
набора юзеры исчезают из `ACL LIST`). `state.redis_users` мутируется **целиком**
(`set: redis_users` = `input.users`, **без** пароля — ИБ). TLS-дискриминатор коннекта —
из `state.tls` (как `add_user`/`restart`). Параметры state `acl` — в
[per-module doc](../../../docs/module/community/redis/README.md#acl--params).

### `rotate_tls` (day-2: ротация TLS cert/key/CA без рестарта)

[`scenario/rotate_tls/main.yml`](scenario/rotate_tls/main.yml) — ротация
TLS-материала Redis **без рестарта**: Redis 6.2+ перечитывает cert/key/CA на лету по
`CONFIG SET tls-*-file`. Оператор задаёт **новые Vault-пути** серверного cert/key/CA
(`cert_ref`/`key_ref`/`ca_ref`, каждый опционален — частичная ротация; не заданный
берёт текущий из `state.tls`). Три шага:

1. **guard** (`assert:`, keeper-side) — TLS обязан быть включён (`state.tls.enable=true`);
   ротация на plaintext-инстансе бессмысленна, `false` обрывает render ДО dispatch.
2. **re-render PEM** в `${conf_dir}/tls/{redis.crt,redis.key,ca.crt}` из новых refs
   (через `vault(ref)` в ячейке `content`, seal-маскинг). Шаг помечен
   `register: tls_certs` — applier-register (orchestration.md §2.1.1): движок эмитит
   синтетическую `core.noop.run` с агрегатом `changed = OR(child.changed)` по дочерним
   `core.file.present` (`redis.crt`/`redis.key`/`ca.crt`).
3. **re-read под `onchanges`** — три `community.redis.command` (`CONFIG SET tls-cert-file` /
   `tls-key-file` / `tls-ca-cert-file`) пересоздают SSL_CTX на живом инстансе, **гейтятся
   `onchanges: [tls_certs]`**: выполняются только когда хоть один PEM реально сменился.

> **★ Идемпотентен по `onchanges`.** Три `CONFIG SET tls-*-file` гейтятся
> `onchanges: [tls_certs]` (applier-register шага 2), **не** `changed: true`. Redis 6.2+
> пересоздаёт SSL_CTX по `CONFIG SET tls-*-file` даже при неизменном пути и перечитывает
> PEM с диска — поэтому используется **`command`** (raw verb), а не `config`-state:
> honest-diff `community.redis.config` счёл бы `CONFIG SET tls-*-file` no-op при том же
> пути и **не** дёрнул бы команду. Гейт `onchanges` даёт converge-семантику: новый ref →
> новый PEM → `core.file.present` `changed` → агрегат `tls_certs.changed=true` → re-read.
> Повторный прогон с тем же материалом → файлы не меняются → `tls_certs.changed=false` →
> три `CONFIG SET` **skipped** → весь сценарий no-op. Это «приведи к состоянию», а не
> «форсни re-read на каждый вызов». Рендер PEM сам по себе тоже идемпотентен (тот же ref
> → тот же файл). `state.tls.*_ref` фиксирует новые refs; `enable`/`only`/`port` не меняются.

### `migrate_cluster` (day-2: миграция с внешнего кластера)

[`scenario/migrate_cluster/main.yml`](scenario/migrate_cluster/main.yml) — **развернуть
новый** Redis-кластер **и залить** в него данные с **внешнего** (старого) кластера через
**нативную репликацию Redis** (`REPLICAOF` на внешний источник). Это **create-подобный**
сценарий (`create: true` — поднимает инфраструктуру с нуля, как `create`), но
дополнительно подключает свежий кластер к внешнему источнику и ждёт, пока он догонит
источник по репликационному offset. Источник остаётся **жив** — финальное отцепление
(`REPLICAOF NO ONE`) делает отдельный day-2 [`detach_source`](#detach_source-day-2-отцепить-внешний-источник)
**после** того, как оператор остановит запись в старый кластер.

**Диспетчер по `redis_type`** (как `create`): `main.yml` — диспетчер, реализованы обе
ветки через conditional-include:

- **sentinel** ([`migrate_cluster/sentinel.yml`](scenario/migrate_cluster/sentinel.yml)) —
  (1) `include redis-deploy-sentinel.yml` (то же общее деплой-тело service-level, что и
  `create/sentinel.yml` — поднять новый кластер; **SENTINEL MONITOR отложен**,
  `sentinel_monitor_now: false`, см. ниже); (2) свежий master реплицирует **внешний**
  источник (`community.redis.replica` с `source_external: true`); (3) **offset-lag gate**
  (`community.redis.offset-synced`, `retry`/`until`) — блокирующе ждёт, пока новый master
  догонит источник в пределах `lag_threshold`. После этого `seeded_from.detached: false`
  (источник ещё подключён — нужен `detach_source`);
- **cluster** ([`migrate_cluster/cluster.yml`](scenario/migrate_cluster/cluster.yml)) —
  слот-aware миграция honest hash-slot Redis Cluster: joinable-деплой новых нод
  (`include redis-deploy-cluster-joinable.yml`) → join-external (реплики старых мастеров
  1:1) → failover-takeover (промоушен новых в мастера) → forget-external (выкинуть старые
  ноды). **Атомарна** в migrate-фазе → detach не нужен (`seeded_from.detached` сразу
  `true`). Операционные предусловия (cluster-bus, выровненный пароль, graceful failover) —
  на операторе (шапка `migrate_cluster/cluster.yml`). Миграция cluster→cluster с
  репликами на стороне источника отвергается (кейс `cluster-migrate-replicas-rejected`).

**★ SENTINEL MONITOR откладывается (split-brain-фикс).** В sentinel-ветке миграции
sentinel-демоны поднимаются деплой-телом (шаг 1: `sentinel.conf` + unit + PONG-gate), но
команду `SENTINEL MONITOR` **не дают** (`sentinel_monitor_now: false`): пока идёт заливка,
новый master в роли slave, ранний MONITOR увидел бы slave-master и инициировал бы failover
→ split-brain. MONITOR выполняет `detach_source` **после** `REPLICAOF NO ONE` (новый master
снова master).

**★ Учётка внешнего источника.** Пароли сервиса — по конвенции `secret/redis/<incarnation>`
(keeper-side `vault()`). **Исключение** — учётка **внешнего** источника: её Vault-**путь**
(не значение) задаёт оператор в `input.source.password_ref` под жёстким pattern-guard
`secret/redis/migrate/*` (источник чужой, его секрет не лежит по нашей конвенции). `migrate`
**персистит** эти ссылки в `state.seeded_from.{source_password_ref, source_tls_ca_ref}`
(v10) — чтобы `detach_source` смог сделать финальный offset-gate к AUTH/TLS-источнику.

**★ Граница (НЕ входит): квиес записи в старый кластер.** Soul Stack не управляет
клиентским трафиком приложений — остановку записи в источник перед финальным `detach`
делает **сам оператор**. Данные, записанные в источник **после** offset-snapshot
`detach_source`, будут потеряны, если оператор не остановил запись.

L0-кейсы — [`migrate_cluster/tests/`](scenario/migrate_cluster/tests/):
[`sentinel-migrate-1master-1replica`](scenario/migrate_cluster/tests/sentinel-migrate-1master-1replica/case.yml),
[`cluster-migrate-2shards`](scenario/migrate_cluster/tests/cluster-migrate-2shards/case.yml),
[`sentinel-migrate-provision-enabled`](scenario/migrate_cluster/tests/sentinel-migrate-provision-enabled/case.yml)
(provision + миграция в один прогон),
[`cluster-migrate-replicas-rejected`](scenario/migrate_cluster/tests/cluster-migrate-replicas-rejected/case.yml).
Состояние плагина (`replica` `source_external`, `offset-synced`) — в
[per-module doc](../../../docs/module/community/redis/README.md).

### `detach_source` (day-2: отцепить внешний источник)

[`scenario/detach_source/main.yml`](scenario/detach_source/main.yml) — **отцепить**
внешний (старый) кластер от инкарнации, залитой через `migrate_cluster`. После того как
новый кластер догнал источник по offset **и оператор остановил запись** в старый кластер,
сценарий делает финальную проверку offset ещё раз (fail-closed) и переводит новый master в
самостоятельный режим (`REPLICAOF NO ONE`) — источник больше не нужен.

**Диспетчер по `incarnation.state.redis_type`** (day-2 знает режим из state, не из input —
входного `redis_type` у `detach` нет): реализована ветка **sentinel**
([`detach_source/sentinel.yml`](scenario/detach_source/sentinel.yml)); ветка **cluster** —
fail-closed `assert`-заглушка ([`detach_source/cluster.yml`](scenario/detach_source/cluster.yml)):
cluster-миграция атомарна в migrate-фазе, отдельный detach ей не нужен.

- **Источник — из state, не из input:** берётся из
  `incarnation.state.seeded_from.source_endpoints[0]` (записан `migrate_cluster`) — оператор
  **не** передаёт его повторно (исключает «detach не того источника»). Входного контракта у
  `detach_source` **нет** (`input: {}`) — источник, режим и коннект-данные из `state`.
- **Учётка источника — из ссылок в state (v10):** финальный offset-gate коннектится к
  внешнему источнику; пароль/CA источника восстанавливаются из
  `state.seeded_from.{source_password_ref, source_tls_ca_ref}` через `vault()` keeper-side.
  До v10 эти ссылки не хранились → gate к AUTH/TLS-источнику падал коннектом → detach
  никогда не проходил (`error_locked`); v10 это закрыл.
- **Guard на не-мигрированную инкарнацию:** `detach` — обычный day-2, оператор может
  запустить его и на `create`'нутой инкарнации, которая **никогда не мигрировала** (у неё
  `seeded_from.source_endpoints` пуст). Такой случай отсекает `assert`-guard в `sentinel.yml`
  внятным отказом «отцеплять нечего», а не опаковым index-out-of-bounds.
- **Идемпотентность:** detach уже отцеплённой инкарнации (`seeded_from.detached == true`) —
  `REPLICAOF NO ONE` на самостоятельном master no-op (плагин `community.redis.detached`
  идемпотентен).

L0-кейсы — [`detach_source/tests/`](scenario/detach_source/tests/):
[`sentinel-detach-source`](scenario/detach_source/tests/sentinel-detach-source/case.yml),
[`sentinel-detach-source-auth`](scenario/detach_source/tests/sentinel-detach-source-auth/case.yml)
(AUTH/TLS-источник — v10-фикс),
[`empty-seeded-source`](scenario/detach_source/tests/empty-seeded-source/case.yml)
(guard на не-мигрированную).

### `destroy` (teardown cloud-provisioned VM)

[`scenario/destroy/main.yml`](scenario/destroy/main.yml) — **снять VM**, поднятые
create/migrate-прогоном через cloud-provision ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md)),
и каскадно вычистить keeper-реестры этих хостов. **Lifecycle**-сценарий, **не** runnable из
Run-формы — terminal-флоу `DELETE /v1/incarnations/{name}` (keeper трактует имя `destroy`
как фазу жизненного цикла, режим `TerminalDestroy`; симметрично `create` — bootstrap).

- **Что снимать — факт из state, не input** (`input: {}`): `destroy` читает обратно
  `provisioned_vm_ids` (provider-id для `core.cloud.destroyed`), `provisioned_sids`
  (Keeper-id для cascade) и `provisioned_provider`, записанные provision-прогоном.
- **Cascade одной PG-транзакцией:** `PluginHost.Destroy(vm_ids)` + `souls → destroyed`,
  активные `soul_seeds → orphaned`, активные `bootstrap_tokens → burned`. Cascade нужен,
  т.к. Reaper **не** снимает pending-VM-souls сам — без `sids` записи повисли бы
  осиротевшими (нарушение anti-replay bootstrap-токенов).
- **★ Парность `sids ↔ vm_ids` (assert):** оба читаются **парой** (рассогласование длин =
  orphan). `provision` пишет оба одним guard-ом (один источник размера), поэтому в норме
  длины равны; render-time `assert` ловит порчу state (ручная правка / баг) **ДО**
  необратимого destroy. На не-провиженной инкарнации обе стороны default `[]` → `0 == 0` →
  вакуумно true.
- **★ Group-drop на не-провиженных:** на инкарнации, развёрнутой **без** provision (deploy
  на существующие VM), снимать нечего. Гейт `when: size(provisioned_vm_ids) > 0` на задаче
  `destroyed` статичен (зависит только от `incarnation.*`) → render-phase **group-drop**:
  на пустом `provisioned_vm_ids` задача физически не попадает в план. Teardown без
  cloud-вызова проходит.
- **★ `state_changes` отсутствуют намеренно:** `destroy` терминальный — инкарнация удаляется
  (`auto_destroy`), чистить `provisioned_*` в state бессмысленно (state уходит вместе с
  инкарнацией).

L0-кейсы — [`destroy/tests/`](scenario/destroy/tests/):
[`destroy-provisioned`](scenario/destroy/tests/destroy-provisioned/case.yml),
[`destroy-not-provisioned`](scenario/destroy/tests/destroy-not-provisioned/case.yml)
(group-drop задачи `destroyed`),
[`destroy-sids-vmids-mismatch`](scenario/destroy/tests/destroy-sids-vmids-mismatch/case.yml)
(assert-guard парности).

## Безопасность

Пароли — **из Vault**, не во входном контракте сценария. Сценарий читает их
keeper-side CEL-функцией `vault(...)` в render-фазе (templating.md §2.3/§4) по
конвенции:

- requirepass: `secret/redis/<incarnation.name>#password`;
- per-user (operator-extra **и** системные `replica`/`monitoring`/`sentinel`/`haproxy`):
  `secret/redis/<incarnation.name>/users/<name>#password`;
- sentinel-демон `default`-юзер (в `sentinel-users.acl`): главный
  `secret/redis/<incarnation.name>#password` (общий с requirepass redis-server).

Путь строится из доверенного контекста (incarnation, не operator-input). В destiny
и в плагин через `apply.input` / `params` уходит уже **зарезолвленное значение** —
пароль доезжает на хост значением, а не ссылкой; Soul vault-клиент не тянет
(ADR-012). В git нет ни значения, ни operator-указателя на секрет. В `users.acl`
**и** `sentinel-users.acl` пароль пишется **хешем** (`#<sha256>`), plaintext в файл
не попадает. Оба файла — `mode 0640`, owner/group `redis` (читает только сервис).
Плагин `community.redis` не логирует `params["password"]` (ADR-010).

> **★ Исключение: `sentinel auth-pass` в `sentinel.conf` — plaintext на диске.**
> Пароль, которым sentinel-демон аутентифицируется на monitored master
> (`sentinel auth-pass <master> <pass>`), пишется в `sentinel.conf` **в открытом виде** —
> это требование протокола Sentinel (хеш `#<sha256>` он **не** принимает, в отличие от
> ACL-aclfile). Поле `sentinel.auth_pass` помечено `secret: true` (маскируется в
> логах/трейсах/state), сам файл — `mode 0640` owner `redis`, но на диске значение
> хранится открытым. Это единственное место, где Redis-секрет лежит plaintext-ом
> (вынужденно, by-protocol), — в отличие от хешей в обоих aclfile.

**TLS-PEM — Vault-ПУТИ, не литеральный PEM.** Оператор задаёт `tls.cert_ref` /
`tls.key_ref` / `tls.ca_ref` — Vault-**пути** (форма `<mount>/<path>#<field>`,
бьют essence-дефолты `essence.tls_*_ref`), **не** сам PEM. destiny читает PEM
CEL-функцией `vault(ref)` **прямо в ячейке** `content` задачи `core.file.present`
(не `.tmpl`-пустышка, не проброс уже-резолвленного PEM через `apply.input`): детектор
seal помечает ячейку sealed vault-слоем в render-фазе destiny-прохода (ADR-010 §7.4),
маскинг скрывает PEM в `error_summary`/`state`. PEM уезжает в файлы
`/etc/redis/tls/{redis.crt,redis.key,ca.crt}` (mode `0600`, owner `redis`); vault-клиент
на Soul не тянется (ADR-012). Литеральный operator-PEM не поддержан намеренно: он шёл
бы через `apply.input`, а seal destiny-прохода схему secret-input не видит → PEM не
маскировался бы.

## Прогон L0

L0-испытание (Trial, ADR-023), render-only, герметично — из каталога `keeper/`:

```sh
# sentinel single-host (standalone-эквивалент: 1 master + sentinel-демон)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/full-stack/case.yml
# cluster
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/cluster-create-3shards/case.yml
# sentinel
go run ./cmd/soul-trial run ../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml
# add_node (day-2)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/add_node/tests/add-replica-explicit-master/case.yml
# remove_node (day-2)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/remove_node/tests/remove-node-from-cluster/case.yml
# reshard (day-2, НЕ идемпотентен)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/reshard/tests/reshard-slots-from-to/case.yml
# restart (day-2, rolling)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/restart/tests/rolling-restart-replicas/case.yml
# add_user (day-2, точечный upsert одного юзера)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/add_user/tests/add-user-plaintext/case.yml
# update_users (day-2, bulk-replace всего набора operator-extra)
go run ./cmd/soul-trial run ../examples/service/redis/scenario/update_users/tests/bulk-replace-removes-user/case.yml
```

[Кейс `create/tests/full-stack`](scenario/create/tests/full-stack/case.yml)
проверяет план sentinel single-host (`replicas_per_master: 0` — standalone-эквивалент):
задачи destiny `redis` (install + render `users.acl` + render `redis.conf` +
`sentinel.conf` + systemd hardening drop-in + running + restarted) + задача
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
  — `shards=3`, `replicas_per_master=0` (3 хоста): проверяет
  cluster-директивы в render `redis.conf` (`cluster-enabled`/`cluster-config-file`/
  `cluster-node-timeout`/`cluster-announce-ip`), детерминированный `nodes`-MAP по
  SID и наличие `community.redis.cluster` (`action: create`) в плане; sentinel-
  ветка погашена placeholder-skip-ом.
- [`cluster-create-2shards-1replica`](scenario/create/tests/cluster-create-2shards-1replica/case.yml)
  — `shards=2`, `replicas_per_master=1` (4 хоста): non-zero replicas в `nodes`-MAP;
  `cluster_node_timeout` из input (`8000`, не дефолт).

Sentinel-кейсы под [`scenario/create/tests/`](scenario/create/tests/):

- [`sentinel-create-1master-2replica`](scenario/create/tests/sentinel-create-1master-2replica/case.yml)
  — `replicas_per_master=2` (3 хоста): master-election (первый по SID = master),
  `community.redis.replica` (`master_addr` host-инвариантен),
  `community.redis.sentinel` (`monitor.ip` host-инвариантен, `quorum` авто `size/2+1`),
  `redis_sentinel` в state.
- [`sentinel-no-replicas-auto-quorum`](scenario/create/tests/sentinel-no-replicas-auto-quorum/case.yml)
  — `replicas_per_master=0` (standalone-эквивалент, один хост): auto-quorum `size/2+1`,
  дефолтный `master_name` из essence.

TLS- и install-кейсы под [`scenario/create/tests/`](scenario/create/tests/):

- [`tls-enabled`](scenario/create/tests/tls-enabled/case.yml)
  — `tls.enable: true` (плюс `tls.only`): TLS-PEM-задачи активны, `tls-port`/
  `tls-cert-file`/… в merged `redis.conf`, `port 0` при `only`.
- [`tls-enabled-no-only`](scenario/create/tests/tls-enabled-no-only/case.yml)
  — `tls.enable: true`, `tls.only` не задан: TLS-порт открыт, plain-порт остаётся.
- [`tls-essence-refs`](scenario/create/tests/tls-essence-refs/case.yml)
  — TLS включён галочками (`tls_enabled`/`tls_keep_plain`), порт и Vault-пути PEM —
  из essence (нестандартный `tls_port` 7400, refs `secret/ops/redis/tls`).
- [`tls-disabled`](scenario/create/tests/tls-disabled/case.yml)
  — `tls` не передан: TLS off, PEM-задачи placeholder-skip, директив TLS нет.
- [`tls-cluster`](scenario/create/tests/tls-cluster/case.yml) — TLS + cluster:
  `tls-replication`/`tls-cluster yes` в merged config.
- [`install-package`](scenario/create/tests/install-package/case.yml) — default-ветка
  `install.method=package`: distro-пакет с `=version`-пином.
- [`install-binary`](scenario/create/tests/install-binary/case.yml) —
  `install.method=binary`: `core.url.fetched` (tarball по `base_url`+`version`+arch)
  → `core.archive.extracted` → install бинарей + свой systemd-юнит; package-ветка
  placeholder-skip.
- [`modules-no-checksum`](scenario/create/tests/modules-no-checksum/case.yml)
  — полный набор Redis-модулей: загрузка `.so` по content-идемпотентности
  (без integrity-verify).

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

Day-2 hot-reload реализован: `update_config` (live `CONFIG SET` дельты, → state
`config`), `add_user` (точечный upsert одного юзера, → state `redis_users`, `ACL LOAD`),
`update_users` (bulk-replace всего набора operator-extra, → state `redis_users`,
`ACL LOAD`), `rotate_tls` (→ state `command`, re-read SSL_CTX под `onchanges: [tls_certs]`
— идемпотентно). Реализованы и **миграция с внешнего кластера** —
[`migrate_cluster`](#migrate_cluster-day-2-миграция-с-внешнего-кластера) (sentinel +
cluster) + day-2 [`detach_source`](#detach_source-day-2-отцепить-внешний-источник)
(sentinel; cluster-detach не нужен — миграция атомарна) — и **teardown**
[`destroy`](#destroy-teardown-cloud-provisioned-vm) (lifecycle, снимает cloud-provisioned
VM + cascade-чистка реестров). Следующие батчи эпика redis-консолидации (в этом сервисе
**пока не реализованы**):

- day-2 sentinel: failover (switchover) и прочие day-2-операции sentinel-топологии;
- плагинный state `community.redis.failover` (`command` / `pinged` / `role` /
  `replica-synced` / `config` / `acl` / `cluster` (create/add-node/remove-node/
  reshard) / `replica` / `sentinel` уже есть);
- TLS sentinel-демона (`:26379`): data-плоскость redis-server TLS уже реализована
  (operator-dict `tls.enable`/`tls.only`, бьёт essence), TLS для sentinel-демона —
  follow-up;
- **cloud-provision (`input.provision`) — keeper-side реализован, live ждёт C1**: доставка
  bootstrap-токена реализована keeper-side (модуль `core.bootstrap.delivered`, [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md),
  SSH-доставка). Поток render/L0-валиден и проходит unit-тесты, но живой провизион «VM→redis
  в один прогон» ждёт слайса **C1** (cloud-init CA-signed host-key для verify host-cert без
  TOFU) + live-e2e — см. [«Cloud-provision»](#cloud-provision-create-поднимает-vm-live-ждёт-c1).
  Teardown снятых VM — сценарий [`destroy`](#destroy-teardown-cloud-provisioned-vm)
  (реализован);
- **version-aware валидатор `redis_settings` в day-2 `update_config`** — пока только в
  `create` (см. [«Валидатор имён директив»](#валидатор-имён-директив-redis_settings)).

Состояние плагина `community.redis` (какие states реализованы) — в его per-module
doc [`docs/module/community/redis/README.md`](../../../docs/module/community/redis/README.md).
