# ADR-029. Service registry → Postgres

> Closes the remainder of [ADR-028(h)](0028-rbac-storage.md#adr-028-rbac-storage--postgres) (config→DB for the other managed entities). Same pattern as RBAC (ADR-028): the managed catalog moves out of the static `keeper.yml` into Postgres with a runtime snapshot + pub/sub invalidation. Unlike ADR-028 (Phase 0 — documentation), here the **implementation is done** in slices S1–S4.

- **Context.** The Service registry (`keeper.yml → services[]`) and the associated well-known scalars (`default_destiny_source`, `default_module_source`) lived in the static config. This produced the same defects as YAML-RBAC before ADR-028:
  - **Per-host divergence.** The stateless Keeper cluster ([ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) — each instance knew only its own `keeper.yml`; adding a Service required editing the file + a reload on every node.
  - **No API management.** Services (a managed runtime entity, symmetric to Incarnation/Provider/Profile — all in the DB per [ADR-005](0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)) could not be created via OpenAPI/MCP, only by editing YAML.
  - **`default_module_source` — a dead field:** declared in the config schema but with no consumer (module resolution through it is not implemented).

- **Decision.**

  **(a) Storage — table `service_registry` + key-value `keeper_settings`.**

  | Table | Role |
  |---|---|
  | **`service_registry`** | Catalog of Services. `name` PK (kebab-case, matches `service.yml → name`), `git`, `ref` ([ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте): version = git ref), `refresh` (opt. duration for auto-fetch, NULL-able), `created_at` / `updated_at`, `created_by_aid` / `updated_by_aid` FK→`operators(aid)` NULL-able (seed / no initiating Archon). |
  | **`keeper_settings`** | Well-known cluster scalars: `key` PK → `value`, `updated_by_aid` FK→`operators(aid)` NULL-able, `updated_at`. MVP key — `default_destiny_source`. Extended with new keys without a table migration. |

  **(b) Source of truth — the DB, runtime — snapshot + invalidation.** Consumers (scenario-runner: `ServiceRegistry.Resolve`, `DestinySource`) do not read the DB per-call, but an in-memory snapshot `serviceregistry.Holder` (the `rbac.Holder` pattern from [ADR-028(d)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)): an atomic snapshot `{services map[name]ServiceEntry, default_destiny_source}`, at startup — a synchronous build from the DB (fatal on error), refresh — TTL-poll (background goroutine; a re-read error leaves the previous snapshot in place + warn) + Redis pub/sub invalidation via the topic **`service:invalidate`** (envelope `{origin_kid, at}`, self-filter by KID — the `applybus`/`rbac:invalidate` pattern). The getters `Resolve(name) (ServiceEntry, bool)` / `DefaultDestinySource() string` are **synchronous** (lock-free atomic.Pointer.Load, no ctx/error): the absence of a Service is a normal `false`, not a failure. This keeps the consumers' contract unchanged when the source is swapped from cfg→DB.

  **(c) Link to the Destiny catalog — lazy resolve.** Resolving `apply: { destiny: <name> }` ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) does not change semantically: the hybrid "per-entry `destiny[].git` override → otherwise `default_destiny_source` + `{name}`". What changes is the source of the scalar: `default_destiny_source` is read **lazily** from `Holder.DefaultDestinySource()` on each resolve (rather than copied into the `DestinySource` constructor) — otherwise a hot-reload of the scalar via `service:invalidate` would not reach the resolve.

  **(d) API/MCP — `service.*` operations.** The registry is managed via OpenAPI/MCP (like Archons in [ADR-014(d)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) and roles in [ADR-028(e)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)): `service.create` / `service.update` / `service.delete` / `service.list`. Names fixed via propose-and-wait; the permission catalog — [naming-rules.md](../naming-rules.md) / [rbac.md](../keeper/rbac.md).

  **(e) Config hard-cut.** The keys `services:` / `default_destiny_source:` / `default_module_source:` are **removed** from `keeper.yml` — the fields `KeeperConfig.Services` / `DefaultDestinySource` / `DefaultModuleSource` and the type `config.ServiceRegistryEntry` go away; the keys start being rejected by the parser as `unknown_key` (reflect-walker `shared/config/walk.go`). There are no legacy installations (no production) — no YAML→DB migration layer is required (as in [ADR-028(g)](0028-rbac-storage.md#adr-028-rbac-storage--postgres)). `soul.yml` is not affected.

  **(f) `default_module_source` is not carried over.** A dead field with no consumer — abolished without a replacement (does not appear in `keeper_settings` nor in the API). If module resolution via a template is ever needed — it is introduced by a separate decision as a new well-known key `keeper_settings` (an extension without migration).

  **(g) Well-known `keeper_settings` keys (a growing list).** The `keeper_settings` table is an untyped key-value store (see (a)/(b)); new well-known keys are added **without a table migration** (only a new row + a consumer). Fixed keys:

  | Key | Value | Semantics |
  |---|---|---|
  | **`default_destiny_source`** | git-source string | MVP scalar: the default Destiny-catalog source for resolving `apply: destiny` (ADR-009, (c)). absent → the consumer uses per-entry `destiny[].git`. |
  | **`provisioning_allowed_methods`** | CSV over the domain `{user, ldap, oidc}` | Policy of the allowed methods for **CREATING** an operator ([ADR-058(i)](0058-operator-auth-ldap-oidc.md#adr-058-федеративная-аутентификация-операторов-archon--ldap--oauth2oidc)). **absent** → all methods allowed (back-compat). **Set-but-empty** → config-error on snapshot load (anti-lockout: you cannot forbid all creation methods). `bootstrap`/`system` are NOT in the domain (never gated). Gates only the operator-creation branch; runtime management — `GET`/`PUT /v1/provisioning-policy`. |

  The `serviceregistry.Holder` snapshot parses both keys on build (like `DefaultDestinySource()`); the getters are synchronous (lock-free atomic.Pointer.Load).

- **Rationale.** Mirrors [ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres): compliance with [ADR-005](0005-storage-postgres.md#adr-005-keeper-state-storage--postgres) (managed runtime-state in Postgres) and [ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) (shared PG → the registry is identical across the whole cluster without rolling YAML out to nodes). A Service stands alongside Incarnation/Provider/Profile — all managed via the API, the source of truth is the DB, git holds only the code of the Service repo (the git↔Postgres boundary is preserved: the DB holds the coordinates `name → git@ref`, not the code).

- **ADR reconciliation.**
  - [ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — a Service's `ref` is now the column `service_registry.ref`, not the field `keeper.yml::services[].ref`; the "version = git ref" principle is unchanged.
  - [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) — `default_destiny_source` for resolving `apply:destiny` is read from `keeper_settings`, not from `keeper.yml`; the hybrid resolve rule is unchanged.
  - [ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml) — the Service registry is **excluded** from the hot-reload-config path: an update = a DB mutation + `service:invalidate`, not `SIGHUP` / config-swap (like RBAC in ADR-028(d)).
  - [ADR-028(h)](0028-rbac-storage.md#adr-028-rbac-storage--postgres) — the remaining scope (config→DB for the other managed entities) is closed by this ADR.

- **Consequences.**
  - **Two new PG tables** `service_registry` / `keeper_settings` ([keeper/storage.md](../keeper/storage.md)).
  - **`service.*` permissions** ([naming-rules.md](../naming-rules.md), [rbac.md](../keeper/rbac.md)).
  - **Hard-cut config** — removed `config.ServiceRegistryEntry` / `KeeperConfig.{Services,DefaultDestinySource,DefaultModuleSource}`; three keys → `unknown_key`. The examples [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) / [`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) lose those blocks.
  - **Consumers** in scenario (`ServiceRegistry` / `DestinySource`) read `serviceregistry.Holder`; the synchronous `Resolve` contract is unchanged.
  - **Operator API / MCP** get `service.*` endpoints ([operator-api.md](../keeper/operator-api.md), [mcp-tools.md](../keeper/mcp-tools.md)).

- **Trade-offs.**
  - **Snapshot + invalidation (best-effort + TTL-fallback)** — the same compromise as with RBAC ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)) and `Summons` ([ADR-027](0027-apply-work-queue.md#adr-027-модель-исполнения-apply--work-queue--claim-acolyte-пул-ward-claim)): loss of the pub/sub signal → a node sees a stale snapshot until the next TTL re-read (a short window). Service mutations are rare — acceptable.
  - **`keeper_settings` as an untyped key-value store** — scalars are stored as strings, without per-key columns. The cost is no SQL typing of the value; the gain is that a new well-known key requires no table migration. Accepted (symmetric to the RAW permission in ADR-028).
  - **Hard-cut without a YAML→DB migration** — there is no production, no legacy installations; a `services:`-block importer is not needed.

- **Implementation slices.** S1 — storage (migrations `service_registry` + `keeper_settings`, repository, CRUD-Service). S2 — `serviceregistry.Holder` (synchronous snapshot Resolve/DefaultDestinySource + Run/WatchInvalidations). S3 — OpenAPI/MCP `service.*`. **S4 — config hard-cut + switching consumers over to the Holder** (this slice: removing the `config` fields, moving `scenario.ServiceRegistry`/`DestinySource` onto the Holder, the daemon passes the Holder instead of cfg).
