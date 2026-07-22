# Keeper - concept

Keeper is the central server of Soul Stack. The control node to which Souls connect and through which the operator manages the Souls.

## Role

- **Souls Registry.** Stores and maintains tables `souls`, `soul_seeds`, `bootstrap_tokens` in Postgres ([storage.md](storage.md)). Accepts CSR during onboarding, issues SoulSeed certificates via Vault PKI, monitors heartbeat and statuses.
- **Destiny directory.** Pulls Destiny / Service / Module git repositories by `ref:` from the config ([config.md](config.md)), validates and renders steps before distribution. See [architecture.md → Soul Stack Artifacts](../architecture.md).
- **Distributing runs.** In pull mode, sends Souls commands via a live gRPC bidi stream over mTLS ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). In push mode, it itself navigates via SSH through the module `keeper.push` ([push.md](push.md)).
- **Soulprint aggregation.** Souls sends prints (facts about the host) - Keeper adds them to Postgres, sends them through the API with an RBAC filter.
- **Integration with Vault.** Full client: Essence secrets, PKI for SoulSeed release, SSH-CA for `keeper.push`, cloud driver credentials ([config.md](config.md) → block `vault:`).

## HA cluster, stateless

Keeper - **horizontally scalable stateless cluster** ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). Several instances `keeper` with different KIDs are behind the common Postgres and Redis. Any instance can serve any request - the source of truth lies outside the binary:

- **Postgres** - cold storage: registries, Destiny directory, incarnation, state_history, logs ([ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)).
- **Redis** - hot layer and coordination: Souls heartbeat cache, lease on SID, pub/sub between Keeper instances, leader lease for Reaper ([ADR-006](../adr/0006-cache-redis.md)).

Details of the data layout are in [storage.md](storage.md). A soul-side algorithm for selecting a Keeper from a list of endpoints is in [../soul/connection.md](../soul/connection.md).

## KID

Each Keeper cluster instance has a **KID** (Keeper ID) - a stable human-readable identifier. Used for:

- **Lease on SID** in Redis: `SET sid:lock <kid> NX EX <ttl>` - which Keeper currently has an active gRPC stream to this Soul.
- **Column `last_seen_by_kid`** in `souls` - which Keeper last saw this Soul.
- **Audit logs and metrics** - for separating events by instance.

KID is set in the config in one line (see [config.md](config.md) → `kid:`).

## Operator Interfaces

Primary path is **OpenAPI and MCP** ([ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)). Everything the operator does (establishing Souls, issuing bootstrap tokens, creating incarnation, push runs, managing Provider/Profile, reading Soulprint) is done through OpenAPI or MCP-tool. RBAC applies to both uniformly ([rbac.md](rbac.md)).

The CLI is acceptable as a **thin wrapper** on top of the API, not as the primary path. Will it be a separate binary, a subcommand `keeper` in client mode, or only third-party tools - [open Q No. 2](../architecture.md).

Internal transport Keeper ↔ Souls - gRPC bidi on a separate listener, externally via mTLS.

## What is included in the `keeper` binary

By [ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) Soul Stack supplies three separate binaries (`keeper`, `soul`, `soul-lint`). Contains `keeper`:

- **gRPC server for Souls** - receiving bidi streams, mTLS, releasing SoulSeed.
- **OpenAPI + MCP facade** - gRPC-Gateway/connect-go on top of the same kernel, MCP server.
- **Module `keeper.push`** - SSH delivery of Destiny to hosts without a Soul agent. Not a separate binary, see [push.md](push.md).
- **Module `keeper.cloud`** - cloud operations (Provider/Profile, CloudDriver plugins), see [cloud.md](cloud.md).
- **Reaper / Reaper** - background cleaning of the database, leader via Redis-lease, see [reaper.md](reaper.md).
- **Plugin-host for CloudDriver and SshProvider** - sub-process by gRPC-stdio, see [plugins.md](plugins.md).
- **Integrations out of the box** - Vault, OTel, Prometheus metrics, RBAC, log rotation, hot-reload config ([requirements.md](../requirements.md)).

The `keeper` binary **does not include**: agent server code, implementation of Destiny core modules (this is `soul`), offline linter (this is `soul-lint`).

## See also

- [storage.md](storage.md) - where the registries and cache live.
- [push.md](push.md) - module `keeper.push`.
- [reaper.md](reaper.md) - background database cleaning.
- [plugins.md](plugins.md) - CloudDriver and SshProvider.
- [cloud.md](cloud.md) — `keeper.cloud`.
- [rbac.md](rbac.md) — RBAC.
- [config.md](config.md) - format `keeper.yml`.
- [`../soul/`](../soul/README.md) - `soul`-binary (neighboring component).
- [architecture.md → Roles of binaries](../architecture.md) - a short summary of all three binaries.
- [architecture.md → Topology](../architecture.md) - Keeper's place in the overall picture.
