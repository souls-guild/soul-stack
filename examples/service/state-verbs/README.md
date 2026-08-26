# `state-verbs` — the corpus exercise for `core.state.<verb>`

This service deploys nothing. It exists so that every verb of the state-capture
family ([ADR-0084](../../../docs/adr/0084-explicit-state-capture.md)) is
rendered, linted and folded by something in the corpus, not only by Go unit
tests.

Before it, every example wrote state with `core.state.set` and nothing else.
`present`, `add`, `append`, `modify`, `remove` and `unset` shipped with unit
coverage and no scenario an author could copy — and, more to the point, nothing
that would redden if a verb's meaning drifted between the L0 fold and the run.

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

Every capture carries `on: keeper`. Without it the task is dispatched to a host,
which has no `core.state` module — an ERROR at parse
(`state_capture_not_on_keeper`), because the L0 fold keys on the module address
and would otherwise predict the state for a plan the run cannot execute.
