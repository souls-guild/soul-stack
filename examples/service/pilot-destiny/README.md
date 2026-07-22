# pilot-destiny — `apply:destiny` pilot (slice A) + `include:` (slice B)

A minimal service demonstrating **`apply:destiny`** — delegating scenario work to a
reusable destiny with an isolated render pass (V2, ADR-009) — and **`include:`**
(expanding neighboring files into a flat list before render).

## What it demonstrates

- **scenario-include:** `scenario/create/main.yml` includes the neighboring
  `marker.yml` via `include:` — expanded BEFORE render (two-level resolution
  scenario-local → service-level, [orchestration.md §6](../../../docs/scenario/orchestration.md#6-two-level-resource-resolution)).
  Inside `marker.yml` is a task `apply: { destiny: pilot-flat, input: {…} }`.
- **apply:destiny:** the apply task expands into destiny tasks (spliced into the
  overall plan with through-indices).
- **within-destiny include:** `pilot-flat/tasks/main.yml` includes
  `record.yml` via `include:` — within-destiny include is expanded when the
  destiny loads ([destiny/tasks.md §4](../../../docs/destiny/tasks.md#4-basic-blocks)).
  The resulting flat list is `core.file.present` (0) + `core.exec.run` (1).
- **Isolation:** the destiny sees ONLY its own `input:` (`marker_file`/`marker_payload`)
  passed via `apply.input`. Scenario scope (`input.path`/`input.content`, vars,
  register, soulprint) does NOT reach the destiny env — a structural boundary.
- **Default fallback:** `marker_mode` is not passed in `apply.input` → it falls back
  to the destiny contract's `default` (`0644`).

## Destiny layout

In this fixture the `pilot-flat` destiny lives next to the service (`pilot-flat/`) —
the hermetic L0 run loads it from the local tree, git is not needed. In production the
destiny git URL is derived from `keeper.yml::default_destiny_source` + `{name}`, and
`ref` from `service.yml → destiny[]` (ADR-007).

## Running L0

```sh
cd keeper
go run ./cmd/soul-trial run ../examples/service/pilot-destiny/scenario/create/tests/render-flat/case.yml
```

Expected: `PASS` — two destiny tasks with resolved input.
