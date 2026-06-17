# core.file

Управление файлами: создание с заданным содержимым/правами/владельцем,
удаление, рендер из text/template-шаблона. **Soul-side**, статически встроен в
`soul`-бинарь. Реализация — [`soul/internal/coremod/file/file.go`](../../../../soul/internal/coremod/file/file.go)
(present/absent) и [`soul/internal/coremod/file/rendered.go`](../../../../soul/internal/coremod/file/rendered.go)
(rendered).

`core.file` намеренно покрывает несколько ролей: `present` с inline-`content`
заменяет `core.copy`, `rendered` заменяет `core.template`
([ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список): эти модули
отдельно не выделены). Каталоги `core.file` **не создаёт** — только файлы (для
каталога используйте `core.exec.run` с `install -d`).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Файл существует с заданными `content` / `mode` / `owner` / `group`. | `changed=true`, если файла не было либо отличается содержимое (сверка по SHA-256), `mode` или владелец/группа. Совпало всё — `changed=false`. |
| `absent` | Файл удалён. | `changed=true`, если файл был и удалён. Файла нет — `changed=false`. |
| `rendered` | Файл = результат рендера text/template-шаблона ([ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)). | Рендер в память → SHA-256 сверяется с существующим файлом → запись только при diff. `changed=true`, если изменился хотя бы один из content / mode / owner. Запись атомарна (temp + rename в той же директории). |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Целевой путь файла. |
| `content` | string | optional (default `""`) | Содержимое файла. Пустая строка — валидный пустой файл. |
| `mode` | string | optional | Права в octal-форме (`"0640"`, `"0755"`). Если не задан — при создании берётся mode по умолчанию `os.WriteFile`; существующий mode не сверяется и не правится. |
| `owner` | string | optional | Владелец (имя пользователя). Резолвится через `/etc/passwd`. |
| `group` | string | optional | Группа (имя). Резолвится через `/etc/group`. |

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Путь удаляемого файла. |

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
| `vars` | map | optional | Переменные шаблона; в text/template доступны как `.vars.*`. Кладутся Keeper-ом в `render_context.vars`. |
| `mode` | string | optional | Права в octal-форме, как у `present`. |
| `owner` | string | optional | Владелец, как у `present`. |
| `group` | string | optional | Группа, как у `present`. |

Шаблон видит корень контекста `{ vars, self, role, essence }`: `.vars.*` — из
`vars:`, `.self.*` — проекция soulprint
([ADR-018](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)), `.role` —
declared-роль хоста, `.essence.*` — effective essence. Отсутствие переменной в
шаблоне — ошибка рендера (text/template strict-mode, `missingkey=error`).

**Ключи `.self.*` — snake_case** (proto field names, канон ADR-018), симметрично
CEL `soulprint.self.*` — единая точка правды. Составные ключи через `_`:
`.self.os.pkg_mgr`, `.self.os.init_system`, `.self.network.primary_ip` — буквально
как `soulprint.self.os.pkg_mgr` в YAML-выражениях. camelCase (`.self.os.pkgMgr`)
не сработает (значения нет под этим ключом).

## Capabilities / side-effects

- **Меняет файловую систему:** создаёт / перезаписывает / удаляет файл, правит
  mode и владельца. Для системных путей (`/etc/...`) требует соответствующих
  прав — на практике `run_as_root`.
- **Не выполняет подпроцессов** для present/absent/rendered (рендер и запись —
  in-process, без shell).
- `rendered` использует встроенный text/template-движок (`shared/tmpl`,
  sprig-allowlist; нет доступа к FS/сети/окружению — три sandbox-барьера
  [ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).

## Output / register

`present` и `rendered` отдают `{ path, sha256, mode, installed: true }`, где
`sha256` — хэш записанного содержимого. `absent` — `{ path, installed: false }`.
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

`rendered` из шаблона (роль `core.template`); `register` — чтобы рестартить сервис
только при изменении конфига:

```yaml
- name: Render redis.conf with dual-access (TCP + unix socket)
  module: core.file.rendered
  register: redis_conf
  params:
    path: /etc/redis/redis.conf
    template: templates/redis.conf.tmpl
    mode: "0640"
    owner: redis
    group: redis
    vars:
      socket:    "${ input.redis_socket }"
      password:  "${ input.redis_password }"
      maxmemory: "${ input.maxmemory }"
      config:    "${ input.config }"
```

(из [`examples/destiny/destiny-redis-single/tasks/main.yml`](../../../../examples/destiny/destiny-redis-single/tasks/main.yml))

## Безопасность

- **Прямая запись в произвольный путь файловой системы, включая системные
  (`/etc/...`), — главный риск модуля.** `present`/`rendered` создают и
  перезаписывают файл по `path`, а `absent` его удаляет; целевой путь модуль не
  ограничивает песочницей. Недоверенный `path` = запись/удаление произвольного
  системного файла (например подмена `/etc/passwd`, `/etc/sudoers`,
  unit-файла). `path`, `content`, `mode`, `owner`, `group` должны приходить от
  автора Destiny/scenario, а не из недоверенного ввода.
- **Атомарность записи различается по state.** `rendered` пишет атомарно — рендер
  в память, затем temp + rename в той же директории
  (`util.AtomicWrite`, [`rendered.go`](../../../../soul/internal/coremod/file/rendered.go)),
  так что наблюдатель не видит частично записанный конфиг. `present` пишет через
  `os.WriteFile` напрямую ([`file.go`](../../../../soul/internal/coremod/file/file.go))
  — **без** temp+rename: при сбое в момент записи файл может остаться
  усечённым. Для конфигов, читаемых конкурентно работающим демоном, предпочитайте
  `rendered` (или гарантируйте рестарт потребителя через `onchanges:`).
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
