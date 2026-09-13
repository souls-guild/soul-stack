# Soul Stack examples

Concrete illustrations of what Soul Stack artifacts look like in reality. These examples are **not working code**, but samples of file structure and YAML formats for the current architecture (see [docs/architecture.md](../docs/architecture.md)).

The goal is to have "here's what this looks like in practice" on hand while reading the architecture.

## Contents

| Folder | What |
|---|---|
| [`destiny/redis/`](destiny/redis/) | An atomic destiny brick for "how to install and configure Redis on a host." A separate git repo in real life. Includes `tasks/main.yml` (top-level task list without a wrapper) and [`tests/install-and-ping/case.yml`](destiny/redis/tests/install-and-ping/case.yml) — an illustration of the molecule-style destiny test format. Full format breakdown — in [docs/destiny/](../docs/destiny/README.md). |
| [`service/redis/`](service/redis/) | A full example of a service repo: `service.yml`, a `vars/` layer stack, a set of scenarios, migrations, tests. |
| [`keeper/keeper.yml`](keeper/keeper.yml) | Config for the central `keeper` instance (HA-clustered, on top of Postgres+Redis+Vault). |
| [`soul/soul.yml`](soul/soul.yml) | Config for the `soul` agent on a managed host: fallback endpoint list, retry, failback. |
| [`module/redis-failover/`](module/redis-failover/) | A skeleton of a custom module for Destiny: schema document and interface (no full implementation). Since NIM-377 an artifact publishes a generated [`schema.json`](module/redis-failover/schema.json) instead of a hand-written `manifest.yaml`, and it declares only its modules — the `acme` in `acme.redis-failover.promoted` is the alias an operator registers it under and appears nowhere in the document. ⚠ **LEAVING THE DICTIONARY (NIM-797):** level 2 there is `redis-failover`, the plugin's own subject, where the [address rule](../docs/naming-rules.md#the-discipline-binding-the-three-levels) puts the **object** a module manages. Marked rather than rewritten — its object would be `replica`, which [`redis`](module/redis/) already serves, so whether this skeleton survives is what NIM-797 decides. |

## Behavior of the examples

- **YAML without secrets.** All passwords/tokens are `vault:secret/...` references.
- **Hostnames** — `*.example`, to avoid confusion with real hosts.
- **Versions** — git tags in `ref:` are fictitious, illustrative (see [ADR-007](../docs/adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field) — there is no `version:` field in manifests).
- If something in the architecture changes, the examples are updated alongside it.
