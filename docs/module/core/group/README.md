# core.group

Управление локальными группами OS. **Soul-side**, статически встроен в
`soul`-бинарь. Реализация — [`soul/internal/coremod/group/group.go`](../../../../soul/internal/coremod/group/group.go).

Backend — `groupadd` / `groupdel`. Семантика `present` — present-or-create:
существующая группа **не реконсилится** (повторный вызов на существующей группе —
`changed=false`, `gid` не сверяется и не правится).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Группа существует (создаётся через `groupadd`, если её нет). | `changed=true`, если группы не было и она создана. Если группа уже есть — `changed=false` (`gid` существующей группы не сверяется). |
| `absent` | Группа удалена. | `changed=true`, если группа была и удалена (`groupdel`). Группы нет — `changed=false`. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя группы. |
| `gid` | int | optional | Явный gid (`groupadd -g`). Если не задан — gid выбирает `groupadd`. |
| `system` | bool | optional (default `false`) | Системная группа (`groupadd -r`): gid из системного диапазона. Совместимо с `gid` (можно задать оба). Нужно для сервис-аккаунтов stateful-сервисов (например primary-группа `redis`). |

Оба опциональных params (`gid`/`system`) применяются **только при создании**. Для
уже существующей группы — no-op.

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя удаляемой группы. |

## Capabilities / side-effects

- **Требует root** (`run_as_root`): `groupadd` / `groupdel` правят `/etc/group`.
- **Выполняет подпроцессы** (`exec_subprocess`): `groupadd` (present) / `groupdel`
  (absent). Проверка существования — in-process через `user.LookupGroup` (без
  подпроцесса).
- **Меняет систему:** набор локальных групп.

## Output / register

`present` отдаёт `{ name, exists: true, created }`, где `created` = `true`, если
группа была создана этим шагом, и `false`, если уже существовала. `absent` —
`{ name, exists: false }`.

## Пример

В committed examples `core.group.present` намеренно **не** используется (см.
[core/user/README.md](../user/README.md): сервис-аккаунты дают через `DynamicUser=yes`).
Минимальный пример системной primary-группы под сервис-аккаунт:

```yaml
- name: Ensure the app system group exists
  module: core.group.present
  params:
    name: appsvc
    system: true
```

Связка с `core.user` (группа создаётся ДО пользователя, т.к. `core.user -g`
требует существующую primary-группу) — в примере [core/user/README.md](../user/README.md).

## Безопасность

- **Главный риск — воссоздание привилегированной группы.** Сам по себе модуль
  только создаёт/удаляет группу через `groupadd` / `groupdel` и членством **не**
  управляет (членов в группу добавляет [`core.user`](../user/README.md) через
  `-G` / `-g`). Но `name` не валидируется на смысл: `core.group.present` с
  `name: sudo` / `wheel` / `docker` создаст привилегированную группу, если её
  нет (например на свежей VM), и дальнейший `core.user.present` с
  `groups: [<эта группа>]` даст носителю фактический путь к root. То есть риск —
  не в самом `groupadd`, а в том, что группа становится готовым «носителем
  привилегии» для последующего членства. Имя группы из `input.*` / `register.*`
  / `soulprint.*` должно быть доверенным (автор Destiny/scenario), а не внешним
  вводом.
- **Привилегии.** Манифест
  [`group.yaml`](../../../../shared/coremanifest/group.yaml) объявляет
  `required_capabilities: [run_as_root, exec_subprocess]` — `groupadd` /
  `groupdel` правят `/etc/group` и без UID 0 не сработают, а оба действия —
  запуск подпроцессов. Это **декларация** для статической сверки `soul-lint` с
  `allowed_capabilities` хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: операция исполняется с привилегиями процесса
  `soul`-агента (под root), повышения прав внутри модуля нет — под тем же root
  идут оба backend-а.
- **Валидации `gid` нет, реконсиляции нет.** `gid` передаётся в `groupadd -g`
  буквально, без проверки диапазона (сверено:
  [`group.go`](../../../../soul/internal/coremod/group/group.go) `applyPresent`
  только конвертирует params во флаги; `Validate` проверяет лишь тип `gid`/
  `system`). Идемпотентность present — present-or-create: у уже существующей
  группы `gid` **не** сверяется и **не** правится. Следствие: если группа с тем
  же именем уже есть с «чужим» gid, повторный applyPresent её не выровняет —
  модуль доверяет первому создателю. Удаление — через `absent` (`groupdel`);
  активные членства в `/etc/passwd` модуль при этом не чистит — это поведение
  `groupdel`.
- **Опасно vs. правильно.** Имя группы из недоверенного источника:

  ```yaml
  # ОПАСНО: имя группы из внешнего ввода — может оказаться sudo/wheel/docker,
  # и последующий core.user -G <эта группа> выдаст путь к root.
  - name: Ensure group
    module: core.group.present
    params:
      name: "${ input.group_name }"
  ```

  Для сервис-аккаунта фиксируйте имя системной группы в авторе Destiny:

  ```yaml
  # БЕЗОПАСНО: явная непривилегированная system-группа под сервис-аккаунт.
  - name: Ensure the app system group exists
    module: core.group.present
    params:
      name: appsvc
      system: true
  ```

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/user/README.md](../user/README.md) — `core.user` (primary-группа `-g` ссылается на неё).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
