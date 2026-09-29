## ADR-draft. Per-host dispatch of a task's `params:` — open Q #25 closed for params

**Status:** active (number stamped at squash-merge, `docs/adr/draft-per-host-params-dispatch.md` until then)
**Amends:** [ADR-012](0012-keeper-soul-grpc.md) (§d — what one `RenderedTask` means when the plan reaches N hosts), [ADR-010](0010-templating.md) (params were host-invariant by construction; they are not any more), [ADR-009](0009-scenario-dsl.md) (its 2026-09-28 amendment made `apply: input:` per host and named this as the thing it could not deliver), [ADR-0082](0082-service-vars.md) (its "`apply: input:` resolves on `targeted[0]`" sentence), [ADR-056](0056-staged-render-passage.md) (a Passage re-renders, so per-host params are re-derived per Passage — noted, not changed)
**Implemented by:** NIM-908

### Problem

`apply: input:` rendered once, on the roster's first host by SID, and the result went
to every host — while `soulprint.self.*` and `register.*` were readable there and
refused nothing. Nine hosts received the first one's address, silently, and the run
went green. [ADR-009's 2026-09-28 amendment](0009-scenario-dsl.md) made that input
per host.

That fixed the value and exposed where it could not go. A task's rendered `params:`
were **host-invariant by construction**: one `RenderedTask` carried one params struct
for the whole roster, and a task that rendered differently on two hosts was refused —

```
render: task "announce master" gives host-dependent params (a.example.com vs b.example.com)
  - host variance of params is outside pilot scope (per-host ApplyRequest is an
    orchestrator-layer concern)
```

So per-host input reached a `core.file.rendered` template (whose `render_context` is
already dispatched per SID) and a destiny's `where:` and `vars.yml`, and hit that
refusal everywhere else. The operator's actual request — *every destiny and job that
runs on hosts should render its own data, not just the first host's* — was answered
for one channel and refused for the rest.

**The deferral had gone stale.** Open Q #25 reads "per-host ApplyRequest is an
orchestrator-layer concern" and defers Option B to its own ADR. The orchestrator
layer had since become per-host anyway: `scenario/dispatch.go` groups by host and
calls `ToProtoTasksForHost(perHost[sid], sid)`, `scenario/claim.go` does the same for
a claimed run, and `pushorch`'s `fanOut` already builds one `ApplyRequest` per SID.
Each host has had its own request since the `RenderContextBySID` work. What was
missing was not a channel — it was a converter that knew which host it was building
for, and a render that produced more than one answer.

### Decision

**A task's `params:` are dispatched per host.** `RenderedTask` gains
`ParamsBySID map[string]*structpb.Struct`, mirroring the existing
`RenderContextBySID`, and `ToProtoTasksForHost(tasks, sid)` answers with that host's
struct.

**No protocol change.** `ApplyRequest` is already per-SID and `RenderedTask.params`
is already one struct per request; what changes is which struct Keeper puts there.
No proto field is added, moved or reused, so [ADR-012](0012-keeper-soul-grpc.md)'s
only-add rule is not engaged and no Soul needs to know. Soul's behaviour is
unchanged: it renders nothing, it applies the params it was handed, and it is now
handed its own.

**Populated only when it changes an answer.** The map is nil unless the roster has
more than one host AND at least two of them rendered different params. One host, or
a task whose params read nothing per-host — which is nearly every task in the tree —
keeps the single struct and every existing reader is bit-for-bit unchanged.

**All-or-nothing when it is populated.** It carries an entry for EVERY targeted host,
not only the ones that differ. A partial map would send the hosts it omits back to
`RenderedTask.Params`, which is the first host by SID: correct only by coincidence,
and the coincidence is the defect this ticket exists to remove.

**`Params` keeps the first-by-SID host's struct** as the golden path, because four
readers depend on it and none of them is per-host: the plan row, the keeper-task
message masker, the trial comparators, and the sid-less converter.

#### The refusals that stay, and why each one is not an oversight

Closing this for params does not close it for everything, and the boundary is
deliberate:

- **Flow control — `when:` / `changed_when:` / `failed_when:` / `until:` — still
  refuses on a host-variant predicate.** The reason is the blast radius, not the
  plumbing: a predicate deciding differently per host changes what `changed_when:`
  means for the run's AGGREGATE result, and what a `when:`-skip on some hosts means
  for a requisite reading that register. A different decision, and NIM-908 did not
  take it. (Plumbing would not have stopped it. The predicates do travel to Soul as
  text, but `flow_context` is built by Keeper per host already and only the first
  host's is shipped — so a `FlowContextBySID` would need no proto field either, by
  this ADR's own argument.) The two fail-closed layers (`guardFlowControlHostInvariant`
  on the predicate text, `flowContextHostInvariant` on the narrowed snapshots) were
  documented as temporary "until per-host dispatch lands"; that sentence was about
  the wrong channel and is corrected rather than honoured.
- **A context with no roster still refuses a per-host root**, and that is the
  backstop the ticket asks for in any case: an `on: keeper` task, the loop axis,
  `on: [covens]`, `compute:`, a capture's `match:`, a static `when:`, an include's
  `when:`. There is no host to dispatch for there even in principle — an include
  group is spliced into the plan or dropped from it for everyone. Mechanism:
  [`cel.HostScope`](0010-templating.md), ADR-010's 2026-09-28 amendment.
- **Trial L2 refuses a per-host plan.** It applies one `ApplyRequest` to one stand,
  so a multi-host fixture has no host to be converted for; flattening would let a
  case pass while proving something about a host it never applied to. ★ And L0 does
  NOT cover it either: `compareRenderedTasks` asserts against `rt.Params`, the first
  host by SID, and the case format has no host axis. So a multi-host per-host plan has
  no trial tier that can assert what host 2 received — the unit tests in
  `keeper/internal/render` are the only place. Giving L0 a per-host assertion is its
  own ticket; stated here rather than left as a redirection to a tier that cannot
  answer.
- **The sid-less `ToProtoTasks` cannot express it**, by design, and its callers are
  held to that by an AST guard test rather than by a comment.

#### The operator-facing plan is marked, not silently flattened

`apply_run_plan` and `GET …/runs/{id}/tasks` carry ONE params map per task beside
per-host results. Showing the first host's as if it were everyone's would be the same
substitution, in the one view an operator opens to check what ran. A cell that
differs between hosts is replaced by `<per-host>`; cells that do not are still shown.

Deliberately not a schema change: no API field moves, and the companion UI needs no
release. Showing the real per-host values needs a per-host params column in the reply
and is its own ticket.

### Consequences

**The `host-dependent params` render error is gone.** A scenario that hit it now
renders and dispatches. Nothing that used to run stops running — this direction of
the change removes a refusal, it does not add one.

**The backstop closes one more hop than the first cut did.** A destiny's own
`vars.yml` resolves per host, so a local reading a per-host `input.<name>` is per-host
itself — and the reference that reaches a host-free decision says only `vars.<name>`.
Measured before it was closed: a destiny `when: vars.addr == '<second host's ip>'`,
true on the second host, was decided on the first and the task was skipped for BOTH,
silently and green. `hostVariantDestinyVars` closes that layer transitively, the
mirror of the applier-`vars:` closure one level up.

**Memory.** A task with per-host params holds N structs instead of one, for the
duration of the render pass. `RenderContextBySID` has had the same shape since the
Option A work; this extends it from one key to the whole struct for the tasks that
need it, and to no task that does not.

**The seal is unaffected, and this was verified rather than assumed.** Sealing marks
PATHS, from the author's raw `${ … }` text, once per task, before the per-host loop —
so per-host VALUES under the same path are all covered and the sealed SET cannot move
with the roster. Redaction on both observable channels reads the path set, not a
value. Guarded by `TestPerHostParams_SealedSetDoesNotMoveWithTheRoster` beside the
two older roster-invariance tests, and by a mutation that moves the collection onto
the rendered struct.

**Staged render ([ADR-056](0056-staged-render-passage.md)) needs nothing.** Each
Passage re-renders from its own accumulated per-host register, so per-host params are
re-derived per Passage exactly as host-invariant ones were. The per-host register
that feeds them is the mechanism ADR-056 already built.

**The price on the corpus is zero**, checked before merging rather than after. The
two real services (`wb/service/redis`, the public `soul-stack-services/redis` and the
in-tree `examples/destiny/redis` it mirrors) were scanned for cells that flip from
green to error. One candidate was found and it was ours, not theirs: WB's
`apply: input:` builds `config` from `register.system_acl_users.effective` — written
by a `core.state.present` step, i.e. **keeper-side**, one value for the whole run —
and the redis destiny gates a task on `when: … has(input.config.unixsocket)`, a
static predicate. Classifying the whole `register` root as per-host made that working
`when:` a refusal. The classification is per NAME as a result: a register produced by
a keeper-side task of the same scenario is host-invariant, every other name is the
host's own bucket, and a name whose producer the manifest does not show is
fail-closed per-host.

### Rejected

- **A per-host `RenderedTask` list instead of a per-host struct on one task.** It
  would duplicate every host-invariant field N times and break the plan index, which
  is the correlation key for register, requisites and `async:` joins across the whole
  run.
- **A new proto field.** There is nothing to carry: the request is already per host.
  Adding one would put the fan-out on the wire and make Soul's view of a task depend
  on how many hosts the run had.
- **Keeping the refusal and documenting the workaround** ("put the per-host part in a
  `.tmpl`"). That is what Option A was, and the operator's request is the resume
  condition open Q #25 itself names.
- **Extending it to flow control in the same change.** See above — a separate
  decision, deliberately not taken here, and still refused loudly meanwhile.
