# core.noop

No-op step: does nothing and always returns success without changing state
(`changed = false`). **Soul-side**, statically built into the `soul` binary.
Implementation - [`soul/internal/coremod/noop/noop.go`](../../../../soul/internal/coremod/noop/noop.go).

This is a verb module: the only state is `run` (without declarative semantics
"lead to state"). `params:` do not have a schema - the module exists as
is a syntactic anchor, not as an operation on a resource.

## Purpose

- **barrier anchor.** Task `core.noop.run` accessing `register.*`
several previous tasks, gives the point at which the framework waits for them
completion. The barrier itself is provided not by the module, but by the dependency graph (`require:`
/ register-links) - `core.noop.run` is just an empty body of such a task.
- **placeholder.** An empty step in the destiny/scenario framework before the real one appears
logic, or the `output:`-projection carrier: `output:` reads `register.*`
the previous tasks, the step does not perform its own work.

## Read-only: doesn't change anything

`changed = false` **always**, constructive and non-configurable - no-op does not change
host status. Use case - read-probe modules ([`core.http`](../http/README.md),
[`core.exec`](../exec/README.md)): module does not declare drift, interpretation
specifies scenario. Idempotency is by nature (empty operation).

## States

| State (verb) | Destination | `changed` |
|---|---|---|
| `run` | No-op: does nothing, success without change. | `false` always. |

## run — params

No schematic: `params:` missing or `{}`. Any keys are accepted and
are ignored - the anchor task has no inputs.

## Capabilities / side-effects

- **Does not execute or write anything.** In the manifest
  ([`noop.yaml`](../../../../shared/coremanifest/noop.yaml)) `required_capabilities`
empty: there is no `exec_subprocess`, `fs_write_root`, or `network_outbound`.
- **`changed = false` constructive** (see "Read-only: doesn't change anything").
- **Errand-safe** ([ADR-033](../../../adr/0033-errand.md)): no-op is safe to
ad-hoc invocation via Errand pull loop (the module implements `ErrandReadSafe`).

## Output / register

`{ changed: false }`. The step does not produce its own output; useful data
are collected at the task level through the `output:` projection of `register.*` previous
tasks (see "placeholder").

## Examples

### Barrier via register dependencies

Three parallel probe tasks, then `core.noop.run` collects their results -
calling `register.ping`/`register.repl`/`register.mem` creates implicit
barrier (the framework waits for all three to complete before starting the anchor):

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

### Placeholder-anchor

Barrier that waits for a set of previous tasks through `require:` and does nothing
does it himself:

```yaml
- name: barrier
  module: core.noop.run
  require:
    - Install package
    - Render config
```
