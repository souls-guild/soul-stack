# core.mount

Управление точками монтирования и записями `/etc/fstab`. **Soul-side**,
статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/mount/mount.go`](../../../../soul/internal/coremod/mount/mount.go).

Текущий mount-статус определяется через `findmnt --target <path>`
(util-linux/busybox). Запись fstab — **preserve-by-default**
(`util.AtomicWritePreserving`, паттерн пилота `core.line`): fstab правится
in-place, его существующие mode и владелец сохраняются; модуль не сбрасывает их
в `0644`/владельца процесса. owner/group fstab параметрами не принимаются.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `present` | Запись есть в `/etc/fstab` **и** ФС смонтирована. | `changed=true`, если изменился fstab (запись добавлена/обновлена) **или** ФС была не смонтирована и смонтирована сейчас. Совпали оба — `changed=false`. fstab сверяется по строке целиком при совпадении `target`. |
| `absent` | ФС размонтирована **и** запись удалена из `/etc/fstab`. | `changed=true`, если запись была в fstab и удалена **или** ФС была смонтирована и размонтирована. Ничего из этого — `changed=false`. |
| `mounted` | ФС смонтирована «как есть», **без** правки fstab (runtime-mount). | `changed=true`, если ФС была не смонтирована и смонтирована сейчас. Уже смонтирована — `changed=false`. |
| `unmounted` | ФС размонтирована, **без** удаления записи из fstab (запись остаётся). | `changed=true`, если ФС была смонтирована и размонтирована. Уже размонтирована — `changed=false`. |

## present — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Mount-point (target). Создаётся при монтировании, если каталога нет (`mkdir -p`, mode `0755`). |
| `source` | string | required | Источник — устройство / NFS-share / label (`/dev/sdb1`, `nfs-host:/export`). |
| `fstype` | string | required | Тип ФС (`ext4`, `xfs`, `nfs`, `tmpfs`). |
| `opts` | string | optional (default `defaults`) | Опции монтирования (4-е поле fstab), они же передаются в `mount -o`. |

Поля `dump` и `pass` записи fstab фиксированы как `0 0` (параметрами не
управляются). Запись fstab: `<source> <path> <fstype> <opts> 0 0`.

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Mount-point: по нему ищется запись в fstab и проверяется mount-статус. |

## mounted — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Mount-point (target). |
| `source` | string | required | Источник монтирования. |
| `fstype` | string | required | Тип ФС. |
| `opts` | string | optional (default `defaults`) | Опции для `mount -o`. fstab не правится. |

## unmounted — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Mount-point для размонтирования. Запись в fstab сохраняется. |

## Capabilities / side-effects

- **Требует root** (`run_as_root`): `mount` / `umount`, правка `/etc/fstab`.
- **Выполняет подпроцессы** (`exec_subprocess`): `findmnt` (проба статуса),
  `mount`, `umount`. Позиционные `source`/`path` отделяются `--` от опций
  (защита от аргументов, начинающихся с `-`).
- **Пишет вне `/var/lib/soul-stack`** (`fs_write_root`): правит `/etc/fstab`
  (атомарно, preserve mode и владельца), создаёт mount-point при
  `present`/`mounted`.
- **Меняет систему:** монтирует / размонтирует ФС. fstab записывается только
  когда содержимое реально поменялось (idempotent no-op fstab не трогает).

## Output / register

- `present` — `{ path, source, fstype, mounted: true, in_fstab: true }`.
- `absent` — `{ path, mounted: false, in_fstab: false }`.
- `mounted` — `{ path, mounted: true }`.
- `unmounted` — `{ path, mounted: false }`.

## Пример

```yaml
- name: Mount data volume persistently
  module: core.mount.present
  params:
    path: /data
    source: /dev/sdb1
    fstype: ext4
    opts: "defaults,noatime"
```

(минимальный валидный пример — в `examples/` задач для `core.mount` пока нет)

## Безопасность

- **Монтирование недоверенного источника (особенно сетевого NFS/SMB) — главный
  риск модуля.** `present`/`mounted` исполняют `mount -t <fstype> -o <opts> -- <source> <path>`
  ([`ensureMounted`](../../../../soul/internal/coremod/mount/mount.go)) и
  доверяют содержимому смонтированной ФС. Сетевой `source`
  (`nfs-host:/export`, SMB-share) под контролем атакующего может подсунуть
  файлы с setuid-битом, символические ссылки за пределы mount-point или просто
  вредоносное содержимое, которое затем прочитают другие шаги/процессы.
  `source`, `path`, `fstype`, `opts` должны приходить от автора Destiny/scenario,
  а не из недоверенного ввода. Защиту от setuid/устройств на недоверенной ФС
  задаёт сам автор через `opts` (`nosuid,nodev,noexec`) — модуль их **не**
  навязывает (default — `defaults`).
- **DoS через опции и источник.** `opts` передаётся в `mount -o` дословно;
  агрессивные сетевые параметры (например жёсткий `hard` NFS-mount на
  недоступный хост) могут привести к зависанию операции и блокировке процессов,
  обращающихся к mount-point. `tmpfs` без ограничения `size=` способен
  исчерпать память. Это свойство передаваемых `opts`/`source`, модуль их
  семантику не валидирует.
- **Инъекция аргументов закрыта `--`.** Позиционные `source`/`path` отделяются
  `--` от опций при вызове `mount`/`umount` (security review L1,
  [`ensureMounted`/`ensureUnmounted`](../../../../soul/internal/coremod/mount/mount.go)):
  `source`/`path`, начинающиеся с `-`, трактуются как пути, а не как флаги
  `mount`. Shell при этом не участвует — значения идут отдельными argv-токенами.
- **Идемпотентность и правка `/etc/fstab`.** Текущий статус определяется
  `findmnt --target <path>`; запись fstab — preserve-by-default
  (`util.AtomicWritePreserving`): fstab правится атомарно (temp+rename), его
  существующие mode и владелец сохраняются, модуль их не сбрасывает. Запись
  происходит **только** при реальном изменении содержимого (idempotent no-op
  fstab не трогает), сверка — по строке записи целиком при совпадении `target`.
  Ошибочный `present` пишет persistent-запись, которая попытается смонтировать
  источник при каждой загрузке — цена ошибки в `source`/`opts` повторяется на
  каждом boot, в отличие от разового `mounted`.
- **Привилегии.** Манифест
  [`mount.yaml`](../../../../shared/coremanifest/mount.yaml) объявляет
  `required_capabilities: [run_as_root, exec_subprocess, fs_write_root]` —
  `mount`/`umount` требуют UID 0, исполняются как подпроцессы
  (`findmnt`/`mount`/`umount`), а правка `/etc/fstab` — запись за пределами
  `/var/lib/soul-stack`. Это **декларация** для статической сверки `soul-lint` с
  `allowed_capabilities` хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: операция исполняется с привилегиями процесса
  `soul`-агента (под root), повышения прав внутри модуля нет.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
