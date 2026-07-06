# Документация Soul Stack

Единая карта документации. Один и тот же навигационный каркас — и для человека, и для ИИ-агента: сначала определи, **что ты хочешь сделать**, потом иди в соответствующий раздел ниже.

**Маршрутизация по роли:**

- **Новый пользователь, первое знакомство** → раздел [Изучить](#изучить-новичок).
- **Оператор, эксплуатирует кластер** (развернуть / обновить / бэкап / мониторинг / восстановление) → раздел [Сделать](#сделать-оператор).
- **Автор конфигов** (destiny / scenario / service / шаблоны / миграции / модули) → раздел [Найти](#найти-reference).
- **Архитектор / contributor** (дизайн, инварианты, решения) → раздел [Понять](#понять-архитекторcontributor).
- **ИИ-агент** → начни **с этой карты**: найди раздел по назначению, перейди по ссылке. Правила работы агента и сводка решений — в [../CLAUDE.md](../CLAUDE.md).

Корневой обзор продукта — [../README.md](../README.md). Границы закрытой беты (что НЕ входит) — [known-limitations.md](known-limitations.md).

---

## Изучить (новичок)

Первое знакомство: понять словарь и поднять demo-сетап без чтения ADR.

| Документ | Что это / для кого |
|---|---|
| [getting-started.md](getting-started.md) | **Старт здесь.** Quickstart для внешнего оператора: собрать бинари из исходников (`make build`), поднять single-keeper + обязательную инфру (Postgres / Redis / Vault) локально (`make dev-up` / `dev-provision` / `dev-keeper`), забутстрапить первого Архонта, онбордить один Soul по CSR-флоу, применить сценарий `hello-world`. ~30 минут, с командами. Показывает и браузерный API-вьювер `GET /docs`. |
| [guides/first-service.md](guides/first-service.md) | **Следующий шаг после quickstart.** Пошаговый туториал: собрать **свой** сервис с нуля (`service.yml` со state_schema, `scenario/create`, `essence`), офлайн-валидация `soul-lint`, регистрация сервиса (git-ref как версия), создание инкарнации и проверка результата на хосте. Walk-through на реальном [`hello-world`](../examples/service/hello-world/) со ссылками на нормативные спеки для глубины. Мост между getting-started и эксплуатацией. |
| [naming-rules.md](naming-rules.md) | **Словарь имён** Soul Stack (Keeper / Souls / Destiny / Soulprint / Essence + SoulSeed / Coven / SID / Archon-AID / Reaper) с соответствием SaltStack-терминам (master / minion / states / grains / pillars). Читать до того, как начнёшь понимать любые конфиги; обязателен перед введением **любого** нового имени. |

---

## Сделать (оператор)

Операционный runbook прод-инсталляции. Reference, не туториал. Все файлы — в [operations/](operations/README.md) (+ [keeper/prod-setup.md](keeper/prod-setup.md) по Keeper-специфике).

**Развернуть и подготовить:**

| Документ | Что это / для кого |
|---|---|
| [operations/deployment.md](operations/deployment.md) | Развёртывание прод-кластера Keeper: артефакты, конфиг, запуск, проверка готовности. |
| [operations/deb-onboarding.md](operations/deb-onboarding.md) | Онбординг кластера из deb-пакетов: установка → Vault-provision → TLS → keeper init → soul init → connected. |
| [operations/infra.md](operations/infra.md) | Обязательный контур инфры (Postgres + Redis + Vault): требования, версии, настройка под прод. |
| [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md) | Бутстрап первого Архонта и базовый RBAC: `keeper init --archon`, выдача ролей, защита от self-lockout. |
| [keeper/prod-setup.md](keeper/prod-setup.md) | Перевод Keeper из dev-стека в продакшен: отличия от dev, инфра-зависимости вне нашего кода (Vault PKI, Postgres). |

**Эксплуатировать и масштабировать:**

| Документ | Что это / для цели |
|---|---|
| [guides/day-2.md](guides/day-2.md) | **Рабочий цикл оператора (Day-2).** Пошаговые практики: drift-check и reconcile, апгрейды Service и Soul, масштабирование флота, инциденты и восстановление, автоматизация регулярных операций. Мост между первоначальным deployments и повседневной эксплуатацией. |
| [operations/monitoring.md](operations/monitoring.md) | Мониторинг кластера: метрики Prometheus, OTel, что и как наблюдать. |
| [operations/scaling.md](operations/scaling.md) | Горизонтальное масштабирование stateless-Keeper-кластера под большой флот. |
| [operations/upgrade.md](operations/upgrade.md) | Обновление кластера и сервисов: порядок, совместимость, миграции. |

**Восстановить после сбоя:**

| Документ | Что это / для цели |
|---|---|
| [operations/disaster-recovery.md](operations/disaster-recovery.md) | Бэкап и восстановление состояния кластера после отказа. |
| [operations/recovery-reclaim-apply-runs.md](operations/recovery-reclaim-apply-runs.md) | Reclaim зависших apply-run-ов и восстановление прогонов. |
| [operations/faq.md](operations/faq.md) | Частые операционные вопросы и ответы. |

---

## Найти (reference)

Справочник: точные форматы, поведение, параметры. Источник правды — здесь (и в коде), а не в обзорах.

### API Keeper-а

| Документ | Что это / для кого |
|---|---|
| [keeper/operator-api.md](keeper/operator-api.md) | **Operator API**: HTTP-эндпоинты `/v1/*`, conventions, JWT-auth, RFC 7807-ошибки, mapping endpoint ↔ MCP-tool ↔ permission. Детальные секции крупных доменов — в под-папке [operator-api/](keeper/operator-api/) (incarnations / souls / voyages / cadences / tidings / heralds / errands / push / oracle / synods / roles / …). |
| [keeper/openapi.yaml](keeper/openapi.yaml) | **Committed OpenAPI 3.1 спека** (производная от Go-типов huma, [ADR-054](adr/0054-openapi-code-first.md)). Потребляется UI-vendor-ом и `soulctl`. |
| API в браузере — `GET /docs` | RapiDoc-вьювер с full-text-поиском по эндпоинтам и «Try It»; served-спека `GET /openapi.json` / `/openapi.yaml` — за JWT. Механизм и meta-роуты — [keeper/operator-api.md → Served-spec](keeper/operator-api.md#served-spec-openapiyaml--openapijson-за-jwt). Quickstart-вход в браузере — [getting-started.md](getting-started.md). |
| [keeper/mcp-tools.md](keeper/mcp-tools.md) | Каталог MCP-инструментов Keeper-а (+ детали в [mcp-tools/](keeper/mcp-tools/)). |

### DSL и форматы конфигов (автор конфигов)

| Документ | Что это / для кого |
|---|---|
| [destiny/](destiny/README.md) | Папка-индекс destiny: формат `destiny.yml` и `tasks/main.yml`, поля задачи, `input:`/`output:`, molecule-style тесты. |
| [scenario/](scenario/README.md) | Папка-индекс scenario: оркестрационный слой (`on:` / `where:` / `apply:`, probe-идиома, barrier / state-commit), граница с destiny. DSL-ядро задач — в [destiny/tasks.md](destiny/tasks.md). |
| [templating.md](templating.md) | **Нормативная спека шаблонизатора** ([ADR-010](adr/0010-templating.md)): CEL для YAML-выражений (маркер `${ … }`), Go text/template для файлов `.tmpl`, sprig allowlist, security model, фазы рендера, `core.file.rendered`. |
| [migrations.md](migrations.md) | **Нормативная спека state_schema migration DSL** ([ADR-019](adr/0019-state-migration-dsl.md)): плоский `rename`/`set`/`delete`/`move` + CEL + `foreach`, forward-only, sandbox, атомарность одной PG-транзакцией, раскладка тестов. |
| [soul/soulprint.md](soul/soulprint.md) | Typed-схема Soulprint ([ADR-018](adr/0018-soulprint-typed.md)): поля `SoulprintFacts`, каноническая CEL-форма `soulprint.self.<path>`, виртуальная проекция `covens`. |
| [input.md](input.md) | **Стандарт формата `input:`** для destiny / scenario / манифеста модуля: типы, ключи валидации, форматы (hostname / email / semver / …), примеры. Источник правды при расхождениях. |
| [service/manifest.md](service/manifest.md) | Раскладка service-репо и формат `service.yml` (`name` / `state_schema_version` / `state_schema` / `destiny[]` / `modules[]`), запрещённые ключи, миграции state_schema, валидация `soul-lint validate-service`. |
| [service/manifest.md#essence](service/manifest.md#essence) | **Essence** (аналог Salt pillar) — иерархическая сборка параметров incarnation (`essence/_default.yaml` + overlay по Coven / OS-family, опц. `_stack.yaml`-pipeline). Описан внутри manifest.md; полная нормативная спека pipeline — [architecture.md → Essence](architecture.md#essence-pipeline-сборки). |

### Справочник модулей

| Документ | Что это / для кого |
|---|---|
| [module/](module/README.md) | Папка-индекс **per-module справочника**: для каждого реализованного core-модуля — каноническое имя, states, параметры, идемпотентность, side-effects, пример задачи. Документирует поведение по коду (не дизайн — дизайн в [ADR-015](adr/0015-core-modules-mvp.md) / [ADR-017](adr/0017-keeper-side-core.md)). |

Реализованные модули в [module/core/](module/core/) — **23 каталога** (у каждого свой `README.md`), и не все «модули» в одном смысле:

- **18 Soul-side core** (apply на хостах): `pkg`, `file`, `service`, `user`, `group`, `exec`, `cmd`, `cron`, `mount`, `git`, `archive`, `sysctl`, `url`, `line`, `repo`, `firewall`, `http` (17 по [ADR-015](adr/0015-core-modules-mvp.md): 12 исходных MVP + пост-MVP `url`/`line`/`repo`/`firewall`/`http`) + `augur` ([ADR-025](adr/0025-augur.md), read-probe через брокер).
- **4 Keeper-side core** (диспетчер `on: keeper`): `soul`, `cloud`, `vault` ([ADR-017](adr/0017-keeper-side-core.md)) + `choir` ([ADR-044](adr/0044-choir.md)).
- **1 `beacon`** — тело Vigil ([ADR-030](adr/0030-vigil-oracle.md)), read-only наблюдатель, не apply-модуль.

Точная сводка «что считаем» и источник правды (registry в коде) — [module/README.md → Статус каталога](module/README.md#статус-каталога).

### Конфиги бинарей и RBAC

| Документ | Что это / для кого |
|---|---|
| [keeper/](keeper/README.md) | Папка-индекс Keeper-стороны: Postgres + Redis, push, Reaper, plugins (Cloud / SSH), cloud-интеграция, формат `keeper.yml`. |
| [keeper/rbac.md](keeper/rbac.md) | RBAC: роли и permissions, единое применение к OpenAPI / MCP / push, bootstrap первого Архонта. |
| [soul/](soul/README.md) | Папка-индекс Soul-стороны: идентичность, онбординг bootstrap-токеном, алгоритм подключения, формат `soul.yml`, кеш модулей на хосте. |
| [keeper/run-flavors.md](keeper/run-flavors.md) | Сводка entry-point-ов запуска работы: scenario через agent, батч через Voyage, single-Errand, push по SSH. Какой endpoint API под какую задачу. |
| [observability.md](observability.md) | Нормативная спека observability ([ADR-024](adr/0024-observability.md)): префиксы метрик `keeper_*` / `soul_*`, OTel resource-attrs, контроль кардинальности. |
| [soul-lint.md](soul-lint.md) | Офлайн-линтер Destiny / Essence: назначение, список проверок, ограничения. |

---

## Понять (архитектор / contributor)

Дизайн, инварианты, обоснования и границы безопасности.

| Документ | Что это / для кого |
|---|---|
| [architecture.md](architecture.md) | Источник правды по архитектуре: обзорные разделы + стабы-ссылки на ADR, топология, жизненный цикл Soul, реестры, алгоритм подключения, push, Reaper, end-to-end сценарий, открытые вопросы. Перед любой задачей, влияющей на дизайн. |
| [adr/README.md](adr/README.md) | **Индекс всех ADR** со статусами (`active` / `amended` / `superseded`). 51 файл `NNNN-<slug>.md`, максимальный номер — **0054**; нумерация с пропусками (номера 0034, 0036, 0037 не использованы). Один ADR — один файл. |
| [requirements.md](requirements.md) | Продуктовые требования верхнего уровня: модульность, безопасность, метрики, OTel, Vault, RBAC, MCP, OpenAPI, hot-reload, ротация логов. |
| [security/threat-model.md](security/threat-model.md) | **Threat-model** кластера Keeper + флота Souls: активы, актёры / поверхности / границы, остаточные риски, требования к окружению. Reference; документирует уже-реализованные механизмы, новых решений не вводит. |
| [module-collections.md](module-collections.md) | Коллекции модулей как сущность: feature backlog и open Q (имя, дистрибуция, RBAC, signing, push-кеш). |
| [testing/](testing/README.md) | Индекс уровней тестирования (L0–L4); нормативная спека L3a — [testing/e2e.md](testing/e2e.md); runbook облачного live-E2E оркестратора — [testing/e2e-cloud.md](testing/e2e-cloud.md). |
| [testing/load-testing.md](testing/load-testing.md) | План нагрузочного тестирования: оси Souls/API/run, масштаб 1k–100k, harness soul-legion, фазы Ф0/Ф1/Ф2. Ф0+Ф1 ИЗМЕРЕНЫ до 25k (§8); 100k остаётся расчётным. |
| [dev/local-setup.md](dev/local-setup.md) | Локальный dev-стек: docker-compose (PG / Vault / Redis / OTel) + testcontainers-go для integration-тестов. |
| [guides/plugin-author.md](guides/plugin-author.md) | **Как написать свой модуль (soul-mod-*) — указатель.** Авторитетный пошаговый гайд автора — в companion-репо `soul-stack-plugins` ([module-author-guide.md](https://github.com/co-cy/soul-stack-plugins/blob/main/docs/module-author-guide.md)); этот core-side файл — короткий ориентир: когда писать плагин vs core/scenario + указатели на core-артефакты автора (SDK `sdk/module`, `proto/plugin/v1`, ADR-011 / ADR-016 / ADR-026). |
| [../CLAUDE.md](../CLAUDE.md) | Гайд для ИИ-агентов: правила работы, propose-and-wait, документация впереди кода, сводка решений. Каждая агентская сессия. |
| [../examples/](../examples/) | Образцы артефактов (destiny, service Redis HA, конфиги keeper / soul, incarnation-запросы, скелет custom-модуля) — иллюстрация форматов, не работающий код. |
| [web/README.md](web/README.md) | **Companion UI** (soul-stack-web): документация фронтенд-репо, API-контракт с backend, изменения OpenAPI требуют ревью web-стороны. |

---

## Границы и состояние

| Документ | Что это |
|---|---|
| [known-limitations.md](known-limitations.md) | Что НЕ входит в закрытую бету: cloud-provisioning без REST/MCP/UI, неполное MCP-покрытие, audit-scaling на крупных флотах, supply-chain-подпись, JWT-only identity, профиль push / recovery / Redis. Каждый пункт со ссылкой на канон — чтобы отсутствие фичи не принять за баг. |
| [backlog.md](backlog.md) | **Бэклог отложенных крупных эпиков**: сознательно поставленные на паузу фичи с зафиксированным импактом, прагматичным обходом и условиями возобновления (не open Q, не дизайн). Сейчас: per-service уникальность имени инкарнации (отвязка `incarnation.name` от Coven-метки). |
| [prod-readiness.md](prod-readiness.md) | **GA-gap роадмап**: что не готово для продакшена / GA (по результатам по-коду аудита). P0-блокеры (e2e-live blocking, clean-room onboarding, release-дистрибуция + cosign, Shepherd, recovery-lease live, внешний pentest, снять `continue-on-error`), P1-hardening, P2 + сильные места и доказанная нагрузка. Источник правды по GA-границам наравне с known-limitations.md; **не путать с дрейфующим roadmap.md**. |

**Состояние.** MVP feature-complete: три бинаря (`keeper` / `soul` / `soul-lint`) реализованы, HA-кластер Keeper (Postgres + Redis) доказан на живом стенде; **выпущена `v0.1.0-beta.1` (закрытая бета, приватные репо `souls-guild`)**. Сборка / линт / тесты — таргеты [`Makefile`](../Makefile) (`make build` / `make test` / `make check`). Все архитектурные решения проходят через ADR ([adr/](adr/README.md)); документация впереди кода — изменение дизайна это правка соответствующего ADR, а не «новый код как получилось».

---

## Словарь имён (кратко)

Полный словарь и правила — [naming-rules.md](naming-rules.md). Базовое соответствие:

| Soul Stack | SaltStack | Смысл |
|---|---|---|
| **Keeper** | master | Хранитель, центральный сервер |
| **Souls** | minions | Управляемые агенты |
| **Destiny** | states | Что применяется к хосту после прогона |
| **Soulprint** (Принты) | grains | Факты о системе хоста |
| **Essence** | pillars | Параметры / значения, иерархически собираемые на incarnation |

---

## Куда что писать (если добавляешь решение)

- **Новый ADR / меняешь ADR** → файл `adr/NNNN-<slug>.md` + строка в [adr/README.md](adr/README.md) + стаб-ссылка в [architecture.md](architecture.md). ADR/architecture — зона PM + architect, не docs-writer.
- **Новое имя сущности** → таблица в [naming-rules.md](naming-rules.md). Сначала propose-and-wait в чате, потом запись.
- **Новый ключ/тип/формат в `input:`** → [input.md](input.md). Сначала propose-and-wait, потом запись в стандарт.
- **Новая CEL-функция, sprig-allow/deny-правка, фазы рендера, маркер** → [templating.md](templating.md). Сначала propose-and-wait.
- **Новое продуктовое требование** → [requirements.md](requirements.md).
- **Метрика, OTel-инструментация, resource-attr, namespace-префикс** → [observability.md](observability.md) (конвенции) + [naming-rules.md](naming-rules.md) (имена). Propose-and-wait для нового имени.
- **Граница «backend отдаёт vs UI хардкодит»** → [ADR-042](adr/0042-backend-driven-ui.md); сквозной пункт — [requirements.md](requirements.md).
- **Планируемая проверка `soul-lint`** → [soul-lint.md](soul-lint.md), раздел «Планируемые проверки».
- **Концепция / структура destiny** → [destiny/](destiny/README.md).
- **Тестирование destiny и coverage** → [destiny/testing.md](destiny/testing.md).
- **Концепция / спека scenario** → [scenario/](scenario/README.md). DSL-ядро задач не дублируется — оно в [destiny/tasks.md](destiny/tasks.md).
- **Формат `service.yml` / раскладка service-репо / `soul-lint validate-service`** → [service/manifest.md](service/manifest.md).
- **Поведение / конфиг / lifecycle Soul** → [soul/](soul/README.md).
- **Поведение / конфиг / подсистемы Keeper** → [keeper/](keeper/README.md).
- **Augur — брокер внешнего доступа Soul** → [keeper/augur.md](keeper/augur.md) ([ADR-025](adr/0025-augur.md)).
- **Sigil — целостность плагинов** → [keeper/plugins.md → Integrity-model](keeper/plugins.md#integrity-model) ([ADR-026](adr/0026-sigil.md)).
- **Фича/open Q вокруг коллекций модулей** → [module-collections.md](module-collections.md).
- **Новый уровень тестирования / формат L3a** → [testing/](testing/README.md), спека — [testing/e2e.md](testing/e2e.md) ([ADR-039](adr/0039-e2e-testing.md)).
- **Security-граница / модель угроз** → [security/threat-model.md](security/threat-model.md). Документирует реализованное; новых решений не вводит.
- **Крупный эпик отложен (сознательная пауза, не open Q)** → запись в [backlog.md](backlog.md): что хотели, почему отложили, прагматичный обход, условия возобновления. ADR/architecture при этом не трогаются.
- **Документ разрастается на отдельный файл** → создать `docs/<тема>.md`, добавить запись сюда и ссылку из [architecture.md](architecture.md).
