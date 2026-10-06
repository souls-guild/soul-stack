# ADR-draft. Scenario dispatch is linear — a task is dispatched only after the previous one has finished

> **Status: accepted (implementation pending); a draft until its number is stamped (NIM-907).** User decision 2026-09-28. **Supersedes [ADR-056](0056-staged-render-passage.md)**;
> amends [ADR-012(d)](0012-keeper-soul-grpc.md); changes what [ADR-0075](0075-intra-host-async-tasks.md)
> applies to; affects [ADR-027](0027-apply-work-queue.md); amends [ADR-009](0009-scenario-dsl.md) and
> [ADR-0084](0084-explicit-state-capture.md). **Text approved by the user on 2026-10-02**, with the
> two decisions outside the forks: a run that dropped some hosts ends `partial_failed` (item 4), and the
> Acolyte path blocks enabling linear dispatch (item 7). Design, not implemented. §Before merge carries **four forks** — contract decisions that need the user's answer
> before any code, each with the price of every option. **All four are answered**; fork 3's answer
> was re-confirmed after the tree's own canary was shown to change meaning under it, and its flag is
> `serial_scope: task | block`. Fork 4 — the scenario's `settings:` block — outgrew this ADR and is specified in
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
  CHANGED/FAILED facts (`keeper/internal/scenario/crosspassage.go:42-218`, wired at
  `keeper/internal/scenario/run.go:796-814`), and a Keeper that cannot read those facts **refuses
  the run** rather than guessing (`cross_passage_requisite_unsupported`,
  `keeper/internal/scenario/run.go:486-493`).
- **Cross-passage `when:`/`changed_when:`/`failed_when:` is already a fail-closed refusal** — FC-5,
  `config.CodeCrossPassageWhenGating` (`shared/config/passage.go:106-116`), detector at
  `shared/config/passage.go:533-567`, offline in `soul-lint/internal/validate/stage.go:213-230`.
- **A `block:` child reading a sibling's register is already a fail-closed refusal** —
  `within_block_register_dependency` (`shared/config/passage.go:94-105`).
- **The one spelling invisible to all of the above was closed at compile by NIM-909** — choosing a
  register's name by index is refused by `shared/cel.guardRegisterNameByIndex`
  (`shared/cel/register_index.go:80-110`) and offline as `register_index_form`
  (`soul-lint/internal/validate/register_index.go:61`).

So the class was real, and it was **patchable point by point** — one of those patches landed the
same day this ADR was written. The linear model does not cure it and is not offered as a cure. What
it removes is the **dependence on a derived boundary**: one dot-form regex is read by four
subsystems at once — the Passage graph, the cross-reference existence check (which is `soul-lint`'s
`unknown_register_reference`), the Soul's `async:` join set (`soul/internal/runtime`
`flowSet.addNames`) and `config.IsStaticPredicate` (`shared/config/task_refs.go:430`); the four are
named together at `shared/cel/register_index.go:10-46`.

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
(`keeper/internal/scenario/run.go:655-681`), so a host admitted mid-run has executed none of the
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
   how the plan is stamped (`stampPassage`, `keeper/internal/render/pipeline.go:1935-1947`). **A
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
   before the host dispatch of their own Passage** (`keeper/internal/scenario/run.go:548-561`,
   called at `:791` before `dispatchPassage` at `:820`), so in a one-Passage scenario a keeper task
   written *after* host tasks runs *before* them. Under this ADR it runs where it is written.
   `examples/service/long-runner/scenario/serial_waves/main.yml:24-33` is that shape: two
   `core.state.set` steps written after the waves run before them today and after them under this
   ADR — which is what its own description says it means ("state is committed ONCE after all
   waves"); a failed wave now leaves them unwritten. The scenario audit (item 6) lists every keeper
   task written after a host task. Task *k+1* waits for a keeper task *k* by construction — the
   loop is sequential and a keeper failure aborts before the next dispatch. What a keeper task does
   not get is a *cross-host barrier*, because it targets no host (`groupByHost` skips it,
   `keeper/internal/scenario/dispatch.go:588-594`); there is nothing for such a barrier to wait on.

4. **The Soul receives only rendered inputs; whatever depends on another message is settled on
   the Keeper** (user, 2026-10-01 — fork 2). The host needs its inputs, fully rendered, and nothing else:
   `params:` for a module, the rendered `input:` for a destiny.

   - **`when:` is evaluated on the Keeper**, per host, against that host's accumulated registers and
     its `soulprint.self`. A host where it is false is not sent the task. Five things follow; all but
     the last are new:
     - **The skip is still recorded.** A Soul-side skip reports SKIPPED with a register that reads
       `skipped: true` (`soul/internal/runtime/applyrunner.go:1414-1421`), and a later
       `register.X.changed` reads `false` from it. A task the Keeper never sends writes nothing, and
       that read would raise instead. The Keeper writes the SKIPPED event and register for that host
       itself — as it must for a host the requisite gate drops (`crosspassage.go:107-115`). Inside a
       group the child is sent with `when: false` instead of being left out, so the Soul writes the
       SKIPPED register in the message, where a sibling's open predicate can read it. That `false`
       holds on some hosts and not others, so it travels in the per-host overlay
       ([predicate-rendering.md §5](../scenario/predicate-rendering.md)).
     - **An evaluation error fails *that host's task*,** judged by `on_failure`, not the whole run —
       inside a block rolled as one group, where the child cannot fail alone, the group on that host,
       with the records [predicate-rendering.md §1](../scenario/predicate-rendering.md) gives it.
       Today every render error leaves the whole pass and the stage loop aborts the run
       (`keeper/internal/scenario/run.go:731-737`), as a failing static `when:` does
       (`render.evalStaticWhen`; `shared/cel/register_index.go:31-35`). This needs error capture per
       task and host, and a Keeper-written FAILED event, `apply_runs` row and audit record.
     - **Flow control is evaluated for the active task only.** Already-active tasks are re-rendered at
       every step (§Price); re-evaluating a finished task's `when:` against registers written since
       could fail the run over a task that has already run.
     - **A decided `when:` is cleared from the wire.** Today a host where it holds still receives
       `RenderedTask.When` (`keeper/internal/render/prototask.go:121`): the Soul would evaluate
       `register.<earlier>` against registers it no longer has and fail with
       `flowcontrol.when_error`, and the field alone makes the Keeper demand `CapabilityFlowControl`
       (`keeper/internal/scenario/soulcompat.go:97-99`). A decided `when:` is blanked — except inside
       a group, where a false one travels as `false` and an open one as its residual, both per host.
     - **The static-only rules stay in S1.** An applier's, a keeper task's and an `include:`'s `when:`
       must be static today (`guardApplierWhen` and `guardKeeperWhen`,
       `keeper/internal/render/pipeline.go:1881-1887`, `:1914-1920`;
       `shared/config/include_expand.go:451-467`); whether the Keeper-side evaluator lifts them is a
       later decision. `when: register.self.*` is refused at compile: the task has not run yet.

     So the static/dynamic classification, which NIM-909 found load-bearing and fragile, stops
     deciding where a module task's `when:` runs — and only that. The predicate language does not
     change: predicates compile in the flow-control environment (`cel.NewFlowControl`,
     `shared/cel/engine.go:190`), not the render one (`cel.New`, `:175`), so `compute.*`,
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

     and the Soul receives `register.self.stdout != "bob"`.

     **The rewrite owes an equivalence, not an algorithm**, and is specified in
     [docs/scenario/predicate-rendering.md](../scenario/predicate-rendering.md). For every value the
     open set can take, the predicate the Soul receives gives what the author's predicate gives
     against that host's full context; where the Keeper cannot guarantee that, the host's task —
     inside a group, the group on that host — fails before dispatch with a named code. Every trap
     the reviews found is a case in a differential test S1 must pass. **The open set is
     `register.self`, and inside a group also the group's own registers**, which the Soul
     accumulates inside the message, as a destiny does. Three things have to be built, each absent
     today — a per-host overlay for the predicates (params got one in NIM-908, `ParamsBySID`;
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
   per host, from the audit log's CHANGED sets (`keeper/internal/scenario/crosspassage.go:94-178`).
   Under this ADR every requisite source outside a group rolled as one is in an earlier message, so
   that branch runs on every
   dispatch instead of rarely — an applier source included, which it got wrong until NIM-931
   (§`onchanges:`). Inside a group, a child the gate drops on a host is sent there as `when: false`
   (decision 4), so its siblings still see its SKIPPED register. **`onfail:` is a different matter
   and is fork 1.**

6. **Destiny is unchanged.** Passages never existed inside a destiny and none are introduced;
   `vars.yml` and task params are already resolved per host.

7. **`async:` and `require:` outside a destiny are deferred to NIM-906.**
   Neither has anything to act on when a message holds one task. ADR-0075's barriers are `require:`,
   an implicit register reference, and the end of the run — *"which is the end of THIS
   ApplyRequest"* (`soul/internal/runtime/applyrunner.go:316-319`); and a `require:` source in an
   earlier Passage is already encoded as the sentinel `-1`, which for a barrier reads *"nothing to
   wait for"* (`soul/internal/runtime/async.go:182-184`,
   `keeper/internal/render/prototask.go:111-117`). Inside a destiny both still work and ADR-0075
   applies unchanged (a block refuses `async:` today, `shared/config/scenario_task.go:1097-1099`). A key that stays legal and stops having an effect is the silence this ADR is about, so
   both become **refusals** — `async:` at top level, and `require:` anywhere outside a destiny:
   `async:` is already refused inside a block (`keeper/internal/render/block.go:458-459`), so a
   block child's `require:` would wait on nothing. The shape of those refusals is NIM-906's.
   `examples/service/example-cloud-bootstrap/scenario/existing-vm/main.yml` uses a top-level
   `require:` (`:56`, `:71`) and is migrated with them.

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
  cross-reference existence check, the Soul's `async:` join set and `config.IsStaticPredicate` stay —
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
> dropped `include:` group (`:209-214`), a statically-false `when:` (`:973-975` →
> `dispatch.go:588-606` → `:66-73`), a keeper-side task (decision 3), and a task whose `where:` or
> requisite gate leaves it with no host (`crosspassage.go:107-115`). Under the happens-before
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
  (`keeper/internal/scenario/run.go:731-738`). It does **not** render the whole plan — a task in a
  future Passage is emitted as a placeholder and its register-dependent keys are not resolved
  (`keeper/internal/render/pipeline.go:264-277`) — but every *already-active* task is rendered
  again, so task 0 is fully rendered T times, `${ vault() }` included. See §Consequences for why
  that is a correctness question and not only a cost.
- **One Keeper↔Soul round trip per step per host**, where there was one per Passage.
- **Write amplification**: one `apply_runs` row per step per host, where a whole Passage shared one
  — and that row is public.
- **Crash recovery for scenario runs is lost until the Acolyte path is restored.** The work-queue
  dispatch of [ADR-027](0027-apply-work-queue.md) is gated off for staged runs —
  `if r.acolyteEnabled && !hasSerialTask(scn) && !staged` (`keeper/internal/scenario/run.go:580`),
  with `staged := passage.Count > 1` (`:405`). Under this ADR every scenario with two or more
  steps is staged — a single default block with two children included — so `dispatchPlanned`, claim/fencing and `ReclaimApplyRuns` become
  unreachable and every run inherits the limitation `:464-466` already names.
- **A straggler now slows every step**, and `serial:` multiplies the wave count.

**No number in this ADR is a measurement of the change.**

## Consequences

### ★ Fail-stop becomes fleet-wide — the largest behavioural change for existing scenarios

Today most scenarios are a single Passage: every host gets one message and they run independently.
A host that fails at task 3 does not stop host B from reaching task 12 — B's message was already
dispatched and B runs it to the end. The failure is observed at the barrier, after the fact.

Under this ADR the barrier sits between every pair of tasks, and one host's failure ends the run
there: `classify` returns an error on any host in `failed`/`cancelled`/`orphaned`
(`keeper/internal/scenario/dispatch.go:505-513`), `waitBarrier` propagates it (`:426-429`, surfaced to the stage loop at `:108-110`), and
`run()` aborts (`keeper/internal/scenario/run.go:820-823`). **Tasks 4..12 are then dispatched to
nobody — including the hosts that were healthy.**

This is not a new mechanism; it is today's barrier semantics applied T times instead of once. The
precedent is `serial:`, which already behaves this way within a Passage: *"First host, finally-failed
after the inherited `retry:` is exhausted, stops rolling: subsequent waves will not start"*
([orchestration.md §2.2.1](../scenario/orchestration.md); the per-wave barrier at
`dispatch.go:103-110`). What changes is that the blast radius of one host's failure grows from "its
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
| `scenario` | **The default.** The run ends at the barrier where the host failed; no later task is dispatched to anybody. |
| `host` | Only that host. It leaves the run; the remaining tasks go to the rest. |

A threshold sits beside it, `tolerate`: with radius `host`, the run still ends once **more than** N
hosts — or N% of the run's hosts — have failed in total. Together the two reproduce Ansible's
`any_errors_fatal` and `max_fail_percentage` (user, 2026-10-01), so they read the way an operator
already expects. Neither the unit nor the arithmetic is [ADR-043](0043-voyage.md)'s: the unit is a
failed host of this run, not a run unit, and "more than N" is **off by one** from `max_failures: N`,
which stops *at* the N-th. The full rules are in [docs/scenario/settings.md](../scenario/settings.md).

★ **Choosing this vocabulary also removes the collision, not just the confusion.** `RunSpec`
(`keeper/internal/scenario/scenario.go:229-289`) carries no failure policy today; ADR-043's keys live
on the `voyages` row and are read only to decide whether to start the next Leg
(`keeper/internal/voyageorch/scenario.go:235`). Had the task barrier read that field with ADR-043's
*values*, every existing `kind=scenario` Voyage — which defaults to `continue` — would have silently
switched its within-scenario barrier to drop-and-continue. **The task-barrier policy is therefore a
separate field, declared in the scenario and fed by nothing at run time** (fork 4), and its value
vocabulary is disjoint from ADR-043's, so the two cannot be confused even by eye.

**Three terminal statuses, and they are not interchangeable.** `classify` ends the run on `failed`,
`cancelled` *and* `orphaned` today (`keeper/internal/scenario/dispatch.go:505-513`):

- `failed` is a host failure and is what the policy governs. `orphaned` would be too, but it is
  only ever written on the Acolyte path, which linear runs do not take — a dead host is caught
  instead by `soul_timeout` ([settings.md §2.2](../scenario/settings.md)).
- `cancelled` **ends the run regardless of the policy.** An operator's cancel is an instruction, not
  a failure, and "keep dispatching because cancellation is not counted as a failure" would be
  absurd. ADR-043 excludes `cancelled` from its failure *count* for a different purpose and that
  exclusion does not transfer.
- `no_match` does not arise here: it is written only on the Acolyte claim path
  (`keeper/internal/scenario/claim.go:123,153`), which §Price establishes is unreachable for a
  multi-task scenario; the inline path gives a non-targeted host no row at all.

⚠ **What `on_failure: host` costs, stated so it is chosen with open eyes.** The host set shrinks mid-run, so
`serial:` widths and `soulprint.hosts` change under the author's feet between tasks; a host that
failed at task 3 is simply absent from task 7's `where:` result, with nothing at that point saying
why; and the run needs a terminal status meaning "finished without some hosts", which the run
status does not have — `applyrun.RunStatus` (`keeper/internal/applyrun/runsview.go:20-37`), exposed
as the inline enum `applying,success,failed,cancelled` (`shared/api/wire/incarnation.go:182`,
`:231`). **Decided (user, 2026-10-02): such a run ends `partial_failed`**, the value a Voyage and
the push views already use for "some failed, the rest finished" (`shared/api/wire/enums.go:114`,
`:126`, `:162`); it joins the run-status enum, which is a public-schema change (item 4). None of
this argues against offering `host` — it argues for `scenario` being the default, which it is.

This also settles what §Provenance asks: under `on_failure: host` the dropped host's remaining tasks are a
real outcome and need a record, so "not reached" synthesis ships with the policy, not separately.

### Top-level `onfail:` would become unreachable — fork 1, answered

Today a rescue works because the failure and the rescue travel in the same message: the Soul runs
the rescue tail itself after a fail-stop (`soul/internal/runtime/applyrunner.go:334-341`,
`:492-513`), and requisites are deliberately not passage-defining
(`keeper/internal/scenario/crosspassage.go:7-11`), so source and rescue stay together.

Under this ADR every top-level `onfail:` source is in an earlier message, and by the section above
its failure ends the run before the rescue message is built. The onfail call to `resolveKind`
(`crosspassage.go:136`) can only fire inside a run the barrier has already ended.

Under `on_failure: host` it does not reach the failed host either: that host has been dropped, and a
dropped host receives no later task. So the rescue misses its one target under **both** radii.

This is not created by this ADR — cross-passage `onfail:` is already dead the same way today; it is
simply rare. This ADR would make it the only shape a top-level `onfail:` can take. **The user's answer
(fork 1): rescue is extended across barriers** — a failed host still receives the `onfail:` tasks
whose source failed on it — under `on_failure: host` after it has been dropped, under `scenario`
before the run ends.

### `onchanges:` — already answered, and now on the only path

`remapRequisites` encodes "source not in this message" as `-1`, on which the Soul reports
`changed/failed = false` (`keeper/internal/render/prototask.go:186-194`). With one step per message
every requisite source outside a group rolled as one is outside the message, so on the sentinel path alone every `onchanges:`
would stop firing silently — the exact silence this ADR is escaping.

It does not reach that path. `applyGate` resolves the requisite Keeper-side first, and either strips
it from the wire (a source changed → consumer runs) or drops the consumer from the host slice
(`crosspassage.go:153-178`). A source that was SKIPPED, or filtered out by `where:` on that host,
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
over the destiny's children (`soul/internal/runtime/applyrunner.go:679-691`, `aggregateRegisterData`
at `:1478`). Inside one message that is enough, because the Soul gates on the register. The Keeper
gates on the status, so `onchanges: [<applier>]` resolved Keeper-side reads "not changed" every time;
and after a child's fail-stop the terminal is still emitted as OK, carrying the aggregate
(`:492-500`), so `onfail: [<applier>]` never sees FAILED either. Latent today — a requisite does not split a Passage, so the two rarely sit
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
(`keeper/internal/scenario/run.go:486-493`, detector `shared/config/passage.go:466-494`). That is
loud and right. But the detector will now fire on *every* `onchanges:`/`onfail:`, so an audit log
becomes an operational prerequisite for any scenario with a requisite.

### `passage` becomes the step's index, not a dispatch counter

The column cannot be deleted: the proto field is only-add (`proto/keeper/v1/apply.proto:300`, echoed
at `:403` and `:475`), it is in the primary key `apply_runs (apply_id, sid, passage)` and the
foreign key from `apply_task_register` (migration `078_add_apply_runs_passage.up.sql`), in the audit
record (`shared/audit/task_executed.go:56-60`), in the register accumulation query's
`WHERE passage < $2` (`keeper/internal/applyrun/taskregister.go:129-142`), and on the **public**
surface — `json:"passage"` in the API wire types (`shared/api/wire/incarnation.go:197,266`), a
printed column in `soulctl incarnation` (`soulctl/internal/cmd/incarnation.go:302`) and the
documented projection *"One host corresponds to N rows (by Passage)"*
(`keeper/internal/applyrun/runsview.go:60-62`).

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
takes the passage from that position (`keeper/internal/render/pipeline.go:196-199`); the `continue`s
for a dropped `include:` group (`:213`) and an `assert:` (`:236`) skip `idx++`, and `idx` counts
**RenderedTasks**, not top-level entries. `i` always advances. `config.Passage.TaskPassage` is
already indexed by that position (`shared/config/passage.go:62-69`), so `passage` is input-independent
as it stands, and the future-task placeholder gate — `passage > in.ActivePassage`
(`pipeline.go:268`) — is **already keyed to the top-level ordinal**. This decision redefines what
goes *into* `TaskPassage`, not how the loop counts — with one exception.

⚠ **A default block's children each need their own number, and that is a re-keying.** Today every
descendant of a top-level task takes its passage — *"block is an atomic Passage unit"*
(`stampPassage`, `keeper/internal/render/pipeline.go:1935-1947`). A message is selected by
`t.Passage == p` (`tasksForPassage`, `keeper/internal/scenario/dispatch.go:558-566`), the barrier
filters host rows by it (`classify`, `:475`), `apply_runs` is keyed `(apply_id, sid, passage)`
(migration `078_add_apply_runs_passage.up.sql`), and registers load with `passage < $2`
(`taskregister.go:129-142`). With one number shared, a default block's children would travel in one
message or, sent separately, collide on that key, and none could read its sibling's register. So
the index counts steps, each child of a default block takes one, and `stampPassage` and
`TaskPassage` are re-keyed to it — still from the plan alone, since a block's mode is written in
the file. **The placeholder gate is also applied inside block rendering**: today the gate runs once per
top-level entry (`keeper/internal/render/pipeline.go:196-199`, `:268`) and `renderBlockTask` then
renders every child in the same pass (`:367-368`), each child's own `where:` included
(`keeper/internal/render/block.go:256-264`). So while child *k* is active, child *k+1* would be
rendered too and a read of *k*'s register would raise. The gate and its placeholder are applied per
child as well, in `walkBlockChildren` (`block.go:132`), before a child's targets are resolved
(`:181`, `:195`); the top-level gate stays, keyed to the block's first step, so the block's own
`where:` (`keeper/internal/render/pipeline.go:301`) is not resolved early either. And fork 3's rule
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
against an empty register (`keeper/internal/render/pipeline.go:264-277`, contract at
`keeper/internal/render/render.go:268-279`). There is no contradiction, and the mechanism this ADR
needs already exists.

The gate compares `passage > in.ActivePassage`, so it works unchanged once `passage` is the step
index — it compares the active step against the task's own. **The render pipeline's load-bearing
changes are that numbering, the gate inside a default block and the held block targets**
(§`passage`): a default block's children each take their own number, each is a placeholder until
its step, and the block's `where:` and `run_once:` are resolved once.

⚠ **The `${ vault() }` exposure widens, and memoization already exists — its SCOPE is the change.**
`cel.ReadKVMemoized` memoizes a secret read for the duration of one render pass
(`shared/cel/vault.go:156-181`), bound on the context by `Pipeline.Render`
(`keeper/internal/render/pipeline.go:124,130`) and used by both the `vault:` phase and CEL
`vault()`. What changes is that a run now has T passes instead of N, and the memo does not span
them. Two consequences, and the second is the serious one:

- an already-active task's re-render is **discarded** — `tasksForPassage` keeps only the active
  ordinal (`keeper/internal/scenario/dispatch.go:558-566`) — so re-resolving task 0's secret at every
  later step is pure cost and Vault load, not a correctness problem;
- **a secret that disappears mid-run now fails the whole run, T times more often.** Every pass
  re-resolves every already-active task's secrets, and a failure there is `render_failed`
  (`keeper/internal/scenario/run.go:731-737`) — so a rotated-away path, or Vault being briefly
  unreachable at step *k*, kills a run over a secret that only a *completed* task ever needed.

Widening the memo from pass to run fixes both. It is S1 because the frequency, not the mechanism,
is what makes it matter.

### `serial:` — the width collapses between tasks, the wave count multiplies

Today the wave width is one per Passage: the minimum positive `serial:` among the tasks *of that
Passage* (`keeper/internal/scenario/dispatch.go:636-647`), aggregated for one documented reason —
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
fires where ADR-056's derivation — kept for this check alone — would put a top-level host task
carrying `serial:` in one Passage with a later host task, which is exactly the set of scenarios
whose meaning changes, and it goes when the derivation goes, after the service repositories are
audited. No example in this tree triggers it. **And it must ship before linear
dispatch is turned on, not in a later slice** — the meaning changes in S1, so a diagnostic that arrives in S2 is
a window of silent regression. The audit of existing scenarios belongs in the same window.

### `retry:`/`until:` survives, because it never crosses a boundary

The retry loop is entirely inside one task inside one `Run`
(`soul/internal/runtime/applyrunner.go:802-903`), and `until:` is evaluated against `register.self`
merged into `registerByName` (`:931-932`). A barrier around the message does not touch it.

A **cross-task** register inside `until:` was the open edge: FC-5 excludes `retry.until` by design
(`shared/config/passage.go:579-588`), and on the Soul such a read dies as `flowcontrol.until_error`
→ task FAILED (`:867-873`). Decision 4 closes it differently: the Keeper renders the reference into a
value before the task leaves, so `until:` arrives reading only `register.self`.

### Provenance: finer per step, thinner after a failure

Each dispatched step gets its own `apply_runs` row, status and events. A 12-task destiny
is still one task, so it still yields one row per host.

⚠ **But a failure now records less.** Today a fail-stop still emits a SKIPPED `TaskEvent` for every
remaining task of the message (`soul/internal/runtime/applyrunner.go:492-513`), so the audit says
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
the module's result (`soul/internal/runtime/applyrunner.go:1146-1157`, `:867`), so deciding them on
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
its source FAILED (`keeper/internal/scenario/crosspassage.go:136`). But it only *filters* the hosts
already in the consumer's plan — it cannot add one — and the run aborts at the barrier before the
rescue message is built. Removing the abort is not enough; the rescue still misses unless S1 also
builds these:

- **a dropped host stays targetable** by an `onfail:` consumer whose source failed on it — under
  `on_failure: host` it has left the roster;
- **an applier source** resolves from its children (§`onchanges:`) — shipped in NIM-931;
- **a failure with no task row** — `send_apply_failed` (`keeper/internal/scenario/dispatch.go:336-340`),
  an expired `soul_timeout`, a push transport error — is written as a
  FAILED event for every task the message carried: the top-level task and each child of its group,
  by their plan indexes — a SKIPPED one for a child the Keeper had decided to skip on that host.
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
RunResult stays FAILED"* (`soul/internal/runtime/applyrunner.go:334-339`). And **a rescue's own
failure is judged like any task's** (user, 2026-10-01), by its own effective `on_failure`: under
`scenario` it is one more entry in the failure list — the host leaves two errors, the source's and
the rescue's — and under `host` the host, already out, stays out and the log records that it left.
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

**Re-confirmed after review round 7's evidence (user, 2026-10-01).** The tree's own canary is a plain
block: `examples/service/dragonfly/scenario/restart/main.yml:104-132` puts "restart dragonfly" and
"wait until the replica has resynced with master" in one block under `serial: 1`, so that rolling
does not move to the next replica until the current one has caught up. Under this default, without
`serial_scope: block`, the same file restarts every replica, one at a time, and only then checks any
of them. The user kept the default anyway: a block is first of all a way to group tasks, as in
Ansible, where a block's tasks run one after another across the hosts — the canary is what
`serial_scope: block` is for. What ships with that answer:

- **The dragonfly example gets `serial_scope: block`** in the change that switches the default.
- **A refusal with an explicit opt-out** on every block that has `serial:` — its own or inherited —
  and two or more host-dispatched tasks beneath it, counted through nested blocks on the list after `include:`
  expansion, where the flat checks already run (`shared/config/include_expand.go:163-166`): its
  effective `serial_scope` must
  come from a line the author wrote — on the block or on any level above it — either value. Written
  out, the default is a choice; left implicit, it is the silent change this ADR refuses. It lives in
  the shared validator, so the Keeper refuses such a scenario too: service repositories are not
  linted, and a `soul-lint`-only check would leave the change silent there. That example is the only
  such block among the examples. The `soul-lint` fixture
  `soul-lint/testdata/scenario-broken/scenario-within-block-peer-register.yml` has the same shape and
  gets `serial_scope: block`, or it stops guarding `within_block_register_dependency`. Service
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
   value from before the write.
3. **The `settings:` block** ([docs/scenario/settings.md](../scenario/settings.md)): one type and one
   validator at every level; key-by-key merge; the placement rule by kind (task / barrier / block / run); the
   barrier **waiting for every dispatched host** instead of returning on the first failure, **not
   force-failing hosts still working**, and judging each by every failure it reported, kept as an
   append-only list; `soul_timeout` as Keeper's dead-host detector on the inline path;
   `scenario_timeout` replacing the 5-minute `defaultRunTimeout`. **No run-level override, and no field
   for one on `RunSpec`.**
4. **A terminal status meaning "finished without some hosts"**, required by `on_failure: host`. The
   run status is `applyrun.RunStatus` (`keeper/internal/applyrun/runsview.go:20-37`), on the wire
   `applying,success,failed,cancelled` (`shared/api/wire/incarnation.go:182`). **Decided (user,
   2026-10-02): the run ends `partial_failed`** — the value Voyages and push views already use for
   "some failed, the rest finished" — **the incarnation ends `error_locked`**, as after any run that
   did not apply everywhere, and **a Voyage counts the run as failed**. It needs the enum change, a
   `naming-rules.md` row and the companion web UI.
5. **What `on_failure: host` needs besides the status.** First, a non-obligation stated so it is not
   re-litigated: **no RBAC gate** — with no run-time override, whoever may *merge* the scenario
   decides its radius, so the control is code review on the service repository. Then the obligations,
   each absent today: **the drop set must survive a roster re-resolve** — a refresh boundary re-reads
   the live roster (`keeper/internal/scenario/run.go:655-662`) and would re-admit a dropped host that is
   still online; **"not reached" records** for the dropped host's remaining tasks; **audit of the drop itself** — nothing records "host X left this run at barrier
   k", and §Provenance's "not reached" synthesis covers the dropped host's *tasks*, not the drop;
   **state capture on a dropped host** — §"State capture: a capture now waits for the steps before it" is
   written for the `scenario` radius and is not re-examined for a run that keeps committing `core.state.<verb>`
   steps after a host has left; **`soulprint.hosts` mid-run** — it shrinks between tasks, and
   nothing says whether a later task sees the pre-drop or post-drop roster.
6. **The canary diagnostics and the scenario audit ship before linear dispatch is enabled**, not in a
   later slice: the one for top-level `{change, check}` pairs (§Consequences) and the one for a block
   with `serial:` that does not write its mode out (fork 3). The audit also lists every keeper task
   written after a host task (decision 3). The fixtures with a block under `serial:` migrate with
   them: the `soul-lint` fixture named in fork 3, the inline YAML at
   `shared/config/passage_test.go:116-118`, and `TestDispatch_BlockSerialWave`
   (`keeper/internal/scenario/block_serial_test.go:13-16`), which asserts the whole-block wave the
   new default flips.
7. **The Acolyte path.** **Decided (user, 2026-10-02): restoring work-queue dispatch for linear runs
   blocks enabling linear dispatch**, rather than following later: the default radius ends a whole
   fleet's run mid-flight, and the wall clock grows T-fold, so the chance of a run being alive across
   a Keeper redeploy — with no reclaim — rises on both axes at once.
8. **The `passage` capability gate's blast radius.** `gatePassageCapability`
   (`keeper/internal/scenario/soulcompat.go:250`; the capability constant is
   `shared/config/soul_capability.go`, contract at `proto/keeper/v1/lifecycle.proto:23-26`) today
   gates the rare staged run, fail-closed as `soul_passage_unsupported`. It will gate *every*
   multi-task scenario, so one Soul that never announced the capability stops scenarios for its whole
   fleet.
9. **`AuditReader` becomes an operational prerequisite**, not a diagnostic one
   (`keeper/internal/scenario/run.go:486-493`).
10. **A capability bit is needed for the rendered predicates.** An earlier revision required one for
    carried registers; that reason is gone, but another replaces it. Literal-aware text guards and
    masking at the echo sites are Soul-side changes, and an older Soul compiles the *normalized*
    text (`shared/cel/eval.go:70`, `:77`): whitespace after an escaped quote collapses into a
    silently different literal, and a substituted secret is echoed in its errors. So a Soul that does
    not announce the bit is not sent a rendered predicate — the run is refused, fail-closed, as
    `gatePassageCapability` refuses today (item 8), with the same blast radius: one such Soul stops
    every scenario that carries a rendered predicate to it. A `when:` the Keeper decides sends
    nothing new — for a step it is blanked, and a grouped child's `when: false` is a predicate any
    Soul already evaluates — and a predicate in which no reference was resolved travels as written and
    needs no bit. One catch: a child with no `when:` of its own that the requisite gate drops on one
    host now carries `when: false` there, and a non-empty `when` makes the capability gate demand
    `CapabilityFlowControl` — from the shared `When` today
    (`keeper/internal/scenario/soulcompat.go:97-99`). That gate must read the per-host value, and
    so run after the requisite gate; today it runs first (`keeper/internal/scenario/run.go:769-771`,
    the requisite gate applied at `keeper/internal/scenario/dispatch.go:65`). And per step it would
    refuse late: today a one-Passage run is checked before any dispatch (`run.go:753-766`), while a
    per-step check refuses step *k* after the steps before it have changed hosts. So the check is
    kept **up front over the whole plan** for everything known before the run — modules and
    features — each task checked against the hosts its targeting resolves to before the run, so a
    host a static `where:` excludes cannot refuse it. That needs a pass resolving every task's
    static targeting and features up front: today a future task is a placeholder whose only input
    to the gate is its module — no hosts, no flow-control fields
    (`keeper/internal/render/pipeline.go:264-277`). Three refusals stay late, and are stated rather
    than hidden: a task whose `where:` reads a register, whose hosts are unknown until its step — the
    reason the gate runs per Passage today (`run.go:756-760`); a host admitted at a refresh boundary,
    which did not exist when the up-front check ran (`run.go:674-676` says so for the `passage` bit);
    and the per-host flow-control part, which depends on the results of earlier steps.
11. **The public `passage` surface** — one row per host becomes T rows per host in the OpenAPI
    projection (`keeper/internal/applyrun/runsview.go:60-62`), in `soulctl incarnation`
    (`soulctl/internal/cmd/incarnation.go:302`) and in the companion web UI.
12. **Keep `within_block_register_dependency` for a block rolled as a group** — a default block no
    longer needs it — and establish whether a destiny has an equivalent.
    If not, add one or state the gap in [docs/known-limitations.md](../known-limitations.md).
13. **Checks.** Refusals for top-level `async:` and for `require:` outside a destiny (NIM-906 owns
    their shape) live in the shared validator, so the Keeper enforces them too — service
    repositories are not linted. In `soul-lint`: a warning for a child's `serial:` or barrier-kind
    setting inside a block rolled as one group ([settings.md](../scenario/settings.md) rule 4), and
    removal of the diagnostics that only meant something under derivation
    (`soul-lint/internal/validate/stage.go`).
14. **A real benchmark** on a fleet, on a scenario with several top-level tasks, measuring separately:
    the round trips, the Soul's per-message overhead, and the quadratic re-render with its Vault
    load.
15. **Operations and rollout.**
    - **HA Keepers of different versions are not fail-closed here.** The same `{change, check}` under
      `serial:` rolls as a canary or across the whole fleet at once depending on which Keeper picked
      the run up. The execution model must be **pinned to the run** at its start and recorded, not
      read from whichever instance is serving.
    - **Rollback:** a cross-task `when:` is legal in the new model and an older Keeper refuses it
      (FC-5) — loudly, which is acceptable, but it means a scenario authored after the change cannot
      run on a rolled-back Keeper. What goes *silent* on rollback is the order: an older Keeper runs
      keeper tasks before host dispatch again and lets healthy hosts run on after one fails. The
      release note says both.
    - Barrier timeout and straggler policy; a run started under ADR-056 while the Keeper is
      redeployed.
16. **Applier sources resolve from their children** for the cross-barrier gate (§`onchanges:`), with
    a guard test in the `rotate_tls` shape. Shipped ahead of the train in NIM-931.
17. **Decision 4's machinery**, each piece absent today except the guards it lifts: a rewrite that
    meets the equivalence in
    [predicate-rendering.md](../scenario/predicate-rendering.md), with the differential test
    covering every trap listed there, and the walk that checks nothing outside the open set is left;
    `guardFlowControlHostInvariant` and `flowContextHostInvariant` lifted for rendered predicates; `register: self` reserved, as
    `register_name_reserved` already reserves `hosts` (`shared/config/scenario_task.go:1812-1820`),
    and a bare `register` refused in the post-module predicates;
    their per-host overlay; Soul text guards that leave a substituted value alone; masking at every
    echo site; Keeper-written SKIPPED and FAILED records; error capture per task and host.
18. **Rescue routing** — fork 1's four obligations.

## Slices

- **S1 (atomic)** — the linear dispatch loop; `passage` as the step index, a default block's
  children each numbered and gated per child, its targets held; the execution model pinned to the run (item 15); the `vault()` memo
  widened to the run; Keeper-side `when:` and partial evaluation of the post-module predicates,
  with their secret masking, and items 16-18; the `passage` capability bit and item 10's new one;
  decision 7's `async:`/`require:` refusals, which must land when those keys stop having an effect;
  FC-5's retirement on both sides at once — the Keeper's refusal (`keeper/internal/scenario/run.go:448-449`)
  and `soul-lint`'s (`soul-lint/internal/validate/stage.go:223`) — or one of them refuses scenarios
  that are now legal; for the same reason `soul-lint`'s Passage-keyed state checks —
  `StaleStateRead` (`:258`) retires, since a capture now finishes before a later reader renders, and
  `StoreAfterUse` (`:240`) is re-keyed to plan order;
  fork 1's outcome; the `settings:` block with its fail-by-default, its one
  validator, the key-by-key merge and the "not inherited from the Voyage key" statement; the terminal status
  `on_failure: host` needs and its obligations (items 4-5); the canary diagnostics (item 6). Any
  subset is a silent regression. The benchmark (item 14) runs before linear dispatch is enabled.
- **S2** — lands with S1, not after it: `serial:` per step, fork 3's two block modes and item 13's
  warnings for a child's `serial:` or barrier-kind setting inside a group. S1's step numbering
  already splits a default block, so the modes that decide which blocks split cannot come later.
- **S3** — the Acolyte path, which must be in before linear dispatch is enabled (item 7), and the
  public `passage` projection.
- **S4** — `soul-lint`: drop the other derivation-only diagnostics.

`within_block_register_dependency` is removed in **no** slice; it is narrowed to blocks rolled as one
group.

## Relation to other ADRs and documents

| Document | Effect |
|---|---|
| [ADR-056](0056-staged-render-passage.md) | **superseded** — the derived N-Passage plan is replaced by a happens-before order; FC-5 retires, since every reference across a barrier is resolved on the Keeper |
| [ADR-012(d)](0012-keeper-soul-grpc.md) | **amended** — for a scenario task, `when:` is evaluated on the Keeper and the post-module predicates arrive with everything but `register.self` — and, in a group, the group's own registers — rendered in, per host, behind a capability bit (item 10); a grouped child's `when:` that still reads a sibling goes to the Soul the same way; no register crosses the wire, and `flow_context` loses its purpose |
| [ADR-0075](0075-intra-host-async-tasks.md) | still applies inside a destiny; top-level `async:`, and `require:` outside a destiny, must be refused (NIM-906) |
| [ADR-027](0027-apply-work-queue.md) | **affected** — the work-queue path is unreachable for linear runs unless restored |
| [ADR-009](0009-scenario-dsl.md) | **amended** — a `settings:` block with one schema at every level, inherited as a value like `vars:` (not composed like `when:`/`where:`), the first execution-behaviour container in the grammar; the execution model changes. Block inheritance of `retry:`/`timeout:`/`async:` is a **separate** amendment, NIM-910 |
| [ADR-010](0010-templating.md) | unaffected — the register grammar and the NIM-909 amendment stand |
| [ADR-0084](0084-explicit-state-capture.md) | **amended** — a capture runs where it is written (§State capture); its per-Passage state refresh and stale same-Passage read rule (`0084:228-235`, `:328-341`) become per step, so that stale read can no longer arise and its `soul-lint` check retires in S1 |
| [orchestration.md §2.2.1](../scenario/orchestration.md) | **rewritten** — the per-Passage `serial:` minimum, the wave/barrier invariant and the canary idiom |
| [ADR-043](0043-voyage.md) | **unaffected, and nothing is reused** — a disjoint value vocabulary (`host`/`scenario` vs `abort`/`continue`), a different unit (a host at one barrier vs a run unit), and a threshold off by one from `max_failures`. The Leg policy and `soulctl --on-failure` stay exactly as they are |
| [naming-rules.md](../naming-rules.md) | **new rows** — `settings` and its six keys (`serial_scope` included), the refusal code for a run-kind key below the scenario level, the residual-check code and the code for a predicate the Keeper cannot carry, the refusals of a bare `register` and of `when: register.self.*`, `self` under `register_name_reserved`, and `partial_failed` joining the run-status enum. The live Voyage table has no `on_failure` row to amend, and the `wave.on_failure` row is **Tide's**, dead since ADR-043 |
| [orchestration.md §2.2.3](../scenario/orchestration.md) | **corrected** — it still shows Tide's `--wave-size`/`--wave-on-failure`/`--target-coven` invocation and the "REPLACE concurrency" rule that ADR-043 §B1 and the code contradict |
| [orchestration.md](../scenario/orchestration.md) line 273 | **closes an open question** — *"there is no tolerance threshold for partial failure (§8, open Q)"* is answered by `tolerate` at the barrier |
| [settings.md](../scenario/settings.md) | **new** — the scenario `settings:` block (fork 4); design, not implemented |
| [ADR-draft per-host params dispatch](draft-per-host-params-dispatch.md) | **builds on it** — NIM-908's `ParamsBySID` is the model for the per-host overlay the rendered predicates need; NIM-908 left flow control refused when it differs per host (open Q #25), which decision 4 answers |
| [predicate-rendering.md](../scenario/predicate-rendering.md) | **new** — how the Keeper renders a post-module predicate (decision 4): the equivalence it owes, the traps the differential test covers, what has to be built; design, not implemented |
| [known-limitations.md](../known-limitations.md) | **updated if** item 12 finds no destiny equivalent; there is no register/destiny entry there today |
| `shared/audit/task_executed.go:56-60` | **comment is now wrong** — it asserts keeper tasks carry `passage=0`, which ADR-056 Slice 2 already falsified and this ADR makes uniformly false |
