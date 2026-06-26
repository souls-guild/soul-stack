# State_schema migration DSL

Нормативная спецификация формата `migrations/<NNN>_to_<MMM>.yml` в service-репозитории. Источник правды по решению — [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl). Этот документ — грамматика, разрешённые CEL-функции в migration-контексте, конвенция тестов, примеры.

## Назначение

State_schema-миграция преобразует `incarnation.state` (jsonb в Postgres) с версии N на N+1 при upgrade сервиса (`keeper.incarnation.upgrade name=X to_version=v...`). Миграция — это **чистая функция `state_v<N> → state_v<M>`** без хостовых side-effects, выполняется на keeper-стороне в одной PG-транзакции (см. [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) разделы (c) атомарность и (e) безопасность).

## Раскладка файла

`<service-repo>/migrations/<NNN>_to_<MMM>.yml` — один файл = один шаг миграции. Цепочка `001 → 002 → 003 → ...` прогоняется keeper-ом последовательно при upgrade.

```
redis/
├── service.yml                            # state_schema_version: 2
├── migrations/
│   ├── 001_to_002.yml                     # описанный ниже формат
│   └── 001_to_002/                        # тесты этой миграции
│       └── tests/
│           ├── users-list-to-map.yml
│           ├── single-user.yml
│           ├── empty-users.yml
│           └── preserves-unrelated-fields.yml
└── ...
```

Имя файла: `NNN_to_MMM.yml` — три цифры с ведущими нулями, разделитель `_to_`.

## Структура файла

```yaml
from_version: 1
to_version: 2

description: >
  Переход с массива redis_users[] на map redis_users{name: {acl, state}}
  для поддержки per-user ACL и enabled/disabled-флага.

# Список операций. Применяются в порядке. Каждая операция видит state,
# мутированный предыдущими операциями той же миграции.
transform:
  # Атомарные операции:
  - rename: { from: state.redis_users, to: state.redis_users_legacy_v1 }

  # CEL-выражения в значениях set:
  - set:
      path: state.maxmemory_bytes
      value: "${ int(state.maxmemory_mb) * 1048576 }"

  - delete: { path: state.maxmemory_mb }

  # Итерация по коллекции через структурный foreach.
  - foreach: "${ state.redis_users_legacy_v1 }"
    as: user_name
    do:
      - set:
          path: "state.redis_users.${ user_name }"
          value:
            acl: "off ~* &* +@all"
            state: "off"

  - delete: { path: state.redis_users_legacy_v1 }
```

## Операции `transform:`

| Операция | Параметры | Семантика |
|---|---|---|
| **`rename`** | `from: <path>`, `to: <path>` | Переместить значение из `from` в `to`. Если `to` уже существует — ошибка (явный `delete` перед rename). |
| **`set`** | `path: <path>`, `value: <yaml>` либо `<CEL-выражение>` | Записать `value` в `path`. Если ключ существует — перезаписывается. `value` может быть литералом YAML (map/list/scalar) либо CEL-выражением через `${ … }` либо вложенной структурой со встроенными `${ … }`-интерполяциями. |
| **`delete`** | `path: <path>` | Удалить значение по `path`. Если не существует — no-op (не ошибка). |
| **`move`** | `from: <path>`, `to: <path>` | Алиас для `rename` (исторический; одинаковая семантика). |
| **`foreach`** | `in: <CEL-выражение>` (либо краткая форма `foreach: <CEL-выражение>`), `as: <var-name>`, `do: [<operation>, ...]` | Структурный цикл: итерация по списку/значениям map-а, на каждом шаге `<var-name>` биндится к текущему элементу. `do:` — вложенный transform-список. Внутри `do:` доступны `<var-name>` и весь текущий `state.*`. |

**Список операций сейчас закрыт** (`rename`/`set`/`delete`/`move`/`foreach`). Условный `if:`-ключ — на post-MVP (см. [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl), вариант (c) target).

## Адресация — `path:`

Точечная нотация от корня state-объекта: `state.foo`, `state.bar.baz`, `state.users.${ name }.acl`.

- Префикс `state.` обязателен (явное указание области).
- Сегменты пути — буквы/цифры/`_`/`-` или `${ <CEL> }`-интерполяция.
- Доступ к элементу массива по индексу: `state.hosts.0.ip` (в MVP не используется в примерах — добавится при необходимости).

## CEL в migration-контексте

Любое значение в `set.value`, `foreach.in`, `path:` поддерживает CEL-выражения через маркер `${ … }` ([ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).

### Доступные переменные

| Имя | Тип | Семантика |
|---|---|---|
| `state` | object | Текущий state (мутируемый по ходу операций). Корневое значение `incarnation.state`. |

Внутри `foreach.do[*]` дополнительно:

| Имя | Тип | Семантика |
|---|---|---|
| `<as-name>` | dyn | Текущий элемент итерации (значение, если `in` — map; элемент списка, если `in` — list). |

### Доступные CEL-функции

Стандартные CEL-функции (`int`, `string`, `bool`, `size`, `has`, comprehensions `map`/`filter`/`all`/`exists`/`exists_one`) + операторы (`+`/`-`/`*`/`/`/`==`/`!=`/`<`/`>`/`<=`/`>=`/`&&`/`||`/`!`/`in`/`?:`).

migration-CEL — sandbox с минимальной surface area: только stdlib (объявлена лишь переменная `state`). `glob()`/`merge()`/`default()` и любые pure-расширения обычного CEL здесь **не** зарегистрированы (расширение требует отдельного ADR). `keys()`/`values()` в этом списке **нет** — их в migration-CEL нет, `${ keys(...) }` падает на компиляции (`undeclared reference`).

Для итерации по map используется нативный макрос `.map()` **над самим map-ом**: он обходит **ключи** (элемент итерации = ключ), значение достаётся индексом `m[k]`. Так свёртывается `map → array` без `keys()` — см. миграцию [`examples/service/redis/migrations/005_to_006.yml`](../examples/service/redis/migrations/005_to_006.yml) (`state.redis_users.map(n, {'name': n, 'perms': state.redis_users[n].perms, ...})`).

### Запрещено в migration-CEL

| Имя | Почему |
|---|---|
| `vault(...)` | Миграция не должна тянуть секреты. |
| `now()` | Для воспроизводимости тестов. |
| `register.*` | Нет хост-контекста (миграция — keeper-side). |
| `soulprint.*` | Аналогично. |
| `essence.*` | Миграция должна быть чистой функцией от старого state, не зависеть от текущего essence. |
| `input.*` | Миграция не принимает оператор-параметров (только `state`). |
| Любые user-defined CEL-функции | Sandbox by design. |

## Reverse / downgrade

В MVP — **только forward**. Восстановление при инциденте — через `state_history` snapshot (см. [`docs/architecture.md → state_history`](architecture.md#state_history--журнал-изменений-state)).

Optional `down:` блок в migration-файле может быть добавлен post-MVP без breaking change. Текущая грамматика этот блок не поддерживает.

## Атомарность

Цепочка миграций `<from_version>` → `<to_version>` keeper выполняет в **одной PG-транзакции**:

1. `BEGIN`.
2. `SELECT state, state_schema_version FROM incarnation WHERE name = ? FOR UPDATE`.
3. Применить миграции последовательно в памяти (Go): `state_v1 → state_v2 → state_v3 → ...`.
4. На каждом шаге `INSERT INTO state_history (state_before, state_after, scenario, changed_by_aid, ...)` с `scenario: "migration"`.
5. `UPDATE incarnation SET state = ?, state_schema_version = ?, service_version = ?`.
6. `COMMIT`.

При фейле любого шага — `ROLLBACK`, incarnation помечается `status: migration_failed` ([architecture.md → §«Versioning и миграции state_schema»](architecture.md#versioning-и-миграции-state_schema)).

## Тестирование

Тесты миграции живут в `migrations/<NNN_to_MMM>/tests/<case>.yml`. Формат:

```yaml
name: redis-users-array-to-map
description: >
  Базовый случай: массив имён переходит в map с per-user ACL.

state_before:
  redis_users: ["app", "monitor"]
  maxmemory_mb: 512

state_after:
  redis_users:
    app:     { acl: "off ~* &* +@all", state: "off" }
    monitor: { acl: "off ~* &* +@all", state: "off" }
  maxmemory_bytes: 536870912
```

Тест:
1. Загружает `state_before` как `state`.
2. Применяет операции миграции.
3. Сверяет получившийся `state` с `state_after` (deep-equal).

Запускается через `soul-trial <service-repo>/migrations/<NNN_to_MMM>/` ([ADR-023](adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage): «исполняет → soul-trial», в отличие от чисто статического `soul-lint`). Механика раннера — отдельная задача после spec'и.

## Связанные документы

- [ADR-019 в `docs/architecture.md`](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) — фиксация решения.
- [ADR-009 в `docs/architecture.md`](adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — старое упоминание плоского DSL (теперь заменено ADR-019).
- [ADR-010 в `docs/architecture.md`](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) — CEL как единый движок выражений.
- [`docs/architecture.md` → §«Versioning и миграции state_schema»](architecture.md#versioning-и-миграции-state_schema) — высокоуровневое описание (`state_schema_version`, upgrade-механизм, atomicity).
- [`docs/architecture.md` → §«`state_history`»](architecture.md#state_history--журнал-изменений-state) — журнал, через который доступно восстановление при инциденте.
- [`docs/templating.md`](templating.md) — CEL общая спека.
- [`examples/service/redis/migrations/`](../examples/service/redis/migrations/) — пример (миграция `001_to_002`: `redis_users` из списка имён в map `name → {perms, state}` через `foreach`).
