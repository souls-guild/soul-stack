# Scenario — индекс

Документация по scenario — оркестрационному слою Soul Stack-а («как провести одну операцию над целым кластером»: `create`, `add_user`, `restart`, `add_replica`, …).

Scenario — единица операции над [Incarnation](../architecture.md#incarnation--runtime-инстанс-сервиса). Папка `scenario/<name>/` в git-репо сервиса, точка входа `main.yml`. Версия — git ref service-репо ([ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте)).

## С чего начать

| Документ | О чём |
|---|---|
| [concept.md](concept.md) | Что такое scenario в новой модели, граница с destiny (рекомендация, не стена), declared-роль vs actual-роль, role-agnostic essence. |
| [orchestration.md](orchestration.md) | **Нормативная спецификация** оркестрационного слоя: `on:`/`where:`-таргетинг, probe-идиома, двухуровневый резолв ресурсов, тесты сценария, barrier/state-commit инвариант. DSL-ядро задач — делегировано в [destiny/tasks.md](../destiny/tasks.md). |
| [ADR-043 §7/§8](../adr/0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) | Политика state-commit при батчевом scenario-прогоне: **per-incarnation state-commit** (батч = N инкарнаций по Leg-ам, B1). Преемник удалённого Tide per-Surge state-commit. | Семантика commit-а БД при Voyage `kind=scenario`. |

## Связанные документы

- [`docs/destiny/tasks.md`](../destiny/tasks.md) — **полная спецификация DSL-ядра задач** (`module`, `include`, `block`, `parallel`, `loop`, `register`, `onchanges`/`onfail`/`require`, `retry`, `timeout`, `changed_when`/`failed_when`, шаблонный контекст). Scenario наследует это ядро целиком; [orchestration.md](orchestration.md) описывает только дельту scenario поверх него.
- [`docs/architecture.md`](../architecture.md):
  - [ADR-008](../adr/0008-coven-stable-tags.md#adr-008-coven--только-стабильные-логические-теги) — Coven как стабильные логические теги (не роль).
  - [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — scenario получает полную DSL задач destiny; граница с destiny — рекомендация.
  - [Service — структура и manifest](../architecture.md#service--структура-и-manifest) — раскладка service-репо, где живёт `scenario/`.
  - [Targeting и связь хостов](../architecture.md#targeting-и-связь-хостов) — `on:`/`where:`, контракт резолвера, probe как механизм волатильной роли.
  - [Incarnation — runtime-инстанс сервиса](../architecture.md#incarnation--runtime-инстанс-сервиса) — над чем работает scenario, `state`/`status`/`error_locked`.
- [`docs/destiny/concept.md`](../destiny/concept.md) — что такое destiny, чем отличается от scenario.
- [`docs/destiny/testing.md`](../destiny/testing.md) — разграничение scenario-тестов, destiny-molecule и service-smoke.
- [`docs/input.md`](../input.md) — **общий** стандарт формата `input:` (применяется к destiny, scenario и манифесту модуля).
- [`docs/templating.md`](../templating.md) — спека шаблонизатора (ADR-010): CEL для всех scenario-выражений (`where:`/`when:`/`changed_when:`/`failed_when:`/`until:`, `params:`, `apply: input:`, `on:`-литералы), маркер `${ … }`, граница с Go text/template, footgun-ы `soulprint.where(...)` vs `soulprint.hosts.where(...)`.
- [`docs/soul-lint.md`](../soul-lint.md) — статические проверки (в т.ч. backlog для scenario-специфики).
- [`examples/service/redis/`](../../examples/service/redis/) — рабочий пример service-репо с раскладкой `scenario/` (create + операционные `add_node`/`remove_node`/`reshard`/`restart`).
