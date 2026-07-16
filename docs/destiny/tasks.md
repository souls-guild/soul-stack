# `tasks/main.yml` - destiny task format specification

This document is the **complete specification** of the destiny task format. The source of truth in implementation. Any key not described here is **invalid** and is a validation error. Format extensions - propose-and-wait → editing this document → then code.

Related documents: [concept.md](concept.md), [manifest.md](manifest.md), [input.md](input.md), [vars.md](vars.md), [testing.md](testing.md).

## 1. File format

- **`tasks/main.yml`** — destiny entry point. Top-level YAML - **list** of tasks executed in order of appearance.
- **`tasks/<sub>.yml`** - include-neighbors, same structure (top-level list of tasks).
- There is no wrapper (`tasks:` key) at the top level of the file - the path to the file tells the context.

## 2. Types of tasks

A list element is **one** of three types, discriminated by the presence of exactly one of the keys `module:` / `include:` / `block:`. Having two or zero is an error.

| View | Discriminator | What does |
|---|---|---|
| **Module-task** | `module:` | Calls one state module with parameters |
| **Include-task** | `include:` | Includes the adjacent file `tasks/<name>.yml` (expands inline) |
| **Block** | `block:` | Inline task group with common `when:` / requisites (see §6.5) |

Parallel execution - `parallel: true` flag on any task, **not** a separate view. See §6.

## 3. Complete list of task blocks

Pivot table. Semantics, validation and examples are in §4-§8.

| Block | Type | Apply to | Obligation |
|---|---|---|---|
| `name:` | string | everyone | recommended |
| `module:` | string | module-task | one of {module, include, block} |
| `include:` | string | include-task | one of {module, include, block} |
| `block:` | array of tasks | block-task | one of {module, include, block} |
| `params:` | map | module-task | required if `module:` |
| `vars:` | map | everyone | optional |
| `when:` | string (template-expr) | everyone (on include there is only a **static** predicate, see §4) | optional |
| `parallel:` | bool | everyone | optional, default `false` |
| `loop:` | map | module-task | optional (on include - **deferred**, see §7) |
| `register:` | string (identifier) | module-task | optional |
| `id:` | string (identifier) | module-task (pilot) | optional |
| `output:` | map | module-task | optional |
| `no_log:` | bool | module-task | optional, default `false` |
| `onchanges:` | array of register-id | everyone | optional |
| `onfail:` | array of register-id | everyone | optional |
| `require:` | array of register-id OR `"all"` | everyone | optional |
| `changed_when:` | string (template-expr) | module-task | optional |
| `failed_when:` | string (template-expr) | module-task | optional |
| `retry:` | map | module-task | optional |
| `timeout:` | duration | module-task | optional |

Any other key is a validation error.

> Keys `serial:` / `run_once:` - **scenario-only**: in destiny (`tasks/main.yml`) are not valid (see [`docs/scenario/orchestration.md §2`](../scenario/orchestration.md#2-дельта-scenario-относительно-dsl-ядра)).

## 4. Basic blocks

### `name:`

- **Type:** string.
- **Applies to:** all types of problems.
- **Required:** **recommended** (lint rule will be assigned separately, see [Open Q](#12-open-q)).
- **Semantics:** human-readable string that goes into the apply log, in the trace for coverage ([testing.md](testing.md)), in the Keeper UI and in task error messages. **Not an identifier**: duplicates are allowed, the task cannot be referenced by `name:`.
- **Naming convention:** See §5.

### `module:`

- **Type:** string.
- **Applies to:** module task.
- **Semantics:** full 3-level module name `<namespace>.<module>.<state>` (see [architecture.md → "Addressing modules"](../architecture.md#адресация-модулей)).
- **Validation (static, `soul-lint`):**
  - format `<ns>.<module>.<state>`;
  - namespace either `core` or mentioned in `required_modules:` destiny;
  - `<state>` exists in the module manifest.

### `params:`

- **Type:** map.
- **Applies to:** module task.
- **Required:** required (even if `params: {}` is empty).
- **Semantics:** module arguments. The schema is from the module manifest for a specific `<state>`.
- **Validation:** `soul-lint` checks against `parameters_schema` of the module manifest.

### `include:`

- **Type:** string.
- **Applies to:** include task.
- **Semantics:** file name from the same folder `tasks/` (without slashes). Expands inline, without a separate scope.
- **Expansion - before render, into a flat list.** within-destiny `include:`
is expanded when the destiny artifact is loaded (before the render phase), just like
scenario-include ([scenario/orchestration.md §6](../scenario/orchestration.md#6-двухуровневый-резолв-ресурсов)):
include-task is replaced by tasks of the included inline file in their place;
render gets a flat list without `include:` nodes. After flattening the keeper-side
`registerIndex` and the stratifier are flat: `register:` is addressed by index in the plan.
- **Checking register links remains per-file (cross-file `onchanges` is rejected).**
`register` links in `onchanges:`/`onfail:`/`require:` and CEL predicates are validated
**boot** linter (`validateTaskRefs` to `shared/config/destiny_tasks.go`) on
level **single file**. `onchanges:` in one include file on `register:` declared
in **another** include file, raises `unknown_register_reference` - despite the fact
that after flattening both fall into the general plan. Practical invariant: **`register:`
and its `onchanges:`-consumer are kept in one file `tasks/<name>.yml`** (standards -
`examples/destiny/node-exporter` and `examples/destiny/redis`: install-register and restart
live together). Extending this to cross-file addressing is open Q §12.
- **Rules:**
  - relative paths beyond `tasks/` are not allowed (`../...`, absolute);
resolve strictly inside the snapshot directory `tasks/` (securejoin-clamp);
  - the included file has the same structure (top-level list of tasks);
  - nested `include:` are allowed (expanded recursively);
  - **cycles** (`a → b → a`, direct self-include) are detected along the resolved path
(error `include_cycle`), depth limited by hard ceiling - not
infinite recursion;
  - `name:` on the include task works as **group header** in the apply log;
  - on an include task are allowed **only** `include:`/`name:`/`when:`; any other
modifier (`vars:`/`loop:`/`register:`/`parallel:`/scenario-delta) - error
`include_modifier_unsupported` (disclosure whitelist so that scope is not lost silently).
- **Conditional include (`when:` on the include task) - render-phase group-drop.** On
include-task allow `when:` - then the connected task group is included in the plan
**only** if the predicate is true. When `when:` is false - ALL tasks of the connected
file are **physically missing** from the plan (real exception, not placeholder): they
are not issued, the index is not reserved. This is different from static-false `when:` by
regular/block task (placeholder-skip with index reservation):
  - **predicate scope - static only** (`input.*`/`essence.*`/`incarnation.*`/
`vars.*`). include is expanded **before** stratification when `register:` previous
tasks have not yet been collected, and per-host `soulprint` is unknown. Dynamic include-when
(`register.*`/`soulprint.*`) → error `include_when_dynamic_unsupported` (catches and
disclosure, and `soul-lint` offline);
  - mechanism: expansion drags include-when and group id into each pasted
task, render evaluates include-when **once per group** (ADR-009 amendment
2026-06-24, conditional-include). Works on **both** expansion paths: in scenario
(`Pipeline.Render`) and inside `apply: destiny` (isolated destiny passage). B
destiny include-when is evaluated in **isolated destiny-env** (only
resolved `apply.input` + schema-defaults, WITHOUT scenario-scope), and the solution cache
groups - **separate for each pass** (destiny id groups do not intersect with
    scenario);
  - security: `onchanges:` from outside to `register:` dropped group is not possible -
cross-file register link is already rejected by the per-file linter (see rule below),
so the drop does not leave dangling register links.

> **In the scenario the rule `include:` is different.** The behavior for **destiny** is described here: `include:` is strictly a neighbor in the same folder `tasks/`. In scenario, the resolution is two-level (locally → service-level, fallback is done by the engine, `../` is still prohibited in the syntax) - see [`docs/scenario/orchestration.md §6`](../scenario/orchestration.md#6-двухуровневый-резолв-ресурсов). This rule §4 for destiny **does not change**; the difference in scenario is fixed in the scenario spec.

### `block:`

See §6.5 - inline task group with common `when:` / requisites.

### `parallel:`

Flag on the task - see §6. Not a separate type of task.

## 5. Naming convention `name:`

Written as a **human-readable English sentence in the imperative**, starting with a **capital letter**, without a period at the end. The style matches the conventional commit subject and Ansible task names - the operator reads the apply log from top to bottom and sees the sequence of actions.

Valid:
- `Install redis-server package`
- `Render /etc/redis/redis.conf from template`
- `Ensure redis-server is running and enabled at boot`

Invalid/deprecated:

| Example | What's wrong |
|---|---|
| `install redis-server package` | Starting lowercase letter |
| `install-redis` | Kebab-case identifier, not a sentence |
| `Installing redis-server.` | Present continuous + period at the end |
| `install redis` | Non-Latin (source style - English) |

The specific max length will be fixed along with the lint rule.

> The capital letter rule is fixed **only for destiny tasks**. For scenario and tests - a separate solution.

## 6. Parallelism (`parallel: true`)

`parallel:` - **flag** on a normal task (`bool`, default `false`). Not a separate type of task and **not a grouping mechanism**. Semantics - fire-and-forget: the task starts in a separate flow/thread, the main flow goes further without waiting for its completion.

```yaml
- name: Ping redis             # starts in thread A → flow moves on
  module: core.exec.run
  parallel: true
  register: ping
  params: { command: "redis-cli ping" }

- name: Read replication state # starts in thread B → flow moves on
  module: core.exec.run
  parallel: true
  register: repl
  params: { command: "redis-cli INFO replication" }

- name: Read memory usage      # starts in thread C → flow moves on
  module: core.exec.run
  parallel: true
  register: mem
  params: { command: "redis-cli INFO memory" }

- name: Collect diagnose result    # accesses register.ping/repl/mem →
  module: core.noop.run            # implicit barrier: waiting for all three threads
  params: {}
  output:
    ping:        "${ register.ping.stdout }"
    replication: "${ register.repl.stdout }"   # parsing INFO replication - on the caller side scenario
    memory:      "${ register.mem.stdout }"
```

### Barriers

Parallel tasks **MUST wait for completion** - but strictly in two moments: when referring to their `register:` and at the very end of destiny. There are no automatic barriers at the `include:` / `block:` boundaries: parallel tasks freely "flow" across these boundaries and continue to work in the background.

1. **Implicit barrier - access to `register.<name>`.** If a subsequent task (via `when:`, `params:`, `output:`, `onchanges:`, `onfail:`, `require:`) references `register: <name>` parallel tasks - it blocks until this parallel task completes. The barrier is local: only for a specific `register: <name>`; the remaining parallel tasks continue to execute in the background.

2. **Final barrier - the end of destiny.** At the very end of the destiny run, the framework certainly waits for **still** running parallel tasks. Destiny is not considered complete until all threads are finalized, and if at least one thread fails, destiny is considered failed. This is required behavior, not optional.

3. **Explicit barrier - `require:` or `require: all`.** You can create a barrier task that waits for the mentioned parallel tasks (according to the register-id list) or **all** active parallel tasks (`require: all`). Details - §8.

### Parallel task leakage across boundaries

`parallel: true` on the task inside `include:` or `block:` a thread starts **in the common run-context destiny**. When the include / block ends, the main flow moves on - parallel tasks continue to work in the background until their natural completion or until the final barrier.

```yaml
# tasks/main.yml
- name: Run background warmup
  include: warmup.yml         # there are `parallel: true` tasks inside

- name: Render main config    # starts immediately after include - does not wait for warmup
  module: core.file.present
  params: { ... }

- name: Verify warmup complete
  require: all                # explicit barrier - wait for all parallel tasks
  module: core.noop.run
  params: {}
```

If this behavior is not desired (e.g. warmup should finish before render-config) - this is an explicit dependency via `require:`, not the implicit semantics of include.

### Rules

- **No grouping.** `parallel: true` on two adjacent tasks **does not** mean "execute together". Each one simply runs in its own thread independently. Neighbors can be anything - other parallel tasks, regular (non-parallel) tasks, include, block.
- **Failure semantics by default.** If the parallel task fails, `register.<name>.failed = true` is written. The main flow is **not** interrupted at the moment of fail - it learns about failure only with an implicit barrier (or in a final barrier). At the final barrier, destiny: if at least one parallel task is finally-failed → the entire destiny is considered failed.
- **Cancel on first failure** — open Q (see §12). Now: a failed parallel task does not cancel the others.
- **`when: false`** - the task is skipped, the thread does not start, `register.<name>` is missing, subsequent calls to it result in a validation error.
- **Requisites via `register.<name>`** - they work, and the implicit barrier is implemented through them. `onchanges: [ping]` after `parallel: true ping` is correct: the framework waits for `ping` to complete, checks `register.ping.changed`, then executes/skips the current one.
- **`include:` with `parallel: true`** - the entire connected file is executed in one thread (tasks inside are sequential, as usual); the main flow goes further. At the include boundary **there is no implicit barrier** - parallel tasks from the included file continue to run in the background until their natural completion / final barrier / explicit `require:`.
- **`block:` with `parallel: true`** - block is executed in one thread (tasks inside are sequential); the main flow goes further. Through the block border - the same rule as for include: there is no barrier.
- **`loop:` + `parallel: true`** - each iteration runs in a separate thread, the main flow goes on. Details - §7.

## 6.5. Block - inline task group

`block:` is the third type of task: an **inline list** of N tasks, to which the general `when:` (and optional `onchanges:` / `onfail:` / `require:`) applies. An analogue of Ansible `block:`. It is used when several neighboring tasks have **the same condition** and you don't want to repeat `when:` on each one or put it into a separate file via `include:`.

```yaml
- name: Apply Redis configuration
  when: input.action == 'apply'
  block:
    - name: Install redis-server package
      module: core.pkg.installed
      params: { name: redis-server, version: "${ input.version }" }

    - name: Render redis.conf
      module: core.file.present
      register: redis_conf
      params: { path: /etc/redis/redis.conf, content: "..." }

    - name: Ensure redis-server is running
      module: core.service.running
      params: { name: redis-server }
```

All three tasks are executed only when `input.action == 'apply'`; if false, all three are skipped in one decision.

### Rules

- **Contents** `block:` - top-level list of tasks, the same format and the same set of fields as in `tasks/main.yml`. In pilot C1, `module:`-tasks, `apply:`-tasks and nested `block:` are allowed inside. Within-block `include:` (include-child) - **post-pilot** (within-block include is deferred; within block there are currently only `module:`, `apply:` and nested `block:`); the design intent is preserved, the implementation of pilot C1 does not support it.
- **`when:` block tasks** is a wrapping condition. Falsy → all tasks are skipped. The `when:` block task is ANDed into **each child** (see inheritance below), so the `skipped: when` mark is placed on **each child**, rather than as a single entry on the block task. This is an invariant: `register:` of children remains visible from the outside even with skip (see "register of children is visible from the outside").
- **`onchanges:` / `onfail:` / `require:` block tasks** - apply to the entire group in the same way as to a single task: if the condition is not met, the entire group is skipped.
- **Internal `when:`** are valid and can be combined with an external one by AND: the internal task is executed only if the external `when:` block is truthy AND its own `when:` truthy.
- **`register:`** on the block task itself does not make sense and is **disabled** - block does not call the module, there is nothing to register. Tasks inside can have their own `register:` names; they are available after the block.
- **`name:` block tasks** - group header in the apply log, like the `include:` task. The naming convention is the same (§5).
- **Parallelism:** `parallel: true` on the block task - **post-pilot** (parallel is entirely deferred - no prod-consumer; gate fail-closed, see error code `parallel_on_block_invalid`). Design Intent: `parallel: true` marks **the entire group** as a member of the current level parallel group; within the block, tasks are performed among themselves according to their own rules. Not implemented in pilot C1.
- **`loop:`** on the block task - **post-pilot** (block+loop combination is deferred). Design intent: block is expanded N times with different values of the iteration variable, inside `${ <as>.* }` (or naked `<as>.*` in expression-keys) is available. Not implemented in pilot C1.
- **Nesting.** Block inside block is allowed, depth is not strictly limited. In practice, 2 levels cover all realistic cases.

### Border: implemented in pilot C1 / post-pilot

The specification above is the full design intent of `block:`. The implementation of pilot C1 does not cover everything; an explicit boundary so that during a currency audit the spec is not read as drift:

**Implemented in pilot C1:**

- `module:`-descendants, `apply:`-descendants (delegation to destiny with inherited `serial:`-window) and nested `block:` inside block.
- Inheritance from a block task to its descendants: `when:` (combined with internal AND), `where:`, `serial:`, requisites (`onchanges:` / `onfail:` / `require:`), `vars:`.
- Static-skip of the entire group by `when:` (falsy → all tasks are skipped). Expands into **per-descendant** skip-placeholders: block.`when:` is poured into each child via AND, static-when each one becomes false, and each child carries its own `skipped: when` + its own `register:`. `register:` of children is visible outside block and with static-skip (flat-register-scope is invariant with respect to truthy/falsy - see below).
- Fail-closed failure of module-specific keys at the block level (codes `*_on_block_invalid`, see [naming-rules.md → Error codes](../naming-rules.md)).

**Post-pilot (spec saved as design intent, pilot C1 implementation does not support):**

- `parallel: true` on block - parallel is entirely deferred (no prod-consumer; gate fail-closed `parallel_on_block_invalid`).
- `loop:` on block - block+loop combination delayed.
- Within-block `include:` (include-descendant) - inside the block so far there is only `module:` and nested `block:`.

### `block:` inside the destiny-passage (apply:destiny)

`block:` is supported not only in scenario, but also **within destiny**, rendered via `apply: { destiny: … }` ([ADR-009 amendment](../adr/0009-scenario-dsl.md)). Previously, `block:` in the destiny pass was rejected by the pilot restriction (`ErrUnsupportedDSL`) - the restriction was **removed**. Mirror scenario-block in destiny semantics: inheritance `when:` (AND-merge with internal), `vars:` (base→override), requisites (`onchanges:` / `onfail:` / `require:` - union), nested `block:`; descendants - `module:` or nested `block:`.

**Destiny-block key boundary** (stricter than scenario-block): scenario orchestration on destiny-block or its descendant is **prohibited** (`ErrUnsupportedDSL`) - in destiny it is meaningless (no roster resolution on the descendant, no nested `apply:`):

- `where:` / `on:` / `serial:` / `run_once:` - targeting and waves live on the scenario-applier task, not inside destiny;
- `parallel:` - parallel is entirely deferred (as in scenario-block);
- `loop:` — block+loop combination deferred;
- `include:` - within-block include deferred;
- `apply:`-child - nested `apply:` in destiny is prohibited (the same boundary as a flat destiny-task).

Roster is inherited by the **entire** block (block does NOT narrow down hosts - unlike the scenario where `where:` of the child narrows down); `serial:` - the applier task window is extended to all descendants. `register:` of the descendant block is visible to tasks **outside** block (flat register-scope: descendants are pasted into the general plan of the destiny pass with end-to-end indexes - a typical case "TLS tasks in `block.when`, restart-`onchanges:` reads them from the outside `register:`"). **Static-false `when:` block is expanded into per-child skip-placeholders** (NOT one placeholder for the entire block): block.`when:` is poured into each child via AND, and each child emits its own skip-placeholder with its `register:`. Therefore, `register:` descendants of the static-false block are visible from the outside through these placeholders - **flat-register-scope is invariant with respect to static-skip** (does not depend on the truthy/falsy outcome of `when:`); restart-`onchanges:` outside resolves the same way both when the block is active and when the block is extinguished.

### When `block:` vs `include:`

- **`block:`** is a short inline group (2–6 tasks) that is not reused. We don't create files.
- **`include:`** is an overused group or large block (>~10 tasks) that overloads reading `main.yml`.

### What is **not yet** (open questions)

- **`rescue:`** (Ansible) - a list of tasks executed when fail inside `block:`. Now we make do with `onfail:` tasks after the block.
- **`always:`** (Ansible) - a list of tasks performed regardless of the outcome of the block (cleanup, finalize). Now we get by with ordinary tasks without `onfail:`/`onchanges:`-dependence on the block.

Both mechanisms are fixed in §12 as open Q. Before their implementation, the semantics of `block:` is only grouping by condition, without error-handling.

## 7. Cycles (`loop:`)

Repeats one task or one `include:` over the elements of the collection.

### Revealing phase (normative)

`loop:` expands in **render phase**, per-host: one task → **N
`RenderedTask`** by elements `items:`, with end-to-end indexes of run tasks
(symmetrical to the expansion of `apply: destiny`). This is **different** from `include:`,
which is revealed earlier, at the config-splice stage (pasting the adjacent file into
flat task list **to** render). Reason for difference: `items:` —
CEL/template-expression (`${ input.users }` / `${ vars.x }`) and CEL is calculated
only in the render phase; at the config-splice stage, the collection values are not yet known.

Expansion order in render: resolve target (`on:` → `where:` → `run_once:`) →
resolve `items:` (once per run, host-invariant source `input`/`vars`)
→ for each element (after the `when:` filter) render `params:` with active
loop variable → N `RenderedTask`. The `loop:` and `serial:` axes are orthogonal (see
[orchestration.md §2.2](../scenario/orchestration.md)): the entire loop is rolled
on each wave host.

**Static-when precedes loop-fan-out.** If task-level `when:` is static
(register-/soulprint-independent) and is calculated in `false`, the task is skipped
ENTIRELY **to** resolution `items:` — Option b placeholder-skip
([ADR-012(d)](../adr/0012-keeper-soul-grpc.md)) extended to loop. Therefore
loop task of inactive branch with `items:` to missing optional-input
(`items: ${ input.users }` with another `action`) does not drop `no-such-key`, but gives
skip-placeholder: N placeholder if `items:` still resolves (parity with
active branch), otherwise one placeholder for the entire task.

**Static-when precedes DSL validation.** The same static-when-gate is triggered
**before** checking support for DSL task constructs (`guardPilotDSL`/
`guardDestinyTask`): statically-false task gated off is skipped even if it carries
design not yet supported in the pilot (`parallel:`/`block:`). Therefore
inactive branch multi-action destiny does not block rendering of active branch - unsupported
DSL is rejected `unsupported_dsl` ONLY when that branch is activated (per-action
validation). This is not a disguise: the task is not physically executed until `parallel:`
the stream does not reach. Example: `diagnose`-branch with `parallel: true` +
`when: input.action == 'diagnose'` silently boils at `action: update_acls`, and
with `action: diagnose` is rejected as unsupported.

### Basic syntax

```yaml
- name: Apply ACL for each redis user
  module: core.exec.run
  loop:
    items: "${ input.users }"       # array or object
    as: user                        # variable name in iteration; default: item
  params:
    command: "redis-cli ACL SETUSER ${ user.name } ${ user.acl }"
    no_log: true
```

### Fields `loop:`

| Field | Type | Default | Description |
|---|---|---|---|
| `items:` | template-expr | — *(required)* | Item Source: Link to `input.<X>` / `vars.<X>` with schema `array` or `object` |
| `as:` | string | `item` | Variable name of the current element in task templates |
| `index_as:` | string | — | Variable name for index/key (optional) |
| `when:` | template-expr | — | Element filter: Iterate only if the expression is truthy for a particular element |

Parallelism of iterations is controlled by task-level `parallel: true`, not by a separate `loop.parallel:`. See subsection "`loop:` + `parallel: true`" below.

> **Pilot:** `items:` and `loop.when:` are calculated **once per run in
> host-invariant context** (`input`/`register`/`incarnation` + loop-
> variable, **without `soulprint`**). `when:` - filter by element content
> (`item.enabled`), non-per-host predicate: reference to `soulprint` in `loop.when:`
> → validation error (host-variable `when:`/`items:` via `soulprint`
> specific host is deferred along with per-host loop filtering). Per-host selection
> according to the host facts, it is done at task-level `where:`, not inside `loop:`.

### Semantics `items:`

- **Array** (`[a, b, c]`) → `as` - the variable runs through the elements. `index_as:` — 0-based numeric index.
- **Object** (`{k1: v1, k2: v2}`) → `as` variable is a value. `index_as:` is the key. The iteration order is alphabetical by key.

### Semantics of `loop` on `include:` - **deferred**

> **Not implemented.** `loop:` on the include task is rejected by expansion as
> `include_modifier_unsupported` (on include only `include:`/`name:`/ are allowed
> `when:`). The design intent below is retained as a guide for the future slice, but in
> not available for current pilot. For "N times over elements" use `loop:` on
> module-task (implemented) or a repeatable structure inside the included file.

Design intent (not implemented):

```yaml
- name: Provision each redis user
  include: ensure-user.yml
  loop:
    items: "${ input.users }"
    as: user
```

The contents of `ensure-user.yml` would be executed N times, at each iteration `${ user.* }` (in lines) / `user.*` (in expression-keys) is available in the file's tasks.

### `loop:` + `parallel: true`

If the task has `parallel: true` **and** `loop:`, each iteration runs **in its own thread** (fire-and-forget). The main flow moves on after all iterations have started, without waiting.

All iterations are written to the same `register: <name>` as an array of results (`register.<name>[0]`, `register.<name>[1]`, …). There are no special barrier semantics on the loop - the general rule §6 applies: the link to `register.<name>` waits for the completion of the **entire** task (that is, all iterations), because there is only one register by this name and is filled with the entire task.

Use with caution: modules must be thread-safe relative to host state. For example, `core.pkg.installed` of two different packages in parallel is a conflict via apt-lock (apt holds a global lock). `core.exec.run` independent redis commands - usually safe.

### What is not available

- **Nested `loop:`** in one task. If you need nesting, use an external `loop:` to `include:`, which has its own `loop:` inside.

## 8. Requisites - Salt-style dependencies

All three blocks refer to `register:` names of other tasks. `register:` therefore plays a dual role: place for the result + identifier for addressing.

### `register:`

- **Type:** string (identifier, `[a-z][a-z0-9_]*`).
- **Applies to:** module task.
- **Semantics:** the name of the variable into which the task result is saved. The value contains at least the field `.changed` (bool - whether the task changed the state of the host) and `.failed` (bool - whether the task crashed). Fields from the module (`.stdout`, `.exit_code`, ...) are from its `output:`.
- **Access:** `${ register.<name>.* }` in string interpolation; in top-level expression-keys (`when:`/`changed_when:`/`failed_when:`/`until:`/`where:`) is the bare form of `register.<name>.*` (see [`docs/templating.md`](../templating.md)). In subsequent tasks (the order is not broken).
- **Uniqueness:** Destiny `register:` must be unique within a single run.

### `id:`

- **Type:** string (identifier, `[a-z][a-z0-9_]*` - same format as `register:`).
- **Applies to:** module-task *(pilot-constraint; on `block:`/`include:` not yet supported)*.
- **Semantics:** **address only** of the task for subscribing to "this task has changed state" alerts (per-task-changed-notifications, [ADR-009 amendment](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). Unlike `register:`, `id:` **doesn't** capture the output of the task and **doesn't** create the variable `register.<name>` - it is an extremely stable selector for subscription.
- **Optional.** Absence of `id:` is normal; most tasks don't have it.
- **Mutually exclusive with `register:`.** The task with `register:` is already addressable by it, so `id:` is redundant and prohibited on it (validation error). `id:` is needed for tasks **without** `register:` that still want to be addressable for alerts. Single format - because `register` and `id` live in the same subscription address space.
- **Works for keeper-side tasks (`on: keeper`).** The address `register ∪ id` of the keeper-side task (`core.cloud.provisioned`/`core.vault.kv-read`/`core.soul.registered`, [docs/keeper/modules.md](../keeper/modules.md)) is also included in the `changed_tasks` terminal event run, and the subscription-alert for it is working. A typical case is `provision_vm` with `id:` without `register:` (see [ADR-052 amend §k/§l](../adr/0052-herald-notifications.md#amend-kl-t4-fix-2026-06-12-changed_tasks-и-task-подписка-покрывают-keeper-side-задачи)).

```yaml
- name: Reload sysctl after tuning
  id: sysctl_reloaded          # address for the "sysctl has changed" alert; output is not captured
  module: core.sysctl.present
  params: { name: vm.swappiness, value: "10" }
```

> **Pilot.** Now `id:` is valid only on a module task (it has its own changed signal). On `block:`/`include:` - validation error; expansion - a separate approach. The uniqueness of `id:` by run (after expanding `include`/`block`) is checked by a separate cross-ref phase of the lint.

### `onchanges:`

- **Type:** array of register-id.
- **Applies to:** all types of problems.
- **Semantics:** the task is executed **only if** at least one of the mentioned tasks had `register.<name>.changed == true`. If no one has changed the state, the task is skipped with the mark `skipped: no changes upstream`.
- **Purpose:** Salt-style handlers - "restart the service if the config has changed."

```yaml
- name: Render redis.conf
  module: core.file.present
  register: redis_conf
  params: { path: /etc/redis/redis.conf, content: "..." }

- name: Restart redis-server because config changed
  module: core.service.restarted
  onchanges: [redis_conf]
  params: { name: redis-server }
```

### `onfail:`

- **Type:** array of register-id.
- **Applies to:** all types of problems.
- **Semantics:** the task is executed **only if** at least one of the mentioned tasks had `register.<name>.failed == true` (mirror `onchanges:`, triggered by `failed` instead of `changed`). `TIMED_OUT`-source is a special case of `failed` (`register.<name>.failed == true`), also triggered by `onfail`. If none fell, the task is skipped (`skipped`). In a normal run without failures, the `onfail` task is **always** `skipped`.
- **Purpose:** rescue / recovery handlers, local compensation (rollback, cleanup, failure notification).

```yaml
- name: Apply migration
  module: core.exec.run
  register: migration
  params: { command: "redis-migrate up" }

- name: Rollback migration on failure
  module: core.exec.run
  onfail: [migration]
  params: { command: "redis-migrate down" }
```

#### `onfail:` changes the fail-stop run (rescue semantics)

By default, the run operates in **fail-stop** mode: the first failed task (`failed`/`timed_out`) stops the main flow. `onfail:` introduces an exception to this invariant - **rescue-tail**:

- The first `failed`/`timed_out` task **irreversibly** marks the result of the run as a failure (`RunStatus = FAILED`).
- But the task cycle **is not interrupted**: all subsequent **regular** (without `onfail:`) tasks are skipped (`skipped`, the module is not called), and are processed **only** by `onfail:` tasks whose source has failed. This is rescue/cleanup.
- `onfail:`-task failure **not cancelled**: even if all rescue handlers were successful, the final `RunStatus` remains `FAILED`. `onfail:` is compensation, not "forgiveness," for failure.
- `failed_when: false` (ignore_errors, [see below](#failed_when)) does task `OK` - it **doesn't** trigger either fail-stop or `onfail:` subsequent ones (it doesn't `failed`).
- Canceling a run by the operator (`CancelApply`) is **not** a fail-stop; when canceling, rescue **does not** work, the cycle is terminated unconditionally (`RunStatus = CANCELLED`).

> Ansible analogue `rescue:` inside `block:`, but through the unified requisite mechanism: `onfail:` task after the block instead of a nested `rescue:` list (see [§6.5](#65-block---inline-task-group)).

### `require:`

- **Type:** **array of register-id** OR **string `"all"`**.
- **Applies to:** all types of problems.
- **Semantics:** the task does not start until the mentioned parallel tasks are completed. In a linear flow without `parallel:` the requirement is redundant (order is already guaranteed).
- **Form 1 - `require: [a, b, c]`.** Wait for the listed tasks by their `register:`-id. The barrier is local - only for these three, the remaining parallel tasks continue to be executed.
- **Form 2 - `require: all`.** Special meaning: wait for **all** active parallel tasks started earlier in this destiny run. Used as an explicit "synchronization barrier" - for example, before a task that wants to see a consistent state after several parallel phases. Mixed (`require: [a, "all"]`) - validation error; the form is strictly one of two.

```yaml
# Point Barrier
- name: Send report once metrics are ready
  module: core.exec.run
  require: [collect_cpu, collect_memory]
  params: { command: "report-send.sh" }

# Global barrier - wait for all parallel tasks
- name: Final consistency check
  module: core.exec.run
  require: all
  params: { command: "check-cluster-state.sh" }
```

> **Border with onchanges/onfail.** `require:` - about **order** (wait). `onchanges:`/`onfail:` - about **condition** (to fulfill or not). They can be combined: `require: [migration]` + `onfail: [migration]` = "wait for migration, execute only if it fails."

### Addressing via `register:`, not via `name:`

`name:` - human readable string, duplicates allowed, not an identifier. Addressing in requisites is strictly through `register:`. If a task is not mentioned anywhere via requisites, it doesn't need `register:`.

## 9. Strength and control of execution

### `when:`

- **Type:** CEL expression, the entire line is treated as a CEL without the wrapper `${ … }` ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md §2.1`](../templating.md#21-top-level-expression-ключи)). The result is converted to bool.
- **Applies to:** all types of problems.
- **Semantics:** the task is executed only if the expression is truthy. Falsy → the task is skipped with the mark `skipped: when`.
- **Combination with requisites:** all conditions are summed by AND. First, requisites (`onchanges`, `onfail`, `require`) are checked, then `when:`.

#### `has()`-guard for optional-input with non-static `when:`

If `when:` of a task is **not static** (referring to `register.*` - a probe result known only to Soul-side; or a mixed `input.x && register.y`), Keeper **must** render `params:`/`vars:`/`loop.items:` of this task: it cannot statically prove skip, because that the `register.*` part of the condition is only known during a run on Soul. Therefore, **optional-input, which is missing in the current run**, will fall `no such key` during the render phase - before Soul calculates `when:` and skips the task.

Pattern solution - the author's `has()`-guard on each call to optional-input in `params:`/`vars:`/`loop.items:` such task:

```yaml
- name: Tune maxmemory on master only
  when: register.role.stdout == 'master'        # NOT static → params are always rendered
  module: core.exec.run
  params:
    command: "redis-cli CONFIG SET maxmemory ${ has(input.maxmemory) ? input.maxmemory : '256mb' }"
```

Here `${ has(input.maxmemory) ? input.maxmemory : '256mb' }` substitutes default when `maxmemory` is not passed to `input:` - render does not crash, and on non-master hosts the task will still be skipped to `when:`.

**Contrast with static `when:`.** For **static** `when:` (register-/soulprint-independent, calculated on Keeper - for example `input.action == 'update_acls'`) guard **not needed**: Keeper proves `when: false` even before rendering and skips the entire task along with it `params:` (static-when placeholder-skip, Option b - [ADR-012(d)](../adr/0012-keeper-soul-grpc.md), see §7 "Static-when precedes loop-fan-out"). Therefore, the task of the inactive multi-action destiny branch safely reads the missing optional-input without `has()` - its `params:` are not rendered at all. `has()`-guard is needed exactly where static-skip is not available: register-dependent or mixed `when:`.

### `retry:`

- **Type:** map.
- **Applies to:** module task.
- **Semantics:** repeat the task up to `count` times if it fails or the `until:` condition is not met. **Enforce - Soul-side** (flow-control, `applyrunner.runTaskWithRetry`): Keeper pulls `count`/`delay`/`until` into `RenderedTask`, the loop is wound by Soul during the run (`until` depends on `register.self.*` fresh attempt - known only to Soul).

```yaml
retry:
  count: 5                     # max. number of attempts (including the first). 0/1/empty - one attempt (no retry).
  delay: 10s                   # pause between attempts; empty - default 5s. convention `duration` Soul Stack.
  until: register.self.changed # opt.: top-level expression-key, whole line = CEL without wrapper ${ ... }.
```

**Loop semantics** (Soul, per architect; per-attempt order: Apply → timeout-check → `changed_when` override → `failed_when` override → `until`-eval):

- **`retry:` WITHOUT `until:`** - retry until attempt FAILED **or** TIMED_OUT; first non-FAILED outcome (OK/CHANGED) → exit. All attempts are exhausted → the final status of the **last** attempt is as is (FAILED **or** TIMED_OUT - TIMED_OUT **doesn't** collapse to FAILED).
- **`until:` (+`retry:`)** - `until` is calculated **after each attempt** (after `changed_when`/`failed_when` override), with the same sandboxed sandbox and activation as `failed_when`. `until`-true → exit, final status = attempt status **as is** (`until` **not** override-it `failed`: failed remains failed; truthy-until on FAILED attempt → final FAILED). `until`-false → `delay` → next attempt. After `count` attempts with `until`-false → task **FAILED** (`flowcontrol.until_exhausted`), **even if** the last attempt was OK/CHANGED. `until` without `count` (count=1) → one attempt + one `until`-eval (false → FAILED). On a **TIMED_OUT** attempt, `until` is **not** calculated (timeout = "failure, repeat if attempts remain").
- **`failed_when: false` × `retry:`** - attempt with `failed_when: false` (ignore_errors) → `failed=false` → "non-FAILED outcome" → exit **on the first attempt** (ignore_errors defeats retry; a failed module, suppressed by a business condition, does not retreats).
- **`delay`** applies **only between attempts** (not before the first, not after the last). Aborted by run cancellation (`CancelApply`): cancel during `delay`/attempt → loop exit, task CANCELLED. `timeout:` (per-attempt) `delay` **is not** included - there is no separate ceiling for the entire loop.

A task is considered finally-failed if all `count` attempts failed/timed out (without `until:`) or `until:` never became truthy (with `until:`). `register.<name>.failed`/`.timed_out` reflect the final outcome. retries-exhausted FAILED triggers `onfail:`/rescue like normal FAILED. Intermediate attempts outward are **not** issued - the contract "one `TaskEvent` for `task_idx`" is saved, the attempts-counter in `register.self.*` **not** is introduced (deferred, Option B).

### `timeout:`

- **Type:** duration (Go syntax: `30s`, `5m`, `1h30m`).
- **Applies to:** module task.
- **Semantics:** a hard limit on one attempt (with `retry:` - for each separately: each Apply gets its own `context.WithTimeout`). Upon expiration, the module receives a cancellation signal (host-side gRPC cancel), the attempt is marked TIMED_OUT (`register.<name>.timed_out == true`). With `retry:` TIMED_OUT the attempt is retraced if there are still attempts (see loop semantics above); `until` is not evaluated on a TIMED_OUT attempt.

### `no_log:`

- **Type:** bool, default `false`.
- **Applies to:** module task.
- **Semantics:** with `true` fields `params:` and `output:` tasks are not written to the apply log, are not saved in the trace, and are masked in the API response. For tasks that leak secrets (passwords, tokens).

### `output:`

- **Type:** map (name → template-expr).
- **Applies to:** module task.
- **Semantics:** values that destiny publishes externally as its result. Task-level `output:` **writes to declared top-level `output:` fields** destiny (see [destiny/output.md](output.md)); names not declared in top-level `output:` destiny - validation error. Used by `expect:` in tests and `register:` caller chains (`register.<applier>.<output field>` - [scenario/orchestration.md §2](../scenario/orchestration.md#2-дельта-scenario-относительно-dsl-ядра)).

### `changed_when:`

- **Type:** CEL expression, whole line = CEL without wrapper `${ … }` (top-level expression-key, [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).
- **Applies to:** module task.
- **Semantics:** overrides how the framework considers `register.<name>.changed`. By default, the module itself reports its `changed` (for example, `core.file.present` compares desired vs actual). For modules that always return `changed=true` (typically `core.exec.run`), `changed_when:` allows you to give a custom criterion.

```yaml
- name: Check if migration needed
  module: core.exec.run
  register: migration_status
  changed_when: contains(register.self.stdout, "pending")
  params:
    command: "redis-migrate status"
```

Inside the expression, `register.self.*` is available—the fields of the task's own result (`.stdout`, `.exit_code`, custom fields from the `output:` module).

`changed_when: false` - the task is never considered to change state (read-only commands).

`changed_when:` directly affects `onchanges:` of the following tasks - handlers are triggered by the overridden `changed`, not by the raw one.

### `failed_when:`

- **Type:** CEL expression, whole line = CEL without wrapper `${ … }` (top-level expression-key, [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов)).
- **Applies to:** module task.
- **Semantics:** overrides how the framework considers `register.<name>.failed`. The default is exit_code != 0 (for exec modules) or the module itself reports failure. `failed_when:` gives a custom criterion.

```yaml
- name: Run migration with non-zero exit on partial
  module: core.exec.run
  register: migration
  failed_when: "!(register.self.exit_code in [0, 2])"    # 2 = partial OK
  params: { command: "redis-migrate up" }
```

`failed_when: false` - the task is never considered abandoned (no matter what happens, we continue). Similar to Ansible `ignore_errors: true`, but through a unified mechanism.

`failed_when:` directly affects `onfail:` the following tasks and stops the main flow when fail.

### `vars:` (task-level)

- **Type:** map.
- **Applies to:** all types of tasks (module, include, block).
- **Semantics:** local variables, accessible **only within this task** (and its iterations, if there is `loop:`). Visible in task templates as `${ vars.<name> }` (in string interpolation) or naked `vars.<name>` (in top-level expression-keys).

**Priority.** If the name in task-level `vars:` matches the name in [`vars.yml`](vars.md), **task-level beats file-level**—the more local scope wins. This is mandatory semantics, not open Q.

```yaml
# vars.yml
redis_unit_name: redis-server

# tasks/main.yml
- name: Render config for staging variant
  module: core.file.present
  vars:
    redis_unit_name: redis-server-staging    # interrupts vars.yml
  params:
    path: "/etc/systemd/system/${ vars.redis_unit_name }.service.d/override.conf"
    content: "..."
```

In this problem, `${ vars.redis_unit_name }` → `redis-server-staging`. In neighboring tasks without their own task-level, `vars:` is again `redis-server` from `vars.yml`.

- **Visibility:** only within one task. This does not apply to tasks connected via `include:` / `block:` child (but the **include-task itself** with `vars:` is yes: the variables are visible to all tasks of the connected file, just like any task).
- **Links inside `vars:`:** values can refer to `input.*`, `incarnation.*`, `soulprint.self.*`, `essence.*`, `register.*`, loop variable `<as>` (if the task is in `loop:`) - **but not to their own task-vars** (no circular/reciprocal references). Each `vars:` value is evaluated in a task context where other task-vars are not yet visible; calling `${ vars.<other> }` inside one of the values `vars:` → error `no such key`. The order in which the keys are declared in `vars:` is therefore irrelevant. *(File-level `vars.yml` as a source of links and the priority "task-level interrupts file-level" - the designed layer; in the current pilot render only task-level `vars:` is implemented.)*
- **Resolve per-task, per-host:** `vars:` are evaluated BEFORE `params:` / `where:` of the same task. Because the values can reference `soulprint.self.*`, they are calculated for each targeted host; the final `params:` must remain host-invariant (pilot limitation, see render pipeline).
- **Value type:** string is interpreted as CEL interpolation (`${ … }`; single block → native type, otherwise splicing into a string - [§10](#10-template-context)). Non-string literals (number/bool/collection) are passed as is, without CEL parsing (symmetrically `params:`).
- **Application:** override of locals for loop iterations, one-off correction of paths in one task, simplification of long template expressions.

### Dry-run

Dry-run is a **mode for running the entire destiny**, not a flag for a separate task. Enabled by the operator at `keeper.incarnation.run --dry-run` or via MCP/API. In this mode:

- The framework calls `Plan(...)` RPC for each module instead of `Apply(...)`. Contract `SoulModule` - see [architecture.md → "Modules Protocol"](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style).
- **Every module must implement `Plan`.** This is part of the SoulModule contract, not an optional feature. If a module cannot actually schedule (for example, a custom module with non-specific logic), it implements `Plan` **as a stub** that returns "no changes / unknown". The stub is valid and acceptable, but the module MUST respond to `Plan`.
- There are no side-effects on the host - modules in `Plan` only read/parse.
- Render templates, validation `input:` / `params:`, verification `when:` / `onchanges:` - performed as in a real run.
- `register.<name>` is filled with the planned values (the module returns them to `PlanReply`).
- Task parameters that affect flow (`retry`, `timeout`, `parallel`) are **ignored** in dry-run: retry does not repeat (`Plan` is deterministic), timeout does not limit (but the framework sets a general timeout safety), parallel does not launch real threads (execution is sequential for a predictable report).

There is no special field for dry-run at the task level. If a task needs the behavior "do something special only in dry-run", this can be solved through the template context: `run.mode == 'dry_run'` (top-level expression-key, CEL without a wrapper) - open Q, whether such a variable should be entered (see §12).

## 10. Template context

Template engine - CEL for YAML expressions ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [`docs/templating.md`](../templating.md)). Two positions:

- **Top-level expression-keys** (`when:`, `changed_when:`, `failed_when:`, `until:`; in scenario also `where:`) - entire line = CEL **without wrapper**.
- **String interpolation** in `params:`, `output:`, `loop.*:`, `vars.yml` values, task-level `vars:`, `apply: input:` - via the `${ … }` marker.

Available in both positions:

| Name | Contents |
|---|---|
| `input.<name>` | Destiny parameters from the caller, declared in `destiny.yml → input:` and validated ([input.md](input.md)). |
| `vars.<name>` | Destiny locals. Resolves according to the rule **task-level `vars:` beats file-level `vars.yml`** - the more local scope wins. More details: [vars.md](vars.md), task-level - §9. |
| `soulprint.self.<…>` | Facts about the current host: `soulprint.self.os.family`, `soulprint.self.network.primary_ip`, `soulprint.self.memory.total_mb`, … ([ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp), [`docs/soul/soulprint.md`](../soul/soulprint.md)). Bare `soulprint.<path>` without `.self` - validation error `soul-lint`. Cross-host requests (`soulprint.where(...)`, `soulprint.hosts`) - scenario level, **not destiny**. |
| `register.<name>.*` | Results of previous tasks (per `register:`). Standard fields: `.changed`, `.failed`, `.timed_out` + fields from `output:` task. On a parallel task, calling `register.<name>` creates an **implicit barrier** (see §6). In a scenario, the probe step is executed on each target host → `register.<name>` is a per-host **map** `sid → payload`, not a scalar; convolution to one value - [scenario/orchestration.md §4.3](../scenario/orchestration.md#43-свёртка-агрегатного-register-к-одному-значению). |
| `register.self.*` | Own result of the task. Available **in all expression-key task contexts** - `changed_when:` / `failed_when:` / `until:` (destiny + scenario), as well as `where:` (scenario-only, see [orchestration.md §4](../scenario/orchestration.md#4-волатильный-предикат--where)). |
| `soulprint.hosts` | **Scenario-only.** List of run hosts; element - stable facts `sid` / `role` (declared) / `network` / `os` / `covens`. `.where("<pred>")` filters by any attribute (predicate-string). In destiny **not available** (like any cross-host `soulprint` request); destiny receives topology only through `apply: input:`. Specification - [scenario/orchestration.md §4.1](../scenario/orchestration.md#41-soulprinthosts--список-хостов-прогона-scenario-only-аксессор). |
| `incarnation.host_count` | **Scenario-only.** Number of hosts in the run target. Used in the probe idiom of completeness (`failed_when: size(register.<p>) < incarnation.host_count`, [scenario/orchestration.md §5](../scenario/orchestration.md#5-probe-идиома-и-обработка-ошибок)). Not available in destiny (part of `incarnation.*`, scenario-scope). The formal definition is [`docs/scenario/orchestration.md §4.2`](../scenario/orchestration.md#42-incarnationhost_count--размер-таргета-прогона). |
| `<as>` / `<index_as>` | The current item/index of the active `loop:` (names are configured in `loop:`). |

### Context depends on the calling entity (destiny = host / scenario = cluster)

The DSL core of the tasks above is common to destiny and scenario ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)). But **the template context is different** because the scope is different (one host vs one cluster):

| | destiny (single host) | scenario (one cluster) |
|---|---|---|
| `essence.*` | **NO** - destiny is isolated, sees only `input:`. Service inserts values into `input:` when `apply:` is called. | **is** - merged essence (default → os → coven → spec) is available directly. |
| `incarnation.*` / `scenario.*` / `state.*` | **NO** - destiny does not know about the database and who called it. | **is** - attributes of the calling scenario / incarnation, current `state` from the database. |
| `soulprint.where(<predicate>)` | **NO** - cross-host requests are a scenario level. | **is** - cross-host lookup by string predicate (`"'db' in covens"`, `"coven == 'prod'"`); in detail - [templating.md §2.3](../templating.md#23-зарегистрированные-cel-функции-стартовый-минимум). |
| `vars.*` | destiny-locals from [`vars.yml`](vars.md) + task-level | scenario-locals (scenario-`vars.yml`) + task-level - two-level resolution, see [scenario/orchestration.md](../scenario/orchestration.md). |

The listed "NO" refers specifically to the destiny context. Scenario context, orchestration keys (`on:`/`where:`/`apply:`) and cross-host mechanisms are specified in [`docs/scenario/`](../scenario/README.md) - are not duplicated here.

The exact template engine is fixed [ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов): CEL for YAML expressions (top-level expression-keys without wrapper + `${ … }`-interpolation in lines), Go text/template + sprig-allowlist for files `.tmpl` (rendering via `core.file.rendered`). Regulatory specification - [`docs/templating.md`](../templating.md). The same engine also serves scenario ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

## 11. Full composite example

```yaml
# redis/tasks/main.yml - dispatcher
- name: Apply Redis configuration
  include: apply.yml
  when: input.action == 'apply'
```

```yaml
# redis/tasks/apply.yml
- name: Install redis-server package
  module: core.pkg.installed
  retry: { count: 3, delay: 10s }      # network may blink
  params:
    name: "${ vars.redis_unit_name }"
    version: "${ input.version }"

- name: Render redis.conf from template
  module: core.file.rendered             # ADR-010: .tmpl renderer does core.file.rendered
  register: redis_conf
  params:
    path: "${ vars.redis_conf_path }"
    template: templates/redis.conf.tmpl
    vars:
      maxmemory: "${ input.maxmemory }"  # CEL → text/template-context (see templating.md §6)
    mode: "0640"
    owner: "${ vars.redis_user }"
    group: "${ vars.redis_group }"

- name: Ensure redis-server is running and enabled at boot
  module: core.service.running
  params:
    name: "${ vars.redis_unit_name }"
    enabled: true

- name: Restart redis-server because config changed
  module: core.service.restarted
  onchanges: [redis_conf]
  timeout: 30s
  params:
    name: "${ vars.redis_unit_name }"

- name: Apply ACL for each redis user
  module: core.exec.run
  loop:
    items: "${ input.users }"
    as: user
  params:
    command: "redis-cli ACL SETUSER ${ user.name } ${ user.acl }"
    no_log: true
```

## 12. Open Q

Resolved before or during implementation, fixed by editing this document.

### Coverage and testing
- **Coverage semantics of `loop:`.** Is each iteration a separate coverage-hit, or "task with loop = one hit with N≥1"? Affects the coverage formula in [testing.md](testing.md).
- **Max length for `name:`.** Specific number + what to do with excess (warn / error).

### Parallel
- **Cancel on first failure.** When a parallel task fails, should the remaining in-flights be canceled via host-side cancel, or should they be allowed to finish (current behavior)?
- **Concurrency limit.** Is it worth introducing max-concurrent-tasks for destiny (protection from a barrage of threads)? Now there is no limit.
- **Final-barrier timeout.** Global limit on the final wait at the end of destiny. Currently absent - destiny waits for parallel tasks indefinitely (the modules themselves can give up on `timeout:` tasks).
- **`require: all` scope.** Currently "all active parallel tasks started earlier in this run." To clarify: should we count already completed ones (for consistency, so that failure-state is available), or only in-flight? Now only in-flight is expected.
- **Addressing a specific iteration in `loop` + `parallel:`.** Now the reference to `register.<name>` is waiting for the entire task (all iterations). If someone wants to wait for a specific `register.<name>[i]` - should it be a local barrier on the iteration? Or does this turn the loop into a DAG, which is what we're intentionally avoiding?

### Requisites
- **`prereq:`** (Salt) - the reverse of `require:` (A waits for changes in B; if B is about to change, A is executed first). Is it necessary?
- **Wildcard in requisites.** `onchanges: [redis_*]` — all register-ids by prefix. Convenient, but complicates validation.
- **Cross-file addressing.** Currently: the register link in `onchanges:`/`onfail:`/`require:` is validated by **per-file** (boot `validateTaskRefs`), so the link to `register:` from the adjacent include file is rejected (`unknown_register_reference`) - register and its consumer are obliged be in one file (see §4 `include:`). Open Q: should we introduce explicit cross-file addressing (validation of links according to a flattened plan, not per-file) - keeper-side `registerIndex` after flattening this would have already been possible, but static checking of links would have remained more difficult.

### Include
- **Dynamic `when:` on include - deferred.** Conditional include (§4) supports
only **static** predicate (`input.*`/`essence.*`/`incarnation.*`/`vars.*`),
because include is expanded **before** stratification - `register:` of previous tasks
not built yet, per-host `soulprint` unknown. Dynamic include-when
(`register.*`/`soulprint.*`) → `include_when_dynamic_unsupported`. Open Q: whether to enter
"delayed include expansion" (group resolution in post-probe Passage), which would give
`when: register.X` to include - but would complicate the phase model (disclosure stops
be pre-render phase). So far rejected as premature.
- **`loop:` on include - deferred.** "Include file N times element by element" (§7) not
implemented (`include_modifier_unsupported`); costs `loop:` on a module task or
repeatable structure within the file. Open Q: Should I implement render-time fan-out
include groups by `loop:` (symmetry with `loop:` on the module task).

### Block — error-handling
- **`rescue:`** (Ansible) - a list of tasks executed when fail inside `block:`. Currently unsecured; we make do with `onfail:` tasks after the block. Does it need to be entered as an explicit syntax.
- **`always:`** (Ansible) - a list of tasks performed regardless of the outcome of the block (cleanup, finalize). The same question is whether explicit syntax is worth it.
- **Scope `register:` inside `block:`.** Is `register:` tasks inside the block visible **outside** the block? Now it is assumed "yes, flat scope" - but it may be useful to have a scoped option (there is a block inside, no outside).

### Retry / timeout
- **`retry.until:`-completeness.** What functions/filters are available in the `until:`-expression. Must match the template engine.
- **Backoff strategy in `retry:`.** Currently fixed `delay:`; `exponential`, `jitter` may be needed.

### Dry-run
- **Variable `run.mode`.** Do we need to give tasks a way to know that the run is dry-run (via `when: run.mode == 'dry_run'` in templates)? This is a backdoor for custom behavior, but useful for destiny with critical operations.
- **`Plan`-RPC API.** The exact contract between the host and the module is described in [architecture.md](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style), but the details (event-streaming, prediction format, how the stub should respond) are open Q.
- **Dry-run for requisites.** In dry-run, `register.<name>.changed` is a **planned** change, not an actual one. Does this trigger onchanges-handler? Most likely yes - the operator should see "restart redis scheduled".

### Changed_when / failed_when
- **`register.self.*` scope.** Currently fixed: only available in `changed_when:` / `failed_when:`. Distribution to other task fields (for example, `output:` via `register.self.*` to publish post-overridden `.changed`) - open Q.
- **Impact of `failed_when:` on `retry:`.** If a task finishes with `failed_when: true`, is it considered "failed and needs retry"? Now we assume yes.

### Task-level vars
- **Composition with `loop:` - CLOSED.** Task-level `vars:` are recalculated at **each** loop iteration and can refer to the loop variable `<as>`/`<index_as>` (vars are resolved within the iteration, after loop expansion). Captured in §9 "Links within `vars:`".

### Output
- **Output at the destiny level is CLOSED.** Top-level `output:` in `destiny.yml` (scheme declaration, symmetrical to `input:`) + task-level `output:` (filling declared fields) are accepted as a general mechanism. Specification - [destiny/output.md](output.md); reading from scenario via `register:` on the applier task - [scenario/orchestration.md §2](../scenario/orchestration.md#2-дельта-scenario-относительно-dsl-ядра).
- **Output via `include:`.** Is `output:` visible to tasks from the included file in the caller?

### `name:` as lint rule
- Make it mandatory (error) or leave it as a recommendation (warn). Now a recommendation.

## 13. See also

- [concept.md](concept.md) - what is destiny.
- [manifest.md](manifest.md) - `destiny.yml` and folder layout.
- [input.md](input.md) - `input:`-contract.
- [vars.md](vars.md) - `vars.yml`-destiny locals.
- [testing.md](testing.md) - molecule-style testing (including coverage).
- [architecture.md → "Addressing modules"](../architecture.md#адресация-модулей) - format `<namespace>.<module>.<state>`.
- [architecture.md → "Module Manifest"](../architecture.md#манифест-модуля) - where the `params:` schema for each `module:` comes from.
