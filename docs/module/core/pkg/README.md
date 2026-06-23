# core.pkg

Управление пакетами OS через native package manager. **Soul-side**, статически
встроен в `soul`-бинарь. Реализация — [`soul/internal/coremod/pkg/pkg.go`](../../../../soul/internal/coremod/pkg/pkg.go).

Backend берётся из soulprint-факта `os.pkg_mgr` (**primary**, [ADR-018(b)](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)); **fallback** при пустом/unknown факте — рантайм-детект `util.DetectPkgMgr`: **apt** (Debian/Ubuntu),
**dnf** (RHEL ≥ 8), **yum** (RHEL ≤ 7), **apk** (Alpine). Если ни факт, ни детект не
дали менеджер — шаг падает (`no supported package manager detected`).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `installed` | Пакет установлен (опционально точная версия). | `changed=true`, если пакета не было или установленная версия ≠ запрошенной `version`. Если уже стоит нужная версия (или `version` не задан и пакет есть) — `changed=false`. |
| `absent` | Пакет удалён. | `changed=true`, если пакет был и удалён. Если пакета нет — `changed=false`. |
| `latest` | Пакет установлен и подтянут до новейшей версии репозитория. | `changed=true`, если пакета не было либо версия после операции изменилась. |

## installed — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя пакета. |
| `version` | string | optional (default — без пина) | Точная версия в distro-native форме (`1.2.3`, `5:7.0.15-1~deb12u7`). Транслируется в `name=version` (apt/apk) или `name-version` (dnf/yum). Пусто/не задан → ставится версия по умолчанию из репозитория без пина. |

## absent — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя пакета. |

## latest — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `name` | string | required | Имя пакета. (`version` для `latest` игнорируется — смысл state в подтягивании новейшей версии.) |

## Capabilities / side-effects

- **Требует root** (`run_as_root`): install/remove через пакетный менеджер.
- **Выполняет подпроцессы** (`exec_subprocess`): `apt-get` / `dnf` / `yum` / `apk`,
  плюс query-команды (`dpkg-query` / `rpm -q` / `apk info`).
- **Меняет систему:** установленный набор пакетов.
- **Refresh индекса репозиториев** перед `installed`/`latest` на apt/apk
  (`apt-get update` / `apk update`) выполняется один раз за жизнь процесса `soul`
  (важно для свежих VM/контейнеров, где индекс пуст). dnf/yum не refresh-ятся —
  они подтягивают metadata по expiration сами.

## Output / register

`installed`/`latest` отдают `{ name, installed: true, version }`; `absent` —
`{ name, installed: false }`. Версия в output — best-effort: если pkg-mgr не отдаёт
её компактно, поле может быть пустым (на флаг `installed` это не влияет).

## Пример

```yaml
# Пакет из репозитория дистрибутива. version опционален: caller может не передать
# ключ — тогда core.pkg ставит версию по умолчанию из репо без =version-пина.
- name: Install redis-server package
  module: core.pkg.installed
  retry: { count: 3, delay: 10s }
  params:
    name: redis-server
    version: "${ has(input.version) ? input.version : '' }"
```

(рабочая установка `redis-server` — в [`examples/destiny/redis/tasks/install.yml`](../../../../examples/destiny/redis/tasks/install.yml); там `version` сделан `required: true` и читается голым `input.version`, без тернара — это выбор destiny, а не ограничение модуля: пустой `version` ставит дефолт из репо)

## Безопасность

- **Установка пакета = исполнение arbitrary postinst-кода с правами процесса
  `soul` — главный риск модуля.** `installed`/`latest` вызывают
  `apt-get install` / `dnf install` / `yum install` / `apk add`
  ([`runInstall`/`runLatest`](../../../../soul/internal/coremod/pkg/pkg.go)), а
  пакетный менеджер исполняет maintainer-скрипты пакета (postinst/pre-remove
  и т.п.) как root. Доверие модуля транзитивно: оно ровно равно доверию к
  репозиторию и подписи пакета, не к самому Soul Stack. Имя пакета (`name`) и
  версия (`version`) должны приходить от автора Destiny/scenario, а не из
  недоверенного ввода — недоверенное `name` = установка произвольного пакета из
  настроенных репозиториев.
- **Верификация подписи — на стороне пакетного менеджера, не модуля.** `core.pkg`
  **сам GPG/checksum не проверяет**: он не добавляет ключи и не переключает
  policy верификации. Гарантия целостности — это apt/dnf/yum/apk с их
  репозиторными ключами на хосте. Установка из непроверенного/неподписанного репо
  (или с отключённой проверкой на уровне OS) обходит эту защиту, и модуль это
  никак не страхует — корректная настройка доверенных репозиториев лежит вне
  модуля. Флагов вида `--allow-unauthenticated` / `--nogpgcheck` модуль **не**
  выставляет (сверено: `install` идёт без них).
- **Привилегии.** Манифест [`pkg.yaml`](../../../../shared/coremanifest/pkg.yaml)
  объявляет `required_capabilities: [run_as_root, exec_subprocess]` —
  install/remove через пакетный менеджер всегда требуют root и запуска
  подпроцессов. Это **декларация** для статической сверки `soul-lint` с
  `allowed_capabilities` хоста (см. [docs/keeper/plugins.md →
  required_capabilities](../../../keeper/plugins.md#required_capabilities-таблица)),
  а **не** runtime-повышение прав: операция исполняется с привилегиями процесса
  `soul`-агента (под root), повышения прав внутри модуля нет; postinst-скрипты
  идут под тем же root.
- **Backend и refresh индекса.** Менеджер берётся из soulprint-факта
  `SoulprintFacts.os.pkg_mgr` (**primary**, [ADR-018(b)](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp));
  **fallback** при пустом/unknown факте — рантайм-детект (`util.DetectPkgMgr` через
  `command -v`). Перед `installed`/`latest` на apt/apk
  один раз за жизнь процесса выполняется `apt-get update` / `apk update`
  ([`refreshIndex`](../../../../soul/internal/coremod/pkg/pkg.go)) — он подтягивает
  актуальные метаданные (включая отзыв пакетов) из настроенных репозиториев;
  доверие к содержимому индекса — снова свойство репо, не модуля.

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-018(b)](../../../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp) — `SoulprintFacts.os.pkg_mgr` как **primary** источник backend-а; рантайм-детект — fallback.
