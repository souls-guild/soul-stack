# Scenario - concept

Scenario - **orchestration layer** Soul Stack: one operation on the whole cluster (`create`, `add_user`, `update_acl`, `add_replica`, `restart`, ...). In the Soul Stack dictionary, scenario corresponds to the combination of Salt Orchestration + State, but in one language and in one place.

Folder `scenario/<name>/` in the service git repo, entry point `main.yml`. Version - git ref service-repo ([ADR-007](../adr/0007-versioning-git-ref.md)).

> **The second auto-discovery channel is `upgrade/`.** Version-to-version upgrade scripts live in a separate directory `upgrade/<slug>/` next to `scenario/` (self-describing key `from:` - source versions), are launched by the upgrade (`POST /v1/incarnations/{name}/upgrade`) and in regular day-2 script lists are not shown. Design - [ADR-0068](../adr/0068-service-upgrade-v2.md).

## What is scenario in the new model

Before [ADR-009](../adr/0009-scenario-dsl.md), the invariant "scenario only `apply: { destiny: … }`, without `module:`" was in effect. **This invariant has been removed.** Scenario receives:

- **Complete DSL core of destiny tasks** - entirely from [destiny/tasks.md](../destiny/tasks.md): `module:` (including modifying modules, not only read-only), `templates`, task-level `vars:`, `register:`, `loop:`, `block:`, `parallel:`, `onchanges:`/`onfail:`/`require:`, `changed_when:`/`failed_when:`, `retry:`, `timeout:`. This kernel is **not duplicated** in the scenario spec - there is only one source of truth.
- **The orchestration layer on top** is something that destiny doesn't have: targeting (`on:`/`where:`), cross-host coordination, `apply: { destiny: … }`, writing `incarnation.state` via `state_changes`. The delta regulatory specification is [orchestration.md](orchestration.md).

destiny **remains** an independent entity (see border below): reusable, independently-versionable (git ref, [ADR-007](../adr/0007-versioning-git-ref.md)), isolated, molecule-testable brick of "how to bring one host into state X".

## Destiny / scenario boundary - recommendation, not a wall

Boundary used to be an isolation invariant. Now this is a **recommendation**: "what is reused or critical, put it in destiny." The criterion for inclusion in destiny is three "yes":

1. **Reuse.** More than one script (or more than one service) needs this logic.
2. **Molecular-idempotency is needed.** The logic should be covered with a separate destiny-molecule test with a guarantee of re-running without changes ([destiny/testing.md](../destiny/testing.md)).
3. **isolable.** The logic can be described as "bring one host to state X", without knowing about the Keeper database, cluster topology and other Souls.

All three "yes" → take it to destiny, call via `apply: { destiny: … }`. Otherwise, inline implementation of `module:` steps directly in the scenario is acceptable.

> **Isolation and `output:` destiny.** destiny publishes its own result through the declared top-level `output:` (symmetrically `input:`, read by the caller - `register:` on the applier task). This **doesn't** violate isolation: destiny gives away its own, does not read someone else's. More details - [destiny/output.md](../destiny/output.md), [ADR-009](../adr/0009-scenario-dsl.md).

> **Risk and its mitigation.** Inline `module:` mutations in scenario do not go through independent git versioning of destiny ([ADR-007](../adr/0007-versioning-git-ref.md)) - a point edit in `scenario/<name>/main.yml` bypasses the review and tagging that a separate destiny repo would receive. Mitigation: recommendation above + backlog lint-warn in [soul-lint.md](../soul-lint.md) ("inline mutation in scenario without removal to destiny"). This is an erosion of the ADR-007 discipline, deliberately adopted for the sake of simplicity of typical operations - see [ADR-009](../adr/0009-scenario-dsl.md).

| | Destiny | Scenario |
|---|---|---|
| **Level** | one host | one cluster (one incarnation) |
| **Knows about other hosts?** | no | yes (via `on:`/`where:` and `soulprint.where`) |
| **Writes state to the database?** | no | yes (`state_changes`) |
| **Access `essence.*` in templates** | no (`input:` only) | yes (merged essence after pipeline) |
| **Task DSL** | [destiny/tasks.md](../destiny/tasks.md) | same core + orchestration delta ([orchestration.md](orchestration.md)) |
| **Version** | git ref destiny-repo | git ref service-repo |
| **Testing** | molecule on an ephemeral stand (one host) | your own mechanism: multi-host stand + topology assertions/`incarnation.state` ([orchestration.md](orchestration.md), [destiny/testing.md](../destiny/testing.md)) |

## declared-role vs actual-role

The role of the host (master / replica) in the Soul Stack is **not Coven** ([ADR-008](../adr/0008-coven-stable-tags.md)). There are two conceptually different roles:

- **declared-role** - declared by the operator, lives **only** in `incarnation.spec.hosts[].role`. Used in the bootstrap operation `create`, when Redis is not yet running and there is nothing to probe: the first master / first replicas are taken from `spec`. Also serves for topology and auditing. essence **does not consume it** (see below). In the scenario declared-run topology is available by the accessor `soulprint.hosts` (list of hosts with stable facts, scenario-only; specification - [orchestration.md §4.1](orchestration.md#41-soulprinthosts---list-of-run-hosts-scenario-only-accessor)); destiny receives it only through an explicit `apply: input:`.
- **actual-role** - the actual role at the moment (who is now the real master of `redis-cli role`). **Volatile**, changes during failover. It is not stored anywhere stably: it is obtained by a **live probe step** immediately before use (`module: core.exec.run` + `register:`), the targeting of the next step follows `where:` from this `register:` ([orchestration.md](orchestration.md)). After failover - just a new probe, no cache or freshness mechanism.

> Volatile (role) **does not live in Soulprint**. Soulprint - only stable and slowly changing host facts. There are no volatile soulprint facts, no role collector, no freshness mechanism. This is a consequence of [ADR-008](../adr/0008-coven-stable-tags.md) and was recorded there.

## Essence role-agnostic

Essence layer by role **removed**: the pipeline hierarchy no longer **contains** the `role/<Y>.yaml` stage. The build order is `default → os → coven → incarnation.spec` (see [architecture.md → "Essence: build pipeline"](../architecture.md)). essence does not consume the declared role and does not layer by role at all.

Parameters that previously depended on the role (what was in `role/master.yaml` / `role/replica.yaml`) move to **destiny** and are passed through `input:` as a result of the probe role. That is, the scenario first probes the actual role, then calls `apply: { destiny: …, input: { … } }` with different values ​​for the master and replica hosts - rather than putting them through the essence layer.

> Transferring specific values ​​of `role/*.yaml` to destiny-`input:` is a separate implementation task, see the mention in [orchestration.md](orchestration.md). Here only the model is fixed: essence role-agnostic, the role-dependency moves to destiny using probe.

## See also

- [orchestration.md](orchestration.md) - regulatory specification of the orchestration delta scenario.
- [destiny/tasks.md](../destiny/tasks.md) - DSL task core (inherited entirely by the scenario).
- [destiny/concept.md](../destiny/concept.md) - what is destiny, how does it differ from scenario.
- [architecture.md → ADR-008](../adr/0008-coven-stable-tags.md), [ADR-009](../adr/0009-scenario-dsl.md) - fixing solutions.
- [architecture.md → "Targeting and host communication"](../architecture.md) - `on:`/`where:`, resolver contract.
- [destiny/testing.md](../destiny/testing.md) - differentiation between scenario tests / destiny-molecule / service-smoke.
