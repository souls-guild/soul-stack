# Community plugins directory

Third-party plugins: they are **not** built into the `soul` binary (unlike core modules
`core.*`) and **not** supplied by the Soul Stack team (as opposed to
[official/README.md](../official/README.md)). Via the SoulModule contract
(gRPC-over-stdio, [ADR-020](../../adr/0020-plugin-infrastructure.md)) such a plugin is used
on the host along with core modules.

> ⚠ **`community` is not a namespace.** It named these plugins' **origin**, not what they
> manage, and the origin-grouping level of a plugin address is **removed**
> ([ADR-020 amendment 2026-09-02](../../adr/0020-plugin-infrastructure.md#amendment-2026-09-02-nim-764--nim-765-a-plugin-address-is-pluginobjectaction-and-the-origin-grouping-level-is-removed),
> NIM-765): a plugin step is addressed `<plugin-name>.<object>.<action>`, and origin is
> answered by the catalog entry's `source` plus the Sigil allow-list. Every `community.*`
> address on this page is therefore the **old form** — and it is the form that ships, because
> the artifact registers under the alias `community` and serves one module named `redis`
> (`mongo`). The conversions are **NIM-766** (redis) and **NIM-769** (mongo); this directory
> keeps its name and path until each artifact is re-laid-out.

Binari - `soul-mod-community-<name>`. Implementation - in
[`examples/module/`](../../../examples/module/) (in this repository - as
reference example; assembled separately `go.mod`, not included in `go.work`).

## Implemented

| Plugin | States | Destination |
|---|---|---|
| [`community.redis`](redis/README.md) ⚠ **LEAVING THE DICTIONARY (NIM-766)** | `command` / `pinged` / `role` / `replica-synced` / `offset-synced` / `config` / `acl` / `cluster` / `replica` / `detached` / `sentinel` (12) | MAIN interface to **live Redis** (role-based concept): scenario orchestrates order/rolling, plugin performs one operation on one instance. Backend - `go-redis/v9`. Covers hot-reload (`config`/`acl`), cluster build and day-2 evolution (`cluster` create/add-node/remove-node/reshard), live migration between clusters (`cluster` join-external/failover-takeover/forget-external), replication and migration from an external source (`replica source_external` → `offset-synced` → `detached`), Sentinel reconciliation. |
| [`community.mongo`](mongo/README.md) ⚠ **LEAVING THE DICTIONARY (NIM-769)** | `pinged` / `user` / `command` (3, **PILOT**) | MAIN interface to **live MongoDB** (PILOT slice, role-based concept): scenario orchestrates order/health-gate, plugin performs one operation on one `mongod` instance. Backend - `go-mongo-driver` (not `mongosh`). `user` (createUser/dropUser upsert) creates the first admin via **localhost-exception** (analogous to redis `default_admin` bootstrap: no-auth loopback connection while the admin database is empty). PILOT scope - **standalone** only; replica-set / sharded / keyFile / TLS - the following slices. |

## Agreements (common for community plugins)

- **Capability-minimum.** The plugin declares exactly those `required_capabilities` that it
are needed ([ADR-020(f)](../../adr/0020-plugin-infrastructure.md)).
`community.redis` and `community.mongo` - only `network_outbound`, **without**
`vault_access`: secrets (password, PEM) are resolved by Keeper in the render phase and transferred
plugin value
([ADR-012](../../adr/0012-keeper-soul-grpc.md)),
The plugin does not need its own Vault client.
- **Secret masking.** Secret fields are declared `secret: true` + `pattern: "^vault:.*"`
(vault source token for auditing); masking - output layer `shared/audit`
([templating.md §7.4](../../templating.md)). Invariant code "the secret does not flow into
events/logs/errors" is checked by guard tests (L0).
- **Dry-run.** `PlanReadSafe` ([ADR-031 Scry](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile))
implements the plugin at its own discretion. `community.redis` and `community.mongo` it
**not** implemented deliberately: on `dry_run` host applies default-deny (honest
"preview not supported" rather than the false "no drift").

## See also

- [README.md](../README.md) - modules directory (core + links to official/community).
- [official/README.md](../official/README.md) - plugins supplied by the Soul Stack team.
- [ADR-016](../../adr/0016-parity-license.md) - parity strategy: core rewrite + community plugins `soul-mod-*`.
- [ADR-020](../../adr/0020-plugin-infrastructure.md) - plugin infrastructure (manifest/handshake/lifecycle).
- [soul/modules.md](../../soul/modules.md) - host side: where the plugin binaries, cache, cleanup are located.
