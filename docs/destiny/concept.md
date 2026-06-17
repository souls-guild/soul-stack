# Destiny — концепция

Destiny — атомарный декларативный кирпичик «как привести один хост в нужное состояние». В Soul Stack-словаре destiny соответствует **states** SaltStack-а / **roles** или **playbook tasks** Ansible-а, но без их более широкого scope.

## Что такое destiny

- **Атомарность.** Один destiny отвечает за одну вещь: «как поставить и настроить Redis на хосте», «как раскатать конфиг haproxy», «как ротировать SSL-сертификат». Не «как развернуть весь кластер» — это уровень service (см. ниже).
- **Декларативность.** Содержимое destiny — это **желаемое состояние хоста**, не «команды, которые надо выполнить». Шаги формулируются через адреса модулей вида `core.pkg.installed`, `core.service.running` — читаются как «пакет установлен», «сервис запущен», а не «установи пакет», «запусти сервис». См. [architecture.md → «Адресация модулей»](../architecture.md#адресация-модулей).
- **Идемпотентность.** Повторный apply destiny с теми же входными параметрами должен оставлять хост в том же состоянии. Это инвариант, а не свойство «по возможности». Двойной прогон в destiny-testing именно его проверяет — см. [testing.md](testing.md).
- **Без runtime-state.** Destiny не знает про БД Keeper-а, про другие хосты, про текущее состояние кластера, **и не видит essence сервиса**. Всё, что ей нужно от внешнего мира, приходит через **`input:`-контракт**; локальные значения автор задаёт в **`vars.yml`** (изолированы, снаружи не переопределяются). Этим destiny отличается от scenario, см. ниже.

> **Граница destiny / scenario — рекомендация, не инвариант** ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Прежний инвариант «scenario только `apply:`, без `module:`» снят: scenario получило всё DSL-ядро задач destiny (включая изменяющие `module:`) плюс оркестрацию. destiny при этом **остаётся** изолированным, независимо-версионируемым, molecule-тестируемым кирпичом — но выбор «вынести в destiny vs инлайнить в scenario» теперь определяется рекомендацией (переиспользуемое/критичное → в destiny), а не запретом. Свойства destiny из этого списка не меняются; меняется только статус границы. Подробно — [`docs/scenario/concept.md`](../scenario/concept.md).

## Где destiny в общей картине

```
оператор ──► incarnation.spec   (что задекларировано)
                  │
                  ▼
          scenario(create/restart/…)
                  │
       ┌──────────┴──────────┐
       ▼                     ▼
     on: keeper         on: [coven, …]
       │                     │
       ▼                     ▼
     destiny             destiny   ◄── атомарный кирпич
       │                     │
       ▼                     ▼
   module(state)        module(state)
```

| Слой | Кто | Что делает |
|---|---|---|
| **Service** | `service.yml` + `scenario/<name>/main.yml` в git-репо сервиса | Тип сервиса (Redis HA), набор операций (`create`, `add_user`, `restart`, …). Склеивает destiny на разные хосты через `tasks:` сценария, читает/пишет `incarnation.state` в БД. |
| **Scenario** | `scenario/<name>/main.yml` | Конкретная операция над сервисом. Может смешивать `on: keeper` (cloud-create, vault-resolve) и `on: [coven, …]` / опущенный `on:` (исполнение на Souls); волатильный per-host фильтр — `where:`. См. [scenario/orchestration.md](../scenario/orchestration.md). |
| **Destiny** | `destiny-<name>/destiny.yml` + `tasks/main.yml` | Атомарная декларация «как привести хост в состояние X». Не знает про БД, про cluster topology, про других Souls. Принимает значения через `input:`. |
| **Module** | `core.pkg.installed`, `wb.haproxy.reloaded`, … | Реализация одного «глагола» (поставить пакет, поднять сервис, отрендерить файл). См. [architecture.md → «Модель модулей»](../architecture.md#модель-модулей). |

Service `tasks:` ↔ destiny `tasks:` — разные сущности с одним именем (см. сноску в [tasks.md](tasks.md)):

- Service-task — `apply: { destiny: redis, input: {...} }` на конкретный `on:`. Это «применить destiny на множестве Souls».
- Destiny-task — `module: core.pkg.installed` с `params:`. Это «вызвать одно state-форму модуля на одном хосте».

## Чем destiny отличается от соседей

| | Destiny | Scenario | Module |
|---|---|---|---|
| **Уровень** | один хост | один кластер | одна операция (verb) |
| **Знает про другие хосты?** | нет | да (через `on:`/`where:` и `soulprint.where`) | нет |
| **Пишет state в БД?** | нет | да (`state_changes`) | нет |
| **Параметризуется?** | да (`input:`) | да (`input:`) | да (`params:` шага) |
| **DSL задач** | [tasks.md](tasks.md) | то же ядро + оркестрационная дельта ([scenario/orchestration.md](../scenario/orchestration.md)) | — |
| **Изоляция / `module:`** | изолирована, видит только `input:` | граница — рекомендация, `module:` разрешён ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) | — |
| **Версия** | git ref destiny-репо (см. [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)) | git ref service-репо | git ref module-репо |
| **Дистрибуция** | отдельный git-репо | отдельный git-репо | артефакт-бинарь (custom) или встроен в `soul` (core) |

## Где живёт destiny

Один destiny = один git-репозиторий, со своей историей и тегами. Версия destiny — git ref ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)). Поле `version:` в `destiny.yml` отсутствует — это сознательное решение, чтобы не было «второй истины» рядом с git-тегом.

Полная структура папки destiny — в [manifest.md](manifest.md). Кратко:

```
destiny-<name>/
├── destiny.yml          # манифест: name, description, input, required_modules
├── vars.yml             # ОПЦ.: destiny-локалы (см. vars.md)
├── tasks/               # задачи (то, что раньше было в steps: внутри destiny.yml)
│   └── main.yml         # точка входа; через include: подключаются соседи
├── templates/           # .tmpl-шаблоны для core.file.rendered (ADR-010, ADR-015)
└── tests/               # molecule-style тесты этого destiny (см. testing.md)
```

## См. также

- [manifest.md](manifest.md) — формат `destiny.yml` и раскладка папки.
- [tasks.md](tasks.md) — формат `tasks/main.yml` и соглашения именования.
- [input.md](input.md) — `input:`-контракт destiny (где валидируется, как используется).
- [vars.md](vars.md) — `vars.yml`-локалы destiny (что доступно в шаблонах задач как `vars.*`).
- [testing.md](testing.md) — molecule-style тестирование destiny на эфемерном стенде.
- [architecture.md → «Модель модулей»](../architecture.md#модель-модулей) — слой, на котором destiny стоит.
- [architecture.md → «Service — структура и manifest»](../architecture.md#service--структура-и-manifest) — слой, который над destiny.
