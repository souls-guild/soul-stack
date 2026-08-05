## ADR-0082. Service vars replace Essence — one `vars` namespace, `incarnation.spec.essence` removed

**Status:** active
**Amends:** [ADR-008](0008-coven-stable-tags.md) (essence is role-agnostic; the assembly order `default → os → coven → incarnation.spec`), [ADR-009](0009-scenario-dsl.md) (the scenario template context and its reserved names), [ADR-010](0010-templating.md) (the CEL roots and the `core.file.rendered` context), [ADR-012](0012-keeper-soul-grpc.md) (the `flow_context` key set carried to Soul)
**Implemented by:** NIM-412 · NIM-413 · NIM-414 · NIM-415 · NIM-416, with the column drop in NIM-408

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
not the Run form … are NOT persisted in state … day-2 `add_node` reads
`essence.conf_dir`/`data_dir`."*

Not one of them moves. They stay in `vars/00-base.yaml`; only the way to override them
changes.

That second group is also why this decision keeps a different boundary intact. Those keys are
desired constants that must survive between runs, and `incarnation.state` is closed to them:
**state is a projection of what IS, and these describe what SHOULD BE.** Any design that
relocated them into `input:` would have parked desired values in `state` through the day-2
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
`core.file.rendered` templates, whose `render_context` is built per host. **Not
`apply: input:`** — that one resolves on `targeted[0]` (`destiny.go`), so a destiny receives
ONE set of values for its whole roster, and a `soulprint.self.*` in it silently means the
first host by SID. That predates this ADR and is unchanged by it; it is stated here because
it is the boundary that decides the paragraph below.

**Per-host service vars were considered and deferred, with a measured reason.** Making the
layer resolve per host is mechanically straightforward — the plumbing for a per-host map
already exists (`RegisterByHost`, `DestinyVarsResolved`). What stops it is reach: the two
host-invariant boundaries above would not move with it. `apply: input:` carries **136**
`vars.` reads across the thirteen shipped services, and `compute:` — whose own contract says
host-invariance is what makes it safe to feed `apply.input` and `state_changes` at once —
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
the public contract anyway (`spec` and `essence` leave `GET /v1/incarnations/{name}`) and
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
  Mechanically workable (`form_layout.go` has `Collapsed`/`ShowWhen`, day-2 prefills from
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
  leave `GET /v1/incarnations/{name}`; every service repo renames a directory and a file and
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
