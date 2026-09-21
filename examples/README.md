# Soul Stack examples

Concrete illustrations of what Soul Stack artifacts look like in reality, for having "here's what this looks like in practice" on hand while reading the architecture (see [docs/architecture.md](../docs/architecture.md)).

**These are working artifacts, not paper.** The header here used to say "not working code", which stopped being true long before NIM-871 removed it: `service/*` and `destiny/*` are linted by `make lint`, rendered by `make trial` over their own `tests/<case>/case.yml`, stamped by `make stamp-examples` where they carry a migration ladder, and `service/hello-world` is materialized into a git repo and seeded into the stand's `service_registry` by `dev/provision.sh`. Treat an edit here as an edit to code that the gate runs.

The one thing they are deliberately NOT is a place to keep a product service. `service/redis` — a full service repo with twelve scenarios, a fifteen-rung ladder and the L3a/L3b acceptance suites built on it — was removed by NIM-871 and now lives out of tree: a service is its own repository, and the engine bundling one made the engine's gate depend on a service's lifecycle. What stays below is the smallest artifact that demonstrates a shape.

## Contents

| Folder | What |
|---|---|
| [`destiny/redis/`](destiny/redis/) | An atomic destiny brick for "how to install and configure Redis on a host." A separate git repo in real life. Includes `tasks/main.yml` (top-level task list without a wrapper) and [`tests/install-and-ping/case.yml`](destiny/redis/tests/install-and-ping/case.yml) — an illustration of the molecule-style destiny test format. Full format breakdown — in [docs/destiny/](../docs/destiny/README.md). Kept when the redis *service* left: a destiny is an independent artifact, and the out-of-tree service consumes this one. |
| [`service/dragonfly/`](service/dragonfly/) | The fullest service repo in the tree: `service.yml`, a `vars/` layer stack, six scenarios, a migration ladder with `schema.lock`, and `tests/<case>/case.yml` per scenario. The reference for what a service looks like. |
| [`service/hello-world/`](service/hello-world/) | The smallest one that actually runs — the service the dev stand registers and `make dev-smoke` exercises. |
| [`module/redis/`](module/redis/) | A real SoulModule artifact (Go, its own `go.mod`), named after the module it serves. Built and stamped into the dev stand by `dev/provision.sh`. |
| [`keeper/keeper.yml`](keeper/keeper.yml) | Config for the central `keeper` instance (HA-clustered, on top of Postgres+Redis+Vault). |
| [`soul/soul.yml`](soul/soul.yml) | Config for the `soul` agent on a managed host: fallback endpoint list, retry, failback. |
| [`module/redis-failover/`](module/redis-failover/) | A skeleton of a custom module for Destiny: schema document and interface (no full implementation). Since NIM-377 an artifact publishes a generated [`schema.json`](module/redis-failover/schema.json) instead of a hand-written `manifest.yaml`, and it declares only its modules — the `acme` in `acme.redis-failover.promoted` is the alias an operator registers it under and appears nowhere in the document. ⚠ **LEAVING THE DICTIONARY (NIM-797):** level 2 there is `redis-failover`, the plugin's own subject, where the [address rule](../docs/naming-rules.md#the-discipline-binding-the-three-levels) puts the **object** a module manages. Marked rather than rewritten — its object would be `replica`, which [`redis`](module/redis/) already serves, so whether this skeleton survives is what NIM-797 decides. |

## Behavior of the examples

- **YAML without secrets.** All passwords/tokens are `vault:secret/...` references.
- **Hostnames** — `*.example`, to avoid confusion with real hosts.
- **Versions** — git tags in `ref:` are fictitious, illustrative (see [ADR-007](../docs/adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field) — there is no `version:` field in manifests).
- If something in the architecture changes, the examples are updated alongside it.
