# Operational runbook Soul Stack

Reference documentation for DevOps / SREs deploying and operating Soul Stack in production. Focuses on **operational details**: what to deploy, how to backup, how to scale, what to do in case of failure. Not a tutorial or a repeat of the architecture - for the architecture, see [`docs/architecture.md`](../architecture.md) with ADR-001...053 ([index](../adr/README.md)).

The documentation is divided into logical zones. One section - one topic, which can be referenced separately.

| File | Zone |
|---|---|
| [deployment.md](deployment.md) | Deploying binaries: artifacts, system requirements, systemd, multi-keeper for LB, basic rollout. |
| [bootstrap-rbac.md](bootstrap-rbac.md) | Bootstrap of the first Archon (`keeper init`), creation of additional Archons, RBAC. |
| [infra.md](infra.md) | Infra-dependencies: Postgres (backup/restore/retention/sizing), Redis (what lies, TTL/eviction), Vault (KV/PKI/SSH/Sigil, rotation). |
| [scaling.md](scaling.md) | Horizontal scaling Keeper: Conclave, Watchman, Acolyte-pool, Refuse-guard, LB. |
| [upgrade.md](upgrade.md) | Rolling upgrade Keeper / Soul: forward-compat proto, state_schema migrations, rollback. |
| [monitoring.md](monitoring.md) | Prometheus metrics, OTel traces, key alerts, logs. |
| [disaster-recovery.md](disaster-recovery.md) | Recovery from PG/Redis/Vault/Keeper/total disaster. |
| [recovery-reclaim-apply-runs.md](recovery-reclaim-apply-runs.md) | Enabling Reaper rule `reclaim_apply_runs` in production (operationalization of GATE-1 from ADR-027). |
| [faq.md](faq.md) | Frequently encountered problems and their triage (zombie Souls, hanging applying, drift 422, ...). |

## What's not here

This folder is **only the operational details** of a specific installation. Architectural rationales, contracts and regulatory specifications live in the core documentation, see below.

| Topic | Where to look |
|---|---|
| Architectural solutions (ADR-001…053) | [`docs/architecture.md`](../architecture.md), [ADR index](../adr/README.md). |
| Dictionary of names | [`docs/naming-rules.md`](../naming-rules.md). |
| Threat model (assets, surfaces, residual risks, environmental requirements) | [`docs/security/threat-model.md`](../security/threat-model.md). |
| Config `keeper.yml` (field types, parser diagnostics) | [`docs/keeper/config.md`](../keeper/config.md). |
| Config `soul.yml` | [`docs/soul/config.md`](../soul/config.md). |
| Prod-deployment of Keeper (Vault AppRole + persistent + auto-unseal + signing-key) | [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md). |
| Reaper-rules, recovery-enable | [`docs/keeper/reaper.md`](../keeper/reaper.md). |
| RBAC: permissions directory, scope filters | [`docs/keeper/rbac.md`](../keeper/rbac.md). |
| Operator API (HTTP/JSON), MCP-tools | [`docs/keeper/operator-api.md`](../keeper/operator-api.md), [`docs/keeper/mcp-tools.md`](../keeper/mcp-tools.md). |
| Observability: metrics, namespaces, OTel resource-attrs | [`docs/observability.md`](../observability.md). |
| Audit-pipeline (storage, schema, retention) | [`docs/architecture.md` → ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention). |
| State_schema migrations DSL | [`docs/migrations.md`](../migrations.md). |
| Local dev stack (docker-compose) | [`docs/dev/local-setup.md`](../dev/local-setup.md). |
| Layout `deploy/` (Dockerfile/systemd/nfpm) | [`deploy/README.md`](../../deploy/README.md). |

## Operating Model Principles

Briefly, for context when reading the following sections:

- **Stateless cluster Keeper** ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). N instances on top of a common Postgres ([ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)) and Redis ([ADR-006](../adr/0006-cache-redis.md)). Each instance has its own `kid`, unique in the cluster.
- **Hot → Redis, cold → Postgres.** Presence Souls (SID-lease), presence of Keeper instances ([Conclave](../adr/0006-cache-redis.md)), heartbeat cache, leadership lease, pub/sub - in Redis. Registries (`souls` / `operators` / `incarnation` / `state_history` / `apply_runs` / `audit_log` / `rbac_*` / `service_registry` / `plugin_sigils`) - in Postgres. Invariant - for scale up to 100k VM.
- **Safety comes first** ([requirements.md](../requirements.md)). default-deny RBAC, JWT with short TTL, mTLS Keeper↔Soul, Vault resolution of all secrets at start, no plaintext secret in `*.yml`.
- **Documentation before the code.** If the runbook and reality diverge, first correct the canon in `docs/`, then the code/procedure.
