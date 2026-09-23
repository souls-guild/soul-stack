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
own directories beside it: [`redis`](redis/README.md) and [`mongo`](mongo/README.md) — both
re-laid-out onto `<plugin>.<object>.<action>` (NIM-766, NIM-769), so neither sits under an
origin-grouping directory any more — plus [official/README.md](official/README.md).

> ⚠ **`official` and `community` are no longer namespaces.** The origin-grouping level of a plugin
> address is **removed** ([ADR-020 amendment 2026-09-02](../adr/0020-plugin-infrastructure.md#amendment-2026-09-02-nim-764--nim-765-a-plugin-address-is-pluginobjectaction-and-the-origin-grouping-level-is-removed),
> NIM-765): a plugin step is addressed `<plugin-name>.<object>.<action>`, and origin is answered by
> the catalog entry's `source` plus the Sigil allow-list instead. `core.*` is untouched — it is
> [reserved](../naming-rules.md#reserved-namespace-names) and real. `community/` was the last
> directory named after an origin and is **gone** (NIM-769 moved its only member out);
> `official/` keeps its path and name because it groups *documents*, not addresses.

Related documents (intentionally not duplicated here):

- [soul/modules.md](../soul/modules.md) - host side of the modules: where are they located,
how they are cached, cleanup; manifest custom modules and `spec.states`.
- [keeper/modules.md](../keeper/modules.md) - specification of Keeper-side core modules
(routed by their address; a task does not carry `on:`).
- [naming-rules.md → Destiny Modules](../naming-rules.md) - dictionary
names and a summary table of all core modules.

## Addressing

The destiny step is addressed as `core.<module>.<state>` - for example
`core.pkg.installed`, `core.file.rendered`. Top (`core.<module>`) —
module name in Registry; `<state>`-suffix comes to the module in `ApplyRequest.state`
and dispatched within the implementation. Verb-forms (`run`, `shell`, `probe`, `request`, `fetched`,
`extracted`) - the same mechanism, just without the declarative semantics "lead to
condition."

The Soul-side / Keeper-side split of this catalog **is** the routing rule
([ADR-0087](../adr/0087-task-side-derived-from-module-address.md), shipped in
NIM-747/NIM-749): the two registries below share no base name, so the module address
alone decides the side. `on:` is back to its one meaning — which covens — and the
linter refuses `on: keeper` on a core address as redundant
([scenario/orchestration.md §3](../scenario/orchestration.md#3-step-target---on)).

★ The derivation reads a **catalog**, not a naming pattern, so an address the catalog
does not know is routed **Soul-side** — that is the honest answer for a plugin, whose
side is declared in its own schema document. A core address that has been *removed*
therefore does not error; it silently changes side. `on: keeper` stays as the only
spelling a keeper-side **plugin** address has.

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
| [`core.noop`](core/noop/README.md) | `run` (verb) | No-op barrier anchor ([ADR-015](../adr/0015-core-modules-mvp.md), `changed=false`) — a task to hang `require:` / `onchanges:` on. |
| `core.module` (author-address `core.module.installed`) | `installed` | SoulModule plugin delivery to the host: allow-check → `FetchModule` → Sigil-verify → atomic install ([ADR-065](../adr/0065-core-module-installed.md), host side in [soul/modules.md](../soul/modules.md)). |

## Keeper-side core modules

Executed on the Keeper side, not on the host — and said by the ADDRESS, not by a key:
`on: keeper` on any row below is `on_keeper_redundant`. Spec -
[keeper/modules.md](../keeper/modules.md).

Conditional registration is the norm here: a keeper-side module whose dependency is
absent is not registered at all, and a step addressing it fails `unknown keeper-side
module` rather than running degraded (`keeper/internal/coremod.Default`).

| Module | States | Destination |
|---|---|---|
| [`core.soul.registered`](core/soul/README.md) | `registered` | Linking SID to coven tags of the souls registry. |
| [`core.choir`](core/choir/README.md) | `present` / `absent` | Voice membership (SID) in the Choir incarnation (ADR-044). |
| [`core.vault`](core/vault/README.md) (author-addresses `core.vault.kv-read` / `core.vault.kv-present`) | `kv-read` (verb) / `kv-present` | `kv-read` — reading the secret from Vault KV (v1/v2, auto-detect) on the keeper side; `kv-present` — generate-if-absent (generate the missing secret using password-policy, [ADR-017 amend 2026-06-28](../adr/0017-keeper-side-core.md)). |
| [`core.bootstrap.issued`](core/bootstrap/README.md) | `issued` | One-time bootstrap capabilities for a list of ready-made VM FQDN/SIDs ([ADR-063](../adr/0063-bootstrap-token-delivery.md); `delivered` was removed by NIM-834). |
| [`core.ssh.run`](core/ssh/README.md) | `run` (verb) | Agentless command transport — the only way to execute anything on a host that has no Soul yet (NIM-849, [keeper/modules.md](../keeper/modules.md#coresshrun)). |
| `core.state` (author-addresses `core.state.set` / `.present` / `.add` / `.append` / `.modify` / `.remove` / `.unset`) | one state per [ADR-057](../adr/0057-state-changes-crud-verbs.md) verb | The write point of a service state field, captured at step time ([ADR-0084](../adr/0084-explicit-state-capture.md); the declared-secret rule is [ADR-0083](../adr/0083-declared-secret-state-fields.md) §4). Spec — [keeper/modules.md](../keeper/modules.md#corestateverb). |
| `core.cert` (author-addresses `core.cert.registered` / `core.cert.issued`) | `registered` / `issued` | Warrant issue and registration (NIM-99). The one keeper-side address with no schema document, so its params go unchecked offline — see the note in `shared/coremanifest/side.go`. Spec — [keeper/modules.md](../keeper/modules.md#corecertregistered--corecertissued). |

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
| `redis/` | [redis/README.md](redis/README.md) | The `redis` plugin — interface to live Redis, seven objects / nineteen actions. It is NOT under `community/` since NIM-766: with the origin-grouping level gone from the address, the document sits under the plugin's own name. The `user` object (`ACL SETUSER`/`DELUSER` on one user) was added by NIM-767. |
| `mongo/` | [mongo/README.md](mongo/README.md) | The `mongo` plugin — interface to live MongoDB, standalone AND replica-set, eight objects / fifteen actions. Moved out from under `community/` by NIM-769, which also split `user` into the `present` / `absent` actions the address rule asks for; NIM-805 added `replicaset`, `role`, `collection`, `index` and `database` beside the first three and made a wrong-typed param a refusal rather than a coercion (NIM-800). Sharding is deferred with a reason in NIM-820. |

## Catalog status

> ★ **Where the artifacts live (NIM-825).** The plugins are no longer under
> `examples/module/`: [ADR-011](../adr/0011-go-layout.md) reserves `examples/` for
> non-Go artifacts and sends runnable Go to separate repositories. Five now live in
> the [`soul-stack-plugin`](https://github.com/soul-stack-plugin) organisation, one
> repository each — [`mongo`](https://github.com/soul-stack-plugin/mongo),
> [`cassandra`](https://github.com/soul-stack-plugin/cassandra),
> [`ssh-static`](https://github.com/soul-stack-plugin/ssh-static),
> [`ssh-teleport`](https://github.com/soul-stack-plugin/ssh-teleport),
> [`ssh-vault`](https://github.com/soul-stack-plugin/ssh-vault) — and each carries its
> own gate (`make check`). `redis` is still in this tree; see the ADR-011
> amendment for why it did not follow the others.

The catalog is complete. The source of truth is the two registries in the code —
`soul/internal/coremod.Default` and `keeper/internal/coremod.Default` — and the count
is stated here once, against them:

**21 Soul-side + 7 Keeper-side = 28 apply modules.** The two tables above list exactly
those; `core.beacon` is in neither count (Vigil body, read-only observer — not an apply
module, see the "core-beacon" section).

- **21 Soul-side.** 18 by [ADR-015](../adr/0015-core-modules-mvp.md) (12 original MVPs +
post-MVP `url` / `line` / `repo` / `firewall` / `http`, accepted on real requests, +
`directory` split out of `core.file` by [Amendment 2026-07-17](../adr/0015-core-modules-mvp.md)),
plus `augur` by [ADR-025](../adr/0025-augur.md), `noop` (barrier anchor, ADR-015) and
`module` by [ADR-065](../adr/0065-core-module-installed.md).
- **7 Keeper-side.** `core.soul` and `core.vault` by [ADR-017](../adr/0017-keeper-side-core.md),
`core.choir` by [ADR-044](../adr/0044-choir.md), `core.bootstrap` by [ADR-063](../adr/0063-bootstrap-token-delivery.md),
`core.cert` (NIM-99), `core.ssh` (NIM-849) and `core.state` by [ADR-0084](../adr/0084-explicit-state-capture.md).
All but the first two are registered conditionally on their dependency.
`core.cloud` is **not** among them: NIM-761 removed it together with the CloudDriver
contract, and a VM is created by an ordinary `side: keeper` SoulModule plugin with its
own address ([known-limitations.md](../known-limitations.md)).

The Keeper-side table's seven base addresses are exactly what
`shared/coremanifest.KeeperSideAddrs()` returns — the list the linter and the render
pipeline route by, so a row that disagrees with it is a bug in the row.

`docs/module/core/` holds **26 directories** — 25 of these modules plus `core-beacon`.
Three modules have no per-module page yet and are documented where their spec lives:
`core.module` in [ADR-065](../adr/0065-core-module-installed.md) and
[service/manifest.md → `modules[]`](../service/manifest.md) (the host-side cache layout is in
[soul/modules.md](../soul/modules.md)); `core.state` and `core.cert` in
[keeper/modules.md](../keeper/modules.md).

Standards (pilot) - [`core/pkg/README.md`](core/pkg/README.md) and
[`core/file/README.md`](core/file/README.md). All links in the tables above lead to
existing documents.

## See also

- [ADR-015](../adr/0015-core-modules-mvp.md) - exact list of Soul-side core MVPs.
- [ADR-017](../adr/0017-keeper-side-core.md) - Keeper-side core extensions.
- [ADR-010](../adr/0010-templating.md) - template engine and `core.file.rendered`.
- [naming-rules.md → Destiny Modules](../naming-rules.md) - a dictionary of names.
