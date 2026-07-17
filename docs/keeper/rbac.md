# RBAC and operators

RBAC is built into Keeper out of the box ([requirements.md](../requirements.md)) and applies to **OpenAPI / MCP / push operations uniformly**: the same set of policies decides whether an operator can start a Soul, pull `push.apply`, read the Soulprint of a specific coven, or create a Provider.

The model is the classic trio "operators (Archons) ↔ roles ↔ permissions." Everything is stored in Postgres ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)): Archons ([ADR-014](../adr/0014-operator-identity.md)) - registry `operators`, roles and their permissions - tables `rbac_roles` / `rbac_role_permissions`, binding "operator ↔ role" (**membership**) - table `rbac_role_operators`. Management - via OpenAPI/MCP (`role.*`-permissions, § Permissions directory), not by editing YAML.

> **Hard-cut: block `rbac:` in `keeper.yml` removed ([ADR-028(g)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** Before ADR-028, roles / permissions / membership were declared in `keeper.yml::rbac` - this gave BUG-1 (`keeper init` creates an Archon in the database, but membership puts in a JWT-claim, which the enforcer does not read, resolving it from YAML → bootstrap - Archon receives `403`). The key `rbac:` is now rejected by the parser as `unknown_key`; types `config.KeeperRBAC` / `config.RBACRole` / field `KeeperConfig.RBAC` removed. There are no legacy installations, YAML→DB migration is not required.

## Storage - three PG tables

RBAC materialized in Postgres ([ADR-028(a)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), schemas - [storage.md](storage.md)):

| Table | Columns | Role |
|---|---|---|
| **`rbac_roles`** | `name` PK (kebab-case, CHECK on format), `description`, `builtin` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able | Directory of roles. `builtin=true` disables `role.delete` / `role.update` (built-in role - `cluster-admin`, see § Built-in Roles). `created_by_aid IS NULL` - seed roles without an Archon initiator. |
| **`rbac_role_permissions`** | `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `permission` TEXT, PK `(role_name, permission)` | Role permissions. `permission` is stored as a **RAW string** and parsed by `ParsePermission` (§ Permissions format) - the database does not interpret the string. |
| **`rbac_role_operators`** | `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able, PK `(role_name, aid)` | **Membership** "role ↔ operator". The absence of this layer used to be the cause of BUG-1 - membership had nowhere to persistently live so that it could be seen by both `keeper init` and the enforcer on all nodes. |

**The term "FK".** The "FK" here is **a real PG foreign key** (`rbac_role_operators.aid` / `granted_by_aid` / `created_by_aid` → `operators(aid)`). The former metaphorical "FK" of the YAML list `roles[].operators` is **membership** (line `rbac_role_operators`), not a link in the file ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), [naming-rules.md → RBAC](../naming-rules.md)).

## Basic Policy

| Concept | Meaning |
|---|---|
| **`default_policy: deny`** | By default, any action not covered by an explicit allow-permission is **prohibited**. No exceptions. This is an enforcer invariant (not a config field after hard-cut). |
| **Role** | Record `rbac_roles` (`name` kebab-case) + set of permissions (`rbac_role_permissions`) + set of bound AIDs (`rbac_role_operators`). |
| **Permission** | Line `rbac_role_permissions.permission`: either `*` (all) or `<resource>.<action>` with optional filter `on <selector>` (§ Format permissions). |
| **Membership** | Line `rbac_role_operators` `(role_name, aid)` - "AID has this role." |

## Example (logic model)

Role `db-operator`, tied to two Archons, with two permissions:

```
rbac_roles:
  name: db-operator        builtin: false   created_by_aid: archon-alice

rbac_role_permissions:
  (db-operator, "incarnation.* on service=redis,vault-cluster")
  (db-operator, "soul.list")

rbac_role_operators:
  (db-operator, archon-db-01)   granted_by_aid: archon-alice
  (db-operator, archon-db-02)   granted_by_aid: archon-alice
```

`db-operator` can perform any operations on the incarnation services `redis` and `vault-cluster` and see the list of Souls; everything else is prohibited. Created via OpenAPI/MCP: `role.create` (role + permissions) + `role.grant-operator` (membership) - § Permissions directory.

## Format permissions

Permission string - either `*` (full access, equivalent to cluster-admin), or a two-level name `<resource>.<action>` with an optional selector `on <selector>`. Formally:

```
permission := "*" | <resource>.<action> ( " on " <selector> )?
resource   := [a-z][a-z0-9-]*
action     := "*" | [a-z][a-z0-9-]*
selector   := <key>=<values> | "regex='" <re2-pattern> "'" | "soulprint='" <cel-predicate> "'" | "trait=" <trait-pair>
key        := "service" | "coven" | "incarnation" | "host"
values     := <value> ( "," <value> )*
value      := [a-zA-Z0-9_.-]+
trait-pair := <value> ":" <value>
```

Rules not expressed by grammar:

- **Exactly two segments.** `incarnation.create` - valid; `keeper.incarnation.create` - invalid (three segments; a name of this type is **MCP-tool name**, not permission).
- **Wildcard только в `<action>`.** Поддерживаются ровно **две** wildcard-формы: `<resource>.*` — все действия одного ресурса (`incarnation.*`) — и полный `*` (без точки) — cluster-admin над всеми ресурсами. `*.create` (wildcard в `<resource>`) и resource-glob (`incarn*.run`) в MVP **не** поддерживаются. Action-wildcard **скоупится своим ресурсом и не течёт на другие**: `incarnation.*` не даёт прав на `service`/`role`/`operator` — `Matches` точно сравнивает `<resource>` (регресс — `permission_test.go`, `subset_test.go` в `keeper/internal/rbac/`).
- **`<resource>.*` = ВСЕ действия ресурса, включая чувствительные.** `incarnation.*` конферит и `incarnation.view-secrets` (раскрытие plaintext-секрета, NIM-74), и `incarnation.destroy` (уничтожение), а `role.*` — и `role.grant-operator` (выдача роли оператору): `<resource>.*` не эквивалентен «read/run по ресурсу», он включает уничтожение, раскрытие секретов и выдачу прав. Редактор ролей в UI (RBAC → Permissions) даёт `<resource>.*` отдельной галкой «все действия» на ресурсе; галка покрывает и будущие действия этого ресурса, поэтому, если полный доступ не нужен, перечисляйте действия явно.
- **Whitespace.** There is exactly one space between `<resource>.<action>` and `on`; between the selector key and value - `=` without spaces; between values - `,` without spaces.
- **Case.** Permission names, selector keys and values are case-sensitive. Canon - lower-case (`coven=`, not `Coven=`).
- **Kebab-case in action.** Hyphen in `<action>` is acceptable (`operator.issue-token`); Space and underscore are not.

Grammar expansion (new selector keys, wildcards in values, new forms) - separate PR in `rbac.md` with justification. Adding new keys is not breaking (old roles continue to be validated).

## Permission ↔ MCP-tool / OpenAPI endpoint

**1:1 correspondence.** Each permission of the two-segment format `<resource>.<action>` controls:

- MCP-tool keeper-side with name `keeper.<resource>.<action>` (4-segment name).
- Corresponding OpenAPI endpoint (POST `/v1/<resource>/<action>` or similar).

Example: permission `incarnation.create` gives the right to call MCP-tool `keeper.incarnation.create` or HTTP endpoint `POST /v1/incarnations`. Full compliance with MCP-tool ↔ OpenAPI endpoint ↔ permission is normalized in [operator-api.md → Mapping endpoint ↔ MCP-tool ↔ permission](operator-api.md#mapping-endpoint--mcp-tool--permission); MCP side (tool declaration format, transport, async-convention, input/output schemas) - in [mcp-tools.md](mcp-tools.md).

## Selector grammar

The selector is a single-variable filter `<key>=<v1>,<v2>,…`, where `<key>` is from closed enum:

| Key | Semantics | Value source for query matching |
|---|---|---|
| `service=` | Permission is limited to incarnations of the specified Service types. | **incarnation-operations:** `incarnation.service` (Service name from git, [architecture.md → Artifacts](../architecture.md)). On create - `service` from the request body. |
| `coven=` | Permission is limited to the specified Coven tags. | **incarnation operations** (run / destroy / upgrade / get / history): declared `incarnation.covens` ∪ `{incarnation.name}` - incarnation env tags plus its name as the root Coven label ([ADR-008](../adr/0008-coven-stable-tags.md), amendment a). On create - declared `covens` from the request body ∪ `{name}`. **soul-operations** (`soul.coven-assign` / `soul.list`): host label from body/query (`souls.coven[]`). |
| `incarnation=` | Permission is limited to a specific instance. | `incarnation.name` target instance. |
| `host=` | Permission is limited to a specific host. **Context source is path/body mutation** (`soul.issue-token` / `soul.ssh-target-update` / `errand.run`), where the SID is known before the handler. On the read-visibility of souls (`soul.list` and the get/soulprint/history covered by it) the `host=` selector at the gate stage is NOT applied - read is gated by existence-`RequireAction`, narrowing by scope is done by handler (ADR-047 §g G1, § Two-layer authorization read-endpoints). | Host SID ([identity.md](../soul/identity.md)) from path/body mutation. |
| `regex='…'` | Permission is limited to hosts whose SID/name matches the RE2 pattern ([ADR-047](../architecture.md) S2a). | SID/hostname - `host`- or `sid`-request context key. |
| `soulprint='…'` | Permission is limited to hosts whose facts satisfy the CEL predicate `soulprint.self.*` ([ADR-047](../architecture.md) S2b, [ADR-018](../adr/0018-soulprint-typed.md)). | Host facts (`SoulprintFacts`) - supplied by list-visibility/target resolver (slices S3/S4). |
| `trait=key:value` | Permission is limited to incarnations whose operator-set trait label `key` is equal to `value` (exact scalar-equality, **not** CEL). ADR-047 amendment / [ADR-060](../adr/0060-traits.md) p. 7 slice 1. **OR-dimension** (several trait-permissions = "either this pair or that"); AND-narrowing on several pairs - follow-up (multi-key). | incarnation trait pair (`incarnation.traits` jsonb) - feeds the resolver `incarnation.list`/`get` (slice 1: `inc.Traits[key] == value`). |

**Multiple values** are listed comma-separated without spaces (`coven=db,cache`), matching is exact-match for each value; OR logic among the values of one selector (`coven=db,cache` = "coven `db` OR `cache`").

**regex key (ADR-047 S2a).** Value - one RE2 pattern (Go `regexp`, consistent with UI `compileSidRegex`) in **single quotes**: `incarnation.run on regex='^web-.*'`. Quotes are required and separate the regex from the `,` value-list delimiter - a comma inside the regex (`{1,3}`) does not break the value; regex special characters do not pass `reSelValue`, so the unquoted form is rejected on load. One pattern per key (multi-regex via `,` is ambiguous with regex comma); union of several regex is typed by several roles/permissions. The pattern is compiled when the image is loaded - a broken regex fails load (as unknown-permission); length is limited to 256 characters (RE2 without catastrophic-backtracking, cap - insurance). Matching - `regexp.MatchString` vs `host`/`sid`-context; request without a host/sid key → deny (like an exact key without its own key). **REAL application** of regex to the visibility of list endpoints and the intersection of targets - S3/S4 slices; in S2a regex participates in `Check`-matching (host-context-endpoints) and least-privilege subset.

**regex in the least-privilege subset.** Covering one regex with another (`^web-` ⊇ `^web-prod-`?) is statically undecidable in the general case. MVP - **string-equality fail-closed**: the caller has the right to issue a regex-permission only if it has `*`, either a matching bare-permission (not limited by regex), or a matching permission with an **identical** regex string. Different/narrower regex → DENY (safe). Containment regex is not implemented.

**soulprint key (ADR-047 S2b).** Value is a single CEL predicate on host facts in **canonical form `soulprint.self.*`** ([ADR-018](../adr/0018-soulprint-typed.md)) in **single quotes**: `incarnation.run on soulprint='soulprint.self.os.family == "debian"'`. Quotes are required and separate the CEL from the `,` value-list delimiter - spaces/double quotes/commas inside the predicate do not break the value; the unquoted form is rejected on load. One predicate per key; union of several - several roles/permissions. The predicate is compiled when loading a snapshot via `shared/cel` (sandbox mode `NewFlowControl`: declared `soulprint.self.*`, prohibited `vault()`/`now()`, host accessor `soulprint.hosts`/`soulprint.where`) - broken/using `vault`/`now`/`state`/`soulprint.hosts` predicate fails load (as unknown-permission); length is limited to 512 characters. **REAL CEL-eval** against `SoulprintFacts` host - S3/S4 slices (visibility of list endpoints / intersection of targets): there the resolver provides facts. In `Check`-matching S2b soulprint-dimension **fail-closed (deny)** - request-context (`map[string]string`) does not carry nested facts of the host; soulprint participates in S2b in grammar, `Purview.SoulprintExprs` and least-privilege subset.

**soulprint in the least-privilege subset.** The logical containment of one CEL predicate by another is statically unprovable. MVP - **string-equality fail-closed** (as regex): the caller can issue soulprint-permission only if it has `*`, either a matching bare-permission (not limited by soulprint), or a matching permission with an **identical** predicate. Another/narrower predicate → DENY.

**trait-key (ADR-047 amendment / [ADR-060](../adr/0060-traits.md) p. 7 slice 1) - IMPLEMENTED.** Value - pair `key:value` (**not** CEL, unlike soulprint/state): `incarnation.run on trait=owner:alice`. The grammar is exactly **one** `:` (key-value separator), both halves are non-empty, and the match is `[a-zA-Z0-9_.-]+` (the same character class as regular exact values; space/colon inside halves is not allowed, so there is no ambiguity "which `:` separator" is). Stored in `Selector["trait"]` by the normalized string `key:value`, not broken. The semantics of match is **exact scalar-equality** against the pair `incarnation.traits` (as `coven=`, not a predicate): permission matches if the incarnation has `traits[key] == value`. One trait per key; **OR-dimension** (several trait-permissions = "either this pair or that" - union, like `Covens`). **★ Slice 1 - OR only; AND-narrowing by several trait pairs (multi-key `trait=a:x,b:y`) - follow-up**, in the current grammar the form `trait=` carries exactly one pair. In `Check`-matching, the trait dimension **fail-closed (deny)** - request-context (`map[string]string`) does not carry the nested-traits of the host; **real match** against incarnation traits is done by the resolver `incarnation.list`/`get` (two-layer authorization, slice 1), trait is involved in the grammar, `Purview.TraitExprs` and least-privilege subset.

**trait in the least-privilege subset.** Exact equality → **string-equality fail-closed** (like regex/soulprint): the caller can issue trait-permission only if it has `*`, or a matching bare-permission (not limited by trait), or a matching permission with an **identical** pair `key:value`. Another/narrower pair → DENY (it is impossible to issue `owner:bob`, having only `owner:alice`; a bare-permission caller covers any trait, but a trait-scoped caller cannot issue bare - bare is broader).

**Multi-value coven for incarnation operations.** The source `coven=` for incarnation is *set* (declared `covens` ∪ `{name}`), and enforcer operates on one value per key. Therefore, permission is allowed if its `coven=` value matches **at least one** label from this set (OR by candidates). Example: incarnation `redis-prod` with `covens=[prod, dc1]` has an effective coven set of `{prod, dc1, redis-prod}`; the role `incarnation.run on coven=prod` matches it, the role `incarnation.run on coven=dev` does not. The name incarnation is always included in the set as the root Coven tag (ADR-008), so the role `incarnation.* on coven=redis-prod` works even for incarnation without env tags.

> **History (ADR-008 amendment a).** Previously, this section declared the source `coven=` for the incarnation "`soulprint.self.covens` target host" - but incarnation endpoints do not resolve hosts at the RBAC gate stage (chicken-egg + volatility), and the code landed in the context only `{incarnation: name}` without `coven`/`service`. Because of this, the roles `incarnation.* on coven=…` / `on service=…` were silently NOT matched (enforcer for the missing key → deny). The source was the stable attributes of the incarnation itself (declared `covens` ∪ `name` + `service`), resolved from its string in Postgres.

**Wildcards (`*`) in values are prohibited in MVP** - the extension requires a separate ADR (escaping forms, semantics for FQDN hostnames, empty value behavior). The parser (`reSelValue = ^[a-zA-Z0-9_.-]+$`) rejects `*` as the value of the selector on the load of the snapshot, so the form `coven=*` **does not exist** as a loaded permission. For `soul.coven-assign` this means: unrestricted-scope (any label, any host) is achieved by **bare**-permission `soul.coven-assign` (without `on coven=…`) or full `*`-permission - not through `coven=*`.

**`namespace=`** (filter by plugin namespace) is not introduced yet - RBAC on plugin-namespace is not included in MVP scenarios. Will appear if necessary.

Extending enum keys - via PR in `rbac.md`; adding new keys does not break existing roles (old rules continue to be validated, new keys are optional).

## Semantics of conflict

- **OR logic among allow-permissions.** An Archon can have multiple roles; permission matches if **at least one** roles[].permissions[] of this Archon satisfies the request. Conflict between roles - union permissions.
- **No deny-permissions.** MVP does not support explicit `deny <permission>`. Any permission is specified by the allow rule; everything else is prohibited `default_policy: deny`. The extension is a separate ADR when a real "allow everything except X" scenario appears.
- **`default_policy: deny`.** Any action not covered by an explicit allow-permission is rejected. This is a built-in invariant of the enforcer ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) - after the hard-cut `rbac:` there is no config field `default_policy`); `allow`-mode is not provided as an option outside of dev/test.
- **Union of selectors of the same permission.** If two roles of the same operator give `incarnation.create on service=foo` and `incarnation.create on service=bar` - the effective selector = `service=foo,bar`.

Algorithm for checking permission request `incarnation.create` with context `{service: redis, incarnation: redis-prod, …}`:

1. Find all Archon roles by membership (`rbac_role_operators` where `aid = <requesting AID>`).
2. Expand to a flat list of permissions (`rbac_role_permissions` by found roles).
3. For each permission: does the `<resource>.<action>` request match (taking into account the wildcard `*` in `<action>`); if there is a selector, does it match the context (the key is in the context, the value from values matches).
4. If **at least one** permission matches → allow. Otherwise → deny.

Steps 1–2 are based on the enforcer's **in-memory snapshot**, not live SQL (see § How an enforcer resolves).

## How the enforcer resolves

Interface `PermissionChecker.Check` **does not go to Postgres for every request** ([ADR-028(d)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) - it matches the in-memory snapshot `map[AID][]*Role`.

- **The source of the snapshot is the database.** The snapshot is built by three SELECTs using `rbac_roles` ⋈ `rbac_role_permissions` ⋈ `rbac_role_operators` (instead of the previous parsing `keeper.yml::rbac`). Permission lines are parsed `ParsePermission` when creating a snapshot.
- **Updating the snapshot - B2 (implemented).** The snapshot is invalidated via Redis pub/sub: each mutation of the role / permissions / membership publishes a signal to the topic **`rbac:invalidate`** (envelope `{origin_kid, at}`), and all nodes re-read the snapshot from the database. **Self-filter by KID**: the node ignores its own signal (pattern `applybus` - the publishing node has already updated the snapshot in the same mutation transaction). **B1 TTL-poll remains fallback**: background goroutine (`rbac.Holder`) rereads the snapshot from the database at a fixed interval (`DefaultRefreshInterval` = 10s) - best-effort insurance in case of Redis being unavailable/lost signal; If the reread fails, the previous snapshot remains + warn. The aging window (seconds until the next TTL reread when the signal is lost) is acceptable: role/membership mutations are rare.
- **Self-lockout checks - from the database, not from the snapshot.** The invariant "≥1 active `*`-admin will remain" (see § Built-in roles) is checked **not** by the enforcer's in-memory snapshot, but by direct SQL under `SELECT … FOR UPDATE` on `rbac_role_operators` / `rbac_role_permissions` / `operators` in the same transaction as the mutation. The snapshot becomes obsolete in the TTL window - a solution to it would give a staleness hole (you can remove the last admin if the snapshot still "remembers" the already-revoked second one); `FOR UPDATE` additionally serializes concurrent lockout operations on different nodes. See § Role Management.
- **RBAC outside the hot-reload-config-path.** The snapshot is rebuilt from the database (Redis pub/sub-invalidation via topic `rbac:invalidate` + TTL-poll fallback), **not** by `SIGHUP` / config-swap ([ADR-021](../adr/0021-hot-reload-config.md), clarified [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Revocation of role/membership (`role.revoke-operator` / `role.delete`) - `DELETE` in the database + re-reads the snapshot, effect for all future checks no later than the TTL window (separate from the irrevocability of the active JWT until `exp` - [ADR-014(d)](../adr/0014-operator-identity.md)).
- **`Check` interface is unchanged** - only the source of the snapshot and the mechanism for updating it change, not the verification signature.

## Role management (REST + MCP)

RBAC-CRUD (roles, permissions, membership) managed via OpenAPI / MCP - Phase 2 [ADR-028(e)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) (not YAML editing). One source of truth is `rbac.Service`: REST handlers (`/v1/roles`) and MCP tools (`keeper.role.*`) - thin transport wrappers over it, business invariants (builtin boundary, self-lockout, name/permission validation) live in the Service.

### REST `/v1/roles`

Six endpoints. RBAC check - in middleware (`role.*`-permission without selector), before the handler. Errors - RFC 7807 ([operator-api.md → Error types](operator-api.md)).

| Method + path | Permission | Body/path | Success | Error codes |
|---|---|---|---|---|
| `POST /v1/roles` | `role.create` | body `{name, description?, permissions[]}` | `201` (body empty) | `403 forbidden` (least-privilege: right outside the caller set); `409 role-already-exists`; `422 validation-failed` (broken `name` / `permission`); `400 malformed-request` |
| `GET /v1/roles` | `role.list` | — | `200 {items: [...]}` | `500 internal-error` |
| `DELETE /v1/roles/{name}` | `role.delete` | path `name` | `204` | `404 role-not-found`; `409 role-builtin`; `409 would-lock-out-cluster` |
| `PATCH /v1/roles/{name}/permissions` | `role.update` | path `name` + body `{permissions[]}` (replace) | `204` | `403 forbidden` (least-privilege: added right outside the caller's set); `404 role-not-found`; `409 role-builtin`; `409 would-lock-out-cluster`; `422 validation-failed`; `400 malformed-request` |
| `POST /v1/roles/{name}/operators` | `role.grant-operator` | path `name` + body `{aid}` | `204` | `403 forbidden` (least-privilege: role contains a right outside the caller's set); `404 role-not-found`; `404 not-found` (AID does not exist); `422 validation-failed` (empty/broken AID); `400 malformed-request` |
| `DELETE /v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | path `name`, `aid` | `204` | `404 not-found` (no pair `(name, aid)`); `409 would-lock-out-cluster`; `422 validation-failed` (broken path-AID) |

- **`GET /v1/roles` items[]** — `{name, description, builtin, permissions[], operators[]}`; `permissions` / `operators` are serialized by a non-nil array (`[]`, not `null`).
- **`grant-operator` idempotent** - re-binding the same pair `(name, aid)` - no-op (`204`).
- **`granted_by_aid`** for grant is taken from the JWT-claim caller.

### MCP `keeper.role.*`

1:1 with REST: `keeper.role.<action>` ↔ `role.<action>` ↔ one endpoint `/v1/roles`. Input schemes and mapping errors RFC 7807 → MCP-tool error - in [mcp-tools/roles.md](mcp-tools/roles.md). Mutating tools return an empty output object (`{}`), `keeper.role.list` - `{roles: [...]}`.

### Self-lockout invariant: four ways

Invariant [ADR-028(f)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / § Built-in roles: after the operation, **≥1 active** (`revoked_at IS NULL`) Archon with an effective `*`-permission must remain in the cluster. Four mutations can break it:

| Path | When the check is triggered | What counts as "survivors" |
|---|---|---|
| `operator.revoke` | Review of the Archon holding `*` ([ADR-013(c)](../adr/0013-bootstrap-archon.md)). | Active `*`-admins, except the revoked AID. |
| `role.delete` | The role being removed gives `*`. | Active `*`-admins through **other** roles (≠ to be deleted). |
| `role.update` | The old role set gave `*`, the new one did not (removing `*`). | Active `*`-admins through other roles. If the new set also gives `*`, no check is needed. |
| `role.revoke-operator` | The role is given `*` and membership is removed. | Active `*`-admins after excluding exactly the pair `(role, aid)` - AID remains if it holds `*` through another role; the role remains for other AIDs. |

Violation → `409 would-lock-out-cluster` (common problem-type for operator and role paths, [naming-rules.md → Error codes](../naming-rules.md#error-codes)). Self-lockout - "downward" protection (you cannot lock admin-set). A separate § Invariant least-privilege; `role.create` / `role.grant-operator` obey it, although self-lockout does not.

**Check - from the database under `FOR UPDATE`, not from the enforcer snapshot** (see § How an enforcer resolves). The control SQL takes a row-lock on `rbac_role_operators` / `rbac_role_permissions` / `operators` in the same transaction as the mutation: excludes the target role/pair from the sample and checks that ≥1 row remains. The snapshot becomes outdated on the TTL window - checking against it would be a hole; `FOR UPDATE` serializes parallel lockout operations (two txs that unlock `*` in different ways cannot both pass).

### Invariant least-privilege (subset-check)

Separate from self-lockout protection - against **vertical escalation of privileges**. Without it, an operator with `role.create` + `role.grant-operator` (but **without** `*`) could: create a role with `permissions: ["*"]` → bind it to himself via `role.grant-operator` → become an effective cluster-admin. That is, the rights to *manage roles* would be converted into *any* rights.

**Invariant: an operator cannot issue permission through a role that it does not itself have.** Three mutations are subject to it:

| Path | What is checked against the effective dialing of a caller |
|---|---|
| `role.create` | **each** permission of the new role. |
| `role.update` | **each ADDED** permission (which was not in the old set). Removing rights is **not** limited - cutting someone else's role is not escalation. |
| `role.grant-operator` | **each** permission **granted role** (otherwise bypass: cluster-admin created a powerful role, suboperator with `role.grant-operator` assigned it to himself/other and rose). |

- **Coverage** - the same implication semantics as `Check` (§ How enforcer resolves): caller "has" permission `P` if at least one of its permissions matches `P` (taking into account `*` → covers everything; `resource.*` → covers any action of this resource; selector `on key=a,b` → caller must cover **every** value). Only the owner of `*` can issue a full-wildcard `*`.
- **cluster-admin (`*`)** passes any such check - its set covers everything.
- **Source of caller set is DB** (same mutation transaction, filter `operators.revoked_at IS NULL`), not enforcer snapshot: same-tx-read fresher than TTL snapshot. Read-only (without `FOR UPDATE`): subset-check is an authorization gate, and not a consistency invariant like self-lockout, so it does not add row-locks and does not affect the deterministic lock order of the self-lockout kernel (no deadlock risk).
- **Bootstrap-grant** (`keeper init`, `granted_by_aid IS NULL`, without caller-Archon) subset-check fails - it binds the first Archon to `cluster-admin` before any subject appears.
- Violation → `403 forbidden` (REST `TypeForbidden` / MCP `forbidden`), sentinel `ErrPermissionNotHeld` - separate from `ErrPermissionDenied` ("no right to the operation itself", checked by middleware/tool before Service).

Self-lockout and least-privilege **coexist**: the first prohibits locking the admin-set "down", the second prohibits granting the "up" right. They check different things and don't conflict in order.

### Builtin-border

`cluster-admin` (`builtin=true`, § Built-in Roles):

- `role.delete` / `role.update` above it - **prohibited** → `409 role-builtin` (builtin check goes **before** self-lockout; builtin is more important).
- `role.grant-operator` / `role.revoke-operator` above it - **allowed** (otherwise you cannot add a second admin or remove an erroneously assigned one), with the same self-lockout on revoke.

## Managing Archon Groups (Synod)

**Synod** ([ADR-049](../adr/0049-synod.md)) - intermediate level of the **Archon → Synod → Roles** model: a group of archons **banding a set of roles**. Instead of granting roles to each archon individually, the operator assembles a group with the required bundle of roles and adds archons to it - all members automatically receive the entire bundle.

- **Synod does NOT carry its own scope.** Scope (selectors `on coven=…` / `on service=…` / `regex` / `soulprint`) lives on roles ([Purview](../adr/0047-purview.md) / [ADR-047](../adr/0047-purview.md)); Synod only groups already-scoped roles. Group-scope ADR-049 is not introduced (additive for the future).
- **Synod flat** - the group does not contain other groups (nesting is an additive extension).
- **An Archon can belong to several groups.**

### Effective roles = direct ∪ via Synod

Effective roles of an archon = **direct** (membership `rbac_role_operators`) **∪** roles through **all his Synods** (`synod_operators` ⋈ `synod_roles`). The union is assembled when constructing an in-memory snapshot of the enforcer (§ How an enforcer resolves) - the matching layer `Check` does not distinguish the source of the role (a role through a direct grant and a role through a group are equivalent). A duplicate role (through a direct grant AND a group, or through two groups) is idempotent - a union of a set, not a multiset.

### Storage - three PG tables (pattern `rbac_*`)

The Synod registry is materialized with the same pattern as `rbac_*` ([migration 069](../adr/0049-synod.md), schemes - [storage.md](storage.md)):

| Table | Columns | Role |
|---|---|---|
| **`synods`** | `name` PK (kebab-case, CHECK `^[a-z][a-z0-9-]*$`), `description`, `builtin` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able | Catalog of groups. `builtin=true` disables `synod.delete` (symmetry `rbac_roles.builtin`). |
| **`synod_operators`** | PK `(synod_name, aid)`; `synod_name` FK→`synods(name)` `ON DELETE CASCADE`, `aid` FK→`operators(aid)` `ON DELETE CASCADE`, `added_at`, `added_by_aid` FK→`operators(aid)` NULL-able | **Membership** "Synod ↔ archon." CASCADE on both sides: deleting a group or archon auto-clears membership. |
| **`synod_roles`** | PK `(synod_name, role_name)`; `synod_name` FK→`synods(name)` `ON DELETE CASCADE`, `role_name` FK→`rbac_roles(name)` `ON DELETE CASCADE`, `granted_at`, `granted_by_aid` FK→`operators(aid)` NULL-able | **Bundle** "Synod ↔ role." CASCADE on both sides: deleting a group cleans the bundle, deleting a role removes it from all groups. |

### REST `/v1/synods`

Eight endpoints. RBAC check - in middleware (`synod.*`-permission, NoSelector), before the handler. One source of truth is `rbac.Service`: REST-handlers and MCP-tools `keeper.synod.*` ([mcp-tools/synods.md](mcp-tools/synods.md)) are thin transport wrappers. Errors - RFC 7807 ([operator-api.md → Error types](operator-api.md)).

| Method + path | Permission | Body/path | Success | Error codes |
|---|---|---|---|---|
| `POST /v1/synods` | `synod.create` | body `{name, description?}` | `201` (body empty) | `409 synod-already-exists`; `422 validation-failed` (empty/broken `name`); `400 malformed-request` |
| `GET /v1/synods` | `synod.list` | — | `200 {items: [...]}` | `500 internal-error` |
| `PATCH /v1/synods/{name}` | `synod.update` | path `name` + body `{description}` (required, 1..1024 characters) | `204` | `404 synod-not-found`; `422 validation-failed` (empty `description` / limit exceeded); `400 malformed-request` (broken JSON, unknown field - including `name` in the body) |
| `DELETE /v1/synods/{name}` | `synod.delete` | path `name` | `204` | `404 synod-not-found`; `409 synod-builtin`; `409 would-lock-out-cluster` |
| `POST /v1/synods/{name}/operators` | `synod.add-operator` | path `name` + body `{aid}` | `204` | `403 forbidden` (least-privilege: group bundle contains a right outside the caller's set); `404 synod-not-found`; `404 not-found` (AID does not exist); `422 validation-failed` (empty/broken AID); `400 malformed-request` |
| `DELETE /v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | path `name`, `aid` | `204` | `404 not-found` (no pair `(name, aid)`); `409 would-lock-out-cluster`; `422 validation-failed` (broken path-AID) |
| `POST /v1/synods/{name}/roles` | `synod.grant-role` | path `name` + body `{role}` | `204` | `403 forbidden` (least-privilege: role contains a right outside the caller's set); `404 synod-not-found`; `404 role-not-found`; `422 validation-failed` (empty `role`); `400 malformed-request` |
| `DELETE /v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | path `name`, `role_name` | `204` | `404 not-found` (no bundle pair `(name, role)`); `409 would-lock-out-cluster` |

- **`GET /v1/synods` items[]** — `{name, description, builtin, roles[], operators[]}`; `roles` / `operators` are serialized by a non-nil array (`[]`, not `null`), sorted deterministically.
- **`add-operator` / `grant-role` are idempotent** - re-adding the same pair is a no-op (`204`).
- **`added_by_aid` / `granted_by_aid`** are taken from the JWT-claim caller; for seed/bootstrap lines - `NULL`.

### Synod Security Invariants

Both RBAC protections (§ Self-lockout invariant, § Least-privilege invariant) **must take into account roles via Synod** ([ADR-049 §f](../adr/0049-synod.md)) - otherwise any of them would have to go through the group. The effective `*`-permission of an archon can come **via Synod**, so self-lockout checks the Synod path as well; a member of a group receives its entire bundle, so the least-privilege subset checks the bundle/role of the group.

| Mutation | Protection | What is being checked |
|---|---|---|
| `synod.add-operator` | **least-privilege subset** | The member receives **the entire bundle of group roles**. The Caller must hold **all effective rights** of this bundle (each role is deployed under its own `default_scope`), otherwise `403 forbidden` (`ErrPermissionNotHeld`). Self-lockout **no** - add only extends admin-set. |
| `synod.grant-role` | **least-privilege subset** | The role is issued to all members. The Caller must hold **all effective rights of the facing role** (under its `default_scope`), otherwise `403 forbidden`. Self-lockout **no**. Non-existent role → `404 role-not-found` (FK-violation, not false subset-pass). |
| `synod.delete` | **builtin-border + self-lockout** | `builtin=true` → `409 synod-builtin` (**first**, builtin is more important than lockout). If the group is bandit `*`-giving role and someone held `*` only through it → `409 would-lock-out-cluster`. |
| `synod.remove-operator` | **self-lockout** | Removal takes away the archon's group roles. If the group gives `*` and the archon held it only through it → `409 would-lock-out-cluster` (exactly the pair `(synod, aid)` is excluded; `*` through a direct grant / another group remains). |
| `synod.revoke-role` | **self-lockout** | Withdrawal removes role rights from all members. If the role being removed is the last `*`-giving role of the group and the member held `*` only through it → `409 would-lock-out-cluster`. |

Synod self-lockout checks - **from the database under `SELECT … FOR UPDATE`**, not from the enforcer snapshot (as for role paths, § How an enforcer resolves): deterministic lock order (group → its roles → admin-set), exclusion of the target pair from the Synod branch admin-set-probe, check "≥1 line left". The lockout check is launched **only if** the group/role actually bundles the `*`-giving role - otherwise the admin-set is not reduced, an extra probe is not needed.

> **The RBAC gate above Synod does not change.** The "effective roles = direct ∪ via Synod" resolution is an addition to the enforcer snapshot assembly (§ How an enforcer resolves); matching layer `Check` / Purview ([ADR-047](../adr/0047-purview.md)) role source does not distinguish and is not overwritten.

## Permissions directory

Full list of permission names validated by Keeper in MVP. Names outside this directory are rejected by the `keeper.yml` parser with error `unknown_permission`.

### Operator (5) — [ADR-014](../adr/0014-operator-identity.md)

| Permission | Semantics |
|---|---|
| `operator.create` | Creating a new Archon in the registry `operators` (via OpenAPI/MCP). The first Archon is created not through this permission, but through `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md)). |
| `operator.revoke` | Setting `revoked_at` for an existing Archon. Active JWTs continue to run until `exp` ([ADR-014(d)](../adr/0014-operator-identity.md)). |
| `operator.issue-token` | Issuing a new JWT for an existing Archon (for example, an operator has lost a token; another operator with this right issues a new one). |
| `operator.list` | Enumeration of Archons with filters (`auth_method` / `revoked`). Also covers single-archon read `GET /v1/operators/{aid}` - the one-permission-on-read pattern, like `soul.list` / `service.list`. The selector is NoSelector in MVP. |
| `operator.read` | Registered forward-only in the directory; MVP is not used in the router (route mounts `operator.list` on both endpoints). Introduced so that role configs can specify it without `unknown_permission`. |

### Role (6) — [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)

RBAC management (roles, permissions, membership) via OpenAPI/MCP - RBAC-storage in Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`, § Storage).

| Permission | Semantics |
|---|---|
| `role.create` | Creating a role (`rbac_roles` + its permissions in `rbac_role_permissions`). You cannot enable permission outside the caller set (§ least-privilege invariant). |
| `role.delete` | Removing a role (permissions + membership cascade). **Forbidden** over `builtin=true` (`cluster-admin`) and when the self-lockout invariant is violated (§ Built-in roles). |
| `role.list` | Listing the roles with their permissions and membership. |
| `role.update` | Changing role permissions. **Forbidden** over `builtin=true`; you cannot remove `*` from a role that holds the only effective `*` (self-lockout); You cannot add permission outside the caller set (least-privilege). |
| `role.grant-operator` | Binding `(role, aid)` - adding a membership string to `rbac_role_operators`. You cannot grant a role with permission outside the caller set (§ Least-privilege invariant). |
| `role.revoke-operator` | Removing the membership line. **Disabled** if it removes the last active AID with effective `*` (self-lockout). |

### Synod (8) — [ADR-049](../adr/0049-synod.md)

Managing **Synod groups** (groups of archons, banding roles - the intermediate level of the model **Archon → Synod → Roles**, § Managing groups of archons). Selector - **NoSelector** (group management - cluster-level operation without scope by coven/host, like `role.*` / `operator.*`; group-scope ADR-049 does NOT enter). Those who mutate write audit, read-only `synod.list` - no.

| Permission | Semantics | Audit-event |
|---|---|---|
| `synod.create` | Creating a Synod group (`POST /v1/synods`). An empty rights group does not issue - least-privilege/self-lockout is not applicable to create (roles are added later via `synod.grant-role`). | `synod.created` |
| `synod.update` | Edit **ONLY `description`** group (`PATCH /v1/synods/{name}`, ADR-049 amend). `name` (PK) **immutable** - rename is deliberately not supported (would violate the invariant of immutable identifiers; symmetry with `rbac_roles.name`). **builtin-border NOT applied** - builtin-group is edited (`description` - cosmetics for UI/audit, not behavior). **Without subset-check and self-lockout** (`description` does not grant or take away rights - both invariants are not applicable); The enforcer's snapshot is not invalidated (`description` is not included in the matching). | `synod.updated` |
| `synod.delete` | Deleting a group (cascade membership + bundle, `DELETE /v1/synods/{name}`). **Forbidden** over `builtin=true` → `409 synod-builtin` (builtin is more important than lockout, checked first); prohibited if the disappearance of the group would leave the cluster without an effective `*` admin → `409 would-lock-out-cluster` (self-lockout). | `synod.deleted` |
| `synod.list` | Enumeration of groups with expanded roles (bundle) and AID members (`GET /v1/synods`). | — (read-only) |
| `synod.add-operator` | Adding an archon to the group (`POST /v1/synods/{name}/operators`). Idempotent. **Under the least-privilege subset:** a member receives the entire bundle of group roles - the caller must hold all effective rights of this bundle, otherwise `403 forbidden` (§ Managing archon groups). | `synod.operator-added` |
| `synod.remove-operator` | Removing an archon from the group (`DELETE /v1/synods/{name}/operators/{aid}`). **Under self-lockout:** removal takes away the group roles from the archon (including `*`-giver) - prohibited if it orphans the last `*`-administrator → `409 would-lock-out-cluster`. | `synod.operator-removed` |
| `synod.grant-role` | Adding a role to the bundle group (`POST /v1/synods/{name}/roles`). Idempotent. **Under least-privilege subset:** the role is issued to all members of the group - the caller must hold all effective rights of the role, otherwise `403 forbidden`. | `synod.role-granted` |
| `synod.revoke-role` | Removing a role from the bundle group (`DELETE /v1/synods/{name}/roles/{role_name}`). **Under self-lockout:** removal takes away the rights of the role from all members - prohibited if this is the last `*`-giving role of the group and someone held `*` only through it → `409 would-lock-out-cluster`. | `synod.role-revoked` |

### Incarnation (13, one of them is deprecated-alias) - [ADR-009](../adr/0009-scenario-dsl.md) / [scenario/](../scenario/README.md) / [ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile) / [ADR-060](../adr/0060-traits.md)

| Permission | Semantics |
|---|---|
| `incarnation.create` | Creating a new instance - running the `create` service scenario. |
| `incarnation.rerun-last` | Restarting the **last fallen** incarnation scenario from `error_locked` (`POST /v1/incarnations/{name}/rerun-last`, [architecture.md → Atomicity and error_locked](../architecture.md)). Atomically removes the block (`state` DOES NOT touch - last known-good, snapshot in `state_history`) and with the same action restarts the last fallen scenario - bootstrap (`create`/...) on the create path OR day-2 operation (`add_user`/...) - with the saved input of the failed run (`error_locked → applying` bypassing `ready` under one `FOR UPDATE`). Input is restored from `incarnation.spec.input` (create-path) or from the failed run recipe (`apply_runs.recipe.input`, day-2-path); if the recipe is not available (the run fell to dispatch - render/no_hosts/preflight, the recipe was not written; cleaned up by Reaper retention; legacy run) → `409` (fail-closed: remove the block with the usual `unlock` and run the scenario manually with an explicit input). Separate right from `incarnation.create` (creating a new incarnation) and `incarnation.unlock` (removing a block without restarting): rerun requires an explicit `reason`. Works only from status `error_locked` (otherwise `409`). The selectors are the same as for other incarnation mutations (`coven=`/`service=`/`incarnation=`). Audit event - `incarnation.rerun_last` (NOT `incarnation.unlocked`). |
| `incarnation.run` | Run a custom scenario (`add_user`, `restart`, any other from `scenario/`). |
| `incarnation.get` | Read `spec` + `state` + `status` instance. |
| `incarnation.list` | Enumeration of instances (with filters). |
| `incarnation.history` | Reading `state_history` instance (snapshot per-change). |
| `incarnation.unlock` | Removal of `error_locked` status after manual disassembly of the consequences of a partial failure. |
| `incarnation.upgrade` | Transferring instance to new `state_schema_version` (running migrations, [migrations.md](../migrations.md)). |
| `incarnation.destroy` | Delete instance (with tombstone period for cloud VMs, [cloud.md](cloud.md)). |
| `incarnation.check-drift` | Scry on-demand drift check ([ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): render `scenario/converge/` in `dry_run` mode + build `DriftReport`. Sync operation (not async). The selectors are the same as for `incarnation.run` (`coven=`/`service=`/`incarnation=`). |
| `incarnation.update-hosts` | Changing mutable fields of an incarnation record via the Operator API. MVP volume — declared `spec.hosts[]` (`PATCH /v1/incarnations/{name}/hosts`, three modes: replace/append/remove; ADR-008). The selectors are the same as for other incarnation mutations (`coven=`/`service=`/`incarnation=`). The former name is `incarnation.update` (deprecated-alias, next line). |
| `incarnation.update` | **DEPRECATED-alias** `incarnation.update-hosts` (PM-decision 2026-06-02: name narrowed to accommodate future update-covens/update-spec). `ParsePermission` canonicalizes it to `incarnation.update-hosts` on the load of the enforcer snapshot - existing roles in `keeper.yml`/DB with the old name continue to work (access to `PATCH /v1/incarnations/{name}/hosts`), no migration is required. Remains in the directory forever (closed enum, removed names - never); The router mounts only the canonical name. |
| `incarnation.traits-set` | Holistic replacement of operator-set key-value trait tags of incarnation (`incarnation.traits` jsonb - source of truth, [ADR-060](../adr/0060-traits.md) R1 slice a) via `PUT /v1/incarnations/{name}/traits`; projected by a sync hook to `souls.traits` member hosts. Transfer of operator-facing trait control from per-soul (`soul.traits-assign`, deprecated) to per-incarnation. Action - kebab (`traits-set`), grammar `<resource>.<action>` (pattern `soul.traits-assign` / `incarnation.update-hosts`). trait-**key** NOT scope-dimension RBAC - authorization with one incarnation-scope-gate (`coven=`/`service=`/`incarnation=` by path-`name`, the same selector as `incarnation.update-hosts`). Audit event `incarnation.traits_changed` (KEYS only, not values). MCP mirror - `keeper.incarnation.traits-set`. |
| `incarnation.view-secrets` | Reveal the plaintext value of the incarnation secret declared by the `revealable_secrets` service (POST `.../secrets/reveal` + discovery GET `.../secrets/revealable`). Strictly privileged `incarnation.get` (removal of mask). Selectors - `coven=`/`service=`/`incarnation=`. Audit `incarnation.secret_revealed` (no value). |

### Choir (5) — [ADR-044](../adr/0044-choir.md)

CRUD named host topology inside incarnation (Choir / Voice, tables `incarnation_choirs` / `incarnation_choir_voices`). **REST-only** (`/v1/incarnations/{name}/choirs*`, no MCP tools; bodies and semantics - [operator-api/choirs.md](operator-api/choirs.md)); routes are connected only when the ChoirDB pool is configured. Choir belongs to the incarnation, so the selector is the same as `incarnation.*`: `incarnation=` / `service=` / `coven=` (landing on path-`{name}`); bare - unrestricted. Those who mutate write audit, read-only `choir.list` - no.

| Permission | Semantics | Audit-event |
|---|---|---|
| `choir.create` | Creating a Choir within an incarnation (`POST /v1/incarnations/{name}/choirs`). | `choir.created` |
| `choir.delete` | Delete Choir (`DELETE /v1/incarnations/{name}/choirs/{choir}`). | `choir.deleted` |
| `choir.list` | Enumeration of incarnation Choirs (`GET …/choirs`) and Voice members of one Choir (`GET …/choirs/{choir}/voices`) - one-permission-on-read. | — (read-only) |
| `choir.add-voice` | Adding a Voice (host) to Choir (`POST …/choirs/{choir}/voices`). Action - kebab, grammar `<resource>.<action>` (pattern `soul.ssh-target-update` / `sigil.key-introduce`). | `choir.voice_added` |
| `choir.remove-voice` | Removing Voice from Choir (`DELETE …/choirs/{choir}/voices/{sid}`). | `choir.voice_removed` |

### Soul (6) - host registry

| Permission | Semantics |
|---|---|
| `soul.create` | Registering a new host in the registry `souls` (`status: pending`) and issuing the first bootstrap token ([onboarding.md](../soul/onboarding.md)). `transport` (`agent`/`ssh`) required; for `ssh` bootstrap token is not issued. |
| `soul.issue-token` | Re-issue of bootstrap token for existing Soul with `transport: agent` (loss of token, planned re-issue). If the token is already active, `409` is rejected if `force=true` is not specified. For `transport: ssh` - `422` (ssh host does not have a bootstrap phase). **Mutation** - scope-aware gate `RequirePermission`, selector `host=<sid>` from path (the context is known before the handler, as opposed to read-visibility). |
| `soul.list` | Listing Souls in the registry (with filters by coven/status/transport). Also covers single-soul read `GET /v1/souls/{sid}`, soulprint-read `GET /v1/souls/{sid}/soulprint` and per-host history `GET /v1/souls/{sid}/history` - the one-permission-on-read pattern, like `service.list` / `omen.list` / `vigil.list` / `decree.list`. **Read visibility is authorized in two layers** (ADR-047 section g/G1, section Two-layer authorization of read endpoints): gate `RequireAction` (existence - does `soul.list` hold in principle) + narrowing by scope in handler (`soulpurview` - coven-pushdown / regex-keyset/soulprint-CEL/`InScope`). **The selector is NOT used at the gate stage of read routes** - scope is resolved from database lines that do not yet exist; The selector form (`coven=`/`host=`) for the role still narrows the visibility in the handler, but the gate ignores it. host selector (`host=<sid>`) is a form for **mutations** souls (see `soul.issue-token` / `soul.ssh-target-update`), not for read. |
| `soul.coven-assign` | Bulk assignment/removal of one Coven tag to hosts using a selector (`POST /v1/souls/coven`). Coven - cold PG tag: pure UPDATE `souls`, no Redis. **Two-layer authorization:** middleware checks the right in general + the assigned label (gate b - the coven-scoped operator passes only for the label in its `coven=`-scope); The service layer intersects target hosts with the scope of the operator (gate a - `souls.coven && scope`). Without both gates bulk = privilege-escalation (an operator with `coven=dev` would assign `prod` to all Souls). |
| `soul.traits-assign` | **DEPRECATED** ([ADR-060](../adr/0060-traits.md) R1 slice a - Trait relocated per-soul → per-incarnation; use `incarnation.traits-set`). forward-compat saved (NOT deleted): roles with this right do not break, per-soul write still works, but is **overwritten by the projection** `incarnation.traits` at the next sync (create / bind). Bulk assignment of operator-set key-value trait tags to hosts by selector (`POST /v1/souls/traits`, jsonb column `souls.traits`). Modes `merge` / `replace` / `remove`. Mirrors coven-write-path (`soul.coven-assign` / `soul.coven-changed` / `POST /v1/souls/coven`), but trait-**key** is NOT an RBAC scope dimension (unlike Coven-tag), therefore it is authorized by **one** gate, not two: existence-gate `RequireAction(soul, traits-assign)` (is there a right at all) + service-layer intersects target hosts with scope operator (gate a - `souls.coven && scope`, the same `BulkScope` as for coven-assign). There is no gate (b) for keys - least-privilege is maintained by a single gate and is no weaker than coven-assign. More details - § Two-layer authorization of read endpoints. |
| `soul.ssh-target-update` | Update per-host SSH push-flow details (`PUT /v1/souls/{sid}/ssh-target`, ADR-032 amendment 2026-05-26, S7-1). Body: `{ssh_port, ssh_user, soul_path}`. Action - hyphenated (`ssh-target-update`), because permission grammar - exactly `<resource>.<action>` (pattern `sigil.key-introduce`); MCP-tool - 3-segment `keeper.soul.ssh-target.update`. **Mutation** - scope-aware gate `RequirePermission`, selector `host=<sid>` from path. Audit `soul.ssh-target.updated`. |

The `coven=` selector (§ Selector Grammar) applies to mutating (`soul.issue-token` / `soul.coven-assign` / `soul.ssh-target-update`) and read- (`soul.list`) permissions: restricts the action to hosts with the specified Coven labels. For **scope-aware mutations** (`soul.issue-token` / `soul.coven-assign` / `soul.ssh-target-update`) it narrows the scope at the gate stage (scope-aware `Check`, context from path/body); `soul.coven-assign` additionally specifies the valid set of assigned labels and a subset of target hosts (scope-intersection). For **read** (`soul.list` and the get/soulprint/history it covers), coven-/regex-/soulprint-narrowing makes the handler via `soulpurview` (gate is only the existence of `RequireAction`), see § Two-layer authorization of read endpoints.

`soul.traits-assign` stands apart: on the **per-soul write-path** trait-**key is not an RBAC scope-dimension** (unlike the Coven-tag), therefore the traits selector for this bulk-write is not applied in the grammar, and the permission itself is authorized by the existence-gate (see directory line above); in its write-path traits change only within the coven-scope operator (single `BulkScope`). This **does not** contradict the fact that **on the incarnation dimension** trait-scope is **implemented** (slice 1, § Selector grammar - `trait=key:value` narrows the visibility of `incarnation.list`/`get`): per-soul bulk-write (`soul.traits-assign`, deprecated) and per-incarnation read-scope (`trait=`-selector on `incarnation.*`) - different surfaces. AND-narrowing on several trait pairs remains follow-up.

**Example** - the operator controls only the dev environment:

```yaml
roles:
  - name: dev-coven-ops
    permissions:
      - "soul.coven-assign on coven=dev,stage"   # attaches/unsets only dev|stage, only on dev|stage hosts
      - "soul.list on coven=dev,stage"
```

With this role, `POST /v1/souls/coven {mode: append, label: dev, selector: {all: true}}` will only affect hosts with the label `dev`/`stage`; an attempt to assign `label: prod` is rejected by `422` (the label is outside the scope), and hosts outside `dev`/`stage` are not included in the UPDATE.

Future Candidates (`soul.revoke` for SoulSeed Review) - Introduced as a separate PR when appropriate API operations occur. `soul.get` is deliberately not introduced: single-soul read is covered by `soul.list` (pattern service/omen/vigil/decree).

### Service (4) — [ADR-029](../adr/0029-service-registry.md)

Managing the Service registry `service_registry` (git source + service ref; routes - [operator-api.md → Service](operator-api.md), registry - [ADR-029](../adr/0029-service-registry.md)). The selector is **NoSelector** (CRUD operates on the registry itself, pattern `provider.*` / `push-provider.*` / `operator.*`). Mutating three write audit ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)), read-only `service.list` - no.

| Permission | Semantics |
|---|---|
| `service.register` | Registering Service in the registry (`POST /v1/services`; MCP `keeper.service.register`). `409 service-already-exists` for take `name`. |
| `service.update` | Editing a registry entry (`PATCH /v1/services/{name}`; MCP `keeper.service.update`). |
| `service.list` | Enumeration (`GET /v1/services`) + single-get (`GET /v1/services/{name}`) + four git projections (`/refs` / `/scenarios` / `/state-schema` / `/dependencies`) - one-permission-on-read, no separate `service.get`. MCP `keeper.service.list`. |
| `service.deregister` | Removing Service from the registry (`DELETE /v1/services/{name}`; MCP `keeper.service.deregister`). |

### Push (3) — [push.md](push.md)

| Permission | Semantics |
|---|---|
| `push.apply` | SSH delivery of Destiny to the host via the `keeper.push` module. |
| `push.cleanup` | Cleaning `/var/lib/soul-stack/` on the host when `revoke` or output from the registry ([push.md → Cleanup](push.md)). |
| `push.read` | Read push run status (`GET /v1/push/{apply_id}`, Variant C orchestrator). |

### Push-Provider (5) — [push.md → S7-2 migration](push.md#s7-2-migration-to-push_providers-pg-table-2026-05-26)

CRUD of the Push-Provider registry - per-provider env-payload params of SSH push-flow plugins (ADR-032 amendment 2026-05-26, S7-2). The entity is implemented as an "SSH Provider" variant of Provider (see amendment). The selector is NoSelector (like `provider.*` / `service.*`).

| Permission | Semantics |
|---|---|
| `push-provider.create` | Create an entry in `push_providers` (`POST /v1/push-providers`). Sensitive params (secret_id/token/password/private_key) must be vault-refs. |
| `push-provider.update` | Replace params of an existing record (`PUT /v1/push-providers/{name}`, replace semantics). |
| `push-provider.delete` | Delete entry (`DELETE /v1/push-providers/{name}`). |
| `push-provider.list` | List records (`GET /v1/push-providers`). |
| `push-provider.read` | Read one entry (`GET /v1/push-providers/{name}`). |

### Errand (3) — [ADR-033](../adr/0033-errand.md)

| Permission | Semantics |
|---|---|
| `errand.run` | Running Errand on Soul via `POST /v1/souls/{sid}/exec` ([ADR-033](../adr/0033-errand.md)). Selectors: `host=<sid>` / `coven=<label>`; bare - unrestricted. |
| `errand.cancel` | Cancel in-flight Errand via `DELETE /v1/errands/{errand_id}` (ADR-033 slice E5). Selector - NoSelector (SID is known only after lookup of the errand line, which is incompatible with pre-handler-middleware-check). |
| `errand.list` | Reading registry Errand (`GET /v1/errands` + `GET /v1/errands/{errand_id}`). Read-only. Selectors filter per-row visibility. |

### Cadence (6) - [ADR-046](../adr/0046-cadence.md)

CRUD of the Cadence schedule registry (`cadences`) - a schedule that spawns a regular [Voyage](../adr/0043-voyage.md) time run ([ADR-046 §7](../adr/0046-cadence.md)). Selector - NoSelector in MVP (CRUD operates with the schedule registry itself, pattern `push-provider.*` / `operator.*`); per-name scope - a separate slice when a multi-tenant RBAC appears. Mutating ones write audit (`cadence.created` / `cadence.updated` / `cadence.deleted`), read-only `cadence.list` - no.

| Permission | Semantics |
|---|---|
| `cadence.create` | Creating a Cadence schedule (`POST /v1/cadences`). **In addition to the route-level `cadence.create`, the second level of guard is triggered - see below § Two-level guard.** |
| `cadence.list` | Enumeration of Cadence schedules (`GET /v1/cadences`) and detail of one (`GET /v1/cadences/{id}`) - one-permission-on-read pattern (like `soul.list` / `errand.list`). |
| `cadence.update` | Editing the schedule recipe (`PATCH /v1/cadences/{id}`). **Backcompat:** remains a valid grant for toggle (`enable`/`disable`) - roles with the old `cadence.update` retain the ability to pause/resume (amendment 2026-06-02). |
| `cadence.delete` | Removing the schedule (`DELETE /v1/cadences/{id}`). The history of Voyage spawns is preserved ([ADR-046 §9](../adr/0046-cadence.md)). |
| `cadence.enable` | Resume schedule without deleting (`POST /v1/cadences/{id}/enable`). Granular Law; endpoint allows `cadence.enable` **OR** `cadence.update` (OR-gate, amendment 2026-06-02). |
| `cadence.disable` | Pause schedule without deleting (`POST /v1/cadences/{id}/disable`). Granular Law; endpoint allows `cadence.disable` **OR** `cadence.update` (OR gate). |

`GET /v1/cadences/{id}/runs` (child Voyage schedules, reuse Voyage-DTO) is gated **`incarnation.history`** (NoSelector), and not `cadence.list` - this is reading the history of child runs, symmetrically to the rest of the history endpoints.

> Spawn flow (Reaper leader `spawn_due_cadence` → Insert child Voyage upon onset of `next_run_at`) RBAC-permission **not controlled** is an autonomous initiative of a background Reaper rule, not an operator call ([ADR-046 §8](../adr/0046-cadence.md), audit-source `background` / `archon_aid: NULL`). Authorization for execution is "hardened" at the time of creation (two-level guard below) - spawning occurs on behalf of the `created_by_aid` schedule.

#### Cadence: two-level guard on create

The `cadence.*` right controls the **schedule** itself, but the Cadence recipe spawns **Voyage**. If only one `cadence.create` were enough for the Cadence establishment, an operator without the right to launch runs could create a schedule that runs them for him - this is a privilege-escalation bypass of RBAC. Therefore, `POST /v1/cadences` is checked **at two levels** (parallel to the two-layer authorization of Voyage-create and `soul.coven-assign`):

1. **Route-level (middleware):** `cadence.create` (NoSelector) - the right to manage schedules in general. Checked before the handler.
2. **Body-level (handler):** Voyage-permission **according to `kind` recipe** (kind is visible only from the body, so the check is not in middleware, but in `CadenceHandler.Create`, parity Voyage-create):
   - `kind: scenario` → required **`incarnation.run`**;
   - `kind: command` → **`errand.run`** required.

The names of these Voyage-permission and kind-mapping are the same as those of the one-time Voyage-create ([ADR-043 §6](../adr/0043-voyage.md)). To start Cadence, you need **both** levels: both the right to manage the schedule and the right to launch what the schedule will spawn. Second level violation → `403 forbidden` (problem-detail of the form `cadence recipe requires Voyage-permission <resource>.<action> by kind=<kind>`); unknown `kind` → `422 validation-failed`. Target in a recipe is the choice from the creator's RBAC scope at the time of creation (parity [ADR-043 §5](../adr/0043-voyage.md)).

### Herald / Tiding (10) - [ADR-052](../adr/0052-herald-notifications.md)

CRUD registries for notifications about run events: **Herald** (delivery channels, `heralds`) and **Tiding** (subscription rules, `tidings`). Selector - NoSelector (cluster-level channel/rule management, pattern `push-provider.*` / `omen.*` / `role.*`); per-name scope - a separate slice when a multi-tenant RBAC appears. Mutating ones write audit (`herald.created`/`updated`/`deleted` + `tiding.*`), read-only `*.list`/`*.read` - no.

| Permission | Semantics |
|---|---|
| `herald.create` | Create a Herald channel (`POST /v1/heralds`). Webhook-config under SSRF-guard; `secret_ref` - vault-ref to signing-token. |
| `herald.read` | Read one Herald channel (`GET /v1/heralds/{name}`). |
| `herald.list` | List Herald channels (`GET /v1/heralds`). |
| `herald.update` | Replace mutable channel fields (`PUT /v1/heralds/{name}`, replace semantics as Push-Provider). |
| `herald.delete` | Delete channel(`DELETE /v1/heralds/{name}`); cascade demolishes related Tiding subscriptions. |
| `tiding.create` | Create a Tiding subscription rule (`POST /v1/tidings`). `herald` - FK to an existing channel; `event_types` - area-glob in the scope of runs. |
| `tiding.read` | Read one Tiding rule (`GET /v1/tidings/{name}`). |
| `tiding.list` | List Tiding Rules (`GET /v1/tidings`). |
| `tiding.update` | Replace mutable fields of the rule (`PUT /v1/tidings/{name}`, replace). |
| `tiding.delete` | Delete rule (`DELETE /v1/tidings/{name}`). |

### Provisioning (2) — [ADR-058](../adr/0058-operator-auth-ldap-oidc.md)

Runtime policy management of **CREATE** operator methods - key `provisioning_allowed_methods` in `keeper_settings` (CSV from domain `{user,ldap,oidc}`). The policy gates ONLY the operator creation branch (`POST /v1/operators` → `user`; federated auto-provision → `ldap`/`oidc`); existing operators log in regardless of policy, `bootstrap`/`system` are never gated. NO key = all methods are allowed (back-compat); specified-but-empty = config-error (anti-lockout - you cannot prohibit ALL methods and lock the establishment of operators). The selector is **NoSelector** (cluster-level policy, like `operator.*` / `role.*`). `update` writes audit (`provisioning.policy_changed`), read-only `read` - no.

| Permission | Semantics |
|---|---|
| `provisioning.read` | Read the current statement creation method policy (`GET /v1/provisioning-policy`). `policy_set=false` → policy is not set (default: everything is allowed). |
| `provisioning.update` | Change policy (`PUT /v1/provisioning-policy`, replace semantics). Empty list → 422 (anti-lockout); method outside `{user,ldap,oidc}` → 422. Audited (`provisioning.policy_changed`). |

### Audit (1) — [ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)

Read-only access to the audit event feed (`audit_log`) via `GET /v1/audit` (UI iteration 2). The very fact of reading the audit table is NOT written to audit (we avoid recursion - each GET would double the table).

| Permission | Semantics |
|---|---|
| `audit.read` | Reading `audit_log` with filters (`type` multi-value, `source` multi-value, `archon_aid`, `correlation_id`, `started_after`/`started_before`). Selector - NoSelector in MVP; per-AID/coven-scope on audit-trail - a separate slice if necessary. |

### Cloud (6) — [cloud.md](cloud.md)

CRUD registries of Cloud-Providers (`providers`) and Cloud-Profiles (`profiles`, ADR-017). Full surface **implemented** (REST `/v1/providers*` + `/v1/profiles*` and MCP `keeper.provider.*` / `keeper.profile.*`). The selector is **NoSelector** (CRUD operates on the registry itself, pattern `push-provider.*` / `service.*`). **`update`-permission NO** - Provider/Profile are immutable (change parameters = `delete` + `create`); read-visibility (list + get) gates one permission `*.read` (pattern `operator.list`↔`read`). Those who mutate write audit, read-only - no.

| Permission | Semantics | Audit-event |
|---|---|---|
| `provider.create` | Creating a Provider record in Postgres (`POST /v1/providers`) - a configured cloud account. `409 provider-already-exists` per take `name`; `credentials_ref` must be `vault:<path>` (creds are not resolved in the API). | `provider.created` |
| `provider.read` | Enumerating Providers (`GET /v1/providers`) and reading one (`GET /v1/providers/{name}`) is the one-permission-on-read pattern. | — (read-only) |
| `provider.delete` | Deleting Provider record (`DELETE /v1/providers/{name}`). `409 provider-has-profiles`, if the Provider is referenced by Profiles (FK `ON DELETE RESTRICT`, migration 020) - first delete dependent Profiles. | `provider.deleted` |
| `profile.create` | Creating a Profile record (`POST /v1/profiles`) - a reusable VM-spec on top of the Provider. `409 profile-already-exists` per take `name`; `422 validation-failed` to a reference to a non-existent Provider (FK). | `profile.created` |
| `profile.read` | Enumerating Profiles (`GET /v1/profiles`, optional filter `provider=`) and reading one (`GET /v1/profiles/{name}`). | — (read-only) |
| `profile.delete` | Deleting Profile record (`DELETE /v1/profiles/{name}`). | `profile.deleted` |

### Augur (6) - [ADR-025](../adr/0025-augur.md) / [augur.md](augur.md)

CRUD registries of the external access broker Augur (Omen - external system, Rite - grant). OpenAPI / MCP surface starts as **stub directory** ([augur.md](augur.md)); permissions are normalized here.

| Permission | Semantics |
|---|---|
| `omen.create` | Creating an Omen record in Postgres (`omens`) - external system (vault/prometheus/elk) with vault-ref to master-cred. |
| `omen.list` | Listing Omens in the registry. |
| `omen.delete` | Removing Omen record (cascade removes related Rites - `rites.omen ON DELETE CASCADE`). |
| `rite.create` | Creating a Rite record in Postgres (`rites`) - grant a subject (coven/sid) to Omen with allow-list and `delegate`. |
| `rite.list` | Listing Rites in the registry. |
| `rite.delete` | Removing Rite record. |

> **Live-fetch from Soul (`AugurRequest`) RBAC-permission is not controlled** - this is not an operator operation via OpenAPI / MCP, but a machine request from Soul via gRPC EventStream. Live-fetch authorization is a separate Augur mechanism (Omen + Rite + allow-list by mTLS→SID→covens, [augur.md → Authorization](augur.md)), not the Archon's RBAC-permission.

### Oracle (6) - [ADR-030](../adr/0030-vigil-oracle.md)

CRUD of Oracle beacons circuit registries (Vigil - Soul-side check, Decree - reactor rule). OpenAPI (`POST/GET/DELETE /v1/vigils*` + `/v1/decrees*`) and MCP (`keeper.oracle.vigil.*` / `keeper.oracle.decree.*`) - surface implemented (S3). All six are checked by `RequirePermission`-middleware (selector is NoSelector, like `omen.*`/`rite.*`); failure → 403 `forbidden`. Mutating ones (`*.create`/`*.delete`) write audit, read-only `*.list` (and get) do not.

| Permission | Semantics | Audit-event |
|---|---|---|
| `vigil.create` | Creating a Vigil record in Postgres (`vigils`) - Soul-side check (check - address core-beacon + interval + subject coven XOR sid). | `vigil.created` |
| `vigil.list` | List Vigils in the registry (and get them by name). | — (read-only) |
| `vigil.delete` | Deleting Vigil record (stops being heard in `VigilSnapshot`; Decrees do not cascade). | `vigil.deleted` |
| `decree.create` | Creating a Decree record in Postgres (`decrees`) - reactor rule (on_beacon × subject × incarnation_name → named scenario; option where-CEL + cooldown). | `decree.created` |
| `decree.list` | List Decrees in the registry (and get them by name). | — (read-only) |
| `decree.delete` | Removing Decree record (cleans cooldown-state `oracle_fires` in a cascade). | `decree.deleted` |

> **Reactor-flow (`Portent` → match Decree → enqueue scenario) RBAC-permission is not controlled** - this is a machine Soul-initiated path via gRPC EventStream, not an operator operation. Protection - subject binding Decree (coven XOR sid) + membership-check + default-deny + whitelist scenario ([ADR-030(b)](../adr/0030-vigil-oracle.md)), not RBAC-permission of the Archon.

### Plugin Sigil (3)

[ADR-026](../adr/0026-sigil.md) / [plugins.md → Integrity-model](plugins.md#integrity-model). Managing the allow-list of plugin integrity **Sigil** - explicit permission by the Archon of a specific binary to the registry `plugin_sigils`. OpenAPI surface **implemented** (S4a, `POST/GET/DELETE /v1/plugins/sigils*`); MCP - S4b. All three are checked by `RequirePermission`-middleware (selector is NoSelector, like `operator.*`/`role.*`); failure → 403 `forbidden`. Mutating ones (`plugin.allow`/`plugin.revoke`) write audit (see below), read-only `plugin.list` - no.

| Permission | Semantics | Audit-event |
|---|---|---|
| `plugin.allow` | Allowance `(namespace, name, ref)` in allow-list `plugin_sigils` — Keeper reads the binary of the active cache slot via `current`-symlink (R-nested `<ns>-<name>/<commit_sha>/`), reads `sha256`, signs and inserts the record. `ref` - git-verified (Keeper resolves `source`+`ref` into `commit_sha` slot via go-git, [ADR-026(g)](../adr/0026-sigil.md)). Human-verified supply-chain control operation. | `plugin.allowed` |
| `plugin.revoke` | Revocation of a previously accepted entry from `plugin_sigils` (the binary no longer passes Sigil verification). | `plugin.revoked` |
| `plugin.list` | Enumeration of active entries of the allow-list `plugin_sigils` (without signature/manifest). | — (read-only) |

> **Sigil verification before seal/exec RBAC-permission is not controlled** - this is a host-side verification of the Keeper's digest + signature ([ADR-026(b)](../adr/0026-sigil.md)), and not an operator operation. Other plugin-management operations (`plugin.install` / `plugin.update` delivery/cache) - separate PR when the corresponding API appears.

### Sigil signing keys (4) - [ADR-026(h)](../adr/0026-sigil.md) / R3

Rotation of trust-anchor-**signing keys** Sigil (registry `sigil_signing_keys`, separate from permissions `plugin_sigils`). OpenAPI **implemented** (`POST/GET /v1/sigil/keys`, `POST /v1/sigil/keys/{key_id}/primary`, `DELETE /v1/sigil/keys/{key_id}`) + MCP `keeper.sigil.key.*`. All four are `RequirePermission`-middleware(selector NoSelector like `plugin.*`); failure → 403. Mutating ones write audit, read-only `sigil.key-list` - no. **Resource `sigil`, action - hyphenated** (`key-introduce`): permission grammar - exactly `<resource>.<action>` (3-segment `sigil.key.introduce` is MCP-tool, not permission); correspondence `keeper.sigil.key.<verb>` ↔ `sigil.key-<verb>`.

| Permission | Semantics | Audit-event |
|---|---|---|
| `sigil.key-introduce` | Entering a new signing key: Keeper generates an ed25519 pair, writes the private key to Vault KV (`secret/keeper/sigil-keys/<key_id>`), inserts the public part into the registry. Private is NEVER in the response/log. | `sigil.key-introduced` |
| `sigil.key-set-primary` | Make the active key primary (new Sigils are signed with it after the cluster reload). | `sigil.key-primary-set` |
| `sigil.key-retire` | Deriving a key from a set (Soul forgets the next time `SigilTrustAnchors`). Prohibited for primary directly and for last active. | `sigil.key-retired` |
| `sigil.key-list` | Enumeration of active signature keys (primary first, without `vault_ref`). | — (read-only) |

### Bootstrap

**Bootstrap-specific permissions are missing.** `keeper init` ([ADR-013](../adr/0013-bootstrap-archon.md)) operates under admin-bypass: only the "registry `operators` is empty" invariant is checked under PG advisory lock, no permission checks. The First Archon receives the role `cluster-admin` (`permissions: ["*"]`).

## Built-in roles

MVP has exactly one built-in role:

- **`cluster-admin`** — `permissions: ["*"]`, `builtin=true`. Inserted by **seed migration** (E1, [ADR-028(b)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) into `rbac_roles` + `rbac_role_permissions` before any `keeper init`. Binds to the first Archon at `keeper init` - this is the **membership string** `(cluster-admin, <aid>)` in `rbac_role_operators` (fix BUG-1, [ADR-028(c)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). The `builtin=true` flag protects it from `role.delete` / `role.update`. Additional operators are assigned this role through `role.grant-operator` (an operator with this privilege).

The remaining roles are created via OpenAPI/MCP (`role.create` + `role.grant-operator`); they do not have special built-in semantics.

**Invariant self-lockout ([ADR-028(f)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** You cannot leave a cluster without at least one active Archon (`revoked_at IS NULL`) with an effective `*`-permission. Checked on all paths that could break this:

- `operator.revoke` - recall of the last Archon from `*` ([ADR-013(c)](../adr/0013-bootstrap-archon.md)).
- `role.delete` - removing the role (`cluster-admin` or other) that provides the only path to `*`.
- `role.update` — removing permission `*` from the role through which the only effective `*` is maintained.
- `role.revoke-operator` - removes the last AID with the effective `*`.

When trying, the API returns `409` (`would-lock-out-cluster`, [naming-rules.md → Error codes](../naming-rules.md#error-codes)). The only way to recover from a lockout is to wipe Postgres + repeat `keeper init`, which is unacceptable for sales - hence the invariant.

## Application

RBAC is checked **before** the operation is executed, regardless of the transport:

- **OpenAPI** - at the input of the HTTP handler, before the business logic.
- **MCP** - at the MCP-tool input, before execution.
- **`keeper.push`** - before opening an SSH session and before starting each Destiny step on a push host.

Audit events are written to the run log ([storage.md](storage.md)) indicating the AID operator, resource, action, RBAC check result and (if failure) reason.

### Two-layer authorization of read endpoints (ADR-047 §g G1)

Read endpoints with scoped visibility (souls-list/get/soulprint/history; ADR-047) are authorized **in two layers**, because one gate layer cannot simultaneously "pass the right holder" and "narrow visibility by scope":

1. **Gate (existence) - `RequireAction` over `HoldsAction`.** It only asks: does the `<resource>.<action>` operator hold **in principle**, in any scope, ignoring the context selector. Doesn't hold → `403`.
2. **Scope-narrowing - handler.** After the row fetch, handler reduces output to the scope-boundary of the operator through the per-resource resolver: `keeper/internal/soulpurview` (coven-pushdown in SQL / regex-keyset / soulprint-page-CEL) for a list and `InScope` for a single object; `keeper/internal/statepredicate` (state-CEL) for incarnations. Outside scope: list is empty, single-get/soulprint/history is `404`/`403`.

**Why two layers and not a scope-aware gate.** Scope-aware `Check(aid, resource, action, context)` for **scoped**-permission with an empty context gives false `deny`: the selector-key (`coven`/`host`/…) is missing in the `nil` context of the read request, and enforcer for a missing key, deny is returned (the same mechanics as in incarnation gates before ADR-008 amendment a). This broke the scoped operator's access to **its own** list - `RequirePermission(...Selector)` cut it off even **before** the handler, although by its scope it is required to see hosts. Root: read endpoint **does not carry a scope context at the gate stage** - scope (`coven` host, `state` incarnation) is resolved from database lines that do not yet exist at the time of the middleware check. `HoldsAction` removes the context from the question (existence, not applicability), and the real narrowing is deferred to the handler, where the lines are already fetched.

**Most of the mutations remain on scope-aware `Check`.** issue-token / ssh-target-update / coven-assign by souls carry the scope-context from path/body (`host=<sid>` from path, `coven=<label>` from body), so they continue to gate scope-aware `RequirePermission` / `RequirePermissionMulti` - for them the context is known before the handler and there is no false deny. The boundary for them is: **read** (context from database rows, not yet) → `RequireAction`; **scope-aware mutation** (context from path/body) → `RequirePermission`.

**The exception is `soul.traits-assign` (`POST /v1/souls/traits`).** This bulk mutation is gated by the **existence-gate `RequireAction`**, and NOT by the selector `RequirePermission`, although the context is in the body. The reason is different from that of read endpoints: trait-**key** (unlike the Coven-tag of `coven-assign`) **is not a scope-dimension of RBAC**, therefore there is no selector gate by key (gate b) for it - the selector `RequirePermission` here would not narrow the scope, but would cut off the coven-scoped operator (its `coven=dev`-permission would not match a request without a coven context at the gate stage), although he has the right to change traits on his dev hosts. Least-privilege for traits is maintained **by a single gate a** at the service-layer level (`soul.BulkAssignTraits`/`BulkReplaceTraits`): target hosts ⊆ coven-scope operator (the same `BulkScope` as `coven-assign`) - therefore traits-write is **not weaker** than coven-write in terms of severity. RBAC-scope on the traits themselves as a selector-dimension is implemented **on the incarnation side** (`trait=key:value` narrows the read-visibility of `incarnation.list`/`get`, slice 1 - § Selector grammar); it **does not** apply to this per-soul bulk-write (per-soul `soul.traits-assign` deprecated, [ADR-060](../adr/0060-traits.md) R1 slice a).

### Revoked semantics on read paths (ADR-047 §g G1)

`ResolvePurview` has become **revoked-aware**: for a revoked (`operators.revoked_at IS NOT NULL`) operator returns `Purview{Deny:true}` (terminal flag, [naming-rules.md → Purview](../naming-rules.md)) **before** collecting any scope dimensions - otherwise bare `*`-role The revoked operator would return `Unrestricted`. This is a **single point** cutting off revoked on all read-souls paths, because they are all derived from `ResolvePurview`:

- **gate** — `HoldsAction`→`Deny`→`false`→`403`;
- **list** — `soulpurview.Resolve`→Empty-scope→empty list;
- **single-get / soulprint / history** — `readScope`→Empty→`InScope`→`false`→`404`.

Per-host **history** (`GET /v1/souls/{sid}/history`) goes through the **same** handler-`InScope`-gate as get/soulprint - the visibility of the host is checked before the timeline is expanded, revoked is cut off there. This is the first real use of the `Purview.Deny` field (before G1 - blank, always `false`).

On read revoked = "no access" (`403`/`404`), and NOT `401`'s scope-aware parity `Check` (which mapped revoked to `401 operator-revoked`): visibility of Souls should not differentiate between revoked and no-permission.

## Bootstrap of the first Archon

Pinned [ADR-013](../adr/0013-bootstrap-archon.md). Briefly:

- **Entity name is Archon** (Archon). See [naming-rules.md → Domain Entities](../naming-rules.md).
- **Mechanism for issuing the first credential** - administrative subcommand `keeper init`:

  ```bash
  keeper init --archon=archon-alice --config=/etc/keeper/keeper.yml
  ```

  - The command requires an empty registry `operators` in Postgres (protection via PG advisory lock + explicit `--initialize` flag for normal start of Keeper before bootstrap).
  - Creates the first Archon with the specified **AID** ([Archon ID](../naming-rules.md)), binds the role `cluster-admin` (`permissions: ["*"]`) to it - writes the membership string `(cluster-admin, <aid>)` to `rbac_role_operators` (the role `cluster-admin` already exists from the E1 seed migration, [ADR-028(b/c)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres); this is a BUG-1 fix.
  - Issues a JWT-credential (form - [ADR-014](../adr/0014-operator-identity.md)) and puts it in a file with `mode 0400`. This token is the operator's only path into the system after bootstrap.
  - When called again on an already initialized database, it refuses.
- **Restart semantics.** If `operators` is empty and `--initialize` is not specified, Keeper refuses to start with a prompt to run `keeper init`. This is protection against accidental re-bootstrap after a catastrophic wipe of Postgres.
- **HA race-condition.** PG advisory lock at stage `keeper init` excludes the race of N instances of the cluster.

After the release of the first Archon, all other operators are created through the usual OpenAPI/MCP with RBAC checking - An Archon with the right `operator.create` creates new ones, in `operators` they receive FK `created_by_aid` for the parent.

## Role examples

Roles are created via OpenAPI/MCP (`role.create` for role + permissions, `role.grant-operator` for membership - § Permissions directory). Below is the logical model (which lies in `rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`); permission lines follow the grammar § permissions format.

**Simple roles - no selector:**

```
role: soul-reader
  permissions: ["soul.list", "incarnation.list", "incarnation.get"]
  operators:   ["archon-monitor-01"]

role: cloud-admin
  permissions:
    - "provider.create"
    - "provider.read"
    - "provider.delete"
    - "profile.create"
    - "profile.read"
    - "profile.delete"
  operators:   ["archon-cloud-01"]
```

**Complicated role - with selectors:**

```
role: db-operator
  permissions:
    - "incarnation.* on service=redis,vault-cluster"
    - "incarnation.upgrade on service=redis,vault-cluster"
    - "push.apply on coven=db,cache"
    - "soul.list"
  operators: ["archon-db-01", "archon-db-02"]
```

`db-operator` can: do any operations on incarnations of services `redis` and `vault-cluster` (including `upgrade`); do push-apply to hosts with coven `db` or `cache`; see the Souls list. Other - prohibited (`default_policy: deny`).

**Point role - for a specific instance:**

```
role: redis-prod-only
  permissions:
    - "incarnation.run on incarnation=redis-prod"
    - "incarnation.get on incarnation=redis-prod"
    - "incarnation.history on incarnation=redis-prod"
  operators: ["archon-oncall-01"]
```

The `redis-prod` attendant can run scripts and read the state of only this instance.

**Per-Coven / per-Service roles (ADR-008 amendment a):**

```
role: prod-operator
  permissions:
    - "incarnation.run on coven=prod"
    - "incarnation.destroy on coven=prod"
  operators: ["archon-prod-oncall"]

role: redis-fleet
  permissions:
    - "incarnation.* on service=redis"
  operators: ["archon-redis-team"]
```

- `prod-operator` can launch and demolish incarnations whose declared `covens` contains `prod` (or incarnation name = `prod`) - env-scope by Coven label, regardless of the service. he will not touch incarnation with `covens=[dev]`.
- `redis-fleet` can perform any incarnation operations on incarnations of the `redis` (`incarnation.service == "redis"`) service, regardless of their env tags.
- When create, the `coven=prod` rule will not allow creating an incarnation with a tag outside `prod`: scope resolves from declared `covens` request body ∪ `{name}` (see § Selector grammar).

## See also

- [architecture.md → ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) - transfer of RBAC-storage to Postgres, fix BUG-1, solution package (tables / permissions / self-lockout / hard-cut / phases).
- [operator-api.md](operator-api.md) - OpenAPI side: HTTP endpoints, 1:1 mapping endpoint ↔ permission ↔ MCP-tool.
- [mcp-tools.md](mcp-tools.md) - MCP side: tools directory, declaration format, async-convention, error mapping.
- [push.md](push.md) - push under a single RBAC.
- [cloud.md](cloud.md) - RBAC for cloud operations.
- [storage.md](storage.md) - registry `operators` and tables `rbac_roles` / `rbac_role_permissions` / `rbac_role_operators` in Postgres.
- [architecture.md → End-to-end requirements](../architecture.md).
- [architecture.md → ADR-013](../adr/0013-bootstrap-archon.md) and [ADR-014](../adr/0014-operator-identity.md) - bootstrap of the first Archon, credential form, registry `operators`.
- [naming-rules.md → RBAC](../naming-rules.md) - tables `rbac_*`, permissions `role.*`, dilution of the term "FK".
- [requirements.md](../requirements.md) - RBAC as a requirement out of the box.
