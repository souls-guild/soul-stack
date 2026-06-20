# Локальный dev-стек

Локальная инфраструктура для разработки и интерактивной отладки Keeper-а.
Поднимается через docker-compose, persistent volume. Для автоматизированных
integration-тестов используется отдельный механизм — testcontainers-go
(см. [Integration tests](#integration-tests) ниже), он поднимает свой
эфемерный контейнер per-package, не пересекаясь с `dev-up`.

## Что поднимается

| Компонент | Назначение | ADR |
|---|---|---|
| **postgres:16-alpine** | Холодное хранилище Keeper-а (`audit_log`, далее `souls`/`operators`/`incarnation`). | [ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres) |
| **hashicorp/vault:1.18** | Vault в dev-режиме для чтения JWT signing key (`secret/keeper/jwt-signing-key`, ADR-014) и других KV. Root token = `root`. | [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon), [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) |
| **redis:7-alpine** | Reaper-lease, SoulLease, Outbound pub/sub между Keeper-инстансами. Без пароля в dev. | [ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis) |
| **otel/opentelemetry-collector-contrib** | Приём OTLP gRPC (`:4317`) трейсов от keeper/soul → экспорт в Jaeger + debug-лог. Конфиг pipeline-а — [`dev/otel-collector.yaml`](../../dev/otel-collector.yaml). | [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) |
| **jaegertracing/all-in-one** | Хранилище + UI трейсов (`:16686`, in-memory storage). Принимает от коллектора OTLP внутри docker-сети. | [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) |

Backlog для следующих slice-ов: Vault PKI (Keeper-side issuance mTLS,
отдельный mount от `secret/`), `audit.otel_export`
([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)).

## Команды

| Команда | Что делает |
|---|---|
| `make dev-up` | `docker compose up -d` в `dev/`. Данные persist в named volume `postgres_data`. |
| `make dev-stop` | Останавливает локальные `keeper run`/`soul run`-демоны dev-воркфлоу (foreground-процессы из [Smoke recipe](#smoke-recipe-e2e)). Pidfile не пишется — матчит по `pkill -f` со специфичным паттерном dev-конфига (`keeper.dev.yml`/`soul.dev.yml`), чужие keeper/soul не задевает; не падает, если процессов нет. |
| `make dev-down` | `make dev-stop` (гасит локальные демоны) → `docker compose down` без `-v` — контейнер останавливается, данные сохраняются. |
| `make dev-reset` | `docker compose down -v && docker compose up -d` — полный сброс с потерей данных. |
| `make dev-provision` | Idempotent bootstrap: Vault KV (`secret/keeper/postgres`, `secret/keeper/jwt-signing-key`) + Vault PKI (`pki/` engine, root cert, role `soul-seed`) + TLS-материал из Vault PKI в `/tmp/keeper-dev/tls/` + каталоги `plugins/`, `plugin-sockets/` + **git-репо service/destiny-артефактов** из `examples/` под file://-URL-ами из `keeper.dev.yml` (см. [Артефакты service/destiny](#артефакты-servicedestiny-для-резолва)). Скрипт — [`dev/provision.sh`](../../dev/provision.sh), безопасен к повторному запуску. |
| `make dev-smoke` | Полный цикл: `dev-up` → `dev-provision` → собрать `keeper` → `keeper init --archon=archon-alice`. JWT-файл оператора → `/tmp/keeper-dev/archon-alice.jwt`. Повторный прогон требует `make dev-reset && make dev-smoke` (operators registry уже не пуст). |
| `make dev-keeper` | Рестарт keeper в фоне с ПОЛНЫМ dev-env (см. [Фоновые dev-демоны](#фоновые-dev-демоны-обёртка-над-keeper-runsoul-run)): гасит старый процесс по паттерну `keeper.dev.yml`, чистит leader-leases в Redis (`conductor:leader`/`reaper:leader`), создаёт cache-каталоги, выставляет `VAULT_ADDR`/`VAULT_TOKEN`/`KEEPER_SERVICE_CACHE_DIR`/`KEEPER_DESTINY_CACHE_DIR`/`SOUL_STACK_ALLOW_FILE_REPOS=1`, поднимает `nohup keeper run` и ждёт healthz 200 на `:8080`. Нет бинаря — собирает; нет TLS — подсказывает `dev-provision`. Лог → `/tmp/keeper-dev/keeper.log`. Скрипт — [`dev/keeper-run.sh`](../../dev/keeper-run.sh). |
| `make dev-jwt [AID=… ROLES=… TTL=…]` | Печатает в stdout HS256-JWT Архонта для ad-hoc API-вызовов **без** `keeper init`. Ключ берётся из того же Vault KV, что и у keeper (`secret/keeper/jwt-signing-key`, поле `signing_key`, base64-decode), `iss=keeper-dev-01`. Дефолты: `AID=archon-alice`, `ROLES='["cluster-admin"]'`, `TTL=43200` (12h). Только токен в stdout (служебное — в stderr) → `TOKEN=$(make dev-jwt)`. Требует python3 + поднятый Vault. Скрипт — [`dev/mint-jwt.sh`](../../dev/mint-jwt.sh). |
| `make dev-souls` | Переподнимает локальный флот souls по реестру БД (`SELECT sid FROM souls`): на каждый sid пишет per-sid `soul.yml` (если нет), онбордит (`issue-token?force=true` → `soul init`) ТОЛЬКО при отсутствии валидного seed (три файла `cert/key/ca.pem` по `seed/current`), (пере)запускает `soul run`. Covens в БД сохраняются — заново НЕ регистрирует. В конце печатает `SELECT status, count(*) FROM souls`. Чинит «все souls disconnected». Скрипт — [`dev/souls-up.sh`](../../dev/souls-up.sh). |
| `make dev-web [WEB_DIR=…]` | Vite dev-сервер companion-репо (`WEB_DIR`, default `../soul-stack-web`) с обязательным `--host` — иначе vite биндится только на IPv6 `[::1]` и `http://127.0.0.1:5173` отказывает. Гасит старый vite этого репо, поднимает `nohup npm run dev -- --host`, ждёт 200 на `:5173`. Лог → `/tmp/keeper-dev/web-dev.log`. Скрипт — [`dev/web-run.sh`](../../dev/web-run.sh). |
| `make dev-stand` | Полный подъём стенда одной командой: `dev-provision` → `dev-keeper` → `dev-souls` → `dev-web` + сводка адресов и напоминание про `make dev-jwt`. Применять после рестарта / смены суток (см. [Быстрое восстановление стенда](#быстрое-восстановление-стенда-после-tmp-чистки)). |

> **Два UI: dev vite-сервер vs embed на keeper — не путать.** UI с [ADR-055](../adr/0055-embed-ui-bundle.md#adr-055-embed-ui-bundle--опциональный-single-binary-keeper-с-ui-на-ui) встроен в `keeper`-бинарь (`go:embed`) и в проде/бете отдаётся самим keeper-ом на **`http://<keeper>:8080/ui`** (тоггл [`web_ui_enabled`](../keeper/config.md#web_ui_enabled-top-level), default-ON). Для **разработки фронта** `make dev-web` поднимает живой vite dev-сервер с HMR на **`http://127.0.0.1:5173/ui/`** (отдельный процесс, hot-reload исходников из companion `soul-stack-web`) — это «настоящий» UI при работе над фронтом. Embed на `:8080/ui` показывает **завендоренный снапшот** (`keeper/internal/webui/assets/`, обновляется `make sync-webui`) — он может отставать от исходников companion-а, пока снапшот не пере-синкнут. То есть: фронт правишь и смотришь на `:5173/ui/`; «как увидит пользователь беты» проверяешь на `:8080/ui` после `make sync-webui`. Подробнее про вендоринг — [docs/web/README.md](../web/README.md).

## Connection details

Порты выбраны так, чтобы не конфликтовать с типичными пользовательскими
docker-стеками (`agent-platform-postgres:5432`, `agent-platform-valkey:6380`,
`dba-salt-redis:6379`). Если занят и `5434`/`6381`/`8200` — см.
[Troubleshooting](#troubleshooting).

### Postgres

| Параметр | Значение |
|---|---|
| Host / port | `127.0.0.1:5434` |
| Database | `keeper` |
| User / password | `keeper` / `keeper` |
| DSN | `postgres://keeper:keeper@127.0.0.1:5434/keeper?sslmode=disable` |

### Vault

| Параметр | Значение |
|---|---|
| Host / port | `127.0.0.1:8200` |
| UI | http://127.0.0.1:8200/ui |
| Root token | `root` |
| KV mount | `secret` (v2, активирован автоматически в dev-режиме) |
| Vault address для CLI | `export VAULT_ADDR=http://127.0.0.1:8200` |
| Vault token для CLI | `export VAULT_TOKEN=root` |

### Redis

| Параметр | Значение |
|---|---|
| Host / port | `127.0.0.1:6381` |
| Password | пусто (dev) |
| URL для CLI | `redis-cli -h 127.0.0.1 -p 6381 ping` |

### OTel-стек (трейсы)

| Параметр | Значение |
|---|---|
| OTLP gRPC (приём от keeper/soul) | `127.0.0.1:4317` (insecure, без TLS) |
| Jaeger UI | http://127.0.0.1:16686 |
| Конфиг collector-а | [`dev/otel-collector.yaml`](../../dev/otel-collector.yaml) |

dev-конфиги keeper и soul уже указывают на коллектор: `otel.enabled: true`,
`endpoint: 127.0.0.1:4317` ([`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) /
[`dev/soul.dev.yml`](../../dev/soul.dev.yml)).

**Посмотреть трейсы:**

1. Подними стек (`make dev-up`) — коллектор и Jaeger стартуют вместе с PG/Vault/Redis.
2. Прогони keeper/soul через dev-конфиги (см. [Smoke recipe](#smoke-recipe-e2e)).
3. Открой Jaeger UI http://127.0.0.1:16686, выбери service `keeper` или `soul`,
   нажми **Find Traces**. Сквозная трасса оператор → Keeper → Soul видна как один
   trace (trace-context едет в `ApplyRequest.trace_context`, [observability.md §4](../observability.md)).
4. Без UI — `docker compose -f dev/docker-compose.yml logs -f otel-collector`
   печатает принятые спаны (debug-exporter).

> **Прод — не этот стек.** all-in-one Jaeger хранит трейсы in-memory (теряются
> при restart) и принимает OTLP без TLS — только для локалки. Прод-`keeper.yml`
> ([`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml)) оставляет
> `otel:`-блок конфигурируемым (endpoint реального коллектора + TLS),
> dev-эндпоинт туда не хардкодится.

Bootstrap-провижининг секретов для local-dev (ADR-014/M0.5b/M0.5d) — после
`make dev-up` одной командой:

```sh
make dev-provision
```

Скрипт [`dev/provision.sh`](../../dev/provision.sh) идемпотентен и сам
делает следующие шаги (то же, что раньше выполнялось вручную):

```sh
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=root

# JWT signing-key (auth.jwt.signing_key_ref → secret/keeper/jwt-signing-key, поле `signing_key`)
vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"

# Postgres DSN (postgres.dsn_ref → secret/keeper/postgres, поле `dsn`)
vault kv put secret/keeper/postgres \
  dsn="postgres://keeper:keeper@127.0.0.1:5434/keeper?sslmode=disable"
```

При отсутствии `vault`-CLI на хосте скрипт прозрачно проксирует команды
через `docker exec soul-stack-vault vault ...`.

После `make dev-down`/`dev-reset` запустить `make dev-provision` снова —
dev-mode Vault хранит секреты только в RAM.

Vault-ссылки `vault:secret/...` в `keeper.yml` резолвятся на старте бинаря
keeper (M0.5b — `auth.jwt.signing_key_ref`, M0.5d — `postgres.dsn_ref`).
Convention имени поля внутри KV: короткое (`signing_key` / `dsn`), документировано
в [docs/keeper/config.md](../keeper/config.md). Остальные `_ref`-поля
(`redis.password_ref`, AppRole credentials) — отложены на следующие slice-ы.

## Готовый dev-конфиг

Для smoke-прогона Keeper-а есть закоммиченный конфиг
[`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) — пристрелян под compose-стек
выше (PG 5434, Redis 6381, Vault 8200, root-token, TLS из
`/tmp/keeper-dev/tls/`, OTel-трейсы в collector на `127.0.0.1:4317`,
plugin-кэш в `/tmp/keeper-dev/`).
`services[]` зарегистрированы два сервиса — `hello-world` и `redis` (оба по
file://-репо из `/tmp/keeper-dev/repos/`, ref `main`);
`default_destiny_source` — `file:///tmp/keeper-dev/destiny/{name}`. Сами репо
создаёт `make dev-provision` (см. [Артефакты
service/destiny](#артефакты-servicedestiny-для-резолва)). Запуск:

```sh
./keeper/bin/keeper init --archon=archon-alice \
  --config=dev/keeper.dev.yml \
  --credential-out=/tmp/keeper-dev/archon-alice.jwt

# CACHE_DIR-ы по умолчанию указывают на /var/lib/soul-stack-keeper/ (не
# writable в локалке); file://-репо требуют явного флага. См. раздел ниже.
export KEEPER_SERVICE_CACHE_DIR=/tmp/keeper-dev/services
export KEEPER_DESTINY_CACHE_DIR=/tmp/keeper-dev/destiny-cache
export SOUL_STACK_ALLOW_FILE_REPOS=1
./keeper/bin/keeper run --config=dev/keeper.dev.yml
```

Отличия от `examples/keeper/keeper.yml`: dev-shortcut `vault.token: "root"`
вместо AppRole; HTTP-Vault без TLS; TLS-leaf из Vault PKI в
`/tmp/keeper-dev/tls/`; `otel.endpoint` указывает на dev-collector
(`127.0.0.1:4317`, insecure); `services[]` указывают на локальные file://-репо.
Для прод-конфигурации использовать example, не dev-копию.

Полный разбор прод-отличий (AppRole вместо root-токена, persistent Vault +
auto-unseal, least-privilege policy, ротация JWT signing-key) — в
[prod-setup.md](../keeper/prod-setup.md).

## Артефакты service/destiny для резолва

Прод-резолв Keeper-а (`artifact.ServiceLoader` / `DestinyLoader`,
ADR-007/ADR-009) тянет service- и destiny-артефакты как **git-репозитории по
ref**, а не из локальной директории. `make dev-provision` материализует эти
репо из `examples/` под file://-URL-ами, на которые указывает
`dev/keeper.dev.yml`:

| Артефакт | git-URL (из `keeper.dev.yml`) | ref | источник в `examples/` |
|---|---|---|---|
| service `hello-world` | `file:///tmp/keeper-dev/repos/hello-world` | `main` | `examples/service/hello-world` |
| service `redis` | `file:///tmp/keeper-dev/repos/redis` | `main` | `examples/service/redis` |
| destiny `redis-single` | `file:///tmp/keeper-dev/destiny/redis-single` | `v1.0.0` | `examples/destiny/redis-single` |
| destiny `redis-exporter` | `file:///tmp/keeper-dev/destiny/redis-exporter` | `v1.0.0` | `examples/destiny/redis-exporter` |
| destiny `node-exporter` | `file:///tmp/keeper-dev/destiny/node-exporter` | `v1.0.0` | `examples/destiny/node-exporter` |

destiny-URL — это `default_destiny_source` (`file:///tmp/keeper-dev/destiny/{name}`)
с подстановкой `{name}` из `redis/service.yml::destiny[]`; ref `v1.0.0`
там же объявлен. Каталог destiny-репо называется по `{name}` (`redis-single`),
**не** по examples-имени (`redis-single`).

Provision создаёт репо детерминированно (фиксированные author/date → стабильный
commit-SHA при неизменном содержимом): повторный `make dev-provision` не плодит
сироты в snapshot-кеше Keeper-а.

Чтобы Keeper смог их склонировать в локалке, нужны два env при `keeper run`:

| Env | Зачем | Значение для dev |
|---|---|---|
| `SOUL_STACK_ALLOW_FILE_REPOS` | `file://`-репо запрещены в проде (`artifact/scheme.go`) — флаг включает их для dev/test. | `1` |
| `KEEPER_SERVICE_CACHE_DIR` | snapshot-кеш service-репо. Default `/var/lib/soul-stack-keeper/services` не writable в локалке. | `/tmp/keeper-dev/services` |
| `KEEPER_DESTINY_CACHE_DIR` | snapshot-кеш destiny-репо. Default `/var/lib/soul-stack-keeper/destiny` не writable. | `/tmp/keeper-dev/destiny-cache` |

> `KEEPER_DESTINY_CACHE_DIR` НЕ совпадает с каталогом destiny-**репо**
> (`/tmp/keeper-dev/destiny/`): первый — кеш снапшотов Keeper-а, второй —
> исходные git-репо, куда указывает `default_destiny_source`. Разные пути
> намеренно, чтобы кеш не затирал исходники.

## Фоновые dev-демоны (обёртка над `keeper run`/`soul run`)

Ручные `keeper run` / `soul run` / `npm run dev` из smoke-рецепта ниже остаются
как **low-level альтернатива** (полезны, когда нужен foreground-лог в терминале
или нестандартный конфиг). Для повседневной отладки те же запуски обёрнуты в
dev-таргеты — это **обёртка над теми же бинарями**, добавляющая три вещи поверх
ручного шага:

1. **фоновый запуск** (`nohup … &`, лог в `/tmp/keeper-dev/`), не занимающий терминал;
2. **healthz-wait** — таргет не возвращается, пока компонент не ответит 200
   (keeper `:8080/healthz`, web `:5173`), иначе печатает хвост лога и фейлится;
3. **правильный env** — выверенный набор переменных, который при ручном запуске
   постоянно теряется (особенно `SOUL_STACK_ALLOW_FILE_REPOS=1` + writable
   cache-dirs для file://-резолва, см. [Артефакты
   service/destiny](#артефакты-servicedestiny-для-резолва)).

| Таргет | Обёртка над | Что добавляет |
|---|---|---|
| `make dev-keeper` | `keeper run --config=dev/keeper.dev.yml` | kill старого по паттерну `keeper.dev.yml` → DEL leader-leases (`conductor:leader`/`reaper:leader`) → wait `:9090` свободен → full dev-env → `nohup` → wait healthz `:8080`. Бинаря нет — собирает; TLS нет — подсказывает `dev-provision`. |
| `make dev-souls` | `soul init` + `soul run` на каждый sid | онбординг только при невалидном seed, covens из БД не трогает, сводка `status, count(*)` в конце. |
| `make dev-web` | `npm run dev -- --host` | обязательный `--host` (IPv4-loopback) + wait `:5173`. |
| `make dev-stand` | всё сразу | `dev-provision` → `dev-keeper` → `dev-souls` → `dev-web`. |

`make dev-stop` гасит фоновые keeper/soul-демоны, поднятые как этими таргетами,
так и вручную (матч по паттерну dev-конфига).

**Пример `dev-jwt`** — токен для ad-hoc вызовов Operator API без `keeper init`:

```sh
# admin-токен по дефолту (archon-alice / cluster-admin / 12h):
TOKEN=$(make dev-jwt)
curl -H "Authorization: Bearer ${TOKEN}" 127.0.0.1:8080/v1/souls

# произвольный субъект и роли (например, для RBAC-демо keyset):
make dev-jwt AID=archon-keyset ROLES='["keyset-demo"]'
```

## Быстрое восстановление стенда после /tmp-чистки

На macOS смена суток (а также reboot) **чистит `/tmp`** — исчезает весь
dev-материал под `/tmp/keeper-dev/`: TLS (`tls/`), плагины (`plugins/`) и
per-soul seed (`<sid>/seed/*.pem`). Стенд после этого выглядит «сломанным»,
хотя ни код, ни БД не пострадали.

**Типичные симптомы:**

| Симптом | Что потерялось |
|---|---|
| keeper падает на старте `load bootstrap TLS … no such file or directory` | `/tmp/keeper-dev/tls/` (TLS-leaf из Vault PKI) |
| souls в `disconnected`, `soul run` падает `SoulSeed not found` | `/tmp/keeper-dev/<sid>/seed/` (mTLS-пары) |
| сценарии резолвятся в 502 `file:// запрещён` | `SOUL_STACK_ALLOW_FILE_REPOS=1` в env keeper-процесса (env, не файл — теряется при ручном перезапуске) |

**Рецепт восстановления** — переразложить материал и переподнять демоны:

```sh
make dev-provision   # tls/ + plugins/ + git-репо артефактов из Vault PKI/examples
make dev-keeper      # keeper с полным dev-env (включая SOUL_STACK_ALLOW_FILE_REPOS=1)
make dev-souls       # переонбордит souls с битым/исчезнувшим seed и переподнимет run
```

Либо одной командой — `make dev-stand` (делает то же `dev-provision → dev-keeper
→ dev-souls` + поднимает web). БД (`souls`/`operators`/`incarnation`, covens)
переживает /tmp-чистку — `keeper init` повторять НЕ нужно (operators registry не
пуст); `dev-souls` восстанавливает только seed и run, реестр sid и их covens
берёт из БД.

> Если потеряна сама **БД** (после `make dev-reset` или `docker compose down -v`)
> — это другой случай: нужен полный `make dev-smoke` (с `keeper init`), а не
> восстановление /tmp. /tmp-чистка БД не трогает.

## Smoke recipe (E2E)

Воспроизводимая последовательность для полного smoke-прогона. Шаги 1–4
автоматизированы через `make dev-smoke` (поднимает стек, провижининг,
собирает `keeper`, делает `keeper init`); `keeper run` остаётся как
отдельный foreground-шаг — он не должен запускаться из `dev-smoke`.

> Foreground-`keeper run` ниже — low-level вариант. Для фонового запуска с
> healthz-wait и тем же dev-env есть обёртка `make dev-keeper` (см. [Фоновые
> dev-демоны](#фоновые-dev-демоны-обёртка-над-keeper-runsoul-run)) — она делает
> ровно эти три `export` + сам `run` за один шаг.

```sh
make dev-smoke

# keeper run — отдельным foreground-шагом, с dev-env для file://-резолва
# service/destiny-артефактов (см. «Артефакты service/destiny»):
export KEEPER_SERVICE_CACHE_DIR=/tmp/keeper-dev/services
export KEEPER_DESTINY_CACHE_DIR=/tmp/keeper-dev/destiny-cache
export SOUL_STACK_ALLOW_FILE_REPOS=1
./keeper/bin/keeper run --config=dev/keeper.dev.yml

# Когда наигрался — остановить локальный keeper (Ctrl-C во foreground), либо,
# если демон ушёл в фон/осиротел:
make dev-stop
```

`make dev-smoke` под капотом запускает `make dev-provision`
([`dev/provision.sh`](../../dev/provision.sh)) — единый источник правды по
шагам provisioning (Vault KV + PKI + TLS-leaf из Vault PKI + git-репо
артефактов). Раскладка вручную больше не дублируется здесь, чтобы doc и скрипт
не расходились; читать актуальные шаги — в самом скрипте.

> **Foot-gun: `dev-provision` на свежей БД нужен ДВАЖДЫ.** `make dev-smoke`
> делает это сам (`dev-provision` → `keeper init` → `dev-provision`), но при
> ручном прогоне порядок важен: схему БД (`service_registry`/`keeper_settings`)
> создаёт `keeper init` (`migrate.Apply`), поэтому **первый** provision-проход
> на свежей БД (`dev-reset`) пропускает seed service-реестра — таблиц ещё нет.
> Реестр сервисов сеется только **вторым** проходом, после `keeper init`.
> `provision.sh` идемпотентен — двойной вызов безопасен; без второго прохода
> резолв читает пустой service-реестр (`services[]` убраны из `keeper.dev.yml`).
>
> `keeper init` при успехе печатает `Bootstrap complete. Token written to
> <path>` (JWT первого Архонта в файле `mode 0400`).

Проверяемые манипуляции после `run`:

- `curl 127.0.0.1:8080/healthz`, `/readyz`, `/openapi.yaml`, `/metrics` → 200.
- `POST /v1/operators` с Bearer JWT первого Архонта → 201 + JWT нового
  оператора; повтор → 409 `operator-already-exists`.
- `POST /v1/incarnations` для `service: redis` / `scenario: create` → 202 +
  `apply_id`. redis + 3 destiny резолвятся из file://-репо
  (созданы `dev-provision`), рендерятся Keeper-side (CEL + text/template).
- `POST /v1/operators/archon-alice/revoke` → 409 `would-lock-out-cluster`
  (инвариант ADR-013: последнего `*`-permission удалить нельзя).
- MCP на `127.0.0.1:8081`, эндпоинт `POST /mcp` (JSON-RPC 2.0; корень `/`
  отдаёт 404) — `initialize` → `tools/list` (41 tool, число закреплено тестом
  `mcp.TestCatalog_TotalCount`) → `tools/call`.
- `audit_log` (psql или `GET /v1/audit`) содержит записи трёх `source`:
  `keeper_internal` / `api` / `mcp` (ADR-022b).
- Повтор `keeper init` → exit 1 `ErrAlreadyInitialized`.
- Graceful shutdown по `SIGTERM` — 5 listeners stopped clean.

Полная цепочка вместе с примерными commit-полями зафиксирована в
commit-message `97c67e2`.

## Soul-failover демо (два keeper)

Ручная процедура для проверки **soul-failover вживую**: soul, потеряв
priority-1-keeper, переподключается к priority-2 и возвращается обратно при
восстановлении (ADR-002 multi-endpoint + failback). Продакшен-код Soul-failback
(`soul/internal/grpc` DialPriority/orderedEndpoints + `soul/cmd/soul` reconnect/
failback-loop) покрыт unit- и integration-тестами; эта процедура валидирует его
на двух **реальных** keeper-процессах — закрывает прежнее стенд-ограничение
мега-теста (раньше `dev/soul.dev.yml` хардкодил один endpoint и
`failback.enabled: false`, поэтому live-failover не воспроизводился).

Конфиги:

| Файл | kid | bootstrap | event_stream | openapi | mcp | metrics |
|---|---|---|---|---|---|---|
| [`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) | `keeper-dev-01` | 9442 | 9443 | 8080 | 8081 | 9090 |
| [`dev/keeper-b.dev.yml`](../../dev/keeper-b.dev.yml) | `keeper-dev-b` | 9542 | 9543 | 8082 | 8083 | 9092 |

Оба keeper делят PG/Redis/Vault и тот же TLS из `/tmp/keeper-dev/tls/`. **Оба**
с `acolytes: 2` — симметричные HA-участники work-queue (ADR-027). Это
обязательно: при двух живых инстансах в Conclave refuse-guard soul-shedding
(S3, `allow_unsafe_single_path_multi_keeper: false`) откажет в старте инстансу
с `acolytes: 0` на single-path. С `acolytes: 0` на keeper-a демо сломалась бы на
**фазе 4** (рестарт keeper-a при живом keeper-b → `CountLive=2` → refuse);
`acolytes>0` на обоих снимает guard на всех фазах. `dev/soul.dev.yml`
перечисляет оба keeper в `keeper.endpoints` (priority 1 = keeper-a, 2 =
keeper-b) и включает `keeper.failback.enabled: true`.

> **Быстрая демо: сократи `failback.interval`.** В `dev/soul.dev.yml` interval
> опущен → дефолт `loadFailback` = **1h** (на проде failback намеренно ленивый,
> чтобы не дёргать сессию). Чтобы failback-возврат (фаза 4) случился за секунды,
> в локальной копии конфига добавь под `failback:` строки
> `interval: 5s` и `spray: 0s`. Fallback (фаза 2) от interval не зависит —
> срабатывает сразу на разрыве priority-1.

### Процедура

```sh
# 0. Стек + provision + keeper init (как в Smoke recipe). Один раз.
make dev-smoke

# dev-env для file://-резолва — общий для обоих keeper-процессов.
export KEEPER_SERVICE_CACHE_DIR=/tmp/keeper-dev/services
export KEEPER_DESTINY_CACHE_DIR=/tmp/keeper-dev/destiny-cache
export SOUL_STACK_ALLOW_FILE_REPOS=1

# 1. keeper-a (priority 1) — терминал A.
./keeper/bin/keeper run --config=dev/keeper.dev.yml

# 2. keeper-b (priority 2) — терминал B.
./keeper/bin/keeper run --config=dev/keeper-b.dev.yml

# 3. soul init + run — терминал C. init идёт на priority-1 bootstrap (9442);
#    при недоступном priority-1 онбординг сам уходит на priority-2 (9542).
./soul/bin/soul init --config=dev/soul.dev.yml
./soul/bin/soul run  --config=dev/soul.dev.yml
```

**Фаза 1 — initial connect.** В логе soul:
`eventstream: connected ... priority=1 kid=keeper-dev-01`. Soul ведёт сессию на
keeper-a.

**Фаза 2 — падение keeper-a → fallback на keeper-b.** Убей keeper-a — Ctrl-C в
терминале A (`make dev-stop` матчит только `keeper.dev.yml`, keeper-b с
`keeper-b.dev.yml` под этот паттерн не попадает — гаси его Ctrl-C отдельно).
Soul теряет стрим, reconnect-loop дайлит по приоритету: priority-1 недоступен →
берёт priority-2. В логе:
`eventstream: connected ... priority=2 kid=keeper-dev-b`.
Проверка по реестру/apply — soul виден через keeper-b:

```sh
curl 127.0.0.1:8082/metrics | grep soul   # keeper-b openapi/metrics живы
# либо POST /v1/incarnations на keeper-b (8082) → apply доезжает до soul
```

**Фаза 3 — Watchman-вариант (без kill).** Альтернатива фазе 2: НЕ убивая
keeper-a, изолируй его от PG/Redis (например, `docker stop soul-stack-redis`).
Watchman keeper-a (probe-интервал 5s, порог 3 подряд-провала) детектит потерю
зависимости и **сам** закрывает локальные EventStream-стримы (soul-shedding S2).
Soul видит разрыв и уходит на keeper-b так же, как в фазе 2 — но keeper-a при
этом остаётся «жив» как процесс. Верни Redis (`docker start soul-stack-redis`),
чтобы keeper-a снова принимал стримы.

**Фаза 4 — восстановление keeper-a → failback.** Подними keeper-a заново
(шаг 1). Failback-loop soul (с сокращённым `interval`, см. врезку выше) проактивно
дайлит higher-priority endpoint, открывает новую сессию на keeper-a и
gracefully закрывает старую на keeper-b (zero-downtime swap). В логе:
`eventstream: connected ... priority=1 kid=keeper-dev-01` — снова на keeper-a.

```sh
# когда наигрался: soul (терминал C) и keeper-a (терминал A) — Ctrl-C либо
# `make dev-stop` (матчит keeper.dev.yml + soul.dev.yml). keeper-b
# (keeper-b.dev.yml) под паттерн dev-stop не попадает — Ctrl-C в терминале B.
make dev-stop
```

### Vault PKI

PKI-backend нужен для выпуска SoulSeed-сертификатов через gRPC `Bootstrap`-RPC
(ADR-012, ADR-014). Поднимается отдельно от KV — на mount-е, заданном в
`keeper.yml::vault.pki_mount` (например, `pki/`), с PKI role
`soul-seed` (`vault.pki_role`). `make dev-provision` делает это
автоматически; ниже — шаги в виде ручных команд для справки:

```sh
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=root

# Enable PKI secrets engine.
vault secrets enable -path=pki pki
vault secrets tune -max-lease-ttl=87600h pki

# Root certificate.
vault write pki/root/generate/internal \
  common_name="soul-stack" ttl=87600h

# Role `soul-seed` — выпускает SoulSeed-сертификаты для домена(ов)
# тестовых хостов. В проде `allowed_domains` совпадает с FQDN-конвенцией
# организации. Через PKI_ROLE_DOMAINS можно переопределить список доменов
# для `make dev-provision`.
vault write pki/roles/soul-seed \
  allowed_domains=example.com,test,localhost \
  allow_subdomains=true \
  allow_localhost=true \
  max_ttl=720h
```

После `make dev-down`/`dev-reset` запустить `make dev-provision` снова.

`dev-reset` пересоздаёт Vault dev-сервер, а значит и PKI root (новый serial).
Шаг выпуска TLS в `provision.sh` reset-aware: он скипает перевыпуск только
если `tls/vault-ca.crt` всё ещё совпадает с текущим `vault read pki/cert/ca`
и `keeper.crt` цепляется к нему; иначе серты перевыпускаются. Это держит
ClientCAs Keeper-а (`event_stream.tls.ca`) синхронными с SoulSeed-ами, которые
подписаны актуальным root — иначе mTLS-онбординг нового Soul после reset ломался бы.

## Логи и состояние

| Действие | Команда |
|---|---|
| Логи Postgres | `docker compose -f dev/docker-compose.yml logs -f postgres` |
| psql внутрь контейнера | `docker exec -it soul-stack-postgres psql -U keeper -d keeper` |
| Список таблиц | `\dt` в psql |
| Просмотр `audit_log` | `SELECT audit_id, event_type, source, created_at FROM audit_log ORDER BY created_at DESC LIMIT 20;` |
| Логи Vault | `docker compose -f dev/docker-compose.yml logs -f vault` |
| Vault status внутри контейнера | `docker exec soul-stack-vault vault status` (Sealed: false в dev-режиме) |
| Vault CLI внутри контейнера | `docker exec -it -e VAULT_TOKEN=root soul-stack-vault vault kv list secret/` |
| Логи Redis | `docker compose -f dev/docker-compose.yml logs -f redis` |
| Redis CLI внутри контейнера | `docker exec -it soul-stack-redis redis-cli` |
| Ping Redis с хоста | `redis-cli -h 127.0.0.1 -p 6381 ping` |
| Логи OTel-collector (принятые спаны) | `docker compose -f dev/docker-compose.yml logs -f otel-collector` |
| Логи Jaeger | `docker compose -f dev/docker-compose.yml logs -f jaeger` |
| Jaeger UI | http://127.0.0.1:16686 (service `keeper` / `soul`) |

## Troubleshooting

### Конфликт портов с другими docker-стеками

Compose-стек намеренно использует не-default-порты:

| Сервис | Порт хоста | Default | Причина |
|---|---|---|---|
| Postgres | `5434` | `5432` | избегает `agent-platform-postgres:5432` |
| Redis | `6381` | `6379` | избегает `dba-salt-redis:6379` и `agent-platform-valkey:6380` |
| Vault | `8200` | `8200` | без изменений |
| OTLP gRPC (collector) | `4317` | `4317` | стандартный OTLP-порт |
| Jaeger UI | `16686` | `16686` | стандартный Jaeger UI-порт |

Если занят и `5434` / `6381` / `8200` / `4317` / `16686` (`docker compose up`
падает с `bind: address already in use`):

1. Найти процесс — `lsof -nP -iTCP:5434 -sTCP:LISTEN` (или `:6381` / `:8200` /
   `:4317` / `:16686`).
2. Остановить конфликтующий контейнер (`docker stop <name>`), либо
   поменять port-mapping в `dev/docker-compose.yml` (только левую часть —
   `"5435:5432"`) и параллельно поправить связанные конфиги: для Redis —
   `dev/keeper.dev.yml` (`redis.addr`) + Vault KV (`postgres.dsn`); для OTLP —
   `otel.endpoint` в `dev/keeper.dev.yml` и `dev/soul.dev.yml` (Jaeger UI-порт
   нигде в конфигах не зашит — только в compose).

Изменять правую часть mapping-а (внутрь контейнера) не нужно — `dsn` и
healthcheck-и оперируют внутренним портом.

### `keeper init` падает `pq: connection refused`

`POSTGRES_DSN` в Vault указывает не на тот порт. После `make dev-reset`
секреты теряются — запустить `make dev-provision` снова.

### `keeper run` падает на TLS-сертификате

Сертификат в `/tmp/keeper-dev/tls/` не сгенерирован или удалён (на macOS
`/tmp` чистится при reboot **и при смене суток**). Перегенерировать —
`make dev-provision`. Если разом слетели и souls, и file://-резолв — это та же
/tmp-чистка целиком, см. [Быстрое восстановление
стенда](#быстрое-восстановление-стенда-после-tmp-чистки) (`make dev-stand`).

### Резолв service/destiny падает на git-клоне или `file:// запрещён`

- `file:// запрещён в проде (выставьте SOUL_STACK_ALLOW_FILE_REPOS=1 …)` —
  забыт env-флаг при `keeper run`. См. [Артефакты
  service/destiny](#артефакты-servicedestiny-для-резолва).
- `permission denied` / `mkdir /var/lib/soul-stack-keeper/...` — не выставлены
  `KEEPER_SERVICE_CACHE_DIR` / `KEEPER_DESTINY_CACHE_DIR` на `/tmp/keeper-dev/`.
- `ref "v1.0.0" не резолвится` / репо не найдено — не запущен
  `make dev-provision` (или `/tmp` почистился при reboot). Перезапустить.

## Integration tests

Автоматизированные тесты, которым нужен реальный Postgres, гоняются через
[testcontainers-go](https://golang.testcontainers.org/). Каждый
`internal/<pkg>/integration_test.go` поднимает `postgres:16-alpine` на
эфемерном порту, применяет миграции из `keeper/migrations/`, прогоняет
write/read round-trip и удаляет контейнер по выходу `TestMain`.

| Команда | Что делает |
|---|---|
| `make test-integration` | `go test -tags=integration -race -count=1 ./...` по всем модулям. Default-сценарий. |
| `cd keeper && go test -tags=integration -race -count=1 ./internal/auditpg/` | Прицельно один пакет. |

Требования:

- **Docker**. Testcontainers использует docker-sock; на macOS — Docker
  Desktop / OrbStack / Colima; на Linux — `dockerd` + права на сокет.
- `make test` / `make test-race` (без `-integration`) **не требуют docker** —
  файлы под `//go:build integration` исключаются из обычной сборки.
- `SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1` (или `true`) — переменная для
  CI-режима: если testcontainers не смог стартовать, тесты **fail-ятся** с
  `log.Fatalf`, а не skip-ятся. Локально переменную не ставят, тогда при
  недоступном docker `TestMain` логирует причину и возвращает exit 0
  (тесты считаются пропущенными).

### Build-tags

| Tag | Где | Зачем |
|---|---|---|
| `integration` | `keeper/internal/*/integration_test.go` | testcontainers-go; default путь для CI и локальной проверки. |
| `smoke` | `keeper/internal/migrate/smoke_test.go` | Manual fallback на случай, когда docker-sock недоступен (Codespaces, restricted CI). Запускается с `SOUL_STACK_SMOKE_DSN=postgres://... go test -tags=smoke ./internal/migrate/`. Свой `make dev-up` поднимаешь сам. |

### CI

В репозитории пока нет GitHub Actions / GitLab CI. При первой настройке
pipeline-а:

- В большинстве GitHub Actions runner-ов (`ubuntu-latest`) docker daemon
  доступен из коробки; на restricted runners — настроить
  Docker-in-Docker или `DOCKER_HOST`. В GitLab — явный `DOCKER_HOST` или
  privileged runner.
- В CI-job environment установить `SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1`,
  чтобы интеграционные тесты были обязательными (без флага молчаливо
  skip-ятся при недоступном docker — недопустимо для CI).
