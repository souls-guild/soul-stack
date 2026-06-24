# ADR-019. State_schema migration DSL

- **Контекст.** [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) упоминает «плоский DSL: `rename`/`set`/`delete`/`move`» как формат `migrations/<NNN>_to_<MMM>.yml`. Но прежний пример migration-файла redis-сервиса использовал `{% for %}` (Jinja2-стиль из эпохи до ADR-010) — что **не помещается в плоский DSL**. Этот пример был сознательно НЕ тронут во время массовой миграции под ADR-010 (помечен «out of scope, open Q №18»). Реальные сценарии state_schema-миграций включают преобразование коллекций, вычисления от старых полей, splits/merges структур — плоского DSL не хватает.
- **Решение.**

  **(a) Грамматика DSL — плоский + CEL-выражения + структурный `foreach` (MVP).**

  Операции (закрытый список в MVP): **`rename`** (move без переименования места), **`set`** (запись значения; `value:` может быть YAML-литералом или CEL-выражением через `${ … }`), **`delete`** (удаление по `path:`), **`move`** (алиас `rename`), **`foreach`** (структурный цикл: `in: <CEL-list/map>`, `as: <var>`, `do: [<operation>, ...]`).

  Условный `if:`-ключ — **не в MVP** (рекомендуемый таргет (c) по разведке). Расширение до (c) — без breaking change через добавление optional ключа.

  Полная грамматика, конвенция тестов, примеры — в [`docs/migrations.md`](../migrations.md).

  **(b) CEL — единый движок выражений (как и весь Soul Stack по [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).** В migration-CEL-контексте:
  - **Доступно:** `state.*` (текущая мутируемая версия), `<as-name>` внутри `foreach.do[*]`, стандартные CEL-функции (`int`/`string`/`size`/`has`/`keys`/`values`, comprehensions `map`/`filter`/`all`/`exists`).
  - **Запрещено:** `vault(...)` (не тянуть секреты), `now()` (воспроизводимость тестов), `register.*` / `soulprint.*` / `essence.*` / `input.*` (миграция — чистая функция от старого state, без хост-контекста и без оператор-параметров), user-defined CEL-функции.

  Это закрывает поверхность исполнения миграции: пасс side-effect-free function `state→state` в sandbox CEL.

  **(c) Атомарность — одна PG-транзакция на всю цепочку миграций.** При `keeper.incarnation.upgrade name=X to_version=v3.0` (с `state_schema_version: 1` → `3`) keeper:
  1. `BEGIN`.
  2. `SELECT state, state_schema_version FROM incarnation WHERE name = ? FOR UPDATE`.
  3. Применить `001_to_002` → `002_to_003` последовательно in-memory (in-Go).
  4. На каждом шаге `INSERT INTO state_history` с `scenario: "migration"`, `state_before` / `state_after`, `changed_by_aid`.
  5. `UPDATE incarnation SET state, state_schema_version, service_version`.
  6. `COMMIT`.
  
  При фейле — `ROLLBACK`, `incarnation.status: migration_failed` ([§«Versioning и миграции state_schema»](../architecture.md#versioning-и-миграции-state_schema)).

  **(d) Reverse — только forward в MVP.** `down:`-блок не поддерживается. Восстановление при инциденте — через `state_history` snapshot. Расширение до optional `down:` post-MVP — без breaking change (новый optional ключ верхнего уровня файла).

  **(e) Escape-модуль (`state.migrate` / `core.incarnation.state-migrate`) — не вводится в MVP.** Старая ссылка в [§«Versioning и миграции state_schema»](../architecture.md#versioning-и-миграции-state_schema) на «destiny-модуль `state.migrate`» — отвергается: имя вне словаря (не в [naming-rules.md](../naming-rules.md)), и реальные сложные случаи (which согласно разведке составляют <10%) покрываются грамматикой (a). Если когда-нибудь понадобится — отдельный ADR с propose-and-wait по имени (`core.incarnation.state-migrate` — кандидат по образцу `core.soul.registered`).

  **(f) Тесты миграций — в `migrations/<NNN_to_MMM>/tests/<case>.yml`.** Формат: `state_before` → migration → assert `state_after`. Симметрия с конвенцией destiny/scenario (тесты рядом с тестируемым артефактом). Полный формат — в [`docs/migrations.md`](../migrations.md).

  **(g) Связь с ADR-009 и ADR-010.** Этот ADR — явное **расширение** «плоского DSL» из ADR-009 на грамматику (a). ADR-009 в части migration-DSL отсылает сюда. Использование CEL — соответствие ADR-010 (один движок выражений на весь Soul Stack).

- **Consequences.**
  - `docs/migrations.md` — новый файл (нормативная спецификация формата).
  - `docs/architecture.md` § «Versioning и миграции state_schema» — обновляется ссылкой на ADR-019 (старое описание «плоский DSL» → «по ADR-019»).
  - Пример migration-файла redis-сервиса переписывается под грамматику (a) (вместо `{% for %}`-Jinja — структурный `foreach`). Реализован в [`examples/service/redis/migrations/001_to_002.yml`](../../examples/service/redis/migrations/001_to_002.yml) после redis-консолидации (`redis_users` из списка имён в map `name → {perms, state}`).
  - Open Q №18 закрыт.
  - Соул-side изоляция: миграция = keeper-side, никаких изменений в `proto/keeper/v1/`.
- **Trade-offs.**
  - Грамматика чуть шире плоского DSL — нужно специфицировать `foreach` (один новый ключ). Это компенсируется симметрией с essence-pipeline (`foreach: + as: + when:` уже зафиксированы) — оператор узнаёт паттерн.
  - `if`-ключ отложен — условные миграции записей делаются через `foreach + filter` в CEL (`in: ${ state.users.filter(u, u.flag) }`). Менее очевидно, чем явный `if`-в-DSL, но покрывает кейсы. Расширение до (c) — при первом запросе.
  - Forward-only — оператор не может откатить миграцию декларативно. Принимаем: восстановление через `state_history` — рабочий путь, mandatory `down:` — overkill для редкой операции.
