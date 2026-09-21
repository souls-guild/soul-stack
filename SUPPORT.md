# Support

Soul Stack is in **closed small beta**. This document explains where beta
participants should go with questions and issues.

## Support level

**Best-effort, no SLA.** We respond as we're able; there's no guaranteed
response time during the public beta. The goal of the beta is to gather
feedback and catch bugs before the stable release, not to provide production
support.

## Where to go

| What you have                                | Where                                                                 |
|-----------------------------------------------|------------------------------------------------------------------------|
| Bug, unexpected behavior, failed run          | **GitHub Issues** in this repository ("Bug report" template)          |
| Idea / feature request                        | **GitHub Issues** (describe it in free text)                          |
| Question, "how do I…", design discussion      | **GitHub Discussions** in this repository                             |
| Quick question, discussion, community chat    | **Discord** — https://discord.gg/cMwMW2UTyE                           |
| Security vulnerability                        | **GitHub Security Advisory** — see [SECURITY.md](SECURITY.md). NOT a public issue. |

Real-time community chat is on our [Discord](https://discord.gg/cMwMW2UTyE).

Before filing a bug, check [docs/known-limitations.md](docs/known-limitations.md):
some limitations are intentionally **out of scope** for the beta.

## Commercial support and services

Soul Stack ships complete: the web UI, LDAP/OIDC login, RBAC, audit trail, OpenAPI,
the MCP server, and automatic certificate rotation are all part of the open-source
core. What we offer commercially is help, not features — our time to get you to
production faster and shape Soul Stack around your environment:

- **Priority feature work** — need a module, cloud driver, or capability ahead of the
  community roadmap? We can build it and upstream it into the core.
- **Integration help** — wiring Soul Stack into what you already run: your Vault, your
  identity provider, your CI, and your existing fleet.
- **Consulting and rollout** — architecture review, migration from your current
  configuration-management tooling, and hands-on help bringing your first fleet live.

Reach out through [soul-stack.com](https://soul-stack.com) or email [licensing@soul-stack.com](mailto:licensing@soul-stack.com).

## Before filing an issue

- Check the version: `keeper version`. Does it reproduce on the current `v0.1.0-beta.x`?
- Attach reproduction steps and a relevant log/audit excerpt.
- **Do not paste secrets** (JWTs, Vault contents, private keys, DSNs with
  passwords) — mask anything sensitive.

## Where to start

- [docs/getting-started.md](docs/getting-started.md) — bring up a cluster and apply your first scenario.
- [docs/README.md](docs/README.md) — index of all documentation.
- [docs/operations/](docs/operations/README.md) — operations runbook.
- [soul-stack.com](https://soul-stack.com) — project site and online documentation.
