# ADR-028. RBAC-storage → Postgres

> **This is design, no implementation (Phase 0 — documentation).** The ADR fixes moving the RBAC store from `keeper.yml` into Postgres and closes BUG-1 (init does not write membership to where the enforcer reads it from). Code for the new schema (migrations `rbac_*`, DB-resolve in the enforcer, init-membership) is not in the repository — a phased implementation Phase 0 → 4 below.

- **Context.** Currently the RBAC model is split between two stores:
  - **Archons** live in Postgres (the `operators` registry, [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).
  - **Roles, their permissions and the "operator ↔ role" binding** are declared in the `rbac:` block of `keeper.yml` ([rbac.md](../keeper/rbac.md), [config.md](../keeper/config.md)): `roles[].name` / `roles[].permissions` / `roles[].operators`.

  This gives **BUG-1 — self-lockout of the first Archon**. `keeper init` ([ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) creates the first Archon in Postgres (an `operators` record) and puts `roles: ["cluster-admin"]` into the JWT claim ([ADR-014(b)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). But the enforcer resolves the "AID → roles" membership **not from the JWT claim and not from the DB, but from `keeper.yml::rbac.roles[].operators`** — where `keeper init` writes nothing (it does not edit the YAML on disk). Result: the freshly issued bootstrap Archon gets `403` on the very first API/MCP call — the enforcer has not a single role bound to its AID. The cluster-admin, created to unlock the cluster, is itself locked out. The JWT claim `roles` turned out to be **informational**, not the authoritative source of membership.

  The root of the bug is the **absence of a persistent membership layer**: "operator ↔ role" is nowhere materialized as a record seen by both `keeper init` and the enforcer on all nodes of the cluster. The YAML list `roles[].operators` is seen only by the instance whose `keeper.yml` contains it, and only after a manual edit of the file + reload. In parallel, the YAML model contradicts [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) ("identity in Postgres for hot-add + audit + working FKs"): half of identity (Archons) in the DB, half (membership + roles) in a file.

  The user's decision — **move all RBAC (roles, permissions, membership) into Postgres**, keeping `operators` as is. Propose-and-wait over all names passed.

- **Decision.**

  **(a) Storage — three PG tables with the `rbac_*` prefix.**

  | Table | Role |
  |---|---|
  | **`rbac_roles`** | Role catalog. `name` PK (kebab-case, CHECK on format), `description`, `builtin BOOL` (protection against `role.delete` / `role.update`), `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able (NULL — seed roles without an initiating Archon). |
  | **`rbac_role_permissions`** | Role permissions. `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `permission TEXT` (stored as a **RAW string**, parsed by the existing `ParsePermission` — [rbac.md → Permissions format](../keeper/rbac.md#формат-permissions)), PK `(role_name, permission)`. |
  | **`rbac_role_operators`** | **Membership** "role ↔ operator" — the thing that was missing (the cause of BUG-1). `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(role_name, aid)`. |

  A permission is stored as a raw string (not decomposed into columns) — parsing and matching stay in Go (`ParsePermission`), the DB does not interpret the table. This keeps the [rbac.md](../keeper/rbac.md) grammar unchanged.

  **(b) cluster-admin — seed migration (E1) + `builtin`.** The seed migration inserts the role `cluster-admin` with `builtin=true` and a single permission `*` into `rbac_role_permissions`. `builtin=true` forbids `role.delete` / `role.update` over it (a built-in role is not editable and not deletable — see (e)). This is **E1** — the role with its permissions exists in the DB before any `keeper init`.

  **(c) `keeper init` writes only membership (fix for BUG-1).** In its advisory-lock transaction ([ADR-013(e)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)) `keeper init`, after creating the `operators` record of the first Archon, adds **one membership row** into `rbac_role_operators`: `(cluster-admin, <aid>)`. The role and its permissions already exist from the seed (b). This is exactly the fix for BUG-1: membership lives where the enforcer reads it from (the DB), not in the JWT claim, which the enforcer did not read. The JWT claim `roles` ([ADR-014(b)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) stops being a source of membership — the authority for membership is exclusively `rbac_role_operators`.

  **(d) Enforcer — in-memory snapshot from the DB + B2-invalidation.** The `PermissionChecker.Check` interface does **not** change — `Check` still does NOT go to Postgres on every request. What changes is the source of the snapshot: instead of parsing `keeper.yml::rbac` the enforcer builds a `map[AID][]*Role` snapshot with three SELECTs (`rbac_roles` ⋈ `rbac_role_permissions` ⋈ `rbac_role_operators`). Snapshot update — **B2 (implemented)**: in-memory cache + Redis pub/sub invalidation via the topic **`rbac:invalidate`** (a role/membership mutation on any node publishes an envelope `{origin_kid, at}` → all nodes re-read the snapshot; **self-filter by KID** — the publishing node ignores its own signal, the `applybus` pattern) + TTL-fallback (a periodic re-read in case the signal is lost, by analogy with the `Summons` poll-fallback [ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)). This removes the old RBAC dependency on the hot-reload-config path ([ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)): RBAC is no longer re-read on `SIGHUP` / config-swap, but reacts to a DB mutation.

  **(e) API/MCP permissions — four role operations + a membership pair.** RBAC becomes manageable through OpenAPI/MCP (like Archons in [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), not by editing YAML:
  - `role.create` — create a role (+ its permissions).
  - `role.delete` — delete a role (cascading permissions + membership; forbidden over `builtin=true`).
  - `role.list` — list roles with permissions and membership.
  - `role.update` — change a role's permissions (forbidden over `builtin=true`).
  - `role.grant-operator` — add membership `(role, aid)` into `rbac_role_operators`.
  - `role.revoke-operator` — remove membership.

  The names are fixed by the user (propose-and-wait passed), added to [naming-rules.md](../naming-rules.md) and the catalog [rbac.md → Permissions catalog](../keeper/rbac.md#каталог-permissions).

  **(f) Self-lockout invariant extended to the new paths.** The invariant [ADR-013(c)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) / [rbac.md](../keeper/rbac.md) "the cluster cannot be left without an active Archon holding an effective `*`" is now checked on three new paths:
  - `role.delete` over `cluster-admin` (or any role providing the only path to `*`);
  - `role.update` removing the permission `*` from a role through which the only effective `*` is held;
  - `role.revoke-operator` removing the last active AID with an effective `*`.

  All three, before the mutation, check "after the operation there will remain ≥ 1 active (`revoked_at IS NULL`) AID with an effective `*` permission"; otherwise `409 would-lock-out-cluster` ([naming-rules.md → Error codes](../naming-rules.md#error-codes)).

  **(g) Config hard-cut.** The `rbac:` block is **removed** from `keeper.yml` — the types `config.KeeperRBAC` / `config.RBACRole` / the field `KeeperConfig.RBAC` are gone, the key `rbac:` starts being rejected by the parser as `unknown_key` ([naming-rules.md → Error codes](../naming-rules.md#error-codes)). There are no legacy installations (there is no RBAC-resolve code yet — Phase 0), no YAML→DB migration layer is needed. `soul.yml` is not affected — a Soul has neither PG nor RBAC.

  **(h) Scope.** This ADR is **RBAC only**. Moving the remaining sections of `keeper.yml` into the DB (`services[]` / `default_*_source`, etc.) is **NOT** in this decision; a candidate for a future separate ADR (config→DB for other managed entities), not fixed here. *(Closed by [ADR-029](0029-service-registry.md#adr-029-реестр-service-ов--postgres): the Service registry + `default_destiny_source` moved into Postgres by the same pattern.)*

- **Rationale.**
  - **Alignment with [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon).** "Identity in Postgres → hot-add + audit + working FKs" now extends to all of RBAC: membership and roles get real PG FKs (`rbac_role_operators.aid` → `operators(aid)`, `created_by_aid` / `granted_by_aid` → `operators`), hot-add of a role/binding through the API without editing files, audit of mutations via FK + audit-events.
  - **Alignment with [ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) (stateless cluster).** The shared Postgres is visible to all nodes — membership is the same across the whole cluster without rolling YAML out to instances. The YAML model gave per-host divergence (one instance knows a role, another does not until its file is edited).
  - **Security first.** Revocation of a role / binding — a `DELETE` in the DB + Redis invalidation, the effect is immediate on all nodes. In the YAML model, revocation required editing the file + reload on each instance. This removes the window "the operator should no longer have access, but the file has not been rolled out yet". (Separate from the non-revocability of an active JWT until `exp` — [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon); a membership-DELETE removes the role for all future checks immediately.)

- **ADR reconciliation.**
  - [ADR-013(c)](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) — `keeper init` writes membership into `rbac_role_operators` (DB), not into YAML; the previous wording "including the AID into `roles[].operators` via FK" is refined: membership = a row in `rbac_role_operators`.
  - [ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) — RBAC-storage moved to the DB; the term "FK" is disambiguated: a real PG FK (`created_by_aid`, `rbac_role_operators.aid`) vs. the previous metaphorical "FK" of the YAML list `roles[].operators` (now — a **membership** row in `rbac_role_operators`). The JWT claim `roles` stops being a source of membership.
  - [ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) — RBAC is **excluded** from the hot-reload-config path: an RBAC reload = a DB mutation + Redis invalidation (d), not `SIGHUP` / config-swap. (The section [ADR-021 Consequences](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) mentioned `keeper/internal/rbac.Holder` as an eager-rebuild consumer of the config-reload — after ADR-028 the RBAC holder is rebuilt from DB invalidation, not from the config callback.)

- **Consequences.**
  - **Three new PG tables** `rbac_roles` / `rbac_role_permissions` / `rbac_role_operators` ([keeper/storage.md](../keeper/storage.md) is extended).
  - **Six new permissions** `role.create` / `role.delete` / `role.list` / `role.update` / `role.grant-operator` / `role.revoke-operator` ([naming-rules.md](../naming-rules.md), [rbac.md → Permissions catalog](../keeper/rbac.md#каталог-permissions)).
  - **Extension of the self-lockout invariant** to `role.delete` / `role.update` / `role.revoke-operator` (f).
  - **Config hard-cut** — removal of `config.KeeperRBAC` / `config.RBACRole` / `KeeperConfig.RBAC`; `rbac:` → `unknown_key`. The example [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) loses the `rbac:` block.
  - **Enforcer** is rebuilt onto a DB snapshot + B2-invalidation; the `PermissionChecker.Check` interface is unchanged.
  - **Operator API / MCP** get `role.*` endpoints ([operator-api.md](../keeper/operator-api.md), [mcp-tools.md](../keeper/mcp-tools.md)) — normalizing the HTTP facade / MCP-tool mapping is a separate task at implementation.
  - **Audit-events** `role.created` / `role.deleted` / `role.updated` / `role.operator_granted` / `role.operator_revoked` — added to [naming-rules.md → Audit-events](../naming-rules.md#audit-events) at write-path implementation (category `api` / `mcp`, [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); in this ADR the names are not fixed normatively.

- **Phasing.**
  - **Phase 0 — documentation** (this ADR + amend ADR-013/014/021 + naming-rules + rbac.md). No code.
  - **Phase 1 — migrations `rbac_*` + seed cluster-admin (E1) + enforcer on a DB snapshot (without B2) + init writes membership.** Closes **BUG-1**.
  - **Phase 2 — Operator API / MCP `role.*` + extended self-lockout (f).**
  - **Phase 3 — Redis pub/sub snapshot invalidation (B2) — implemented** (topic `rbac:invalidate`, envelope `{origin_kid, at}`, self-filter by KID; TTL-poll kept as a fallback).
  - **Phase 4 — remove `config.KeeperRBAC` / `RBACRole` / `KeeperConfig.RBAC`, `rbac:` → `unknown_key` (g).**

- **Trade-offs.**
  - **A permission is stored as a RAW string, not decomposed into columns.** The DB does not validate and does not match a permission (that is done by `ParsePermission` in Go). The cost — you cannot SQL-filter by `<resource>`/`<action>`; the gain — the [rbac.md](../keeper/rbac.md) grammar is not duplicated into the DB schema, extending the grammar does not require a table migration. Accepted.
  - **B2-invalidation (Redis pub/sub) — best-effort + TTL-fallback.** Loss of the signal → a node sees a stale snapshot until the next TTL re-read (a short window). The same compromise as `Summons` ([ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)); acceptable for RBAC (role mutations are rare, the window is small). Strict immediate consistency of all nodes is rejected as excessive.
  - **Hard-cut without a YAML→DB migration.** Accepted deliberately — there is no RBAC-resolve code yet (Phase 0), no legacy installations. If installations existed — a one-time importer of the `rbac:` block would be needed; it is not needed here.

- **Amendment (2026-06-09, Synod — a group of Archons [ADR-049](0049-synod.md#adr-049-synod--группа-архонов)).** Between "operator" and "role" an intermediate level is introduced — the **[Synod](0049-synod.md#adr-049-synod--группа-архонов)** (a group of Archons bundling roles). RBAC-storage is extended with **three PG tables** by the same `rbac_*` pattern:
  - **`synods`** — the group catalog (`name` PK kebab-CHECK, `description`, `builtin`, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able). Symmetric to `rbac_roles`.
  - **`synod_operators`** — membership "Synod ↔ Archon" (`synod_name` FK→`synods(name)` CASCADE, `aid` FK→`operators(aid)`, `added_at`, `added_by_aid` FK→`operators(aid)` NULL-able, PK `(synod_name, aid)`). Symmetric to `rbac_role_operators`.
  - **`synod_roles`** — bundle "Synod ↔ role" (`synod_name` FK→`synods(name)` CASCADE, `role_name` FK→`rbac_roles(name)` CASCADE, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(synod_name, role_name)`). CASCADE on both sides.

  **The enforcer's snapshot build (d) is extended** with two SELECTs (`synod_operators` ⋈ `synod_roles`): the membership-map `AID → []*Role` unions direct roles (`rbac_role_operators`) with roles via all of an Archon's Synods **before the snapshot is fixed**. The `PermissionChecker.Check` interface and the matching layer do **not** change — a Synod is invisible below the snapshot build. Redis invalidation (topic `rbac:invalidate`) covers mutations of a Synod / its membership / its bundle by the same path as role mutations. **The self-lockout invariant (f) is extended** to the Synod paths (`synod.delete` / `synod.revoke-role` / `synod.remove-operator`): the check "there will remain ≥ 1 active AID with an effective `*`" unrolls Synod-membership (an effective `*` may come through a Synod). Full fixing — [ADR-049](0049-synod.md#adr-049-synod--группа-архонов).
