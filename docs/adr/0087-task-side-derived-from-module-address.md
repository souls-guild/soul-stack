## ADR-0087. A task's side is derived from its module address, not declared by `on:`

**Status:** accepted, **implemented** for the core-address rule (NIM-749) and for the plugin
executor (NIM-758); **NIM-750** — sweeping the now-redundant `on: keeper` out of the service
repositories — is outstanding (epic NIM-747; this ADR is NIM-748)
**Amends:** [ADR-009](0009-scenario-dsl.md) (the orchestration delta: `on:` returns to one meaning — a
list of covens — and the scalar `keeper` leaves the key **on core addresses**; on a plugin address it
stays legal permanently, see (f)), [ADR-017](0017-keeper-side-core.md)
(the keeper-side dispatcher is no longer `on: keeper`; the module address is the dispatcher),
[ADR-020](0020-plugin-infrastructure.md) (the schema document's **per-module** object grows a `side`
field, default `soul`), [ADR-0084](0084-explicit-state-capture.md) (F-D's rule survives unchanged and
its trigger moves from the `on:` key to the `module:` key; the `state_capture_not_on_keeper`
diagnostic it introduced is retired)
**Implemented by:** NIM-749 (the `side` field, the core-address diagnostics, the side-derived
routing and stratifier), NIM-758 (the keeper-side plugin executor that makes `side: keeper` a
control rather than a declaration). Outstanding: NIM-750.

---

## Problem

`on:` is overloaded. For a Soul task it is a coven filter — a list of labels naming where the step
runs. `keeper` is a magic scalar in the same key meaning something unrelated: *do not send this to
hosts at all*. One key, two jobs, and the second one is not a target at all but a routing verdict the
platform already knows.

The cost is visible in the live corpus rather than in theory. `examples/service/redis` carries
**89** `on: keeper` lines across 14 files, 20 of them in `scenario/create/main.yml` alone, and the
whole of `examples/` carries **171** — the great majority on `core.state.<verb>` captures, where the
address `core.state.set` has already said everything the `on:` line repeats. Every count in this ADR
is of `on:` at **key position** with the value exactly `keeper`
(`grep -rE '^[[:space:]]*(-[[:space:]]+)?on:[[:space:]]*keeper[[:space:]]*$' --include='*.yml' --include='*.yaml' examples/`);
a comment or a line inside a block scalar neither parses nor stratifies and is not counted, and
`tests/` carries none.

Six validators live around the key in [`shared/config/scenario_task.go`](../../shared/config/scenario_task.go),
and one of them (`state_capture_not_on_keeper`, `:1404`) exists for no purpose other than to force
the author to write a line the platform could have derived.

The redundancy is not merely verbose. It is a second, author-maintained copy of a fact the tree
already holds in code, and the two can disagree — which is the failure mode recorded in Consequences
(h) below.

## Decision

### (a) The side is derived from the module address, and the two core registries are disjoint

A task's side is decided by the base of its `module:` address. `on:` returns to one meaning: a list
of covens (or omitted, meaning the whole roster).

The derivation is unambiguous because the two core registries share no name. **Keeper-side, seven
bases:**

| base | registration |
|---|---|
| `core.bootstrap` | [`keeper/internal/coremod/bootstrap/delivered.go:77`](../../keeper/internal/coremod/bootstrap/delivered.go) |
| `core.cert` | [`keeper/internal/coremod/cert/registered.go:53`](../../keeper/internal/coremod/cert/registered.go) |
| `core.choir` | [`keeper/internal/coremod/choir/member.go:43`](../../keeper/internal/coremod/choir/member.go) |
| `core.cloud` | [`keeper/internal/coremod/cloud/provisioned.go:61`](../../keeper/internal/coremod/cloud/provisioned.go) |
| `core.soul` | [`keeper/internal/coremod/soul/registered.go:55`](../../keeper/internal/coremod/soul/registered.go) |
| `core.state` | [`keeper/internal/stateop/verb.go:21`](../../keeper/internal/stateop/verb.go) |
| `core.vault` | [`keeper/internal/coremod/vault/kvread.go:52`](../../keeper/internal/coremod/vault/kvread.go) |

**Soul-side, twenty-one bases**, registered in one map at
[`soul/internal/coremod/registry.go:62-82`](../../soul/internal/coremod/registry.go): `core.archive`,
`core.augur`, `core.cmd`, `core.cron`, `core.directory`, `core.exec`, `core.file`, `core.firewall`,
`core.git`, `core.group`, `core.http`, `core.line`, `core.module`, `core.mount`, `core.noop`,
`core.pkg`, `core.repo`, `core.service`, `core.sysctl`, `core.url`, `core.user`.

**No name appears in both**, so the address answers the routing question with no ambiguity to
resolve. Routing today is one line —
`IsKeeperTask(task) = task.On.(string) == "keeper"`
([`keeper/internal/render/dispatch.go:88-91`](../../keeper/internal/render/dispatch.go)) — and it
becomes a lookup of the address instead.

### (b) The refusal matrix

An omitted `on:` now means two different things depending on the address, and that is the one
genuine author-facing cost of this decision. It is stated as a table rather than as prose so that no
reader has to reconstruct it.

| address side | `on:` written | verdict | diagnostic |
|---|---|---|---|
| keeper-side core | `keeper` | error — redundant | `on_keeper_redundant` |
| keeper-side core | `[coven-a]` | error — never silently ignored | `on_covens_on_keeper_module` |
| keeper-side core | omitted | the correct form | — |
| Soul-side core | `keeper` | error | `on_keeper_on_soul_module` |
| Soul-side core | omitted / covens | unchanged | — |
| non-core (plugin alias) | `keeper` | legal, see (f) | — |
| base absent from `shared/coremanifest` | anything | silent, routes by the written `on:` | — |

**`on: keeper` on a Soul-side address is a separate code from redundancy.** They are opposite
mistakes with opposite fixes — *"delete the line"* against *"this step cannot run where you sent
it"* — and collapsing them into one diagnostic would hand the author the wrong remedy. Today the
Soul-side case is accepted offline and dies at
[`keeper/internal/scenario/keeper_dispatch.go:274-276`](../../keeper/internal/scenario/keeper_dispatch.go)
(`unknown keeper-side module`), after the run has started, at the head of its Passage.

**`on: [coven-a]` on a keeper-side address is an error and is never ignored.** Silently dropping a
targeting key is the same defect class [ADR-0084](0084-explicit-state-capture.md)'s F-D spent a
section removing for `when:`: a file that says the step is targeted, and a step that ignores it.

### (c) The unknown rule — a base with no declaration has no side

**A base absent from `shared/coremanifest` has no side: every new diagnostic in (b) stays silent for
it, and routing falls back to the written `on:`.**

This is not implementer's discretion. Two live bases are absent from the shared table today:

- **`core.cert`** — the keeper-side module exists
  ([`keeper/internal/coremod/cert/registered.go:53`](../../keeper/internal/coremod/cert/registered.go))
  and `core.cert.registered` / `core.cert.issued` are shipped states
  ([ADR-017](0017-keeper-side-core.md) amendments 2026-07-01 and 2026-07-09), but there is no
  `mod_cert.go` and no `modCert` entry in `coreModules`
  ([`shared/coremanifest/coremanifest.go:51-83`](../../shared/coremanifest/coremanifest.go)).
- **`core.augur`** — the symmetric Soul-side hole, already recorded as **unchecked** in
  [ADR-0076(p)](0076-engine-compat-window.md): *a core module with no embedded manifest at all is
  unchecked rather than "accepts nothing"*.

Without this rule, `on: keeper` on `core.cert.registered` would be reported redundant — a **false
error on working code**. The rule is the same principle ADR-0076(p) already states: absence of a
declaration is not a declaration of absence.

Both holes are to be closed in this epic, in the same move that adds the field. The rule stays
afterwards, because it is what makes adding the eighth keeper-side module safe.

### (d) Where the offline answer comes from — one table, not a new constant set

Nothing new is introduced to hold the mapping.
[`shared/coremanifest/coremanifest.go:51-83`](../../shared/coremanifest/coremanifest.go) already
lists both sides in a single `coreModules` slice, separated only by the comment
`// Keeper-side core (ADR-017/ADR-044, on: keeper)` at `:74`. It already lives in `shared/`, it is
already imported by `soul-lint` and by `shared/config`
([`shared/config/module_params.go:9,:48`](../../shared/config/module_params.go)), and it already
depends on nothing but `sdk/schema`. The comment becomes a field.

The field is **`side` on `sdk/schema.Module`**
([`sdk/schema/schema.go:114-128`](../../sdk/schema/schema.go)) — one definition with two homes, core
and plugin, instead of two definitions of one fact.

It subsumes three ad-hoc constants that are the present drift surface:

- `stateModuleAddr = "core.state"` ([`shared/config/passage_refresh.go:295`](../../shared/config/passage_refresh.go))
- `refreshModuleAddr = "core.soul.registered"` ([`shared/config/passage_refresh.go:86`](../../shared/config/passage_refresh.go))
- `vaultEmitterModuleAddr = "core.vault.kv-present"` ([`shared/config/passage_vault.go:49`](../../shared/config/passage_vault.go))

The agreement is pinned by the guard-test pattern that already exists for exactly this hazard:
`TestManifestAndDispatchAgree`
([`keeper/internal/stateop/manifest_pairing_test.go:18`](../../keeper/internal/stateop/manifest_pairing_test.go)),
whose own comment says *"nothing but this test holds them together"* — widened from one module to
registry level.

> ⚠ **The keeper registry is conditional, not static.** `core.state` needs `Vault` + `StateStore`,
> `core.choir` needs `ChoirStore`, `core.cert` needs `CertStore` + `Vault`, and `core.bootstrap`
> needs an issuer or a delivery set. The shared table is the **maximal** set, so the guard test must
> build a full-dependency registry — otherwise it passes vacuously on three of the seven bases and
> proves nothing about them.

### (e) `side:` in the module schema document — per module, default `soul`

A plugin module declares its side with **`side: keeper | soul`**, **default `soul`**, in the
per-module object of the schema document — beside `capabilities` and `side_effects`, which are
already per module for the reason stated at
[`sdk/schema/schema.go:106-108`](../../sdk/schema/schema.go): *invoking `acl` must not disclose what
`config` touches*. A bundle serves several modules of one subject, so a document-level field would
foreclose a mixed artifact and would also land on the three kinds that carry no `modules[]` at all
(`cloud_driver`, `ssh_provider`, `soul_beacon`), where "where the module runs" has no meaning.

**The absent key and `side: soul` are indistinguishable BY DESIGN, permanently.** That sentence is
written in those words so that nobody later "normalises" stored documents by stamping the default
in — doing so changes every document's bytes and therefore every sha256. Three independent reasons:

1. **Documents are signed and stored.** Requiring the key would invalidate every existing approval
   at once (`plugin_sigils`, the Sigil signature, `plugin.allow`).
2. **A required key would make the stated default a lie.** If
   [`sdk/schema/validate.go`](../../sdk/schema/validate.go) refused a document without it, "default
   `soul`" would describe nothing.
3. **The non-`soul_module` kinds have no modules to carry it.**

### (f) The plugin boundary — accepted and inert until the executor exists

> **Amended 2026-09-03 (NIM-758): the executor exists, the boundary does not move.** `applyKeeperTask`
> now falls back from the core Registry to the discovered plugins and runs one declaring
> `side: keeper` ([keeper/modules.md → keeper-side plugin modules](../keeper/modules.md#keeper-side-plugin-modules)),
> so the "honoured by nothing" half below is **superseded**. What is NOT superseded is the ruling
> this clause exists for: `on: keeper` on a plugin address stays legal and stays REQUIRED, now
> permanently. The reason has changed shape — it is no longer "the keeper cannot execute one", it is
> that a plugin's `side:` lives in its stamped schema document, which the Keeper reads at dispatch
> and a **scenario cannot read at all**. So a plugin address, unlike a core one, never announces its
> own side to the linter or the render pipeline, and the errors of (b) remain core-address errors
> only. A module declaring `soul` on a keeper-side address is refused by name at dispatch — the
> silent-skip prohibition below is honoured, not relaxed.

`on: keeper` on a **plugin** address stays **legal** until a separate ticket. The keeper cannot
execute a keeper-side plugin at all (NIM-688), and a plugin has nowhere to declare its side before
this ADR ships. The errors of (b) are **core-address errors only**.

The matching half has to be stated as plainly: **`side: keeper` on a plugin will be honoured by
nothing.** `applyKeeperTask` looks up `r.keeperModules`, a `coremod.Registry` and nothing else
([`keeper/internal/scenario/keeper_dispatch.go`](../../keeper/internal/scenario/keeper_dispatch.go)),
so such a step still dies `unknown keeper-side module` — loudly, exactly as today. The field is
therefore **accepted and inert for plugins until NIM-688 lands the executor, and the failure stays
loud rather than becoming a silent skip.**

This matters beyond bookkeeping. An unenforced declaration that reads like a control is precisely
the defect [ADR-020](0020-plugin-infrastructure.md)'s 2026-08-06 amendment spent three paragraphs
undoing for `side_effects` / `required_capabilities`, and ADR-0087 must not re-ship it under a new
name.

## Consequences

### (g) The blocker — the Passage stratifier, and it is not a validator

`onTargetsRoster` is literally `on == nil`
([`shared/config/passage_refresh.go:240-242`](../../shared/config/passage_refresh.go)).

Drop `on: keeper` and every keeper task's `On` becomes nil. `taskReadsRoster`
([`:193-194`](../../shared/config/passage_refresh.go)) therefore turns **true** for it, and
`Stratify`'s roster edge ([`shared/config/passage.go:247-265`](../../shared/config/passage.go), with
`readsRoster` filled at [`:174-178`](../../shared/config/passage.go)) pushes every keeper task
standing after a `refresh_soulprint` emitter into a **later Passage**.

**The affected set is narrower than the 171 lines, and the count is not the point.** The roster edge
is drawn only where a plan has a refresh emitter at all — the fill above is gated on
`if len(refreshEmitters) > 0` — and then only for tasks standing **after** one
([`shared/config/passage.go:254`](../../shared/config/passage.go), `j >= i → break`). In `examples/` exactly four trees carry a `refresh_soulprint` at all — `redis`,
`dragonfly`, `example-cloud-bootstrap`, `create-roster-guard` — so what re-stratifies today is the
keeper tasks standing after an emitter inside those four.

**The hazard is silence, not volume.** Whatever the count, the change moves Passage boundaries with
**no diagnostic anywhere**: more Passages, more `apply_runs` rows, different register-visibility
windows, and nothing in the output saying the plan was re-cut. That is a blocker at any count — one
silently re-stratified scenario is already a wrong run nobody can see. Worse, the
refresh emitter itself — `core.soul.registered`, keeper-side
([`shared/config/passage_refresh.go:86`](../../shared/config/passage_refresh.go)) — would become a
roster **consumer** of any earlier emitter, which the code comment at
[`:190-192`](../../shared/config/passage_refresh.go) explicitly says must not happen.

**Therefore `onTargetsRoster` must become side-derived in the same move:** *an omitted `on:` is the
whole roster only for a Soul-side task.* Pinned by a test asserting Passage counts on the existing
fixtures.

This does not contradict [ADR-056](0056-staged-render-passage.md) or
[ADR-0061](0061-onboarding-await-and-midrun-reresolve.md) — it silently changes what they produce,
which is worse than contradicting them. It is a **requirement on NIM-749/750**, not an optional
follow-up.

### (h) The `when:` story — the true version, which is stronger than "the predicate is ignored"

A dynamic `when:` on a keeper-side task is **not** ignored today. It is half-covered, and the
uncovered half is worse than being ignored.

**Covered.** A dynamic `when:` written **with** `on: keeper` is refused offline
([`shared/config/scenario_task.go:1364-1385`](../../shared/config/scenario_task.go),
`when_on_keeper_dynamic_unsupported`) and at render (`guardKeeperWhen`,
[`keeper/internal/render/pipeline.go:1753`](../../keeper/internal/render/pipeline.go)).

**Not covered.** A dynamic `when:` on a keeper-side-module task that **omits** `on: keeper`:

- Offline, `validateWhenOnKeeper` is reached only from the `present["on"]` branch
  ([`shared/config/scenario_task.go:707-711`](../../shared/config/scenario_task.go)), so with the key
  absent it never runs. For `core.state.<verb>` a different validator catches the file
  (`state_capture_not_on_keeper`, [`:1404-1424`](../../shared/config/scenario_task.go) — the one this
  decision deletes), and **for the other six keeper-side bases nothing fires offline at all.**
- At render `IsKeeperTask` is false, so the task takes the Soul-side path and the predicate rides the
  `RenderedTask` to the agent — where it is evaluated **before** the module lookup: `evalWhen` at
  [`soul/internal/runtime/applyrunner.go:522`](../../soul/internal/runtime/applyrunner.go), the skip
  at [`:559-566`](../../soul/internal/runtime/applyrunner.go), and the lookup that would have raised
  `module.not_found` only at [`:998`](../../soul/internal/runtime/applyrunner.go).

So `when` **true** gives a loud failure. But **`when` false on every host gives SKIPPED everywhere,
a GREEN run, and the cloud-create / soul-register / bootstrap-delivery silently never happened.** The
predicate is honoured by the wrong evaluator, on hosts the step was never meant to touch, and when it
comes out false the run is green with the keeper-side effect absent and no error anywhere.

Under derivation the shape is unreachable — the address routes the step keeper-side whether or not
the author wrote a targeting key, so the predicate meets `guardKeeperWhen` instead of a host.

**The reinforcing fact: the tree already derives from the address in three places while the router
derives from `on:`.**

- `stateCaptureVerb` ([`shared/config/state_capture_order.go:229-238`](../../shared/config/state_capture_order.go))
- the Passage stratifier's state and refresh axes
  ([`shared/config/passage_refresh.go:295`](../../shared/config/passage_refresh.go),
  [`:86`](../../shared/config/passage_refresh.go))
- the L0 trial's state fold ([`keeper/internal/trial/harness.go:294-316`](../../keeper/internal/trial/harness.go),
  where `stateop.OpsFromPlan` keys the captures off the plan's module addresses)

And [`docs/naming-rules.md`](../naming-rules.md) already records the consequence in so many words,
in its entry for **`state_capture_not_on_keeper`** (cited by code name rather than by line, because
this commit inserts a row above it): *"the L0 trial folds a capture by its module ADDRESS — a task
missing `on:` predicts
`state_after` exactly as a routed one does, so the case goes green on a plan the run cannot
execute."* **The decision removes a divergence the tree already documents.**

### (i) What happens to the six validators

| validator | fate |
|---|---|
| `validateStateVerbOnKeeper` → `state_capture_not_on_keeper` ([`:1404`](../../shared/config/scenario_task.go)) | **deleted** — it exists only to force the key |
| `validateBlockOnKeeper` ([`:1321`](../../shared/config/scenario_task.go)) | **deleted** |
| `validateBlockChildOnKeeper` ([`:1033`](../../shared/config/scenario_task.go)) | **survives with a new trigger**, keeping the code `block_on_keeper_invalid` |
| `validateAsyncOnKeeper` ([`:1286`](../../shared/config/scenario_task.go)) | survives, relocated |
| `validateWhenOnKeeper` ([`:1364`](../../shared/config/scenario_task.go)) | survives, relocated |
| `validateOnField` → `enum_invalid` ([`:1465`](../../shared/config/scenario_task.go), the check at [`:1473`](../../shared/config/scenario_task.go)) | **survives unchanged** |

`validateOnField` is the `on:`-**shape** validator, not a side validator: it enforces *"only 'keeper'
is allowed as scalar; use a sequence of coven-ids otherwise"*. It must survive **exactly as written**,
because under (f) the scalar stays grammatically legal on a plugin address, permanently — tightening
it to reject every scalar would break the one form this decision deliberately leaves standing. The
new codes of (b) refuse the scalar on a *core* address; `validateOnField` keeps refusing every scalar
that is not `keeper`, on any address. The two do not overlap.

Deleting `validateBlockOnKeeper` is a genuine improvement rather than a loss of coverage: a `block:`
has no `module:`, hence no address, so "keeper-side inside a block" becomes **inexpressible** rather
than refused.

`validateBlockChildOnKeeper` keeps its code and changes its trigger — a keeper-side **address**
inside a block. Without it the child is fanned over hosts and dies `module.not_found`.

The two survivors relocate from the `present["on"]` branch
([`shared/config/scenario_task.go:707-711`](../../shared/config/scenario_task.go)) to the
`present["module"]` branch ([`:656-658`](../../shared/config/scenario_task.go)).

**One new code the framing above does not reach: `keeper_module_in_destiny`.** `renderKeeperTask`
exists only on the scenario path
([`keeper/internal/render/pipeline.go:1093`](../../keeper/internal/render/pipeline.go)), and a
destiny task is Soul-side by construction. The deleted `state_capture_not_on_keeper` was carrying
that duty for `core.state` — its own doc comment at
[`:1387-1403`](../../shared/config/scenario_task.go) says so — and its destiny half must be replaced
and **generalised to all keeper-side bases**. Miss it and `core.cloud.created` in a destiny reaches
a host silently.

### (j) The break, stated without softening — there is no transition window

171 lines go from legal to a **load-time validation error in one release**: here, and in every
external service repository (`wb-service-redis`, customer forks). The YAML still parses — the
diagnostics of (b) are raised at `PhaseSchemaValidate` / `PhaseSemanticValidate`, the phases the
`on:`-keyed validators already use — but the definition no longer loads, which for the operator is
the same wall.

A service is loaded at a **pinned `ref:`** ([ADR-007](0007-versioning-git-ref.md)), so an old
repository is not edited before a new keeper reads it. **An unswept fork stops loading.** The core
tree's 171 lines are swept in the same release; external forks break and must be edited.

This is deliberately a **different axis** from [ADR-0076](0076-engine-compat-window.md)'s
engine-compat window, and the two must not be conflated —
[ADR-0085](0085-entity-id-and-label.md) warns against exactly that conflation for its own one-off
spelling transition. ADR-0076 declares, per entity, the range of keeper versions that can execute a
definition. This is a grammar change with no per-entity declaration and no pin.

A warn-then-error window — the [ADR-0085](0085-entity-id-and-label.md) shape — **was offered and
declined by the user on 2026-09-01.** Recorded here so that it reads as a decision rather than as an
oversight.

### (k) Rollout order for `side:` — souls first, then keeper, then re-stamp

Schema-document decoding is **strict**: `dec.DisallowUnknownFields()` at
[`sdk/schema/canonical.go:62`](../../sdk/schema/canonical.go), whose own doc comment already names
the consequence. A document carrying `side:` therefore **fails to parse on an older `soul`**, and
`soul` reads the document at install time
([`soul/internal/coremod/module/installed.go:78`](../../soul/internal/coremod/module/installed.go)).

Order: **souls first, then keeper, then re-stamp artifacts** — the same order
[ADR-0076(s)](0076-engine-compat-window.md) already imposes for adding a parameter.

The rest of the blast radius, compactly, and all of it additive except the older-soul parse:

- `sdk/module.Def` / `Bundle.Document()`; `soul-mod stamp` / `soul-mod verify`.
- The **Sigil** signature and `plugin_sigils.schema`: a re-stamp means a new sha256 and therefore a
  fresh `plugin.allow` entry. Already-approved artifacts keep their old bytes and their approval.
- [`keeper/internal/sigil/lookup.go:44`](../../keeper/internal/sigil/lookup.go).
- [`keeper/internal/api/handlers/modulecatalog.go`](../../keeper/internal/api/handlers/modulecatalog.go)
  → `GET /v1/modules` → [`keeper/openapi.yaml`](../keeper/openapi.yaml) → the companion
  `soul-stack-web`. ⚠ **That catalog item already carries a `kind: "core"|"plugin"` field**
  (`modulecatalog.go:157`) meaning something else entirely; the two must not be confused, in the
  code or in the UI.
- [`soul-lint/internal/validate/modules.go:94`](../../soul-lint/internal/validate/modules.go) — the
  `for _, m := range doc.Modules` fold that keys each module object by `alias + "." + m.Name`, which
  is where a new per-module field arrives.
- The companion `soul-stack-plugins`, every artifact of which needs re-stamp + re-sign + re-allow.

### (l) Naming friction — `side` and `side_effects` sit in the same object

`side:` lands in the same per-module object as `side_effects:`, and "side" is also the prose word in
"keeper-side". Disambiguated once, where the field is documented:

> **`side` is where the module runs; `side_effects` is what it touches.**

## Rejected

- **`runs_on:` as the field name.** It echoes the very task key being retired, so a reader would take
  it for the same key relocated into the manifest — which is the one inference this decision most
  needs to prevent. `side` is the word the repository already uses everywhere ("keeper-side module",
  "Soul-side core"). Chosen by the user on 2026-08-31; the propose-and-wait on the name is closed.
- **A `Document`-level `side`.** It forecloses a mixed bundle (one artifact serving both a keeper-side
  and a Soul-side module), and it lands on the three kinds that carry no `modules[]` at all —
  `cloud_driver`, `ssh_provider`, `soul_beacon` — where the field has no meaning to hold. See (e).
- **A warn-then-error transition window.** Offered, declined by the user on 2026-09-01. See (j).
- **Recording this as amendments inside ADR-009 and ADR-017 rather than as its own ADR.** ADR-017 was
  already 148 lines of sixteen amendments before this ADR appended a seventeenth, and a reader of
  ADR-009 alone would still get `on:` wrong. The repository's own precedent for a cross-cutting reversal is a new ADR that amends N
  others — [ADR-0084](0084-explicit-state-capture.md) amends four,
  [ADR-0085](0085-entity-id-and-label.md) amends sixteen.
