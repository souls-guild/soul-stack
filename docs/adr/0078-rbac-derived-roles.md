# ADR-078. Derived roles — `parent_role`, attenuation and cascade

- **Status.** Active. J1 (NIM-179) landed the model, storage and graph guards;
  J2 (NIM-180) landed chain resolution, the write-time attenuation gate and the
  self-lockout correction of §(i). The API surface for `parent_role` is NIM-181,
  the web selector NIM-182.

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
    tools, and the parent in `GET /v1/roles`. The transport error mapping for
    `role-has-children`, "derived role exceeds its parent" and the graph sentinels
    is already in place.
  - **J4 (NIM-182).** The web selector and the "you inherit X, you cannot widen it"
    panel.

- **Consequences.**
  - `rbac_roles` grows one nullable column; every existing role reads back as plain,
    with ADR-047 semantics bit-for-bit. No data migration, no backfill.
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

- **Amends** [ADR-028](0028-rbac-storage.md) (the `rbac_roles` schema),
  [ADR-047](0047-purview.md) (`default_scope` gains its delta reading),
  [ADR-049](0049-synod.md) (unchanged in substance — a Synod still bundles roles;
  recorded so the RBAC trio stays cross-referenced).
