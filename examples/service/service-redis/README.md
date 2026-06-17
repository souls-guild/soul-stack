# service-redis — Redis с мониторингом, собранный КОМПОЗИЦИЕЙ destiny

Сервис «redis» демонстрирует главную модель Soul Stack: **один сервис подключает
несколько переиспользуемых standalone-destiny через `apply:destiny`** (ADR-009,
изолированный render каждой destiny), вместо того чтобы инлайнить задачи.

Контраст с [`service-redis-monitored`](../service-redis-monitored/): тот же стек
(redis-server + redis_exporter + node_exporter), но там всё инлайн в одном
`scenario/create/main.yml`, а здесь сервис **ничего не рендерит сам** — только
оркеструет порядок `apply:destiny` и прокидывает каждой destiny её `input`.

## Подключённые destiny

Все три — standalone в [`examples/destiny/`](../../destiny/), объявлены в
[`service.yml → destiny[]`](service.yml) и резолвятся по имени:

| destiny | роль | контракт `input` |
|---|---|---|
| [`redis-single`](../../destiny/destiny-redis-single/) | redis-server + redis.conf с unix-сокетом | `version`, `redis_password`, `redis_socket`, `maxmemory`, `config` |
| [`redis-exporter`](../../destiny/destiny-redis-exporter/) | redis_exporter через unix-сокет | `version`, `sha256`, `arch`, `bin_dir`, `listen`, `redis_socket`, `redis_password` |
| [`node-exporter`](../../destiny/destiny-node-exporter/) | node_exporter (метрики хоста) | `version`, `sha256`, `arch`, `bin_dir`, `listen` |

**Резолв (ADR-007).** `ref` каждой зависимости — из `service.yml::destiny[]`
(здесь `v1.0.0` для всех трёх). git-URL — гибрид
(`docs/keeper/config.md::default_destiny_source`): запись без `git:` →
`default_destiny_source` + `{name}`, в проде — `git@.../{name}.git`. В local-dev
`dev/keeper.dev.yml` это `file:///tmp/keeper-dev/destiny/{name}`, а сами
destiny-репо материализует `make dev-provision` из этого каталога `examples/`
как git-репо на ref `v1.0.0` (см.
[docs/dev/local-setup.md → Артефакты service/destiny](../../../docs/dev/local-setup.md#артефакты-servicedestiny-для-резолва)).
Запись с `git:` → прямой URL. `apply:destiny` ссылается только на имена,
объявленные в `destiny[]`.

**Точка интеграции** redis ↔ redis_exporter — путь unix-сокета
(`essence.redis_socket`): один и тот же путь уходит в `redis-single` (директива
`unixsocket` в redis.conf, права 770) и в `redis-exporter`
(`--redis.addr=unix://...`). redis_exporter работает под выделенным
least-privilege пользователем в группе `redis` (group-доступ к сокету 770).

## Сценарии

- **`create`** — разворачивает стек композицией трёх destiny в **строгом
  порядке** `redis-single → redis-exporter → node-exporter` (redis создаёт
  группу/сокет до exporter-а). Тройка `apply:destiny` вынесена в
  [`install.yml`](scenario/create/install.yml) и подключена через `include:`.
- **`add_acl_user`** — приводит ACL-пользователей Redis к **переданному
  актуальному списку** (`input.users` — array). Демонстрирует `loop:
  items: ${ input.users }` (slice E1): одна задача → N `RenderedTask`
  (`redis-cli ACL SETUSER` на каждого), `no_log: true` (ACL содержит пароль).
- **`update_config`** — re-apply `redis-single` с новым `maxmemory`/`config`:
  redis.conf перерисовывается, redis рестартится только при изменении
  (`onchanges` внутри destiny).
- **`add_replicas`** — масштабирует стек **репликами**. Адрес актуального
  primary опознаётся рантайм-probe-ом существующих хостов
  (`core.exec.run`, `changed_when: false`, `where: !(sid in input.replicas)`);
  на новые реплики ставится `redis-single` (`apply:destiny`), затем они
  направляются `replicaof` на этот primary через
  [`redis-replication-config`](../../destiny/destiny-redis-replication-config/),
  получая топологию прогона через `apply: input: hosts: ${ soulprint.hosts }`
  (E3a scenario-only аксессор). master_addr — свёртка агрегатного
  `register.master_addr` (карта sid→payload) к одному значению. Раскат
  `serial: 1` (rolling по одной реплике), завершается health-gate-ом
  (probe + `retry`/`until`). Эталон топологии — [`service-redis-cluster`](../service-redis-cluster/).

## Безопасность

- Пароль Redis — из **Vault**, НЕ во входном контракте сценария. Сценарии читают
  его keeper-side CEL-функцией `vault('secret/redis/${ incarnation.name }#password')`
  в render-фазе (templating.md §2.3/§4): путь строится из доверенного контекста
  (incarnation), в destiny через `apply.input` уходит уже зарезолвленное значение.
  Так пароль доезжает на хост значением, а не ссылкой — Soul vault-клиент не тянет
  (ADR-012). В git нет ни значения, ни operator-указателя на секрет.
- unix-сокет 770, exporter least-privilege (см. контракты destiny).

## Прогон L0

```sh
cd keeper
go run ./cmd/soul-trial run ../examples/service/service-redis/scenario/add_acl_user/tests/three-users/case.yml     # PASS
go run ./cmd/soul-trial run ../examples/service/service-redis/scenario/add_replicas/tests/one-replica/case.yml     # PASS
go run ./cmd/soul-trial run ../examples/service/service-redis/scenario/create/tests/full-stack/case.yml            # PASS
go run ./cmd/soul-trial run ../examples/service/service-redis/scenario/update_config/tests/bump-maxmemory/case.yml # PASS
```

> **Примечание (cross-service L0-резолв `apply:destiny`).** Кейсы `create`,
> `update_config` и `add_replicas` используют `apply:destiny` к standalone-destiny
> в `examples/destiny/`. L0-резолвер
> (`keeper/internal/trial/destiny.go::fixtureDestinyResolver`) достаёт их
> герметично через `case.yml::fixtures.default_destiny_source` — file://-шаблон
> (здесь `file://../../destiny/destiny-{name}`, путь относительно service-root),
> зеркалящий прод `keeper.yml::default_destiny_source`. `add_acl_user` —
> инлайн `loop:` без `apply:destiny`.
>
> В живом Keeper service-redis + destiny резолвятся как git-репо по ref
> (ADR-007/009) — в local-dev они материализуются `make dev-provision` под
> file://-URL-ами из `dev/keeper.dev.yml` (см.
> [docs/dev/local-setup.md → Артефакты service/destiny](../../../docs/dev/local-setup.md#артефакты-servicedestiny-для-резолва)).
