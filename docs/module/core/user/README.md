# core.user

Управление локальными пользователями OS. **Soul-side**, статически встроен в
`soul`-бинарь. Реализация — [`soul/internal/coremod/user/user.go`](../../../../soul/internal/coremod/user/user.go).

Backend — `useradd` / `userdel` (busybox-совместимое подмножество; на Alpine это
пакет `shadow` или busybox-built-ins — оба понимают используемые флаги). Семантика
`present` — **present-or-create** (MVP): существующий пользователь **не
реконсилится**, опциональные params действуют только при создании. `usermod` в MVP
не вызывается.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Пользователь существует (создаётся через `useradd`, если его нет). | `changed=true`, если пользователя не было и он создан. Если пользователь уже есть — `changed=false` (reconcile uid/shell/home/groups/system/group **не выполняется**, см. вводный абзац). |
| `absent` | Пользователь удалён. | `changed=true`, если пользователь был и удалён (`userdel`). Пользователя нет — `changed=false`. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя пользователя. |
| `uid` | int | optional | Явный uid (`useradd -u`). Если не задан — uid выбирает `useradd`. |
| `shell` | string | optional | Login shell (`useradd -s`). Пусто/не задан → дефолт `useradd`. |
| `home` | string | optional | Домашний каталог (`useradd -d`). Создаётся с флагом `-M` (без авто-создания home), как реализовано в коде. Пусто/не задан → дефолт `useradd`. |
| `groups` | []string | optional | Supplementary-группы (`useradd -G a,b`). Группы должны существовать. |
| `system` | bool | optional (default `false`) | Системный аккаунт (`useradd -r`): uid из системного диапазона, для сервис-аккаунтов stateful-сервисов (например `redis`). |
| `group` | string | optional | Primary-группа (`useradd -g`). Группа **должна уже существовать** — caller создаёт её через `core.group` ДО. Отличается от `groups` (supplementary, `-G`). |

Все опциональные params (`uid`/`shell`/`home`/`groups`/`system`/`group`)
применяются **только при создании**. Для уже существующего пользователя они
no-op — не триггерят `usermod`/reconcile.

## Валидация ввода и формат params

Это **input-validation/safety нашего кода**, а не ограничение оператора: проверки
отсекают инъекции (ведущий `-` → argument confusion в argv `useradd`) и
заведомо-битый ввод с понятной ошибкой, **не** ужесточая реальные ограничения
`useradd`. Срабатывают на `Validate` (ранний отказ `soul-lint` / фаза валидации) и
повторно в `Apply` (если шаг вызван без предшествующей `Validate`-фазы — битое имя
не должно дойти до `useradd`/`userdel`).

| Param | Правило | Мотивация |
|---|---|---|
| `name` | `NAME_REGEX` shadow-utils по умолчанию: `^[a-z_][a-z0-9_-]*\$?$` (финальный `$` — литерал Samba/NIS-суффикса машинного аккаунта), длина ≤ 32, не пустой. Это конвенция самого `useradd` (с `--badnames` он её ослабляет — мы держим дефолт). | Имя обязано начинаться с буквы/`_` → ведущий `-` исключён (arg-injection guard на уровне формата). Слэш/пробел/спецсимволы не проходят regex. |
| `uid` | целое в диапазоне `[0, 2147483647]` (uid_t — знаковый 32-бит на Linux). | Range-guard от заведомо-битого ввода до запуска подпроцесса; тип уже проверяет `OptIntParam`. |
| `shell` | если задан — **абсолютный путь** (начинается с `/`), без ведущего `-`. Существование файла **не** проверяется. | `useradd` не требует существования shell (гибрид-правило гибкости — как в Ansible); абсолютность + запрет `-` отсекают argument confusion. |
| `home` | если задан — **абсолютный путь** (начинается с `/`), без ведущего `-`. Каталог **не** создаётся (флаг `-M`). | Симметрично `shell`: путь к home должен быть абсолютным, инъекционная форма отсекается. |
| `group` (primary) | то же `NAME_REGEX`, что и `name`. | Уходит в `useradd -g <group>`: ведущий `-` иначе распарсился бы как опция. |
| каждый из `groups` | то же `NAME_REGEX`, что и `name`. | Уходит в `useradd -G a,b`: каждое имя валидируется отдельно. |

**Arg-injection guard в argv.** Поверх формат-проверки имени модуль ставит `--`
перед позиционным `name`: `useradd … -- <name>` и `userdel -- <name>`. `useradd`
парсит опции через `getopt_long`, который понимает `--` как конец опций (man
useradd) — это defense-in-depth: даже если имя-опция как-то прошло бы формат-чек,
оно не будет интерпретировано как флаг. (`core.user` строит argv напрямую, без
`sh` — shell-injection не релевантен; релевантна именно arg-confusion позиционного
аргумента.)

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя удаляемого пользователя. |

## Capabilities / side-effects

- **Требует root** (`run_as_root`): `useradd` / `userdel` правят `/etc/passwd`,
  `/etc/shadow`, `/etc/group`.
- **Выполняет подпроцессы** (`exec_subprocess`): `useradd` (present) / `userdel`
  (absent). Проверка существования — in-process через `user.Lookup` (без
  подпроцесса).
- **Меняет систему:** набор локальных пользователей.

## Output / register

`present` отдаёт `{ name, exists: true, created }`, где `created` = `true`, если
пользователь был создан этим шагом, и `false`, если уже существовал. `absent` —
`{ name, exists: false }`.

## Пример

Выбор «`core.user.present` vs `DynamicUser=yes`» — это [гибрид-правило прод-конвенции §2](../../../destiny/production-conventions.md#2-сервис-аккаунт--гибрид-правило): **stateless**-демон без owned data-dir получает эфемерный аккаунт от systemd (`DynamicUser=yes`, ручной `core.user` не нужен), а **stateful**-сервису нужен стабильный uid-владелец каталога — его заводит `core.user.present`. Рабочий пример stateful-аккаунта — `node_exporter` в эталонной destiny [`node-exporter`](../../../../examples/destiny/node-exporter/tasks/account.yml) (textfile-каталог железных метрик переживает рестарты, нужен стабильный владелец); инлайн-redis_exporter в [`monitoring`](../../../../examples/service/monitoring/scenario/create/main.yml) — другой полюс: least-privilege `core.user.present` без стабильного state. Минимальный шаблон:

```yaml
# Primary-группа создаётся ДО пользователя (core.user -g требует существующую).
- name: Ensure the node_exporter system group exists
  module: core.group.present
  params:
    name: node_exporter
    system: true

- name: Ensure the node_exporter system user exists
  module: core.user.present
  params:
    name: node_exporter
    system: true
    group: node_exporter
    shell: /usr/sbin/nologin
    home: /
```

(из [`examples/destiny/node-exporter/tasks/main.yml`](../../../../examples/destiny/node-exporter/tasks/main.yml))

## Безопасность

- **Формат/инъекции проверяются, привилегированность смысла — нет.** Модуль
  валидирует **форму** ввода ([`Validate`](../../../../soul/internal/coremod/user/user.go)
  + повтор в `applyPresent`): `name`/`group`/`groups` по `NAME_REGEX`, `uid` в
  диапазоне, `shell`/`home` — абсолютный путь, и ставит `--` перед позиционным
  именем в argv (см. раздел «Валидация ввода»). Это закрывает **arg-injection** и
  заведомо-битый ввод. Но модуль **не** оценивает, насколько создаваемый аккаунт
  привилегирован: `uid: 0` создаёт второго пользователя с правами root (uid 0 — это
  и есть «root» для ядра, имя роли не важно — `0` проходит range-проверку как
  валидное значение), а `groups: ["sudo"]` / `["wheel"]` / `["docker"]` даёт
  носителю фактический путь к root (имена групп валидны по формату). Эти значения —
  часть attack surface: если они приходят из `input.*` / `register.*` /
  `soulprint.*`, им должен доверять автор Destiny/scenario, а не внешний ввод.
  Формат-валидация ≠ авторизация: она ловит инъекцию, не «опасный, но валидный»
  смысл.
- **Привилегии.** Манифест
  [`user.yaml`](../../../../shared/coremanifest/user.yaml) объявляет
  `required_capabilities: [run_as_root, exec_subprocess]` — `useradd` / `userdel`
  правят `/etc/passwd`, `/etc/shadow`, `/etc/group` и без UID 0 не сработают, а
  оба действия — запуск подпроцессов. Это **декларация** для статической сверки
  `soul-lint` с `allowed_capabilities` хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: операция исполняется с привилегиями процесса
  `soul`-агента (под root), повышения прав внутри модуля нет — под тем же root
  идут и `useradd`, и `userdel`.
- **Семантику значений не проверяем — держите ввод доверенным.** Формат — да (см.
  «Валидация ввода»), но смысл — нет. `shell` уходит в `useradd -s` буквально:
  абсолютность проверяется, **существование** файла login-shell — нет (это решает
  сам `useradd`, либо остаётся валидный, но «нерабочий» shell — гибрид-правило
  гибкости). `home` передаётся через `-d` совместно с `-M` (каталог **не**
  создаётся модулем). `group` обязан существовать заранее — модуль его не создаёт
  (его создаёт [`core.group`](../group/README.md) отдельным шагом ДО; формат имени
  модуль проверяет, наличие группы в системе — нет). Идемпотентность present —
  present-or-create: уже
  существующий пользователь **не** реконсилится, поэтому понизить привилегии
  ранее созданного аккаунта повторным applyPresent **нельзя** (uid/groups не
  правятся) — это снижает риск случайной правки, но и не лечит ошибочно выданные
  привилегии. Удаление — через `absent` (`userdel`).
- **Опасно vs. правильно.** Привилегированные значения из недоверенного источника:

  ```yaml
  # ОПАСНО: uid/groups из внешнего ввода создают root-эквивалентный аккаунт.
  # input.uid = 0 → второй root; input.extra_groups = ["sudo"] → путь к root.
  - name: Create app user
    module: core.user.present
    params:
      name: appsvc
      uid: "${ input.uid }"
      groups: "${ input.extra_groups }"
  ```

  Для сервис-аккаунта фиксируйте безопасные значения в авторе Destiny и не
  кладите его в привилегированные группы:

  ```yaml
  # БЕЗОПАСНО: system-аккаунт без login и без sudo/wheel/docker.
  - name: Ensure the app system user exists
    module: core.user.present
    params:
      name: appsvc
      system: true
      group: appsvc
      shell: /usr/sbin/nologin
      home: /var/lib/appsvc
  ```

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/group/README.md](../group/README.md) — `core.group` (primary-группа `-g` создаётся ею).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
