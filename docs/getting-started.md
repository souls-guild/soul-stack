# Getting Started — поднять Soul Stack за ~30 минут

Quickstart для оператора, который впервые знакомится с Soul Stack: один Keeper, обязательная инфра локально (Postgres + Redis + Vault), первый Архонт, один онбордженный Soul и применённый сценарий `hello-world`. По шагам, с командами.

Это **локальный demo-сетап** для знакомства, не прод-инсталляция. Прод-раскатка (HA, managed-инфра, persistent Vault, TLS-материал, systemd) — [docs/operations/deployment.md](operations/deployment.md). Что не входит в бету — [known-limitations.md](known-limitations.md).

## Что понадобится

| Инструмент | Зачем |
|---|---|
| Go 1.26+ | собрать бинари `keeper` / `soul` из исходников |
| Docker + docker compose | поднять Postgres / Redis / Vault / OTel локально (`dev/docker-compose.yml`) |
| `curl`, `git`, `openssl` | вызовы Operator API, материализация service-репо, генерация ключей |
| `make` | dev-таргеты (`make dev-up` / `make build` / `make dev-smoke`) |

Все команды — из корня репозитория.

## Шаг 0. Получить исходники

В бете дистрибуции бинарных релизов нет — Soul Stack собирается из исходников. Доступ к коду на этом этапе — **по инвайту в приватный GitHub-репозиторий** (как получить инвайт — [SUPPORT.md](../SUPPORT.md)). После принятия инвайта:

```sh
git clone git@github.com:<org>/soul-stack.git
cd soul-stack
```

Сборка бинарей — `make build` (шаг 3). Версию собранного бинаря печатает `keeper version` (формат `keeper <версия> (<go-runtime>)` — версия инжектится линкером при `make build`):

```sh
./keeper/bin/keeper version
```

## Шаг 1. Инфра: Postgres + Redis + Vault

Обязательный контур Keeper-кластера — три компонента: PostgreSQL + Redis + Vault ([ADR-053](adr/0053-dependency-tiers.md)). Все три проверяются на старте; без любого из них Keeper не стартует (fail-fast).

Локальный dev-стек поднимается одной командой — она запускает `dev/docker-compose.yml` (Postgres на `:5434`, Vault dev-mode на `:8200`, Redis на `:6381`, плюс OTel-collector + Jaeger):

```sh
make dev-up
```

> **Vault здесь — dev-mode** (in-memory, auto-unseal, HTTP без TLS, root-token `root`). Подходит **только** для локального знакомства — данные теряются при рестарте контейнера. Прод требует persistent storage + auto-unseal + TLS, см. [operations/infra.md → Vault](operations/infra.md#vault).

## Шаг 2. Провижининг секретов и TLS

Keeper читает DSN Postgres, JWT signing-key и выпускает SoulSeed-сертификаты через Vault. Локальный провижининг (запись KV-секретов, включение PKI-engine, выпуск self-issued TLS-материала Keeper-а) делает один идемпотентный скрипт:

```sh
make dev-provision
```

Что он кладёт (детали — [dev/provision.sh](../dev/provision.sh)):

- `secret/keeper/postgres` (поле `dsn`), `secret/keeper/jwt-signing-key`, PKI-engine `pki/` с ролью `soul-seed`;
- TLS-материал Keeper-а в `/tmp/keeper-dev/tls/` (leaf-cert + корневой Vault-CA, на который позже цепляется SoulSeed).

Повторный запуск безопасен — каждый шаг проверяет своё состояние.

## Шаг 3. Собрать бинари

```sh
make build
```

Результат — `keeper/bin/keeper`, `soul/bin/soul`, `soul-lint/bin/soul-lint`.

## Шаг 4. Bootstrap первого Архонта

**Archon** — оператор Soul Stack (идентификатор вида `archon-<имя>`). При первой инициализации реестр операторов пуст, и без bootstrap любой вызов API вернёт `403` (default-deny). Bootstrap — administrative subcommand `keeper init` ([ADR-013](adr/0013-bootstrap-archon.md), [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md)):

```sh
./keeper/bin/keeper init \
  --archon=archon-alice \
  --config=dev/keeper.dev.yml \
  --credential-out=/tmp/keeper-dev/archon-alice.jwt
```

Что происходит: под PG advisory lock проверяется, что реестр операторов пуст, создаётся первый Архонт с ролью `cluster-admin` (permissions `["*"]`), выпускается JWT (bootstrap-TTL), JWT пишется в `--credential-out` с `mode 0400`. Эта же команда применяет схему БД (миграции) идемпотентно.

> Файл `archon-alice.jwt` — admin-credential с правами `*`. В реальной инсталляции его сразу прячут в password manager / Vault и отзывают после настройки остальных операторов.

После bootstrap реестр сервисов ещё пуст. Повторный провижининг засевает его demo-записями (см. [Шаг 7, dev-shortcut](#шаг-7-apply-применить-сценарий-hello-world)):

```sh
make dev-provision
```

## Шаг 5. Запустить Keeper

`make dev-keeper` поднимает Keeper с выверенным dev-окружением (writable cache-каталоги для резолва service-артефактов, `file://`-репо разрешены), ждёт `healthz`:

```sh
make dev-keeper
```

Проверка готовности:

```sh
curl -s http://127.0.0.1:8080/healthz        # → ok
```

Слушающие порты dev-Keeper-а (`dev/keeper.dev.yml`): OpenAPI `:8080`, MCP `:8081`, metrics `:9090`, Bootstrap-RPC `:9442`, EventStream `:9443`. Маппинг портов и listener-ов в проде — [operations/deployment.md → Сетевые порты](operations/deployment.md#сетевые-порты).

> **Смотреть API в браузере — `GET /docs`** (RapiDoc-вьювер, [ADR-054](adr/0054-openapi-code-first.md)). Откройте `http://127.0.0.1:8080/docs`: страница показывает поле ввода Archon JWT — вставьте токен (см. ниже), и она подгрузит полную спеку с поиском по эндпоинтам (full-text) и кнопкой «Try It». Сама спека (`GET /openapi.json`) за JWT — без токена видно только поле ввода, API-поверхность не раскрыта. Токен живёт лишь в текущей вкладке (session storage), не сохраняется персистентно.

Удобный alias: токен Архонта для вызовов API положите в переменную окружения.

```sh
TOKEN=$(cat /tmp/keeper-dev/archon-alice.jwt)
```

## Шаг 6. Онбордить один Soul

**Soul** — агент на управляемом хосте. Онбординг — через CSR: приватный ключ генерируется на хосте и никогда его не покидает ([ADR-002](adr/0002-transport-grpc-ha.md), [soul/onboarding.md](soul/onboarding.md)). Поток в два хода: оператор регистрирует хост и получает одноразовый bootstrap-токен, затем `soul init` на хосте обменивает токен + CSR на SoulSeed-сертификат.

### 6.1. Зарегистрировать хост (получить bootstrap-токен)

`SID` Soul-а = FQDN хоста. Для локального demo используем имя текущей машины (или любой FQDN из `example.com` — он покрыт PKI-ролью `soul-seed`):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/souls \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sid": "host-01.example.com", "transport": "agent", "covens": ["demo"]}'
```

В ответе — `bootstrap_token` (**возвращается один раз**), `expires_at` (TTL по умолчанию 24h). `covens: ["demo"]` — стабильная метка хоста, по ней позже можно таргетировать сценарии. Потерянный токен восстановить нельзя — только перевыпустить через `POST /v1/souls/{sid}/issue-token` ([soul/onboarding.md → Восстановление](soul/onboarding.md#восстановление-потерян-токен)).

### 6.2. Применить токен на хосте (`soul init`)

`soul init` читает токен из stdin (флаг с токеном не поддерживается — чтобы не светить в `ps`), генерирует приватный ключ + CSR, подключается к Bootstrap-listener-у Keeper-а и раскладывает полученный SoulSeed. Нужен `soul.yml` с адресом Keeper-а и путём к доверенному CA.

Минимальный `soul.yml` для локального demo (CA — корневой Vault-CA из шага 2):

```yaml
sid: host-01.example.com
paths:
  modules: /tmp/soul-demo/modules
  seed:    /tmp/soul-demo/seed
keeper:
  endpoints:
    - host: 127.0.0.1
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 1
  tls:
    ca: /tmp/keeper-dev/tls/vault-ca.crt
```

Полный контракт `soul.yml` — [soul/config.md](soul/config.md). Онбординг:

```sh
printf '%s' '<bootstrap_token из 6.1>' | ./soul/bin/soul init --config /tmp/soul-demo/soul.yml
```

При успехе SoulSeed (cert/key/ca) раскладывается в `paths.seed`, запись в `souls` переходит `pending → connected`. Запустить демон (держит EventStream к Keeper-у):

```sh
./soul/bin/soul run --config /tmp/soul-demo/soul.yml &
```

Проверка — хост виден как `connected`:

```sh
curl -s http://127.0.0.1:8080/v1/souls -H "Authorization: Bearer $TOKEN"
```

> **Способ доставки токена и бинаря на реальный хост** — выбор оператора (SSH/SCP, cloud-init, CI, `keeper.push`). Перечень — [soul/onboarding.md → Способы доставки](soul/onboarding.md#способы-доставки-токена).

## Шаг 7. Apply: применить сценарий `hello-world`

Что применяем: **service** `hello-world` ([examples/service/hello-world/](../examples/service/hello-world/)) — минимальный сервис со сценарием `create`, который пишет greeting-файл `/tmp/soul-stack-hello` на каждом хосте incarnation и фиксирует путь в `incarnation.state`.

Чтобы Keeper мог резолвить сервис, он должен быть в реестре сервисов (git-источник + ref). В проде это `POST /v1/services`:

```sh
curl -s -X POST http://127.0.0.1:8080/v1/services \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "hello-world", "git": "https://git.internal/svc/hello-world.git", "ref": "main"}'
```

> **Dev-shortcut.** Для локального знакомства держать git-репо неудобно. `make dev-provision` (шаг 2) уже материализует `hello-world` как локальный `file://`-репо и засевает реестр сервисов demo-записями — отдельный `POST /v1/services` тогда не нужен. Это **dev-only**: `file://`-резолв включается флагом `SOUL_STACK_ALLOW_FILE_REPOS=1`, который проставляет `make dev-keeper`; в проде источник сервиса — настоящий git-URL.

Создать incarnation — запускает сценарий `create` сервиса на хостах. Привяжем к coven `demo` (там наш Soul из шага 6):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hello-demo",
    "service": "hello-world",
    "covens": ["demo"],
    "input": { "greeting": "hello from soul-stack" }
  }'
```

Ответ `202 Accepted` с `apply_id` — операция асинхронная ([operator-api/incarnations.md → POST /v1/incarnations](keeper/operator-api/incarnations.md)).

## Шаг 8. Увидеть результат

Опросить статус incarnation (`applying` → `ready` при успехе, `error_locked` при провале):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo -H "Authorization: Bearer $TOKEN"
```

При `status: ready` — `incarnation.state.greeting_file` указывает на созданный путь. Прямо на хосте:

```sh
cat /tmp/soul-stack-hello        # → hello from soul-stack
```

История прогонов (snapshots в `state_history`):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo/history -H "Authorization: Bearer $TOKEN"
```

## Всё-в-одном (dev-smoke)

Шаги 1–4 + seed реестра автоматизированы одной командой (`make dev-smoke` = `dev-up` → `dev-provision` → собрать keeper → `keeper init` → повторный `dev-provision` для seed реестра):

```sh
make dev-smoke      # инфра + провижининг + bootstrap первого Архонта + seed demo-сервисов
make dev-keeper     # запустить Keeper
```

Дальше — онбординг Soul (шаг 6) и apply (шаги 7–8). Полный подъём dev-стенда (включая Web-UI из companion-репозитория) — `make dev-stand`; токен для ad-hoc вызовов — `TOKEN=$(make dev-jwt)`.

## Troubleshooting первого запуска

Типовые спотыкания на demo-сетапе и что с ними делать. Прод-проблемы — [operations/infra.md](operations/infra.md).

- **После рестарта пропали секреты / Keeper падает на старте «cannot read postgres dsn».** Dev-Vault — это in-memory dev-mode (шаг 1): данные живут только пока жив контейнер, рестарт `make dev-up` стирает KV/PKI. Лечится повторным `make dev-provision` (шаг 2) — он идемпотентно перезаписывает `secret/keeper/*` и PKI-роль. Для постоянного хранилища нужен прод-Vault (persistent storage + auto-unseal), см. [operations/infra.md → Vault](operations/infra.md#vault).
- **`soul init` падает на TLS / «certificate is not valid for host».** `SID` Soul-а = FQDN (шаг 6.1), и PKI-роль `soul-seed` выписывает SoulSeed только на имена из своего allowed-домена (в demo — `*.example.com`). Используйте FQDN из этого домена (`host-01.example.com`), а не короткое hostname или произвольное имя. CA в `soul.yml` (`keeper.tls.ca`) должен указывать на корневой Vault-CA из шага 2 (`/tmp/keeper-dev/tls/vault-ca.crt`) — иначе Soul не доверяет цепочке Keeper-а.
- **`422 Unprocessable Entity` на вызове API.** Это schema-валидация запроса (huma): тело/квери не прошли контракт — неизвестное поле, значение вне enum, нарушение формата. Ответ несёт `detail` с конкретным путём поля; сверьте payload со спекой в `/docs` (валидно, а не угадывая). Это не баг сервера — 422 отдаётся до бизнес-логики.
- **API вернул `401` хотя токен «есть».** JWT короткоживущий и **немедленного отзыва по содержимому токена нет** — отозванный Архонт работает до `exp`, а истёкший токен просто перестаёт приниматься. Если получили `401` — выпишите свежий токен (bootstrap-JWT перевыпуск — повторный `keeper init` недоступен после первого Архонта; для dev — `TOKEN=$(make dev-jwt)`; в проде — через `POST /v1/operators/...`, см. [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md)).
- **`soul init` прошёл, но хост ещё не `connected` в `GET /v1/souls`.** Переход `pending → connected` не мгновенный: статус в реестре — это снимок `souls.status`, который Keeper сводит с фактом presence фоном (Reaper-правило `mark_disconnected`, lease-aware reconcile — [keeper/reaper.md](keeper/reaper.md)). После `soul run` подождите несколько секунд и опросите снова. Авторитет «Soul online» — живой стрим (Redis-lease), снимок догоняет его с лагом — это by-design, не зависание.
- **WSL2-грабли** (если поднимаете demo под Windows/WSL2):
  - `ETXTBSY` при `make build` / запуске бинаря — файл бинаря держится другим процессом. Остановите ранее запущенный `keeper`/`soul` (`pkill -x keeper`, `pkill -x soul` — именно `-x`, точное имя, не `-f`) и пересоберите.
  - Docker-инфра не поднимается — проверьте, что **Docker Desktop запущен** и WSL2-интеграция включена; без него `make dev-up` не найдёт docker-демон.
  - После перезагрузки исчезли `/tmp/keeper-dev/*` (JWT, TLS-материал) — WSL2 чистит `/tmp` при старте. Прогоните `make dev-smoke` заново (он пересоздаёт bootstrap-Архонта и провижининг).

Если поведение не совпадает с описанным и похоже на баг — заведите **GitHub Issue** (шаблон «Bug report», как и куда — [SUPPORT.md](../SUPPORT.md)). Поддержка беты — best-effort, без SLA.

## Что дальше

- [known-limitations.md](known-limitations.md) — что не входит в бету (cloud-provisioning, MCP-покрытие cadence, audit-scaling).
- [operations/deployment.md](operations/deployment.md) — прод-раскатка: HA multi-keeper, managed-инфра, systemd, deb/rpm.
- [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md) — второй+ Архонт, роли, permission-строки, scope.
- [operations/infra.md](operations/infra.md) — прод-настройка Postgres / Redis / Vault, backup/restore.
- [scenario/](scenario/README.md) и [destiny/](destiny/README.md) — как писать собственные сценарии и Destiny.
- [keeper/operator-api.md](keeper/operator-api.md) — полный Operator API.
