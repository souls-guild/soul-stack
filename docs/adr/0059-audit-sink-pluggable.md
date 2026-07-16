# ADR-059. Pluggable audit sink — choosing the audit-export backend (PG / Kafka / off)

> **Status: proposed / deferred (design-only, 2026-06-24).** Design ADR. The directional decision was made by the user (at target scale audit is not written synchronously to PG, it is exported to Kafka, behind a toggle), but **there is no code for this ADR in the beta** — this is a post-beta plan, recorded BEFORE implementation (documentation ahead of code). Design — architect. Before implementation, decoupling the Herald/`changed_tasks` dependency on audit-in-PG is mandatory (see open question (n)). The working name of the entity is **"audit sink"** (config key `audit.sink`); the thematic name **Chronicle** is an optional alternative, NOT recorded in [naming-rules.md](../naming-rules.md) (up to PM/user).

**Context.** [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) standardized the audit pipeline on a single backend: `audit_log` in the shared Postgres ([ADR-005](0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)), single source of truth, retention via the Reaper rule `purge_audit_old`. The ADR-022 trade-offs already noted: "Postgres-only vs a separate audit-store... growing volume (365 days × all subsystems × HA cluster) — an upcoming concern over time". [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--sized-for-a-small-beta) honestly records: at the target scale of **100k VM** the volume of runs hits the INSERT rate and table size — `audit_log` is the main consumer of PG volume.

Synchronous writing of every audit event to PG does not scale: a single run over 100k hosts produces hundreds of thousands of `task.executed` INSERTs. The user's decision (2026-06-24): at target scale, audit is **not written synchronously to PG** and **no in-app preview is built** (preview = read load on PG, the same concern from the read side) — events are **exported to Kafka**, behind explicit toggles. This is a plan, not code in the beta: the beta stays on the PG sink (a small set of Souls, `audit_log` is more than enough).

The abstraction point already exists and needs no rework: `shared/audit.Writer` — an async/best-effort interface; `MultiWriter` provides a tap point (used by [Herald](0052-herald-notifications.md#adr-052-herald--tiding--notifications-of-run-events) (c)). What is **not recorded:** that there can be different sink implementations, how they are selected in config, what delivery guarantees the Kafka sink provides (audit is compliance-critical — events must not be lost), how this fits the tier model ([ADR-053](0053-dependency-tiers.md#adr-053-tiers-of-infrastructure-dependencies)), and how, in Kafka-only mode (without audit-in-PG), the subsystems that today **derive data from audit-in-PG** (Herald, `changed_tasks`) operate.

**Decision (direction; implementation deferred).**

**(a) Sink abstraction behind `shared/audit.Writer`.** The concrete audit-export backend is a choice of `audit.Writer` **implementation**, not a new interface and not a new dictionary entity. The existing abstraction (`Writer` async/best-effort + `MultiWriter` tap) is sufficient. The sink is selected in `setupAudit` ([keeper/cmd/keeper/daemon.go](../../keeper/cmd/keeper/daemon.go), the wiring point) based on config and assembled once at startup. The Herald tap chain (`MultiWriter` decorator) **stays on top of the selected sink** — Herald is layered over any backend (but see limitation (n): today Herald derives `changed_tasks` via an SQL query over `audit_log`, not from a tap event).

**(b) Three values of `audit.sink`.** Config `keeper.yml → audit.sink: pg | kafka | off`:

| Value | Backend | Behavior |
|---|---|---|
| **`pg`** | `audit_log` in Postgres ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) | **default**, current behavior; implementation — the existing PG writer (working package name — `auditpg`). Source of truth, retention via Reaper. |
| **`kafka`** | Kafka topic | new sink; events are serialized and published to Kafka, a downstream consumer puts them into long-term storage (data lake / SIEM / ClickHouse — outside Soul Stack). PG `audit_log` is **not written** (or written in truncated form — see (n)). |
| **`off`** | — | audit is not exported anywhere. A deliberate operator choice (dev / ephemeral stand); **not** the default. |

Extending the enum (e.g. `s3`/`elasticsearch`) — propose-and-wait, additive, non-breaking. `off` is a separate value, **not** `enabled: false` (ADR-022(i) `audit.enabled` remains an orthogonal master toggle; `sink: off` explicitly says "backend = nowhere", which reads as a deliberate choice rather than "forgot to configure").

**(c) Config block (extension of `audit:` from ADR-022(i)).** Form (normative typing — [config.md → audit](../keeper/config.md#audit), docs-writer at implementation time):

```yaml
audit:
  enabled: true            # ADR-022(i), master toggle (orthogonal to sink)
  sink:    pg              # pg | kafka | off  (default pg)
  otel_export: true        # ADR-022(f)
  retention_days: 365      # ADR-022(d), relevant only for sink=pg

  kafka:                   # read only when sink=kafka
    brokers:  ["broker-1:9092", "broker-2:9092"]
    topic:    "soul-stack.audit"
    acks:     all          # at-least-once guarantee — do NOT weaken (see (d))
    # connection secrets (SASL/TLS) — vault-ref, NOT cleartext (Herald secret_ref pattern)
    sasl_ref: "secret/keeper/kafka-audit/sasl"
    tls_ref:  "secret/keeper/kafka-audit/tls"
```

`audit.kafka.*` is read only when `sink: kafka`. Kafka connection secrets are **vault-ref** (the [ADR-052(e)](0052-herald-notifications.md#adr-052-herald--tiding--notifications-of-run-events) `secret_ref` / [ADR-025](0025-augur.md#adr-025-augur--keeper-side-broker-for-soul-external-access) `auth_ref` pattern), not PG/disk cleartext.

**(d) Delivery guarantees — Kafka at-least-once, fail-closed degradation.** Audit is compliance-critical ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — SOC2/ISO: identity/authn/authz must be logged) — **audit events must not be lost**. The Kafka sink provides **at-least-once**, not at-most-once:

- **Producer `acks=all`** (synchronous acknowledgement from all in-sync replicas) — mandatory, do not weaken to `acks=1`/`acks=0`.
- **Degradation when Kafka is unavailable — fail-closed**, not fail-open. The fail-open/fail-closed concept comes from [ADR-053](0053-dependency-tiers.md#adr-053-tiers-of-infrastructure-dependencies) (Sigil/Purview = fail-closed, Tempo = fail-open). Audit fail-closed means: when an audit event write cannot be confirmed, the sink chooses a **local durable fallback** (see below) or blocks the write path, rather than silently dropping the event. **Which exact fail-closed mechanism is an open design question (m).**
- **Dedup — downstream by `audit_id`.** at-least-once → a duplicate event in the topic is possible. `audit_id` — a ULID, already the PK in the schema ([ADR-022(a)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — serves as the deduplication key on the consumer side (the consumer is idempotent by `audit_id`). Dedup is the downstream consumer's concern, not Soul Stack's.

**(e) Kafka write guarantee — two candidates (open choice, m).** at-least-once on the operator-critical path requires that the success of a business operation does not outrun the write of its audit fact. Two patterns (choice deferred to implementation):

1. **Transactional outbox** — the audit event is written to a PG outbox table in **the same transaction** as the business change; a separate relay worker (claim-queue parity with [ADR-027(d)](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)/[Herald delivery](0052-herald-notifications.md#adr-052-herald--tiding--notifications-of-run-events)) moves it to Kafka and marks it delivered. The strongest guarantee (atomicity with the business tx), but it **does not remove the load from PG INSERT** (the outbox INSERT is of the same order as the `audit_log` INSERT) — contradicting the motive of "removing PG writes". The outbox table lives shorter (truncated after relay), but the write rate is the same.
2. **Direct producer `acks=all`** — synchronous publishing to Kafka on the write path without PG. Removes PG writes entirely (motive achieved), but requires its own durable fallback in case Kafka is unavailable (otherwise at-least-once is violated — fail-closed (d)).

Trade-off (m): outbox = strong guarantee at the cost of retaining PG writes (motive partially unmet); direct = achieved motive at the cost of a complex fallback. The choice — at implementation time, not in this ADR.

**(f) Tier — Kafka is strictly OPTIONAL, not a 4th required.** Per [ADR-053](0053-dependency-tiers.md#adr-053-tiers-of-infrastructure-dependencies): the required core remains **PG + Redis + Vault**. Default `audit.sink: pg` — the required core is intact, no new dependency. Kafka — **OPTIONAL-with-degradation**: it appears as a requirement only with an explicit `sink: kafka`. The ADR-053 rule "a new required component — only through an explicit user decision" is honored: Kafka does not become a fourth required-by-default. A row in the ADR-053 OPTIONAL table is added. The Kafka sink's degradation — **fail-closed** (d) — is a deliberate security trade-off, explicitly recorded (audit must not be lost).

**(g) Switching the sink — restart-required.** Changing `audit.sink` (as well as `audit.kafka.*`) is **not hot-reload**, it requires a restart of the Keeper instance; the `web_ui_enabled` pattern ([ADR-055](0055-embed-ui-bundle.md)). The sink is selected and assembled once in `setupAudit` at startup; changing the backend on the fly (re-wiring the write path of all initiators + the tap chain) is unnecessary complexity for a rare operation. In the per-block reload-policy table [config.md → Hot-reload](../keeper/config.md#hot-reload), `audit.sink`/`audit.kafka.*` are marked restart-required (`audit.enabled`/`otel_export`/`retention_days` remain reload-able per ADR-022). docs-writer at implementation time.

**(h) shared stays pgx-free.** The [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules) invariant: `shared/` — cross-cutting Soul-safe code without server dependencies (no pgx). The sink abstraction (`Writer`/`MultiWriter`) lives in `shared/audit` and stays backend-agnostic. **Both** concrete implementations are in `keeper/internal`: the PG sink (working name `auditpg`) and the new Kafka sink. The Kafka client is pulled into `keeper/` (server-side), not into `shared/`. This preserves: `shared` without pgx and without the Kafka driver, and Soul isolation ([ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules) — Soul pulls neither PG nor Kafka).

**(i) Relation to the audit-scaling backlog.** [known-limitations.md → Audit-scaling](../known-limitations.md#audit-scaling--sized-for-a-small-beta) lists options for large Souls installations: partitioning, hot-cold, batched INSERT, **Redis Stream buffering**. The Kafka sink is a point on the **write-throughput** axis:

- **Supersedes the Redis Stream option** — Kafka covers the same task (buffering the audit stream while offloading PG writes) more fully: a durable log with downstream consumers, dedup by `audit_id`, without loading Redis (Redis is the hot layer [ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis), its role ≠ a long-term audit buffer). The Redis Stream option is removed from the backlog.
- **batched INSERT remains** as a cheaper alternative on the write-throughput axis for installations where Kafka is overkill but the synchronous per-event INSERT already pinches: batching PG INSERTs (sink=pg + batch flush) lowers the write rate without new infrastructure. This is an orthogonal optimization of the PG sink, not superseded by Kafka.
- **Partitioning / hot-cold** — axes of the PG sink's **storage size**, not relevant to the Kafka sink (with Kafka-only, PG `audit_log` does not grow), they remain in the backlog for `sink=pg`.

**(j) `off` — boundaries.** `sink: off` disables audit export entirely. Permissible for dev / ephemeral stands. Operations note (docs-writer at implementation time): `off` removes the compliance guarantee ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) — the log at startup must clearly warn that audit is not being written (the "degradation is explicit" pattern [ADR-053](0053-dependency-tiers.md#adr-053-tiers-of-infrastructure-dependencies)). `off` ≠ a silent no-op.

**Consequences (at implementation time; no code in the beta).**

- **`shared/audit`** — the sink abstraction already exists (`Writer`/`MultiWriter`), no new code required; only the wiring in `setupAudit` changes. The pgx-free invariant (h) is preserved.
- **`keeper/internal` — new Kafka sink** alongside the PG sink (`auditpg`); the Kafka client in `keeper/` go.mod (h).
- **`setupAudit` ([daemon.go](../../keeper/cmd/keeper/daemon.go))** — the sink-selection point based on `audit.sink`; the Herald tap chain is layered over the selected sink (subject to (n)).
- **Config `audit:`** — the `sink` field + the `kafka.*` block (c); restart-required (g); [config.md](../keeper/config.md#audit) (docs-writer).
- **[known-limitations.md](../known-limitations.md#audit-scaling--sized-for-a-small-beta)** — clarification: the Kafka sink is designed (this ADR), not implemented in the beta; it supersedes the Redis Stream option, batched INSERT remains (i).
- **[ADR-053](0053-dependency-tiers.md#adr-053-tiers-of-infrastructure-dependencies)** — a row in the OPTIONAL table (Kafka sink, fail-closed).
- **[ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** — amendment pointer: PG remains default + source-of-truth FOR NOW, the sink is abstracted, Kafka is opt-in (this ADR).
- **Names** — working `audit sink` / key `audit.sink` / values `pg`/`kafka`/`off`. The thematic **Chronicle** is a candidate alternative, **not** recorded in [naming-rules.md](../naming-rules.md) (up to PM/user). Recording the name in naming-rules is a separate propose-and-wait step if the thematic variant is chosen.

**Open questions / dependencies (BEFORE implementation).**

- **(n) CRITICAL — Herald and `changed_tasks` today derive data from audit-in-PG.** This is a blocking dependency, not glossed over:
  - **`incarnation.run_completed` → `changed_tasks`** ([ADR-052 §k](0052-herald-notifications.md#k-the-run-terminal-event-incarnationrun_completed-carries-changed_tasks)) is assembled as follows: `scenario.Runner` at the end of a run **reads the `audit_log` aggregate** via an SQL query (`WHERE correlation_id = apply_id AND event_type = 'task.executed' AND payload->>'status' = 'TASK_STATUS_CHANGED'`) and folds it into `changed_tasks`. The source of the changed fact is **the audit log in PG** (explicitly: "the source of `changed` is the audit log, not a new table").
  - **`GET /v1/audit` read API** ([ADR-022(j)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) and **audit filters** of Herald visibility (`payload_voyage`/`payload_herald`, [ADR-052 §k visibility](0052-herald-notifications.md#amend-k-visibility-2026-06-12-optional-voyage_id--audit-filter-payload_voyage)) — SQL over `audit_log`.

  With **`sink: kafka` and no PG write** this source disappears: `audit_log` is empty (or truncated) → the `changed_tasks` fold is empty → Herald notifications about the per-task changed breakdown **silently break**, `GET /v1/audit` returns nothing. **This must be resolved BEFORE implementing the Kafka sink.** Directional candidates (the choice is a separate design pass with architect/user, NOT in this ADR):
    1. **An alternative source of the changed fact** — `scenario.Runner` assembles `changed_tasks` from the in-memory run aggregate (it already holds `[]RenderedTask` and sees per-`(sid, task_idx)` statuses), not from SQL over `audit_log`. This decouples the fold from the audit backend. This is a standalone decoupling, useful even with `sink: pg`.
    2. **Hybrid sink** — `task.executed` (and run terminals) are **always** written to PG (this is the "hot" operational layer needed by Herald/`changed_tasks`/`GET /v1/audit`), while the **full** stream goes to Kafka for compliance/SIEM. Then `sink: kafka` means "Kafka **in addition**" rather than "instead of PG" — but the "remove PG writes" motive is only partially achieved (the hot operational stream stays in PG).
    3. **The read API and Herald migrate to a Kafka consumer projection** — a separate read model from Kafka. A heavy option, a separate subsystem.

  Without a decision on (n), the Kafka-only mode **cannot be implemented** — it silently breaks Herald notifications and audit reads. Recorded as a hard dependency.

- **(m) Kafka write guarantee — outbox vs direct producer** (see (e)): strong guarantee at the cost of PG writes vs achieved motive at the cost of a durable fallback. The concrete fail-closed fallback (d) (a local spool file? blocking the write path? a buffer with alerting?) is part of this question. The choice — at implementation time.

- **(o) In-app audit preview.** The user's decision — do not build a preview (read load on PG = the same concern). The current `GET /v1/audit` ([ADR-022(j)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) and the Audit Log UI ([known-limitations.md](../known-limitations.md), Oracle fires → audit filter), with `sink: kafka` and no PG, lose their source — this intersects with (n). The read-API behavior with a non-PG sink (404? empty? projection?) — to be decided together with (n).

**Trade-offs.**

- **Pluggable sink vs hardcoded PG.** Pluggable — the 100k VM scale requires offloading PG writes; the abstraction already exists (`Writer`), the cost is low. The cost — config complexity + dependency (n). PG remains the default → the beta and small installations pay nothing.
- **Kafka vs Redis Stream (backlog).** Kafka — a durable log, dedup by `audit_id`, downstream consumers, does not load Redis (the hot layer). Redis Stream would offload PG writes, but Redis is not a place for a long-term compliance buffer ([ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis) hot→Redis invariant). Kafka supersedes the Redis Stream option (i).
- **fail-closed vs fail-open when Kafka is unavailable.** fail-closed — audit is compliance-critical, event loss is unacceptable ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). The cost — the write path degrades when Kafka fails (slower/blocked/spool), rather than "continuing without audit". A deliberate security trade-off, opposite to Tempo's fail-open ([ADR-050](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)).
- **restart-required vs hot-reload sink.** restart — re-wiring the entire write path + tap on the fly for a rare operation (changing the audit backend) is not justified; the `web_ui_enabled` pattern ([ADR-055](0055-embed-ui-bundle.md)).
- **`sink: off` — a separate value vs `enabled: false`.** A separate value — `off` reads as a deliberate "export nowhere", whereas `enabled: false` reads as the master switch of the audit logic; they are separated to keep the config explicit.

**Relation to ADRs.**

- **[ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)** — standardized the PG sink (`audit_log`); this ADR abstracts the backend, PG remains default + source-of-truth FOR NOW. Amendment pointer in ADR-022.
- **[ADR-053](0053-dependency-tiers.md#adr-053-tiers-of-infrastructure-dependencies)** — Kafka sink = OPTIONAL-with-degradation (fail-closed); PG+Redis+Vault remain the only required core; the rule "a new required — only through a user decision" is honored.
- **[ADR-052](0052-herald-notifications.md#adr-052-herald--tiding--notifications-of-run-events)** — Herald tap over the sink + the `changed_tasks` dependency on audit-in-PG (open question (n)).
- **[ADR-006](0006-cache-redis.md#adr-006-cache-and-coordination--redis)** — Redis hot layer ≠ a long-term audit buffer (why Kafka, not Redis Stream).
- **[ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules)** — `shared` pgx-free + backend-agnostic; both sink implementations in `keeper/internal` (h).
- **[ADR-027](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim)** — the claim-queue pattern for the outbox-relay candidate (e).
- **[ADR-055](0055-embed-ui-bundle.md)** — the restart-required toggle pattern (`web_ui_enabled`).
- **[ADR-050](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)** — the contrast fail-open vs audit fail-closed.
