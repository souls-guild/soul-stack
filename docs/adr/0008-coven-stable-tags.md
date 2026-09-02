# ADR-008. Coven — Stable Logical Tags Only

- **Context.** Originally Coven was used both for stable grouping (cluster / project / environment / datacenter) and for the host's role: `incarnation.name` became the root tag, and sub-roles followed the convention `{incarnation.name}-{role}` (`…-master`, `…-replica`). The role (who is currently master in Redis HA) is **volatile**: it changes on failover at any moment, without Keeper's involvement. Tying a volatile role to a Coven tag would mean the tag either goes stale (targeting `…-master` hits the former master) or requires a freshness mechanism / a role collector / volatile Soulprint facts — an extra moving part on the critical path of destructive operations.
- **Decision.**
  - **Coven — stable logical tags only:** cluster, project, environment, datacenter, hardware type. Whatever does not change on its own while the service runs.
  - `incarnation.name` **remains** the root Coven tag for all hosts of an incarnation, assigned automatically by keeper. **[SUPERSEDED 2026-07-17 (NIM-124) — see the amendment "incarnation.name is not a Coven; membership is a first-class relation" below.](#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)**
  - **The `{incarnation.name}-{role}` convention is removed.** There are no more role-based sub-covens.
  - **Role (master / replica) is not a Coven.** There are two conceptually different roles: **declared** — in `incarnation.spec.hosts[].role`, for bootstrap (`create`, where there is nothing yet to probe), topology, and audit; **actual** — volatile, obtained by a **live probe step** in the scenario (`module: core.exec.run` + `register:`), the next step's target follows a `where:` on that `register:`. After a failover — just a new probe. **[SUPERSEDED 2026-07-30 (NIM-330) as to WHERE the declared role lives: `spec.hosts[]` is removed; a declared role is a Voice in a Choir. The declared/actual split itself stands unchanged.](#amendment-2026-07-30-nim-330-the-declared-role-is-a-voice-not-a-spec-field)**
  - **The volatile does not live in Soulprint.** Soulprint holds only stable / slowly changing facts. There are no volatile Soulprint facts, no role collector, no freshness mechanism.
  - **Essence is role-agnostic.** The `role/<Y>.yaml` stage is removed from the essence build pipeline; the build order is `default → os → coven → incarnation.spec`. Role-dependent parameters move into destiny and are passed via `input:` based on the probed role.
- **Rationale.** A stable tag never goes stale — targeting by Coven is always correct against Postgres. The volatile (role) is queried exactly at the moment of use and is not cached — there is no desync window before a destructive operation. We remove a whole class of mechanisms (role collector, freshness, volatile facts) that existed only to support role-as-a-tag. P2P Soul↔Soul for exchanging role was rejected separately — it breaks ADR-002/004 and the security model.
- **Trade-off.** A scenario that needs the actual role must contain an explicit probe step before targeting (you cannot simply write `coven: …-master`). This is more lines in the scenario, but the price buys correctness of destructive operations across a failover. We accept it.
- **Supersedes.** The "Targeting and host relationships" section with regard to `{incarnation.name}-{role}` sub-covens; the essence layer `role/*.yaml` and the line `role/{{ host.role }}.yaml` in `essence/_stack.yaml`. The full user decision is recorded; targeting details — the ["Targeting and host communication"](../architecture.md#targeting-and-host-communication) section, scenario details — [`docs/scenario/`](../scenario/README.md).
- **Amendment (2026-05-25, environments = Coven, first-class `Environment` rejected).** On the question of "support for environments with a fixed set and permission separation," it is recorded: **an environment remains a special case of the Coven tag** (as already stated in "Decision"), a separate `Environment` axis/entity is **not introduced** (rejected in favor of a single targeting axis — otherwise tomorrow the same would be requested for datacenter/project and we would multiply axes). Two **planned** tasks follow from this (not implemented as of 2026-05-25):
  - **(a) Per-Coven RBAC scope for incarnation operations — IMPLEMENTED (C+D, 2026-05-25).** A known gap (the incarnation-endpoints extractor put only `{incarnation: name}` into the RBAC context, without `coven`/`service`, so a role like `incarnation.* on coven=prod` did not match — a docs↔code drift) **is closed**:
    - **(C) Declared environment tags — the `incarnation.covens` column** (migration **046**): stores the declared set of stable coven tags of an incarnation (including environment). This removes the ambiguity of "what an incarnation's coven means" from the original planned wording — it is not tags smeared across hosts, but an explicitly declared incarnation-level set.
    - **(D) The RBAC context of incarnation routes is enriched** to `service=` + **multi-value** `coven=` (= `incarnation.covens` ∪ `{incarnation.name}` — `incarnation.name` remains the root Coven tag, see "Decision"). Matching is an **OR-Check** (`RequirePermissionMulti`): access is granted if the permission covers **at least one** tag in the set. `enforcer.Matches` is NOT touched (only context collection changed, not the engine). **[SUPERSEDED 2026-07-17 (NIM-124): `{incarnation.name}` is dropped from the coven set — the RBAC context is `service=` + `coven=incarnation.covens` + the `incarnation=<name>` dimension; scope by the incarnation's own name uses `incarnation=`, not `coven=`. See the amendment below.](#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)**
    - **`create` is body-scoped:** the check runs against `covens` from the request body, so an operator with scope `coven=dev` cannot create an incarnation with `covens=[prod]` (no "sneaking" into someone else's environment on creation).
    - **REST↔MCP parity:** all 7 incarnation MCP tools (`run` / `upgrade` / `destroy` / `get` / `history` / `unlock` / `create`) perform the same scoped OR-Check, **fail-closed** (no matching permission → denied).
    - **Limitation (backlog).** Selectors are **single-key** (`coven=` OR `service=`); a combined multi-key **AND** (`coven=X,service=Y` — "only this service's prod environment") **cannot be expressed** by the permission-selector grammar. The `Permission.Selector` structure already supports AND semantics — only the parsing grammar is missing. Extending this requires a parser extension + a separate ADR (see [Open Questions](../architecture.md#open-questions)).
  - **(b) A "predefined fixed set" of tags — a validated coven-tag directory.** Currently `souls.coven` is free-form strings with no directory (`migrations/007`), so a typo silently spawns a phantom tag. Planned: a registry of allowed coven tags (optionally typed as `environment`/`datacenter`/…), validated against it. The extension point is a `CovenLabelValidator` hook in the Service layer (no-op in the MVP), so the API doesn't break once the directory appears.
- **Amendment (2026-05-29, cross-incarnation at the Voyage layer — a deliberate user decision).** The invariant "cross-incarnation targeting is forbidden by the grammar" (see "Decision" above and ["Targeting and host communication"](../architecture.md#targeting-and-host-communication)) **is lifted at the upper Voyage layer** ([ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run)): a `kind=scenario` Voyage **legitimately orchestrates several incarnations** in one run (batch = N incarnations, B1). At the level of the **scenario resolver and a single scenario run the prohibition STANDS** unchanged: one scenario run works strictly with one incarnation (one cross-host barrier, one per-incarnation state commit, [ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)). Voyage does not blur the incarnation boundary — it orchestrates **N independent scenario runs**, each with its own incarnation and its own state commit. The cross-incarnation prohibition inside the scenario `on:`/`where:` grammar is not relaxed.
- **Amendment (2026-05-29, Choir ≠ coven — an explicit boundary).** [ADR-044](0044-choir.md#adr-044-choir--named-host-topology-within-an-incarnation) introduces the **Choir** entity — a named host position WITHIN an incarnation (a topological "part"). **Choir is NOT a Coven** and does not resurrect the removed `{incarnation.name}-{role}` sub-coven: coven is the global, stable targeting/RBAC axis (cluster / project / environment / datacenter), Choir is an intra-incarnation topological axis with no coven RBAC semantics. That said, **Choir is a stable fact** (like coven, not like a volatile role): it is declared by the operator and does not change on its own while the service runs, so it is available in `where:` predicates **without a probe step** (via `soulprint.self.choirs` / `soulprint.hosts[].choirs`, [ADR-044](0044-choir.md#adr-044-choir--named-host-topology-within-an-incarnation) item 5). The volatile **actual** role (who is currently master after a failover) is still obtained only via probe + register + `where:` — Choir does NOT replace it (Choir = declared topology, not actual state).

## Amendment (2026-07-17, NIM-124): `incarnation.name` is not a Coven — membership is a first-class relation

Fixed by the user (R4). The original "Decision" made `incarnation.name` the **root Coven tag**, automatically appended to every member host's `souls.coven[]`, and derived incarnation **membership** from that fact (`incarnation.name ∈ souls.coven[]`). This **conflated two axes** on one column: (1) *membership* — which incarnation a host belongs to, and (2) *stable logical tags* — cluster / project / environment / datacenter. The user rules this a design defect: **membership must be its own relation, not a Coven equal to the incarnation name.** Hosts are selected by membership, not by a synthetic coven; `${ incarnation.name }` **ceases to be a targetable Coven tag**.

**Decision (reversal).**

1. **Membership — a first-class M:N relation `incarnation_membership(incarnation_name, sid)`** (a join table modeled on `incarnation_choir_voices`, FK `sid → souls`, FK `incarnation_name → incarnation`, PK `(incarnation_name, sid)`, `bound_at`/`bound_by_aid` for audit). A host MAY be a member of several incarnations (M:N — deliberate, for maximum flexibility; the "one scenario run = one incarnation" invariant is unchanged, it constrains a *run*, not a *host*). Membership is **no longer** stored in `souls.coven[]`.

2. **Coven reverts to purely stable logical tags** (cluster / project / environment / datacenter / hardware type), as the original "Decision" intended — but **never the incarnation identity**. `incarnation.name` is not injected into `souls.coven[]` and is not a valid Coven value for targeting.

3. **The `on:` resolver bases on membership, not on the name-coven.**
   - **omitted `on:`** = all member hosts of the incarnation (resolved via `incarnation_membership`, not via `= ANY(coven)`).
   - **`on: [coven-a, coven-b]`** = AND-intersection of the listed *stable* covens, **always ⊆ members** (the roster is already membership-scoped, so cross-incarnation remains impossible by construction — the security invariant is now enforced by the membership join, not by the name-coven being implicitly ANDed).
   - **`on:` containing `${ incarnation.name }`** (e.g. `on: ["${ incarnation.name }"]`) is a **validation error** (`name` is not a Coven), with a message steering to the omitted `on:` form. This is fail-closed: stale scenarios error out instead of silently resolving to an empty set.

4. **`core.soul.registered` — the bind act sets membership implicitly** from the current run's incarnation (cross-incarnation binding is forbidden by the grammar, so the target is unambiguous). Its `coven:` parameter becomes **optional** and carries **only real stable tags** (it may be empty). The former footgun guard ("a host must always carry the root coven incarnation": `params.coven` `min_items: 1` + the `mode: replace` empty-set error) is **removed** — membership no longer lives in `coven[]`, so no coven operation can strip it. `core.soul.registered` = bind (membership + onboarding barrier); assigning coven tags = pure stable-tag management (no longer a membership act).

5. **RBAC — supersedes item (D) above.** The incarnation-route RBAC context drops `{incarnation.name}` from the coven set: it becomes `service=<service>` + `coven=<incarnation.covens>` (the declared stable tags) + the existing `incarnation=<name>` dimension. Scope by the incarnation's own name is expressed as `incarnation=<name>`, **not** `coven=<name>`. Roles previously written as `incarnation.* on coven=<incarnation-name>` must migrate to `incarnation=<incarnation-name>` (a behavior change, called out in release notes). The OR-Check engine (`RequirePermissionMulti`) is unchanged; only the collected context changes.

6. **Projections drop the synthetic name.** `soulprint.self.covens` and `soulprint.hosts[].covens` no longer carry `${ incarnation.name }` — they project only the real `souls.coven[]` stable tags. The documented pattern `soulprint.where("incarnation.name in covens")` ("hosts of this incarnation") is **removed**: the `soulprint.hosts` / `soulprint.where(...)` accessors are already incarnation-scoped (they operate on the run roster = members), so "all members" is simply `soulprint.hosts` / `soulprint.where("true")`.

7. **Migration.** A migration (a) creates `incarnation_membership`, (b) backfills it from the existing `incarnation.name ∈ souls.coven[]` fact, then (c) strips every `incarnation.name` value out of `souls.coven[]` so the coven axis holds only real stable tags. Guard tests: "omitted `on:` → exactly the members, with no name-coven"; "no resolver / RBAC / bulk-selector selects by `coven == incarnation.name`".

**Supersedes.** The "Decision" line "`incarnation.name` remains the root Coven tag … assigned automatically by keeper" and amendment item (D) "`coven = incarnation.covens ∪ {incarnation.name}`". The removed `{incarnation.name}-{role}` sub-coven rule (already removed by the original Decision) is unaffected. The cross-incarnation prohibition (Voyage amendment above) stands — it is now enforced by the membership roster rather than by the name-coven.

**Amends [ADR-060](0060-traits.md#adr-060-trait--operator-set-key-value-labels-host-and-incarnation).** The Trait projection's member criterion ("member of incarnation = host whose incarnation name ∈ `souls.coven[]`", `soul.BulkSelector{Incarnation}`) now resolves membership via `incarnation_membership`, not via `= ANY(coven)`. The Trait read/target layer is otherwise untouched.

**Trade-off.** A one-time migration cost (backfill + coven strip) and a behavior change for RBAC roles scoped by `coven=<incarnation-name>` and for scenarios using `on: ["${ incarnation.name }"]` (now the omitted form). We accept it: the coven axis stops meaning two things at once, membership becomes explicit and auditable, and `mode: replace` on coven can no longer accidentally sever a host from its incarnation.

**Amendment (2026-07-27, NIM-121 — Coven is inherited from the incarnation, [ADR-080](0080-label-inheritance-union.md)). REVOKED 2026-08-05 by
[NIM-281](#amendment-2026-08-05-nim-281-a-label-is-never-inherited).** It made a host's effective covens its own `souls.coven[]` unioned at read time
with the `incarnation.covens[]` and names of every incarnation it belongs to. That union no longer exists; see the NIM-281 amendment below for the rule
that replaces it.

## Amendment (2026-08-05, NIM-281): a label is never inherited

**A host's covens are exactly `souls.coven[]` — the tags an operator attached to that host, and nothing else.** Belonging to an incarnation attaches
none. There is no union, no read-time projection, no materialized copy, and the incarnation's name is not among a host's tags (NIM-124 already removed
the injected copy; nothing puts it back).

This reverts NIM-121 / [ADR-080](0080-label-inheritance-union.md) on both axes, Coven and Trait, and on both sides of the wire — the RBAC scope
predicate, `soulprint.self.covens` / `soulprint.hosts[].covens`, the souls list filter, the bulk selector and its gates, push provider routing, the
Vigil/Decree and Augur Rite subjects. Every one of them now reads the bare column. `keeper/internal/soul.EffectiveCovens` and the union arm of
`rbac.CovenScopeSQL` are gone; the set-based readers render one predicate, `souls.coven && $N::text[]`.

The incarnation side is symmetric: a coven scope matches an incarnation by `covens &&` alone. `name = ANY($x)` is gone from that predicate too — an
incarnation's name is its identity, answered by the `incarnation=` dimension, not a label.

**Reaching an incarnation's hosts is a MEMBERSHIP question, and it is spelled `incarnation=<name>`** — resolved from `incarnation_membership`, the same
relation the roster comes from. `coven=` asks a label question and answers it from labels; neither dimension stands in for the other.

**Consequence, stated plainly:** an incarnation's tag reaches its hosts nowhere. Labelling the incarnation `prod` does not make its members visible to a
`coven=prod` role and does not route them behind that coven's bastion. To give a host a tag, attach the
tag to the host (`POST /v1/souls/coven`, `POST /v1/souls/traits`). The write paths are unchanged, as are both of coven-assign's gates.

> The single exception, added the same day by
> [NIM-280](#amendment-2026-08-05-nim-280-a-rules-subject-reads-both-levels--targeting-only): a rule's **subject** does read both levels, so
> labelling the incarnation `prod` does now match a Vigil, Decree or Rite scoped to `coven: ["prod"]`. That is a targeting decision resolved at match
> time; the host still carries nothing, and no authorization path changed.

An incarnation's own labels keep the one job that was always theirs: selecting overlays of **its own** config, through a `foreach:` over
`incarnation.covens` in `vars/_stack.yaml` ([ADR-0082](0082-service-vars.md)). That is the incarnation's config, resolved once per run — not a label on
any host.

**Known narrowing. CLOSED the same day by [NIM-280](#amendment-2026-08-05-nim-280-a-rules-subject-reads-both-levels--targeting-only) (below).** A Vigil/Decree or an Augur Rite took a subject of `sid` XOR `coven`, so with inheritance gone there was no way to bind one to "every
member of incarnation X" without tagging those hosts by hand. The missing membership dimension in the Rite/Decree grammar was **NIM-280**.

## Amendment (2026-08-05, NIM-280): a rule's SUBJECT reads both levels — targeting only

**This does not weaken the amendment above. A host still carries exactly `souls.coven[]` / `souls.traits`, and nothing is ever written to `souls`.**
What changed is one read, in one place: when a Vigil, Decree or Augur Rite asks *which hosts do I reach*, the answer is computed over the host's own
labels **unioned with the labels of every incarnation it is a member of** — for the duration of that one match, and nowhere else.

A rule's subject is now EXACTLY ONE of four dimensions ([ADR-030](0030-vigil-oracle.md#adr-030-vigil--oracle--event-driven-monitoring-beacons--reactor),
[ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access)), named after the RBAC scope vocabulary:

| dimension | reaches | levels read |
|---|---|---|
| `sid: [<sid>, …]` | those hosts, by identity | host |
| `incarnation: {service, name}` | every host on that incarnation's roster | the relation `incarnation_membership` |
| `coven: [<label>, …]` | a host carrying one of the labels, **and** every member of an incarnation carrying one | host + incarnation |
| `trait: {key, value}` | the same, on the traits map | host + incarnation |

**Why this is not the inheritance NIM-281 removed.** Inheritance made the HOST CARRY the incarnation's label, so *every* consumer saw it — RBAC scope
predicates, `soulprint.self.covens` in CEL, the souls list filter, the bulk selector, push provider routing — including consumers that never asked
about incarnations, and including ones that make authorization decisions. Here the union is local to one selector match: the column is unchanged, the
CEL projection is unchanged, the scope predicate is unchanged. Unbinding a host takes the reach away again with nothing to un-write.

**★ The boundary (normative).** Targeting expands; operator authorization never does. `rbac.CovenScopeSQL` stays `souls.coven && $N::text[]` — the row's
OWN column, no join to `incarnation` — and the two resolvers must never be wired together. A subject decides which hosts a rule REACHES; a scope decides
what an Archon MAY DO. Tagging an incarnation `prod` must never grant a `coven=prod` role permanent visibility of its roster.

**⚠ The label namespace is shared, so labelling an incarnation is now a rule-affecting act.** `prod` on an incarnation widens every existing
`coven: ["prod"]` **subject** to its members with no rule edited — the audit trail records an incarnation update, not a rule change. On the Augur side
the widened thing is access to a secret. That is the declared meaning of a label subject; where it is not wanted, address the roster explicitly
(`incarnation`) or name the hosts (`sid`). Symmetrically, **unbinding a host withdraws every rule that reached it through its incarnation** — membership
changes are monitoring- and grant-affecting.

An incarnation's **name** is still not one of its labels. `coven: ["<incarnation-name>"]` keeps reaching only hosts somebody tagged with that string;
the roster is addressed by the `incarnation` dimension, which reads the relation.

## Amendment (2026-07-28, NIM-209): membership has an operator path — bind / unbind / read

**Context.** The [2026-07-17 amendment](#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation) made membership a first-class relation and named exactly one writer: item 4, "`core.soul.registered` — the bind act sets membership implicitly from the current run's incarnation". It did not say how an operator binds a host that is **already onboarded** — and the answer turned out to be: they cannot.

That gap closes a real product surface. A run resolves its roster at start and aborts `no_hosts` when it is empty (`scenario/run.go` §3, the two bypass classes do not apply). `POST /v1/incarnations` inserts the row and starts the create run in the same call when `lifecycle.auto_create` is on, so there is no window in between. Consequently a create scenario that deploys onto ready hosts — `examples/service/redis/scenario/create_from_souls`, described as "deploy onto a ready roster of already onboarded online souls" — was **unreachable through the operator API**: a roster could only come into being through cloud-provision, where `core.soul.registered` binds the SIDs it has just created. The e2e harness worked around it by seeding the incarnation row with direct SQL, which is not a path an operator has. Choirs inherited the same dead end: `AddVoice` requires existing membership (`ErrNotMembers`), so a Choir could not be built for an incarnation nothing had ever bound to.

**Decision.**

1. **Membership becomes an operator-visible sub-resource of the incarnation** — `POST /v1/incarnations/{name}/members`, `DELETE /v1/incarnations/{name}/members/{sid}`, `GET /v1/incarnations/{name}/members`, with the MCP twins `keeper.incarnation.bind-member` / `.unbind-member` / `.members`. The relation, its columns and its FKs are unchanged; this amendment adds the missing writer and reader, not a new model.

2. **Two permissions, `incarnation.bind-member` and `incarnation.unbind-member`** (the `choir.add-voice` / `choir.remove-voice` split). Unbinding is the destructive half — it removes the host from the roster of every FUTURE run — so it is grantable separately. Reading the roster rides on `incarnation.get`: a roster is part of knowing what an incarnation is.

3. **Authorization is TWO gates, and one is not enough.** Gate (a) is the ordinary incarnation selector (`incarnation=` / `coven=` / `service=` by path-`{name}`), applied by middleware on REST and by the explicit body-scoped OR-Check on MCP. Gate (b) requires **every target SID to be inside the caller's soul visibility** (the `soul.list` purview, `soulpurview.InScope`).

   Gate (b) is not belt-and-braces. A scope predicate on `incarnation=X` is satisfied without ever examining the host, so a holder of `incarnation.bind-member on incarnation=X` would otherwise be able to pull **any** host in the fleet into X — and then reach it with `incarnation.run`, which is scoped to the same incarnation. Membership is therefore an escalation edge, and the host side of it must be checked on the host axis. This mirrors the lesson of NIM-198 / NIM-202: a basis wider than the rights it is derived from is an escalation, not a convenience.

   Gate (b) is **all-or-nothing**: one out-of-scope SID rejects the whole call. A partial bind would report success while leaving the roster short a host, and the run would then fail somewhere else entirely.

   Gate (b) is evaluated **before** the bind, and the ordering still matters after [NIM-281](#amendment-2026-08-05-nim-281-a-label-is-never-inherited) removed inheritance. A bind mints no tag now, but it does hand the host to every `incarnation=`-scoped permission on that incarnation; checking after would let the act create its own authorization on that dimension. So the caller must already see the host on some other basis — its own `souls.coven[]`, a `host=` selector, or another incarnation it is already in. In practice this means labelling the host at onboarding, or letting a provisioning scenario stamp it, and it is why the operator path is a bind of hosts you can already reach rather than a way to reach new ones.

   No new selector keys: the grammar stays `{service, coven, incarnation, host}` ([rbac.md § Selector grammar](../keeper/rbac.md)).

4. **The operator may bind only a `connected` host.** A `pending` host has never reported; `disconnected` / `revoked` / `expired` / `destroyed` will not answer the run. Binding them would produce a roster that looks right and fails at dispatch. The keeper-internal bind act is explicitly **not** held to this rule — `core.soul.registered` binds hosts it has itself just created, still `pending`, which is the whole point of the provision-from-zero path.

5. **Both directions are idempotent, and both stay legible.** A re-bind writes nothing (`ON CONFLICT DO NOTHING`) yet the reply and the audit payload split `bound` from `already_member`, so a repeat is distinguishable from a first bind. Unbinding a non-member succeeds with `removed: false`. A host deleted from the registry is a no-op rather than a 404 — FK `sid → souls ON DELETE CASCADE` has already taken its memberships with it.

6. **The FK invariant is unchanged and now visible in the API shape.** Membership requires its incarnation row to exist; the path parameter `{name}` is resolved before anything is written, so "row first, membership second" is enforced by the route rather than by a constraint violation. Unknown SIDs are rejected with a 422 that names them instead of surfacing an FK error.

7. **Audit.** `incarnation.member_bound` (`{name, sids, bound, already_member}`) and `incarnation.member_unbound` (`{name, sid, removed}`), written by the handler itself. The keeper-internal bind inside a run does not emit them — it is covered by that run's own `task.executed` trail; these two events are specifically the operator path.

**What this does NOT change.** The bind act inside a scenario run (item 4 of the 2026-07-17 amendment) keeps working exactly as before, including its `pending`-host behavior. The cross-incarnation prohibition stands — a bind names one incarnation, and the roster remains the boundary. `lifecycle.auto_create: false` remains a **service** policy for deferring the create run; it is not the mechanism for binding, and on its own it never was one.

**Trade-off.** Membership becomes operator-mutable, which is a new way to change what a future run touches — so it is permissioned in both directions, gated on both axes and audited on both. The alternative considered was accepting a list of SIDs on `POST /v1/incarnations` and binding them in the same transaction: it is a smaller change, but it only ever works at creation time, offers no way to unbind, and would still leave Choirs unreachable for an incarnation whose roster was never built. We chose the sub-resource.

## Amendment (2026-07-30, NIM-330): the declared role is a Voice, not a spec field

**What changes.** The "Decision" above named `incarnation.spec.hosts[].role` as the home of the **declared** role. That field is **removed**. A declared role is now `voice.role` — an attribute of a host's membership in a Choir ([ADR-044 amendment 2026-07-30](0044-choir.md#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)). Nothing else in this ADR moves: the declared/actual split, "the volatile does not live in Soulprint", "essence is role-agnostic", and the rule that the **actual** role comes only from a live probe + `register:` + `where:` are all unchanged.

**Why the field could go.** [ADR-044](0044-choir.md#adr-044-choir--named-host-topology-within-an-incarnation) item 2 had already ruled that Choir absorbs the declared role, keeping `spec.hosts[].role` only as a fallback for bootstrap-`create` — the one moment where, at the time, no Voice could exist yet. The keeper-side core module `core.choir` closed that hole: a create scenario writes its Voices as an ordinary `on: keeper` step, before any host-facing step reads a role. So the fallback stopped covering anything, and a field that covers nothing but still shows up in `GET /v1/incarnations/{name}` is a false statement about the system.

**Consequence for this ADR's own wording.** Wherever this file says the declared role "lives only in `incarnation.spec.hosts[].role`", read: lives only in `incarnation_choir_voices.role`. A host with no Voice has **no declared role** — an empty value, not a default one, symmetric to the way this ADR already treats an unlabelled host on the coven axis.

**What an author writes instead.** `module: core.choir.present` with `on: keeper` inside the create scenario (params `incarnation` / `choir` / `sid` / `role` / `position`), or `POST /v1/incarnations/{name}/choirs/{choir}/voices` day-2. Both existed before this amendment; they are now the only ways.

## Amendment (2026-08-03, NIM-410, [ADR-0082](0082-service-vars.md)): the assembly order collapses to one lexical layer

The clause **"essence is role-agnostic — there is no `role/<Y>.yaml` stage"** stands, and so does the
reason for it: a role is volatile and belongs to a probe, not to a directory name. What changes is
everything else in the sentence that carried it.

The order this ADR fixed as `default → os → coven → incarnation.spec` no longer exists. The layer
is now **service vars** — `<service>/vars/`, read as `vars.*` — and its default order is every
`*.yaml`/`*.yml` directly inside that directory, sorted lexically from `00-base.yaml`. The three
non-default rungs are gone for two reasons:

- **`os/<family>.yaml` and `coven/<label>.yaml`** were implemented and used by nothing outside the
  resolver's own unit tests — no shipped example, no external destiny repo. Conditional assembly is
  now written explicitly in `vars/_stack.yaml` (which this ADR's line 13 already referenced, and
  which until NIM-413 existed only in prose), so there is one mechanism for it rather than two.
- **`incarnation.spec.essence`** is removed outright: it had two readers and no writer. A fleet that
  needs different defaults **forks the service repo** and re-pins its `ServiceRef`.

**The coven axis of service vars survives, and it was never the inherited one.** An incarnation's
label still selects overlays of **its own** config — through a `foreach:` step over
**`incarnation.covens`**, the row's own declared tags. Not `soulprint.self.covens`: the step context
has no `soulprint` root at all, and it must not, because a service's vars are resolved once per run
and handed to every host ([ADR-0082 §3](0082-service-vars.md#3-the-os-and-coven-layers-are-deleted-vars_stackyaml-becomes-real)).
The old coven layer nominally read a host's labels — but on the keeper path it was already being
handed `inc.Covens` under that name, so the incarnation's own tags are what it resolved from in the
case this guard covers. That is why [NIM-281](#amendment-2026-08-05-nim-281-a-label-is-never-inherited)
leaves this axis untouched while deleting inheritance everywhere else: what decides the overlay is a
label on the incarnation, applied to the incarnation's config. Membership decides WHICH incarnation's
config a host is owed; the incarnation's labels decide which overlays of that config apply. The live
guard (`TestIntegration_TelemetryIncarnationCovenSelectsServiceVarsOverlay`, NIM-248) is retargeted
onto that step, not dropped.

**Narrowing worth naming:** a tag attached to a HOST does not select a service-vars overlay, and
after NIM-281 nothing carries a host's tag into that layer at all. Nothing shipped did that (no
example carried a `coven/` overlay), and it is the price of a layer that is host-invariant by
construction rather than by a representative host.
