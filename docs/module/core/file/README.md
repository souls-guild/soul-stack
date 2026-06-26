# core.file

Управление файлами и каталогами: создание с заданным содержимым/правами/владельцем,
удаление, рендер из text/template-шаблона, создание каталога. **Soul-side**,
статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/file/file.go`](../../../../soul/internal/coremod/file/file.go)
(present/absent), [`soul/internal/coremod/file/rendered.go`](../../../../soul/internal/coremod/file/rendered.go)
(rendered) и [`soul/internal/coremod/file/directory.go`](../../../../soul/internal/coremod/file/directory.go)
(directory).

`core.file` намеренно покрывает несколько ролей: `present` с inline-`content`
заменяет `core.copy`, `rendered` заменяет `core.template`
([ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список): эти модули
отдельно не выделены), `directory` декларативно создаёт каталог вместо
`core.exec.run` с `install -d`.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Файл существует с заданными `content` (inline) **или** `src` (копия с хоста), плюс `mode` / `owner` / `group`. | `changed=true`, если файла не было либо отличается содержимое (сверка по SHA-256 — для `src` это `sha256(src-байты)`), `mode` или владелец/группа. Совпало всё — `changed=false`. |
| `absent` | Файл удалён. | `changed=true`, если файл был и удалён. Файла нет — `changed=false`. |
| `rendered` | Файл = результат рендера text/template-шаблона ([ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)). | Рендер в память → SHA-256 сверяется с существующим файлом → запись только при diff. `changed=true`, если изменился хотя бы один из content / mode / owner. Запись атомарна (temp + rename в той же директории). |
| `directory` | Каталог существует с заданными `mode` / `owner` / `group`. | `changed=true`, если каталога не было (создан) либо дрейфят `mode` или владелец/группа (починены — chmod/chown). Совпало всё — `changed=false`. Путь занят файлом — ошибка, без перезаписи. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Целевой путь файла. |
| `content` | string | optional | Содержимое файла inline. Взаимоисключается с `src`. Ни `content`, ни `src` не заданы — создаётся пустой файл (legacy-поведение). |
| `src` | string | optional | **Абсолютный** путь regular-файла на хосте; его содержимое копируется в `path` (типично — результат `core.archive.extracted`). Задаёт **только** содержимое, не атрибуты источника. Взаимоисключается с `content`. |
| `mode` | string | optional | Права в octal-форме (`"0640"`, `"0755"`). Если не задан — при создании берётся mode по умолчанию `os.WriteFile`; существующий mode не сверяется и не правится. |
| `owner` | string | optional | Владелец (имя пользователя). Резолвится через `/etc/passwd`. |
| `group` | string | optional | Группа (имя). Резолвится через `/etc/group`. |

### content vs src

`present` задаёт содержимое файла **ровно одним** из двух способов:

- **`content`** — содержимое inline, прямо в задаче.
- **`src`** — копия содержимого уже лежащего на хосте regular-файла по абсолютному
  пути (типично результат `core.archive.extracted`: распаковали архив → положили
  один из извлечённых файлов на место).

Правила взаимоисключения:

- **`content` и `src` вместе → ошибка** (`content and src are mutually exclusive`).
  Конфликт ловится по **присутствию ключа**, а не по пустоте строки: `content: ""`
  вместе с `src:` — тоже конфликт.
- **Ни `content`, ни `src` → пустой файл** (legacy-поведение `present`, обратная
  совместимость; не ошибка).
- `src` задаёт **только содержимое**. `mode`/`owner`/`group` целевого файла берутся
  из явных params `present`, атрибуты `src`-файла НЕ наследуются.

`src`-граница (MVP — только regular file):

- путь обязан быть **абсолютным** (относительный → `src must be absolute`);
- тип проверяется через `os.Lstat` + `IsRegular()` — **именно Lstat**: симлинк
  reject-ится, а не следуется (защита от подмены источника симлинком на
  чувствительный файл); каталог / симлинк / device / socket / fifo →
  `src %s is not a regular file`;
- отсутствующий `src` → `read src %s: no such file`; нечитаемый — permission-ошибка
  пробрасывается как есть.

Идемпотентность с `src`: желаемый хэш = `sha256(содержимое src)`, `changed=true`
если `path` отсутствует, либо `sha256(path) != sha256(src)`, либо дрейфит `mode` /
владелец / группа. `src` читается в память один раз — тот же буфер хэшируется и
пишется (защита от TOCTOU между сверкой и записью).

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Путь удаляемого файла. |

## directory — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Целевой путь каталога. |
| `mode` | string | optional | Права в octal-форме (`"0755"`, `"0750"`). Если задан — применяется при создании и сверяется/чинится при дрейфе; если не задан — mode при создании зависит от umask и не сверяется. |
| `owner` | string | optional | Владелец (имя пользователя). Резолвится через `/etc/passwd`, дрейф чинится `chown`. |
| `group` | string | optional | Группа (имя). Резолвится через `/etc/group`, дрейф чинится `chown`. |
| `parents` | bool | optional (default `false`) | `true` — создавать промежуточные каталоги (семантика `mkdir -p`). `false` — отсутствующий родитель приводит к ошибке. |

Идемпотентность: каталог есть и `mode`/`owner`/`group` совпадают → `changed=false`;
каталога нет → создаётся (`changed=true`, в `output` — `created: true`); каталог
есть, но атрибуты дрейфят → `chmod`/`chown` чинит, `changed=true`; путь занят
**файлом** (не каталогом) → ошибка, существующий файл **не** перезаписывается.
`recurse` (рекурсивное выставление прав на содержимое) в MVP **не** поддержан —
управляется только сам каталог.

**Отличие от `core.exec.run install -d`.** `install -d` — императивный
shell-вызов: выполняется каждый прогон, `changed` определяется не модулем
(`core.exec.run` сам по себе всегда «выполнил команду»), сверки/починки дрейфа
владельца и mode нет, dry-run (Scry) невозможен. `core.file.directory` —
декларативный: idempotent по `mode`/`owner`/`group`, чинит дрейф, отдаёт
честный `changed`, конфликт с файлом не маскирует, и поддерживает Plan/Scry
([ADR-031](../../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) — `planDirectory` сообщает тот же
`changed`, что выполнил бы Apply, не трогая хост.

## rendered — params

Param-контракт **уровня destiny** (как пишет автор задачи) отличается от
wire-формы, которую видит Soul: автор указывает `template:` (путь к `.tmpl`) и
`vars:`, а Keeper после CEL-фазы транслирует их в `template_content` (literal-тело
шаблона) и `render_context` (корень контекста §3.2) внутри `ApplyRequest.params`
— см. [ADR-018](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) и
[templating.md §3.2](../../../templating.md). Ниже — то, что пишет автор destiny.

На wire Keeper **обязан** доставить И `template_content`, И `render_context`:
без любого из них state `rendered` падает (`template_content` отсутствует →
нечего рендерить; `render_context` отсутствует → шаблоны с `.self.*` / `.vars.*`
падают strict-mode). Это прод-инвариант golden-path, а не optional — handoff
Keeper→Soul без обоих полей считается прод-блокером (см. комментарии
[`soul/internal/coremod/file/rendered.go`](../../../../soul/internal/coremod/file/rendered.go)).

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Целевой путь рендеренного файла. |
| `template` | string | required | Путь к `.tmpl`-шаблону (резолв scenario-local → service-level, [ADR-009](../../../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Keeper читает тело и доставляет как `template_content`. |
| `vars` | map | optional | ПРОИЗВОДНЫЕ переменные шаблона (вычисленные CEL-ом); в text/template доступны как `.vars.*`. Кладутся Keeper-ом в `render_context.vars`. Для проброса operator-input как есть `vars:` НЕ нужен — шаблон читает `.input.*` напрямую (Вариант B). |
| `mode` | string | optional | Права в octal-форме, как у `present`. |
| `owner` | string | optional | Владелец, как у `present`. |
| `group` | string | optional | Группа, как у `present`. |

Шаблон видит корень контекста `{ vars, self, role, essence }` + **условный**
`input`: `.vars.*` — из `vars:` (производные значения), `.input.*` — резолвнутый
operator-input прохода (**Вариант B**: шаблон читает поля ввода напрямую, без
passthrough через `vars:`; ключ `input` Keeper кладёт **только если шаблон реально
обращается к `.input.*`** — детект по обходу parse-AST `.tmpl`, не string-search;
шаблоны на одних `.vars` `input` не получают), `.self.*` — проекция soulprint
([ADR-018](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)), `.role` —
declared-роль хоста, `.essence.*` — effective essence. Отсутствие переменной в
шаблоне — ошибка рендера (text/template strict-mode, `missingkey=error`).

> **`.input.*` и секреты.** Secret-поле operator-input, прочитанное шаблоном через
> `.input.<name>`, маскируется в наблюдаемых каналах (status_details/error/логи) по
> схеме прохода (seal-механизм S-1, [templating.md §7.4](../../../templating.md#74-secret-маскинг)).
> На wire (`ApplyRequest.params.render_context`) значение реальное — Soul рендерит
> файл фактическим секретом, маскинг затрагивает только observability-вывод.

**Ключи `.self.*` — snake_case** (proto field names, канон ADR-018), симметрично
CEL `soulprint.self.*` — единая точка правды. Составные ключи через `_`:
`.self.os.pkg_mgr`, `.self.os.init_system`, `.self.network.primary_ip` — буквально
как `soulprint.self.os.pkg_mgr` в YAML-выражениях. camelCase (`.self.os.pkgMgr`)
не сработает (значения нет под этим ключом).

## Capabilities / side-effects

- **Меняет файловую систему:** создаёт / перезаписывает / удаляет файл, создаёт
  каталог, правит mode и владельца. Для системных путей (`/etc/...`) требует
  соответствующих прав — на практике `run_as_root`.
- **Не выполняет подпроцессов** для present/absent/rendered/directory (рендер,
  запись, `mkdir`/`chmod`/`chown` — in-process, без shell).
- `rendered` использует встроенный text/template-движок (`shared/tmpl`,
  sprig-allowlist; нет доступа к FS/сети/окружению — три sandbox-барьера
  [ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).

## Output / register

`present` и `rendered` отдают `{ path, sha256, mode, installed: true }`, где
`sha256` — хэш записанного содержимого. `absent` — `{ path, installed: false }`.
`directory` — `{ path, mode, created }`, где `created` — был ли каталог создан в
этом прогоне (`false`, если уже существовал и лишь чинились атрибуты).
`register:` на rendered-задаче типично используется как якорь `onchanges:` для
рестарта сервиса при изменении конфига.

## Примеры

`present` с inline-content (роль `core.copy`):

```yaml
- name: Drop a static marker file
  module: core.file.present
  params:
    path: /etc/soul-stack/marker
    content: "managed by soul-stack"
    mode: "0644"
    owner: root
    group: root
```

`directory` (замена `core.exec.run install -d`); `parents: true` создаёт всю
цепочку каталогов:

```yaml
- name: Ensure exporter data directory
  module: core.file.directory
  params:
    path: /var/lib/node_exporter/textfile
    parents: true
    mode: "0755"
    owner: node_exporter
    group: node_exporter
```

`rendered` из шаблона (роль `core.template`); `register` — чтобы рестартить сервис
только при изменении конфига:

```yaml
- name: Render redis.conf
  module: core.file.rendered
  register: redis_conf
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    mode: "0640"
    owner: redis
    group: redis
    vars:
      password: "${ input.password }"
      config:   "${ has(input.config) ? input.config : {} }"
```

(из [`examples/destiny/redis/tasks/server.yml`](../../../../examples/destiny/redis/tasks/server.yml); полный merged-конфиг приходит одним map-ом `config` — maxmemory/persistence/passthrough сливаются scenario через `merge()` ещё до render)

## Безопасность

- **Прямая запись в произвольный путь файловой системы, включая системные
  (`/etc/...`), — главный риск модуля.** `present`/`rendered` создают и
  перезаписывают файл по `path`, а `absent` его удаляет; целевой путь модуль не
  ограничивает песочницей. Недоверенный `path` = запись/удаление произвольного
  системного файла (например подмена `/etc/passwd`, `/etc/sudoers`,
  unit-файла). `path`, `content`, `mode`, `owner`, `group` должны приходить от
  автора Destiny/scenario, а не из недоверенного ввода.
- **Атомарность записи различается по state и по ветке `present`.** `rendered`
  пишет атомарно — рендер в память, затем temp + rename в той же директории
  (`util.AtomicWrite`, [`rendered.go`](../../../../soul/internal/coremod/file/rendered.go)),
  так что наблюдатель не видит частично записанный конфиг. У `present`
  ([`file.go`](../../../../soul/internal/coremod/file/file.go)) ветка различается:
  **`src`-ветка пишет атомарно** (`util.AtomicWrite`, temp + rename — копия конфига
  с хоста может читаться конкурентным демоном), а **`content`-ветка пишет через
  `os.WriteFile` напрямую** — **без** temp+rename: при сбое в момент записи файл
  может остаться усечённым. Для конфигов с inline-`content`, читаемых конкурентно
  работающим демоном, предпочитайте `rendered`/`src` (или гарантируйте рестарт
  потребителя через `onchanges:`).
- **`src` — копирование с хоста, не из недоверенного ввода.** `src` указывает
  абсолютный путь regular-файла на самом хосте; `present` reject-ит симлинк через
  `os.Lstat` (источник не следуется по символической ссылке — защита от подмены),
  каталог/device/socket/fifo и относительные пути. Это ограничивает форму
  источника, но не делает его доверенным: `src`, как и `path`, должен приходить от
  автора Destiny/scenario, а не из недоверенного ввода — иначе содержимое
  произвольного файла хоста окажется скопировано в `path`.
- **`mode`/`owner` — ответственность автора, не дефолт модуля.** Если `mode` не
  задан, при создании берётся mode по умолчанию `os.WriteFile` (зависит от umask),
  а существующий mode **не** сверяется и не правится — секрет, записанный без
  явного `mode`, может оказаться world-readable. Для файлов с чувствительным
  содержимым (ключи, пароли в конфиге) задавайте `mode` явно (`"0600"`/`"0640"`)
  и `owner`/`group`. `owner`/`group` резолвятся через `/etc/passwd` и `/etc/group`
  на хосте.
- **`rendered`: рендер в sandbox, секрет в контексте.** Тело шаблона и контекст
  приходят от Keeper как `template_content` + `render_context` после CEL-фазы
  ([ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов),
  [ADR-012](../../../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)); Soul-сторона
  рендерит сам через `shared/tmpl` (sprig-allowlist; без доступа к FS/сети/
  окружению — три sandbox-барьера). Это ограничивает, **что** шаблон может сделать,
  но не делает содержимое безопасным: секреты (`${ vault(...) }`, пароли через
  `vars:`) попадают в `render_context` и в итоговый файл — отсюда обязательность
  явного `mode` для конфигов с секретами. Отсутствие переменной в шаблоне —
  ошибка рендера (strict-mode `missingkey=error`), а не молчаливая пустая
  подстановка.
- **Привилегии.** Манифест [`file.yaml`](../../../../shared/coremanifest/file.yaml)
  объявляет [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum)
  (запись за пределами `/var/lib/soul-stack/`). Запись в системные пути на
  практике требует root — модуль исполняется с привилегиями процесса
  `soul`-агента, без повышения прав внутри. present/absent/rendered подпроцессы
  **не** запускают (запись и рендер in-process, без shell).

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [templating.md](../../../templating.md) — нормативная спека шаблонизатора (CEL + text/template).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) — рендер `core.file.rendered`, security model.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP; почему `core.copy`/`core.template` не выделены.
- [ADR-018](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) — `render_context` и `self`-проекция soulprint.
