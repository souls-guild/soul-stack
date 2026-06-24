# core.sysctl

Управление kernel-параметрами (`vm.*`, `kernel.*`, `net.*`). **Soul-side**,
статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/sysctl/sysctl.go`](../../../../soul/internal/coremod/sysctl/sysctl.go)
(state `present`) и
[`soul/internal/coremod/sysctl/applied.go`](../../../../soul/internal/coremod/sysctl/applied.go)
(state `applied`).

State `present` приводит к согласованности **обе** стороны: runtime-значение (через
`sysctl -w`) и persist-запись в `/etc/sysctl.d/<filename>.conf` (чтобы значение
переживало reboot). Текущее runtime-значение читается через `sysctl -n <name>`;
multi-value ключи (tab-разделённые) нормализуются перед сравнением, чтобы
пробелы vs табы не давали ложного diff.

State `applied` управляет НАБОРОМ параметров через один детерминированный drop-in
(см. ниже): модуль сам строит контент из map (sorted keys), пишет его атомарно и
перечитывает точечно `sysctl -p <file>` при изменении.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Kernel-параметр `name` имеет значение `value` (runtime) **и** persist-запись `<name> = <value>` лежит в `/etc/sysctl.d/<filename>.conf`. | `changed=true`, если runtime-значение отличалось (применён `sysctl -w`) **или** persist-файл отсутствовал/содержал другую строку (перезаписан). Совпали оба — `changed=false`. |
| `applied` | BULK-набор `settings` (map) материализуется ОДНИМ drop-in `/etc/sysctl.d/<filename>.conf` (sorted keys, формат `key = value`); reload через `sysctl -p <file>` (точечно по drop-in, НЕ весь `--system`). | `changed=true`, если контент drop-in отличался от существующего (или файла не было) → атомарная перезапись. Совпал — `changed=false`. **reload сам `changed` НЕ помечает**; gating: `never` — никогда, `always` — безусловно, `auto` (default) — только при file-change. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя kernel-параметра (`vm.overcommit_memory`, `net.ipv4.ip_forward`). |
| `value` | string | required | Целевое значение. Сравнение с текущим — после нормализации по полям (multi-value ключи сводятся к одному пробелу + trim). |
| `filename` | string | optional (default — `<name>` с заменой `.` на `-`) | Имя persist-файла в `/etc/sysctl.d/`. Если не оканчивается на `.conf` — суффикс добавляется автоматически. По умолчанию `vm.overcommit_memory` → `vm-overcommit_memory.conf`. |

## applied — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `settings` | map `string→string` | required | Набор kernel-параметр→значение. Ключи **сортируются** при рендере drop-in → контент детерминирован между прогонами (нет ложного `changed`/повторного reload). Значения — строки (multi-value параметры через пробел). |
| `filename` | string | required | Имя drop-in в `/etc/sysctl.d/` (напр. `30-redis`). Если не оканчивается на `.conf` — суффикс добавляется автоматически. `filepath.Join(/etc/sysctl.d, …)` держит запись внутри каталога. |
| `reload` | string enum `auto`/`always`/`never` | optional (default `auto`) | Когда делать `sysctl -p <file>` (точечно по drop-in): `auto` — только при изменении файла; `always` — безусловно; `never` — opt-out (не делать вообще). Сам reload `changed` НЕ помечает. Тот же словарь, что `daemon_reload` у `core.service`. |
| `ignore_failures` | bool | optional (default `false`) | `true` → reload через `sysctl -e -p <file>` (`-e`/`--ignore` глушит read-only/несуществующие ключи в контейнерах, не валя прогон). Явный opt-in. |

## Capabilities / side-effects

- **Требует root** (`run_as_root`): `sysctl -w`/`sysctl -p` и запись в `/etc/sysctl.d/`.
- **Выполняет подпроцессы** (`exec_subprocess`): `sysctl -n <name>` (чтение
  текущего значения, `present`), `sysctl -w <name>=<value>` (применение runtime,
  `present`), `sysctl [-e] -p <file>` (reload drop-in, `applied`).
- **Меняет систему:** runtime kernel-параметр(ы) и persist-конфиг(и) в
  `/etc/sysctl.d/`.
- **Пишет вне `/var/lib/soul-stack`** (`fs_write_root`): создаёт каталог
  `/etc/sysctl.d` при необходимости, пишет persist-файл/drop-in. `present` —
  mode `0644`; `applied` — атомарная запись с preserve-by-default mode
  существующего файла (новый — `0644`). Запись выполняется только при diff
  (idempotent no-op файл не трогает).

## Output / register

- `present` отдаёт `{ name, value, path }`, где `path` — полный путь к
  persist-файлу (`/etc/sysctl.d/<filename>.conf`).
- `applied` отдаёт `{ path, settings }`, где `path` — полный путь к drop-in,
  `settings` — число применённых ключей.

## Примеры

```yaml
# present — один ключ.
- name: Enable IPv4 forwarding persistently
  module: core.sysctl.present
  params:
    name: net.ipv4.ip_forward
    value: "1"
```

```yaml
# applied — bulk-набор одним drop-in + точечный reload. ignore_failures для
# контейнеров, где часть vm.* read-only.
- name: Apply redis sysctl kernel parameters
  module: core.sysctl.applied
  params:
    filename: 30-redis
    reload: auto
    ignore_failures: true
    settings:
      vm.overcommit_memory: "1"
      vm.swappiness:        "1"
      net.core.somaxconn:   "65535"
```

(`applied` используется в host-tuning extras примера `examples/destiny/redis`)

## Безопасность

- **Главный риск — изменение kernel-параметра под root: неверное значение = DoS
  или ослабление защиты ядра.** Модуль применяет `value` напрямую через
  `sysctl -w <name>=<value>` и пишет persist-запись в `/etc/sysctl.d/`
  ([`ensureRuntime`/`ensurePersist`](../../../../soul/internal/coremod/sysctl/sysctl.go))
  — это глобальная настройка ядра, действующая на весь хост. Ошибочное значение
  бьёт по доступности (например `vm.max_map_count` или `fs.file-max` слишком
  мало → процессы падают на лимитах; `net.*`-параметры рвут сеть) или по
  безопасности (например ослабление `kernel.*`/`net.*`-защит). `value` из
  `input.*` / `register.*` / `soulprint.*` должно быть доверенным (автор
  Destiny/scenario), а не внешним вводом.
- **Валидации значения и имени НЕТ — это by design MVP.** Сверено:
  [`sysctl.go`](../../../../soul/internal/coremod/sysctl/sysctl.go) `Apply`
  только читает `name`/`value`/`filename` как строки и передаёт `value` в
  `sysctl -w` буквально; `Validate` делегирован в манифест
  ([`sysctl.yaml`](../../../../shared/coremanifest/sysctl.yaml)) и проверяет
  лишь known-state + required (`name`/`value`), но **не** диапазон, тип или
  допустимость самого параметра. Неизвестный/нечисловой `value` ловит уже сам
  `sysctl -w` (non-zero exit → шаг падает), а не модуль — модуль не «подстелет
  соломки» заранее. Имя файла `filename` нормализуется (`.`→`-`, суффикс
  `.conf`), но путь всегда внутри `/etc/sysctl.d/` (`filepath.Join(m.Dir, …)`) —
  записать persist-файл за пределы каталога через `filename` нельзя.
- **Привилегии.** Манифест
  [`sysctl.yaml`](../../../../shared/coremanifest/sysctl.yaml) объявляет
  `required_capabilities: [run_as_root, exec_subprocess, fs_write_root]` —
  изменение kernel-параметра и запись в `/etc/sysctl.d/` требуют UID 0,
  применение идёт через подпроцесс `sysctl` (`-n`/`-w`), а persist-файл пишется
  за пределами `/var/lib/soul-stack`. Это **декларация** для статической сверки
  `soul-lint` с `allowed_capabilities` хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: backend-вызовы и запись persist-файла идут с
  привилегиями процесса `soul`-агента (root), повышения прав внутри модуля нет.
  Это самый «системно-глобальный» из модулей зоны: эффект — на всё ядро хоста, а
  не на один аккаунт/каталог.
- **Персистентность усиливает цену ошибки.** Каждый шаг трогает **обе** стороны:
  runtime (`sysctl -w`, действует немедленно) **и** persist
  (`/etc/sysctl.d/<filename>.conf`, mode `0644`, переживает reboot). Поэтому
  неверное значение не «само починится» после перезагрузки — оно сохранится в
  конфиге и применится снова. Откат — отдельный `core.sysctl.present` с верным
  значением (модуль перезапишет и runtime, и persist-файл); удаления
  persist-записи как отдельного state в MVP нет.
- **Опасно vs. правильно.** Значение параметра из недоверенного источника:

  ```yaml
  # ОПАСНО: value из внешнего ввода применяется к ядру без проверки.
  # input.somaxconn = "0" или мусор → деградация сети / падение sysctl -w.
  - name: Tune net backlog
    module: core.sysctl.present
    params:
      name: net.core.somaxconn
      value: "${ input.somaxconn }"
  ```

  Фиксируйте проверенное значение в авторе Destiny:

  ```yaml
  # БЕЗОПАСНО: явное проверенное значение, заданное автором Destiny.
  - name: Enable IPv4 forwarding persistently
    module: core.sysctl.present
    params:
      name: net.ipv4.ip_forward
      value: "1"
  ```

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
