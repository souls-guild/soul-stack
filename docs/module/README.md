# Каталог core-модулей

Per-module справочник реализованных core-модулей Soul Stack: каноническое имя,
поддерживаемые states, параметры каждого state, идемпотентность, side-effects,
пример задачи destiny.

Каталог документирует **только** то, что реально есть в коде
(`soul/internal/coremod/`, `keeper/internal/coremod/`). Это справочник по
поведению, а не нормативный источник дизайна — за дизайн отвечают
[ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) (Soul-side
core), [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)
(Keeper-side core), [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)
(рендер `core.file.rendered`).

Соседние документы (намеренно не дублируются здесь):

- [soul/modules.md](../soul/modules.md) — хостовая сторона модулей: где лежат,
  как кешируются, cleanup; manifest custom-модулей и `spec.states`.
- [keeper/modules.md](../keeper/modules.md) — спецификация Keeper-side core-модулей
  (диспетчер `on: keeper`).
- [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny) — словарь
  имён и сводная таблица всех core-модулей.

## Адресация

Шаг destiny адресуется как `core.<module>.<state>` — например
`core.pkg.installed`, `core.file.rendered`. Верхняя часть (`core.<module>`) —
имя модуля в Registry; `<state>`-суффикс приходит модулю в `ApplyRequest.state`
и диспетчится внутри реализации. Verb-формы (`run`, `shell`, `probe`, `fetched`,
`extracted`) — тот же механизм, просто без declarative-семантики «привести к
состоянию».

Диспетчер Soul-side / Keeper-side — scenario-ключ `on:`
([scenario/orchestration.md §3](../scenario/orchestration.md#3-таргет-шага--on)):
Soul-side core применяются на хостах (`on:` опущен или coven-метки), Keeper-side
core — только `on: keeper`.

## Soul-side core-модули

Статически встроены в `soul`-бинарь. Применяются одинаково в pull (демон) и push
(oneshot).

| Модуль | States | Назначение |
|---|---|---|
| [`core.pkg`](core/pkg/README.md) | `installed` / `absent` / `latest` | Пакеты OS через native pkg-mgr (apt/dnf/yum/apk). |
| [`core.file`](core/file/README.md) | `present` / `absent` / `rendered` | Файл с literal-content / отсутствует / отрендерен из `.tmpl`. |
| [`core.service`](core/service/README.md) | `running` / `stopped` / `restarted` / `enabled` | Сервис через systemd/openrc/sysv. |
| [`core.user`](core/user/README.md) | `present` / `absent` | Локальные пользователи OS. |
| [`core.group`](core/group/README.md) | `present` / `absent` | Локальные группы OS. |
| [`core.exec`](core/exec/README.md) | `run` (verb) | Произвольная команда через exec() (без shell). |
| [`core.cmd`](core/cmd/README.md) | `shell` (verb) | shell-команда (pipes, redirects). |
| [`core.cron`](core/cron/README.md) | `present` / `absent` | Cron-задачи. |
| [`core.mount`](core/mount/README.md) | `present` / `absent` / `mounted` / `unmounted` | Точки монтирования и /etc/fstab. |
| [`core.git`](core/git/README.md) | `cloned` / `pulled` | Клонирование / обновление git-репозитория на хосте. |
| [`core.archive`](core/archive/README.md) | `extracted` | Распаковка архивов (tar/tar.gz/tar.bz2/zip). |
| [`core.sysctl`](core/sysctl/README.md) | `present` / `applied` | Kernel-параметры (`vm.*`, `kernel.*`): `present` — один ключ, `applied` — bulk-набор одним drop-in + reload. |
| [`core.url`](core/url/README.md) | `fetched` | Загрузка файла по URL (только `https://`, idempotency через checksum). |
| [`core.line`](core/line/README.md) | `present` / `absent` | In-place построчная правка файла (lineinfile-эквивалент). |
| [`core.repo`](core/repo/README.md) | `present` / `absent` | Пакетный репозиторий (apt/dnf/yum/apk). |
| [`core.firewall`](core/firewall/README.md) | `present` / `absent` | Одно правило файрвола (ufw/firewalld). |
| [`core.http`](core/http/README.md) | `probe` (verb) | Read-probe HTTP (health-check / readiness, `changed=false`). |
| [`core.augur`](core/augur/README.md) | `fetch` (verb) | Read-probe живого доступа к внешней системе (Vault/Prometheus/ELK) через брокер Augur ([ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul), `changed=false`). |

## Keeper-side core-модули

Диспетчер `on: keeper` — выполняются на стороне Keeper-а, не на хосте. Спека —
[keeper/modules.md](../keeper/modules.md).

| Модуль | States | Назначение |
|---|---|---|
| [`core.soul.registered`](core/soul/README.md) | `registered` | Привязка SID к coven-меткам реестра souls. |
| [`core.cloud.provisioned`](core/cloud/README.md) | `created` / `destroyed` | Cloud-инстанс через CloudDriver-плагин. |
| [`core.choir`](core/choir/README.md) | `present` / `absent` | Членство Voice-а (SID) в Choir-е инкарнации (ADR-044). |
| [`core.vault.kv-read`](core/vault/README.md) | `kv-read` (verb) | Чтение секрета из Vault KV (v1/v2, auto-detect) на keeper-стороне. |

## core-beacon

Встроенные **core-beacon** ([ADR-030](../adr/0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-мониторинг-beacons--reactor))
— это тело [Vigil](../naming-rules.md#сущности-предметной-области) (Soul-side
event-driven мониторинг), а **НЕ** apply-модуль: beacon **наблюдает** состояние
хоста (read-only по конструкции) и при его смене поднимает Portent. У них нет
`states` и они не приводят хост к состоянию — поэтому они вынесены из таблиц
core-модулей выше. Адресуются как `core.beacon.<name>` в поле `VigilDef.check`.
Per-beacon справочник — [`core/beacon/README.md`](core/beacon/README.md).

## Статус каталога

Каталог укомплектован. Что считаем (источник правды — registry в коде,
`soul/internal/coremod/registry.go` и `keeper/internal/coremod/registry.go`):

- **18 Soul-side core** — 17 по [ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)
  (12 исходных MVP + пост-MVP `url` / `line` / `repo` / `firewall` / `http`,
  приняты по реальным запросам) + `augur` по [ADR-025](../adr/0025-augur.md#adr-025-augur--keeper-side-брокер-внешнего-доступа-soul)
  (read-probe через брокер Augur). Таблица «Soul-side core-модули» выше.
- **4 Keeper-side core** — `core.soul` / `core.cloud` / `core.vault` по
  [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)
  + `core.choir` по [ADR-044](../adr/0044-choir.md) (регистрируется при наличии
  `Deps.ChoirStore`). Таблица «Keeper-side core-модули» выше.

Итого **22 apply-модуля** (18 + 4). В `docs/module/core/` — **23 каталога**: эти
22 модуля плюс `core-beacon` (тело Vigil, read-only наблюдатель — не apply-модуль,
вынесен из таблиц, см. раздел «core-beacon»).

Эталоны (pilot) — [`core/pkg/README.md`](core/pkg/README.md) и
[`core/file/README.md`](core/file/README.md). Все ссылки в таблицах выше ведут на
существующие документы.

## См. также

- [ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — точный список Soul-side core MVP.
- [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read) — Keeper-side core расширения.
- [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) — шаблонизатор и `core.file.rendered`.
- [naming-rules.md → Модули Destiny](../naming-rules.md#модули-destiny) — словарь имён.
