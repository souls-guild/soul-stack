# Scenario - orchestration layer specification

This document is a **normative specification of the delta scenario** on top of the destiny task DSL core. The source of truth when implementing a scenario orchestrator.

**DSL task core is NOT duplicated here.** All task blocks (`module:`, `include:`, `block:`, `params:`, task-level `vars:`, `when:`, `parallel:`, `loop:`, `register:`, `output:`, `no_log:`, `onchanges:`, `onfail:`, `require:`, `changed_when:`, `failed_when:`, `retry:`, `timeout:`), their semantics, barriers, requisites and template context - **are fully described in [destiny/tasks.md](../destiny/tasks.md)** and are inherited by the scenario as is. This document covers **only what destiny doesn't**: targeting, cross-host coordination, `apply: { destiny: … }`, `incarnation.state` entry, resource resolution, script tests.

Any key not described here and not described in [destiny/tasks.md](../destiny/tasks.md) is a scenario validation error.

Related documents: [concept.md](concept.md), [destiny/tasks.md](../destiny/tasks.md), [architecture.md → "Targeting and host communication"](../architecture.md), [architecture.md → "Service - structure and manifest"](../architecture.md), [ADR-009](../adr/0009-scenario-dsl.md).

## 1. File format and layout

```
scenario/<name>/
├── main.yml                  # entry point: name, description, input, state_changes, tasks (inline)
├── <sub>.yml                 # include neighbors (same task structure)
├── templates/                # OPTS: templates used by the steps in this script
├── vars.yml                  # OPTS: scenario locales (like destiny vars.yml)
└── tests/                    # OPT.: tests of this script
    └── <case>/
        └── case.yml
```

`main.yml` contains **inline** `input:`, `state_changes` and `tasks:`. Neighboring `*.yml` are connected via `include:` ([destiny/tasks.md §4](../destiny/tasks.md#4-basic-blocks)). Folder layout (`templates/`, `vars.yml`, `tests/`) - **symmetrical to destiny** intentionally, without a separate dictionary; two-level resolve - see §6.

Structure `main.yml` (blocks `name`, `description`, `input`, `state_changes`, `tasks`) - in [architecture.md → "`scenario/<name>/main.yml`"](../architecture.md). Block `input:` - according to the general standard [docs/input.md](../input.md).

## 2. Delta scenario relative to the DSL core

On top of the tasks from [destiny/tasks.md](../destiny/tasks.md), the scenario task has additional keys:

| Block | Type | Apply to | Obligation |
|---|---|---|---|
| `on:` | `keeper` OR list of coven-id OR omitted | all types of tasks | optional (omitted = entire incarnation) |
| `where:` | string (predicate-expr) | all types of tasks | optional |
| `apply:` | map (`destiny:` + `input:`) | task-applier | alternative `module:` |
| `assert:` | map (`that:` + `message:`) | assert task | alternative `module:`/`apply:`/`include:`/`block:` (see §2.3) |
| `serial:` | int (1..M) OR string `"<N>%"` | module/apply/`block:`-task | optional (omitted = entire target width) *Granularity - per-Passage min-width (in N=1 = per-RUN), see subsection §2.2.1 below* |
| `run_once:` | bool, default `false` | module/apply/`block:`-task | optional |

In addition to the per-task keys, scenario has **top-level** blocks: `compute:` — calculated run vars (§2.4); `validate:` - declarative input invariants (§2.5); `extends:` - inheritance of the general service-level contract of sections from `covenant.yml` (§6.1).

Everything else in the scenario task is exactly the same as in the destiny task, with the same semantics ([destiny/tasks.md §3–§10](../destiny/tasks.md#3-complete-list-of-task-blocks)). Discrepancies with destiny are explicitly listed in §6 (resource resolution) and §10 (template context).

### 2.1. `apply:` - destiny challenge

```yaml
- name: Install redis on all cluster hosts
  apply:
    destiny: redis                 # name destiny from service.yml → destiny:
    input:
      version:  "${ essence.redis_version }"
      password: "${ input.redis_password }"
```

`apply:` is the place where the scenario delegates work to an isolated destiny. `destiny:` - name from the dependency registry `service.yml` (resolve ref - [ADR-007](../adr/0007-versioning-git-ref.md)). `input:` - values ​​included in the `input:` destiny contract; destiny validates them with its contract ([destiny/input.md](../destiny/input.md)). The task with `apply:` is an **applier-task**; `apply:` and `module:` are mutually exclusive in the same task.

#### 2.1.1. `register:` on the applier task - reading the result of destiny

An Applier task can carry `register: <name>` ([destiny/tasks.md §8](../destiny/tasks.md#8-requisites---salt-style-dependencies)) inherited from the DSL core. `register.<name>` has two parts with different implementation status:

- **DSL core `.changed` / `.failed` / `.timed_out` - implemented.** This is **unit `OR`** for all child destiny tasks of the applier: `changed = OR(child.changed)`, similar to `failed` / `timed_out` (`skipped` is always `false`). External `onchanges: [<name>]` / `onfail: [<name>]` / `when: register.<name>.changed` resolves for this unit (previously it crashed with the "unknown register" error). If all child destiny tasks are filtered (`where:` / `include`-`when:`), the aggregate is reduced to `changed/failed/timed_out = false` (no-op applier).
- **`.<output field>` according to the declared top-level `output:`-contract destiny - PLANNED.** Forwarding application fields destiny ([destiny/output.md](../destiny/output.md)) to `register.<name>.<output field>` is **not yet implemented** (a separate future slice, see note in [destiny/output.md](../destiny/output.md#how-the-caller-reads---register-on-the-applier-task)). In the current volume, `register.<name>` carries only the DSL core.

Working example (unit `.changed` / `.failed`):

```yaml
- name: Apply Redis config on all cluster hosts
  apply:
    destiny: redis-config
    input: { ... }
  register: cfg

- name: Restart Redis only where config actually changed
  when: register.cfg.changed          # on: omitted = all member hosts
  module: core.service.restarted
  params: { name: redis-server }
```

Here `register.cfg.changed` is true if at least one child task destiny `redis-config` has reported `changed` (aggregate `OR`); The restart is skipped if the config has already been converged on all hosts. The same unit is available through `onchanges: [cfg]` / `onfail: [cfg]`.

> **Illustration of the future output projection (does NOT work in MVP).** The example below is based on forwarding the `output:` application field applier-register - this is the **planned** slice (see above), now `register.reload.drifted_sids` does not resolve. Do not copy into the working script.
>
> ```yaml
> - name: Reload Redis cluster config on all hosts
>   apply:
>     destiny: redis-reload
>     input: { ... }
>   register: reload
>
> - name: Restart only nodes that reported config drift
>   where: soulprint.self.sid in register.reload.drifted_sids # on: omitted = all members; output projection, not yet implemented
>   module: core.service.restarted
>   params: { name: redis-server }
> ```
>
> When the output projection is implemented, destiny `redis-reload` will declare the `drifted_sids: { type: array, items: { type: string, format: fqdn } }` field in its top-level `output:`, and scenario will read it through `register.reload.drifted_sids`. The format of the `output:` block in `destiny.yml` and the filling rules through the task-level `output:` are [destiny/output.md](../destiny/output.md).

> When to write `apply:`, and when to write inline `module:` - see the boundary recommendation in [concept.md](concept.md) ([ADR-009](../adr/0009-scenario-dsl.md)). Removing the old "scenario only `apply:`" invariant means: `module:` (including modifying modules) in scenario is now legal.

### 2.3. `assert:` — render-time precondition

```yaml
- name: cluster topology matches
  when: "input.redis_type == 'cluster'"
  assert:
    that:
      - "size(soulprint.hosts) == (has(input.shards) ? int(input.shards) : 0) * (1 + int(input.replicas_per_shard))"
    message: "topology mismatch: hosts != shards*(1+replicas_per_shard)"
```

`assert:` - **checking the run invariant at the model stage** (Ansible module form `assert`, [ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-23). This is the **fifth discriminator** of the task: `assert:` is mutually exclusive with `module:`/`apply:`/`include:`/`block:` (assert - check, not executable work).

- **`that:`** is a non-empty list of CEL-bool predicates (each entire line = CEL, without the wrapper `${ … }`, like `where:`). All must be `true`.
- **`message:`** - optional human-readable message in the error text upon failure (if omitted, default message).

**Semantics:**

- **Computed by Keeper-side in the render phase**, in the full scenario-CEL context - **including `soulprint.hosts`** (`AllowHosts=true`, like `where:`): assert sees the run roster (topology), unlike Soul-side flow-control, to which `soulprint.hosts` is not available.
- **Run-level** (once per run, not per-host): assert tests a run topology invariant, not a per-host predicate.
- **The first `false` breaks render** with a clear error (`message` + text of the failed predicate). **Not a single task is left on Soul** - the run does not start (fails at the model stage). All `true` → the task "disappears" from the plan: assert **does not emit RenderedTask** (this is a check, not a task), the indexes of subsequent tasks do NOT reserve a position for it.
- **Gates `when:`** like any task: statically-false `when:` (placeholder-skip, e.g. `when: input.redis_type == 'cluster'` on a standalone run) → assert is NOT calculated.
- **Not a wire entity.** assert does not change proto Keeper↔Soul and Soul contract (render construct like `block:`).

**Motive.** Replacing the `core.cmd.shell`-guard hack (`test "${ <cel> }" = "true" || { echo ...; exit 1; }` is an arbitrary shell on Soul for the sake of control-flow) with a declarative keeper-side check: less attack-surface (no shell execution for the sake of checking), the failure is transferred from the host to the model (earlier and clearer).

### 2.2. Cross-host step execution model

DSL core ([destiny/tasks.md](../destiny/tasks.md)) describes the execution of the step
**on the same host**. Scenario adds a "target hosts" axis. Basic model,
on top of which `serial:` and `run_once:` work:

- Step with a target of M hosts (after resolving `on:`+`where:`, see §3–§4) by
by default applies **to all M hosts in the same wave** (cross-host
fan-out), then a cross-host join before the next step. Join subordinate
barrier/state-commit invariant (§7): `state_changes` commit strictly
after completing the steps on all hosts of the run, not host by host.
- Host order in any phased processing (waves `serial:`, host selection
for `run_once:`) **deterministic**: lexicographically by `SID`.
Non-deterministic order is prohibited - it breaks reproducibility
destructive operations and topology assertions in script tests (§6).

This axis is **orthogonal** to `parallel:` and `loop:` DSL cores - do not confuse:

| Mechanism | Axis | Source | Semantics |
|---|---|---|---|
| `parallel:` (tasks.md §6) | threads on ONE host | — | fire-and-forget, flow does not wait |
| `loop:` (tasks.md §7) | data collection | `input.*` / `vars.*` | repeat step by element |
| `serial:` | Target HOSTS | resolve `on:`/`where:` | waves across ≤N hosts, waves sequentially |
| `run_once:` | Target HOSTS | resolve `on:`/`where:` | exactly one (first by SID) target host |

Combinable: step with `loop:` under `serial:` runs the entire loop on each
wave host; `parallel:` inside the host works regardless of waves.

> **Discovery phase of `loop:` (normative).** `loop:` is expanded in
> **render phase** - one task gives **N `RenderedTask`** for elements `items:`,
> with end-to-end indexes (symmetrically `apply: destiny`). This is **not** config-splice
> like `include:` (it is pasted into the flat task list BEFORE render): `items:` —
> CEL/template expression known only after the CEL phase. The reveal comes AFTER
> resolve target (`on:`/`where:`/`run_once:`), inside each targeted host;
> `serial:` - the width is inherited by all iterations. **Pilot:** `items:` and
> `loop.when:` host-invariant (resolve once per run, without `soulprint`);
> host-variable `when:`/`items:` via `soulprint` specific host (per-host
> loop filtering) deferred. See [destiny/tasks.md §7](../destiny/tasks.md).

#### 2.2.1. `serial:` - wave (rolling) version

- **Value.** `N` (integer 1..M) or `"<N>%"` (percentage of target width,
rounding up, minimum 1). Target hosts sorted by `SID`,
beat into successive waves of size ≤N: inside the wave - in parallel,
waves - strictly sequential.
- **Apply to** module-, applier- and `block:`-task. On `block:`-task
the wave rolls **the entire block** (all its internal steps) one at a time
recruiting hosts before moving on to the next wave is an idiom
"wave = {change, check health}" (see §5).
- **Falling in a wave (fail-stop, invariant).** First host, finally-failed
after the inherited `retry:` is exhausted, stops rolling:
subsequent waves will not start. Direct consequence of absolute fail-closed
and §7. There is no tolerance threshold for partial failure (§8, open Q).
- **Interaction with barrier (§7) is an invariant, not an option.** `serial:`
**does not split** state-commit. `state_changes` commit **once after
completion of ALL waves across all hosts**, never wave by wave. Partial
commit after a successful wave when the next one falls is prohibited - otherwise
`incarnation.state` will diverge from the fact (see §7).

> **Granularity `serial:` is per-Passage min-width.** The grammar allows `serial:` to be written on each task independently, and the wave width is **one per Passage**: the minimum positive `serial:`-width among tasks **of this Passage** (the narrowest window). Tasks without `serial:` do not narrow the window; if `serial:` is not carried out by any Passage task, it goes in one wave (the entire width of the target). The reason for aggregation is the dispatch model "one `ApplyRequest` per host with all its tasks en masse" (ADR-012(d)) on top of the composite PK `(apply_id, sid, passage)` table `apply_runs`: tasks of one Passage are sent to the host in one message, so it is impossible to send different Passage tasks in different waves. Choosing a minimum (not a maximum) - fail-closed: the window is narrowed to the narrowest intention of the author, the blast radius when falling is minimal.
>
> **Per-Passage, NOT per-RUN ([ADR-056](../adr/0056-staged-render-passage.md) §serial, S-2D1).** Before stratification (one pass, N=1), the width was calculated over the entire run - this is a **special case** of per-Passage, **bit-for-bit matching** at N=1. With staged-render (probe→where gives >1 Passage), the width is derived from the tasks of **each Passage separately**: probe-Passage without `serial:` travels **in one wave**, even when the subsequent Passage carries `serial:1` - the narrow window of one Passage **does not leak** into another (otherwise the probe-pass would silently travel along one host - destructive throttle). A consequence for the author: a task without its own `serial:` will travel in narrow waves only if there is at least one narrow task in **its Passage** (not anywhere in the run). 2D `serial`×`passage` - implemented; the previous restricted pilot (serial + staged was rejected) was filmed. The fail-stop and barrier/state-commit invariants (§7) are preserved: the waves are still consistent within each Passage, the state is committed once after the **last** Passage.

#### 2.2.2. `run_once:` - execution on one target host

- **Value.** `bool`, default `false`. With `true` the step is executed smoothly
on **one** host - the first one on `SID` from resolution `on:`+`where:`.
- **>1 host in the target is the norm** (typical case: failover, when "current
master" by probe may turn out to be >1). The step is on one deterministic one.
- **0 hosts in the target.** `run_once:` **does not introduce its own policy**
empty/unknown target - the general semantics of §5 apply (decides
`failed_when:`/module, then standard step fall processing). Separate
there is no fail-closed invariant for `run_once:`.
- `serial:` and `run_once:` are mutually exclusive in the same task (error
validation) are different width strategies.

#### 2.2.3. Batch run (Voyage `kind=scenario`, [ADR-043](../adr/0043-voyage.md))

`serial:` / `run_once:` / `on:` / `where:` capture split and target
**at the scenario declaration level**. For mass rollouts for 100k+ souls
the operator can split the run into successive batches (**Leg**) +
narrow the target by labels/coven/CEL via **invocation parameters**. This
second level of orchestration **above** scenario-runner: ADR-009 commits
declaration-level, **Voyage** captures the invocation-level.

Example:

```bash
soulctl incarnation run prod-cache converge \
  --wave-size 10000 \
  --wave-on-failure abort \
  --where 'soulprint.self.os.family == "debian"' \
  --target-coven prod-eu \
  --concurrency 100
```

→ Voyage `kind=scenario` breaks down the run into Legs and by incarnations, sequential
execution, abort on the first failed Leg. Each incarnation is complete
scenario-run with its barrier + state-commit (pairing §7).

**Invariants with respect to §2.2:**

- **AND-merge target.** `target.coven[]` and `target.where` invocation
**narrow** scenario `on:`/`where:` (intersection), do not expand. Operator
cannot go beyond the declared scenario target through invocation.
- **REPLACE concurrency.** `concurrency` invocation overrides scenario
`serial:` (runtime-knob, not declared invariant).
- **`run_once:` + batch - conflict.** Scenario with `run_once:` step
incompatible with wave partitioning (`run_once` = exactly one target host,
there is nothing to break). Validation rejects batch request (`422
  validation-failed`).
- **Per-incarnation state-commit.** §7 barrier works per-incarnation: state
commits after each successfully completed incarnation of Leg, not after the entire Voyage.

Full contract (PG-table `voyages` + `voyage_targets`, back-link `voyage_targets.apply_id → apply_runs(apply_id)`,
failure-policy, RBAC `incarnation.run`, statuses `pending`/`running`/`succeeded`/`failed`/
`partial_failed`/`cancelled`; rancid running-Voyage returns Reaper-`reclaim_voyages`
to `pending` for re-claim) —
[ADR-043](../adr/0043-voyage.md),
HTTP facade - `POST /v1/voyages` (`kind=scenario`).
Per-incarnation state-commit (ADR-043 §7/§8) commits the state after a successful run.

#### 2.2.4. `wait:`/poll-until is NOT entered - `retry:`+`until:` is reused

Former key `wait: { condition:, timeout: }` from old examples **removed**
and **is not replaced by a new key**. Semantics "wait until the expression is over"
fresh probe will become true, before the time budget" is expressed
inherited `retry:`+`until:`
([destiny/tasks.md §9](../destiny/tasks.md#9-strength-and-control-of-execution))
at the probe step: `count × delay` sets the time budget, `until:` — the condition
health. This is a conscious decision (not to produce a primitive that duplicates
`retry.until:`); the rolling-restart idiom with health-gate is in §5. Any
occurrence of `wait:` in scenario is a validation error; when rewriting old ones
examples `wait: { condition: C, timeout: T }` → probe step with
`retry: { count:, delay:, until: C' }`, where `C'` was rewritten from a remote
`soulprint.self.*` on `register.self.*` fresh probe
([ADR-008](../adr/0008-coven-stable-tags.md)).

### 2.4. `compute:` - calculated vars of the run

`compute:` - **top-level** script block (next to `input:`/`state_changes:`/`tasks:`, **not** per-task key): map `<name>: <CEL expression>`, which Keeper resolves **ONCE per run** and makes available as `compute.<name>` in `apply: input:` and in `state_changes` ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-23).

```yaml
name: create
compute:
  redis_config: >-
    ${ merge(essence.redis_config,
             essence.persistence_presets[input.persistence],
             default(input.redis_settings, {})) }
state_changes:
  - set: redis_config
    value: "${ compute.redis_config }"     # ← same compute
tasks:
  - apply:
      destiny: redis
      input:
        config: "${ compute.redis_config }" # ← and here is the same compute
```

**Why.** `apply: input:` and `state_changes` are **different** CEL contexts, and neither of them **sees** the task-level `vars:` (`vars:` is the scope of one task, §10). The general expression (big `merge()` - translation of a simple input into `redis.conf`) would have to be written **twice** and synchronized by hand. `compute:` declares it **once** - drift "state ≡ live config" is removed by the mechanism itself, and not by the author's discipline.

**The resolution context is run-level, WITHOUT `soulprint`.** `compute:` is calculated in the context of `input.*` / `essence.*` / `incarnation.*` / `register.*` - **without** `soulprint.self` / `soulprint.hosts`. This is a **structural host invariance barrier**: reference to `soulprint.*` in a compute expression → CEL no-such-key. The consequence is that `compute.<name>` **is the same for all hosts**, so the same value is correctly sent to both `apply: input:` (resolved on the first target host on `SID`) and to `state_changes` (per-RUN, not per-host). Per-host values, as before, are expressed by direct per-host CEL in `params:` / template `.self`, not through `compute:`.

**Declaration order is significant.** `compute[i]` can refer to a previously declared `compute[j]` (j<i) as `${ compute.<name_j> }` (accumulating from left to right). Link forward → no-such-key.

**Isolation from destiny ([ADR-009](../adr/0009-scenario-dsl.md) V2).** `compute:` - **scenario-entity**: inside the isolated destiny-passage (`apply: { destiny: … }`) it **does not leak**. Destiny only sees the **result** - what the scenario passed through `apply: input:`. Inside destiny `compute.<name>` → no-such-key (like `register.*`/`essence.*` - §10).

**Names.** compute-var name - CEL-field-accessible identifier (letters/numbers/`_`, starts with letter or `_`); hyphen/dot are not allowed (would break `compute.<name>`). The name must not obscure the root context names (`input`/`register`/`incarnation`/`soulprint`/`essence`/`vars`/`compute`).

> **Boundary `compute:` ↔ `vars:`.** Task-level `vars:` (§10, [destiny/tasks.md §9](../destiny/tasks.md#9-strength-and-control-of-execution)) - local for one task and **visible only in its** `params:`. `compute:` - rune-level, visible in `apply: input:` **and** `state_changes`. If the value is needed in both state and destiny transfer, this is `compute:`; if only within `params:` of one task - `vars:`.

### 2.5. `validate:` - declarative input invariants

`validate:` - **top-level** script section (next to `input:`): a list of `[{that: <CEL-bool>, message: <str>}]` rules, each an **INPUT invariant**, which must be true ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-23). The first `false` rejects the run to the statement as **422 `validation-failed`** - **before commit incarnation and before applying**, with `message` rules.

```yaml
name: create
input:
  redis_type: { type: string, default: standalone, enum: [standalone, sentinel, cluster, sentinel_only] }
  replicas:        { type: integer, default: 0, min: 0 }
  sentinel_quorum: { type: integer, required: false, min: 1 }
validate:
  - that: "!has(input.sentinel_quorum) || input.redis_type != 'sentinel' || int(input.sentinel_quorum) <= 1 + int(input.replicas)"
    message: "sentinel_quorum cannot exceed the number of sentinels (1 + replicas): quorum will become unattainable"
tasks: [ ... ]
```

**Why.** Covers **cross-field** preconditions "must be X, not Y", not expressed by a single schema key (`enum`/`min`/`max` - about **one** field, the ceiling here is **another** field) and not covered by `required_when` (about **presence** of a field, not about **ratio** of values). Before `validate:`, such an invariant would have had to be fenced off with a keeper-side shell guard (`core.cmd.shell ... || exit 1`) or dropped with an incomprehensible runtime failure after the fact.

**Rule context is INPUT-ONLY.** `that` sees **only `input.*`** (same narrow cel-go-sandbox as `required_when`). Link to `essence.*`/`soulprint.*`/`register.*`/`vault()`/`now()` → CEL error undeclared reference - **structural input-only barrier by undeclaration**, not text guard. Topology/roster checks (`size(soulprint.hosts) == …`) - **not here**, but in `assert:` (§2.3): it has a full scenario context with `soulprint.hosts`.

**When - pre-flight on the request path.** Calculated in the same phase as `required_when` (`scenario.ValidateInput`): AFTER merge defaults / required / type-validation over **merged** input, BEFORE committing incarnation and entering `applying`. Covers both paths - `POST /v1/incarnations` (create) and `POST .../scenarios/{scenario}` (run). Render fail-safe (as in `assert:`) **not needed**: `validate:` is determined from `input.*`, which does not change between the request path and the start of the run (unlike the assert's roster - TOCTOU).

**Rule form.** `that` is a required non-empty CEL-bool string (entire string = CEL without wrapper, like `where:`). `message` - **mandatory** non-empty string (unlike the optional `assert.message`: assert has a task name as fallback, validate rule has no name → 422 would be anonymous). Multiple rules - first `false` wins (short circuit in declaration order).

> **Boundary `validate:` ↔ `required_when` ↔ `assert:`.** Three mechanisms, three different niches:
>
> | Mechanism | Context | What checks | Error code |
> |---|---|---|---|
> | `required_when` (input-schema key, [docs/input.md](../input.md)) | input-only | **presence** fields provided above other input | 422 `validation-failed` |
> | `validate:` (top-level section) | input-only | **cross-field invariant** INPUT (value relationship) | 422 `validation-failed` |
> | `assert:` (task discriminator, §2.3) | full (`soulprint.hosts`) | **TOPOLOGY invariant** run (roster) | 422 `assert-failed` |
>
> `validate:` ADDITIONS, does not replace: "is port required?" → `required_when`; "does quorum exceed the number of sentinels?" → `validate:`; "Is the roster suitable for an N-shard cluster?" → `assert:`.

## 3. Step target - `on:`

`on:` - **stable place** for step execution. Resolved by Postgres (stable layer: the incarnation **membership** relation `incarnation_membership` and the hosts' stable covens). Resolution point - **start of each Passage** ([ADR-056](../adr/0056-staged-render-passage.md)); the omitted `on:` target is stable within the Passage, but at the refresh boundary (`refresh_soulprint: true`) it will be re-resolved according to the updated roster - see §4.1 "Stability of the roster run" ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). Three forms:

| Form | Semantics |
|---|---|
| **omitted** | entire incarnation: all **member** hosts, resolved via the membership relation `incarnation_membership` (NOT via a coven). `incarnation.name` is not a Coven — `on: ["${ incarnation.name }"]` is a **validation error** (steering to the omitted form), see [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation) |
| `on: keeper` | keeper-side: local task on the keeper itself (cloud-create, vault-resolve, http-call) |
| `on: [coven-a, coven-b]` | intersection (AND) of the listed **stable** covens; result **always ⊆ members** (the roster is already membership-scoped) |

```yaml
# Entire incarnation (on: omitted)
- name: Apply base config everywhere
  apply: { destiny: redis-base, input: { ... } }

# keeper only
- name: Provision VMs
  on: keeper
  module: core.cloud.provisioned
  state: created
  params: { ... }

# Intersection of stable covens, ⊆ members
- name: Tune kernel on bare-metal hosts of this cluster
  on: [baremetal]                  # incarnation scope is implicit (roster is membership-scoped)
  apply: { destiny: kernel-tuning, input: { ... } }
```

**Resolver contract `on:` (invariant):**

1. **Membership, not a name-coven.** The omitted `on:` = all **member** hosts of the incarnation, resolved via the membership relation `incarnation_membership` (NOT via `= ANY(coven)`). `incarnation.name` is **not** a Coven and is not a valid `on:` value: `on: ["${ incarnation.name }"]` is a **validation error** with a message steering to the omitted form (fail-closed — a stale scenario errors out instead of resolving to an empty set). See [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation).
2. List in `on:` - **AND/intersection** of **stable** covens. Additional stable covens (e.g. `baremetal`, `dc-eu`, `prod`) narrow the set, but **cannot expand it beyond the incarnation members**.
3. **Cross-incarnation targeting is prohibited by construction.** The `on:` resolver cannot return a host that is not a member of the current incarnation, given any set of covens — the roster is membership-scoped, so a stable coven can only intersect it, never reach outside it. This is a security invariant (see [ADR-008](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)), now enforced by the membership join rather than by the name-coven.
4. Role (master / replica) **never Coven** and does not participate in `on:`. The volatile role is expressed only through `where:` (see §4).

## 4. Volatile predicate - `where:`

`where:` — **volatile string predicate** that selects per-host hosts based on the result of the previous probe step (`register:`). Resolves **by `register:` in runtime**, not by Postgres.

```yaml
# probe: who is the actual master now (see §5 - probe idiom)
- name: Detect actual redis role per host
  module: core.exec.run                                        # on: omitted = all member hosts
  register: redis_role
  changed_when: false                                          # probe state does not change
  params:
    command: "redis-cli role | head -1"

# the next step targets the register of the previous probe, per-host
- name: Restart only the current replicas
  where: register.redis_role.stdout == 'slave'                 # on: omitted = all members
  module: core.service.restarted
  params: { name: redis-server }
```

**Two-phase target resolution.** First, `on:` narrows the set by Postgres (stable), then `where:` filters the resulting set **per-host** by `register:` (runtime). The order is strict: `on:` → Postgres, `where:` → `register:`.

> **Implementation of probe→where is staged-render (Passage, [ADR-056](../adr/0056-staged-render-passage.md)).** In order for `where:` (and `apply: input:`/`params:`/`vars:` of the next task) to actually see `register:` of the previous probe, the run is executed as N ordered **Passage** (render→dispatch→barrier→register collection): the task with `register.X` is stratified in Passage **after** the probe issuing `X`, and is rendered with the register already filled. Until this feature is rolled out on the current keeper, the mechanism resolves as it is implemented (see slice map [ADR-056](../adr/0056-staged-render-passage.md)).

**`where:` is a predicate on two data classes.** Predicate `where:`
operates on (1) **register data** of the previous probe
(`register.<name>.*`/`register.self.*`) - volatile per-host
result; and/or (2) **host stable facts** (`soulprint.self.*`
- host's own stable facts: `sid`, `network.*`, `os.*`,
`covens`; symmetrical `register.self.*`). **Invariant (specified, not
weakened):** **every** `register.<name>` occurring in `where:`,
must be `register:` of the probe step that completed before this step
(otherwise a validation error). Predicate containing **none**
`register.*` (purely stable, e.g.
`soulprint.self.sid == vars.new_sid`), **probe is not required** —
stable facts are available from the Postgres layer without a runtime probe. This is not
weakening of the register-invariant (it applies exactly to
register-links), and explicit permission of the stable per-host filter
without probe - needed to target a host based on its own
stable `SID` (case `add_replica`: the new host is not yet
probe-abel). Volatile role still **only** via probe +
register ([ADR-008](../adr/0008-coven-stable-tags.md)
not affected).

> **The reference canon to `register:` is a single prefix.** In the `where:` predicate
> register is addressed in **the same form** as in
> `changed_when:`/`failed_when:`/`until:`/requisites: `register.<name>.*`
> (`register.self.*` - only your result, in
> `changed_when:`/`failed_when:`/`until:`,
> [tasks.md §10](../destiny/tasks.md#10-template-context)).
> Naked form `<name>.*` in `where:` - **validation error**. One
> register namespace for the entire DSL core
> ([ADR-009](../adr/0009-scenario-dsl.md),
> without dialects). All examples §4–§5 and the pilot `restart` are given in this form.

**`where:` can refer to multiple `register:`.** Predicate expression
`where:` has the right to use register cards from **different** previous ones
probe steps (former master case:
`where: "register.redis_role_after.stdout == 'slave' and register.redis_role.stdout == 'master'"`).
Invariant: **every** register-id occurring in the expression `where:`,
must be a `register:` probe step that **completed before** this step;
failure for any of the mentioned registers is a validation error
(generalization of the rule "`where:` without a preceding probe is an error" from one
register on N). Per-host join register cards follow the host key (`SID`).
Semantic disclaimer: register from probes taken at different moments -
pictures from different times; comparison of images taken at different times
(`role_before` vs `role_after`) semantically the responsibility of the author
script, the engine only guarantees join by `SID`.

### `where:`-step key vs `soulprint.where(...)`-function - DIFFERENT positions

These are two different mechanisms, they should not be confused:

| | `where:` - step key | `soulprint.where(...)` - function in expression |
|---|---|---|
| **Where is it** | separate top-level key for scenario tasks | call inside a CEL expression (in `params:` through `${ … }`, in `apply: input:`, in `when:`/`where:`, ...) |
| **What does** | selects **on which hosts** to perform the step (per-host target filter) | returns **data** from other hosts using a stable coven (cross-host lookup) |
| **Data Source** | `register:` previous probe (volatile, runtime) | Postgres + Redis hot layer (stable Soulprint facts) |
| **When it resolves** | in the step target resolution phase | when rendering an expression using a template engine |

Example of shared but separate use:

```yaml
- name: Point replicas at the master
  where: register.redis_role.stdout == 'slave'   # KEY: replicas only (on: omitted = all members)
  module: core.exec.run
  params:
    command: "redis-cli replicaof ${ soulprint.hosts[0].network.primary_ip } 6379"   # FUNCTION: host data (all members = soulprint.hosts)
```

> `soulprint.where(<predicate>)` accepts a CEL predicate **static string literal** ([templating.md §2.3](../templating.md)); keyword style (`coven=...`) is not used. Inside the predicate, the element fields (`covens`/`os.*`/`sid`) and the external context (`incarnation.*`, etc.) are available. **All members of the run** are simply `soulprint.hosts` (the accessor is already incarnation-scoped) — there is **no** "by incarnation coven" predicate: `incarnation.name` is not a Coven and never appears in `covens`, so `soulprint.where("incarnation.name in covens")` is removed ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). A stable-coven filter "by literal coven X" is `soulprint.where("'<X>' in covens")` — **without** dynamic string concatenation (`"'" + some_var + "' in covens"` - prohibited, the predicate is expanded in the compile phase, see [templating.md §2.3](../templating.md)). Deep nesting of quotes is a well-known footgun, see [templating.md §8](../templating.md): the recommendation is to place such expressions in the `vars:` step.

> `where:`-key - position "on which hosts". `soulprint.where(<predicate>)` - position "where to get the value from". They are independent; confusing them is a reading error, not an alternative. (Since Soulprint after [ADR-008](../adr/0008-coven-stable-tags.md) stores only stable facts, `soulprint.where(...)` operates with a stable layer; the volatile role is exclusively through probe + `where:`-key.)

> `soulprint.self.*` in `where:` - own stable facts
> candidate host (per-host, stable layer), symmetrically
> `register.self.*`; not to be confused with `soulprint.where(...)` (cross-host
> lookup) and `soulprint.hosts` (list of all run hosts).

### Migration: `filter:` withdrawn

In previous examples, the key `filter:` was used to select hosts. **`filter:` was completely removed** and replaced with `where:`. Any occurrence of `filter:` in scenario is a validation error; when rewriting examples `filter: <predicate>` → `where: <predicate>`. Previous convention of sub-covens `{incarnation.name}-{role}` (for example, `coven: {{ incarnation.name }}-master`) - removed ([ADR-008](../adr/0008-coven-stable-tags.md)); instead probe + `where:`.

## 4.1. `soulprint.hosts` - list of run hosts (scenario-only accessor)

`soulprint.hosts` - built-in **scenario-context** accessor: list all
hosts of the current run (incarnation hosts, as seen by the roster resolver). Roster
resolves **at the start of each Passage** (stable online set of incarnations,
Postgres + presence-filter); within one Passage it is stable, at
refresh-border will be re-resolved - see subsection **"Roster stability"** below
([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). Everyone has
element - **stable** host facts:

| Element field | Type | Contents |
|---|---|---|
| `sid` | string | `SID` host (FQDN). |
| `role` | string | **declared-role** from `incarnation.spec.hosts[].role` (`master`/`replica`/…). This is **declared, NOT actual**: the actual role is volatile and is taken only by a live probe + `where:` key ([ADR-008](../adr/0008-coven-stable-tags.md)). On `create` (redis is not running yet, probe has nothing to poll) declared is the only topology source. The field may be **empty/`null`** for hosts bound to incarnation **outside declared-spec** (for example, host branch `add_replica`: an existing Soul is bound to the incarnation as a member, but the operator in `incarnation.spec.hosts[]` did not declare its role). The declared role reflects the **intent in the spec** and is not auto-filled with the fact of binding; the actual role of such a host is recorded in `incarnation.state` by the script `state_changes`, not in `soulprint.hosts[].role`. Declared-role consumer - bootstrap only `create` (probe not possible); runtime operations take on the role of a probe ([ADR-008](../adr/0008-coven-stable-tags.md)). |
| `network` | map | stable host network facts (`network.primary_ip`, `network.fqdn`, `network.interfaces[]`) - typed scheme [ADR-018](../adr/0018-soulprint-typed.md), full spec [`docs/soul/soulprint.md → NetworkFacts`](../soul/soulprint.md#networkfacts). The same stable layer that gives off `soulprint.where(...)`. |
| `os` | map | stable host OS facts (`os.family`, ...). |
| `covens` | array of string | stable host Coven labels — **real stable tags only** (cluster / project / environment / datacenter). The incarnation name is **no longer** projected here ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)); membership lives in `incarnation_membership`, not in `covens`. |

`soulprint.hosts.where("<predicate>")` filters the list by any stable
element attribute(`role`, `sid`, `covens`, `network.*`, `os.*`); result -
again a list with the same fields (`[0]` for the first element, `.size()` /
`size(...)`, indexing is the same as `soulprint.where(...)`). Predicate -
**static string literal**, expanded in the compile phase into an inline
CEL filter-comprehension (not runtime string execution): dynamic merging
predicate is prohibited, `.where` is allowed only on `soulprint.hosts`/
`soulprint.where(...)`, `.first` is not entered (the first element is `[0]`). Full
mechanics - [templating.md §2.3](../templating.md).

**Contact with `soulprint.where(<predicate>)`.** `soulprint.where("'X' in covens")` —
special case: the same list of run hosts, filtered by affiliation
coven `X`. `soulprint.hosts` - full list without filter; `.where(...)`
generalizes the filter to any stable attribute (not just coven). Signature and
predicate canon - [templating.md §2.3](../templating.md):
**predicate-string** (`"'db' in covens"`, `"os.family == 'debian'"`),
**not** keyword-args (`coven=...` - not supported by CEL). Source
there is only one data - stable Postgres layer + hot Redis layer.

**Scenario-only. destiny directly does NOT see `soulprint.hosts`.** Accessor
lives **exactly on the same level** as `soulprint.where(<predicate>)` —
scenario-context, NOT destiny ([destiny/tasks.md §10](../destiny/tasks.md#10-template-context):
cross-host `soulprint` requests - scenario level). destiny gets
topology **only** through explicit `apply: { input: { … } }` forwarding and only
if destiny declared the corresponding key in its input contract
([destiny/input.md](../destiny/input.md)). Isolation of destiny by topology is not
changes**: `soulprint.hosts`/`soulprint.where(...)` to `tasks/main.yml` destiny - validation error.

> **`soulprint.self.*` in destiny - available** (ADR-009/ADR-010 amendment 2026-06-18).
> Destiny isolation boundary runs on **self vs run topology**: stable
> target host self-fact (`soulprint.self.os.arch`, `.os.family`, `.network.*`, …)
> - per-host property of the host itself on which destiny is executed and accessible
> destiny-CEL directly; cross-host `soulprint.hosts`/`soulprint.where(...)` —
> is still only scenario + explicit `apply: input:` forwarding.

**`soulprint.hosts` is a function-in-expression, not a target-key.** Like
`soulprint.where(...)`, this is the "where to get the data" position (in `params:`,
`apply: input:`, `when:`, expressions), **not** key `where:`/`on:` ("on which
hosts"). The role here is **declared**; per-host volatile targeting -
still only probe + `where:`-key (§4, invariant
"`where:` without preceding probe - validation error" **not weakened**).

**Bootstrap targeting `create` (probe not possible).** On `create` actual role
no. Per-role steps `create` DO NOT require per-role step targeting: the step is in progress
wide (`on:` omitted = entire incarnation, or `on: [coven]`), and the role
is resolved by passing `soulprint.hosts` to destiny via `apply: input:` —
destiny gets the topology (list of roles + master address via
`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`) and configure
each host according to its declared role. This closes the former open Q "cross-host
master discovery instead of sub-coven `{incarnation.name}-master`" (see §8).

### Stability roster run ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md))

Roster run (set of hosts seen by `soulprint.hosts`,
`soulprint.where(...)`, the omitted `on:`, and
`incarnation.host_count`, §4.2) **stable within one Passage**, but **not on
entire run**. The exact invariant is a weakening of the previous "roster resolves once
up-front and the entire run does not change" ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md),
amends [ADR-009 §7](../adr/0009-scenario-dsl.md)):

- **Within a Passage the roster is fixed.** All Passage hosts see the same
list; determinism of waves `serial:` (§2.2.1), host selection `run_once:` (§2.2.2) and
topological `assert:` (§2.3) inside Passage is preserved without changes. Sorting
by `SID` (§2.2) still gives a reproducible order.
- **At the refresh border, the roster will re-resolve.** The border is a successful step
`core.soul.registered` with `refresh_soulprint: true` ([ADR-017](../adr/0017-keeper-side-core.md),
[ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)): after it
scenario-runner will re-resolve the incarnation roster **before the next Passage**.
Consumers roster in subsequent Passage (`soulprint.hosts`, the omitted `on:`,
`soulprint.self.*`) they see the already re-resolved set. Without `refresh_soulprint` step
roster between Passage remains the same.
- **The semantics of re-resolve is live-snapshot, NOT monotonic growth.** Re-resolve is
fresh snapshot of the **current online set** incarnation (same Postgres + presence layer,
the same as a regular up-front resolution), but **not** merging with the old roster. Set
**grows** as promoted hosts are onboarded (the created VMs raised EventStream
→ become visible), but may also **reduce**: a host that has gone offline to
refresh-border, from the live-snapshot **is excluded** (rolling the role to the fallen host is not
necessary). Don't expect the monotony of just-growing.
- **`refresh_soulprint` — passage-defining.** Any consumer of the roster after
of such a step the stratifier places in the **next** Passage
([ADR-056](../adr/0056-staged-render-passage.md): new edge class "roster-refresh"),
otherwise its render would have been seen by the old roster - silent-wrong-target. This is a separate axis
from register-dependency; The refresh border does not introduce register links.
- **Barrier/state-commit (§7) NOT affected.** Re-resolve - roster axis (who
target), not the commit axis. `incarnation.state` is still committed **one
times** after the last Passage; re-resolving the roster inside the run does not split it up.

A typical case is a single create scenario "provision → onboarding → role":
`core.cloud.provisioned` (`on: keeper`) creates N VM → `core.soul.registered`
(`await_online: true`, `refresh_soulprint: true`) registers their SID and blocks
waits for onboarding → the next Passage applies the role to already-online hosts via
the omitted `on:` / `soulprint.hosts`. Onboarding barrier and list-SID - on
same module, see [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)
and [docs/keeper/modules.md](../keeper/modules.md).

## 4.2. `incarnation.host_count` — run target size

`incarnation.host_count` - built-in **scenario-only** variable
template context, available in any expression-key (`when:`/`changed_when:`/`failed_when:`/`until:`/`where:`) and in string interpolation via `${ … }`.

| Field | Type | Semantics |
|---|---|---|
| `incarnation.host_count` | int | The number of hosts in the target of the run **after** resolving `on:` (according to the roster of the current Passage) and **before** applying `where:` (volatility filter). On the probe step that targets the entire incarnation, this is `size(soulprint.hosts)` for the corresponding run. It is considered according to **the same roster** as `soulprint.hosts`: stable within Passage, will be re-resolved at the refresh boundary (live-snapshot, see §4.1 "Stability of the roster run", [ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)) - the value in Passage after the `refresh_soulprint` step reflects the updated online set. |

**Purpose.** `incarnation.host_count` — run target size for expressions that need to know the incarnation width: thresholds/percentages in the script's own logic (`serial: "${ ... }"`-derived calculations, topology assertions in tests §6, sizing of parameters passed to destiny via `apply: input:`).

> **Not for "probe completeness".** Idiom `failed_when: size(register.<probe>) < incarnation.host_count` ("fall if not all hosts responded to the probe") **not used and not executable**: `failed_when:` is calculated by Soul-side per-host, sees only `register:` previous tasks and `register.self.*`, but **not** its own aggregate `register.<this-probe>` and **not** cross-host `size(...)` - the call gives CEL `no such key`. The completeness of the probe does not need protection: a destructive operation on an incomplete probe is guaranteed to be cut off by the fail-stop staged-render barrier (§5, [ADR-056 §g](../adr/0056-staged-render-passage.md)), rather than manual verification.

**Access in destiny - no.** Field - part of `incarnation.*`-namespace, which is not visible in destiny ([destiny/tasks.md §10](../destiny/tasks.md#10-template-context)). destiny receives the value, if it needs it, via `apply: input:` forwarding.

## 4.3. Convolution of aggregate `register` to a single value

Probe step with `register: <X>` accumulates per-host **card** `sid → payload`
(one probe runs on each target host, §4).
Therefore `register.<X>` is a **card**, and `register.<X>[<sid>]` is the payload of one
host (map `{stdout, changed, failed, …}`), **not a scalar**. Common
error - write `register.<X>.stdout`: there is no such key on >1 host (`.stdout`
lies inside the per-host payload, and not on the card itself), the expression will not be resolved.

When you need to collapse the map to one value (typical case - primary discovery:
probe on several existing hosts, primary prints its address, replicas -
empty), **canonical form**:

```
register.<X>.map(k, register.<X>[k].stdout).filter(v, v != '')[0]
```

- `map(k, register.<X>[k].stdout)` - comprehension by **keys** of the map (`k` -
`SID`); for each host reads `.stdout` of its payload → list of values.
Read `.stdout` element **required**: map element - payload-map, not
string.
- `.filter(v, v != '')[0]` - first non-empty: discards hosts that respond
empty (on replicas of the probe primary address prints an empty line), takes the first
remaining.

Example (primary discovery before point reconfiguration of replicas):

```yaml
- name: Detect actual redis primary address on existing hosts
  module: core.exec.run                            # on: omitted = all member hosts
  where: "!(soulprint.self.sid in input.replicas)"
  register: master_addr
  changed_when: false
  params:
    command: "[ \"$(redis-cli role | head -1)\" = master ] && redis-cli config get bind | awk 'NR==2{print $1}' || true"

- name: Point new replicas at the actual primary
  where: soulprint.self.sid in input.replicas      # on: omitted = all members
  apply:
    destiny: redis
    input:
      master_addr: "${ register.master_addr.map(k, register.master_addr[k].stdout).filter(v, v != '')[0] }"
```

> **`.values()` / `.keys()` are NOT available in the current engine.** Soul CEL environment
> Stack only connects `cel.StdLib()` ([templating.md §2.3](../templating.md));
> extension `ext.*` (`ext.Lists`/`ext.Strings`, etc.) **not connected**. Therefore
> iteration over the map is done **by keys** through `map(k, …[k])`, and not through
> `.values()`. Seductive shape `register.<X>.values().filter(...)` —
> compile error "no matching overload". `map`/`filter`/indexing `[…]` —
> StdLib macros work. (Otherwise you may step on the rake of the "standard",
> written under `ext.Lists`.)

## 5. Probe idiom and error handling

**Probe is a regular scenario step, not a special construct.** Probe = `module: core.exec.run` (or other read-only module) + `register:` + `changed_when: false`. No separate type of task, no special "fail-closed for target" invariant, no new attribute.

```yaml
- name: Detect actual redis role per host
  module: core.exec.run                                        # on: omitted = all member hosts
  register: redis_role
  changed_when: false                                          # probe state does not change
  params:
    command: "redis-cli role | head -1"
```

**The success of the probe is through the semantics of the module (`failed_when:` to `register.self.*`).** The Probe step is no different from the usual one: the status of the host is determined by the module (non-zero exit `core.exec.run` → host `failed`) or the inherited `failed_when:` **by its own result** (`register.self.stdout`/`.rc`). If the probe crashes on the host, **standard step fall handling** from the DSL core works (`retry:` / `onfail:` / script stop / `error_locked`).

> **"Probe completeness" is NOT expressed in a manual idiom.** The previous spec example carried `failed_when: size(register.redis_role) < incarnation.host_count` ("fail if not all hosts responded to the probe"). This idiom is **removed** - it is physically unexecutable: `failed_when:` is calculated by Soul-side per-host and sees only `register:` previous tasks + `register.self.*`; **own** aggregate `register.<this-probe>` by name and cross-host `size(...)` to it **not available** → CEL `no such key`. The completeness of the probe does not need manual verification - protection from destructive operations on an incomplete probe is provided by the fail-stop staged-render barrier (see footgun below).

Error handling in scenario - **only mechanisms inherited from the DSL core**: `retry:`, `onfail:`, `failed_when:`, `onchanges:` ([destiny/tasks.md §8, §9](../destiny/tasks.md#8-requisites---salt-style-dependencies)). **No new attribute is entered.**

> **Probe and its consumer are different Passage ([ADR-056](../adr/0056-staged-render-passage.md)).** The step reading `register:` probe (via `where:` / `apply: input:` / `params:` / `vars:`) is executed in the **next** Passage relative to the probe itself (probe and consumer cannot be in the same Passage). This ensures that `register:` has already been collected by the barrier of the previous Passage by the time the consumer is rendered. Before the staged-render rollout is completed on the current keeper, the mechanism resolves as it is implemented.

> **Footgun: silent-destructive-on-partial - closed by a barrier, NOT an idiom.** Danger: a probe to which some of the hosts did not respond returns an incomplete `register:`, and the next step with `where:` therefore `register:` would apply a destructive operation (restart, failover) only to the "responding" part. **Closing - fail-stop staged-render barrier ([ADR-056 §g](../adr/0056-staged-render-passage.md)), without manual completeness check:**
>
> - **The candidate host fell into probe-Passage** → the barrier of this Passage records a failure → the run stops, **the next Passage (where `where:` is located) does not start**, `incarnation.state` is not committed → `error_locked` (§7). The destructive step simply doesn't reach dispatch.
> - **The host is terminal, but `register:` is incomplete** (probe returned without the required key for a specific host) → when rendering `where:` of the next Passage, accessing `register.<probe>.*` of this host gives an eval error `no such key` → task `failed` (caught normally, §7), and not silently "the wrong target."
>
> Manual completeness idiom (`failed_when: size(...) < incarnation.host_count`) **needless and unenforceable** (see above and §4.2): safety provides a barrier. review/architect When reviewing scenario specs and the pilot, they check that there is no path left that bypasses the fail-stop barrier for destructive-on-failed/partial-probe.

## 6. Two-level resource resolution

`templates/`, `vars.yml`, `tests/`, `include:` - script goals are resolved **two-level**:

1. **Local first:** `scenario/<name>/<kind>/` (for example, `scenario/restart/templates/redis.conf.tmpl`).
2. **Then service-level:** shared resource of the same `<kind>` at the service level (`service-<x>/<kind>/`).

**Name collision - shadowing.** If the name exists both locally and at the service-level - **the near one completely overlaps the far one, without merging**. This is consistent with the priority rule of task-level `vars:` over file-level in [destiny/tasks.md §9](../destiny/tasks.md#9-strength-and-control-of-execution) (more local scope wins entirely).

**`../` is not allowed in the syntax.** The script writer **never writes** relative paths with `../`. Fallback to service-level is done by the **engine**, not the author: the author refers to the resource by name (`template: templates/redis.conf.tmpl` in the step `core.file.rendered`, `include: replication.yml`), the engine searches first locally, then at the service-level. **The resolved path is printed to the apply log** and checked by `soul-lint` (see backlog in [soul-lint.md](../soul-lint.md)).

#### Expanding `include:` - before render, into a flat list

`include:` expands into a **flat list of tasks BEFORE the render phase**, at
scenario-loader-layer (between `main.yml` parsing and CEL-render). Each
include-task is replaced by tasks of the included file **inline, in their place**;
nested `include:` are expanded recursively. Render gets already flat
list - `include:`-nodes remain in its input (if the node has reached render - this is
expansion software error, not "outside pilot volume"). Path resolution - two-level
(local → service-level, above); the included file has the same structure as
`tasks/main.yml` (top-level list of tasks).

**Cycle protection.** `include: a → b → a` (and direct self-include) **detected
by resolved-path**: re-entry of the path into the active discovery chain -
error `include_cycle` (not infinite recursion). Chain depth optional
limited by a hard ceiling (insurance on top of cycle-detection).

**Scope forwarding via `include:` (pilot constraint).** On the current slice is clear
`include: <file>` (opt. `name:`) splice flat, without scope transfer. On include-
task is also allowed **static** `when:` - conditional include (see below).
Include task with other scope/control modifiers (`vars:` / `loop:` /
requisites / `on:` / `where:`) **not expanded** - this is an error
`include_modifier_unsupported` (so that scope is not lost silently). Full forwarding
(task-level `vars:` on the include task are visible to the tasks of the connected file; `loop:` on
`include:` - repeat the file N times) - subsequent slice (see §8).

**Conditional include (`when:` on the include task) - render-phase group-drop ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-24).** On the include task, allow `when:` - then the connected group is included in the plan only if the predicate is true; if false - ALL tasks of the connected file are **physically absent** in the plan (real exception, not placeholder: not issued, index not reserved). The predicate must be **static** (`input.*`/`essence.*`/`incarnation.*`/`vars.*`) because include is expanded **before** stratification when `register:` is not yet collected and per-host `soulprint` is unknown; dynamic include-when (`register.*`/`soulprint.*`) → `include_when_dynamic_unsupported` (catches both expansion and `soul-lint` offline). Full semantics - [destiny/tasks.md §4](../destiny/tasks.md#4-basic-blocks) (scenario is inherited as is).

> **This is an override of the rule `include:` from [destiny/tasks.md §4](../destiny/tasks.md#4-basic-blocks).** In destiny `include:` is strictly a neighbor in the same folder `tasks/`, going beyond it is prohibited. In the scenario, the rule is **different**: `include:` (and resolve `templates/`/`vars.yml`/`tests/`) two-level - locally, then service-level, fallback is done by the engine. `tasks.md §4` **does not change** - the behavior for destiny is described there; the difference in scenario is recorded here.

> **Synthesized tasks `core.module.installed`.** Immediately after expanding `include:` (before stratification), Keeper inserts install steps of custom modules from `service.yml::modules[]` into the flat plan - before the first consumer task of each module, with the marker name `install <ns>.<module> (service manifest)`. These are regular tasks of the plan (render → dispatch → TaskEvent), in run-view they are visible like the rest. Synthesis mechanics (position, takeover by explicit step, MVP restrictions) - [keeper/modules.md → Auto-synthesis](../keeper/modules.md), [ADR-065](../adr/0065-core-module-installed.md).

### Script tests

Layout: `scenario/<name>/tests/<case>/case.yml`. Format `case.yml` - `verify:` / `expect:` - **reused from [destiny/testing.md](../destiny/testing.md)** (there is no separate DSL assertions, same approach). Delta scenario:

- **L0 render-multi-host is executed by the standard harness.** Roster of run hosts is set by `fixtures.hosts: [...]` (host record `sid`/`covens`/`role`/`soulprint`/`choirs`, format - [destiny/testing.md](../destiny/testing.md)). Render invariants on the topology (`size(soulprint.hosts)`-guard, `soulprint.hosts.where(...)`-projection, nodes-determinism master/replica, `state_changes` on multi-host) are driven hermetically `soul-trial run`, without docker and without dispatch (amendment 2026-06-22, [ADR-023](../adr/0023-trial-test-runner.md)).
- **L3-dispatch remains stub.** Docker `stand:` on the cluster topology, assertions "who really executes the master" (`assert.dispatch`) and committed cross-host `incarnation.state` after the barrier (§7) are postponed. The exact format of the multi-host block `stand:` inherits from open Q sandbox from [destiny/testing.md](../destiny/testing.md) (see §8 below).

> This is an **open Q extension about sandbox** from [destiny/testing.md](../destiny/testing.md): **L0 render-multi-host (`fixtures.hosts`) - closed** and executed by a standard harness (sealed render level). **L3-dispatch** (multi-host docker stand, `assert.dispatch`, committed cross-host `state`) is not a closed solution, an explicitly marked extension of an open issue. Does not close silently; before solving L3-dispatch - declarative-stub `stand:`, as in destiny-molecule.

## 6.1. `extends:` - inheritance of the general section contract from `covenant.yml`

`extends: <covenant-name>` - **top-level** script key: the script **inherits** the general service-level contract of sections `input:` / `compute:` / `state_changes:` / `validate:` from the covenant fragment file in the **root service repo** ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-29). Covenant - **service-level shared catalog**, isomorphic to `types.yml` ([ADR-062](../adr/0062-input-types.md), named input schemas) and service-level `include:` (§6, task sets): three mechanisms fumble between scenarios of different nature (type / tasks / contract sections), all resolve BEFORE consumers and do not introduce a wire entity.

**`extends:` NAMES the covenant file.** The meaning of `extends:` is **name of the covenant file without extension**: `extends: <name>` resolves to the file **`<name>.yml`** in the root of the service repo (the mechanism supports an arbitrary name, symmetrically to how `apply: { destiny: <name> }` addresses destiny by name). **Convention:** the general contract sections of the service are called **`covenant`** → file **`covenant.yml`**, link **`extends: covenant`**. This convention is used in all the examples below.

```yaml
# covenant.yml (service-repo root) - TOTAL minimum
input:
  redis_type: { type: string, default: standalone, enum: [standalone, sentinel, cluster] }
  password:   { type: string, vault_scope: secret }
compute:
  redis_config: "${ merge(essence.redis_config, default(input.redis_settings, {})) }"
state_changes:
  - set: redis_type
    value: "${ input.redis_type }"
validate:
  - that: "input.redis_type != 'cluster' || has(input.shards)"
    message: "cluster requires shards"
```

```yaml
# scenario/create_from_souls/main.yml - zero delta (all from covenant)
name: create_from_souls
create: true
extends: covenant
tasks: [ ... ]

# scenario/create/main.yml - delta +provision over covenant
name: create
create: true
extends: covenant
input:
  vm_count: { type: integer, min: 1 }   # ← additional field, it is NOT in covenant
tasks: [ ... ]
```

**Add-only merge, fail-closed.** Covenant - **minimum** (common base); the script **ADDS** its delta. Merging - **shallow by top key** sections (input field / compute name / list element `state_changes`/`validate`): top keys are merged, there is no recursive merging of values. Double top key (one input field in both covenant and script; one compute name; intersection of `set` fields `state_changes`) → error `section_key_conflict` (**NOT last-wins, NOT override**). The intent "base = general, scenario = incremental only" is made explicit; collision = author's error, not silent erasure. **Deep merge rejected** (would hide which part of the schema is from the covenant - the same "either link or inline" principle from [ADR-062](../adr/0062-input-types.md)).

**`form:` DOES NOT merge.** UI-`form:`-block ([ADR-045](../adr/0045-param-dsl.md)) from covenant **not inherited** - this is a form layout for a specific operation, local-only. Covenant carries a data contract, not a presentation.

**Append order is covenant FIRST.** In ordered sections (`compute:` §2.4, `state_changes:` §7.1, `validate:` §2.5) covenant elements come **before** local ones: local `compute[i]` can refer to covenant-`compute[j]` (`compute` resolves from left to right, §2.4), local `state_changes`/`validate` - rely on what has already been applied from the covenant. The reverse order would break the forward-reference covenant→local.

**The resolution point is BEFORE the consumers.** The merge is performed at a single resolution point of the manifest (isomorphic to `$type`-resolution [ADR-062](../adr/0062-input-types.md) and `include`-resolution §6): load `covenant.yml` → merge 4 add-only sections (fail-closed on a conflict) → then a **regular** pipeline on an ALREADY-merged manifest (input-merge → required → render → dispatch). All consumers (`ValidateInput`, render, `assert:`/`validate:`-eval, soul-lint) see a complete manifest - **fragment-aware of no code outside the resolution point**.

**MVP restrictions.**

- **One `extends:` per script** (not a list of covenants).
- **covenant does NOT extend-it covenant** (flat sheet, no recursion/chaining → cycle-detection is not needed on this layer, unlike `$type`).
- **covenant carries ONLY 4 sections** - `input:` / `compute:` / `state_changes:` / `validate:`. **NOT** `tasks:` / `name:` / `create:` / `form:` / `extends:` (different top key in the covenant file → validation error). Tasks (`tasks:`) are divided through service-level `include:` (§6), not through covenant.

> **Convention: one `covenant.yml` per service (recommendation).** The mechanism supports several covenant files (`extends: <name>` → `<name>.yml`), but **it is recommended** to keep one common contract section of the service in the file `covenant.yml` (`extends: covenant`). Several covenant files are justified only when the service has several unrelated families of scripts with different common contracts - for a typical service this is unnecessary fragmentation.

**Forward-compat.** Key `extends:` **optional**; script without `extends:` - manifest **bit-for-bit as it is now** (resolution point in the absence of `extends:` - no-op). The service without `covenant.yml` does not break.

> **Boundary `extends:` (covenant) ↔ service-level `include:` (§6) ↔ `types.yml` (`$type`).** Three service-level shared mechanisms, three different niches: `covenant.yml` (+`extends:`) fumbles ** contract sections** run (`input`/`compute`/`state_changes`/`validate`); service-level `include:` (two-level resolve, §6) fumbles **task sets** (`tasks`-fragments); `types.yml` (+`$type`) searches for **named input circuits**. Not to be confused: the common **set of steps** of the deployment (`cluster.yml`/`sentinel.yml`) is `include:`, the common **input contract/state** between scripts is `extends:`.

## 7. Barrier / state-commit invariant

Commit `incarnation.state` (applying `state_changes` script) is **cross-host final-barrier**:

1. The script unconditionally waits for the completion of **all** parallel tasks of **all** run hosts (final-barrier extension from [destiny/tasks.md §6](../destiny/tasks.md#6-parallelism-parallel-true) from one host to the cross-host scenario level).
2. Only **after** this barrier `state_changes` are committed to `incarnation.state` (Postgres).
3. If at least one task on at least one host is finally-failed → `state` **NOT committed** → incarnation goes to `status: error_locked` ([architecture.md → "Incarnation"](../architecture.md)).

**This is an invariant, not an option.** A state commit is allowed strictly after an unconditional cross-host barrier; partial commit with a partial failure is prohibited - otherwise `incarnation.state` will diverge from the actual state of the hosts. Applicable incl. to probe-footgun from §5: partial probe → `failed` → barrier commits failure → state is not committed.

### 7.1. Grammar `state_changes` - list of CRUD operations

`state_changes` declares **what** the script writes to `incarnation.state` after
barrier, **and where the value comes from**. This is a **ordered list of operations**
(YAML list, **not** map), each element is one **CRUD verb** (singular):
`set` / `add` / `modify` / `remove` + structural `foreach`. Grammar
committed [ADR-057](../adr/0057-state-changes-crud-verbs.md).

| Verb | Form | Semantics |
|---|---|---|
| `set` | `set: <field>` + `value: "${CEL}"` | overwriting the entire `incarnation.state.<field>` field. |
| `add` | `add: <collection>` + (map: `key:`+`value:` \| list: `value:`+opt.`match:`) + `on_conflict:` | add an element to the collection. |
| `modify` | `modify: <collection>` + `match:` + `patch: { <path>: "${CEL}" }` | patch **all** matching `match` (all-by-default). |
| `remove` | `remove: <collection>` + `match:` | remove **all** matching `match` (all-by-default). |
| `foreach` | `foreach: "${CEL-list|map}"` + `as: <name>` + `do: [<verb...>]` | bulk fan-out N operations. The form is literally from migration-DSL ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). |

The empty list (`state_changes: []`) is valid - the script does not change the state.

**Order and atomicity.** Operations are applied **in the order they are declared**,
sequentially, to the **intermediate** state (each sees the result of the previous ones).
The entire chain is one PG transaction, one `state_history`-snapshot (§7). Fail anyone
operations (eval CEL error, violated `expect`, `on_conflict: error`) →
`incarnation.state` **not committed** → `error_locked` (§7 barrier invariant not
weakened: operations are applied AFTER the cross-host barrier).

**Collection type is from `state_schema`** (`service.yml`). `add` to the missing field
→ materialize an empty map/list from the schema (the shape is known from the schema, even if
no value yet).

#### CEL operations environment

The value in `value:` / `patch:` and the predicate in `match:` are expressions rendered
Keeper-side after the barrier (§7) in the same CEL environment as `params:` tasks
([ADR-010](../adr/0010-templating.md), token `${ … }`). A literal without `${ … }` is assigned as is.
Non-string result of CEL (number/bool/list/map) - according to interpolation rules
[ADR-010](../adr/0010-templating.md) (whole cell = one `${…}` → native type).

**Full context** - `input.*` / `incarnation.*` / `soulprint.self.*` /
`register.*` / `vars.*` / `essence.*` / `compute.*` (§2.4). On top of it in
`match`/`patch`/`value` **local bindings of the current element are valid
collections**:

| Binding | Semantics |
|---|---|
| `elem` | the current element of the list collection (or a scalar, if list of scalars). |
| `key` / `value` | key/value of the current map collection entry. |

The name `elem` (not `self`) was chosen to avoid a collision with per-host `soulprint.self`.

**Per-RUN semantics.** Values are taken from `input/vars/incarnation/register`
run is per-RUN, **NOT** per-host union. If the expression gives different values
per-host (`${ soulprint.self.* }` / `${ register.* }`) - **last-wins by sorting
`SID`** (deterministic, like "last entry wins" in
[output.md](../destiny/output.md)).

#### `set` - field overwrite

```yaml
state_changes:
  - set: redis_version
    value: "${ input.version }"
  - set: greeting_file
    value: "${ vars.greeting_path }"
  - set: created_at
    value: "static-literal"        # literal without ${ } - assigned as is
```

#### `add` - collection growth (idempotent by default)

list-collection (opt. `match:` - grandfather predicate; `on_conflict: skip` = "add,
if not"):

```yaml
state_changes:
  - add: redis_hosts
    value: "${ vars.new_sid }"
    match: "elem == vars.new_sid"
    on_conflict: skip              # DEFAULT: repeat does not duplicate
```

map-collection (`key:` + `value:`):

```yaml
state_changes:
  - add: redis_users
    key: "${ input.username }"
    value:
      acl:   "${ input.acl }"
      state: "on"
    on_conflict: error             # double creation is a clear error
```

`on_conflict: skip` (default) `| replace | error` — collision behavior (key
map is busy / list-`match` already finds the element).

#### `modify` / `remove` - patch and delete by predicate

`match:` — CEL predicate over the element; patched/deleted **all** suitable
(all-by-default). Multiplicity is a property of the predicate, not a flag.

```yaml
state_changes:
  # point patch of one map entry
  - modify: redis_users
    match: "key == input.username"
    patch:
      acl:   "${ input.acl }"
      state: "${ input.state }"
  # patch ALL replicas at once
  - modify: redis_hosts
    match: "elem.role == 'replica'"
    patch:
      config_version: "${ input.version }"
  # remove host (with multiplicity assertion)
  - remove: redis_hosts
    match: "elem == input.sid"
    expect: one
```

**`expect: one | at_most_one | any`** (DEFAULT `any`) - opt. runtime assertion numbers
meshed `match` elements in `modify`/`remove`. Multiplicity ≠ expected (`one` -
exactly one, `at_most_one` - zero or one) → `error_locked` **before commit**.

**Empty-match → no-op** (idempotent) for `modify` AND `remove`: predicate not
caught nothing - the operation quietly does nothing, not an error.

**Wide match fuse** - `soul-lint` issues **WARN** on
constant-true predicate (`match: true`) and for **absence** `match:`
`remove`/`modify` (which would demolish/repatch the entire collection).

#### `foreach` — bulk fan-out

```yaml
state_changes:
  - foreach: "${ input.new_replicas }"
    as: sid
    do:
      - add: redis_hosts
        value: "${ sid }"
        match: "elem == sid"
        on_conflict: skip
```

`foreach`/`as`/`do` - same structural form as in migration-DSL
([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl),
[migrations.md](../migrations.md)). Inside `do:` the same verbs and binding are available
`<as-name>` to the current iteration element (on top of the full CEL context).

#### `register.*` as value source

`value:` / `patch:` / `match:` see `register.<task>.<field>` - result
`register:`-run tasks (host-probe **or** keeper-side step, see below):

```yaml
state_changes:
  - set: leader_host
    value: "${ register.elect.stdout }"   # from the register probe step: elect
```

Channel Soul→Keeper: `TaskEvent.register_data` of each task accumulates for
Keeper side in table `apply_task_register` (one row per `(apply_id, sid,
task_idx)`). **After** cross-host barrier (§7) scenario-runner loads
register-run data, resolve `task_idx → register-name` according to its plan
tasks and builds a **per-host** register map (`sid → register-name → payload`).
Rendering is per-host in the same last-wins order as for `soulprint.self.*`:
register **task host** addresses **result of exactly the host** for which
the value is calculated.

**Register keeper-side tasks (`on: keeper`) - run-level layer.** Register,
issued by a keeper-side task (for example `core.cloud.created` with
`register: provision`), available in the `state_changes` render as **run-level
substrate**: there is one keeper per run, the value is identical for all hosts. When
name collisions per-host register of a specific host **takes precedence** (host-wins).
Channel visibility limit ([ADR-056](../adr/0056-staged-render-passage.md)) when
this is saved: in `params:` / `when:` / `changed_when:` **host tasks**
keeper-register is still **not visible** - deliberate isolation of per-host
context from the keeper channel (the keeper channel itself of the keeper tasks of subsequent Passages
read as before). The keeper-register underlay is valid **only** in
`state_changes` — run-level construct that calculates the total `incarnation.state`.
Canonical consumer - cloud-provision read-model
([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)):
`state_changes` reads `register.provision.vm_ids` / `.hosts` from keeper step
`core.cloud.created` (see `provisioned_*` in
[`examples/service/redis/covenant.yml`](../../examples/service/redis/covenant.yml)).

`register.*` here is a **stable** post-barrier snapshot (values already
are recorded by the fact of successful apply), non-volatile runtime predicate as
`where:` (§5). Storage - Postgres (not in-memory): on a multi-Keeper cluster
([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) `TaskEvent` may not arrive
to the instance that runs run-goroutine; the general table is experiencing
cross-Keeper routing and does not allow you to commit state using an incomplete picture of register.

Referring to the register name of the **task host** for which this host did not provide
register data (there is no such probe task on the host), - eval error "no such key"
→ run `error_locked` (like any call to an undeclared key in CEL).
Keeper-register-link resolves from the run-level substrate on any host and gives
"no such key", only if the keeper task in the register run did not issue (step
dropped - for example, static-when group-drop provision branches). Conditional reading
gated by a predicate on always existing data: canon - gate by `input.*`
with short circuit ternary, **not** `has(register.…)` (see comment at
`provisioned_*` in redis `covenant.yml`).

**`no_log` does not fall into the state graph.** If the probe task is marked
`no_log: true` ([destiny/tasks.md](../destiny/tasks.md)), her `register` **not
is accumulated** into the per-host register-map: scenario-runner upon resolution
`task_idx → register-name` skips no_log tasks. The consequence is an operation,
referring to `register.<no_log-task>.*` gets "no such key" (run
`error_locked`), and the sensitive value from such a task **never
settles in the stored `incarnation.state`**. This is source protection: the secret is not
reaches the state physically, and is not masked after the fact. Output masking
external GET channels (`GET /incarnations`, `/history`) - independent second
defense-in-depth layer (see [keeper/operator-api.md → Secret masking](../keeper/operator-api.md)).

#### Transition period (deprecated map form)

Before [ADR-057](../adr/0057-state-changes-crud-verbs.md) `state_changes` was
**map** with keys `sets` / `appends` / `modifies`:

```yaml
state_changes:        # DEPRECATED form (transition period)
  sets:               # → translated into a sequence of `set` elements
    redis_version: "${ input.version }"
  appends: [redis_hosts]          # was a no-op placeholder (state did not grow!)
  modifies: [redis_users.*.acl]   # was a no-op placeholder
```

`sets` - map `<field>: <CEL>` - has been implemented (field overwriting), equivalent
sequence of `set` elements of the new list. `appends` / `modifies` —
**placeholder declaration without value source**, by the engine **not used**
(`incarnation.state` did not grow: `add_replica`/`add_user`/`update_acl` passed
successfully, but the collection did not change - a latent bug that verbs fix
`add`/`modify`).

**Transit (breaking).** map form is parsed **one release** as DEPRECATED
(dual-parse, `soul-lint` warn): `sets` is translated into `set` elements, non-empty
`appends`/`modifies` remain no-op (the behavior does not change) with the warning "rewrite to
`add`/`modify`, otherwise the state does not grow." In the next release the map form will be **removed**
(parsing the old form → validation error).

## 8. Open questions (extensions, do not close silently)

- **Per-task granularity `serial:`.** Current model is per-**Passage** min-width
(see §2.2.1; staged-render gives per-task dispatch along the task axis via Passage).
Truly per-task waves (each task in Passage has its own width) remain
deferred: within one Passage, tasks go to the host with one `ApplyRequest`. New
ADR for a real request.
- **Service-level location of include targets.** Two-level resolve (§6)
looks for the include target first locally (`scenario/<name>/<file>`), then on
service-level. The current implementation treats service-level as a shared directory
`scenario/<file>` (parent of script directories). This is a worker default; exact
canonical place of common include resources of the service (the same `scenario/`, or
separate directory) is not a closed solution.
- **Full scope forwarding via `include:`.** Now only pure scope is expanded
`include: <file>`; scope/control modifiers on an include task
(`vars:`/`loop:`/`when:`/requisites/`on:`/`where:`) are rejected
(`include_modifier_unsupported`, §6). Forwarding semantics (task-level `vars:`
visible to tasks of the connected file; `loop:` to `include:` - file repeat) -
subsequent slice.
- **Multi-host sandbox (L3-dispatch).** **L0 render-multi-host (`fixtures.hosts`) - closed** (amendment 2026-06-22, [ADR-023](../adr/0023-trial-test-runner.md)): the list of hosts is driven by a standard harness at the render level. **L3-dispatch** remains open: docker block format `stand:` for multi-host scenario test, `assert.dispatch` (who actually executes the master) and assertions for committed cross-host `incarnation.state` - open Q extension about sandbox from [destiny/testing.md](../destiny/testing.md). Not a closed solution.
- **Move `role/*.yaml` to destiny-`input:`.** Parameters that previously depended on the role (the essence layer `role/*` has been removed, see [concept.md](concept.md)), are moved to destiny through `input:` by probe-role - a separate implementation task (pilot and a batch of rewriting examples).
- **Bootstrap role source on `create` is CLOSED.** On `create` redis more
is not running, probe is not possible → topology (declared roles + master address)
is taken from `soulprint.hosts` (declared from `incarnation.spec.hosts[].role`,
see §4.1) and is forwarded to destiny via `apply: input:`. Per-role
step targeting for `create` is not entered; `where:`-invariant
(register-only-after-probe) is not weakened. Old wording
"cross-host master discovery - pending propose-and-wait" has been removed.

## 9. See also

- [concept.md](concept.md) - what is a scenario, border with destiny, declared vs actual role, role-agnostic essence.
- [destiny/tasks.md](../destiny/tasks.md) - **DSL task core**, inherited by scenario entirely (source of truth according to `module`/`include`/`block`/`parallel`/`loop`/`register`/requisites/`retry`/`timeout`/`changed_when`/`failed_when`/template context).
- [architecture.md → ADR-008](../adr/0008-coven-stable-tags.md), [ADR-009](../adr/0009-scenario-dsl.md).
- [architecture.md → "Targeting and host communication"](../architecture.md) - `on:`/`where:`, resolver contract, probe.
- [architecture.md → "Service - structure and manifest"](../architecture.md) - service repo layout and `scenario/<name>/main.yml`.
- [destiny/testing.md](../destiny/testing.md) - format `case.yml`/`verify:`/`expect:`, differentiation between scenario tests / destiny-molecule / service-smoke.
- [docs/input.md](../input.md) - `input:` standard for `input:` script block.
- [soul-lint.md](../soul-lint.md) — backlog of statistical checks scenario (`where:`/`on:`-literals, inline mutation).
