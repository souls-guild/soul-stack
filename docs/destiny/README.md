# Destiny — индекс

Документация по destiny — атомарному декларативному кирпичику Soul Stack-а («как привести один хост в нужное состояние»).

## С чего начать

| Документ | О чём |
|---|---|
| [concept.md](concept.md) | Что такое destiny, как соотносится с service / scenario / module, чем отличается от соседей. |
| [manifest.md](manifest.md) | Раскладка папки destiny-репо, формат `destiny.yml` (только манифест), правила про `version:` и `tasks:`. |
| [tasks.md](tasks.md) | **Полная спецификация** формата задач: все блоки (`module`, `include`, `parallel`, `loop`, `register`, `onchanges`, `onfail`, `require`, `retry`, `timeout`, …), соглашения об именовании, шаблонный контекст. Источник правды для имплементации DSL. |
| [input.md](input.md) | `input:`-контракт destiny: где валидируется, как используется, соотношение с общим стандартом [`docs/input.md`](../input.md). |
| [output.md](output.md) | `output:`-контракт destiny (top-level): симметричный `input:` блок-декларация того, какой результат destiny публикует caller-у; читается из scenario через `register:` на applier-задаче. |
| [vars.md](vars.md) | `vars.yml` — destiny-локалы. Жёсткие значения автора destiny; снаружи не переопределяются; могут ссылаться на `input.*`. |
| [testing.md](testing.md) | Molecule-style тестирование destiny на эфемерном стенде; раскладка `tests/<case>/`; open Q вокруг coverage-инструмента. |
| [production-conventions.md](production-conventions.md) | Чек-лист «прод-grade destiny»: passthrough-флаги, гибрид-правило сервис-аккаунта (DynamicUser vs ручной uid), обязательный systemd-hardening, supply-chain, изоляция. Эталон — `node-exporter` (stateful-аккаунт + supply-chain + привилегированные textfile-коллекторы). |

## Связанные документы

- [`docs/scenario/`](../scenario/README.md) — слой над destiny: оркестрация, `on:`/`where:`-таргетинг, `apply: { destiny: … }`. После [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) scenario наследует DSL-ядро задач из [tasks.md](tasks.md); граница destiny/scenario — рекомендация.
- [`docs/architecture.md`](../architecture.md) — слои выше и ниже destiny:
  - [Адресация модулей](../architecture.md#адресация-модулей), [Манифест модуля](../architecture.md#манифест-модуля) — слой под destiny.
  - [Service — структура и manifest](../architecture.md#service--структура-и-manifest), [Targeting и связь хостов](../architecture.md#targeting-и-связь-хостов) — слой над destiny.
  - [ADR-007](../adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — почему в `destiny.yml` нет поля `version:`.
- [`docs/input.md`](../input.md) — **общий** стандарт формата `input:` (применяется к destiny, scenario и манифесту модуля).
- [`docs/templating.md`](../templating.md) — спека шаблонизатора (ADR-010): CEL для выражений в задачах destiny, Go text/template для `templates/*.tmpl`, маркер `${ … }`, `core.file.rendered` как рендер-модуль.
- [`docs/soul-lint.md`](../soul-lint.md) — статические проверки destiny на этапе CI/IDE.
- [`docs/module-collections.md`](../module-collections.md) — namespace-префикс в адресации модулей.
- [`examples/destiny/redis/`](../../examples/destiny/redis/) — рабочий пример полной раскладки.
