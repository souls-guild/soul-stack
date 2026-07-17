# Soul Stack

A configuration management system in the spirit of SaltStack, but with its own naming dictionary in a "soul" metaphor. The central server (**Keeper**) holds long-lived secured connections to agents on hosts (**Souls**) and brings each host to its declared desired state (**Destiny**).

> Status: closed small beta. Suitable for a handful of operators and a fleet of up to a few hundred hosts. What's **not** included in the beta — [docs/known-limitations.md](docs/known-limitations.md).

## Dictionary

If you're familiar with SaltStack, here's the term mapping:

| Soul Stack | SaltStack | Meaning |
|---|---|---|
| **Keeper** | master | Guardian, central node. |
| **Souls** | minions | Managed agents on hosts. |
| **Destiny** | states | Desired host state after a run. |
| **Soulprint** | grains | Facts about the host system (OS, kernel, CPU, network). |
| **Essence** | pillars | Parameters/values substituted into Destiny. |

Full naming dictionary — [docs/naming-rules.md](docs/naming-rules.md).

## Key properties

- **HA out of the box.** Keeper is a horizontally scalable stateless cluster on top of shared Postgres and Redis. Any instance serves any request; the failure of one does not bring down fleet management.
- **mTLS fleet identity.** Keeper↔Soul communication is a bidirectional gRPC stream over mTLS. Each Soul onboards via CSR (the private key never leaves the host) and receives a short-lived certificate (**SoulSeed**), which is rotated automatically.
- **Built-in RBAC.** Operator access (**Archon**) is via JWT and permission strings scoped by coven / service / incarnation. Default is deny.
- **Vault as the single secret store.** All secrets (Postgres DSN, JWT signing key, PKI for SoulSeed) live in Vault; secrets are never materialized on the Keeper cluster's disk.
- **OpenAPI + MCP.** The primary operator interface is REST (OpenAPI) and an MCP server for AI agents. The CLI is a thin wrapper, not the main path.
- **Observability out of the box.** Prometheus metrics and OpenTelemetry traces in every binary, built-in log rotation, config hot-reload.

## Architecture in one paragraph

Three binaries (ADR-004): `keeper` (central server — gRPC, OpenAPI, MCP, background Reaper), `soul` (agent daemon on the managed host), and `soul-lint` (offline artifact linter). The stream is always initiated by Soul — managed hosts have no open inbound ports. Cold cluster state lives in Postgres, the hot layer (presence, lease, leader election) lives in Redis. The required infrastructure tier is **Postgres + Redis + Vault** ([ADR-053](docs/adr/0053-dependency-tiers.md)).

## Where to start

- **[docs/getting-started.md](docs/getting-started.md)** — bring up a single-keeper instance plus infra, bootstrap the first Archon, onboard one Soul, and apply a simple scenario. ~30 minutes.
- **[docs/known-limitations.md](docs/known-limitations.md)** — what's not included in the beta.
- **[docs/operations/](docs/operations/README.md)** — operations runbook for a production install (deployment / HA / backup / disaster recovery).
- **[docs/README.md](docs/README.md)** — index of all documentation.
- **[docs/architecture.md](docs/architecture.md)** — architecture overview and links to ADRs (the source of truth for design).

## Security and feedback

Closed beta, best-effort support (no SLA).

- **Bugs and unexpected behavior** — [GitHub Issues](https://github.com/co-cy/soul-stack/issues) ("Bug report" template). Get the version from `keeper version`.
- **Security vulnerabilities** — private Security Advisory, **not** a public issue: [SECURITY.md](SECURITY.md).
- **Where to reach out and support level** — [SUPPORT.md](SUPPORT.md).

## License

[Apache License 2.0](LICENSE). Open core: enterprise features live in separate repositories under a separate commercial license, pulling this core in as a dependency ([ADR-016](docs/adr/0016-parity-license.md)).
