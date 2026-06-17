# soulctl

Клиентский CLI оператора Soul Stack — тонкая обёртка над Operator API Keeper-а
(парный к агенту `soul`, как `kubectl` ↔ `kubelet`). По
[ADR-004](../docs/adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)
первичный интерфейс оператора — OpenAPI и MCP; CLI допустим как тонкая
обёртка над OpenAPI, не как отдельный поведенческий контракт.

Контракт API — [docs/keeper/operator-api.md](../docs/keeper/operator-api.md) и
[docs/keeper/openapi.yaml](../docs/keeper/openapi.yaml).

## Команды

Дерево команд — семь верхних групп. Каждая команда — тонкая обёртка над
Operator API; ссылки на эндпоинты — [operator-api.md](../docs/keeper/operator-api.md).

### `incarnation` — runtime-инстансы сервисов

| Команда | Назначение | Флаги |
|---|---|---|
| `incarnation list` | перечислить incarnation | `--service`, `--status`, `--coven` (client-side), `--limit`, `--offset` |
| `incarnation get <name>` | показать incarnation (spec/state/status/covens), всегда JSON | — |
| `incarnation run <name> <scenario>` | запустить scenario на incarnation | `--input <json>`, `--dry-run`, `--wait`, `--wait-timeout` (default `5m`) |
| `incarnation history <name>` | записи `state_history` | `--limit`, `--offset` |
| `incarnation check-drift <name>` | Scry-проверка drift (ADR-031) | `--input <json>` (override converge-input) |

`--wait` у `run` поллит `history` + `status` incarnation (отдельного
`/v1/applies/{apply_id}` в MVP нет), fail-fast при `error_locked` /
`migration_failed` / `destroy_failed`.

### `souls` — реестр управляемых агентов (множественное)

| Команда | Назначение | Флаги |
|---|---|---|
| `souls list` | перечислить зарегистрированные Souls | `--coven` (повторяемый), `--status`, `--transport` (`agent\|ssh`), `--limit`, `--offset` |
| `souls get <sid>` | показать Soul по SID (фоллбэк через list — `soul.get` не выставлен в MVP), JSON | — |
| `souls ssh-target set <sid>` | задать per-host `ssh_target` push-flow ↔ `PUT /v1/souls/{sid}/ssh-target` | `--port` (default `22`), `--user` (default `root`), `--soul-path` (default `/usr/local/bin/soul`), `--ssh-provider` |
| `souls ssh-target bulk-set` | массово задать `ssh_provider` всем Souls в Coven (client-side list→per-SID PUT) | `--coven` (обяз.), `--ssh-provider` (обяз.), `--port`, `--user`, `--soul-path` |

### `soul` — одиночные действия на конкретном хосте (единственное)

Отделён от `souls` намеренно: `souls` — реестр (list/get), `soul` — действие на одном хосте.

| Команда | Назначение | Флаги |
|---|---|---|
| `soul exec <sid>` | ad-hoc одиночный модуль на Soul (Errand, ADR-033) ↔ `POST /v1/souls/{sid}/exec` | `--module` (обяз.), `--input <json>` (default `{}`), `--timeout` (сек, default `30`, 1..300), `--dry-run`, `--poll` (default `true`) |

Whitelist модулей и cap stdout/stderr применяет Soul-side errand-runner.
`--poll` дожимает async-результат (при превышении server-cap) через
`errand get` до терминала.

### `errand` — реестр Errand-ов (ADR-033)

| Команда | Назначение | Флаги |
|---|---|---|
| `errand list` | перечислить Errand-ы | `--sid`, `--status`, `--started-after <RFC3339>`, `--limit`, `--offset` |
| `errand get <errand_id>` | показать состояние Errand-а | `--poll` (default `false`) — дожать running до терминала |
| `errand cancel <errand_id>` | отменить in-flight Errand (permission `errand.cancel`) | — |

### `archon` — аутентификация и идентичность оператора

| Команда | Назначение | Флаги |
|---|---|---|
| `archon login` | сохранить keeper_url + JWT в credentials.yaml (валидируется ping-ом) | `--keeper-url` (обяз.), `--jwt-file` (обяз.) |
| `archon whoami` | показать текущего Архонта (AID + claims из JWT) | — |
| `archon logout` | удалить credentials.yaml | — |

Подробности — раздел «Аутентификация» ниже.

### `push-providers` (алиас `push-provider`) — params SSH-плагинов push-flow

Замещает inline-форму `keeper.yml::push.providers[]` (ADR-032 amendment).
Sensitive params (`secret_id`/`token`/`password`/`private_key`) обязаны быть
vault-refs (`vault:<path>`).

| Команда | Назначение | Флаги |
|---|---|---|
| `push-providers create <name>` | создать запись (permission `push-provider.create`) | `--params <json>` |
| `push-providers update <name>` | заменить params (replace-семантика) | `--params <json>` (обяз.) |
| `push-providers delete <name>` | удалить запись | — |
| `push-providers list` | перечислить, JSON | `--name-pattern` (LIKE-форма, напр. `vault%`), `--limit` (default `100`), `--offset` |
| `push-providers get <name>` | прочитать запись, JSON | — |

### `run` — высокоуровневый UX-зонтик (Salt-parity)

Сосуществует с `incarnation run` / `soul exec` без deprecation: `run` —
оператор-frontend, low-level прямые команды остаются для CI/скриптов.

| Команда | Назначение | Backend |
|---|---|---|
| `run scenario <service>/<scenario>` | батчевый scenario через Voyage | `POST /v1/voyages` (`kind=scenario`, ADR-043) |
| `run cmd '<команда>'` | ad-hoc shell-команда на N хостов | `POST /v1/voyages` (`kind=command`, ADR-043) |
| `run push <destiny@ref>` | push-применение destiny через SSH-провайдер | `POST /v1/push/apply` |

Per-команда флаги:

- `run scenario`: `--incarnation` (если не задано — auto-detect: ровно одна
  incarnation на сервис), `--input <json>`, `--batch-size`, `--batch N|N%`,
  `--max-failures N|N%`, `--concurrency` (0→default 50, max 500),
  `--on-failure` (`continue`\|`abort`), `--wait`, `--wait-timeout` (default `10m`).
  Target-флаги к scenario неприменимы (цель — инкарнация, не хост) — передача
  `--target-*` сюда даёт ошибку.
- `run cmd`: target обязателен; `--module` (default `core.cmd.shell`),
  `--concurrency`, `--on-failure`, `--batch-size`, `--batch N|N%`,
  `--max-failures N|N%`, `--wait`, `--wait-timeout` (default `10m`).
- `run push`: `--ssh-provider` (пусто → server-default), `--input <json>`,
  `--cleanup-stale-versions`. Target ограничен `--target-sids` (inventory
  exact-match); `coven`/`glob`/`regex`/`where` для push недоступны — ошибка
  валидации на CLI.

Универсальные `--target-*` флаги (`run cmd`/`run push`):

| Флаг | Семантика |
|---|---|
| `--target-sids host1,host2` | CSV exact-match SID-ов |
| `--target-coven prod-eu,dc1` | CSV Coven-меток (AND по `souls.coven`) |
| `--target-glob 'web-*'` | shell-glob → CEL `sid.glob("X")` |
| `--target-regex 'host-[0-9]+'` | regex → CEL `sid.matches("X")` |
| `--target-where '<CEL>'` | raw CEL-предикат; AND-merge с glob/regex |

`glob`/`regex`/`where` склеиваются в один итоговый `where` через `&&`; `sids`
и `coven` остаются отдельными полями (backend делает AND-пересечение —
invocation сужает scope, не расширяет).

## Глобальные флаги

| Флаг | Назначение |
|---|---|
| `--output / -o table\|json\|yaml` | Формат вывода. `table` (default) — kubectl-стиль через `text/tabwriter`. `json` — pretty-JSON. `yaml` зарезервировано, пока совпадает с `json`. Часть команд (`get`-формы, `push-providers list`) всегда печатают JSON. |
| `--config <path>` | Путь к credentials.yaml вместо `~/.config/soul-stack/credentials.yaml`. |
| `--version` | Версия бинаря (инжектится через `-ldflags`, см. [RELEASING.md](../RELEASING.md)). |

## Аутентификация

```yaml
# ~/.config/soul-stack/credentials.yaml — mode 0600
keeper_url: https://keeper.example.com:8443
archon_jwt: <JWT>
```

- `soulctl archon login --keeper-url <url> --jwt-file <path>` — читает JWT из
  файла, валидирует через `GET /v1/incarnations?limit=1` (любой авторизованный
  endpoint), сохраняет credentials.
- `soulctl archon whoami` — печатает AID + claims из локального JWT
  (signature не перепроверяется — keeper уже принял JWT при login).
- `soulctl archon logout` — удаляет credentials.yaml.

## Ошибки

| HTTP | Сообщение CLI |
|---|---|
| 401 | `not authenticated. Run `soulctl archon login`` |
| 403 | `forbidden: <detail из RFC 7807>` |
| 404 | `not found: <detail>` |
| 5xx | `keeper error: <detail>` |

`--output json` для list-команд при ошибке возвращает non-zero exit и стандартное
ProblemDetails в stderr (не пустой JSON), чтобы скрипты не путались.

## Сборка

```sh
make build-soulctl   # → soulctl/bin/soulctl
make build           # включает soulctl
make test            # юнит-тесты
```

## Известные ограничения (TODO)

Эти разрывы — между openapi MVP и нуждами CLI. Не обходятся костылями;
поднимаются PM-у при появлении ADR / расширении контракта.

- **`/v1/whoami` отсутствует.** В Operator API MVP нет отдельного whoami; AID
  и роли извлекаются из JWT claims. signature локально не проверяется (Keeper
  уже валидировал JWT при login через ping).
- **`/v1/applies/{apply_id}` отсутствует** ([operator-api.md → Async operations](../docs/keeper/operator-api.md)).
  `incarnation run --wait` поллит `GET /v1/incarnations/{name}/history` (запись с
  apply_id появляется после успешного commit) и `GET /v1/incarnations/{name}`
  (status incarnation, fail-fast при error_locked/migration_failed/destroy_failed).
- **`GET /v1/souls/{sid}` отсутствует** в MVP (нет permission `soul.get`,
  [operator-api.md → ID в path](../docs/keeper/operator-api.md)). `soulctl souls
  get <sid>` использует фоллбэк через list + client-side фильтр. Большие кластеры
  (≥10⁴ хостов) — кандидат на отдельный endpoint, когда появится permission.
- **`coven`-фильтр у incarnation list — client-side.** В openapi у
  `/v1/incarnations` нет query-параметра `coven` (есть только у `/v1/souls`).
  Поле `total` в ответе соответствует фильтру service/status сервера, не client-
  side фильтру по coven. При необходимости — расширение openapi.
- **history-команда: STATUS/DURATION пустые.** Записи `state_history`
  существуют только при успешном commit, поэтому в таблице две колонки пустые.
  Если в openapi появится `apply_runs`-эндпоинт с full lifecycle — заполнятся.

## Структура пакета

```
soulctl/
  cmd/soulctl/main.go               # entry, version-bind
  internal/
    cmd/                            # cobra-команды (root + семь групп)
      root.go                       # глобальные флаги, loadClient, renderAPIError
      archon.go                     # archon login / whoami / logout
      incarnation.go                # incarnation list / get / run / history / check-drift
      souls.go                      # souls list / get / ssh-target + soul exec
      errand.go                     # errand list / get / cancel
      pushprovider.go               # push-providers create / update / delete / list / get
      run.go                        # run-зонтик (root)
      run_scenario.go               # run scenario
      run_cmd.go                    # run cmd
      run_push.go                   # run push
      run_target.go                 # общие --target-* флаги
    client/                         # типизированный HTTP-клиент
    config/config.go                # credentials.yaml loader
    output/output.go                # table / json renderers
```
