# ADR-057. `state_changes` — упорядоченный список CRUD-глаголов (`set`/`add`/`modify`/`remove`)

- **Контекст.** Грамматика `state_changes` из [ADR-009 §7.1](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) определяла три ключа: `sets` (map `<поле>: <CEL>`, реализован) и `appends`/`modifies` (списки field-path). Последние два были **декларацией-плейсхолдером без источника значения** — движком не применялись (см. [orchestration.md §7.1 «`appends`/`modifies` — future»](../scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций)). Это латентный баг: сценарий с `appends: [redis_hosts]` (`add_replica`, `add_replicas`, `add_user`) проходил успешно, но `incarnation.state` **не рос** — добавленный хост/пользователь не оседал в state. `modifies` (`update_acl`) аналогично — патч коллекции не применялся. Корень в том, что `appends`/`modifies` несли только **field-path** (что трогать), без **источника значения** (что записать) и без **предиката** (какой элемент). А `sets` умеет только перезапись поля целиком — для роста коллекций и точечного патча элементов её не хватает.

  Параллельно `sets` — map, то есть **неупорядоченный**: при росте грамматики (несколько мутаций одной коллекции, зависимых по порядку) map не задаёт детерминированной последовательности применения.

- **Решение.** `state_changes` — **упорядоченный список операций** (YAML-список, не map). Каждый элемент — ровно один **CRUD-глагол** (сингуляр): `set` / `add` / `modify` / `remove`. Множественность выражается **match-предикатом** (CEL над элементом коллекции), а не флагами/ручками. Bulk fan-out — структурным `foreach` (форма буквально как migration-DSL [ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

  ### (a) Глаголы

  - **`set: <field>`** + **`value: "${CEL}"`** — перезапись поля целиком (заменяет прежний `sets`-map; одна запись `sets` ≡ один `set`-элемент списка).
  - **`add: <collection>`** — добавить элемент в коллекцию:
    - **map-коллекция:** `key: "${CEL}"` (ключ) + `value: {obj}` (значение записи).
    - **list-коллекция:** `value: {obj|scalar}` + опц. `match: "<CEL-предикат>"` (дедуп-предикат — если элемент с таким предикатом уже есть, действует `on_conflict`).
    - **`on_conflict: skip | replace | error`** (DEFAULT `skip`) — поведение при коллизии (ключ map уже занят / list-`match` уже находит элемент). `skip` даёт идемпотентное «добавить, если нет».
  - **`modify: <collection>`** + **`match: "<CEL-предикат над элементом>"`** + **`patch: { <путь-в-элементе>: "${CEL}" }`** — патч **ВСЕХ** подходящих элементов (all-by-default).
  - **`remove: <collection>`** + **`match: "<CEL-предикат>"`** — удалить **ВСЕХ** подходящих элементов.
  - **`foreach: "${CEL-список|map}"`** + **`as: <имя>`** + **`do: { <глагол...> }`** — bulk fan-out N операций из коллекции/map. Внутри `do:` доступны те же глаголы и биндинг `<имя>` к текущему элементу итерации. Форма (`foreach`/`as`/`do`) — буквально из migration-DSL ([ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl), [migrations.md](../migrations.md)); оператор узнаёт уже знакомый паттерн.

  ### (b) CEL-биндинги в `match`/`patch`/`value`

  Поверх полного sets-контекста (`input.*` / `incarnation.*` / `soulprint.self.*` / `register.*` / `vars.*` / `essence.*` — тот же CEL-env, что у `params:` задач, [ADR-010](0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)) вводятся **локальные биндинги текущего элемента коллекции**:

  - **`elem`** — текущий элемент list-коллекции (или скаляр, если коллекция — list of scalars).
  - **`key`** / **`value`** — ключ и значение текущей записи map-коллекции.

  Имя **`elem`** выбрано вместо `self` намеренно — `self` коллидирует с per-host `soulprint.self`.

  ### (c) `expect` — опц. runtime-ассерт кратности

  **`expect: one | at_most_one | any`** (DEFAULT `any`) — необязательный ассерт числа элементов, зацепленных `match` в `modify`/`remove`. Если фактическая кратность ≠ ожидаемой (`one` требует ровно один, `at_most_one` — ноль или один) — прогон уходит в `error_locked` **до коммита** state. Не обязательный ключ; по умолчанию `any` (любое число, в т.ч. ноль).

  ### (d) Предохранители широкого match

  - **soul-lint WARN** на константно-истинный предикат: `match: true`, либо отсутствие `match:` на `remove`/`modify` (которое снесло/перепатчило бы всю коллекцию).
  - **empty-match → no-op** (идемпотентно) для `modify` И `remove`: предикат не зацепил ничего — операция тихо ничего не делает, не ошибка.

  ### (e) Порядок и атомарность

  - Операции применяются **в порядке объявления**, последовательно, к **промежуточному** state (каждая видит результат предыдущих — детерминированная цепочка).
  - Вся цепочка — **одна PG-транзакция**, **один `state_history`-snapshot** (как в [ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Фейл любой операции (eval-ошибка CEL, нарушенный `expect`, `on_conflict: error`) → `error_locked`, state **не коммитится** (barrier/state-commit-инвариант §7 не ослаблен — операции применяются ПОСЛЕ cross-host барьера).

  ### (f) Тип коллекции — из state_schema

  Тип коллекции (map vs list) берётся из объявленной `state_schema` (`service.yml`). `add` в отсутствующее поле → материализовать **пустую map/list из схемы** (форма коллекции известна по схеме, даже если значения ещё нет).

  ### (g) Семантика per-RUN

  Значения берутся из `input.* / vars.* / incarnation.* / register.*` прогона. Это **per-RUN**, НЕ per-host union. Если выражение даёт разные значения per-host (`${ soulprint.self.* }` / `${ register.* }`) — действует **last-wins по сортировке SID** (как для `sets` в [ADR-009 §7.1](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация); per-host свёртка коллекций в union — НЕ вводится).

- **Форма каждого глагола на реальных сценариях.**

  **`add` (одиночный) — `add_replica`** (заменяет `appends: [redis_hosts]`):
  ```yaml
  state_changes:
    - add: redis_hosts
      value: "${ vars.new_sid }"
      match: "elem == vars.new_sid"        # дедуп: тот же SID уже в списке?
      on_conflict: skip                    # идемпотентно: повтор не дублирует
  ```

  **`add` через `foreach` — `add_replicas`** (bulk, заменяет `appends: [redis_hosts]` для пачки):
  ```yaml
  state_changes:
    - foreach: "${ input.new_replicas }"
      as: sid
      do:
        - add: redis_hosts
          value: "${ sid }"
          match: "elem == sid"
          on_conflict: skip
  ```

  **`add` в map — `add_user`** (заменяет `appends: [redis_users]`):
  ```yaml
  state_changes:
    - add: redis_users
      key: "${ input.username }"
      value:
        acl:   "${ input.acl }"
        state: "on"
      on_conflict: error                   # двойное создание пользователя — явная ошибка
  ```

  **`modify` всех подходящих — `update_acl`** (заменяет `modifies: [redis_users.*.acl, redis_users.*.state]`):
  ```yaml
  state_changes:
    - modify: redis_users
      match: "key == input.username"       # точечный патч одной записи map
      patch:
        acl:   "${ input.acl }"
        state: "${ input.state }"
  ```

  **`modify` ВСЕХ реплик** (множественность через предикат, без ручек):
  ```yaml
  state_changes:
    - modify: redis_hosts
      match: "elem.role == 'replica'"      # все реплики разом — all-by-default
      patch:
        config_version: "${ input.version }"
  ```

  **`remove` — `remove_replica`**:
  ```yaml
  state_changes:
    - remove: redis_hosts
      match: "elem == input.sid"
      expect: one                          # ассерт: ровно один такой хост был
  ```

- **Множественность — через `match`-предикат, не через ручки/флаги.** `modify`/`remove` патчат/удаляют **всех** подходящих под `match` по умолчанию (all-by-default). Сузить до одного — уточнить предикат (`key == X`, `elem.id == Y`) + опц. `expect: one`. Это сознательный отказ от пары глаголов `*_one`/`*_all` и от флага `all:` (см. отвергнутые альтернативы): один глагол + декларативный предикат покрывают оба кейса без диалекта.

- **Transit-план (breaking, dual-parse один релиз).** `state_changes` меняет форму **map → list**. Один релиз keeper парсит **обе** формы (dual-parse):
  - Новая форма — список глаголов (канон выше).
  - Старая map-форма (`sets:` / `appends:` / `modifies:`) парсится как **DEPRECATED**, soul-lint выдаёт warn. `sets`-map транслируется в эквивалентную последовательность `set`-элементов. `appends`/`modifies` были no-op-плейсхолдерами без источника — их deprecated-парс остаётся no-op (поведение не меняется, фиксируется warn-ом «перепиши на `add`/`modify`, иначе state не растёт»).
  - В следующем релизе map-форма **удаляется** (парс старой формы → ошибка валидации).

- **Связь с migration-DSL ([ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).**
  - **`foreach`/`as`/`do` — reuse** той же структурной формы (один паттерн на два DSL; оператор не учит второй синтаксис цикла).
  - **`remove` (state_changes) ≠ `delete` (migration-DSL) — намеренно разные имена.** `delete` в миграции = «снести путь в state-структуре» (адресация `path:`, операция над формой данных при смене схемы). `remove` в state_changes = «вынуть элемент из коллекции» (адресация `match:`-предикатом над элементами, операция над содержимым при runtime-сценарии). Разные операции → разные имена, чтобы не путать «удалить поле схемы» и «удалить элемент коллекции».

- **Отвергнутые альтернативы.**
  - **Парные глаголы `remove_one` / `remove_all` (и `modify_one`/`modify_all`).** Удвоение словаря ради того, что выражается кратностью предиката + опц. `expect`. Отвергнуто: множественность — свойство `match`, а не имени глагола.
  - **Флаг `all: true`.** Тот же недостаток — управляющая ручка вместо декларативного предиката; `all` неявно при отсутствии — а `expect` явно фиксирует ожидание автора, и линт ловит широкий `match` независимо от флага.
  - **Позиционный `remove` (первый/последний элемент).** Невоспроизводим в JSONB-хранилище (порядок элементов в jsonb-массиве не является контрактом state); «первый по какому полю» = это `match` + сортировка, а не позиция. Отвергнуто как недетерминированное.
  - **`clear`** (очистка коллекции) — избыточен: `set: <field>` + `value: []`/`{}`.
  - **`rename` / `move`** — это операции над **формой** state при смене схемы, принадлежат migration-DSL ([ADR-019](0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)), не runtime-сценарию. В `state_changes` не вводятся.
  - **`upsert`** — слитное «add-или-modify». Скрывает намерение автора (создаём новое vs патчим существующее — разные семантики аудита). Покрывается явным `add` + `on_conflict: replace`, где намерение видно.

- **Consequences.**
  - [orchestration.md §7.1](../scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций) переписывается под list-of-verbs (нормативная спека); абзацы про `appends`/`modifies` уезжают в раздел «deprecated, переходный период».
  - [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) получает amendment (грамматика `state_changes` расширена; статус `amended`).
  - [naming-rules.md](../naming-rules.md) пополняется глаголами и ключами state_changes.
  - Латентный баг «`appends`/`modifies` не растят state» закрыт реализацией `add`/`modify`.
  - Реализация (keeper-side render/apply state_changes) — параллельный слайс developer-а, вне этого ADR.

- **Trade-offs.**
  - Breaking-смена формы (map→list) — оправдана dual-parse-окном в один релиз + soul-lint warn; миграция механическая (`sets:` → список `set`-элементов).
  - `expect` — опциональный, не обязательный: автор сам решает, нужна ли ассерт-страховка кратности; дефолт `any` не навязывает оверхед.
  - all-by-default у `modify`/`remove` потенциально широк — компенсируется soul-lint WARN на константно-истинный/отсутствующий `match` и empty-match-as-no-op (идемпотентность).

- **Amendment (2026-06-24): операционные сценарии читают развёрнутый факт из `incarnation.state`.** Симметрично write-семантике этого ADR (как `state` **записывается** после `create`) фиксируется read-конвенция: **операционный сценарий читает развёрнутый факт о сервисе из `incarnation.state`, а НЕ из `essence`/`input`.**

  - **Причина.** `create` транслирует операторский `input` (бьёт `essence`-дефолты) в развёрнутую конфигурацию и кладёт её в `state` (`state_changes`, глаголы `set`/`add`/…). На операционном этапе этот `input` уже недоступен, а `essence` — лишь author-подложка, не видящая operator-override. Операционный сценарий, читающий `essence`, видит **не то, что реально применено** → рассинхрон «сценарий думает одно, на хосте другое». `state` — единственное место, где зафиксировано *что применено*.
  - **Граница.** `essence`/`input` на операционном этапе используются только для того, что в `state` принципиально не лежит. Главный случай — **секреты** (ИБ-инвариант: PEM/пароли в `state` не материализуются; в `state` лежит лишь **путь** к секрету на хосте). Их операционный сценарий резолвит `vault(<ref>)` тем же способом, что и `create` (`ref` из author-context `essence.<...>_ref` или конвенции пути). Известное ограничение: operator-override `ref` на `create` в `state` не сохраняется → операционный сценарий читает `essence`-`ref` (совпадает с реально развёрнутым секретом при `create` без override; сохранение operator-override-ref в `state` — отдельный слайс при реальном запросе).
  - **CEL-идиома чтения ключа с дефисом.** Проверка наличия ключа коллекции — оператором `'<key>' in <map>`, **не** `has(<map>['<key>'])`: `has()` — макрос только для field-selection, index-аргумент отвергается парсером. Защита от no-such-key (отсутствие `state` в push/trial-прогоне без State, ещё-не-материализованная коллекция) — `has(incarnation.state) && has(incarnation.state.<col>) && '<key>' in incarnation.state.<col>`. Bracket-нотация остаётся в **доступе** к значению (`incarnation.state.<col>['<key>']`).
  - **Нормативная спека конвенции** — [docs/destiny/production-conventions.md §7a «Операционные сценарии: источник истины = `incarnation.state`»](../destiny/production-conventions.md). Рабочая иллюстрация — `restart` сервиса [`redis`](../../examples/service/redis/scenario/restart/main.yml) (TLS-дискриминатор из `incarnation.state.redis_config`, guard-кейсы `rolling-restart-replicas`/`rolling-restart-tls` на оба пути).

- **Статус.** amended (2026-06-24: операционная read-конвенция).
