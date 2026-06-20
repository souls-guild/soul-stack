# `tasks/main.yml` — спецификация формата задач destiny

Этот документ — **полная спецификация** формата задач destiny. Источник правды при реализации. Любой ключ, не описанный здесь, **недопустим** — это ошибка валидации. Расширения формата — propose-and-wait → правка этого документа → потом код.

Связанные документы: [concept.md](concept.md), [manifest.md](manifest.md), [input.md](input.md), [vars.md](vars.md), [testing.md](testing.md).

## 1. Файловый формат

- **`tasks/main.yml`** — точка входа destiny. Top-level YAML — **список** задач, исполняемых в порядке появления.
- **`tasks/<sub>.yml`** — include-соседи, та же структура (top-level список задач).
- Никакой обёртки (`tasks:` ключом) на верхнем уровне файла нет — путь к файлу сообщает контекст.

## 2. Виды задач

Элемент списка — это **один** из трёх видов, дискриминируется по наличию ровно одного из ключей `module:` / `include:` / `block:`. Наличие двух или ноль — ошибка.

| Вид | Дискриминатор | Что делает |
|---|---|---|
| **Module-задача** | `module:` | Вызывает один state-модуль с параметрами |
| **Include-задача** | `include:` | Подключает соседний файл `tasks/<name>.yml` (раскрывается inline) |
| **Block** | `block:` | Inline-группа задач с общим `when:` / requisites (см. §6.5) |

Параллельное исполнение — флаг `parallel: true` на любой задаче, **не** отдельный вид. См. §6.

## 3. Полный список блоков задачи

Сводная таблица. Семантика, валидация и примеры — в §4-§8.

| Блок | Тип | Применим к | Обязательность |
|---|---|---|---|
| `name:` | string | всем | рекомендуется |
| `module:` | string | module-задаче | один из {module, include, block} |
| `include:` | string | include-задаче | один из {module, include, block} |
| `block:` | array of tasks | block-задаче | один из {module, include, block} |
| `params:` | map | module-задаче | обязателен, если `module:` |
| `vars:` | map | всем | опционально |
| `when:` | string (template-expr) | всем | опционально |
| `parallel:` | bool | всем | опционально, default `false` |
| `loop:` | map | module-задаче, include-задаче | опционально |
| `register:` | string (identifier) | module-задаче | опционально |
| `id:` | string (identifier) | module-задаче (pilot) | опционально |
| `output:` | map | module-задаче | опционально |
| `no_log:` | bool | module-задаче | опционально, default `false` |
| `onchanges:` | array of register-id | всем | опционально |
| `onfail:` | array of register-id | всем | опционально |
| `require:` | array of register-id ИЛИ `"all"` | всем | опционально |
| `changed_when:` | string (template-expr) | module-задаче | опционально |
| `failed_when:` | string (template-expr) | module-задаче | опционально |
| `retry:` | map | module-задаче | опционально |
| `timeout:` | duration | module-задаче | опционально |

Любой другой ключ — ошибка валидации.

> Ключи `serial:` / `run_once:` — **scenario-only**: в destiny (`tasks/main.yml`) недопустимы (см. [`docs/scenario/orchestration.md §2`](../scenario/orchestration.md#2-дельта-scenario-относительно-dsl-ядра)).

## 4. Базовые блоки

### `name:`

- **Тип:** string.
- **Применим к:** всем видам задач.
- **Обязательность:** **рекомендуется** (lint-правило закрепится отдельно, см. [Open Q](#12-open-q)).
- **Семантика:** человекочитаемая строка, идущая в лог apply, в trace для coverage ([testing.md](testing.md)), в UI Keeper-а и в сообщения об ошибках задачи. **Не идентификатор**: дубликаты допускаются, ссылаться на задачу по `name:` нельзя.
- **Соглашение об именовании:** см. §5.

### `module:`

- **Тип:** string.
- **Применим к:** module-задаче.
- **Семантика:** полное 3-уровневое имя модуля `<namespace>.<module>.<state>` (см. [architecture.md → «Адресация модулей»](../architecture.md#адресация-модулей)).
- **Валидация (статика, `soul-lint`):**
  - формат `<ns>.<module>.<state>`;
  - namespace либо `core`, либо упомянут в `required_modules:` destiny;
  - `<state>` существует в манифесте модуля.

### `params:`

- **Тип:** map.
- **Применим к:** module-задаче.
- **Обязательность:** обязателен (даже если пустой `params: {}`).
- **Семантика:** аргументы модуля. Схема — из манифеста модуля для конкретного `<state>`.
- **Валидация:** `soul-lint` сверяет с `parameters_schema` манифеста модуля.

### `include:`

- **Тип:** string.
- **Применим к:** include-задаче.
- **Семантика:** имя файла из той же папки `tasks/` (без слэшей). Раскрывается inline, без отдельного scope.
- **Раскрытие — до render, в плоский список.** within-destiny `include:`
  раскрывается при загрузке destiny-артефакта (до фазы render), так же как
  scenario-include ([scenario/orchestration.md §6](../scenario/orchestration.md#6-двухуровневый-резолв-ресурсов)):
  include-задача заменяется задачами подключённого файла inline, на их месте;
  render получает плоский список без `include:`-узлов.
- **Правила:**
  - относительные пути за пределы `tasks/` запрещены (`../...`, абсолютные);
    резолв строго внутри каталога `tasks/` снапшота (securejoin-кламп);
  - подключаемый файл имеет ту же структуру (top-level список задач);
  - вложенные `include:` допустимы (раскрываются рекурсивно);
  - **циклы** (`a → b → a`, прямой self-include) детектируются по resolved-пути
    (ошибка `include_cycle`), глубина ограничена жёстким потолком — не
    бесконечная рекурсия;
  - `name:` на include-задаче работает как **заголовок группы** в логе apply.

> **В scenario правило `include:` иное.** Здесь описано поведение для **destiny**: `include:` — строго сосед в той же папке `tasks/`. В scenario резолв двухуровневый (локально → service-level, fallback делает движок, `../` в синтаксисе по-прежнему запрещён) — см. [`docs/scenario/orchestration.md §6`](../scenario/orchestration.md#6-двухуровневый-резолв-ресурсов). Это правило §4 для destiny **не меняется**; отличие scenario зафиксировано в scenario-спеке.

### `block:`

См. §6.5 — inline-группа задач с общим `when:` / requisites.

### `parallel:`

Флаг на задаче — см. §6. Не отдельный вид задачи.

## 5. Соглашение об именовании `name:`

Пишется как **человекочитаемое английское предложение в императиве**, начинающееся с **заглавной буквы**, без точки в конце. Стиль совпадает с conventional commit subject и Ansible task names — оператор читает лог apply сверху вниз и видит последовательность действий.

Валидные:
- `Install redis-server package`
- `Render /etc/redis/redis.conf from template`
- `Ensure redis-server is running and enabled at boot`

Невалидные / нерекомендуемые:

| Пример | Что не так |
|---|---|
| `install redis-server package` | Строчная буква в начале |
| `install-redis` | Kebab-case identifier, не предложение |
| `Installing redis-server.` | Present continuous + точка в конце |
| `установить redis` | Нелатиница (стиль исходников — английский) |

Конкретный max length зафиксируется вместе с lint-правилом.

> Правило заглавной буквы закреплено **только для destiny-задач**. Для scenario и tests — отдельным решением.

## 6. Параллелизм (`parallel: true`)

`parallel:` — **флаг** на обычной задаче (`bool`, default `false`). Не отдельный вид задачи и **не группирующий механизм**. Семантика — fire-and-forget: задача стартует в отдельном потоке/треде, основной flow идёт дальше, не дожидаясь её завершения.

```yaml
- name: Ping redis             # стартует в треде A → flow идёт дальше
  module: core.exec.run
  parallel: true
  register: ping
  params: { command: "redis-cli ping" }

- name: Read replication state # стартует в треде B → flow идёт дальше
  module: core.exec.run
  parallel: true
  register: repl
  params: { command: "redis-cli INFO replication" }

- name: Read memory usage      # стартует в треде C → flow идёт дальше
  module: core.exec.run
  parallel: true
  register: mem
  params: { command: "redis-cli INFO memory" }

- name: Collect diagnose result    # обращается к register.ping/repl/mem →
  module: core.noop.run            # implicit barrier: ждёт все три треда
  params: {}
  output:
    ping:        "${ register.ping.stdout }"
    replication: "${ register.repl.stdout }"   # парсинг INFO replication — на стороне caller-а scenario
    memory:      "${ register.mem.stdout }"
```

### Барьеры

Параллельные задачи **ОБЯЗАТЕЛЬНО дожидаются завершения** — но строго в двух моментах: при ссылке на их `register:` и в самом конце destiny. Никаких автоматических барьеров на границах `include:` / `block:` нет: parallel-задачи свободно «утекают» через эти границы и продолжают работать в фоне.

1. **Implicit barrier — обращение к `register.<name>`.** Если последующая задача (через `when:`, `params:`, `output:`, `onchanges:`, `onfail:`, `require:`) ссылается на `register: <name>` parallel-задачи — она блокируется, пока эта parallel-задача не завершится. Барьер локальный: только на конкретный `register: <name>`; остальные parallel-задачи продолжают исполняться в фоне.

2. **Final barrier — конец destiny.** В самом конце прогона destiny фреймворк безусловно ждёт **все** ещё работающие parallel-задачи. Destiny не считается завершённой, пока все треды не finalized, и если хоть одна failed — destiny считается failed. Это обязательное поведение, не опциональное.

3. **Явный barrier — `require:` или `require: all`.** Можно сделать задачу-барьер, которая ждёт упомянутые parallel-задачи (по списку register-id) или **все** активные parallel-задачи (`require: all`). Подробности — §8.

### Утечка parallel-задач через границы

`parallel: true` на задаче внутри `include:` или `block:` стартует поток **в общий run-context destiny**. Когда include / block заканчивается, основной flow идёт дальше — parallel-задачи продолжают работать в фоне до своего естественного завершения или до final barrier-а.

```yaml
# tasks/main.yml
- name: Run background warmup
  include: warmup.yml         # внутри есть `parallel: true` задачи

- name: Render main config    # стартует сразу после include — не ждёт warmup
  module: core.file.present
  params: { ... }

- name: Verify warmup complete
  require: all                # явный barrier — ждать всех parallel-задач
  module: core.noop.run
  params: {}
```

Если такое поведение нежелательно (например, warmup должен закончиться до render-config) — это явная зависимость через `require:`, не неявная семантика include.

### Правила

- **Группировки нет.** `parallel: true` на двух соседних задачах **не** означает «выполнить вместе». Каждая просто запускается в своём потоке независимо. Соседи могут быть любые — другие parallel-задачи, обычные (не-parallel) задачи, include, block.
- **Failure-семантика по умолчанию.** Если parallel-задача упала, `register.<name>.failed = true` записывается. Основной flow **не** прерывается в момент fail-а — он узнаёт о failure только при implicit barrier (либо в final barrier). На финальном барьере destiny: если хоть одна parallel-задача finally-failed → весь destiny считается failed.
- **Cancel on first failure** — open Q (см. §12). Сейчас: упавшая parallel-задача не отменяет остальные.
- **`when: false`** — задача пропускается, поток не стартует, `register.<name>` отсутствует, последующие обращения к нему — ошибка валидации.
- **Requisites через `register.<name>`** — работают, через них и реализуется implicit barrier. `onchanges: [ping]` после `parallel: true ping` — корректно: фреймворк дожидается завершения `ping`, проверяет `register.ping.changed`, потом выполняет/пропускает текущую.
- **`include:` с `parallel: true`** — весь подключённый файл выполняется в одном потоке (задачи внутри последовательны, как обычно); основной flow идёт дальше. На границе include **никакого implicit barrier нет** — parallel-задачи из подключённого файла продолжают работать в фоне до их естественного завершения / final barrier-а / явного `require:`.
- **`block:` с `parallel: true`** — block выполняется в одном потоке (задачи внутри последовательны); основной flow идёт дальше. Через границу block — то же правило, что и для include: barrier-а нет.
- **`loop:` + `parallel: true`** — каждая итерация запускается в отдельном потоке, основной flow идёт дальше. Подробности — §7.

## 6.5. Block — inline-группа задач

`block:` — третий вид задачи: **inline-список** из N задач, к которому применяется общий `when:` (и опционально `onchanges:` / `onfail:` / `require:`). Аналог Ansible `block:`. Используется, когда несколько соседних задач имеют **одинаковое условие** и не хочется повторять `when:` на каждой или выносить в отдельный файл через `include:`.

```yaml
- name: Apply Redis configuration
  when: input.action == 'apply'
  block:
    - name: Install redis-server package
      module: core.pkg.installed
      params: { name: redis-server, version: "${ input.version }" }

    - name: Render redis.conf
      module: core.file.present
      register: redis_conf
      params: { path: /etc/redis/redis.conf, content: "..." }

    - name: Ensure redis-server is running
      module: core.service.running
      params: { name: redis-server }
```

Все три задачи выполняются только когда `input.action == 'apply'`; если falsy — пропускаются все три одним решением.

### Правила

- **Содержимое** `block:` — top-level список задач, тот же формат и тот же набор полей, что в `tasks/main.yml`. Внутри допустимы любые виды задач: `module:`, `include:`, вложенный `block:`.
- **`when:` block-задачи** — оборачивающее условие. Falsy → все задачи пропускаются как одна группа, с пометкой `skipped: when` на самой block-задаче (не на каждой внутренней).
- **`onchanges:` / `onfail:` / `require:` block-задачи** — применяются ко всей группе так же, как к одной задаче: если условие не выполнено, вся группа пропускается.
- **Внутренние `when:`** допустимы и комбинируются с внешним по AND: внутренняя задача выполняется только если внешний `when:` block-а truthy И её собственный `when:` truthy.
- **`register:`** на самой block-задаче не имеет смысла и **запрещён** — block не вызывает модуль, нечего регистрировать. Задачи внутри могут иметь свои `register:`-имена; они доступны после block-а.
- **`name:` block-задачи** — заголовок группы в логе apply, как у `include:`-задачи. Соглашение об именовании — то же (§5).
- **Параллелизм:** `parallel: true` на block-задаче помечает **всю группу** как члена parallel-группы текущего уровня; внутри блока задачи выполняются между собой по своим правилам (`parallel: true`-флаги соседей внутри блока работают как обычно).
- **`loop:`** на block-задаче **разрешён** — block раскрывается N раз с разными значениями переменной итерации; внутри `${ <as>.* }` (или голая `<as>.*` в expression-keys) доступен.
- **Вложенность.** Block внутри block разрешён, глубина не ограничена жёстко. На практике 2 уровня покрывают все реалистичные кейсы.

### Когда `block:` vs `include:`

- **`block:`** — короткая inline-группа (2–6 задач), которая не переиспользуется. Не плодим файлы.
- **`include:`** — переиспользуемая группа или большой блок (>~10 задач), который перегружает чтение `main.yml`.

### Чего пока **нет** (открытые вопросы)

- **`rescue:`** (Ansible) — список задач, выполняемых при fail внутри `block:`. Сейчас обходимся `onfail:`-задачами после block-а.
- **`always:`** (Ansible) — список задач, выполняемых независимо от исхода block-а (cleanup, finalize). Сейчас обходимся обычными задачами без `onfail:`/`onchanges:`-зависимости от block-а.

Оба механизма зафиксированы в §12 как open Q. До их реализации семантика `block:` — только группировка по условию, без error-handling.

## 7. Циклы (`loop:`)

Повторяет одну задачу или один `include:` по элементам коллекции.

### Фаза раскрытия (нормативно)

`loop:` раскрывается в **render-фазе**, per-host: одна задача → **N
`RenderedTask`** по элементам `items:`, со сквозными индексами задач прогона
(симметрично раскрытию `apply: destiny`). Это **отличается** от `include:`,
который раскрывается раньше, на этапе config-splice (вклейка соседнего файла в
плоский список задач **до** render). Причина различия: `items:` —
CEL/template-выражение (`${ input.users }` / `${ vars.x }`), а CEL вычисляется
только в render-фазе; на этапе config-splice значения коллекции ещё не известны.

Порядок раскрытия в render: резолв таргета (`on:` → `where:` → `run_once:`) →
резолв `items:` (один раз на прогон, host-инвариантный источник `input`/`vars`)
→ для каждого элемента (после `when:`-фильтра) рендер `params:` с активной
loop-переменной → N `RenderedTask`. Оси `loop:` и `serial:` ортогональны (см.
[orchestration.md §2.2](../scenario/orchestration.md)): весь loop прокатывается
на каждом хосте волны.

### Базовый синтаксис

```yaml
- name: Apply ACL for each redis user
  module: core.exec.run
  loop:
    items: "${ input.users }"       # array или object
    as: user                        # имя переменной в итерации; default: item
  params:
    command: "redis-cli ACL SETUSER ${ user.name } ${ user.acl }"
    no_log: true
```

### Поля `loop:`

| Поле | Тип | Default | Описание |
|---|---|---|---|
| `items:` | template-expr | — *(required)* | Источник элементов: ссылка на `input.<X>` / `vars.<X>` со схемой `array` или `object` |
| `as:` | string | `item` | Имя переменной текущего элемента в шаблонах задачи |
| `index_as:` | string | — | Имя переменной для индекса/ключа (опционально) |
| `when:` | template-expr | — | Фильтр элементов: итерация выполняется, только если выражение truthy для конкретного элемента |

Параллелизм итераций управляется task-level `parallel: true`, не отдельным `loop.parallel:`. См. ниже подраздел «`loop:` + `parallel: true`».

> **Пилот:** `items:` и `loop.when:` вычисляются **один раз на прогон в
> host-инвариантном контексте** (`input`/`register`/`incarnation` + loop-
> переменная, **без `soulprint`**). `when:` — фильтр по содержимому элемента
> (`item.enabled`), не per-host предикат: ссылка на `soulprint` в `loop.when:`
> → ошибка валидации (host-вариативный `when:`/`items:` через `soulprint`
> конкретного хоста отложен вместе с per-host loop-фильтрацией). Per-host выбор
> по фактам хоста делается на task-level `where:`, не внутри `loop:`.

### Семантика `items:`

- **Array** (`[a, b, c]`) → `as`-переменная пробегает по элементам. `index_as:` — числовой индекс 0-based.
- **Object** (`{k1: v1, k2: v2}`) → `as`-переменная — это значение. `index_as:` — ключ. Порядок итерации — алфавитный по ключам.

### Семантика `loop` на `include:`

```yaml
- name: Provision each redis user
  include: ensure-user.yml
  loop:
    items: "${ input.users }"
    as: user
```

Содержимое `ensure-user.yml` исполняется N раз, на каждой итерации `${ user.* }` (в строках) / `user.*` (в expression-keys) доступен в задачах файла.

### `loop:` + `parallel: true`

Если на задаче стоит `parallel: true` **и** `loop:`, каждая итерация запускается **в собственном потоке** (fire-and-forget). Основной flow идёт дальше после старта всех итераций, не дожидаясь.

Все итерации записываются в один и тот же `register: <name>` как массив результатов (`register.<name>[0]`, `register.<name>[1]`, …). Никакой особой barrier-семантики на loop'е нет — действует общее правило §6: ссылка на `register.<name>` ждёт завершения **всей** задачи (то есть всех итераций), потому что register по этому имени один и заполняется задачей целиком.

Использовать с осторожностью: модули должны быть thread-safe относительно состояния хоста. Например, `core.pkg.installed` двух разных пакетов параллельно — конфликт через apt-lock (apt держит global lock). `core.exec.run` независимых redis-команд — обычно безопасно.

### Что недоступно

- **Вложенные `loop:`** в одной задаче. Если нужна вложенность — внешний `loop:` на `include:`, внутри которого есть свой `loop:`.

## 8. Requisites — Salt-style зависимости

Все три блока ссылаются на `register:`-имена других задач. `register:` поэтому играет двойную роль: место для результата + identifier для адресации.

### `register:`

- **Тип:** string (identifier, `[a-z][a-z0-9_]*`).
- **Применим к:** module-задаче.
- **Семантика:** имя переменной, в которую сохраняется результат задачи. Значение содержит как минимум поле `.changed` (bool — изменила ли задача state хоста) и `.failed` (bool — упала ли задача). Поля от модуля (`.stdout`, `.exit_code`, …) — из его `output:`.
- **Доступ:** `${ register.<name>.* }` в строковой интерполяции; в top-level expression-keys (`when:`/`changed_when:`/`failed_when:`/`until:`/`where:`) — голая форма `register.<name>.*` (см. [`docs/templating.md`](../templating.md)). В последующих задачах (порядок не нарушен).
- **Уникальность:** в пределах одного прогона destiny `register:` должен быть уникален.

### `id:`

- **Тип:** string (identifier, `[a-z][a-z0-9_]*` — тот же формат, что `register:`).
- **Применим к:** module-задаче *(pilot-ограничение; на `block:`/`include:` пока не поддерживается)*.
- **Семантика:** **только адрес** задачи для подписки на алерты «эта таска изменила state» (per-task-changed-уведомления, [ADR-009 amendment](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). В отличие от `register:`, `id:` **не** захватывает output задачи и **не** создаёт переменную `register.<name>` — это исключительно стабильный селектор для подписки.
- **Опционально.** Отсутствие `id:` — норма; большинство задач его не имеют.
- **Взаимоисключим с `register:`.** Задача с `register:` уже адресуема по нему, поэтому `id:` на ней избыточен и запрещён (ошибка валидации). `id:` нужен задачам **без** `register:`, которые всё равно хотят быть адресуемыми для алертов. Единый формат — потому что `register` и `id` живут в одном адресном пространстве подписки.
- **Работает и для keeper-side задач (`on: keeper`).** Адрес `register ∪ id` keeper-side задачи (`core.cloud.provisioned`/`core.vault.kv-read`/`core.soul.registered`, [docs/keeper/modules.md](../keeper/modules.md)) точно так же попадает в `changed_tasks` терминального события прогона, и подписка-алерт на него рабочая. Типичный кейс — `provision_vm` с `id:` без `register:` (см. [ADR-052 amend §k/§l](../adr/0052-herald-notifications.md#amend-kl-t4-fix-2026-06-12-changed_tasks-и-task-подписка-покрывают-keeper-side-задачи)).

```yaml
- name: Reload sysctl after tuning
  id: sysctl_reloaded          # адрес для алерта «sysctl изменился»; output не захватывается
  module: core.sysctl.present
  params: { name: vm.swappiness, value: "10" }
```

> **Pilot.** Сейчас `id:` допустим только на module-задаче (у неё есть собственный changed-сигнал). На `block:`/`include:` — ошибка валидации; расширение — отдельным заходом. Уникальность `id:` по прогону (после раскрытия `include`/`block`) проверяется отдельной cross-ref-фазой линта.

### `onchanges:`

- **Тип:** array of register-id.
- **Применим к:** всем видам задач.
- **Семантика:** задача выполняется **только если** хотя бы одна из упомянутых задач имела `register.<name>.changed == true`. Если ни одна не изменила state — задача пропускается с пометкой `skipped: no changes upstream`.
- **Назначение:** Salt-style handlers — «перезапустить сервис, если изменился конфиг».

```yaml
- name: Render redis.conf
  module: core.file.present
  register: redis_conf
  params: { path: /etc/redis/redis.conf, content: "..." }

- name: Restart redis-server because config changed
  module: core.service.restarted
  onchanges: [redis_conf]
  params: { name: redis-server }
```

### `onfail:`

- **Тип:** array of register-id.
- **Применим к:** всем видам задач.
- **Семантика:** задача выполняется **только если** хотя бы одна из упомянутых задач имела `register.<name>.failed == true` (зеркало `onchanges:`, триггер по `failed` вместо `changed`). `TIMED_OUT`-источник — частный случай `failed` (`register.<name>.failed == true`), тоже триггерит `onfail`. Если ни одна не упала — задача пропускается (`skipped`). В нормальном прогоне без провалов `onfail`-задача **всегда** `skipped`.
- **Назначение:** rescue / recovery-обработчики, локальная компенсация (rollback, cleanup, уведомление о провале).

```yaml
- name: Apply migration
  module: core.exec.run
  register: migration
  params: { command: "redis-migrate up" }

- name: Rollback migration on failure
  module: core.exec.run
  onfail: [migration]
  params: { command: "redis-migrate down" }
```

#### `onfail:` меняет fail-stop прогона (rescue-семантика)

По умолчанию прогон работает в режиме **fail-stop**: первая упавшая задача (`failed`/`timed_out`) останавливает основной flow. `onfail:` вводит в этот инвариант исключение — **rescue-хвост**:

- Первая `failed`/`timed_out`-задача **необратимо** помечает результат прогона как провал (`RunStatus = FAILED`).
- Но цикл задач **не прерывается**: все последующие **обычные** (без `onfail:`) задачи пропускаются (`skipped`, модуль не вызывается), а отрабатывают **только** `onfail:`-задачи, чей источник упал. Это и есть rescue/cleanup.
- `onfail:`-задачи провал **не отменяют**: даже если все rescue-обработчики прошли успешно, итоговый `RunStatus` остаётся `FAILED`. `onfail:` — это компенсация, а не «прощение» провала.
- `failed_when: false` (ignore_errors, [см. ниже](#failed_when)) делает задачу `OK` — она **не** триггерит ни fail-stop, ни `onfail:` последующих (она не `failed`).
- Отмена прогона оператором (`CancelApply`) — это **не** fail-stop; на отмену rescue **не** срабатывает, цикл прерывается безусловно (`RunStatus = CANCELLED`).

> Аналог Ansible `rescue:` внутри `block:`, но через unified-механизм requisite-ов: `onfail:`-задача после блока вместо вложенного `rescue:`-списка (см. [§6.5](#65-block--inline-группа-задач)).

### `require:`

- **Тип:** **array of register-id** ИЛИ **строка `"all"`**.
- **Применим к:** всем видам задач.
- **Семантика:** задача не стартует до завершения упомянутых parallel-задач. В линейном flow без `parallel:` требование избыточно (порядок и так гарантирован).
- **Форма 1 — `require: [a, b, c]`.** Ждать перечисленные задачи по их `register:`-id. Барьер локальный — только на эти три, остальные parallel-задачи продолжают исполняться.
- **Форма 2 — `require: all`.** Специальное значение: ждать **все** активные parallel-задачи, запущенные ранее в этом прогоне destiny. Используется как явный «синхронизирующий барьер» — например, перед задачей, которая хочет видеть согласованное состояние после нескольких parallel-фаз. Mixed (`require: [a, "all"]`) — ошибка валидации; форма строго одна из двух.

```yaml
# Точечный барьер
- name: Send report once metrics are ready
  module: core.exec.run
  require: [collect_cpu, collect_memory]
  params: { command: "report-send.sh" }

# Глобальный барьер — ждать все parallel-задачи
- name: Final consistency check
  module: core.exec.run
  require: all
  params: { command: "check-cluster-state.sh" }
```

> **Граница с onchanges/onfail.** `require:` — про **порядок** (ждать). `onchanges:`/`onfail:` — про **условие** (выполнять или нет). Их можно комбинировать: `require: [migration]` + `onfail: [migration]` = «дождись migration, выполни только если она упала».

### Адресация через `register:`, не через `name:`

`name:` — человекочитаемая строка, дубликаты допустимы, не идентификатор. Адресация в requisites — строго через `register:`. Если задача нигде не упомянута через requisites, `register:` ей не нужен.

## 9. Прочность и контроль исполнения

### `when:`

- **Тип:** CEL-выражение, вся строка трактуется как CEL без обёртки `${ … }` ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md §2.1`](../templating.md#21-top-level-expression-ключи)). Результат приводится к bool.
- **Применим к:** всем видам задач.
- **Семантика:** задача выполняется, только если выражение truthy. Falsy → задача пропускается с пометкой `skipped: when`.
- **Сочетание с requisites:** все условия суммируются по AND. Сначала проверяются requisites (`onchanges`, `onfail`, `require`), потом `when:`.

### `retry:`

- **Тип:** map.
- **Применим к:** module-задаче.
- **Семантика:** повторить задачу до `count` раз, если она упала или `until:`-условие не выполнено. **Энфорс — Soul-side** (flow-control, `applyrunner.runTaskWithRetry`): Keeper протягивает `count`/`delay`/`until` в `RenderedTask`, петлю накручивает Soul во время прогона (`until` зависит от `register.self.*` свежей попытки — известно только Soul).

```yaml
retry:
  count: 5                     # макс. число попыток (включая первую). 0/1/пусто — одна попытка (без retry).
  delay: 10s                   # пауза между попытками; пусто — default 5s. convention `duration` Soul Stack.
  until: register.self.changed # ОПЦ.: top-level expression-key, вся строка = CEL без обёртки ${ … }.
```

**Семантика петли** (Soul, per architect; порядок per-попытка: Apply → timeout-check → `changed_when` override → `failed_when` override → `until`-eval):

- **`retry:` БЕЗ `until:`** — повтор, пока попытка FAILED **или** TIMED_OUT; первый не-FAILED исход (OK/CHANGED) → выход. Все попытки исчерпаны → финальный статус **последней** попытки как есть (FAILED **или** TIMED_OUT — TIMED_OUT **не** схлопывается в FAILED).
- **`until:` (+`retry:`)** — `until` вычисляется **после каждой попытки** (после `changed_when`/`failed_when` override), той же sandboxed-песочницей и активацией, что `failed_when`. `until`-true → выход, финальный статус = статус попытки **как есть** (`until` **не** override-ит `failed`: failed остаётся failed; truthy-until на FAILED-попытке → финал FAILED). `until`-false → `delay` → следующая попытка. После `count` попыток с `until`-false → задача **FAILED** (`flowcontrol.until_exhausted`), **даже если** последняя попытка была OK/CHANGED. `until` без `count` (count=1) → одна попытка + одна `until`-eval (false → FAILED). На **TIMED_OUT**-попытке `until` **не** вычисляется (таймаут = «неуспех, повторить если попытки остались»).
- **`failed_when: false` × `retry:`** — попытка с `failed_when: false` (ignore_errors) → `failed=false` → «не-FAILED исход» → выход **на первой попытке** (ignore_errors побеждает retry; упавший модуль, заглушённый бизнес-условием, не ретраится).
- **`delay`** применяется **только между попытками** (не перед первой, не после последней). Прерывается отменой прогона (`CancelApply`): cancel во время `delay`/попытки → выход из петли, задача CANCELLED. В `timeout:` (per-попытка) `delay` **не** входит — отдельного ceiling на всю петлю нет.

Задача считается finally-failed, если все `count` попыток упали/таймнули (без `until:`) или `until:` так и не стало truthy (с `until:`). `register.<name>.failed`/`.timed_out` отражают финальный исход. retries-exhausted FAILED триггерит `onfail:`/rescue как обычный FAILED. Промежуточные попытки наружу **не** эмитятся — контракт «один `TaskEvent` на `task_idx`» сохранён, attempts-счётчик в `register.self.*` **не** вводится (отложено, Вариант B).

### `timeout:`

- **Тип:** duration (Go syntax: `30s`, `5m`, `1h30m`).
- **Применим к:** module-задаче.
- **Семантика:** жёсткий лимит на одну попытку (с `retry:` — на каждую отдельно: каждый Apply получает свой `context.WithTimeout`). По истечении модуль получает сигнал отмены (host-side gRPC cancel), попытка отмечается TIMED_OUT (`register.<name>.timed_out == true`). С `retry:` TIMED_OUT-попытка ретраится, если попытки остались (см. семантику петли выше); `until` на TIMED_OUT-попытке не вычисляется.

### `no_log:`

- **Тип:** bool, default `false`.
- **Применим к:** module-задаче.
- **Семантика:** при `true` поля `params:` и `output:` задачи не пишутся в лог apply, не сохраняются в трейс, маскируются в API-ответе. Для задач, прокидывающих секреты (пароли, токены).

### `output:`

- **Тип:** map (имя → template-expr).
- **Применим к:** module-задаче.
- **Семантика:** значения, которые destiny публикует наружу как свой результат. Task-level `output:` **пишет в объявленные top-level `output:`-поля** destiny (см. [destiny/output.md](output.md)); имена, не объявленные в top-level `output:` destiny — ошибка валидации. Используется `expect:` в тестах и `register:`-цепочках caller-а (`register.<applier>.<output-поле>` — [scenario/orchestration.md §2](../scenario/orchestration.md#2-дельта-scenario-относительно-dsl-ядра)).

### `changed_when:`

- **Тип:** CEL-выражение, вся строка = CEL без обёртки `${ … }` (top-level expression-key, [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).
- **Применим к:** module-задаче.
- **Семантика:** переопределяет, как фреймворк считает `register.<name>.changed`. По умолчанию модуль сам сообщает свой `changed` (например, `core.file.present` сравнивает desired vs actual). Для модулей, которые всегда возвращают `changed=true` (типично — `core.exec.run`), `changed_when:` позволяет дать кастомный critery.

```yaml
- name: Check if migration needed
  module: core.exec.run
  register: migration_status
  changed_when: contains(register.self.stdout, "pending")
  params:
    command: "redis-migrate status"
```

Внутри выражения доступен `register.self.*` — поля собственного результата задачи (`.stdout`, `.exit_code`, custom поля из `output:`-модуля).

`changed_when: false` — задача никогда не считается изменяющей state (read-only команды).

`changed_when:` напрямую влияет на `onchanges:` следующих задач — handlers триггерятся по переопределённому `changed`, не по сырому.

### `failed_when:`

- **Тип:** CEL-выражение, вся строка = CEL без обёртки `${ … }` (top-level expression-key, [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).
- **Применим к:** module-задаче.
- **Семантика:** переопределяет, как фреймворк считает `register.<name>.failed`. По умолчанию — exit_code != 0 (для exec-модулей) или модуль сам сообщает failure. `failed_when:` даёт кастомный criterion.

```yaml
- name: Run migration with non-zero exit on partial
  module: core.exec.run
  register: migration
  failed_when: "!(register.self.exit_code in [0, 2])"    # 2 = partial OK
  params: { command: "redis-migrate up" }
```

`failed_when: false` — задача никогда не считается упавшей (что бы ни случилось — продолжаем). Похоже на Ansible `ignore_errors: true`, но через unified механизм.

`failed_when:` напрямую влияет на `onfail:` следующих задач и на остановку основного flow при fail.

### `vars:` (task-level)

- **Тип:** map.
- **Применим к:** всем видам задач (module, include, block).
- **Семантика:** локальные переменные, доступные **только внутри этой задачи** (и её итераций, если есть `loop:`). Видны в шаблонах задачи как `${ vars.<name> }` (в строковой интерполяции) или голая `vars.<name>` (в top-level expression-keys).

**Приоритет.** Если имя в task-level `vars:` совпадает с именем в [`vars.yml`](vars.md), **task-level перебивает file-level** — побеждает более локальный scope. Это обязательная семантика, не open Q.

```yaml
# vars.yml
redis_unit_name: redis-server

# tasks/main.yml
- name: Render config for staging variant
  module: core.file.present
  vars:
    redis_unit_name: redis-server-staging    # перебивает vars.yml
  params:
    path: "/etc/systemd/system/${ vars.redis_unit_name }.service.d/override.conf"
    content: "..."
```

В этой задаче `${ vars.redis_unit_name }` → `redis-server-staging`. В соседних задачах без своего task-level `vars:` — снова `redis-server` из `vars.yml`.

- **Видимость:** только внутри одной задачи. На задачи, подключённые через `include:` / `block:` дочерней этой не распространяется (но **сама** include-задача с `vars:` — да: переменные видны всем задачам подключённого файла как у любой задачи).
- **Ссылки внутри `vars:`:** значения могут ссылаться на `input.*`, `incarnation.*`, `soulprint.self.*`, `essence.*`, `register.*`, loop-переменную `<as>` (если задача в `loop:`) — **но не на свои же task-vars** (нет циклических/взаимных ссылок). Каждое значение `vars:` вычисляется в контексте задачи, где другие task-vars ещё не видны; обращение `${ vars.<other> }` внутри одного из значений `vars:` → ошибка `no such key`. Порядок объявления ключей в `vars:` поэтому безразличен. *(File-level `vars.yml` как источник ссылок и приоритет «task-level перебивает file-level» — спроектированный слой; в текущем pilot render-а реализованы только task-level `vars:`.)*
- **Резолв per-task, per-host:** `vars:` вычисляются ДО `params:` / `where:` той же задачи. Поскольку значения могут ссылаться на `soulprint.self.*`, они вычисляются для каждого targeted-хоста; итоговые `params:` при этом обязаны оставаться host-инвариантны (pilot-ограничение, см. рендер-пайплайн).
- **Тип значений:** строка трактуется как CEL-интерполяция (`${ … }`; одиночный блок → нативный тип, иначе склейка в строку — [§10](#10-шаблонный-контекст)). Non-string-литералы (число/bool/коллекция) проходят как есть, без CEL-разбора (симметрично `params:`).
- **Применение:** override локалов для loop-итераций, one-off-корректировка путей в одной задаче, упрощение длинных шаблонных выражений.

### Dry-run

Dry-run — это **режим прогона всего destiny**, не флаг отдельной задачи. Включается оператором при `keeper.incarnation.run --dry-run` или через MCP/API. В этом режиме:

- Фреймворк вызывает у каждого модуля `Plan(...)` RPC вместо `Apply(...)`. Контракт `SoulModule` — см. [architecture.md → «Протокол модулей»](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style).
- **Каждый модуль обязан реализовать `Plan`.** Это часть SoulModule контракта, не опциональная фича. Если модуль не умеет реально планировать (например, custom-модуль с неконкретной логикой) — он реализует `Plan` **как заглушку**, возвращающую «изменений нет / неизвестно». Заглушка валидна и допустима, но модуль ОБЯЗАН отвечать на `Plan`.
- Side-effects на хосте отсутствуют — модули в `Plan` только читают/анализируют.
- Render шаблонов, валидация `input:` / `params:`, проверка `when:` / `onchanges:` — выполняются как в реальном прогоне.
- `register.<name>` заполняется планируемыми значениями (модуль возвращает их в `PlanReply`).
- Параметры задачи, влияющие на flow (`retry`, `timeout`, `parallel`), в dry-run **игнорируются**: retry не повторяет (`Plan` детерминирован), timeout не ограничивает (но фреймворк ставит общий timeout safety), parallel не запускает реальные потоки (исполнение последовательное для предсказуемого отчёта).

На уровне задачи специального поля для dry-run нет. Если задаче нужно поведение «делать что-то особенное только в dry-run» — это решается через template-контекст: `run.mode == 'dry_run'` (top-level expression-key, CEL без обёртки) — open Q, нужно ли вводить такую переменную (см. §12).

## 10. Шаблонный контекст

Шаблонизатор — CEL для YAML-выражений ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md`](../templating.md)). Две позиции:

- **Top-level expression-keys** (`when:`, `changed_when:`, `failed_when:`, `until:`; в scenario также `where:`) — вся строка = CEL **без обёртки**.
- **Строковая интерполяция** в `params:`, `output:`, `loop.*:`, значениях `vars.yml`, task-level `vars:`, `apply: input:` — через маркер `${ … }`.

В обеих позициях доступны:

| Имя | Содержание |
|---|---|
| `input.<name>` | Параметры destiny от caller-а, объявленные в `destiny.yml → input:` и провалидированные ([input.md](input.md)). |
| `vars.<name>` | Destiny-локалы. Резолвится по правилу **task-level `vars:` перебивает file-level `vars.yml`** — побеждает более локальный scope. Подробнее: [vars.md](vars.md), task-level — §9. |
| `soulprint.self.<…>` | Факты о текущем хосте: `soulprint.self.os.family`, `soulprint.self.network.primary_ip`, `soulprint.self.memory.total_mb`, … ([ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp), [`docs/soul/soulprint.md`](../soul/soulprint.md)). Голая `soulprint.<path>` без `.self` — ошибка валидации `soul-lint`. Кросс-хостовые запросы (`soulprint.where(...)`, `soulprint.hosts`) — уровень scenario, **не destiny**. |
| `register.<name>.*` | Результаты предыдущих задач (по `register:`). Стандартные поля: `.changed`, `.failed`, `.timed_out` + поля из `output:` задачи. На parallel-задаче обращение к `register.<name>` создаёт **implicit barrier** (см. §6). В scenario probe-шаг выполняется на каждом хосте таргета → `register.<name>` это per-host **карта** `sid → payload`, а не скаляр; свёртка к одному значению — [scenario/orchestration.md §4.3](../scenario/orchestration.md#43-свёртка-агрегатного-register-к-одному-значению). |
| `register.self.*` | Собственный результат задачи. Доступно **во всех expression-key контекстах задачи** — `changed_when:` / `failed_when:` / `until:` (destiny + scenario), а также `where:` (scenario-only, см. [orchestration.md §4](../scenario/orchestration.md#4-волатильный-предикат--where)). |
| `soulprint.hosts` | **Scenario-only.** Список хостов прогона; элемент — стабильные факты `sid` / `role` (declared) / `network` / `os` / `covens`. `.where("<pred>")` фильтрует по любому атрибуту (predicate-string). В destiny **недоступен** (как и любой кросс-хостовый `soulprint`-запрос); destiny получает топологию только через `apply: input:`. Спецификация — [scenario/orchestration.md §4.1](../scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор). |
| `incarnation.host_count` | **Scenario-only.** Число хостов в таргете прогона. Используется в probe-идиоме полноты (`failed_when: size(register.<p>) < incarnation.host_count`, [scenario/orchestration.md §5](../scenario/orchestration.md#5-probe-идиома-и-обработка-ошибок)). В destiny недоступен (часть `incarnation.*`, scenario-scope). Формальное определение — [`docs/scenario/orchestration.md §4.2`](../scenario/orchestration.md#42-incarnationhost_count--размер-таргета-прогона). |
| `<as>` / `<index_as>` | Текущий элемент / индекс активного `loop:` (имена настраиваются в `loop:`). |

### Контекст зависит от вызывающей сущности (destiny = хост / scenario = кластер)

DSL-ядро задач выше — общее для destiny и scenario ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Но **шаблонный контекст у них разный**, потому что разный scope (один хост vs один кластер):

| | destiny (один хост) | scenario (один кластер) |
|---|---|---|
| `essence.*` | **НЕТ** — destiny изолирована, видит только `input:`. Service подкладывает значения в `input:` при `apply:`-вызове. | **есть** — merged essence (default → os → coven → spec) доступен напрямую. |
| `incarnation.*` / `scenario.*` / `state.*` | **НЕТ** — destiny не знает про БД и про того, кто её вызвал. | **есть** — атрибуты вызывающего scenario / incarnation, текущий `state` из БД. |
| `soulprint.where(<predicate>)` | **НЕТ** — кросс-хостовые запросы это уровень scenario. | **есть** — cross-host lookup по предикату-строке (`"'db' in covens"`, `"coven == 'prod'"`); подробно — [templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум). |
| `vars.*` | destiny-локалы из [`vars.yml`](vars.md) + task-level | scenario-локалы (scenario-`vars.yml`) + task-level — двухуровневый резолв, см. [scenario/orchestration.md](../scenario/orchestration.md). |

Перечисленные «НЕТ» относятся именно к destiny-контексту. Scenario-контекст, оркестрационные ключи (`on:`/`where:`/`apply:`) и cross-host механизмы специфицированы в [`docs/scenario/`](../scenario/README.md) — не дублируются здесь.

Точный шаблонизатор зафиксирован [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов): CEL для YAML-выражений (top-level expression-keys без обёртки + `${ … }`-интерполяция в строках), Go text/template + sprig-allowlist для файлов `.tmpl` (рендер через `core.file.rendered`). Нормативная спека — [`docs/templating.md`](../templating.md). Тот же engine обслуживает и scenario ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

## 11. Полный композитный пример

```yaml
# redis/tasks/main.yml — диспетчер
- name: Apply Redis configuration
  include: apply.yml
  when: input.action == 'apply'
```

```yaml
# redis/tasks/apply.yml
- name: Install redis-server package
  module: core.pkg.installed
  retry: { count: 3, delay: 10s }      # сеть может моргать
  params:
    name: "${ vars.redis_unit_name }"
    version: "${ input.version }"

- name: Render redis.conf from template
  module: core.file.rendered             # ADR-010: рендер .tmpl делает core.file.rendered
  register: redis_conf
  params:
    path: "${ vars.redis_conf_path }"
    template: templates/redis.conf.tmpl
    vars:
      maxmemory: "${ input.maxmemory }"  # CEL → text/template-контекст (см. templating.md §6)
    mode: "0640"
    owner: "${ vars.redis_user }"
    group: "${ vars.redis_group }"

- name: Ensure redis-server is running and enabled at boot
  module: core.service.running
  params:
    name: "${ vars.redis_unit_name }"
    enabled: true

- name: Restart redis-server because config changed
  module: core.service.restarted
  onchanges: [redis_conf]
  timeout: 30s
  params:
    name: "${ vars.redis_unit_name }"

- name: Apply ACL for each redis user
  module: core.exec.run
  loop:
    items: "${ input.users }"
    as: user
  params:
    command: "redis-cli ACL SETUSER ${ user.name } ${ user.acl }"
    no_log: true
```

## 12. Open Q

Решаются перед или во время имплементации, фиксируются правкой этого документа.

### Coverage и testing
- **Coverage-семантика `loop:`.** Каждая итерация — отдельный coverage-hit, или «задача с loop = один hit при N≥1»? Влияет на формулу coverage в [testing.md](testing.md).
- **Max length для `name:`.** Конкретное число + что делать с превышением (warn / error).

### Parallel
- **Cancel on first failure.** Когда parallel-задача упала, отменять ли остальные in-flight через host-side cancel, или дать им доработать (текущее поведение)?
- **Лимит concurrency.** Стоит ли вводить max-concurrent-tasks для destiny (защита от шквала тредов)? Сейчас лимита нет.
- **Final-barrier timeout.** Глобальный лимит на финальный wait в конце destiny. Сейчас отсутствует — destiny ждёт parallel-задачи неограниченно (модули сами по `timeout:`-задачи могут сдаться).
- **`require: all` scope.** Сейчас «все активные parallel-задачи, запущенные ранее в этом прогоне». Уточнить: засчитывать ли уже завершённые (для consistency, чтобы failure-state был доступен), или только in-flight? Сейчас предполагается только in-flight.
- **Адресация конкретной итерации в `loop` + `parallel:`.** Сейчас ссылка на `register.<name>` ждёт всю задачу (все итерации). Если кто-то хочет дождаться конкретного `register.<name>[i]` — должен ли это быть локальный barrier на итерации? Или это превращает loop в DAG, что мы намеренно избегаем?

### Requisites
- **`prereq:`** (Salt) — обратное `require:` (А ждёт изменений в Б; если Б собирается меняться, А выполняется первым). Нужно ли.
- **Wildcard в requisites.** `onchanges: [redis_*]` — все register-id-ы по префиксу. Удобно, но усложняет валидацию.
- **Cross-file адресация.** `onchanges: [other-file.task]` или строго в пределах одного `tasks/main.yml`-дерева?

### Block — error-handling
- **`rescue:`** (Ansible) — список задач, выполняемых при fail внутри `block:`. Сейчас не закреплён; обходимся `onfail:`-задачами после block-а. Нужно ли вводить как явный синтаксис.
- **`always:`** (Ansible) — список задач, выполняемых независимо от исхода block-а (cleanup, finalize). Тот же вопрос — стоит ли явного синтаксиса.
- **Scope `register:` внутри `block:`.** Виден ли `register:` задач внутри block-а **снаружи** block-а? Сейчас предполагается «да, плоский scope» — но может быть полезно иметь scoped-вариант (внутри block — есть, снаружи — нет).

### Retry / timeout
- **`retry.until:`-полнота.** Какие функции/филтры доступны в `until:`-выражении. Должно совпадать с шаблонизатором.
- **Backoff strategy в `retry:`.** Сейчас — фиксированный `delay:`; могут понадобиться `exponential`, `jitter`.

### Dry-run
- **Переменная `run.mode`.** Нужно ли давать задачам способ узнать, что прогон — dry-run (через `when: run.mode == 'dry_run'` в шаблонах)? Это бэкдор для кастомного поведения, но удобно для destiny с критическими операциями.
- **`Plan`-RPC API.** Точный контракт между host и модулем — описан в [architecture.md](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style), но детали (event-streaming, prediction format, как заглушка должна отвечать) — open Q.
- **Dry-run для requisites.** В dry-run `register.<name>.changed` — это **планируемое** изменение, не фактическое. Триггерит ли это onchanges-handler? Скорее да — оператор должен видеть «restart redis запланирован».

### Changed_when / failed_when
- **`register.self.*` scope.** Сейчас зафиксировано: доступен только в `changed_when:` / `failed_when:`. Распространение на другие поля задачи (например, `output:` через `register.self.*` чтобы публиковать post-overridden `.changed`) — open Q.
- **Влияние `failed_when:` на `retry:`.** Если задача finishes с `failed_when: true`, считается ли это «упала и надо retry»? Сейчас предполагаем — да.

### Task-level vars
- **Композиция с `loop:` — ЗАКРЫТО.** Task-level `vars:` пересчитываются на **каждой** loop-итерации и могут ссылаться на loop-переменную `<as>`/`<index_as>` (vars резолвятся внутри итерации, после раскрытия loop). Зафиксировано в §9 «Ссылки внутри `vars:`».

### Output
- **Output на уровне destiny — ЗАКРЫТО.** Top-level `output:` в `destiny.yml` (декларация схемы, симметрично `input:`) + task-level `output:` (заполнение объявленных полей) приняты как общий механизм. Спецификация — [destiny/output.md](output.md); чтение из scenario через `register:` на applier-задаче — [scenario/orchestration.md §2](../scenario/orchestration.md#2-дельта-scenario-относительно-dsl-ядра).
- **Output через `include:`.** Виден ли `output:` задач из подключаемого файла в caller-е?

### `name:` как lint-правило
- Сделать обязательным (error) или оставить рекомендацией (warn). Сейчас рекомендация.

## 13. См. также

- [concept.md](concept.md) — что такое destiny.
- [manifest.md](manifest.md) — `destiny.yml` и раскладка папки.
- [input.md](input.md) — `input:`-контракт.
- [vars.md](vars.md) — `vars.yml`-локалы destiny.
- [testing.md](testing.md) — molecule-style тестирование (включая coverage).
- [architecture.md → «Адресация модулей»](../architecture.md#адресация-модулей) — формат `<namespace>.<module>.<state>`.
- [architecture.md → «Манифест модуля»](../architecture.md#манифест-модуля) — откуда берётся схема `params:` для каждого `module:`.
