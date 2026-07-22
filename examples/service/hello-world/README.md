# hello-world

A minimal example service for **E2E with a real commit to `incarnation.state`**.
Unlike [`noop`](../noop/README.md) (there `core.exec.run echo`,
`state_changes: {}` — state doesn't change), here the `create` scenario:

1. writes a greeting file on every host of the incarnation via `core.file.present`;
2. records the file path in `incarnation.state.greeting_file` (`state_changes.sets`).

This gives a smoke check of the whole chain: input -> CEL interpolation -> apply on
host -> cross-host barrier -> commit state to Postgres.

## Layout

```
hello-world/
├── service.yml                       # manifest: state_schema_version=1, state_schema with the greeting_file field
├── essence/
│   └── _default.yaml                 # baseline essence: greeting (fallback)
└── scenario/
    └── create/
        └── main.yml                  # input.greeting -> core.file.present -> state_changes.sets.greeting_file
```

No `migrations/` directory: `state_schema_version = 1`, no migrations needed
([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## Purpose

- **E2E fixture with a real state change.** The simplest service that carries the
  chain through to a write in `incarnation.state`. Good for verifying "runner played
  the scenario, file created on host, Keeper committed greeting_file to Postgres".
- **Demonstrates CEL interpolation of input.** `content: "${ input.greeting }"` —
  the value comes from the scenario `input:` and is substituted during the CEL
  rendering phase
  ([ADR-010](../../../docs/adr/0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)).
- **Spec compliance.**
  - `service.yml` — per [docs/service/manifest.md](../../../docs/service/manifest.md).
  - `scenario/create/main.yml` — per [docs/scenario/orchestration.md](../../../docs/scenario/orchestration.md)
    and the task DSL core [docs/destiny/tasks.md](../../../docs/destiny/tasks.md).
  - `input:` — the general standard [docs/input.md](../../../docs/input.md).
  - `core.file.present` with inline `content` — [ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list)
    (`core.copy` is deliberately not a separate module — covered by `core.file.present`).
  - `state_changes.sets` — format [ADR-009](../../../docs/adr/0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) /
    [ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl).

## Validation

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/hello-world/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/hello-world/scenario/create/main.yml
```

Both should give exit 0 and `OK: <path>`.

## What's deliberately not here

- `migrations/` — `state_schema_version = 1`, no migrations needed.
- `destiny[]` / `modules[]` in `service.yml` — only core modules are used.
- `templates/` — `content` is passed inline via `${ input.greeting }`, no `.tmpl` file.
- `on:` / `where:` — deliberately absent: an omitted `on:` means "the whole
  incarnation" ([orchestration.md §3](../../../docs/scenario/orchestration.md)).
- `essence.greeting` as a fallback — for the pilot `input.greeting` is required
  (`required: true`); essence remains a substrate for future scenarios.
