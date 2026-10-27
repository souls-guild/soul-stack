# ADR-0091. Scenario dispatch is linear — a task is dispatched only after the previous one has finished

> **Status: amended — the default execution model since NIM-952 (2026-10-11); the two NIM-940 refusals of an unwritten `serial_scope` withdrawn by the Amendment 2026-10-14 (NIM-998).** User decision 2026-09-28 (NIM-907); built by
> the NIM-926 release train. **Supersedes [ADR-056](0056-staged-render-passage.md)**, whose model stays selectable
> for one release as the deprecated rollback `keeper.yml::scenario.execution_model: staged` (user, 2026-10-11);
> amends [ADR-012(d)](0012-keeper-soul-grpc.md); changes what [ADR-0075](0075-intra-host-async-tasks.md)
> applies to; amends [ADR-027](0027-apply-work-queue.md), [ADR-009](0009-scenario-dsl.md) and
> [ADR-0084](0084-explicit-state-capture.md). **Text approved by the user on 2026-10-02**, with the
> two decisions outside the forks: a run that dropped some hosts ends `partial_failed` (item 4), and the
> Acolyte path blocks enabling linear dispatch (item 7). §Before merge carries **four forks** — contract decisions that need the user's answer
> before any code, each with the price of every option. **All four are answered**; fork 3's answer
> was re-confirmed after the tree's own canary was shown to change meaning under it, and its flag is
> `serial_scope: task | block`; the two refusals of an unwritten `serial_scope` were withdrawn
> before release — the default is the standard behaviour
> ([Amendment 2026-10-14](#amendment-2026-10-14-nim-998-an-unwritten-serial_scope-is-the-standard-behaviour-and-nothing-refuses-it), NIM-998). Fork 4 — the scenario's `settings:` block — outgrew this ADR and is specified in
> [docs/scenario/settings.md](../scenario/settings.md).
>
> **Amended 2026-10-14 (NIM-993, partly built — not built: the `rescue_unit` marks the Keeper sends, the
> step recipe's failure state, and the Keeper's half of `core.task.failed`):** a rescue that succeeds handles the failure, and
> the host carries on with the run; a failure waits for its rescues; `core.task.failed`. This supersedes
> two of fork 1's consequences — see [the amendment](#amendment-2026-10-14-nim-993-a-rescue-that-succeeds-handles-the-failure).
>
> **Revision history.** Independent reviews, round after round, shaped this text; each round's
> findings and dispositions are recorded in the NIM-907 comments. In outline: the first versions justified the change by a
> bug class the code refutes (an absent register raises, cross-passage gating is already
> fail-closed), so the justification is now the user's — obviousness. Counting barriers gave way to
> a happens-before invariant, and fail-stop became fleet-wide. The failure policy, first borrowed from ADR-043, became the user's
> blast radius `host` | `scenario` with an Ansible-style `tolerate`, and grew into the `settings:`
> block with three timeouts ([settings.md](../scenario/settings.md)). The forks were answered by
> the user: rescue across barriers; no register on the wire, `when:` on the Keeper and post-module
> predicates rendered there; a block as an overlay of steps by default, kept after the tree's own
> canary was shown to change meaning, with `serial_scope` for the canary. The predicate rewrite
> went through several rounds of CEL corner cases and is now stated as an equivalence with a
> differential test ([predicate-rendering.md](../scenario/predicate-rendering.md)). Later rounds
> found that keeper tasks and pulled-forward steps move to where they are written, and that
> `passage` must number steps, not top-level tasks.

## Context

[ADR-056](0056-staged-render-passage.md) executes a scenario run as N ordered **Passages**, where N
is *derived*: `config.ExtractRegisterRefs` scans `where:` / `params:` / `vars:` / `apply: input:` /
`loop.*` for `register.<name>`, `config.Stratify` builds a graph over "task emitting X → reader of
X", and the number of levels becomes the number of barriers.

**What is wrong with that is not that it is broken. It is that it is invisible.** Where a run waits
is not in the file; it is the output of a rule applied to five key families across the whole task
list. An author who wants to know has to run that rule in their head — and the answer changes when
an *unrelated* task gains a `where: register.X`, because that task moves a boundary for everyone.

The decision is the user's, and the reason is stated as given: *obviousness of a run is worth more
than its speed.* Mass changes matter more than the latency of one run.

### What the derivation is NOT guilty of

Two earlier drafts of this ADR said otherwise. Recorded so the claim is not made a third time:

- **An absent register does not read stale — it raises.** `Vars.registerRoot` hands CEL the
  accumulated map as it is, with no default (`shared/cel/vars.go:280-289`), so a name nobody emitted
  is a cel-go `no such key` at render, not an empty read.
- **Cross-passage `onchanges:`/`onfail:` is already resolved Keeper-side** from accumulated per-host
  CHANGED/FAILED facts (`keeper/internal/scenario/crosspassage.go:44-235`, wired at
  `keeper/internal/scenario/run.go:983-996`), and a Keeper that cannot read those facts **refuses
  the run** rather than guessing (`cross_passage_requisite_unsupported`,
  `keeper/internal/scenario/run.go:569-583`).
- **Cross-passage `when:`/`changed_when:`/`failed_when:` is already a fail-closed refusal** — FC-5,
  `config.CodeCrossPassageWhenGating` (`shared/config/passage.go:106-117`), detector at
  `shared/config/passage.go:534-568`, offline in `soul-lint` (retired there by NIM-944).
- **A `block:` child reading a sibling's register is already a fail-closed refusal** —
  `within_block_register_dependency` (`shared/config/passage.go:94-105`).
- **The one spelling invisible to all of the above was closed at compile by NIM-909** — choosing a
  register's name by index is refused by `shared/cel.guardRegisterNameByIndex`
  (`shared/cel/register_index.go:81-111`) and offline as `register_index_form`
  (`soul-lint/internal/validate/register_index.go:61`).

So the class was real, and it was **patchable point by point** — one of those patches landed the
same day this ADR was written. The linear model does not cure it and is not offered as a cure. What
it removes is the **dependence on a derived boundary**: one dot-form regex was read by four
subsystems at once — the Passage graph, the cross-reference existence check (which is `soul-lint`'s
`unknown_register_reference`), the Soul's `async:` join set (`soul/internal/runtime`
`flowSet.addNames`; on the parse tree since NIM-929, the regex kept only for text that does not
parse) and `config.IsStaticPredicate` (`shared/config/task_refs.go:433`); the four are
named together at `shared/cel/register_index.go:10-47`.

This ADR retires the Passage graph — **one of those four readers** — and with `when:` evaluated on
the Keeper (decision 4), the static/dynamic classification stops deciding where a module task's
`when:` runs. **Nothing text-derived is added in their place**: where a grouped child's `when:` runs
is decided by whether its substituted form still has an open reference, read off the AST. An earlier revision admitted "minus one, plus one": a
carried flow-control context needed its own read-set from predicate text. Fork 2's answer — no
register on the wire — removes that reader.

## Decision

**Scenario dispatch is linear. Step *k* is dispatched to any host only after every dispatch of
steps < *k* has completed on all of the hosts it went to** — a step as decision 1 defines it.

That is the whole invariant, and it is unconditional: it holds for a task that goes to no host at
all, because the ordering constraint is on *k* waiting, not on *k−1* producing a barrier. Nothing
about the ordering is derived from register text.

⚠ **Read it as a happens-before over dispatches, not as "every host ran every task in order".** The
roster is not constant across a run: it is re-resolved at a refresh boundary
(`keeper/internal/scenario/run.go:808-834`), so a host admitted mid-run has executed none of the
earlier tasks, and under `on_failure: host` (§Fail-stop) a host leaves the roster the same way.
The invariant is about the hosts a task *went to* — it does not promise a constant fleet.

**Whether a task runs can depend on another task's output; whether it waits cannot.** A `where:`
fed by a probe still decides which hosts — or whether any host — receives task *k*. What no longer
depends on anything is the fact that *k* comes after *k−1*.

1. **One step per `ApplyRequest`.** A *step* is a top-level task other than a default block — an
   applier, a `loop:` fan-out and a block rolled as one group each count as one — or a child of a
   default block (decision 2), at any depth, unless it sits inside an applier's destiny or a group
   rolled as one. A default block is not a step itself; its children are. One per message is a
   consequence, not a choice: a host runs a message to its end and cannot be interrupted, so
   ordering between step *i* and *i+1* requires *i+1* to be a separate dispatch.

2. **An applier and a `loop:` fan-out are one task; a `block:` is one task only when asked to be**
   (fork 3, user, 2026-10-01). An applier (`apply: destiny:`) and a `loop:` fan-out travel whole, in
   one message — a destiny is unchanged (decision 6) and a loop is one task repeated; this is already
   how the plan is stamped (`stampPassage`, `keeper/internal/render/pipeline.go:2045-2052`). **A
   `block:` is, by default, an overlay of defaults over its children**: each child is its own step,
   with its own dispatch and its own barrier, and inherits from the block by the ordinary rule — the
   lower level overrides. **With `serial_scope: block` and a `serial:`, a block rolls as one
   group**: one message per host and one wave loop, so `serial:` on it widens the whole group at
   once — the canary. The default was re-confirmed after the tree's own canary was shown to change
   meaning under it (fork 3); that block gets `serial_scope: block`.

3. **A keeper-side task — `on: keeper`, or a keeper-side core address such as `core.state.*`
   ([ADR-0087](0087-task-side-derived-from-module-address.md)) — runs at its position in the order,
   and that moves it.** It is not an
   exception to the invariant. Today keeper-side tasks execute locally on the Keeper **strictly
   before the host dispatch of their own Passage** (`keeper/internal/scenario/run.go:658-669`,
   called at `:973` before `dispatchPassage` at `:1015`), so in a one-Passage scenario a keeper task
   written *after* host tasks runs *before* them. Under this ADR it runs where it is written.
   *Built (NIM-944):* a keeper-side task cannot sit in a block, so under linear dispatch it is a
   step of its own and the step loop runs it after the barrier of every step before it — "before
   its Passage's host dispatch" is the staged run's order only. A keeper step has no barrier of its
   own, so the loop now reads a cluster-wide Cancel after every step (`run.go:1023-1031`): a Cancel
   that flagged the row of a keeper step written last was read by nothing, and the run committed.
   `examples/service/long-runner/scenario/serial_waves/main.yml:24-33` is that shape: two
   `core.state.set` steps written after the waves run before them today and after them under this
   ADR — which is what its own description says it means ("state is committed ONCE after all
   waves"); a failed wave now leaves them unwritten. The scenario audit (item 6) lists every keeper
   task written after a host task. Task *k+1* waits for a keeper task *k* by construction — the
   loop is sequential and a keeper failure aborts before the next dispatch. What a keeper task does
   not get is a *cross-host barrier*, because it targets no host (`groupByHost` skips it,
   `keeper/internal/scenario/dispatch.go:962-968`); there is nothing for such a barrier to wait on.

4. **The Soul receives only rendered inputs; whatever depends on another message is settled on
   the Keeper** (user, 2026-10-01 — fork 2). The host needs its inputs, fully rendered, and nothing else:
   `params:` for a module, the rendered `input:` for a destiny.

   - **`when:` is evaluated on the Keeper**, per host, against that host's accumulated registers and
     its `soulprint.self`. A host where it is false is not sent the task. Five things follow; all but
     the last are new:
     - **The skip is still recorded.** A Soul-side skip reports SKIPPED with a register that reads
       `skipped: true` (`soul/internal/runtime/applyrunner.go:1472-1479`), and a later
       `register.X.changed` reads `false` from it. A task the Keeper never sends writes nothing, and
       that read would raise instead. The Keeper writes the SKIPPED event and register for that host
       itself — as it must for a host the requisite gate drops (`crosspassage.go:120-128`). Inside a
       group the child is sent with `when: false` instead of being left out, so the Soul writes the
       SKIPPED register in the message, where a sibling's open predicate can read it. That `false`
       holds on some hosts and not others, so it travels in the per-host overlay
       ([predicate-rendering.md §5](../scenario/predicate-rendering.md)).
     - **An evaluation error fails *that host's task*,** judged by `on_failure`, not the whole run —
       inside a block rolled as one group, where the child cannot fail alone, the group on that host,
       with the records [predicate-rendering.md §1](../scenario/predicate-rendering.md) gives it.
       Today every render error leaves the whole pass and the stage loop aborts the run
       (`keeper/internal/scenario/run.go:894-898`), as a failing static `when:` does
       (`render.evalStaticWhen`; `shared/cel/register_index.go:32-36`). This needs error capture per
       task and host, and a Keeper-written FAILED event, `apply_runs` row and audit record.
     - **Flow control is evaluated for the active task only.** Already-active tasks are re-rendered at
       every step (§Price); re-evaluating a finished task's `when:` against registers written since
       could fail the run over a task that has already run.
     - **A decided `when:` is cleared from the wire.** Under staged dispatch a host where it holds
       still receives `RenderedTask.When` (`keeper/internal/render/prototask.go:126`; a linear run
       blanks it there since NIM-943): the Soul would evaluate
       `register.<earlier>` against registers it no longer has and fail with
       `flowcontrol.when_error`, and the field alone makes the Keeper demand `CapabilityFlowControl`
       (`keeper/internal/scenario/soulcompat.go:99-101`). A decided `when:` is blanked — except inside
       a group, where a false one travels as `false` and an open one as its residual, both per host.
     - **The static-only rules stay in S1.** An applier's, a keeper task's and an `include:`'s `when:`
       must be static today (`guardApplierWhen` and `guardKeeperWhen`,
       `keeper/internal/render/pipeline.go:1973-1979`, `:2006-2012`;
       `shared/config/include_expand.go:451-467`); whether the Keeper-side evaluator lifts them is a
       later decision. `when: register.self.*` is refused at compile: the task has not run yet.

     So the static/dynamic classification, which NIM-909 found load-bearing and fragile, stops
     deciding where a module task's `when:` runs — and only that. The predicate language does not
     change: predicates compile in the flow-control environment (`cel.NewFlowControl`,
     `shared/cel/engine.go:204`), not the render one (`cel.New`, `:189`), so `compute.*`,
     `soulprint.hosts`, `vault()` and `generate_secret()` stay out of them.
   - **`onchanges:`/`onfail:`** are resolved by the cross-passage gate (decision 5).
   - **`changed_when:`, `failed_when:` and `retry.until:` are rendered on the Keeper and evaluated
     on the Soul** (user, 2026-10-01). They judge the module's result, so they cannot be *decided*
     before it runs — but everything in them except that result can be *rendered*. The Keeper
     resolves everything but `register.self` — another task's register, `vars`, `input`,
     `incarnation`, `soulprint.self` — under the contract below, and only `register.self` stays
     open. The author writes what they mean:

     ```yaml
     - name: verify replication
       module: core.exec.run
       failed_when: register.self.stdout != register.probe.stdout
     ```

     and the Soul receives `register.self.stdout != dyn("bob")`.

     **The rewrite owes an equivalence, not an algorithm**, and is specified in
     [docs/scenario/predicate-rendering.md](../scenario/predicate-rendering.md). For every value the
     open set can take, the predicate the Soul receives gives what the author's predicate gives
     against that host's full context; where the Keeper cannot guarantee that, the host's task —
     inside a group, the group on that host — fails before dispatch with a named code. Every trap
     the reviews found is a case in a differential test S1 must pass. **The open set is
     `register.self`, and inside a group also the group's own registers**, which the Soul
     accumulates inside the message, as a destiny does. Three things have to be built — absent
     when this was written, two of them since shipped ahead of the train in NIM-929 and the third,
     with the guard lift below, in NIM-943 for a run executing linearly — a per-host overlay for the predicates (params got one in NIM-908, `ParamsBySID`;
     predicates did not), Soul text guards that leave a substituted value alone, and masking at every site that
     echoes a predicate — and the two existing host-invariance guards on predicates are lifted. **The fallback** — typed variables instead of text — costs the readable
     form the user chose, and only the user takes it.

     **`flow_context` loses its purpose for scenario tasks**: with every `vars` / `input` /
     `incarnation` / `self` reference substituted, the snapshot NIM-813 had to narrow need not travel.

   So **no register crosses the wire**, and **FC-5 (`config.CrossPassageWhenGating`) retires**: every
   reference across a barrier is resolved on the Keeper, so there is nothing left for it to refuse;
   what stays open is inside one message, where FC-5 never looked. An earlier revision had the Keeper carry accumulated registers into every message, and a
   later one refused cross-task references in post-module predicates; both are withdrawn.

   **Inside a destiny nothing changes** (decision 6): its tasks travel in one message and the Soul
   keeps accumulating its own registers there, `when:` included.

5. **`onchanges:` needs no new mechanism — the existing Keeper-side path becomes the only path.**
   `crossPassageGate.applyGate` already resolves a requisite whose source is in an earlier Passage,
   per host, from the audit log's CHANGED sets (`keeper/internal/scenario/crosspassage.go:107-191`).
   Under this ADR every requisite source outside a group rolled as one is in an earlier message, so
   that branch runs on every
   dispatch instead of rarely — an applier source included, which it got wrong until NIM-931
   (§`onchanges:`). Inside a group, a child the gate drops on a host is sent there as `when: false`
   (decision 4), so its siblings still see its SKIPPED register. **`onfail:` is a different matter
   and is fork 1.**

6. **Destiny is unchanged.** Passages never existed inside a destiny and none are introduced;
   `vars.yml` and task params are already resolved per host.

7. **`async:` and `require:` outside a destiny are refused.**
   Neither has anything to act on when a message holds one task. ADR-0075's barriers are `require:`,
   an implicit register reference, and the end of the run — *"which is the end of THIS
   ApplyRequest"* (`soul/internal/runtime/applyrunner.go:350-353`); and a `require:` source in an
   earlier Passage is already encoded as the sentinel `-1`, which for a barrier reads *"nothing to
   wait for"* (`soul/internal/runtime/async.go:184-186`,
   `keeper/internal/render/prototask.go:116-122`). Inside a destiny both still work and ADR-0075
   applies unchanged (a block refuses `async:` today, `shared/config/scenario_task.go:1103-1105`). A key that stays legal and stops having an effect is the silence this ADR is about, so
   both become **refusals** — `async:` at top level, and `require:` anywhere outside a destiny:
   `async:` is already refused inside a block (`keeper/internal/render/block.go:710-711`), so a
   block child's `require:` would wait on nothing. **Shape (NIM-941, codes approved by the user
   2026-10-06):** `async_outside_destiny` and `require_outside_destiny`, both ERROR, refused on
   presence on every scenario task — top level, any block depth, a scenario's included body.
   In a scenario, `async:` on a block child is now refused at parse, not only by the render guard; where
   `async_on_apply_invalid`/`async_on_block_invalid`/`async_on_keeper_invalid` already refuses the
   key, that code is the one reported. Unconditional — not keyed to the execution model pinned to the run — since `soul-lint`
   cannot know it. NIM-906 keeps only the "do not wait" flag itself. **What this premise
   misses** (found in NIM-941's review, user kept the refusal 2026-10-07): a scenario `require:`
   could reach an applied destiny's `async:` tasks in the same message — one Passage today, a block
   rolled as one group under linear dispatch — by `require: all`, by a name an `async:` destiny
   task also registers, or written on an `apply:` task and merged into every task of the destiny
   (`keeper/internal/render/block.go:599-607`). The replacement is a `register:` on each `apply:`
   before the waiting point, and a `require:` inside the destiny for a wait among its own tasks: the
   terminal carries every task of the destiny in `aggregate_of`, which the Soul awaits as a gate and
   fail-stops on (`soul/internal/runtime/applyrunner.go:498-510`). Nor does one step mean one task:
   a `loop:` + `async: true` fan-out is one step whose iterations run alongside each other
   (decision 2); refused with the rest, that loop moves into a destiny.
   `examples/service/example-cloud-bootstrap/scenario/existing-vm/main.yml` used a top-level
   `require:` on two keeper-side steps and was migrated with them; the service repositories are
   NIM-940's.

8. **A host's failure ends the run by default, and that default is configurable** (user, 2026-09-30).
   Ending the run stays the behaviour of a scenario that declares nothing, because a barrier sits after a
   step that may already have changed the system. The value names the blast radius — `scenario`
   (default, Ansible's `any_errors_fatal: true`) or `host` — with a threshold of failed hosts beside
   it, and lives in the scenario's `settings:` block ([docs/scenario/settings.md](../scenario/settings.md)).
   **No operator override exists in either direction.** It is **not** ADR-043's Leg policy. See
   §Fail-stop for why the field must be separate and what `host` costs.

### ⚠ What this decision does NOT do

- **It closes nothing inside a group that still travels whole.** An applier, a destiny and a block
  rolled as a group carry several tasks in one message, and a child's render-time keys are still
  resolved before its sibling has run. **`within_block_register_dependency` stays** for a block
  rolled as a group — it is the only thing between an author and that shape in a render-time key;
  a flow-control key there is evaluated on the Soul, inside the message (decision 4). A default block
  needs it no longer: its children sit on opposite sides of a barrier.
- **It does not remove the text scan.** It retires one of the four readers (§Context); the
  cross-reference existence check, the Soul's `async:` join set (on the parse tree since NIM-929, the
  regex only as its fallback) and `config.IsStaticPredicate` stay —
  the last no longer decides where a module task's `when:` runs, but still holds the keys that must
  be static (decision 4).
- **It is not the fix for the empty-register class.** That is §Alternatives B, and it is orthogonal
  to dispatch.
- **It does not make the file self-contained.** The order is over the top-level list **after
  `include:` expansion**, which is as invisible in the file the author opens as the Passage count
  was. What changes is that one rule (read the tasks in order) replaces two (expand, then
  stratify).

> **On counting barriers.** An earlier draft claimed "the number of barriers equals the number of
> top-level tasks". That is false: five shapes are dispatched to no host and so produce no
> cross-host barrier — an `assert:` (`keeper/internal/render/pipeline.go:227-237`), a conditionally
> dropped `include:` group (`:209-214`), a statically-false `when:` (`:1064-1065` →
> `dispatch.go:962-980` → `:94-106`), a keeper-side task (decision 3), and a task whose `where:` or
> requisite gate leaves it with no host (`crosspassage.go:120-128`). Under linear dispatch each of
> these steps still runs a barrier that re-checks its owner's `tolerate` (NIM-948), though none of
> the run's hosts is sent anything. Under the happens-before
> formulation this is a fact about barrier *count*, not about ordering, and it costs the decision
> nothing: step *k+1* still waits for step *k−1*'s barrier.

## Price, stated in advance

- **A 12-step scenario becomes 12 messages and 12 barriers**, each waiting on the slowest host.
  Wall clock moves from `max_h(Σ_t)` to `Σ_t(max_h)`: tail latency stops being paid once per run and
  starts being paid per step.
- **The Soul's per-message overhead is now paid T times** — request setup, module resolution and
  loading, locks, the run span. Once per run before; once per step now. The benchmark
  (§Before merge) must measure this separately from the round trip, because for a fleet of cheap
  tasks it may dominate.
- **Re-render grows quadratically.** The stage loop re-renders before each step
  (`keeper/internal/scenario/run.go:894-898`). It does **not** render the whole plan — a task in a
  future Passage is emitted as a placeholder and its register-dependent keys are not resolved
  (`keeper/internal/render/pipeline.go:265-281`) — but every *already-active* task is rendered
  again, so task 0 is fully rendered T times, `${ vault() }` included. See §Consequences for why
  that is a correctness question and not only a cost.
- **One Keeper↔Soul round trip per step per host**, where there was one per Passage.
- **Write amplification**: one `apply_runs` row per step per host, where a whole Passage shared one
  — and that row is public.
- **Crash recovery for scenario runs is lost until the Acolyte path is restored.** The work-queue
  dispatch of [ADR-027](0027-apply-work-queue.md) is gated off for staged runs —
  `if r.acolyteEnabled && !hasSerialTask(scn) && !staged && spec.executionModel != ExecutionLinear` (`keeper/internal/scenario/run.go:698`),
  with `staged := passage.Count > 1` (`:465`). Under this ADR every scenario with two or more
  steps is staged — a single default block with two children included — so `dispatchPlanned`, claim/fencing and `ReclaimApplyRuns` become
  unreachable and every run inherits the limitation `:540-542` already names.
- **A straggler now slows every step**, and `serial:` multiplies the wave count.

**Measured (NIM-950, 2026-10-08).** The list above was written before any measurement; the
numbers below are one. One scenario of twelve cheap top-level tasks (`core.exec.run true`, two of
them reading a secret through `vault()`), both models on one commit (`release/NIM-926` after
NIM-946, plus NIM-950's `scenario.execution_model` key), real Souls in Docker on one 24-CPU
machine, a push host over SSH, ten runs per cell. Method, harness and every derivation:
[dev/bench/linear-dispatch](../../dev/bench/linear-dispatch/README.md). Medians:

| | staged | linear |
|---|---|---|
| run, 1 / 4 / 8 hosts, inline (`acolytes: 0`) | 227 / 239 / 269 ms | 2534 / 2658 / 2812 ms |
| run, 1 / 4 / 8 hosts, work-queue (`acolytes: 2`) | 222 / 230 / 438¹ ms | 2521 / 2559 / 2610 ms |
| run, push host, +0 / +20 ms delay | 423 / 623 ms | 4925² / 7322 ms |
| messages per host | 1 | 12 |

¹ its one step took 231 ms (median), so 6 of its 10 runs waited two ticks of the barrier poll
(438–470 ms) and 4 one tick (230–236 ms); the same cell at +20 ms delay ran 231 ms.
² see the push bullet: the same inline path ran 3243 ms in the `acolytes: 2` Keeper start.

- **The ~200 ms gap between two steps is the barrier's polling tick, not work.** A step takes 9 ms
  (1 host) to 30 ms (8 hosts); of the ~200 ms that pass before the next step is sent, the Keeper's
  own work is 10–13 ms and the rest is the wait for the next 200 ms tick of the barrier poll
  (`keeper/internal/scenario/scenario.go` `defaultPollInterval`, `failure_policy.go` `waitStep`).
  So the tick, not the round trip, sets the price: +20 ms on every message leaves the linear run
  unchanged (2536 against 2534 ms, 1 host), and any step shorter than a tick costs a tick. NIM-984
  removed the tick from the path — see the measurement after this list.
- **The Soul's per-message overhead is small.** Its `Run` of a one-task message takes 1 ms, against
  13–20 ms for one twelve-task message across all staged cells (1.05–1.7 ms per task). Around
  `Run` — the network (~1.5 ms), the Soul's request handling and the Keeper storing the result —
  7–8 ms per message, against 30–49 ms for the one staged message (inline, no added delay).
- **Re-render and Vault.** Inline: 12 render passes against 1, 9–24 ms of render per run against
  2–5 ms (1–8 hosts), and **one** Vault read per run in both models — the run memo (NIM-934) holds.
  On the work-queue every claimed host renders its step with a read of its own: 24 / 60 / 108
  renders and **11 / 41 / 81 Vault reads** per run for 1 / 4 / 8 hosts against 2 / 5 / 9 staged,
  i.e. hosts × the steps from the first secret-reading task on, plus the run's own read.
- **A push host pays an SSH session per step** — 12 sessions per run against 1, each ~220 ms (dial,
  handshake, delivery check, `soul apply`; 518 ms at +20 ms delay, about 15 round trips). Its
  steps ran 201–284 ms, and 108 of 110 transitions waited two ticks: the run took 4.9 s against
  0.42 s. In the `acolytes: 2` Keeper start, where a linear push step also goes inline (below),
  the steps landed around 200 ms, most were seen on the first tick, and the run took 3.2 s — one
  start per configuration cannot tell that start's variance from the setting, and the harness now
  measures push at `acolytes: 0` only. At +20 ms most steps wait three ticks: 7.3 s against
  0.62 s. The session sets the step; the tick decides whether it costs 200, 400 or 600 ms.
- **Push hosts and the work-queue.** On an `acolytes>0` Keeper a linear step with a push host stays
  inline, whole (`dispatch.go` `queuesStep`); a one-Passage staged run with one is refused before
  dispatch (`dispatch_failed`, `run.go`), while a multi-Passage or `serial:` staged run sends it
  inline.

Not measured: real tasks, which dilute every fixed cost above; how re-render grows with the step
count (always 12 here); HA; a fleet beyond 8 hosts or spread over machines (here every host shares
the Keeper's CPU). The stand's Postgres clock (WSL2) stepped back by ~110–125 ms inside 24 of the
runs: medians absorb it, p90 and single gaps do not.

**Measured again with the barrier waking on the write (NIM-984, 2026-10-10).** A barrier now reads
`apply_runs` when a row of its run is written or the run is cancelled — on whichever Keeper
instance, through the Redis channel `apply:wake` — and polls only as a fallback, once a second
(`keeper/internal/applywake`, `scenario.go` `defaultBarrierFallback`); the two writes that publish
nothing — a row the WardRoster sweep orphans, one `trySend` takes back only to re-send — wait for it. The same harness on the same
stand, on `release/NIM-926` at NIM-951 plus the NIM-984 change. Medians, NIM-950 → NIM-984:

| | staged | linear |
|---|---|---|
| run, 1 / 4 / 8 hosts, inline (`acolytes: 0`) | 227 / 239 / 269 → 75 / 99 / 130 ms | 2534 / 2658 / 2812 → 239 / 382 / 575 ms |
| run, 1 / 4 / 8 hosts, work-queue (`acolytes: 2`) | 222 / 230 / 438 → 83 / 143 / 240 ms | 2521 / 2559 / 2610 → 420 / 1074 / 2002 ms |
| run, push host, +0 / +20 ms delay | 423 / 623 → 299 / 591 ms | 4925 / 7322 → 2786 / 6463 ms |
| gap between two linear steps | – | 181–204 → 8–12 ms |

- **The gap is the Keeper's own work now**: 8–12 ms in every linear cell, against the 10–13 ms the
  first measurement estimated under the tick. Linear inline costs ×3.2 / ×3.9 / ×4.4 of staged at
  1 / 4 / 8 hosts, where it cost ×10–11.
- **The step sets the price now.** Inline, a step is the round trip and the slowest host, 9–35 ms.
  On the work-queue the step grows with the fleet — 25 / 79 / 154 ms at 1 / 4 / 8 hosts, each
  claimed host rendering its step with a Vault read of its own (above) — and at 8 hosts the run is
  2.0 s against 0.24 s staged. A push host's step is its SSH session, ~223 ms (530 ms at +20 ms),
  and twelve of them are its run.
- **Staged gains too**: its one barrier stopped waiting for a tick, 30–200 ms off a run.

Not measured: the wake crossing Keeper instances — one Keeper here; the cross-instance path is held
by a guard test (`TestIntegration_BarrierEndsOnTheWakeNotOnTheFallbackPoll`), not timed.

## Consequences

### ★ Fail-stop becomes fleet-wide — the largest behavioural change for existing scenarios

Today most scenarios are a single Passage: every host gets one message and they run independently.
A host that fails at task 3 does not stop host B from reaching task 12 — B's message was already
dispatched and B runs it to the end. The failure is observed at the barrier, after the fact.

Under this ADR the barrier sits between every pair of tasks, and one host's failure ends the run
there: `classify` returns an error on any host in `failed`/`cancelled`/`orphaned`
(`keeper/internal/scenario/dispatch.go:875-887`), `waitBarrier` propagates it (`:745-748`, surfaced to the stage loop at `:221-223`), and
`run()` aborts (`keeper/internal/scenario/run.go:1015-1018`). **Tasks 4..12 are then dispatched to
nobody — including the hosts that were healthy** — bar the rescues of fork 1, which the run walks
on to before it ends (NIM-951).

This is not a new mechanism; it is today's barrier semantics applied T times instead of once. The
precedent is `serial:`, which already behaves this way within a Passage: *"First host, finally-failed
after the inherited `retry:` is exhausted, stops rolling: subsequent waves will not start"*
([orchestration.md §2.2.1](../scenario/orchestration.md); the per-wave barrier at
`dispatch.go:215-223`). What changes is that the blast radius of one host's failure grows from "its
own remaining tasks" to "the whole fleet's remaining tasks", for every scenario, not only those
using `serial:`. *Since the [NIM-993 amendment](#amendment-2026-10-14-nim-993-a-rescue-that-succeeds-handles-the-failure),
a failure that a later rescue names stops neither: it is held until its rescues have run, in a
`serial:` step too.*

**Decision (user, 2026-09-30): a host's failure ends the whole run by default, and the behaviour is
configurable from the scenario.** A scenario that declares nothing keeps the semantics described above, because
fail-closed is the safe default after a step that may already have changed the system. What is new
is that it stops being the *only* behaviour.

★ **That default is NEW, and it is the opposite of what the same words mean one layer up.** An
earlier revision of this text claimed the default was inherited from
[ADR-043](0043-voyage.md). It is not, and the claim came from a dead row: the only place in the tree
that documents `abort` as a default is `wave.on_failure` in
[naming-rules.md](../naming-rules.md) — a **Tide** invocation key, and Tide was removed by ADR-043.
ADR-043 itself fixes the *values* and states no default; every live surface resolves an unset policy
to `continue` — `voyage.ResolveFailThreshold` returns 0, "no threshold", unless the policy is
explicitly `abort` (`keeper/internal/voyage/voyage.go:122-130`), the wire type documents
`"abort | continue (default)"` (`shared/api/wire/voyage.go:167`), and the CLI flag says the same
(`soulctl/internal/cmd/run_scenario.go:133-134`).

A Voyage decides whether to start the *next incarnation*, where running on is usually right. A task
barrier decides whether to keep changing *this* incarnation after part of it failed, where stopping
is usually right. Same word, different question, opposite answer.

★ **So the value says what the failure KILLS, not what the orchestrator should do** (user,
2026-09-30). `abort`/`continue` is an instruction to the engine; `host`/`scenario` is a statement
about blast radius, which is what an author actually reasons about — and it is the vocabulary this
ADR adopts:

| Value | What a host's failure ends |
|---|---|
| `scenario` | **The default.** The run ends at the barrier where the host failed; no later task is dispatched to anybody but the rescues of fork 1. *Since the NIM-993 amendment, a failure that a later rescue names is held until its rescues have run, and it ends the run only if none of them handles it.* |
| `host` | Only that host. It leaves the run; the remaining tasks go to the rest. *Since the NIM-993 amendment, a failure that a later rescue names is held, and the host leaves only if none of its rescues handles it.* |

A threshold sits beside it, `tolerate`: with radius `host`, the run still ends once **more than** N
hosts — or N% of the run's hosts — have failed in total. Together the two reproduce Ansible's
`any_errors_fatal` and `max_fail_percentage` (user, 2026-10-01), so they read the way an operator
already expects. Neither the unit nor the arithmetic is [ADR-043](0043-voyage.md)'s: the unit is a
failed host of this run, not a run unit, and "more than N" is **off by one** from `max_failures: N`,
which stops *at* the N-th. The full rules are in [docs/scenario/settings.md](../scenario/settings.md).

★ **Choosing this vocabulary also removes the collision, not just the confusion.** `RunSpec`
(`keeper/internal/scenario/scenario.go:238-315`) carried no failure policy, and carries none fed at run
time since NIM-948 either — its `stepSettings` are the scenario's own `settings:`, merged per step; ADR-043's keys live
on the `voyages` row and are read only to decide whether to start the next Leg
(`keeper/internal/voyageorch/scenario.go:235`). Had the task barrier read that field with ADR-043's
*values*, every existing `kind=scenario` Voyage — which defaults to `continue` — would have silently
switched its within-scenario barrier to drop-and-continue. **The task-barrier policy is therefore a
separate field, declared in the scenario and fed by nothing at run time** (fork 4), and its value
vocabulary is disjoint from ADR-043's, so the two cannot be confused even by eye.

**Three terminal statuses, and they are not interchangeable.** `classify` ends the run on `failed`,
`cancelled` *and* `orphaned` today (`keeper/internal/scenario/dispatch.go:875-887`):

- `failed` is a host failure and is what the policy governs. So is `orphaned`: the claim marks a
  queued step's row `dispatched` ([ADR-027 amend (q)](0027-apply-work-queue.md), NIM-961), and since
  NIM-949 an inline step's row is written `dispatched` with the send, so the `WardRoster` sweep
  orphans it on either path when the host's Soul reconnects without the step; a dead host is caught
  by `soul_timeout`, as `failed` `soul_unavailable` ([settings.md §2.2](../scenario/settings.md)).
- `cancelled` **ends the run regardless of the policy.** An operator's cancel is an instruction, not
  a failure, and "keep dispatching because cancellation is not counted as a failure" would be
  absurd. ADR-043 excludes `cancelled` from its failure *count* for a different purpose and that
  exclusion does not transfer.
- `no_match` does not arise here. The inline path gives a non-targeted host no row at all. On the
  work-queue path the run goroutine writes a step row only for a host it sends to, and a claim
  whose render differs refuses that host as `failed` rather than closing it `no_match`
  ([ADR-027 amend (q)](0027-apply-work-queue.md), NIM-961). The claim's NIM-56 branch
  (`keeper/internal/scenario/claim.go:140`) closes a host in status `destroyed` as `no_match`, but
  nothing has written that status since NIM-761.

⚠ **What `on_failure: host` costs, stated so it is chosen with open eyes.** The host set shrinks mid-run, so
`serial:` widths change under the author's feet between tasks (`soulprint.hosts` does not: a dropped
host stays listed with `dropped: true`, NIM-948); a host that
failed at task 3 is simply absent from task 7's `where:` result, with nothing at that point saying
why; and the run needs a terminal status meaning "finished without some hosts", which the run
status does not have — `applyrun.RunStatus` (`keeper/internal/applyrun/runsview.go:21-46`), exposed
as the inline enum `applying,success,failed,cancelled` (`shared/api/wire/incarnation.go:182`,
`:231`). **Decided (user, 2026-10-02): such a run ends `partial_failed`**, the value a Voyage and
the push views already use for "some failed, the rest finished" (`shared/api/wire/enums.go:114`,
`:126`, `:162`); it joins the run-status enum, which is a public-schema change (item 4). None of
this argues against offering `host` — it argues for `scenario` being the default, which it is.

This also settles what §Provenance asks: under `on_failure: host` the dropped host's remaining tasks are a
real outcome and need a record, so "not reached" synthesis ships with the policy, not separately.

### Top-level `onfail:` would become unreachable — fork 1, answered

Today a rescue works because the failure and the rescue travel in the same message: the Soul runs
the rescue tail itself after a fail-stop (`soul/internal/runtime/applyrunner.go:368-375`,
`:526-547`), and requisites are deliberately not passage-defining
(`keeper/internal/scenario/crosspassage.go:7-11`), so source and rescue stay together.

Under this ADR every top-level `onfail:` source is in an earlier message, and by the section above
its failure ends the run before the rescue message is built. The onfail call to `resolveKind`
(`crosspassage.go:149`) can only fire inside a run the barrier has already ended.

Under `on_failure: host` it did not reach the failed host either, before NIM-951: that host had
been dropped, and a dropped host received no later task. So the rescue missed its one target under
**both** radii.

This is not created by this ADR — cross-passage `onfail:` was already dead the same way before
NIM-951, and still is for a staged run; it is simply rare. This ADR would make it the only shape a top-level `onfail:` can take. **The user's answer
(fork 1): rescue is extended across barriers** — a failed host still receives the `onfail:` tasks
whose source failed on it — under `on_failure: host` after it has been dropped, under `scenario`
before the run ends. *Since the [NIM-993 amendment](#amendment-2026-10-14-nim-993-a-rescue-that-succeeds-handles-the-failure),
a failure that a later rescue names is held instead — not dropped, and not ending the run — until its
rescues have run.*

### `onchanges:` — already answered, and now on the only path

`remapRequisites` encodes "source not in this message" as `-1`, on which the Soul reports
`changed/failed = false` (`keeper/internal/render/prototask.go:228-236`). With one step per message
every requisite source outside a group rolled as one is outside the message, so on the sentinel path alone every `onchanges:`
would stop firing silently — the exact silence this ADR is escaping.

It does not reach that path. `applyGate` resolves the requisite Keeper-side first, and either strips
it from the wire (a source changed → consumer runs) or drops the consumer from the host slice
(`crosspassage.go:166-191`). A source that was SKIPPED, or filtered out by `where:` on that host,
correctly counts as not-changed — the set is CHANGED status from the audit log, not "a register row
exists" (`crosspassage.go:18-23`). A source inside a group keeps its own `plan_index`, which is the
audit key.

What changes is standing (for an applier source, also semantics, until NIM-931 shipped that change):
today this is the rare branch, skipped entirely for a single-Passage run; under this ADR it runs on
every dispatch.

⚠ **An applier as a requisite source never fires across a barrier.** The gate's sets are built from
an event's `status` (`keeper/internal/auditpg/changed_tasks.go:73-82`). An applier's register is
carried by a terminal `core.noop.run` task, whose status is the noop's own — OK, always
(`soul/internal/coremod/noop/noop.go:78`); the Soul overwrites only its `register_data` with the OR
over the destiny's children (`soul/internal/runtime/applyrunner.go:713-725`, `aggregateRegisterData`
at `:1536`). Inside one message that is enough, because the Soul gates on the register. The Keeper
gates on the status, so `onchanges: [<applier>]` resolved Keeper-side reads "not changed" every time;
and after a child's fail-stop the terminal is still emitted as OK, carrying the aggregate
(`:526-534`), so `onfail: [<applier>]` never sees FAILED either. Latent today — a requisite does not split a Passage, so the two rarely sit
on opposite sides of one — and universal under this ADR. The tree's own example breaks:
`examples/service/dragonfly/scenario/rotate_tls/main.yml` registers the applier as `tls_certs`
(`:155`) and gates its live-reload `CONFIG SET` steps on `onchanges: [tls_certs]` (`:215` on), so
the rotation would report success without the reload. **S1 resolves an applier source from its
children's statuses** — the Keeper knows them, they are the terminal's `aggregate_of` — with a guard
test in that shape (item 16). **Shipped ahead of the train in NIM-931**, so the Keeper-side failure
described from the ⚠ to here is the code before it: `newCrossPassageGate` reads `aggregate_of`, guarded by
`TestCrossPassageGate_ApplierSourceResolvedByChildren` and
`TestIntegration_CrossPassageOnChanges_ApplierSource`.

⚠ **And one existing refusal goes from rare to universal.** A Keeper with no `AuditReader` already
**refuses** a staged run carrying a cross-passage requisite — `cross_passage_requisite_unsupported`
(`keeper/internal/scenario/run.go:569-583`, detector `shared/config/passage.go:467-495`). That is
loud and right. But the detector will now fire on *every* `onchanges:`/`onfail:`, so an audit log
becomes an operational prerequisite for any scenario with a requisite. *Shipped (NIM-944):* under
linear dispatch the detector is `config.CrossStepRequisite` (`shared/config/passage_linear.go`),
which sees a link between two children of a default block — the staged one reads the block as one
entry, and without an audit log the reload would have been skipped in silence. And the refusal
alone was not the prerequisite: every production Keeper has a reader, while `audit.enabled: false`
suppressed the `task.executed` rows it reads, so every consumer read "not changed". Those rows are
now written with the audit off too ([ADR-022(i)](0022-audit-pipeline.md)).

### `passage` becomes the step's index, not a dispatch counter

The column cannot be deleted: the proto field is only-add (`proto/keeper/v1/apply.proto:300`, echoed
at `:403` and `:475`), it is in the primary key `apply_runs (apply_id, sid, passage)` and the
foreign key from `apply_task_register` (migration `078_add_apply_runs_passage.up.sql`), in the audit
record (`shared/audit/task_executed.go:56-61`), in the register accumulation query's
`WHERE passage < $2` (`keeper/internal/applyrun/taskregister.go:129-142`), and on the **public**
surface — `json:"passage"` in the API wire types (`shared/api/wire/incarnation.go:197,266`), a
printed column in `soulctl incarnation` (`soulctl/internal/cmd/incarnation.go:302`) and the
documented projection *"A host has one row per unit it was sent, keyed by Passage"* (NIM-936)
(`keeper/internal/applyrun/runsview.go:76-79`).

**Decision: `passage` becomes the 0-based index of the step (decision 1) in the plan after
`ExpandIncludes`.** Not a counter of dispatched messages: that would be data-dependent — `where:`,
a statically-false `when:` and a conditionally dropped `include:` all change which tasks dispatch,
so the same task in two runs of the *same* scenario version would get different numbers, and two
runs could not be compared at all. As an index it is stable for a given scenario version regardless
of what the data does, and the values actually present in `apply_runs` simply have **holes** where a
task dispatched to nobody.

Holes are safe: `WHERE passage < $2` is a range predicate (`taskregister.go:129-142`), and the
public readers display the value rather than iterating it. Nothing assumes contiguity or a bound,
and nothing compares the value across hosts — that is the property that makes the re-definition
work.

★ **Nothing needs reserving — an earlier revision of this text invented that obligation by
confusing two counters.** The render loop ranges over the post-`ExpandIncludes` list with `i` and
takes the passage from that position (`keeper/internal/render/pipeline.go:203`); the `continue`s
for a dropped `include:` group (`:217`) and an `assert:` (`:240`) skip `idx++`, and `idx` counts
**RenderedTasks**, not top-level entries. `i` always advances. `config.Passage.TaskPassage` is
already indexed by that position (`shared/config/passage.go:62-69`), so `passage` is input-independent
as it stands, and the future-task placeholder gate — `passage > in.ActivePassage`
(`pipeline.go:276`) — is **already keyed to the top-level ordinal**. This decision redefines what
goes *into* `TaskPassage`, not how the loop counts — with one exception.

⚠ **A default block's children each need their own number, and that is a re-keying.** Today every
descendant of a top-level task takes its passage — *"block is an atomic Passage unit"*
(`stampPassage`, `keeper/internal/render/pipeline.go:2040-2052`). A message is selected by
`t.Passage == p` (`tasksForPassage`, `keeper/internal/scenario/dispatch.go:932-940`), the barrier
filters host rows by it (`classifyExcept`, `:845`), `apply_runs` is keyed `(apply_id, sid, passage)`
(migration `078_add_apply_runs_passage.up.sql`), and registers load with `passage < $2`
(`taskregister.go:129-142`). With one number shared, a default block's children would travel in one
message or, sent separately, collide on that key, and none could read its sibling's register. So
the index counts steps, each child of a default block takes one, and `stampPassage` and
`TaskPassage` are re-keyed to it — still from the plan alone, since a block's mode is written in
the file. **The placeholder gate is also applied inside block rendering**: today the gate runs once per
top-level entry (`keeper/internal/render/pipeline.go:203`, `:276`) and `renderBlockTask` then
renders every child in the same pass (`:419`), each child's own `where:` included
(`blockChildTargets`, `keeper/internal/render/block.go:491-502`). So while child *k* is active, child *k+1* would be
rendered too and a read of *k*'s register would raise. The gate and its placeholder are applied per
child as well, in `walkBlockChildren` (`block.go:335-341`), before a child's targets are resolved
(`:351-355`, `:389`); the top-level gate stays, keyed to the block's first step, so the block's own
`where:` (`keeper/internal/render/pipeline.go:306-307`) is not resolved early either. And fork 3's rule
that a default block's `where:` and `run_once:` are resolved once, at its first child, and held is
new too: today both are re-resolved on every pass. All of it lands in S1.

**Bonus, worth stating:** an index computed from the plan needs no run state, so resuming a run
after a Keeper redeploy does not have to recount anything.

**What it does not buy:** stability *across scenario versions*. Inserting one `include:` shifts
every later index, exactly as it shifted every Passage level under ADR-056. Comparing two runs of
different versions task by task needs the task's `id:` (`shared/config/scenario_task.go:36`) or its
`register:` name.

### Rendering is already bounded by the ordinal; the `vault()` memo scope is the S1 item

An earlier draft said the stage loop "renders the whole plan before each step", which contradicted
this ADR's own claim that an absent register raises. The code does neither: a task whose Passage is
still in the future is emitted as a **placeholder** — correct `Index`, `Register`, `ID` and Passage,
params and targets not computed — precisely so its register-dependent keys are never evaluated
against an empty register (`keeper/internal/render/pipeline.go:265-281`, contract at
`keeper/internal/render/render.go:269-280`). There is no contradiction, and the mechanism this ADR
needs already exists.

The gate compares `passage > in.ActivePassage`, so it works unchanged once `passage` is the step
index — it compares the active step against the task's own. **The render pipeline's load-bearing
changes are that numbering, the gate inside a default block and the held block targets**
(§`passage`): a default block's children each take their own number, each is a placeholder until
its step, and the block's `where:` and `run_once:` are resolved once.

⚠ **The `${ vault() }` exposure widens, and memoization already exists — its SCOPE is the change.**
(Done in NIM-934; this paragraph describes the code before it.)
`cel.ReadKVMemoized` memoizes a secret read for the duration of one render pass
(`shared/cel/vault.go:156-181`), bound on the context by `Pipeline.Render`
(`keeper/internal/render/pipeline.go:124,130`) and used by both the `vault:` phase and CEL
`vault()`. What changes is that a run now has T passes instead of N, and the memo does not span
them. Two consequences, and the second is the serious one:

- an already-active task's re-render is **discarded** — `tasksForPassage` keeps only the active
  ordinal (`keeper/internal/scenario/dispatch.go:932-940`) — so re-resolving task 0's secret at every
  later step is pure cost and Vault load, not a correctness problem;
- **a secret that disappears mid-run now fails the whole run, T times more often.** Every pass
  re-resolves every already-active task's secrets, and a failure there is `render_failed`
  (`keeper/internal/scenario/run.go:894-898`) — so a rotated-away path, or Vault being briefly
  unreachable at step *k*, kills a run over a secret that only a *completed* task ever needed.

Widening the memo from pass to run fixes both. It is S1 because the frequency, not the mechanism,
is what makes it matter.

### `serial:` — the width collapses between tasks, the wave count multiplies

Today the wave width is one per Passage: the minimum positive `serial:` among the tasks *of that
Passage* (`keeper/internal/scenario/dispatch.go:1010-1021`), aggregated for one documented reason —
*"tasks of one Passage are sent to the host in one message, so it is impossible to send different
Passage tasks in different waves"* ([orchestration.md §2.2.1](../scenario/orchestration.md)).

**Between steps the aggregation becomes the identity** and the "a narrow sibling narrows me" leak
disappears — including between the children of a default block, each of which is now a step with its
own width (fork 3). **Inside a block rolled as a group** the group has one width: the block's own.
(Today a block stamps its width on every child, so a module child's `serial:` is ignored and only a
nested block enters `effectiveSerialWidth`'s minimum — an earlier revision described that wrongly.)

**The wave count multiplies.** A run of T steps over M hosts at width N performs `T · ⌈M/N⌉`
sequential wave steps where it performed `⌈M/N⌉`.

⚠ **And a rolling idiom changes meaning without an error.** Two independent top-level tasks
`{change, check}` under `serial: 2` are one Passage today and roll as **one wave** — both steps on 2
hosts, then the next 2: a canary. Under this ADR `change` rolls the whole fleet, and only then does
`check` start — and so does a **default** block, whose children are separate steps. **The canary is
spelled with a block rolled as a group** (fork 3): one message, one wave loop, both steps on two hosts
before the next two. [orchestration.md §2.2.1](../scenario/orchestration.md) documents the plain
`block:` form as the idiom today; it must add `serial_scope: block`.

⛔ **Withdrawn before release** (user, 2026-10-14, NIM-998,
[Amendment 2026-10-14](#amendment-2026-10-14-nim-998-an-unwritten-serial_scope-is-the-standard-behaviour-and-nothing-refuses-it)):
the next paragraph records the refusals NIM-940 built and why; neither code is raised any more,
and the change of meaning it describes is announced in the CHANGELOG's upgrade notes instead.

**This one is not a lint to be added later.** By this ADR's own principle — a key that stays legal
and stops having its effect is a silence, and a silence gets a refusal — the shape needs a
**blocking diagnostic with an explicit opt-out** — group the pair into a block, move the `serial:` onto it and write its
`serial_scope`: `block` for a canary, `task` for steps that really are independent — in the shared
validator so the Keeper enforces it too, since service repositories are not linted. Only
host-dispatched tasks count: a keeper-side task cannot go into a block (`block_on_keeper_invalid`,
`shared/config/scenario_task.go:1036`), so a serial host task followed by keeper steps — as in
`long-runner/scenario/serial_waves` — has nothing to regroup and does not trigger it.
Not a plain refusal: `{change, check}` is textually indistinguishable from two genuinely independent
tasks, so the author must be able to say which they meant. **Its trigger is a transition rule**: it
fires where ADR-056's derivation — kept for this check alone — would put a host step carrying
`serial:` in one Passage with another host step, **in either order**, and it goes when the
derivation goes, after the service repositories are audited. *Decided (user, 2026-10-06, NIM-940):*
an earlier revision said "with a later host task", which misses half the set — today one wave loop
carries the whole Passage at its narrowest width (`keeper/internal/scenario/dispatch.go:1010-1021`),
so a step without `serial:` written *before* one with it rolls too, and stops rolling under this
ADR. The opt-out is the block rule's: a `serial_scope` the author wrote, either value, on the task,
above it or in the scenario's `settings:` — neither value stretches a wave over the next task, so a
canary is still spelled with a block. The codes are `serial_scope_implicit_across_tasks` here and
`serial_scope_implicit` for the block (fork 3), and `soul-lint` warns `group_child_key_unread`
(item 13). An earlier revision also said no example in this tree triggers it; `dragonfly/scenario/restart`
does — "Restart the current master last" shares the replicas' Passage, so today it restarts in its
SID's place among the width-1 waves rather than last — and its `serial_scope: block` answers both
refusals ([settings.md §3.1](../scenario/settings.md)). **And it must ship before linear
dispatch is turned on, not in a later slice** — the meaning changes in S1, so a diagnostic that arrives in S2 is
a window of silent regression. The audit of existing scenarios belongs in the same window.

### `retry:`/`until:` survives, because it never crosses a boundary

The retry loop is entirely inside one task inside one `Run`
(`soul/internal/runtime/applyrunner.go:836-937`), and `until:` is evaluated against `register.self`
merged into `registerByName` (`:965-966`). A barrier around the message does not touch it.

A **cross-task** register inside `until:` was the open edge: FC-5 excludes `retry.until` by design
(`shared/config/passage.go:580-589`), and on the Soul such a read dies as `flowcontrol.until_error`
→ task FAILED (`soul/internal/runtime/applyrunner.go:901-907`). Decision 4 closes it differently: the Keeper renders the reference into a
value before the task leaves, so `until:` arrives reading only `register.self`.

### Provenance: finer per step, thinner after a failure

Each dispatched step gets its own `apply_runs` row, status and events. A 12-task destiny
is still one task, so it still yields one row per host.

⚠ **But a failure now records less.** Today a fail-stop still emits a SKIPPED `TaskEvent` for every
remaining task of the message (`soul/internal/runtime/applyrunner.go:526-547`), so the audit says
what did not run. Under this ADR those tasks are never dispatched and nothing is written about them.
Under `on_failure: host` the dropped host's remaining tasks are a real outcome, so "not
reached" synthesis ships with the failure policy (§Fail-stop) rather than as separate work.

### State capture: a capture now waits for the steps before it

Whether the state a scenario captured before a failure is coherent stays the **author's**
responsibility: a `core.state.<verb>` capture writes at its own step
([ADR-0084](0084-explicit-state-capture.md)) and a later failure keeps what earlier captures
recorded. The engine records facts; it does not reason about which half of an author's state is
meaningful.

What changes is *when* a capture runs (decision 3). A capture is keeper-side, and today it runs
before the host dispatch of its Passage, so one written after a host step records its state even
when that step then fails. Under this ADR it runs where it is written — which is what ADR-0084 says
it does — so a failed host step now leaves a later capture unwritten. Fifteen example scenarios have
this shape, each with a capture after a host step that reads no register and so sits in the first
Passage today: `examples/service/coven-probe/scenario/{create,mark_a,mark_ab,mark_where}`,
`dragonfly/scenario/{create,rotate_tls}`, `hello-world/scenario/create`,
`long-runner/scenario/{create,serial_waves,stagger}`, `mongo/scenario/create`,
`monitoring/scenario/create`, `pilot-destiny/scenario/create`, `smoke-nginx/scenario/create` and
`smoke-nginx-live/scenario/create`. `state-verbs/scenario/per-host-capture` is not among them: its
capture reads a register and already runs after the hosts.

**Not only captures move.** Any step the derivation pulled into an earlier Passage now runs where it
is written. In `dragonfly/scenario/create` the `node-exporter` and `vector` appliers (tasks 9-10)
read no register, sit in the first Passage and run before tasks 3-8 today; under this ADR they run
after. The end state is the same when the run succeeds and differs when tasks 3-8 fail. A scan of
every example scenario found no other such move.

## Alternatives

### A. Keep ADR-056 and keep patching the derivation

Each leak has been closable on its own, and NIM-909 is the most recent instance. Cheapest of all,
keeps the fast path, and nothing in the code argues against it. **Rejected by the user for
obviousness**: the ordering stays derived, so reading a scenario still requires knowing the rule.

### B. Fail-closed register read — the root fix, and this ADR is not it

Make the `register` root raise for a task that is in the plan but has not executed yet,
distinguishing that from a task legitimately skipped by `when:`. That removes the class **under any
dispatch model** — at top level, inside a `block:`, inside a destiny — which is precisely where this
ADR does not reach. It costs no latency.

It is **not an alternative to this ADR** and is not in tension with it: it is worth its own ticket
whether or not the linear model lands.

### C. Move flow control Keeper-side — adopted for whatever crosses a barrier

**Adopted for whatever crosses a barrier** (fork 2). `when:` is decided before the module, and every
input it can read from an earlier message is in the Keeper's hands at render time, so it moves at no
cost. The other three cannot be *decided* there: `changed_when:`, `failed_when:` and `until:` judge
the module's result (`soul/internal/runtime/applyrunner.go:1180-1191`, `:901`), so deciding them on
the Keeper would cost a round trip each. They are rendered there instead (decision 4).

An earlier revision rejected the split as "two evaluation sites against two snapshots of the same
register data". With fork 2's answer that objection is gone: the line is clean — **what crosses a
barrier, the Keeper; what stays inside one message, the Soul.** There is no second snapshot, because
the Soul never sees a register from another message.

## Before merge

### Forks — the user's decisions

All four forks are answered, fork 3's flag name included, and so are the two decisions outside
them (user, 2026-10-02): the terminal status (item 4) and the Acolyte path (item 7).

**Fork 1 — ANSWERED (user, 2026-10-01): `onfail:` works everywhere, on a task and on a block —
option (b).** The rescue semantics one message has today is extended across barriers: once a host
fails, it receives no further *ordinary* task, but every `onfail:` task whose source failed on it is
still dispatched to it, at its own position in the order — under `on_failure: host` although the
host was dropped at the barrier where it failed (and counted by `tolerate` there), under `scenario`
before the run ends. Hosts where the source succeeded receive
nothing from it.

⛔ **Amended by [NIM-993](#amendment-2026-10-14-nim-993-a-rescue-that-succeeds-handles-the-failure)
(user, 2026-10-14).** A failure that a later rescue names is held, not judged. The host is not
dropped and is not counted by `tolerate` at the barrier where it failed, and under `scenario` the
run does not end there. A rescue that succeeds returns the host to the run. If none of its rescues
succeeds, the failure is judged by its source's radius. Of the three consequences below, the second and third are
superseded. The first holds for a failure that no rescue handles.

Half of the resolution exists: the cross-passage gate narrows an `onfail:` consumer to the hosts where
its source FAILED (`keeper/internal/scenario/crosspassage.go:149`). But it only *filters* the hosts
already in the consumer's plan — it cannot add one — and the run aborts at the barrier before the
rescue message is built. Removing the abort is not enough; the rescue still misses unless S1 also
builds these:

- **a dropped host stays targetable** by an `onfail:` consumer whose source failed on it — under
  `on_failure: host` it has left the roster;
- **an applier source** resolves from its children (§`onchanges:`) — shipped in NIM-931;
- **a failure with no task row** — `send_apply_failed` (a queued step's claim, `keeper/internal/scenario/claim.go:261-271`),
  an expired `soul_timeout`, a push transport error, an `orphaned` row — is written as the records
  a Soul's fail-stop leaves for the rest of a message, by plan index (user, 2026-10-08, NIM-951;
  this revision wrote FAILED for every task the message carried): the first task the host did not
  report is FAILED, every later one SKIPPED, not taken, and a task it reported keeps its record —
  a SKIPPED one, too, for a child the Keeper had decided to skip on that host.
  Otherwise no rescue, and no applier resolution (item 16), can see it. The
  radius it is judged by is still the barrier owner's ([settings.md §4](../scenario/settings.md));
- **once a failure ends the run, only rescue tasks are rendered.** Walking on to a rescue's position
  re-renders every task before it (§Price), and an ordinary task that will never be dispatched — say,
  one whose params read a field the failed source never produced — would abort the run before the
  rescue is reached.

An earlier revision priced
this option as "a barrier outcome depending on whether another task names it" — that overstated it:
the order of barriers does not change, only which hosts receive a rescue task, by the requisite
mechanism that is already there.

Three consequences. The run ends only after the last rescue's barrier, and only then closes its
failure list ([settings.md §4](../scenario/settings.md)). A successful rescue does **not** change the
terminal status, matching today's in-message rule that *"onfail tasks never undo the failure:
RunResult stays FAILED"* (`soul/internal/runtime/applyrunner.go:368-373`). And **a rescue's own
failure is judged like any task's** (user, 2026-10-01), by its own effective `on_failure`: under
`scenario` it is one more entry in the failure list — the host leaves two errors, the source's and
the rescue's — and under `host` the host, already out, stays out and the log records that it left.
*As built (NIM-951):* the drop is recorded unless that barrier ends a run still going on or carries
a Cancel, and a host the run had not dropped — its source failed under `scenario`, or at a barrier
that ended the run — is dropped there first ([settings.md §4](../scenario/settings.md)).
⚠ The radius is the rescue's own, not its source's: a source that failed under `host` with a rescue
left at the default `scenario` ends the run for every host if the rescue fails. An author who wants
the rescue local writes `on_failure: host` on it.

**Fork 2 — ANSWERED (user, 2026-10-01): nothing is carried; the host gets rendered inputs only.**
`when:` is evaluated on the Keeper; `changed_when:` / `failed_when:` / `until:` are rendered on the
Keeper with only `register.self` left open and evaluated on the Soul. Inside a group a sibling's
register stays open too, so a predicate reading one — `when:` included — is still evaluated on the
Soul. Both options of the earlier
fork — carry a narrowed set of registers, or carry them all — are withdrawn. See decision 4.

**Fork 3 — ANSWERED (user, 2026-10-01): a block is an overlay by default, and rolls as one group on
request.** Standard inheritance everywhere, `serial:` included: by default each child of a block is its
own step and overrides what it inherits, so a child's own `serial:` is honoured. A **canary** is a
different thing — "change and check two hosts, then the next two" — and it needs the steps to travel
together, so it is spelled with `serial_scope: block` in the block's `settings:`:

```yaml
- block:
    - name: upgrade redis
      module: core.pkg.installed
      params: { name: redis, version: "7.2" }
    - name: check redis answers
      module: core.exec.run
      params: { cmd: redis-cli, args: [ping] }
      failed_when: register.self.stdout != "PONG"
  serial: 2
  settings:
    serial_scope: block   # one wave carries the whole block: both steps on hosts 1-2, then 3-4;
                          # a failed check stops the roll before 3-4 are touched
```

With `serial_scope: task` — the default — `upgrade` rolls over all ten hosts in pairs and only then
does `check` run: if 7.2 is broken, all ten are upgraded before the first check. With `block`, at most
two are. What each mode means elsewhere:

- **A default block** has a barrier after each child, so a child may read a sibling's register and
  every barrier-kind setting on a child is read. More steps, so more round trips (§Price). Its
  `where:` and `run_once:` are resolved **once, at its first child**, and held for the rest:
  re-resolved at each child's step, a `where:` reading a register an earlier child changed, or a
  `run_once:` over a changed host set, would send the children to different hosts.
- **A block rolled as a group** — `serial_scope: block` with a `serial:` — is one message per host: its children cannot roll at different widths
  (a child's `serial:` is not read, `soul-lint` warns), and `within_block_register_dependency` still
  refuses a child reading a sibling's register.

**The key is `serial_scope: task | block`** (user, 2026-10-01). It says what one `serial:` wave
carries — one task, or the whole block. It lives in `settings:`, so a scenario can set it once for
every block; it is read only on a block that has `serial:`, and anywhere else it is unused, not an
error ([settings.md §2](../scenario/settings.md)).

**Re-confirmed after review round 7's evidence (user, 2026-10-01).** The tree's own canary was a plain
block: `examples/service/dragonfly/scenario/restart/main.yml:105-135` puts "restart dragonfly" and
"wait until the replica has resynced with master" in one block under `serial: 1`, so that rolling
does not move to the next replica until the current one has caught up. Under this default, without
`serial_scope: block`, the same file restarts every replica, one at a time, and only then checks any
of them. The user kept the default anyway: a block is first of all a way to group tasks, as in
Ansible, where a block's tasks run one after another across the hosts — the canary is what
`serial_scope: block` is for. What ships with that answer:

- **The dragonfly example gets `serial_scope: block`** — written by NIM-940, ahead of the switch,
  together with the refusal below.
- ⛔ *Withdrawn before release by the
  [Amendment 2026-10-14](#amendment-2026-10-14-nim-998-an-unwritten-serial_scope-is-the-standard-behaviour-and-nothing-refuses-it)
  (NIM-998); recorded as decided.* **A refusal with an explicit opt-out** on every block that has `serial:` — its own or inherited —
  and two or more host-dispatched tasks beneath it, counted through nested blocks on the list after `include:`
  expansion, where the flat checks already run (`shared/config/include_expand.go:163-166`): its
  effective `serial_scope` must
  come from a line the author wrote — on the block or on any level above it — either value. Written
  out, the default is a choice; left implicit, it is the silent change this ADR refuses. It lives in
  the shared validator, so the Keeper refuses such a scenario too: service repositories are not
  linted, and a `soul-lint`-only check would leave the change silent there. That example is the only
  such block among the examples. The `soul-lint` fixture
  `soul-lint/testdata/scenario-broken/scenario-within-block-peer-register.yml` has the same shape and
  gets `serial_scope: block`, or it stops guarding `within_block_register_dependency`. *(As built,
  `soul-lint` judges every block with the staged check, `soul-lint/internal/validate/stage.go`, so
  the fixture guards it either way; the `serial_scope: block` line changes the verdict only on the Keeper's linear run
  path.)* Service
  repositories are not checked and need the same audit.
- **S1 and S2 land together** (§Slices).

**Fork 4 — ANSWERED (user, 2026-09-30 / 2026-10-01): the scenario `settings:` block.** Specified in
full in [docs/scenario/settings.md](../scenario/settings.md): one schema at every level — scenario,
task, block, child — merged key by key like `vars:`; `on_failure: host | scenario` (default `scenario`)
and `tolerate`, which reproduce Ansible's `any_errors_fatal` and `max_fail_percentage`, `tolerate`
counting distinct failed hosts across the run; three distinct timeouts — `soul_timeout` (`1m`),
`task_timeout` (no limit) and `scenario_timeout` (`24h`, replacing today's 5-minute run timeout); an
append-only failure list in which hosts already working are allowed to finish; and no operator
override. It moved out of this ADR because it outgrew it: it is the scenario's configuration surface,
not a property of the barrier alone, and it is expected to grow. Its own open items are listed there.

### Work that must land with the change

1. **The dispatch loop and Keeper-side flow control merge atomically** — `when:` evaluated, and the
   three post-module predicates partially evaluated, on the Keeper — or behind a flag that is off
   until all are in: with the loop alone, a Soul would evaluate `register.X` against registers it no
   longer has.
2. **Widen the `${ vault() }` memo from a render pass to a run** (`shared/cel/vault.go:156-181`,
   bound through `WithVaultFence` at `keeper/internal/render/pipeline.go:129`). Not a new mechanism —
   a scope change — and it is correctness, not cost: without it a secret that disappears mid-run fails
   the whole run over a secret only a completed task needed (§Rendering). ⚠ **It needs invalidation on
   write**: keeper-side steps write Vault mid-run (`keeper/internal/coremod/vault/kvpresent.go:113`,
   `keeper/internal/coremod/state/state.go:648`), and a run-wide memo would hand a later task the
   value from before the write. **Done in NIM-934:** bound once in `Runner.run`; every KV write a
   step of the run makes, `core.cert.issued` included, forgets the written secret in
   `vault.Client.WriteKV`. A write outside the run (the Reaper's rotation, an operator) does not.
3. **The `settings:` block** ([docs/scenario/settings.md](../scenario/settings.md)): one type and one
   validator at every level; key-by-key merge; the placement rule by kind (task / barrier / block / run); the
   barrier **waiting for every dispatched host** instead of returning on the first failure, **not
   force-failing hosts still working**, and judging each by every failure it reported, kept as an
   append-only list; `soul_timeout` as Keeper's dead-host detector on the inline path;
   `scenario_timeout` replacing the 5-minute `defaultRunTimeout`. **No run-level override, and no field
   for one on `RunSpec`.** *Done in NIM-948:* the barrier half — waiting for every dispatched host,
   no force-failing, the append-only list, judging by every reported failure
   (`keeper/internal/scenario/failure_policy.go`, [settings.md §4](../scenario/settings.md)). The
   three timeouts are built in NIM-949: the detector reads the SID lease, a send waits for a stream,
   and the Soul relays a result finished while its stream was down.
4. **A terminal status meaning "finished without some hosts"**, required by `on_failure: host`. The
   run status is `applyrun.RunStatus` (`keeper/internal/applyrun/runsview.go:21-46`), on the wire
   `applying,success,failed,cancelled` (`shared/api/wire/incarnation.go:182`). **Decided (user,
   2026-10-02): the run ends `partial_failed`** — the value Voyages and push views already use for
   "some failed, the rest finished" — **the incarnation ends `error_locked`**, as after any run that
   did not apply everywhere, and **a Voyage counts the run as failed**. It needs the enum change, a
   `naming-rules.md` row and the companion web UI. *Done:* the enum, the row and the UI in NIM-936; the
   record that produces the value in NIM-948 — `apply_runs.dropped` (migration 122, user decision)
   on each dropped host's failed row, written when the run reaches its end without it.
5. **What `on_failure: host` needs besides the status.** First, a non-obligation stated so it is not
   re-litigated: **no RBAC gate** — with no run-time override, whoever may *merge* the scenario
   decides its radius, so the control is code review on the service repository. Then the obligations,
   each absent today: **the drop set must survive a roster re-resolve** — a refresh boundary re-reads
   the live roster (`keeper/internal/scenario/run.go:808-815`) and would re-admit a dropped host that is
   still online; **"not reached" records** for the dropped host's remaining tasks; **audit of the drop itself** — nothing records "host X left this run at barrier
   k", and §Provenance's "not reached" synthesis covers the dropped host's *tasks*, not the drop;
   **state capture on a dropped host** — §"State capture: a capture now waits for the steps before it" is
   written for the `scenario` radius and is not re-examined for a run that keeps committing `core.state.<verb>`
   steps after a host has left; **`soulprint.hosts` mid-run** — it shrinks between tasks, and
   nothing says whether a later task sees the pre-drop or post-drop roster. *Done in NIM-948* (user
   decisions 2026-10-08): the drop set survives the re-read; "not reached" is a SKIPPED
   `task.executed` for each later task whose `on:` selects the host; the drop is audited as
   `apply.host_dropped`; a capture after a drop writes as usual; a dropped host **stays** in
   `soulprint.hosts`, at its SID position, flagged `dropped: true`, so the post-drop roster is
   `soulprint.hosts.filter(h, !h.dropped)` — and it does not "shrink between tasks" after all.
6. **The canary diagnostics and the scenario audit ship before linear dispatch is enabled**, not in a
   later slice: the one for top-level `{change, check}` pairs (§Consequences) and the one for a block
   with `serial:` that does not write its mode out (fork 3). The audit also lists every keeper task
   written after a host task (decision 3). *Shipped in NIM-940:* `serial_scope_implicit_across_tasks`
   and `serial_scope_implicit`, with the audit as a comment on epic NIM-926. The fixtures with a
   block under `serial:` migrated with them: the `soul-lint` fixture named in fork 3, the inline YAML
   at `shared/config/passage_test.go:116-119`, and `TestDispatch_BlockSerialWave`
   (`keeper/internal/scenario/block_serial_test.go:13-19`), which asserts the whole-block wave and so
   now writes `serial_scope: block`. ⛔ *Both diagnostics were withdrawn before release by NIM-998*
   ([Amendment 2026-10-14](#amendment-2026-10-14-nim-998-an-unwritten-serial_scope-is-the-standard-behaviour-and-nothing-refuses-it));
   the fixtures keep their `serial_scope: block`, so each still describes a group under linear
   dispatch, although the model each of those tests runs under does not read it.
7. **The Acolyte path.** **Decided (user, 2026-10-02): restoring work-queue dispatch for linear runs
   blocks enabling linear dispatch**, rather than following later: the default radius ends a whole
   fleet's run mid-flight, and the wall clock grows T-fold, so the chance of a run being alive across
   a Keeper redeploy — with no reclaim — rises on both axes at once. *Built (NIM-960, NIM-961)*;
   the real-binary test — two Keeper processes, the run goroutine on one and every stream on the
   other, three steps through the queue and a `serial:` step inline — is
   `tests/e2e/linear_ha_test.go` (NIM-952).
8. **The `passage` capability gate's blast radius.** `gatePassageCapability`
   (`keeper/internal/scenario/soulcompat.go:412`; the capability constant is
   `shared/config/soul_capability.go`, contract at `proto/keeper/v1/lifecycle.proto:23-26`) today
   gates the rare staged run, fail-closed as `soul_passage_unsupported`. It will gate *every*
   multi-task scenario, so one Soul that never announced the capability stops scenarios for its whole
   fleet. *Kept (NIM-944):* a linear run of more than one step is gated, and its refusal names the
   steps instead of advising to remove a register dependency.
9. **`AuditReader` becomes an operational prerequisite**, not a diagnostic one
   (`keeper/internal/scenario/run.go:569-583`). *Done (NIM-944)* — see §`onchanges:`: a step-aware
   detector, and `task.executed` written regardless of `audit.enabled`.
10. **A capability bit is needed for the rendered predicates.** An earlier revision required one for
    carried registers; that reason is gone, but another replaces it. Literal-aware text guards and
    masking at the echo sites are Soul-side changes, and an older Soul compiles the *normalized*
    text (`shared/cel/eval.go:98`, `:105`, as `normalize` read before NIM-929): whitespace after an escaped quote collapses into a
    silently different literal, and a substituted secret is echoed in its errors. So a Soul that does
    not announce the bit is not sent a rendered predicate — the run is refused, fail-closed, as
    `gatePassageCapability` refuses today (item 8), with the same blast radius: one such Soul stops
    every scenario that carries a rendered predicate to it. A `when:` the Keeper decides sends
    nothing new — for a step it is blanked, and a grouped child's `when: false` is a predicate any
    Soul already evaluates — and a predicate in which no reference was resolved travels as written and
    needs no bit. That an older Soul reads a few shapes of an author's own text as it always did is
    a known limitation, not the bit's concern ([known-limitations.md](../known-limitations.md#an-older-soul-reads-some-authored-predicates-as-before-nim-929)). One catch: a child with no `when:` of its own that the requisite gate drops on one
    host now carries `when: false` there, and a non-empty `when` makes the capability gate demand
    `CapabilityFlowControl` — read from the shared `When` by the staged gate
    (`keeper/internal/scenario/soulcompat.go:94-106`), which runs before the requisite gate
    (`keeper/internal/scenario/run.go:942-949`). That gate must read the per-host value, and
    so run after the requisite gate. And per step it would
    refuse late: a one-Passage run is checked before any dispatch, while a
    per-step check refuses step *k* after the steps before it have changed hosts. So the check is
    kept **up front over the whole plan** for everything known before the run — modules and
    features — each task checked against the hosts its targeting resolves to before the run, so a
    host a static `where:` excludes cannot refuse it. That needs a pass resolving every task's
    static targeting and features up front: a future task is a placeholder whose only input
    to the gate is its module — no hosts, no flow-control fields
    (`keeper/internal/render/pipeline.go:265-282`). Three refusals stay late, and are stated rather
    than hidden: a task whose `where:` reads a register, whose hosts are unknown until its step — the
    reason the staged gate runs per Passage (`run.go:926-941`); a host admitted at a refresh boundary,
    which did not exist when the up-front check ran (`run.go:827-829` says so for the `passage` bit);
    and the per-host flow-control part, which depends on the results of earlier steps.
    *Done (NIM-946).* The first pass of a linear run also surveys the whole plan
    (`keeper/internal/render/soul_needs.go`): per task, its step, module, its features, and the
    hosts its `on:`/`where:`/`run_once:` resolve to when `on:` reads no register and `where:` — with
    the `vars:` it is resolved against, and inside a `soulprint.hosts.where("…")` — reads no
    register and no `incarnation.state` a capture at an earlier step changes (directly, or through
    `compute:` or those `vars:`, which every pass resolves again). Whether a value will be
    substituted depends on the text alone (`Engine.PredicateResolves`, the static half of
    `RenderPredicate`, held to it on the whole differential corpus). What the render drops before a
    step — an include group, a static `when:` — sends nothing, unless it reads state a capture at an
    earlier step changes (directly, or a static `when:` through its `vars:`); that one, a `when:`
    decided at the step, a requisite that drops the task on a host and a loop's items count as sent,
    and a host that leaves the roster later was still asked. A predicate reading a loop variable is
    no exception: the flow context binds none, so such a task fails at its step. The gate then
    refuses the run before its first step and before either dispatch path
    (`keeper/internal/scenario/run.go:645-654`), naming per host the first step and task that needs
    each missing capability. Each step is gated again inside `dispatchPassage`, after the requisite
    gate and on what each host is sent (`render.SentFlow`, `dispatch.go:83-87`), before the inline
    waves and the work-queue alike; the staged in-loop gate no longer runs for a linear run. A
    refusal is the run's under `on_failure: host` too: no host is dropped for it and `tolerate` does
    not count it ([settings.md §4](../scenario/settings.md)). *User decisions (2026-10-08):* an
    applier in a later step has its destiny resolved up front and its tasks checked on the applier's
    hosts; the refusal reuses `soul_capability_unsupported`, no new code. The late refusals, all at
    the step and all saying so: the three above — a task whose targeting reads a register, or, for
    the same reason, state an earlier capture changes; a host or a task a change of the roster
    brings in — a refresh boundary re-resolves it as a fresh snapshot and a host dropped under
    `on_failure: host` leaves it, so hosts join and change facts, `run_once:` may pick another host,
    and a `when:` or `where:` over `incarnation.host_count` or `soulprint.hosts` may switch a task
    on; the per-host flow-control part — then a destiny task a static `when:`, a conditional include
    or its own `on:`/`where:` could switch off on a host, since the survey does not render the
    destiny, and a task the survey cannot judge (an evaluation error, a transient Vault failure
    included). A destiny that does not resolve up front is left to its step, where it fails with its
    own error, as it always did.
11. **The public `passage` surface** — one row per host becomes T rows per host in the OpenAPI
    projection (`keeper/internal/applyrun/runsview.go:76-79`), in `soulctl incarnation`
    (`soulctl/internal/cmd/incarnation.go:302`) and in the companion web UI.
12. **Keep `within_block_register_dependency` for a block rolled as a group** — a default block no
    longer needs it — and establish whether a destiny has an equivalent. *Narrowed (NIM-942)* for
    a run executing linearly; a staged run still refuses it on every block, and `soul-lint`, which
    knows no execution model, still refuses it on every block too.
    If not, add one or state the gap in [docs/known-limitations.md](../known-limitations.md).
    *Established (NIM-939):* it had none. The destiny pass renders against an empty `register`
    root, so there is no stale-value path and a plain read fails at render — but `soul-lint`
    passed it and a `has()`-guarded read took its fallback silently. Added
    `within_destiny_register_dependency`, checked on the flat destiny plan by `soul-lint` and by
    the Keeper's destiny loader. What it does not cover is stated in
    [known-limitations.md](../known-limitations.md#register-reads-a-destiny-cannot-resolve-are-not-all-refused).
13. **Checks.** Refusals for `async:` and `require:` outside a destiny (decision 7; shipped by
    NIM-941) live in the shared validator, so the Keeper enforces them too — service
    repositories are not linted. In `soul-lint`: a warning for a child's `serial:` or barrier-kind
    setting inside a block rolled as one group ([settings.md](../scenario/settings.md) rule 4), and
    removal of the diagnostics that only meant something under derivation
    (`soul-lint/internal/validate/stage.go`).
14. **A real benchmark** on a fleet, on a scenario with several top-level tasks, measuring separately:
    the round trips, the Soul's per-message overhead, and the quadratic re-render with its Vault
    load. *Measured (NIM-950):* §Price, harness in `dev/bench/linear-dispatch` — at twelve steps
    only, so the growth of re-render with the step count is still unmeasured.
15. **Operations and rollout.**
    - **HA Keepers of different versions are not fail-closed here.** The same `{change, check}` under
      `serial:` rolls as a canary or across the whole fleet at once depending on which Keeper picked
      the run up. The execution model must be **pinned to the run** at its start and recorded, not
      read from whichever instance is serving.
      *Pinned (NIM-942):* `execution_model: staged | linear` is decided once at the top of the run
      and recorded in `engine_compat` on success and in the `incarnation.run_completed` payload on
      success and on a failure terminal (user, 2026-10-06) — not for a destroy run, which emits
      neither, nor by an instance that lost the terminal to another. `linear` is the default since
      NIM-952; `staged` is kept for one release as the deprecated rollback, and a run started under
      it logs a warning (user, 2026-10-11). The starting instance takes it from its own
      `keeper.yml::scenario.execution_model`, re-read at each run start (NIM-950,
      [config.md](../keeper/config.md#scenario)). It lives in the run goroutine. Only a linear run's step row carries a step in
      its recipe, with the run's decisions for it, so an instance that claims the row knows the
      model from the row ([ADR-027 amend (q)](0027-apply-work-queue.md), NIM-961).
    - **Rollback:** a `when:`, `changed_when:` or `failed_when:` reading a register from another
      derived Passage is legal in the new model and an older Keeper refuses it (FC-5) — loudly, which is acceptable, but it means a scenario authored after the change cannot
      run on a rolled-back Keeper. What goes *silent* on rollback is the order: an older Keeper runs
      keeper tasks before host dispatch again and lets healthy hosts run on after one fails. The
      release note says both ([CHANGELOG](../../CHANGELOG.md), NIM-952). The cheaper rollback for
      the one release that keeps it is the key, not the binary: `scenario.execution_model: staged`
      reaches the next run on a reload, every instance runs the same binary, so the HA skew above
      does not arise, and no schema change is undone — an older binary refuses to start against
      this release's migrations 121, 122, 123 (NIM-985), 124 (NIM-996), 125 (NIM-989) and 126 (NIM-1000) until they are taken down by hand,
      newest first, and `schema_migrations` is set back to 120.
    - Barrier timeout and straggler policy; a run started under ADR-056 while the Keeper is
      redeployed.
16. **Applier sources resolve from their children** for the cross-barrier gate (§`onchanges:`), with
    a guard test in the `rotate_tls` shape. Shipped ahead of the train in NIM-931.
17. **Decision 4's machinery**, each piece absent when this was written except the guards it lifts: a rewrite that
    meets the equivalence in
    [predicate-rendering.md](../scenario/predicate-rendering.md), with the differential test
    covering every trap listed there, and the walk that checks nothing outside the open set is left;
    `guardFlowControlHostInvariant` and `flowContextHostInvariant` lifted for rendered predicates; `register: self` reserved, as
    `register_name_reserved` already reserves `hosts` (`shared/config/scenario_task.go:1812-1820`),
    and a bare `register` refused in the post-module predicates;
    their per-host overlay; Soul text guards that leave a substituted value alone; masking at every
    echo site; Keeper-written SKIPPED and FAILED records; error capture per task and host. Shipped
    ahead of the train: the rewrite with its differential test, the residual walk, the bare `register`
    refusal and the `register: self` reservation in NIM-928; the Soul text guards and the masking in
    NIM-929.
    *Built on the Keeper (NIM-943)* for a run executing linearly, the step being dispatched only:
    `when:` decided per host and blanked, a module task's static one that fails to evaluate
    included (an applier's, a keeper task's and a loop's still fail the render); the three
    post-module predicates rendered per host (`RenderedTask.FlowBySID`) with no `flow_context`
    beside them; both host-invariance guards lifted; a grouped child sent `when: false` or its open
    residual; a skipped step, a task the requisite gate drops, and a failed host recorded by the
    Keeper (an `apply_runs` row, `task.executed`, the register), the failed host's other tasks
    with it, in the Soul's order (a failed `when:`, a false one, the requisites, a post-module
    predicate); a host failed before dispatch keeps its place among the `serial:` waves, the
    hosts sent beside it finish, and its wave's barrier judges it like any failed host — no later
    wave starts under radius `scenario` but to send rescues (item 18; a held failure does not stop
    them, per the NIM-993 amendment), the host is dropped under `host` (NIM-948); a `when:` reading a secret that fails
    to evaluate keeps CEL's error out of its record.
    Not built here: the capability gate reading the per-host values (since built, item 10), and a statically
    false `when:`, which still resolves no targets and so leaves no SKIPPED record (NIM-971).
18. **Rescue routing** — fork 1's four obligations. *Built (NIM-951)*, described in
    [rescues.md](../scenario/rescues.md). Two answers the fork left open (user, 2026-10-08):
    a dropped host's `where:` is not evaluated for a rescue, nor its blocks' `where:` and
    `run_once:` — the rescue's and its blocks' `on:` and the source's failure on it decide; and
    a rescue step goes through the work-queue like any step, its rows carrying the run's rescue
    routing ([ADR-027 amend (s)](0027-apply-work-queue.md)). The first answer's
    reason — the host's registers stop at its failure — holds for every host once the run has
    ended, so past the end no `where:` is evaluated: a rescue goes to the hosts its `on:` selects
    where its source failed, and nothing that reaches no host is rendered. A step rolled in
    `serial:` waves goes on after the wave whose failure ended the run, sending the hosts a
    rescue fires on their rescues. A keeper-side `onfail:` task is no rescue — the Keeper
    evaluates no requisite of its own tasks ([docs/keeper/modules.md](../keeper/modules.md)) —
    so it does not run after a failure ended the run. ⚠ Not covered ([rescues.md](../scenario/rescues.md)): a
    keeper-side task's own failure still aborts the run at once, before any rescue of it; a
    block rolled as one group takes a percentage `serial:` of its live hosts only; and in the
    later waves of the step that ended the run, a host whose applier rescue fires but whose
    destiny has no task for it is not sent the terminal.
    *Amended by NIM-993 (partly built — the Keeper side in part):* a failure that a later rescue names is held rather
    than judged, a rescue that succeeds returns the host to the run, and if none of its rescues
    succeeds the failure is judged by its source's radius — see [the amendment](#amendment-2026-10-14-nim-993-a-rescue-that-succeeds-handles-the-failure).

## Slices

- **S1 (atomic)** — the linear dispatch loop; `passage` as the step index, a default block's
  children each numbered and gated per child, its targets held; the execution model pinned to the run (item 15); the `vault()` memo
  widened to the run; Keeper-side `when:` and partial evaluation of the post-module predicates,
  with their secret masking, and items 16-18; the `passage` capability bit and item 10's new one;
  decision 7's `async:`/`require:` refusals, which must land when those keys stop having an effect
  (NIM-941 put them on the train ahead of the dispatch loop — unconditional, since `soul-lint` cannot
  see the model, and harmless because the train reaches `main` only with the enablement, NIM-952);
  FC-5's retirement on both sides at once — the Keeper's refusal, kept for a staged run only
  (`keeper/internal/scenario/run.go:523-530`), and `soul-lint`'s — or one of them refuses scenarios
  that are now legal; for the same reason `soul-lint`'s Passage-keyed state checks —
  `StaleStateRead` retires, since a capture now finishes before a later reader renders, and
  `StoreAfterUse` is re-keyed to plan order (all three shipped in NIM-944);
  fork 1's outcome; the `settings:` block with its fail-by-default, its one
  validator, the key-by-key merge and the "not inherited from the Voyage key" statement; the terminal status
  `on_failure: host` needs and its obligations (items 4-5). Any
  subset is a silent regression. The canary diagnostics (item 6) were on this list until the
  Amendment 2026-10-14 (NIM-998) withdrew them. The benchmark (item 14) runs before linear dispatch is enabled.
- **S2** — lands with S1, not after it: `serial:` per step, fork 3's two block modes and item 13's
  warnings for a child's `serial:` or barrier-kind setting inside a group. S1's step numbering
  already splits a default block, so the modes that decide which blocks split cannot come later.
- **S3** — the Acolyte path, which must be in before linear dispatch is enabled (item 7), and the
  public `passage` projection. The queue learns steps first —
  [ADR-027 amend (p)](0027-apply-work-queue.md) (NIM-960): the planned insert, the claim, the
  `dispatched` mark, the claim path's own writes and its `ApplyRequest`, the Cancel read and
  reclaim keyed by `(apply_id, sid, passage)` — the mark and the claim path's writes also fenced
  by `attempt` and reclaim by the row's status, which changes one-step runs too — a fencing epoch
  that survives a re-claimed step, and an older Keeper kept off a linear run's rows (the Conclave
  capability `step_queue`); then the Acolyte renders a step, and the run goroutine asks the
  capability before queueing a step ([ADR-027 amend (q)](0027-apply-work-queue.md), NIM-961).
  The Oracle path gets its step loop by becoming an ordinary incarnation run (NIM-977) — **not
  built when linear dispatch was enabled** (user, 2026-10-11): an Oracle reaction still goes as one
  message to its sender's SID, outside the step model
  ([known-limitations.md](../known-limitations.md#an-oracle-reaction-is-not-dispatched-step-by-step)).
- **S4** — `soul-lint`: drop the other derivation-only diagnostics.

`within_block_register_dependency` is removed in **no** slice; it is narrowed to blocks rolled as one
group.

## Amendment 2026-10-14 (NIM-998): an unwritten `serial_scope` is the standard behaviour, and nothing refuses it

**Decided by the user on 2026-10-14, at the demo of the NIM-926 train.** This revises one point of
fork 3's answer of 2026-10-01. That answer kept the default `serial_scope: task` but required the
author to write the line out. The default is still `task`, and it is now **the standard behaviour**:
a block with `serial:` and no `serial_scope` rolls by task. Each child is a step of its own and goes
over every host in waves of the block's width before the next child starts. A canary is opt-in:
`settings: {serial_scope: block}`. The Keeper and `soul-lint` no longer refuse the file.

The release had not shipped yet, so both refusals NIM-940 built on this train were withdrawn
together:

- `serial_scope_implicit`, fork 3's refusal: a block reached by `serial:` over two or more host
  tasks, with no `serial_scope` written on it, above it or in the scenario's `settings:`.
- `serial_scope_implicit_across_tasks`, the transition rule of §`serial:`: a `serial:` sharing a
  Passage of the ADR-056 derivation with another host step. This one also caught more than the
  canary. A step **without** `serial:` sharing a Passage with one that has it rolled in the narrow
  waves under ADR-056, and under this ADR it goes to every host in one wave. Take a `stop` followed
  by an upgrade with `serial: 2`: the service now stops on every host at once. The user saw that
  case and withdrew this refusal too. To keep "stop and upgrade N hosts, then the next N", the two
  steps go into one block with `serial: N` and `serial_scope: block`; a `serial:` on the `stop`
  alone still stops every host before the first upgrade.

What stays: the key and both of its values; what `block` means (one message per host, one wave
loop: the canary); `soul-lint`'s `group_child_key_unread` warning (item 13); and the
`serial_scope: block` in `examples/service/dragonfly/scenario/restart/main.yml`. The change of
meaning that §`serial:` describes is unchanged. It is announced in the CHANGELOG's upgrade notes
instead of being refused. Under the staged rollback `serial_scope` is not read, and such a file
rolls as it did under ADR-056: each wave carries the whole block, or the whole derived Passage, at
the Passage's narrowest `serial:`.

As built: `shared/config/serial_scope.go` keeps only the warning, and nothing on the Keeper's run
path (`keeper/internal/scenario/run.go`) refuses a file over `serial_scope` any more; the run path
still reads it to decide which blocks roll as groups (`BlockRollsAsGroup`). The two `soul-lint`
fixtures moved from `scenario-broken/` to `scenario-golden/`, where they lint clean. The guards
assert the roll itself: `TestLinearPassage_SerialBlockWithoutScopeRollsByTask` checks the step
numbering, `TestGolden_SerialWithoutScopeLintsClean` checks the linter, and
`TestIntegration_SerialScope_DefaultRollsByTask` checks the order in which the Keeper sends to three
hosts — by task, as the canary, and for a `serial:` task beside a plain step.

## Amendment 2026-10-14 (NIM-993): a rescue that succeeds handles the failure

> **Partly built.** Built:
> - the static refusals of rules 1 and 8, the `core.task.failed` module and the `rescue_unit` field;
> - the in-message rule on the Soul (rule 6), which announces `onfail_handled`;
> - on the Keeper, rule 1 on live hosts: the tasks of every unit with no source before it in its
>   own message are settled SKIPPED there, unevaluated, with only the layout evaluated
>   (2026-10-21);
> - rules 2-4: the run goroutine keeps each host's sources held, handled or judged, and holds,
>   returns and judges a host; `held` in `soulprint.hosts`;
> - rule 5: the `apply_runs.handled` mark (migration 132), the run goroutine that writes it at each
>   of the run's ends, the folds that read it, and `handled` on the wire;
> - the fail-closed rule and the net of rule 6 (2026-10-21);
> - rule 7's interim plan-wide `rescue_hold` gate.
>
> Not built:
> - **the `rescue_unit` marks.** The Keeper sends none (rule 6; render keys
>   `RenderedTask.RescueUnit` only internally). Every Soul therefore runs unmarked, `when:` first,
>   and inside one message a rescue is still compensation. The net of rule 6 is the normal path
>   for a unit task that fails where its unit did not fire, not a backstop;
> - **rule 7's recipe** — the failure state and the marks on the step recipe — and its per-step
>   gate. No instance announces `rescue_hold`, so the interim gate sends the steps of such a plan
>   inline;
> - **rule 8's Keeper half.** A `core.task.failed` step of its own is not settled by the Keeper:
>   it goes to the Soul like any Soul-side task, and its host needs `module:core.task`. The default
>   `message:` across a barrier is not built either. Inside a message the default is the Soul's,
>   which needs the marks, so today a `core.task.failed` written with no `message:` fails with the
>   module's generic text.
>
> User decision at the demo
> of the NIM-926 train (2026-10-14). It supersedes two of fork 1's consequences: *"a successful
> rescue does not change the terminal status"* and *"a rescue's own failure is judged like any
> task's, by its own effective `on_failure`"* (user, 2026-10-01). The second still holds on a host
> where the rescue did not fire (the net of rule 6, user, 2026-10-21). The syntax of `onfail:` does
> not change: a rescue names its sources by register from anywhere in the
> scenario, one rescue may name several, and Ansible's `block:`/`rescue:` shape is **not** adopted
> (user, 2026-10-14). `onfail:` on a `block:` already works ([destiny/tasks.md §6.5](../destiny/tasks.md))
> and the rules below hold for it.

Under fork 1 a rescue only compensates: the host whose failure it handled still leaves the run — it is
dropped under `on_failure: host`, the run ends under `scenario` — and a run with a failure cannot end
`success`. The user's reading: a rescue that succeeded means the error was handled, so the host carries
on with the run.

```yaml
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: /opt/app/deploy.sh }

  - name: reload the balancer
    module: core.exec.run
    params: { cmd: /opt/app/reload-lb.sh }

  - name: roll back
    onfail: [deploy]
    block:
      - name: restore the previous release
        module: core.exec.run
        params: { cmd: /opt/app/rollback.sh }
      - name: check the service
        module: core.exec.run
        register: health
        failed_when: "false"
        params: { cmd: /opt/app/healthcheck.sh }
      - name: not handled while the service is down
        module: core.task.failed
        when: register.health.exit_code != 0

  - name: warm the cache
    module: core.exec.run
    params: { cmd: /opt/app/warm.sh }
```

`deploy` fails on h2. h2 is held: `reload the balancer` goes to h1 and h3 only, and h2 records it
SKIPPED. The three children of `roll back` go to h2 alone. If the health check passes,
`core.task.failed` is skipped by its `when:`, the unit has succeeded, and h2 is back: `warm the cache`
goes to all three hosts and the run can end `success`. If the check fails, `core.task.failed` fails
the unit. No unit is left, so `deploy`'s failure is judged by `deploy`'s radius — under the default
`scenario`, the run ends at that barrier with `deploy`'s error.

**The rules.**

1. **A rescue unit is the outermost construct whose own `onfail:` names a source.** Units are reckoned
   per source (user, 2026-10-14). For one source, each task, `block:` or `apply:` whose written
   `onfail:` names it is a unit, with everything below it. A descendant that also names the source
   belongs to the enclosing unit, because the requisites merge as a union (§6.5). A destiny's tasks
   name only the destiny's registers, and a scenario's tasks only the scenario's, so all the units of
   one source are written in one plan — a scenario, or a destiny, with its includes spliced in. An `apply:` inside a unit — a unit itself, or a child of a
   block unit — is the exception, and §6.5 forces it rather than any choice made here: the `onfail:`
   of the applier and of its enclosing blocks is unioned into every task of its destiny.
   - When the unit fires, that OR-widening runs every task of the destiny, the destiny's own rescues
     included, as part of the one unit.
   - The Keeper strips the whole requisite once its cross-step part is satisfied (`resolveKind`,
     `keeper/internal/scenario/crosspassage.go:171-196`). Inside a firing unit the destiny's rescues
     are therefore ordinary tasks of that unit: they carry no `rescue_unit` mark of their own
     (rule 6), and any failure in the destiny fails the unit.
   - Applied anywhere else, the destiny's units work as written.
   - **Success.** A unit **succeeds** on a host when at least one of its tasks executed there and none
     failed. A task that fails fails the unit, and the unit's later tasks are not sent to that host.
     On a host where the unit did not fire, a failure of its task is the net's instead (rule 6). A
     unit in which nothing executed — its `when:` false, every task skipped — has not succeeded. A
     unit's `async:` tasks are awaited at the unit's last position before the unit is decided. An
     applier's terminal is not a task of its unit: it does not count as executed, it carries no
     `rescue_unit` mark. The Soul never reports it FAILED. The Keeper's `failedHost` records it
     FAILED only after another task of the message failed. So the terminal is left out of the unit's
     success, and every other task of the unit is read.
   - **Nested units of different sources.** A block with `onfail: [a]` may hold a task with
     `onfail: [b]`, `b` written outside the block. Each source then has its own unit, and the
     outermost one decides every source it holds. The task carries the outer unit's `rescue_unit`
     mark, since one field holds one unit, and the Keeper reads it the same way, so the message and
     the barrier agree. This is conservative: a failure anywhere in the outer unit leaves `b`
     unhandled too, even where the inner task succeeded.
   - **Several sources.** A unit that names several sources is decided per (unit, source). It is sent
     to a host while one of its sources is held there (rule 2), or as compensation to a host judged
     at once on one of them. It is skipped on a host where neither holds.
   - **`run_once:`.** A `run_once:` task unit goes, as under NIM-951, to the first host by SID it
     fires on, and it decides only that host's failure (user, 2026-10-14). A block's `run_once:` is
     not read for a held host, just as NIM-951 does not read it for a dropped one. A `run_once:`
     block unit therefore reaches every held host its `on:` selects, and decides each of them.
   - **A unit that does not fire is not evaluated.**
     - **Units with no source before them in their own message** (user, 2026-10-21). A task's own message is
       the message it travels in: a group rolled as one (with any destiny applied inside it), or the
       destiny of an applier that travels alone; any other task travels alone. The outermost unit
       decides (nested units of different sources, above), so the rule reads **the unit's
       sources**: every source that the outermost scenario unit's tasks name, nested units included
       (`config.CrossStepUnits`, `shared/config/passage_linear.go:266-276`). Rule 6 also uses the
       same source set, through `RescueRule.Sources` (`shared/config/rescue_rule.go:80-101`,
       `:200-208`). For a unit holding an `apply:`, the rule also holds
       the destiny's own sources: the destiny's tasks carry the outer unit's key
       (`keeper/internal/render/rescue_unit.go:23-33`) and are fed with the `onfail:` that §6.5
       unions into them (`keeper/internal/scenario/failure_state.go:423-426`). §6.5 makes those
       ordinary tasks of the unit (above), and `CrossStepUnits` never walks into a destiny
       (`shared/config/rescue_unit.go:116-118`). A unit task can fire on a live host only through
       one of the unit's scenario sources in its own message: a sibling before the unit in its
       group, in plan order, at its own step. A forward `onfail:`,
       a source sibling placed after the unit, cannot fire it on a live host, since the Soul runs
       the unit first; such a unit is in `config.CrossStepUnits` and is settled SKIPPED on live
       hosts (`shared/config/passage_linear.go:247-257`, `:277-317`). A source at any other step
       fires nothing on a live host:
       - a source at an earlier step fires a unit only on held or judged hosts, and those are not
         live: a host judged at once is dropped or past the run's end, and a host returns only once
         every source it held is handled;
       - a source at a later step cannot have failed when the unit runs. No pass reads a failure at
         or after the unit's step (`fires`, `keeper/internal/render/rescue.go:63-78`).

       So a unit task with no source before it in its own message never fires on a live host, whatever mix of
       earlier and later steps its unit's sources stand at. A destiny's own units are left out of
       this rule: their sources travel in the destiny, and they are decided in the destiny's message
       (rule 6). Until 2026-10-21 the rule covered only a unit whose sources all stand at earlier
       steps; the defect this left is described under *Today every path evaluates first*. Since
       then `config.CrossStepUnits` takes every unit with no source before it in its own message
       (`shared/config/passage_linear.go:242-317`). A unit with a source before it in its own
       message is left to the Soul: only the Soul knows, once that sibling has run, whether the
       unit fires, so the Keeper cannot settle it (below). This holds even on a host the sibling
       does not reach, because the plan does not show that host: the unit goes to the Soul and the
       net of rule 6. Such a host is one that the sibling's `on:` or `where:` leaves out, one other
       than the host a `run_once:` sibling is sent to, or one where an include-`when:` drops the
       sibling's include group.
     - **On every linear scenario pass while the run goes on**, whether or not anything has failed
       yet, the Keeper settles such a task SKIPPED on each live host that its `on:`, and its
       enclosing constructs' `on:`, select. The exception is an `apply:` unit whose static `when:`
       is false or counted false, which leaves a live host nothing (below). A scenario unit's
       `when:` is **counted false** when it cannot be decided without a host: its value is not
       read, and the task is laid out as a static-false one. In a destiny such a `when:` is
       refused by name instead (below). On a live host the Keeper evaluates none of the task's
       `where:`, `params:` or `when:`, nor a block unit's own `where:`.
     - **The cover reads the `on:` chain only.** A unit child of a group that is no unit is
       settled on every live host its `on:` chain selects, even one that the group's `where:` or
       `run_once:` leaves out. That host still gets the child, as a `when: false` copy. This is
       kept: the cover never reads a `where:`.
     - **On a held, dropped or past-the-end host** such a task is routed as before: it goes where
       one of its sources fires, its `where:` not evaluated (rule 2), and not where a task of its unit
       already failed (`routeUnit`, `keeper/internal/render/rescue.go:410-464`). A later step's source
       never fires it there either.
     - **What fixes the plan's layout is evaluated. A module task's static `when:` is also read
       without a host, only by the guards: to refuse one over a per-host name, and to lift a DSL
       guard's refusal.** Nothing else is. The plan indices of later steps depend on an `apply:`
       unit's destiny layout and on a `loop:`'s items, so a live host evaluates what the layout
       reads, as today. For an `apply:`, that is the host-invariant names of `apply: input:` and of
       the destiny's `vars.yml`, which its include-`when:` may read. The `apply: input:` names are
       evaluated over one synthetic host, as the no-target path does but with the per-host names
       left out (`keeper/internal/render/destiny.go:192-203`; `resolveApplyInput`, `:598-604`). The
       destiny's `vars.yml` is a lazy view since NIM-1014 ([ADR-0082](0082-service-vars.md)
       amendment 2026-10-15): a name is evaluated when a cell reads it, so laid out a per-host one
       is never read. An include `when:` refuses it by name, and a laid-out `flow_context` leaves it
       out (`:290-303`). Today's no-target path renders every name, so this layout-only render is
       new. Telling which input names each `required` or `validate:`
       rule reads is part of that work. The per-host names are left unrendered there:
       `hostVariantInputNames` for `apply: input:` and `hostVariantDestinyVars` for `vars.yml`. The
       input contract runs over the invariant names only, so the `required` and `validate:` checks
       of an unrendered per-host name are skipped there. A read there of an output from the unit's
       own tasks is a read of a skipped task's output (rule 3), and the author guards it. The lift
       is below (`keeper/internal/render/destiny.go:343-346`,
       `keeper/internal/render/block.go:343-346`, `keeper/internal/render/rescue.go:262`).
     - **A static `when:` that fixes the layout is decided without a host** (user, 2026-10-20). A
       unit that fires on no host while the run goes on is **laid out**. Its `when:` is decided
       only where it fixes the layout: a `loop:`'s, in the scenario or in a laid-out destiny, and
       an `apply:` unit's own (`keeper/internal/render/rescue.go:251-262`,
       `keeper/internal/render/destiny.go:339-367`, `keeper/internal/render/block.go:338-386`). A
       laid-out module task is a placeholder whatever its `when:`, which is read only by the guards
       (below). The `when:` is decided over one synthetic host, with the task's
       `vars:` entries that read a per-host name left out: `soulprint.self`, a register the host
       keeps, an `apply: input:` name rendered per host, or another such entry (`hostFreeWhen`,
       `keeper/internal/render/rescue.go:391-408`). A `when:` that reads an entry left out cannot
       be decided there. A scenario unit's counts as false: its value is not read, and the task is
       laid out as a static-false one (`layoutStaticWhen`, `keeper/internal/render/rescue.go:304-343`).
       That is a scenario `loop:` unit and an `apply:` unit's own `when:`, at the top level or in a
       block. A destiny's is refused by name (next bullet), so nothing in a laid-out destiny counts
       as false. Nothing per host is read.
     - **In a destiny, a static `when:` over a per-host name is refused, laid out as routed** (user,
       2026-10-27). NIM-1014 refuses it by name in every destiny pass
       ([ADR-0082](0082-service-vars.md) amendment 2026-10-15, point 7): a `when:` that reads a
       per-host `apply: input:` name, or a name of the task's view over one, its own `vars:`
       included (`guardStaticWhenHost`, `keeper/internal/render/pipeline.go:1005-1014`). A
       laid-out destiny task meets the same guard, over the task's whole view and before any entry
       is left out (`layoutGuard`, `keeper/internal/render/rescue.go:288-290`): a `loop:`, a module
       task and a block's module child alike. So the author meets the refusal on the first run,
       not on the first run where the unit fires, as a task the guard refuses when routed is
       refused when laid out too (below). One exception, a gap the user did not accept on its own:
       it follows from two of the user's decisions, option D (2026-10-20), under which a
       scenario unit's `when:` that cannot be decided without a host counts as false (the
       bullet above), and option A (2026-10-27), this refusal. An `apply:` unit whose own static
       `when:` counts as false at the scenario level is one placeholder, its destiny not laid
       out at all (next bullet), so a `when:` over a per-host name inside that destiny is
       refused only once the unit is routed with its own `when:` true
       (`keeper/internal/render/rescue.go:266-268`, `keeper/internal/render/pipeline.go:301-306`,
       `keeper/internal/render/block.go:390-397`;
       `keeper/internal/render/rescue_unit_route_test.go:749-753`, `:789-794`). A DSL refusal
       still stands instead, as before (`keeper/internal/render/rescue.go:291-297`); routed, the
       host-scope refusal is met first (`keeper/internal/render/destiny.go:369-371`,
       `keeper/internal/render/pipeline.go:1169`).
       Until 2026-10-27 a laid-out destiny counted such a `when:` false and refused it only once
       the unit was routed, so a layout and a routed render of one destiny disagreed on whether it
       was valid (`TestRender_ALaidOutWhenReadingAPerHostVarIsFalse`,
       `keeper/internal/render/rescue_unit_route_test.go:712-734`).
     - **An `apply:` unit whose static `when:` is false, or counted false, leaves a live host
       nothing**, as a static-false applier that is no unit does. It is one placeholder that targets
       no host, its destiny not laid out: a live host gets no SKIPPED record from it, no terminal
       and no `register.<applier>`. Laid out or routed, no host is sent or settled anything
       (`keeper/internal/render/rescue.go:266-268`; `keeper/internal/render/rescue_unit_route_test.go:748`,
       `:777`, `:1125-1139`).
     - **Any other failure of that `when:` still fails the render**, as it does routed: a compile
       error, a context that cannot be built, or an evaluation error in a `when:` that reads no
       entry left out, such as a misspelt var (`keeper/internal/render/rescue.go:331-338`). In a
       destiny the host-scope refusal comes first, laid out as routed: a `when:` there that does
       not parse and names `vars` while the task's view holds a per-host name is refused as a
       whole-namespace read of `vars`, before it is compiled.
     - **A known gap, accepted, at the scenario level alone.** Every evaluation error of a
       scenario unit's `when:` that reads an entry left out is taken for that entry. So an error
       beside it under `||` or `&&` is absorbed while laid out: `vars.at == 'eu' || vars.att == 'x'`,
       with `att` misspelt, counts as false. It fails only on a routed render where the first
       routed host by SID has `at` not `'eu'` (NIM-1035). Evaluating with the entry unknown would
       not catch it either: cel-go lets an unknown absorb an error in a logical operator
       (`keeper/internal/render/rescue.go:314-318`). In a destiny that `when:` is refused by name
       in both passes, and the gap does not arise.
     - **A DSL guard's refusal is lifted for a laid-out task only when its static `when:` is
       decided false without a host**, as an inactive branch's DSL does not block the active one
       (ADR-012(d); `layoutGuard`, `keeper/internal/render/rescue.go:272-302`). A `when:` that
       cannot be decided keeps the refusal. So does one that reads `incarnation.host_count`,
       directly or through the task's `vars:`, since it reads 0 laid out
       (`whenReadsHostCount`, `keeper/internal/render/rescue.go:347-360`). A task the guard
       refuses when routed is therefore refused when laid out too, on a pass where nothing failed.
       A known residual, narrow: the walk reads only the task's own `vars:`. A `when:` that reads
       `incarnation.host_count` through a destiny `vars.yml` entry or an `apply: input:` value is
       still decided at 0 while laid out, and can lift the refusal there. It takes a refused DSL
       key and a `when:` gated on the host count.
     - **`incarnation.host_count` counts the hosts routed to the unit**, in its destiny and in its
       own static `when:`, not the live hosts it is settled SKIPPED on. It is 0 while laid out
       (`keeper/internal/render/pipeline.go:403-405`, `keeper/internal/render/destiny.go:250`,
       `keeper/internal/render/rescue.go:331`).
     - **A laid-out and a routed render of one unit can differ in plan indices.** A scenario
       unit's `when:` counted false laid out may be true for a routed host, and
       `incarnation.host_count` changes. The two
       renders meet only between runs, or against a pass before the unit's step, which dispatches
       none of the unit (`keeper/internal/render/rescue.go:247-250`). Within one run, the unit's
       step is routed the same way on every pass at or after that step. The barriers before it fix
       what is routed (`fires`, `keeper/internal/render/rescue.go:63-78`; `routeUnit`, `:431-440`).
       `RenderInput.ForStep` keeps a host's live, held or dropped standing fixed per step
       (`keeper/internal/render/standing.go:100-126`).
     - **Out of scope: a routed render decides a scenario static `when:` on the first host by
       SID** (`keeper/internal/render/pipeline.go:955-960`). The defect predates the units and
       holds outside rescues too. It is NIM-1035. In a destiny pass NIM-1014 refuses a static
       `when:` over a per-host name instead of deciding it on one host
       ([ADR-0082](0082-service-vars.md) amendment 2026-10-15, point 7), so NIM-1035 is the
       scenario-level case alone.
     - **What it writes is what the requisite gate writes today:** the SKIPPED record with the
       skipped register (`keeper/internal/scenario/keeper_flow.go:100-111`). A grouped child still
       travels as `when: false`, so its siblings can read its register
       (`Grouped`, `keeper/internal/render/render.go:743-747`). This is also the record rule 3
       means for a returned host.
     - **An `apply:` unit not skipped by its static `when:` gets its layout and nothing per
       host.** No per-host `apply: input:`, and none of the destiny's per-host `vars.yml` values,
       `where:` or `params:`, is rendered for a live host. Every destiny task gets a SKIPPED record,
       the destiny's own rescues included: their in-message sources are skipped there, so they
       cannot fire. An applier with `register:` has its terminal
       (`keeper/internal/render/destiny.go:452-491`) sent alone, and the Soul folds that register to
       all four `false`. Inside a group the terminal travels with the message, so a sibling reading
       `register.<applier>` finds it. An applier with no `register:` has no terminal, and no
       register.
     - **Today every path evaluates first and settles after.** The ordinary targeting path
       (`keeper/internal/render/pipeline.go:354-392`, `keeper/internal/render/block.go:466-473`) and
       NIM-951's routing while the run goes on (`routeRescue`, `keeper/internal/render/rescue.go:135-142`)
       evaluate `where:`. The gate settles the task only after render, and after `when:`
       (`keeper/internal/scenario/keeper_flow.go:79-94`, `:126-149`). So a unit task that reads an
       output of its own unit fails the host, or, in `params:`, ends the run. Before rule 1 was built,
       the example above failed on h1 and h3 even in a run where nothing failed. Its `roll back`
       has no source before it in its own message, so rule 1 now settles it SKIPPED there.

       Until 2026-10-21 a unit that also named a later step's source took that path too: the Keeper
       took a unit off it only when every source stood at an earlier step. A unit with a source
       before it in its own message still takes it. Such a unit is a child of a group, so it takes
       the ordinary child targeting (`keeper/internal/render/block.go:466-473`), or `routeRescue`
       once the run holds or has dropped a host, or has ended (`keeper/internal/render/block.go:447-454`).
       `routeRescue` serves only such units, through `walkBlockChildren`, and reads no
       `FailedUnits` (`keeper/internal/render/rescue.go:111-173`): no top-level task reaches it.
       Both evaluate `where:` for the live hosts. The case that the widened scope closed:

       ```yaml
       tasks:
         - name: deploy              # step 1
           module: core.exec.run
           register: deploy
           params: { cmd: ./deploy.sh }
         - name: fix
           onfail: [deploy, verify]
           block:
             - name: r1              # step 2
               module: core.exec.run
               register: r1
               params: { cmd: ./fix.sh }
             - name: r2              # step 3
               module: core.exec.run
               when: register.r1.stdout == 'ok'
               params: { cmd: ./restart.sh }
         - name: verify              # step 4
           module: core.exec.run
           register: verify
           params: { cmd: ./verify.sh }
       ```

       On a live host h1 where `deploy` succeeded, `r1` was settled SKIPPED. The Keeper then settled
       `r2`'s `when:` there, `register.r1.stdout` had no value, and h1 failed for nothing. Its row
       carried only tasks of a unit, so the failure was the unit's and made no entry: h1 stayed
       live, and the run could commit `ready` with a FAILED record on it. Now `fix` has no source before
       it in its own message, so both tasks are settled SKIPPED on h1 unevaluated
       (`TestIntegration_LinearAMixedSourceUnitIsNotEvaluatedWhereNothingFailed`,
       `keeper/internal/scenario/rescue_hold_integration_test.go:1210`), and the net of rule 6
       judges any failure that still reaches such a task.
     - **A unit task with a source before it in its own message** — a grouped task naming a sibling before it — can fire
       on a live host, so the Keeper routes and evaluates it.
       - **Until the Keeper sends marks** (rules 6 and 7, not built), every message is unmarked and
         both sides keep the old order, `when:` first. The Keeper settles a grouped `when:` that
         reads no group register (`settleHostFlow`, `keeper/internal/render/flow.go:158-176`), and
         the Soul reads `when:` before the requisite (`soul/internal/runtime/applyrunner.go:491`,
         `:588-595`). A `when:` that errors on a host where the unit did not fire fails the task,
         and the net of rule 6 judges it.
       - **Once marks are sent**, the Keeper passes such a task's `when:` to the Soul together with
         the requisite, and the Soul checks the requisite first (`skipUnitTask`,
         `soul/internal/runtime/applyrunner.go:535-545`, built in S2), before `evalWhen` (`:595`).
         The order is reversed for the tasks of a unit only, and the net becomes a backstop.
   - **`onfail:` naming a register set inside a unit of the same plan is refused**
     (`onfail_names_rescue`, user, 2026-10-14). A unit's failure is not a failure of its own: it
     means "not handled" for its source (rule 4). A `when:` reading such a register stays legal.
     The check reads one plan — a scenario, or a destiny, with its includes spliced in. A destiny's
     own `onfail:` naming the destiny's registers is judged in
     the destiny, so applying that destiny from a unit does not get it refused.
   - **A group rolled as one holds at most one unit for sources outside it**
     (`group_multiple_rescue_units`, user, 2026-10-14). The Keeper strips a satisfied cross-step
     `onfail:` from the wire, so inside the group's message such a unit is a run of ordinary tasks.
     With one such unit, the message's fail-stop is the unit's own. With two, the Soul could neither
     skip the second after the first succeeded, nor give it its chance after the first failed.
     Units that cover the whole group count as one — the group's own and any enclosing construct's,
     since inside the group's message their plans are the same. A unit strictly inside the group
     counts on its own: an enclosing unit plus one inside is two, and is refused.
2. **A failure waits for its rescues.** At the barrier of the step where a host failed, a failure is
   **held** rather than judged when a unit of its source — the failed task's register, or its
   applier's — has its last step later, and the `on:` of that unit and of its enclosing constructs
   selects the host. This holds in a step rolled in `serial:` waves too: the later waves go on (user,
   2026-10-14). A canary with a rescue downstream therefore no longer stops at its first failure, which
   supersedes, for a held failure, [orchestration.md §2.2.1](../scenario/orchestration.md)'s "no later
   wave starts". A held host:
   - gets no ordinary task, only the units of its held sources, exactly as a dropped host does:
     NIM-951's routing reaches it, and `where:` is not evaluated for it;
   - is listed in `soulprint.hosts` with **`held: true`**, while `dropped` stays `false` (user,
     2026-10-14). `!h.dropped` keeps meaning a host that is part of the run's outcome, since a held
     host may return. The hosts receiving tasks now are `!h.dropped && !h.held`;
   - is not counted by `tolerate`, does not end the run under `scenario`, and is not audited
     `apply.host_dropped`;
   - counts as still in the run for "no host is left" (settings.md §4). A run whose every host is held
     therefore walks on, keeper-side steps between the source and its units included. A
     `core.state.<verb>` capture among them commits, although the run may still end `failed` at a
     unit's barrier under `scenario`.

   A host's failures at one step are held only if **all** of them are. Otherwise the host is judged at
   once, by every failure it reported there. Its units still reach it as compensation, but they cannot
   return it to the run.

   **A host the Keeper fails before sending a group** — a group rolled as one, whose child failed on
   the Keeper — is **not held** when the group carries a source. That source is the cause, or a
   sibling the Keeper records FAILED. The host is judged at once by the first source among them,
   whatever the cause: the judge takes the first record that belongs to a source, so it skips a
   cause whose failure is its unit's (`judgedTasks`, `keeper/internal/scenario/failure_policy.go:540-546`).
   A cause that is a source under the net of rule 6 is one more candidate, and it comes first.
   - The Keeper writes FAILED records for the unsent siblings
     (`failedHost`, `keeper/internal/scenario/keeper_flow.go:153-173`), for tasks that never ran, so a
     host that returned would carry them as failures.
   - Units of the cause and of the siblings still reach the host, as compensation.
   - A group that carries only unit tasks — a unit sent to a held host — fails its unit instead
     (rule 1).
   - A one-task step the Keeper fails has no siblings and is held like any other failure.
   - A `loop:` step is held too. Its iterations share one register and are one source, so the FAILED
     records of the other iterations are failures of that same source.
   - A unit task the Keeper fails — a `when:` that fails on the Keeper, or a `core.task.failed` step it
     settles (rule 8) — fails its unit like any other unit failure (rule 1). It is no cause of this
     kind, except where its unit did not fire: there it is a source like any other (the net, rule 6).

   A failure tied to no task is held the same way when the task its records mark FAILED (settings.md
   §4) is a source with a unit ahead.

   Held failures are not entries. A failure still held when the failure list closes is appended at
   that point, so the run's error names it. The list closes when the run ends:
   - for a run that a failure ended, after the last unit's barrier. Such a run still walks to the
     units (rule 9), and a failure one of them handles is never appended;
   - at the run's end, otherwise;
   - at once, when a Cancel or an abort stops the run.
3. **Units are tried in plan order, and the first that succeeds handles the failure.** The host is
   back in the run from the step after that unit:
   - it gets every later ordinary task, with one exception: a default block whose hosts were chosen at
     its first step while the host was held. The host rejoins at the first step after that block (user,
     2026-10-14), since fork 3 holds a block's targets;
   - the later units of that source are skipped on it. Each leaves a SKIPPED record, except an
     `apply:` unit whose static `when:` is false or counted false (rule 1);
   - the ordinary steps between the source and the handling unit are not replayed. For each task the
     held host did not receive, the Keeper writes the audit SKIPPED it writes for a dropped host today
     (`writeNotReached`, `keeper/internal/scenario/failure_policy.go:923-969`).
   - When it renders a later step for the returned host, the Keeper gives it the register a skipped
     task carries — `{changed: false, failed: false, timed_out: false, skipped: true}`, with no output
     fields. For a missed applier it is what a Soul's fail-stop emits for an applier over skipped
     children: all four `false`, since an applier's `skipped` is always `false`
     ([orchestration.md §2.1.1](../scenario/orchestration.md)). The register is synthesized from the
     run's held record, with no `apply_runs` row behind it and nothing written. It appears in
     `register.hosts.<name>` like any per-host register.
   - Reading an output field of a missed task then fails as it does for any skipped task, with
     `no such key` (user, 2026-10-14). In a `when:` that fails the host; in a `where:` or `params:` it
     ends the run. The author guards with `!register.<name>.skipped`.

   A unit that fails, or that does not succeed, does not handle the failure, and the next unit gets
   its chance. A host is back only when every failure it holds is handled.
4. **If no unit succeeds, the failure is judged by its source's radius.** The judgement happens at the
   barrier of the last unit that could have reached the host. The verdict is made from the held entry,
   because the host may have no failed row at that step: the unit was skipped by `when:`, or a
   `run_once:` unit went elsewhere.
   - Under `scenario`, the run ends at that barrier.
   - Under `host`, the host is dropped there, audited `apply.host_dropped` with the source task, and
     counted by `tolerate` there. `apply_runs.dropped` goes on the source's row and on the rows of the
     units that failed — not on a row of the judging step at which no unit failed.

   The entry names the source task and carries the errors of the units that failed. A unit's own
   `on_failure` is **not read**: like `tolerate` beside `scenario`, it is unused, not an error. Only
   the net of rule 6 reads the failing task's effective `on_failure`, for a unit task that fails
   where its unit did not fire. Each held source is judged at the barrier of its own last unit, by
   its own radius. Under `host` the
   host is dropped at the first such verdict, and its other held sources stay held (see *What the run
   goroutine keeps*, rule 6). Under `scenario` the run ends there. This retires fork 1's ⚠: a rescue left at the default `scenario` no longer ends the run
   for every host when its source failed under `host`. For a failure tied to no task, the source's
   radius is the barrier owner's at the step where it failed (settings.md §4).
5. **A handled failure is not undone, and it is not an entry.**
   - The source keeps its FAILED record, and `register.<source>.failed` stays `true`.
   - A row keeps `failed_plan_index` and `error_summary` as its first failure, handled or not. A row
     that the Soul finished `success` after handling a failure in its message keeps them as the record
     of what it handled. The judge and the held entry read the
     unhandled failures from the facts, not from the row's first failure.
   - A run whose every failure was handled ends `success`, with the incarnation `ready`. One that also
     dropped hosts ends `partial_failed`.
   - A `failed` or `orphaned` row is marked **`apply_runs.handled`** when every failure it holds was
     handled. That covers the source's row, and the rows of units that failed before one succeeded.
     A row that also holds a failure still unhandled when the run ends is not marked: it stays a
     failure of the run.
   - **The mark.** Migration 132 adds it beside `dropped` (migration 122)
     (`keeper/migrations/132_add_apply_runs_handled.up.sql`), and `applyrun.MarkHandled` sets it
     (`keeper/internal/applyrun/crud.go:795-816`). Like `dropped`, it goes on `failed` and
     `orphaned` rows only, and never on a dropped one: `MarkDropped` clears it (`:772-793`).
   - **The rows it goes on.** `runFailures.handledRows` lists a row only when all three hold
     (`keeper/internal/scenario/failure_policy.go:329-368`):
     - a barrier judged the row failed or orphaned (`failedRows`, set in `runFailures.judgeRow`,
       `keeper/internal/scenario/failure_state.go:333-336`);
     - the row holds at least one source, as the source's own row or as the row of a unit of it
       that failed;
     - every source the row holds is handled.

     A row that also holds a failure no source stands for is not listed.
   - **Where the run writes it.** At each of the run's ends:
     - a run that commits `ready` writes the marks inside `commitSuccess`'s transaction, after
       `UpdateStateFromRun` (`keeper/internal/scenario/run.go:1914-1940`). The incarnation's
       `ready` and the marks commit together. This is fail-closed: a mark that fails rolls the
       commit back, and the run ends `state_commit_failed` (`:1231-1253`);
     - `finishPartial` writes them after the failure list closes, before `MarkDropped` and
       `lockIncarnation` (`keeper/internal/scenario/failure_policy.go:976-994`, `markHandled`
       `:996-1019`);
     - an abort writes them after `ensureTerminalApplyRun` returns (the `abort` closure of
       `Runner.run`, `keeper/internal/scenario/run.go:152-224`; `ensureTerminalApplyRun`,
       `:1705-1780`). That call either writes the sentinel or closes the non-terminal rows.
       When an update of a row misses, because the host's own result landed first or the
       update failed, it reads the rows again and writes the sentinel if no settled row
       carries the abort. Each of these decisions counts the rows to be marked as marked
       already. The marks are left off only when it returns false: the rows could not be
       read, at either read, or the sentinel could not be written; a second read that fails still
       writes the sentinel. The run then reads
       `failed`. An abort writes the handled marks only, never `MarkDropped`;
     - a teardown writes them before it deletes the incarnation (`run.go:1191-1193`). The delete
       takes the rows with it (`ON DELETE CASCADE`, `keeper/migrations/018_create_apply_runs.up.sql:45`),
       so the mark matters only when the delete fails.

     The last three are best-effort, as the drop marks are: a row left unmarked reads as a failure
     of the run.
   - The fold subtracts the marked rows from the failures and does **not** count them toward
     `partial_failed`. This holds in `AggregateRunStatus`
     (`keeper/internal/applyrun/runsview.go:149-195`) and in its SQL twin, the status `CASE` of
     `runsAggregateSelect` (`keeper/internal/applyrun/runsglobal.go:129-136`). Every reader of a
     run's outcome goes through one of the two, so a run whose every failure was handled reads
     `success` on all of them: the run status (`SelectRunStatus`, `runsview.go:388-430`), the
     incarnation's run history (`ListRunsByIncarnation`, `:202-269`) and run detail
     (`SelectRunDetail`, `:288-386`), the global runs list (`runsAggregateSelect`), and the Voyage
     outcome, which polls `SelectRunStatus` (`voyagePgIncarnationAwaiter.pollOutcome`,
     `keeper/cmd/keeper/daemon.go:6125-6137`).
   - **`handled` is on the wire** (user, 2026-10-14), as an only-add field of the run's host row
     (`RunHostStatusEntry`, `shared/api/wire/incarnation.go:243`), built with the folds above. It
     is `true` on two kinds of row:
     - a row marked `apply_runs.handled`;
     - a `success` row that carries a failure, one the Soul finished after handling a failure in its
       message. Such a row needs no mark, because a `success` row is no failure of the fold.

     The wire fills `failed_plan_index` "on a failed or handled row", no longer "ONLY on a failed
     row" (`shared/api/wire/incarnation.go:227-231`). Showing it in the web UI is a soul-stack-web ticket.
6. **The same rule holds inside one message.** The Soul applies it within one `ApplyRequest` — a
   destiny, or a group rolled as one:
   - after a failure, the ordinary tasks skip;
   - a unit in the message whose source failed runs, and the first that succeeds handles the failure.
     The Soul resumes the ordinary tasks after it and skips the later units of that source;
   - an `async:` source's failure is observed where it is awaited (ADR-0075(c)) and held from there.
     Ordinary tasks that ran in between are not undone;
   - `RunResult` is `FAILED` only when a failure is still unhandled at the message's end, the final
     barrier's included. This supersedes *"onfail tasks never undo the failure"*
     (`soul/internal/runtime/applyrunner.go:368-373`).

   A failure unhandled at a message's end, with a unit at a later step, is held by the Keeper like any
   other.

   The Soul learns the units from an only-add `RenderedTask` field, **`rescue_unit`**: numbered from
   1 within the message, and zero on a task in no unit. The Keeper sets it on every task of every unit
   in a message that carries a source together with a unit of it. The Keeper decides the
   marks where it builds the message:
   - for a scenario step, in the run goroutine. The step recipe carries them (rule 7), and a claim
     sends the recipe's marks and never derives its own;
   - for a push run ([ADR-0032](0032-push-orchestrator.md)), in the push orchestrator, and for
     `core.ssh.apply` ([draft](draft-ssh-apply-unregistered-hosts.md)), in that module. Both mark a
     destiny's in-message pairs the same way, so the destiny behaves as it does when applied from a
     scenario. Their outcome is the Soul's `RunResult`, which `core.ssh.apply` counts a success only
     as `SUCCESS` (`keeper/internal/coremod/ssh/apply.go:321`). An older binary keeps fail-stop and
     reports `failed`;
   - nowhere else. A staged run's messages carry no marks on any path, inline or claimed, and an
     Oracle reaction's carry none either: both keep fail-stop. A `core.ssh.apply` step marks its own
     messages in a staged run too, because the module does not read the run's execution model.

   The marks are decided after the requisite gate, so a task whose `onfail:` the gate stripped
   carries none (rule 1's firing unit). An `apply:` inside a unit whose source is in the
   same message — both inside a group rolled as one — gets the same treatment by another route. The
   gate strips nothing there, so the Keeper drops the destiny's own sources from the `onfail_idx` of
   the unit's tasks. Those tasks then carry the unit's sources alone, together with the unit's mark.
   They are ordinary tasks of that unit, with no mark of their own. A Soul applies rule 6 only to a
   message in which some task carries it. A message from a Keeper that sets none — an older Keeper
   during the "Souls first" rollout ([ADR-0076](0076-engine-compat-window.md)) — runs with fail-stop to
   the end and reports `FAILED`, as before. A Soul that does not announce **`onfail_handled`** is
   refused a message that carries such a pair (`soul_capability_unsupported`, item 10).

   **Fail-closed.** Take a row the Soul reported `failed` whose message carried a source. If its facts
   hold no source that is not handled, the row's first failed **source** is set **not handled**.
   A failed row that holds no failed source at all is a failure tied to no task. From there,
   rule 2 decides whether that failure is held — a unit of it ahead reaches the host — or judged at
   once. A row whose message carried only units is a unit row instead. Its failure is its unit's
   (rule 1) where that unit fired, and the net's (below) where it did not. A unit row that reports
   no failed task fails each of its units that fired. Where none fired, its failure is tied to no
   task and is judged at once by the barrier owner's radius
   (`keeper/internal/scenario/failure_state.go:343-353`, `:379`, `:670-675`). This covers an older
   binary on a push host: the capability gate exempts push hosts (`exemptPushHosts`,
   `keeper/internal/scenario/soulcompat.go:356-375`), and such a binary runs fail-stop. The host can
   lose a handled outcome, but it never carries on past tasks it skipped.

   **What the run goroutine keeps.** A **source** here is a failed task that is not a task of a unit,
   or one whose unit did not fire on that host (the net, below).
   For each host, the run goroutine keeps every failed source in one of three states:
   - **held** — waiting for a unit ahead (rule 2);
   - **handled** — a unit succeeded (rule 3);
   - **judged** — at once, or at the barrier of its last unit (rule 4).

   *Judged* is final. A unit that succeeds afterwards, as compensation, handles nothing and returns
   nothing.

   A host dropped while some of its sources are still held keeps them held. A unit that succeeds then
   handles such a source, which leaves no entry, but cannot return the host. A source that no unit
   handles is judged at its last unit, by its own radius. Such a host is `dropped: true`,
   `held: false` in `soulprint.hosts`, and it is out of the "no host is left" count.

   A unit task's failure is not a source, except under the net below. It fails its unit (rule 1) and
   is never judged by itself: on a held host the next unit gets its chance, and on a judged host it
   changes nothing.

   **The net: a failure of a unit that did not fire** (user, 2026-10-21). A failed task of a unit,
   on a host where that unit did not fire, is an ordinary failure, as before NIM-993.
   - **"Did not fire"** means that, when the failure is fed to the host's rule, none of the unit's
     sources (rule 1) stands unhandled there, held or judged, and none failed while the unit was
     registered and undecided, that is, in its own message (`sourceFailed`,
     `shared/config/rescue_rule.go:45-49`, `:144`, `:155-159`). A source handled before the unit was
     registered does not count.
   - **It is a source, not a failure of its unit.** A later scenario unit naming the failed task's
     applier can hold it: `RescueRule` expands the applier's register to the applier's children
     (`shared/config/rescue_rule.go:80-90`), and rule 2 holds a failure by its applier's register
     too (`keeper/internal/scenario/failure_state.go:588`, `:711`). That covers a task of a
     destiny's own unit. Otherwise no unit can hold it, since no `onfail:` may name a register
     set inside a unit (`onfail_names_rescue`, rule 1), and rule 2 judges it at once, by the
     task's own radius: its effective `on_failure`, which rule 4 otherwise leaves unread.
   - **Its unit handles nothing.** `RescueRule.Fail` marks the unit lost, and `Decide` reads that
     (`shared/config/rescue_rule.go:50-53`, `:137-160`, `:168-176`). The unit is not recorded as a
     failed unit either: it makes no `FailedUnits` entry, and puts no unit error on its sources
     (`keeper/internal/scenario/failure_state.go:374-375`, `:473-477`).
   - **Inside a group the Keeper failed before sending** (rule 2), only the cause failed, on the
     Keeper; nothing was sent. The replay feeds the cause first, then only the ordinary siblings,
     terminals aside (`replay`, `keeper/internal/scenario/failure_state.go:410-415`, `:451-456`,
     `:465`).
     - A sibling **unit** task, on either side of the cause, is not fed. It neither fires nor
       fails a unit, and it is no net source.
     - An **ordinary** sibling's FAILED record stays a judged source, so its units still reach the
       host as compensation (rule 2). Every source of that step takes the radius of the first
       source among the records (`newSources`, `keeper/internal/scenario/failure_state.go:553-562`,
       `:579-584`), and the judge names that one (`judgedTasks`,
       `keeper/internal/scenario/failure_policy.go:540-546`). A cause that is a net source comes
       first.
   - **It is never swallowed.** No failure that no unit handled may end in a run that commits
     `ready` with a FAILED record and no entry.
   - **Until marks are sent, it is every Soul's path.** The Keeper sends no `rescue_unit` marks
     yet (rules 6 and 7), so every Soul runs unmarked and reads `when:` first
     (`soul/internal/runtime/applyrunner.go:491`, `:588-595`), and the Keeper settles a grouped
     `when:` that reads no group register (`keeper/internal/render/flow.go:158-176`). A `when:`
     that errors where its unit did not fire, in a unit with a source before it in its own message or in a
     destiny's own unit, reaches the net from every current Soul. Once marks are sent, a marked
     unit task whose unit does not fire is skipped by `skipUnitTask` before its `when:` is read
     (`soul/internal/runtime/applyrunner.go:535-545`, `soul/internal/runtime/rescueunit.go:141-152`).
     The net then meets the case only where the Keeper fails such a task itself, and in a row from
     a Soul that ignores the marks, an older binary on a push host (fail-closed, above).
   - **It is a net.** Rule 1 settles every task of a unit with no source before it in its own
     message SKIPPED, unevaluated, on a live host (a grouped task still travels as `when: false`;
     an applier's terminal goes alone), so such a task does not fail there. A unit with a source
     before it in its own message, and a destiny's own unit, are evaluated: for them the net is the normal path
     until marks are sent (above), and a backstop after. Without it, a unit row's failure was its
     unit's on every host and nobody judged it: the example under rule 1's *Today every path
     evaluates first*, where the host stayed live.
   - **Built** (2026-10-21). `RescueRule.Fail` makes a failed task of a unit that does not fire a
     held source, and leaves a unit already decided as it is
     (`shared/config/rescue_rule.go:127-160`). The replay records such a source
     (`keeper/internal/scenario/failure_state.go:445-450`), and the judge takes it like any source
     (rule 2). A fail-closed unit row where no unit fired is a failure tied to no task (`:343-353`,
     `:670-675`). `RescueRule.Counts` is the Soul's alone (`shared/config/rescue_rule.go:241-262`).
     The Keeper's applier fold is its requisite gate's, over the sources no unit handled
     (`crossPassageGate`, `keeper/internal/scenario/crosspassage.go:215-240`;
     `failureView.unhandledKeys`, `keeper/internal/scenario/failure_state.go:1046-1059`), so a
     net source counts there until a unit handles it.

   The run goroutine decides the states at each barrier:
   - first from the recorded facts, by the same rule as the Soul: one type in `shared/config`
     (`RescueRule`), which the Soul drives in message order and the Keeper at its barriers. The
     applier fold is not shared: the Soul's, in `aggregateRegisterData`, reads `RescueRule.Counts`,
     and the Keeper's is its requisite gate's, over the sources no unit handled (below);
   - then by the fail-closed verdict above, which overrides the facts.

   A host's sources are fixed at the barrier that judged its row. A `TaskEvent` that the Soul
   relays after that barrier (`result_relay`) still lands in the audit log and in
   `apply_task_register`, so a later step's `register.<source>.failed` can read `true`. No reader of
   the states sees it, so an `onfail:` naming that source does not fire for it.

   Every reader takes these states and never recomputes them from the facts. A step reads them as
   they stood when that step was dispatched, both a host's standing (live, held or returned) and each
   source's state, for targeting as for routing:
   - a returned host stays out of the ordinary steps it missed, on every re-render. The live roster
     is set once per pass for every step (`keeper/internal/scenario/run.go:975-980`), and a return
     would otherwise grow it backwards;
   - a source handled at or after a step still fires there on a later pass.

   A step therefore renders the same on every pass. That is NIM-951's invariant: no failure after a
   rescue's step is read (`fires`, `keeper/internal/render/rescue.go:63-78`). Rule 5 keeps the
   source's FAILED fact, so the readers that read the FAILED facts today read the sources that are
   **not handled** instead, held or judged. That is what lets a host judged at once still receive its
   units as compensation (rule 2):
   - the requisite gate: `anyKey`/`factIndices`, `keeper/internal/scenario/crosspassage.go:215-240`,
     over `failureView.unhandledKeys` (`keeper/internal/scenario/run.go:1080-1087`);
   - rescue routing: `failureView.rescue`, `keeper/internal/scenario/failure_state.go:998-1044`;
   - the verdict: `stepJudge.verdict` and `judgedTasks`, over the sources no unit handled
     (`keeper/internal/scenario/failure_policy.go:501-559`);
   - the claim's requisite gate (`ClaimRunner.stepGate`, `keeper/internal/scenario/claim.go:326`).
     It is to take the states from the recipe (rule 7, not built); today it reads the FAILED facts
     (`:346`).

   **Built today, "the same on every pass" holds only under `needsRescueHold`** (rule 7): in a
   plan holding a cross-step unit, at every step, and in a plan holding a rescue step, from the
   run's first failure on. Only there does the run goroutine render each earlier step with each
   host's standing as that step saw it (`render.Standing`,
   `keeper/internal/scenario/run.go:975-980`). There a host dropped at a later step is still live
   at the earlier one. Any other plan re-renders every earlier step over the one roster of the
   pass, as before the amendment. A claim re-renders such a plan over the recipe's one roster, and
   the two renders must agree on plan indices until rule 7 puts the standing in the recipe. In
   such a plan an earlier step that reads the hosts still in the run renders differently after a
   drop: a `loop:` over `soulprint.hosts.filter(h, !h.dropped)` loses an iteration on later
   passes, and an index into that list can fail the render (`render_failed`). Both sides are
   pinned (`TestIntegration_LinearReRenderKeepsAHostDroppedLater`,
   `TestIntegration_LinearQueuedReRenderOfAPlanWithoutAUnitKeepsOneRoster`,
   `keeper/internal/scenario/failure_policy_integration_test.go:425`, `:472`). One difference
   reaches every linear plan: rescue routing reads which register failed on which host, and at
   which step, from the sources a barrier recorded, not from the latest pass's tasks. After a
   shift of plan indices, the old reading could name another task.

   Rule 1's "at least one task executed" reads the executed tasks — every status but SKIPPED —
   from the audit reader's `SelectExecutedTaskKeys`, beside the run's FAILED, CHANGED and reported
   keys (`ChangedTaskReader`, `keeper/internal/scenario/scenario.go:470-475`).

   **An applier fails only by an unhandled failure of its children.** When a destiny handles its own
   failure, `register.<applier>.failed` is `false`, and an outer `onfail: [<applier>]` does not fire.
   The fold is taken at the terminal's own position. A child that a later unit naming the applier
   handles therefore leaves `failed: true` there, as rule 5 keeps it for any handled source, and
   the Keeper's fold, its requisite gate, reads it the same way.
7. **The work-queue path carries the same state.** A claim re-renders its step and refuses a host to
   which it would send anything else ([ADR-027 amend (q)](0027-apply-work-queue.md)). Today the recipe
   carries only `Dropped` and the rescue routing (`applyrun.RecipeStep`,
   `keeper/internal/applyrun/recipe.go:60-94`). It gains:
   - each host's sources with their states — held, handled or judged — as the run goroutine decided
     them, with the step at which each became handled or judged, so a claim re-renders every earlier
     step as it stood;
   - the units that failed on each host;
   - the missed tasks whose skipped registers rule 3 synthesizes;
   - the `rescue_unit` marks of each host's message (rule 6).

   This **failure state** is carried on every step from the run's first failure on, handled ones
   included, in a plan that holds a rescue unit; a plan without one needs nothing beyond amend (s). While it is present, a claim reads it alone and never the FAILED facts. A recipe without
   it — no failure yet, or one written by an older run goroutine — leaves the claim reading the FAILED
   facts as before, which is right in both cases.

   A step whose recipe carries the failure state or marks goes through the queue only when every live
   Conclave record lists **`rescue_hold`**, as `step_queue` gates a linear step (amend (p)).
   Otherwise the step is sent inline. A step holding a task of a unit with no source before it in
   its own message (rule 1) needs `rescue_hold` too, failure or not. On such a step a live host gets a
   queued row only through an `apply:` unit's terminal. An older claim evaluates that unit's
   `where:` and `apply: input:` for the host, which the run goroutine does not (rule 1), so its task
   list can differ.
   - **Marks alone count**, with nothing failed. A destiny step that carries its own pair is exactly
     the case the queue takes, since groups and push steps are never queued (`Runner.queuesStep`,
     `keeper/internal/scenario/dispatch.go:401-409`).
   - **A claim from before the amendment** compares only plan indices with the recipe
     (`ClaimRunner.settleStep`, `keeper/internal/scenario/claim.go:308-309`). It would send the step
     without the marks, and it would render without the failure state.
   - **A claim takes the marks and the state from the recipe**, so whichever Keeper version claims a
     row sends what the run goroutine decided, and an older goroutine decides none. This is [ADR-027 amend (v)](0027-apply-work-queue.md).
   - **Built today: a plan-wide gate.** No instance announces `rescue_hold` yet
     (`keeper/internal/redis/conclave.go:281-288`). `needsRescueHold` sends inline every step of a
     plan holding a cross-step unit, and every step of a plan holding a rescue step from the run's
     first failure on (`keeper/internal/scenario/rescue.go:123-125`). A plan whose every unit has
     a source before it in its own message queues as before, until its first failure when it holds
     a rescue step. A plan with no unit queues as before.
   - **Built today: the widened scope** (2026-10-21). A cross-step unit is one with no source
     before it in its own message (rule 1). Render and `hasCrossStepUnit` take it from one function,
     `config.CrossStepUnits`, which reads the union of the unit's sources as rule 1 defines it
     (`shared/config/passage_linear.go:242-317`). Render keeps them as `crossUnits`
     (`keeper/internal/render/pipeline.go:209-212`, `keeper/internal/render/render.go:358-361`), and
     `crossStepUnit` keys a task by the outermost unit holding its position
     (`keeper/internal/render/rescue.go:218-226`). `hasCrossStepUnit` is
     `len(config.CrossStepUnits(...)) > 0` (`keeper/internal/scenario/rescue.go:127-139`). Neither
     waits for a rescue step any more: a plan whose only unit names sources at later steps holds
     none.
     `config.RescueSteps` keeps its meaning, the steps holding a rescue of an earlier step's source
     (`shared/config/passage_linear.go:160-189`): it also chooses the steps a run that a failure
     ended walks to (`keeper/internal/scenario/run.go:796-851`).
8. **`core.task.failed`** is a new core module (user, 2026-10-14: its name and where it runs).
   - **What it does.** It always fails its task. Inside a unit it is the explicit "this error is not
     handled"; elsewhere it fails the task as Ansible's `fail` does.
   - **`message:`** (string). Inside a unit it is optional, and defaults to the original error: what
     the host's held sources of that unit recorded, joined in plan order with `"; "`. Outside a unit it is required
     (`missing_required_field`).
   - **"Inside a unit" is judged in the plan where the task is written** — a scenario, or a destiny,
     with its includes spliced in: by its own `onfail:` or an enclosing one. A destiny linted alone can therefore decide it, and a destiny task that is a
     rescue only through its applier's `onfail:` still needs `message:`.
   - **`failed_when:`, `changed_when:`, `retry:` and `timeout:` on it are refused** — and so is
     `settings.task_timeout` written on the task itself, the long form of `timeout:`. One inherited
     from an enclosing `settings:` stays legal.
     (`<key>_on_task_failed_invalid`). The task fails on purpose. A step of its own is settled by
     the Keeper, where such a key would mean something different or nothing at all, while on the
     Soul `failed_when:`, `changed_when:` and `retry:` would apply to its result: one scenario, two
     behaviours, so the key is refused rather than ignored. `when:` stays, because it is how a unit
     decides whether the error is handled, and how the task fails only sometimes.
   - **Its failure is the unit's** (rule 1) where its unit fired; elsewhere the net's (rule 6).
   - **A step of its own** — a scenario task outside a group rolled as one — **is settled by the
     Keeper** at render:
     - every host the step targets is failed before anything is sent, with the records a host failed by
       the Keeper already gets (`failedHost`, `keeper/internal/scenario/keeper_flow.go:153`);
     - no message reaches a Soul, and a push host opens no session;
     - so both capability gates — the one ahead of the first step and the per-step one — exempt such a
       step: no host is required to announce `module:core.task` for it.
   - **Inside a destiny or a group rolled as one**, it travels in the message and the Soul runs it: its
     `when:` may read a register of that message, which the Keeper does not have.
   - **Its address is Soul-side** ([ADR-0087](0087-task-side-derived-from-module-address.md)), so it is
     legal in a destiny. Settling a step of its own is the Keeper deciding a task it can decide without
     the host, as it decides `when:` (decision 4). It is not a keeper-side module: such a module runs
     once per run with no host, and could not fail one.
   - **Where the original error comes from.** Across a barrier it comes from the held entry; inside a
     message, from the source's `TaskEvent`.
   - **Built today:** the module (`soul/internal/coremod/task/task.go`), the static refusals
     (`shared/config/task_failed.go`, `shared/config/rescue_unit.go:160-166`), and the Soul's
     default inside a marked message (`withDefaultMessage`,
     `soul/internal/runtime/rescueunit.go:173-198`). **Not built:** the Keeper settling a step of
     its own, and the default across a barrier. Until then such a step is sent to the Soul, which
     runs it, so both capability gates require `module:core.task` for it. A `core.task.failed`
     with no `message:` fails with `GenericMessage` everywhere, since no message is marked yet.
9. **What does not change.**
   - A Cancel ends the run at once, and no rescue runs.
   - A keeper-side source's own failure still aborts the run at once (`keeper_dispatch_failed`), so it
     has no rescue to wait for.
   - A keeper-side `onfail:` task is still no rescue.
   - After a failure has ended the run, a held host's units still reach it, and a unit that succeeds
     handles its failure, which then leaves no entry. But no run is left to return to.
   - A staged run, the one-release rollback, gets nothing of the amendment. Its barrier stays
     ADR-056's, and its messages carry no marks, so inside a message it keeps fail-stop (rule 6).
     The one exception is a `core.ssh.apply` step, which marks its own messages on every run. The
     static refusals of rules 1 and 8 apply to every execution model too, since `soul-lint` cannot
     tell one from another. On the Keeper, a staged run skips `group_multiple_rescue_units` only:
     under staged `serial_scope` is not read, so no group exists.

**Rejected** (user, 2026-10-14):
- **Ending the run at the source and returning every host after the rescue.** The healthy hosts would
  silently lose the steps between the source and the rescue.
- **Leaving `scenario` fatal.** The return to the run would then work only under `host`, so the default
  radius would get nothing.
- **Continuing a host past its failure until its rescue.** Ordinary tasks would run on top of a failed
  source, which is the thing fail-stop exists to prevent.
- **"Any unit" for a block.** The first child of a rescue block alone would handle the failure, and
  the host would come back half-restored.
- **Settling `core.task.failed` on the Keeper only.** It would have to be refused inside a destiny,
  where one scenario would then behave differently than across barriers.
- **Judging a failure at once in a `serial:` step.** It kept the canary's stop, but a rescue would never
  return a host that failed in a `serial:` step.
- **Failing only the returned host when it reads an output it missed.** That would be a rule for
  returned hosts alone; the same read of a task skipped by `when:` would still end the run.
- **A `run_once:` unit handling every host it fired on.** A host would come back although nothing ran
  on it.
- **A unit's failure as a failure of its own, with rescues of its own.** One host would carry two
  interleaved verdicts.
- **The outermost unit swallowing the destiny's, as the rule.** A destiny's rescues would then behave
  differently depending on who applies it. The one place it holds — a destiny applied from a firing
  unit — is forced by §6.5's widening (rule 1).
- **A returning host joining the rest of a default block.** It would run the block's tail without its
  head.
- **A wire field naming the held sources a task answers for, so that a group can hold several
  units.** It would cost one more field and more logic on the Soul, for a canary carrying more than
  one rescue.
- **`handled` in the database only.** An operator would see a successful run with failed rows that
  nothing explains.
- **`dropped: true` while held.** A capture or a choice of master written as `!h.dropped`, placed
  between a source and its unit, would leave out a host that later returns.

## Relation to other ADRs and documents

| Document | Effect |
|---|---|
| [ADR-056](0056-staged-render-passage.md) | **superseded** (NIM-952) — the derived N-Passage plan is replaced by a happens-before order; FC-5 retires, since every reference across a barrier is resolved on the Keeper. Its model stays selectable for one release as the deprecated rollback `scenario.execution_model: staged` |
| [ADR-012(d)](0012-keeper-soul-grpc.md) | **amended** — for a scenario task, `when:` is evaluated on the Keeper and the post-module predicates arrive with everything but `register.self` — and, in a group, the group's own registers — rendered in, per host, behind a capability bit (item 10); a grouped child's `when:` that still reads a sibling goes to the Soul the same way; no register crosses the wire, and `flow_context` loses its purpose |
| [ADR-0075](0075-intra-host-async-tasks.md) | still applies inside a destiny; `async:` and `require:` outside a destiny are refused (`async_outside_destiny`, `require_outside_destiny`, NIM-941) |
| [ADR-027](0027-apply-work-queue.md) | **amended** — amends (p)-(t): the queue addresses a step row (NIM-960), a linear step goes through it (NIM-961), the run is read from its lock between steps (NIM-972), a rescue step carries its routing (NIM-951), the barrier wakes on the row write (NIM-984); amend (u) records the default (NIM-952) |
| [ADR-009](0009-scenario-dsl.md) | **amended** — a `settings:` block with one schema at every level, inherited as a value like `vars:` (not composed like `when:`/`where:`), the first execution-behaviour container in the grammar; the execution model changes. `retry:`/`timeout:` on a block stay refused (NIM-910 lifted them as defaults for the children and was reverted, user 2026-10-07: not obvious); `async:` on a block is open, NIM-959 |
| [ADR-010](0010-templating.md) | unaffected — the register grammar and the NIM-909 amendment stand |
| [ADR-0084](0084-explicit-state-capture.md) | **amended** — a capture runs where it is written (§State capture); its per-Passage state refresh and stale same-Passage read rule (`0084:230-237`, `:330-343`) become per step, so that stale read can no longer arise and its `soul-lint` check retires in S1 |
| [orchestration.md §2.2.1](../scenario/orchestration.md) | **rewritten** (NIM-952) — the per-Passage `serial:` minimum, the wave/barrier invariant and the canary idiom |
| [ADR-043](0043-voyage.md) | **unaffected, and nothing is reused** — a disjoint value vocabulary (`host`/`scenario` vs `abort`/`continue`), a different unit (a host at one barrier vs a run unit), and a threshold off by one from `max_failures`. The Leg policy and `soulctl --on-failure` stay exactly as they are |
| [naming-rules.md](../naming-rules.md) | **new rows** — `settings` and its six keys (`serial_scope` included), the refusal code for a run-kind key below the scenario level, the residual-check code and the code for a predicate the Keeper cannot carry, the refusals of a bare `register` and of `when: register.self.*`, `self` under `register_name_reserved`, and `partial_failed` joining the run-status enum. The live Voyage table has no `on_failure` row to amend, and the `wave.on_failure` row is **Tide's**, dead since ADR-043 |
| [orchestration.md §2.2.3](../scenario/orchestration.md) | **corrected** (NIM-952) — it showed Tide's `--wave-size`/`--wave-on-failure`/`--target-coven` invocation and the "REPLACE concurrency" rule that ADR-043 §B1 and the code contradict |
| [orchestration.md](../scenario/orchestration.md) §8 | **closed an open question** (NIM-948) — *"there is no tolerance threshold for partial failure"* is answered by `tolerate` at the barrier |
| [settings.md](../scenario/settings.md) | **new** — the scenario `settings:` block (fork 4); implemented (NIM-927, NIM-940, NIM-942, NIM-948, NIM-949, NIM-951) |
| [ADR-draft per-host params dispatch](draft-per-host-params-dispatch.md) | **builds on it** — NIM-908's `ParamsBySID` is the model for the per-host overlay the rendered predicates need; NIM-908 left flow control refused when it differs per host (open Q #25), which decision 4 answers |
| [predicate-rendering.md](../scenario/predicate-rendering.md) | **new** — how the Keeper renders a post-module predicate (decision 4): the equivalence it owes, the traps the differential test covers; implemented (NIM-928, NIM-929, NIM-943, NIM-946) |
| [known-limitations.md](../known-limitations.md) | item 12 found no destiny equivalent and NIM-939 added one; the reads it does not refuse are listed there |
| NIM-993 amendment — [ADR-012](0012-keeper-soul-grpc.md) | **amends (c)'s field list** — only-add `RenderedTask.rescue_unit`, plus the Soul capability `onfail_handled` ([naming-rules.md](../naming-rules.md)) |
| NIM-993 amendment — [ADR-0032](0032-push-orchestrator.md) and the [`core.ssh.apply` draft](draft-ssh-apply-unregistered-hosts.md) | **amended** — a push run and `core.ssh.apply` mark a destiny's in-message source/unit pairs with `rescue_unit` as a scenario step does (rule 6), so a destiny handles its own failures the same way on every path |
| NIM-993 amendment — [ADR-0087](0087-task-side-derived-from-module-address.md) | **unaffected** — `core.task.failed` is Soul-side by its address; the Keeper is to settle a step of its own the way it decides `when:`, not as a keeper-side module (not built: today the Soul runs it) |
| NIM-993 amendment — [ADR-027](0027-apply-work-queue.md) | **amend (v)**, built in part (a plan-wide `rescue_hold` gate), the rest design: the step recipe carries the run's failure state (failed sources held, handled or judged; failed units; missed tasks) and the `rescue_unit` marks, and the Conclave capability `rescue_hold` gates queueing such a step |
| NIM-993 amendment — [ADR-0076](0076-engine-compat-window.md) | **unaffected** — its "Souls first" rollout is why a Soul applies the in-message rule only to a message that carries `rescue_unit` |
| NIM-993 amendment — [ADR-0075(c)](0075-intra-host-async-tasks.md) and its [architecture.md](../architecture.md) stub | **amended** — "run irreversibly `FAILED` … only the `onfail:` rescue tail runs" no longer holds inside a message: a unit that succeeds handles the failure (rule 6) |
| NIM-993 amendment — [settings.md](../scenario/settings.md) §2.1, §3.1 and §4, with §4's rescue part now [rescues.md](../scenario/rescues.md); [destiny/tasks.md `onfail:`](../destiny/tasks.md) (its §12 open question on `rescue:` is answered now: no explicit syntax); [predicate-rendering.md §1](../scenario/predicate-rendering.md) (a group that fails before dispatch "judged by the child's radius"); [destiny/tasks.md §9 `when:`](../destiny/tasks.md) ("first, requisites … then `when:`", while the code reads `when:` first and only a unit task's requisite comes first); [orchestration.md](../scenario/orchestration.md) §2.2.1, §4.1 (`soulprint.hosts`), §5 (the probe idiom) and §7 item 3 (a finally-failed task judged at its step's barrier); [naming-rules.md](../naming-rules.md) rows `soulprint.hosts`, `apply.host_dropped` and `partial_failed`; [known-limitations.md](../known-limitations.md#rescues-what-is-not-built-yet) "A successful rescue does not handle the failure", now "Rescues: what is not built yet"; [destiny/tasks.md §6](../destiny/tasks.md) (an async failure leaves the run "irreversibly failed"); [orchestration.md §2.1.1](../scenario/orchestration.md) and [destiny/output.md](../destiny/output.md) (an applier's `failed` = OR over its children); [operator-api/incarnations.md](../keeper/operator-api/incarnations.md) (a `success` run holds only `success`/`no_match` rows, the failure fields appear only on a failed row, no `handled`); the `RunHostStatusEntry` doc (`shared/api/wire/incarnation.go`), the run-status doc comments of `keeper/internal/applyrun/runsview.go` and `runsglobal.go`, and `hostFactsToMap`'s idiom (`keeper/internal/render/cel_render.go`) | **rewritten with the code**. Each states the opposite of a rule of the amendment until then. They gain the held failure, rescue units and a handled failure's outcome. The in-message rule *"onfail is compensation, not forgiveness"* stops being true. "A host fails at most once" (§2.1) no longer holds: a host can return and fail again. A held failure does not stop the later `serial:` waves (§2.2.1, and §3.1's "a failed check stops the roll"). Under the default radius, a held failure is a second bypass of the probe idiom's stop (§5), beside `on_failure: host`. `soulprint.hosts` gains `held`. A `success` run can carry `failed` rows marked `handled`, and a handled row keeps its failure fields. The known limitation narrows to what is not built: inside one message a rescue stays compensation until the Keeper sends marks. The user-facing documents describe what is built and mark what is not |
| `shared/audit/task_executed.go:56-61` | **corrected** (NIM-952) — the comment asserted keeper tasks carry `passage=0`, which ADR-056 Slice 2 already falsified and this ADR made uniformly false |
