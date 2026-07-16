# Directory of core modules

Per-module directory of implemented Soul Stack core modules: canonical name,
supported states, parameters of each state, idempotency, side-effects,
example of the destiny task.

The directory documents **only** what is actually in the code
(`soul/internal/coremod/`, `keeper/internal/coremod/`). This is a guide to
behavior, not the normative source of design - design is responsible
[ADR-015](../adr/0015-core-modules-mvp.md) (Soul-side
core), [ADR-017](../adr/0017-keeper-side-core.md)
(Keeper-side core), [ADR-010](../adr/0010-templating.md)
(render `core.file.rendered`).

The directory covers **core modules** (`core.*`, built into the binary). Plugins -
individual namespace directories: [official/README.md](official/README.md) (`official.*`)
and [community/README.md](community/README.md) (`community.*`, incl.
[`community.redis`](community/redis/README.md) and
[`community.mongo`](community/mongo/README.md)).

Related documents (intentionally not duplicated here):

- [soul/modules.md](../soul/modules.md) - host side of the modules: where are they located,
how they are cached, cleanup; manifest custom modules and `spec.states`.
- [keeper/modules.md](../keeper/modules.md) - specification of Keeper-side core modules
(dispatcher `on: keeper`).
- [naming-rules.md → Destiny Modules](../naming-rules.md) - dictionary
names and a summary table of all core modules.

## Addressing

The destiny step is addressed as `core.<module>.<state>` - for example
`core.pkg.installed`, `core.file.rendered`. Top (`core.<module>`) —
module name in Registry; `<state>`-suffix comes to the module in `ApplyRequest.state`
and dispatched within the implementation. Verb-forms (`run`, `shell`, `probe`, `fetched`,
`extracted`) - the same mechanism, just without the declarative semantics "lead to
condition."

Soul-side / Keeper-side dispatcher - scenario-key `on:`
([scenario/orchestration.md §3](../scenario/orchestration.md#3-step-target---on)):
Soul-side core are used on hosts (`on:` omitted or coven tags), Keeper-side
core - `on: keeper` only.

## Soul-side core modules

Statically built into the `soul` binary. Apply the same in pull (daemon) and push
(oneshot).

| Module | States | Destination |
|---|---|---|
| [`core.pkg`](core/pkg/README.md) | `installed` / `absent` / `latest` | OS packages via native pkg-mgr (apt/dnf/yum/apk). |
| [`core.file`](core/file/README.md) | `present` / `absent` / `rendered` | File with literal-content / missing / rendered from `.tmpl`. |
| [`core.service`](core/service/README.md) | `running` / `stopped` / `restarted` / `enabled` | Service via systemd/openrc/sysv. |
| [`core.user`](core/user/README.md) | `present` / `absent` | Local OS users. |
| [`core.group`](core/group/README.md) | `present` / `absent` | OS local groups. |
| [`core.exec`](core/exec/README.md) | `run` (verb) | Arbitrary command via exec() (without shell). |
| [`core.cmd`](core/cmd/README.md) | `shell` (verb) | shell command (pipes, redirects). |
| [`core.cron`](core/cron/README.md) | `present` / `absent` | Cron tasks. |
| [`core.mount`](core/mount/README.md) | `present` / `absent` / `mounted` / `unmounted` | Mount points and /etc/fstab. |
| [`core.git`](core/git/README.md) | `cloned` / `pulled` | Cloning/updating a git repository on the host. |
| [`core.archive`](core/archive/README.md) | `extracted` | Unpacking archives (tar/tar.gz/tar.bz2/zip). |
| [`core.sysctl`](core/sysctl/README.md) | `present` / `applied` | Kernel parameters (`vm.*`, `kernel.*`): `present` - one key, `applied` - bulk set with one drop-in + reload. |
| [`core.url`](core/url/README.md) | `fetched` | Uploading a file via URL (only `https://`, idempotency via checksum). |
| [`core.line`](core/line/README.md) | `present` / `absent` | In-place line-by-line editing of a file (lineinfile equivalent). |
| [`core.repo`](core/repo/README.md) | `present` / `absent` | Batch repository (apt/dnf/yum/apk). |
| [`core.firewall`](core/firewall/README.md) | `present` / `absent` | One firewall rule (ufw/firewalld). |
| [`core.http`](core/http/README.md) | `probe` (verb) | Read-probe HTTP (health-check / readiness, `changed=false`). |
| [`core.augur`](core/augur/README.md) | `fetch` (verb) | Read-probe of live access to an external system (Vault/Prometheus/ELK) via the Augur broker ([ADR-025](../adr/0025-augur.md), `changed=false`). |

## Keeper-side core modules

Dispatcher `on: keeper` - executed on the Keeper side, not on the host. Specka -
[keeper/modules.md](../keeper/modules.md).

| Module | States | Destination |
|---|---|---|
| [`core.soul.registered`](core/soul/README.md) | `registered` | Linking SID to coven tags of the souls registry. |
| [`core.cloud.provisioned`](core/cloud/README.md) | `created` / `destroyed` | Cloud instance via CloudDriver plugin. |
| [`core.choir`](core/choir/README.md) | `present` / `absent` | Voice membership (SID) in the Choir incarnation (ADR-044). |
| [`core.vault`](core/vault/README.md) (author-addresses `core.vault.kv-read` / `core.vault.kv-present`) | `kv-read` (verb) / `kv-present` | `kv-read` — reading the secret from Vault KV (v1/v2, auto-detect) on the keeper side; `kv-present` — generate-if-absent (generate the missing secret using password-policy, [ADR-017 amend 2026-06-28](../adr/0017-keeper-side-core.md)). |

## core-beacon

Built-in **core-beacon** ([ADR-030](../adr/0030-vigil-oracle.md))
is the body of [Vigil](../naming-rules.md) (Soul-side
event-driven monitoring), and **NOT** apply-module: beacon **observes** state
host (read-only by design) and when it is changed, it raises Portent. They don't have
`states` and they do not bring the host into a state - that's why they are removed from the tables
core modules above. Addressed as `core.beacon.<name>` in field `VigilDef.check`.
Per-beacon reference - [`core/beacon/README.md`](core/beacon/README.md).

## Plugins (non-core)

In addition to the built-in `core.*`, destiny steps can address plugins via
SoulModule-contract ([ADR-020](../adr/0020-plugin-infrastructure.md),
gRPC-over-stdio). Each namespace has its own per-module directory:

| Namespace | Index | What is this |
|---|---|---|
| `official.*` | [official/README.md](official/README.md) | Soul Stack team plugins (`soul-mod-official-*`), companion repo `soul-stack-plugins`. |
| `community.*` | [community/README.md](community/README.md) | Third-party plugins (`soul-mod-community-*`). Implemented [`community.redis`](community/redis/README.md) - interface to live Redis (12 states) and [`community.mongo`](community/mongo/README.md) - interface to live MongoDB (3 states, PILOT standalone). |

## Catalog status

The catalog is complete. What we think (the source of truth is the registry in the code,
`soul/internal/coremod/registry.go` and `keeper/internal/coremod/registry.go`):

- **18 Soul-side core** - 17 by [ADR-015](../adr/0015-core-modules-mvp.md)
(12 original MVPs + post-MVP `url` / `line` / `repo` / `firewall` / `http`,
accepted based on real requests) + `augur` by [ADR-025](../adr/0025-augur.md)
(read-probe via Augur broker). Table "Soul-side core modules" above.
- **4 Keeper-side core** - `core.soul` / `core.cloud` / `core.vault` by
[ADR-017](../adr/0017-keeper-side-core.md)
  + `core.choir` by [ADR-044](../adr/0044-choir.md) (registered if available
`Deps.ChoirStore`). `core.vault` - one module with two states (`kv-read` +
`kv-present`, generate-if-absent by [ADR-017 amend 2026-06-28](../adr/0017-keeper-side-core.md)).
"Keeper-side core modules" table above.

Total **22 apply modules** (18 + 4). In `docs/module/core/` - **23 directories**: these
22 modules plus `core-beacon` (Vigil body, read-only observer - not apply module,
removed from tables, see "core-beacon" section).

Standards (pilot) - [`core/pkg/README.md`](core/pkg/README.md) and
[`core/file/README.md`](core/file/README.md). All links in the tables above lead to
existing documents.

## See also

- [ADR-015](../adr/0015-core-modules-mvp.md) - exact list of Soul-side core MVPs.
- [ADR-017](../adr/0017-keeper-side-core.md) - Keeper-side core extensions.
- [ADR-010](../adr/0010-templating.md) - template engine and `core.file.rendered`.
- [naming-rules.md → Destiny Modules](../naming-rules.md) - a dictionary of names.
