# hello-world

Минимальный пример сервиса для **E2E с реальным commit-ом в `incarnation.state`**.
В отличие от [`noop`](../noop/README.md) (там `core.exec.run echo`,
`state_changes: {}` — state не меняется), здесь сценарий `create`:

1. пишет greeting-файл на каждом хосте incarnation через `core.file.present`;
2. фиксирует путь к файлу в `incarnation.state.greeting_file` (`state_changes.sets`).

Это даёт smoke-проверку всей цепочки: input → CEL-интерполяция → apply на хосте →
cross-host барьер → commit state в Postgres.

## Раскладка

```
hello-world/
├── service.yml                       # манифест: state_schema_version=1, state_schema с полем greeting_file
├── essence/
│   └── _default.yaml                 # baseline-essence: greeting (подложка)
└── scenario/
    └── create/
        └── main.yml                  # input.greeting → core.file.present → state_changes.sets.greeting_file
```

Каталога `migrations/` нет: `state_schema_version = 1`, миграции не нужны
([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## Назначение

- **E2E-фикстура с реальным state-change.** Самый простой service, доводящий
  цепочку до записи в `incarnation.state`. Подходит для проверки «runner проиграл
  сценарий, файл на хосте создан, Keeper зафиксировал greeting_file в Postgres».
- **Демонстрация CEL-интерполяции input.** `content: "${ input.greeting }"` —
  значение приходит из scenario-`input:` и подставляется на CEL-фазе рендера
  ([ADR-010](../../../docs/adr/0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)).
- **Соответствие spec-ам.**
  - `service.yml` — по [docs/service/manifest.md](../../../docs/service/manifest.md).
  - `scenario/create/main.yml` — по [docs/scenario/orchestration.md](../../../docs/scenario/orchestration.md)
    и DSL-ядру задач [docs/destiny/tasks.md](../../../docs/destiny/tasks.md).
  - `input:` — общий стандарт [docs/input.md](../../../docs/input.md).
  - `core.file.present` с inline-`content` — [ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)
    (`core.copy` сознательно не выделяется — покрывается `core.file.present`).
  - `state_changes.sets` — формат [ADR-009](../../../docs/adr/0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) /
    [ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl).

## Валидация

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/hello-world/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/hello-world/scenario/create/main.yml
```

Оба должны давать exit 0 и `OK: <path>`.

## Чего здесь специально нет

- `migrations/` — `state_schema_version = 1`, миграции не нужны.
- `destiny[]` / `modules[]` в `service.yml` — используются только core-модули.
- `templates/` — `content` передаётся inline через `${ input.greeting }`, без `.tmpl`-файла.
- `on:` / `where:` — отсутствуют сознательно: опущенный `on:` означает «весь
  incarnation» ([orchestration.md §3](../../../docs/scenario/orchestration.md)).
- `essence.greeting` как fallback — на pilot `input.greeting` обязателен
  (`required: true`); essence остаётся подложкой для будущих сценариев.
