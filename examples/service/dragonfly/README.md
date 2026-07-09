# dragonfly — сервис DragonFly (redis-совместимый in-memory store)

DragonFly — single-binary in-memory store, **совместимый с Redis на проводе**
(PING/REPLICAOF/SENTINEL/INFO работают) и с **нативными Prometheus-метриками**
(встроенный эндпоинт). Сервис разворачивает DragonFly в **sentinel-режиме**
(master-replica + sentinel-демон); `replicas_per_master: 0` даёт standalone-эквивалент
(один master + sentinel-демон).

Оператор задаёт **простые типизированные понятия** (память, реплики, TLS, ACL-юзеры),
а сценарий `create` **транслирует** их во flagfile `dragonfly.conf` (DF-флаги,
underscore-форма). Живой рантайм идёт через плагин
[`community.redis`](../../../docs/module/community/redis/README.md) — правок не требует,
DragonFly redis-совместим.

Разделение обязанностей (ADR-009):

- **destiny [`dragonfly`](../../destiny/dragonfly/)** — per-host install + render
  data-плоскости (flagfile `dragonfly.conf`, `users.acl`, TLS-PEM, systemd, host-tuning).
- **destiny [`redis`](../../destiny/redis/)** в режиме **sentinel_only** (`deploy_redis: false`)
  — sentinel-демон над DragonFly-master-ом. DragonFly **не несёт** `redis-sentinel`,
  поэтому демон ставит distro-пакет `redis-server` через destiny `redis`. В `create` —
  **два** `apply: destiny` (dragonfly + redis).
- **scenario сервиса** — трансляция ввода во flagfile + оркестрация (порядок/таргетинг/health-gate).
- **плагин `community.redis`** — интерфейс к живому DragonFly (PING/REPLICAOF/SENTINEL/ACL).

> **★ PILOT-скоуп.** Только **sentinel**-режим. `cluster` вне скоупа (DragonFly cluster —
> emulated, отдельный слайс). Нет persistence-пресетов (DF persistence — `snapshot_cron`,
> отложена), нет redis-модулей (`.so`), нет `maxmemory_policy` (см.
> [«Трансляция»](#трансляция-простого-ввода-во-df_config)).

## state_schema

[`service.yml → state_schema`](service.yml), `state_schema_version: 6`. Цепочка миграций
forward-only (ADR-019):

- [`001_to_002`](migrations/001_to_002.yml) — `install` + host-layout (`conf_dir`/`data_dir`)
  вынесены из state в `essence` (read-model без читателей / операционные сценарии читают essence напрямую).
  У DragonFly **нет** `modules_base_url` (redis-модули к DF неприменимы) — вырезаются три
  поля, не четыре;
- [`002_to_003`](migrations/002_to_003.yml) — cloud-provision read-model: `provisioned_vm_ids`
  (provider-id созданных VM) + `provisioned_provider`;
- [`003_to_004`](migrations/003_to_004.yml) — cascade-destroy read-model: `provisioned_sids`
  (Keeper-side SID/FQDN созданных VM для teardown);
- [`004_to_005`](migrations/004_to_005.yml) — **обязательный мониторинг** data-плоскости
  (Слайс II, [ADR-024](../../../docs/adr/0024-observability.md)): read-model `monitoring`
  (версия/listen node_exporter). Существующим v4 — консервативный default (версия `''`, порт `:9100`);
- [`005_to_006`](migrations/005_to_006.yml) — **обязательный log-shipping** data-плоскости
  (Слайс V-I, [ADR-067](../../../docs/adr/0067-vector-log-shipping.md)): read-model `logging` (версия/sink/источники vector). `sink_auth_ref`
  сюда **не** пишется (секрет в Vault). Существующим v5 — default (версия `''`, sink `console`).

`incarnation.state` фиксирует, что развёрнуто (`required: [df_config]`):

| поле | тип | смысл |
|---|---|---|
| `df_version` | string | эффективная версия DragonFly (distro-пин при `package`, upstream-semver при `binary`) |
| `df_config` | object | **итог трансляции** — merged flagfile-map `dragonfly.conf` (ключи — DF-флаги, underscore) |
| `tls` | object `{enable, cert_ref, key_ref, ca_ref}` | TLS-намерение + Vault-**пути** PEM (не сам PEM). ★ Без `only`/`port`: у DF TLS на **основном** порту |
| `memory_mb` | integer | бюджет памяти под DragonFly, МБ (из `input.memory_mb`; не задан → 0) |
| `sysctl_settings` | map string→string | применённые kernel-параметры (host-tuning, из essence) |
| `monitoring` | object `{node_exporter_version, node_exporter_listen}` | **(v5)** read-model развёрнутого node_exporter (Слайс II). ★ Без redis_exporter — DF отдаёт метрики нативно |
| `logging` | object `{vector_version, vector_sink_type, vector_sink_endpoint, vector_log_sources}` | **(v6)** read-model развёрнутого vector (Слайс V-I). ★ Без `sink_auth_ref` — секрет в Vault |
| `replicas` | integer | реплик master-а (из `input.replicas_per_master`) |
| `sentinel_quorum` | integer | всегда `0` (auto `size/2+1` вычисляется в apply, в state не материализуется) |
| `df_users` | array `AclUser` | operator-extra ACL-юзеры (`[{name, perms, state}]`, [`types.yml`](types.yml), ADR-062). Пароли — не в state (Vault). Системные служебные сюда **не** пишутся |
| `df_hosts` | array `{sid, role}` | хосты топологии (пишется `[]`; точные роли — follow-up) |
| `df_sentinel` | object `{master_name, master_ip, quorum, down_after_ms, failover_timeout_ms}` | факты sentinel-режима. `master_ip` create не пишет (host-вариативен), `quorum` — `0` (auto) |
| `provisioned_vm_ids` / `provisioned_provider` / `provisioned_sids` | array / string / array | **(v3/v4)** cloud-provision read-model (см. [«Cloud-provision»](#cloud-provision)) |

## Входной контракт оператора

[`scenario/create/main.yml → input:`](scenario/create/main.yml) — строго типизированный
структурный ввод (Named Dict):

| поле | тип | смысл |
|---|---|---|
| `install_method` | enum `package`/`binary`, default `package` | `package` — distro-deb `dragonfly` (`=version`-пин); `binary` — upstream-tarball (URL/версия из `essence.binary_base_url`/`binary_version`, имя файла `dragonfly-<arch>.tar.gz` строит destiny из `soulprint.self.os.arch`) |
| `version` | string, `required_when` `install_method==package` | distro-native пин пакета `dragonfly` (`^([0-9]+:)?[0-9]…`). При `binary` **не** используется |
| `memory_mb` | integer, опц., min `64` | бюджет памяти под DragonFly, МБ; `maxmemory` = доля от него. Пол `min 64` не случаен: при слишком малом усечение даёт `"0mb"` (без лимита) |
| `replicas_per_master` | integer, опц., default `0`, min `0` | реплик master-а. roster = `1 + replicas_per_master` (sentinel-демон на каждом хосте). `0` — standalone-эквивалент |
| `sentinel_down_after_ms` | integer, опц., default `5000` | сколько мс master молчит, прежде чем sentinel считает его упавшим (`sentinel down-after-milliseconds`) |
| `sentinel_failover_timeout_ms` | integer, опц., default `60000` | таймаут одной попытки failover (`sentinel failover-timeout`) |
| `users` | array `AclUser` | operator-extra ACL-юзеры (`[{name, perms, state}]`, [`types.yml`](types.yml), ADR-062). `perms` — полная Redis-ACL-строка (DragonFly её принимает), валидируется re2-паттерном. Имя **не может** совпасть со служебным (см. [«Системные ACL-юзеры»](#системные-acl-юзеры)) |
| `df_settings` | object (passthrough, key→value строки) | произвольные DF-флаги `dragonfly.conf` (underscore-форма) поверх дефолтов/вычислений (last-wins). ★ PILOT: имена **не** валидируются каталогом — опечатка проявится при старте DF, не на render |
| `tls_enabled` | boolean, опц., default `false` | включить TLS DragonFly. ★ TLS встаёт на **основной** порт 6379 (флаг `--tls`; у DF нет отдельного tls-порта → нет `tls_keep_plain`/«только-TLS»). Vault-пути PEM — из `essence.tls_*_ref` |
| `provision` | object, опц., **default-on** `{enabled: true}` | поднять VM под топологию в том же create-прогоне (см. [«Cloud-provision»](#cloud-provision)) |

**Чего во входном контракте нет** (essence-параметры или авто-вычисление):

- `maxmemory_policy` — у DragonFly **нет** такого флага (имя — производное `INFO`/`CONFIG`-поле
  от bool `--cache_mode`). Per-policy выбор redis-имён (`lru`/`lfu`/…) DF не транслирует —
  eviction вне скоупа PILOT;
- `sentinel_quorum` — **авто** `size(hosts)/2+1` (вычисляется в apply);
- `sentinel_master_name` — `essence.sentinel_master_name` (дефолт `master`);
- `conf_dir` / `data_dir` / `run_dir` — `essence` (author-context, host-layout);
- Vault-пути TLS (`tls_cert_ref`/`tls_key_ref`/`tls_ca_ref`) — `essence`;
- `binary_base_url` / `binary_version` — `essence` (при `install_method: binary`).

**Кросс-полевой инвариант** ([`create/main.yml → validate:`](scenario/create/main.yml),
input-only, 422 до applying): имя `input.users` **не** входит в
`[default_admin, replica, monitoring, sentinel, haproxy]` — operator-extra не может занять
имя служебного (его молча перекрыл бы долив из essence).

## Трансляция простого ввода во df_config

`compute.df_config` сливает слои через `merge()` (SHALLOW last-wins, слева направо;
[templating §2.3](../../../docs/templating.md)):

1. `essence.df_config` — дефолты автора (`maxmemory: "256mb"`, `maxclients: 10000`);
2. вычисленный `maxmemory` (has-guard) — `memory_mb * memory_reserve_percent / 100` (МБ,
   резерв 25% под ОС); напр. `memory_mb: 1024` → `maxmemory: "768mb"`;
3. `input.df_settings` — passthrough оператора;
4. TLS-блок (при `tls_enabled`) — DF-флаги `tls`/`tls_cert_file`/`tls_key_file`/
   `tls_ca_cert_file`/`tls_replication` (underscore-форма, значения PEM-путей из `${conf_dir}/tls/`).

**Без** persistence-пресета (отложена), **без** cluster-директив (sentinel-only), **без**
`maxmemory_policy` (у DF нет флага — `absl` FATAL на неизвестном). Merge-подложка и
данные-таблицы — [`essence/_default.yaml`](essence/_default.yaml).

## Host-tuning extras

Режим-агностичные per-host добавки — **безусловный baseline** (не операторский выбор):
отключение Transparent Huge Pages (drop-in `disable-thp.service`), logrotate
(`/var/log/dragonfly/*.log`), sysctl (`core.sysctl.applied` → `/etc/sysctl.d/30-dragonfly.conf`).
Набор sysctl-параметров переиспользован из сервиса `redis` (те же рекомендации in-memory-store:
overcommit/swappiness/сетевые буферы/бэклоги), данные-таблица —
[`essence/_default.yaml → sysctl_settings`](essence/_default.yaml).

## Системные ACL-юзеры

Помимо operator-extra (`input.users`) сервис **всегда доливает** служебных ACL-юзеров:
`default_admin` (полные права `~* &* +@all`), `replica` (репликация PSYNC), `monitoring`
(метрики), `sentinel` (AUTH sentinel↔df), `haproxy` (health-check). perms живут в
[`essence/_default.yaml`](essence/_default.yaml) двумя наборами → **два** aclfile: `users.acl`
(DragonFly, `system_acl_users`) и `sentinel-users.acl` (sentinel-демон, `system_acl_users_sentinel`).

**★ Редизайн `default_admin`** (симметрия с redis). `requirepass` убран из flagfile; **вся**
внутрикластерная аутентификация (репликация `masterauth`, sentinel monitor/auth, health-PING)
идёт под системным юзером `default_admin`. Плагин `community.redis` коннектится
`username=default_admin`. Встроенный DragonFly-юзер `default` рендерится `off` (в наборах его
нет), пока оператор не объявит его в `input.users`.

Во всех задачах, рендерящих `users.acl` (create + операционные `add_user`/`update_users`), набор
собирается двойным `merge()`: системные из essence (нижний слой) + operator-extra (поверх,
last-wins). Системные **не лежат в state** — доливаются из essence на **каждом** рендере,
иначе перерендер стёр бы `replica`/`sentinel` и сломал репликацию.

## Сценарии

### `create` (sentinel-режим, inline-тело)

[`scenario/create/main.yml`](scenario/create/main.yml) — один режим, без диспетчера; тело
прогона инлайн. Шаги:

1. **generate-if-absent** (`core.vault.kv-present`, `on: keeper`) — create сам генерит
   недостающие пароли крипто-случайно (32 alphanumeric) для всех системных + operator-extra
   юзеров. Пассаж-инвариант: запись в Vault **до** render-фазы задач, читающих те же секреты
   через `${ vault(...) }` (ADR-056);
2. **cloud-provision** (conditional, при `provision.enabled` — см. ниже);
3. **size-guard** — render-time `assert: size(soulprint.hosts) == 1 + replicas_per_master`
   (keeper-side, обрывает render до установки; `validate` не годится — нужен `soulprint.hosts`);
4. **`apply: destiny: dragonfly`** — install + render `dragonfly.conf`/`users.acl` + systemd.
   `masteruser`/`masterauth` доливаются во flagfile обычными флагами (персист replica→master
   AUTH через рестарт — `CONFIG SET` не сохраняется);
5. **health-gate PING** (`community.redis.command`) — через **UNIX-сокет** `compute.local_addr`
   (`unix:${run_dir}/dragonfly.sock`): DF при `--bind=primary_ip` **не** слушает loopback,
   локальные вызовы идут на сокет;
6. **`apply: destiny: redis` (sentinel_only)** — sentinel-демон (`deploy_redis: false`) над
   DragonFly-master-ом; `version` — distro-пин пакета `redis-server`
   (`essence.sentinel_redis_package_version`);
7. **REPLICAOF** (`community.redis.replica`, `where:` исключает master по SID) — реплики следуют
   за избранным master (`soulprint.hosts[0]`);
8. **SENTINEL MONITOR** (`community.redis.sentinel`, на каждом хосте);
9. **health-gate PONG** на `:26379` (same-task `register.self`, ADR-056);
10. **node-exporter** и **vector** — обязательный мониторинг/log-shipping (см.
    [«Observability»](#observability)).

### Cloud-provision

**Default-on** ([ADR-061](../../../docs/adr/0061-onboarding-await-and-midrun-reresolve.md),
Вариант A): `input.provision` по умолчанию `{enabled: true}` — один create-прогон поднимает VM
под топологию **и** деплоит DragonFly. Общее тело —
[`scenario/dragonfly-provision.yml`](scenario/dragonfly-provision.yml): (а) cloud-create
(`core.cloud.created`, `on: keeper`; число VM выводится из топологии `1 + replicas_per_master`,
отдельного `node_count` нет), (б) доставка per-VM bootstrap-токена по SSH
(`core.bootstrap.delivered`, [ADR-063](../../../docs/adr/0063-bootstrap-token-delivery.md)),
(в) блокирующее ожидание онбординга (`core.soul.registered` `await_online` + `refresh_soulprint`)
→ roster пере-резолвится, size-guard/деплой видят созданные хосты. `provider`/`profile`/таймауты
фоллбэкают на `essence.provision_*`; секция скрыта из Run-формы. Чтобы катить на **готовый**
roster — `provision: {enabled: false}` явно.

### Операционные сценарии

- **[`add_user`](scenario/add_user/main.yml)** — добавить/переопределить **одного** ACL-юзера
  без рестарта (hot-reload `ACL LOAD` через `community.redis.acl`). Мержит в `state.df_users`,
  рендерит полный `users.acl`;
- **[`update_users`](scenario/update_users/main.yml)** — **bulk-replace** всего набора
  operator-extra (юзер, отсутствующий в новом массиве, удаляется). Системные не трогаются
  (доливаются из essence);
- **[`restart`](scenario/restart/main.yml)** — rolling-restart без смены конфига. Роль каждого
  хоста берётся живым probe (`community.redis.role`), реплики по одной (`serial: 1`), master —
  последним. Рестартится только data-плоскость DragonFly (`core.service.restarted` юнита
  `dragonfly`); sentinel-демон не трогается;
- **[`rotate_tls`](scenario/rotate_tls/main.yml)** — ротация cert/key/CA без рестарта. destiny
  перерендеривает PEM из новых Vault-refs → три `CONFIG SET` **`tls_cert_file`/`tls_key_file`/
  `tls_ca_cert_file`** (★ underscore-форма — DragonFly-специфика, не дефис как у redis) под
  `onchanges`. Предусловие: TLS уже включён (`assert` `state.tls.enable`);
- **[`destroy`](scenario/destroy/main.yml)** — teardown (terminal-флоу `DELETE`, не runnable).
  Две ветки: **cloud-cascade** (при `provisioned_vm_ids > 0` — `core.cloud.destroyed` + cascade
  реестров souls/seeds/tokens) и **Soul-side** (остановить сервисы + удалить `/etc`-артефакты
  по `install_method`).

## Observability

Обе поверхности — **обязательный инвариант data-сервиса**, не операторский выбор: `create`
разворачивает их **безусловно** (без `when`-гейта) на **каждый** хост в конце прогона,
композицией переиспользуемых standalone-destiny через `apply: destiny` (изолированный render,
ADR-009). Версии/порты — author-context в `essence` (оператор переопределяет в `spec.essence`);
`arch` destiny берёт из `soulprint.self.os.arch`.

### node-exporter (метрики хоста, pull — Слайс II)

Шаг 6 деплоя (**после** deploy-ветки) безусловно ставит
[`node-exporter`](../../destiny/node-exporter/) ([ADR-024](../../../docs/adr/0024-observability.md)).
essence: `node_exporter_version` (`1.8.2`), `node_exporter_listen` (`:9100`),
`node_exporter_base_url` (WB Nexus raw-proxy), `node_exporter_allow_private` (`true` — SSRF-guard
opt-out под private-resolve зеркала). Read-model — `state.monitoring`
(`node_exporter_version`/`node_exporter_listen`), зеркалит `apply.input` destiny node-exporter.

> **★ node-exporter-ONLY (by design).** DragonFly отдаёт data-plane метрики **нативно**
> (встроенный Prometheus-эндпоинт) → аналог `redis_exporter` **не** ставится (в отличие от
> сервиса `redis`, где redis_exporter обязателен).

### vector (отгрузка логов, push — Слайс V-I)

Шаг 7 деплоя (**после** экспортера) безусловно ставит [`vector`](../../destiny/vector/) ([ADR-067](../../../docs/adr/0067-vector-log-shipping.md)) — агент
отгрузки логов, дополняющий метрики-плоскость лог-плоскостью. essence: `vector_version`
(`0.40.0`), `vector_sha256` (★ плейсхолдер — реальный оператор **обязан** подставить checksum
под пару `(version, arch)` в `spec.essence`, иначе fail-closed), `vector_base_url` (WB Nexus),
`vector_allow_private` (`true`), `vector_sink_type`/`vector_sink_endpoint`/`vector_sink_auth_ref`,
`vector_log_sources`. Read-model — `state.logging` (`vector_version`/`vector_sink_type`/
`vector_sink_endpoint`/`vector_log_sources`) **без** `sink_auth_ref` (секрет резолвится
Soul-side, в state не оседает — как `tls.*_ref`).

**Две лог-плоскости** (`vector_log_sources`):

- `/var/log/dragonfly/*.log` — сам DragonFly (glog, destiny `dragonfly` `--log_dir`);
- `/var/log/redis/*.log` — sentinel-демон (distro `redis-server`, destiny `redis`).

**Sink — Вариант A** (essence per-incarnation): дефолт `sink_type: console` (безопасно, без
внешней инфры), `sink_endpoint`/`sink_auth_ref` пустые — оператор задаёт реальный коллектор
(loki/elasticsearch/vector) в `spec.essence`. Именование агента — **Слайс V-I vector /
[naming-rules.md](../../../docs/naming-rules.md) §15** (upstream-имя продукта Vector.dev, как
`node-exporter`).

## Безопасность

Пароли — **из Vault**, не во входном контракте. Сценарий читает их keeper-side CEL-функцией
`vault(...)` в render-фазе по **единой** конвенции (все системные, вкл. `default_admin`, **и**
operator-extra):

```
secret/dragonfly/<incarnation.name>/users/<name>#password
```

Главного `requirepass`-секрета **нет** (редизайн `default_admin`). Путь строится из доверенного
контекста (incarnation, не operator-input); в destiny и плагин через `apply.input`/`params`
уходит уже **зарезолвленное значение** — Soul vault-клиент не тянет (ADR-012). В `users.acl`
пароль пишется **хешем** (`#<sha256>`). TLS-PEM — Vault-**пути** (`tls_*_ref`), destiny читает
PEM через `vault(ref)` прямо в ячейке `content` (seal-маскинг), PEM уезжает в
`${conf_dir}/tls/{dragonfly.crt,dragonfly.key,ca.crt}`.

## Прогон L0

L0-испытание (Trial, render-only, герметично) — из каталога `keeper/`:

```sh
# sentinel single-host (standalone-эквивалент: 1 master + sentinel-демон)
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/standalone-no-replicas/case.yml
# sentinel 1 master + 1 replica
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/create-sentinel-1master-1replica/case.yml
# TLS на основном порту
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/create-sentinel-tls/case.yml
# обязательный мониторинг (node-exporter + vector в плане)
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/monitoring-observability/case.yml
# cloud-provision on/off
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/provision-enabled-sentinel/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/create/tests/provision-disabled/case.yml
# операционные сценарии
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/add_user/tests/add-user-plaintext/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/update_users/tests/bulk-replace-removes-user/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/restart/tests/rolling-restart-replicas/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/rotate_tls/tests/rotate-cert-key-ca/case.yml
go run ./cmd/soul-trial run ../examples/service/dragonfly/scenario/destroy/tests/teardown-package/case.yml
```

Кейс [`monitoring-observability`](scenario/create/tests/monitoring-observability/case.yml)
проверяет, что в плане есть безусловные `apply: destiny node-exporter` и `apply: destiny vector`
(step 6/7) с версиями/источниками из essence и что read-model `monitoring`/`logging` попал в
`state_changes`.
