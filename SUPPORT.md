# Support

Soul Stack is in **closed small beta**. This document explains where beta
participants should go with questions and issues.

## Support level

**Best-effort, no SLA.** We respond as we're able; there's no guaranteed
response time during the closed beta. The goal of the beta is to gather
feedback and catch bugs before the stable release, not to provide production
support.

## Where to go

| What you have                                | Where                                                                 |
|-----------------------------------------------|------------------------------------------------------------------------|
| Bug, unexpected behavior, failed run          | **GitHub Issues** in this repository ("Bug report" template)          |
| Idea / feature request                        | **GitHub Issues** (describe it in free text)                          |
| Security vulnerability                        | **GitHub Security Advisory** — see [SECURITY.md](SECURITY.md). NOT a public issue. |

Before filing a bug, check [docs/known-limitations.md](docs/known-limitations.md):
some limitations are intentionally **out of scope** for the beta.

## Before filing an issue

- Check the version: `keeper version`. Does it reproduce on the current `v0.1.0-beta.x`?
- Attach reproduction steps and a relevant log/audit excerpt.
- **Do not paste secrets** (JWTs, Vault contents, private keys, DSNs with
  passwords) — mask anything sensitive.

## Where to start

- [docs/getting-started.md](docs/getting-started.md) — bring up a cluster and apply your first scenario.
- [docs/README.md](docs/README.md) — index of all documentation.
- [docs/operations/](docs/operations/README.md) — operations runbook.
