# ADR-079. Composed incarnation name — `name_template` in the create scenario

- **Status.** Active. Backend landed by NIM-177 (schema key, soul-lint checks,
  server-side composition shared by REST and MCP). The web half — hiding the
  free-text "Name" field and drawing a live preview of the composed name — is a
  separate ticket.

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
  small DevOps-shaped key like `required_when` or `state_changes`, not a new concept
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
  ([`name_template.go`](../../shared/config/name_template.go)):
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
  - **RBAC, known limitation.** `POST /v1/incarnations` scopes the permission check
    from the request body before the handler runs
    ([`IncarnationCreateScopeSelector`](../../keeper/internal/api/handlers/incarnation.go)),
    and a request with no `name` yields an empty context set — which
    `RequirePermissionMulti` admits only for bare/`*` roles. MCP behaves the same way
    (`Check` with a nil context). So a *scoped* operator cannot create a
    templated incarnation on either surface; the direction is fail-closed (it can
    only refuse, never over-grant), and the escalation path is closed by (e), but the
    functional hole is real. Scoping a create whose name is not known until the
    service snapshot resolves needs its own decision and is deliberately left out of
    this ADR.
  - The 63-character ceiling becomes a design constraint on service authors: a
    template with four components leaves roughly 15 characters per component. The
    linter catches the impossible cases; the tight ones surface as a 422 at create,
    which the create form should pre-empt with a live preview and a character count.
