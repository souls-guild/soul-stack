# Свой первый сервис end-to-end

Этот гайд — мост между [getting-started.md](../getting-started.md) (где ты поднял один Keeper, онбордил один Soul и применил готовый `hello-world`) и эксплуатацией. Здесь ты **сам соберёшь сервис с нуля**: напишешь его файлы, зарегистрируешь в Keeper-е, создашь инкарнацию и увидишь результат на хосте.

Это не reference-спека — это пошаговый туториал. Где нужна полная грамматика (все поля задачи, вся CEL-семантика, весь формат миграций) — даю ссылку на нормативный документ. Сам гайд держится на **реальном работающем примере**: [`examples/service/hello-world/`](../../examples/service/hello-world/). Каждый кусок YAML ниже — это файл оттуда, не выдумка.

## 1. Цель и предпосылки

**Что построим.** Минимальный сервис `hello-world`: одна операция `create`, которая пишет greeting-файл `/tmp/soul-stack-hello` на каждом хосте инкарнации, подставляя текст из параметра оператора, и фиксирует путь к файлу в `incarnation.state` (Postgres). Этого достаточно, чтобы пройти всю цепочку: параметр оператора → CEL-рендер → apply на хосте → коммит state.

**Что нужно перед стартом** (всё из [getting-started.md](../getting-started.md)):

- рабочий Keeper (`make dev-keeper`, отвечает на `http://127.0.0.1:8080/healthz`);
- хотя бы один онбордженный Soul в статусе `connected`, привязанный к coven `demo`;
- токен Архонта в переменной окружения: `TOKEN=$(make dev-jwt)` (или `TOKEN=$(cat /tmp/keeper-dev/archon-alice.jwt)`).

Проверка, что Soul на месте:

```sh
curl -s http://127.0.0.1:8080/v1/souls -H "Authorization: Bearer $TOKEN"
```

В ответе должен быть хост со `status: connected` и `covens: ["demo"]`.

## 2. Раскладка service-репо

Сервис — это git-репозиторий определённой формы. Один сервис = один тип сервиса (`hello-world`, `redis`, `postgres-ha`) = один репозиторий. Версия сервиса — это git-ref (tag или branch), под которым закоммичены файлы; поля `version:` в манифесте **нет** ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)).

Раскладка нашего `hello-world` (минимальная — обязательны только `service.yml` и хотя бы один сценарий):

```
hello-world/
├── service.yml                     # манифест: имя, версия state-схемы, структура incarnation.state
├── essence/
│   └── _default.yaml               # baseline-параметры для всех инкарнаций (подложка)
└── scenario/
    ├── create/
    │   ├── main.yml                # операция «создать»: input + state_changes + tasks
    │   └── tests/
    │       └── greeting-hello/case.yml   # L0-тест: проверяет рендер сценария без хостов
    └── converge/
        └── main.yml                # желаемое состояние для drift-проверки (check-drift)
```

Чего здесь специально **нет** и почему — пригодится, чтобы не искать лишнего:

- `migrations/` — миграции нужны только при `state_schema_version > 1` ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)); у нас версия `1`.
- `destiny[]` / `modules[]` в `service.yml` — наш сценарий использует только **core-модули** (`core.file.present`), а они всегда доступны и в манифесте не перечисляются ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).
- `templates/` — `.tmpl`-файлы нужны, когда контент рендерится Go-шаблоном; у нас контент приходит inline через `${ input.greeting }`.

Полный формат раскладки и манифеста — [docs/service/manifest.md](../service/manifest.md).

## 3. `service.yml` — манифест

Манифест короткий по дизайну: только метаданные сервиса и **контракт на структуру runtime-state**. Никаких задач — они живут в сценариях.

[`examples/service/hello-world/service.yml`](../../examples/service/hello-world/service.yml):

```yaml
name: hello-world
state_schema_version: 1
description: Минимальный service с реальным state-change для E2E (запись файла + commit в incarnation.state)

state_schema:
  type: object
  properties:
    greeting_file:
      type: string
```

Разбор полей:

- **`name`** — имя типа сервиса в kebab-case (`^[a-z][a-z0-9-]*$`). Совпадает с именем папки без префикса `service-`. По этому имени Keeper резолвит сервис при создании инкарнации.
- **`state_schema_version`** — версия **структуры** `incarnation.state`, а не версия сервиса (версия = git-ref). Инкрементируется только при breaking-изменении структуры state и тогда требует миграции. У нас `1` → каталог `migrations/` не нужен.
- **`description`** — одна-две фразы; видно в UI Keeper-а, MCP-каталоге и выводе `soul-lint`.
- **`state_schema`** — JSON Schema (draft-07-совместимая, на корне всегда `type: object`), описывающая JSONB-поле `incarnation.state` в Postgres. Здесь мы объявляем единственное поле `greeting_file` типа string. Keeper валидирует state против этой схемы при создании инкарнации и при апгрейде версии схемы.

Полный список полей манифеста (включая `destiny[]` / `modules[]` для сервисов с зависимостями) — [docs/service/manifest.md → `service.yml`](../service/manifest.md#serviceyml--манифест).

## 4. `scenario/create/main.yml` — пишем операцию

Сценарий — это одна операция над сервисом (CRUD-style: `create` / `add_user` / `restart` / …). Каждая папка `scenario/<name>/` — отдельная операция; Keeper находит их auto-discover-ом, перечислять в манифесте не нужно. Точка входа — `main.yml` с тремя блоками: `input:` (контракт входов), `state_changes:` (что писать в state), `tasks:` (шаги).

[`examples/service/hello-world/scenario/create/main.yml`](../../examples/service/hello-world/scenario/create/main.yml):

```yaml
name: create
description: Создаёт greeting-файл на каждом хосте incarnation и фиксирует путь в incarnation.state.

input:
  greeting:
    type: string
    required: true
    description: Текст, который записывается в greeting-файл.

state_changes:
  sets:
    greeting_file: /tmp/soul-stack-hello

tasks:
  - name: Write greeting file on every host of the incarnation
    module: core.file.present
    params:
      path: /tmp/soul-stack-hello
      content: "${ input.greeting }"
```

Разбор по блокам.

**`input:` — параметры сценария.** Контракт того, что оператор обязан/может передать при запуске. Здесь один параметр `greeting`: строка, обязательная (`required: true`). Keeper валидирует переданный `input` против этого контракта **до** запуска — если оператор не передал `greeting`, прогон не стартует. Полный стандарт `input:` (типы, форматы, валидация, `$ref` на переиспользуемые схемы) — [docs/input.md](../input.md).

**`tasks:` — шаги операции.** Один шаг:

- `module: core.file.present` — core-модуль, который обеспечивает наличие файла с заданным содержимым (идемпотентно: если файл уже такой, изменения нет). Поведение модуля и все его параметры — [docs/module/core/file/README.md](../module/core/file/README.md). Полный список core-модулей — [ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список).
- `params.path` — куда писать; `params.content` — что писать.
- **`content: "${ input.greeting }"`** — здесь работает шаблонизатор. Маркер `${ … }` — это CEL-интерполяция: на фазе рендера Keeper подставляет в строку значение `input.greeting`. То есть текст файла приходит из параметра оператора, а не зашит в сценарий. Граница «где CEL, где Go-шаблон», маркер `${ … }`, security-модель — нормативно в [docs/templating.md](../templating.md).

**`state_changes:` — что зафиксировать в state после успеха.** Ключ `sets` — карта `<поле state>: <значение>`. После того как **все** хосты инкарнации успешно отработали (cross-host barrier), Keeper пишет `incarnation.state.greeting_file = /tmp/soul-stack-hello` в Postgres. Здесь значение — литерал; в общем случае это тоже CEL-выражение (можно `${ input.* }`, `${ register.* }` и др.). Инвариант barrier/state-commit и грамматика `state_changes` (`sets` / future `appends` / `modifies`) — [docs/scenario/orchestration.md → §7](../scenario/orchestration.md#7-инвариант-barrier--state-commit).

> Почему `on:` / `where:` тут нет. `on:` — это таргет шага (на каких хостах выполнять). Опущенный `on:` означает «весь incarnation» — все хосты под корневым coven `${ incarnation.name }`. Нам этого достаточно. Таргетинг по covens (`on:`) и волатильный per-host предикат (`where:`) — [orchestration.md → §3–§4](../scenario/orchestration.md#3-таргет-шага--on).

### Тест сценария (опционально, но полезно)

Рядом с сценарием лежит L0-тест — он проверяет, что **рендер** даёт ожидаемые задачи, без реальных хостов. [`scenario/create/tests/greeting-hello/case.yml`](../../examples/service/hello-world/scenario/create/tests/greeting-hello/case.yml):

```yaml
name: create writes greeting file with input.greeting

fixtures:
  input:
    greeting: hi

assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
```

Смысл: «при `input.greeting=hi` отрендерится ровно одна задача `core.file.present` с `content: hi`». Это ловит регресс в рендере (например, если кто-то сломает CEL-интерполяцию). Уровни тестирования — [docs/destiny/testing.md](../destiny/testing.md).

## 5. `essence/_default.yaml` — параметры по умолчанию

Essence — иерархически собираемые параметры инкарнации (Salt-pillar аналог). `_default.yaml` — baseline для всех инкарнаций; поверх него можно класть overlay'и по Coven-меткам (`essence/coven/<label>.yaml`) и по OS-family (`essence/os/<family>.yaml`).

[`examples/service/hello-world/essence/_default.yaml`](../../examples/service/hello-world/essence/_default.yaml):

```yaml
greeting: hello from soul stack
```

В нашем минимальном сценарии `essence.greeting` — это **подложка на будущее**: сам `create` требует `input.greeting` обязательным, поэтому текст приходит от оператора. Essence показывает, как сервис мог бы нести значения по умолчанию, не требуя их от оператора каждый раз. Полная нормативная спека pipeline сборки essence (overlay'и, `_stack.yaml`) — [docs/architecture.md → Essence](../architecture.md#essence-pipeline-сборки).

> `input:` vs `essence` — в чём разница. `input:` — это контракт **на вызов сценария** (оператор передаёт при запуске; валидируется до прогона). `essence` — это **параметры инкарнации**, собранные из git-подложки + overlay'ев; они доступны как контекст и могут подставляться в задачи. У них разный жизненный цикл: input — per-run, essence — per-incarnation.

## 6. Валидация офлайн

Прежде чем регистрировать сервис, прогони статический линтер — он ловит структурные ошибки манифеста и сценария без запуска Keeper-а:

```sh
./soul-lint/bin/soul-lint validate-service  examples/service/hello-world/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/hello-world/scenario/create/main.yml
```

Оба должны дать exit 0 и `OK: <path>`. Что именно проверяет линтер (regex имени, JSON Schema на корне, соответствие `state_schema_version` ↔ `migrations/`, запрещённые ключи) — [docs/service/manifest.md → `soul-lint validate-service`](../service/manifest.md#валидация-soul-lint-validate-service) и [docs/soul-lint.md](../soul-lint.md).

## 7. Регистрируем сервис

Чтобы Keeper мог резолвить сервис, его надо положить в реестр сервисов: git-источник + ref. Версия — это `ref` (tag или branch), не отдельное поле ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). В проде это `POST /v1/services`:

```sh
curl -s -X POST http://127.0.0.1:8080/v1/services \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "hello-world", "git": "https://git.internal/svc/hello-world.git", "ref": "main"}'
```

> **Dev-shortcut.** На локальном стенде держать git-репо неудобно. `make dev-provision` уже материализует `hello-world` как локальный `file://`-репо и засевает реестр сервисов — отдельный `POST /v1/services` тогда не нужен. Это dev-only: `file://`-резолв включается флагом `SOUL_STACK_ALLOW_FILE_REPOS=1`, который проставляет `make dev-keeper`; в проде источник — настоящий git-URL. Подробнее — [getting-started.md → Шаг 7](../getting-started.md#шаг-7-apply-применить-сценарий-hello-world).

Когда правишь свой сервис: закоммить изменения, выставь нужный `ref` (двигай branch или ставь новый tag) — Keeper подтянет именно этот ref при следующем резолве.

## 8. Создаём инкарнацию

Инкарнация — runtime-инстанс сервиса (живёт в Postgres: `spec` / `state` / `status`). Создание инкарнации запускает сценарий `create` на хостах таргета. Привязываем к coven `demo`, где наш Soul:

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hello-demo",
    "service": "hello-world",
    "covens": ["demo"],
    "input": { "greeting": "hello from my first service" }
  }'
```

Ответ `202 Accepted` с `apply_id` — операция асинхронная. Контракт эндпоинта — [keeper/operator-api/incarnations.md](../keeper/operator-api/incarnations.md).

`input.greeting` здесь — это тот самый параметр из `scenario/create/main.yml`; Keeper валидирует его против `input:`-контракта сценария до запуска.

## 9. Apply и проверка

**Статус инкарнации** (`applying` → `ready` при успехе, `error_locked` при провале хотя бы на одном хосте):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo -H "Authorization: Bearer $TOKEN"
```

То же через CLI (`soulctl` — тонкая обёртка над Operator API):

```sh
soulctl incarnation get hello-demo
```

**Результат на хосте** — при `status: ready`:

```sh
cat /tmp/soul-stack-hello        # → hello from my first service
```

**State и история.** В `incarnation.state.greeting_file` — путь к созданному файлу (то, что записал `state_changes.sets`). История прогонов (snapshots в `state_history`):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo/history -H "Authorization: Bearer $TOKEN"
# или
soulctl incarnation history hello-demo
```

### Запустить сценарий повторно

Создание инкарнации запускает `create` один раз. Чтобы прогнать сценарий на **существующей** инкарнации (тот же `create` или другую операцию сервиса), есть `soulctl incarnation run`:

```sh
soulctl incarnation run hello-demo create --input '{"greeting":"hi again"}' --wait
```

`--wait` поллит статус до завершения apply. Сводка способов запуска работы (scenario / батч через Voyage / single-Errand / push) — [keeper/run-flavors.md](../keeper/run-flavors.md).

## 10. Что дальше

Ты собрал односценарный сервис. Дальше — по мере роста:

- **Больше операций.** Добавь сценарии `scenario/<op>/main.yml` (`add_user`, `restart`, …) — каждый со своим `input:` и `state_changes:`. Полная DSL-грамматика задач (loop / block / register / onchanges / retry / …) — [docs/destiny/tasks.md](../destiny/tasks.md); оркестрационная дельта scenario (`on:` / `where:` / `serial:` / `apply:`) — [docs/scenario/orchestration.md](../scenario/orchestration.md).
- **Файлы из шаблонов.** Когда контент сложнее одной строки — Go text/template в `scenario/<name>/templates/<path>.tmpl` + модуль `core.file.rendered`. Спека шаблонизатора — [docs/templating.md](../templating.md).
- **Структурный state и миграции.** Когда state перерастает один-два поля и меняется несовместимо — подними `state_schema_version` и добавь `migrations/<NNN>_to_<MMM>.yml`. Формат миграций (плоский DSL + CEL + `foreach`, forward-only) — [docs/migrations.md](../migrations.md).
- **Зависимости.** Переиспользуемые пакеты задач — выноси в отдельные destiny и подключай через `destiny[]` в `service.yml` + `apply:` в сценарии. Custom-модули — через `modules[]`. Формат — [docs/service/manifest.md](../service/manifest.md).
- **Факты о хосте.** Таргетинг и значения по фактам системы — `soulprint.self.*` (OS-family, pkg_mgr, IP, …). Схема — [docs/soul/soulprint.md](../soul/soulprint.md).
- **Day-2 операции** (мониторинг, апгрейд, восстановление кластера) — раздел [Сделать](../README.md#сделать-оператор) в карте документации и [docs/operations/](../operations/README.md).
- **Готовые образцы** сервисов посложнее — [`examples/service/`](../../examples/service/) (например, [`redis/`](../../examples/service/redis/) — один сервис на все режимы развёртывания standalone/sentinel/cluster/sentinel_only с day-2 rolling-restart и reshard).
