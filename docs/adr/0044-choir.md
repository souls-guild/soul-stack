# ADR-044. Choir — named host topology within an incarnation

> **Status: implemented (S-T2…S-T6 part(1a) + the `spec.hosts[].role` half of the S-T6 remainder).** Migration `060_create_choirs.up.sql` (tables `incarnation_choirs` + `incarnation_choir_voices`), packages `keeper/internal/choir` (CRUD) and `keeper/internal/topology` (Voice role resolver — Voice is now the SOLE source, see the amendment 2026-07-30), REST `/v1/incarnations/{id}/choirs/*` with `RequirePermissionMulti`, RBAC permission `choir` (`keeper/internal/rbac/catalog.go`), OpenAPI endpoints `/choirs` + Choir/Voice schemas, keeper-side core module `core.choir` (`keeper/internal/coremod/choir`) + wire-up `ChoirDB`, B3 integration test NULL-role Voice (`topology/choir_nullrole_integration_test.go`). Remaining: S-T1 (typed input `source:`/`format: sid`) and the other half of the S-T6 remainder (`on: [choir]`) — open (slice map at the end of the ADR).

**Context.** By 2026-05-29 "who stands where within an incarnation" is smeared across two different mechanisms without an explicit topological axis:
- **membership** ("which hosts belong to the incarnation at all") = `incarnation.name` in `souls.coven[]` (already works; the `on:` resolver omitted → the whole incarnation via the predicate `$1 = ANY(coven)`);
- **declared role** of a host (`incarnation.spec.hosts[].role`) — the only place of declared topology, designed for bootstrap-`create`, where a probe is impossible ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)).

What is missing: a **named position of a host within an incarnation** — an analogue of a named host-group like `[redis_nodes_1]` / `[haproxy_frontends]`. The operator cannot declaratively say "these three SIDs are the `redis_primary` part, these two are `redis_replica`" and then target by that part. `spec.hosts[].role` gives only a one-dimensional label on a host, without a named group as an entity, without CRUD, without `where:` targeting.

**Decision (fixed by the user on 2026-05-29).** Introduce a first-class entity **Choir** — a named group of hosts within a single incarnation (the topological "choir part"), and **Voice** — the membership of a specific SID in a specific Choir.

1. **Three DIFFERENT layers — do not duplicate.** Choir does not collapse into either membership or coven:
   - **membership** = `incarnation.name` in `souls.coven[]` (as before; do not touch);
   - **coven** = stable logical tags ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only): cluster / project / environment / data center);
   - **Choir** = a named position of a host WITHIN an incarnation. **Choir ≠ coven** (otherwise this would bring back the removed sub-coven `{incarnation.name}-{role}` — a direct conflict with [ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)) and **≠ membership** (Choir is a subset-role within already-members, not the fact of membership itself).

2. **Choir absorbs `spec.hosts[].role` (locked).** The declared role becomes an attribute of membership — `voice.role`. `soulprint.hosts[].role` is fed from Choir (via Voice), not from `spec.hosts[].role`. `incarnation.spec.hosts[].role` is a **degenerate case / deprecated**: it remains for wire/spec compatibility and bootstrap-`create`, but the sole source of declared topology becomes Choir. A single source of truth for declared topology — the duality "role in spec.hosts vs. somewhere else" is eliminated. **[AMENDED 2026-07-30 (NIM-330): the retained half is gone too — `spec.hosts[]` is removed outright, Voice is the only source. See the amendment at the end.](#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)**

3. **Multi-incarnation membership (locked).** One host (one SID) can belong to **several incarnations simultaneously** — this is permissible and already supported (`souls.coven[]` is an array; host A can carry both `service-haproxy` and `redis`, both incarnations contain A). A Voice is tied to the **triple `(incarnation_name, choir_name, sid)`**, so one SID can legally be a Voice in Choirs of **different** incarnations simultaneously. **Invariant:** a Voice is created only for a SID that is **already a member of that incarnation** (its `souls.coven[]` contains `incarnation.name`) — a Voice does not replace membership, but refines the position within it.

4. **Source of truth — separate PG tables + CRUD API, NOT `incarnation.state` (sketch; implementation — S-T2).** The final column names and SQL are fixed in S-T2 (the pilot does not break). At S-T0 — the skeleton:
   - **`incarnation_choirs`** — the declared group: `incarnation_name` (FK), `choir_name`, `description`, `min_size` / `max_size` (optional part-size constraints), `created_by_aid` (FK `operators(aid)`), timings.
   - **`incarnation_choir_voices`** — SID membership in a Choir: `incarnation_name`, `choir_name` (FK to the pair above), `sid` (FK `souls`), `role` (nullable — the absorbed declared role), `position` (nullable — the ordinal index within the part, e.g. the seed node), `added_by_aid` (FK `operators(aid)`), timings.
   - **Why not `incarnation.state`.** `state` is committed **only under a cross-host barrier** ([ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)) — the topology cannot be fixed there without a full run. `state` remains for the **actual result** (who is factually master after apply). **Declared topology (Choir) ≠ actual state**: Choir declares "how it SHOULD stand", `state` records "how it turned out". Mixing them would bring back the volatility problem from [ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only).

5. **Targeting — approach E1 (locked default).** Choir is a **stable per-host fact** (like coven), available in `where:` predicates and in the scenario accessors `soulprint.hosts[].choirs` / `soulprint.self.choirs` (the additive field `choirs[]`, the list of the host's Choir names in the current incarnation). **`on: [choir]` is NOT introduced in the MVP** — the `on:` resolver knows only coven labels ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)), and we do not touch its contract. Narrowing by Choir is done via `where:` (`'redis_primary' in soulprint.self.choirs`), which is symmetric to the probe-role from ADR-008, but without a probe (Choir is stable). `on: [choir]` is an optional extension of S-T6.

6. **Topology editing — hybrid.** Two ways to change Choir/Voice, symmetric to the already-existing mechanisms:
   - **CRUD API outside a run** — like `soul.coven-assign` / `PATCH /hosts` (the operator edits the topology directly via OpenAPI/MCP);
   - **keeper-side core module (edit-within-scenario, `on: keeper`)** — the keeper-side core dispatcher is already implemented ([ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list), [ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read), [docs/keeper/modules.md](../keeper/modules.md)). This is exactly the previously deferred `core.incarnation.member` pattern. **The module name is propose-and-wait at S-T5, not fixed in this ADR.**

7. **The deferred membership epic is not launched separately.** The separately discussed epic "management of host membership in an incarnation" is **functionally absorbed by Choir**: membership remains `souls.coven[]`, and the named position within it is Choir/Voice. We do not introduce a separate entity for membership.

8. **Typed input — a separate early slice (S-T1, independent).** For selecting Choir parts in scenarios, a typed input layer is needed: a field descriptor with a catalog source (e.g. the list of an incarnation's SIDs) + a `sid` format validator + `min_items` / `max_items` limits. The key names (candidates `source:` / `format: sid`) are **propose-and-wait at S-T1**, not fixed in this ADR (the sub-question remains open).

9. **Slice map (incremental rollout).**

| Slice | Content | Pilot does not break |
|---|---|---|
| **S-T0** ✅ DONE | This ADR + amendments + dictionary (documentation). | yes |
| **S-T1** _(remainder)_ | Typed input (`source:` + `format: sid` + `min_items`/`max_items`; key names propose-and-wait). Independent of the Choir tables. | yes |
| **S-T2** ✅ DONE | Tables `incarnation_choirs` / `incarnation_choir_voices` (migration 060) + CRUD API (`keeper/internal/choir`). | yes |
| **S-T3** ✅ DONE | RBAC `choir.*` (`keeper/internal/rbac/catalog.go`) + audit `choir.*` + OpenAPI (routes `RequirePermissionMulti`). | yes |
| **S-T4** ✅ DONE | Resolver `choirs[]` in `soulprint.hosts` / `soulprint.self` (`keeper/internal/topology`, E1 targeting via `where:`). | yes |
| **S-T5** ✅ DONE | keeper-side core module for topology editing **`core.choir`** (`keeper/internal/coremod/choir`) + wire-up `ChoirDB` — implemented, the name is fixed (sub-question closed). | yes |
| **S-T6 part(1a)** ✅ DONE | Absorption of `spec.hosts[].role` in code: the resolver takes role from Voice with fallback to spec (see the amendment below). | yes |
| **S-T6 part(1b)** ✅ DONE | `spec.hosts[]` REMOVED (not deprecated) — the resolver has no fallback, Voice is the sole source (amendment 2026-07-30, NIM-330). | yes |
| **S-T6** _(opt., remainder)_ | `on: [choir]` (the `on:` resolver). | yes |

**Rejected alternatives.**
- **(a) Choir = coven label.** Would bring back the removed sub-coven `{incarnation.name}-{role}` ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)) — a direct conflict. Coven is a global stable axis; Choir is intra-incarnation.
- **(b) Choir in `incarnation.state`.** state is committed only under a barrier ([ADR-009 §7](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)) — the topology cannot be edited without a run; mixes declared with actual (point 4).
- **(c) `on: [choir]` right away in the MVP.** Extends the `on:` resolver contract (which knows only coven). E1 (`where:` targeting) gives the same expressiveness without editing the resolver; `on: [choir]` is deferred to S-T6.
- **(d) A separate membership epic in parallel.** Would duplicate Choir; absorbed (point 7).

**Open sub-questions (propose-and-wait, NOT fixed by this ADR).**
- Key names of the typed input (`source:` / `format: sid`) — at S-T1.
- ~~The name of the keeper-side core module for topology editing — at S-T5.~~ **Closed (S-T5):** the module is `core.choir` (author forms `core.choir.present` / `core.choir.absent`, symmetric to the present/absent of the other core modules).
- RBAC permission names of the `choir.*` family — at S-T3.

**Amendment (2026-05-29, precedence role + multi-Choir conflict — S-T6 part(1a)).** The implementation of declared-role absorption in the resolver (`keeper/internal/topology`) fixes two rules not previously specified in point 2:

- **(a) Precedence role:** `voice.role` (from `incarnation_choir_voices`) **>** `spec.hosts[].role`. For each host in the roster the resolver takes role from Voice; `spec.hosts[].role` remains the **fallback** for hosts WITHOUT a Voice (bootstrap-`create`, wire compatibility) and for a Voice with empty/`NULL` role. `voice.role` is nullable (migration 060 — `TEXT` without `NOT NULL`): an omitted role is written as SQL `NULL` and treated as "no role" → fallback to spec (not an error). **[SUPERSEDED 2026-07-30 (NIM-330): there is no second tier to fall back to. A Voice with an empty/`NULL` role, and a host without a Voice, both resolve to an empty role.](#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)**
- **(b) Multi-Choir role conflict:** `HostFacts.Role` is a scalar, but one SID can legally be a Voice in several Choirs of **one** incarnation with different non-empty roles. Deterministic rule: role is taken from the **first Choir with a non-empty role sorted by `choir_name`** (`ORDER BY choir_name ASC` in the SQL selection of Voices; Choirs with empty/`NULL` role are skipped) + a **WARN log** about the conflict (SID, the chosen/conflicting Choir and role). If roles are empty in all of a SID's Choirs — fallback to spec (point (a)). The scalar semantics of a host's role and the order of names within `soulprint.hosts[].choirs` are thereby deterministic.

**Amendment (2026-06-30, scenario-driven layout + NULL-vs-default semantics).** Fixes the source of Choir assignment and the behavior when no assignment is present — for the mongo use case (6–9 VMs of different roles: shards / coordinators / management nodes are deployed separately by group). Previously (points 4, 6) only the CRUD/keeper-side mechanism was declared, but not the source-declaration in the service scenario and not the semantics of an empty assignment.

- **(a) Scenario-driven source (declared in the service scenario).** The layout of hosts across Choir groups is **described directly in the service scenario**: the scenario declaratively says "this host → into such-and-such part" (the primary place hosts are laid out across groups during deployment). The assignment is **optional, per-host**. This refines point 6: besides day-2 editing (CRUD API outside a run / keeper-side `core.choir` in a scenario), the scenario declaration is the **primary** way to lay out the topology when deploying a service. **The mechanics of per-shard / per-role layout in a scenario (which scenario key describes a host's Choir, the declaration form) is NOT fixed in this ADR — propose-and-wait at implementation.**

- **(b) Not set → `NULL` in the DB, NOT a default group (locked).** If Choir/Voice is **not set** for a host (the scenario did not lay the host into a part and the operator did not create a Voice manually) — there is **no row** in `incarnation_choir_voices` for that SID, `soulprint.self.choirs` / `soulprint.hosts[].choirs` for it is an **empty list**, and the role (`HostFacts.Role`) resolves via fallback to `spec.hosts[].role` or stays empty (per amendment 2026-05-29 (a)). **The host does NOT fall into any "default"/"standard" part by default** — the empty state is preserved as is. **[AMENDED 2026-07-30 (NIM-330): with `spec.hosts[]` gone the alternative is gone with it — such a host's role is empty, full stop. This is now the ONLY rule, not the second branch of one.](#amendment-2026-07-30-nim-330-spechosts-is-removed-voice-is-the-only-source-of-a-declared-role)**
  - **Rationale:** the empty state is **more honest** than a default — it does not impose on the host a group the operator did not choose, and does not create the illusion of declared topology where there is none. A default part would hide the fact "this host is unmarked anywhere" and would require a reserved part name (an extra entity, a conflict with the service's real Choir names). The NULL semantics is symmetric to `voice.role` (point (a) of amendment 2026-05-29: an omitted role = SQL `NULL` = "no role", not a default) and `soulprint.hosts[].role` (may be `null` for hosts outside the declared spec, [ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)).
  - **Partial assignment.** Some hosts with a Choir, some without → the marked ones live in their parts, the rest are `NULL` (empty `choirs[]`). A mixed state is the norm, not an error.

- **(c) Implementation status: per-shard/per-role layout — DEFERRED until mongo.** Scenario-driven layout across parts is truly needed for **mongo** (separate deployment of shards / coordinators / management nodes). For **redis**, Choir is used "for convenience", per-shard layout is **not required**: for now the redis cluster lives in a single part = one Coven `incarnation.name` ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only), the roster of incarnation hosts without splitting by Choir). This amendment fixes the **semantics** (scenario-driven source + NULL-vs-default); **the implementation of the scenario declaration of layout across parts is planned/deferred** (post-redis, by the time the mongo service appears). The Choir/Voice infrastructure (tables, CRUD, `core.choir`, the `choirs[]` resolver) is already implemented (S-T2…S-T6 part(1a)) and covers day-2 assignment; the missing part is precisely the scenario declaration at deploy time.

## Amendment (2026-07-30, NIM-330): `spec.hosts[]` is removed, Voice is the only source of a declared role

Closes the `spec.hosts[].role` half of the S-T6 remainder — and closes it by **deletion**, not by a deprecation marker. The project rule before the first release is that superseded mechanisms are removed whole rather than carried as a compatibility tier.

**What point 2 above kept, and why it no longer holds.** Point 2 absorbed the declared role into `voice.role` but left `spec.hosts[].role` standing for one named reason: bootstrap-`create`, "where a probe is impossible and there are no Choir memberships yet". That reason expired when S-T5 landed. `core.choir` is a **keeper-side** core module (`on: keeper`, the ADR-017 dispatcher) — `core.choir.present` takes `incarnation` / `choir` / `sid` / `role` / `position` and writes a Voice from inside a run, so a create scenario can lay down its topology as an ordinary step before anything reads a role. The declared role therefore has a home on the create path; it simply stopped being a *field on the incarnation record* and became *a step in the scenario that builds the incarnation*.

The two other justifications had already lapsed on their own. "Wire compatibility" is not owed before the first release. And the field never carried the roster: run membership is `incarnation_membership` ([ADR-008 amendment 2026-07-17](0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)), `POST /v1/incarnations` never accepted `spec.hosts`, and no scenario ever wrote it — its only writer in the whole system was the day-2 endpoint `PATCH /v1/incarnations/{id}/hosts`, so on most incarnations the field was simply empty. [ADR-009](0009-scenario-dsl.md) had already recorded the field as "intent, and `spec.hosts[].role` is already degenerate".

**Decision.**

1. **`incarnation.spec.hosts[]` is removed** — the whole array, not just `role`. The key is stripped from the `spec` jsonb of existing rows by migration `108_drop_incarnation_spec_hosts`; `spec` remains freeform, so the removal is a data cleanup, not a schema change. Leaving the key behind was the alternative and it was rejected: `spec` is shown verbatim in `GET /v1/incarnations/{id}` and in the UI, so a stale `hosts[]` would keep advertising a topology that nothing reads — the exact confusion this ADR is closing.

2. **The role resolver has one tier.** `HostFacts.Role` comes from `voice.role` in `incarnation_choir_voices` and from nowhere else. A host with no Voice, and a Voice with an empty/`NULL` role, both resolve to an **empty role** — the rule of amendment 2026-06-30 (b) is now the only rule rather than the second branch of one, and the precedence ordering of amendment 2026-05-29 (a) is moot with a single source. The multi-Choir conflict rule of amendment 2026-05-29 (b) is untouched: it decides among Voices, which still exist.

3. **The operator surface goes with the field.** `PATCH /v1/incarnations/{id}/hosts` is unmounted and drops out of OpenAPI; the permissions `incarnation.update-hosts` and its deprecated alias `incarnation.update` leave the RBAC catalog. That catalog is a **closed enum** and the enforcer is fail-closed: a role still holding either string would abort the snapshot load and take the whole cluster's authorization with it, not merely lose one grant. Migration `109_drop_permission_update_hosts` therefore deletes those grants from `rbac_role_permissions` — bare and scoped forms alike (`… on coven=prod`), which migration 095 missed for its own rename. The audit event type `incarnation.hosts_updated` is retired; the rows already in `audit_log` keep it as free text and are not rewritten.

4. **The declarative replacement is a scenario step, not a field.** To give a host a declared role on `create`:

   ```yaml
   - name: put the seed node into the primary part
     module: core.choir.present
     on: keeper
     params:
       incarnation: "${ incarnation.name }"
       choir: redis_primary
       sid: "${ soulprint.hosts[0].sid }"
       role: master
   ```

   Day-2 the same thing is `POST /v1/incarnations/{id}/choirs/{choir}/voices`. Both paths already existed (S-T2 / S-T5); what changes is that they are now the *only* paths.

**What this does NOT change.** The Choir/Voice model, its tables, its CRUD and its RBAC family are untouched. `soulprint.hosts[].role` / `soulprint.self.role` remain in the scenario context — they are still fed, just from one source. The **actual** role is still a live probe + `register:` + `where:` ([ADR-008](0008-coven-stable-tags.md#adr-008-coven--stable-logical-tags-only)); Choir never replaced it. And amendment 2026-06-30 (c) still stands: what is deferred until mongo is the **declarative per-shard sugar** — a scenario key that lays a whole batch of hosts into parts in one stroke — not the ability to assign a role in a scenario, which `core.choir.present` provides today, one host at a time.

**Trade-off.** Assigning a role on `create` is now a step an author must write, where before it could be pre-loaded onto the incarnation record through a day-2 endpoint. That is the point: the step runs inside the run that builds the topology, so the declaration and the thing it describes cannot drift apart, and there is exactly one place to look for "what role was this host declared to have". The cost is one extra task in a create scenario that wants roles at all — and today none of `examples/` does, because every one of them takes its role from a probe.
