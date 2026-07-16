# ADR-022. Audit-pipeline: storage, schema, retention

- **Context.** By the time ADR-014 / ADR-020 / ADR-021 were fixed, the system already **names** concrete audit events — `operator.created` / `operator.revoked` ([ADR-014(e)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), `policy_violation` for `side_effects` violations ([ADR-020(g)](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)), `config.reload_succeeded` / `config.reload_failed` ([ADR-021(g)](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). The name catalog is open in [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events). **Not fixed:** the overall normalization of the pipeline — where an audit event is physically stored, what schema it has, who writes it (write-path), how correlation with other subsystems lives (OTel, FK to the operator, apply-chains), how it is cleaned up by retention. Without this normalization the `shared/audit` implementation would guess behavior, and `GET /v1/audit` cannot be added to the Operator API.

  The audit trail is a compliance foundation (SOC2 / ISO 27001 — changes to identity / authn / authz must be journaled), an incident-investigation tool ("who changed role X and when?"), and a debugging channel (aggregation of `TaskEvent` / `RunResult` from a Soul into a single Keeper level). [ADR-014(e)](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) already fixed the invariant of the FK fields `created_by_aid` / `changed_by_aid` ("snapshot of who"); the audit log gives "snapshot of when + what".

- **Decision.**

  **(a) Storage — Postgres table `audit_log`.** Single source of truth — a table in the shared Postgres DB of the Keeper cluster ([ADR-005](0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)). Not an append-only file, not a separate service (Elasticsearch / Kafka), not a shared filesystem. Rationale: Keeper already depends on Postgres, we do not multiply storage; the HA cluster ([ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) is naturally shared through PG, audit is visible globally without cross-host coordination; queries — standard SQL under the Operator API. Schema ([keeper/storage.md](../keeper/storage.md) is extended):

  ```sql
  CREATE TABLE audit_log (
    audit_id        TEXT        PRIMARY KEY,                       -- ULID (sortable timestamp prefix)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    event_type      TEXT        NOT NULL,                          -- <area>.<action>
    source          TEXT        NOT NULL,                          -- enum, see (b)
    archon_aid      TEXT        REFERENCES operators(aid) ON DELETE SET NULL,  -- nullable; audit trail is preserved when the operator is deleted
    correlation_id  TEXT,                                           -- ULID, optional
    payload         JSONB       NOT NULL DEFAULT '{}'::jsonb
  );

  CREATE INDEX idx_audit_event_type   ON audit_log (event_type, created_at);
  CREATE INDEX idx_audit_aid          ON audit_log (archon_aid, created_at) WHERE archon_aid IS NOT NULL;
  CREATE INDEX idx_audit_correlation  ON audit_log (correlation_id)         WHERE correlation_id IS NOT NULL;
  ```

  `audit_id` — ULID (text), not UUID and not bigint autoincrement: the timestamp prefix gives a stable chronological order without dependence on `created_at` clock skew between Keeper instances, while the random component gives global uniqueness without coordination between instances (see (e)). All fields are snake_case (like `operators` / `souls` / `bootstrap_tokens`). `payload` stores the kind-specific payload (`changed_paths`, `validation_errors[]`, etc.); payload typing — per-event-type, normalized in `docs/naming-rules.md → Audit-events` as the catalog fills up.

  **(b) `source` — closed enum, 5 MVP values.** The source category of the event — who initiated it:

  | Value | When |
  |---|---|
  | `signal` | SIGHUP file-edit-path hot-reload ([ADR-021](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). |
  | `api` | HTTP/JSON Operator API request from an Archon. |
  | `mcp` | MCP-tool call from an Archon (LLM agent). |
  | `keeper_internal` | Keeper's own initiative (Reaper, scheduled tasks, bootstrap). |
  | `soul_grpc` | Events from a Soul via the gRPC EventStream ([ADR-012](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)), forwarded by Keeper. |

  Closed enum: extension — propose-and-wait in [`docs/naming-rules.md`](../naming-rules.md). Not a freeform string — this is needed for indexing (filter `WHERE source = 'api'`) and for the typed RBAC filter of `GET /v1/audit` (a separate task).

  **(c) `correlation_id` — ULID.** The same id links an audit event with downstream events (other audit events, OTel spans, `apply_id` / `RunResult`).

  ULID (text) is chosen, not UUID and not an OTel trace-id:

  - sortable timestamp prefix — filter by time without a `created_at` join.
  - compact (26 chars).
  - already used in the project (`apply_id` in `RunResult` / `TaskEvent`, `audit_id`).
  - the OTel trace-id is a different concept (a span tree around a single RPC call), not business-level correlation; the audit pipeline must survive a reset of the OTel chain.

  When it is set:
  - **API/MCP-driven action** → the HTTP middleware / MCP handler generates a `correlation_id` on call entry, propagates it into the downstream context, all audit events of one chain write the same id.
  - **Soul gRPC events** → `apply_id` is reused as `correlation_id` (one chain `RunResult` ↔ N `TaskEvent` ↔ `apply.started` audit event is linked).
  - **`signal` / `keeper_internal`** → a fresh ULID per reload-attempt / per Reaper-cycle. The field is optional (`NULL` is allowed for single standalone events).

  **(d) Retention — via a new Reaper rule `purge_audit_old`.** Cleanup of `audit_log` is built into the existing Reaper mechanism ([reaper.md](../keeper/reaper.md), [ADR-006(d)](0006-cache-redis.md#adr-006-кэш-и-координация--redis)) — not a separate worker, not a cron inside `shared/audit`. A new rule in `keeper.yml → reaper.rules`:

  ```yaml
  purge_audit_old:
    enabled: true
    max_age:  365d            # default
    action:   delete
  ```

  `action: delete` (per the rule-grammar table — [reaper.md → Rule structure](../keeper/reaper.md#структура-правила)), `max_age` mandatory. The exact `max_age` value is tuned to the installation's compliance requirements via hot-reload.

  **(e) Cross-host (HA cluster).** Each Keeper instance writes to the shared `audit_log` independently. Uniqueness via the `audit_id` ULID (the random component protects against collisions without coordination). Reading for `GET /v1/audit` — a standard SQL query with pagination; all instances see the same global picture without cross-host mechanisms.

  **(f) OTel dual-write — optional, default on.** An audit event is written both to Postgres (durable, source of truth) and to an OTel span as an attribute (transient, for distributed tracing). Postgres is the authoritative source, OTel is a debugging aid and cross-cutting correlation (Archon call → Keeper event → Soul call in one trace tree).

  Controlled via `keeper.yml → audit.otel_export: bool` (default `true`, see [config.md → audit](../keeper/config.md#audit)). The Soul side does **not** get a **separate** audit block in `soul.yml`: a Soul physically has no access to the Postgres `audit_log` (isolation — [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Soul-side audit events go through Keeper as `source: soul_grpc` (see (g)); OTel attributes are written by the Soul into its own gRPC EventStream span normally via the `otel:` block of `soul.yml`.

  **(g) Write-path — who writes audit.** A closed set of initiators inside the Keeper process (all write into the single `audit_log` via the shared helper `shared/audit`):

  | Initiator | `source` | When |
  |---|---|---|
  | HTTP middleware Operator API | `api` | On every authenticated request (including read-only); permission-deny → `*.access_denied` audit event. |
  | MCP handler | `mcp` | Same for MCP-tool calls. |
  | Reaper | `keeper_internal` | On every rule action (`reaper.purge_souls.executed`, `reaper.expire_pending_seeds.executed`, etc. — the concrete names are normalized as the rules are implemented). |
  | Hot-reload pipeline | `signal` or `api` | `config.reload_succeeded` / `config.reload_failed` ([ADR-021(g)](0021-hot-reload-config.md#adr-021-hot-reload-конфига-с-write-back-yaml)). |
  | `keeper.cloud` | `api` (if the initiator is an Archon via a scenario) or `keeper_internal` | `cloud.vm.created` / `cloud.vm.destroyed` ([ADR-017](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)). |
  | `keeper.push` | `api` or `keeper_internal` | `push.applied` / `push.failed`. |
  | Bootstrap (`keeper init`) | `keeper_internal` | The first `operator.created` with `archon_aid: NULL` ([ADR-013](0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). |
  | Soul gRPC events forwarded by Keeper | `soul_grpc` | `TaskEvent` / `RunResult` / `SoulprintReport` are received by Keeper and translated into audit, reusing `apply_id` as `correlation_id`. |

  **(h) Event-types catalog.** The name convention is `<area>.<action>` (lowercase, dots), like RBAC permissions ([rbac.md → Permissions format](../keeper/rbac.md#формат-permissions)). The catalog is open and extended by an ordinary PR in [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events) **as the subsystem is implemented** — we do not try to enumerate all 50+ names in advance. By the time ADR-022 was fixed, the catalog was filled with principles (5 categories by the `source` enum) and examples by area (`config.*`, `operator.*`, `incarnation.*`, `push.*`, `cloud.*`, `reaper.*`, `task.*`).

  **(i) Config block `audit:` in `keeper.yml`.** Three fields, all reloadable without restart:

  ```yaml
  audit:
    enabled:        true        # default
    otel_export:    true        # default — duplicate into OTel spans
    retention_days: 365         # default; alias to reaper.rules.purge_audit_old.max_age
  ```

  Normative typing — [config.md → audit](../keeper/config.md#audit). `retention_days` in `audit:` is an alias to `reaper.rules.purge_audit_old.max_age` (normalized by the current ADR, see (d)): one source of truth for retention, a convenient readable day form in `audit:` and a `duration` in `reaper:`. There is **no** audit block in `soul.yml` — rationale in (f).

  **(j) Read API `GET /v1/audit` — a separate task.** Not normalized in this ADR: implementation of the Operator API extension on the first real request. Permission — `audit.read` (one name), filters — `event_type` / `aid` / `correlation_id` / `date_range`, pagination — standard for the Operator API. Adding the endpoint to the Operator API + the permission in [rbac.md → Permissions catalog](../keeper/rbac.md#каталог-permissions) — without breaking changes ([ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon) allows extension without breaking).

  **(k) PII / Secret masking — shared mechanism.** The same rules normalized in [operator-api.md → Secret masking](../keeper/operator-api.md#secret-masking-в-логах-и-трейсах): JWT tokens / private_key / password / credentials_ref / Vault-ref values are masked with `***` before being written into `payload` or into OTel span attributes. The concrete list of masked fields — a shared middleware in `shared/audit` (on top of the same table used by the Operator API OTel exporter) — a separate security task before release. The audit pipeline does not introduce its **own** list of masked fields — it takes it from the shared registry in `shared/`.

- **Consequences.**
  - **Postgres table `audit_log`** — a mandatory registry, schema normalized in (a). [keeper/storage.md](../keeper/storage.md) is extended (Postgres-tables section). Migration — a separate task when implementing the first audit initiator.
  - **New Reaper rule `purge_audit_old`** — [reaper.md](../keeper/reaper.md) (rule table + structure + example). Parameterized via `keeper.yml → reaper.rules`.
  - **New block `audit:` in `keeper.yml`** — [config.md → audit](../keeper/config.md#audit). Three fields, all reloadable. The per-block reload-policy summary table in [config.md → Hot-reload](../keeper/config.md#hot-reload) is extended with a row.
  - **`shared/audit` package** — a shared helper for all write-path initiators (HTTP middleware, MCP handler, Reaper, hot-reload, `keeper.cloud`, `keeper.push`, bootstrap, Soul-event forwarder). Insert into `audit_log`, OTel dual-write per `audit.otel_export`, secret masking. The concrete public API — a separate Tier 2 implementation task.
  - **`GET /v1/audit` + permission `audit.read`** — a separate task when implementing the Operator API extension (see (j)).
  - **Event-types catalog in `docs/naming-rules.md → Audit-events`** — extended by an ordinary PR when a new subsystem is added (the full name catalog is not normalized in this ADR, see (h)).
  - **Cross-link sync.** ADR-014(e) / ADR-020(g) / ADR-021(g) — mention that the overall audit-pipeline normalization is here.
  - `examples/keeper/keeper.yml` — updated by a separate mini-task (add the `audit:` block).

- **Trade-offs.**
  - **Postgres-only vs. a separate audit-store.** Postgres is chosen: one dependency, SQL query out of the box, no new operational component. Growing volume (365 days × all subsystems × HA cluster) — an upcoming concern over time; mitigation — an archive partition into a separate DB post-MVP without breaking changes (the column-layer schema is stable, `audit_log` can be replicated into a data lake by a background worker). The alternative "write straight into Elasticsearch / S3 / Kafka" is rejected: overengineering at MVP, and at growth it will be a migration, not breaking.
  - **OTel dual-write vs. Postgres-only.** An optional dual-write is chosen (default on): distributed tracing is needed for cross-cutting correlation (Archon → Keeper → Soul in one trace), but the Postgres write is the authority (the OTel collector may be unavailable, audit will survive that). Toggleable via `audit.otel_export: false` — for installations without OTel infrastructure. The cost is a double write on every event; mitigation — OTel export is asynchronous, it does not block the hot path.
  - **A single shared `audit_log` in the HA cluster vs. per-host.** Shared is chosen: one place to query (`GET /v1/audit` does not coordinate cross-host), a natural global picture for a compliance audit. The cost is that all instances write into one table: under high write load (tens of thousands of events per second) partitioning by `created_at` may be required (a BRIN index on `created_at` fits the time-series pattern well). Mitigation post-MVP without breaking changes.
  - **`source` closed enum vs. freeform.** Closed — `soul-lint` and the `keeper.yml` parser can validate in a typed way; indexing is stable. The cost is that extending the enum requires a PR via propose-and-wait. It matches the general Soul Stack style (capabilities / side_effects are also closed, [ADR-020](0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)).
  - **Retention via Reaper vs. built-in partition-pruning.** Reaper is chosen: a single cleanup model across all Keeper tables ([reaper.md](../keeper/reaper.md)), one metrics and dry-run mechanism, hot-reload of `max_age` via the same block. The cost is Reaper batch deletion instead of an instant DROP PARTITION; acceptable for MVP scales. Partitioning `audit_log` by `created_at` — a post-MVP extension as volume grows, not breaking (`purge_audit_old` then replaces batch-DELETE with DROP PARTITION).

---

## Amendment pointer (2026-06-24, pluggable audit sink — see ADR-059)

The PG `audit_log` (this ADR) remains the **default and source-of-truth FOR NOW**. At the target scale (100k VM) synchronous PG writing of audit does not scale ([known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--рассчитан-на-малую-бету)); the direction of the decision — the audit-offload backend becomes **selectable** via `keeper.yml → audit.sink: pg | kafka | off` (choosing the `shared/audit.Writer` implementation, the abstraction already exists). A Kafka sink — opt-in, OPTIONAL-with-degradation ([ADR-053](0053-dependency-tiers.md#adr-053-tier-ы-инфраструктурных-зависимостей)), does not change the mandatory contour. The design (proposed / deferred, no code in the beta) — **[ADR-059](0059-audit-sink-pluggable.md)**; it also fixes the hard dependency: `changed_tasks` / `GET /v1/audit` today derive data from `audit_log` in PG and, in Kafka-only mode, require an alternative source (to be resolved BEFORE implementation).
