# ADR-005. Keeper State Storage — Postgres

- **Context.** The Keeper cluster is stateless by design (ADR-002): any instance can serve any request. This requires a shared DB holding the Souls registry, their certificate history, the Destiny catalog, run logs, and operator artifacts (tokens, RBAC policies). An embedded KV (BoltDB/BadgerDB) is incompatible with this model.
- **Decision.** Postgres as the sole cold-state storage for the Keeper cluster.
- **Rationale.** A mature transaction model, well-understood HA/backup/restore procedures, industry-proven operational practices, rich integrity constraints (`UNIQUE WHERE`, FK, CHECK), audit-friendly. No custom consensus and no raft forests.
- **Trade-off.** A heavyweight external dependency — Postgres has to be operated (backups, WAL, vacuum, versions, replication). For very small installations this is overkill; the target scale is **from dozens to thousands of Souls**, for which the PG overhead is justified. An alternative for very small installations (a single-binary Keeper with SQLite) is considered a **possible** future option via a storage interface; it is not a commitment at this time.
