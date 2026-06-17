# Раскладка папки destiny и формат `destiny.yml`

Описывает физическую раскладку одного destiny-репо и поля корневого манифеста. Содержательную сторону (что такое destiny, как соотносится с соседями) см. в [concept.md](concept.md); формат задач — в [tasks.md](tasks.md); `input:`-контракт — в [input.md](input.md); `vars:`-локалы — в [vars.md](vars.md).

## Раскладка папки

```
destiny-<name>/
├── destiny.yml                     # манифест (этот документ)
├── vars.yml                        # ОПЦ.: destiny-локалы (см. vars.md)
├── tasks/
│   ├── main.yml                    # точка входа; см. tasks.md
│   ├── install.yml                 # ОПЦ.: include-соседи main.yml
│   └── restart.yml                 # ОПЦ.
├── templates/                      # ОПЦ.: text/template-шаблоны для core.file.rendered (ADR-010)
│   └── *.tmpl
└── tests/                          # ОПЦ.: molecule-style тесты
    └── <case-name>/
        ├── case.yml
        ├── prepare.yml             # ОПЦ.
        ├── verify.yml              # ОПЦ.
        └── cleanup.yml             # ОПЦ.
```

Обязательны только `destiny.yml` и `tasks/main.yml`. Всё остальное появляется по мере необходимости.

## `destiny.yml` — манифест

Корневой файл — **только** манифест. Список задач в нём не лежит; он живёт в [`tasks/main.yml`](tasks.md). Это сделано сознательно: `destiny.yml` остаётся коротким — name, описание, контракт на входы и зависимости от custom-модулей; читается за один взгляд при ревью.

### Поля

| Поле | Обяз. | Смысл |
|---|---|---|
| `name:` | да | Короткое kebab-case имя destiny (`redis`, `haproxy`, `cert-rotation`). Совпадает с именем папки `destiny-<name>/` без префикса. |
| `description:` | рекомендуется | Одна-две фразы на английском: что destiny делает на хосте. Видно в UI Keeper-а, MCP-каталоге, выводе `soul-lint`. |
| `input:` | да (если есть параметры) | Входной контракт. Формат — общий стандарт [`docs/input.md`](../input.md); destiny-специфика — в [input.md](input.md). |
| `output:` | нет (есть = destiny возвращает результат caller-у) | Выходной контракт. **Симметричен `input:`** по форме — тот же общий стандарт [`docs/input.md`](../input.md); destiny-специфика — в [output.md](output.md). Опциональный: если destiny ничего не публикует наружу, блок опускается. |
| `required_modules:` | нет | Список **custom**-модулей (двухуровневая форма `<namespace>.<module>`), нужных задачам. Core-модули **не перечисляются** — они всегда доступны. См. [architecture.md → «Адресация модулей»](../architecture.md#адресация-модулей). |

### Что в `destiny.yml` НЕ лежит

- **Сам список задач.** Лежит в `tasks/main.yml` как top-level YAML-список (см. [tasks.md](tasks.md)). Если видишь `tasks:` или `steps:` ключом в `destiny.yml` — это устаревший формат.
- **Destiny-локалы (`vars:`).** Лежат в `vars.yml` рядом с `destiny.yml` как top-level YAML-map (см. [vars.md](vars.md)).
- **`version:`.** Версия destiny — git ref, под которым закоммичен файл. См. [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте). Расширение `output:`-контракта — это эволюция контракта, **не** повод вводить `version:`; правило ADR-007 применяется к `output:` так же, как к `input:`.
- **`templates:` / `tests:` секции.** Это **папки** на диске, не поля манифеста. Содержимое подхватывается по convention.

### Пример

```yaml
# destiny-redis/destiny.yml
name: redis
description: Install and configure Redis server on a single host

input:
  action:
    type: string
    required: true
    enum: [apply, ensure_user, restart, ping, replication_status]
  version:
    type: string
    format: semver
  password:
    type: string
    secret: true
    min_length: 16
  # … остальные параметры — см. examples/destiny/destiny-redis/destiny.yml

# Эта destiny использует только core-модули → required_modules не нужен.
# Появляется только когда нужны custom-модули из сторонних коллекций:
#
# required_modules: [wb.haproxy, wb.myapp]
```

Рабочий пример с полным `input:`-блоком — в [examples/destiny/destiny-redis/destiny.yml](../../examples/destiny/destiny-redis/destiny.yml).

## Когда нужны соседи `tasks/main.yml`

Один `tasks/main.yml` справляется до тех пор, пока destiny остаётся атомарным кирпичиком. Если файл уходит за ~150 строк или внутри явно выделяются логические подразделы — выносим их в соседей `tasks/<sub>.yml` и подключаем через `include:`. Сравнение со scenario, где `scenario/<name>/main.yml` сразу проектируется на include-соседей (`install.yml`, `replication.yml` и т.п.):

```yaml
# tasks/main.yml — top-level список задач, без обёртки.
- name: Install redis-server package
  module: core.pkg.installed
  when: input.action == 'apply'
  params: { name: redis-server, version: "${ input.version }" }

- name: Apply Redis configuration
  include: configure.yml             # подключает соседний tasks/configure.yml
  when: input.action == 'apply'

- name: Restart redis-server
  module: core.service.restarted
  when: input.action == 'restart'
  params: { name: redis-server }
```

`include:` подключает файл из той же папки `tasks/`. Точный синтаксис include (вычисление `when:`, scope переменных, обработка `register:` через границу) — см. [tasks.md](tasks.md). Глубина вложенности — по убеждению, без жёсткого лимита; на практике 1 уровень покрывает все реалистичные сценарии.

## См. также

- [tasks.md](tasks.md) — формат `tasks/main.yml`.
- [input.md](input.md) — destiny-специфика `input:`.
- [output.md](output.md) — destiny-специфика `output:` (симметричный документ).
- [testing.md](testing.md) — раскладка `tests/<case>/`.
- [../service/manifest.md](../service/manifest.md) — формат `service.yml` (уровень выше destiny: тип сервиса, scenario-операции, state_schema, миграции).
- [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — почему `version:` отсутствует.
