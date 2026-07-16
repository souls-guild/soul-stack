# noop

Минимальный пример сервиса для **E2E scenario-runner-а** (`M2.x.scenario-runner`).
Состоит из одного сценария `create`, который запускает `core.exec.run` с
`echo hello` на каждом хосте incarnation. Не пишет ничего в `incarnation.state`,
не зависит от cloud-провайдеров, шаблонизатора и custom-модулей.

## Раскладка

```
noop/
├── service.yml                       # манифест: state_schema_version=1, пустой state_schema
├── essence/
│   └── _default.yaml                 # baseline-essence: одно demo-поле `greeting`
└── scenario/
    └── create/
        └── main.yml                  # task: core.exec.run "echo hello"
```

Каталога `migrations/` нет: `state_schema_version = 1`, миграции не нужны
([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## Назначение

- **E2E-фикстура для scenario-runner.** Минимально достаточный пример: один
  core-модуль, никакого cross-host coordination, никакого Vault/Cloud.
  Подходит для smoke-теста «runner поднялся, проиграл сценарий, вернул
  RunResult без ошибок».
- **Соответствие spec-ам.**
  - `service.yml` — по [docs/service/manifest.md](../../../docs/service/manifest.md).
  - `scenario/create/main.yml` — по [docs/scenario/orchestration.md](../../../docs/scenario/orchestration.md)
    и DSL-ядру задач [docs/destiny/tasks.md](../../../docs/destiny/tasks.md).
  - Параметры модуля `core.exec.run` — `cmd:` (имя бинаря) + `args:` (argv-список,
    без shell), см. [ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list).

## Валидация

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/noop/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/noop/scenario/create/main.yml
```

Оба должны давать exit 0 и `OK: <path>`.

## Чего здесь специально нет

- `migrations/` — `state_schema_version = 1`, миграции не нужны.
- `destiny[]` / `modules[]` в `service.yml` — используются только core-модули.
- `input:` в `scenario/create/main.yml` — сценарий не принимает входов.
- `templates/` / `vars.yml` / `tests/` — не требуются для smoke-фикстуры.
- `on:` / `where:` — отсутствуют сознательно: опущенный `on:` означает «весь
  incarnation» ([orchestration.md §3](../../../docs/scenario/orchestration.md)),
  что и нужно для smoke-теста.
