# Soul Stack

**English** · [Русский](docs/i18n/README.ru.md)

**Soulful configuration management for the next generation** — a complete platform
for bringing your fleet to its desired state, not a pile of scripts. Describe the
desired state once; a central **Keeper** holds long-lived, mTLS-secured connections to
a `soul` agent on every host (your **Souls**) and brings each one to its declared
**Destiny** — in pull and in push. Identity, access control, audit, an operator web
console, a REST/MCP API and secret management aren't add-ons you bolt on later — they
ship in the core.

[![License: BSL 1.1](https://img.shields.io/badge/license-BSL%201.1%20%E2%86%92%20Apache%202.0-blue)](LICENSE)
[![Status: public beta](https://img.shields.io/badge/status-public%20beta-orange)](docs/known-limitations.md)

> **Public beta.** No stability or SLA guarantees yet. APIs, schemas, on-disk
> formats and the wire protocol may change between beta releases. What's **not**
> in scope for the beta — [docs/known-limitations.md](docs/known-limitations.md).

## Everything in one platform

Soul Stack is meant to be complete out of the box — the things you usually bolt on
later are part of the core:

- **Web UI** — a full operator console, not an afterthought on top of the CLI.
- **Identity: LDAP / OIDC** — plug in your directory or SSO provider directly.
- **RBAC** — fine-grained, deny-by-default permissions scoped by coven / service /
  incarnation.
- **Audit trail** — every operator action is recorded.
- **REST API (OpenAPI) + MCP** — the API is the primary interface; the CLI is a thin
  wrapper, and there's a built-in MCP server so AI agents can drive it.
- **Certificate rotation** — each Soul's mTLS identity (**SoulSeed**) is issued and
  rotated automatically; nothing to cron by hand.
- **Observability** — Prometheus metrics and OpenTelemetry traces in every binary,
  plus built-in log rotation and config hot-reload.
- **… and more** — the list above is the baseline the core ships with today, not the
  ceiling.

None of this is feature-gated or paywalled — the product is never crippled to sell
tiers. During the beta the license grants **internal use** (running Soul Stack to
manage your own or your
organization's infrastructure); using it beyond that — for example, offering it to
third parties as a hosted or managed service — needs a commercial license (see
[License](#license) below).

## The vocabulary

The "soul" metaphor runs through the whole system — a short glossary:

| Term | What it is |
|---|---|
| **Keeper** | The control plane — the guardian that holds the fleet together. |
| **Souls** | The managed agents, one per host. |
| **Destiny** | The state a host is brought to on each run. |
| **Soulprint** | Facts about a host — OS, kernel, CPU, network. |
| **Essence** | The parameters and values substituted into a Destiny. |

Full dictionary of names — [docs/naming-rules.md](docs/naming-rules.md).

## Key properties

- **HA out of the box.** Keeper is a horizontally scalable stateless cluster on top
  of shared Postgres and Redis. Any instance serves any request; losing one instance
  does not interrupt managing your Souls.
- **mTLS Soul identity.** Keeper↔Soul is a bidirectional gRPC stream over mTLS. Each
  Soul onboards via CSR (the private key never leaves the host) and receives a
  short-lived certificate (**SoulSeed**), rotated automatically. Managed hosts keep
  no open inbound ports — the stream is always dialed by the Soul.
- **Vault as the single secret store.** All secrets (Postgres DSN, JWT signing key,
  PKI for SoulSeed) live in Vault; they're never materialized on the Keeper cluster's
  disk.
- **Deny by default.** Operator access (**Archon**) is JWT + permission strings; the
  default answer is no.

## Architecture in one paragraph

Three binaries ([ADR-004](docs/architecture.md)): `keeper` (central server — gRPC,
OpenAPI, MCP, background Reaper), `soul` (agent daemon on the managed host), and
`soul-lint` (offline artifact linter). The stream is always initiated by the Soul, so
managed hosts have no open inbound ports. Cold cluster state lives in Postgres; the
hot layer (presence, lease, leader election) lives in Redis. The required
infrastructure tier is **Postgres + Redis + Vault**
([ADR-053](docs/adr/0053-dependency-tiers.md)).

## Where to start

- **[docs/getting-started.md](docs/getting-started.md)** — bring up a single Keeper
  plus infra (Postgres + Redis + Vault via `dev/docker-compose.yml`), bootstrap the
  first Archon, onboard one Soul, and apply a simple scenario. ~30 minutes.
- **[docs/known-limitations.md](docs/known-limitations.md)** — what's out of scope in
  the beta.
- **[docs/operations/](docs/operations/README.md)** — the operations runbook for a
  production install (deployment / HA / backup / disaster recovery).
- **[docs/architecture.md](docs/architecture.md)** — architecture overview and links
  to the ADRs (the source of truth for design decisions).
- **[docs/README.md](docs/README.md)** — index of all documentation.

Ready-to-read Destiny and service examples ("batteries included") live under
[`examples/destiny/`](examples/destiny/) and [`examples/service/`](examples/service/):
redis, node-exporter, vector, and more — real layouts you can copy from.

## AI assistants

PRs from AI coding assistants are welcome — we're open to any path that improves Soul
Stack. But AI can be wrong: whatever tool wrote a change, **you** are responsible for
checking that it's correct before you send it. Two asks:

- **Verify what you send.** Re-read and test the changes before opening a PR; don't
  submit output you haven't checked. A green `make check` and a clear description of
  *why* the change is correct go a long way.
- **Respect the design process.** Architectural decisions live in the
  [ADRs](docs/adr/README.md); a change that touches design should reference or update
  the relevant ADR rather than working around it.

The same contribution rules apply to humans and assistants alike — see
[CONTRIBUTING.md](CONTRIBUTING.md).

## Contributing

Issues and pull requests are open. Start with
[CONTRIBUTING.md](CONTRIBUTING.md) — it covers the dev setup, the `make check` gate,
the Contributor License Agreement (signed once, on your first PR), and the coding
conventions. By participating you agree to the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Security and support

- **Security vulnerabilities** — report privately, **not** as a public issue:
  [SECURITY.md](SECURITY.md) (`security@soul-stack.com` or a GitHub private advisory).
- **Bugs and unexpected behavior** — [GitHub Issues](https://github.com/souls-guild/soul-stack/issues)
  ("Bug report" template). Include the output of `keeper version`.
- **Questions and where to reach out** — [SUPPORT.md](SUPPORT.md). Beta support is
  best-effort, no SLA.

## License

Core (this repository) — **[Business Source License 1.1](LICENSE)** (fair-code): the
source is open, and **production use is granted for internal use** — managing your own
or your organization's infrastructure, including commercially. Other production use —
offering Soul Stack to third parties as a hosted or managed service or product,
white-labeling, or embedding it — requires a commercial license. Each version
automatically becomes **[Apache 2.0](https://www.apache.org/licenses/LICENSE-2.0)** two
years after its release (the Change Date), so the restriction is temporary, not
permanent. The SDK, `examples/`, and plugins are Apache 2.0.

Plain-language explanation of what you can and can't do —
[LICENSING.md](LICENSING.md). The "Soul Stack" name and logo are covered by
[trademark](TRADEMARK.md), separately from the code license.

## Links

- **Website:** https://soul-stack.com (overview, guides, hosted docs)
- **Documentation:** [docs/](docs/README.md)
