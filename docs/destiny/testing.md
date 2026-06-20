# Тестирование destiny

> **Форма инструмента закреплена в [ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)** (2026-05-22): раннер — **Trial**, бинарь `soul-trial` (отдельный артефакт в модуле `keeper`, `keeper/cmd/soul-trial`). Две метрики: **`trial coverage`** (DSL — задачи/CEL-ветки/`enum`/`state_changes`) и **`code coverage`** (Go). Уровни **L0 render-only + L1 migration — MVP**; **L2 single-host (docker) — реализован test-only** (Вариант A, build-tag `integration`, см. ADR-023); **L3 multi-host — отложен**. DSL-coverage hook — `CoverageSink` в `shared/cel` на `Engine.EvalExpression`. ADR-023 закрыл open Q №1/6/7/8 ниже; с реализацией L2 закрыты **№4** (real-only) и **№5** (идемпотентность с opt-out), **№3** (sandbox) — закрыт **частично** (single-host docker; multi-host `stand:` для L3 остаётся открытым). Развилки и решения — `.pm/tasks/2026-05-22-testing-framework/`.

Тестирование destiny на эфемерном стенде с измерением coverage. Раскладка и формат тест-кейсов закреплены (см. ниже); детали L0-фикстур/assert и L2/L3-sandbox — в проработке.

Это **не** часть `soul-lint`. По [ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) `soul-lint` строго офлайн и статический (не исполняет); `soul-trial` — прогон. Линия раздела: «не исполняет → soul-lint, исполняет → soul-trial».

## Замысел (грубо)

- К каждому destiny прикладывается набор тест-кейсов: входные параметры, ожидания на `output` / state хоста, возможно последовательности нескольких прогонов разными action.
- По умолчанию исполнение — на эфемерном стенде в docker: контейнер с настоящим `soul`-бинарём и core-модулями + sandbox-контейнер как «целевой хост». Per-test изоляция.
- Задачи и выражения `when` инструментированы — собирается trace: какие задачи выполнялись, какие условия сработали, какие модули были вызваны.
- По итогам прогона публикуется coverage-отчёт. Цель — 100% покрытие; дыра — повод дописать тест-кейс.
- Visualization в UI Keeper-а (когда он появится — см. open Q «UI Keeper-а» в [architecture.md](../architecture.md#открытые-вопросы)) — список destiny / service / роли с цифрами покрытия и подсветкой непокрытых задач и веток.

## Раскладка тест-кейсов рядом с destiny

Формат закреплён: тесты для destiny живут в подкаталоге `tests/` рядом с `destiny.yml`. Один тест — одна папка с произвольным человекочитаемым именем; точка входа — `case.yml`. Этот выбор закрывает open Q №2 (см. ниже).

```
destiny-<name>/
├── destiny.yml
├── tasks/
│   └── main.yml
├── templates/
└── tests/                              # все тесты этого destiny
    ├── install-and-ping/               # один тест-кейс
    │   ├── case.yml                    # обязательная точка входа
    │   ├── prepare.yml                 # ОПЦ.: задачи до основного apply
    │   ├── verify.yml                  # ОПЦ.: если ассерций много — выносить сюда
    │   └── cleanup.yml                 # ОПЦ.: задачи после verify (внешние ресурсы)
    └── failover-restart/
        └── case.yml
```

Рабочий пример с пояснениями к каждому блоку — в [examples/destiny/redis/tests/install-and-ping/case.yml](../../examples/destiny/redis/tests/install-and-ping/case.yml).

### Что декларирует `case.yml`

Минимальный кейс описывает четыре вещи:

1. **`stand:`** — эфемерное окружение (`driver` / `image` / `mode: push` / `init`). **Single-host формат закрыт** (open Q №3 частично): ephemeral docker-container per-case, `mode: push` (Keeper-side render → oneshot `soul apply` в контейнере). Digest-pin образа (`image: name@sha256:…`) разрешён и поощряется (воспроизводимость). Сеть включена по умолчанию (нужна для `core.pkg`), отключается явно `stand.network: none`. Тип init-системы контейнера задаётся `stand.init` (см. ниже). **Multi-host `stand:` (топология кластера) — остаётся открытым** (расширение для L3, propose-and-wait по формату топологии — см. open Q №3 ниже).
2. **`input:`** — значения параметров destiny для прогона. Имя destiny указывать не нужно: фреймворк берёт его из `../../destiny.yml`, рядом с которым лежат `tests/`.
3. **`expect_idempotent:`** — флаг двойного прогона, **по умолчанию `true`** (open Q №5 закрыт: идемпотентность обязательна с opt-out). Второй apply того же `input` → все `register.changed == false`; иначе кейс падает. Кейс с осознанно-неидемпотентным шагом отключает проверку явным `expect_idempotent: false`.
4. **`verify:`** — проверки результата.

### `stand.init` — init-система контейнера-стенда

Опциональное поле `stand.init` задаёт, что запускается как PID 1 внутри L2-стенда. Enum, по умолчанию `none`:

| Значение | Что даёт |
|---|---|
| `none` (или отсутствие поля) | Лёгкий контейнер без init: процесс `sleep infinity` как PID 1, `soul apply` гоняется поверх. Подходит большинству L2-кейсов — пакеты, файлы, exec-проверки. Это прежнее поведение, дефолт. |
| `systemd` | Контейнер с настоящим `systemd` как PID 1 (`/sbin/init`, `privileged` + `cgroupns=host` + tmpfs на `/run` и `/run/lock`). Нужен L2-кейсам, которые реально дёргают `systemctl` и юниты. |

**Когда нужен `systemd`.** Только если кейс проверяет поведение, требующее живой init-системы:

- `core.service.restarted` с `daemon_reload`: перезаписали unit-файл (`core.file.present`) → `systemd` выставляет `NeedDaemonReload=yes` → рестарт обязан подхватить новое определение юнита;
- `enable`/`start` реальных сервисов (а не stub-процессов) и проверка `systemctl is-active` / `is-enabled`;
- systemd-таймеры и прочие юниты, чьё состояние спрашивают через `systemctl show`.

Для всего остального `init: systemd` избыточен — берёт `privileged` и тяжелее стартует, поэтому дефолт `none`.

**Требования и skip.** Режим `systemd` запускается **только** под build-tag `integration` и требует `docker` + `privileged`. На окружении без них (или без docker вообще) кейс делает **skip, а не fatal** — непрогон не считается регрессом (дефолтный `make test` помечает такие кейсы `Skipped`, как и всю L2-форму). Образ стенда переиспользует общий **debian-12 systemd-профиль** (тот же `debian-12.Dockerfile`, что у e2e-live): стенд собирается из этого Dockerfile с `Entrypoint /sbin/init`. Поле `image` при `init: systemd` по схеме обязательно, но фактически игнорируется (стенд берётся из Dockerfile) — в кейсе указывают эталонный тег для читаемости.

Краткий пример стенда с реальным systemd:

```yaml
stand:
  driver: docker
  image:  debian:12-slim    # обязателен по схеме; при init: systemd фактически игнорируется
  mode:   push
  init:   systemd
```

Рабочий кейс целиком (доказывает `daemon_reload=auto`: переписанный unit → `NeedDaemonReload` → рестарт применяет v2) — [`examples/destiny/service-reload/_trial/scenario/verify-l2/tests/auto-reload/case.yml`](../../examples/destiny/service-reload/_trial/scenario/verify-l2/tests/auto-reload/case.yml).

### Verify — через те же destiny, что и в проде

Verify-блок устроен как последовательность задач, каждая из которых исполняет destiny/модуль и сравнивает поля их `output:` с ожидаемыми через блок `expect:` (формат тот же, что в service-уровневых `tests/*.yml`, см. [examples/service/redis-cluster/tests/smoke.yml](../../examples/service/redis-cluster/tests/smoke.yml)). Отдельного DSL-я ассерций **нет** — это сознательное решение:

- Если состояние хоста проверяется через `redis-cli ping` или `systemctl is-active` — пишем это явно через `core.exec.run` и сверяем `output.stdout`.
- Если проверка повторяет то, что умеет сам destiny (`action: ping`, `action: replication_status`) — переиспользуем его. Самосогласованная проверка: destiny одновременно и реализация, и часть контракта.
- Если нужен особенный «file_present» / «service_running» — это означает, что в core-модулях не хватает соответствующего `state` (или его `output:`-полей), и проблему надо решать там, а не дублировать в отдельном DSL для тестов.

Конкретный набор ключей `expect:` (`stdout`, `stdout_contains`, `exit_code`, `output_equals`, …) пока **не зафиксирован** — это пересекается с **open Q №8** (что именно измерять). На уровне примеров используем минимум `stdout` / `stdout_contains` / `exit_code`; полный список зафиксируется отдельно, до начала реализации фреймворка.

### Опциональные соседи

Все три файла рядом с `case.yml` опциональные; кейс прогоняется и без них:

| Файл | Когда нужен |
|---|---|
| `prepare.yml` | preconditions до основного `input` apply: поднять Vault-stub с тестовыми секретами, скачать fixture-файлы, развернуть зависимый сервис в side-контейнере. |
| `verify.yml` | если ассерций становится много, выносим их из `case.yml` и подключаем как `verify: !include verify.yml` (точный синтаксис include — backlog test-фреймворка; шаблонизатор выражений зафиксирован [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md`](../templating.md)). |
| `cleanup.yml` | задачи после verify, нужны если кейс подложил внешние ресурсы (S3-объекты, DNS-записи, реальные cloud-VM через cloud-driver). При driver=docker обычно не нужен — снос контейнера уничтожает всё. |

### Где НЕ живут тесты

- **service-уровневые `tests/*.yml`** ([пример](../../examples/service/redis-cluster/tests/smoke.yml)) — это **другая** сущность: smoke/system-test после успешного `incarnation.create`. Прогоняются на реальных Souls в реальной incarnation, не на эфемерном стенде. Совпадение имени папки `tests/` намеренное — но раскладка внутри другая (плоский список `tests/<name>.yml`, без подпапок).
- **Coverage-метрики и инструмент-прогона** — не часть тест-файлов. Тесты декларативны; что собирается с прогона и в каком виде хранится — отдельный слой (open Q №6, №8).

### Три уровня тестов: destiny-molecule vs scenario-тест vs service-smoke

После [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) у scenario появился собственный механизм тестов — это **третья** сущность, отдельная от двух выше:

| | destiny-molecule | scenario-тест | service-smoke |
|---|---|---|---|
| **Что тестирует** | один destiny на одном хосте | одну операцию сценария на топологии кластера | работающую incarnation после `create` |
| **Раскладка** | `destiny-<name>/tests/<case>/case.yml` | `scenario/<name>/tests/<case>/case.yml` | `service-<x>/tests/<name>.yml` (плоский) |
| **Стенд** | эфемерный, **один** хост | эфемерный, **multi-host** (топология) | реальные Souls, реальная incarnation |
| **Ассерты** | `output:`/state хоста, идемпотентность | + топология (кто master/replica), `incarnation.state` после коммита | smoke/system-проверки |
| **Формат `case.yml`/`verify:`/`expect:`** | этот документ | **переиспользует** этот формат, `stand:` расширен на multi-host | плоский `tests/<name>.yml` |

Формат `case.yml` / `verify:` / `expect:` для scenario-теста **переиспользуется отсюда** без отдельного DSL ассерций. Дельта scenario-теста (multi-host `stand:`, ассерты на топологию и `incarnation.state`) — в [`docs/scenario/orchestration.md §6`](../scenario/orchestration.md#6-двухуровневый-резолв-ресурсов).

> **Расширение open Q №3 (sandbox).** Single-host `stand:` (destiny-molecule, L2) **закрыт** (ephemeral docker, `mode: push`). Multi-host `stand:` для scenario-теста и ассерты на топологию/`incarnation.state` cross-host — **расширение** для L3, остаётся открытым (формат топологии — propose-and-wait), а не закрытое решение. Не закрывается молча; до решения multi-host scenario-кейсы — declarative-stub `stand:`.

### Уровни форматов case.yml: L0 vs L2

В `case.yml` сосуществуют **две формы**, отвечающие разным уровням теста ([ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)). Они различаются составом ключей:

| Форма | Уровень | Состав | Статус |
|---|---|---|---|
| `fixtures:` (input/essence/soulprint/vault/state + `default_destiny_source` для apply:destiny) / `mocks:` / `assert.{rendered_tasks,state_changes,state_after}` | **L0** (render-only, hermetic) | fixtures для render-пайплайна + ожидаемый план задач / дельта sets / итоговый `incarnation.state` | **MVP, исполняется** `soul-trial run` |
| `stand:` / `verify:` / `expect:` (+ `input:` без `fixtures:`) | **L2** (docker single-host, реальные модули; опц. `stand.init: systemd` — стенд с реальным systemd как PID 1) | эфемерный стенд + проверки через `core.exec.run` | **реализован test-only** (build-tag `integration`); прод-`soul-trial run` его **пропускает** (`Skipped`) до отдельной задачи прод-CLI-включения |

`assert.state_after` — L0-секция: ожидаемый итоговый `incarnation.state` = базовый `fixtures.state` + отрендеренные `state_changes.sets` (зеркало прод-коммита), полная сверка (лишний ключ в итоге — расхождение). `assert.dispatch` — L3-секция (осмыслена только на multi-host), L0-загрузчик отвергает её strict-декодом.

L2-форма исполняется **в Go-тестах keeper-модуля** (testcontainers, oneshot `soul apply`, in-process Keeper-side render — тот же `renderCase`, что у L0). Примеры с `stand:`/`verify:` (напр. [`redis/tests/install-and-ping/case.yml`](../../examples/destiny/redis/tests/install-and-ping/case.yml)) помечены в шапке `# L2 fixture (docker-стенд)`; прод-`soul-trial run` их пропускает (`Skipped`), не принимая непрогон за регресс. L0-форма — действующий пилот фреймворка, исполняется всегда.

#### L0-molecule для standalone destiny — scenario-обёртка `apply:destiny`

`soul-trial` L0 рендерит **scenario** (harness завязан на `scenario/<name>/main.yml`), а не standalone destiny напрямую. Для переиспользуемой (standalone) destiny L0-molecule — это **минимальная scenario-обёртка с единственной задачей `apply:destiny`** на эту же destiny. destiny не рендерится отдельным путём: единственная точка инвокации (scenario `apply:` / Keeper) сохраняется и в тестах.

Каноническая раскладка обёртки — подкаталог самой destiny, чтобы каждая destiny владела своим L0-харнессом:

```
destiny-<name>/
├── destiny.yml
├── tasks/
├── templates/
└── _trial/                                   # L0-харнесс этой destiny
    ├── service.yml                            # объявляет destiny[]: [{name: <name>, ref}] — зеркало прода
    └── scenario/
        └── apply/
            ├── main.yml                       # scenario: одна задача apply:destiny <name>
            └── tests/
                └── render-defaults/
                    └── case.yml               # L0-form: fixtures (+ default_destiny_source) + assert.rendered_tasks
```

**Резолв `apply:destiny` в L0 зеркалит прод-модель** ([slice A](../scenario/orchestration.md), [ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)), а не перебирает каталоги эвристикой:

1. **`apply:destiny`-имя → `service.yml::destiny[]`.** Имя должно быть объявлено в `destiny[]` сервиса кейса (для standalone-обёртки — её `_trial/service.yml`). **Необъявленная зависимость отвергается** той же ошибкой, что в проде (`apply:destiny` ссылается только на декларированную зависимость, ADR-007).
2. **URL → герметичный `file://`.** `case.yml::fixtures` несёт ключ **`default_destiny_source`** (то же имя, что у `keeper.yml`) — шаблон URL со схемой `file://` и подстановкой `{name}`, путь относительно service-root кейса (напр. `file://../../destiny/{name}` для cross-location, `file://destiny-{name}` для destiny внутри сервиса). Per-entry `destiny[].git` override побеждает шаблон, но в L0 тоже обязан быть `file://`. Любая не-`file://` схема → явная ошибка «L0 герметичен». `{name}` обязан жить в последнем сегменте пути; граница securejoin проходит по destiny-root (часть URL до `{name}`), так что `{name}` не вырвется через `../` за объявленный каталог destiny.

Прежняя формулировка «резолвер поднимается на пару уровней вверх к каталогу destiny» снята: эвристический перебор `[serviceRoot, parent, grandparent]` не отражал прод и врал на cross-location раскладке (сервис и standalone destiny в разных поддеревьях). Рабочие примеры — [`examples/destiny/node-exporter/_trial/`](../../examples/destiny/node-exporter/_trial/scenario/apply/main.yml), `redis-exporter/_trial/`, `redis-single/_trial/` (cross-location: service-root = `_trial/`, сама destiny уровнем выше в `examples/destiny/`); cross-location сервис → [`examples/service/redis/`](../../examples/service/redis/service.yml) (destiny в `examples/destiny/`). Запуск: `soul-trial run examples/destiny/<name>/_trial`.

## Идеи метрик coverage (тоже на проработку)

Грубый список — что *вероятно* стоит измерять, состав пересматривается при дизайне:

- **Task coverage** — каждая задача destiny (`tasks:` элемент) выполнена хотя бы в одном кейсе.
- **Branch coverage по `when`** — для каждого выражения собраны и truthy-, и falsy-результат (или все ветви, если это switch по enum).
- **Enum-value coverage** — каждое значение `enum:` входного параметра задействовано хотя бы в одном кейсе. Дополняет статическую проверку №1 в [soul-lint.md](../soul-lint.md): статика говорит «литерал в выражении легален», runtime говорит «значение реально прогнано на стенде».
- **Module coverage** — каждый custom-модуль из `required_modules:` хотя бы раз вызывался хотя бы в одном из своих state-форм. Если нет — либо запись лишняя, либо есть непокрытый сценарий. (Core-модули в `required_modules:` не декларируются — см. [«Адресация модулей»](../architecture.md#адресация-модулей).)
- **Output coverage** — каждое поле, объявленное в `output:` какой-то задачи, реально присвоено хотя бы в одном прогоне.

## Открытые вопросы

Все нужно решить **перед** реализацией.

1. **Форма инструмента и имя.** Отдельный бинарь? Подкоманда `keeper`? Часть тулинга вокруг `soul`? Имя — под propose-and-wait, не закрепляется молча.
2. ~~**Формат тест-кейсов.** Отдельные YAML рядом с destiny vs встроенный раздел внутри destiny.~~ **Закрыто:** отдельные файлы, раскладка `destiny-<name>/tests/<case-name>/case.yml` (+ опц. `prepare.yml` / `verify.yml` / `cleanup.yml`). См. раздел «Раскладка тест-кейсов рядом с destiny» выше.
3. **Sandbox-контейнер.** **Single-host — ЗАКРЫТ** (L2 Вариант A, [ADR-023](../adr/0023-trial-test-runner.md#adr-023-тест-раннер-trial-soul-trial-и-dsl-coverage)): ephemeral docker-container per-case, `stand: {driver, image, mode: push}`; digest-pin образа разрешён/поощряется (герметичность), сеть включена по умолчанию (нужна для `core.pkg`), `stand.network: none` — явный opt-out. **Multi-host `stand:` — ОСТАЁТСЯ ОТКРЫТЫМ:** scenario-тест требует топологию кластера (кто master/replica) и ассерты на топологию/`incarnation.state` cross-host — это расширение для L3, фиксируется отдельно через propose-and-wait по формату топологии, **не закрывается молча** (см. [`docs/scenario/orchestration.md §8`](../scenario/orchestration.md#8-открытые-вопросы-расширения-не-закрывать-молча)).
4. ~~**Real vs mock модули.**~~ **Закрыто: real-only.** L2 гоняет реальные модули в docker; быстрый «mock-режим» отдельной сущностью не вводится — его роль выполняет **L0 render-only** (assert на плане задач / state без хоста). Один mock-уровень (L0), один real-уровень (L2), без третьего промежуточного.
5. ~~**Идемпотентность.**~~ **Закрыто: обязательна с opt-out.** `expect_idempotent` по умолчанию `true` — второй apply того же `input` обязан дать все `register.changed == false`. Кейс с осознанно-неидемпотентным шагом отключает проверку явным `expect_idempotent: false`.
6. **Хранение coverage.** Postgres (часть Keeper) vs только CI-артефакт vs оба. Определяет, доступны ли в UI история и сравнение между ранами.
7. **CI gate.** Порог покрытия по умолчанию (warn / fail) и его область действия (per destiny / per service / глобально).
8. **Что именно измерять.** Список метрик выше — стартовая точка, не финальный набор.

## Зависимости

- Шаблонизатор выражений зафиксирован [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), нормативная спека — [`docs/templating.md`](../templating.md): инструментирование `when:` опирается на CEL (тот же engine, что и статические проверки `soul-lint`).
- Open Q «UI Keeper-а» — определяет, появится ли визуализация coverage в составе релиза.
- Связано со статическими проверками `soul-lint` (см. [soul-lint.md](../soul-lint.md)): статический и runtime-coverage — разные слои, нужны оба. Статика ловит мёртвые литералы и опечатки; runtime ловит непокрытые сценарии.

## См. также

- [concept.md](concept.md) — что такое destiny.
- [tasks.md](tasks.md) — формат `tasks/main.yml`, который тестируется.
- [input.md](input.md) — destiny-`input:`, значения которого передаются в `case.yml`.
- [`docs/scenario/orchestration.md`](../scenario/orchestration.md) — scenario-тесты: multi-host `stand:`, ассерты на топологию/`incarnation.state`.
