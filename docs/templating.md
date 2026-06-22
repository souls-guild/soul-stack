# Шаблонизатор Soul Stack

Нормативная спецификация шаблонизатора для всех YAML-выражений (destiny, scenario, essence, keeper.yml, миграции) и для файловых шаблонов на хосте. Решение зафиксировано в [architecture.md → ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов).

В Soul Stack **два движка**, граница между ними проходит **по файлу**:

- **CEL** (google/cel-go) — все YAML-выражения: top-level expression-ключи (`where:`, `when:`, `changed_when:`, `failed_when:`, `until:`) и интерполяция `${ … }` в строковых контекстах.
- **Go text/template** + sprig-allowlist — рендер файлов с расширением `.tmpl`, выполняется только новым core-модулем [`core.file.rendered`](#6-передача-данных-между-движками-конвейер).

В одном файле работает **только один** движок. CEL никогда не выполняется внутри `.tmpl`, text/template никогда не выполняется внутри `.yml`. Это не пересечение, а последовательная передача данных (см. [§6](#6-передача-данных-между-движками-конвейер)).

## 1. Контекст → движок → маркер

| Контекст | Движок | Маркер |
|---|---|---|
| Top-level expression-ключи (`where:`, `when:`, `changed_when:`, `failed_when:`, `until:`) | CEL (google/cel-go) | вся строка = CEL, без обёртки |
| Интерполяция в строковых контекстах (`params:`, `apply: input:`, `on:`-литералы, `vars:`, `essence/_stack.yaml`-выражения, миграции `set:`) | CEL | `${ … }` |
| Файлы в `templates/<path>.tmpl` | Go text/template + sprig allowlist | `{{ … }}` (Go-синтаксис) |

Граница строгая по расширению файла: `.yml` → CEL, `.tmpl` → text/template.

## 2. CEL в YAML

### 2.1. Top-level expression-ключи

Эти ключи принимают **строку, целиком трактуемую как CEL-выражение** — без обёртки `${ … }`. Все возвращают `bool` (кроме `until:`, см. ниже).

Колонка **«Сторона»** — где вычисляется выражение по границе [ADR-012(d)](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) «по внешнему доступу»: `where:` — Keeper (резолв таргета на render-фазе); `when:`/`changed_when:`/`failed_when:`/`until:` — **Soul** (flow-control: зависят от `register.*` — результатов предыдущих задач, известных только на Soul во время прогона; вычисляются sandboxed cel-go-песочницей `shared/cel.NewFlowControl`, см. [§4](#4-соотношение-фаз-обработки-yaml) и [§7.1](#71-cel--sandbox-by-design)).

| Ключ | Возвращает | Сторона | Где используется | Источник |
|---|---|---|---|---|
| `where:` | bool | Keeper | per-host фильтр таргета шага сценария | [scenario/orchestration.md §4](scenario/orchestration.md#4-волатильный-предикат--where) |
| `when:` | bool | Soul | делать ли шаг вообще (gating ДО Apply) | [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения) |
| `changed_when:` | bool | Soul | определение `changed`-статуса шага по результату | [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения) |
| `failed_when:` | bool | Soul | определение `failed`-статуса шага по результату | [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения) |
| `until:` | bool | Soul | условие выхода из цикла retry | [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения) |

> **Статус реализации.** Soul-side вычисляются `when:` (gating ДО Apply), `changed_when:` и `failed_when:` (override `changed`/`failed` ПОСЛЕ Apply, см. [§4](#4-соотношение-фаз-обработки-yaml) фаза 4a) и `until:` (выход из retry-петли, фаза 4a — после `changed_when`/`failed_when`, [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения)).
>
> **Семантика `changed_when:`/`failed_when:`** (Soul, ПОСЛЕ Apply; порядок — сначала `changed_when`, потом `failed_when`):
> - `changed_when:` → override `changed`: была CHANGED + предикат `false` → OK; была OK + предикат `true` → CHANGED. Не трогает `failed`. `changed_when: false` на probe — задача никогда не `changed` (не триггерит `onchanges:`).
> - `failed_when:` → override `failed`: `true` при OK-модуле → FAILED (искусственный провал по бизнес-условию, напр. `failed_when: register.self.exit_code != 0`); **`failed_when: false` при упавшем модуле = ignore_errors** — статус НЕ FAILED (OK/CHANGED), прогон НЕ останавливается (fail-stop не срабатывает), а исходная ошибка модуля СОХРАНЯЕТСЯ как информационная (`register.<name>.ignored_error` + `TaskEvent.error` без FAILED-статуса). `failed` приоритетнее `changed` (FAILED перекрывает CHANGED).
> - **НЕ применяется к `TIMED_OUT`:** таймаут инфраструктурный, остаётся терминальным fail-stop; `failed_when: false` его НЕ глушит.
>
> **Семантика `until:`** (Soul, ПОСЛЕ `changed_when`/`failed_when` каждой попытки retry-петли; полная спека — [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения)): `until:` — **условие выхода из retry**, не override статуса. `until`-true → выход, статус попытки остаётся как есть (`until` НЕ override-ит `failed`: truthy-until на FAILED-попытке → финал FAILED). `until`-false → пауза `retry.delay` → следующая попытка; после `count` попыток с `until`-false → задача FAILED (`flowcontrol.until_exhausted`), даже если попытка OK/CHANGED. На **TIMED_OUT**-попытке `until` НЕ вычисляется (попытка ретраится, если попытки остались). Активация — та же, что у `failed_when` (`flow_context` + `register.*` предыдущих + `register.self.*` свежей попытки с применёнными `changed`/`failed`).

Пример:

```yaml
- name: restart master
  module: core.service.restarted
  params: { name: redis }
  where: register.role.stdout == "master"
  when: input.do_restart
```

### 2.2. Интерполяция `${ … }` в строковых контекстах

В любых строковых полях YAML (`params:`, `vars:`, `apply: input:`, `on:`-литералы, `essence/_stack.yaml`-выражения, миграционные `set:`) CEL-выражение оборачивается в `${ … }`.

**Правила парсинга:**

- Маркер открытия — **ровно** последовательность `${`. Одиночный `$` маркером не является и в дальнейшую обработку не идёт (`price: "$100"` остаётся литералом).
- Маркер закрытия — первая `}` на том же уровне вложенности скобок CEL-выражения. Скобки `()`, `[]`, `{}` внутри `${ … }` балансируются парсером CEL, не текстовым сканером.
- Внутри `${ … }` строковые литералы CEL — **одинарные кавычки** (`'primary'`), потому что внешняя YAML-обёртка занимает двойные.
- Многострочные значения с CEL — через YAML-блок `>` или `|`, либо через явное соединение в `vars:`.

Пример:

```yaml
params:
  name: redis
  command: "redis-cli replicaof ${ register.master.stdout } 6379"
  replicas: "${ input.replicas * 2 }"
```

### 2.3. Зарегистрированные CEL-функции (стартовый минимум)

Стартовый минимум — точный список зреет вместе с pilot-имплементациями. **Расширение списка — через issue / ADR, не молча.** Custom-функции третьей стороной — отложены (см. [§11](#11-см-также)).

| Сигнатура | Назначение | Пример |
|---|---|---|
| `size(x) -> int` | Размер строки/списка/map. | `size(input.hosts) > 0` |
| `contains(s, sub) -> bool` | Подстрока в строке или элемент в списке. | `contains(register.role.stdout, "master")` |
| `s.matches(regex) -> bool` | Regex-матчинг строки (RE2, stdlib CEL, parity SaltStack `-E`). Member-форма. | `input.sid.matches("^db-[0-9]+$")` |
| `s.glob(pattern) -> bool` | Shell-glob матчинг (`*`/`?`/`[abc]`/`[a-z]` через [filepath.Match](https://pkg.go.dev/path/filepath#Match), parity SaltStack `-G`). Member-форма. Битый pattern → `false` без ошибки (per-host предикат `target.where` не должен валиться на отдельном хосте; синтаксис валидирует `soul-lint`). Недоступна в migration-CEL ([ADR-019], sandbox). | `input.sid.glob("prod-*")` |
| `now() -> timestamp` | Возвращает текущее время на момент вычисления выражения (eval-time на каждое обращение, не start-time прогона). | `now() - register.deployed_at > duration("1h")` |
| `duration(string) -> duration` | Конструктор `duration` из строки (например, `"1h"`, `"30s"`). Используется с `now()` и арифметикой времени. | `duration("30s")` |
| `vault(path) -> map / value` | **Keeper-side** чтение секрета Vault KV в CEL-render-фазе (фаза 3, см. [§4](#4-соотношение-фаз-обработки-yaml)). Без `#field` возвращает весь map секрета — поле берётся CEL-доступом `.field`; с `#field` в пути — одно значение напрямую. Реальное значение подставляется в params и уходит на Soul; маскируется только на выходе (логи/OTel/UI). `path` — строковый литерал ИЛИ CEL-выражение из **доверенного** контекста (`incarnation`/`vars`, не operator-`input`); резолвится CEL-ом до чтения Vault, инъекции в Vault-запрос нет. См. [ADR-017](adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read). | `${ vault('secret/redis/admin').password }` или `${ vault('secret/redis/admin#password') }` |
| `merge(m, m...) -> map` ∪ `merge(list(map)) -> map` | Слияние map-ов слева направо по ключу **верхнего уровня** — **SHALLOW** (вложенный map НЕ сливается глубоко: правый целиком замещает совпавший верхний ключ), **last-wins** (правый перекрывает левый). Pure (без I/O/секретов/крипты). Две формы: **varargs** `merge(m, m...)` (≥1 map-аргумент) и **`merge(list(map))`** (ОДИН аргумент-список map-ов, flatten слева направо — для коллекции из `.map(...)`-comprehension, которую в шаблон надо отдать map-ом ради детерминизма порядка, см. [§6](#6-передача-данных-между-движками-конвейер)). Не-map аргумент / элемент → ошибка; пустой список → пустой map. Slot для трансляции «простой типизированный `input` → детальный конфиг»: слить авторский пресет с passthrough-`input`-map. Недоступна в migration-CEL ([ADR-019], sandbox). См. [ADR-010 Amendment 2026-06-22](adr/0010-templating.md). | `${ merge(essence.redis.defaults, input.redis_settings) }`; `${ merge(input.users.map(n, {n: input.users[n]})) }` |
| `soulprint.self.<path>` | Поля Soulprint текущего хоста. | `soulprint.self.os.family == "debian"` |
| `soulprint.hosts -> list` | Список хостов прогона со стабильными фактами (scenario-only, см. [orchestration.md §4.1](scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор)). | `soulprint.hosts.size()` |
| `soulprint.where(<predicate>) -> list` | Хосты, удовлетворяющие предикату-**строке** (CEL над стабильным слоем — `covens`/`os.*`/`network.*`); role — declared, доступна через `soulprint.hosts.where(...)` и только в bootstrap-create, см. [orchestration.md §4.1](scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор). Предикат — **статический строковый литерал**, раскрываемый на compile-фазе во встроенный CEL filter-comprehension (не runtime-исполнение строки). Keyword-args (`coven=...`) не поддерживаются (CEL не имеет keyword-args). | `soulprint.where("'db' in covens")[0].network.primary_ip` |
| `register.<name>.<path>` | Результаты `register:` предыдущих шагов; `register.self.*` — текущий хост в scenario-контексте. | `register.probe.exit_code == 0` |
| `input.<path>` | Значения блока `input:` сценария/destiny. | `input.replicas` |
| `essence.<path>` | Значения собранного essence. | `essence.redis.maxmemory` |
| `incarnation.<path>` | Поля incarnation (`name`, `service_version`, `spec.*`). | `incarnation.name` |
| `vars.<path>` | Локальные task-level и destiny-level `vars:`. | `vars.master_ip` |

> Допустимые предикаты `soulprint.where(...)` — `covens` / `sid` / `network.*` / `os.*`. Роль (`role`) в этом аксессоре **недоступна**: declared-роль — только через `soulprint.hosts.where(...)` (и только для bootstrap-create), волатильная роль — только через probe + `where:`-ключ. См. [scenario/orchestration.md §4](scenario/orchestration.md#4-волатильный-предикат--where) и [ADR-008](adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги).

> **Предикат `.where(...)` — статический строковый литерал, не runtime-строка.** Он раскрывается на **compile-фазе** валидации: всё выражение парсится в AST, вызовы `.where("<pred>")` на `soulprint.hosts`/`soulprint.where(...)` переписываются в нативный CEL filter-comprehension (`soulprint.hosts.filter(<iter>, <pred>)`), где поля предиката (`role`/`covens`/`os.*`/…) квалифицируются полем элемента, а внешний контекст (`incarnation.*`/`input.*`/…) остаётся как есть. Дерево компилируется один раз. Следствия: предикат **обязан** быть строковым литералом — динамическая склейка (`"'" + incarnation.name + "' in covens"`) запрещена (понятная compile-ошибка «predicate must be a static string literal»); `.where(...)` разрешён **только** на `soulprint.hosts`/`soulprint.where(...)` (generic `.where` на произвольном списке — ошибка валидации); вложенный `.where(...)` внутри предиката не поддержан. Первый элемент результата — `[0]` (нативная индексация), `.first` не вводится.

> Внутри предиката доступны comprehension-макросы CEL — `exists`/`all`/`exists_one`/`map`/`filter` (например идиоматичный фильтр по списку `covens.exists(c, c == 'db')`), и макрос может стоять рядом с `.where(...)` в одном выражении (`size(soulprint.hosts.where("role == 'replica'")) > 0 && input.xs.exists(x, x == 2)`). Rewrite-фаза парсит выражение без раскрытия макросов (чтобы переписанное дерево round-trip'ировалось обратно в строку), а финальная компиляция раскрывает `.filter`/`.exists`/… нативно. iter-переменные таких макросов (`c`/`x`) — локальные: они **не** квалифицируются полем элемента `.where`.

### 2.4. Type model

CEL компилируется с известными типами переменных контекста — это основа статической проверки в `soul-lint`.

| Контекст | Источник типов |
|---|---|
| `input.*` | блок `input:` destiny/scenario ([docs/input.md](input.md)) |
| `essence.*` | схема essence сервиса |
| `incarnation.spec.*` | `state_schema` в `service.yml` |
| `register.<name>.*` | для core — встроенные output-схемы модулей; для custom — манифест модуля |
| `soulprint.self.*`, `soulprint.hosts[].*` | спека Soulprint (open Q №6 — до её закрытия типы `dyn`, после — конкретные) |
| `vars.*` | вывод типов из RHS выражения, объявившего `vars:` |

При отсутствии информации о типе — узел получает тип `dyn`. Это не ошибка: CEL продолжает компилироваться и работать, но статическая проверка для этого узла теряется. `soul-lint` поднимает warn-уровень.

### 2.5. Compile-cache

CEL-выражение компилируется **один раз** на пару `(scenario, scenario_version)` и eval-ится многократно с разными activation-ами (per-host, per-iteration loop, per-retry). Compile-cache обязателен — без него каждое `where:` платит парсинг + type-checking на каждый шаг каждого хоста.

Ключ кеша: `(scenario_id, scenario_git_ref, normalized_expr)`. Инвалидация — естественная при смене git ref (новая версия — новый ключ).

## 3. Go text/template в файлах `.tmpl`

text/template — **только** для рендера файлов в `templates/<path>.tmpl` модулем `core.file.rendered`. В YAML text/template не используется.

### 3.1. Strict mode

Рендер запускается с `template.Option("missingkey=error")`. Обращение к отсутствующему полю (`{{ .vars.missing }}`) — **ошибка рендера**, шаг падает штатно. Strict-mode — защита от опечаток в именах полей (отсутствующее поле в контексте → ошибка рендера, а не пустая строка). Вклад в защиту от SSTI см. [§7.3](#73-ssti-через-данные).

### 3.2. Контекст рендера

Контекст рендера `.tmpl` — **изолированный**, не глобальный. text/template не видит `essence.*`, `register.*`, `soulprint.*`, `input.*` напрямую — только явно поднятые автором значения (`params.vars`) плюс фиксированный набор системных полей.

**Корень контекста** — ровно `{ vars, self, role, essence }` (обращение к чему-то ещё в корне → strict-mode ошибка). Этот корень собирает **Keeper-side, per-host** и доставляет рядом с `template_content` (см. [§4](#4-соотношение-фаз-обработки-yaml) и [ADR-012(d)](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)); Soul передаёт его движку **корнем**, поэтому шаблон обращается `.vars.<name>`, `.self.<path>`, `.role`, `.essence.<path>`.

| Поле корня | Содержание |
|---|---|
| `vars.*` | CEL-rendered значения, явно поднятые автором в `params.vars` шага `core.file.rendered` ([§6](#6-передача-данных-между-движками-конвейер)). Единственный канал «прокинуть данные из YAML в шаблон». |
| `self.network.*` | сетевые факты хоста (адреса, интерфейсы): `self.network.primary_ip`, `self.network.interfaces[]` |
| `self.os.*` | os.family, os.version, distribution; составные ключи **snake_case**: `self.os.pkg_mgr`, `self.os.init_system` |
| `self.sid` | SID текущего хоста |
| `role` | declared-роль из `incarnation.spec.hosts[].role` (**bootstrap-create only**: probe ещё невозможен; в runtime-операциях declared-роль НЕ используется, актуальная роль берётся probe-ом + `register.*`, [ADR-008](adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). Может быть пусто. |
| `essence.*` | собранный essence (read-only snapshot, переданный модулю) |

`self` — **та же** `soulprint.self`-проекция хоста, что доступна в CEL-фазе (`soulprint.self.<path>` в YAML ≡ `.self.<path>` в шаблоне, единая точка правды [ADR-018](adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)). Из этого следует: **ключи `.self.*` — snake_case** (proto field names), а не camelCase. Составные ключи пишутся через `_`: `.self.os.pkg_mgr`, `.self.os.init_system`, `.self.network.primary_ip` — буквально как в CEL `soulprint.self.os.pkg_mgr`. camelCase-форма (`.self.os.pkgMgr`) — ошибка (`map has no entry for key "pkg_mgr"` при strict-mode не возникнет, но значение не найдётся).

> **`soulprint.self.*` доступен и в CEL-проходе destiny** (ADR-009/ADR-010 amendment 2026-06-18). Симметрия `.tmpl ↔ .yml` распространяется на destiny-проход: и `render_context.self`, и `soulprint.self.<path>` в `.yml`-выражениях destiny берут один и тот же per-host стабильный слой целевого хоста. Граница изоляции destiny проходит по **self vs топология прогона**: `soulprint.hosts`/`soulprint.where(...)` (cross-host) остаются scenario-only (см. [§7.1](#71-cel--sandbox-by-design)).

> **Шаблонный контекст НЕ host-инвариантен.** `self` per-host, поэтому self-зависимый шаблон даёт у каждого хоста СВОЙ корень контекста — Keeper собирает `render_context` для каждого таргетированного хоста отдельно ([ADR-012(d)](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Это легитимно: host-инвариантность требуется от ПРОЧИХ params шага, но не от `template_content`/`render_context`.

> Точный набор системных полей фиксируется в спеке `core.file.rendered`. Здесь — нормативный минимум.

### 3.3. Sprig allowlist

#### Builtin Go text/template

Помимо разрешённого подмножества sprig, в шаблонах доступны стандартные builtin-функции Go text/template: `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `and`, `or`, `not`, `index`, `len`, `print`, `printf`, `println`. Они не входят в sprig allowlist потому, что они часть самого движка.

#### Sprig allowlist

Sprig подключается **через whitelist**, не через denylist. Allowlist — закрытый список ниже. При upgrade sprig — allowlist **пересматривается явно**, новые функции по умолчанию **запрещены**.

**Разрешено (стартовый минимум):**

- **Nil-handling:** `default`, `coalesce`, `empty`.
- **Строки:** `upper`, `lower`, `trim`, `trimAll`, `trimPrefix`, `trimSuffix`, `quote`, `squote`, `replace`, `repeat`, `split`, `splitList`, `join`.
- **Конверсия:** `toString`, `int`, `int64`, `float64`, `toJson`, `fromJson`.
- **Арифметика:** `add`, `sub`, `mul`, `div`, `mod`.
- **Base64 / хэш (без секретогенерации):** `b64enc`, `b64dec`, `sha256sum`.

**Запрещено явно (denylist для документации — даже если sprig обновится и добавит alias):**

- **Доступ к окружению/исполнение/сеть:** `env`, `expandenv`, `exec`, `getHostByName`.
- **Криптогенерация (не нужна для конфигов, тащит скрытые риски):** `derivePassword`, `genCA`, `genPrivateKey`, `genSelfSignedCert`, `genSignedCert`, `buildCustomCert`.
- **Случайность (недетерминизм в рендере конфига — баг):** `randAlphaNum`, `randAlpha`, `randAscii`, `randNumeric`, `randBytes`.
- **Метапрограммирование (вектор SSTI):** `tpl`, `include` (sprig-вариант).

Любая функция, не входящая в whitelist — недоступна (вызов = ошибка рендера). Расширение whitelist — отдельная задача, не молча.

#### Собственные функции Soul Stack (не sprig)

`toYaml` и `fromYaml` — **не функции sprig** (в upstream sprig их нет, это Helm-only функции). В Soul Stack они реализованы как собственные функции через goccy/go-yaml и добавляются в FuncMap отдельно от sprig-allowlist-а (`shared/tmpl/yaml_funcs.go`). YAML-выражений в YAML-источниках они **не касаются** — это движок text/template для файлов `.tmpl`.

| Функция | Сигнатура | Поведение |
|---|---|---|
| `toYaml` | `toYaml(v any) -> string` | Сериализует значение в YAML. Хвостовой `\n` срезается (результат обычно встраивается в более крупный YAML). Ошибка сериализации **проваливает рендер** (в отличие от Helm-варианта, глотающего ошибку в пустую строку — молчаливая подстановка мусора в конфиг опаснее упавшего шага, [§10](#10-поведение-на-ошибках-и-диагностика)). |
| `fromYaml` | `fromYaml(s string) -> any` | Парсит YAML-строку в структуру (map/list/scalar) для дальнейшей индексации в шаблоне. Ошибка парсинга проваливает рендер. |

Пример (render фрагмента YAML-конфига из переданного в `vars` значения):

```
# templates/app.conf.tmpl
extra_settings:
{{ toYaml .vars.extra }}
```

```yaml
# scenario/destiny
- name: render app config
  module: core.file.rendered
  params:
    path: /etc/app/app.conf
    template: templates/app.conf.tmpl
    vars:
      extra: "${ essence.app.extra_settings }"
```

### 3.4. Расширение `.tmpl` — обязательное

Файл, отдаваемый в `core.file.rendered.params.template`, обязан иметь расширение `.tmpl`. Без расширения — ошибка валидации destiny на `soul-lint`. Расширение служит и маркером «это шаблон» для оператора, и фильтром при сканировании репо.

Историческое `.j2` не используется. Sweep по примерам — отдельным этапом после ADR-010.

## 4. Соотношение фаз обработки YAML

Все YAML-источники (scenario, destiny, essence, keeper.yml, migrations) проходят через одну и ту же конвейерную обработку. Порядок фиксирован, фазы не перемешиваются.

Граница фаз — **по внешнему доступу** ([ADR-012(d)](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)): кто ходит во внешние системы (Vault/реестр/CEL-контекст) — Keeper; локальный compute без I/O — Soul. Фазы 1–3 + доставка literal `.tmpl` — **Keeper-side**; фазы 4 (text/template-COMPUTE) и 4a (flow-control CEL) — **Soul-side**.

1. **vault-resolve** (Keeper). Все `vault:`-ссылки-строки в params заменяются на значения **до** входа в CEL. `${ … }` в самих vault-ссылках **запрещён**: vault-ссылка — строковый литерал, резолвится фазой раньше CEL. Любое `vault: "secret/foo/${ … }"` — ошибка валидации. (Это `vault:`-**ref**-форма; CEL-функция `vault(...)` — отдельный механизм фазы 3, см. ниже.)
2. **input-resolve** (Keeper). Эффективный operator-`input` по контракту destiny/scenario, под-порядок строго: **merge дефолтов + required → scoped input-vault-resolve → value-валидация**. Шаг scoped input-vault-resolve — **отдельный от авторского (фаза 1) ограниченный канал**: значение secret-поля с объявленным `vault_scope` может быть `vault:`-ref, который Keeper резолвит keeper-side с проверкой scope + hard deny-list (`secret/keeper/*`/`secret/internal/*`), резолв аудируется (`input.vault_resolved`), значение секрета не логируется. Default-deny: `vault:`-ref в поле без `vault_scope` — ошибка. `pattern`/`enum`/`min_length` проверяются на **уже резолвнутом** значении (потому value-валидация — после vault-resolve). Полная спека — [docs/input.md → «vault_scope»](input.md#vault_scope-scoped-резолв-vault-ref-в-operator-input). Граница каналов: авторский `vault:`-ref в `params:` (фаза 1) и `${ vault(...) }` (фаза 3) — доверенный канал автора сервиса, `vault_scope` им не нужен и deny-list operator-канала на них не распространяется.
3. **CEL-render** (Keeper). Top-level expression-ключ `where:` (резолв таргета) и все интерполяции `${ … }` (params/vars/on) вычисляются. Non-string результаты подставляются по правилу [§5](#5-non-string-cel-результат-в-yaml). Здесь же резолвится **CEL-функция `vault(path)`** ([§2.3](#23-зарегистрированные-cel-функции-стартовый-минимум)) — keeper-side чтение Vault KV: реальное значение секрета подставляется в params и уходит на Soul (маскируется только на выходе — логи/OTel/UI). В отличие от `vault:`-ref (фаза 1, статическая строка), `vault(...)` принимает путь как CEL-выражение из доверенного контекста и доступен в любой `${ … }`-ячейке. Внешний доступ (Vault) — целиком Keeper-side. Flow-control-ключи `when:`/`changed_when:`/`failed_when:` здесь **НЕ вычисляются** — Keeper протягивает их в `RenderedTask` как CEL-строки (eval — фаза 4a, Soul); Keeper также собирает `flow_context` (см. ниже).
   - **Доставка шаблона + сборка корня контекста** (Keeper, между фазами 3 и 4). Для шага `core.file.rendered` Keeper: (1) читает literal-содержимое `templates/<path>.tmpl` из снапшота (двухуровневый резолв scenario-local→service-level, [ADR-009](adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) и кладёт его в `params.template_content`; (2) собирает **per-host** корень text/template-контекста `{ vars, self, role, essence }` ([§3.2](#32-контекст-рендера)) и кладёт его в `params.render_context`. Путь-ключ `template` и плоский `vars`-ключ из params **удаляются** (Soul читает корень только из `render_context`). text/template здесь **не исполняется** — Keeper доставляет шаблон as-is. A1-вариант: и `template_content`, и `render_context` едут внутри `RenderedTask.params`, без изменений proto. `render_context` host-вариативен (`self` per-host) — он исключён из per-host-сверки host-инвариантности params.
   - **Сборка `flow_context`** (Keeper, для каждой задачи). Литеральный per-host снапшот не-register части CEL-контекста flow-control-предикатов `{ input, vars, essence, incarnation, self }` (то же, что для рендера params, минус `soulprint.hosts` и loop) кладётся в `RenderedTask.flow_context`. `register.*` в него НЕ входит — его Soul строит сам на фазе 4a. Host-вариативен (`self` per-host), исключён из per-host-сверки host-инвариантности params.
4. **text/template-render** (Soul, в `core.file.rendered`). Рендер `template_content` (literal `.tmpl` от Keeper) с `render_context` **корнем** (собран на фазе 3) — см. [§3.2](#32-контекст-рендера) и [§6](#6-передача-данных-между-движками-конвейер). Это **локальный compute без I/O**: Soul тянет только `shared/tmpl` (text/template + sprig-allowlist), внешнего доступа (Vault/сеть/FS-чтение) не требует. Три sandbox-барьера ([§«Обоснование» ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов): strict-mode, sprig-allowlist без `exec`/`env`/чтения FS-сети, изолированный контекст рендера) **сохраняются на Soul**.
4a. **flow-control CEL** (Soul, gating ДО Apply + override ПОСЛЕ Apply). Перед `module.Apply` Soul вычисляет предикат `when:` урезанной cel-go-песочницей (`shared/cel.NewFlowControl`, [§7.1](#71-cel--sandbox-by-design)). Активация: `register.*` (результаты предыдущих задач прогона по register-имени, Soul строит сам) + `flow_context` от Keeper (`input`/`vars`/`essence`/`incarnation` top-level, `soulprint.self` ← `flow_context.self`). `when:false` → задача SKIPPED (Apply не вызывается); связка с `onchanges:` — AND (исполняется только при `when && onchanges-satisfied`). **Локальный compute без I/O**: `vault()`/`now()`/`soulprint.hosts`/`soulprint.where` в песочнице недоступны конструктивно.
   - **`changed_when:`/`failed_when:` — ПОСЛЕ `module.Apply`** (override `changed`/`failed` по результату), той же песочницей и активацией, ПЛЮС `register.self.*` — свежий результат текущей задачи (`changed`/`failed`/`timed_out` + `output:`-поля из ApplyEvent). Порядок: сначала `changed_when` (определяет `changed`), потом `failed_when` (определяет `failed`); `failed` приоритетнее (FAILED перекрывает CHANGED). `failed_when: false` на упавшем модуле = ignore_errors (статус НЕ FAILED, прогон не ломается, исходная ошибка сохраняется в `register.<name>.ignored_error` + `TaskEvent.error`). `TIMED_OUT` обрабатывается ДО этого шага и `failed_when` к нему НЕ применяется. Runtime-error CEL в `changed_when:`/`failed_when:` (например, `register.self.<опечатка>`) → задача FAILED ([§10](#10-поведение-на-ошибках-и-диагностика)), как у `when:`.
   - **`until:` — ПОСЛЕ `changed_when`/`failed_when`** (выход из retry-петли, [destiny/tasks.md §9](destiny/tasks.md#9-прочность-и-контроль-исполнения)), той же песочницей и активацией, что `failed_when` (`register.self.*` с уже применёнными `changed`/`failed`). `until`-true → выход (статус попытки как есть, без override); `until`-false → пауза `retry.delay` → следующая попытка; исчерпание `retry.count` с `until`-false → FAILED (`flowcontrol.until_exhausted`). На TIMED_OUT-попытке `until` НЕ вычисляется. Вся retry-петля (включая `until`-eval и `delay`) — Soul-side, `delay` прерывается отменой прогона.
5. **module.Apply** (Soul). Модуль получает финальные параметры (только если задача не отсеяна фазой 4a / onchanges).

> **Ограничение pilot (flow-control host-инвариантность).** Flow-control-предикаты (`when:`/`changed_when:`/`failed_when:`) должны быть host-**инвариантны** на multi-host таргете. Причина: dispatch-модель pilot раздаёт ОДИН `RenderedTask` (с `flow_context` первого хоста) на всю targeted-группу, поэтому host-вариативный предикат молча вычислился бы по фактам первого хоста для всех. Защита — **два контура fail-closed**, оба временные до per-host dispatch (отдельный ADR):
>
> 1. **Прямая ссылка на `soulprint.self` в тексте предиката** — отсекается regex-guard по тексту `when:`/`changed_when:`/`failed_when:`. Ссылка на `soulprint.self` допустима **только** при single-host таргете; на multi-host рендер падает fail-closed. Host-инвариантные ссылки (`register.*`/`input.*`/`essence.*`/`incarnation.*`) работают всегда. Симметрично ограничению `loop.when` (`reLoopWhenSoulprint`).
> 2. **Производный host-вариативный `vars`** — обход первого контура: значение `vars`, производное от `soulprint.self` (например `vars: { is_debian: "${ soulprint.self.os.family == 'debian' }" }` + `when: vars.is_debian`), протекает в `flow_context.vars`, а текст предиката soulprint не содержит — regex-guard его не ловит. Ловится сверкой собранного `flow_context`-**минус-`self`** между targeted-хостами: `input`/`essence`/`incarnation` host-инвариантны по построению, `self` host-вариантен по природе (его закрывает контур 1), остаётся `vars` — если он различается между хостами, рендер падает fail-closed. Сверка активна только при наличии непустого flow-control-предиката (без него Soul `flow_context` не читает; host-вариативный `vars`-в-params без `when` падает на отдельной проверке host-инвариантности params).

> **Caveat (path-injection в `vault()`).** Путь функции `vault(path)` обязан приходить из **доверенного** контекста (литерал, `incarnation`, `vars`). Путь, склеенный из operator-`input` — например `vault('secret/' + input.tenant + '/db')` — позволяет оператору навести `vault()` на **произвольный** секрет KV, на который у него по контракту доступа быть не должно. Это **контрактное допущение, а не баг**: по принятому варианту (а) ответственность за то, чтобы путь не был производным от `input`, лежит на авторе scenario/destiny, плюс RBAC на секрет-пути в самом Vault (узкие политики токена Keeper). Статический запрет input-производных путей в `vault()` — отдельная задача (если security сочтёт нужным усилить). Никакой текстовой инъекции в Vault-запрос при этом нет: путь — это CEL-значение, вычисленное до `ReadKV`, а не строковая склейка в протокол Vault.

## 5. Non-string CEL результат в YAML

Когда `${ … }` возвращает не-строку (int/bool/list/map/timestamp), поведение зависит от того, **что вокруг** маркера в YAML-ячейке:

**(а) Ячейка состоит ровно из одного `${ … }` без сопровождающего текста.** Результат подставляется в **нативном YAML-типе** (int → int, list → list, …).

```yaml
count: "${ input.replicas * 2 }"
# → count: 4   (int)

hosts: "${ soulprint.where(\"'db' in covens\") }"
# → hosts: [ {sid: ..., network: {...}}, ... ]   (list)

redis_config: "${ merge(essence.redis.defaults, input.redis_settings) }"
# → redis_config: { maxmemory: ..., save: ..., ... }   (map)
```

Результат `merge(...)` — **map**, поэтому подчиняется правилу (а): в ячейке-одиночке подставляется нативной структурой; склейка map со строкой по правилу (б) — ошибка (нужен вынос в отдельную ячейку).

**(б) В ячейке есть текст рядом с `${ … }`.** Результат **стрингифицируется** и склеивается со строкой.

```yaml
command: "redis-cli replicaof ${ register.master.stdout } 6379"
# → command: "redis-cli replicaof 10.0.0.5 6379"   (string)
```

Стрингификация — каноничная: `int`/`float`/`bool` → их строковое представление; `timestamp` → ISO-8601; `list`/`map` → ошибка валидации (склеить структуру со строкой нельзя, нужен либо явный `toJson`-аналог, либо вынос в отдельную ячейку под правилом (а)).

## 6. Передача данных между движками (конвейер)

Движки **не пересекаются** — они последовательно передают данные (и работают на разных сторонах, [ADR-012(d)](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)):

1. **CEL в YAML (Keeper)** вычисляет значения `params.vars` шага `core.file.rendered`. CEL имеет полный контекст scenario (`input`, `essence`, `register`, `soulprint`, `vars`). Keeper же доставляет literal `.tmpl` в `params.template_content`.
2. **text/template в `.tmpl` (Soul)** получает корень `{ vars, self, role, essence }` ([§3.2](#32-контекст-рендера)): `vars` — CEL-rendered `params.vars`, `self`/`role`/`essence` — системные поля, собранные Keeper-ом per-host (`render_context`). Контекст text/template **не содержит** прямого доступа к `input`/`register`/`soulprint.hosts` — только то, что автор явно поднял в `vars` на Keeper-фазе, плюс узкий системный набор. Soul рендерит `template_content` локально, без обращения к внешним системам.

Пример (runtime-операция, master определяется живым probe):

```yaml
- name: probe actual redis role
  on: ["${ incarnation.name }"]
  module: core.exec.run
  register: redis_role
  changed_when: false
  failed_when: size(register.redis_role) < size(soulprint.hosts)
  params:
    command: "redis-cli role | head -1"

- name: capture master address
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'master'
  module: core.exec.run
  register: master_addr
  changed_when: false
  params:
    command: "hostname -i"

- name: render redis.conf on each host
  on: ["${ incarnation.name }"]
  where: register.redis_role.stdout == 'slave'
  module: core.file.rendered
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    vars:
      master_ip: "${ register.master_addr.stdout }"
      maxmemory: "${ essence.redis.maxmemory }"
```

Внутри `templates/redis.conf.tmpl`:

```
maxmemory {{ .vars.maxmemory }}
replicaof {{ .vars.master_ip }} 6379
```

#### Bootstrap-create variant

```yaml
# Bootstrap create: redis ещё не запущен, probe невозможен.
# Топология берётся из declared-роли через scenario-only аксессор
# soulprint.hosts и пробрасывается в destiny через apply: input:.
- name: configure redis on each declared host
  on: ["${ incarnation.name }"]
  apply: destiny/redis-configure
  input:
    role: "${ soulprint.hosts.where(\"sid == soulprint.self.sid\")[0].role }"
    master_ip: "${ soulprint.hosts.where(\"role == 'primary'\")[0].network.primary_ip }"
    replicas: "${ input.replicas }"
```

Runtime-операция использует probe+register (волатильная роль фиксируется живым опросом); bootstrap-create использует `soulprint.hosts.where(...)` (declared-роль из spec, когда probe ещё невозможен). Не путайте контексты — это footgun уровня архитектуры ([ADR-008](adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)).

Один файл = один движок. Один шаг = один переход CEL → text/template (или вообще без text/template, если шаг не `core.file.rendered`).

#### Коллекции в шаблон — map-ом, не list-ом, если важен детерминизм порядка строк (нормативно)

Go text/template `range` по **map** обходит ключи в **отсортированном** порядке (детерминированно), а по **list** — в порядке итерации источника, который для коллекции, построенной CEL-comprehension `.map(...)` над map, наследует **недетерминированный** порядок итерации Go-map. Следствие: рендер списка из `input.<коллекция>.map(...)` даёт строки файла в случайном порядке между прогонами → ложный `changed` в `core.file.rendered` → лишний `onchanges`-рестарт сервиса (на rolling-restart флоте — каскадный лишний рестарт).

**Правило:** если в шаблон передаётся коллекция, для которой важен стабильный порядок строк (ACL-файлы, списки нод/sentinel, любой построчный конфиг), передавать её **map-ом** (имя→объект), а в шаблоне `range`-ить с ключом (`{{- range $name, $u := .vars.users }}`), а НЕ списком. Коллекцию из `.map(...)`-comprehension (CEL даёт список) свернуть в map формой `merge(list(map))` ([§2.3](#23-зарегистрированные-cel-функции-стартовый-минимум)): `${ merge(input.users.map(name, {name: {...}})) }`. Передача list-ом допустима только там, где порядок задаётся самим автором (литеральный список) и не наследует итерацию map.

## 7. Security model

### 7.1. CEL — sandbox by design

CEL не имеет syscall, файлового и произвольного сетевого доступа, не выполняет произвольного кода. Регистрируются только наши функции из [§2.3](#23-зарегистрированные-cel-функции-стартовый-минимум) (см. также [§11](#11-см-также)). Custom-функции третьей стороной — отложены.

Единственная функция с I/O — `vault(path)` ([§2.3](#23-зарегистрированные-cel-функции-стартовый-минимум)): контролируемое чтение Vault KV через инъектированный keeper-side клиент (не произвольный сетевой доступ — фиксированный Vault-endpoint из `keeper.yml`). Безопасна по построению: путь — CEL-выражение из доверенного контекста (не operator-`input` — см. caveat в [§4](#4-соотношение-фаз-обработки-yaml)), резолвится CEL-ом до запроса (инъекции в Vault-запрос нет); значение секрета маскируется на выходе (логи/OTel/UI/отчёты), CEL обрабатывает его нормально. В контекстах без Vault-клиента (например, изолированный compile-only анализ) функция не зарегистрирована и обращение к ней — ошибка валидации. Идентификаторы с префиксом `__` зарезервированы за internal-механизмами CEL-слоя (macro `vault()` разворачивается в `__vault_read(path, __vault_resolver)`); авторское выражение с любым `__`-идентификатором — ошибка валидации, чтобы автор не мог обойти macro `vault()` прямым вызовом internal-функции.

`soulprint.where(...)`/`.where(...)` безопасен по построению: предикат — статический строковый литерал, раскрываемый на compile-фазе в нативный CEL filter-comprehension (см. [§2.3](#23-зарегистрированные-cel-функции-стартовый-минимум)), а не строка, исполняемая в runtime; инъекция через значение предиката исключена конструктивно (динамическая склейка предиката отвергается на валидации).

#### Два CEL-env: Keeper (full) и Soul (flow-control sandbox)

CEL живёт на двух сторонах ([ADR-012(d)](adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)), с разными env:

- **Keeper-env** (`shared/cel.New`, + `WithVault`) — полный: render params/`${ … }`, `where:`, `vault(...)`, `soulprint.hosts`/`soulprint.where`. Единственная сторона с внешним доступом (Vault). Один и тот же Keeper-env обслуживает **scenario-проход** (`soulprint.hosts`/`soulprint.where` доступны) и **destiny-проход** (изолированный render-проход `apply: destiny`, V2 ADR-009): в destiny-проходе cross-host аксессоры `soulprint.hosts`/`soulprint.where` **отсекаются** (`AllowHosts=false` → ошибка изоляции), а стабильный self-факт `soulprint.self.*` целевого хоста **остаётся доступен** (ADR-009/ADR-010 amendment 2026-06-18: self — per-host свойство, не scenario-scope; топология прогона приходит в destiny только через `apply: input:`).
- **Soul flow-control-env** (`shared/cel.NewFlowControl`) — урезанная песочница для предикатов `when:`/`changed_when:`/`failed_when:`. Регистрирует **только** функции без I/O (`size`/`contains`/`has`/`keys`/`values`/comprehensions/конверсии/операторы/`duration`/`glob`/`merge`) и переменные `register.*` (Soul собирает из результатов предыдущих задач по register-имени) + контекст из `flow_context` (`input`/`vars`/`essence`/`incarnation` + `soulprint.self`). Запрещены **конструктивно** (символ не зарегистрирован → compile-error «undeclared reference», sandbox-by-undeclaration, как migration-CEL [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)): `vault(...)`/`now()` (внешний доступ/недетерминизм), `soulprint.hosts`/`soulprint.where` (cross-host scenario-only — изоляция форсится `allowHosts=false`), любой `__`-идентификатор. Soul тянет `cel-go`, но НЕ `vault`-client: Vault-токенов на хосте нет, внешний доступ keeper-only.

### 7.2. text/template — sandbox через три барьера

text/template более опасен (исторически — vector SSTI в шаблонизаторах). Sandbox строится тремя одновременными барьерами:

1. **Strict mode** ([§3.1](#31-strict-mode)) — опечатка в имени поля = ошибка, не молча подставленный `<no value>`.
2. **Sprig allowlist без `exec`, `env`, `tpl`** ([§3.3](#33-sprig-allowlist)) — нет функций для выполнения команд, чтения окружения, выполнения произвольной строки как шаблона.
3. **Изолированный контекст рендера** ([§3.2](#32-контекст-рендера)) — нет глобального доступа к `essence`/`input`/`register`/`soulprint`, доступен только явно переданный `vars` + узкий системный набор.

### 7.3. SSTI через данные

Кейс: CEL-выражение возвращает значение, контролируемое извне (например, `${ input.user_name }`), и оно подставляется в `vars`. Угроза: значение содержит `{{ exec "rm -rf /" }}` и интерпретируется text/template как код.

**Закрыто всеми тремя барьерами одновременно:**

- Даже если text/template попытается интерпретировать строку, `exec` отсутствует в allowlist (барьер 2).
- Strict mode не позволит обратиться к глобальной переменной (барьер 1).
- Контекст рендера не содержит чувствительных данных, доступных через произвольный имя-доступ (барьер 3).

CEL-результат, попадая в `vars`, рассматривается text/template **как текстовый литерал**, а не как шаблон. text/template не парсит значения `vars` рекурсивно — он подставляет их как `.vars.<name>`.

### 7.4. Secret-маскинг

CEL обрабатывает значения с `secret: true` (из `input:`/essence-стандарта, [docs/input.md](input.md)) **как обычные** — без специальной обработки внутри движка. Это критично: легитимный кейс «передать секрет в `params: { password: \"${ input.password }\" }`» должен работать. CEL не имеет права отказать.

Маскинг применяется **на выходе** — на границах вывода:

- логи (rendered-params в лог-записи шага);
- OTel-трейсы (атрибуты span-а);
- UI и API-ответы;
- отчёты о прогоне.

Маскированию подлежит значение, помеченное `secret: true` в схеме источника (см. [docs/input.md](input.md)). Маскинг — обязанность слоя вывода, не шаблонизатора.

#### Sensitive-by-construction params

Помимо `secret: true`-маскинга по схеме источника, существует категория **sensitive-by-construction** — params модуля, которые секретны по самой своей природе, безотносительно того, что в них подставил оператор. Такой param **никогда** не логируется, не кладётся в OTel-атрибуты, не возвращается в `output`/`register` и не попадает в `ApplyEvent` — независимо от наличия `secret: true` на источнике значения.

Текущий список:

- **`core.url.fetched` → `headers`** (`map[string]string`). Заголовки запроса штатно несут `Authorization: Bearer …` / `Cookie` / API-токены. Модуль может логировать только **ключи** заголовков (не значения) при необходимости диагностики; значения и сам блок `headers` в output исключены конструктивно (см. [ADR-015 → `core.url`](adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)).
- **`core.http.probe` → `headers`** (`map[string]string`). Та же категория и та же причина, что у `core.url`: заголовки probe-запроса несут `Authorization`/`Cookie`/API-токены. В output отдаётся только список ключей запрошенных заголовков (`headers_keys`); значения и сам блок `headers` исключены конструктивно (см. [ADR-015 → `core.http`](adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)). Тело ответа (`body`) при этом **не** sensitive-целиком — оно проходит обычный `audit.MaskSecrets` (health-эндпоинты возвращают полезный читаемый JSON).

Отличие от `secret: true`: маскинг по схеме зависит от разметки источника и может быть забыт оператором; sensitive-by-construction зашит в реализацию модуля и не выключается. Новый param этой категории добавляется в список выше при введении.

#### Secure-by-default + явный opt-out для HTTP-модулей

HTTP-модули (`core.url`, `core.http`) ходят в сеть и потому образуют отдельную supply-chain/SSRF-границу. Принцип: **наш код безопасен по умолчанию** — только `https://`, SSRF-guard по фактически резолвнутому IP, верификация TLS-цепочки взведены конструктивно. Политику «что разрешено в этом конкретном вызове» оператор задаёт **явными per-call opt-out-флагами**:

- `allow_http` — допускает `http://` (только схема; `file://`/`ftp://` остаются запрещены, SSRF-guard не ослабляется);
- `insecure_skip_verify` — отключает проверку TLS-цепочки (self-signed / internal CA);
- `allow_private` — снимает SSRF-guard (dial в metadata/loopback/RFC1918/link-local).

Каждый флаг — `default = false`, ослабляет ровно один независимый контур (флаги ортогональны), и снятие любого даёт **warning в output `warnings` `ApplyEvent`** (оператор видит факт ослабления в `RunResult`; в warning попадает только `host`, без полного URL и без `headers` — они sensitive). Это не «вечный запрет возможностей», а безопасный default под аудируемый opt-out (см. [ADR-016](adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)).

> **НОРМАТИВНЫЙ ИНВАРИАНТ.** `shared/netguard` — единый default-deny SSRF/https-guard; opt-out выражается ТОЛЬКО Soul-side per-call (модуль выбирает другой путь: `ValidateFetchURL(allowHTTP)` / `NewHTTPClient` без dial-guard / `checkRedirectAllowingHTTP`), netguard-функции НЕ параметризуются и НЕ ослабляются; Keeper-side Augur opt-out НЕ имеет — default-deny неотключаемо (другой threat-model).

**Отложенный CONCERN (не в этом слайсе):** policy-уровень «запретить insecure на проде» (soul-lint-правило / keeper-policy, статически или централизованно запрещающий взвод opt-out-флагов вне dev-окружения) — отдельный заход.

## 8. Multi-line CEL и кавычки

- Строки с `${ … }` обязательно в **двойных кавычках** YAML или в блочных формах `>`/`|`. Без кавычек YAML-парсер споткнётся о запятые/скобки CEL.
- Внутри `${ … }` строковые литералы CEL — **одинарные кавычки** (`'primary'`). Внешняя YAML-обёртка занимает двойные.
- Глубокая вложенность кавычек (`"${ soulprint.hosts.where('role == \"primary\"')[0].network.primary_ip }"` — bootstrap-create only, см. §6) — известный footgun: requires `\"` escape внутри CEL-литерала внутри YAML-литерала. **Рекомендация:** при глубокой вложенности — вынести выражение в `vars:` шага:

```yaml
vars:
  master_ip: "${ soulprint.hosts.where(\"role == 'primary'\")[0].network.primary_ip }"  # bootstrap-create only
params:
  command: "redis-cli replicaof ${ vars.master_ip } 6379"
```

- Многострочное CEL-выражение — через блочную форму:

```yaml
when: >
  input.do_restart &&
  size(soulprint.where("'db' in covens")) > 0
```

## 9. Escaping

### 9.1. Литерал `${` в YAML

Чтобы вставить буквальные символы `${` без интерпретации как CEL-маркера, используется обратный слэш:

```yaml
note: "shell-var literal: \\${HOME}"
# → note: "shell-var literal: ${HOME}"
```

Единственный поддерживаемый способ — `\${`. YAML-приёмов (anchor + alias и т.п.) для escape — не вводим, один способ.

### 9.2. Литерал `{{` в `.tmpl`

Стандартный Go text/template:

```
welcome {{ "{{" }} user.name {{ "}}" }}
```

## 10. Поведение на ошибках и диагностика

| Класс ошибки | Когда возникает | Поведение |
|---|---|---|
| **Compile-error CEL** | синтаксис, неизвестный идентификатор, несовместимые типы | **до старта прогона**, в фазе валидации; координата (файл, узел YAML, позиция в выражении) + сообщение CEL; прогон не стартует |
| **Runtime-error CEL** | div-by-zero, обращение к `null`-полю, etc. | шаг проваливается, координата + сообщение; штатная обработка через `onfail:` |
| **text/template strict-mode error** | обращение к отсутствующему полю, вызов запрещённой функции | рендер шага падает, та же штатная обработка через `onfail:` |
| **Sprig-allowlist violation** | использование запрещённой функции | compile-error на уровне text/template, шаг падает |
| **`${` без закрывающей `}`** | синтаксическая ошибка интерполяции | compile-error CEL до старта прогона |
| **`vault:` со `${ … }`** | запрещённая комбинация (см. [§4](#4-соотношение-фаз-обработки-yaml)) | ошибка валидации до старта прогона |
| **Не-строковый CEL-результат при склейке** ([§5](#5-non-string-cel-результат-в-yaml) случай (б), list/map) | склейка структуры со строкой | runtime-error |

Все ошибки шаблонизатора попадают в OTel-trace как **структурированные события** с координатами (файл, узел YAML/строка `.tmpl`, позиция в выражении), исходным выражением и сообщением движка. `soul-lint` использует те же coordinate-формы, чтобы вывод линтера и runtime-ошибки указывали на одно и то же место.

## 11. См. также

- [architecture.md → ADR-010](adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) — фиксация выбора движков.
- [architecture.md → ADR-003](adr/0003-destiny-format.md#adr-003-формат-destiny--yaml-с-типизированной-схемой-cuejson-schema) — место шаблонизатора в pipeline `render → validate → apply`.
- [scenario/orchestration.md](scenario/orchestration.md) — `where:`/`when:` в контексте scenario.
- [destiny/tasks.md §10](destiny/tasks.md#10-шаблонный-контекст) — шаблонный контекст внутри destiny.
- [keeper/modules.md](keeper/modules.md) — keeper-side core-модули (общий формат, в который встроится `core.file.rendered` на Soul-side).
- [naming-rules.md](naming-rules.md) — `core.file.rendered`, convention `.tmpl`, маркер `${ … }`.
- [docs/input.md](input.md) — `secret: true`-flag, обрабатываемый маскингом на выходе.
