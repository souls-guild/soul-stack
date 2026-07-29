# RBAC and operators

RBAC is built into Keeper out of the box ([requirements.md](../requirements.md)) and applies to **OpenAPI / MCP / push operations uniformly**: the same set of policies decides whether an operator can start a Soul, pull `push.apply`, read the Soulprint of a specific coven, or create a Provider.

The model is the classic trio "operators (Archons) ↔ roles ↔ permissions." Everything is stored in Postgres ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)): Archons ([ADR-014](../adr/0014-operator-identity.md)) - registry `operators`, roles and their permissions - tables `rbac_roles` / `rbac_role_permissions`, binding "operator ↔ role" (**membership**) - table `rbac_role_operators`. Management - via OpenAPI/MCP (`role.*`-permissions, § Permissions directory), not by editing YAML.

> **Hard-cut: block `rbac:` in `keeper.yml` removed ([ADR-028(g)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** Before ADR-028, roles / permissions / membership were declared in `keeper.yml::rbac` - this gave BUG-1 (`keeper init` creates an Archon in the database, but membership puts in a JWT-claim, which the enforcer does not read, resolving it from YAML → bootstrap - Archon receives `403`). The key `rbac:` is now rejected by the parser as `unknown_key`; types `config.KeeperRBAC` / `config.RBACRole` / field `KeeperConfig.RBAC` removed. There are no legacy installations, YAML→DB migration is not required.

## Storage - three PG tables

RBAC materialized in Postgres ([ADR-028(a)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres), schemas - [storage.md](storage.md)):

| Table | Columns | Role |
|---|---|---|
| **`rbac_roles`** | `name` PK (kebab-case, CHECK on format), `description`, `builtin` BOOL, `created_at`, `created_by_aid` FK→`operators(aid)` NULL-able, `default_scope` TEXT NULL-able ([ADR-047](../adr/0047-purview.md)), `parent_role` TEXT NULL-able self-FK→`rbac_roles(name)` `ON DELETE RESTRICT` + `scope_mode` TEXT NULL-able (`track`/`pin`, NULL iff `parent_role` is) ([ADR-078](../adr/0078-rbac-derived-roles.md)) | Directory of roles. `builtin=true` disables `role.delete` / `role.update` (built-in role - `cluster-admin`, see § Built-in Roles). `created_by_aid IS NULL` - seed roles without an Archon initiator. `parent_role IS NULL` - a **plain** role (see § Derived roles). |
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

Permission string - either `*` (full access, equivalent to cluster-admin), or a two-level name `<resource>.<action>` with an optional **boolean scope** `on <expr>` ([ADR-047 amendment 2026-07-18, NIM-128](../adr/0047-purview.md)). The scope is a boolean condition over **five** selector types (`coven` / `service` / `incarnation` / `host` / `trait`) joined with `AND` / `OR` and parenthesised groups; the removed types `regex` / `soulprint` / `state` are gone (pattern matching on host identity → `host matches <glob>`). Formally (EBNF):

```ebnf
permission    := "*" | "*" " on " expr | <resource>.<action> ( " on " expr )?
resource      := [a-z][a-z0-9-]*
action        := "*" | [a-z][a-z0-9-]*

expr          := or_expr
or_expr       := and_expr ( WS "OR"  WS and_expr )*   ; OR = lowest precedence
and_expr      := factor  ( WS "AND" WS factor  )*     ; AND binds tighter than OR
factor        := condition | "(" WS? expr WS? ")"
condition     := coven_c | service_c | incarnation_c | host_c | trait_c

coven_c       := "coven"       match_set
service_c     := "service"     match_set
incarnation_c := "incarnation" ( match_set | WS "matches" WS glob )
host_c        := "host" ( match_set | WS "matches" WS glob )
trait_c       := "trait." key "=" value             ; dot-notation, one pair (in-list → follow-up)

match_set     := "=" value
               | WS "in" WS "(" WS? value_list WS? ")"
value_list    := value ( WS? "," WS? value )*
value         := unquoted | quoted
glob          := quoted | unquoted_glob             ; full anchored match

key           := [a-z][a-z0-9_.-]*                  ; trait key
unquoted      := [A-Za-z0-9_.-]+
unquoted_glob := [A-Za-z0-9_.*?-]+                  ; + '*' '?' for glob
quoted        := '"' [^"]* '"'                      ; for values with spaces/special chars
```

Rules not expressed by grammar:

- **Exactly two segments.** `incarnation.create` - valid; `keeper.incarnation.create` - invalid (three segments; a name of this type is **MCP-tool name**, not permission).
- **Wildcard only in `<action>`.** Exactly **two** wildcard forms are supported: `<resource>.*` - all actions of one resource (`incarnation.*`) - and the full `*` (without a dot) - cluster-admin over all resources. `*.create` (wildcard in `<resource>`) and resource-glob (`incarn*.run`) are **not** supported in MVP. Action-wildcard **is scoped to its own resource and does not flow to others**: `incarnation.*` does not grant rights on `service`/`role`/`operator` - `Matches` compares `<resource>` exactly (regression - `permission_test.go`, `subset_test.go` in `keeper/internal/rbac/`).
- **Scoped full-wildcard `* on <scope>` (NIM-128).** The full `*` accepts an optional boolean scope: **bare `*`** = cluster-admin (unbounded, § Built-in roles); **`* on <expr>`** = a **scoped super-admin** — every action on every resource, but **only where the predicate holds** (`Matches` for a scoped `*` evaluates `evalScope`, so a context-less cluster-op — e.g. `operator.create` with no coven — is **denied** under `* on coven=X`). `resource.*` (a resource wildcard) does **not** take a scope beyond the ordinary `on <expr>` on the two-segment form; the extra scoped form is specific to the full `*`.
- **`<resource>.*` = ALL actions of the resource, including sensitive ones.** `incarnation.*` confers both `incarnation.view-secrets` (plaintext-secret disclosure, NIM-74) and `incarnation.destroy` (destruction), and `role.*` - both `role.grant-operator` (granting a role to an operator): `<resource>.*` is not equivalent to "read/run on the resource" - it includes destruction, secret disclosure and rights granting. The role editor in the UI (RBAC -> Permissions) provides `<resource>.*` as a separate "all actions" checkbox on the resource; the checkbox also covers future actions of this resource, so if full access is not needed, list the actions explicitly.
- **Whitespace.** There is exactly one space between `<resource>.<action>` and `on`. Inside a condition: `key=value` without spaces (`coven=db`); the operators `AND` / `OR` / `in` / `matches` are surrounded by whitespace (`coven=a AND host matches "web-*"`). Parentheses may hug their content (`(coven=a OR coven=b)`).
- **Boolean operators (NIM-128).** Conditions are joined with `AND` / `OR`; `AND` binds tighter than `OR` (`a AND b OR c` = `(a AND b) OR c`), both left-associative, parentheses override. The UI condition-builder always emits explicit parentheses (each group is homogeneous — all-`AND` or all-`OR`), so a UI-authored scope round-trips 1:1; hand-written strings may rely on the precedence rule. A size cap is enforced on load (≤ 32 atoms, ≤ 4 nesting levels) — a bigger expression fails snapshot load.
- **`matches <glob>` on `host` and `incarnation` (NIM-128).** Replaces the removed `regex='…'`, and is valid for **both** identity dimensions: `host matches "redis-*"` (SID/hostname) **and** `incarnation matches "prod-*"` (incarnation name). Glob only: `*` = any sequence (incl. empty), `?` = one character, everything else literal, **full anchored** match (`redis-*` ≡ `^redis-.*$`). Compiled internally to an anchored RE2 (ReDoS-safe, length cap 256); RE2 syntax is NOT exposed to the operator. `coven` / `service` stay **exact / `in`-list only** — no glob (name-pattern matching is meaningful only for the two identity dimensions).
- **Quoting.** A value with a space / special character / glob metacharacter goes in double quotes (`host matches "redis-*"`, `coven="a b"`); a plain value (character class `[A-Za-z0-9_.-]`) needs no quotes. The historical single-quote form (of the removed `regex`/`soulprint`) is retired.
- **Case.** Operators `AND OR IN MATCHES` are case-insensitive on input and canonicalised to `AND` / `OR` / `in` / `matches`. Keys (`coven`…, `trait.<key>`) are lower-case; values are case-sensitive (labels/names are identifiers).
- **Kebab-case in action.** Hyphen in `<action>` is acceptable (`operator.issue-token`); Space and underscore are not.

Grammar expansion (new selector types, wildcards in values, new forms) - separate PR in `rbac.md` with justification. Adding new selector types is not breaking (old roles continue to be validated). **Removing** a selector type (as NIM-128 removed `regex`/`soulprint`/`state`) is breaking and requires a fail-closed data migration ([migration `100_rbac_drop_pattern_selectors`](../../keeper/migrations/100_rbac_drop_pattern_selectors.up.sql)).

## Permission ↔ MCP-tool / OpenAPI endpoint

**1:1 correspondence.** Each permission of the two-segment format `<resource>.<action>` controls:

- MCP-tool keeper-side with name `keeper.<resource>.<action>` (4-segment name).
- Corresponding OpenAPI endpoint (POST `/v1/<resource>/<action>` or similar).

Example: permission `incarnation.create` gives the right to call MCP-tool `keeper.incarnation.create` or HTTP endpoint `POST /v1/incarnations`. Full compliance with MCP-tool ↔ OpenAPI endpoint ↔ permission is normalized in [operator-api.md → Mapping endpoint ↔ MCP-tool ↔ permission](operator-api.md#mapping-endpoint--mcp-tool--permission); MCP side (tool declaration format, transport, async-convention, input/output schemas) - in [mcp-tools.md](mcp-tools.md).

## Selector grammar

> **Boolean scope (NIM-128, [ADR-047 amendment 2026-07-18](../adr/0047-purview.md)).** The scope after `on` is a **boolean expression** over the five selector types below, joined with `AND` / `OR` and parenthesised groups (grammar — § Format permissions). The old single-selector form (`on coven=a,b`) is the degenerate case of one condition; the removed pattern-matching types `regex` / `soulprint` / `state` are gone. Everything below describes the five surviving condition types and how a boolean scope is contained in the least-privilege subset.

A **condition** filters one dimension; conditions combine with `AND` (both must hold) / `OR` (either holds). The five condition types come from a closed enum:

| Condition | Semantics | Value source for query matching |
|---|---|---|
| `service=v` / `service in (a,b)` | Restricts to incarnations of the specified Service types. | **incarnation-operations:** `incarnation.service` (Service name from git, [architecture.md → Artifacts](../architecture.md)). On create - `service` from the request body. |
| `coven=v` / `coven in (a,b)` | Restricts to the specified Coven tags. | **incarnation operations** (run / destroy / upgrade / get / history): declared `incarnation.covens` — the incarnation's **real stable** env tags **only** ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation): `incarnation.name` is **no longer** added as a root coven tag). On create - declared `covens` from the request body. **soul-operations** (`soul.coven-assign` / `soul.list`): host label from body/query (`souls.coven[]`). To scope by the incarnation's own name, use `incarnation=<name>`, **not** `coven=<name>`. |
| `incarnation=v` / `incarnation in (a,b)` | Restricts to a specific instance. | `incarnation.name` of the target instance. **This is the scope for the incarnation's own name** — a role that previously used `coven=<incarnation-name>` migrates here ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). |
| `host=v` / `host in (a,b)` | Restricts to a specific host (exact SID). **Context source is path/body mutation** (`soul.issue-token` / `soul.ssh-target-update` / `errand.run`), where the SID is known before the handler. `soul.console` is the same class with a later context point: the SID arrives in the WebSocket `open` frame, so the check is in-handler per session rather than in middleware (§ Console). On the read-visibility of souls (`soul.list` and the get/soulprint/history covered by it) the `host=` condition at the gate stage is NOT applied - read is gated by existence-`RequireAction`, narrowing by scope is done by handler (ADR-047 §g G1, § Two-layer authorization read-endpoints). | Host SID ([identity.md](../soul/identity.md)) from path/body mutation, or from the console `open` frame. |
| `host matches <glob>` | Restricts to hosts whose SID/name matches the **glob** (`*`, `?`; full anchored). Replaces the removed `regex='…'`. | SID/hostname - `host`- or `sid`-request context key (list-visibility/target resolver supplies the value for read paths). |
| `incarnation matches <glob>` | Restricts to incarnations whose **name** matches the **glob** (`*`, `?`; full anchored) — NIM-128 extends the glob form to the incarnation identity dimension (`incarnation matches "prod-*"`). Same anchored-glob → `LIKE` pushdown over `incarnation.name`. | `incarnation.name` of the target instance. |
| `trait.<key>=v` | Restricts to resources whose operator-set trait label `<key>` equals `v` (exact scalar-equality, **not** CEL). [ADR-060](../adr/0060-traits.md) p. 7, realised by NIM-128 as a boolean-builder dimension. | An incarnation matches on its own `incarnation.traits`; a **host** matches on its EFFECTIVE traits - its own `souls.traits` **OR** those of the incarnations it belongs to ([ADR-080](../adr/0080-label-inheritance-union.md), a correlated `EXISTS` over `incarnation_membership` in the same SQL pushdown). |

**OR within one dimension** is written with `in (…)`: `coven in (db, cache)` = "coven `db` OR `cache`" (the equivalent of the former `coven=db,cache`). Matching is exact per value. Between different conditions, use explicit `AND` / `OR`.

**`matches <glob>` (replaces regex) — `host` and `incarnation`.** Glob only: `*` = any sequence, `?` = one character, everything else literal, **full anchored** match (`web-*` ≡ `^web-.*$`). Under the hood the glob compiles to an anchored RE2 (`*`→`.*`, `?`→`.`, RE2 metacharacters escaped) reusing the existing ReDoS-safe engine; length cap 256; RE2 syntax is not exposed. Valid for **both identity dimensions**: `host matches` (target = `host` context key, then `sid`) and `incarnation matches` (target = `incarnation.name`); a request without the corresponding key → deny. Glob maps cleanly to SQL `LIKE` (`web-*`→`web-%`, `?`→`_`) for souls/incarnation-visibility pushdown. `coven`/`service` remain **exact / `in`-list only**. The removed `soulprint`/`state` CEL predicates have **no glob replacement** — RBAC visibility by host facts / incarnation state is dropped (see [ADR-047 amendment](../adr/0047-purview.md) / [migration `100`](../../keeper/migrations/100_rbac_drop_pattern_selectors.up.sql)).

**`trait.<key>=v`.** Dot-notation (`trait.owner=dba`), exactly one pair per condition; `<key>` is lower-case `[a-z][a-z0-9_.-]*`, the value is the ordinary exact-value character class. Semantics = exact scalar-equality (like `coven=`, not a CEL predicate). Multiple owners are an OR of two conditions (`trait.owner=dba OR trait.owner=platform`); `trait.<key> in (a,b)` and AND-narrowing over several keys are a follow-up.

The dimension applies to **both** resources ([ADR-080](../adr/0080-label-inheritance-union.md)): an incarnation matches on `incarnation.traits[key]`, and a host matches on its **effective** traits — its own `souls.traits` OR those of every incarnation it belongs to. The same holds for `coven=` on a host (own tags OR the tags and NAME of its incarnations). Labelling the incarnation is therefore the way to scope a whole instance including hosts that join it later; labelling a host scopes exactly that VM. Both resolve inside the SQL pushdown, so pagination and totals stay exact.

### Least-privilege subset over a boolean scope (NIM-128)

Granting a permission requires the issued scope `R` to be a **subset** of the union of the caller's own scopes for the same `(resource, action)` — otherwise it is a privilege escalation, so on any doubt the check is **fail-closed (DENY)**. Over a boolean expression the containment is (`subset.go`, [ADR-047 amendment](../adr/0047-purview.md)):

- **DNF single-conjunct subsumption.** `R` and each caller scope are normalised to DNF (disjunction of conjunctions of atoms `dimension op values`). `R ⊆ Hold` iff **every** disjunct `Rᵢ` of `R` implies some **single** allowing conjunct `Cⱼ` of the caller: for every dimension `Cⱼ` constrains, `Rᵢ`'s atom on that dimension ⊆ `Cⱼ`'s atom; a dimension `Cⱼ` constrains but `Rᵢ` leaves free → NOT covered (this is the key invariant against "dropping a constraint"). Per-atom containment: exact / `in`-list → value-set subset; `host = x` ⊆ `host matches g` → glob `g` matches `x`.
- **Exact fast-path (backward compat).** If a disjunct `Rᵢ` is only exact/`in`-list atoms (no glob), coverage is computed the old way — decomposed into `(key,value)` points and matched against the **union** of the caller's permissions — so purely-pointwise grants keep today's behaviour 1:1 (e.g. a caller holding `coven=a` and `coven=b` separately may still issue `coven in (a,b)`).
- **Conservative glob.** `host matches g_R` ⊆ `host matches g_C` is allowed in the first delivery only for **equal** globs (or an `Unrestricted`/bare host caller); general glob-subsumption and full boolean SAT are deferred.
- **Removed type in an OLD permission → fail-closed.** A stored permission still carrying `regex`/`soulprint`/`state` fails to parse after removal, so the enforcer snapshot build fails and the previous snapshot is held (never a silent unrestricted); the [data migration `100`](../../keeper/migrations/100_rbac_drop_pattern_selectors.up.sql) must run before the new binary. A grant that itself carries a removed type is rejected at input (422).

**Scoped full-wildcard `* on <scope>` (NIM-128, [ADR-047 amendment 2026-07-19](../adr/0047-purview.md)).** Beyond the bare `*` (cluster-admin), the full-wildcard accepts a boolean scope: **`* on X` is a "scoped super-admin"** — every action on every resource, but only where the predicate `X` holds (a context-less cluster-op, e.g. `operator.create` with no coven, is **denied** under a scoped `*`). Least-privilege containment: a caller may issue **`* on X`** only if it holds **bare `*`** or **`* on X'` with `X ⊆ X'`**; issuing **bare `*`** requires holding **bare `*`**. A caller holding `* on X` also covers granting a narrower `resource.action on Y` when `Y ⊆ X`, but is **not** treated as `Unrestricted` (fail-closed, no escalation). Self-lockout counts a cluster-admin **only via bare `*`** — a scoped `*` never counts as the surviving admin (§ Built-in roles / bootstrap invariant intact).

**Multi-value coven for incarnation operations.** The source `coven=` for incarnation is the *set* of declared `covens` (real stable tags only — the incarnation name is **no longer** part of it, [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)), and enforcer operates on one value per key. Therefore, permission is allowed if its `coven=` value matches **at least one** label from this set (OR by candidates). Example: incarnation `redis-prod` with `covens=[prod, dc1]` has an effective coven set of `{prod, dc1}`; the role `incarnation.run on coven=prod` matches it, the role `incarnation.run on coven=dev` does not. To grant on the incarnation's own name, use the `incarnation=` dimension (`incarnation.* on incarnation=redis-prod`) — `coven=redis-prod` no longer matches an incarnation just because it is named `redis-prod`.

> **History (ADR-008 amendment a).** Previously, this section declared the source `coven=` for the incarnation "`soulprint.self.covens` target host" - but incarnation endpoints do not resolve hosts at the RBAC gate stage (chicken-egg + volatility), and the code landed in the context only `{incarnation: name}` without `coven`/`service`. Because of this, the roles `incarnation.* on coven=…` / `on service=…` were silently NOT matched (enforcer for the missing key → deny). The source was the stable attributes of the incarnation itself (declared `covens` ∪ `name` + `service`), resolved from its string in Postgres. **Further superseded 2026-07-17 (NIM-124):** the `∪ name` part is dropped — the coven set is `covens` only, and scope by the incarnation's own name moves to the `incarnation=` dimension ([ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)).

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
2. Expand to a list of permissions (`rbac_role_permissions` by found roles), **keeping each permission attached to its role** - the role's `default_scope` is part of what the permission means (step 3).
3. For each permission: does the `<resource>.<action>` request match (taking into account the wildcard `*` in `<action>`); then resolve its **effective scope** and evaluate it against the context. The effective scope is a per-permission `on <expr>` if there is one, otherwise the role's `default_scope` ([ADR-047(a)](../adr/0047-purview.md)), and nothing at all for a bare `*` or a bare permission on a role that sets no scope (§ Selector grammar / [ADR-047(b)](../adr/0047-purview.md)). No effective scope → match; otherwise the predicate must hold on the context, and a dimension the context does not carry fails **closed**.
4. If **at least one** permission matches → allow. Otherwise → deny.

Steps 1–2 are based on the enforcer's **in-memory snapshot**, not live SQL (see § How an enforcer resolves).

> **A permission is not a decision (NIM-219).** Step 3 is the only correct reading of a permission, and it needs the ROLE. Judging a permission on its own - "no `on <expr>` → applies anywhere" - reads a bare right under a `default_scope`d role as unrestricted, which is how the scope-aware gate came to be **wider than** the read path it was supposed to mirror ([ADR-047 amendment 2026-07-28](../adr/0047-purview.md)). The rule lives in one function (`effectiveScope`, `keeper/internal/rbac/permission.go`) that both `Check` and `ResolvePurview` call, and the guard test asserts the two agree: `Check(aid, r, a, ctx) == nil` **⟺** `ResolvePurview(aid, r, a).Match(ctx)`.

## How the enforcer resolves

Interface `PermissionChecker.Check` **does not go to Postgres for every request** ([ADR-028(d)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)) - it matches the in-memory snapshot `map[AID][]*Role`. The snapshot is a map to **roles**, not to a flattened permission list, and that shape is required rather than incidental: a permission is judged under the `default_scope` of the role it came through (§ Semantics of conflict, step 3), so flattening away the role would erase the scope.

- **The source of the snapshot is the database.** The snapshot is built by three SELECTs using `rbac_roles` ⋈ `rbac_role_permissions` ⋈ `rbac_role_operators` (instead of the previous parsing `keeper.yml::rbac`). Permission lines are parsed `ParsePermission` when creating a snapshot.
- **Updating the snapshot - B2 (implemented).** The snapshot is invalidated via Redis pub/sub: each mutation of the role / permissions / membership publishes a signal to the topic **`rbac:invalidate`** (envelope `{origin_kid, at}`), and all nodes re-read the snapshot from the database. **Self-filter by KID**: the node ignores its own signal (pattern `applybus` - the publishing node has already updated the snapshot in the same mutation transaction). **B1 TTL-poll remains fallback**: background goroutine (`rbac.Holder`) rereads the snapshot from the database at a fixed interval (`DefaultRefreshInterval` = 10s) - best-effort insurance in case of Redis being unavailable/lost signal; If the reread fails, the previous snapshot remains + warn. The aging window (seconds until the next TTL reread when the signal is lost) is acceptable: role/membership mutations are rare.
- **Self-lockout checks - from the database, not from the snapshot.** The invariant "≥1 active `*`-admin will remain" (see § Built-in roles) is checked **not** by the enforcer's in-memory snapshot, but by direct SQL under `SELECT … FOR UPDATE` on `rbac_role_operators` / `rbac_role_permissions` / `rbac_roles` / `operators` in the same transaction as the mutation (`rbac_roles` both filters out derived roles and locks against a concurrent re-rooting — see § Derived roles). The snapshot becomes obsolete in the TTL window - a solution to it would give a staleness hole (you can remove the last admin if the snapshot still "remembers" the already-revoked second one); `FOR UPDATE` additionally serializes concurrent lockout operations on different nodes. See § Role Management.
- **RBAC outside the hot-reload-config-path.** The snapshot is rebuilt from the database (Redis pub/sub-invalidation via topic `rbac:invalidate` + TTL-poll fallback), **not** by `SIGHUP` / config-swap ([ADR-021](../adr/0021-hot-reload-config.md), clarified [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)). Revocation of role/membership (`role.revoke-operator` / `role.delete`) - `DELETE` in the database + re-reads the snapshot, effect for all future checks no later than the TTL window (separate from the irrevocability of the active JWT until `exp` - [ADR-014(d)](../adr/0014-operator-identity.md)).
- **`Check` interface is unchanged** - only the source of the snapshot and the mechanism for updating it change, not the verification signature.

## Role management (REST + MCP)

RBAC-CRUD (roles, permissions, membership) managed via OpenAPI / MCP - Phase 2 [ADR-028(e)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) (not YAML editing). One source of truth is `rbac.Service`: REST handlers (`/v1/roles`) and MCP tools (`keeper.role.*`) - thin transport wrappers over it, business invariants (builtin boundary, self-lockout, name/permission validation) live in the Service.

### REST `/v1/roles`

Six endpoints. RBAC check - in middleware (`role.*`-permission without selector), before the handler. Errors - RFC 7807 ([operator-api.md → Error types](operator-api.md)).

| Method + path | Permission | Body/path | Success | Error codes |
|---|---|---|---|---|
| `POST /v1/roles` | `role.create` (+ `role.create-root` when no `parent_role`) | body `{name, description?, permissions[]}` | `201` (body empty) | `403 forbidden` (least-privilege: right outside the caller set; or a parentless role without `role.create-root`, § Root roles); `409 role-already-exists`; `422 validation-failed` (broken `name` / `permission`); `400 malformed-request` |
| `GET /v1/roles` | `role.list` | — | `200 {items: [...]}` (filtered to the caller, § Catalog visibility) | `500 internal-error` |
| `DELETE /v1/roles/{name}` | `role.delete` | path `name` | `204` | `403 forbidden` (may not administer a role whose rights the caller could not grant, § Who may administer a role); `404 role-not-found`; `409 role-builtin`; `409 would-lock-out-cluster` |
| `PATCH /v1/roles/{name}/permissions` | `role.update` (+ `role.create-root` when the result has no parent) | path `name` + body `{permissions[]}` (replace) | `204` | `403 forbidden` (least-privilege: a right the PATCH newly grants is outside the caller's set; or minting a parentless role, § Root roles); `404 role-not-found`; `409 role-builtin`; `409 would-lock-out-cluster`; `422 validation-failed`; `400 malformed-request` |
| `POST /v1/roles/{name}/operators` | `role.grant-operator` | path `name` + body `{aid}` | `204` | `403 forbidden` (least-privilege: role contains a right outside the caller's set); `404 role-not-found`; `404 not-found` (AID does not exist); `422 validation-failed` (empty/broken AID); `400 malformed-request` |
| `DELETE /v1/roles/{name}/operators/{aid}` | `role.revoke-operator` | path `name`, `aid` | `204` | `404 not-found` (no pair `(name, aid)`); `409 would-lock-out-cluster`; `422 validation-failed` (broken path-AID) |

- **`GET /v1/roles` items[]** — `{name, description, builtin, permissions[], operators[]}`; `permissions` / `operators` are serialized by a non-nil array (`[]`, not `null`). The list holds only the roles the caller may see (§ Catalog visibility) — an operator is NOT told how many were withheld.
- **`grant-operator` idempotent** - re-binding the same pair `(name, aid)` - no-op (`204`).
- **`granted_by_aid`** for grant is taken from the JWT-claim caller.

### MCP `keeper.role.*`

1:1 with REST: `keeper.role.<action>` ↔ `role.<action>` ↔ one endpoint `/v1/roles`. Input schemes and mapping errors RFC 7807 → MCP-tool error - in [mcp-tools/roles.md](mcp-tools/roles.md). Mutating tools return an empty output object (`{}`), `keeper.role.list` - `{roles: [...]}`.

### Self-lockout invariant: four ways

Invariant [ADR-028(f)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) / § Built-in roles: after the operation, **≥1 active** (`revoked_at IS NULL`) Archon with an effective `*`-permission must remain in the cluster. Four mutations can break it:

| Path | When the check is triggered | What counts as "survivors" |
|---|---|---|
| `operator.revoke` | Review of the Archon holding `*` ([ADR-013(c)](../adr/0013-bootstrap-archon.md)). | Active `*`-admins, except the revoked AID. |
| `role.delete` | The role being removed is a **source**: bare `*` on a **plain** role. | Active `*`-admins through **other** plain roles (≠ to be deleted). |
| `role.update` | The role **stops** being a source: `*` removed from the set, **or** the role becomes derived (`parent_role` set). | Active `*`-admins through other plain roles. If the result is still a plain `*` role, no check is needed. |
| `role.revoke-operator` | The role is a source and membership is removed. | Active `*`-admins after excluding exactly the pair `(role, aid)` - AID remains if it holds `*` through another role; the role remains for other AIDs. |

A **source** is a bare `*` on a role with `parent_role IS NULL`. A derived role never counts, in either column — see § Derived roles.

Violation → `409 would-lock-out-cluster` (common problem-type for operator and role paths, [naming-rules.md → Error codes](../naming-rules.md#error-codes)). Self-lockout - "downward" protection (you cannot lock admin-set). A separate § Invariant least-privilege; `role.create` / `role.grant-operator` obey it, although self-lockout does not.

**Check - from the database under `FOR UPDATE`, not from the enforcer snapshot** (see § How an enforcer resolves). The control SQL takes a row-lock on `rbac_role_operators` / `rbac_role_permissions` / `operators` in the same transaction as the mutation: excludes the target role/pair from the sample and checks that ≥1 row remains. The snapshot becomes outdated on the TTL window - checking against it would be a hole; `FOR UPDATE` serializes parallel lockout operations (two txs that unlock `*` in different ways cannot both pass).

### Catalog visibility (`role.list`)

`role.list` is the right to read the role catalog — **not** the right to read the cluster's privilege map. The two are different because a role carries more than its name: its permission set, its scope and the AIDs holding it. Read together, a full catalog answers "who administers what", "which covens and services exist" and "which operator to attack to reach `*`". Before NIM-202 every holder of `role.list` got all of it, including a `coven`-scoped operator who administers one team.

**The rule: a caller sees a role exactly when the caller could GRANT what that role grants.** Formally, `GET /v1/roles` / `keeper.role.list` return role `R` iff the caller's effective rights cover `effective_perms(R)` under `effective_scope(R)` — the same containment as § Invariant least-privilege, `callerHolds` unchanged ([`role_visibility.go`](../../keeper/internal/rbac/role_visibility.go)).

| Caller | Sees |
|---|---|
| bare `*` (cluster-admin) | the whole catalog — the administrator's view is unchanged |
| `role.list-all` (or `role.*`) | the whole catalog — the explicit right, below |
| `* on <expr>` (scoped super-admin) | roles within that predicate; **not** a role with a bare `*` |
| scoped operator (e.g. `incarnation.* on coven=dba`) | their own role, roles derived from it, other roles inside `coven=dba` |
| an operator holding nothing | only roles that grant nothing |

Why the write-side predicate rather than a visibility rule of its own:

- it is already a boundary the caller cannot cross, so showing what is inside it leaks nothing — a visible role is one the caller could have created, been granted, or derived from;
- **one definition of `⊆` for the subsystem** ([ADR-078(c)](../adr/0078-rbac-derived-roles.md)). A second, read-only notion of "close enough to show" is free to disagree with the decision layer, and in this direction a disagreement is a leak;
- nothing has to maintain a list of "roles that are safe to show".

Details that follow from the rule:

- **Judged on the EFFECTIVE form**, never the stored rows: the effective form is what the role actually grants ([§ Derived roles](#derived-roles-parent_role)). A derived role carrying a row its parent stopped covering grants nothing through that row and is not hidden for carrying it.
- **A role that grants nothing is visible to everyone.** It exposes no privilege, and any caller could create the same empty role.
- **Filtering happens in `rbac.Service`, not in a transport**, so REST and MCP answer a given caller identically. A caller-less read is refused (`ErrPermissionNotHeld`) rather than falling back to the whole catalog.
- **The count is not published.** A truncated list does not say how many roles were withheld; "N of M" would itself be a fact about the catalog.

#### The full catalog — `role.list-all` (NIM-203)

Real readers must see roles they hold nothing of: an auditor, a security review, first-line support identifying who holds what. No coverage rule can express that, so it is an explicit right.

`role.list-all` is a **breadth modifier, not a route.** `role.list` still gates `GET /v1/roles` / `keeper.role.list`; `role.list-all` decides how much comes back. An auditor therefore needs **both** (or `role.*`), and granting `role.list-all` alone opens nothing. This is the `operator.read` pattern — a catalog name with no endpoint of its own.

- **Checked with the ordinary containment** against the caller set the filter already loaded — no extra query, and `*` / `role.*` cover it for free, so an existing RBAC administrator needs no re-grant.
- **NoSelector, enforced by construction.** The required permission is *bare*, so the subset check demands the caller be UNRESTRICTED on it: a scoped `role.list-all on coven=X` yields the ordinary filtered view. That is the only sound reading — the scope grammar has no `role=` dimension, so such a scope selects nothing.
- **All of it, not a redacted form.** The full catalog means permission sets and operator lists too: an auditor who cannot see a role's rights cannot audit it, and a names-only view would still map the organisation while being useless for the job.
- **No `rbac-auditor` builtin.** The catalog keeps exactly one builtin (`cluster-admin`); a builtin can be neither updated nor deleted (`409 role-builtin`), which is the wrong shape for a role every organisation defines differently. Build it: `role.list` + `role.list-all` + `audit.read`.
- Appears in `GET /v1/me/permissions` like any other action (`{resource: "role", action: "list-all"}`), so the UI reads it rather than inferring it.

> **Adjacent surface:** `GET /v1/synods` had the same leak one step weaker — role NAMES rather than their permission sets — and is closed the same way (§ Synod catalog visibility, NIM-216). `GET /v1/operators` was never affected: it publishes the Archon registry, not the role↔operator mapping.

### Invariant least-privilege (subset-check)

Separate from self-lockout protection - against **vertical escalation of privileges**. Without it, an operator with `role.create` + `role.grant-operator` (but **without** `*`) could: create a role with `permissions: ["*"]` → bind it to himself via `role.grant-operator` → become an effective cluster-admin. That is, the rights to *manage roles* would be converted into *any* rights.

**Invariant: an operator cannot issue permission through a role that it does not itself have.** Three mutations are subject to it:

| Path | What is checked against the effective dialing of a caller |
|---|---|
| `role.create` | **each** permission of the new role. |
| `role.update` | **each right the PATCH newly grants** — the role's effective rights AFTER, compared with BEFORE by coverage. Not the rows that changed: a PATCH that moves the role's **ceiling** without touching a single row (clearing `parent_role`, replacing `default_scope`, re-pinning the delta) widens every bare permission under it, and a row diff reports that as nothing at all (NIM-230). Removing rights, and narrowing them, add nothing and are not limited here — but reaching the role at all is a separate question (§ Who may administer a role). |
| `role.delete` | nothing is granted by a deletion, so this floor has nothing to measure — the caller is bounded by § Who may administer a role instead. |
| `role.grant-operator` | **each** permission **granted role** (otherwise bypass: cluster-admin created a powerful role, suboperator with `role.grant-operator` assigned it to himself/other and rose). |

- **Coverage** - the same implication semantics as `Check` (§ How enforcer resolves): caller "has" permission `P` if at least one of its permissions matches `P` (taking into account `*` → covers everything; `resource.*` → covers any action of this resource; selector `on key=a,b` → caller must cover **every** value). Only the owner of `*` can issue a full-wildcard `*`.
- **cluster-admin (`*`)** passes any such check - its set covers everything.
- **Source of caller set is DB** (same mutation transaction, filter `operators.revoked_at IS NULL`), not enforcer snapshot: same-tx-read fresher than TTL snapshot. Read-only (without `FOR UPDATE`): subset-check is an authorization gate, and not a consistency invariant like self-lockout, so it does not add row-locks and does not affect the deterministic lock order of the self-lockout kernel (no deadlock risk).
- **Bootstrap-grant** (`keeper init`, `granted_by_aid IS NULL`, without caller-Archon) subset-check fails - it binds the first Archon to `cluster-admin` before any subject appears.
- Violation → `403 forbidden` (REST `TypeForbidden` / MCP `forbidden`), sentinel `ErrPermissionNotHeld` - separate from `ErrPermissionDenied` ("no right to the operation itself", checked by middleware/tool before Service).

Self-lockout and least-privilege **coexist**: the first prohibits locking the admin-set "down", the second prohibits granting the "up" right. They check different things and don't conflict in order.

### Who may administer a role (`role.update` / `role.delete`)

Creating a role was always bounded — you cannot put in what you do not hold. **Editing and deleting one were not.** Both rights are `NoSelector`, so any holder could rewrite or drop any non-builtin role in the cluster, including roles far above their own rights; `role.delete` did not take a caller at all and ran no caller-side check of any kind.

The least-privilege floor did not object, and correctly so on its own terms: taking rights away grants nothing, which is why trimming had been free since ADR-028. What that leaves open is not escalation but **demolition** — one holder of `role.update` can zero out every team's access, and self-lockout only notices when the last `*` admin would go.

**The rule: a caller may administer a role exactly when the caller could GRANT what that role grants** ([`role_admin.go`](../../keeper/internal/rbac/role_admin.go)) — the same containment as § Invariant least-privilege and § Catalog visibility, applied to the role's **current** resolved form. Refusal → `403` (`ErrPermissionNotHeld`).

The model behind it ([ADR-078 §(m)](../adr/0078-rbac-derived-roles.md), from NIM-201) is that a role belongs to its **parent**, not to its author. That model is a consequence of the rule rather than a second mechanism:

| Target | Who administers it |
|---|---|
| a role derived from `P` | every holder of `P` — a child's rights are contained in its parent's, transitively and through a Synod |
| a plain role | whoever could have created it (`role.create-root` already selects for that) — it is not orphaned |
| a role that grants nothing | anyone: there is no privilege to protect, mirroring § Catalog visibility |
| anything, for a bare `*` | unchanged — a cluster-admin covers everything |

Three questions now live side by side and are deliberately **not** merged: **see** (§ Catalog visibility), **grant** (§ Invariant least-privilege) and **administer** — all three read the same `callerHolds`, asked about different things. They nest rather than conflict: administering a role implies covering it, which implies seeing it.

**Two consequences are deliberate, and one is breaking.** Trimming a role you do not cover is now refused — that reverses "cutting someone else's role is not escalation", true as far as escalation goes and exactly the demolition surface this closes. Trimming a role you DO cover is untouched. And `role.update` / `role.delete` remain the right to **reach the endpoint**, never the reach itself: the boundary is in `rbac.Service`, so REST and MCP cannot drift apart.

#### Taking a binding apart (NIM-285)

The same surface ran one door along: `role.revoke-operator`, `synod.remove-operator` and `synod.revoke-role` carried **no caller at all** — the field was absent from the service inputs — so any holder could strip any archon of any role, or empty any group. Self-lockout was the only obstacle, and it fires when the **last** `*` admin would go.

**Each revoke asks for exactly what its matching grant asks for.** Symmetry rather than a fresh judgement: the pair is one authorization surface seen from two directions, and any asymmetry between them is a gap by construction.

| Revoke | Measured against | Mirrors |
|---|---|---|
| `role.revoke-operator` | the role's effective rights | `role.grant-operator` |
| `synod.remove-operator` | the group's **whole** bundle — a member receives all of it ([ADR-049 §f](../adr/0049-synod.md)) | `synod.add-operator` |
| `synod.revoke-role` | the revoked **role's** rights | `synod.grant-role` |

Removing a right still grants nothing, so none of this is escalation either. It is the same demolition surface, closed by the same argument.

### Derived roles (`parent_role`)

A role may name another role in **`rbac_roles.parent_role`** ([ADR-078](../adr/0078-rbac-derived-roles.md)). The named role is its **parent**; the naming role is **derived** from it and can never exceed it. `parent_role IS NULL` is a **plain** role — what every role is today, with unchanged semantics.

A derived role is a role, not a new entity: same table, same `role.*` family, same endpoints. It stores the parent's name plus a **delta**, and the delta is the role's existing `default_scope` (§ Selector grammar) read differently:

| Role | Meaning of its `default_scope` |
|---|---|
| plain (`parent_role IS NULL`) | the role's **absolute** scope ([ADR-047 §a](../adr/0047-purview.md)) |
| derived (`parent_role` set) | the **attenuating delta**, conjoined with the parent's effective scope |

```
effective_scope(role) = effective_scope(parent) AND default_scope(role)
effective_perms(role) = own_perms(role) ∩ effective_perms(parent)
```

A plain role's parent side is the unrestricted top, so the same formula gives plain ADR-047 behaviour. `∩` is the containment of § Invariant least-privilege (so `incarnation.*` covers `incarnation.get`) — one definition of "⊆" for the whole subsystem.

**Example.** Parent `dba` holds `redis.restart` + `redis.read` at `coven=dba`; child `dba-aboba` sets `parent_role: dba`, keeps only `redis.read`, and adds the delta `trait.project=aboba` → its effective right is `redis.read` at `coven=dba AND trait.project=aboba`. Move the parent to `coven=dbaas` and the child follows. **On a `track` role the delta stores only the ADDED narrowing** and never repeats the parent's predicate — a child that restated `coven=dba` would resolve to the empty set the moment the parent moved. Repeating it is exactly what `pin` does, deliberately and on the record (§ Tracking vs pinning).

**Why the conjunction and the intersection.** The scope grammar has no `NOT`, so `AND`-ing only narrows: attenuation of scope is structural rather than a rule to remember. The permission side is an **intersection, not a copy**: a child's rows are never implicitly the parent's, because a permission added to the parent would then appear on every descendant — a widening cascade. Narrowing cascades and only narrowing: remove a permission from the parent and it drops out of every descendant at the next snapshot build.

**Resolution.** The chain is flattened **once**, when the enforcer snapshot is built (§ How the enforcer resolves) — never walked per request, so a check costs the same as today. Cascade needs no extra machinery: the snapshot is already rebuilt on any role mutation ([ADR-028(d)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres) TTL poll + `rbac:invalidate`).

`default_scope` is a **default**; the parent's effective scope is a **ceiling**. A per-permission `on <expr>` overrides the role's own `default_scope` (§ Selector grammar) and a `*` ignores it entirely — but neither escapes the parent's ceiling, which is conjoined onto every permission of a derived role. A `*` on a role derived from a scoped parent is therefore a **scoped** super-admin, not an unrestricted one. A bare permission is the exception: it stays bare and inherits the role's resolved scope, because writing the ceiling onto it would replace — and so discard — the child's own delta.

Anything not **provably** inside the parent is dropped rather than clipped. An undecidable glob containment (`host matches web-0?` inside `host matches web-*`, left conservative by NIM-128 §C.5) costs the child that permission; a chain whose conjoined scope cannot be normalized within the DNF caps builds no enforcer at all.

**Graph rules**, enforced in the schema ([migration 102](../../keeper/migrations/102_rbac_roles_parent_role.up.sql)) so they hold for every write path, and re-checked in Go at snapshot build:

| Rule | Behaviour on violation |
|---|---|
| a role cannot be its own parent | refused (`SS001`) |
| a chain cannot close on itself (`A → B → A`) | refused (`SS001`) |
| a chain cannot exceed **4 roles** (3 parent hops) | refused (`SS002`) |
| the parent must exist | refused (self-FK) |
| **deleting a role that is still someone's parent** | refused — `ON DELETE RESTRICT`, `ErrRoleHasChildren` → `409` |

Re-parenting is checked from both ends — the ancestors above the moved role and the subtree already hanging below it. A catalog whose graph is somehow broken (a hand-edited row, a restore) builds **no** enforcer, degrading exactly as an unparseable permission already does: on a TTL refresh the previous enforcer is kept with a warn, at startup the Keeper refuses to come up.

**The orphan policy is fail-closed on purpose.** Clearing the parent would turn the child's delta into an absolute scope and drop the parent's narrowing — a **widening**, i.e. escalation; re-rooting to the grandparent widens by definition; cascading the delete silently strips membership. Refusing is the only option that changes nobody's rights unasked. The operator re-parents or deletes the children explicitly.

**Least-privilege still applies on top.** Creating or updating a derived role must satisfy **both** `child ⊆ parent` (structural) **and** the caller holding the parent (§ Invariant least-privilege, unchanged) — otherwise an operator with `role.create` could derive from a role far above their own rights. It is the **parent** the caller must cover, not merely what the child asks for today: the cascade will carry every later widening of that parent into the child. Both halves re-run on **every** role update, not only the one that sets `parent_role` — otherwise the create gate would be one PATCH wide. A child beyond its parent → `403` (`ErrRoleExceedsParent`), naming **every** row the parent fails to cover so the role can be repaired in one PATCH.

**The floor judges the role's RIGHTS, not its rows** ([ADR-078(h) amendment](../adr/0078-rbac-derived-roles.md)). On a derived role the least-privilege comparison resolves the child against its parent first: a bare `incarnation.get` under a `coven=dba` ceiling is weighed as `incarnation.get on coven=dba`, which is what the role would actually grant. Comparing the stored rows made the floor stricter than the authorization it protects — a delegator scoped to their own coven could not create a role squarely inside it, and the only way through was to restate the parent's predicate in the delta, silently turning a tracking role into a pinned one. The same resolution applies when a role is **bound** to an operator or bundled into a Synod: both confer what the role grants. A **plain** role is unaffected — with no parent there is no ceiling, and a bare permission is still a request to grant it unrestricted.

**Self-lockout counts PLAIN roles only.** A derived role is **never** a source of cluster-admin (§ Self-lockout invariant), including when its chain happens to resolve to an unrestricted `*` right now — that depends on a parent any other mutation may narrow. Two halves: the lockout probes filter `parent_role IS NULL` when counting survivors, and a role that **becomes derived** runs the same lockout check that removing its `*` would. Without the second half the invariant is trivially bypassable — keep the permission set identical, add a parent, and the cluster is locked out with no rule visibly firing.

**Several roles on one operator — derivation narrows a ROLE, not an OPERATOR.** An operator's effective rights remain the **union** across their roles (§ Semantics of conflict — OR among allows). A derived role resolves to its attenuated rights first and then joins that union like any other role, so a child can never carry in more than its parent allows.

The consequence is the opposite of what "I gave them the narrow role" suggests, and is worth stating plainly:

| Operator holds | Effective rights |
|---|---|
| only `dba-aboba` (derived from `dba`) | `dba`'s rights ∩ the child's own rows, at `coven=dba AND trait.project=aboba` |
| **both** `dba` and `dba-aboba` | **`dba`'s full rights** — the narrow role adds nothing and restricts nothing |

To actually confine an operator, revoke the wide parent from them; adding a narrower derived role on top is **not** a restriction mechanism. Attenuation bounds what a role may contain, not what its holder ends up with.

This is also why a new derived role's parent is **one explicitly chosen role**, never "the caller's rights": a caller's union is wider than any single role they hold, so deriving against the union would mint a role broader than any role they could point at. The UI shows the ceiling of the **selected parent** for the same reason.

#### Tracking vs pinning (`scope_mode`)

Two opposite intentions produced an identical row, so the intent is stored explicitly in **`rbac_roles.scope_mode`** ([migration 105](../../keeper/migrations/105_rbac_roles_scope_mode.up.sql), [ADR-078(k)](../adr/0078-rbac-derived-roles.md)):

| Mode | The delta holds | The parent widens | The parent narrows |
|---|---|---|---|
| **`track`** (default) | only the ADDED narrowing | the child widens with it | the child narrows |
| **`pin`** | the parent's effective scope **materialized** into it at write time | the child does **not** move | the child narrows |

`scope_mode` is `NULL` exactly when `parent_role` is (a CHECK holds the two together), so a plain role has no mode and cannot be given one.

**Resolution does not branch on the mode** — it is `effective_scope(parent) AND default_scope(role)` for both. The modes differ only in what was written into the delta, which is why a pinned role still narrows with its parent: pinning is a defence against drift, never an escape from attenuation. Pinning against an already-unrestricted parent is vacuous (there is no room to widen into), and a pin **can** expire: move the parent sideways and the frozen predicate conjoins with the new one to the empty set — the condition § Inert permissions makes visible.

Sending `pin` again RE-PINS onto the parent's scope as of now. An edit that does not mention `scope_mode` never re-pins: an ordinary permission PATCH must not quietly become an authorization change.

#### The cascade is confirmed, not silent

Editing a role changes every role derived from it. Any mutation that moves the **effective rights** of anything below it — either direction, any depth — is refused with `409` (`role-cascade-not-confirmed`) unless the request carries **`confirm_cascade`**. The refusal names the derived roles that would gain rights, the ones that would lose them, and how many active operators hold any of them (directly or through a Synod).

Both directions are reported and neither is the safe one: widening hands out access nobody granted directly, narrowing takes access away, possibly mid-incident. A mutation the subtree does not feel passes silently — being a parent is not itself a consequence. A pinned child is absent from the widening half by construction.

**The edited role is normally not part of its own blast radius**, and for one reason: the request carries its permission list, so whatever it ends up granting is what the operator typed. Growing a role never prompts. The exception is a `PATCH` that takes the role's **parent away** — there the rows in the request can be identical to the stored ones and the rights still move, because the ceiling they resolved under is gone. Everyone holding that role gains access nobody granted them, and the operator has no other way to learn how many that is, so the role joins its own report there and nowhere else. Childlessness does not exempt it: a derived role with no children of its own is still reported when it loses its parent (NIM-252). An un-parenting that widens nothing — an unrestricted parent, or a delta that already stated the whole ceiling — is not reported, because the gate reads rights, not verbs.

The gate is **advisory in nature**: skipping it would produce exactly the same rights. It is a gate on the operator's attention, and it runs **last**, so a refusal about the caller's own rights is always reported before one that asks them to decide. `confirm_cascade` is recorded in the audit payload whenever it is sent — it is the operator accepting a change to roles other than the one the record names.

#### Inert permissions

A row the chain no longer covers stays in `permissions`, drops out of `effective_permissions`, and is published under **`inert_permissions`** ([ADR-078(l)](../adr/0078-rbac-derived-roles.md)). Without it, "a role listing two permissions and granting none" is indistinguishable from "a role somebody deliberately emptied", and the two call for opposite responses. The set is published rather than derivable: subtracting one list from the other compares a stored form against a resolved one.

**The emptiness is correct.** `incarnation.*` under a parent narrowed to `incarnation.get` resolves to **nothing**, not to `incarnation.get` — clipping the wildcard to the parent's current set would mean a permission later added to the parent silently appears on every descendant holding a `*`, the widening cascade the intersection exists to forbid.

**API surface.** Derivation is not a separate entity, so it adds no endpoint and no `role.*` permission — the existing role surface carries a few more fields ([ADR-078(a)](../adr/0078-rbac-derived-roles.md)):

| Where | Field | Semantics |
|---|---|---|
| `POST /v1/roles`, `keeper.role.create` | `parent_role` | omitted/null → a plain role; a name → derive from it. `default_scope` then means the **delta** |
| `PATCH /v1/roles/{name}/permissions`, `keeper.role.update` | `parent_role` | PATCH presence, mirroring `default_scope`: **key absent** → derivation untouched; present → replaced (`null` makes the role plain again) |
| `GET /v1/roles`, `keeper.role.list` | `parent_role` | the role's ceiling; absent/empty → a plain role |
| `GET /v1/roles`, `keeper.role.list` | `effective_permissions`, `effective_scope` | the role **as resolved** against its chain |
| `POST /v1/roles`, `PATCH …/permissions`, `keeper.role.create` / `.update` | `scope_mode` | `track` (default) / `pin`; on update it follows PATCH presence, and sending `pin` re-pins |
| `PATCH …/permissions`, `keeper.role.update` | `confirm_cascade` | accept a change that moves the roles derived from this one; not presence-sensitive |
| `GET /v1/roles`, `keeper.role.list` | `scope_mode`, `inert_permissions` | the delta's intent, and the stored rows the chain no longer covers |

The read side returns each role in **both** forms — as stored (`permissions` / `default_scope`) and as resolved (`effective_*`) — and the difference between them is exactly what an operator needs to see: a stored row the parent does not cover is present in the first and absent from the second, i.e. written but granting nothing. Resolution runs the same code the enforcer runs, so no consumer re-derives inheritance from `parent_role` and none can arrive at a wider answer than the decision layer. A catalog whose graph does not resolve fails the read rather than serving the unattenuated rows.

Refusals on the write side: `404` for a parent outside the catalog, `403` for a role beyond its parent (`ErrRoleExceedsParent`) or beyond the caller's own rights, `422` for a cycle / an over-deep chain / an unresolvable scope / a `scope_mode` without a parent, `409` on deleting a role that still has children and `409` (`role-cascade-not-confirmed`) on an unconfirmed cascade.

The audit records of `role.created` / `role.permissions-updated` carry the derivation, not just the permission list — on a derived role the list is the delta and the ceiling lives in `parent_role`. On create `parent_role`, `default_scope` and `scope_mode` are always present (`null`/empty on a plain role); on update they appear only when the request sent them, so an absent key reads as "untouched" and a present `null` as "cleared".

> **Status.** The model, its guards, chain resolution, the write-time gate, the API surface, the `scope_mode` intent, the cascade confirmation and inert rows are all in place (NIM-179 + NIM-180 + NIM-181 + NIM-198/199/200). Remaining: the web selector and the "you inherit X, you cannot widen it" panel (NIM-182) — until then a derived role is created through the API rather than the UI.

### Root roles (`role.create-root`)

A derived role **follows** its parent: narrow the parent and the child narrows at the next snapshot build. A **plain** role follows nothing. It is a snapshot of privilege that outlives whatever its author held: revoke their `coven=dba` role tomorrow and the plain role they minted keeps granting `coven=dba` to whoever holds it, with no rule having visibly fired.

The least-privilege floor does not catch this — every permission in that role **was** covered when it was written. The floor bounds what may go into a role, not whether the result keeps tracking the rights it came from. So minting a parentless role is its own action:

| Caller | `POST /v1/roles` with no `parent_role` |
|---|---|
| bare `*`, `role.*`, or `role.create-root` | allowed |
| any other holder of `role.create` | **`403`** (`ErrRootRoleNotPermitted`) — derive from a role you hold |

**The default becomes: derive from a role you hold, and the cascade does the rest.** The ceiling is then ONE named role — an organisational object, administered by everyone who holds it — and never the creator: a creator's union across their roles is wider than any single role they could point at ([ADR-078(e)](../adr/0078-rbac-derived-roles.md)), and a person can be revoked or leave, which must not silently strip or widen the roles they once wrote.

Details:

- **Gated on the SHAPE OF THE RESULT, not the verb.** It fires wherever a caller puts privilege into a role that will have no parent: on create, on a `PATCH` that clears `parent_role`, and on a `PATCH` that grows an already-plain role. Gating only creation would leave it one PATCH wide — the trap [ADR-078(h)](../adr/0078-rbac-derived-roles.md) already records for the attenuation gate.
- **Clearing `parent_role` is judged on the WHOLE resulting set** — including when the rights come out numerically unchanged, because the child's delta already restated the parent's predicate. Before the PATCH everything the role granted was tracked; after it, nothing is, and the tracking is the entire subject of this gate. A role that was ALREADY plain is judged only on what it gained, which is why trimming one stays free (NIM-230).
- **A parentless role that grants NOTHING is free.** There is no privilege to strand; this mirrors § Catalog visibility, where an empty role is visible to everyone.
- **Trimming stays ungated.** Removing permissions adds nothing, so nothing is minted — unchanged from § Invariant least-privilege.
- **`*` and `role.*` cover the action for free**, so a cluster-admin and an existing RBAC administrator need no re-grant. Only a role that enumerates `role.create` explicitly is affected.

> **What this does NOT do:** taking a parent role away from an operator does not remove the roles they derived from it. Those roles belong to the parent, not to their author, and other holders of that parent administer them.

### Builtin-border

`cluster-admin` (`builtin=true`, § Built-in Roles):

- `role.delete` / `role.update` above it - **prohibited** → `409 role-builtin` (builtin check goes **before** self-lockout; builtin is more important).
- `role.grant-operator` / `role.revoke-operator` above it - **allowed** (otherwise you cannot add a second admin or remove an erroneously assigned one), with the same self-lockout on revoke.

## Managing Archon Groups (Synod)

**Synod** ([ADR-049](../adr/0049-synod.md)) - intermediate level of the **Archon → Synod → Roles** model: a group of archons **banding a set of roles**. Instead of granting roles to each archon individually, the operator assembles a group with the required bundle of roles and adds archons to it - all members automatically receive the entire bundle.

- **Synod does NOT carry its own scope.** Scope (the boolean `on <expr>` over `coven` / `service` / `incarnation` / `host` / `trait`, § Selector grammar) lives on roles ([Purview](../adr/0047-purview.md) / [ADR-047](../adr/0047-purview.md)); Synod only groups already-scoped roles. Group-scope ADR-049 is not introduced (additive for the future).
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
| `GET /v1/synods` | `synod.list` | — | `200 {items: [...]}` (filtered to the caller, § Synod catalog visibility) | `500 internal-error` |
| `PATCH /v1/synods/{name}` | `synod.update` | path `name` + body `{description}` (required, 1..1024 characters) | `204` | `404 synod-not-found`; `422 validation-failed` (empty `description` / limit exceeded); `400 malformed-request` (broken JSON, unknown field - including `name` in the body) |
| `DELETE /v1/synods/{name}` | `synod.delete` | path `name` | `204` | `404 synod-not-found`; `409 synod-builtin`; `409 would-lock-out-cluster` |
| `POST /v1/synods/{name}/operators` | `synod.add-operator` | path `name` + body `{aid}` | `204` | `403 forbidden` (least-privilege: group bundle contains a right outside the caller's set); `404 synod-not-found`; `404 not-found` (AID does not exist); `422 validation-failed` (empty/broken AID); `400 malformed-request` |
| `DELETE /v1/synods/{name}/operators/{aid}` | `synod.remove-operator` | path `name`, `aid` | `204` | `404 not-found` (no pair `(name, aid)`); `409 would-lock-out-cluster`; `422 validation-failed` (broken path-AID) |
| `POST /v1/synods/{name}/roles` | `synod.grant-role` | path `name` + body `{role}` | `204` | `403 forbidden` (least-privilege: role contains a right outside the caller's set); `404 synod-not-found`; `404 role-not-found`; `422 validation-failed` (empty `role`); `400 malformed-request` |
| `DELETE /v1/synods/{name}/roles/{role_name}` | `synod.revoke-role` | path `name`, `role_name` | `204` | `404 not-found` (no bundle pair `(name, role)`); `409 would-lock-out-cluster` |

- **`GET /v1/synods` items[]** — `{name, description, builtin, roles[], operators[]}`; `roles` / `operators` are serialized by a non-nil array (`[]`, not `null`), sorted deterministically. The list holds only the groups the caller may see (§ Synod catalog visibility) — an operator is NOT told how many were withheld.
- **`add-operator` / `grant-role` are idempotent** - re-adding the same pair is a no-op (`204`).
- **`added_by_aid` / `granted_by_aid`** are taken from the JWT-claim caller; for seed/bootstrap lines - `NULL`.

### Synod catalog visibility (`synod.list`)

`synod.list` is the right to read the group catalog — **not** the right to read the cluster's delegation structure. A group carries the two things § Catalog visibility had just taken off `role.list`: **who is in it**, the readiest answer to "who do I attack to reach X", and **which roles it bundles**, i.e. which packages of rights exist and who was handed them. After NIM-202 this was the last way to read a piece of the privilege map out of a list.

The rule needs no invention, because the write side already states it. By [ADR-049(f)](../adr/0049-synod.md) `synod.add-operator` demands the caller hold the effective rights of **every** role the group bundles — a member receives the whole bundle. Read backwards:

**A group is visible ⟺ the caller may see every role it bundles** — that is, "I see the group exactly when I could have put someone in it", the same shape as "I see the role exactly when I could have granted it". It is literally the role rule applied across the bundle ([`synod_visibility.go`](../../keeper/internal/rbac/synod_visibility.go)), so there is no second notion of "close enough to show" that could drift from the decision layer.

Two consequences fall out rather than being decided:

- **A group that bundles NOTHING is visible to everyone.** The quantifier runs over an empty bundle. Same as an empty role, and for the same reason: no privilege is exposed.
- **A visible group comes back WHOLE**, roster included. Visibility already means the caller could add any of those members; hiding the list would be a half-truth with no boundary behind it.

`synod.list-all` is the mirror of `role.list-all` and exists for the same reader: the auditor who must see every group while holding nothing any of them bundle. Without it, an auditor granted the full **role** catalog would still be blind to the groups those roles are bundled into. It is a breadth modifier on `synod.list`, mounted on no endpoint, and bare by construction — a scoped `synod.list-all on coven=X` yields the ordinary filtered view, because the scope grammar has no `synod=` dimension for it to select with.

Filtering lives in `rbac.Service`, not in transport, so REST and MCP answer the same caller identically.

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

### Role (8) — [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)

RBAC management (roles, permissions, membership) via OpenAPI/MCP - RBAC-storage in Postgres (`rbac_roles` / `rbac_role_permissions` / `rbac_role_operators`, § Storage).

| Permission | Semantics |
|---|---|
| `role.create` | Creating a role (`rbac_roles` + its permissions in `rbac_role_permissions`). You cannot enable permission outside the caller set (§ least-privilege invariant). On its own it admits a **derived** role; a role with no parent additionally needs `role.create-root` (§ Root roles). |
| `role.create-root` | Creating a role with **no parent** that grants something — privilege that tracks nothing (§ Root roles). Mounted on **no endpoint of its own**: `role.create` still gates `POST /v1/roles`, this decides whether the result may be parentless. Also required to CLEAR `parent_role` or to grow an already-plain role via `PATCH`. NoSelector. |
| `role.delete` | Removing a role (permissions + membership cascade). **Forbidden** over `builtin=true` (`cluster-admin`), when the self-lockout invariant is violated (§ Built-in roles), and when the caller could not grant what the role grants (§ Who may administer a role). |
| `role.list` | Listing the roles with their permissions and membership — **the roles the caller could grant, not the whole catalog** (§ Catalog visibility). |
| `role.list-all` | Breadth modifier on `role.list`: see the **whole** catalog, including roles the caller holds nothing of (auditor / security review / first-line support). Mounted on **no endpoint of its own** — `role.list` still gates `GET /v1/roles`, this decides how much comes back, so an auditor needs **both**. Granting it alone opens nothing. NoSelector: scoping it is meaningless (no `role=` dimension in the grammar), and only an unrestricted holder gets the full catalog. |
| `role.update` | Changing role permissions. **Forbidden** over `builtin=true`; you cannot remove `*` from a role that holds the only effective `*` (self-lockout); you cannot add permission outside the caller set (least-privilege); and you cannot touch a role whose rights you could not grant at all (§ Who may administer a role). |
| `role.grant-operator` | Binding `(role, aid)` - adding a membership string to `rbac_role_operators`. You cannot grant a role with permission outside the caller set (§ Least-privilege invariant). |
| `role.revoke-operator` | Removing the membership line. **Disabled** if it removes the last active AID with effective `*` (self-lockout). |

### Synod (9) — [ADR-049](../adr/0049-synod.md)

Managing **Synod groups** (groups of archons, banding roles - the intermediate level of the model **Archon → Synod → Roles**, § Managing groups of archons). Selector - **NoSelector** (group management - cluster-level operation without scope by coven/host, like `role.*` / `operator.*`; group-scope ADR-049 does NOT enter). Those who mutate write audit, read-only `synod.list` - no.

| Permission | Semantics | Audit-event |
|---|---|---|
| `synod.create` | Creating a Synod group (`POST /v1/synods`). An empty rights group does not issue - least-privilege/self-lockout is not applicable to create (roles are added later via `synod.grant-role`). | `synod.created` |
| `synod.update` | Edit **ONLY `description`** group (`PATCH /v1/synods/{name}`, ADR-049 amend). `name` (PK) **immutable** - rename is deliberately not supported (would violate the invariant of immutable identifiers; symmetry with `rbac_roles.name`). **builtin-border NOT applied** - builtin-group is edited (`description` - cosmetics for UI/audit, not behavior). **Without subset-check and self-lockout** (`description` does not grant or take away rights - both invariants are not applicable); The enforcer's snapshot is not invalidated (`description` is not included in the matching). | `synod.updated` |
| `synod.delete` | Deleting a group (cascade membership + bundle, `DELETE /v1/synods/{name}`). **Forbidden** over `builtin=true` → `409 synod-builtin` (builtin is more important than lockout, checked first); prohibited if the disappearance of the group would leave the cluster without an effective `*` admin → `409 would-lock-out-cluster` (self-lockout). | `synod.deleted` |
| `synod.list` | Enumeration of groups with expanded roles (bundle) and AID members (`GET /v1/synods`) — **the groups the caller could add someone to, not the whole catalog** (§ Synod catalog visibility). | — (read-only) |
| `synod.list-all` | See the **whole** group catalog, not only the groups the caller covers (§ Synod catalog visibility). The mirror of `role.list-all`: a breadth modifier on `synod.list`, mounted on **no endpoint of its own**, NoSelector. Granting it alone opens nothing. | — (read-only) |
| `synod.add-operator` | Adding an archon to the group (`POST /v1/synods/{name}/operators`). Idempotent. **Under the least-privilege subset:** a member receives the entire bundle of group roles - the caller must hold all effective rights of this bundle, otherwise `403 forbidden` (§ Managing archon groups). | `synod.operator-added` |
| `synod.remove-operator` | Removing an archon from the group (`DELETE /v1/synods/{name}/operators/{aid}`). **Under self-lockout:** removal takes away the group roles from the archon (including `*`-giver) - prohibited if it orphans the last `*`-administrator → `409 would-lock-out-cluster`. | `synod.operator-removed` |
| `synod.grant-role` | Adding a role to the bundle group (`POST /v1/synods/{name}/roles`). Idempotent. **Under least-privilege subset:** the role is issued to all members of the group - the caller must hold all effective rights of the role, otherwise `403 forbidden`. | `synod.role-granted` |
| `synod.revoke-role` | Removing a role from the bundle group (`DELETE /v1/synods/{name}/roles/{role_name}`). **Under self-lockout:** removal takes away the rights of the role from all members - prohibited if this is the last `*`-giving role of the group and someone held `*` only through it → `409 would-lock-out-cluster`. | `synod.role-revoked` |

### Incarnation (16, one of them is deprecated-alias) - [ADR-009](../adr/0009-scenario-dsl.md) / [scenario/](../scenario/README.md) / [ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile) / [ADR-060](../adr/0060-traits.md)

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
| `incarnation.bind-member` | Bind already-onboarded, **connected** Souls to the incarnation's roster (`POST /v1/incarnations/{name}/members`, MCP `keeper.incarnation.bind-member`; [ADR-008 amendment 2026-07-28](../adr/0008-coven-stable-tags.md), NIM-209). Selectors - `coven=`/`service=`/`incarnation=` by path-`name`, as other incarnation mutations. **The route gate is only HALF the authorization** - see § Incarnation membership below. Idempotent (re-bind → `already_member`). Audit `incarnation.member_bound`. |
| `incarnation.unbind-member` | Remove a host from the incarnation's roster (`DELETE /v1/incarnations/{name}/members/{sid}`, MCP `keeper.incarnation.unbind-member`). Split from `bind-member` (the `choir.add-voice`/`choir.remove-voice` pattern) because it is the destructive half: the host stops being a target of every FUTURE run. Same selectors and the same second gate as `bind-member`. Idempotent (non-member → `removed:false`). Audit `incarnation.member_unbound`. **Reading the roster** (`GET .../members`, MCP `keeper.incarnation.members`) has no right of its own - it rides on `incarnation.get` and is narrowed to the caller's soul visibility. |

#### Incarnation membership — two gates, not one

`incarnation.bind-member` / `incarnation.unbind-member` are authorized on **two axes at once** ([ADR-008 amendment 2026-07-28](../adr/0008-coven-stable-tags.md), NIM-209). Both must pass; either one alone is a hole.

| Gate | Where | Question | On refusal |
|---|---|---|---|
| (a) the incarnation | `RequirePermissionMulti` middleware (REST) / explicit body-scoped OR-Check (MCP) | May this Archon change the membership of **this incarnation**? Selector `incarnation=`/`coven=`/`service=` by path-`name`. | `403 forbidden` |
| (b) each host | in-handler, `soulpurview.InScope` over the `soul.list` purview | Is **this SID** inside the Archon's soul visibility? | `403 forbidden` naming the SIDs |

**Why (b) exists.** A scope predicate on `incarnation=X` is satisfied without ever examining the host — the souls table has no incarnation column, and the condition never looks at one. So a holder of `incarnation.bind-member on incarnation=X` would, with gate (a) alone, be able to pull **any** host in the fleet into X — and then reach it with `incarnation.run on incarnation=X`, which they already hold. Membership is an escalation edge, so the host side of it is checked on the host axis. Same lesson as [NIM-198 / NIM-202](../adr/0047-purview.md): a basis wider than the rights it derives from is an escalation.

**All-or-nothing.** One out-of-scope SID rejects the whole call; nothing is written. A partial bind would report success while leaving the roster short a host, and the run would fail later somewhere unrelated.

**Gate (b) judges effective covens, not the raw column.** Visibility resolves over the [ADR-080](../adr/0080-label-inheritance-union.md) union — the host's own `souls.coven` plus the tags (and names) of the incarnations it already belongs to — the same basis as the souls read, the roster resolver and the SQL pushdown. Judging `souls.coven` alone would refuse binds the Archon can plainly make, and this gate would be the one place where a `where:` and a scope check disagree about a host. Note the ordering: the union is taken **before** the bind, so the tags the target incarnation would confer are not yet in it. Otherwise a bind would mint the very label that authorizes it — the caller must already reach the host by some other route (its own coven, a `host=` selector, or an incarnation it is already in).

**No new selector keys.** Membership reuses `{service, coven, incarnation, host}` — there is no `member=` dimension ([ADR-047 §S4](../adr/0047-purview.md)).

**Refusal order does not leak state.** Screening reports unknown SIDs → out-of-scope → not-connected, in that order: an Archon who may not see a host is told "forbidden", never "that host is disconnected".

**The keeper-internal bind is NOT gated by this.** `core.soul.registered` inside a scenario run binds hosts it has itself just created (still `pending`) and acts as the keeper, not as an operator — the connected-only rule and gate (b) apply to the operator path only.

### Choir (5) — [ADR-044](../adr/0044-choir.md)

CRUD named host topology inside incarnation (Choir / Voice, tables `incarnation_choirs` / `incarnation_choir_voices`). **REST-only** (`/v1/incarnations/{name}/choirs*`, no MCP tools; bodies and semantics - [operator-api/choirs.md](operator-api/choirs.md)); routes are connected only when the ChoirDB pool is configured. Choir belongs to the incarnation, so the selector is the same as `incarnation.*`: `incarnation=` / `service=` / `coven=` (landing on path-`{name}`); bare - unrestricted. Those who mutate write audit, read-only `choir.list` - no.

| Permission | Semantics | Audit-event |
|---|---|---|
| `choir.create` | Creating a Choir within an incarnation (`POST /v1/incarnations/{name}/choirs`). | `choir.created` |
| `choir.delete` | Delete Choir (`DELETE /v1/incarnations/{name}/choirs/{choir}`). | `choir.deleted` |
| `choir.list` | Enumeration of incarnation Choirs (`GET …/choirs`) and Voice members of one Choir (`GET …/choirs/{choir}/voices`) - one-permission-on-read. | — (read-only) |
| `choir.add-voice` | Adding a Voice (host) to Choir (`POST …/choirs/{choir}/voices`). Action - kebab, grammar `<resource>.<action>` (pattern `soul.ssh-target-update` / `sigil.key-introduce`). | `choir.voice_added` |
| `choir.remove-voice` | Removing Voice from Choir (`DELETE …/choirs/{choir}/voices/{sid}`). | `choir.voice_removed` |

### Soul (7) - host registry

| Permission | Semantics |
|---|---|
| `soul.create` | Registering a new host in the registry `souls` (`status: pending`) and issuing the first bootstrap token ([onboarding.md](../soul/onboarding.md)). `transport` (`agent`/`ssh`) required; for `ssh` bootstrap token is not issued. |
| `soul.issue-token` | Re-issue of bootstrap token for existing Soul with `transport: agent` (loss of token, planned re-issue). If the token is already active, `409` is rejected if `force=true` is not specified. For `transport: ssh` - `422` (ssh host does not have a bootstrap phase). **Mutation** - scope-aware gate `RequirePermission`, selector `host=<sid>` from path (the context is known before the handler, as opposed to read-visibility). |
| `soul.list` | Listing Souls in the registry (with filters by coven/status/transport). Also covers single-soul read `GET /v1/souls/{sid}`, soulprint-read `GET /v1/souls/{sid}/soulprint` and per-host history `GET /v1/souls/{sid}/history` - the one-permission-on-read pattern, like `service.list` / `omen.list` / `vigil.list` / `decree.list`. **Read visibility is authorized in two layers** (ADR-047 section g/G1, section Two-layer authorization of read endpoints): gate `RequireAction` (existence - does `soul.list` hold in principle) + narrowing by scope in handler (`soulpurview` - AST→SQL pushdown over coven / host-glob (`LIKE`) / `InScope`). **The selector is NOT used at the gate stage of read routes** - scope is resolved from database lines that do not yet exist; The selector form (`coven=`/`host=`) for the role still narrows the visibility in the handler, but the gate ignores it. host selector (`host=<sid>`) is a form for **mutations** souls (see `soul.issue-token` / `soul.ssh-target-update`), not for read. |
| `soul.coven-assign` | Bulk assignment/removal of one Coven tag to hosts using a selector (`POST /v1/souls/coven`). Coven - cold PG tag: pure UPDATE `souls`, no Redis. **Two-layer authorization:** middleware checks the right in general + the assigned label (gate b - the coven-scoped operator passes only for the label in its `coven=`-scope); The service layer intersects target hosts with the scope of the operator (gate a - `souls.coven && scope`). Without both gates bulk = privilege-escalation (an operator with `coven=dev` would assign `prod` to all Souls). |
| `soul.traits-assign` | Bulk assignment of operator-set key-value trait tags **to hosts** by selector (`POST /v1/souls/traits`, jsonb column `souls.traits`). Modes `merge` / `replace` / `remove`. **First-class since [ADR-080](../adr/0080-label-inheritance-union.md)** (NIM-121, deprecation lifted): a host label is stored on the host and nothing projects over it; the per-incarnation counterpart is `incarnation.traits-set`, and a host's effective traits are the union of the two. Mirrors the coven-write-path (`soul.coven-assign` / `soul.coven-changed` / `POST /v1/souls/coven`) in **both** gates: existence-gate `RequireAction(soul, traits-assign)` + service-layer intersection of target hosts with the operator's coven-scope (gate a - `souls.coven && scope`, the same `BulkScope`), **plus gate (b)** on merge/replace - every pair attached must lie inside the operator's own trait-scope ([Enforcer.TraitScope](../../keeper/internal/rbac/enforcer.go), pure-trait disjuncts only). Gate (b) is required because a host-attached pair GRANTS visibility permanently and `trait.<key>=v` is a scope dimension; `remove` is ungated on the pair, exactly as with coven. More details - § Two-layer authorization of read endpoints. |
| `soul.ssh-target-update` | Update per-host SSH push-flow details (`PUT /v1/souls/{sid}/ssh-target`, ADR-032 amendment 2026-05-26, S7-1). Body: `{ssh_port, ssh_user, soul_path}`. Action - hyphenated (`ssh-target-update`), because permission grammar - exactly `<resource>.<action>` (pattern `sigil.key-introduce`); MCP-tool - 3-segment `keeper.soul.ssh-target.update`. **Mutation** - scope-aware gate `RequirePermission`, selector `host=<sid>` from path. Audit `soul.ssh-target.updated`. |
| `soul.console` | Opening an interactive console (PTY) on a host over the WebSocket `/v1/console` ([ADR-0074](../adr/0074-interactive-console-pty.md), [console.md](console.md)), running a one-shot command line on a host through the MCP tool `keeper.soul.run-command` ([ADR-0074 amendment](../adr/0074-interactive-console-pty.md), NIM-147), **and** reading a recorded session back over `GET /v1/console/recordings…` (NIM-148) - a recording is the session's content moved in time, so no lighter right is minted for it; withhold playback by narrowing the SCOPE, not by a weaker right. Selectors: `host=<sid>` / `coven=<label>`; bare - unrestricted. **The most privileged right in the catalog on the execution axis** - see § Console below. Audit `console.opened` / `console.closed` / `console.command` / `console.recording-read`. |

#### Console: `soul.console` is strictly stronger than `errand.run`

An [Errand](../adr/0033-errand.md) is one named module call with declared params, capped output and a fixed end - it can be reasoned about before it runs. A console is an interactive shell under a real pty, running as the Soul daemon's user (typically **root**), whose commands are not knowable in advance and therefore **cannot be checked against a module allow-list**. So it gets a right of its own, and the two rights are **independent**: `errand.run` never implies `soul.console`, and `soul.console` never implies `errand.run`. A role that should not hand out shells simply does not carry it.

**Independent, and now also conjoined on one path.** Since NIM-197 the two rights are *both* required to reach `core.cmd.shell` / `core.exec.run` through an Errand (§ Errand). That is a conjunction on one code path, not an implication in either direction: `soul.console` alone still does not open the Errand path, and `keeper.soul.run-command` still needs no `errand.run`. The practical rule is unchanged and now enforced rather than advised — **a role that must not hand out shells carries neither right**.

**`soul.*` carries it — a widening at upgrade, not a defect.** The catalog is closed and a wildcard in the action position expands to every known action of the resource (§ Selector Grammar), so a role written before `soul.console` existed grants it afterwards: the live tty, the MCP `keeper.soul.run-command`, and playback of anyone's recorded session. Same mechanism as `incarnation.*` when `incarnation.view-secrets` landed ([ADR-0070](../adr/0070-secret-reveal-path.md)). A role that must not hand out shells therefore **enumerates actions** instead of the wildcard; as of this release the full expansion of `soul.*` is

```
soul.list  soul.create  soul.issue-token  soul.coven-assign
soul.traits-assign  soul.ssh-target-update  soul.console
```

pinned against the catalog by `TestCatalog_WildcardRostersPinnedForReleaseNotes`, which fails when an added action makes this list stale. Note what the wildcard does *not* cover: `role.create-root`, `role.list-all` and `synod.list-all` are checked bare, so only an **unrestricted** `role.*` / `synod.*` reaches them. Withholding console access from a scoped role is done by narrowing the scope; there is no weaker right to grant instead (§ the recording rows above).

**Keeper cannot switch the console plane off.** The `console:` block in `keeper.yml` is operator envelope only — sessions per Archon, per instance, idle timeout, recording cap — and its absence means built-in defaults, not "off"; the route is mounted by any real `keeper run`. The host has the last word: `console: {enabled: false}` in `soul.yml` makes the Soul refuse every open with a terminal `ConsoleExit`, whatever this catalog permits. So a host that must never be shelled is protected by that flag first and by a withheld `soul.console` second.

The right is checked **twice**, because the target host is not in the URL ([ADR-0074(c)](../adr/0074-interactive-console-pty.md)):

| Gate | Where | Question | Refusal |
|---|---|---|---|
| `soul.console`, NoSelector | chi middleware, before the WebSocket upgrade | May this Archon open consoles at all? | **HTTP 403**, before any socket exists. |
| `soul.console` + `host=<sid>` | in-handler, per `open` frame | May they open one on **this** host? | A session-scoped `error{code: "forbidden"}` frame; the socket and its other panes stay live. |
| `soul.console` + `host=<sid>` | in-tool, MCP `keeper.soul.run-command` | May they run a command on **this** host? | MCP `forbidden`. |
| `soul.console`, NoSelector | chi middleware, on `/v1/console/recordings…` | May this Archon reach recordings at all? | **HTTP 403**, before the store is touched. |
| `soul.console` purview | in-handler, per row / per recording | May they read a session recorded on **this** host? | Rows outside the purview are absent from the list; a single read is **404**, identical to an unknown id. |

The two recording rows are the playback surface ([ADR-0074 amendment](../adr/0074-interactive-console-pty.md), NIM-148) and repeat the WebSocket's split for the same cause - a listing names no host, so the route gate can only ask the existence question. Note the refusal shape: out-of-scope is 404 rather than 403, because a 403 would confirm the recording exists and turn the route into an oracle for which hosts have been consoled into. The per-host boundary is resolved through `ResolvePurview` (pushed into SQL for the list), never through `Check`: the boundary has to be applied per ROW, and `Check` answers about one context at a time. Until NIM-219 there was a second, sharper reason - a role's `default_scope` sat outside what `Check` matched, so a bare `soul.console` passed it for any host; that hole is closed ([ADR-047 amendment 2026-07-28](../adr/0047-purview.md)) and `Check` is now safe to use wherever the host IS known, as the two per-host rows above do.

The MCP row is the **non-interactive console** ([ADR-0074 amendment](../adr/0074-interactive-console-pty.md), NIM-147): dropping the tty removes the echo, not the privilege, so `keeper.soul.run-command` needs `soul.console` and **not** `errand.run`. There is no two-gate split there - the SID is an argument of the call, so the one scope-aware `Check` has its context from the start and there is no upgrade to guard. The check runs **before** the errand-stack wiring guard: whether this Keeper can dispatch at all is not something a caller without the right gets to learn.

The split follows the existing boundary (§ Two-layer authorization of read endpoints): the socket carries no host, so a scope-aware gate at the upgrade would falsely deny every scoped role; the SID arrives in the `open` frame, which is exactly the "context known before the action" case that a scope-aware `Check` wants. One socket multiplexes many panes, so a per-pane denial must not tear down the panes that are permitted.

**No new selector keys.** Per-host consoles reuse `host=<sid>`, per-environment consoles reuse `coven=<label>`; the RBAC selector keys stay `{service, coven, incarnation, host}` and any narrowing beyond them goes through the existing Purview dimensions ([ADR-047 §S4](../adr/0047-purview.md)).

The `coven=` selector (§ Selector Grammar) applies to mutating (`soul.issue-token` / `soul.coven-assign` / `soul.ssh-target-update`) and read- (`soul.list`) permissions: restricts the action to hosts with the specified Coven labels. For **scope-aware mutations** (`soul.issue-token` / `soul.coven-assign` / `soul.ssh-target-update`) it narrows the scope at the gate stage (scope-aware `Check`, context from path/body); `soul.coven-assign` additionally specifies the valid set of assigned labels and a subset of target hosts (scope-intersection). For **read** (`soul.list` and the get/soulprint/history it covers), coven-/host-glob-narrowing makes the handler via `soulpurview` (gate is only the existence of `RequireAction`), see § Two-layer authorization of read endpoints.

`soul.traits-assign` carries **two** gates, like coven-assign ([ADR-080](../adr/0080-label-inheritance-union.md)): the permission itself is authorized by the existence-gate (see the directory line above) and the target hosts are narrowed by the operator's coven-scope (gate a, a single `BulkScope`); on `merge`/`replace` every pair being attached must additionally lie inside the operator's own trait-scope (gate b). The earlier reasoning for having no key gate - "a trait key is not a scope dimension" - stopped holding when NIM-128 made `trait.<key>=v` a scope dimension and ADR-080 made a host-attached pair permanent: without gate (b) any holder of this permission could hand a host to a foreign role by stamping its pair. AND-narrowing on several trait pairs remains a follow-up.

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

An **interactive console is not an Errand** and is not covered by any of these three rights: it is gated by `soul.console`, which is strictly stronger (§ Console: `soul.console` is strictly stronger than `errand.run`). The same holds for the **non-interactive** console — the MCP tool `keeper.soul.run-command` runs an arbitrary command line and is gated by `soul.console` too, even though it rides the Errand transport.

**`errand.run` is not enough for a verb-shell module** ([ADR-0074 amendment 2026-07-28](../adr/0074-interactive-console-pty.md), NIM-197). `core.cmd.shell` and `core.exec.run` are on the Errand runner's allow-list, and their declared input IS an arbitrary command line — so the allow-list bounds the *module*, not what it carries. Reaching either through an Errand therefore requires **`soul.console` in addition to `errand.run`**, with the same selector the entry point already resolved. `errand.run` stays necessary; it stopped being sufficient. An `ErrandReadSafe` module is unaffected — an ordinary Errand never demands a console right.

| Entry point | `errand.run` selector | added `soul.console` check |
|---|---|---|
| `POST /v1/souls/{sid}/exec` | `host=<sid>` | `host=<sid>` |
| MCP `keeper.soul.errand.run` | `host=<sid>` | `host=<sid>` |
| `POST /v1/voyages`, `kind=command` | Purview over the resolved target | `host=<sid>` on **every** resolved host (all-or-nothing — the batch is not trimmed) |
| `POST /v1/voyages/preview`, `kind=command` | same | same — preview shares the create path's resolver and refuses in the same places, so a preview never promises a run the create would refuse |
| `POST`/`PATCH /v1/cadences`, `kind=command` | bare | bare (a recipe's target is declarative; there is no host yet) |
| Cadence spawn (background) | — | `host=<sid>` on every resolved host, against the recipe's `created_by_aid` |

`DELETE /v1/voyages/{id}` is deliberately **not** gated: cancelling is de-escalation, and the emergency brake must not need a stronger right than the accelerator.

**Deprecation window.** The gate ships in two stages, set by `console.errand_shell_gate` in keeper.yml (see [config.md](config.md)): `warn` (default for one minor) lets the call through and records the would-be denial; `enforce` denies. Before flipping it, read the inventory:

- `keeper_rbac_shell_errand_legacy_roles` — roles granting `errand.run` without `soul.console` at all, i.e. the grants `enforce` will break. Recomputed on every RBAC snapshot rebuild; the names appear in a WARN log line whenever the set changes. Scope-agnostic, so it is a **floor**: a role holding both rights but with a narrower console scope is not counted.
- `keeper_rbac_shell_errand_gate_total{surface,result}` — live decisions. `result="would_deny"` is the exact answer, because it is the real check on real targets. `surface` names which entry point (`rest` / `mcp` / `voyage` / `cadence` / `cadence_spawn`).

**Cadence breaks on a timer, so it is made loud.** A recipe written under the old rule keeps firing after `enforce` lands, and refusing it in the background has no operator to answer. The spawn therefore skips, still advances `next_run_at` (so the series does not wedge) and writes **`cadence.skipped_forbidden`** `{cadence_id, scheduled_for, reason, module}` — a distinct event from `cadence.skipped_overlap`, which is ordinary scheduling.

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

### Settings — [ADR-0073](../adr/0073-keeper-runtime-config-pg.md)

The SettingsStore overlay of reload-able Keeper parameters — the `cfg_*` rows of `keeper_settings` merged onto each instance's `keeper.yml` and propagated cluster-wide without a restart. A family of its own rather than `service.*`, even though both live in `keeper_settings`: editing a cluster-wide runtime tunable is a different privilege from registering a Service, so an operator can be granted it without any other cluster-admin power. The selector is **NoSelector** (cluster-level, like `provisioning.*` / `role.*`). Mutations are audited (`setting.updated` / `setting.deleted`), read is not. The admitted keys and their ranges are in [config.md → SettingsStore](config.md#settingsstore--the-admitted-keys-and-their-operator-surface).

| Permission | Semantics |
|---|---|
| `setting.read` | Read the settings catalog (`GET /v1/settings`): per key its type, range bounds, default, the **effective** value on the answering instance and its `source ∈ {default, file, pg}`. |
| `setting.update` | Override a key cluster-wide (`PUT /v1/settings/{key}`). Unknown key → 404 (admission is enumerated); unparsable or out-of-range value → 422 with `keeper_settings` unchanged. Audited (`setting.updated`). |
| `setting.delete` | Drop an override (`DELETE /v1/settings/{key}`), so the `keeper.yml` value or the built-in default is back in effect. No override → 404. Audited (`setting.deleted`). |

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

**Invariant self-lockout ([ADR-028(f)](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).** You cannot leave a cluster without at least one active Archon (`revoked_at IS NULL`) with an effective **bare** `*`-permission (a scoped `* on <expr>` does **not** count as a cluster-admin for this check — NIM-128; `HasWildcard`/`ClusterAdmins` are bare-only). Checked on all paths that could break this:

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
2. **Scope-narrowing - handler.** After the row fetch, handler reduces output to the scope-boundary of the operator through the per-resource resolver: `keeper/internal/soulpurview` (a single scope resolver translating the boolean scope AST into a souls-query `WHERE` — coven and `host matches` glob→`LIKE` pushdown) for a list and `InScope` for a single object. Outside scope: list is empty, single-get/soulprint/history is `404`/`403`. (The former `regex`/`soulprint`/`state` visibility dimensions are removed with NIM-128 — [ADR-047 amendment](../adr/0047-purview.md).)

**Why two layers and not a scope-aware gate.** Scope-aware `Check(aid, resource, action, context)` for **scoped**-permission with an empty context gives false `deny`: the selector-key (`coven`/`host`/…) is missing in the `nil` context of the read request, and enforcer for a missing key, deny is returned (the same mechanics as in incarnation gates before ADR-008 amendment a). This broke the scoped operator's access to **its own** list - `RequirePermission(...Selector)` cut it off even **before** the handler, although by its scope it is required to see hosts. Root: read endpoint **does not carry a scope context at the gate stage** - scope (`coven` host, `state` incarnation) is resolved from database lines that do not yet exist at the time of the middleware check. `HoldsAction` removes the context from the question (existence, not applicability), and the real narrowing is deferred to the handler, where the lines are already fetched.

**Most of the mutations remain on scope-aware `Check`.** issue-token / ssh-target-update / coven-assign by souls carry the scope-context from path/body (`host=<sid>` from path, `coven=<label>` from body), so they continue to gate scope-aware `RequirePermission` / `RequirePermissionMulti` - for them the context is known before the handler and there is no false deny. The boundary for them is: **read** (context from database rows, not yet) → `RequireAction`; **scope-aware mutation** (context from path/body) → `RequirePermission`.

**This boundary became load-bearing with NIM-219.** Until then `Check` ignored a role's `default_scope` (§ Semantics of conflict), so a mis-gated route - a scoped-capable action gated by `Check` with **no** context - looked like it worked: the bare permission passed regardless. Now that the scope is applied, such a route returns `403` to exactly the scoped roles it should have been narrowing all along. The remedy is the same as it was for souls-read (`RequireAction` + handler narrowing), **never** a return to a role-blind gate: the alternative is a gate that permits more than the read path shows. Two shapes are legitimately unaffected - a bare `*` cluster-admin (never bounded, [ADR-047(b)](../adr/0047-purview.md) exception #1) and a role that sets no `default_scope` at all (bare permissions stay unrestricted, exception #2), which is most deployed roles.

**A cluster-level right does not belong on a scoped role.** `default_scope` is inherited by **every** bare permission of the role, `operator.create` included - and no request carries a coven for it, so under `default_scope: coven=dba` it is denied everywhere. This is default-deny working as specified ([ADR-047(b)](../adr/0047-purview.md)) and matches what a scoped `* on X` has done since NIM-128; a role that needs both a scope and a context-less right splits into two roles, or writes the context-less right with its own overriding `on <expr>`.

**The exception is `soul.traits-assign` (`POST /v1/souls/traits`).** At the ROUTE it is gated by the **existence-gate `RequireAction`**, not by the selector `RequirePermission`, although the context is in the body: the selector gate would cut off a coven-scoped operator here (its `coven=dev` permission cannot match a request that carries no coven context at gate time) even though it may legitimately change traits on its dev hosts. Least-privilege is enforced one layer down instead, and by **two** gates ([ADR-080](../adr/0080-label-inheritance-union.md)): gate (a) at the service layer (`soul.BulkAssignTraits`/`BulkReplaceTraits`) narrows target hosts to the operator's coven-scope (the same `BulkScope` as `coven-assign`), and gate (b) in the handler rejects any pair outside the operator's own trait-scope before touching the DB. So traits-write is no weaker than coven-write on either axis - the host it may touch, and the label it may attach.

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

- `prod-operator` can launch and demolish incarnations whose declared `covens` contains `prod` - env-scope by Coven label, regardless of the service. he will not touch incarnation with `covens=[dev]`. (To scope by the incarnation's own name instead, use `incarnation=<name>` — the name is no longer a coven, [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation).)
- `redis-fleet` can perform any incarnation operations on incarnations of the `redis` (`incarnation.service == "redis"`) service, regardless of their env tags.
- When create, the `coven=prod` rule will not allow creating an incarnation with a tag outside `prod`: scope resolves from declared `covens` of the request body (real stable tags only; see § Selector grammar).

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
