# ADR-078. Derived roles — `parent_role`, attenuation and cascade

- **Status.** Active, amended 2026-07-27. J1 (NIM-179) landed the model, storage
  and graph guards; J2 (NIM-180) landed chain resolution, the write-time
  attenuation gate and the self-lockout correction of §(i); J3 (NIM-181) landed
  the API surface. The web selector is NIM-182. The **2026-07-27 amendment**
  (NIM-198 / NIM-199 / NIM-200) corrects §(h) to measure the child in its resolved
  form, and adds §(k) `scope_mode` + the cascade report and §(l) inert rows.

- **Context.** Roles are flat. An operator who runs the `dba` team can already be
  scoped — `default_scope: coven=dba` ([ADR-047 §a](0047-purview.md)) — but there is no way
  to say "the same rights as `dba`, only for project *aboba*". Today that is done by
  hand: copy the role, copy its permissions, add a narrower scope. The copy is a
  snapshot, not a relationship — when the parent role's scope moves from `coven=dba`
  to `coven=dbaas`, every hand-made variant silently keeps pointing at the old
  coven, and nothing in the system knows they were supposed to follow.

  The anti-escalation half of the problem is already solved and must not be
  disturbed. [`subset.go`](../../keeper/internal/rbac/subset.go) enforces
  "you cannot grant a permission you do not hold yourself" inside every role
  mutation's transaction, reading the caller's rights from the DB in that same
  transaction and filtering revoked operators; the scope grammar has no `NOT`, so
  it is monotone and adding a predicate can only narrow. What is missing is purely
  the **relationship**: a persistent link from a role to the role it was derived
  from, so that narrowing the parent narrows the children by construction rather
  than by an operator remembering to.

- **Decision.** A role may name another role in **`rbac_roles.parent_role`**. The
  named role is its **parent**; the naming role is **derived** from it. A role with
  no parent is a **plain** role — what every role is today.

  **(a) No new entity.** A derived role is a role, not a new kind of object: same
  table, same `role.*` permission family, same endpoints. The vocabulary addition is
  one field name, `parent_role`, deliberately plain rather than metaphorical
  ([naming-rules](../naming-rules.md)); the UI label is "derived role / inherits
  from X". Introducing a separate entity would have doubled the CRUD surface and
  the audit vocabulary for what is one column.

  **(b) Storage = reference + delta.** A derived role stores the parent's name plus
  its own delta. The delta is not a new field: it is the role's existing
  **`default_scope`** ([ADR-047 §a](0047-purview.md)), reinterpreted. On a plain role
  `default_scope` is the role's absolute scope; on a derived role it is the
  **attenuating delta** conjoined with the parent's effective scope. One formula
  covers both, because a plain role's parent side is the unrestricted top:

  ```
  effective_scope(r) = effective_scope(parent(r)) AND default_scope(r)
  effective_scope(r) = default_scope(r)                     when r has no parent
  ```

  Conjunction is the whole point: the grammar has no `NOT`, so `AND`-ing a predicate
  can only ever narrow. Attenuation of the scope is therefore **structural**, not a
  rule someone has to check.

  A worked example. Parent `dba` holds `redis.restart` and `redis.read` at
  `coven=dba`; child `dba-aboba` names `parent_role: dba`, keeps only `redis.read`,
  and adds the delta `trait.project=aboba`. Its effective right is `redis.read` at
  `coven=dba AND trait.project=aboba`. Move the parent to `coven=dbaas` and the child
  follows automatically to `coven=dbaas AND trait.project=aboba` — which is exactly
  why the delta stores **only the added narrowing** and never repeats the parent's
  predicate. A child that restated `coven=dba` would resolve to
  `coven=dbaas AND coven=dba` — the empty set — the moment the parent moved.

  **(c) Model = variant B: a subset of the rights, plus a narrower area.** A child
  does not merely re-scope its parent; it may also hold **fewer** permissions. Its
  own permission rows are its effective set, bounded by the parent:

  ```
  effective_perms(r) = own_perms(r) ∩ effective_perms(parent(r))
  ```

  where `∩` is the existing containment predicate of
  [`subset.go`](../../keeper/internal/rbac/subset.go) (so `incarnation.*` covers
  `incarnation.get`), reused rather than reimplemented — one definition of "⊆" in
  the codebase.

  The intersection is deliberate and it is where the security lives. A child's rows
  are **not** an implicit copy of the parent's: were they, a permission added to the
  parent would silently appear on every descendant — a **widening** cascade, the
  exact escalation this design exists to prevent. Narrowing cascades, and only
  narrowing: remove a permission from the parent and it drops out of every
  descendant at the next snapshot build, with no role rewritten by hand. Because the
  intersection is recomputed on every build, "a child never exceeds its parent" holds
  at the **decision** layer, not merely at the moment of writing.

  **(d) Resolution is flattened once, at snapshot build.** The chain is not walked
  per request. `NewEnforcerFromSnapshot` resolves each role's effective permissions
  and scope once ([`flatten.go`](../../keeper/internal/rbac/flatten.go)), and every
  subsequent check reads the flattened form — the same cost as today. Cascade needs no
  propagation machinery: the snapshot is already rebuilt
  on a role mutation (TTL poll + the `rbac:invalidate` pub/sub of
  [ADR-028(d)](0028-rbac-storage.md)), so a change to a parent reaches its children by
  the same path that already carries every other role change.

  Resolution applies the two rules above as two distinct mechanisms, and both are
  needed. The **scope ceiling** (b) is conjoined onto every permission of the child,
  including a per-permission `on <expr>` and a `*`; the **set intersection** (c)
  drops what the parent does not hold at all. Neither subsumes the other: a parent
  whose narrowing lives in per-permission scopes has no `default_scope` for the
  ceiling to conjoin, and a child that merely re-scopes needs no dropping.

  This draws one line the flat model did not have to: **`default_scope` is a
  default, the parent's effective scope is a ceiling.** A per-permission scope
  overrides the role's own `default_scope` ([ADR-047 §b](0047-purview.md)) and a
  `*` ignores it entirely (ADR-047(b) #1) — but neither escapes the parent's
  ceiling, which is conjoined regardless. A `*` on a role derived from a scoped
  parent is therefore a *scoped* super-admin, not an unrestricted one. A **bare**
  permission is the one case left untouched: it stays bare and inherits the role's
  resolved scope through `ResolvePurview` exactly as on a plain role, because
  writing the ceiling onto it would replace — and so discard — the child's own
  delta.

  Everything not **provably** within the parent is dropped rather than clipped:
  an undecidable glob containment (`host matches web-0?` inside `host matches
  web-*`, left conservative by NIM-128 §C.5) costs the child that permission. A
  chain whose conjoined scope cannot be normalized within the DNF caps fails the
  snapshot build outright, on the same terms as (f).

  **(e) The parent is ONE named role.** Not the union of the caller's roles, which
  would be wider than any single one of them, and not multiple parents. Choosing a
  single explicit ceiling is what makes "the child is bounded by X" a statement an
  operator can read off the role.

  **(f) Guards on the graph, in the DB.** Three integrity rules, enforced in the
  schema ([migration 102](../../keeper/migrations/102_rbac_roles_parent_role.up.sql))
  so they hold for every write path rather than only the Go one:
  a role cannot be its own parent; a chain cannot close on itself (`A → B → A`); a
  chain cannot exceed **4 roles** (three parent hops), mirroring the scope-nesting cap
  of [ADR-047](0047-purview.md). Depth is capped for two reasons — flattening stays
  bounded, and a ceiling four roles up is still something a human can follow.
  Re-parenting is checked from both ends: the ancestors above the moved role *and*
  the subtree already hanging below it.

  The same rules are re-checked in Go when a snapshot is built. A catalog whose graph
  is broken — a hand-edited row, a restore, an older binary — does **not** produce an
  enforcer. The degradation is [`Holder`](../../keeper/internal/rbac/holder.go)'s
  existing one, reached by the same code path as an unparseable permission and
  introducing no new failure mode: on a TTL refresh the previous enforcer is kept and
  the failure is warned; at startup the Keeper refuses to come up. The trade-off is
  deliberate: a stale-but-coherent catalog over a partial one, since dropping "just
  the broken roles" silently denies rights nobody asked to remove.

  **(g) Deleting a parent is refused — fail-closed RESTRICT.** A role that is still
  someone's `parent_role` cannot be deleted (`ON DELETE RESTRICT`,
  [`ErrRoleHasChildren`](../../keeper/internal/rbac/derived.go)); the operator
  re-parents or deletes the children explicitly. Every alternative changes somebody's
  rights implicitly, and two of them do so **upward**:
  - *clear the parent* (`SET NULL`) — the child's delta stops being a delta and
    becomes its absolute scope, dropping the parent's narrowing entirely. A
    **widening**, i.e. privilege escalation — the same reasoning that made
    [migration 100](../../keeper/migrations/100_rbac_drop_pattern_selectors.up.sql)
    refuse to strip selectors;
  - *re-root to the grandparent* — widening by definition, the grandparent's ceiling
    is at least as wide as the parent's;
  - *re-root to the caller* — makes a role's ceiling depend on who happened to run the
    delete;
  - *cascade the delete* — silently strips membership, and could remove a role behind
    the back of the self-lockout check.

  Refusing is the only option that changes nobody's rights without being asked.

  **(h) The write-time floor stays.** Structural attenuation is added **on top of**
  the existing least-privilege check, never in place of it. Creating or updating a
  derived role must satisfy **both**: `child ⊆ parent` (structural) **and** the caller
  holds the parent (`subset.go`, unchanged). Without the second, an operator holding
  `role.create` could derive from a role far above their own rights and grant
  themselves the result. The safe rule is the conjunction:
  **`child ⊆ parent AND caller-holds-parent`**.

  Note what "holds the parent" means and why it is the parent rather than the
  child: the caller must cover the **parent's** effective rights, because the
  cascade will carry every later widening of that parent into the child. Covering
  only what the child asks for today would let an operator mint a role that tracks
  a ceiling above their own. Both halves live in
  [`attenuate.go`](../../keeper/internal/rbac/attenuate.go) and re-run on **every**
  role update, not only the one that sets `parent_role` — otherwise the create gate
  would be a formality one PATCH wide.

  **Amendment 2026-07-27 (NIM-198): the child is measured in its RESOLVED form.**
  The least-privilege floor originally compared the child's rows *as stored*
  against the caller's rights. On a derived role those are not the same currency:
  a bare `incarnation.get` reads as unrestricted, while the role would grant it
  only inside the parent's ceiling. The floor therefore demanded more than the
  authorization it was protecting, and the delegation case — an operator scoped to
  `coven=dba` deriving from a role carrying that same ceiling — was refused
  outright. The only way through was to restate `coven=dba` in the delta, which is
  not a delta at all: per §(b) it pins the child to the coven the parent is in
  *today*, so the workaround silently converted a tracking role into a pinned one
  and cost the delegation the very cascade it was for.

  The floor now resolves the child against the parent first
  (`effectiveRoleRights`) and compares what the role **grants**. This removes no
  boundary. For a derived role the two conditions above already require the caller
  to cover the parent's *entire* effective set, and the child is contained in that
  set by the structural check — so caller-holds-parent dominates, and the stored-row
  comparison was not an additional guard but a contradiction with it. A **plain**
  role is untouched: with no parent there is no ceiling to resolve against, and a
  bare permission is still a request to grant it unrestricted.

  The same correction applies wherever a role's rights are weighed rather than its
  rows: binding a role to an operator (`role.grant-operator`) and bundling one into
  a Synod both confer what the role grants, and both now read the resolved form. A
  delegator who may create a derived role but not hand it out has not been
  delegated anything.

  The gate resolves the parent's chain inside the mutation's transaction but does
  **not** lock it. Deliberate: a parent narrowed concurrently is absorbed by
  re-resolution at the next snapshot build, since (c) recomputes the intersection
  every time. The write gate exists to refuse early and explain why, not to be the
  boundary — the boundary is the decision layer.

  **(i) Self-lockout counts PLAIN roles only.** The invariant
  "the last active operator with a bare `*` cannot be removed"
  ([ADR-013](0013-bootstrap-archon.md)) counts only **bare, unrestricted** `*`
  ([ADR-047 amendment, NIM-128](0047-purview.md)). A `*` sitting in a **derived**
  role is capped by its parent and is therefore *not* an unrestricted cluster-admin;
  counting it would overstate the number of admins and could let the real last one be
  removed. So a derived role **never** counts as a source of cluster-admin — including
  when its chain happens to resolve to an unrestricted `*` right now, because that
  depends on a parent any other mutation may narrow. Skipping is the conservative
  direction: it can only refuse a mutation, never permit one.

  The rule has two halves, and both are required:
  - **who survives** — every self-lockout probe joins `rbac_roles` and filters
    `parent_role IS NULL` ([`crud.go`](../../keeper/internal/rbac/crud.go),
    [`service.go`](../../keeper/internal/rbac/service.go)); `Enforcer.HasWildcard`
    applies the same filter in memory.
  - **when to probe** — a role STOPS being a source not only by losing `*` but by
    **becoming derived**. Setting `parent_role` on the last `*`-granting role
    therefore runs the same lockout check that removing its `*` would, and is
    refused on the same terms. Without this half the invariant is trivially
    bypassable: keep the permission set identical, add a parent, and the cluster is
    locked out with no rule having visibly fired.

  `rbac_roles` is in the probes' `FOR UPDATE` list for that second half. Turning a
  role derived writes only `rbac_roles`, so without the row lock "delete the last
  `*` role" and "re-root the last `*` role" run concurrently, each sees the other's
  role as the survivor, and both commit — a race none of the previously locked
  tables would catch.

  **(j) Several roles on one operator — derivation narrows a ROLE, not an OPERATOR.**
  An operator's effective rights stay what they are today: the **union** across their
  roles (OR among allows, [ADR-028](0028-rbac-storage.md)). A derived role resolves to
  its attenuated rights first and then joins that union like any other role — the
  union is over *resolved* roles, so a child can never smuggle in more than its parent
  allows.

  The counter-intuitive consequence has to be said out loud, because it is the
  opposite of what "I gave them the narrow role" suggests: granting an operator **both**
  `dba` and the derived `dba-aboba` leaves them with `dba`'s full rights. The narrow
  role adds nothing and restricts nothing — union is union. **Attenuation bounds what a
  role may contain, not what its holder ends up with.** To actually confine an operator,
  the wide parent must be revoked from them; deriving a narrower role and adding it on
  top is not a restriction mechanism.

  This is also why the parent of a new derived role is **one explicitly chosen role**
  rather than "the caller's rights": the caller's own union is wider than any single
  role they hold, so deriving against the union would let an operator mint a role
  broader than any role they could point at. The UI must therefore show the ceiling of
  the **selected parent**, not the caller's union — the correction tracked in NIM-182.

  **(k) The delta's intent is stored, and the cascade is confirmed** (Amendment
  2026-07-27, NIM-199). Two opposite intentions produced an identical row, and the
  system kept neither:

  - **track** — "my ceiling is the parent, whatever becomes of it". The delta
    holds only the added narrowing; moving the parent moves the child. This is the
    §(b) contract and remains the **default**.
  - **pin** — "my ceiling is the parent's scope as of now". The parent's effective
    scope is materialized **into** the delta at write time.

  The difference was visible only by comparing strings, and only by guessing — a
  child that happens to repeat its parent's predicate looks pinned whether anyone
  chose that or not, which is precisely what the floor bug of §(h) forced operators
  to write. **`rbac_roles.scope_mode`** ([migration
  105](../../keeper/migrations/105_rbac_roles_scope_mode.up.sql), `track` | `pin`,
  NULL exactly when `parent_role` is NULL, held to that by a CHECK) records it, so
  `pin` is a property of the role rather than a convention of whichever client
  wrote it.

  **Resolution does not branch on the mode.** Effective scope is
  `effective_scope(parent) AND default_scope(role)` for both; they differ only in
  what was written into the delta. Two consequences follow, and both are the point:
  a pinned role still **narrows** when its parent narrows (the monotone-AND
  argument of §(b) is untouched, and a pin is a defence against drift, never an
  escape from attenuation), and a pin against an already-unrestricted parent is
  vacuous — there is no room to widen into, so nothing needs freezing.

  A pin does expire in a way worth stating: if the parent moves *sideways*, the
  frozen predicate and the parent's new one conjoin to the empty set and the role
  grants nothing. That is the safe direction, and it is exactly the condition §(l)
  makes visible.

  **The cascade is no longer silent.** Any role mutation that changes the
  **effective rights** of any role below it — in either direction, at any depth
  within the cap — is refused with `ErrRoleCascadeNeedsConfirm` (409) unless the
  request carries `confirm_cascade`. The refusal names the derived roles that would
  gain rights, the ones that would lose them, and how many active operators hold
  any of them (directly or through a Synod): the operator is being asked to take a
  position, so they are handed what the position is about.

  Direction is computed by the containment predicate, not by comparing rendered
  strings, and the subtree is resolved twice — against the role as stored and
  against the role as the mutation leaves it — using the same [`attenuate`] the
  enforcer runs. A change nobody below feels passes silently: being a parent is not
  itself a consequence. A pinned child is absent from the *widening* half by
  construction, with no special case in the code — its materialized delta simply
  leaves it unmoved.

  This gate is **advisory in nature and mandatory in practice**: skipping it
  entirely would produce exactly the same rights. It is a gate on the operator's
  attention, not on the boundary — the boundary remains §(c)/§(h). It is placed
  last among the write-path checks, so a refusal about the caller's own rights is
  always reported before one that asks them to decide something.

  **(l) A row the chain no longer covers is published, not hidden** (Amendment
  2026-07-27, NIM-200). When a parent narrows past one of a child's rows, that row
  stays in `permissions` and drops out of `effective_permissions` — the cascade
  working as designed. The catalog now also publishes it under
  **`inert_permissions`**, because "a role listing two permissions and granting
  none" is indistinguishable from "a role somebody deliberately emptied", and the
  two call for opposite responses.

  The set is published rather than derivable: subtracting `effective_permissions`
  from `permissions` compares a stored form against a resolved one, and a consumer
  doing that arithmetic would be re-deriving the attenuation rules — the mistake
  §J3 exists to prevent.

  **The emptiness itself is correct and stays.** `incarnation.*` under a parent
  narrowed to `incarnation.get` resolves to **nothing**, not to `incarnation.get`.
  Clipping the wildcard down to the parent's current set would mean a permission
  later added to the parent silently appears on every descendant holding a `*` —
  the widening cascade §(c) exists to forbid. Everything not *provably* within the
  parent is dropped, and a wildcard is provably within nothing.

  One consequence has to be handled rather than documented: once rows go inert,
  every later edit of the child is refused by §(c) until they are dropped, so
  `ErrRoleExceedsParent` names **every** uncovered row rather than the first. An
  operator told about one offending row at a time cannot repair the role in a
  single PATCH.

- **Delivery.**
  - **J1 (NIM-179).** The column, its guards, and the plumbing that carries
    `parent_role` into the enforcer snapshot and the role catalog. Nothing read
    `parent_role` when a permission was checked — the safe direction: an unresolved
    parent means a child grants only its own rows, narrower than the intended
    semantics, never wider, so the gate landed before the resolver without opening a
    window.
  - **J2 (NIM-180).** Flattening at snapshot build (d), the `∩`/`AND` resolution,
    the write-time gate (h) with `ParentRole` on the service's create/update inputs,
    and the self-lockout correction (i). From here a stored `parent_role` is
    authoritative at the decision layer.
  - **J3 (NIM-181).** The API surface: `parent_role` on the role endpoints and MCP
    tools (create takes it, update takes it with the PATCH presence of
    `default_scope`), and the catalog returning each role in both forms — as stored
    and as resolved (`effective_permissions` / `effective_scope`). The read side
    resolves through the same code the enforcer runs, so no consumer re-derives
    inheritance and none can arrive at a wider answer than the decision layer; a
    catalog whose graph does not resolve fails the read rather than publishing the
    unattenuated rows. The audit records of `role.created` /
    `role.permissions-updated` carry the ceiling and the delta, not only the
    permission list, since on a derived role the list alone is not the
    authorization change. The transport error mapping for `role-has-children`,
    "derived role exceeds its parent" and the graph sentinels was already in place.
  - **J4 (NIM-182).** The web selector and the "you inherit X, you cannot widen it"
    panel.
  - **J5–J7 (NIM-198 / NIM-199 / NIM-200), one change.** The §(h) amendment, the
    `scope_mode` column with the cascade report of §(k), and the inert rows of
    §(l). Delivered together because they are one semantics seen from three sides:
    the floor bug forced operators to pin, pinning was unrecorded, and the pins it
    produced are the roles most likely to go inert.

- **Amendment 2026-07-28 (NIM-202 / NIM-203) — the catalog is a read, and reads need a ceiling too.**
  J3 published each role in both forms, and did it for everyone: `role.list` answered
  with the whole catalog. A role carries more than a name — its permission set, its
  scope and the AIDs holding it — so the full catalog is the cluster's privilege map,
  and a `coven`-scoped operator was reading all of it.

  **A caller sees a role exactly when the caller could GRANT what that role grants.**
  The predicate is (c)'s containment, unchanged, evaluated against the role's
  **effective** form ([`role_visibility.go`](../../keeper/internal/rbac/role_visibility.go)).
  Reusing the write-side rule is the whole point: it is a boundary the caller already
  cannot cross, so what lies inside it leaks nothing, and there stays **one definition
  of ⊆** for the subsystem. A second, read-only notion of "close enough to show" would
  be free to disagree with the decision layer, and in this direction a disagreement is
  a leak. A bare `*` covers everything, so the administrator's view is unchanged.

  Judging the **effective** form rather than the stored rows matters here for the same
  reason it matters in (c): a derived role carrying a row its parent stopped covering
  grants nothing through that row, and must not be hidden on account of it.

  Coverage cannot express the reader who must see roles they hold nothing of — an
  auditor, a security review — so that is an explicit right, **`role.list-all`**: a
  breadth modifier on `role.list`, mounted on no endpoint of its own (the
  `operator.read` pattern). It is checked with the same containment, against the
  caller set the filter has already loaded, so `*` and `role.*` cover it for free; the
  required permission is bare, so only an UNRESTRICTED holder gets the full catalog —
  a scoped `role.list-all on X` selects nothing, the grammar having no `role=`
  dimension. Details in [rbac.md → Catalog visibility](../keeper/rbac.md).

- **Amendment 2026-07-28 (NIM-201) — a PLAIN role tracks nothing, and that is an
  action of its own.** (c)/(d) make a derived role follow its parent; a role with
  no parent follows nothing. It is a snapshot of privilege that outlives whatever
  its author held — revoke their `coven=dba` role and the plain role they minted
  keeps granting `coven=dba`, with no rule having visibly fired. The write-time
  floor of (h) does not catch it: every permission in that role WAS covered when
  it was written. The floor bounds what may go into a role, not whether the result
  keeps tracking the rights it came from.

  So minting a parentless role that grants something requires **`role.create-root`**
  ([`root_role.go`](../../keeper/internal/rbac/root_role.go)); `role.create` alone
  admits a derived role. **The default becomes: derive from a role you hold, and
  the cascade does the rest.**

  The ceiling stays ONE named role, and deliberately never the creator. Binding it
  to a person would reintroduce exactly what (e) rejects — a creator's union across
  their roles is wider than any single role they could point at — and would make
  every role they wrote depend on their continued employment: revoking one operator
  would zero every role they created, and granting them a role would widen those
  roles through `created_by_aid`, an audit field invisible in the role editor. A
  role belongs to its parent, not to its author; the operators holding that parent
  administer it.

  The gate judges the **shape of the result**, not the verb: it fires on create, on
  a PATCH that clears `parent_role`, and on a PATCH that grows an already-plain
  role. Gating only creation would leave it one PATCH wide — the same trap (h)
  records. A parentless role that grants NOTHING is free (no privilege to strand),
  and trimming stays ungated (removal adds nothing). `*` and `role.*` cover the new
  action for free, so only a role enumerating `role.create` explicitly is affected.

- **Consequences.**
  - `rbac_roles` grows one nullable column; every existing role reads back as plain,
    with ADR-047 semantics bit-for-bit. No data migration, no backfill.
  - `rbac_roles` grows a second nullable column, `scope_mode` (migration 105), tied
    to `parent_role` by a CHECK. Every write path that sets a parent must set a mode
    with it — including test fixtures and any hand-written SQL.
  - Editing a role that has derived roles is a two-step operation whenever the edit
    moves them: read the report, then resend confirming it. Deliberate friction on
    the change with the widest reach, and the one the operator can least see.
  - The default for a new derived role is `track`. With §(h) fixed there is no
    longer a reason for a client to default to `pin` — the workaround that made it
    necessary is gone, and defaulting to `pin` would quietly opt every role out of
    the cascade the feature is for.
  - `default_scope` becomes context-dependent — absolute on a plain role, a delta on a
    derived one. This is the price of not adding a second scope column, and it is
    contained: the two readings differ only in what sits on the left of the `AND`.
  - A parent cannot be deleted while it has children. Deliberate friction on a
    security-critical edge.
  - Chains are capped at four roles. Deeper hierarchies must be expressed as a wider
    tree or as separate roles.
  - Cycles and over-deep chains are rejected at write time and, if they somehow
    reach storage, take the whole snapshot build down rather than degrade it.

- **Rejected.**
  - **A materialized `effective_scope` column** (flatten on write, store the result).
    Faster to read, but it duplicates the truth: the stored value and the chain can
    disagree after any missed recomputation, and a stale *scope* is a wrong *authorization*
    decision. Kept in reserve for a role graph large enough that per-build flattening
    shows up in a profile — the formula above is the contract either way.
  - **Multiple parents.** The ceiling would be a union or an intersection of several
    roles, neither of which an operator can read off the role, and the union form is
    wider than any single parent — precisely the escalation shape avoided in (e).
  - **Implicitly copying the parent's permissions to the child.** A widening cascade;
    see (c).
  - **A separate `derived_role` entity / a `role.derive` verb of its own.** Doubles
    the CRUD and audit surface for one column; see (a).
  - **Deriving from a Synod.** A [Synod](0049-synod.md) bundles roles for operators
    and carries no scope of its own, so there is nothing to attenuate against.
    Derivation stays role → role.
  - **A pin that stops the parent's narrowing too** (a genuinely frozen snapshot,
    resolved without conjoining the parent). It would make `pin` an exemption from
    §(c) rather than a defence against drift: a child would keep rights its parent
    had lost, which is the one thing derivation exists to make impossible. Pinning
    materializes into the delta instead, so the conjunction — and the invariant —
    still runs (§(k)).
  - **Clipping an inert wildcard down to the parent's set** instead of dropping it.
    Intuitive, and a widening cascade: the child's `*` would absorb every permission
    later added to the parent (§(l)).
  - **Warning instead of refusing on a cascade.** A warning in the response body is
    read after the write has happened, which is the wrong side of a change that
    reaches roles the operator did not name. Refusing costs one extra round trip and
    makes the acceptance explicit — and auditable (§(k)).

- **Amends** [ADR-028](0028-rbac-storage.md) (the `rbac_roles` schema),
  [ADR-047](0047-purview.md) (`default_scope` gains its delta reading),
  [ADR-049](0049-synod.md) (unchanged in substance — a Synod still bundles roles;
  recorded so the RBAC trio stays cross-referenced).
