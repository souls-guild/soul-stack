# core.line

In-place построчная правка существующего файла (lineinfile-эквивалент).
**Soul-side**, статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/line/line.go`](../../../../soul/internal/coremod/line/line.go)
(диспетчер + валидация),
[`soul/internal/coremod/line/apply.go`](../../../../soul/internal/coremod/line/apply.go)
(чтение params, запись файла) и
[`soul/internal/coremod/line/edit.go`](../../../../soul/internal/coremod/line/edit.go)
(чистые функции редактирования строк).

Это первый core-модуль, который **не** перезаписывает файл целиком (как
`core.file`), а изменяет отдельные строки. Запись — атомарная
(`util.AtomicWrite`: temp + rename), не in-place truncate. По умолчанию
mode/owner/group существующего файла **сохраняются** ([ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)),
явные `mode`/`owner`/`group` для `present` — override.

## Осторожно с `regexp` — главный источник drift

`core.line` — намеренно урезанный безопасный MVP именно по той причине, по
которой lineinfile откладывали: **«regexp matches not what you think»**. Что
именно матчится:

- `regexp` применяется к **каждой логической строке файла без терминатора `\n`**
  через Go `regexp.MatchString` — то есть **частичное** совпадение по строке (не
  «вся строка целиком», если вы сами не поставили якоря `^…$`).
- CRLF **не** нормализуется: `\r` остаётся частью строки и участвует в матчинге
  и сравнении как есть (предсказуемость — модуль не угадывает намерения).
- **Backrefs не поддержаны**: подставить группы из `regexp` в `line` нельзя.
- `regexp` валидируется на `Validate` (`regexp.Compile`); невалидный паттерн —
  ошибка до запуска.

Поведение `present` с `regexp` при **множественном** совпадении: заменяется
**только первая** матчащая строка, остальные не трогаются, в output попадает
`warning` с числом совпавших. Это сознательный безопасный выбор, а не баг.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Строка `line` присутствует в файле. **С `regexp`:** первая матчащая строка заменяется на `line` (если совпадений нет — `line` добавляется по правилам вставки; если первое совпадение уже равно `line` — no-op). **Без `regexp`:** точная строка `line` добавляется, если её ещё нет. | `changed=true`, если строка добавлена или заменена. Совпадение/наличие → `changed=false`. |
| `absent` | **С `regexp`:** удаляются **все** матчащие строки. **Без `regexp`:** удаляются **все** точные совпадения `line`. | `changed=true`, если удалена хотя бы одна строка. Удалять нечего (или файла нет) → `changed=false`. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Целевой файл. |
| `line` | string | required | Точное значение строки, которым управляем: добавляется (без `regexp`) или подставляется на место первого совпадения (с `regexp`). |
| `regexp` | string | optional | Go-regexp; **частичное** совпадение по строке. Без якорей `^…$` матчит подстроку. Backrefs не поддержаны. |
| `insertafter` | string | optional (`""` \| `EOF` \| литерал) | Позиция вставки при добавлении: после первой строки, точно равной литералу; `EOF` или ненайденный якорь → конец файла. Взаимоисключающ с `insertbefore`. |
| `insertbefore` | string | optional (`""` \| `BOF` \| литерал) | Позиция вставки: перед первой строкой, точно равной литералу; `BOF` → начало файла; ненайденный якорь → конец файла. Взаимоисключающ с `insertafter`. |
| `create` | bool | optional (default `false`) | Если файла нет: `create:true` создаёт его с единственной строкой `line`; иначе шаг падает (`file not found, set create:true to create it`). |
| `mode` | string | optional | Права в octal-форме (`"0644"`). Применяются только при `create:true` (новый файл) либо как override при правке. |
| `owner` | string | optional | Владелец (имя пользователя); резолвится через `/etc/passwd`. |
| `group` | string | optional | Группа (имя); резолвится через `/etc/group`. |

Якоря `insertafter`/`insertbefore` (кроме `EOF`/`BOF`) — это **точное совпадение
строки**, а не regexp: позиция вставки предсказуема. Якорь не найден → fallback
на EOF.

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Целевой файл. Если файла нет — no-op (`changed=false`, `create` игнорируется). |
| `line` | string | требуется `line` **или** `regexp` | Без `regexp` — критерий точного совпадения для удаления всех таких строк. |
| `regexp` | string | требуется `line` **или** `regexp` | Удаляет **все** матчащие строки (частичное совпадение, см. предупреждение выше). |

`absent` не принимает `mode`/`owner`/`group` — при правке всегда сохраняет
текущие mode/owner/group файла.

## Capabilities / side-effects

- **Меняет файловую систему** (`fs_write_root` для системных путей): правит /
  создаёт файл атомарной записью (temp + rename), может менять владельца/режим.
  Для путей `/etc/...` на практике требует `run_as_root`.
- **Не выполняет подпроцессов** — чтение, редактирование и запись in-process,
  без shell.
- Финальный перевод строки исходного файла сохраняется; пустой файл не
  превращается в файл с пустой строкой.

## Output / register

- `present` (правка): `{ path, changed, matched, replaced }`; `warning` —
  добавляется только при множественном `regexp`-совпадении.
- `present` (создание, `create:true`): `{ path, changed: true, created: true }`.
- `present`/`absent` без изменений: `{ path, changed: false, matched }` или
  `{ path, changed: false, removed: 0 }`.
- `absent` (удаление): `{ path, changed: true, removed: <N> }`.

## Примеры

`present` — гарантировать настройку, заменив существующую строку по regexp:

```yaml
- name: Ensure PasswordAuthentication is off in sshd_config
  module: core.line.present
  params:
    path: /etc/ssh/sshd_config
    regexp: '^#?PasswordAuthentication'
    line: 'PasswordAuthentication no'
```

`absent` — удалить все строки, матчащие паттерн:

```yaml
- name: Remove legacy include directives
  module: core.line.absent
  params:
    path: /etc/app/app.conf
    regexp: '^include\s+/etc/app/legacy/'
```

(минимальный валидный пример — в `examples/` задач для `core.line` пока нет)

## Безопасность

- **`regexp` правит чужой файл частичным совпадением — главный источник опасной
  правки.** `regexp` применяется к каждой логической строке через
  `regexp.MatchString` без неявных якорей
  ([`presentRegexp` / `absentEdit`](../../../../soul/internal/coremod/line/edit.go)):
  без `^…$` он матчит **подстроку**, а в `absent` удаляет **все** совпавшие строки.
  Слишком широкий паттерн (особенно из недоверенной интерполяции) может снести
  больше строк, чем задумано, в системном конфиге. См. подробный разбор семантики
  совпадения в разделе «[Осторожно с `regexp`](#осторожно-с-regexp--главный-источник-drift)»
  выше — он же и есть основной security-инвариант модуля. Якоря
  `insertafter`/`insertbefore` — это **точное** совпадение строки (литерал), не
  regexp ([`appendByPosition`](../../../../soul/internal/coremod/line/edit.go)),
  поэтому позиция вставки предсказуема и не зависит от паттерна.
- **ReDoS не грозит, backrefs запрещены.** Движок — Go `regexp` (RE2):
  компиляция через `regexp.Compile` на `Validate` и в `readParams`
  ([`line.go`](../../../../soul/internal/coremod/line/line.go),
  [`apply.go`](../../../../soul/internal/coremod/line/apply.go)) — невалидный
  паттерн отвергается до записи. RE2 гарантирует линейное время матчинга, поэтому
  catastrophic backtracking / ReDoS на недоверенном `regexp` невозможен (в отличие
  от PCRE). Backrefs (подстановка групп `regexp` в `line`) **не поддержаны** в MVP
  ([ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)): это
  убирает целый класс ошибок «вписали в файл не то, что думали».
- **Запись атомарна (temp + rename), без частично-записанного файла.** Модуль
  никогда не делает in-place truncate: новое содержимое пишется во временный файл
  в той же директории и атомарно переименовывается
  (`util.AtomicWrite` / `util.AtomicWritePreserving`,
  [`apply.go`](../../../../soul/internal/coremod/line/apply.go)). Прерывание прогона
  (краш, OOM) не оставляет конфиг наполовину переписанным. При in-place правке
  mode/owner/group **сохраняются** по умолчанию
  ([ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)) —
  модуль не понижает права существующего файла молча.
- **Опасно vs. правильно.** Недоверенный паттерн в `absent`:

  ```yaml
  # ОПАСНО: regexp из внешнего ввода без якорей. value = "" → матчит каждую
  # строку → absent удалит ВЕСЬ файл; широкий паттерн снесёт лишнее.
  - name: Remove user-supplied lines
    module: core.line.absent
    params:
      path: /etc/app/app.conf
      regexp: "${ input.user_pattern }"
  ```

  ```yaml
  # БЕЗОПАСНО: regexp — фиксированный якорный паттерн от автора задачи,
  # матчит ровно нужные строки.
  - name: Remove legacy include directives
    module: core.line.absent
    params:
      path: /etc/app/app.conf
      regexp: '^include\s+/etc/app/legacy/'
  ```

- **Привилегии.** Модуль **не** объявляет `run_as_root` — в манифесте
  ([`line.yaml`](../../../../shared/coremanifest/line.yaml)) только
  [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum) (запись за
  пределы `/var/lib/soul-stack/`). Правка идёт с привилегиями процесса
  `soul`-агента; для путей `/etc/...` на практике требует root. Подпроцессов
  модуль **не** запускает — чтение, редактирование и запись in-process, без shell
  ([`apply.go`](../../../../soul/internal/coremod/line/apply.go)).

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/file/README.md](../file/README.md) — управление файлом целиком (present/absent/rendered).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP; почему `core.line` урезан в MVP.
