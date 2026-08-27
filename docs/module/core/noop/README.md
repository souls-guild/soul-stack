# core.noop

No-op step: does nothing and always returns success without changing state
(`changed = false`). **Soul-side**, statically built into the `soul` binary.
Implementation - [`soul/internal/coremod/noop/noop.go`](../../../../soul/internal/coremod/noop/noop.go).

This is a verb module: the only state is `run` (without declarative semantics
"lead to state"). `params:` carries no schema and therefore accepts nothing but
`{}` - the module exists as a syntactic anchor, not as an operation on a
resource.

## Purpose

- **barrier anchor.** Task `core.noop.run` accessing `register.*`
several previous tasks, gives the point at which the framework waits for them
completion. The barrier itself is provided not by the module, but by the dependency graph (`require:`
/ register-links) - `core.noop.run` is just an empty body of such a task.
- **placeholder.** An empty step in the destiny/scenario framework before the real
logic appears.

> ★ This page used to name a third purpose - the `output:`-projection carrier, a
> `core.noop.run` step whose `output:` reads `register.*` of previous tasks. That
> projection was never built and a task-level `output:` is now **refused** on every
> task kind (`output_unsupported`, NIM-334), so a noop step written that way fails
> to load. To carry a value forward, `register:` the producing task and read
> `register.<name>.<field>` where you need it - see
> [destiny/tasks.md §9](../../../destiny/tasks.md).

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

No schema: write `params: {}` (the key is required on a module task). Any other
key is **refused** at load with `unknown_param` - the check compares each param
against the module's declared input schema, and this module declares none. The
anchor task has no inputs; to name registers on it, use `vars:` (see the barrier
example below).

## Capabilities / side-effects

- **Does not execute or write anything.** In the manifest
  ([`noop` module](../../../../shared/coremanifest/mod_noop.go)) `required_capabilities`
empty: there is no `exec_subprocess`, `fs_write_root`, or `network_outbound`.
- **`changed = false` constructive** (see "Read-only: doesn't change anything").
- **Errand-safe** ([ADR-033](../../../adr/0033-errand.md)): no-op is safe to
ad-hoc invocation via Errand pull loop (the module implements `ErrandReadSafe`).

## Output / register

`{ changed: false }`. The step does not produce its own output, and it cannot
collect anyone else's: the task-level `output:` projection it was documented as
carrying is unbuilt and the key is refused (`output_unsupported`, see "placeholder").
A noop task's value is its place in the dependency graph, not a payload.

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
  vars:
    ping:              "${ register.ping.stdout }"
    replication_state: "${ register.repl.stdout }"
    used_memory:       "${ register.mem.stdout }"
```

Nothing reads those three values - `core.noop` has no inputs and the anchor
applies no payload. The **references** are the point: `vars:` is a
passage-defining source field, so naming three registers there puts the anchor in
a strictly later Passage than all three probes, which is the barrier. The three
names are illustrative - each must be emitted by a `register:` on an earlier task,
or the reference is `unknown_register_reference`.

They go in `vars:` rather than `params:` because a param key is checked against
the module's declared input schema, and `core.noop.run` declares none - three keys
under `params:` would be three `unknown_param` errors and the task would not load.
`vars:` is not schema-checked. `params: {}` stays because a module task requires
the key.

The example used to write them into a task-level `output:`; that key is refused
now (`output_unsupported`) and never carried a value in the first place.

### Placeholder-anchor

Barrier that waits for a set of previous tasks through `require:` and does nothing
does it himself:

```yaml
- name: barrier
  module: core.noop.run
  params: {}
  require:
    - Install package
    - Render config
```

(`require:` names **registers**, not task names - an entry that matches no
`register:` is `unknown_register_reference`. The names above are illustrative.)
