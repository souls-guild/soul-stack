# Scenario - orchestration layer specification

This document is a **normative specification of the delta scenario** on top of the destiny task DSL core. The source of truth when implementing a scenario orchestrator.

**DSL task core is NOT duplicated here.** All task blocks (`module:`, `include:`, `block:`, `params:`, task-level `vars:`, `when:`, `async:`, `loop:`, `register:`, `output:`, `onchanges:`, `onfail:`, `require:`, `changed_when:`, `failed_when:`, `retry:`, `timeout:`), their semantics, barriers, requisites and template context - **are fully described in [destiny/tasks.md](../destiny/tasks.md)** and are inherited by the scenario as is. This document covers **only what destiny doesn't**: targeting, cross-host coordination, `apply: { destiny: … }`, `incarnation.state` entry, resource resolution, script tests.

Any key not described here and not described in [destiny/tasks.md](../destiny/tasks.md) is a scenario validation error.

Related documents: [concept.md](concept.md), [destiny/tasks.md](../destiny/tasks.md), [architecture.md → "Targeting and host communication"](../architecture.md), [architecture.md → "Service - structure and manifest"](../architecture.md), [ADR-009](../adr/0009-scenario-dsl.md).

## 1. File format and layout

```
scenario/
├── _<family>/                # OPT.: shared bodies of a family of scenarios (NOT a scenario)
│   ├── provision.yml
│   └── deploy.yml
├── <shared>.yml              # OPT.: service-level include neighbor (flat form)
└── <name>/
    ├── main.yml              # entry point: name, description, input, tasks (inline)
    ├── <sub>.yml             # include neighbors (same task structure)
    ├── templates/            # OPTS: templates used by the steps in this script
    ├── vars.yml              # OPTS: scenario locales (like destiny vars.yml)
    └── tests/                # OPT.: tests of this script
        └── <case>/
            └── case.yml
```

`main.yml` contains **inline** `input:` and `tasks:` (state is written by `core.state.<verb>` steps inside `tasks:`, §7). Neighboring `*.yml` are connected via `include:` ([destiny/tasks.md §4](../destiny/tasks.md#4-basic-blocks)). Folder layout (`templates/`, `vars.yml`, `tests/`) - **symmetrical to destiny** intentionally, without a separate dictionary; two-level resolve - see §6.

**A directory under `scenario/` whose name starts with `_` or `.` is NOT a scenario** ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-08-17). It holds shared task bodies of a family of scenarios (`create`, `create_from_souls`, later `update`/`destroy`), addressed as `include: _<family>/<file>.yml` (§6). It is skipped by scenario discovery **even when it does contain `main.yml`** — the prefix is the signal, not the absence of an entry point — so a shared body never turns up in the scenario listings the API serves, nor in the deprecation walk. Discovery is what the prefix governs: it removes the directory from the listings, and is not an admission check on the run path. The same rule applies to `upgrade/`. `soul-lint` is not a discoverer (`validate-scenario` takes an explicit path), so nothing there filters the name; what skips these directories is whatever walks a `scenario/` tree — the two keeper walkers above, and the corpus loop in the repo `Makefile`.

Structure `main.yml` (blocks `name`, `description`, `input`, `tasks`) - in [architecture.md → "`scenario/<name>/main.yml`"](../architecture.md). Block `input:` - according to the general standard [docs/input.md](../input.md).

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
| `transport:` | string (transport name) OR map keyed by ONE transport name | module/apply/`block:`-task, Soul-side only | optional (omitted = the registry decides) — see §2.2.5 |

In addition to the per-task keys, scenario has **top-level** blocks: `compute:` — calculated run vars (§2.4); `validate:` - declarative input invariants (§2.5); `extends:` - inheritance of the general service-level contract of sections from `covenant.yml` (§6.1).

Everything else in the scenario task is exactly the same as in the destiny task, with the same semantics ([destiny/tasks.md §3–§10](../destiny/tasks.md#3-complete-list-of-task-blocks)). Discrepancies with destiny are explicitly listed in §6 (resource resolution) and §10 (template context).

### 2.1. `apply:` - destiny challenge

```yaml
- name: Install redis on all cluster hosts
  apply:
    destiny: redis                 # name destiny from service.yml → destiny:
    input:
      version:  "${ vars.redis_version }"
      password: "${ input.redis_password }"
```

`apply:` is the place where the scenario delegates work to an isolated destiny. `destiny:` - name from the dependency registry `service.yml` (resolve ref - [ADR-007](../adr/0007-versioning-git-ref.md)). `input:` - values ​​included in the `input:` destiny contract; destiny validates them with its contract ([destiny/input.md](../destiny/input.md)). The task with `apply:` is an **applier-task**; `apply:` and `module:` are mutually exclusive in the same task.

#### 2.1.1. `register:` on the applier task - reading the result of destiny

An Applier task can carry `register: <name>` ([destiny/tasks.md §8](../destiny/tasks.md#8-requisites---inter-task-dependencies)) inherited from the DSL core. `register.<name>` has two parts with different implementation status:

- **DSL core `.changed` / `.failed` / `.timed_out` - implemented.** This is **unit `OR`** for all child destiny tasks of the applier: `changed = OR(child.changed)`, similar to `failed` / `timed_out` (`skipped` is always `false`). External `onchanges: [<name>]` / `onfail: [<name>]` / `when: register.<name>.changed` resolves for this unit (previously it crashed with the "unknown register" error). If all child destiny tasks are filtered (`where:` / `include`-`when:`), the aggregate is reduced to `changed/failed/timed_out = false` (no-op applier).
- **`.<output field>` according to the declared top-level `output:`-contract destiny - PLANNED.** Forwarding application fields destiny ([destiny/output.md](../destiny/output.md)) to `register.<name>.<output field>` is **not yet implemented** (a separate future slice, see note in [destiny/output.md](../destiny/output.md#how-the-caller-reads---register-on-the-applier-task)). In the current volume, `register.<name>` carries only the DSL core. The producing half is unbuilt too: a destiny task cannot fill those fields, because task-level `output:` is refused (`output_unsupported`, [destiny/tasks.md §9](../destiny/tasks.md#9-strength-and-control-of-execution)).

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

#### 2.1.2. The applier's own keys - what reaches the group and what decides before it

An applier expands into N destiny tasks, so each of its own keys has to be answered somewhere. Which place depends on **where the key can be resolved**, and the split is not cosmetic - a key with no place to go used to be dropped in silence (NIM-245).

| Key on the applier | Where it is answered |
|---|---|
| `onchanges:` · `onfail:` · `require:` | **Merged into every destiny task of the group**, exactly as a `block:` passes its own down ([destiny/tasks.md §6.5](../destiny/tasks.md)). These are resolved from register **name to task index** over the whole flat plan, so an index means the same thing on both sides of the destiny boundary. |
| `where:` · `on:` · `run_once:` | **Before the group is rendered** - they select the hosts the whole destiny lands on (§4, §2.2.2). `where:` is the register- and soulprint-capable one: use it for any host-variant condition. |
| `serial:` | Inherited by every destiny task - the whole destiny rolls as one wave (§2.2.1). |
| `when:` | **At render, Keeper-side** - and therefore it must be **static** (`input.` / `vars.` / `incarnation.`). A static-false applier collapses into a single skip placeholder carrying its own `register:`; a static-true one renders normally. |
| `async:` | **Refused** (`async_on_apply_invalid`) - asynchrony of a whole group is deferred ([ADR-0075](../adr/0075-intra-host-async-tasks.md)). |
| `vars:` | **On the caller's side, into `apply.input`** - the applier's own `vars:` plus any a `block:` above merged in are resolved in the scenario env, and only the resulting VALUES cross into the destiny, exactly like every other `apply.input` value. NOT inherited by the children: the destiny renders its own locals from its `vars.yml` ([destiny/vars.md](../destiny/vars.md)) and never sees its caller's. |
| `changed_when:` · `failed_when:` · `retry:` · `timeout:` · `params:` | **Refused** (`<key>_on_apply_invalid`) - module-specific keys that an applier cannot answer, see below. |
| `id:` · `loop:` | **Refused** (`id_unsupported_target` / `loop_unsupported_target`) - both are allowed only on a module task in the pilot; an applier is one of the discriminators they already cover. |
| `output:` | **Refused** (`output_unsupported`) - not because an applier cannot answer it, but because it is unimplemented on **every** task kind and belongs to the output-contract slice (§2.1.1) that nothing has built yet. Its own code, not `<key>_on_apply_invalid`: the key is unbuilt, not meaningless here. |

**A `when:` that reads `register.*` or `soulprint.*` on an applier is an error** (`apply_when_dynamic_unsupported`), not a slow path. It cannot be decided at render, and it cannot be handed to the group either: a destiny task's flow context is built in the **isolated destiny env**, where `input.` and `vars.` name different things than in the scenario the predicate was written in - the same text would answer a different question. Until it was refused, the key was dropped and the destiny applied **everywhere, including on the hosts the author had gated off**.

Both replacements work today and cover the cases in practice:

```yaml
# host-variant condition → where: (Keeper-side targeting, reads register and soulprint)
- name: Apply the redis destiny only on debian hosts
  where: soulprint.self.os.family == 'debian'
  apply: { destiny: redis, input: { action: apply } }

# "only if that source changed" → onchanges: (now reaches every task of the group)
- name: Render the cluster config
  module: core.file.rendered
  register: cluster_conf
  params: { src: cluster.conf.tmpl, path: /etc/redis/cluster.conf }

- name: Apply the redis destiny only when the config actually changed
  onchanges: [cluster_conf]
  apply: { destiny: redis, input: { action: apply } }
```

★ The requisites merge as a **union**, as they do on a `block:`: an applier naming a source and a destiny task naming its own end up naming both. For `onchanges:`/`onfail:` that composes as OR - the task runs if **any** named source changed/failed - so an applier-level `onchanges:` **widens, rather than narrows**, the gating of a destiny task that already had one. This is the normative behaviour of both group constructs, stated in full in [destiny/tasks.md §6.5](../destiny/tasks.md#65-block---inline-task-group); note that `when:` on the same applier composes the other way (AND, narrowing), and the two axes are deliberately different.

Concretely, with a destiny task written as

```yaml
# examples/destiny/dragonfly/tasks/install.yml
- name: Restart DragonFly because the binary or unit changed
  module: core.service.restarted
  onchanges: [dragonfly_bin, dragonfly_unit]
```

an applier that adds `onchanges: [df_config]` over that destiny does **not** confine the restart to a config change - the task ends up gated on `[df_config, dragonfly_bin, dragonfly_unit]` and restarts on a binary change even when `df_config` never moved. A destiny task carrying no requisite of its own is unaffected: for it the applier's `onchanges:` is a pure narrowing, which is the case shown above. Expressing "applier AND destiny task" needs a wire change and is deferred to NIM-351; until then, if the inner requisite must stay authoritative, gate the applier with `where:` instead, or leave the requisite off the applier.

**Module-specific keys on an applier are refused** (family `<key>_on_apply_invalid`, the apply-side mirror of `<key>_on_block_invalid`, NIM-286). The membership rule is that the key **works on a module task and is lost on an applier**: an applier invokes no module, and render hands its children only the three requisites, so nothing else it carries reaches a rendered task. A key that turns out to be answerable on the caller's side leaves the family rather than staying refused - `vars:` did, see below.

| Refused key | Why it has no answer here | Write instead |
|---|---|---|
| `changed_when:` · `failed_when:` | No module result to re-judge. | Put it on the destiny task whose result you are re-judging; the applier's `register:` already reports `changed`/`failed` as the OR of its children (§2.1.1). |
| `retry:` | Repeats **one** module call; a group has no single call to repeat, and re-running the group is a different operation. | Put `retry:` on the destiny task that can be retried on its own. |
| `timeout:` | Bounds **one** module call, not the duration of a group. | Put `timeout:` on the destiny tasks that need bounding. |
| `params:` | Module arguments, and an applier calls no module. `module:`+`apply:` is caught as `task_discriminator_multiple`; a lone `params:` used to slip through. | `apply: { input: { … } }`, checked against the destiny's own `input:` contract. |

★ These were not simply dropped, which is why refusing beats leaving them: a static-false `when:` collapses the applier into one skip placeholder that **does** copy `changed_when`/`failed_when`/`timeout`/`id` onto itself. The keys were honoured exactly when they could not matter, and ignored whenever they could.

★ `vars:` was in this list and left it (NIM-336). It turned out to be the one member that **could** be answered here: `apply.input` renders in the scenario env, so resolving the applier's locals there and letting only the resulting values cross is what every other `apply.input` value already does - isolation is untouched. Refusing it also could not reach the second entrance, where the loss actually showed: a `block:` passes its `vars:` to every descendant (§6.5), that key is written on the block where it is legal, and an offline validator therefore never sees it. Before the fix a module descendant of such a block could read `${ vars.x }` and an `apply:` descendant could not - the render failed there with "no such key", loudly but for no reason the author could act on.

### 2.3. `assert:` — render-time precondition

```yaml
- name: cluster topology matches
  when: "input.redis_type == 'cluster'"
  assert:
    that:
      - "size(soulprint.hosts) == (has(input.shards) ? int(input.shards) : 0) * (1 + int(input.replicas_per_shard))"
    message: "topology mismatch: hosts != shards*(1+replicas_per_shard)"
```

`assert:` - **checking the run invariant at the model stage** ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-23). This is the **fifth discriminator** of the task: `assert:` is mutually exclusive with `module:`/`apply:`/`include:`/`block:` (assert - check, not executable work).

- **`that:`** is a non-empty list of CEL-bool predicates (each entire line = CEL, without the wrapper `${ … }`, like `where:`). All must be `true`.
- **`message:`** - optional human-readable message in the error text upon failure (if omitted, default message).

**Semantics:**

- **Computed by Keeper-side in the render phase**, in the full scenario-CEL context - **including `soulprint.hosts`** (`AllowHosts=true`, like `where:`): assert sees the run roster (topology), unlike Soul-side flow-control, to which `soulprint.hosts` is not available.
- **Run-level** (once per run, not per-host): assert tests a run topology invariant, not a per-host predicate.
- **The first `false` breaks render** with a clear error (`message` + text of the failed predicate). **Not a single task is left on Soul** - the run does not start (fails at the model stage). All `true` → the task "disappears" from the plan: assert **does not emit RenderedTask** (this is a check, not a task), the indexes of subsequent tasks do NOT reserve a position for it.
- **Gates `when:`** like any task: statically-false `when:` (placeholder-skip, e.g. `when: input.redis_type == 'cluster'` on a standalone run) → assert is NOT calculated.
- **Not a wire entity.** assert does not change proto Keeper↔Soul and Soul contract (render construct like `block:`).

**Motive.** Replacing the `core.cmd.shell`-guard hack (`test "${ <cel> }" = "true" || { echo ...; exit 1; }` is an arbitrary shell on Soul for the sake of control-flow) with a declarative keeper-side check: less attack-surface (no shell execution for the sake of checking), the failure is transferred from the host to the model (earlier and clearer).

#### 2.3.1. Where an `assert:` is answered — and what that costs the operator

An `assert:` is evaluated at **two points from one source** (`render.evalAssertTask`): a **pre-flight** gate on the request path, and **render** as a fail-safe ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-23, form A). Which point answers a given assert is **not a property of the assert alone** — it depends on what the predicate reads and on how the run was started. The difference is not cosmetic: pre-flight answers **422 with nothing mutated**, render answers **`error_locked`**, which the operator must then `unlock` before anything else can run.

Two facts decide it. A roster only exists once the incarnation row does (membership FKs it, [ADR-008 amendment / NIM-124](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)), and some plans **create their own roster mid-run** (a machine-provider plugin step → `core.soul.registered` with `refresh_soulprint`), so the hosts such an assert is about do not exist at request time under any design.

| The predicate reads | How the run started | Answered at | A failure looks like |
|---|---|---|---|
| `input.` / `vars.` / `incarnation.` only | any | **pre-flight** | **422 `assert-failed`**, nothing created, nothing locked |
| the roster (`soulprint.*`) | explicit run, plan consumes an existing roster | **pre-flight** | **422 `assert-failed`**, the run never starts |
| the roster | `POST /v1/incarnations` (a `create: true` starter) | render | `error_locked` → `unlock` |
| the roster | plan builds its own roster (all-keeper, or carries a refresh emitter) | render, **after** the refresh boundary | `error_locked` → `unlock` |

Read the last two rows as the same rule: **pre-flight declines to measure a roster that is not the one the assert is about**, because the alternative is worse than a late answer — before this was fixed, a create carrying a topology guard was rejected 422 unconditionally, with no input that could satisfy it ([ADR-009 amendment 2026-07-28](../adr/0009-scenario-dsl.md#amendment-2026-07-28-nim-235-a-roster-reading-assert-has-no-pre-flight-point-at-create)).

Practical consequences when authoring:

- **If the invariant fits `input.*` alone, write it as [`validate:`](#25-validate--declarative-input-invariants) instead.** It is the mechanism built for that, and it answers 422 on every path.
- **A roster guard in a `create: true` scenario is legitimate but late.** `soul-lint` says so at authoring time (`assert_roster_deferred_on_create`, a WARNING) rather than letting you discover it on a live stand.
- **The same scenario reached as an explicit run does get the synchronous answer.** The operator path for deploying onto hosts you already have is: create the incarnation without a bootstrap run → bind members → run the scenario ([ADR-008 amendment / NIM-209](../adr/0008-coven-stable-tags.md#amendment-2026-07-28-nim-209-membership-has-an-operator-path--bind--unbind--read)). That is where the topology guard pays off.
- **Render is never removed as a fail-safe.** Even when pre-flight answers, the roster can change between the request and the goroutine start (TOCTOU); the render evaluation stays.

### 2.2. Cross-host step execution model

DSL core ([destiny/tasks.md](../destiny/tasks.md)) describes the execution of the step
**on the same host**. Scenario adds a "target hosts" axis. Basic model,
on top of which `serial:` and `run_once:` work:

- Step with a target of M hosts (after resolving `on:`+`where:`, see §3–§4) by
by default applies **to all M hosts in the same wave** (cross-host
fan-out), then a cross-host join before the next step. Join subordinate
to the cross-host barrier invariant (§7): the next Passage starts strictly
after the step has completed on all hosts of the run, not host by host — and a
`core.state.<verb>` capture, being a keeper task, is one of those next steps.
- Host order in any phased processing (waves `serial:`, host selection
for `run_once:`) **deterministic**: lexicographically by `SID`.
Non-deterministic order is prohibited - it breaks reproducibility
destructive operations and topology assertions in script tests (§6).

This axis is **orthogonal** to `async:` and `loop:` DSL cores - do not confuse:

| Mechanism | Axis | Source | Semantics |
|---|---|---|---|
| `async:` (tasks.md §6) | concurrent flows on ONE host | — | fire-and-forget, the flow does not wait; barriers are `require:` / a register reference / the end of the run |
| `loop:` (tasks.md §7) | data collection | `input.*` / `vars.*` | repeat step by element |
| `serial:` | Target HOSTS | resolve `on:`/`where:` | waves across ≤N hosts, waves sequentially |
| `run_once:` | Target HOSTS | resolve `on:`/`where:` | exactly one (first by SID) target host |

Combinable: step with `loop:` under `serial:` runs the entire loop on each
wave host; `async:` inside the host works regardless of waves.

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
**does not split a Passage**: the wave loop closes inside the Passage, and only
then does the next Passage start. A `core.state.<verb>` capture placed after a
`serial:` step therefore runs once, after **ALL** waves across all hosts, never
wave by wave. What a capture commits is its own write at its own step (§7), so a
run that dies in a later wave leaves the captures that already ran — deliberately,
not as a partial commit of one block.

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

#### 2.2.5. `transport:` — how this task reaches its hosts

`transport:` names the way the Keeper gets to a host for this task. It sits at the
TASK level, beside `on:`/`where:`/`serial:`/`run_once:`, and **not** inside `apply:`:
the key is about delivery, not about appliers, and it is meant to cover a `module:`
task as well (NIM-870, owner's decision 2026-09-14).

Two forms — the scalar, and a map keyed by the transport's NAME. They mean the same
thing; the map only exists to carry parameters:

```yaml
- name: Install the agent over SSH
  transport: ssh
  apply: { destiny: soul-install }

- name: The same, through a named bastion, as a named user
  transport:
    ssh:
      ssh_provider: vault-bastion
      user: deploy
      port: 2222
  apply: { destiny: soul-install }
```

Scalar-or-map is the established idiom here: `on:` is `"keeper" | [list]`, `require:`
is `[list] | "all"`, `serial:` is `int | "50%"`. **Exactly one key** in the map — two
transports on one task have no defined order of preference, and picking one would make
the run depend on map iteration order.

The value space is a **closed enumeration**, so `soul-lint` can judge the key offline
with no keeper to ask:

| transport | what it is | params |
|---|---|---|
| `agent` | the gRPC EventStream to a Soul already running on the host ([ADR-012](../adr/0012-keeper-soul-grpc.md)). What a task without the key gets — but ⚠ writing it inside a **push** run is a contradiction (the run *is* the ssh transport) and fails the run rather than falling through. | none |
| `ssh` | the agentless push flow ([ADR-004](../adr/0004-binaries.md), [ADR-032](../adr/0032-push-orchestrator.md)): the Keeper opens an SSH session, delivers `soul` and execs it. | `ssh_provider`, `user`, `port` |

An unregistered name is `transport_unknown`; a map with two keys is
`transport_multiple`; a param the transport does not take is `unknown_key`.

**The task wins on the three fields it carries.** `transport.ssh.{ssh_provider,user,port}`
beats `souls.ssh_target` and beats the cluster defaults in `keeper.yml::push.*`. ★ That
knowingly makes a THIRD source of truth for those three fields, so a run that used the key
**says so**: the transport the task named, the effective provider, the fields the task
overrode, and the level that picked the provider are written into
`push_runs.summary.hosts[]` for a bare push run, and into the run's `apply.dispatched`
audit event for a scenario run dispatched over PUSH (the agent branch's
`apply.dispatched` carries only `sid`/`apply_id`/`tasks_count`, as it always has — there
is no third source of truth to disambiguate on a stream) — see
[keeper/push.md](../keeper/push.md#transport-precedence). Without that an incident review
reads a registry row the run never went to.

**What it does NOT move is the branch.** Which way a host is reached is the host's own
`souls.transport`, and naming the other one is a REFUSAL, not an override
([`transport_mismatch`](../keeper/push.md#a-scenario-over-push)). The address, the
credentials and the very existence of a push target live in the registry row — a task
that could retype a host would be a task that bootstraps a bare VM, and it is not (see
below). So the key states the transport out loud and carries the three overrides; the registry
decides the branch, and the two must agree.

⚠ **`soul-lint` cannot check the agreement** — it has no registry to ask. Offline it
judges the key's SHAPE: a registered transport name, one key in the map, a known param,
an integer port, no interpolation. That a task names `ssh` for a host whose row says
`agent` is a REFUSAL AT DISPATCH (`transport_mismatch`), before any session opens and
before anything is applied — loud and fail-closed, but at run time.

`transport:` is therefore optional on a push host: a task with no key targeting a
`transport: ssh` member goes over push all the same. Writing it is how a scenario says
which hosts it is about, and how a run that targets the wrong ones stops instead of
half-working.

**Three things the key deliberately does not do.**

- **It does not bootstrap a bare VM.** The host, its ADDRESS and its provider still
  come from the registry: `core.bootstrap.issued` writes `transport='agent'` as a
  literal and refuses `ssh`, and `souls.ssh_target` has no address column at all (its
  `Host` is the SID, while a fresh VM has only a `primary_ip`). There is deliberately
  no address parameter, so the key cannot be read as solving this.
- **It is refused on a keeper-side task** (`transport_on_keeper_invalid`) and **inside a
  destiny**. A keeper task never leaves the Keeper, so there is no host at the far end; a
  destiny is rendered per host and shipped whole to the ONE transport the scenario task
  that applies it chose, so a second name there has nothing to act on. A keeper-side
  module that dials hosts itself — `core.ssh.run` — takes its own `ssh_provider`/
  `hosts` params, and its direct/teleport mode stays `keeper.yml::push.transport`.
- **It is not interpolated** (`transport_interpolation_unsupported`). The key is
  decided once per task, before the hosts are resolved, so there is no per-host env
  to resolve `${ … }` against. Write the values literally.

On a `block:` the key is an inherited default: descendants that write their own win,
the rest take the block's — the same direction as `where:` and `serial:`. Blocks nest,
and the **nearest** enclosing block wins: an inner block's key is not overridden by the
one outside it.

### 2.4. `compute:` - calculated vars of the run

`compute:` - **top-level** script block (next to `input:`/`tasks:`, **not** per-task key): map `<name>: <CEL expression>`, which Keeper resolves **ONCE per run** and makes available as `compute.<name>` in the run's render contexts — a task's `params:` / `where:` / `vars:` and `apply: input:`, on the Soul side and under `on: keeper` alike, a `core.state.<verb>` capture included ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-23). The full table of what is in scope, and what an out-of-scope reference now says, is at the end of this section.

```yaml
name: create
compute:
  redis_config: >-
    ${ merge(vars.redis_config,
             vars.persistence_presets[input.persistence],
             default(input.redis_settings, {})) }
tasks:
  - name: record the effective config
    module: core.state.set
    params:
      field: redis_config
      value: "${ compute.redis_config }"    # ← same compute
  - apply:
      destiny: redis
      input:
        config: "${ compute.redis_config }" # ← and here is the same compute
```

**Why.** `apply: input:` (per-host, resolved on the first target `SID`) and a capture's `params:` (keeper-side, §7.1) are **different** CEL contexts, and neither of them **sees** the task-level `vars:` of the other task (`vars:` is the scope of one task, §10). The general expression (big `merge()` - translation of a simple input into `redis.conf`) would have to be written **twice** and synchronized by hand. `compute:` declares it **once** - drift "state ≡ live config" is removed by the mechanism itself, and not by the author's discipline.

**The resolution context is run-level, WITHOUT `soulprint`.** `compute:` is calculated in the context of `input.*` / `vars.*` / `incarnation.*` / `register.*` - **without** `soulprint.self` / `soulprint.hosts`. This is a **structural host invariance barrier**: reference to `soulprint.*` in a compute expression → CEL no-such-key. The consequence is that `compute.<name>` **is the same for all hosts**, so the same value is correctly sent both to `apply: input:` (resolved on the first target host on `SID`) and to a `core.state.<verb>` capture (keeper-side, hostless by construction). Per-host values, as before, are expressed by direct per-host CEL in `params:` / template `.self`, not through `compute:`.

**Declaration order is significant.** `compute[i]` can refer to a previously declared `compute[j]` (j<i) as `${ compute.<name_j> }` (accumulating from left to right). Link forward → no-such-key.

**A keeper-side task reads it too.** Its `params:` (and its task-level `vars:`) resolve in the same run-level soulprint-free context `compute:` itself is resolved in, one phase later — so `compute.<name>` is readable there, with the same value every other reader sees. [ADR-0083](../adr/0083-declared-secret-state-fields.md) §4 relies on it: the `core.state.<verb>` capture step that mints a state field's declared secrets derives the account set from the same compute var the destiny passage is handed, instead of restating the expression.

**Isolation from destiny ([ADR-009](../adr/0009-scenario-dsl.md) V2).** `compute:` - **scenario-entity**: inside the isolated destiny-passage (`apply: { destiny: … }`) it **does not leak**. Destiny only sees the **result** - what the scenario passed through `apply: input:`. Inside destiny `compute.<name>` is rejected as out-of-scope (see the table below). `vars.*` resolves inside a destiny too, but to the destiny's OWN `vars.yml`, not the scenario's ([ADR-0082](../adr/0082-service-vars.md)) — the name survives the boundary, the meaning does not.

**Where the namespace exists, and what a reference outside it says (NIM-619).** `compute:` is resolved in the run-level Keeper context, and it is readable wherever that context is the one being rendered — on the Soul side and on the keeper side alike. A few contexts run *before* the value has a meaning, *beside* it, or on the other side of the wire entirely, and there the name is not merely empty — the namespace is **absent**:

| Context | `compute.<name>` |
|---|---|
| A Soul-side task: `params:` / `where:` / task `vars:` / `apply: input:` | **in scope** |
| A keeper-side task — its `params:` and its own `vars:` | **in scope** |
| A `core.state.<verb>` capture — every param the render interpolates | **in scope** |
| A later `compute[i]` referring to an earlier `compute[j]`, j<i | **in scope** |
| `loop.items:` / `loop.when:` — the host-invariant loop axis | out of scope |
| `on: [covens]` — resolved once per run, before a host is chosen | out of scope |
| The isolated destiny pass | out of scope |
| A capture's `match:` predicate, re-evaluated per element at merge | out of scope |
| `when:` / `changed_when:` / `failed_when:` / `retry.until:` | out of scope |

The `match:` row is the narrow one, and it does not contradict the row above it. `match:` is an ordinary param, so a `${ compute.x }` cell inside it is substituted by the render like any other; what merge then evaluates is the **leftover predicate text**, once per collection element, over `elem`/`key`/`value` and nothing else. A **bare** `compute.x` written as part of the predicate is therefore read at a point where no run context exists.

The last row is the one that surprises, because those four keys sit on a task whose `params:` **do** read `compute.*`. They are not rendered by Keeper at all: they travel into the `RenderedTask` verbatim and Soul evaluates them itself, in the flow-control sandbox ([ADR-012](../adr/0012-keeper-soul-grpc.md)(d)) whose context is `input` / `vars` / `incarnation` / `soulprint.self` / `register` — `compute` is not one of its names, on either side of the wire. The detour is one line, and it is the ordinary one: put the value in the task's own `vars:` (which **is** rendered, with the namespace in scope) and write the predicate against that.

```yaml
- name: scale out
  vars:
    n: "${ compute.node_count }"      # rendered by Keeper — compute is in scope here
  when: vars.n > 1                    # evaluated by Soul — reads the rendered vars
  module: core.exec.run
```

An out-of-scope reference in a **rendered** context fails **at compile**, before any evaluation, and the message names the namespace and the context rather than the key:

```
CEL out of scope "compute.node_count": the compute namespace does not exist in
loop.items:/loop.when: (the host-invariant loop axis) -- the whole namespace is
absent here, not just this name; the loop axis sees input.*/vars.*/register.*/
incarnation.* and soulprint.hosts -- drive the loop from one of those, or build
the list inside loop.items: itself
```

Because the check is at compile it also fires on the forms that used to be **silent**: `has(compute.x)` no longer evaluates to `false` and `size(compute)` no longer returns `0` in a context that has no namespace. The four flow-control keys already refused honestly and still do, in cel-go's own words (`undeclared reference to 'compute'`) — their sandbox never declared the namespace, so there was nothing to correct at the engine; what was missing was anyone saying so **before** the run. `soul-lint` now does, for both halves: `compute_out_of_scope` with the YAML path to edit — and, where the namespace *is* in scope, a name that no `compute:` entry declares (or one declared further down) as `compute_unknown_name`, listing the names that do exist. The offline walk covers the `compute:` block and the task list (`block:` children and a capture's `match:` included); the destiny pass and tasks spliced in by `include:` are left to the run.

**Names.** compute-var name - CEL-field-accessible identifier (letters/numbers/`_`, starts with letter or `_`); hyphen/dot are not allowed (would break `compute.<name>`). The name must not obscure the root context names (`input`/`register`/`incarnation`/`soulprint`/`vars`/`compute`).

> **Boundary `compute:` ↔ `vars:`.** Task-level `vars:` (§10, [destiny/tasks.md §9](../destiny/tasks.md#9-strength-and-control-of-execution)) - local for one task and **visible only in its** `params:`. `compute:` - run-level, visible in `apply: input:` **and** in a capture's `params:`. If the value is needed in both the state write and the destiny transfer, this is `compute:`; if only within `params:` of one task - `vars:`.

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

**Rule context is `input` + `incarnation`, and nothing else** ([NIM-833](../adr/0009-scenario-dsl.md)). Everything decidable from what arrived on the request is decided here, before a single task runs. A reference to `vars.*`/`compute.*`/`soulprint.*`/`register.*`/`vault()`/`now()` is a CEL undeclared-reference error — **a structural barrier by undeclaration**, not a text guard. `vars` are the service's parameters rather than the request, so a rule that needs them is not checking the request; `compute` resolves inside the run, later than this point by construction. Topology/roster checks (`size(soulprint.hosts) == …`) — **not here**, but in `assert:` (§2.3), which has the full scenario context.

**Which incarnation facts a rule may read depends on the path, and the difference is expressed rather than papered over.**

| Path | `incarnation.*` carries | A rule reading anything else |
|---|---|---|
| day-2 run (`POST .../scenarios/{scenario}`) | `id` (+ the `name` window alias), `service`, `service_version`, `state` — a **subset** of what the run itself sees | refused |
| create, operator-supplied id | `id` / `name` only — the incarnation does not exist yet | refused |
| create, scenario with an `id:` block (§2.6) | nothing — the id is composed from `input:` *after* this gate | refused |
| isolated destiny pass, L0 trial case | nothing | refused |

What a path **answers for** does not depend on the row: `incarnation.state` is in scope on every day-2 request, and an incarnation whose state column is NULL gives the ordinary `no such key` the run gives. The alternative — deriving the answerable set from the row — reports the same scenario broken for one incarnation and fine for its sibling, which is a diagnosis about data dressed up as one about the file.

A refusal is a **pre-flight malfunction (5xx)**, not a 422: the operator's input is not what is wrong, the rule is, and the message names the field, the path and what that path does have. The check is a **pre-pass over every rule, before any is evaluated** — inside the loop it would sit behind the first-false short-circuit, and then whether a scenario looks broken would depend on what the operator typed. It reads the predicate's AST, so it fires on every reference position — including `has(incarnation.state)` and `size(incarnation)`, the two forms that would otherwise answer `false` and `0` and let a rule report success over an empty namespace ([NIM-619](../adr/0009-scenario-dsl.md) records the same shape for `compute`). Substituting an empty structure for the absent half is exactly the outcome this prevents: a rule that checks nothing and is believed.

**A create scenario is caught offline.** `soul-lint` judges a `create: true` scenario's rules — its own and any inherited from `covenant.yml` — against the stance a create request will arrive with, and refuses one that can never run (`validate_rule_out_of_scope`, ERROR). Same core as the runtime guard, so the linter cannot come to flag what the keeper allows. Fail-closed and worth knowing: a `create: true` scenario that is *also* run day-2 is still refused a day-2-only rule — split it into a day-2 scenario.

> **The upshot for a create scenario.** A constraint on the identifier — "the id must fit the cloud's VM-name grammar" — belongs in `validate:` on a create scenario that takes the id from the operator: it answers 422 **before** the row is written. A scenario that composes its id from `input:` writes that rule over the `input.*` components instead, because there the id *is* those components.

**The block is part of the service's public contract.** `that` and `message` travel in the scenario listing (`GET /v1/services/{id}/scenarios` → `scenarios[].validate[]`) beside `input_schema`, so an operator form can show the requirements before anything is submitted instead of discovering them as a 422 afterwards. Only the **text** travels: evaluation stays keeper-side, and a client that wants a verdict rather than the requirements asks the server. Two evaluators of one rule diverge — the question is only when. A `validate:` block inherited from `covenant.yml` (§6.1) is merged into the published list covenant-first, the same order the keeper evaluates it in.

> **One rule does NOT travel: one that names a declared secret.** A predicate reads input fields by name and often compares one to a literal, and this endpoint needs only `service.list` — weaker than `incarnation.run`. A rule mentioning a field that was stripped from the published `input_schema` for being a `type: secret` ([ADR-0086](../adr/0086-one-schema-dialect.md) §5) is dropped from the published list, as the matching `form:` field already is: "not on the form" has to be true of all three halves of the reply or it is not true. The rule still **runs** — this is about what is published, not what is enforced.

**When - pre-flight on the request path.** Calculated in the same phase as `required_when` (`scenario.ValidateInput`): AFTER merge defaults / required / type-validation over **merged** input, BEFORE committing incarnation and entering `applying`. Covers both paths - `POST /v1/incarnations` (create) and `POST .../scenarios/{scenario}` (run). Render fail-safe (as in `assert:`) **not needed**: `validate:` is determined from `input.*`, which does not change between the request path and the start of the run (unlike the assert's roster - TOCTOU).

**Rule form.** `that` is a required non-empty CEL-bool string (entire string = CEL without wrapper, like `where:`). `message` - **mandatory** non-empty string (unlike the optional `assert.message`: assert has a task name as fallback, validate rule has no name → 422 would be anonymous). Multiple rules - first `false` wins (short circuit in declaration order).

> **Boundary `validate:` ↔ `required_when` ↔ `assert:`.** Three mechanisms, three different niches:
>
> | Mechanism | Context | What checks | Error code |
> |---|---|---|---|
> | `required_when` (input-schema key, [docs/input.md](../input.md)) | input-only | **presence** fields provided above other input | 422 `validation-failed` |
> | `validate:` (top-level section) | `input` + `incarnation` (what the path knows) | **cross-field invariant** over the request (value relationship, identifier grammar) | 422 `validation-failed` |
> | `assert:` (task discriminator, §2.3) | full (`soulprint.hosts`) | **TOPOLOGY invariant** run (roster) | `error_locked` at render — see below |
>
> `validate:` ADDITIONS, does not replace: "is port required?" → `required_when`; "does quorum exceed the number of sentinels?" → `validate:`; "Is the roster suitable for an N-shard cluster?" → `assert:`.
>
> **Where a failing `assert:` surfaces.** Only `required_when` and `validate:` answer on the request path in every case. A `assert:` that reads the roster is evaluated at RENDER, so its failure is `error_locked` (unlock, then fix and re-run), not a 422 — the create-path pre-flight gate runs BEFORE the incarnation row exists, and since membership FKs that row ([ADR-008 amendment / NIM-124](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)) there is no roster there to measure ([ADR-009 amendment 2026-07-28 / NIM-235](../adr/0009-scenario-dsl.md#amendment-2026-07-28-nim-235-a-roster-reading-assert-has-no-pre-flight-point-at-create)). An `assert:` that reads only `input.`/`vars.`/`incarnation.` still answers 422 `assert-failed` pre-flight on create — but if the check fits input-only, `validate:` is the mechanism designed for it and reports on both the create and the run path.

### 2.6. `id:` — composed incarnation id (create only)

`id:` — **top-level** scenario block (next to `input:`), read **only on the create path** ([ADR-079](../adr/0079-incarnation-name-template.md)): `template:` is a `${ … }` template that composes the incarnation's id from that scenario's own `input:` components instead of the operator typing it as free text, and `max_length:` is the ceiling the composed string must fit.

```yaml
name: create
create: true
id:
  template:   "${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}"
  max_length: 50          # optional; the platform ceiling (63) applies without it
input:
  name:         { type: string, required: true }
  project:      { type: string, required: true }
  subproject:   { type: string, required: true }
  service_type: { type: string, default: sentinel }
tasks: [ ... ]
```

`{name: cache, project: billing, subproject: inv}` → incarnation `cache-billing-inv-redis-sentinel` (`service_type` comes from its schema default — the template renders over the **resolved** input, not over the raw request).

**Why.** In a fleet of similar instances the name is never actually free text: it follows a house convention, retyped by hand every time and drifting every time. The name is `incarnation.id` — a `TEXT PRIMARY KEY`, globally unique and **immutable** (no rename; changing it is destroy + recreate), so a typo is not repairable in place.

**Composition is server-side, before the insert.** Rendered by the Keeper in `scenario.ResolveCreatePlan` — the shared path of `POST /v1/incarnations` and `keeper.incarnation.create` — after the input gate and **before** the pre-flight `assert:`, so the assert and the bootstrap run already see the final name. No new run phase, no DB migration. `compute:` (§2.4) is not a candidate: it resolves RUN-LEVEL, i.e. after the row exists.

**Context is INPUT-ONLY.** Each block compiles against the same narrow cel-go sandbox as `required_when`: a reference to `vars.*`/`incarnation.*`/`soulprint.*`/`register.*`/`vault()`/`now()` is an undeclared-reference compile error. (`validate:` §2.5 also sees `incarnation.*`; `id.template` cannot — it is what composes the identifier.) A name must be a pure function of the request. Unlike ordinary interpolation ([ADR-010 §5(a)](../adr/0010-templating.md)), a single block does **not** yield a native type here — a name is a string, so every block is stringified and concatenated with the literal text; a block evaluating to a list or map is an error.

**`name` in the request becomes optional — and forbidden when a template exists.** Sending both is **422 `id_not_composable`**: not silently ignored (the RBAC `incarnation=<name>` dimension would then be checked against one name while another is inserted) and not an override (that would defeat the convention). With no template in play, nothing changes — `name` is required exactly as before.

**Overflow refuses, it never truncates.** The composed string is checked against the effective ceiling and then against the incarnation id grammar `^[a-z0-9][a-z0-9-]{0,62}$`, before the insert. Four components plus literal text pass 63 characters easily, so this is the failure operators actually hit: **422 `composed_id_invalid`** quoting the composed string, its length and WHICH ceiling it hit, so it is clear both which component to shorten and which file holds the number. A silently truncated name would be a *different* immutable identity.

**`max_length:` is the service's own ceiling, an absolute number, and the SERVICE derives it.** Without it the bound is the platform's 63. A service declares a lower one when something downstream appends to the id and clips the result — the shape of the argument, worked through on the WB redis service, is: its cloud refuses a machine name over 50, and a machine name is the id plus the suffix the provisioning plugin appends (`<id>-<tail>` with a five-character tail, so id + 6), which lands on 44.

The number is absolute rather than a reserve subtracted from 63 because the arithmetic belongs to the service: the limit lives in a plugin and a cloud API the engine cannot see, and `reserve: 9` would have yielded 54 and let a 45-character id through silently. It also **differs per topology** — a clustered machine name carries a group prefix and number as well (id + 7 + prefix + digits, so 41 at 3–9 shards and 40 at 10–99). A single static bound must therefore be the **loosest** topology's, or it rejects legal configurations; the remainder stays with a render-time `assert:`, where the input those summands come from is available. A value outside `1..63` is a soul-lint ERROR (`id_max_length_invalid` / `id_max_length_over_ceiling`) and is treated as unset at runtime — a typo in a bound must not refuse every create.

> ⚠ **No service in any repository declares `max_length` today.** The redis case above is the one that motivated the key and it dissolved: the machine name derives from a `name` PARAMETER the step passes, not from the id, so once that parameter stops carrying the namespace the machine-name limit becomes a cap on ONE input field — which `input:` has always been able to express. Reach for `max_length` only when something the service does not control appends to the id itself; when the constraint can be expressed as a cap on the components, cap those instead.

The bound is enforced **on the request**, by the same `scenario.ComposeID` the preview calls, which is what it buys over a render-time `assert:` on a derived string: the refusal arrives before the incarnation row exists, instead of after it is committed. It does **not** replace such an assert, for two reasons: the input-dependent remainder above, and the fact that `max_length` is consulted only where an id is composed — on **create**. An incarnation created before the bound existed re-runs its scenarios with an id no static key ever measured, so a service whose real limit is a machine name keeps asserting on the machine name.

**Name components are write-once identity.** Composition runs only on create, and `incarnation.spec.input` is written once and never rewritten — a later run passing a different `project` changes what that run does, not what the incarnation is called. Nothing renames an incarnation.

**The key was the scalar `id_template:` until NIM-899**, and both spellings are read for a compatibility window: a service repository still on the scalar loads, composes and runs exactly as before (with no ceiling of its own — a scalar has nowhere to put one), and `soul-lint` warns (`id_template_legacy_spelling`) with the line and the replacement. Declaring **both** in one file is an error (`id_template_conflict`) — two templates composing one id is an authoring mistake whichever value a loader picked. Removing the scalar is a separate ticket, after service repositories have moved.

**`name_template:` — the pre-[ADR-0085](../adr/0085-entity-id-and-label.md) spelling — is no longer read at all.** Its window closed with NIM-899 rather than surviving beside the block: it had zero occurrences in every service repository and in `examples/`, so keeping it open would have left three spellings of one key. It answers `unknown_key` with the replacement in the hint.

**soul-lint** catches the class offline: `id_template_input_unknown` (ERROR — `${input.X}` with X not declared in `input:`; it would fail for every operator), `id_template_invalid` (ERROR — a block outside the input sandbox, or the index form `input['x']`, which hides the component name from static analysis), `id_template_too_long` (ERROR — the literal skeleton alone exceeds the **effective** ceiling, so declaring `max_length: 40` sharpens this rule from 63 down to 40), `id_max_length_over_ceiling` (ERROR — a bound above 63 narrows nothing while reading as though it did), `id_max_length_invalid` (ERROR — a bound below 1 bounds no id), `empty_value` (ERROR — an `id:` block with no `template:`, which is a ceiling on nothing), `id_template_constant` (WARNING — no block at all, so every incarnation composes to the same id) and `id_template_ignored` (WARNING — the scenario is not a `create: true` starter, so the key is dead config). Under `extends:` (§6.1) the whole set runs **post-merge**, exactly like `form:` — before the covenant merge a component declared in the fragment would look undeclared.

> **Known limitation (RBAC).** `POST /v1/incarnations` scopes its permission check from the request body before the handler runs, so a request with no `name` yields an empty context set — which admits only bare/`*` roles (MCP behaves the same way). A **scoped** operator therefore cannot create a templated incarnation yet. Fail-closed, so it can only refuse, never over-grant; scoping a create whose name is unknown until the service snapshot resolves needs its own decision.

## 3. Step target - `on:`

`on:` - **stable place** for step execution. Resolved by Postgres (stable layer: the incarnation **membership** relation `incarnation_membership` and the hosts' stable covens). Resolution point - **start of each Passage** ([ADR-056](../adr/0056-staged-render-passage.md)); the omitted `on:` target is stable within the Passage, but at the refresh boundary (`refresh_soulprint: true`) it will be re-resolved according to the updated roster - see §4.1 "Stability of the roster run" ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)). Three forms:

| Form | Semantics |
|---|---|
| **omitted** | entire incarnation: all **member** hosts, resolved via the membership relation `incarnation_membership` (NOT via a coven). `incarnation.id` is not a Coven — `on: ["${ incarnation.id }"]` is a **validation error** (steering to the omitted form), see [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation) |
| `on: [coven-a, coven-b]` | intersection (AND) of the listed **stable** covens; result **always ⊆ members** (the roster is already membership-scoped) |

### The side is the module's, not the task's

[ADR-0087](../adr/0087-task-side-derived-from-module-address.md), implemented in NIM-749.

**A keeper-side step is not written with `on:` at all.** The side follows from the **module
address**: the core module sets are disjoint — `core.bootstrap` / `core.cert` / `core.choir` /
`core.soul` / `core.ssh` / `core.state` / `core.vault` are executed by the Keeper, every other core
address by a Soul on each host — so the address decides on its own, and an author writing
`on: keeper` was telling the engine something it already knew. On a core keeper-side address the key
is now an **error** (`on_keeper_redundant`).

The keeper-side list above is the catalog in `shared/coremanifest/side.go`
(`coremanifest.KeeperSideAddrs`), which is also what the diagnostic's own hint prints. An address
that leaves it does not become "a module that needs `on: keeper`" — it becomes **Soul-side**, which
is what an unknown address has always been, and a plan that relied on it being keeper-side then
aborts `no_hosts` on the empty roster it was written to run on (NIM-863).

That leaves `on:` with one meaning, the one this section is about: **which covens**. It used to carry
two, a list of labels and a magic scalar meaning "no hosts at all", in the same key. A coven list on a
keeper-side address is refused too (`on_covens_on_keeper_module`) — it selects hosts for a step that
has none, and render never reads it, so the labels would vanish in silence. And the literal on a
**Soul-side** core address is refused from the other direction (`on_keeper_on_soul_module`): render
honours it whatever the address, so it would send a host module to the keeper, which has no such
module.

**A plugin address is the exception, permanently so far.** `on: keeper` on `<alias>.<module>.<state>`
is still accepted, and still required. The Keeper does execute such a plugin now — NIM-758 routes a
keeper-side address the core registry does not know to a module whose schema document declares
`side: keeper` ([keeper/modules.md → keeper-side plugin modules](../keeper/modules.md#keeper-side-plugin-modules)) —
but the declaration lives in the artifact, in the Keeper's plugin cache, and a scenario cannot read
it. Nothing a service repo contains says the side of a plugin address, so the key stays the only
spelling: dropping it here would silently send the step to hosts, which is the failure mode this
whole section exists to prevent.

```yaml
# Entire incarnation (on: omitted)
- name: Apply base config everywhere
  apply: { destiny: redis-base, input: { ... } }

# Keeper-side core: no `on:` — `core.soul` is a keeper-side module, so the address routes it
- name: Register the new hosts
  module: core.soul.registered
  params: { ... }

# Keeper-side PLUGIN: `on: keeper` is required — nothing in this repo declares a plugin's side
- name: Provision VMs
  module: democloud.vm.created
  on: keeper
  params: { ... }

# Intersection of stable covens, ⊆ members
- name: Tune kernel on bare-metal hosts of this cluster
  on: [baremetal]                  # incarnation scope is implicit (roster is membership-scoped)
  apply: { destiny: kernel-tuning, input: { ... } }
```

**Resolver contract `on:` (invariant):**

1. **Membership, not a name-coven.** The omitted `on:` = all **member** hosts of the incarnation, resolved via the membership relation `incarnation_membership` (NOT via `= ANY(coven)`). `incarnation.id` is **not** a Coven and is not a valid `on:` value: `on: ["${ incarnation.id }"]` is a **validation error** with a message steering to the omitted form (fail-closed — a stale scenario errors out instead of resolving to an empty set). See [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation).
2. List in `on:` - **AND/intersection** of **stable** covens. Additional stable covens (e.g. `baremetal`, `dc-eu`, `prod`) narrow the set, but **cannot expand it beyond the incarnation members**.
3. **Cross-incarnation targeting is prohibited by construction.** The `on:` resolver cannot return a host that is not a member of the current incarnation, given any set of covens — the roster is membership-scoped, so a stable coven can only intersect it, never reach outside it. This is a security invariant (see [ADR-008](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)), now enforced by the membership join rather than by the name-coven.
4. Role (master / replica) **never Coven** and does not participate in `on:`. The volatile role is expressed only through `where:` (see §4).

**Flow control on a keeper-side task is not the Soul-side one.** The keeper's own runner walks its tasks in plan order and evaluates no predicate, so `async:` is refused (`async_on_keeper_invalid`), `loop:`/`apply:`/`block:` are refused, `require:` is accepted-and-redundant, and a `when:` reading `register.*`/`soulprint.*` is refused (`when_on_keeper_dynamic_unsupported`, [ADR-0084](../adr/0084-explicit-state-capture.md) F-D) - it would be accepted and dropped, and the step that runs regardless of its condition is the one that writes incarnation state (§7). A **static** `when:` stays the working form: the keeper settles it at render, before the task is routed keeper-side. The full key-by-key answer with replacements - [keeper/modules.md](../keeper/modules.md).

These four are judged by the task's **side**, i.e. by its module address — not by a written
`on: keeper`. That is a real widening rather than a restatement: while the checks keyed on the
literal, a dynamic `when:` on a capture written without it went through untouched, so the file said
the step was conditional and the step wrote state every time.

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

> `soulprint.where(<predicate>)` accepts a CEL predicate **static string literal** ([templating.md §2.3](../templating.md)); keyword style (`coven=...`) is not used. Inside the predicate, the element fields (`covens`/`os.*`/`sid`) and the external context (`incarnation.*`, etc.) are available. **All members of the run** are simply `soulprint.hosts` (the accessor is already incarnation-scoped) — there is **no** "by incarnation coven" predicate: `incarnation.id` is not a Coven and never appears in `covens`, so `soulprint.where("incarnation.id in covens")` is removed ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). A stable-coven filter "by literal coven X" is `soulprint.where("'<X>' in covens")` — **without** dynamic string concatenation (`"'" + some_var + "' in covens"` - prohibited, the predicate is expanded in the compile phase, see [templating.md §2.3](../templating.md)). Deep nesting of quotes is a well-known footgun, see [templating.md §8](../templating.md): the recommendation is to place such expressions in the `vars:` step.

> `where:`-key - position "on which hosts". `soulprint.where(<predicate>)` - position "where to get the value from". They are independent; confusing them is a reading error, not an alternative. (Since Soulprint after [ADR-008](../adr/0008-coven-stable-tags.md) stores only stable facts, `soulprint.where(...)` operates with a stable layer; the volatile role is exclusively through probe + `where:`-key.)

> `soulprint.self.*` in `where:` - own stable facts
> candidate host (per-host, stable layer), symmetrically
> `register.self.*`; not to be confused with `soulprint.where(...)` (cross-host
> lookup) and `soulprint.hosts` (list of all run hosts).

### Migration: `filter:` withdrawn

In previous examples, the key `filter:` was used to select hosts. **`filter:` was completely removed** and replaced with `where:`. Any occurrence of `filter:` in scenario is a validation error; when rewriting examples `filter: <predicate>` → `where: <predicate>`. Previous convention of sub-covens `{incarnation.id}-{role}` (for example, `coven: {{ incarnation.id }}-master`) - removed ([ADR-008](../adr/0008-coven-stable-tags.md)); instead probe + `where:`.

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
| `role` | string | **declared-role** — the `role` of this host's Choir Voice (`incarnation_choir_voices.role`, `master`/`replica`/…), and since [ADR-044 amendment 2026-07-30](../adr/0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role) (NIM-330) that is its ONLY source — the former `incarnation.spec.hosts[].role` fallback is gone with the field. This is **declared, NOT actual**: the actual role is volatile and is taken only by a live probe + `where:` key ([ADR-008](../adr/0008-coven-stable-tags.md)). On `create` (redis is not running yet, probe has nothing to poll) declared is the only topology source, and the create scenario lays it down itself with a `core.choir.present` step (`on: keeper`) before any task reads it. The field is **empty/`null`** for a host no Voice places into a part (for example the `add_replica` branch: an existing Soul is bound to the incarnation as a member, but nobody gave it a Voice) — "no declared role", NOT a default group. The declared role reflects **intent** and is not auto-filled by the fact of binding; the actual role of such a host is recorded in `incarnation.state` by a `core.state.<verb>` capture step (§7.1), not in `soulprint.hosts[].role`. |
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
master discovery instead of sub-coven `{incarnation.id}-master`" (see §8).

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
a machine-provider plugin step (`<alias>.vm.created`, `on: keeper` — the only
spelling a plugin's side has) creates N VM → `core.soul.registered`
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
  module: core.cmd.shell                                       # on: omitted = all member hosts
  register: redis_role
  changed_when: false                                          # probe state does not change
  params:
    cmd: "redis-cli role | head -1"                            # a pipeline -> shell, not argv
```

**A probe that exits non-zero fails its host by default.** The probe step is no different from the usual one: without `failed_when:` the host's status is whatever the module reported, and the verb modules judge that by their `exit_codes` param, which defaults to `[0]` ([destiny/tasks.md](../destiny/tasks.md), `failed_when:`). For a probe that answers by exit code — `grep -q`, `test`, `systemctl is-active` — the accepted codes belong on the task:

```yaml
params:
  cmd: "systemctl is-active redis-server"
  exit_codes: [0, 3]                                           # 3 = inactive, still an answer
```

`failed_when:` on `register.self.*` still has the last word over both, and `failed_when: false` tolerates any code. The probe's own result carries `register.self.stdout` / `.stderr` / `.exit_code` — there is **no** `.rc` field, and referencing one is a CEL `no such key` that fails the task. Those fields are present even when the probe failed on its exit code, so a predicate over them is always evaluable. Once the probe does fail the host, **standard step fall handling** from the DSL core works (`retry:` / `onfail:` / script stop / `error_locked`).

> Mind the pipeline: `sh -c` reports the exit code of the **last** command, so `redis-cli role | head -1` stays 0 even when `redis-cli` cannot connect. An exit-code check does not cover that — a probe whose answer must be non-empty asserts on `register.self.stdout`, as the `retry: until:` idiom below does.

> **"Probe completeness" is NOT expressed in a manual idiom.** The previous spec example carried `failed_when: size(register.redis_role) < incarnation.host_count` ("fail if not all hosts responded to the probe"). This idiom is **removed** - it is physically unexecutable: `failed_when:` is calculated by Soul-side per-host and sees only `register:` previous tasks + `register.self.*`; **own** aggregate `register.<this-probe>` by name and cross-host `size(...)` to it **not available** → CEL `no such key`. The completeness of the probe does not need manual verification - protection from destructive operations on an incomplete probe is provided by the fail-stop staged-render barrier (see footgun below).

Error handling in scenario - **only mechanisms inherited from the DSL core**: `retry:`, `onfail:`, `failed_when:`, `onchanges:` ([destiny/tasks.md §8, §9](../destiny/tasks.md#8-requisites---inter-task-dependencies)). **No new attribute is entered.**

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

**`../` is not allowed in the syntax.** The script writer **never writes** relative paths with `../`. Fallback to service-level is done by the **engine**, not the author: the author refers to the resource by name (`template: templates/redis.conf.tmpl` in the step `core.file.rendered`, `include: replication.yml`), the engine searches first locally, then at the service-level. **The resolved path is printed to the apply log** and checked by `soul-lint` (see [soul-lint.md](../soul-lint.md)).

**One subdirectory level in `include:`** ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-08-17). The target is `<file>.yml` or `<dir>/<file>.yml` — **at most one** directory deep, each segment matching `[a-z_][a-z0-9_-]*`. Both forms go through the same two levels, so `include: _create/provision.yml` written in `scenario/create/main.yml` finds `scenario/create/_create/provision.yml` first and `scenario/_create/provision.yml` second — the shared-bodies directory of §1 is reached by the ordinary service-level fallback, not by a new mechanism. `.` is outside the segment alphabet on purpose: `..`, absolute paths and hidden names **cannot be written at all**, so traversal is refused by the grammar before any resolve, and securejoin inside the loader is the second line of defence. Two levels (`a/b/c.yml`) is a validation error — the cap keeps the include namespace shallow enough to read at a glance. The clamp is applied at the **service root**, both by the keeper against the materialised snapshot and by `soul-lint` offline, so both sides read the same file for the same name. What the clamp judges is the symlink target as **written**, not where it eventually lands: a target that never expresses an escape resolves normally (`scenario/create/deploy.yml -> ../../shared_bodies/deploy.yml` is fine), while a leading `..` run that would step above the service root is truncated **at** the root before the rest of the path is appended. So a target that walks out of the repo and back in again (`../../../<repo>/shared`) is refused even though its destination is inside the service — it is re-rooted, not followed.

**The two levels are FIXED directories, not "relative to the including file".** Resolution always tries `scenario/<name>/` and then `scenario/`, whichever file the `include:` was written in. So inside a shared body `scenario/_create/provision.yml`, a plain `include: sentinel.yml` does **not** find its own neighbour `scenario/_create/sentinel.yml` — the two levels tried are `scenario/<name>/sentinel.yml` and `scenario/sentinel.yml`. A shared body addresses its siblings the same way everyone else does, by the full `include: _create/sentinel.yml`. This keeps an include target's meaning independent of which file spliced it, so a body moved between directories resolves identically; the failure names both attempted paths, so the mistake is one error message from being fixed.

**The same two directories serve an upgrade scenario** ([ADR-0068](../adr/0068-service-upgrade-v2.md) §3, the second auto-discovery channel). `upgrade/<slug>/main.yml` is loaded from its own channel, but its `include:` still resolves `scenario/<slug>/` then `scenario/` — the levels are keyed on the scenario NAME, and the resolver is never told which channel the entry point came from. `templates/` behaves the same way, for the same reason, so the two agree. **A body placed next to an upgrade scenario is therefore unreachable**: `upgrade/<slug>/install.yml` is at neither level. Shared bodies for an upgrade live where every other body lives, under `scenario/` or `scenario/_<family>/`. `soul-lint` reports the mistake offline, naming both attempted paths (NIM-753).

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

**Conditional include (`when:` on the include task) - render-phase group-drop ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-24).** On the include task, allow `when:` - then the connected group is included in the plan only if the predicate is true; if false - ALL tasks of the connected file are **physically absent** in the plan (real exception, not placeholder: not issued, index not reserved). The predicate must be **static** (`input.*`/`vars.*`/`incarnation.*`) because include is expanded **before** stratification when `register:` is not yet collected and per-host `soulprint` is unknown; dynamic include-when (`register.*`/`soulprint.*`) → `include_when_dynamic_unsupported` (catches both expansion and `soul-lint` offline). Full semantics - [destiny/tasks.md §4](../destiny/tasks.md#4-basic-blocks) (scenario is inherited as is).

> **This is an override of the rule `include:` from [destiny/tasks.md §4](../destiny/tasks.md#4-basic-blocks).** In destiny `include:` stays inside the folder `tasks/` (one subdirectory level is allowed since 2026-08-17, but there is only ONE tier); going beyond `tasks/` is prohibited. In the scenario, the rule is **different**: `include:` (and resolve `templates/`/`vars.yml`/`tests/`) two-level - locally, then service-level, fallback is done by the engine. The **grammar of the target is shared** (`<file>.yml` or `<dir>/<file>.yml`, one level); what differs is where it is resolved. `tasks.md §4` **does not change** in kind - the behavior for destiny is described there; the difference in scenario is recorded here.

> **Synthesized tasks `core.module.installed`.** Immediately after expanding `include:` (before stratification), Keeper inserts install steps of custom modules from `service.yml::modules[]` into the flat plan - before the first consumer task of each module, with the marker name `install <alias> (service manifest)`. These are regular tasks of the plan (render → dispatch → TaskEvent), in run-view they are visible like the rest. Synthesis mechanics (position, takeover by explicit step, MVP restrictions) - [keeper/modules.md → Auto-synthesis](../keeper/modules.md), [ADR-065](../adr/0065-core-module-installed.md).

### Script tests

Layout: `scenario/<name>/tests/<case>/case.yml`. Format `case.yml` - `verify:` / `expect:` - **reused from [destiny/testing.md](../destiny/testing.md)** (there is no separate DSL assertions, same approach). Delta scenario:

- **L0 render-multi-host is executed by the standard harness.** Roster of run hosts is set by `fixtures.hosts: [...]` (host record `sid`/`covens`/`role`/`soulprint`/`choirs`, format - [destiny/testing.md](../destiny/testing.md)). Render invariants on the topology (`size(soulprint.hosts)`-guard, `soulprint.hosts.where(...)`-projection, nodes-determinism master/replica, `core.state.<verb>` capture on multi-host) are driven hermetically `soul-trial run`, without docker and without dispatch (amendment 2026-06-22, [ADR-023](../adr/0023-trial-test-runner.md)).
- **L3-dispatch remains stub.** Docker `stand:` on the cluster topology, assertions "who really executes the master" (`assert.dispatch`) and committed cross-host `incarnation.state` after the barrier (§7) are postponed. The exact format of the multi-host block `stand:` inherits from open Q sandbox from [destiny/testing.md](../destiny/testing.md) (see §8 below).

> This is an **open Q extension about sandbox** from [destiny/testing.md](../destiny/testing.md): **L0 render-multi-host (`fixtures.hosts`) - closed** and executed by a standard harness (sealed render level). **L3-dispatch** (multi-host docker stand, `assert.dispatch`, committed cross-host `state`) is not a closed solution, an explicitly marked extension of an open issue. Does not close silently; before solving L3-dispatch - declarative-stub `stand:`, as in destiny-molecule.

## 6.1. `extends:` - inheritance of the general section contract from `covenant.yml`

`extends: <covenant-name>` - **top-level** script key: the script **inherits** the general service-level contract of sections `input:` / `compute:` / `validate:` from the covenant fragment file in the **root service repo** ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-06-29). Covenant - **service-level shared catalog**, isomorphic to `types.yml` ([ADR-062](../adr/0062-input-types.md), named input schemas) and service-level `include:` (§6, task sets): three mechanisms fumble between scenarios of different nature (type / tasks / contract sections), all resolve BEFORE consumers and do not introduce a wire entity.

**`extends:` NAMES the covenant file.** The meaning of `extends:` is **name of the covenant file without extension**: `extends: <name>` resolves to the file **`<name>.yml`** in the root of the service repo (the mechanism supports an arbitrary name, symmetrically to how `apply: { destiny: <name> }` addresses destiny by name). **Convention:** the general contract sections of the service are called **`covenant`** → file **`covenant.yml`**, link **`extends: covenant`**. This convention is used in all the examples below.

**One subdirectory level in the name** ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-08-17). `extends: <name>` and `extends: <dir>/<name>` are both valid — at most one directory deep, each segment matching `[a-z_][a-z0-9_-]*`, resolved relative to the **service root** (`extends: shared/scenario_create` → `shared/scenario_create.yml`). A service with several covenants (several families of scenarios, see the convention note below) can therefore keep them in `shared/` instead of scattering them across the repo root. Resolution stays clamped to the service root by securejoin, and `.` is outside the segment alphabet, so `..`, absolute paths and hidden names are unrepresentable in the grammar to begin with.

```yaml
# covenant.yml (service-repo root) - TOTAL minimum
input:
  redis_type: { type: string, default: standalone, enum: [standalone, sentinel, cluster] }
  password:   { type: string, vault_scope: secret }
compute:
  redis_config: "${ merge(vars.redis_config, default(input.redis_settings, {})) }"
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

**Add-only merge, fail-closed.** Covenant - **minimum** (common base); the script **ADDS** its delta. Merging - **shallow by top key** sections (input field / compute name / list element `validate`): top keys are merged, there is no recursive merging of values. Double top key (one input field in both covenant and script; one compute name) → error `section_key_conflict` (**NOT last-wins, NOT override**). The intent "base = general, scenario = incremental only" is made explicit; collision = author's error, not silent erasure. **Deep merge rejected** (would hide which part of the schema is from the covenant - the same "either link or inline" principle from [ADR-062](../adr/0062-input-types.md)).

**`form:` DOES NOT merge.** UI-`form:`-block ([ADR-045](../adr/0045-param-dsl.md)) from covenant **not inherited** - this is a form layout for a specific operation, local-only. Covenant carries a data contract, not a presentation.

**Append order is covenant FIRST.** In ordered sections (`compute:` §2.4, `validate:` §2.5) covenant elements come **before** local ones: local `compute[i]` can refer to covenant-`compute[j]` (`compute` resolves from left to right, §2.4), local `validate` - relies on what has already been applied from the covenant. The reverse order would break the forward-reference covenant→local.

**The resolution point is BEFORE the consumers.** The merge is performed at a single resolution point of the manifest (isomorphic to `$type`-resolution [ADR-062](../adr/0062-input-types.md) and `include`-resolution §6): load `covenant.yml` → merge 3 add-only sections (fail-closed on a conflict) → then a **regular** pipeline on an ALREADY-merged manifest (input-merge → required → render → dispatch). All consumers (`ValidateInput`, render, `assert:`/`validate:`-eval, soul-lint) see a complete manifest - **fragment-aware of no code outside the resolution point**.

**MVP restrictions.**

- **One `extends:` per script** (not a list of covenants).
- **covenant does NOT extend-it covenant** (flat sheet, no recursion/chaining → cycle-detection is not needed on this layer, unlike `$type`).
- **covenant carries ONLY 3 sections** - `input:` / `compute:` / `validate:`. **NOT** `tasks:` / `name:` / `create:` / `form:` / `extends:` (a foreign top key in the covenant file → `covenant_unexpected_key`). Tasks (`tasks:`) are divided through service-level `include:` (§6), not through covenant.
- **A covenant does NOT carry state writes.** `state_changes:` was the fourth section until
  [ADR-0084](../adr/0084-explicit-state-capture.md) removed it from the grammar, and it does **not**
  come back as a task list: a capture is a step with a position, and a section that dropped its ops
  at the end of the run had no position to inherit. The shared write moves into the scenarios as a
  `core.state.<verb>` step, shared the way steps are shared — through service-level `include:` (§6).
  A covenant carrying `state_changes:` is rejected like any other foreign key.

> **Convention: one `covenant.yml` per service (recommendation).** The mechanism supports several covenant files (`extends: <name>` → `<name>.yml`), but **it is recommended** to keep one common contract section of the service in the file `covenant.yml` (`extends: covenant`). Several covenant files are justified only when the service has several unrelated families of scripts with different common contracts - for a typical service this is unnecessary fragmentation.

**Forward-compat.** Key `extends:` **optional**; script without `extends:` - manifest **bit-for-bit as it is now** (resolution point in the absence of `extends:` - no-op). The service without `covenant.yml` does not break.

> **Boundary `extends:` (covenant) ↔ service-level `include:` (§6) ↔ `types.yml` (`$type`).** Three service-level shared mechanisms, three different niches: `covenant.yml` (+`extends:`) fumbles ** contract sections** run (`input`/`compute`/`validate`); service-level `include:` (two-level resolve, §6) fumbles **task sets** (`tasks`-fragments); `types.yml` (+`$type`) searches for **named input circuits**. Not to be confused: the common **set of steps** of the deployment (`cluster.yml`/`sentinel.yml`) is `include:`, the common **input contract** between scripts is `extends:`. A shared **state write** is a step, so it goes through `include:` as well ([ADR-0084](../adr/0084-explicit-state-capture.md) F-B).

## 7. State capture and the cross-host barrier

`incarnation.state` is written by **capture steps** — ordinary keeper-side tasks addressed
`core.state.<verb>` — and every write lands **at its step**, not at the end of the run
([ADR-0084](../adr/0084-explicit-state-capture.md)). The end-of-run `state_changes:` section that
used to hold all of a scenario's writes is **removed from the grammar**: a scenario still carrying
one is rejected (`unknown_key`), never ignored — silently dropping the block would stop the writes
it describes with nothing saying so.

**A capture carries no `on:` key.** `core.state` lives on the keeper and nowhere else, so its
address routes the step (§3) and there is nothing left for the author to declare; writing
`on: keeper` on it is an error (`on_keeper_redundant`). The rule this replaced was the mirror image
— `state_capture_not_on_keeper`, which refused a capture that did NOT carry the key — and it existed
only to make the author restate what the address already said.

```yaml
- name: Record the namespace
  module: core.state.set
  params: { field: namespace, value: "${ input.namespace }" }
```

**That rule reversed with [ADR-0087](../adr/0087-task-side-derived-from-module-address.md)**
(shipped in NIM-747/NIM-749, see §"The side is the module's, not the task's"): the side is derived
from the address, so writing `on: keeper` on a capture is now the error (`on_keeper_redundant`) and
`state_capture_not_on_keeper` is retired. The form above — no `on:` at all — is what ships.

The **cross-host barrier is unchanged** and still unconditional:

1. Each Passage waits for **all** async tasks of **all** its run hosts before the next Passage
   starts (final-barrier extension from [destiny/tasks.md §6](../destiny/tasks.md#6-asynchronous-tasks-async-true)
   from one host to the cross-host scenario level; per-host that final barrier is the end of the
   host's `ApplyRequest`, i.e. the end of its current Passage,
   [ADR-0075](../adr/0075-intra-host-async-tasks.md)).
2. A capture is a keeper task, so it lands in a keeper Passage and is subject to that same barrier:
   it starts only after the previous Passage has joined on every host, and a host task after it
   starts only once the capture has committed.
3. A finally-failed task stops the run and moves the incarnation to `status: error_locked`
   ([architecture.md → "Incarnation"](../architecture.md)).

**What moved is the commit, not the barrier.** A run that dies half-way now leaves state describing
exactly what was captured **before** it died, rather than nothing at all. That is the point of the
change: `provisioned_vm_ids` captured immediately after provisioning survives a failure three tasks
later, so a day-2 cascade-destroy can still find the VMs it has to reap. The old all-or-nothing
commit lost them and left real cloud instances that nothing in Postgres pointed at.

**`error_locked` is incarnation-scoped, not host-scoped**, and that is what makes a partial commit
safe. The lock freezes further scenarios against the incarnation; ad-hoc `command` / `run` against a
host stays available by construction (neither `keeper/internal/errand` nor `keeper/internal/console`
reads the status). Partial state + a frozen incarnation + a reachable host is the triple an operator
repairs from.

**`state_history` is a snapshot per capture**, not per run: every capture opens its own transaction,
re-reads the row `FOR UPDATE`, mutates, appends history, commits.

### 7.1. The capture verbs

The **verb is the module's state suffix**, exactly as for the rest of keeper-side core
(`core.choir.present` / `core.choir.absent`), and the field the operation acts on is the `field:`
param. The verb dictionary is [ADR-057](../adr/0057-state-changes-crud-verbs.md); moving it onto the
module address is [ADR-0084](../adr/0084-explicit-state-capture.md).

| Address | Params beyond `field:` | Semantics |
|---|---|---|
| `core.state.set` | `value:` | overwrite `incarnation.state.<field>` whole. |
| `core.state.present` | `value:` | write only if the field holds no value; an existing one wins. |
| `core.state.add` | `value:` + opt. `key:` / `match:` / `on_conflict:` | insert an element **by identity** (`key:` for a map, `match:` for a list). |
| `core.state.append` | `value:` | append to a list with **no** identity check. |
| `core.state.modify` | `patch:` + opt. `match:` / `expect:` | patch **every** element matching `match:`. |
| `core.state.remove` | opt. `match:` / `expect:` | delete **every** element matching `match:`. |
| `core.state.unset` | — | delete the whole field. |

An address the table does not list is **refused**, and so is a param the verb does not take: a
`patch:` handed to a `set` is an authoring mistake, and accepting it silently would write the field
without the change the author asked for.

A scenario that writes nothing simply carries no capture step — the former `state_changes: []` has
no successor and needs none.

**Collection shape comes from `state_schema`** (`service.yml`). An `add` / `append` to a field with
no value yet materialises the empty map/list the schema declares.

**A capture reports what it wrote.** With `register: <name>` on the step,
`register.<name>.effective` carries the field's state **after** the write, `.field` echoes the field
name, and `.generated` lists the Vault paths this run minted
([ADR-0083](../adr/0083-declared-secret-state-fields.md) §4). Reading `.effective` is the normal way
for a later step to see what a capture produced — see "Reading state while writing it" below.

#### CEL context of a capture

`value:` / `patch:` / `match:` / `key:` are ordinary task params, so they render in the ordinary
`params:` CEL environment of a keeper-side task ([ADR-010](../adr/0010-templating.md), marker
`${ … }`); a literal without `${ … }` is taken as-is, and a cell that is exactly one `${…}` keeps
its native type.

Available: `input.*` / `incarnation.*` (incl. `incarnation.state.*`) / `vars.*` / `compute.*` (§2.4)
/ `register.*` (keeper-side only, see below). **Not** available: `soulprint.self.*` and
`soulprint.hosts` — a keeper task has no host. This is the same context every other keeper-side step
renders in, not a second one.

On top of it, `match:` and `patch:` of the read-modify-write verbs bind the **current element** of
the collection:

| Binding | Semantics |
|---|---|
| `elem` | the current element of a list collection (a scalar for a list of scalars). |
| `key` / `value` | key and value of the current map entry. |

The name `elem` (not `self`) was chosen to avoid the collision with per-host `soulprint.self`. These
bindings are evaluated by the module at merge time, against the state as of that step.

#### `set` / `present` — writing a whole field

```yaml
- name: record the version we deployed
  module: core.state.set
  params:
    field: redis_version
    value: "${ input.version }"

- name: pin the master this cluster was built around
  module: core.state.present       # a later run must not move it
  params:
    field: origin_master
    value: "${ compute.master_sid }"
```

`present` discards the proposal when the field already holds a value and resolves the **stored**
value instead — including for a field declared `type: secret`, where nothing is minted for a write
that was thrown away (a minted-then-discarded credential would be a live secret in Vault that
nothing in state points at).

That yielding is also what makes `present` the one verb that can **fail closed** on a secret. The
stored element carries no `generate_secret({…})` request — declared secrets are stripped on the way
into state — so if the derived Vault path is empty, the step has nothing to keep and nothing it is
allowed to mint, and it refuses ("is stored but was never minted in Vault"). Under every other verb
the same empty path refuses too, unless the proposed value carries a request. **A missing value is
never minted on its own**; see [ADR-0083](../adr/0083-declared-secret-state-fields.md), amendment
2026-09-02, for the three outcomes and the cites.

#### `add` / `append` — growing a collection

```yaml
- name: record the new replica
  module: core.state.add
  params:
    field: redis_hosts             # list
    value: "${ vars.new_sid }"
    match: "elem == vars.new_sid"  # identity predicate
    on_conflict: skip              # DEFAULT: a repeat run does not duplicate

- name: record the new user
  module: core.state.add
  params:
    field: redis_users             # map
    key: "${ input.username }"
    value:
      acl:   "${ input.acl }"
      state: "on"
    on_conflict: error             # a double create is an outright error
```

`on_conflict: skip` (default) `| replace | error` — what happens when the map key is taken or the
list `match:` already finds an element. `append` is the deliberate no-identity form: it grows the
list unconditionally and is therefore **not** idempotent, which is what an append-only log wants and
what a roster does not.

**Identity belongs to the collection kind, and the two spellings are not interchangeable.** `key:`
names an element of a **map** field; `match:` names an element of a **list** field. Which of the two
the engine reads is decided by the field's kind in `state_schema`, not by which one the author
wrote, so the wrong spelling is an **error**, not a fallback — a `key:` on a list would otherwise
drop through to whole-element deep equality, and a re-run with the same key and one changed property
would append a second element instead of hitting `on_conflict:`. Writing **both** on one task is
refused for the same reason: one of them would be silently ignored.

#### `modify` / `remove` — patch and delete by predicate

`match:` is a CEL predicate over the element; **every** matching element is patched or deleted
(all-by-default). Multiplicity is a property of the predicate, not a flag.

```yaml
- name: apply the new ACL
  module: core.state.modify
  params:
    field: redis_users
    match: "key == input.username"
    patch:
      acl:   "${ input.acl }"
      state: "${ input.state }"

- name: retire the host
  module: core.state.remove
  params:
    field: redis_hosts
    match: "elem == input.sid"
    expect: one                    # runtime multiplicity assertion
```

**`expect: one | at_most_one | any`** (default `any`) — an optional runtime assertion on how many
elements `match:` selected, checked **before** mutating. A mismatch fails the task, so the capture
does not commit.

**Empty match → no-op** (idempotent) for `modify` and `remove` alike: a predicate that catches
nothing quietly does nothing, and is not an error. An **absent** `match:` is the same case, not the
opposite one — it matches **nothing**, so the step is a no-op and a forgotten line demolishes
nothing. "Every element" is written out, as `match: "true"` (and takes the wide-match WARN below).
`add` reads an absent `match:` differently: there it means deep equality of the whole element, since
the predicate is an *identity*, not a selection — see the per-kind rule under `add`.

**Wide-match fuse.** `soul-lint` emits a **WARN** (`state_wide_match`) for a `modify` / `remove`
whose `match:` is absent, empty or a constant `true` — the form that repatches or demolishes the
**whole** collection. A warning and not an error, because the wide form is legitimate ("clear every
user"); it is just far more often a predicate the author meant to narrow. A `match:` carrying an
interpolation is **not** reported: its value is decided at render, and warning on every `${ … }`
predicate would make the rule noise. The check runs in the stage pass, after `include:` expansion,
because a capture routinely arrives through an include and the per-file task rules never see it.

> **There is no `foreach`.** [ADR-057](../adr/0057-state-changes-crud-verbs.md)'s structural
> `foreach` was a render-time expander of the removed block and does not become a module state:
> iteration over a step is the DSL's own `loop:`. ⚠ **But `loop:` does not reach a keeper-side
> task today** — `renderKeeperTask` rejects it alongside `apply:` and `async:`, a pilot restriction
> older than this change. Until that lifts, a capture over a runtime-sized collection has to be
> written out one task per element. See
> [ADR-0084 → "What is retired"](../adr/0084-explicit-state-capture.md).

#### Ordering: generate → capture → use

**Store-after-use is the one broken order**, and `soul-lint` rejects it as an **ERROR**
(`state_store_after_use`). If a value is generated, captured, then used to configure a host, every
crash point is recoverable: crash before the capture and nothing was applied; crash after it and the
value is in Postgres/Vault where an operator can reach it. If it is generated, **used**, and only
then captured, a crash in the window between leaves a live host configured with a value that exists
nowhere else.

A second rule rides the same pass. **A stale same-Passage read is an ERROR too**
(`state_stale_same_passage_read`): an interpolated `${ incarnation.state.<field> }` refreshes only
at a Passage boundary, so a task reading a field that an earlier task in the **same** Passage
captures would render the *pre-capture* value — while the verb engine, which always reads live,
would have written the new one. Rather than let the two mechanisms disagree in production, soul-lint
rejects the pair; the author's options are the ones the DSL already has (read
`register.<capture>.effective`, or push the reader into a later Passage with a `register:`
dependency).

Both rules run after `include:` expansion, since a producer and its capture routinely land in
different files. Cross-Passage dataflow goes through `apply_task_register` and is not a lexical
relationship, so the guards cover the within-Passage case — which is where the whole current corpus
lives.

#### Reading state while writing it

**State accumulates: a capture is observable to the steps after it, not only to the next run.** How
it is observable differs between the two mechanisms, and their granularity is not the same.

*The verb engine always reads live.* Every capture opens its own transaction and re-reads the row
`FOR UPDATE` before mutating, so `on_conflict:`, `match:` and `expect:` are evaluated against the
state as of that step — two captures in the same Passage included. That is what makes the
read-modify-write verbs mean anything: an `add … on_conflict: skip` is idempotent only because it
can see the element a previous step inserted, and an `expect` asserting against a frozen snapshot
would be worse than no assert at all.

*An interpolated `${ incarnation.state.… }` read refreshes per Passage.* A Passage renders as one
pass before it dispatches, so the read carries the state as of that Passage's render; from Passage 1
on the runner re-reads the row at the boundary. The re-read is unlocked on purpose (the row is
already held `applying` by this very run, so the only writer is this run's own capture path) and is
gated on the plan actually containing a capture step, so a plan without one renders bit-for-bit as
before.

*Ordering two captures across a boundary* uses what the DSL already has: `register:` on the first
plus a `register.<name>` read in the second puts them in different Passages (`render.Stratify`).
Usually nothing is needed — read `register.<name>.effective` instead of going back through
`incarnation.state`.

**What this costs, stated plainly:** a rendered plan is no longer a pure function of pre-run state —
moving a capture step changes what a later step reads. What contains it is that the order is
explicit and reviewable (a capture is a task in a numbered list, not a block that floats to the end
of the run) and that the one genuinely broken order is rejected statically by the guard above.

#### `register.*` as a value source

`value:` / `patch:` / `match:` see `register.<task>.<field>` — but a capture is a keeper task, and a
keeper task reads the **keeper** register bucket only ([ADR-056](../adr/0056-staged-render-passage.md)
slice 2): registers issued by keeper-side tasks of **previous Passages**. The per-host register of a
host probe is **not** visible to it. The channel is one-way by design — a host task reads the union
of its own bucket and the keeper bucket (its own wins,
[ADR-0083](../adr/0083-declared-secret-state-fields.md) §5), a keeper task never sees host register.

The shape is a capture reading the register of a keeper step from an earlier Passage — a
`side: keeper` plugin that creates something, then a capture recording what it created:

```yaml
- name: create the VMs
  module: democloud.vm.created
  register: provision
  params: { … }

- name: record the VMs we just created
  module: core.state.append
  params:
    field: provisioned_vm_ids
    value: "${ register.provision.vm_ids }"
```

A capture cannot reach into a host's register bucket **by name**, and there is no other per-host
root to reach for either: a keeper task binds no soulprint at all, so `soulprint.self` and the
scenario-only `soulprint.hosts` (§4.1) are both unavailable there, and `incarnation.host_count`
reads 0. `register.<task>` in a keeper task is the keeper bucket, full stop. The one way a per-host
value reaches `incarnation.state` is the explicit accessor below.

#### `register.hosts.<name>` — one register across every host

`register.hosts.<name>` is the map **{SID → payload}** for register `<name>` across the hosts that
produced it, and it is readable **only from a keeper-side task**. One capture writes the whole
per-host set in one expression:

```yaml
- name: probe the node id
  module: core.exec.run
  on: [redis]
  register: node_id
  changed_when: "false"
  params: { cmd: redis-cli, args: ["cluster", "myid"] }

- name: record all node ids
  module: core.state.set
  params:
    field: node_ids
    value: "${ register.hosts.node_id }"   # → { "<sid>": {stdout: …, exit_code: 0}, … }
```

The payload under each SID is the whole register value, the same shape `register.<name>` has on the
host that produced it — `.stdout`, `.exit_code`, whatever the module returned. Hosts that never ran the
task (filtered out by `on:`/`where:`, or skipped) simply have no key; the map is not padded.

*It is an accessor, not a task key.* The dependency it declares is on `<name>` — the same
register, read across hosts instead of on one — so the capture lands in a **later Passage** than
the probe ([ADR-056](../adr/0056-staged-render-passage.md), `render.Stratify`), exactly as
`register.<name>` would. Working example and its L0 case:
[`examples/service/state-verbs/scenario/per-host-capture/`](../../examples/service/state-verbs/scenario/per-host-capture/main.yml).

*Only from a keeper-side task.* Anywhere else — a host task, the destiny pass, `when:`/`changed_when:`/
`until:`, a migration — it is a **compile error**, not an empty map: a host task reading
`register.<name>` is deliberately reading its OWN value
([ADR-0083](../adr/0083-declared-secret-state-fields.md) §5), and a silent empty map would make
`.size() == 0` and an empty `foreach` read as facts. For the same reason `register: hosts` on a task
is refused at parse (`register_name_reserved`): such a register is unreadable from either side — in
a keeper task the accessor wins over it, and on a host task `register.hosts` is refused at compile
whether or not the register exists.

*Per-host values into **different** state fields* is still not a thing — one capture writes one
field, and the field it writes here is the whole SID-keyed map. Splitting it per host would need a
task that repeats per host on the keeper side, which is a different decision
([ADR-0084](../adr/0084-explicit-state-capture.md), amendment 2026-08-26).

Referring to a register name that no keeper task of an earlier Passage issued is an eval error
("no such key") → the task fails → `error_locked`, like any undeclared key in CEL. A conditional
read is gated by a predicate over always-present data — the canon is a ternary over `input.*` with
short-circuit, **not** `has(register.…)`.

**A secret-carrying task DOES fall into the state graph.** Until
[ADR-0083](../adr/0083-declared-secret-state-fields.md) §8 a `no_log: true` probe had its `register`
dropped from the per-host register map, so `register.<task>.*` read "no such key" and the value
could not reach `incarnation.state`. That source-side drop is gone with the key: the same fold feeds
the next Passage's render ([ADR-056](../adr/0056-staged-render-passage.md)), so dropping a row to
protect one field breaks the register chain for every consumer of that task. What keeps that
plaintext off the audit surfaces is the render-time seal, and reading such a register through
`register.hosts.<name>` seals the cell exactly as reading it directly does.

What protects state instead is that the value is never in it. A field declared `type: secret` in
`state_schema` lives in Vault and the capture writes a `vault:` reference rather than plaintext —
and resolution is **orthogonal to the verb**: every verb that writes a field keeps that field's
existing Vault values, and none of them rotates a live credential
([ADR-0083](../adr/0083-declared-secret-state-fields.md) §4,
[ADR-0084](../adr/0084-explicit-state-capture.md)). Minting is *not* the verb's either, and it is
not automatic: a value is minted only where the step's own value carries `generate_secret({…})`,
so an empty derived path resolved without one is a refusal rather than a fresh credential
([ADR-0083](../adr/0083-declared-secret-state-fields.md), amendment 2026-09-02). Output masking on the external GET channels
(`GET /incarnations`, `/history`) remains the independent second layer (see
[keeper/operator-api.md → Secret masking](../keeper/operator-api.md)).

## 8. Open questions (extensions, do not close silently)

- **Per-task granularity `serial:`.** Current model is per-**Passage** min-width
(see §2.2.1; staged-render gives per-task dispatch along the task axis via Passage).
Truly per-task waves (each task in Passage has its own width) remain
deferred: within one Passage, tasks go to the host with one `ApplyRequest`. New
ADR for a real request.
- **Service-level location of include targets - CLOSED** (2026-08-17). Two-level
resolve (§6) looks for the include target first locally
(`scenario/<name>/<file>`), then at service-level, and the canonical service-level
place is the **same `scenario/` directory**: flat (`scenario/<file>.yml`) for a
resource shared by the whole service, or a `_`-prefixed subdirectory
(`scenario/_<family>/<file>.yml`) for the bodies of one family of scenarios (§1).
A separate top-level directory was rejected: it would need its own discovery rule,
while `_<family>/` reuses the existing service-level tier and is excluded from
scenario discovery by the prefix alone.
- **Full scope forwarding via `include:`.** Now only pure scope is expanded
`include: <file>`; scope/control modifiers on an include task
(`vars:`/`loop:`/`when:`/requisites/`on:`/`where:`) are rejected
(`include_modifier_unsupported`, §6). Forwarding semantics (task-level `vars:`
visible to tasks of the connected file; `loop:` to `include:` - file repeat) -
subsequent slice.
- **Multi-host sandbox (L3-dispatch).** **L0 render-multi-host (`fixtures.hosts`) - closed** (amendment 2026-06-22, [ADR-023](../adr/0023-trial-test-runner.md)): the list of hosts is driven by a standard harness at the render level. **L3-dispatch** remains open: docker block format `stand:` for multi-host scenario test, `assert.dispatch` (who actually executes the master) and assertions for committed cross-host `incarnation.state` - open Q extension about sandbox from [destiny/testing.md](../destiny/testing.md). Not a closed solution.
- **Move `role/*.yaml` to destiny-`input:`.** Parameters that previously depended on the role (the `role/*` layer was removed, and with [ADR-0082](../adr/0082-service-vars.md) the whole directory-overlay scheme went with it, see [concept.md](concept.md)), are moved to destiny through `input:` by probe-role - a separate implementation task (pilot and a batch of rewriting examples).
- **Bootstrap role source on `create` is CLOSED.** On `create` redis more
is not running, probe is not possible → topology (declared roles + master address)
is taken from `soulprint.hosts` (declared from the host's Choir Voice, which
the create scenario writes with a `core.choir.present` step; see §4.1) and is
forwarded to destiny via `apply: input:`. Per-role
step targeting for `create` is not entered; `where:`-invariant
(register-only-after-probe) is not weakened. Old wording
"cross-host master discovery - pending propose-and-wait" has been removed.

## 9. See also

- [concept.md](concept.md) - what is a scenario, border with destiny, declared vs actual role, role-agnostic service vars.
- [destiny/tasks.md](../destiny/tasks.md) - **DSL task core**, inherited by scenario entirely (source of truth according to `module`/`include`/`block`/`async`/`loop`/`register`/requisites/`retry`/`timeout`/`changed_when`/`failed_when`/template context).
- [architecture.md → ADR-008](../adr/0008-coven-stable-tags.md), [ADR-009](../adr/0009-scenario-dsl.md).
- [architecture.md → "Targeting and host communication"](../architecture.md) - `on:`/`where:`, resolver contract, probe.
- [architecture.md → "Service - structure and manifest"](../architecture.md) - service repo layout and `scenario/<name>/main.yml`.
- [destiny/testing.md](../destiny/testing.md) - format `case.yml`/`verify:`/`expect:`, differentiation between scenario tests / destiny-molecule / service-smoke.
- [docs/input.md](../input.md) - `input:` standard for `input:` script block.
- [soul-lint.md](../soul-lint.md) — backlog of statistical checks scenario (`where:`/`on:`-literals, inline mutation).
