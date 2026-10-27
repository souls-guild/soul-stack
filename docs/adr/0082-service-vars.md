## ADR-0082. Service vars replace Essence — one `vars` namespace, `incarnation.spec.essence` removed

> ★ **`examples/service/redis` is no longer in this repository.** It left with NIM-871 and lives on its own at [`soul-stack-services/redis`](https://github.com/soul-stack-services/redis). Every citation of that path below names a decision and the shape it took, not a file you can open here.

**Status:** amended — [2026-10-15 (NIM-1014)](#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late): vars are evaluated on request and bound late
**Amends:** [ADR-008](0008-coven-stable-tags.md) (essence is role-agnostic; the assembly order `default → os → coven → incarnation.spec`), [ADR-009](0009-scenario-dsl.md) (the scenario template context and its reserved names), [ADR-010](0010-templating.md) (the CEL roots and the `core.file.rendered` context), [ADR-012](0012-keeper-soul-grpc.md) (the `flow_context` key set carried to Soul); the 2026-10-15 amendment supersedes in part the var→var amendment of 2026-06-24 to [ADR-009](0009-scenario-dsl.md) / [ADR-010](0010-templating.md) (the eager-topological resolve, the eager `var_unknown_ref`, the index-form refusal, the file↔task isolation), the 2026-08-03 ladder of ADR-009 ("sideways stays refused"), the NIM-908 amendment of ADR-009 (the service layer not walked by the host-variance classification), the NIM-811 seal text of ADR-010 (the service layer exempt from the seal) and [ADR-0085](0085-entity-id-and-label.md) (the `_stack.yaml` cells its legacy-root check walks)
**Implemented by:** NIM-412 · NIM-413 · NIM-414 · NIM-415 · NIM-416, with the column drop in NIM-408; the 2026-10-15 amendment — NIM-1014

### Problem

A service repository carries default parameter values in `essence/`. A destiny and a
scenario carry local values in `vars.yml` and in task-level `vars:`. Both are "named
values the author supplies"; they are two namespaces because of one distinction, and
that distinction is about to stop existing.

The distinction was overridability. [`docs/destiny/vars.md`](../destiny/vars.md) states it
outright — *"Neither the scenario, nor the operator via the API, nor the essence service do
interrupt the values"* — whereas essence was designed to be overridden from outside, by
`incarnation.spec.essence`. Two names for two lifetimes.

**`incarnation.spec.essence` has readers and no writer.** `IncarnationCreateRequest` has
no field for it; the only write of the key anywhere is the fixture of the unit test that
covers reading it back (`TestSpecEssence`). The two
live reads are `keeper/internal/scenario/state.go` and
`keeper/internal/grpc/events_telemetry.go` — three call sites through two copies of the same
three-line `specEssence()` helper, one of which says so in its own comment — and neither has
returned anything but `nil` in production. It is a designed extension point that was
declared and never finished — and it is the last thing standing between NIM-408 and the
`incarnation.spec` column, which [ADR-081](0081-roster-at-create.md) and NIM-330 have
already emptied of everything else.

Three more things about the layer are not what they look like:

- **`essence/os/<family>.yaml` and `essence/coven/<label>.yaml` are implemented and used by
  nobody.** All thirteen `examples/service/*/essence/` directories contain exactly one file,
  `_default.yaml`, and no `examples/` tree carries an `os/` or `coven/` subdirectory at all; the
  external destiny repositories (qdrant, cassandra, dragonfly) carry no essence at all. The only
  things that exercise those two layers are the resolver's own unit tests plus two keeper tests —
  five test functions across three files, and nothing a service ships.
- **`essence/_stack.yaml` is the mirror image: documented as working, absent from the code.**
  [`docs/architecture.md`](../architecture.md) describes its `file:`/`inline:`/`when:`/
  `optional:`/`foreach:` grammar in full, with a table of operators, and
  [`docs/service/manifest.md`](../service/manifest.md) records it as an optional file of the
  layout; `keeper/internal/essence/essence.go` says
  `// Convention-based ordering (no _stack.yaml)`. A service author following the docs writes
  a file the Keeper silently ignores.
- **`_default.yaml` is first by fiat, not by name.** The resolver reads it through a hard-coded
  constant and never looks at the directory (`defaultFile = essenceDir + "/_default.yaml"`), so a
  `base.yaml` sitting next to it is simply invisible. That is fine while the layers are a fixed
  list of three — and it stops being fine the moment the DIRECTORY becomes the order, which is
  what [§3](#3-the-os-and-coven-layers-are-deleted-vars_stackyaml-becomes-real) does. Then the name
  starts carrying the precedence, and `_` is `0x5F` — between `Z` (`0x5A`) and `a` (`0x61`). Under
  `sort.Strings` the order is `00-base.yaml` < `Base.yaml` < `_default.yaml` < `base.yaml`: an
  underscore-named base is first only while every neighbour happens to be lower-case.

### Decision

#### 1. The mechanism becomes `vars`

| was | becomes |
|---|---|
| `<service>/essence/` | `<service>/vars/` |
| `<service>/essence/_default.yaml` | `<service>/vars/00-base.yaml` |
| CEL root `essence.*` | CEL root `vars.*` |
| `keeper/internal/essence` | `keeper/internal/servicevars` |
| `incarnation.spec.essence` | *removed* |

The objection to the name dies with the override. Once nothing outside the service repo can
interrupt a service var, `vars` means the same thing at all three levels — service, scenario,
destiny: *values the author of this artifact nailed down, resolvable by expressions, not part
of anyone's external contract*. What an operator is meant to supply has one home, and always
had one: `input:`.

**Merged, not merely renamed.** `vars.*` is a single flat namespace — but it is a namespace per
PASS, and the two passes never share one. Drawn as one ladder it reads as if a service's vars
reach a destiny, which is the opposite of what happens (§6), so it is drawn as two:

```
scenario pass:   <service>/vars/*.yaml  →  block: vars:  →  task vars:
destiny pass:    <destiny>/vars.yml     →  block: vars:  →  task vars:
                 ─── apply: ────────────────────────────────────────────
                 nothing crosses except what the caller writes in `apply: input:`
```

That separation is structural, not a convention: the isolated destiny `RenderInput` carries
`ServiceVars: nil` (`render/destiny.go`), and the scenario pass has no file layer at all
(`fileVars` is empty in `cel_render.go`), because `scenario/<name>/vars.yml` is documented and read
by nothing — which is why NIM-416 deletes it from the docs rather than implementing it. So a
service var and a destiny var can carry the same name and never meet; `run_dir` in the shipped
examples does exactly that.

Within a pass, block and task are ONE rung, not two. `mergeBlockInheritance` (`block.go`) folds a
block's vars into each child task's own `vars:` BEFORE the layer is resolved, so the task wins on a
collision — which is also why a block var and a task var can reference each other. What stacks at
resolve time is therefore the file (or service) layer under the merged block+task layer
(`resolveTaskVars`, `cel_render.go`). There is no operator rung, because
[§2](#2-a-fleet-overrides-a-services-defaults-by-forking-the-service-repo) removes the only one
that existed.

The name surviving a boundary whose meaning does not is stated head-on rather than removed
([§6](#6-the-asymmetry-across-the-apply-boundary-is-stated-not-removed)); an independent review of
NIM-415 went looking for destiny expressions newly seeing a service var precisely because the
single-ladder drawing above implied they could, and found none, because they cannot.

**A layer may reference the layers BELOW it.** Before this change a task var reached the service
layer by spelling `${ essence.X }` — a different root, always in scope. With one root the same
expression is `${ vars.X }`, and refusing it would turn working scenarios into `var_unknown_ref`
for no reason an author could act on. Sideways is still refused: a task var cannot see a destiny
file var or the reverse, and the service layer sitting below both gives neither a path to the
other.

> ⚠ **Superseded in part by the [amendment 2026-10-15](#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late)
> (NIM-1014).** The scenario pass gains the rung the ladder above leaves out and nothing read until
> then: the scenario's top-level `vars:`, between the service rung and block + task. "Below" stops
> being a boundary: within a pass a task has ONE view, and every definition, whatever its rung, reads
> `vars.<other>` through it — so the sideways refusal (task var ↔ destiny file var) is removed in
> both directions. The two passes and the `apply:` boundary are unchanged.

**The merge can shadow silently**, and that is its one real cost: `vars.redis_version` (a
service var) and `vars.acl_path` (a task local) now live in one map, so a task var can quietly
take over a service var's name. Covered by a soul-lint WARNING,
`vars_shadows_service_var` — modelled on the existing `vars_collision`
([`destiny/vars.md`](../destiny/vars.md)), which already reports exactly this shape for the
file↔task pair.

**A smaller profit than it first looks.** The shipped services relay service values through
`compute:` before using them:

```yaml
compute: { conf_dir: "${ default(essence.conf_dir, '/etc/dragonfly') }" }
```

dragonfly does this in four scenarios, mongo in its covenant. After the merge a task reads
`vars.conf_dir` directly and the hop is unnecessary **where its only job was to cross the
namespace**. It is NOT unnecessary everywhere, and the difference matters: every consumer of
`compute.conf_dir` in `examples/` is an `apply: input:` value, and that hop is the destiny
isolation boundary [§6](#6-the-asymmetry-across-the-apply-boundary-is-stated-not-removed) keeps.
`compute:` also marks a value host-invariant, which is a statement worth keeping on its own. So
this removes a namespace crossing, not a layer of indirection — NIM-415 decides case by case
rather than deleting every `compute:` it finds.

#### 2. A fleet overrides a service's defaults by forking the service repo

This is the section that exists so the override does not grow back.

A service pins to a git ref (`ServiceRef`, [ADR-007](0007-versioning-git-ref.md)). A fleet that
needs different defaults — an internal apt mirror, its own exporter versions, its own
`conf_dir` — **forks the service repository, edits `vars/`, and registers its fork's ref**.
This is the ansible-role model, and it needs no mechanism that does not already exist.

The alternative, an override field on the incarnation, is what is being removed, and the
shipped redis example shows why it never worked. Its `essence/_default.yaml` carries eighteen
comments of the form *"the operator overrides this in `spec.essence`"*; several cover a
neighbouring key as well (`conf_dir`'s also covers `data_dir`, the provisioning one covers the
whole `provision_*` set), so the affected keys number in the mid-twenties. They fall into two
groups.

**Fleet facts** — `install_method`, `install_package`, `binary_base_url`,
`binary_allow_private`, `modules_base_url`, `modules_allow_private`, `provision_provider`,
`node_exporter_version`, `redis_exporter_version`, `redis_exporter_sha256`, `vector_version`,
`vector_sha256`, `vector_sink_type` — identical across every redis incarnation of a given
fleet, yet the only place to write them was one instance's row. The comments say so
themselves: *"a **fleet** on an internal mirror sets true in spec.essence"*.

**Per-incarnation values** — `conf_dir`, `sentinel_master_name`, `vector_log_sources`,
`sentinel_master_defaults` — deliberately kept out of the Run form. `covenant.yml` records
the reasoning: *"NOT operator input … the override is in the incarnation's `spec.essence`,
not the Run form … are NOT persisted in state … [the advanced scenario] `add_node` reads
`essence.conf_dir`/`data_dir`."*

Not one of them moves. They stay in `vars/00-base.yaml`; only the way to override them
changes.

That second group is also why this decision keeps a different boundary intact. Those keys are
desired constants that must survive between runs, and `incarnation.state` is closed to them:
**state is a projection of what IS, and these describe what SHOULD BE.** Any design that
relocated them into `input:` would have parked desired values in `state` through the advanced-scenario
form-prefill path, and the `state = projection of actual` line would have blurred to buy an
override nobody could point at a user of.

**Not a dead end.** If a fleet ends up forking three service repos to change one mirror URL,
that is the signal the fleet group wants `keeper_settings`
([ADR-0073](0073-keeper-runtime-config-pg.md)) — added later, on top, changing nothing here.
Building the mechanism first and then discovering a fork would have done is the expensive
order.

#### 3. The `os/` and `coven/` layers are deleted; `vars/_stack.yaml` becomes real

Conditional assembly is expressed by **one** mechanism, not two. `_stack.yaml` is implemented
as documented (`file:` / `inline:` / `when:` / `optional:` / `foreach:` + `as:`), and the two
hard-wired directory conventions it subsumes are removed.

Without a `_stack.yaml`, the order is **every `*.yaml`/`*.yml` directly inside `vars/`, sorted
lexically**, `_stack.yaml` itself excluded. Subdirectories are **not** walked in this mode —
a nested directory is only reachable through an explicit step, and one left unreferenced
raises the soul-lint diagnostic `vars_dir_nested` rather than vanishing quietly.

**The capability the `coven/` layer provided survives; only its expression moves.**
The axis is **`incarnation.covens` — the labels of the incarnation itself**, selecting which
layers of its OWN config get stacked. It never read a host label, and the live guard for it
(`TestIntegration_IncarnationCovenSelectsServiceVarsOverlay`, NIM-248) proves exactly that.
This is why [NIM-281](0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited)
— which removed label inheritance from the system entirely — leaves the mechanism untouched:
what it deleted was the read-time union that put an incarnation's labels onto its member hosts,
and this overlay was never on that path. The guard is **retargeted onto a `foreach:` step, not
deleted** — what it protects is still true, and a `_stack.yaml` that fans out over the
incarnation's own labels is the way to write it:

```yaml
# vars/_stack.yaml
stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
    optional: true
  - inline:
      maxmemory: "${ vars.memory_mb * 3 / 4 }mb"
    when: "!has(vars.maxmemory)"
```

> ⚠ **Superseded by the [amendment 2026-10-15](#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late),
> point 14 (NIM-1014).** `inline:` is removed and a step's selectors no longer see `vars.*`: a step
> chooses files, never values. This example and the "`vars.*` accumulated so far" of the step context
> below describe the design before it; the normative grammar is
> [`architecture.md` → Service vars](../architecture.md#service-vars-the-assembly-pipeline).

**Step context — `incarnation.*` (name / service / service_version / covens / traits),
`vars.*` accumulated so far, and the `foreach:` binding. Nothing else.** Two roots the
documented draft offered are deliberately absent:

- **`host`** — on the coven axis it duplicated the incarnation's labels, and on the keeper
  axis it lied: a keeper-context resolve (provision-from-zero) has no host at all, and the
  old code quietly substituted the *incarnation's* declared tags for a host's. An author who
  wants the incarnation's labels now writes `incarnation.covens` and gets what the name says.
- **`soulprint.self`** — and this one is the load-bearing refusal. **A service's vars are
  resolved ONCE per run and handed to every host.** A step keyed on one host's facts would
  therefore apply that host's answer to the whole roster — silently, and worst exactly where
  it matters: a mixed debian/rhel roster would get one family's overlay everywhere. That is
  the failure this ADR exists to stop repeating, so the layer is host-invariant *by
  construction* rather than by convention.

**What an author uses instead, and what they must not reach for.** Host-dependent behaviour
belongs where the render is already per-host: `where:` on a task (targeting, the answer to
"do this only on cache hosts"), a task's own `vars:`/`params:` reading `soulprint.self.*`, and
`core.file.rendered` templates, whose `render_context` is built per host. **And, since
[NIM-908](0009-scenario-dsl.md#amendment-2026-09-28-nim-908-apply-input-renders-per-host),
`apply: input:` as well** — it renders per host now. Until 2026-09-28 it resolved on
`targeted[0]` (`destiny.go`), so a destiny received ONE set of values for its whole roster and a
`soulprint.self.*` in it silently meant the first host by SID; that was true when this ADR was
written and is the sentence NIM-908 retired. The paragraph below is unaffected either way: what
it rests on is that a SERVICE's `vars/` layer resolves once per run, which NIM-908 did not
touch: a `soulprint.self` in a `vars/_stack.yaml` step is still refused outright, and NIM-908's
classification deliberately does not walk this layer at all. It DOES walk the applier's own
task-level `vars:`, which is a different layer and can be per-host — a `${ vars.addr }` in
`apply: input:` over `vars: {addr: "${ soulprint.self… }"}` is classified per-host, and a
service var forwarded the same way is not.

> ⚠ **Superseded in part by the [amendment
> 2026-10-15](#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late),
> points 6–8 and 16 (NIM-1014).** The service VIEW is still host-free — built once per run and
> reused by every Passage, and once per pre-flight gate and claim render — and a service
> definition's text still cannot name `soulprint.self`. But inside a task's view a service definition
> reads that task's overrides, so it can become per-host through a per-host override — and NIM-908's
> classification therefore walks the service definitions too. "Deliberately does not walk this layer"
> and "a service var forwarded the same way is not" no longer hold.

**Per-host service vars were considered and deferred, with a measured reason.** Making the
layer resolve per host is mechanically straightforward — the plumbing for a per-host map
already exists (`RegisterByHost`, `DestinyVarsResolved`). What stops it is reach: the two
host-invariant boundaries above would not move with it. `apply: input:` carries **136**
`vars.` reads across the thirteen shipped services, and `compute:` — whose own contract says
host-invariance is what makes it safe to feed `apply.input` and a state capture's `params:` at once —
carries five more. A per-host layer that stopped at those boundaries would be per-host in
tasks and silently first-host everywhere else: the same class of quiet wrong answer this ADR
removes, and worse than not having the feature, because it would look like it worked.

Doing it honestly means REFUSING `vars.*` in `compute:` and in `apply: input:` — a compile
error rather than a silent first-host answer — and rewriting those 141 sites to pass the
value explicitly. That is a train of its own, not a footnote to this one, and nothing shipped
asks for it yet: no example carried a `coven/` overlay at all. Selecting on a HOST's covens is
`where: "'cache' in soulprint.self.covens"`, which is per-host by construction and already
works. Selecting on the INCARNATION's covens is the `foreach:` step above.

Adopting a documented-but-unbuilt root would have been the same mistake as the phantom
`scenario/<name>/vars.yml` that NIM-416 deletes from the docs: prose is not a contract until
something reads it.

**Consequence worth stating: the `os/<family>.yaml` overlay has no replacement.** It was used
by zero shipped services, and under the old resolver it was already answering with
`hosts[0]`'s family for every host in the run — a wrong answer nobody had hit yet. A service
that genuinely needs per-OS behaviour expresses it where the render is per-host: a `when:` on
a task, or `soulprint.self.os.family` inside `apply: input:`.

**And the incarnation must carry its covens on every path that resolves.** `SelectByName`
always read the column; the runner's own `FOR UPDATE` read did not, and the telemetry query
did not. Both now do — otherwise the same `_stack.yaml` would resolve one way for a run and
another for the pre-flight gate or a telemetry push, which is precisely the divergence class
being removed.

#### 4. `strategy: deep|replace` per step

Merging between files is a deep merge — maps recurse, scalars and lists are replaced whole.
That is the right default and the wrong only option. `examples/service/redis` documents
`install_package` as *"it is the whole map that an override replaces, not individual keys"*,
while `mergeInto` recurses: overriding `repo_uri` alone leaves the base's `gpg_key_url`
attached to it — a mirror URL from one place and a signing key from another. The comment
describes an intent the mechanism cannot express, and `pipeline_test.go` pins the recursive
behaviour, so it is not a bug to fix but a missing knob.

`strategy:` on a step selects it. `deep` (default) preserves today's behaviour bit for bit.

#### 5. `00-base.yaml`, not `_default.yaml`

Numbered like `/etc/*.d/`. The rename is forced by [§3](#3-the-os-and-coven-layers-are-deleted-vars_stackyaml-becomes-real),
not by a defect in the old scheme: once the directory listing IS the order, the base layer's
primacy is carried by its name, and an underscore only sorts first while every neighbour is
lower-case. A number sorts first regardless, and reads as first without knowing where `_` falls in
ASCII.

The regression guard is a test with `Base.yaml`, `base.yaml` and `00-base.yaml` in one directory —
renaming the file proves nothing on its own.

#### 6. The asymmetry across the `apply:` boundary is stated, not removed

Inside a scenario, `vars` means service vars plus scenario and task locals. Inside a destiny,
`vars` means that destiny's own `vars.yml` plus its task locals, and nothing else — destiny
isolation is untouched, and a destiny still receives from its caller only what crossed in
`apply: input:`.

The same word therefore names different sets on the two sides of one keyword. This asymmetry
**already exists** and has already been paid for: `apply_when_dynamic_unsupported`
([`docs/scenario/orchestration.md §2.1.2`](../scenario/orchestration.md)) refuses a dynamic
`when:` on an applier precisely because *"a destiny task's flow context is built in the
isolated destiny env, where `input.`/`vars.`/`essence.` name different things than in the
scenario the predicate was written in."* Unifying the names removes the camouflage, not the
boundary. It is documented head-on in [`docs/destiny/vars.md`](../destiny/vars.md) rather than
left to be rediscovered.

#### 7. Two context maps that cross the wire are renamed with it

The rename is not confined to the Keeper. Two `google.protobuf.Struct` payloads carry the
namespace to Soul, and both lose the `essence` key:

- **`RenderedTask.flow_context`** ([ADR-012](0012-keeper-soul-grpc.md), `apply.proto`) — the
  per-host snapshot `{input, vars, essence, incarnation, self}` that Soul's flow-control
  engine activates for `when:`/`changed_when:`/`failed_when:`. Becomes
  `{input, vars, incarnation, self}`, with the service layer merged into `vars`.
- **`core.file.rendered`'s `render_context`** ([ADR-010](0010-templating.md),
  [`templating.md` §3.2](../templating.md)) — the text/template root
  `{vars, self, role, essence}` plus the CONDITIONAL `input` of the 2026-06-26 amendment.
  Becomes `{vars, self, role}` + the same conditional `input`.

No proto field number moves and no field is deleted, so [ADR-012(c)](0012-keeper-soul-grpc.md)
only-add is not violated; what changes is the key set inside a Struct. It is nonetheless a
**breaking Keeper↔Soul change within the release**: a Soul from before this train reads
`flow_context["essence"]` and finds nothing. Acceptable only because the whole train breaks
the public contract anyway (`spec` and `essence` leave `GET /v1/incarnations/{id}`) and
both sides ship together; it is recorded here so nobody discovers it from a field report.

Cheap in practice on the template side: **no `.tmpl` in `examples/` reads `.essence`** — the
key was carried into every render context and used by none.

#### 8. `Essence` retires from the dictionary

[`docs/naming-rules.md`](../naming-rules.md) lists **Essence** among the primary terms. It is
retired, with the dictionary entry rewritten to point here rather than deleted — a term used by
twenty-three other ADRs (two more use "in essence" as ordinary English) and by the shipped examples
throughout needs a forwarding address. The name is dropped outright only from the list of
metaphor EXAMPLES, where a retired term would be actively misleading. Nothing is renamed
*to* Essence; nothing new adopts it.

### Rejected

- **A replacement column, `incarnation.essence_override`.** Proposed while NIM-408 was being
  scoped. It preserves the shape whose only implementation was a single test fixture, and puts a
  second copy of a fleet fact on an instance row — the defect `spec` is being dropped for.
- **`keeper_settings` ([ADR-0073](0073-keeper-runtime-config-pg.md)) for the fleet
  group.** The better long-term home, and still available later, but it needs machinery that
  does not exist: a `settings.*` CEL root and a rule binding a service's vars to keeper
  settings. Adding an entity is the opposite of what this ADR is for. See
  [§2](#2-a-fleet-overrides-a-services-defaults-by-forking-the-service-repo) for the signal
  that would justify it.
- **Moving the per-incarnation keys into `input:` behind a collapsed `form:` section.**
  Mechanically workable (`form_layout.go` has `Collapsed`/`ShowWhen`, advanced-scenario forms prefill from
  state) and rejected for the boundary it costs: desired constants would land in `state`,
  which is a projection of actual.
- **Keeping the name `Essence`.** The name was justified by a property that is being removed.
  Keeping it would leave two namespaces distinguished by nothing.
- **Keeping `_default.yaml`.** Under the directory-as-order scheme its precedence would be
  conditional on its neighbours' letter case, and a service author has no reason to know where `_`
  falls in ASCII. (Under the OLD scheme the name carried nothing — the resolver read it by a
  constant — so this is a cost the rename avoids, not a bug it fixes.)
- **Keeping `os/`/`coven/` alongside `_stack.yaml`.** Two mechanisms for one job, one of them
  unused by every shipped example, and the implicit one silently overruling the explicit one
  in any service that has both.
- **A `host` root in the `_stack.yaml` step context.** On the coven axis it duplicates the
  incarnation's own labels; on the keeper axis it misreports a context that has no host at all. See
  [§3](#3-the-os-and-coven-layers-are-deleted-vars_stackyaml-becomes-real), which also refuses
  `soulprint.self` for a separate and stronger reason.

### What this ADR does not carry

The normative prose still describes the deleted design, and deliberately so: it is
rewritten by **NIM-416**, together with the soul-lint diagnostics this ADR names
(`vars_dir_nested`, `vars_shadows_service_var`, `stack_step_invalid`). Until then
[`docs/architecture.md`](../architecture.md) §"Essence: assembly pipeline",
[`docs/templating.md`](../templating.md) §3.2, [`docs/service/manifest.md`](../service/manifest.md),
[`docs/destiny/vars.md`](../destiny/vars.md), [`docs/guides/first-service.md`](../guides/first-service.md)
and [`docs/README.md`](../README.md) still document `essence/`. `make check-doc-links`
does not catch this — every anchor still resolves — so the gate is the ticket, not
the tooling. The service repositories under `examples/` and `dev/` are **NIM-415**.

### Consequences

- **Breaking, for the public API and for every service repository.** `spec` and `essence`
  leave `GET /v1/incarnations/{id}`; every service repo renames a directory and a file and
  rewrites its `essence.<key>` references. In this repo that is 642 references across `examples/`
  (624 of them under the thirteen `examples/service/` trees, the rest under `examples/destiny/`),
  plus three `dev/upgrade-demo/tree/v*/essence/` fixtures; the external destiny repos are
  unaffected (they carry no essence).
- **`incarnation.spec` can be dropped.** With `spec.essence` gone, `spec.input`/`spec.traits`
  moved by NIM-408, and `spec.hosts` removed by NIM-330, the column has no remaining key.
  A migration clears the `essence` key ahead of the drop, on the model of
  [`108_drop_incarnation_spec_hosts.up.sql`](../../keeper/migrations/108_drop_incarnation_spec_hosts.up.sql).
- **The migration-CEL sandbox list shortens.** [ADR-019](0019-state-migration-dsl.md) forbids
  `vault()`/`now()`/`register.*`/`soulprint.*`/`essence.*`/`input.*`; `essence.*` simply stops
  existing. Migration mode declares only `state`, so nothing changes in behaviour — only the
  prose.
- **No trial case loses its subject, and none of them tested `spec.essence`.** Worth stating
  because the epic assumed otherwise: a trial fixture's `essence:` block is injected into the
  ALREADY-RESOLVED map (`harness.go` → `RenderInput`), never through `IncarnationSpec`, so the
  override path has no test coverage at all — consistent with it having no writer. What
  `conf-data-dir-override`, `conf-data-dir-override-tls` and `tls-essence-refs` actually assert is
  that `conf_dir`/`data_dir` reach the destiny tasks' paths, which the service-vars layer carries
  verbatim. The fixture KEY is renamed (`essence:` → `vars:`) in 103 cases; nothing else moves.
- **Roughly six comments in `redis/covenant.yml` become false** the moment the override goes
  and must be rewritten in the same change, not after it.

### Amendment 2026-10-15 (NIM-1014): vars are evaluated on request and bound late

**Decided by the user on 2026-10-15.** It replaces how `vars` is EVALUATED, in every rung of both
passes; what the namespace contains, the two passes and the `apply:` boundary stay as §1 and §6 fix
them. It supersedes in part the var→var amendment of 2026-06-24 to [ADR-009](0009-scenario-dsl.md)
and [ADR-010](0010-templating.md) — the eager-topological resolve, the eager `var_unknown_ref`, the
refusal of `vars['k']` and the file↔task isolation — and the sentences of §1 and §3 that the forward
notes above name. Implemented by NIM-1014: these documents first, the code in the same ticket before
it merges.

#### What evaluation was at `2cc27029`

Three evaluators, none of them lazy, and one rung nothing reads (`release/NIM-926` at `2cc27029`):

- **A service's `vars/` files are data.** `readLayer` (`keeper/internal/servicevars/pipeline.go:175`)
  unmarshals a file and nothing evaluates what it read, so `${ … }` in a `NN-*.yaml` is a literal.
  The rung's only expressions are `_stack.yaml` `inline:` steps (`renderInline`, `stack.go:180`),
  evaluated eagerly in step order over the vars accumulated so far (`resolveStack`, `stack.go:66`),
  top-level strings only. The rung resolves once per run (`keeper/internal/scenario/run.go:346`), and
  separately for the pre-flight gate (`preflight.go:273`), in `render_host.go:132` and for telemetry
  (`keeper/internal/grpc/events_telemetry.go:133`); the directive catalog assembles the files alone
  (`ResolveLexical`, `keeper/internal/artifact/directives.go:54`).
- **A destiny's `vars.yml` is eager-topological.** `resolveVarLayer` / `topoSort`
  (`keeper/internal/render/varlayer.go:65`, `:151`) build the dependency graph, raise `ErrVarCycle` and
  an eager `ErrVarUnknownRef`, and evaluate EVERY name on EVERY targeted host (`resolveDestinyVars`,
  `render/destiny.go:479`). A `${ … }` nested in a map or list value is skipped (`varlayer.go:74-77`)
  and stays literal.
- **Task `vars:`**, a `block:`'s merged in by `mergeBlockInheritance` (`render/block.go:534`), are ALL
  evaluated per task per host through `resolveTaskVars` (`render/cel_render.go:111`), at seven call
  sites: `renderTaskIter`, `staticWhenSkips`, `evalIncludeWhen`, `evalAssertTask` and
  `renderKeeperTask` (`render/pipeline.go:684`, `:889`, `:944`, `:1246`, `:1344`), `resolveTargets`
  (`render/dispatch.go:42`) and `resolveApplyInput` (`render/destiny.go:584`). A task var sees the
  service layer below it and NOT the destiny `vars.yml`, which is stacked in after the resolve
  (`cel_render.go:119-133`). `vars['k']` inside a var definition is refused (`ErrVarIndexForm`,
  `shared/cel/varrefs.go:19`); in a `params:` cell it is already legal.
- **The scenario's top-level `vars:`** (`ScenarioManifest.Vars`, `shared/config/scenario.go:33`) is
  parsed and read by nothing in render, although ADR-010's 2026-08-03 amendment
  (`0010-templating.md:56`) and §6 of this ADR describe it as a rung and soul-lint's
  `vars_shadows_service_var` reports it as one.

Two facts follow and shape the design. soul-lint computes no vars at all — there is no offline cycle
or unknown-reference check, so the eager runtime was the only thing that caught either. And an L0
trial never reads `vars/`: its `fixtures.vars` IS the service rung
(`keeper/internal/trial/harness.go:273`).

What that costs an author: a service file cannot derive one value from another without an `inline:`
step; a `${ … }` one indent level down is silently literal in `vars.yml` and task `vars:`; a typo in
a var nothing reads fails the run, and every var is evaluated on every host whether read or not; and a
task override never reaches a value derived from the name it overrides, because the derivation was
already computed in a rung that cannot see the task.

#### 1. A definition may carry `${ … }` anywhere, in any rung

Every value in every rung may contain `${ … }` at any depth — a string inside a map or a list is
interpolated like a top-level one. The cell rule of [ADR-010](0010-templating.md) is unchanged: a
cell that is one whole `${ … }` takes the native type of its result, anything else is the stringified
concatenation. A non-string value is a literal.

#### 2. The rungs of a pass

```
scenario pass:  <service>/vars/ (one rung)  →  scenario vars:  →  block + task vars:
destiny pass:   <destiny>/vars.yml          →  block + task vars:
                ─── apply: ──────────────────────────────────────────────────────
                nothing crosses except what the caller writes in `apply: input:`
```

- All the files of a service's `vars/` are ONE rung, ordered inside it by file name or by
  `_stack.yaml` step.
- The scenario's top-level `vars:` becomes a real rung — the user's choice: the docs described it
  and nothing read it. Its definitions see run data only (point 6), the user's second choice.
- Block and task stay one rung (`mergeBlockInheritance`, as today).
- The `apply:` boundary is untouched: a destiny pass sees none of its caller's names.

#### 3. Merge raw, evaluate later

The service rung's files deep-merge exactly as today — `_strategy` in a file, `strategy:` on a step —
but over UNEVALUATED definitions, and each cell remembers the file or step that contributed it.
Across rungs a higher rung replaces a name at the top level, as today.

Two consequences of merging before evaluating:

- A definition that is one whole `${ … }` is a SCALAR to the merge, whatever its result will be. A
  later file's map replaces it whole, and it replaces an earlier map whole — an expression's result
  is never deep-merged with a file's map.
- A cell inside `N`'s value that reads `vars.N.<k>` is a self-reference (point 9): it reads `N` as
  the layers below its own define it, so it cannot see a sibling key its own file or a later one
  contributes.

#### 4. A var is evaluated when it is read, once

`vars` in a CEL activation is a lazy map (cel-go `traits.Mapper`). `vars.x`, `vars['x']`,
`vars[expr]`, `has(vars.x)` and `default(vars.x, …)` all reach one lookup: cel-go v0.29.2 qualifies a
`traits.Mapper` through `Find` for select, index and presence alike (`interpreter/attributes.go:1375`,
`refQualify`), and `Find` is where a definition is evaluated. So `has()` and `default()` EVALUATE `x`
and propagate its error rather than hiding it — `default()` is the macro `has(x) ? x : y`
([templating.md §2.3](../templating.md#23-registered-cel-functions-starting-minimum)). Key-only
operations — `size(vars)`, `'x' in vars` — evaluate nothing.

- The unit of request is the top-level name: `vars.install_package.repo_uri` evaluates all of
  `install_package`.
- A name nobody reads is never evaluated, a broken one included.
- `ErrVarIndexForm` is retired: a lazy map needs no name known in advance.
- No dependency graph is built at run time, and nothing is pre-parsed for evaluation.

#### 5. Late binding: one view per task

Within a pass a task has ONE view of `vars`: the union of its rungs, the highest rung winning a name,
and EVERY definition — whatever rung it came from — reads `vars.<other>` through that same view. This
is the Ansible model, and the user chose it.

```yaml
# <service>/vars/20-host-layout.yaml
conf_dir: /etc/redis
acl_file: "${ vars.conf_dir }/users.acl"
```

```yaml
# scenario/<name>/main.yml
tasks:
  - name: write the ACL file
    vars:
      conf_dir: /opt/redis
    module: core.file.present
    params:
      path: "${ vars.acl_file }"     # /opt/redis/users.acl
      content: "..."
```

The order of declaration never matters. The file↔task isolation of the 2026-06-24 amendment is
REMOVED in both directions: a task var may read a `vars.yml` name, and a `vars.yml` definition sees
the task's override.

**The exceptions: task cells that skip the task rung.** Five contexts belong to a task and still do
not read its block + task rung. Each keeps its reason; the table in point 7 states the whole map.

- **The loop axis** — `loop.items:` and `loop.when:` read the service rung and the scenario `vars:`
  rung. A task-rung definition is evaluated per iteration and may read the loop binding, so the set
  being iterated cannot depend on it. At `2cc27029` `loopInvariantVars`
  (`keeper/internal/render/loop.go:246-268`) seeded the service layer alone
  (`Vars: in.ServiceVars`, `:253`); the amendment adds the one rung above it that is not per
  iteration (`loopAxisView`, `loop.go:326-328`).
- **The `template:` path of `core.file.rendered`** — it resolves in `keeperVars`
  (`pipeline.go:1880-1896`), before any host, to choose the one file Keeper reads, and reads the
  service and scenario rungs.
- **An include `when:`** — it decides whether a group of tasks exists at all, so it cannot read a
  task's own `vars:` (`pipeline.go:1048-1060` builds its view without one). It reads the service and
  scenario rungs, and in a destiny pass the first host's `vars.yml`.
- **An `on:` coven list** — it chooses the roster and reads no `vars.*` at all, as at `2cc27029`
  (`resolveCovenList`, `dispatch.go:263-281`).
- **A `core.state` capture's merge-time `match:` / `patch:`** — evaluated per element at merge, with
  no run context, and reads no `vars.*` at all, as at `2cc27029` (`StateOpEvaluators`,
  `pipeline.go:2025-2037`: `Loop` and the scopes only).

#### 6. Each definition keeps its own rung's environment

The view decides what `vars` means. Which OTHER roots a definition's text may name is decided by the
rung it was written in:

| Rung | Environment | Roots besides `vars` |
|---|---|---|
| service `vars/` | `cel.NewServiceVars` | `incarnation.*` — the row's own fields, covens and traits included — and nothing else |
| scenario `vars:` | run data only — a closed list | roots `input.*`, `incarnation.*`, `compute.*`; the task env's functions except `vault()` and `generate_secret()`. Refused at compile: all of `soulprint` (`.self`, `.hosts`, `.where`), all of `register` (`register.hosts` included), `vault()`, `generate_secret()` |
| block + task | the env of the context that reads it | as today, its host scope included |
| destiny `vars.yml` | the destiny env | `input.*` (the destiny's own), `soulprint.self.*`, `incarnation.*` |

So §3's invariant stands: a service definition's TEXT can never name a per-host or operator root. It
can still become per-host INSIDE a task's view, but only by reading a name that task overrides per
host; the service view below never does.

**The scenario rung sees run data only — the user's choice of 2026-10-19.** A value that depends on
the host, on a probe or on a Vault read is written in the task's own `vars:` (one indirection,
`compute.*`, is unfollowed by the Passage cut today, for task text too — point 16). The reason is
what classifies a task: the Passage cut (`config.Stratify`, `shared/config/passage.go:138`, with
its register, Vault and
roster axes), `soul_needs` (`render/soul_needs.go`) and the first-host decisions (an include `when:`,
a static `when:`, an `assert:`) are all built around a task's own text and the axes it names. A
scenario-wide `register.probe`, `vault()` or `soulprint.self` would add an axis to every task reading
the name. With run data only, the rung names none of those roots, so it adds no axis of its own.

What any rung can still carry is a dependency through a name: a scenario definition over
`incarnation.state`, or a service or scenario definition derived from a task override. Point 16
covers it per analysis:
- the seal, the host-variance classification and `soul_needs` follow the name;
- the Passage cut needs no following;
- the static-predicate test is NIM-1020.

**What values a definition is evaluated with.** The rung decides the roots; the values come from:

- **a service definition** — its own rung's `incarnation` map (`servicevars.IncarnationContext`,
  `shared/servicevars/servicevars.go:66-119`: id, service, service_version, covens, traits; no
  `state`, no `host_count`). Never the reader's, never a loop binding.
- **a `vars.yml` definition** — the destiny pass's values for the host of the view. Never a loop
  binding.
- **a scenario-rung definition** — the reading task's run-level values: its `input`, its
  `incarnation` map and `compute`. No loop binding. The `incarnation` map is the reader's own, so it
  is not the same for every reader. A host task gets `incarnationVars` (`cel_render.go:122-133`), with
  `host_count` = its targeted hosts. A keeper task gets `keeperVars` (`dispatch.go:210-219`), with
  `host_count: 0`. Both carry `state`. The compute scope travels with the reader, as the host
  scope does: on the loop axis, where `compute` is out of scope (NIM-619,
  `ComputeOutOfScopeLoopAxis`, `shared/cel/scope.go:79-81`), a scenario definition that reads
  `compute.*` is refused, so `loop.items: "${ vars.s }"` over `s: "${ compute.x }"` fails rather than
  reopening what NIM-619 closed.
- **a block + task definition** — the context of the cell that reads it, loop binding and host scope
  included. A keeper task (`HostFreeKeeper`) is the only host-free context that reads the task
  rung: a `soulprint.self` in it is refused by name at compile there (`cel.Vars.HostScope`,
  `shared/cel/host_scope.go`), as it was at `2cc27029`.

#### 7. What each context reads

The service view is the service rung ALONE. A run builds it once, at run start (`run.go:363`), and it
lives for the whole run, across Passages; the run's `compute:` reads it and nothing else, and every
task view of the run stacks on it. `compute:` does NOT see the scenario `vars:` rung: that rung reads
`compute.*`, so the reverse would be a cycle between namespaces.

Four other places build a service view of their own:
- the pre-flight gate (`preflight.go:276`), whose asserts then read task views — see the table;
- the claim render (`RenderForHost`, `render_host.go:133`), which reproduces the run's render on
  each claim (ADR-027) and reads task views exactly as the run does;
- telemetry (`grpc/events_telemetry.go:134`), outside any run and host-free, which reads only that
  view;
- the directive catalog (`artifact/directives.go:58`), likewise. It has no incarnation: it keeps
  assembling the files alone, `_stack.yaml` ignored (`LexicalView` over
  `servicevars.LexicalLayers`, as `ResolveLexical` did), and evaluates only the key it reads
  against an empty incarnation.

Every other context reads a view of its own:

| Context | Rungs of `vars.*` it reads | Host stance |
|---|---|---|
| a host task in the scenario pass — `params:`, `where:`, its `vars:`, `apply: input:`, `flow_context`, `render_context` values | service + scenario + block/task | host-bound, per host |
| a static `when:`, an `assert:` and an `assert:` task's own `when:` gate (at render, and in the pre-flight gate through `EvalAsserts`, `preflight.go:212`) | the task's view, as above | decided on the roster's first host, as at `2cc27029` — see the note below the table (NIM-1020); a keeper task's static `when:` in `keeperVars` instead, where its params are read (`pipeline.go:971-989`) |
| an `on: keeper` task | service + scenario + its block/task | `HostFreeKeeper` |
| an include `when:` | service + scenario; in a destiny pass the first host's `vars.yml`, guarded by `HostVariantVars` (`evalIncludeWhen`, `pipeline.go:1030-1070`) | `HostFreeIncludeWhen` |
| `loop.items:` / `loop.when:` | service + scenario (point 5); none in a destiny pass, as at `2cc27029` (`loopAxisView`, `loop.go:326-328`: `runLevelView` is nil there) | `HostFreeLoopAxis` |
| the `template:` path of `core.file.rendered` | service + scenario (`keeperVars`, `pipeline.go:1880-1896`); none in a destiny pass | `HostFreeTemplatePath` |
| `compute:` | service (`compute.go:55`) | host-free |
| an `on:` coven list | no `vars.*`, as at `2cc27029` (`dispatch.go:263-281`) | `HostFreeCovenList` |
| a `core.state` capture's merge-time `match:` / `patch:` | no `vars.*`, as at `2cc27029` (`pipeline.go:2025-2037`) | `HostFreeStateMatch` |
| a host task in the destiny pass | `vars.yml` + block/task | host-bound, per host |

Two contexts in that row change path. The first is an `assert:` task's own `when:` gate:
- **At `2cc27029`** it was evaluated over `hostVars` alone (`evalAssertTask`,
  `pipeline.go:1216-1234`): service vars, no task `vars:`, and no `GuardHostFreePredicate`.
- **Now** it reads the task's view on the roster's first host (`pipeline.go:1342-1346`), behind the
  guard `staticWhenSkips` applies (`:1355`; `staticWhenSkips`' own, `:961`). So it can read its own
  task's `vars:`.

The second is a keeper task's static `when:`. At `2cc27029` `staticWhenSkips` decided it on the
roster's first host, over `hostVars`. It is now read in `keeperVars`, where the task's params are:
no host bound, `host_count` 0 (`pipeline.go:971-989`;
`TestStaticWhen_AKeeperTaskIsDecidedInTheKeeperContext`).

`evalAssertTask` is called from the scenario render loop and from `EvalAsserts` alone
(`pipeline.go:267`, `:1306`), both scenario-pass paths. There the guard is inert, as for every
scenario-pass static `when:` (NIM-1020).

A static `when:` and an `assert:` task's `when:` gate are classified static from their own text and
decided on the first host (`config.IsStaticPredicate`; `staticWhenSkips`, `pipeline.go:921-1003`, and
the assert gate, `:1351-1371`). An `assert:`'s `that:` is always evaluated on the first host — a
synthetic empty host on an empty roster (`pipeline.go:1342-1381`). A task-rung
`vars:` that reads `soulprint.self` or `register` behind `vars.<x>` is therefore decided for the whole
roster by one host. That defect is older than this amendment and is filed as **NIM-1020**. In the
scenario pass no rung adds a host root to it: the scenario rung sees run data only, and the service
rung's text names none. The amendment does open two more paths to it:

- under late binding, a service or scenario definition (in a destiny pass, a `vars.yml` one) derived
  from a host-dependent task override;
- an `assert:` task's own `when:` gate, which can now read its task's `vars:`. At `2cc27029` a gate reading
  a host-dependent task var failed on a missing key (a `has()`-guarded read was simply false), or read
  the service value of the same name, instead.

In the scenario pass both paths stay with the rest of NIM-1020. In the destiny pass the defect itself
is closed here, as a side effect of point 16. The per-host set becomes per task view, and
`staticWhenSkips` already guards a destiny static `when:` with that set (`pipeline.go:961`). So a
destiny static `when:` over a host-dependent task var — or over a `vars.yml` name derived from one — is
REFUSED by name at render instead of decided by one host. The corpus has no such task (scan below).

#### 8. Memo scopes

A memoized value is keyed by (name, layer bound): the value of `N` as the layers up to that bound
define it. An ordinary read uses the view's top. A self-reference (point 9) uses the bound just below
the layer that contributed the reading cell. For a top-level name deep-merged from several files, the
value at a bound is the merge of the contributions up to it.

A memoized value records the `vars` names its evaluation read — found or absent, and transitively:
reading `vars.b` adds every name `b`'s own evaluation read. `'x' in vars` records `x`; `size(vars)`
and a whole read (point 13) record the whole namespace.

- A service-rung or `vars.yml` definition evaluated once — in a service view, or per
  destiny pass per host — is REUSED in any task view that defines none of the names it recorded. A service var no task override touches is computed once per service view — the
  run's (shared by all its Passages), the pre-flight gate's, and each claim's (point 7) — however
  many tasks × hosts read it. A value that recorded the whole namespace is reused only in a view
  whose upper rungs define nothing.
- Scenario-rung and task-rung definitions are memoized per task view only — per task, host and loop
  iteration.
- Concurrent duplicate evaluation is harmless: within one render an evaluation is deterministic, and
  the first write wins. `vault()` reads go through the run's resolution memo, and the only memo here
  that outlives a render is the service view's, whose environment has no `vault()`. A read dropped
  by `ForgetVaultReads` after a write (`shared/cel/vault.go:171-177`) therefore cannot leave a stale
  value behind in a memo of this design. An error is kept for one read only — a top-level `Lookup`,
  `Materialize` or `Check`, or one CEL evaluation over the lazy `vars`: within it a definition that
  failed returns its first error and is not evaluated again, and the next read evaluates it afresh.
  CEL evaluates both operands of `+`, so without that a read evaluated a failing definition once per
  path to it, exponentially in a cyclic group. A cycle error depends on the definitions in progress,
  and so does every failure that met one, whether CEL propagated it or absorbed it (`||`, `&&`, a
  comprehension). So within the read a failure is kept only while the definitions it may depend on
  are in progress or have failed too; when one of them returns a value instead, the failure is
  dropped, and the next path to it evaluates it again (`varRead.settle`,
  `shared/cel/varview.go:215-238`).

#### 9. Self-reference reads the definition below

`conf_dir: "${ vars.conf_dir }/wb"` in a later file, a later step or a higher rung reads `conf_dir` as
the layers STRICTLY BELOW the one contributing that cell define it. Nothing below → `var_cycle`
`conf_dir → conf_dir`, the rule the task rung had at `2cc27029` (`varlayer.go:86-96`). The lazy view
applies it to every rung: `lazyVars.bound` sends a self-read below its own layer
(`shared/cel/varview.go:643-652`), and `VarView.resolve` raises the cycle when nothing is there
(`:286-292`). Inside the service
rung "below" is file and step order, so the rule holds between two files of one rung exactly as it
does between rungs.

#### 10. Cycles are caught on the evaluation path

The evaluation path is the chain of (name, layer bound) pairs in progress — the key of point 8, a
layer being a rung or, inside the service rung, the file or step that contributed the definition. A
lookup of a pair already on the path is `var_cycle`. A cycle through `has()` or `default()` is a
cycle too: both evaluate their operand (point 4). The cycle is rendered by name and rotated to
start at its lexicographically
smallest name (at that name's lowest layer when it occurs twice), so a ring's text is identical
whichever of its vars was requested first.

Determinism in general: what a request yields, a name's value or its failure, is the same for any
order of requests, and inside a read a kept failure (point 8) is returned only where evaluating its
definition again would fail too (`TestVarView_OutcomesDoNotDependOnThePathOrTheOrder`; against the
memo off, `TestVarView_TheFailureMemoChangesNoOutcome`). Both rest on CEL turning an error into a
value only where any value gives the same result, and the one place it does otherwise, an equality
that meets the lazy `vars` (`[vars].exists(m, m != {…})` turned a cycle into `true`), is a whole read
wherever the root can reach it: itself, an element of a literal list or map, an index or a select into
one, a list concatenation, a comprehension's variable or result, and the result of any call but one
whose result cannot hold an argument's value (`size()`, `type()`, a scalar conversion, a comparison),
so `merge({'x': vars}).x` too (`comparesVars`, `TestVarView_AComparedCopyOfVarsIsTheSameInEveryOrder`).
The error text and kind are not held to that: a kept failure carries the error of the path that first
met it, which can be `var_cycle` where the memo off meets a type-conversion error, and a ring names
the definitions that path went through. So where more than one error could end a read, which one it
reports can depend on the path inside the read and, through the values earlier reads memoized, on the
order of requests. The Keeper env has no time source — `now()` is refused everywhere (`shared/cel/functions.go:93`) — and
`generate_secret()` yields a request marker, not a value
([ADR-0083](0083-declared-secret-state-fields.md)
§3), so when it is evaluated cannot change what it yields.

#### 11. Errors at run time

A reference to a name no rung of the view defines is the ordinary CEL no-such-key, raised only when
the referencing var is read, and wrapped with the name of the var being evaluated. `var_cycle` keeps
its stable marker, so `examples/service/pilot-destiny/scenario/vars-cycle/tests/cycle-aborts/case.yml`
still passes: its task reads `vars.a`. A broken var nobody reads no longer fails a run — point 12 is
what catches it.

An error raised while a definition is evaluated is reported with THAT definition's text as the failing
expression and `vars.<name>` as where it failed — not the cell that asked for it. NIM-1000's
withholding (`render/failure.go`, `withhold`) then judges it by that definition's taint (point 15):
CEL quotes a missing key by its value, so a definition reading a secret must not have its error
quoted, however many hops away the asking cell is.

#### 12. The full check runs once per edit, not once per run

The user's decision. Static analysis is acceptable here because it runs once per edit, never per run.

- **(a) soul-lint, offline.** It compiles every definition against its rung's environment and walks
  the static reference graph of every view an artifact forms. `var_cycle` and `var_unknown_ref`
  become soul-lint ERROR codes, and so does `var_invalid` (the code name is pending the user's
  confirmation): a definition that does not compile against its rung's environment, or that fails
  when `validate-service` evaluates the service view on the stub incarnation below. A definition must
  resolve from its own rung and the rungs below it: one
  that needs a higher rung to supply a name is an error (the service view of point 7 would fail on
  it), while overriding a name is fine.

  A guarded read is not a need. A read of `vars.x` in an expression that also tests `has(vars.x)` or
  `'x' in vars` on that same root name is not `var_unknown_ref`. That covers the shape
  `default(vars.x, y)` expands to (`has(vars.x) ? vars.x : y`, `shared/cel/default.go`), the
  hand-written ternary, and `'x' in vars ? vars.x : y` — a key-only test evaluates nothing (point 4),
  so it guards exactly as `has()` does. It is the
  optional-override hook late binding makes useful: `tls: "${ default(vars.tls_override, false) }"`
  in a service file, answered by a task that sets `tls_override`. `has(vars.x.y)` and
  `default(vars.x.y, d)` guard `y`, not `x`: `x` is read strictly and stays an error. The test is
  textual — `!has(vars.x) ? vars.x : y` passes it too — so it is an approximation, and strict mode
  (b) is what evaluates the shapes it lets through.

  The walk parses with the `default()` macro expanded. This relaxes the eager runtime of `2cc27029`,
  which refused such a read inside a var definition: `VarRefs` parsed without macros
  (`shared/cel/varrefs.go:41-80` at `2cc27029`), so `default(vars.x, y)` was an edge like any other.
  An edge the text does not spell — `vars[expr]` with a non-literal key — is not in the static graph;
  (b) is what evaluates it.

  The walk covers every layer file any `_stack.yaml` step can include, whether or not a given
  incarnation would select it (`servicevars.EveryLayer`, `shared/servicevars/layers.go`). It mirrors
  the lazy view on the edges the text spells:
  - **Selections.** It walks a view in stacks the Keeper builds for some incarnation. A layer is
    conditional when its step has a `when:`, or a `foreach:` or `file:` that reads `incarnation.*`;
    an `optional:` step whose file exists, and a constant `foreach:` with a constant `file:`, count
    as always stacked. A conditional step whose `file:` is composed and that has no `foreach:` is a
    one-of step: it stacks at most one file per incarnation (`file: "inc/${ incarnation.id }.yaml"`),
    so its files are alternatives, and no incarnation stacks two of them. The stacks are:
    - S_all, every layer but the files of one-of steps;
    - S_base, the always-stacked layers alone;
    - S_L, the always-stacked layers and one conditional layer L, for each L in stack order, each
      file of a one-of step included.

    `validate-service` walks S_all, S_base, then each S_L. `validate-scenario` walks the service rung
    in S_all and S_base only, under the scenario's own rungs. A destiny's views hold no conditional
    layer, so their S_all is their S_base. A stack that is the same as one already walked is not
    walked again. A ring is reported once per view, by its names, at the first stack that finds it.
  - **Winners.** Inside one stack a read resolves as the Keeper's does: to the name's top winner, its
    cells merged deep within the rung and cut by `strategy: replace`; a self-read to the same below
    the reading cell's layer. A ring then passes a name more than once only through self-reads, and
    names it once per layer, as the Keeper's text does. A ring that needs one name from two layers at
    once is no ring.
  - **Citation.** A ring is cited at the string that reads the next name, in the layer that wrote it.
    In a map merged deep that can be a lower layer than the name's top: `m.a: "${ vars.p }"` of
    `00-base.yaml` closes `m → p → m` under `20-top.yaml`'s `m: {c: 3}`, and is cited at
    `00-base.yaml` `$.m.a`. The hint names the same layers.
  - **Literals.** A read whose result a bool literal decides is no edge and no need. In the branch of
    `?:` a literal condition never takes (`true ? 1 : vars.y`) CEL never evaluates it; in a term of
    `&&` beside a term that is always false (`false && vars.y`, `vars.y && false`), and of `||` beside
    one that is always true (`true || vars.y`), CEL skips it or drops its error, a cycle's included.
    A whole read there is none either, except a whole comparison: the Keeper materializes the view
    before it evaluates an expression that may compare the root (point 10), whatever decides the
    result, so `false && vars != {}` and `true || [vars].exists(m, m != {})` are whole reads. A
    condition or a term is always true or false when it is built of bool literals with `!`, `&&`,
    `||` and a comparison with a bool literal, as under Key tests.
  - **Limits.**
    - A cycle that needs two or more particular conditional layers, short of all of them, is missed;
      the stub below builds the stack its own incarnation selects.
    - Mutually exclusive `when:` conditions are not decided, so S_all may be a stack no incarnation
      builds.
    - A `foreach:` that reads `incarnation.*` is not evaluated: its step's files stack lexically at
      the step's position, not in the order an incarnation's list binds them, so a self-read from one
      of them to another is judged in lexical order.
    - A scenario's views walk the service rung in S_all and S_base only. A ring through a conditional
      layer that a higher conditional layer shadows in S_all, or through a one-of step's file, is
      missed there: `10-a.yaml`'s `x: "${ default(vars.s, 0) }"` under `20-b.yaml`'s `x: 2`, both
      `when:`-gated, and the scenario's `s: "${ vars.x }"` form `s → x → s`, which an incarnation in
      `a` alone raises.
    - A ring is reported once per view by its names, so a second definition that closes the same
      names into a ring from another layer is not reported until the first is fixed: over the base's
      `y: "${ vars.x }"`, `10-a.yaml`'s and `20-b.yaml`'s `x: "${ vars.y }-…"` are one `x → y → x`,
      cited at `20-b.yaml`. And the search walks at most 16 rings per strongly-connected group of one
      stack, those through the group's least name first, so in a larger group a ring through another
      definition may go unwalked until those are broken, and the ring the Keeper raises may be one of
      them.
  - **Order.** Without `_stack.yaml` the files stack lexically. With one, steps stack in step order;
    a constant `foreach:` stacks its files in binding order, and otherwise a step's files stack
    lexically at the step's position.
  - **Missing files.** A literal `file:` of a step that is not `optional:` names one path for every
    incarnation; when the tree lacks it, it is `var_invalid` at the step whatever the step's `when:`
    or `foreach:`, since the Keeper refuses every incarnation that stacks the step.
  - **Unknown rung below.** When the rung below is unknown, a self-read is not a cycle, since that
    rung may define the name. That covers a scenario linted without its service, a service rung that
    failed to assemble, and a destiny `vars.yml` that does not parse, which is `yaml_parse_error`.
  - **Key tests.** For a self-read, a key test (`'x' in vars`) protects only the reads where it holds.
    A condition built of key tests with `!`, `&&`, `||` and a comparison with a bool literal implies
    keys where it is true and where it is false (`&&`: any term's where true, every term's where
    false; `||` the reverse), and protects the `?:` branch on each side; a term of `&&` (`||`) is
    protected by what any other term implies where true (false). A bool literal implies every key on
    the side it never takes, so `'x' in vars || false` implies `x` where true. Any other condition
    implies nothing. `'x' in vars && vars.x != '' ? vars.x : y` with nothing below is no cycle,
    `'x' in vars ? y : vars.x` is.
  - **Whole reads.** A whole read of `vars` reads every name of the view, the definition's own below
    it included. `type(vars)` and `'x' in vars ? vars : {}` are whole reads, conservatively.

  `validate-service` additionally evaluates the WHOLE service view against a stub incarnation and
  fails on any error. The stub's service name is the one given by `--service-name`, else a
  placeholder: a manifest no longer states its own name (NIM-726). Every `validate-*` command shares
  one flag parser (`soul-lint/cmd/soul-lint/main.go:202-208`, passed on at `:257`). At `2cc27029`
  `validate-service` parsed the flag but neither advertised it (the usage line, `main.go:105`) nor
  used it (`soul-lint/internal/validate/validate.go:262` read it for scenarios only). It advertises it
  (`main.go:109`) and hands it to the stub (`serviceVarsDiags`, `validate.go:188`).

  The stub also has a placeholder id and empty covens and traits, so a coven-gated file is evaluated
  only by the static walk. Its traits are an empty map, as the database row's are:
  `has(incarnation.traits)` holds, and only a key in it can be missing. Empty traits are intended: a
  trait is optional per incarnation, so a definition reading `incarnation.traits.<k>` without `has()`
  fails for every incarnation lacking it, and the check says so before a run does. The stub skips a
  missing file only when the step's `file:` is composed and reads `incarnation.*`, itself or through
  the binding of a `foreach:` that reads it: such a path names the placeholder, and the walk covers
  every file the step can name. So a composed path every incarnation lacks, such as
  `coven/${ c }.yaml` over `incarnation.covens + ['zz']`, is not reported. Its rings are deduplicated
  against the walk's by their names, and its failures by the definition, name and layer, the walk
  already reported.
- **(b) A strict render mode**, set by the L0 trial harness and by Go tests and never in production,
  materializes whole every TASK view the render built — each host task × host × loop iteration, each
  keeper task, each `assert:` task on the host it is decided on (`evalAssertTask`,
  `pipeline.go:1342-1346`), each destiny task × host × loop iteration — and fails the case on any
  error. The other host-free views
  — the loop axis, an include `when:`, a `template:` path — evaluate only what their cells read, as in
  production: a scenario definition reading `compute.*` is refused on the loop axis by design (point
  6), and materializing that view whole would fail a valid case. Nor is a view on the stand-in host
  of an empty target or roster materialized: a host task whose `where:` targets no host
  (`len(targeted) > 0`, `pipeline.go:747`), an `assert:` on an empty roster (`:1384`), an applier
  with no targets (`destiny.go:615`). That host is no host, and a definition over its
  `soulprint.self` would fail though no host reads it.
  `TestStrictVars_LeavesTheViewsItDoesNotClaimAlone` pins the three stand-in host cases (the host
  task, the applier with no targets, the `assert:` on an empty roster) and the three host-free views.

  A keeper task's view is materialized whole. Of its rungs only its own `vars:` can name a host root,
  and that failed eagerly at `2cc27029` already. Its view also holds the scenario rung, evaluated in
  the keeper task's own context (`keeperVars`, `dispatch.go:210-219`), where `incarnation.host_count`
  is 0. So a scenario var that fails there, such as `100 / incarnation.host_count`, fails strict mode
  through any keeper task, as it would fail that task if read. The rule is deliberate: a var must
  evaluate in every view it is visible in
  (`TestStrictVars_AKeeperTaskChecksTheScenarioRungInItsOwnContext`,
  `keeper/internal/render/varview_test.go`).

  It covers the views a case's render BUILDS. A view the render never builds is not covered: the
  tasks of a dropped include group, a loop with zero items. Nor is the view of a task a static
  `when:` skips: `staticWhenSkips` builds one for its flow context, but an inactive task may
  legitimately read a name only another mode of the scenario defines, and materializing it would
  fail a valid case. In all three, soul-lint's static walk (a) is the backstop — compile, cycle,
  unknown reference — and an evaluation error nothing builds a view for surfaces only on a run that
  does. Strict mode is an explicit render flag that the L0 harness and Go tests set; nothing turns
  it on implicitly.

  An L0 case's `fixtures.vars` stays the service rung (`keeper/internal/trial/harness.go:279`), but its
  values are now definitions, evaluated on request like a file's. They are evaluated against the
  case's own incarnation — its id and service (`harness.go:278-279`), with empty covens and an empty
  `traits` map, as soul-lint's stub. A case carries no incarnation labels.

  Strict mode is unrelated to text/template's strict mode (`missingkey=error`).

#### 13. Reading the whole namespace

What the user called "request everything" is a whole read of the namespace: `${ vars }`, an equality
over `vars`, an iteration over its values, `vars` itself passed to `merge()`, `toYaml .vars` /
`range .vars` / `index .vars` in a `.tmpl`, a whole read of `vars` in `flow_context` — and strict mode
(point 12b). There is no new UI, no new API and no new metric. A metric was proposed and dropped by
the user on 2026-10-19: what it would watch is proven in CI instead, by the evaluation-counter guard
("not requested ⇒ not evaluated") and by a benchmark that measures the saving at one host and at N,
on the model of `BenchmarkApplyInput_ResolveByRoster`. If production ever needs it, it is its own
ticket.

#### 14. `vars/_stack.yaml`: a step chooses files, never values

The user's decision. `inline:` is REMOVED: it is refused when the stack is parsed, as
`stack_step_invalid`, with the hint to put the values in a file under `vars/` and name it with
`file:`. The selectors — `when:`, `foreach:`, `file:` — see ONLY `incarnation.*` and the `foreach:`
binding; `vars.*` there is refused as `stack_step_invalid`. A step decides which files are stacked,
and reading a value before the final merge is early binding:

```yaml
# vars/_stack.yaml — refused
stack:
  - file: 00-base.yaml          # tls: false
  - file: 50-tls.yaml
    when: "vars.tls"            # read here, while tls is still false
  - file: 90-wb.yaml            # tls: true
```

Evaluated in step order, this leaves `50-tls.yaml` out silently while the merged result says
`tls: true`. The `foreach:` binding is not visible inside the stacked file's contents: those are
evaluated after the merge, on request, where no step is in scope. The §3 example with `inline:` and
`when: "!has(vars.maxmemory)"` is superseded; the normative grammar and example are
[`architecture.md` → Service vars](../architecture.md#service-vars-the-assembly-pipeline).

#### 15. The seal stays static and conservative

The seal of [templating.md §7.4](../templating.md#74-secret-masking) is still decided from RAW
definition text, once per task, over the whole view's raw definitions — never per host, because
provenance must not depend on the host. It is static analysis and evaluates nothing, so laziness buys
nothing here, and the whole-read rule below needs the whole view anyway. A name is tainted if ANY of
its definitions in the view reads a secret source or a tainted name, self-reference included: the
conservative union, the same direction as the rule before this amendment, which carried the layer
below down whole (`sealedVarNames`, now replaced by `sealViewVars` in `render/seal.go`).

- **Every string of a definition counts, at any depth.** At `2cc27029` only top-level strings
  participated (`render/seal.go:116-118`, `:149-151`), because nothing nested was evaluated. Now it is, so a
  definition's taint is the union over every string in it. The top-level name is tainted WHOLE:
  `creds: {password: "${ input.password }"}` seals a cell reading `${ vars.creds }`.
- **Service definitions are walked inside a task's view.** Their text still cannot name a secret
  source, but under late binding one reads the task's override: a service
  `dsn: "redis://:${ vars.password }@localhost"` under a task's `password: "${ input.password }"` (a
  `secret: true` input) taints `dsn` in that task. Scenario-rung definitions are walked the same way;
  they can read an `input.<secret>` or a secret `incarnation.state.<field>` directly (`vault()` is
  refused there, point 6).
- **Index form.** `vars['lit']` on the `vars` root is read as `vars.lit`.
- **A read that names no field.** A non-literal `vars[expr]`, or a whole read of `vars`
  (`${ vars }`, `merge(vars, …)`), names no field the walk can follow — in a cell OR inside a
  definition. Either way it is tainted if any name of the view is tainted, computed over the whole
  view rather than lazily. Inside a definition this matters most: a service file's
  `pick: "${ vars[vars.k] }"` read by a cell as `${ vars.pick }` would otherwise carry a task's secret
  one hop down, in a file the task's author never sees.

This closes the `vars` root of NIM-827 (a whole-map read is not sealed), in cells and definitions
alike. A whole read of `vars` is the one that materializes every definition (point 13), so leaving it
unsealed would hand over exactly the values the seal exists for. NIM-827 stays open for the other
roots (`input`, `register`, `incarnation`), and NIM-830 for indexed reads on them. Sealing more only
masks more output; no value changes.

#### 16. The analyses that follow a name through the view

**A read of `vars.<x>` counts as a read of everything the definitions of `x` in the task's view read.**
It is transitive and conservative: the union over every rung's definition of `x`, the winning one or
not, and on through every name those definitions read in turn. The rule applies to the three
analyses below. It does not apply to every analysis of task text: the Passage cut needs no following
(below), and the static-predicate test is NIM-1020.

- the seal (point 15);
- NIM-908's host-variance classification (`hostVariantInputNames`, `hostVariantDestinyVars`,
  `render/destiny.go:671`, `:831`, built over the view by `viewClosure`, `render/varview.go:137`);
- `soul_needs`' targeting and moving-state sets (`targetingKnown` and `movingVars`,
  `render/soul_needs.go:234`, `:333`), and its include-`when:` test in `off()` (`:206-207`). At
  `2cc27029` that test passed no vars at all (`movesWithState(task.IncludeWhen, step, nil, nil)`,
  `:199`), while an include `when:` now reads the scenario rung (point 5).

These analyses read RAW text, as before, and EVERY string of a definition at any depth: point 1
makes a nested `${ … }` evaluate, and at `2cc27029` `soul_needs` read top-level strings only
(`:232-234`, `:291-292`).

At `2cc27029` they walked the task's own `vars:` text. That was enough while no lower rung could see a task
value. Under late binding a service or scenario definition can carry a dependency to a name the task
text never spells:

- its own dependency — a scenario `x: "${ incarnation.state.y }"` read by `where: "vars.x == 'a'"` after
  a capture;
- a task override's dependency — a service definition derived from a task var over a per-host input,
  a secret or moving state.

An analysis that stopped at the task text would miss it.

**The Passage cut needs no change, and that is point 6's work rather than this rule's.** Its register,
Vault and roster axes (`taskOwnReads`, `shared/config/passage.go:403`, walking `t.Vars` at `:410`;
`taskReadsVaultSecret`, `passage_vault.go:83`, `:90`; `taskReadsRoster`, `passage_refresh.go:211`,
`:222`) already walk the task's WHOLE `vars:` map, nested values included, whether or not anything
reads a name. No lower rung may name `register`, `vault()` or `soulprint` at all: the service env
does not declare them, and the scenario rung refuses them. So every root those axes look for is
already in the task text they read. The closed list of point 6 is what keeps it so, and it carries
its own guard.

One indirection stays unfollowed, and it is not this amendment's: `compute.*`.
- A `compute:` entry may read `register.*` (`compute.go:50`) and runs on the Keeper's engine, which
  has `vault()`.
- No Passage axis follows a `compute.<x>` read (`passage.go:403-418` scans text).
- That holds today for a task spelling `compute.x` directly. A scenario definition reading
  `compute.*` adds one more hop to the same gap, not a new gap.
- Filed as **NIM-1021**.

The static-predicate test (`config.IsStaticPredicate`) is NOT on this list either: it is the
NIM-1020 defect, which reads the predicate alone.

"The SERVICE vars layer is deliberately not walked" (`destiny.go:711` at `2cc27029`, and §3 above) is superseded.

The destiny's per-host set was computed once per pass at `2cc27029` (`destiny.go:296`, from `vars.yml` alone).
Under late binding it belongs to a task view, so it becomes the union of the pass's `vars.yml` and the
task's own block + task rung. The consequence for a destiny static `when:` is in point 7.

#### 17. `render_context.vars` and `flow_context.vars` are requests against the task view

`flow_context` ships the names the task's predicates read (a whole read → all of them), with
late-bound values.

`render_context` keeps the NAME set of `2cc27029` — the service and file-rung names the template
reads, plus `params.vars` — and adds the scenario rung's. A whole read in the template
(`index .vars "x"`, `range`, `toYaml .vars`) widens the service and scenario rungs and NEVER a
destiny's `vars.yml`, exactly as at `2cc27029` for the service layer (`pipeline.go:716-735` at
`2cc27029`; `renderContextVarNames`, `render/varview.go:304-321`): the file layer's internal
plumbing would otherwise cross the wire for nothing. What changes is where the VALUES come from — the task view. So `.vars.acl_file` in
a template equals `${ vars.acl_file }` in that task's params, and a name the task's own `vars:`
overrides now reaches the template with the task's value. That last part is breaking (see
Consequences). No proto field changes, and Soul is untouched.

#### Corpus scan (2026-10-15)

Turning evaluation on breaks no known repository:

- `_stack.yaml` exists in NO repository — `wb/service/redis`, `soul-stack-services/redis`,
  `examples/`, `dev/`, `destiny/soul` — only in Go test fixtures.
- `${` inside a service vars file occurs only in YAML comments, zero values: three lines,
  `soul-stack-services/redis` `vars/30-acl-users.yaml:9` and
  `examples/service/dragonfly/vars/00-base.yaml:76`,
  `:87`. A fourth, in `wb/service/redis`, was removed by NIM-1015 the same day.
- A task whose static `when:` reads a host-dependent task var — over `soulprint.self`,
  `register.*` or a per-host destiny input (the NIM-1020 shape, and the one the destiny-pass refusal
  of point 7 hits): zero, across `destiny/`, `wb/service/`, `services/` and `examples/`. Fifteen
  tasks there have a `when:` over `vars.`, and none of them carries a `vars:` of its own.
- A `${` nested in a `vars.yml` or task `vars:` map or list value: zero. The scan covered 386 YAML
  files under `wb/`, `services/`, `destiny/` and this repository's `examples/`, `dev/`, `tests/` and
  `keeper/internal/trial/`.
- A scenario's top-level `vars:`: zero uses.

#### Consequences

Breaking, each one stated:

- `${ … }` in a service vars file now evaluates.
- A `${ … }` nested in a `vars.yml` or task `vars:` value now evaluates.
- `inline:` in `_stack.yaml` is refused, and so is `vars.*` in a `_stack.yaml` selector.
- A broken var nobody reads no longer fails a run; soul-lint and strict mode catch it.
- A task var may read a `vars.yml` name, and a `vars.yml` definition sees a task's override.
- A scenario's top-level `vars:` starts applying.
- `ErrVarIndexForm` is gone: `vars['k']` and `vars[expr]` work inside a var definition too.
- `has()` and `default()` over a broken var now raise its error.
- A `${ … }` in an L0 case's `fixtures.vars` now evaluates.
- A template's `.vars.<name>` for a name the task's own `vars:` overrides, or for one derived from
  such a name, now carries the task's value instead of the lower rung's — the service's, or a
  destiny's `vars.yml` (point 17).
- A scenario `vars:` definition naming `soulprint`, `register`, `vault()` or `generate_secret()` is
  refused at compile, and one reading `compute.*` is refused on the loop axis (point 6).
- A destiny static `when:` over a host-dependent task var is refused at render instead of decided by
  the first host (point 7).
- An `assert:` task's own `when:` gate reading a name its task's `vars:` overrides now gets the
  task's value instead of the service's (point 7): `vars: {cluster: false}` with `when: "vars.cluster"`
  over a service `cluster: true` ran the assert and now skips it.
- A keeper task's static `when:` is decided in `keeperVars` instead of on the roster's first host
  (point 7): `incarnation.host_count` there is 0, so `when: "incarnation.host_count > 0"` skips it.
- A cell reading `vars` whole or by a non-literal index is sealed when any name of the view is tainted
  (point 15). Output is masked more; no value changes.
- A comprehension over a map from any root — `input`, `vars`, `register`, `incarnation`, `compute`,
  `soulprint`, a loop binding — or from `merge()` / `vault()` iterates in key order
  (`shared/cel/ordered.go`), so `.map(…)` builds one list and a failing `.all(…)` fails on one
  element on every render; a map literal does not. Two CEL keys that convert to one native key (`1`
  and `'1'`) are an error naming that key
  ([templating.md](../templating.md#collections-in-the-template---map-not-list-if-the-determinism-of-the-row-order-is-important-normative)).

Relaxed rather than broken: a `has()`-guarded `vars.<x>` inside a var definition (point 12a) is no
longer refused, and the loop axis, the `template:` path and an include `when:` now also see the
scenario `vars:` rung (point 5). An `assert:` task's own `when:` gate reading a task var the
service lacks no longer fails on a missing key (point 7).

Left to **NIM-1020**: in the scenario pass a static `when:` / `assert:` over a host-dependent task
var is still decided on the first host, as before (point 7). Two new paths join it there: a service
or scenario name derived from such a var, and an `assert:` task's own `when:` gate reading one, which
at `2cc27029` failed on a missing key (or, `has()`-guarded, was simply false), or read the service
value of the same name, instead.

Given the corpus scan, none of these hits a known repository. Unchanged: the two passes and the
`apply:` boundary, the ADR-010 cell rule, the migration CEL environment (`vars` stays undeclared
there), the absence of an operator rung (§2), the `vars_shadows_service_var` and `vars_collision`
warnings, every proto field, and Soul.

#### Guards the code carries

- a cycle, with its normalized path, identical for every request order;
- late binding: a service-derived value follows a task override (the `conf_dir` / `acl_file` example
  of point 5);
- not requested ⇒ not evaluated, on the destiny and the task rungs: an unused broken var does not fail
  the render, and an evaluation counter proves the skip;
- taint along a chain: a service definition over a task override that reads `input.<secret>` seals
  the cell;
- determinism under a randomized request order: identical values and identical error text;
- memo reuse: one evaluation of an untouched service var across N tasks × M hosts;
- a failed definition is evaluated once per read: a 24-var cyclic group and a 30-var failing lattice
  fail with their usual error, each definition evaluated once, through every kind of read
  (`TestVarView_AFailedDefinitionIsEvaluatedOncePerRead`), and a Keeper render reading `vars.v00`
  over either returns that error within n evaluations per host read
  (`TestFailingVars_ARenderReturnsTheFailurePromptly`, point 8);
- the failure memo changes no outcome: over 2400 seeded random views, one or a parent and a child,
  every Lookup, Materialize, Check and cell succeeds or fails, with the same value, as it does with
  the memo off, and evaluates no definition more than three times
  (`TestVarView_TheFailureMemoChangesNoOutcome`, point 8);
- an equality over a copy of `vars`, a comprehension's or merge()'s, is a whole read, so its outcome
  does not depend on the order of requests (`TestVarView_AComparedCopyOfVarsIsTheSameInEveryOrder`,
  point 10);
- a kept failure that depends on a definition that returns failed depends on that one's parent
  alone: it survives the failed return and is dropped when the parent returns
  (`TestVarView_AFailureOutlivesAFailedReturnAndNotItsParent`, point 8);
- a benchmark of a task view's render at one host and at N, on the model of
  `BenchmarkApplyInput_ResolveByRoster`, so the saving is measured rather than quoted (point 13);
- strict mode fails a trial on an unused broken var;
- the loop axis does not see a task-rung definition (point 5);
- a `has()`-guarded read is not `var_unknown_ref` in soul-lint, an unguarded one is, and
  `default(vars.x.y, d)` does not exempt `x` (point 12a);
- a scenario `vars:` definition is refused at compile for EVERY entry of point 6's closed list —
  `soulprint.self`, `soulprint.hosts`, `soulprint.where(…)`, `register.<p>`, `register.hosts`,
  `vault()`, `generate_secret()` — also inside a nested string (`{a: "${ register.p.x }"}`) and for
  `register.self`. This is what keeps the Passage cut's axes complete (point 16);
- a nested secret seals the whole name (`creds.password` → `${ vars.creds }`), and a definition reading
  `vars[expr]` or `vars` whole is tainted when any name of the view is (point 15);
- an error inside a tainted definition is withheld however many hops away the asking cell is
  (point 11);
- a template's `.vars.<x>` equals `${ vars.<x> }` in the same task's params, and a destiny's `vars.yml`
  is not widened by a whole read (point 17);
- a CELL reading `${ vars }` is sealed when any name of the view is tainted, and is not when none is
  (point 15);
- the name-following rule for the two analyses other than the seal (whose case is the "taint along
  a chain" guard above): a task reading `vars.<x>` gets
  - `soul_needs`' moving-state mark for a scenario `x` over `incarnation.state` after a capture —
    from a `where:`, a static `when:` and an include `when:` alike, and for a nested string too;
  - the host-variance mark for a service `x` over a per-host override (point 16);
- a scenario definition reading `compute.*` is refused on the loop axis, and read in a task (point 6);
- a destiny static `when:` over a host-dependent task var is refused (point 7);
- an `assert:` with `vars: {cluster: false}` and `when: "vars.cluster"` is left unevaluated, at
  render and in `EvalAsserts` alike (point 7). At `2cc27029` that case runs the assert (with a
  service `cluster: true`) or fails on a missing key (without one), so the guard can fail;
- strict mode fails a scenario var that fails in a keeper task's context, where `host_count` is 0,
  though a lenient render passes it, and passes it guarded
  (`TestStrictVars_AKeeperTaskChecksTheScenarioRungInItsOwnContext`, point 12b);
- a comprehension over a map from each root, the lazy `vars` included, walks its keys in sorted
  order (`TestComprehensionOverAMap_IteratesInKeyOrder`);
- a comprehension over a map fails on the same element on every render, for a task var's map and an
  input map alike (`TestFailure_AComprehensionOverAMapFailsOnTheSameKey`);
- a create's pre-flight gate, with no row yet, gives a service var an empty `traits` map, as the run
  does (`TestPreflightAssert_CreateWithoutTraitsSeesAnEmptyTraitsMap`);
- soul-lint judges a conditional definition another conditional layer shadows, in that layer's
  stack over the always-stacked ones, while an always-stacked one, or a present `optional:` file,
  still shadows (`TestVarCycle_ShadowedConditionalLayer`, point 12a);
- inside one stack a read has one winner, so rings whose layer choices contradict each other are no
  `var_cycle` (`TestVarCycle_ContradictoryLayerChoicesAreNoCycle`), a ring only one conditional
  layer forms is (`TestVarCycle_EachConditionalLayerAlone`), and the walk costs a pass per stack,
  S_base included (`TestVarCycle_WorkIsLinearInTheLayers`, point 12a);
- the files of a one-of step are alternatives: S_all stacks none of them, so `inc/a.yaml`'s
  `maxmemory` over `mem` and `inc/b.yaml`'s `mem` over `maxmemory` are no `var_cycle`, as the Keeper
  agrees for ids `a`, `b` and another, while a ring inside one per-id file is
  (`TestVarCycle_OneOfStepFilesAreAlternatives`); `servicevars.EveryLayer` marks a conditional
  composed `file:` without `foreach:` as one-of, and only it (`TestEveryLayer_OrderAndConditionality`,
  point 12a);
- S_base finds a ring of the always-stacked layers that a conditional layer changes in S_all, in
  `validate-service` without the stub's help and in `validate-scenario` through a scenario var, and
  the stub does not report it again (`TestVarCycle_StubRingIsNotReportedTwice`, point 12a);
- a read whose result a bool literal decides is no edge, and its live counterpart is, while a whole
  comparison is a whole read there too, each row checked against the Keeper's view
  (`TestVarCycle_ADeadReadIsNoEdge`, `TestExprReads_NamesAndGuards`, point 12a);
- a ring through a map merged deep is cited at the string that reads, in its own layer
  (`TestVarCycle_CitesTheCellThatReads`, point 12a);
- a self-read over an unknown rung below is no `var_cycle` in soul-lint
  (`TestVarCycle_NothingKnownBelowIsNoCycle`, point 12a);
- the stub skips a missing file only when its step's composed `file:` reads `incarnation.*`, itself
  or through a `foreach:` binding, and a missing literal `file:` is `var_invalid` at its step
  whatever its `when:` or `foreach:` (`TestVarInvalid_StubSkipsAFileNamedByItsIdentity`, point 12a);
- a key test protects only the branch where it holds (`TestVarCycle_InvertedKeyTestInATask`,
  point 12a);
- soul-lint's var findings come in the same order on every run, byte for byte
  (`TestVarChecks_OrderIsFixed`).
