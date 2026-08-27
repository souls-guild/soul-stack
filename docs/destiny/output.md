# `output:` in destiny

> **Implementation status.** The **declaration** works: a top-level `output:` block in
> `destiny.yml` parses and is validated as a schema. **Everything that moves a value
> through it does not exist yet.** Task-level `output:` is **refused** with
> `output_unsupported` ([destiny/tasks.md §9](tasks.md#9-strength-and-control-of-execution)),
> nothing collects the declared fields, neither Soul nor Keeper validates collected
> values, and `register.<applier>.<field>` does not resolve. This page describes the
> agreed **design** of one unbuilt slice; the sections below say per-section what is
> real today. Declaring `output:` in a destiny is legal and inert - it publishes
> nothing.

This document describes the **destiny-specific** block `output:` - a declaration of what result destiny publishes to the outside. The block is **symmetrical in shape to `input:`** ([docs/destiny/input.md](input.md)): the same core of the schema - one common standard. Here is where `output:` lives, how it is filled with tasks `tasks/main.yml` and how the scenario is read by the caller.

## Source of truth on the format

Exact keys (`type`, `enum`, `pattern`, `format`, `min_length`, `secret`, ...), types (`string`, `integer`, `number`, `boolean`, `array`, `object`) and validation rules are the same as for `input:`: general standard [`docs/input.md`](../input.md). In case of discrepancies, priority goes to that document. Any new key - propose-and-wait → edit [`docs/input.md`](../input.md) → then this file and examples.

`output:` - **symmetric** `input:` block (same field description scheme), not a separate DSL. This is deliberate: one format for both destiny contracts (input and output), one source of truth.

## Where the block lives

At the root `destiny.yml` (see [manifest.md](manifest.md) → field `output:`). Not in `tasks/main.yml`. One destiny - one block `output:`. It is **optional**: if destiny does not return anything to the caller, the entire block is omitted (the caller will receive only the standard `.changed` / `.failed` of its `register:`).

## Semantics - destiny publishes, does not read

`output:` - **giving** one's own result to destiny outward. This is **not** a mechanism for reading someone else's state:

- destiny **publishes** its run result through the declared `output:` fields.
- destiny **never reads** the caller's context (scenario / other destiny / state). Destiny isolation is not broken ([ADR-009](../adr/0009-scenario-dsl.md)).

The symmetry is obvious: `input:` is from outside to inside destiny, `output:` is from inside to outside. Both contracts are declared by destiny, and neither allows destiny to spy on someone else's context. ★ Only `input:` is validated by the engine today; validation of collected `output:` values is part of the unbuilt slice (see below).

## How it is filled in - task-level `output:` writes to the top-level fields — **PLANNED**

> **Not implemented.** Task-level `output:` is refused today (`output_unsupported`,
> [destiny/tasks.md §9](tasks.md#9-strength-and-control-of-execution)): the key was
> accepted and then silently dropped, which read as "the contract is filled" to every
> author who wrote it. The rules below are the design this slice will implement, not
> current behaviour.

Top-level `output:` block in `destiny.yml` declares the **schema** of the result (field names + types + validation). The actual values are collected through the task-level `output:` ([destiny/tasks.md §9](tasks.md#9-strength-and-control-of-execution)): the values declared in the `output:` task fill in the **declared** top-level `output:` destiny fields.

Rules (planned):

- Field name in top-level `output:` - connection point. When a task in `tasks/main.yml` writes to its task-level `output:` (`<name>: "${ ... }"`), this value is assigned to the top-level field destiny.
- If a task publishes a name **not declared** in the top-level `output:` destiny there is a validation error (destiny does not return something that is not in its output schema).
- If the top-level `output:` field is declared as `required: true`, but by the end of the run no task has filled it in - an error (the field is not provided contrary to the contract).
- Last entry wins: if two tasks write to the same field, the value of the last entry (in order of execution) applies.

## Where is validated — **PLANNED**

> **Not implemented.** No values are collected, so neither round runs. The two rounds
> below are the design; today a destiny's declared `output:` fields are never
> populated and never checked.

Defense in depth, similar to `input:`:

1. **Soul collects values and checks** them against the top-level `output:` scheme before giving the result to the Keeper. Schema inconsistency (type, `enum`, `pattern`, absence of `required`) → destiny finally-failed.
2. **Keeper checks again upon reception** - insurance against desynchronization of versions and bugs on the Soul side.

Same two rounds, same principle as `input:` (see [destiny/input.md → Where is validated](input.md#where-is-validated)).

## How the caller reads - `register:` on the applier task

Scenario calls destiny via `apply: { destiny: ..., input: { ... } }` ([scenario/orchestration.md §2.1.1](../scenario/orchestration.md)). The scenario's Applier task sets `register: <name>`, and `register.<name>` has two parts with different implementation status:

- **DSL core `.changed` / `.failed` / `.timed_out` - implemented.** These fields are materialized as **aggregate `OR`** for all child applier tasks (`changed = OR(child.changed)`, similar to `failed` / `timed_out`; `skipped` - always `false`). External `onchanges: [<name>]` / `onfail: [<name>]` / `when: register.<name>.changed` resolves for this unit. The DSL core fields are present **regardless** of whether destiny top-level `output:` is declared.
- **`.<output field>` according to the announced top-level `output:` contract - PLANNED.** Forwarding application fields of destiny from its `output:` block to `register.<name>.<output field>` has not yet been **implemented** (a separate future slice - see note below).

```yaml
- name: Apply the application database config
  apply:
    destiny: db-bootstrap
    input:
      name:    "${ input.db_name }"
      version: "${ vars.db_version }"
  register: bootstrapped

- name: Run migrations only if config actually changed
  when: register.bootstrapped.changed   # OR unit on child tasks - WORKING
  module: core.noop.run
  params: {}
```

> **The forwarding of applied destiny-`output:` to `register.<applier>.<field>` is out of scope.** When the output projection is implemented, the fields declared by destiny in its top-level `output:` block will become available in `register.<name>.<output field>`. Before this slice, the canonical form `register.<name>.<output field>` (`register.bootstrapped.dsn`, etc.) does not resolve - this is an orchestrator layer not covered by the current implementation of applier-register. Only the DSL core `.changed` / `.failed` / `.timed_out` is implemented (unit `OR`, see above).

## `output:` ≠ artifact version

The appearance/expansion of the `output:` contract destiny is an evolution of the contract, **not** a reason to introduce the `version:` field into `destiny.yml`. The destiny version is git ref, under which the file is committed ([ADR-007](../adr/0007-versioning-git-ref.md)); the rule applies to `output:` in exactly the same way as to `input:` and the rest of the manifest.

## Communication with `output:` scenario

Scenario `output:` block **no**: a scenario writes its result to `incarnation.state` with a `core.state.<verb>` capture step ([scenario/orchestration.md §7.1](../scenario/orchestration.md#71-the-capture-verbs)), rather than returning values to the caller. Top-level `output:` is a destiny-entity, symmetrical to destiny-`input:`. A scenario task cannot carry task-level `output:` either - the key is refused on every kind in both entities ([destiny/tasks.md §9](tasks.md#9-strength-and-control-of-execution)); an internal chain between tasks is written with `register:` and `register.<name>.<field>`.

> **`register:` as the source of a capture's `value:`.** A `core.state.<verb>` step
> reads `register.<task>.<field>` like any other keeper task
> ([scenario/orchestration.md §7.1](../scenario/orchestration.md#71-the-capture-verbs)).
> Being an `on: keeper` task it sees the **keeper** register bucket — the registers of
> keeper-side tasks of previous Passages (`core.cloud.created`, `core.vault.kv-read`, an
> earlier capture's own `register:`). A host probe's `register.<task>.stdout` is **not**
> reachable from a capture; the retired `state_changes:` block folded the per-host register
> map after the barrier and could read one. For a per-host fact use `soulprint.hosts`, or
> re-derive it keeper-side. Forwarding a destiny `output:` (read through `register:` on the
> applier task) into a capture's `value:` is still out of scope — the same planned
> output-projection slice described in
> ["How the caller reads"](#how-the-caller-reads---register-on-the-applier-task). The DSL
> core `register.<applier>.changed`/`.failed`/`.timed_out` (unit `OR`) does materialize and a
> capture could read it; the applied `output:` fields themselves do not.

## See also

- [`docs/input.md`](../input.md) - general format standard (validation keys are the same).
- [`docs/destiny/input.md`](input.md) - destiny specificity of the input contract (symmetric document).
- [manifest.md](manifest.md) - where `output:` lives in `destiny.yml`.
- [tasks.md §9](tasks.md#9-strength-and-control-of-execution) — task-level `output:`: refused (`output_unsupported`) until this slice lands.
- [scenario/orchestration.md §2.1.1](../scenario/orchestration.md) - `register:` on the applier task: implemented aggregate `.changed`/`.failed`/`.timed_out` vs planned output projection.
- [ADR-009](../adr/0009-scenario-dsl.md) - destiny isolation: `output:` (giving your own) does not break it.
