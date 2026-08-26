# `state-verbs` — the corpus exercise for `core.state.<verb>`

This service deploys nothing. It exists so that every verb of the state-capture
family ([ADR-0084](../../../docs/adr/0084-explicit-state-capture.md)) is
rendered, linted and folded by something in the corpus, not only by Go unit
tests.

Before it, every example wrote state with `core.state.set` and nothing else.
`present`, `add`, `append`, `modify`, `remove` and `unset` shipped with unit
coverage and no scenario an author could copy — and, more to the point, nothing
that would redden if a verb's meaning drifted between the L0 fold and the run.

## `scenario/create` — every verb, once

`scenario/create/main.yml` runs each verb once, in an order chosen so that the
final state is exactly predictable. Two cases:

| Case | What it pins |
|---|---|
| `tests/all-verbs` | The fold from an **empty** state: what each verb writes, and that `unset` drops the key rather than blanking it (`state_absent`). |
| `tests/idempotent-rerun` | The same plan over the state the first run left: `present` keeps the stored value, `add`/`modify`/`remove` converge, and `append` deliberately does **not**. |

Two things the file is written to show, because both are error-not-fallback:

- **Identity belongs to the collection kind.** `key:` names an element of a map
  field, `match:` an element of a list field, and the engine picks by the
  field's kind in `state_schema` — so `members` is `type: object` and `nodes` is
  `type: array` on purpose.
- **An omitted `match:` on `modify` / `remove` matches nothing.** The step
  becomes a no-op, so a forgotten line cannot demolish a collection. "Every
  element" is written out as `match: "true"`, which takes the `state_wide_match`
  warning.

## `scenario/per-host-capture` — the per-host route into state

`scenario/create/` writes host-invariant values. A value that exists only **per
host** — a probed node id, a generated port, a cluster member address — had no
route into `incarnation.state` at all until NIM-711: the capture is a keeper
task, so it binds no soulprint, targets no host and reads only the keeper
register bucket ([ADR-056](../../../docs/adr/0056-staged-render-passage.md) slice 2).
`register.node_id` there is not "some host's value" — it is nothing.

`scenario/per-host-capture/main.yml` closes it with **one** accessor:
`register.hosts.<name>` — the map {SID → payload} for one register across the
hosts that produced it, so a single capture writes the whole per-host set
([ADR-0084 amendment](../../../docs/adr/0084-explicit-state-capture.md),
[orchestration.md §7](../../../docs/scenario/orchestration.md)).

| Case | What it pins |
|---|---|
| `tests/two-hosts` | The **shape** the capture writes: one field, one entry per host of the roster, keyed by SID. |

Two things this file is written to show:

- **It is an accessor, not a task key.** The dependency edge it declares is on
  `node_id`, not on `hosts` — which is what puts the capture in a **later
  Passage** than the probe, so the map is populated by the time it renders. A
  same-Passage capture would read an empty map.
- **Outside `on: keeper` it is a compile error, not an empty map.** A host task's
  `register.<name>` is deliberately its own value ([ADR-0083](../../../docs/adr/0083-declared-secret-state-fields.md)
  §5), and an empty map would let `.size() == 0` and an empty `foreach` read as
  facts. `register: hosts` on a task is refused at parse
  (`register_name_reserved`): such a register is unreadable from either side — the
  accessor wins over it on the keeper, and on a host it is this same compile error.

The case pins the shape, not the per-host distinctness of the values: the L0
harness applies one `mocks.register` payload to every host by SID, so both
entries carry the same value and only the KEYS differ. Distinct per-host
payloads are pinned in Go instead — `keeper/internal/render/register_hosts_test.go`.

## Both scenarios

Every capture carries `on: keeper`. Without it the task is dispatched to a host,
which has no `core.state` module — an ERROR at parse
(`state_capture_not_on_keeper`), because the L0 fold keys on the module address
and would otherwise predict the state for a plan the run cannot execute.
