# Scenario - index

Documentation for the scenario - the orchestration layer of the Soul Stack ("how to perform one operation on an entire cluster": `create`, `add_user`, `restart`, `add_replica`, …).

Scenario - unit of operation on [Incarnation](../architecture.md). Folder `scenario/<name>/` in the service git repo, entry point `main.yml`. Version - git ref service-repo ([ADR-007](../adr/0007-versioning-git-ref.md)).

## Where to start

| Document | What about |
|---|---|
| [concept.md](concept.md) | What is a scenario in the new model, the boundary with destiny (a recommendation, not a wall), declared role vs actual role, role-agnostic essence. |
| [orchestration.md](orchestration.md) | **Normative specification** orchestration layer: `on:`/`where:`-targeting, probe-idiom, two-level resource resolution, script tests, barrier/state-commit invariant. DSL task core - delegated to [destiny/tasks.md](../destiny/tasks.md). |
| [ADR-043 §7/§8](../adr/0043-voyage.md) | State-commit policy for a batch scenario run: **per-incarnation state-commit** (batch = N incarnations by Legs, B1). Successor to the removed Tide per-Surge state-commit. | Semantics of database commit for Voyage `kind=scenario`. |

## Related Documents

- [`docs/destiny/tasks.md`](../destiny/tasks.md) - **full specification of the DSL task core** (`module`, `include`, `block`, `parallel`, `loop`, `register`, `onchanges`/`onfail`/`require`, `retry`, `timeout`, `changed_when`/`failed_when`, template context). Scenario inherits this core entirely; [orchestration.md](orchestration.md) describes only the delta scenario on top of it.
- [`docs/architecture.md`](../architecture.md):
  - [ADR-008](../adr/0008-coven-stable-tags.md) - Coven as stable boolean tags (not role).
  - [ADR-009](../adr/0009-scenario-dsl.md) - scenario receives the full DSL of destiny tasks; border with destiny - recommendation.
  - [Service - structure and manifest](../architecture.md) - layout of the service repo where `scenario/` lives.
  - [Targeting and host communication](../architecture.md) - `on:`/`where:`, resolver contract, probe as a volatile role mechanism.
  - [Incarnation - runtime service instance](../architecture.md) - what the scenario, `state`/`status`/`error_locked` is working on.
- [`docs/destiny/concept.md`](../destiny/concept.md) - what is destiny, how does it differ from scenario.
- [`docs/destiny/testing.md`](../destiny/testing.md) - differentiation between scenario tests, destiny-molecule and service-smoke.
- [`docs/input.md`](../input.md) - **general** format standard for `input:` (applies to destiny, scenario and module manifest).
- [`docs/templating.md`](../templating.md) — template engine spec (ADR-010): CEL for all scenario expressions (`where:`/`when:`/`changed_when:`/`failed_when:`/`until:`, `params:`, `apply: input:`, `on:`-literals), marker `${ … }`, border with Go text/template, footguns `soulprint.where(...)` vs `soulprint.hosts.where(...)`.
- [`docs/soul-lint.md`](../soul-lint.md) - static checks (including backlog for scenario specifics).
- [`examples/service/redis/`](../../examples/service/redis/) - working example of a service repo with layout `scenario/` (create + day-2 `add_node`/`remove_node`/`reshard`/`restart`).
