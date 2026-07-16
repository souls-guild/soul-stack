# Soul — index

Documentation for the `soul` binary — the agent daemon on a managed host. The same `soul` runs in pull mode (as a daemon) and in push mode (as a one-shot command from Keeper); hence the unified set of documents.

## Where to start

| Document | About |
|---|---|
| [concept.md](concept.md) | What the `soul` binary is, its place in the overall picture, the two transports (`agent` / `ssh`), the principles (no outbound traffic beyond Keeper, one binary for both modes, one `souls` record in the DB). |
| [identity.md](identity.md) | Soul identity: SID = FQDN, SoulSeed (mTLS), the `souls` / `soul_seeds` / `bootstrap_tokens` registries, statuses and transitions, SoulSeed rotation, revoke, host rename. |
| [onboarding.md](onboarding.md) | Bootstrap-token lifecycle (issue → delivery → CSR → burn), details on the Keeper and Soul sides, operator recommendations, ways to deliver the token to the host, protections. |
| [connection.md](connection.md) | Connecting to the Keeper cluster: the `priority + failback` algorithm, the YAML config of the `keeper:` block, parameters (`retry`, `failback.interval`, `failback.spray`), guarantees. |
| [config.md](config.md) | The `soul.yml` format: `sid`, `paths`, `keeper:`, `soulprint:`, `cleanup:`, `logging:`, `metrics:`, `otel:`, `tls`. Config layout on the Soul host side. |
| [modules.md](modules.md) | The cache of core and custom modules on the host: the `/var/lib/soul-stack/{bin,modules}/` layout, the `soul-mod-<name>-<sha>` naming scheme, behavior in pull and push, local cleanup. |
| [soulprint.md](soulprint.md) | The Soulprint typed schema MVP ([ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)): the `SoulprintFacts` fields (os/kernel/cpu/memory/network/hostname/sid), the `family→pkg_mgr/init_system` mapping table, the canonical CEL accessors `soulprint.self.<path>`, the Soulprint↔`souls`-registry boundary (covens — a Keeper-side projection), `collected_at` vs `received_at`. |

## Related documents

- [`docs/architecture.md`](../architecture.md) — the layers above and below Soul:
  - [Soul lifecycle and the soul registry](../architecture.md#жизненный-цикл-soul-и-реестр-душ) — registries and statuses.
  - [Soul connection: priority and failback](../architecture.md#подключение-soul-priority-и-failback) — the algorithm specification.
  - [Push mode (`keeper.push`)](../architecture.md#push-режим-keeperpush) — the second transport.
  - [Module model](../architecture.md#модель-модулей) — what Soul executes.
  - [Delivering the SoulSeed token to the host](../architecture.md#доставка-soulseed-токена-на-хост) — ways to deliver the token.
- [`docs/naming-rules.md`](../naming-rules.md) — the vocabulary of names (SID, KID, SoulSeed, Coven, Reaper).
- [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — a working example of a Soul-host config.
