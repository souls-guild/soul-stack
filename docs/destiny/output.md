# `output:` в destiny

Этот документ описывает **destiny-специфику** блока `output:` — декларацию того, какой результат destiny публикует наружу. По форме блок **симметричен `input:`** ([docs/destiny/input.md](input.md)): то же ядро схемы — один общий стандарт. Здесь — где `output:` живёт, как заполняется задачами `tasks/main.yml` и как читается caller-ом сценария.

## Источник правды на формат

Точные ключи (`type`, `enum`, `pattern`, `format`, `min_length`, `secret`, …), типы (`string`, `integer`, `number`, `boolean`, `array`, `object`) и правила валидации — те же, что у `input:`: общий стандарт [`docs/input.md`](../input.md). При расхождениях приоритет за тем документом. Любой новый ключ — propose-and-wait → правка [`docs/input.md`](../input.md) → потом этот файл и примеры.

`output:` — **симметричный** `input:` блок (одна и та же схема описания полей), не отдельный DSL. Это сознательно: один формат на оба контракта destiny (вход и выход), один источник правды.

## Где блок живёт

В корне `destiny.yml` (см. [manifest.md](manifest.md) → поле `output:`). Не в `tasks/main.yml`. Один destiny — один блок `output:`. Он **опционален**: если destiny ничего не возвращает caller-у, блок опускается целиком (caller получит только стандартные `.changed` / `.failed` своего `register:`).

## Семантика — destiny публикует, не читает

`output:` — **отдача** собственного результата destiny наружу. Это **не** механизм чтения чужого состояния:

- destiny **публикует** через декларированные `output:`-поля свой результат прогона.
- destiny **никогда не читает** контекст caller-а (scenario / другой destiny / state). Изоляция destiny не нарушается ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

Симметрия очевидна: `input:` — снаружи внутрь destiny, `output:` — изнутри наружу. Оба контракта декларируются destiny-ой, оба валидируются движком, ни один не разрешает destiny подсматривать чужой контекст.

## Как заполняется — task-level `output:` пишет в top-level поля

Top-level `output:`-блок в `destiny.yml` объявляет **схему** результата (имена полей + типы + валидация). Фактические значения собираются через task-level `output:` ([destiny/tasks.md §9](tasks.md#9-прочность-и-контроль-исполнения)): значения, объявленные в `output:` задачи, заполняют **объявленные** top-level `output:`-поля destiny.

Правила:

- Имя поля в top-level `output:` — точка соединения. Когда задача в `tasks/main.yml` пишет в свой task-level `output:` (`<имя>: "${ ... }"`), это значение присваивается одноимённому top-level полю destiny.
- Если задача публикует имя, **не объявленное** в top-level `output:` destiny — это ошибка валидации (destiny не возвращает того, чего нет в её output-схеме).
- Если top-level `output:`-поле объявлено как `required: true`, но к концу прогона ни одна задача его не заполнила — ошибка (поле не предоставлено вопреки контракту).
- Последняя запись побеждает: если две задачи пишут в одно и то же поле, действует значение последней записи (по порядку исполнения).

## Где валидируется

Defense in depth, по аналогии с `input:`:

1. **Soul собирает значения и проверяет** их по top-level `output:`-схеме перед тем, как отдать результат Keeper-у. Несоответствие схеме (тип, `enum`, `pattern`, отсутствие `required`) → destiny finally-failed.
2. **Keeper при приёме** проверяет ещё раз — страховка от рассинхронизации версий и багов на Soul-стороне.

Те же два раунда, тот же принцип, что у `input:` (см. [destiny/input.md → Где валидируется](input.md#где-валидируется)).

## Как читает caller — `register:` на applier-задаче

Scenario вызывает destiny через `apply: { destiny: ..., input: { ... } }` ([scenario/orchestration.md §2.1.1](../scenario/orchestration.md#211-register-на-applier-задаче--чтение-результата-destiny)). Applier-задача сценария ставит `register: <имя>`, и у `register.<имя>` две части с разным статусом реализации:

- **DSL-ядро `.changed` / `.failed` / `.timed_out` — реализовано.** Эти поля материализуются как **агрегат `OR`** по всем дочерним destiny-задачам applier-а (`changed = OR(child.changed)`, аналогично `failed` / `timed_out`; `skipped` — всегда `false`). Внешний `onchanges: [<имя>]` / `onfail: [<имя>]` / `when: register.<имя>.changed` резолвится по этому агрегату. Поля DSL-ядра присутствуют **независимо** от того, объявила ли destiny top-level `output:`.
- **`.<output-поле>` по объявленному top-level `output:`-контракту — ПЛАНИРУЕТСЯ.** Проброс прикладных полей destiny из её `output:`-блока в `register.<имя>.<output-поле>` пока **не реализован** (отдельный будущий slice — см. note ниже).

```yaml
- name: Apply the application database config
  apply:
    destiny: db-bootstrap
    input:
      name:    "${ input.db_name }"
      version: "${ essence.db_version }"
  register: bootstrapped

- name: Run migrations only if config actually changed
  when: register.bootstrapped.changed   # агрегат OR по дочерним задачам — РАБОТАЕТ
  module: core.noop.run
  params: {}
```

> **Проброс прикладного destiny-`output:` в `register.<applier>.<поле>` — вне объёма.** Когда output-проекция будет реализована, в `register.<имя>.<output-поле>` станут доступны поля, объявленные destiny в её top-level `output:`-блоке. До этого slice-а каноническая форма `register.<имя>.<output-поле>` (`register.bootstrapped.dsn` и т.п.) не резолвится — это слой orchestrator-а, не покрытый текущей реализацией applier-register. Реализованы только DSL-ядро `.changed` / `.failed` / `.timed_out` (агрегат `OR`, см. выше).

## `output:` ≠ версия артефакта

Появление / расширение `output:`-контракта destiny — это эволюция контракта, **не** повод вводить поле `version:` в `destiny.yml`. Версия destiny — git ref, под которым закоммичен файл ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)); правило применяется к `output:` ровно так же, как к `input:` и к остальному манифесту.

## Связь с `output:` scenario

Scenario `output:`-блока **нет**: scenario пишет результат в `incarnation.state` через `state_changes` ([architecture.md → Incarnation](../architecture.md#incarnation--runtime-инстанс-сервиса)), а не возвращает значения caller-у. Top-level `output:` — это destiny-сущность, симметричная destiny-`input:`. У scenario есть только task-level `output:` (часть DSL-ядра задач, [destiny/tasks.md §9](tasks.md#9-прочность-и-контроль-исполнения)) для внутренних `register:`-цепочек.

> **`register:` как источник `state_changes`.** `state_changes.sets` может
> читать `register.<task>.<поле>` probe-задачи прогона
> ([scenario/orchestration.md §7.1](../scenario/orchestration.md#71-грамматика-state_changes--список-crud-операций)).
> `TaskEvent.register_data` накапливается на Keeper-стороне (таблица
> `apply_task_register`), после барьера scenario-runner строит per-host
> register-карту и рендерит `sets`. Это сознательно **отдельно** от
> chaining-а внутри сценария: `register.*` в `where:` — волатильный рантайм-предикат
> до коммита, а `register.*` в `sets` — стабильный post-barrier-снимок.
> Прямой проброс destiny-`output:` (прочитанного через `register:` на
> applier-задаче) в `sets` для destiny-вызовов через `apply:` пока вне объёма —
> это тот же планируемый output-проекционный slice, что описан в разделе
> [«Как читает caller»](#как-читает-caller--register-на-applier-задаче). DSL-ядро
> `register.<applier>.changed`/`.failed`/`.timed_out` (агрегат `OR`) при этом уже
> материализуется и `sets` мог бы его читать; не материализуются именно
> прикладные `output:`-поля.

## См. также

- [`docs/input.md`](../input.md) — общий стандарт формата (ключи валидации одни и те же).
- [`docs/destiny/input.md`](input.md) — destiny-специфика входного контракта (симметричный документ).
- [manifest.md](manifest.md) — где `output:` живёт в `destiny.yml`.
- [tasks.md §9](tasks.md#9-прочность-и-контроль-исполнения) — task-level `output:` (заполняет объявленные top-level поля).
- [scenario/orchestration.md §2.1.1](../scenario/orchestration.md#211-register-на-applier-задаче--чтение-результата-destiny) — `register:` на applier-задаче: реализованный агрегат `.changed`/`.failed`/`.timed_out` vs планируемая output-проекция.
- [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — изоляция destiny: `output:` (отдача своего) её не нарушает.
