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
using `serial:`.

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
| `scenario` | **The default.** The run ends at the barrier where the host failed; no later task is dispatched to anybody but the rescues of fork 1. |
| `host` | Only that host. It leaves the run; the remaining tasks go to the rest. |

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
before the run ends.

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
    wave starts under radius `scenario` but to send rescues (item 18), the host is dropped under `host` (NIM-948); a `when:` reading a secret that fails
    to evaluate keeps CEL's error out of its record.
    Not built here: the capability gate reading the per-host values (since built, item 10), and a statically
    false `when:`, which still resolves no targets and so leaves no SKIPPED record (NIM-971).
18. **Rescue routing** — fork 1's four obligations. *Built (NIM-951)*, described in
    [settings.md §4](../scenario/settings.md). Two answers the fork left open (user, 2026-10-08):
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
    so it does not run after a failure ended the run. ⚠ Not covered (settings.md §4): a
    keeper-side task's own failure still aborts the run at once, before any rescue of it; a
    block rolled as one group takes a percentage `serial:` of its live hosts only; and in the
    later waves of the step that ended the run, a host whose applier rescue fires but whose
    destiny has no task for it is not sent the terminal.

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
| `shared/audit/task_executed.go:56-61` | **corrected** (NIM-952) — the comment asserted keeper tasks carry `passage=0`, which ADR-056 Slice 2 already falsified and this ADR made uniformly false |
