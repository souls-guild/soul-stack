## ADR-0084. Explicit state capture — `core.state.<verb>` replaces end-of-run `state_changes`

> ★ **`examples/service/redis` is no longer in this repository.** It left with NIM-871 and lives on its own at [`soul-stack-services/redis`](https://github.com/soul-stack-services/redis). Every citation of that path below names a decision and the shape it took, not a file you can open here.

**Status:** accepted, implemented (NIM-699); amended 2026-08-26 (NIM-711)
**Amends:** [ADR-0083](0083-declared-secret-state-fields.md) §4 (the module's address becomes `core.state.set`, the secret rule stops being the verb's, and the record no longer reaches Postgres through an end-of-run commit), [ADR-057](0057-state-changes-crud-verbs.md) (the CRUD verbs move from a scenario section to a module address, and gain `present`/`append`/`unset`), [ADR-009](0009-scenario-dsl.md) (the `state_changes:` section leaves the scenario grammar), [ADR-017](0017-keeper-side-core.md) (`core.state` gains six more states)
**Implemented by:** NIM-699; amendment 2026-08-26 — NIM-711

---

## Problem

State commits exactly once, at the end of a successful run. After the cross-host barrier
(`keeper/internal/scenario/run.go`): load the register by host → `RenderStateOps` →
`mergeStateChanges(stateBefore, …)` → `commitSuccess`. A run that dies before the barrier commits
nothing at all. (`RenderStateOps` and `mergeStateChanges` are the mechanism this ADR removes; they
no longer exist in the tree — the citations here describe the state it replaced.)

That is not a safety property, it is a defect. A run that creates three database users and then
dies on the fourth task leaves `incarnation.state` saying there are no users. The state is not
merely incomplete — it is **false**, and it is false about things that already exist on the host.
The concrete case this design came out of: a generated password was applied to a live server and
then lost, because the run failed after the user was created and before the barrier.

The mechanism is also structurally awkward in two smaller ways:

- It renders in a context nothing else has. `RenderStateOps` resolved `compute` itself via
  `p.resolveCompute(in)`, because it ran after the barrier with no preceding render pass. It was
  the only renderer in the system that built its own CEL context.
- It cannot see the roster. `state_changes` renders per-host with `soulprint.SELF`
  (`AllowHosts=false`), so `soulprint.hosts` is unavailable. The shipped redis contract works
  around this by hard-zeroing two fields and explaining why in a comment
  (`examples/service/redis/covenant.yml:830-846`: `redis_hosts: "${ [] }"`,
  `sentinel_quorum: "${ 0 }"`). Those values are known to be wrong at authoring time.

## Decision

**State is written only where a scenario says so, by a keeper-side step, and the write lands at the
step rather than at the end of the run.**

### The verb is the module's state suffix

Keeper-side core already addresses an operation by `base` + `state` — `core.soul.registered`,
`core.choir.present` / `core.choir.absent`, `core.vault.kv-read` / `core.vault.kv-present`.
`config.SplitModuleAddr` splits the address and the state travels on `pluginv1.ApplyRequest.state`.
The ADR-057 verb maps onto exactly that slot, so the grammar needs no second dispatch mechanism:

| module address | ADR-057 verb | meaning |
|---|---|---|
| `core.state.set` | `set` | overwrite the field |
| `core.state.present` | *(new)* | write only if the field has no value; an existing one wins |
| `core.state.add` | `add` | insert an element by identity (`key:` for a map, `match:` for a list) |
| `core.state.append` | *(new)* | append to a list with no identity check |
| `core.state.modify` | `modify` | `patch` every element matching `match:` |
| `core.state.remove` | `remove` | delete every element matching `match:` |
| `core.state.unset` | *(new)* | delete the whole field |

An address the table does not list is **refused**, and so is a param the verb does not take. A
`patch:` handed to a `set` is an authoring mistake; dropping it silently would write the field
without the change the author asked for.

Every other key keeps its ADR-057 name and meaning unchanged: `value`, `key`, `match`, `patch`,
`on_conflict`, `expect`. The field the operation acts on is `field:` — in `state_changes` it was the
verb's argument (`- set: redis_users`), and as module params it needs a name of its own.

```yaml
- name: record the VMs we just created
  module: core.state.append
  on: keeper
  params:
    field: provisioned_vm_ids
    value: "${ register.provision.vm_ids }"

- name: capture the users we just created
  module: core.state.add
  on: keeper
  params:
    field: redis_users
    key: "${ compute.new_user.name }"
    value: "${ compute.new_user }"
    on_conflict: skip
```

### This renames the address NIM-698 shipped

698's module is named `core.state.present`, and its `present` is a **field-level** word chosen for a
value-level reason: a field carrying declared secrets must not lose the values it already has. What
the module does to the field's ordinary content is overwrite it — which the ADR-057 dictionary calls
`set`. Keeping the name would leave one word meaning two different things depending on which half of
the value you look at.

| ADR-0083 §4 as shipped | here |
|---|---|
| `core.state.present` | `core.state.set` |

The params do not move: 698 already ships `field:` and `value:` with their ADR-057 meanings
([ADR-0083 amendment 2026-08-25](0083-declared-secret-state-fields.md)). Migration for the current
adopters is the address alone, and it is mechanical (`examples/service/{redis,dragonfly,mongo}/**`).
`core.state.present` is then free to mean what ADR-057 would expect of it, and becomes a genuinely
new operation rather than a renamed one.

`value:` also stops being declared `list`: 698 narrowed it because a field carrying declared secrets
is a keyed collection, but `field:`/`value:` is now the general write and a scalar field is ordinary.

### Secret resolution is orthogonal to the verb

This is the part the rename makes visible, and it is the load-bearing statement of the whole design.

Every verb that writes a field resolves that field's properties declared `type: secret`
([ADR-0083](0083-declared-secret-state-fields.md) §4) **the same way**: an existing Vault value is
kept, a missing one is minted from the step's `generate_secret()` request, and the register carries a
`vault:` reference rather than plaintext. `core.state.set` overwrites the field's ordinary content
and still does not rotate a live credential; `core.state.add` resolves the element it inserts.

So there are two questions, and each has exactly one answer:

| question | answered by |
|---|---|
| does the incoming value reach the field? | the **verb** — `set` overwrites, `present` yields to an existing value, `add` matches on identity, … |
| what happens to a property declared `type: secret`? | **always** resolve, never rotate — whichever verb wrote it |

Rotation therefore stays inexpressible after the rename, for the reason ADR-0083 §4 gives, and it
stays inexpressible under *every* verb rather than under one name.

`core.state.present` over a field that already holds a value discards the proposal and resolves the
**stored** value instead. The register then quotes what won, and nothing is minted for a write that
was thrown away — a minted-then-discarded credential would be a live secret in Vault that nothing in
state points at.

### `value:` is polymorphic and the SDK type system cannot say so

Recorded because it is a real gap, not an oversight. `core.state.set` writes whatever the
`state_schema` declares for the field — a list, a map, an integer, a string — but `sdk/schema`'s
param types are a closed set (`string`/`int`/`bool`/`list`/`map` plus synonyms,
`sdk/schema/validate.go`) with no `any`, and an empty type is a validator error that
`TestCoreDeclarationsPassSDKValidator` enforces over the whole core catalog.

`value:` is therefore declared `string`, which is correct for 100 % of the current corpus (47 CEL
expressions, 19 scalar strings, 18 block scalars, zero literal ints/bools/flow collections) because a
CEL-wrapped cell is exempt from the literal type check (`config.checkParamType`). A literal list
written out in YAML would be rejected by a check that has no business judging it.

Widening the SDK's type set is a public-contract change touching form generation and input coercion,
so it is **not** taken here. It is filed as **NIM-710**, which also has to decide between an `any`
member and a union spelling, and whether `renderValue`'s recursion into `params:` already evaluates
the cells inside a literal map for free.

### What is retired

`run.go` (render + merge + commit after the barrier) is deleted, the `state_changes:`
scenario section is removed from the grammar (`shared/config/scenario.go` — the `StateChanges` field
and type, its `UnmarshalYAML` dual-parse and `validateStateChanges*`; the `StateVerb` constants and
`stateOpVerbs` stay, they are what the module addresses resolve against). The two `mergeStateChanges`
implementations are already gone: prod merges through `keeper/internal/stateop`, and the trial-side
duplicate (`trial/diff.go`) plus the Mirror tests that pinned the two to each other (`trial/diff_test.go`)
were deleted when the harness moved onto the same engine (F-C, below). The per-verb apply functions
themselves (`applyAddOp`, `applyModifyOp`, `applyRemoveOp`, `applyPatch`, `checkExpect`,
`findListMatch`) are **not** deleted — they live in `stateop` behind the module, which is what makes
porting the grammar cheap.

`foreach` does not become a module state. It was a render-time expander that never reached the
engine, and two iteration mechanisms in one place would be a trap: iteration over a step is the DSL's
own `loop:`.

⚠ **`loop:` does not reach an `on: keeper` task today** — `renderKeeperTask`
(`keeper/internal/render/pipeline.go`) rejects it alongside `apply:` and `async:`, a pilot
restriction older than this ADR. A capture is a keeper task, so until that lifts a capture over a
runtime-sized collection must be written out one task per element. That is the one thing `foreach`
could express and the replacement cannot; the corpus does not hit it (**zero** scenarios use
`foreach` — see F-A), which is why this ADR names the gap instead of widening the pilot scope to
close it. The restriction is pinned by `TestRender_LoopOnKeeperTaskRejected`
(`keeper/internal/render/async_test.go`) so lifting it has to be a deliberate act.

**Consequences that follow directly:**

- A failed run leaves state describing what was actually captured before it died, not an
  all-or-nothing snapshot. `provisioned_vm_ids` / `provisioned_sids`
  (`examples/service/redis/covenant.yml:809-821`) stop being lost when a task after provisioning
  fails — which is the failure that leaves real cloud VMs that day-2 cascade-destroy cannot find.
- `state_history` becomes a snapshot per capture rather than per run.
- Capture is an ordinary task: it has a position, it participates in `require:` / `onchanges:`, and
  it is visible in the rendered plan and in run visibility like every other step.
- The two hard-zeroed redis fields stop being lies — a capture step is not bound by the
  `AllowHosts=false` restriction of the state render context.
- ADR-019 migrations are unaffected. They operate on stored state and never on the write path.

### What the existing corpus needs

The full inventory is in the NIM-699 comment; the sizing facts that constrain this ADR:

**55 authored blocks · 113 ops · 100 % verb `set` · 10 services** (81 ops after de-duplicating the
downstream ↔ in-tree mirror). Twenty-two of the 55 blocks are
`state_changes: {}` and translate to nothing at all.

★ **The ADR-057 CRUD-verb grammar has zero real users.** No scenario in any reachable repo uses
`add`, `modify`, `remove` or `foreach`, and none uses `match` / `patch` / `key` / `on_conflict` /
`expect`. The grammar is implemented (then `keeper/internal/scenario/state.go`, now `keeper/internal/stateop`) and exercised only by its own unit tests.
That is the single largest scope lever in this ticket — see fork **F-A**.

Two thirds of the ops (75/113) are a mechanical move: the same CEL string becomes a task param.
The remaining third is covered by the semantics below and by the forks.

## The ordering guard

**Generate → store → use is crash-safe. Store-after-use is the one broken order.**

If a secret is generated, then captured, then used to configure a service, every crash point leaves
recoverable state: crash before capture and nothing was applied; crash after capture and the value
is in Postgres/Vault where the operator can reach it. If it is generated, used, and only then
captured, a crash in the window between use and capture leaves a live host configured with a value
that exists nowhere else. That is exactly the failure this ADR is written against.

The order is **statically checkable**: the producing step, the capture step and the consuming step
are all in the rendered plan, and the dataflow between them is visible in the CEL expressions
(`register.<name>`, `compute.<name>`). A capture step that reads a register which is first consumed
by a task at a *later* index than a task that already consumed it is a plan defect, not a runtime
condition.

**Therefore the guard belongs in soul-lint, as an ERROR, not in code review.** Review catches this
kind of thing exactly as often as reviewers are paying attention, which is not a control. soul-lint
runs offline on the scenario tree, has the whole plan, and already resolves the service-level tier.

**A second rule rides on the same pass: a stale same-Passage read.** An interpolated
`${ incarnation.state.<field> }` refreshes only at a Passage boundary (see "Reading state while
writing it"), so a task that reads a field an earlier task in the SAME Passage captures renders the
*pre-capture* value — while the verb engine, which always reads live, would have written the new
one. Both orders are legible in the plan, so soul-lint rejects the pair as an ERROR rather than
letting the two mechanisms disagree in production. The author's options are the ones the DSL already
has: read the capture's `effective` output through `register.<name>`, or push the reader into a
later Passage with a `register:` dependency. This is also what keeps L0 honest — the harness threads
state task-by-task, and this rule is what makes the difference unobservable.

Two limitations to record, since a guard that silently under-covers is worse than none:

- soul-lint is blind across `include:` boundaries for per-task rules. A capture step and its
  producer landing in different included files will not be compared. The guard must therefore run
  after include expansion (`ExpandIncludes`), not on the raw per-file task list.
- Cross-passage dataflow goes through `apply_task_register` and is not a lexical relationship. The
  guard covers the within-passage case, which is where every op in the current corpus lives; a
  cross-passage capture is a separate rule if it ever appears.

## The routing guard

**A capture without `on: keeper` is an ERROR** (`state_capture_not_on_keeper`), raised at config
parse and therefore in soul-lint as well.

The runtime failure alone would not justify a guard: `core.state` is a keeper-side module and exists
nowhere else, so a step routed to a host dies there loudly. What justifies it is the *offline* half.
The L0 harness folds a capture by its module **address** — F-C's whole mechanism — and the address
is identical whether or not the task carries `on:`. An unrouted capture therefore predicts
`state_after` exactly as a routed one does, and the case goes green on a plan the run cannot
execute. That is the false-green class this ADR exists to close, reappearing one level down.

The guard sits at parse rather than among the soul-lint **rules**, and the layer is load-bearing,
not a matter of taste. A rule is called over `scn.Tasks`, which offline still holds the `- include:`
node itself — the splice is keeper's work — so a per-task rule is silent on any task living in an
included file, which is how services are actually written (a thin `main.yml`, the body in
`_shared/*.yml`). A parse-time validator descends into the resolved include and reports at the
included file's own line and column. Both halves verified on one probe service: an
`on: ["${ incarnation.name }"]` inline raises `on_incarnation_id` (`on_incarnation_name` when
this was written; renamed by NIM-730) and the same task moved into
`_shared/capture.yml` raises nothing, while an unrouted `core.state.set` in that same included file
is flagged at `capture.yml:2:3`. The check itself needs nothing but the task — the module address
and the task's own `on:` key. `on:` as a *sequence* is a coven list, not the keeper literal, so
`on: ["keeper"]` is caught too: it names a coven that happens to be spelled `keeper`.

## Why this does not conflict with `error_locked`

Already verified; recorded here so it is not re-litigated.

`error_locked` is **incarnation-scoped, not host-scoped**. `ErrLocked` is checked in the claim path
under the same `FOR UPDATE` as the transition to `applying`
(`keeper/internal/scenario/scenario.go:158-163`) and in the voyage spawner
(`keeper/cmd/keeper/daemon.go:5723`). Neither `keeper/internal/errand` nor `keeper/internal/console`
references the status at all — so ad-hoc `command` / `run` against a host stays available while the
incarnation is locked, by construction rather than by exception.

That escape hatch is precisely what makes explicit capture safe. The three pieces fit:

1. A run that dies half-way leaves state describing exactly what was captured before it died.
2. The incarnation is frozen against further scenarios.
3. The operator can still reach the host by hand to inspect and repair.

**No new entity, no per-host quarantine flag.** Nothing in this ADR changes locking semantics.

## Reading state while writing it

Eleven ops (6 deduped) read `incarnation.state.*` in the same block that writes it —
`examples/service/redis/scenario/update_config/main.yml:158-170` (three
`default(input.X, default(incarnation.state.X, …))` substrate reads),
`.../detach_source/main.yml:79-81` (`merge(incarnation.state.seeded_from, {'detached': true})`),
`.../rotate_tls/main.yml:92-102`.

Today the answer is trivial and unstated: rendering happens once, after the barrier, so
`incarnation.state` always means **pre-run** state. Mid-run capture makes the question real.

**Decision: state accumulates. A capture is observable to the steps after it, not only to the next
run.** How exactly it is observable differs between the verb engine and an interpolated read; the
boundary is spelled out below, because an ADR that says "always" here would be lying.

This reverses an earlier draft of this section, which froze `incarnation.state` at pre-run for the
whole run. That freeze is incompatible with carrying the verb grammar, and the grammar is the point:

- `add` with `on_conflict: skip` is idempotent only if it can see the element a previous step
  inserted. Against a frozen pre-run snapshot the same run would insert twice.
- `expect` asserts match cardinality **before** mutating (`checkExpect`, `keeper/internal/stateop`).
  Against a frozen snapshot it would assert against a
  state that no longer exists — an assert that is worse than no assert.
- `modify` after an `add` in the same run would silently patch nothing.

So the read-modify-write verbs only mean anything if the write is observable. Freezing the snapshot
and porting the grammar are mutually exclusive; this ADR takes the grammar.

**Where the accumulation is observed, precisely.** Two mechanisms, and they do not have the same
granularity.

*The verb engine always reads live.* Every capture opens its own transaction and re-reads the row
under `FOR UPDATE` before mutating (`incarnation.CaptureState`,
`keeper/internal/incarnation/capture.go` — `SELECT … FOR UPDATE` → `mutate` → `state_history` →
`UPDATE state`). `on_conflict`, `match:` and `expect` are therefore evaluated against the state as
of that step, never against a snapshot — two captures in the SAME Passage included. The three
bullets above hold unconditionally; nothing about them depends on render.

*An interpolated `${ incarnation.state.… }` read refreshes per Passage.* A Passage renders as one
pass before it dispatches, so an interpolated read carries the state as of that Passage's render;
from Passage 1 on the runner re-reads the row at the boundary before re-rendering
(`incarnation.SelectState` from the `capturesState` branch of the Passage loop in
`keeper/internal/scenario/run.go`). The read is unlocked on purpose: the row is already held
`applying` by this run, so the only writer is this run's own capture path, and every capture of an
earlier Passage finished before that Passage's barrier. It is gated on the plan actually containing
a `core.state.<verb>` step, so a plan without one renders bit-for-bit as before and cannot be
aborted by a transient error reading state it never asked for.

*Ordering two captures across the boundary* uses what the DSL already has: `register:` on the first
plus a `register.<name>` read in the second puts them in different Passages (`render.Stratify`).
Usually nothing is needed — a capture's `effective` output carries the value that was written, so
the following step reads `register.<name>.effective` instead of going back through
`incarnation.state`.

**What this costs, stated plainly.** A rendered plan is no longer a pure function of pre-run state:
moving a capture step changes what a later step reads. Three things contain that:

1. Order is explicit and reviewable — capture is a task in a numbered list, not a block that floats
   to the end of the run.
2. The ordering guard above is a static check, so the one genuinely broken order (store after use)
   is rejected by soul-lint rather than found in production.
3. The eleven existing read-then-write ops are unaffected: each reads state before any capture in
   its own scenario has run, so pre-run is what they see anyway.

L0 has to model this — the harness renders the task list in order and threads the accumulating state
through it, instead of rendering once against a fixture. Per-Passage granularity is a production
detail L0 does not reproduce (the trial harness does not stratify), and task-by-task threading makes
a case see MORE of its own writes than production does. That divergence is a **false green**, not a
false red: a step reading a field an earlier capture wrote in the SAME Passage renders the new value
under the harness and the old one in production. The resolution is to make that shape unreachable
rather than to model Passages in L0 — the ordering guard above rejects it statically, so no case can
be written that depends on it. That is the substance of fork F-C.

## Amendment 2026-08-26 (NIM-711): `register.hosts.<name>` — the per-host route into state

**A per-host value reaches `incarnation.state` through one accessor, `register.hosts.<name>`,
readable only from an `on: keeper` task.**

### The gap this closes

The Problem section above lists "it cannot see the roster" as one of the *smaller* structural
awkwardnesses of `state_changes:`. That was an understatement in one direction: the retired block
rendered **per host**, so a per-host register was at least reachable — it folded the map last-wins by
SID, which is wrong for anything that differs per host, but not empty. A capture is a keeper task,
and a keeper task binds no soulprint, targets no host and reads only the keeper register bucket
([ADR-056](0056-staged-render-passage.md) slice 2). So the verbs shipped with a hole the block did
not have: a probed node id, a generated port, a member address that exists only per host had **no**
route into state at all. NIM-699 shipped anyway and left this open on purpose.

### The accessor

`register.hosts.<name>` is the run's register buckets inverted by name: the map **{SID → payload}**
for register `<name>` across the hosts that produced it, the payload being the whole register value
as `register.<name>` has it on the host. One capture writes the whole set in one expression:

```yaml
- name: probe the node id
  module: core.exec.run
  on: [redis]
  register: node_id
  params: { cmd: redis-cli, args: ["cluster", "myid"] }

- name: record all node ids
  module: core.state.set
  on: keeper
  params:
    field: node_ids
    value: "${ register.hosts.node_id }"   # → { "<sid>": {stdout: …, exit_code: 0}, … }
```

Three properties are worth stating because each is a place a reader would otherwise guess:

- **It is an accessor, not a task key.** The dependency it declares is on `<name>` — the same
  register, read across hosts rather than on one — so the same `ExtractRegisterRefs` parser that
  feeds `render.Stratify` puts the capture in a **later Passage** than the probe. Without the
  accessor's hop through `hosts.` the extracted name would be `hosts`, which nothing emits: a bogus
  `unknown_register_reference`, and — the dangerous half — **no Passage edge**, collapsing probe and
  capture into one Passage where the capture reads an empty map while soul-lint exits 0.
- **The keeper's own bucket is excluded.** `RegisterByHost` carries a synthetic `keeper` entry
  (`render.KeeperTargetSID`) alongside the real SIDs. It is not a host, so it is not in the map — a
  capture writing `register.hosts.<name>` into state must not put a fake host key into the state
  contract.
- **Secret safety is inherited for [ADR-0083](0083-declared-secret-state-fields.md) §6, but §8 had
  to be taught the shape.** The map is built from the same per-host buckets `hostRegister` reads, so
  a `type: secret` field still lands in state as a `vault:` reference — a DECLARED state secret has
  no plaintext form to leak here. A **module-declared** `secret: true` output (§8) is the other case:
  it does reach `apply_task_register` in the clear by design, and what keeps it out of
  `apply_run_plan.params` / `status_details` is the render-time seal, which taints a cell reading
  `register.<name>` for a sealed `<name>`. That detector matches `<ident>.<field>`, and
  `register.hosts.<name>` is two hops — its inner field is `hosts`, a name reserved at parse and so
  never sealed. Left alone, the accessor would have been the one route by which a sealed register
  reached an unmasked audit surface, since before this amendment a keeper task could not read a host
  register at all. The two hops are flattened in `selectBaseField` so the taint is decided on the
  register name the author actually read; the guard is
  `TestRegisterHosts_SealedRegisterStaysSealedAcrossHosts`.

### The isolation gate is the decision, not the plumbing

Outside a keeper task, `register.hosts` is a **compile error**
(`cel.ErrUnsupported`), not an empty map — in a host task, in the destiny pass, in
`when:`/`changed_when:`/`until:`, in migration-CEL. An empty map would be worse than an error in the
specific way this ADR keeps arguing about: `.size() == 0` and an empty `foreach` would read as
facts. The reason a host task in particular is closed is that `register.<name>` there is
deliberately its OWN value ([ADR-0083](0083-declared-secret-state-fields.md) §5); an accessor that
quietly widened that to every host's value would invert the one-way channel the same §5 establishes.

The gate is a separate flag from `soulprint.hosts`'s `AllowHosts`, and that is not tidiness:
`AllowHosts` is **true** for host tasks in the scenario pass, which is exactly the context
`register.hosts` must stay closed in. It is fail-closed by zero value — a context that does not opt
in gets the error — and it joins the compile-cache key, because one Engine is shared across a run's
keeper and host tasks and a cache that ignored the flag would let whichever task compiled first
decide the isolation for the other.

### `register: hosts` is refused at parse (`register_name_reserved`)

A register named `hosts` is unreadable from **either** side. The accessor is injected at a fixed
field of the `register` root and wins there, so inside a keeper task `register.hosts` is the
SID-keyed map, not the author's payload; on a host task the same expression is refused at compile as
the keeper-only accessor, because that cut-off is syntactic and does not consult what the run
registered. Two different failures, one confusing and one misleading, and neither names the line
that chose the name. Refusing the name at parse replaces both with a diagnostic that does. The check
sits at parse rather than among the soul-lint rules for the reason recorded in
"The routing guard" above: only a parse-time validator descends into a resolved `include:` and
reports at the included file's own line and column. It applies across the whole register address
space — scenario, destiny and `block:` children share `validateTaskNode`.

### What this does NOT add

**Per-host values into *different* state fields.** One capture writes one field, and the field it
writes here is the whole SID-keyed map. Fanning a per-host value out into per-host *fields* would
need a task that repeats per host on the keeper side — a new task key **and** a new CEL root, buying
a case with no user in the corpus. Rejected on those grounds, not deferred as an oversight.

Also rejected, with the reasons, so they are not re-proposed:

- **`soulprint.hosts.registers(...)`.** A register is volatile per run; the soulprint root is
  stable-only by [ADR-018](0018-soulprint-typed.md). Hanging run-scoped data off it makes the one
  reliable statement about that root false.
- **Dispatching `core.state` to hosts.** Impossible by construction, not merely undesirable: `soul`
  does not link a Postgres client and [ADR-011](0011-go-layout.md) makes that a compiler-enforced
  isolation, not a convention.

## Forks

F-A, F-B and F-C are decided and recorded below with the reasoning. F-D is a defect, not a fork.

### F-A. Does capture carry the verb grammar? — DECIDED: yes, and extended

Decided with the user 2026-08-25. The corpus argument for dropping the grammar (116 of 116 ops are
`set`, no scenario has ever written `add`/`modify`/`remove`) was **rejected, correctly**: zero usage
is a symptom of the old shape, not evidence of no demand. An end-of-run block recomputes the whole
field from a pre-run snapshot in one shot, and in that shape `set` is genuinely sufficient — there
is nothing for read-modify-write to bite on. Once capture is a step against accumulating state, the
incremental verbs become the natural form. Dropping them would have been reading the wrong lesson
out of the corpus.

So the grammar ports in full, and gains three operations that only make sense as a step:

- **`present`** — a field-level guard: if the field already has a value, do not write. This is *not*
  698's `present`, which decided the same question at the level of a declared secret property and
  overwrote everything else; that operation is `set` here. Without a `present` verb the field-level
  question would have no address at all.
- **`append`** — add to a list with no identity check. Today this is written
  `set: field, value: "${ existing + [new] }"`, which reads the **pre-run** value; as a step it must
  read the accumulated one, and it is the single most common capture (recording a created resource).
  `add` cannot serve here because it requires `key:` or `match:`.
- **`unset`** — delete a whole field, for teardown. Deliberately a separate address rather than
  `remove` with `match:` omitted, because `remove` cannot express it and must not learn to. An
  omitted `match:` is a WARN in [ADR-057](0057-state-changes-crud-verbs.md) §d and a no-op in the
  engine — `evalOpBool` returns false for an empty predicate, so nothing is selected. That fail-safe
  is the point: a forgotten line removes nothing. Teaching `remove` to read an omitted `match:` as
  "everything" would invert it, and a forgotten line would empty the field instead. Deleting a whole
  field is a different intent from removing elements, so it gets its own address and is written on
  purpose.

**Deferred, and named so it is not mistaken for an omission: `expect` on `set`/`present`/`append`.**
An earlier draft of this section proposed extending `expect` there as compare-and-swap — assert the
prior value before overwriting — as a defence against two day-2 scenarios racing on one field. It is
not implemented, and the reason is that it would not be the same `expect`. ADR-057's `expect` counts
how many elements `match:` hit (`checkExpect`); a whole-field `set` has no `match:`, so the guard
would have to compare a **value**, which is a different mechanism wearing an existing word. That is a
new entity and gets proposed on its own terms. `expect` therefore stays on `modify` and `remove`,
meaning exactly what ADR-057 says it means.

**Not carried:** `on_conflict` on `set`. Overwriting is what `set` means.

**Flagged, deliberately not taken: a `force:` escape hatch on `present`.** It was raised as an
alternative to `expect`-on-`set`, and it has to be named rather than quietly implemented, because
for a field carrying declared secrets `present` + `force` **is secret rotation** — and ADR-0083 §4
closes that door on purpose: *"An incidental re-render must not rotate a live credential, and
deliberate rotation is therefore not expressible here by design — it gets its own decision and its
own ticket."* Full overwrite already has an address here: `core.state.set`, guarded by `expect` when
the prior value matters. Adding `force:` would re-open 698's decision from inside a different
ticket, so it stays out until someone decides rotation on its own terms.

**Not carried:** an `effective` register value for every operation. The four verbs that carry a
`value:` (`set`, `present`, `add`, `append`) report the resolved form of what they wrote — ADR-0083
§6 depends on it to hand `vault:` references to consumers. `modify`, `remove` and `unset` have no
proposed value to resolve and report none; the key is **absent** rather than null, so a consumer
cannot mistake "this verb writes no value" for "resolution produced nothing". If `modify` later needs
to report the field it patched, that is an additive change and does not block this ticket.

### F-B. What does a covenant do with capture? — DECIDED: B2, the ops move into the scenarios

Decided with the user 2026-08-25.

The largest single block — redis's 23 ops at `examples/service/redis/covenant.yml:700-880` (the
downstream mirror carries 22 at `covenant.yml:688-847`) — lives in a shared contract fragment
and reaches three scenarios through `extends: covenant` (`scenario/create/main.yml:22`,
`scenario/create_from_souls/main.yml:33`, `scenario/migrate_cluster/main.yml:46`; merge at
`shared/config/covenant.go:197-215`). `create` has no block of its own; it inherits all 23.

A shared *section* has an obvious meaning. A shared *step* does not, and four properties of the
covenant say so:

1. **A covenant has no `tasks:`, by an explicit decision.** `ScenarioFragment`
   (`shared/config/covenant.go:41-46`) carries four sections, and the reason sits at `:29-35` —
   *"a scenario's identity/tasks/form/inheritance do NOT belong to the fragment (a covenant is a
   contract, not a standalone scenario)"*. Capture is a task.
2. **Position.** Today the merge order (covenant ops first, local after) is a free choice: every op
   applies to one snapshot at the barrier, and two `set`s on one field are rejected outright, so no
   two ops interact. As steps against accumulating state, a capture step must sit after the task
   that produced its value — and the three scenarios have three different task orders.
3. **The conflict rule stops being right.** `mergeStateChangeSections` errors on a duplicate
   `set <field>`. Correct for a declarative block; wrong for a sequence, where `set` then `modify`,
   or `append` twice, is ordinary.
4. **The guarantee moves.** `extends: covenant` today *guarantees* the 23 fields are written — the
   block merges in and a scenario cannot forget one. With steps, a scenario can omit one silently.

**Decision: B2.** `state_changes` leaves `covenant.yml`; the capture steps are written locally in
each of the three scenarios. The covenant keeps `input`/`compute`/`validate` — including the
`compute:` entries the capture expressions read, so the *values* stay defined once even though the
steps do not.

B1 (a task section in the covenant) is rejected: it breaks the invariant quoted above, and no single
splice point suits three different task orders — any anchor invented for it becomes a second
ordering mechanism next to `include:`.

The cost is named rather than hidden: B2 re-introduces the `create` / `create_from_souls` drift that
`extends: covenant` was added to remove, and point 4's guarantee is lost — nothing fails at render
if a scenario forgets a field. It is accepted here instead of being paid for with new surface,
because the same duplication already exists in this tree for tasks and for `form:`, and removing it
is a composition problem larger than this ticket. Shareable pieces live in five places with
different rules: `<root>/covenant.yml` and `<root>/shared/*.yml` via `extends:` (one per scenario,
add-only); `<root>/scenario/*.yml` and `<root>/scenario/<name>/*.yml` via `include:`, where the
second silently shadows the first (`keeper/internal/scenario/include.go:25-48`); and `<root>/vars/`.
Parts have no declared parameters — `examples/service/redis/scenario/redis-deploy-cluster.yml:17-26`
states a 12-name contract in a comment and admits *"soul-lint cross-file does NOT catch this"* —
which is why a behavioural variant exists as a whole second copy (`redis-deploy-cluster-joinable.yml`).
And `form:` is excluded from the fragment on purpose (`covenant.go:29-35`), so the two redis
scenarios repeat 60 of their ~95/105 form lines. Capture duplication joins that queue and is fixed
with it, in its own ticket.

**A side effect that favours this ticket.** Because the block merges wholesale, `migrate_cluster`
writes `provisioned_vm_ids` as well, and "not applicable" has to be spelled as a ternary —
`(has(input.provision) && input.provision.enabled) ? register.provision.vm_ids : []`
(`covenant.yml:809-813`). That is the origin of the whole conditional-capture family, and with it of
the `when:`-on-a-keeper-task problem in F-D. As steps, a scenario that does not provision simply has
no such step: the family disappears instead of moving.

### F-C. How does L0 assert capture? — DECIDED: C4 + C3

**141 case files, 905 asserted fields** hang off `assert.state_changes`
(`examples/service/redis` 64/426, its downstream mirror 64/426, dragonfly 10/47, mongo 3/6). The
comparison target is `setOpsProjection` (`trial/diff.go:346-357`), which projects `set` ops into a
field→value map. `assert.state_after` — the shape that would survive this change intact — is
implemented (`keeper/internal/trial/harness.go:280-291`) and has **zero** users, i.e. it is
untested in anger.

- **C1. Keep `assert.state_changes` as sugar**: the harness scans the rendered plan for
  `core.state.*` steps and re-projects their params into the same field→value map. All 141 files
  keep working untouched. Cost: a projection layer survives the mechanism it was built for, and it
  can only project the verbs that look like `set`.
- **C2. Rewrite 905 assertions as `task_present`** over capture steps. Mechanical but large, and it
  trades a merged-value check for a per-step param check — weaker, since two capture steps writing
  the same field would no longer be compared against their combined result.
- **C3. Move everything to `assert.state_after`.** The honest shape, and the one that keeps working
  whatever F-A decides — but it is a 141-file rewrite onto a code path with no existing users.
- **C4. Make `assert.state_after` a subset assertion**: a case names the fields it cares about and
  the harness compares only those, instead of requiring the whole post-run state. Not an alternative
  to C3 — the thing that makes C3 affordable.

**Decision: C4 + C3.** Decided with the user 2026-08-25. `assert.state_after` becomes partial, then
all 141 case files move onto it and `assert.state_changes` — with `setOpsProjection`
(`trial/diff.go:346-357`) — is deleted.

C4 has landed (`keeper/internal/trial/harness.go` `compareStateSubset` over the shared
`compareFieldsByKey`; L1 migration keeps `compareState`, the full form, for the reason ADR-019 gives).
Recorded as an amendment on [ADR-023](0023-trial-test-runner.md), which is where the `assert` section
contract lives.

**How L0 predicts a capture.** `assert.state_after` no longer compares against the fixture: the
harness collects, in plan order, the merge op of every `core.state.<verb>` step of the rendered plan
(`stateop.OpsFromPlan`, `keeper/internal/stateop/verb.go`), then merges `fixtures.state` → captures
through `stateop.Merge` — the order prod applies them in, and prod's engine. There is no third stage:
the end-of-run commit those ops used to run in is what this ADR retires.

The op is built by the code the module itself runs: `Spec`, `States`, `CheckParams` and `BuildOp`
moved out of `keeper/internal/coremod/state` into `keeper/internal/stateop`
(`keeper/internal/stateop/verb.go`), so the module and the harness read `field:`, `match:`,
`on_conflict:`, `expect:` through one parser. The plan-wide fold sits there too (`OpsFromPlan`), for
the same reason one level up: the harness and the render→merge unit tests both have to predict a run
without running it, and the copy that did not go through `CheckParams` accepted a param combination
the keeper refuses at dispatch. A second reading of those params would be a second
answer to what a verb means, and it would make the harness green on a plan the keeper refuses. The
same move retires the older duplicate: `trial.mergeStateChanges` and the Mirror tests that pinned it
to the prod copy are deleted (see "What is retired"), because a duplicate that has to be pinned is a
duplicate that can drift between the pinning runs.

What L0 does **not** reproduce is per-Passage granularity. The plan is rendered once, so a step's
`value:` is what it renders to against `fixtures.state`, not against the state its predecessors just
wrote — offline, a capture and a later interpolated read of the same field cannot disagree the way
they would in a run. That divergence would be a false green; what makes it unreachable is
`config.StaleStateRead` rejecting the pair offline (see "The ordering guard"), not the harness
modelling Passages. A capture whose params do not survive `CheckParams` is reported as an error
rather than a failed assert — the run would refuse the same task at load, so there is no verdict to
give — and it is checked for every case, including one that asserts no state at all.

**`assert.state_absent`.** A subset check cannot assert a removal: a field the case does not name is a
field the case has no opinion about, which is exactly what an absence looks like. So C4 comes with a
second section, `assert.state_absent: [field, …]`, naming the fields the run must leave gone —
`core.state.unset`, a `remove` that empties a field, and a declared secret stripped on the way out
all end there. Rejected alternative: an `<absent>` sentinel value inside `state_after`, which would
collide with a service that legitimately stores that string and would hide the assertion inside a
value position. The check is strict key absence — a field present but holding null is reported
**with** its value rather than accepted, because dropping a key and blanking it are two different
states and the message has to say which one the run produced.

C1 is rejected precisely because it is cheap: it keeps a projection of the *old* mechanism alive, so
a case would go on asserting what the plan proposed rather than what the state became, and the two
stop agreeing the moment a scenario uses a verb other than `set` — which is the point of F-A. C2 is
rejected for the weakening it names in its own description.

C4 first is what makes C3 honest rather than merely large. Whole-state equality would force every
case to spell out fields it has no opinion about, and a case that must restate the world to assert
one field is a case that will be updated by copying whatever the run produced — which asserts
nothing. Subset comparison also survives a service adding a state field, instead of reddening every
case in the suite.

### F-D. `when:` on a keeper task — DECIDED: refused offline as an ERROR, not implemented

`when:` on an `on: keeper` task is **half-evaluated**, and the half that is missing is exactly the
half this ticket needs.

- A **static** `when:` — no `register.<name>` reference, no soulprint reference, per `isStaticWhen`
  (`keeper/internal/render/pipeline.go`) — *is* honoured, for every task including a keeper one:
  `emitStaticWhenSkip` runs in the task loop before the `IsKeeperTask` branch.
- A **non-static** `when:` — one that reads a register or a soulprint — was copied onto the
  `RenderedTask` (`renderKeeperTask`) and **never evaluated**. Keeper dispatch holds no reference to
  `When`; the only reader in the tree is `keeper/internal/scenario/soulcompat.go`, and it reads the
  field to negotiate `CapabilityFlowControl` — i.e. `when:` is a *Soul-side* capability, and a
  keeper task has no Soul to evaluate it. No guard rejected the combination: `guardApplierWhen`
  covers *apply* tasks only.

This matters here because the natural translation of the conditional ops
(`(has(input.provision) && input.provision.enabled) ? register.provision.* : []`) is a `when:`-gated
capture step — and that predicate reads a register, so it lands squarely in the silent half and
would capture unconditionally. Keeping the ternary inside the value still works, so the family is
expressible; the obvious form is the wrong one and nothing says so. Either a register-dependent
`when:` on a keeper-side task is implemented, or soul-lint rejects it as an ERROR. Leaving it to
review is not an option for a step that writes state.

**Decision: rejected, not implemented — offline ERROR plus a fail-closed render backstop.**
Implementing it would mean teaching the keeper's scenario runner to evaluate flow-control
predicates, which is a Soul-side capability the runner deliberately does not have
(`CapabilityFlowControl`), and it would buy nothing the working form does not already buy: the
condition inside the value (`${ cond ? a : b }`) is evaluated at render, in the keeper env, where a
previous keeper task's register is bound. Two layers, drawn by one definition of "static":

- **Offline, at parse** — `validateWhenOnKeeper` (`shared/config/scenario_task.go`), code
  `when_on_keeper_dynamic_unsupported`, `LevelError`, with a line and a column. It sits beside
  `validateAsyncOnKeeper` on the same `on:` dispatch and reuses `config.IsStaticPredicate`
  (`shared/config/task_refs.go`) — the same predicate behind `apply_when_dynamic_unsupported` and
  `include_when_dynamic_unsupported`, so the linter cannot draw the line differently from the render
  it is predicting. Reached by `soul-lint` *and* by the keeper's own parse.
- **At render** — `guardKeeperWhen` (`keeper/internal/render/pipeline.go`), returning
  `ErrUnsupportedDSL` beside the existing `apply:`/`loop:`/`async:` refusals in `renderKeeperTask`.
  Defense-in-depth: this layer sees a `block:`'s `when:` ANDed into a keeper descendant, which the
  AST layer does not, exactly as with `apply_when_dynamic_unsupported`/`guardApplierWhen`.

A **static** `when:` on a keeper task is untouched and stays the working form — `emitStaticWhenSkip`
settles it before the task is routed keeper-side, false collapsing it to a skip placeholder and true
rendering it normally. Both outcomes are pinned bit-for-bit
(`TestRender_StaticWhenOnKeeperTaskStillWorks`,
`TestLoadScenarioManifest_WhenOnKeeperStaticAccepted`); the refusal is pinned by
`TestRender_NonStaticWhenOnKeeperTaskRejected` and `TestLoadScenarioManifest_WhenOnKeeperDynamic`.
No YAML in the tree regressed: every `when:` under an `on: keeper` task in the corpus is static.
The key-by-key answer for a keeper-side task — including `loop:`, `require:` and the register scope
— is [docs/keeper/modules.md](../keeper/modules.md).

## Dependency this ticket did not record — now paid by NIM-698

**`compute.*` was unreachable from a keeper-side task as of NIM-694 (2026-08-18).** `keeperVars`
(`keeper/internal/render/dispatch.go`) bound `{Input, Register, Incarnation, Vars, Ctx}` — no
`Compute`. Twenty-one ops read `compute.*` and had no translation until that changed.

**Resolved in NIM-698 (2026-08-22).** That ticket needed the same binding for its own §4 mint task and added
`Compute: in.Compute` to `keeperVars`, with a comment naming the exact failure this ticket
predicted: *"Omitting it here made an author's `compute.x` in a keeper task's params fail at eval
with a bare `no such key: x` while soul-lint accepted the file."* The `compute.*` family is now
expressible, and NIM-619 is no longer a blocking dependency of this ticket.

Note `ComputeScope` was **not** added alongside it — only `Compute`. Whether the compile-time scope
guard needs to follow is a NIM-619 question, not a NIM-699 one.

## Rejected

- **A per-host quarantine flag / a new lock entity.** `error_locked` is incarnation-scoped and the
  errand/console path is already outside it; nothing needs to be added. See above.
- **Keeping end-of-run commit as a fallback for scenarios with no capture step.** Two write paths
  with different crash semantics, and the silent one is the wrong one. A scenario that captures
  nothing captures nothing.
- **Making capture implicit in `core.*` modules** (a module declaring what it writes to state). It
  moves the decision from the scenario author, who knows what the state contract means, into module
  authors, who do not; and it makes the ordering guard undecidable statically.
- **Capture visible to later expressions in the same run** (the F5 alternative). Order-dependent
  read-modify-write with no grammar to signal it. See above.

## Amendment 2026-09-01 (NIM-748, [ADR-0087](0087-task-side-derived-from-module-address.md)): F-D's rule survives, its trigger moves off `on:`

**Not implemented.** Recorded here because the decision is accepted; the code is NIM-749 / NIM-750.

[ADR-0087](0087-task-side-derived-from-module-address.md) derives a task's side from its module
address, so `on: keeper` on a `core.state.<verb>` capture becomes an error rather than a
requirement. Two things follow for this ADR.

**F-D stands.** A `when:` reading `register.*` / `soulprint.*` on a keeper-side task is still an
error, with the same code (`when_on_keeper_dynamic_unsupported`) and the same reasoning; what moves
is the **trigger**. Today `validateWhenOnKeeper` is reached only from the `present["on"]` branch, so
it never runs when the author omits the key — which leaves a hole F-D did not close: on a keeper-side
module written without `on: keeper`, the predicate rides to the agent and is evaluated **before** the
module lookup, so `when` false on every host yields SKIPPED everywhere, a **green run**, and the
keeper-side effect silently absent. Under derivation the shape is unreachable, and the validator
relocates to the `present["module"]` branch.

**`state_capture_not_on_keeper` is retired.** The diagnostic exists only to force the author to write
a key the address already implies, and ADR-0087 makes writing it an error instead. Its **destiny
half is not retired but generalised**: the doc comment on that validator names the destiny case, a
destiny task is Soul-side by construction, and the replacement code `keeper_module_in_destiny` covers
all seven keeper-side bases rather than `core.state` alone.

The capture verbs, the ordering guard, the cross-host barrier and `register.hosts.<name>` are
untouched. Until NIM-749 / NIM-750 land, `on: keeper` on a capture remains required.

## Amendment 2026-09-02 (NIM-746, [ADR-0083](0083-declared-secret-state-fields.md)): half of "resolve the same way" is wrong — a missing value is **not** minted on its own

§ *Secret resolution is orthogonal to the verb* says every verb resolves a declared secret *"an
existing Vault value is kept, a missing one is minted from the step's `generate_secret()` request"*
(`:107-111`). Keeping is verb-independent and stands. **Minting is not**, and the second clause of
that sentence already says why without following through: it is the *request* that mints, not the
absence of a value. Where no `generate_secret({…})` marker reaches the resolve, a missing value is a
**refusal**, not a mint
([`keeper/internal/coremod/state/state.go:682`](../../keeper/internal/coremod/state/state.go)).

The two-question table below it is therefore right in shape and wrong in one cell. *"What happens to
a property declared `type: secret`? — **always** resolve, never rotate — whichever verb wrote it"*
holds; what does not is reading "resolve" as "mint if absent". And the row above it is not quite
verb-only either: `present` yielding to an existing value also decides what the **secret** resolve
sees, because it swaps in the stored element — from which
[`config.StripDeclaredSecrets`](../../shared/config/secret_field.go) has already removed the marker
— and sets `noMint`. That is the single case where `present` over a populated field fails closed
(`state.go:679-681`) rather than keeping quiet, and the paragraph at `:123-126` describing that case
is complete only if "nothing is minted" is read as "and the step refuses", which is what it does.

The full three-outcome account, the cites and the consequence for a re-pointed secret path live in
[ADR-0083](0083-declared-secret-state-fields.md), amendment of the same date. Nothing here changes
in the code or in the verb grammar: rotation stays inexpressible under every verb, which is what
this section was written to say.
