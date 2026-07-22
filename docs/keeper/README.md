# Keeper - index

Documentation for Keeper - the central server of the Soul Stack. Keeper stores the Souls registry and Destiny catalog, validates and distributes runs, aggregates Soulprint, exposes OpenAPI/MCP/gRPC; operates as a horizontally scalable stateless cluster on top of shared Postgres and Redis.

## Where to start

| Document | What about |
|---|---|
| [concept.md](concept.md) | What is Keeper: role, HA-stateless model, KID, operator interfaces, what is included in the `keeper` binary (module `keeper.push`). |
| [storage.md](storage.md) | Keeper cluster storage: both in Postgres (souls, soul_seeds, bootstrap_tokens, Destiny catalog, incarnation, state_history), and in Redis (heartbeat cache, lease on SID, pub/sub, leadership lease `reaper:leader` / `conductor:leader`). |
| [push.md](push.md) | Push mode (`keeper.push`): Destiny SSH delivery without Soul agent, push↔agent migration, `/var/lib/soul-stack/` layout, running algorithm. |
| [reaper.md](reaper.md) | Reaper: background database cleaning (cleanup domain), leader lease `reaper:leader`, rules (`expire_pending_seeds`, `purge_used_tokens`, ...), Charon name reserve. Cadence spawn is moved to Conductor (ADR-048). |
| [conductor.md](conductor.md) | Conductor: leader-elected executor of Cadence schedules, lease `conductor:leader` (independent of Reaper), tick-interval `cadence_scheduler.interval`, default-ON with Redis (footgun-guard), metrics `keeper_conductor_*` ([ADR-048](../adr/0048-conductor.md)). |
| [plugins.md](plugins.md) | Keeper plugin infrastructure: contracts `CloudDriver` and `SshProvider`, plugin directory in `keeper.yml`. |
| [modules.md](modules.md) | Keeper-side core modules: `core.soul.registered` specification (SID binding to souls registry coven tags), Soul-side/Keeper-side dispatcher via `on:`. |
| [cloud.md](cloud.md) | Cloud integration (`keeper.cloud`): Provider and Profile in Postgres, cloud-create as a scenario step, destroy security. |
| [augur.md](augur.md) | **Design (no implementation)** external access broker Soul (Augur): live access to Vault / Prometheus / ELK during render / apply, two phases (broker `delegate=false` / delegation `delegate=true`), registries `omens` / `rites` in Postgres, regulatory invariant "master-credential not on Soul" ([ADR-025](../adr/0025-augur.md)). |
| [rbac.md](rbac.md) | RBAC: roles and permissions, single application to OpenAPI / MCP / push. |
| [operator-api.md](operator-api.md) | Operator API: Keeper HTTP endpoints (`/v1/*`), conventions, JWT auth, RFC 7807 errors, mapping endpoint ↔ MCP-tool ↔ permission, request/response schemas. Detailed endpoint sections of large domains are placed in the subfolder [`operator-api/`](operator-api/) (paired with [`mcp-tools/`](mcp-tools/)). |
| [run-flavors.md](run-flavors.md) | Summary table of entry-points for starting work on hosts: single-incarnation scenario via agent, batch via Voyage (`kind=scenario` / `kind=command`), single-Errand exec, push via SSH. Decompose "what" (scenario/module) vs "how" (agent/ssh) vs "where" (target). |
| [mcp-tools.md](mcp-tools.md) | MCP-tools directory: transport (MCP-HTTP), auth (JWT Bearer), declaration format according to MCP spec, `_apply_id`-convention for async, error mapping RFC 7807 → MCP-tool error, directory 72 tool 1:1 with Operator API. Detailed tool sections are placed in the subfolder [`mcp-tools/`](mcp-tools/). |
| [config.md](config.md) | Full bypass of `keeper.yml`: blocks `kid`, `listen`, `postgres`, `redis`, `vault`, `auth`, `otel`, `logging`, `plugins`, `plugin_runtime`, `audit`, `hot_reload`, `reaper`, `cadence_scheduler` (Conductor, ADR-048). Moved to the database (rejected as `unknown_key`): `rbac` (ADR-028), `services`/`default_destiny_source`/`default_module_source` (ADR-029). Reserved (non-regulatory): `reactor`. |
| [prod-setup.md](prod-setup.md) | Prod deployment: differences from dev, Vault AppRole + persistent + auto-unseal, least-privilege [vault-policy.hcl](../../examples/keeper/vault-policy.hcl), JWT signing-key rotation, recovery-enable gate. |

## Related Documents

- [`docs/architecture.md`](../architecture.md) - source of truth on architecture:
  - [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) - gRPC bidi + HA Keeper cluster.
  - [ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) - binary layout.
  - [ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres) - Postgres as the only cold storage.
  - [ADR-006](../adr/0006-cache-redis.md) - Redis as a hot layer and coordination.
  - [ADR-007](../adr/0007-versioning-git-ref.md) - version of artifacts via git ref.
  - [Soul Stack artifacts: what's in git, what's in the database](../architecture.md) - git/PG boundary.
- [`docs/naming-rules.md`](../naming-rules.md) - dictionary of names (Keeper, KID, Reaper/Charon, `keeper.push`, ...).
- [`docs/soul/`](../soul/README.md) - adjacent folder about `soul` binary: Soul identity, modules, host cleanup.
- [`docs/destiny/`](../destiny/README.md) - Destiny as an artifact that Keeper stores and distributes.
- [`docs/requirements.md`](../requirements.md) - end-to-end requirements (OpenAPI, MCP, RBAC, Vault, OTel, hot-reload, log rotation).
- [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) - a working example of the config for one Keeper cluster instance.
