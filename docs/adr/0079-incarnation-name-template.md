# ADR-079. Composed incarnation id — the `id:` block in the create scenario

- **Status.** Active, amended 2026-07-30, 2026-09-04 and 2026-09-24. Backend landed by NIM-177 (schema key,
  soul-lint checks, server-side composition shared by REST and MCP). The
  **2026-07-30 amendment** (NIM-333) resolves the RBAC limitation this ADR had
  deliberately deferred: a scoped operator could not create a templated incarnation
  at all, because the gate scoped on `incarnation=` — the one dimension a template
  does not have yet — and discarded `service=`/`coven=`, which the request does
  carry. See the RBAC bullet under (f). The web half — hiding the free-text "Name"
  field and drawing a live preview of the composed name — landed by NIM-331, over
  the resolve endpoint described under (g).

- **Context.** The incarnation name is typed by the operator as free text. In a
  fleet of similar instances that text is not actually free: it follows a house
  convention like `{name}-{project}-{subproject}-redis-{service_type}`, assembled by
  hand every time. Hand-assembly drifts — a missing segment, a different separator,
  a typo in the project — and the name is **immutable**: it is the `TEXT PRIMARY KEY`
  of the `incarnation` table ([migration 005](../../keeper/migrations/005_create_incarnation.up.sql)),
  globally unique, with no rename operation. Fixing a mistyped name means destroy
  and recreate.

  Two facts make this cheap to solve properly. First, the name is no longer a
  targeting label: [ADR-008](0008-coven-stable-tags.md) made `incarnation.name` the
  root Coven tag, and [NIM-124](0008-coven-stable-tags.md) superseded that part —
  the name is not injected into the Soulprint and `on: ["${incarnation.name}"]` is
  rejected. Composing it therefore ripples nowhere near targeting; only membership
  FKs, the RBAC `incarnation=<name>` dimension, URLs and audit read the final
  string. Second, at the moment of create the keeper already holds everything a
  composition needs — the resolved `input:` and the service snapshot — **before** the
  row is inserted ([`ResolveCreatePlan`](../../keeper/internal/scenario/create_scenarios.go)),
  so no new run phase and no DB migration are involved.

- **Decision.** A create scenario may declare a top-level **`name_template`**: a
  `${ … }` template over its own `input:` components. When present, the keeper
  composes the incarnation name from the resolved input and the request must not
  carry a `name` of its own.

  **(a) Composition is server-side.** The template is rendered by the keeper in
  `ResolveCreatePlan`, on the shared path both `POST /v1/incarnations` and
  `keeper.incarnation.create` already take. Not client-side: the naming convention
  then lives in git next to the scenario that owns it, one implementation, and a
  client cannot route around it. The composed name is what gets inserted, run,
  audited and echoed back.

  **(b) The template lives in the create `scenario.yml`, top-level, next to
  `input:`.** It is a property of the operation that creates instances of this
  service, so it belongs with that operation's input contract, versioned by the same
  git ref ([ADR-007](0007-versioning-git-ref.md)). `compute:` is not a candidate:
  it resolves RUN-LEVEL, i.e. after the row exists.

  **(c) The key is named `name_template` — plain, not a dictionary entity.** It is a
  small DevOps-shaped key like `required_when` or `changed_when`, not a new concept
  in the Soul Stack vocabulary ([naming-rules](../naming-rules.md)); nothing is added
  to the dictionary beyond the key itself.

  **(d) The sandbox is input-only.** Every `${ … }` block compiles against an env
  declaring the single variable `input` — the same narrow cel-go sandbox as
  `required_when` and `validate:`
  ([`input_required_when.go`](../../shared/config/input_required_when.go)).
  Referencing `essence` / `soulprint` / `register` / `vault()` / `now()` is an
  undeclared-reference compile error: the barrier is structural, like migration-CEL
  in [ADR-019](0019-state-migration-dsl.md). A name must be a pure function of the
  operator's input, reproducible from the request alone.

  Unlike general interpolation ([ADR-010 §5(a)](0010-templating.md)), a single block
  does **not** yield a native type here — a name is a string, so every block is
  stringified and concatenated with the surrounding literal text. A block evaluating
  to a list or map is an error, not a stringified container.

  **(e) `name` becomes optional in the API, and mutually exclusive with a
  template.** `IncarnationCreateRequest.name` lost `required: true`; the domain
  still answers 422 `field 'name' is required` when nothing composes one. Sending
  `name` against a composing scenario is 422 `name_not_composable` — **not** silently
  ignored and **not** an override. Ignoring it would let the RBAC
  `incarnation=<name>` dimension be checked against one name while a different one is
  inserted; overriding it would defeat the convention the template exists to enforce.
  With the rule, the request name and the row name can never disagree.

  **(f) Overflow refuses, it never truncates.** The composed string is validated
  against `incarnation.NamePattern` (`^[a-z0-9][a-z0-9-]{0,62}$`) before the insert.
  Four components plus the template's literal text pass 63 characters easily, so this
  is the failure operators will actually hit: it answers 422
  `composed_name_invalid` quoting the composed string and its length, so the operator
  knows which component to shorten. Truncating to fit would silently create a
  *different* identity under a name nobody asked for — and the name is immutable, so
  the mistake is not repairable in place.

  **(g) Name components are write-once identity.** The composition runs once, on the
  create path. Nothing renames an incarnation afterwards: a later run passing a
  different `project` changes what that run does, not what the incarnation is called
  (`incarnation.spec.input` is written at create and never rewritten). Components
  that feed the name are therefore part of the instance's identity, not editable
  settings — a distinction the create form must make visible, because "I changed the
  project and nothing was renamed" is otherwise an operator trap.

  **(h) soul-lint catches the class statically.** In the schema phase
  ([`id_template.go`](../../shared/config/id_template.go)):
  `name_template_input_unknown` (ERROR) for a `${input.X}` with X undeclared in
  `input:` — modelled on the existing `form_field_unknown`, since such a template
  fails for *every* operator; `name_template_invalid` (ERROR) for a block outside the
  input sandbox or written in index form `input['x']` (which hides the component name
  from static analysis, mirroring `vars[...]` in
  [`shared/cel/varrefs.go`](../../shared/cel/varrefs.go)); `name_template_too_long`
  (ERROR) when the literal skeleton alone exceeds 63 characters, which no input can
  rescue; `name_template_constant` (WARNING) for a template with no block at all,
  which would collide on the second create; `name_template_ignored` (WARNING) on a
  scenario that is not a create starter, where the key is dead config.

  Under `extends:` ([covenant fragments](0009-scenario-dsl.md)) the reference check
  is gated exactly like `form:` — the effective input exists only after the merge, so
  it runs post-merge in `ResolveScenarioCovenant`; checking it earlier would report a
  false `name_template_input_unknown` for a component the covenant declares.

- **Consequences.**

  - Fully opt-in. A service without `name_template` behaves bit-for-bit as before:
    free-text `name`, same validation, same errors.
  - REST and MCP cannot drift: both read `CreatePlan.ComposedName` from the same
    `ResolveCreatePlan`, and `CreatePlan.EffectiveName` is the single place that
    decides which name wins.
  - **RBAC — RESOLVED (Amendment 2026-07-30, NIM-333).** As first written, this ADR
    left a functional hole: `POST /v1/incarnations` scopes the permission check from
    the request body before the handler runs
    ([`IncarnationCreateScopeSelector`](../../keeper/internal/api/handlers/incarnation.go)),
    a request with no `name` yielded an empty context set, and
    `RequirePermissionMulti` admits that only for bare/`*` roles — MCP the same, via
    `Check` with a nil context. So a **scoped operator could not create a templated
    incarnation on either surface**, and there was no fallback: passing `name`
    explicitly is refused by `ErrNameNotComposable` under (b). The direction was
    fail-closed, but with templating becoming the primary path it made creation the
    privilege of an unrestricted role, which is the opposite of what scoping is for.

    The resolution needed no new grammar and relaxed nothing. The gate was
    **discarding two dimensions the request already carries**: `service` is
    `required` in the schema and `covens` are declared, and both are dimensions of
    the scope grammar ([ADR-047](0047-purview.md) `coven | service | incarnation |
    host | trait`). They are also **ceiling-checked by construction** — the declared
    values go INTO the context and the role's predicate is evaluated against them, so
    declaring a coven outside your scope makes the check *fail*. A caller cannot widen
    their reach by claiming more, which is why no separate "compare the claim against
    the ceiling" step is needed at this gate. An absent dimension is **omitted**
    rather than sent empty: `evalCond` reads a missing dimension as "no candidate", so
    an omitted key denies a role scoped on it.

    A **second gate** covers what the first cannot answer. Once `ResolveCreatePlan`
    has composed the name,
    [`ScreenIncarnationCreateScope`](../../keeper/internal/api/handlers/incarnation_create_scope.go)
    re-asks with the full context, as an **AND over every declared coven** —
    all-or-nothing, no silent trim, the shape [ADR-049(f)](0049-synod.md)-adjacent
    bulk gates settled on. That matters because gate (a) is an OR: a request naming
    one coven the caller holds and one it does not matches on the first. It also
    measures the composed name — derived from operator `input`, and the identity every
    `incarnation=` scope is written against — against the caller's ceiling rather than
    trusting it. Both gates read the same
    predicate over contexts from one builder, so there is no second notion of scope
    ([NIM-219](0047-purview.md)); fail-closed when no checker is wired.

    **What is still refused, deliberately:** a role scoped ONLY by `incarnation=`
    cannot create a templated incarnation. That dimension is unanswerable before
    composition, so gate (a) denies before the name exists and gate (b) is never
    reached. Fail-closed and documented rather than discovered live.

    **Gate (b) runs on every create, named ones included.** Gate (a)'s OR admits a
    superset: a request declaring one coven the caller holds and one it does not
    matched on the first and was created carrying both — minting an object inside a
    scope the caller does not hold, readable and runnable by every role scoped to that
    coven, with its service vars resolved through that coven's overlay
    ([ADR-0082](0082-service-vars.md)). That is an escalation, it predates templating, and a named
    create has always been able to do it. Closing it tightens named create too — a
    deliberate decision taken with the templated fix rather than after it, since the
    two are one gate.
  - The 63-character ceiling becomes a design constraint on service authors: a
    template with four components leaves roughly 15 characters per component. The
    linter catches the impossible cases; the tight ones surface as a 422 at create,
    which the create form should pre-empt with a live preview and a character count.

- **(g) The preview is a RESOLVE, not a second implementation (Amendment
  2026-07-30, NIM-331).** The bullet above asks the create form to pre-empt the
  ceiling with a live preview, which raises the obvious question of where the
  composition runs. It runs **only on the server**:
  [`POST /v1/incarnations/resolve-name`](../keeper/openapi.yaml) takes
  `service` + `create_scenario` + the `input` so far and answers with the composed
  name, its length against the ceiling, whether it is a legal name, and whether it
  is free. Nothing is created or stored; the create still re-validates everything.

  Composing client-side was the tempting shape and is the one thing this ADR
  forbids. Template blocks are CEL, stringified by cel-go's own coercion rules
  (`nameBlockString`); a JavaScript evaluator would round a number or spell a bool
  differently and compose a **different string from the same input**. Under an
  immutable primary key that is not a cosmetic mismatch — the operator approves one
  identity and is handed another, silently, which is the same failure mode this ADR
  refuses truncation over. So the endpoint calls
  [`scenario.ComposeName`](../../keeper/internal/scenario/create_scenarios.go), the
  function the create path itself reaches through `composeIncarnationName`, and the
  agreement is pinned by a guard test rather than by inspection. The form
  reimplements nothing about composition; it duplicates only what is not
  composition — a character count, and the name pattern it already receives.

  **One deliberate asymmetry.** A preview runs while the operator is still typing,
  so it merges schema defaults *without* the required/`validate:` phases —
  otherwise every keystroke would be rejected before a name could exist. Merge is
  also the only phase that CHANGES a value (require and validate merely reject), so
  whenever the create would have been accepted, both paths compose over the same map.
  The preview therefore rejects *less*, never *differently*.

  **Occupancy is answered here, deliberately.** The form must not probe
  `GET /v1/incarnations/{id}`: that turns the status code into an existence oracle
  and lets a scoped operator walk names outside their scope. The reply is scope-aware
  in two grains — "taken" for anyone who could create the name, "taken by service X"
  only for a caller who may already see that incarnation — and the same rule now
  phrases the create's 409, which under a template otherwise names a string the
  operator never typed. The endpoint carries permission `incarnation.create` and
  re-measures the **composed** name against the caller's scope (gate (b) above);
  without that it would compose arbitrary names and report their availability, which
  is the oracle again by another door.

  Scenario listing gained a **boolean** `composes_name` next to `input_schema`, so
  the form knows which mode to open in. A flag, not the template text: the operator
  is shown the resulting name rather than the formula (NIM-340), and a client
  holding the expression is one step from evaluating it — the divergence above.


## Amendment 2026-09-04 (NIM-730): the key is `id_template:`, and every name in this ADR is an id

[ADR-0085](0085-entity-id-and-label.md) spells a registry entity's identifier `id`,
and this whole family followed it. **Everything below the rename is unchanged** —
the input-only sandbox, composition before the insert on the shared
`ResolveCreatePlan` path, the refusal to truncate, write-once identity, the
post-merge gate under `extends:`. What moved is spelling, and only spelling. Read
this ADR with `name` → `id` throughout; the file keeps its slug because links
point at it.

| was | is |
|---|---|
| `name_template:` (scenario key) | **`id_template:`** |
| `incarnation.name` (CEL root) | **`incarnation.id`** |
| `composes_name` (scenario listing) | **`composes_id`** |
| 422 `name_not_composable` | **`id_not_composable`** |
| 422 `composed_name_invalid` | **`composed_id_invalid`** |
| `POST /v1/incarnations/resolve-name` | **`POST /v1/incarnations/resolve-id`**, reply field `composed_name` → `composed_id` |
| soul-lint `name_template_*` | **`id_template_*`** (same five rules, same levels) |
| `config.RenderNameTemplate` / `scenario.ComposeName` | **`RenderIDTemplate`** / **`ComposeID`** |

**A compatibility window covers the two spellings a service repository writes**,
and only those two — the scenario key and the CEL root. Both are read; the old
one is reported by `soul-lint` with a file, a line and the replacement
(`id_template_legacy_spelling` for the key, `incarnation_name_legacy_root` for
the root), and both are WARNINGs, because an error would close the window it
exists to keep open. Declaring both spellings of the key in one file is an
ERROR (`id_template_conflict`): two templates composing one id is an authoring
mistake whichever value a loader picked. Dropping the old spellings is a separate
ticket, after service repositories have moved.

**The API rename has no window and needs none.** A caller of
`/v1/incarnations/resolve-name` or a reader of `composes_name` is a client of this
cluster's own API, versioned and shipped with it — not a file in a repository
nobody here can see. That is the same line NIM-729 drew across the other ten
registries.

**Why the root's warning is load-bearing.** `incarnation` is `cel.DynType` in all
three CEL environments, so dropping the old key at the end of the window is a
`no such key` at EVALUATION, not a compile error — and one of the three
environments is flow-control, evaluated on the host, where a stale
`when: incarnation.name == …` fails mid-run after earlier tasks have applied.
`soul-lint` is the only static catcher on either side of the wire
([ADR-0085](0085-entity-id-and-label.md) §"The stale CEL root is a runtime failure,
not a compile error").


## Amendment 2026-09-24 (NIM-899): the key is the block `id:`, and it carries the bounds of the composed id

The scalar became a block — `id: {template, max_length}` — because a composed id needs a
**ceiling** as much as it needs a formula, and a scalar key has nowhere to put one.

**What the scalar could not express.** A service can bound its own inputs: the WB redis
service caps `uniq_name` at 30 characters. It cannot bound the id, because the id's length
also depends on `namespace`, which nothing above caps, and on the literal text between the
components. The only thing that caught an overrun was a hand-written render-time `assert:`
in one service, and it was wrong in three ways at once: it is rewritten in every service
that needs it; it fires **at render**, after the incarnation row is already committed,
where the remedy is a destroy and a re-create; and it asserts on a **derived** string (a
machine name) rather than on the id, because "my id must fit in 50" was not sayable.

- **(i) `max_length:` is checked on the REQUEST, by the same code the preview calls.**
  [`scenario.ComposeID`](../../keeper/internal/scenario/create_scenarios.go) measures the
  composed string against the effective ceiling before `incarnation.Create`, so the answer
  is a **422 `composed_id_invalid`** naming the id, its length and *whose* ceiling was hit.
  That last part is not decoration: it decides which file the reader opens, since
  `id.max_length` is theirs to change and the platform's 63 is not. The live preview
  (`POST /v1/incarnations/resolve-id`) reports the same number in `max_length`, so the
  form's character counter divides by what the create will actually enforce rather than by
  a copy of 63 kept client-side — the precise drift this key exists to remove.

  **It is consulted ONLY where an id is composed, which is the create path.** An incarnation
  created before a bound existed — or before this key did, under the platform's 63 — re-runs
  its scenarios with an id nothing static ever measured. A service whose real limit is a
  derived string therefore keeps asserting on that string at render, and the two are not
  duplicates: the block moves the check earlier for every id composed from now on, and the
  assert remains the only catcher for the ones that were not. That is why the WB redis
  service keeps its machine-name assert unchanged by this amendment.

- **(ii) The ceiling is an ABSOLUTE number, not a reserve, and deriving it is the
  SERVICE's work.** `reserve: 9` subtracted from the platform's 63 would have yielded 54,
  and the number a real service needs is nothing like that: the WB redis service is bounded
  by its cloud refusing a machine name over 50, and a machine name is the id plus a suffix
  its provisioning plugin appends (`<id>-<tail>`, a five-character tail: id + 6). The
  arithmetic lands on 44, so the reserve form would have passed a 45-character id
  **silently**.

  That derivation is the whole argument for an absolute number. It is arithmetic over a
  limit living outside the platform, in a plugin and a cloud API the engine cannot see, and
  it differs per topology — a clustered machine name carries a group prefix and number as
  well (id + 7 + prefix + digits, so 41 at 3–9 shards and 40 at 10–99). A single static
  bound must therefore be the LOOSEST of the topologies or it rejects legal configurations,
  and the remainder stays with a render-time assert that can see the input those summands
  come from. A reserve computed from 63 could express none of this, and it would have hidden
  the arithmetic in the engine instead of writing it down in the service that owns the
  constraint. The engine supplies only the invariant that a bound can never widen:
  `Ceiling()` is `min(max_length, 63)`, because the grammar stops at 63 whatever a manifest
  says.

  > ⚠ **`max_length` ships with NO declaring service, and that is worth stating rather than
  > leaving to be discovered.** The case above is redis's, and it dissolved while this ticket
  > was in flight: the machine name derives from a `name` PARAMETER the service chooses, not
  > from the id, so once that parameter stops carrying the namespace the machine-name limit
  > becomes a limit on ONE input field — which `input:` has always been able to cap. redis
  > therefore declares `id: {template}` and no ceiling, and nothing else in any repository
  > declares one either.
  >
  > It ships anyway, for one reason and not the other. **Not** because a key might be useful
  > later — that is the argument this ADR refuses in (v) below. Because the bound is BUILT,
  > tested and gated, and a schema key costs in the carrying rather than in the having:
  > removing it now is work whose only product is a smaller engine, while keeping it costs
  > an author one line of documentation saying "you probably do not need this". The first
  > service whose id feeds something it does not control then needs no engine change.
  >
  > The asymmetry with `min_length` is deliberate and is exactly that: `min_length` was never
  > built and no case for it was even imaginable, so it never reached the point where removal
  > costs more than retention.

- **(iii) A block rather than sibling scalars (`id_max_length:`), because the parameters
  keep arriving.** The next one is `pattern:` — the grammar it would narrow already exists
  (`^[a-z0-9][a-z0-9-]{0,62}$`) and redis already needs a narrower one, which is why the
  render-time assert above is not deleted by this amendment but **trimmed** to what remains
  about the machine: two grammar edges the platform permits and the cloud does not, and a
  length budget whose summands (`shards`, a group prefix) arrive in the input and cannot be
  a static number. A block also puts the bound next to the template it bounds, which is the
  only place an author reads the two together.

- **(iv) `id_template_too_long` sharpens instead of gaining a sibling.** The static rule
  already compared the template's literal skeleton against a ceiling; it now compares it
  against the **effective** one. A 45-character skeleton under `max_length: 40` is reported
  offline, where before there was only 63 to compare with and the linter stayed silent. Two
  new ERRORs guard the bound itself — `id_max_length_over_ceiling` (above 63: narrows
  nothing while reading as though it did) and `id_max_length_invalid` (below 1: bounds no
  id at all, and `0` is indistinguishable from *unset* in the decoded struct, so the rule
  reads the AST). An `id:` block with no `template:` is `empty_value`: a ceiling on nothing.
  Both bound rules are refusals rather than silent clamps — the number is in the file to be
  read by a human, and a clamped one would be read as true. At **runtime** an out-of-range
  bound falls back to the platform ceiling instead: refusing every create over a typo in a
  bound is worse than enforcing the bound that applies anyway.

- **(v) `min_length` was specified and is NOT built.** It was in the ticket for symmetry,
  and no motivating case turned up on review: the platform grammar admits a one-character
  id, nothing downstream needs a floor, and the components that feed an id already carry
  their own `min_length`. An unused key in a schema is worse than an absent one — it has to
  be documented, linted, guarded and explained — and adding it later is purely additive,
  since *absent* already means *no floor*. So it is left out, deliberately and on the
  record.

**Two windows, and only one of them is still open.** The scalar `id_template:` is read for
a compatibility window and folded into `id.template` once, at the top of
`schemaValidateScenario`, so nothing downstream learns which spelling the file used; it
warns `id_template_legacy_spelling` with a line and the replacement, and carries **no**
bound — a scalar has nowhere to put one, so such a scenario gets the platform ceiling and
nothing narrower. Declaring both spellings is `id_template_conflict`, unchanged in form
from the previous window. The pre-[ADR-0085](0085-entity-id-and-label.md)
`name_template:` **is no longer read at all**: its window had zero users — no occurrence in
any service repository or in `examples/` — and keeping it open beside the block would have
left three spellings of one key and a reader for the third. It answers `unknown_key` with
the replacement in the hint.

Everything else in this ADR is untouched: the input-only sandbox, composition before the
insert on the shared `ResolveCreatePlan` path, the refusal to truncate, write-once identity,
and the post-merge gate under `extends:` — which is also where the bound rules run, because
that gate is all-or-nothing per scenario and the services writing this key are exactly the
ones that use `extends:`.
