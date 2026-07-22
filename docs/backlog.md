# Backlog of deferred epics

Large epics for which a **decision to postpone** was made (not "open Q", but a conscious pause with a recorded impact). Here is not a design or an ADR, but a brief note: what they wanted, why they postponed it, what pragmatic workaround was adopted, and what will be required when resuming.

Resumption of any epic from here - **separate epic** with consultation from architect and `propose-and-wait` for everything that touches ADR / name dictionary / contracts. This page does not change the "documentation before code" rule.

Related sources of beta and GA limits are [known-limitations.md](known-limitations.md) (which is NOT in beta) and [prod-readiness.md](prod-readiness.md) (GA-gap roadmap). The backlog around module collections lives separately - [module-collections.md](module-collections.md).

---

## Per-service uniqueness of the incarnation name (the name is still a global PK)

**Status:** BACKLOG - postponed by user decision 2026-06-25.

**What they wanted.** Allow **the same incarnation names for different services** (per-service uniqueness): the `prod` incarnation of the `redis` service and the `prod` incarnation of the `postgres` service must coexist. Now this is prohibited.

**Why not now (by-design).** `incarnation.name` is a global PRIMARY KEY (PG migration 005), and this is intentional. Allowing duplicate names requires replacing that PK with a synthetic id and a composite `(service, name)` uniqueness, which cascades across the schema and the API (see impact below).

> **Update 2026-07-17 (NIM-124).** One sub-blocker of this epic is **already resolved**: `incarnation.name` is **no longer a Coven** — incarnation membership moved to the first-class relation `incarnation_membership` ([ADR-008 amendment 2026-07-17](adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). The old cross-service danger ("duplicate names → both services' hosts carry `coven=[prod]` → roster `WHERE 'prod' = ANY(coven)` hooks cross-service hosts on `destroy`") **no longer applies**: the roster is resolved by membership, not by `= ANY(coven)`, and the name never appears in `coven[]`. The remaining reason the name must stay globally unique is purely that it is the global PK — not the coven coupling.

**Impact during implementation (architect assessment):**

- ~~amend [ADR-008](adr/0008-coven-stable-tags.md) - unbind `incarnation.name` from the Coven label~~ — **DONE by NIM-124** (membership is now `incarnation_membership`, the name is not a coven);
- enter a synthetic incarnation-id (PK), and make `(service, name)` composite-unique;
- composite-FK `(service, name)` on >5 tables: `state_history` (006), `apply_runs` (018), `incarnation_choirs` + `incarnation_voices` (060), plus soft-links `decrees` (041), `tides` (055), `voyages` (059), `incarnation_archive` (039);
- breaking change Operator API: path `/v1/incarnations/{name}` → `/v1/incarnations/{service}/{name}` - breaks UI routing, `soulctl` and `types.gen.ts`;
- RBAC revision [Purview](adr/0047-purview.md): the incarnation RBAC scope is now `service=` + declared `covens` + the `incarnation=<name>` dimension (NIM-124 dropped the former `covens ∪ {name}`), and the name is no longer globally unique.

**Related desired end-state (take into account when developing the epic).** The user wants a hierarchy of rights like `<services>.<incarnations>.<other>`; [Trait](naming-rules.md) tags work according to rights **only within the framework of their services** and **narrow** the scope of visibility (first "show service incarnations", then limit traits from above). This intersects with the deferred slice "RBAC-scope by traits" ([ADR-0060](adr/0060-traits.md): RBAC-scope by traits - pilot includes only targeting + metadata, scope is deferred).

**Pragmatic workaround (accepted now, Option A).** The incarnation name remains globally unique; uniqueness is achieved by namespace within the name itself according to the scheme `<NS>-<uniq_name>-redis-<cl|s>` - collected on the SD side / at `create`. The duplicate is caught by the **existing** response `409 incarnation-exists` BEFORE the VM is promoted, that is, before irreversible cloud operations.

**When resuming:** separate epic + amend [ADR-008](adr/0008-coven-stable-tags.md), `propose-and-wait` (unlinking the name from Coven - changing the dictionary invariant and API contract).
