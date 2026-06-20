# core.noop

No-op-шаг: ничего не делает и всегда возвращает успех без изменения состояния
(`changed = false`). **Soul-side**, статически встроен в `soul`-бинарь.
Реализация — [`soul/internal/coremod/noop/noop.go`](../../../../soul/internal/coremod/noop/noop.go).

Это verb-модуль: единственное состояние — `run` (без declarative-семантики
«привести к состоянию»). `params:` не имеют схемы — модуль существует как
синтаксический якорь, а не как операция над ресурсом.

## Назначение

- **barrier-якорь.** Задача `core.noop.run`, обращающаяся к `register.*`
  нескольких предыдущих задач, даёт точку, в которой фреймворк дожидается их
  завершения. Сам барьер обеспечивает не модуль, а граф зависимостей (`require:`
  / register-ссылки) — `core.noop.run` лишь пустое тело такой задачи.
- **placeholder.** Пустой шаг в каркасе destiny/scenario до появления реальной
  логики, либо носитель `output:`-проекции: `output:` читает `register.*`
  предыдущих задач, собственной работы шаг не выполняет.

## Read-only: ничего не меняет

`changed = false` **всегда**, конструктивно и ненастраиваемо — no-op не меняет
состояние хоста. Прецедент — read-probe-модули ([`core.http`](../http/README.md),
[`core.exec`](../exec/README.md)): модуль не объявляет drift, интерпретацию
задаёт scenario. Идемпотентность — по природе (пустая операция).

## States

| State (verb) | Назначение | `changed` |
|---|---|---|
| `run` | No-op: ничего не делает, успех без change. | `false` всегда. |

## run — params

Схемы нет: `params:` отсутствует или `{}`. Любые ключи принимаются и
игнорируются — задача-якорь не имеет входов.

## Capabilities / side-effects

- **Ничего не выполняет и не пишет.** В манифесте
  ([`noop.yaml`](../../../../shared/coremanifest/noop.yaml)) `required_capabilities`
  пуст: нет ни `exec_subprocess`, ни `fs_write_root`, ни `network_outbound`.
- **`changed = false` конструктивно** (см. «Read-only: ничего не меняет»).
- **Errand-safe** ([ADR-033](../../../adr/0033-errand.md)): no-op безопасен к
  ad-hoc invocation через Errand pull-контур (модуль реализует `ErrandReadSafe`).

## Output / register

`{ changed: false }`. Собственного output шаг не отдаёт; полезные данные
собираются на уровне задачи через `output:`-проекцию `register.*` предыдущих
задач (см. «placeholder»).

## Примеры

### Barrier через register-зависимости

Три параллельных probe-задачи, затем `core.noop.run` собирает их результаты —
обращение к `register.ping`/`register.repl`/`register.mem` создаёт implicit
barrier (фреймворк ждёт завершения всех трёх перед стартом якоря):

```yaml
- name: Collect diagnose result
  module: core.noop.run
  when: input.action == 'diagnose'
  params: {}
  output:
    ping:              "${ register.ping.stdout }"
    replication_state: "${ register.repl.stdout }"
    used_memory:       "${ register.mem.stdout }"
```

### Placeholder-якорь

Барьер, который дожидается набора предыдущих задач через `require:` и ничего не
делает сам:

```yaml
- name: barrier
  module: core.noop.run
  require:
    - Install package
    - Render config
```
