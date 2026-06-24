# redis — режим-агностичный per-host кирпич Redis

Destiny `redis` — **один** per-host кирпич Redis на все режимы развёртывания
(концепция Ansible-роли Redis, B-гибрид [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
Идемпотентно ставит `redis-server`, рендерит `redis.conf` / `users.acl` / TLS-PEM /
systemd-hardening drop-in, применяет host-tuning extras (THP/logrotate/sysctl) и
поднимает сервис **одного инстанса**. Режим (`standalone` / `cluster` / `sentinel` /
`sentinel_only`) destiny **сама не знает** — его выбирает scenario сервиса, передавая
флаги и готовый merged-конфиг через `apply: input:`.

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
| [`install.yml`](tasks/install.yml) | установка бинарей redis — диспетчер по `install.method`: **package** (default, distro-пакет) **или** **binary** (opt-in upstream-tarball: fetch → extract → distro-юзер/группа → `install -m0755` → свой systemd-юнит + СВОЙ рестарт) + каталог unix-сокета (обе ветки) | [`core.pkg`](../../../docs/module/core/pkg/README.md), [`core.url`](../../../docs/module/core/url/README.md), [`core.archive`](../../../docs/module/core/archive/README.md), [`core.group`](../../../docs/module/core/group/README.md), [`core.user`](../../../docs/module/core/user/README.md), [`core.cmd`](../../../docs/module/core/cmd/README.md), [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`server.yml`](tasks/server.yml) | data-плоскость `redis-server` (gated `deploy_redis`): TLS-PEM (cert/key/ca) → `users.acl` → `redis.conf` → systemd-hardening drop-in → `core.service running/restarted` | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`sentinel.yml`](tasks/sentinel.yml) | sentinel-демон (gated `sentinel_enabled`): `sentinel.conf` + systemd-юнит + `core.service running/restarted` | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md) |
| [`extras.yml`](tasks/extras.yml) | host-tuning, **безусловно** (рекомендация Redis / hardening, не выбор оператора): отключение THP (oneshot-юнит) / logrotate / sysctl kernel-параметры | [`core.file`](../../../docs/module/core/file/README.md), [`core.service`](../../../docs/module/core/service/README.md), [`core.sysctl`](../../../docs/module/core/sysctl/README.md) |
| [`modules.yml`](tasks/modules.yml) | каталог `.so` + loop-fetch Redis-модулей (RediSearch/RedisJSON/RedisTimeSeries/RedisBloom) на Redis < 8 | [`core.file`](../../../docs/module/core/file/README.md), [`core.url`](../../../docs/module/core/url/README.md) |

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
- **`install`** — способ доставки бинарей: `{method, base_url, version, sha256}`.
  `method=package` (default) — distro-пакет; `binary` — upstream-tarball. `sha256`
  опционален (задан → fail-closed integrity-verify; нет → загрузка по
  content-идемпотентности). `version` верхнего уровня (distro-epoch-пин,
  напр. `5:7.0.15-1~deb12u7`) — для package-ветки; `install.version` (upstream-semver) —
  для binary-ветки.
- **TLS.** `tls: {enable, only, port, cert_ref, key_ref, ca_ref}` — единый dict
  (host-инвариант). PEM-материал рендерится **через `core.file.present` + `${ vault(ref) }`
  в ячейке `content`** (seal-маскинг, [templating.md §7.4](../../../docs/templating.md)),
  **не** `.tmpl` и **не** уже-резолвленный PEM через `apply.input`: destiny получает
  Vault-**пути** (`cert_ref`/`key_ref`/`ca_ref`) и читает значение сама в render-фазе
  Keeper-а; vault-клиент на Soul не тянется ([ADR-012](../../../docs/adr/0012-keeper-soul-grpc.md)).
- **Секреты.** `password` (`requirepass`) и `users` (ACL-map `имя → {perms, state, password}`)
  приходят уже зарезолвленными keeper-side; в `users.acl` пишется sha256-хеш пароля,
  plaintext не материализуется. Маскируются в логах/трейсах/UI.
- **Конфиг.** `config` — **готовый merged-map** директив `redis.conf` (default → preset →
  вычисления → passthrough делает scenario сервиса). Сюда же scenario кладёт
  persistence/`maxmemory`/`maxmemory-policy`/`unixsocket`/TLS- и cluster-директивы.
  destiny повторно **не мержит**.
- **Host-tuning.** `sysctl_settings` (map kernel-параметр → значение, строки) → drop-in
  `/etc/sysctl.d/30-redis.conf` через [`core.sysctl.applied`](../../../docs/module/core/sysctl/README.md).
- **Redis-модули.** `modules` (алиасы `search`/`json`/`timeseries`/`bloom`),
  `modules_dir`, `modules_base_url`, `modules_sha256` (опц., per-алиас) — для Redis < 8;
  URL `.so` арх-специфичен (строится из `soulprint.self.os.arch` per-host).
- **Sentinel.** `sentinel_enabled` (bool) + `sentinel: {master_name, master_ip,
  master_port, quorum, auth_user, auth_pass, config}` — задействованы только в
  sentinel-режимах; `master_ip` обязателен, когда dict передан.

## Чем НЕ является

Это **destiny-кирпич** (пакет per-host задач для одного инстанса), а **не сервис**.
Здесь нет `state_schema`, миграций, оператора простого ввода (`memory_mb` / `persistence` /
`shards` / `redis_type`), трансляции в `redis_config`, оркестрации и day-2-операций —
всё это сервис-обёртка [`examples/service/redis/`](../../service/redis/README.md).
Destiny не решает, какой режим разворачивается, и не вызывает плагин
[`community.redis`](../../../docs/module/community/redis/README.md) (живой Redis —
зона scenario сервиса).

## Ссылки

- [`examples/service/redis/`](../../service/redis/README.md) — сервис-обёртка: входной
  контракт оператора, `state_schema`, трансляция, оркестрация, режимы и day-2.
- [`destiny.yml`](destiny.yml) — манифест и полная схема `input:`.
- [docs/destiny/](../../../docs/destiny/README.md) — формат destiny и механика `tasks/`/`include:`.
- [docs/templating.md](../../../docs/templating.md) — CEL + text/template, `${ vault(ref) }`, seal-маскинг.
- Per-module README используемых модулей — см. колонку «модули» в таблице состава выше.
