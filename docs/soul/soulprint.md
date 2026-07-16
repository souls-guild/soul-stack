# Soulprint — the typed schema MVP

Soulprint is our analogue of Salt grains: facts about the host that the Soul agent collects and periodically pushes to Keeper. They are used in scenario targeting, in the essence pipeline, in core modules for abstraction via the native pkg-mgr/init-system, and in the template rendering of configs.

**The source of truth on the schema is [ADR-018](../adr/0018-soulprint-typed.md).** This document is a detailed spec of the fields, semantics, collection algorithms, and use-cases.

## Delivery contract

- The Soul agent periodically collects facts (`refresh_interval` in `soul.yml`, default `5m` — see [`soul/config.md`](config.md)) and sends `SoulprintReport` over the EventStream ([ADR-012](../adr/0012-keeper-soul-grpc.md)).
- `SoulprintReport.collected_at` — the Soul-side timestamp of the moment of collection. On unmarshal, Keeper additionally sets `received_at` in Postgres (not part of the wire format). On a divergence > 10 minutes — a warn in the OTel trace.
- `SoulprintReport.facts` (`google.protobuf.Struct`) — a **deprecated** stub from the era of [ADR-012(g)](../adr/0012-keeper-soul-grpc.md). Kept for wire-compat (forward-compat only-add); Soul agents of newer versions do not fill it, Keeper is tolerant.
- `SoulprintReport.typed_facts` (`SoulprintFacts`) — the main channel, see below.

## The full SoulprintFacts schema

### Root message

| Field | Type | Semantics |
|---|---|---|
| `sid` | string | Echo SID for logs. **The authority is the mTLS peer cert** (see [ADR-012(i)](../adr/0012-keeper-soul-grpc.md)), not an identity claim. |
| `hostname` | string | The short name (without the domain), the result of `uname -n` / `gethostname()`. Differs from `network.fqdn` (the full FQDN). |
| `os` | [OsFacts](#osfacts) | The operating system. |
| `kernel` | [KernelFacts](#kernelfacts) | The kernel. |
| `cpu` | [CpuFacts](#cpufacts) | The processors. |
| `memory` | [MemoryFacts](#memoryfacts) | The memory. |
| `network` | [NetworkFacts](#networkfacts) | The network. |

**Field numbers 8..14 are reserved** for post-MVP extensions (`uptime`, `timezone`, `virtualization`, `cloud_provider`, `disks`, `bios`). They are added by separate ADRs with propose-and-wait on the field name. **Field 15** is reserved for an optional `extra: google.protobuf.Struct` for user-collectors (open Q №22).

### OsFacts

| Field | Type | Example | Semantics |
|---|---|---|---|
| `family` | string | `debian` / `rhel` / `alpine` / `windows` / `darwin` | Used in the essence pipeline (the `os/<family>.yaml` step, see [ADR-009](../adr/0009-scenario-dsl.md)). |
| `distro` | string | `ubuntu` / `rocky` / `alpine` | The concrete distribution. |
| `version` | string | `22.04` / `9.3` / `3.19` | The distribution version as a string (not SemVer). |
| `codename` | string | `jammy` / `bookworm` / `""` | Optional (not present in all distros). |
| `arch` | string | `amd64` / `arm64` | The target OS architecture. |
| `pkg_mgr` | string | `apt` / `dnf` / `apk` / `pacman` | **Collected by the Soul agent** through a `family+distro → pkg_mgr` mapping table. Read directly by `core.pkg.installed` for abstraction via the native pkg-mgr. |
| `init_system` | string | `systemd` / `openrc` / `sysv` / `launchd` | Similarly, for `core.service.*`. |

**The `pkg_mgr` / `init_system` mapping table** is inside the Soul agent (Go code). MVP coverage:

| family | distro | pkg_mgr | init_system |
|---|---|---|---|
| debian | ubuntu | apt | systemd |
| debian | debian | apt | systemd |
| rhel | rocky | dnf | systemd |
| rhel | centos | dnf | systemd |
| rhel | fedora | dnf | systemd |
| alpine | alpine | apk | openrc |
| darwin | macos | brew | launchd |
| windows | windows | (n/a) | (n/a) |

Extensions are separate changes to the Soul binary, a new version. This is the deliberate price of centralizing the table in one place.

### KernelFacts

| Field | Type | Example |
|---|---|---|
| `version` | string | `5.15.0-101-generic` (full, with the distribution suffix) |
| `release` | string | `5.15.0` (only the kernel version, without the suffix) |

### CpuFacts

| Field | Type | Semantics |
|---|---|---|
| `count` | int32 | The number of logical CPUs (accounting for HT/SMT). |
| `model` | string | The marketing name (`Intel Xeon E5-2670`, `Apple M2`). |
| `vendor` | string | `GenuineIntel` / `AuthenticAMD` / `ARM` / `Apple`. |

**Not included in the MVP:** `cores` (physical cores, without HT), `freq_mhz`, `cache_kb`. Added later only-add.

### MemoryFacts

| Field | Type | Semantics |
|---|---|---|
| `total_mb` | int64 | The full RAM volume in MB (not bytes!). Used in the essence pipeline: `int(soulprint.self.memory.total_mb * 0.6)`. |
| `available_mb` | int64 | Free right now (the value from `/proc/meminfo` or an equivalent). |
| `swap_mb` | int64 | The swap volume. |

### NetworkFacts

| Field | Type | Semantics |
|---|---|---|
| `primary_ip` | string | The host's primary IPv4 — the one used as the default bind address. **The Soul agent's heuristic:** the interface with the default route → its primary IPv4. Used in 90% of cases (for example, `redis.conf.tmpl: bind {{ .self.network.primary_ip }}`). |
| `fqdn` | string | The full FQDN, usually matching the SID. Differs from `hostname` (the short one). |
| `interfaces` | [NetworkInterface[]](#networkinterface) | The full list of network interfaces for multi-homed/VLAN-aware cases. |

#### NetworkInterface

| Field | Type | Semantics |
|---|---|---|
| `name` | string | `eth0` / `ens3` / `wlan0` / `lo` |
| `ipv4` | string[] | The interface's IPv4 addresses (CIDR notation: `10.0.0.1/24`). |
| `ipv6` | string[] | IPv6 addresses. |
| `mac` | string | The MAC address. |
| `mtu` | int32 | The interface MTU. |

## CEL access — the canonical form

### The stable layer (the current host)

From destiny and scenario:

| Path | Type | Context |
|---|---|---|
| `soulprint.self.sid` | string | everywhere |
| `soulprint.self.hostname` | string | everywhere |
| `soulprint.self.os.family` | string | everywhere |
| `soulprint.self.os.pkg_mgr` | string | everywhere (used by core.pkg.*) |
| `soulprint.self.os.init_system` | string | everywhere (used by core.service.*) |
| `soulprint.self.kernel.version` | string | everywhere |
| `soulprint.self.cpu.count` | int | everywhere |
| `soulprint.self.memory.total_mb` | int | everywhere (essence pipeline) |
| `soulprint.self.network.primary_ip` | string | everywhere |
| `soulprint.self.network.fqdn` | string | everywhere |
| `soulprint.self.network.interfaces[i].ipv4` | list<string> | everywhere |
| `soulprint.self.covens` | list<string> | **a Keeper-registry projection** (not from `SoulprintFacts`, see below) |
| `soulprint.self.choirs` | list<string> | **a Keeper-registry projection** of the host's Choir membership (ADR-044, a mirror of `covens`; not from `SoulprintFacts`, see below) |
| `soulprint.self.traits` | map<string, scalar\|list> | **a Keeper-registry projection** of operator-set key-value labels (ADR-060; not from `SoulprintFacts`, see below). The keys are dynamic — `soulprint.self.traits.<key>` without a static check of the 3rd segment. |

**The bare form `soulprint.<path>` without `.self`** is a **`soul-lint` validation error**. The canonical form is mandatory.

**`soulprint.self.sid` / `.covens` / `.choirs` / `.traits` / `.role` are a registry projection, available ALWAYS**, regardless of whether Soul has sent a `SoulprintReport`. The source is Keeper's roster (`souls.sid` / `souls.coven[]` / `incarnation_choir_voices` / `souls.traits` / the role from a Voice or `incarnation.spec.hosts[].role`), not collected facts: `sid` is authoritative via the mTLS peer cert. Keeper mixes them into `soulprint.self` when resolving CEL even with NULL reported facts (a freshly connected host / the collector is not yet implemented). Registry fields **overwrite** identically named reported keys if those happen to be in the push (the registry is the source of truth, [ADR-018](../adr/0018-soulprint-typed.md)). The other branches (`os` / `network` / `kernel` / `cpu` / `memory`) are available only when Soul has sent them — otherwise an access gives the ordinary `no such key`.

### Scenario-only accessors

These are available **only from a scenario**, not from a destiny (see [destiny/tasks.md §10](../destiny/tasks.md)):

| Path | Type | Semantics |
|---|---|---|
| `soulprint.hosts` | list<HostFacts> | All hosts of the current run with their stable facts (see [scenario/orchestration.md §4.1](../scenario/orchestration.md)). |
| `soulprint.hosts.where(<predicate>)` | list<HostFacts> | A filter by a **CEL predicate string** (`"'db' in covens"`, `"'replicas' in choirs"`, `"os.family == 'debian'"`, `"sid == soulprint.self.sid"`). The attributes — covens / choirs / sid / network.* / os.* / role. |
| `soulprint.where(<predicate>)` | list<HostFacts> | The list of hosts of the **current run** satisfying a CEL predicate string over the stable facts. Coincides with `soulprint.hosts.where(<predicate>)` in semantics and data source — it is a synonym for the frequent case when the full `soulprint.hosts` list is not intermediately needed. **Scenario-only**, like `soulprint.hosts` ([orchestration.md §4.1](../scenario/orchestration.md)). Keyword-args (`coven=...`) are not supported (CEL has no keyword-args). |

The structure of a `HostFacts` element coincides with `SoulprintFacts` + `covens`, `choirs`, `traits` are added (all three are Keeper-registry projections; `traits` are operator-set key-value labels, ADR-060) and `role` (a Keeper-registry projection, available on any run). `role` is filled by the topology resolver for each host of the roster with precedence **Choir Voice > spec**: the source is the Voice's role from `incarnation_choir_voices` (ADR-044, S-T6), and `incarnation.spec.hosts[].role` is a fallback for hosts without a Voice (including bootstrap-create, where there are no Choir memberships yet). If neither the Voice nor the spec gives a role — `role` is empty.

### Text/template context (for .tmpl)

Inside `.tmpl` files (rendered via `core.file.rendered`, [ADR-010](../adr/0010-templating.md)) there is a fixed set of system fields:

- `.self.sid` — string
- `.self.hostname` — string
- `.self.os.*` — the OsFacts fields
- `.self.kernel.*` — the KernelFacts fields
- `.self.cpu.*` — the CpuFacts fields
- `.self.memory.*` — the MemoryFacts fields
- `.self.network.*` — the NetworkFacts fields
- `.self.covens` — list<string>
- `.self.choirs` — list<string> (a registry projection of Choir membership, ADR-044)
- `.self.traits` — map<string, scalar|list> (a registry projection of operator-set labels, ADR-060)

These fields are available without explicitly passing them in `vars:` — this is the `core.file.rendered` contract.

## The Soulprint ↔ souls-registry boundary

| Where it lives | What | Who fills it |
|---|---|---|
| `SoulprintFacts` (Soul → Keeper, push) | os/kernel/cpu/memory/network/hostname/sid | The Soul agent |
| `souls.coven[]` (Postgres, Keeper-side) | covens | The operator via API / `core.soul.registered` |
| `souls.traits` (Postgres, Keeper-side, jsonb) | traits (operator-set key-value labels) | The operator (registry given, like coven; ADR-060) |
| `incarnation_choir_voices` (Postgres, Keeper-side) | choirs (the host's Choir membership) | `core.choir.present`/`absent` (ADR-044) |
| `souls.status` (Postgres) | pending/connected/disconnected/revoked | Keeper-managed |
| `incarnation_choir_voices.role` (Postgres) | the host's role (Choir Voice — the priority source) | `core.choir.present`/`absent` (ADR-044, S-T6) |
| `incarnation.spec.hosts[].role` (Postgres) | the declared role (fallback when there is no Voice; including bootstrap-create) | The operator in the spec |

**`soulprint.self.covens` in CEL** is a virtual projection: on resolve Keeper glues `SoulprintFacts` (from Soul) + `souls.coven[]` (from Postgres) into a logical view. Soul knows nothing about covens. The same for `soulprint.hosts[].covens` and `soulprint.hosts[].role`.

**`soulprint.self.choirs` in CEL** is a symmetric virtual projection: Keeper mixes in the host's Choir names from `incarnation_choir_voices` (ADR-044), this is **not** a collected `SoulprintFacts` fact. Available always (like `covens`), the list is registry data of the roster, not a Soul push. The same for `soulprint.hosts[].choirs`.

**`soulprint.self.traits` in CEL** are operator-set key-value labels ([ADR-060](../adr/0060-traits.md)), a separate axis next to the flat Coven. A Trait value is a scalar (`namespace: dba-ns`, `product: aboba`) or a list (`owners: [alice, bob]`); the keys are set by the operator and are **dynamic** (arbitrary names). The source is the registry column `souls.traits` (jsonb), a Trait is an organizational owner/product label, **NOT** a collected fact and **NOT** Soul-reported (as with `covens`/`choirs`/`role`). Available always; the projection **overrides** an identically named reported key if Soul were to send one (the registry is the source of truth, anti-spoofing: a compromised Soul cannot forge its membership). The same for `soulprint.hosts[].traits`. Targeting — `where: soulprint.self.traits.namespace == 'dba-ns'` (a scalar) or `where: 'alice' in soulprint.self.traits.owners` (a list). Soul-lint statically checks only the `traits` prefix, it does not verify the second segment (the operator key) — as with `covens`. In the pilot only the read/target path is implemented; the bulk-assign API, the audit-event, and the RBAC scope by traits are separate slices (see [ADR-060](../adr/0060-traits.md)).

## Use-cases (an inventory from the current examples)

| Use-case | Field | File |
|---|---|---|
| Destiny builds an arch-specific URL (Redis modules) | `soulprint.self.os.arch` | `examples/destiny/redis/tasks/modules.yml`, `examples/destiny/redis/destiny.yml` |
| Render a config via `.tmpl` (bind/announce) | `.self.network.primary_ip` | `examples/destiny/redis/templates/redis.conf.tmpl` |
| Scenario `where:` by SID (targeting a new node) | `soulprint.self.sid == input.new_node_sid` | `examples/service/redis/scenario/add_node/main.yml` |
| Scenario roster → an endpoint map by hosts | `soulprint.hosts.map(h, { h.sid: { 'addr': h.network.primary_ip + ':6379' } })` | `examples/service/redis/scenario/create/cluster.yml` |
| Scenario guard "both SIDs are members of the roster" | `size(soulprint.hosts.where("sid == input.new_node_sid")) == 1` | `examples/service/redis/scenario/add_node/main.yml` |
| Scenario master-election (declared, the first by SID) | `soulprint.hosts[0]` | `examples/service/redis/scenario/create/sentinel.yml` |
| core.pkg.installed → the native pkg-mgr | `soulprint.self.os.pkg_mgr` | inside the core module |
| core.service.* → the init system | `soulprint.self.os.init_system` | inside the core module |

## What is NOT in the MVP

- **User-collectors** (`/etc/soul/soulprint.d/*` collectors — open Q №22). Requires separate decisions on the collector format, sandbox, launch rights, output validation. Closed by a separate ADR when a concrete scenario appears.
- **`uptime` / `timezone`** — will be added post-MVP only-add (field 8/9 in `SoulprintFacts`).
- **`virtualization`** (KVM / Hyper-V / WSL / container) — post-MVP.
- **`cloud_provider`** (aws / gcp / azure detection via metadata) — post-MVP, overlaps with CloudDriver plugins.
- **`disks`** (mount points / FS / size) — post-MVP.
- **`bios`** (vendor / version / virtualization-extensions) — post-MVP.
- **`cpu.cores`** (physical, without HT), **`cpu.freq_mhz`**, **`cpu.cache_kb`** — post-MVP only-add.

## Related documents

- [ADR-018 in `docs/architecture.md`](../adr/0018-soulprint-typed.md) — the decision record.
- [ADR-012(g) in `docs/architecture.md`](../adr/0012-keeper-soul-grpc.md) — the `facts: Struct` stub (now deprecated).
- [`docs/soul/config.md`](config.md) — the `soulprint:` block (`refresh_interval`).
- [`docs/keeper/storage.md`](../keeper/storage.md) — where Soulprint lives in Postgres.
- [`docs/keeper/modules.md`](../keeper/modules.md) — `core.soul.registered` (Keeper-registry covens).
- [ADR-060](../adr/0060-traits.md) — Trait (operator-set key-value labels, the `soulprint.self.traits` registry projection).
- [`docs/templating.md`](../templating.md) — the CEL and text/template contexts.
- [`docs/destiny/tasks.md`](../destiny/tasks.md) — the destiny context for Soulprint.
- [`docs/scenario/orchestration.md`](../scenario/orchestration.md) — the scenario-only accessors (`soulprint.hosts`, `soulprint.where`).
- [`proto/keeper/v1/soulprint.proto`](../../proto/keeper/v1/soulprint.proto) — the actual proto file.
