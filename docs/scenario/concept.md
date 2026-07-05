# Scenario — концепция

Scenario — **оркестрационный слой** Soul Stack-а: одна операция над целым кластером (`create`, `add_user`, `update_acl`, `add_replica`, `restart`, …). В Soul Stack-словаре scenario соответствует комбинации Salt Orchestration + State, но в одном языке и в одном месте.

Папка `scenario/<name>/` в git-репо сервиса, точка входа `main.yml`. Версия — git ref service-репо ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)).

> **Второй канал авто-дискавери — `upgrade/`.** Version-к-версии upgrade-сценарии живут в отдельном каталоге `upgrade/<slug>/` рядом со `scenario/` (self-describing ключ `from:` — версии-источники), запускаются апгрейдом (`POST /v1/incarnations/{name}/upgrade`) и в обычных day-2-списках сценариев не показываются. Дизайн — [ADR-0068](../adr/0068-service-upgrade-v2.md).

## Что такое scenario в новой модели

До [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) действовал инвариант «scenario только `apply: { destiny: … }`, без `module:`». **Этот инвариант снят.** Scenario получает:

- **Полное DSL-ядро задач destiny** — целиком из [destiny/tasks.md](../destiny/tasks.md): `module:` (включая изменяющие модули, не только read-only), `templates`, task-level `vars:`, `register:`, `loop:`, `block:`, `parallel:`, `onchanges:`/`onfail:`/`require:`, `changed_when:`/`failed_when:`, `retry:`, `timeout:`. Это ядро **не дублируется** в scenario-спеке — источник правды один.
- **Оркестрационный слой сверху** — то, чего у destiny нет: таргетинг (`on:`/`where:`), кросс-хостовая координация, `apply: { destiny: … }`, запись `incarnation.state` через `state_changes`. Нормативная спецификация дельты — [orchestration.md](orchestration.md).

destiny при этом **остаётся** самостоятельной сущностью (см. границу ниже): переиспользуемый, независимо-версионируемый (git ref, [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)), изолированный, molecule-тестируемый кирпич «как привести один хост в состояние X».

## Граница destiny / scenario — рекомендация, не стена

Раньше граница была инвариантом изоляции. Теперь это **рекомендация**: «переиспользуемое или критичное — выноси в destiny». Критерий выноса в destiny — три «да»:

1. **Переиспользование.** Эта логика нужна больше чем одному сценарию (или больше чем одному сервису).
2. **Нужна molecule-идемпотентность.** Логику стоит покрыть отдельным destiny-molecule-тестом с гарантией повторного прогона без изменений ([destiny/testing.md](../destiny/testing.md)).
3. **Изолируемо.** Логику можно описать как «привести один хост в состояние X», не зная про БД Keeper-а, топологию кластера и других Souls.

Все три «да» → выноси в destiny, вызывай через `apply: { destiny: … }`. Иначе допустима инлайн-реализация `module:`-шагами прямо в scenario.

> **Изоляция и `output:` destiny.** destiny публикует собственный результат через декларированный top-level `output:` (симметрично `input:`, чтение caller-ом — `register:` на applier-задаче). Изоляцию это **не** нарушает: destiny отдаёт своё, не читает чужое. Подробнее — [destiny/output.md](../destiny/output.md), [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация).

> **Риск и его смягчение.** Инлайн-`module:`-мутации в scenario не проходят через независимое git-версионирование destiny ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)) — точечная правка в `scenario/<name>/main.yml` минует ревью и тегирование, которое получил бы отдельный destiny-репо. Смягчение: рекомендация выше + backlog lint-warn в [soul-lint.md](../soul-lint.md) («инлайн-мутация в scenario без выноса в destiny»). Это эрозия дисциплины ADR-007, осознанно принятая ради простоты типовых операций — см. [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация).

| | Destiny | Scenario |
|---|---|---|
| **Уровень** | один хост | один кластер (один incarnation) |
| **Знает про другие хосты?** | нет | да (через `on:`/`where:` и `soulprint.where`) |
| **Пишет state в БД?** | нет | да (`state_changes`) |
| **Доступ к `essence.*` в шаблонах** | нет (только `input:`) | да (merged essence после pipeline) |
| **DSL задач** | [destiny/tasks.md](../destiny/tasks.md) | то же ядро + оркестрационная дельта ([orchestration.md](orchestration.md)) |
| **Версия** | git ref destiny-репо | git ref service-репо |
| **Тестирование** | molecule на эфемерном стенде (один хост) | свой механизм: multi-host стенд + ассерты на топологию/`incarnation.state` ([orchestration.md](orchestration.md), [destiny/testing.md](../destiny/testing.md)) |

## declared-роль vs actual-роль

Роль хоста (master / replica) в Soul Stack-е — **не Coven** ([ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги)). Существует две концептуально разные роли:

- **declared-роль** — задекларированная оператором, живёт **только** в `incarnation.spec.hosts[].role`. Используется на bootstrap-операции `create`, когда Redis ещё не запущен и probe-ить нечего: первый master / первые replica берутся из `spec`. Также служит для топологии и аудита. essence её **не потребляет** (см. ниже). В scenario declared-топология прогона доступна аксессором `soulprint.hosts` (список хостов со стабильными фактами, scenario-only; спецификация — [orchestration.md §4.1](orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор)); destiny получает её только через явный `apply: input:`.
- **actual-роль** — фактическая роль в данный момент (кто сейчас реально master по `redis-cli role`). **Волатильна**, меняется при failover. Не хранится нигде стабильно: получается **живым probe-шагом** непосредственно перед использованием (`module: core.exec.run` + `register:`), таргетинг следующего шага идёт по `where:` от этого `register:` ([orchestration.md](orchestration.md)). После failover — просто новый probe, никакого кэша и механизма свежести.

> Волатильное (роль) **не живёт в Soulprint**. Soulprint — только стабильные и медленно меняющиеся факты хоста. Нет волатильных soulprint-фактов, нет коллектора роли, нет механизма свежести. Это следствие [ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) и зафиксировано там же.

## Essence role-agnostic

Essence-слой по роли **удалён**: иерархия pipeline-а больше **не содержит** ступени `role/<Y>.yaml`. Порядок сборки — `default → os → coven → incarnation.spec` (см. [architecture.md → «Essence: pipeline сборки»](../architecture.md#essence-pipeline-сборки)). essence не потребляет declared-роль и не слоится по роли вовсе.

Параметры, которые раньше зависели от роли (то, что было в `role/master.yaml` / `role/replica.yaml`), переезжают в **destiny** и передаются через `input:` по результату probe-роли. То есть scenario сначала probe-ит actual-роль, затем вызывает `apply: { destiny: …, input: { … } }` с разными значениями для master- и replica-хостов — а не подкладывает их через essence-слой.

> Перенос конкретных значений `role/*.yaml` в destiny-`input:` — отдельная задача имплементации, см. упоминание в [orchestration.md](orchestration.md). Здесь фиксируется только модель: essence role-agnostic, роль-зависимость переезжает в destiny по probe.

## См. также

- [orchestration.md](orchestration.md) — нормативная спецификация оркестрационной дельты scenario.
- [destiny/tasks.md](../destiny/tasks.md) — DSL-ядро задач (наследуется scenario целиком).
- [destiny/concept.md](../destiny/concept.md) — что такое destiny, чем отличается от scenario.
- [architecture.md → ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги), [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — закрепляющие решения.
- [architecture.md → «Targeting и связь хостов»](../architecture.md#targeting-и-связь-хостов) — `on:`/`where:`, контракт резолвера.
- [destiny/testing.md](../destiny/testing.md) — разграничение scenario-тестов / destiny-molecule / service-smoke.
