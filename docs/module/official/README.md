# Official plugins catalog

`official.*` is the namespace for plugins that are supplied and maintained by the Soul Stack team, as opposed to core modules (`core.*`, statically built into the `soul` binary) and community plugins (`community.*`, third-party).

Binari - `soul-mod-official-<name>`. They live in the companion repo [`soul-stack-plugins/`](https://github.com/souls-guild/soul-stack-plugins) (Apache 2.0, [ADR-016](../../adr/0016-parity-license.md)).

## Pilot (SDK-2, 2026-05-27)

The first 3 modules are pattern-fixture for the SDK-3 edition.

| Module | States | Destination |
|---|---|---|
| [`official.postgres-user`](postgres-user/README.md) | `present` / `absent` | PostgreSQL ROLE (CREATE/ALTER/DROP) via `pgx/v5` + probe `pg_roles`. |
| [`official.nginx-vhost`](nginx-vhost/README.md) | `present` / `absent` | nginx vhost-config: render + `nginx -t` validate + write + symlink + reload. |
| [`official.docker-container`](docker-container/README.md) | `running` / `stopped` / `absent` | docker container via docker-CLI + drift-detect (image/env/ports/volumes/networks). |

Each pilot implements Plan + `PlanReadSafe` ([ADR-031 Scry](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) - drift-detect on `dry_run: true` without host mutation.

## Full list (planned SDK-3 edition)

See [ADR-016 amendment SDK-2 (2026-05-27)](../../adr/0016-parity-license.md).

## Test coverage

Pattern-fixture (fixed SDK-2 for SDK-3 edition):

- **L0** - in-memory fake-runner + fake stream via `grpc.ServerStreamingServer[ApplyEvent]`. Hermetic, no network/disk/docker.
- **L1** (build-tag `integration`) - testcontainers or real-daemon. Full lifecycle.
- **L3b** (build-tag `live`) - privileged Debian-12 + real Soul-host + plugin. On SDK-2 - skeleton `t.Skip`, waiting for Vigil-extension L3b-harness in core-repo.

## Soul-third-party use

```yaml
# destiny/destiny.yml
- module: official.postgres-user
  state: present
  params:
    dsn: "${ vault('secret/keeper/pg-admin#dsn') }"
    username: appuser
    password: "${ vault('secret/keeper/app-credentials#pg_password') }"
    createdb: true
```

Keeper-side vault-resolve ([ADR-010](../../adr/0010-templating.md)) substitutes real values BEFORE `ApplyRequest` - the plugin sees the resolved one.

## See also

- [ADR-016](../../adr/0016-parity-license.md) - parity + open core / freemium strategy.
- [ADR-020](../../adr/0020-plugin-infrastructure.md) - plugin infrastructure (manifest/handshake/lifecycle).
- [ADR-026](../../adr/0026-sigil.md) — Sigil (integrity of official binaries under the signature of the Archon).
- [ADR-031](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile) — Scry (read-safe Plan marker, default-deny on `dry_run`).
- [Plugin author guide](https://github.com/souls-guild/soul-stack-plugins/blob/main/docs/module-author-guide.md).
