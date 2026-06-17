# Коллекции модулей (feature backlog)

Этот документ собирает идеи и open Q вокруг **коллекции** — верхнего уровня адресации модулей (`namespace` в `<namespace>.<module>.<state>`, см. [«Адресация модулей»](architecture.md#адресация-модулей)). Сама схема адресации закреплена. Коллекция как полноценная **сущность** Soul Stack-а — фич-бэклог: ниже список того, что она может дать, и список развилок до начала реализации.

Имя сущности на уровне Soul Stack-словаря **пока не выбрано** — в документах используем нейтральное «коллекция / namespace». Кандидаты см. в open Q ниже.

## Зачем коллекция (что она нам даёт)

Сейчас в адресации `core.pkg.installed` префикс `core` работает просто как разделитель пространств имён. Если развернуть коллекцию в полноценную сущность, появляется набор практически полезных возможностей.

### 1. Единица дистрибуции

Коллекция = bundle модулей с одним манифестом, единым git ref-ом и источником. Не качаем 30 отдельных бинарей — качаем коллекцию по конкретному tag-у и получаем все её модули консистентным набором. По аналогии с Ansible Galaxy collections и Salt Packs.

```yaml
# гипотетический keeper-конфиг или service.yml.
# Версия коллекции — git ref (tag или branch), без semver-range — см. ADR-007.
collections:
  - { name: core,      builtin: true }
  - { name: wb,        ref: v1.5.0,  source: "git://gitlab.wildberries.ru/grimoires/wb" }
  - { name: community, ref: v0.12.3, source: "https://collections.soul-stack.io" }
```

### 2. Граница доверия

Подпись и проверка происходят на уровне коллекции, а не каждого модуля. Один publisher → один ключ. У Keeper-а — политика «доверяем `core` + `wb`, не доверяем `community`».

> **Текущая модель — Sigil (Вариант A).** Целостность плагинов в MVP закрыта **Sigil** — Keeper-signed digest-индексом ([ADR-026](adr/0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)): допуск конкретных хешей `(namespace, name, ref) → sha256`, подпись ключом Keeper-а, явный допуск Архонтом. Author-signed коллекции/издатели (граница доверия на уровне коллекции по `(namespace, ref)`, один publisher → один ключ — **Вариант B**) — **post-MVP** вместе с этой сущностью, расширяет Sigil аддитивно без breaking changes.

### 3. RBAC и allow-list

«Роль `app-team` может звать только `core.*` и `wb.*`», «production-destinies не могут использовать `community.*`». В реалиях большого парка операторов это почти всегда нужно.

### 4. Consistent versioning

Модули внутри одной коллекции двигаются вместе. Не «`pkg@1.5` совместим с `service@1.3`, а с `service@1.4` — нет», а «весь `wb` под git-tag-ом `v1.5.0` — consistent set». В destiny / service.yml декларируется один `ref:` коллекции вместо матрицы версий по модулям.

### 5. Discovery, UI, MCP

«Покажи все модули из `wb`», «какие состояния есть у `wb.haproxy`». UI Keeper-а группирует каталог по коллекциям, MCP-сервер отдаёт его операторам и LLM-агентам в структурированном виде.

### 6. Кеш доставки в push-режиме

В архитектуре уже описан кеш `/var/lib/soul-stack/modules/` по SHA-256 для push. С коллекциями ключ кеша становится не «отдельный модуль», а «коллекция@ref» — меньше штук кешировать, легче проверять консистентность, легче пайплайн «обнови коллекцию на всём парке».

### 7. Визуальная подсказка о происхождении

Из строки `core.pkg.installed` сразу видно: встроено, без сетевых зависимостей. Из `community.kubernetes.deployed` — сторонняя коллекция, нужна установка. Это решает «непонятно, где core / где custom», с чего разговор начался.

## Что нужно решить до реализации (open Q)

Все пункты — propose-and-wait, не закрепляются молча.

1. **Имя сущности в словаре Soul Stack.** Кандидаты:
   - **Grimoire** (гримуар) — модули = заклинания, гримуар = их книга. Ложится в «душевную» метафору.
   - **Codex** (кодекс) — нейтральнее, та же идея «книги».
   - **Order** (орден) — социальная метафора, гильдия публишеров. Слабее, чем книжная.
   - **Collection** / **Pack** / **Bundle** — нейтральные, без метафоры. Понятно, но не оригинально.
   - **Namespace** — техническое имя, не сущность.

2. **Декларация в destiny / service.yml.** Варианты:
   - Явный блок `required_collections:` с версиями + `required_modules:` ссылается на коротко-имя в духе `core.pkg`;
   - Версии тянутся из имени модуля автоматически (`core` → встроено, `wb` → последняя установленная);
   - Гибрид: коллекции декларируются глобально (Keeper-конфиг), destiny ссылается только на имена.

3. **Модель версионирования.** Базовое правило закреплено в [ADR-007](adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте): версия коллекции — это git ref (tag или branch), никаких semver-range. Открыто: соглашение об именовании tag-ов (обязательный `vMAJOR.MINOR.PATCH` или свободная форма), политика breaking change по `protocol_version` модулей внутри, нужно ли коллекции иметь собственный manifest c `min_keeper_version`-подобными compat-флагами.

4. **Где живёт реестр trusted-коллекций.** Postgres (часть Keeper-state) vs static конфиг Keeper-а vs оба. Влияет на то, можно ли управлять коллекциями через API/MCP в рантайме, или это деплой-time артефакт.

5. **Источник коллекции.** Git-репо / OCI registry / собственный artifact store / smesh. Сходимость с подходом к доставке `soul`-бинаря и кастомных модулей (см. «Доставка soul-бинаря и модулей на хост» в architecture.md).

6. **Push-кеш по коллекции.** Tar-bundle всей коллекции одним артефактом vs кеш по отдельным модулям. Tar-bundle проще для целостности и подписи, поштучный — экономит трафик при частичных обновлениях.

7. ~~**Состав core-коллекции.**~~ **Закрыто** ([ADR-015](adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) / [ADR-017](adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)). Состав core MVP: **16 Soul-side** — `core.pkg`, `core.file` (включая `core.file.rendered` — рендер шаблонов), `core.service`, `core.user`, `core.group`, `core.exec`, `core.cmd`, `core.cron`, `core.mount`, `core.git`, `core.archive`, `core.sysctl`, `core.url` (`fetched` — download-by-URL, https-only), `core.line` (`present`/`absent` — пилот in-place построчной правки, lineinfile-эквивалент), `core.repo` (`present`/`absent` — пакетный репозиторий apt/dnf/yum/apk), `core.firewall` (`present`/`absent` — одно правило файрвола ufw/firewalld, без enable/default-policy); **3 Keeper-side** (диспетчер `on: keeper`) — `core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`; **инфраструктурный** — `core.module.installed` (доставка/кеш плагинов на хост). `core.template` сознательно НЕ выделяется — рендер делает `core.file.rendered`. `core.copy` сознательно НЕ выделяется — покрывается `core.file.present` с inline-content. `cloud-provision` как destiny-конструкция отвергнут — это keeper-side step `core.cloud.provisioned` ([ADR-017](adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)). `state.migrate` как escape-модуль отвергнут — миграции state_schema покрываются DSL ([ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

8. **Совместимость и breaking change при обновлении коллекции.** Что считать совместимым обновлением; как Keeper маркирует destiny, которая больше не валидна с новой версией коллекции; нужно ли «закрепить версию коллекции в incarnation».

## Зависимости

- Связано с [ADR-004](adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) (раскладка бинарей) и разделом [«Модель модулей»](architecture.md#модель-модулей) — коллекция = надстройка над текущим module-моделом.
- Связано с разделом [«Доставка `soul`-бинаря и модулей на хост»](keeper/push.md#доставка-soul-бинаря-и-модулей-на-хост) в `docs/keeper/push.md` и push-кешем.
- При появлении UI Keeper-а ([open Q «UI Keeper-а»](architecture.md#открытые-вопросы)) — каталог коллекций становится одной из основных страниц.
