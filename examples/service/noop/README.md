# noop

A minimal example service for the **E2E scenario-runner** (`M2.x.scenario-runner`).
Consists of a single `create` scenario that runs `core.exec.run` with
`echo hello` on every host in the incarnation. Writes nothing to
`incarnation.state`, and has no dependency on cloud providers, the templating engine,
or custom modules.

## Layout

```
noop/
├── service.yml                       # manifest: state_schema_version=1, empty state_schema
├── essence/
│   └── _default.yaml                 # baseline essence: one demo field `greeting`
└── scenario/
    └── create/
        └── main.yml                  # task: core.exec.run "echo hello"
```

No `migrations/` directory: `state_schema_version = 1`, no migrations needed
([ADR-019](../../../docs/adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

## Purpose

- **E2E fixture for scenario-runner.** A minimally sufficient example: one
  core module, no cross-host coordination, no Vault/Cloud.
  Suits a smoke test "the runner came up, played the scenario, returned
  a RunResult with no errors."
- **Spec compliance.**
  - `service.yml` — per [docs/service/manifest.md](../../../docs/service/manifest.md).
  - `scenario/create/main.yml` — per [docs/scenario/orchestration.md](../../../docs/scenario/orchestration.md)
    and the task DSL core [docs/destiny/tasks.md](../../../docs/destiny/tasks.md).
  - `core.exec.run` module parameters — `cmd:` (binary name) + `args:` (argv list,
    no shell), see [ADR-015](../../../docs/adr/0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list).

## Validation

```bash
./soul-lint/bin/soul-lint validate-service  examples/service/noop/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/noop/scenario/create/main.yml
```

Both should return exit 0 and `OK: <path>`.

## What's deliberately not here

- `migrations/` — `state_schema_version = 1`, no migrations needed.
- `destiny[]` / `modules[]` in `service.yml` — only core modules are used.
- `input:` in `scenario/create/main.yml` — the scenario takes no inputs.
- `templates/` / `vars.yml` / `tests/` — not required for a smoke fixture.
- `on:` / `where:` — deliberately absent: an omitted `on:` means "the whole
  incarnation" ([orchestration.md §3](../../../docs/scenario/orchestration.md)),
  which is exactly what's needed for the smoke test.
