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

The directory covers **core modules** (`core.*`, built into the binary). Plugins live in their
own directories beside it: [`redis`](redis/README.md) — re-laid-out onto
`redis.<object>.<action>` by NIM-766, so it no longer sits under an origin-grouping directory —
plus [official/README.md](official/README.md) and [community/README.md](community/README.md)
(incl. [`community.mongo`](community/mongo/README.md) ⚠ **LEAVING THE DICTIONARY (NIM-769, not
implemented — ships today)**).

> ⚠ **`official` and `community` are no longer namespaces.** The origin-grouping level of a plugin
> address is **removed** ([ADR-020 amendment 2026-09-02](../adr/0020-plugin-infrastructure.md#amendment-2026-09-02-nim-764--nim-765-a-plugin-address-is-pluginobjectaction-and-the-origin-grouping-level-is-removed),
> NIM-765): a plugin step is addressed `<plugin-name>.<object>.<action>`, and origin is answered by
> the catalog entry's `source` plus the Sigil allow-list instead. `core.*` is untouched — it is
> [reserved](../naming-rules.md#reserved-namespace-names) and real. The two directories keep their
> present paths and names; they group *documents*, not addresses.

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
and dispatched within the implementation. Verb-forms (`run`, `shell`, `probe`, `request`, `fetched`,
`extracted`) - the same mechanism, just without the declarative semantics "lead to
condition."

Soul-side / Keeper-side dispatcher - scenario-key `on:`
([scenario/orchestration.md §3](../scenario/orchestration.md#3-step-target---on)):
Soul-side core are used on hosts (`on:` omitted or coven tags), Keeper-side
core - `on: keeper` only.

The Soul-side / Keeper-side split of this catalog is exactly what the routing
derivation of [ADR-0087](../adr/0087-task-side-derived-from-module-address.md)
reads: the two registries below share no base name, so the module address alone
decides the side and `on: keeper` on a core address becomes redundant. That ADR is
accepted and **not implemented**, so the dispatcher described above is still the
one that ships.

## Soul-side core modules

Statically built into the `soul` binary. Apply the same in pull (daemon) and push
(oneshot).

| Module | States | Destination |
|---|---|---|
| [`core.pkg`](core/pkg/README.md) | `installed` / `absent` / `latest` | OS packages via native pkg-mgr (apt/dnf/yum/apk). |
| [`core.file`](core/file/README.md) | `present` / `absent` / `rendered` | File with literal-content / missing / rendered from `.tmpl`. |
| [`core.directory`](core/directory/README.md) | `present` / `absent` | Directory exists with owner/group/mode (`parents` = `mkdir -p`) / removed (a non-empty one only with `recursive: true`). Split out of the former `core.file.directory`. |
| [`core.service`](core/service/README.md) | `running` / `stopped` / `restarted` / `enabled` / `disabled` / `masked` | Service via systemd/openrc/sysv (`masked` is systemd-only). |
| [`core.user`](core/user/README.md) | `present` / `absent` | Local OS users. |
| [`core.group`](core/group/README.md) | `present` / `absent` | OS local groups. |
| [`core.exec`](core/exec/README.md) | `run` (verb) | Arbitrary command via exec() (without shell). Success = the `exit_codes` set, default `[0]`. |
| [`core.cmd`](core/cmd/README.md) | `shell` (verb) | shell command (pipes, redirects). The same `exit_codes`, default `[0]`. |
| [`core.cron`](core/cron/README.md) | `present` / `absent` | Cron tasks. |
| [`core.mount`](core/mount/README.md) | `present` / `absent` / `mounted` / `unmounted` | Mount points and /etc/fstab. |
| [`core.git`](core/git/README.md) | `cloned` / `pulled` | Cloning/updating a git repository on the host. |
| [`core.archive`](core/archive/README.md) | `extracted` | Unpacking archives (tar/tar.gz/tar.bz2/zip). |
| [`core.sysctl`](core/sysctl/README.md) | `present` / `applied` | Kernel parameters (`vm.*`, `kernel.*`): `present` - one key, `applied` - bulk set with one drop-in + reload. |
| [`core.url`](core/url/README.md) | `fetched` | Uploading a file via URL (only `https://`, idempotency via checksum). |
| [`core.line`](core/line/README.md) | `present` / `absent` | In-place line-by-line editing of a file (lineinfile equivalent). |
| [`core.repo`](core/repo/README.md) | `present` / `absent` | Batch repository (apt/dnf/yum/apk). |
| [`core.firewall`](core/firewall/README.md) | `present` / `absent` | One firewall rule (ufw/firewalld). |
| [`core.http`](core/http/README.md) | `probe` / `request` (verbs) | Read-only GET/HEAD probe (`changed=false`) and explicit POST/PUT/PATCH/DELETE API mutation (`changed=true` on expected status). |
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
gRPC-over-stdio). A plugin step is addressed `<plugin-name>.<object>.<action>`
([address rule](../naming-rules.md#the-discipline-binding-the-three-levels)) — level 1 is the
alias the operator chose at registration, and there is **no origin-grouping level**. The two
directories below group documents by where a plugin came from; that grouping does **not** appear
in an address:

| Directory | Index | What is this |
|---|---|---|
| `official/` | [official/README.md](official/README.md) | Soul Stack team plugins (`soul-mod-official-*`), companion repo `soul-stack-plugins`. Their `official.*` addresses are the old form; no follow-up ticket, the artifacts are not in this repo. |
| `redis/` | [redis/README.md](redis/README.md) | The `redis` plugin — interface to live Redis, seven objects / nineteen actions (`soul-mod-redis`). It is NOT under `community/` since NIM-766: with the origin-grouping level gone from the address, the document sits under the plugin's own name. The `user` object (`ACL SETUSER`/`DELUSER` on one user) was added by NIM-767. |
| `community/` | [community/README.md](community/README.md) | Third-party plugins still on the old grouping. Implemented [`community.mongo`](community/mongo/README.md) ⚠ **LEAVING THE DICTIONARY (NIM-769, not implemented — ships today)** - interface to live MongoDB (3 states, PILOT standalone). |

## Catalog status

The catalog is complete. What we think (the source of truth is the registry in the code,
`soul/internal/coremod/registry.go` and `keeper/internal/coremod/registry.go`):

- **19 Soul-side core** - 18 by [ADR-015](../adr/0015-core-modules-mvp.md)
(12 original MVPs + post-MVP `url` / `line` / `repo` / `firewall` / `http`,
accepted based on real requests, + `directory` split out of `core.file` by
[Amendment 2026-07-17](../adr/0015-core-modules-mvp.md)) + `augur` by
[ADR-025](../adr/0025-augur.md) (read-probe via Augur broker). Table "Soul-side
core modules" above.
- **5 Keeper-side core** - `core.soul` / `core.cloud` / `core.vault` by
[ADR-017](../adr/0017-keeper-side-core.md)
  + `core.choir` by [ADR-044](../adr/0044-choir.md) (registered if available
`Deps.ChoirStore`). `core.vault` - one module with two states (`kv-read` +
`kv-present`, generate-if-absent by [ADR-017 amend 2026-06-28](../adr/0017-keeper-side-core.md)). `core.state` (`core.state.set` / `.present` / `.add` / `.append` / `.modify` / `.remove` / `.unset` - one state per [ADR-057](../adr/0057-state-changes-crud-verbs.md) verb, the write point of a service state field; registered when `Deps.Vault` is present. [ADR-0083](../adr/0083-declared-secret-state-fields.md) §4 for the declared-secret rule, [ADR-0084](../adr/0084-explicit-state-capture.md) for the verbs and the step-time capture).
"Keeper-side core modules" table above.

Total **23 apply modules** (19 + 4). In `docs/module/core/` - **24 directories**: these
23 modules plus `core-beacon` (Vigil body, read-only observer - not apply module,
removed from tables, see "core-beacon" section).

Standards (pilot) - [`core/pkg/README.md`](core/pkg/README.md) and
[`core/file/README.md`](core/file/README.md). All links in the tables above lead to
existing documents.

## See also

- [ADR-015](../adr/0015-core-modules-mvp.md) - exact list of Soul-side core MVPs.
- [ADR-017](../adr/0017-keeper-side-core.md) - Keeper-side core extensions.
- [ADR-010](../adr/0010-templating.md) - template engine and `core.file.rendered`.
- [naming-rules.md → Destiny Modules](../naming-rules.md) - a dictionary of names.
