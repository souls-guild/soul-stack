# dragonfly — режим-агностичный per-host кирпич DragonFly

Destiny `dragonfly` — per-host кирпич [DragonflyDB](https://www.dragonflydb.io/)
(концепция Ansible-роли, B-гибрид [ADR-009](../../../docs/adr/0009-scenario-dsl.md)).
Идемпотентно ставит DragonFly (distro-deb **или** upstream-tarball), рендерит
**flagfile** `dragonfly.conf` / `users.acl` / TLS-PEM, поднимает sentinel-демон
(`redis-sentinel` из distro-пакета), применяет host-tuning extras (THP/logrotate/sysctl)
и поднимает сервис **одного инстанса**. Конкретную форму destiny **сама не знает** — её
выбирает scenario сервиса флагами `deploy_dragonfly` / `sentinel_enabled` / `install.method`
и готовым merged-конфигом через `apply: input:`.

DragonFly **redis-совместим** на проводе, поэтому весь живой рантайм (PING / REPLICAOF /
SENTINEL MONITOR) идёт через тот же плагин `community.redis` **без правок**, а
sentinel-демон — штатный `redis-sentinel` (DragonFly его не несёт, ставится отдельным
distro-пакетом).

## DragonFly vs `redis` (зачем отдельный кирпич)

Структура копирует destiny [`redis`](../../destiny/redis/README.md) с подменой трёх вещей:

- **config = flagfile** (`--flag=value`, absl-флаги), **не** `redis.conf`. Имена
  флагов — через подчёркивание (`tls_cert_file`, `snapshot_cron`), значения без кавычек
  (DragonFly явно отвергает их). Bool-флаги — `--tls=true`. Шаблон
  [`dragonfly.flags.tmpl`](templates/dragonfly.flags.tmpl).
- **unit = `Type=simple`** (DragonFly — foreground без `sd_notify`, в отличие от
  `Type=notify` redis-server), `ExecStart … --flagfile`, hardening **внутри** юнита
  (binary-ветка), не drop-in. Шаблон [`dragonfly.service.tmpl`](templates/dragonfly.service.tmpl).
- **install = один бинарь `dragonfly`** (tarball `dragonfly-<arch>.tar.gz`, arch
  `x86_64`/`aarch64`) + **отдельно** distro-пакеты `redis-server` (sentinel-демон) и
  `redis-tools` (`redis-cli` для health-gate / REPLICAOF) — DragonFly их не поставляет.

**Вне скоупа** (PILOT, sentinel-only): cluster (DragonFly cluster — emulated), redis-модули
`.so`, version-guard Redis 8+.

Логирование — **glog** (`--log_dir`, DragonFly именует файлы сам), не redis-style
`logfile <path>`. Snapshot — `--snapshot_cron` / `--dbfilename` (redis `save`/`appendonly`
к DragonFly неприменимы).

Версия destiny — git ref ([ADR-007](../../../docs/adr/0007-versioning-git-ref.md)). Манифест
и список задач — [`destiny.yml`](destiny.yml) и [`tasks/main.yml`](tasks/main.yml).

## Состав (tasks-split)

[`tasks/main.yml`](tasks/main.yml) — только include-список; группы раскрываются inline в
плоский план ДО render. Порядок include = порядок задач.

| Файл | Задачи | Используемые модули |
|---|---|---|
| [`install.yml`](tasks/install.yml) | sentinel-инструментарий (`redis-server`/`redis-tools`, **безусловно**) + DragonFly — диспетчер по `install.method`: **package** (distro-deb `dragonfly`) **или** **binary** (upstream-tarball: fetch → extract → distro-юзер/группа → `core.file.present` (`src:`) разложение `dragonfly` в `/usr/local/bin` → свой systemd-юнит + СВОЙ рестарт) | `core.pkg`, `core.url`, `core.archive`, `core.group`, `core.user`, `core.file`, `core.service` |
| [`server.yml`](tasks/server.yml) | data-плоскость `dragonfly` (gated `deploy_dragonfly`): TLS-PEM (cert/key/ca) → `users.acl` → `dragonfly.conf` (flagfile) → `core.service running`. Без hardening drop-in (hardening в самом юните) | `core.file`, `core.service` |
| [`sentinel.yml`](tasks/sentinel.yml) | sentinel-демон `redis-sentinel` (gated `sentinel_enabled`): `sentinel-users.acl` (2-й aclfile) → `sentinel.conf` → systemd-юнит → `core.service running/restarted`. Под distro-юзером `redis` | `core.file`, `core.service` |
| [`extras.yml`](tasks/extras.yml) | host-tuning, **безусловно**: отключение THP (oneshot-юнит) / logrotate / sysctl kernel-параметры | `core.file`, `core.service`, `core.sysctl` |

## Переиспользование из `redis`

1:1 копии (контракт sentinel-демона и ACL-формат у DragonFly идентичны redis):
`users.acl.tmpl`, `sentinel-users.acl.tmpl`, `sentinel.conf.tmpl`,
`redis-sentinel.service.tmpl`, `disable-thp.service.tmpl`. `logrotate.tmpl` адаптирован
под два лог-каталога (DragonFly glog + sentinel под `redis`).
