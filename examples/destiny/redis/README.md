# redis — режим-агностичный per-host кирпич Redis

Destiny `redis` — **один** per-host кирпич Redis на все режимы развёртывания
(концепция Ansible-роли Redis, B-гибрид [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
Идемпотентно ставит `redis-server`, рендерит `redis.conf` / `users.acl` / TLS-PEM /
systemd-hardening drop-in, применяет host-tuning extras (THP/logrotate/sysctl) и
поднимает сервис **одного инстанса**. Конкретную форму инстанса destiny **сама не
знает** — её выбирает scenario сервиса флагами `deploy_redis` / `sentinel_enabled` /
`install.method` и готовым merged-конфигом через `apply: input:`. Комбинации флагов
покрывают и cluster, и sentinel (master-replica + sentinel-демон), и **тонкий
sentinel-слой над внешним master** (`deploy_redis: false`) — последний остаётся
**capability кирпича** для переиспользования другими сервисами (например DragonFly),
хотя сервис [`redis`](../../service/redis/README.md) разворачивает только режимы
`redis_type ∈ [sentinel, cluster]` (`standalone`/`sentinel_only` из сервиса убраны
2026-06-25).

Кирпич остаётся **«глупым»**: вся оркестрация — топология, master-election,
rolling-restart, failover, sentinel reconcile, health-gate, merge `redis.conf` —
живёт в scenario сервиса [`examples/service/redis/`](../../service/redis/README.md).
Destiny получает значения уже зарезолвленными (config — готовым merged-map-ом,
секреты — через `vault()` в render-фазе Keeper-а) и только материализует их на хосте.

Версия destiny — git ref ([ADR-007](../../../docs/adr/0007-versioning-git-ref.md)),
поля `version:` верхнего уровня нет. Манифест и список задач — [`destiny.yml`](destiny.yml)
и [`tasks/main.yml`](tasks/main.yml).

## Состав (tasks-split)

[`tasks/main.yml`](tasks/main.yml) — только include-список; логические группы разнесены
по соседним файлам и раскрываются inline в плоский план ДО render ([tasks.md §4](../../../docs/destiny/tasks.md)).
Порядок include = порядок задач (от него зависят финальные индексы плоского плана, на
которые ассертят L0-тесты):

| Файл | Задачи | Используемые модули |
|---|---|---|
| [`install.yml`](tasks/install.yml) | установка бинарей redis — диспетчер по `install.method`: **package** (default-пакет distro) **или** **binary** (default — отдельные бинари из Nexus: distro-юзер/группа → **четыре** `core.url.fetched` качают `redis-server`/`redis-cli`/`redis-benchmark`/`redis-sentinel` в `/usr/local/bin` (URL `<base_url>/<arch>/Debian/<distro_ver>/<version>/…`) → симлинки `redis-check-aof`/`redis-check-rdb` → свой systemd-юнит + СВОЙ рестарт) + каталог unix-сокета (обе ветки) | [`core.pkg`](../../../docs/module/core/pkg/README.md), [`core.url`](../../../docs/module/core/url/README.md), [`core.cmd`](../../../docs/module/core/cmd/README.md), [`core.group`](../../../docs/module/core/group/README.md), [`core.user`](../../../docs/module/core/user/README.md), [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`server.yml`](tasks/server.yml) | data-плоскость `redis-server` (gated `deploy_redis`): TLS-PEM (cert/key/ca) → `users.acl` → `redis.conf` → systemd-hardening drop-in → `core.service running/restarted` | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`sentinel.yml`](tasks/sentinel.yml) | sentinel-демон (gated `sentinel_enabled`): `sentinel-users.acl` (2-й aclfile) → `sentinel.conf` → systemd-юнит → `core.service running/restarted` | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`extras.yml`](tasks/extras.yml) | host-tuning, **безусловно** (рекомендация Redis / hardening, не выбор оператора): отключение THP (oneshot-юнит) / logrotate / sysctl kernel-параметры | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md), [`core.sysctl`](../../../docs/module/core/sysctl/README.md) |
| [`modules.yml`](tasks/modules.yml) | каталог `.so` + fetch Redis-модулей (RediSearch/RedisJSON/RedisTimeSeries/RedisBloom). Весь файл gated (`vars.redis_modules_enabled`): включён при data-плоскости **И** Redis < 8 **И** НЕПУСТОМ `modules_base_url`; иначе group-drop — пустой `modules_base_url` даёт **vanilla** redis (без `loadmodule`/fetch) | [`core.file`](../../../docs/module/core/file/README.md), [`core.url`](../../../docs/module/core/url/README.md) |

Все core-модули — никакого `required_modules:` ([`community.redis`](../../../docs/module/community/redis/README.md)
вызывается из scenario сервиса, не из этой destiny).

Гейты режимов реализованы static-skip-ом ([ADR-009](../../../docs/adr/0009-scenario-dsl.md)
block-static-skip): предикаты `deploy_redis` / `sentinel_enabled` / `install.method`
зависят только от `input.*` → Keeper гасит неактивные ветки placeholder-ом, **не сдвигая
индексы** (skip занимает слот, flat-register-scope инвариантен). register-ссылки в
`onchanges` (ротация cert → рестарт redis и т.п.) **обязаны жить в одном файле** со своим
потребителем: загрузочный per-file линтер отвергает cross-file `onchanges`. Поэтому
binary-ветка несёт собственный рестарт `redis-server` прямо в [`install.yml`](tasks/install.yml).

## Входной контракт (обзор)

destiny видит **только свой** `input:` (изоляция, [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
Полная типизированная схема с описаниями каждого поля — [`destiny.yml → input:`](destiny.yml);
как scenario сервиса формирует и передаёт эти значения оператору — в
[service-README → Входной контракт](../../service/redis/README.md). Ключевые группы:

- **Гейт data-плоскости.** `deploy_redis` (bool, default `true`) — разворачивать ли
  `redis-server`. `false` (режим `sentinel_only`) гасит всю data-плоскость
  ([`server.yml`](tasks/server.yml)), пакет redis при этом ставится всё равно (он же
  несёт sentinel-демон).
- **`install`** — способ доставки бинарей: `{method, base_url, version}`.
  `method=package` — distro-пакет; `method=binary` — **отдельные бинари из Nexus**
  (`redis-server`/`redis-cli`/`redis-benchmark`/`redis-sentinel` качаются per-host из
  `<base_url>/<arch>/Debian/<distro_ver>/<version>/…` по content-идемпотентности
  SHA-256, без integrity-verify — `redis-sentinel` отдельный бинарь, `redis-check-aof`/
  `redis-check-rdb` симлинки на `redis-server`). `version` верхнего уровня (distro-пин) —
  для package-ветки; `install.version` — для binary-ветки. **Способ установки на уровне
  сервиса задаёт `install_method` (default `binary`), а `base_url`/`version` бинаря —
  `essence`** (см. [service-README](../../service/redis/README.md)); destiny собирает из
  них структуру `install`.
- **TLS.** `tls: {enable, only, port, cert_ref, key_ref, ca_ref}` — единый dict
  (host-инвариант). PEM-материал рендерится **через `core.file.present` + `${ vault(ref) }`
  в ячейке `content`** (seal-маскинг, [templating.md §7.4](../../../docs/templating.md)),
  **не** `.tmpl` и **не** уже-резолвленный PEM через `apply.input`: destiny получает
  Vault-**пути** (`cert_ref`/`key_ref`/`ca_ref`) и читает значение сама в render-фазе
  Keeper-а; vault-клиент на Soul не тянется ([ADR-012](../../../docs/adr/0012-keeper-soul-grpc.md)).
- **Секреты.** `password` (`requirepass`) и `users` (ACL-map `имя → {perms, state, password}`,
  redis-server) приходят уже зарезолвленными keeper-side; в `users.acl` пишется sha256-хеш
  пароля, plaintext не материализуется. Маскируются в логах/трейсах/UI. `users`-набор
  scenario сервиса формирует как **системные + operator-extra** (см.
  [«Системные ACL-юзеры и второй aclfile»](#системные-acl-юзеры-и-второй-aclfile)).
- **`sentinel_users`** (опц., additive) — ACL-map того же вида для **sentinel-демона**,
  рендерится в **отдельный** `sentinel-users.acl` (см. ниже). Передаётся только в
  sentinel-режиме; пустой/не передан → пустой aclfile (back-compat).
- **Конфиг.** `config` — **готовый merged-map** директив `redis.conf` (default → preset →
  вычисления → passthrough делает scenario сервиса). Сюда же scenario кладёт
  persistence/`maxmemory`/`maxmemory-policy`/`unixsocket`/TLS- и cluster-директивы.
  destiny повторно **не мержит**.
- **Host-tuning.** `sysctl_settings` (map kernel-параметр → значение, строки) → drop-in
  `/etc/sysctl.d/30-redis.conf` через [`core.sysctl.applied`](../../../docs/module/core/sysctl/README.md).
- **Redis-модули.** `modules_base_url` — единственное поле модулей (поля `modules`/
  `modules_dir` удалены: набор — инвариант кирпича `vars.redis_modules`, `modules_dir`
  выводится из `data_dir`). Директива «модули **всегда все**» = all-or-nothing: непустой
  `modules_base_url` на Redis < 8 → качается **весь** набор (RediSearch/RedisJSON/
  RedisTimeSeries/RedisBloom); **пустой** `modules_base_url` → **vanilla** redis (группа
  `modules.yml` group-drop, ни одного `.so`). `.so` качаются по content-идемпотентности
  (SHA-256), URL арх-специфичен (`soulprint.self.os.arch` per-host).
- **Sentinel.** `sentinel_enabled` (bool) + `sentinel: {master_name, master_ip,
  master_port, quorum, auth_user, auth_pass, config}` — задействованы только в
  sentinel-режимах; `master_ip` обязателен, когда dict передан.

## Системные ACL-юзеры и второй aclfile

Кирпич рендерит **два** ACL-файла, оба `core.file.rendered`, оба `mode 0640` owner/group
`redis`, оба пишут пароль **хешем** (`#<sha256>`, sprig `sha256sum`), не plaintext-ом:

| Файл | Шаблон | Источник `vars.users` | aclfile-директива |
|---|---|---|---|
| `${conf_dir}/users.acl` | [`users.acl.tmpl`](templates/users.acl.tmpl) | `input.users` (системные redis-server + operator-extra) | `aclfile` в `redis.conf` |
| `${conf_dir}/sentinel-users.acl` | [`sentinel-users.acl.tmpl`](templates/sentinel-users.acl.tmpl) | `input.sentinel_users` (системные sentinel-демона) | `aclfile` в `sentinel.conf` |

**Два файла, потому что у redis-server и sentinel-демона разные perms** — sentinel-демону
нужны `sentinel|*`-команды, а не обычные redis-команды, поэтому их служебные наборы не
совпадают (в Ansible-роли — `redis_system_users` vs `redis_sentinel_system_users`). Какой
набор кладётся (`replica`/`monitoring`/`sentinel`/`haproxy`, плюс `default` только в
sentinel-aclfile) и откуда берутся пароли — решает **scenario сервиса**; destiny получает
готовый map и только рендерит. См.
[service-README → «Системные ACL-юзеры»](../../service/redis/README.md#системные-acl-юзеры).

`sentinel-users.acl` рендерится **первой** задачей [`sentinel.yml`](tasks/sentinel.yml)
(ДО `sentinel.conf` — его `aclfile`-директива должна указывать на уже существующий файл),
под `register: redis_sentinel_acl`. ACL sentinel-демона перечитывается только **рестартом**
демона (`onchanges: [redis_sentinel_conf, redis_sentinel_acl, redis_sentinel_unit]`) —
отдельного hot-reload-сценария для sentinel-aclfile нет: набор служебный, меняется лишь на
create/ротации. Пустой map → валидный пустой aclfile (back-compat: destiny старого ref без
поля `sentinel_users` рендерит пустой файл, директива безвредна).

> **`default`-юзер: в `users.acl` его нет, в `sentinel-users.acl` есть.** redis-server
> авторизует `default` через `requirepass` в `redis.conf` (`users.acl` его не дублирует).
> У sentinel-демона `requirepass`-эквивалента для aclfile-доступа нет, поэтому `default`
> объявляется прямо в `sentinel-users.acl`.

## Чем НЕ является

Это **destiny-кирпич** (пакет per-host задач для одного инстанса), а **не сервис**.
Здесь нет `state_schema`, миграций, оператора простого ввода (`memory_mb` / `persistence` /
`shards` / `redis_type`), трансляции в `redis_config`, оркестрации и эксплуатационных операций —
всё это сервис-обёртка [`examples/service/redis/`](../../service/redis/README.md).
Destiny не решает, какой режим разворачивается, и не вызывает плагин
[`community.redis`](../../../docs/module/community/redis/README.md) (живой Redis —
зона scenario сервиса).

## Ссылки

- [`examples/service/redis/`](../../service/redis/README.md) — сервис-обёртка: входной
  контракт оператора, `state_schema`, трансляция, оркестрация, режимы и операционные сценарии.
- [`destiny.yml`](destiny.yml) — манифест и полная схема `input:`.
- [docs/destiny/](../../../docs/destiny/README.md) — формат destiny и механика `tasks/`/`include:`.
- [docs/templating.md](../../../docs/templating.md) — CEL + text/template, `${ vault(ref) }`, seal-маскинг.
- Per-module README используемых модулей — см. колонку «модули» в таблице состава выше.
