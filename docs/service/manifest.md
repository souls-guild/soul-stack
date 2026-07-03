# Раскладка папки сервиса и формат `service.yml`

Этот документ описывает раскладку git-репозитория сервиса и поля корневого манифеста `service.yml`. Соседние темы вынесены в отдельные нормативные документы:

- Концепция Service (что это в общей картине, граница с destiny / scenario) — [`docs/destiny/concept.md`](../destiny/concept.md) и [`docs/architecture.md → Service`](../architecture.md#service--структура-и-manifest).
- Формат scenario — [`docs/scenario/`](../scenario/README.md).
- Стандарт `input:`-блока для scenario — [`docs/input.md`](../input.md).
- Миграции state_schema — [`docs/migrations.md`](../migrations.md), [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl).

## Что такое сервис

Сервис — это **тип сервиса** (Redis HA, PostgreSQL, Vector-collector). Один сервис — один git-репозиторий со своим manifest, набором операций (сценариев), параметрами по умолчанию (essence), миграциями state и тестами.

Версия сервиса как артефакта — это git tag, под которым закоммичен манифест ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). Поле `version:` верхнего уровня в `service.yml` намеренно отсутствует.

## Раскладка репозитория

```
service-<name>/
├── service.yml                         # манифест (этот документ)
├── essence/                            # параметры в иерархии — см. architecture.md → Essence
│   ├── _default.yaml                   # baseline для всех incarnation
│   ├── _stack.yaml                     # ОПЦ.: декларативный pipeline сборки
│   ├── coven/                          # ОПЦ.: параметры по Coven-меткам
│   │   ├── prod.yaml
│   │   └── dev.yaml
│   └── os/                             # ОПЦ.: параметры по soulprint.os.family
│       ├── ubuntu.yaml
│       └── debian.yaml
├── scenario/                           # операции; auto-discover из директории
│   ├── create/
│   │   ├── main.yml                    # точка входа: input + state_changes + tasks
│   │   ├── install.yml                 # ОПЦ.: include-соседи main.yml
│   │   ├── vars.yml                    # ОПЦ.: scenario-локалы
│   │   ├── templates/                  # ОПЦ.: scenario-локальные шаблоны
│   │   └── tests/                      # ОПЦ.: тесты сценария
│   ├── add_user/
│   │   └── main.yml
│   ├── update_acl/
│   │   └── main.yml
│   └── restart/
│       └── main.yml
├── types.yml                           # ОПЦ.: переиспользуемые именованные input-типы (секция types:, $type-ссылка — ADR-062)
├── migrations/                         # ОПЦ.: state_schema миграции (если state_schema_version > 1)
│   ├── 001_to_002.yml
│   ├── 001_to_002/
│   │   └── tests/                      # тесты миграции (state_before → state_after)
│   └── 002_to_003.yml
└── tests/                              # ОПЦ.: service-level тесты (smoke, chaos)
    ├── smoke.yml
    └── chaos.yml
```

Обязательны только `service.yml` и хотя бы один сценарий (`scenario/<name>/main.yml`). Всё остальное появляется по мере необходимости.

Сценарии перечислять в `service.yml` не нужно — keeper находит их auto-discover-ом по каталогу `scenario/`.

## `service.yml` — манифест

Корневой файл содержит только метаданные сервиса и контракт на структуру runtime-state. Сценарии и destiny-задачи живут в соседних файлах. Это сделано сознательно: `service.yml` остаётся коротким и читается за один взгляд при ревью.

### Поля

| Поле | Обяз. | Тип | Смысл |
|---|---|---|---|
| `name` | да | string (kebab-case) | Имя типа сервиса (`redis`, `postgres-ha`). Совпадает с именем папки сервиса (в `examples/service/<name>/` — голое имя без приставки `service-`). Regex `^[a-z][a-z0-9-]*$`. |
| `description` | рекомендуется | string | Одна-две фразы: что это за сервис. Видно в UI Keeper-а, MCP-каталоге, выводе `soul-lint`. |
| `state_schema_version` | да | integer (≥1) | Версия структуры `incarnation.state` в Postgres. **НЕ** версия сервиса (это git tag по [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). Инкрементируется явно при breaking-изменениях schema; требует соответствующей миграции в `migrations/`. |
| `state_schema` | да | JSON Schema object | Структура `incarnation.state` JSONB-поля в Postgres. Формат — JSON Schema (`type: object` на корне), draft-07-совместимый. См. [«Формат `state_schema`»](#формат-state_schema) ниже. |
| `destiny` | да (если есть зависимости) | array<{name, ref, git?}> | Список destiny-зависимостей. Каждая запись: `{ name: <kebab-case>, ref: <git-tag-или-branch> }` + опц. `git: <полный-URL>` (override источника, см. ниже). Core-модули **не перечисляются** — они всегда доступны ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). |
| `modules` | да (если есть зависимости) | array<{name, ref}> | Список custom-модулей `{ name: <namespace>.<module>, ref: <git-tag-или-branch> }`. Core-модули **не перечисляются** ([ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)). Из записей Keeper **авто-синтезирует** install-шаги `core.module.installed` в план прогона — см. [ниже](#modules--источник-авто-синтеза-install-шагов-adr-065). |

### Что в `service.yml` НЕ лежит

- **`version`** — версия сервиса = git tag, под которым закоммичен файл ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). Любое появление `version:` в `service.yml` — ошибка валидации с hint про ADR-007.
- **`tasks:` / `steps:`** — это destiny/scenario-уровень. В `service.yml` задачи не перечисляются.
- **`input:`** — это scenario-уровень (`scenario/<name>/main.yml`). В `service.yml` `input:` нет.
- **`scenarios[]`** — сценарии auto-discover-ятся по каталогу. Перечислять их в манифесте не нужно.

### Формат `state_schema`

`state_schema` — это JSON Schema-документ, описывающий ожидаемую структуру JSONB-поля `incarnation.state` в Postgres. На корне всегда `type: object` с описанием полей.

Поддерживаются стандартные JSON Schema draft-07-конструкции:

- `type` — `object` / `array` / `string` / `integer` / `number` / `boolean`.
- `required` — список обязательных ключей.
- `properties` — карта поля → схема.
- `additionalProperties` — bool или вложенная схема.
- `enum`, `pattern`, `min`/`max`, `items` для массивов.

Keeper валидирует `incarnation.state` против `state_schema` при создании incarnation и при upgrade на новую версию schema через миграцию (см. [`docs/migrations.md`](../migrations.md)).

### Формат `destiny[]` и `modules[]`

Каждая запись — объект:

| Поле | Обяз. | Тип | Смысл |
|---|---|---|---|
| `name` | да | string | Имя destiny (для `destiny:`) или `<namespace>.<module>` (для `modules:`). |
| `ref` | да | string | Git ref — tag (`v2.0.0`) или branch (`main`). Никаких semver-range — точный ref ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). |
| `git` | нет | string (полный git-URL) | **Только `destiny[]`.** Per-entry override источника. При заданном `git:` Keeper грузит destiny напрямую по этому URL, игнорируя `default_destiny_source` (keeper.yml). В `modules[]` поле запрещено — парсер отвергает с `unknown_key`. |

Формат `name`:
- `destiny[].name` — kebab-case одноуровневое имя destiny, regex `^[a-z][a-z0-9-]*$`.
- `modules[].name` — strict двухуровневая форма `<namespace>.<module>`, regex `^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$`. Симметрично `destiny.yml → required_modules[]` (см. [`docs/destiny/manifest.md`](../destiny/manifest.md)). Core-модули в `modules:` не перечисляются.

**Гибрид источника destiny** (как Keeper выводит git-URL для зависимости):
- запись **без** `git:` → стандартный путь: git-URL = `default_destiny_source` (keeper.yml) с подстановкой `{name}`;
- запись **с** `git:` → override: git-URL берётся из `git:` напрямую, шаблон не используется.

`ref` берётся из записи в обоих случаях. Полный алгоритм резолва — [`docs/keeper/config.md → default_destiny_source`](../keeper/config.md#services--default_destiny_source--default_module_source).

Прочее расширение полей (`enabled`, `optional` и т.п.) — отдельный propose-and-wait.

### `modules[]` — источник авто-синтеза install-шагов (ADR-065)

`modules[]` — не только декларация зависимостей для валидации и UI. Keeper из каждой записи **синтезирует** Soul-side шаг `core.module.installed` с `params: {name, ref}` и вставляет его в план прогона непосредственно перед первой задачей-потребителем модуля (задача `module: <ns>.<module>.<state>`; потребитель внутри `block:` → вставка перед block-ом целиком). Зависимость декларируется один раз на сервис — install-boilerplate в каждом сценарии не нужен ([ADR-065 amendment 2026-07-03](../adr/0065-core-module-installed.md)).

- **Модуль без задач-потребителей в сценарии не синтезируется.**
- **Takeover:** явный шаг `core.module.installed` с тем же литеральным `params.name` отключает синтез этого имени — оператор сам управляет позицией, `ref` и `when:`.
- `ref` записи уходит в params синтез-шага как **pin-сверка**: активный Sigil-допуск обязан быть на этом ref.
- **Ограничение MVP:** потребители определяются по `module:`-задачам сценария; модуль, используемый только внутри destiny (через `apply:`), потребителем не считается — ему нужен явный install-шаг.

Полная механика (точки синтеза, позиция, имя-маркер, идемпотентность) — [`docs/keeper/modules.md → Авто-синтез`](../keeper/modules.md#авто-синтез-coremoduleinstalled-из-serviceymlmodules).

### Пример

```yaml
name: redis
state_schema_version: 2
description: Redis по концепции Ansible-роли (standalone/sentinel/cluster/sentinel_only)

# Структура incarnation.state в БД
state_schema:
  type: object
  required: [redis_type, redis_config]
  properties:
    redis_type: { type: string, enum: [standalone, sentinel, cluster, sentinel_only] }
    redis_version: { type: string }
    redis_config:
      type: object
      additionalProperties: true
    redis_users:                      # map username → {perms, state}
      type: object
      additionalProperties:
        type: object
        required: [perms, state]
        properties:
          perms: { type: string }
          state: { type: string, enum: [on, off] }
    redis_hosts:
      type: array
      items:
        type: object
        properties:
          sid:  { type: string }
          role: { type: string, enum: [primary, replica, sentinel] }

# Артефакты-зависимости — ref: git tag/branch (см. ADR-007)
destiny:
  - { name: redis, ref: v1.0.0 }      # режим-агностичный кирпич: install + render redis.conf

# Custom-модули, нужные сценариям (двухуровневая форма <namespace>.<module>).
# Keeper синтезирует из записи install-шаг core.module.installed перед первым
# потребителем в плане прогона (ADR-065) — явный шаг в сценарии не нужен.
modules:
  - { name: community.redis, ref: v1.0.0 }  # живой Redis-рантайм (CONFIG SET, ACL, cluster, sentinel)
```

Рабочий пример с полной раскладкой папки — [`examples/service/redis/`](../../examples/service/redis/).

## Сценарии

Каждая папка `scenario/<name>/` — отдельная операция над сервисом (CRUD-style: `create`/`add_user`/`restart`/...). `main.yml` — точка входа сценария: содержит inline `input:` (контракт входов по [`docs/input.md`](../input.md)), `state_changes:` (какие поля `incarnation.state` сценарий обновит при успехе), `tasks:` (шаги).

Полная нормативная спека scenario-DSL — [`docs/scenario/`](../scenario/README.md).

### Стартовый сценарий — `create: true`

Стартовый (bootstrap) сценарий — тот, которым оператор создаёт новую incarnation (`POST /v1/incarnations`, поле `create_scenario`). Сценарий объявляет эту способность top-level ключом `create: true` в своём `scenario/<name>/main.yml`:

```yaml
# scenario/create/main.yml
name: create
create: true          # сценарий годен как стартовый (bootstrap новой incarnation)
input:
  # ...
state_changes:
  # ...
tasks:
  # ...
```

Правила:

- **Декларация — в сценарии, не в манифесте.** Стартовый набор сервиса keeper выводит **auto-discover-ом**: сканирует `scenario/`, в набор попадают **ровно** сценарии с `create: true`. В `service.yml` стартовые сценарии не перечисляются (как и любые другие — keeper находит их по каталогу `scenario/`, см. [«Раскладка репозитория»](#раскладка-репозитория)).
- **Имя `create` НЕ привилегировано.** Сценарий с именем `create` попадает в набор, только если сам несёт `create: true` — ровно как любой другой. Магического дефолтного `create` больше нет.
- **Несколько create-сценариев — норма.** Сервис может предлагать несколько стартовых путей (например redis: `create` — с нуля, `create_from_souls` — на готовых хостах, `migrate_cluster` — с заливкой данных из внешнего источника). Оператор выбирает один полем `create_scenario`; если у сервиса ≥1 create-сценарий, выбор **обязателен** (пустой → `422`, input валидируется против схемы конкретного сценария).
- **Сервис без `create: true`-сценариев → bare-инкарнация.** Если ни один сценарий не несёт `create: true`, `POST /v1/incarnations` создаёт **bare-инкарнацию**: запись в `ready` без прогона и без `apply_id` (`incarnation.created_scenario` = `null`). Дальше работа — через day-2-операции (`POST /v1/incarnations/{name}/scenarios/{scenario}`). Такой сервис состоит только из day-2-сценариев и не умеет «поднимать себя с нуля» одним вызовом — это валидный паттерн.

Семантика выбора, три ветви контракта и bare-инкарнация со стороны API — [`docs/keeper/operator-api/incarnations.md → Выбор стартового сценария и bare-инкарнация`](../keeper/operator-api/incarnations.md#выбор-стартового-сценария-и-bare-инкарнация).

### Когда нужны соседи `main.yml`

Один `main.yml` справляется, пока сценарий остаётся обозримым (~150 строк). Если внутри явно выделяются логические подразделы — выносим их в `scenario/<name>/<sub>.yml` и подключаем через `include:`. Аналогично [`docs/destiny/manifest.md → Когда нужны соседи tasks/main.yml`](../destiny/manifest.md#когда-нужны-соседи-tasksmainyml).

## Переиспользуемые именованные input-типы — `types.yml`

Опциональный service-level файл `types.yml` объявляет **переиспользуемые именованные input-схемы** — секция `types:` с map `<PascalCase>` → схема в том же input-DSL ([`docs/input.md`](../input.md)). Сценарий ссылается на тип директивой `$type: <Имя>` (как самостоятельное поле или `items: {$type: <Имя>}` для массива) — так составной тип (например запись пользователя `{name, perms, state}`) не дублируется inline в каждом сценарии. Резолв — service-level (только этот сервис), с обязательным cycle-detection. Полный формат, резолв, классы ошибок и границы MVP — [`docs/input.md → «Переиспользуемые именованные типы»`](../input.md#переиспользуемые-именованные-типы-types--type), [ADR-062](../adr/0062-input-types.md). Прежний нереализованный задел `$ref` на внешний JSON-Schema-файл в `schemas/` им заменён.

## Essence

Иерархическая сборка параметров (Salt-pillar + PillarStack аналог). `essence/_default.yaml` — baseline для всех incarnation; опциональные подкаталоги `coven/<label>.yaml` и `os/<family>.yaml` добавляют overlay'и по Coven-меткам и `soulprint.os.family`. Опциональный `_stack.yaml` — декларативный pipeline сборки (сложные условия и итерации).

Полная нормативная спека — [`docs/architecture.md → Essence: pipeline сборки`](../architecture.md#essence-pipeline-сборки). Essence — role-agnostic ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)): ступени `role/<Y>.yaml` в pipeline НЕТ.

## Миграции state_schema

Если `state_schema_version > 1`, в репозитории должен быть каталог `migrations/` с цепочкой миграций. Формат — плоский DSL + CEL-выражения + `foreach` ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

Полная нормативная спека — [`docs/migrations.md`](../migrations.md).

Соглашения:

- `migrations/<NNN>_to_<MMM>.yml` — миграция с версии NNN на MMM.
- `migrations/<NNN>_to_<MMM>/tests/<case>.yml` — тесты миграции (state_before → migration → assert state_after).
- Цепочка должна быть полной: миграции `1→2`, `2→3`, …, `(N-1)→N` — без пропусков.
- Forward-only в MVP (`down:` не поддерживается, восстановление через `state_history`).

Миграция запускается явной операцией оператора (`keeper.incarnation.upgrade to_version=N`), не автоматически при apply сценария.

## Валидация `soul-lint validate-service`

`soul-lint validate-service <path>` — статическая проверка корневого `service.yml` и связанных файлов. В MVP проверяется:

- **`service.yml` манифест:**
  - `name` regex `^[a-z][a-z0-9-]*$`, непустой.
  - `description` — string (если есть).
  - `state_schema_version` — integer ≥1.
  - `state_schema` — валидный JSON Schema; `type: object` на корне.
  - `destiny[]` / `modules[]` — каждая запись имеет `name` + `ref`, оба непустые. `name` matches kebab-case; для `modules:` — двухуровневая форма `<namespace>.<module>`. Опц. `destiny[].git` — override источника; в `modules[]` поле `git:` отвергается (`unknown_key`).
  - Unknown top-level keys → `unknown_key` с hint про deprecated (`version` → ADR-007; `tasks`/`steps`/`scenarios` → auto-discover/destiny-level; `input` → scenario-level).

- **Соответствие `state_schema_version` и `migrations/`:**
  - Если `state_schema_version == 1` — каталог `migrations/` не обязателен.
  - Если `state_schema_version > 1` — должна быть полная цепочка миграций `1→2`, …, `(N-1)→N` без пропусков.
  - Каждый файл `migrations/<NNN>_to_<MMM>.yml` валидируется отдельно (формат миграции — [`docs/migrations.md`](../migrations.md)).

Расширенные проверки (cross-file refs: каждый `apply: destiny: <name>` в сценариях ссылается на запись в `service.yml → destiny:`; каждый `module: <ns>.<mod>.<state>` существует в `modules:` или в core-модулях; и т.д.) — отложены в M1.5 ([`docs/soul-lint.md`](../soul-lint.md)).

## Open Q

- **Strict-валидация `incarnation.state` против `state_schema` при apply сценария** — обязательная check перед каждым apply, или только при создании incarnation/upgrade? Связано с runtime-pipeline-ом Keeper-а (post-MVP).
- **Дополнительные поля в `destiny[]` / `modules[]`** (`enabled`/`optional`/...) — `destiny[]` уже несёт опц. `git:` (override источника), прочие расширения через propose-and-wait.
- **Cross-file refs валидация** (apply: destiny ⊆ service.yml destiny) — отложена в M1.5.

## См. также

- [`docs/destiny/manifest.md`](../destiny/manifest.md) — формат `destiny.yml`.
- [`docs/destiny/concept.md`](../destiny/concept.md) — концепция destiny и Service-слой в общей картине.
- [`docs/scenario/`](../scenario/README.md) — формат scenario.
- [`docs/input.md`](../input.md) — стандарт `input:`-блока (для сценариев).
- [`docs/migrations.md`](../migrations.md) — формат миграций.
- [`docs/architecture.md → Service`](../architecture.md#service--структура-и-manifest) — архитектурный summary.
- [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — версионирование через git ref.
- [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — граница scenario/destiny.
- [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) — Migration DSL.
